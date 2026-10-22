//! Generates the binding types (TypeScript, Go, Rust, and a JSON Schema) from
//! the Zig protocol types with comptime reflection. Run with `zig build
//! bindgen`; it writes every output into the directory given as the first
//! argument, and the build step copies them into the source tree.
//!
//! The wire rules mirror `json.write`: snake_case field names, fields at their
//! default value left out (so decoders default every missing field), enums as
//! tag names, `RawJson` as any JSON, and a struct whose `jsonStringify` writes
//! one union field flattened (such as `Event`) as one object tagged "type".
const std = @import("std");
const agent = @import("agent");
const types = agent.types;
const rpc = agent.rpc;
const RawJson = agent.json.RawJson;
const Writer = std.Io.Writer;

/// Provider-specific options, by provider: the module and the types sent
/// as `initialize.providers` (Options), `Config.provider_options`
/// (SessionOptions), and `Input.provider_options` (TurnOptions).
const provider_options = .{
    .{ "claude", @import("claude_options") },
    .{ "codex", @import("codex_options") },
    .{ "pi", @import("pi_options") },
};

/// The provider options types that exist, in provider order.
const option_types: []const type = blk: {
    var list: []const type = &.{};
    for (provider_options) |entry| {
        for (.{ "Options", "SessionOptions", "TurnOptions" }) |decl| {
            if (!@hasDecl(entry[1], decl)) continue;
            const T = @field(entry[1], decl);
            if (@typeInfo(T).@"struct".field_names.len > 0) list = list ++ [_]type{T};
        }
    }
    break :blk list;
};

// ---------------------------------------------------------------- roots

/// The protocol types, in `rpc.zig` order. Everything they reference is
/// exported too.
const roots = [_]type{
    rpc.InitializeParams,   rpc.InitializeResult,  rpc.ProviderInfo,        rpc.ProviderParams,
    rpc.ListSessionsParams, rpc.ReadHistoryParams, rpc.DeleteSessionParams, rpc.ListSubagentsParams,
    rpc.StartParams,        rpc.ForkParams,        rpc.StartResult,         rpc.SessionParams,
    rpc.SendParams,         rpc.SendResult,        rpc.WaitParams,          rpc.TurnParams,
    rpc.ValueParams,        rpc.CompactParams,     rpc.StopTaskParams,      rpc.CallParams,
    rpc.RespondParams,      rpc.EventNotification, rpc.ClosedNotification,  rpc.ToolCallParams,
    types.Event,            types.ToolResult,
} ++ option_types;

const Method = struct {
    name: []const u8,
    Params: type,
    Result: type,
    /// What the method does, as a sentence starting with a verb, for the
    /// doc comment of a generated binding method.
    doc: []const u8 = "",
};

/// Client requests. `void` results are JSON null.
const methods = [_]Method{
    .{ .name = "initialize", .Params = rpc.InitializeParams, .Result = rpc.InitializeResult },
    .{ .name = "provider/check", .Params = rpc.ProviderParams, .Result = types.ProviderStatus, .doc = "Reports whether the provider's harness is installed and signed in, without starting a session. Problems with the harness are in the status's `error`; this fails only when the check could not run." },
    .{ .name = "provider/listModels", .Params = rpc.ProviderParams, .Result = []const types.Model, .doc = "Lists the models the provider's harness offers." },
    .{ .name = "provider/listSessions", .Params = rpc.ListSessionsParams, .Result = []const types.SessionSummary, .doc = "Lists the stored sessions, newest first." },
    .{ .name = "provider/readHistory", .Params = rpc.ReadHistoryParams, .Result = []const types.Item, .doc = "Reads the transcript of a stored session without starting it. Offsets count back from the newest item." },
    .{ .name = "provider/deleteSession", .Params = rpc.DeleteSessionParams, .Result = void, .doc = "Deletes a stored session. It cannot be undone. An empty cwd searches everywhere." },
    .{ .name = "provider/listSubagents", .Params = rpc.ListSubagentsParams, .Result = []const types.Task, .doc = "Lists the subagents a stored session ran. An empty cwd searches everywhere." },
    .{ .name = "session/start", .Params = rpc.StartParams, .Result = rpc.StartResult },
    .{ .name = "session/fork", .Params = rpc.ForkParams, .Result = rpc.StartResult },
    .{ .name = "session/send", .Params = rpc.SendParams, .Result = rpc.SendResult },
    .{ .name = "session/wait", .Params = rpc.WaitParams, .Result = types.TurnResult },
    .{ .name = "session/info", .Params = rpc.SessionParams, .Result = types.SessionInfo, .doc = "Asks the session for its metadata." },
    .{ .name = "session/interrupt", .Params = rpc.SessionParams, .Result = void, .doc = "Stops the running turn. Queued input still runs afterwards." },
    .{ .name = "session/interruptTurn", .Params = rpc.TurnParams, .Result = void, .doc = "Stops one turn: a queued turn is dropped and a running turn is interrupted. A turn that already ended is not an error." },
    .{ .name = "session/setModel", .Params = rpc.ValueParams, .Result = void, .doc = "Switches the model for later requests." },
    .{ .name = "session/setEffort", .Params = rpc.ValueParams, .Result = void, .doc = "Changes the reasoning effort, such as `high`." },
    .{ .name = "session/setPermissionMode", .Params = rpc.ValueParams, .Result = void, .doc = "Changes how tool calls are approved, such as `accept_edits`." },
    .{ .name = "session/setTitle", .Params = rpc.ValueParams, .Result = void, .doc = "Renames the session." },
    .{ .name = "session/compact", .Params = rpc.CompactParams, .Result = void, .doc = "Compacts the context and returns after compaction finished. Instructions guide the summary where the harness supports it; empty gives none." },
    .{ .name = "session/rewind", .Params = rpc.TurnParams, .Result = void, .doc = "Drops a turn and every later turn from the conversation. File changes are not undone." },
    .{ .name = "session/stopTask", .Params = rpc.StopTaskParams, .Result = void, .doc = "Stops one background task, such as a subagent, without interrupting the turn." },
    .{ .name = "session/rateLimits", .Params = rpc.SessionParams, .Result = []const types.RateLimit, .doc = "Reads the account's plan usage windows." },
    .{ .name = "session/pendingRequests", .Params = rpc.SessionParams, .Result = []const types.Event, .doc = "Lists the request events still waiting for an answer, in arrival order." },
    .{ .name = "session/call", .Params = rpc.CallParams, .Result = ?RawJson, .doc = "Sends a native request and returns the harness's answer: a Claude control request subtype, a Codex app-server method, or a Pi command. Reaches every harness feature, including ones newer than this library." },
    .{ .name = "session/close", .Params = rpc.SessionParams, .Result = void },
    .{ .name = "request/respond", .Params = rpc.RespondParams, .Result = void },
};

const Notification = struct { name: []const u8, Params: type };

/// Notifications from the library.
const notifications = [_]Notification{
    .{ .name = "session/event", .Params = rpc.EventNotification },
    .{ .name = "session/closed", .Params = rpc.ClosedNotification },
    .{ .name = "tool/cancel", .Params = rpc.ToolCancelParams },
};

/// Requests from the library to the client.
const reverse_requests = [_]Method{
    .{ .name = "tool/call", .Params = rpc.ToolCallParams, .Result = types.ToolResult },
};

/// Namespaces of named string values, exported as constants. The fields that
/// take them stay plain strings, because other harness-native values pass
/// through.
const constant_groups = [_]type{ types.effort, types.permission_mode };

/// The constant group of a field holding a named string value (or a list
/// of them), found by field name: the Zig types are plain strings.
fn stringGroup(comptime f: Field) ?type {
    const string_fields = .{
        .{ "effort", types.effort },                    .{ "default_effort", types.effort },
        .{ "efforts", types.effort },                   .{ "permission_mode", types.permission_mode },
        .{ "permission_modes", types.permission_mode },
    };
    inline for (string_fields) |entry| {
        if (comptime eql(f.name, entry[0])) {
            if (f.T != []const u8 and f.T != []const []const u8) @compileError("bindgen: field " ++ f.name ++ " is not a string");
            return entry[1];
        }
    }
    return null;
}

fn isList(comptime f: Field) bool {
    return f.T == []const []const u8;
}

/// The group's namespace name, such as `permission_mode`.
fn groupName(comptime G: type) []const u8 {
    return typeName(G, "");
}

fn groupDoc(comptime G: type) []const u8 {
    return "Known `" ++ groupName(G) ++ "` values; the field takes any string.";
}

/// The variant of a flattened union's payload enum for a type this binding
/// does not know.
const unknown_variant = "Unknown";

const header = "Code generated by `zig build bindgen` from the Zig protocol types. DO NOT EDIT.";

// ---------------------------------------------------------------- type collection

const Kind = enum {
    /// A struct written as an object.
    record,
    /// A string enum.
    enumeration,
    /// A struct with one union field written flattened: the union's tag as
    /// "type", its payload fields, and the struct's other fields.
    flattened,
    /// Another name for `T`, for a flattened union variant whose payload
    /// type is shared or named differently.
    alias,
};

const Def = struct { name: []const u8, T: type, kind: Kind };

const defs: []const Def = blk: {
    @setEvalBranchQuota(200_000);
    var list: []const Def = &.{};
    for (roots) |T| list = visit(list, T, "");
    for (methods ++ reverse_requests) |m| {
        list = visit(list, m.Params, "");
        list = visit(list, m.Result, "");
    }
    for (notifications) |n| list = visit(list, n.Params, "");
    checkNames(list);
    break :blk list;
};

/// The types a client writes: request params and the results it returns.
const input_defs: []const Def = blk: {
    @setEvalBranchQuota(200_000);
    var list: []const Def = &.{};
    for (methods) |m| list = visit(list, m.Params, "");
    for (reverse_requests) |m| list = visit(list, m.Result, "");
    for (option_types) |T| list = visit(list, T, "");
    break :blk list;
};

