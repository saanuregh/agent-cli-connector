//! The Claude Code provider: starting sessions and the provider-level
//! operations (check, models, persisted sessions).
const std = @import("std");
const builtin = @import("builtin");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const agent = @import("agent");
const types = agent.types;
const errors = agent.errors;
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const Diagnostics = agent.Diagnostics;
const kit = agent.providerkit;
const eql = kit.eql;

const options = @import("options.zig");
const session_mod = @import("session.zig");
const history = @import("history.zig");
const subagents = @import("subagents.zig");
const ClaudeSession = session_mod.ClaudeSession;
const name = @import("root.zig").name;

/// The policy file next to which the CLI refuses --strict-mcp-config, as
/// https://code.claude.com/docs/en/managed-mcp lists.
const managed_mcp_config = switch (builtin.os.tag) {
    .macos => "/Library/Application Support/ClaudeCode/managed-mcp.json",
    .windows => "C:\\Program Files\\ClaudeCode\\managed-mcp.json",
    else => "/etc/claude-code/managed-mcp.json",
};

pub const Provider = struct {
    env: agent.Env,
    /// Owns `options`.
    arena: std.heap.ArenaAllocator,
    options: options.Options,

    pub fn path(provider: *const Provider) []const u8 {
        return if (provider.options.path.len > 0) provider.options.path else "claude";
    }

    pub fn startupTimeoutMs(provider: *const Provider) i64 {
        return kit.orDefault(provider.options.startup_timeout_ms, kit.default_startup_timeout_ms);
    }

    pub fn shutdownGraceMs(provider: *const Provider) i64 {
        return kit.orDefault(provider.options.shutdown_grace_ms, kit.default_shutdown_grace_ms);
    }

    /// The Claude config directory a session uses.
    pub fn configDir(provider: *const Provider, a: Allocator, cfg: types.Config, diag: ?*Diagnostics) errors.Error![]const u8 {
        const key = "CLAUDE_CONFIG_DIR";
        if (cfg.env) |e| if (e.map.get(key)) |d| if (d.len > 0) return d;
        if (provider.options.env) |e| if (e.map.get(key)) |d| if (d.len > 0) return d;
        if (kit.getEnv(provider.env, key)) |d| if (d.len > 0) return d;
        return std.fs.path.join(a, &.{ try kit.homeDir(provider.env, diag), ".claude" });
    }
};

pub fn create(env: agent.Env, provider_options: options.Options) !agent.Provider {
    const provider = try env.gpa.create(Provider);
    provider.* = .{ .env = env, .arena = .init(env.gpa), .options = undefined };
    provider.options = agent.clone(options.Options, provider.arena.allocator(), provider_options) catch |e| {
        provider.arena.deinit();
        env.gpa.destroy(provider);
        return e;
    };
    return .{ .ptr = provider, .vtable = &.{
        .name = name,
        .capabilities = session_mod.provider_capabilities,
        .start = start,
        .deinit = deinit,
        .check = check,
        .list_models = listModels,
        .list_sessions = history.listSessions,
        .read_history = history.readHistory,
        .delete_session = history.deleteSession,
        .list_subagents = subagents.listSubagents,
    } };
}

fn deinit(ptr: *anyopaque) void {
    const provider: *Provider = @ptrCast(@alignCast(ptr));
    provider.arena.deinit();
    provider.env.gpa.destroy(provider);
}

fn start(ptr: *anyopaque, start_options: agent.StartOptions, diag: ?*Diagnostics) errors.Error!*agent.Session {
    const provider: *Provider = @ptrCast(@alignCast(ptr));
    return session_mod.start(provider, start_options, false, diag);
}

