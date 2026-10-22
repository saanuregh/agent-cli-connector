//! A harness process that exchanges newline-delimited messages over stdin
//! and stdout. Stdout lines go to a handler on a reader task; stderr is
//! kept as a short tail for error reports.
const std = @import("std");
const builtin = @import("builtin");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const Env = @import("runtime.zig").Env;
const types = @import("types.zig");
const errors = @import("errors.zig");
const json = @import("json.zig");
const Diagnostics = errors.Diagnostics;
const log = std.log.scoped(.agent);

const stderr_tail_lines = 40;
/// How long readers may keep running after the process exited, for output
/// still in the pipes. A descendant that left the process group could hold
/// the pipes open forever.
const drain_grace_ms = 2000;
/// The longest output line read. A longer line is dropped with a warning,
/// so one runaway message cannot exhaust memory.
pub const max_line_bytes = 256 << 20;

pub const Handler = struct {
    context: *anyopaque,
    /// Called for each non-blank stdout line, trailing CR removed. The line
    /// is valid only during the call.
    on_line: *const fn (context: *anyopaque, line: []const u8) void,
    /// Called once after the process exited and its output was read.
    /// `err` is null for a clean exit (status 0).
    on_exit: *const fn (context: *anyopaque, err: ?types.ErrorInfo) void,
};

pub const Options = struct {
    /// Names the harness in errors.
    provider: []const u8,
    argv: []const []const u8,
    /// Runs before `argv` on the command line, such as a wrapper.
    command_prefix: []const []const u8 = &.{},
    cwd: []const u8 = "",
    /// Added to the inherited environment.
    env: ?types.StringMap = null,
    /// Removed from the inherited environment.
    unset_env: []const []const u8 = &.{},
    /// Receives every protocol line in both directions.
    tap: ?Tap = null,
};

pub const Tap = struct {
    context: *anyopaque,
    line: *const fn (context: *anyopaque, sent: bool, line: []const u8) void,
};

