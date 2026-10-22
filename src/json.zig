//! JSON helpers: the compact wire encoding of the library's types, raw JSON
//! passthrough, and a forgiving reader for native harness messages.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Value = std.json.Value;
const Stringify = std.json.Stringify;

/// A JSON value kept as its encoded text, written to output unchanged.
pub const RawJson = struct {
    bytes: []const u8,

    pub fn of(bytes: []const u8) RawJson {
        return .{ .bytes = bytes };
    }

    /// Encodes `v` with `std.json` (nulls left out).
    pub fn encode(a: Allocator, v: anytype) !RawJson {
        return .{ .bytes = try Stringify.valueAlloc(a, v, .{ .emit_null_optional_fields = false }) };
    }

    pub fn parse(r: RawJson, comptime T: type, a: Allocator) !T {
        return std.json.parseFromSliceLeaky(T, a, r.bytes, .{ .ignore_unknown_fields = true, .allocate = .alloc_always });
    }

    pub fn jsonStringify(r: RawJson, jw: *Stringify) !void {
        try jw.beginWriteRaw();
        try jw.writer.writeAll(if (r.bytes.len == 0) "null" else r.bytes);
        jw.endWriteRaw();
    }

    pub fn jsonParse(a: Allocator, source: anytype, options: std.json.ParseOptions) !RawJson {
        const v = try Value.jsonParse(a, source, options);
        return fromJsonValue(a, v);
    }

    pub fn jsonParseFromValue(a: Allocator, v: Value, _: std.json.ParseOptions) !RawJson {
        return fromJsonValue(a, v);
    }

    pub fn fromJsonValue(a: Allocator, v: Value) error{OutOfMemory}!RawJson {
        return .{ .bytes = try Stringify.valueAlloc(a, v, .{}) };
    }
};

// ---------------------------------------------------------------- wire encoding

/// Encodes a library value in the wire format: fields holding their default
/// value are left out and unions become objects tagged with "type".
pub fn stringify(a: Allocator, v: anytype) error{OutOfMemory}![]u8 {
    var out: std.Io.Writer.Allocating = .init(a);
    defer out.deinit();
    var jw: Stringify = .{ .writer = &out.writer };
    write(&jw, v) catch return error.OutOfMemory;
    return out.toOwnedSlice();
}

/// Writes `v` in the wire format; see `stringify`.
pub fn write(jw: *Stringify, v: anytype) Stringify.Error!void {
    const T = @TypeOf(v);
    switch (@typeInfo(T)) {
        .@"struct" => |info| {
            if (comptime std.meta.hasFn(T, "jsonStringify")) return v.jsonStringify(jw);
            try jw.beginObject();
            inline for (info.field_names, info.field_types, info.field_attrs) |name, FT, attrs|
                try writeField(jw, name, FT, attrs, @field(v, name));
            try jw.endObject();
        },
        .@"union" => {
            if (comptime std.meta.hasFn(T, "jsonStringify")) return v.jsonStringify(jw);
            try jw.beginObject();
            try writeUnionBody(jw, v);
            try jw.endObject();
        },
        .optional => if (v) |x| try write(jw, x) else try jw.write(null),
        .pointer => |p| if (p.size == .slice and p.child != u8) {
            try jw.beginArray();
            for (v) |x| try write(jw, x);
            try jw.endArray();
        } else try jw.write(v),
        .@"enum" => try jw.write(@tagName(v)),
        else => try jw.write(v),
    }
}

/// Writes `v`'s fields and its union field `union_field` as one object
/// tagged with the union's active tag.
pub fn writeFlattened(jw: *Stringify, v: anytype, comptime union_field: []const u8) Stringify.Error!void {
    try jw.beginObject();
    try writeUnionBody(jw, @field(v, union_field));
    const info = @typeInfo(@TypeOf(v)).@"struct";
    inline for (info.field_names, info.field_types, info.field_attrs) |name, FT, attrs| {
        if (comptime !std.mem.eql(u8, name, union_field)) try writeField(jw, name, FT, attrs, @field(v, name));
    }
    try jw.endObject();
}

fn writeUnionBody(jw: *Stringify, u: anytype) Stringify.Error!void {
    switch (u) {
        inline else => |payload, tag| {
            try jw.objectField("type");
            try jw.write(@tagName(tag));
            const P = @TypeOf(payload);
            if (@typeInfo(P) == .@"struct" and !comptime std.meta.hasFn(P, "jsonStringify")) {
                const info = @typeInfo(P).@"struct";
                inline for (info.field_names, info.field_types, info.field_attrs) |name, FT, attrs|
                    try writeField(jw, name, FT, attrs, @field(payload, name));
            } else if (P != void) {
                try jw.objectField("value");
                try write(jw, payload);
            }
        },
    }
}

const FieldAttributes = std.lang.Type.Struct.FieldAttributes;

