//! The uniform vocabulary shared by every provider: configuration, input,
//! transcript items, events, requests, and results.
//!
//! Every type here is also the wire format of the C ABI and of every binding:
//! `json.stringify` writes fields in snake_case, leaves out fields that hold
//! their default value, and tags unions with a `"type"` field.
const std = @import("std");
const RawJson = @import("json.zig").RawJson;

pub const StringMap = std.json.ArrayHashMap([]const u8);

/// Reasoning effort. Any harness-native string passes through unchanged.
pub const Effort = []const u8;
pub const effort = struct {
    pub const none = "none";
    pub const minimal = "minimal";
    pub const low = "low";
    pub const medium = "medium";
    pub const high = "high";
    pub const xhigh = "xhigh";
    pub const max = "max";
};

/// How tool calls are approved. The named modes map to each harness's closest
/// native setting; any other string passes unchanged to Claude (a native mode)
/// and Codex (a native approval policy), and Pi refuses it.
pub const PermissionMode = []const u8;
pub const permission_mode = struct {
    /// Asks before risky actions.
    pub const default = "default";
    /// Runs file edits without asking.
    pub const accept_edits = "accept_edits";
    /// Plans without changing anything.
    pub const plan = "plan";
    /// Runs every action without asking.
    pub const bypass = "bypass";
    /// Allows reading but not changing anything.
    pub const read_only = "read_only";
    /// Never asks; actions that would ask are denied.
    pub const dont_ask = "dont_ask";
    /// Lets the harness decide each call.
    pub const auto = "auto";
};

// ---------------------------------------------------------------- config

/// Per-session settings shared by every provider. Fields a harness cannot
/// honor are ignored and listed in a `notice` with code "config_ignored", or
/// refused when `strict` is set. Restrictions a harness cannot enforce, such
/// as `disallowed_tools` on Codex, always fail `start`.
pub const Config = struct {
    /// Working directory; empty means this process's.
    cwd: []const u8 = "",
    model: []const u8 = "",
    effort: Effort = "",
    permission_mode: PermissionMode = "",
    /// Replaces the harness system prompt.
    system_prompt: []const u8 = "",
    append_system_prompt: []const u8 = "",
    /// JSON Schema every turn's final answer must match.
    output_schema: ?RawJson = null,
    /// Native session ID to continue.
    resume_id: []const u8 = "",
    /// With `resume_id`, continue in a new session and keep the original.
    fork: bool = false,
    /// With `fork`, keep history only through this turn.
    fork_at_turn: []const u8 = "",
    /// Requested ID for a new session (Claude needs a UUID).
    session_id: []const u8 = "",
    /// Do not persist the session.
    ephemeral: bool = false,
    title: []const u8 = "",
    mcp_servers: ?std.json.ArrayHashMap(McpServer) = null,
    /// Host tools the model can call. Calls go to `StartOptions.tool_handler`
    /// (Zig) or arrive as `tool/call` requests (C ABI).
    tools: []const ToolDef = &.{},
    allowed_tools: []const []const u8 = &.{},
    disallowed_tools: []const []const u8 = &.{},
    additional_directories: []const []const u8 = &.{},
    /// Added to the inherited environment of the harness process.
    env: ?StringMap = null,
    /// Removed from the inherited environment.
    unset_env: []const []const u8 = &.{},
    /// Words placed before the harness executable, such as a sandbox
    /// wrapper: the harness runs as `command_prefix ++ [executable] ++ args`.
    command_prefix: []const []const u8 = &.{},
    /// Caps unread events; the oldest are dropped and `events_dropped`
    /// reports how many. Zero means no limit.
    event_buffer_limit: u32 = 0,
    /// Emit `wire` events with every raw protocol line.
    wire_tap: bool = false,
    /// Keep the native message in each event's `raw`.
    include_raw: bool = false,
    /// Fail `start` instead of ignoring unsupported fields.
    strict: bool = false,
    /// Provider-specific settings, such as Claude's `max_turns`.
    provider_options: ?RawJson = null,
};

pub const McpTransport = enum { stdio, http, sse };

