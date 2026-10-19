// Package claude drives Claude Code through its stream-json control
// protocol, the same protocol the official TypeScript Agent SDK speaks.
//
// The uniform agent.Session API covers everyday use. *Session adds every
// native control (MCP management, file rewind, context usage, background
// tasks, settings, hooks, and more) plus Control, an escape hatch for
// control requests added to Claude Code after this package was written.
//
// Mapping of agent.Config:
//
//   - Model, Effort, PermissionMode, SystemPrompt, AppendSystemPrompt,
//     Resume, Fork, SessionID, Ephemeral, Title, MCPServers, AllowedTools,
//     DisallowedTools, and AdditionalDirectories map to the matching CLI
//     flags or initialize fields.
//   - OutputSchema becomes --json-schema and applies to every turn.
//     Claude sets the schema per session, so a per-turn
//     agent.Input.OutputSchema is rejected.
//   - ForkAtTurn becomes --resume-session-at, resolved from the session
//     transcript: the last entry of that turn, or the given message.
//   - Tools are served by an in-process MCP server (named "host" unless
//     SessionOptions.ToolServerName says otherwise); the model sees them as
//     "mcp__host__<name>".
//   - PermissionReadOnly has no Claude equivalent and is rejected.
//   - A Tool with NeedsApproval gets the "always_ask" permission policy
//     on the host tool server, so the CLI asks before every call even under
//     an allow rule or bypassPermissions (under dontAsk the call is
//     denied; in auto mode the classifier decides). Other tools are
//     allowed tools.
//   - MCPServer.AlwaysLoad becomes the server's alwaysLoad field, and
//     ApprovedTools become "mcp__<server>__<tool>" allowed tools at launch
//     and, for http and sse servers, the "always_allow" per-tool policy,
//     which SetMCPServers also sends. The CLI reads no per-tool policy for
//     stdio servers, so approvals of a stdio server added with
//     SetMCPServers after launch do not apply; add rules with
//     ApplyFlagSettings instead. EnabledTools is refused, because the CLI has
//     no per-server tool filter and a restriction is never ignored (use
//     DisallowedTools instead).
//   - An empty SystemPrompt keeps Claude Code's own system prompt. The
//     TypeScript SDK instead sends an empty system prompt when none is
//     given; pass SessionOptions.SystemPromptBlocks with one empty block to
//     get that behavior.
//   - An empty PermissionMode leaves the choice to the CLI, which can pick
//     auto mode (a classifier approves calls) on some accounts and
//     providers. Pass agent.PermissionDefault to get a PermissionRequest
//     for every call that needs approval.
//   - UnsetEnv is removed from the process environment together with the
//     variables that mark a parent Claude Code session (CLAUDECODE,
//     CLAUDE_CODE_SSE_PORT, CLAUDE_CODE_ENTRYPOINT,
//     CLAUDE_AGENT_SDK_VERSION; the package sets the last two itself) and,
//     as the TypeScript SDK does, CLAUDE_CODE_AGENT_PROGRESS_SUMMARIES,
//     NODE_OPTIONS, and DEBUG. Config.Env can set any of them again. Pass
//     OpenTelemetry trace context as TRACEPARENT and TRACESTATE in
//     Config.Env.
//
// Behavior worth knowing:
//
//   - Input sent during a turn goes to the CLI's own queue with a message
//     priority: DeliverQueue is "later", DeliverSteer "next", and
//     DeliverInterrupt "now" (TurnOptions.Priority overrides it).
//   - Input.Model and Input.Effort switch the session when Send runs, so
//     they are refused on input queued behind a running turn.
//   - Interrupt stops the running turn; queued messages still run.
//     InterruptTurn removes one queued message (its turn ends with
//     agent.ErrCancelled) or interrupts the running turn.
package claude