pub const Process = struct {
    env: Env,
    provider: []const u8,
    child: std.process.Child,
    pid: std.process.Child.Id,
    tap: ?Tap,
    handler: Handler,

    write_mutex: Io.Mutex = .init,
    stdin: ?Io.File,
    stdin_buf: [64 * 1024]u8 = undefined,
    stdin_writer: Io.File.Writer = undefined,

    stdout: Io.File,
    stderr: Io.File,

    tail_mutex: Io.Mutex = .init,
    tail: [stderr_tail_lines]std.ArrayList(u8) = @splat(.empty),
    tail_next: usize = 0,

    exited: Io.Event = .unset,
    stdout_done: Io.Event = .unset,
    stderr_done: Io.Event = .unset,
    term: ?std.process.Child.Term = null,
    waiter: Io.Future(void) = undefined,
    stdout_reader: Io.Future(void) = undefined,
    stderr_reader: Io.Future(void) = undefined,
    stopped: std.atomic.Value(bool) = .init(false),

    /// Starts the harness. `handler` starts receiving lines at once.
    pub fn start(env: Env, opts: Options, handler: Handler, diag: ?*Diagnostics) errors.Error!*Process {
        const gpa = env.gpa;
        var arena: std.heap.ArenaAllocator = .init(gpa);
        defer arena.deinit();
        const a = arena.allocator();

        var env_map = try environment(env, a, opts, diag);
        const argv = try std.mem.concat(a, []const u8, &.{ opts.command_prefix, opts.argv });
        const exe = resolveExecutable(env, a, argv[0], env_map.get("PATH") orelse "") orelse
            return errors.failWith(diag, .{
                .kind = .not_found,
                .code = "executable_not_found",
                .message = try std.fmt.allocPrint(a, "{s}: executable \"{s}\" not found", .{ opts.provider, argv[0] }),
            });
        argv[0] = exe;

        const p = try gpa.create(Process);
        p.child = std.process.spawn(env.io, .{
            .argv = argv,
            .cwd = if (opts.cwd.len > 0) .{ .path = opts.cwd } else .inherit,
            .environ_map = &env_map,
            .stdin = .pipe,
            .stdout = .pipe,
            .stderr = .pipe,
            .pgid = if (builtin.os.tag == .windows) null else 0,
        }) catch |e| {
            gpa.destroy(p);
            return errors.fail(diag, .exit, "{s}: start \"{s}\": {t}", .{ opts.provider, exe, e });
        };

        p.* = .{
            .env = env,
            .provider = opts.provider,
            .child = p.child,
            .pid = p.child.id.?,
            .tap = opts.tap,
            .handler = handler,
            .stdin = p.child.stdin.?,
            .stdout = p.child.stdout.?,
            .stderr = p.child.stderr.?,
        };
        // The pipes are ours: Child.wait would close them under the readers.
        p.child.stdin = null;
        p.child.stdout = null;
        p.child.stderr = null;
        p.stdin_writer = p.stdin.?.writerStreaming(env.io, &p.stdin_buf);

        // Cancelable tasks rather than plain threads: a read blocked on a pipe
        // that a descendant holds open can be stopped, so the readers are
        // known to be done before the exit handler runs.
        p.stdout_reader = env.io.concurrent(readStdout, .{p}) catch return p.startFailed(diag);
        p.stderr_reader = env.io.concurrent(readStderr, .{p}) catch return p.startFailed(diag);
        p.waiter = env.io.concurrent(waitExit, .{p}) catch return p.startFailed(diag);
        return p;
    }

    /// Kills the harness. The process is leaked: a reader task may already
    /// be running on it.
    fn startFailed(p: *Process, diag: ?*Diagnostics) errors.Error {
        p.kill(.kill);
        return errors.fail(diag, .internal, "{s}: cannot start reader tasks", .{p.provider});
    }

    /// Writes one message followed by a newline. Concurrent writes do not
    /// interleave.
    pub fn writeLine(p: *Process, line: []const u8, diag: ?*Diagnostics) errors.Error!void {
        p.write_mutex.lockUncancelable(p.env.io);
        defer p.write_mutex.unlock(p.env.io);
        if (p.stdin == null) return errors.fail(diag, .closed, "{s}: input closed", .{p.provider});
        if (p.tap) |t| t.line(t.context, true, line);
        const w = &p.stdin_writer.interface;
        w.writeAll(line) catch return p.writeFailed(diag);
        w.writeByte('\n') catch return p.writeFailed(diag);
        w.flush() catch return p.writeFailed(diag);
    }

    fn writeFailed(p: *Process, diag: ?*Diagnostics) errors.Error {
        if (p.exited.isSet()) {
            var arena: std.heap.ArenaAllocator = .init(p.env.gpa);
            defer arena.deinit();
            return errors.failWith(diag, p.exitInfo(arena.allocator()) catch .{ .kind = .exit });
        }
        return errors.fail(diag, .exit, "{s}: write to harness failed", .{p.provider});
    }

    /// Closes the harness input, which asks most harnesses to exit.
    pub fn closeStdin(p: *Process) void {
        p.write_mutex.lockUncancelable(p.env.io);
        defer p.write_mutex.unlock(p.env.io);
        if (p.stdin) |f| f.close(p.env.io);
        p.stdin = null;
    }

    /// Shuts the process down: closes stdin, waits up to `grace_ms` for it
    /// to exit, then terminates the process group, and finally kills it.
    /// Returns after the exit handler ran; do not call it from the handler.
    pub fn stop(p: *Process, grace_ms: i64) void {
        if (p.stopped.swap(true, .acq_rel)) return;
        p.closeStdin();
        if (!p.waitExited(grace_ms)) {
            p.kill(.terminate);
            if (!p.waitExited(2000)) p.kill(.kill);
        }
        p.waiter.await(p.env.io);
    }

    /// Frees the process. Call after `stop`.
    pub fn destroy(p: *Process) void {
        p.stdout.close(p.env.io);
        p.stderr.close(p.env.io);
        for (&p.tail) |*l| l.deinit(p.env.gpa);
        p.env.gpa.destroy(p);
    }

    /// Signals the harness's process group. On Windows, `taskkill /T` stops
    /// the process tree, falling back to terminating the harness alone.
    fn kill(p: *Process, how: enum { terminate, kill }) void {
        if (builtin.os.tag == .windows) {
            if (windowsPid(p.pid)) |pid| {
                var buf: [24]u8 = undefined;
                const id = std.fmt.bufPrint(&buf, "{d}", .{pid}) catch unreachable;
                const argv: []const []const u8 = switch (how) {
                    .terminate => &.{ "taskkill", "/T", "/PID", id },
                    .kill => &.{ "taskkill", "/F", "/T", "/PID", id },
                };
                if (std.process.run(p.env.gpa, p.env.io, .{ .argv = argv })) |res| {
                    p.env.gpa.free(res.stdout);
                    p.env.gpa.free(res.stderr);
                    if (res.term == .exited and res.term.exited == 0) return;
                } else |_| {}
            }
            if (how == .kill) _ = std.os.windows.ntdll.NtTerminateProcess(p.pid, @fromBackingInt(1));
            return;
        }
        std.posix.kill(-p.pid, switch (how) {
            .terminate => .TERM,
            .kill => .KILL,
        }) catch {};
    }

    fn waitExited(p: *Process, ms: i64) bool {
        const deadline = (Io.Timeout{ .duration = .{ .raw = .fromMilliseconds(ms), .clock = .awake } }).toDeadline(p.env.io);
        while (!p.exited.isSet()) {
            p.exited.waitTimeout(p.env.io, deadline) catch {
                if ((deadline.toDurationFromNow(p.env.io) orelse return false).raw.nanoseconds <= 0) return p.exited.isSet();
            };
        }
        return true;
    }

    pub fn hasExited(p: *Process) bool {
        return p.exited.isSet();
    }

    /// The last lines the harness wrote to stderr, joined with newlines.
    pub fn stderrTail(p: *Process, a: Allocator) Allocator.Error![]u8 {
        p.tail_mutex.lockUncancelable(p.env.io);
        defer p.tail_mutex.unlock(p.env.io);
        var out: std.ArrayList(u8) = .empty;
        for (0..stderr_tail_lines) |k| {
            const line = p.tail[(p.tail_next + k) % stderr_tail_lines].items;
            if (line.len == 0) continue;
            if (out.items.len > 0) try out.append(a, '\n');
            try out.appendSlice(a, line);
        }
        return out.toOwnedSlice(a);
    }

    /// Describes how the process ended. Call after it exited.
    pub fn exitInfo(p: *Process, a: Allocator) Allocator.Error!types.ErrorInfo {
        const how: []u8 = if (p.term) |t| switch (t) {
            .exited => |code| try std.fmt.allocPrint(a, "exit status {d}", .{code}),
            .signal => |s| try std.fmt.allocPrint(a, "signal {t}", .{s}),
            else => try a.dupe(u8, "unknown status"),
        } else try a.dupe(u8, "unknown status");
        return .{
            .kind = .exit,
            .message = try std.fmt.allocPrint(a, "{s} process exited: {s}", .{ p.provider, how }),
            .stderr = try p.stderrTail(a),
        };
    }

    fn readStdout(p: *Process) void {
        defer p.stdout_done.set(p.env.io);
        readLines(p, p.stdout, struct {
            fn line(proc: *Process, l: []const u8) void {
                if (proc.tap) |t| t.line(t.context, false, l);
                proc.handler.on_line(proc.handler.context, l);
            }
        }.line);
    }

    fn readStderr(p: *Process) void {
        defer p.stderr_done.set(p.env.io);
        readLines(p, p.stderr, struct {
            fn line(proc: *Process, l: []const u8) void {
                log.debug("{s} stderr: {s}", .{ proc.provider, l });
                proc.tail_mutex.lockUncancelable(proc.env.io);
                defer proc.tail_mutex.unlock(proc.env.io);
                const slot = &proc.tail[proc.tail_next];
                slot.clearRetainingCapacity();
                slot.appendSlice(proc.env.gpa, l[0..@min(l.len, 4096)]) catch {};
                proc.tail_next = (proc.tail_next + 1) % stderr_tail_lines;
            }
        }.line);
    }

    fn readLines(p: *Process, file: Io.File, comptime handle: fn (*Process, []const u8) void) void {
        var buf: [64 * 1024]u8 = undefined;
        var fr = file.readerStreaming(p.env.io, &buf);
        splitLines(&fr.interface, p.env.gpa, max_line_bytes, p.provider, p, handle);
    }

    fn waitExit(p: *Process) void {
        const io = p.env.io;
        p.term = p.child.wait(io) catch null;
        p.exited.set(io);
        // Give the readers a moment to deliver what is still in the pipes.
        const deadline = (Io.Timeout{ .duration = .{ .raw = .fromMilliseconds(drain_grace_ms), .clock = .awake } }).toDeadline(io);
        while (!p.stdout_done.isSet()) p.stdout_done.waitTimeout(io, deadline) catch {
            if ((deadline.toDurationFromNow(io) orelse break).raw.nanoseconds <= 0) break;
        };
        // Stops readers still blocked after the grace period and waits for
        // them, so no line reaches the handler after it is told of the exit.
        p.stdout_reader.cancel(io);
        p.stderr_reader.cancel(io);
        var arena: std.heap.ArenaAllocator = .init(p.env.gpa);
        defer arena.deinit();
        const clean = if (p.term) |t| t == .exited and t.exited == 0 else false;
        const info: ?types.ErrorInfo = if (clean) null else p.exitInfo(arena.allocator()) catch .{ .kind = .exit };
        p.handler.on_exit(p.handler.context, info);
    }
};

