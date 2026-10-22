//! Persisted Codex threads, read through an app-server without starting a
//! session: listing, history, deletion, and subagents.
const std = @import("std");
const agent = @import("agent");
const decode = @import("decode.zig");
const session = @import("session.zig");
const provider_module = @import("provider.zig");
const Client = @import("client.zig").Client;
const Options = @import("options.zig").Options;
const Allocator = std.mem.Allocator;
const types = agent.types;
const errors = agent.errors;
const providerkit = agent.providerkit;
const JsonView = agent.json.JsonView;
const ObjectBuilder = agent.json.ObjectBuilder;
const IdMap = providerkit.IdMap;
const Diagnostics = agent.Diagnostics;
const Value = std.json.Value;
const Error = errors.Error;
const eql = providerkit.eql;
const name = @import("root.zig").name;

/// Bounds the threads `listSubagents` reads.
const max_subagent_threads = 100;

/// Persisted threads, newest first, reading thread/list pages until offset
/// and limit are covered. When `Options.config` sets model_provider, only
/// threads of that provider are listed, because Codex otherwise lists only
/// threads of its configured default provider.
pub fn listSessions(ptr: *anyopaque, a: Allocator, opts: types.ListSessionsOptions, diag: ?*Diagnostics) Error![]const types.SessionSummary {
    const provider = provider_module.of(ptr);
    const client = try provider_module.acquire(provider, diag);
    defer client.release();
    var seen: IdMap(void) = .empty;
    var out: std.ArrayList(types.SessionSummary) = .empty;
    var cursor: []const u8 = "";
    while (true) {
        var params: ObjectBuilder = .init(a);
        try params.str("sortKey", "updated_at");
        if (opts.limit > 0) try params.put("limit", .{ .integer = @as(i64, opts.offset) + opts.limit - @as(i64, @intCast(seen.count())) });
        if (opts.cwd.len > 0) try params.str("cwd", providerkit.absDir(provider.env, a, opts.cwd));
        const model_provider = modelProvider(provider.opts);
        if (model_provider.len > 0) try params.put("modelProviders", try agent.json.stringArray(a, &.{model_provider}));
        try params.strIf("cursor", cursor);
        const response = try client.callJson(a, "thread/list", params.value(), .{}, diag);
        var added: usize = 0;
        for (response.get("data").items()) |thread| {
            const summary = try sessionSummary(a, JsonView.of(thread)) orelse continue;
            if (seen.contains(.of(summary.id))) continue;
            try seen.put(a, .of(summary.id), {});
            added += 1;
            if (seen.count() <= opts.offset) continue;
            try out.append(a, summary);
            if (opts.limit > 0 and out.items.len >= opts.limit) return out.items;
        }
        // A page without new rows means the cursor stopped advancing.
        cursor = response.str("nextCursor");
        if (added == 0 or cursor.len == 0) return out.items;
    }
}

/// model_provider from `Options.config`, without TOML quotes.
fn modelProvider(opts: Options) []const u8 {
    const config = opts.config orelse return "";
    const value = std.mem.trim(u8, config.map.get("model_provider") orelse return "", " \t\r\n");
    if (value.len >= 2 and (value[0] == '"' or value[0] == '\'') and value[value.len - 1] == value[0]) return value[1 .. value.len - 1];
    return value;
}

fn sessionSummary(a: Allocator, thread: JsonView) Error!?types.SessionSummary {
    const id = thread.str("id");
    if (id.len == 0) return null;
    const preview = std.mem.trim(u8, thread.str("preview"), " \t\r\n");
    return .{
        .id = id,
        .provider = name,
        .title = providerkit.firstOf(&.{ thread.str("name"), preview }),
        .cwd = thread.str("cwd"),
        .created_at_ms = @max(thread.int("createdAt"), 0) * 1000,
        .updated_at_ms = @max(thread.int("updatedAt"), 0) * 1000,
        .path = thread.str("path"),
        .model = thread.str("model"),
        .effort = thread.str("reasoningEffort"),
        .first_prompt = preview,
        .raw = try thread.raw(a),
    };
}

