//! A connection to one `codex app-server` process: JSON-RPC calls, replies
//! to server requests, and routing of thread messages to sessions.
const std = @import("std");
const agent = @import("agent");
const decode = @import("decode.zig");
const Options = @import("options.zig").Options;
const CodexSession = @import("session.zig").CodexSession;
const translate = @import("translate.zig");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const errors = agent.errors;
const types = agent.types;
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const Id = agent.Id;
const Diagnostics = agent.Diagnostics;
const ObjectBuilder = agent.json.ObjectBuilder;
const providerkit = agent.providerkit;
const IdMap = providerkit.IdMap;
const Value = std.json.Value;
const name = @import("root.zig").name;
const log = std.log.scoped(.agent);

/// Notifications for threads that are not registered yet, such as the
/// warnings Codex sends while thread/start runs, are kept within these
/// bounds until the thread registers.
const max_orphans = 256;
const max_orphan_threads = 32;

/// Names the client to Codex before initialize runs.
const originator_env = "CODEX_INTERNAL_ORIGINATOR_OVERRIDE";
/// The client name Codex reserves for hosts that must not become the
/// originator of model requests.
const daemon_client_name = "codex_app_server_daemon";

/// Runs on the reader task right after a call's reply is read, before
/// any later message is handled. It must not block.
pub const Hook = struct {
    context: *anyopaque,
    run: *const fn (context: *anyopaque, failed: bool) void,
};

pub const CallOptions = struct {
    /// Zero waits until the reply arrives or the app-server exits.
    timeout_ms: i64 = 0,
    /// The session the call belongs to; `cancelOwner` fails its calls.
    owner: ?*const anyopaque = null,
    hook: ?Hook = null,
};

const Call = struct {
    done: Io.Event = .unset,
    owner: ?*const anyopaque,
    hook: ?Hook,
    /// The raw result, owned by the client allocator.
    result: ?[]u8 = null,
    rpc_code: i64 = 0,
    rpc_message: ?[]u8 = null,
    rpc_data: ?[]u8 = null,
    closed: bool = false,
};

/// Per-process settings for `start`.
pub const StartConfig = struct {
    cwd: []const u8 = "",
    env: ?types.StringMap = null,
    unset_env: []const []const u8 = &.{},
    /// Replaces `Options.command_prefix` when not empty.
    command_prefix: []const []const u8 = &.{},
    /// Receives every protocol line as wire events.
    tap: ?*agent.runtime.Runtime = null,
};