pub const McpServer = struct {
    /// Defaults to http when `url` is set, else stdio.
    transport: ?McpTransport = null,
    command: []const u8 = "",
    args: []const []const u8 = &.{},
    env: ?StringMap = null,
    cwd: []const u8 = "",
    url: []const u8 = "",
    headers: ?StringMap = null,
    enabled: ?bool = null,
    tool_timeout_ms: i64 = 0,
    startup_timeout_ms: i64 = 0,
    always_load: bool = false,
    enabled_tools: []const []const u8 = &.{},
    approved_tools: []const []const u8 = &.{},

    pub fn resolvedTransport(s: McpServer) McpTransport {
        return s.transport orelse if (s.url.len > 0) .http else .stdio;
    }
};

/// A host tool the model can call.
pub const ToolDef = struct {
    name: []const u8,
    description: []const u8 = "",
    input_schema: ?RawJson = null,
    read_only: bool = false,
    /// Ask for permission before each call.
    needs_approval: bool = false,
    title: []const u8 = "",
    destructive: bool = false,
    open_world: bool = false,
    always_load: bool = false,
    search_hint: []const u8 = "",
};

/// Runs host tool calls for every tool of a session, on a library thread.
/// Allocate the result from `arena`; it lives until the result is sent.
pub const ToolHandler = struct {
    context: ?*anyopaque = null,
    call: *const fn (context: ?*anyopaque, arena: std.mem.Allocator, call: ToolCallRequest) ToolResult,
    /// Asks a running call to stop, when the turn is interrupted or the
    /// session closes. Runs on another thread than `call`; `call` should
    /// then return soon, such as with an error result.
    cancel: ?*const fn (context: ?*anyopaque, call_id: []const u8) void = null,
};

pub const ToolCallRequest = struct {
    id: []const u8 = "",
    name: []const u8 = "",
    arguments: ?RawJson = null,
    session_id: []const u8 = "",
};

pub const ToolResult = struct {
    content: []const ToolContent = &.{},
    is_error: bool = false,
    structured: ?RawJson = null,

    /// A text result allocated from `a`.
    pub fn text(a: std.mem.Allocator, s: []const u8) !ToolResult {
        return .{ .content = try a.dupe(ToolContent, &.{.{ .text = s }}) };
    }
    /// An error result allocated from `a`.
    pub fn err(a: std.mem.Allocator, s: []const u8) !ToolResult {
        return .{ .content = try a.dupe(ToolContent, &.{.{ .text = s }}), .is_error = true };
    }
    /// The static error result for when memory runs out.
    pub const out_of_memory: ToolResult = .{ .content = &.{.{ .text = "out of memory" }}, .is_error = true };
    /// Joins the text content with newlines.
    pub fn joinedText(r: ToolResult, a: std.mem.Allocator) ![]u8 {
        var out: std.ArrayList(u8) = .empty;
        for (r.content) |c| if (c.type == .text) {
            if (out.items.len > 0) try out.append(a, '\n');
            try out.appendSlice(a, c.text);
        };
        return out.toOwnedSlice(a);
    }
};

pub const ToolContent = struct {
    type: enum { text, image } = .text,
    text: []const u8 = "",
    /// Base64 image bytes.
    data: []const u8 = "",
    media_type: []const u8 = "",
};

// ---------------------------------------------------------------- input

pub const Delivery = enum {
    /// Run after the running turn.
    queue,
    /// Inject into the running turn.
    steer,
    /// Stop the running turn, then run.
    interrupt,
};

