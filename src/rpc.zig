//! The library as a JSON-RPC 2.0 peer: the protocol behind the C ABI, the
//! `agentd` executable, and every language binding.
//!
//! The client sends requests (`session/start`, `session/send`, ...) and
//! answers the library's requests (`tool/call`). The library sends
//! `session/event` and `session/closed` notifications. Requests run as
//! concurrent tasks; requests for one session that change it run in the order
//! they arrived, while `session/wait`, `session/info`, `session/state`,
//! `session/pendingRequests`, `session/call`, `session/interrupt`,
//! `session/interruptTurn`, `session/close`, and `request/respond` run at
//! once.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const agent = @import("root.zig");
const types = agent.types;
const errors = agent.errors;
const json = agent.json;
const registry = @import("registry.zig");
const RawJson = json.RawJson;
const Session = agent.Session;
const Diagnostics = errors.Diagnostics;
const log = std.log.scoped(.agent);

pub const protocol_version = 1;
pub const library_version = "0.3.0";

// ---------------------------------------------------------------- protocol

pub const InitializeParams = struct {
    protocol_version: u32 = protocol_version,
    /// Providers to offer, by name, with their options. Empty offers every
    /// real harness with default options.
    providers: ?std.json.ArrayHashMap(?RawJson) = null,
};

pub const InitializeResult = struct {
    protocol_version: u32,
    library_version: []const u8,
    providers: []const ProviderInfo,
};

pub const ProviderInfo = struct {
    name: []const u8,
    capabilities: types.Capabilities,
};

pub const ProviderParams = struct { provider: []const u8 };
pub const ListSessionsParams = struct { provider: []const u8, options: types.ListSessionsOptions = .{} };
pub const ReadHistoryParams = struct { provider: []const u8, session_id: []const u8, options: types.ReadHistoryOptions = .{} };
pub const DeleteSessionParams = struct { provider: []const u8, session_id: []const u8, cwd: []const u8 = "" };
pub const ListSubagentsParams = struct { provider: []const u8, session_id: []const u8, cwd: []const u8 = "" };
pub const ProviderCallParams = struct { provider: []const u8, method: []const u8, params: ?RawJson = null };

pub const StartParams = struct {
    provider: []const u8,
    config: types.Config = .{},
    /// The handle the new session gets, chosen by the client so it can
    /// route the session's events before this request is answered. Empty
    /// lets the library choose one.
    handle: []const u8 = "",
};
pub const ForkParams = struct {
    session: []const u8,
    config: types.Config = .{},
    /// As in `StartParams`, for the new session.
    handle: []const u8 = "",
};

pub const StartResult = struct {
    /// The library's handle for the session, used by every `session/*`
    /// method. It stays the same when the native ID changes.
    session: []const u8,
    info: types.SessionInfo,
    capabilities: types.Capabilities,
};

pub const SessionParams = struct { session: []const u8 };
pub const SendParams = struct {
    session: []const u8,
    input: types.Input,
    /// The turn ID, chosen by the client so it can route the turn's events
    /// before this request is answered. Empty lets the library choose one.
    turn_id: []const u8 = "",
};
pub const SendResult = struct { turn_id: []const u8 };
pub const WaitParams = struct {
    session: []const u8,
    turn_id: []const u8,
    /// Zero waits until the turn ends.
    timeout_ms: i64 = 0,
};
pub const TurnParams = struct { session: []const u8, turn_id: []const u8 };
pub const ValueParams = struct { session: []const u8, value: []const u8 };
pub const CompactParams = struct { session: []const u8, instructions: []const u8 = "" };
pub const StopTaskParams = struct { session: []const u8, task_id: []const u8 };
pub const CallParams = struct { session: []const u8, method: []const u8, params: ?RawJson = null };

/// Exactly one response is set, matching the request's kind.
pub const RespondParams = struct {
    session: []const u8,
    request_id: []const u8,
    permission: ?types.PermissionResponse = null,
    user_input: ?types.UserInputResponse = null,
    elicitation: ?types.ElicitationResponse = null,
    dialog: ?types.DialogResponse = null,
    native: ?types.NativeResponse = null,
};

/// `session/event` notification.
pub const EventNotification = struct { session: []const u8, event: types.Event };
/// `session/closed` notification.
pub const ClosedNotification = struct { session: []const u8, @"error": ?types.ErrorInfo = null };
/// `tool/call` request from the library; the result is a `ToolResult`.
pub const ToolCallParams = struct { session: []const u8, call: types.ToolCallRequest };
/// `tool/cancel` notification: the turn running tool call `call_id` was
/// interrupted or the session closed. The library no longer waits for its
/// result.
pub const ToolCancelParams = struct { session: []const u8, call_id: []const u8 };

/// JSON-RPC error codes. Library failures use `library_error` with an
/// `ErrorInfo` as data.
pub const code = struct {
    pub const parse_error = -32700;
    pub const invalid_request = -32600;
    pub const method_not_found = -32601;
    pub const invalid_params = -32602;
    pub const library_error = -32000;
};

// ---------------------------------------------------------------- server

const Method = enum {
    initialize,
    @"provider/check",
    @"provider/listModels",
    @"provider/listSessions",
    @"provider/readHistory",
    @"provider/deleteSession",
    @"provider/listSubagents",
    @"provider/call",
    @"session/start",
    @"session/fork",
    @"session/send",
    @"session/wait",
    @"session/info",
    @"session/state",
    @"session/interrupt",
    @"session/interruptTurn",
    @"session/setModel",
    @"session/setEffort",
    @"session/setPermissionMode",
    @"session/setTitle",
    @"session/compact",
    @"session/rewind",
    @"session/stopTask",
    @"session/rateLimits",
    @"session/pendingRequests",
    @"session/call",
    @"session/close",
    @"request/respond",

    /// Methods that change a session run in arrival order. `session/call`
    /// runs at once, so a native call is not stuck behind a compaction.
    fn ordered(m: Method) bool {
        return switch (m) {
            .@"session/wait", .@"session/interrupt", .@"session/interruptTurn", .@"session/close", .@"request/respond", .@"session/info", .@"session/state", .@"session/pendingRequests", .@"session/call" => false,
            else => std.mem.startsWith(u8, @tagName(m), "session/"),
        };
    }
};

