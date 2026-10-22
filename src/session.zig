//! The public interfaces: a `Provider` starts `Session`s for one harness. A
//! session is one live conversation. Input goes in with `send` or `run`,
//! events come out of `nextEvent`, and requests (permissions, questions,
//! elicitations, dialogs, native requests) are answered with `respond`.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const types = @import("types.zig");
const errors = @import("errors.zig");
const runtime = @import("runtime.zig");
const RawJson = @import("json.zig").RawJson;

const Error = errors.Error;
const Diagnostics = errors.Diagnostics;
const Runtime = runtime.Runtime;
const log = std.log.scoped(.agent);
pub const Id = runtime.Id;
pub const OwnedEvent = runtime.OwnedEvent;
pub const EventSink = runtime.EventSink;
pub const Env = runtime.Env;
pub const NextEventError = runtime.NextEventError;

pub const StartOptions = struct {
    config: types.Config = .{},
    /// Runs host tool calls; required when `config.tools` is not empty.
    tool_handler: ?types.ToolHandler = null,
    /// Answers requests with callbacks instead of events.
    request_handler: ?RequestHandler = null,
    /// Receives events as they happen instead of the session's queue. It
    /// runs with the session lock held and must not block or call the
    /// session.
    sink: ?EventSink = null,
};

/// Answers requests as they arrive. A request whose kind has a callback is
/// not queued as an event; the callback runs on its own task and its
/// answer is sent with `respond`. `Session.deinit` waits for running
/// callbacks, so a callback must return (check `Session.isClosed` while
/// waiting on a person) and must not free the session itself.
pub const RequestHandler = struct {
    context: ?*anyopaque = null,
    permission: ?*const fn (context: ?*anyopaque, req: *const types.PermissionRequest) types.PermissionResponse = null,
    user_input: ?*const fn (context: ?*anyopaque, req: *const types.UserInputRequest) types.UserInputResponse = null,
    elicitation: ?*const fn (context: ?*anyopaque, req: *const types.ElicitationRequest) types.ElicitationResponse = null,
    dialog: ?*const fn (context: ?*anyopaque, req: *const types.DialogRequest) types.DialogResponse = null,
    native: ?*const fn (context: ?*anyopaque, req: *const types.NativeRequest) types.NativeResponse = null,

    fn handles(h: RequestHandler, kind: types.RequestKind) bool {
        return switch (kind) {
            .permission => h.permission != null,
            .user_input => h.user_input != null,
            .elicitation => h.elicitation != null,
            .dialog => h.dialog != null,
            .native => h.native != null,
        };
    }
};

