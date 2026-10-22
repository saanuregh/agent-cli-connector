//! The provider-independent state of one session: event delivery, turn
//! tracking, the input queue (queue, steer, interrupt), and request
//! bookkeeping. A provider session embeds a `Runtime`, feeds it translated
//! events, and builds its `Session` methods on the helpers below.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const types = @import("types.zig");
const errors = @import("errors.zig");
const clone = @import("clone.zig").clone;
const RawJson = @import("json.zig").RawJson;

const Event = types.Event;
const Diagnostics = errors.Diagnostics;
const ErrorInfo = types.ErrorInfo;
const TurnResult = types.TurnResult;
const log = std.log.scoped(.agent);

/// What the library needs from its host: I/O and a thread-safe allocator.
pub const Env = struct {
    io: Io,
    /// Thread-safe.
    gpa: Allocator,
    /// Inherited by harness processes.
    environ: std.process.Environ,

    pub fn nowMs(env: Env) i64 {
        return Io.Clock.real.now(env.io).toMilliseconds();
    }

    /// A random UUID v4.
    pub fn uuid(env: Env) Id {
        var b: [16]u8 = undefined;
        env.io.random(&b);
        b[6] = (b[6] & 0x0f) | 0x40;
        b[8] = (b[8] & 0x3f) | 0x80;
        var id: Id = .{};
        const hex = std.fmt.bytesToHex(b, .lower);
        id.len = 36;
        @memcpy(id.buf[0..8], hex[0..8]);
        id.buf[8] = '-';
        @memcpy(id.buf[9..13], hex[8..12]);
        id.buf[13] = '-';
        @memcpy(id.buf[14..18], hex[12..16]);
        id.buf[18] = '-';
        @memcpy(id.buf[19..23], hex[16..20]);
        id.buf[23] = '-';
        @memcpy(id.buf[24..36], hex[20..32]);
        return id;
    }
};

/// A short identifier (turn, request) stored inline, so it can be copied out
/// of locked state without allocation.
pub const Id = struct {
    buf: [capacity]u8 = undefined,
    len: u8 = 0,

    /// The longest identifier kept. Longer ones from callers are rejected
    /// (`Session` checks them); longer ones from a harness are cut short.
    pub const capacity = 128;

    pub fn of(s: []const u8) Id {
        var id: Id = .{};
        if (s.len > id.buf.len) log.warn("identifier longer than {d} bytes truncated", .{id.buf.len});
        const n = @min(s.len, id.buf.len);
        @memcpy(id.buf[0..n], s[0..n]);
        id.len = @intCast(n);
        return id;
    }

    pub fn slice(id: *const Id) []const u8 {
        return id.buf[0..id.len];
    }

    pub fn eql(id: *const Id, s: []const u8) bool {
        return std.mem.eql(u8, id.slice(), s);
    }

    pub fn isEmpty(id: Id) bool {
        return id.len == 0;
    }
};

/// An event with the memory it points to.
pub const OwnedEvent = struct {
    arena: std.heap.ArenaAllocator,
    event: Event,

    pub fn create(gpa: Allocator, ev: Event) Allocator.Error!*OwnedEvent {
        const self = try gpa.create(OwnedEvent);
        self.arena = .init(gpa);
        errdefer self.destroy();
        self.event = try clone(Event, self.arena.allocator(), ev);
        return self;
    }

    pub fn destroy(self: *OwnedEvent) void {
        const gpa = self.arena.child_allocator;
        self.arena.deinit();
        gpa.destroy(self);
    }
};

/// Receives events as they are emitted, instead of the session's queue.
/// Runs with the session lock held: it must not block or call the session.
pub const EventSink = struct {
    context: ?*anyopaque = null,
    emit: *const fn (context: ?*anyopaque, ev: *const Event) void,
    /// Called once when the session ended, without the session lock.
    /// `err` is null after a clean close.
    closed: ?*const fn (context: ?*anyopaque, err: ?ErrorInfo) void = null,
};

/// Starts queued input as turn `id`. It returns once the harness accepted or
/// refused the input; an error abandons the turn with the details recorded
/// in `diag`.
pub const Starter = struct {
    context: *anyopaque,
    start: *const fn (context: *anyopaque, id: []const u8, in: types.Input, diag: ?*Diagnostics) errors.Error!void,
};

/// Why `nextEvent` returned no event.
pub const NextEventError = error{
    Timeout,
    /// The session (or turn) ended and every event was delivered.
    EndOfStream,
};

const TurnState = struct {
    id: Id,
    /// The ID `send` returned, kept when the harness renames the turn.
    submitted: Id,
    started_ms: i64,
    arena: std.heap.ArenaAllocator,
    items: std.ArrayList(types.Item) = .empty,
    done: bool = false,
    result: ?TurnResult = null,
    err: ?ErrorInfo = null,
    /// The turn runs inside the library, without a harness run to steer.
    local: bool = false,
    /// The `submitted` ID of the turn this one joined; it ends with that
    /// turn's result.
    follows: Id = .{},

    fn create(gpa: Allocator, id: []const u8, now_ms: i64, opts: TurnOptions) !*TurnState {
        const t = try gpa.create(TurnState);
        t.* = .{ .id = .of(id), .submitted = .of(id), .started_ms = now_ms, .arena = .init(gpa), .local = opts.local };
        return t;
    }

    fn destroy(t: *TurnState, gpa: Allocator) void {
        t.arena.deinit();
        gpa.destroy(t);
    }

    fn matches(t: *const TurnState, id: []const u8) bool {
        return t.id.eql(id) or t.submitted.eql(id);
    }
};

/// A request that waits for an answer.
pub const Pending = struct {
    event: *OwnedEvent,
    kind: types.RequestKind,
    id: Id,
    turn_id: Id,
    /// Provider data needed to answer, such as the native request ID.
    native: []const u8,
    state: enum { pending, answered, withdrawn } = .pending,
    /// An answer is being sent without the lock held: the request stays
    /// allocated until `answer` returns, whatever happens meanwhile.
    answering: bool = false,

    pub fn permission(p: *const Pending) *const types.PermissionRequest {
        return &p.event.event.payload.permission_request;
    }
};

const Queued = struct { id: Id, arena: std.heap.ArenaAllocator, input: types.Input };

/// A result held back because its turn ended before the start in flight
/// was accepted.
const Parked = struct { id: Id, arena: std.heap.ArenaAllocator, result: TurnResult };

/// How a turn is registered.
pub const TurnOptions = struct {
    /// The turn runs inside the library, such as a built-in command, with
    /// no harness run to steer. `isLocal` reports it until the turn ends.
    local: bool = false,
};

const recent_turns = 64;

/// Stack size for the threads of an `Io.Threaded` the library creates
/// itself (the C ABI). Zig's 16 MiB default reserves far more address space
/// than the library's tasks use.
pub const thread_stack_size = 1 << 20;
const max_early_withdrawn = 256;

/// Tasks that end before their owner is freed. Unlike a bare `Io.Group`,
/// starting a task is safe from any thread at any time: once closing, the
/// scope refuses new tasks, so closing never races a start.
pub const TaskScope = struct {
    mutex: Io.Mutex = .init,
    /// Signalled when a task ends or closing finished.
    cond: Io.Condition = .init,
    group: Io.Group = .init,
    /// Tasks and reservations not yet ended.
    running: usize = 0,
    state: enum { open, closing, closed } = .open,

    pub const SpawnError = error{ Closed, ConcurrencyUnavailable };

    /// Runs `function(args...)` as a task of the scope, like
    /// `Io.Group.concurrent`. Fails with `Closed` once the scope closes.
    pub fn concurrent(scope: *TaskScope, io: Io, comptime function: anytype, args: std.meta.ArgsTuple(@TypeOf(function))) SpawnError!void {
        const Args = @TypeOf(args);
        const Task = struct {
            fn run(owner: *TaskScope, task_io: Io, task_args: Args) void {
                defer owner.release(task_io);
                _ = @as(Io.Cancelable!void, @call(.auto, function, task_args)) catch {};
            }
        };
        scope.mutex.lockUncancelable(io);
        defer scope.mutex.unlock(io);
        if (scope.state != .open) return error.Closed;
        scope.running += 1;
        scope.group.concurrent(io, Task.run, .{ scope, io, args }) catch {
            scope.running -= 1;
            return error.ConcurrencyUnavailable;
        };
    }

    /// Counts work that runs outside the scope's tasks, such as on a plain
    /// thread, so closing waits for it. End it with `release`.
    pub fn reserve(scope: *TaskScope, io: Io) error{Closed}!void {
        scope.mutex.lockUncancelable(io);
        defer scope.mutex.unlock(io);
        if (scope.state != .open) return error.Closed;
        scope.running += 1;
    }

    /// Ends work counted by `reserve`.
    pub fn release(scope: *TaskScope, io: Io) void {
        scope.mutex.lockUncancelable(io);
        defer scope.mutex.unlock(io);
        scope.running -= 1;
        if (scope.running == 0) scope.cond.broadcast(io);
    }

    /// Refuses new tasks, cancels the running ones, so their blocked `Io`
    /// calls return `error.Canceled`, and waits until every task and
    /// reservation ended. Work outside the tasks is waited for, not
    /// cancelled. Idempotent; concurrent callers all wait. A task must not
    /// close its own scope.
    pub fn close(scope: *TaskScope, io: Io) void {
        scope.end(io, true);
    }

    /// `close` without cancelling: waits for the tasks to finish.
    pub fn join(scope: *TaskScope, io: Io) void {
        scope.end(io, false);
    }

    fn end(scope: *TaskScope, io: Io, cancel: bool) void {
        scope.mutex.lockUncancelable(io);
        if (scope.state != .open) {
            while (scope.state != .closed) scope.cond.waitUncancelable(io, &scope.mutex);
            scope.mutex.unlock(io);
            return;
        }
        scope.state = .closing;
        scope.mutex.unlock(io);
        // No task starts anymore, so the group is used by this thread only.
        if (cancel) scope.group.cancel(io) else scope.group.await(io) catch {};
        scope.mutex.lockUncancelable(io);
        defer scope.mutex.unlock(io);
        while (scope.running > 0) scope.cond.waitUncancelable(io, &scope.mutex);
        scope.state = .closed;
        scope.cond.broadcast(io);
    }
};