pub const Server = struct {
    env: agent.Env,

    mutex: Io.Mutex = .init,
    /// Signalled when the outbox gets a message, and when a shutdown starts
    /// or finishes.
    cond: Io.Condition = .init,
    outbox: std.Deque([:0]u8) = .empty,
    closed: bool = false,
    /// `shutdown` finished: no message is added to the outbox anymore.
    shut_down: bool = false,
    /// Requests being handled. Each is counted under the mutex, with the
    /// `closed` check, so `shutdown` waits for every request it let in.
    tasks: agent.runtime.TaskScope = .{},

    providers: std.StringArrayHashMapUnmanaged(agent.Provider) = .empty,
    initialized: bool = false,
    sessions: std.StringHashMapUnmanaged(*Entry) = .empty,
    /// Handles not in `sessions` that are still in use: sessions starting,
    /// and closed sessions not yet freed, whose last notifications must not
    /// reach a new session with the same handle.
    reserved: std.StringHashMapUnmanaged(void) = .empty,
    next_session: u64 = 1,

    calls: std.AutoHashMapUnmanaged(u64, *Call) = .empty,
    next_call: u64 = 1,

    pub fn create(env: agent.Env) Allocator.Error!*Server {
        const s = try env.gpa.create(Server);
        s.* = .{ .env = env };
        return s;
    }

    /// Closes every session and frees the server. Messages not yet read
    /// are dropped.
    pub fn destroy(s: *Server) void {
        s.shutdown();
        const gpa = s.env.gpa;
        while (s.outbox.popFront()) |m| gpa.free(m);
        s.outbox.deinit(gpa);
        s.sessions.deinit(gpa);
        s.reserved.deinit(gpa);
        s.calls.deinit(gpa);
        s.providers.deinit(gpa);
        gpa.destroy(s);
    }

    /// Stops accepting requests, closes every session, and waits for
    /// running requests to finish. Safe to call from several threads; each
    /// returns once the shutdown is complete. `recv` fails with
    /// `error.Closed` after that, once the queued messages are read.
    pub fn shutdown(s: *Server) void {
        const io = s.env.io;
        const gpa = s.env.gpa;
        var entries: std.ArrayList(*Entry) = .empty;
        defer entries.deinit(gpa);

        s.mutex.lockUncancelable(io);
        if (s.closed) {
            // Another caller is shutting down: wait until it is done, so
            // `destroy` never frees what it still uses.
            while (!s.shut_down) s.cond.waitUncancelable(io, &s.mutex);
            s.mutex.unlock(io);
            return;
        }
        s.closed = true;
        var calls = s.calls.valueIterator();
        while (calls.next()) |c| c.*.done.set(io);
        var it = s.sessions.valueIterator();
        while (it.next()) |e| {
            // A session left out here is closed when it is freed below.
            entries.append(gpa, e.*) catch break;
            e.*.refs += 1; // keeps it alive while it closes
        }
        s.cond.broadcast(io);
        s.mutex.unlock(io);

        for (entries.items) |e| {
            e.session.close();
            s.release(e);
        }

        s.tasks.join(io);
        // No request runs anymore, so nothing else holds these. Each is
        // freed without the lock: freeing may still send notifications.
        while (true) {
            s.mutex.lockUncancelable(io);
            var rest = s.sessions.valueIterator();
            const e = (rest.next() orelse {
                s.mutex.unlock(io);
                break;
            }).*;
            _ = s.sessions.remove(e.handle);
            s.mutex.unlock(io);
            e.destroy();
        }
        for (s.providers.keys(), s.providers.values()) |k, p| {
            p.deinit();
            gpa.free(k);
        }
        s.providers.clearRetainingCapacity();

        s.mutex.lockUncancelable(io);
        s.shut_down = true;
        s.cond.broadcast(io);
        s.mutex.unlock(io);
    }

    /// Waits up to `timeout` for the next message to the client, a
    /// NUL-terminated JSON text. The caller owns it (free with the server's
    /// allocator). Returns null on timeout, and `error.Closed` once the
    /// server shut down and every message was read.
    pub fn recv(s: *Server, timeout: Io.Timeout) error{Closed}!?[:0]u8 {
        const io = s.env.io;
        s.mutex.lockUncancelable(io);
        defer s.mutex.unlock(io);
        const deadline = timeout.toDeadline(io);
        while (true) {
            if (s.outbox.popFront()) |m| return m;
            if (s.shut_down) return error.Closed;
            s.cond.waitTimeout(io, &s.mutex, deadline) catch return null;
        }
    }

    /// `recv` that takes every queued message at once: the messages joined
    /// by newlines (JSON texts contain no raw newline), NUL-terminated.
    pub fn recvBatch(s: *Server, timeout: Io.Timeout) error{Closed}!?[:0]u8 {
        const first = try s.recv(timeout) orelse return null;
        const io = s.env.io;
        const gpa = s.env.gpa;
        // Take the queued messages under the lock and join them without it:
        // producers push to the outbox while holding their session's lock.
        var queued: std.Deque([:0]u8) = .empty;
        s.mutex.lockUncancelable(io);
        std.mem.swap(std.Deque([:0]u8), &queued, &s.outbox);
        s.mutex.unlock(io);
        defer queued.deinit(gpa);
        if (queued.len == 0) return first;
        var total = first.len;
        for (0..queued.len) |i| total += 1 + queued.at(i).len;
        const out = gpa.allocSentinel(u8, total, 0) catch {
            // Hand the messages back, ahead of anything pushed meanwhile.
            s.mutex.lockUncancelable(io);
            defer s.mutex.unlock(io);
            while (queued.popBack()) |m| s.outbox.pushFront(gpa, m) catch gpa.free(m);
            return first;
        };
        @memcpy(out[0..first.len], first);
        gpa.free(first);
        var at = first.len;
        while (queued.popFront()) |m| {
            out[at] = '\n';
            @memcpy(out[at + 1 ..][0..m.len], m);
            at += 1 + m.len;
            gpa.free(m);
        }
        return out;
    }

    /// Takes one message from the client. Requests are answered through
    /// `recv`.
    pub fn receive(s: *Server, bytes: []const u8) void {
        const gpa = s.env.gpa;
        const job = gpa.create(Job) catch return log.err("out of memory: message dropped", .{});
        job.* = .{ .server = s, .method = undefined, .arena = .init(gpa) };
        const a = job.arena.allocator();
        // Parsed into the job's memory, which the request keeps.
        const msg = json.JsonView.parseBytes(a, a.dupe(u8, bytes) catch "");
        const id: ?RawJson = msg.get("id").raw(a) catch null;
        const method_name = msg.str("method");
        if (msg.object() == null) {
            job.discard();
            return s.sendError(null, code.parse_error, .{ .kind = .invalid_params, .message = "invalid JSON message" });
        }
        if (method_name.len == 0) {
            if (id != null) s.answerCall(msg);
            return job.discard();
        }
        job.method = std.meta.stringToEnum(Method, method_name) orelse {
            if (id != null) s.sendError(id, code.method_not_found, .{ .kind = .unsupported, .message = method_name });
            return job.discard();
        };
        job.id = id;
        job.params = msg.get("params").v orelse .null;
        s.dispatch(job);
    }

    fn dispatch(s: *Server, job: *Job) void {
        const method = job.method;
        const needs_session = std.mem.startsWith(u8, @tagName(method), "session/") and method != .@"session/start" or method == .@"request/respond";
        s.mutex.lockUncancelable(s.env.io);
        if (s.closed) {
            s.mutex.unlock(s.env.io);
            job.finishError(code.library_error, .{ .kind = .closed, .message = "server closed" });
            return;
        }
        if (needs_session) {
            const handle = (json.JsonView{ .v = job.params }).str("session");
            const entry = s.sessions.get(handle) orelse {
                s.mutex.unlock(s.env.io);
                job.finishError(code.library_error, .{ .kind = .session_not_found, .message = "unknown session handle" });
                return;
            };
            entry.refs += 1;
            job.entry = entry;
        }
        // The scope closes only after `closed` is set.
        s.tasks.reserve(s.env.io) catch {
            s.mutex.unlock(s.env.io);
            job.finishError(code.library_error, .{ .kind = .closed, .message = "server closed" });
            return;
        };
        s.mutex.unlock(s.env.io);
        // Taken on the receiving thread, so ordered requests keep their
        // arrival order.
        if (job.entry) |e| if (method.ordered()) {
            job.ticket = e.takeTicket();
        };
        // Initialize is quick and everything else depends on it: run it
        // here, so requests sent right after it see its providers.
        if (method == .initialize) return runReserved(s, job);
        if (s.tasks.concurrent(s.env.io, Job.run, .{job})) |_| {
            // The task holds its own place in the scope.
            s.tasks.release(s.env.io);
        } else |_| {
            // Running it here could block receiving the very answer it
            // waits for: try a plain thread first.
            const thread = std.Thread.spawn(.{}, runReserved, .{ s, job }) catch return runReserved(s, job);
            thread.detach();
        }
    }

    /// Runs a request outside the task scope and ends its reservation.
    fn runReserved(s: *Server, job: *Job) void {
        job.run();
        s.tasks.release(s.env.io);
    }

    fn push(s: *Server, msg: [:0]u8) void {
        s.mutex.lockUncancelable(s.env.io);
        defer s.mutex.unlock(s.env.io);
        s.outbox.pushBack(s.env.gpa, msg) catch return s.env.gpa.free(msg);
        s.cond.broadcast(s.env.io);
    }

    /// Queues a message built by `body`, which writes the members after
    /// `"jsonrpc":"2.0"`.
    fn send(s: *Server, ctx: anytype, comptime body: fn (@TypeOf(ctx), *std.json.Stringify) std.json.Stringify.Error!void) void {
        // Most messages are events of a few hundred bytes.
        var out = std.Io.Writer.Allocating.initCapacity(s.env.gpa, 512) catch return log.err("out of memory: message dropped", .{});
        var jw: std.json.Stringify = .{ .writer = &out.writer };
        writeMessage(&jw, ctx, body) catch {
            out.deinit();
            return log.err("out of memory: message dropped", .{});
        };
        s.push(out.toOwnedSliceSentinel(0) catch return out.deinit());
    }

    fn writeMessage(jw: *std.json.Stringify, ctx: anytype, comptime body: fn (@TypeOf(ctx), *std.json.Stringify) std.json.Stringify.Error!void) std.json.Stringify.Error!void {
        try jw.beginObject();
        try jw.objectField("jsonrpc");
        try jw.write("2.0");
        try body(ctx, jw);
        try jw.endObject();
    }

    fn sendResult(s: *Server, id: ?RawJson, result: anytype) void {
        const Ctx = struct { id: ?RawJson, result: @TypeOf(result) };
        s.send(Ctx{ .id = id, .result = result }, struct {
            fn body(c: Ctx, jw: *std.json.Stringify) !void {
                try jw.objectField("id");
                try jw.write(c.id);
                try jw.objectField("result");
                try json.write(jw, c.result);
            }
        }.body);
    }

    fn sendError(s: *Server, id: ?RawJson, rpc_code: i64, info: types.ErrorInfo) void {
        const Ctx = struct { id: ?RawJson, code: i64, info: types.ErrorInfo };
        s.send(Ctx{ .id = id, .code = rpc_code, .info = info }, struct {
            fn body(c: Ctx, jw: *std.json.Stringify) !void {
                try jw.objectField("id");
                try jw.write(c.id);
                try jw.objectField("error");
                try jw.beginObject();
                try jw.objectField("code");
                try jw.write(c.code);
                try jw.objectField("message");
                try jw.write(if (c.info.message.len > 0) c.info.message else @tagName(c.info.kind));
                try jw.objectField("data");
                try json.write(jw, c.info);
                try jw.endObject();
            }
        }.body);
    }

    fn notify(s: *Server, method: []const u8, params: anytype) void {
        const Ctx = struct { method: []const u8, params: @TypeOf(params) };
        s.send(Ctx{ .method = method, .params = params }, struct {
            fn body(c: Ctx, jw: *std.json.Stringify) !void {
                try jw.objectField("method");
                try jw.write(c.method);
                try jw.objectField("params");
                try json.write(jw, c.params);
            }
        }.body);
    }

    // ------------------------------------------------------------ requests to the client

    const Call = struct {
        /// The session the call is for; `session/close` ends its calls.
        entry: ?*Entry,
        /// The tool call ID, for `tool/call`.
        tool_call_id: []const u8 = "",
        done: Io.Event = .unset,
        result: ?[]u8 = null,
        failed: bool = false,
    };

    /// Sends a request to the client and waits for the answer. Returns the
    /// result as JSON in `a`, or null when the client answered with an error
    /// or the server closed.
    fn callClient(s: *Server, a: Allocator, entry: ?*Entry, tool_call_id: []const u8, method: []const u8, params: anytype) ?[]const u8 {
        const io = s.env.io;
        var call: Call = .{ .entry = entry, .tool_call_id = tool_call_id };
        s.mutex.lockUncancelable(io);
        if (s.closed) {
            s.mutex.unlock(io);
            return null;
        }
        const call_id = s.next_call;
        s.next_call += 1;
        s.calls.put(s.env.gpa, call_id, &call) catch {
            s.mutex.unlock(io);
            return null;
        };
        s.mutex.unlock(io);

        const Ctx = struct { id: u64, method: []const u8, params: @TypeOf(params) };
        s.send(Ctx{ .id = call_id, .method = method, .params = params }, struct {
            fn body(c: Ctx, jw: *std.json.Stringify) !void {
                try jw.objectField("id");
                try jw.write(c.id);
                try jw.objectField("method");
                try jw.write(c.method);
                try jw.objectField("params");
                try json.write(jw, c.params);
            }
        }.body);
        call.done.waitUncancelable(io);

        s.mutex.lockUncancelable(io);
        _ = s.calls.remove(call_id);
        s.mutex.unlock(io);
        const result = call.result orelse return null;
        defer s.env.gpa.free(result);
        if (call.failed) return null;
        return a.dupe(u8, result) catch null;
    }

    fn answerCall(s: *Server, msg: json.JsonView) void {
        const call_id: u64 = @intCast(@max(0, msg.int("id")));
        const io = s.env.io;
        s.mutex.lockUncancelable(io);
        defer s.mutex.unlock(io);
        const call = s.calls.get(call_id) orelse return;
        if (call.done.isSet()) return;
        const failed = msg.has("error");
        const value = if (failed) msg.get("error") else msg.get("result");
        call.result = if (value.v) |v| std.json.Stringify.valueAlloc(s.env.gpa, v, .{}) catch null else s.env.gpa.dupe(u8, "null") catch null;
        call.failed = failed;
        call.done.set(io);
    }

    // ------------------------------------------------------------ sessions

    fn release(s: *Server, e: *Entry) void {
        s.mutex.lockUncancelable(s.env.io);
        e.refs -= 1;
        const last = e.refs == 0 and e.removed;
        s.mutex.unlock(s.env.io);
        if (last) e.destroy();
    }

    /// Where a new session comes from.
    const Origin = union(enum) { provider: agent.Provider, fork: *Session };

    fn register(s: *Server, opts: agent.StartOptions, origin: Origin, handle: []const u8, diag: ?*Diagnostics) agent.Error!*Entry {
        const gpa = s.env.gpa;
        if (handle.len > agent.Id.capacity) return errors.fail(diag, .invalid_params, "session handle is longer than {d} bytes", .{agent.Id.capacity});
        const e = try gpa.create(Entry);
        errdefer gpa.destroy(e);
        e.* = .{ .server = s, .session = undefined };
        {
            s.mutex.lockUncancelable(s.env.io);
            defer s.mutex.unlock(s.env.io);
            if (handle.len > 0) {
                if (s.handleTakenLocked(handle)) return errors.fail(diag, .invalid_params, "session handle \"{s}\" is in use", .{handle});
                e.handle_id = .of(handle);
            } else while (true) {
                var buf: [24]u8 = undefined;
                const name = std.fmt.bufPrint(&buf, "s{d}", .{s.next_session}) catch unreachable;
                s.next_session += 1;
                if (!s.handleTakenLocked(name)) {
                    e.handle_id = .of(name);
                    break;
                }
            }
            e.handle = e.handle_id.slice();
            // Reserved while the session starts, so no other start takes it.
            try s.reserved.put(gpa, e.handle, {});
        }
        defer {
            s.mutex.lockUncancelable(s.env.io);
            _ = s.reserved.remove(e.handle);
            s.mutex.unlock(s.env.io);
        }

        var o = opts;
        o.sink = .{ .context = e, .emit = Entry.onEvent, .closed = Entry.onClosed };
        o.tool_handler = .{ .context = e, .call = Entry.onToolCall, .cancel = Entry.onToolCancel };
        e.session = switch (origin) {
            .provider => |p| try p.start(o, diag),
            .fork => |from| try from.fork(o, diag),
        };

        s.mutex.lockUncancelable(s.env.io);
        const put = s.sessions.put(gpa, e.handle, e);
        s.mutex.unlock(s.env.io);
        put catch {
            e.session.deinit();
            return error.OutOfMemory;
        };
        return e;
    }

    fn handleTakenLocked(s: *Server, handle: []const u8) bool {
        return s.sessions.contains(handle) or s.reserved.contains(handle);
    }

    fn startResult(e: *Entry, a: Allocator) agent.Error!StartResult {
        return .{ .session = e.handle, .info = try e.session.info(a), .capabilities = e.session.capabilities };
    }

    fn provider(s: *Server, name: []const u8, diag: ?*Diagnostics) agent.Error!agent.Provider {
        s.mutex.lockUncancelable(s.env.io);
        defer s.mutex.unlock(s.env.io);
        if (!s.initialized) return errors.fail(diag, .invalid_params, "call initialize first", .{});
        return s.providers.get(name) orelse errors.fail(diag, .not_found, "provider \"{s}\" is not offered", .{name});
    }

    fn initialize(s: *Server, a: Allocator, p: InitializeParams, diag: ?*Diagnostics) agent.Error!InitializeResult {
        if (p.protocol_version != protocol_version)
            return errors.fail(diag, .invalid_params, "protocol version {d} is not supported (want {d})", .{ p.protocol_version, protocol_version });
        s.mutex.lockUncancelable(s.env.io);
        const already = s.initialized;
        s.mutex.unlock(s.env.io);
        if (already) return errors.fail(diag, .invalid_params, "already initialized", .{});

        var wanted: std.ArrayList(struct { []const u8, ?RawJson }) = .empty;
        if (p.providers) |m| {
            var it = m.map.iterator();
            while (it.next()) |kv| try wanted.append(a, .{ kv.key_ptr.*, kv.value_ptr.* });
        } else for (registry.default_names) |n| try wanted.append(a, .{ n, null });

        var created: std.StringArrayHashMapUnmanaged(agent.Provider) = .empty;
        errdefer {
            for (created.keys(), created.values()) |k, v| {
                v.deinit();
                s.env.gpa.free(k);
            }
            created.deinit(s.env.gpa);
        }
        for (wanted.items) |w| {
            const prov = try registry.create(s.env, w[0], w[1], diag);
            const key = s.env.gpa.dupe(u8, w[0]) catch |e| {
                prov.deinit();
                return e;
            };
            try created.put(s.env.gpa, key, prov);
        }

        s.mutex.lockUncancelable(s.env.io);
        s.providers = created;
        s.initialized = true;
        s.mutex.unlock(s.env.io);

        const infos = try a.alloc(ProviderInfo, created.count());
        for (created.values(), infos) |prov, *info| info.* = .{ .name = prov.name(), .capabilities = prov.capabilities() };
        return .{ .protocol_version = protocol_version, .library_version = library_version, .providers = infos };
    }
};

