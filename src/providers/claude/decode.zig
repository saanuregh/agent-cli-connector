//! Readers of native message shapes shared by the live stream and the
//! transcript reader: tool classification, tool result content, usage
//! counters, tool call details, task lists, and prompt text.
const std = @import("std");
const Allocator = std.mem.Allocator;
const agent = @import("agent");
const types = agent.types;
const JsonView = agent.json.JsonView;
const kit = agent.providerkit;
const eql = kit.eql;

/// Former Claude Code tool names and their current names, from the Agent
/// SDK's LEGACY_TOOL_NAME_ALIASES.
const legacy_tool_names = [_][2][]const u8{
    .{ "Task", "Agent" },                          .{ "KillShell", "TaskStop" },
    .{ "KillBash", "TaskStop" },                   .{ "ListPeers", "ListAgents" },
    .{ "Brief", "SendUserMessage" },               .{ "ListMcpResources", "ListMcpResourcesTool" },
    .{ "ReadMcpResource", "ReadMcpResourceTool" }, .{ "ReadMcpResourceDir", "ReadMcpResourceDirTool" },
};

/// The tool a subagent calls to deliver its final report (Claude Code
/// 2.1.271 and later, in auto mode).
pub const handback_tool = "SubagentHandback";

/// Content blocks that carry a server-side tool call's result in the
/// assistant message itself.
pub const server_tool_result_types = [_][]const u8{
    "web_search_tool_result",          "web_fetch_tool_result",                  "mcp_tool_result",     "code_execution_tool_result",
    "bash_code_execution_tool_result", "text_editor_code_execution_tool_result", "advisor_tool_result", "tool_search_tool_result",
};

pub fn isServerToolResult(t: []const u8) bool {
    return kit.contains(&server_tool_result_types, t);
}

/// Classifies Claude Code tool names. Legacy names count as the tools that
/// replaced them. TaskStop and Workflow manage background agents and tasks,
/// so they count as subagent tools; the MCP resource tools count as MCP
/// calls; SendUserMessage only shows the user a message and stays "other".
pub fn toolKind(name_in: []const u8, host_server: []const u8) types.ToolKind {
    var name = name_in;
    for (legacy_tool_names) |e| if (eql(name, e[0])) {
        name = e[1];
        break;
    };
    const table = [_]struct { types.ToolKind, []const []const u8 }{
        .{ .shell, &.{ "Bash", "BashOutput", "PowerShell", "Monitor" } },
        .{ .read, &.{ "Read", "NotebookRead" } },
        .{ .edit, &.{ "Edit", "MultiEdit", "Write", "NotebookEdit" } },
        .{ .search, &.{ "Grep", "Glob", "LS", "ToolSearch", "LSP" } },
        .{ .web, &.{ "WebFetch", "WebSearch", "web_search", "web_fetch" } },
        .{ .subagent, &.{ "Agent", "SendMessage", "ListAgents", "TaskStop", "Workflow" } },
        .{ .mcp, &.{ "ListMcpResourcesTool", "ReadMcpResourceTool", "ReadMcpResourceDirTool" } },
        .{ .todo, &.{ "TodoWrite", "TaskCreate", "TaskUpdate", "TaskList", "TaskGet" } },
        .{ .question, &.{"AskUserQuestion"} },
        .{ .plan, &.{ "ExitPlanMode", "EnterPlanMode" } },
    };
    for (table) |e| if (kit.contains(e[1], name)) return e[0];
    if (std.mem.startsWith(u8, name, "mcp__")) {
        if (host_server.len > 0 and std.mem.startsWith(u8, name[5..], host_server) and
            std.mem.startsWith(u8, name[5 + host_server.len ..], "__")) return .host;
        return .mcp;
    }
    return .other;
}

