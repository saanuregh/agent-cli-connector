//! Rewind: dropping a turn and every later turn, in place with
//! rewind_conversation, or by restarting the CLI on a truncated resume.
const std = @import("std");
const Allocator = std.mem.Allocator;
const agent = @import("agent");
const types = agent.types;
const errors = agent.errors;
const JsonView = agent.json.JsonView;
const Session = agent.Session;

const decode = @import("decode.zig");
const history = @import("history.zig");
const control = @import("control.zig");
const launch_mod = @import("launch.zig");
const session_mod = @import("session.zig");
const Provider = @import("provider.zig").Provider;
const ClaudeSession = session_mod.ClaudeSession;
const RawJson = agent.json.RawJson;
const Diagnostics = agent.Diagnostics;
const kit = agent.providerkit;
const eql = kit.eql;
const name = @import("root.zig").name;

/// Refusals of rewind_conversation after which a restart on a truncated
/// resume can still drop the turn: the target is only in the transcript
/// (such as before a compaction), or the in-place rewind could not be
/// recorded.
const fallback_reasons = [_][]const u8{ "target_not_found", "stale_target", "persist_failed" };

/// Describes a finished rewind.
const Result = struct {
    /// The CLI rewound in place; false means the process was restarted on
    /// a truncated resume.
    native: bool = false,
    /// The prompt UUID of the first dropped turn.
    target_message_id: []const u8 = "",
    /// That prompt's text, for an "edit and resend" UI.
    prefill_text: []const u8 = "",
    preceding_assistant_id: []const u8 = "",
    removed_item_ids: []const []const u8 = &.{},
    /// The session ID after the rewind.
    session_id: []const u8 = "",
};

/// `Session.VTable.rewind`. The session continues under its ID unless a
/// restart at the first turn started a new one; it never forks.
pub fn rewind(s: *Session, turn_id: []const u8, a: Allocator, diag: ?*Diagnostics) errors.Error!types.RewindResult {
    const result = try rewindTurn(ClaudeSession.of(s), a, turn_id, diag);
    return .{ .session_id = result.session_id, .removed_item_ids = result.removed_item_ids };
}

/// Drops `turn_id` (a turn ID, an item ID of this session, or a message
/// UUID from the transcript) and every later turn. It asks the CLI to
/// rewind in place, which keeps the process, its background tasks, and the
/// session ID. When the CLI cannot, it
/// restarts the CLI with --resume, --resume-session-at set to the entry
/// before that turn's prompt, and --resume-drops-turn set to the prompt;
/// rewinding the first turn that way starts a new, empty session. An
/// items_removed event lists the dropped items. File changes are not undone.
pub fn rewindTurn(claude: *ClaudeSession, a: Allocator, turn_id: []const u8, diag: ?*Diagnostics) errors.Error!Result {
    const io = claude.io();
    claude.swap_lock.lockUncancelable(io);
    defer claude.swap_lock.unlock(io);
    const rt = claude.rt();
    try rt.ensureOpen(diag);
    if (rt.busy()) return errors.fail(diag, .invalid_params, "claude: cannot rewind while a turn runs", .{});

    // A live turn is known by its ID or one of its items; anything else is
    // resolved through the transcript, read before the CLI changes it.
    var target = try claude.turn_items.turnOf(io, a, turn_id);
    if (target.len == 0) target = try claude.turn_items.turnOf(io, a, try claude.translator().messageOf(a, turn_id));
    var graph: ?history.Graph = null;
    var historic: []const []const u8 = &.{};
    if (!claude.cfg.ephemeral) {
        const session_id = try claude.sessionId(a);
        if (history.loadTranscript(claude.provider, a, claude.cfg, session_id, diag)) |loaded| {
            graph = loaded;
        } else |e| if (target.len == 0) return e;
        if (graph != null and target.len == 0) {
            const i = try graph.?.promptIndex(a, try claude.translator().messageOf(a, turn_id), diag);
            target = graph.?.entries[i].uuid;
            historic = try graph.?.itemIdsFrom(a, i);
        }
    }
    if (target.len == 0) target = turn_id;

    // The refusal's reason decides whether a restart can still rewind.
    var native_diag: Diagnostics = .{};
    var result = nativeRewind(claude, a, target, &native_diag) catch |e| switch (e) {
        error.Unsupported => try restart(claude, a, graph, target, diag),
        error.Protocol => blk: {
            const info = Diagnostics.of(&native_diag, e);
            if (!kit.contains(&fallback_reasons, info.code)) return forward(diag, &native_diag, e);
            break :blk try restart(claude, a, graph, target, diag);
        },
        else => return forward(diag, &native_diag, e),
    };

    var turns, var ids = try claude.turn_items.drop(io, claude.gpa(), a, target);
    if (turns.len == 0) {
        // A turn from before this process: every live turn came later.
        const first = try claude.turn_items.first(io, a);
        if (first.len > 0) turns, ids = try claude.turn_items.drop(io, claude.gpa(), a, first);
    }
    var all: std.ArrayList([]const u8) = .empty;
    try all.appendSlice(a, ids);
    for (historic) |id| if (!kit.contains(all.items, id)) try all.append(a, id);
    claude.translator().forget(turns, all.items);
    if (all.items.len > 0) claude.emit(.{ .payload = .{ .items_removed = .{ .ids = all.items } } });
    result.removed_item_ids = all.items;
    result.session_id = try claude.sessionId(a);
    return result;
}