/// A session the server owns.
const Entry = struct {
    server: *Server,
    handle_id: agent.Id = .{},
    /// `handle_id`'s text.
    handle: []const u8 = "",
    session: *Session,
    /// Guarded by the server mutex.
    refs: u32 = 0,
    removed: bool = false,

    order_mutex: Io.Mutex = .init,
    order_cond: Io.Condition = .init,
    next_ticket: u64 = 0,
    serving: u64 = 0,

    fn takeTicket(e: *Entry) u64 {
        const io = e.server.env.io;
        e.order_mutex.lockUncancelable(io);
        defer e.order_mutex.unlock(io);
        const t = e.next_ticket;
        e.next_ticket += 1;
        return t;
    }

    fn waitTurn(e: *Entry, ticket: u64) void {
        const io = e.server.env.io;
        e.order_mutex.lockUncancelable(io);
        defer e.order_mutex.unlock(io);
        while (e.serving != ticket) e.order_cond.waitUncancelable(io, &e.order_mutex);
    }

    fn doneTurn(e: *Entry) void {
        const io = e.server.env.io;
        e.order_mutex.lockUncancelable(io);
        defer e.order_mutex.unlock(io);
        e.serving += 1;
        e.order_cond.broadcast(io);
    }

    fn destroy(e: *Entry) void {
        e.session.deinit();
        const s = e.server;
        s.mutex.lockUncancelable(s.env.io);
        _ = s.reserved.remove(e.handle);
        s.mutex.unlock(s.env.io);
        s.env.gpa.destroy(e);
    }

    fn onEvent(ctx: ?*anyopaque, ev: *const types.Event) void {
        const e: *Entry = @ptrCast(@alignCast(ctx.?));
        e.server.notify("session/event", EventNotification{ .session = e.handle, .event = ev.* });
    }

    fn onClosed(ctx: ?*anyopaque, err: ?types.ErrorInfo) void {
        const e: *Entry = @ptrCast(@alignCast(ctx.?));
        e.server.notify("session/closed", ClosedNotification{ .session = e.handle, .@"error" = err });
    }

    fn onToolCancel(ctx: ?*anyopaque, call_id: []const u8) void {
        const e: *Entry = @ptrCast(@alignCast(ctx.?));
        const s = e.server;
        s.mutex.lockUncancelable(s.env.io);
        var found = false;
        var calls = s.calls.valueIterator();
        while (calls.next()) |c| if (c.*.entry == e and std.mem.eql(u8, c.*.tool_call_id, call_id)) {
            c.*.done.set(s.env.io);
            found = true;
        };
        s.mutex.unlock(s.env.io);
        if (found) s.notify("tool/cancel", ToolCancelParams{ .session = e.handle, .call_id = call_id });
    }

    fn onToolCall(ctx: ?*anyopaque, arena: Allocator, call: types.ToolCallRequest) types.ToolResult {
        const e: *Entry = @ptrCast(@alignCast(ctx.?));
        const result = e.server.callClient(arena, e, call.id, "tool/call", ToolCallParams{ .session = e.handle, .call = call }) orelse
            return types.ToolResult.err(arena, "the tool call was cancelled or the host could not run it") catch .out_of_memory;
        return json.parse(types.ToolResult, arena, result) catch
            types.ToolResult.err(arena, "the host returned an invalid tool result") catch .out_of_memory;
    }
};

