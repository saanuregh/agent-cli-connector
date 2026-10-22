//! Creates providers by name, with options given as JSON.
const std = @import("std");
const agent = @import("root.zig");
const errors = agent.errors;
const RawJson = agent.json.RawJson;

const modules = .{ agent.claude, agent.codex, agent.pi, agent.fake };

/// The providers `create` knows, in the order `initialize` offers them.
pub const names = blk: {
    var list: []const []const u8 = &.{};
    for (modules) |m| if (@hasDecl(m, "create")) {
        list = list ++ .{m.name};
    };
    break :blk list;
};

/// The providers offered when the client names none: every real harness.
pub const default_names = blk: {
    var list: []const []const u8 = &.{};
    for (names) |n| if (!std.mem.eql(u8, n, agent.fake.name)) {
        list = list ++ .{n};
    };
    break :blk list;
};

/// Creates provider `name` with its `Options` parsed from `options`.
pub fn create(env: agent.Env, name: []const u8, options: ?RawJson, diag: ?*errors.Diagnostics) agent.Error!agent.Provider {
    var arena: std.heap.ArenaAllocator = .init(env.gpa);
    defer arena.deinit();
    inline for (modules) |m| if (comptime @hasDecl(m, "create")) {
        if (std.mem.eql(u8, name, m.name)) {
            const opts = try agent.providerkit.options(m.Options, arena.allocator(), m.name, options, diag);
            // Options are copied by the provider; the arena can go.
            return m.create(env, opts) catch |e| errors.fail(diag, .internal, "{s}: create provider: {t}", .{ m.name, e });
        }
    };
    return errors.fail(diag, .not_found, "unknown provider \"{s}\"", .{name});
}