pub const Runtime = struct {
    env: Env,
    provider: []const u8,
    include_raw: bool,
    wire_tap: bool,
    event_limit: u32,
    sink: ?EventSink,

    mutex: Io.Mutex = .init,
    /// Signalled when events are queued, turns end, or the session ends.
    cond: Io.Condition = .init,

    session_id: std.ArrayList(u8) = .empty,
    info_arena: std.heap.ArenaAllocator,
    info: types.SessionInfo = .{},

    events: std.Deque(*OwnedEvent) = .empty,
    dropped: u64 = 0,
    started: bool = false,
    early: std.ArrayList(*OwnedEvent) = .empty,
    last_status: types.SessionStatus = .idle,

    turns: std.ArrayList(*TurnState) = .empty,
    recent: std.ArrayList(*TurnState) = .empty,
    pending: std.ArrayList(*Pending) = .empty,
    early_withdrawn: std.ArrayList(Id) = .empty,

    finished: bool = false,
    end_arena: std.heap.ArenaAllocator,
    end_error: ?ErrorInfo = null,

    /// The runtime's tasks (the input queue, request handlers) and the
    /// provider's (`spawn`). `closeTasks` cancels them; `deinit` waits for
    /// them.
    tasks: TaskScope = .{},
    handled_requests: std.EnumSet(types.RequestKind),
    on_request: ?*const fn (r: *Runtime, request_id: Id) void,

    // The turn queue. One running slot holds the turn the harness works on
    // or whose start is in flight; input sent meanwhile waits in `inputs`
    // and starts on a runtime thread once the slot frees.
    starter: ?Starter = null,
    inputs: std.ArrayList(*Queued) = .empty,
    running: Id = .{},
    starting: std.ArrayList(Id) = .empty,
    /// A steer in flight that may start a run of its own.
    steering: Id = .{},
    /// The turn an interrupt was asked for, until it ends.
    interrupt: Id = .{},
    /// The interrupt was asked while the turn's start was in flight.
    interrupt_pending: bool = false,
    renamed_from: Id = .{},
    renamed_to: Id = .{},
    pumping: bool = false,
    /// Results of turns that ended while a start was in flight, before any
    /// start claimed them; `accepted` delivers them.
    parked: std.ArrayList(*Parked) = .empty,

    pub const Options = struct {
        include_raw: bool = false,
        wire_tap: bool = false,
        event_limit: u32 = 0,
        sink: ?EventSink = null,
        /// Requests of these kinds are not delivered as events; each runs
        /// `on_request` as a runtime task instead.
        handled_requests: std.EnumSet(types.RequestKind) = .empty,
        on_request: ?*const fn (r: *Runtime, request_id: Id) void = null,
    };

    pub fn init(env: Env, provider: []const u8, opts: Options) Runtime {
        return .{
            .env = env,
            .provider = provider,
            .include_raw = opts.include_raw,
            .wire_tap = opts.wire_tap,
            .event_limit = opts.event_limit,
            .sink = opts.sink,
            .handled_requests = opts.handled_requests,
            .on_request = opts.on_request,
            .info_arena = .init(env.gpa),
            .end_arena = .init(env.gpa),
        };
    }

    /// Frees everything. The session must be finished and no thread may use
    /// it anymore.
    pub fn deinit(r: *Runtime) void {
        const gpa = r.env.gpa;
        // The input queue and request handlers may still be returning.
        r.tasks.join(r.env.io);
        while (r.events.popFront()) |ev| ev.destroy();
        r.events.deinit(gpa);
        for (r.early.items) |ev| ev.destroy();
        r.early.deinit(gpa);
        for (r.turns.items) |t| t.destroy(gpa);
        r.turns.deinit(gpa);
        for (r.recent.items) |t| t.destroy(gpa);
        r.recent.deinit(gpa);
        for (r.pending.items) |p| destroyPending(gpa, p);
        r.pending.deinit(gpa);
        for (r.inputs.items) |q| destroyQueued(gpa, q);
        r.inputs.deinit(gpa);
        r.early_withdrawn.deinit(gpa);
        r.starting.deinit(gpa);
        for (r.parked.items) |p| destroyParked(gpa, p);
        r.parked.deinit(gpa);
        r.session_id.deinit(gpa);
        r.info_arena.deinit();
        r.end_arena.deinit();
    }

    fn lock(r: *Runtime) void {
        r.mutex.lockUncancelable(r.env.io);
    }

    fn unlock(r: *Runtime) void {
        r.mutex.unlock(r.env.io);
    }

    fn wake(r: *Runtime) void {
        r.cond.broadcast(r.env.io);
    }

    // ------------------------------------------------------------ identity

    pub fn setSessionId(r: *Runtime, id: []const u8) void {
        r.lock();
        defer r.unlock();
        r.setSessionIdLocked(id);
    }

    fn setSessionIdLocked(r: *Runtime, id: []const u8) void {
        r.session_id.clearRetainingCapacity();
        r.session_id.appendSlice(r.env.gpa, id) catch log.err("out of memory: session ID {s} not recorded", .{id});
    }

    /// Copies the native session ID into `a`.
    pub fn sessionId(r: *Runtime, a: Allocator) Allocator.Error![]u8 {
        r.lock();
        defer r.unlock();
        return a.dupe(u8, r.session_id.items);
    }

    /// Copies the latest session info into `a`.
    pub fn sessionInfo(r: *Runtime, a: Allocator) Allocator.Error!types.SessionInfo {
        r.lock();
        defer r.unlock();
        return clone(types.SessionInfo, a, r.info);
    }

    /// Replaces the session info (copied). With `announce`, emits
    /// `session_updated` when the session has started.
    pub fn setInfo(r: *Runtime, info: types.SessionInfo, announce: bool) void {
        r.lock();
        defer r.unlock();
        r.setInfoLocked(info);
        if (announce) r.announceInfoLocked();
    }

    /// Changes the session info in place: `change` edits a copy, allocating
    /// in the copy's arena, and the copy replaces the info. With
    /// `announce`, emits `session_updated` when the session has started.
    pub fn updateInfo(
        r: *Runtime,
        context: anytype,
        announce: bool,
        comptime change: fn (@TypeOf(context), *types.SessionInfo, Allocator) Allocator.Error!void,
    ) Allocator.Error!void {
        r.lock();
        defer r.unlock();
        var next: std.heap.ArenaAllocator = .init(r.env.gpa);
        errdefer next.deinit();
        var copy = try clone(types.SessionInfo, next.allocator(), r.info);
        try change(context, &copy, next.allocator());
        r.replaceInfoLocked(next, copy);
        if (announce) r.announceInfoLocked();
    }

    fn announceInfoLocked(r: *Runtime) void {
        if (r.started and !r.finished) r.emitLocked(.{ .payload = .{ .session_updated = .{ .info = r.info } } });
    }

    fn setInfoLocked(r: *Runtime, info: types.SessionInfo) void {
        var next: std.heap.ArenaAllocator = .init(r.env.gpa);
        const copy = clone(types.SessionInfo, next.allocator(), info) catch {
            next.deinit();
            log.err("out of memory: session info not updated", .{});
            return;
        };
        r.replaceInfoLocked(next, copy);
    }

    /// Takes `copy`, allocated in `arena`, as the session info.
    fn replaceInfoLocked(r: *Runtime, arena: std.heap.ArenaAllocator, copy: types.SessionInfo) void {
        r.info_arena.deinit();
        r.info_arena = arena;
        r.info = copy;
        if (copy.id.len > 0) r.setSessionIdLocked(copy.id);
    }

    // ------------------------------------------------------------ events

    /// Stamps `ev` with provider, session, time, and the current turn where
    /// missing, records completed items for the turn result, and delivers
    /// it. Events emitted before `started` are held back until then. A
    /// `status_changed` that repeats the current status is dropped. `ev` is
    /// copied; its memory may be freed afterwards.
    pub fn emit(r: *Runtime, ev: Event) void {
        r.lock();
        defer r.unlock();
        if (!r.finished) r.emitLocked(ev);
    }

    /// Shorthand for emitting a payload with an optional native message.
    pub fn emitPayload(r: *Runtime, payload: Event.Payload, raw: ?RawJson) void {
        r.emit(.{ .payload = payload, .raw = raw });
    }

    /// Emits a raw protocol line when wire tapping is on.
    pub fn tap(r: *Runtime, dir: enum { send, receive }, line: []const u8) void {
        if (!r.wire_tap) return;
        r.emit(.{ .payload = .{ .wire = .{ .direction = switch (dir) {
            .send => .send,
            .receive => .receive,
        }, .line = line } } });
    }

    fn emitLocked(r: *Runtime, ev_in: Event) void {
        var ev = ev_in;
        switch (ev.payload) {
            .status_changed => |sc| {
                if (sc.status == r.last_status) return;
                r.last_status = sc.status;
            },
            else => {},
        }
        r.stampLocked(&ev);
        if (r.findTurn(ev.turn_id)) |t| switch (ev.payload) {
            .item_completed => |ic| {
                // A turn result missing an item is better than a lost event.
                if (clone(types.Item, t.arena.allocator(), ic.item)) |item| {
                    t.items.append(t.arena.allocator(), item) catch log.err("out of memory: item {s} left out of the turn result", .{ic.item.id});
                } else |_| log.err("out of memory: item {s} left out of the turn result", .{ic.item.id});
            },
            else => {},
        };
        switch (ev.payload) {
            .items_removed => |rm| for (r.turns.items) |t| {
                var i: usize = 0;
                while (i < t.items.items.len) {
                    if (contains(rm.ids, t.items.items[i].id)) _ = t.items.orderedRemove(i) else i += 1;
                }
            },
            else => {},
        }
        r.deliverLocked(ev);
    }

    fn deliverLocked(r: *Runtime, ev: Event) void {
        if (!r.started) {
            const owned = OwnedEvent.create(r.env.gpa, ev) catch return r.lostLocked();
            r.early.append(r.env.gpa, owned) catch {
                owned.destroy();
                r.lostLocked();
            };
            return;
        }
        r.pushLocked(ev);
    }

    fn pushLocked(r: *Runtime, ev: Event) void {
        if (r.sink) |s| return s.emit(s.context, &ev);
        const owned = OwnedEvent.create(r.env.gpa, ev) catch return r.lostLocked();
        r.pushOwnedLocked(owned);
    }

    fn pushOwnedLocked(r: *Runtime, owned: *OwnedEvent) void {
        if (r.sink) |s| {
            s.emit(s.context, &owned.event);
            owned.destroy();
            return;
        }
        if (r.event_limit > 0 and r.events.len >= r.event_limit) r.dropOldestLocked();
        r.events.pushBack(r.env.gpa, owned) catch {
            owned.destroy();
            return r.lostLocked();
        };
        r.wake();
    }

    /// Counts an event lost to a failed allocation; the reader learns of it
    /// through `events_dropped`.
    fn lostLocked(r: *Runtime) void {
        r.dropped += 1;
        r.wake();
    }

    /// Drops the oldest event that can be lost without harm: requests and
    /// turn outcomes are kept.
    fn dropOldestLocked(r: *Runtime) void {
        var i: usize = 0;
        while (i < r.events.len) : (i += 1) {
            const ev = r.events.at(i);
            switch (ev.event.payload) {
                .permission_request, .user_input_request, .elicitation_request, .turn_completed, .session_started => continue,
                else => {},
            }
            r.removeEventLocked(i).destroy();
            r.dropped += 1;
            return;
        }
    }

    /// Takes queued event `i` out, keeping the order of the rest.
    fn removeEventLocked(r: *Runtime, i: usize) *OwnedEvent {
        if (i == 0) return r.events.popFront().?;
        const ev = r.events.at(i);
        var j = i;
        while (j + 1 < r.events.len) : (j += 1) r.events.atPtr(j).* = r.events.at(j + 1);
        _ = r.events.popBack();
        return ev;
    }

    /// Delivers `session_started` followed by the events emitted during
    /// startup. Call once the session is ready.
    pub fn markStarted(r: *Runtime, info: types.SessionInfo, resumed: bool) void {
        r.lock();
        defer r.unlock();
        if (r.finished or r.started) return;
        r.setInfoLocked(info);
        var ev: Event = .{ .payload = .{ .session_started = .{ .info = r.info, .resumed = resumed } } };
        r.stampLocked(&ev);
        r.started = true;
        r.pushLocked(ev);
        for (r.early.items) |owned| {
            // The session ID is often known only now.
            if (owned.event.session_id.len == 0) owned.event.session_id = owned.arena.allocator().dupe(u8, r.session_id.items) catch "";
            r.pushOwnedLocked(owned);
        }
        r.early.clearRetainingCapacity();
    }

    fn stampLocked(r: *Runtime, ev: *Event) void {
        if (ev.provider.len == 0) ev.provider = r.provider;
        if (ev.session_id.len == 0) ev.session_id = r.session_id.items;
        if (ev.time_ms == 0) ev.time_ms = r.env.nowMs();
        if (!r.include_raw) ev.raw = null;
        if (ev.turn_id.len == 0 and r.turns.items.len > 0) switch (ev.payload) {
            .session_started, .session_updated, .wire => {},
            // The running turn, else the oldest unfinished one: queued
            // turns produce no events.
            else => ev.turn_id = if (!r.running.isEmpty()) r.running.slice() else r.turns.items[0].id.slice(),
        };
        // Callers know turns by the ID `send` returned, even after the
        // harness renamed them.
        if (r.findTurn(ev.turn_id) orelse r.findRecent(ev.turn_id)) |t| ev.turn_id = t.submitted.slice();
    }

    /// The harness's ID for turn `id` (the ID `send` returned, or a native
    /// one), for provider calls that take turn IDs.
    pub fn nativeTurnId(r: *Runtime, id: []const u8) Id {
        r.lock();
        defer r.unlock();
        if (r.findTurn(id) orelse r.findRecent(id)) |t| return t.id;
        return .of(id);
    }

    /// Waits up to `timeout` for the next event. The caller owns the event
    /// and must `destroy` it.
    pub fn nextEvent(r: *Runtime, timeout: Io.Timeout) NextEventError!*OwnedEvent {
        r.lock();
        defer r.unlock();
        const deadline = timeout.toDeadline(r.env.io);
        while (true) {
            if (r.dropped > 0) {
                var ev: Event = .{ .payload = .{ .events_dropped = .{ .count = r.dropped } } };
                r.stampLocked(&ev);
                if (OwnedEvent.create(r.env.gpa, ev)) |owned| {
                    r.dropped = 0;
                    return owned;
                } else |_| {}
            }
            if (r.events.popFront()) |ev| return ev;
            if (r.finished) return error.EndOfStream;
            r.cond.waitTimeout(r.env.io, &r.mutex, deadline) catch return error.Timeout;
        }
    }

    /// Waits up to `timeout` for the next event of turn `turn_id`, leaving
    /// other events queued. Ends once the turn ended and none of its
    /// events is left, including when its `turn_completed` was lost.
    pub fn nextEventOf(r: *Runtime, turn_id: []const u8, timeout: Io.Timeout) NextEventError!*OwnedEvent {
        r.lock();
        defer r.unlock();
        const deadline = timeout.toDeadline(r.env.io);
        while (true) {
            for (0..r.events.len) |i| {
                if (std.mem.eql(u8, r.events.at(i).event.turn_id, turn_id)) return r.removeEventLocked(i);
            }
            if (r.findTurn(turn_id) == null) return error.EndOfStream;
            r.cond.waitTimeout(r.env.io, &r.mutex, deadline) catch return error.Timeout;
        }
    }

    // ------------------------------------------------------------ turns

    fn findTurn(r: *Runtime, id: []const u8) ?*TurnState {
        if (id.len == 0) return null;
        for (r.turns.items) |t| if (t.matches(id)) return t;
        return null;
    }

    fn findRecent(r: *Runtime, id: []const u8) ?*TurnState {
        for (r.recent.items) |t| if (t.matches(id)) return t;
        return null;
    }

    /// Registers a turn the session submitted. Turns are tracked in
    /// submission order; the oldest unfinished turn is the current one.
    pub fn newTurn(r: *Runtime, id: []const u8, diag: ?*Diagnostics) errors.Error!void {
        r.lock();
        defer r.unlock();
        return r.newTurnLocked(id, .{}, diag);
    }

    fn newTurnLocked(r: *Runtime, id: []const u8, opts: TurnOptions, diag: ?*Diagnostics) errors.Error!void {
        if (r.finished) return r.closedErrorLocked(diag);
        if (r.findTurn(id) != null or r.findRecent(id) != null) return;
        const t = try TurnState.create(r.env.gpa, id, r.env.nowMs(), opts);
        r.turns.append(r.env.gpa, t) catch |e| {
            t.destroy(r.env.gpa);
            return e;
        };
    }

    /// Registers a turn the harness started on its own, so its items are
    /// collected.
    pub fn trackTurn(r: *Runtime, id: []const u8) void {
        r.newTurn(id, null) catch |e| if (e != error.Closed) log.err("turn {s} not tracked: {t}", .{ id, e });
    }

    /// The oldest unfinished turn, or empty when idle.
    pub fn currentTurn(r: *Runtime) Id {
        r.lock();
        defer r.unlock();
        return if (r.turns.items.len > 0) r.turns.items[0].id else .{};
    }

    /// Whether turn `id` is unfinished or among the recently finished.
    pub fn knowsTurn(r: *Runtime, id: []const u8) bool {
        r.lock();
        defer r.unlock();
        return r.findTurn(id) != null or r.findRecent(id) != null;
    }

    pub fn hasTurn(r: *Runtime, id: []const u8) bool {
        r.lock();
        defer r.unlock();
        return r.findTurn(id) != null;
    }

    /// Whether unfinished turn `id` runs inside the library (see
    /// `TurnOptions.local`).
    pub fn isLocal(r: *Runtime, id: []const u8) bool {
        r.lock();
        defer r.unlock();
        const t = r.findTurn(id) orelse return false;
        return t.local;
    }

    /// Changes whether unfinished turn `id` runs inside the library, for a
    /// provider that decides only when the turn starts.
    pub fn setLocal(r: *Runtime, id: []const u8, local: bool) void {
        r.lock();
        defer r.unlock();
        if (r.findTurn(id)) |t| t.local = local;
    }

    /// Whether any turn is unfinished.
    pub fn busy(r: *Runtime) bool {
        r.lock();
        defer r.unlock();
        return r.turns.items.len > 0;
    }

    /// Completes a turn: fills the result's turn ID, items, and duration
    /// when missing, emits `turn_completed`, and wakes waiters. An empty
    /// `id` means the current turn. An untracked turn still produces the
    /// event, except while a start is in flight: the result is then held
    /// until `accepted`, since the start's turn may be it. Finishing the
    /// turn in the running slot frees it, and queued input starts. Turns
    /// that joined it (`joinTurn`) end with its outcome (see
    /// `followerResult`). `res` is copied.
    pub fn finishTurn(r: *Runtime, id_in: []const u8, res_in: TurnResult) void {
        r.lock();
        defer r.unlock();
        if (r.finished) return;
        r.finishTurnLocked(id_in, res_in);
    }

    fn finishTurnLocked(r: *Runtime, id_in: []const u8, res_in: TurnResult) void {
        var id_buf: Id = .of(id_in);
        if (id_buf.isEmpty() and r.turns.items.len > 0) id_buf = r.turns.items[0].id;
        const id = id_buf.slice();

        const state = r.detachLocked(id);
        if (state == null and id.len > 0) {
            if (r.findRecent(id) != null) return; // already ended
            if (r.starting.items.len > 0) return r.parkLocked(id, res_in);
        }

        var res = res_in;
        if (res.turn_id.len == 0) res.turn_id = id;
        if (state) |t| {
            if (!t.submitted.eql(t.id.slice())) res.native_turn_id = t.id.slice();
            res.turn_id = t.submitted.slice();
            if (res.items.len == 0) res.items = t.items.items;
            if (res.duration_ms == 0) res.duration_ms = r.env.nowMs() - t.started_ms;
        }
        normalizeResult(&res);
        var ev: Event = .{ .turn_id = res.turn_id, .raw = res.raw, .payload = .{ .turn_completed = .{ .result = res } } };
        r.stampLocked(&ev);
        if (state) |t| {
            // The turn's own items already live in its arena: keep them
            // without a second copy.
            const own_items = res.items.ptr == t.items.items.ptr;
            var kept = res;
            if (own_items) kept.items = &.{};
            t.result = clone(TurnResult, t.arena.allocator(), kept) catch null;
            if (own_items) if (t.result) |*stored| {
                stored.items = t.items.items;
            };
            t.done = true;
        }
        r.pushLocked(ev);
        if (state) |t| {
            // `res` points into `t`, which stays allocated until remembered.
            r.finishFollowersLocked(t, res);
            r.rememberLocked(t);
        }
        r.wake();
    }

    /// Ends the turns that joined `t` with a copy of `res`, its result,
    /// under their own IDs.
    fn finishFollowersLocked(r: *Runtime, t: *const TurnState, res: TurnResult) void {
        var i: usize = 0;
        while (i < r.turns.items.len) {
            const f = r.turns.items[i];
            if (!f.follows.eql(t.submitted.slice())) {
                i += 1;
                continue;
            }
            r.finishTurnLocked(f.submitted.slice(), followerResult(res));
        }
    }

    fn parkLocked(r: *Runtime, id: []const u8, res: TurnResult) void {
        const gpa = r.env.gpa;
        if (r.takeParkedLocked(id)) |old| destroyParked(gpa, old);
        const p = gpa.create(Parked) catch return log.err("out of memory: the result of turn {s} lost", .{id});
        p.* = .{ .id = .of(id), .arena = .init(gpa), .result = undefined };
        p.result = clone(TurnResult, p.arena.allocator(), res) catch {
            destroyParked(gpa, p);
            return log.err("out of memory: the result of turn {s} lost", .{id});
        };
        r.parked.append(gpa, p) catch {
            destroyParked(gpa, p);
            return log.err("out of memory: the result of turn {s} lost", .{id});
        };
    }

    fn takeParkedLocked(r: *Runtime, id: []const u8) ?*Parked {
        for (r.parked.items, 0..) |p, i| if (p.id.eql(id)) return r.parked.orderedRemove(i);
        return null;
    }

    /// Registers turn `id`, if needed, as joining turn `into`: steered input
    /// that the harness merged into the running turn. `id` ends with
    /// `into`'s outcome, at once when `into` already ended. Fails with
    /// `not_found` when `into` is unknown or ended too long ago.
    pub fn joinTurn(r: *Runtime, id: []const u8, into: []const u8, diag: ?*Diagnostics) errors.Error!void {
        r.lock();
        defer r.unlock();
        if (r.finished) return r.closedErrorLocked(diag);
        if (r.findTurn(into)) |target| {
            const follows = target.submitted;
            try r.newTurnLocked(id, .{}, diag);
            if (r.findTurn(id)) |f| f.follows = follows;
            return;
        }
        const target = r.findRecent(into) orelse
            return errors.fail(diag, .not_found, "turn {s} is unknown or ended too long ago", .{into});
        try r.newTurnLocked(id, .{}, diag);
        if (r.findTurn(id) == null) return;
        if (target.result) |res| {
            r.finishTurnLocked(id, followerResult(res));
        } else r.abandonLocked(id, target.err orelse .{ .kind = .closed });
    }

    /// Completes a tracked turn that never ran, such as input the harness
    /// rejected: `waitTurn` fails with `err`, and a failed `turn_completed`
    /// carrying `err` is emitted so per-turn event streams end.
    pub fn abandonTurn(r: *Runtime, id: []const u8, err: ErrorInfo) void {
        r.lock();
        defer r.unlock();
        r.abandonLocked(id, err);
    }

    fn abandonLocked(r: *Runtime, id: []const u8, err: ErrorInfo) void {
        const t = r.detachLocked(id) orelse return;
        t.err = clone(ErrorInfo, t.arena.allocator(), err) catch .{ .kind = err.kind };
        t.done = true;
        if (r.started) {
            var ev: Event = .{ .turn_id = t.submitted.slice(), .payload = .{ .turn_completed = .{ .result = .{
                .turn_id = t.submitted.slice(),
                .status = .failed,
                .@"error" = t.err,
            } } } };
            r.stampLocked(&ev);
            r.pushLocked(ev);
        }
        // Turns that joined it fail with it.
        var i: usize = 0;
        while (i < r.turns.items.len) {
            const f = r.turns.items[i];
            if (f.follows.eql(t.submitted.slice())) r.abandonLocked(f.submitted.slice(), t.err.?) else i += 1;
        }
        r.rememberLocked(t);
        r.wake();
    }

    /// Takes turn `id` out of the unfinished turns and frees the running
    /// slot and interrupt it holds, under any of its names.
    fn detachLocked(r: *Runtime, id: []const u8) ?*TurnState {
        r.releaseLocked(id);
        const t = r.removeTurnLocked(id) orelse return null;
        // The harness may have renamed the turn the slot still names.
        r.releaseLocked(t.id.slice());
        r.releaseLocked(t.submitted.slice());
        return t;
    }

    fn removeTurnLocked(r: *Runtime, id: []const u8) ?*TurnState {
        for (r.turns.items, 0..) |t, i| if (t.matches(id)) {
            _ = r.turns.orderedRemove(i);
            return t;
        };
        return null;
    }

    fn rememberLocked(r: *Runtime, t: *TurnState) void {
        r.recent.append(r.env.gpa, t) catch return t.destroy(r.env.gpa);
        if (r.recent.items.len > recent_turns) r.recent.orderedRemove(0).destroy(r.env.gpa);
    }

    /// Waits for turn `id` to end and copies its result into `a`. A turn
    /// that failed or was interrupted returns its result; check `status`.
    /// A turn that never ran (rejected, cancelled, or the session closed)
    /// fails with its error.
    pub fn waitTurn(r: *Runtime, id: []const u8, a: Allocator, timeout: Io.Timeout, diag: ?*Diagnostics) errors.Error!TurnResult {
        r.lock();
        defer r.unlock();
        const deadline = timeout.toDeadline(r.env.io);
        while (true) {
            if (r.findRecent(id)) |t| {
                if (t.result) |res| return clone(TurnResult, a, res);
                return errors.failWith(diag, t.err orelse .{ .kind = .closed });
            }
            if (r.findTurn(id) == null) {
                if (r.finished) return r.closedErrorLocked(diag);
                return errors.fail(diag, .not_found, "turn {s} is unknown or ended too long ago", .{id});
            }
            r.cond.waitTimeout(r.env.io, &r.mutex, deadline) catch
                return errors.fail(diag, .timeout, "timed out waiting for turn {s}", .{id});
        }
    }

    // ------------------------------------------------------------ requests

    /// Registers a request that needs an answer and delivers it as an
    /// event. `payload` is a request payload; `native` is provider data for
    /// answering it (copied). A `request_resolved` event follows once it is
    /// answered or withdrawn.
    pub fn request(r: *Runtime, payload: Event.Payload, raw: ?RawJson, native: []const u8) void {
        const gpa = r.env.gpa;
        r.lock();
        defer r.unlock();
        if (r.finished) return;
        const kind: types.RequestKind, const req_id: []const u8 = switch (payload) {
            .permission_request => |p| .{ .permission, p.id },
            .user_input_request => |p| .{ .user_input, p.id },
            .elicitation_request => |p| .{ .elicitation, p.id },
            else => unreachable,
        };
        for (r.early_withdrawn.items, 0..) |w, i| if (w.eql(req_id)) {
            // The harness withdrew the request before it was registered.
            _ = r.early_withdrawn.swapRemove(i);
            return;
        };
        var ev: Event = .{ .payload = payload, .raw = raw };
        r.stampLocked(&ev);
        // A request that cannot be registered cannot be answered: the
        // harness waits until the turn is interrupted.
        const owned = OwnedEvent.create(gpa, ev) catch return log.err("out of memory: request {s} lost", .{req_id});
        const p = gpa.create(Pending) catch {
            owned.destroy();
            return log.err("out of memory: request {s} lost", .{req_id});
        };
        p.* = .{
            .event = owned,
            .kind = kind,
            .id = .of(req_id),
            .turn_id = .of(ev.turn_id),
            .native = "",
        };
        p.native = gpa.dupe(u8, native) catch {
            destroyPending(gpa, p);
            return log.err("out of memory: request {s} lost", .{req_id});
        };
        r.pending.append(gpa, p) catch {
            destroyPending(gpa, p);
            return log.err("out of memory: request {s} lost", .{req_id});
        };
        if (r.handled_requests.contains(kind)) {
            if (r.tasks.concurrent(r.env.io, runRequestHook, .{ r, p.id })) |_| return else |_| {
                log.warn("request {s} delivered as an event: no task for its handler", .{req_id});
            }
        }
        // Requests are delivered even during startup: the harness may wait
        // for an answer before startup can finish.
        r.pushLocked(owned.event);
    }

    fn runRequestHook(r: *Runtime, request_id: Id) void {
        r.on_request.?(r, request_id);
    }

    /// Copies pending request `id`'s event into `a`; null when it is no
    /// longer pending.
    pub fn pendingRequest(r: *Runtime, a: Allocator, id: []const u8) Allocator.Error!?Event {
        r.lock();
        defer r.unlock();
        for (r.pending.items) |p| if (p.id.eql(id) and p.state == .pending) return try clone(Event, a, p.event.event);
        return null;
    }

    /// Answers request `id` through `answer`, which sends the native reply.
    /// Fails with `request_done` when it was already answered or withdrawn.
    /// `answer` runs without the lock held.
    pub fn answer(
        r: *Runtime,
        id: []const u8,
        context: anytype,
        diag: ?*Diagnostics,
        comptime answerFn: fn (@TypeOf(context), *const Pending, ?*Diagnostics) errors.Error!void,
    ) errors.Error!void {
        r.lock();
        const p = for (r.pending.items) |p| {
            if (p.id.eql(id)) break p;
        } else {
            r.unlock();
            return errors.fail(diag, .request_done, "request {s} is not pending", .{id});
        };
        if (p.state != .pending or p.answering) {
            r.unlock();
            return errors.fail(diag, .request_done, "request {s} was already answered or withdrawn", .{id});
        }
        p.answering = true;
        r.unlock();

        const result = answerFn(context, p, diag);

        r.lock();
        defer r.unlock();
        p.answering = false;
        if (result) |_| {
            if (p.state == .pending) p.state = .answered;
        } else |_| {}
        // A request withdrawn or abandoned meanwhile is resolved now; a
        // failed answer leaves it pending.
        if (p.state != .pending or r.finished) r.resolveLocked(p);
        return result;
    }

    /// Withdraws pending request `id`, for harnesses that cancel requests
    /// themselves. A request withdrawn before it is registered is dropped
    /// when it arrives.
    pub fn withdrawRequest(r: *Runtime, id: []const u8) void {
        r.lock();
        defer r.unlock();
        for (r.pending.items) |p| if (p.id.eql(id)) {
            if (p.state != .pending) return;
            p.state = .withdrawn;
            if (!p.answering) r.resolveLocked(p); // else resolved when the answer returns
            return;
        };
        if (r.early_withdrawn.items.len >= max_early_withdrawn) r.early_withdrawn.clearRetainingCapacity();
        r.early_withdrawn.append(r.env.gpa, .of(id)) catch log.err("out of memory: withdrawal of request {s} not recorded", .{id});
    }

    /// Withdraws every unanswered request.
    pub fn withdrawAll(r: *Runtime) void {
        r.lock();
        defer r.unlock();
        var i: usize = 0;
        while (i < r.pending.items.len) {
            const p = r.pending.items[i];
            if (p.state == .pending and !p.answering) {
                p.state = .withdrawn;
                r.resolveLocked(p);
            } else {
                if (p.state == .pending) p.state = .withdrawn;
                i += 1;
            }
        }
    }

    fn resolveLocked(r: *Runtime, p: *Pending) void {
        const i = std.mem.indexOfScalar(*Pending, r.pending.items, p) orelse return;
        _ = r.pending.orderedRemove(i);
        if (!r.finished) {
            r.emitLocked(.{ .turn_id = p.turn_id.slice(), .payload = .{ .request_resolved = .{
                .request_id = p.id.slice(),
                .kind = p.kind,
                .outcome = if (p.state == .withdrawn) .withdrawn else .answered,
            } } });
        }
        destroyPending(r.env.gpa, p);
    }

    /// Copies the requests still waiting for an answer, in arrival order.
    pub fn pendingRequests(r: *Runtime, a: Allocator) Allocator.Error![]Event {
        r.lock();
        defer r.unlock();
        var out: std.ArrayList(Event) = .empty;
        for (r.pending.items) |p| if (p.state == .pending and !p.answering) {
            try out.append(a, try clone(Event, a, p.event.event));
        };
        return out.toOwnedSlice(a);
    }

    // ------------------------------------------------------------ the end

    /// Ends the session: pending requests are withdrawn, unfinished turns
    /// fail with `err` (or `closed`), and waiters wake. Events already
    /// queued are still delivered. Only the first call has an effect.
    pub fn finish(r: *Runtime, err: ?ErrorInfo) void {
        if (!r.finishLocked(err)) return;
        if (r.sink) |s| if (s.closed) |closed| closed(s.context, r.end_error);
    }

    fn finishLocked(r: *Runtime, err: ?ErrorInfo) bool {
        r.lock();
        defer r.unlock();
        if (r.finished) return false;
        if (err) |e| r.end_error = clone(ErrorInfo, r.end_arena.allocator(), e) catch e;
        const turn_err: ErrorInfo = r.end_error orelse .{ .kind = .closed, .message = "session closed" };
        while (r.inputs.pop()) |q| destroyQueued(r.env.gpa, q);
        while (r.turns.items.len > 0) r.abandonLocked(r.turns.items[0].id.slice(), turn_err);
        // Requests being answered are freed when their answer returns.
        var i: usize = 0;
        while (i < r.pending.items.len) {
            const p = r.pending.items[i];
            if (p.answering) {
                p.state = .withdrawn;
                i += 1;
            } else destroyPending(r.env.gpa, r.pending.orderedRemove(i));
        }
        for (r.early.items) |ev| ev.destroy();
        r.early.clearRetainingCapacity();
        for (r.parked.items) |p| destroyParked(r.env.gpa, p);
        r.parked.clearRetainingCapacity();
        r.finished = true;
        r.wake();
        return true;
    }

    pub fn isFinished(r: *Runtime) bool {
        r.lock();
        defer r.unlock();
        return r.finished;
    }

    /// Why the session ended, copied into `a`; null while it runs or after
    /// a clean close.
    pub fn endError(r: *Runtime, a: Allocator) ?ErrorInfo {
        r.lock();
        defer r.unlock();
        const e = r.end_error orelse return null;
        return clone(ErrorInfo, a, e) catch e;
    }

    /// The error for calls on an ended session.
    pub fn closedError(r: *Runtime, diag: ?*Diagnostics) errors.Error {
        r.lock();
        defer r.unlock();
        return r.closedErrorLocked(diag);
    }

    fn closedErrorLocked(r: *Runtime, diag: ?*Diagnostics) errors.Error {
        if (r.end_error) |e| {
            var info = e;
            info.kind = .closed;
            return errors.failWith(diag, info);
        }
        return errors.fail(diag, .closed, "session closed", .{});
    }

    /// Fails with `closed` when the session ended.
    pub fn ensureOpen(r: *Runtime, diag: ?*Diagnostics) errors.Error!void {
        if (r.isFinished()) return r.closedError(diag);
    }

    // ------------------------------------------------------------ tasks

    /// Runs `function(args...)` as a task of the session. Fails with
    /// `Closed` once `closeTasks` began.
    pub fn spawn(r: *Runtime, comptime function: anytype, args: std.meta.ArgsTuple(@TypeOf(function))) TaskScope.SpawnError!void {
        return r.tasks.concurrent(r.env.io, function, args);
    }

    /// Refuses new tasks, cancels the running ones (blocked `Io` calls
    /// return `error.Canceled`), and waits until all ended. Providers call
    /// it from `close` once nothing they wait for needs a task anymore.
    pub fn closeTasks(r: *Runtime) void {
        r.tasks.close(r.env.io);
    }

    // ------------------------------------------------------------ turn queue

    pub fn setStarter(r: *Runtime, s: Starter) void {
        r.lock();
        defer r.unlock();
        r.starter = s;
    }

    /// No turn runs or starts, no steer that may start a run is in flight,
    /// and no input is queued.
    pub fn idle(r: *Runtime) bool {
        r.lock();
        defer r.unlock();
        return r.idleLocked();
    }

    fn idleLocked(r: *Runtime) bool {
        return r.running.isEmpty() and r.starting.items.len == 0 and r.steering.isEmpty() and r.inputs.items.len == 0;
    }

    /// Starts turn `id` now when the session is idle: takes the running
    /// slot, registers the turn, and runs `start` on the calling thread. A
    /// failed start abandons the turn and returns the error. Returns false,
    /// doing nothing, when the session is busy.
    pub fn startIdle(
        r: *Runtime,
        id: []const u8,
        opts: TurnOptions,
        context: anytype,
        diag: ?*Diagnostics,
        comptime startFn: fn (@TypeOf(context), ?*Diagnostics) errors.Error!void,
    ) errors.Error!bool {
        {
            r.lock();
            defer r.unlock();
            if (!r.finished and !r.idleLocked()) return false;
            try r.reserveLocked(id, opts, diag);
        }
        try r.runStart(id, context, diag, startFn);
        return true;
    }

    /// `startIdle` running the starter given to `setStarter`.
    pub fn startInput(r: *Runtime, id: []const u8, in: types.Input, diag: ?*Diagnostics) errors.Error!bool {
        const starter = blk: {
            r.lock();
            defer r.unlock();
            break :blk r.starter.?;
        };
        const Ctx = struct { starter: Starter, id: []const u8, in: types.Input };
        return r.startIdle(id, .{}, Ctx{ .starter = starter, .id = id, .in = in }, diag, struct {
            fn start(c: Ctx, d: ?*Diagnostics) errors.Error!void {
                return c.starter.start(c.starter.context, c.id, c.in, d);
            }
        }.start);
    }

    /// Registers turn `id` and queues `in` (copied) to run once the running
    /// slot is free.
    pub fn enqueue(r: *Runtime, id: []const u8, in: types.Input, opts: TurnOptions, diag: ?*Diagnostics) errors.Error!void {
        r.lock();
        defer r.unlock();
        try r.newTurnLocked(id, opts, diag);
        const q = try r.env.gpa.create(Queued);
        q.* = .{ .id = .of(id), .arena = .init(r.env.gpa), .input = undefined };
        q.input = clone(types.Input, q.arena.allocator(), in) catch |e| {
            destroyQueued(r.env.gpa, q);
            return e;
        };
        r.inputs.append(r.env.gpa, q) catch |e| {
            destroyQueued(r.env.gpa, q);
            return e;
        };
        r.kickLocked();
    }

    /// Submits input as turn `id`: starts it when the session is idle, and
    /// otherwise delivers it by `in.delivery`. Queue input waits for the
    /// running slot; interrupt input is queued and the running turn
    /// interrupted; steer input goes into the running turn when one runs
    /// with its start done and the provider can steer it, else it is
    /// queued.
    ///
    /// `hooks` is a value whose type declares:
    /// - `fn start(hooks, id, in, diag) Error!void`: starts input as turn
    ///   `id`, which holds the running slot (see `startIdle`);
    /// - `fn steer(hooks, id, in, running, diag) Error!void`: injects input
    ///   into running turn `running`;
    /// - `fn interrupt(hooks, running, diag) Error!void`: interrupts
    ///   running turn `running`;
    /// - optionally `fn canSteer(hooks, running, in) bool`: whether input
    ///   can steer running turn `running` (true when missing);
    /// - optionally a field `local: bool`, registering the turn as local.
    pub fn submit(r: *Runtime, id: []const u8, in: types.Input, diag: ?*Diagnostics, hooks: anytype) errors.Error!void {
        const Hooks = @TypeOf(hooks);
        const opts: TurnOptions = .{ .local = if (@hasField(Hooks, "local")) hooks.local else false };
        const Start = struct {
            hooks: Hooks,
            id: []const u8,
            in: types.Input,
            fn run(s: @This(), d: ?*Diagnostics) errors.Error!void {
                return s.hooks.start(s.id, s.in, d);
            }
        };
        if (try r.startIdle(id, opts, Start{ .hooks = hooks, .id = id, .in = in }, diag, Start.run)) return;
        const running, const starting = r.runningTurn();
        switch (in.delivery) {
            .steer => {
                const can_steer = if (@hasDecl(Hooks, "canSteer")) hooks.canSteer(running.slice(), in) else true;
                if (running.isEmpty() or starting or !can_steer) return r.enqueue(id, in, opts, diag);
                return hooks.steer(id, in, running.slice(), diag);
            },
            .interrupt => {
                try r.enqueue(id, in, opts, diag);
                if (running.isEmpty()) return;
                // The input is queued and runs either way: a failed interrupt
                // only delays it, so it is reported, not returned.
                var interrupt_diag: Diagnostics = .{};
                hooks.interrupt(running.slice(), &interrupt_diag) catch |e| {
                    const info = Diagnostics.of(&interrupt_diag, e);
                    r.emitPayload(.{ .notice = .{ .level = .warning, .code = "interrupt_failed", .message = info.message } }, null);
                };
            },
            .queue => return r.enqueue(id, in, opts, diag),
        }
    }

    /// Drops all queued input and fails its turns with `err`.
    pub fn abandonQueue(r: *Runtime, err: ErrorInfo) void {
        r.lock();
        defer r.unlock();
        r.abandonQueueLocked(err);
    }

    fn abandonQueueLocked(r: *Runtime, err: ErrorInfo) void {
        while (r.inputs.items.len > 0) {
            const q = r.inputs.orderedRemove(0);
            r.abandonLocked(q.id.slice(), err);
            destroyQueued(r.env.gpa, q);
        }
    }

    /// Drops queued input `id` and fails its turn with `cancelled`.
    /// Returns false when `id` is not queued.
    pub fn cancelQueued(r: *Runtime, id: []const u8) bool {
        r.lock();
        defer r.unlock();
        for (r.inputs.items, 0..) |q, i| if (q.id.eql(id)) {
            _ = r.inputs.orderedRemove(i);
            r.abandonLocked(id, .{ .kind = .cancelled, .message = "input cancelled before it ran" });
            destroyQueued(r.env.gpa, q);
            return true;
        };
        return false;
    }

    /// The turn in the running slot (empty when free) and whether a start
    /// is in flight.
    pub fn runningTurn(r: *Runtime) struct { Id, bool } {
        r.lock();
        defer r.unlock();
        return .{ r.running, r.starting.items.len > 0 };
    }

    /// Marks the start of turn `id` done: the harness accepted the input. A
    /// harness that assigns its own turn IDs passes it as `harness_id`, and
    /// the turn takes that ID. A result held back for the turn (see
    /// `finishTurn`) finishes it now; once no start is in flight, results
    /// held back for other turns are delivered first, as turns the session
    /// did not start. Returns whether an interrupt was asked for while the
    /// start was in flight; the provider sends it now.
    pub fn accepted(r: *Runtime, id: []const u8, harness_id: []const u8) bool {
        r.lock();
        defer r.unlock();
        const final = r.acceptLocked(id, harness_id);
        var interrupt = false;
        if (r.interrupt.eql(final.slice())) {
            interrupt = r.interrupt_pending;
            r.interrupt_pending = false;
        }
        r.kickLocked();
        if (r.finished) return interrupt;
        const own = r.takeParkedLocked(final.slice());
        if (r.starting.items.len == 0) while (r.parked.items.len > 0) {
            const other = r.parked.orderedRemove(0);
            r.finishTurnLocked(other.id.slice(), other.result);
            destroyParked(r.env.gpa, other);
        };
        const p = own orelse return interrupt;
        r.finishTurnLocked(final.slice(), p.result);
        destroyParked(r.env.gpa, p);
        // The turn already ended: there is nothing to interrupt.
        return false;
    }

    fn acceptLocked(r: *Runtime, id_in: []const u8, harness_id: []const u8) Id {
        const id: Id = .of(id_in);
        removeId(&r.starting, id.slice());
        if (harness_id.len == 0 or std.mem.eql(u8, harness_id, id.slice())) return id;
        if (r.findTurn(id.slice())) |t| t.id = .of(harness_id);
        if (r.running.eql(id.slice())) r.running = .of(harness_id);
        if (r.interrupt.eql(id.slice())) r.interrupt = .of(harness_id);
        r.renamed_from = id;
        r.renamed_to = .of(harness_id);
        return r.renamed_to;
    }

    /// Reports a run the harness started; `id` is its turn ID, or empty.
    /// When the slot is free, the run takes it as a tracked turn: the steer
    /// in flight, else `id`, else a new ID. When the slot's start is the
    /// only one in flight and `id` is set, the run is that start's turn.
    /// Returns the turn holding the slot and whether a start is in flight.
    pub fn runStarted(r: *Runtime, id: []const u8) struct { Id, bool } {
        r.lock();
        defer r.unlock();
        if (id.len > 0 and r.starting.items.len == 1 and r.starting.items[0].eql(r.running.slice())) {
            _ = r.acceptLocked(r.running.slice(), id);
        }
        if (!r.running.isEmpty() or r.starting.items.len > 0 or r.finished) return .{ r.running, r.starting.items.len > 0 };
        r.running = if (!r.steering.isEmpty()) r.steering else if (id.len > 0) Id.of(id) else r.env.uuid();
        r.steering = .{};
        r.newTurnLocked(r.running.slice(), .{}, null) catch |e| log.err("turn {s} not tracked: {t}", .{ r.running.slice(), e });
        return .{ r.running, false };
    }

    /// Marks steer `id` into turn `current` in flight. Returns false,
    /// marking nothing, unless `current` holds the slot with its start done
    /// and no other steer is in flight; such input should be queued.
    pub fn beginSteer(r: *Runtime, id: []const u8, current: []const u8) bool {
        r.lock();
        defer r.unlock();
        if (current.len == 0 or !r.running.eql(current) or r.starting.items.len > 0 or !r.steering.isEmpty()) return false;
        r.steering = .of(id);
        return true;
    }

    /// Ends steer `id` that did not start a run.
    pub fn endSteer(r: *Runtime, id: []const u8) void {
        r.lock();
        defer r.unlock();
        if (r.steering.eql(id)) {
            r.steering = .{};
            r.kickLocked();
        }
    }

    /// Records an interrupt of the turn in the slot when it is `want`, or of
    /// any turn when `want` is empty. Returns the turn (empty when none) and
    /// whether its start is in flight; such an interrupt is reported by
    /// `accepted`.
    pub fn requestInterrupt(r: *Runtime, want_in: []const u8) struct { Id, bool } {
        r.lock();
        defer r.unlock();
        var want = want_in;
        if (want.len > 0 and r.renamed_from.eql(want)) want = r.renamed_to.slice();
        if (r.running.isEmpty() or (want.len > 0 and !r.running.eql(want))) return .{ .{}, false };
        r.interrupt = r.running;
        r.interrupt_pending = containsId(r.starting.items, r.running.slice());
        return .{ r.interrupt, r.interrupt_pending };
    }

    /// Records an interrupt of tracked turn `id`, or of the current turn
    /// when `id` is empty, for harnesses whose turns do not use the running
    /// slot. Returns the turn (empty when none is tracked). The record ends
    /// with the turn; `interrupting` reports it until then.
    pub fn markInterrupt(r: *Runtime, id: []const u8) Id {
        r.lock();
        defer r.unlock();
        const t = (if (id.len > 0) r.findTurn(id) else if (r.turns.items.len > 0) r.turns.items[0] else null) orelse return .{};
        r.interrupt = t.id;
        r.interrupt_pending = false;
        return t.id;
    }

    /// The turn an interrupt was asked for (empty when none) and whether it
    /// is still pending on the turn's start.
    pub fn interrupting(r: *Runtime) struct { Id, bool } {
        r.lock();
        defer r.unlock();
        return .{ r.interrupt, r.interrupt_pending };
    }

    /// Forgets the interrupt of turn `id`, for an interrupt the harness
    /// refused.
    pub fn clearInterrupt(r: *Runtime, id: []const u8) void {
        r.lock();
        defer r.unlock();
        if (r.interrupt.eql(id)) {
            r.interrupt = .{};
            r.interrupt_pending = false;
        }
    }

    fn reserveLocked(r: *Runtime, id: []const u8, opts: TurnOptions, diag: ?*Diagnostics) errors.Error!void {
        try r.starting.ensureUnusedCapacity(r.env.gpa, 1);
        try r.newTurnLocked(id, opts, diag);
        r.running = .of(id);
        r.starting.appendAssumeCapacity(.of(id));
    }

    fn runStart(
        r: *Runtime,
        id: []const u8,
        context: anytype,
        caller_diag: ?*Diagnostics,
        comptime startFn: fn (@TypeOf(context), ?*Diagnostics) errors.Error!void,
    ) errors.Error!void {
        // The turn records why it failed even when the caller skips details.
        var own: Diagnostics = undefined;
        const diag = caller_diag orelse blk: {
            own = .{};
            break :blk &own;
        };
        // Details of an earlier failure must not be taken for this one's.
        diag.info = null;
        const result = startFn(context, diag);
        const err_info: ?ErrorInfo = if (result) |_| null else |e| Diagnostics.of(diag, e);
        r.lock();
        defer r.unlock();
        if (indexOfId(r.starting.items, id)) |i| {
            _ = r.starting.orderedRemove(i);
            if (err_info == null and r.interrupt.eql(id)) r.interrupt_pending = false;
        }
        r.kickLocked();
        if (err_info) |info| r.abandonLocked(id, info);
        return result;
    }

    fn releaseLocked(r: *Runtime, id: []const u8) void {
        if (id.len == 0) return;
        if (r.interrupt.eql(id)) {
            r.interrupt = .{};
            r.interrupt_pending = false;
        }
        if (r.running.eql(id)) {
            r.running = .{};
            r.kickLocked();
        }
    }

    fn canStartLocked(r: *Runtime) bool {
        return !r.finished and r.starter != null and r.inputs.items.len > 0 and
            r.running.isEmpty() and r.starting.items.len == 0 and r.steering.isEmpty();
    }

    /// Starts the queue task when queued input can start. Starting never
    /// happens on the caller's thread, which may be the harness reader.
    fn kickLocked(r: *Runtime) void {
        if (r.pumping or !r.canStartLocked()) return;
        r.pumping = true;
        r.tasks.concurrent(r.env.io, pump, .{r}) catch |e| {
            r.pumping = false;
            // A closing session keeps its queue; finishing abandons it.
            if (e != error.Closed) r.abandonQueueLocked(.{ .kind = .internal, .message = "cannot start the input queue task" });
        };
    }

    fn pump(r: *Runtime) void {
        while (true) {
            r.lock();
            if (!r.canStartLocked()) {
                r.pumping = false;
                r.wake();
                r.unlock();
                return;
            }
            const q = r.inputs.orderedRemove(0);
            r.starting.append(r.env.gpa, q.id) catch {
                r.abandonLocked(q.id.slice(), .{ .kind = .internal, .message = "out of memory" });
                destroyQueued(r.env.gpa, q);
                r.unlock();
                continue;
            };
            r.running = q.id;
            const starter = r.starter.?;
            r.unlock();
            const Ctx = struct { s: Starter, q: *Queued };
            // A failed start abandons its turn with the details.
            r.runStart(q.id.slice(), Ctx{ .s = starter, .q = q }, null, struct {
                fn start(c: Ctx, diag: ?*Diagnostics) errors.Error!void {
                    return c.s.start(c.s.context, c.q.id.slice(), c.q.input, diag);
                }
            }.start) catch {};
            destroyQueued(r.env.gpa, q);
        }
    }
};

