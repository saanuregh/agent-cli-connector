//! A live Pi RPC session ("pi --mode rpc"): process and command plumbing,
//! startup, input delivery (queue, steer, interrupt), the bridge channel
//! (host tools, the permission gate, subagent events), and requests.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const agent = @import("agent");
const types = agent.types;
const errors = agent.errors;
const kit = agent.providerkit;
const json = agent.json;
const JsonView = json.JsonView;
const RawJson = json.RawJson;
const Session = agent.Session;
const Id = agent.Id;
const Process = agent.process.Process;
const Error = errors.Error;
const Diagnostics = errors.Diagnostics;
const eql = kit.eql;
const log = std.log.scoped(.agent);

const decode = @import("decode.zig");
const bridge = @import("bridge.zig");
const control = @import("control.zig");
const Translator = @import("translate.zig").Translator;
const options = @import("options.zig");
const Provider = @import("provider.zig").Provider;

const name = @import("root.zig").name;

/// The bridge command that rewinds the session in place.
pub const rewind_command = "agent-cli-rewind";

/// Bounds the turns whose IDs rewind and fork_at_turn map to session
/// entries.
const max_remembered_turns = 1000;

pub const PiSession = struct {
    session: Session,
    provider: *Provider,
    gpa: Allocator,
    io: Io,
    /// Holds `cfg`, `opts`, and other startup values for the session's
    /// lifetime.
    cfg_arena: std.heap.ArenaAllocator,
    cfg: types.Config = .{},
    opts: options.SessionOptions = .{},
    /// The bridge's per-session token; empty without the bridge.
    token: []const u8 = "",
    /// Pi's names of `McpServer.approved_tools`.
    approved_mcp: []const []const u8 = &.{},
    proc: ?*Process = null,
    translator: Translator,
    /// Holds what the reader task handles for one output line; only that
    /// task uses it.
    line_arena: std.heap.ArenaAllocator,

    next_id: std.atomic.Value(u64) = .init(0),
    /// Set between Pi's compaction_start and compaction_end.
    compacting: std.atomic.Value(bool) = .init(false),
    /// The session's commands list the bridge's rewind command. The bridge
    /// registers it after its tool gate, so it also shows the gate is in
    /// place.
    bridge_loaded: std.atomic.Value(bool) = .init(false),
    closing: std.atomic.Value(bool) = .init(false),
    /// Set when the session closes; wakes timers.
    stopped: Io.Event = .unset,

    wait_mutex: Io.Mutex = .init,
    waiters: std.ArrayList(*Waiter) = .empty,
    exited: bool = false,

    /// Runs one steer at a time; the runtime tracks a single steer.
    steer_mutex: Io.Mutex = .init,

    /// Guards the fields below.
    mutex: Io.Mutex = .init,
    mode: []const u8 = "",
    model_provider: Id = .{},
    window: i64 = 0,
    /// Whether the current model takes images; null when unknown.
    model_images: ?bool = null,
    /// allow_session grants by grant key.
    allowed: kit.StringSet = .{},
    /// Tool calls the gate refused, by tool call ID.
    denied: kit.StringSet = .{},
    /// Each turn's first user message timestamp, which locates the turn's
    /// session entry, and the entry once the bridge reported it.
    turn_entries: std.ArrayList(TurnEntry) = .empty,
    /// Prompts sent with a client ID whose user message Pi has not echoed.
    client_ids: std.ArrayList(PendingClient) = .empty,
    /// Rewinds waiting for the bridge's answer.
    rewinds: std.ArrayList(*RewindSlot) = .empty,
    /// Each subagent task's session file.
    subagent_files: kit.OwnedMap([]const u8) = .{},
    /// Subagent tasks reported on the extension event bus.
    tasks: std.ArrayList(*TaskEntry) = .empty,
    /// Turns that asked for structured output.
    outputs: std.ArrayList(OutputRequest) = .empty,

    pub const vtable: Session.VTable = .{
        .send = send,
        .interrupt = interrupt,
        .interrupt_turn = interruptOneTurn,
        .respond = respond,
        .close = close,
        .destroy = destroy,
        .set_model = control.setModel,
        .set_effort = control.setEffort,
        .set_permission_mode = control.setPermissionMode,
        .set_title = control.setTitle,
        .compact = control.compact,
        .rewind = control.rewind,
        .fork = control.fork,
        .call = control.call,
    };

    pub fn of(session: *Session) *PiSession {
        return @fieldParentPtr("session", session);
    }

    pub fn rt(pi: *PiSession) *agent.runtime.Runtime {
        return &pi.session.rt;
    }

    pub fn env(pi: *PiSession) agent.Env {
        return pi.session.rt.env;
    }

    pub fn bridgeDisabled(pi: *PiSession) bool {
        return pi.provider.options.disable_bridge or pi.opts.disable_bridge;
    }

    pub fn lock(pi: *PiSession) void {
        pi.mutex.lockUncancelable(pi.io);
    }

    pub fn unlock(pi: *PiSession) void {
        pi.mutex.unlock(pi.io);
    }

    pub fn contextWindow(pi: *PiSession) i64 {
        pi.lock();
        defer pi.unlock();
        return pi.window;
    }

    pub fn permissionMode(pi: *PiSession) []const u8 {
        pi.lock();
        defer pi.unlock();
        return pi.mode;
    }

    /// The turn an interrupt was asked for (empty when none) and whether it
    /// is still pending on the turn's start.
    pub fn interrupting(pi: *PiSession) struct { Id, bool } {
        return pi.rt().interrupting();
    }

    /// Sets one field of the session info to a copy of `value`, announcing
    /// the change with `session_updated` when `announce` is set.
    pub fn setInfoField(
        pi: *PiSession,
        announce: bool,
        comptime field: std.meta.FieldEnum(types.SessionInfo),
        value: @FieldType(types.SessionInfo, @tagName(field)),
    ) Allocator.Error!void {
        const Change = struct {
            fn apply(new_value: @TypeOf(value), info: *types.SessionInfo, a: Allocator) Allocator.Error!void {
                @field(info, @tagName(field)) = try agent.clone(@TypeOf(value), a, new_value);
            }
        };
        try pi.rt().updateInfo(value, announce, Change.apply);
    }

    /// Announces the session info with `session_updated`.
    pub fn announceInfo(pi: *PiSession) Allocator.Error!void {
        const Change = struct {
            fn apply(_: void, _: *types.SessionInfo, _: Allocator) Allocator.Error!void {}
        };
        try pi.rt().updateInfo({}, true, Change.apply);
    }

    // ------------------------------------------------------------ workers

    /// Runs `function(pi, args...)` as a task of the session. Close waits
    /// for every worker. Returns false when the session is closing or no
    /// task could start.
    pub fn spawn(pi: *PiSession, comptime function: anytype, args: anytype) bool {
        // A worker that starts while close stops Pi is still waited for.
        if (pi.closing.load(.acquire)) return false;
        pi.rt().spawn(function, .{pi} ++ args) catch return false;
        return true;
    }

    // ------------------------------------------------------------ commands

    pub fn newRequestId(pi: *PiSession) Id {
        var buf: [32]u8 = undefined;
        return .of(std.fmt.bufPrint(&buf, "req_{d}", .{pi.next_id.fetchAdd(1, .monotonic) + 1}) catch unreachable);
    }

    /// Sends RPC command `command_type` and returns its data. Waits until Pi
    /// answers or exits: once written, Pi runs the command whatever happens.
    pub fn command(pi: *PiSession, a: Allocator, command_type: []const u8, payload: anytype, diag: ?*Diagnostics) Error!JsonView {
        return pi.callOrdered(a, pi.newRequestId(), command_type, payload, null, 0, diag);
    }

    /// `command` that gives up after `timeout_ms`.
    pub fn commandTimeout(pi: *PiSession, a: Allocator, command_type: []const u8, payload: anytype, timeout_ms: i64, diag: ?*Diagnostics) Error!JsonView {
        return pi.callOrdered(a, pi.newRequestId(), command_type, payload, null, timeout_ms, diag);
    }

    /// Sends command `command_type` with request ID `id` and waits for its
    /// response (up to `timeout_ms` when positive). `reply`, when set, runs
    /// on the reader task before any later line is handled; it must not
    /// block.
    pub fn callOrdered(pi: *PiSession, a: Allocator, id: Id, command_type: []const u8, payload: anytype, reply: ?Reply, timeout_ms: i64, diag: ?*Diagnostics) Error!JsonView {
        try pi.rt().ensureOpen(diag);
        const proc = pi.proc orelse return errors.fail(diag, .closed, "pi: no process", .{});
        const line = try encodeCommand(a, id.slice(), command_type, payload, diag);
        var waiter: Waiter = .{ .id = id, .reply = reply };
        pi.wait_mutex.lockUncancelable(pi.io);
        if (pi.exited) {
            pi.wait_mutex.unlock(pi.io);
            return pi.exitError(a, diag);
        }
        try pi.waiters.append(pi.gpa, &waiter);
        pi.wait_mutex.unlock(pi.io);
        defer {
            pi.wait_mutex.lockUncancelable(pi.io);
            for (pi.waiters.items, 0..) |x, i| if (x == &waiter) {
                _ = pi.waiters.swapRemove(i);
                break;
            };
            pi.wait_mutex.unlock(pi.io);
            if (waiter.line) |l| pi.gpa.free(l);
        }
        try proc.writeLine(line, diag);
        if (timeout_ms > 0) {
            if (!kit.waitEvent(pi.io, &waiter.done, timeout_ms))
                return errors.fail(diag, .timeout, "pi: {s}: no answer within {d} ms", .{ command_type, timeout_ms });
        } else waiter.done.waitUncancelable(pi.io);
        // Set under the lock before the event.
        const got = waiter.line orelse return pi.exitError(a, diag);
        const response = JsonView.parseBytes(a, try a.dupe(u8, got));
        if (!response.flag("success")) return errors.failWith(diag, .{
            .kind = .protocol,
            .message = try std.fmt.allocPrint(a, "pi: {s}: {s}", .{ command_type, response.str("error") }),
            .code = command_type,
        });
        return response.get("data");
    }

    fn exitError(pi: *PiSession, a: Allocator, diag: ?*Diagnostics) Error {
        if (pi.proc) |proc| if (proc.hasExited()) return errors.failWith(diag, try proc.exitInfo(a));
        return pi.rt().closedError(diag);
    }

    pub fn writeLine(pi: *PiSession, line: []const u8, diag: ?*Diagnostics) Error!void {
        const proc = pi.proc orelse return errors.fail(diag, .closed, "pi: no process", .{});
        return proc.writeLine(line, diag);
    }

    /// Answers extension UI request `id` with the fields of `fields`.
    pub fn uiRespond(pi: *PiSession, id: []const u8, fields: anytype, diag: ?*Diagnostics) Error!void {
        var arena: std.heap.ArenaAllocator = .init(pi.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        var message: json.ObjectBuilder = .init(a);
        try message.str("type", "extension_ui_response");
        try message.str("id", id);
        try message.putAll(try paramFields(a, fields, diag));
        return pi.writeLine(try json.encodeValue(a, message.value()), diag);
    }

    /// Answers a bridge request with the JSON of `value`.
    pub fn bridgeReply(pi: *PiSession, ui_id: []const u8, value: anytype, diag: ?*Diagnostics) Error!void {
        var arena: std.heap.ArenaAllocator = .init(pi.gpa);
        defer arena.deinit();
        const text = if (@TypeOf(value) == RawJson) value.bytes else (RawJson.encode(arena.allocator(), value) catch return error.OutOfMemory).bytes;
        return pi.uiRespond(ui_id, .{ .value = text }, diag);
    }

    // ------------------------------------------------------------ process

    fn onLine(context: *anyopaque, line: []const u8) void {
        const pi: *PiSession = @ptrCast(@alignCast(context));
        defer _ = pi.line_arena.reset(.retain_capacity);
        pi.handleLine(pi.line_arena.allocator(), line) catch |e| log.warn("pi: handling output failed: {t}", .{e});
    }

    fn handleLine(pi: *PiSession, a: Allocator, line: []const u8) !void {
        const message = JsonView.parseBytes(a, line);
        if (message.object() == null) {
            log.warn("pi: ignoring malformed output", .{});
            return;
        }
        const message_type = message.str("type");
        if (eql(message_type, "response")) {
            const id = message.str("id");
            const success = message.flag("success");
            pi.wait_mutex.lockUncancelable(pi.io);
            const found = for (pi.waiters.items) |waiter| {
                if (waiter.id.eql(id)) break waiter;
            } else null;
            if (found) |waiter| {
                if (waiter.reply) |reply| reply.function(reply.context, success, message);
                if (waiter.line == null) waiter.line = try pi.gpa.dupe(u8, line);
                waiter.done.set(pi.io);
            }
            pi.wait_mutex.unlock(pi.io);
            if (found == null and !success) {
                pi.rt().emit(.{ .raw = .of(line), .payload = .{ .notice = .{ .level = .@"error", .code = message.str("command"), .message = message.str("error") } } });
            }
        } else if (eql(message_type, "extension_ui_request")) {
            try pi.extensionUi(a, message, line);
        } else {
            try pi.translator.handle(a, message_type, message, line);
        }
    }

    fn onExit(context: *anyopaque, err: ?types.ErrorInfo) void {
        const pi: *PiSession = @ptrCast(@alignCast(context));
        pi.wait_mutex.lockUncancelable(pi.io);
        pi.exited = true;
        for (pi.waiters.items) |waiter| waiter.done.set(pi.io);
        pi.wait_mutex.unlock(pi.io);
        pi.rt().finish(if (pi.closing.load(.acquire)) null else err);
    }

    fn onTap(context: *anyopaque, sent: bool, line: []const u8) void {
        const pi: *PiSession = @ptrCast(@alignCast(context));
        pi.rt().tap(if (sent) .send else .receive, line);
    }

    // ------------------------------------------------------------ bookkeeping

    /// Forgets the session entries of earlier turns.
    pub fn forgetTurns(pi: *PiSession) void {
        pi.lock();
        defer pi.unlock();
        pi.turn_entries.clearRetainingCapacity();
    }

    /// Records the first user message of turn `turn`; returns whether it is
    /// the turn's first.
    pub fn recordTurnStamp(pi: *PiSession, turn: []const u8, stamp: i64) Allocator.Error!bool {
        pi.lock();
        defer pi.unlock();
        for (pi.turn_entries.items) |entry| if (entry.turn.eql(turn)) return false;
        if (pi.turn_entries.items.len >= max_remembered_turns) _ = pi.turn_entries.orderedRemove(0);
        try pi.turn_entries.append(pi.gpa, .{ .turn = .of(turn), .stamp = stamp });
        return true;
    }

    pub fn recordTurnEntry(pi: *PiSession, turn: []const u8, entry_id: []const u8) void {
        pi.lock();
        defer pi.unlock();
        for (pi.turn_entries.items) |*entry| if (entry.turn.eql(turn)) {
            entry.entry = .of(entry_id);
        };
    }

    pub fn takeDenied(pi: *PiSession, call_id: []const u8) bool {
        pi.lock();
        defer pi.unlock();
        return pi.denied.take(pi.gpa, call_id) != null;
    }

    /// Forgets turn `id`'s output request; returns whether it had one and
    /// whether its schema was wrapped.
    pub fn takeOutput(pi: *PiSession, id: []const u8) struct { bool, bool } {
        pi.lock();
        defer pi.unlock();
        for (pi.outputs.items, 0..) |request, i| if (request.id.eql(id)) {
            _ = pi.outputs.swapRemove(i);
            return .{ true, request.wrapped };
        };
        return .{ false, false };
    }

    /// Remembers `client_id` for the user message Pi echoes for prompt
    /// `message`, which carries no structured output line: the bridge strips
    /// it before Pi records the message.
    pub fn expectClientId(pi: *PiSession, client_id: []const u8, message: []const u8) Allocator.Error!void {
        if (client_id.len == 0) return;
        var text = message;
        if (pi.token.len > 0) {
            var buf: [256]u8 = undefined;
            const marker = std.fmt.bufPrint(&buf, "\n{s}:output:", .{pi.token}) catch "";
            if (marker.len > 0) if (std.mem.lastIndexOf(u8, text, marker)) |i| {
                text = text[0..i];
            };
        }
        pi.lock();
        defer pi.unlock();
        // Prompts Pi rewrites, such as skills, are never matched; keep the
        // list short.
        if (pi.client_ids.items.len >= 16) pi.client_ids.orderedRemove(0).deinit(pi.gpa);
        const owned_text = try pi.gpa.dupe(u8, text);
        const owned_id = pi.gpa.dupe(u8, client_id) catch |e| {
            pi.gpa.free(owned_text);
            return e;
        };
        const pending: PendingClient = .{ .text = owned_text, .client_id = owned_id };
        pi.client_ids.append(pi.gpa, pending) catch |e| {
            pending.deinit(pi.gpa);
            return e;
        };
    }

    fn dropClientId(pi: *PiSession, client_id: []const u8) void {
        if (client_id.len == 0) return;
        pi.lock();
        defer pi.unlock();
        var i: usize = 0;
        while (i < pi.client_ids.items.len) {
            if (eql(pi.client_ids.items[i].client_id, client_id)) pi.client_ids.orderedRemove(i).deinit(pi.gpa) else i += 1;
        }
    }

    fn clearClientIds(pi: *PiSession) void {
        pi.lock();
        defer pi.unlock();
        for (pi.client_ids.items) |pending| pending.deinit(pi.gpa);
        pi.client_ids.clearRetainingCapacity();
    }

    /// The client ID of the pending prompt whose text is `text`, copied into
    /// `a`, and forgets it.
    pub fn takeClientId(pi: *PiSession, a: Allocator, text: []const u8) Allocator.Error![]const u8 {
        pi.lock();
        defer pi.unlock();
        // Pi appends hints, such as image resizing notes, on lines after the
        // prompt.
        for (pi.client_ids.items, 0..) |pending, i| {
            if (eql(text, pending.text) or (std.mem.startsWith(u8, text, pending.text) and text.len > pending.text.len and text[pending.text.len] == '\n')) {
                const out = try a.dupe(u8, pending.client_id);
                pi.client_ids.orderedRemove(i).deinit(pi.gpa);
                return out;
            }
        }
        return "";
    }

    pub fn addSubagentFiles(pi: *PiSession, files: []const decode.SubagentFile) Allocator.Error!void {
        if (files.len == 0) return;
        pi.lock();
        defer pi.unlock();
        for (files) |file| try pi.subagent_files.putCopy(pi.gpa, file.task_id, file.path);
    }

    // ------------------------------------------------------------ input

    /// Starts input as turn `id`, which holds the running slot: a built-in
    /// command runs in the background as a local turn, other input is sent
    /// as a prompt.
    fn start(pi: *PiSession, id: []const u8, in: types.Input, diag: ?*Diagnostics) Error!void {
        var arena: std.heap.ArenaAllocator = .init(pi.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        try control.applyInputSettings(pi, in, diag);
        const builtin = try pi.builtinCommand(a, in);
        // The commands may have changed since the input was sent.
        pi.rt().setLocal(id, builtin != null);
        if (builtin) |slash| {
            const args = try pi.gpa.dupe(u8, slash.args);
            if (!pi.spawn(control.runBuiltin, .{ Id.of(id), slash.kind, args })) {
                pi.gpa.free(args);
                return errors.fail(diag, .closed, "pi: session closing", .{});
            }
            return;
        }
        var input = in;
        if (input.output_schema == null and !try pi.extensionCommand(a, in)) input.output_schema = pi.cfg.output_schema;
        const rendered = try pi.promptFor(a, id, input, diag);
        return pi.prompt(a, id, in.client_id, rendered, diag);
    }

    fn startQueued(context: *anyopaque, id: []const u8, in: types.Input, diag: ?*Diagnostics) Error!void {
        const pi: *PiSession = @ptrCast(@alignCast(context));
        return pi.start(id, in, diag);
    }

    /// Renders input for the prompt of turn `id`. With an output schema it
    /// appends the line the bridge turns into the structured_output tool
    /// and remembers that the turn wants structured output.
    fn promptFor(pi: *PiSession, a: Allocator, id: []const u8, in: types.Input, diag: ?*Diagnostics) Error!Prompt {
        var rendered = try pi.promptPayload(a, in, diag);
        const schema = in.output_schema orelse return rendered;
        const out = try decode.outputSchema(a, schema, diag);
        const payload = try std.fmt.allocPrint(a, "{{\"schema\":{s}}}", .{out.params.bytes});
        const encoder = std.base64.url_safe_no_pad.Encoder;
        const encoded = try a.alloc(u8, encoder.calcSize(payload.len));
        _ = encoder.encode(encoded, payload);
        {
            pi.lock();
            defer pi.unlock();
            try pi.outputs.append(pi.gpa, .{ .id = .of(id), .wrapped = out.wrapped });
        }
        rendered.message = try std.fmt.allocPrint(a, "{s}\n{s}:output:{s}", .{ rendered.message, pi.token, encoded });
        return rendered;
    }

    /// Sends the prompt of turn `id`. A prompt an extension command
    /// consumed ends the turn at once, as no run starts.
    fn prompt(pi: *PiSession, a: Allocator, id: []const u8, client_id: []const u8, rendered: Prompt, diag: ?*Diagnostics) Error!void {
        const result = pi.sendPrompt(a, client_id, rendered, diag);
        if (result) |disposition| {
            if ((eql(disposition, "started") or eql(disposition, "queued")) and pi.rt().accepted(id, "")) {
                // Interrupted while the prompt started.
                _ = pi.command(a, "abort", .{}, null) catch pi.rt().clearInterrupt(id);
            }
            if (eql(disposition, "handled")) {
                // No run starts, so no run result takes the output request.
                _ = pi.takeOutput(id);
                pi.rt().finishTurn(id, .{ .stop_reason = "handled" });
            }
        } else |e| {
            _ = pi.takeOutput(id);
            return e;
        }
    }

    /// Sends a prompt that steers a run Pi already started, so it never
    /// fails because Pi is busy, and returns Pi's disposition: started,
    /// queued, or handled.
    fn sendPrompt(pi: *PiSession, a: Allocator, client_id: []const u8, rendered: Prompt, diag: ?*Diagnostics) Error![]const u8 {
        try pi.expectClientId(client_id, rendered.message);
        // Once written, Pi runs the prompt, so the reply is awaited
        // regardless. It can wait for a compaction Pi runs before the
        // prompt; the wait ends if Pi exits.
        const data = pi.command(a, "prompt", .{
            .message = rendered.message,
            .streamingBehavior = "steer",
            .images = if (rendered.images.len > 0) rendered.images else null,
        }, diag) catch |e| {
            pi.dropClientId(client_id);
            return e;
        };
        return data.str("disposition");
    }

    /// Injects input into the running turn. Pi may have gone idle before
    /// it reads the command, in which case it starts a new run that becomes
    /// turn `id`. Input Pi queues into the running run joins it: turn `id`
    /// then ends with the running turn's result.
    fn steer(pi: *PiSession, a: Allocator, in: types.Input, id: []const u8, current: Id, rendered: Prompt, diag: ?*Diagnostics) Error!void {
        pi.steer_mutex.lockUncancelable(pi.io);
        defer pi.steer_mutex.unlock(pi.io);
        const runtime = pi.rt();
        // Queued input waits while the steer may still start a run. Input
        // for a turn that is still starting, or that ended, is queued.
        if (!runtime.beginSteer(id, current.slice())) return runtime.enqueue(id, in, .{}, diag);
        {
            errdefer runtime.endSteer(id);
            // A steer's model and effort apply to the run it joins; images
            // are checked against that model.
            try control.applyInputSettings(pi, in, diag);
            if (in.model.len > 0) _ = try pi.promptPayload(a, in, diag);
            try runtime.newTurn(id, diag);
        }
        var prompt_diag: Diagnostics = .{};
        var result = pi.sendPrompt(a, in.client_id, rendered, &prompt_diag);
        if (result) |_| {} else |e| {
            if (std.mem.indexOf(u8, Diagnostics.of(&prompt_diag, e).message, "compaction is in progress") != null) {
                // Pi refuses prompts during a manual compaction started
                // outside a built-in turn; its steering queue still takes
                // the message.
                if (pi.command(a, "steer", .{ .message = rendered.message, .images = if (rendered.images.len > 0) rendered.images else null }, null)) |_| {
                    result = "queued";
                    // Pi already took the input: finish the steer either way.
                    pi.expectClientId(in.client_id, rendered.message) catch
                        log.err("out of memory: client ID of a steer not recorded", .{});
                } else |_| {}
            }
        }
        // Pi started a run for the steer and it took the slot; the run is
        // live whatever the reply said.
        if (runtime.runningTurn()[0].eql(id)) return;
        const disposition = result catch |e| {
            const info = Diagnostics.of(&prompt_diag, e);
            runtime.endSteer(id);
            runtime.abandonTurn(id, info);
            return errors.failWith(diag, info);
        };
        if (!eql(disposition, "started")) runtime.endSteer(id);
        if (eql(disposition, "started")) return;
        if (eql(disposition, "handled")) return runtime.finishTurn(id, .{ .stop_reason = "handled" });
        var join_diag: Diagnostics = .{};
        runtime.joinTurn(id, current.slice(), &join_diag) catch |e| runtime.abandonTurn(id, Diagnostics.of(&join_diag, e));
    }

    /// Renders input for the prompt command. Images are refused when the
    /// current model does not accept them.
    fn promptPayload(pi: *PiSession, a: Allocator, in: types.Input, diag: ?*Diagnostics) Error!Prompt {
        const rendered = try renderPrompt(pi.env(), a, in, diag);
        if (rendered.images.len == 0) return rendered;
        pi.lock();
        const images = pi.model_images;
        pi.unlock();
        if (images == false) {
            var arena: std.heap.ArenaAllocator = .init(pi.gpa);
            defer arena.deinit();
            const info = try pi.rt().sessionInfo(arena.allocator());
            return errors.fail(diag, .unsupported, "pi: image input for model {s} is not supported", .{info.model});
        }
        return rendered;
    }

    pub const BuiltinKind = enum { compact, autocompact };

    /// Input "/compact" or "/autocompact", which Pi runs only in its
    /// terminal UI, unless an extension defines the name.
    pub fn builtinCommand(pi: *PiSession, a: Allocator, in: types.Input) Error!?struct { kind: BuiltinKind, args: []const u8 } {
        for (try in.allParts(a)) |part| if (part.type != .text) return null;
        const slash = try slashCommand(a, in) orelse return null;
        const kind = std.meta.stringToEnum(BuiltinKind, slash.name) orelse return null;
        const info = try pi.rt().sessionInfo(a);
        for (info.commands) |c| if (eql(c.name, slash.name) and !eql(c.scope, "builtin")) return null;
        return .{ .kind = kind, .args = std.mem.trim(u8, slash.args, " \t\r\n") };
    }

    /// Whether input runs an extension command, which Pi runs with the rest
    /// of the prompt as its arguments.
    pub fn extensionCommand(pi: *PiSession, a: Allocator, in: types.Input) Error!bool {
        const slash = try slashCommand(a, in) orelse return false;
        const info = try pi.rt().sessionInfo(a);
        for (info.commands) |c| if (eql(c.name, slash.name) and eql(c.kind, "extension")) return true;
        return false;
    }

    /// Refuses a mode that gates tool calls unless the bridge, whose gate
    /// enforces it, is loaded.
    pub fn checkGate(pi: *PiSession, mode: []const u8, diag: ?*Diagnostics) Error!void {
        if (gatesNothing(mode) or pi.bridge_loaded.load(.acquire)) return;
        return errors.fail(diag, .unsupported, "pi: the bridge extension did not load, so permission mode {s} cannot be enforced", .{mode});
    }

    // ------------------------------------------------------------ interrupt

    /// Aborts the running turn if it is `want`, or any running turn when
    /// `want` is empty. It first clears Pi's queued steering and follow-up
    /// messages, which Pi would otherwise run after the abort.
    pub fn interruptTurn(pi: *PiSession, want: []const u8, diag: ?*Diagnostics) Error!void {
        var arena: std.heap.ArenaAllocator = .init(pi.gpa);
        defer arena.deinit();
        const a = arena.allocator();
        const runtime = pi.rt();
        // The record marks the turn interrupted when it settles.
        const current, const starting = runtime.requestInterrupt(want);
        if (current.isEmpty()) return;
        if (starting and !runtime.isLocal(current.slice())) {
            // Before the "started" reply Pi may compact, and an abort stops
            // that compaction; otherwise Pi ignores it. Either way the
            // record stays pending, and prompt sends the abort again once Pi
            // reports the run started.
            if (pi.compacting.load(.acquire)) _ = try pi.command(a, "abort", .{}, diag);
            return;
        }
        // Pi's abort waits for the run to stop, and a tool call waiting on a
        // bridge answer cannot stop until it gets one.
        for (try runtime.pendingRequests(a)) |event| switch (event.payload) {
            .permission_request => |request| pi.answerInterrupted(request.id, .{ .permission = .{ .decision = .deny, .message = "The turn was interrupted." } }),
            .user_input_request => |request| pi.answerInterrupted(request.id, .{ .user_input = .{ .cancelled = true } }),
            else => {},
        };
        var clear_diag: Diagnostics = .{};
        var clear_error: ?types.ErrorInfo = null;
        if (pi.command(a, "clear_queue", .{}, &clear_diag)) |_| {
            // Steers Pi had queued are gone; their echoes never arrive.
            pi.clearClientIds();
        } else |e| {
            const info = Diagnostics.of(&clear_diag, e);
            // Older Pi versions lack the command.
            if (std.mem.indexOf(u8, info.message, "Unknown command") == null) clear_error = try agent.clone(types.ErrorInfo, a, info);
        }
        if (!pi.interrupting()[0].eql(current.slice())) {
            // The turn ended while the queue was cleared; an abort now would
            // stop the input that replaced it.
            if (clear_error) |info| return errors.failWith(diag, info);
            return;
        }
        // A refused abort is cleared as its response is read, so a turn end
        // that follows it is not taken as aborted.
        const AbortReply = struct {
            pi: *PiSession,
            id: Id,
            fn onReply(context: *anyopaque, success: bool, _: JsonView) void {
                const reply: *@This() = @ptrCast(@alignCast(context));
                if (!success) reply.pi.rt().clearInterrupt(reply.id.slice());
            }
        };
        var abort_reply: AbortReply = .{ .pi = pi, .id = current };
        _ = pi.callOrdered(a, pi.newRequestId(), "abort", .{}, .{ .context = &abort_reply, .function = AbortReply.onReply }, 0, diag) catch |e| {
            runtime.clearInterrupt(current.slice());
            return e;
        };
        if (clear_error) |info| return errors.failWith(diag, info);
    }

    /// Answers a request of a turn being interrupted, so the tool call
    /// waiting on it can stop.
    fn answerInterrupted(pi: *PiSession, request_id: []const u8, response: types.Response) void {
        var answer_diag: Diagnostics = .{};
        pi.session.respond(request_id, response, &answer_diag) catch |e| switch (e) {
            // Answered or withdrawn meanwhile.
            error.RequestDone => {},
            else => log.err("pi: answering request {s} of an interrupted turn failed: {s}", .{ request_id, Diagnostics.of(&answer_diag, e).message }),
        };
    }

    // ------------------------------------------------------------ extension UI

    /// The extension UI methods Pi sends; `other` stands for the rest.
    const UiMethod = enum { input, setStatus, notify, select, confirm, editor, other };

    /// Handles extension UI requests: bridge calls, dialogs, and
    /// fire-and-forget display updates.
    fn extensionUi(pi: *PiSession, a: Allocator, request: JsonView, line: []const u8) !void {
        const raw: RawJson = .of(line);
        const method = std.meta.stringToEnum(UiMethod, request.str("method")) orelse .other;
        if (pi.token.len > 0) switch (method) {
            .input => if (eql(request.str("title"), pi.token)) return pi.startBridgeCall(request, line),
            .setStatus => if (try pi.bridgeStatus(a, request, raw)) return,
            else => {},
        };
        switch (method) {
            .notify => {
                const level = std.meta.stringToEnum(enum { warning, @"error" }, request.str("notifyType"));
                pi.rt().emit(.{ .raw = raw, .payload = .{ .notice = .{
                    .level = if (level) |l| switch (l) {
                        .warning => .warning,
                        .@"error" => .@"error",
                    } else .info,
                    .code = "extension",
                    .message = request.str("message"),
                } } });
            },
            .select, .confirm, .input, .editor => try pi.askUser(a, request, method, raw),
            .setStatus, .other => {
                const data = try RawJson.encode(a, .{ .method = request.str("method"), .fields = raw });
                pi.rt().emit(.{ .raw = raw, .payload = .{ .native = .{ .name = "pi/ui_update", .data = data } } });
            },
        }
    }

    /// Runs a bridge call (a host tool or a permission check) as a worker,
    /// since it waits for the host.
    fn startBridgeCall(pi: *PiSession, request: JsonView, line: []const u8) !void {
        const id = request.str("id");
        const job = try BridgeJob.create(pi.gpa, .{ .id = id, .payload = request.str("placeholder"), .line = line });
        if (!pi.spawn(bridgeCall, .{job})) {
            job.destroy(pi.gpa);
            try pi.uiRespond(id, .{ .cancelled = true }, null);
        }
    }

    /// Handles a status the bridge set to report to the host: a subagent
    /// event, a user entry, or a rewind answer. Returns false for other
    /// statuses.
    fn bridgeStatus(pi: *PiSession, a: Allocator, request: JsonView, raw: RawJson) !bool {
        const key = request.str("statusKey");
        const text = request.str("statusText");
        if (!std.mem.startsWith(u8, key, pi.token) or key.len <= pi.token.len or key[pi.token.len] != ':') return false;
        const BridgeStatus = enum { event, user, rewind };
        switch (std.meta.stringToEnum(BridgeStatus, key[pi.token.len + 1 ..]) orelse return false) {
            .event => try pi.subagentEvent(a, text, raw),
            .user => try pi.translator.userEntry(a, text),
            .rewind => try pi.rewindAnswer(a, text),
        }
        return true;
    }

    /// Asks the host a dialog question as a user input request.
    fn askUser(pi: *PiSession, a: Allocator, request: JsonView, method: UiMethod, raw: RawJson) !void {
        const id = request.str("id");
        const title = request.str("title");
        var question: types.Question = .{ .id = "answer", .text = title, .header = title };
        switch (method) {
            .select => {
                const choices = request.get("options").items();
                const out = try a.alloc(types.QuestionOption, choices.len);
                for (choices, out) |choice, *option| option.* = .{ .label = JsonView.of(choice).string() };
                question.options = out;
            },
            .confirm => {
                question.text = std.mem.trim(u8, try std.fmt.allocPrint(a, "{s}\n\n{s}", .{ title, request.str("message") }), " \t\r\n");
                question.options = &.{ .{ .label = "Yes" }, .{ .label = "No" } };
            },
            .input => question.placeholder = request.str("placeholder"),
            .editor => {
                question.multiline = true;
                question.default = request.str("prefill");
            },
            else => unreachable,
        }
        const answer_native = try RawJson.encode(a, NativeAnswer{ .k = "ui", .ui = id, .method = request.str("method") });
        pi.rt().request(.{ .user_input_request = .{ .id = id, .title = title, .questions = try a.dupe(types.Question, &.{question}) } }, raw, answer_native.bytes);
        const timeout = request.int("timeout");
        if (timeout > 0) {
            // Pi answers the dialog itself when its timeout expires. Without
            // a worker the request stays until it is answered.
            _ = pi.spawn(withdrawLater, .{ Id.of(id), timeout });
        }
    }

    fn withdrawLater(pi: *PiSession, id: Id, ms: i64) void {
        if (!kit.waitEvent(pi.io, &pi.stopped, ms)) pi.rt().withdrawRequest(id.slice());
    }

    /// Answers a bridge call. Runs as a worker; a failure is logged, since
    /// the bridge then waits for an answer that never comes.
    fn bridgeCall(pi: *PiSession, job: *BridgeJob) void {
        defer job.destroy(pi.gpa);
        var arena: std.heap.ArenaAllocator = .init(pi.gpa);
        defer arena.deinit();
        var diag: Diagnostics = .{};
        const call = job.value;
        pi.answerBridgeCall(arena.allocator(), call.id, call.payload, call.line, &diag) catch |e|
            log.err("pi: answering bridge call {s} failed: {s}", .{ call.id, Diagnostics.of(&diag, e).message });
    }

    fn answerBridgeCall(pi: *PiSession, a: Allocator, ui_id: []const u8, payload: []const u8, line: []const u8, diag: ?*Diagnostics) Error!void {
        const request = JsonView.parseBytes(a, payload);
        const BridgeCall = enum { tool, permission };
        const kind = if (request.object() != null) std.meta.stringToEnum(BridgeCall, request.str("kind")) else null;
        switch (kind orelse return pi.uiRespond(ui_id, .{ .cancelled = true }, diag)) {
            .tool => {
                const tool_name = request.str("name");
                const result: types.ToolResult = for (pi.cfg.tools) |tool| {
                    if (eql(tool.name, tool_name)) {
                        const session_id = pi.session.id(a) catch "";
                        break pi.session.callTool(a, .{ .id = request.str("id"), .name = tool_name, .arguments = request.get("arguments").raw(a) catch null, .session_id = session_id });
                    }
                } else types.ToolResult.err(a, std.fmt.allocPrint(a, "unknown tool {s}", .{tool_name}) catch "unknown tool") catch .out_of_memory;
                return pi.bridgeReply(ui_id, try bridgeToolResult(a, result), diag);
            },
            .permission => return pi.gate(a, ui_id, request, .of(line), diag),
        }
    }

    /// Decides a tool call for the bridge, asking the host when the mode
    /// requires it.
    fn gate(pi: *PiSession, a: Allocator, ui_id: []const u8, request: JsonView, raw: RawJson, diag: ?*Diagnostics) Error!void {
        const tool = request.str("toolName");
        const call_id = request.str("toolCallId");
        const input = request.get("input");
        const command_text = input.str("command");
        const path = input.str("path");
        const grant = try decode.grantKey(a, tool, command_text, path);
        pi.lock();
        const mode = pi.mode;
        const configured = kit.contains(pi.cfg.allowed_tools, tool) or kit.contains(pi.approved_mcp, tool);
        const granted = pi.allowed.contains(grant);
        pi.unlock();
        // A read-only hint counts only from built-in extensions, such as MCP
        // servers and tool search; any other extension could claim it.
        var read_only = decode.isReadTool(tool) or (request.flag("readOnly") and std.mem.startsWith(u8, request.str("source"), "builtin:"));
        var host_approved = false;
        for (pi.cfg.tools) |host_tool| if (eql(host_tool.name, tool)) {
            host_approved = !host_tool.needs_approval;
            read_only = host_tool.read_only;
        };
        const pm = types.permission_mode;
        // The mode decides first, so no grant or allowed tool lifts the
        // refusals of read_only, plan, and dont_ask.
        if (gatesNothing(mode) or read_only) return pi.bridgeReply(ui_id, .{ .allow = true }, diag);
        if (eql(mode, pm.read_only) or eql(mode, pm.plan))
            return pi.gateDeny(ui_id, call_id, try std.fmt.allocPrint(a, "{s} is not allowed in read-only mode", .{tool}), diag);
        if (eql(mode, pm.dont_ask)) {
            // Only the session's configuration pre-approves tools here.
            if (configured) return pi.bridgeReply(ui_id, .{ .allow = true }, diag);
            return pi.gateDeny(ui_id, call_id, try std.fmt.allocPrint(a, "{s} is not pre-approved", .{tool}), diag);
        }
        if (configured or granted or host_approved) return pi.bridgeReply(ui_id, .{ .allow = true }, diag);
        if (eql(mode, pm.accept_edits) and decode.isEditTool(tool)) return pi.bridgeReply(ui_id, .{ .allow = true }, diag);

        var kind: types.PermissionKind = .tool;
        var command_line: []const u8 = "";
        var changes: []const types.FileChange = &.{};
        if (eql(tool, "bash")) {
            kind = .command;
            command_line = command_text;
        } else if (decode.isEditTool(tool)) {
            kind = .file_change;
            changes = try a.dupe(types.FileChange, &.{.{ .path = path, .kind = .update }});
        }
        const title = try std.fmt.allocPrint(a, "Run {s}", .{if (command_line.len > 0) command_line else tool});
        const answer_native = try RawJson.encode(a, NativeAnswer{ .k = "gate", .ui = ui_id, .grant = grant, .call = call_id });
        pi.rt().request(.{ .permission_request = .{
            .id = ui_id,
            .kind = kind,
            .tool_name = tool,
            .tool_call_id = call_id,
            .input = try input.raw(a),
            .title = title,
            .command = command_line,
            .changes = changes,
            .decisions = &.{ .allow, .allow_session, .deny, .cancel },
        } }, raw, answer_native.bytes);
    }

    fn gateDeny(pi: *PiSession, ui_id: []const u8, call_id: []const u8, reason: []const u8, diag: ?*Diagnostics) Error!void {
        {
            pi.lock();
            defer pi.unlock();
            try pi.denied.put(pi.gpa, call_id, {});
        }
        return pi.bridgeReply(ui_id, .{ .allow = false, .reason = reason }, diag);
    }

    /// The subagent events the bridge forwards from the extension event bus.
    const SubagentEvent = enum { created, started, steered, compacted, completed, failed };

    /// Maps a subagents:* event the bridge forwarded to a task update.
    fn subagentEvent(pi: *PiSession, a: Allocator, text: []const u8, raw: RawJson) !void {
        const forwarded = JsonView.parseBytes(a, text);
        const payload = forwarded.get("payload");
        const id = payload.str("id");
        if (id.len == 0) return;
        const event = std.meta.stringToEnum(SubagentEvent, forwarded.str("event"));
        var snapshot: types.Task = undefined;
        {
            pi.lock();
            defer pi.unlock();
            const entry = for (pi.tasks.items) |task_entry| {
                if (eql(task_entry.value.id, id)) break task_entry;
            } else blk: {
                const task_entry = try TaskEntry.create(pi.gpa, .{ .id = id, .kind = .subagent, .native_kind = "pi-subagent", .status = .running, .subagent = .{ .depth = 1 } });
                pi.tasks.append(pi.gpa, task_entry) catch |e| {
                    task_entry.destroy(pi.gpa);
                    return e;
                };
                break :blk task_entry;
            };
            const task_arena = entry.arena.allocator();
            const task = &entry.value;
            if (payload.str("description").len > 0) task.description = try task_arena.dupe(u8, payload.str("description"));
            if (payload.str("type").len > 0) task.subagent.?.type = try task_arena.dupe(u8, payload.str("type"));
            if (payload.has("isBackground")) task.subagent.?.background = payload.flag("isBackground");
            if (event) |kind| switch (kind) {
                .created, .started => task.status = .running,
                .steered => task.summary = try std.fmt.allocPrint(task_arena, "steered: {s}", .{payload.str("message")}),
                .compacted => task.summary = "context compacted",
                .completed, .failed => {
                    const status = payload.str("status");
                    task.status = if (eql(status, "stopped") or eql(status, "aborted")) .stopped else if (kind == .failed) .failed else .completed;
                    task.output = try task_arena.dupe(u8, payload.str("result"));
                    task.@"error" = try task_arena.dupe(u8, payload.str("error"));
                    task.tool_uses = payload.uint32("toolUses");
                    task.duration_ms = payload.int("durationMs");
                    if (payload.has("usage")) task.usage = decode.usage(payload.get("usage"));
                },
            };
            snapshot = try agent.clone(types.Task, a, task.*);
            if (task.status != .running) {
                for (pi.tasks.items, 0..) |task_entry, i| if (task_entry == entry) {
                    _ = pi.tasks.swapRemove(i);
                    break;
                };
                entry.destroy(pi.gpa);
            }
        }
        pi.rt().emit(.{ .raw = raw, .payload = .{ .task_updated = .{ .task = snapshot } } });
    }

    fn rewindAnswer(pi: *PiSession, a: Allocator, text: []const u8) !void {
        const answer = JsonView.parseBytes(a, text);
        pi.lock();
        defer pi.unlock();
        for (pi.rewinds.items) |slot| if (slot.nonce.eql(answer.str("nonce")) and !slot.done) {
            slot.done = true;
            slot.ok = answer.flag("ok");
            slot.cancelled = answer.flag("cancelled");
            slot.err = try pi.gpa.dupe(u8, answer.str("error"));
        };
    }
};

// ---------------------------------------------------------------- types

pub const Reply = struct {
    context: *anyopaque,
    /// Runs on the reader task with the response message.
    function: *const fn (context: *anyopaque, success: bool, response: JsonView) void,
};

const Waiter = struct {
    id: Id,
    done: Io.Event = .unset,
    /// The response line, owned by the session's allocator.
    line: ?[]u8 = null,
    reply: ?Reply = null,
};

const TurnEntry = struct { turn: Id, stamp: i64, entry: Id = .{} };

const OutputRequest = struct { id: Id, wrapped: bool };

const PendingClient = struct {
    text: []u8,
    client_id: []u8,
    fn deinit(pending: PendingClient, gpa: Allocator) void {
        gpa.free(pending.text);
        gpa.free(pending.client_id);
    }
};

pub const RewindSlot = struct {
    nonce: Id,
    done: bool = false,
    ok: bool = false,
    cancelled: bool = false,
    err: []const u8 = "",
};

/// A bridge call waiting for its worker: the request's ID, its payload, and
/// the line it came in.
const BridgeJob = kit.Owned(struct { id: []const u8, payload: []const u8, line: []const u8 });

/// A task with the memory it points to.
const TaskEntry = kit.Owned(types.Task);

/// What a request's answer needs, stored with the pending request.
const NativeAnswer = struct {
    k: []const u8,
    ui: []const u8,
    method: []const u8 = "",
    grant: []const u8 = "",
    call: []const u8 = "",
};

pub const Image = struct { type: []const u8 = "image", data: []const u8, mimeType: []const u8 };

pub const Prompt = struct { message: []const u8, images: []const Image = &.{} };

// ---------------------------------------------------------------- helpers

pub fn gatesNothing(mode: []const u8) bool {
    return mode.len == 0 or eql(mode, types.permission_mode.bypass);
}

/// The fields of command parameters `params`, a struct or a RawJson holding
/// an object. A RawJson's "id" and "type" are dropped: the command sets them.
fn paramFields(a: Allocator, params: anytype, diag: ?*Diagnostics) Error!std.json.ObjectMap {
    const T = @TypeOf(params);
    if (T == RawJson or T == ?RawJson) {
        const optional: ?RawJson = params;
        const raw = optional orelse return .empty;
        if (raw.bytes.len == 0 or eql(raw.bytes, "null")) return .empty;
        const parsed = json.parseValue(a, raw.bytes) orelse
            return errors.fail(diag, .invalid_params, "pi: command params are not valid JSON", .{});
        if (parsed != .object) return errors.fail(diag, .invalid_params, "pi: command params must be a JSON object", .{});
        var fields = parsed.object;
        _ = fields.orderedRemove("id");
        _ = fields.orderedRemove("type");
        return fields;
    }
    // An empty literal `.{}` encodes as an array and means no fields.
    const value = try json.toValue(a, params);
    return if (value == .object) value.object else .empty;
}

fn encodeCommand(a: Allocator, id: []const u8, command_type: []const u8, payload: anytype, diag: ?*Diagnostics) Error![]const u8 {
    var message: json.ObjectBuilder = .init(a);
    try message.str("id", id);
    try message.str("type", command_type);
    try message.putAll(try paramFields(a, payload, diag));
    return json.encodeValue(a, message.value());
}

fn bridgeToolResult(a: Allocator, result: types.ToolResult) Allocator.Error!RawJson {
    const Content = struct { type: []const u8, text: ?[]const u8 = null, data: ?[]const u8 = null, mimeType: ?[]const u8 = null };
    const content = try a.alloc(Content, result.content.len);
    for (result.content, content) |part, *out| out.* = switch (part.type) {
        .text => .{ .type = "text", .text = part.text },
        .image => .{ .type = "image", .data = part.data, .mimeType = part.media_type },
    };
    return RawJson.encode(a, .{ .content = content, .isError = result.is_error, .structured = result.structured }) catch error.OutOfMemory;
}

/// Splits input "/name args" into its name and arguments.
pub fn slashCommand(a: Allocator, in: types.Input) Allocator.Error!?struct { name: []const u8, args: []const u8 } {
    const text = std.mem.trim(u8, try in.plainText(a), " \t\r\n");
    if (!std.mem.startsWith(u8, text, "/")) return null;
    const rest = text[1..];
    const space = std.mem.indexOfScalar(u8, rest, ' ') orelse return .{ .name = rest, .args = "" };
    return .{ .name = rest[0..space], .args = rest[space + 1 ..] };
}

/// Renders input as the prompt text and images. Pi expands a skill only
/// when the prompt starts with "/skill:name", so the first skill part leads
/// and the rest of the input becomes its arguments.
pub fn renderPrompt(env: agent.Env, a: Allocator, in: types.Input, diag: ?*Diagnostics) Error!Prompt {
    const parts = try in.allParts(a);
    var images: std.ArrayList(Image) = .empty;
    for (parts) |part| {
        if (part.type != .image) continue;
        if (part.url.len > 0) return errors.unsupported(diag, name, "image URLs");
        var read_diag: Diagnostics = .{};
        const image = kit.readPart(env, a, part, &read_diag) catch |e|
            return errors.fail(diag, .invalid_params, "pi: image {s}: {s}", .{ part.path, Diagnostics.of(&read_diag, e).message });
        try images.append(a, .{ .data = image.data, .mimeType = image.media_type });
    }
    var message: []const u8 = undefined;
    const skill = for (parts, 0..) |part, i| {
        if (part.type == .skill) break i;
    } else null;
    if (skill) |i| {
        const skill_name = parts[i].name;
        const prefix: []const u8 = if (std.mem.startsWith(u8, skill_name, "skill:")) "" else "skill:";
        const rest = try a.alloc(types.Part, parts.len - 1);
        @memcpy(rest[0..i], parts[0..i]);
        @memcpy(rest[i..], parts[i + 1 ..]);
        const args = try (types.Input{ .parts = rest }).plainText(a);
        message = if (args.len > 0)
            try std.fmt.allocPrint(a, "/{s}{s} {s}", .{ prefix, skill_name, args })
        else
            try std.fmt.allocPrint(a, "/{s}{s}", .{ prefix, skill_name });
    } else message = try in.plainText(a);
    if (message.len == 0 and images.items.len == 0) return errors.fail(diag, .invalid_params, "pi: input has no content", .{});
    return .{ .message = message, .images = try images.toOwnedSlice(a) };
}

// ---------------------------------------------------------------- vtable

/// How `Runtime.submit` delivers input to Pi.
const Delivery = struct {
    pi: *PiSession,
    a: Allocator,
    /// The input rendered for a steer.
    rendered: Prompt,
    /// The input is a built-in command, which runs as a local turn.
    local: bool,

    pub fn start(delivery: Delivery, id: []const u8, in: types.Input, diag: ?*Diagnostics) Error!void {
        return delivery.pi.start(id, in, diag);
    }

    pub fn steer(delivery: Delivery, id: []const u8, in: types.Input, running: []const u8, diag: ?*Diagnostics) Error!void {
        return delivery.pi.steer(delivery.a, in, id, .of(running), delivery.rendered, diag);
    }

    pub fn interrupt(delivery: Delivery, running: []const u8, diag: ?*Diagnostics) Error!void {
        return delivery.pi.interruptTurn(running, diag);
    }

    /// A built-in command runs without a Pi run to steer, and input with an
    /// output schema needs a run of its own: such input is queued.
    pub fn canSteer(delivery: Delivery, running: []const u8, in: types.Input) bool {
        return !delivery.local and in.output_schema == null and !delivery.pi.rt().isLocal(running);
    }
};

/// Submits input. While Pi works, steer uses Pi's steering queue
/// (delivered before the next model call), interrupt aborts and then runs
/// the input, and queue runs the input as the next turn.
fn send(session: *Session, in: types.Input, turn_id: []const u8, diag: ?*Diagnostics) Error!void {
    const pi = PiSession.of(session);
    if (in.provider_options) |options_json| if (options_json.bytes.len > 0 and !eql(options_json.bytes, "null"))
        return errors.fail(diag, .invalid_params, "pi: Input.provider_options is set; pi has no turn options", .{});
    var arena: std.heap.ArenaAllocator = .init(pi.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const is_builtin = try pi.builtinCommand(a, in) != null;
    // A switched-to session may have lost the bridge. Built-in commands run
    // no tools, so only turns that could run tools ungated are refused.
    if (!is_builtin) try pi.checkGate(pi.permissionMode(), diag);
    if (in.output_schema) |schema| {
        if (pi.token.len == 0 or pi.opts.no_tools) return errors.unsupported(diag, name, "output schema without the bridge extension or with no_tools");
        if (is_builtin or try pi.extensionCommand(a, in)) return errors.unsupported(diag, name, "output schema for a command");
        _ = try decode.outputSchema(a, schema, diag);
    }
    // Rendered now to refuse bad input at once; start renders it again, and
    // checks images against the input's model once it switched. Built-in
    // commands are never steered, so they need no rendering.
    const rendered: Prompt = if (is_builtin)
        .{ .message = "" }
    else if (in.model.len > 0)
        try renderPrompt(pi.env(), a, in, diag)
    else
        try pi.promptPayload(a, in, diag);
    return pi.rt().submit(turn_id, in, diag, Delivery{ .pi = pi, .a = a, .rendered = rendered, .local = is_builtin });
}

fn interrupt(session: *Session, diag: ?*Diagnostics) Error!void {
    return PiSession.of(session).interruptTurn("", diag);
}

fn interruptOneTurn(session: *Session, id: []const u8, diag: ?*Diagnostics) Error!void {
    return PiSession.of(session).interruptTurn(id, diag);
}

fn respond(session: *Session, pending: *const Session.Pending, response: types.Response, diag: ?*Diagnostics) Error!void {
    const pi = PiSession.of(session);
    var arena: std.heap.ArenaAllocator = .init(pi.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const answer = RawJson.of(pending.native).parse(NativeAnswer, a) catch return errors.fail(diag, .internal, "pi: bad request data", .{});
    switch (response) {
        .permission => |permission| {
            if (permission.native) |raw| return pi.bridgeReply(answer.ui, raw, diag);
            switch (permission.decision) {
                .deny, .cancel => {
                    try pi.gateDeny(answer.ui, answer.call, if (permission.message.len > 0) permission.message else "The user denied this action.", diag);
                    if (permission.decision == .cancel and !pi.spawn(interruptWorker, .{}))
                        log.err("pi: cannot interrupt the turn after a cancelled request: the session is closing or no task could start", .{});
                    return;
                },
                .allow_session, .allow_always => {
                    pi.lock();
                    defer pi.unlock();
                    try pi.allowed.put(pi.gpa, answer.grant, {});
                },
                .allow => {},
            }
            return pi.bridgeReply(answer.ui, .{ .allow = true, .input = permission.updated_input }, diag);
        },
        .user_input => |user_input| {
            const answers: []const []const u8 = if (user_input.answers) |m| m.map.get("answer") orelse &.{} else &.{};
            if (user_input.cancelled or answers.len == 0) return pi.uiRespond(answer.ui, .{ .cancelled = true }, diag);
            if (eql(answer.method, "confirm")) return pi.uiRespond(answer.ui, .{ .confirmed = std.ascii.eqlIgnoreCase(answers[0], "yes") }, diag);
            return pi.uiRespond(answer.ui, .{ .value = try std.mem.join(a, "\n", answers) }, diag);
        },
        .elicitation => return errors.fail(diag, .invalid_answer, "pi: no elicitation requests", .{}),
    }
}

/// Interrupts the running turn after the user cancelled one of its
/// requests. Runs as a worker.
fn interruptWorker(pi: *PiSession) void {
    var diag: Diagnostics = .{};
    pi.interruptTurn("", &diag) catch |e| log.err("pi: interrupting after a cancelled request failed: {s}", .{Diagnostics.of(&diag, e).message});
}

/// Stops Pi, waits for its exit and for every worker.
fn close(session: *Session) void {
    const pi = PiSession.of(session);
    if (pi.closing.swap(true, .acq_rel)) return;
    pi.stopped.set(pi.io);
    // Pi's exit fails the commands workers wait for, so they can end.
    if (pi.proc) |proc| proc.stop(pi.provider.shutdownGraceMs());
    pi.rt().finish(null);
    pi.rt().closeTasks();
}

fn destroy(session: *Session) void {
    const pi = PiSession.of(session);
    const gpa = pi.gpa;
    if (pi.proc) |proc| proc.destroy();
    pi.translator.deinit();
    pi.waiters.deinit(gpa);
    pi.allowed.deinit(gpa);
    pi.denied.deinit(gpa);
    pi.turn_entries.deinit(gpa);
    for (pi.client_ids.items) |pending| pending.deinit(gpa);
    pi.client_ids.deinit(gpa);
    pi.rewinds.deinit(gpa);
    pi.subagent_files.deinit(gpa);
    for (pi.tasks.items) |task_entry| task_entry.destroy(gpa);
    pi.tasks.deinit(gpa);
    pi.outputs.deinit(gpa);
    pi.cfg_arena.deinit();
    pi.line_arena.deinit();
    gpa.destroy(pi);
}

// ---------------------------------------------------------------- start

/// Launches a session. Returns once Pi answered its first command, so
/// startup failures surface here.
pub fn start(provider: *Provider, opts: agent.StartOptions, diag: ?*Diagnostics) Error!*Session {
    const gpa = provider.env.gpa;
    const pi = try gpa.create(PiSession);
    pi.* = .{
        .session = undefined,
        .provider = provider,
        .gpa = gpa,
        .io = provider.env.io,
        .cfg_arena = .init(gpa),
        .translator = undefined,
        .line_arena = .init(gpa),
    };
    const ca = pi.cfg_arena.allocator();
    pi.cfg = agent.clone(types.Config, ca, opts.config) catch |e| {
        pi.cfg_arena.deinit();
        gpa.destroy(pi);
        return e;
    };
    pi.opts = kit.options(options.SessionOptions, ca, name, pi.cfg.provider_options, diag) catch |e| {
        pi.cfg_arena.deinit();
        gpa.destroy(pi);
        return e;
    };
    var caps = @import("provider.zig").capabilities(!pi.bridgeDisabled());
    if (pi.opts.no_tools) {
        caps.host_tools = false;
        caps.mcp_servers = false;
        caps.output_schema = false;
    }
    var start_opts = opts;
    start_opts.config = pi.cfg;
    pi.session = .init(provider.env, name, &PiSession.vtable, caps, start_opts);
    pi.translator = .init(pi);
    errdefer pi.session.deinit();
    pi.session.rt.setStarter(.{ .context = pi, .start = PiSession.startQueued });
    try startSession(pi, diag);
    return &pi.session;
}

fn startSession(pi: *PiSession, diag: ?*Diagnostics) Error!void {
    var arena: std.heap.ArenaAllocator = .init(pi.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const ca = pi.cfg_arena.allocator();
    const cfg = &pi.cfg;
    const runtime = pi.rt();

    if (cfg.fork and cfg.resume_id.len == 0) return errors.fail(diag, .invalid_params, "pi: fork requires resume_id", .{});
    var ignored: std.ArrayList([]const u8) = .empty;
    if (cfg.additional_directories.len > 0) try ignored.append(a, "additional_directories");
    if (cfg.output_schema) |schema| {
        _ = try decode.outputSchema(a, schema, diag);
        // The schema needs the bridge's structured_output tool.
        if (pi.bridgeDisabled() or pi.opts.no_tools) {
            try ignored.append(a, "output_schema");
            cfg.output_schema = null;
        }
    }
    if (try kit.ignoredConfig(a, name, cfg.*, ignored.items, diag)) |notice| runtime.emitPayload(notice, null);

    for (cfg.tools) |tool| if (eql(tool.name, decode.output_tool))
        return errors.fail(diag, .invalid_params, "pi: tool name \"{s}\" is reserved for structured output", .{decode.output_tool});
    var approved: std.ArrayList([]const u8) = .empty;
    if (cfg.mcp_servers) |servers| {
        var namespaces: std.StringHashMapUnmanaged([]const u8) = .empty;
        var it = servers.map.iterator();
        while (it.next()) |entry| {
            const server = entry.key_ptr.*;
            if (entry.value_ptr.resolvedTransport() == .sse)
                return errors.fail(diag, .unsupported, "pi: SSE MCP server {s} is not supported (Pi supports stdio and streamable HTTP)", .{server});
            for (server) |c| if (!(std.ascii.isAlphanumeric(c) or c == '_' or c == '-'))
                return errors.fail(diag, .invalid_params, "pi: invalid MCP server name \"{s}\" (use letters, digits, \"_\", and \"-\")", .{server});
            if (server.len == 0) return errors.fail(diag, .invalid_params, "pi: empty MCP server name", .{});
            const namespace = try a.dupe(u8, server);
            std.mem.replaceScalar(u8, namespace, '-', '_');
            if (namespaces.get(namespace)) |other| return errors.fail(diag, .invalid_params, "pi: MCP server names \"{s}\" and \"{s}\" collide (Pi treats \"-\" and \"_\" alike)", .{ other, server });
            try namespaces.put(a, namespace, server);
            for (entry.value_ptr.approved_tools) |tool| try approved.append(ca, try decode.mcpToolName(ca, server, tool));
        }
    }
    pi.approved_mcp = approved.items;
    const has_mcp = cfg.mcp_servers != null and cfg.mcp_servers.?.map.count() > 0;
    if ((cfg.tools.len > 0 or has_mcp) and pi.bridgeDisabled())
        return errors.fail(diag, .unsupported, "pi: tools and MCP servers need the bridge extension", .{});
    // Pi has no permission system: without the bridge every tool runs.
    if (pi.bridgeDisabled()) {
        if (!gatesNothing(cfg.permission_mode)) return errors.fail(diag, .unsupported, "pi: permission mode {s} is not supported without the bridge extension", .{cfg.permission_mode});
    } else try kit.checkPermissionMode(name, cfg.permission_mode, pi.session.capabilities.permission_modes, diag);
    pi.mode = cfg.permission_mode;

    var env_map: types.StringMap = .{};
    for ([_]?types.StringMap{ pi.provider.options.env, cfg.env }) |layer| if (layer) |m| {
        for (m.map.keys(), m.map.values()) |key, value| try env_map.map.put(ca, key, value);
    };
    const args = try buildArgs(pi, ca, &env_map, diag);
    const argv = try a.alloc([]const u8, args.len + 1);
    argv[0] = pi.provider.path();
    @memcpy(argv[1..], args);
    pi.proc = try Process.start(pi.env(), .{
        .provider = name,
        .argv = argv,
        .cwd = cfg.cwd,
        .env = env_map,
        .unset_env = cfg.unset_env,
        .tap = if (cfg.wire_tap) .{ .context = pi, .line = PiSession.onTap } else null,
    }, .{ .context = pi, .on_line = PiSession.onLine, .on_exit = PiSession.onExit }, diag);

    const timeout = pi.provider.startupTimeoutMs();
    var state_diag: Diagnostics = .{};
    const state = control.getState(pi, a, timeout, &state_diag) catch |e| {
        const info = Diagnostics.of(&state_diag, e);
        if (pi.proc.?.hasExited()) {
            // Let the exit handler record why the process ended.
            const exit = pi.proc.?.exitInfo(a) catch info;
            return errors.failWith(diag, .{ .kind = exit.kind, .message = try std.fmt.allocPrint(a, "pi: start: {s}", .{exit.message}), .stderr = exit.stderr });
        }
        return errors.failWith(diag, .{ .kind = info.kind, .message = try std.fmt.allocPrint(a, "pi: start: {s}", .{info.message}), .code = info.code });
    };
    try control.applyState(pi, a, state, false);
    // A resumed session file runs in the directory its header records.
    try control.followSessionCwd(pi, a, state.str("sessionFile"));
    try control.applyStartupSettings(pi, diag);
    if (cfg.resume_id.len > 0 and cfg.model.len > 0) try control.applyResumeModel(pi, a);
    control.loadCommands(pi, a);
    try pi.checkGate(cfg.permission_mode, diag);
    if (cfg.fork and cfg.fork_at_turn.len > 0) try control.truncateAfter(pi, a, cfg.fork_at_turn, diag);
    runtime.markStarted(try runtime.sessionInfo(a), cfg.resume_id.len > 0);
}

/// The pi command line.
fn buildArgs(pi: *PiSession, a: Allocator, env_map: *types.StringMap, diag: ?*Diagnostics) Error![]const []const u8 {
    const cfg = pi.cfg;
    const session_options = pi.opts;
    var args: std.ArrayList([]const u8) = .empty;
    try args.appendSlice(a, &.{ "--mode", "rpc" });
    // A resumed session keeps its model unless the requested one is
    // available; applyResumeModel sets it after start.
    if (cfg.model.len > 0 and cfg.resume_id.len == 0) try args.appendSlice(a, &.{ "--model", cfg.model });
    if (session_options.provider.len > 0) try args.appendSlice(a, &.{ "--provider", session_options.provider });
    if (cfg.effort.len > 0) try args.appendSlice(a, &.{ "--thinking", decode.thinkingLevel(cfg.effort) });
    if (cfg.system_prompt.len > 0) try args.appendSlice(a, &.{ "--system-prompt", cfg.system_prompt });
    if (cfg.append_system_prompt.len > 0) try args.appendSlice(a, &.{ "--append-system-prompt", cfg.append_system_prompt });
    if (cfg.ephemeral) try args.append(a, "--no-session");
    // Pi asks on stdin before opening a session ID recorded under another
    // directory, which breaks RPC mode; a session file path opens directly.
    var resume_path = cfg.resume_id;
    if (resume_path.len > 0) {
        const store = pi.provider.store(session_options.session_dir, env_map.*);
        if (store.findSession(a, resume_path, cfg.cwd, null)) |path| resume_path = path else |_| {}
        try args.appendSlice(a, &.{ if (cfg.fork) "--fork" else "--session", resume_path });
    }
    if (cfg.session_id.len > 0) try args.appendSlice(a, &.{ "--session-id", cfg.session_id });
    if (cfg.title.len > 0) try args.appendSlice(a, &.{ "--name", cfg.title });
    if (session_options.models.len > 0) try args.appendSlice(a, &.{ "--models", try std.mem.join(a, ",", session_options.models) });
    if (!pi.bridgeDisabled()) {
        const path = if (pi.provider.options.bridge_path.len > 0) pi.provider.options.bridge_path else try a.dupe(u8, try bridge.localPath(pi.env(), diag));
        pi.token = try std.fmt.allocPrint(a, "agent-cli-bridge-{s}", .{pi.env().uuid().slice()});
        try env_map.map.put(a, "AGENT_CLI_BRIDGE", try bridgeConfig(a, pi.token, pi.cfg));
        try args.appendSlice(a, &.{ "--extension", path });
    }
    for (session_options.extensions) |extension| try args.appendSlice(a, &.{ "--extension", extension });
    if (session_options.no_extensions) try args.append(a, "--no-extensions");
    for (session_options.skills) |skill| try args.appendSlice(a, &.{ "--skill", skill });
    if (session_options.no_skills) try args.append(a, "--no-skills");
    for (session_options.prompt_templates) |template| try args.appendSlice(a, &.{ "--prompt-template", template });
    if (session_options.no_prompt_templates) try args.append(a, "--no-prompt-templates");
    if (session_options.no_context_files) try args.append(a, "--no-context-files");
    if (session_options.tools.len > 0) try args.appendSlice(a, &.{ "--tools", try std.mem.join(a, ",", try toolAllowlist(pi, a)) });
    if (session_options.no_tools) {
        if (cfg.tools.len > 0 or (cfg.mcp_servers != null and cfg.mcp_servers.?.map.count() > 0))
            return errors.fail(diag, .invalid_params, "pi: no_tools removes Config.tools and MCP server tools; list the tools in SessionOptions.tools instead", .{});
        try args.append(a, "--no-tools");
    }
    if (session_options.no_builtin_tools) try args.append(a, "--no-builtin-tools");
    if (cfg.disallowed_tools.len > 0) try args.appendSlice(a, &.{ "--exclude-tools", try std.mem.join(a, ",", cfg.disallowed_tools) });
    if (session_options.no_mcp) try args.append(a, "--no-mcp");
    const dir = kit.firstOf(&.{ session_options.session_dir, pi.provider.options.session_dir });
    if (dir.len > 0) try args.appendSlice(a, &.{ "--session-dir", dir });
    if (session_options.trust_project) |trust| try args.append(a, if (trust) "--approve" else "--no-approve");
    if (session_options.offline) try args.append(a, "--offline");
    try args.appendSlice(a, pi.provider.options.extra_args);
    try args.appendSlice(a, session_options.extra_args);
    return args.toOwnedSlice(a);
}

/// SessionOptions.tools plus the bridge's tools, which Pi would otherwise
/// drop. Pi refuses lists that mix plain names with "+name"/"-name"
/// changes, so added names follow the list's form.
fn toolAllowlist(pi: *PiSession, a: Allocator) ![]const []const u8 {
    var tools: std.ArrayList([]const u8) = .empty;
    try tools.appendSlice(a, pi.opts.tools);
    var changes = false;
    for (pi.opts.tools) |tool| if (std.mem.startsWith(u8, tool, "+") or std.mem.startsWith(u8, tool, "-")) {
        changes = true;
    };
    var extra: std.ArrayList([]const u8) = .empty;
    for (pi.cfg.tools) |tool| try extra.append(a, tool.name);
    if (!pi.bridgeDisabled()) try extra.append(a, decode.output_tool);
    for (extra.items) |tool_name| {
        const entry = if (changes) try std.fmt.allocPrint(a, "+{s}", .{tool_name}) else tool_name;
        if (!kit.contains(tools.items, entry)) try tools.append(a, entry);
    }
    return tools.toOwnedSlice(a);
}

/// The bridge's configuration, as the bridge extension reads it.
const BridgeConfig = struct {
    token: []const u8,
    gate: bool = true,
    tools: []const BridgeTool,
    mcpServers: std.json.ArrayHashMap(BridgeServer),
};

const BridgeTool = struct { name: []const u8, description: []const u8, parameters: RawJson, readOnly: bool };

/// An MCP server as Pi registers it: a URL for http, else a command; fields
/// left null are not written.
const BridgeServer = struct {
    url: ?[]const u8 = null,
    headers: ?types.StringMap = null,
    command: ?[]const u8 = null,
    args: ?[]const []const u8 = null,
    env: ?types.StringMap = null,
    cwd: ?[]const u8 = null,
    enabled: ?bool = null,
    exposure: ?[]const u8 = null,
    toolExposure: ?types.StringMap = null,
    timeout: ?i64 = null,
};

fn bridgeServer(a: Allocator, server: types.McpServer) Allocator.Error!BridgeServer {
    var out: BridgeServer = .{ .enabled = server.enabled };
    if (server.resolvedTransport() == .http) {
        out.url = server.url;
        if (server.headers) |headers| if (headers.map.count() > 0) {
            out.headers = headers;
        };
    } else {
        out.command = server.command;
        if (server.args.len > 0) out.args = server.args;
        if (server.env) |env_map| if (env_map.map.count() > 0) {
            out.env = env_map;
        };
        if (server.cwd.len > 0) out.cwd = server.cwd;
    }
    const exposure: []const u8 = if (server.always_load) "direct" else "codemode"; // codemode is Pi's default
    if (server.enabled_tools.len > 0) {
        out.exposure = "hidden";
        var tool_exposure: types.StringMap = .{};
        for (server.enabled_tools) |tool| try tool_exposure.map.put(a, tool, exposure);
        out.toolExposure = tool_exposure;
    } else if (server.always_load) out.exposure = exposure;
    if (server.tool_timeout_ms > 0) out.timeout = @divFloor(server.tool_timeout_ms + 999, 1000);
    return out;
}

/// The bridge's configuration: token, host tools, and MCP servers.
fn bridgeConfig(a: Allocator, token: []const u8, cfg: types.Config) ![]const u8 {
    const tools = try a.alloc(BridgeTool, cfg.tools.len);
    for (cfg.tools, tools) |tool, *out| out.* = .{
        .name = tool.name,
        .description = tool.description,
        .parameters = tool.input_schema orelse .of("{\"type\":\"object\"}"),
        .readOnly = tool.read_only,
    };
    var servers: std.json.ArrayHashMap(BridgeServer) = .{};
    if (cfg.mcp_servers) |configured| {
        for (configured.map.keys(), configured.map.values()) |server_name, server|
            try servers.map.put(a, server_name, try bridgeServer(a, server));
    }
    const config: BridgeConfig = .{ .token = token, .tools = tools, .mcpServers = servers };
    return (RawJson.encode(a, config) catch return error.OutOfMemory).bytes;
}

test "commands encode as one object with the id and type first" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const no_images: ?[]const Image = null;
    try std.testing.expectEqualStrings("{\"id\":\"r1\",\"type\":\"get_state\"}", try encodeCommand(a, "r1", "get_state", .{}, null));
    try std.testing.expectEqualStrings(
        "{\"id\":\"r1\",\"type\":\"prompt\",\"message\":\"hi\"}",
        try encodeCommand(a, "r1", "prompt", .{ .message = "hi", .images = no_images }, null),
    );
    const params: ?RawJson = .of("{\"id\":\"x\",\"type\":\"y\",\"entryId\":\"e\"}");
    try std.testing.expectEqualStrings("{\"id\":\"r1\",\"type\":\"fork\",\"entryId\":\"e\"}", try encodeCommand(a, "r1", "fork", params, null));
    const none: ?RawJson = null;
    try std.testing.expectEqualStrings("{\"id\":\"r1\",\"type\":\"fork\"}", try encodeCommand(a, "r1", "fork", none, null));
    try std.testing.expectError(error.InvalidParams, encodeCommand(a, "r1", "fork", RawJson.of("[1]"), null));
}

test "bridge config lists host tools and MCP servers" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var servers: std.json.ArrayHashMap(types.McpServer) = .{};
    try servers.map.put(a, "web", .{ .url = "http://x", .enabled = false, .tool_timeout_ms = 1500 });
    try servers.map.put(a, "files", .{ .command = "npx", .args = &.{"files"}, .always_load = true, .enabled_tools = &.{"read"} });
    const tools = [_]types.ToolDef{.{ .name = "lookup", .read_only = true }};
    const config = try bridgeConfig(a, "tok", .{ .tools = &tools, .mcp_servers = servers });
    try std.testing.expectEqualStrings(
        "{\"token\":\"tok\",\"gate\":true,\"tools\":[{\"name\":\"lookup\",\"description\":\"\",\"parameters\":{\"type\":\"object\"},\"readOnly\":true}]," ++
            "\"mcpServers\":{\"web\":{\"url\":\"http://x\",\"enabled\":false,\"timeout\":2}," ++
            "\"files\":{\"command\":\"npx\",\"args\":[\"files\"],\"exposure\":\"hidden\",\"toolExposure\":{\"read\":\"direct\"}}}}",
        config,
    );
}
