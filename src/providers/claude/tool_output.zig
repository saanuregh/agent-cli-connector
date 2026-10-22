//! Streams the output of foreground Bash calls as `tool_output_delta`
//! events (`SessionOptions.tail_shell_output`). The CLI otherwise sends no
//! output until the call ends.
const std = @import("std");
const builtin = @import("builtin");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const agent = @import("agent");
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const kit = agent.providerkit;

const control = @import("control.zig");
const Translator = @import("translate.zig").Translator;
const ClaudeSession = @import("session.zig").ClaudeSession;

const poll_interval_ms = 250;
const locate_tries = 8;
const max_read_bytes = 256 << 10;
const tmp_dir_env = "CLAUDE_CODE_TMPDIR";

/// A running tail of one tool call. Closing the session cancels it with
/// the session's other tasks; `stopTail` ends it when its call ends.
pub const Tail = struct {
    stop: Io.Event = .unset,
    done: Io.Event = .unset,

    pub fn destroy(tail: *Tail, gpa: Allocator) void {
        gpa.destroy(tail);
    }
};

const Job = struct {
    arena: std.heap.ArenaAllocator,
    tail: *Tail,
    root: []const u8,
    session_id: []const u8,
    task_id: []const u8,
    item_id: []const u8,
};

/// Follows the CLI's private output file of a foreground shell task:
/// <CLAUDE_CODE_TMPDIR or the temp dir>/claude-<uid>/<project>/<session>/tasks/<task>.output.
/// When that file cannot be found, or its root directory is missing or not
/// this user's private directory, it polls get_task_output instead.
pub fn startTail(translator: *Translator, tool_use_id: []const u8, task_id: []const u8) void {
    const claude = translator.claude;
    if (!claude.opts.tail_shell_output) return;
    const call = translator.tools.get(tool_use_id) orelse return;
    const tool = call.value.item.tool orelse return;
    if (tool.kind != .shell) return;
    const gpa = translator.gpa;
    // Without a tail the call's output still arrives when it ends.
    const job = gpa.create(Job) catch return;
    job.arena = .init(gpa);
    fillJob(job, claude, tool_use_id, task_id) catch {
        job.arena.deinit();
        gpa.destroy(job);
        return;
    };
    {
        translator.tail_mutex.lockUncancelable(claude.io());
        defer translator.tail_mutex.unlock(claude.io());
        if (translator.tails.contains(tool_use_id) or (translator.tails.put(gpa, tool_use_id, job.tail) catch null) == null) {
            job.tail.destroy(gpa);
            job.arena.deinit();
            gpa.destroy(job);
            return;
        }
    }
    claude.rt().spawn(run, .{ claude, job }) catch {
        job.tail.done.set(claude.io());
        job.arena.deinit();
        gpa.destroy(job);
    };
}

fn fillJob(job: *Job, claude: *ClaudeSession, tool_use_id: []const u8, task_id: []const u8) Allocator.Error!void {
    const a = job.arena.allocator();
    job.tail = try claude.gpa().create(Tail);
    errdefer claude.gpa().destroy(job.tail);
    job.tail.* = .{};
    job.root = shellOutputRoot(claude, a);
    job.session_id = try claude.sessionId(a);
    job.task_id = try a.dupe(u8, task_id);
    job.item_id = try a.dupe(u8, tool_use_id);
}

/// Stops the tail of a tool call and waits for it, so no delta follows the
/// call's completion.
pub fn stopTail(translator: *Translator, id: []const u8) void {
    const io = translator.claude.io();
    const tail = blk: {
        translator.tail_mutex.lockUncancelable(io);
        defer translator.tail_mutex.unlock(io);
        break :blk translator.tails.take(translator.gpa, id) orelse return;
    };
    tail.stop.set(io);
    tail.done.waitUncancelable(io);
    tail.destroy(translator.gpa);
}

