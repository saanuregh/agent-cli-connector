//! agentd serves the library's JSON-RPC protocol over standard input and
//! output, one message per line, for programs that prefer a subprocess to
//! loading the library. It exits when standard input closes.
const std = @import("std");
const agent = @import("agent");

pub fn main(init: std.process.Init) !void {
    const io = init.io;
    // The C allocator copes with many busy threads; smp_allocator has one
    // slot per CPU and threads beyond that spin waiting for one.
    const gpa = std.heap.c_allocator;
    const server = try agent.rpc.Server.create(.{ .io = io, .gpa = gpa, .environ = init.minimal.environ });
    // Closes every session, stopping the harnesses, before exiting.
    defer server.destroy();

    var reader = try io.concurrent(readRequests, .{ io, init.gpa, server });
    // Runs first: no request arrives while the server goes.
    defer reader.cancel(io);

    var buf: [64 * 1024]u8 = undefined;
    var out = std.Io.File.stdout().writerStreaming(io, &buf);
    const w = &out.interface;
    // Writes what is queued, then flushes once: one write per burst of
    // events rather than per event.
    var timeout: std.Io.Timeout = .none;
    while (true) {
        const msg = server.recv(timeout) catch break orelse {
            w.flush() catch break;
            timeout = .none;
            continue;
        };
        defer gpa.free(msg);
        w.writeAll(msg) catch break;
        w.writeByte('\n') catch break;
        timeout = .{ .duration = .{ .raw = .zero, .clock = .awake } };
    }
    w.flush() catch {};
}

fn readRequests(io: std.Io, gpa: std.mem.Allocator, server: *agent.rpc.Server) void {
    defer server.shutdown();
    var buf: [64 * 1024]u8 = undefined;
    var in = std.Io.File.stdin().readerStreaming(io, &buf);
    const r = &in.interface;
    var line: std.Io.Writer.Allocating = .init(gpa);
    defer line.deinit();
    while (true) {
        line.clearRetainingCapacity();
        _ = r.streamDelimiter(&line.writer, '\n') catch |e| switch (e) {
            error.EndOfStream => {
                if (line.written().len > 0) server.receive(line.written());
                return;
            },
            else => return,
        };
        r.toss(1);
        const l = std.mem.trim(u8, line.written(), " \r\t");
        if (l.len > 0) server.receive(l);
    }
}