pub const Client = struct {
    env: agent.Env,
    /// Holds the options copy and the initialize answer.
    arena: std.heap.ArenaAllocator,
    opts: Options,
    proc: *agent.process.Process = undefined,
    user_agent: []const u8 = "",
    tap: ?*agent.runtime.Runtime,
    refs: std.atomic.Value(u32) = .init(1),
    next_id: std.atomic.Value(i64) = .init(0),
    closing: std.atomic.Value(bool) = .init(false),

    mutex: Io.Mutex = .init,
    calls: std.AutoArrayHashMapUnmanaged(i64, *Call) = .empty,
    /// Sessions whose calls fail (see `cancelOwner`).
    cancelled_owners: std.ArrayList(*const anyopaque) = .empty,
    threads: IdMap(*CodexSession) = .empty,
    /// Subagent threads, routed to the session that spawned them.
    children: IdMap(*CodexSession) = .empty,
    orphans: IdMap(std.ArrayList([]u8)) = .empty,
    /// The app-server exited; a session registered later ends at once.
    ended: bool = false,
    end_arena: std.heap.ArenaAllocator,
    /// Scratch for the line the reader is handling; reset after each line.
    line_arena: std.heap.ArenaAllocator,
    end_error: ?types.ErrorInfo = null,
    done: Io.Event = .unset,
    /// Held by the reader while it hands a message to a session, so a
    /// closing session can wait until no delivery to it is in flight.
    dispatch: Io.Mutex = .init,

    /// Starts an app-server and completes the initialize handshake.
    pub fn start(env: agent.Env, opts: Options, config: StartConfig, diag: ?*Diagnostics) errors.Error!*Client {
        const gpa = env.gpa;
        const client = try gpa.create(Client);
        client.* = .{ .env = env, .arena = .init(gpa), .opts = undefined, .tap = config.tap, .end_arena = .init(gpa), .line_arena = .init(gpa) };
        errdefer {
            client.arena.deinit();
            client.end_arena.deinit();
            client.line_arena.deinit();
            gpa.destroy(client);
        }
        client.opts = try agent.clone(Options, client.arena.allocator(), opts);

        var scratch: std.heap.ArenaAllocator = .init(gpa);
        defer scratch.deinit();
        const a = scratch.allocator();

        var proc_env: types.StringMap = .{};
        for ([_]?types.StringMap{ opts.env, config.env }) |layer| if (layer) |extra| {
            for (extra.map.keys(), extra.map.values()) |key, value| try proc_env.map.put(a, key, value);
        };
        if (!proc_env.map.contains(originator_env) and opts.client_name.len > 0 and !std.mem.eql(u8, opts.client_name, daemon_client_name))
            try proc_env.map.put(a, originator_env, opts.client_name);
        var unset: std.ArrayList([]const u8) = .empty;
        try unset.appendSlice(a, opts.unset_env);
        try unset.appendSlice(a, config.unset_env);

        client.proc = try agent.process.Process.start(env, .{
            .provider = name,
            .argv = try appServerArgs(a, opts),
            .command_prefix = if (config.command_prefix.len > 0) config.command_prefix else opts.command_prefix,
            .cwd = config.cwd,
            .env = proc_env,
            .unset_env = unset.items,
            .tap = .{ .context = client, .line = tapLine },
        }, .{ .context = client, .on_line = onLine, .on_exit = onExit }, diag);
        errdefer {
            client.proc.stop(0);
            client.proc.destroy();
        }

        var initialize_diag: Diagnostics = .{};
        const result = client.initialize(a, &initialize_diag) catch |e| {
            const info = Diagnostics.of(&initialize_diag, e);
            client.close();
            if (client.proc.hasExited()) {
                const exit = client.proc.exitInfo(a) catch info;
                return errors.failWith(diag, .{ .kind = .exit, .message = try std.fmt.allocPrint(a, "codex: initialize: {s}", .{exit.message}), .stderr = exit.stderr });
            }
            var wrapped = info;
            wrapped.message = try std.fmt.allocPrint(a, "codex: initialize: {s}", .{info.message});
            return errors.failWith(diag, wrapped);
        };
        client.user_agent = try client.arena.allocator().dupe(u8, result.str("userAgent"));
        return client;
    }

    fn initialize(client: *Client, a: Allocator, diag: ?*Diagnostics) errors.Error!JsonView {
        const opts = client.opts;
        var capabilities: ObjectBuilder = .init(a);
        try capabilities.put("experimentalApi", .{ .bool = !opts.disable_experimental_api });
        try capabilities.put("requestAttestation", .{ .bool = false });
        if (opts.opt_out_notifications.len > 0) try capabilities.put("optOutNotificationMethods", try agent.json.stringArray(a, opts.opt_out_notifications));
        var extensions: ObjectBuilder = .init(a);
        try extensions.put("openai/form", .{ .object = .empty });
        if (agent.json.valueOf(a, opts.initialize_capabilities)) |extra| if (extra == .object) {
            for (extra.object.keys(), extra.object.values()) |key, value| {
                if (std.mem.eql(u8, key, "extensions") and value == .object) {
                    try extensions.putAll(value.object);
                    continue;
                }
                try capabilities.put(key, value);
            }
        };
        if (!capabilities.has("extensions")) {
            var kept: ObjectBuilder = .init(a);
            for (extensions.map.keys(), extensions.map.values()) |key, value| if (value != .null) try kept.put(key, value);
            try capabilities.put("extensions", kept.value());
        }
        var info: ObjectBuilder = .init(a);
        try info.str("name", agent.providerkit.firstOf(&.{ opts.client_name, "agent-cli-connector" }));
        try info.put("title", if (opts.client_title.len > 0) .{ .string = opts.client_title } else .null);
        try info.str("version", agent.providerkit.firstOf(&.{ opts.client_version, "0.1.0" }));
        var params: ObjectBuilder = .init(a);
        try params.put("clientInfo", info.value());
        try params.put("capabilities", capabilities.value());
        const timeout = providerkit.orDefault(opts.startup_timeout_ms, providerkit.default_startup_timeout_ms);
        const result = try client.callJson(a, "initialize", params.value(), .{ .timeout_ms = timeout }, diag);
        try client.notifyServer(a, "initialized", diag);
        return result;
    }

    /// The Codex version from the initialize user agent, such as "0.163.0"
    /// in "codex_cli_rs/0.163.0 (Linux)", or "".
    pub fn version(client: *Client) []const u8 {
        const slash = std.mem.indexOfScalar(u8, client.user_agent, '/') orelse return "";
        const rest = client.user_agent[slash + 1 ..];
        return agent.providerkit.parseVersion(rest[0 .. std.mem.indexOfScalar(u8, rest, ' ') orelse rest.len]);
    }

    pub fn retain(client: *Client) void {
        _ = client.refs.fetchAdd(1, .monotonic);
    }

    /// Drops a reference; the last one stops the process and frees the
    /// client.
    pub fn release(client: *Client) void {
        if (client.refs.fetchSub(1, .acq_rel) != 1) return;
        client.close();
        client.proc.destroy();
        const gpa = client.env.gpa;
        for (client.orphans.values()) |*list| freeLines(gpa, list);
        client.orphans.deinit(gpa);
        client.threads.deinit(gpa);
        client.children.deinit(gpa);
        client.calls.deinit(gpa);
        client.cancelled_owners.deinit(gpa);
        client.end_arena.deinit();
        client.line_arena.deinit();
        client.arena.deinit();
        gpa.destroy(client);
    }

    /// Stops the app-server, ending every session on it. Returns after it
    /// exited.
    pub fn close(client: *Client) void {
        if (client.closing.swap(true, .acq_rel)) {
            while (!client.done.isSet()) client.done.waitUncancelable(client.env.io);
            return;
        }
        client.proc.stop(client.shutdownGraceMs());
        while (!client.done.isSet()) client.done.waitUncancelable(client.env.io);
    }

    pub fn shutdownGraceMs(client: *Client) i64 {
        return providerkit.orDefault(client.opts.shutdown_grace_ms, providerkit.default_shutdown_grace_ms);
    }

    pub fn hasEnded(client: *Client) bool {
        return client.done.isSet();
    }

    // ------------------------------------------------------------ calls

    /// Sends a request and returns the raw result, allocated in `a`. A
    /// JSON-RPC error answer fails with `protocol`; its message, code, and
    /// data are in `diag`.
    pub fn call(client: *Client, a: Allocator, method: []const u8, params: ?Value, opts: CallOptions, diag: ?*Diagnostics) errors.Error!RawJson {
        const encoded: ?[]const u8 = if (params) |value| try agent.json.encodeValue(a, value) else null;
        return client.callRaw(a, method, encoded, opts, diag);
    }

    /// `call` with the result parsed.
    pub fn callJson(client: *Client, a: Allocator, method: []const u8, params: ?Value, opts: CallOptions, diag: ?*Diagnostics) errors.Error!JsonView {
        const result = try client.call(a, method, params, opts, diag);
        return JsonView.parseBytes(a, result.bytes);
    }

    /// Sends a request whose params are already encoded.
    pub fn callRaw(client: *Client, a: Allocator, method: []const u8, params: ?[]const u8, opts: CallOptions, diag: ?*Diagnostics) errors.Error!RawJson {
        const gpa = client.env.gpa;
        const io = client.env.io;
        const id = client.next_id.fetchAdd(1, .monotonic) + 1;
        const slot = try gpa.create(Call);
        defer destroyCall(gpa, slot);
        slot.* = .{ .owner = opts.owner, .hook = opts.hook };
        {
            client.mutex.lockUncancelable(io);
            defer client.mutex.unlock(io);
            if (client.ended) return client.endedError(diag);
            if (opts.owner) |owner| if (std.mem.indexOfScalar(*const anyopaque, client.cancelled_owners.items, owner) != null)
                return client.endedError(diag);
            try client.calls.put(gpa, id, slot);
        }
        const method_json = try agent.json.quoteString(a, method);
        const line = if (params) |encoded|
            try std.fmt.allocPrint(a, "{{\"id\":{d},\"method\":{s},\"params\":{s}}}", .{ id, method_json, encoded })
        else
            try std.fmt.allocPrint(a, "{{\"id\":{d},\"method\":{s}}}", .{ id, method_json });
        client.proc.writeLine(line, diag) catch |e| {
            _ = client.forgetCall(id);
            return e;
        };

        if (opts.timeout_ms > 0 and !providerkit.waitEvent(io, &slot.done, opts.timeout_ms) and client.forgetCall(id))
            return errors.fail(diag, .timeout, "codex: {s}: no reply within {d} ms", .{ method, opts.timeout_ms });
        // Also reached when the reader took the call as the timeout fired:
        // its reply is on the way.
        slot.done.waitUncancelable(io);
        if (slot.closed) {
            client.mutex.lockUncancelable(io);
            defer client.mutex.unlock(io);
            return client.endedError(diag);
        }
        if (slot.rpc_message) |message| {
            const data = JsonView.parseBytes(a, slot.rpc_data orelse "");
            return errors.failWith(diag, .{
                .kind = .protocol,
                .message = message,
                .rpc_code = slot.rpc_code,
                .code = decode.errorCode(a, data.get("codexErrorInfo")),
                .data = if (slot.rpc_data) |bytes| RawJson.of(bytes) else null,
            });
        }
        return .{ .bytes = try a.dupe(u8, slot.result orelse "null") };
    }

    /// Removes a call that was not answered; false when the reader took it
    /// meanwhile and its answer is about to be read.
    fn forgetCall(client: *Client, id: i64) bool {
        client.mutex.lockUncancelable(client.env.io);
        defer client.mutex.unlock(client.env.io);
        return client.calls.swapRemove(id);
    }

    fn endedError(client: *Client, diag: ?*Diagnostics) errors.Error {
        if (client.end_error) |info| return errors.failWith(diag, info);
        return errors.fail(diag, .closed, "codex: the app-server was closed", .{});
    }

    /// Fails the calls `owner` is waiting for, and those it makes until
    /// `unregisterSession`, so a closing session does not wait for answers.
    pub fn cancelOwner(client: *Client, owner: *const anyopaque) void {
        client.mutex.lockUncancelable(client.env.io);
        defer client.mutex.unlock(client.env.io);
        if (std.mem.indexOfScalar(*const anyopaque, client.cancelled_owners.items, owner) == null) {
            // Without the record, a later call waits until the app-server
            // answers it.
            client.cancelled_owners.append(client.env.gpa, owner) catch log.err("codex: out of memory: calls of a closing session are not refused", .{});
        }
        var i: usize = 0;
        while (i < client.calls.count()) {
            const slot = client.calls.values()[i];
            if (slot.owner == owner) {
                slot.closed = true;
                client.calls.swapRemoveAt(i);
                slot.done.set(client.env.io);
            } else i += 1;
        }
    }

    pub fn notifyServer(client: *Client, a: Allocator, method: []const u8, diag: ?*Diagnostics) errors.Error!void {
        const line = try std.fmt.allocPrint(a, "{{\"method\":{s}}}", .{try agent.json.quoteString(a, method)});
        return client.proc.writeLine(line, diag);
    }

    /// Answers server request `id` (its raw JSON) with `result`; null sends
    /// an empty object.
    pub fn reply(client: *Client, a: Allocator, id: []const u8, result: ?Value, diag: ?*Diagnostics) errors.Error!void {
        const body = if (result) |value| try agent.json.encodeValue(a, value) else "{}";
        return client.proc.writeLine(try std.fmt.allocPrint(a, "{{\"id\":{s},\"result\":{s}}}", .{ id, body }), diag);
    }

    pub fn replyError(client: *Client, a: Allocator, id: []const u8, code: i64, message: []const u8, diag: ?*Diagnostics) errors.Error!void {
        const encoded = try agent.json.quoteString(a, message);
        return client.proc.writeLine(try std.fmt.allocPrint(a, "{{\"error\":{{\"code\":{d},\"message\":{s}}},\"id\":{s}}}", .{ code, encoded, id }), diag);
    }

    // ------------------------------------------------------------ routing

    pub fn register(client: *Client, codex: *CodexSession, thread: []const u8) void {
        client.publish(thread, codex, false);
    }

    /// Replays a thread's held notifications to `codex`, then routes new ones
    /// to it. Until the held ones are drained, new ones keep queueing
    /// behind them, so `codex` sees them in arrival order. On an app-server
    /// that already exited, `codex` ends with its error instead.
    fn publish(client: *Client, thread: []const u8, codex: *CodexSession, child: bool) void {
        const io = client.env.io;
        const gpa = client.env.gpa;
        const key: Id = .of(thread);
        while (true) {
            client.mutex.lockUncancelable(io);
            var held = if (client.orphans.fetchOrderedRemove(key)) |kv| kv.value else std.ArrayList([]u8).empty;
            if (held.items.len == 0) {
                held.deinit(gpa);
                if (client.ended) {
                    const err = client.end_error;
                    client.mutex.unlock(io);
                    codex.endFromClient(err);
                    return;
                }
                const map = if (child) &client.children else &client.threads;
                const routed = if (map.put(gpa, key, codex)) true else |_| false;
                client.mutex.unlock(io);
                if (routed) return;
                log.err("codex: cannot route thread {s}: out of memory", .{thread});
                // A session whose thread is not routed would never hear
                // from it again.
                if (!child) codex.endFromClient(.{ .kind = .internal, .message = "codex: cannot route the thread's messages: out of memory" });
                return;
            }
            client.mutex.unlock(io);
            defer freeLines(gpa, &held);
            for (held.items) |line| client.deliverNotification(codex, line);
        }
    }

    fn deliverNotification(client: *Client, codex: *CodexSession, line: []const u8) void {
        var arena: std.heap.ArenaAllocator = .init(client.env.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        const message = JsonView.parseBytes(a, line);
        translate.notify(codex, a, message.str("method"), message.get("params"), line);
    }

    /// Stops routing `thread` to its session, and the session's subagent
    /// threads with it.
    pub fn unregister(client: *Client, thread: []const u8) void {
        client.mutex.lockUncancelable(client.env.io);
        defer client.mutex.unlock(client.env.io);
        const codex = (client.threads.fetchOrderedRemove(.of(thread)) orelse return).value;
        removeValue(&client.children, codex);
    }

    /// Removes every route to `codex`, and forgets that its calls fail.
    pub fn unregisterSession(client: *Client, codex: *CodexSession) void {
        client.mutex.lockUncancelable(client.env.io);
        defer client.mutex.unlock(client.env.io);
        removeValue(&client.threads, codex);
        removeValue(&client.children, codex);
        const owner: *const anyopaque = codex;
        if (std.mem.indexOfScalar(*const anyopaque, client.cancelled_owners.items, owner)) |i| _ = client.cancelled_owners.swapRemove(i);
    }

    /// Waits until no message is being handed to a session.
    pub fn quiesce(client: *Client) void {
        client.dispatch.lockUncancelable(client.env.io);
        client.dispatch.unlock(client.env.io);
    }

    /// Drops the held notifications of a thread no session will register.
    pub fn forget(client: *Client, thread: []const u8) void {
        client.mutex.lockUncancelable(client.env.io);
        defer client.mutex.unlock(client.env.io);
        if (client.orphans.fetchOrderedRemove(.of(thread))) |kv| {
            var list = kv.value;
            freeLines(client.env.gpa, &list);
        }
    }

    /// Routes a subagent thread's messages to its parent session.
    pub fn registerChild(client: *Client, thread: []const u8, parent: *CodexSession) void {
        {
            client.mutex.lockUncancelable(client.env.io);
            defer client.mutex.unlock(client.env.io);
            if (client.threads.contains(.of(thread)) or client.children.contains(.of(thread))) return;
        }
        client.publish(thread, parent, true);
    }

    pub fn unregisterChild(client: *Client, thread: []const u8, parent: *CodexSession) void {
        client.mutex.lockUncancelable(client.env.io);
        defer client.mutex.unlock(client.env.io);
        if (client.children.get(.of(thread))) |routed| if (routed == parent) {
            _ = client.children.orderedRemove(.of(thread));
        };
    }

    // ------------------------------------------------------------ reader

    fn tapLine(context: *anyopaque, sent: bool, line: []const u8) void {
        const client: *Client = @ptrCast(@alignCast(context));
        if (client.tap) |rt| rt.tap(if (sent) .send else .receive, line);
    }

    fn onLine(context: *anyopaque, line: []const u8) void {
        const client: *Client = @ptrCast(@alignCast(context));
        defer _ = client.line_arena.reset(.retain_capacity);
        const a = client.line_arena.allocator();
        const message = JsonView.parseBytes(a, line);
        if (message.object() == null) {
            log.warn("codex: ignoring malformed app-server output", .{});
            return;
        }
        const method = message.str("method");
        const has_id = !message.get("id").isNull();
        if (method.len == 0 and has_id) return client.onResponse(a, message);
        client.dispatch.lockUncancelable(client.env.io);
        defer client.dispatch.unlock(client.env.io);
        if (method.len > 0 and has_id) return client.onRequest(a, message, line);
        if (method.len > 0) client.onNotification(a, method, message.get("params"), line);
    }

    fn onResponse(client: *Client, a: Allocator, message: JsonView) void {
        const io = client.env.io;
        const gpa = client.env.gpa;
        const id = message.get("id");
        if (id.v.? != .integer) return;
        client.mutex.lockUncancelable(io);
        defer client.mutex.unlock(io);
        const slot = (client.calls.fetchSwapRemove(id.v.?.integer) orelse return).value;
        const err = message.get("error");
        if (slot.hook) |hook| hook.run(hook.context, !err.isNull());
        if (!err.isNull()) {
            slot.rpc_code = err.int("code");
            slot.rpc_message = gpa.dupe(u8, err.str("message")) catch null;
            if (!err.get("data").isNull()) {
                const data: ?RawJson = err.get("data").raw(a) catch null;
                slot.rpc_data = gpa.dupe(u8, if (data) |raw| raw.bytes else "null") catch null;
            }
            if (slot.rpc_message == null) slot.closed = true;
        } else {
            const result: ?RawJson = message.get("result").raw(a) catch null;
            slot.result = gpa.dupe(u8, if (result) |raw| raw.bytes else "null") catch null;
            if (slot.result == null) slot.closed = true;
        }
        slot.done.set(io);
    }

    fn onRequest(client: *Client, a: Allocator, message: JsonView, line: []const u8) void {
        const id_value = message.get("id").raw(a) catch return;
        const id_raw = (id_value orelse return).bytes;
        const method = message.str("method");
        const params = message.get("params");
        const thread = decode.threadOf(params);
        if (thread.len > 0) if (client.sessionFor(thread)) |codex| return translate.serverRequest(codex, a, id_raw, method, params, line);
        var diag: Diagnostics = .{};
        if (std.mem.eql(u8, method, "currentTime/read")) {
            const result = agent.json.toValue(a, .{ .currentTimeAt = client.env.nowMs() }) catch return;
            client.reply(a, id_raw, result, &diag) catch |e|
                log.err("codex: answering currentTime/read failed: {s}", .{Diagnostics.of(&diag, e).message});
            return;
        }
        // account/chatgptAuthTokens/refresh lands here too: hosts that
        // manage ChatGPT tokens themselves are not supported.
        client.replyError(a, id_raw, -32601, std.fmt.allocPrint(a, "unsupported request: {s}", .{method}) catch method, &diag) catch |e|
            log.err("codex: refusing request {s} failed: {s}", .{ method, Diagnostics.of(&diag, e).message });
    }

    fn sessionFor(client: *Client, thread: []const u8) ?*CodexSession {
        client.mutex.lockUncancelable(client.env.io);
        defer client.mutex.unlock(client.env.io);
        return client.threads.get(.of(thread)) orelse client.children.get(.of(thread));
    }

    fn onNotification(client: *Client, a: Allocator, method: []const u8, params: JsonView, line: []const u8) void {
        const io = client.env.io;
        const thread = decode.threadOf(params);
        if (thread.len == 0) {
            client.mutex.lockUncancelable(io);
            const all = a.dupe(*CodexSession, client.threads.values()) catch &.{};
            client.mutex.unlock(io);
            for (all) |codex| translate.notify(codex, a, method, params, line);
            return;
        }
        var owner: ?*CodexSession = null;
        client.mutex.lockUncancelable(io);
        const routed = client.threads.get(.of(thread)) orelse client.children.get(.of(thread));
        if (routed == null) {
            client.holdOrphanLocked(thread, line);
            if (std.mem.eql(u8, method, "turn/started")) {
                // A thread whose turn descends from one of our turns is a
                // subagent of that session, even before an item names it.
                const root = params.get("turn").str("rootTurnId");
                for (client.threads.values()) |codex| if (codex.ownsTurn(root)) {
                    owner = codex;
                    break;
                };
            }
        }
        client.mutex.unlock(io);
        if (routed) |codex| translate.notify(codex, a, method, params, line);
        if (owner) |parent| translate.addChild(parent, thread, "");
    }

    fn holdOrphanLocked(client: *Client, thread: []const u8, line: []const u8) void {
        const gpa = client.env.gpa;
        const key: Id = .of(thread);
        if (client.orphans.count() >= max_orphan_threads and !client.orphans.contains(key)) return;
        const gop = client.orphans.getOrPut(gpa, key) catch return;
        if (!gop.found_existing) gop.value_ptr.* = .empty;
        if (gop.value_ptr.items.len >= max_orphans) return;
        const copy = gpa.dupe(u8, line) catch return;
        gop.value_ptr.append(gpa, copy) catch gpa.free(copy);
    }

    fn onExit(context: *anyopaque, err: ?types.ErrorInfo) void {
        const client: *Client = @ptrCast(@alignCast(context));
        const io = client.env.io;
        const gpa = client.env.gpa;
        client.dispatch.lockUncancelable(io);
        defer client.dispatch.unlock(io);
        client.mutex.lockUncancelable(io);
        client.ended = true;
        if (!client.closing.load(.acquire)) if (err) |info| {
            client.end_error = agent.clone(types.ErrorInfo, client.end_arena.allocator(), info) catch .{ .kind = .exit };
        };
        for (client.calls.values()) |slot| {
            slot.closed = true;
            slot.done.set(io);
        }
        client.calls.clearRetainingCapacity();
        var sessions: std.ArrayList(*CodexSession) = .empty;
        defer sessions.deinit(gpa);
        // Without memory for a copy the routes are taken instead. No route
        // is added once the client ended, so every session still ends.
        var taken: IdMap(*CodexSession) = .empty;
        defer taken.deinit(gpa);
        sessions.appendSlice(gpa, client.threads.values()) catch {
            taken = client.threads;
            client.threads = .empty;
        };
        const end_error = client.end_error;
        client.mutex.unlock(io);
        for (sessions.items) |codex| codex.endFromClient(end_error);
        for (taken.values()) |codex| codex.endFromClient(end_error);
        client.done.set(io);
    }
};

fn appServerArgs(a: Allocator, opts: Options) Allocator.Error![]const []const u8 {
    var args: std.ArrayList([]const u8) = .empty;
    try args.append(a, if (opts.path.len > 0) opts.path else "codex");
    try args.append(a, "app-server");
    if (opts.config) |overrides| {
        for (try providerkit.sortedKeys(a, overrides.map.keys())) |key| {
            try args.append(a, "-c");
            try args.append(a, try std.fmt.allocPrint(a, "{s}={s}", .{ key, overrides.map.get(key).? }));
        }
    }
    for (opts.enable) |feature| try args.appendSlice(a, &.{ "--enable", feature });
    for (opts.disable) |feature| try args.appendSlice(a, &.{ "--disable", feature });
    try args.appendSlice(a, opts.extra_args);
    return args.items;
}

fn destroyCall(gpa: Allocator, slot: *Call) void {
    if (slot.result) |r| gpa.free(r);
    if (slot.rpc_message) |m| gpa.free(m);
    if (slot.rpc_data) |d| gpa.free(d);
    gpa.destroy(slot);
}

fn freeLines(gpa: Allocator, list: *std.ArrayList([]u8)) void {
    for (list.items) |line| gpa.free(line);
    list.deinit(gpa);
}

fn removeValue(map: *IdMap(*CodexSession), codex: *CodexSession) void {
    var i: usize = 0;
    while (i < map.count()) {
        if (map.values()[i] == codex) map.orderedRemoveAt(i) else i += 1;
    }
}
