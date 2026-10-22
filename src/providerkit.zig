//! Small helpers the built-in providers share.
const std = @import("std");
const builtin = @import("builtin");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const types = @import("types.zig");
const errors = @import("errors.zig");
const json = @import("json.zig");
const process = @import("process.zig");
const Env = @import("runtime.zig").Env;
const runtime = @import("runtime.zig");
const Id = runtime.Id;
const RawJson = json.RawJson;
const Diagnostics = errors.Diagnostics;

/// Parses provider-specific options; null parses as the defaults.
pub fn options(comptime T: type, a: Allocator, provider: []const u8, raw: ?RawJson, diag: ?*Diagnostics) errors.Error!T {
    const r = raw orelse return .{};
    if (r.bytes.len == 0 or std.mem.eql(u8, r.bytes, "null")) return .{};
    return r.parse(T, a) catch |e| errors.fail(diag, .invalid_params, "{s}: invalid provider_options: {t}", .{ provider, e });
}

/// An inline image or file part's bytes as base64, reading `path` when
/// there are no inline bytes, and its media type.
pub fn readPart(env: Env, a: Allocator, part: types.Part, diag: ?*Diagnostics) errors.Error!struct { data: []const u8, media_type: []const u8 } {
    var media = part.media_type;
    if (media.len == 0 and part.path.len > 0) media = mediaType(part.path);
    if (part.data.len > 0) return .{ .data = part.data, .media_type = media };
    if (part.path.len == 0) return errors.fail(diag, .invalid_params, "{t} part has neither data nor path", .{part.type});
    const bytes = Io.Dir.cwd().readFileAlloc(env.io, part.path, a, .limited(64 << 20)) catch |e|
        return errors.fail(diag, .invalid_params, "read {s}: {t}", .{ part.path, e });
    const enc = std.base64.standard.Encoder;
    const out = try a.alloc(u8, enc.calcSize(bytes.len));
    return .{ .data = enc.encode(out, bytes), .media_type = media };
}

/// The media type for a file name's extension.
pub fn mediaType(path: []const u8) []const u8 {
    const ext = std.fs.path.extension(path);
    const table = [_]struct { []const u8, []const u8 }{
        .{ ".png", "image/png" },         .{ ".jpg", "image/jpeg" },  .{ ".jpeg", "image/jpeg" },
        .{ ".gif", "image/gif" },         .{ ".webp", "image/webp" }, .{ ".svg", "image/svg+xml" },
        .{ ".pdf", "application/pdf" },   .{ ".txt", "text/plain" },  .{ ".md", "text/markdown" },
        .{ ".json", "application/json" }, .{ ".html", "text/html" },  .{ ".csv", "text/csv" },
    };
    for (table) |e| if (std.ascii.eqlIgnoreCase(ext, e[0])) return e[1];
    return "application/octet-stream";
}

/// `cfg` with unset fields taken from the live session and then from the
/// session's original config, for forks.
pub fn inheritConfig(cfg: types.Config, base: types.Config, live: types.SessionInfo) types.Config {
    var out = cfg;
    out.cwd = firstOf(&.{ cfg.cwd, base.cwd, live.cwd });
    out.model = firstOf(&.{ cfg.model, live.model, base.model });
    out.effort = firstOf(&.{ cfg.effort, live.effort, base.effort });
    out.permission_mode = firstOf(&.{ cfg.permission_mode, live.permission_mode, base.permission_mode });
    out.system_prompt = firstOf(&.{ cfg.system_prompt, base.system_prompt });
    out.append_system_prompt = firstOf(&.{ cfg.append_system_prompt, base.append_system_prompt });
    if (cfg.output_schema == null) out.output_schema = base.output_schema;
    out.ephemeral = cfg.ephemeral or base.ephemeral;
    if (cfg.mcp_servers == null) out.mcp_servers = base.mcp_servers;
    if (cfg.tools.len == 0) out.tools = base.tools;
    if (cfg.allowed_tools.len == 0) out.allowed_tools = base.allowed_tools;
    if (cfg.disallowed_tools.len == 0) out.disallowed_tools = base.disallowed_tools;
    if (cfg.additional_directories.len == 0) out.additional_directories = base.additional_directories;
    if (cfg.env == null) out.env = base.env;
    if (cfg.unset_env.len == 0) out.unset_env = base.unset_env;
    if (cfg.event_buffer_limit == 0) out.event_buffer_limit = base.event_buffer_limit;
    out.wire_tap = cfg.wire_tap or base.wire_tap;
    out.include_raw = cfg.include_raw or base.include_raw;
    if (cfg.provider_options == null) out.provider_options = base.provider_options;
    return out;
}

