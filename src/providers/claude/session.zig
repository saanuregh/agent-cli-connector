//! A live Claude Code session: the CLI process (replaced on a restarting
//! rewind), the uniform session methods, and the state they share.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const agent = @import("agent");
const types = agent.types;
const errors = agent.errors;
const Session = agent.Session;
const RawJson = agent.json.RawJson;
const JsonView = agent.json.JsonView;
const ObjectBuilder = agent.json.ObjectBuilder;
const Value = std.json.Value;
const Diagnostics = agent.Diagnostics;
const kit = agent.providerkit;
const eql = kit.eql;

const options = @import("options.zig");
const host_tools = @import("host_tools.zig");
const control = @import("control.zig");
const launch_mod = @import("launch.zig");
const permissions = @import("permissions.zig");
const rewind_mod = @import("rewind.zig");
const history = @import("history.zig");
const Translator = @import("translate.zig").Translator;
const Provider = @import("provider.zig").Provider;

const name = @import("root.zig").name;
const log = std.log.scoped(.agent);

/// The first Claude Code version that honors the per-tool permission
/// policies `ToolDef.needs_approval` relies on.
const min_tool_policy_version = "2.1.301";
/// Bounds the get_context_usage request at start, like the effort read.
const context_usage_timeout_ms = 3000;

pub const provider_capabilities: types.Capabilities = .{
    .streaming = true,
    .reasoning_stream = true,
    .images = true,
    .steer = true,
    .queue = true,
    .interrupt = true,
    .interrupt_turn = true,
    .set_model = true,
    .set_effort = true,
    .permission_modes = &.{
        types.permission_mode.default, types.permission_mode.accept_edits, types.permission_mode.plan,
        types.permission_mode.bypass,  types.permission_mode.dont_ask,     types.permission_mode.auto,
    },
    .permission_prompt = true,
    .user_questions = true,
    .elicitation = true,
    .host_tools = true,
    .mcp_servers = true,
    .resume_session = true,
    .fork = true,
    .fork_at_turn = true,
    .rewind = true,
    .compact = true,
    .set_title = true,
    .stop_task = true,
    .read_rate_limits = true,
    .system_prompt = true,
    .output_schema = false,
    .plans = true,
    .tasks = true,
    .subagents = true,
    .cost_reporting = true,
    .rate_limits = true,
    .hooks = true,
    .goals = true,
    // Only with SessionOptions.tail_shell_output; set per session.
    .tool_output_stream = false,
    .plan_stream = false,
    // A rewind the CLI cannot do in place restarts it, and restarting at
    // the first turn starts a new session.
    .rewind_changes_session_id = true,
    .requests_from_subagents = true,
    .background_turns = true,
    // Steered input waits for the running tool call ("next").
    .steer_interrupts_tools = false,
    .dialogs = true,
    .native_requests = false,
    .check = true,
    .list_models = true,
    .list_sessions = true,
    .read_history = true,
    .delete_session = true,
    .list_subagents = true,
    .provider_call = false,
};

/// One run of the CLI and the state that lives as long as it. A restarting
/// rewind retires it and starts another, linked to it through `previous`.
pub const Launch = struct {
    claude: *ClaudeSession,
    /// The launch this one replaced; the session frees the chain.
    previous: ?*Launch = null,
    process: *agent.process.Process = undefined,
    /// Translates this process's output.
    translator: *Translator,
    /// Set once the launch is the session's latest; the handlers wait for
    /// it.
    ready: Io.Event = .unset,
    retired: std.atomic.Value(bool) = .init(false),
    /// adopted, or provisional while a rewind restart decides whether to
    /// keep the process; orphaned means it exited while provisional, so
    /// ending the session was left to the rewind.
    state: std.atomic.Value(State) = .init(.adopted),
    /// Set after the exit handler ran.
    done: Io.Event = .unset,
    exit_arena: std.heap.ArenaAllocator,
    exit_info: ?types.ErrorInfo = null,
    /// Holds one stdout line's decoded message; the reader resets it after
    /// each line.
    line_arena: std.heap.ArenaAllocator,
    /// Every control request ID this process sent, so a prompt listed again
    /// in an initialize response is not asked twice, and whether it is
    /// still unanswered (false once answered or withdrawn). Guarded by
    /// `ClaudeSession.control_mutex`.
    requests: kit.OwnedMap(bool) = .{},

    pub const State = enum(u8) { adopted, provisional, orphaned };

    fn onLine(context: *anyopaque, line: []const u8) void {
        const launch: *Launch = @ptrCast(@alignCast(context));
        launch.ready.waitUncancelable(launch.claude.io());
        defer _ = launch.line_arena.reset(.retain_capacity);
        // The reader cannot return the failure; the message is lost.
        control.handleLine(launch.claude, launch, launch.line_arena.allocator(), line) catch |e|
            log.err("claude: handling a message failed: {t}: {s}", .{ e, line[0..@min(line.len, 200)] });
    }

    fn onExit(context: *anyopaque, err: ?types.ErrorInfo) void {
        const launch: *Launch = @ptrCast(@alignCast(context));
        const claude = launch.claude;
        launch.ready.waitUncancelable(claude.io());
        defer launch.done.set(claude.io());
        if (err) |e| launch.exit_info = agent.clone(types.ErrorInfo, launch.exit_arena.allocator(), e) catch e;
        control.failWaiters(claude, launch);
        if (launch.retired.load(.acquire) or launch.state.cmpxchgStrong(.provisional, .orphaned, .acq_rel, .acquire) == null) return;
        if (claude.closing.load(.acquire)) {
            claude.rt().finish(null);
            return;
        }
        launch.translator.failRunningTasks();
        claude.rt().finish(launch.exit_info);
    }

    fn tapLine(context: *anyopaque, sent: bool, line: []const u8) void {
        const launch: *Launch = @ptrCast(@alignCast(context));
        launch.claude.rt().tap(if (sent) .send else .receive, line);
    }

    /// Why the process exited, or null when it runs or exited cleanly. It
    /// is recorded before waiting control requests fail, so a request that
    /// failed on the exit sees it.
    pub fn exitError(launch: *Launch) ?types.ErrorInfo {
        return launch.exit_info;
    }

    pub fn write(launch: *Launch, bytes: []const u8, diag: ?*Diagnostics) errors.Error!void {
        return launch.process.writeLine(bytes, diag);
    }

    /// Frees the launch; its process must have stopped.
    fn destroy(launch: *Launch, gpa: Allocator) void {
        launch.process.destroy();
        launch.translator.destroy();
        launch.requests.deinit(gpa);
        launch.exit_arena.deinit();
        launch.line_arena.deinit();
        gpa.destroy(launch);
    }
};

