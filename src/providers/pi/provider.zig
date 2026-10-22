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
    /// The result of "pi --version" once it named a supported version;
    /// guarded by `version_mutex` and allocated in `arena`. Anything else is
    /// checked again next time, so installing or upgrading Pi needs no
    /// restart.
    version_status: ?types.ProviderStatus = null,
    version_mutex: Io.Mutex = .init,

    pub fn of(ptr: *anyopaque) *Provider {
        return @ptrCast(@alignCast(ptr));
    }

    /// The installation status from "pi --version", in `a`.
    pub fn versionStatus(provider: *Provider, a: Allocator) Allocator.Error!types.ProviderStatus {
        provider.version_mutex.lockUncancelable(provider.env.io);
        defer provider.version_mutex.unlock(provider.env.io);
        if (provider.version_status) |cached| return agent.clone(types.ProviderStatus, a, cached);
        const status = kit.versionStatus(provider.env, a, provider.path(), provider.options.env);
        if (status.installed and status.@"error" == null and try versionProblem(a, status.version) == null)
            provider.version_status = try agent.clone(types.ProviderStatus, provider.arena.allocator(), status);
        return status;
    }

    /// Fails with `unsupported` when Pi runs but its version is unknown or
    /// below `minimum_version`. A Pi that cannot run is left for the
    /// launch to report.
    pub fn requireVersion(provider: *Provider, diag: ?*Diagnostics) Error!void {
        var arena: std.heap.ArenaAllocator = .init(provider.env.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        const status = try provider.versionStatus(a);
        if (!status.installed or status.@"error" != null) return;
        if (try versionProblem(a, status.version)) |message| return errors.fail(diag, .unsupported, "{s}", .{message});
    }

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
        // Tool updates and bash output stream (translate.zig toolUpdate,
        // bash_execution_update).
        .tool_output_stream = true,
        // Rewind forks into a new session file (control.zig rewind).
        .rewind_changes_session_id = true,
        // Extensions can start runs; their turn_started has origin
        // "extension" (translate.zig agent_start).
        .background_turns = true,
        // Not set: plans arrive whole from todo tool results
        // (plan_updated, no plan_delta); requests come from the main
        // agent only; Pi delivers steering after running tool calls
        // finish; Pi has no dialogs, other native requests, or calls
        // without a session.
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

const of = Provider.of;

/// The oldest Pi the bridge runs on: it registers MCP servers with
/// `pi.registerMcpServer`, added in Pi 0.99.0.
pub const minimum_version = "0.99.0";

/// Why Pi `version` cannot be used, or null when it is `minimum_version`
/// or newer. A pre-release sorts before its release.
pub fn versionProblem(a: Allocator, version: []const u8) Allocator.Error!?[]const u8 {
    if (version.len == 0) return try std.fmt.allocPrint(a, "could not determine the Pi version; {s} or newer is required", .{minimum_version});
    const order = kit.compareVersions(version, minimum_version);
    const pre_release = std.mem.indexOfScalar(u8, std.mem.sliceTo(version, '+'), '-') != null;
    if (order > 0 or (order == 0 and !pre_release)) return null;
    return try std.fmt.allocPrint(a, "Pi {s} is unsupported; {s} or newer is required", .{ version, minimum_version });
}

fn deinit(ptr: *anyopaque) void {
    const provider = of(ptr);
    provider.arena.deinit();
    provider.env.gpa.destroy(provider);
}

fn start(ptr: *anyopaque, opts: agent.StartOptions, diag: ?*Diagnostics) Error!*agent.Session {
    return pi_session.start(of(ptr), opts, diag);
}

/// Runs "pi --version" (once per provider), requires `minimum_version`,
/// and counts the models Pi can use: Pi has no account, so having at least
/// one model with credentials counts as signed in.
fn check(ptr: *anyopaque, a: Allocator, _: ?*Diagnostics) Error!types.ProviderStatus {
    const provider = of(ptr);
    var status = try provider.versionStatus(a);
    if (!status.installed or status.@"error" != null) return status;
    if (try versionProblem(a, status.version)) |message| {
        status.@"error" = .{ .kind = .unsupported, .message = message };
        return status;
    }
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

test "Pi versions below 0.99.0 are unsupported" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try std.testing.expectEqualStrings("Pi 0.98.9 is unsupported; 0.99.0 or newer is required", (try versionProblem(a, "0.98.9")).?);
    try std.testing.expectEqualStrings("Pi 0.99.0-rc.1 is unsupported; 0.99.0 or newer is required", (try versionProblem(a, "0.99.0-rc.1")).?);
    try std.testing.expectEqualStrings("could not determine the Pi version; 0.99.0 or newer is required", (try versionProblem(a, "")).?);
    try std.testing.expectEqual(null, try versionProblem(a, "0.99.0"));
    try std.testing.expectEqual(null, try versionProblem(a, "0.100.0"));
    try std.testing.expectEqual(null, try versionProblem(a, "1.4.0"));
}

test "check and start refuse an unsupported Pi without starting it" {
    if (@import("builtin").os.tag == .windows) return error.SkipZigTest;
    const gpa = std.testing.allocator;
    var threaded: std.Io.Threaded = .init(gpa, .{});
    defer threaded.deinit();
    const env: agent.Env = .{ .io = threaded.io(), .gpa = gpa, .environ = .empty };
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();
    try tmp.dir.writeFile(env.io, .{ .sub_path = "pi", .data = "#!/bin/sh\necho 0.98.0\n", .flags = .{ .permissions = .executable_file } });
    const p = try create(env, .{ .disable_bridge = true, .path = try tmp.dir.realPathFileAlloc(env.io, "pi", arena.allocator()) });
    defer p.deinit();
    const status = try p.check(arena.allocator(), null);
    try std.testing.expectEqualStrings("0.98.0", status.version);
    try std.testing.expectEqual(.unsupported, status.@"error".?.kind);
    try std.testing.expectEqualStrings("Pi 0.98.0 is unsupported; 0.99.0 or newer is required", status.@"error".?.message);
    var diag: Diagnostics = .{};
    try std.testing.expectError(error.Unsupported, p.start(.{}, &diag));
    try std.testing.expectEqualStrings("Pi 0.98.0 is unsupported; 0.99.0 or newer is required", diag.info.?.message);
}
