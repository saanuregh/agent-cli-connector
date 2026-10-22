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

    const res = try s.run(.{ .text = "hi" }, a, .none, null);
    try std.testing.expectEqual(agent.types.TurnStatus.completed, res.status);
    try std.testing.expectEqualStrings("Hello", res.text);
    try std.testing.expectEqual(@as(f64, 0.5), res.usage.cost_usd);
    try std.testing.expectEqual(@as(i64, 200000), res.usage.context_window);

    var deltas: std.ArrayList(u8) = .empty;
    var bash: ?agent.types.Item = null;
    var saw_user = false;
    var info_mode: []const u8 = "";
    while (s.nextEvent(.{ .duration = .{ .raw = .fromMilliseconds(200), .clock = .awake } })) |owned| {
        defer owned.destroy();
        switch (owned.event.payload) {
            .text_delta => |delta| try deltas.appendSlice(a, delta.delta),
            .item_completed => |completed| {
                if (completed.item.kind == .tool_call) bash = try agent.clone(agent.types.Item, a, completed.item);
                if (completed.item.kind == .user_message) saw_user = true;
            },
            .session_updated => |update| info_mode = try a.dupe(u8, update.info.permission_mode),
            .turn_completed => break,
            else => {},
        }
    } else |_| {}
    try std.testing.expectEqualStrings("Hello", deltas.items);
    try std.testing.expect(saw_user);
    try std.testing.expectEqualStrings("accept_edits", info_mode);
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
    \\first=$(echo "$l" | sed 's/.*"uuid":"\([^"]*\)".*/\1/')
    \\read l
    \\printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"req_3","response":{"cancelled":["'"$first"'"]}}}'
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