/// Turn starts and items in emission order, so a rewind can report every
/// item it drops. Two maps find an entry without scanning the list.
pub const TurnLog = struct {
    mutex: Io.Mutex = .init,
    entries: std.ArrayList(Entry) = .empty,
    /// A turn to the index of its start entry, and an item to the index of
    /// its entry. The keys are the entries' own strings, so an entry leaves
    /// both maps before its strings are freed.
    starts: std.StringHashMapUnmanaged(usize) = .empty,
    by_item: std.StringHashMapUnmanaged(usize) = .empty,

    /// A turn start (item empty) or an item of a turn; strings owned.
    const Entry = struct { turn: []u8, item: []u8 };

    /// Records `item` of `turn`. On failure a rewind cannot report the
    /// item as removed, so the loss is logged.
    pub fn add(turn_log: *TurnLog, io: Io, gpa: Allocator, turn: []const u8, item: []const u8) void {
        if (item.len == 0) return;
        turn_log.mutex.lockUncancelable(io);
        defer turn_log.mutex.unlock(io);
        if (turn.len > 0 and !turn_log.starts.contains(turn)) turn_log.append(gpa, turn, "") catch
            return log.err("claude: out of memory: turn {s} not recorded for rewind", .{turn});
        if (turn_log.by_item.contains(item)) return;
        turn_log.append(gpa, turn, item) catch log.err("claude: out of memory: item {s} not recorded for rewind", .{item});
    }

    fn append(turn_log: *TurnLog, gpa: Allocator, turn: []const u8, item: []const u8) Allocator.Error!void {
        const index = turn_log.entries.items.len;
        try turn_log.entries.ensureUnusedCapacity(gpa, 1);
        const turn_copy = try gpa.dupe(u8, turn);
        errdefer gpa.free(turn_copy);
        const item_copy = try gpa.dupe(u8, item);
        errdefer gpa.free(item_copy);
        if (item.len == 0) try turn_log.starts.put(gpa, turn_copy, index) else try turn_log.by_item.put(gpa, item_copy, index);
        turn_log.entries.appendAssumeCapacity(.{ .turn = turn_copy, .item = item_copy });
    }

    /// The turn that produced `item` (copied into `a`), or "".
    pub fn turnOf(turn_log: *TurnLog, io: Io, a: Allocator, item: []const u8) Allocator.Error![]const u8 {
        turn_log.mutex.lockUncancelable(io);
        defer turn_log.mutex.unlock(io);
        if (turn_log.starts.contains(item)) return a.dupe(u8, item);
        const index = turn_log.by_item.get(item) orelse return "";
        return a.dupe(u8, turn_log.entries.items[index].turn);
    }

    /// Forgets `turn` and everything recorded after it; returns the turns
    /// started from then on and every dropped item ID (turn IDs included),
    /// copied into `a`.
    pub fn drop(turn_log: *TurnLog, io: Io, gpa: Allocator, a: Allocator, turn: []const u8) Allocator.Error!struct { []const []const u8, []const []const u8 } {
        turn_log.mutex.lockUncancelable(io);
        defer turn_log.mutex.unlock(io);
        const from = turn_log.starts.get(turn) orelse return .{ &.{}, &.{} };
        var turns: std.ArrayList([]const u8) = .empty;
        var ids: std.ArrayList([]const u8) = .empty;
        for (turn_log.entries.items[from..]) |entry| {
            var id: []const u8 = entry.item;
            if (id.len == 0) {
                try turns.append(a, try a.dupe(u8, entry.turn));
                id = entry.turn;
            }
            if (!kit.contains(ids.items, id)) try ids.append(a, try a.dupe(u8, id));
        }
        for (turn_log.entries.items[from..]) |entry| {
            _ = if (entry.item.len == 0) turn_log.starts.remove(entry.turn) else turn_log.by_item.remove(entry.item);
            gpa.free(entry.turn);
            gpa.free(entry.item);
        }
        turn_log.entries.shrinkRetainingCapacity(from);
        return .{ turns.items, ids.items };
    }

    /// The oldest recorded turn (copied into `a`), or "".
    pub fn first(turn_log: *TurnLog, io: Io, a: Allocator) Allocator.Error![]const u8 {
        turn_log.mutex.lockUncancelable(io);
        defer turn_log.mutex.unlock(io);
        for (turn_log.entries.items) |entry| if (entry.item.len == 0) return a.dupe(u8, entry.turn);
        return "";
    }

    pub fn reset(turn_log: *TurnLog, io: Io, gpa: Allocator) void {
        turn_log.mutex.lockUncancelable(io);
        defer turn_log.mutex.unlock(io);
        turn_log.clearLocked(gpa);
    }

    fn clearLocked(turn_log: *TurnLog, gpa: Allocator) void {
        turn_log.starts.clearRetainingCapacity();
        turn_log.by_item.clearRetainingCapacity();
        for (turn_log.entries.items) |entry| {
            gpa.free(entry.turn);
            gpa.free(entry.item);
        }
        turn_log.entries.clearRetainingCapacity();
    }

    pub fn deinit(turn_log: *TurnLog, gpa: Allocator) void {
        turn_log.clearLocked(gpa);
        turn_log.entries.deinit(gpa);
        turn_log.starts.deinit(gpa);
        turn_log.by_item.deinit(gpa);
    }
};

