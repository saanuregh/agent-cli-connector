//! The Pi provider: capabilities, installation check, models, and the
//! persisted-session operations.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const agent = @import("agent");
const types = agent.types;
const errors = agent.errors;
const kit = agent.providerkit;
const JsonView = agent.json.JsonView;
const Error = errors.Error;
const Diagnostics = errors.Diagnostics;
const decode = @import("decode.zig");
const history = @import("history.zig");
const options = @import("options.zig");
const pi_session = @import("session.zig");

const name = @import("root.zig").name;

pub const Provider = struct {
    env: agent.Env,
    arena: std.heap.ArenaAllocator,
    options: options.Options,

    pub fn path(provider: *const Provider) []const u8 {
        return if (provider.options.path.len > 0) provider.options.path else "pi";
    }

    pub fn startupTimeoutMs(provider: *const Provider) i64 {
        return kit.orDefault(provider.options.startup_timeout_ms, kit.default_startup_timeout_ms);
    }

    pub fn shutdownGraceMs(provider: *const Provider) i64 {
        return kit.orDefault(provider.options.shutdown_grace_ms, kit.default_shutdown_grace_ms);
    }

    /// Finds sessions where a Pi process with `extra_env` and the session
    /// directory `session_dir` (empty means the provider's) stores them.
    pub fn store(provider: *const Provider, session_dir: []const u8, extra_env: ?types.StringMap) history.Store {
        return .{
            .env = provider.env,
            .session_dir = kit.firstOf(&.{ session_dir, provider.options.session_dir }),
            .extra_env = extra_env orelse provider.options.env,
        };
    }
};

/// The supported uniform features. Without the bridge extension,
/// permission prompts and modes, host tools, MCP servers, and output
/// schemas are unsupported.
pub fn capabilities(with_bridge: bool) types.Capabilities {
    const pm = types.permission_mode;
    var caps: types.Capabilities = .{
        .streaming = true,
        .reasoning_stream = true,
        .images = true,
        .steer = true,
        .queue = true,
        .interrupt = true,
        .interrupt_turn = true,
        .set_model = true,
        .set_effort = true,
        .user_questions = true,
        .resume_session = true,
        .fork = true,
        .fork_at_turn = true,
        .rewind = true,
        .compact = true,
        .set_title = true,
        .system_prompt = true,
        .cost_reporting = true,
        // Subagent extensions, such as Pi's example subagent tool, report
        // their runs in tool results.
        .subagents = true,
        .tasks = true,
        // With a todo extension, such as Pi's example todo tool.
        .plans = true,
        .check = true,
        .list_models = true,
        .list_sessions = true,
        .read_history = true,
        .delete_session = true,
        .list_subagents = true,
    };
    if (with_bridge) {
        caps.permission_modes = &.{ pm.default, pm.accept_edits, pm.plan, pm.read_only, pm.dont_ask, pm.bypass };
        caps.permission_prompt = true;
        caps.host_tools = true;
        caps.mcp_servers = true;
        caps.output_schema = true;
    }
    return caps;
}

fn vtableFor(comptime with_bridge: bool) agent.Provider.VTable {
    return .{
        .name = name,
        .capabilities = capabilities(with_bridge),
        .start = start,
        .deinit = deinit,
        .check = check,
        .list_models = listModels,
        .list_sessions = listSessions,
        .read_history = readHistory,
        .delete_session = deleteSession,
        .list_subagents = listSubagents,
    };
}

const with_bridge_vtable = vtableFor(true);
const without_bridge_vtable = vtableFor(false);

/// Creates the provider. Sessions keep a pointer to it: deinit it after
/// its sessions.
pub fn create(env: agent.Env, opts: options.Options) !agent.Provider {
    const provider = try env.gpa.create(Provider);
    provider.* = .{ .env = env, .arena = .init(env.gpa), .options = .{} };
    provider.options = agent.clone(options.Options, provider.arena.allocator(), opts) catch |e| {
        provider.arena.deinit();
        env.gpa.destroy(provider);
        return e;
    };
    return .{ .ptr = provider, .vtable = if (opts.disable_bridge) &without_bridge_vtable else &with_bridge_vtable };
}

