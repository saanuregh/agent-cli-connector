//! Errors with details. Fallible library calls return `Error` and take an
//! optional `*Diagnostics` that receives the failure's details.
const std = @import("std");
const types = @import("types.zig");
const ErrorInfo = types.ErrorInfo;
const ErrorKind = types.ErrorKind;

pub const Error = error{
    Unsupported,
    Closed,
    Cancelled,
    RequestDone,
    SessionNotFound,
    SteerRejected,
    Interrupted,
    InvalidAnswer,
    InvalidParams,
    NotFound,
    Protocol,
    Exit,
    TurnFailed,
    Timeout,
    Internal,
    OutOfMemory,
};

pub fn toError(kind: ErrorKind) Error {
    return switch (kind) {
        .unsupported => error.Unsupported,
        .closed => error.Closed,
        .cancelled => error.Cancelled,
        .request_done => error.RequestDone,
        .session_not_found => error.SessionNotFound,
        .steer_rejected => error.SteerRejected,
        .interrupted => error.Interrupted,
        .invalid_answer => error.InvalidAnswer,
        .invalid_params => error.InvalidParams,
        .not_found => error.NotFound,
        .protocol => error.Protocol,
        .exit => error.Exit,
        .turn_failed => error.TurnFailed,
        .timeout => error.Timeout,
        .internal => error.Internal,
    };
}

pub fn toKind(err: anyerror) ErrorKind {
    return switch (err) {
        error.Unsupported => .unsupported,
        error.Closed => .closed,
        error.Cancelled => .cancelled,
        error.RequestDone => .request_done,
        error.SessionNotFound => .session_not_found,
        error.SteerRejected => .steer_rejected,
        error.Interrupted => .interrupted,
        error.InvalidAnswer => .invalid_answer,
        error.InvalidParams => .invalid_params,
        error.NotFound, error.FileNotFound => .not_found,
        error.Protocol => .protocol,
        error.Exit => .exit,
        error.TurnFailed => .turn_failed,
        error.Timeout => .timeout,
        else => .internal,
    };
}

/// Details of a failure, filled by the call it is passed to. Pass one to
/// any fallible call to learn more than the error name; pass null to skip
/// them. Holds its own copy, so the details stay valid until the next
/// failure is recorded in it.
pub const Diagnostics = struct {
    info: ?ErrorInfo = null,
    /// Two buffers used in turn, so a failure can be recorded from details
    /// that point into the previous one.
    buffers: [2][8 * 1024]u8 = undefined,
    current: u1 = 0,

    /// Records a copy of `info`.
    pub fn record(d: *Diagnostics, info: ErrorInfo) void {
        d.current +%= 1;
        var fba: std.heap.FixedBufferAllocator = .init(&d.buffers[d.current]);
        d.info = @import("clone.zig").clone(ErrorInfo, fba.allocator(), info) catch copyShort(&fba, info);
    }

    /// The recorded details when they match `err`, else a bare description.
    pub fn of(d: ?*const Diagnostics, err: anyerror) ErrorInfo {
        const kind = toKind(err);
        if (d) |diag| if (diag.info) |info| if (info.kind == kind) return info;
        return .{ .kind = kind, .message = @errorName(err) };
    }
};

/// Records `info` in `diag` and returns the matching error.
pub fn failWith(diag: ?*Diagnostics, info: ErrorInfo) Error {
    if (diag) |d| d.record(info);
    return toError(info.kind);
}

/// The details that fit when the whole copy does not: kind, message, code.
fn copyShort(fba: *std.heap.FixedBufferAllocator, info: ErrorInfo) ErrorInfo {
    fba.reset();
    const a = fba.allocator();
    const message = info.message[0..@min(info.message.len, 4096)];
    return .{
        .kind = info.kind,
        .message = a.dupe(u8, message) catch "",
        .code = a.dupe(u8, info.code[0..@min(info.code.len, 256)]) catch "",
        .rpc_code = info.rpc_code,
        .http_status = info.http_status,
        .retryable = info.retryable,
        .resets_at_ms = info.resets_at_ms,
        .stderr = a.dupe(u8, info.stderr[info.stderr.len -| 2048..]) catch "",
    };
}

/// Records a failure of `kind` with a formatted message.
pub fn fail(diag: ?*Diagnostics, kind: ErrorKind, comptime fmt: []const u8, args: anytype) Error {
    if (diag == null) return toError(kind);
    var buf: [2048]u8 = undefined;
    const msg = std.fmt.bufPrint(&buf, fmt, args) catch fmt;
    return failWith(diag, .{ .kind = kind, .message = msg });
}

/// Records an unsupported-feature failure.
pub fn unsupported(diag: ?*Diagnostics, provider: []const u8, feature: []const u8) Error {
    return fail(diag, .unsupported, "{s}: {s} is not supported", .{ provider, feature });
}

test "diagnostics keep details of the latest failure" {
    var diag: Diagnostics = .{};
    try std.testing.expectEqual(error.NotFound, fail(&diag, .not_found, "no {s}", .{"thing"}));
    try std.testing.expectEqualStrings("no thing", diag.info.?.message);
    // Recording from the previous details is safe.
    try std.testing.expectEqual(error.Closed, failWith(&diag, .{ .kind = .closed, .message = diag.info.?.message }));
    try std.testing.expectEqualStrings("no thing", Diagnostics.of(&diag, error.Closed).message);
    try std.testing.expectEqualStrings("Timeout", Diagnostics.of(&diag, error.Timeout).message);
    try std.testing.expectEqual(error.Closed, fail(null, .closed, "ignored", .{}));
}