pub const ClaudeSession = struct {
    session: Session,
    provider: *Provider,
    /// Owns `cfg` and `opts`.
    arena: std.heap.ArenaAllocator,
    cfg: types.Config,
    opts: options.SessionOptions,
    tools: ?host_tools.Server = null,
    /// Skips reading the applied effort at start, for the short-lived
    /// processes of check and list_models.
    skip_applied_effort: bool = false,

    launch_mutex: Io.Mutex = .init,
    /// The latest launch; the launches a rewind retired hang off it through
    /// `Launch.previous` until the session is freed, because other tasks
    /// may still hold them.
    launch: ?*Launch = null,
    /// Held for writing while a rewind replaces the process, so send waits
    /// for the new one.
    swap_lock: Io.RwLock = .init,

    next_id: std.atomic.Value(u64) = .init(0),
    /// Set once by the first close.
    closing: std.atomic.Value(bool) = .init(false),

    /// Guards `waiters` and each launch's `requests`.
    control_mutex: Io.Mutex = .init,
    waiters: std.array_hash_map.String(*control.Waiter) = .empty,

    init_raw_mutex: Io.Mutex = .init,
    /// The latest initialize response (gpa-owned).
    init_raw: []u8 = &.{},

    tasks_mutex: Io.Mutex = .init,
    /// IDs of tasks reported in task_updated, for stop_task.
    known_tasks: kit.StringSet = .{},
    turn_items: TurnLog = .{},

    pub const vtable: Session.VTable = .{
        .send = send,
        .interrupt = interrupt,
        .respond = permissions.respond,
        .close = close,
        .destroy = destroy,
        .interrupt_turn = interruptTurn,
        .set_model = setModel,
        .set_effort = setEffort,
        .set_permission_mode = setPermissionMode,
        .set_title = setTitle,
        .compact = compact,
        .rewind = rewind_mod.rewind,
        .stop_task = stopTask,
        .fork = fork,
        .rate_limits = control.rateLimits,
        .call = control.call,
    };

    pub fn of(s: *Session) *ClaudeSession {
        return @fieldParentPtr("session", s);
    }

    pub fn rt(claude: *ClaudeSession) *agent.runtime.Runtime {
        return &claude.session.rt;
    }

    pub fn env(claude: *ClaudeSession) agent.Env {
        return claude.rt().env;
    }

    pub fn gpa(claude: *ClaudeSession) Allocator {
        return claude.rt().env.gpa;
    }

    pub fn io(claude: *ClaudeSession) Io {
        return claude.rt().env.io;
    }

    /// The latest launch of the CLI.
    pub fn current(claude: *ClaudeSession) ?*Launch {
        claude.launch_mutex.lockUncancelable(claude.io());
        defer claude.launch_mutex.unlock(claude.io());
        return claude.launch;
    }

    /// The latest launch's translator. A started session always has a
    /// launch.
    pub fn translator(claude: *ClaudeSession) *Translator {
        return claude.current().?.translator;
    }

    /// Writes one message to the running CLI process.
    pub fn write(claude: *ClaudeSession, bytes: []const u8, diag: ?*Diagnostics) errors.Error!void {
        const launch = claude.current() orelse return claude.rt().closedError(diag);
        return launch.write(bytes, diag);
    }

    pub fn toolServerName(claude: *ClaudeSession) []const u8 {
        return if (claude.opts.tool_server_name.len > 0) claude.opts.tool_server_name else "host";
    }

    /// The native session ID, copied into `a`.
    pub fn sessionId(claude: *ClaudeSession, a: Allocator) Allocator.Error![]const u8 {
        return claude.rt().sessionId(a);
    }

    /// Emits `ev`, stamping items with their turn as history items carry
    /// it, and records the items of each turn for rewind and the tasks for
    /// stop_task.
    pub fn emit(claude: *ClaudeSession, event_in: types.Event) void {
        var event = event_in;
        var turn_buf: agent.Id = .{};
        const item: ?*types.Item = switch (event.payload) {
            .item_started => |*p| &p.item,
            .item_updated => |*p| &p.item,
            .item_completed => |*p| &p.item,
            else => null,
        };
        if (item) |stamped| {
            if (event.turn_id.len == 0) event.turn_id = stamped.turn_id;
            if (event.turn_id.len == 0) {
                turn_buf = claude.eventTurn();
                event.turn_id = turn_buf.slice();
            }
            stamped.turn_id = event.turn_id;
        }
        claude.rt().emit(event);
        switch (event.payload) {
            .task_updated => |update| {
                claude.tasks_mutex.lockUncancelable(claude.io());
                defer claude.tasks_mutex.unlock(claude.io());
                if (!claude.known_tasks.contains(update.task.id)) claude.known_tasks.put(claude.gpa(), update.task.id, {}) catch
                    log.err("claude: out of memory: task {s} not recorded; stop_task will refuse it", .{update.task.id});
            },
            else => {},
        }
        if (item) |recorded| claude.turn_items.add(claude.io(), claude.gpa(), event.turn_id, recorded.id);
    }

    /// The turn the runtime stamps on an event without one: the running
    /// turn, else the oldest unfinished one.
    pub fn eventTurn(claude: *ClaudeSession) agent.Id {
        const running, _ = claude.rt().runningTurn();
        if (!running.isEmpty()) return running;
        return claude.rt().currentTurn();
    }

    /// Sets one field of the session info to a copy of `value` and
    /// announces it.
    pub fn setInfoField(claude: *ClaudeSession, comptime field: []const u8, value: @FieldType(types.SessionInfo, field)) void {
        claude.setInfoFieldFor(field, value, .reset);
    }

    /// Like `setInfoField`; a session ID change reports `reason`.
    pub fn setInfoFieldFor(claude: *ClaudeSession, comptime field: []const u8, value: @FieldType(types.SessionInfo, field), reason: types.SessionIdChangedReason) void {
        const Change = struct {
            fn apply(new_value: @TypeOf(value), info: *types.SessionInfo, a: Allocator) Allocator.Error!void {
                @field(info, field) = try agent.clone(@TypeOf(value), a, new_value);
            }
        };
        claude.rt().updateInfoFor(value, true, reason, Change.apply) catch log.err("claude: out of memory: session info update lost", .{});
    }

    /// A copy of the latest initialize response in `a`.
    pub fn initRaw(claude: *ClaudeSession, a: Allocator) Allocator.Error![]const u8 {
        claude.init_raw_mutex.lockUncancelable(claude.io());
        defer claude.init_raw_mutex.unlock(claude.io());
        return a.dupe(u8, claude.init_raw);
    }

    pub fn setInitRaw(claude: *ClaudeSession, raw: []const u8) Allocator.Error!void {
        const copy = try claude.gpa().dupe(u8, raw);
        claude.init_raw_mutex.lockUncancelable(claude.io());
        defer claude.init_raw_mutex.unlock(claude.io());
        claude.gpa().free(claude.init_raw);
        claude.init_raw = copy;
    }

    /// Whether the CLI is launched so that it accepts the bypass mode.
    pub fn bypassAllowed(claude: *ClaudeSession, cfg: types.Config, session_options: options.SessionOptions) bool {
        const flag = "--allow-dangerously-skip-permissions";
        return session_options.allow_bypass_permissions or permissions.isBypass(cfg.permission_mode) or
            kit.contains(session_options.extra_args, flag) or kit.contains(claude.provider.options.extra_args, flag);
    }
};