pub const Input = struct {
    /// Text of the message, placed before `parts`. Shorthand for a text part:
    /// `.{ .text = "Explain this repository" }`.
    text: []const u8 = "",
    parts: []const Part = &.{},
    /// Echoed back by harnesses that support it, to match replies.
    client_id: []const u8 = "",
    delivery: Delivery = .queue,
    /// Switch model from this turn on.
    model: []const u8 = "",
    effort: Effort = "",
    /// Overrides `Config.output_schema` for this turn.
    output_schema: ?RawJson = null,
    /// Provider-specific turn settings.
    provider_options: ?RawJson = null,

    /// `text` as a part followed by `parts`.
    pub fn allParts(in: Input, a: std.mem.Allocator) ![]const Part {
        if (in.text.len == 0) return in.parts;
        const out = try a.alloc(Part, in.parts.len + 1);
        out[0] = .{ .text = in.text };
        @memcpy(out[1..], in.parts);
        return out;
    }

    /// Text parts joined by blank lines; files as "@path", skills and
    /// mentions as "/name". For harnesses without structured input.
    pub fn plainText(in: Input, a: std.mem.Allocator) ![]u8 {
        var out: std.ArrayList(u8) = .empty;
        for (try in.allParts(a)) |p| {
            const s: []const u8, const prefix: []const u8 = switch (p.type) {
                .text => .{ p.text, "" },
                .file => .{ p.path, "@" },
                .skill, .mention => .{ p.name, "/" },
                .image => continue,
            };
            if (s.len == 0) continue;
            if (out.items.len > 0) try out.appendSlice(a, "\n\n");
            try out.appendSlice(a, prefix);
            try out.appendSlice(a, s);
        }
        return out.toOwnedSlice(a);
    }
};

pub const Part = struct {
    type: enum { text, image, file, skill, mention } = .text,
    text: []const u8 = "",
    /// Base64 bytes of an inline image or file.
    data: []const u8 = "",
    media_type: []const u8 = "",
    path: []const u8 = "",
    url: []const u8 = "",
    /// Skill or mention name.
    name: []const u8 = "",
};

// ---------------------------------------------------------------- items

pub const ItemKind = enum { user_message, assistant_message, reasoning, tool_call, plan, compaction, @"error", other };
pub const ItemStatus = enum { in_progress, completed, failed, declined, cancelled };

/// One entry of the conversation transcript.
pub const Item = struct {
    id: []const u8 = "",
    kind: ItemKind = .other,
    status: ItemStatus = .completed,
    /// The tool call (such as a subagent's) that produced this item.
    parent_id: []const u8 = "",
    /// The task ID of the subagent that produced this item.
    subagent_id: []const u8 = "",
    text: []const u8 = "",
    tool: ?ToolCall = null,
    turn_id: []const u8 = "",
    time_ms: i64 = 0,
    client_id: []const u8 = "",
    /// Produced by the harness rather than typed by the user, such as a
    /// skill expansion.
    synthetic: bool = false,
    raw: ?RawJson = null,
};

pub const ToolKind = enum { shell, read, edit, search, web, mcp, subagent, todo, question, plan, host, other };

pub const ToolCall = struct {
    name: []const u8 = "",
    kind: ToolKind = .other,
    input: ?RawJson = null,
    output: []const u8 = "",
    output_data: ?RawJson = null,
    is_error: bool = false,
    command: []const u8 = "",
    cwd: []const u8 = "",
    exit_code: ?i32 = null,
    changes: []const FileChange = &.{},
    path: []const u8 = "",
    server: []const u8 = "",
    query: []const u8 = "",
    url: []const u8 = "",
    duration_ms: i64 = 0,
    display_name: []const u8 = "",
    server_display_name: []const u8 = "",
    icon_url: []const u8 = "",
    images: []const Image = &.{},
};

pub const Image = struct {
    media_type: []const u8 = "",
    /// Base64 bytes.
    data: []const u8 = "",
    url: []const u8 = "",
    path: []const u8 = "",
};

pub const FileChange = struct {
    path: []const u8 = "",
    kind: enum { add, delete, update } = .update,
    move_path: []const u8 = "",
    diff: []const u8 = "",
};

pub const PlanStep = struct {
    id: []const u8 = "",
    text: []const u8 = "",
    status: enum { pending, in_progress, completed } = .pending,
};

pub const TaskStatus = enum { running, completed, failed, stopped };
pub const TaskKind = enum { subagent, shell, workflow, other };

