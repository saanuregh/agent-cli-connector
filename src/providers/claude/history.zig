//! Persisted sessions: the transcripts Claude Code writes under
//! <config dir>/projects/<project>/<session>.jsonl, read for session
//! listing, history, rewind, and fork points, and copied by fork.
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
const Value = std.json.Value;
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

// ---------------------------------------------------------------- fork

/// What `forkTranscript` copies.
pub const ForkOptions = struct {
    /// Keeps the conversation only up to and including this message UUID.
    up_to_message_id: []const u8 = "",
    /// The fork's title; empty means "<parent title> (fork)".
    title: []const u8 = "",
    /// The fork's session ID; empty means a new random one.
    session_id: []const u8 = "",
};

/// Copies a persisted session into a new one next to it and returns the new
/// session ID, so the fork exists on disk before any turn runs. A port of
/// the Agent SDK's forkSession (version `options.protocol_version`): the
/// main conversation is copied with a new UUID per message, parentUuid
/// links that skip progress entries, `forkedFrom` naming the original
/// message, and the parent's content replacements, atis latch, relocation,
/// and history suppression; the title is the parent's with " (fork)"
/// appended.
pub fn forkTranscript(provider: *Provider, a: Allocator, cfg: types.Config, parent_id: []const u8, options: ForkOptions, diag: ?*Diagnostics) errors.Error![]const u8 {
    const io = provider.env.io;
    if (options.session_id.len > 0 and !kit.isUuid(options.session_id)) return errors.fail(diag, .invalid_params, "claude: session_id must be a UUID", .{});
    const path = try findTranscript(provider, a, cfg, parent_id, cfg.cwd, diag);
    const data = Io.Dir.cwd().readFileAlloc(io, path, a, .limited(max_transcript_bytes)) catch |e|
        return errors.fail(diag, .internal, "claude: read transcript {s}: {t}", .{ path, e });
    if (data.len == 0) return errors.fail(diag, .session_not_found, "claude: session {s} not found", .{parent_id});
    const project_dir = std.fs.path.dirname(path) orelse ".";
    const sidecar = try sidecarTitle(io, a, project_dir, parent_id);
    const now = try isoTime(a, provider.env.nowMs());
    const fork = forkEntries(a, provider.env, data, parent_id, options, sidecar, now) catch |e| switch (e) {
        error.OutOfMemory => return error.OutOfMemory,
        error.NoMessages => return errors.fail(diag, .invalid_params, "claude: session {s} has no messages to fork", .{parent_id}),
        error.MessageNotFound => return errors.fail(diag, .not_found, "claude: message {s} not found in session {s}", .{ options.up_to_message_id, parent_id }),
    };
    const name = try std.fmt.allocPrint(a, "{s}" ++ transcript_suffix, .{fork.session_id});
    writeNewFile(io, project_dir, name, fork.bytes) catch |e|
        return errors.fail(diag, .internal, "claude: write fork {s}: {t}", .{ name, e });
    return fork.session_id;
}

/// Writes a file that must not exist yet, all at once: readers never see a
/// partial transcript. Only the owner can read it, as the CLI's own
/// transcripts.
fn writeNewFile(io: Io, dir_path: []const u8, name: []const u8, bytes: []const u8) !void {
    var dir = try Io.Dir.cwd().openDir(io, dir_path, .{});
    defer dir.close(io);
    const permissions: Io.File.Permissions = if (@hasDecl(Io.File.Permissions, "fromMode")) .fromMode(0o600) else .default_file;
    var file = try dir.createFileAtomic(io, name, .{ .permissions = permissions });
    defer file.deinit(io);
    try file.file.writeStreamingAll(io, bytes);
    try file.file.sync(io);
    try file.link(io);
}

/// The title the CLI keeps in <project>/<session>/custom-title.json, cleaned
/// as the SDK does; null when there is none.
fn sidecarTitle(io: Io, a: Allocator, project_dir: []const u8, session_id: []const u8) Allocator.Error!?[]const u8 {
    const path = try std.fs.path.join(a, &.{ project_dir, session_id, "custom-title.json" });
    const data = Io.Dir.cwd().readFileAlloc(io, path, a, .limited(1 << 20)) catch return null;
    const value = std.json.parseFromSliceLeaky(Value, a, data, .{}) catch return null;
    if (value != .object) return null;
    const title = value.object.get("customTitle") orelse return null;
    if (title != .string) return null;
    const cleaned = try cleanTitle(a, title.string);
    return if (cleaned.len == 0) null else cleaned;
}

/// Control and format characters become spaces, C1 controls go, and the
/// title is cut to 200 characters (the SDK's title sanitizer).
fn cleanTitle(a: Allocator, text: []const u8) Allocator.Error![]const u8 {
    var spaced: std.ArrayList(u8) = .empty;
    var in_run = false;
    var it = (std.unicode.Utf8View.init(jsTrim(text)) catch return jsTrim(text)).iterator();
    while (it.nextCodepoint()) |cp| {
        if (isControlOrFormat(cp)) {
            if (!in_run) try spaced.append(a, ' ');
            in_run = true;
            continue;
        }
        in_run = false;
        var buf: [4]u8 = undefined;
        const n = std.unicode.utf8Encode(cp, &buf) catch continue;
        try spaced.appendSlice(a, buf[0..n]);
    }
    return jsTrim(kit.truncateChars(spaced.items, 200));
}

/// Unicode categories Cc and Cf, and the line and paragraph separators.
fn isControlOrFormat(cp: u21) bool {
    return cp <= 0x1f or (cp >= 0x7f and cp <= 0x9f) or cp == 0xad or (cp >= 0x600 and cp <= 0x605) or cp == 0x61c or
        cp == 0x6dd or cp == 0x70f or cp == 0x180e or (cp >= 0x200b and cp <= 0x200f) or (cp >= 0x2028 and cp <= 0x202e) or
        (cp >= 0x2060 and cp <= 0x2064) or (cp >= 0x2066 and cp <= 0x206f) or cp == 0xfeff or (cp >= 0xfff9 and cp <= 0xfffb);
}

/// `text` without the leading and trailing white space JavaScript's trim
/// removes.
fn jsTrim(text: []const u8) []const u8 {
    var start: usize = 0;
    var end: usize = text.len;
    while (start < end) {
        const n = std.unicode.utf8ByteSequenceLength(text[start]) catch break;
        if (start + n > end) break;
        const cp = std.unicode.utf8Decode(text[start .. start + n]) catch break;
        if (!isJsSpace(cp)) break;
        start += n;
    }
    while (end > start) {
        var begin = end - 1;
        while (begin > start and text[begin] & 0xC0 == 0x80) begin -= 1;
        const cp = std.unicode.utf8Decode(text[begin..end]) catch break;
        if (!isJsSpace(cp)) break;
        end = begin;
    }
    return text[start..end];
}

fn isJsSpace(cp: u21) bool {
    return switch (cp) {
        '\t', '\n', 0x0b, 0x0c, '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff => true,
        else => cp >= 0x2000 and cp <= 0x200a,
    };
}

/// `ms` as an ISO 8601 UTC time with milliseconds, as JavaScript's
/// toISOString writes it.
fn isoTime(a: Allocator, ms: i64) Allocator.Error![]const u8 {
    const seconds: u64 = @intCast(@max(@divFloor(ms, 1000), 0));
    const epoch: std.time.epoch.EpochSeconds = .{ .secs = seconds };
    const year_day = epoch.getEpochDay().calculateYearDay();
    const month_day = year_day.calculateMonthDay();
    const day_seconds = epoch.getDaySeconds();
    return std.fmt.allocPrint(a, "{d:0>4}-{d:0>2}-{d:0>2}T{d:0>2}:{d:0>2}:{d:0>2}.{d:0>3}Z", .{
        year_day.year,                      month_day.month.numeric(),        month_day.day_index + 1,
        day_seconds.getHoursIntoDay(),      day_seconds.getMinutesIntoHour(), day_seconds.getSecondsIntoMinute(),
        @as(u64, @intCast(@mod(ms, 1000))),
    });
}

/// The fields the SDK resets on every forked message: isSidechain becomes
/// false and these go.
const fork_reset_fields = [_][]const u8{ "teamName", "agentName", "sessionKind", "slug", "sourceToolAssistantUUID", "summarizedTurnPromptUuids", "droppedPromptUuids" };