// ---------------------------------------------------------------- start

/// Launches a session and returns it started.
pub fn start(provider: *Provider, start_options: agent.StartOptions, skip_applied_effort: bool, diag: ?*Diagnostics) errors.Error!*Session {
    const gpa = provider.env.gpa;
    const claude = try gpa.create(ClaudeSession);
    claude.* = .{
        .session = undefined,
        .provider = provider,
        .arena = .init(gpa),
        .cfg = undefined,
        .opts = undefined,
        .skip_applied_effort = skip_applied_effort,
    };
    const a = claude.arena.allocator();
    claude.cfg = agent.clone(types.Config, a, start_options.config) catch |e| {
        claude.arena.deinit();
        gpa.destroy(claude);
        return e;
    };
    claude.opts = kit.options(options.SessionOptions, a, name, claude.cfg.provider_options, diag) catch |e| {
        claude.arena.deinit();
        gpa.destroy(claude);
        return e;
    };
    var caps = provider_capabilities;
    if (claude.opts.no_partial_messages) {
        caps.streaming = false;
        caps.reasoning_stream = false;
    }
    if (claude.opts.no_permission_prompts or claude.opts.permission_prompt_tool.len > 0) caps.permission_prompt = false;
    caps.tool_output_stream = claude.opts.tail_shell_output;
    var session_start_options = start_options;
    session_start_options.config = claude.cfg;
    claude.session = .init(provider.env, name, &ClaudeSession.vtable, caps, session_start_options);

    // The details stay in `diag`, which the cleanup does not touch.
    startSession(claude, diag) catch |e| {
        claude.session.deinit();
        return e;
    };
    return &claude.session;
}

fn startSession(claude: *ClaudeSession, diag: ?*Diagnostics) errors.Error!void {
    const a = claude.arena.allocator();
    var cfg = &claude.cfg;
    const provider = claude.provider;
    if (permissions.nativeMode(cfg.permission_mode) == null) return errors.unsupported(diag, name, "permission mode read_only");
    if (cfg.mcp_servers) |servers| {
        for (servers.map.keys(), servers.map.values()) |k, server| {
            if (server.enabled_tools.len > 0) return errors.fail(diag, .unsupported, "claude: mcp_servers[{s}].enabled_tools is not supported (use disallowed_tools)", .{k});
        }
    }
    if (cfg.fork and cfg.resume_id.len == 0 and !claude.opts.@"continue") return errors.fail(diag, .invalid_params, "claude: fork requires resume_id", .{});
    if (cfg.session_id.len > 0 and !kit.isUuid(cfg.session_id)) return errors.fail(diag, .invalid_params, "claude: session_id must be a UUID", .{});
    if (claude.opts.@"continue" and cfg.resume_id.len == 0) {
        if (history.latestSessionId(provider, a, cfg.*)) |latest| cfg.resume_id = latest;
    }
    if (cfg.fork_at_turn.len > 0) {
        if (!cfg.fork or cfg.resume_id.len == 0) return errors.fail(diag, .invalid_params, "claude: fork_at_turn requires fork and resume_id", .{});
        if (claude.opts.resume_session_at.len == 0) {
            claude.opts.resume_session_at = try rewind_mod.forkPoint(provider, a, cfg.*, cfg.resume_id, cfg.fork_at_turn, diag);
        }
    }
    if (cfg.tools.len > 0) claude.tools = .{ .name = claude.toolServerName(), .version = "1.0.0", .tools = cfg.tools };

    var session_id: []const u8 = cfg.session_id;
    var uuid_buf: agent.Id = undefined;
    if (cfg.resume_id.len > 0 and !cfg.fork) {
        session_id = cfg.resume_id;
    } else if (session_id.len == 0) {
        uuid_buf = claude.env().uuid();
        session_id = try a.dupe(u8, uuid_buf.slice());
    }
    claude.rt().setSessionId(session_id);

    var scratch: std.heap.ArenaAllocator = .init(claude.gpa());
    defer scratch.deinit();
    const scratch_allocator = scratch.allocator();
    const init = try launch_mod.launch(claude, scratch_allocator, cfg.*, claude.opts, session_id, false, diag);
    const init_response = JsonView.parseBytes(scratch_allocator, init.raw);
    const version = init_response.str("claude_code_version");
    var needs_approval = false;
    for (cfg.tools) |tool| needs_approval = needs_approval or tool.needs_approval;
    if (needs_approval and version.len > 0 and kit.compareVersions(version, min_tool_policy_version) < 0) {
        return errors.fail(diag, .unsupported, "claude: host tools with needs_approval need Claude Code {s} or newer to be enforced (found {s})", .{ min_tool_policy_version, version });
    }
    const native_mode = init_response.str("current_permission_mode");
    var context_window: i64 = 0;
    if (!claude.skip_applied_effort) {
        // "summary" answers from local estimates, without token-count
        // calls. Without an answer the window stays unknown until the
        // first turn's usage, so the failure's details are not needed.
        if (control.control(claude, scratch_allocator, "get_context_usage", RawJson.of("{\"detail\":\"summary\"}"), .{ .timeout_ms = context_usage_timeout_ms }, null)) |usage| {
            context_window = JsonView.parseBytes(scratch_allocator, usage).int("maxTokens");
        } else |_| {}
    }
    const info: types.SessionInfo = .{
        .id = session_id,
        .provider = name,
        .cwd = cfg.cwd,
        .model = kit.firstOf(&.{ cfg.model, control.defaultModel(init_response) }),
        .effort = kit.firstOf(&.{ cfg.effort, init.applied_effort }),
        .permission_mode = kit.firstOf(&.{ cfg.permission_mode, permissions.uniformMode(native_mode) }),
        .title = cfg.title,
        .version = version,
        .commands = try control.slashCommands(scratch_allocator, init_response.get("commands")),
        .context_window = context_window,
        .raw = RawJson.of(init.raw),
    };
    claude.rt().markStarted(info, cfg.resume_id.len > 0);
}

