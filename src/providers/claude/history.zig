//! Persisted sessions: the transcripts Claude Code writes under
//! <config dir>/projects/<project>/<session>.jsonl, read for session
//! listing, history, rewind, and fork points.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const agent = @import("agent");
const types = agent.types;
const errors = agent.errors;
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const Diagnostics = agent.Diagnostics;
const kit = agent.providerkit;
const eql = kit.eql;
const firstOf = kit.firstOf;
const parseTimeMs = kit.parseTimeMs;

const decode = @import("decode.zig");
const Provider = @import("provider.zig").Provider;

/// The longest project directory name before the SDK truncates it and
/// appends a hash.
const project_dir_limit = 200;
/// Caps session summary prompt previews.
const preview_limit = 160;
/// Caps one transcript file read.
const max_transcript_bytes = 1 << 30;
/// The file extension of a session transcript.
pub const transcript_suffix = ".jsonl";

/// `path` without the transcript extension.
pub fn withoutTranscriptSuffix(path: []const u8) []const u8 {
    return if (std.mem.endsWith(u8, path, transcript_suffix)) path[0 .. path.len - transcript_suffix.len] else path;
}

/// The directory Claude Code stores a working directory's sessions in,
/// ported from the Agent SDK: every UTF-16 code unit other than an ASCII
/// letter or digit becomes "-", and a name longer than the limit is cut and
/// suffixed with a hash of the path. The SDK's NFC normalization on macOS
/// is not applied.
pub fn projectDirName(a: Allocator, cwd: []const u8) Allocator.Error![]const u8 {
    var units: std.ArrayList(u16) = .empty;
    var it = (std.unicode.Utf8View.init(cwd) catch std.unicode.Utf8View.initUnchecked(cwd)).iterator();
    while (it.nextCodepoint()) |cp| {
        if (cp < 0x10000) {
            try units.append(a, @intCast(cp));
        } else {
            const v = cp - 0x10000;
            try units.append(a, @intCast(0xD800 + (v >> 10)));
            try units.append(a, @intCast(0xDC00 + (v & 0x3FF)));
        }
    }
    const out = try a.alloc(u8, units.items.len);
    for (units.items, out) |c, *o| o.* = if (c < 128 and std.ascii.isAlphanumeric(@intCast(c))) @intCast(c) else '-';
    if (out.len <= project_dir_limit) return out;
    // The SDK's hash: a 32-bit Java-style string hash in base 36.
    var hash: i32 = 0;
    for (units.items) |c| hash = (hash << 5) -% hash +% @as(i32, c);
    const abs: u64 = @abs(@as(i64, hash));
    var buf: [16]u8 = undefined;
    var n: usize = buf.len;
    var remaining = abs;
    while (true) {
        n -= 1;
        buf[n] = "0123456789abcdefghijklmnopqrstuvwxyz"[@intCast(remaining % 36)];
        remaining /= 36;
        if (remaining == 0) break;
    }
    return std.fmt.allocPrint(a, "{s}-{s}", .{ out[0..project_dir_limit], buf[n..] });
}

/// The directories that may hold `cwd`'s sessions: the one for the
/// symlink-resolved path the CLI uses, then the one for the path as given
/// when it differs.
pub fn projectDirs(provider: *Provider, a: Allocator, config_dir: []const u8, cwd: []const u8) Allocator.Error![]const []const u8 {
    const abs = kit.absDir(provider.env, a, cwd);
    const root = try std.fs.path.join(a, &.{ config_dir, "projects" });
    var dirs: std.ArrayList([]const u8) = .empty;
    if (Io.Dir.cwd().realPathFileAlloc(provider.env.io, abs, a)) |real| {
        if (!eql(real, abs)) try dirs.append(a, try std.fs.path.join(a, &.{ root, try projectDirName(a, real) }));
    } else |_| {}
    try dirs.append(a, try std.fs.path.join(a, &.{ root, try projectDirName(a, abs) }));
    return dirs.items;
}