/// A transcript record of the main conversation kinds. Reads follow
/// JavaScript: null from `get` is an absent field.
const Record = struct {
    object: *const std.json.ObjectMap,
    uuid: []const u8,
    /// Reads as the SDK's copy with the fork reset fields applied.
    reset: bool = false,

    fn get(record: *const Record, key: []const u8) ?Value {
        if (record.reset) {
            if (eql(key, "isSidechain")) return .{ .bool = false };
            if (kit.contains(&fork_reset_fields, key)) return null;
        }
        return record.object.get(key);
    }

    fn str(record: *const Record, key: []const u8) []const u8 {
        const value = record.get(key) orelse return "";
        return if (value == .string) value.string else "";
    }

    fn isType(record: *const Record, record_type: []const u8) bool {
        return eql(record.str("type"), record_type);
    }

    fn isMessage(record: *const Record) bool {
        return record.isType("user") or record.isType("assistant");
    }

    /// The source_uuid of a queued_command attachment.
    fn sourceUuid(record: *const Record) []const u8 {
        if (!record.isType("attachment")) return "";
        const attachment = record.get("attachment") orelse return "";
        if (attachment != .object or !eql(stringOf(attachment.object.get("type")), "queued_command")) return "";
        return stringOf(attachment.object.get("source_uuid"));
    }

    /// The API message ID of an assistant record.
    fn messageId(record: *const Record) []const u8 {
        if (!record.isType("assistant")) return "";
        return stringOf(member(record.get("message"), "id"));
    }

    /// The `field` strings of the message content blocks of `block_type`.
    fn blockIds(record: *const Record, a: Allocator, block_type: []const u8, field: []const u8) Allocator.Error![]const []const u8 {
        const content = member(record.get("message"), "content") orelse return &.{};
        if (content != .array) return &.{};
        var out: std.ArrayList([]const u8) = .empty;
        for (content.array.items) |block| {
            if (block != .object or !eql(stringOf(block.object.get("type")), block_type)) continue;
            const id = block.object.get(field) orelse continue;
            if (id == .string) try out.append(a, id.string);
        }
        return out.items;
    }

    /// A user record answering tool calls.
    fn isToolResult(record: *const Record, parent: ?Value) bool {
        if (!record.isType("user") or !truthy(parent)) return false;
        const content = member(record.get("message"), "content") orelse return false;
        if (content != .array) return false;
        for (content.array.items) |block| if (block == .object and eql(stringOf(block.object.get("type")), "tool_result")) return true;
        return false;
    }
};

fn member(value: ?Value, key: []const u8) ?Value {
    const v = value orelse return null;
    return if (v == .object) v.object.get(key) else null;
}

fn stringOf(value: ?Value) []const u8 {
    const v = value orelse return "";
    return if (v == .string) v.string else "";
}

/// A non-empty string value, the only kind of link the SDK follows.
fn nonEmptyString(value: ?Value) ?[]const u8 {
    const v = value orelse return null;
    return if (v == .string and v.string.len > 0) v.string else null;
}

/// JavaScript truthiness of a JSON value; null is an absent field.
fn truthy(value: ?Value) bool {
    const v = value orelse return false;
    return switch (v) {
        .null => false,
        .bool => |b| b,
        .integer => |n| n != 0,
        .float => |f| f != 0 and !std.math.isNan(f),
        .number_string => |s| (std.fmt.parseFloat(f64, s) catch 1) != 0,
        .string => |s| s.len > 0,
        .array, .object => true,
    };
}

/// JavaScript `===` for JSON values; null is an absent field. Objects and
/// arrays are never equal, as distinct parsed values.
fn jsStrictEqual(x: ?Value, y: ?Value) bool {
    const left = x orelse return y == null;
    const right = y orelse return false;
    return switch (left) {
        .null => right == .null,
        .bool => |b| right == .bool and right.bool == b,
        .string => |s| right == .string and eql(right.string, s),
        .number_string => |s| right == .number_string and eql(right.number_string, s),
        .integer => |n| right == .integer and right.integer == n,
        .float => |f| right == .float and right.float == f,
        .array, .object => false,
    };
}

const ForkError = error{ OutOfMemory, NoMessages, MessageNotFound };

/// A forked transcript: its session ID and its lines.
const Fork = struct { session_id: []const u8, bytes: []const u8 };

/// What the SDK reads from a transcript to fork it.
const Parsed = struct {
    records: []const Record,
    content_replacements: std.json.Array,
    relocated_cwd: []const u8 = "",
    history_suppressed: bool = false,
    atis_latch: ?[]const u8 = null,
};

/// The record types forkSession copies.
const transcript_record_types = [_][]const u8{ "user", "assistant", "attachment", "system", "progress" };

fn parseTranscript(a: Allocator, data: []const u8, parent_id: []const u8) Allocator.Error!Parsed {
    var records: std.ArrayList(Record) = .empty;
    var parsed: Parsed = .{ .records = &.{}, .content_replacements = .init(a) };
    var lines = std.mem.splitScalar(u8, data, '\n');
    while (lines.next()) |raw_line| {
        var start: usize = 0;
        while (start < raw_line.len and raw_line[start] <= 32) start += 1;
        if (start == raw_line.len) continue;
        const value = std.json.parseFromSliceLeaky(Value, a, raw_line[start..], .{ .parse_numbers = false, .allocate = .alloc_always, .duplicate_field_behavior = .use_last }) catch |e| switch (e) {
            error.OutOfMemory => return error.OutOfMemory,
            else => continue,
        };
        if (value != .object) continue;
        const object = try a.create(std.json.ObjectMap);
        object.* = value.object;
        const record_type = stringOf(object.get("type"));
        const ours = eql(stringOf(object.get("sessionId")), parent_id);
        if (kit.contains(&transcript_record_types, record_type)) {
            const uuid = object.get("uuid") orelse continue;
            if (uuid == .string) try records.append(a, .{ .object = object, .uuid = uuid.string });
        } else if (eql(record_type, "history-suppression")) {
            parsed.history_suppressed = true;
        } else if (eql(record_type, "atis-latch") and ours) {
            const atis = object.get("atis") orelse continue;
            if (atis == .string and printableAscii(atis.string)) parsed.atis_latch = atis.string;
        } else if (eql(record_type, "content-replacement") and ours) {
            const replacements = object.get("replacements") orelse continue;
            if (replacements == .array) try parsed.content_replacements.appendSlice(replacements.array.items);
        } else if (eql(record_type, "relocated") and ours) {
            const cwd = stringOf(object.get("relocatedCwd"));
            if (cwd.len > 0) parsed.relocated_cwd = cwd;
        }
    }
    parsed.records = records.items;
    return parsed;
}

fn printableAscii(s: []const u8) bool {
    for (s) |c| if (c < 0x21 or c > 0x7e) return false;
    return true;
}

