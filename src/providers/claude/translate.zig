//! Turns the CLI's stdout messages into events. It runs on the reader
//! task, except for the state guarded by `task_mutex` (tasks and effort,
//! also used by hook tasks), `lookup_mutex` (lookups rewind, fork, and
//! native calls use), and `tail_mutex` (shell output tails).
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const agent = @import("agent");
const types = agent.types;
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const clone = agent.clone;
const kit = agent.providerkit;
const eql = kit.eql;
const firstOf = kit.firstOf;

const decode = @import("decode.zig");
const launch_mod = @import("launch.zig");
const subagents = @import("subagents.zig");
const tool_output = @import("tool_output.zig");
const control = @import("control.zig");
const permissions = @import("permissions.zig");
const ClaudeSession = @import("session.zig").ClaudeSession;
const Owned = kit.Owned;
const OwnedMap = kit.OwnedMap;
const log = std.log.scoped(.agent);

/// Where an event came from: the turn it belongs to (empty means the
/// runtime's current turn) and the native message.
pub const Meta = struct { turn_id: []const u8 = "", raw: ?RawJson = null };

const BlockRef = struct { item_id: []const u8, kind: types.ItemKind, parent: []const u8, subagent: []const u8 };
/// The assistant frames of one message seen so far (frame n carries content
/// block n) and the parent tool call of its thread.
const Frames = struct { n: u32 = 0, parent: []const u8 = "" };
/// A tool call waiting for its result and the turn it started in.
const ToolEntry = struct { item: types.Item, turn: []const u8 = "" };
/// An item of a subagent frame that arrived before task_started named its
/// subagent. A done item's completion waits for the subagent.
const Held = struct { item: types.Item, turn_id: []const u8 = "", raw: ?RawJson = null, done: bool = false };

/// Bounds effort remembered for subagents not seen yet.
const max_pending_effort = 256;
/// Bounds the walk up nested subagent calls.
const max_subagent_depth = 16;

