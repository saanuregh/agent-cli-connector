//! Readers of app-server message shapes, shared by live translation and
//! persisted history: thread items, subagent task updates, rate limits,
//! token usage, and errors. Everything returned is allocated in the
//! caller's arena.
const std = @import("std");
const agent = @import("agent");
const Allocator = std.mem.Allocator;
const types = agent.types;
const JsonView = agent.json.JsonView;
const ObjectBuilder = agent.json.ObjectBuilder;
const Value = std.json.Value;
const eql = agent.providerkit.eql;

/// What an item carries beyond the uniform fields.
pub const Extra = struct {
    phase: []const u8 = "",
    /// Subagent threads the item names.
    children: []const []const u8 = &.{},
    /// An agent message with questions answered by a later user message.
    is_async: bool = false,
    questions: []const AsyncQuestion = &.{},
};

pub const AsyncQuestion = struct { title: []const u8, options: []const []const u8 };

pub const Converted = struct { item: types.Item, extra: Extra };

pub fn itemStatus(status: []const u8) types.ItemStatus {
    if (status.len == 0 or eql(status, "inProgress")) return .in_progress;
    if (eql(status, "failed")) return .failed;
    if (eql(status, "declined")) return .declined;
    if (eql(status, "interrupted")) return .cancelled;
    return .completed;
}

/// The ThreadItem types read into uniform items. Others become `other`.
const ItemType = enum {
    userMessage,
    agentMessage,
    reasoning,
    plan,
    contextCompaction,
    commandExecution,
    fileChange,
    mcpToolCall,
    dynamicToolCall,
    collabAgentToolCall,
    subAgentActivity,
    webSearch,
    imageView,
    imageGeneration,
    enteredReviewMode,
    exitedReviewMode,
};

/// Maps a Codex ThreadItem to a uniform item.
pub fn threadItem(a: Allocator, thread_item: JsonView) Allocator.Error!Converted {
    var out: Converted = .{ .item = .{ .id = thread_item.str("id"), .status = .completed, .raw = try thread_item.raw(a) }, .extra = .{} };
    const item = &out.item;
    const item_type = std.meta.stringToEnum(ItemType, thread_item.str("type")) orelse {
        item.kind = .other;
        return out;
    };
    switch (item_type) {
        .userMessage => {
            item.kind = .user_message;
            var texts: std.ArrayList([]const u8) = .empty;
            for (thread_item.get("content").items()) |part_value| {
                const part = JsonView.of(part_value);
                if (eql(part.str("type"), "text")) try texts.append(a, part.str("text"));
            }
            item.text = try std.mem.join(a, "\n", texts.items);
            item.client_id = thread_item.str("clientId");
        },
        .agentMessage => try agentMessage(a, thread_item, &out),
        .reasoning => {
            item.kind = .reasoning;
            var parts: std.ArrayList([]const u8) = .empty;
            try parts.appendSlice(a, try thread_item.get("summary").strings(a));
            try parts.appendSlice(a, try thread_item.get("content").strings(a));
            item.text = try std.mem.join(a, "\n\n", parts.items);
        },
        .plan => {
            item.kind = .plan;
            item.text = thread_item.str("text");
        },
        .contextCompaction => item.kind = .compaction,
        .commandExecution => try commandExecution(a, thread_item, item),
        .fileChange => try fileChange(a, thread_item, item),
        .mcpToolCall => try mcpToolCall(a, thread_item, item),
        .dynamicToolCall => try dynamicToolCall(a, thread_item, item),
        .collabAgentToolCall => {
            item.kind = .tool_call;
            item.status = itemStatus(thread_item.str("status"));
            const receivers = try thread_item.get("receiverThreadIds").strings(a);
            var input: ObjectBuilder = .init(a);
            try input.str("tool", thread_item.str("tool"));
            try input.put("receiverThreadIds", try agent.json.stringArray(a, receivers));
            if (thread_item.has("prompt")) try input.str("prompt", thread_item.str("prompt"));
            item.tool = .{ .name = thread_item.str("tool"), .kind = .subagent, .input = try input.toRaw() };
            out.extra.children = receivers;
        },
        .subAgentActivity => {
            item.kind = .tool_call;
            const thread = thread_item.str("agentThreadId");
            var input: ObjectBuilder = .init(a);
            try input.str("kind", thread_item.str("kind"));
            try input.str("agentThreadId", thread);
            try input.str("agentPath", thread_item.str("agentPath"));
            if (thread_item.has("model")) try input.str("model", thread_item.str("model"));
            item.tool = .{ .name = "subagent", .kind = .subagent, .input = try input.toRaw(), .output = thread_item.str("kind") };
            if (thread.len > 0) out.extra.children = try a.dupe([]const u8, &.{thread});
        },
        .webSearch => try webSearch(a, thread_item, item),
        .imageView => {
            item.kind = .tool_call;
            var input: ObjectBuilder = .init(a);
            try input.str("path", thread_item.str("path"));
            item.tool = .{ .name = "imageView", .kind = .read, .path = thread_item.str("path"), .input = try input.toRaw() };
        },
        .imageGeneration => try imageGeneration(a, thread_item, item),
        .enteredReviewMode, .exitedReviewMode => {
            item.kind = .other;
            item.text = thread_item.str("review");
        },
    }
    return out;
}

