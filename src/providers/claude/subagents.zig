//! The subagents and workflows a persisted session ran, from the files the
//! CLI writes beside the transcript. The files are undocumented CLI
//! internals, so fields can be missing.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const agent = @import("agent");
const types = agent.types;
const errors = agent.errors;
const JsonView = agent.json.JsonView;
const Diagnostics = agent.Diagnostics;
const kit = agent.providerkit;
const eql = kit.eql;
const firstOf = kit.firstOf;

const decode = @import("decode.zig");
const history = @import("history.zig");
const Provider = @import("provider.zig").Provider;

/// Caps the workflow output file read for `Task.output`.
const max_workflow_output = 1 << 20;

/// What a transcript records about a tool call that started a subagent or
/// workflow.
const ParentCall = struct {
    name: []const u8 = "",
    subagent_type: []const u8 = "",
    description: []const u8 = "",
    done: bool = false,
    failed: bool = false,
    output: []const u8 = "",
};

const Links = struct {
    by_id: std.StringArrayHashMapUnmanaged(ParentCall) = .empty,
    /// Workflow run ID to its tool call.
    by_run: std.StringArrayHashMapUnmanaged([]const u8) = .empty,
};

/// Lists subagent transcripts (subagents/**/agent-<id>.jsonl with their
/// agent-<id>.meta.json sidecars) and workflow run summaries
/// (workflows/<run>.json). A workflow's members have its run ID as parent.
pub fn listSubagents(ptr: *anyopaque, a: Allocator, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) errors.Error![]const types.Task {
    const provider: *Provider = @ptrCast(@alignCast(ptr));
    const io = provider.env.io;
    const path = try history.findTranscript(provider, a, .{}, session_id, cwd, diag);
    const graph = history.Graph.read(io, a, path) catch |e| return errors.fail(diag, .internal, "claude: read transcript: {t}", .{e});
    const links = try parentCalls(a, graph);
    const session_dir = history.withoutTranscriptSuffix(path);
    var out: std.ArrayList(types.Task) = .empty;
    const sub_root = try std.fs.path.join(a, &.{ session_dir, "subagents" });
    if (Io.Dir.cwd().openDir(io, sub_root, .{ .iterate = true })) |opened| {
        var dir = opened;
        defer dir.close(io);
        var walker = try dir.walk(a);
        defer walker.deinit();
        while (walker.next(io) catch null) |dir_entry| {
            if (dir_entry.kind != .file or !std.mem.startsWith(u8, dir_entry.basename, "agent-") or !std.mem.endsWith(u8, dir_entry.basename, history.transcript_suffix)) continue;
            const id = try a.dupe(u8, history.withoutTranscriptSuffix(dir_entry.basename)["agent-".len..]);
            const rel = try a.dupe(u8, dir_entry.path);
            const file = try std.fs.path.join(a, &.{ sub_root, rel });
            var task = try subagentTask(io, a, file, id, links);
            // Workflow members live in subagents/workflows/<run>/.
            var parts = std.mem.splitScalar(u8, rel, std.fs.path.sep);
            const first = parts.next() orelse "";
            const second = parts.next() orelse "";
            if (eql(first, "workflows") and parts.next() != null) task.parent_id = second;
            try out.append(a, task);
        }
    } else |_| {}
    const workflow_dir = try std.fs.path.join(a, &.{ session_dir, "workflows" });
    if (Io.Dir.cwd().openDir(io, workflow_dir, .{ .iterate = true })) |opened| {
        var dir = opened;
        defer dir.close(io);
        var it = dir.iterate();
        while (it.next(io) catch null) |dir_entry| {
            if (dir_entry.kind == .directory or !std.mem.endsWith(u8, dir_entry.name, ".json")) continue;
            if (workflowTask(io, a, try std.fs.path.join(a, &.{ workflow_dir, dir_entry.name }), links)) |task| try out.append(a, task);
        }
    } else |_| {}
    return out.items;
}