/// Reads `r` to its end and calls `handle` with each non-blank line, trailing
/// CR removed. A line longer than `limit` bytes is dropped with a warning.
fn splitLines(
    r: *Io.Reader,
    gpa: Allocator,
    limit: usize,
    provider: []const u8,
    context: anytype,
    comptime handle: fn (@TypeOf(context), []const u8) void,
) void {
    var line: Io.Writer.Allocating = .init(gpa);
    defer line.deinit();
    while (true) {
        line.clearRetainingCapacity();
        const end = blk: {
            // One byte over the limit tells a line of exactly `limit` bytes
            // from a longer one.
            _ = r.streamDelimiterLimit(&line.writer, '\n', .limited(limit + 1)) catch |e| switch (e) {
                error.StreamTooLong => {
                    const rest = r.discardDelimiterInclusive('\n');
                    log.warn("{s}: dropped an output line of {d}{s} bytes, over the {d}-byte limit", .{
                        provider,
                        line.written().len + (rest catch 0),
                        if (rest) |_| "" else |_| " or more",
                        limit,
                    });
                    // Keep the reader's memory small again.
                    line.deinit();
                    line = .init(gpa);
                    _ = rest catch return;
                    continue;
                },
                error.ReadFailed, error.WriteFailed => break :blk true,
            };
            // Without a delimiter in the buffer, the stream ended.
            if (r.seek == r.end) break :blk true;
            r.toss(1);
            break :blk false;
        };
        const l = std.mem.trimEnd(u8, line.written(), "\r");
        if (std.mem.trim(u8, l, " \t").len > 0) handle(context, l);
        if (end) return;
    }
}

