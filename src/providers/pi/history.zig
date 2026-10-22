//! Pi's persisted sessions: where they are stored, listing, reading the
//! active branch as transcript items, subagent tasks, and deletion.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const agent = @import("agent");
const types = agent.types;
const errors = agent.errors;
const kit = agent.providerkit;
const JsonView = agent.json.JsonView;
const Diagnostics = errors.Diagnostics;
const eql = kit.eql;
const decode = @import("decode.zig");

const name = @import("root.zig").name;

/// Finds sessions where a Pi process stores them.
pub const Store = struct {
    env: agent.Env,
    /// The configured session directory; empty means Pi's own resolution.
    session_dir: []const u8 = "",
    /// Environment variables the Pi process gets on top of the inherited
    /// ones.
    extra_env: ?types.StringMap = null,

    fn getenv(store: Store, key: []const u8) []const u8 {
        if (store.extra_env) |m| if (m.map.get(key)) |v| if (v.len > 0) return v;
        return kit.getEnv(store.env, key) orelse "";
    }

    fn expandHome(store: Store, a: Allocator, path: []const u8) []const u8 {
        if (!eql(path, "~") and !std.mem.startsWith(u8, path, "~/") and !std.mem.startsWith(u8, path, "~\\")) return path;
        const home = kit.homeDir(store.env, null) catch return path;
        return std.fs.path.join(a, &.{ home, path[1..] }) catch path;
    }

    /// Pi's agent directory.
    fn agentDir(store: Store, a: Allocator, diag: ?*Diagnostics) errors.Error![]const u8 {
        const dir = store.getenv("PI_CODING_AGENT_DIR");
        if (dir.len > 0) return store.expandHome(a, dir);
        return std.fs.path.join(a, &.{ try kit.homeDir(store.env, diag), ".pi", "agent" });
    }

    /// The directory Pi stores `cwd`'s sessions under, in Pi's order of
    /// precedence. `per_cwd` reports Pi's default layout, which keeps one
    /// subdirectory per working directory; a configured directory holds
    /// the sessions of every working directory.
    pub fn sessionsDir(store: Store, a: Allocator, cwd: []const u8, diag: ?*Diagnostics) errors.Error!struct { dir: []const u8, per_cwd: bool } {
        const agent_dir = try store.agentDir(a, diag);
        const candidates = [_][]const u8{
            store.session_dir,
            store.getenv("PI_CODING_AGENT_SESSION_DIR"),
            settingsSessionDir(store.env, a, try std.fs.path.join(a, &.{ cwd, ".pi", "settings.json" })),
            settingsSessionDir(store.env, a, try std.fs.path.join(a, &.{ agent_dir, "settings.json" })),
        };
        for (candidates) |candidate| {
            if (candidate.len == 0) continue;
            var dir = store.expandHome(a, candidate);
            if (!std.fs.path.isAbsolute(dir)) dir = try std.fs.path.join(a, &.{ cwd, dir });
            // Pi treats any configured directory as flat, even the default.
            return .{ .dir = try std.fs.path.resolve(a, &.{dir}), .per_cwd = false };
        }
        return .{ .dir = try std.fs.path.join(a, &.{ agent_dir, "sessions" }), .per_cwd = true };
    }

    /// The session file for a session ID or path.
    pub fn findSession(store: Store, a: Allocator, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) errors.Error![]const u8 {
        const io = store.env.io;
        if (isSessionPath(session_id)) {
            if (Io.Dir.cwd().statFile(io, session_id, .{})) |stat| {
                if (stat.kind != .directory) return session_id;
            } else |_| {}
        }
        const abs = kit.absDir(store.env, a, cwd);
        const where = store.sessionsDir(a, abs, diag) catch |e| return errors.fail(diag, .internal, "pi: session directory: {t}", .{e});
        // Pi names files "<timestamp>_<id>.jsonl" and IDs may contain "_",
        // so the name only narrows the search: the header must carry the ID.
        const suffix = try std.fmt.allocPrint(a, "_{s}.jsonl", .{session_id});
        if (cwd.len > 0 and where.per_cwd) {
            const dir = try std.fs.path.join(a, &.{ where.dir, try cwdDirName(a, abs) });
            if (Io.Dir.cwd().openDir(io, dir, .{ .iterate = true })) |opened| {
                var cwd_dir = opened;
                defer cwd_dir.close(io);
                var it = cwd_dir.iterate();
                while (it.next(io) catch null) |entry| {
                    if (!std.mem.endsWith(u8, entry.name, suffix)) continue;
                    const path = try std.fs.path.join(a, &.{ dir, entry.name });
                    if (headerMatches(store.env, a, path, session_id)) return path;
                }
            } else |_| {}
        }
        for (try sessionFiles(store.env, a, where.dir)) |file| {
            if (std.mem.endsWith(u8, std.fs.path.basename(file.path), suffix) and headerMatches(store.env, a, file.path, session_id)) return file.path;
        }
        return errors.fail(diag, .session_not_found, "pi: session {s} not found", .{session_id});
    }

    pub fn listSessions(store: Store, a: Allocator, opts: types.ListSessionsOptions, diag: ?*Diagnostics) errors.Error![]const types.SessionSummary {
        const cwd = kit.absDir(store.env, a, opts.cwd);
        const where = store.sessionsDir(a, cwd, diag) catch |e| return errors.fail(diag, .internal, "pi: session directory: {t}", .{e});
        var scan = where.dir;
        if (opts.cwd.len > 0 and where.per_cwd) scan = try std.fs.path.join(a, &.{ where.dir, try cwdDirName(a, cwd) });
        const files = try sessionFiles(store.env, a, scan);
        const SessionFilter = struct {
            env: agent.Env,
            cwd: []const u8,
            filter: bool,
            fn summary(context: @This(), out: Allocator, path: []const u8) ?types.SessionSummary {
                const found = summarize(context.env, out, path) orelse return null;
                if (context.filter and !sameDir(context.env, out, found.cwd, context.cwd)) return null;
                return found;
            }
        };
        return kit.pageSessionFiles(a, files, opts.limit, opts.offset, SessionFilter{ .env = store.env, .cwd = cwd, .filter = opts.cwd.len > 0 and !where.per_cwd }, SessionFilter.summary);
    }

    /// The active branch of a persisted session as items. Items carry the
    /// user entry ID that started their turn as `turn_id`, which rewind and
    /// fork_at_turn accept. With `subagent_id`, reads the session file a
    /// subagent extension recorded for that task.
    pub fn readHistory(store: Store, a: Allocator, session_id: []const u8, opts: types.ReadHistoryOptions, diag: ?*Diagnostics) errors.Error![]const types.Item {
        const path = try store.findSession(a, session_id, opts.cwd, diag);
        var chain = try readBranch(store.env, a, path, diag);
        if (opts.subagent_id.len > 0) {
            const found = try branchSubagents(a, chain);
            const file = for (found.files) |f| {
                if (eql(f.task_id, opts.subagent_id)) break f.path;
            } else return errors.fail(diag, .not_found, "pi: subagent {s} of session {s} has no session file", .{ opts.subagent_id, session_id });
            const full = if (std.fs.path.isAbsolute(file)) file else try std.fs.path.join(a, &.{ std.fs.path.dirname(path) orelse ".", file });
            chain = try readBranch(store.env, a, full, diag);
        }
        return kit.lastPage(types.Item, try historyItems(a, chain), opts.limit, opts.offset);
    }

    /// The subagent tasks that "subagent" tool calls on the session's
    /// active branch report.
    pub fn listSubagents(store: Store, a: Allocator, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) errors.Error![]const types.Task {
        const path = try store.findSession(a, session_id, cwd, diag);
        return (try branchSubagents(a, try readBranch(store.env, a, path, diag))).tasks;
    }

    /// Removes a persisted session's file. `session_id` may also be a
    /// session file path. Subagent session files are kept.
    pub fn deleteSession(store: Store, a: Allocator, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) errors.Error!void {
        // IDs become name patterns; paths are used as they are.
        if (session_id.len == 0 or (!isSessionPath(session_id) and std.mem.indexOfAny(u8, session_id, "*?[") != null))
            return errors.fail(diag, .invalid_params, "pi: invalid session ID \"{s}\"", .{session_id});
        const path = try store.findSession(a, session_id, cwd, diag);
        // A path ID could name any file; only remove one that starts with a
        // Pi session header.
        if (!eql(std.fs.path.extension(path), ".jsonl") or readHeaderId(store.env, a, path).len == 0)
            return errors.fail(diag, .invalid_params, "pi: {s} is not a Pi session file", .{path});
        Io.Dir.cwd().deleteFile(store.env.io, path) catch |e| return errors.fail(diag, .internal, "pi: delete session: {t}", .{e});
    }
};