import (
	"context"
	"encoding/json"
	"maps"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// ProtocolVersion is the Claude Agent SDK release whose wire protocol this
// package implements. It is sent as CLAUDE_AGENT_SDK_VERSION, which the CLI
// only reports in its User-Agent.
const ProtocolVersion = "0.3.302"

// Options configures the provider.
type Options struct {
	// Path is the claude executable. Empty means "claude" on PATH.
	Path string
	// Spawner starts the process. Nil means agent.LocalSpawner.
	Spawner agent.Spawner
	// Env is added to every session's environment.
	Env map[string]string
	// ExtraArgs are appended to every session's command line.
	ExtraArgs []string
	// StartupTimeout bounds the initialize handshake. Zero means 60s.
	StartupTimeout time.Duration
	// ShutdownGrace is how long Close waits for a clean exit before
	// terminating the process. Zero means 5s.
	ShutdownGrace time.Duration
	// ClientApp identifies the host application in Claude's User-Agent,
	// such as "my-app/1.0.0".
	ClientApp string
}

// SessionOptions holds Claude-specific per-session settings. Pass a
// *SessionOptions in agent.Config.ProviderOptions.
type SessionOptions struct {
	// Agent runs the main thread as the named agent (--agent).
	Agent string
	// Agents defines subagents by name.
	Agents map[string]AgentDefinition
	// BuiltinTools selects the built-in tools. Nil means the default set;
	// an empty non-nil slice disables every built-in tool.
	BuiltinTools []string
	// ToolAliases redirects model-emitted tool names to other tools.
	ToolAliases map[string]string
	// FallbackModel is tried when the main model is unavailable.
	FallbackModel string
	// Thinking controls extended thinking. Nil means the model default.
	Thinking *Thinking
	// MaxTurns stops a turn after this many model round trips.
	MaxTurns int
	// MaxBudgetUSD stops the session after this estimated spend.
	MaxBudgetUSD float64
	// TaskBudgetTokens tells the model its token budget (alpha).
	TaskBudgetTokens int
	// Betas enables API beta features such as "context-1m-2025-08-07".
	Betas []string

	// SystemPromptBlocks replaces the system prompt with these blocks.
	// Use SystemPromptDynamicBoundary to split cacheable and dynamic parts.
	SystemPromptBlocks []string
	// ExcludeDynamicSections strips per-user sections from the default
	// system prompt so it caches across users.
	ExcludeDynamicSections bool
	// SystemPromptSnapshot controls whether the rendered system prompt is
	// recorded and reused for the conversation. Nil means the default.
	SystemPromptSnapshot *bool
	// AppendSubagentSystemPrompt is appended to subagent system prompts.
	AppendSubagentSystemPrompt string
	// PlanModeInstructions replaces the plan-mode workflow instructions.
	PlanModeInstructions string

	// SettingSources selects which settings files load ("user",
	// "project", "local"). Nil loads all; an empty non-nil slice loads
	// none.
	SettingSources []string
	// Settings is a settings object or a path to a settings file, loaded
	// into the flag settings layer.
	Settings any
	// ManagedSettings are policy-tier settings from the host.
	ManagedSettings any
	// Sandbox configures command sandboxing; it is merged into Settings.
	Sandbox map[string]any
	// Plugins loads local plugin directories.
	Plugins []Plugin
	// Skills enables skills by name; ["all"] enables every skill. Nil
	// leaves the CLI default.
	Skills []string
	// StrictMCPConfig ignores MCP servers from settings files and plugins.
	StrictMCPConfig bool
	// MCPViaControl registers agent.Config.MCPServers through the
	// mcp_set_servers control request before Start returns, instead of
	// the --mcp-config flag. Servers are always registered that way when
	// one has Headers, Env, Args, or a URL with credentials or a query
	// string, so those values stay out of the command line, where other
	// local users can read them.
	MCPViaControl bool
	// ProjectConfigRoot reads project configuration from this checkout
	// instead of the working directory.
	ProjectConfigRoot string

	// Hooks registers in-process hook callbacks by event name.
	Hooks map[HookEvent][]HookMatcher
	// OnUserDialog renders CLI dialogs of the kinds in
	// SupportedDialogKinds. Return nil to leave the dialog unanswered.
	OnUserDialog func(ctx context.Context, dialog UserDialog) (json.RawMessage, error)
	// SupportedDialogKinds lists the dialog kinds OnUserDialog renders.
	SupportedDialogKinds []string

	// NoPartialMessages turns off token streaming. Text then arrives per
	// content block instead of as deltas.
	NoPartialMessages bool
	// IncludeHookEvents reports hook lifecycle as HookEvent events.
	IncludeHookEvents bool
	// HideSubagentText stops subagents' text and thinking from being
	// forwarded; their tool calls are still reported.
	HideSubagentText bool
	// PromptSuggestions emits a predicted next prompt after each turn.
	PromptSuggestions bool
	// AgentProgressSummaries adds generated summaries to subagent
	// progress events.
	AgentProgressSummaries bool
	// NoPerTaskStopAffordance stops declaring that the host can stop
	// background tasks one by one (StopTask). Without that declaration an
	// interrupt also kills background subagents and shells.
	NoPerTaskStopAffordance bool
	// TrackSubagentEffort registers observation-only PreToolUse,
	// PostToolUse, and SubagentStop hooks that report the effort each
	// subagent actually runs at in Task.Subagent.Effort. Every tool call
	// then makes one extra hook round trip.
	TrackSubagentEffort bool
	// VerbatimPrompts delivers prompt text as written: no @path expansion
	// and no slash-command dispatch.
	VerbatimPrompts bool
	// TailShellOutput streams the output of foreground Bash calls as
	// agent.ToolOutputDelta events. The CLI otherwise sends no output until
	// the call ends. It polls the CLI's private task output file (Unix
	// only), and the get_task_output control request when that file cannot
	// be found, such as with a remote agent.Spawner; get_task_output
	// returns at most the last 8 KiB, so a gap longer than that between
	// polls is skipped.
	TailShellOutput bool

	// EnableFileCheckpointing records file backups so RewindFiles works.
	EnableFileCheckpointing bool
	// Continue resumes the most recent session in the working directory.
	Continue bool
	// ResumeSessionAt resumes only up to this message UUID.
	ResumeSessionAt string
	// ResumeDropsTurn validates a truncating resume; see the SDK docs.
	ResumeDropsTurn string
	// PermissionPromptTool routes permission prompts to this MCP tool
	// instead of the host. Permission requests are then not produced.
	PermissionPromptTool string
	// NoPermissionPrompts denies anything that would ask, without asking.
	NoPermissionPrompts bool
	// AllowBypassPermissions launches the CLI so that SetPermissionMode can
	// switch to agent.PermissionBypass later. Starting in bypass mode
	// implies it.
	AllowBypassPermissions bool
	// ToolConfig configures built-in tools, such as
	// {"askUserQuestion": {"previewFormat": "html"}}.
	ToolConfig map[string]any

	// ToolServerName names the in-process MCP server serving
	// agent.Config.Tools. Empty means "host".
	ToolServerName string
	// ToolTimeout bounds one call of a host tool (agent.Config.Tools);
	// values under one second are ignored by the CLI. Zero means the CLI
	// default, which is effectively unbounded.
	ToolTimeout time.Duration
	// OnControlRequest answers control requests from the CLI that this
	// package does not handle, by subtype; req is the whole request
	// object. The returned value is the success payload, and an error
	// becomes an error reply. Without a callback such requests get an
	// "unsupported" error reply.
	OnControlRequest func(ctx context.Context, subtype string, req json.RawMessage) (any, error)
	// Debug enables CLI debug logging; DebugFile writes it to a file.
	Debug     bool
	DebugFile string
	// ExtraArgs are appended to the command line.
	ExtraArgs []string
	// InitializeExtra adds fields to the initialize request, for options
	// newer than this package.
	InitializeExtra map[string]any

	// skipAppliedEffort skips reading the applied effort at start, for the
	// short-lived processes of Check and ListModels.
	skipAppliedEffort bool
}

// SystemPromptDynamicBoundary splits a block system prompt into a
// cacheable prefix and a session-specific suffix.
const SystemPromptDynamicBoundary = "__SYSTEM_PROMPT_DYNAMIC_BOUNDARY__"

// Thinking configures extended thinking.
type Thinking struct {
	// Type is "adaptive", "enabled", or "disabled".
	Type string
	// BudgetTokens is the thinking budget for Type "enabled".
	BudgetTokens int
	// Display is "summarized" or "omitted".
	Display string
}

// AgentDefinition defines a subagent.
type AgentDefinition struct {
	Description     string   `json:"description"`
	Prompt          string   `json:"prompt"`
	Tools           []string `json:"tools,omitempty"`
	DisallowedTools []string `json:"disallowedTools,omitempty"`
	Model           string   `json:"model,omitempty"`
	MCPServers      []any    `json:"mcpServers,omitempty"`
	// CriticalSystemReminder is an experimental reminder added to the
	// agent's system prompt.
	CriticalSystemReminder string   `json:"criticalSystemReminder_EXPERIMENTAL,omitempty"`
	Skills                 []string `json:"skills,omitempty"`
	InitialPrompt          string   `json:"initialPrompt,omitempty"`
	MaxTurns               int      `json:"maxTurns,omitempty"`
	// AutoCompactWindow is the token count at which the agent compacts its
	// own conversation when it runs as a subagent; it can only lower the
	// inherited window.
	AutoCompactWindow int    `json:"autoCompactWindow,omitempty"`
	Background        bool   `json:"background,omitempty"`
	OmitClaudeMD      bool   `json:"omitClaudeMd,omitempty"`
	Memory            string `json:"memory,omitempty"`
	// Effort is a named effort level; NumericEffort, when set, sends an
	// integer effort instead.
	Effort         string `json:"effort,omitempty"`
	NumericEffort  int    `json:"-"`
	PermissionMode string `json:"permissionMode,omitempty"`
	// Observer names an agent type started as a read-only background
	// observer whenever this agent runs; ObserverMessage is appended to
	// each activity digest it receives.
	Observer        string         `json:"observer,omitempty"`
	ObserverMessage string         `json:"observerMessage,omitempty"`
	Extra           map[string]any `json:"-"`
}

// MarshalJSON includes NumericEffort and Extra fields.
func (a AgentDefinition) MarshalJSON() ([]byte, error) {
	type plain AgentDefinition
	extra := a.Extra
	if a.NumericEffort != 0 {
		extra = maps.Clone(extra)
		if extra == nil {
			extra = map[string]any{}
		}
		if _, ok := extra["effort"]; !ok {
			extra["effort"] = a.NumericEffort
		}
	}
	return mergeJSON(plain(a), extra)
}

// Plugin is a local plugin directory.
type Plugin struct {
	Path string
	// SkipMCPDiscovery loads the plugin without its MCP servers.
	SkipMCPDiscovery bool
}

// UserDialog is a blocking dialog the CLI asks the host to render.
type UserDialog struct {
	Kind      string
	Payload   json.RawMessage
	ToolUseID string
	RequestID string
}

// TurnOptions holds Claude-specific per-message settings. Pass a
// *TurnOptions in agent.Input.ProviderOptions.
type TurnOptions struct {
	// Priority is "now", "next", or "later" for messages sent while a
	// turn runs.
	Priority string
	// NoQuery appends the message without starting a turn; it joins the
	// next message that does.
	NoQuery bool
	// Verbatim delivers this message as written (no @path expansion or
	// slash commands).
	Verbatim bool
	// PastedContent lists pasted text appended after the typed text.
	PastedContent []string
	// Origin stamps who wrote the message. The CLI treats a message
	// without one as unattributed, which strict trust checks refuse; a
	// host relaying keyboard input can pass &MessageOrigin{Kind: "human"}.
	// Nil sends none, as the TypeScript SDK does.
	Origin *MessageOrigin
	// Extra adds fields to the user message.
	Extra map[string]any
}

// MessageOrigin is the provenance of a user message.
type MessageOrigin struct {
	// Kind is "human", "channel", or "peer".
	Kind string `json:"kind"`
	// Server is the channel's MCP server for Kind "channel".
	Server string `json:"server,omitempty"`
	// From is the sending session for Kind "peer"; FromMode is its
	// permission class ("bypass" or "prompting"), Name its display name,
	// and FromSession a host session ID to link back to.
	From        string `json:"from,omitempty"`
	FromMode    string `json:"fromMode,omitempty"`
	Name        string `json:"name,omitempty"`
	FromSession string `json:"fromSession,omitempty"`
}

func mergeJSON(v any, extra map[string]any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil || len(extra) == 0 {
		return data, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	maps.Copy(m, extra)
	return json.Marshal(m)
}