/// Forks the transcript `data` of session `parent_id`; `now` is the ISO time
/// stamped on the last message and the added records.
fn forkEntries(a: Allocator, env: agent.Env, data: []const u8, parent_id: []const u8, options: ForkOptions, sidecar_title: ?[]const u8, now: []const u8) ForkError!Fork {
    const parsed = try parseTranscript(a, data, parent_id);
    var main: std.ArrayList(*const Record) = .empty;
    for (parsed.records) |*record| if (!truthy(record.get("isSidechain"))) try main.append(a, record);
    if (main.items.len == 0) return error.NoMessages;
    var kept: []const *const Record = main.items;
    if (options.up_to_message_id.len > 0) kept = try sliceUpTo(a, env, kept, options.up_to_message_id);

    var new_ids: std.StringHashMapUnmanaged([]const u8) = .empty;
    for (kept) |record| try new_ids.put(a, record.uuid, try newUuid(a, env));
    var messages: std.ArrayList(*const Record) = .empty;
    for (kept) |record| if (!record.isType("progress")) try messages.append(a, record);
    if (messages.items.len == 0) return error.NoMessages;
    var message_ids: std.StringHashMapUnmanaged([]const u8) = .empty;
    for (messages.items) |record| try message_ids.put(a, record.uuid, new_ids.get(record.uuid).?);
    var by_uuid: std.StringHashMapUnmanaged(*const Record) = .empty;
    for (kept) |record| try by_uuid.put(a, record.uuid, record);

    const session_id = if (options.session_id.len > 0) options.session_id else try newUuid(a, env);
    var out: std.ArrayList(u8) = .empty;
    if (parsed.history_suppressed) {
        var entry: std.json.ObjectMap = .empty;
        try entry.put(a, "type", .{ .string = "history-suppression" });
        try entry.put(a, "sessionId", .{ .string = session_id });
        try entry.put(a, "cause", .{ .string = "fork_inherit" });
        try entry.put(a, "ts", .{ .string = now });
        try writeLine(a, &out, .{ .object = entry });
    }
    for (messages.items, 0..) |record, n| {
        // The parent is the nearest ancestor that is not a progress entry.
        var parent: ?[]const u8 = null;
        var next = nonEmptyString(record.get("parentUuid"));
        var seen: std.StringHashMapUnmanaged(void) = .empty;
        while (next) |id| {
            const ancestor = by_uuid.get(id) orelse break;
            if (!ancestor.isType("progress")) {
                parent = new_ids.get(id);
                break;
            }
            if (seen.contains(ancestor.uuid)) {
                parent = new_ids.get(ancestor.uuid);
                break;
            }
            try seen.put(a, ancestor.uuid, {});
            next = nonEmptyString(ancestor.get("parentUuid"));
        }
        var entry = try record.object.clone(a);
        if (record.isType("system") and eql(record.str("subtype"), "model_refusal_fallback")) try entry.put(a, "neutralizedByFork", .{ .bool = true });
        if (try deferredToolsRecord(a, record, &new_ids)) |attachment| try entry.put(a, "attachment", attachment);
        if (try remappedCompactMetadata(a, record, &message_ids)) |metadata| try entry.put(a, "compactMetadata", metadata);
        const source_uuid = record.sourceUuid();
        if (if (source_uuid.len > 0) new_ids.get(source_uuid) else null) |source| {
            var attachment = try record.get("attachment").?.object.clone(a);
            try attachment.put(a, "source_uuid", .{ .string = source });
            try entry.put(a, "attachment", .{ .object = attachment });
        }
        try entry.put(a, "uuid", .{ .string = new_ids.get(record.uuid).? });
        try entry.put(a, "parentUuid", if (parent) |p| .{ .string = p } else .null);
        if (record.get("logicalParentUuid")) |logical| {
            const mapped: ?[]const u8 = if (logical == .string) new_ids.get(logical.string) else null;
            try entry.put(a, "logicalParentUuid", if (logical == .null) .null else if (mapped) |m| .{ .string = m } else .null);
        }
        try entry.put(a, "sessionId", .{ .string = session_id });
        if (n == messages.items.len - 1) {
            try entry.put(a, "timestamp", .{ .string = now });
        }
        try entry.put(a, "isSidechain", .{ .bool = false });
        for (fork_reset_fields) |field| _ = entry.orderedRemove(field);
        var forked_from: std.json.ObjectMap = .empty;
        try forked_from.put(a, "sessionId", .{ .string = parent_id });
        try forked_from.put(a, "messageUuid", .{ .string = record.uuid });
        try entry.put(a, "forkedFrom", .{ .object = forked_from });
        try writeLine(a, &out, .{ .object = entry });
    }
    if (parsed.content_replacements.items.len > 0) {
        var entry: std.json.ObjectMap = .empty;
        try entry.put(a, "type", .{ .string = "content-replacement" });
        try entry.put(a, "sessionId", .{ .string = session_id });
        try entry.put(a, "replacements", .{ .array = parsed.content_replacements });
        try entry.put(a, "uuid", .{ .string = try newUuid(a, env) });
        try entry.put(a, "timestamp", .{ .string = now });
        try writeLine(a, &out, .{ .object = entry });
    }
    if (parsed.atis_latch) |atis| {
        var entry: std.json.ObjectMap = .empty;
        try entry.put(a, "type", .{ .string = "atis-latch" });
        try entry.put(a, "sessionId", .{ .string = session_id });
        try entry.put(a, "atis", .{ .string = atis });
        try writeLine(a, &out, .{ .object = entry });
    }
    if (parsed.relocated_cwd.len > 0) {
        var entry: std.json.ObjectMap = .empty;
        try entry.put(a, "type", .{ .string = "relocated" });
        try entry.put(a, "sessionId", .{ .string = session_id });
        try entry.put(a, "relocatedCwd", .{ .string = parsed.relocated_cwd });
        try writeLine(a, &out, .{ .object = entry });
    }
    var title = jsTrim(options.title);
    if (title.len == 0) {
        const parent_title = try transcriptTitle(a, data, sidecar_title);
        title = try std.fmt.allocPrint(a, "{s} (fork)", .{if (parent_title.len > 0) parent_title else "Forked session"});
    }
    var entry: std.json.ObjectMap = .empty;
    try entry.put(a, "type", .{ .string = "custom-title" });
    try entry.put(a, "sessionId", .{ .string = session_id });
    try entry.put(a, "customTitle", .{ .string = title });
    try entry.put(a, "uuid", .{ .string = try newUuid(a, env) });
    try entry.put(a, "timestamp", .{ .string = now });
    try writeLine(a, &out, .{ .object = entry });
    return .{ .session_id = session_id, .bytes = out.items };
}

fn newUuid(a: Allocator, env: agent.Env) Allocator.Error![]const u8 {
    const id = env.uuid();
    return a.dupe(u8, id.slice());
}

/// A deferred_tools_record attachment whose name-only announcements name
/// forked messages, with the announcements of dropped messages left out.
fn deferredToolsRecord(a: Allocator, record: *const Record, new_ids: *const std.StringHashMapUnmanaged([]const u8)) Allocator.Error!?Value {
    if (!record.isType("attachment")) return null;
    const attachment = record.get("attachment") orelse return null;
    if (attachment != .object or !eql(stringOf(attachment.object.get("type")), "deferred_tools_record")) return null;
    const announcements = attachment.object.get("nameOnlyAnnouncements") orelse return null;
    if (announcements != .array) return null;
    var mapped: std.json.Array = .init(a);
    for (announcements.array.items) |announcement| {
        if (announcement != .string) continue;
        if (new_ids.get(announcement.string)) |id| try mapped.append(.{ .string = id });
    }
    var copy = try attachment.object.clone(a);
    try copy.put(a, "nameOnlyAnnouncements", .{ .array = mapped });
    return .{ .object = copy };
}

/// A compact boundary's metadata with its preserved message and segment
/// UUIDs naming the forked messages; null when it names none.
fn remappedCompactMetadata(a: Allocator, record: *const Record, message_ids: *const std.StringHashMapUnmanaged([]const u8)) Allocator.Error!?Value {
    if (!record.isType("system")) return null;
    const metadata = record.get("compactMetadata") orelse return null;
    if (metadata != .object) return null;
    const preserved_messages = metadata.object.get("preservedMessages");
    const preserved_segment = metadata.object.get("preservedSegment");
    if (preserved_messages == null and preserved_segment == null and metadata.object.get("runningTurnSends") == null) return null;
    var out: std.json.ObjectMap = .empty;
    for (metadata.object.keys(), metadata.object.values()) |key, value| {
        if (eql(key, "preservedMessages") or eql(key, "preservedSegment") or eql(key, "runningTurnSends")) continue;
        try out.put(a, key, value);
    }
    if (preserved_messages) |value| try out.put(a, "preservedMessages", try remapFields(a, value, &.{ "anchorUuid", "uuids", "allUuids" }, message_ids));
    if (preserved_segment) |value| try out.put(a, "preservedSegment", try remapFields(a, value, &.{ "headUuid", "anchorUuid", "tailUuid" }, message_ids));
    return .{ .object = out };
}

/// `value` with each of `fields` (a UUID or a list of them) mapped to the
/// forked UUIDs; UUIDs not forked stay.
fn remapFields(a: Allocator, value: Value, fields: []const []const u8, ids: *const std.StringHashMapUnmanaged([]const u8)) Allocator.Error!Value {
    if (value != .object) return value;
    var copy = try value.object.clone(a);
    for (fields) |field| {
        const ptr = copy.getPtr(field) orelse continue;
        if (ptr.* == .array) {
            var list: std.json.Array = .init(a);
            for (ptr.array.items) |item| try list.append(remapUuid(item, ids));
            ptr.* = .{ .array = list };
        } else ptr.* = remapUuid(ptr.*, ids);
    }
    return .{ .object = copy };
}

fn remapUuid(value: Value, ids: *const std.StringHashMapUnmanaged([]const u8)) Value {
    if (value != .string) return value;
    return .{ .string = ids.get(value.string) orelse value.string };
}

