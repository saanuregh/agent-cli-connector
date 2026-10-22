//! Readers of Pi's native message shapes, shared by live translation and
//! history: content blocks, usage, tool details, subagent and todo results,
//! thinking levels, model names, and output schemas.
const std = @import("std");
const Allocator = std.mem.Allocator;
const agent = @import("agent");
const types = agent.types;
const errors = agent.errors;
const kit = agent.providerkit;
const Diagnostics = errors.Diagnostics;
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const Value = std.json.Value;

/// The bridge tool that receives structured output.
pub const output_tool = "structured_output";

/// Pi's thinking ladder, lowest first.
pub const thinking_levels = [_][]const u8{ "off", "minimal", "low", "medium", "high", "xhigh", "max" };

const eql = kit.eql;

// ---------------------------------------------------------------- usage

/// A message's usage; the context fields stay zero.
pub fn usage(fields: JsonView) types.Usage {
    return .{
        .input_tokens = fields.int("input"),
        .output_tokens = fields.int("output"),
        .cache_read_tokens = fields.int("cacheRead"),
        .cache_write_tokens = fields.int("cacheWrite"),
        .reasoning_tokens = fields.int("reasoning"),
        .total_tokens = fields.int("totalTokens"),
        .cost_usd = fields.get("cost").float("total"),
    };
}

/// The context a message filled: its input, cached input, and output.
pub fn contextTokens(fields: JsonView) i64 {
    return fields.int("input") + fields.int("cacheRead") + fields.int("cacheWrite") + fields.int("output");
}

// ---------------------------------------------------------------- content

/// The roles of Pi messages; `other` stands for any role this library
/// does not read.
pub const Role = enum {
    user,
    assistant,
    toolResult,
    custom,
    hookMessage,
    bashExecution,
    compactionSummary,
    branchSummary,
    other,

    pub fn of(message: JsonView) Role {
        return std.meta.stringToEnum(Role, message.str("role")) orelse .other;
    }
};

pub const BlockKind = enum { text, thinking, toolCall, image, other };

pub const Block = struct {
    type: []const u8 = "",
    text: []const u8 = "",
    thinking: []const u8 = "",
    id: []const u8 = "",
    name: []const u8 = "",
    arguments: JsonView = .empty,
    data: []const u8 = "",
    mime_type: []const u8 = "",
    raw: JsonView = .empty,

    pub fn kind(block: Block) BlockKind {
        return std.meta.stringToEnum(BlockKind, block.type) orelse .other;
    }
};

/// The content blocks of a message: a list of blocks, or a string as one
/// text block.
pub fn blocks(a: Allocator, content: JsonView) Allocator.Error![]const Block {
    if (content.v) |v| if (v == .string) {
        if (v.string.len == 0) return &.{};
        return a.dupe(Block, &.{.{ .type = "text", .text = v.string }});
    };
    const all = content.items();
    const out = try a.alloc(Block, all.len);
    for (all, out) |v, *b| {
        const block = JsonView.of(v);
        b.* = .{
            .type = block.str("type"),
            .text = block.str("text"),
            .thinking = block.str("thinking"),
            .id = block.str("id"),
            .name = block.str("name"),
            .arguments = block.get("arguments"),
            .data = block.str("data"),
            .mime_type = block.str("mimeType"),
            .raw = block,
        };
    }
    return out;
}

/// The text blocks of a message joined by newlines.
pub fn messageText(a: Allocator, content: JsonView) Allocator.Error![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    for (try blocks(a, content)) |b| {
        if (!eql(b.type, "text") or b.text.len == 0) continue;
        if (out.items.len > 0) try out.append(a, '\n');
        try out.appendSlice(a, b.text);
    }
    return out.toOwnedSlice(a);
}