/// Copies the details of `err` from `details` into `diag` and returns it.
fn forward(diag: ?*Diagnostics, details: *const Diagnostics, err: errors.Error) errors.Error {
    if (diag) |d| d.record(Diagnostics.of(details, err));
    return err;
}

fn restart(claude: *ClaudeSession, a: Allocator, graph: ?history.Graph, target: []const u8, diag: ?*Diagnostics) errors.Error!Result {
    const loaded = graph orelse return errors.unsupported(diag, name, "rewinding an ephemeral session on a CLI without rewind_conversation");
    return restartRewind(claude, a, loaded, target, diag);
}

/// Sends rewind_conversation for the turn whose prompt is `target`. A
/// refusal fails with `protocol` and the reason as code; a CLI that does
/// not know the request, or answers in an unknown shape, gives
/// `unsupported`.
fn nativeRewind(claude: *ClaudeSession, a: Allocator, target: []const u8, diag: ?*Diagnostics) errors.Error!Result {
    const raw = control.control(claude, a, "rewind_conversation", try RawJson.encode(a, .{ .target_message_uuid = target }), .{}, diag) catch |e| switch (e) {
        error.Protocol => return errors.unsupported(diag, name, "rewind_conversation"),
        else => return e,
    };
    const response = JsonView.parseBytes(a, raw);
    if (response.get("rewound").isNull()) return errors.unsupported(diag, name, "rewind_conversation");
    if (!response.flag("rewound")) {
        return errors.failWith(diag, .{
            .kind = .protocol,
            .code = response.str("reason"),
            .message = try std.fmt.allocPrint(a, "claude: rewind refused: {s}", .{kit.firstOf(&.{ response.str("error"), response.str("reason") })}),
        });
    }
    return .{
        .native = true,
        .target_message_id = kit.firstOf(&.{ response.str("targetMessageUuid"), target }),
        .prefill_text = response.str("prefillText"),
        .preceding_assistant_id = response.str("precedingAssistantUuid"),
    };
}