/// Background work beside the conversation, such as a subagent.
pub const Task = struct {
    id: []const u8 = "",
    kind: TaskKind = .other,
    native_kind: []const u8 = "",
    status: TaskStatus = .running,
    description: []const u8 = "",
    summary: []const u8 = "",
    output: []const u8 = "",
    @"error": []const u8 = "",
    tool_call_id: []const u8 = "",
    parent_id: []const u8 = "",
    subagent: ?Subagent = null,
    tool_uses: u32 = 0,
    last_tool: []const u8 = "",
    duration_ms: i64 = 0,
    usage: ?Usage = null,
    workflow: ?Workflow = null,
    hidden: bool = false,
};

pub const Subagent = struct {
    type: []const u8 = "",
    name: []const u8 = "",
    prompt: []const u8 = "",
    model: []const u8 = "",
    effort: Effort = "",
    background: bool = false,
    depth: u32 = 0,
};

pub const Workflow = struct {
    phases: []const WorkflowPhase = &.{},
    agents: []const WorkflowAgent = &.{},
};

pub const WorkflowPhase = struct { index: u32 = 0, title: []const u8 = "" };

pub const WorkflowAgent = struct {
    index: u32 = 0,
    phase: u32 = 0,
    label: []const u8 = "",
    state: []const u8 = "",
    agent_id: []const u8 = "",
    model: []const u8 = "",
    attempt: u32 = 0,
    tokens: i64 = 0,
    tool_calls: u32 = 0,
    duration_ms: i64 = 0,
    prompt: []const u8 = "",
    result: []const u8 = "",
};

pub const Usage = struct {
    input_tokens: i64 = 0,
    output_tokens: i64 = 0,
    cache_read_tokens: i64 = 0,
    cache_write_tokens: i64 = 0,
    reasoning_tokens: i64 = 0,
    total_tokens: i64 = 0,
    cost_usd: f64 = 0,
    context_window: i64 = 0,
    context_tokens: i64 = 0,

    pub fn add(u: *Usage, o: Usage) void {
        u.input_tokens += o.input_tokens;
        u.output_tokens += o.output_tokens;
        u.cache_read_tokens += o.cache_read_tokens;
        u.cache_write_tokens += o.cache_write_tokens;
        u.reasoning_tokens += o.reasoning_tokens;
        u.total_tokens += o.total_tokens;
        u.cost_usd += o.cost_usd;
        if (o.context_window != 0) u.context_window = o.context_window;
        if (o.context_tokens != 0) u.context_tokens = o.context_tokens;
    }
};

// ---------------------------------------------------------------- results

pub const TurnStatus = enum { completed, interrupted, failed };

pub const TurnResult = struct {
    /// The ID `send` returned.
    turn_id: []const u8 = "",
    /// The harness's own ID for the turn, when it differs.
    native_turn_id: []const u8 = "",
    status: TurnStatus = .completed,
    /// Final assistant text.
    text: []const u8 = "",
    structured_output: ?RawJson = null,
    usage: Usage = .{},
    duration_ms: i64 = 0,
    steps: u32 = 0,
    stop_reason: []const u8 = "",
    /// Set when status is failed or interrupted.
    @"error": ?ErrorInfo = null,
    items: []const Item = &.{},
    /// Who started the turn when not the host, such as "task_notification".
    origin: []const u8 = "",
    raw: ?RawJson = null,
};

/// Error kinds shared by the Zig API (as error names), the C ABI and every
/// binding (as `ErrorInfo.kind`).
pub const ErrorKind = enum {
    unsupported,
    closed,
    cancelled,
    request_done,
    session_not_found,
    steer_rejected,
    interrupted,
    invalid_answer,
    invalid_params,
    not_found,
    protocol,
    exit,
    turn_failed,
    timeout,
    internal,
};

/// A failure with details. The Zig API returns `error.<kind>` and records
/// the details in the `Diagnostics` passed to the call.
pub const ErrorInfo = struct {
    kind: ErrorKind = .internal,
    message: []const u8 = "",
    /// Native error class, such as "rate_limit" or a JSON-RPC error code name.
    code: []const u8 = "",
    /// Numeric protocol error code.
    rpc_code: i64 = 0,
    http_status: u32 = 0,
    /// A retry may succeed (after `resets_at_ms` when set).
    retryable: bool = false,
    resets_at_ms: i64 = 0,
    /// Harness stderr tail when the process exited.
    stderr: []const u8 = "",
    data: ?RawJson = null,
};

