//! One Codex thread as a `Session`: opening it, sending input (queue,
//! steer, interrupt), starting turns, and closing.
const std = @import("std");
const agent = @import("agent");
const decode = @import("decode.zig");
const output_schema = @import("output_schema.zig");
const control = @import("control.zig");
const translate = @import("translate.zig");
const options = @import("options.zig");
const Client = @import("client.zig").Client;
const Allocator = std.mem.Allocator;
const Io = std.Io;
const types = agent.types;
const errors = agent.errors;
const providerkit = agent.providerkit;
const Session = agent.Session;
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const ObjectBuilder = agent.json.ObjectBuilder;
const Id = agent.Id;
const Diagnostics = agent.Diagnostics;
const Value = std.json.Value;
const Error = errors.Error;
const eql = providerkit.eql;
const name = @import("root.zig").name;
const log = std.log.scoped(.agent);

/// Bounds a turn/start call, which is not abandoned once sent.
const start_timeout_ms = 60_000;

pub const CodexSession = struct {
    session: Session,
    env: agent.Env,
    client: *Client = undefined,
    /// The session owns a dedicated app-server.
    owned: bool,
    experimental: bool,
    /// The start config and session options, copied.
    cfg_arena: std.heap.ArenaAllocator,
    cfg: types.Config,
    opts: options.SessionOptions = .{},
    /// `cfg.output_schema` as sent, for turns without their own.
    output_schema: ?RawJson = null,

    /// Guards the fields below and the translator.
    mutex: Io.Mutex = .init,
    /// Changes when a rewind falls back to a fork.
    thread: Id = .{},
    model: Id = .{},
    effort: Id = .{},
    plan: bool = false,
    /// The thread's native sandbox policy, and the latest workspace-write
    /// policy seen or read from config, so mode changes keep writable
    /// roots and network access. JSON owned by the session allocator.
    sandbox: ?[]u8 = null,
    workspace_write: ?[]u8 = null,
    read_workspace: bool = false,
    /// The last additional context sent, restored after compaction.
    context: ?[]u8 = null,
    /// Set while a compact call waits for its compaction.
    compacting: bool = false,
    compact_generation: u64 = 0,
    compact_condition: Io.Condition = .init,
    translator: translate.Translator = .{},

    closed: std.atomic.Value(bool) = .init(false),

    pub const vtable: Session.VTable = .{
        .send = send,
        .interrupt = interrupt,
        .interrupt_turn = interruptTurnEntry,
        .respond = translate.respond,
        .close = close,
        .destroy = destroy,
        .set_model = control.setModel,
        .set_effort = control.setEffort,
        .set_permission_mode = control.setPermissionMode,
        .set_title = control.setTitle,
        .compact = control.compact,
        .rewind = control.rewind,
        .stop_task = control.stopTask,
        .fork = control.fork,
        .rate_limits = control.rateLimits,
        .call = control.call,
    };

    pub fn of(session: *Session) *CodexSession {
        return @fieldParentPtr("session", session);
    }

    pub fn rt(codex: *CodexSession) *agent.runtime.Runtime {
        return &codex.session.rt;
    }

    /// Allocates a session that is not open yet; `config` is copied.
    pub fn create(env: agent.Env, start: agent.StartOptions, owned: bool, experimental: bool, capabilities: types.Capabilities, diag: ?*Diagnostics) Error!*CodexSession {
        const gpa = env.gpa;
        const codex = try gpa.create(CodexSession);
        codex.* = .{
            .session = .init(env, name, &vtable, capabilities, start),
            .env = env,
            .owned = owned,
            .experimental = experimental,
            .cfg_arena = .init(gpa),
            .cfg = undefined,
        };
        errdefer codex.discard();
        const a = codex.cfg_arena.allocator();
        codex.cfg = try agent.clone(types.Config, a, start.config);
        codex.opts = try providerkit.options(options.SessionOptions, a, name, codex.cfg.provider_options, diag);
        codex.plan = std.mem.eql(u8, codex.cfg.permission_mode, types.permission_mode.plan);
        codex.effort = .of(codex.cfg.effort);
        codex.translator.have_total = codex.cfg.resume_id.len == 0;
        return codex;
    }

    /// Frees a session that never opened.
    pub fn discard(codex: *CodexSession) void {
        codex.session.rt.finish(null);
        codex.session.rt.deinit();
        codex.freeState();
    }

    fn freeState(codex: *CodexSession) void {
        const gpa = codex.env.gpa;
        codex.translator.deinit(gpa);
        for ([_]?[]u8{ codex.sandbox, codex.workspace_write, codex.context }) |buffer| if (buffer) |bytes| gpa.free(bytes);
        codex.cfg_arena.deinit();
        gpa.destroy(codex);
    }

    pub fn threadId(codex: *CodexSession) Id {
        codex.mutex.lockUncancelable(codex.env.io);
        defer codex.mutex.unlock(codex.env.io);
        return codex.thread;
    }

    /// Starts, resumes, or forks the thread on `client`, which the session
    /// keeps a reference to, and delivers `session_started`.
    pub fn open(codex: *CodexSession, client: *Client, diag: ?*Diagnostics) Error!void {
        codex.client = client;
        var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        const cfg = codex.cfg;
        if (try checkConfig(a, cfg, codex.owned, codex.experimental, diag)) |notice| codex.rt().emitPayload(notice, null);
        codex.output_schema = try codex.strictSchema(codex.cfg_arena.allocator(), cfg.output_schema, diag);
        codex.rt().setStarter(.{ .context = codex, .start = startQueued });

        var params = try codex.threadParams(a, cfg, codex.opts);
        var method: []const u8 = "thread/start";
        if (cfg.resume_id.len > 0) {
            method = if (cfg.fork) "thread/fork" else "thread/resume";
            try params.str("threadId", cfg.resume_id);
        }
        var call_diag: Diagnostics = .{};
        const result = client.call(a, method, params.value(), .{ .owner = codex }, &call_diag) catch |e| retry: {
            const info = Diagnostics.of(&call_diag, e);
            if (std.mem.eql(u8, method, "thread/resume") and isArchived(info)) {
                if (control.unarchive(codex, a, cfg.resume_id, null)) {
                    break :retry client.call(a, method, params.value(), .{ .owner = codex }, &call_diag) catch |retry_error|
                        return openFailed(a, method, cfg.resume_id, Diagnostics.of(&call_diag, retry_error), diag);
                } else |_| {}
            }
            return openFailed(a, method, cfg.resume_id, info, diag);
        };
        const response = JsonView.parseBytes(a, result.bytes);
        const thread = response.get("thread");
        const id = thread.str("id");
        if (id.len == 0) return errors.fail(diag, .protocol, "codex: {s} returned no thread ID", .{method});
        codex.mutex.lockUncancelable(codex.env.io);
        codex.thread = .of(id);
        codex.model = .of(response.str("model"));
        if (codex.effort.isEmpty()) codex.effort = .of(response.str("reasoningEffort"));
        const effort = codex.effort;
        codex.mutex.unlock(codex.env.io);
        codex.rememberSandbox(a, response.get("sandbox"));
        codex.rt().setSessionId(id);

        var tools: std.ArrayList([]const u8) = .empty;
        for (cfg.tools) |tool| try tools.append(a, tool.name);
        const info: types.SessionInfo = .{
            .id = id,
            .provider = name,
            .cwd = providerkit.firstOf(&.{ response.str("cwd"), thread.str("cwd"), cfg.cwd }),
            .model = response.str("model"),
            .effort = effort.slice(),
            .permission_mode = providerkit.firstOf(&.{ cfg.permission_mode, modeOf(response.get("approvalPolicy"), response.get("sandbox")) }),
            .title = providerkit.firstOf(&.{ cfg.title, thread.str("name") }),
            .version = providerkit.firstOf(&.{ thread.str("cliVersion"), client.version() }),
            .tools = tools.items,
            .raw = result,
        };
        codex.rt().setInfo(info, false);
        // Nothing below fails: a registered session is not discarded.
        client.register(codex, id);

        codex.rt().spawn(control.loadSkills, .{codex}) catch
            log.warn("codex: cannot start a task to list skills; the session has no skill commands", .{});
        if (cfg.title.len > 0 and std.mem.eql(u8, method, "thread/start")) {
            var title_diag: Diagnostics = .{};
            control.setTitle(&codex.session, cfg.title, &title_diag) catch |e|
                log.warn("codex: setting the thread name failed: {s}", .{Diagnostics.of(&title_diag, e).message});
        }
        codex.rt().markStarted(codex.rt().sessionInfo(a) catch info, cfg.resume_id.len > 0);
    }

    fn openFailed(a: Allocator, method: []const u8, resume_id: []const u8, info: types.ErrorInfo, diag: ?*Diagnostics) Error {
        var wrapped = info;
        if (!std.mem.eql(u8, method, "thread/start") and isNoRollout(info)) {
            wrapped.kind = .session_not_found;
            wrapped.message = std.fmt.allocPrint(a, "codex: {s} {s}: session not found: {s}", .{ method, resume_id, info.message }) catch info.message;
        } else {
            wrapped.message = std.fmt.allocPrint(a, "codex: {s}: {s}", .{ method, info.message }) catch info.message;
        }
        return errors.failWith(diag, wrapped);
    }

    /// The schema as sent on turn/start: normalized to strict form unless
    /// `disable_strict_output_schema` is set.
    pub fn strictSchema(codex: *CodexSession, a: Allocator, schema: ?RawJson, diag: ?*Diagnostics) Error!?RawJson {
        const raw = schema orelse return null;
        if (raw.bytes.len == 0) return null;
        if (codex.opts.disable_strict_output_schema) return raw;
        return try output_schema.strictOutputSchema(a, raw, diag);
    }

    /// Sends a request about this session's thread, refusing once the
    /// session ended: a shared app-server would otherwise still act on it.
    pub fn threadCall(codex: *CodexSession, a: Allocator, method: []const u8, params: ?Value, timeout_ms: i64, diag: ?*Diagnostics) Error!JsonView {
        try codex.rt().ensureOpen(diag);
        return codex.client.callJson(a, method, params, .{ .owner = codex, .timeout_ms = timeout_ms }, diag);
    }

    /// A params object holding this session's thread ID.
    pub fn threadIdParams(codex: *CodexSession, a: Allocator) Allocator.Error!ObjectBuilder {
        var params: ObjectBuilder = .init(a);
        try params.str("threadId", try a.dupe(u8, codex.threadId().slice()));
        return params;
    }

    /// Fields of the session info to change; null leaves a field as it is.
    pub const InfoChange = struct {
        id: ?[]const u8 = null,
        model: ?[]const u8 = null,
        effort: ?[]const u8 = null,
        permission_mode: ?[]const u8 = null,
        title: ?[]const u8 = null,
        commands: ?[]const types.SlashCommand = null,
        /// Added, or replaces the status of the server with the same name.
        mcp_server: ?types.McpServerStatus = null,
    };

    /// Applies `change` to the session info and reports it with
    /// `session_updated`. Safe with the session lock held.
    pub fn updateInfo(codex: *CodexSession, change: InfoChange) void {
        codex.rt().updateInfo(change, true, applyInfoChange) catch
            log.err("codex: updating the session info failed: out of memory", .{});
    }

    fn applyInfoChange(change: InfoChange, info: *types.SessionInfo, a: Allocator) Allocator.Error!void {
        if (change.id) |id| info.id = try a.dupe(u8, id);
        if (change.model) |model| info.model = try a.dupe(u8, model);
        if (change.effort) |effort| info.effort = try a.dupe(u8, effort);
        if (change.permission_mode) |mode| info.permission_mode = try a.dupe(u8, mode);
        if (change.title) |title| info.title = try a.dupe(u8, title);
        if (change.commands) |commands| info.commands = try agent.clone([]const types.SlashCommand, a, commands);
        if (change.mcp_server) |status| info.mcp_servers = try withServerStatus(a, info.mcp_servers, try agent.clone(types.McpServerStatus, a, status));
    }

    /// Whether `turn` is a turn of this thread.
    pub fn ownsTurn(codex: *CodexSession, turn: []const u8) bool {
        return turn.len > 0 and codex.rt().hasTurn(turn);
    }

    /// Ends the session because the app-server exited or was closed.
    pub fn endFromClient(codex: *CodexSession, err: ?types.ErrorInfo) void {
        codex.rt().finish(err);
        codex.mutex.lockUncancelable(codex.env.io);
        codex.compact_condition.broadcast(codex.env.io);
        codex.mutex.unlock(codex.env.io);
    }

    // ------------------------------------------------------------ settings

    /// thread/start, thread/resume, or thread/fork parameters.
    pub fn threadParams(codex: *CodexSession, a: Allocator, cfg: types.Config, session_options: options.SessionOptions) Allocator.Error!ObjectBuilder {
        const env = codex.env;
        var params: ObjectBuilder = .init(a);
        try params.strIf("model", cfg.model);
        if (cfg.cwd.len > 0) try params.str("cwd", providerkit.absDir(env, a, cfg.cwd));
        var permission = permissionSettings(cfg.permission_mode);
        var approval: ?Value = if (permission.approval.len > 0) .{ .string = permission.approval } else null;
        if (agent.json.valueOf(a, session_options.approval_policy)) |policy| approval = policy;
        if (session_options.sandbox.len > 0) permission.sandbox = session_options.sandbox;
        if (session_options.approvals_reviewer.len > 0) permission.reviewer = session_options.approvals_reviewer;
        if (approval) |policy| try params.put("approvalPolicy", policy);
        if (session_options.permissions.len > 0) try params.str("permissions", session_options.permissions) else try params.strIf("sandbox", permission.sandbox);
        if (permission.reviewer.len == 0 and cfg.permission_mode.len > 0 and !std.mem.eql(u8, cfg.permission_mode, types.permission_mode.auto)) {
            // The reviewer is sticky: a resumed thread keeps auto review
            // unless an explicit mode pins the user.
            permission.reviewer = "user";
        }
        try params.strIf("approvalsReviewer", permission.reviewer);
        try params.strIf("baseInstructions", cfg.system_prompt);
        try params.strIf("developerInstructions", cfg.append_system_prompt);
        try params.strIf("modelProvider", session_options.model_provider);
        try params.strIf("serviceTier", session_options.service_tier);
        // thread/fork takes no personality.
        if (!cfg.fork) try params.strIf("personality", session_options.personality);
        if (cfg.additional_directories.len > 0 and cfg.cwd.len > 0) {
            var roots: std.ArrayList([]const u8) = .empty;
            try roots.append(a, providerkit.absDir(env, a, cfg.cwd));
            for (cfg.additional_directories) |directory| try roots.append(a, providerkit.absDir(env, a, directory));
            try params.put("runtimeWorkspaceRoots", try agent.json.stringArray(a, roots.items));
        }
        var config: ObjectBuilder = .init(a);
        try config.merge(session_options.config);
        if (cfg.mcp_servers) |servers| for (servers.map.keys(), servers.map.values()) |server_name, server| {
            try config.put(try std.fmt.allocPrint(a, "mcp_servers.{s}", .{server_name}), try mcpServerConfig(a, server));
        };
        codex.mutex.lockUncancelable(env.io);
        const effort = try a.dupe(u8, codex.effort.slice());
        codex.mutex.unlock(env.io);
        try config.strIf("model_reasoning_effort", effort);
        try config.strIf("model_reasoning_summary", session_options.reasoning_summary);
        if (config.map.count() > 0) try params.put("config", config.value());
        if ((cfg.resume_id.len == 0 or cfg.fork) and cfg.ephemeral) try params.put("ephemeral", .{ .bool = true });
        if (cfg.resume_id.len > 0) {
            // The session never reads the history the response would carry.
            try params.put("excludeTurns", .{ .bool = true });
            if (cfg.fork) try params.strIf("lastTurnId", cfg.fork_at_turn);
        } else {
            try params.strIf("historyMode", session_options.history_mode);
            try params.strIf("serviceName", session_options.service_name);
            if (cfg.tools.len > 0 and codex.experimental) {
                var specs: std.json.Array = .init(a);
                for (cfg.tools) |tool| try specs.append(try agent.json.toValue(a, .{
                    .type = "function",
                    .name = tool.name,
                    .description = tool.description,
                    .inputSchema = agent.json.valueOf(a, tool.input_schema) orelse (agent.json.parseValue(a, "{\"type\":\"object\"}").?),
                }));
                try params.put("dynamicTools", .{ .array = specs });
            }
        }
        try params.merge(session_options.extra_thread_params);
        return params;
    }

    /// Records the native sandbox policy the thread runs with.
    pub fn rememberSandbox(codex: *CodexSession, a: Allocator, policy: JsonView) void {
        if (policy.object() == null) return;
        const bytes = agent.json.encodeValue(a, policy.v.?) catch return;
        codex.mutex.lockUncancelable(codex.env.io);
        defer codex.mutex.unlock(codex.env.io);
        codex.setSandboxLocked(bytes);
    }

    pub fn setSandboxLocked(codex: *CodexSession, policy: []const u8) void {
        const gpa = codex.env.gpa;
        const copy = gpa.dupe(u8, policy) catch return;
        if (codex.sandbox) |old| gpa.free(old);
        codex.sandbox = copy;
        var scratch: std.heap.ArenaAllocator = .init(gpa);
        defer scratch.deinit();
        if (std.mem.eql(u8, JsonView.parseBytes(scratch.allocator(), policy).str("type"), "workspaceWrite")) {
            const workspace_write = gpa.dupe(u8, policy) catch return;
            if (codex.workspace_write) |old| gpa.free(old);
            codex.workspace_write = workspace_write;
        }
    }

    /// The native policy for a sandbox mode. A policy of the type the
    /// thread already runs with is reused, and workspace-write keeps the
    /// writable roots, network access, and tmp exclusions last seen on the
    /// thread or configured as sandbox_workspace_write.
    pub fn resolveSandbox(codex: *CodexSession, a: Allocator, mode: []const u8) ?Value {
        const default_policy = sandboxPolicy(a, mode) orelse return null;
        const want = JsonView.of(default_policy).str("type");
        codex.mutex.lockUncancelable(codex.env.io);
        const current = if (codex.sandbox) |bytes| a.dupe(u8, bytes) catch null else null;
        var workspace_write: ?[]const u8 = if (codex.workspace_write) |bytes| a.dupe(u8, bytes) catch null else null;
        const read = codex.read_workspace;
        codex.mutex.unlock(codex.env.io);
        if (current) |bytes| if (std.mem.eql(u8, JsonView.parseBytes(a, bytes).str("type"), want)) return agent.json.parseValue(a, bytes);
        if (!std.mem.eql(u8, want, "workspaceWrite")) return default_policy;
        if (workspace_write == null and !read) {
            workspace_write = control.configuredWorkspaceWrite(codex, a);
            codex.mutex.lockUncancelable(codex.env.io);
            defer codex.mutex.unlock(codex.env.io);
            codex.read_workspace = true;
            if (codex.workspace_write == null) if (workspace_write) |bytes| {
                codex.workspace_write = codex.env.gpa.dupe(u8, bytes) catch null;
            };
        }
        if (workspace_write) |bytes| return agent.json.parseValue(a, bytes);
        return default_policy;
    }

    // ------------------------------------------------------------ turns

    /// Submits input as turn `turn_id`: starts it when idle, else queues,
    /// steers, or interrupts according to `in.delivery`.
    pub fn submit(codex: *CodexSession, in: types.Input, turn_id: []const u8, diag: ?*Diagnostics) Error!void {
        return codex.rt().submit(turn_id, in, diag, Delivery{ .codex = codex });
    }

    /// How `Runtime.submit` delivers input to Codex.
    const Delivery = struct {
        codex: *CodexSession,

        pub fn start(delivery: Delivery, id: []const u8, in: types.Input, diag: ?*Diagnostics) Error!void {
            return delivery.codex.startTurn(id, in, diag);
        }

        pub fn steer(delivery: Delivery, id: []const u8, in: types.Input, running: []const u8, diag: ?*Diagnostics) Error!void {
            return delivery.codex.steer(in, id, running, diag);
        }

        pub fn interrupt(delivery: Delivery, running: []const u8, diag: ?*Diagnostics) Error!void {
            return delivery.codex.interruptTurn(running, diag);
        }
    };

    /// Injects input into the running turn `active` with turn/steer. When
    /// the turn ended before the steer arrived, the input runs as the next
    /// turn instead. The steered input's turn ends with the turn it joined.
    fn steer(codex: *CodexSession, in: types.Input, turn_id: []const u8, active: []const u8, diag: ?*Diagnostics) Error!void {
        var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        var params = try codex.threadIdParams(a);
        try params.put("input", try userInput(codex.env, a, try in.allParts(a), diag));
        try params.str("expectedTurnId", active);
        try params.strIf("clientUserMessageId", in.client_id);
        var call_diag: Diagnostics = .{};
        const response = codex.client.callJson(a, "turn/steer", params.value(), .{ .owner = codex }, &call_diag) catch |e| {
            const info = Diagnostics.of(&call_diag, e);
            if (isInvalidRequest(info, "no active turn to steer")) return codex.rt().enqueue(turn_id, in, .{}, diag);
            if (!isSteerRejected(info)) return forward(diag, info, e);
            var refused = info;
            refused.kind = .steer_rejected;
            return errors.failWith(diag, refused);
        };
        try codex.rt().joinTurn(turn_id, providerkit.firstOf(&.{ response.str("turnId"), active }), diag);
    }

    /// Calls turn/start for turn `id`, which holds the running slot.
    pub fn startTurn(codex: *CodexSession, id: []const u8, in: types.Input, diag: ?*Diagnostics) Error!void {
        var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        const params = try codex.turnParams(a, in, diag);
        translate.beginStart(codex, params.has("outputSchema"));
        // Once sent, the start is not abandoned: Codex may already run the
        // turn, and a later start could be mistaken for it.
        const response = codex.client.callJson(a, "turn/start", params.value(), .{ .owner = codex, .timeout_ms = start_timeout_ms }, diag) catch |e| {
            translate.endStart(codex, "");
            return e;
        };
        const native = response.get("turn").str("id");
        translate.endStart(codex, native);
        const sandbox: ?[]const u8 = if (params.map.get("sandboxPolicy")) |policy| (if (policy == .object) try agent.json.encodeValue(a, policy) else null) else null;
        codex.accepted(id, native, sandbox);
    }

    /// Completes the start of turn `id`, which Codex started as `native`
    /// with the sandbox policy `sandbox`, when set.
    pub fn accepted(codex: *CodexSession, id: []const u8, native: []const u8, sandbox: ?[]const u8) void {
        codex.mutex.lockUncancelable(codex.env.io);
        // While a start is in flight the translator leaves turn tracking
        // to it, so the turn is registered exactly once. A turn that failed
        // instantly may have completed before it was registered; the
        // runtime held its result back for this moment.
        const stop = codex.rt().accepted(id, native);
        if (sandbox) |policy| codex.setSandboxLocked(policy);
        codex.mutex.unlock(codex.env.io);
        if (stop) {
            // This is not the reader task, so the interrupt can run here
            // when no task can start.
            codex.rt().spawn(interruptStarted, .{ codex, Id.of(native) }) catch interruptStarted(codex, .of(native));
        }
    }

    /// Interrupts a turn that an interrupt was asked for while its
    /// turn/start was in flight.
    fn interruptStarted(codex: *CodexSession, id: Id) void {
        var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
        defer arena.deinit();
        var diag: Diagnostics = .{};
        sendInterrupt(codex, arena.allocator(), id.slice(), &diag) catch |e| {
            const info = Diagnostics.of(&diag, e);
            if (isInvalidRequest(info, "no active turn to interrupt")) return;
            codex.rt().clearInterrupt(id.slice());
            codex.rt().emitPayload(.{ .notice = .{ .level = .@"error", .code = "interrupt_failed", .message = info.message } }, null);
        };
    }

    fn sendInterrupt(codex: *CodexSession, a: Allocator, turn: []const u8, diag: ?*Diagnostics) Error!void {
        var params = try codex.threadIdParams(a);
        try params.str("turnId", turn);
        _ = try codex.client.call(a, "turn/interrupt", params.value(), .{ .owner = codex, .timeout_ms = 10_000 }, diag);
    }

    fn turnParams(codex: *CodexSession, a: Allocator, in: types.Input, diag: ?*Diagnostics) Error!ObjectBuilder {
        const io = codex.env.io;
        var params = try codex.threadIdParams(a);
        try params.put("input", try userInput(codex.env, a, try in.allParts(a), diag));
        const turn_schema = if (in.output_schema != null) try codex.strictSchema(a, in.output_schema, diag) else codex.output_schema;
        if (turn_schema) |schema| try params.put("outputSchema", agent.json.parseValue(a, schema.bytes) orelse return errors.fail(diag, .invalid_params, "codex: invalid output schema", .{}));
        try params.strIf("clientUserMessageId", in.client_id);
        codex.mutex.lockUncancelable(io);
        // Codex keeps a turn's model and effort for later turns.
        if (in.model.len > 0) codex.model = .of(in.model);
        if (in.effort.len > 0) codex.effort = .of(in.effort);
        const plan = codex.plan;
        const model = try a.dupe(u8, codex.model.slice());
        const effort = try a.dupe(u8, codex.effort.slice());
        codex.mutex.unlock(io);
        if (in.model.len > 0 or in.effort.len > 0) codex.updateInfo(.{
            .model = if (in.model.len > 0) in.model else null,
            .effort = if (in.effort.len > 0) in.effort else null,
        });
        if (in.model.len > 0) try params.str("model", in.model);
        try params.strIf("effort", effort);
        try params.strIf("summary", codex.opts.reasoning_summary);
        if (plan) try params.put("collaborationMode", try collaborationMode(a, "plan", model, effort));
        try params.merge(codex.opts.extra_turn_params);
        const turn_options = try providerkit.options(options.TurnOptions, a, name, in.provider_options, diag);
        if (agent.json.valueOf(a, turn_options.collaboration_mode)) |mode| try params.put("collaborationMode", mode);
        try params.strIf("summary", turn_options.reasoning_summary);
        try params.strIf("serviceTierForTurn", turn_options.service_tier);
        try params.strIf("serviceTier", turn_options.sticky_service_tier);
        if (agent.json.valueOf(a, turn_options.approval_policy)) |policy| try params.put("approvalPolicy", policy);
        if (agent.json.valueOf(a, turn_options.sandbox_policy)) |policy| {
            if (policy == .string) {
                if (codex.resolveSandbox(a, policy.string)) |resolved| try params.put("sandboxPolicy", resolved);
            } else if (policy != .null) try params.put("sandboxPolicy", policy);
        }
        try params.strIf("approvalsReviewer", turn_options.approvals_reviewer);
        if (turn_options.cwd.len > 0) try params.str("cwd", providerkit.absDir(codex.env, a, turn_options.cwd));
        if (agent.json.valueOf(a, turn_options.additional_context)) |context| if (context == .object and context.object.count() > 0) {
            try params.put("additionalContext", context);
            const bytes = try codex.env.gpa.dupe(u8, try agent.json.encodeValue(a, context));
            codex.mutex.lockUncancelable(io);
            if (codex.context) |old| codex.env.gpa.free(old);
            codex.context = bytes;
            codex.mutex.unlock(io);
        };
        try params.merge(turn_options.extra);
        return params;
    }

    // ------------------------------------------------------------ interrupt

    /// Interrupts the running turn when it is `want` (any when empty) and
    /// the running turns of its subagents. A turn whose turn/start is in
    /// flight is interrupted once Codex reports its ID; a turn that already
    /// ended is not an error.
    pub fn interruptTurn(codex: *CodexSession, want: []const u8, diag: ?*Diagnostics) Error!void {
        var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        codex.mutex.lockUncancelable(codex.env.io);
        const active, const starting = codex.rt().requestInterrupt(want);
        const children = translate.childTurnsLocked(codex, a);
        codex.mutex.unlock(codex.env.io);
        if (active.isEmpty() or starting) return;
        var params = try codex.threadIdParams(a);
        try params.str("turnId", active.slice());
        // A refused interrupt is cleared as its reply is read, so a
        // turn/completed that follows it is not taken as interrupted.
        const Clear = struct {
            runtime: *agent.runtime.Runtime,
            id: Id,
            fn run(context: *anyopaque, failed: bool) void {
                const clear: *@This() = @ptrCast(@alignCast(context));
                if (failed) clear.runtime.clearInterrupt(clear.id.slice());
            }
        };
        var clear: Clear = .{ .runtime = codex.rt(), .id = active };
        var result: Error!void = {};
        var call_diag: Diagnostics = .{};
        _ = codex.client.call(a, "turn/interrupt", params.value(), .{ .owner = codex, .hook = .{ .context = &clear, .run = Clear.run } }, &call_diag) catch |e| {
            codex.rt().clearInterrupt(active.slice());
            const info = Diagnostics.of(&call_diag, e);
            if (!isInvalidRequest(info, "no active turn to interrupt")) result = forward(diag, info, e);
        };
        for (children) |child| {
            const child_params = try agent.json.toValue(a, .{ .threadId = child.thread, .turnId = child.turn });
            _ = codex.client.call(a, "turn/interrupt", child_params, .{ .owner = codex }, null) catch |e|
                log.debug("codex: interrupting subagent turn of {s} failed: {t}", .{ child.thread, e });
        }
        return result;
    }

    // ------------------------------------------------------------ close

    /// Ends the session. A dedicated app-server is stopped; on a shared one
    /// the thread's pending requests are refused, its turn interrupted, and
    /// the thread unsubscribed.
    fn shutdown(codex: *CodexSession) void {
        if (codex.closed.swap(true, .acq_rel)) return;
        const client = codex.client;
        const id = codex.threadId();
        client.unregister(id.slice());
        if (codex.owned) {
            client.close();
        } else {
            var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
            defer arena.deinit();
            const a = arena.allocator();
            // Best-effort cleanup of a thread the session leaves: the
            // app-server drops what is left when the thread is
            // unsubscribed below.
            const pending = codex.rt().pendingRequests(a) catch &.{};
            for (pending) |event| {
                const response: types.Response = switch (event.payload) {
                    .permission_request => .{ .permission = .{ .decision = .cancel } },
                    .user_input_request => .{ .user_input = .{ .cancelled = true } },
                    .elicitation_request => .{ .elicitation = .{ .action = .cancel } },
                    else => continue,
                };
                const request_id = switch (event.payload) {
                    inline .permission_request, .user_input_request, .elicitation_request => |request| request.id,
                    else => continue,
                };
                codex.session.respond(request_id, response, null) catch {};
            }
            codex.interruptTurn("", null) catch {};
            if (codex.threadIdParams(a)) |params| {
                _ = client.call(a, "thread/unsubscribe", params.value(), .{ .owner = codex, .timeout_ms = client.shutdownGraceMs() }, null) catch {};
            } else |_| {}
            client.forget(id.slice());
        }
        // Session tasks end before the session's memory goes. Their
        // app-server calls fail from now on, so none waits for an answer
        // that never comes. The session ends first: a request handler that
        // waits on a person returns once it sees the session closed.
        client.cancelOwner(codex);
        codex.rt().finish(null);
        codex.rt().closeTasks();
        client.unregisterSession(codex);
        client.quiesce();
        codex.mutex.lockUncancelable(codex.env.io);
        codex.compact_condition.broadcast(codex.env.io);
        codex.mutex.unlock(codex.env.io);
    }
};