/// The records up to message `up_to` (a UUID, or the source UUID of a
/// queued command), found on the conversation chain first, with compacted
/// segments the cut leaves behind dropped.
fn sliceUpTo(a: Allocator, env: agent.Env, records: []const *const Record, up_to: []const u8) ForkError![]const *const Record {
    const chain = try conversationChain(a, records, null);
    var first_index: std.StringHashMapUnmanaged(usize) = .empty;
    for (records, 0..) |record, i| {
        const gop = try first_index.getOrPut(a, record.uuid);
        if (!gop.found_existing) gop.value_ptr.* = i;
    }
    var chain_end: isize = -1;
    for (chain) |record| if (first_index.get(record.uuid)) |i| {
        chain_end = @max(chain_end, @as(isize, @intCast(i)));
    };
    var candidates: std.ArrayList(*const Record) = .empty;
    try candidates.appendSlice(a, chain);
    try candidates.appendSlice(a, records[@intCast(chain_end + 1)..]);
    const end: usize = if (findMessage(candidates.items, up_to)) |k|
        first_index.get(candidates.items[k].uuid) orelse return error.MessageNotFound
    else
        findMessage(records, up_to) orelse return error.MessageNotFound;
    return dropCompactedSegments(a, env, records[0 .. end + 1]);
}

fn findMessage(records: []const *const Record, id: []const u8) ?usize {
    for (records, 0..) |record, i| if (eql(record.uuid, id)) return i;
    for (records, 0..) |record, i| if (eql(record.sourceUuid(), id)) return i;
    return null;
}

/// The SDK's cut repair: compact boundaries (and the segments under them)
/// that the cut conversation no longer reaches are left out, until the
/// conversation chain of the slice is what it was with the cut.
fn dropCompactedSegments(a: Allocator, env: agent.Env, slice: []const *const Record) Allocator.Error![]const *const Record {
    if (slice.len == 0) return slice;
    const last = slice[slice.len - 1];
    const reset = try a.alloc(*const Record, slice.len);
    for (slice, reset) |record, *copy| {
        const r = try a.create(Record);
        r.* = record.*;
        r.reset = true;
        copy.* = r;
    }
    var sentinel_object: std.json.ObjectMap = .empty;
    const sentinel_uuid = try newUuid(a, env);
    try sentinel_object.put(a, "type", .{ .string = "system" });
    try sentinel_object.put(a, "uuid", .{ .string = sentinel_uuid });
    try sentinel_object.put(a, "parentUuid", .{ .string = last.uuid });
    if (last.get("sessionId")) |v| try sentinel_object.put(a, "sessionId", v);
    if (last.get("timestamp")) |v| try sentinel_object.put(a, "timestamp", v);
    const sentinel = try a.create(Record);
    sentinel.* = .{ .object = try copyObject(a, sentinel_object), .uuid = sentinel_uuid };
    const with_sentinel = try a.alloc(*const Record, slice.len + 1);
    @memcpy(with_sentinel[0..slice.len], reset);
    with_sentinel[slice.len] = sentinel;
    const cut_chain = try conversationChain(a, with_sentinel, null);

    var state: CutState = .{ .slice = slice, .left_out = .empty };
    for (slice, 0..) |record, i| {
        const gop = try state.index.getOrPut(a, record.uuid);
        if (!gop.found_existing) gop.value_ptr.* = i;
        try state.by_uuid.put(a, record.uuid, record);
    }
    state.start = if (cut_chain.len > 0) state.index.get(cut_chain[0].uuid) orelse return slice else return slice;
    var cut_ids: std.StringHashMapUnmanaged(void) = .empty;
    for (cut_chain) |record| try cut_ids.put(a, record.uuid, {});
    var roots: std.ArrayList([]const u8) = .empty;
    try roots.append(a, last.uuid);
    for (cut_chain) |record| try roots.append(a, record.uuid);
    for (roots.items) |root| {
        var current = state.by_uuid.get(root);
        while (current) |record| {
            if (state.live.contains(record.uuid)) break;
            try state.live.put(a, record.uuid, {});
            current = if (nonEmptyString(record.get("parentUuid"))) |p| state.by_uuid.get(p) else null;
        }
    }
    for (slice) |record| {
        if (!record.isType("system") or !eql(record.str("subtype"), "compact_boundary") or !state.droppable(record)) continue;
        if (preservesLive(record, &state.live)) continue;
        try state.leaveOut(a, record);
    }
    while (true) {
        var remaining: std.ArrayList(*const Record) = .empty;
        for (reset) |record| if (!state.left_out.contains(record.uuid)) try remaining.append(a, record);
        var found: Found = .{};
        const chain = try conversationChain(a, remaining.items, &found);
        const unchanged = chain.len == cut_ids.count() and for (chain) |record| {
            if (!cut_ids.contains(record.uuid)) break false;
        } else true;
        if (unchanged) break;
        const terminal = found.terminal orelse break;
        if (!state.droppable(terminal)) break;
        try state.leaveOut(a, terminal);
    }
    if (state.left_out.count() == 0) return slice;
    var out: std.ArrayList(*const Record) = .empty;
    for (slice) |record| if (!state.left_out.contains(record.uuid)) try out.append(a, record);
    return out.items;
}

fn copyObject(a: Allocator, object: std.json.ObjectMap) Allocator.Error!*const std.json.ObjectMap {
    const ptr = try a.create(std.json.ObjectMap);
    ptr.* = object;
    return ptr;
}

/// Whether a compact boundary preserves a message the cut conversation
/// still reaches.
fn preservesLive(record: *const Record, live: *const std.StringHashMapUnmanaged(void)) bool {
    const metadata = record.get("compactMetadata") orelse return false;
    if (metadata != .object) return false;
    if (metadata.object.get("preservedMessages")) |messages| if (messages == .object) {
        if (messages.object.get("uuids")) |uuids| if (uuids == .array) for (uuids.array.items) |id| {
            if (id == .string and live.contains(id.string)) return true;
        };
    };
    if (metadata.object.get("preservedSegment")) |segment| if (segment == .object) {
        for ([_][]const u8{ "headUuid", "tailUuid" }) |field| {
            const id = segment.object.get(field) orelse continue;
            if (id == .string and live.contains(id.string)) return true;
        }
    };
    return false;
}

/// The bookkeeping of `dropCompactedSegments`.
const CutState = struct {
    slice: []const *const Record,
    by_uuid: std.StringHashMapUnmanaged(*const Record) = .empty,
    /// First position of each UUID in the slice.
    index: std.StringHashMapUnmanaged(usize) = .empty,
    /// Messages the cut conversation reaches.
    live: std.StringHashMapUnmanaged(void) = .empty,
    /// Where the cut conversation starts in the slice.
    start: usize = 0,
    left_out: std.StringHashMapUnmanaged(void),

    /// Not reached by the cut conversation and not before its start.
    fn droppable(state: *const CutState, record: *const Record) bool {
        return !state.live.contains(record.uuid) and (state.index.get(record.uuid) orelse state.start) >= state.start;
    }

    /// Leaves out `record`, its droppable ancestors, and every later
    /// record hanging off one left out.
    fn leaveOut(state: *CutState, a: Allocator, record: *const Record) Allocator.Error!void {
        var current = state.by_uuid.get(record.uuid);
        while (current) |r| {
            if (!state.droppable(r) or state.left_out.contains(r.uuid)) break;
            try state.left_out.put(a, r.uuid, {});
            current = if (nonEmptyString(r.get("parentUuid"))) |p| state.by_uuid.get(p) else null;
        }
        for (state.slice) |r| {
            if (nonEmptyString(r.get("parentUuid"))) |p| if (state.left_out.contains(p)) try state.left_out.put(a, r.uuid, {});
        }
    }
};

/// Where a conversation chain search ended: the leaf it started from when
/// that leaf reached a message.
const Found = struct { terminal: ?*const Record = null };

/// One record in the chain search with its possibly relinked parent.
const ChainNode = struct { record: *const Record, parent: ?Value };

