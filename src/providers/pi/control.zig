//! Session commands beyond input: state and info, model and effort,
//! permission modes, rewind and forks, session switches, compaction,
//! built-in commands, bash, and native passthrough.
const std = @import("std");
const Allocator = std.mem.Allocator;
const agent = @import("agent");
const types = agent.types;
const errors = agent.errors;
const kit = agent.providerkit;
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const Session = agent.Session;
const Id = agent.Id;
const Error = errors.Error;
const Diagnostics = errors.Diagnostics;
const eql = kit.eql;
const log = std.log.scoped(.agent);
const decode = @import("decode.zig");
const history = @import("history.zig");
const pi_session = @import("session.zig");
const PiSession = pi_session.PiSession;

const name = @import("root.zig").name;

// ---------------------------------------------------------------- state

pub fn getState(pi: *PiSession, a: Allocator, timeout_ms: i64, diag: ?*Diagnostics) Error!JsonView {
    const state = try pi.commandTimeout(a, "get_state", .{}, timeout_ms, diag);
    if (state.object() == null) return errors.fail(diag, .protocol, "pi: get_state returned no state", .{});
    return state;
}

/// Takes Pi's state into the session info.
pub fn applyState(pi: *PiSession, a: Allocator, state: JsonView, announce: bool) Allocator.Error!void {
    pi.rt().setSessionId(state.str("sessionId"));
    const model = state.get("model");
    pi.lock();
    if (model.object() != null) {
        pi.model_provider = .of(model.str("provider"));
        pi.window = model.int("contextWindow");
        pi.model_images = if (model.get("input").v != null and model.get("input").v.? == .array) kit.contains(try model.get("input").strings(a), "image") else null;
    }
    const mode = pi.mode;
    pi.unlock();
    try pi.rt().updateInfo(StateUpdate{ .pi = pi, .state = state, .mode = mode }, announce, StateUpdate.apply);
}

/// Pi's state and the permission mode, to take into the session info.
const StateUpdate = struct {
    pi: *PiSession,
    state: JsonView,
    mode: []const u8,

    fn apply(update: StateUpdate, info: *types.SessionInfo, a: Allocator) Allocator.Error!void {
        const state = update.state;
        info.id = state.str("sessionId");
        info.provider = name;
        // Set at startup; refreshSwitched follows session switches.
        if (info.cwd.len == 0) info.cwd = kit.absDir(update.pi.env(), a, update.pi.cfg.cwd);
        // Without a mode Pi runs every tool, as bypass does.
        info.permission_mode = kit.firstOf(&.{ update.mode, types.permission_mode.bypass });
        info.title = state.str("sessionName");
        info.effort = decode.effortOf(state.str("thinkingLevel"));
        const model = state.get("model");
        if (model.object() != null) info.model = try std.fmt.allocPrint(a, "{s}/{s}", .{ model.str("provider"), model.str("id") });
        info.raw = try state.raw(a);
        var tools: std.ArrayList([]const u8) = .empty;
        try tools.appendSlice(a, info.tools);
        for (update.pi.cfg.tools) |tool| if (!kit.contains(tools.items, tool.name)) try tools.append(a, tool.name);
        info.tools = tools.items;
    }
};

/// Sets the info's cwd to the directory a session file's header records,
/// which Pi runs the session in.
pub fn followSessionCwd(pi: *PiSession, a: Allocator, file: []const u8) Allocator.Error!void {
    if (file.len == 0) return;
    const cwd = history.readHeader(pi.env(), a, file).str("cwd");
    if (cwd.len == 0) return;
    try pi.setInfoField(false, .cwd, cwd);
}

/// Reads the session's commands into the info. Commands also report
/// whether the bridge is loaded, which in-place rewinds and gating
/// permission modes need. A failure keeps the previous commands.
pub fn loadCommands(pi: *PiSession, a: Allocator) void {
    var diag: Diagnostics = .{};
    const list = commands(pi, a, &diag) catch |e| {
        log.warn("pi: cannot read the session's commands: {s}", .{Diagnostics.of(&diag, e).message});
        return;
    };
    pi.setInfoField(false, .commands, list) catch log.err("pi: out of memory recording the session's commands", .{});
}

