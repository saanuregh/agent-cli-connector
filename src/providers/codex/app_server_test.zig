//! Behavior tests against a scripted app-server: requests the library does
//! not know, close on a shared app-server, rewind of a legacy thread, turn
//! parameters, provider calls, and launch wrappers. The script logs every
//! line it reads to wire.log and its arguments to argv.log.
const std = @import("std");
const agent = @import("agent");
const root = @import("root.zig");
const types = agent.types;

/// Answers every request; a turn whose text starts with "ask" sends the
/// server request custom/ping and completes once it is answered, one
/// starting with "bg" is followed by a turn Codex starts itself, and one
/// starting with "usage" reports a new context window.
const script =
    \\#!/bin/sh
    \\dir=$(dirname "$0")
    \\printf '%s\n' "$*" >> "$dir/argv.log"
    \\turn=0
    \\done_turn() { printf '%s\n' '{"method":"turn/completed","params":{"threadId":"th1","turn":{"id":"'"$1"'","status":"completed","items":[]}}}'; }
    \\while IFS= read -r l; do
    \\  printf '%s\n' "$l" >> "$dir/wire.log"
    \\  case "$l" in
    \\    '{"id":"srv'*|'{"error":'*) done_turn "tu$turn"; continue;;
    \\  esac
    \\  id=$(printf '%s' "$l" | sed -n 's/^{"id":\([0-9]*\),.*/\1/p')
    \\  method=$(printf '%s' "$l" | sed -n 's/^{"id":[0-9]*,"method":"\([^"]*\)".*/\1/p')
    \\  [ -z "$id" ] && continue
    \\  case "$method" in
    \\    initialize) r='{"userAgent":"codex_cli_rs/0.163.0 (Linux)"}';;
    \\    thread/start|thread/resume) r='{"thread":{"id":"th1"},"model":"m1","cwd":"/w"}';;
    \\    config/read) r='{"config":{"model_context_window":272000}}';;
    \\    skills/list) r='{"data":[]}';;
    \\    thread/read) r='{"thread":{"id":"th1","historyMode":"legacy"}}';;
    \\    turn/start) turn=$((turn+1)); r='{"turn":{"id":"tu'"$turn"'","status":"inProgress","items":[]}}';;
    \\    *) r='{"method":"'"$method"'"}';;
    \\  esac
    \\  printf '%s\n' "{\"id\":$id,\"result\":$r}"
    \\  [ "$method" = turn/start ] || continue
    \\  printf '%s\n' '{"method":"turn/started","params":{"threadId":"th1","turn":{"id":"tu'"$turn"'","status":"inProgress","items":[]}}}'
    \\  case "$l" in
    \\    *'"text":"ask'*) printf '%s\n' '{"id":"srv'"$turn"'","method":"custom/ping","params":{"threadId":"th1","n":'"$turn"'}}';;
    \\    *'"text":"bg'*)
    \\      done_turn "tu$turn"
    \\      printf '%s\n' '{"method":"turn/started","params":{"threadId":"th1","turn":{"id":"bg1","status":"inProgress","items":[]}}}'
    \\      done_turn bg1;;
    \\    *'"text":"usage'*)
    \\      printf '%s\n' '{"method":"thread/tokenUsage/updated","params":{"threadId":"th1","turnId":"tu'"$turn"'","tokenUsage":{"total":{"inputTokens":5,"outputTokens":1},"last":{"inputTokens":5,"outputTokens":1},"modelContextWindow":258400}}}'
    \\      done_turn "tu$turn";;
    \\    *) done_turn "tu$turn";;
    \\  esac
    \\done
;

/// Logs its arguments without the first, which it drops, then runs the
/// rest.
const wrapper =
    \\#!/bin/sh
    \\printf 'wrapped %s\n' "$*" >> "$(dirname "$0")/argv.log"
    \\shift
    \\exec "$@"
;

const timeout: std.Io.Timeout = .{ .duration = .{ .raw = .fromSeconds(10), .clock = .awake } };