fn agentMessage(a: Allocator, thread_item: JsonView, out: *Converted) Allocator.Error!void {
    out.item.kind = .assistant_message;
    out.item.text = thread_item.str("text");
    out.extra.phase = thread_item.str("phase");
    out.extra.is_async = eql(thread_item.str("delivery"), "async");
    var questions: std.ArrayList(AsyncQuestion) = .empty;
    for (thread_item.get("questions").items()) |question_value| {
        const question = JsonView.of(question_value);
        try questions.append(a, .{ .title = question.str("title"), .options = try question.get("options").strings(a) });
    }
    out.extra.questions = questions.items;
}

fn commandExecution(a: Allocator, thread_item: JsonView, item: *types.Item) Allocator.Error!void {
    item.kind = .tool_call;
    item.status = itemStatus(thread_item.str("status"));
    var call: types.ToolCall = .{ .name = "commandExecution", .kind = .shell, .command = thread_item.str("command"), .cwd = thread_item.str("cwd") };
    if (!thread_item.get("exitCode").isNull()) call.exit_code = @intCast(std.math.clamp(thread_item.int("exitCode"), std.math.minInt(i32), std.math.maxInt(i32)));
    call.output = thread_item.str("aggregatedOutput");
    call.duration_ms = thread_item.int("durationMs");
    call.is_error = item.status == .failed or (call.exit_code != null and call.exit_code.? != 0);
    var input: ObjectBuilder = .init(a);
    try input.str("command", call.command);
    try input.str("cwd", call.cwd);
    if (thread_item.has("processId")) try input.str("processId", thread_item.str("processId"));
    call.input = try input.toRaw();
    item.tool = call;
}

fn fileChange(a: Allocator, thread_item: JsonView, item: *types.Item) Allocator.Error!void {
    item.kind = .tool_call;
    item.status = itemStatus(thread_item.str("status"));
    var call: types.ToolCall = .{ .name = "fileChange", .kind = .edit, .is_error = item.status == .failed };
    var changes: std.ArrayList(types.FileChange) = .empty;
    for (thread_item.get("changes").items()) |change_value| {
        const change = JsonView.of(change_value);
        const kind = change.get("kind").str("type");
        try changes.append(a, .{
            .path = change.str("path"),
            .diff = change.str("diff"),
            .kind = if (eql(kind, "add")) .add else if (eql(kind, "delete")) .delete else .update,
            .move_path = change.get("kind").str("move_path"),
        });
    }
    call.changes = changes.items;
    if (changes.items.len > 0) call.path = changes.items[0].path;
    item.tool = call;
}

fn mcpToolCall(a: Allocator, thread_item: JsonView, item: *types.Item) Allocator.Error!void {
    item.kind = .tool_call;
    item.status = itemStatus(thread_item.str("status"));
    const server = thread_item.str("server");
    var call: types.ToolCall = .{
        .name = try std.fmt.allocPrint(a, "mcp__{s}__{s}", .{ server, thread_item.str("tool") }),
        .kind = .mcp,
        .server = server,
        .input = try thread_item.get("arguments").raw(a),
        .display_name = thread_item.get("appContext").str("actionName"),
        .server_display_name = thread_item.get("appContext").str("appName"),
        .duration_ms = thread_item.int("durationMs"),
    };
    if (!thread_item.get("error").isNull()) {
        call.is_error = true;
        call.output = thread_item.get("error").str("message");
    } else if (!thread_item.get("result").isNull()) {
        const result = thread_item.get("result");
        var texts: std.ArrayList([]const u8) = .empty;
        var images: std.ArrayList(types.Image) = .empty;
        for (result.get("content").items()) |content_value| {
            const content = JsonView.of(content_value);
            if (eql(content.str("type"), "text")) try texts.append(a, content.str("text"));
            if (eql(content.str("type"), "image") and validBase64(content.str("data")))
                try images.append(a, .{ .media_type = content.str("mimeType"), .data = content.str("data") });
        }
        call.output = try std.mem.join(a, "\n", texts.items);
        call.images = images.items;
        call.output_data = try result.get("structuredContent").raw(a);
    }
    item.tool = call;
}