fn writeField(jw: *Stringify, comptime name: []const u8, comptime FT: type, comptime attrs: FieldAttributes, v: FT) Stringify.Error!void {
    if (comptime attrs.defaultValue(FT)) |d| if (isDefault(FT, v, d)) return;
    try jw.objectField(name);
    try write(jw, v);
}

/// Empty strings and lists, false, and null are left out. Numbers and enums
/// are always written: a missing number or enum would read as undefined in
/// dynamic languages rather than as its default.
fn isDefault(comptime T: type, v: T, d: T) bool {
    return switch (@typeInfo(T)) {
        .optional => v == null and d == null,
        .pointer => |p| p.size == .slice and v.len == 0 and d.len == 0,
        .bool => !v and !d,
        else => false,
    };
}

/// Parses a wire message into `T`, ignoring unknown fields. Strings are
/// copied into `a`.
pub fn parse(comptime T: type, a: Allocator, bytes: []const u8) !T {
    return std.json.parseFromSliceLeaky(T, a, bytes, .{ .ignore_unknown_fields = true, .allocate = .alloc_always });
}

/// Converts a parsed `Value` into `T`, ignoring unknown fields.
pub fn fromValue(comptime T: type, a: Allocator, v: Value) !T {
    return std.json.parseFromValueLeaky(T, a, v, .{ .ignore_unknown_fields = true });
}

// ---------------------------------------------------------------- building

/// A JSON object under construction, for native messages. Keys keep
/// insertion order and are not copied.
pub const ObjectBuilder = struct {
    a: Allocator,
    map: std.json.ObjectMap = .empty,

    pub fn init(a: Allocator) ObjectBuilder {
        return .{ .a = a };
    }

    pub fn put(o: *ObjectBuilder, key: []const u8, v: Value) Allocator.Error!void {
        try o.map.put(o.a, key, v);
    }

    pub fn str(o: *ObjectBuilder, key: []const u8, s: []const u8) Allocator.Error!void {
        try o.put(key, .{ .string = s });
    }

    /// Puts `s` when it is not empty.
    pub fn strIf(o: *ObjectBuilder, key: []const u8, s: []const u8) Allocator.Error!void {
        if (s.len > 0) try o.str(key, s);
    }

    /// Puts every field of `map`.
    pub fn putAll(o: *ObjectBuilder, map: std.json.ObjectMap) Allocator.Error!void {
        for (map.keys(), map.values()) |k, v| try o.put(k, v);
    }

    pub fn has(o: *const ObjectBuilder, key: []const u8) bool {
        return o.map.contains(key);
    }

    /// Copies the fields of the JSON object `extra` over this object.
    pub fn merge(o: *ObjectBuilder, extra: ?RawJson) Allocator.Error!void {
        const v = valueOf(o.a, extra) orelse return;
        if (v != .object) return;
        var it = v.object.iterator();
        while (it.next()) |e| try o.put(e.key_ptr.*, e.value_ptr.*);
    }

    pub fn value(o: ObjectBuilder) Value {
        return .{ .object = o.map };
    }

    pub fn toRaw(o: ObjectBuilder) Allocator.Error!RawJson {
        return .{ .bytes = try encodeValue(o.a, o.value()) };
    }
};

/// `v` (any value std.json encodes, nulls left out) as a JSON value, for
/// fixed message shapes written as struct literals.
pub fn toValue(a: Allocator, v: anytype) Allocator.Error!Value {
    const bytes = Stringify.valueAlloc(a, v, .{ .emit_null_optional_fields = false }) catch return error.OutOfMemory;
    return parseValue(a, bytes) orelse .null;
}

/// `s` as a JSON string literal.
pub fn quoteString(a: Allocator, s: []const u8) Allocator.Error![]u8 {
    return std.fmt.allocPrint(a, "{f}", .{std.json.fmt(s, .{})});
}

/// Parses JSON keeping numbers as written; null when invalid or empty.
pub fn parseValue(a: Allocator, bytes: []const u8) ?Value {
    if (bytes.len == 0) return null;
    return std.json.parseFromSliceLeaky(Value, a, bytes, .{ .parse_numbers = false, .allocate = .alloc_always }) catch null;
}

/// `r` as a value, or null when absent or invalid.
pub fn valueOf(a: Allocator, r: ?RawJson) ?Value {
    return parseValue(a, (r orelse return null).bytes);
}

/// A JSON array of strings.
pub fn stringArray(a: Allocator, list: []const []const u8) Allocator.Error!Value {
    var arr: std.json.Array = try .initCapacity(a, list.len);
    for (list) |s| arr.appendAssumeCapacity(.{ .string = s });
    return .{ .array = arr };
}

pub fn encodeValue(a: Allocator, v: Value) Allocator.Error![]u8 {
    return Stringify.valueAlloc(a, v, .{}) catch return error.OutOfMemory;
}