fn of(ptr: *anyopaque) *Provider {
    return @ptrCast(@alignCast(ptr));
}

fn deinit(ptr: *anyopaque) void {
    const provider = of(ptr);
    provider.arena.deinit();
    provider.env.gpa.destroy(provider);
}

fn start(ptr: *anyopaque, opts: agent.StartOptions, diag: ?*Diagnostics) Error!*agent.Session {
    return pi_session.start(of(ptr), opts, diag);
}

/// Runs "pi --version" and counts the models Pi can use: Pi has no
/// account, so having at least one model with credentials counts as signed
/// in.
fn check(ptr: *anyopaque, a: Allocator, _: ?*Diagnostics) Error!types.ProviderStatus {
    const provider = of(ptr);
    var status = kit.versionStatus(provider.env, a, provider.path(), provider.options.env);
    if (!status.installed or status.@"error" != null) return status;
    var models_diag: Diagnostics = .{};
    const models = listModels(ptr, a, &models_diag) catch |e| return kit.failedStatus(a, status, &models_diag, e);
    status.auth = .{ .authenticated = models.len > 0 };
    return status;
}

/// The configured models, as "provider/id". Extensions load, since they
/// can register model providers.
fn listModels(ptr: *anyopaque, a: Allocator, diag: ?*Diagnostics) Error![]const types.Model {
    const provider = of(ptr);
    const io = provider.env.io;
    const dir = try kit.tempDir(provider.env, a, "agent-pi-models", diag);
    // Best effort: a left-over directory under TMPDIR is harmless.
    defer Io.Dir.cwd().deleteTree(io, dir) catch {};
    const session = try start(ptr, .{ .config = .{
        .cwd = dir,
        .ephemeral = true,
        .provider_options = .of("{\"disable_bridge\":true,\"no_skills\":true,\"no_prompt_templates\":true,\"no_context_files\":true,\"no_mcp\":true}"),
    } }, diag);
    defer session.deinit();
    const pi = pi_session.PiSession.of(session);
    const data = try pi.command(a, "get_available_models", .{}, diag);
    const info = try session.info(a);
    var out: std.ArrayList(types.Model) = .empty;
    for (data.get("models").items()) |v| {
        const model = JsonView.of(v);
        const id = try std.fmt.allocPrint(a, "{s}/{s}", .{ model.str("provider"), model.str("id") });
        const efforts = try decode.modelEfforts(a, model);
        try out.append(a, .{
            .id = id,
            .display_name = model.str("name"),
            .provider = model.str("provider"),
            .context_window = model.int("contextWindow"),
            .is_default = std.mem.eql(u8, id, info.model),
            .raw = try model.raw(a),
            .input_modalities = try model.get("input").strings(a),
            .efforts = efforts,
            .default_effort = decode.clampEffort(info.effort, efforts),
        });
    }
    return out.toOwnedSlice(a);
}

fn listSessions(ptr: *anyopaque, a: Allocator, opts: types.ListSessionsOptions, diag: ?*Diagnostics) Error![]const types.SessionSummary {
    return of(ptr).store("", null).listSessions(a, opts, diag);
}

fn readHistory(ptr: *anyopaque, a: Allocator, session_id: []const u8, opts: types.ReadHistoryOptions, diag: ?*Diagnostics) Error![]const types.Item {
    return of(ptr).store("", null).readHistory(a, session_id, opts, diag);
}

fn deleteSession(ptr: *anyopaque, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) Error!void {
    const provider = of(ptr);
    var arena: std.heap.ArenaAllocator = .init(provider.env.gpa);
    defer arena.deinit();
    return provider.store("", null).deleteSession(arena.allocator(), session_id, cwd, diag);
}

fn listSubagents(ptr: *anyopaque, a: Allocator, session_id: []const u8, cwd: []const u8, diag: ?*Diagnostics) Error![]const types.Task {
    return of(ptr).store("", null).listSubagents(a, session_id, cwd, diag);
}