/// One request being handled.
const Job = struct {
    server: *Server,
    method: Method,
    arena: std.heap.ArenaAllocator,
    id: ?RawJson = null,
    params: std.json.Value = .null,
    entry: ?*Entry = null,
    ticket: ?u64 = null,
    diag: Diagnostics = .{},

    fn run(job: *Job) void {
        if (job.ticket) |t| job.entry.?.waitTurn(t);
        job.handle() catch |err| {
            const info = Diagnostics.of(&job.diag, err);
            job.finishError(if (info.kind == .invalid_params) code.invalid_params else code.library_error, info);
            return;
        };
        job.finish();
    }

    /// Frees a job that was never dispatched.
    fn discard(job: *Job) void {
        job.arena.deinit();
        job.server.env.gpa.destroy(job);
    }

    /// Releases the job's session and ordering ticket and frees the job.
    fn finish(job: *Job) void {
        const s = job.server;
        if (job.entry) |e| {
            if (job.ticket != null) e.doneTurn();
            s.release(e);
        }
        job.arena.deinit();
        s.env.gpa.destroy(job);
    }

    fn finishError(job: *Job, rpc_code: i64, info: types.ErrorInfo) void {
        if (job.id != null) job.server.sendError(job.id, rpc_code, info);
        job.finish();
    }

    fn reply(job: *Job, result: anytype) void {
        if (job.id != null) job.server.sendResult(job.id, result);
    }

    fn param(job: *Job, comptime T: type) agent.Error!T {
        return json.fromValue(T, job.arena.allocator(), job.params) catch |e|
            errors.fail(&job.diag, .invalid_params, "invalid params for {s}: {t}", .{ @tagName(job.method), e });
    }

    fn handle(job: *Job) agent.Error!void {
        const s = job.server;
        const a = job.arena.allocator();
        const diag = &job.diag;
        const sess = if (job.entry) |e| e.session else undefined;
        switch (job.method) {
            .initialize => job.reply(try s.initialize(a, try job.param(InitializeParams), diag)),
            .@"provider/check" => {
                const p = try job.param(ProviderParams);
                job.reply(try (try s.provider(p.provider, diag)).check(a, diag));
            },
            .@"provider/listModels" => {
                const p = try job.param(ProviderParams);
                job.reply(try (try s.provider(p.provider, diag)).listModels(a, diag));
            },
            .@"provider/listSessions" => {
                const p = try job.param(ListSessionsParams);
                job.reply(try (try s.provider(p.provider, diag)).listSessions(a, p.options, diag));
            },
            .@"provider/readHistory" => {
                const p = try job.param(ReadHistoryParams);
                job.reply(try (try s.provider(p.provider, diag)).readHistory(a, p.session_id, p.options, diag));
            },
            .@"provider/deleteSession" => {
                const p = try job.param(DeleteSessionParams);
                try (try s.provider(p.provider, diag)).deleteSession(p.session_id, p.cwd, diag);
                job.reply(no_result);
            },
            .@"provider/listSubagents" => {
                const p = try job.param(ListSubagentsParams);
                job.reply(try (try s.provider(p.provider, diag)).listSubagents(a, p.session_id, p.cwd, diag));
            },
            .@"provider/call" => {
                const p = try job.param(ProviderCallParams);
                job.reply(try (try s.provider(p.provider, diag)).call(p.method, p.params, a, diag));
            },
            .@"session/start" => {
                const p = try job.param(StartParams);
                const prov = try s.provider(p.provider, diag);
                const e = try s.register(.{ .config = p.config }, .{ .provider = prov }, p.handle, diag);
                job.reply(try Server.startResult(e, a));
            },
            .@"session/fork" => {
                const p = try job.param(ForkParams);
                const e = try s.register(.{ .config = p.config }, .{ .fork = sess }, p.handle, diag);
                job.reply(try Server.startResult(e, a));
            },
            .@"session/send" => {
                const p = try job.param(SendParams);
                const turn = if (p.turn_id.len > 0) try sess.sendAs(p.turn_id, p.input, diag) else try sess.send(p.input, diag);
                job.reply(SendResult{ .turn_id = turn.id.slice() });
            },
            .@"session/wait" => {
                const p = try job.param(WaitParams);
                const timeout: Io.Timeout = if (p.timeout_ms > 0) .{ .duration = .{ .raw = .fromMilliseconds(p.timeout_ms), .clock = .awake } } else .none;
                job.reply(try sess.wait(p.turn_id, a, timeout, diag));
            },
            .@"session/info" => job.reply(try sess.info(a)),
            .@"session/state" => job.reply(try sess.state(a)),
            .@"session/interrupt" => {
                try sess.interrupt(diag);
                job.reply(no_result);
            },
            .@"session/interruptTurn" => {
                try sess.interruptTurn((try job.param(TurnParams)).turn_id, diag);
                job.reply(no_result);
            },
            .@"session/setModel" => {
                try sess.setModel((try job.param(ValueParams)).value, diag);
                job.reply(no_result);
            },
            .@"session/setEffort" => {
                try sess.setEffort((try job.param(ValueParams)).value, diag);
                job.reply(no_result);
            },
            .@"session/setPermissionMode" => {
                try sess.setPermissionMode((try job.param(ValueParams)).value, diag);
                job.reply(no_result);
            },
            .@"session/setTitle" => {
                try sess.setTitle((try job.param(ValueParams)).value, diag);
                job.reply(no_result);
            },
            .@"session/compact" => {
                try sess.compact((try job.param(CompactParams)).instructions, diag);
                job.reply(no_result);
            },
            .@"session/rewind" => job.reply(try sess.rewind((try job.param(TurnParams)).turn_id, a, diag)),
            .@"session/stopTask" => {
                try sess.stopTask((try job.param(StopTaskParams)).task_id, diag);
                job.reply(no_result);
            },
            .@"session/rateLimits" => job.reply(try sess.rateLimits(a, diag)),
            .@"session/pendingRequests" => job.reply(try sess.pendingRequests(a)),
            .@"session/call" => {
                const p = try job.param(CallParams);
                job.reply(try sess.call(p.method, p.params, a, diag));
            },
            .@"session/close" => {
                const e = job.entry.?;
                s.mutex.lockUncancelable(s.env.io);
                const was_removed = e.removed;
                if (!was_removed) {
                    _ = s.sessions.remove(e.handle);
                    e.removed = true;
                    // Kept until the entry is freed (`Entry.destroy`).
                    s.reserved.put(s.env.gpa, e.handle, {}) catch log.warn("out of memory: handle {s} may be reused while it closes", .{e.handle});
                }
                // Tool calls still waiting for the client fail, so closing
                // does not wait on them.
                var calls = s.calls.valueIterator();
                while (calls.next()) |c| if (c.*.entry == e) c.*.done.set(s.env.io);
                s.mutex.unlock(s.env.io);
                if (!was_removed) sess.close();
                job.reply(no_result);
            },
            .@"request/respond" => {
                const p = try job.param(RespondParams);
                const resp: types.Response = if (p.permission) |r| .{ .permission = r } else if (p.user_input) |r| .{ .user_input = r } else if (p.elicitation) |r| .{ .elicitation = r } else if (p.dialog) |r| .{ .dialog = r } else if (p.native) |r| .{ .native = r } else return errors.fail(diag, .invalid_params, "request/respond needs permission, user_input, elicitation, dialog, or native", .{});
                try sess.respond(p.request_id, resp, diag);
                job.reply(no_result);
            },
        }
    }
};