/// The JSON object `base` with the fields of the object `extra` added.
pub fn mergeObject(a: Allocator, base: []const u8, extra: ?RawJson) Allocator.Error![]const u8 {
    const e = valueOf(a, extra) orelse return base;
    if (e != .object or e.object.count() == 0) return base;
    var b: ObjectBuilder = .init(a);
    if (parseValue(a, base)) |bv| if (bv == .object) {
        b.map = bv.object;
    };
    for (e.object.keys(), e.object.values()) |k, v| try b.put(k, v);
    return encodeValue(a, b.value());
}

// ---------------------------------------------------------------- reading native messages

/// A forgiving view of a parsed native JSON message. Missing fields and
/// fields of the wrong type read as empty values, which keeps translation
/// code free of error handling for shapes it does not care about.
pub const JsonView = struct {
    v: ?Value,

    pub const empty: JsonView = .{ .v = null };

    pub fn of(v: ?Value) JsonView {
        return .{ .v = v };
    }

    /// Parses `bytes`; invalid JSON reads as empty.
    pub fn parseBytes(a: Allocator, bytes: []const u8) JsonView {
        return .{ .v = std.json.parseFromSliceLeaky(Value, a, bytes, .{}) catch null };
    }

    /// The field `key`, or empty.
    pub fn get(j: JsonView, key: []const u8) JsonView {
        const v = j.v orelse return empty;
        return switch (v) {
            .object => |o| .{ .v = o.get(key) },
            else => empty,
        };
    }

    pub fn isNull(j: JsonView) bool {
        return j.v == null or j.v.? == .null;
    }

    pub fn has(j: JsonView, key: []const u8) bool {
        return !j.get(key).isNull();
    }

    /// The string value, or "".
    pub fn string(j: JsonView) []const u8 {
        const v = j.v orelse return "";
        return switch (v) {
            .string, .number_string => |x| x,
            else => "",
        };
    }

    /// The string field `key`, or "".
    pub fn str(j: JsonView, key: []const u8) []const u8 {
        return j.get(key).string();
    }

    pub fn integer(j: JsonView) i64 {
        const v = j.v orelse return 0;
        return switch (v) {
            .integer => |x| x,
            .float => |f| std.math.lossyCast(i64, f),
            .number_string, .string => |x| std.fmt.parseInt(i64, x, 10) catch 0,
            else => 0,
        };
    }

    /// The integer field `key`, or 0.
    pub fn int(j: JsonView, key: []const u8) i64 {
        return j.get(key).integer();
    }

    pub fn number(j: JsonView) f64 {
        const v = j.v orelse return 0;
        return switch (v) {
            .integer => |x| @floatFromInt(x),
            .float => |x| x,
            .number_string, .string => |x| std.fmt.parseFloat(f64, x) catch 0,
            else => 0,
        };
    }

    /// The integer `key`, clamped to the u32 range.
    pub fn uint32(j: JsonView, key: []const u8) u32 {
        return @intCast(std.math.clamp(j.int(key), 0, std.math.maxInt(u32)));
    }

    /// The number field `key`, or 0.
    pub fn float(j: JsonView, key: []const u8) f64 {
        return j.get(key).number();
    }

    pub fn boolean(j: JsonView) bool {
        const v = j.v orelse return false;
        return v == .bool and v.bool;
    }

    /// The boolean field `key`, or false.
    pub fn flag(j: JsonView, key: []const u8) bool {
        return j.get(key).boolean();
    }

    /// Array elements, or none.
    pub fn items(j: JsonView) []const Value {
        const v = j.v orelse return &.{};
        return switch (v) {
            .array => |arr| arr.items,
            else => &.{},
        };
    }

    pub fn len(j: JsonView) usize {
        return j.items().len;
    }

    pub fn index(j: JsonView, n: usize) JsonView {
        const all = j.items();
        return if (n < all.len) .{ .v = all[n] } else empty;
    }

    pub fn object(j: JsonView) ?std.json.ObjectMap {
        const v = j.v orelse return null;
        return if (v == .object) v.object else null;
    }

    /// The value re-encoded as raw JSON, or null when missing.
    pub fn raw(j: JsonView, a: Allocator) !?RawJson {
        if (j.isNull()) return null;
        return try RawJson.fromJsonValue(a, j.v.?);
    }

    /// Strings of an array of strings.
    pub fn strings(j: JsonView, a: Allocator) ![]const []const u8 {
        const all = j.items();
        var out = try std.ArrayList([]const u8).initCapacity(a, all.len);
        for (all) |v| if (v == .string) out.appendAssumeCapacity(v.string);
        return out.toOwnedSlice(a);
    }
};

test "wire encoding leaves out defaults and tags unions" {
    const types = @import("types.zig");
    const a = std.testing.allocator;
    const ev: types.Event = .{ .session_id = "s1", .payload = .{ .text_delta = .{ .delta = "hi" } } };
    const out = try stringify(a, ev);
    defer a.free(out);
    try std.testing.expectEqualStrings("{\"type\":\"text_delta\",\"delta\":\"hi\",\"session_id\":\"s1\",\"time_ms\":0}", out);
}