/// Fills the error of a failed or interrupted result that has none.
fn normalizeResult(res: *TurnResult) void {
    if (res.status == .completed or res.@"error" != null) return;
    res.@"error" = .{
        .kind = if (res.status == .interrupted) .interrupted else .turn_failed,
        .code = res.stop_reason,
        .message = if (res.status == .interrupted) "turn interrupted" else "turn failed",
    };
}

fn destroyPending(gpa: Allocator, p: *Pending) void {
    p.event.destroy();
    gpa.free(p.native);
    gpa.destroy(p);
}

fn destroyQueued(gpa: Allocator, q: *Queued) void {
    q.arena.deinit();
    gpa.destroy(q);
}

fn destroyParked(gpa: Allocator, p: *Parked) void {
    p.arena.deinit();
    gpa.destroy(p);
}

/// The result a joining turn ends with: `res` under the joining turn's own
/// IDs, which `finishTurn` fills in.
/// The result of a turn that joined `res`'s turn: its outcome, without the
/// items, usage, and native message that belong to the joined turn, so a
/// client adding up turns counts them once.
fn followerResult(res: TurnResult) TurnResult {
    var copy = res;
    copy.turn_id = "";
    copy.native_turn_id = "";
    copy.items = &.{};
    copy.usage = .{};
    copy.duration_ms = 0;
    copy.raw = null;
    return copy;
}

