//! The Claude Code provider. It drives the `claude` CLI through its
//! stream-json control protocol, the protocol the official TypeScript Agent
//! SDK speaks.
//!
//! Mapping of `Config`:
//!   - model, effort, permission_mode, system_prompt, append_system_prompt,
//!     resume_id, fork, session_id, ephemeral, title, mcp_servers,
//!     allowed_tools, disallowed_tools, and additional_directories map to
//!     CLI flags or initialize fields. Uniform permission modes map to the
//!     native ones (accept_edits is "acceptEdits", bypass is
//!     "bypassPermissions", dont_ask is "dontAsk"); read_only is refused.
//!   - output_schema becomes --json-schema for every turn; a per-turn
//!     `Input.output_schema` is refused.
//!   - fork with resume_id becomes --fork-session, and fork_at_turn
//!     --resume-session-at, resolved from the session transcript.
//!     `Session.fork` instead copies the transcript into a new session
//!     first (as the TypeScript SDK's forkSession does), cut at
//!     fork_at_turn, and resumes that copy, so the fork persists before
//!     its first turn; an ephemeral fork keeps the flags.
//!   - command_prefix goes before the CLI executable.
//!   - tools are served by an in-process MCP server (named "host" unless
//!     `SessionOptions.tool_server_name` says otherwise); the model sees
//!     them as "mcp__host__<name>". A tool with needs_approval gets the
//!     "always_ask" policy; other tools are allowed tools.
//!   - mcp_servers[*].enabled_tools is refused (use disallowed_tools).
//!   - unset_env is removed together with the variables that mark a parent
//!     Claude Code session and NODE_OPTIONS, DEBUG, and
//!     CLAUDE_CODE_AGENT_PROGRESS_SUMMARIES.
//!
//! Input sent during a turn goes to the CLI's own queue with a priority:
//! queue is "later", steer "next", and interrupt "now"
//! (`TurnOptions.priority` overrides it). `Session.call` sends any control
//! request subtype; "rewind" with {"turn_id"} returns the rewind result.
const std = @import("std");
const agent = @import("agent");
const options = @import("options.zig");
const provider = @import("provider.zig");

pub const name = "claude";
pub const Options = options.Options;
pub const SessionOptions = options.SessionOptions;
pub const TurnOptions = options.TurnOptions;
pub const Thinking = options.Thinking;
pub const Plugin = options.Plugin;
pub const protocol_version = options.protocol_version;

/// Creates the provider. `options` is copied.
pub fn create(env: agent.Env, opts: Options) !agent.Provider {
    return provider.create(env, opts);
}

test {
    _ = @import("decode.zig");
    _ = @import("session.zig");
    _ = @import("translate.zig");
    _ = @import("host_tools.zig");
    _ = @import("permissions.zig");
    _ = @import("history.zig");
    _ = @import("subagents.zig");
    _ = @import("translate_test.zig");
    _ = @import("live_test.zig");
}