/// The session transcripts in `dir` with their modification times.
fn transcriptFiles(provider: *Provider, a: Allocator, dir_path: []const u8) ![]kit.SessionFile {
    const io = provider.env.io;
    var dir = try Io.Dir.cwd().openDir(io, dir_path, .{ .iterate = true });
    defer dir.close(io);
    var files: std.ArrayList(kit.SessionFile) = .empty;
    var it = dir.iterate();
    while (try it.next(io)) |dir_entry| {
        if (dir_entry.kind == .directory or !std.mem.endsWith(u8, dir_entry.name, transcript_suffix)) continue;
        const stat = dir.statFile(io, dir_entry.name, .{}) catch continue;
        try files.append(a, .{ .path = try std.fs.path.join(a, &.{ dir_path, dir_entry.name }), .mtime_ms = @intCast(stat.mtime.toMilliseconds()) });
    }
    return files.items;
}

/// The most recently modified session of the config's working directory.
pub fn latestSessionId(provider: *Provider, a: Allocator, cfg: types.Config) ?[]const u8 {
    const config_dir = provider.configDir(a, cfg, null) catch return null;
    const dirs = projectDirs(provider, a, config_dir, cfg.cwd) catch return null;
    for (dirs) |dir| {
        const files = transcriptFiles(provider, a, dir) catch continue;
        if (files.len == 0) return null;
        var best = files[0];
        for (files[1..]) |f| if (f.mtime_ms > best.mtime_ms) {
            best = f;
        };
        return withoutTranscriptSuffix(std.fs.path.basename(best.path));
    }
    return null;
}

/// One transcript line.
pub const Entry = struct {
    /// The whole line.
    record: JsonView,
    type: []const u8,
    uuid: []const u8,
    parent_uuid: []const u8,
    logical_parent_uuid: []const u8,
    timestamp: []const u8,
    subtype: []const u8,
    effort: []const u8,
    is_compact_summary: bool,
    is_sidechain: bool,
    is_meta: bool,
    is_synthetic: bool,
    tool_use_result: JsonView,
    message: JsonView,

    fn of(record: JsonView) Entry {
        return .{
            .record = record,
            .type = record.str("type"),
            .uuid = record.str("uuid"),
            .parent_uuid = record.str("parentUuid"),
            .logical_parent_uuid = record.str("logicalParentUuid"),
            .timestamp = record.str("timestamp"),
            .subtype = record.str("subtype"),
            .effort = record.str("effort"),
            .is_compact_summary = record.flag("isCompactSummary"),
            .is_sidechain = record.flag("isSidechain"),
            .is_meta = record.flag("isMeta"),
            .is_synthetic = record.flag("isSynthetic"),
            .tool_use_result = record.get("toolUseResult"),
            .message = record.get("message"),
        };
    }
};

fn lineHead(line: []const u8) []const u8 {
    return line[0..@min(line.len, 200)];
}

fn isApiRequestLine(line: []const u8) bool {
    return std.mem.indexOf(u8, lineHead(line), "\"type\":\"api-request") != null;
}

/// Skips the large API request and attachment records without decoding.
fn relevantLine(line: []const u8) bool {
    return !isApiRequestLine(line) and std.mem.indexOf(u8, lineHead(line), "\"type\":\"attachment\"") == null;
}

pub fn notApiRequestLine(line: []const u8) bool {
    return !isApiRequestLine(line);
}

/// The entries of a transcript file that `keep` accepts, decoded into `a`.
/// Lines that do not decode are skipped.
pub fn readEntries(io: Io, a: Allocator, path: []const u8, comptime keep: fn ([]const u8) bool) ![]const Entry {
    const data = try Io.Dir.cwd().readFileAlloc(io, path, a, .limited(max_transcript_bytes));
    var out: std.ArrayList(Entry) = .empty;
    var it = std.mem.splitScalar(u8, data, '\n');
    while (it.next()) |line| {
        if (line.len == 0 or !keep(line)) continue;
        const record = JsonView.parseBytes(a, line);
        if (record.object() == null) continue;
        try out.append(a, .of(record));
    }
    return out.items;
}

// ---------------------------------------------------------------- listing