// ---------------------------------------------------------------- vtable

fn send(session: *Session, in: types.Input, turn_id: []const u8, diag: ?*Diagnostics) Error!void {
    const codex = CodexSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    if ((try in.allParts(a)).len == 0) return errors.fail(diag, .invalid_params, "codex: input has no content", .{});
    _ = try providerkit.options(options.TurnOptions, a, name, in.provider_options, diag);
    return codex.submit(in, turn_id, diag);
}

/// Starts queued input as turn `id`.
fn startQueued(context: *anyopaque, id: []const u8, in: types.Input, diag: ?*Diagnostics) Error!void {
    const codex: *CodexSession = @ptrCast(@alignCast(context));
    var start_diag: Diagnostics = .{};
    codex.startTurn(id, in, &start_diag) catch |e| {
        const info = Diagnostics.of(&start_diag, e);
        codex.rt().emitPayload(.{ .notice = .{ .level = .@"error", .code = "queued_turn_failed", .message = info.message } }, null);
        return errors.failWith(diag, info);
    };
}

fn interrupt(session: *Session, diag: ?*Diagnostics) Error!void {
    return CodexSession.of(session).interruptTurn("", diag);
}

fn interruptTurnEntry(session: *Session, turn_id: []const u8, diag: ?*Diagnostics) Error!void {
    return CodexSession.of(session).interruptTurn(turn_id, diag);
}

