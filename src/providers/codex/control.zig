//! Session operations beyond sending input: settings, title, compaction,
//! rewind, fork, subagent stops, rate limits, and native calls.
const std = @import("std");
const agent = @import("agent");
const decode = @import("decode.zig");
const session_module = @import("session.zig");
const translate = @import("translate.zig");
const CodexSession = session_module.CodexSession;
const Allocator = std.mem.Allocator;
const types = agent.types;
const errors = agent.errors;
const providerkit = agent.providerkit;
const Session = agent.Session;
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const ObjectBuilder = agent.json.ObjectBuilder;
const Id = agent.Id;
const Diagnostics = agent.Diagnostics;
const Error = errors.Error;
const log = std.log.scoped(.agent);

/// Changes thread settings with thread/settings/update.
fn updateSettings(codex: *CodexSession, a: Allocator, settings: ObjectBuilder, diag: ?*Diagnostics) Error!void {
    var params = try codex.threadIdParams(a);
    try params.putAll(settings.map);
    _ = try codex.threadCall(a, "thread/settings/update", params.value(), 0, diag);
}

pub fn setModel(session: *Session, model: []const u8, diag: ?*Diagnostics) Error!void {
    const codex = CodexSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    var settings: ObjectBuilder = .init(a);
    try settings.str("model", model);
    try updateSettings(codex, a, settings, diag);
    codex.mutex.lockUncancelable(codex.env.io);
    defer codex.mutex.unlock(codex.env.io);
    codex.model = .of(model);
    codex.updateInfo(.{ .model = model });
}

pub fn setEffort(session: *Session, effort: types.Effort, diag: ?*Diagnostics) Error!void {
    const codex = CodexSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    var settings: ObjectBuilder = .init(a);
    try settings.put("effort", if (effort.len > 0) .{ .string = effort } else .null);
    try updateSettings(codex, a, settings, diag);
    codex.mutex.lockUncancelable(codex.env.io);
    defer codex.mutex.unlock(codex.env.io);
    codex.effort = .of(effort);
    codex.updateInfo(.{ .effort = effort });
}

/// Changes the approval policy and sandbox for later turns. Plan switches
/// the plan collaboration mode and the read-only sandbox on; any other mode
/// switches plan mode off and applies its own sandbox.
pub fn setPermissionMode(session: *Session, mode: types.PermissionMode, diag: ?*Diagnostics) Error!void {
    const codex = CodexSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const permission = session_module.permissionSettings(mode);
    var settings: ObjectBuilder = .init(a);
    try settings.strIf("approvalPolicy", permission.approval);
    const policy = codex.resolveSandbox(a, permission.sandbox);
    if (policy) |value| try settings.put("sandboxPolicy", value);
    if (permission.reviewer.len > 0) try settings.str("approvalsReviewer", permission.reviewer) else if (mode.len > 0) try settings.str("approvalsReviewer", "user");
    const plan = std.mem.eql(u8, mode, types.permission_mode.plan);
    codex.mutex.lockUncancelable(codex.env.io);
    const model = try a.dupe(u8, codex.model.slice());
    const effort = try a.dupe(u8, codex.effort.slice());
    const was_plan = codex.plan;
    codex.mutex.unlock(codex.env.io);
    // The collaboration mode is sent only when plan mode toggles. Its null
    // developer_instructions selects the mode's built-in instructions and
    // keeps the thread's developerInstructions (append_system_prompt);
    // setting it would replace Codex's plan-mode instructions (verified
    // against codex-cli 0.163.0).
    if (plan != was_plan) try settings.put("collaborationMode", try session_module.collaborationMode(a, if (plan) "plan" else "default", model, effort));
    try updateSettings(codex, a, settings, diag);
    const sandbox = if (policy) |value| try agent.json.encodeValue(a, value) else null;
    codex.mutex.lockUncancelable(codex.env.io);
    defer codex.mutex.unlock(codex.env.io);
    if (sandbox) |bytes| codex.setSandboxLocked(bytes);
    codex.plan = plan;
    codex.updateInfo(.{ .permission_mode = mode });
}

pub fn setTitle(session: *Session, title: []const u8, diag: ?*Diagnostics) Error!void {
    const codex = CodexSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    var params = try codex.threadIdParams(a);
    try params.str("name", title);
    _ = try codex.threadCall(a, "thread/name/set", params.value(), 0, diag);
    codex.updateInfo(.{ .title = title });
}

