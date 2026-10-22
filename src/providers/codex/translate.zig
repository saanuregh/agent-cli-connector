//! Turns app-server notifications and requests for one thread, and its
//! subagent threads, into events, and answers requests. The translator's
//! state is guarded by the session lock.
const std = @import("std");
const agent = @import("agent");
const decode = @import("decode.zig");
const session_module = @import("session.zig");
const CodexSession = session_module.CodexSession;
const Allocator = std.mem.Allocator;
const types = agent.types;
const errors = agent.errors;
const providerkit = agent.providerkit;
const Session = agent.Session;
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const ObjectBuilder = agent.json.ObjectBuilder;
const Id = agent.Id;
const Diagnostics = agent.Diagnostics;
const Value = std.json.Value;
const Error = errors.Error;
const IdMap = providerkit.IdMap;
const containsId = providerkit.containsId;
const StoredItem = providerkit.Owned(types.Item);
const StoredTask = providerkit.Owned(types.Task);
const eql = providerkit.eql;
const log = std.log.scoped(.agent);

/// Bounds the remembered main-thread turns.
const max_turn_order = 256;
/// Bounds the child threads remembered; beyond it the oldest ended ones are
/// forgotten.
const max_children = 256;
/// Bounds the recent stopped turns whose late subagent turns are stopped.
const max_stopped_turns = 32;

const RetryState = struct { attempt: u32 = 0, max: u32 = 0 };

pub const Translator = struct {
    /// Set while a turn/start that asked for an output schema is in flight
    /// and its turn has not started yet.
    start_schema: bool = false,
    schema_turns: IdMap(void) = .empty,
    final_text: IdMap([]u8) = .empty,
    /// Started main-thread items, in start order.
    items: IdMap(*StoredItem) = .empty,
    /// Child thread → the item that spawned it (empty when only the turn
    /// chain links them).
    children: IdMap(Id) = .empty,
    tasks: IdMap(*StoredTask) = .empty,
    /// Child thread → its running turn.
    child_turns: IdMap(Id) = .empty,
    /// Started items of each child thread, in start order.
    child_running: IdMap(std.ArrayList(*StoredItem)) = .empty,
    /// Child threads in the order they became known.
    child_order: std.ArrayList(Id) = .empty,
    /// Recent turns that ended interrupted or failed.
    stopped: std.ArrayList(Id) = .empty,
    /// The thread total at the latest usage update, valid when
    /// `have_total`; `turn_usage` sums the current turn's share.
    usage_total: decode.TokenUsage = .{},
    have_total: bool = false,
    turn_usage: decode.TokenUsage = .{},
    context_limit: i64 = 0,
    context_tokens: i64 = 0,
    retries: IdMap(RetryState) = .empty,
    /// The merged native rate-limit snapshot (JSON) per limit ID.
    rate_limits: IdMap([]u8) = .empty,
    /// Recent main-thread turns in start order, and the items reported for
    /// each, so a rewind can report them removed.
    turn_order: std.ArrayList(Id) = .empty,
    turn_item_ids: IdMap(std.ArrayList(Id)) = .empty,
    /// Pending async question requests → their turn.
    async_turns: IdMap(Id) = .empty,

    pub fn deinit(translator: *Translator, gpa: Allocator) void {
        translator.schema_turns.deinit(gpa);
        for (translator.final_text.values()) |text| gpa.free(text);
        translator.final_text.deinit(gpa);
        for (translator.items.values()) |item| item.destroy(gpa);
        translator.items.deinit(gpa);
        translator.children.deinit(gpa);
        for (translator.tasks.values()) |task| task.destroy(gpa);
        translator.tasks.deinit(gpa);
        translator.child_turns.deinit(gpa);
        for (translator.child_running.values()) |*list| freeItems(gpa, list);
        translator.child_running.deinit(gpa);
        translator.child_order.deinit(gpa);
        translator.stopped.deinit(gpa);
        translator.retries.deinit(gpa);
        for (translator.rate_limits.values()) |bytes| gpa.free(bytes);
        translator.rate_limits.deinit(gpa);
        translator.turn_order.deinit(gpa);
        for (translator.turn_item_ids.values()) |*list| list.deinit(gpa);
        translator.turn_item_ids.deinit(gpa);
        translator.async_turns.deinit(gpa);
    }
};

fn freeItems(gpa: Allocator, list: *std.ArrayList(*StoredItem)) void {
    for (list.items) |item| item.destroy(gpa);
    list.deinit(gpa);
}

fn lock(codex: *CodexSession) void {
    codex.mutex.lockUncancelable(codex.env.io);
}

fn unlock(codex: *CodexSession) void {
    codex.mutex.unlock(codex.env.io);
}

fn emit(codex: *CodexSession, turn: []const u8, raw: ?RawJson, payload: types.Event.Payload) void {
    codex.rt().emit(.{ .turn_id = turn, .raw = raw, .payload = payload });
}

/// Logs translator state lost for lack of memory. The reader cannot pass
/// the error to anyone, and the stream goes on without that state.
fn outOfMemory(what: []const u8) void {
    log.err("codex: {s} failed: out of memory", .{what});
}

/// Appends `id` to `list`, dropping the oldest entry beyond `max`.
fn appendBounded(gpa: Allocator, list: *std.ArrayList(Id), id: []const u8, max: usize) Allocator.Error!void {
    try list.append(gpa, .of(id));
    if (list.items.len > max) _ = list.orderedRemove(0);
}

// ---------------------------------------------------------------- starts

/// Records, before turn/start is sent, whether the turn asks for an output
/// schema: Codex can start and finish the turn before the reply arrives.
pub fn beginStart(codex: *CodexSession, has_schema: bool) void {
    lock(codex);
    defer unlock(codex);
    codex.translator.start_schema = has_schema;
}

/// Ends the turn/start of `turn` (empty when it failed). A schema not
/// already given to the turn when it started is given to it now.
pub fn endStart(codex: *CodexSession, turn: []const u8) void {
    lock(codex);
    defer unlock(codex);
    if (codex.translator.start_schema and turn.len > 0) codex.translator.schema_turns.put(codex.env.gpa, .of(turn), {}) catch
        outOfMemory("recording a turn's output schema");
    codex.translator.start_schema = false;
}

pub const ChildTurn = struct { thread: []const u8, turn: []const u8 };

pub fn childTurnsLocked(codex: *CodexSession, a: Allocator) []const ChildTurn {
    var out: std.ArrayList(ChildTurn) = .empty;
    collectChildTurns(codex, a, &out) catch outOfMemory("listing the subagent turns to interrupt");
    return out.items;
}

fn collectChildTurns(codex: *CodexSession, a: Allocator, out: *std.ArrayList(ChildTurn)) Allocator.Error!void {
    for (codex.translator.child_turns.keys(), codex.translator.child_turns.values()) |*thread, *turn| {
        try out.append(a, .{ .thread = try a.dupe(u8, thread.slice()), .turn = try a.dupe(u8, turn.slice()) });
    }
}

// ---------------------------------------------------------------- notifications

/// The app-server notifications mapped to events. Others become `native`
/// events.
const Notification = enum {
    @"serverRequest/resolved",
    @"turn/started",
    @"turn/completed",
    @"item/started",
    @"item/completed",
    @"item/agentMessage/delta",
    @"item/plan/delta",
    @"item/reasoning/summaryTextDelta",
    @"item/reasoning/summaryPartAdded",
    @"item/reasoning/textDelta",
    @"item/commandExecution/outputDelta",
    @"item/fileChange/outputDelta",
    @"item/mcpToolCall/progress",
    @"item/commandExecution/terminalInteraction",
    @"turn/plan/updated",
    @"turn/diff/updated",
    @"thread/tokenUsage/updated",
    @"thread/status/changed",
    @"error",
    @"thread/name/updated",
    @"thread/settings/updated",
    @"model/rerouted",
    warning,
    configWarning,
    deprecationNotice,
    guardianWarning,
    @"mcpServer/startupStatus/updated",
    @"hook/started",
    @"hook/completed",
    @"thread/goal/updated",
    @"thread/goal/cleared",
    @"account/rateLimits/updated",
    @"thread/closed",
};

pub fn notify(codex: *CodexSession, a: Allocator, method: []const u8, params: JsonView, line: []const u8) void {
    notifyInner(codex, a, method, params, line) catch |e| log.debug("codex: handling {s} failed: {t}", .{ method, e });
}

fn childParent(codex: *CodexSession, thread: []const u8) ?Id {
    if (thread.len == 0) return null;
    lock(codex);
    defer unlock(codex);
    if (codex.thread.eql(thread)) return null;
    return codex.translator.children.get(.of(thread));
}

