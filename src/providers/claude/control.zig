//! The control protocol: requests the host sends the CLI and their
//! responses, requests the CLI sends the host (permissions, host tool MCP
//! messages, hooks, elicitations, dialogs), and the native `call`
//! passthrough.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const agent = @import("agent");
const types = agent.types;
const errors = agent.errors;
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const ObjectBuilder = agent.json.ObjectBuilder;
const Session = agent.Session;
const Diagnostics = agent.Diagnostics;
const kit = agent.providerkit;
const eql = kit.eql;

const session_mod = @import("session.zig");
const permissions = @import("permissions.zig");
const launch_mod = @import("launch.zig");
const rewind_mod = @import("rewind.zig");
const ClaudeSession = session_mod.ClaudeSession;
const Launch = session_mod.Launch;
const log = std.log.scoped(.agent);

/// A host control request waiting for the CLI's response.
pub const Waiter = struct {
    id: agent.Id,
    launch: *Launch,
    event: Io.Event = .unset,
    /// The response object (gpa-owned), set by the reader task.
    response: ?[]u8 = null,
    failed: bool = false,
};

/// How often a wait with a deadline or a cancel flag looks at them.
const poll_slice_ms = 50;

pub const ControlOptions = struct {
    /// Gives up, sending control_cancel_request, after this long. Zero
    /// waits until the response or the process exit.
    timeout_ms: i64 = 0,
    /// Gives up when set, polled while waiting.
    cancel: ?*const Io.Event = null,
};

/// Sends a control request and returns the response payload (copied into
/// `a`; empty when the CLI sent none). `payload` is a JSON object or null.
pub fn control(claude: *ClaudeSession, a: Allocator, subtype: []const u8, payload: ?RawJson, options: ControlOptions, diag: ?*Diagnostics) errors.Error![]const u8 {
    const rt = claude.rt();
    try rt.ensureOpen(diag);
    const io = claude.io();
    var body: ObjectBuilder = .init(a);
    if (payload) |raw| {
        const value = agent.json.valueOf(a, raw);
        if (value == null or value.? != .object) return errors.fail(diag, .invalid_params, "claude: {s} payload must be a JSON object", .{subtype});
        body.map = value.?.object;
    }
    try body.str("subtype", subtype);
    const launch = claude.current() orelse return rt.closedError(diag);
    var id_buf: [32]u8 = undefined;
    var waiter: Waiter = .{ .id = .of(std.fmt.bufPrint(&id_buf, "req_{d}", .{claude.next_id.fetchAdd(1, .monotonic) + 1}) catch unreachable), .launch = launch };
    {
        claude.control_mutex.lockUncancelable(io);
        defer claude.control_mutex.unlock(io);
        try claude.waiters.put(claude.gpa(), waiter.id.slice(), &waiter);
    }
    defer {
        claude.control_mutex.lockUncancelable(io);
        _ = claude.waiters.swapRemove(waiter.id.slice());
        claude.control_mutex.unlock(io);
        if (waiter.response) |response| claude.gpa().free(response);
    }
    var message: ObjectBuilder = .init(a);
    try message.str("type", "control_request");
    try message.str("request_id", waiter.id.slice());
    try message.put("request", body.value());
    try launch.write(try agent.json.encodeValue(a, message.value()), diag);

    if (!waitFor(claude, &waiter, options)) {
        const cancel = try RawJson.encode(a, .{ .type = "control_cancel_request", .request_id = waiter.id.slice() });
        // Best effort: the request already failed, and a CLI that misses
        // the cancel only answers into a request nobody waits for.
        launch.write(cancel.bytes, null) catch {};
        if (options.cancel) |c| if (c.isSet()) return errors.fail(diag, .cancelled, "claude: {s} cancelled", .{subtype});
        return errors.fail(diag, .timeout, "claude: {s} timed out", .{subtype});
    }
    // The response was stored under the lock before the event was set.
    claude.control_mutex.lockUncancelable(io);
    const failed = waiter.failed and waiter.response == null;
    const response_bytes = if (waiter.response) |r| try a.dupe(u8, r) else "";
    claude.control_mutex.unlock(io);
    if (failed) {
        if (launch.retired.load(.acquire)) return errors.fail(diag, .closed, "claude: the CLI process was restarted by rewind", .{});
        if (launch.exitError()) |exit| return errors.failWith(diag, exit);
        return rt.closedError(diag);
    }
    const response = JsonView.parseBytes(a, response_bytes);
    if (eql(subtype, "initialize")) redeliver(claude, launch, a, response);
    if (!eql(response.str("subtype"), "success")) {
        return errors.failWith(diag, .{
            .kind = .protocol,
            .message = try std.fmt.allocPrint(a, "claude: {s}: {s}", .{ subtype, response.str("error") }),
            .code = response.str("error_code"),
        });
    }
    return if (try response.get("response").raw(a)) |r| r.bytes else "";
}