const Fixture = struct {
    tmp: std.testing.TmpDir,
    arena: std.heap.ArenaAllocator,
    env: agent.Env,
    path: []const u8,
    wrapper_path: []const u8,

    fn init() !Fixture {
        const io = std.testing.io;
        var fixture: Fixture = .{
            .tmp = std.testing.tmpDir(.{}),
            .arena = .init(std.testing.allocator),
            .env = .{ .io = io, .gpa = std.testing.allocator, .environ = std.testing.environ },
            .path = "",
            .wrapper_path = "",
        };
        errdefer fixture.deinit();
        const a = fixture.arena.allocator();
        try fixture.tmp.dir.writeFile(io, .{ .sub_path = "codex.sh", .data = script, .flags = .{ .permissions = .executable_file } });
        try fixture.tmp.dir.writeFile(io, .{ .sub_path = "wrap.sh", .data = wrapper, .flags = .{ .permissions = .executable_file } });
        fixture.path = try fixture.tmp.dir.realPathFileAlloc(io, "codex.sh", a);
        fixture.wrapper_path = try fixture.tmp.dir.realPathFileAlloc(io, "wrap.sh", a);
        return fixture;
    }

    fn deinit(fixture: *Fixture) void {
        fixture.arena.deinit();
        fixture.tmp.cleanup();
    }

    /// The lines the script logged to `name`.
    fn log(fixture: *Fixture, name: []const u8) ![]const u8 {
        return fixture.tmp.dir.readFileAlloc(std.testing.io, name, fixture.arena.allocator(), .limited(1 << 20));
    }

    /// The logged lines holding `needle`.
    fn linesWith(fixture: *Fixture, name: []const u8, needle: []const u8) ![]const []const u8 {
        const a = fixture.arena.allocator();
        var out: std.ArrayList([]const u8) = .empty;
        var lines = std.mem.splitScalar(u8, try fixture.log(name), '\n');
        while (lines.next()) |line| if (std.mem.indexOf(u8, line, needle) != null) try out.append(a, line);
        return out.items;
    }
};

/// Reads events until one of kind `tag`, which the caller destroys.
fn waitFor(session: *agent.Session, tag: std.meta.Tag(types.Event.Payload)) !*agent.runtime.OwnedEvent {
    while (true) {
        const owned = try session.nextEvent(timeout);
        if (owned.event.kind() == tag) return owned;
        owned.destroy();
    }
}

test "an unknown server request becomes native_request and is answered with a result or an error" {
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    var fixture: Fixture = try .init();
    defer fixture.deinit();
    const a = fixture.arena.allocator();
    const provider = try root.create(fixture.env, .{ .path = fixture.path });
    defer provider.deinit();
    try std.testing.expect(provider.capabilities().native_requests);
    const session = try provider.start(.{}, null);
    defer session.deinit();

    const first = try session.send(.{ .text = "ask once" }, null);
    {
        const owned = try waitFor(session, .native_request);
        defer owned.destroy();
        const request = owned.event.payload.native_request;
        try std.testing.expectEqualStrings("custom/ping", request.method);
        try std.testing.expectEqualStrings("{\"threadId\":\"th1\",\"n\":1}", request.params.?.bytes);
        try std.testing.expectEqualStrings("", request.subagent_id);
        try session.respond(request.id, .{ .native = .{ .result = .of("{\"ok\":1}") } }, null);
    }
    try std.testing.expectEqual(types.TurnStatus.completed, (try first.wait(a, timeout, null)).status);

    const second = try session.send(.{ .text = "ask again" }, null);
    {
        const owned = try waitFor(session, .native_request);
        defer owned.destroy();
        try session.respond(owned.event.payload.native_request.id, .{ .native = .{ .@"error" = .{ .code = 7, .message = "nope" } } }, null);
    }
    try std.testing.expectEqual(types.TurnStatus.completed, (try second.wait(a, timeout, null)).status);

    try std.testing.expectEqualStrings("{\"id\":\"srv1\",\"result\":{\"ok\":1}}", (try fixture.linesWith("wire.log", "\"srv1\""))[0]);
    try std.testing.expectEqualStrings("{\"error\":{\"code\":7,\"message\":\"nope\"},\"id\":\"srv2\"}", (try fixture.linesWith("wire.log", "\"srv2\""))[0]);
}

