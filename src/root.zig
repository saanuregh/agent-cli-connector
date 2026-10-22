//! A uniform API over coding-agent CLIs: Claude Code, Codex, and Pi.
//!
//! A `Provider` starts `Session`s for one harness. A session is one live
//! conversation: input goes in with `send` or `run`, typed events come out of
//! `nextEvent`, and requests (permissions, questions, MCP elicitations) are
//! answered with `respond` or a `RequestHandler`. `Session.call` reaches
//! every native feature. Fallible calls take an optional `*Diagnostics`
//! that receives the failure's details.
const std = @import("std");

pub const types = @import("types.zig");
pub const json = @import("json.zig");
pub const errors = @import("errors.zig");
pub const runtime = @import("runtime.zig");
pub const process = @import("process.zig");
pub const providerkit = @import("providerkit.zig");
/// Deep-copies a library value into an allocator.
pub const clone = @import("clone.zig").clone;

const session = @import("session.zig");
pub const Session = session.Session;
pub const Provider = session.Provider;
pub const StartOptions = session.StartOptions;
pub const Env = runtime.Env;
pub const Id = runtime.Id;
pub const OwnedEvent = runtime.OwnedEvent;
pub const EventSink = runtime.EventSink;

pub const Error = errors.Error;
pub const Diagnostics = errors.Diagnostics;
pub const RequestHandler = session.RequestHandler;
pub const Turn = session.Turn;
pub const NextEventError = session.NextEventError;

pub const fake = @import("providers/fake.zig");
pub const claude = @import("claude");
pub const codex = @import("codex");
pub const pi = @import("pi");

pub const rpc = @import("rpc.zig");
pub const registry = @import("registry.zig");

test {
    _ = errors;
    _ = json;
    _ = runtime;
    _ = process;
    _ = providerkit;
    _ = fake;
    _ = rpc;
}