/// Waits for the waiter's response; false on timeout or cancellation.
fn waitFor(claude: *ClaudeSession, waiter: *Waiter, options: ControlOptions) bool {
    const io = claude.io();
    if (options.cancel == null and options.timeout_ms <= 0) {
        waiter.event.waitUncancelable(io);
        return true;
    }
    const deadline: ?Io.Timeout = if (options.timeout_ms > 0) kit.timeoutMs(options.timeout_ms).toDeadline(io) else null;
    while (!waiter.event.isSet()) {
        if (options.cancel) |c| if (c.isSet()) return false;
        if (deadline) |d| if ((d.toDurationFromNow(io) orelse return false).raw.nanoseconds <= 0) return false;
        // A timeout here only ends one poll slice.
        _ = kit.waitEvent(io, &waiter.event, poll_slice_ms);
    }
    return true;
}

/// Wakes the control requests sent to `launch`, whose process exited.
pub fn failWaiters(claude: *ClaudeSession, launch: *Launch) void {
    const io = claude.io();
    claude.control_mutex.lockUncancelable(io);
    defer claude.control_mutex.unlock(io);
    for (claude.waiters.values()) |waiter| if (waiter.launch == launch) {
        waiter.failed = true;
        waiter.event.set(io);
    };
}

/// The stdout message types the control protocol handles; any other type
/// goes to the translator.
const MessageType = enum { control_response, control_request, control_cancel_request, keep_alive, transcript_mirror };

/// Handles one stdout line of `launch`'s process. Runs on the reader task.
pub fn handleLine(claude: *ClaudeSession, launch: *Launch, a: Allocator, line: []const u8) !void {
    const message = JsonView.parseBytes(a, line);
    if (message.object() == null) {
        log.warn("claude: ignoring malformed output: {s}", .{line[0..@min(line.len, 200)]});
        return;
    }
    const message_type = std.meta.stringToEnum(MessageType, message.str("type")) orelse
        return launch.translator.handle(a, message, line);
    switch (message_type) {
        .control_response => {
            const response = message.get("response");
            const io = claude.io();
            claude.control_mutex.lockUncancelable(io);
            defer claude.control_mutex.unlock(io);
            if (claude.waiters.get(response.str("request_id"))) |waiter| if (waiter.response == null) {
                const raw = (try response.raw(a)) orelse return;
                waiter.response = try claude.gpa().dupe(u8, raw.bytes);
                waiter.event.set(io);
            };
        },
        .control_request => {
            const request = (try message.get("request").raw(a)) orelse RawJson.of("{}");
            deliverControlRequest(claude, launch, message.str("request_id"), request.bytes, line);
        },
        .control_cancel_request => {
            const id = message.str("request_id");
            {
                claude.control_mutex.lockUncancelable(claude.io());
                defer claude.control_mutex.unlock(claude.io());
                if (launch.requests.getPtr(id)) |unanswered| unanswered.* = false;
            }
            claude.rt().withdrawRequest(id);
        },
        // Nothing to report.
        .keep_alive, .transcript_mirror => {},
    }
}

