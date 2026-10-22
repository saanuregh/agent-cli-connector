//! Turns Pi session events into uniform events. Runs on the reader task.
const std = @import("std");
const Allocator = std.mem.Allocator;
const agent = @import("agent");
const types = agent.types;
const kit = agent.providerkit;
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const Id = agent.Id;
const eql = kit.eql;
const log = std.log.scoped(.agent);
const decode = @import("decode.zig");
const tool_output = @import("tool_output.zig");
const PiSession = @import("session.zig").PiSession;

/// A tool call item with the memory it points to.
const ToolEntry = kit.Owned(types.Item);

/// A running subagent task of a tool call, which is its `tool_call_id`.
const Subtask = kit.Owned(types.Task);

/// A user message item waiting for the bridge to report its entry ID.
const PendingUser = kit.Owned(struct {
    item: types.Item,
    stamp: i64,
    /// The turn's first user message, whose entry locates the turn.
    first: bool,
    raw: RawJson,
});

/// The state of one run, cleared when the run settles.
const Run = struct {
    cycle_started: bool = false,
    usage: types.Usage = .{},
    steps: u32 = 0,
    text: []const u8 = "",
    error_message: []const u8 = "",
    error_detail: []const u8 = "",
    stop_reason: []const u8 = "",
    /// The arguments of the run's structured_output call.
    output: ?RawJson = null,
};

/// The Pi session events the translator maps; others become `native`
/// events named after their type.
const EventType = enum {
    message_update,
    agent_start,
    turn_start,
    message_start,
    message_end,
    tool_execution_start,
    tool_execution_update,
    tool_execution_end,
    agent_settled,
    queue_update,
    compaction_start,
    compaction_end,
    auto_retry_start,
    auto_retry_end,
    summarization_retry_scheduled,
    summarization_retry_finished,
    session_info_changed,
    thinking_level_changed,
    bash_execution_update,
    extension_error,
    entry_appended,
    agent_end,
    turn_end,
};

/// The kinds of streaming assistant message updates.
const UpdateKind = enum {
    text_start,
    thinking_start,
    text_delta,
    thinking_delta,
    toolcall_start,
    toolcall_delta,
    toolcall_end,
};

