//! Behavior tests against a scripted CLI: handshake, one streamed turn
//! with a tool call, and the events and result it produces; and an
//! interrupt that stays with the turn it was asked for.
const std = @import("std");
const agent = @import("agent");
const root = @import("root.zig");

/// Answers the handshake, then plays one recorded turn for the first user
/// message, echoing its uuid in the result.
const script =
    \\#!/bin/sh
    \\read l
    \\printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"req_1","response":{"commands":[{"name":"review","builtin":false}],"models":[{"value":"default","resolvedModel":"m1"}],"claude_code_version":"9.9.9","account":{"email":"a@example.com"}}}}'
    \\read l
    \\printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"req_2","response":{"applied":{"effort":"medium"}}}}'
    \\read l
    \\printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"req_3","response":{"maxTokens":200000}}}'
    \\read l
    \\uuid=$(echo "$l" | sed 's/.*"uuid":"\([^"]*\)".*/\1/')
    \\printf '%s\n' '{"type":"system","subtype":"init","cwd":"/w","tools":["Bash"],"model":"m1","permissionMode":"acceptEdits","claude_code_version":"9.9.9","mcp_servers":[],"session_id":"'"$SID"'"}'
    \\printf '%s\n' '{"type":"command_lifecycle","command_uuid":"'"$uuid"'","state":"started"}'
    \\printf '%s\n' '{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":1}}}}'
    \\printf '%s\n' '{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}'
    \\printf '%s\n' '{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}}'
    \\printf '%s\n' '{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}}'
    \\printf '%s\n' '{"type":"assistant","uuid":"w-a1","parent_tool_use_id":null,"message":{"id":"m1","model":"m1","content":[{"type":"text","text":"Hello"}],"stop_reason":null}}'
    \\printf '%s\n' '{"type":"assistant","uuid":"w-a2","parent_tool_use_id":null,"message":{"id":"m1","model":"m1","content":[{"type":"tool_use","id":"tb","name":"Bash","input":{"command":"ls"}}],"stop_reason":"tool_use"}}'
    \\printf '%s\n' '{"type":"user","uuid":"w-u1","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"tb","content":"x"}]},"tool_use_result":{"stdout":"a\nb","stderr":"","interrupted":false,"exitCode":0}}'
    \\printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"Hello","num_turns":2,"duration_ms":5,"total_cost_usd":0.5,"usage":{"input_tokens":10,"output_tokens":3},"modelUsage":{"m1":{"contextWindow":200000}},"user_message_uuids":["'"$uuid"'"]}'
    \\cat >/dev/null
;

test "a scripted turn streams text and completes a shell call" {
    // The scripted CLI is a shell script.
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();
    try tmp.dir.writeFile(io, .{ .sub_path = "claude.sh", .data = script, .flags = .{ .permissions = .executable_file } });
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const path = try tmp.dir.realPathFileAlloc(io, "claude.sh", a);
    const env: agent.Env = .{ .io = io, .gpa = gpa, .environ = std.testing.environ };

    const provider = try root.create(env, .{ .path = path });
    defer provider.deinit();
    const s = try provider.start(.{ .config = .{ .session_id = "0b4c3f0e-1d2a-4b5c-8d9e-0f1a2b3c4d5e" } }, null);
    defer s.deinit();

    const res = try s.run(.{ .text = "hi", .client_id = "c1" }, a, .none, null);
    try std.testing.expectEqual(agent.types.TurnStatus.completed, res.status);
    try std.testing.expectEqualStrings("Hello", res.text);
    try std.testing.expectEqual(@as(f64, 0.5), res.usage.cost_usd);
    try std.testing.expectEqual(@as(i64, 200000), res.usage.context_window);

    var deltas: std.ArrayList(u8) = .empty;
    var bash: ?agent.types.Item = null;
    var saw_user = false;
    var info_mode: []const u8 = "";
    var started_client: []const u8 = "";
    while (s.nextEvent(.{ .duration = .{ .raw = .fromMilliseconds(200), .clock = .awake } })) |owned| {
        defer owned.destroy();
        switch (owned.event.payload) {
            .text_delta => |delta| try deltas.appendSlice(a, delta.delta),
            .item_completed => |completed| {
                if (completed.item.kind == .tool_call) bash = try agent.clone(agent.types.Item, a, completed.item);
                if (completed.item.kind == .user_message) saw_user = true;
            },
            .session_updated => |update| info_mode = try a.dupe(u8, update.info.permission_mode),
            .turn_started => |started| started_client = try a.dupe(u8, started.client_id),
            .turn_completed => break,
            else => {},
        }
    } else |_| {}
    try std.testing.expectEqualStrings("Hello", deltas.items);
    try std.testing.expect(saw_user);
    try std.testing.expectEqualStrings("accept_edits", info_mode);
    try std.testing.expectEqualStrings("c1", started_client);
    try std.testing.expectEqual(@as(i64, 200000), (try s.info(a)).context_window);
    const call = bash.?.tool.?;
    try std.testing.expectEqual(agent.types.ToolKind.shell, call.kind);
    try std.testing.expectEqualStrings("ls", call.command);
    try std.testing.expectEqualStrings("a\nb", call.output);
    try std.testing.expectEqual(@as(?i32, 0), call.exit_code);
}