fn close(session: *Session) void {
    CodexSession.of(session).shutdown();
}

fn destroy(session: *Session) void {
    const codex = CodexSession.of(session);
    codex.client.release();
    codex.freeState();
}

// ---------------------------------------------------------------- helpers

/// Refuses the config restrictions Codex cannot honor, because a
/// restriction is never ignored, and returns the notice for ignored
/// fields. `owned` is false on an app-server the session does not own,
/// whose environment it cannot change.
pub fn checkConfig(a: Allocator, cfg: types.Config, owned: bool, experimental: bool, diag: ?*Diagnostics) Error!?types.Event.Payload {
    if (cfg.session_id.len > 0) return errors.unsupported(diag, name, "choosing the session ID");
    if (cfg.fork and cfg.resume_id.len == 0) return errors.fail(diag, .invalid_params, "codex: fork requires resume_id", .{});
    if (cfg.fork_at_turn.len > 0 and !cfg.fork) return errors.fail(diag, .invalid_params, "codex: fork_at_turn requires fork", .{});
    if (cfg.disallowed_tools.len > 0) return errors.unsupported(diag, name, "disallowed_tools");
    if (!owned and cfg.unset_env.len > 0) return errors.unsupported(diag, name, "unset_env on a shared app-server");
    var ignored: std.ArrayList([]const u8) = .empty;
    if (cfg.tools.len > 0 and !experimental) try ignored.append(a, "tools (dynamic tools need the experimental API)");
    if (cfg.allowed_tools.len > 0) try ignored.append(a, "allowed_tools");
    if (cfg.mcp_servers) |servers| {
        for (try providerkit.sortedKeys(a, servers.map.keys())) |key| if (servers.map.get(key).?.always_load) try ignored.append(a, try std.fmt.allocPrint(a, "mcp_servers[{s}].always_load", .{key}));
    }
    if (!owned and cfg.env != null and cfg.env.?.map.count() > 0) try ignored.append(a, "env on a shared app-server");
    if (!owned and cfg.wire_tap) try ignored.append(a, "wire_tap on a shared app-server");
    return providerkit.ignoredConfig(a, name, cfg, ignored.items, diag);
}