/// The first non-empty string.
pub fn firstOf(list: []const []const u8) []const u8 {
    for (list) |s| if (s.len > 0) return s;
    return "";
}

/// For config fields the harness ignores: fails when `cfg.strict` is set,
/// otherwise returns the `config_ignored` notice to emit after
/// `session_started` (null when nothing is ignored).
pub fn ignoredConfig(a: Allocator, provider: []const u8, cfg: types.Config, ignored: []const []const u8, diag: ?*Diagnostics) errors.Error!?types.Event.Payload {
    if (ignored.len == 0) return null;
    const list = try std.mem.join(a, ", ", ignored);
    if (cfg.strict) return errors.fail(diag, .unsupported, "{s}: {s} not supported (config.strict is set)", .{ provider, list });
    return .{ .notice = .{
        .level = .warning,
        .code = "config_ignored",
        .message = try std.fmt.allocPrint(a, "{s} ignores {s}", .{ provider, list }),
    } };
}

/// Fails unless `mode` is empty or one of `supported`.
pub fn checkPermissionMode(provider: []const u8, mode: []const u8, supported: []const []const u8, diag: ?*Diagnostics) errors.Error!void {
    if (mode.len == 0) return;
    for (supported) |s| if (std.mem.eql(u8, s, mode)) return;
    return errors.fail(diag, .unsupported, "{s}: permission mode {s} is not supported", .{ provider, mode });
}

/// Runs `<exe> --version` and reports the installation. Harness problems
/// go into the status's error.
pub fn versionStatus(env: Env, a: Allocator, exe: []const u8, extra_env: ?types.StringMap) types.ProviderStatus {
    var diag: Diagnostics = .{};
    const out = process.output(env, a, &.{ exe, "--version" }, extra_env, &diag) catch |e| {
        return .{ .@"error" = @import("clone.zig").clone(types.ErrorInfo, a, Diagnostics.of(&diag, e)) catch .{ .kind = errors.toKind(e) } };
    };
    return .{ .installed = true, .version = parseVersion(out) };
}

/// The first "x.y.z" version in `s`, with any suffix such as "-beta.1".
pub fn parseVersion(s: []const u8) []const u8 {
    var i: usize = 0;
    while (i < s.len) : (i += 1) {
        if (!std.ascii.isDigit(s[i]) or (i > 0 and (std.ascii.isAlphanumeric(s[i - 1]) or s[i - 1] == '.'))) continue;
        var j = i;
        var dots: u8 = 0;
        while (j < s.len and (std.ascii.isDigit(s[j]) or (s[j] == '.' and j + 1 < s.len and std.ascii.isDigit(s[j + 1])))) : (j += 1) {
            if (s[j] == '.') dots += 1;
        }
        if (dots < 2) continue;
        while (j < s.len and (std.ascii.isAlphanumeric(s[j]) or s[j] == '.' or s[j] == '+' or s[j] == '-')) j += 1;
        return s[i..j];
    }
    return "";
}

/// Compares dotted versions numerically: <0, 0, or >0.
pub fn compareVersions(x: []const u8, y: []const u8) i32 {
    var xi = std.mem.tokenizeAny(u8, x, ".-+");
    var yi = std.mem.tokenizeAny(u8, y, ".-+");
    for (0..3) |_| {
        const xn = std.fmt.parseInt(u64, xi.next() orelse "0", 10) catch 0;
        const yn = std.fmt.parseInt(u64, yi.next() orelse "0", 10) catch 0;
        if (xn != yn) return if (xn < yn) -1 else 1;
    }
    return 0;
}