/// Compacts the thread's context and waits until the compaction ends.
/// Codex takes no summary instructions, so `instructions` is ignored.
pub fn compact(session: *Session, instructions: []const u8, diag: ?*Diagnostics) Error!void {
    _ = instructions;
    const codex = CodexSession.of(session);
    const io = codex.env.io;
    var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    codex.mutex.lockUncancelable(io);
    translate.endCompactionLocked(codex);
    codex.compacting = true;
    codex.compact_generation += 1;
    const generation = codex.compact_generation;
    codex.mutex.unlock(io);
    const params = try codex.threadIdParams(a);
    _ = codex.threadCall(a, "thread/compact/start", params.value(), 0, diag) catch |e| {
        codex.mutex.lockUncancelable(io);
        if (codex.compact_generation == generation) translate.endCompactionLocked(codex);
        codex.mutex.unlock(io);
        return e;
    };
    while (true) {
        codex.mutex.lockUncancelable(io);
        // A timeout only checks the session again.
        if (codex.compacting and codex.compact_generation == generation)
            codex.compact_condition.waitTimeout(io, &codex.mutex, providerkit.timeoutMs(200)) catch {};
        const done = !(codex.compacting and codex.compact_generation == generation);
        codex.mutex.unlock(io);
        if (done) return;
        if (codex.closed.load(.acquire) or codex.rt().isFinished()) return codex.rt().closedError(diag);
    }
}

/// Interrupts a subagent's running turn. `task_id` is the subagent's
/// thread ID. A subagent that is not running is not an error; an unknown
/// one is.
pub fn stopTask(session: *Session, task_id: []const u8, diag: ?*Diagnostics) Error!void {
    const codex = CodexSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    codex.mutex.lockUncancelable(codex.env.io);
    const known = codex.translator.children.contains(.of(task_id));
    const turn = codex.translator.child_turns.get(.of(task_id)) orelse Id{};
    codex.mutex.unlock(codex.env.io);
    if (!known and turn.isEmpty()) return errors.fail(diag, .not_found, "codex: unknown task {s}", .{task_id});
    if (turn.isEmpty()) return; // not running
    const params = try agent.json.toValue(a, .{ .threadId = task_id, .turnId = turn.slice() });
    _ = try codex.threadCall(a, "turn/interrupt", params, 0, diag);
}

/// Removes `turn_id` and every later turn from the thread history. A
/// paginated thread is reverted in place with thread/revert. A legacy
/// thread, or one whose revert fails, is forked before `turn_id` and the
/// session continues on the fork: its ID changes, which `session_updated`
/// reports, and the original thread stays unchanged.
pub fn rewind(session: *Session, turn_id: []const u8, diag: ?*Diagnostics) Error!void {
    const codex = CodexSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    if (!codex.rt().idle()) return errors.fail(diag, .invalid_params, "codex: cannot rewind while a turn runs", .{});
    var read = try codex.threadIdParams(a);
    try read.put("includeTurns", .{ .bool = false });
    const mode: ?[]const u8 = if (codex.threadCall(a, "thread/read", read.value(), 0, null)) |response|
        providerkit.firstOf(&.{ response.get("thread").str("historyMode"), "legacy" })
    else |_|
        null;
    var revert_diag: Diagnostics = .{};
    var revert_error: ?types.ErrorInfo = null;
    if (mode == null or std.mem.eql(u8, mode.?, "paginated")) {
        var params = try codex.threadIdParams(a);
        try params.str("beforeTurnId", turn_id);
        if (codex.threadCall(a, "thread/revert", params.value(), 0, &revert_diag)) |_| {
            translate.rewound(codex, a, turn_id);
            return;
        } else |e| revert_error = Diagnostics.of(&revert_diag, e);
    }
    var fork_diag: Diagnostics = .{};
    forkBefore(codex, a, turn_id, &fork_diag) catch |e| {
        var info = Diagnostics.of(&fork_diag, e);
        info.message = if (revert_error) |revert|
            try std.fmt.allocPrint(a, "codex: rewind: thread/revert: {s}; thread/fork: {s}", .{ revert.message, info.message })
        else
            try std.fmt.allocPrint(a, "codex: rewind: {s}", .{info.message});
        return errors.failWith(diag, info);
    };
    translate.rewound(codex, a, turn_id);
}