pub const PermissionSettings = struct { approval: []const u8 = "", sandbox: []const u8 = "", reviewer: []const u8 = "" };

/// The approval policy, sandbox mode, and reviewer of a permission mode.
pub fn permissionSettings(mode: []const u8) PermissionSettings {
    const modes = types.permission_mode;
    if (mode.len == 0) return .{};
    if (eql(mode, modes.default) or eql(mode, modes.accept_edits)) return .{ .approval = "on-request", .sandbox = "workspace-write" };
    // Plan mode promises no changes, so it runs read-only.
    if (eql(mode, modes.read_only) or eql(mode, modes.plan)) return .{ .approval = "on-request", .sandbox = "read-only" };
    if (eql(mode, modes.dont_ask)) return .{ .approval = "never", .sandbox = "workspace-write" };
    if (eql(mode, modes.bypass)) return .{ .approval = "never", .sandbox = "danger-full-access" };
    if (eql(mode, modes.auto)) return .{ .approval = "on-request", .sandbox = "workspace-write", .reviewer = "auto_review" };
    // Native approval policies pass through.
    return .{ .approval = mode };
}

pub fn sandboxPolicy(a: Allocator, mode: []const u8) ?Value {
    const text: []const u8 = if (eql(mode, "read-only"))
        \\{"type":"readOnly","networkAccess":false}
    else if (eql(mode, "danger-full-access"))
        \\{"type":"dangerFullAccess"}
    else if (eql(mode, "workspace-write"))
        \\{"type":"workspaceWrite","writableRoots":[],"networkAccess":false,"excludeTmpdirEnvVar":false,"excludeSlashTmp":false}
    else
        return null;
    return agent.json.parseValue(a, text);
}