/// The process ID behind a Windows process handle.
fn windowsPid(handle: std.process.Child.Id) ?usize {
    const w = std.os.windows;
    var info: w.PROCESS.BASIC_INFORMATION = undefined;
    const status = w.ntdll.NtQueryInformationProcess(handle, .BasicInformation, &info, @sizeOf(w.PROCESS.BASIC_INFORMATION), null);
    if (status != .SUCCESS) return null;
    return info.UniqueProcessId;
}

/// The inherited environment with the options' changes applied.
fn environment(env: Env, a: Allocator, opts: Options, diag: ?*Diagnostics) errors.Error!std.process.Environ.Map {
    var map = std.process.Environ.createMap(env.environ, a) catch |e| return errors.fail(diag, .internal, "read environment: {t}", .{e});
    for (opts.unset_env) |name| _ = map.swapRemove(name);
    if (opts.env) |extra| {
        var it = extra.map.iterator();
        while (it.next()) |e| try map.put(e.key_ptr.*, e.value_ptr.*);
    }
    return map;
}

/// Finds `name` on `path_list` unless it already contains a separator.
pub fn resolveExecutable(env: Env, a: Allocator, name: []const u8, path_list: []const u8) ?[]const u8 {
    if (std.mem.indexOfAny(u8, name, "/\\") != null) return name;
    // Windows finds "claude" as "claude.cmd" or "claude.exe".
    const extensions: []const []const u8 = if (builtin.os.tag == .windows) &.{ ".exe", ".cmd", ".bat", ".com", "" } else &.{""};
    var it = std.mem.tokenizeScalar(u8, path_list, std.fs.path.delimiter);
    while (it.next()) |dir| for (extensions) |ext| {
        const full = std.mem.concat(a, u8, &.{ dir, std.fs.path.sep_str, name, ext }) catch return null;
        Io.Dir.cwd().access(env.io, full, .{ .execute = builtin.os.tag != .windows }) catch continue;
        return full;
    };
    return null;
}

