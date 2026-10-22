//! Hand checks against the installed claude CLI, skipped unless
//! CLAUDE_LIVE=1. They run small haiku turns, print what they saw, and
//! delete the sessions they made; run with
//! `CLAUDE_LIVE=1 zig build test-claude -Dproviders=claude --summary all`.
const std = @import("std");
const agent = @import("agent");
const claude = @import("root.zig");
const history = @import("history.zig");
const types = agent.types;

const timeout: std.Io.Timeout = .{ .duration = .{ .raw = .fromSeconds(180), .clock = .awake } };

fn textOf(items: []const types.Item) bool {
    for (items) |item| if (std.mem.indexOf(u8, item.text, "giraffe") != null) return true;
    return false;
}

test "live claude fork persists before its first turn" {
    if (comptime @import("builtin").os.tag == .windows) return error.SkipZigTest;
    if (std.testing.environ.getPosix("CLAUDE_LIVE") == null) return error.SkipZigTest;
    const gpa = std.heap.smp_allocator;
    var threaded: std.Io.Threaded = .init(gpa, .{ .environ = std.testing.environ });
    defer threaded.deinit();
    const env: agent.Env = .{ .io = threaded.io(), .gpa = gpa, .environ = std.testing.environ };
    const io = env.io;
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const a = arena.allocator();

    const dir = "/tmp/claude-live-fork-check";
    std.Io.Dir.cwd().createDirPath(io, dir) catch {};
    const home = std.testing.environ.getPosix("HOME").?;
    const project = try std.fs.path.join(a, &.{ home, ".claude", "projects", try history.projectDirName(a, dir) });
    const project_existed = if (std.Io.Dir.cwd().access(io, project, .{})) |_| true else |_| false;

    const provider = try claude.create(env, .{});
    defer provider.deinit();
    var created: std.ArrayList([]const u8) = .empty;
    defer {
        for (created.items) |id| provider.deleteSession(id, dir, null) catch |e| std.debug.print("  delete {s}: {t}\n", .{ id, e });
        // Removes the project folder only when this check made it and left
        // it empty but for the CLI's empty memory folder.
        if (!project_existed) {
            const memory = std.fs.path.join(a, &.{ project, "memory" }) catch project;
            std.Io.Dir.cwd().deleteDir(io, memory) catch {};
            std.Io.Dir.cwd().deleteDir(io, project) catch |e| std.debug.print("  project folder {s} kept: {t}\n", .{ project, e });
        }
    }

    const config: types.Config = .{ .cwd = dir, .model = "haiku", .effort = "low" };
    const session = try provider.start(.{ .config = config }, null);
    defer session.deinit();
    const info = try session.info(a);
    try created.append(a, info.id);
    std.debug.print("started {s} context_window={d}\n", .{ info.id, info.context_window });

    const first = try session.run(.{ .text = "Remember the word zebra. Reply with just OK." }, a, timeout, null);
    std.debug.print("turn 1 {t}: {s}\n", .{ first.status, first.text });
    const second = try session.run(.{ .text = "Also remember the word giraffe. Reply with just OK." }, a, timeout, null);
    std.debug.print("turn 2 {t}: {s}\n", .{ second.status, second.text });

    // A fork closed before any turn stays readable and resumable.
    const fork = try session.fork(.{ .config = .{} }, null);
    const fork_id = try a.dupe(u8, (try fork.info(a)).id);
    try created.append(a, fork_id);
    fork.deinit();
    const fork_items = try provider.readHistory(a, fork_id, .{ .cwd = dir }, null);
    std.debug.print("fork {s}: {d} history items, giraffe={}\n", .{ fork_id, fork_items.len, textOf(fork_items) });
    try std.testing.expect(fork_items.len > 0);

    // A fork at the first turn leaves the second out.
    const at_first = try session.fork(.{ .config = .{ .fork_at_turn = first.turn_id } }, null);
    const at_first_id = try a.dupe(u8, (try at_first.info(a)).id);
    try created.append(a, at_first_id);
    at_first.deinit();
    const at_first_items = try provider.readHistory(a, at_first_id, .{ .cwd = dir }, null);
    std.debug.print("fork at turn 1 {s}: {d} history items, giraffe={}\n", .{ at_first_id, at_first_items.len, textOf(at_first_items) });
    try std.testing.expect(at_first_items.len > 0 and !textOf(at_first_items));

    var resume_config = config;
    resume_config.resume_id = fork_id;
    const resumed = try provider.start(.{ .config = resume_config }, null);
    defer resumed.deinit();
    const answer = try resumed.run(.{ .text = "What was the first word I asked you to remember? Reply with the word only." }, a, timeout, null);
    std.debug.print("resumed fork {t}: {s}\n", .{ answer.status, answer.text });
    var lower: std.ArrayList(u8) = .empty;
    for (answer.text) |c| try lower.append(a, std.ascii.toLower(c));
    try std.testing.expect(std.mem.indexOf(u8, lower.items, "zebra") != null);
}