/// The permission mode an approval policy and sandbox amount to: a shared
/// mode where one matches, else the native approval policy.
fn modeOf(approval: JsonView, sandbox: JsonView) []const u8 {
    const policy = approval.string();
    if (approval.v == null or approval.v.? != .string) return "";
    const sandbox_type = sandbox.str("type");
    const modes = types.permission_mode;
    if (eql(policy, "on-request") and eql(sandbox_type, "readOnly")) return modes.read_only;
    if (eql(policy, "on-request") and eql(sandbox_type, "workspaceWrite")) return modes.default;
    if (eql(policy, "never") and eql(sandbox_type, "workspaceWrite")) return modes.dont_ask;
    if (eql(policy, "never") and eql(sandbox_type, "dangerFullAccess")) return modes.bypass;
    return policy;
}

pub fn collaborationMode(a: Allocator, mode: []const u8, model: []const u8, effort: []const u8) Allocator.Error!Value {
    var settings: ObjectBuilder = .init(a);
    try settings.str("model", model);
    try settings.put("reasoning_effort", if (effort.len > 0) .{ .string = effort } else .null);
    try settings.put("developer_instructions", .null);
    var collaboration: ObjectBuilder = .init(a);
    try collaboration.str("mode", mode);
    try collaboration.put("settings", settings.value());
    return collaboration.value();
}