pub fn listSessions(ptr: *anyopaque, a: Allocator, opts: types.ListSessionsOptions, diag: ?*Diagnostics) errors.Error![]const types.SessionSummary {
    const provider: *Provider = @ptrCast(@alignCast(ptr));
    const io = provider.env.io;
    const config_dir = try provider.configDir(a, .{}, diag);
    const root = try std.fs.path.join(a, &.{ config_dir, "projects" });
    var dirs: std.ArrayList([]const u8) = .empty;
    if (opts.cwd.len > 0) {
        try dirs.appendSlice(a, try projectDirs(provider, a, config_dir, opts.cwd));
    } else {
        var dir = Io.Dir.cwd().openDir(io, root, .{ .iterate = true }) catch |e| switch (e) {
            error.FileNotFound => return &.{},
            else => return errors.fail(diag, .internal, "claude: list sessions: {t}", .{e}),
        };
        defer dir.close(io);
        var it = dir.iterate();
        while (it.next(io) catch null) |e| if (e.kind == .directory) try dirs.append(a, try std.fs.path.join(a, &.{ root, e.name }));
    }
    var files: std.ArrayList(kit.SessionFile) = .empty;
    for (dirs.items) |d| try files.appendSlice(a, transcriptFiles(provider, a, d) catch continue);
    return kit.pageSessionFiles(a, files.items, opts.limit, opts.offset, provider, struct {
        fn summary(listing_provider: *Provider, summary_arena: Allocator, path: []const u8) ?types.SessionSummary {
            return summarize(listing_provider.env.io, summary_arena, path);
        }
    }.summary);
}

/// The transcript entry types a session summary reads.
const SummaryEntry = enum { @"custom-title", @"ai-title", summary, user, assistant };

fn summarize(io: Io, a: Allocator, path: []const u8) ?types.SessionSummary {
    var sum: types.SessionSummary = .{ .id = withoutTranscriptSuffix(std.fs.path.basename(path)), .provider = @import("root.zig").name, .path = path };
    const entries = readEntries(io, a, path, relevantLine) catch return null;
    var first_prompt: []const u8 = "";
    var custom_title: []const u8 = "";
    var ai_title: []const u8 = "";
    var has_messages = false;
    for (entries) |entry| {
        const entry_type = std.meta.stringToEnum(SummaryEntry, entry.type) orelse continue;
        switch (entry_type) {
            .@"custom-title" => custom_title = entry.record.str("customTitle"),
            .@"ai-title" => ai_title = entry.record.str("aiTitle"),
            .summary => if (ai_title.len == 0) {
                ai_title = entry.record.str("summary");
            },
            .user, .assistant => {
                has_messages = true;
                if (sum.cwd.len == 0) sum.cwd = entry.record.str("cwd");
                const time_ms = parseTimeMs(entry.timestamp);
                if (time_ms != 0) {
                    if (sum.created_at_ms == 0) sum.created_at_ms = time_ms;
                    sum.updated_at_ms = time_ms;
                }
                if (entry.is_sidechain) continue;
                if (entry_type == .assistant) {
                    const model = entry.message.str("model");
                    if (model.len > 0 and !eql(model, "<synthetic>")) sum.model = model;
                    if (entry.effort.len > 0) sum.effort = entry.effort;
                    continue;
                }
                if (entry.is_meta or entry.is_synthetic or !entry.tool_use_result.isNull()) continue;
                const text = decode.promptText(a, entry.message) catch continue;
                if (text.len == 0) continue;
                if (first_prompt.len == 0) {
                    first_prompt = text;
                    sum.first_prompt = preview(a, text) catch "";
                }
                sum.last_prompt = preview(a, text) catch "";
            },
        }
    }
    if (!has_messages) return null;
    sum.title = firstOf(&.{ custom_title, ai_title, kit.utf8Prefix(first_prompt, 200) });
    return sum;
}

/// Collapses whitespace and cuts text to the preview limit.
fn preview(a: Allocator, text: []const u8) Allocator.Error![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    var it = std.mem.tokenizeAny(u8, text, " \t\r\n\x0b\x0c");
    while (it.next()) |w| {
        if (out.items.len > 0) try out.append(a, ' ');
        try out.appendSlice(a, w);
    }
    return kit.utf8Prefix(out.items, preview_limit);
}