/// Terminal commands Pi runs only in its UI, which send runs through RPC.
const builtin_commands = [_]types.SlashCommand{
    .{ .name = "compact", .description = "Compact the conversation", .argument_hint = "[instructions]", .kind = "command", .scope = "builtin" },
    .{ .name = "autocompact", .description = "Turn automatic compaction on or off", .argument_hint = "[on|off|toggle]", .kind = "command", .scope = "builtin" },
};

/// Extension commands, prompt templates, skills, and the built-in commands
/// send runs through RPC.
fn commands(pi: *PiSession, a: Allocator, diag: ?*Diagnostics) Error![]const types.SlashCommand {
    const data = try pi.command(a, "get_commands", .{}, diag);
    var out: std.ArrayList(types.SlashCommand) = .empty;
    var loaded = false;
    for (data.get("commands").items()) |v| {
        const command = JsonView.of(v);
        if (eql(command.str("name"), pi_session.rewind_command) and eql(command.str("source"), "extension")) {
            loaded = true;
            continue;
        }
        const source = command.get("sourceInfo");
        try out.append(a, .{ .name = command.str("name"), .description = command.str("description"), .kind = command.str("source"), .path = source.str("path"), .scope = decode.commandScope(source.str("scope")) });
    }
    // Pi loads extensions again for every session it switches to.
    pi.bridge_loaded.store(loaded, .release);
    for (builtin_commands) |builtin| {
        for (out.items) |command| {
            if (eql(command.name, builtin.name)) break;
        } else try out.append(a, builtin);
    }
    return out.toOwnedSlice(a);
}

/// Reloads state after commands that change the session.
fn refresh(pi: *PiSession, a: Allocator, diag: ?*Diagnostics) Error!void {
    try applyState(pi, a, try getState(pi, a, 0, diag), true);
}

/// Reloads state after Pi replaced the session. Pi reloads extensions,
/// prompts, and skills for the new session from its own directory, so the
/// working directory and commands are read again.
fn refreshSwitched(pi: *PiSession, a: Allocator, diag: ?*Diagnostics) Error!void {
    const state = try getState(pi, a, 0, diag);
    try applyState(pi, a, state, false);
    try followSessionCwd(pi, a, state.str("sessionFile"));
    loadCommands(pi, a);
    try pi.announceInfo();
    var gate_diag: Diagnostics = .{};
    pi.checkGate(pi.permissionMode(), &gate_diag) catch |e| {
        const message = Diagnostics.of(&gate_diag, e).message;
        pi.rt().emitPayload(.{ .notice = .{ .level = .@"error", .code = "bridge_unloaded", .message = try std.fmt.allocPrint(a, "{s}; send refuses turns until the bridge loads again or the mode gates nothing", .{message}) } }, null);
    };
}