fn notifyInner(codex: *CodexSession, a: Allocator, method: []const u8, params: JsonView, line: []const u8) !void {
    const raw = RawJson.of(line);
    const turn = params.str("turnId");
    const notification = std.meta.stringToEnum(Notification, method);
    if (notification) |known| if (known == .@"serverRequest/resolved") {
        codex.rt().withdrawRequest(decode.requestKey(a, params.get("requestId")));
        return;
    };
    if (childParent(codex, params.str("threadId"))) |parent| {
        return childNotify(codex, a, method, notification, params, try a.dupe(u8, parent.slice()), raw);
    }
    const known = notification orelse {
        emit(codex, turn, raw, .{ .native = .{ .name = method, .data = try params.raw(a) } });
        return;
    };
    const item_id = params.str("itemId");
    switch (known) {
        .@"serverRequest/resolved" => unreachable, // handled above
        .@"turn/started" => turnStarted(codex, params.get("turn").str("id"), raw),
        .@"turn/completed" => try turnCompleted(codex, a, params, raw),
        .@"item/started", .@"item/completed" => try onItem(codex, a, known == .@"item/completed", params, raw),
        .@"item/agentMessage/delta" => emit(codex, turn, raw, .{ .text_delta = .{ .item_id = item_id, .delta = params.str("delta") } }),
        .@"item/plan/delta" => emit(codex, turn, raw, .{ .plan_delta = .{ .item_id = item_id, .delta = params.str("delta") } }),
        .@"item/reasoning/summaryTextDelta" => emit(codex, turn, raw, .{ .reasoning_delta = .{ .item_id = item_id, .delta = params.str("delta"), .summary = true } }),
        .@"item/reasoning/summaryPartAdded" => emit(codex, turn, raw, .{ .reasoning_delta = .{ .item_id = item_id, .delta = "\n\n", .summary = true } }),
        .@"item/reasoning/textDelta" => emit(codex, turn, raw, .{ .reasoning_delta = .{ .item_id = item_id, .delta = params.str("delta") } }),
        .@"item/commandExecution/outputDelta", .@"item/fileChange/outputDelta" => emit(codex, turn, raw, .{ .tool_output_delta = .{ .item_id = item_id, .delta = params.str("delta") } }),
        .@"item/mcpToolCall/progress" => emit(codex, turn, raw, .{ .tool_output_delta = .{ .item_id = item_id, .delta = try std.fmt.allocPrint(a, "{s}\n", .{params.str("message")}) } }),
        .@"item/commandExecution/terminalInteraction" => {
            const data = try RawJson.encode(a, .{ .item_id = item_id, .process_id = params.str("processId"), .stdin = params.str("stdin") });
            emit(codex, turn, raw, .{ .native = .{ .name = "codex/terminal_interaction", .data = data } });
        },
        .@"turn/plan/updated" => try planUpdated(codex, a, turn, params, raw),
        .@"turn/diff/updated" => {
            emit(codex, turn, raw, .{ .native = .{ .name = "codex/diff_updated", .data = try RawJson.encode(a, .{ .diff = params.str("diff") }) } });
        },
        .@"thread/tokenUsage/updated" => tokenUsage(codex, turn, params, raw),
        .@"thread/status/changed" => statusChanged(codex, turn, params.get("status"), raw),
        .@"error" => {
            const err = params.get("error");
            if (params.flag("willRetry")) {
                emit(codex, turn, raw, retry(codex, a, turn, err));
            } else {
                emit(codex, turn, raw, .{ .notice = .{ .level = .@"error", .code = decode.errorCode(a, err.get("codexErrorInfo")), .message = decode.errorMessage(a, err) } });
            }
        },
        .@"thread/name/updated" => codex.updateInfo(.{ .title = params.str("threadName") }),
        .@"thread/settings/updated" => settingsUpdated(codex, params.get("threadSettings")),
        .@"model/rerouted" => try modelRerouted(codex, a, turn, params, raw),
        .warning, .configWarning, .deprecationNotice, .guardianWarning => emit(codex, turn, raw, .{ .notice = .{
            .level = .warning,
            .code = method,
            .message = providerkit.firstOf(&.{ params.str("message"), params.str("summary"), params.str("details") }),
        } }),
        .@"mcpServer/startupStatus/updated" => codex.updateInfo(.{ .mcp_server = .{ .name = params.str("name"), .status = params.str("status"), .@"error" = params.str("error") } }),
        .@"hook/started", .@"hook/completed" => try hookRun(codex, a, turn, params.get("run"), known == .@"hook/completed", raw),
        .@"thread/goal/updated" => emit(codex, turn, raw, .{ .goal_updated = .{ .goal = decode.goal(params.get("goal")) } }),
        .@"thread/goal/cleared" => emit(codex, turn, raw, .{ .goal_updated = .{} }),
        .@"account/rateLimits/updated" => try rateLimitsUpdated(codex, a, turn, params.get("rateLimits"), raw),
        .@"thread/closed" => {
            emit(codex, turn, raw, .{ .notice = .{ .level = .warning, .code = "thread_closed", .message = "the app server closed the thread" } });
            const id = codex.threadId();
            codex.client.unregister(id.slice());
            codex.rt().finish(.{ .kind = .closed, .message = try std.fmt.allocPrint(a, "codex: thread {s} was closed by the app server", .{id.slice()}) });
        },
    }
}

fn planUpdated(codex: *CodexSession, a: Allocator, turn: []const u8, params: JsonView, raw: RawJson) !void {
    var steps: std.ArrayList(types.PlanStep) = .empty;
    for (params.get("plan").items()) |step_value| {
        const step = JsonView.of(step_value);
        const status = step.str("status");
        try steps.append(a, .{ .text = step.str("step"), .status = if (eql(status, "inProgress")) .in_progress else if (eql(status, "completed")) .completed else .pending });
    }
    emit(codex, turn, raw, .{ .plan_updated = .{ .steps = steps.items, .explanation = params.str("explanation") } });
}

fn statusChanged(codex: *CodexSession, turn: []const u8, status: JsonView, raw: RawJson) void {
    const status_type = status.str("type");
    if (eql(status_type, "idle")) {
        emit(codex, turn, raw, .{ .status_changed = .{ .status = .idle } });
    } else if (eql(status_type, "active")) {
        var session_status: types.SessionStatus = .running;
        for (status.get("activeFlags").items()) |flag| {
            if (flag == .string and std.mem.startsWith(u8, flag.string, "waitingOn")) session_status = .waiting;
        }
        emit(codex, turn, raw, .{ .status_changed = .{ .status = session_status } });
    } else if (eql(status_type, "systemError")) {
        emit(codex, turn, raw, .{ .notice = .{ .level = .@"error", .code = "systemError", .message = "thread entered a system error state" } });
    }
}

fn settingsUpdated(codex: *CodexSession, settings: JsonView) void {
    lock(codex);
    defer unlock(codex);
    const model: ?[]const u8 = if (settings.has("model")) settings.str("model") else null;
    if (model) |name| codex.model = .of(name);
    codex.updateInfo(.{ .model = model, .effort = if (settings.has("effort")) settings.str("effort") else null });
}

fn modelRerouted(codex: *CodexSession, a: Allocator, turn: []const u8, params: JsonView, raw: RawJson) !void {
    const reason = params.get("reason");
    const why = if (reason.v != null and reason.v.? == .string) reason.string() else if (reason.v) |value| try agent.json.encodeValue(a, value) else "";
    emit(codex, turn, raw, .{ .notice = .{
        .level = .warning,
        .code = "model_rerouted",
        .message = try std.fmt.allocPrint(a, "model rerouted from {s} to {s} ({s})", .{ params.str("fromModel"), params.str("toModel"), why }),
    } });
    codex.updateInfo(.{ .model = params.str("toModel") });
}

fn hookRun(codex: *CodexSession, a: Allocator, turn: []const u8, run: JsonView, done: bool, raw: RawJson) !void {
    var out: std.ArrayList([]const u8) = .empty;
    for (run.get("entries").items()) |entry| try out.append(a, JsonView.of(entry).str("text"));
    if (run.str("statusMessage").len > 0) try out.append(a, run.str("statusMessage"));
    emit(codex, turn, raw, .{ .hook_run = .{
        .id = run.str("id"),
        .event = run.str("eventName"),
        .name = run.str("sourcePath"),
        .done = done,
        .status = run.str("status"),
        .outcome = decode.hookOutcome(run.str("status")),
        .output = try std.mem.join(a, "\n", out.items),
    } });
}

fn rateLimitsUpdated(codex: *CodexSession, a: Allocator, turn: []const u8, update: JsonView, raw: RawJson) !void {
    const snapshot = try mergeRateLimits(codex, a, update);
    const id = decode.limitId(snapshot);
    const limits = try decode.rateLimits(a, snapshot, id);
    if (eql(id, decode.main_limit_id)) {
        emit(codex, turn, raw, .{ .rate_limit_updated = .{ .limits = limits } });
    } else {
        // An allowance other than the main one, such as a model-specific
        // allowance.
        const Other = struct { limit_id: []const u8, limit_name: []const u8 = "", limits: []const types.RateLimit };
        const data = try agent.json.stringify(a, Other{ .limit_id = id, .limit_name = snapshot.str("limitName"), .limits = limits });
        emit(codex, turn, raw, .{ .native = .{ .name = "codex/other_rate_limit_updated", .data = .of(data) } });
    }
}

/// A retry event, reading "N/M" attempt progress from the message and
/// counting on from the turn's previous retry otherwise.
fn retry(codex: *CodexSession, a: Allocator, turn: []const u8, err: JsonView) types.Event.Payload {
    const message = err.str("message");
    lock(codex);
    defer unlock(codex);
    const previous = codex.translator.retries.get(.of(turn)) orelse RetryState{};
    var attempt = previous.attempt + 1;
    var max = previous.max;
    if (retryProgress(message)) |progress| {
        attempt = progress[0];
        max = progress[1];
    }
    // Lost, the next retry only counts from an earlier attempt.
    codex.translator.retries.put(codex.env.gpa, .of(turn), .{ .attempt = attempt, .max = max }) catch {};
    return .{ .retry = .{
        .attempt = attempt,
        .max_attempts = max,
        .message = decode.errorMessage(a, err),
        .code = decode.errorCode(a, err.get("codexErrorInfo")),
        .http_status = decode.httpStatus(err.get("codexErrorInfo")),
    } };
}