fn visit(comptime list: []const Def, comptime T: type, comptime context: []const u8) []const Def {
    if (T == void or T == RawJson) return list;
    switch (@typeInfo(T)) {
        .optional => |o| return visit(list, o.child, context),
        .pointer => |p| {
            if (p.size != .slice) @compileError("bindgen: unsupported pointer " ++ @typeName(T));
            return if (p.child == u8) list else visit(list, p.child, context);
        },
        .bool, .int, .float => return list,
        .@"enum" => return if (find(list, T) != null) list else list ++ [_]Def{.{ .name = typeName(T, context), .T = T, .kind = .enumeration }},
        .@"struct" => |info| {
            if (isMap(T)) return visit(list, MapValue(T), context);
            if (find(list, T) != null) return list;
            const name = typeName(T, context);
            var out = list;
            if (flattenedField(T)) |union_name| {
                out = out ++ [_]Def{.{ .name = name, .T = T, .kind = .flattened }};
                for (info.field_names, info.field_types) |f, FT| {
                    if (!eql(f, union_name)) out = visit(out, FT, name ++ pascal(f));
                }
                const U = @FieldType(T, union_name);
                for (@typeInfo(U).@"union".field_names, @typeInfo(U).@"union".field_types) |v, P| {
                    if (@typeInfo(P) != .@"struct" or isMap(P) or P == RawJson)
                        @compileError("bindgen: flattened variant " ++ v ++ " must be a struct");
                    // Every variant's type is named after the variant: an
                    // anonymous payload directly, a named payload with another
                    // name (such as a shared `Delta`) through an alias.
                    out = visit(out, P, pascal(v));
                    if (!eql(nameIn(out, P), pascal(v))) out = out ++ [_]Def{.{ .name = pascal(v), .T = P, .kind = .alias }};
                }
                return out;
            }
            out = out ++ [_]Def{.{ .name = name, .T = T, .kind = .record }};
            for (info.field_names, info.field_types) |f, FT| out = visit(out, FT, name ++ pascal(f));
            return out;
        },
        .@"union" => @compileError("bindgen: a union must be the one union field of a struct with jsonStringify: " ++ @typeName(T)),
        else => @compileError("bindgen: unsupported type " ++ @typeName(T)),
    }
}

fn find(comptime list: []const Def, comptime T: type) ?usize {
    @setEvalBranchQuota(1_000_000);
    for (list, 0..) |d, i| if (d.T == T and d.kind != .alias) return i;
    return null;
}

fn nameIn(comptime list: []const Def, comptime T: type) []const u8 {
    return list[find(list, T) orelse @compileError("bindgen: no definition for " ++ @typeName(T))].name;
}

/// The exported name of a collected struct or enum.
fn nameOf(comptime T: type) []const u8 {
    return nameIn(defs, T);
}

/// The last segment of a named type's name; anonymous types take `context`
/// (their parent's name and field, or their union variant).
fn typeName(comptime T: type, comptime context: []const u8) []const u8 {
    const full = @typeName(T);
    if (isAnonymous(T)) {
        if (context.len == 0) @compileError("bindgen: anonymous root type " ++ full);
        return context;
    }
    const dot = std.mem.lastIndexOfScalar(u8, full, '.') orelse return full;
    // Types of a provider's options file take the provider's name, such as
    // ClaudeSessionOptions.
    for (provider_options) |entry| {
        for (@typeInfo(entry[1]).@"struct".decl_names) |decl| {
            if (@TypeOf(@field(entry[1], decl)) == type and @field(entry[1], decl) == T) return pascal(entry[0]) ++ decl;
        }
    }
    return full[dot + 1 ..];
}

fn isAnonymous(comptime T: type) bool {
    const full = @typeName(T);
    return std.mem.indexOf(u8, full, "__struct_") != null or std.mem.indexOf(u8, full, "__enum_") != null or
        std.mem.indexOf(u8, full, "__union_") != null;
}

fn isMap(comptime T: type) bool {
    return @typeInfo(T) == .@"struct" and @hasDecl(T, "jsonParseFromValue") and @hasField(T, "map");
}

fn MapValue(comptime T: type) type {
    return @FieldType(@FieldType(T, "map").KV, "value");
}

/// A plain struct: not a map, not raw JSON.
fn isRecord(comptime T: type) bool {
    return @typeInfo(T) == .@"struct" and !isMap(T) and T != RawJson;
}

/// The union field of a struct written with `json.writeFlattened`.
fn flattenedField(comptime T: type) ?[]const u8 {
    if (!std.meta.hasFn(T, "jsonStringify")) return null;
    const info = @typeInfo(T).@"struct";
    var found: ?[]const u8 = null;
    for (info.field_names, info.field_types) |f, FT| {
        if (@typeInfo(FT) != .@"union") continue;
        if (found != null) @compileError("bindgen: more than one union field in " ++ @typeName(T));
        found = f;
    }
    return found orelse @compileError("bindgen: custom jsonStringify without a union field in " ++ @typeName(T));
}

fn checkNames(comptime list: []const Def) void {
    @setEvalBranchQuota(1_000_000);
    var names: []const []const u8 = &.{};
    for (constant_groups) |G| names = names ++ [_][]const u8{pascal(groupName(G))};
    for (list) |d| {
        names = names ++ [_][]const u8{d.name};
        if (d.kind == .flattened) names = names ++ [_][]const u8{ d.name ++ "Meta", d.name ++ "Type", d.name ++ "Payload" };
        if (d.kind == .flattened) {
            const union_name = flattenedField(d.T).?;
            const meta = @typeInfo(d.T).@"struct".field_names;
            for (meta) |m| if (eql(m, "type") and !eql(m, union_name)) @compileError("bindgen: field \"type\" clashes with the tag in " ++ d.name);
            const U = @FieldType(d.T, union_name);
            for (@typeInfo(U).@"union".field_names) |v| if (eql(pascal(v), unknown_variant)) @compileError("bindgen: variant " ++ v ++ " clashes with the fallback variant in " ++ d.name);
            for (@typeInfo(U).@"union".field_types) |P| {
                for (@typeInfo(P).@"struct".field_names) |pf| {
                    if (eql(pf, "type")) @compileError("bindgen: payload field \"type\" clashes with the tag in " ++ d.name);
                    for (meta) |m| if (eql(m, pf)) @compileError("bindgen: payload field " ++ pf ++ " clashes with a field of " ++ d.name);
                }
            }
        }
    }
    for (names, 0..) |a, i| for (names[i + 1 ..]) |b| if (eql(a, b)) @compileError("bindgen: two types named " ++ a);
}

// ---------------------------------------------------------------- fields and defaults

const Field = struct { name: []const u8, T: type, default: ?*const anyopaque };

/// The fields of `T` except `skip`.
fn fieldsOf(comptime T: type, comptime skip: ?[]const u8) []const Field {
    @setEvalBranchQuota(1_000_000);
    const info = @typeInfo(T).@"struct";
    var out: []const Field = &.{};
    for (info.field_names, info.field_types, info.field_attrs) |n, FT, attrs| {
        if (skip) |s| if (eql(s, n)) continue;
        out = out ++ [_]Field{.{ .name = n, .T = FT, .default = attrs.default_value_ptr }};
    }
    return out;
}

/// The fields of a definition: the meta fields for a flattened struct.
fn defFields(comptime d: Def) []const Field {
    return fieldsOf(d.T, if (d.kind == .flattened) flattenedField(d.T) else null);
}

fn defaultValue(comptime f: Field) ?f.T {
    const p = f.default orelse return null;
    return @as(*const f.T, @ptrCast(@alignCast(p))).*;
}

/// A default worth stating: a non-empty string, a non-zero number, true, or
/// an enum tag. Empty values, null, and struct defaults are not shown.
fn visibleDefault(comptime f: Field) bool {
    const v = defaultValue(f) orelse return false;
    return switch (@typeInfo(f.T)) {
        .pointer => |p| blk: {
            if (v.len != 0 and p.child != u8) @compileError("bindgen: non-empty slice default in field " ++ f.name);
            break :blk v.len != 0;
        },
        .optional => if (v != null) @compileError("bindgen: non-null optional default in field " ++ f.name) else false,
        .bool => v,
        .int, .float => v != 0,
        .@"enum" => true,
        else => false,
    };
}

/// The visible default as JSON text.
fn jsonDefault(comptime f: Field) []const u8 {
    const v = defaultValue(f).?;
    return switch (@typeInfo(f.T)) {
        .pointer => quote(v),
        .@"enum" => quote(@tagName(v)),
        .bool => "true",
        else => std.fmt.comptimePrint("{d}", .{v}),
    };
}

fn quote(comptime s: []const u8) []const u8 {
    @setEvalBranchQuota(1_000_000);
    for (s) |c| if (c == '"' or c == '\\' or c < 0x20 or c > 0x7e) @compileError("bindgen: default needs escaping: " ++ s);
    return "\"" ++ s ++ "\"";
}

/// The tag a Rust enum's `#[default]` marks: the first default any field
/// gives it, else its first tag.
fn enumDefaultTag(comptime E: type) []const u8 {
    @setEvalBranchQuota(1_000_000);
    for (defs) |d| {
        if (d.kind != .record and d.kind != .flattened) continue;
        for (defFields(d)) |f| {
            if (f.T == E) if (defaultValue(f)) |v| return @tagName(v);
        }
    }
    return @typeInfo(E).@"enum".field_names[0];
}

// ---------------------------------------------------------------- names

fn eql(comptime a: []const u8, comptime b: []const u8) bool {
    return std.mem.eql(u8, a, b);
}

/// `read_only` -> `readOnly`.
fn camel(comptime s: []const u8) []const u8 {
    comptime {
        const p = pascal(s);
        return [_]u8{std.ascii.toLower(p[0])} ++ p[1..];
    }
}

/// `text_delta` -> `TextDelta`, `session/listModels` -> `SessionListModels`.
fn pascal(comptime s: []const u8) []const u8 {
    @setEvalBranchQuota(1_000_000);
    var out: []const u8 = "";
    var upper = true;
    for (s) |c| {
        if (c == '_' or c == '/') {
            upper = true;
            continue;
        }
        out = out ++ [_]u8{if (upper) std.ascii.toUpper(c) else c};
        upper = false;
    }
    return out;
}

const go_initialisms = [_][]const u8{ "id", "url", "uri", "http", "json", "api", "ui", "usd" };

/// `session_id` -> `SessionID`.
fn goName(comptime s: []const u8) []const u8 {
    @setEvalBranchQuota(1_000_000);
    var out: []const u8 = "";
    var it = std.mem.splitScalar(u8, s, '_');
    while (it.next()) |part| {
        if (part.len == 0) continue;
        const initialism = for (go_initialisms) |i| {
            if (eql(i, part)) break true;
        } else false;
        if (initialism) {
            for (part) |c| out = out ++ [_]u8{std.ascii.toUpper(c)};
        } else out = out ++ [_]u8{std.ascii.toUpper(part[0])} ++ part[1..];
    }
    return out;
}