// ---------------------------------------------------------------- input

/// The image types the Claude API accepts.
const image_media_types = [_][]const u8{ "image/jpeg", "image/png", "image/gif", "image/webp" };

fn send(s: *Session, in: types.Input, turn_id: []const u8, diag: ?*Diagnostics) errors.Error!void {
    const claude = ClaudeSession.of(s);
    claude.swap_lock.lockSharedUncancelable(claude.io());
    defer claude.swap_lock.unlockShared(claude.io());
    try s.rt.ensureOpen(diag);
    if (in.output_schema != null) return errors.unsupported(diag, name, "per-turn output schema (use Config.output_schema)");
    var arena: std.heap.ArenaAllocator = .init(claude.gpa());
    defer arena.deinit();
    const a = arena.allocator();
    const turn_options = try kit.options(options.TurnOptions, a, name, in.provider_options, diag);
    const busy = s.rt.busy();
    const queued = busy and (eql(turn_options.priority, "later") or (turn_options.priority.len == 0 and in.delivery == .queue));
    if (queued and (in.model.len > 0 or in.effort.len > 0)) {
        // The CLI keeps the queue, so the switch would reach the running
        // turn rather than wait for this input.
        return errors.unsupported(diag, name, "Input.model or Input.effort on input queued behind a running turn");
    }
    if (in.model.len > 0) try setModel(s, in.model, diag);
    if (in.effort.len > 0) try setEffort(s, in.effort, diag);
    const content = try encodeContent(claude, a, try in.allParts(a), diag);
    try s.rt.newTurnFor(turn_id, .{ .client_id = in.client_id }, diag);

    var message: ObjectBuilder = .init(a);
    try message.str("role", "user");
    try message.put("content", content);
    var envelope: ObjectBuilder = .init(a);
    try envelope.str("type", "user");
    try envelope.put("message", message.value());
    try envelope.put("parent_tool_use_id", .null);
    try envelope.str("session_id", "");
    try envelope.str("uuid", turn_id);
    const priority: []const u8 = if (turn_options.priority.len > 0) turn_options.priority else if (!busy) "" else switch (in.delivery) {
        .steer => "next",
        .interrupt => "now",
        .queue => "later",
    };
    try envelope.strIf("priority", priority);
    if (turn_options.no_query) try envelope.put("shouldQuery", .{ .bool = false });
    if (turn_options.verbatim or claude.opts.verbatim_prompts) try envelope.put("client_composed", .{ .bool = true });
    if (turn_options.pasted_content.len > 0) try envelope.put("pasted_content", try agent.json.stringArray(a, turn_options.pasted_content));
    if (turn_options.origin) |origin| try envelope.put("origin", agent.json.valueOf(a, origin) orelse .null);
    const bytes = try agent.json.mergeObject(a, try agent.json.encodeValue(a, envelope.value()), turn_options.extra);

    // The item goes out before the write: the CLI can finish the turn
    // before send returns, and the turn's items must include the message.
    claude.emit(.{ .turn_id = turn_id, .payload = .{ .item_completed = .{ .item = .{
        .id = turn_id,
        .kind = .user_message,
        .status = .completed,
        .text = try in.plainText(a),
        .client_id = in.client_id,
    } } } });
    // The turn's waiters see the failure too, so it keeps its details.
    var write_diag: Diagnostics = .{};
    claude.write(bytes, &write_diag) catch |e| {
        const info = Diagnostics.of(&write_diag, e);
        claude.emit(.{ .turn_id = turn_id, .payload = .{ .items_removed = .{ .ids = &.{turn_id} } } });
        s.rt.abandonTurn(turn_id, info);
        return errors.failWith(diag, info);
    };
    // The message joins the transcript without starting a turn.
    if (turn_options.no_query) s.rt.finishTurn(turn_id, .{ .status = .completed, .stop_reason = "no_query" });
}