/// Handles the permission prompts and dialogs an initialize response
/// carries again, such as ones a resumed session left open.
fn redeliver(claude: *ClaudeSession, launch: *Launch, a: Allocator, response: JsonView) void {
    for ([_][]const u8{ "pending_permission_requests", "pending_user_dialog_requests" }) |field| {
        for (response.get(field).items()) |value| {
            const envelope = JsonView.of(value);
            const id = envelope.str("request_id");
            if (id.len == 0) continue;
            const subtype = envelope.get("request").str("subtype");
            if (!eql(subtype, "can_use_tool") and !eql(subtype, "request_user_dialog")) continue;
            const request = (envelope.get("request").raw(a) catch {
                log.err("claude: out of memory: pending request {s} not delivered", .{id});
                continue;
            }) orelse continue;
            const request_line = (envelope.raw(a) catch {
                log.err("claude: out of memory: pending request {s} not delivered", .{id});
                continue;
            }) orelse continue;
            deliverControlRequest(claude, launch, id, request.bytes, request_line.bytes);
        }
    }
}

/// Handles a CLI control request of `launch` once, on its own task,
/// recording it there so the answer goes back to that launch's process.
fn deliverControlRequest(claude: *ClaudeSession, launch: *Launch, id: []const u8, request: []const u8, line: []const u8) void {
    const gpa = claude.gpa();
    {
        claude.control_mutex.lockUncancelable(claude.io());
        defer claude.control_mutex.unlock(claude.io());
        if (launch.requests.contains(id)) return;
        launch.requests.put(gpa, id, true) catch return log.err("claude: out of memory: control request {s} dropped", .{id});
    }
    const job = Job.create(gpa, id, request, line) catch return log.err("claude: out of memory: control request {s} dropped", .{id});
    claude.rt().spawn(Job.run, .{ job, claude, launch }) catch {
        job.destroy(gpa);
        log.err("claude: cannot start a task for control request {s}", .{id});
    };
}

const Job = struct {
    arena: std.heap.ArenaAllocator,
    id: []const u8,
    request: []const u8,
    line: []const u8,

    fn create(gpa: Allocator, id: []const u8, request: []const u8, line: []const u8) Allocator.Error!*Job {
        const job = try gpa.create(Job);
        job.arena = .init(gpa);
        const a = job.arena.allocator();
        job.id = a.dupe(u8, id) catch |e| return job.fail(gpa, e);
        job.request = a.dupe(u8, request) catch |e| return job.fail(gpa, e);
        job.line = a.dupe(u8, line) catch |e| return job.fail(gpa, e);
        return job;
    }

    fn fail(job: *Job, gpa: Allocator, e: Allocator.Error) Allocator.Error {
        job.destroy(gpa);
        return e;
    }

    fn destroy(job: *Job, gpa: Allocator) void {
        job.arena.deinit();
        gpa.destroy(job);
    }

    fn run(job: *Job, claude: *ClaudeSession, launch: *Launch) void {
        defer job.destroy(claude.gpa());
        handleControlRequest(claude, launch, job.arena.allocator(), job.id, job.request, job.line) catch |e|
            log.err("claude: handling control request {s} failed: {t}", .{ job.id, e });
    }
};

/// The control request subtypes the CLI sends the host.
const RequestSubtype = enum { can_use_tool, elicitation, hook_callback, mcp_message, request_user_dialog };