/// The CLI's private temp directory, <temp root>/claude-<uid>, or "" when
/// it is missing or not ours: another user could create it first in a
/// shared temp root and plant files or symlinks in it, so it is used only
/// when it is a plain directory this user owns and others cannot open.
/// Outside Linux output is always polled.
fn shellOutputRoot(claude: *ClaudeSession, a: Allocator) []const u8 {
    if (builtin.os.tag != .linux) return "";
    const linux = std.os.linux;
    const uid = linux.getuid();
    const root = std.fmt.allocPrint(a, "{s}/claude-{d}", .{ tempDir(claude), uid }) catch return "";
    const root_path = a.dupeSentinel(u8, root, 0) catch return "";
    var st: linux.Statx = undefined;
    const rc = linux.statx(linux.AT.FDCWD, root_path, linux.AT.SYMLINK_NOFOLLOW, .{ .TYPE = true, .MODE = true, .UID = true }, &st);
    if (linux.errno(rc) != .SUCCESS) return "";
    if (st.mode & linux.S.IFMT != linux.S.IFDIR or st.uid != uid or st.mode & 0o077 != 0) {
        std.log.scoped(.agent).warn("claude: not reading shell output files: {s} is not a private directory of this user", .{root});
        return "";
    }
    return root;
}

/// The CLI's temp root: CLAUDE_CODE_TMPDIR when set, else the system temp
/// directory.
fn tempDir(claude: *ClaudeSession) []const u8 {
    if (claude.cfg.env) |e| if (e.map.get(tmp_dir_env)) |d| if (d.len > 0) return d;
    if (claude.provider.options.env) |e| if (e.map.get(tmp_dir_env)) |d| if (d.len > 0) return d;
    if (kit.getEnv(claude.env(), tmp_dir_env)) |d| if (d.len > 0) return d;
    return kit.tempRoot(claude.env());
}

/// Waits one poll interval; false once the tail should end: it was
/// stopped, its task was cancelled, or the session ended.
fn wait(claude: *ClaudeSession, tail: *Tail) bool {
    tail.stop.waitTimeout(claude.io(), kit.timeoutMs(poll_interval_ms)) catch |e| switch (e) {
        error.Timeout => return !tail.stop.isSet() and !claude.rt().isFinished(),
        error.Canceled => return false,
    };
    return false;
}

fn run(claude: *ClaudeSession, job: *Job) void {
    const gpa = claude.gpa();
    defer {
        job.tail.done.set(claude.io());
        job.arena.deinit();
        gpa.destroy(job);
    }
    const a = job.arena.allocator();
    var path: []const u8 = "";
    if (job.root.len > 0) {
        const name = std.fmt.allocPrint(a, "{s}/tasks/{s}.output", .{ job.session_id, job.task_id }) catch return;
        for (0..locate_tries) |try_n| {
            path = findOutput(claude, a, job.root, name);
            if (path.len > 0 or try_n + 1 >= locate_tries) break;
            if (!wait(claude, job.tail)) return;
        }
    }
    if (path.len == 0) return poll(claude, job);
    followFile(claude, job, path);
}

/// <root>/*/<name>, or "".
fn findOutput(claude: *ClaudeSession, a: Allocator, root: []const u8, name: []const u8) []const u8 {
    const io = claude.io();
    var dir = Io.Dir.cwd().openDir(io, root, .{ .iterate = true }) catch return "";
    defer dir.close(io);
    var it = dir.iterate();
    while (it.next(io) catch null) |dir_entry| {
        if (dir_entry.kind != .directory) continue;
        const candidate = std.fmt.allocPrint(a, "{s}/{s}/{s}", .{ root, dir_entry.name, name }) catch return "";
        Io.Dir.cwd().access(io, candidate, .{}) catch continue;
        return candidate;
    }
    return "";
}

