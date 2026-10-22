//! Hand checks against the installed codex CLI, skipped unless
//! CODEX_LIVE=1. They print what they saw; run with
//! `CODEX_LIVE=1 zig build test-codex -Dproviders=codex --summary all`.
const std = @import("std");
const agent = @import("agent");
const codex = @import("root.zig");
const types = agent.types;

const timeout: std.Io.Timeout = .{ .duration = .{ .raw = .fromSeconds(180), .clock = .awake } };

fn echoTool(_: ?*anyopaque, a: std.mem.Allocator, call: types.ToolCallRequest) types.ToolResult {
    const args = if (call.arguments) |r| r.bytes else "{}";
    return types.ToolResult.text(a, std.fmt.allocPrint(a, "echo:{s}", .{args}) catch "echo") catch .out_of_memory;
}

/// Drains events until turn `turn` completes, answering permission requests
/// with `decision`, and prints a one-line summary per kind of event seen.
fn drive(session: *agent.Session, turn: []const u8, decision: types.PermissionDecision, interrupt_after_delta: bool) !void {
    var counts: std.EnumArray(std.meta.Tag(types.Event.Payload), u32) = .initFill(0);
    var interrupted = false;
    while (session.nextEvent(timeout)) |owned| {
        defer owned.destroy();
        const event = owned.event;
        counts.getPtr(event.kind()).* += 1;
        switch (event.payload) {
            .permission_request => |request| {
                std.debug.print("  permission_request kind={t} command={s} -> {t}\n", .{ request.kind, request.command, decision });
                try session.respond(request.id, .{ .permission = .{ .decision = decision } }, null);
            },
            .item_completed => |completed| if (completed.item.tool) |tool| std.debug.print("  item_completed tool={s} kind={t} status={t}\n", .{ tool.name, tool.kind, completed.item.status }),
            .notice => |notice| std.debug.print("  notice {t} {s}: {s}\n", .{ notice.level, notice.code, notice.message }),
            .text_delta => if (interrupt_after_delta and !interrupted) {
                interrupted = true;
                try session.interrupt(null);
                std.debug.print("  interrupt sent\n", .{});
            },
            .native => |native| std.debug.print("  native {s}\n", .{native.name}),
            // One turn runs at a time here. The event carries Codex's turn
            // ID, not the one `send` returned.
            .turn_completed => |completed| {
                std.debug.print("  turn_completed {s} (sent as {s})\n", .{ completed.result.turn_id, turn });
                break;
            },
            else => {},
        }
    } else |_| {}
    var entries = counts.iterator();
    std.debug.print("  events:", .{});
    while (entries.next()) |entry| if (entry.value.* > 0) std.debug.print(" {t}={d}", .{ entry.key, entry.value.* });
    std.debug.print("\n", .{});
}