/// The text and images of a tool result, or of a toolResult message, which
/// has the same content field.
pub fn resultContent(a: Allocator, result: JsonView) Allocator.Error!struct { text: []const u8, images: []const types.Image } {
    var text: std.ArrayList(u8) = .empty;
    var images: std.ArrayList(types.Image) = .empty;
    for (result.get("content").items()) |v| {
        const block = JsonView.of(v);
        const kind = block.str("type");
        if (eql(kind, "text")) {
            try text.appendSlice(a, block.str("text"));
        } else if (eql(kind, "image")) {
            const data = block.str("data");
            // Only valid base64 counts as an image.
            _ = std.base64.standard.Decoder.calcSizeForSlice(data) catch continue;
            try images.append(a, .{ .media_type = block.str("mimeType"), .data = data });
        }
    }
    return .{ .text = try text.toOwnedSlice(a), .images = try images.toOwnedSlice(a) };
}

pub fn resultText(a: Allocator, result: JsonView) Allocator.Error![]const u8 {
    return (try resultContent(a, result)).text;
}

// ---------------------------------------------------------------- tools

pub fn isReadTool(name: []const u8) bool {
    return kit.contains(&.{ "read", "grep", "find", "ls" }, name);
}

pub fn isEditTool(name: []const u8) bool {
    return eql(name, "edit") or eql(name, "write");
}

pub fn toolKind(name: []const u8, host_tools: []const types.ToolDef) types.ToolKind {
    for (host_tools) |t| if (eql(t.name, name)) return .host;
    const table = [_]struct { []const []const u8, types.ToolKind }{
        .{ &.{"bash"}, .shell },
        .{ &.{"read"}, .read },
        .{ &.{ "edit", "write" }, .edit },
        .{ &.{ "grep", "find", "ls", "tool_search" }, .search },
        .{ &.{ "web_search", "web_fetch", "fetch" }, .web },
        .{ &.{ "subagent", "Agent", "task" }, .subagent },
        .{ &.{ "todo", "todos", "todo_write" }, .todo },
        .{ &.{ "ask_user_question", "question", "questionnaire" }, .question },
    };
    for (table) |e| if (kit.contains(e[0], name)) return e[1];
    if (std.mem.startsWith(u8, name, "mcp__")) return .mcp;
    return .other;
}

pub fn fillToolDetails(a: Allocator, call: *types.ToolCall, args: JsonView) Allocator.Error!void {
    switch (call.kind) {
        .shell => call.command = args.str("command"),
        .read => call.path = args.str("path"),
        .edit => {
            call.path = args.str("path");
            const kind: @FieldType(types.FileChange, "kind") = if (eql(call.name, "write")) .add else .update;
            call.changes = try a.dupe(types.FileChange, &.{.{ .path = call.path, .kind = kind }});
        },
        .search => {
            call.query = kit.firstOf(&.{ args.str("pattern"), args.str("query") });
            call.path = args.str("path");
        },
        .web => {
            call.url = args.str("url");
            call.query = args.str("query");
        },
        else => {},
    }
}

fn isEmptyObject(value: JsonView) bool {
    const object = value.object() orelse return false;
    return object.count() == 0;
}

/// Fills a tool call's output data, exit code, and diff from a tool result,
/// or a toolResult message, which has the same details and
/// structuredContent fields.
pub fn applyToolResult(a: Allocator, call: *types.ToolCall, result: JsonView) Allocator.Error!void {
    const structured = result.get("structuredContent");
    const details = result.get("details");
    if (!structured.isNull()) {
        call.output_data = try structured.raw(a);
    } else if (!details.isNull() and !isEmptyObject(details)) {
        call.output_data = try details.raw(a);
    }
    switch (call.kind) {
        .shell => {
            if (structured.has("exit_code")) {
                call.exit_code = @truncate(structured.int("exit_code"));
            } else if (details.has("exitCode")) {
                call.exit_code = @truncate(details.int("exitCode"));
            } else call.exit_code = null;
        },
        .edit => if (call.changes.len == 1) {
            const changes = try a.dupe(types.FileChange, call.changes);
            changes[0].diff = kit.firstOf(&.{ details.str("patch"), details.str("diff") });
            call.changes = changes;
        },
        else => {},
    }
}

