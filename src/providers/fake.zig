//! An in-process scripted provider for tests and demos, and the smallest
//! complete provider implementation. Each turn streams its prompt back. A
//! prompt containing "permission" first asks for permission to run a tool;
//! one containing "tool:<name>" calls that host tool and replies with its
//! result.
const std = @import("std");
const Allocator = std.mem.Allocator;
const agent = @import("../root.zig");
const types = agent.types;
const errors = agent.errors;
const Session = agent.Session;
const StartOptions = agent.StartOptions;
const Env = agent.Env;
const Diagnostics = agent.Diagnostics;

pub const name = "fake";

pub const Options = struct {
    /// Delay between streamed words, in milliseconds.
    delay_ms: i64 = 0,
};

const capabilities: types.Capabilities = .{
    .streaming = true,
    .queue = true,
    .interrupt = true,
    .interrupt_turn = true,
    .set_model = true,
    .permission_prompt = true,
    .host_tools = true,
    .permission_modes = &.{ types.permission_mode.default, types.permission_mode.bypass },
};

const Provider = struct {
    env: Env,
    options: Options,
};

pub fn create(env: Env, options: Options) !agent.Provider {
    const p = try env.gpa.create(Provider);
    p.* = .{ .env = env, .options = options };
    return .{ .ptr = p, .vtable = &.{
        .name = name,
        .capabilities = capabilities,
        .start = start,
        .deinit = deinit,
        .check = check,
    } };
}

fn deinit(ptr: *anyopaque) void {
    const p: *Provider = @ptrCast(@alignCast(ptr));
    p.env.gpa.destroy(p);
}

fn check(_: *anyopaque, _: Allocator, _: ?*Diagnostics) errors.Error!types.ProviderStatus {
    return .{ .installed = true, .version = "1.0.0", .auth = .{ .authenticated = true, .method = "none" } };
}

const FakeSession = struct {
    session: Session,
    options: Options,
    mutex: std.Io.Mutex = .init,
    answer: ?types.PermissionDecision = null,
    answered: std.Io.Condition = .init,
    stop: std.atomic.Value(bool) = .init(false),
    /// Set by `close`: queued input no longer starts.
    closing: std.atomic.Value(bool) = .init(false),

    const vtable: Session.VTable = .{
        .send = send,
        .interrupt = interrupt,
        .interrupt_turn = interruptTurn,
        .respond = respond,
        .close = close,
        .destroy = destroy,
        .set_model = setModel,
    };

    fn of(s: *Session) *FakeSession {
        return @fieldParentPtr("session", s);
    }
};

fn start(ptr: *anyopaque, opts: StartOptions, _: ?*Diagnostics) errors.Error!*Session {
    const p: *Provider = @ptrCast(@alignCast(ptr));
    const fs = try p.env.gpa.create(FakeSession);
    fs.* = .{ .session = .init(p.env, name, &FakeSession.vtable, capabilities, opts), .options = p.options };
    const s = &fs.session;
    s.rt.setStarter(.{ .context = fs, .start = startQueued });
    const id = if (opts.config.resume_id.len > 0) opts.config.resume_id else p.env.uuid().slice();
    s.rt.markStarted(.{
        .id = id,
        .provider = name,
        .cwd = opts.config.cwd,
        .model = if (opts.config.model.len > 0) opts.config.model else "echo",
        .version = "1.0.0",
    }, opts.config.resume_id.len > 0);
    return s;
}

fn send(s: *Session, in: types.Input, turn_id: []const u8, diag: ?*Diagnostics) errors.Error!void {
    if (in.delivery == .interrupt) try interrupt(s, diag);
    if (!try s.rt.startInput(turn_id, in, diag)) try s.rt.enqueue(turn_id, in, .{}, diag);
}

/// Accepts the input and runs the turn as its own task, as a harness
/// process would.
fn startQueued(ctx: *anyopaque, id: []const u8, in: types.Input, diag: ?*Diagnostics) errors.Error!void {
    const fs: *FakeSession = @ptrCast(@alignCast(ctx));
    const rt = &fs.session.rt;
    if (fs.closing.load(.acquire)) return errors.fail(diag, .closed, "session closing", .{});
    const job = try rt.env.gpa.create(Job);
    job.* = .{ .fs = fs, .id = .of(id), .arena = .init(rt.env.gpa) };
    job.prompt = in.plainText(job.arena.allocator()) catch |e| {
        job.destroy();
        return e;
    };
    fs.stop.store(false, .release);
    rt.spawn(runTurn, .{job}) catch {
        job.destroy();
        return errors.fail(diag, .internal, "cannot start turn", .{});
    };
}