/// Runs a short command to completion and returns its trimmed stdout, for
/// version and status checks. Fails when it exits with a non-zero status.
pub fn output(env: Env, a: Allocator, argv: []const []const u8, extra_env: ?types.StringMap, diag: ?*Diagnostics) errors.Error![]const u8 {
    var env_map = try environment(env, a, .{ .provider = "", .argv = argv, .env = extra_env }, diag);
    const exe = resolveExecutable(env, a, argv[0], env_map.get("PATH") orelse "") orelse
        return errors.fail(diag, .not_found, "executable \"{s}\" not found", .{argv[0]});
    const full = try a.dupe([]const u8, argv);
    full[0] = exe;
    const res = std.process.run(a, env.io, .{
        .argv = full,
        .environ_map = &env_map,
        .timeout = .{ .duration = .{ .raw = .fromSeconds(30), .clock = .awake } },
    }) catch |e| return errors.fail(diag, .exit, "{s}: {t}", .{ argv[0], e });
    if (!(res.term == .exited and res.term.exited == 0)) {
        return errors.failWith(diag, .{ .kind = .exit, .message = try std.fmt.allocPrint(a, "{s} failed", .{argv[0]}), .stderr = std.mem.trim(u8, res.stderr, " \n\r\t") });
    }
    return std.mem.trim(u8, res.stdout, " \n\r\t");
}

test "process round trip" {
    const gpa = std.testing.allocator;
    var threaded: Io.Threaded = .init(gpa, .{ .environ = std.testing.environ });
    defer threaded.deinit();
    const env: Env = .{ .io = threaded.io(), .gpa = gpa, .environ = std.testing.environ };

    const Ctx = struct {
        lines: std.ArrayList([]u8) = .empty,
        exit: ?types.ErrorKind = null,
        fn line(c: *anyopaque, l: []const u8) void {
            const self: *@This() = @ptrCast(@alignCast(c));
            self.lines.append(std.testing.allocator, std.testing.allocator.dupe(u8, l) catch return) catch {};
        }
        fn exited(c: *anyopaque, err: ?types.ErrorInfo) void {
            const self: *@This() = @ptrCast(@alignCast(c));
            self.exit = if (err) |e| e.kind else null;
        }
    };
    var ctx: Ctx = .{};
    defer {
        for (ctx.lines.items) |l| gpa.free(l);
        ctx.lines.deinit(gpa);
    }
    const argv: []const []const u8 = if (builtin.os.tag == .windows)
        &.{ "cmd", "/c", "set /p l=& call echo got %l%& (echo err)1>&2& exit 3" }
    else
        &.{ "sh", "-c", "read l; echo \"got $l\"; echo err >&2; exit 3" };
    const p = try Process.start(env, .{ .provider = "sh", .argv = argv }, .{ .context = &ctx, .on_line = Ctx.line, .on_exit = Ctx.exited }, null);
    try p.writeLine("hi", null);
    p.stop(5000);
    defer p.destroy();
    try std.testing.expectEqualStrings("got hi", ctx.lines.items[0]);
    try std.testing.expectEqual(types.ErrorKind.exit, ctx.exit.?);
    const tail = try p.stderrTail(gpa);
    defer gpa.free(tail);
    try std.testing.expectEqualStrings("err", tail);
}

