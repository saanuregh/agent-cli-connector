//! Claude-specific settings: provider options, per-session options
//! (`Config.provider_options`), and per-turn options
//! (`Input.provider_options`). All parse from JSON with snake_case names.
const std = @import("std");
const agent = @import("agent");
const RawJson = agent.json.RawJson;

/// The Claude Agent SDK release whose wire protocol this package implements.
/// It is sent as CLAUDE_AGENT_SDK_VERSION, which the CLI only reports in its
/// User-Agent.
pub const protocol_version = "0.3.302";

/// Splits a block system prompt into a cacheable prefix and a
/// session-specific suffix.
pub const system_prompt_dynamic_boundary = "__SYSTEM_PROMPT_DYNAMIC_BOUNDARY__";

pub const Options = struct {
    /// The claude executable. Empty means "claude" on PATH.
    path: []const u8 = "",
    /// Added to every session's environment.
    env: ?agent.types.StringMap = null,
    /// Appended to every session's command line.
    extra_args: []const []const u8 = &.{},
    /// Bounds the initialize handshake. Zero means 60 s.
    startup_timeout_ms: i64 = 0,
    /// How long close waits for a clean exit before terminating the
    /// process. Zero means 5 s.
    shutdown_grace_ms: i64 = 0,
    /// Identifies the host application in Claude's User-Agent, such as
    /// "my-app/1.0.0".
    client_app: []const u8 = "",
};

pub const Thinking = struct {
    /// "adaptive", "enabled", or "disabled".
    type: []const u8 = "",
    /// The thinking budget for type "enabled".
    budget_tokens: i64 = 0,
    /// "summarized" or "omitted".
    display: []const u8 = "",
};

pub const Plugin = struct {
    path: []const u8 = "",
    /// Loads the plugin without its MCP servers.
    skip_mcp_discovery: bool = false,
};

pub const SessionOptions = struct {
    /// Runs the main thread as the named agent (--agent).
    agent: []const u8 = "",
    /// Subagent definitions by name, in the CLI's native shape
    /// (`{"name": {"description": ..., "prompt": ..., ...}}`).
    agents: ?RawJson = null,
    /// Selects the built-in tools. Null means the default set; an empty
    /// list disables every built-in tool.
    builtin_tools: ?[]const []const u8 = null,
    /// Redirects model-emitted tool names to other tools (an object).
    tool_aliases: ?RawJson = null,
    /// Tried when the main model is unavailable.
    fallback_model: []const u8 = "",
    thinking: ?Thinking = null,
    /// Stops a turn after this many model round trips.
    max_turns: i64 = 0,
    /// Stops the session after this estimated spend.
    max_budget_usd: f64 = 0,
    /// Tells the model its token budget (alpha).
    task_budget_tokens: i64 = 0,
    /// API beta features such as "context-1m-2025-08-07".
    betas: []const []const u8 = &.{},

    /// Replaces the system prompt with these blocks.
    system_prompt_blocks: []const []const u8 = &.{},
    /// Strips per-user sections from the default system prompt.
    exclude_dynamic_sections: bool = false,
    system_prompt_snapshot: ?bool = null,
    append_subagent_system_prompt: []const u8 = "",
    plan_mode_instructions: []const u8 = "",

    /// Which settings files load ("user", "project", "local"). Null loads
    /// all; an empty list loads none.
    setting_sources: ?[]const []const u8 = null,
    /// A settings object or a path to a settings file (a JSON string).
    settings: ?RawJson = null,
    /// Policy-tier settings: an object or a JSON string.
    managed_settings: ?RawJson = null,
    /// Command sandboxing (an object); merged into `settings`.
    sandbox: ?RawJson = null,
    plugins: []const Plugin = &.{},
    /// Enables skills by name; ["all"] enables every skill. Null leaves the
    /// CLI default.
    skills: ?[]const []const u8 = null,
    /// Ignores MCP servers from settings files and plugins.
    strict_mcp_config: bool = false,
    /// Registers `Config.mcp_servers` with the mcp_set_servers control
    /// request instead of --mcp-config. Always done when a server has
    /// headers, env, args, or a URL with credentials or a query string.
    mcp_via_control: bool = false,
    /// Reads project configuration from this checkout.
    project_config_root: []const u8 = "",

    /// Turns off token streaming.
    no_partial_messages: bool = false,
    /// Reports hook lifecycle as `hook_run` events.
    include_hook_events: bool = false,
    /// Stops forwarding subagents' text and thinking.
    hide_subagent_text: bool = false,
    /// Emits `claude/prompt_suggestion` after each turn.
    prompt_suggestions: bool = false,
    agent_progress_summaries: bool = false,
    /// Without the per-task stop declaration an interrupt also kills
    /// background subagents and shells.
    no_per_task_stop_affordance: bool = false,
    /// Registers observation-only hooks that report each subagent's
    /// effort in `Task.subagent.effort`.
    track_subagent_effort: bool = false,
    /// Delivers prompt text as written: no @path expansion and no
    /// slash-command dispatch.
    verbatim_prompts: bool = false,
    /// Streams the output of foreground Bash calls as `tool_output_delta`.
    tail_shell_output: bool = false,

    /// Records file backups so `rewind_files` works.
    enable_file_checkpointing: bool = false,
    /// Resumes the most recent session in the working directory.
    @"continue": bool = false,
    /// Resumes only up to this message UUID.
    resume_session_at: []const u8 = "",
    resume_drops_turn: []const u8 = "",
    /// Routes permission prompts to this MCP tool instead of the host.
    permission_prompt_tool: []const u8 = "",
    /// Denies anything that would ask, without asking.
    no_permission_prompts: bool = false,
    /// Launches the CLI so that the bypass mode can be set later.
    allow_bypass_permissions: bool = false,
    /// Configures built-in tools, such as
    /// {"askUserQuestion": {"previewFormat": "html"}}.
    tool_config: ?RawJson = null,

    /// Names the in-process MCP server serving `Config.tools`. Empty means
    /// "host".
    tool_server_name: []const u8 = "",
    /// Bounds one host tool call; zero means the CLI default.
    tool_timeout_ms: i64 = 0,
    debug: bool = false,
    debug_file: []const u8 = "",
    extra_args: []const []const u8 = &.{},
    /// Fields added to the initialize request (an object).
    initialize_extra: ?RawJson = null,
};

pub const TurnOptions = struct {
    /// "now", "next", or "later" for messages sent while a turn runs.
    priority: []const u8 = "",
    /// Appends the message without starting a turn.
    no_query: bool = false,
    /// Delivers this message as written.
    verbatim: bool = false,
    pasted_content: []const []const u8 = &.{},
    /// The message provenance, such as {"kind": "human"}.
    origin: ?RawJson = null,
    /// Fields added to the user message (an object).
    extra: ?RawJson = null,
};