// ---------------------------------------------------------------- session info

/// Every uniform feature as a flag. A false flag means the matching method
/// fails with `unsupported` or the matching config field is ignored.
pub const Capabilities = struct {
    streaming: bool = false,
    reasoning_stream: bool = false,
    images: bool = false,
    steer: bool = false,
    queue: bool = false,
    interrupt: bool = false,
    interrupt_turn: bool = false,
    set_model: bool = false,
    set_effort: bool = false,
    permission_modes: []const PermissionMode = &.{},
    permission_prompt: bool = false,
    user_questions: bool = false,
    elicitation: bool = false,
    host_tools: bool = false,
    mcp_servers: bool = false,
    resume_session: bool = false,
    fork: bool = false,
    fork_at_turn: bool = false,
    rewind: bool = false,
    compact: bool = false,
    set_title: bool = false,
    stop_task: bool = false,
    read_rate_limits: bool = false,
    system_prompt: bool = false,
    /// Per-turn `Input.output_schema`.
    output_schema: bool = false,
    plans: bool = false,
    tasks: bool = false,
    subagents: bool = false,
    cost_reporting: bool = false,
    rate_limits: bool = false,
    hooks: bool = false,
    goals: bool = false,
    /// Tool output streams as `tool_output_delta` events.
    tool_output_stream: bool = false,
    /// Plans stream as `plan_delta` events.
    plan_stream: bool = false,
    /// Rewinding continues under a new session ID (`session_id_changed`
    /// with reason `rewind`).
    rewind_changes_session_id: bool = false,
    /// Subagents send requests of their own (`subagent_id` is set).
    requests_from_subagents: bool = false,
    /// The harness starts turns on its own, such as when a background task
    /// finishes; their `turn_started` has an `origin`.
    background_turns: bool = false,
    /// Steered input stops running tool calls.
    steer_interrupts_tools: bool = false,
    /// The harness sends `dialog_request` events.
    dialogs: bool = false,
    /// The harness sends `native_request` events for requests without a
    /// uniform meaning.
    native_requests: bool = false,
    // Provider-level operations.
    check: bool = false,
    list_models: bool = false,
    list_sessions: bool = false,
    read_history: bool = false,
    delete_session: bool = false,
    list_subagents: bool = false,
    /// `Provider.call` reaches the harness without a session.
    provider_call: bool = false,
};

pub const SessionInfo = struct {
    id: []const u8 = "",
    provider: []const u8 = "",
    cwd: []const u8 = "",
    model: []const u8 = "",
    effort: Effort = "",
    permission_mode: PermissionMode = "",
    title: []const u8 = "",
    version: []const u8 = "",
    tools: []const []const u8 = &.{},
    commands: []const SlashCommand = &.{},
    mcp_servers: []const McpServerStatus = &.{},
    /// The model's context window in tokens; zero when unknown.
    context_window: i64 = 0,
    raw: ?RawJson = null,
};

/// The result of `Session.rewind`.
pub const RewindResult = struct {
    /// The session ID after the rewind.
    session_id: []const u8 = "",
    /// The rewind continued in a new session; the old one is unchanged.
    forked: bool = false,
    /// Transcript items the rewind dropped.
    removed_item_ids: []const []const u8 = &.{},
};

/// A snapshot of a live session, for a client that (re)connects.
pub const SessionState = struct {
    status: SessionStatus = .idle,
    /// The turn the harness works on; empty when none.
    running_turn_id: []const u8 = "",
    /// Input waiting to run, oldest first.
    queued_turn_ids: []const []const u8 = &.{},
    /// Tasks not yet finished.
    tasks: []const Task = &.{},
    /// The request events still waiting for an answer, in arrival order.
    pending_requests: []const Event = &.{},
    /// The latest `usage_updated` usage: the running or last turn's usage so
    /// far, with the context use.
    usage: Usage = .{},
    info: SessionInfo = .{},
    /// A task is unfinished, or the running turn was started by the harness
    /// (its `turn_started` has an `origin`).
    background_work: bool = false,
};