test "a line over the limit is dropped and the next line is delivered" {
    const Lines = struct {
        arena: Allocator,
        got: std.ArrayList([]const u8) = .empty,
        fn line(self: *@This(), l: []const u8) void {
            self.got.append(self.arena, self.arena.dupe(u8, l) catch return) catch {};
        }
    };
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    var lines: Lines = .{ .arena = arena.allocator() };
    // The dropped lines are logged as warnings, which the test runner
    // would print.
    const log_level = std.testing.log_level;
    std.testing.log_level = .err;
    defer std.testing.log_level = log_level;
    const too_long: [40]u8 = @splat('x');
    const at_limit: [16]u8 = @splat('y');
    var r: Io.Reader = .fixed("short\n" ++ too_long ++ "\n" ++ at_limit ++ "\nnext\r\n" ++ too_long);
    splitLines(&r, std.testing.allocator, 16, "test", &lines, Lines.line);
    try std.testing.expectEqual(@as(usize, 3), lines.got.items.len);
    try std.testing.expectEqualStrings("short", lines.got.items[0]);
    // A line of exactly the limit is kept.
    try std.testing.expectEqualStrings(&at_limit, lines.got.items[1]);
    try std.testing.expectEqualStrings("next", lines.got.items[2]);
}

test "exit is reported while a descendant holds the output open" {
    if (builtin.os.tag == .windows) return error.SkipZigTest; // needs a POSIX shell
    const gpa = std.testing.allocator;
    var threaded: Io.Threaded = .init(gpa, .{ .environ = std.testing.environ });
    defer threaded.deinit();
    const env: Env = .{ .io = threaded.io(), .gpa = gpa, .environ = std.testing.environ };
    const Ctx = struct {
        lines: usize = 0,
        fn line(c: *anyopaque, _: []const u8) void {
            const self: *@This() = @ptrCast(@alignCast(c));
            self.lines += 1;
        }
        fn exited(_: *anyopaque, _: ?types.ErrorInfo) void {}
    };
    var ctx: Ctx = .{};
    const started = Io.Clock.awake.now(env.io);
    // The background sleep keeps stdout open after the shell exits.
    const p = try Process.start(env, .{ .provider = "sh", .argv = &.{ "sh", "-c", "echo hi; sleep 3 & exit 0" } }, .{ .context = &ctx, .on_line = Ctx.line, .on_exit = Ctx.exited }, null);
    p.stop(5000);
    p.destroy();
    const ms = started.untilNow(env.io, .awake).toMilliseconds();
    try std.testing.expectEqual(@as(usize, 1), ctx.lines);
    try std.testing.expect(ms < 2900); // the grace period, not the descendant's lifetime
}

test "stop ends a harness that ignores its closed input" {
    const gpa = std.testing.allocator;
    var threaded: Io.Threaded = .init(gpa, .{ .environ = std.testing.environ });
    defer threaded.deinit();
    const env: Env = .{ .io = threaded.io(), .gpa = gpa, .environ = std.testing.environ };
    const Ctx = struct {
        fn line(_: *anyopaque, _: []const u8) void {}
        fn exited(_: *anyopaque, _: ?types.ErrorInfo) void {}
    };
    var dummy: u8 = 0;
    const argv: []const []const u8 = if (builtin.os.tag == .windows)
        &.{ "cmd", "/c", "ping -n 60 127.0.0.1 >nul" }
    else
        &.{ "sh", "-c", "trap '' TERM; sleep 60" };
    const started = Io.Clock.awake.now(env.io);
    const p = try Process.start(env, .{ .provider = "sleeper", .argv = argv }, .{ .context = &dummy, .on_line = Ctx.line, .on_exit = Ctx.exited }, null);
    p.stop(100);
    p.destroy();
    // Grace, then terminate (ignored), then kill: well under the sleep.
    try std.testing.expect(started.untilNow(env.io, .awake).toMilliseconds() < 10_000);
}