/// One live conversation with a harness. All methods are safe to call from
/// any thread. Free it with `deinit`, which closes it first.
///
/// Fallible methods take an optional `*Diagnostics` that receives the
/// failure's details; pass null to skip them.
pub const Session = struct {
    pub const Pending = runtime.Pending;

    rt: Runtime,
    vtable: *const VTable,
    capabilities: types.Capabilities,
    tool_handler: ?types.ToolHandler,
    request_handler: ?RequestHandler,
    /// Host tool calls running now, so interrupting can cancel them.
    tool_mutex: Io.Mutex = .init,
    tool_calls: std.ArrayList(Id) = .empty,

    /// Implemented by providers. Optional entries left null are reported as
    /// unsupported. Each entry receives the `Session` embedded in the
    /// provider's session type (use `@fieldParentPtr`) and records failure
    /// details in `diag`.
    pub const VTable = struct {
        /// Submits input as turn `turn_id`, using the runtime's turn queue.
        send: *const fn (s: *Session, in: types.Input, turn_id: []const u8, diag: ?*Diagnostics) Error!void,
        interrupt: *const fn (s: *Session, diag: ?*Diagnostics) Error!void,
        /// Answers a pending request natively.
        respond: *const fn (s: *Session, p: *const Pending, resp: types.Response, diag: ?*Diagnostics) Error!void,
        /// Stops the harness and every provider task, and finishes the
        /// runtime. Idempotent. The runtime is freed right after.
        close: *const fn (s: *Session) void,
        /// Frees the provider session. Called once, after `close`.
        destroy: *const fn (s: *Session) void,

        interrupt_turn: ?*const fn (s: *Session, turn_id: []const u8, diag: ?*Diagnostics) Error!void = null,
        set_model: ?*const fn (s: *Session, model: []const u8, diag: ?*Diagnostics) Error!void = null,
        set_effort: ?*const fn (s: *Session, effort: types.Effort, diag: ?*Diagnostics) Error!void = null,
        set_permission_mode: ?*const fn (s: *Session, mode: types.PermissionMode, diag: ?*Diagnostics) Error!void = null,
        set_title: ?*const fn (s: *Session, title: []const u8, diag: ?*Diagnostics) Error!void = null,
        compact: ?*const fn (s: *Session, instructions: []const u8, diag: ?*Diagnostics) Error!void = null,
        /// Allocates the result from `a`; an empty `session_id` means the
        /// session ID did not change.
        rewind: ?*const fn (s: *Session, turn_id: []const u8, a: Allocator, diag: ?*Diagnostics) Error!types.RewindResult = null,
        stop_task: ?*const fn (s: *Session, task_id: []const u8, diag: ?*Diagnostics) Error!void = null,
        fork: ?*const fn (s: *Session, opts: StartOptions, diag: ?*Diagnostics) Error!*Session = null,
        rate_limits: ?*const fn (s: *Session, a: Allocator, diag: ?*Diagnostics) Error![]const types.RateLimit = null,
        /// Sends a native request: a Claude control subtype, a Codex
        /// app-server method, or a Pi command type.
        call: ?*const fn (s: *Session, method: []const u8, params: ?RawJson, a: Allocator, diag: ?*Diagnostics) Error!?RawJson = null,
    };

    /// Initializes the parts every session shares. Providers call it when
    /// creating their session type.
    pub fn init(env: Env, provider_name: []const u8, vtable: *const VTable, capabilities: types.Capabilities, opts: StartOptions) Session {
        var handled: std.EnumSet(types.RequestKind) = .empty;
        if (opts.request_handler) |h| for (std.enums.values(types.RequestKind)) |kind| {
            if (h.handles(kind)) handled.insert(kind);
        };
        return .{
            .rt = .init(env, provider_name, .{
                .include_raw = opts.config.include_raw,
                .wire_tap = opts.config.wire_tap,
                .event_limit = opts.config.event_buffer_limit,
                .sink = opts.sink,
                .handled_requests = handled,
                .on_request = if (opts.request_handler != null) answerWithHandler else null,
            }),
            .vtable = vtable,
            .capabilities = capabilities,
            .tool_handler = opts.tool_handler,
            .request_handler = opts.request_handler,
        };
    }

    pub fn provider(s: *Session) []const u8 {
        return s.rt.provider;
    }

    /// The native session ID (the value to pass as `Config.resume_id`).
    pub fn id(s: *Session, a: Allocator) Allocator.Error![]u8 {
        return s.rt.sessionId(a);
    }

    pub fn info(s: *Session, a: Allocator) Allocator.Error!types.SessionInfo {
        return s.rt.sessionInfo(a);
    }

    /// A snapshot of the session: status, running and queued turns,
    /// unfinished tasks, pending requests, usage, and info.
    pub fn state(s: *Session, a: Allocator) Allocator.Error!types.SessionState {
        return s.rt.sessionState(a);
    }

    /// Waits up to `timeout` for the next event. Free it with `destroy`.
    /// Each event is delivered once: events a `Turn.nextEvent` took are not
    /// here, and session-level events (`session_started`, `session_updated`,
    /// `session_id_changed`, `wire`) are only here, so read them now and
    /// then when using turns with a long-lived session.
    pub fn nextEvent(s: *Session, timeout: Io.Timeout) NextEventError!*OwnedEvent {
        return s.rt.nextEvent(timeout);
    }

    /// Submits input. When a turn is running, `in.delivery` decides whether
    /// the input is queued, steered into it, or interrupts it.
    pub fn send(s: *Session, in: types.Input, diag: ?*Diagnostics) Error!Turn {
        const generated = s.rt.env.uuid();
        return s.sendAs(generated.slice(), in, diag);
    }

    /// `send` with the turn ID chosen by the caller. Fails with
    /// `invalid_params` when the session already knows that turn ID.
    pub fn sendAs(s: *Session, turn_id: []const u8, in: types.Input, diag: ?*Diagnostics) Error!Turn {
        try checkId(turn_id, diag);
        if (turn_id.len == 0) return errors.fail(diag, .invalid_params, "empty turn ID", .{});
        try s.rt.ensureOpen(diag);
        if (s.rt.knowsTurn(turn_id)) return errors.fail(diag, .invalid_params, "turn ID {s} is in use", .{turn_id});
        const turn: Id = .of(turn_id);
        try s.vtable.send(s, in, turn.slice(), diag);
        return .{ .session = s, .id = turn };
    }

    /// Waits for turn `turn_id` to end. A turn that failed or was
    /// interrupted returns its result; check `status`. Input that never ran
    /// (rejected, cancelled, or the session closed) fails with its error.
    pub fn wait(s: *Session, turn_id: []const u8, a: Allocator, timeout: Io.Timeout, diag: ?*Diagnostics) Error!types.TurnResult {
        try checkId(turn_id, diag);
        return s.rt.waitTurn(turn_id, a, timeout, diag);
    }

    /// Sends input and waits for its turn. When `timeout` passes first, the
    /// turn is interrupted and `error.Timeout` returned.
    pub fn run(s: *Session, in: types.Input, a: Allocator, timeout: Io.Timeout, diag: ?*Diagnostics) Error!types.TurnResult {
        var turn = try s.send(in, diag);
        return turn.wait(a, timeout, diag) catch |e| {
            if (e == error.Timeout) turn.interrupt(null) catch s.interrupt(null) catch {};
            return e;
        };
    }

    /// Stops the running turn. Queued input still runs afterwards.
    pub fn interrupt(s: *Session, diag: ?*Diagnostics) Error!void {
        try s.rt.ensureOpen(diag);
        s.cancelToolCalls();
        return s.vtable.interrupt(s, diag);
    }

    /// Stops one turn: a queued turn is dropped and a running turn is
    /// interrupted. A turn that already ended is not an error.
    pub fn interruptTurn(s: *Session, turn_id: []const u8, diag: ?*Diagnostics) Error!void {
        try checkId(turn_id, diag);
        try s.rt.ensureOpen(diag);
        if (s.rt.cancelQueued(turn_id)) return;
        const f = s.vtable.interrupt_turn orelse return s.unsupported("interrupt_turn", diag);
        // Only one turn runs at a time, so running tool calls are its own.
        s.cancelToolCalls();
        const native = s.rt.nativeTurnId(turn_id);
        return f(s, native.slice(), diag);
    }

    pub fn setModel(s: *Session, model: []const u8, diag: ?*Diagnostics) Error!void {
        return s.optional("set_model", s.vtable.set_model, .{model}, diag);
    }

    pub fn setEffort(s: *Session, effort: types.Effort, diag: ?*Diagnostics) Error!void {
        return s.optional("set_effort", s.vtable.set_effort, .{effort}, diag);
    }

    pub fn setPermissionMode(s: *Session, mode: types.PermissionMode, diag: ?*Diagnostics) Error!void {
        return s.optional("set_permission_mode", s.vtable.set_permission_mode, .{mode}, diag);
    }

    pub fn setTitle(s: *Session, title: []const u8, diag: ?*Diagnostics) Error!void {
        return s.optional("set_title", s.vtable.set_title, .{title}, diag);
    }

    /// Compacts the context; returns after compaction finished.
    pub fn compact(s: *Session, instructions: []const u8, diag: ?*Diagnostics) Error!void {
        return s.optional("compact", s.vtable.compact, .{instructions}, diag);
    }

    /// Drops `turn_id` and every later turn from the conversation. File
    /// changes are not undone. The result, allocated from `a`, has the
    /// session ID afterwards, which changes on some harnesses
    /// (`Capabilities.rewind_changes_session_id`).
    pub fn rewind(s: *Session, turn_id: []const u8, a: Allocator, diag: ?*Diagnostics) Error!types.RewindResult {
        try checkId(turn_id, diag);
        try s.rt.ensureOpen(diag);
        const f = s.vtable.rewind orelse return s.unsupported("rewind", diag);
        const native = s.rt.nativeTurnId(turn_id);
        var res = try f(s, native.slice(), a, diag);
        if (res.session_id.len == 0) res.session_id = try s.rt.sessionId(a);
        return res;
    }

    /// Stops one background task, such as a subagent.
    pub fn stopTask(s: *Session, task_id: []const u8, diag: ?*Diagnostics) Error!void {
        return s.optional("stop_task", s.vtable.stop_task, .{task_id}, diag);
    }

    /// Copies the conversation into a new, independent session.
    pub fn fork(s: *Session, opts: StartOptions, diag: ?*Diagnostics) Error!*Session {
        try checkId(opts.config.fork_at_turn, diag);
        try s.rt.ensureOpen(diag);
        const f = s.vtable.fork orelse return s.unsupported("fork", diag);
        var o = opts;
        const native = s.rt.nativeTurnId(opts.config.fork_at_turn);
        if (o.config.fork_at_turn.len > 0) o.config.fork_at_turn = native.slice();
        return f(s, o, diag);
    }

    /// Reads the account's plan usage windows.
    pub fn rateLimits(s: *Session, a: Allocator, diag: ?*Diagnostics) Error![]const types.RateLimit {
        try s.rt.ensureOpen(diag);
        const f = s.vtable.rate_limits orelse return s.unsupported("rate_limits", diag);
        return f(s, a, diag);
    }

    /// Sends a native request and returns the harness's answer: a Claude
    /// control request subtype, a Codex app-server method, or a Pi command.
    /// Reaches every harness feature, including ones newer than this
    /// library.
    pub fn call(s: *Session, method: []const u8, params: ?RawJson, a: Allocator, diag: ?*Diagnostics) Error!?RawJson {
        try s.rt.ensureOpen(diag);
        const f = s.vtable.call orelse return s.unsupported("call", diag);
        return f(s, method, params, a, diag);
    }

    /// Answers pending request `request_id`. Fails with `request_done` when
    /// it was already answered or withdrawn, and with `invalid_answer` for
    /// an answer that does not fit the request.
    pub fn respond(s: *Session, request_id: []const u8, resp: types.Response, diag: ?*Diagnostics) Error!void {
        try checkId(request_id, diag);
        const Ctx = struct { s: *Session, resp: types.Response };
        return s.rt.answer(request_id, Ctx{ .s = s, .resp = resp }, diag, struct {
            fn answer(c: Ctx, p: *const Pending, d: ?*Diagnostics) Error!void {
                try validate(p, c.resp, d);
                return c.s.vtable.respond(c.s, p, c.resp, d);
            }
        }.answer);
    }

    /// Copies the requests still waiting for an answer, in arrival order.
    pub fn pendingRequests(s: *Session, a: Allocator) Allocator.Error![]types.Event {
        return s.rt.pendingRequests(a);
    }

    /// Ends the session and stops the harness. Idempotent.
    pub fn close(s: *Session) void {
        s.cancelToolCalls();
        s.vtable.close(s);
        s.rt.finish(null);
    }

    /// Closes the session and frees it.
    pub fn deinit(s: *Session) void {
        s.close();
        s.rt.deinit();
        s.tool_calls.deinit(s.rt.env.gpa);
        s.vtable.destroy(s);
    }

    pub fn isClosed(s: *Session) bool {
        return s.rt.isFinished();
    }

    /// Why the session ended; null while it runs or after a clean close.
    pub fn endError(s: *Session, a: Allocator) ?types.ErrorInfo {
        return s.rt.endError(a);
    }

    /// Runs a host tool call through the configured handler, turning a
    /// missing handler into an error result.
    pub fn callTool(s: *Session, arena: Allocator, req: types.ToolCallRequest) types.ToolResult {
        const h = s.tool_handler orelse return types.ToolResult.err(arena, "no handler for host tools") catch .out_of_memory;
        const io = s.rt.env.io;
        const call_id: Id = .of(req.id);
        s.tool_mutex.lockUncancelable(io);
        const tracked = if (s.tool_calls.append(s.rt.env.gpa, call_id)) true else |_| false;
        s.tool_mutex.unlock(io);
        if (!tracked) return types.ToolResult.err(arena, "out of memory") catch .out_of_memory;
        defer {
            s.tool_mutex.lockUncancelable(io);
            for (s.tool_calls.items, 0..) |*c, i| if (c.eql(call_id.slice())) {
                _ = s.tool_calls.swapRemove(i);
                break;
            };
            s.tool_mutex.unlock(io);
        }
        return h.call(h.context, arena, req);
    }

    /// Asks every running host tool call to stop.
    fn cancelToolCalls(s: *Session) void {
        const h = s.tool_handler orelse return;
        const cancel = h.cancel orelse return;
        const io = s.rt.env.io;
        var ids: [16]Id = undefined;
        s.tool_mutex.lockUncancelable(io);
        const n = @min(s.tool_calls.items.len, ids.len);
        @memcpy(ids[0..n], s.tool_calls.items[0..n]);
        s.tool_mutex.unlock(io);
        for (ids[0..n]) |*running| cancel(h.context, running.slice());
    }

    /// Answers request `request_id` with the request handler. Runs as a
    /// runtime task.
    fn answerWithHandler(rt: *Runtime, request_id: Id) void {
        const s: *Session = @fieldParentPtr("rt", rt);
        const h = s.request_handler.?;
        var arena: std.heap.ArenaAllocator = .init(s.rt.env.gpa);
        defer arena.deinit();
        const ev = s.rt.pendingRequest(arena.allocator(), request_id.slice()) catch return orelse return;
        const resp: types.Response = switch (ev.payload) {
            .permission_request => |*req| .{ .permission = h.permission.?(h.context, req) },
            .user_input_request => |*req| .{ .user_input = h.user_input.?(h.context, req) },
            .elicitation_request => |*req| .{ .elicitation = h.elicitation.?(h.context, req) },
            .dialog_request => |*req| .{ .dialog = h.dialog.?(h.context, req) },
            .native_request => |*req| .{ .native = h.native.?(h.context, req) },
            else => return,
        };
        var diag: Diagnostics = .{};
        s.respond(request_id.slice(), resp, &diag) catch |e| switch (e) {
            error.RequestDone, error.Closed => {},
            else => log.warn("answering request {s} failed: {s}", .{ request_id.slice(), Diagnostics.of(&diag, e).message }),
        };
    }

    fn unsupported(s: *Session, feature: []const u8, diag: ?*Diagnostics) Error {
        return errors.unsupported(diag, s.rt.provider, feature);
    }

    fn optional(s: *Session, comptime feature: []const u8, f: anytype, args: anytype, diag: ?*Diagnostics) Error!void {
        try s.rt.ensureOpen(diag);
        const func = f orelse return s.unsupported(feature, diag);
        return @call(.auto, func, .{s} ++ args ++ .{diag});
    }
};