/// `session/listModels` -> `SESSION_LIST_MODELS`.
fn screamingSnake(comptime s: []const u8) []const u8 {
    @setEvalBranchQuota(1_000_000);
    var out: []const u8 = "";
    for (s, 0..) |c, i| {
        if (c == '/' or c == '_') {
            out = out ++ "_";
        } else if (std.ascii.isUpper(c) and i > 0 and std.ascii.isLower(s[i - 1])) {
            out = out ++ [_]u8{ '_', c };
        } else out = out ++ [_]u8{std.ascii.toUpper(c)};
    }
    return out;
}

/// serde's `rename_all = "snake_case"` of a PascalCase variant.
fn serdeSnake(comptime s: []const u8) []const u8 {
    @setEvalBranchQuota(1_000_000);
    var out: []const u8 = "";
    for (s, 0..) |c, i| {
        if (std.ascii.isUpper(c)) {
            if (i > 0) out = out ++ "_";
            out = out ++ [_]u8{std.ascii.toLower(c)};
        } else out = out ++ [_]u8{c};
    }
    return out;
}

const rust_keywords = [_][]const u8{
    "as",       "async",  "await",  "break",   "const",   "continue", "crate",  "dyn",    "else",  "enum",  "extern",
    "false",    "fn",     "for",    "if",      "impl",    "in",       "let",    "loop",   "match", "mod",   "move",
    "mut",      "pub",    "ref",    "return",  "self",    "Self",     "static", "struct", "super", "trait", "true",
    "type",     "unsafe", "use",    "where",   "while",   "abstract", "become", "box",    "do",    "final", "macro",
    "override", "priv",   "typeof", "unsized", "virtual", "yield",    "try",    "gen",
};

fn rustField(comptime s: []const u8) []const u8 {
    @setEvalBranchQuota(1_000_000);
    for (rust_keywords) |k| if (eql(k, s)) return "r#" ++ s;
    return s;
}

fn Union(comptime d: Def) type {
    return @FieldType(d.T, flattenedField(d.T).?);
}

// ---------------------------------------------------------------- binding methods

const Lang = enum { go, rust, typescript };

/// A binding method whose body only builds the params and sends the request:
/// `provider/*` methods on the client, `session/*` methods on the session.
/// Its arguments are the params fields after `session`, in order.
const Wrapper = struct {
    method: []const u8,
    /// Arguments named differently from their field, as field and name.
    renames: []const [2][]const u8 = &.{},
    /// Bindings whose method of this name does more, so it is written by hand.
    hand_written: []const Lang = &.{},
};

const wrappers = [_]Wrapper{
    .{ .method = "provider/check" },
    .{ .method = "provider/listModels" },
    .{ .method = "provider/listSessions" },
    .{ .method = "provider/readHistory" },
    .{ .method = "provider/deleteSession" },
    .{ .method = "provider/listSubagents" },
    // Go's FetchInfo and TypeScript's info also keep the info they read.
    .{ .method = "session/info", .hand_written = &.{ .go, .typescript } },
    .{ .method = "session/interrupt" },
    .{ .method = "session/interruptTurn" },
    .{ .method = "session/setModel", .renames = &.{.{ "value", "model" }} },
    .{ .method = "session/setEffort", .renames = &.{.{ "value", "effort" }} },
    .{ .method = "session/setPermissionMode", .renames = &.{.{ "value", "permission_mode" }} },
    .{ .method = "session/setTitle", .renames = &.{.{ "value", "title" }} },
    .{ .method = "session/compact" },
    .{ .method = "session/rewind" },
    .{ .method = "session/stopTask" },
    .{ .method = "session/rateLimits" },
    // TypeScript adds the answering helpers to the events.
    .{ .method = "session/pendingRequests", .hand_written = &.{.typescript} },
    .{ .method = "session/call" },
};

/// A session method that answers a request with one kind of response: its
/// arguments are the request ID, then the response fields of the same name.
/// Generated for Go and Rust; TypeScript puts these helpers on the events.
const Answer = struct {
    name: []const u8,
    doc: []const u8,
    Response: type,
    /// Response fields set to an enum tag, as field and tag.
    tags: []const [2][]const u8 = &.{},
    args: []const []const u8 = &.{},
};

const answers = [_]Answer{
    .{ .name = "allow", .doc = "Allows a permission request once.", .Response = types.PermissionResponse, .tags = &.{.{ "decision", "allow" }} },
    .{ .name = "deny", .doc = "Denies a permission request; the message tells the model why.", .Response = types.PermissionResponse, .tags = &.{.{ "decision", "deny" }}, .args = &.{"message"} },
    .{ .name = "answer", .doc = "Answers a user input request: question IDs to the chosen option labels or free text.", .Response = types.UserInputResponse, .args = &.{"answers"} },
    .{ .name = "accept", .doc = "Accepts an elicitation with its form content.", .Response = types.ElicitationResponse, .tags = &.{.{ "action", "accept" }}, .args = &.{"content"} },
    .{ .name = "decline", .doc = "Declines an elicitation.", .Response = types.ElicitationResponse, .tags = &.{.{ "action", "decline" }} },
};

/// One argument of a generated method and the field it sets.
const Arg = struct {
    field: []const u8,
    name: []const u8,
    T: type,
    /// The field has a default, so the request may leave it out.
    optional: bool,
};

fn methodNamed(comptime name: []const u8) Method {
    for (methods) |m| if (eql(m.name, name)) return m;
    @compileError("bindgen: no method " ++ name);
}

fn onSession(comptime w: Wrapper) bool {
    return std.mem.startsWith(u8, w.method, "session/");
}

fn generatedFor(comptime w: Wrapper, comptime lang: Lang) bool {
    for (w.hand_written) |l| if (l == lang) return false;
    return true;
}

/// The method name after its namespace, such as `setModel`.
fn baseName(comptime w: Wrapper) []const u8 {
    const slash = std.mem.indexOfScalar(u8, w.method, '/') orelse @compileError("bindgen: no namespace in " ++ w.method);
    return w.method[slash + 1 ..];
}

fn wrapperDoc(comptime w: Wrapper) []const u8 {
    const doc = methodNamed(w.method).doc;
    if (doc.len == 0) @compileError("bindgen: method " ++ w.method ++ " has no doc");
    return doc;
}

fn wrapperArgs(comptime w: Wrapper) []const Arg {
    @setEvalBranchQuota(1_000_000);
    const fields = fieldsOf(methodNamed(w.method).Params, null);
    if (onSession(w) and (fields.len == 0 or !eql(fields[0].name, "session")))
        @compileError("bindgen: the params of " ++ w.method ++ " do not start with session");
    for (w.renames) |r| {
        for (fields) |f| {
            if (eql(f.name, r[0])) break;
        } else @compileError("bindgen: no field " ++ r[0] ++ " to rename in " ++ w.method);
    }
    var out: []const Arg = &.{};
    for (fields[if (onSession(w)) 1 else 0..]) |f| {
        var name = f.name;
        for (w.renames) |r| if (eql(r[0], f.name)) {
            name = r[1];
        };
        out = out ++ [_]Arg{.{ .field = f.name, .name = name, .T = f.T, .optional = f.default != null }};
    }
    return out;
}

fn answerArgs(comptime a: Answer) []const Arg {
    var out: []const Arg = &.{};
    for (a.args) |n| out = out ++ [_]Arg{.{ .field = n, .name = n, .T = @FieldType(a.Response, n), .optional = false }};
    return out;
}

/// The argument as a field, so a named string value takes its group's type.
fn argField(comptime a: Arg) Field {
    return .{ .name = a.name, .T = a.T, .default = null };
}

/// An optional raw JSON argument: any value, where null sends none.
fn isRawArg(comptime a: Arg) bool {
    if (a.T == RawJson) @compileError("bindgen: required raw JSON argument " ++ a.name);
    return a.T == ?RawJson;
}

fn lowerFirst(comptime s: []const u8) []const u8 {
    return [_]u8{std.ascii.toLower(s[0])} ++ s[1..];
}

fn upperFirst(comptime s: []const u8) []const u8 {
    return [_]u8{std.ascii.toUpper(s[0])} ++ s[1..];
}

fn join(comptime parts: []const []const u8, comptime separator: []const u8) []const u8 {
    var out: []const u8 = "";
    for (parts, 0..) |p, i| out = out ++ (if (i > 0) separator else "") ++ p;
    return out;
}

/// `text` as comment lines of at most `width` columns, each starting with
/// `prefix`.
fn commentLines(comptime prefix: []const u8, comptime text: []const u8, comptime width: usize) []const u8 {
    @setEvalBranchQuota(1_000_000);
    var out: []const u8 = "";
    var line: []const u8 = prefix;
    var empty = true;
    var words = std.mem.tokenizeScalar(u8, text, ' ');
    while (words.next()) |word| {
        if (!empty and line.len + 1 + word.len > width) {
            out = out ++ line ++ "\n";
            line = prefix;
            empty = true;
        }
        line = line ++ (if (empty) "" else " ") ++ word;
        empty = false;
    }
    return out ++ line ++ "\n";
}

const comment_width = 80;

// ---------------------------------------------------------------- Go methods

fn goLocal(comptime name: []const u8) []const u8 {
    return lowerFirst(goName(name));
}

fn goArgType(comptime a: Arg) []const u8 {
    if (isRawArg(a)) return "any";
    if (stringGroup(argField(a))) |G| return goName(groupName(G));
    return goRef(a.T);
}

fn goArgExpr(comptime a: Arg) []const u8 {
    if (isRawArg(a)) return goLocal(a.name) ++ "JSON";
    if (stringGroup(argField(a)) != null) return "string(" ++ goLocal(a.name) ++ ")";
    return goLocal(a.name);
}

fn goParamList(comptime args: []const Arg) []const u8 {
    var out: []const u8 = "ctx context.Context";
    for (args) |a| out = out ++ ", " ++ goLocal(a.name) ++ " " ++ goArgType(a);
    return out;
}

/// The struct literal of type `name` setting `inits` and the arguments.
fn goLiteral(comptime name: []const u8, comptime inits: []const []const u8, comptime args: []const Arg) []const u8 {
    var parts: []const []const u8 = inits;
    for (args) |a| parts = parts ++ [_][]const u8{goName(a.field) ++ ": " ++ goArgExpr(a)};
    return name ++ "{" ++ join(parts, ", ") ++ "}";
}

/// Encodes the raw JSON arguments; the method returns only an error.
fn goRawPrelude(comptime args: []const Arg) []const u8 {
    var out: []const u8 = "";
    for (args) |a| if (isRawArg(a)) {
        out = out ++ "\t" ++ goArgExpr(a) ++ ", err := marshalRaw(" ++ goLocal(a.name) ++ ")\n\tif err != nil {\n\t\treturn err\n\t}\n";
    };
    return out;
}