/// An environment variable of the process environment.
pub fn getEnv(env: Env, name: []const u8) ?[]const u8 {
    if (builtin.os.tag != .windows) return env.environ.getPosix(name);
    return windowsEnvironment(env).get(name);
}

var windows_env_mutex: Io.Mutex = .init;
var windows_env: ?std.process.Environ.Map = null;

/// Windows offers no borrowed view of the environment: copy it once.
fn windowsEnvironment(env: Env) *const std.process.Environ.Map {
    windows_env_mutex.lockUncancelable(env.io);
    defer windows_env_mutex.unlock(env.io);
    if (windows_env == null) windows_env = std.process.Environ.createMap(env.environ, std.heap.smp_allocator) catch .init(std.heap.smp_allocator);
    return &windows_env.?;
}

/// The user's home directory.
pub fn homeDir(env: Env, diag: ?*Diagnostics) errors.Error![]const u8 {
    return getEnv(env, "HOME") orelse getEnv(env, "USERPROFILE") orelse errors.fail(diag, .not_found, "no home directory (HOME is unset)", .{});
}

/// `<user cache>/agent-cli-connector/<sub>`, created with private
/// permissions.
pub fn cacheDir(env: Env, a: Allocator, sub: []const u8, diag: ?*Diagnostics) errors.Error![]const u8 {
    const base = if (getEnv(env, "XDG_CACHE_HOME")) |x| x else try std.fs.path.join(a, &.{ try homeDir(env, diag), ".cache" });
    const dir = try std.fs.path.join(a, &.{ base, "agent-cli-connector", sub });
    Io.Dir.cwd().createDirPath(env.io, dir) catch |e| return errors.fail(diag, .internal, "create cache directory {s}: {t}", .{ dir, e });
    return dir;
}

/// An absolute form of `dir`; empty means the current directory.
pub fn absDir(env: Env, a: Allocator, dir: []const u8) []const u8 {
    if (dir.len > 0 and std.fs.path.isAbsolute(dir)) return dir;
    const cwd = Io.Dir.cwd().realPathFileAlloc(env.io, ".", a) catch return dir;
    return if (dir.len == 0) cwd else std.fs.path.join(a, &.{ cwd, dir }) catch dir;
}

/// A string-keyed map that owns copies of its keys and frees its values.
/// A value is a string (owned), void, a pointer to a type with
/// `destroy(gpa)`, or a struct with `deinit(gpa)`. Insertion order is kept.
pub fn OwnedMap(comptime V: type) type {
    return struct {
        map: std.array_hash_map.String(V) = .empty,

        const Self = @This();

        pub fn get(self: *const Self, key: []const u8) ?V {
            return self.map.get(key);
        }

        pub fn getPtr(self: *const Self, key: []const u8) ?*V {
            return self.map.getPtr(key);
        }

        pub fn contains(self: *const Self, key: []const u8) bool {
            return self.map.contains(key);
        }

        pub fn count(self: *const Self) usize {
            return self.map.count();
        }

        pub fn keys(self: *const Self) []const []const u8 {
            return self.map.keys();
        }

        /// Stores `value`, taking ownership of it; a value already stored
        /// under `key` is freed.
        pub fn put(self: *Self, gpa: Allocator, key: []const u8, value: V) Allocator.Error!void {
            const gop = try self.map.getOrPut(gpa, key);
            if (gop.found_existing) {
                freeValue(gpa, gop.value_ptr);
            } else {
                gop.key_ptr.* = gpa.dupe(u8, key) catch |e| {
                    self.map.orderedRemoveAt(gop.index);
                    freeValueOf(gpa, value);
                    return e;
                };
            }
            gop.value_ptr.* = value;
        }

        /// Removes `key`, freeing its key and value.
        pub fn remove(self: *Self, gpa: Allocator, key: []const u8) void {
            const kv = self.map.fetchOrderedRemove(key) orelse return;
            gpa.free(kv.key);
            var v = kv.value;
            freeValue(gpa, &v);
        }

        /// Removes `key` and returns its value, which the caller now owns.
        pub fn take(self: *Self, gpa: Allocator, key: []const u8) ?V {
            const kv = self.map.fetchOrderedRemove(key) orelse return null;
            gpa.free(kv.key);
            return kv.value;
        }

        pub fn clear(self: *Self, gpa: Allocator) void {
            for (self.map.keys(), self.map.values()) |k, *v| {
                gpa.free(k);
                freeValue(gpa, v);
            }
            self.map.clearRetainingCapacity();
        }

        pub fn deinit(self: *Self, gpa: Allocator) void {
            self.clear(gpa);
            self.map.deinit(gpa);
        }

        /// Copies of the keys in sorted order, allocated in `a`.
        pub fn sortedKeys(self: *const Self, a: Allocator) Allocator.Error![]const []const u8 {
            const out = try a.alloc([]const u8, self.map.count());
            for (self.map.keys(), out) |k, *o| o.* = try a.dupe(u8, k);
            std.mem.sort([]const u8, out, {}, stringLessThan);
            return out;
        }

        /// Stores a copy of `value`: a string is duplicated, and an
        /// `*Owned(T)` value is created from a `T`.
        pub fn putCopy(self: *Self, gpa: Allocator, key: []const u8, value: anytype) Allocator.Error!void {
            const copy: V = if (V == []const u8 or V == []u8)
                try gpa.dupe(u8, value)
            else
                try @typeInfo(V).pointer.child.create(gpa, value);
            return self.put(gpa, key, copy);
        }

        fn freeValueOf(gpa: Allocator, value: V) void {
            var v = value;
            freeValue(gpa, &v);
        }

        fn freeValue(gpa: Allocator, v: *V) void {
            if (V == void) return;
            if (V == []const u8 or V == []u8) return gpa.free(v.*);
            switch (@typeInfo(V)) {
                .pointer => |p| if (p.size == .one) v.*.destroy(gpa),
                .@"struct" => v.deinit(gpa),
                else => {},
            }
        }
    };
}