/// Codex UserInput values for input parts.
pub fn userInput(env: agent.Env, a: Allocator, parts: []const types.Part, diag: ?*Diagnostics) Error!Value {
    var out: std.json.Array = .init(a);
    for (parts) |part| {
        const input: Value = switch (part.type) {
            .text => try agent.json.toValue(a, .{ .type = "text", .text = part.text, .text_elements = .{} }),
            .image => if (part.url.len > 0)
                try agent.json.toValue(a, .{ .type = "image", .url = part.url })
            else if (part.path.len > 0 and part.data.len == 0)
                try agent.json.toValue(a, .{ .type = "localImage", .path = providerkit.absDir(env, a, part.path) })
            else
                try agent.json.toValue(a, .{
                    .type = "image",
                    .url = try std.fmt.allocPrint(a, "data:{s};base64,{s}", .{ providerkit.firstOf(&.{ part.media_type, "image/png" }), part.data }),
                }),
            // UserInput has no file type, and the model never sees a
            // mention item, so the path goes in as text, the form
            // Codex's own file search inserts.
            .file => if (part.path.len == 0)
                return errors.unsupported(diag, name, "file part without path")
            else
                try agent.json.toValue(a, .{ .type = "text", .text = providerkit.absDir(env, a, part.path), .text_elements = .{} }),
            .skill, .mention => try agent.json.toValue(a, .{ .type = @tagName(part.type), .name = part.name, .path = part.path }),
        };
        try out.append(input);
    }
    return .{ .array = out };
}