fn goRawNotes(comptime args: []const Arg) []const u8 {
    var out: []const u8 = "";
    for (args) |a| if (isRawArg(a)) {
        out = out ++ " " ++ goLocal(a.name) ++ " is any JSON value; nil sends none.";
    };
    return out;
}

fn goWrapper(comptime w: Wrapper) []const u8 {
    @setEvalBranchQuota(1_000_000);
    const m = methodNamed(w.method);
    const args = wrapperArgs(w);
    const name = upperFirst(baseName(w));
    const receiver = if (onSession(w)) "s *Session" else "c *Client";
    const call = if (onSession(w)) "s.client.call" else "c.call";
    const params = goLiteral(nameOf(m.Params), if (onSession(w)) &.{"Session: s.handle"} else &.{}, args);
    const request = call ++ "(ctx, Method" ++ pascal(m.name) ++ ", " ++ params;
    const prelude = goRawPrelude(args);
    var doc: []const u8 = wrapperDoc(w) ++ goRawNotes(args);
    var param_list: []const u8 = goParamList(args);
    var result: []const u8 = undefined;
    var body: []const u8 = undefined;
    if (m.Result == void) {
        result = "error";
        body = "\treturn " ++ request ++ ", nil)\n";
    } else if (m.Result == ?RawJson) {
        doc = doc ++ " The result is decoded into result; nil discards it.";
        param_list = param_list ++ ", result any";
        result = "error";
        body = "\treturn " ++ request ++ ", result)\n";
    } else if (prelude.len > 0) {
        @compileError("bindgen: Go wrapper " ++ w.method ++ " encodes JSON but returns a value");
    } else if (isRecord(m.Result)) {
        result = "(*" ++ goRef(m.Result) ++ ", error)";
        body = "\tvar out " ++ goRef(m.Result) ++ "\n\tif err := " ++ request ++ ", &out); err != nil {\n\t\treturn nil, err\n\t}\n\treturn &out, nil\n";
    } else {
        result = "(" ++ goRef(m.Result) ++ ", error)";
        body = "\tvar out " ++ goRef(m.Result) ++ "\n\terr := " ++ request ++ ", &out)\n\treturn out, err\n";
    }
    return commentLines("// ", name ++ " " ++ lowerFirst(doc), comment_width) ++
        "func (" ++ receiver ++ ") " ++ name ++ "(" ++ param_list ++ ") " ++ result ++ " {\n" ++ prelude ++ body ++ "}\n";
}

fn goTags(comptime a: Answer) []const []const u8 {
    var out: []const []const u8 = &.{};
    for (a.tags) |t| out = out ++ [_][]const u8{goName(t[0]) ++ ": " ++ goEnumConst(@FieldType(a.Response, t[0]), t[1])};
    return out;
}

fn goAnswer(comptime a: Answer) []const u8 {
    @setEvalBranchQuota(1_000_000);
    const args = answerArgs(a);
    const name = upperFirst(a.name);
    const response = goLiteral(nameOf(a.Response), goTags(a), args);
    return commentLines("// ", name ++ " " ++ lowerFirst(a.doc ++ goRawNotes(args)), comment_width) ++
        "func (s *Session) " ++ name ++ "(ctx context.Context, requestID string" ++ goParamList(args)["ctx context.Context".len..] ++ ") error {\n" ++
        goRawPrelude(args) ++ "\treturn s.Respond(ctx, requestID, " ++ response ++ ")\n}\n";
}

fn emitGoMethods(w: *Writer) Writer.Error!void {
    @setEvalBranchQuota(1_000_000);
    try w.writeAll(comptime "// " ++ header ++ "\n\npackage agent\n\nimport \"context\"\n");
    inline for (wrappers) |x| if (comptime generatedFor(x, .go)) try w.writeAll(comptime "\n" ++ goWrapper(x));
    inline for (answers) |a| try w.writeAll(comptime "\n" ++ goAnswer(a));
}

// ---------------------------------------------------------------- Rust methods

fn rustArgType(comptime a: Arg) []const u8 {
    if (isRawArg(a)) return "serde_json::Value";
    if (stringGroup(argField(a))) |G| return "impl Into<" ++ pascal(groupName(G)) ++ ">";
    if (a.T == []const u8) return "&str";
    return switch (@typeInfo(a.T)) {
        .optional => |o| rustRef(o.child),
        else => rustRef(a.T),
    };
}

/// The field initializer setting the argument's field.
fn rustArgInit(comptime a: Arg) []const u8 {
    const n = rustField(a.name);
    const f = rustField(a.field);
    if (isRawArg(a)) return f ++ ": (!" ++ n ++ ".is_null()).then_some(" ++ n ++ ")";
    if (stringGroup(argField(a)) != null) return f ++ ": " ++ n ++ ".into().0.into_owned()";
    if (a.T == []const u8) return f ++ ": " ++ n ++ ".into()";
    if (@typeInfo(a.T) == .optional) return f ++ ": Some(" ++ n ++ ")";
    return if (eql(f, n)) n else f ++ ": " ++ n;
}

fn rustRawNotes(comptime args: []const Arg) []const u8 {
    var out: []const u8 = "";
    for (args) |a| if (isRawArg(a)) {
        out = out ++ " A null `" ++ a.name ++ "` sends none.";
    };
    return out;
}

/// A method signature, broken the way rustfmt breaks one that does not fit.
fn rustSignature(comptime name: []const u8, comptime params: []const []const u8, comptime result: []const u8) []const u8 {
    var all: []const []const u8 = &.{"&self"};
    all = all ++ params;
    const one_line = "    pub async fn " ++ name ++ "(" ++ join(all, ", ") ++ ") -> Result<" ++ result ++ "> {\n";
    if (one_line.len - 1 <= 100) return one_line;
    var out: []const u8 = "    pub async fn " ++ name ++ "(\n";
    for (all) |p| out = out ++ "        " ++ p ++ ",\n";
    return out ++ "    ) -> Result<" ++ result ++ "> {\n";
}

/// `let <local> = <literal>;` with the struct literal one field per line,
/// as rustfmt writes literals longer than its short-literal width.
fn rustLet(comptime local: []const u8, comptime name: []const u8, comptime inits: []const []const u8) []const u8 {
    var out: []const u8 = "        let " ++ local ++ " = " ++ name ++ " {\n";
    for (inits) |i| out = out ++ "            " ++ i ++ (if (std.mem.startsWith(u8, i, "..")) "\n" else ",\n");
    return out ++ "        };\n";
}

/// An awaited call, with `.await` on its own line when rustfmt would break
/// the chain.
fn rustAwait(comptime call: []const u8) []const u8 {
    return if (call.len + ".await".len <= 60) "        " ++ call ++ ".await\n" else "        " ++ call ++ "\n            .await\n";
}

fn rustParams(comptime args: []const Arg) []const []const u8 {
    var out: []const []const u8 = &.{};
    for (args) |a| out = out ++ [_][]const u8{rustField(a.name) ++ ": " ++ rustArgType(a)};
    return out;
}

fn rustWrapper(comptime w: Wrapper) []const u8 {
    @setEvalBranchQuota(1_000_000);
    const m = methodNamed(w.method);
    const args = wrapperArgs(w);
    const result = if (m.Result == void) "()" else if (m.Result == ?RawJson) "serde_json::Value" else rustRef(m.Result);
    var inits: []const []const u8 = if (onSession(w)) &.{"session: self.session_handle()"} else &.{};
    for (args) |a| inits = inits ++ [_][]const u8{rustArgInit(a)};
    return commentLines("    /// ", wrapperDoc(w) ++ rustRawNotes(args), comment_width) ++
        rustSignature(serdeSnake(baseName(w)), rustParams(args), result) ++
        rustLet("params", nameOf(m.Params), inits) ++
        rustAwait("self.request(method::" ++ screamingSnake(m.name) ++ ", params)") ++ "    }\n";
}

fn rustAnswer(comptime a: Answer) []const u8 {
    @setEvalBranchQuota(1_000_000);
    const args = answerArgs(a);
    var inits: []const []const u8 = &.{};
    for (a.tags) |t| inits = inits ++ [_][]const u8{rustField(t[0]) ++ ": " ++ nameOf(@FieldType(a.Response, t[0])) ++ "::" ++ pascal(t[1])};
    for (args) |x| inits = inits ++ [_][]const u8{rustArgInit(x)};
    if (inits.len < fieldsOf(a.Response, null).len) inits = inits ++ [_][]const u8{"..Default::default()"};
    return commentLines("    /// ", a.doc ++ rustRawNotes(args), comment_width) ++
        rustSignature(a.name, @as([]const []const u8, &.{"request_id: &str"}) ++ rustParams(args), "()") ++
        rustLet("response", nameOf(a.Response), inits) ++
        rustAwait("self.respond(request_id, response)") ++ "    }\n";
}

fn emitRustMethods(w: *Writer) Writer.Error!void {
    @setEvalBranchQuota(1_000_000);
    const uses_map = comptime blk: {
        for (answers) |a| for (answerArgs(a)) |x| if (std.mem.indexOf(u8, rustArgType(x), "HashMap") != null) break :blk true;
        for (wrappers) |x| for (wrapperArgs(x)) |y| if (std.mem.indexOf(u8, rustArgType(y), "HashMap") != null) break :blk true;
        break :blk false;
    };
    try w.writeAll(comptime "// " ++ header ++ "\n\n");
    try w.writeAll("use crate::client::Client;\nuse crate::error::Result;\nuse crate::protocol::*;\nuse crate::session::Session;\n");
    if (uses_map) try w.writeAll("use std::collections::HashMap;\n");
    try w.writeAll("\nimpl Client {\n");
    var first = true;
    inline for (wrappers) |x| if (comptime !onSession(x) and generatedFor(x, .rust)) {
        if (!first) try w.writeAll("\n");
        first = false;
        try w.writeAll(comptime rustWrapper(x));
    };
    try w.writeAll("}\n\nimpl Session {\n");
    first = true;
    inline for (wrappers) |x| if (comptime onSession(x) and generatedFor(x, .rust)) {
        if (!first) try w.writeAll("\n");
        first = false;
        try w.writeAll(comptime rustWrapper(x));
    };
    inline for (answers) |a| try w.writeAll(comptime "\n" ++ rustAnswer(a));
    try w.writeAll("}\n");
}

// ---------------------------------------------------------------- TypeScript methods