test "closing on a shared app-server only unsubscribes and withdraws pending requests locally" {
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    var fixture: Fixture = try .init();
    defer fixture.deinit();
    const provider = try root.create(fixture.env, .{ .path = fixture.path, .shared_server = true });
    defer provider.deinit();
    const session = try provider.start(.{}, null);
    defer session.deinit();

    _ = try session.send(.{ .text = "ask and wait" }, null);
    const request_id = blk: {
        const owned = try waitFor(session, .native_request);
        defer owned.destroy();
        break :blk try fixture.arena.allocator().dupe(u8, owned.event.payload.native_request.id);
    };
    session.close();
    const owned = try waitFor(session, .request_resolved);
    defer owned.destroy();
    const resolved = owned.event.payload.request_resolved;
    try std.testing.expectEqualStrings(request_id, resolved.request_id);
    try std.testing.expectEqual(types.RequestKind.native, resolved.kind);
    try std.testing.expectEqual(types.RequestOutcome.withdrawn, resolved.outcome);

    try std.testing.expectEqual(@as(usize, 1), (try fixture.linesWith("wire.log", "\"thread/unsubscribe\"")).len);
    try std.testing.expectEqual(@as(usize, 0), (try fixture.linesWith("wire.log", "\"turn/interrupt\"")).len);
    try std.testing.expectEqual(@as(usize, 0), (try fixture.linesWith("wire.log", "\"srv1\"")).len);
}

test "rewinding a thread without paginated history fails" {
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    var fixture: Fixture = try .init();
    defer fixture.deinit();
    const a = fixture.arena.allocator();
    const provider = try root.create(fixture.env, .{ .path = fixture.path });
    defer provider.deinit();
    try std.testing.expect(!provider.capabilities().rewind_changes_session_id);
    const session = try provider.start(.{}, null);
    defer session.deinit();
    const turn = try session.send(.{ .text = "hello" }, null);
    _ = try turn.wait(a, timeout, null);

    var diag: agent.Diagnostics = .{};
    try std.testing.expectError(error.Unsupported, session.rewind(turn.id.slice(), a, &diag));
    try std.testing.expectEqualStrings("codex: rewind needs a paginated thread", diag.info.?.message);
    try std.testing.expectEqual(@as(usize, 0), (try fixture.linesWith("wire.log", "\"thread/fork\"")).len);
    try std.testing.expectEqual(@as(usize, 0), (try fixture.linesWith("wire.log", "\"thread/revert\"")).len);
}

test "turns ask for detailed reasoning summaries unless an option sets one" {
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    var fixture: Fixture = try .init();
    defer fixture.deinit();
    const a = fixture.arena.allocator();
    const provider = try root.create(fixture.env, .{ .path = fixture.path });
    defer provider.deinit();
    {
        const session = try provider.start(.{}, null);
        defer session.deinit();
        _ = try session.run(.{ .text = "one" }, a, timeout, null);
        _ = try session.run(.{ .text = "two", .provider_options = .of("{\"reasoning_summary\":\"concise\"}") }, a, timeout, null);
    }
    {
        const session = try provider.start(.{ .config = .{ .provider_options = .of("{\"reasoning_summary\":\"auto\"}") } }, null);
        defer session.deinit();
        _ = try session.run(.{ .text = "three" }, a, timeout, null);
    }
    const starts = try fixture.linesWith("wire.log", "\"turn/start\"");
    try std.testing.expectEqual(@as(usize, 3), starts.len);
    const want = [_][]const u8{ "\"summary\":\"detailed\"", "\"summary\":\"concise\"", "\"summary\":\"auto\"" };
    for (starts, want) |line, summary| {
        try std.testing.expect(std.mem.indexOf(u8, line, summary) != null);
        try std.testing.expectEqual(@as(usize, 1), std.mem.count(u8, line, "\"summary\""));
    }
}

