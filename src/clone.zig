//! Deep copies of library values, so data produced while handling one
//! native message can outlive that message's memory.
const std = @import("std");
const Allocator = std.mem.Allocator;
const RawJson = @import("json.zig").RawJson;

/// Copies `v` and everything it points to into `a`.
pub fn clone(comptime T: type, a: Allocator, v: T) Allocator.Error!T {
    if (T == RawJson) return .{ .bytes = try a.dupe(u8, v.bytes) };
    switch (@typeInfo(T)) {
        .@"struct" => |info| {
            if (comptime isArrayHashMap(T)) return cloneMap(T, a, v);
            var out: T = v;
            inline for (info.field_names, info.field_types) |name, FT| {
                @field(out, name) = try clone(FT, a, @field(v, name));
            }
            return out;
        },
        .@"union" => switch (v) {
            inline else => |payload, tag| {
                return @unionInit(T, @tagName(tag), try clone(@TypeOf(payload), a, payload));
            },
        },
        .optional => |o| return if (v) |x| try clone(o.child, a, x) else null,
        .pointer => |p| {
            if (p.size != .slice) return v;
            if (p.child == u8) return a.dupe(u8, v);
            const out = try a.alloc(p.child, v.len);
            for (v, out) |x, *o| o.* = try clone(p.child, a, x);
            return out;
        },
        else => return v,
    }
}

fn isArrayHashMap(comptime T: type) bool {
    return @hasDecl(T, "jsonParseFromValue") and @hasField(T, "map");
}

fn cloneMap(comptime T: type, a: Allocator, v: T) Allocator.Error!T {
    var out: T = .{};
    var it = v.map.iterator();
    while (it.next()) |e| {
        const V = @TypeOf(e.value_ptr.*);
        try out.map.put(a, try a.dupe(u8, e.key_ptr.*), try clone(V, a, e.value_ptr.*));
    }
    return out;
}