fn mcpServerConfig(a: Allocator, server: types.McpServer) Allocator.Error!Value {
    var config: ObjectBuilder = .init(a);
    switch (server.resolvedTransport()) {
        .http, .sse => {
            try config.str("url", server.url);
            if (server.headers) |headers| if (headers.map.count() > 0) try config.put("http_headers", try providerkit.stringMapValue(a, headers));
        },
        .stdio => {
            try config.str("command", server.command);
            if (server.args.len > 0) try config.put("args", try agent.json.stringArray(a, server.args));
            if (server.env) |env| if (env.map.count() > 0) try config.put("env", try providerkit.stringMapValue(a, env));
            try config.strIf("cwd", server.cwd);
        },
    }
    if (server.enabled) |enabled| try config.put("enabled", .{ .bool = enabled });
    if (server.tool_timeout_ms > 0) try config.put("tool_timeout_sec", .{ .float = @as(f64, @floatFromInt(server.tool_timeout_ms)) / 1000 });
    if (server.startup_timeout_ms > 0) try config.put("startup_timeout_sec", .{ .float = @as(f64, @floatFromInt(server.startup_timeout_ms)) / 1000 });
    if (server.enabled_tools.len > 0) try config.put("enabled_tools", try agent.json.stringArray(a, server.enabled_tools));
    if (server.approved_tools.len > 0) {
        var tools: ObjectBuilder = .init(a);
        for (server.approved_tools) |tool| try tools.put(tool, try agent.json.toValue(a, .{ .approval_mode = "approve" }));
        try config.put("tools", tools.value());
    }
    return config.value();
}