/// The first positive "N/M" in `message` (`\b(\d+)\s*/\s*(\d+)\b`).
fn retryProgress(message: []const u8) ?[2]u32 {
    var i: usize = 0;
    while (i < message.len) : (i += 1) {
        if (!std.ascii.isDigit(message[i]) or (i > 0 and isWordCharacter(message[i - 1]))) continue;
        var j = i;
        while (j < message.len and std.ascii.isDigit(message[j])) j += 1;
        const first = message[i..j];
        var k = j;
        while (k < message.len and std.ascii.isWhitespace(message[k])) k += 1;
        if (k >= message.len or message[k] != '/') continue;
        k += 1;
        while (k < message.len and std.ascii.isWhitespace(message[k])) k += 1;
        const start = k;
        while (k < message.len and std.ascii.isDigit(message[k])) k += 1;
        if (k == start or (k < message.len and isWordCharacter(message[k]))) continue;
        const attempt = std.fmt.parseInt(u32, first, 10) catch continue;
        const max = std.fmt.parseInt(u32, message[start..k], 10) catch continue;
        if (attempt > 0 and max > 0) return .{ attempt, max };
        return null;
    }
    return null;
}

fn isWordCharacter(c: u8) bool {
    return std.ascii.isAlphanumeric(c) or c == '_';
}

/// A failed turn's error. Usage and rate limits are retryable and carry
/// the time the exhausted windows reset.
fn turnError(codex: *CodexSession, a: Allocator, err: JsonView) types.ErrorInfo {
    var out: types.ErrorInfo = .{ .kind = .turn_failed, .code = "failed", .message = "turn failed" };
    if (err.isNull()) return out;
    out.message = decode.errorMessage(a, err);
    out.http_status = decode.httpStatus(err.get("codexErrorInfo"));
    const code = decode.errorCode(a, err.get("codexErrorInfo"));
    if (code.len > 0) out.code = code;
    if (eql(out.code, "usageLimitExceeded") or eql(out.code, "rateLimitExceeded")) {
        out.retryable = true;
        lock(codex);
        defer unlock(codex);
        const snapshot = rateLimitSnapshotLocked(codex, a, decode.main_limit_id);
        const limits = decode.rateLimits(a, snapshot, decode.main_limit_id) catch &.{};
        out.resets_at_ms = decode.resetsAtMs(limits);
    } else if (eql(out.code, "serverOverloaded")) {
        out.retryable = true;
    }
    return out;
}

fn turnStarted(codex: *CodexSession, id: []const u8, raw: RawJson) void {
    const gpa = codex.env.gpa;
    const translator = &codex.translator;
    lock(codex);
    translator.turn_usage = .{};
    // A turn starting while a turn/start is in flight is that start's turn
    // (see Runtime.runStarted), so it takes the start's schema.
    if (translator.start_schema and id.len > 0) {
        translator.schema_turns.put(gpa, .of(id), {}) catch outOfMemory("recording a turn's output schema");
        translator.start_schema = false;
    }
    if (id.len > 0 and !containsId(translator.turn_order.items, id)) {
        if (translator.turn_order.append(gpa, .of(id))) {
            if (translator.turn_order.items.len > max_turn_order) {
                const old = translator.turn_order.orderedRemove(0);
                if (translator.turn_item_ids.fetchOrderedRemove(old)) |kv| {
                    var list = kv.value;
                    list.deinit(gpa);
                }
            }
        } else |_| outOfMemory("remembering a turn for rewind");
    }
    unlock(codex);
    // While a start is in flight, the start registers its turn.
    _, const starting = codex.rt().runStarted(id);
    if (!starting and id.len > 0) codex.rt().trackTurn(id);
    emit(codex, id, raw, .{ .turn_started = .{} });
}

fn turnCompleted(codex: *CodexSession, a: Allocator, params: JsonView, raw: RawJson) !void {
    const gpa = codex.env.gpa;
    const translator = &codex.translator;
    // A compaction that failed ends with its turn rather than its item.
    lock(codex);
    if (codex.compacting) endCompactionLocked(codex);
    unlock(codex);
    const turn = params.get("turn");
    const id = turn.str("id");
    var status = turn.str("status");
    const stopping, const pending = codex.rt().interrupting();
    if (id.len > 0 and stopping.eql(id) and !pending and eql(status, "completed")) {
        // Codex can finish a turn normally while the interrupt is in
        // flight; the caller asked for it to stop.
        status = "interrupted";
    }
    var result: types.TurnResult = .{ .turn_id = id, .status = .completed, .raw = raw, .stop_reason = turn.str("status"), .duration_ms = turn.int("durationMs") };
    if (eql(status, "interrupted")) {
        result.status = .interrupted;
        result.@"error" = .{ .kind = .interrupted, .code = "interrupted", .message = "turn interrupted" };
    } else if (eql(status, "failed")) {
        result.status = .failed;
        result.@"error" = turnError(codex, a, turn.get("error"));
    }
    if (result.status != .completed) endRunningItems(codex, a, id, result.status);
    // An interrupt dismisses the questions the agent asked.
    if (result.status == .interrupted) withdrawAsync(codex, a, "", true);

    lock(codex);
    if (result.status != .completed) appendBounded(gpa, &translator.stopped, id, max_stopped_turns) catch
        outOfMemory("remembering a stopped turn");
    _ = translator.retries.orderedRemove(.of(id));
    if (translator.final_text.fetchOrderedRemove(.of(id))) |kv| {
        result.text = a.dupe(u8, kv.value) catch "";
        gpa.free(kv.value);
    }
    if (translator.schema_turns.orderedRemove(.of(id)) and result.text.len > 0) {
        if (std.json.parseFromSliceLeaky(Value, a, result.text, .{})) |_| {
            result.structured_output = .of(result.text);
        } else |_| {}
    }
    result.usage = turnUsageLocked(codex);
    translator.turn_usage = .{};
    unlock(codex);
    // A turn unknown while a turn/start is in flight may be that start's:
    // the runtime holds its result until the start is accepted. Steered
    // input that joined the turn ends with it.
    codex.rt().finishTurn(id, result);
}

pub fn endCompactionLocked(codex: *CodexSession) void {
    codex.compacting = false;
    codex.compact_condition.broadcast(codex.env.io);
}

/// Completes the tool items of a stopped turn that Codex left running, so
/// they do not stay in progress forever.
fn endRunningItems(codex: *CodexSession, a: Allocator, turn: []const u8, status: types.TurnStatus) void {
    const item_status: types.ItemStatus = if (status == .failed) .failed else .cancelled;
    var ended: std.ArrayList(types.Item) = .empty;
    lock(codex);
    var i: usize = 0;
    while (i < codex.translator.items.count()) {
        const stored = codex.translator.items.values()[i];
        if (!eql(stored.value.turn_id, turn)) {
            i += 1;
            continue;
        }
        codex.translator.items.orderedRemoveAt(i);
        defer stored.destroy(codex.env.gpa);
        var item = agent.clone(types.Item, a, stored.value) catch {
            outOfMemory("ending a stopped turn's item");
            continue;
        };
        if (item.tool) |*call| call.is_error = status == .failed;
        item.status = item_status;
        ended.append(a, item) catch outOfMemory("ending a stopped turn's item");
    }
    unlock(codex);
    for (ended.items) |item| emit(codex, turn, null, .{ .item_completed = .{ .item = item } });
}

/// Adds the growth of the thread total to the running turn. A total that
/// went down, as after a revert or compaction, counts the latest request's
/// usage instead; updates for other turns only move the baseline.
fn tokenUsage(codex: *CodexSession, turn: []const u8, params: JsonView, raw: RawJson) void {
    const token_usage = params.get("tokenUsage");
    if (token_usage.object() == null) return;
    const total = decode.TokenUsage.parse(token_usage.get("total"));
    const last = decode.TokenUsage.parse(token_usage.get("last"));
    const translator = &codex.translator;
    lock(codex);
    if (token_usage.has("modelContextWindow")) translator.context_limit = token_usage.int("modelContextWindow");
    translator.context_tokens = last.input + last.output;
    const running, _ = codex.rt().runningTurn();
    const counted = turn.len == 0 or running.eql(turn);
    if (counted) {
        var delta = last;
        if (translator.have_total and !total.below(translator.usage_total)) delta = total.minus(translator.usage_total);
        translator.turn_usage = translator.turn_usage.plus(delta);
    }
    translator.usage_total = total;
    translator.have_total = true;
    const usage = turnUsageLocked(codex);
    unlock(codex);
    if (counted) emit(codex, turn, raw, .{ .usage_updated = .{ .usage = usage } });
}

fn turnUsageLocked(codex: *CodexSession) types.Usage {
    var usage = codex.translator.turn_usage.usage();
    usage.context_window = codex.translator.context_limit;
    usage.context_tokens = codex.translator.context_tokens;
    return usage;
}