/// Reads the sessionDir setting of a Pi settings file.
fn settingsSessionDir(env: agent.Env, a: Allocator, path: []const u8) []const u8 {
    const data = Io.Dir.cwd().readFileAlloc(env.io, path, a, .limited(1 << 20)) catch return "";
    return JsonView.parseBytes(a, data).str("sessionDir");
}

/// The directory Pi stores a working directory's sessions in.
pub fn cwdDirName(a: Allocator, cwd: []const u8) ![]const u8 {
    const trimmed = std.mem.trimStart(u8, cwd, "/\\");
    const out = try std.fmt.allocPrint(a, "--{s}--", .{trimmed});
    for (out[2 .. out.len - 2]) |*c| if (c.* == '/' or c.* == '\\' or c.* == ':') {
        c.* = '-';
    };
    return out;
}

fn sameDir(env: agent.Env, a: Allocator, first: []const u8, second: []const u8) bool {
    const first_resolved = std.fs.path.resolve(a, &.{first}) catch first;
    const second_resolved = std.fs.path.resolve(a, &.{second}) catch second;
    if (eql(first_resolved, second_resolved)) return true;
    const first_real = Io.Dir.cwd().realPathFileAlloc(env.io, first, a) catch return false;
    const second_real = Io.Dir.cwd().realPathFileAlloc(env.io, second, a) catch return false;
    return eql(first_real, second_real);
}