fn dynamicToolCall(a: Allocator, thread_item: JsonView, item: *types.Item) Allocator.Error!void {
    item.kind = .tool_call;
    item.status = itemStatus(thread_item.str("status"));
    const namespace = std.mem.trim(u8, thread_item.str("namespace"), " \t\r\n");
    var call: types.ToolCall = .{
        .name = if (namespace.len > 0) try std.fmt.allocPrint(a, "{s}.{s}", .{ namespace, thread_item.str("tool") }) else thread_item.str("tool"),
        .kind = .host,
        .input = try thread_item.get("arguments").raw(a),
        .duration_ms = thread_item.int("durationMs"),
    };
    var texts: std.ArrayList([]const u8) = .empty;
    for (thread_item.get("contentItems").items()) |content| {
        if (eql(JsonView.of(content).str("type"), "inputText")) try texts.append(a, JsonView.of(content).str("text"));
    }
    call.output = try std.mem.join(a, "\n", texts.items);
    const success = thread_item.get("success");
    call.is_error = !success.isNull() and !success.boolean();
    item.tool = call;
}

fn webSearch(a: Allocator, thread_item: JsonView, item: *types.Item) Allocator.Error!void {
    item.kind = .tool_call;
    var call: types.ToolCall = .{ .name = "webSearch", .kind = .web, .query = thread_item.str("query") };
    var input: ObjectBuilder = .init(a);
    try input.str("query", thread_item.str("query"));
    const action = thread_item.get("action");
    if (!action.isNull()) {
        const action_type = action.str("type");
        try input.str("action", action_type);
        if (eql(action_type, "search")) {
            const joined = try std.mem.join(a, "\n", try action.get("queries").strings(a));
            call.query = agent.providerkit.firstOf(&.{ thread_item.str("query"), action.str("query"), joined });
        } else if (eql(action_type, "openPage")) {
            call.url = action.str("url");
        } else if (eql(action_type, "findInPage")) {
            call.query = agent.providerkit.firstOf(&.{ action.str("pattern"), thread_item.str("query") });
            call.url = action.str("url");
        }
        if (call.url.len > 0) try input.str("url", call.url);
        try input.str("query", call.query);
    }
    call.input = try input.toRaw();
    item.tool = call;
}

fn imageGeneration(a: Allocator, thread_item: JsonView, item: *types.Item) Allocator.Error!void {
    item.kind = .tool_call;
    var call: types.ToolCall = .{ .name = "imageGeneration", .kind = .other, .path = thread_item.str("savedPath") };
    var input: ObjectBuilder = .init(a);
    if (thread_item.has("revisedPrompt")) try input.str("revisedPrompt", thread_item.str("revisedPrompt"));
    call.input = try input.toRaw();
    const result = thread_item.str("result");
    if (call.path.len > 0) {
        call.images = try a.dupe(types.Image, &.{.{ .path = call.path }});
    } else if (result.len > 0 and validBase64(result)) {
        call.images = try a.dupe(types.Image, &.{.{ .media_type = "image/png", .data = result }});
    }
    item.status = itemStatus(thread_item.str("status"));
    const failure = thread_item.get("failure");
    if (!failure.isNull() or item.status == .failed) {
        item.status = .failed;
        call.is_error = true;
        call.output = agent.providerkit.firstOf(&.{ failure.str("type"), "image generation failed" });
        call.output_data = try failure.raw(a);
    }
    item.tool = call;
}

fn validBase64(text: []const u8) bool {
    var buffer: [4096]u8 = undefined;
    const size = std.base64.standard.Decoder.calcSizeForSlice(text) catch return false;
    if (size > buffer.len) {
        // Long payloads: check the alphabet only.
        for (text) |c| if (!(std.ascii.isAlphanumeric(c) or c == '+' or c == '/' or c == '=')) return false;
        return true;
    }
    std.base64.standard.Decoder.decode(buffer[0..size], text) catch return false;
    return true;
}

// ---------------------------------------------------------------- subagents