test "live codex" {
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    if (std.testing.environ.getPosix("CODEX_LIVE") == null) return error.SkipZigTest;
    const gpa = std.heap.smp_allocator;
    var threaded: std.Io.Threaded = .init(gpa, .{ .environ = std.testing.environ });
    defer threaded.deinit();
    const env: agent.Env = .{ .io = threaded.io(), .gpa = gpa, .environ = std.testing.environ };
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();

    const dir = "/tmp/codex-live-check";
    std.Io.Dir.cwd().createDirPath(env.io, dir) catch {};

    const shared = std.testing.environ.getPosix("CODEX_SHARED") != null;
    const provider = try codex.create(env, .{ .client_name = "agent-cli-connector-check", .shared_server = shared });
    defer provider.deinit();

    const status = try provider.check(a, null);
    std.debug.print("check: installed={} version={s} auth={} method={s}\n", .{ status.installed, status.version, status.auth.?.authenticated, status.auth.?.method });
    const models = try provider.listModels(a, null);
    for (models) |model| if (model.is_default) std.debug.print("models: {d}, default {s} efforts={d}\n", .{ models.len, model.id, model.efforts.len });

    const tools = [_]types.ToolDef{.{ .name = "echo", .description = "Echoes its arguments back.", .input_schema = .of(
        \\{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}
    ) }};
    const session = try provider.start(.{
        .config = .{ .cwd = dir, .effort = "low", .permission_mode = types.permission_mode.default, .tools = &tools, .ephemeral = false },
        .tool_handler = .{ .call = echoTool },
    }, null);
    const info = try session.info(a);
    std.debug.print("started: id={s} model={s} effort={s} mode={s} version={s}\n", .{ info.id, info.model, info.effort, info.permission_mode, info.version });
    const id = try session.id(a);

    std.debug.print("turn 1 (streaming text):\n", .{});
    var turn = try session.send(.{ .text = "Reply with exactly the word: pong" }, null);
    try drive(session, turn.id.slice(), .allow, false);
    var result = try turn.wait(a, timeout, null);
    std.debug.print("  result status={t} text={s} usage in={d} out={d}\n", .{ result.status, result.text, result.usage.input_tokens, result.usage.output_tokens });

    std.debug.print("turn 2 (host tool):\n", .{});
    turn = try session.send(.{ .text = "Call the echo tool with text \"ping\", then reply with exactly what it returned." }, null);
    try drive(session, turn.id.slice(), .allow, false);
    result = try turn.wait(a, timeout, null);
    std.debug.print("  result status={t} text={s}\n", .{ result.status, result.text });

    std.debug.print("turn 3 (permission, native untrusted policy):\n", .{});
    try session.setPermissionMode("untrusted", null);
    turn = try session.send(.{ .text = "Use the shell to run exactly: touch created-by-check.txt   Then reply done." }, null);
    try drive(session, turn.id.slice(), .allow, false);
    result = try turn.wait(a, timeout, null);
    const created = if (std.Io.Dir.cwd().access(env.io, dir ++ "/created-by-check.txt", .{})) true else |_| false;
    std.debug.print("  result status={t} text={s} file_created={}\n", .{ result.status, result.text, created });
    try session.setPermissionMode(types.permission_mode.default, null);

    std.debug.print("turn 4 (interrupt):\n", .{});
    turn = try session.send(.{ .text = "Write the numbers from 1 to 300, one per line, as words." }, null);
    try drive(session, turn.id.slice(), .allow, true);
    result = try turn.wait(a, timeout, null);
    std.debug.print("  result status={t} error={s}\n", .{ result.status, if (result.@"error") |err| err.code else "" });

    std.debug.print("turn 5 (steer):\n", .{});
    turn = try session.send(.{ .text = "Count from 1 to 40 in words, one per line." }, null);
    while (session.nextEvent(timeout)) |owned| {
        defer owned.destroy();
        if (owned.event.kind() == .text_delta) break;
    } else |_| {}
    const steered = session.send(.{ .text = "Actually stop counting and finish with the single word: steered", .delivery = .steer }, null) catch |err| blk: {
        std.debug.print("  steer failed: {t}\n", .{err});
        break :blk turn;
    };
    try drive(session, turn.id.slice(), .allow, false);
    result = try turn.wait(a, timeout, null);
    const steered_result = try steered.wait(a, timeout, null);
    std.debug.print("  result status={t} tail={s} steered turn={s} status={t}\n", .{ result.status, result.text[result.text.len -| 30..], steered_result.turn_id, steered_result.status });
    const last_turn = result.turn_id;

    std.debug.print("fork:\n", .{});
    const forked = try session.fork(.{}, null);
    const fork_info = try forked.info(a);
    turn = try forked.send(.{ .text = "Reply with exactly: forked" }, null);
    try drive(forked, turn.id.slice(), .deny, false);
    result = try turn.wait(a, timeout, null);
    std.debug.print("  fork id={s} model={s} result={s}\n", .{ fork_info.id, fork_info.model, result.text });
    forked.deinit();

    var diag: agent.Diagnostics = .{};
    std.debug.print("compact:\n", .{});
    session.compact("", &diag) catch |err| std.debug.print("  compact failed: {t} {s}\n", .{ err, agent.Diagnostics.of(&diag, err).message });
    std.debug.print("  compact done\n", .{});
    // Codex runs compaction as a turn; rewinding waits until it ended.
    while (!session.rt.idle()) {
        const owned = session.nextEvent(timeout) catch break;
        defer owned.destroy();
        switch (owned.event.payload) {
            .compaction => |compaction| std.debug.print("  compaction done={} trigger={s}\n", .{ compaction.done, compaction.trigger }),
            .turn_completed => |completed| std.debug.print("  compaction turn {s} {t}\n", .{ completed.result.turn_id, completed.result.status }),
            else => {},
        }
    }
    std.debug.print("rewind {s}:\n", .{last_turn});
    session.rewind(last_turn, &diag) catch |err| std.debug.print("  rewind failed: {t} {s}\n", .{ err, agent.Diagnostics.of(&diag, err).message });
    while (session.nextEvent(.{ .duration = .{ .raw = .fromMilliseconds(500), .clock = .awake } })) |owned| {
        defer owned.destroy();
        switch (owned.event.payload) {
            .items_removed => |removed| std.debug.print("  items_removed {d}\n", .{removed.ids.len}),
            .compaction => |compaction| std.debug.print("  compaction done={} trigger={s}\n", .{ compaction.done, compaction.trigger }),
            .session_updated => |updated| std.debug.print("  session_updated id={s}\n", .{updated.info.id}),
            else => {},
        }
    } else |_| {}

    const limits = session.rateLimits(a, null) catch |err| blk: {
        std.debug.print("rate limits failed: {t}\n", .{err});
        break :blk &.{};
    };
    for (limits) |limit| std.debug.print("rate limit {s}/{s}: {d}%\n", .{ limit.limit_id, limit.name, limit.used_percent });
    const params = try std.fmt.allocPrint(a, "{{\"threadId\":\"{s}\",\"includeTurns\":false}}", .{id});
    const read = try session.call("thread/read", .of(params), a, null);
    std.debug.print("call thread/read: {s}...\n", .{read.?.bytes[0..@min(read.?.bytes.len, 100)]});
    try session.setTitle("agent-cli-connector check", null);
    session.deinit();

    std.debug.print("resume:\n", .{});
    const resumed = try provider.start(.{ .config = .{ .cwd = dir, .effort = "low", .resume_id = id } }, null);
    turn = try resumed.send(.{ .text = "What exact word did you reply with in your first answer? Reply with only that word." }, null);
    try drive(resumed, turn.id.slice(), .deny, false);
    result = try turn.wait(a, timeout, null);
    std.debug.print("  resumed result status={t} text={s}\n", .{ result.status, result.text });
    resumed.deinit();

    const sessions = try provider.listSessions(a, .{ .cwd = dir, .limit = 3 }, null);
    for (sessions) |summary| std.debug.print("session {s} title={s} updated={d}\n", .{ summary.id, summary.title, summary.updated_at_ms });
    const items = try provider.readHistory(a, id, .{ .limit = 6 }, null);
    for (items) |item| std.debug.print("history {t} {t} turn={s} time={d} text={s}\n", .{ item.kind, item.status, item.turn_id[0..@min(item.turn_id.len, 8)], item.time_ms, item.text[0..@min(item.text.len, 40)] });
}