/// Finds a session's transcript: in `cwd`'s project folders, else in any
/// project.
pub fn findTranscript(provider: *Provider, a: Allocator, cfg: types.Config, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) errors.Error![]const u8 {
    // Session IDs are UUIDs; anything else could name a path.
    if (!kit.isUuid(session_id)) return errors.fail(diag, .session_not_found, "claude: session ID \"{s}\" is not a UUID", .{session_id});
    const io = provider.env.io;
    const config_dir = try provider.configDir(a, cfg, diag);
    const name = try std.fmt.allocPrint(a, "{s}" ++ transcript_suffix, .{session_id});
    if (cwd.len > 0) for (try projectDirs(provider, a, config_dir, cwd)) |dir| {
        const path = try std.fs.path.join(a, &.{ dir, name });
        Io.Dir.cwd().access(io, path, .{}) catch continue;
        return path;
    };
    const root = try std.fs.path.join(a, &.{ config_dir, "projects" });
    var dir = Io.Dir.cwd().openDir(io, root, .{ .iterate = true }) catch
        return errors.fail(diag, .session_not_found, "claude: session {s} not found", .{session_id});
    defer dir.close(io);
    var it = dir.iterate();
    while (it.next(io) catch null) |dir_entry| {
        if (dir_entry.kind != .directory) continue;
        const path = try std.fs.path.join(a, &.{ root, dir_entry.name, name });
        Io.Dir.cwd().access(io, path, .{}) catch continue;
        return path;
    }
    return errors.fail(diag, .session_not_found, "claude: session {s} not found", .{session_id});
}

/// <session>/subagents/[...]/agent-<id>.jsonl next to the transcript.
fn findSubagentTranscript(provider: *Provider, a: Allocator, session_path: []const u8, agent_id: []const u8, diag: ?*Diagnostics) errors.Error![]const u8 {
    const io = provider.env.io;
    const root = try std.fs.path.join(a, &.{ withoutTranscriptSuffix(session_path), "subagents" });
    const name = try std.fmt.allocPrint(a, "agent-{s}" ++ transcript_suffix, .{agent_id});
    var dir = Io.Dir.cwd().openDir(io, root, .{ .iterate = true }) catch
        return errors.fail(diag, .not_found, "claude: subagent {s} transcript not found", .{agent_id});
    defer dir.close(io);
    var walker = try dir.walk(a);
    defer walker.deinit();
    while (walker.next(io) catch null) |dir_entry| {
        if (dir_entry.kind == .file and eql(dir_entry.basename, name)) return std.fs.path.join(a, &.{ root, dir_entry.path });
    }
    return errors.fail(diag, .not_found, "claude: subagent {s} transcript not found", .{agent_id});
}