/// A short-lived session in a temporary directory that loads only the
/// given setting sources and no MCP servers, hooks, or tools.
const Isolated = struct {
    session: *agent.Session,
    dir: []const u8,

    fn open(provider: *Provider, a: Allocator, setting_sources: []const []const u8, diag: ?*Diagnostics) errors.Error!Isolated {
        const io = provider.env.io;
        const dir = try kit.tempDir(provider.env, a, "agent-claude", diag);
        // Best-effort cleanup of the temporary directory.
        errdefer Io.Dir.cwd().deleteTree(io, dir) catch {};
        const strict = if (Io.Dir.cwd().access(io, managed_mcp_config, .{})) |_| false else |_| true;
        const provider_options = try RawJson.encode(a, .{
            .setting_sources = setting_sources,
            .settings = .{ .disableAllHooks = true },
            .builtin_tools = &[_][]const u8{},
            .strict_mcp_config = strict,
        });
        var env: types.StringMap = .{};
        try env.map.put(a, "ENABLE_CLAUDEAI_MCP_SERVERS", "false");
        try env.map.put(a, "CLAUDE_CODE_AUTO_CONNECT_IDE", "0");
        const session = try session_mod.start(provider, .{ .config = .{
            .cwd = dir,
            .ephemeral = true,
            .env = env,
            .provider_options = provider_options,
        } }, true, diag);
        return .{ .session = session, .dir = dir };
    }

    fn close(isolated: Isolated, provider: *Provider) void {
        isolated.session.deinit();
        // Best-effort cleanup of the temporary directory.
        Io.Dir.cwd().deleteTree(provider.env.io, isolated.dir) catch {};
    }
};

/// Reports the CLI version from "claude --version" and the sign-in state
/// from the account in a short-lived initialize handshake that loads no MCP
/// servers, hooks, or tools.
fn check(ptr: *anyopaque, a: Allocator, _: ?*Diagnostics) errors.Error!types.ProviderStatus {
    const provider: *Provider = @ptrCast(@alignCast(ptr));
    var status = kit.versionStatus(provider.env, a, provider.path(), provider.options.env);
    if (!status.installed or status.@"error" != null) return status;
    // A failed handshake is reported in the status, with its details.
    var open_diag: Diagnostics = .{};
    const isolated = Isolated.open(provider, a, &.{}, &open_diag) catch |e| return kit.failedStatus(a, status, &open_diag, e);
    defer isolated.close(provider);
    const init = JsonView.parseBytes(a, try ClaudeSession.of(isolated.session).initRaw(a));
    if (status.version.len == 0) status.version = init.str("claude_code_version");
    status.auth = try authStatus(a, init.get("account"));
    return status;
}

/// The sign-in state from the initialize response's account.
fn authStatus(a: Allocator, account: JsonView) Allocator.Error!types.AuthStatus {
    const provider = account.str("apiProvider");
    const key_source = account.str("apiKeySource");
    const token_source = account.str("tokenSource");
    var method: []const u8 = "";
    if (provider.len > 0 and !eql(provider, "firstParty")) {
        method = provider;
    } else if (key_source.len > 0 and !eql(key_source, "none")) {
        method = "apiKey";
    } else if ((token_source.len > 0 and !eql(token_source, "none")) or account.str("email").len > 0 or account.str("subscriptionType").len > 0) {
        method = "oauth";
    }
    return .{
        .authenticated = method.len > 0,
        .method = method,
        .email = account.str("email"),
        .plan = account.str("subscriptionType"),
        .raw = try account.raw(a),
    };
}

/// The models from the initialize handshake of a short-lived process. It
/// loads no MCP servers, hooks, or tools, but reads user settings, so
/// model overrides and gateway variables there apply.
fn listModels(ptr: *anyopaque, a: Allocator, diag: ?*Diagnostics) errors.Error![]const types.Model {
    const provider: *Provider = @ptrCast(@alignCast(ptr));
    const isolated = try Isolated.open(provider, a, &.{"user"}, diag);
    defer isolated.close(provider);
    const init = JsonView.parseBytes(a, try ClaudeSession.of(isolated.session).initRaw(a));
    const list = init.get("models").items();
    const out = try a.alloc(types.Model, list.len);
    for (list, out) |value, *o| {
        const model = JsonView.of(value);
        const id = model.str("value");
        const resolved = model.str("resolvedModel");
        o.* = .{
            .id = id,
            .display_name = model.str("displayName"),
            .description = model.str("description"),
            .is_default = eql(id, "default"),
            .resolved_id = if (eql(resolved, id)) "" else resolved,
            .efforts = try model.get("supportedEffortLevels").strings(a),
            .raw = try model.raw(a),
        };
    }
    return out;
}