test "turn_started carries the Codex turn ID, the client ID, and the origin of turns Codex starts" {
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    var fixture: Fixture = try .init();
    defer fixture.deinit();
    const a = fixture.arena.allocator();
    const provider = try root.create(fixture.env, .{ .path = fixture.path });
    defer provider.deinit();
    const session = try provider.start(.{}, null);
    defer session.deinit();
    try std.testing.expectEqual(@as(i64, 272000), (try session.info(a)).context_window);

    const turn = try session.send(.{ .text = "bg please", .client_id = "c1" }, null);
    {
        const owned = try waitFor(session, .turn_started);
        defer owned.destroy();
        const started = owned.event.payload.turn_started;
        try std.testing.expectEqualStrings(turn.id.slice(), owned.event.turn_id);
        try std.testing.expectEqualStrings("tu1", started.native_turn_id);
        try std.testing.expectEqualStrings("c1", started.client_id);
        try std.testing.expectEqualStrings("", started.origin);
    }
    {
        const owned = try waitFor(session, .turn_started);
        defer owned.destroy();
        const started = owned.event.payload.turn_started;
        try std.testing.expectEqualStrings("bg1", started.native_turn_id);
        try std.testing.expectEqualStrings("codex", started.origin);
        try std.testing.expectEqualStrings("", started.client_id);
    }

    _ = try session.run(.{ .text = "usage" }, a, timeout, null);
    try std.testing.expectEqual(@as(i64, 258400), (try session.info(a)).context_window);
}

test "provider call reaches a short-lived and a shared app-server" {
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    var fixture: Fixture = try .init();
    defer fixture.deinit();
    const a = fixture.arena.allocator();
    for ([_]bool{ false, true }) |shared| {
        const provider = try root.create(fixture.env, .{ .path = fixture.path, .shared_server = shared });
        defer provider.deinit();
        try std.testing.expect(provider.capabilities().provider_call);
        const result = try provider.call("account/read", .of("{}"), a, null);
        try std.testing.expectEqualStrings("{\"method\":\"account/read\"}", result.?.bytes);
    }
    const calls = try fixture.linesWith("wire.log", "\"account/read\"");
    try std.testing.expectEqual(@as(usize, 2), calls.len);
    try std.testing.expectEqualStrings("{\"id\":2,\"method\":\"account/read\",\"params\":{}}", calls[0]);
}

test "command prefixes wrap the app-server; a shared app-server ignores the session's" {
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    var fixture: Fixture = try .init();
    defer fixture.deinit();
    const a = fixture.arena.allocator();
    const prefix: []const []const u8 = &.{ fixture.wrapper_path, "provider-level" };
    {
        const provider = try root.create(fixture.env, .{ .path = fixture.path, .command_prefix = prefix });
        defer provider.deinit();
        _ = try provider.call("account/read", null, a, null);
        const session = try provider.start(.{ .config = .{ .command_prefix = &.{ fixture.wrapper_path, "session-level" } } }, null);
        session.deinit();
    }
    {
        const provider = try root.create(fixture.env, .{ .path = fixture.path, .shared_server = true });
        defer provider.deinit();
        const session = try provider.start(.{ .config = .{ .command_prefix = &.{ fixture.wrapper_path, "ignored" } } }, null);
        defer session.deinit();
        const owned = try waitFor(session, .notice);
        defer owned.destroy();
        try std.testing.expectEqualStrings("config_ignored", owned.event.payload.notice.code);
        try std.testing.expect(std.mem.indexOf(u8, owned.event.payload.notice.message, "command_prefix on a shared app-server") != null);
    }
    const wrapped = try fixture.linesWith("argv.log", "wrapped");
    try std.testing.expectEqual(@as(usize, 2), wrapped.len);
    try std.testing.expectEqualStrings(try std.fmt.allocPrint(a, "wrapped provider-level {s} app-server", .{fixture.path}), wrapped[0]);
    try std.testing.expectEqualStrings(try std.fmt.allocPrint(a, "wrapped session-level {s} app-server", .{fixture.path}), wrapped[1]);
    try std.testing.expectEqual(@as(usize, 0), (try fixture.linesWith("argv.log", "ignored")).len);
}