/// The SDK's conversation chain (its pd): compact boundaries relink their
/// preserved messages, then the chain runs from the root to the latest
/// main-conversation leaf, with the sibling records of parallel tool calls
/// added after their assistant message.
fn conversationChain(a: Allocator, input: []const *const Record, found: ?*Found) Allocator.Error![]const *const Record {
    var nodes: std.StringArrayHashMapUnmanaged(ChainNode) = .empty;
    for (input) |record| try nodes.put(a, record.uuid, .{ .record = record, .parent = record.get("parentUuid") });
    for (0..nodes.count()) |k| {
        const boundary = nodes.values()[k].record;
        if (!boundary.isType("system") or !eql(boundary.str("subtype"), "compact_boundary")) continue;
        const metadata = boundary.get("compactMetadata");
        const messages = member(metadata, "preservedMessages");
        const segment = member(metadata, "preservedSegment");
        if (truthy(messages)) {
            const uuids_value = member(messages, "uuids") orelse continue;
            if (uuids_value != .array) continue;
            const uuids = uuids_value.array.items;
            if (uuids.len == 0) continue;
            const all_present = for (uuids) |id| {
                if (id != .string or !nodes.contains(id.string)) break false;
            } else true;
            if (!all_present) continue;
            const anchor = member(messages, "anchorUuid");
            var previous: ?Value = anchor;
            for (uuids) |id| {
                nodes.getPtr(id.string).?.parent = previous;
                previous = id;
            }
            const head = uuids[0].string;
            const tail = uuids[uuids.len - 1];
            for (nodes.keys(), nodes.values()) |key, *node| {
                if (jsStrictEqual(node.parent, anchor) and !eql(key, head)) node.parent = tail;
            }
        } else if (truthy(segment)) {
            const head = member(segment, "headUuid");
            const anchor = member(segment, "anchorUuid");
            if (head) |h| if (h == .string) if (nodes.getPtr(h.string)) |node| {
                node.parent = anchor;
            };
            for (nodes.keys(), nodes.values()) |key, *node| {
                const is_head = if (head) |h| h == .string and eql(h.string, key) else false;
                if (jsStrictEqual(node.parent, anchor) and !is_head) node.parent = member(segment, "tailUuid");
            }
        }
    }
    var last_index: std.StringHashMapUnmanaged(usize) = .empty;
    for (input, 0..) |record, i| try last_index.put(a, record.uuid, i);
    const position = struct {
        fn of(map: *const std.StringHashMapUnmanaged(usize), record: *const Record) isize {
            return if (map.get(record.uuid)) |i| @intCast(i) else -1;
        }
    }.of;
    var has_children: std.StringHashMapUnmanaged(void) = .empty;
    var has_main_children: std.StringHashMapUnmanaged(void) = .empty;
    for (nodes.values()) |node| if (nonEmptyString(node.parent)) |p| {
        try has_children.put(a, p, {});
        if (isMainRecord(node.record)) try has_main_children.put(a, p, {});
    };
    var leaves: std.ArrayList(*const Record) = .empty;
    for (nodes.values()) |node| if (isMainRecord(node.record) and !has_main_children.contains(node.record.uuid)) try leaves.append(a, node.record);
    const Order = struct {
        index: *const std.StringHashMapUnmanaged(usize),
        fn later(context: @This(), x: *const Record, y: *const Record) bool {
            return position(context.index, x) > position(context.index, y);
        }
    };
    std.mem.sort(*const Record, leaves.items, Order{ .index = &last_index }, Order.later);

    var visited: std.StringHashMapUnmanaged(void) = .empty;
    var message: ?*const Record = null;
    var terminal: ?*const Record = null;
    search: for (leaves.items) |leaf| {
        var path: std.ArrayList([]const u8) = .empty;
        var on_path: std.StringHashMapUnmanaged(void) = .empty;
        var current: ?*const Record = leaf;
        while (current) |record| {
            if (visited.contains(record.uuid) or on_path.contains(record.uuid)) break;
            if (record.isMessage()) {
                message = record;
                terminal = leaf;
                break :search;
            }
            try on_path.put(a, record.uuid, {});
            try path.append(a, record.uuid);
            current = parentOf(&nodes, record);
        }
        for (path.items) |id| try visited.put(a, id, {});
    }
    var chosen = message;
    if (chosen == null) {
        var reached: std.ArrayList(*const Record) = .empty;
        for (nodes.values()) |node| {
            if (has_children.contains(node.record.uuid)) continue;
            var seen: std.StringHashMapUnmanaged(void) = .empty;
            var current: ?*const Record = node.record;
            while (current) |record| {
                if (seen.contains(record.uuid)) break;
                try seen.put(a, record.uuid, {});
                if (record.isMessage()) {
                    try reached.append(a, record);
                    break;
                }
                current = parentOf(&nodes, record);
            }
        }
        if (reached.items.len == 0) return &.{};
        var latest_main: ?*const Record = null;
        var latest: *const Record = reached.items[0];
        for (reached.items) |record| {
            if (position(&last_index, record) > position(&last_index, latest)) latest = record;
            if (truthy(record.get("isSidechain")) or truthy(record.get("teamName")) or truthy(record.get("isMeta"))) continue;
            if (latest_main == null or position(&last_index, record) > position(&last_index, latest_main.?)) latest_main = record;
        }
        chosen = latest_main orelse latest;
    }
    var chain: std.ArrayList(*const Record) = .empty;
    var on_chain: std.StringHashMapUnmanaged(void) = .empty;
    var current: ?*const Record = nodes.get(chosen.?.uuid).?.record;
    while (current) |record| {
        if (on_chain.contains(record.uuid)) break;
        try on_chain.put(a, record.uuid, {});
        try chain.append(a, record);
        current = parentOf(&nodes, record);
    }
    std.mem.reverse(*const Record, chain.items);
    if (found) |f| f.terminal = if (message != null) terminal else null;
    return addToolSiblings(a, &nodes, chain.items, &on_chain);
}

fn parentOf(nodes: *const std.StringArrayHashMapUnmanaged(ChainNode), record: *const Record) ?*const Record {
    const parent = nonEmptyString(nodes.get(record.uuid).?.parent) orelse return null;
    return if (nodes.get(parent)) |node| node.record else null;
}

/// A record that can end the main conversation.
fn isMainRecord(record: *const Record) bool {
    if (truthy(record.get("isSidechain")) or truthy(record.get("teamName")) or record.isType("progress")) return false;
    return !(record.isType("attachment") and eql(stringOf(member(record.get("attachment"), "type")), "fork_briefing"));
}