pub const Translator = struct {
    claude: *ClaudeSession,
    gpa: Allocator,

    /// "messageID/index" to the item a stream content block belongs to.
    blocks: OwnedMap(*Owned(BlockRef)) = .{},
    /// The message being streamed per parent tool call ("" is the main
    /// conversation).
    stream_message: OwnedMap([]const u8) = .{},
    frames: OwnedMap(*Owned(Frames)) = .{},
    tools: OwnedMap(*Owned(ToolEntry)) = .{},
    /// A spawning tool call to the subagent it started.
    agents: OwnedMap([]const u8) = .{},
    /// Mirrors the TaskCreate/TaskUpdate task list.
    plan: ?*Owned([]const types.PlanStep) = null,
    started_turns: kit.StringSet = .{},
    last_init: []u8 = &.{},
    pending_items: OwnedMap(*Owned([]const Held)) = .{},
    /// The model of a not-yet-named subagent's first frame, by parent call.
    subagent_models: OwnedMap([]const u8) = .{},
    tail_mutex: Io.Mutex = .init,
    tails: OwnedMap(*tool_output.Tail) = .{},
    /// The API error class and text of the current turn's assistant error
    /// frame, and when the latest exhausted rate limit resets.
    turn_error: []u8 = &.{},
    turn_error_text: []u8 = &.{},
    limit_resets_at_ms: i64 = 0,
    turn_usage: types.Usage = .{},
    context_window: i64 = 0,
    context_tokens: i64 = 0,
    request: decode.RequestUsage = .{},
    main_model: []u8 = &.{},
    /// SubagentHandback calls, which become assistant messages.
    hidden: kit.StringSet = .{},
    compacting: bool = false,
    session_missing: std.atomic.Value(bool) = .init(false),
    drops_turn_rejected: std.atomic.Value(bool) = .init(false),
    plugin_errors: []u8 = &.{},

    task_mutex: Io.Mutex = .init,
    tasks: OwnedMap(*Owned(types.Task)) = .{},
    /// Effort reported before the subagent's task_started.
    pending_effort: OwnedMap([]const u8) = .{},

    lookup_mutex: Io.Mutex = .init,
    last_cost: f64 = 0,
    /// A wire message UUID to the items it produced, and back; a turn to
    /// its latest main assistant message UUID.
    message_items: OwnedMap(*Owned([]const []const u8)) = .{},
    item_message: OwnedMap([]const u8) = .{},
    cursors: OwnedMap([]const u8) = .{},
    /// The model-scoped allowance get_usage reported, which rate-limit
    /// events call "seven_day_overage_included".
    overage_limit: []u8 = &.{},

    pub fn create(claude: *ClaudeSession) Allocator.Error!*Translator {
        const translator = try claude.gpa().create(Translator);
        translator.* = .{ .claude = claude, .gpa = claude.gpa() };
        return translator;
    }

    pub fn destroy(translator: *Translator) void {
        const gpa = translator.gpa;
        translator.blocks.deinit(gpa);
        translator.stream_message.deinit(gpa);
        translator.frames.deinit(gpa);
        translator.tools.deinit(gpa);
        translator.agents.deinit(gpa);
        if (translator.plan) |plan| plan.destroy(gpa);
        translator.started_turns.deinit(gpa);
        gpa.free(translator.last_init);
        translator.pending_items.deinit(gpa);
        translator.subagent_models.deinit(gpa);
        translator.tails.deinit(gpa);
        gpa.free(translator.turn_error);
        gpa.free(translator.turn_error_text);
        gpa.free(translator.main_model);
        translator.hidden.deinit(gpa);
        gpa.free(translator.plugin_errors);
        translator.tasks.deinit(gpa);
        translator.pending_effort.deinit(gpa);
        translator.message_items.deinit(gpa);
        translator.item_message.deinit(gpa);
        translator.cursors.deinit(gpa);
        gpa.free(translator.overage_limit);
        gpa.destroy(translator);
    }

    fn io(translator: *Translator) Io {
        return translator.claude.io();
    }

    /// A held `lookup_mutex` or `task_mutex`; `unlock` releases it.
    const Lock = struct {
        mutex: *Io.Mutex,
        io: Io,

        fn unlock(lock: Lock) void {
            lock.mutex.unlock(lock.io);
        }
    };

    fn lockLookups(translator: *Translator) Lock {
        translator.lookup_mutex.lockUncancelable(translator.io());
        return .{ .mutex = &translator.lookup_mutex, .io = translator.io() };
    }

    fn lockTasks(translator: *Translator) Lock {
        translator.task_mutex.lockUncancelable(translator.io());
        return .{ .mutex = &translator.task_mutex, .io = translator.io() };
    }

    fn emit(translator: *Translator, meta: Meta, payload: types.Event.Payload) void {
        translator.claude.emit(.{ .turn_id = meta.turn_id, .raw = meta.raw, .payload = payload });
    }

    /// A copy in `a` of a string map's value, or "".
    fn getString(a: Allocator, map: *const OwnedMap([]const u8), key: []const u8) Allocator.Error![]const u8 {
        return a.dupe(u8, map.get(key) orelse return "");
    }

    fn currentTurn(translator: *Translator, a: Allocator) Allocator.Error![]const u8 {
        const id = translator.claude.rt().currentTurn();
        return a.dupe(u8, id.slice());
    }

    fn eventTurn(translator: *Translator, a: Allocator) Allocator.Error![]const u8 {
        const id = translator.claude.eventTurn();
        return a.dupe(u8, id.slice());
    }

    // ------------------------------------------------------------ lookups

    /// Carries the item, message, and turn lookups of the translator a
    /// rewind replaced, so IDs from before the rewind still resolve.
    pub fn keepLookups(translator: *Translator, old: *Translator) void {
        const old_lock = old.lockLookups();
        defer old_lock.unlock();
        const lock = translator.lockLookups();
        defer lock.unlock();
        translator.copyLookups(old) catch log.err("claude: out of memory: item lookups lost across the rewind restart", .{});
    }

    fn copyLookups(translator: *Translator, old: *Translator) Allocator.Error!void {
        for (old.message_items.keys(), old.message_items.map.values()) |k, v| {
            try translator.message_items.putCopy(translator.gpa, k, v.value);
        }
        for (old.item_message.keys(), old.item_message.map.values()) |k, v| try translator.item_message.putCopy(translator.gpa, k, v);
        for (old.cursors.keys(), old.cursors.map.values()) |k, v| try translator.cursors.putCopy(translator.gpa, k, v);
    }

    /// Drops the lookups of rewound turns and items.
    pub fn forget(translator: *Translator, turns: []const []const u8, ids: []const []const u8) void {
        const lock = translator.lockLookups();
        defer lock.unlock();
        for (turns) |turn| translator.cursors.remove(translator.gpa, turn);
        for (ids) |id| if (translator.item_message.take(translator.gpa, id)) |uuid| {
            translator.message_items.remove(translator.gpa, uuid);
            translator.gpa.free(uuid);
        };
    }

    /// Records that wire message `uuid` produced item `id`. A lost record
    /// keeps rewind and retraction from finding the item, so it is logged.
    fn remember(translator: *Translator, uuid: []const u8, id: []const u8) void {
        if (uuid.len == 0 or id.len == 0) return;
        const lock = translator.lockLookups();
        defer lock.unlock();
        translator.rememberLocked(uuid, id) catch log.err("claude: out of memory: item {s} of message {s} not recorded", .{ id, uuid });
    }

    fn rememberLocked(translator: *Translator, uuid: []const u8, id: []const u8) Allocator.Error!void {
        var arena: std.heap.ArenaAllocator = .init(translator.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        var list: std.ArrayList([]const u8) = .empty;
        if (translator.message_items.get(uuid)) |current| try list.appendSlice(a, current.value);
        if (!kit.contains(list.items, id)) {
            try list.append(a, id);
            try translator.message_items.putCopy(translator.gpa, uuid, list.items);
        }
        try translator.item_message.putCopy(translator.gpa, id, uuid);
    }

    /// The wire message UUID that produced a live item, or `id` itself,
    /// copied into `a`.
    pub fn messageOf(translator: *Translator, a: Allocator, id: []const u8) Allocator.Error![]const u8 {
        const lock = translator.lockLookups();
        defer lock.unlock();
        return a.dupe(u8, translator.item_message.get(id) orelse id);
    }

    /// The latest main assistant message UUID of a turn, copied into `a`.
    pub fn cursor(translator: *Translator, a: Allocator, turn: []const u8) Allocator.Error![]const u8 {
        const lock = translator.lockLookups();
        defer lock.unlock();
        return getString(a, &translator.cursors, turn);
    }

    pub fn setOverageLimit(translator: *Translator, limit: []const u8) Allocator.Error!void {
        const lock = translator.lockLookups();
        defer lock.unlock();
        try setString(translator.gpa, &translator.overage_limit, limit);
    }

    /// Emits items_removed for the items the given messages produced.
    fn retract(translator: *Translator, a: Allocator, uuids: []const std.json.Value, meta: Meta) Allocator.Error!void {
        var ids: std.ArrayList([]const u8) = .empty;
        {
            const lock = translator.lockLookups();
            defer lock.unlock();
            for (uuids) |value| if (value == .string) {
                const uuid = value.string;
                if (translator.message_items.get(uuid)) |list| for (list.value) |id| {
                    if (!kit.contains(ids.items, id)) try ids.append(a, try a.dupe(u8, id));
                    translator.item_message.remove(translator.gpa, id);
                };
                translator.message_items.remove(translator.gpa, uuid);
            };
        }
        for (ids.items) |id| {
            translator.tools.remove(translator.gpa, id);
            tool_output.stopTail(translator, id);
        }
        if (ids.items.len > 0) translator.emit(meta, .{ .items_removed = .{ .ids = ids.items } });
    }

    /// Ends the translator's background work.
    pub fn stop(translator: *Translator) void {
        var arena: std.heap.ArenaAllocator = .init(translator.gpa);
        defer arena.deinit();
        const ids = blk: {
            translator.tail_mutex.lockUncancelable(translator.io());
            defer translator.tail_mutex.unlock(translator.io());
            break :blk translator.tails.sortedKeys(arena.allocator()) catch
                return log.err("claude: out of memory: shell output tails not stopped", .{});
        };
        for (ids) |id| tool_output.stopTail(translator, id);
    }

    /// Reports a message the translator does not map as a native event
    /// named "type" or "type/subtype".
    fn raw(translator: *Translator, a: Allocator, message: JsonView, line: []const u8) Allocator.Error!void {
        const message_type = message.str("type");
        const event_name = if (message.str("subtype").len > 0) try std.fmt.allocPrint(a, "{s}/{s}", .{ message_type, message.str("subtype") }) else message_type;
        translator.emit(.{ .raw = RawJson.of(line) }, .{ .native = .{ .name = event_name, .data = RawJson.of(line) } });
    }

    fn native(translator: *Translator, a: Allocator, meta: Meta, comptime event_name: []const u8, data: anytype) Allocator.Error!void {
        translator.emit(meta, .{ .native = .{ .name = "claude/" ++ event_name, .data = try RawJson.encode(a, data) } });
    }

    /// Records that the CLI moved the conversation to session `id`.
    fn moveSession(translator: *Translator, id: []const u8) void {
        translator.claude.rt().setSessionId(id);
        translator.claude.setInfoField("id", id);
    }

    // ------------------------------------------------------------ dispatch

    /// The stdout message types the translator maps; others become native
    /// events.
    const MessageType = enum {
        stream_event,
        assistant,
        user,
        result,
        system,
        command_lifecycle,
        rate_limit_event,
        conversation_reset,
        active_goal,
        prompt_suggestion,
        tool_progress,
        tool_use_summary,
        auth_status,
    };

    pub fn handle(translator: *Translator, a: Allocator, message: JsonView, line: []const u8) !void {
        const meta: Meta = .{ .raw = RawJson.of(line) };
        const session_id = message.str("session_id");
        if (session_id.len > 0 and message.str("parent_tool_use_id").len == 0 and !eql(session_id, try translator.claude.sessionId(a))) {
            // The CLI moved the conversation to a new session, such as
            // after a hook or /clear; resume must name the new one.
            translator.moveSession(session_id);
        }
        const message_type = std.meta.stringToEnum(MessageType, message.str("type")) orelse return translator.raw(a, message, line);
        switch (message_type) {
            .stream_event => return translator.stream(a, message, meta),
            .assistant => return translator.assistant(a, message, meta),
            .user => return translator.user(a, message, meta),
            .result => return translator.result(a, message, line),
            .system => return translator.system(a, message, line),
            .command_lifecycle => return translator.commandLifecycle(a, message, meta),
            .rate_limit_event => return translator.rateLimit(a, message, meta),
            .conversation_reset => return translator.conversationReset(a, message, meta),
            .active_goal => {
                const value = message.get("value");
                const goal: ?types.Goal = if (value.isNull()) null else .{
                    .objective = value.str("condition"),
                    .status = "active",
                    .iterations = value.uint32("iterations"),
                };
                translator.emit(meta, .{ .goal_updated = .{ .goal = goal } });
            },
            .prompt_suggestion => return translator.native(a, meta, "prompt_suggestion", .{ .suggestion = message.str("suggestion") }),
            .tool_progress => return translator.native(a, meta, "tool_progress", .{
                .tool_use_id = message.str("tool_use_id"),
                .tool_name = message.str("tool_name"),
                .parent_tool_use_id = message.str("parent_tool_use_id"),
                .elapsed_ms = @as(i64, @intFromFloat(message.float("elapsed_time_seconds") * 1000)),
                .task_id = message.str("task_id"),
            }),
            .tool_use_summary => return translator.native(a, meta, "tool_use_summary", .{
                .summary = message.str("summary"),
                .tool_use_ids = try message.get("preceding_tool_use_ids").strings(a),
            }),
            .auth_status => {
                if (message.str("error").len > 0) translator.emit(meta, .{ .notice = .{ .level = .@"error", .code = "auth", .message = message.str("error") } });
                return translator.raw(a, message, line);
            },
        }
    }

    const CommandState = enum { started, cancelled, completed };

    fn commandLifecycle(translator: *Translator, a: Allocator, message: JsonView, meta: Meta) !void {
        const uuid = message.str("command_uuid");
        const state = message.str("state");
        if (std.meta.stringToEnum(CommandState, state)) |known| switch (known) {
            .started => try translator.turnStarted(uuid, meta),
            .cancelled => translator.claude.rt().abandonTurn(uuid, .{ .kind = .cancelled, .message = "message cancelled" }),
            // The CLI completes a message whose uuid its transcript already
            // holds without running it, and nothing will answer it. A
            // message folded into a running turn is finished by that turn's
            // result instead.
            .completed => if (!translator.started_turns.contains(uuid) and translator.started_turns.count() == 0 and translator.claude.rt().hasTurn(uuid)) {
                translator.claude.rt().abandonTurn(uuid, .{ .kind = .turn_failed, .message = "claude: the CLI acknowledged the message but never started a turn for it" });
            },
        };
        return translator.native(a, meta, "command_lifecycle", .{ .command_uuid = uuid, .state = state });
    }

    fn conversationReset(translator: *Translator, a: Allocator, message: JsonView, meta: Meta) !void {
        const new_id = message.str("new_conversation_id");
        if (new_id.len > 0 and !eql(new_id, try translator.claude.sessionId(a))) translator.moveSession(new_id);
        // Rewind cannot reach the dropped conversation's turns any more.
        translator.claude.turn_items.reset(translator.io(), translator.gpa);
        {
            const lock = translator.lockLookups();
            defer lock.unlock();
            translator.message_items.clear(translator.gpa);
            translator.item_message.clear(translator.gpa);
            translator.cursors.clear(translator.gpa);
        }
        if (translator.plan) |plan| plan.destroy(translator.gpa);
        translator.plan = null;
        translator.emit(meta, .{ .conversation_reset = .{ .trigger = message.str("trigger"), .new_session_id = new_id } });
    }

    /// Marks a submitted message's turn as started. Messages the host did
    /// not send (such as automatic follow-ups) are tracked too.
    fn turnStarted(translator: *Translator, id: []const u8, meta_in: Meta) Allocator.Error!void {
        if (id.len == 0 or translator.started_turns.contains(id)) return;
        try translator.started_turns.put(translator.gpa, id, {});
        if (!translator.claude.rt().hasTurn(id)) translator.claude.rt().trackTurn(id);
        var meta = meta_in;
        meta.turn_id = id;
        translator.emit(meta, .{ .turn_started = .{} });
        translator.emit(meta, .{ .status_changed = .{ .status = .running } });
    }

    // ------------------------------------------------------------ stream

    /// The stream_event event types the translator maps.
    const StreamEvent = enum { message_start, content_block_start, content_block_delta, message_delta };

    /// Content block types of assistant messages.
    const BlockType = enum { text, thinking, redacted_thinking, tool_use, server_tool_use, mcp_tool_use };

    const DeltaType = enum { text_delta, thinking_delta, input_json_delta };

    fn stream(translator: *Translator, a: Allocator, message: JsonView, meta: Meta) !void {
        const parent = message.str("parent_tool_use_id");
        const subagent_id = try translator.subagentOf(a, parent, "");
        const event = message.get("event");
        const event_type = std.meta.stringToEnum(StreamEvent, event.str("type")) orelse return;
        switch (event_type) {
            .message_start => {
                const message_id = event.get("message").str("id");
                // A thread's requests run one after another, so the previous
                // message of this thread has delivered all its frames.
                if (translator.stream_message.get(parent)) |previous| if (!eql(previous, message_id)) {
                    const previous_id = try a.dupe(u8, previous);
                    translator.frames.remove(translator.gpa, previous_id);
                };
                try translator.stream_message.putCopy(translator.gpa, parent, message_id);
                if (parent.len == 0) {
                    translator.request = decode.RequestUsage.of(event.get("message").get("usage"));
                    if (translator.request.input_tokens != null) {
                        translator.context_tokens = translator.request.total();
                        translator.emitUsage(meta);
                    }
                }
            },
            .content_block_start => try translator.blockStart(a, event, parent, subagent_id, meta),
            .content_block_delta => {
                const key = try std.fmt.allocPrint(a, "{s}/{d}", .{ translator.stream_message.get(parent) orelse "", event.int("index") });
                const ref = translator.blocks.get(key) orelse return;
                const item_id = ref.value.item_id;
                const delta = event.get("delta");
                const delta_type = std.meta.stringToEnum(DeltaType, delta.str("type")) orelse return;
                switch (delta_type) {
                    .text_delta => translator.emit(meta, .{ .text_delta = .{ .item_id = item_id, .parent_id = parent, .subagent_id = subagent_id, .delta = delta.str("text") } }),
                    .thinking_delta => translator.emit(meta, .{ .reasoning_delta = .{ .item_id = item_id, .parent_id = parent, .subagent_id = subagent_id, .delta = delta.str("thinking") } }),
                    .input_json_delta => if (!translator.hidden.contains(item_id)) {
                        translator.emit(meta, .{ .tool_input_delta = .{ .item_id = item_id, .parent_id = parent, .subagent_id = subagent_id, .delta = delta.str("partial_json") } });
                    },
                }
            },
            .message_delta => {
                const usage = event.get("usage");
                if (usage.object() != null and parent.len == 0) {
                    translator.turn_usage.add(decode.usageOf(usage));
                    // Delta counters are whole-request totals that replace the
                    // message_start ones; some gateways only know them here.
                    translator.request.merge(decode.RequestUsage.of(usage));
                    translator.context_tokens = translator.request.total();
                    translator.emitUsage(meta);
                }
            },
        }
    }

    /// Starts the item of a streamed content block.
    fn blockStart(translator: *Translator, a: Allocator, event: JsonView, parent: []const u8, subagent_id: []const u8, meta: Meta) !void {
        const key = try std.fmt.allocPrint(a, "{s}/{d}", .{ translator.stream_message.get(parent) orelse "", event.int("index") });
        const block = event.get("content_block");
        const block_type = std.meta.stringToEnum(BlockType, block.str("type")) orelse return;
        switch (block_type) {
            .text, .thinking, .redacted_thinking => {
                const kind: types.ItemKind = if (block_type == .text) .assistant_message else .reasoning;
                try translator.blocks.putCopy(translator.gpa, key, BlockRef{ .item_id = key, .kind = kind, .parent = parent, .subagent = subagent_id });
                translator.emit(meta, .{ .item_started = .{ .item = .{ .id = key, .kind = kind, .status = .in_progress, .parent_id = parent, .subagent_id = subagent_id } } });
                if (kind == .reasoning and block.str("thinking").len > 0) {
                    translator.emit(meta, .{ .reasoning_delta = .{ .item_id = key, .parent_id = parent, .subagent_id = subagent_id, .delta = block.str("thinking") } });
                }
            },
            .tool_use, .server_tool_use, .mcp_tool_use => {
                try translator.blocks.putCopy(translator.gpa, key, BlockRef{ .item_id = block.str("id"), .kind = .tool_call, .parent = parent, .subagent = subagent_id });
                if (eql(block.str("name"), decode.handback_tool)) {
                    try translator.hidden.put(translator.gpa, block.str("id"), {});
                    return;
                }
                _ = try translator.startTool(a, block, parent, subagent_id, meta);
            },
        }
    }

    /// Reports the turn's usage so far with the current context use.
    fn emitUsage(translator: *Translator, meta: Meta) void {
        var snapshot = translator.turn_usage;
        snapshot.context_tokens = translator.context_tokens;
        snapshot.context_window = translator.context_window;
        translator.emit(meta, .{ .usage_updated = .{ .usage = snapshot } });
    }

    /// Records a new tool call and announces it.
    fn startTool(translator: *Translator, a: Allocator, block: JsonView, parent: []const u8, subagent_id: []const u8, meta: Meta) Allocator.Error!types.Item {
        const item: types.Item = .{
            .id = block.str("id"),
            .kind = .tool_call,
            .status = .in_progress,
            .parent_id = parent,
            .subagent_id = subagent_id,
            .tool = try decode.toolCallOf(a, block, translator.claude.toolServerName()),
        };
        try translator.setTool(item.id, .{ .item = item, .turn = try translator.currentTurn(a) });
        translator.emit(meta, .{ .item_started = .{ .item = item } });
        return item;
    }

    fn setTool(translator: *Translator, id: []const u8, entry: ToolEntry) Allocator.Error!void {
        try translator.tools.putCopy(translator.gpa, id, entry);
    }

    /// A copy in `a` of a pending tool call.
    fn toolEntry(translator: *Translator, a: Allocator, id: []const u8) Allocator.Error!?ToolEntry {
        const entry = translator.tools.get(id) orelse return null;
        return try clone(ToolEntry, a, entry.value);
    }

    // ------------------------------------------------------------ assistant

    fn assistant(translator: *Translator, a: Allocator, message: JsonView, meta: Meta) !void {
        const parent = message.str("parent_tool_use_id");
        const subagent_id = try translator.subagentOf(a, parent, message.str("agent_id"));
        const api_message = message.get("message");
        const message_id = api_message.str("id");
        const uuid = message.str("uuid");
        if (message.get("supersedes").len() > 0) try translator.retract(a, message.get("supersedes").items(), meta);
        const err = message.str("error");
        if (err.len > 0) {
            const text = try decode.textOfBlocks(a, api_message.get("content"));
            if (parent.len == 0) {
                try setString(translator.gpa, &translator.turn_error, err);
                try setString(translator.gpa, &translator.turn_error_text, text);
            }
            translator.emit(meta, .{ .notice = .{ .level = .@"error", .code = err, .message = text } });
        }
        if (parent.len == 0 and uuid.len > 0) {
            const turn = try translator.currentTurn(a);
            const lock = translator.lockLookups();
            defer lock.unlock();
            try translator.cursors.putCopy(translator.gpa, turn, uuid);
        }
        const model = api_message.str("model");
        if (parent.len > 0) {
            try translator.subagentModel(a, parent, subagent_id, model, meta);
        } else if (model.len > 0 and !eql(model, "<synthetic>")) {
            try setString(translator.gpa, &translator.main_model, model);
        }
        const status: types.ItemStatus = if (message.flag("aborted")) .cancelled else .completed;
        for (api_message.get("content").items()) |value| {
            const block = JsonView.of(value);
            const n = if (translator.frames.get(message_id)) |frames| frames.value.n else 0;
            try translator.frames.putCopy(translator.gpa, message_id, Frames{ .n = n + 1, .parent = parent });
            const key = try std.fmt.allocPrint(a, "{s}/{d}", .{ message_id, n });
            var streamed_id: ?[]const u8 = null;
            if (translator.blocks.take(translator.gpa, key)) |ref| {
                streamed_id = try a.dupe(u8, ref.value.item_id);
                ref.destroy(translator.gpa);
            }
            const block_type_name = block.str("type");
            const block_type = std.meta.stringToEnum(BlockType, block_type_name) orelse {
                if (decode.isServerToolResult(block_type_name)) {
                    translator.remember(uuid, block.str("tool_use_id"));
                    try translator.completeTool(a, block.str("tool_use_id"), block.get("content"), block.flag("is_error") or decode.isErrorContent(block.get("content")), .empty, "", meta);
                }
                continue;
            };
            const thread: Thread = .{ .parent = parent, .subagent_id = subagent_id, .uuid = uuid };
            switch (block_type) {
                .text, .thinking, .redacted_thinking => {
                    const is_text = block_type == .text;
                    const kind: types.ItemKind = if (is_text) .assistant_message else .reasoning;
                    const id = streamed_id orelse key;
                    if (streamed_id == null) {
                        translator.emit(meta, .{ .item_started = .{ .item = .{ .id = id, .kind = kind, .status = .in_progress, .parent_id = parent, .subagent_id = subagent_id } } });
                    }
                    translator.remember(uuid, id);
                    try translator.completeItem(a, .{
                        .id = id,
                        .kind = kind,
                        .status = status,
                        .parent_id = parent,
                        .subagent_id = subagent_id,
                        .text = if (is_text) block.str("text") else block.str("thinking"),
                        .raw = try block.raw(a),
                    }, meta);
                },
                .tool_use, .server_tool_use, .mcp_tool_use => if (eql(block.str("name"), decode.handback_tool)) {
                    try translator.handback(a, block, thread, meta);
                } else {
                    try translator.toolUse(a, block, message.get("tool_use_meta"), thread, meta);
                },
            }
        }
        if (!api_message.get("stop_reason").isNull()) translator.frames.remove(translator.gpa, message_id);
    }

    /// Where an assistant frame's blocks come from: the parent tool call
    /// of its thread ("" is the main conversation), the subagent, and the
    /// wire message UUID.
    const Thread = struct { parent: []const u8, subagent_id: []const u8, uuid: []const u8 };

    /// Records a tool call's complete input from an assistant frame.
    fn toolUse(translator: *Translator, a: Allocator, block: JsonView, tool_use_meta: JsonView, thread: Thread, meta: Meta) !void {
        const id = block.str("id");
        var entry = (try translator.toolEntry(a, id)) orelse ToolEntry{ .item = try translator.startTool(a, block, thread.parent, thread.subagent_id, meta), .turn = try translator.currentTurn(a) };
        var item = entry.item;
        item.tool.?.input = try block.get("input").raw(a);
        item.raw = try block.raw(a);
        try decode.fillToolDetails(a, &item.tool.?, block.get("input"));
        for (tool_use_meta.items()) |value| {
            const display = JsonView.of(value);
            if (!eql(display.str("id"), id)) continue;
            item.tool.?.display_name = display.str("display_name");
            item.tool.?.server_display_name = display.str("server_display_name");
            item.tool.?.icon_url = display.str("icon_url");
        }
        entry.item = item;
        try translator.setTool(id, entry);
        translator.remember(thread.uuid, id);
        try translator.holdSubagentItem(thread.parent, thread.subagent_id, item);
        translator.emit(meta, .{ .item_updated = .{ .item = item } });
        // A subagent's to-do list is its own, not the session plan.
        if (eql(block.str("name"), "TodoWrite") and thread.parent.len == 0) {
            if (try decode.todoSteps(a, block.get("input"))) |steps| translator.emit(meta, .{ .plan_updated = .{ .steps = steps, .item_id = id } });
        }
    }

    /// Shows a subagent's SubagentHandback call as its final message. The
    /// call and its acknowledging result are not reported.
    fn handback(translator: *Translator, a: Allocator, block: JsonView, thread: Thread, meta: Meta) !void {
        try translator.hidden.put(translator.gpa, block.str("id"), {});
        const report = block.get("input").str("message");
        if (std.mem.trim(u8, report, " \t\r\n").len == 0) return;
        translator.remember(thread.uuid, block.str("id"));
        try translator.completeItem(a, .{ .id = block.str("id"), .kind = .assistant_message, .status = .completed, .parent_id = thread.parent, .subagent_id = thread.subagent_id, .text = report, .raw = try block.raw(a) }, meta);
    }

    /// Keeps a running item of a subagent frame whose subagent is not known
    /// yet, so task_started can fill in its subagent ID.
    fn holdSubagentItem(translator: *Translator, parent: []const u8, subagent_id: []const u8, item: types.Item) Allocator.Error!void {
        if (parent.len == 0 or subagent_id.len > 0) return;
        try translator.hold(parent, .{ .item = item });
    }

    fn hold(translator: *Translator, parent: []const u8, held: Held) Allocator.Error!void {
        var arena: std.heap.ArenaAllocator = .init(translator.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        var list: std.ArrayList(Held) = .empty;
        if (translator.pending_items.get(parent)) |cur| try list.appendSlice(a, cur.value);
        for (list.items) |*x| {
            if (eql(x.item.id, held.item.id)) {
                x.* = held;
                break;
            }
        } else try list.append(a, held);
        try translator.pending_items.putCopy(translator.gpa, parent, list.items);
    }

    /// Emits item_completed, or holds it while the item belongs to a
    /// subagent that task_started has not named yet.
    fn completeItem(translator: *Translator, a: Allocator, item: types.Item, meta: Meta) Allocator.Error!void {
        if (item.parent_id.len > 0 and item.subagent_id.len == 0) {
            const turn = if (meta.turn_id.len > 0) meta.turn_id else try translator.eventTurn(a);
            try translator.hold(item.parent_id, .{ .item = item, .turn_id = turn, .raw = meta.raw, .done = true });
            return;
        }
        translator.emit(meta, .{ .item_completed = .{ .item = item } });
    }

    /// Completes the held items of a parent call whose subagent was never
    /// named, such as when the call ends first.
    fn releaseHeldItems(translator: *Translator, parent: []const u8) void {
        const held = translator.pending_items.take(translator.gpa, parent) orelse return;
        defer held.destroy(translator.gpa);
        for (held.value) |h| if (h.done) translator.emit(.{ .turn_id = h.turn_id, .raw = h.raw }, .{ .item_completed = .{ .item = h.item } });
    }

    /// Drops the per-thread state of a finished parent tool call.
    fn forgetThread(translator: *Translator, a: Allocator, parent: []const u8) Allocator.Error!void {
        translator.releaseHeldItems(parent);
        translator.subagent_models.remove(translator.gpa, parent);
        translator.stream_message.remove(translator.gpa, parent);
        var drop: std.ArrayList([]const u8) = .empty;
        for (translator.frames.keys(), translator.frames.map.values()) |k, f| if (eql(f.value.parent, parent)) try drop.append(a, try a.dupe(u8, k));
        for (drop.items) |k| translator.frames.remove(translator.gpa, k);
    }

    /// Records the model a subagent's frames report.
    fn subagentModel(translator: *Translator, a: Allocator, parent: []const u8, subagent_id: []const u8, model: []const u8, meta: Meta) Allocator.Error!void {
        if (model.len == 0 or eql(model, "<synthetic>")) return;
        if (subagent_id.len == 0) {
            if (translator.subagent_models.get(parent) == null) try translator.subagent_models.putCopy(translator.gpa, parent, model);
            return;
        }
        const lock = translator.lockTasks();
        defer lock.unlock();
        const entry = translator.tasks.get(subagent_id) orelse return;
        var task = try clone(types.Task, a, entry.value);
        if (task.subagent == null or task.subagent.?.model.len > 0) return;
        task.subagent.?.model = model;
        try translator.tasks.putCopy(translator.gpa, subagent_id, task);
        translator.emit(meta, .{ .task_updated = .{ .task = task } });
    }

    /// Records the effort a subagent's hooks report. It runs on a hook
    /// task, which cannot return the failure, so a lost record is logged.
    pub fn subagentEffort(translator: *Translator, agent_id: []const u8, effort: []const u8) void {
        const lock = translator.lockTasks();
        defer lock.unlock();
        translator.recordSubagentEffort(agent_id, effort) catch log.err("claude: out of memory: effort of subagent {s} not recorded", .{agent_id});
    }

    fn recordSubagentEffort(translator: *Translator, agent_id: []const u8, effort: []const u8) Allocator.Error!void {
        var arena: std.heap.ArenaAllocator = .init(translator.gpa);
        defer arena.deinit();
        const entry = translator.tasks.get(agent_id);
        if (entry == null or entry.?.value.subagent == null) {
            if (translator.pending_effort.count() < max_pending_effort) try translator.pending_effort.putCopy(translator.gpa, agent_id, effort);
            return;
        }
        if (eql(entry.?.value.subagent.?.effort, effort)) return;
        var task = try clone(types.Task, arena.allocator(), entry.?.value);
        task.subagent.?.effort = effort;
        try translator.tasks.putCopy(translator.gpa, agent_id, task);
        translator.emit(.{}, .{ .task_updated = .{ .task = task } });
    }

    /// Reports the tasks still running as failed, because they die with
    /// the CLI process.
    pub fn failRunningTasks(translator: *Translator) void {
        translator.endRunningTasks(.failed, "the claude process exited");
    }

    /// Ends the tasks still running with `status`, and completes the
    /// spawning calls of background subagents among them. Callers cannot
    /// return a failure, so tasks left running are logged.
    pub fn endRunningTasks(translator: *Translator, status: types.TaskStatus, reason: []const u8) void {
        const lock = translator.lockTasks();
        defer lock.unlock();
        translator.endRunningTasksLocked(status, reason) catch log.err("claude: out of memory: running tasks not ended", .{});
    }

    fn endRunningTasksLocked(translator: *Translator, status: types.TaskStatus, reason: []const u8) Allocator.Error!void {
        var arena: std.heap.ArenaAllocator = .init(translator.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        for (try translator.tasks.sortedKeys(a)) |id| {
            const entry = translator.tasks.get(id) orelse continue;
            if (entry.value.status != .running) continue;
            var task = try clone(types.Task, a, entry.value);
            task.status = status;
            if (task.@"error".len == 0) task.@"error" = reason;
            try translator.tasks.putCopy(translator.gpa, id, task);
            translator.emit(.{}, .{ .task_updated = .{ .task = task } });
            try translator.completeAsyncCall(a, task, .{});
        }
    }

    /// Completes the items left open: streamed blocks no assistant frame
    /// closed get `block_status`, and tool calls without a result are
    /// cancelled. With `keep_background`, the items of background
    /// subagents, which outlive the turn, stay open, and so does the call
    /// `keep`, which the CLI deferred to the host.
    pub fn endOpenItems(translator: *Translator, a: Allocator, block_status: types.ItemStatus, keep_background: bool, keep: []const u8) Allocator.Error!void {
        for (try translator.blocks.sortedKeys(a)) |key| {
            const ref = try clone(BlockRef, a, translator.blocks.get(key).?.value);
            if (keep_background and (translator.inBackground(ref.parent) or translator.backgroundTask(ref.subagent))) continue;
            translator.blocks.remove(translator.gpa, key);
            if (ref.kind == .tool_call) continue;
            try translator.completeItem(a, .{ .id = ref.item_id, .kind = ref.kind, .status = block_status, .parent_id = ref.parent, .subagent_id = ref.subagent }, .{});
        }
        for (try translator.tools.sortedKeys(a)) |id| {
            const entry = (try translator.toolEntry(a, id)) orelse continue;
            if (eql(id, keep) or (keep_background and (translator.inBackground(id) or translator.backgroundTask(entry.item.subagent_id)))) continue;
            tool_output.stopTail(translator, id);
            translator.tools.remove(translator.gpa, id);
            try translator.forgetThread(a, id);
            var item = entry.item;
            item.status = .cancelled;
            try translator.completeItem(a, item, .{ .turn_id = entry.turn });
        }
        for (try translator.pending_items.sortedKeys(a)) |parent| {
            if (!keep_background or !translator.inBackground(parent)) translator.releaseHeldItems(parent);
        }
        var drop: std.ArrayList([]const u8) = .empty;
        for (translator.frames.keys(), translator.frames.map.values()) |key, frames| {
            if (!keep_background or !translator.inBackground(frames.value.parent)) try drop.append(a, try a.dupe(u8, key));
        }
        for (drop.items) |k| translator.frames.remove(translator.gpa, k);
    }

    /// Whether `parent` is, or runs inside, the spawning call of a
    /// background subagent: a call its launch acknowledgement left open.
    fn inBackground(translator: *Translator, parent_in: []const u8) bool {
        var parent = parent_in;
        for (0..max_subagent_depth) |_| {
            if (parent.len == 0) return false;
            const call = translator.tools.get(parent) orelse return false;
            if (call.value.item.tool) |tool| if (tool.output_data != null) return true;
            parent = call.value.item.parent_id;
        }
        return false;
    }

    /// Whether `id` is a running background subagent.
    fn backgroundTask(translator: *Translator, id: []const u8) bool {
        if (id.len == 0) return false;
        const lock = translator.lockTasks();
        defer lock.unlock();
        const task = (translator.tasks.get(id) orelse return false).value;
        return task.status == .running and task.subagent != null and task.subagent.?.background;
    }

    // ------------------------------------------------------------ user

    fn user(translator: *Translator, a: Allocator, message: JsonView, meta: Meta) !void {
        if (message.flag("isReplay") or message.flag("isCompactSummary") or message.flag("isVisibleInTranscriptOnly")) return;
        const parent = message.str("parent_tool_use_id");
        const subagent_id = try translator.subagentOf(a, parent, message.str("agent_id"));
        const content = message.get("message").get("content");
        const uuid = message.str("uuid");
        const synthetic_flag = message.flag("isSynthetic");
        const content_value = content.v orelse return;
        if (content_value == .string) {
            // Plain string content: a prompt the CLI added itself.
            const text = content_value.string;
            if (text.len > 0 and !decode.isTranscriptNoise(text) and !decode.isCommandEcho(text)) {
                try translator.completeItem(a, .{
                    .id = uuid,
                    .kind = .user_message,
                    .status = .completed,
                    .parent_id = parent,
                    .subagent_id = subagent_id,
                    .text = text,
                    .synthetic = synthetic_flag or decode.isInjectedPrompt(text),
                }, meta);
            }
            return;
        }
        if (content_value != .array) return;
        var texts: std.ArrayList([]const u8) = .empty;
        for (content.items()) |value| {
            const block = JsonView.of(value);
            const block_type = block.str("type");
            if (eql(block_type, "tool_result")) {
                const id = block.str("tool_use_id");
                var non_execution: []const u8 = "";
                for (message.get("tool_result_meta").items()) |meta_value| {
                    const result_meta = JsonView.of(meta_value);
                    if (eql(result_meta.str("id"), id)) non_execution = result_meta.str("non_execution_kind");
                }
                if (translator.hidden.contains(id)) {
                    translator.hidden.remove(translator.gpa, id);
                    continue;
                }
                translator.remember(uuid, id);
                try translator.completeTool(a, id, block.get("content"), block.flag("is_error"), message.get("tool_use_result"), non_execution, meta);
            } else if (eql(block_type, "text")) {
                try texts.append(a, block.str("text"));
            }
        }
        if (texts.items.len == 0) return;
        const text = try std.mem.join(a, "\n", texts.items);
        if (parent.len > 0 and !synthetic_flag) {
            try translator.completeItem(a, .{ .id = uuid, .kind = .user_message, .status = .completed, .parent_id = parent, .subagent_id = subagent_id, .text = text }, meta);
        } else if (parent.len == 0 and (synthetic_flag or decode.isInjectedPrompt(text)) and !decode.isTranscriptNoise(text) and !decode.isCommandEcho(text)) {
            // The host's own prompts are not echoed; these the CLI added.
            translator.emit(meta, .{ .item_completed = .{ .item = .{ .id = uuid, .kind = .user_message, .status = .completed, .text = text, .synthetic = true } } });
        }
    }

    /// Finishes a tool call with its result.
    fn completeTool(translator: *Translator, a: Allocator, id: []const u8, content: JsonView, is_error: bool, structured: JsonView, non_execution: []const u8, meta: Meta) !void {
        var entry = (try translator.toolEntry(a, id)) orelse return;
        tool_output.stopTail(translator, id);
        var item = entry.item;
        try decode.applyToolResult(a, &item, content, is_error, structured, non_execution);
        if (item.status == .in_progress) {
            // The subagent runs on; its task notification completes the call.
            entry.item = item;
            try translator.setTool(id, entry);
            translator.emit(meta, .{ .item_updated = .{ .item = item } });
            try translator.toolEffects(a, item, structured, meta);
            return;
        }
        translator.tools.remove(translator.gpa, id);
        try translator.forgetThread(a, id);
        try translator.completeItem(a, item, meta);
        if (!is_error) try translator.toolEffects(a, item, structured, meta);
    }

    /// Updates state that tool results reveal: the task list kept by
    /// TaskCreate, TaskUpdate, and TaskList, and the model a subagent runs
    /// on.
    fn toolEffects(translator: *Translator, a: Allocator, item: types.Item, structured: JsonView, meta: Meta) !void {
        const tool = item.tool orelse return;
        if (eql(tool.name, "Agent") or eql(tool.name, "Task")) {
            const agent_id = structured.str("agentId");
            if (agent_id.len == 0) return;
            // Only a background subagent's frames follow its call's result.
            if (item.status == .in_progress) try translator.agents.putCopy(translator.gpa, item.id, agent_id);
            const resolved = structured.str("resolvedModel");
            const lock = translator.lockTasks();
            defer lock.unlock();
            const entry = translator.tasks.get(agent_id) orelse return;
            if (entry.value.subagent == null or resolved.len == 0 or eql(entry.value.subagent.?.model, resolved)) return;
            var task = try clone(types.Task, a, entry.value);
            task.subagent.?.model = resolved;
            try translator.tasks.putCopy(translator.gpa, agent_id, task);
            translator.emit(meta, .{ .task_updated = .{ .task = task } });
            return;
        }
        // A subagent's task list calls do not change the session plan.
        if (item.parent_id.len > 0) return;
        const input = if (tool.input) |raw_input| JsonView.parseBytes(a, raw_input.bytes) else JsonView.empty;
        const current: []const types.PlanStep = if (translator.plan) |plan| plan.value else &.{};
        const next = (try decode.applyPlanTool(a, current, tool.name, input, structured)) orelse return;
        const owned = try Owned([]const types.PlanStep).create(translator.gpa, next);
        if (translator.plan) |plan| plan.destroy(translator.gpa);
        translator.plan = owned;
        translator.emit(meta, .{ .plan_updated = .{ .steps = next, .item_id = item.id } });
    }

    // ------------------------------------------------------------ result

    /// Error texts of an exhausted usage allowance and of a service the
    /// organization disabled, from the Agent SDK.
    const usage_limit_prefixes = [_][]const u8{
        "You've hit your",                              "You've reached your",                  "You're out of usage credits",
        "Your org is out of usage · add funds to continue",
        "Your org is out of usage · contact your admin",
        "Your seat type doesn't include usage credits", "Your seat type doesn't include usage", "Your usage allocation has been disabled by your admin",
        "Your group's usage limit is set to $0",        "Fable 5 requires usage credits",       "You're out of extra usage",
        "Your seat type doesn't include extra usage",
    };
    const org_policy_prefixes = [_][]const u8{"This service is disabled for your org"};

    fn hasAnyPrefix(texts: []const []const u8, prefixes: []const []const u8) bool {
        for (texts) |text_in| {
            const text = std.mem.trim(u8, text_in, " \t\r\n");
            for (prefixes) |prefix| if (std.mem.startsWith(u8, text, prefix)) return true;
        }
        return false;
    }

    fn result(translator: *Translator, a: Allocator, message: JsonView, line: []const u8) !void {
        const meta: Meta = .{ .raw = RawJson.of(line) };
        const subtype = message.str("subtype");
        const result_text = message.str("result");
        if (!eql(subtype, "success")) {
            var rejected = std.mem.indexOf(u8, result_text, launch_mod.drops_turn_rejected_text) != null;
            for (message.get("errors").items()) |e| if (e == .string and std.mem.indexOf(u8, e.string, launch_mod.drops_turn_rejected_text) != null) {
                rejected = true;
            };
            if (rejected) {
                // A startup refusal of the resume a rewind asked for; the
                // rewind recovers, and no turn ran.
                translator.drops_turn_rejected.store(true, .release);
                return;
            }
        }
        const total_cost = message.float("total_cost_usd");
        const cost = blk: {
            const lock = translator.lockLookups();
            defer lock.unlock();
            var cost_delta = total_cost - translator.last_cost;
            if (cost_delta < 0) cost_delta = total_cost;
            translator.last_cost = total_cost;
            break :blk cost_delta;
        };
        var usage = decode.usageOf(message.get("usage"));
        usage.cost_usd = cost;
        // modelUsage accumulates every model the session used, subagents
        // included; the active model's entry describes the context.
        const model_usage = message.get("modelUsage");
        const info = try translator.claude.rt().sessionInfo(a);
        const active = firstOf(&.{ translator.main_model, info.model });
        const active_window = model_usage.get(active).int("contextWindow");
        if (active_window > 0) {
            translator.context_window = active_window;
        } else if (model_usage.object()) |models| {
            var widest: i64 = 0;
            for (models.values()) |v| widest = @max(widest, JsonView.of(v).int("contextWindow"));
            if (widest > 0) translator.context_window = widest;
        }
        usage.context_window = translator.context_window;
        usage.context_tokens = translator.context_tokens;
        translator.turn_usage = .{};

        var turn_result: types.TurnResult = .{
            .status = .completed,
            .text = result_text,
            .usage = usage,
            .duration_ms = message.int("duration_ms"),
            .steps = message.uint32("num_turns"),
            .stop_reason = firstOf(&.{ message.str("stop_reason"), message.str("terminal_reason") }),
            .origin = message.get("origin").str("kind"),
            .raw = RawJson.of(line),
            .structured_output = try message.get("structured_output").raw(a),
        };
        const deferred = message.get("deferred_tool_use");
        if (!deferred.isNull()) {
            try translator.native(a, meta, "deferred_tool_use", .{
                .tool_use_id = deferred.str("id"),
                .tool_name = deferred.str("name"),
                .input = try deferred.get("input").raw(a),
            });
        }
        var ids: std.ArrayList([]const u8) = .empty;
        for (message.get("user_message_uuids").items()) |v| if (v == .string) try ids.append(a, v.string);
        if (ids.items.len == 0 and message.str("user_message_uuid").len > 0) try ids.append(a, message.str("user_message_uuid"));
        var finish: std.ArrayList([]const u8) = .empty;
        for (ids.items) |id| if (translator.claude.rt().hasTurn(id)) try finish.append(a, id);
        if (finish.items.len == 0) {
            if (ids.items.len > 0) {
                // A turn the host did not start, such as an automatic
                // follow-up.
                try finish.append(a, ids.items[ids.items.len - 1]);
            } else {
                try finish.append(a, try translator.currentTurn(a));
            }
        }

        const http_status: i64 = message.int("api_error_status");
        const terminal = message.str("terminal_reason");
        const is_error = message.flag("is_error");
        // The terminal reason is authoritative; an interrupt asked for one
        // of the finished turns covers CLIs that predate it, and only for a
        // turn that did not succeed.
        const interrupting, _ = translator.claude.rt().interrupting();
        const requested = !interrupting.isEmpty() and for (finish.items) |id| {
            if (interrupting.eql(id)) break true;
        } else false;
        const interrupted = std.mem.startsWith(u8, terminal, "aborted") or
            (requested and terminal.len == 0 and (is_error or !eql(subtype, "success")));
        if (interrupted) {
            turn_result.status = .interrupted;
            turn_result.@"error" = .{ .kind = .interrupted, .code = firstOf(&.{ terminal, "interrupted" }), .message = "turn interrupted" };
        } else if (is_error or !eql(subtype, "success") or http_status == 429 or http_status == 529) {
            turn_result.status = .failed;
            var errs: std.ArrayList([]const u8) = .empty;
            for (message.get("errors").items()) |e| if (e == .string and !std.mem.startsWith(u8, e.string, "[ede_diagnostic]")) try errs.append(a, e.string);
            var error_message = result_text;
            if (!eql(subtype, "success") or error_message.len == 0) error_message = try std.mem.join(a, "; ", errs.items);
            var code = subtype;
            if (eql(subtype, "success")) code = firstOf(&.{ message.str("startup_failure_reason"), terminal, translator.turn_error, "error" });
            var usage_limit = http_status == 429 or eql(terminal, "blocking_limit") or eql(translator.turn_error, "rate_limit");
            const lower = try std.ascii.allocLowerString(a, try std.fmt.allocPrint(a, "{s} {s}", .{ error_message, translator.turn_error }));
            var retryable = usage_limit or http_status == 529 or http_status == 503 or
                std.mem.indexOf(u8, lower, "overloaded") != null or std.mem.indexOf(u8, lower, "rate limit") != null or
                std.mem.indexOf(u8, lower, "rate_limit") != null;
            const texts = [_][]const u8{ error_message, translator.turn_error_text };
            if (hasAnyPrefix(&texts, &org_policy_prefixes)) {
                // The organization turned the service off; waiting does not
                // help.
                code = "org_policy_limit";
                retryable = false;
                usage_limit = false;
            } else if (hasAnyPrefix(&texts, &usage_limit_prefixes)) {
                code = "usage_limit";
                retryable = true;
                usage_limit = true;
            }
            var err: types.ErrorInfo = .{
                .kind = .turn_failed,
                .code = try a.dupe(u8, code),
                .message = error_message,
                .http_status = message.uint32("api_error_status"),
                .retryable = retryable,
                .resets_at_ms = if (usage_limit) translator.limit_resets_at_ms else 0,
            };
            if (std.mem.indexOf(u8, error_message, launch_mod.no_conversation_text) != null) {
                translator.session_missing.store(true, .release);
                err.kind = .session_not_found;
            }
            turn_result.@"error" = err;
        }
        try setString(translator.gpa, &translator.turn_error, "");
        try setString(translator.gpa, &translator.turn_error_text, "");

        try translator.endOpenItems(a, if (turn_result.status == .completed) .completed else .cancelled, true, deferred.str("id"));

        for (finish.items) |id| {
            var finished = turn_result;
            finished.turn_id = id;
            translator.started_turns.remove(translator.gpa, id);
            translator.claude.rt().finishTurn(id, finished);
        }
        if (!translator.claude.rt().busy()) translator.emit(meta, .{ .status_changed = .{ .status = .idle } });
    }

    // ------------------------------------------------------------ system

    /// The system message subtypes the translator maps; others become
    /// native events.
    const SystemSubtype = enum {
        init,
        status,
        compact_boundary,
        api_retry,
        model_refusal_fallback,
        model_refusal_no_fallback,
        informational,
        elicitation_complete,
        plugin_install,
        session_state_changed,
        notification,
        permission_denied,
        task_started,
        task_progress,
        task_updated,
        task_notification,
        commands_changed,
        hook_started,
        hook_progress,
        hook_response,
        local_command_output,
        background_tasks_changed,
    };

    fn system(translator: *Translator, a: Allocator, message: JsonView, line: []const u8) !void {
        const meta: Meta = .{ .raw = RawJson.of(line) };
        const subtype = std.meta.stringToEnum(SystemSubtype, message.str("subtype")) orelse return translator.raw(a, message, line);
        switch (subtype) {
            .init => try translator.systemInit(a, message, meta),
            .status => try translator.systemStatus(a, message, meta),
            .compact_boundary => {
                const compact = message.get("compact_metadata");
                translator.compacting = false;
                if (compact.int("post_tokens") > 0) translator.context_tokens = compact.int("post_tokens");
                translator.emit(meta, .{ .compaction = .{ .done = true, .trigger = compact.str("trigger"), .tokens_before = compact.int("pre_tokens"), .tokens_after = compact.int("post_tokens") } });
                translator.emit(meta, .{ .item_completed = .{ .item = .{ .id = message.str("uuid"), .kind = .compaction, .status = .completed, .text = compact.str("trigger"), .raw = RawJson.of(line) } } });
            },
            .api_retry => {
                var notice_text: []const u8 = message.str("error");
                var http: u32 = 0;
                if (!message.get("error_status").isNull()) {
                    http = message.uint32("error_status");
                    notice_text = try std.fmt.allocPrint(a, "{s} (HTTP {d})", .{ message.str("error"), http });
                }
                translator.emit(meta, .{ .retry = .{
                    .attempt = message.uint32("attempt"),
                    .max_attempts = message.uint32("max_retries"),
                    .delay_ms = message.int("retry_delay_ms"),
                    .code = message.str("error"),
                    .http_status = http,
                    .message = notice_text,
                } });
            },
            .model_refusal_fallback => {
                try translator.retract(a, message.get("retracted_message_uuids").items(), meta);
                translator.emit(meta, .{ .notice = .{ .level = .warning, .code = "model_refusal_fallback", .message = message.str("content") } });
                const scope = message.str("scope");
                const fallback = message.str("fallback_model");
                if ((scope.len == 0 or eql(scope, "session")) and fallback.len > 0) translator.claude.setInfoField("model", fallback);
            },
            .model_refusal_no_fallback => translator.emit(meta, .{ .notice = .{ .level = .@"error", .code = "model_refusal", .message = message.str("content") } }),
            .informational => {
                const level = message.str("level");
                const notice_level: types.NoticeLevel = if (eql(level, "warning") or eql(level, "suggestion")) .warning else .info;
                translator.emit(meta, .{ .notice = .{ .level = notice_level, .code = message.str("tag"), .message = message.str("content") } });
            },
            .elicitation_complete => try translator.elicitationComplete(a, message.str("mcp_server_name"), message.str("elicitation_id"), meta),
            .plugin_install => {
                if (eql(message.str("status"), "failed")) {
                    const notice_text = try std.fmt.allocPrint(a, "{s}: {s}", .{ message.str("name"), message.str("error") });
                    translator.emit(meta, .{ .notice = .{ .level = .@"error", .code = "plugin_install_failed", .message = if (message.str("name").len == 0) message.str("error") else notice_text } });
                }
                try translator.raw(a, message, line);
            },
            .session_state_changed => {
                const state = message.str("state");
                const session_status: ?types.SessionStatus = if (eql(state, "idle")) .idle else if (eql(state, "running")) .running else if (eql(state, "requires_action")) .waiting else null;
                if (session_status) |new_status| translator.emit(meta, .{ .status_changed = .{ .status = new_status } });
            },
            .notification => {
                const priority = message.str("priority");
                const notice_level: types.NoticeLevel = if (eql(priority, "high") or eql(priority, "immediate")) .warning else .info;
                translator.emit(meta, .{ .notice = .{ .level = notice_level, .code = message.str("key"), .message = message.str("text") } });
            },
            .permission_denied => {
                const id = message.str("tool_use_id");
                if (try translator.toolEntry(a, id)) |entry| {
                    var declined = entry;
                    declined.item.status = .declined;
                    try translator.setTool(id, declined);
                }
                const fallback = try std.fmt.allocPrint(a, "{s} was denied", .{message.str("tool_name")});
                translator.emit(meta, .{ .notice = .{ .level = .warning, .code = "permission_denied", .message = firstOf(&.{ message.str("message"), message.str("decision_reason"), fallback }) } });
            },
            .task_started => try translator.handleTask(a, .started, message, meta),
            .task_progress => try translator.handleTask(a, .progress, message, meta),
            .task_updated => try translator.handleTask(a, .updated, message, meta),
            .task_notification => try translator.handleTask(a, .notification, message, meta),
            .commands_changed => translator.claude.setInfoField("commands", try control.slashCommands(a, message.get("commands"))),
            .hook_started, .hook_progress, .hook_response => translator.emit(meta, .{ .hook_run = .{
                .id = message.str("hook_id"),
                .event = message.str("hook_event"),
                .name = message.str("hook_name"),
                .done = subtype == .hook_response,
                .status = message.str("outcome"),
                .outcome = decode.hookOutcome(message.str("outcome"), message.get("exit_code")),
                .output = firstOf(&.{ message.str("output"), message.str("stdout"), message.str("stderr") }),
            } }),
            .local_command_output => translator.emit(meta, .{ .item_completed = .{ .item = .{ .id = message.str("uuid"), .kind = .assistant_message, .status = .completed, .text = message.str("content") } } }),
            .background_tasks_changed => translator.emit(meta, .{ .native = .{ .name = "claude/background_tasks_changed", .data = (try message.get("tasks").raw(a)) orelse RawJson.of("[]") } }),
        }
    }

    fn systemInit(translator: *Translator, a: Allocator, message: JsonView, meta: Meta) !void {
        for (message.get("user_message_uuids").items()) |id| if (id == .string) try translator.turnStarted(id.string, meta);
        try translator.applyInit(a, message);
        // The CLI repeats plugin errors in every init message.
        const key = if (try message.get("plugin_errors").raw(a)) |errors_raw| errors_raw.bytes else "null";
        if (eql(key, translator.plugin_errors)) return;
        try setString(translator.gpa, &translator.plugin_errors, key);
        for (message.get("plugin_errors").items()) |value| {
            const plugin_error = JsonView.of(value);
            const notice_text = try std.fmt.allocPrint(a, "{s}: {s}", .{ plugin_error.str("plugin"), firstOf(&.{ plugin_error.str("message"), plugin_error.str("type") }) });
            translator.emit(meta, .{ .notice = .{ .level = .warning, .code = "plugin_error", .message = notice_text } });
        }
    }

    fn systemStatus(translator: *Translator, a: Allocator, message: JsonView, meta: Meta) !void {
        const mode = message.str("permissionMode");
        if (mode.len > 0) {
            const uniform = permissions.uniformMode(mode);
            const info = try translator.claude.rt().sessionInfo(a);
            if (!eql(uniform, info.permission_mode)) translator.claude.setInfoField("permission_mode", uniform);
        }
        if (eql(message.get("status").string(), "compacting")) {
            // The CLI repeats the status while compaction runs.
            if (!translator.compacting) {
                translator.compacting = true;
                translator.emit(meta, .{ .compaction = .{} });
            }
            translator.emit(meta, .{ .status_changed = .{ .status = .compacting } });
        } else if (eql(message.str("compact_result"), "failed")) {
            translator.compacting = false;
            translator.emit(meta, .{ .compaction = .{ .done = true, .@"error" = firstOf(&.{ message.str("compact_error"), "compaction failed" }) } });
        } else {
            translator.compacting = false;
        }
    }

    /// Settles the url-mode elicitation an MCP server reported complete: a
    /// pending request is accepted on the user's behalf, since the user
    /// finished it in the browser. Without one it is reported as a notice.
    fn elicitationComplete(translator: *Translator, a: Allocator, server: []const u8, id: []const u8, meta: Meta) !void {
        const pending = try translator.claude.session.pendingRequests(a);
        for (pending) |event| switch (event.payload) {
            .elicitation_request => |request| if (id.len > 0 and eql(request.elicitation_id, id) and (server.len == 0 or eql(request.server, server))) {
                // Answering writes to the CLI and never waits for its reply,
                // so it is safe on the reader task. A failed answer leaves
                // the completion to be reported as a notice.
                translator.claude.session.respond(request.id, .{ .elicitation = .{ .action = .accept } }, null) catch continue;
                return;
            },
            else => {},
        };
        const notice_text = if (server.len > 0) try std.fmt.allocPrint(a, "{s}: elicitation {s} completed", .{ server, id }) else try std.fmt.allocPrint(a, "elicitation {s} completed", .{id});
        translator.emit(meta, .{ .notice = .{ .level = .info, .code = "elicitation_complete", .message = notice_text } });
    }

    /// The session info an init message reports. Its encoding detects a
    /// repeated init.
    const InitInfo = struct {
        model: []const u8,
        mode: []const u8,
        cwd: []const u8,
        tools: []const []const u8,
        servers: []const types.McpServerStatus,
        version: []const u8,
        effort: ?[]const u8,

        fn apply(message_init: InitInfo, info: *types.SessionInfo, a: Allocator) Allocator.Error!void {
            const init = try clone(InitInfo, a, message_init);
            if (init.model.len > 0) info.model = init.model;
            if (init.mode.len > 0) info.permission_mode = permissions.uniformMode(init.mode);
            if (init.cwd.len > 0) info.cwd = init.cwd;
            if (init.version.len > 0) info.version = init.version;
            if (init.effort) |effort| info.effort = effort;
            info.tools = init.tools;
            info.mcp_servers = init.servers;
        }
    };

    fn applyInit(translator: *Translator, a: Allocator, message: JsonView) !void {
        const list = message.get("mcp_servers").items();
        const servers = try a.alloc(types.McpServerStatus, list.len);
        for (list, servers) |value, *server| server.* = .{ .name = JsonView.of(value).str("name"), .status = JsonView.of(value).str("status") };
        const init: InitInfo = .{
            .model = message.str("model"),
            .mode = message.str("permissionMode"),
            .cwd = message.str("cwd"),
            .tools = try message.get("tools").strings(a),
            .servers = servers,
            .version = message.str("claude_code_version"),
            .effort = if (message.get("effort").isNull()) null else message.str("effort"),
        };
        const key = try RawJson.encode(a, init);
        if (eql(key.bytes, translator.last_init)) return;
        try setString(translator.gpa, &translator.last_init, key.bytes);
        translator.claude.rt().updateInfo(init, true, InitInfo.apply) catch
            log.err("claude: out of memory: session info update lost", .{});
    }

    /// The subagent producing a frame: the frame's own agent_id, or the
    /// agent started by its parent tool call.
    fn subagentOf(translator: *Translator, a: Allocator, parent: []const u8, agent_id: []const u8) Allocator.Error![]const u8 {
        if (agent_id.len > 0) {
            if (parent.len > 0) try translator.agents.putCopy(translator.gpa, parent, agent_id);
            return agent_id;
        }
        if (parent.len == 0) return "";
        return getString(a, &translator.agents, parent);
    }

    // ------------------------------------------------------------ tasks

    const NativeTaskType = enum { local_agent, remote_agent, in_process_teammate, local_bash, local_workflow };

    fn taskKind(task_type: []const u8) ?types.TaskKind {
        if (task_type.len == 0) return null;
        return switch (std.meta.stringToEnum(NativeTaskType, task_type) orelse return .other) {
            .local_agent, .remote_agent, .in_process_teammate => .subagent,
            .local_bash => .shell,
            .local_workflow => .workflow,
        };
    }

    /// The task_* system messages.
    const TaskMessage = enum { started, progress, updated, notification };

    const NativeTaskStatus = enum { completed, failed, stopped, killed, running, pending, paused };

    fn taskStatus(native_status: []const u8) ?types.TaskStatus {
        return switch (std.meta.stringToEnum(NativeTaskStatus, native_status) orelse return null) {
            .completed => .completed,
            .failed => .failed,
            .stopped, .killed => .stopped,
            .running, .pending, .paused => .running,
        };
    }

    fn handleTask(translator: *Translator, a: Allocator, kind: TaskMessage, message: JsonView, meta: Meta) !void {
        const task_id = message.str("task_id");
        if (task_id.len == 0) return;
        const lock = translator.lockTasks();
        defer lock.unlock();
        const existing = translator.tasks.get(task_id);
        // A task seen before keeps its kind; `known_kind` marks one set.
        var task: types.Task = if (existing) |entry| try clone(types.Task, a, entry.value) else .{ .id = task_id, .status = .running };
        var known_kind = existing != null;
        if (taskKind(message.str("task_type"))) |task_kind| {
            task.native_kind = message.str("task_type");
            task.kind = task_kind;
            known_kind = true;
        }
        if (message.str("subagent_type").len > 0 and !known_kind) {
            task.kind = .subagent;
            known_kind = true;
        }
        if (!known_kind) task.kind = .other;
        if (message.flag("skip_transcript") or message.flag("ambient")) task.hidden = true;
        const patch = message.get("patch");
        if (kind == .progress) {
            // A progress description is the latest step, not the title.
            if (message.str("summary").len == 0 and message.str("description").len > 0) task.summary = message.str("description");
            task.description = firstOf(&.{ task.description, message.str("description") });
        } else {
            task.description = firstOf(&.{ message.str("description"), patch.str("description"), task.description });
        }
        var new_agent = false;
        const tool_use_id = message.str("tool_use_id");
        if (tool_use_id.len > 0) {
            task.tool_call_id = tool_use_id;
            if (task.kind == .subagent and !eql(translator.agents.get(tool_use_id) orelse "", task_id)) {
                try translator.agents.putCopy(translator.gpa, tool_use_id, task_id);
                new_agent = true;
            }
        }
        task.parent_id = firstOf(&.{ message.str("parent_task_id"), task.parent_id });
        if (task.kind == .subagent) {
            var subagent = task.subagent orelse types.Subagent{};
            subagent.type = firstOf(&.{ message.str("subagent_type"), subagent.type });
            subagent.prompt = firstOf(&.{ message.str("prompt"), subagent.prompt });
            if (message.int("spawn_depth") > 0) subagent.depth = message.uint32("spawn_depth");
            if (!message.get("is_backgrounded").isNull()) subagent.background = message.flag("is_backgrounded");
            if (!patch.get("is_backgrounded").isNull()) subagent.background = patch.flag("is_backgrounded");
            if (translator.pending_effort.get(task_id)) |effort| {
                subagent.effort = try a.dupe(u8, effort);
                translator.pending_effort.remove(translator.gpa, task_id);
            }
            task.subagent = subagent;
            if (new_agent or (kind == .started and task.tool_call_id.len > 0)) try translator.bindSubagent(a, &task, meta);
        }
        const progress = message.get("workflow_progress");
        if (progress.len() > 0) task.workflow = try mergeWorkflow(a, task.workflow, progress);
        if (message.str("workflow_name").len > 0 and task.description.len == 0) task.description = message.str("workflow_name");
        const usage = message.get("usage");
        if (usage.object() != null) {
            task.usage = .{ .total_tokens = usage.int("total_tokens") };
            task.tool_uses = usage.uint32("tool_uses");
            task.duration_ms = usage.int("duration_ms");
        }
        if (message.str("last_tool_name").len > 0) task.last_tool = message.str("last_tool_name");
        var native_status = message.str("status");
        switch (kind) {
            .updated => {
                native_status = patch.str("status");
                if (patch.str("error").len > 0) task.@"error" = patch.str("error");
            },
            .progress => if (message.str("summary").len > 0) {
                task.summary = message.str("summary");
            },
            .started => if (task.kind == .shell and !message.flag("is_backgrounded") and tool_use_id.len > 0) {
                tool_output.startTail(translator, tool_use_id, task_id);
            },
            .notification => try finalReport(a, &task, message, translator.claude.env()),
        }
        if (taskStatus(native_status)) |new_status| task.status = new_status;
        translator.emit(meta, .{ .task_updated = .{ .task = task } });
        if (task.status != .running) try translator.completeAsyncCall(a, task, meta);
        if (task.status != .running and kind == .notification) {
            translator.tasks.remove(translator.gpa, task_id);
            if (eql(translator.agents.get(task.tool_call_id) orelse "", task.id)) translator.agents.remove(translator.gpa, task.tool_call_id);
        } else {
            try translator.tasks.putCopy(translator.gpa, task_id, task);
        }
    }

    /// Sets a finished task's output from its notification: the summary is
    /// the final report, unless a hand-back report or a workflow result
    /// replaces it.
    fn finalReport(a: Allocator, task: *types.Task, message: JsonView, env: agent.Env) Allocator.Error!void {
        const handback_report = message.get("handback_report");
        if (handback_report.str("text").len > 0) {
            task.output = handback_report.str("text");
            if (handback_report.str("warning").len > 0) task.output = try std.fmt.allocPrint(a, "{s}\n\n{s}", .{ handback_report.str("warning"), handback_report.str("text") });
        } else if (message.str("summary").len > 0) {
            task.output = message.str("summary");
        }
        if (task.kind == .workflow and message.str("output_file").len > 0) {
            const output = subagents.readWorkflowOutput(env, a, message.str("output_file"));
            if (output.len > 0) task.output = output;
        }
    }

    /// Fills in what was learned about a subagent before its task_started:
    /// the model and effort from the spawning call's input or its first
    /// frame, and the subagent ID of items it already produced.
    fn bindSubagent(translator: *Translator, a: Allocator, task: *types.Task, meta: Meta) !void {
        var subagent = &task.subagent.?;
        if (try translator.toolEntry(a, task.tool_call_id)) |call| if (call.item.tool) |tool| if (tool.input) |raw_input| {
            const input = JsonView.parseBytes(a, raw_input.bytes);
            if (input.object() != null) {
                subagent.name = firstOf(&.{ subagent.name, input.str("name") });
                if (subagent.model.len == 0 and !eql(input.str("model"), "inherit")) subagent.model = input.str("model");
                if (subagent.effort.len == 0) subagent.effort = input.str("effort");
            }
        };
        if (translator.subagent_models.get(task.tool_call_id)) |model| if (model.len > 0) {
            subagent.model = try a.dupe(u8, model);
        };
        translator.subagent_models.remove(translator.gpa, task.tool_call_id);
        for (try translator.tools.sortedKeys(a)) |id| {
            var entry = (try translator.toolEntry(a, id)) orelse continue;
            if (eql(entry.item.parent_id, task.tool_call_id) and entry.item.subagent_id.len == 0) {
                entry.item.subagent_id = task.id;
                try translator.setTool(id, entry);
            }
        }
        // Held completions are announced now, with the subagent; running
        // calls were announced already and are updated.
        const held = translator.pending_items.take(translator.gpa, task.tool_call_id) orelse return;
        defer held.destroy(translator.gpa);
        for (held.value) |held_item| {
            if (held_item.done) {
                var item = held_item.item;
                item.subagent_id = task.id;
                translator.emit(.{ .turn_id = held_item.turn_id, .raw = held_item.raw }, .{ .item_completed = .{ .item = item } });
            } else if (try translator.toolEntry(a, held_item.item.id)) |call| {
                translator.emit(meta, .{ .item_updated = .{ .item = call.item } });
            }
        }
    }

    /// Completes a background subagent's spawning call, which its launch
    /// acknowledgement left running.
    fn completeAsyncCall(translator: *Translator, a: Allocator, task: types.Task, meta_in: Meta) Allocator.Error!void {
        const entry = (try translator.toolEntry(a, task.tool_call_id)) orelse return;
        var call = entry.item;
        if (task.kind != .subagent or call.tool == null or call.tool.?.output_data == null) return;
        translator.tools.remove(translator.gpa, task.tool_call_id);
        translator.agents.remove(translator.gpa, task.tool_call_id);
        try translator.forgetThread(a, task.tool_call_id);
        // The call belongs to the turn that made it, not the one running now.
        var meta = meta_in;
        meta.turn_id = entry.turn;
        call.status = .completed;
        switch (task.status) {
            .failed => {
                call.status = .failed;
                call.tool.?.is_error = true;
            },
            .stopped => call.status = .cancelled,
            else => {},
        }
        call.tool.?.output = firstOf(&.{ task.output, task.@"error", call.tool.?.output });
        translator.emit(meta, .{ .item_completed = .{ .item = call } });
    }

    // ------------------------------------------------------------ rate limits

    const RateLimitStatus = @FieldType(types.RateLimit, "status");

    /// What the headline window of a rate_limit_event reports.
    const Headline = struct {
        name: []const u8,
        status: RateLimitStatus,
        using_overage: bool,
        /// The model-scoped allowance get_usage reported.
        overage_limit: []const u8,
    };

    /// Maps a rate_limit_event to the uniform windows. The headline window
    /// carries the event's status and overage state; the others are graded
    /// by use. A rejection is not an error while overage is allowed.
    fn rateLimit(translator: *Translator, a: Allocator, message: JsonView, meta: Meta) !void {
        const info = message.get("rate_limit_info");
        if (info.object() == null) return;
        const native_status = info.str("status");
        const status: RateLimitStatus = if (eql(native_status, "allowed_warning")) .warning else if (eql(native_status, "rejected")) .exceeded else .ok;
        const overage_allowed = eql(info.str("overageStatus"), "allowed") or eql(info.str("overageStatus"), "allowed_warning");
        const headline: Headline = .{
            .name = info.str("rateLimitType"),
            .status = status,
            .using_overage = info.flag("isUsingOverage") or info.flag("overageInUse"),
            .overage_limit = blk: {
                const lock = translator.lockLookups();
                defer lock.unlock();
                break :blk try a.dupe(u8, translator.overage_limit);
            },
        };
        translator.limit_resets_at_ms = 0;
        var limits: std.ArrayList(types.RateLimit) = .empty;
        const windows = info.get("unifiedWindows");
        if (windows.object()) |window_map| {
            for (try kit.sortedKeys(a, window_map.keys())) |k| {
                const window = JsonView.of(window_map.get(k));
                try translator.addRateLimit(a, &limits, k, window.float("utilization"), window.int("resetsAt"), headline);
            }
        }
        if (headline.name.len > 0 and windows.get(headline.name).isNull()) {
            try translator.addRateLimit(a, &limits, headline.name, info.float("utilization"), info.int("resetsAt"), headline);
        }
        if (status == .exceeded and translator.limit_resets_at_ms == 0 and info.int("resetsAt") > 0) translator.limit_resets_at_ms = info.int("resetsAt") * 1000;
        translator.emit(meta, .{ .rate_limit_updated = .{ .limits = limits.items } });
        if (status == .exceeded and overage_allowed) {
            translator.emit(meta, .{ .notice = .{ .level = .warning, .code = "rate_limit_overage", .message = try std.fmt.allocPrint(a, "rate limit reached ({s}); using overage", .{headline.name}) } });
        } else if (status == .exceeded) {
            translator.emit(meta, .{ .notice = .{ .level = .@"error", .code = "rate_limit", .message = try std.fmt.allocPrint(a, "rate limit reached ({s})", .{headline.name}) } });
        }
    }

    /// Adds window `window_name`, graded by use unless it is the headline
    /// window, and records when an exceeded headline window resets.
    fn addRateLimit(translator: *Translator, a: Allocator, out: *std.ArrayList(types.RateLimit), window_name: []const u8, utilization: f64, resets_at_s: i64, headline: Headline) Allocator.Error!void {
        var limit: types.RateLimit = .{ .name = window_name, .used_percent = utilization * 100, .window_ms = control.windowMs(window_name), .status = .ok };
        if (resets_at_s > 0) limit.resets_at_ms = resets_at_s * 1000;
        if (eql(window_name, "seven_day_overage_included")) limit.limit_id = headline.overage_limit;
        if (eql(window_name, headline.name)) {
            limit.status = headline.status;
            limit.overage = headline.using_overage;
            if (headline.status == .exceeded) translator.limit_resets_at_ms = limit.resets_at_ms;
        } else if (utilization >= 1) limit.status = .exceeded;
        try out.append(a, limit);
    }
};

/// Replaces an owned string with a copy of `value`. An empty value needs no
/// allocation, so clearing cannot fail.
fn setString(gpa: Allocator, field: *[]u8, value: []const u8) Allocator.Error!void {
    const copy: []u8 = if (value.len == 0) &.{} else try gpa.dupe(u8, value);
    gpa.free(field.*);
    field.* = copy;
}

/// Applies a workflow_progress snapshot to the known phases and agents,
/// keyed by index. An agent's older attempt does not replace a newer one.
fn mergeWorkflow(a: Allocator, previous: ?types.Workflow, entries: JsonView) Allocator.Error!types.Workflow {
    var phases: std.ArrayList(types.WorkflowPhase) = .empty;
    var agents: std.ArrayList(types.WorkflowAgent) = .empty;
    if (previous) |known| {
        try phases.appendSlice(a, known.phases);
        try agents.appendSlice(a, known.agents);
    }
    for (entries.items()) |value| {
        const progress_entry = JsonView.of(value);
        if (progress_entry.get("index").isNull()) continue;
        const index: u32 = progress_entry.uint32("index");
        if (eql(progress_entry.str("type"), "workflow_phase")) {
            const phase: types.WorkflowPhase = .{ .index = index, .title = progress_entry.str("title") };
            for (phases.items) |*known| {
                if (known.index == index) {
                    known.* = phase;
                    break;
                }
            } else try phases.append(a, phase);
        } else if (eql(progress_entry.str("type"), "workflow_agent")) {
            const workflow_agent: types.WorkflowAgent = .{
                .index = index,
                .phase = progress_entry.uint32("phaseIndex"),
                .label = progress_entry.str("label"),
                .state = progress_entry.str("state"),
                .agent_id = progress_entry.str("agentId"),
                .model = progress_entry.str("model"),
                .attempt = progress_entry.uint32("attempt"),
                .tokens = progress_entry.int("tokens"),
                .tool_calls = progress_entry.uint32("toolCalls"),
                .duration_ms = progress_entry.int("durationMs"),
                .prompt = progress_entry.str("promptPreview"),
                .result = progress_entry.str("resultPreview"),
            };
            for (agents.items) |*known| {
                if (known.index == index) {
                    if (workflow_agent.attempt >= known.attempt) known.* = workflow_agent;
                    break;
                }
            } else try agents.append(a, workflow_agent);
        }
    }
    std.mem.sort(types.WorkflowPhase, phases.items, {}, struct {
        fn less(_: void, x: types.WorkflowPhase, y: types.WorkflowPhase) bool {
            return x.index < y.index;
        }
    }.less);
    std.mem.sort(types.WorkflowAgent, agents.items, {}, struct {
        fn less(_: void, x: types.WorkflowAgent, y: types.WorkflowAgent) bool {
            return x.index < y.index;
        }
    }.less);
    return .{ .phases = phases.items, .agents = agents.items };
}

test "a held item replaces the one with its ID or is added" {
    const gpa = std.testing.allocator;
    var translator: Translator = .{ .claude = undefined, .gpa = gpa };
    defer translator.pending_items.deinit(gpa);
    try translator.hold("call", .{ .item = .{ .id = "a" } });
    try translator.hold("call", .{ .item = .{ .id = "b" } });
    try translator.hold("call", .{ .item = .{ .id = "a" }, .done = true });
    const held = translator.pending_items.get("call").?.value;
    try std.testing.expectEqual(@as(usize, 2), held.len);
    try std.testing.expectEqualStrings("a", held[0].item.id);
    try std.testing.expect(held[0].done);
    try std.testing.expectEqualStrings("b", held[1].item.id);
    try std.testing.expect(!held[1].done);
}

test "workflow progress adds phases and agents and updates known ones" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const first = JsonView.parseBytes(a,
        \\[{"type":"workflow_phase","index":0,"title":"one"},{"type":"workflow_phase","index":1,"title":"two"},
        \\{"type":"workflow_agent","index":0,"attempt":1,"label":"x"}]
    );
    const merged = try mergeWorkflow(a, null, first);
    try std.testing.expectEqual(@as(usize, 2), merged.phases.len);
    try std.testing.expectEqual(@as(usize, 1), merged.agents.len);
    const later = JsonView.parseBytes(a,
        \\[{"type":"workflow_phase","index":1,"title":"two again"},{"type":"workflow_phase","index":2,"title":"three"},
        \\{"type":"workflow_agent","index":0,"attempt":0,"label":"older"},{"type":"workflow_agent","index":1,"attempt":1,"label":"y"}]
    );
    const updated = try mergeWorkflow(a, merged, later);
    try std.testing.expectEqual(@as(usize, 3), updated.phases.len);
    try std.testing.expectEqualStrings("two again", updated.phases[1].title);
    try std.testing.expectEqual(@as(usize, 2), updated.agents.len);
    try std.testing.expectEqualStrings("x", updated.agents[0].label);
    try std.testing.expectEqualStrings("y", updated.agents[1].label);
}