/// Fills a shell item from a bash command's result: a cancelled command is
/// cancelled, and a nonzero exit code is an error.
pub fn bashOutcome(item: *types.Item, output: []const u8, exit_code: ?i32, cancelled: bool) void {
    var call = item.tool orelse types.ToolCall{};
    call.output = output;
    call.exit_code = exit_code;
    item.status = .completed;
    if (cancelled) {
        item.status = .cancelled;
    } else if (exit_code) |c| if (c != 0) {
        call.is_error = true;
    };
    item.tool = call;
}

/// The tool input `{"command": command}`.
pub fn commandInput(a: Allocator, command: []const u8) Allocator.Error!RawJson {
    return RawJson.encode(a, .{ .command = command }) catch error.OutOfMemory;
}

/// Pi's name for an MCP server tool.
pub fn mcpToolName(a: Allocator, server: []const u8, tool: []const u8) Allocator.Error![]const u8 {
    const out = try std.fmt.allocPrint(a, "mcp__{s}__{s}", .{ server, tool });
    for (out[5..]) |*c| {
        if (c.* == '_' or std.ascii.isAlphanumeric(c.*)) continue;
        c.* = '_';
    }
    return out;
}

/// What an allow_session answer approves: the exact command of a bash
/// call, the exact path of an edit or write, and otherwise the tool, since
/// Pi's other tools carry no narrower identity the host can check.
pub fn grantKey(a: Allocator, tool: []const u8, command: []const u8, path: []const u8) Allocator.Error![]const u8 {
    if (eql(tool, "bash")) return std.fmt.allocPrint(a, "bash\x00command\x00{s}", .{command});
    if (isEditTool(tool)) return std.fmt.allocPrint(a, "{s}\x00path\x00{s}", .{ tool, path });
    return a.dupe(u8, tool);
}

// ---------------------------------------------------------------- subagents and todos

pub const SubagentFile = struct { task_id: []const u8, path: []const u8 };

pub const SubagentResult = struct {
    tasks: []const types.Task = &.{},
    files: []const SubagentFile = &.{},
};