/// The SDK's repair of parallel tool calls (its Nwe): the other records of
/// each assistant message on the chain, and the tool results answering
/// them, are inserted after the message's last record on the chain.
fn addToolSiblings(
    a: Allocator,
    nodes: *const std.StringArrayHashMapUnmanaged(ChainNode),
    chain: []const *const Record,
    on_chain: *std.StringHashMapUnmanaged(void),
) Allocator.Error![]const *const Record {
    var assistants: std.ArrayList(*const Record) = .empty;
    for (chain) |record| if (record.isType("assistant")) try assistants.append(a, record);
    if (assistants.items.len == 0) return chain;
    var last_of_message: std.StringHashMapUnmanaged(*const Record) = .empty;
    for (assistants.items) |record| {
        const id = record.messageId();
        if (id.len > 0) try last_of_message.put(a, id, record);
    }
    var parts: std.StringArrayHashMapUnmanaged(std.ArrayList(*const Record)) = .empty;
    var children: std.StringHashMapUnmanaged(std.ArrayList(*const Record)) = .empty;
    var added_child: std.StringHashMapUnmanaged(void) = .empty;
    // The record that called each tool, or null when several messages did.
    var callers: std.StringHashMapUnmanaged(?*const Record) = .empty;
    var results: std.ArrayList(*const Record) = .empty;
    for (nodes.values()) |node| {
        const record = node.record;
        const id = record.messageId();
        if (id.len > 0) {
            const gop = try parts.getOrPut(a, id);
            if (!gop.found_existing) gop.value_ptr.* = .empty;
            try gop.value_ptr.append(a, record);
            for (try record.blockIds(a, "tool_use", "id")) |tool| {
                const gop_caller = try callers.getOrPut(a, tool);
                if (!gop_caller.found_existing) {
                    gop_caller.value_ptr.* = record;
                } else if (gop_caller.value_ptr.*) |caller| {
                    gop_caller.value_ptr.* = if (eql(caller.messageId(), id)) record else null;
                }
            }
        } else if (record.isToolResult(node.parent)) try results.append(a, record);
    }
    const addChild = struct {
        fn add(arena: Allocator, map: *std.StringHashMapUnmanaged(std.ArrayList(*const Record)), dedup: *std.StringHashMapUnmanaged(void), parent: []const u8, child: *const Record) Allocator.Error!void {
            const key = try std.fmt.allocPrint(arena, "{s}\n{s}", .{ parent, child.uuid });
            if (dedup.contains(key)) return;
            try dedup.put(arena, key, {});
            const gop = try map.getOrPut(arena, parent);
            if (!gop.found_existing) gop.value_ptr.* = .empty;
            try gop.value_ptr.append(arena, child);
        }
    }.add;
    const sameThread = struct {
        fn check(x: *const Record, y: *const Record) bool {
            const x_side = x.get("isSidechain") orelse Value{ .bool = false };
            const y_side = y.get("isSidechain") orelse Value{ .bool = false };
            const x_flag: Value = if (x_side == .null) .{ .bool = false } else x_side;
            const y_flag: Value = if (y_side == .null) .{ .bool = false } else y_side;
            return jsStrictEqual(x_flag, y_flag) and jsStrictEqual(x.get("agentId"), y.get("agentId"));
        }
    }.check;
    for (results.items) |record| {
        const parent = nodes.get(record.uuid).?.parent;
        if (nonEmptyString(parent)) |p| try addChild(a, &children, &added_child, p, record);
        if (record.get("sourceToolAssistantUUID")) |source| if (source == .string and !jsStrictEqual(source, parent)) {
            if (nodes.get(source.string)) |caller_node| if (sameThread(record, caller_node.record)) {
                try addChild(a, &children, &added_child, caller_node.record.uuid, record);
            };
        };
        for (try record.blockIds(a, "tool_result", "tool_use_id")) |tool| {
            const caller = (callers.get(tool) orelse continue) orelse continue;
            if (sameThread(record, caller)) try addChild(a, &children, &added_child, caller.uuid, record);
        }
    }
    var answered_on_chain: std.StringHashMapUnmanaged(void) = .empty;
    for (chain) |record| for (try record.blockIds(a, "tool_result", "tool_use_id")) |tool| try answered_on_chain.put(a, tool, {});
    var key_position: std.StringHashMapUnmanaged(usize) = .empty;
    for (nodes.keys(), 0..) |key, i| try key_position.put(a, key, i);

    var done_messages: std.StringHashMapUnmanaged(void) = .empty;
    var inserts: std.StringHashMapUnmanaged([]const *const Record) = .empty;
    var inserted: usize = 0;
    for (assistants.items) |assistant| {
        const id = assistant.messageId();
        if (id.len == 0 or done_messages.contains(id)) continue;
        try done_messages.put(a, id, {});
        const group: []const *const Record = if (parts.get(id)) |list| list.items else &.{assistant};
        var group_ids: std.StringHashMapUnmanaged(void) = .empty;
        for (group) |record| try group_ids.put(a, record.uuid, {});
        var off_chain: std.ArrayList(*const Record) = .empty;
        for (group) |record| if (!on_chain.contains(record.uuid)) try off_chain.append(a, record);
        var direct: std.ArrayList(*const Record) = .empty;
        var indirect: std.ArrayList(*const Record) = .empty;
        var taken: std.StringHashMapUnmanaged(void) = .empty;
        for (group) |record| {
            const list = children.get(record.uuid) orelse continue;
            for (list.items) |child| {
                if (on_chain.contains(child.uuid) or taken.contains(child.uuid)) continue;
                try taken.put(a, child.uuid, {});
                const child_parent = nonEmptyString(nodes.get(child.uuid).?.parent);
                if (child_parent != null and group_ids.contains(child_parent.?)) try direct.append(a, child) else try indirect.append(a, child);
            }
        }
        if (indirect.items.len > 0) {
            var answered: std.StringHashMapUnmanaged(void) = .empty;
            var it = answered_on_chain.keyIterator();
            while (it.next()) |tool| try answered.put(a, tool.*, {});
            for (direct.items) |child| for (try child.blockIds(a, "tool_result", "tool_use_id")) |tool| try answered.put(a, tool, {});
            const ByPosition = struct {
                positions: *const std.StringHashMapUnmanaged(usize),
                fn less(context: @This(), x: *const Record, y: *const Record) bool {
                    const max = std.math.maxInt(usize);
                    return (context.positions.get(x.uuid) orelse max) < (context.positions.get(y.uuid) orelse max);
                }
            };
            std.mem.sort(*const Record, indirect.items, ByPosition{ .positions = &key_position }, ByPosition.less);
            for (indirect.items) |child| {
                const tools = try child.blockIds(a, "tool_result", "tool_use_id");
                const answers_group = for (tools) |tool| {
                    if (answered.contains(tool)) continue;
                    const caller = (callers.get(tool) orelse continue) orelse continue;
                    if (group_ids.contains(caller.uuid)) break true;
                } else false;
                if (!answers_group) continue;
                for (tools) |tool| try answered.put(a, tool, {});
                try direct.append(a, child);
            }
        }
        if (off_chain.items.len == 0 and direct.items.len == 0) continue;
        const ByTime = struct {
            fn less(_: void, x: *const Record, y: *const Record) bool {
                return std.mem.order(u8, x.str("timestamp"), y.str("timestamp")) == .lt;
            }
        };
        std.mem.sort(*const Record, off_chain.items, {}, ByTime.less);
        std.mem.sort(*const Record, direct.items, {}, ByTime.less);
        var added: std.ArrayList(*const Record) = .empty;
        try added.appendSlice(a, off_chain.items);
        try added.appendSlice(a, direct.items);
        for (added.items) |record| try on_chain.put(a, record.uuid, {});
        inserted += added.items.len;
        try inserts.put(a, last_of_message.get(id).?.uuid, added.items);
    }
    if (inserted == 0) return chain;
    var out: std.ArrayList(*const Record) = .empty;
    for (chain) |record| {
        try out.append(a, record);
        if (inserts.get(record.uuid)) |added| try out.appendSlice(a, added);
    }
    return out.items;
}

// ------------------------------------------------------------ fork titles

/// The SDK reads titles from this much of the start and the end of a
/// transcript.
const title_window = 65536;

/// The parent's title as the SDK finds it: the latest custom title near the
/// end, else the sidecar title or one near the start, else an AI title,
/// else the first prompt; "" when there is none.
fn transcriptTitle(a: Allocator, data: []const u8, sidecar_title: ?[]const u8) Allocator.Error![]const u8 {
    const head = data[0..@min(data.len, title_window)];
    const tail = data[data.len -| title_window..];
    const custom = if (try latestStringField(a, tail, "customTitle")) |t| t else if (sidecar_title) |t| t else try latestStringField(a, head, "customTitle");
    if (custom) |t| if (t.len > 0) return t;
    if (try latestStringField(a, tail, "aiTitle")) |t| if (t.len > 0) return t;
    if (try latestStringField(a, head, "aiTitle")) |t| if (t.len > 0) return t;
    return firstPromptTitle(a, head);
}

/// The value of the last `"key":"..."` (or `"key": "..."`) in `text`,
/// found without decoding the JSON around it.
fn latestStringField(a: Allocator, text: []const u8, key: []const u8) Allocator.Error!?[]const u8 {
    var best: ?[]const u8 = null;
    var best_at: isize = -1;
    for ([_][]const u8{ "\":\"", "\": \"" }) |separator| {
        const pattern = try std.fmt.allocPrint(a, "\"{s}{s}", .{ key, separator });
        var from: usize = 0;
        while (std.mem.indexOfPos(u8, text, from, pattern)) |at| {
            const start = at + pattern.len;
            var i = start;
            while (i < text.len) {
                if (text[i] == '\\') {
                    i += 2;
                    continue;
                }
                if (text[i] == '"') {
                    if (@as(isize, @intCast(at)) > best_at) {
                        best = try unescape(a, text[start..i]);
                        best_at = @intCast(at);
                    }
                    break;
                }
                i += 1;
            }
            from = i + 1;
            if (from >= text.len) break;
        }
    }
    return best;
}

/// A JSON string body decoded, or as is when it does not decode.
fn unescape(a: Allocator, body: []const u8) Allocator.Error![]const u8 {
    if (std.mem.indexOfScalar(u8, body, '\\') == null) return body;
    const quoted = try std.fmt.allocPrint(a, "\"{s}\"", .{body});
    const value = std.json.parseFromSliceLeaky([]const u8, a, quoted, .{ .allocate = .alloc_always }) catch |e| switch (e) {
        error.OutOfMemory => return error.OutOfMemory,
        else => return body,
    };
    return value;
}

