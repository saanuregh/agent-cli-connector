//! Pi-specific settings.
const agent = @import("agent");
const types = agent.types;

/// Provider-level settings.
pub const Options = struct {
    /// The pi executable; empty means "pi" on PATH.
    path: []const u8 = "",
    /// Added to every session's environment.
    env: ?types.StringMap = null,
    /// Appended to every session's command line.
    extra_args: []const []const u8 = &.{},
    /// Bounds startup; zero means 60 s.
    startup_timeout_ms: i64 = 0,
    /// How long close waits for a clean exit; zero means 5 s.
    shutdown_grace_ms: i64 = 0,
    /// Where the pi process finds the bridge extension. Empty means the
    /// library writes it to the user cache directory.
    bridge_path: []const u8 = "",
    /// Runs Pi without the bridge extension. Host tools, permission modes,
    /// MCP servers, output schemas, and in-place rewind are then
    /// unavailable.
    disable_bridge: bool = false,
    /// Where sessions are stored and looked up. Empty means Pi's own
    /// resolution: PI_CODING_AGENT_SESSION_DIR, then sessionDir in the
    /// project's .pi/settings.json, then in the agent directory's
    /// settings.json, then the agent directory's sessions folder.
    session_dir: []const u8 = "",
};

/// Per-session settings, passed as `Config.provider_options`.
pub const SessionOptions = struct {
    /// The model provider for `Config.model` (--provider).
    provider: []const u8 = "",
    /// Limits model cycling to these patterns (--models).
    models: []const []const u8 = &.{},
    extensions: []const []const u8 = &.{},
    no_extensions: bool = false,
    skills: []const []const u8 = &.{},
    no_skills: bool = false,
    prompt_templates: []const []const u8 = &.{},
    no_prompt_templates: bool = false,
    /// Skips AGENTS.md and CLAUDE.md files.
    no_context_files: bool = false,
    /// Allowlist of enabled tools (--tools). It controls which tools exist;
    /// `Config.allowed_tools` controls which run without asking.
    tools: []const []const u8 = &.{},
    no_tools: bool = false,
    no_builtin_tools: bool = false,
    /// Disables Pi's built-in MCP support.
    no_mcp: bool = false,
    /// Empty means `Options.session_dir`.
    session_dir: []const u8 = "",
    /// true passes --approve, false --no-approve, null leaves Pi's setting.
    trust_project: ?bool = null,
    offline: bool = false,
    /// "all" or "one-at-a-time".
    steering_mode: []const u8 = "",
    follow_up_mode: []const u8 = "",
    auto_compaction: ?bool = null,
    auto_retry: ?bool = null,
    /// Runs this session without the bridge extension.
    disable_bridge: bool = false,
    extra_args: []const []const u8 = &.{},
};

/// Pi has no turn options; `Input.provider_options` is refused.
pub const TurnOptions = struct {};