/// Reads the result of a "subagent" tool call: Pi's example subagent
/// extension and pi-subagents, whose details list one row per agent run,
/// and @gotgenes/pi-subagents, whose details describe one agent.
pub fn subagentTasks(a: Allocator, call_id: []const u8, result: JsonView, args: JsonView, done: bool) Allocator.Error!SubagentResult {
    if (result.object() == null) return .{};
    const details = result.get("details");
    const results = details.get("results").items();
    var tasks: std.ArrayList(types.Task) = .empty;
    var files: std.ArrayList(SubagentFile) = .empty;
    if (results.len == 0 and details.str("agentId").len > 0) {
        var task: types.Task = .{
            .id = try std.fmt.allocPrint(a, "{s}:subagent:0", .{call_id}),
            .kind = .subagent,
            .native_kind = "subagent-tool",
            .status = .running,
            .description = kit.firstOf(&.{ details.str("description"), args.str("description"), args.str("prompt") }),
            .tool_call_id = call_id,
            .tool_uses = @intCast(@max(0, @min(details.int("toolUses"), std.math.maxInt(u32)))),
            .duration_ms = details.int("durationMs"),
            .subagent = .{ .type = kit.firstOf(&.{ details.str("subagentType"), args.str("subagent_type") }), .prompt = args.str("prompt"), .depth = 1 },
        };
        const status = details.str("status");
        if (eql(status, "completed")) task.status = .completed;
        if (eql(status, "error")) task.status = .failed;
        if (eql(status, "aborted") or eql(status, "stopped")) task.status = .stopped;
        if (task.status == .failed) {
            task.@"error" = try resultText(a, result);
        } else if (task.status != .running) task.output = try resultText(a, result);
        if (details.str("transcriptPath").len > 0) try files.append(a, .{ .task_id = task.id, .path = details.str("transcriptPath") });
        try tasks.append(a, task);
        return .{ .tasks = try tasks.toOwnedSlice(a), .files = try files.toOwnedSlice(a) };
    }
    for (results, 0..) |v, i| {
        const run = JsonView.of(v);
        if (run.str("agent").len == 0) continue;
        var step: i64 = @intCast(i);
        if (run.has("index")) step = run.int("index") else if (run.has("step")) step = run.int("step");
        var prompt = run.str("task");
        if (prompt.len == 0 and results.len == 1) prompt = args.str("task");
        var task: types.Task = .{
            .id = try std.fmt.allocPrint(a, "{s}:subagent:{d}", .{ call_id, step }),
            .kind = .subagent,
            .native_kind = "subagent-tool",
            .status = .running,
            .description = prompt,
            .tool_call_id = call_id,
            .subagent = .{ .type = run.str("agent"), .prompt = prompt, .model = run.str("model"), .depth = 1 },
        };
        var output = try lastAssistantText(a, run.get("messages"));
        if (output.len == 0 and results.len == 1) output = try resultText(a, result);
        const stop = run.str("stopReason");
        const failed_flag = run.has("success") and !run.flag("success");
        if (!done and !run.flag("finished")) {
            task.summary = output;
        } else if (eql(stop, "aborted")) {
            task.status = .stopped;
        } else if (run.int("exitCode") != 0 or eql(stop, "error") or failed_flag) {
            task.status = .failed;
            const stderr = std.mem.trim(u8, run.str("stderr"), " \t\r\n");
            task.@"error" = if (run.str("errorMessage").len > 0) run.str("errorMessage") else if (stderr.len > 0) stderr else try std.fmt.allocPrint(a, "exit code {d}", .{run.int("exitCode")});
        } else task.status = .completed;
        if (task.status != .running) task.output = output;
        const run_usage = run.get("usage");
        if (!run_usage.isNull()) {
            const input = run_usage.int("input");
            const output_tokens = run_usage.int("output");
            const cache_read = run_usage.int("cacheRead");
            const cache_write = run_usage.int("cacheWrite");
            task.usage = .{
                .input_tokens = input,
                .output_tokens = output_tokens,
                .cache_read_tokens = cache_read,
                .cache_write_tokens = cache_write,
                .total_tokens = input + output_tokens + cache_read + cache_write,
                .cost_usd = run_usage.float("cost"),
            };
        }
        if (run.str("sessionFile").len > 0) try files.append(a, .{ .task_id = task.id, .path = run.str("sessionFile") });
        try tasks.append(a, task);
    }
    return .{ .tasks = try tasks.toOwnedSlice(a), .files = try files.toOwnedSlice(a) };
}

fn lastAssistantText(a: Allocator, messages: JsonView) Allocator.Error![]const u8 {
    const all = messages.items();
    var i = all.len;
    while (i > 0) {
        i -= 1;
        const message = JsonView.of(all[i]);
        if (!eql(message.str("role"), "assistant")) continue;
        for (message.get("content").items()) |b| {
            const block = JsonView.of(b);
            if (eql(block.str("type"), "text") and block.str("text").len > 0) return block.str("text");
        }
    }
    _ = a;
    return "";
}

fn idString(a: Allocator, id: JsonView) Allocator.Error![]const u8 {
    const v = id.v orelse return "";
    return switch (v) {
        .integer => |n| std.fmt.allocPrint(a, "{d}", .{n}),
        .float => |f| std.fmt.allocPrint(a, "{d}", .{f}),
        .string, .number_string => |s| s,
        else => "",
    };
}