/// The protocol type names `T` refers to.
fn tsNames(comptime T: type) []const []const u8 {
    if (T == void or T == RawJson) return &.{};
    return switch (@typeInfo(T)) {
        .optional => |o| tsNames(o.child),
        .pointer => |p| if (p.child == u8) &.{} else tsNames(p.child),
        .@"enum" => &.{nameOf(T)},
        .@"struct" => if (isMap(T)) tsNames(MapValue(T)) else &.{nameOf(T)},
        else => &.{},
    };
}

fn tsImports() []const []const u8 {
    @setEvalBranchQuota(1_000_000);
    var all: []const []const u8 = &.{"Methods"};
    for (wrappers) |x| if (generatedFor(x, .typescript)) {
        all = all ++ tsNames(methodNamed(x.method).Result);
        for (wrapperArgs(x)) |a| {
            all = all ++ if (stringGroup(argField(a))) |G| &[_][]const u8{pascal(groupName(G))} else tsNames(a.T);
        }
    };
    var out: []const []const u8 = &.{};
    for (all) |n| {
        for (out) |o| {
            if (eql(o, n)) break;
        } else out = out ++ [_][]const u8{n};
    }
    return out;
}

fn tsDoc(comptime doc: []const u8) []const u8 {
    const one_line = "  /** " ++ doc ++ " */";
    if (one_line.len <= comment_width) return one_line ++ "\n";
    return "  /**\n" ++ commentLines("   * ", doc, comment_width) ++ "   */\n";
}

fn tsWrapper(comptime w: Wrapper) []const u8 {
    @setEvalBranchQuota(1_000_000);
    const m = methodNamed(w.method);
    const args = wrapperArgs(w);
    var params: []const []const u8 = &.{};
    var fields: []const []const u8 = &.{};
    for (args, 0..) |a, i| {
        const trailing_optional = for (args[i..]) |b| {
            if (!b.optional) break false;
        } else true;
        params = params ++ [_][]const u8{camel(a.name) ++ (if (trailing_optional) "?: " else ": ") ++ tsFieldType(argField(a))};
        fields = fields ++ [_][]const u8{if (eql(a.field, camel(a.name))) a.field else a.field ++ ": " ++ camel(a.name)};
    }
    const object = if (fields.len == 0) "{}" else "{ " ++ join(fields, ", ") ++ " }";
    const request = (if (onSession(w)) "this.sessionRequest(" else "this.request(") ++ quote(m.name) ++ ", " ++ object ++ ")";
    const signature = baseName(w) ++ "(" ++ join(params, ", ") ++ "): Promise<";
    return tsDoc(wrapperDoc(w)) ++ if (m.Result == void)
        "  async " ++ signature ++ "void> {\n    await " ++ request ++ ";\n  }\n"
    else
        "  " ++ signature ++ tsRef(m.Result) ++ "> {\n    return " ++ request ++ ";\n  }\n";
}

