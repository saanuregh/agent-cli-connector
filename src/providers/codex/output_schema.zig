//! Output schemas in the strict form OpenAI structured outputs require.
const std = @import("std");
const agent = @import("agent");
const Allocator = std.mem.Allocator;
const Value = std.json.Value;
const RawJson = agent.json.RawJson;
const errors = agent.errors;
const contains = agent.providerkit.contains;
const Diagnostics = agent.Diagnostics;

/// Keywords whose value maps names to subschemas.
const schema_map_keywords = [_][]const u8{ "properties", "patternProperties", "$defs", "definitions", "dependentSchemas" };

/// Keywords whose value is a subschema or an array of subschemas.
const subschema_keywords = [_][]const u8{
    "items",            "prefixItems",   "additionalItems", "contains", "additionalProperties", "unevaluatedProperties",
    "unevaluatedItems", "propertyNames", "allOf",           "anyOf",    "oneOf",                "not",
    "if",               "then",          "else",
};

/// Rewrites a JSON Schema into strict form: the root is an object schema,
/// and every object schema, at any depth, sets additionalProperties to false
/// and lists all its properties as required. Fails when an object schema
/// allows additional properties. Property order and numbers are kept as
/// written, because the model writes keys in schema order.
pub fn strictOutputSchema(a: Allocator, schema: RawJson, diag: ?*Diagnostics) errors.Error!RawJson {
    var root = std.json.parseFromSliceLeaky(Value, a, schema.bytes, .{ .parse_numbers = false, .allocate = .alloc_always }) catch |e|
        return errors.fail(diag, .invalid_params, "codex: output schema: {t}", .{e});
    if (root != .object) return errors.fail(diag, .invalid_params, "codex: output schema must be a JSON object", .{});
    try strictNode(a, &root.object, "$", diag);
    if (!isObjectSchema(&root.object)) return errors.fail(diag, .invalid_params, "codex: output schema root must be an object schema", .{});
    return .{ .bytes = try agent.json.encodeValue(a, root) };
}

fn strictSubschema(a: Allocator, subschema: *Value, path: []const u8, diag: ?*Diagnostics) errors.Error!void {
    switch (subschema.*) {
        .object => |*object| try strictNode(a, object, path, diag),
        .array => |array| for (array.items, 0..) |*entry, i| {
            if (entry.* == .object) try strictNode(a, &entry.object, try std.fmt.allocPrint(a, "{s}[{d}]", .{ path, i }), diag);
        },
        else => {},
    }
}

fn strictNode(a: Allocator, node: *std.json.ObjectMap, path: []const u8, diag: ?*Diagnostics) errors.Error!void {
    for (schema_map_keywords) |keyword| {
        const map = node.getPtr(keyword) orelse continue;
        if (map.* != .object) continue;
        var entries = map.object.iterator();
        while (entries.next()) |entry| try strictSubschema(a, entry.value_ptr, try std.fmt.allocPrint(a, "{s}.{s}.{s}", .{ path, keyword, entry.key_ptr.* }), diag);
    }
    for (subschema_keywords) |keyword| {
        const subschema = node.getPtr(keyword) orelse continue;
        try strictSubschema(a, subschema, try std.fmt.allocPrint(a, "{s}.{s}", .{ path, keyword }), diag);
    }
    if (!isObjectSchema(node)) return;
    if (node.get("additionalProperties")) |additional| {
        if (!(additional == .bool and !additional.bool)) return errors.fail(
            diag,
            .invalid_params,
            "codex: output schema: {s} must set additionalProperties to false; strict structured outputs reject other values (set disable_strict_output_schema to send the schema unchanged)",
            .{path},
        );
    } else try node.put(a, "additionalProperties", .{ .bool = false });
    const props = node.get("properties") orelse return;
    if (props != .object) return;
    var required: std.ArrayList([]const u8) = .empty;
    if (node.get("required")) |list| if (list == .array) for (list.array.items) |entry| {
        if (entry == .string and !contains(required.items, entry.string)) try required.append(a, entry.string);
    };
    for (props.object.keys()) |name| if (!contains(required.items, name)) try required.append(a, name);
    try node.put(a, "required", try agent.json.stringArray(a, required.items));
}

fn isObjectSchema(node: *const std.json.ObjectMap) bool {
    if (node.get("properties")) |properties| if (properties == .object) return true;
    const schema_type = node.get("type") orelse return false;
    return switch (schema_type) {
        .string => |s| std.mem.eql(u8, s, "object"),
        .array => |array| for (array.items) |entry| {
            if (entry == .string and std.mem.eql(u8, entry.string, "object")) break true;
        } else false,
        else => false,
    };
}

test "strict output schema" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const out = try strictOutputSchema(a, .of(
        \\{"type":"object","properties":{"b":{"type":"object","properties":{"x":{"type":"number","maximum":1.50}}},"a":{"type":"string"}},"required":["a"]}
    ), null);
    try std.testing.expectEqualStrings(
        \\{"type":"object","properties":{"b":{"type":"object","properties":{"x":{"type":"number","maximum":1.50}},"additionalProperties":false,"required":["x"]},"a":{"type":"string"}},"required":["a","b"],"additionalProperties":false}
    , out.bytes);
    try std.testing.expectError(error.InvalidParams, strictOutputSchema(a, .of(
        \\{"type":"object","additionalProperties":true}
    ), null));
    try std.testing.expectError(error.InvalidParams, strictOutputSchema(a, .of("{\"type\":\"string\"}"), null));
}