/// A value deep-copied into its own arena, so it can be kept in a map and
/// freed alone.
pub fn Owned(comptime T: type) type {
    return struct {
        arena: std.heap.ArenaAllocator,
        value: T,

        const Self = @This();

        pub fn create(gpa: Allocator, value: T) Allocator.Error!*Self {
            const self = try gpa.create(Self);
            self.arena = .init(gpa);
            self.value = @import("clone.zig").clone(T, self.arena.allocator(), value) catch |e| {
                self.arena.deinit();
                gpa.destroy(self);
                return e;
            };
            return self;
        }

        pub fn destroy(self: *Self, gpa: Allocator) void {
            self.arena.deinit();
            gpa.destroy(self);
        }
    };
}

/// A set of owned strings in insertion order.
pub const StringSet = OwnedMap(void);

fn stringLessThan(_: void, x: []const u8, y: []const u8) bool {
    return std.mem.lessThan(u8, x, y);
}

const IdContext = struct {
    pub fn hash(_: IdContext, k: Id) u32 {
        return @truncate(std.hash.Wyhash.hash(0, k.slice()));
    }
    pub fn eql(_: IdContext, x: Id, y: Id, _: usize) bool {
        return std.mem.eql(u8, x.slice(), y.slice());
    }
};

/// An insertion-ordered map keyed by inline identifiers, so keys need no
/// allocation.
pub fn IdMap(comptime V: type) type {
    return std.ArrayHashMapUnmanaged(Id, V, IdContext, true);
}

pub const contains = runtime.contains;
pub const containsId = runtime.containsId;
pub const indexOfId = runtime.indexOfId;

pub fn eql(x: []const u8, y: []const u8) bool {
    return std.mem.eql(u8, x, y);
}

/// A sorted copy of `keys`; the strings are not copied.
pub fn sortedKeys(a: Allocator, keys: []const []const u8) Allocator.Error![]const []const u8 {
    const out = try a.dupe([]const u8, keys);
    std.mem.sort([]const u8, out, {}, stringLessThan);
    return out;
}

/// A timeout of `ms` milliseconds.
pub fn timeoutMs(ms: i64) Io.Timeout {
    return .{ .duration = .{ .raw = .fromMilliseconds(ms), .clock = .awake } };
}