/// The running task of a subagent thread.
pub fn newSubagentTask(thread: []const u8) types.Task {
    return .{ .id = thread, .kind = .subagent, .native_kind = "thread", .status = .running, .subagent = .{} };
}

/// Which threads a subagent item may update.
pub const Skip = struct {
    exclude: [2][]const u8 = .{ "", "" },
    /// When set, every other thread is skipped.
    only: []const u8 = "",

    fn skips(skip: Skip, thread: []const u8) bool {
        if (skip.only.len > 0 and !eql(thread, skip.only)) return true;
        for (skip.exclude) |excluded| if (excluded.len > 0 and eql(thread, excluded)) return true;
        return false;
    }
};

/// An update of one subagent's task implied by a subAgentActivity or
/// collabAgentToolCall item.
pub const TaskChange = struct {
    thread: []const u8,
    item: JsonView,
    sender: []const u8,
    sender_depth: u32,
    /// For collabAgentToolCall: the receiver's state.
    state_status: []const u8 = "",
    state_message: []const u8 = "",

    /// Applies the change. Strings point into the item's memory.
    pub fn apply(change: TaskChange, task: *types.Task) void {
        if (task.subagent == null) task.subagent = .{};
        const subagent = &task.subagent.?;
        const item = change.item;
        if (eql(item.str("type"), "subAgentActivity")) {
            const path = item.str("agentPath");
            if (path.len > 0) {
                task.description = path;
                subagent.type = path[if (std.mem.lastIndexOfScalar(u8, path, '/')) |i| i + 1 else 0..];
            }
            if (item.has("model")) subagent.model = item.str("model");
            if (item.has("reasoningEffort")) subagent.effort = item.str("reasoningEffort");
            const kind = item.str("kind");
            if (eql(kind, "started")) {
                task.tool_call_id = item.str("id");
                task.status = .running;
                change.spawned(task);
            } else if (eql(kind, "interrupted")) {
                task.status = .stopped;
            } else if (eql(kind, "completed")) {
                if (task.status == .running) task.status = .completed;
            }
            return;
        }
        if (eql(item.str("tool"), "spawnAgent")) {
            task.tool_call_id = item.str("id");
            if (item.has("prompt")) subagent.prompt = item.str("prompt");
            if (item.has("model")) subagent.model = item.str("model");
            if (item.has("reasoningEffort")) subagent.effort = item.str("reasoningEffort");
            change.spawned(task);
        }
        const state = change.state_status;
        if (eql(state, "running") or eql(state, "pendingInit")) task.status = .running;
        if (eql(state, "completed")) task.status = .completed;
        if (eql(state, "errored") or eql(state, "notFound")) task.status = .failed;
        if (eql(state, "interrupted") or eql(state, "shutdown")) task.status = .stopped;
        if (change.state_message.len > 0) {
            if (task.status == .failed) task.@"error" = change.state_message else task.output = change.state_message;
        }
    }

    fn spawned(change: TaskChange, task: *types.Task) void {
        const subagent = &task.subagent.?;
        if (subagent.depth == 0) subagent.depth = change.sender_depth + 1;
        if (task.parent_id.len == 0) task.parent_id = change.sender;
    }
};

/// The task updates a subAgentActivity or collabAgentToolCall item implies.
/// `sender` is the thread that produced the item ("" for the main thread)
/// and `sender_depth` its subagent depth (0 for the main thread).
pub fn subagentChanges(a: Allocator, thread_item: JsonView, sender: []const u8, sender_depth: u32, skip: Skip) Allocator.Error![]TaskChange {
    var out: std.ArrayList(TaskChange) = .empty;
    const item_type = thread_item.str("type");
    if (eql(item_type, "subAgentActivity")) {
        const thread = thread_item.str("agentThreadId");
        if (thread.len == 0 or skip.skips(thread)) return &.{};
        try out.append(a, .{ .thread = thread, .item = thread_item, .sender = sender, .sender_depth = sender_depth });
    } else if (eql(item_type, "collabAgentToolCall")) {
        for (thread_item.get("receiverThreadIds").items()) |receiver| {
            if (receiver != .string or skip.skips(receiver.string)) continue;
            const state = thread_item.get("agentsStates").get(receiver.string);
            try out.append(a, .{
                .thread = receiver.string,
                .item = thread_item,
                .sender = sender,
                .sender_depth = sender_depth,
                .state_status = state.str("status"),
                .state_message = state.str("message"),
            });
        }
    }
    return out.items;
}

// ---------------------------------------------------------------- usage

