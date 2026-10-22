//! The Codex provider: starts sessions on dedicated or shared app-servers,
//! checks the installation, and lists models. Persisted threads are read
//! in `history.zig`.
const std = @import("std");
const agent = @import("agent");
const session = @import("session.zig");
const history = @import("history.zig");
const options = @import("options.zig");
const Client = @import("client.zig").Client;
const CodexSession = session.CodexSession;
const Options = options.Options;
const Allocator = std.mem.Allocator;
const Io = std.Io;
const types = agent.types;
const errors = agent.errors;
const providerkit = agent.providerkit;
const JsonView = agent.json.JsonView;
const ObjectBuilder = agent.json.ObjectBuilder;
const Diagnostics = agent.Diagnostics;
const Value = std.json.Value;
const Error = errors.Error;
const name = @import("root.zig").name;

pub const Provider = struct {
    env: agent.Env,
    /// Holds the options copy.
    arena: std.heap.ArenaAllocator,
    opts: Options,
    vtable: agent.Provider.VTable,
    mutex: Io.Mutex = .init,
    shared: ?*Client = null,
};

pub fn capabilities(opts: Options) types.Capabilities {
    const modes = types.permission_mode;
    return .{
        .streaming = true,
        .reasoning_stream = true,
        .images = true,
        .steer = true,
        .queue = true,
        .interrupt = true,
        .interrupt_turn = true,
        .set_model = true,
        .set_effort = true,
        .permission_modes = &.{ modes.default, modes.accept_edits, modes.plan, modes.bypass, modes.read_only, modes.dont_ask, modes.auto },
        .permission_prompt = true,
        .user_questions = true,
        .elicitation = true,
        .host_tools = !opts.disable_experimental_api,
        .mcp_servers = true,
        .resume_session = true,
        .fork = true,
        .fork_at_turn = true,
        .rewind = true,
        .compact = true,
        .set_title = true,
        .stop_task = true,
        .read_rate_limits = true,
        .system_prompt = true,
        .output_schema = true,
        .plans = true,
        .tasks = true,
        .subagents = true,
        .cost_reporting = false,
        .rate_limits = true,
        .hooks = true,
        .goals = true,
        .check = true,
        .list_models = true,
        .list_sessions = true,
        .read_history = true,
        .delete_session = true,
        .list_subagents = true,
    };
}

/// Creates the provider. `options` is copied.
pub fn create(env: agent.Env, provider_options: Options) !agent.Provider {
    const provider = try env.gpa.create(Provider);
    errdefer env.gpa.destroy(provider);
    provider.* = .{ .env = env, .arena = .init(env.gpa), .opts = undefined, .vtable = undefined };
    errdefer provider.arena.deinit();
    provider.opts = try agent.clone(Options, provider.arena.allocator(), provider_options);
    provider.vtable = .{
        .name = name,
        .capabilities = capabilities(provider.opts),
        .start = start,
        .deinit = deinit,
        .check = check,
        .list_models = listModels,
        .list_sessions = history.listSessions,
        .read_history = history.readHistory,
        .delete_session = history.deleteSession,
        .list_subagents = history.listSubagents,
    };
    return .{ .ptr = provider, .vtable = &provider.vtable };
}

pub fn of(ptr: *anyopaque) *Provider {
    return @ptrCast(@alignCast(ptr));
}

/// Stops the shared app-server, if any, ending its sessions.
fn deinit(ptr: *anyopaque) void {
    const provider = of(ptr);
    if (provider.shared) |client| {
        client.close();
        client.release();
    }
    provider.arena.deinit();
    provider.env.gpa.destroy(provider);
}