/// Waits up to `ms` for `event`; whether it is set.
pub fn waitEvent(io: Io, event: *Io.Event, ms: i64) bool {
    event.waitTimeout(io, timeoutMs(ms)) catch {};
    return event.isSet();
}

/// The longest prefix of `s` that fits in `max_bytes` without cutting a
/// UTF-8 sequence.
pub fn utf8Prefix(s: []const u8, max_bytes: usize) []const u8 {
    if (s.len <= max_bytes) return s;
    var end = max_bytes;
    while (end > 0 and s[end] & 0xC0 == 0x80) end -= 1;
    return s[0..end];
}

/// The first `max_chars` characters of `s`.
pub fn truncateChars(s: []const u8, max_chars: usize) []const u8 {
    var chars: usize = 0;
    for (s, 0..) |c, i| {
        if (c & 0xC0 == 0x80) continue;
        if (chars == max_chars) return s[0..i];
        chars += 1;
    }
    return s;
}

/// Startup bound used when a provider's `startup_timeout_ms` is zero.
pub const default_startup_timeout_ms = 60_000;
/// Close grace used when a provider's `shutdown_grace_ms` is zero.
pub const default_shutdown_grace_ms = 5_000;

/// `value`, or `default` when it is zero.
pub fn orDefault(value: i64, default: i64) i64 {
    return if (value > 0) value else default;
}

/// `status` with the failure `err` recorded as its error.
pub fn failedStatus(a: Allocator, status: types.ProviderStatus, diag: ?*const Diagnostics, err: anyerror) types.ProviderStatus {
    var out = status;
    out.@"error" = @import("clone.zig").clone(types.ErrorInfo, a, Diagnostics.of(diag, err)) catch .{ .kind = errors.toKind(err) };
    return out;
}

/// A fresh private directory under the system's temporary directory,
/// named `<prefix>-<random>`. The caller deletes it.
pub fn tempDir(env: Env, a: Allocator, prefix: []const u8, diag: ?*Diagnostics) errors.Error![]const u8 {
    var random: [8]u8 = undefined;
    env.io.random(&random);
    const dir = try std.fmt.allocPrint(a, "{s}{c}{s}-{x}", .{ tempRoot(env), std.fs.path.sep, prefix, random });
    Io.Dir.cwd().createDirPath(env.io, dir) catch |e| return errors.fail(diag, .internal, "create {s}: {t}", .{ dir, e });
    return dir;
}

/// The system's temporary directory.
pub fn tempRoot(env: Env) []const u8 {
    return getEnv(env, "TMPDIR") orelse getEnv(env, "TEMP") orelse if (builtin.os.tag == .windows) "C:\\Windows\\Temp" else "/tmp";
}

/// A `StringMap` as a JSON object value.
pub fn stringMapValue(a: Allocator, map: types.StringMap) Allocator.Error!std.json.Value {
    var out: json.ObjectBuilder = .init(a);
    for (map.map.keys(), map.map.values()) |k, v| try out.str(k, v);
    return out.value();
}

/// Whether `s` has the canonical 8-4-4-4-12 hex UUID form.
pub fn isUuid(s: []const u8) bool {
    if (s.len != 36) return false;
    for (s, 0..) |c, i| switch (i) {
        8, 13, 18, 23 => if (c != '-') return false,
        else => if (!std.ascii.isHex(c)) return false,
    };
    return true;
}

/// A transcript file found when listing sessions.
pub const SessionFile = struct { path: []const u8, mtime_ms: i64 };

/// Pages session files newest first: `summarize` reads one file and returns
/// null to skip it.
pub fn pageSessionFiles(
    a: Allocator,
    files: []SessionFile,
    limit: u32,
    offset: u32,
    context: anytype,
    comptime summarize: fn (@TypeOf(context), Allocator, []const u8) ?types.SessionSummary,
) Allocator.Error![]const types.SessionSummary {
    std.mem.sort(SessionFile, files, {}, struct {
        fn newer(_: void, x: SessionFile, y: SessionFile) bool {
            return x.mtime_ms > y.mtime_ms;
        }
    }.newer);
    var out: std.ArrayList(types.SessionSummary) = .empty;
    var skipped: u32 = 0;
    for (files) |f| {
        if (limit > 0 and out.items.len >= limit) break;
        var sum = summarize(context, a, f.path) orelse continue;
        if (skipped < offset) {
            skipped += 1;
            continue;
        }
        if (sum.updated_at_ms == 0) sum.updated_at_ms = f.mtime_ms;
        try out.append(a, sum);
    }
    return out.toOwnedSlice(a);
}