/// The server part of an "mcp__server__tool" name.
pub fn mcpServerOf(name: []const u8) []const u8 {
    if (!std.mem.startsWith(u8, name, "mcp__")) return "";
    const rest = name[5..];
    const end = std.mem.indexOf(u8, rest, "__") orelse rest.len;
    return rest[0..end];
}

/// The input counters of one model request; null means absent.
pub const RequestUsage = struct {
    input_tokens: ?i64 = null,
    cache_read_input_tokens: ?i64 = null,
    cache_creation_input_tokens: ?i64 = null,
    output_tokens: ?i64 = null,

    pub fn of(usage: JsonView) RequestUsage {
        return .{
            .input_tokens = optInt(usage.get("input_tokens")),
            .cache_read_input_tokens = optInt(usage.get("cache_read_input_tokens")),
            .cache_creation_input_tokens = optInt(usage.get("cache_creation_input_tokens")),
            .output_tokens = optInt(usage.get("output_tokens")),
        };
    }

    /// Replaces the counters `next` carries.
    pub fn merge(usage: *RequestUsage, next: RequestUsage) void {
        if (next.input_tokens) |v| usage.input_tokens = v;
        if (next.cache_read_input_tokens) |v| usage.cache_read_input_tokens = v;
        if (next.cache_creation_input_tokens) |v| usage.cache_creation_input_tokens = v;
        if (next.output_tokens) |v| usage.output_tokens = v;
    }

    /// The context the request used: its input plus its output.
    pub fn total(usage: RequestUsage) i64 {
        return (usage.input_tokens orelse 0) + (usage.cache_read_input_tokens orelse 0) +
            (usage.cache_creation_input_tokens orelse 0) + (usage.output_tokens orelse 0);
    }
};

fn optInt(number: JsonView) ?i64 {
    const v = number.v orelse return null;
    return switch (v) {
        .integer, .float, .number_string => number.integer(),
        else => null,
    };
}

/// The uniform usage of an API usage object.
pub fn usageOf(usage: JsonView) types.Usage {
    const input = usage.int("input_tokens");
    const output = usage.int("output_tokens");
    const cache_read = usage.int("cache_read_input_tokens");
    const cache_write = usage.int("cache_creation_input_tokens");
    return .{
        .input_tokens = input,
        .output_tokens = output,
        .cache_read_tokens = cache_read,
        .cache_write_tokens = cache_write,
        .reasoning_tokens = usage.get("output_tokens_details").int("thinking_tokens"),
        .total_tokens = input + output + cache_read + cache_write,
    };
}

/// The text of text blocks, joined with newlines.
pub fn textOfBlocks(a: Allocator, blocks: JsonView) Allocator.Error![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    for (blocks.items()) |value| {
        const block = JsonView.of(value);
        if (!eql(block.str("type"), "text")) continue;
        const text = block.str("text");
        if (text.len == 0) continue;
        if (out.items.len > 0) try out.append(a, '\n');
        try out.appendSlice(a, text);
    }
    return out.items;
}

/// The text and images of tool_result content: a string or content blocks.
pub fn toolResultContent(a: Allocator, content: JsonView) Allocator.Error!struct { []const u8, []const types.Image } {
    const v = content.v orelse return .{ "", &.{} };
    switch (v) {
        .string => |s| return .{ s, &.{} },
        .array => {},
        else => return .{ "", &.{} },
    }
    var images: std.ArrayList(types.Image) = .empty;
    for (content.items()) |value| {
        const block = JsonView.of(value);
        if (!eql(block.str("type"), "image")) continue;
        const source = block.get("source");
        const source_type = source.str("type");
        if (eql(source_type, "base64")) {
            try images.append(a, .{ .media_type = source.str("media_type"), .data = source.str("data") });
        } else if (eql(source_type, "url")) {
            try images.append(a, .{ .media_type = source.str("media_type"), .url = source.str("url") });
        }
    }
    return .{ try textOfBlocks(a, content), images.items };
}