/// The result of methods that return nothing.
const no_result: ?bool = null;

test "rpc: initialize, start, send, wait, events, tool call" {
    const gpa = std.testing.allocator;
    var threaded: Io.Threaded = .init(gpa, .{});
    defer threaded.deinit();
    const env: agent.Env = .{ .io = threaded.io(), .gpa = gpa, .environ = .empty };
    const s = try Server.create(env);
    defer s.destroy();

    const next = struct {
        fn next(srv: *Server, a: Allocator, method_or_id: []const u8) !json.JsonView {
            while (true) {
                const m = (try srv.recv(.none)).?;
                const msg = json.JsonView.parseBytes(a, try a.dupe(u8, m));
                srv.env.gpa.free(m);
                if (std.mem.eql(u8, msg.str("method"), method_or_id)) return msg;
                if (msg.get("id").v) |idv| if (idv == .integer and std.fmt.parseInt(i64, method_or_id, 10) catch -1 == idv.integer and msg.str("method").len == 0) return msg;
            }
        }
    }.next;
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();

    s.receive(
        \\{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"providers":{"fake":null}}}
    );
    const init_res = try next(s, a, "1");
    try std.testing.expectEqualStrings("fake", init_res.get("result").get("providers").index(0).str("name"));

    s.receive(
        \\{"jsonrpc":"2.0","id":2,"method":"session/start","params":{"provider":"fake","config":{"tools":[{"name":"lookup"}]}}}
    );
    const handle = (try next(s, a, "2")).get("result").str("session");
    try std.testing.expectEqualStrings("s1", handle);

    s.receive(
        \\{"jsonrpc":"2.0","id":3,"method":"session/send","params":{"session":"s1","input":{"text":"tool:lookup"}}}
    );
    // The send result and the tool call arrive in either order.
    var turn_id: []const u8 = "";
    var answered = false;
    while (turn_id.len == 0 or !answered) {
        const m = (try s.recv(.none)).?;
        const msg = json.JsonView.parseBytes(a, try a.dupe(u8, m));
        gpa.free(m);
        if (std.mem.eql(u8, msg.str("method"), "tool/call")) {
            try std.testing.expectEqualStrings("lookup", msg.get("params").get("call").str("name"));
            s.receive(try std.fmt.allocPrint(a,
                \\{{"jsonrpc":"2.0","id":{d},"result":{{"content":[{{"text":"ticket 42"}}]}}}}
            , .{msg.int("id")}));
            answered = true;
        } else if (msg.int("id") == 3) turn_id = msg.get("result").str("turn_id");
    }

    const wait = try std.fmt.allocPrint(a,
        \\{{"jsonrpc":"2.0","id":4,"method":"session/wait","params":{{"session":"s1","turn_id":"{s}"}}}}
    , .{turn_id});
    s.receive(wait);
    const res = try next(s, a, "4");
    try std.testing.expectEqualStrings("ticket 42", res.get("result").str("text"));

    s.receive(
        \\{"jsonrpc":"2.0","id":5,"method":"session/close","params":{"session":"s1"}}
    );
    _ = try next(s, a, "session/closed");
}