/// The first prompt among the user records in `head` that the SDK would
/// title a session with, else the first slash command's name.
fn firstPromptTitle(a: Allocator, head: []const u8) Allocator.Error![]const u8 {
    var command_fallback: []const u8 = "";
    var lines = std.mem.splitScalar(u8, head, '\n');
    while (lines.next()) |line| {
        if (std.mem.indexOf(u8, line, "\"type\":\"user\"") == null and std.mem.indexOf(u8, line, "\"type\": \"user\"") == null) continue;
        if (std.mem.indexOf(u8, line, "\"tool_result\"") != null) continue;
        if (std.mem.indexOf(u8, line, "\"isMeta\":true") != null or std.mem.indexOf(u8, line, "\"isMeta\": true") != null) continue;
        if (std.mem.indexOf(u8, line, "\"isCompactSummary\":true") != null or std.mem.indexOf(u8, line, "\"isCompactSummary\": true") != null) continue;
        const value = std.json.parseFromSliceLeaky(Value, a, line, .{ .parse_numbers = false, .allocate = .alloc_always, .duplicate_field_behavior = .use_last }) catch |e| switch (e) {
            error.OutOfMemory => return error.OutOfMemory,
            else => continue,
        };
        if (try promptTitle(a, value, &command_fallback)) |title| return title;
    }
    return command_fallback;
}

/// The title text of one user record, or null (the SDK's Ui).
fn promptTitle(a: Allocator, value: Value, command_fallback: *[]const u8) Allocator.Error!?[]const u8 {
    if (value != .object) return null;
    const record = value.object;
    if (!eql(stringOf(record.get("type")), "user")) return null;
    if (jsStrictEqual(record.get("isMeta"), .{ .bool = true }) or jsStrictEqual(record.get("isCompactSummary"), .{ .bool = true })) return null;
    const message = record.get("message") orelse return null;
    if (!truthy(message)) return null;
    const content = member(message, "content") orelse return null;
    var texts: std.ArrayList([]const u8) = .empty;
    switch (content) {
        .string => |s| try texts.append(a, s),
        .array => |blocks| for (blocks.items) |block| {
            if (block != .object) continue;
            const block_type = stringOf(block.object.get("type"));
            if (eql(block_type, "tool_result")) return null;
            if (eql(block_type, "text")) if (block.object.get("text")) |t| if (t == .string) try texts.append(a, t.string);
        },
        else => {},
    }
    for (texts.items) |raw_text| {
        const unwrapped = try unwrapPastedContent(a, raw_text);
        const text = jsTrim(try std.mem.replaceOwned(u8, a, unwrapped, "\n", " "));
        if (text.len == 0) continue;
        if (betweenTags(text, "<command-name>", "</command-name>", false)) |name| {
            if (command_fallback.len == 0) command_fallback.* = name;
            continue;
        }
        if (betweenTags(text, "<bash-input>", "</bash-input>", true)) |input| return try std.fmt.allocPrint(a, "! {s}", .{jsTrim(input)});
        if (startsWithTagOrInterruption(text)) continue;
        if (utf16Length(text) > 200) return try std.fmt.allocPrint(a, "{s}…", .{jsTrim(utf16Prefix(text, 200))});
        return text;
    }
    return null;
}

/// The text between the first `open` and the next `close` after it. Without
/// `any`, a line break ends the search, as "." in a JavaScript pattern.
fn betweenTags(text: []const u8, open: []const u8, close: []const u8, any: bool) ?[]const u8 {
    var from: usize = 0;
    while (std.mem.indexOfPos(u8, text, from, open)) |at| {
        const start = at + open.len;
        const end = std.mem.indexOfPos(u8, text, start, close) orelse return null;
        const between = text[start..end];
        if (any or (std.mem.indexOfScalar(u8, between, '\r') == null and std.mem.indexOf(u8, between, "\u{2028}") == null and std.mem.indexOf(u8, between, "\u{2029}") == null)) return between;
        from = at + 1;
    }
    return null;
}

/// Whether text starts with an HTML-like tag or an interruption marker,
/// which the SDK does not title sessions with.
fn startsWithTagOrInterruption(text: []const u8) bool {
    if (std.mem.startsWith(u8, text, "[Request interrupted by user")) {
        if (std.mem.indexOfScalarPos(u8, text, "[Request interrupted by user".len, ']')) |_| return true;
    }
    var i: usize = 0;
    while (i < text.len and std.ascii.isWhitespace(text[i])) i += 1;
    if (i + 2 >= text.len or text[i] != '<' or !std.ascii.isLower(text[i + 1])) return false;
    i += 2;
    while (i < text.len and (std.ascii.isAlphanumeric(text[i]) or text[i] == '_' or text[i] == '-')) i += 1;
    return i < text.len and (std.ascii.isWhitespace(text[i]) or text[i] == '>');
}

/// The number of UTF-16 code units of `text`.
fn utf16Length(text: []const u8) usize {
    var n: usize = 0;
    var it = std.unicode.Utf8View.initUnchecked(text).iterator();
    while (it.nextCodepoint()) |cp| n += if (cp >= 0x10000) 2 else 1;
    return n;
}

/// The first `units` UTF-16 code units of `text`, without a split
/// surrogate pair.
fn utf16Prefix(text: []const u8, units: usize) []const u8 {
    var n: usize = 0;
    var it = std.unicode.Utf8View.initUnchecked(text).iterator();
    while (true) {
        const start = it.i;
        const cp = it.nextCodepoint() orelse return text;
        const width: usize = if (cp >= 0x10000) 2 else 1;
        if (n + width > units) return text[0..start];
        n += width;
    }
}

/// `text` with pasted-content blocks replaced by their bodies.
fn unwrapPastedContent(a: Allocator, text: []const u8) Allocator.Error![]const u8 {
    const open = "<pasted_content id=\"";
    var out: std.ArrayList(u8) = .empty;
    var parts: usize = 0;
    var only_text = true;
    var copied: usize = 0;
    var from: usize = 0;
    while (std.mem.indexOfPos(u8, text, from, open)) |at| {
        const id_start = at + open.len;
        const id_end = id_start + 4;
        if (id_end > text.len or !isPasteId(text[id_start..id_end]) or !std.mem.startsWith(u8, text[id_end..], "\">\n")) {
            from = id_start;
            continue;
        }
        const body_start = id_end + 3;
        const close = try std.fmt.allocPrint(a, "\n</pasted_content id=\"{s}\">", .{text[id_start..id_end]});
        const close_at = std.mem.indexOfPos(u8, text, body_start - 1, close) orelse break;
        var text_end = at;
        var newlines: usize = 0;
        while (newlines < 2 and text_end > copied and text[text_end - 1] == '\n') : (newlines += 1) text_end -= 1;
        if (text_end > copied) {
            try out.appendSlice(a, text[copied..text_end]);
            parts += 1;
        }
        var next = close_at + close.len;
        newlines = 0;
        while (newlines < 2 and next < text.len and text[next] == '\n') : (newlines += 1) next += 1;
        try out.appendSlice(a, text[body_start..close_at]);
        parts += 1;
        only_text = false;
        copied = next;
        from = next;
    }
    if (copied < text.len) {
        try out.appendSlice(a, text[copied..]);
        parts += 1;
    }
    if (only_text and parts == 1) return text;
    return out.items;
}

fn isPasteId(id: []const u8) bool {
    for (id) |c| if (!std.ascii.isDigit(c) and !(c >= 'a' and c <= 'f')) return false;
    return true;
}