fn start(ptr: *anyopaque, opts: agent.StartOptions, diag: ?*Diagnostics) Error!*agent.Session {
    const provider = of(ptr);
    var arena: std.heap.ArenaAllocator = .init(provider.env.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const owned = !provider.opts.shared_server;
    const experimental = !provider.opts.disable_experimental_api;
    _ = try providerkit.options(options.SessionOptions, a, name, opts.config.provider_options, diag);
    // Refused before an app-server starts.
    _ = try session.checkConfig(a, opts.config, owned, experimental, diag);
    const codex = try CodexSession.create(provider.env, opts, owned, experimental, provider.vtable.capabilities, diag);
    const started = if (owned) Client.start(provider.env, provider.opts, .{
        .cwd = opts.config.cwd,
        .env = opts.config.env,
        .unset_env = opts.config.unset_env,
        .tap = if (opts.config.wire_tap) codex.rt() else null,
    }, diag) else sharedClient(provider, diag);
    const client = started catch |e| {
        codex.discard();
        return e;
    };
    codex.open(client, diag) catch |e| {
        // The client's wire tap points into the session: release it first.
        client.release();
        codex.discard();
        return e;
    };
    return &codex.session;
}

/// The shared app-server with a reference for the caller, started when
/// missing or exited.
fn sharedClient(provider: *Provider, diag: ?*Diagnostics) Error!*Client {
    provider.mutex.lockUncancelable(provider.env.io);
    defer provider.mutex.unlock(provider.env.io);
    if (provider.shared) |client| if (client.hasEnded()) {
        client.release();
        provider.shared = null;
    };
    if (provider.shared == null) provider.shared = try Client.start(provider.env, provider.opts, .{}, diag);
    provider.shared.?.retain();
    return provider.shared.?;
}

/// The shared app-server, or a short-lived one. Release it when done.
pub fn acquire(provider: *Provider, diag: ?*Diagnostics) Error!*Client {
    if (provider.opts.shared_server) return sharedClient(provider, diag);
    return Client.start(provider.env, provider.opts, .{}, diag);
}

/// Reports the installed version from "codex --version" and the sign-in
/// state from account/read.
fn check(ptr: *anyopaque, a: Allocator, _: ?*Diagnostics) Error!types.ProviderStatus {
    const provider = of(ptr);
    var status = providerkit.versionStatus(provider.env, a, providerkit.firstOf(&.{ provider.opts.path, "codex" }), provider.opts.env);
    if (!status.installed or status.@"error" != null) return status;
    var diag: Diagnostics = .{};
    const client = acquire(provider, &diag) catch |e| return providerkit.failedStatus(a, status, &diag, e);
    defer client.release();
    const response = client.callJson(a, "account/read", .{ .object = .empty }, .{}, &diag) catch |e|
        return providerkit.failedStatus(a, status, &diag, e);
    // A provider that needs no OpenAI sign-in, such as a local model, is
    // usable without an account.
    var auth: types.AuthStatus = .{ .authenticated = !response.flag("requiresOpenaiAuth"), .raw = try response.raw(a) };
    const account = response.get("account");
    if (account.object() != null) {
        auth.authenticated = true;
        auth.method = account.str("type");
        auth.email = account.str("email");
        auth.plan = account.str("planType");
    }
    status.auth = auth;
    return status;
}

/// Calls a cursor-paginated method until its last page and returns every
/// page's data.
fn listPages(client: *Client, a: Allocator, method: []const u8, params: *ObjectBuilder, diag: ?*Diagnostics) Error![]const Value {
    var out: std.ArrayList(Value) = .empty;
    while (true) {
        const response = try client.callJson(a, method, params.value(), .{}, diag);
        try out.appendSlice(a, response.get("data").items());
        const cursor = response.str("nextCursor");
        if (cursor.len == 0) return out.items;
        try params.str("cursor", cursor);
    }
}

fn listModels(ptr: *anyopaque, a: Allocator, diag: ?*Diagnostics) Error![]const types.Model {
    const provider = of(ptr);
    const client = try acquire(provider, diag);
    defer client.release();
    var params: ObjectBuilder = .init(a);
    try params.put("includeHidden", .{ .bool = false });
    var out: std.ArrayList(types.Model) = .empty;
    for (try listPages(client, a, "model/list", &params, diag)) |value| {
        const native = JsonView.of(value);
        if (native.object() == null) continue;
        var model: types.Model = .{
            .id = providerkit.firstOf(&.{ native.str("model"), native.str("id") }),
            .display_name = native.str("displayName"),
            .description = native.str("description"),
            .is_default = native.flag("isDefault"),
            .default_effort = native.str("defaultReasoningEffort"),
            .default_service_tier = native.str("defaultServiceTier"),
            .raw = try native.raw(a),
        };
        var efforts: std.ArrayList([]const u8) = .empty;
        for (native.get("supportedReasoningEfforts").items()) |effort| try efforts.append(a, JsonView.of(effort).str("reasoningEffort"));
        model.efforts = efforts.items;
        var tiers: std.ArrayList([]const u8) = .empty;
        for (native.get("serviceTiers").items()) |tier| try tiers.append(a, JsonView.of(tier).str("id"));
        model.service_tiers = if (tiers.items.len > 0) tiers.items else try native.get("additionalSpeedTiers").strings(a);
        try out.append(a, model);
    }
    return out.items;
}