/// A TokenUsageBreakdown.
pub const TokenUsage = struct {
    total: i64 = 0,
    input: i64 = 0,
    cached_input: i64 = 0,
    cache_write_input: i64 = 0,
    output: i64 = 0,
    reasoning_output: i64 = 0,

    pub fn parse(breakdown: JsonView) TokenUsage {
        return .{
            .total = breakdown.int("totalTokens"),
            .input = breakdown.int("inputTokens"),
            .cached_input = breakdown.int("cachedInputTokens"),
            .cache_write_input = breakdown.int("cacheWriteInputTokens"),
            .output = breakdown.int("outputTokens"),
            .reasoning_output = breakdown.int("reasoningOutputTokens"),
        };
    }

    pub fn minus(tokens: TokenUsage, other: TokenUsage) TokenUsage {
        return .{
            .total = tokens.total - other.total,
            .input = tokens.input - other.input,
            .cached_input = tokens.cached_input - other.cached_input,
            .cache_write_input = @max(tokens.cache_write_input - other.cache_write_input, 0),
            .output = tokens.output - other.output,
            .reasoning_output = tokens.reasoning_output - other.reasoning_output,
        };
    }

    pub fn plus(tokens: TokenUsage, other: TokenUsage) TokenUsage {
        return .{
            .total = tokens.total + other.total,
            .input = tokens.input + other.input,
            .cached_input = tokens.cached_input + other.cached_input,
            .cache_write_input = tokens.cache_write_input + other.cache_write_input,
            .output = tokens.output + other.output,
            .reasoning_output = tokens.reasoning_output + other.reasoning_output,
        };
    }

    /// Whether any running total is lower than in `other`, as after a revert or
    /// compaction.
    pub fn below(tokens: TokenUsage, other: TokenUsage) bool {
        return tokens.total < other.total or tokens.input < other.input or tokens.cached_input < other.cached_input or
            tokens.output < other.output or tokens.reasoning_output < other.reasoning_output;
    }

    pub fn usage(tokens: TokenUsage) types.Usage {
        return .{
            // Codex counts cached input inside input; the uniform fields
            // keep them apart.
            .input_tokens = tokens.input - tokens.cached_input,
            .output_tokens = tokens.output,
            .cache_read_tokens = tokens.cached_input,
            .cache_write_tokens = tokens.cache_write_input,
            .reasoning_tokens = tokens.reasoning_output,
            .total_tokens = tokens.total,
        };
    }
};

// ---------------------------------------------------------------- rate limits

/// The limit ID of the account's main Codex allowance. Snapshots without a
/// limit ID describe it too.
pub const main_limit_id = "codex";

pub fn limitId(snapshot: JsonView) []const u8 {
    return agent.providerkit.firstOf(&.{ snapshot.str("limitId"), main_limit_id });
}

/// The primary and secondary windows of a native snapshot. Codex reports no
/// warning level, so a window is exceeded at 100% or when the server says a
/// limit was reached.
pub fn rateLimits(a: Allocator, snapshot: JsonView, limit_id: []const u8) Allocator.Error![]types.RateLimit {
    var out: std.ArrayList(types.RateLimit) = .empty;
    for ([_][]const u8{ "primary", "secondary" }) |name| {
        const window = snapshot.get(name);
        if (window.isNull()) continue;
        var limit: types.RateLimit = .{ .name = name, .used_percent = window.float("usedPercent"), .limit_id = limit_id };
        limit.window_ms = window.int("windowDurationMins") * 60_000;
        limit.resets_at_ms = window.int("resetsAt") * 1000;
        if (limit.used_percent >= 100) limit.status = .exceeded;
        try out.append(a, limit);
    }
    if (!snapshot.get("rateLimitReachedType").isNull() and out.items.len > 0) out.items[0].status = .exceeded;
    return out.items;
}

/// When every exhausted window has reset, or 0 when none is exhausted or a
/// reset time is unknown.
pub fn resetsAtMs(limits: []const types.RateLimit) i64 {
    var latest: i64 = 0;
    for (limits) |limit| {
        if (limit.used_percent < 100) continue;
        if (limit.resets_at_ms == 0) return 0;
        latest = @max(latest, limit.resets_at_ms);
    }
    return latest;
}

/// `update` with the fields of `old` it lacks when both are objects, such
/// as a usage window updated with only its used share; otherwise `update`.
pub fn mergeObject(a: Allocator, old: ?Value, update: Value) Allocator.Error!Value {
    const previous = old orelse return update;
    if (previous != .object or update != .object) return update;
    var merged: ObjectBuilder = .init(a);
    try merged.putAll(previous.object);
    try merged.putAll(update.object);
    return merged.value();
}