/// Handles item/started and item/completed of the main thread.
fn onItem(codex: *CodexSession, a: Allocator, completed: bool, params: JsonView, raw: RawJson) !void {
    const gpa = codex.env.gpa;
    const translator = &codex.translator;
    const converted = try decode.threadItem(a, params.get("item"));
    var item = converted.item;
    const extra = converted.extra;
    const turn = params.str("turnId");
    item.turn_id = turn;
    if (!completed) item.status = .in_progress;
    var trigger: []const u8 = "auto";
    var restore: ?[]u8 = null;
    lock(codex);
    noteItemLocked(codex, turn, item.id);
    if (completed) {
        if (item.kind == .assistant_message) {
            const have = if (translator.final_text.get(.of(turn))) |text| text.len > 0 else false;
            if (eql(extra.phase, "final_answer") or extra.phase.len == 0 or !have) {
                if (gpa.dupe(u8, item.text)) |copy| {
                    if (translator.final_text.fetchOrderedRemove(.of(turn))) |old| gpa.free(old.value);
                    translator.final_text.put(gpa, .of(turn), copy) catch {
                        gpa.free(copy);
                        outOfMemory("recording a turn's final text");
                    };
                } else |_| outOfMemory("recording a turn's final text");
            }
        }
        if (translator.items.fetchOrderedRemove(.of(item.id))) |kv| kv.value.destroy(gpa);
    } else if (StoredItem.create(gpa, item)) |stored| {
        if (translator.items.getPtr(.of(item.id))) |slot| {
            slot.*.destroy(gpa);
            slot.* = stored;
        } else translator.items.put(gpa, .of(item.id), stored) catch {
            stored.destroy(gpa);
            outOfMemory("tracking a running item");
        };
    } else |_| outOfMemory("tracking a running item");
    if (item.kind == .compaction) {
        if (codex.compacting) trigger = "manual";
        if (completed) {
            endCompactionLocked(codex);
            if (!codex.opts.disable_context_restore) if (codex.context) |context| {
                restore = gpa.dupe(u8, context) catch blk: {
                    outOfMemory("restoring additional context after compaction");
                    break :blk null;
                };
            };
        }
    }
    unlock(codex);
    for (extra.children) |child| addChild(codex, child, item.id);
    if (completed) subagentItem(codex, a, params.get("item"), "", raw);
    if (item.kind == .compaction) emit(codex, turn, raw, .{ .compaction = .{ .done = completed, .trigger = trigger } });
    // The reader task must stay free to read the reply.
    if (restore) |entries| codex.rt().spawn(restoreContext, .{ codex, entries }) catch {
        gpa.free(entries);
        log.err("codex: cannot start a task to restore additional context after compaction", .{});
    };
    if (completed) {
        emit(codex, turn, raw, .{ .item_completed = .{ .item = item } });
        if (extra.is_async and extra.questions.len > 0) try asyncQuestions(codex, a, item, extra.questions, raw);
    } else {
        emit(codex, turn, raw, .{ .item_started = .{ .item = item } });
    }
}

/// Injects the last additional context again after compaction dropped it:
/// application entries as developer messages, untrusted entries as user
/// messages in <external_key> tags. Frees `entries`.
fn restoreContext(codex: *CodexSession, entries: []u8) void {
    defer codex.env.gpa.free(entries);
    var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
    defer arena.deinit();
    injectContext(codex, arena.allocator(), entries) catch |e|
        log.warn("codex: restoring additional context after compaction failed: {t}", .{e});
}

fn injectContext(codex: *CodexSession, a: Allocator, entries: []const u8) Error!void {
    const context = agent.json.parseValue(a, entries) orelse return;
    if (context != .object) return;
    var items: std.json.Array = .init(a);
    for (try providerkit.sortedKeys(a, context.object.keys())) |key| {
        const entry = JsonView.of(context.object.get(key).?);
        if (entry.object() == null) continue;
        const untrusted = eql(entry.str("kind"), "untrusted");
        const text = if (untrusted)
            try std.fmt.allocPrint(a, "<external_{s}>{s}</external_{s}>", .{ key, entry.str("value"), key })
        else
            try std.fmt.allocPrint(a, "<{s}>{s}</{s}>", .{ key, entry.str("value"), key });
        try items.append(try agent.json.toValue(a, .{
            .type = "message",
            .role = if (untrusted) "user" else "developer",
            .content = .{.{ .type = "input_text", .text = text }},
        }));
    }
    if (items.items.len == 0) return;
    var params = try codex.threadIdParams(a);
    try params.put("items", .{ .array = items });
    _ = try codex.threadCall(a, "thread/inject_items", params.value(), 10_000, null);
}

/// Raises the questions of an asynchronous agent message. The answer is
/// sent as the next user message.
fn asyncQuestions(codex: *CodexSession, a: Allocator, item: types.Item, native_questions: []const decode.AsyncQuestion, raw: RawJson) !void {
    var questions: std.ArrayList(types.Question) = .empty;
    var titles: std.ArrayList([]const u8) = .empty;
    for (native_questions, 0..) |question, i| {
        var choices: std.ArrayList(types.QuestionOption) = .empty;
        for (question.options) |label| try choices.append(a, .{ .label = label });
        try questions.append(a, .{ .id = try std.fmt.allocPrint(a, "{d}", .{i}), .header = "Question", .text = question.title, .options = choices.items, .allow_other = true });
        try titles.append(a, question.title);
    }
    const request_id = try std.fmt.allocPrint(a, "async:{s}", .{item.id});
    lock(codex);
    const tracked = codex.translator.async_turns.put(codex.env.gpa, .of(request_id), .of(item.turn_id));
    unlock(codex);
    try tracked;
    const native = try nativeRequest(a, "null", "async", try agent.json.toValue(a, .{ .questions = titles.items }));
    codex.rt().request(.{ .user_input_request = .{ .id = request_id, .questions = questions.items, .is_async = true } }, raw, native);
}

/// Withdraws the pending async questions of `turn`, or all with `all`.
fn withdrawAsync(codex: *CodexSession, a: Allocator, turn: []const u8, all: bool) void {
    var ids: std.ArrayList([]const u8) = .empty;
    lock(codex);
    var i: usize = 0;
    while (i < codex.translator.async_turns.count()) {
        const question_turn = codex.translator.async_turns.values()[i];
        if (all or question_turn.eql(turn)) {
            const id = a.dupe(u8, codex.translator.async_turns.keys()[i].slice()) catch "";
            ids.append(a, id) catch outOfMemory("withdrawing an async question");
            codex.translator.async_turns.orderedRemoveAt(i);
        } else i += 1;
    }
    unlock(codex);
    for (ids.items) |id| codex.rt().withdrawRequest(id);
}

/// Records that item `id` was reported for `turn`, a remembered turn.
fn noteItemLocked(codex: *CodexSession, turn: []const u8, id: []const u8) void {
    const translator = &codex.translator;
    if (turn.len == 0 or id.len == 0 or !containsId(translator.turn_order.items, turn)) return;
    const gop = translator.turn_item_ids.getOrPut(codex.env.gpa, .of(turn)) catch return outOfMemory("remembering an item for rewind");
    if (!gop.found_existing) gop.value_ptr.* = .empty;
    if (!containsId(gop.value_ptr.items, id)) gop.value_ptr.append(codex.env.gpa, .of(id)) catch outOfMemory("remembering an item for rewind");
}

/// Forgets `turn` and later turns after a rewind, withdraws their pending
/// async questions, and reports their items removed. An unknown turn came
/// before every turn this session saw, so all of them go.
pub fn rewound(codex: *CodexSession, a: Allocator, turn: []const u8) void {
    const gpa = codex.env.gpa;
    const translator = &codex.translator;
    var removed: std.ArrayList([]const u8) = .empty;
    var ids: std.ArrayList([]const u8) = .empty;
    lock(codex);
    var index: usize = 0;
    for (translator.turn_order.items, 0..) |*remembered, i| if (remembered.eql(turn)) {
        index = i;
        break;
    };
    const known = containsId(translator.turn_order.items, turn);
    for (translator.turn_order.items[index..]) |*remembered| {
        removed.append(a, a.dupe(u8, remembered.slice()) catch "") catch outOfMemory("reporting rewound turns");
        if (translator.turn_item_ids.fetchOrderedRemove(remembered.*)) |kv| {
            var list = kv.value;
            for (list.items) |*item_id| ids.append(a, a.dupe(u8, item_id.slice()) catch "") catch outOfMemory("reporting rewound items");
            list.deinit(gpa);
        }
    }
    translator.turn_order.shrinkRetainingCapacity(index);
    var withdraw: std.ArrayList([]const u8) = .empty;
    var i: usize = 0;
    while (i < translator.async_turns.count()) {
        const question_turn = translator.async_turns.values()[i];
        const hit = !known or for (removed.items) |removed_turn| {
            if (question_turn.eql(removed_turn)) break true;
        } else false;
        if (hit) {
            withdraw.append(a, a.dupe(u8, translator.async_turns.keys()[i].slice()) catch "") catch outOfMemory("withdrawing an async question");
            translator.async_turns.orderedRemoveAt(i);
        } else i += 1;
    }
    unlock(codex);
    for (withdraw.items) |id| codex.rt().withdrawRequest(id);
    if (ids.items.len > 0) emit(codex, "", null, .{ .items_removed = .{ .ids = ids.items } });
}

// ---------------------------------------------------------------- subagents