/// Whether a session ID is a session file path rather than an ID.
pub fn isSessionPath(id: []const u8) bool {
    return std.mem.endsWith(u8, id, ".jsonl") or std.mem.indexOfAny(u8, id, "/\\") != null;
}

/// The session files under `root`, at any depth.
fn sessionFiles(env: agent.Env, a: Allocator, root: []const u8) Allocator.Error![]kit.SessionFile {
    const io = env.io;
    var out: std.ArrayList(kit.SessionFile) = .empty;
    var dir = Io.Dir.cwd().openDir(io, root, .{ .iterate = true }) catch return out.toOwnedSlice(a);
    defer dir.close(io);
    var walker = try dir.walk(a);
    defer walker.deinit();
    while (walker.next(io) catch null) |entry| {
        if (entry.kind != .file or !std.mem.endsWith(u8, entry.basename, ".jsonl")) continue;
        const stat = entry.dir.statFile(io, entry.basename, .{}) catch continue;
        try out.append(a, .{ .path = try std.fs.path.join(a, &.{ root, entry.path }), .mtime_ms = stat.mtime.toMilliseconds() });
    }
    return out.toOwnedSlice(a);
}

/// Calls `onLine` with each line of a file until it returns false.
fn eachLine(env: agent.Env, path: []const u8, context: anytype, comptime onLine: fn (@TypeOf(context), []const u8) bool) bool {
    const io = env.io;
    const file = Io.Dir.cwd().openFile(io, path, .{}) catch return false;
    defer file.close(io);
    var buf: [64 * 1024]u8 = undefined;
    var file_reader = file.readerStreaming(io, &buf);
    const reader = &file_reader.interface;
    var line: Io.Writer.Allocating = .init(env.gpa);
    defer line.deinit();
    while (true) {
        line.clearRetainingCapacity();
        const end = blk: {
            _ = reader.streamDelimiter(&line.writer, '\n') catch break :blk true;
            reader.toss(1);
            break :blk false;
        };
        if (line.written().len > 0 and !onLine(context, line.written())) return true;
        if (end) return true;
    }
}

/// The ID in the session header on the first line of `path`, or "".
fn readHeaderId(env: agent.Env, a: Allocator, path: []const u8) []const u8 {
    return readHeader(env, a, path).str("id");
}

/// The session header on the first line of `path`, or empty.
pub fn readHeader(env: agent.Env, a: Allocator, path: []const u8) JsonView {
    const Header = struct {
        a: Allocator,
        out: *JsonView,
        fn first(header: @This(), line: []const u8) bool {
            const entry = JsonView.parseBytes(header.a, line);
            if (eql(entry.str("type"), "session")) header.out.* = entry;
            return false;
        }
    };
    var out: JsonView = .empty;
    _ = eachLine(env, path, Header{ .a = a, .out = &out }, Header.first);
    return out;
}

fn headerMatches(env: agent.Env, a: Allocator, path: []const u8, id: []const u8) bool {
    return eql(readHeaderId(env, a, path), id);
}

/// The session entry types a summary reads.
const SummaryEntry = enum { session, session_info, model_change, thinking_level_change, message };

