//! The Pi provider: drives the Pi coding agent through its RPC mode
//! ("pi --mode rpc").
//!
//! Pi has no permission system, host tools, or MCP flags of its own. The
//! provider loads a small bridge extension (bridge.ts, embedded) that adds
//! them through Pi's extension API: host tools are registered as Pi tools,
//! tool calls are gated through the uniform permission request flow, and
//! MCP servers are registered with pi.registerMcpServer. The bridge talks
//! to the host over the RPC extension UI channel.
//!
//! Mapping of `Config`:
//!
//!   - model ("provider/model" or a pattern), effort (thinking level),
//!     system_prompt, append_system_prompt, resume_id, fork, session_id,
//!     ephemeral, title, and disallowed_tools map to CLI flags.
//!   - permission_mode is enforced by the bridge: an empty mode keeps Pi's
//!     default of running every tool, which the info reports as bypass;
//!     default asks before tools that are not read-only, except
//!     allowed_tools, MCP approved_tools, host tools without
//!     needs_approval, and allow_session grants; accept_edits also runs
//!     edit and write without asking; read_only and plan refuse every tool
//!     that is not read-only, whatever allowed_tools and grants say;
//!     dont_ask runs read-only tools, allowed_tools, and MCP approved_tools,
//!     and refuses anything else; bypass runs everything. Without the
//!     bridge, or when it did not load, only an empty mode and bypass are
//!     accepted. When a session switch leaves the bridge unloaded under a
//!     gating mode, a notice with code "bridge_unloaded" is sent and send
//!     refuses turns until the bridge is back or the mode is bypass.
//!   - An allow_session answer approves the same bash command, the same
//!     path for edit and write, and the same tool otherwise. Grants end
//!     with new_session and switch_session.
//!   - additional_directories is ignored, because Pi does not sandbox file
//!     access; a "config_ignored" notice says so, and strict refuses it.
//!   - On resume, model is not passed on the command line. It is applied
//!     after start with set_model, and only when get_available_models lists
//!     it; otherwise the session keeps its recorded model.
//!   - mcp_servers are registered by the bridge. Names may contain only
//!     letters, digits, "_", and "-", and names that differ only in "-" and
//!     "_" collide. always_load maps to Pi's "direct" exposure;
//!     enabled_tools hides the server's other tools through Pi's
//!     toolExposure; approved_tools run without asking.
//!
//! Behavior worth knowing:
//!
//!   - Images are refused with `unsupported` when the current model does
//!     not list "image" input.
//!   - With the bridge loaded, rewind moves the session back in place, so
//!     the session ID stays; live user message items carry their Pi entry
//!     ID as the item ID, which rewind and fork_at_turn accept. Without the
//!     bridge, rewind forks at the turn's prompt and the session ID changes.
//!   - Interrupt clears Pi's queued steering and follow-up messages before
//!     it aborts; input queued by this library still runs afterwards.
//!   - A steer that Pi queues into the running run gets its own turn ID,
//!     which ends with the running turn's result.
//!   - Session switches (the new_session, switch_session, clone, and fork
//!     commands through `call`) and compact hold the session like a turn:
//!     they fail while a turn runs or input is queued, and input sent while
//!     they run waits for them.
//!   - send runs "/compact [instructions]" and "/autocompact
//!     [on|off|toggle]" through RPC, since Pi runs its built-in commands
//!     only in its terminal UI.
//!   - Input.output_schema needs the bridge. The bridge gives that turn a
//!     structured_output tool whose parameters are the schema (wrapped in a
//!     "value" property unless it is an object schema); a turn that ends
//!     without calling it fails with code "no_structured_output".
//!     Config.output_schema applies the same way to every turn without one.
//!   - Pi-specific events arrive as `native` events: "pi/queue_updated",
//!     "pi/ui_update", "pi/extension_error", "pi/entry_appended", and Pi's
//!     own type for unmapped messages.
const std = @import("std");
const agent = @import("agent");
const options = @import("options.zig");
const provider = @import("provider.zig");

pub const name = "pi";
pub const Options = options.Options;
pub const SessionOptions = options.SessionOptions;
pub const TurnOptions = options.TurnOptions;
pub const create = provider.create;

/// The bridge extension's TypeScript source, for hosts that place it
/// themselves (see `Options.bridge_path`).
pub const bridge_source = @import("bridge.zig").source;

test {
    _ = @import("decode.zig");
    _ = @import("tool_output.zig");
    _ = @import("history.zig");
    _ = @import("translate.zig");
    _ = @import("session.zig");
    _ = @import("control.zig");
    _ = provider;
}

test "capabilities follow the bridge setting" {
    const gpa = std.testing.allocator;
    var threaded: std.Io.Threaded = .init(gpa, .{});
    defer threaded.deinit();
    const env: agent.Env = .{ .io = threaded.io(), .gpa = gpa, .environ = .empty };
    const with = try create(env, .{});
    defer with.deinit();
    try std.testing.expect(with.capabilities().host_tools);
    const without = try create(env, .{ .disable_bridge = true, .extra_args = &.{"--offline"} });
    defer without.deinit();
    try std.testing.expect(!without.capabilities().host_tools);
    try std.testing.expectEqual(@as(usize, 0), without.capabilities().permission_modes.len);
}