pub fn contains(list: []const []const u8, s: []const u8) bool {
    for (list) |x| if (std.mem.eql(u8, x, s)) return true;
    return false;
}

pub fn containsId(list: []const Id, s: []const u8) bool {
    return indexOfId(list, s) != null;
}

pub fn indexOfId(list: []const Id, s: []const u8) ?usize {
    for (list, 0..) |*x, i| if (x.eql(s)) return i;
    return null;
}

fn removeId(list: *std.ArrayList(Id), s: []const u8) void {
    var i: usize = 0;
    while (i < list.items.len) {
        if (list.items[i].eql(s)) _ = list.orderedRemove(i) else i += 1;
    }
}

test "a marked interrupt names one turn and ends with it" {
    const gpa = std.testing.allocator;
    var threaded: Io.Threaded = .init(gpa, .{});
    defer threaded.deinit();
    const env: Env = .{ .io = threaded.io(), .gpa = gpa, .environ = .empty };
    var r: Runtime = .init(env, "test", .{});
    defer r.deinit();
    r.markStarted(.{ .id = "s1" }, false);
    try r.newTurn("a", null);
    try r.newTurn("b", null);
    try std.testing.expectEqualStrings("a", r.markInterrupt("").slice());
    try std.testing.expectEqualStrings("a", r.interrupting()[0].slice());
    r.finishTurn("a", .{ .status = .interrupted });
    try std.testing.expect(r.interrupting()[0].isEmpty());
    try std.testing.expect(r.markInterrupt("missing").isEmpty());
    r.finish(null);
}