/// Renders input parts as message content blocks. The CLI expands a slash
/// command only in the last text block, so a skill part becomes a final
/// "/name args" block whose arguments are the input's text parts; the other
/// blocks keep their order before it. Likewise a text part starting with
/// "/name" is moved after the attachments that follow it.
fn encodeContent(claude: *ClaudeSession, a: Allocator, parts: []const types.Part, diag: ?*Diagnostics) errors.Error!Value {
    const skill: ?usize = for (parts, 0..) |part, i| {
        if (part.type == .skill) break i;
    } else null;
    var args: std.ArrayList([]const u8) = .empty;
    var out: std.json.Array = .init(a);
    for (parts, 0..) |part, i| switch (part.type) {
        .text => {
            if (skill != null) {
                const text = std.mem.trim(u8, part.text, " \t\r\n");
                if (text.len > 0) try args.append(a, text);
                continue;
            }
            try out.append(try textBlock(a, part.text));
        },
        .image => {
            if (part.url.len > 0) {
                try out.append(try agent.json.toValue(a, .{ .type = "image", .source = .{ .type = "url", .url = part.url } }));
                continue;
            }
            const data = try kit.readPart(claude.env(), a, part, diag);
            if (!kit.contains(&image_media_types, data.media_type)) {
                return errors.fail(diag, .invalid_params, "claude: image media type \"{s}\" is not supported; use one of image/jpeg, image/png, image/gif, image/webp", .{data.media_type});
            }
            try out.append(try agent.json.toValue(a, .{ .type = "image", .source = .{ .type = "base64", .media_type = data.media_type, .data = data.data } }));
        },
        .file => {
            const media = if (part.media_type.len == 0 and part.path.len > 0) kit.mediaType(part.path) else part.media_type;
            if (eql(media, "application/pdf") or part.data.len > 0) {
                const data = try kit.readPart(claude.env(), a, part, diag);
                try out.append(try agent.json.toValue(a, .{ .type = "document", .source = .{ .type = "base64", .media_type = data.media_type, .data = data.data } }));
                continue;
            }
            try out.append(try textBlock(a, try std.fmt.allocPrint(a, "@{s}", .{part.path})));
        },
        .skill => if (i != skill.?) try out.append(try textBlock(a, try std.fmt.allocPrint(a, "/{s}", .{part.name}))),
        .mention => try out.append(try textBlock(a, try std.fmt.allocPrint(a, "/{s}", .{part.name}))),
    };
    if (skill) |k| {
        const joined = try std.mem.join(a, "\n\n", args.items);
        const command = try std.fmt.allocPrint(a, "/{s} {s}", .{ parts[k].name, joined });
        try out.append(try textBlock(a, std.mem.trim(u8, command, " \t\r\n")));
    } else commandLast(&out);
    if (out.items.len == 0) return errors.fail(diag, .invalid_params, "claude: input has no content", .{});
    return .{ .array = out };
}

fn textBlock(a: Allocator, text: []const u8) Allocator.Error!Value {
    return agent.json.toValue(a, .{ .type = "text", .text = text });
}

/// Moves the first text block that starts with a slash command to the end
/// when attachments follow it.
fn commandLast(blocks: *std.json.Array) void {
    const items = blocks.items;
    const i = for (items, 0..) |value, k| {
        const block = JsonView.of(value);
        if (eql(block.str("type"), "text") and isSlashCommand(block.str("text"))) break k;
    } else return;
    const attachments = for (items[i + 1 ..]) |value| {
        if (!eql(JsonView.of(value).str("type"), "text")) break true;
    } else false;
    if (!attachments) return;
    const command = blocks.orderedRemove(i);
    blocks.appendAssumeCapacity(command);
}

/// Text of the form "/name ...".
fn isSlashCommand(text: []const u8) bool {
    const trimmed = std.mem.trimStart(u8, text, " \t\n");
    if (trimmed.len < 2 or trimmed[0] != '/') return false;
    return std.ascii.isAlphanumeric(trimmed[1]);
}

// ---------------------------------------------------------------- controls

fn interrupt(s: *Session, diag: ?*Diagnostics) errors.Error!void {
    if (!s.rt.busy()) return;
    var arena: std.heap.ArenaAllocator = .init(s.rt.env.gpa);
    defer arena.deinit();
    _ = try interruptWithOptions(ClaudeSession.of(s), arena.allocator(), "", false, diag);
}

/// Stops the running turn, which is `turn` or, when empty, the oldest
/// unfinished one; with `cancel_queued`, queued messages are cancelled too.
/// Returns the CLI's receipt.
pub fn interruptWithOptions(claude: *ClaudeSession, a: Allocator, turn: []const u8, cancel_queued: bool, diag: ?*Diagnostics) errors.Error![]const u8 {
    const marked = claude.rt().markInterrupt(turn);
    const payload = RawJson.of(if (cancel_queued) "{\"cancel_queued\":true}" else "{}");
    const raw = control.control(claude, a, "interrupt", payload, .{}, diag) catch |e| {
        if (!marked.isEmpty()) claude.rt().clearInterrupt(marked.slice());
        return e;
    };
    const receipt = JsonView.parseBytes(a, raw);
    for (receipt.get("cancelled").items()) |id| if (id == .string) {
        claude.rt().abandonTurn(id.string, .{ .kind = .cancelled, .message = "message cancelled by interrupt" });
    };
    return raw;
}

fn interruptTurn(s: *Session, id: []const u8, diag: ?*Diagnostics) errors.Error!void {
    if (!s.rt.hasTurn(id)) return;
    const claude = ClaudeSession.of(s);
    var arena: std.heap.ArenaAllocator = .init(claude.gpa());
    defer arena.deinit();
    // The CLI does not cancel a message it already started, so an
    // uncancelled turn that is still tracked is running. A CLI without the
    // cancel request answers with an error; interrupting is then the best
    // remaining choice.
    const cancelled = cancelQueuedMessage(claude, arena.allocator(), id, diag) catch |e| switch (e) {
        error.Protocol => false,
        else => return e,
    };
    if (cancelled or !s.rt.hasTurn(id)) return;
    _ = try interruptWithOptions(claude, arena.allocator(), id, false, diag);
}