/// Builds a session summary from the lines of a session file.
const Summarizer = struct {
    keep: Allocator,
    scratch: *std.heap.ArenaAllocator,
    summary: *types.SessionSummary,
    title: *[]const u8,

    fn line(summarizer: Summarizer, text: []const u8) bool {
        defer _ = summarizer.scratch.reset(.retain_capacity);
        const a = summarizer.scratch.allocator();
        const entry = JsonView.parseBytes(a, text);
        if (entry.object() == null) return true;
        // Only allocation fails here; the summary then lacks this entry.
        summarizer.add(a, entry) catch {};
        return true;
    }

    fn add(summarizer: Summarizer, a: Allocator, entry: JsonView) Allocator.Error!void {
        const keep = summarizer.keep;
        const summary = summarizer.summary;
        const timestamp = kit.parseTimeMs(entry.str("timestamp"));
        if (std.meta.stringToEnum(SummaryEntry, entry.str("type"))) |kind| switch (kind) {
            .session => {
                summary.id = try keep.dupe(u8, entry.str("id"));
                summary.cwd = try keep.dupe(u8, entry.str("cwd"));
                summary.created_at_ms = timestamp;
            },
            .session_info => summarizer.title.* = try keep.dupe(u8, entry.str("name")),
            .model_change => if (entry.str("modelId").len > 0) {
                summary.model = try keep.dupe(u8, try decode.modelName(a, entry.str("provider"), entry.str("modelId")));
            },
            .thinking_level_change => if (entry.str("thinkingLevel").len > 0) {
                summary.effort = try keep.dupe(u8, decode.effortOf(entry.str("thinkingLevel")));
            },
            .message => try summarizer.message(a, entry.get("message")),
        };
        if (timestamp != 0) summary.updated_at_ms = timestamp;
    }

    fn message(summarizer: Summarizer, a: Allocator, fields: JsonView) Allocator.Error!void {
        const keep = summarizer.keep;
        const summary = summarizer.summary;
        const role = fields.str("role");
        if (eql(role, "user")) {
            const text = try decode.messageText(a, fields.get("content"));
            if (text.len > 0) {
                // A session summary shows the first 200 characters.
                const prompt = try keep.dupe(u8, kit.truncateChars(text, 200));
                if (summary.first_prompt.len == 0) summary.first_prompt = prompt;
                summary.last_prompt = prompt;
            }
        } else if (eql(role, "assistant") and fields.str("model").len > 0) {
            summary.model = try keep.dupe(u8, try decode.modelName(a, fields.str("provider"), fields.str("model")));
        }
    }
};

fn summarize(env: agent.Env, out: Allocator, path: []const u8) ?types.SessionSummary {
    var keep: std.heap.ArenaAllocator = .init(env.gpa);
    defer keep.deinit();
    var scratch: std.heap.ArenaAllocator = .init(env.gpa);
    defer scratch.deinit();
    var summary: types.SessionSummary = .{ .provider = name, .path = path };
    var title: []const u8 = "";
    const summarizer: Summarizer = .{ .keep = keep.allocator(), .scratch = &scratch, .summary = &summary, .title = &title };
    if (!eachLine(env, path, summarizer, Summarizer.line)) return null;
    if (summary.id.len == 0) return null;
    summary.title = kit.firstOf(&.{ title, summary.first_prompt });
    return agent.clone(types.SessionSummary, out, summary) catch null;
}

/// One session entry on a branch.
pub const Entry = struct {
    json: JsonView,
    id: []const u8,
    parent: ?[]const u8,
};