fn handleControlRequest(claude: *ClaudeSession, launch: *Launch, a: Allocator, id: []const u8, request_bytes: []const u8, line: []const u8) !void {
    {
        claude.control_mutex.lockUncancelable(claude.io());
        defer claude.control_mutex.unlock(claude.io());
        const unanswered = launch.requests.getPtr(id) orelse return;
        if (!unanswered.*) return;
        if (launch.retired.load(.acquire)) {
            // A rewind replaced the asking process before this ran.
            unanswered.* = false;
            return;
        }
    }
    const request = JsonView.parseBytes(a, request_bytes);
    const subtype_name = request.str("subtype");
    const subtype = std.meta.stringToEnum(RequestSubtype, subtype_name) orelse
        return answer(claude, a, id, null, try std.fmt.allocPrint(a, "unsupported control request subtype: {s}", .{subtype_name}));
    switch (subtype) {
        .can_use_tool => try permissions.handlePermission(claude, a, id, request, request_bytes, line),
        .elicitation => try permissions.handleElicitation(claude, a, id, request, line),
        .hook_callback => if (handleHook(claude, request)) |payload| {
            answer(claude, a, id, payload, null);
        } else |_| answer(claude, a, id, null, try std.fmt.allocPrint(a, "no hook callback \"{s}\"", .{request.str("callback_id")})),
        .mcp_message => {
            const payload = handleMcp(claude, a, request) catch |e| switch (e) {
                error.OutOfMemory => return e,
                error.NotFound => return answer(claude, a, id, null, try std.fmt.allocPrint(a, "SDK MCP server not found: {s}", .{request.str("server_name")})),
            };
            answer(claude, a, id, payload, null);
        },
        .request_user_dialog => try permissions.handleDialog(claude, a, id, request, request_bytes, line),
    }
}

/// Answers a CLI control request from a handler task, where the failure
/// cannot be returned. A request withdrawn meanwhile needs no answer; any
/// other failure leaves the CLI waiting, so it is logged.
fn answer(claude: *ClaudeSession, a: Allocator, id: []const u8, payload: ?[]const u8, err: ?[]const u8) void {
    var diag: Diagnostics = .{};
    respond(claude, a, id, payload, err, &diag) catch |e| switch (e) {
        error.RequestDone => {},
        else => log.err("claude: answering control request {s} failed: {s}", .{ id, Diagnostics.of(&diag, e).message }),
    };
}

/// Answers the hook callbacks the effort tracking registered. They change
/// nothing; they read the effort level a subagent runs at.
fn handleHook(claude: *ClaudeSession, request: JsonView) error{NoHookCallback}![]const u8 {
    const id = request.str("callback_id");
    const known = claude.opts.track_subagent_effort and std.mem.startsWith(u8, id, "hook_") and
        (std.fmt.parseInt(usize, id[5..], 10) catch launch_mod.effort_hook_events.len) < launch_mod.effort_hook_events.len;
    if (!known) return error.NoHookCallback;
    const input = request.get("input");
    const agent_id = input.str("agent_id");
    const level = input.get("effort").str("level");
    if (agent_id.len > 0 and level.len > 0) claude.translator().subagentEffort(agent_id, level);
    return "{}";
}

fn handleMcp(claude: *ClaudeSession, a: Allocator, request: JsonView) error{ OutOfMemory, NotFound }![]const u8 {
    const server = claude.tools orelse return error.NotFound;
    if (!eql(request.str("server_name"), server.name)) return error.NotFound;
    const message = (try request.get("message").raw(a)) orelse RawJson.of("{}");
    const session_id = try claude.sessionId(a);
    const response = (try server.handle(a, message.bytes, session_id, &claude.session)) orelse "{\"jsonrpc\":\"2.0\",\"result\":{},\"id\":0}";
    return std.fmt.allocPrint(a, "{{\"mcp_response\":{s}}}", .{response});
}

