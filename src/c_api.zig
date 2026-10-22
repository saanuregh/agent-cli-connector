//! The C ABI: an in-process JSON-RPC 2.0 message pipe (see `rpc.zig` and
//! `include/agent.h`). The library owns its I/O and memory; messages it
//! returns are freed with `agent_free`.
const std = @import("std");
const builtin = @import("builtin");
const agent = @import("agent");
const Server = agent.rpc.Server;

const gpa = std.heap.c_allocator;

var threaded: std.Io.Threaded = undefined;
var environ: std.process.Environ = undefined;
var state: std.atomic.Value(enum(u8) { none, starting, ready }) = .init(.none);

/// Starts the library's I/O once per process.
fn env() agent.Env {
    if (state.cmpxchgStrong(.none, .starting, .acq_rel, .acquire) == null) {
        environ = if (builtin.os.tag == .windows)
            .{ .block = .global }
        else
            .{ .block = .{ .slice = std.mem.span(@as([*:null]const ?[*:0]const u8, @ptrCast(std.c.environ))) } };
        threaded = .init(gpa, .{ .environ = environ, .stack_size = agent.runtime.thread_stack_size });
        addOnStack();
        state.store(.ready, .release);
    }
    while (state.load(.acquire) != .ready) std.Thread.yield() catch {};
    return .{ .io = threaded.io(), .gpa = gpa, .environ = environ };
}

/// The I/O implementation installs SIGIO and SIGPIPE handlers. Hosts with
/// their own runtime, such as Go, require every handler to run on the
/// alternate signal stack.
fn addOnStack() void {
    if (builtin.os.tag == .windows) return;
    const posix = std.posix;
    for ([_]posix.SIG{ .IO, .PIPE }) |sig| {
        var act: posix.Sigaction = undefined;
        posix.sigaction(sig, null, &act);
        act.flags |= posix.SA.ONSTACK;
        posix.sigaction(sig, &act, null);
    }
}

/// Opens a connection: a fresh set of providers and sessions.
export fn agent_open() ?*Server {
    return Server.create(env()) catch null;
}

/// Sends one JSON-RPC message. Returns 0, or -1 for invalid arguments.
export fn agent_send(server: ?*Server, msg: ?[*]const u8, len: usize) c_int {
    const s = server orelse return -1;
    const m = msg orelse return -1;
    s.receive(m[0..len]);
    return 0;
}

/// Waits up to `timeout_ms` (negative waits forever) for the next message.
/// Returns 1 and sets `*out` to a NUL-terminated JSON text (free with
/// `agent_free`), 0 on timeout, or -1 once the connection is closed and
/// every message was read.
export fn agent_recv(server: ?*Server, timeout_ms: c_int, out: ?*?[*:0]u8) c_int {
    const s = server orelse return -1;
    const o = out orelse return -1;
    o.* = null;
    const msg = (s.recv(timeoutOf(timeout_ms)) catch return -1) orelse return 0;
    o.* = msg.ptr;
    return 1;
}

/// `agent_recv` that takes every queued message at once: they are joined
/// by newlines in one NUL-terminated buffer of `*len` bytes (free with
/// `agent_free`).
export fn agent_recv_batch(server: ?*Server, timeout_ms: c_int, out: ?*?[*:0]u8, len: ?*usize) c_int {
    const s = server orelse return -1;
    const o = out orelse return -1;
    const l = len orelse return -1;
    o.* = null;
    l.* = 0;
    const msg = (s.recvBatch(timeoutOf(timeout_ms)) catch return -1) orelse return 0;
    o.* = msg.ptr;
    l.* = msg.len;
    return 1;
}

fn timeoutOf(timeout_ms: c_int) std.Io.Timeout {
    return if (timeout_ms < 0) .none else .{ .duration = .{ .raw = .fromMilliseconds(timeout_ms), .clock = .awake } };
}

/// Frees a message returned by `agent_recv` or `agent_recv_batch`.
export fn agent_free(msg: ?[*:0]u8) void {
    if (msg) |m| gpa.free(std.mem.span(m));
}

/// Closes every session of the connection and wakes a blocked
/// `agent_recv`, which then drains the remaining messages and returns -1.
/// Call `agent_destroy` afterwards, once no thread uses the connection.
export fn agent_close(server: ?*Server) void {
    if (server) |s| s.shutdown();
}

/// Frees a connection, closing it first.
export fn agent_destroy(server: ?*Server) void {
    if (server) |s| s.destroy();
}

/// The library version, such as "0.2.0".
export fn agent_version() [*:0]const u8 {
    return agent.rpc.library_version;
}

test "c abi round trip" {
    const s = agent_open().?;
    defer agent_destroy(s);
    const init = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"providers\":{\"fake\":null}}}";
    try std.testing.expectEqual(@as(c_int, 0), agent_send(s, init, init.len));
    var out: ?[*:0]u8 = null;
    try std.testing.expectEqual(@as(c_int, 1), agent_recv(s, 5000, &out));
    defer agent_free(out);
    try std.testing.expect(std.mem.indexOf(u8, std.mem.span(out.?), "\"fake\"") != null);
    agent_close(s);
    var rest: ?[*:0]u8 = null;
    try std.testing.expectEqual(@as(c_int, -1), agent_recv(s, 0, &rest));
}

test "c abi batch receive joins queued messages" {
    const s = agent_open().?;
    defer agent_destroy(s);
    const msgs = [_][]const u8{
        "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"providers\":{\"fake\":null}}}",
        "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"nope\"}",
        "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"nope\"}",
    };
    for (msgs) |m| try std.testing.expectEqual(@as(c_int, 0), agent_send(s, m.ptr, m.len));
    // Every request is answered; the answers arrive in one or more batches.
    var lines: usize = 0;
    while (lines < msgs.len) {
        var out: ?[*:0]u8 = null;
        var len: usize = 0;
        try std.testing.expectEqual(@as(c_int, 1), agent_recv_batch(s, 5000, &out, &len));
        defer agent_free(out);
        try std.testing.expectEqual(len, std.mem.span(out.?).len);
        lines += std.mem.count(u8, out.?[0..len], "\n") + 1;
    }
    try std.testing.expectEqual(msgs.len, lines);
}