/// The active branch of a session file: the chain of entries from the
/// newest entry back to the root, oldest first.
pub fn readBranch(env: agent.Env, a: Allocator, path: []const u8, diag: ?*Diagnostics) errors.Error![]const Entry {
    const data = Io.Dir.cwd().readFileAlloc(env.io, path, a, .unlimited) catch |e|
        return errors.fail(diag, .not_found, "pi: read session {s}: {t}", .{ path, e });
    var by_id: std.StringHashMapUnmanaged(Entry) = .empty;
    var leaf: ?[]const u8 = null;
    var count: usize = 0;
    var lines = std.mem.splitScalar(u8, data, '\n');
    while (lines.next()) |line| {
        if (std.mem.trim(u8, line, " \t\r").len == 0) continue;
        const json = JsonView.parseBytes(a, line);
        if (json.object() == null or eql(json.str("type"), "session")) continue;
        var entry: Entry = .{ .json = json, .id = json.str("id"), .parent = if (json.get("parentId").isNull()) null else json.str("parentId") };
        if (entry.id.len == 0) {
            // Version 1 files, which Pi migrates when it next opens them,
            // are a linear list without IDs.
            entry.id = try std.fmt.allocPrint(a, "v1-{d}", .{count});
            entry.parent = leaf;
        }
        count += 1;
        try by_id.put(a, entry.id, entry);
        leaf = entry.id;
    }
    var chain: std.ArrayList(Entry) = .empty;
    var current = leaf;
    while (current) |id| {
        const entry = by_id.get(id) orelse break;
        try chain.append(a, entry);
        if (chain.items.len > by_id.count()) break; // a cycle
        current = entry.parent;
    }
    std.mem.reverse(Entry, chain.items);
    return chain.toOwnedSlice(a);
}

/// The session entry types that become transcript items.
const ItemEntry = enum { compaction, branch_summary, custom_message, message };

/// The transcript items of a branch.
pub fn historyItems(a: Allocator, chain: []const Entry) Allocator.Error![]const types.Item {
    var items: std.ArrayList(types.Item) = .empty;
    // The tool calls awaiting their results, by call ID.
    var tools: std.StringArrayHashMapUnmanaged(usize) = .empty;
    var turn: []const u8 = "";
    for (chain) |entry| {
        const start = items.items.len;
        const json = entry.json;
        if (std.meta.stringToEnum(ItemEntry, json.str("type"))) |kind| switch (kind) {
            .compaction, .branch_summary => {
                cancelPending(&items, &tools);
                try items.append(a, .{ .id = entry.id, .kind = .compaction, .text = json.str("summary") });
            },
            .custom_message => if (json.flag("display")) {
                try items.append(a, .{ .id = entry.id, .kind = .other, .text = try decode.messageText(a, json.get("content")) });
            },
            .message => {
                const message = json.get("message");
                if (message.object() == null) continue;
                const role = decode.Role.of(message);
                // Calls whose results never came end, as the live session
                // ends them when the run settles.
                if (role != .toolResult) cancelPending(&items, &tools);
                if (role == .user) turn = entry.id;
                try appendHistory(a, &items, &tools, entry.id, message);
            },
        };
        const at = kit.parseTimeMs(json.str("timestamp"));
        for (items.items[start..]) |*item| {
            item.turn_id = turn;
            item.time_ms = at;
        }
    }
    return items.toOwnedSlice(a);
}

fn cancelPending(items: *std.ArrayList(types.Item), tools: *std.StringArrayHashMapUnmanaged(usize)) void {
    for (tools.values()) |i| items.items[i].status = .cancelled;
    tools.clearRetainingCapacity();
}

fn appendHistory(a: Allocator, items: *std.ArrayList(types.Item), tools: *std.StringArrayHashMapUnmanaged(usize), id: []const u8, message: JsonView) Allocator.Error!void {
    switch (decode.Role.of(message)) {
        .user => try items.append(a, .{ .id = id, .kind = .user_message, .text = try decode.messageText(a, message.get("content")), .raw = try message.raw(a) }),
        .assistant => try appendAssistant(a, items, tools, id, message),
        .toolResult => {
            const call_id = message.str("toolCallId");
            const pending = tools.fetchSwapRemove(call_id) orelse return;
            const item = &items.items[pending.value];
            var call = item.tool.?;
            const content = try decode.resultContent(a, message);
            call.output = content.text;
            call.images = content.images;
            call.is_error = message.flag("isError");
            try decode.applyToolResult(a, &call, message);
            item.tool = call;
            item.status = if (call.is_error) .failed else .completed;
        },
        // hookMessage is the version 2 name of custom.
        .custom, .hookMessage => if (message.flag("display")) {
            try items.append(a, .{ .id = id, .kind = .other, .text = try decode.messageText(a, message.get("content")), .raw = try message.raw(a) });
        },
        .bashExecution => {
            const command = message.str("command");
            var item: types.Item = .{ .id = id, .kind = .tool_call, .tool = .{ .name = "bash", .kind = .shell, .command = command, .input = try decode.commandInput(a, command) }, .raw = try message.raw(a) };
            const exit: ?i32 = if (message.has("exitCode")) @truncate(message.int("exitCode")) else null;
            decode.bashOutcome(&item, message.str("output"), exit, message.flag("cancelled"));
            try items.append(a, item);
        },
        .compactionSummary, .branchSummary, .other => {},
    }
}