pub const SlashCommand = struct {
    name: []const u8 = "",
    description: []const u8 = "",
    argument_hint: []const u8 = "",
    /// "command", "skill", "prompt", or "extension".
    kind: []const u8 = "command",
    path: []const u8 = "",
    scope: []const u8 = "",
};

pub const McpServerStatus = struct {
    name: []const u8 = "",
    status: []const u8 = "",
    @"error": []const u8 = "",
};

pub const Model = struct {
    id: []const u8 = "",
    display_name: []const u8 = "",
    description: []const u8 = "",
    /// Upstream provider for multi-provider harnesses such as Pi.
    provider: []const u8 = "",
    is_default: bool = false,
    /// For an alias such as Claude's "default", the model it selects.
    resolved_id: []const u8 = "",
    efforts: []const Effort = &.{},
    default_effort: Effort = "",
    context_window: i64 = 0,
    service_tiers: []const []const u8 = &.{},
    default_service_tier: []const u8 = "",
    input_modalities: []const []const u8 = &.{},
    raw: ?RawJson = null,
};

pub const ProviderStatus = struct {
    installed: bool = false,
    version: []const u8 = "",
    auth: ?AuthStatus = null,
    /// What is wrong with the installation, if anything.
    @"error": ?ErrorInfo = null,
};

pub const AuthStatus = struct {
    authenticated: bool = false,
    /// Such as "api_key", "oauth", or "chatgpt".
    method: []const u8 = "",
    email: []const u8 = "",
    plan: []const u8 = "",
    raw: ?RawJson = null,
};

pub const SessionSummary = struct {
    id: []const u8 = "",
    provider: []const u8 = "",
    title: []const u8 = "",
    cwd: []const u8 = "",
    created_at_ms: i64 = 0,
    updated_at_ms: i64 = 0,
    path: []const u8 = "",
    model: []const u8 = "",
    effort: Effort = "",
    first_prompt: []const u8 = "",
    last_prompt: []const u8 = "",
    raw: ?RawJson = null,
};

pub const ListSessionsOptions = struct {
    cwd: []const u8 = "",
    limit: u32 = 0,
    offset: u32 = 0,
};

pub const ReadHistoryOptions = struct {
    cwd: []const u8 = "",
    /// Read this subagent's transcript (a Task ID).
    subagent_id: []const u8 = "",
    /// Newest `limit` items, in transcript order. Zero means all.
    limit: u32 = 0,
    /// Skip this many of the newest items first.
    offset: u32 = 0,
};

pub const RateLimit = struct {
    /// Such as "five_hour" or "primary".
    name: []const u8 = "",
    used_percent: f64 = 0,
    window_ms: i64 = 0,
    resets_at_ms: i64 = 0,
    status: enum { ok, warning, exceeded } = .ok,
    limit_id: []const u8 = "",
    overage: bool = false,
};

pub const Goal = struct {
    objective: []const u8 = "",
    status: []const u8 = "",
    tokens_used: i64 = 0,
    token_budget: i64 = 0,
    iterations: u32 = 0,
    time_used_ms: i64 = 0,
};

// ---------------------------------------------------------------- requests

pub const PermissionKind = enum { tool, command, file_change, grant, plan_approval };
pub const PermissionDecision = enum { allow, allow_session, allow_always, deny, cancel };

pub const PermissionRequest = struct {
    id: []const u8 = "",
    kind: PermissionKind = .tool,
    tool_name: []const u8 = "",
    tool_call_id: []const u8 = "",
    input: ?RawJson = null,
    title: []const u8 = "",
    description: []const u8 = "",
    reason: []const u8 = "",
    command: []const u8 = "",
    cwd: []const u8 = "",
    changes: []const FileChange = &.{},
    paths: []const []const u8 = &.{},
    /// The plan to approve, for `plan_approval`.
    plan: []const u8 = "",
    /// Decisions the harness offers; empty means allow and deny.
    decisions: []const PermissionDecision = &.{},
    subagent_id: []const u8 = "",
};