/// Maps a subagent thread's notifications into the parent's stream: its
/// items nest under the spawning item and carry the child thread as
/// subagent ID, and its turns update the subagent's task.
fn childNotify(codex: *CodexSession, a: Allocator, method: []const u8, notification: ?Notification, params: JsonView, parent: []const u8, raw: RawJson) !void {
    const main_turn = try a.dupe(u8, codex.rt().currentTurn().slice());
    const child = params.str("threadId");
    const turn = params.get("turn");
    const item_id = params.str("itemId");
    const known = notification orelse return childNative(codex, a, main_turn, method, params, raw);
    switch (known) {
        .@"item/started", .@"item/completed" => try childItem(codex, a, params, known == .@"item/started", parent, main_turn, raw),
        .@"item/agentMessage/delta" => emit(codex, main_turn, raw, .{ .text_delta = .{ .item_id = item_id, .parent_id = parent, .subagent_id = child, .delta = params.str("delta") } }),
        .@"item/reasoning/summaryTextDelta", .@"item/reasoning/textDelta" => emit(codex, main_turn, raw, .{ .reasoning_delta = .{
            .item_id = item_id,
            .parent_id = parent,
            .subagent_id = child,
            .delta = params.str("delta"),
            .summary = known == .@"item/reasoning/summaryTextDelta",
        } }),
        .@"item/commandExecution/outputDelta", .@"item/fileChange/outputDelta" => emit(codex, main_turn, raw, .{ .tool_output_delta = .{ .item_id = item_id, .delta = params.str("delta") } }),
        .@"turn/started" => {
            updateTask(codex, a, child, main_turn, raw, .{ .turn_started = turn.str("id") });
            stopLateChildTurn(codex, child, turn.str("id"), turn.str("rootTurnId"));
        },
        .@"thread/settings/updated" => updateTask(codex, a, child, main_turn, raw, .{ .settings = params.get("threadSettings") }),
        .@"model/rerouted" => updateTask(codex, a, child, main_turn, raw, .{ .model = params.str("toModel") }),
        .@"turn/completed" => {
            const status = turn.str("status");
            if (eql(status, "failed")) endChildItems(codex, a, child, turn.str("id"), .failed, main_turn, raw);
            if (eql(status, "interrupted")) endChildItems(codex, a, child, turn.str("id"), .cancelled, main_turn, raw);
            updateTask(codex, a, child, main_turn, raw, .{ .turn_completed = turn });
        },
        .@"thread/tokenUsage/updated" => updateTask(codex, a, child, main_turn, raw, .{ .usage = decode.TokenUsage.parse(params.get("tokenUsage").get("total")) }),
        else => try childNative(codex, a, main_turn, method, params, raw),
    }
}

fn childNative(codex: *CodexSession, a: Allocator, main_turn: []const u8, method: []const u8, params: JsonView, raw: RawJson) !void {
    emit(codex, main_turn, raw, .{ .native = .{ .name = try std.fmt.allocPrint(a, "subagent/{s}", .{method}), .data = try params.raw(a) } });
}

fn childItem(codex: *CodexSession, a: Allocator, params: JsonView, started: bool, parent: []const u8, main_turn: []const u8, raw: RawJson) !void {
    const child = params.str("threadId");
    const converted = try decode.threadItem(a, params.get("item"));
    var item = converted.item;
    item.parent_id = parent;
    item.subagent_id = child;
    item.turn_id = params.str("turnId");
    if (started) item.status = .in_progress;
    trackChildItem(codex, child, item, started, main_turn);
    for (converted.extra.children) |grandchild| addChild(codex, grandchild, item.id);
    if (!started) {
        childItemDone(codex, a, child, item, converted.extra, raw);
        subagentItem(codex, a, params.get("item"), child, raw);
        emit(codex, main_turn, raw, .{ .item_completed = .{ .item = item } });
    } else {
        emit(codex, main_turn, raw, .{ .item_started = .{ .item = item } });
    }
}

/// Interrupts a subagent turn that started after its parent turn was
/// interrupted or failed, or while an interrupt of it is in flight.
fn stopLateChildTurn(codex: *CodexSession, child: []const u8, turn: []const u8, root: []const u8) void {
    var stopping, const pending = codex.rt().interrupting();
    if (pending) stopping = .{};
    lock(codex);
    const stop = (root.len > 0 and containsId(codex.translator.stopped.items, root)) or
        (!stopping.isEmpty() and (root.len == 0 or stopping.eql(root)));
    unlock(codex);
    if (stop) codex.rt().spawn(interruptChild, .{ codex, Id.of(child), Id.of(turn) }) catch
        log.err("codex: cannot start a task to interrupt the late subagent turn of {s}", .{child});
}

fn interruptChild(codex: *CodexSession, child: Id, turn: Id) void {
    var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
    defer arena.deinit();
    sendChildInterrupt(codex, arena.allocator(), child.slice(), turn.slice()) catch |e|
        log.debug("codex: interrupting late subagent turn of {s} failed: {t}", .{ child.slice(), e });
}

fn sendChildInterrupt(codex: *CodexSession, a: Allocator, child: []const u8, turn: []const u8) Error!void {
    const params = try agent.json.toValue(a, .{ .threadId = child, .turnId = turn });
    _ = try codex.client.call(a, "turn/interrupt", params, .{ .owner = codex, .timeout_ms = 10_000 }, null);
}

/// Records a child thread's item as running when started and forgets it
/// when completed. `main_turn` is the turn the item is reported under.
fn trackChildItem(codex: *CodexSession, thread: []const u8, item: types.Item, started: bool, main_turn: []const u8) void {
    const gpa = codex.env.gpa;
    lock(codex);
    defer unlock(codex);
    noteItemLocked(codex, main_turn, item.id);
    const gop = codex.translator.child_running.getOrPut(gpa, .of(thread)) catch return outOfMemory("tracking a subagent item");
    if (!gop.found_existing) gop.value_ptr.* = .empty;
    const list = gop.value_ptr;
    const index = for (list.items, 0..) |running, i| {
        if (eql(running.value.id, item.id)) break i;
    } else null;
    if (started) {
        const stored = StoredItem.create(gpa, item) catch return outOfMemory("tracking a subagent item");
        if (index) |i| {
            list.items[i].destroy(gpa);
            list.items[i] = stored;
        } else list.append(gpa, stored) catch {
            stored.destroy(gpa);
            outOfMemory("tracking a subagent item");
        };
    } else if (index) |i| {
        list.orderedRemove(i).destroy(gpa);
    }
    if (list.items.len == 0) {
        list.deinit(gpa);
        _ = codex.translator.child_running.orderedRemove(.of(thread));
    }
}

/// Completes the items of a child thread's stopped turn that Codex left
/// running.
fn endChildItems(codex: *CodexSession, a: Allocator, thread: []const u8, turn: []const u8, status: types.ItemStatus, main_turn: []const u8, raw: RawJson) void {
    const gpa = codex.env.gpa;
    var ended: std.ArrayList(types.Item) = .empty;
    lock(codex);
    if (codex.translator.child_running.getPtr(.of(thread))) |list| {
        var i: usize = 0;
        while (i < list.items.len) {
            const stored = list.items[i];
            const item_turn = stored.value.turn_id;
            if (turn.len > 0 and item_turn.len > 0 and !eql(item_turn, turn)) {
                i += 1;
                continue;
            }
            _ = list.orderedRemove(i);
            defer stored.destroy(gpa);
            var item = agent.clone(types.Item, a, stored.value) catch {
                outOfMemory("ending a stopped subagent item");
                continue;
            };
            if (item.tool) |*call| call.is_error = status == .failed;
            item.status = status;
            ended.append(a, item) catch outOfMemory("ending a stopped subagent item");
        }
        if (list.items.len == 0) {
            list.deinit(gpa);
            _ = codex.translator.child_running.orderedRemove(.of(thread));
        }
    }
    unlock(codex);
    for (ended.items) |item| emit(codex, main_turn, raw, .{ .item_completed = .{ .item = item } });
}

/// Routes a subagent thread's messages to this session. `spawn` is the
/// item that started it, or empty when only the turn chain links them.
pub fn addChild(codex: *CodexSession, thread: []const u8, spawn: []const u8) void {
    lock(codex);
    const old = codex.translator.children.get(.of(thread));
    if (old == null) trackChildLocked(codex, thread);
    if (old == null or (old.?.isEmpty() and spawn.len > 0)) codex.translator.children.put(codex.env.gpa, .of(thread), .of(spawn)) catch
        outOfMemory("linking a subagent thread");
    unlock(codex);
    codex.client.registerChild(thread, codex);
    pruneChildren(codex, thread);
}

fn trackChildLocked(codex: *CodexSession, thread: []const u8) void {
    if (codex.translator.children.contains(.of(thread)) or codex.translator.tasks.contains(.of(thread))) return;
    codex.translator.child_order.append(codex.env.gpa, .of(thread)) catch outOfMemory("remembering a subagent thread");
}