const Job = struct {
    fs: *FakeSession,
    id: agent.Id,
    arena: std.heap.ArenaAllocator,
    prompt: []const u8 = "",

    fn destroy(job: *Job) void {
        job.arena.deinit();
        job.fs.session.rt.env.gpa.destroy(job);
    }
};

fn runTurn(job: *Job) void {
    defer job.destroy();
    const fs = job.fs;
    const rt = &fs.session.rt;
    const a = job.arena.allocator();
    const turn = job.id.slice();
    rt.emit(.{ .turn_id = turn, .payload = .{ .turn_started = .{} } });
    rt.emit(.{ .turn_id = turn, .payload = .{ .status_changed = .{ .status = .running } } });

    var reply: []const u8 = job.prompt;
    if (std.mem.indexOf(u8, job.prompt, "permission") != null) {
        const decision = askPermission(fs);
        if (decision != .allow and decision != .allow_session and decision != .allow_always) reply = "permission denied";
    }
    if (std.mem.indexOf(u8, job.prompt, "tool:")) |at| {
        const tool = std.mem.sliceTo(job.prompt[at + 5 ..], ' ');
        const res = fs.session.callTool(a, .{ .id = "call-1", .name = tool, .session_id = "fake" });
        reply = res.joinedText(a) catch "";
        // Interrupting cancels the call before it stops the turn: an error
        // result may be that cancel, with the stop right behind it.
        if (res.is_error) awaitStop(fs, 1000);
    }

    const item_id = std.fmt.allocPrint(a, "msg-{s}", .{turn}) catch "msg";
    var status: types.TurnStatus = .completed;
    var it = std.mem.splitScalar(u8, reply, ' ');
    var first = true;
    while (it.next()) |word| {
        if (fs.stop.load(.acquire)) {
            status = .interrupted;
            break;
        }
        const delta = if (first) word else std.fmt.allocPrint(a, " {s}", .{word}) catch word;
        first = false;
        rt.emit(.{ .turn_id = turn, .payload = .{ .text_delta = .{ .item_id = item_id, .delta = delta } } });
        if (fs.options.delay_ms > 0) rt.env.io.sleep(.fromMilliseconds(fs.options.delay_ms), .awake) catch {};
    }
    rt.emit(.{ .turn_id = turn, .payload = .{ .item_completed = .{ .item = .{ .id = item_id, .kind = .assistant_message, .text = reply, .turn_id = turn } } } });
    rt.emit(.{ .turn_id = turn, .payload = .{ .status_changed = .{ .status = .idle } } });
    rt.finishTurn(turn, .{
        .status = status,
        .text = reply,
        .usage = .{ .input_tokens = @intCast(job.prompt.len), .output_tokens = @intCast(reply.len) },
    });
}

fn askPermission(fs: *FakeSession) types.PermissionDecision {
    const rt = &fs.session.rt;
    const io = rt.env.io;
    fs.mutex.lockUncancelable(io);
    fs.answer = null;
    fs.mutex.unlock(io);
    const req_id = rt.env.uuid();
    rt.request(.{ .permission_request = .{
        .id = req_id.slice(),
        .kind = .tool,
        .tool_name = "Bash",
        .title = "Run a command",
        .command = "echo hello",
        .decisions = &.{ .allow, .allow_session, .deny },
    } }, null, "");
    fs.mutex.lockUncancelable(io);
    defer fs.mutex.unlock(io);
    while (fs.answer == null and !fs.stop.load(.acquire)) fs.answered.waitUncancelable(io, &fs.mutex);
    return fs.answer orelse .cancel;
}

/// Waits up to `ms` for an interrupt.
fn awaitStop(fs: *FakeSession, ms: i64) void {
    const io = fs.session.rt.env.io;
    const deadline = agent.providerkit.timeoutMs(ms).toDeadline(io);
    fs.mutex.lockUncancelable(io);
    defer fs.mutex.unlock(io);
    while (!fs.stop.load(.acquire)) fs.answered.waitTimeout(io, &fs.mutex, deadline) catch return;
}