test "turn lifecycle: events, items, result" {
    const gpa = std.testing.allocator;
    var threaded: Io.Threaded = .init(gpa, .{});
    defer threaded.deinit();
    const env: Env = .{ .io = threaded.io(), .gpa = gpa, .environ = .empty };
    var r: Runtime = .init(env, "test", .{});
    defer r.deinit();

    r.emitPayload(.{ .notice = .{ .message = "early" } }, null);
    r.markStarted(.{ .id = "s1" }, false);
    try r.newTurn("t1", null);
    r.emitPayload(.{ .item_completed = .{ .item = .{ .id = "i1", .kind = .assistant_message, .text = "hello" } } }, null);
    r.finishTurn("t1", .{ .text = "hello" });

    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const res = try r.waitTurn("t1", arena.allocator(), .none, null);
    try std.testing.expectEqualStrings("t1", res.turn_id);
    try std.testing.expectEqual(@as(usize, 1), res.items.len);

    const want = [_]std.meta.Tag(Event.Payload){ .session_started, .notice, .item_completed, .turn_completed };
    for (want) |tag| {
        const ev = try r.nextEvent(.none);
        defer ev.destroy();
        try std.testing.expectEqual(tag, ev.event.kind());
        try std.testing.expectEqualStrings("s1", ev.event.session_id);
    }
    r.finish(null);
    try std.testing.expectError(error.EndOfStream, r.nextEvent(.none));
}