/// The newest `limit` items (all when zero) after skipping `offset` of the
/// newest, in their original order.
pub fn lastPage(comptime T: type, items: []const T, limit: u32, offset: u32) []const T {
    const end = items.len -| offset;
    const start = if (limit > 0) end -| limit else 0;
    return items[start..end];
}

/// Parses an RFC 3339 timestamp into Unix milliseconds; 0 when invalid.
pub fn parseTimeMs(s: []const u8) i64 {
    // YYYY-MM-DDTHH:MM:SS[.fff][Z|±HH:MM]
    if (s.len < 19) return 0;
    const year = std.fmt.parseInt(i32, s[0..4], 10) catch return 0;
    const month = std.fmt.parseInt(u8, s[5..7], 10) catch return 0;
    const day = std.fmt.parseInt(u8, s[8..10], 10) catch return 0;
    const hour = std.fmt.parseInt(i64, s[11..13], 10) catch return 0;
    const minute = std.fmt.parseInt(i64, s[14..16], 10) catch return 0;
    const second = std.fmt.parseInt(i64, s[17..19], 10) catch return 0;
    var i: usize = 19;
    var ms: i64 = 0;
    if (i < s.len and s[i] == '.') {
        i += 1;
        var scale: i64 = 100;
        while (i < s.len and std.ascii.isDigit(s[i])) : (i += 1) {
            ms += @as(i64, s[i] - '0') * scale;
            scale = @divTrunc(scale, 10);
        }
    }
    var offset_min: i64 = 0;
    if (i + 6 <= s.len and (s[i] == '+' or s[i] == '-')) {
        const oh = std.fmt.parseInt(i64, s[i + 1 .. i + 3], 10) catch 0;
        const om = std.fmt.parseInt(i64, s[i + 4 .. i + 6], 10) catch 0;
        offset_min = (oh * 60 + om) * @as(i64, if (s[i] == '-') -1 else 1);
    }
    const days = daysFromCivil(year, month, day);
    return ((days * 86400 + hour * 3600 + minute * 60 + second - offset_min * 60) * 1000) + ms;
}

fn daysFromCivil(y_in: i32, m: u8, d: u8) i64 {
    const y: i64 = if (m <= 2) y_in - 1 else y_in;
    const era = @divFloor(y, 400);
    const yoe = y - era * 400;
    const mp: i64 = @mod(@as(i64, m) + 9, 12);
    const doy = @divFloor(153 * mp + 2, 5) + d - 1;
    const doe = yoe * 365 + @divFloor(yoe, 4) - @divFloor(yoe, 100) + doy;
    return era * 146097 + doe - 719468;
}

test "owned maps free what they hold" {
    const gpa = std.testing.allocator;
    var map: OwnedMap([]const u8) = .{};
    defer map.deinit(gpa);
    try map.put(gpa, "a", try gpa.dupe(u8, "one"));
    try map.put(gpa, "a", try gpa.dupe(u8, "two"));
    try std.testing.expectEqualStrings("two", map.get("a").?);
    try std.testing.expect(isUuid("0b4c3f0e-1d2a-4b5c-8d9e-0f1a2b3c4d5e"));
}

test "parseVersion and time" {
    try std.testing.expectEqualStrings("0.163.0", parseVersion("codex-cli 0.163.0"));
    try std.testing.expectEqualStrings("2.1.301", parseVersion("2.1.301 (Claude Code)"));
    try std.testing.expectEqual(@as(i64, 1700000000123), parseTimeMs("2023-11-14T22:13:20.123Z"));
    try std.testing.expect(compareVersions("2.1.10", "2.1.9") > 0);
}