test "rpc: interrupting cancels a host tool call the client has not answered" {
    const gpa = std.testing.allocator;
    var threaded: Io.Threaded = .init(gpa, .{});
    defer threaded.deinit();
    const env: agent.Env = .{ .io = threaded.io(), .gpa = gpa, .environ = .empty };
    const s = try Server.create(env);
    defer s.destroy();
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();

    s.receive(
        \\{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"providers":{"fake":null}}}
    );
    s.receive(
        \\{"jsonrpc":"2.0","id":2,"method":"session/start","params":{"provider":"fake"}}
    );
    // The handle exists once session/start has answered.
    while (true) {
        const m = (try s.recv(.none)).?;
        defer gpa.free(m);
        if (std.mem.indexOf(u8, m, "\"id\":2,") != null) break;
    }
    s.receive(
        \\{"jsonrpc":"2.0","id":3,"method":"session/send","params":{"session":"s1","input":{"text":"tool:slow"}}}
    );
    var seen_cancel = false;
    var completed = false;
    while (!(seen_cancel and completed)) {
        const m = (try s.recv(.none)).?;
        const msg = json.JsonView.parseBytes(a, try a.dupe(u8, m));
        gpa.free(m);
        const method = msg.str("method");
        // The client never answers the tool call; it interrupts instead.
        if (std.mem.eql(u8, method, "tool/call")) s.receive(
            \\{"jsonrpc":"2.0","id":4,"method":"session/interrupt","params":{"session":"s1"}}
        );
        if (std.mem.eql(u8, method, "tool/cancel")) seen_cancel = true;
        if (std.mem.eql(u8, method, "session/event") and std.mem.eql(u8, msg.get("params").get("event").str("type"), "turn_completed")) completed = true;
    }
}