pub const PermissionResponse = struct {
    decision: PermissionDecision = .deny,
    message: []const u8 = "",
    /// Replaces the tool input (Claude).
    updated_input: ?RawJson = null,
    /// A native answer sent as is, bypassing the mapping.
    native: ?RawJson = null,
};

pub const QuestionOption = struct {
    label: []const u8 = "",
    description: []const u8 = "",
    preview: []const u8 = "",
};

pub const Question = struct {
    id: []const u8 = "",
    header: []const u8 = "",
    text: []const u8 = "",
    options: []const QuestionOption = &.{},
    multi_select: bool = false,
    allow_other: bool = false,
    secret: bool = false,
    multiline: bool = false,
    placeholder: []const u8 = "",
    default: []const u8 = "",
};

pub const UserInputRequest = struct {
    id: []const u8 = "",
    title: []const u8 = "",
    questions: []const Question = &.{},
    tool_call_id: []const u8 = "",
    /// The harness does not block on the answer.
    is_async: bool = false,
    subagent_id: []const u8 = "",
};

pub const UserInputResponse = struct {
    /// Question ID to selected labels or typed text.
    answers: ?std.json.ArrayHashMap([]const []const u8) = null,
    cancelled: bool = false,
};

pub const ElicitationRequest = struct {
    id: []const u8 = "",
    server: []const u8 = "",
    message: []const u8 = "",
    /// "form" or "url".
    mode: []const u8 = "form",
    schema: ?RawJson = null,
    url: []const u8 = "",
    elicitation_id: []const u8 = "",
    mcp_meta: ?RawJson = null,
    title: []const u8 = "",
    description: []const u8 = "",
    subagent_id: []const u8 = "",
};

pub const ElicitationResponse = struct {
    action: enum { accept, decline, cancel } = .decline,
    content: ?RawJson = null,
    mcp_meta: ?RawJson = null,
};

/// A harness dialog the host shows, such as Claude's `request_user_dialog`.
/// The harness sends only the dialog kinds the host declared.
pub const DialogRequest = struct {
    id: []const u8 = "",
    /// The dialog kind, such as "resume_return".
    kind: []const u8 = "",
    /// The dialog's native fields.
    data: ?RawJson = null,
    tool_use_id: []const u8 = "",
    subagent_id: []const u8 = "",
};

pub const DialogResponse = struct {
    behavior: enum { completed, cancelled } = .cancelled,
    /// The dialog's native answer, with `completed`.
    result: ?RawJson = null,
};

/// A harness request without a uniform meaning, such as a Codex app-server
/// request this library does not know. Answer it with its native result or
/// an error.
pub const NativeRequest = struct {
    id: []const u8 = "",
    method: []const u8 = "",
    params: ?RawJson = null,
    subagent_id: []const u8 = "",
};

pub const NativeResponse = struct {
    result: ?RawJson = null,
    /// Sent instead of `result` when set.
    @"error": ?struct { code: i64 = -32603, message: []const u8 = "" } = null,
};

pub const Response = union(enum) {
    permission: PermissionResponse,
    user_input: UserInputResponse,
    elicitation: ElicitationResponse,
    dialog: DialogResponse,
    native: NativeResponse,
};

pub const RequestKind = enum { permission, user_input, elicitation, dialog, native };

// ---------------------------------------------------------------- events

pub const SessionStatus = enum { idle, running, compacting, waiting };
pub const NoticeLevel = enum { info, warning, @"error" };
pub const HookOutcome = enum { none, succeeded, failed, blocked };
pub const RequestOutcome = enum { pending, answered, withdrawn };
/// Why the native session ID changed.
pub const SessionIdChangedReason = enum { reset, rewind, @"resume" };

