//! Permission prompts, AskUserQuestion, and MCP elicitations: CLI control
//! requests that become host requests, and their answers. Also maps
//! permission mode names between the uniform and Claude forms.
const std = @import("std");
const Allocator = std.mem.Allocator;
const agent = @import("agent");
const types = agent.types;
const errors = agent.errors;
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const ObjectBuilder = agent.json.ObjectBuilder;
const Value = std.json.Value;
const Session = agent.Session;
const Diagnostics = agent.Diagnostics;
const kit = agent.providerkit;
const eql = kit.eql;

const decode = @import("decode.zig");
const control = @import("control.zig");
const ClaudeSession = @import("session.zig").ClaudeSession;

/// Turns a can_use_tool request into a permission request, or a user input
/// request for AskUserQuestion. `request_bytes` (the request body) is kept to
/// answer it.
pub fn handlePermission(claude: *ClaudeSession, a: Allocator, id: []const u8, request: JsonView, request_bytes: []const u8, line: []const u8) !void {
    const input = if (request.get("input").isNull()) JsonView.parseBytes(a, "{}") else request.get("input");
    const tool = request.str("tool_name");
    if (eql(tool, "AskUserQuestion")) return askUserQuestion(claude, a, id, request, input, request_bytes, line);

    var kind: types.PermissionKind = .tool;
    var command: []const u8 = "";
    var plan: []const u8 = "";
    var changes: []const types.FileChange = &.{};
    switch (decode.toolKind(tool, claude.toolServerName())) {
        .shell => {
            kind = .command;
            command = input.str("command");
        },
        .edit => {
            kind = .file_change;
            changes = try decode.editChanges(a, tool, input, .empty);
        },
        .plan => if (eql(tool, "ExitPlanMode")) {
            kind = .plan_approval;
            plan = input.str("plan");
        },
        else => {},
    }
    const paths: []const []const u8 = if (request.str("blocked_path").len > 0) try a.dupe([]const u8, &.{request.str("blocked_path")}) else &.{};
    const decisions: []const types.PermissionDecision = if (offersRules(request))
        &.{ .allow, .allow_session, .allow_always, .deny, .cancel }
    else
        &.{ .allow, .deny, .cancel };
    claude.rt().request(.{ .permission_request = .{
        .id = id,
        .kind = kind,
        .tool_name = tool,
        .tool_call_id = request.str("tool_use_id"),
        .input = try input.raw(a),
        .title = kit.firstOf(&.{ request.str("title"), request.str("display_name") }),
        .description = request.str("description"),
        .reason = request.str("decision_reason"),
        .command = command,
        .changes = changes,
        .paths = paths,
        .plan = plan,
        .decisions = decisions,
        .subagent_id = request.str("agent_id"),
    } }, RawJson.of(line), request_bytes);
}

/// Whether the request can be allowed with a rule: only the CLI's own
/// suggestions are scoped to this call, and suppress_always_allow_rule
/// forbids rules.
fn offersRules(request: JsonView) bool {
    return request.get("permission_suggestions").len() > 0 and !request.flag("suppress_always_allow_rule");
}

/// Turns the AskUserQuestion tool's permission prompt into a user input
/// request. The answers go back as the tool's updated input.
fn askUserQuestion(claude: *ClaudeSession, a: Allocator, id: []const u8, request: JsonView, input: JsonView, request_bytes: []const u8, line: []const u8) !void {
    const question_values = input.get("questions").items();
    const questions = try a.alloc(types.Question, question_values.len);
    for (question_values, questions) |value, *question| {
        const native = JsonView.of(value);
        const option_values = native.get("options").items();
        const question_options = try a.alloc(types.QuestionOption, option_values.len);
        for (option_values, question_options) |option_value, *option| {
            const native_option = JsonView.of(option_value);
            option.* = .{ .label = native_option.str("label"), .description = native_option.str("description"), .preview = native_option.str("preview") };
        }
        question.* = .{
            .id = native.str("question"),
            .header = native.str("header"),
            .text = native.str("question"),
            .options = question_options,
            .multi_select = native.flag("multiSelect"),
            .allow_other = true,
        };
    }
    claude.rt().request(.{ .user_input_request = .{
        .id = id,
        .questions = questions,
        .tool_call_id = request.str("tool_use_id"),
        .subagent_id = request.str("agent_id"),
    } }, RawJson.of(line), request_bytes);
}

/// Turns an MCP elicitation into an elicitation request. The schema keeps
/// the raw JSON, so the server's property order survives.
pub fn handleElicitation(claude: *ClaudeSession, a: Allocator, id: []const u8, request: JsonView, line: []const u8) !void {
    claude.rt().request(.{ .elicitation_request = .{
        .id = id,
        .server = request.str("mcp_server_name"),
        .message = request.str("message"),
        .mode = kit.firstOf(&.{ request.str("mode"), "form" }),
        .schema = try request.get("requested_schema").raw(a),
        .url = request.str("url"),
        .elicitation_id = request.str("elicitation_id"),
        .title = kit.firstOf(&.{ request.str("title"), request.str("display_name") }),
        .description = request.str("description"),
    } }, RawJson.of(line), "");
}