fn followFile(claude: *ClaudeSession, job: *Job, path: []const u8) void {
    const io = claude.io();
    const gpa = claude.gpa();
    var offset: u64 = 0;
    var carry: std.ArrayList(u8) = .empty;
    defer carry.deinit(gpa);
    while (true) {
        if (Io.Dir.cwd().statFile(io, path, .{})) |stat| {
            if (stat.size < offset) {
                offset = 0;
                carry.clearRetainingCapacity();
            }
            if (stat.size > offset) {
                var start = offset;
                var prefix: []const u8 = "";
                if (stat.size - offset > max_read_bytes) {
                    start = stat.size - max_read_bytes;
                    prefix = "\n";
                    carry.clearRetainingCapacity();
                }
                readAndEmit(claude, job, path, start, stat.size - start, prefix, &carry, &offset);
            }
        } else |_| {}
        if (!wait(claude, job.tail)) return;
    }
}

fn readAndEmit(claude: *ClaudeSession, job: *Job, path: []const u8, start: u64, n: u64, prefix: []const u8, carry: *std.ArrayList(u8), offset: *u64) void {
    const io = claude.io();
    const gpa = claude.gpa();
    const file = Io.Dir.cwd().openFile(io, path, .{}) catch return;
    defer file.close(io);
    const buf = gpa.alloc(u8, @intCast(n)) catch return;
    defer gpa.free(buf);
    const got = file.readPositionalAll(io, buf, start) catch return;
    offset.* = start + got;
    carry.appendSlice(gpa, buf[0..got]) catch return;
    const data = carry.items;
    const cut = completeUtf8(data);
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const text = std.mem.concat(arena.allocator(), u8, &.{ prefix, data[0..cut] }) catch return;
    const rest = arena.allocator().dupe(u8, data[cut..]) catch return;
    carry.clearRetainingCapacity();
    // `carry` has room for `rest`, which it held a moment ago.
    carry.appendSliceAssumeCapacity(rest);
    if (text.len > 0) claude.rt().emit(.{ .payload = .{ .tool_output_delta = .{ .item_id = job.item_id, .delta = text } } });
}

/// Streams a task's output through get_task_output, which returns the last
/// 8 KiB and the total size. It stops when the CLI refuses the task.
fn poll(claude: *ClaudeSession, job: *Job) void {
    var seen: i64 = 0;
    while (true) {
        var arena: std.heap.ArenaAllocator = .init(claude.gpa());
        defer arena.deinit();
        const a = arena.allocator();
        const payload = RawJson.encode(a, .{ .task_id = job.task_id }) catch return;
        // A refusal or a stopped tail ends the polling; nobody reads why.
        const raw = control.control(claude, a, "get_task_output", payload, .{ .cancel = &job.tail.stop }, null) catch return;
        const output = JsonView.parseBytes(a, raw);
        const total = output.int("total_bytes");
        const out = output.str("output");
        if (total < seen) seen = 0;
        const fresh = total - seen;
        if (fresh > 0) {
            var text: []const u8 = out;
            if (fresh <= out.len) {
                var start = out.len - @as(usize, @intCast(fresh));
                while (start < out.len and out[start] & 0xC0 == 0x80) start += 1;
                text = out[start..];
            } else if (seen > 0) {
                text = std.mem.concat(a, u8, &.{ "\n", out }) catch return;
            }
            seen = total;
            if (text.len > 0) claude.rt().emit(.{ .payload = .{ .tool_output_delta = .{ .item_id = job.item_id, .delta = text } } });
        }
        if (!wait(claude, job.tail)) return;
    }
}

/// The length of the longest prefix of `b` that does not end inside a
/// multi-byte character.
fn completeUtf8(bytes: []const u8) usize {
    var i = bytes.len;
    while (i > 0 and bytes.len - i < 4) {
        i -= 1;
        if (bytes[i] & 0xC0 != 0x80) {
            const need = std.unicode.utf8ByteSequenceLength(bytes[i]) catch return bytes.len;
            return if (bytes.len - i >= need) bytes.len else i;
        }
    }
    return bytes.len;
}