/// The task list of a todo tool result: Pi's example todo extension
/// (details.todos) or rpiv-todo (details.tasks). Null when it has none.
pub fn todoSteps(a: Allocator, result: JsonView) Allocator.Error!?[]const types.PlanStep {
    const details = result.get("details");
    if (result.object() == null or details.str("error").len > 0) return null;
    var steps: std.ArrayList(types.PlanStep) = .empty;
    if (details.get("todos").v != null and details.get("todos").v.? == .array) {
        for (details.get("todos").items()) |v| {
            const todo = JsonView.of(v);
            try steps.append(a, .{ .id = try idString(a, todo.get("id")), .text = todo.str("text"), .status = if (todo.flag("done")) .completed else .pending });
        }
    } else if (details.get("tasks").v != null and details.get("tasks").v.? == .array) {
        for (details.get("tasks").items()) |v| {
            const task = JsonView.of(v);
            const PlanStatus = @FieldType(types.PlanStep, "status");
            // Deleted tasks have no plan status.
            const status = std.meta.stringToEnum(enum { pending, in_progress, completed }, task.str("status")) orelse continue;
            try steps.append(a, .{ .id = try idString(a, task.get("id")), .text = task.str("subject"), .status = switch (status) {
                .pending => PlanStatus.pending,
                .in_progress => .in_progress,
                .completed => .completed,
            } });
        }
    } else return null;
    return try steps.toOwnedSlice(a);
}

// ---------------------------------------------------------------- models and thinking

/// Pi's thinking level for a uniform effort.
pub fn thinkingLevel(effort: []const u8) []const u8 {
    return if (eql(effort, types.effort.none)) "off" else effort;
}

/// The uniform effort for a Pi thinking level.
pub fn effortOf(level: []const u8) []const u8 {
    return if (eql(level, "off")) types.effort.none else level;
}

/// The thinking levels a model accepts. xhigh and max are opt-in through
/// the level map, where null marks a level the model lacks.
pub fn modelEfforts(a: Allocator, model: JsonView) Allocator.Error![]const []const u8 {
    if (!model.flag("reasoning")) return &.{};
    const map = model.get("thinkingLevelMap").object();
    var out: std.ArrayList([]const u8) = .empty;
    for (thinking_levels) |level| {
        const mapped: ?Value = if (map) |m| m.get(level) else null;
        if (mapped) |v| {
            if (v == .null) continue;
        } else if (eql(level, "xhigh") or eql(level, "max")) continue;
        try out.append(a, effortOf(level));
    }
    return out.toOwnedSlice(a);
}

fn levelIndex(level: []const u8) ?usize {
    for (thinking_levels, 0..) |l, i| if (eql(l, level)) return i;
    return null;
}

/// `want` if the model accepts it, else the nearest higher accepted level,
/// else the nearest lower one, as Pi clamps thinking levels.
pub fn clampEffort(want: []const u8, accepted: []const []const u8) []const u8 {
    if (want.len == 0 or accepted.len == 0) return "";
    if (kit.contains(accepted, want)) return want;
    const i = levelIndex(thinkingLevel(want)) orelse return "";
    for (thinking_levels[i + 1 ..]) |l| if (kit.contains(accepted, effortOf(l))) return effortOf(l);
    var j = i;
    while (j > 0) {
        j -= 1;
        if (kit.contains(accepted, effortOf(thinking_levels[j]))) return effortOf(thinking_levels[j]);
    }
    return accepted[0];
}

pub const ModelName = struct { provider: []const u8 = "", id: []const u8 = "", level: []const u8 = "" };

/// Splits "provider/id" or "provider:id", with an optional ":thinking"
/// suffix. The provider is empty for a bare ID.
pub fn splitModel(model_in: []const u8) ModelName {
    var model = model_in;
    var level: []const u8 = "";
    if (std.mem.lastIndexOfScalar(u8, model, ':')) |i| if (levelIndex(model[i + 1 ..]) != null) {
        level = model[i + 1 ..];
        model = model[0..i];
    };
    const slash = std.mem.indexOfScalar(u8, model, '/');
    const colon = std.mem.indexOfScalar(u8, model, ':');
    if (slash) |s| if (colon == null or s < colon.?) return .{ .provider = model[0..s], .id = model[s + 1 ..], .level = level };
    if (colon) |c| return .{ .provider = model[0..c], .id = model[c + 1 ..], .level = level };
    return .{ .id = model, .level = level };
}