fn emitTypeScriptMethods(w: *Writer) Writer.Error!void {
    @setEvalBranchQuota(1_000_000);
    try w.writeAll(comptime "// " ++ header ++ "\n\nimport type { " ++ join(tsImports(), ", ") ++ " } from \"./protocol.ts\";\n\n");
    try w.writeAll(
        \\/** The `provider/*` methods of `Client` that only send a request. */
        \\export abstract class ClientMethods {
        \\  /** Sends any protocol request. */
        \\  abstract request<M extends keyof Methods>(method: M, params: Methods[M]["params"]): Promise<Methods[M]["result"]>;
        \\
    );
    inline for (wrappers) |x| if (comptime !onSession(x) and generatedFor(x, .typescript)) try w.writeAll(comptime "\n" ++ tsWrapper(x));
    try w.writeAll(
        \\}
        \\
        \\/** The `session/*` methods of `Session` that only send a request. */
        \\export abstract class SessionMethods {
        \\  /** Sends a request for this session: the session handle is added to `params`. */
        \\  protected abstract sessionRequest<M extends keyof Methods>(
        \\    method: M,
        \\    params: Omit<Methods[M]["params"], "session">,
        \\  ): Promise<Methods[M]["result"]>;
        \\
    );
    inline for (wrappers) |x| if (comptime onSession(x) and generatedFor(x, .typescript)) try w.writeAll(comptime "\n" ++ tsWrapper(x));
    try w.writeAll("}\n");
}

// ---------------------------------------------------------------- outputs

const Output = struct { file: []const u8, emit: *const fn (*Writer) Writer.Error!void };

const outputs = [_]Output{
    .{ .file = "protocol.schema.json", .emit = emitSchema },
    .{ .file = "protocol.ts", .emit = emitTypeScript },
    .{ .file = "protocol.go", .emit = emitGo },
    .{ .file = "protocol.rs", .emit = emitRust },
    .{ .file = "methods.go", .emit = emitGoMethods },
    .{ .file = "methods.rs", .emit = emitRustMethods },
    .{ .file = "methods.ts", .emit = emitTypeScriptMethods },
};

pub fn main(init: std.process.Init) !void {
    const io = init.io;
    var args = init.minimal.args.iterate();
    _ = args.next();
    const out_path = args.next() orelse return error.MissingOutputDirectory;
    try std.Io.Dir.cwd().createDirPath(io, out_path);
    var dir = try std.Io.Dir.cwd().openDir(io, out_path, .{});
    defer dir.close(io);
    for (outputs) |o| {
        var out: Writer.Allocating = .init(init.gpa);
        defer out.deinit();
        try o.emit(&out.writer);
        try dir.writeFile(io, .{ .sub_path = o.file, .data = out.written() });
    }
}

// ---------------------------------------------------------------- JSON Schema

fn emitSchema(w: *Writer) Writer.Error!void {
    @setEvalBranchQuota(1_000_000);
    var jw: std.json.Stringify = .{ .writer = w, .options = .{ .whitespace = .indent_2 } };
    try jw.beginObject();
    try jw.objectField("$schema");
    try jw.write("https://json-schema.org/draft/2020-12/schema");
    try jw.objectField("$comment");
    try jw.write(header);
    try jw.objectField("title");
    try jw.write("agent-cli-connector protocol");

    try jw.objectField("methods");
    try jw.beginObject();
    inline for (methods) |m| try schemaMethod(&jw, m);
    try jw.endObject();
    try jw.objectField("notifications");
    try jw.beginObject();
    inline for (notifications) |n| {
        try jw.objectField(n.name);
        try schemaRef(&jw, n.Params);
    }
    try jw.endObject();
    try jw.objectField("reverse_requests");
    try jw.beginObject();
    inline for (reverse_requests) |m| try schemaMethod(&jw, m);
    try jw.endObject();

    try jw.objectField("$defs");
    try jw.beginObject();
    inline for (defs) |d| {
        try jw.objectField(d.name);
        switch (d.kind) {
            .record => try schemaObject(&jw, defFields(d)),
            .enumeration => try schemaEnum(&jw, @typeInfo(d.T).@"enum".field_names),
            .alias => try schemaRef(&jw, d.T),
            .flattened => {
                try jw.beginObject();
                try jw.objectField("oneOf");
                try jw.beginArray();
                inline for (@typeInfo(Union(d)).@"union".field_names) |v| {
                    try jw.beginObject();
                    try jw.objectField("allOf");
                    try jw.beginArray();
                    try jw.beginObject();
                    try jw.objectField("type");
                    try jw.write("object");
                    try jw.objectField("properties");
                    try jw.beginObject();
                    try jw.objectField("type");
                    try jw.beginObject();
                    try jw.objectField("const");
                    try jw.write(v);
                    try jw.endObject();
                    try jw.endObject();
                    try jw.objectField("required");
                    try jw.write(&[_][]const u8{"type"});
                    try jw.endObject();
                    try writeRef(&jw, pascal(v));
                    try writeRef(&jw, d.name ++ "Meta");
                    try jw.endArray();
                    try jw.endObject();
                }
                try jw.endArray();
                try jw.endObject();
                try jw.objectField(d.name ++ "Meta");
                try schemaObject(&jw, defFields(d));
                try jw.objectField(d.name ++ "Type");
                try schemaEnum(&jw, @typeInfo(Union(d)).@"union".field_names);
            },
        }
    }
    inline for (constant_groups) |G| {
        try jw.objectField(comptime pascal(groupName(G)));
        try jw.beginObject();
        try jw.objectField("description");
        try jw.write(comptime groupDoc(G));
        try jw.objectField("type");
        try jw.write("string");
        try jw.objectField("examples");
        try jw.beginArray();
        inline for (@typeInfo(G).@"struct".decl_names) |c| try jw.write(@field(G, c));
        try jw.endArray();
        try jw.endObject();
    }
    try jw.endObject();
    try jw.endObject();
    try w.writeAll("\n");
}

fn schemaMethod(jw: *std.json.Stringify, comptime m: Method) !void {
    try jw.objectField(m.name);
    try jw.beginObject();
    try jw.objectField("params");
    try schemaRef(jw, m.Params);
    try jw.objectField("result");
    try schemaRef(jw, m.Result);
    try jw.endObject();
}

fn writeRef(jw: *std.json.Stringify, comptime name: []const u8) !void {
    try jw.beginObject();
    try jw.objectField("$ref");
    try jw.write("#/$defs/" ++ name);
    try jw.endObject();
}

fn schemaEnum(jw: *std.json.Stringify, comptime tags: []const [:0]const u8) !void {
    try jw.beginObject();
    try jw.objectField("type");
    try jw.write("string");
    try jw.objectField("enum");
    try jw.beginArray();
    inline for (tags) |t| try jw.write(t);
    try jw.endArray();
    try jw.endObject();
}

fn schemaObject(jw: *std.json.Stringify, comptime fields: []const Field) !void {
    @setEvalBranchQuota(1_000_000);
    try jw.beginObject();
    try jw.objectField("type");
    try jw.write("object");
    try jw.objectField("properties");
    try jw.beginObject();
    inline for (fields) |f| {
        try jw.objectField(f.name);
        if (comptime visibleDefault(f)) {
            // The reference and the default side by side.
            try jw.beginObject();
            try jw.objectField("allOf");
            try jw.beginArray();
            try schemaFieldRef(jw, f);
            try jw.endArray();
            try jw.objectField("default");
            try jw.beginWriteRaw();
            try jw.writer.writeAll(comptime jsonDefault(f));
            jw.endWriteRaw();
            try jw.endObject();
        } else try schemaFieldRef(jw, f);
    }
    try jw.endObject();
    const required = comptime blk: {
        var r: []const []const u8 = &.{};
        for (fields) |f| {
            if (f.default == null) r = r ++ [_][]const u8{f.name};
        }
        break :blk r;
    };
    if (required.len > 0) {
        try jw.objectField("required");
        try jw.write(required);
    }
    try jw.endObject();
}

fn schemaFieldRef(jw: *std.json.Stringify, comptime f: Field) !void {
    const G = stringGroup(f) orelse return schemaRef(jw, f.T);
    if (!isList(f)) return writeRef(jw, comptime pascal(groupName(G)));
    try jw.beginObject();
    try jw.objectField("type");
    try jw.write("array");
    try jw.objectField("items");
    try writeRef(jw, comptime pascal(groupName(G)));
    try jw.endObject();
}

fn schemaRef(jw: *std.json.Stringify, comptime T: type) !void {
    if (T == RawJson) {
        try jw.beginObject();
        return jw.endObject();
    }
    if (T == void) return schemaType(jw, "null");
    switch (@typeInfo(T)) {
        .optional => |o| {
            if (o.child == RawJson) return schemaRef(jw, o.child);
            try jw.beginObject();
            try jw.objectField("anyOf");
            try jw.beginArray();
            try schemaRef(jw, o.child);
            try schemaType(jw, "null");
            try jw.endArray();
            try jw.endObject();
        },
        .pointer => |p| {
            if (p.child == u8) return schemaType(jw, "string");
            try jw.beginObject();
            try jw.objectField("type");
            try jw.write("array");
            try jw.objectField("items");
            try schemaRef(jw, p.child);
            try jw.endObject();
        },
        .bool => try schemaType(jw, "boolean"),
        .float => try schemaType(jw, "number"),
        .int => |i| {
            try jw.beginObject();
            try jw.objectField("type");
            try jw.write("integer");
            if (i.signedness == .unsigned) {
                try jw.objectField("minimum");
                try jw.write(0);
            }
            try jw.endObject();
        },
        .@"enum" => try writeRef(jw, nameOf(T)),
        .@"struct" => if (comptime isMap(T)) {
            try jw.beginObject();
            try jw.objectField("type");
            try jw.write("object");
            try jw.objectField("additionalProperties");
            try schemaRef(jw, MapValue(T));
            try jw.endObject();
        } else try writeRef(jw, nameOf(T)),
        else => comptime unreachable,
    }
}

fn schemaType(jw: *std.json.Stringify, comptime name: []const u8) !void {
    try jw.beginObject();
    try jw.objectField("type");
    try jw.write(name);
    try jw.endObject();
}

// ---------------------------------------------------------------- TypeScript

fn tsRef(comptime T: type) []const u8 {
    if (T == void) return "null";
    if (T == RawJson) return "unknown";
    return switch (@typeInfo(T)) {
        .optional => |o| if (o.child == RawJson) "unknown" else tsRef(o.child) ++ " | null",
        .pointer => |p| if (p.child == u8) "string" else blk: {
            const e = tsRef(p.child);
            break :blk if (std.mem.indexOfScalar(u8, e, '|') != null) "(" ++ e ++ ")[]" else e ++ "[]";
        },
        .bool => "boolean",
        .int, .float => "number",
        .@"enum" => nameOf(T),
        .@"struct" => if (isMap(T)) "Record<string, " ++ tsRef(MapValue(T)) ++ ">" else nameOf(T),
        else => unreachable,
    };
}

/// A field's TypeScript type: optional fields drop their `| null`, because
/// null is never written.
fn tsFieldType(comptime f: Field) []const u8 {
    if (stringGroup(f)) |G| return pascal(groupName(G)) ++ if (isList(f)) "[]" else "";
    return switch (@typeInfo(f.T)) {
        .optional => |o| tsRef(o.child),
        else => tsRef(f.T),
    };
}

fn tsUnion(comptime members: []const []const u8) []const u8 {
    @setEvalBranchQuota(1_000_000);
    var one_line: []const u8 = "";
    for (members, 0..) |m, i| one_line = one_line ++ (if (i > 0) " | " else "") ++ m;
    if (one_line.len <= 80) return " " ++ one_line ++ ";\n";
    var out: []const u8 = "";
    for (members) |m| out = out ++ "\n  | " ++ m;
    return out ++ ";\n";
}

fn quotedTags(comptime tags: []const [:0]const u8) []const []const u8 {
    @setEvalBranchQuota(1_000_000);
    var out: []const []const u8 = &.{};
    for (tags) |t| out = out ++ [_][]const u8{quote(t)};
    return out;
}

fn emitTypeScript(w: *Writer) Writer.Error!void {
    @setEvalBranchQuota(1_000_000);
    try w.writeAll(comptime "// " ++ header ++ "\n\n");
    try w.writeAll(comptime "/** Client requests: method name to params and result. */\nexport interface Methods {\n");
    inline for (methods) |m| try w.writeAll(comptime "  " ++ quote(m.name) ++ ": { params: " ++ tsRef(m.Params) ++ "; result: " ++ tsRef(m.Result) ++ " };\n");
    try w.writeAll(comptime "}\n\n/** Notifications from the library: method name to params. */\nexport interface Notifications {\n");
    inline for (notifications) |n| try w.writeAll(comptime "  " ++ quote(n.name) ++ ": " ++ tsRef(n.Params) ++ ";\n");
    try w.writeAll(comptime "}\n\n/** Requests from the library to the client: method name to params and result. */\nexport interface ReverseRequests {\n");
    inline for (reverse_requests) |m| try w.writeAll(comptime "  " ++ quote(m.name) ++ ": { params: " ++ tsRef(m.Params) ++ "; result: " ++ tsRef(m.Result) ++ " };\n");
    try w.writeAll(comptime "}\n");
    inline for (constant_groups) |G| {
        try w.writeAll(comptime "\n/** " ++ groupDoc(G) ++ " */\nexport const " ++ pascal(groupName(G)) ++ " = {\n");
        inline for (@typeInfo(G).@"struct".decl_names) |c| try w.writeAll(comptime "  " ++ camel(c) ++ ": " ++ quote(@field(G, c)) ++ ",\n");
        const name = comptime pascal(groupName(G));
        try w.writeAll(comptime "} as const;\nexport type " ++ name ++ " = (typeof " ++ name ++ ")[keyof typeof " ++ name ++ "] | (string & {});\n");
    }

    inline for (defs) |d| {
        try w.writeAll("\n");
        switch (d.kind) {
            .record => try tsInterface(w, d.name, defFields(d), find(input_defs, d.T) == null),
            .enumeration => try w.writeAll(comptime "export type " ++ d.name ++ " =" ++ tsUnion(quotedTags(@typeInfo(d.T).@"enum".field_names))),
            .alias => try w.writeAll(comptime "export type " ++ d.name ++ " = " ++ nameOf(d.T) ++ ";\n"),
            .flattened => {
                const tags = @typeInfo(Union(d)).@"union".field_names;
                const variants = comptime blk: {
                    var out: []const []const u8 = &.{};
                    for (tags) |v| out = out ++ [_][]const u8{"({ type: " ++ quote(v) ++ " } & " ++ pascal(v) ++ " & " ++ d.name ++ "Meta)"};
                    break :blk out;
                };
                try w.writeAll(comptime "/** Discriminated by `type`; payload and meta fields share one object. */\n");
                try w.writeAll(comptime "export type " ++ d.name ++ " =" ++ tsUnion(variants));
                try w.writeAll(comptime "\nexport type " ++ d.name ++ "Type =" ++ tsUnion(quotedTags(tags)));
                try w.writeAll(comptime "\n/** The " ++ d.name ++ " variant with type `T`. */\n");
                try w.writeAll(comptime "export type " ++ d.name ++ "Of<T extends " ++ d.name ++ "Type> = Extract<" ++ d.name ++ ", { type: T }>;\n\n");
                try tsInterface(w, d.name ++ "Meta", defFields(d), true);
            },
        }
    }
}

/// Numbers, enums, and structs are written even at their default (see
/// `json.write`).
fn alwaysSent(comptime T: type) bool {
    return switch (@typeInfo(T)) {
        .int, .float, .@"enum", .@"struct" => true,
        else => false,
    };
}

/// `output`: the type only flows from the library to the client, so fields
/// the library always writes are required.
fn tsInterface(w: *Writer, comptime name: []const u8, comptime fields: []const Field, comptime output: bool) !void {
    @setEvalBranchQuota(1_000_000);
    if (fields.len == 0) return w.writeAll(comptime "export interface " ++ name ++ " {}\n");
    try w.writeAll(comptime "export interface " ++ name ++ " {\n");
    inline for (fields) |f| {
        if (comptime visibleDefault(f)) try w.writeAll(comptime "  /** Default: " ++ jsonDefault(f) ++ ". */\n");
        try w.writeAll(comptime "  " ++ f.name ++ (if (f.default == null or (output and alwaysSent(f.T))) ": " else "?: ") ++ tsFieldType(f) ++ ";\n");
    }
    try w.writeAll(comptime "}\n");
}

// ---------------------------------------------------------------- Go

fn goRef(comptime T: type) []const u8 {
    if (T == RawJson) return "json.RawMessage";
    return switch (@typeInfo(T)) {
        .optional => |o| if (isRecord(o.child) or switch (@typeInfo(o.child)) {
            .bool, .int, .float => true,
            else => false,
        }) "*" ++ goRef(o.child) else goRef(o.child),
        .pointer => |p| if (p.child == u8) "string" else "[]" ++ goRef(p.child),
        .bool => "bool",
        .int => "int64",
        .float => "float64",
        .@"enum" => nameOf(T),
        .@"struct" => if (isMap(T)) "map[string]" ++ goRef(MapValue(T)) else nameOf(T),
        else => unreachable,
    };
}

fn goFieldType(comptime f: Field) []const u8 {
    const G = stringGroup(f) orelse return goRef(f.T);
    return (if (isList(f)) "[]" else "") ++ goName(groupName(G));
}

fn goEnumConst(comptime E: type, comptime tag: []const u8) []const u8 {
    return nameOf(E) ++ goName(tag);
}

/// Go field initializers for the non-zero defaults of `fields`, including
/// those of nested structs, or "" when every default is Go's zero value.
fn goInits(comptime fields: []const Field) []const u8 {
    @setEvalBranchQuota(1_000_000);
    var out: []const u8 = "";
    for (fields) |f| {
        const value: []const u8 = if (visibleDefault(f)) switch (@typeInfo(f.T)) {
            .@"enum" => goEnumConst(f.T, @tagName(defaultValue(f).?)),
            else => jsonDefault(f),
        } else if (isRecord(f.T) and goInits(fieldsOf(f.T, null)).len > 0)
            nameOf(f.T) ++ "{" ++ goInits(fieldsOf(f.T, null)) ++ "}"
        else
            "";
        if (value.len > 0) out = out ++ (if (out.len > 0) ", " else "") ++ goName(f.name) ++ ": " ++ value;
    }
    return out;
}

const Row = []const []const u8;

/// Writes rows aligned the way gofmt aligns them: every cell but the last
/// is padded to its column's widest cell plus one space.
fn writeAligned(w: *Writer, indent: []const u8, rows: []const Row) !void {
    var width: [4]usize = @splat(0);
    for (rows) |r| {
        for (r[0 .. r.len - 1], 0..) |cell, i| width[i] = @max(width[i], cell.len);
    }
    for (rows) |r| {
        try w.writeAll(indent);
        for (r[0 .. r.len - 1], 0..) |cell, i| {
            try w.writeAll(cell);
            try w.splatByteAll(' ', width[i] + 1 - cell.len);
        }
        try w.writeAll(r[r.len - 1]);
        try w.writeAll("\n");
    }
}

fn goStruct(w: *Writer, comptime name: []const u8, comptime fields: []const Field) !void {
    @setEvalBranchQuota(1_000_000);
    if (fields.len == 0) return w.writeAll(comptime "type " ++ name ++ " struct{}\n");
    const rows = comptime blk: {
        var out: []const Row = &.{};
        for (fields) |f| {
            const opt = if (f.default == null) "" else if (isRecord(f.T)) ",omitzero" else ",omitempty";
            out = out ++ [_]Row{&.{ goName(f.name), goFieldType(f), "`json:\"" ++ f.name ++ opt ++ "\"`" }};
        }
        break :blk out;
    };
    try w.writeAll(comptime "type " ++ name ++ " struct {\n");
    try writeAligned(w, "\t", rows);
    try w.writeAll(comptime "}\n");
}

/// An UnmarshalJSON that starts from the protocol defaults, for a struct
/// whose defaults are not all Go zero values.
fn goDefaults(w: *Writer, comptime name: []const u8, comptime fields: []const Field) !void {
    const inits = comptime goInits(fields);
    if (inits.len == 0) return;
    try w.writeAll(comptime "\n// UnmarshalJSON applies the protocol defaults to missing fields.\n");
    try w.writeAll(comptime "func (x *" ++ name ++ ") UnmarshalJSON(data []byte) error {\n");
    try w.writeAll(comptime "\ttype plain " ++ name ++ "\n");
    try w.writeAll(comptime "\tp := plain{" ++ inits ++ "}\n");
    try w.writeAll(comptime "\tif err := json.Unmarshal(data, &p); err != nil {\n\t\treturn err\n\t}\n");
    try w.writeAll(comptime "\t*x = " ++ name ++ "(p)\n\treturn nil\n}\n");
}

fn goConsts(w: *Writer, comptime type_name: []const u8, comptime names: []const []const u8, comptime values: []const []const u8) !void {
    @setEvalBranchQuota(1_000_000);
    const rows = comptime blk: {
        var out: []const Row = &.{};
        for (names, values) |n, v| out = out ++ [_]Row{if (type_name.len == 0) &.{ n, "= " ++ quote(v) } else &.{ n, type_name, "= " ++ quote(v) }};
        break :blk out;
    };
    try w.writeAll(comptime "const (\n");
    try writeAligned(w, "\t", rows);
    try w.writeAll(comptime ")\n");
}

fn goEnum(w: *Writer, comptime name: []const u8, comptime tags: []const [:0]const u8) !void {
    @setEvalBranchQuota(1_000_000);
    const names = comptime blk: {
        var out: []const []const u8 = &.{};
        for (tags) |t| out = out ++ [_][]const u8{name ++ goName(t)};
        break :blk out;
    };
    const values = comptime blk: {
        var out: []const []const u8 = &.{};
        for (tags) |t| out = out ++ [_][]const u8{t};
        break :blk out;
    };
    try w.writeAll(comptime "type " ++ name ++ " string\n\n");
    try goConsts(w, name, names, values);
}

fn emitGo(w: *Writer) Writer.Error!void {
    @setEvalBranchQuota(1_000_000);
    try w.writeAll(comptime "// " ++ header ++ "\n\n");
    try w.writeAll(comptime "package agent\n\nimport \"encoding/json\"\n\n");
    try w.writeAll(comptime "// JSON-RPC method names: client requests, notifications, and requests to the client.\n");
    const names = comptime blk: {
        var out: []const []const u8 = &.{};
        for (methods) |m| out = out ++ [_][]const u8{"Method" ++ pascal(m.name)};
        for (notifications) |n| out = out ++ [_][]const u8{"Notification" ++ pascal(n.name)};
        for (reverse_requests) |m| out = out ++ [_][]const u8{"Request" ++ pascal(m.name)};
        break :blk out;
    };
    const values = comptime blk: {
        var out: []const []const u8 = &.{};
        for (methods) |m| out = out ++ [_][]const u8{m.name};
        for (notifications) |n| out = out ++ [_][]const u8{n.name};
        for (reverse_requests) |m| out = out ++ [_][]const u8{m.name};
        break :blk out;
    };
    try goConsts(w, "", names, values);
    inline for (constant_groups) |G| {
        const prefix = comptime goName(groupName(G));
        const decls = @typeInfo(G).@"struct".decl_names;
        const group_names = comptime blk: {
            var out: []const []const u8 = &.{};
            for (decls) |c| out = out ++ [_][]const u8{prefix ++ goName(c)};
            break :blk out;
        };
        const group_values = comptime blk: {
            var out: []const []const u8 = &.{};
            for (decls) |c| out = out ++ [_][]const u8{@field(G, c)};
            break :blk out;
        };
        try w.writeAll(comptime "\n// " ++ prefix ++ " is a named string value; any other string also passes through.\ntype " ++ prefix ++ " string\n\n// " ++ groupDoc(G) ++ "\n");
        try goConsts(w, prefix, group_names, group_values);
    }

    inline for (defs) |d| {
        try w.writeAll("\n");
        switch (d.kind) {
            .record => {
                try goStruct(w, d.name, defFields(d));
                try goDefaults(w, d.name, defFields(d));
            },
            .enumeration => try goEnum(w, d.name, @typeInfo(d.T).@"enum".field_names),
            .alias => {
                try w.writeAll(comptime "type " ++ d.name ++ " " ++ nameOf(d.T) ++ "\n");
                try goDefaults(w, d.name, fieldsOf(d.T, null));
            },
            .flattened => try goFlattened(w, d),
        }
    }
}

/// Go source from `text`: "$" becomes `name` and each leading group of four
/// spaces a tab (Zig multiline strings cannot hold tabs).
fn goTemplate(comptime name: []const u8, comptime text: []const u8) []const u8 {
    @setEvalBranchQuota(1_000_000);
    var out: []const u8 = "";
    var lines = std.mem.splitScalar(u8, text, '\n');
    var first = true;
    while (lines.next()) |line| {
        if (!first) out = out ++ "\n";
        first = false;
        var rest = line;
        while (std.mem.startsWith(u8, rest, "    ")) : (rest = rest[4..]) out = out ++ "\t";
        for (rest) |c| out = out ++ (if (c == '$') name else &[_]u8{c});
    }
    return out;
}

fn goFlattened(w: *Writer, comptime d: Def) !void {
    @setEvalBranchQuota(1_000_000);
    const n = d.name;
    const tags = @typeInfo(Union(d)).@"union".field_names;
    try w.writeAll(comptime "// " ++ n ++ " is one JSON object: \"type\", the fields of the payload struct\n");
    try w.writeAll(comptime "// that matches it, and the " ++ n ++ "Meta fields.\n");
    try w.writeAll(comptime "type " ++ n ++ " struct {\n\t" ++ n ++ "Meta\n");
    try writeAligned(w, "\t", &.{
        &.{ "Type", n ++ "Type", "`json:\"type\"`" },
        &.{ "Payload", n ++ "Payload", "`json:\"-\"`" },
    });
    try w.writeAll(comptime "}\n\n");
    try goStruct(w, n ++ "Meta", defFields(d));
    try goDefaults(w, n ++ "Meta", defFields(d));
    try w.writeAll("\n");
    try goEnum(w, n ++ "Type", tags);
    try w.writeAll(comptime "\n// " ++ n ++ "Payload is the payload struct of one " ++ n ++ " type.\n");
    try w.writeAll(comptime "type " ++ n ++ "Payload interface {\n\t" ++ n ++ "Type() " ++ n ++ "Type\n}\n\n");
    const methods_rows = comptime blk: {
        var out: []const Row = &.{};
        for (tags) |v| out = out ++ [_]Row{&.{ "func (" ++ pascal(v) ++ ") " ++ n ++ "Type() " ++ n ++ "Type", "{ return " ++ n ++ "Type" ++ goName(v) ++ " }" }};
        break :blk out;
    };
    try writeAligned(w, "", methods_rows);

    try w.writeAll(comptime goTemplate(n,
        \\
        \\// MarshalJSON writes the type, the payload fields, and the meta fields as
        \\// one object.
        \\func (e $) MarshalJSON() ([]byte, error) {
        \\    t := e.Type
        \\    if e.Payload != nil {
        \\        t = e.Payload.$Type()
        \\    }
        \\    out, err := json.Marshal(t)
        \\    if err != nil {
        \\        return nil, err
        \\    }
        \\    out = append([]byte(`{"type":`), out...)
        \\    parts := []any{e.$Meta}
        \\    if e.Payload != nil {
        \\        parts = []any{e.Payload, e.$Meta}
        \\    }
        \\    for _, part := range parts {
        \\        b, err := json.Marshal(part)
        \\        if err != nil {
        \\            return nil, err
        \\        }
        \\        if len(b) > 2 {
        \\            out = append(out, ',')
        \\            out = append(out, b[1:len(b)-1]...)
        \\        }
        \\    }
        \\    return append(out, '}'), nil
        \\}
        \\
        \\// UnmarshalJSON decodes the payload struct that matches "type". Payload
        \\// stays nil for a type this file does not know.
        \\func (e *$) UnmarshalJSON(data []byte) error {
        \\    var head struct {
        \\        Type $Type `json:"type"`
        \\    }
        \\    if err := json.Unmarshal(data, &head); err != nil {
        \\        return err
        \\    }
        \\    var meta $Meta
        \\    if err := json.Unmarshal(data, &meta); err != nil {
        \\        return err
        \\    }
        \\    payload, err := decode$Payload(head.Type, data)
        \\    if err != nil {
        \\        return err
        \\    }
        \\    *e = ${$Meta: meta, Type: head.Type, Payload: payload}
        \\    return nil
        \\}
        \\
        \\
    ));
    try w.writeAll(comptime "func decode" ++ n ++ "Payload(t " ++ n ++ "Type, data []byte) (" ++ n ++ "Payload, error) {\n\tswitch t {\n");
    inline for (tags) |v| {
        try w.writeAll(comptime "\tcase " ++ n ++ "Type" ++ goName(v) ++ ":\n");
        try w.writeAll(comptime "\t\tvar p " ++ pascal(v) ++ "\n\t\terr := json.Unmarshal(data, &p)\n\t\treturn p, err\n");
    }
    try w.writeAll(comptime "\tdefault:\n\t\treturn nil, nil\n\t}\n}\n");
}

// ---------------------------------------------------------------- Rust

fn rustRef(comptime T: type) []const u8 {
    if (T == RawJson) return "serde_json::Value";
    return switch (@typeInfo(T)) {
        .optional => |o| "Option<" ++ rustRef(o.child) ++ ">",
        .pointer => |p| if (p.child == u8) "String" else "Vec<" ++ rustRef(p.child) ++ ">",
        .bool => "bool",
        .int => "i64",
        .float => "f64",
        .@"enum" => nameOf(T),
        .@"struct" => if (isMap(T)) "HashMap<String, " ++ rustRef(MapValue(T)) ++ ">" else nameOf(T),
        else => unreachable,
    };
}

/// The field's protocol default equals Rust's `Default` for its type.
fn rustDefaultIsTypeDefault(comptime f: Field) bool {
    const v = defaultValue(f) orelse return false;
    if (@typeInfo(f.T) == .@"enum") return eql(@tagName(v), enumDefaultTag(f.T));
    return !visibleDefault(f);
}

/// A Rust expression for the field's protocol default.
fn rustDefaultExpr(comptime f: Field) []const u8 {
    const v = defaultValue(f) orelse return "Default::default()";
    if (!visibleDefault(f)) return "Default::default()";
    return switch (@typeInfo(f.T)) {
        .pointer => "String::from(" ++ quote(v) ++ ")",
        .@"enum" => nameOf(f.T) ++ "::" ++ pascal(@tagName(v)),
        .float => std.fmt.comptimePrint("{d}", .{v}) ++ (if (v == @trunc(v)) ".0" else ""),
        else => jsonDefault(f),
    };
}

const rust_derive = "#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]\n";

/// Rust's `Default` exists for the type: false for a flattened struct, which
/// has no default variant, and for structs that hold one.
fn rustHasDefault(comptime T: type) bool {
    return switch (@typeInfo(T)) {
        .@"struct" => if (!isRecord(T)) true else if (flattenedField(T) != null) false else for (fieldsOf(T, null)) |f| {
            if (!rustHasDefault(f.T)) break false;
        } else true,
        else => true,
    };
}

/// One field of a struct without a container `#[serde(default)]`: each
/// field that can default does so on its own.
fn rustOwnDefaultField(w: *Writer, comptime f: Field) !void {
    if (comptime rustDefaultIsTypeDefault(f)) {
        try w.writeAll("    #[serde(default, skip_serializing_if = \"is_default\")]\n");
    } else if (comptime f.default != null) {
        @compileError("bindgen: Rust cannot default field " ++ f.name ++ " of a struct without Default");
    } else if (comptime rustHasDefault(f.T)) try w.writeAll("    #[serde(default)]\n");
    try w.writeAll(comptime "    pub " ++ rustField(f.name) ++ ": " ++ rustFieldType(f) ++ ",\n");
}

fn rustFieldType(comptime f: Field) []const u8 {
    const G = stringGroup(f) orelse return rustRef(f.T);
    const name = pascal(groupName(G));
    return if (isList(f)) "Vec<" ++ name ++ ">" else name;
}

fn rustStruct(w: *Writer, comptime name: []const u8, comptime fields: []const Field) !void {
    @setEvalBranchQuota(1_000_000);
    const has_default = comptime for (fields) |f| {
        if (!rustHasDefault(f.T)) break false;
    } else true;
    if (!has_default) {
        try w.writeAll(comptime rust_derive ++ "pub struct " ++ name ++ " {\n");
        inline for (fields) |f| try rustOwnDefaultField(w, f);
        return w.writeAll("}\n");
    }
    const derive_default = comptime for (fields) |f| {
        if (f.default != null and !rustDefaultIsTypeDefault(f)) break false;
    } else true;
    try w.writeAll(comptime if (derive_default) "#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]\n" else rust_derive);
    if (fields.len == 0) return w.writeAll(comptime "pub struct " ++ name ++ " {}\n");
    try w.writeAll(comptime "#[serde(default)]\npub struct " ++ name ++ " {\n");
    inline for (fields) |f| {
        if (comptime rustDefaultIsTypeDefault(f)) try w.writeAll(comptime "    #[serde(skip_serializing_if = \"is_default\")]\n");
        try w.writeAll(comptime "    pub " ++ rustField(f.name) ++ ": " ++ rustFieldType(f) ++ ",\n");
    }
    try w.writeAll(comptime "}\n");
    if (derive_default) return;
    try w.writeAll(comptime "\nimpl Default for " ++ name ++ " {\n    fn default() -> Self {\n        Self {\n");
    inline for (fields) |f| try w.writeAll(comptime "            " ++ rustField(f.name) ++ ": " ++ rustDefaultExpr(f) ++ ",\n");
    try w.writeAll(comptime "        }\n    }\n}\n");
}

fn rustEnum(w: *Writer, comptime name: []const u8, comptime E: type) !void {
    @setEvalBranchQuota(1_000_000);
    const default_tag = comptime enumDefaultTag(E);
    try w.writeAll(comptime "#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Hash, Serialize, Deserialize)]\n");
    try w.writeAll(comptime "#[serde(rename_all = \"snake_case\")]\npub enum " ++ name ++ " {\n");
    inline for (@typeInfo(E).@"enum".field_names) |t| {
        if (comptime eql(t, default_tag)) try w.writeAll(comptime "    #[default]\n");
        if (comptime !eql(serdeSnake(pascal(t)), t)) try w.writeAll(comptime "    #[serde(rename = " ++ quote(t) ++ ")]\n");
        try w.writeAll(comptime "    " ++ pascal(t) ++ ",\n");
    }
    try w.writeAll(comptime "}\n");
}

fn emitRust(w: *Writer) Writer.Error!void {
    @setEvalBranchQuota(1_000_000);
    try w.writeAll(comptime "// " ++ header ++ "\n\n");
    try w.writeAll(
        \\use serde::{Deserialize, Serialize};
        \\use std::borrow::Cow;
        \\use std::collections::HashMap;
        \\
        \\fn is_default<T: Default + PartialEq>(v: &T) -> bool {
        \\    *v == T::default()
        \\}
        \\
        \\/// JSON-RPC method names: client requests, notifications, and requests to the client.
        \\pub mod method {
        \\
    );
    inline for (methods ++ reverse_requests) |m| try w.writeAll(comptime "    pub const " ++ screamingSnake(m.name) ++ ": &str = " ++ quote(m.name) ++ ";\n");
    inline for (notifications) |m| try w.writeAll(comptime "    pub const " ++ screamingSnake(m.name) ++ ": &str = " ++ quote(m.name) ++ ";\n");
    try w.writeAll(comptime "}\n");
    inline for (constant_groups) |G| {
        const n = comptime pascal(groupName(G));
        try w.writeAll(comptime "\n/// A named `" ++ groupName(G) ++ "` value (the constants), or any other string the harness accepts.\n" ++
            "#[derive(Debug, Clone, Default, PartialEq, Eq, Hash, Serialize, Deserialize)]\n#[serde(transparent)]\n" ++
            "pub struct " ++ n ++ "(pub Cow<'static, str>);\n\nimpl " ++ n ++ " {\n");
        inline for (@typeInfo(G).@"struct".decl_names) |c| try w.writeAll(comptime "    pub const " ++ screamingSnake(c) ++ ": " ++ n ++ " = " ++ n ++ "(Cow::Borrowed(" ++ quote(@field(G, c)) ++ "));\n");
        try w.writeAll(comptime "\n    pub fn as_str(&self) -> &str {\n        &self.0\n    }\n}\n\n" ++
            "impl From<&str> for " ++ n ++ " {\n    fn from(s: &str) -> Self {\n        Self(Cow::Owned(s.to_owned()))\n    }\n}\n\n" ++
            "impl From<String> for " ++ n ++ " {\n    fn from(s: String) -> Self {\n        Self(Cow::Owned(s))\n    }\n}\n\n" ++
            "impl std::ops::Deref for " ++ n ++ " {\n    type Target = str;\n    fn deref(&self) -> &str {\n        &self.0\n    }\n}\n\n" ++
            "impl std::fmt::Display for " ++ n ++ " {\n    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {\n        f.write_str(&self.0)\n    }\n}\n");
    }

    inline for (defs) |d| {
        try w.writeAll("\n");
        switch (d.kind) {
            .record => try rustStruct(w, d.name, defFields(d)),
            .enumeration => try rustEnum(w, d.name, d.T),
            .alias => try w.writeAll(comptime "pub type " ++ d.name ++ " = " ++ nameOf(d.T) ++ ";\n"),
            .flattened => {
                const n = d.name;
                try w.writeAll(comptime "/// One JSON object: \"type\", the payload fields, and the meta fields.\n");
                try w.writeAll(comptime rust_derive ++ "pub struct " ++ n ++ " {\n    #[serde(flatten)]\n    pub payload: " ++ n ++ "Payload,\n");
                inline for (comptime defFields(d)) |f| try rustOwnDefaultField(w, f);
                try w.writeAll(comptime "}\n\n" ++ rust_derive ++ "#[serde(tag = \"type\", rename_all = \"snake_case\")]\npub enum " ++ n ++ "Payload {\n");
                inline for (@typeInfo(Union(d)).@"union".field_names) |v| {
                    if (comptime !eql(serdeSnake(pascal(v)), v)) try w.writeAll(comptime "    #[serde(rename = " ++ quote(v) ++ ")]\n");
                    try w.writeAll(comptime "    " ++ pascal(v) ++ "(" ++ pascal(v) ++ "),\n");
                }
                try w.writeAll(comptime "    /// A type this binding does not know, or a payload it cannot decode: the\n");
                try w.writeAll(comptime "    /// \"type\" and payload fields as JSON.\n");
                try w.writeAll(comptime "    #[serde(untagged)]\n    " ++ unknown_variant ++ "(serde_json::Value),\n");
                try w.writeAll(comptime "}\n");
            },
        }
    }
}
