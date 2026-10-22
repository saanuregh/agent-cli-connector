//! An MCP server for host tools that answers JSON-RPC messages without a
//! transport: Claude tunnels them through `mcp_message` control requests.
const std = @import("std");
const Allocator = std.mem.Allocator;
const agent = @import("agent");
const types = agent.types;
const JsonView = agent.json.JsonView;
const Value = std.json.Value;
const ObjectBuilder = agent.json.ObjectBuilder;
const kit = agent.providerkit;

/// Offered when the client names no protocol version.
pub const default_protocol_version = "2025-06-18";

/// The MCP methods the server answers.
const Method = enum { initialize, ping, @"tools/list", @"tools/call", @"resources/list", @"prompts/list" };

pub const Server = struct {
    name: []const u8,
    version: []const u8,
    tools: []const types.ToolDef,

    pub fn hasTool(server: Server, name: []const u8) bool {
        for (server.tools) |tool| if (kit.eql(tool.name, name)) return true;
        return false;
    }

    /// Answers one message; null for notifications. Tool calls run through
    /// `session.callTool` with `session_id`.
    pub fn handle(server: Server, a: Allocator, message_bytes: []const u8, session_id: []const u8, session: *agent.Session) Allocator.Error!?[]const u8 {
        const parsed = std.json.parseFromSliceLeaky(Value, a, message_bytes, .{}) catch {
            return @as(?[]const u8, try respond(a, .null, null, .{ .code = -32700, .message = "parse error" }));
        };
        const message = JsonView.of(parsed);
        const id = message.get("id");
        if (id.isNull()) return null;
        var err: ?RpcError = null;
        const result = try server.dispatch(a, message, session_id, session, &err);
        return try respond(a, id.v.?, result, err);
    }

    const RpcError = struct { code: i64, message: []const u8 };

    fn dispatch(server: Server, a: Allocator, message: JsonView, session_id: []const u8, session: *agent.Session, err: *?RpcError) Allocator.Error!?Value {
        const method_name = message.str("method");
        const params = message.get("params");
        const method = std.meta.stringToEnum(Method, method_name) orelse {
            err.* = .{ .code = -32601, .message = try std.fmt.allocPrint(a, "method not found: {s}", .{method_name}) };
            return null;
        };
        return switch (method) {
            .initialize => try agent.json.toValue(a, .{
                .protocolVersion = kit.firstOf(&.{ params.str("protocolVersion"), default_protocol_version }),
                .capabilities = .{ .tools = .{ .listChanged = false } },
                .serverInfo = .{ .name = server.name, .version = server.version },
            }),
            .ping => ObjectBuilder.init(a).value(),
            .@"tools/list" => try server.toolList(a),
            .@"tools/call" => try server.callTool(a, params, session_id, session, err),
            .@"resources/list" => agent.json.parseValue(a, "{\"resources\":[]}") orelse .null,
            .@"prompts/list" => agent.json.parseValue(a, "{\"prompts\":[]}") orelse .null,
        };
    }

    fn callTool(server: Server, a: Allocator, params: JsonView, session_id: []const u8, session: *agent.Session, err: *?RpcError) Allocator.Error!?Value {
        if (params.object() == null) {
            err.* = .{ .code = -32602, .message = "invalid params" };
            return null;
        }
        const name = params.str("name");
        if (!server.hasTool(name)) {
            err.* = .{ .code = -32602, .message = try std.fmt.allocPrint(a, "unknown tool {s}", .{name}) };
            return null;
        }
        const arguments = (try params.get("arguments").raw(a)) orelse agent.json.RawJson.of("{}");
        const result = session.callTool(a, .{
            .id = params.get("_meta").str("claudecode/toolUseId"),
            .name = name,
            .arguments = arguments,
            .session_id = session_id,
        });
        return try encodeResult(a, result);
    }

    fn toolList(server: Server, a: Allocator) Allocator.Error!Value {
        var list: std.json.Array = .init(a);
        for (server.tools) |tool| {
            var entry: ObjectBuilder = .init(a);
            try entry.str("name", tool.name);
            try entry.str("description", tool.description);
            const schema = if (tool.input_schema) |input_schema| input_schema.bytes else "{\"type\":\"object\"}";
            try entry.put("inputSchema", agent.json.parseValue(a, schema) orelse ObjectBuilder.init(a).value());
            var annotations: ObjectBuilder = .init(a);
            if (tool.read_only) try annotations.put("readOnlyHint", .{ .bool = true });
            if (tool.destructive) try annotations.put("destructiveHint", .{ .bool = true });
            if (tool.open_world) try annotations.put("openWorldHint", .{ .bool = true });
            if (tool.title.len > 0) {
                try annotations.str("title", tool.title);
                try entry.str("title", tool.title);
            }
            if (annotations.map.count() > 0) try entry.put("annotations", annotations.value());
            var meta: ObjectBuilder = .init(a);
            if (tool.always_load) try meta.put("anthropic/alwaysLoad", .{ .bool = true });
            try meta.strIf("anthropic/searchHint", tool.search_hint);
            if (meta.map.count() > 0) try entry.put("_meta", meta.value());
            try list.append(entry.value());
        }
        var out: ObjectBuilder = .init(a);
        try out.put("tools", .{ .array = list });
        return out.value();
    }
};

/// A tool result as an MCP CallToolResult: images become base64 image
/// blocks, everything else text blocks.
pub fn encodeResult(a: Allocator, result: types.ToolResult) Allocator.Error!Value {
    var content: std.json.Array = .init(a);
    for (result.content) |part| {
        var block: ObjectBuilder = .init(a);
        switch (part.type) {
            .image => {
                try block.str("type", "image");
                try block.str("data", part.data);
                try block.str("mimeType", part.media_type);
            },
            .text => {
                try block.str("type", "text");
                try block.str("text", part.text);
            },
        }
        try content.append(block.value());
    }
    var out: ObjectBuilder = .init(a);
    try out.put("content", .{ .array = content });
    if (result.is_error) try out.put("isError", .{ .bool = true });
    // MCP requires structuredContent to be an object.
    if (result.structured) |structured| {
        const trimmed = std.mem.trim(u8, structured.bytes, " \t\r\n");
        if (trimmed.len > 0 and trimmed[0] == '{') try out.put("structuredContent", agent.json.parseValue(a, trimmed) orelse ObjectBuilder.init(a).value());
    }
    return out.value();
}

fn respond(a: Allocator, id: Value, result: ?Value, err: ?Server.RpcError) Allocator.Error![]const u8 {
    return (try agent.json.RawJson.encode(a, .{
        .jsonrpc = "2.0",
        .id = id,
        .result = if (err == null) result else null,
        .@"error" = err,
    })).bytes;
}
