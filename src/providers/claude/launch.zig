//! Launching the CLI: command line, environment, the initialize handshake,
//! and MCP server registration.
const std = @import("std");
const Allocator = std.mem.Allocator;
const agent = @import("agent");
const types = agent.types;
const errors = agent.errors;
const JsonView = agent.json.JsonView;
const RawJson = agent.json.RawJson;
const ObjectBuilder = agent.json.ObjectBuilder;
const Value = std.json.Value;
const Diagnostics = agent.Diagnostics;
const kit = agent.providerkit;
const eql = kit.eql;

const options = @import("options.zig");
const control = @import("control.zig");
const permissions = @import("permissions.zig");
const session_mod = @import("session.zig");
const ClaudeSession = session_mod.ClaudeSession;
const name = @import("root.zig").name;

/// Mark a process as running inside a Claude Code session; inherited, they
/// make the CLI behave as a nested session.
const parent_session_env = [_][]const u8{ "CLAUDECODE", "CLAUDE_CODE_SSE_PORT", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_AGENT_SDK_VERSION" };
/// Inherited variables the TypeScript SDK removes too.
const sdk_unset_env = [_][]const u8{ "CLAUDE_CODE_AGENT_PROGRESS_SUMMARIES", "NODE_OPTIONS", "DEBUG" };

/// Starts the CLI's error for a resume target it does not have.
pub const no_conversation_text = "No conversation found with session ID";
/// Starts the CLI's refusal of a truncating resume whose dropped range
/// holds more than the declared turn.
pub const drops_turn_rejected_text = "Resume rejected by --resume-drops-turn:";
/// The error code launch reports for that refusal.
pub const drops_turn_rejected_code = "resume_drops_turn_rejected";

/// Hook events the effort hooks observe, in callback ID order.
pub const effort_hook_events = [_][]const u8{ "PostToolUse", "PreToolUse", "SubagentStop" };

pub const Init = struct {
    /// The initialize response.
    raw: []const u8,
    /// The effort the CLI's settings apply, or "".
    applied_effort: []const u8,
};

/// Starts a CLI process, makes it the session's latest launch, and runs the
/// initialize handshake, then registers `cfg.mcp_servers` through
/// mcp_set_servers when needed. A provisional process does not end the
/// session when it exits; a rewind restart adopts it once it worked.
pub fn launch(claude: *ClaudeSession, a: Allocator, cfg: types.Config, session_options: options.SessionOptions, session_id: []const u8, provisional: bool, diag: ?*Diagnostics) errors.Error!Init {
    const argv = try buildArgs(claude, a, cfg, session_options, session_id, diag);
    var unset: std.ArrayList([]const u8) = .empty;
    try unset.appendSlice(a, &parent_session_env);
    try unset.appendSlice(a, &sdk_unset_env);
    try unset.appendSlice(a, cfg.unset_env);
    const next = try session_mod.spawnLaunch(claude, .{
        .provider = name,
        .argv = argv,
        .command_prefix = cfg.command_prefix,
        .cwd = cfg.cwd,
        .env = try buildEnv(claude, a, cfg, session_options),
        .unset_env = unset.items,
    }, provisional, diag);
    {
        claude.launch_mutex.lockUncancelable(claude.io());
        defer claude.launch_mutex.unlock(claude.io());
        next.previous = claude.launch;
        claude.launch = next;
        if (claude.closing.load(.acquire)) next.retired.store(true, .release);
    }
    next.ready.set(claude.io());
    if (next.retired.load(.acquire)) {
        next.process.stop(claude.provider.shutdownGraceMs());
        return claude.rt().closedError(diag);
    }

    const request = RawJson.of(try initializeRequest(claude, a, cfg, session_options));
    const raw = control.control(claude, a, "initialize", request, .{ .timeout_ms = claude.provider.startupTimeoutMs() }, diag) catch |e| {
        const translator = next.translator;
        const exit = next.exitError();
        const stderr = if (exit) |x| x.stderr else "";
        if (translator.session_missing.load(.acquire) or std.mem.indexOf(u8, stderr, no_conversation_text) != null) {
            return errors.fail(diag, .session_not_found, "claude: resume {s}: session not found", .{cfg.resume_id});
        }
        if (translator.drops_turn_rejected.load(.acquire) or std.mem.indexOf(u8, stderr, drops_turn_rejected_text) != null) {
            return errors.failWith(diag, .{ .kind = .protocol, .code = drops_turn_rejected_code, .message = "claude: the CLI refused the truncated resume" });
        }
        if (exit) |x| {
            var info = x;
            info.message = try std.fmt.allocPrint(a, "claude: initialize: {s}", .{x.message});
            return errors.failWith(diag, info);
        }
        return e;
    };
    try claude.setInitRaw(raw);
    var applied: []const u8 = "";
    if (!claude.skip_applied_effort) {
        // The CLI reports its effort only when a turn starts; its settings
        // already hold the level it applies. Without them the effort stays
        // unknown, so the failure's details are not needed.
        if (control.control(claude, a, "get_settings", null, .{ .timeout_ms = 3000 }, null)) |settings| {
            applied = JsonView.parseBytes(a, settings).get("applied").str("effort");
        } else |_| {}
    }
    try registerMcpServers(claude, a, cfg, session_options, diag);
    return .{ .raw = raw, .applied_effort = applied };
}

/// Registers `cfg.mcp_servers` through mcp_set_servers when `mcpViaControl`
/// says so, and reports servers the CLI could not add as notices.
fn registerMcpServers(claude: *ClaudeSession, a: Allocator, cfg: types.Config, session_options: options.SessionOptions, diag: ?*Diagnostics) errors.Error!void {
    if (!mcpViaControl(cfg, session_options)) return;
    const servers = try mcpConfig(a, cfg.mcp_servers);
    if (servers.object.count() == 0) return;
    var register_diag: Diagnostics = .{};
    const raw = setMcpServers(claude, a, servers, &register_diag) catch |e| {
        const info = Diagnostics.of(&register_diag, e);
        return errors.fail(diag, info.kind, "claude: register MCP servers: {s}", .{info.message});
    };
    const failures = JsonView.parseBytes(a, raw).get("errors").object() orelse return;
    for (try kit.sortedKeys(a, failures.keys())) |k| {
        const message = try std.fmt.allocPrint(a, "{s}: {s}", .{ k, JsonView.of(failures.get(k)).string() });
        claude.emit(.{ .payload = .{ .notice = .{ .level = .warning, .code = "mcp_server_failed", .message = message } } });
    }
}

/// Replaces the dynamically added MCP servers (`servers` in the CLI's
/// config shape). The in-process host tool server stays registered with its
/// tool policies and timeout.
pub fn setMcpServers(claude: *ClaudeSession, a: Allocator, servers_in: Value, diag: ?*Diagnostics) errors.Error![]const u8 {
    var servers = servers_in;
    if (claude.tools) |tools| {
        // Without its settings the CLI would drop the host server's tool
        // policies.
        var entry: ObjectBuilder = .init(a);
        if (try toolServerSettings(claude, a)) |settings| entry.map = settings.object;
        try entry.str("type", "sdk");
        try entry.str("name", tools.name);
        try servers.object.put(a, tools.name, entry.value());
    }
    var payload: ObjectBuilder = .init(a);
    try payload.put("servers", servers);
    return control.control(claude, a, "mcp_set_servers", try payload.toRaw(), .{}, diag);
}

fn flag(a: Allocator, flag_name: []const u8, value: []const u8) Allocator.Error![]const u8 {
    return std.fmt.allocPrint(a, "--{s}={s}", .{ flag_name, value });
}

fn allSkills(skills: []const []const u8) bool {
    return skills.len == 1 and eql(skills[0], "all");
}

pub fn buildArgs(claude: *ClaudeSession, a: Allocator, cfg: types.Config, session_options: options.SessionOptions, session_id: []const u8, diag: ?*Diagnostics) errors.Error![]const []const u8 {
    var args: std.ArrayList([]const u8) = .empty;
    try args.append(a, claude.provider.path());
    try args.appendSlice(a, &.{ "--output-format", "stream-json", "--verbose", "--input-format", "stream-json" });
    if (session_options.thinking) |thinking| {
        if (eql(thinking.type, "enabled")) {
            if (thinking.budget_tokens > 0) {
                try args.append(a, try flag(a, "max-thinking-tokens", try std.fmt.allocPrint(a, "{d}", .{thinking.budget_tokens})));
            } else try args.append(a, "--thinking=adaptive");
        } else if (eql(thinking.type, "disabled")) {
            try args.append(a, "--thinking=disabled");
        } else if (eql(thinking.type, "adaptive") or thinking.type.len == 0) {
            try args.append(a, "--thinking=adaptive");
        } else return errors.fail(diag, .invalid_params, "claude: unknown thinking type \"{s}\"", .{thinking.type});
        if (!eql(thinking.type, "disabled") and thinking.display.len > 0) try args.append(a, try flag(a, "thinking-display", thinking.display));
    }
    if (cfg.effort.len > 0) try args.append(a, try flag(a, "effort", session_mod.nativeEffort(cfg.effort)));
    if (session_options.max_turns > 0) try args.append(a, try flag(a, "max-turns", try std.fmt.allocPrint(a, "{d}", .{session_options.max_turns})));
    if (session_options.max_budget_usd > 0) try args.append(a, try flag(a, "max-budget-usd", try std.fmt.allocPrint(a, "{d}", .{session_options.max_budget_usd})));
    if (session_options.task_budget_tokens > 0) try args.append(a, try flag(a, "task-budget", try std.fmt.allocPrint(a, "{d}", .{session_options.task_budget_tokens})));
    if (cfg.model.len > 0) try args.append(a, try flag(a, "model", cfg.model));
    if (session_options.agent.len > 0) try args.append(a, try flag(a, "agent", session_options.agent));
    if (session_options.betas.len > 0) try args.append(a, try flag(a, "betas", try std.mem.join(a, ",", session_options.betas)));
    if (cfg.output_schema) |schema| try args.append(a, try flag(a, "json-schema", schema.bytes));
    if (session_options.debug_file.len > 0) {
        try args.append(a, try flag(a, "debug-file", session_options.debug_file));
    } else if (session_options.debug) try args.append(a, "--debug");
    try args.append(a, try flag(a, "permission-prompt-tool", if (session_options.permission_prompt_tool.len > 0) session_options.permission_prompt_tool else "stdio"));
    if (session_options.no_permission_prompts) try args.append(a, try flag(a, "permission-prompts", "none"));
    if (cfg.resume_id.len > 0) try args.append(a, try flag(a, "resume", cfg.resume_id));

    var allowed: std.ArrayList([]const u8) = .empty;
    try allowed.appendSlice(a, cfg.allowed_tools);
    if (cfg.mcp_servers) |servers| {
        for (try kit.sortedKeys(a, servers.map.keys())) |k| for (servers.map.get(k).?.approved_tools) |tool| {
            try allowed.append(a, try std.fmt.allocPrint(a, "mcp__{s}__{s}", .{ k, tool }));
        };
    }
    if (claude.tools) |server| for (cfg.tools) |tool| if (!tool.needs_approval) {
        try allowed.append(a, try std.fmt.allocPrint(a, "mcp__{s}__{s}", .{ server.name, tool.name }));
    };
    if (session_options.skills) |skills| {
        if (allSkills(skills)) {
            try allowed.append(a, "Skill");
        } else for (skills) |skill| try allowed.append(a, try std.fmt.allocPrint(a, "Skill({s})", .{skill}));
    }
    if (allowed.items.len > 0) try args.append(a, try flag(a, "allowedTools", try std.mem.join(a, ",", allowed.items)));
    if (cfg.disallowed_tools.len > 0) try args.append(a, try flag(a, "disallowedTools", try std.mem.join(a, ",", cfg.disallowed_tools)));
    if (session_options.builtin_tools) |tools| try args.append(a, try flag(a, "tools", try std.mem.join(a, ",", tools)));
    const servers = try mcpConfig(a, cfg.mcp_servers);
    if (servers.object.count() > 0 and !mcpViaControl(cfg, session_options)) {
        var wrap: ObjectBuilder = .init(a);
        try wrap.put("mcpServers", servers);
        try args.appendSlice(a, &.{ "--mcp-config", try agent.json.encodeValue(a, wrap.value()) });
    }
    if (session_options.setting_sources) |sources| try args.append(a, try flag(a, "setting-sources", try std.mem.join(a, ",", sources)));
    if (session_options.strict_mcp_config) try args.append(a, "--strict-mcp-config");
    if (cfg.permission_mode.len > 0) {
        const native = permissions.nativeMode(cfg.permission_mode) orelse return errors.unsupported(diag, name, "permission mode read_only");
        try args.append(a, try flag(a, "permission-mode", native));
    }
    if (claude.bypassAllowed(cfg, session_options)) try args.append(a, "--allow-dangerously-skip-permissions");
    if (session_options.fallback_model.len > 0) {
        if (eql(session_options.fallback_model, cfg.model)) return errors.fail(diag, .invalid_params, "claude: fallback_model must differ from model", .{});
        try args.append(a, try flag(a, "fallback-model", session_options.fallback_model));
    }
    if (session_options.include_hook_events) try args.append(a, "--include-hook-events");
    if (!session_options.no_partial_messages) try args.append(a, "--include-partial-messages");
    if (session_options.project_config_root.len > 0) try args.append(a, try flag(a, "project-config-root", session_options.project_config_root));
    for (cfg.additional_directories) |dir| try args.append(a, try flag(a, "add-dir", dir));
    for (session_options.plugins) |plugin| try args.append(a, try flag(a, if (plugin.skip_mcp_discovery) "plugin-dir-no-mcp" else "plugin-dir", plugin.path));
    if (cfg.fork) try args.append(a, "--fork-session");
    if (session_options.resume_session_at.len > 0) try args.append(a, try flag(a, "resume-session-at", session_options.resume_session_at));
    if (session_options.resume_drops_turn.len > 0) try args.append(a, try flag(a, "resume-drops-turn", session_options.resume_drops_turn));
    if (cfg.resume_id.len == 0 or cfg.fork) try args.append(a, try flag(a, "session-id", session_id));
    if (cfg.ephemeral) try args.append(a, "--no-session-persistence");
    if (session_options.managed_settings) |managed| try args.appendSlice(a, &.{ "--managed-settings", try settingsArg(a, managed) });
    const settings = try mergedSettings(a, session_options.settings, session_options.sandbox, diag);
    if (settings.len > 0) try args.appendSlice(a, &.{ "--settings", settings });
    try args.appendSlice(a, claude.provider.options.extra_args);
    try args.appendSlice(a, session_options.extra_args);
    return args.items;
}

/// A settings value as a flag argument: a JSON string is a file path or
/// inline JSON and passes as is; anything else is encoded.
fn settingsArg(a: Allocator, raw: RawJson) Allocator.Error![]const u8 {
    const value = agent.json.valueOf(a, raw) orelse .null;
    if (value == .string) return value.string;
    return raw.bytes;
}

/// Combines the settings option with the sandbox option the way the
/// TypeScript SDK does: the sandbox block is merged into inline settings; a
/// settings file path cannot be combined with it.
fn mergedSettings(a: Allocator, settings: ?RawJson, sandbox: ?RawJson, diag: ?*Diagnostics) errors.Error![]const u8 {
    const sandbox_raw = sandbox orelse return if (settings) |raw| try settingsArg(a, raw) else "";
    var merged_settings: ObjectBuilder = .init(a);
    if (settings) |raw| {
        const value = agent.json.valueOf(a, raw) orelse .null;
        switch (value) {
            .null => {},
            .string => |text| {
                const inner = std.json.parseFromSliceLeaky(Value, a, text, .{}) catch null;
                if (inner == null or inner.? != .object)
                    return errors.fail(diag, .invalid_params, "claude: sandbox cannot be combined with a settings file path; put the sandbox in the file", .{});
                merged_settings.map = inner.?.object;
            },
            .object => |object| merged_settings.map = object,
            else => return errors.fail(diag, .invalid_params, "claude: settings must be an object", .{}),
        }
    }
    var merged_sandbox: ObjectBuilder = .init(a);
    if (merged_settings.map.get("sandbox")) |existing| if (existing == .object) {
        try merged_sandbox.putAll(existing.object);
    };
    const sandbox_value = agent.json.valueOf(a, sandbox_raw) orelse .null;
    if (sandbox_value == .object) try merged_sandbox.putAll(sandbox_value.object);
    if (merged_sandbox.map.get("enabled")) |enabled| if (enabled == .bool and enabled.bool and !merged_sandbox.has("failIfUnavailable")) {
        try merged_sandbox.put("failIfUnavailable", .{ .bool = true });
    };
    try merged_settings.put("sandbox", merged_sandbox.value());
    return agent.json.encodeValue(a, merged_settings.value());
}

/// MCP servers in the CLI's config shape, by sorted name. The tool timeout
/// becomes the per-server "timeout" in milliseconds; approved tools of http
/// and sse servers become "always_allow" per-tool policies.
pub fn mcpConfig(a: Allocator, servers: ?std.json.ArrayHashMap(types.McpServer)) Allocator.Error!Value {
    var out: ObjectBuilder = .init(a);
    const map = servers orelse return out.value();
    for (try kit.sortedKeys(a, map.map.keys())) |k| {
        const server = map.map.get(k).?;
        if (server.enabled) |enabled| if (!enabled) continue;
        var entry: ObjectBuilder = .init(a);
        switch (server.resolvedTransport()) {
            .http, .sse => |transport| {
                try entry.str("type", @tagName(transport));
                try entry.str("url", server.url);
                if (server.headers) |headers| if (headers.map.count() > 0) try entry.put("headers", try kit.stringMapValue(a, headers));
                if (server.approved_tools.len > 0) {
                    var policies: std.json.Array = .init(a);
                    for (server.approved_tools) |tool| try policies.append(try toolPolicy(a, tool, "always_allow"));
                    try entry.put("tools", .{ .array = policies });
                }
            },
            .stdio => {
                try entry.str("type", "stdio");
                try entry.str("command", server.command);
                if (server.args.len > 0) try entry.put("args", try agent.json.stringArray(a, server.args));
                if (server.env) |env| if (env.map.count() > 0) try entry.put("env", try kit.stringMapValue(a, env));
            },
        }
        if (server.tool_timeout_ms > 0) try entry.put("timeout", .{ .integer = server.tool_timeout_ms });
        if (server.always_load) try entry.put("alwaysLoad", .{ .bool = true });
        try out.put(k, entry.value());
    }
    return out.value();
}

fn toolPolicy(a: Allocator, tool: []const u8, policy: []const u8) Allocator.Error!Value {
    return agent.json.toValue(a, .{ .name = tool, .permission_policy = policy });
}

/// Whether `cfg.mcp_servers` are registered with mcp_set_servers instead of
/// --mcp-config: when asked for, and whenever a server has values that may
/// be secrets, which other local users could read from the command line.
pub fn mcpViaControl(cfg: types.Config, session_options: options.SessionOptions) bool {
    if (session_options.mcp_via_control) return true;
    const map = cfg.mcp_servers orelse return false;
    for (map.map.values()) |server| {
        if (server.enabled) |enabled| if (!enabled) continue;
        const has_headers = if (server.headers) |h| h.map.count() > 0 else false;
        const has_env = if (server.env) |e| e.map.count() > 0 else false;
        if (has_headers or has_env or server.args.len > 0 or urlMayHoldSecret(server.url)) return true;
    }
    return false;
}

/// A server URL with credentials or a query string.
fn urlMayHoldSecret(url: []const u8) bool {
    if (url.len == 0) return false;
    if (std.mem.indexOfScalar(u8, url, '?') != null) return true;
    const rest = if (std.mem.indexOf(u8, url, "://")) |i| url[i + 3 ..] else return true;
    const authority = rest[0 .. std.mem.indexOfAny(u8, rest, "/#") orelse rest.len];
    return std.mem.indexOfScalar(u8, authority, '@') != null;
}

/// The settings of the in-process host tool server: the tool timeout and
/// the "always_ask" policy of tools that need approval; null when none.
pub fn toolServerSettings(claude: *ClaudeSession, a: Allocator) Allocator.Error!?Value {
    if (claude.tools == null) return null;
    var settings: ObjectBuilder = .init(a);
    if (claude.opts.tool_timeout_ms > 0) try settings.put("timeout", .{ .integer = claude.opts.tool_timeout_ms });
    var policies: std.json.Array = .init(a);
    for (claude.cfg.tools) |tool| if (tool.needs_approval) {
        try policies.append(try toolPolicy(a, tool.name, "always_ask"));
    };
    if (policies.items.len > 0) try settings.put("tools", .{ .array = policies });
    if (settings.map.count() == 0) return null;
    return settings.value();
}

fn buildEnv(claude: *ClaudeSession, a: Allocator, cfg: types.Config, session_options: options.SessionOptions) Allocator.Error!types.StringMap {
    var env: types.StringMap = .{};
    try env.map.put(a, "CLAUDE_CODE_ENTRYPOINT", "sdk-ts");
    try env.map.put(a, "CLAUDE_AGENT_SDK_VERSION", options.protocol_version);
    try env.map.put(a, "CLAUDE_CODE_SDK_READS_SESSION_STATE", "1");
    if (claude.provider.options.client_app.len > 0) try env.map.put(a, "CLAUDE_AGENT_SDK_CLIENT_APP", claude.provider.options.client_app);
    if (session_options.enable_file_checkpointing) try env.map.put(a, "CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING", "true");
    var startup: i64 = 0;
    if (cfg.mcp_servers) |map| for (map.map.values()) |server| {
        if (server.enabled) |enabled| if (!enabled) continue;
        startup = @max(startup, server.startup_timeout_ms);
    };
    if (startup > 0) try env.map.put(a, "MCP_TIMEOUT", try std.fmt.allocPrint(a, "{d}", .{startup}));
    if (session_options.tool_config) |tool_config| {
        const format = JsonView.parseBytes(a, tool_config.bytes).get("askUserQuestion").str("previewFormat");
        if (format.len > 0) try env.map.put(a, "CLAUDE_CODE_QUESTION_PREVIEW_FORMAT", format);
    }
    try putAll(a, &env, claude.provider.options.env);
    try putAll(a, &env, cfg.env);
    return env;
}

fn putAll(a: Allocator, env: *types.StringMap, extra: ?types.StringMap) Allocator.Error!void {
    const map = extra orelse return;
    for (map.map.keys(), map.map.values()) |k, v| try env.map.put(a, k, v);
}

/// The initialize request for the session's settings.
pub fn initializeRequest(claude: *ClaudeSession, a: Allocator, cfg: types.Config, session_options: options.SessionOptions) Allocator.Error![]const u8 {
    var request: ObjectBuilder = .init(a);
    if (session_options.track_subagent_effort) {
        // Observation-only hooks that read the effort a subagent runs at,
        // which the CLI reports only to hooks.
        var hooks: ObjectBuilder = .init(a);
        for (effort_hook_events, 0..) |event, n| {
            var entry: ObjectBuilder = .init(a);
            try entry.put("hookCallbackIds", try agent.json.stringArray(a, &.{try std.fmt.allocPrint(a, "hook_{d}", .{n})}));
            var list: std.json.Array = .init(a);
            try list.append(entry.value());
            try hooks.put(event, .{ .array = list });
        }
        try request.put("hooks", hooks.value());
    }
    if (claude.tools) |tools| {
        try request.put("sdkMcpServers", try agent.json.stringArray(a, &.{tools.name}));
        if (try toolServerSettings(claude, a)) |settings| {
            var configs: ObjectBuilder = .init(a);
            try configs.put(tools.name, settings);
            try request.put("sdkMcpServerConfigs", configs.value());
        }
    }
    if (cfg.output_schema) |schema| try request.put("jsonSchema", agent.json.valueOf(a, schema) orelse .null);
    if (session_options.system_prompt_blocks.len > 0) {
        try request.put("systemPrompt", try agent.json.stringArray(a, session_options.system_prompt_blocks));
    } else if (cfg.system_prompt.len > 0) {
        try request.put("systemPrompt", try agent.json.stringArray(a, &.{cfg.system_prompt}));
    }
    try request.strIf("appendSystemPrompt", cfg.append_system_prompt);
    try request.strIf("planModeInstructions", session_options.plan_mode_instructions);
    if (session_options.system_prompt_snapshot) |snapshot| try request.put("systemPromptSnapshot", .{ .bool = snapshot });
    try request.strIf("appendSubagentSystemPrompt", session_options.append_subagent_system_prompt);
    if (session_options.tool_aliases) |aliases| try request.put("toolAliases", agent.json.valueOf(a, aliases) orelse .null);
    if (session_options.exclude_dynamic_sections) try request.put("excludeDynamicSections", .{ .bool = true });
    if (session_options.agents) |agents| try request.put("agents", agent.json.valueOf(a, agents) orelse .null);
    try request.strIf("title", cfg.title);
    if (session_options.skills) |skills| if (!allSkills(skills)) try request.put("skills", try agent.json.stringArray(a, skills));
    if (session_options.prompt_suggestions) try request.put("promptSuggestions", .{ .bool = true });
    if (session_options.agent_progress_summaries) try request.put("agentProgressSummaries", .{ .bool = true });
    try request.put("forwardSubagentText", .{ .bool = !session_options.hide_subagent_text });
    if (!session_options.no_per_task_stop_affordance) try request.put("perTaskStopAffordance", .{ .bool = true });
    if (session_options.supported_dialog_kinds.len > 0) try request.put("supportedDialogKinds", try agent.json.stringArray(a, session_options.supported_dialog_kinds));
    return agent.json.mergeObject(a, try agent.json.encodeValue(a, request.value()), session_options.initialize_extra);
}