pub fn deleteSession(ptr: *anyopaque, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) errors.Error!void {
    const provider: *Provider = @ptrCast(@alignCast(ptr));
    if (!kit.isUuid(session_id)) return errors.fail(diag, .invalid_params, "claude: invalid session ID \"{s}\"", .{session_id});
    var arena: std.heap.ArenaAllocator = .init(provider.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const path = try findTranscript(provider, a, .{}, session_id, cwd, diag);
    const io = provider.env.io;
    Io.Dir.cwd().deleteFile(io, path) catch |e| return errors.fail(diag, .internal, "claude: delete session: {t}", .{e});
    Io.Dir.cwd().deleteTree(io, withoutTranscriptSuffix(path)) catch |e| return errors.fail(diag, .internal, "claude: delete session folder: {t}", .{e});
}

// ---------------------------------------------------------------- graph

/// A transcript's chain entries, linked by parentUuid. Rewinds and in-place
/// resumes add branches, so the conversation is the chain ending at the
/// latest entry, not file order.
pub const Graph = struct {
    entries: []const Entry,
    by_id: std.StringHashMapUnmanaged(usize),

    pub fn read(io: Io, a: Allocator, path: []const u8) !Graph {
        const all = try readEntries(io, a, path, notApiRequestLine);
        var entries: std.ArrayList(Entry) = .empty;
        var by_id: std.StringHashMapUnmanaged(usize) = .empty;
        for (all) |entry| {
            if (entry.uuid.len == 0) continue;
            const gop = try by_id.getOrPut(a, entry.uuid);
            if (gop.found_existing) continue;
            gop.value_ptr.* = entries.items.len;
            try entries.append(a, entry);
        }
        return .{ .entries = entries.items, .by_id = by_id };
    }

    /// The index of entry i's parent, or null. A compact boundary has no
    /// parent but links to the conversation before it.
    pub fn parent(graph: *const Graph, i: usize) ?usize {
        const entry = graph.entries[i];
        const id = if (entry.parent_uuid.len > 0) entry.parent_uuid else entry.logical_parent_uuid;
        if (id.len == 0) return null;
        return graph.by_id.get(id);
    }

    /// Whether the entries carry parentUuid links at all.
    pub fn linked(graph: *const Graph) bool {
        for (graph.entries) |entry| if (entry.parent_uuid.len > 0 or entry.logical_parent_uuid.len > 0) return true;
        return false;
    }

    /// The latest main-conversation message.
    pub fn leaf(graph: *const Graph) ?usize {
        var i = graph.entries.len;
        while (i > 0) {
            i -= 1;
            const entry = graph.entries[i];
            if ((eql(entry.type, "user") or eql(entry.type, "assistant")) and !entry.is_sidechain) return i;
        }
        return null;
    }

    /// The entries from the root to `leaf_i`, in order.
    pub fn chain(graph: *const Graph, a: Allocator, leaf_i: usize) Allocator.Error![]const usize {
        var out: std.ArrayList(usize) = .empty;
        var cur: ?usize = leaf_i;
        while (cur) |i| {
            if (std.mem.indexOfScalar(usize, out.items, i) != null) break;
            try out.append(a, i);
            cur = graph.parent(i);
        }
        std.mem.reverse(usize, out.items);
        return out.items;
    }

    /// Whether entry i starts a turn: a user message with content of its own
    /// rather than tool results.
    pub fn isPrompt(graph: *const Graph, i: usize) bool {
        const entry = graph.entries[i];
        if (!eql(entry.type, "user") or entry.is_meta or entry.is_compact_summary or entry.is_sidechain) return false;
        const content = entry.message.get("content");
        const v = content.v orelse return false;
        if (v == .string) return v.string.len > 0;
        if (v != .array) return false;
        for (v.array.items) |b| if (!eql(JsonView.of(b).str("type"), "tool_result")) return true;
        return false;
    }

    /// Resolves a message UUID, also accepting the "uuid/n" item IDs history
    /// derives for later content blocks.
    pub fn find(graph: *const Graph, id: []const u8) ?usize {
        if (graph.by_id.get(id)) |i| return i;
        if (std.mem.indexOfScalar(u8, id, '/')) |slash| return graph.by_id.get(id[0..slash]);
        return null;
    }

    /// The prompt entry of the turn containing `id`.
    pub fn promptIndex(graph: *const Graph, a: Allocator, id: []const u8, diag: ?*Diagnostics) errors.Error!usize {
        const start = graph.find(id) orelse return errors.fail(diag, .not_found, "claude: turn {s} not found in the session transcript", .{id});
        var seen: std.ArrayList(usize) = .empty;
        var cur: ?usize = start;
        while (cur) |i| {
            if (std.mem.indexOfScalar(usize, seen.items, i) != null) break;
            try seen.append(a, i);
            if (graph.isPrompt(i)) return i;
            cur = graph.parent(i);
        }
        return errors.fail(diag, .not_found, "claude: no prompt found for {s} in the session transcript", .{id});
    }

    /// The item IDs history gives the conversation from entry i to the
    /// latest message, when i is on that conversation.
    pub fn itemIdsFrom(graph: *const Graph, a: Allocator, i: usize) Allocator.Error![]const []const u8 {
        const leaf_index = graph.leaf() orelse return &.{};
        const path = try graph.chain(a, leaf_index);
        const pos = std.mem.indexOfScalar(usize, path, i) orelse return &.{};
        var ids: std.ArrayList([]const u8) = .empty;
        for (path[pos..]) |k| {
            const entry = graph.entries[k];
            if (entry.is_sidechain) continue;
            if (eql(entry.type, "system") and eql(entry.subtype, "compact_boundary")) {
                try ids.append(a, entry.uuid);
                continue;
            }
            if (!eql(entry.type, "user") and !eql(entry.type, "assistant")) continue;
            try ids.append(a, entry.uuid);
            for (entry.message.get("content").items(), 0..) |value, n| {
                const block = JsonView.of(value);
                if (n > 0) try ids.append(a, try historyItemId(a, entry.uuid, n));
                if (block.str("id").len > 0) try ids.appendSlice(a, &.{ block.str("id"), try std.fmt.allocPrint(a, "{s}/plan", .{block.str("id")}) });
                if (block.str("tool_use_id").len > 0) try ids.append(a, try std.fmt.allocPrint(a, "{s}/plan", .{block.str("tool_use_id")}));
            }
        }
        return ids.items;
    }

    /// The entry to resume at so that the conversation is kept up to and
    /// including `id`: through the end of the turn when `id` is a prompt,
    /// otherwise through that message.
    pub fn forkPoint(graph: *const Graph, a: Allocator, id: []const u8, diag: ?*Diagnostics) errors.Error![]const u8 {
        const target = graph.find(id) orelse return errors.fail(diag, .not_found, "claude: turn {s} not found in the session transcript", .{id});
        if (!graph.isPrompt(target)) return graph.entries[target].uuid;
        // The turn continues along the latest branch below the prompt.
        var leaf_i = target;
        var i = graph.entries.len;
        while (i > target + 1) {
            i -= 1;
            const entry = graph.entries[i];
            if ((eql(entry.type, "user") or eql(entry.type, "assistant")) and !entry.is_sidechain and graph.descends(i, target)) {
                leaf_i = i;
                break;
            }
        }
        const path = try graph.chain(a, leaf_i);
        const pos = std.mem.indexOfScalar(usize, path, target).?;
        for (pos + 1..path.len) |k| if (graph.isPrompt(path[k])) return graph.entries[path[k - 1]].uuid;
        return graph.entries[path[path.len - 1]].uuid;
    }

    fn descends(graph: *const Graph, i: usize, target: usize) bool {
        var current: ?usize = i;
        var steps: usize = 0;
        while (current) |index| : (steps += 1) {
            if (index == target) return true;
            if (steps > graph.entries.len) return false;
            current = graph.parent(index);
        }
        return false;
    }
};

/// The history item of block `n` of entry `uuid`.
pub fn historyItemId(a: Allocator, uuid: []const u8, n: usize) Allocator.Error![]const u8 {
    if (n == 0) return uuid;
    return std.fmt.allocPrint(a, "{s}/{d}", .{ uuid, n });
}

/// Loads a session's transcript graph.
pub fn loadTranscript(provider: *Provider, a: Allocator, cfg: types.Config, session_id: []const u8, diag: ?*Diagnostics) errors.Error!Graph {
    const path = try findTranscript(provider, a, cfg, session_id, cfg.cwd, diag);
    return Graph.read(provider.env.io, a, path) catch |e| errors.fail(diag, .internal, "claude: read transcript {s}: {t}", .{ path, e });
}

// ---------------------------------------------------------------- history

pub fn readHistory(ptr: *anyopaque, a: Allocator, session_id: []const u8, opts: types.ReadHistoryOptions, diag: ?*Diagnostics) errors.Error![]const types.Item {
    const provider: *Provider = @ptrCast(@alignCast(ptr));
    var path = try findTranscript(provider, a, .{}, session_id, opts.cwd, diag);
    if (opts.subagent_id.len > 0) path = try findSubagentTranscript(provider, a, path, opts.subagent_id, diag);
    const graph = Graph.read(provider.env.io, a, path) catch |e| return errors.fail(diag, .internal, "claude: read transcript: {t}", .{e});
    var leaf_i = graph.leaf();
    if (opts.subagent_id.len > 0 and leaf_i == null) {
        // Subagent entries are all sidechain entries.
        var i = graph.entries.len;
        while (i > 0 and leaf_i == null) {
            i -= 1;
            if (eql(graph.entries[i].type, "user") or eql(graph.entries[i].type, "assistant")) leaf_i = i;
        }
    }
    const leaf_index = leaf_i orelse return &.{};
    var order = try graph.chain(a, leaf_index);
    if (!graph.linked()) {
        const all = try a.alloc(usize, graph.entries.len);
        for (all, 0..) |*o, i| o.* = i;
        order = all;
    }
    var builder: Builder = .{ .a = a };
    for (order) |i| {
        const entry = graph.entries[i];
        if (entry.is_sidechain and opts.subagent_id.len == 0) continue;
        if (eql(entry.type, "system") and eql(entry.subtype, "compact_boundary")) {
            try builder.compaction(entry);
            continue;
        }
        if ((!eql(entry.type, "user") and !eql(entry.type, "assistant")) or entry.is_compact_summary or entry.is_meta) continue;
        try builder.add(entry);
    }
    builder.endOrphanedCalls();
    return kit.lastPage(types.Item, builder.items.items, opts.limit, opts.offset);
}

/// The content block types history turns into items; server tool results
/// are recognized by `decode.isServerToolResult`.
const BlockType = enum { text, thinking, tool_use, server_tool_use, mcp_tool_use, tool_result };

/// Turns transcript entries into items.
const Builder = struct {
    a: Allocator,
    items: std.ArrayList(types.Item) = .empty,
    /// A tool call waiting for its result to its item index.
    tools: std.StringArrayHashMapUnmanaged(usize) = .empty,
    /// The prompt UUID of the current turn.
    turn: []const u8 = "",
    /// The task list TaskCreate, TaskUpdate, and TaskList maintain.
    plan: []const types.PlanStep = &.{},

    fn addItem(builder: *Builder, entry: Entry, item_in: types.Item) Allocator.Error!void {
        var item = item_in;
        item.turn_id = builder.turn;
        item.time_ms = parseTimeMs(entry.timestamp);
        try builder.items.append(builder.a, item);
    }

    fn compaction(builder: *Builder, entry: Entry) Allocator.Error!void {
        try builder.addItem(entry, .{ .id = entry.uuid, .kind = .compaction, .text = entry.record.get("compactMetadata").str("trigger"), .raw = try entry.record.raw(builder.a) });
    }

    fn planItem(builder: *Builder, entry: Entry, id: []const u8, steps: []const types.PlanStep) Allocator.Error!void {
        try builder.addItem(entry, .{
            .id = try std.fmt.allocPrint(builder.a, "{s}/plan", .{id}),
            .kind = .plan,
            .text = try planText(builder.a, steps),
            .raw = try RawJson.encode(builder.a, steps),
        });
    }

    fn add(builder: *Builder, entry: Entry) Allocator.Error!void {
        const a = builder.a;
        const content = entry.message.get("content");
        const content_value = content.v orelse return;
        const kind: types.ItemKind = if (eql(entry.type, "assistant")) .assistant_message else .user_message;
        const plain = content_value == .string;
        if (!plain and content_value != .array) return;
        var blocks: std.ArrayList(JsonView) = .empty;
        if (plain) {
            try blocks.append(a, .of(try agent.json.toValue(a, .{ .type = "text", .text = content_value.string })));
        } else for (content_value.array.items) |value| try blocks.append(a, JsonView.of(value));
        // toolUseResult belongs to the entry's tool result when it has one.
        var results: usize = 0;
        for (blocks.items) |block| if (eql(block.str("type"), "tool_result")) {
            results += 1;
        };
        const structured: JsonView = if (entry.tool_use_result.object() != null and results == 1) entry.tool_use_result else .empty;
        if (eql(entry.type, "user")) {
            for (blocks.items) |block| if (!eql(block.str("type"), "tool_result") and !decode.isTranscriptNoise(block.str("text"))) {
                builder.turn = entry.uuid;
                break;
            };
        }
        for (blocks.items, 0..) |block, i| {
            const id = try historyItemId(a, entry.uuid, i);
            const block_type_name = block.str("type");
            const block_type = std.meta.stringToEnum(BlockType, block_type_name) orelse {
                if (decode.isServerToolResult(block_type_name)) {
                    try builder.result(entry, block.str("tool_use_id"), block.get("content"), block.flag("is_error") or decode.isErrorContent(block.get("content")), .empty);
                }
                continue;
            };
            switch (block_type) {
                .text => try builder.text(entry, block, id, kind, plain),
                .thinking => try builder.addItem(entry, .{ .id = id, .kind = .reasoning, .text = block.str("thinking"), .raw = try block.raw(a) }),
                .tool_use, .server_tool_use, .mcp_tool_use => try builder.toolCall(entry, block),
                .tool_result => try builder.result(entry, block.str("tool_use_id"), block.get("content"), block.flag("is_error"), structured),
            }
        }
    }

    fn text(builder: *Builder, entry: Entry, block: JsonView, id: []const u8, kind: types.ItemKind, plain: bool) Allocator.Error!void {
        const a = builder.a;
        const block_text = block.str("text");
        if (block_text.len == 0 or decode.isTranscriptNoise(block_text)) return;
        var item: types.Item = .{ .id = id, .kind = kind, .text = block_text, .raw = try block.raw(a) };
        if (eql(entry.type, "user")) {
            item.text = try decode.normalizeCommand(a, block_text);
            item.synthetic = entry.is_synthetic or std.mem.startsWith(u8, std.mem.trim(u8, block_text, " \t\r\n"), "<task-notification>");
        }
        if (plain) item.raw = try entry.message.raw(a);
        try builder.addItem(entry, item);
    }

    fn toolCall(builder: *Builder, entry: Entry, block: JsonView) Allocator.Error!void {
        const a = builder.a;
        if (eql(block.str("name"), decode.handback_tool)) {
            const message = block.get("input").str("message");
            if (std.mem.trim(u8, message, " \t\r\n").len > 0) try builder.addItem(entry, .{ .id = block.str("id"), .kind = .assistant_message, .text = message, .raw = try block.raw(a) });
            return;
        }
        var call = try decode.toolCallOf(a, block, "");
        call.input = try block.get("input").raw(a);
        try decode.fillToolDetails(a, &call, block.get("input"));
        try builder.tools.put(a, block.str("id"), builder.items.items.len);
        try builder.addItem(entry, .{ .id = block.str("id"), .kind = .tool_call, .status = .in_progress, .tool = call, .raw = try block.raw(a) });
        if (eql(block.str("name"), "TodoWrite")) {
            if (try decode.todoSteps(a, block.get("input"))) |steps| try builder.planItem(entry, block.str("id"), steps);
        }
    }

    /// Completes a tool call with its result the way the live stream does.
    /// Transcripts do not record why a call did not run, so only an
    /// interrupted shell call reads as cancelled. A background Agent launch
    /// reads as done, because its completion is a later prompt.
    fn result(builder: *Builder, entry: Entry, id: []const u8, content: JsonView, is_error: bool, structured: JsonView) Allocator.Error!void {
        const pending = builder.tools.fetchOrderedRemove(id) orelse return;
        const item = &builder.items.items[pending.value];
        try decode.applyToolResult(builder.a, item, content, is_error, structured, "");
        if (item.status == .in_progress) item.status = .completed;
        if (is_error) return;
        // Like the live stream, the TaskCreate list is kept apart from
        // TodoWrite snapshots.
        const tool = item.tool.?;
        const input = if (tool.input) |raw_input| JsonView.parseBytes(builder.a, raw_input.bytes) else JsonView.empty;
        if (try decode.applyPlanTool(builder.a, builder.plan, tool.name, input, structured)) |steps| {
            builder.plan = steps;
            try builder.planItem(entry, id, steps);
        }
    }

    /// Cancels the tool calls that never got a result although a later turn
    /// started. Calls of the last turn may still be running.
    fn endOrphanedCalls(builder: *Builder) void {
        for (builder.tools.values()) |index| {
            const item = &builder.items.items[index];
            if (!eql(item.turn_id, builder.turn)) item.status = .cancelled;
        }
    }
};

/// Steps as a Markdown checklist; "[-]" marks the step in progress.
fn planText(a: Allocator, steps: []const types.PlanStep) Allocator.Error![]const u8 {
    var b: std.ArrayList(u8) = .empty;
    for (steps, 0..) |st, i| {
        const mark: u8 = switch (st.status) {
            .completed => 'x',
            .in_progress => '-',
            .pending => ' ',
        };
        if (i > 0) try b.append(a, '\n');
        try b.print(a, "- [{c}] {s}", .{ mark, st.text });
    }
    return b.items;
}

test "project directory names match the SDK" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try std.testing.expectEqualStrings("-home-u-my-proj", try projectDirName(a, "/home/u/my.proj"));
    const long = try a.alloc(u8, 251);
    @memset(long, 'a');
    long[0] = '/';
    const name = try projectDirName(a, long);
    try std.testing.expect(name.len > 200 and name[200] == '-');
}

