//! Streaming tool output: turns the partial results Pi sends for a running
//! tool into deltas. Pi's bash tool sends snapshots of only the latest
//! output once it is large, so the place of each snapshot in the whole
//! output is inferred.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const agent = @import("agent");
const JsonView = agent.json.JsonView;
const decode = @import("decode.zig");

/// `Partial.seen` is bounded; a snapshot is at most Pi's 50 KB byte limit,
/// so twice that always reaches back past its start.
const max_seen = 128 << 10;

/// A tool's latest partial result: its text and, for a snapshot Pi
/// truncated, the size of the whole output so far in bytes and whether the
/// snapshot starts at a line start. `emitted` is how much of the whole
/// output the deltas covered, and `seen` the latest of those bytes, ending
/// at `emitted`.
pub const Partial = struct {
    text: []const u8 = "",
    total: i64 = 0,
    line_start: bool = false,
    emitted: i64 = 0,
    seen: []const u8 = "",
    /// Marks a snapshot after a gap whose final newline is undecided and
    /// not yet emitted; `guess` is 1 when it is likely there.
    open: bool = false,
    guess: i64 = 0,
    /// The file Pi writes the whole output to once it truncates.
    full_output: []const u8 = "",

    pub fn parse(a: Allocator, result: JsonView) Allocator.Error!Partial {
        var partial: Partial = .{ .text = try decode.resultText(a, result) };
        const details = result.get("details");
        partial.full_output = details.str("fullOutputPath");
        const truncation = details.get("truncation");
        if (!truncation.isNull()) {
            partial.total = truncation.int("totalBytes");
            // Without a cut line, Pi's snapshot starts right after a newline.
            partial.line_start = !truncation.flag("lastLinePartial");
        }
        return partial;
    }

    /// Copies the strings into `gpa`; free with `deinit`.
    pub fn dupe(partial: Partial, gpa: Allocator) Allocator.Error!Partial {
        var out = partial;
        out.text = try gpa.dupe(u8, partial.text);
        errdefer gpa.free(out.text);
        out.seen = try gpa.dupe(u8, partial.seen);
        errdefer gpa.free(out.seen);
        out.full_output = try gpa.dupe(u8, partial.full_output);
        return out;
    }

    pub fn deinit(partial: Partial, gpa: Allocator) void {
        gpa.free(partial.text);
        gpa.free(partial.seen);
        gpa.free(partial.full_output);
    }
};

fn tail(text: []const u8) []const u8 {
    return text[text.len -| max_seen..];
}

fn newlines(count: i64) []const u8 {
    return if (count > 0) "\n" else "";
}

/// The output that `cur` adds to the previous partial result. Fills cur's
/// `emitted`, `seen`, `open`, and `guess`. Strings are allocated from `a`.
///
/// A truncated snapshot usually lacks the output's final newline, which
/// the total counts, so where it sits in the whole output is found by
/// checking both placements against the output already emitted. Otherwise
/// a result that extends the previous one adds its new text, and a tool
/// that replaced its progress text has the new text follow on a line of its
/// own.
pub fn delta(a: Allocator, prev: Partial, cur: *Partial) Allocator.Error![]const u8 {
    if (cur.total == 0) {
        cur.emitted = @intCast(cur.text.len);
        cur.seen = tail(cur.text);
        if (prev.total == 0 and std.mem.startsWith(u8, cur.text, prev.text)) return cur.text[prev.text.len..];
        if (prev.text.len == 0 or cur.text.len == 0) return cur.text;
        return std.fmt.allocPrint(a, "\n{s}", .{cur.text});
    }
    if (prev.open) {
        // Settle the previous snapshot's final newline: the hypothesis
        // this snapshot fits decides it, the guess first.
        var with = prev;
        var without = prev;
        with.open = false;
        without.open = false;
        with.seen = try std.fmt.allocPrint(a, "{s}\n", .{prev.seen});
        with.emitted = prev.total;
        without.emitted = prev.total;
        const pw = place(with, cur.*);
        const po = place(without, cur.*);
        if (pw.fits and po.fits and pw.newline == 1 and po.newline == 0) {
            // Both fit and give the same bytes but for cur's own final
            // newline, the bytes lost in the gap absorbing the difference:
            // emit the shared bytes and keep that newline open.
            const d = try delta(a, without, cur);
            cur.open = true;
            cur.guess = 1;
            if (!cur.line_start and std.mem.indexOfScalar(u8, cur.text, '\n') == null) cur.guess = 0;
            cur.emitted = cur.total - cur.guess;
            return d;
        }
        var h = with;
        var other = without;
        if (prev.guess == 0) {
            h = without;
            other = with;
        }
        if (!place(h, cur.*).fits and place(other, cur.*).fits) h = other;
        const d = try delta(a, h, cur);
        if (!std.mem.eql(u8, h.seen, prev.seen)) return std.fmt.allocPrint(a, "\n{s}", .{d});
        return d;
    }
    const newline = place(prev, cur.*).newline;
    const start = cur.total - newline - @as(i64, @intCast(cur.text.len));
    const full = try std.fmt.allocPrint(a, "{s}{s}", .{ cur.text, newlines(newline) });
    var d: []const u8 = "";
    const from = prev.emitted - start;
    if (from == -1 and cur.line_start) {
        // The one byte skipped is the newline the snapshot starts after,
        // as when Pi left out a line too long for its byte limit.
        d = try std.fmt.allocPrint(a, "\n{s}", .{full});
    } else if (from < 0) {
        // Pi skipped output between the snapshots, as it does when more
        // than its limit arrives within one 100 ms update; the gap is lost.
        // Nothing anchors this snapshot's final newline, so it waits for
        // the next snapshot (or the tool's end) to decide.
        cur.emitted = cur.total - newline;
        cur.seen = cur.text;
        cur.open = true;
        cur.guess = newline;
        return cur.text;
    } else if (from < full.len) {
        d = full[@intCast(from)..];
    }
    cur.emitted = cur.total;
    cur.seen = tail(try std.fmt.allocPrint(a, "{s}{s}", .{ prev.seen, d }));
    return d;
}

