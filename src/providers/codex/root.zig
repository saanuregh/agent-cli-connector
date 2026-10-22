//! The Codex provider: drives the OpenAI Codex CLI through its app-server
//! JSON-RPC protocol ("codex app-server").
//!
//! By default each session runs its own app-server process; with
//! `Options.shared_server` every session shares one process until the
//! provider is freed. `Session.call` sends any app-server method (Codex
//! `Client.Call`) and returns its result.
//!
//! Mapping of `Config`:
//!
//!   - model, cwd, system_prompt (baseInstructions), append_system_prompt
//!     (developerInstructions), resume_id, fork, fork_at_turn (lastTurnId),
//!     ephemeral, title, mcp_servers, and additional_directories (runtime
//!     workspace roots) map to thread parameters.
//!   - effort is sent with each turn and stays in effect.
//!   - permission_mode maps to an approval policy and sandbox: default and
//!     accept_edits → on-request + workspace-write, read_only → on-request +
//!     read-only, dont_ask → never + workspace-write, bypass → never +
//!     danger-full-access, auto → on-request with the auto reviewer, plan →
//!     plan collaboration mode + on-request + read-only. Any other mode is
//!     sent as the native approval policy, such as "untrusted".
//!   - tools are served as dynamic tools.
//!   - allowed_tools has no Codex equivalent and is ignored (a
//!     "config_ignored" notice; `strict` refuses it). disallowed_tools is
//!     refused, because a restriction is never ignored. session_id is not
//!     supported (Codex assigns thread IDs).
//!   - McpServer.enabled_tools maps to enabled_tools and approved_tools to
//!     tools.<name>.approval_mode = "approve"; always_load is ignored.
//!   - env, unset_env, and command_prefix apply to a dedicated app-server
//!     only. On a shared server env, command_prefix, and wire_tap are
//!     ignored, and unset_env is refused; `Options.command_prefix` wraps
//!     the shared server.
//!   - output_schema applies to every turn without `Input.output_schema`;
//!     both are normalized to OpenAI strict form unless
//!     `SessionOptions.disable_strict_output_schema` is set.
//!
//! Behavior worth knowing:
//!
//!   - Steer uses turn/steer; the steered input's turn ends with the turn it
//!     joined. When the turn has just ended the input runs as a new turn;
//!     when Codex refuses the steer otherwise, `send` fails with
//!     `steer_rejected`.
//!   - Interrupt stops the running turn and its subagents' turns; queued
//!     input still runs.
//!   - A permission answer cannot change a call's arguments, so an allowing
//!     answer with `updated_input` is refused.
//!   - A file part is sent as text holding its absolute path.
//!   - A thread's server requests without a uniform meaning become
//!     `native_request` events, answered with a JSON-RPC result or error.
//!     Requests for no thread, such as account/chatgptAuthTokens/refresh,
//!     are answered with JSON-RPC error -32601, except currentTime/read.
//!   - Closing a session on a shared server only unsubscribes its thread:
//!     its turn is not interrupted, and its pending requests are withdrawn
//!     without an answer to Codex.
//!   - Rewind reverts the thread in place; a thread without paginated
//!     history cannot be rewound.
//!   - Turns ask for "detailed" reasoning summaries unless
//!     `reasoning_summary` is set.
//!   - `Provider.call` sends an app-server method without a thread, on the
//!     shared server or a short-lived one.
const std = @import("std");
const agent = @import("agent");
const options = @import("options.zig");
const provider = @import("provider.zig");

pub const name = "codex";
pub const Options = options.Options;
pub const SessionOptions = options.SessionOptions;
pub const TurnOptions = options.TurnOptions;
pub const capabilities = provider.capabilities;
pub const strictOutputSchema = @import("output_schema.zig").strictOutputSchema;

/// Creates the provider. `options` is copied.
pub fn create(env: agent.Env, opts: Options) !agent.Provider {
    return provider.create(env, opts);
}

test "create copies options and reports capabilities" {
    const gpa = std.testing.allocator;
    var threaded: std.Io.Threaded = .init(gpa, .{});
    defer threaded.deinit();
    const env: agent.Env = .{ .io = threaded.io(), .gpa = gpa, .environ = .empty };
    var arena: std.heap.ArenaAllocator = .init(gpa);
    const codex = try create(env, .{ .client_name = try arena.allocator().dupe(u8, "host"), .disable_experimental_api = true });
    arena.deinit();
    defer codex.deinit();
    try std.testing.expectEqualStrings("codex", codex.name());
    try std.testing.expect(!codex.capabilities().host_tools);
    try std.testing.expect(codex.capabilities().rewind);
}

test {
    _ = @import("output_schema.zig");
    _ = @import("decode.zig");
    _ = @import("session.zig");
    _ = @import("translate.zig");
    _ = @import("history.zig");
    _ = @import("live_test.zig");
    _ = @import("app_server_test.zig");
}