test "transcript graph follows the latest branch" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const lines = [_][]const u8{
        "{\"type\":\"user\",\"uuid\":\"u1\",\"message\":{\"content\":\"first\"}}",
        "{\"type\":\"assistant\",\"uuid\":\"a1\",\"parentUuid\":\"u1\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"one\"}]}}",
        "{\"type\":\"user\",\"uuid\":\"u2\",\"parentUuid\":\"a1\",\"message\":{\"content\":\"second\"}}",
        "{\"type\":\"assistant\",\"uuid\":\"a2\",\"parentUuid\":\"u2\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"two\"}]}}",
    };
    var entries: [lines.len]Entry = undefined;
    var by_id: std.StringHashMapUnmanaged(usize) = .empty;
    for (lines, 0..) |l, i| {
        entries[i] = .of(JsonView.parseBytes(a, l));
        try by_id.put(a, entries[i].uuid, i);
    }
    const graph: Graph = .{ .entries = &entries, .by_id = by_id };
    try std.testing.expectEqualStrings("a1", try graph.forkPoint(a, "u1", null));
    try std.testing.expectEqual(@as(usize, 2), try graph.promptIndex(a, "a2", null));
    const ids = try graph.itemIdsFrom(a, 2);
    try std.testing.expectEqual(@as(usize, 2), ids.len);
}
