//! The bridge extension (bridge.ts): written once per process to a
//! content-addressed file in the user cache directory, which Pi loads with
//! --extension.
const std = @import("std");
const Io = std.Io;
const agent = @import("agent");
const errors = agent.errors;
const kit = agent.providerkit;
const Diagnostics = errors.Diagnostics;

pub const source = @embedFile("bridge.ts");

/// An unused bridge version is kept this long: Pi reads the file again
/// whenever it replaces a session, so a version a running Pi may still use
/// must stay.
const stale_ms: i64 = 30 * 24 * 3600 * 1000;

var mutex: Io.Mutex = .init;
var path_buf: [std.fs.max_path_bytes]u8 = undefined;
var path_len: usize = 0;

/// The bridge file's path, writing it on first use.
pub fn localPath(env: agent.Env, diag: ?*Diagnostics) errors.Error![]const u8 {
    const io = env.io;
    mutex.lockUncancelable(io);
    defer mutex.unlock(io);
    if (path_len == 0) try write(env, diag);
    const path = path_buf[0..path_len];
    // Marks the version in use on every start, for long-running hosts.
    Io.Dir.cwd().setTimestampsNow(io, path, .{}) catch {};
    return path;
}

fn write(env: agent.Env, diag: ?*Diagnostics) errors.Error!void {
    const io = env.io;
    var arena: std.heap.ArenaAllocator = .init(env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const dir = try kit.cacheDir(env, a, "", diag);
    var sum: [32]u8 = undefined;
    std.crypto.hash.sha2.Sha256.hash(source, &sum, .{});
    const name = try std.fmt.allocPrint(a, "pi-bridge-{x}.ts", .{sum[0..8]});
    const path = try std.fs.path.join(a, &.{ dir, name });
    if (path.len > path_buf.len) return errors.fail(diag, .internal, "pi: bridge path too long", .{});
    defer prune(env, dir, name);

    const cwd = Io.Dir.cwd();
    const existing = cwd.readFileAlloc(io, path, a, .limited(1 << 20)) catch |e| switch (e) {
        error.FileNotFound => null,
        else => return errors.fail(diag, .internal, "pi: read bridge: {t}", .{e}),
    };
    if (existing == null or !std.mem.eql(u8, existing.?, source)) {
        var rnd: [8]u8 = undefined;
        io.random(&rnd);
        const tmp = try std.fmt.allocPrint(a, "{s}.{x}.tmp", .{ path, &rnd });
        cwd.writeFile(io, .{ .sub_path = tmp, .data = source }) catch |e| return errors.fail(diag, .internal, "pi: write bridge: {t}", .{e});
        cwd.rename(tmp, cwd, path, io) catch |e| {
            cwd.deleteFile(io, tmp) catch {};
            return errors.fail(diag, .internal, "pi: write bridge: {t}", .{e});
        };
    }
    @memcpy(path_buf[0..path.len], path);
    path_len = path.len;
}

/// Removes bridge versions other than `keep` that no process wrote or used
/// for `stale_ms`.
fn prune(env: agent.Env, dir_path: []const u8, keep: []const u8) void {
    const io = env.io;
    var dir = Io.Dir.cwd().openDir(io, dir_path, .{ .iterate = true }) catch return;
    defer dir.close(io);
    const now = env.nowMs();
    var it = dir.iterate();
    while (it.next(io) catch null) |entry| {
        if (!std.mem.startsWith(u8, entry.name, "pi-bridge-") or !std.mem.endsWith(u8, entry.name, ".ts") or std.mem.eql(u8, entry.name, keep)) continue;
        const stat = dir.statFile(io, entry.name, .{}) catch continue;
        // Best effort: a version left behind is removed on a later start.
        if (now - stat.mtime.toMilliseconds() > stale_ms) dir.deleteFile(io, entry.name) catch {};
    }
}