/// Forks the thread before `turn_id` and moves the session to the fork.
fn forkBefore(codex: *CodexSession, a: Allocator, turn_id: []const u8, diag: ?*Diagnostics) Error!void {
    const old = try a.dupe(u8, codex.threadId().slice());
    var cfg = providerkit.inheritConfig(.{}, codex.cfg, try codex.rt().sessionInfo(a));
    cfg.resume_id = old;
    cfg.fork = true;
    var params = try codex.threadParams(a, cfg, codex.opts);
    try params.str("threadId", old);
    try params.str("beforeTurnId", turn_id);
    try params.put("excludeTurns", .{ .bool = true });
    const raw = try codex.client.call(a, "thread/fork", params.value(), .{ .owner = codex }, diag);
    const response = JsonView.parseBytes(a, raw.bytes);
    const id = response.get("thread").str("id");
    if (id.len == 0) return errors.fail(diag, .protocol, "codex: decode thread/fork: no thread ID", .{});
    codex.mutex.lockUncancelable(codex.env.io);
    codex.thread = .of(id);
    codex.mutex.unlock(codex.env.io);
    codex.rememberSandbox(a, response.get("sandbox"));
    codex.client.rekey(old, id, codex);
    codex.rt().setSessionId(id);
    {
        codex.mutex.lockUncancelable(codex.env.io);
        defer codex.mutex.unlock(codex.env.io);
        // The fork's token totals start from its own history.
        codex.translator.have_total = false;
        codex.updateInfo(.{ .id = id });
    }
    _ = codex.threadCall(a, "thread/unsubscribe", try agent.json.toValue(a, .{ .threadId = old }), 0, null) catch |e|
        log.debug("codex: unsubscribing the rewound thread {s} failed: {t}", .{ old, e });
    codex.client.forget(old);
}

/// Copies the thread into a new session on the same app-server. Empty
/// fields of `opts.config` are taken from this session, because Codex does
/// not carry a thread's MCP servers, prompts, or model into a fork.
pub fn fork(session: *Session, opts: agent.StartOptions, diag: ?*Diagnostics) Error!*Session {
    const codex = CodexSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    var start = opts;
    start.config = providerkit.inheritConfig(opts.config, codex.cfg, try codex.rt().sessionInfo(a));
    start.config.resume_id = try a.dupe(u8, codex.threadId().slice());
    start.config.fork = true;
    if (start.tool_handler == null) start.tool_handler = codex.session.tool_handler;
    const child = try CodexSession.create(codex.env, start, false, codex.experimental, codex.session.capabilities, diag);
    codex.client.retain();
    child.open(codex.client, diag) catch |e| {
        child.discard();
        codex.client.release();
        return e;
    };
    return &child.session;
}

/// Reads the account's usage windows with account/rateLimits/read: the main
/// Codex allowance first, then model-specific ones, each with `limit_id`
/// set. Later updates are merged into this snapshot.
pub fn rateLimits(session: *Session, a: Allocator, diag: ?*Diagnostics) Error![]const types.RateLimit {
    const codex = CodexSession.of(session);
    const response = try codex.threadCall(a, "account/rateLimits/read", null, 0, diag);
    var snapshots: ObjectBuilder = .init(a);
    if (response.get("rateLimitsByLimitId").object()) |by_limit| try snapshots.putAll(by_limit);
    if (snapshots.map.count() == 0) if (response.get("rateLimits").v) |main| if (main == .object) {
        try snapshots.put(decode.limitId(JsonView.of(main)), main);
    };
    const ids = try a.dupe([]const u8, snapshots.map.keys());
    std.mem.sort([]const u8, ids, {}, struct {
        fn less(_: void, x: []const u8, y: []const u8) bool {
            const x_main = std.mem.eql(u8, x, decode.main_limit_id);
            const y_main = std.mem.eql(u8, y, decode.main_limit_id);
            if (x_main or y_main) return x_main and !y_main;
            return std.mem.lessThan(u8, x, y);
        }
    }.less);
    var out: std.ArrayList(types.RateLimit) = .empty;
    codex.mutex.lockUncancelable(codex.env.io);
    defer codex.mutex.unlock(codex.env.io);
    for (ids) |id| {
        try translate.storeRateLimitLocked(codex, id, try agent.json.encodeValue(a, snapshots.map.get(id).?));
        const snapshot = translate.rateLimitSnapshotLocked(codex, a, id);
        try out.appendSlice(a, try decode.rateLimits(a, snapshot, providerkit.firstOf(&.{ snapshot.str("limitId"), id })));
    }
    return out.items;
}