// ---------------------------------------------------------------- errors

/// The name of a codexErrorInfo: a string, or the key of a single-key
/// object.
pub fn errorCode(a: Allocator, info: JsonView) []const u8 {
    const value = info.v orelse return "";
    return switch (value) {
        .null => "",
        .string => |s| s,
        .object => |object| if (object.count() > 0) object.keys()[0] else "",
        else => agent.json.encodeValue(a, value) catch "",
    };
}

/// The HTTP status a codexErrorInfo object carries.
pub fn httpStatus(info: JsonView) u32 {
    const object = info.object() orelse return 0;
    for (object.values()) |value| {
        const entry = JsonView.of(value);
        if (!entry.get("httpStatusCode").isNull()) return entry.uint32("httpStatusCode");
    }
    return 0;
}

/// A TurnError's message joined with its additional details.
pub fn errorMessage(a: Allocator, turn_error: JsonView) []const u8 {
    const message = turn_error.str("message");
    const details = std.mem.trim(u8, turn_error.str("additionalDetails"), " \t\r\n");
    if (details.len == 0 or eql(details, message)) return message;
    if (message.len == 0) return details;
    return std.fmt.allocPrint(a, "{s}: {s}", .{ message, details }) catch message;
}

/// A JSON-RPC request ID as a string key.
pub fn requestKey(a: Allocator, id: JsonView) []const u8 {
    const value = id.v orelse return "";
    return switch (value) {
        .integer => |n| std.fmt.allocPrint(a, "{d}", .{n}) catch "",
        .string, .number_string => |s| s,
        else => agent.json.encodeValue(a, value) catch "",
    };
}

pub fn hookOutcome(status: []const u8) types.HookOutcome {
    if (eql(status, "completed")) return .succeeded;
    if (eql(status, "failed")) return .failed;
    if (eql(status, "blocked") or eql(status, "stopped")) return .blocked;
    return .none;
}

pub fn goal(native: JsonView) types.Goal {
    return .{
        .objective = native.str("objective"),
        .status = native.str("status"),
        .tokens_used = native.int("tokensUsed"),
        .token_budget = native.int("tokenBudget"),
        .time_used_ms = native.int("timeUsedSeconds") * 1000,
    };
}

/// The thread a message's params name.
pub fn threadOf(params: JsonView) []const u8 {
    const thread = params.get("threadId");
    if (thread.v != null and thread.v.? == .string) return thread.string();
    return params.str("conversationId");
}

pub fn containsIgnoreCase(haystack: []const u8, needle: []const u8) bool {
    if (needle.len > haystack.len) return false;
    for (0..haystack.len - needle.len + 1) |i| if (std.ascii.eqlIgnoreCase(haystack[i..][0..needle.len], needle)) return true;
    return false;
}

test "decode thread items" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const cmd = try threadItem(a, JsonView.parseBytes(a,
        \\{"type":"commandExecution","id":"c1","command":"ls","cwd":"/w","status":"completed","exitCode":2,"processId":"pty-7"}
    ));
    try std.testing.expectEqual(types.ItemKind.tool_call, cmd.item.kind);
    try std.testing.expect(cmd.item.tool.?.is_error);
    try std.testing.expectEqualStrings("{\"command\":\"ls\",\"cwd\":\"/w\",\"processId\":\"pty-7\"}", cmd.item.tool.?.input.?.bytes);
    const web = try threadItem(a, JsonView.parseBytes(a,
        \\{"type":"webSearch","id":"w2","query":"","action":{"type":"search","queries":["a","b"]}}
    ));
    try std.testing.expectEqualStrings("a\nb", web.item.tool.?.query);
    const dyn = try threadItem(a, JsonView.parseBytes(a,
        \\{"type":"dynamicToolCall","id":"d1","namespace":"files","tool":"read","status":"completed","success":false}
    ));
    try std.testing.expectEqualStrings("files.read", dyn.item.tool.?.name);
    try std.testing.expect(dyn.item.tool.?.is_error);
    const message = try threadItem(a, JsonView.parseBytes(a,
        \\{"type":"agentMessage","id":"a1","text":"Which?","delivery":"async","questions":[{"title":"Pick","options":["red","blue"]}]}
    ));
    try std.testing.expect(message.extra.is_async);
    try std.testing.expectEqual(@as(usize, 2), message.extra.questions[0].options.len);
}