/// Describes a tool_use, server_tool_use, or mcp_tool_use block. An MCP
/// connector call is named "mcp__<server>__<tool>" like other MCP tools.
pub fn toolCallOf(a: Allocator, block: JsonView, host_server: []const u8) Allocator.Error!types.ToolCall {
    var name = block.str("name");
    if (eql(block.str("type"), "mcp_tool_use") and block.str("server_name").len > 0) {
        name = try std.fmt.allocPrint(a, "mcp__{s}__{s}", .{ block.str("server_name"), name });
    }
    var call: types.ToolCall = .{ .name = name, .kind = toolKind(name, host_server) };
    if (call.kind == .mcp or call.kind == .host) call.server = mcpServerOf(name);
    return call;
}

/// Decodes well-known tool arguments into the uniform fields.
pub fn fillToolDetails(a: Allocator, call: *types.ToolCall, input: JsonView) Allocator.Error!void {
    const first = kit.firstOf;
    switch (call.kind) {
        // The MCP resource tools name their server in the arguments.
        .mcp => if (call.server.len == 0) {
            call.server = input.str("server");
        },
        .shell => call.command = input.str("command"),
        .read => call.path = first(&.{ input.str("file_path"), input.str("notebook_path") }),
        .edit => {
            call.path = first(&.{ input.str("file_path"), input.str("notebook_path") });
            call.changes = try editChanges(a, call.name, input, .empty);
        },
        .search => {
            call.query = first(&.{ input.str("pattern"), input.str("query") });
            call.path = input.str("path");
        },
        .web => {
            call.url = input.str("url");
            call.query = input.str("query");
        },
        else => {},
    }
}

/// The file changes of an edit tool from its input and, once known, its
/// structured result.
pub fn editChanges(a: Allocator, tool: []const u8, input: JsonView, result: JsonView) Allocator.Error![]const types.FileChange {
    const path = kit.firstOf(&.{ input.str("file_path"), input.str("notebook_path") });
    if (path.len == 0) return &.{};
    var change: types.FileChange = .{ .path = path, .kind = .update };
    if (result.object() != null) {
        const typ = result.str("type");
        if (eql(typ, "create") or (eql(tool, "Write") and result.get("originalFile").isNull() and !eql(typ, "update"))) {
            change.kind = .add;
        }
        const patch = result.get("structuredPatch");
        if (patch.len() > 0) {
            var b: std.ArrayList(u8) = .empty;
            try b.print(a, "--- a/{s}\n+++ b/{s}\n", .{ path, path });
            for (patch.items()) |value| {
                const hunk = JsonView.of(value);
                try b.print(a, "@@ -{d},{d} +{d},{d} @@\n", .{ hunk.int("oldStart"), hunk.int("oldLines"), hunk.int("newStart"), hunk.int("newLines") });
                for (hunk.get("lines").items()) |l| if (l == .string) {
                    try b.appendSlice(a, l.string);
                    try b.append(a, '\n');
                };
            }
            change.diff = b.items;
        }
    }
    if (change.diff.len == 0) {
        const edits = input.get("edits");
        if (eql(tool, "Write")) {
            change.diff = try simpleDiff(a, path, "", input.str("content"));
        } else if (edits.len() > 0) {
            var b: std.ArrayList(u8) = .empty;
            for (edits.items()) |value| {
                const edit = JsonView.of(value);
                try b.appendSlice(a, try simpleDiff(a, path, edit.str("old_string"), edit.str("new_string")));
            }
            change.diff = b.items;
        } else if (input.str("old_string").len > 0 or input.str("new_string").len > 0) {
            change.diff = try simpleDiff(a, path, input.str("old_string"), input.str("new_string"));
        }
    }
    return a.dupe(types.FileChange, &.{change});
}