/// Input submitted with `Session.send` and the turn it runs as.
pub const Turn = struct {
    session: *Session,
    id: Id,
    ended: bool = false,

    /// Waits up to `timeout` for the turn's next event, leaving other
    /// events for `Session.nextEvent`. Ends after the turn's
    /// `turn_completed`.
    pub fn nextEvent(t: *Turn, timeout: Io.Timeout) NextEventError!*OwnedEvent {
        if (t.ended) return error.EndOfStream;
        const ev = try t.session.rt.nextEventOf(t.id.slice(), timeout);
        if (ev.event.payload == .turn_completed) t.ended = true;
        return ev;
    }

    /// Waits for the turn to end. See `Session.wait`.
    pub fn wait(t: *const Turn, a: Allocator, timeout: Io.Timeout, diag: ?*Diagnostics) Error!types.TurnResult {
        return t.session.wait(t.id.slice(), a, timeout, diag);
    }

    /// Drops the turn when it is queued, and interrupts it when it runs.
    pub fn interrupt(t: *const Turn, diag: ?*Diagnostics) Error!void {
        return t.session.interruptTurn(t.id.slice(), diag);
    }
};

/// Rejects an identifier too long to be one the library handed out.
fn checkId(value: []const u8, diag: ?*Diagnostics) Error!void {
    if (value.len > Id.capacity) return errors.fail(diag, .invalid_params, "identifier is longer than {d} bytes", .{Id.capacity});
}

