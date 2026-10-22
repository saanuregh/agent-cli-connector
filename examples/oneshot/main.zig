//! oneshot sends one prompt to a harness, streams the reply, and prints what
//! the turn used. It is the smallest complete program: start a session,
//! send, stream, wait, close.
//!
//!     zig build && ./zig-out/bin/oneshot "Summarize this repository in three bullets"
//!     ./zig-out/bin/oneshot --provider codex "What does build.zig build?"
//!     git diff | ./zig-out/bin/oneshot --provider pi "Review this diff:"
//!
//! Tool calls that need approval are refused, so the agent can read but not
//! change anything. The session is ephemeral: nothing is left in the
//! harness's session list.
const std = @import("std");
const agent = @import("agent");

pub fn main(init: std.process.Init) !void {
    const io = init.io;
    const gpa = init.gpa;
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();

    var provider_name: []const u8 = "claude";
    var model: []const u8 = "";
    var words: std.ArrayList([]const u8) = .empty;
    var args = try init.minimal.args.iterateAllocator(a);
    _ = args.skip();
    while (args.next()) |arg| {
        if (std.mem.eql(u8, arg, "--provider")) {
            provider_name = args.next() orelse return usage();
        } else if (std.mem.eql(u8, arg, "--model")) {
            model = args.next() orelse return usage();
        } else try words.append(a, arg);
    }
    var prompt: []const u8 = try std.mem.join(a, " ", words.items);
    if (!try std.Io.File.stdin().isTty(io)) {
        var buf: [4096]u8 = undefined;
        var in = std.Io.File.stdin().readerStreaming(io, &buf);
        const piped = try in.interface.allocRemaining(a, .limited(16 << 20));
        prompt = try std.fmt.allocPrint(a, "{s}\n\n{s}", .{ prompt, piped });
    }
    prompt = std.mem.trim(u8, prompt, " \n\r\t");
    if (prompt.len == 0) return usage();

    const env: agent.Env = .{ .io = io, .gpa = gpa, .environ = init.minimal.environ };
    var diag: agent.Diagnostics = .{};
    const provider = agent.registry.create(env, provider_name, null, &diag) catch |e| return fail(&diag, e);
    defer provider.deinit();
    const session = provider.start(.{
        .config = .{ .model = model, .ephemeral = true },
        .request_handler = .{ .permission = denyAll },
    }, &diag) catch |e| return fail(&diag, e);
    defer session.deinit();

    var out_buf: [4096]u8 = undefined;
    var stdout = std.Io.File.stdout().writerStreaming(io, &out_buf);
    const out = &stdout.interface;
    var err_buf: [1024]u8 = undefined;
    var stderr = std.Io.File.stderr().writerStreaming(io, &err_buf);
    const err = &stderr.interface;

    var turn = session.send(.{ .text = prompt }, &diag) catch |e| return fail(&diag, e);
    while (turn.nextEvent(.none)) |owned| {
        defer owned.destroy();
        switch (owned.event.payload) {
            .text_delta => |d| if (d.parent_id.len == 0) { // not a subagent's text
                try out.writeAll(d.delta);
                try out.flush();
            },
            .item_started => |s| if (s.item.tool) |t| {
                try err.print("[{t} {s}]\n", .{ t.kind, t.name });
                try err.flush();
            },
            else => {},
        }
    } else |_| {}

    const res = turn.wait(a, .none, &diag) catch |e| return fail(&diag, e);
    const info = try session.info(a);
    const u = res.usage;
    try out.writeAll("\n");
    try out.flush();
    try err.print("\n{s} on {s}: {t}, {d} in / {d} out tokens", .{ session.provider(), info.model, res.status, u.input_tokens, u.output_tokens });
    if (u.cost_usd > 0) try err.print(", ${d:.4}", .{u.cost_usd});
    try err.print(", {d:.1}s\n", .{@as(f64, @floatFromInt(res.duration_ms)) / 1000});
    if (res.@"error") |e| try err.print("error: {s}\n", .{e.message});
    try err.flush();
}

fn denyAll(_: ?*anyopaque, _: *const agent.types.PermissionRequest) agent.types.PermissionResponse {
    return .{ .decision = .deny, .message = "This run is read-only." };
}

fn usage() void {
    std.debug.print("usage: oneshot [--provider claude|codex|pi] [--model id] <prompt>\n", .{});
    std.process.exit(2);
}

fn fail(diag: *const agent.Diagnostics, err: agent.Error) void {
    const info = agent.Diagnostics.of(diag, err);
    std.debug.print("error: {s}\n", .{info.message});
    if (info.stderr.len > 0) std.debug.print("{s}\n", .{info.stderr});
    std.process.exit(1);
}