fn appendAssistant(a: Allocator, items: *std.ArrayList(types.Item), tools: *std.StringArrayHashMapUnmanaged(usize), id: []const u8, message: JsonView) Allocator.Error!void {
    const stop = message.str("stopReason");
    for (try decode.blocks(a, message.get("content")), 0..) |block, i| {
        const block_id = try std.fmt.allocPrint(a, "{s}/{d}", .{ id, i });
        switch (block.kind()) {
            .text => try items.append(a, .{ .id = block_id, .kind = .assistant_message, .text = block.text }),
            .thinking => try items.append(a, .{ .id = block_id, .kind = .reasoning, .text = block.thinking }),
            .toolCall => {
                var call: types.ToolCall = .{ .name = block.name, .kind = decode.toolKind(block.name, &.{}), .input = try block.arguments.raw(a) };
                try decode.fillToolDetails(a, &call, block.arguments);
                var status: types.ItemStatus = .in_progress;
                if (eql(stop, "aborted") or eql(stop, "error")) {
                    // Pi runs no tool call of a message that failed.
                    status = .cancelled;
                } else try tools.put(a, block.id, items.items.len);
                try items.append(a, .{ .id = block.id, .kind = .tool_call, .status = status, .tool = call });
            },
            .image, .other => {},
        }
    }
}

/// The subagent tasks that "subagent" tool results on a branch report, and
/// their session files.
pub fn branchSubagents(a: Allocator, chain: []const Entry) Allocator.Error!decode.SubagentResult {
    var args: std.StringHashMapUnmanaged(JsonView) = .empty;
    var tasks: std.ArrayList(types.Task) = .empty;
    var files: std.ArrayList(decode.SubagentFile) = .empty;
    for (chain) |entry| {
        if (!eql(entry.json.str("type"), "message")) continue;
        const message = entry.json.get("message");
        const role = message.str("role");
        if (eql(role, "assistant")) {
            for (try decode.blocks(a, message.get("content"))) |block| {
                if (eql(block.type, "toolCall") and eql(block.name, "subagent")) try args.put(a, block.id, block.arguments);
            }
        } else if (eql(role, "toolResult") and eql(message.str("toolName"), "subagent")) {
            const call_id = message.str("toolCallId");
            const found = try decode.subagentTasks(a, call_id, message, args.get(call_id) orelse .empty, true);
            for (found.tasks) |task| {
                for (tasks.items) |*t| {
                    if (eql(t.id, task.id)) {
                        t.* = task;
                        break;
                    }
                } else try tasks.append(a, task);
            }
            try files.appendSlice(a, found.files);
        }
    }
    return .{ .tasks = try tasks.toOwnedSlice(a), .files = try files.toOwnedSlice(a) };
}

test "history ends tool calls like a live session" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const lines = [_][]const u8{
        \\{"type":"message","id":"u1","parentId":null,"timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}
        ,
        \\{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}]}}
        ,
        \\{"type":"message","id":"r1","parentId":"a1","message":{"role":"toolResult","toolCallId":"c1","content":[{"type":"text","text":"x"}],"details":{"exitCode":0}}}
        ,
        \\{"type":"message","id":"a2","parentId":"r1","message":{"role":"assistant","content":[{"type":"toolCall","id":"c2","name":"read","arguments":{"path":"f"}}]}}
        ,
        \\{"type":"message","id":"u2","parentId":"a2","message":{"role":"user","content":[{"type":"text","text":"next"}]}}
        ,
    };
    var chain: std.ArrayList(Entry) = .empty;
    for (lines) |line| {
        const json = JsonView.parseBytes(a, line);
        try chain.append(a, .{ .json = json, .id = json.str("id"), .parent = null });
    }
    const items = try historyItems(a, chain.items);
    try std.testing.expectEqual(@as(usize, 5), items.len);
    try std.testing.expectEqualStrings("a1/0", items[1].id);
    try std.testing.expectEqual(types.ItemStatus.completed, items[2].status);
    try std.testing.expectEqualStrings("x", items[2].tool.?.output);
    try std.testing.expectEqual(@as(?i32, 0), items[2].tool.?.exit_code);
    try std.testing.expectEqual(types.ItemStatus.cancelled, items[3].status);
    try std.testing.expectEqualStrings("u2", items[4].turn_id);
    try std.testing.expectEqualStrings("u1", items[2].turn_id);
}