/// "provider/model" without a leading slash when the provider is empty.
pub fn modelName(a: Allocator, provider: []const u8, model: []const u8) Allocator.Error![]const u8 {
    if (provider.len == 0) return model;
    return std.fmt.allocPrint(a, "{s}/{s}", .{ provider, model });
}

/// Maps Pi's source scopes to the uniform "user" and "project".
pub fn commandScope(scope: []const u8) []const u8 {
    if (kit.contains(&.{ "global", "personal", "user" }, scope)) return "user";
    if (kit.contains(&.{ "workspace", "local", "project" }, scope)) return "project";
    return scope;
}

/// Describes a failed assistant message beyond its error text.
pub fn errorDetail(a: Allocator, message: JsonView) Allocator.Error![]const u8 {
    var parts: std.ArrayList([]const u8) = .empty;
    if (message.str("stopReason").len > 0) try parts.append(a, try std.fmt.allocPrint(a, "stop reason {s}", .{message.str("stopReason")}));
    if (message.str("model").len > 0) try parts.append(a, try std.fmt.allocPrint(a, "model {s}", .{try modelName(a, message.str("provider"), message.str("model"))}));
    const response_model = message.str("responseModel");
    if (response_model.len > 0 and !eql(response_model, message.str("model"))) try parts.append(a, try std.fmt.allocPrint(a, "response model {s}", .{response_model}));
    if (message.str("responseId").len > 0) try parts.append(a, try std.fmt.allocPrint(a, "response {s}", .{message.str("responseId")}));
    return std.mem.join(a, ", ", parts.items);
}

/// The parameters of the structured_output tool for an output schema. Tool
/// parameters must be an object, so another schema is wrapped in the
/// "value" property, and `wrapped` is true.
pub fn outputSchema(a: Allocator, schema: RawJson, diag: ?*Diagnostics) errors.Error!struct { params: RawJson, wrapped: bool } {
    const v = std.json.parseFromSliceLeaky(Value, a, schema.bytes, .{}) catch |e|
        return errors.fail(diag, .invalid_params, "pi: output schema must be a JSON object: {t}", .{e});
    if (v != .object) return errors.fail(diag, .invalid_params, "pi: output schema must be a JSON object", .{});
    if (eql(JsonView.of(v).str("type"), "object")) return .{ .params = schema, .wrapped = false };
    const params = try std.fmt.allocPrint(a, "{{\"type\":\"object\",\"properties\":{{\"value\":{s}}},\"required\":[\"value\"],\"additionalProperties\":false}}", .{schema.bytes});
    return .{ .params = .of(params), .wrapped = true };
}

test "efforts, clamping, and model names" {
    const a = std.testing.allocator;
    try std.testing.expectEqualStrings("max", clampEffort("high", &.{ "low", "medium", "max" }));
    try std.testing.expectEqualStrings("medium", clampEffort("max", &.{ "low", "medium" }));
    const m = splitModel("deepseek:deepseek-flash:high");
    try std.testing.expectEqualStrings("deepseek", m.provider);
    try std.testing.expectEqualStrings("deepseek-flash", m.id);
    try std.testing.expectEqualStrings("high", m.level);
    var arena: std.heap.ArenaAllocator = .init(a);
    defer arena.deinit();
    const model = JsonView.parseBytes(arena.allocator(),
        \\{"reasoning":true,"thinkingLevelMap":{"minimal":null,"low":"low","medium":null,"high":"high","max":"max"}}
    );
    const efforts = try modelEfforts(arena.allocator(), model);
    try std.testing.expectEqual(@as(usize, 4), efforts.len);
    try std.testing.expectEqualStrings("none", efforts[0]);
    try std.testing.expectEqualStrings("max", efforts[3]);
    try std.testing.expectEqualStrings("mcp__my_srv__do_it", try mcpToolName(arena.allocator(), "my-srv", "do.it"));
}