/// Renders a replacement as a unified diff without context.
pub fn simpleDiff(a: Allocator, path: []const u8, old: []const u8, new: []const u8) Allocator.Error![]const u8 {
    var b: std.ArrayList(u8) = .empty;
    try b.print(a, "--- a/{s}\n+++ b/{s}\n", .{ path, path });
    const old_n = lineCount(old);
    const new_n = lineCount(new);
    try b.print(a, "@@ -{d},{d} +{d},{d} @@\n", .{ @min(1, old_n), old_n, @min(1, new_n), new_n });
    try appendLines(a, &b, '-', old);
    try appendLines(a, &b, '+', new);
    return b.items;
}

fn lineCount(s: []const u8) usize {
    if (s.len == 0) return 0;
    return std.mem.count(u8, std.mem.trimEnd(u8, s, "\n")[0..], "\n") + 1;
}

fn appendLines(a: Allocator, b: *std.ArrayList(u8), mark: u8, s: []const u8) Allocator.Error!void {
    if (s.len == 0) return;
    const body = if (std.mem.endsWith(u8, s, "\n")) s[0 .. s.len - 1] else s;
    var it = std.mem.splitScalar(u8, body, '\n');
    while (it.next()) |l| {
        try b.append(a, mark);
        try b.appendSlice(a, l);
        try b.append(a, '\n');
    }
}

/// Whether a server tool result's content is an error, such as
/// {"type": "web_search_tool_result_error"}.
pub fn isErrorContent(content: JsonView) bool {
    return std.mem.endsWith(u8, content.str("type"), "_tool_result_error");
}

/// Joins a shell call's streams. Claude trims each stream's trailing
/// newline.
pub fn shellOutput(a: Allocator, stdout_in: []const u8, stderr_in: []const u8) Allocator.Error![]const u8 {
    const stdout = if (std.mem.trim(u8, stdout_in, " \t\r\n").len == 0) "" else stdout_in;
    const stderr = if (std.mem.trim(u8, stderr_in, " \t\r\n").len == 0) "" else stderr_in;
    if (stdout.len > 0 and stderr.len > 0 and !std.mem.endsWith(u8, stdout, "\n")) {
        return std.fmt.allocPrint(a, "{s}\n{s}", .{ stdout, stderr });
    }
    return std.mem.concat(a, u8, &.{ stdout, stderr });
}

/// An Agent result that only acknowledges a background launch.
pub fn isAsyncLaunch(structured: JsonView) bool {
    return structured.flag("isAsync") or eql(structured.str("status"), "async_launched");
}

/// Fills in a tool call's result, shared by the live stream and history:
/// the output, the file changes and shell details the structured result
/// adds, and the final status. A background Agent launch stays running.
/// `non_execution` is the CLI's reason a call did not run, such as
/// "cancelled".
pub fn applyToolResult(a: Allocator, item: *types.Item, content: JsonView, is_error: bool, structured: JsonView, non_execution: []const u8) Allocator.Error!void {
    var call = &item.tool.?;
    call.output, call.images = try toolResultContent(a, content);
    call.is_error = is_error;
    if (!structured.isNull()) {
        call.output_data = try structured.raw(a);
        switch (call.kind) {
            .edit => {
                const input = if (call.input) |in| JsonView.parseBytes(a, in.bytes) else JsonView.empty;
                call.changes = try editChanges(a, call.name, input, structured);
            },
            .shell => if (structured.object() != null) {
                if (structured.flag("interrupted")) item.status = .cancelled;
                const out = try shellOutput(a, structured.str("stdout"), structured.str("stderr"));
                if (out.len > 0) call.output = out;
                if (!structured.get("exitCode").isNull()) call.exit_code = @intCast(std.math.clamp(structured.int("exitCode"), std.math.minInt(i32), std.math.maxInt(i32)));
            },
            .subagent => if (!is_error and isAsyncLaunch(structured)) return,
            else => {},
        }
    }
    if (eql(non_execution, "cancelled")) item.status = .cancelled;
    if (item.status == .in_progress) item.status = if (is_error) .failed else .completed;
}

