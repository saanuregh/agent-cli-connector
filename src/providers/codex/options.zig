//! Codex-specific settings: provider `Options`, per-session
//! `SessionOptions` (in `Config.provider_options`), and per-turn
//! `TurnOptions` (in `Input.provider_options`). All parse from JSON with
//! snake_case field names.
const agent = @import("agent");
const RawJson = agent.json.RawJson;
const StringMap = agent.types.StringMap;

/// Configures the provider and its app-server processes.
pub const Options = struct {
    /// The codex executable; empty means "codex" on PATH.
    path: []const u8 = "",
    /// Added to the app-server environment.
    env: ?StringMap = null,
    /// Inherited variables removed from the app-server environment.
    unset_env: []const []const u8 = &.{},
    /// "-c key=value" overrides, values in TOML syntax.
    config: ?StringMap = null,
    /// Codex features to turn on (--enable) and off (--disable).
    enable: []const []const u8 = &.{},
    disable: []const []const u8 = &.{},
    /// Appended to the app-server command line.
    extra_args: []const []const u8 = &.{},
    /// Words placed before the executable of every app-server the provider
    /// starts, such as a sandbox wrapper. A dedicated app-server uses
    /// `Config.command_prefix` instead when that is set.
    command_prefix: []const []const u8 = &.{},
    /// Bounds the initialize handshake; zero means 60 s.
    startup_timeout_ms: i64 = 0,
    /// How long closing waits for a clean exit; zero means 5 s.
    shutdown_grace_ms: i64 = 0,
    /// Identify the host to Codex. A `client_name` also becomes
    /// CODEX_INTERNAL_ORIGINATOR_OVERRIDE unless `env` sets it; the reserved
    /// name "codex_app_server_daemon" makes the host non-originating.
    client_name: []const u8 = "",
    client_title: []const u8 = "",
    client_version: []const u8 = "",
    /// Runs every session on one app-server process until the provider is
    /// freed. Per-session `env` and `command_prefix` are then ignored and
    /// `unset_env` refused.
    shared_server: bool = false,
    /// Turns off experimental app-server methods and fields. Host tools
    /// need them, so `Config.tools` is then ignored.
    disable_experimental_api: bool = false,
    /// Notification methods the server should not send.
    opt_out_notifications: []const []const u8 = &.{},
    /// Merged into the initialize capabilities, such as
    /// {"extensions": {...}}. The "openai/form" extension is declared by
    /// default; an "extensions" object here is merged with it, and
    /// "openai/form": null removes it.
    initialize_capabilities: ?RawJson = null,
};

/// Codex thread settings.
pub const SessionOptions = struct {
    /// Overrides the policy derived from the permission mode: "untrusted",
    /// "on-request", "never", or a granular policy object.
    approval_policy: ?RawJson = null,
    /// Overrides the sandbox derived from the permission mode: "read-only",
    /// "workspace-write", or "danger-full-access".
    sandbox: []const u8 = "",
    /// Routes approvals to "user", "auto_review", or "guardian_subagent".
    approvals_reviewer: []const u8 = "",
    /// A named permissions profile used instead of `sandbox`.
    permissions: []const u8 = "",
    /// A model provider from the Codex config. Listing sessions filters by
    /// `Options.config["model_provider"]` instead.
    model_provider: []const u8 = "",
    /// A service tier such as "flex".
    service_tier: []const u8 = "",
    /// "auto", "concise", "detailed", or "none".
    reasoning_summary: []const u8 = "",
    personality: []const u8 = "",
    /// Config overrides for this thread, keyed by dotted path.
    config: ?RawJson = null,
    /// The persisted history contract.
    history_mode: []const u8 = "",
    /// Labels the thread for analytics.
    service_name: []const u8 = "",
    /// Added to thread/start, thread/resume, and thread/fork.
    extra_thread_params: ?RawJson = null,
    /// Added to every turn/start.
    extra_turn_params: ?RawJson = null,
    /// Sends output schemas unchanged instead of in OpenAI strict form.
    disable_strict_output_schema: bool = false,
    /// Stops re-sending the last `TurnOptions.additional_context` after
    /// Codex compacts the context (Codex drops it on compaction).
    disable_context_restore: bool = false,
};

/// Codex per-turn settings.
pub const TurnOptions = struct {
    /// A native collaboration mode, such as
    /// {"mode":"plan","settings":{"model":"...","reasoning_effort":null,"developer_instructions":null}}.
    collaboration_mode: ?RawJson = null,
    reasoning_summary: []const u8 = "",
    /// The service tier for this turn only.
    service_tier: []const u8 = "",
    /// The service tier for this and later turns.
    sticky_service_tier: []const u8 = "",
    /// Override the thread settings for this and later turns.
    /// `approval_policy` is a policy name or a granular policy object;
    /// `sandbox_policy` is "read-only", "workspace-write",
    /// "danger-full-access", or a native SandboxPolicy object.
    approval_policy: ?RawJson = null,
    sandbox_policy: ?RawJson = null,
    approvals_reviewer: []const u8 = "",
    cwd: []const u8 = "",
    /// Keyed context fragments, each {"kind": "application"|"untrusted",
    /// "value": "..."}.
    additional_context: ?RawJson = null,
    /// Added to turn/start.
    extra: ?RawJson = null,
};