pub fn applyStartupSettings(pi: *PiSession, diag: ?*Diagnostics) Error!void {
    var arena: std.heap.ArenaAllocator = .init(pi.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const session_options = pi.opts;
    if (session_options.steering_mode.len > 0) _ = try pi.command(a, "set_steering_mode", .{ .mode = session_options.steering_mode }, diag);
    if (session_options.follow_up_mode.len > 0) _ = try pi.command(a, "set_follow_up_mode", .{ .mode = session_options.follow_up_mode }, diag);
    if (session_options.auto_compaction) |on| _ = try pi.command(a, "set_auto_compaction", .{ .enabled = on }, diag);
    if (session_options.auto_retry) |on| _ = try pi.command(a, "set_auto_retry", .{ .enabled = on }, diag);
}

/// Switches a resumed session to `Config.model` when Pi lists it as
/// available; otherwise the session keeps its recorded model.
pub fn applyResumeModel(pi: *PiSession, a: Allocator) Allocator.Error!void {
    const want = decode.splitModel(pi.cfg.model);
    const provider = kit.firstOf(&.{ want.provider, pi.opts.provider });
    var diag: Diagnostics = .{};
    const data = pi.command(a, "get_available_models", .{}, &diag) catch |e| {
        log.warn("pi: cannot list models to apply {s}: {s}", .{ pi.cfg.model, Diagnostics.of(&diag, e).message });
        return;
    };
    const found = for (data.get("models").items()) |v| {
        const model = JsonView.of(v);
        if (eql(model.str("id"), want.id) and (provider.len == 0 or eql(model.str("provider"), provider))) break model;
    } else {
        const info = try pi.rt().sessionInfo(a);
        log.warn("pi: requested model {s} is not available; keeping the session's model", .{pi.cfg.model});
        pi.rt().emitPayload(.{ .notice = .{ .level = .warning, .code = "model_unavailable", .message = try std.fmt.allocPrint(a, "model {s} is not available; the session keeps {s}", .{ pi.cfg.model, info.model }) } }, null);
        return;
    };
    const full = try std.fmt.allocPrint(a, "{s}/{s}", .{ found.str("provider"), found.str("id") });
    const info = try pi.rt().sessionInfo(a);
    if (!eql(full, info.model)) setModelOn(pi, a, full, &diag) catch |e| {
        log.warn("pi: cannot apply model {s}: {s}", .{ full, Diagnostics.of(&diag, e).message });
        return;
    };
    if (want.level.len > 0 and pi.cfg.effort.len == 0) setEffortOn(pi, a, decode.effortOf(want.level), &diag) catch |e|
        log.warn("pi: cannot apply thinking level {s}: {s}", .{ want.level, Diagnostics.of(&diag, e).message });
}

/// Switches to the input's model and effort, when set, as the input starts.
pub fn applyInputSettings(pi: *PiSession, in: types.Input, diag: ?*Diagnostics) Error!void {
    if (in.model.len == 0 and in.effort.len == 0) return;
    var arena: std.heap.ArenaAllocator = .init(pi.gpa);
    defer arena.deinit();
    if (in.model.len > 0) try setModelOn(pi, arena.allocator(), in.model, diag);
    if (in.effort.len > 0) try setEffortOn(pi, arena.allocator(), in.effort, diag);
}

// ---------------------------------------------------------------- settings

/// Switches the model: "provider/id" or "provider:id", optionally with a
/// ":thinking" suffix, or an ID of the current provider.
pub fn setModel(session: *Session, model: []const u8, diag: ?*Diagnostics) Error!void {
    const pi = PiSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(pi.gpa);
    defer arena.deinit();
    return setModelOn(pi, arena.allocator(), model, diag);
}

fn setModelOn(pi: *PiSession, a: Allocator, model: []const u8, diag: ?*Diagnostics) Error!void {
    const wanted = decode.splitModel(model);
    var provider = wanted.provider;
    if (provider.len == 0) {
        pi.lock();
        provider = try a.dupe(u8, pi.model_provider.slice());
        pi.unlock();
    }
    const data = try pi.command(a, "set_model", .{ .provider = provider, .modelId = wanted.id }, diag);
    if (wanted.level.len > 0) _ = try pi.command(a, "set_thinking_level", .{ .level = wanted.level }, diag);
    // Without fresh state, the command's answer still names the model.
    if (refresh(pi, a, null)) |_| return else |_| {}
    const new_provider = kit.firstOf(&.{ data.str("provider"), provider });
    const full = try std.fmt.allocPrint(a, "{s}/{s}", .{ new_provider, kit.firstOf(&.{ data.str("id"), wanted.id }) });
    pi.lock();
    pi.model_provider = .of(new_provider);
    if (data.int("contextWindow") > 0) pi.window = data.int("contextWindow");
    pi.model_images = if (data.get("input").v != null) kit.contains(try data.get("input").strings(a), "image") else null;
    pi.unlock();
    try pi.setInfoField(true, .model, full);
}

/// Sets the thinking level; the info reflects the level Pi clamped it to.
pub fn setEffort(session: *Session, effort: types.Effort, diag: ?*Diagnostics) Error!void {
    const pi = PiSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(pi.gpa);
    defer arena.deinit();
    return setEffortOn(pi, arena.allocator(), effort, diag);
}

fn setEffortOn(pi: *PiSession, a: Allocator, requested: []const u8, diag: ?*Diagnostics) Error!void {
    const effort = kit.firstOf(&.{ requested, types.effort.none });
    _ = try pi.command(a, "set_thinking_level", .{ .level = decode.thinkingLevel(effort) }, diag);
    // Without fresh state, the requested level is the best guess.
    if (refresh(pi, a, null)) |_| return else |_| {}
    try pi.setInfoField(true, .effort, effort);
}

/// Changes how the bridge gates tool calls.
pub fn setPermissionMode(session: *Session, mode: types.PermissionMode, diag: ?*Diagnostics) Error!void {
    const pi = PiSession.of(session);
    if (pi.bridgeDisabled()) return errors.unsupported(diag, name, "permission modes without the bridge extension");
    const modes = session.capabilities.permission_modes;
    try kit.checkPermissionMode(name, mode, modes, diag);
    try pi.checkGate(mode, diag);
    // Keep the library's copy of the name: the caller's may not live on.
    const owned: []const u8 = for (modes) |known| {
        if (eql(known, mode)) break known;
    } else "";
    pi.lock();
    pi.mode = owned;
    pi.unlock();
    try pi.setInfoField(true, .permission_mode, kit.firstOf(&.{ owned, types.permission_mode.bypass }));
}

/// Sets the session display name.
pub fn setTitle(session: *Session, title: []const u8, diag: ?*Diagnostics) Error!void {
    const pi = PiSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(pi.gpa);
    defer arena.deinit();
    _ = try pi.command(arena.allocator(), "set_session_name", .{ .name = title }, diag);
    try pi.setInfoField(true, .title, title);
}

// ---------------------------------------------------------------- rewind and fork

/// The session entry of turn `id`'s prompt. An ID that is not a turn of
/// this session is taken as a Pi entry ID, such as a user message item's.
fn userEntry(pi: *PiSession, a: Allocator, id: []const u8, diag: ?*Diagnostics) Error![]const u8 {
    if (std.mem.startsWith(u8, id, "v1-"))
        // read_history numbers the entries of version 1 files, which Pi
        // gives new IDs when it opens them.
        return errors.fail(diag, .invalid_params, "pi: {s} is from a version 1 session file; read the history again after resuming the session", .{id});
    var stamp: ?i64 = null;
    {
        pi.lock();
        defer pi.unlock();
        for (pi.turn_entries.items) |turn_entry| if (turn_entry.turn.eql(id)) {
            if (!turn_entry.entry.isEmpty()) return a.dupe(u8, turn_entry.entry.slice());
            stamp = turn_entry.stamp;
        };
    }
    const want = stamp orelse return id;
    const data = try pi.command(a, "get_entries", .{}, diag);
    const entries = data.get("entries").items();
    var i = entries.len;
    while (i > 0) {
        i -= 1;
        const entry = JsonView.of(entries[i]);
        const message = entry.get("message");
        if (eql(entry.str("type"), "message") and eql(message.str("role"), "user") and message.int("timestamp") == want) return entry.str("id");
    }
    return errors.fail(diag, .not_found, "pi: no user message for turn {s}", .{id});
}

/// Drops `turn_id` and every later turn. With the bridge loaded, the
/// session moves back in place and keeps its ID; without it, the session
/// forks at the message and its ID changes.
pub fn rewind(session: *Session, turn_id: []const u8, diag: ?*Diagnostics) Error!void {
    const pi = PiSession.of(session);
    if (!session.rt.idle()) return errors.fail(diag, .invalid_params, "pi: cannot rewind while a turn runs or input is queued", .{});
    var arena: std.heap.ArenaAllocator = .init(pi.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    return rewindTo(pi, a, try userEntry(pi, a, turn_id, diag), "rewind", diag);
}

/// Rewinds to `entry` in place through the bridge, else by forking there.
fn rewindTo(pi: *PiSession, a: Allocator, entry: []const u8, verb: []const u8, diag: ?*Diagnostics) Error!void {
    if (!pi.bridgeDisabled() and pi.bridge_loaded.load(.acquire)) return rewindInPlace(pi, a, entry, diag);
    const data = try forkAt(pi, a, entry, diag);
    if (data.flag("cancelled")) return errors.fail(diag, .cancelled, "pi: an extension cancelled the {s}", .{verb});
}

/// Runs the bridge's rewind command. Pi runs extension commands at once
/// and answers "handled" after the command finished, and the bridge's
/// answer arrives before that response.
fn rewindInPlace(pi: *PiSession, a: Allocator, entry: []const u8, diag: ?*Diagnostics) Error!void {
    const nonce = pi.env().uuid();
    const payload = try RawJson.encode(a, .{ .token = pi.token, .nonce = nonce.slice(), .targetId = entry });
    const encoder = std.base64.url_safe_no_pad.Encoder;
    const encoded = try a.alloc(u8, encoder.calcSize(payload.bytes.len));
    _ = encoder.encode(encoded, payload.bytes);
    var slot: pi_session.RewindSlot = .{ .nonce = nonce };
    {
        pi.lock();
        defer pi.unlock();
        try pi.rewinds.append(pi.gpa, &slot);
    }
    defer {
        pi.lock();
        for (pi.rewinds.items, 0..) |x, i| if (x == &slot) {
            _ = pi.rewinds.swapRemove(i);
            break;
        };
        pi.unlock();
        pi.gpa.free(slot.err);
    }
    const data = try pi.command(a, "prompt", .{ .message = try std.fmt.allocPrint(a, "/{s} {s}", .{ pi_session.rewind_command, encoded }) }, diag);
    pi.lock();
    const answer = slot;
    pi.unlock();
    if (!answer.done) return errors.fail(diag, .protocol, "pi: the bridge did not answer the rewind (disposition \"{s}\")", .{data.str("disposition")});
    if (answer.cancelled) return errors.fail(diag, .cancelled, "pi: an extension cancelled the rewind", .{});
    if (!answer.ok) return errors.fail(diag, .protocol, "pi: rewind: {s}", .{answer.err});
    return refresh(pi, a, diag);
}

/// Keeps the conversation up to and including the turn whose prompt is
/// `entry`, by rewinding to the next prompt on the active branch.
pub fn truncateAfter(pi: *PiSession, a: Allocator, entry: []const u8, diag: ?*Diagnostics) Error!void {
    // Pi lists the prompts of every branch; after a rewind the abandoned
    // ones are still there. Keep the active branch's, in order.
    const data = try pi.command(a, "get_fork_messages", .{}, diag);
    const all = data.get("messages").items();
    const branch = try activeBranchIds(pi, a, diag);
    var points: std.ArrayList([]const u8) = .empty;
    for (all) |message| {
        const id = JsonView.of(message).str("entryId");
        if (branch == null or branch.?.contains(id)) try points.append(a, id);
    }
    const i = for (points.items, 0..) |id, i| {
        if (eql(id, entry)) break i;
    } else return errors.fail(diag, .invalid_params, "pi: fork_at_turn {s} is not a prompt on the active branch", .{entry});
    if (i == points.items.len - 1) return;
    // In place when it can, so the fork keeps its ID and no second copy is
    // written.
    return rewindTo(pi, a, points.items[i + 1], "fork", diag);
}

/// The entry IDs on the session file's active branch, or null when the
/// session has no file or it cannot be read (all prompts are then taken as
/// active).
fn activeBranchIds(pi: *PiSession, a: Allocator, diag: ?*Diagnostics) Error!?std.StringHashMapUnmanaged(void) {
    const path = (try getState(pi, a, 0, diag)).str("sessionFile");
    if (path.len == 0) return null;
    const chain = history.readBranch(pi.rt().env, a, path, null) catch return null;
    var ids: std.StringHashMapUnmanaged(void) = .empty;
    for (chain) |branch_entry| try ids.put(a, branch_entry.id, {});
    return ids;
}

/// Runs the fork command; callers hold the slot or own the session.
fn forkAt(pi: *PiSession, a: Allocator, entry: []const u8, diag: ?*Diagnostics) Error!JsonView {
    const data = try pi.command(a, "fork", .{ .entryId = entry }, diag);
    if (!data.flag("cancelled")) try refreshSwitched(pi, a, diag);
    return data;
}

/// Starts a new Pi process on a copy of this conversation and leaves this
/// session as it is. Unset fields of the config are taken from this
/// session.
pub fn fork(session: *Session, opts: agent.StartOptions, diag: ?*Diagnostics) Error!*Session {
    const pi = PiSession.of(session);
    if (pi.cfg.ephemeral) return errors.unsupported(diag, name, "forking an ephemeral session");
    var arena: std.heap.ArenaAllocator = .init(pi.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    var cfg = kit.inheritConfig(opts.config, pi.cfg, try session.info(a));
    if (cfg.fork_at_turn.len > 0) cfg.fork_at_turn = try userEntry(pi, a, cfg.fork_at_turn, diag);
    cfg.resume_id = try session.id(a);
    cfg.fork = true;
    return pi_session.start(pi.provider, .{ .config = cfg, .tool_handler = opts.tool_handler orelse session.tool_handler, .sink = opts.sink }, diag);
}

// ---------------------------------------------------------------- exclusive operations

const Outcome = struct { text: []const u8 = "", usage: types.Usage = .{}, data: JsonView = .empty };

/// Takes the running slot as a local turn of its own, so input sent
/// meanwhile is queued and waits for it. Fails with `busy` while a turn
/// runs or input is queued. End the turn with `endExclusive`.
fn beginExclusive(pi: *PiSession, busy: []const u8, diag: ?*Diagnostics) Error!Id {
    const id = pi.env().uuid();
    const Nothing = struct {
        fn start(_: void, _: ?*Diagnostics) Error!void {}
    };
    if (!try pi.rt().startIdle(id.slice(), .{ .local = true }, {}, diag, Nothing.start))
        return errors.fail(diag, .invalid_params, "{s}", .{busy});
    return id;
}

/// Ends exclusive turn `id` with `result`, whose failure details are in
/// `result_diag`, and returns the result.
fn endExclusive(pi: *PiSession, id: Id, result: Error!Outcome, result_diag: *const Diagnostics, diag: ?*Diagnostics) Error!Outcome {
    const outcome = result catch |e| {
        const info = Diagnostics.of(result_diag, e);
        pi.rt().finishTurn(id.slice(), builtinFailure(pi, id.slice(), info));
        return errors.failWith(diag, info);
    };
    pi.rt().finishTurn(id.slice(), .{ .stop_reason = "handled", .text = outcome.text, .usage = outcome.usage });
    return outcome;
}

/// The result of built-in command turn `id` that failed: interrupted when
/// an interrupt stopped it, else failed.
fn builtinFailure(pi: *PiSession, id: []const u8, info: types.ErrorInfo) types.TurnResult {
    if (pi.interrupting()[0].eql(id)) return .{ .status = .interrupted, .stop_reason = "aborted", .@"error" = .{ .kind = .interrupted, .code = "aborted", .message = "turn interrupted" } };
    return .{ .status = .failed, .stop_reason = "error", .@"error" = .{ .kind = .turn_failed, .code = "error", .message = info.message } };
}

/// Runs Pi's compact command for built-in turn `id`.
fn compactCommand(pi: *PiSession, a: Allocator, id: []const u8, instructions: []const u8, diag: ?*Diagnostics) Error!Outcome {
    // Interrupted before the compaction began; Pi ignored the abort.
    if (pi.interrupting()[0].eql(id)) return errors.fail(diag, .interrupted, "pi: compaction interrupted", .{});
    const data = try pi.command(a, "compact", .{ .customInstructions = if (instructions.len > 0) instructions else null }, diag);
    return .{ .text = data.str("summary"), .usage = decode.usage(data.get("usage")), .data = data };
}

/// Compacts the conversation. The compaction holds the running slot as a
/// turn of its own, as "/compact" sent as input does.
pub fn compact(session: *Session, instructions: []const u8, diag: ?*Diagnostics) Error!void {
    const pi = PiSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(pi.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const id = try beginExclusive(pi, "pi: cannot compact while a turn runs or input is queued; send /compact to queue it", diag);
    var compact_diag: Diagnostics = .{};
    _ = try endExclusive(pi, id, compactCommand(pi, a, id.slice(), instructions, &compact_diag), &compact_diag, diag);
}

/// Runs a built-in command for turn `id` (a worker).
pub fn runBuiltin(pi: *PiSession, id: Id, kind: PiSession.BuiltinKind, args: []u8) void {
    defer pi.gpa.free(args);
    var arena: std.heap.ArenaAllocator = .init(pi.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const turn = id.slice();
    var diag: Diagnostics = .{};
    const result: Error!Outcome = switch (kind) {
        .compact => compactCommand(pi, a, turn, args, &diag),
        .autocompact => autocompact(pi, a, args, &diag),
    };
    const turn_result: types.TurnResult = if (result) |outcome|
        .{ .stop_reason = "handled", .text = outcome.text, .usage = outcome.usage }
    else |e|
        builtinFailure(pi, turn, Diagnostics.of(&diag, e));
    pi.rt().finishTurn(turn, turn_result);
}

fn autocompact(pi: *PiSession, a: Allocator, args: []const u8, diag: ?*Diagnostics) Error!Outcome {
    var enabled: bool = undefined;
    if (std.ascii.eqlIgnoreCase(args, "on")) {
        enabled = true;
    } else if (std.ascii.eqlIgnoreCase(args, "off")) {
        enabled = false;
    } else if (args.len == 0 or std.ascii.eqlIgnoreCase(args, "toggle")) {
        enabled = !(try getState(pi, a, 0, diag)).flag("autoCompactionEnabled");
    } else return errors.fail(diag, .invalid_params, "usage: /autocompact [on|off|toggle]", .{});
    _ = try pi.command(a, "set_auto_compaction", .{ .enabled = enabled }, diag);
    return .{ .text = if (enabled) "Auto-compaction enabled" else "Auto-compaction disabled" };
}

/// The commands that replace the session.
const SwitchCommand = enum { new_session, switch_session, clone, fork };

/// Replaces the session with `command`. Pi aborts the running run and
/// rebinds its event stream to the new session, so the switch holds the
/// slot and fails while a turn runs or input is queued.
fn switchCommand(pi: *PiSession, a: Allocator, command: SwitchCommand, params: ?RawJson, diag: ?*Diagnostics) Error!JsonView {
    const id = try beginExclusive(pi, "pi: cannot switch sessions while a turn runs or input is queued", diag);
    var switch_diag: Diagnostics = .{};
    const outcome = try endExclusive(pi, id, runSwitch(pi, a, command, params, &switch_diag), &switch_diag, diag);
    return outcome.data;
}

fn runSwitch(pi: *PiSession, a: Allocator, command: SwitchCommand, params: ?RawJson, diag: ?*Diagnostics) Error!Outcome {
    const data = try pi.command(a, @tagName(command), params, diag);
    if (data.flag("cancelled")) return .{ .data = data };
    switch (command) {
        .new_session, .switch_session => {
            // The turns of the previous session no longer resolve, and its
            // grants do not carry over to another conversation.
            pi.forgetTurns();
            pi.lock();
            pi.allowed.clear(pi.gpa);
            pi.unlock();
        },
        .clone, .fork => {},
    }
    try refreshSwitched(pi, a, diag);
    return .{ .data = data };
}

// ---------------------------------------------------------------- bash

/// Completes the tool call item of a bash command when Pi answers. Runs on
/// the reader task, after the output deltas.
const BashReply = struct {
    pi: *PiSession,
    item: types.Item,
    completed: bool = false,

    fn onReply(context: *anyopaque, success: bool, response: JsonView) void {
        const reply: *BashReply = @ptrCast(@alignCast(context));
        reply.completed = true;
        var done = reply.item;
        const data = response.get("data");
        if (!success or data.object() == null) {
            failItem(&done, response.str("error"));
        } else {
            const exit: ?i32 = if (data.has("exitCode")) @truncate(data.int("exitCode")) else null;
            decode.bashOutcome(&done, data.str("output"), exit, data.flag("cancelled"));
        }
        reply.pi.rt().emit(.{ .payload = .{ .item_completed = .{ .item = done } } });
    }
};

fn failItem(item: *types.Item, message: []const u8) void {
    var tool_call = item.tool.?;
    tool_call.is_error = true;
    tool_call.output = message;
    item.tool = tool_call;
    item.status = .failed;
}

/// Runs a shell command whose output joins the conversation. It is a tool
/// call item "bash-<request id>" whose output streams as tool_output_delta
/// events and which completes when Pi answers. The answer carries the
/// item's ID as "itemId".
fn bash(pi: *PiSession, a: Allocator, params: ?RawJson, diag: ?*Diagnostics) Error!JsonView {
    const fields = if (params) |raw| JsonView.parseBytes(a, raw.bytes) else JsonView.empty;
    const command_text = fields.str("command");
    const id = pi.newRequestId();
    const item_id = try std.fmt.allocPrint(a, "bash-{s}", .{id.slice()});
    const item: types.Item = .{ .id = item_id, .kind = .tool_call, .status = .in_progress, .tool = .{ .name = "bash", .kind = .shell, .command = command_text, .input = try decode.commandInput(a, command_text) } };
    pi.rt().emit(.{ .payload = .{ .item_started = .{ .item = item } } });
    var reply: BashReply = .{ .pi = pi, .item = item };
    var bash_diag: Diagnostics = .{};
    const data = pi.callOrdered(a, id, "bash", params, .{ .context = &reply, .function = BashReply.onReply }, 0, &bash_diag) catch |e| {
        const info = Diagnostics.of(&bash_diag, e);
        if (!reply.completed) {
            var done = item;
            failItem(&done, info.message);
            pi.rt().emit(.{ .payload = .{ .item_completed = .{ .item = done } } });
        }
        return errors.failWith(diag, info);
    };
    var value = data.v orelse return data;
    if (value == .object) try value.object.put(a, "itemId", .{ .string = item_id });
    return JsonView.of(value);
}

// ---------------------------------------------------------------- passthrough

/// Native commands with more logic than passing them through.
const NativeCommand = enum {
    new_session,
    switch_session,
    clone,
    fork,
    bash,
    clear_queue,
    set_model,
    set_thinking_level,
    cycle_model,
    cycle_thinking_level,
    set_session_name,
    other,
};

/// Sends Pi RPC command `method` with the fields of `params` and returns
/// its data. Commands with more logic than passing them through keep it:
/// new_session, switch_session, clone, and fork hold the running slot and
/// reload state; clear_queue also drops the library's queued input; bash
/// reports a tool call item; model, thinking level, and name changes
/// reload the session info.
pub fn call(session: *Session, method: []const u8, params: ?RawJson, a: Allocator, diag: ?*Diagnostics) Error!?RawJson {
    const pi = PiSession.of(session);
    const command = std.meta.stringToEnum(NativeCommand, method) orelse .other;
    const data: JsonView = switch (command) {
        .new_session => try switchCommand(pi, a, .new_session, params, diag),
        .switch_session => try switchCommand(pi, a, .switch_session, params, diag),
        .clone => try switchCommand(pi, a, .clone, params, diag),
        .fork => try switchCommand(pi, a, .fork, params, diag),
        .bash => try bash(pi, a, params, diag),
        .clear_queue => blk: {
            // Drops the library's queued input, as clear_queue drops Pi's.
            pi.rt().abandonQueue(.{ .kind = .cancelled, .message = "input cancelled before it ran" });
            break :blk try pi.command(a, method, params, diag);
        },
        .set_model, .set_thinking_level, .cycle_model, .cycle_thinking_level, .set_session_name => blk: {
            const answer = try pi.command(a, method, params, diag);
            var refresh_diag: Diagnostics = .{};
            refresh(pi, a, &refresh_diag) catch |e|
                log.warn("pi: cannot reload the session info after {s}: {s}", .{ method, Diagnostics.of(&refresh_diag, e).message });
            break :blk answer;
        },
        .other => try pi.command(a, method, params, diag),
    };
    return data.raw(a);
}