test "rpc: the client chooses session handles and turn IDs" {
    const gpa = std.testing.allocator;
    var threaded: Io.Threaded = .init(gpa, .{});
    defer threaded.deinit();
    const env: agent.Env = .{ .io = threaded.io(), .gpa = gpa, .environ = .empty };
    const s = try Server.create(env);
    defer s.destroy();
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();

    const reply = struct {
        fn reply(srv: *Server, al: Allocator, id: i64) !json.JsonView {
            while (true) {
                const m = (try srv.recv(.none)).?;
                const msg = json.JsonView.parseBytes(al, try al.dupe(u8, m));
                srv.env.gpa.free(m);
                if (msg.str("method").len == 0 and msg.int("id") == id) return msg;
            }
        }
    }.reply;

    s.receive(
        \\{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"providers":{"fake":null}}}
    );
    _ = try reply(s, a, 1);
    s.receive(
        \\{"jsonrpc":"2.0","id":2,"method":"session/start","params":{"provider":"fake","handle":"mine"}}
    );
    try std.testing.expectEqualStrings("mine", (try reply(s, a, 2)).get("result").str("session"));
    s.receive(
        \\{"jsonrpc":"2.0","id":3,"method":"session/start","params":{"provider":"fake","handle":"mine"}}
    );
    try std.testing.expectEqualStrings("invalid_params", (try reply(s, a, 3)).get("error").get("data").str("kind"));

    s.receive(
        \\{"jsonrpc":"2.0","id":4,"method":"session/send","params":{"session":"mine","turn_id":"t-1","input":{"text":"hi"}}}
    );
    try std.testing.expectEqualStrings("t-1", (try reply(s, a, 4)).get("result").str("turn_id"));
    s.receive(
        \\{"jsonrpc":"2.0","id":5,"method":"session/send","params":{"session":"mine","turn_id":"t-1","input":{"text":"again"}}}
    );
    try std.testing.expectEqualStrings("invalid_params", (try reply(s, a, 5)).get("error").get("data").str("kind"));
}

test "rpc: a closing session's handle stays taken until it is freed" {
    const gpa = std.testing.allocator;
    var threaded: Io.Threaded = .init(gpa, .{});
    defer threaded.deinit();
    const env: agent.Env = .{ .io = threaded.io(), .gpa = gpa, .environ = .empty };
    const s = try Server.create(env);
    defer s.destroy();
    s.receive(
        \\{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"providers":{"fake":null}}}
    );
    const handle_taken = struct {
        fn taken(srv: *Server, handle: []const u8) bool {
            srv.mutex.lockUncancelable(srv.env.io);
            defer srv.mutex.unlock(srv.env.io);
            return srv.handleTakenLocked(handle);
        }
    }.taken;
    var diag: errors.Diagnostics = .{};
    const prov = try s.provider("fake", &diag);
    const e = try s.register(.{}, .{ .provider = prov }, "h", &diag);
    // As `session/close` does: out of the table, but not yet freed.
    s.mutex.lockUncancelable(env.io);
    _ = s.sessions.remove(e.handle);
    e.removed = true;
    try s.reserved.put(gpa, e.handle, {});
    s.mutex.unlock(env.io);
    try std.testing.expect(handle_taken(s, "h"));
    e.destroy();
    try std.testing.expect(!handle_taken(s, "h"));
}

/// A server with the fake provider and one session under `handle`, for
/// tests.
const TestServer = struct {
    threaded: Io.Threaded,
    server: *Server,
    arena: std.heap.ArenaAllocator,

    const wait_limit: Io.Timeout = .{ .duration = .{ .raw = .fromSeconds(10), .clock = .awake } };

    fn start(t: *TestServer, handle: []const u8) !void {
        const gpa = std.testing.allocator;
        t.threaded = .init(gpa, .{});
        t.arena = .init(gpa);
        t.server = try Server.create(.{ .io = t.threaded.io(), .gpa = gpa, .environ = .empty });
        t.server.receive(
            \\{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"providers":{"fake":null}}}
        );
        _ = try t.reply(1);
        t.send("{{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"session/start\",\"params\":{{\"provider\":\"fake\",\"handle\":\"{s}\"}}}}", .{handle});
        _ = try t.reply(2);
    }

    fn deinit(t: *TestServer) void {
        t.server.destroy();
        t.arena.deinit();
        t.threaded.deinit();
    }

    fn send(t: *TestServer, comptime fmt: []const u8, args: anytype) void {
        t.server.receive(std.fmt.allocPrint(t.arena.allocator(), fmt, args) catch @panic("out of memory"));
    }

    /// The next message, failing the test when none arrives in time.
    fn next(t: *TestServer) !json.JsonView {
        const m = (try t.server.recv(wait_limit)) orelse return error.TestTimedOut;
        defer t.server.env.gpa.free(m);
        const a = t.arena.allocator();
        return json.JsonView.parseBytes(a, try a.dupe(u8, m));
    }

    /// Reads until the answer to request `id`.
    fn reply(t: *TestServer, id: i64) !json.JsonView {
        while (true) {
            const msg = try t.next();
            if (msg.str("method").len == 0 and msg.int("id") == id) return msg;
        }
    }

    /// Reads until a session event of type `event_type`.
    fn event(t: *TestServer, event_type: []const u8) !json.JsonView {
        while (true) {
            const msg = try t.next();
            if (!std.mem.eql(u8, msg.str("method"), "session/event")) continue;
            const ev = msg.get("params").get("event");
            if (std.mem.eql(u8, ev.str("type"), event_type)) return ev;
        }
    }
};