/// Drops the turn whose prompt is `target` by restarting the CLI on a
/// truncated resume of the transcript.
fn restartRewind(claude: *ClaudeSession, a: Allocator, graph: history.Graph, target: []const u8, diag: ?*Diagnostics) errors.Error!Result {
    const prompt = try graph.promptIndex(a, target, diag);
    var result: Result = .{ .target_message_id = graph.entries[prompt].uuid, .prefill_text = try decode.promptText(a, graph.entries[prompt].message) };
    var resume_at: []const u8 = "";
    if (graph.parent(prompt)) |parent| resume_at = graph.entries[parent].uuid;
    var current = graph.parent(prompt);
    var steps: usize = 0;
    while (current) |i| : (steps += 1) {
        if (steps > graph.entries.len) break;
        const entry = graph.entries[i];
        if (eql(entry.type, "assistant") and !entry.is_sidechain) {
            result.preceding_assistant_id = entry.uuid;
            break;
        }
        current = graph.parent(i);
    }

    var session_id = try claude.sessionId(a);
    retireProcess(claude);
    var cfg = claude.cfg;
    var session_options = claude.opts;
    cfg.fork = false;
    cfg.fork_at_turn = "";
    session_options.@"continue" = false;
    if (resume_at.len == 0) {
        const id = claude.env().uuid();
        session_id = try a.dupe(u8, id.slice());
        cfg.resume_id = "";
        session_options.resume_session_at = "";
        session_options.resume_drops_turn = "";
    } else {
        cfg.resume_id = session_id;
        session_options.resume_session_at = resume_at;
        session_options.resume_drops_turn = result.target_message_id;
    }
    claude.rt().setSessionId(session_id);
    // The failure's code decides whether to retry without the guard.
    var launch_diag: Diagnostics = .{};
    const first = launch_mod.launch(claude, a, cfg, session_options, session_id, true, &launch_diag);
    const init = if (first) |ok| ok else |e| blk: {
        if (!eql(Diagnostics.of(&launch_diag, e).code, launch_mod.drops_turn_rejected_code)) return restartFailed(claude, a, e, &launch_diag, diag);
        // --resume-drops-turn only accepts dropping exactly that turn; later
        // turns make it refuse. Rewinding drops them too, so resume at the
        // same point without the guard.
        retireProcess(claude);
        session_options.resume_drops_turn = "";
        break :blk launch_mod.launch(claude, a, cfg, session_options, session_id, true, &launch_diag) catch |retry_error|
            return restartFailed(claude, a, retry_error, &launch_diag, diag);
    };
    adopt(claude, claude.current().?);

    var info = try claude.rt().sessionInfo(a);
    const init_response = JsonView.parseBytes(a, init.raw);
    info.version = kit.firstOf(&.{ init_response.str("claude_code_version"), info.version });
    info.commands = try control.slashCommands(a, init_response.get("commands"));
    info.raw = .of(init.raw);
    info.id = session_id;
    claude.rt().setInfoFor(info, true, .rewind);
    return result;
}

/// Ends the session after a failed restart and returns the failure, whose
/// details are in `details`.
fn restartFailed(claude: *ClaudeSession, a: Allocator, err: errors.Error, details: *const Diagnostics, diag: ?*Diagnostics) errors.Error {
    var info = Diagnostics.of(details, err);
    info.message = std.fmt.allocPrint(a, "claude: restart after rewind: {s}", .{info.message}) catch info.message;
    const copy = agent.clone(types.ErrorInfo, a, info) catch info;
    claude.rt().finish(copy);
    claude.session.close();
    return errors.failWith(diag, copy);
}

/// Makes a provisional process an ordinary one, so its exit ends the
/// session. It ends the session itself when the process already exited.
fn adopt(claude: *ClaudeSession, launch: *session_mod.Launch) void {
    if (launch.state.cmpxchgStrong(.provisional, .adopted, .acq_rel, .acquire) == null) return;
    if (launch.state.load(.acquire) == .orphaned and !claude.closing.load(.acquire)) {
        launch.translator.failRunningTasks();
        claude.rt().finish(launch.exit_info);
    }
}

/// Retires the latest launch: stops its CLI process, withdraws the pending
/// requests, whose answers it now refuses, and ends its tasks and open
/// items. The next launch's translator keeps its item lookups.
fn retireProcess(claude: *ClaudeSession) void {
    const old = claude.current() orelse return;
    old.retired.store(true, .release);
    old.process.stop(claude.provider.shutdownGraceMs());
    const rt = claude.rt();
    rt.withdrawAll();
    // A turn the CLI started on its own after the idle check dies with the
    // process; nothing else would ever finish it.
    while (true) {
        const turn = rt.currentTurn();
        if (turn.isEmpty()) break;
        rt.abandonTurn(turn.slice(), .{ .kind = .closed, .message = "claude: the CLI process was restarted by rewind" });
    }
    const previous = old.translator;
    previous.stop();
    // The old process's background work dies with it; nothing else would
    // end its tasks and open calls.
    previous.endRunningTasks(.stopped, "the claude process restarted");
    var arena: std.heap.ArenaAllocator = .init(claude.gpa());
    defer arena.deinit();
    previous.endOpenItems(arena.allocator(), .cancelled, false, "") catch
        log.err("claude: out of memory: open items of the restarted process not completed", .{});
}

/// The --resume-session-at value that keeps a persisted session up to and
/// including `turn_id`.
pub fn forkPoint(provider: *Provider, a: Allocator, cfg: types.Config, session_id: []const u8, turn_id: []const u8, diag: ?*Diagnostics) errors.Error![]const u8 {
    const graph = try history.loadTranscript(provider, a, cfg, session_id, diag);
    return graph.forkPoint(a, turn_id, diag);
}

const log = std.log.scoped(.agent);