/// Permanently deletes a thread. `cwd` is unused: Codex finds threads by
/// ID.
pub fn deleteSession(ptr: *anyopaque, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) Error!void {
    _ = cwd;
    const provider = provider_module.of(ptr);
    var arena: std.heap.ArenaAllocator = .init(provider.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const client = try provider_module.acquire(provider, diag);
    defer client.release();
    var call_diag: Diagnostics = .{};
    _ = client.call(a, "thread/delete", try agent.json.toValue(a, .{ .threadId = session_id }), .{}, &call_diag) catch |e| {
        const info = Diagnostics.of(&call_diag, e);
        if (session.isNoRollout(info)) return notFound(a, session_id, info, diag);
        return session.forward(diag, info, e);
    };
}

fn notFound(a: Allocator, thread: []const u8, info: types.ErrorInfo, diag: ?*Diagnostics) Error {
    var missing = info;
    missing.kind = .session_not_found;
    missing.message = std.fmt.allocPrint(a, "codex: thread {s}: session not found: {s}", .{ thread, info.message }) catch info.message;
    return errors.failWith(diag, missing);
}

/// `err` as `session_not_found` when it is thread/read's answer for a
/// thread that does not exist. A stored thread that is not loaded is still
/// read, so "thread not loaded" means missing.
fn missingThread(a: Allocator, thread: []const u8, info: types.ErrorInfo, err: Error, diag: ?*Diagnostics) Error {
    if (std.mem.startsWith(u8, session.protocolMessage(info), "thread not loaded") or session.isNoRollout(info)) return notFound(a, thread, info, diag);
    return session.forward(diag, info, err);
}

fn readTurns(client: *Client, a: Allocator, thread: []const u8, diag: ?*Diagnostics) Error![]const Value {
    const params = try agent.json.toValue(a, .{ .threadId = thread, .includeTurns = true });
    const response = try client.callJson(a, "thread/read", params, .{}, diag);
    return response.get("thread").get("turns").items();
}

/// The thread's newest turns from thread/turns/list until they hold at
/// least `want` items, oldest first.
fn readLatestTurns(client: *Client, a: Allocator, thread: []const u8, want: usize, diag: ?*Diagnostics) Error![]const Value {
    var turns: std.ArrayList(Value) = .empty;
    var items: usize = 0;
    var cursor: []const u8 = "";
    while (items < want) {
        const params = try agent.json.toValue(a, .{
            .threadId = thread,
            .sortDirection = "desc",
            .itemsView = "full",
            .limit = 20,
            .cursor = if (cursor.len > 0) cursor else null,
        });
        const response = try client.callJson(a, "thread/turns/list", params, .{}, diag);
        const data = response.get("data").items();
        for (data) |turn| {
            try turns.append(a, turn);
            items += JsonView.of(turn).get("items").len();
        }
        cursor = response.str("nextCursor");
        if (cursor.len == 0 or data.len == 0) break;
    }
    std.mem.reverse(Value, turns.items);
    return turns.items;
}

/// A persisted thread's items, or a subagent thread's with
/// `opts.subagent_id`. Items carry their turn ID and their own time, else
/// their turn's start (user messages) or completion. Items a stopped turn
/// left in progress come back cancelled or failed.
pub fn readHistory(ptr: *anyopaque, a: Allocator, session_id: []const u8, opts: types.ReadHistoryOptions, diag: ?*Diagnostics) Error![]const types.Item {
    const provider = provider_module.of(ptr);
    const target = providerkit.firstOf(&.{ opts.subagent_id, session_id });
    const client = try provider_module.acquire(provider, diag);
    defer client.release();
    const parent = if (opts.subagent_id.len > 0) try spawnItem(client, a, session_id, opts.subagent_id) else "";
    var turns: []const Value = &.{};
    var need_full = opts.limit == 0;
    var read_diag: Diagnostics = .{};
    if (opts.limit > 0) {
        turns = readLatestTurns(client, a, target, @as(usize, opts.offset) + opts.limit, &read_diag) catch |e| blk: {
            // A server that refuses thread/turns/list, such as an older
            // one, reads the whole thread.
            if (e != error.Protocol) return missingThread(a, target, Diagnostics.of(&read_diag, e), e, diag);
            need_full = true;
            break :blk &.{};
        };
    }
    if (need_full) turns = readTurns(client, a, target, &read_diag) catch |e|
        return missingThread(a, target, Diagnostics.of(&read_diag, e), e, diag);
    var out: std.ArrayList(types.Item) = .empty;
    for (turns) |turn_value| {
        const turn = JsonView.of(turn_value);
        for (turn.get("items").items()) |raw| {
            var item = (try decode.threadItem(a, JsonView.of(raw))).item;
            item.turn_id = turn.str("id");
            item.parent_id = parent;
            item.subagent_id = opts.subagent_id;
            item.time_ms = itemTimeMs(JsonView.of(raw), item.kind, turn);
            endStoppedItem(&item, turn.str("status"));
            try out.append(a, item);
        }
    }
    return providerkit.lastPage(types.Item, out.items, opts.limit, opts.offset);
}

/// Ends an item that an interrupted or failed turn left in progress, as
/// the live session does when the turn ends.
fn endStoppedItem(item: *types.Item, turn_status: []const u8) void {
    if (item.status != .in_progress) return;
    if (eql(turn_status, "interrupted")) item.status = .cancelled;
    if (eql(turn_status, "failed")) {
        item.status = .failed;
        if (item.tool) |*call| call.is_error = true;
    }
}

/// The item that spawned the subagent thread, read from the history of its
/// parent thread (`session_id` when Codex names none), or "". A history
/// that cannot be read names no item.
fn spawnItem(client: *Client, a: Allocator, session_id: []const u8, subagent: []const u8) Error![]const u8 {
    const params = try agent.json.toValue(a, .{ .threadId = subagent, .includeTurns = false });
    var parent = session_id;
    if (client.callJson(a, "thread/read", params, .{}, null)) |response| {
        parent = providerkit.firstOf(&.{ response.get("thread").str("parentThreadId"), session_id });
    } else |_| {}
    const turns = readTurns(client, a, parent, null) catch return "";
    var task = decode.newSubagentTask(subagent);
    for (turns) |turn| for (JsonView.of(turn).get("items").items()) |raw| {
        for (try decode.subagentChanges(a, JsonView.of(raw), "", 0, .{ .only = subagent })) |change| change.apply(&task);
    };
    return task.tool_call_id;
}

/// An item's own timestamp, or its turn's time.
fn itemTimeMs(raw: JsonView, kind: types.ItemKind, turn: JsonView) i64 {
    for ([_][]const u8{ "timestamp", "createdAt" }) |key| {
        const time = parseTimeMs(raw.get(key));
        if (time != 0) return time;
    }
    var first = turn.int("completedAt");
    var second = turn.int("startedAt");
    if (kind == .user_message) std.mem.swap(i64, &first, &second);
    if (first > 0) return first * 1000;
    if (second > 0) return second * 1000;
    return 0;
}

/// Unix seconds, Unix milliseconds, or an RFC 3339 string.
fn parseTimeMs(time: JsonView) i64 {
    const value = time.v orelse return 0;
    switch (value) {
        .integer, .number_string => {
            const number = time.integer();
            if (number <= 0) return 0;
            return if (number > 1_000_000_000_000) number else number * 1000;
        },
        .string => |text| return providerkit.parseTimeMs(text),
        else => return 0,
    }
}

/// The subagents a persisted thread started, including nested ones, from
/// the subagent items of its history and its subagent threads' histories.
/// A subagent thread that cannot be read is skipped.
pub fn listSubagents(ptr: *anyopaque, a: Allocator, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) Error![]const types.Task {
    _ = cwd;
    const provider = provider_module.of(ptr);
    const client = try provider_module.acquire(provider, diag);
    defer client.release();
    const Pending = struct { thread: []const u8, depth: u32 };
    var tasks: IdMap(types.Task) = .empty;
    var queue: std.ArrayList(Pending) = .empty;
    try queue.append(a, .{ .thread = session_id, .depth = 0 });
    var seen: IdMap(void) = .empty;
    try seen.put(a, .of(session_id), {});
    var reads: usize = 0;
    var head: usize = 0;
    var read_diag: Diagnostics = .{};
    while (head < queue.items.len and reads < max_subagent_threads) : (reads += 1) {
        const current = queue.items[head];
        head += 1;
        const turns = readTurns(client, a, current.thread, &read_diag) catch |e| {
            if (head == 1) return missingThread(a, current.thread, Diagnostics.of(&read_diag, e), e, diag);
            continue;
        };
        if (tasks.getPtr(.of(current.thread))) |task| if (task.status == .running and turns.len > 0) {
            const status = JsonView.of(turns[turns.len - 1]).str("status");
            if (eql(status, "completed")) task.status = .completed;
            if (eql(status, "failed")) task.status = .failed;
            if (eql(status, "interrupted")) task.status = .stopped;
        };
        const sender = if (eql(current.thread, session_id)) "" else current.thread;
        for (turns) |turn| for (JsonView.of(turn).get("items").items()) |raw| {
            for (try decode.subagentChanges(a, JsonView.of(raw), sender, current.depth, .{ .exclude = .{ current.thread, session_id } })) |change| {
                const gop = try tasks.getOrPut(a, .of(change.thread));
                if (!gop.found_existing) gop.value_ptr.* = decode.newSubagentTask(change.thread);
                change.apply(gop.value_ptr);
                if (!seen.contains(.of(change.thread))) {
                    try seen.put(a, .of(change.thread), {});
                    try queue.append(a, .{ .thread = change.thread, .depth = @max(gop.value_ptr.subagent.?.depth, current.depth + 1) });
                }
            }
        };
    }
    return tasks.values();
}