fn respond(s: *Session, _: *const Session.Pending, resp: types.Response, _: ?*Diagnostics) errors.Error!void {
    const fs = FakeSession.of(s);
    const io = s.rt.env.io;
    fs.mutex.lockUncancelable(io);
    defer fs.mutex.unlock(io);
    fs.answer = resp.permission.decision;
    fs.answered.broadcast(io);
}

fn interrupt(s: *Session, _: ?*Diagnostics) errors.Error!void {
    const fs = FakeSession.of(s);
    const io = s.rt.env.io;
    fs.mutex.lockUncancelable(io);
    defer fs.mutex.unlock(io);
    fs.stop.store(true, .release);
    fs.answered.broadcast(io);
}

fn interruptTurn(s: *Session, turn_id: []const u8, diag: ?*Diagnostics) errors.Error!void {
    const running, _ = s.rt.runningTurn();
    if (running.eql(turn_id)) return interrupt(s, diag);
}

fn setModel(s: *Session, model: []const u8, _: ?*Diagnostics) errors.Error!void {
    var arena: std.heap.ArenaAllocator = .init(s.rt.env.gpa);
    defer arena.deinit();
    var info = try s.rt.sessionInfo(arena.allocator());
    info.model = model;
    s.rt.setInfo(info, true);
}

fn close(s: *Session) void {
    FakeSession.of(s).closing.store(true, .release);
    interrupt(s, null) catch unreachable; // never fails
    // A running turn notices the stop and ends before the session does.
    s.rt.closeTasks();
}

fn destroy(s: *Session) void {
    const fs = FakeSession.of(s);
    s.rt.env.gpa.destroy(fs);
}

test "fake provider: run, stream, permission" {
    const gpa = std.testing.allocator;
    var threaded: std.Io.Threaded = .init(gpa, .{});
    defer threaded.deinit();
    const env: Env = .{ .io = threaded.io(), .gpa = gpa, .environ = .empty };

    const provider = try create(env, .{});
    defer provider.deinit();
    const s = try provider.start(.{}, null);
    defer s.deinit();

    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const res = try s.run(.{ .text = "hello world" }, arena.allocator(), .none, null);
    try std.testing.expectEqualStrings("hello world", res.text);
    try std.testing.expectEqual(@as(usize, 1), res.items.len);

    // A permission prompt arrives as one of the turn's events and is
    // answered by ID.
    var turn = try s.send(.{ .text = "needs permission" }, null);
    var diag: Diagnostics = .{};
    while (turn.nextEvent(.none)) |ev| {
        defer ev.destroy();
        if (ev.event.payload == .permission_request) {
            const id = ev.event.payload.permission_request.id;
            try s.respond(id, .{ .permission = .{ .decision = .allow } }, null);
            try std.testing.expectError(error.RequestDone, s.respond(id, .{ .permission = .{ .decision = .allow } }, &diag));
            try std.testing.expect(std.mem.indexOf(u8, diag.info.?.message, "not pending") != null);
        }
    } else |e| try std.testing.expectEqual(error.EndOfStream, e);
    const res2 = try turn.wait(arena.allocator(), .none, null);
    try std.testing.expectEqualStrings("needs permission", res2.text);

    // An identifier longer than any the library hands out is rejected.
    const long_id: [200]u8 = @splat('x');
    try std.testing.expectError(error.InvalidParams, s.wait(&long_id, arena.allocator(), .none, null));
}

test "fake provider: a request handler answers instead of an event" {
    const gpa = std.testing.allocator;
    var threaded: std.Io.Threaded = .init(gpa, .{});
    defer threaded.deinit();
    const env: Env = .{ .io = threaded.io(), .gpa = gpa, .environ = .empty };

    const provider = try create(env, .{});
    defer provider.deinit();
    const Deny = struct {
        fn permission(_: ?*anyopaque, _: *const types.PermissionRequest) types.PermissionResponse {
            return .{ .decision = .deny };
        }
    };
    const s = try provider.start(.{ .request_handler = .{ .permission = Deny.permission } }, null);
    defer s.deinit();

    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const res = try s.run(.{ .text = "needs permission" }, arena.allocator(), .none, null);
    try std.testing.expectEqualStrings("permission denied", res.text);
    while (s.nextEvent(.{ .duration = .{ .raw = .zero, .clock = .awake } })) |ev| {
        defer ev.destroy();
        try std.testing.expect(ev.event.payload != .permission_request);
    } else |_| {}
}