/// Answers a pending request natively (`Session.VTable.respond`).
pub fn respond(s: *Session, pending: *const Session.Pending, response: types.Response, diag: ?*Diagnostics) errors.Error!void {
    const claude = ClaudeSession.of(s);
    var arena: std.heap.ArenaAllocator = .init(claude.gpa());
    defer arena.deinit();
    const a = arena.allocator();
    const request = JsonView.parseBytes(a, if (pending.native.len > 0) pending.native else "{}");
    const payload: []const u8 = switch (response) {
        .permission => |answer| try permissionResult(a, request, answer),
        .user_input => |answer| try questionAnswer(a, request, answer),
        .elicitation => |answer| blk: {
            var out: ObjectBuilder = .init(a);
            try out.str("action", @tagName(answer.action));
            if (answer.content) |content| try out.put("content", agent.json.valueOf(a, content) orelse .null);
            break :blk try agent.json.encodeValue(a, out.value());
        },
    };
    return control.respond(claude, a, pending.id.slice(), payload, null, diag);
}

/// The native answer to a permission request. allow_session and
/// allow_always without rules to offer allow the one call, because a rule
/// made up here would allow every call of the tool.
fn permissionResult(a: Allocator, request: JsonView, answer: types.PermissionResponse) Allocator.Error![]const u8 {
    if (answer.native) |native| return native.bytes;
    var out: ObjectBuilder = .init(a);
    const tool_use_id = request.str("tool_use_id");
    switch (answer.decision) {
        .allow, .allow_session, .allow_always => {
            try out.str("behavior", "allow");
            const input: Value = if (answer.updated_input) |updated|
                agent.json.valueOf(a, updated) orelse .null
            else if (request.get("input").isNull())
                ObjectBuilder.init(a).value()
            else
                request.get("input").v.?;
            try out.put("updatedInput", input);
            try out.strIf("toolUseID", tool_use_id);
            var classification: []const u8 = "user_temporary";
            if (offersRules(request) and answer.decision != .allow) {
                classification = "user_permanent";
                const suggestions = request.get("permission_suggestions").v.?;
                if (answer.decision == .allow_session) {
                    var list: std.json.Array = .init(a);
                    for (suggestions.array.items) |suggestion| {
                        var copy: ObjectBuilder = .init(a);
                        if (suggestion == .object) try copy.putAll(suggestion.object);
                        try copy.str("destination", "session");
                        try list.append(copy.value());
                    }
                    try out.put("updatedPermissions", .{ .array = list });
                } else try out.put("updatedPermissions", suggestions);
            }
            try out.str("decisionClassification", classification);
        },
        .deny, .cancel => {
            try out.str("behavior", "deny");
            try out.str("message", if (answer.message.len > 0) answer.message else "The user denied this action.");
            if (answer.decision == .cancel) try out.put("interrupt", .{ .bool = true });
            try out.strIf("toolUseID", tool_use_id);
            try out.str("decisionClassification", "user_reject");
        },
    }
    return agent.json.encodeValue(a, out.value());
}

/// The answer to AskUserQuestion: the tool input with the answers added,
/// or a denial when the user declined.
fn questionAnswer(a: Allocator, request: JsonView, answer: types.UserInputResponse) Allocator.Error![]const u8 {
    var out: ObjectBuilder = .init(a);
    if (answer.cancelled) {
        try out.str("behavior", "deny");
        try out.str("message", "The user declined to answer the questions.");
    } else {
        var input: ObjectBuilder = .init(a);
        if (request.get("input").object()) |fields| try input.putAll(fields);
        var answers: ObjectBuilder = .init(a);
        if (answer.answers) |map| for (map.map.keys(), map.map.values()) |question, choices| {
            try answers.str(question, try std.mem.join(a, ", ", choices));
        };
        try input.put("answers", answers.value());
        try out.str("behavior", "allow");
        try out.put("updatedInput", input.value());
    }
    try out.strIf("toolUseID", request.str("tool_use_id"));
    return agent.json.encodeValue(a, out.value());
}

// ---------------------------------------------------------------- permission modes

/// Uniform permission modes and their Claude names.
const mode_names = [_][2][]const u8{
    .{ types.permission_mode.default, "default" },
    .{ types.permission_mode.accept_edits, "acceptEdits" },
    .{ types.permission_mode.plan, "plan" },
    .{ types.permission_mode.bypass, "bypassPermissions" },
    .{ types.permission_mode.dont_ask, "dontAsk" },
    .{ types.permission_mode.auto, "auto" },
};

/// The Claude permission mode for a uniform one; other strings pass
/// through. `read_only` has no Claude equivalent and returns null.
pub fn nativeMode(mode: []const u8) ?[]const u8 {
    if (eql(mode, types.permission_mode.read_only) or eql(mode, "readOnly")) return null;
    for (mode_names) |names| if (eql(mode, names[0])) return names[1];
    return mode;
}

/// The uniform permission mode for a Claude one; unknown modes pass
/// through.
pub fn uniformMode(native: []const u8) []const u8 {
    for (mode_names) |names| if (eql(native, names[1])) return names[0];
    return native;
}

pub fn isBypass(mode: []const u8) bool {
    return eql(mode, types.permission_mode.bypass) or eql(mode, "bypassPermissions");
}

test "permission without suggestions allows only the call" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const request = JsonView.parseBytes(a, "{\"tool_name\":\"Bash\",\"input\":{\"command\":\"ls\"},\"tool_use_id\":\"tb\"}");
    const out = try permissionResult(a, request, .{ .decision = .allow_always });
    try std.testing.expectEqualStrings("{\"behavior\":\"allow\",\"updatedInput\":{\"command\":\"ls\"},\"toolUseID\":\"tb\",\"decisionClassification\":\"user_temporary\"}", out);
}

test "permission modes map both ways" {
    try std.testing.expectEqualStrings("acceptEdits", nativeMode("accept_edits").?);
    try std.testing.expectEqualStrings("bypass", uniformMode("bypassPermissions"));
    try std.testing.expect(nativeMode("read_only") == null);
}