test "rpc: session/call runs while a compaction blocks the session" {
    var t: TestServer = undefined;
    try t.start("c");
    defer t.deinit();
    t.send(
        \\{{"jsonrpc":"2.0","id":3,"method":"session/compact","params":{{"session":"c"}}}}
    , .{});
    _ = try t.event("compaction");
    t.send(
        \\{{"jsonrpc":"2.0","id":4,"method":"session/call","params":{{"session":"c","method":"echo","params":{{"x":1}}}}}}
    , .{});
    // The call is answered while the compaction still waits.
    while (true) {
        const msg = try t.next();
        if (msg.str("method").len > 0) continue;
        try std.testing.expectEqual(@as(i64, 4), msg.int("id"));
        try std.testing.expectEqual(@as(i64, 1), msg.get("result").int("x"));
        break;
    }
    t.send(
        \\{{"jsonrpc":"2.0","id":5,"method":"session/call","params":{{"session":"c","method":"finish_compaction"}}}}
    , .{});
    var compacted = false;
    var finished = false;
    while (!(compacted and finished)) {
        const msg = try t.next();
        if (msg.str("method").len > 0) continue;
        try std.testing.expect(!msg.has("error"));
        if (msg.int("id") == 3) compacted = true;
        if (msg.int("id") == 5) finished = true;
    }
}

test "rpc: dialog and native requests are answered with request/respond" {
    var t: TestServer = undefined;
    try t.start("d");
    defer t.deinit();

    t.send(
        \\{{"jsonrpc":"2.0","id":3,"method":"session/send","params":{{"session":"d","turn_id":"t-1","input":{{"text":"dialog"}}}}}}
    , .{});
    const dialog = try t.event("dialog_request");
    try std.testing.expectEqualStrings("fake_dialog", dialog.str("kind"));
    // An answer of another kind is refused and leaves the request pending.
    t.send(
        \\{{"jsonrpc":"2.0","id":4,"method":"request/respond","params":{{"session":"d","request_id":"{s}","native":{{}}}}}}
    , .{dialog.str("id")});
    try std.testing.expectEqualStrings("invalid_answer", (try t.reply(4)).get("error").get("data").str("kind"));
    t.send(
        \\{{"jsonrpc":"2.0","id":5,"method":"request/respond","params":{{"session":"d","request_id":"{s}","dialog":{{"behavior":"completed","result":{{"ok":true}}}}}}}}
    , .{dialog.str("id")});
    const resolved = try t.event("request_resolved");
    try std.testing.expectEqualStrings("dialog", resolved.str("kind"));
    try std.testing.expectEqualStrings("answered", resolved.str("outcome"));
    t.send(
        \\{{"jsonrpc":"2.0","id":6,"method":"session/wait","params":{{"session":"d","turn_id":"t-1"}}}}
    , .{});
    try std.testing.expectEqualStrings("dialog completed {\"ok\":true}", (try t.reply(6)).get("result").str("text"));

    t.send(
        \\{{"jsonrpc":"2.0","id":7,"method":"session/send","params":{{"session":"d","turn_id":"t-2","input":{{"text":"native"}}}}}}
    , .{});
    const native = try t.event("native_request");
    try std.testing.expectEqualStrings("fake/ping", native.str("method"));
    t.send(
        \\{{"jsonrpc":"2.0","id":8,"method":"request/respond","params":{{"session":"d","request_id":"{s}","native":{{"error":{{"code":-32001,"message":"no"}}}}}}}}
    , .{native.str("id")});
    t.send(
        \\{{"jsonrpc":"2.0","id":9,"method":"session/wait","params":{{"session":"d","turn_id":"t-2"}}}}
    , .{});
    try std.testing.expectEqualStrings("native error -32001 no", (try t.reply(9)).get("result").str("text"));
}

test "rpc: provider/call, session/rewind, and session/state" {
    var t: TestServer = undefined;
    try t.start("r");
    defer t.deinit();

    t.send(
        \\{{"jsonrpc":"2.0","id":3,"method":"provider/call","params":{{"provider":"fake","method":"account/read"}}}}
    , .{});
    try std.testing.expectEqualStrings("unsupported", (try t.reply(3)).get("error").get("data").str("kind"));

    t.send(
        \\{{"jsonrpc":"2.0","id":4,"method":"session/state","params":{{"session":"r"}}}}
    , .{});
    const before = (try t.reply(4)).get("result");
    const first_id = before.get("info").str("id");
    try std.testing.expect(first_id.len > 0);

    t.send(
        \\{{"jsonrpc":"2.0","id":5,"method":"session/send","params":{{"session":"r","turn_id":"t-1","input":{{"text":"hello"}}}}}}
    , .{});
    t.send(
        \\{{"jsonrpc":"2.0","id":6,"method":"session/wait","params":{{"session":"r","turn_id":"t-1"}}}}
    , .{});
    _ = try t.reply(6);
    t.send(
        \\{{"jsonrpc":"2.0","id":7,"method":"session/rewind","params":{{"session":"r","turn_id":"t-1"}}}}
    , .{});
    const changed = try t.event("session_id_changed");
    try std.testing.expectEqualStrings(first_id, changed.str("previous_id"));
    try std.testing.expectEqualStrings("rewind", changed.str("reason"));
    const rewound = (try t.reply(7)).get("result");
    try std.testing.expectEqualStrings(changed.str("id"), rewound.str("session_id"));
    try std.testing.expectEqualStrings("msg-t-1", rewound.get("removed_item_ids").index(0).string());

    t.send(
        \\{{"jsonrpc":"2.0","id":8,"method":"session/state","params":{{"session":"r"}}}}
    , .{});
    const after = (try t.reply(8)).get("result");
    try std.testing.expectEqualStrings(changed.str("id"), after.get("info").str("id"));
}