/// Whether the final newline follows `cur` in the whole output, trying the
/// likelier placement first, and whether that placement fits the output
/// already emitted.
fn place(prev: Partial, cur: Partial) struct { newline: i64, fits: bool } {
    var order = [2]i64{ 1, 0 };
    if (!cur.line_start and std.mem.indexOfScalar(u8, cur.text, '\n') == null) {
        // The snapshot is the end of one cut line, most likely still being
        // written.
        order = .{ 0, 1 };
    }
    for (order) |d| {
        const start = cur.total - d - @as(i64, @intCast(cur.text.len));
        if (fitsAfter(prev, start, cur.text, d, cur.line_start)) return .{ .newline = d, .fits = true };
    }
    return .{ .newline = 0, .fits = false };
}

/// Whether `text` (plus `extra` newlines) placed at offset `start` of the
/// whole output agrees with the output already emitted where the two
/// overlap, and, for a snapshot that starts a line, whether the byte before
/// it is a newline. Without an overlap nothing contradicts it.
fn fitsAfter(prev: Partial, start: i64, text: []const u8, extra: i64, line_start: bool) bool {
    if (start < 0) return false;
    const seen = prev.seen;
    const seen_start = prev.emitted - @as(i64, @intCast(seen.len));
    if (line_start and start > seen_start and start <= prev.emitted and seen[@intCast(start - 1 - seen_start)] != '\n') return false;
    const text_len = @as(i64, @intCast(text.len)) + extra;
    const lo = @max(seen_start, start);
    const hi = @min(prev.emitted, start + text_len);
    if (hi <= lo) return true;
    var i = lo;
    while (i < hi) : (i += 1) {
        const t = i - start;
        const tc: u8 = if (t < text.len) text[@intCast(t)] else '\n';
        if (seen[@intCast(i - seen_start)] != tc) return false;
    }
    return true;
}

/// Settles the final newline left open after a gap when the tool ended:
/// from the output Pi's bash tool returns, whose tail is exact; else from
/// the last byte of Pi's full output file, which Pi completes before it
/// reports the end; else by the guess.
pub fn finalNewline(io: Io, partial: Partial, result: JsonView) bool {
    const out = result.get("structuredContent").get("output");
    if (out.v) |v| if (v == .string) return std.mem.endsWith(u8, v.string, "\n");
    if (partial.full_output.len > 0) if (lastByte(io, partial.full_output)) |b| return b == '\n';
    return partial.guess == 1;
}

fn lastByte(io: Io, path: []const u8) ?u8 {
    const dir = Io.Dir.cwd();
    // Only a regular file: opening a named pipe would block.
    const stat = dir.statFile(io, path, .{ .follow_symlinks = false }) catch return null;
    if (stat.kind != .file or stat.size == 0) return null;
    const file = dir.openFile(io, path, .{}) catch return null;
    defer file.close(io);
    var last: [1]u8 = undefined;
    const count = file.readPositionalAll(io, &last, stat.size - 1) catch return null;
    return if (count == 1) last[0] else null;
}

fn joined(a: Allocator, updates: []Partial) ![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    var prev: Partial = .{};
    for (updates) |*u| {
        try out.appendSlice(a, try delta(a, prev, u));
        prev = u.*;
    }
    return out.toOwnedSlice(a);
}

/// Pi's partial result for output `full`: all of it while short, else its
/// last `max_lines` lines joined without the final newline, with the total
/// size of `full`.
fn snapshot(a: Allocator, full: []const u8, max_lines: usize) !Partial {
    var lines: std.ArrayList([]const u8) = .empty;
    var it = std.mem.splitScalar(u8, full, '\n');
    while (it.next()) |l| try lines.append(a, l);
    if (std.mem.endsWith(u8, full, "\n")) _ = lines.pop();
    if (lines.items.len <= max_lines) return .{ .text = full };
    return .{ .text = try std.mem.join(a, "\n", lines.items[lines.items.len - max_lines ..]), .total = @intCast(full.len) };
}

test "deltas follow truncated snapshots" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const pieces = [_][]const u8{ "ok\nok\n", "ok\nhalf ", "line ü\n", "ok\nok\nok\n", "é", "è\n", "ok\n", "end\nok", "\n", "tail" };
    var full: std.ArrayList(u8) = .empty;
    var updates: std.ArrayList(Partial) = .empty;
    for (pieces) |p| {
        try full.appendSlice(a, p);
        var s = try snapshot(a, full.items, 4);
        if (s.total > 0) s.line_start = true;
        try updates.append(a, s);
    }
    try std.testing.expectEqualStrings(full.items, try joined(a, updates.items));

    var progress = [_]Partial{ .{ .text = "Parallel: 0/3 done" }, .{ .text = "Parallel: 1/3 done" }, .{ .text = "Parallel: 1/3 done, writing" } };
    try std.testing.expectEqualStrings("Parallel: 0/3 done\nParallel: 1/3 done, writing", try joined(a, &progress));
}