/// One event of a session. `payload` decides the `"type"` on the wire.
pub const Event = struct {
    provider: []const u8 = "",
    session_id: []const u8 = "",
    /// Empty for events outside a turn.
    turn_id: []const u8 = "",
    time_ms: i64 = 0,
    /// Native message the event came from (with `Config.include_raw`).
    raw: ?RawJson = null,
    payload: Payload,

    pub const Payload = union(enum) {
        session_started: struct { info: SessionInfo, resumed: bool = false },
        session_updated: struct { info: SessionInfo },
        status_changed: struct { status: SessionStatus },
        turn_started: struct {
            /// The harness's own ID for the turn, when it differs.
            native_turn_id: []const u8 = "",
            /// Who started the turn when not the host, such as
            /// "task_notification".
            origin: []const u8 = "",
            /// The `Input.client_id` the turn runs.
            client_id: []const u8 = "",
        },
        turn_completed: struct { result: TurnResult },
        item_started: struct { item: Item },
        item_updated: struct { item: Item },
        item_completed: struct { item: Item },
        text_delta: Delta,
        reasoning_delta: struct {
            item_id: []const u8 = "",
            parent_id: []const u8 = "",
            subagent_id: []const u8 = "",
            delta: []const u8 = "",
            summary: bool = false,
        },
        tool_input_delta: Delta,
        tool_output_delta: Delta,
        plan_updated: struct {
            steps: []const PlanStep = &.{},
            explanation: []const u8 = "",
            item_id: []const u8 = "",
        },
        plan_delta: Delta,
        usage_updated: struct { usage: Usage },
        task_updated: struct { task: Task },
        compaction: struct {
            done: bool = false,
            trigger: []const u8 = "",
            tokens_before: i64 = 0,
            tokens_after: i64 = 0,
            summary: []const u8 = "",
            @"error": []const u8 = "",
        },
        retry: struct {
            attempt: u32 = 0,
            max_attempts: u32 = 0,
            delay_ms: i64 = 0,
            message: []const u8 = "",
            code: []const u8 = "",
            http_status: u32 = 0,
            done: bool = false,
        },
        notice: struct {
            level: NoticeLevel = .info,
            code: []const u8 = "",
            message: []const u8 = "",
        },
        permission_request: PermissionRequest,
        user_input_request: UserInputRequest,
        elicitation_request: ElicitationRequest,
        dialog_request: DialogRequest,
        native_request: NativeRequest,
        request_resolved: struct {
            request_id: []const u8,
            kind: RequestKind,
            outcome: RequestOutcome,
        },
        rate_limit_updated: struct { limits: []const RateLimit = &.{}, limit_id: []const u8 = "" },
        hook_run: struct {
            id: []const u8 = "",
            event: []const u8 = "",
            name: []const u8 = "",
            done: bool = false,
            status: []const u8 = "",
            outcome: HookOutcome = .none,
            output: []const u8 = "",
        },
        goal_updated: struct { goal: ?Goal = null },
        conversation_reset: struct { trigger: []const u8 = "", new_session_id: []const u8 = "" },
        /// The session continues under a new native ID (the value for
        /// `Config.resume_id`).
        session_id_changed: struct {
            previous_id: []const u8 = "",
            id: []const u8 = "",
            reason: SessionIdChangedReason = .reset,
        },
        items_removed: struct { ids: []const []const u8 = &.{} },
        events_dropped: struct { count: u64 = 0 },
        /// A native message or harness feature without a uniform meaning,
        /// such as "codex/diff_updated" or "claude/prompt_suggestion".
        native: struct { name: []const u8, data: ?RawJson = null },
        /// A raw protocol line, with `Config.wire_tap`.
        wire: struct { direction: enum { send, receive }, line: []const u8 },
    };

    pub const Delta = struct {
        item_id: []const u8 = "",
        parent_id: []const u8 = "",
        subagent_id: []const u8 = "",
        delta: []const u8 = "",
    };

    pub fn kind(ev: Event) std.meta.Tag(Payload) {
        return ev.payload;
    }

    pub fn jsonStringify(ev: Event, jw: anytype) !void {
        try @import("json.zig").writeFlattened(jw, ev, "payload");
    }
};