/// Decodes a TodoWrite input into plan steps.
pub fn todoSteps(a: Allocator, input: JsonView) Allocator.Error!?[]const types.PlanStep {
    if (input.object() == null) return null;
    const todos = input.get("todos").items();
    const out = try a.alloc(types.PlanStep, todos.len);
    for (todos, out) |value, *o| {
        const todo = JsonView.of(value);
        o.* = .{ .text = todo.str("content"), .status = planStatus(todo.str("status")) };
    }
    return out;
}

pub fn planStatus(s: []const u8) @FieldType(types.PlanStep, "status") {
    if (eql(s, "in_progress")) return .in_progress;
    if (eql(s, "completed")) return .completed;
    return .pending;
}

/// The tools that change the task list.
const PlanTool = enum { TaskCreate, TaskUpdate, TaskList };

/// The task list after a successful TaskCreate, TaskUpdate, or TaskList
/// call; null for other calls or results that change nothing.
pub fn applyPlanTool(a: Allocator, plan: []const types.PlanStep, tool: []const u8, input: JsonView, structured: JsonView) Allocator.Error!?[]const types.PlanStep {
    const plan_tool = std.meta.stringToEnum(PlanTool, tool) orelse return null;
    var out: std.ArrayList(types.PlanStep) = .empty;
    try out.appendSlice(a, plan);
    switch (plan_tool) {
        .TaskCreate => {
            const task = structured.get("task");
            if (structured.object() == null or task.str("id").len == 0) return null;
            try out.append(a, .{ .id = task.str("id"), .text = kit.firstOf(&.{ task.str("subject"), input.str("subject") }), .status = .pending });
        },
        .TaskUpdate => {
            if (input.object() == null) return null;
            const id = input.str("taskId");
            const i = for (out.items, 0..) |step, k| {
                if (eql(step.id, id)) break k;
            } else return null;
            const status = input.str("status");
            if (eql(status, "deleted")) {
                _ = out.orderedRemove(i);
                return out.items;
            }
            if (input.str("subject").len > 0) out.items[i].text = input.str("subject");
            if (status.len > 0) out.items[i].status = planStatus(status);
        },
        .TaskList => {
            if (structured.object() == null) return null;
            out.clearRetainingCapacity();
            for (structured.get("tasks").items()) |value| {
                const task = JsonView.of(value);
                try out.append(a, .{ .id = task.str("id"), .text = task.str("subject"), .status = planStatus(task.str("status")) });
            }
        },
    }
    return out.items;
}

const HookOutcome = enum { success, @"error", cancelled };

/// A hook_response outcome. The CLI reports any non-zero exit as "error";
/// exit code 2 is Claude Code's blocking error. "cancelled" (aborted or
/// timed out) counts as failed.
pub fn hookOutcome(outcome: []const u8, exit_code: JsonView) types.HookOutcome {
    return switch (std.meta.stringToEnum(HookOutcome, outcome) orelse return .none) {
        .success => .succeeded,
        .@"error" => if (!exit_code.isNull() and exit_code.integer() == 2) .blocked else .failed,
        .cancelled => .failed,
    };
}

/// Text the CLI records that is not conversation: interrupt placeholders,
/// "No response requested.", and local command output.
pub fn isTranscriptNoise(text_in: []const u8) bool {
    const text = std.mem.trim(u8, text_in, " \t\r\n");
    if (std.mem.startsWith(u8, text, "[Request interrupted by user") and std.mem.endsWith(u8, text, "]") and
        std.mem.indexOfScalar(u8, text[1 .. text.len - 1], ']') == null) return true;
    if (eql(text, "No response requested.")) return true;
    const opens = std.mem.startsWith(u8, text, "<local-command-stdout>") or std.mem.startsWith(u8, text, "<local-command-stderr>");
    const closes = std.mem.endsWith(u8, text, "</local-command-stdout>") or std.mem.endsWith(u8, text, "</local-command-stderr>");
    return opens and closes;
}