fn parentCalls(a: Allocator, graph: history.Graph) Allocator.Error!Links {
    var links: Links = .{};
    for (graph.entries) |entry| {
        for (entry.message.get("content").items()) |value| {
            const block = JsonView.of(value);
            const block_type = block.str("type");
            if (eql(block_type, "tool_use") and (eql(block.str("name"), "Agent") or eql(block.str("name"), "Task") or eql(block.str("name"), "Workflow"))) {
                const input = block.get("input");
                try links.by_id.put(a, block.str("id"), .{ .name = input.str("name"), .subagent_type = input.str("subagent_type"), .description = input.str("description") });
            } else if (eql(block_type, "tool_result")) {
                const call = links.by_id.getPtr(block.str("tool_use_id")) orelse continue;
                call.done = true;
                call.failed = block.flag("is_error");
                call.output, _ = try decode.toolResultContent(a, block.get("content"));
                if (workflowRunId(call.output)) |run| try links.by_run.put(a, run, block.str("tool_use_id"));
            }
        }
    }
    return links;
}

/// The run ID of a "Run ID: wf_..." line.
fn workflowRunId(text: []const u8) ?[]const u8 {
    var lines = std.mem.splitScalar(u8, text, '\n');
    while (lines.next()) |line| {
        if (!std.mem.startsWith(u8, line, "Run ID:")) continue;
        const rest = std.mem.trimStart(u8, line["Run ID:".len..], " \t\r");
        if (!std.mem.startsWith(u8, rest, "wf_")) continue;
        var end: usize = 3;
        while (end < rest.len and (std.ascii.isAlphanumeric(rest[end]) or rest[end] == '-')) end += 1;
        if (end > 3) return rest[0..end];
    }
    return null;
}

fn subagentTask(io: Io, a: Allocator, file: []const u8, id: []const u8, links: Links) Allocator.Error!types.Task {
    var task: types.Task = .{ .id = id, .kind = .subagent, .native_kind = "local_agent", .status = .running };
    var subagent: types.Subagent = .{};
    const meta_path = try std.fmt.allocPrint(a, "{s}.meta.json", .{history.withoutTranscriptSuffix(file)});
    const meta = if (Io.Dir.cwd().readFileAlloc(io, meta_path, a, .limited(1 << 20))) |data| JsonView.parseBytes(a, data) else |_| JsonView.empty;
    const tool_use_id = meta.str("toolUseId");
    task.tool_call_id = tool_use_id;
    subagent.type = meta.str("agentType");
    subagent.depth = meta.uint32("spawnDepth");
    subagent.background = eql(meta.str("requestShape"), "background");
    task.description = meta.str("description");
    if (tool_use_id.len > 0) if (links.by_id.get(tool_use_id)) |call| {
        subagent.name = call.name;
        subagent.type = firstOf(&.{ call.subagent_type, subagent.type });
        task.description = firstOf(&.{ call.description, task.description });
        if (call.done and !subagent.background) {
            task.status = .completed;
            if (call.failed) {
                task.status = .failed;
                task.@"error" = call.output;
            }
        }
    };
    try readSubagentRun(io, a, file, &task, &subagent);
    task.subagent = subagent;
    return task;
}

/// Fills in what a subagent's own transcript tells: its prompt, model,
/// effort, final answer, and the context size of its last request. The
/// final answer is its last SubagentHandback report, else its last text.
fn readSubagentRun(io: Io, a: Allocator, file: []const u8, task: *types.Task, subagent: *types.Subagent) Allocator.Error!void {
    const entries = history.readEntries(io, a, file, history.notApiRequestLine) catch return;
    var last: []const u8 = "";
    var handback: []const u8 = "";
    var last_stop: []const u8 = "";
    for (entries) |entry| {
        const message = entry.message;
        if (eql(entry.type, "user")) {
            if (subagent.prompt.len == 0 and !entry.is_meta) subagent.prompt, _ = try decode.toolResultContent(a, message.get("content"));
        } else if (eql(entry.type, "assistant")) {
            const model = message.str("model");
            if (model.len > 0 and !eql(model, "<synthetic>")) subagent.model = model;
            if (entry.effort.len > 0) subagent.effort = entry.effort;
            const usage = decode.RequestUsage.of(message.get("usage"));
            if (usage.total() > 0) task.usage = .{ .total_tokens = usage.total() };
            for (message.get("content").items()) |value| {
                const block = JsonView.of(value);
                if (eql(block.str("type"), "tool_use") and eql(block.str("name"), decode.handback_tool)) {
                    const report = block.get("input").str("message");
                    if (std.mem.trim(u8, report, " \t\r\n").len > 0) handback = report;
                }
            }
            const text, _ = try decode.toolResultContent(a, message.get("content"));
            if (text.len > 0) last = text;
            last_stop = message.str("stop_reason");
        }
    }
    task.output = firstOf(&.{ handback, last });
    if (task.status == .running and eql(last_stop, "end_turn")) task.status = .completed;
}