/// Answers the handshake, cancels the first message when interrupted, then
/// fails the second message's turn without a terminal reason, as a CLI
/// that predates terminal reasons reports an interrupted turn.
const interrupt_script =
    \\#!/bin/sh
    \\read l
    \\printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"req_1","response":{"models":[],"claude_code_version":"9.9.9"}}}'
    \\read l
    \\printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"req_2","response":{}}}'
    \\read l
    \\printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"req_3","response":{"maxTokens":200000}}}'
    \\read l
    \\first=$(echo "$l" | sed 's/.*"uuid":"\([^"]*\)".*/\1/')
    \\read l
    \\printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"req_4","response":{"cancelled":["'"$first"'"]}}}'
    \\read l
    \\second=$(echo "$l" | sed 's/.*"uuid":"\([^"]*\)".*/\1/')
    \\printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","num_turns":1,"duration_ms":5,"usage":{"input_tokens":1,"output_tokens":1},"user_message_uuids":["'"$second"'"]}'
    \\cat >/dev/null
;

test "an interrupt of one turn does not mark the next turn's result" {
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();
    try tmp.dir.writeFile(io, .{ .sub_path = "claude.sh", .data = interrupt_script, .flags = .{ .permissions = .executable_file } });
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const path = try tmp.dir.realPathFileAlloc(io, "claude.sh", a);
    const env: agent.Env = .{ .io = io, .gpa = gpa, .environ = std.testing.environ };

    const provider = try root.create(env, .{ .path = path });
    defer provider.deinit();
    const s = try provider.start(.{ .config = .{ .session_id = "0b4c3f0e-1d2a-4b5c-8d9e-0f1a2b3c4d5e" } }, null);
    defer s.deinit();

    const first = try s.send(.{ .text = "one" }, null);
    try s.interrupt(null);
    try std.testing.expectError(error.Cancelled, first.wait(a, .none, null));

    const second = try s.run(.{ .text = "two" }, a, .none, null);
    try std.testing.expectEqual(agent.types.TurnStatus.failed, second.status);
}