/// `servers` with `status` added, or replacing the entry of the same name.
fn withServerStatus(a: Allocator, servers: []const types.McpServerStatus, status: types.McpServerStatus) Allocator.Error![]const types.McpServerStatus {
    for (servers, 0..) |server, i| if (eql(server.name, status.name)) {
        const list = try a.dupe(types.McpServerStatus, servers);
        list[i] = status;
        return list;
    };
    var list: std.ArrayList(types.McpServerStatus) = .empty;
    try list.appendSlice(a, servers);
    try list.append(a, status);
    return list.items;
}

// ---------------------------------------------------------------- errors

/// Returns `err` with the details `info`, read from another
/// `Diagnostics`, recorded in `diag`.
pub fn forward(diag: ?*Diagnostics, info: types.ErrorInfo, err: Error) Error {
    if (diag) |target| target.record(info);
    return err;
}

/// The app-server's error message, or "" for other errors, so matching
/// never sees text the library wrapped around it.
pub fn protocolMessage(info: types.ErrorInfo) []const u8 {
    return if (info.kind == .protocol) info.message else "";
}

/// An app-server "invalid request" error with exactly this message.
pub fn isInvalidRequest(info: types.ErrorInfo, message: []const u8) bool {
    return info.kind == .protocol and info.rpc_code == -32600 and eql(info.message, message);
}

/// An answer for a thread that has no stored rollout.
pub fn isNoRollout(info: types.ErrorInfo) bool {
    return std.mem.indexOf(u8, protocolMessage(info), "no rollout found") != null;
}

/// A resume refused because the thread is archived.
fn isArchived(info: types.ErrorInfo) bool {
    const message = protocolMessage(info);
    return decode.containsIgnoreCase(message, "is archived") or decode.containsIgnoreCase(message, "codex unarchive");
}

/// A steer the app-server definitely refused, so the input was not
/// delivered. Other errors, such as timeouts, are ambiguous.
fn isSteerRejected(info: types.ErrorInfo) bool {
    if (info.kind != .protocol) return false;
    if (info.rpc_code == -32601) return true;
    if (info.rpc_code != -32600) return false;
    if (eql(info.code, "activeTurnNotSteerable")) return true;
    return eql(info.message, "active turn uses a different output schema") or expectedTurnMismatch(info.message);
}

/// "expected active turn id `X` but found `Y`".
fn expectedTurnMismatch(message: []const u8) bool {
    const prefix = "expected active turn id `";
    if (!std.mem.startsWith(u8, message, prefix) or message.len < 2 or message[message.len - 1] != '`') return false;
    const rest = message[prefix.len .. message.len - 1];
    const middle = std.mem.indexOf(u8, rest, "` but found `") orelse return false;
    const first = rest[0..middle];
    const second = rest[middle + "` but found `".len ..];
    return first.len > 0 and second.len > 0 and std.mem.indexOfScalar(u8, first, '`') == null and std.mem.indexOfScalar(u8, second, '`') == null;
}

test "permission settings and steer rejection" {
    try std.testing.expectEqualStrings("never", permissionSettings(types.permission_mode.bypass).approval);
    try std.testing.expectEqualStrings("read-only", permissionSettings(types.permission_mode.plan).sandbox);
    try std.testing.expectEqualStrings("untrusted", permissionSettings("untrusted").approval);
    try std.testing.expect(expectedTurnMismatch("expected active turn id `a` but found `b`"));
    try std.testing.expect(!expectedTurnMismatch("expected active turn id `a` but found ``"));
}

test "user input and MCP server config wire shape" {
    var threaded: std.Io.Threaded = .init(std.testing.allocator, .{});
    defer threaded.deinit();
    const env: agent.Env = .{ .io = threaded.io(), .gpa = std.testing.allocator, .environ = .empty };
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const input = try userInput(env, a, &.{
        .{ .type = .text, .text = "hi" },
        .{ .type = .image, .url = "https://x/y.png" },
        .{ .type = .image, .path = "/tmp/y.png" },
        .{ .type = .image, .data = "QQ==" },
        .{ .type = .file, .path = "/tmp/f.txt" },
        .{ .type = .skill, .name = "s", .path = "/p" },
        .{ .type = .mention, .name = "m", .path = "/q" },
    }, null);
    try std.testing.expectEqualStrings(
        \\[{"type":"text","text":"hi","text_elements":[]},{"type":"image","url":"https://x/y.png"},{"type":"localImage","path":"/tmp/y.png"},{"type":"image","url":"data:image/png;base64,QQ=="},{"type":"text","text":"/tmp/f.txt","text_elements":[]},{"type":"skill","name":"s","path":"/p"},{"type":"mention","name":"m","path":"/q"}]
    , try agent.json.encodeValue(a, input));
    try std.testing.expectError(error.Unsupported, userInput(env, a, &.{.{ .type = .file }}, null));
    const config = try mcpServerConfig(a, .{ .command = "run", .approved_tools = &.{ "read", "write" } });
    try std.testing.expectEqualStrings(
        \\{"command":"run","tools":{"read":{"approval_mode":"approve"},"write":{"approval_mode":"approve"}}}
    , try agent.json.encodeValue(a, config));
}