fn workflowTask(io: Io, a: Allocator, file: []const u8, links: Links) ?types.Task {
    const data = Io.Dir.cwd().readFileAlloc(io, file, a, .limited(16 << 20)) catch return null;
    const run = JsonView.parseBytes(a, data);
    const run_id = run.str("runId");
    if (run_id.len == 0) return null;
    var task: types.Task = .{
        .id = run_id,
        .kind = .workflow,
        .native_kind = "local_workflow",
        .description = firstOf(&.{ std.mem.trim(u8, run.str("summary"), " \t\r\n"), std.mem.trim(u8, run.str("workflowName"), " \t\r\n") }),
        .tool_call_id = links.by_run.get(run_id) orelse "",
        .output = formatWorkflowResult(a, run.get("result")) catch "",
    };
    // A run summary that is not terminal can no longer finish.
    const status = std.ascii.allocLowerString(a, std.mem.trim(u8, run.str("status"), " \t\r\n")) catch return null;
    task.status = if (eql(status, "completed")) .completed else if (eql(status, "canceled") or eql(status, "cancelled") or eql(status, "killed") or eql(status, "stopped")) .stopped else .failed;
    if (run.int("totalTokens") > 0) task.usage = .{ .total_tokens = run.int("totalTokens") };
    const end = kit.parseTimeMs(run.str("timestamp"));
    const start = run.int("startTime");
    if (end > 0 and start > 0 and end > start) task.duration_ms = end - start;
    return task;
}

/// The result in a workflow's output file, or "" when the file is missing,
/// too large, or holds no result.
pub fn readWorkflowOutput(env: agent.Env, a: Allocator, path: []const u8) []const u8 {
    const data = Io.Dir.cwd().readFileAlloc(env.io, path, a, .limited(max_workflow_output)) catch return "";
    const out = JsonView.parseBytes(a, data);
    if (out.object() == null) return "";
    return formatWorkflowResult(a, out.get("result")) catch "";
}

/// A workflow result as text: objects with a single key are unwrapped,
/// strings, numbers, and booleans are printed as is, and anything else
/// becomes an indented JSON block.
pub fn formatWorkflowResult(a: Allocator, raw: JsonView) Allocator.Error![]const u8 {
    var unwrapped = raw;
    for (0..8) |_| {
        const object = unwrapped.object() orelse break;
        if (object.count() != 1) break;
        unwrapped = JsonView.of(object.values()[0]);
    }
    const val = unwrapped.v orelse return "";
    return switch (val) {
        .null => "",
        .string => |s| std.mem.trim(u8, s, " \t\r\n"),
        .integer => |n| std.fmt.allocPrint(a, "{d}", .{n}),
        .float => |f| std.fmt.allocPrint(a, "{d}", .{f}),
        .number_string => |s| s,
        .bool => |b| if (b) "true" else "false",
        else => std.fmt.allocPrint(a, "```json\n{s}\n```", .{try std.json.Stringify.valueAlloc(a, val, .{ .whitespace = .indent_2 })}),
    };
}

test "workflow results" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try std.testing.expectEqualStrings("done", try formatWorkflowResult(a, JsonView.parseBytes(a, "{\"r\":{\"x\":\" done \"}}")));
    try std.testing.expectEqualStrings("```json\n[\n  1\n]\n```", try formatWorkflowResult(a, JsonView.parseBytes(a, "[1]")));
    try std.testing.expectEqualStrings("wf_ab-1", workflowRunId("x\nRun ID: wf_ab-1 more").?);
}