/// Answers every control request by subtype and records the lines the host
/// sends that matter to the tests in out.txt (in the working directory):
/// the initialize request and the answers to the CLI's requests.
/// "trigger_dialogs" makes it ask four dialogs, one of a kind the host did
/// not declare; "withdraw" withdraws dialog d3.
const dialog_script =
    \\#!/bin/sh
    \\answer() { printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"'"$1"'","response":'"$2"'}}'; }
    \\while read -r l; do
    \\  id=$(printf '%s' "$l" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
    \\  case "$l" in
    \\    *'"type":"control_response"'*) printf '%s\n' "$l" >> out.txt ;;
    \\    *'"subtype":"initialize"'*) printf '%s\n' "$l" >> out.txt; answer "$id" '{"models":[],"claude_code_version":"9.9.9"}' ;;
    \\    *'"subtype":"get_context_usage"'*) answer "$id" '{"totalTokens":5,"maxTokens":123456,"rawMaxTokens":200000}' ;;
    \\    *'"subtype":"trigger_dialogs"'*)
    \\      printf '%s\n' '{"type":"control_request","request_id":"d1","request":{"subtype":"request_user_dialog","dialog_kind":"resume_return","payload":{"x":1},"tool_use_id":"tu1"}}'
    \\      printf '%s\n' '{"type":"control_request","request_id":"d2","request":{"subtype":"request_user_dialog","dialog_kind":"undeclared","payload":{}}}'
    \\      printf '%s\n' '{"type":"control_request","request_id":"d3","request":{"subtype":"request_user_dialog","dialog_kind":"resume_return","payload":{}}}'
    \\      printf '%s\n' '{"type":"control_request","request_id":"d4","request":{"subtype":"request_user_dialog","dialog_kind":"resume_return","payload":{}}}'
    \\      answer "$id" '{}' ;;
    \\    *'"subtype":"withdraw"'*) printf '%s\n' '{"type":"control_cancel_request","request_id":"d3"}'; answer "$id" '{}' ;;
    \\    *'"type":"control_request"'*) answer "$id" '{}' ;;
    \\  esac
    \\done
;

/// Writes `dialog_script` as claude.sh in `tmp` and returns its path.
fn writeDialogScript(tmp: *std.testing.TmpDir, a: std.mem.Allocator) ![]const u8 {
    const io = std.testing.io;
    try tmp.dir.writeFile(io, .{ .sub_path = "claude.sh", .data = dialog_script, .flags = .{ .permissions = .executable_file } });
    return tmp.dir.realPathFileAlloc(io, "claude.sh", a);
}

const short_wait: std.Io.Timeout = .{ .duration = .{ .raw = .fromMilliseconds(200), .clock = .awake } };

test "dialogs of declared kinds become dialog requests; answers and withdrawals reach the CLI" {
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const path = try writeDialogScript(&tmp, a);
    const cwd = try tmp.dir.realPathFileAlloc(io, ".", a);
    const env: agent.Env = .{ .io = io, .gpa = gpa, .environ = std.testing.environ };

    const provider = try root.create(env, .{ .path = path });
    defer provider.deinit();
    const s = try provider.start(.{ .config = .{
        .cwd = cwd,
        .session_id = "0b4c3f0e-1d2a-4b5c-8d9e-0f1a2b3c4d5e",
        .provider_options = .of("{\"supported_dialog_kinds\":[\"resume_return\"]}"),
    } }, null);
    defer s.deinit();
    try std.testing.expect(s.capabilities.dialogs);

    _ = try s.call("trigger_dialogs", null, a, null);
    var dialogs: std.ArrayList(agent.types.DialogRequest) = .empty;
    var waits: usize = 0;
    while (dialogs.items.len < 3 and waits < 50) : (waits += 1) {
        const owned = s.nextEvent(short_wait) catch continue;
        defer owned.destroy();
        if (owned.event.payload == .dialog_request) try dialogs.append(a, try agent.clone(agent.types.DialogRequest, a, owned.event.payload.dialog_request));
    }
    try std.testing.expectEqual(@as(usize, 3), dialogs.items.len);
    for (dialogs.items) |dialog| {
        try std.testing.expect(!std.mem.eql(u8, dialog.id, "d2"));
        try std.testing.expectEqualStrings("resume_return", dialog.kind);
        if (std.mem.eql(u8, dialog.id, "d1")) {
            try std.testing.expectEqualStrings("{\"x\":1}", dialog.data.?.bytes);
            try std.testing.expectEqualStrings("tu1", dialog.tool_use_id);
        }
    }

    try s.respond("d1", .{ .dialog = .{ .behavior = .completed, .result = .of("{\"choice\":\"a\"}") } }, null);
    try s.respond("d4", .{ .dialog = .{} }, null);
    _ = try s.call("withdraw", null, a, null);
    var withdrawn = false;
    waits = 0;
    while (!withdrawn and waits < 50) : (waits += 1) {
        const owned = s.nextEvent(short_wait) catch continue;
        defer owned.destroy();
        switch (owned.event.payload) {
            .request_resolved => |resolved| if (std.mem.eql(u8, resolved.request_id, "d3")) {
                try std.testing.expectEqual(agent.types.RequestOutcome.withdrawn, resolved.outcome);
                try std.testing.expectEqual(agent.types.RequestKind.dialog, resolved.kind);
                withdrawn = true;
            },
            .dialog_request => |dialog| try std.testing.expect(!std.mem.eql(u8, dialog.id, "d2")),
            else => {},
        }
    }
    try std.testing.expect(withdrawn);
    try std.testing.expectError(error.RequestDone, s.respond("d3", .{ .dialog = .{} }, null));
    // The script handles lines in order, so the answers are recorded once
    // this returns.
    _ = try s.call("sync", null, a, null);

    const out = try tmp.dir.readFileAlloc(io, "out.txt", a, .limited(1 << 20));
    try std.testing.expect(std.mem.indexOf(u8, out, "\"supportedDialogKinds\":[\"resume_return\"]") != null);
    try std.testing.expect(std.mem.indexOf(u8, out, "{\"request_id\":\"d1\",\"subtype\":\"success\",\"response\":{\"behavior\":\"completed\",\"result\":{\"choice\":\"a\"}}}") != null);
    try std.testing.expect(std.mem.indexOf(u8, out, "{\"request_id\":\"d4\",\"subtype\":\"success\",\"response\":{\"behavior\":\"cancelled\"}}") != null);
    // A dialog of an undeclared kind is never answered.
    try std.testing.expect(std.mem.indexOf(u8, out, "\"d2\"") == null);
    try std.testing.expect(std.mem.indexOf(u8, out, "\"d3\"") == null);
    try std.testing.expectEqual(@as(i64, 123456), (try s.info(a)).context_window);
}