fn validate(p: *const Session.Pending, resp: types.Response, diag: ?*Diagnostics) Error!void {
    switch (resp) {
        .permission => |r| {
            if (p.kind != .permission) return mismatch(p, diag);
            if (r.native != null) return;
            const offered = p.permission().decisions;
            switch (r.decision) {
                .deny, .cancel => {},
                else => if (offered.len > 0 and std.mem.indexOfScalar(types.PermissionDecision, offered, r.decision) == null)
                    return errors.fail(diag, .invalid_answer, "permission decision {t} not offered", .{r.decision}),
            }
        },
        .user_input => if (p.kind != .user_input) return mismatch(p, diag),
        .elicitation => if (p.kind != .elicitation) return mismatch(p, diag),
        .dialog => if (p.kind != .dialog) return mismatch(p, diag),
        .native => if (p.kind != .native) return mismatch(p, diag),
    }
}

fn mismatch(p: *const Session.Pending, diag: ?*Diagnostics) Error {
    return errors.fail(diag, .invalid_answer, "request {s} is a {t} request", .{ p.id.slice(), p.kind });
}

/// A harness integration. Create one with a provider's `create`, such as
/// `agent.claude.create(env, .{})`.
pub const Provider = struct {
    ptr: *anyopaque,
    vtable: *const VTable,

    pub const VTable = struct {
        name: []const u8,
        capabilities: types.Capabilities,
        start: *const fn (ptr: *anyopaque, opts: StartOptions, diag: ?*Diagnostics) Error!*Session,
        deinit: *const fn (ptr: *anyopaque) void,
        check: ?*const fn (ptr: *anyopaque, a: Allocator, diag: ?*Diagnostics) Error!types.ProviderStatus = null,
        list_models: ?*const fn (ptr: *anyopaque, a: Allocator, diag: ?*Diagnostics) Error![]const types.Model = null,
        list_sessions: ?*const fn (ptr: *anyopaque, a: Allocator, opts: types.ListSessionsOptions, diag: ?*Diagnostics) Error![]const types.SessionSummary = null,
        read_history: ?*const fn (ptr: *anyopaque, a: Allocator, session_id: []const u8, opts: types.ReadHistoryOptions, diag: ?*Diagnostics) Error![]const types.Item = null,
        delete_session: ?*const fn (ptr: *anyopaque, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) Error!void = null,
        list_subagents: ?*const fn (ptr: *anyopaque, a: Allocator, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) Error![]const types.Task = null,
        /// Sends a native request that needs no session, such as a Codex
        /// app-server method.
        call: ?*const fn (ptr: *anyopaque, method: []const u8, params: ?RawJson, a: Allocator, diag: ?*Diagnostics) Error!?RawJson = null,
    };

    pub fn name(p: Provider) []const u8 {
        return p.vtable.name;
    }

    pub fn capabilities(p: Provider) types.Capabilities {
        return p.vtable.capabilities;
    }

    /// Launches a new or resumed session. Returns after the harness
    /// finished its startup handshake, so startup failures surface here.
    pub fn start(p: Provider, opts: StartOptions, diag: ?*Diagnostics) Error!*Session {
        return p.vtable.start(p.ptr, opts, diag);
    }

    /// Reports whether the harness is installed and signed in, without
    /// starting a session. Problems with the harness are in the status's
    /// `error`; this fails only when the check could not run.
    pub fn check(p: Provider, a: Allocator, diag: ?*Diagnostics) Error!types.ProviderStatus {
        const f = p.vtable.check orelse return errors.unsupported(diag, p.name(), "check");
        return f(p.ptr, a, diag);
    }

    pub fn listModels(p: Provider, a: Allocator, diag: ?*Diagnostics) Error![]const types.Model {
        const f = p.vtable.list_models orelse return errors.unsupported(diag, p.name(), "list_models");
        return f(p.ptr, a, diag);
    }

    /// Persisted sessions, newest first.
    pub fn listSessions(p: Provider, a: Allocator, opts: types.ListSessionsOptions, diag: ?*Diagnostics) Error![]const types.SessionSummary {
        const f = p.vtable.list_sessions orelse return errors.unsupported(diag, p.name(), "list_sessions");
        return f(p.ptr, a, opts, diag);
    }

    /// The persisted transcript of a session, without starting it.
    pub fn readHistory(p: Provider, a: Allocator, session_id: []const u8, opts: types.ReadHistoryOptions, diag: ?*Diagnostics) Error![]const types.Item {
        const f = p.vtable.read_history orelse return errors.unsupported(diag, p.name(), "read_history");
        return f(p.ptr, a, session_id, opts, diag);
    }

    /// Deletes a persisted session. Cannot be undone.
    pub fn deleteSession(p: Provider, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) Error!void {
        const f = p.vtable.delete_session orelse return errors.unsupported(diag, p.name(), "delete_session");
        return f(p.ptr, session_id, cwd, diag);
    }

    /// The subagents a persisted session ran.
    pub fn listSubagents(p: Provider, a: Allocator, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) Error![]const types.Task {
        const f = p.vtable.list_subagents orelse return errors.unsupported(diag, p.name(), "list_subagents");
        return f(p.ptr, a, session_id, cwd, diag);
    }

    /// Sends a native request without a session and returns the harness's
    /// answer, such as Codex's `account/read`.
    pub fn call(p: Provider, method: []const u8, params: ?RawJson, a: Allocator, diag: ?*Diagnostics) Error!?RawJson {
        const f = p.vtable.call orelse return errors.unsupported(diag, p.name(), "call");
        return f(p.ptr, method, params, a, diag);
    }

    pub fn deinit(p: Provider) void {
        p.vtable.deinit(p.ptr);
    }
};