/// Appends `value` and a line break as JavaScript's JSON.stringify writes
/// it, so copied records keep their bytes: numbers stay as parsed
/// (`parse_numbers = false`) and strings escape the same characters.
fn writeLine(a: Allocator, out: *std.ArrayList(u8), value: Value) Allocator.Error!void {
    try out.appendSlice(a, try agent.json.encodeValue(a, value));
    try out.append(a, '\n');
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

/// A small transcript: a prompt, a progress record, a tool call answered
/// in a parallel record, a sidechain message, a team member's answer, and a
/// custom title.
const fork_fixture =
    \\{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"P","timestamp":"2026-01-01T00:00:00.000Z","isSidechain":false,"message":{"role":"user","content":"hello there"}}
    \\{"type":"progress","uuid":"p1","parentUuid":"u1","sessionId":"P","timestamp":"2026-01-01T00:00:01.000Z"}
    \\{"type":"assistant","uuid":"a1","parentUuid":"p1","sessionId":"P","timestamp":"2026-01-01T00:00:02.000Z","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}
    \\{"type":"user","uuid":"r1","parentUuid":"a1","sessionId":"P","timestamp":"2026-01-01T00:00:03.000Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok\u001b"}]},"sourceToolAssistantUUID":"a1"}
    \\{"type":"assistant","uuid":"s1","parentUuid":"r1","sessionId":"P","isSidechain":true,"message":{"id":"m9","content":[]}}
    \\{"type":"user","uuid":"u2","parentUuid":"r1","sessionId":"P","timestamp":"2026-01-01T00:00:04.000Z","message":{"content":"second"}}
    \\{"type":"assistant","uuid":"a2","parentUuid":"u2","logicalParentUuid":"a1","sessionId":"P","timestamp":"2026-01-01T00:00:05.000Z","teamName":"x","cost":1.50,"message":{"id":"m2","content":[{"type":"text","text":"done ✓"}]}}
    \\{"type":"custom-title","sessionId":"P","customTitle":"Parent title"}
    \\{"type":"content-replacement","sessionId":"P","replacements":[{"k":1}]}
    \\
;

test "fork copies the main conversation with new IDs" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const env: agent.Env = .{ .io = std.testing.io, .gpa = std.testing.allocator, .environ = std.testing.environ };
    const now = "2026-02-02T00:00:00.000Z";
    const fork = try forkEntries(a, env, fork_fixture, "P", .{}, null, now);
    var lines: std.ArrayList(JsonView) = .empty;
    var it = std.mem.splitScalar(u8, std.mem.trimEnd(u8, fork.bytes, "\n"), '\n');
    while (it.next()) |line| try lines.append(a, JsonView.parseBytes(a, line));
    // u1, a1, r1, u2, a2, the content replacements, and the title.
    try std.testing.expectEqual(@as(usize, 7), lines.items.len);
    const first = lines.items[0];
    const answer = lines.items[1];
    const last = lines.items[4];
    try std.testing.expect(kit.isUuid(fork.session_id));
    try std.testing.expectEqualStrings(fork.session_id, first.str("sessionId"));
    try std.testing.expect(first.get("parentUuid").v.? == .null);
    try std.testing.expectEqualStrings("u1", first.get("forkedFrom").str("messageUuid"));
    try std.testing.expectEqualStrings("P", first.get("forkedFrom").str("sessionId"));
    // The progress record is skipped in the parent chain.
    try std.testing.expectEqualStrings(first.str("uuid"), answer.str("parentUuid"));
    try std.testing.expectEqualStrings(answer.str("uuid"), last.get("logicalParentUuid").string());
    try std.testing.expect(!last.has("teamName"));
    try std.testing.expectEqualStrings(now, last.str("timestamp"));
    try std.testing.expectEqualStrings("2026-01-01T00:00:00.000Z", first.str("timestamp"));
    try std.testing.expectEqualStrings("content-replacement", lines.items[5].str("type"));
    try std.testing.expectEqualStrings("Parent title (fork)", lines.items[6].str("customTitle"));
    // Copied values keep their bytes.
    try std.testing.expect(std.mem.indexOf(u8, fork.bytes, "\"cost\":1.50,") != null);
    try std.testing.expect(std.mem.indexOf(u8, fork.bytes, "ok\\u001b") != null);
    try std.testing.expect(std.mem.indexOf(u8, fork.bytes, "done ✓") != null);

    const cut = try forkEntries(a, env, fork_fixture, "P", .{ .up_to_message_id = "u2", .title = " Mine " }, null, now);
    try std.testing.expectEqual(@as(usize, 6), std.mem.count(u8, cut.bytes, "\n"));
    try std.testing.expect(std.mem.indexOf(u8, cut.bytes, "\"customTitle\":\"Mine\"") != null);
    try std.testing.expectError(error.MessageNotFound, forkEntries(a, env, fork_fixture, "P", .{ .up_to_message_id = "missing" }, null, now));
}

test "fork titles fall back to the first prompt" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const transcript = "{\"type\":\"user\",\"uuid\":\"u1\",\"message\":{\"content\":\"<command-name>/x</command-name>\"}}\n" ++
        "{\"type\":\"user\",\"uuid\":\"u2\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"fix\\nthe bug \"}]}}\n";
    try std.testing.expectEqualStrings("fix the bug", try transcriptTitle(a, transcript, null));
    try std.testing.expectEqualStrings("Side", try transcriptTitle(a, transcript, "Side"));
    try std.testing.expectEqualStrings("/x", try transcriptTitle(a, transcript[0 .. std.mem.indexOfScalar(u8, transcript, '\n').? + 1], null));
}

test "fork times are written as JavaScript writes them" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    try std.testing.expectEqualStrings("1970-01-01T00:00:00.000Z", try isoTime(arena.allocator(), 0));
    try std.testing.expectEqualStrings("2026-10-23T09:05:07.042Z", try isoTime(arena.allocator(), 1792746307042));
}

test "fork writes the transcript next to the parent's" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const root = try tmp.dir.realPathFileAlloc(io, ".", a);
    const cwd = "/work/project";
    const parent_id = "0b4c3f0e-1d2a-4b5c-8d9e-0f1a2b3c4d5e";
    const project = try std.fs.path.join(a, &.{ "projects", try projectDirName(a, cwd) });
    try tmp.dir.createDirPath(io, project);
    const fixture = try std.mem.replaceOwned(u8, a, fork_fixture, "\"P\"", "\"" ++ parent_id ++ "\"");
    try tmp.dir.writeFile(io, .{ .sub_path = try std.fs.path.join(a, &.{ project, parent_id ++ ".jsonl" }), .data = fixture });

    const env: agent.Env = .{ .io = io, .gpa = gpa, .environ = std.testing.environ };
    const created = try @import("provider.zig").create(env, .{});
    defer created.deinit();
    const provider: *Provider = @ptrCast(@alignCast(created.ptr));
    var config_env: types.StringMap = .{};
    try config_env.map.put(a, "CLAUDE_CONFIG_DIR", root);
    const cfg: types.Config = .{ .cwd = cwd, .env = config_env };
    const fork_id = try forkTranscript(provider, a, cfg, parent_id, .{}, null);
    const path = try findTranscript(provider, a, cfg, fork_id, cwd, null);
    try std.testing.expectEqualStrings(project, std.fs.path.dirname(path).?[root.len + 1 ..]);
    const written = try Io.Dir.cwd().readFileAlloc(io, path, a, .limited(1 << 20));
    try std.testing.expectEqual(@as(usize, 7), std.mem.count(u8, written, "\n"));
    try std.testing.expectError(error.SessionNotFound, forkTranscript(provider, a, cfg, "1b4c3f0e-1d2a-4b5c-8d9e-0f1a2b3c4d5e", .{}, null));
}

// Forks for the differential check against the SDK: with
// ACC_FORK_DIFF_DIR set to a Claude config directory, forks the session
// ACC_FORK_DIFF_SESSION of working directory ACC_FORK_DIFF_CWD (up to
// ACC_FORK_DIFF_UP_TO when set) and writes the fork's ID to
// <dir>/fork-id.txt.
test "fork differential entry" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    const env: agent.Env = .{ .io = io, .gpa = gpa, .environ = std.testing.environ };
    const dir = kit.getEnv(env, "ACC_FORK_DIFF_DIR") orelse return error.SkipZigTest;
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const created = try @import("provider.zig").create(env, .{});
    defer created.deinit();
    const provider: *Provider = @ptrCast(@alignCast(created.ptr));
    var config_env: types.StringMap = .{};
    try config_env.map.put(a, "CLAUDE_CONFIG_DIR", dir);
    const cfg: types.Config = .{ .cwd = kit.getEnv(env, "ACC_FORK_DIFF_CWD") orelse "", .env = config_env };
    const fork_id = try forkTranscript(provider, a, cfg, kit.getEnv(env, "ACC_FORK_DIFF_SESSION") orelse "", .{
        .up_to_message_id = kit.getEnv(env, "ACC_FORK_DIFF_UP_TO") orelse "",
    }, null);
    try Io.Dir.cwd().writeFile(io, .{ .sub_path = try std.fs.path.join(a, &.{ dir, "fork-id.txt" }), .data = fork_id });
}