/// Sends any app-server method with `params` and returns the result.
pub fn call(session: *Session, method: []const u8, params: ?RawJson, a: Allocator, diag: ?*Diagnostics) Error!?RawJson {
    const codex = CodexSession.of(session);
    return try codex.client.callRaw(a, method, if (params) |raw| raw.bytes else null, .{ .owner = codex }, diag);
}

/// Fills the session info's commands with the skills available in the
/// working directory. Runs as a session task; the session works without
/// the list.
pub fn loadSkills(codex: *CodexSession) void {
    var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
    defer arena.deinit();
    const commands = skillCommands(codex, arena.allocator()) catch |e| {
        if (e == error.OutOfMemory) return log.err("codex: listing skills failed: out of memory", .{});
        return log.debug("codex: listing skills failed: {t}", .{e});
    };
    codex.updateInfo(.{ .commands = commands });
}

fn skillCommands(codex: *CodexSession, a: Allocator) Error![]const types.SlashCommand {
    const info = try codex.rt().sessionInfo(a);
    const params = try agent.json.toValue(a, .{ .cwds = if (info.cwd.len > 0) &[_][]const u8{info.cwd} else null });
    const response = try codex.client.callJson(a, "skills/list", params, .{ .owner = codex, .timeout_ms = 30_000 }, null);
    var commands: std.ArrayList(types.SlashCommand) = .empty;
    for (response.get("data").items()) |entry| for (JsonView.of(entry).get("skills").items()) |skill_value| {
        const skill = JsonView.of(skill_value);
        if (!skill.flag("enabled")) continue;
        const short = providerkit.firstOf(&.{ skill.get("interface").str("shortDescription"), skill.str("shortDescription") });
        try commands.append(a, .{
            .name = skill.str("name"),
            .description = providerkit.firstOf(&.{ short, skill.str("description") }),
            .kind = "skill",
            .path = skill.str("path"),
            .scope = skill.str("scope"),
        });
    };
    return commands.items;
}

/// sandbox_workspace_write from config/read as a native workspace-write
/// policy (JSON), or null when it is not set or cannot be read.
pub fn configuredWorkspaceWrite(codex: *CodexSession, a: Allocator) ?[]const u8 {
    return readWorkspaceWrite(codex, a) catch |e| {
        log.debug("codex: reading sandbox_workspace_write failed: {t}", .{e});
        return null;
    };
}

fn readWorkspaceWrite(codex: *CodexSession, a: Allocator) Error!?[]const u8 {
    const info = try codex.rt().sessionInfo(a);
    const params = try agent.json.toValue(a, .{ .includeLayers = false, .cwd = if (info.cwd.len > 0) info.cwd else null });
    const response = try codex.client.callJson(a, "config/read", params, .{ .owner = codex, .timeout_ms = 10_000 }, null);
    const workspace_write = response.get("config").get("sandbox_workspace_write");
    if (workspace_write.object() == null) return null;
    var policy: ObjectBuilder = .init(a);
    try policy.str("type", "workspaceWrite");
    try policy.put("writableRoots", try agent.json.stringArray(a, try workspace_write.get("writable_roots").strings(a)));
    try policy.put("networkAccess", .{ .bool = workspace_write.flag("network_access") });
    try policy.put("excludeSlashTmp", .{ .bool = workspace_write.flag("exclude_slash_tmp") });
    try policy.put("excludeTmpdirEnvVar", .{ .bool = workspace_write.flag("exclude_tmpdir_env_var") });
    return try agent.json.encodeValue(a, policy.value());
}

/// Restores an archived thread. A thread that is not archived is not an
/// error.
pub fn unarchive(codex: *CodexSession, a: Allocator, thread: []const u8, diag: ?*Diagnostics) Error!void {
    var call_diag: Diagnostics = .{};
    _ = codex.client.call(a, "thread/unarchive", try agent.json.toValue(a, .{ .threadId = thread }), .{ .owner = codex }, &call_diag) catch |e| {
        const info = Diagnostics.of(&call_diag, e);
        if (decode.containsIgnoreCase(session_module.protocolMessage(info), "no archived rollout found")) return;
        return session_module.forward(diag, info, e);
    };
}