test "a result that arrives before its start is accepted is replayed by accepted" {
    const gpa = std.testing.allocator;
    var threaded: Io.Threaded = .init(gpa, .{});
    defer threaded.deinit();
    const env: Env = .{ .io = threaded.io(), .gpa = gpa, .environ = .empty };
    var r: Runtime = .init(env, "test", .{});
    defer r.deinit();
    r.markStarted(.{ .id = "s1" }, false);

    const Start = struct {
        r: *Runtime,
        fn start(c: @This(), _: ?*Diagnostics) errors.Error!void {
            // The harness ran and finished the turn, and another turn this
            // session did not start, before the start call returned.
            c.r.finishTurn("other", .{ .text = "other" });
            c.r.finishTurn("native-1", .{ .text = "done" });
            if (c.r.accepted("t1", "native-1")) return error.Internal;
        }
    };
    try std.testing.expect(try r.startIdle("t1", .{}, Start{ .r = &r }, null, Start.start));

    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const short: Io.Timeout = .{ .duration = .{ .raw = .fromMilliseconds(200), .clock = .awake } };
    const res = try r.waitTurn("t1", arena.allocator(), short, null);
    try std.testing.expectEqualStrings("t1", res.turn_id);
    try std.testing.expectEqualStrings("native-1", res.native_turn_id);
    try std.testing.expectEqualStrings("done", res.text);
    try std.testing.expect(r.idle());

    // Results of turns the session did not start are delivered first.
    var completed: [2][]const u8 = undefined;
    var n: usize = 0;
    while (r.nextEvent(short)) |ev| {
        defer ev.destroy();
        if (ev.event.payload == .turn_completed) {
            completed[n] = try arena.allocator().dupe(u8, ev.event.turn_id);
            n += 1;
            if (n == completed.len) break;
        }
    } else |e| return e;
    try std.testing.expectEqualStrings("other", completed[0]);
    try std.testing.expectEqualStrings("t1", completed[1]);
}