/// Forgets the oldest child threads that are not running while more than
/// `max_children` are known, and stops routing them: their later messages
/// are held like those of any unknown thread until a turn chain or an item
/// names them again. `keep` is never forgotten.
fn pruneChildren(codex: *CodexSession, keep: []const u8) void {
    const gpa = codex.env.gpa;
    const translator = &codex.translator;
    var forgotten: std.ArrayList(Id) = .empty;
    defer forgotten.deinit(gpa);
    lock(codex);
    var i: usize = 0;
    while (translator.child_order.items.len > max_children and i < translator.child_order.items.len) {
        const thread = translator.child_order.items[i];
        const task = translator.tasks.get(thread);
        const running = (task != null and task.?.value.status == .running);
        if (thread.eql(keep) or translator.child_turns.contains(thread) or translator.child_running.contains(thread) or running) {
            i += 1;
            continue;
        }
        // Without room to remember it, the thread is kept, so it is never
        // routed here while this session no longer knows it.
        forgotten.append(gpa, thread) catch {
            outOfMemory("forgetting a subagent thread");
            break;
        };
        _ = translator.child_order.orderedRemove(i);
        _ = translator.children.orderedRemove(thread);
        if (translator.tasks.fetchOrderedRemove(thread)) |kv| kv.value.destroy(gpa);
        _ = translator.child_turns.orderedRemove(thread);
    }
    unlock(codex);
    for (forgotten.items) |*thread| codex.client.unregisterChild(thread.slice(), codex);
}

/// A change to a subagent's task.
const TaskUpdate = union(enum) {
    /// The subagent's turn with this ID started.
    turn_started: []const u8,
    /// The thread settings Codex reported.
    settings: JsonView,
    /// Codex rerouted the subagent to this model.
    model: []const u8,
    /// The subagent's turn ended.
    turn_completed: JsonView,
    usage: decode.TokenUsage,
    /// The subagent's final answer.
    output: []const u8,
    /// The subagent called the tool with this name.
    tool_used: []const u8,
    /// What a subagent item says about the task.
    change: decode.TaskChange,
};

/// Applies `update` to `task`, under the session lock.
fn applyTaskUpdate(codex: *CodexSession, task: *types.Task, update: TaskUpdate) void {
    switch (update) {
        .turn_started => |turn| {
            task.status = .running;
            codex.translator.child_turns.put(codex.env.gpa, .of(task.id), .of(turn)) catch outOfMemory("tracking a subagent turn");
        },
        .settings => |settings| {
            if (settings.has("model")) task.subagent.?.model = settings.str("model");
            if (settings.has("effort")) task.subagent.?.effort = settings.str("effort");
        },
        .model => |model| task.subagent.?.model = model,
        .turn_completed => |turn| {
            _ = codex.translator.child_turns.orderedRemove(.of(task.id));
            const status = turn.str("status");
            if (eql(status, "failed")) {
                task.status = .failed;
                if (!turn.get("error").isNull()) task.@"error" = turn.get("error").str("message");
            } else if (eql(status, "interrupted")) {
                task.status = .stopped;
            } else task.status = .completed;
            if (turn.has("durationMs")) task.duration_ms = turn.int("durationMs");
        },
        .usage => |total| task.usage = total.usage(),
        .output => |text| task.output = text,
        .tool_used => |tool| {
            task.tool_uses += 1;
            task.last_tool = tool;
        },
        .change => |change| change.apply(task),
    }
}

/// Changes a subagent's task under the session lock and emits its new
/// state.
fn updateTask(codex: *CodexSession, a: Allocator, thread: []const u8, main_turn: []const u8, raw: ?RawJson, update: TaskUpdate) void {
    const gpa = codex.env.gpa;
    lock(codex);
    const existing = codex.translator.tasks.get(.of(thread));
    var task: types.Task = if (existing) |stored| (agent.clone(types.Task, a, stored.value) catch {
        unlock(codex);
        return outOfMemory("updating a subagent task");
    }) else blk: {
        trackChildLocked(codex, thread);
        var new_task = decode.newSubagentTask(a.dupe(u8, thread) catch thread);
        if (codex.translator.children.get(.of(thread))) |spawn| new_task.tool_call_id = a.dupe(u8, spawn.slice()) catch "";
        break :blk new_task;
    };
    if (task.subagent == null) task.subagent = .{};
    applyTaskUpdate(codex, &task, update);
    if (StoredTask.create(gpa, task)) |stored| {
        if (codex.translator.tasks.getPtr(.of(thread))) |slot| {
            slot.*.destroy(gpa);
            slot.* = stored;
        } else codex.translator.tasks.put(gpa, .of(thread), stored) catch {
            stored.destroy(gpa);
            outOfMemory("storing a subagent task");
        };
    } else |_| outOfMemory("storing a subagent task");
    unlock(codex);
    emit(codex, main_turn, raw, .{ .task_updated = .{ .task = task } });
    if (task.status != .running) pruneChildren(codex, "");
}

/// Records what a finished child item says about its task.
fn childItemDone(codex: *CodexSession, a: Allocator, thread: []const u8, item: types.Item, extra: decode.Extra, raw: RawJson) void {
    const main_turn = a.dupe(u8, codex.rt().currentTurn().slice()) catch "";
    if (item.kind == .assistant_message and (eql(extra.phase, "final_answer") or extra.phase.len == 0)) {
        updateTask(codex, a, thread, main_turn, raw, .{ .output = item.text });
    } else if (item.kind == .tool_call) {
        updateTask(codex, a, thread, main_turn, raw, .{ .tool_used = if (item.tool) |tool| tool.name else "" });
    }
}

/// Updates tasks from a subAgentActivity or collabAgentToolCall item.
/// `sender` is the thread that produced it: "" for the main thread, or a
/// subagent that started nested subagents.
fn subagentItem(codex: *CodexSession, a: Allocator, thread_item: JsonView, sender: []const u8, raw: RawJson) void {
    const main = a.dupe(u8, codex.threadId().slice()) catch return outOfMemory("updating subagent tasks");
    var depth: u32 = 0;
    if (sender.len > 0) {
        lock(codex);
        depth = 1;
        if (codex.translator.tasks.get(.of(sender))) |stored| if (stored.value.subagent) |subagent| if (subagent.depth > 0) {
            depth = subagent.depth;
        };
        unlock(codex);
    }
    const main_turn = a.dupe(u8, codex.rt().currentTurn().slice()) catch "";
    const changes = decode.subagentChanges(a, thread_item, sender, depth, .{ .exclude = .{ main, sender } }) catch
        return outOfMemory("updating subagent tasks");
    for (changes) |change| updateTask(codex, a, change.thread, main_turn, raw, .{ .change = change });
}

// ---------------------------------------------------------------- rate limits

/// Folds a partial rate-limit update into the snapshot of its limit:
/// fields the update carries replace earlier values, absent ones keep them.
fn mergeRateLimits(codex: *CodexSession, a: Allocator, update: JsonView) !JsonView {
    const id = decode.limitId(update);
    lock(codex);
    defer unlock(codex);
    const old = rateLimitSnapshotLocked(codex, a, id);
    var merged: ObjectBuilder = .init(a);
    if (old.object()) |object| try merged.putAll(object);
    if (update.object()) |object| {
        for (object.keys(), object.values()) |key, value| try merged.put(key, try decode.mergeObject(a, old.get(key).v, value));
    }
    try storeRateLimitLocked(codex, id, try agent.json.encodeValue(a, merged.value()));
    return JsonView.of(merged.value());
}

pub fn storeRateLimitLocked(codex: *CodexSession, id: []const u8, bytes: []const u8) !void {
    const gpa = codex.env.gpa;
    const copy = try gpa.dupe(u8, bytes);
    if (codex.translator.rate_limits.getPtr(.of(id))) |slot| {
        gpa.free(slot.*);
        slot.* = copy;
    } else codex.translator.rate_limits.put(gpa, .of(id), copy) catch |e| {
        gpa.free(copy);
        return e;
    };
}

pub fn rateLimitSnapshotLocked(codex: *CodexSession, a: Allocator, id: []const u8) JsonView {
    const bytes = codex.translator.rate_limits.get(.of(id)) orelse return JsonView.empty;
    return JsonView.parseBytes(a, bytes);
}

// ---------------------------------------------------------------- requests

/// The app-server requests answered through the session. Others are
/// refused with JSON-RPC error -32601.
const RequestMethod = enum {
    @"item/tool/call",
    @"item/commandExecution/requestApproval",
    @"item/fileChange/requestApproval",
    @"item/permissions/requestApproval",
    @"item/tool/requestUserInput",
    @"mcpServer/elicitation/request",
    applyPatchApproval,
    execCommandApproval,
};

/// The parts of a server request every request event carries.
const Request = struct {
    /// The JSON-RPC ID as written, for the reply.
    id: []const u8,
    /// The request ID events and answers use.
    key: []const u8,
    /// The subagent thread that asked, or "".
    subagent: []const u8,
    /// The provider data kept with the pending request.
    native: []const u8,
    raw: RawJson,
};

/// The provider data stored with a pending request: the native request ID,
/// method, and params.
fn nativeRequest(a: Allocator, id: []const u8, method: []const u8, params: Value) ![]const u8 {
    return std.fmt.allocPrint(a, "{{\"id\":{s},\"method\":{s},\"params\":{s}}}", .{ id, try agent.json.quoteString(a, method), try agent.json.encodeValue(a, params) });
}

/// Answers or defers an app-server request for this thread or one of its
/// subagent threads.
pub fn serverRequest(codex: *CodexSession, a: Allocator, id: []const u8, method: []const u8, params: JsonView, line: []const u8) void {
    serverRequestInner(codex, a, id, method, params, line) catch |e| {
        log.warn("codex: handling request {s} failed: {t}", .{ method, e });
        var diag: Diagnostics = .{};
        codex.client.replyError(a, id, -32603, @errorName(e), &diag) catch |reply_error|
            log.err("codex: refusing request {s} failed: {s}", .{ method, Diagnostics.of(&diag, reply_error).message });
    };
}