/// Writes the answer to a CLI control request: `payload` (JSON) on
/// success, else the error message `err`.
pub fn respond(claude: *ClaudeSession, a: Allocator, id: []const u8, payload: ?[]const u8, err: ?[]const u8, diag: ?*Diagnostics) errors.Error!void {
    // Only the latest launch can have sent a request that is still open; a
    // launch a rewind retired is never answered.
    const latest = claude.current();
    const launch = blk: {
        claude.control_mutex.lockUncancelable(claude.io());
        defer claude.control_mutex.unlock(claude.io());
        const unanswered = (latest orelse break :blk null).requests.getPtr(id) orelse break :blk null;
        const open = unanswered.*;
        unanswered.* = false;
        break :blk if (open) latest else null;
    };
    // A request withdrawn by the CLI or asked by a process a rewind
    // replaced must not be answered on the current process.
    if (launch == null or launch.?.retired.load(.acquire)) return errors.fail(diag, .request_done, "claude: request {s} was withdrawn or its process restarted", .{id});
    var response: ObjectBuilder = .init(a);
    try response.str("request_id", id);
    if (err) |message| {
        try response.str("subtype", "error");
        try response.str("error", message);
    } else {
        try response.str("subtype", "success");
        try response.put("response", agent.json.parseValue(a, payload orelse "{}") orelse .null);
    }
    var message: ObjectBuilder = .init(a);
    try message.str("type", "control_response");
    try message.put("response", response.value());
    return launch.?.write(try agent.json.encodeValue(a, message.value()), diag);
}

// ---------------------------------------------------------------- initialize data

/// The model the CLI picks when none is set: the "default" entry's
/// resolved model, or "".
pub fn defaultModel(init: JsonView) []const u8 {
    for (init.get("models").items()) |value| {
        const model = JsonView.of(value);
        if (eql(model.str("value"), "default")) return model.str("resolvedModel");
    }
    return "";
}

pub fn slashCommands(a: Allocator, list: JsonView) Allocator.Error![]const types.SlashCommand {
    const values = list.items();
    const out = try a.alloc(types.SlashCommand, values.len);
    for (values, out) |value, *o| {
        const command = JsonView.of(value);
        var kind: []const u8 = "command";
        var scope: []const u8 = "builtin";
        if (!command.flag("builtin")) {
            kind = "skill";
            scope = "";
        }
        if (command.str("plugin").len > 0) scope = "plugin";
        o.* = .{ .name = command.str("name"), .description = command.str("description"), .argument_hint = command.str("argumentHint"), .kind = kind, .scope = scope };
    }
    return out;
}

// ---------------------------------------------------------------- native calls

/// Native methods `call` handles itself, or whose answers change session
/// state.
const NativeMethod = enum {
    rewind,
    interrupt,
    cancel_async_message,
    stop_task,
    set_model,
    set_permission_mode,
    rename_session,
    set_cwd,
    initialize,
};

/// Sends control request `method` with `params` and returns the response
/// payload. Requests whose answers change session state update it too;
/// "rewind" (params {"turn_id"}) runs a rewind and returns its result.
pub fn call(s: *Session, method: []const u8, params: ?RawJson, a: Allocator, diag: ?*Diagnostics) errors.Error!?RawJson {
    const claude = ClaudeSession.of(s);
    const native = std.meta.stringToEnum(NativeMethod, method) orelse return sendRequest(claude, a, method, params, diag);
    const fields = JsonView.parseBytes(a, if (params) |p| p.bytes else "{}");
    switch (native) {
        .rewind => return try RawJson.encode(a, try rewind_mod.rewindTurn(claude, a, fields.str("turn_id"), diag)),
        .interrupt => return RawJson.of(try session_mod.interruptWithOptions(claude, a, "", fields.flag("cancel_queued"), diag)),
        .cancel_async_message => {
            const cancelled = try session_mod.cancelQueuedMessage(claude, a, fields.str("message_uuid"), diag);
            return RawJson.of(if (cancelled) "{\"cancelled\":true}" else "{\"cancelled\":false}");
        },
        .stop_task => {
            try s.stopTask(fields.str("task_id"), diag);
            return null;
        },
        .set_model => {
            const response = try sendRequest(claude, a, method, params, diag);
            claude.setInfoField("model", fields.str("model"));
            return response;
        },
        .set_permission_mode => {
            const response = try sendRequest(claude, a, method, params, diag);
            claude.setInfoField("permission_mode", permissions.uniformMode(fields.str("mode")));
            return response;
        },
        .rename_session => {
            const response = try sendRequest(claude, a, method, params, diag);
            claude.setInfoField("title", fields.str("title"));
            return response;
        },
        .set_cwd => {
            const response = try sendRequest(claude, a, method, params, diag);
            claude.setInfoField("cwd", fields.str("path"));
            return response;
        },
        .initialize => {
            const response = try sendRequest(claude, a, method, params, diag);
            try claude.setInitRaw(if (response) |r| r.bytes else "");
            return response;
        },
    }
}