fn testEnv(threaded: *Io.Threaded) Env {
    return .{ .io = threaded.io(), .gpa = std.testing.allocator, .environ = .empty };
}

test "task scope: close cancels and waits for tasks, then refuses new ones" {
    var threaded: Io.Threaded = .init(std.testing.allocator, .{});
    defer threaded.deinit();
    const io = threaded.io();
    var scope: TaskScope = .{};
    var done: std.atomic.Value(u32) = .init(0);
    const Task = struct {
        fn sleepLong(t_io: Io, counter: *std.atomic.Value(u32)) void {
            // Cancelled by close; without it the test would take a minute.
            t_io.sleep(.fromSeconds(60), .awake) catch {};
            _ = counter.fetchAdd(1, .acq_rel);
        }
    };
    try scope.concurrent(io, Task.sleepLong, .{ io, &done });
    try scope.concurrent(io, Task.sleepLong, .{ io, &done });
    // Work outside the tasks is waited for too.
    try scope.reserve(io);
    const Outside = struct {
        fn run(s: *TaskScope, t_io: Io, counter: *std.atomic.Value(u32)) void {
            t_io.sleep(.fromMilliseconds(20), .awake) catch {};
            _ = counter.fetchAdd(1, .acq_rel);
            s.release(t_io);
        }
    };
    const thread = try std.Thread.spawn(.{}, Outside.run, .{ &scope, io, &done });
    defer thread.join();
    scope.close(io);
    try std.testing.expectEqual(@as(u32, 3), done.load(.acquire));
    try std.testing.expectError(error.Closed, scope.concurrent(io, Task.sleepLong, .{ io, &done }));
    try std.testing.expectError(error.Closed, scope.reserve(io));
    scope.close(io);
    scope.join(io);
}