fn serverRequestInner(codex: *CodexSession, a: Allocator, id: []const u8, method: []const u8, params: JsonView, line: []const u8) !void {
    const known = std.meta.stringToEnum(RequestMethod, method) orelse
        return codex.client.replyError(a, id, -32601, try std.fmt.allocPrint(a, "unsupported request: {s}", .{method}), null);
    const thread = decode.threadOf(params);
    const request: Request = .{
        .id = id,
        .key = decode.requestKey(a, JsonView.parseBytes(a, id)),
        .subagent = if (thread.len > 0 and !codex.threadId().eql(thread)) thread else "",
        .native = try nativeRequest(a, id, method, params.v orelse .null),
        .raw = RawJson.of(line),
    };
    switch (known) {
        .@"item/tool/call" => try toolCall(codex, a, request, params),
        .@"item/commandExecution/requestApproval" => try commandApproval(codex, a, request, params),
        .@"item/fileChange/requestApproval" => try fileChangeApproval(codex, a, request, params),
        .@"item/permissions/requestApproval" => codex.rt().request(.{ .permission_request = .{
            .id = request.key,
            .kind = .grant,
            .tool_name = "permissions",
            .tool_call_id = params.str("itemId"),
            .input = try params.get("permissions").raw(a),
            .cwd = params.str("cwd"),
            .reason = params.str("reason"),
            .title = "Grant additional permissions",
            .decisions = &.{ .allow, .allow_session, .deny },
            .subagent_id = request.subagent,
        } }, request.raw, request.native),
        .@"item/tool/requestUserInput" => try userInputRequest(codex, a, request, params),
        .@"mcpServer/elicitation/request" => codex.rt().request(.{ .elicitation_request = .{
            .id = request.key,
            .server = params.str("serverName"),
            .message = providerkit.firstOf(&.{ params.str("message"), params.str("description") }),
            .mode = providerkit.firstOf(&.{ params.str("mode"), "form" }),
            .schema = try params.get("requestedSchema").raw(a),
            .url = params.str("url"),
            .elicitation_id = params.str("elicitationId"),
            .mcp_meta = try params.get("_meta").raw(a),
            .subagent_id = request.subagent,
        } }, request.raw, request.native),
        .applyPatchApproval, .execCommandApproval => try legacyApproval(codex, a, request, known == .applyPatchApproval, method, params),
    }
}

/// Every decision, for requests that do not list the ones they accept.
const all_decisions: []const types.PermissionDecision = &.{ .allow, .allow_session, .deny, .cancel };

/// Runs a host tool call as a session task, because the host's handler may
/// block.
fn toolCall(codex: *CodexSession, a: Allocator, request: Request, params: JsonView) !void {
    const tool = params.str("tool");
    const known = for (codex.cfg.tools) |defined| {
        if (eql(defined.name, tool)) break true;
    } else false;
    if (!known) {
        const text = try std.fmt.allocPrint(a, "unknown tool {s}", .{tool});
        return codex.client.reply(a, request.id, try toolResult(a, try types.ToolResult.err(a, text)), null);
    }
    const job = try ToolJob.create(codex.env.gpa, request.id, params.str("callId"), tool, try params.get("arguments").raw(a));
    codex.rt().spawn(ToolJob.run, .{ job, codex }) catch |e| {
        job.destroy();
        return e;
    };
}

fn commandApproval(codex: *CodexSession, a: Allocator, request: Request, params: JsonView) !void {
    const command = params.str("command");
    codex.rt().request(.{ .permission_request = .{
        .id = request.key,
        .kind = .command,
        .tool_name = "commandExecution",
        .tool_call_id = params.str("itemId"),
        .input = try params.raw(a),
        .command = command,
        .cwd = params.str("cwd"),
        .reason = params.str("reason"),
        .title = try std.fmt.allocPrint(a, "Run {s}", .{command}),
        .decisions = try commandDecisions(a, params.get("availableDecisions")),
        .subagent_id = request.subagent,
    } }, request.raw, request.native);
}

fn fileChangeApproval(codex: *CodexSession, a: Allocator, request: Request, params: JsonView) !void {
    const item_id = params.str("itemId");
    var changes: []const types.FileChange = &.{};
    lock(codex);
    if (codex.translator.items.get(.of(item_id))) |stored| if (stored.value.tool) |call| {
        changes = agent.clone([]const types.FileChange, a, call.changes) catch &.{};
    };
    if (request.subagent.len > 0) if (codex.translator.child_running.get(.of(request.subagent))) |list| for (list.items) |stored| {
        if (eql(stored.value.id, item_id)) if (stored.value.tool) |call| {
            changes = agent.clone([]const types.FileChange, a, call.changes) catch &.{};
        };
    };
    unlock(codex);
    codex.rt().request(.{ .permission_request = .{
        .id = request.key,
        .kind = .file_change,
        .tool_name = "fileChange",
        .tool_call_id = item_id,
        .input = try params.raw(a),
        .reason = params.str("reason"),
        .title = "Apply file changes",
        .changes = changes,
        .paths = if (params.has("grantRoot")) try a.dupe([]const u8, &.{params.str("grantRoot")}) else &.{},
        .decisions = all_decisions,
        .subagent_id = request.subagent,
    } }, request.raw, request.native);
}

fn userInputRequest(codex: *CodexSession, a: Allocator, request: Request, params: JsonView) !void {
    var questions: std.ArrayList(types.Question) = .empty;
    for (params.get("questions").items()) |question_value| {
        const question = JsonView.of(question_value);
        var choices: std.ArrayList(types.QuestionOption) = .empty;
        for (question.get("options").items()) |option_value| {
            const option = JsonView.of(option_value);
            try choices.append(a, .{ .label = option.str("label"), .description = option.str("description") });
        }
        try questions.append(a, .{
            .id = question.str("id"),
            .header = question.str("header"),
            .text = question.str("question"),
            .options = choices.items,
            .allow_other = question.flag("isOther"),
            .secret = question.flag("isSecret"),
        });
    }
    codex.rt().request(.{ .user_input_request = .{ .id = request.key, .questions = questions.items, .tool_call_id = params.str("itemId"), .subagent_id = request.subagent } }, request.raw, request.native);
}

/// applyPatchApproval and execCommandApproval, the older approval requests.
fn legacyApproval(codex: *CodexSession, a: Allocator, request: Request, patch: bool, method: []const u8, params: JsonView) !void {
    const command = try std.mem.join(a, " ", try params.get("command").strings(a));
    codex.rt().request(.{ .permission_request = .{
        .id = request.key,
        .kind = if (patch) .file_change else .command,
        .tool_name = method,
        .tool_call_id = params.str("callId"),
        .input = try params.raw(a),
        .command = command,
        .cwd = params.str("cwd"),
        .reason = params.str("reason"),
        .paths = if (params.has("grantRoot")) try a.dupe([]const u8, &.{params.str("grantRoot")}) else &.{},
        .decisions = all_decisions,
        .subagent_id = request.subagent,
    } }, request.raw, request.native);
}

/// A host tool call, run as a session task because the host's handler may
/// block.
const ToolJob = struct {
    arena: std.heap.ArenaAllocator,
    id: []const u8,
    call_id: []const u8,
    tool: []const u8,
    arguments: ?RawJson,

    fn create(gpa: Allocator, id: []const u8, call_id: []const u8, tool: []const u8, arguments: ?RawJson) !*ToolJob {
        const job = try gpa.create(ToolJob);
        job.arena = .init(gpa);
        errdefer job.destroy();
        const a = job.arena.allocator();
        job.id = try a.dupe(u8, id);
        job.call_id = try a.dupe(u8, call_id);
        job.tool = try a.dupe(u8, tool);
        job.arguments = if (arguments) |raw| RawJson.of(try a.dupe(u8, raw.bytes)) else null;
        return job;
    }

    fn destroy(job: *ToolJob) void {
        const gpa = job.arena.child_allocator;
        job.arena.deinit();
        gpa.destroy(job);
    }

    fn run(job: *ToolJob, codex: *CodexSession) void {
        defer job.destroy();
        const a = job.arena.allocator();
        const session_id = a.dupe(u8, codex.threadId().slice()) catch "";
        const result = codex.session.callTool(a, .{ .id = job.call_id, .name = job.tool, .arguments = job.arguments, .session_id = session_id });
        var diag: Diagnostics = .{};
        const value = toolResult(a, result) catch {
            codex.client.replyError(a, job.id, -32603, "out of memory", &diag) catch |e|
                log.err("codex: answering tool call {s} failed: {s}", .{ job.call_id, Diagnostics.of(&diag, e).message });
            return;
        };
        codex.client.reply(a, job.id, value, &diag) catch |e|
            log.err("codex: answering tool call {s} failed: {s}", .{ job.call_id, Diagnostics.of(&diag, e).message });
    }
};

fn toolResult(a: Allocator, result: types.ToolResult) !Value {
    var items: std.json.Array = .init(a);
    for (result.content) |content| try items.append(switch (content.type) {
        .image => try agent.json.toValue(a, .{
            .type = "inputImage",
            .imageUrl = try std.fmt.allocPrint(a, "data:{s};base64,{s}", .{ content.media_type, content.data }),
        }),
        .text => try agent.json.toValue(a, .{ .type = "inputText", .text = content.text }),
    });
    if (items.items.len == 0) try items.append(try agent.json.toValue(a, .{ .type = "inputText", .text = "" }));
    return agent.json.toValue(a, .{ .contentItems = Value{ .array = items }, .success = !result.is_error });
}