/// Removes a queued user message before it runs.
pub fn cancelQueuedMessage(claude: *ClaudeSession, a: Allocator, uuid: []const u8, diag: ?*Diagnostics) errors.Error!bool {
    const raw = try control.control(claude, a, "cancel_async_message", try RawJson.encode(a, .{ .message_uuid = uuid }), .{}, diag);
    const cancelled = JsonView.parseBytes(a, raw).flag("cancelled");
    if (cancelled) claude.rt().abandonTurn(uuid, .{ .kind = .cancelled, .message = "queued message cancelled" });
    return cancelled;
}

/// Uniform efforts Claude lacks map to the nearest one.
pub fn nativeEffort(effort: []const u8) []const u8 {
    if (eql(effort, types.effort.none) or eql(effort, types.effort.minimal)) return types.effort.low;
    return effort;
}

fn setModel(s: *Session, model: []const u8, diag: ?*Diagnostics) errors.Error!void {
    const claude = ClaudeSession.of(s);
    var arena: std.heap.ArenaAllocator = .init(claude.gpa());
    defer arena.deinit();
    const a = arena.allocator();
    const payload = if (model.len > 0) try RawJson.encode(a, .{ .model = model }) else RawJson.of("{}");
    _ = try control.control(claude, a, "set_model", payload, .{}, diag);
    claude.setInfoField("model", model);
}

fn setEffort(s: *Session, effort: types.Effort, diag: ?*Diagnostics) errors.Error!void {
    const claude = ClaudeSession.of(s);
    var arena: std.heap.ArenaAllocator = .init(claude.gpa());
    defer arena.deinit();
    const a = arena.allocator();
    var settings: ObjectBuilder = .init(a);
    try settings.put("effortLevel", if (effort.len == 0) .null else .{ .string = nativeEffort(effort) });
    var payload: ObjectBuilder = .init(a);
    try payload.put("settings", settings.value());
    _ = try control.control(claude, a, "apply_flag_settings", try payload.toRaw(), .{}, diag);
    claude.setInfoField("effort", effort);
}

fn setPermissionMode(s: *Session, mode: types.PermissionMode, diag: ?*Diagnostics) errors.Error!void {
    const claude = ClaudeSession.of(s);
    const native = permissions.nativeMode(mode) orelse return errors.unsupported(diag, name, "permission mode read_only");
    if (permissions.isBypass(mode) and !claude.bypassAllowed(claude.cfg, claude.opts)) {
        return errors.fail(diag, .invalid_params, "claude: the bypass mode needs SessionOptions.allow_bypass_permissions when the session starts in another mode", .{});
    }
    var arena: std.heap.ArenaAllocator = .init(claude.gpa());
    defer arena.deinit();
    const a = arena.allocator();
    _ = try control.control(claude, a, "set_permission_mode", try RawJson.encode(a, .{ .mode = native }), .{}, diag);
    claude.setInfoField("permission_mode", mode);
}

fn setTitle(s: *Session, title: []const u8, diag: ?*Diagnostics) errors.Error!void {
    const claude = ClaudeSession.of(s);
    var arena: std.heap.ArenaAllocator = .init(claude.gpa());
    defer arena.deinit();
    const a = arena.allocator();
    const session_id = try claude.sessionId(a);
    _ = try control.control(claude, a, "rename_session", try RawJson.encode(a, .{ .title = title, .source = "host", .session_id = session_id }), .{}, diag);
    claude.setInfoField("title", title);
}