test "a joining turn ends with the turn it joined, also when that turn already ended" {
    var threaded: Io.Threaded = .init(std.testing.allocator, .{});
    defer threaded.deinit();
    var r: Runtime = .init(testEnv(&threaded), "test", .{});
    defer r.deinit();
    r.markStarted(.{ .id = "s1" }, false);
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();

    try r.newTurn("run", null);
    try r.joinTurn("steer-1", "run", null);
    try std.testing.expect(r.hasTurn("steer-1"));
    r.emitPayload(.{ .item_completed = .{ .item = .{ .id = "i1", .kind = .assistant_message, .text = "hi", .turn_id = "run" } } }, null);
    r.finishTurn("run", .{ .text = "hi", .usage = .{ .output_tokens = 2 } });
    const joined = try r.waitTurn("steer-1", a, .none, null);
    try std.testing.expectEqualStrings("steer-1", joined.turn_id);
    try std.testing.expectEqualStrings("", joined.native_turn_id);
    try std.testing.expectEqualStrings("hi", joined.text);
    // The items and usage belong to the joined turn only.
    try std.testing.expectEqual(@as(usize, 0), joined.items.len);
    try std.testing.expectEqual(@as(i64, 0), joined.usage.output_tokens);

    // The target already ended: the joining turn ends at once.
    try r.joinTurn("steer-2", "run", null);
    try std.testing.expect(!r.hasTurn("steer-2"));
    const late = try r.waitTurn("steer-2", a, .none, null);
    try std.testing.expectEqualStrings("steer-2", late.turn_id);
    try std.testing.expectEqualStrings("hi", late.text);

    // A joining turn fails with a target that never ran.
    try r.newTurn("rejected", null);
    try r.joinTurn("steer-3", "rejected", null);
    r.abandonTurn("rejected", .{ .kind = .protocol, .message = "refused" });
    try std.testing.expectError(error.Protocol, r.waitTurn("steer-3", a, .none, null));

    try std.testing.expectError(error.NotFound, r.joinTurn("steer-4", "unknown", null));
    try std.testing.expect(!r.knowsTurn("steer-4"));
}

/// Delivery hooks that record what `submit` asked for. Starting holds the
/// running slot until `finishTurn`.
const TestDelivery = struct {
    log: *std.ArrayList(u8),
    local: bool = false,
    steerable: bool = true,

    fn record(d: TestDelivery, what: []const u8, id: []const u8) void {
        d.log.print(std.testing.allocator, "{s}:{s} ", .{ what, id }) catch {};
    }
    pub fn start(d: TestDelivery, id: []const u8, _: types.Input, _: ?*Diagnostics) errors.Error!void {
        d.record("start", id);
    }
    pub fn steer(d: TestDelivery, id: []const u8, _: types.Input, running: []const u8, _: ?*Diagnostics) errors.Error!void {
        d.record("steer", id);
        d.record("into", running);
    }
    pub fn interrupt(d: TestDelivery, running: []const u8, _: ?*Diagnostics) errors.Error!void {
        d.record("interrupt", running);
    }
    pub fn canSteer(d: TestDelivery, _: []const u8, _: types.Input) bool {
        return d.steerable;
    }
};

test "submit starts idle input and dispatches the rest by delivery" {
    var threaded: Io.Threaded = .init(std.testing.allocator, .{});
    defer threaded.deinit();
    var r: Runtime = .init(testEnv(&threaded), "test", .{});
    defer r.deinit();
    r.markStarted(.{ .id = "s1" }, false);
    var log_buf: std.ArrayList(u8) = .empty;
    defer log_buf.deinit(std.testing.allocator);
    const d: TestDelivery = .{ .log = &log_buf };

    try r.submit("t1", .{ .text = "a" }, null, d);
    _ = r.accepted("t1", "");
    try r.submit("t2", .{ .text = "b", .delivery = .steer }, null, d);
    try r.submit("t3", .{ .text = "c", .delivery = .queue }, null, d);
    try r.submit("t4", .{ .text = "d", .delivery = .interrupt }, null, d);
    var refusing = d;
    refusing.steerable = false;
    try r.submit("t5", .{ .text = "e", .delivery = .steer }, null, refusing);
    try std.testing.expectEqualStrings("start:t1 steer:t2 into:t1 interrupt:t1 ", log_buf.items);
    // Queue, interrupt, and unsteerable input wait in the queue, in order.
    try std.testing.expectEqual(@as(usize, 3), r.inputs.items.len);
    try std.testing.expectEqualStrings("t3", r.inputs.items[0].id.slice());
    try std.testing.expectEqualStrings("t4", r.inputs.items[1].id.slice());
    try std.testing.expectEqualStrings("t5", r.inputs.items[2].id.slice());
    r.finish(null);
}

test "a local turn keeps its flag while queued and loses it when it ends" {
    var threaded: Io.Threaded = .init(std.testing.allocator, .{});
    defer threaded.deinit();
    var r: Runtime = .init(testEnv(&threaded), "test", .{});
    defer r.deinit();
    r.markStarted(.{ .id = "s1" }, false);
    var log_buf: std.ArrayList(u8) = .empty;
    defer log_buf.deinit(std.testing.allocator);

    try r.submit("run", .{ .text = "a" }, null, TestDelivery{ .log = &log_buf });
    try std.testing.expect(!r.isLocal("run"));
    try r.submit("local", .{ .text = "/compact" }, null, TestDelivery{ .log = &log_buf, .local = true });
    try std.testing.expect(r.isLocal("local"));

    // The queued turn takes the running slot with its flag.
    const NoStart = struct {
        fn start(_: *anyopaque, _: []const u8, _: types.Input, _: ?*Diagnostics) errors.Error!void {}
    };
    var unused: u8 = 0;
    r.setStarter(.{ .context = &unused, .start = NoStart.start });
    _ = r.accepted("run", "");
    r.finishTurn("run", .{});
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const deadline: Io.Timeout = .{ .duration = .{ .raw = .fromSeconds(5), .clock = .awake } };
    r.lock();
    while (!r.running.eql("local")) r.cond.waitTimeout(r.env.io, &r.mutex, deadline.toDeadline(r.env.io)) catch break;
    r.unlock();
    try std.testing.expect(r.runningTurn()[0].eql("local"));
    try std.testing.expect(r.isLocal("local"));
    r.finishTurn("local", .{ .stop_reason = "handled" });
    try std.testing.expect(!r.isLocal("local"));
    _ = try r.waitTurn("local", arena.allocator(), .none, null);
}