/// Prompt text the CLI writes itself, such as background task
/// notifications.
pub fn isInjectedPrompt(text_in: []const u8) bool {
    const text = std.mem.trim(u8, text_in, " \t\r\n");
    return std.mem.startsWith(u8, text, "<task-notification>") or std.mem.startsWith(u8, text, "<local-command-");
}

/// The CLI's wrapper around a slash command the host sent.
pub fn isCommandEcho(text: []const u8) bool {
    return std.mem.indexOf(u8, text, "<command-name>") != null or std.mem.indexOf(u8, text, "<command-message>") != null;
}

fn tagContent(text: []const u8, comptime tag: []const u8) ?[]const u8 {
    const open = "<" ++ tag ++ ">";
    const close = "</" ++ tag ++ ">";
    const start = (std.mem.indexOf(u8, text, open) orelse return null) + open.len;
    const end = std.mem.indexOfPos(u8, text, start, close) orelse return null;
    return text[start..end];
}

/// Rewrites the CLI's slash-command wrapper to the "/name args" the user
/// typed. Other text is returned unchanged.
pub fn normalizeCommand(a: Allocator, text: []const u8) Allocator.Error![]const u8 {
    const raw = tagContent(text, "command-name") orelse tagContent(text, "command-message") orelse return text;
    const name = std.mem.trim(u8, raw, " \t\r\n");
    const slash: []const u8 = if (std.mem.startsWith(u8, name, "/")) "" else "/";
    if (tagContent(text, "command-args")) |args_raw| {
        const args = std.mem.trim(u8, args_raw, " \t\r\n");
        if (args.len > 0) return std.fmt.allocPrint(a, "{s}{s} {s}", .{ slash, name, args });
    }
    return std.fmt.allocPrint(a, "{s}{s}", .{ slash, name });
}

/// The first text of a user message that is a prompt: command wrappers
/// become "/name args", and CLI noise, notifications, and other
/// XML-wrapped text yield "".
pub fn promptText(a: Allocator, msg: JsonView) Allocator.Error![]const u8 {
    const content = msg.get("content");
    var texts: std.ArrayList([]const u8) = .empty;
    if (content.v != null and content.v.? == .string) {
        try texts.append(a, content.v.?.string);
    } else for (content.items()) |bv| {
        const b = JsonView.of(bv);
        if (eql(b.str("type"), "text")) try texts.append(a, b.str("text"));
    }
    for (texts.items) |t| {
        const n = std.mem.trim(u8, try normalizeCommand(a, t), " \t\r\n");
        if (n.len == 0 or isTranscriptNoise(n) or std.mem.startsWith(u8, n, "<")) continue;
        return n;
    }
    return "";
}

test "tool kinds and diffs" {
    try std.testing.expect(isTranscriptNoise("[Request interrupted by user for tool use]"));
    try std.testing.expect(isTranscriptNoise("<local-command-stdout>x</local-command-stdout>"));
    {
        var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
        defer arena.deinit();
        try std.testing.expectEqualStrings("/review src", try normalizeCommand(arena.allocator(), "<command-name>review</command-name><command-args> src </command-args>"));
    }
    try std.testing.expectEqual(types.ToolKind.subagent, toolKind("Task", ""));
    try std.testing.expectEqual(types.ToolKind.host, toolKind("mcp__host__echo", "host"));
    try std.testing.expectEqual(types.ToolKind.mcp, toolKind("mcp__hostx__echo", "host"));
    try std.testing.expectEqualStrings("srv", mcpServerOf("mcp__srv__t"));
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const d = try simpleDiff(arena.allocator(), "f", "a\nb\n", "c");
    try std.testing.expectEqualStrings("--- a/f\n+++ b/f\n@@ -1,2 +1,1 @@\n-a\n-b\n+c\n", d);
}