/// Compacts the conversation with the /compact command and waits for it.
fn compact(s: *Session, instructions: []const u8, diag: ?*Diagnostics) errors.Error!void {
    var arena: std.heap.ArenaAllocator = .init(s.rt.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const text = std.mem.trim(u8, try std.fmt.allocPrint(a, "/compact {s}", .{instructions}), " ");
    const turn = try s.send(.{ .text = text }, diag);
    _ = try turn.wait(a, .none, diag);
}

fn stopTask(s: *Session, task_id: []const u8, diag: ?*Diagnostics) errors.Error!void {
    const claude = ClaudeSession.of(s);
    // The CLI accepts any ID, so an unknown one is refused here.
    const known = blk: {
        claude.tasks_mutex.lockUncancelable(claude.io());
        defer claude.tasks_mutex.unlock(claude.io());
        break :blk claude.known_tasks.contains(task_id);
    };
    if (!known) return errors.fail(diag, .not_found, "claude: unknown task {s}", .{task_id});
    var arena: std.heap.ArenaAllocator = .init(claude.gpa());
    defer arena.deinit();
    const a = arena.allocator();
    _ = try control.control(claude, a, "stop_task", try RawJson.encode(a, .{ .task_id = task_id }), .{}, diag);
}

/// Starts a new session that continues this conversation. Empty fields of
/// the config are taken from this session. The fork's transcript is
/// written before it starts, so it persists even if it never runs a turn;
/// an ephemeral fork is the CLI's own unsaved fork instead.
fn fork(s: *Session, start_options: agent.StartOptions, diag: ?*Diagnostics) errors.Error!*Session {
    const claude = ClaudeSession.of(s);
    if (claude.cfg.ephemeral) return errors.unsupported(diag, name, "forking an ephemeral session");
    var arena: std.heap.ArenaAllocator = .init(claude.gpa());
    defer arena.deinit();
    const a = arena.allocator();
    const live = try s.rt.sessionInfo(a);
    var cfg = kit.inheritConfig(start_options.config, claude.cfg, live);
    const parent_id = try claude.sessionId(a);
    cfg.resume_id = parent_id;
    cfg.fork = true;
    cfg.session_id = start_options.config.session_id;
    cfg.title = start_options.config.title;
    // The parent's transcript, read once for the cut point; unread
    // without fork_at_turn, and an ephemeral fork needs none.
    var parent_graph: ?history.Graph = null;
    if (cfg.fork_at_turn.len > 0) {
        parent_graph = history.loadTranscript(claude.provider, a, claude.cfg, parent_id, null) catch null;
        // A turn of this session forks at its latest main assistant
        // message, once that message has reached the transcript.
        const cursor = try claude.translator().cursor(a, cfg.fork_at_turn);
        if (cursor.len > 0) if (parent_graph) |graph| if (graph.by_id.contains(cursor)) {
            cfg.fork_at_turn = cursor;
        };
    }
    var next = start_options;
    if (cfg.ephemeral) {
        next.config = cfg;
        return start(claude.provider, next, false, diag);
    }
    const up_to = if (cfg.fork_at_turn.len > 0) blk: {
        const graph = parent_graph orelse try history.loadTranscript(claude.provider, a, claude.cfg, parent_id, diag);
        break :blk try graph.forkPoint(a, cfg.fork_at_turn, diag);
    } else "";
    const fork_id = try history.forkTranscript(claude.provider, a, claude.cfg, parent_id, .{
        .up_to_message_id = up_to,
        .title = cfg.title,
        .session_id = cfg.session_id,
    }, diag);
    cfg.resume_id = fork_id;
    cfg.fork = false;
    cfg.fork_at_turn = "";
    cfg.session_id = "";
    next.config = cfg;
    return start(claude.provider, next, false, diag) catch |e| {
        // The fork did not start: its transcript would be an orphan.
        if (history.findTranscript(claude.provider, a, claude.cfg, fork_id, claude.cfg.cwd, null)) |path| {
            Io.Dir.cwd().deleteFile(claude.io(), path) catch |delete_error|
                log.err("claude: the transcript of fork {s}, which did not start, was not deleted: {t}", .{ fork_id, delete_error });
        } else |_| {}
        return e;
    };
}

// ---------------------------------------------------------------- the end

fn close(s: *Session) void {
    const claude = ClaudeSession.of(s);
    if (claude.closing.swap(true, .acq_rel)) return;
    // The process stops first, so control requests waiting for it fail
    // and no new message starts a task; the tasks left, such as shell
    // output tails, are cancelled.
    if (claude.current()) |launch| launch.process.stop(claude.provider.shutdownGraceMs());
    s.rt.finish(null);
    s.rt.closeTasks();
}

fn destroy(s: *Session) void {
    const claude = ClaudeSession.of(s);
    const gpa = claude.gpa();
    var next = claude.launch;
    while (next) |launch| {
        next = launch.previous;
        launch.destroy(gpa);
    }
    claude.waiters.deinit(gpa);
    claude.known_tasks.deinit(gpa);
    claude.turn_items.deinit(gpa);
    gpa.free(claude.init_raw);
    claude.arena.deinit();
    gpa.destroy(claude);
}

/// Starts a CLI process with a fresh translator that keeps the latest
/// launch's item lookups. The caller links it into the session, then sets
/// `ready`.
pub fn spawnLaunch(claude: *ClaudeSession, process_options: agent.process.Options, provisional: bool, diag: ?*Diagnostics) errors.Error!*Launch {
    const gpa = claude.gpa();
    const launch = try gpa.create(Launch);
    const translator = Translator.create(claude) catch |e| {
        gpa.destroy(launch);
        return e;
    };
    launch.* = .{ .claude = claude, .translator = translator, .exit_arena = .init(gpa), .line_arena = .init(gpa) };
    if (claude.current()) |previous| translator.keepLookups(previous.translator);
    if (provisional) launch.state.store(.provisional, .release);
    var with_tap = process_options;
    if (claude.cfg.wire_tap) with_tap.tap = .{ .context = launch, .line = Launch.tapLine };
    launch.process = agent.process.Process.start(claude.env(), with_tap, .{ .context = launch, .on_line = Launch.onLine, .on_exit = Launch.onExit }, diag) catch |e| {
        if (e == error.Internal) {
            // Reader tasks may have started and still hold `launch`: leak
            // it, marked so its handlers do nothing.
            launch.retired.store(true, .release);
            launch.ready.set(claude.io());
            return e;
        }
        translator.destroy();
        launch.exit_arena.deinit();
        launch.line_arena.deinit();
        gpa.destroy(launch);
        return e;
    };
    return launch;
}

test "the turn log finds items and drops from a turn" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var turn_log: TurnLog = .{};
    defer turn_log.deinit(gpa);
    turn_log.add(io, gpa, "t1", "t1");
    turn_log.add(io, gpa, "t1", "i1");
    turn_log.add(io, gpa, "t1", "i1");
    turn_log.add(io, gpa, "t2", "i2");
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();
    try std.testing.expectEqual(@as(usize, 5), turn_log.entries.items.len);
    try std.testing.expectEqualStrings("t1", try turn_log.turnOf(io, a, "i1"));
    try std.testing.expectEqualStrings("t2", try turn_log.turnOf(io, a, "t2"));
    try std.testing.expectEqualStrings("", try turn_log.turnOf(io, a, "missing"));
    const turns, const ids = try turn_log.drop(io, gpa, a, "t2");
    try std.testing.expectEqual(@as(usize, 1), turns.len);
    try std.testing.expectEqualStrings("t2", turns[0]);
    try std.testing.expectEqual(@as(usize, 2), ids.len);
    try std.testing.expectEqualStrings("", try turn_log.turnOf(io, a, "i2"));
    try std.testing.expectEqualStrings("t1", try turn_log.first(io, a));
    turn_log.add(io, gpa, "t2", "i2");
    try std.testing.expectEqualStrings("t2", try turn_log.turnOf(io, a, "i2"));
}