/// The name of a string or single-key object decision.
fn decisionName(value: Value) []const u8 {
    return switch (value) {
        .string => |s| s,
        .object => |object| if (object.count() > 0) object.keys()[0] else "",
        else => "",
    };
}

/// The command decisions Codex offers.
const CommandDecision = enum { accept, acceptForSession, acceptWithExecpolicyAmendment, decline, cancel };

fn commandDecisions(a: Allocator, available: JsonView) ![]const types.PermissionDecision {
    if (available.len() == 0) return all_decisions;
    var out: std.ArrayList(types.PermissionDecision) = .empty;
    for (available.items()) |value| {
        const decision = std.meta.stringToEnum(CommandDecision, decisionName(value)) orelse continue;
        try out.append(a, switch (decision) {
            .accept => .allow,
            .acceptForSession => .allow_session,
            .acceptWithExecpolicyAmendment => .allow_always,
            .decline => .deny,
            .cancel => .cancel,
        });
    }
    return out.items;
}

/// Whether the server offered decision `want`; a request that lists none
/// offers all.
fn offered(available: JsonView, want: []const u8) bool {
    if (available.len() == 0) return true;
    for (available.items()) |value| if (eql(decisionName(value), want)) return true;
    return false;
}

/// The native decision for a uniform answer, falling back to the closest
/// decision the server offered.
fn commandDecision(a: Allocator, response: types.PermissionResponse, available: JsonView, amendment: JsonView) !Value {
    switch (response.decision) {
        .allow_always => if (offered(available, "acceptWithExecpolicyAmendment") and !amendment.isNull()) {
            return agent.json.toValue(a, .{ .acceptWithExecpolicyAmendment = .{ .execpolicy_amendment = amendment.v.? } });
        } else return .{ .string = if (offered(available, "acceptForSession")) "acceptForSession" else "accept" },
        .allow_session => return .{ .string = if (offered(available, "acceptForSession")) "acceptForSession" else "accept" },
        .deny => return .{ .string = if (offered(available, "decline")) "decline" else "cancel" },
        .cancel => return .{ .string = "cancel" },
        .allow => return .{ .string = "accept" },
    }
}

/// Grants everything requested, dropping null members the response type
/// does not accept.
fn grantedPermissions(a: Allocator, requested: JsonView) !Value {
    var out: ObjectBuilder = .init(a);
    if (requested.object()) |object| {
        for (object.keys(), object.values()) |key, value| if (value != .null) try out.put(key, value);
    }
    return out.value();
}

/// Answers a pending request natively.
pub fn respond(session: *Session, pending: *const Session.Pending, response: types.Response, diag: ?*Diagnostics) Error!void {
    const codex = CodexSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(codex.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const native = JsonView.parseBytes(a, pending.native);
    const method = native.str("method");
    const params = native.get("params");
    const id = if (native.get("id").v) |value| try agent.json.encodeValue(a, value) else "null";

    if (eql(method, "async")) return answerAsync(codex, a, pending, params, response.user_input, diag);

    const result: Value = switch (response) {
        .permission => |answer| try permissionResult(a, method, params, answer, diag),
        .user_input => |answer| blk: {
            var answers: ObjectBuilder = .init(a);
            if (!answer.cancelled) if (answer.answers) |map| for (map.map.keys(), map.map.values()) |question_id, values| {
                try answers.put(question_id, try agent.json.toValue(a, .{ .answers = values }));
            };
            break :blk try agent.json.toValue(a, .{ .answers = answers.value() });
        },
        .elicitation => |answer| blk: {
            var out: ObjectBuilder = .init(a);
            try out.str("action", @tagName(answer.action));
            try out.put("content", agent.json.valueOf(a, answer.content) orelse .null);
            try out.put("_meta", agent.json.valueOf(a, answer.mcp_meta) orelse .null);
            break :blk out.value();
        },
    };
    return codex.client.reply(a, id, result, diag);
}

fn permissionResult(a: Allocator, method: []const u8, params: JsonView, answer: types.PermissionResponse, diag: ?*Diagnostics) Error!Value {
    if (answer.updated_input != null and answer.native == null and (answer.decision == .allow or answer.decision == .allow_session or answer.decision == .allow_always))
        return errors.unsupported(diag, @import("root.zig").name, "permission response updated_input");
    const native = agent.json.valueOf(a, answer.native);
    var out: ObjectBuilder = .init(a);
    // Requests this library does not know are answered like the older
    // approval requests.
    switch (std.meta.stringToEnum(RequestMethod, method) orelse .execCommandApproval) {
        .@"item/commandExecution/requestApproval" => {
            try out.put("decision", native orelse try commandDecision(a, answer, params.get("availableDecisions"), params.get("proposedExecpolicyAmendment")));
        },
        .@"item/fileChange/requestApproval" => try out.put("decision", native orelse .{ .string = switch (answer.decision) {
            .allow => "accept",
            .allow_session, .allow_always => "acceptForSession",
            .deny => "decline",
            .cancel => "cancel",
        } }),
        .@"item/permissions/requestApproval" => {
            if (native) |value| return value;
            const granted = answer.decision != .deny and answer.decision != .cancel;
            try out.put("permissions", if (granted) try grantedPermissions(a, params.get("permissions")) else .{ .object = .empty });
            try out.str("scope", if (answer.decision == .allow_session or answer.decision == .allow_always) "session" else "turn");
        },
        else => {
            const decision: Value = native orelse switch (answer.decision) {
                .allow => .{ .string = "approved" },
                .allow_session, .allow_always => .{ .string = "approved_for_session" },
                .cancel => .{ .string = "abort" },
                .deny => try agent.json.toValue(a, .{ .denied = .{ .rejection = providerkit.firstOf(&.{ answer.message, "The user denied this action." }) } }),
            };
            try out.put("decision", decision);
        },
    }
    return out.value();
}

/// Sends the answer to an asynchronous question: steered into the running
/// turn when there is one, otherwise, or when Codex refuses the steer, as
/// the next turn.
fn answerAsync(codex: *CodexSession, a: Allocator, pending: *const Session.Pending, params: JsonView, response: types.UserInputResponse, diag: ?*Diagnostics) Error!void {
    lock(codex);
    _ = codex.translator.async_turns.orderedRemove(pending.id);
    unlock(codex);
    if (response.cancelled) return;
    const titles = params.get("questions").items();
    var lines: std.ArrayList([]const u8) = .empty;
    for (titles, 0..) |title, i| {
        const key = try std.fmt.allocPrint(a, "{d}", .{i});
        const values = if (response.answers) |map| map.map.get(key) orelse &.{} else &.{};
        const answer = try std.mem.join(a, ", ", values);
        if (answer.len == 0) continue;
        try lines.append(a, if (titles.len == 1) answer else try std.fmt.allocPrint(a, "{s}\n{s}", .{ JsonView.of(title).string(), answer }));
    }
    if (lines.items.len == 0) return;
    const text = try std.mem.join(a, "\n\n", lines.items);
    const turn_id = codex.env.uuid();
    const active, const starting = codex.rt().runningTurn();
    if (!active.isEmpty() and !starting) {
        codex.submit(.{ .text = text, .delivery = .steer }, turn_id.slice(), diag) catch |e| {
            if (e != error.SteerRejected) return e;
            return codex.submit(.{ .text = text }, codex.env.uuid().slice(), diag);
        };
        return;
    }
    return codex.submit(.{ .text = text }, turn_id.slice(), diag);
}

test "retry progress" {
    try std.testing.expectEqual([2]u32{ 2, 5 }, retryProgress("Reconnecting... 2/5").?);
    try std.testing.expectEqual([2]u32{ 3, 10 }, retryProgress("attempt 3 / 10 failed").?);
    try std.testing.expect(retryProgress("v1/2x") == null);
    try std.testing.expect(retryProgress("none") == null);
}

test "answer wire shapes" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const result = try toolResult(a, .{ .content = &.{ .{ .text = "t" }, .{ .type = .image, .media_type = "image/png", .data = "QQ==" } }, .is_error = true });
    try std.testing.expectEqualStrings(
        \\{"contentItems":[{"type":"inputText","text":"t"},{"type":"inputImage","imageUrl":"data:image/png;base64,QQ=="}],"success":false}
    , try agent.json.encodeValue(a, result));
    const empty = try toolResult(a, .{});
    try std.testing.expectEqualStrings(
        \\{"contentItems":[{"type":"inputText","text":""}],"success":true}
    , try agent.json.encodeValue(a, empty));
    const denied = try permissionResult(a, "execCommandApproval", JsonView.empty, .{ .decision = .deny }, null);
    try std.testing.expectEqualStrings(
        \\{"decision":{"denied":{"rejection":"The user denied this action."}}}
    , try agent.json.encodeValue(a, denied));
    const params = JsonView.parseBytes(a,
        \\{"proposedExecpolicyAmendment":["git","status"]}
    );
    const amended = try permissionResult(a, "item/commandExecution/requestApproval", params, .{ .decision = .allow_always }, null);
    try std.testing.expectEqualStrings(
        \\{"decision":{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["git","status"]}}}
    , try agent.json.encodeValue(a, amended));
    try std.testing.expectEqualStrings(
        \\{"id":7,"method":"a\"b","params":{"x":1}}
    , try nativeRequest(a, "7", "a\"b", agent.json.parseValue(a, "{\"x\":1}").?));
}