pub const Translator = struct {
    pi: *PiSession,
    gpa: Allocator,
    /// Makes the item IDs of assistant messages unique to this session,
    /// since Pi gives a message no ID until it ended.
    id_prefix: Id,
    messages: u64 = 0,
    message_key: Id = .{},
    blocks: std.AutoHashMapUnmanaged(i64, Id) = .empty,
    tools: std.StringArrayHashMapUnmanaged(*ToolEntry) = .empty,
    /// Latest partial result per tool call.
    partial: kit.OwnedMap(tool_output.Partial) = .{},
    users: u64 = 0,
    run_arena: std.heap.ArenaAllocator,
    run: Run = .{},
    context_tokens: i64 = 0,
    live_total: i64 = 0,
    /// The latest retry of a compaction or branch summary, until the
    /// retries finish.
    summary_attempt: u32 = 0,
    pending_users: std.ArrayList(*PendingUser) = .empty,
    subtasks: std.ArrayList(*Subtask) = .empty,

    pub fn init(pi: *PiSession) Translator {
        var prefix: [32]u8 = undefined;
        const id = pi.provider.env.uuid();
        const id_prefix = std.fmt.bufPrint(&prefix, "msg-{s}-", .{id.slice()[0..8]}) catch unreachable;
        return .{ .pi = pi, .gpa = pi.gpa, .id_prefix = .of(id_prefix), .run_arena = .init(pi.gpa) };
    }

    pub fn deinit(translator: *Translator) void {
        const gpa = translator.gpa;
        translator.blocks.deinit(gpa);
        for (translator.tools.values()) |entry| entry.destroy(gpa);
        translator.tools.deinit(gpa);
        translator.partial.deinit(gpa);
        for (translator.pending_users.items) |pending| pending.destroy(gpa);
        translator.pending_users.deinit(gpa);
        for (translator.subtasks.items) |subtask| subtask.destroy(gpa);
        translator.subtasks.deinit(gpa);
        translator.run_arena.deinit();
    }

    fn emit(translator: *Translator, raw: ?RawJson, payload: types.Event.Payload) void {
        translator.pi.rt().emit(.{ .raw = raw, .payload = payload });
    }

    fn emitTurn(translator: *Translator, turn: []const u8, raw: ?RawJson, payload: types.Event.Payload) void {
        translator.pi.rt().emit(.{ .turn_id = turn, .raw = raw, .payload = payload });
    }

    pub fn handle(translator: *Translator, a: Allocator, event_type: []const u8, event: JsonView, line: []const u8) !void {
        const raw: RawJson = .of(line);
        const known = std.meta.stringToEnum(EventType, event_type) orelse
            return translator.emit(raw, .{ .native = .{ .name = event_type, .data = raw } });
        switch (known) {
            // The bridge reports user entries before Pi prints these events.
            .message_start, .turn_end, .agent_end, .agent_settled => translator.flushUsers(),
            else => {},
        }
        const pi = translator.pi;
        switch (known) {
            .message_update => {
                const update = event.get("assistantMessageEvent");
                if (update.object() != null) try translator.messageUpdate(a, update, raw);
                translator.liveUsage(event.get("usage"), raw);
            },
            .agent_start => {
                // A run the host did not start, such as one an extension
                // triggered, becomes a tracked turn. Host input in flight
                // makes the runtime busy or the run's start pending.
                const unsolicited = pi.rt().idle();
                const current, const starting = pi.rt().runStarted("");
                if (translator.run.cycle_started) return;
                translator.run.cycle_started = true;
                translator.emitTurn(current.slice(), raw, .{ .turn_started = .{
                    .origin = if (unsolicited and !starting) "extension" else "",
                } });
                translator.emitTurn(current.slice(), raw, .{ .status_changed = .{ .status = .running } });
            },
            .turn_start => translator.run.steps += 1,
            .message_start => if (eql(event.get("message").str("role"), "assistant")) {
                translator.messages += 1;
                var buf: [160]u8 = undefined;
                translator.message_key = .of(std.fmt.bufPrint(&buf, "{s}{d}", .{ translator.id_prefix.slice(), translator.messages }) catch "");
                translator.blocks.clearRetainingCapacity();
            },
            .message_end => try translator.messageEnd(a, event.get("message"), raw),
            .tool_execution_start => try translator.toolStart(a, event, raw),
            .tool_execution_update => try translator.toolUpdate(a, event, raw),
            .tool_execution_end => try translator.toolExecutionEnd(a, event, raw),
            .agent_settled => try translator.settled(a, event.flag("aborted"), raw),
            .queue_update => {
                const data = try RawJson.encode(a, .{ .steering = try event.get("steering").strings(a), .follow_up = try event.get("followUp").strings(a) });
                translator.emit(raw, .{ .native = .{ .name = "pi/queue_updated", .data = data } });
            },
            .compaction_start => translator.compactionStart(event, raw),
            .compaction_end => translator.compactionEnd(event, raw),
            .auto_retry_start => translator.emit(raw, .{ .retry = .{ .attempt = event.uint32("attempt"), .max_attempts = event.uint32("maxAttempts"), .delay_ms = event.int("delayMs"), .message = event.str("errorMessage") } }),
            .auto_retry_end => if (event.flag("success")) {
                translator.emit(raw, .{ .retry = .{ .attempt = event.uint32("attempt"), .done = true } });
            } else translator.emit(raw, .{ .notice = .{ .level = .@"error", .code = "retry_failed", .message = event.str("finalError") } }),
            .summarization_retry_scheduled => {
                // Compaction and branch summaries retry like model requests;
                // a failure that ends the retries arrives as compaction_end.
                translator.summary_attempt = event.uint32("attempt");
                translator.emit(raw, .{ .retry = .{
                    .attempt = translator.summary_attempt,
                    .max_attempts = event.uint32("maxAttempts"),
                    .delay_ms = event.int("delayMs"),
                    .message = try std.fmt.allocPrint(a, "summary: {s}", .{event.str("errorMessage")}),
                } });
            },
            .summarization_retry_finished => if (translator.summary_attempt > 0) {
                translator.emit(raw, .{ .retry = .{ .attempt = translator.summary_attempt, .done = true } });
                translator.summary_attempt = 0;
            },
            .session_info_changed => try pi.setInfoField(true, .title, event.str("name")),
            .thinking_level_changed => try pi.setInfoField(true, .effort, decode.effortOf(event.str("level"))),
            .bash_execution_update => translator.emit(raw, .{ .tool_output_delta = .{ .item_id = try std.fmt.allocPrint(a, "bash-{s}", .{event.str("id")}), .delta = event.str("delta") } }),
            .extension_error => {
                const data = try RawJson.encode(a, .{ .extension_path = event.str("extensionPath"), .event = event.str("event"), .@"error" = event.str("error") });
                translator.emit(raw, .{ .native = .{ .name = "pi/extension_error", .data = data } });
                translator.emit(raw, .{ .notice = .{ .level = .@"error", .code = "extension_error", .message = try std.fmt.allocPrint(a, "{s}: {s}", .{ event.str("extensionPath"), event.str("error") }) } });
            },
            .entry_appended => {
                const data = try RawJson.encode(a, .{ .entry = try event.get("entry").raw(a) });
                translator.emit(raw, .{ .native = .{ .name = "pi/entry_appended", .data = data } });
            },
            .agent_end, .turn_end => {},
        }
    }

    fn toolStart(translator: *Translator, a: Allocator, event: JsonView, raw: RawJson) !void {
        const entry = try translator.toolItem(event.str("toolCallId"), event.str("toolName"), event.str("parentToolCallId"), raw);
        const args = event.get("args");
        if (args.isNull()) return;
        const new_input = (try args.raw(a)).?;
        const old = if (entry.value.tool.?.input) |input| input.bytes else "";
        if (!eql(old, new_input.bytes)) try translator.setInput(entry, args, raw);
    }

    fn toolUpdate(translator: *Translator, a: Allocator, event: JsonView, raw: RawJson) !void {
        const call_id = event.str("toolCallId");
        const result = event.get("partialResult");
        var current = try tool_output.Partial.parse(a, result);
        const previous = translator.partial.get(call_id) orelse tool_output.Partial{};
        const delta = try tool_output.delta(a, previous, &current);
        try translator.partial.put(translator.gpa, call_id, try current.dupe(translator.gpa));
        if (delta.len > 0) translator.emit(raw, .{ .tool_output_delta = .{ .item_id = call_id, .delta = delta } });
        if (eql(event.str("toolName"), "subagent")) try translator.subagentToolTasks(a, call_id, result, translator.toolArgs(a, call_id), false, raw);
    }

    fn toolExecutionEnd(translator: *Translator, a: Allocator, event: JsonView, raw: RawJson) !void {
        const call_id = event.str("toolCallId");
        const tool_name = event.str("toolName");
        const result = event.get("result");
        const is_error = event.flag("isError");
        const args = translator.toolArgs(a, call_id);
        try translator.toolEnd(a, event, raw);
        if (eql(tool_name, "subagent")) try translator.subagentToolTasks(a, call_id, result, args, true, raw);
        // Tasks the result did not report finished, as when the tool
        // failed or was aborted, end with it.
        if (is_error) {
            const text = try decode.resultText(a, result);
            translator.closeSubtasks(call_id, .failed, kit.firstOf(&.{ text, "the subagent tool failed" }), raw);
        } else translator.closeSubtasks(call_id, .stopped, "", raw);
        if (eql(tool_name, decode.output_tool) and !is_error) {
            const details = result.get("details");
            if (!details.isNull()) translator.run.output = .of(try translator.run_arena.allocator().dupe(u8, (try details.raw(a)).?.bytes));
        }
        if (eql(tool_name, "todo") and !is_error) {
            if (try decode.todoSteps(a, result)) |steps| translator.emit(raw, .{ .plan_updated = .{ .steps = steps, .item_id = call_id } });
        }
    }

    fn compactionStart(translator: *Translator, event: JsonView, raw: RawJson) void {
        const pi = translator.pi;
        pi.compacting.store(true, .release);
        const id, const pending = pi.interrupting();
        if (pending and !pi.rt().isLocal(id.slice())) {
            // Interrupted while the prompt started, before Pi began this
            // compaction: an abort now stops it. Sent off the reader task,
            // which delivers the answer.
            if (!pi.spawn(abortWorker, .{})) log.err("pi: cannot abort the compaction of an interrupted turn: the session is closing or no task could start", .{});
        }
        translator.emit(raw, .{ .compaction = .{ .trigger = event.str("reason") } });
        translator.emit(raw, .{ .status_changed = .{ .status = .compacting } });
    }

    /// Aborts the running compaction. Runs as a worker.
    fn abortWorker(pi: *PiSession) void {
        var arena: std.heap.ArenaAllocator = .init(pi.gpa);
        defer arena.deinit();
        var diag: agent.Diagnostics = .{};
        _ = pi.command(arena.allocator(), "abort", .{}, &diag) catch |e|
            log.err("pi: aborting the compaction of an interrupted turn failed: {s}", .{agent.Diagnostics.of(&diag, e).message});
    }

    fn compactionEnd(translator: *Translator, event: JsonView, raw: RawJson) void {
        const pi = translator.pi;
        pi.compacting.store(false, .release);
        const result = event.get("result");
        var compaction: @FieldType(types.Event.Payload, "compaction") = .{ .done = true, .trigger = event.str("reason") };
        const current = pi.rt().runningTurn()[0];
        const user_run = !current.isEmpty() and !pi.rt().isLocal(current.slice());
        if (result.object() != null) {
            // The summary's model call is part of the run's cost, including
            // a compaction Pi runs before the prompt's run starts. Built-in
            // commands report it in their own result.
            if (user_run and !result.get("usage").isNull()) translator.run.usage.add(decode.usage(result.get("usage")));
            compaction.summary = result.str("summary");
            compaction.tokens_before = result.int("tokensBefore");
            compaction.tokens_after = result.int("estimatedTokensAfter");
        } else if (event.flag("aborted")) {
            compaction.@"error" = "compaction aborted";
        } else compaction.@"error" = kit.firstOf(&.{ event.str("errorMessage"), "compaction failed" });
        translator.emit(raw, .{ .compaction = compaction });
        translator.emit(raw, .{ .status_changed = .{ .status = if (user_run) .running else .idle } });
        // Best effort: the usage refresh only corrects the context estimate.
        if (result.object() != null) _ = pi.spawn(refreshContextUsage, .{ current, translator.turnUsage() });
    }

    /// Reports the streaming message's cumulative usage, which Pi attaches
    /// to message updates, on top of the turn's finished messages.
    fn liveUsage(translator: *Translator, fields: JsonView, raw: RawJson) void {
        if (fields.object() == null) return;
        const total = fields.int("totalTokens");
        if (total <= 0 or total == translator.live_total) return;
        translator.live_total = total;
        var usage = translator.run.usage;
        usage.add(decode.usage(fields));
        usage.context_tokens = decode.contextTokens(fields);
        translator.emit(raw, .{ .usage_updated = .{ .usage = usage } });
    }

    /// The turn's usage with the context the last finished message filled.
    fn turnUsage(translator: *Translator) types.Usage {
        var usage = translator.run.usage;
        usage.context_tokens = translator.context_tokens;
        usage.context_window = translator.pi.rt().contextWindow();
        return usage;
    }

    fn blockKey(translator: *Translator, a: Allocator, index: i64) ![]const u8 {
        return std.fmt.allocPrint(a, "{s}/{d}", .{ translator.message_key.slice(), index });
    }

    fn blockId(translator: *Translator, a: Allocator, index: i64) ![]const u8 {
        if (translator.blocks.get(index)) |id| return a.dupe(u8, id.slice());
        return translator.blockKey(a, index);
    }

    fn messageUpdate(translator: *Translator, a: Allocator, update: JsonView, raw: RawJson) !void {
        const kind = std.meta.stringToEnum(UpdateKind, update.str("type")) orelse return;
        const index = update.int("contentIndex");
        switch (kind) {
            .text_start, .thinking_start => {
                const id = try translator.blockKey(a, index);
                try translator.blocks.put(translator.gpa, index, .of(id));
                translator.emit(raw, .{ .item_started = .{ .item = .{ .id = id, .kind = if (kind == .text_start) .assistant_message else .reasoning, .status = .in_progress } } });
            },
            .text_delta => translator.emit(raw, .{ .text_delta = .{ .item_id = try translator.blockId(a, index), .delta = update.str("delta") } }),
            .thinking_delta => translator.emit(raw, .{ .reasoning_delta = .{ .item_id = try translator.blockId(a, index), .delta = update.str("delta") } }),
            .toolcall_start => {
                try translator.blocks.put(translator.gpa, index, .of(update.str("id")));
                _ = try translator.toolItem(update.str("id"), update.str("toolName"), "", raw);
            },
            .toolcall_delta => if (update.str("delta").len > 0) {
                translator.emit(raw, .{ .tool_input_delta = .{ .item_id = try translator.blockId(a, index), .delta = update.str("delta") } });
            },
            .toolcall_end => {
                const call = update.get("toolCall");
                if (call.object() != null) {
                    const entry = try translator.toolItem(call.str("id"), call.str("name"), "", raw);
                    try translator.setInput(entry, call.get("arguments"), raw);
                }
            },
        }
    }

    /// The tool call item for `id`, emitting item_started when it is new.
    fn toolItem(translator: *Translator, id: []const u8, tool_name: []const u8, parent: []const u8, raw: RawJson) !*ToolEntry {
        if (translator.tools.get(id)) |entry| return entry;
        var call: types.ToolCall = .{ .name = tool_name, .kind = decode.toolKind(tool_name, translator.pi.cfg.tools) };
        if (call.kind == .mcp) {
            const rest = tool_name["mcp__".len..];
            call.server = rest[0 .. std.mem.indexOf(u8, rest, "__") orelse rest.len];
        }
        const entry = try ToolEntry.create(translator.gpa, .{ .id = id, .kind = .tool_call, .status = .in_progress, .parent_id = parent, .tool = call });
        errdefer entry.destroy(translator.gpa);
        try translator.tools.put(translator.gpa, entry.value.id, entry);
        translator.emit(raw, .{ .item_started = .{ .item = entry.value } });
        return entry;
    }

    /// Sets a tool call's input and reports the item.
    fn setInput(translator: *Translator, entry: *ToolEntry, args: JsonView, raw: RawJson) !void {
        const entry_arena = entry.arena.allocator();
        var item = entry.value;
        var call = item.tool.?;
        call.input = try args.raw(entry_arena);
        try decode.fillToolDetails(entry_arena, &call, args);
        item.tool = call;
        entry.value = try agent.clone(types.Item, entry_arena, item);
        translator.emit(raw, .{ .item_updated = .{ .item = entry.value } });
    }

    fn toolArgs(translator: *Translator, a: Allocator, call_id: []const u8) JsonView {
        const entry = translator.tools.get(call_id) orelse return .empty;
        const input = entry.value.tool.?.input orelse return .empty;
        return JsonView.parseBytes(a, input.bytes);
    }

    fn toolEnd(translator: *Translator, a: Allocator, event: JsonView, raw: RawJson) !void {
        const pi = translator.pi;
        const call_id = event.str("toolCallId");
        const result = event.get("result");
        const entry = try translator.toolItem(call_id, event.str("toolName"), event.str("parentToolCallId"), raw);
        defer {
            _ = translator.tools.orderedRemove(call_id);
            entry.destroy(translator.gpa);
        }
        if (translator.partial.get(call_id)) |partial| if (partial.open and tool_output.finalNewline(pi.io, partial, result)) {
            translator.emit(raw, .{ .tool_output_delta = .{ .item_id = call_id, .delta = "\n" } });
        };
        translator.partial.remove(translator.gpa, call_id);
        var item = entry.value;
        var call = item.tool.?;
        const content = try decode.resultContent(a, result);
        call.output = content.text;
        call.images = content.images;
        call.is_error = event.flag("isError");
        if (event.has("durationMs")) call.duration_ms = event.int("durationMs");
        try decode.applyToolResult(a, &call, result);
        item.tool = call;
        item.status = if (pi.takeDenied(call_id)) .declined else if (call.is_error) .failed else .completed;
        translator.emit(raw, .{ .item_completed = .{ .item = item } });
    }

    fn messageEnd(translator: *Translator, a: Allocator, message: JsonView, raw: RawJson) !void {
        switch (decode.Role.of(message)) {
            .assistant => try translator.assistantEnd(a, message, raw),
            .user => try translator.userEnd(a, message, raw),
            .custom => {
                if (!message.flag("display")) return translator.emit(raw, .{ .native = .{ .name = "custom_message", .data = raw } });
                const id = try std.fmt.allocPrint(a, "custom-{d}", .{message.int("timestamp")});
                translator.emit(raw, .{ .item_completed = .{ .item = .{ .id = id, .kind = .other, .text = try decode.messageText(a, message.get("content")), .raw = raw } } });
            },
            .compactionSummary, .branchSummary => {
                const id = try std.fmt.allocPrint(a, "{s}-{d}", .{ message.str("role"), message.int("timestamp") });
                translator.emit(raw, .{ .item_completed = .{ .item = .{ .id = id, .kind = .compaction, .text = message.str("summary"), .raw = raw } } });
            },
            .toolResult, .hookMessage, .bashExecution, .other => {},
        }
    }

    fn assistantEnd(translator: *Translator, a: Allocator, message: JsonView, raw: RawJson) !void {
        const stop = message.str("stopReason");
        const status: types.ItemStatus = if (eql(stop, "aborted")) .cancelled else .completed;
        var texts: std.ArrayList([]const u8) = .empty;
        for (try decode.blocks(a, message.get("content")), 0..) |block, i| {
            const index: i64 = @intCast(i);
            switch (block.kind()) {
                .text, .thinking => {
                    const thinking = block.kind() == .thinking;
                    const kind: types.ItemKind = if (thinking) .reasoning else .assistant_message;
                    const text = if (thinking) block.thinking else block.text;
                    if (!thinking and text.len > 0) try texts.append(a, text);
                    const id = if (translator.blocks.get(index)) |known| try a.dupe(u8, known.slice()) else blk: {
                        const id = try translator.blockKey(a, index);
                        translator.emit(raw, .{ .item_started = .{ .item = .{ .id = id, .kind = kind, .status = .in_progress } } });
                        break :blk id;
                    };
                    translator.emit(raw, .{ .item_completed = .{ .item = .{ .id = id, .kind = kind, .status = status, .text = text, .raw = try block.raw.raw(a) } } });
                },
                .toolCall => {
                    const entry = try translator.toolItem(block.id, block.name, "", raw);
                    if (entry.value.tool.?.input == null) try translator.setInput(entry, block.arguments, raw);
                },
                .image, .other => {},
            }
        }
        const run_allocator = translator.run_arena.allocator();
        if (texts.items.len > 0) translator.run.text = try std.mem.join(run_allocator, "\n", texts.items);
        translator.live_total = 0;
        const usage = message.get("usage");
        if (usage.object() != null) {
            translator.run.usage.add(decode.usage(usage));
            translator.context_tokens = decode.contextTokens(usage);
            translator.emit(raw, .{ .usage_updated = .{ .usage = translator.turnUsage() } });
        }
        translator.run.stop_reason = try run_allocator.dupe(u8, stop);
        if (eql(stop, "error")) {
            translator.run.error_message = try run_allocator.dupe(u8, message.str("errorMessage"));
            translator.run.error_detail = try run_allocator.dupe(u8, try decode.errorDetail(a, message));
            translator.emit(raw, .{ .notice = .{ .level = .@"error", .code = "model_error", .message = message.str("errorMessage") } });
        } else if (!eql(stop, "aborted")) {
            translator.run.error_message = "";
            translator.run.error_detail = "";
        }
    }

    fn userEnd(translator: *Translator, a: Allocator, message: JsonView, raw: RawJson) !void {
        const pi = translator.pi;
        translator.users += 1;
        const text = try decode.messageText(a, message.get("content"));
        const current = pi.rt().runningTurn()[0];
        const stamp = message.int("timestamp");
        const first = !current.isEmpty() and try pi.recordTurnStamp(current.slice(), stamp);
        const item: types.Item = .{
            .id = try std.fmt.allocPrint(a, "user-{d}-{d}", .{ translator.users, stamp }),
            .kind = .user_message,
            .text = text,
            .turn_id = current.slice(),
            .client_id = try pi.takeClientId(a, text),
            .time_ms = stamp,
            .raw = raw,
        };
        if (pi.bridgeDisabled()) return translator.emit(raw, .{ .item_completed = .{ .item = item } });
        // The item waits for its entry ID, which the bridge reports before
        // Pi's next event.
        const pending = try PendingUser.create(translator.gpa, .{ .item = item, .stamp = stamp, .first = first, .raw = raw });
        errdefer pending.destroy(translator.gpa);
        try translator.pending_users.append(translator.gpa, pending);
    }

    /// Gives a waiting user message item the session entry ID the bridge
    /// reported, and emits it.
    pub fn userEntry(translator: *Translator, a: Allocator, text: []const u8) !void {
        const reported = JsonView.parseBytes(a, text);
        const entry = reported.str("entryId");
        if (entry.len == 0) return;
        const stamp = reported.int("timestamp");
        for (translator.pending_users.items, 0..) |pending, i| if (pending.value.stamp == stamp) {
            _ = translator.pending_users.orderedRemove(i);
            defer pending.destroy(translator.gpa);
            const user = &pending.value;
            user.item.id = entry;
            if (user.first and user.item.turn_id.len > 0) translator.pi.recordTurnEntry(user.item.turn_id, entry);
            translator.emit(user.raw, .{ .item_completed = .{ .item = user.item } });
            return;
        };
    }

    /// Emits user message items whose entry ID never arrived, under their
    /// provisional IDs.
    fn flushUsers(translator: *Translator) void {
        for (translator.pending_users.items) |pending| {
            translator.emit(pending.value.raw, .{ .item_completed = .{ .item = pending.value.item } });
            pending.destroy(translator.gpa);
        }
        translator.pending_users.clearRetainingCapacity();
    }

    /// Completes the current turn when Pi has no more automatic work.
    fn settled(translator: *Translator, a: Allocator, aborted_event: bool, raw: RawJson) !void {
        const pi = translator.pi;
        const rt = pi.rt();
        const current = rt.runningTurn()[0];
        var aborted = aborted_event;
        const interrupted = pi.interrupting()[0];
        if (!interrupted.isEmpty() and interrupted.eql(current.slice())) aborted = true;

        const usage = translator.turnUsage();
        var result: types.TurnResult = .{
            .text = try a.dupe(u8, translator.run.text),
            .usage = usage,
            .steps = translator.run.steps,
            .stop_reason = try a.dupe(u8, translator.run.stop_reason),
            .raw = raw,
        };
        if (aborted) {
            result.status = .interrupted;
            result.@"error" = .{ .kind = .interrupted, .code = "aborted", .message = "turn interrupted" };
        } else if (translator.run.error_message.len > 0 or eql(translator.run.stop_reason, "error")) {
            result.status = .failed;
            var message = kit.firstOf(&.{ translator.run.error_message, "the model request failed" });
            if (translator.run.error_detail.len > 0) message = try std.fmt.allocPrint(a, "{s} ({s})", .{ message, translator.run.error_detail });
            result.@"error" = .{ .kind = .turn_failed, .code = "error", .message = try a.dupe(u8, message) };
        }
        const wanted, const wrapped = pi.takeOutput(current.slice());
        if (wanted) try translator.structuredOutput(a, &result, wrapped);
        _ = translator.run_arena.reset(.retain_capacity);
        translator.run = .{};
        // Tool calls that never finished, such as ones the abort cut off.
        for (translator.tools.values()) |entry| {
            var item = entry.value;
            item.status = .cancelled;
            translator.emit(raw, .{ .item_completed = .{ .item = item } });
            translator.partial.remove(translator.gpa, item.id);
            entry.destroy(translator.gpa);
        }
        translator.tools.clearRetainingCapacity();
        while (translator.subtasks.items.len > 0) {
            const call_id = try a.dupe(u8, translator.subtasks.items[0].value.tool_call_id);
            translator.closeSubtasks(call_id, .stopped, "", raw);
        }

        if (!current.isEmpty()) {
            if (rt.hasTurn(current.slice())) {
                rt.finishTurn(current.slice(), result);
            } else rt.abandonTurn(current.slice(), .{ .kind = .internal }); // frees the slot
        }
        // Ending the turn may start queued input, which is not idle.
        if (rt.idle()) translator.emit(raw, .{ .status_changed = .{ .status = .idle } });
        // Best effort: the usage refresh only corrects the context estimate.
        _ = pi.spawn(refreshContextUsage, .{ current, usage });
    }

    /// Sets the structured output of a turn that asked for it, or fails a
    /// turn that otherwise succeeded without it.
    fn structuredOutput(translator: *Translator, a: Allocator, result: *types.TurnResult, wrapped: bool) !void {
        var out: ?RawJson = if (translator.run.output) |output| RawJson.of(try a.dupe(u8, output.bytes)) else null;
        if (wrapped) if (out) |output| {
            const value = JsonView.parseBytes(a, output.bytes).get("value");
            out = try value.raw(a);
        };
        if (out) |output| {
            result.structured_output = output;
            return;
        }
        if (result.status == .completed) {
            result.status = .failed;
            result.@"error" = .{ .kind = .turn_failed, .code = "no_structured_output", .message = "the model did not call " ++ decode.output_tool };
        }
    }

    /// Reports the runs of a subagent tool as subagent tasks and remembers
    /// their session files.
    fn subagentToolTasks(translator: *Translator, a: Allocator, call_id: []const u8, result: JsonView, args: JsonView, done: bool, raw: RawJson) !void {
        const found = try decode.subagentTasks(a, call_id, result, args, done);
        try translator.pi.addSubagentFiles(found.files);
        for (found.tasks) |task| {
            const existing = for (translator.subtasks.items, 0..) |subtask, i| {
                if (eql(subtask.value.tool_call_id, call_id) and eql(subtask.value.id, task.id)) break i;
            } else null;
            if (existing) |i| translator.subtasks.orderedRemove(i).destroy(translator.gpa);
            if (task.status == .running) {
                const subtask = try Subtask.create(translator.gpa, task);
                errdefer subtask.destroy(translator.gpa);
                try translator.subtasks.append(translator.gpa, subtask);
            }
            translator.emit(raw, .{ .task_updated = .{ .task = task } });
        }
    }

    /// Ends the subagent tasks of tool call `call_id` still running with
    /// `status`.
    fn closeSubtasks(translator: *Translator, call_id: []const u8, status: types.TaskStatus, err_text: []const u8, raw: RawJson) void {
        var i: usize = 0;
        while (i < translator.subtasks.items.len) {
            const subtask = translator.subtasks.items[i];
            if (!eql(subtask.value.tool_call_id, call_id)) {
                i += 1;
                continue;
            }
            _ = translator.subtasks.orderedRemove(i);
            var task = subtask.value;
            task.status = status;
            task.@"error" = err_text;
            translator.emit(raw, .{ .task_updated = .{ .task = task } });
            subtask.destroy(translator.gpa);
        }
    }
};

/// Reports Pi's context-window estimate, which stays right after a
/// compaction, unlike the last message's usage. Runs as a worker; a
/// failure keeps the earlier estimate.
fn refreshContextUsage(pi: *PiSession, turn: Id, turn_usage: types.Usage) void {
    var arena: std.heap.ArenaAllocator = .init(pi.gpa);
    defer arena.deinit();
    const stats = pi.commandTimeout(arena.allocator(), "get_session_stats", .{}, 10_000, null) catch return;
    const context_usage = stats.get("contextUsage");
    if (context_usage.get("tokens").isNull()) return;
    const tokens = context_usage.int("tokens");
    const window = context_usage.int("contextWindow");
    var usage = turn_usage;
    if (tokens == usage.context_tokens and (window == 0 or window == usage.context_window)) return;
    usage.context_tokens = tokens;
    if (window > 0) usage.context_window = window;
    const current = pi.rt().runningTurn()[0];
    // A later turn started; its own usage reports supersede this.
    if (!current.isEmpty() and !current.eql(turn.slice())) return;
    pi.rt().emit(.{ .turn_id = turn.slice(), .payload = .{ .usage_updated = .{ .usage = usage } } });
}