test "without declared dialog kinds the initialize request names none" {
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const path = try writeDialogScript(&tmp, a);
    const cwd = try tmp.dir.realPathFileAlloc(io, ".", a);
    const env: agent.Env = .{ .io = io, .gpa = gpa, .environ = std.testing.environ };

    const provider = try root.create(env, .{ .path = path });
    defer provider.deinit();
    const s = try provider.start(.{ .config = .{ .cwd = cwd, .session_id = "0b4c3f0e-1d2a-4b5c-8d9e-0f1a2b3c4d5e" } }, null);
    defer s.deinit();
    _ = try s.call("sync", null, a, null);
    const out = try tmp.dir.readFileAlloc(io, "out.txt", a, .limited(1 << 20));
    try std.testing.expect(std.mem.indexOf(u8, out, "\"subtype\":\"initialize\"") != null);
    try std.testing.expect(std.mem.indexOf(u8, out, "supportedDialogKinds") == null);
}

/// Records its arguments in argv.txt, then runs them.
const wrapper_script =
    \\#!/bin/sh
    \\for arg in "$@"; do printf '%s\n' "$arg" >> argv.txt; done
    \\exec "$@"
;

test "command_prefix runs the CLI through a wrapper" {
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const path = try writeDialogScript(&tmp, a);
    try tmp.dir.writeFile(io, .{ .sub_path = "wrap.sh", .data = wrapper_script, .flags = .{ .permissions = .executable_file } });
    const wrapper = try tmp.dir.realPathFileAlloc(io, "wrap.sh", a);
    const cwd = try tmp.dir.realPathFileAlloc(io, ".", a);
    const env: agent.Env = .{ .io = io, .gpa = gpa, .environ = std.testing.environ };

    const provider = try root.create(env, .{ .path = path });
    defer provider.deinit();
    const s = try provider.start(.{ .config = .{
        .cwd = cwd,
        .session_id = "0b4c3f0e-1d2a-4b5c-8d9e-0f1a2b3c4d5e",
        .command_prefix = &.{wrapper},
    } }, null);
    defer s.deinit();
    const argv = try tmp.dir.readFileAlloc(io, "argv.txt", a, .limited(1 << 20));
    var lines = std.mem.splitScalar(u8, argv, '\n');
    try std.testing.expectEqualStrings(path, lines.next().?);
    try std.testing.expectEqualStrings("--output-format", lines.next().?);
}