/// Sends control request `method` and returns its response payload, or null
/// when it has none.
fn sendRequest(claude: *ClaudeSession, a: Allocator, method: []const u8, params: ?RawJson, diag: ?*Diagnostics) errors.Error!?RawJson {
    const raw = try control(claude, a, method, params, .{}, diag);
    return if (raw.len == 0) null else RawJson.of(raw);
}

/// The rate-limit window lengths, by name.
pub fn windowMs(window_name: []const u8) i64 {
    const day: i64 = 24 * 3600 * 1000;
    if (eql(window_name, "five_hour")) return 5 * 3600 * 1000;
    if (std.mem.startsWith(u8, window_name, "seven_day")) return 7 * day;
    return 0;
}

/// Reads the claude.ai plan usage windows through get_usage. Returns no
/// windows when plan limits do not apply, such as with an API key.
pub fn rateLimits(s: *Session, a: Allocator, diag: ?*Diagnostics) errors.Error![]const types.RateLimit {
    const claude = ClaudeSession.of(s);
    const raw = try control(claude, a, "get_usage", RawJson.of("{\"skip_behaviors\":true}"), .{}, diag);
    const usage = JsonView.parseBytes(a, raw);
    var out: std.ArrayList(types.RateLimit) = .empty;
    const limits = usage.get("rate_limits");
    if (!usage.flag("rate_limits_available") or limits.object() == null) return out.items;
    for ([_][]const u8{ "five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet" }) |window_name| {
        try addWindow(a, &out, window_name, "", limits.get(window_name));
    }
    for (limits.get("model_scoped").items(), 0..) |value, i| {
        const model_window = JsonView.of(value);
        const display = model_window.str("display_name");
        try addWindow(a, &out, try std.fmt.allocPrint(a, "seven_day_{s}", .{try scopedName(a, display)}), display, model_window);
        if (i == 0) {
            // The CLI lists only overage-included models here; rate-limit
            // events name the first one "seven_day_overage_included".
            try claude.translator().setOverageLimit(display);
        }
    }
    return out.items;
}

fn addWindow(a: Allocator, out: *std.ArrayList(types.RateLimit), window_name: []const u8, limit_id: []const u8, window: JsonView) Allocator.Error!void {
    if (window.get("utilization").isNull()) return;
    const used = window.float("utilization");
    const length = windowMs(window_name);
    try out.append(a, .{
        .name = window_name,
        .limit_id = limit_id,
        .used_percent = used,
        .window_ms = if (length > 0) length else 7 * 24 * 3600 * 1000,
        .resets_at_ms = kit.parseTimeMs(window.str("resets_at")),
        .status = if (used >= 100) .exceeded else .ok,
    });
}

/// A model display name as a window name part, such as "opus_4_5" for
/// "Opus 4.5".
fn scopedName(a: Allocator, display: []const u8) Allocator.Error![]const u8 {
    var b: std.ArrayList(u8) = .empty;
    var under = false;
    for (display) |c_in| {
        const c = std.ascii.toLower(c_in);
        if (std.ascii.isAlphanumeric(c) and c < 0x80) {
            try b.append(a, c);
            under = false;
        } else if (!under and b.items.len > 0) {
            try b.append(a, '_');
            under = true;
        }
    }
    return std.mem.trimEnd(u8, b.items, "_");
}
