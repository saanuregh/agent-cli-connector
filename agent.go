// Package agent is a uniform Go API over coding-agent command-line harnesses
// such as Claude Code, Codex, Pi, and OpenCode.
//
// A [Provider] starts [Session] values. A session is one live conversation
// backed by a harness process. Every session speaks the same vocabulary:
// [Input] goes in through [Session.Send] or [Session.Run], and typed [Event]
// values come out of [Session.Events]. Requests that need an answer from the
// host (tool permissions, questions for the user, MCP elicitations, and calls
// to host-defined [Tool] values) arrive as events with Respond methods, or go
// to the callbacks in [Handlers].
//
// The uniform surface never hides a harness feature. Each provider package
// returns a concrete session type with typed methods for every native
// control, plus a generic escape hatch for controls added after this library
// was written. Every event produced from a native message keeps that message
// in [EventMeta.Raw], and native messages without a uniform meaning arrive as
// [RawEvent] or as provider-specific event types.
//
// Provider packages:
//
//   - github.com/saanuregh/agent-cli-connector/claude
//   - github.com/saanuregh/agent-cli-connector/codex
//   - github.com/saanuregh/agent-cli-connector/pi
//   - github.com/saanuregh/agent-cli-connector/opencode
package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// Provider starts sessions for one harness.
//
// Provider values are safe for concurrent use. A provider holds static
// configuration such as the executable path; per-conversation settings go
// in [Config].
type Provider interface {
	// Name returns the stable provider identifier, such as "claude".
	Name() string
	// Capabilities reports which uniform features this provider supports.
	Capabilities() Capabilities
	// Start launches a new or resumed session. The returned session is
	// ready to accept input. Start returns after the harness completed its
	// startup handshake, so a startup failure is reported here.
	Start(ctx context.Context, cfg Config) (Session, error)
}

// ModelLister is implemented by providers that can list the models the
// harness offers.
type ModelLister interface {
	ListModels(ctx context.Context) ([]Model, error)
}

// SessionLister is implemented by providers that can list persisted
// sessions, for example to offer them for resumption.
type SessionLister interface {
	ListSessions(ctx context.Context, opts ListSessionsOptions) ([]SessionSummary, error)
}

// HistoryReader is implemented by providers that can read the persisted
// transcript of a session without starting it.
type HistoryReader interface {
	ReadHistory(ctx context.Context, sessionID string, opts ReadHistoryOptions) ([]Item, error)
}

// SessionDeleter is implemented by providers that can delete a persisted
// session. Deletion cannot be undone. cwd narrows the search where the
// harness stores sessions per directory; empty searches everywhere. Do not
// delete a session that a running session still uses.
type SessionDeleter interface {
	DeleteSession(ctx context.Context, sessionID, cwd string) error
}

// Titler is implemented by sessions that can rename themselves.
type Titler interface {
	SetTitle(ctx context.Context, title string) error
}

// Compacter is implemented by sessions that can compact their context on
// request. Compact returns after the compaction finished. instructions
// guide the summary where the harness supports it and are ignored
// elsewhere.
type Compacter interface {
	Compact(ctx context.Context, instructions string) error
}

// Forker is implemented by sessions that can copy their conversation into a
// new, independent session. cfg supplies settings for the new session;
// Resume and Fork are filled in.
type Forker interface {
	ForkSession(ctx context.Context, cfg Config) (Session, error)
}

// Rewinder is implemented by sessions that can drop the latest turns of
// their conversation in place. Rewind removes turnID and every later turn;
// file changes are not undone. turnID is a TurnResult.TurnID from this
// session, or a native turn or message ID from the session's history. The
// session ID can change, which a SessionUpdated event reports.
type Rewinder interface {
	Rewind(ctx context.Context, turnID string) error
}

// Checker is implemented by providers that can report whether their
// harness is installed and signed in without starting a session. Problems
// with the harness, such as a missing executable, an unsupported version,
// or a failed sign-in check, are reported in ProviderStatus.Err with a nil
// error. The error is non-nil only when the check could not run, such as
// when ctx ended.
type Checker interface {
	Check(ctx context.Context) (*ProviderStatus, error)
}

// ProviderStatus describes a harness installation.
type ProviderStatus struct {
	// Installed is false when the executable cannot be started.
	Installed bool
	// Version is the harness version, such as "2.1.301".
	Version string
	// Auth is nil when the sign-in state is unknown.
	Auth *AuthStatus
	// Err explains what is wrong with the harness, such as a missing
	// executable or an unsupported version.
	Err error
}

// AuthStatus describes how the harness is signed in.
type AuthStatus struct {
	Authenticated bool
	// Method is how the harness authenticates, such as "apiKey", "oauth",
	// or "chatgpt".
	Method string
	Email  string
	// Plan is the subscription, such as "max" or "plus".
	Plan string
	Raw  json.RawMessage
}

// RateLimitReader is implemented by sessions that can read the account's
// plan usage windows on demand.
type RateLimitReader interface {
	RateLimits(ctx context.Context) ([]RateLimit, error)
}

// SubagentLister is implemented by providers that can list the subagents
// a persisted session ran. Pass a Task ID as ReadHistoryOptions.SubagentID
// to read one subagent's transcript.
type SubagentLister interface {
	ListSubagents(ctx context.Context, sessionID string, opts ListSubagentsOptions) ([]Task, error)
}

// TaskStopper is implemented by sessions that can stop one task, such as a
// subagent, without interrupting the turn. taskID is a Task.ID the session
// reported; an unknown ID is an error, and a task that already finished is
// not.
type TaskStopper interface {
	StopTask(ctx context.Context, taskID string) error
}

// TurnInterrupter is implemented by sessions that can stop one turn
// without touching the others: a queued turn is dropped and fails with
// ErrCancelled, and a running turn is interrupted. A turn that already
// ended is not an error. Session.Run uses it when its context ends.
type TurnInterrupter interface {
	InterruptTurn(ctx context.Context, turnID string) error
}

// Session is one live conversation with a harness.
//
// All methods are safe for concurrent use. Methods that talk to the harness
// take a context that bounds only that call; cancelling it does not end the
// session. Close ends the session.
type Session interface {
	// ID returns the native session identifier: the Claude session ID, the
	// Codex thread ID, the Pi session ID, or the OpenCode session ID. It is
	// the value to pass as Config.Resume later.
	ID() string
	// Provider returns the provider name, such as "claude".
	Provider() string
	// Info returns the latest known session metadata.
	Info() SessionInfo
	// Capabilities reports which uniform features this session supports.
	// They can be narrower than the provider's when session settings turn
	// features off.
	Capabilities() Capabilities

	// Events returns the session's event stream. The same channel is
	// returned on every call. It is closed after the session ends and the
	// last event was delivered. Events are queued without limit until read
	// unless Config.EventBufferLimit is set, so a session that is never
	// drained grows in memory.
	Events() <-chan Event

	// Send submits input. When the session is idle a new turn starts. When
	// a turn is running, Input.Delivery decides whether the input is queued
	// for after the turn, steered into it, or interrupts it. The returned
	// turn completes when the harness reports the turn's outcome.
	Send(ctx context.Context, in Input) (*Turn, error)
	// Run sends input and waits for the turn to finish. It returns the
	// error of Turn.Wait: a turn that ran but failed or was interrupted
	// returns both its result and its *TurnError.
	Run(ctx context.Context, in Input) (*TurnResult, error)
	// Interrupt stops the running turn, if any. Input queued behind it
	// still runs afterwards; TurnInterrupter stops one queued input.
	// Steered input the model has not seen yet may be dropped; see each
	// provider's Interrupt.
	Interrupt(ctx context.Context) error

	// SetModel switches the model for later requests.
	SetModel(ctx context.Context, model string) error
	// SetEffort changes the reasoning effort for later requests.
	SetEffort(ctx context.Context, effort Effort) error
	// SetPermissionMode changes how tool calls are approved.
	SetPermissionMode(ctx context.Context, mode PermissionMode) error

	// PendingRequests lists requests that still wait for an answer, in the
	// order they arrived. Use it to show requests again after a UI
	// reconnects.
	PendingRequests() []Request

	// Close ends the session and stops the harness process. It waits for
	// the process to exit. Close is idempotent.
	Close() error
	// Done is closed after the session ended.
	Done() <-chan struct{}
	// Err returns the reason the session ended, or nil while it runs and
	// after a clean Close.
	Err() error
}

// Config holds the per-session settings shared by every provider. Fields a
// harness cannot honor are ignored and reported in a Notice, or refused when
// Strict is set; restrictions it cannot enforce, such as DisallowedTools or
// a read-only permission mode, are always refused. See each provider
// package for the exact mapping.
// Provider-specific settings go in ProviderOptions.
type Config struct {
	// Cwd is the working directory of the session. Empty means the current
	// directory of this process.
	Cwd string
	// Model selects the model. Empty means the harness default. Pi accepts
	// "provider/model" and optional ":thinking" suffixes.
	Model string
	// Effort selects reasoning effort. Empty means the harness default.
	Effort Effort
	// PermissionMode selects how tool calls are approved. Empty means the
	// harness default.
	PermissionMode PermissionMode

	// SystemPrompt replaces the harness system prompt when not empty.
	SystemPrompt string
	// AppendSystemPrompt is appended to the system prompt.
	AppendSystemPrompt string
	// OutputSchema asks every turn of the session to end with an answer
	// matching this JSON Schema, returned in TurnResult.StructuredOutput.
	// Input.OutputSchema overrides it for one turn where the harness
	// supports per-turn schemas.
	OutputSchema json.RawMessage

	// Resume is a native session ID to continue.
	Resume string
	// Fork, together with Resume, continues the conversation in a new
	// session and leaves the original unchanged.
	Fork bool
	// ForkAtTurn, together with Fork, keeps the conversation only up to
	// and including this turn: a TurnResult.TurnID, or a native turn or
	// message ID from the session's history.
	ForkAtTurn string
	// SessionID requests a specific ID for a new session where the harness
	// supports choosing it (Claude needs a UUID; Pi accepts any ID).
	SessionID string
	// Ephemeral asks the harness not to persist the session.
	Ephemeral bool
	// Title names the session where supported.
	Title string

	// MCPServers adds MCP servers by name.
	MCPServers map[string]MCPServer
	// Tools are host-implemented tools the model can call. The library
	// serves them over each harness's native extension point.
	Tools []Tool
	// AllowedTools lists tools that run without asking.
	AllowedTools []string
	// DisallowedTools lists tools the model may not use.
	DisallowedTools []string
	// AdditionalDirectories grants access to directories outside Cwd.
	AdditionalDirectories []string

	// Env adds or overrides environment variables for the harness process.
	// The process inherits this process's environment first.
	Env map[string]string
	// UnsetEnv removes inherited environment variables, such as ones that
	// mark this process as running inside another harness session.
	UnsetEnv []string
	// Handlers answer requests through callbacks instead of events.
	Handlers Handlers
	// Logger receives diagnostics such as harness stderr. Nil discards.
	Logger *slog.Logger
	// WireTap, when set, receives every raw protocol line in both
	// directions. It runs on the I/O goroutine and must not block.
	WireTap func(dir WireDirection, line []byte)
	// EventBufferLimit caps the number of unread events. When the cap is
	// reached, the oldest unread events are dropped and an EventsDropped
	// event reports how many. Zero means no limit.
	EventBufferLimit int

	// ProviderOptions carries provider-specific settings, such as
	// *claude.SessionOptions. Start fails for a value of another provider's
	// type.
	ProviderOptions any

	// Strict makes Start fail, with an error wrapping ErrUnsupported, when
	// the config sets fields this harness cannot honor. Without it such
	// fields are ignored and one Notice with Code "config_ignored", sent
	// right after SessionStarted, lists them. Restrictions a harness cannot
	// enforce fail Start either way.
	Strict bool
}

// WireDirection tells whether a tapped protocol line was sent to or
// received from the harness.
type WireDirection int

const (
	// WireSend marks a line written to the harness.
	WireSend WireDirection = iota + 1
	// WireReceive marks a line read from the harness.
	WireReceive
)

func (d WireDirection) String() string {
	switch d {
	case WireSend:
		return "send"
	case WireReceive:
		return "receive"
	default:
		return "unknown"
	}
}

// Effort is a reasoning-effort level. The named constants cover the common
// levels; any harness-native string is passed through unchanged.
type Effort string

// Common effort levels. Not every harness or model accepts every level.
const (
	EffortNone    Effort = "none"
	EffortMinimal Effort = "minimal"
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
	EffortXHigh   Effort = "xhigh"
	EffortMax     Effort = "max"
)

// PermissionMode decides how tool calls are approved. The named constants
// are mapped to each harness's closest native setting. Any other string is
// passed unchanged to Claude (a native mode) and Codex (a native approval
// policy); Pi and OpenCode refuse it.
type PermissionMode string

const (
	// PermissionDefault asks before risky actions.
	PermissionDefault PermissionMode = "default"
	// PermissionAcceptEdits runs file edits without asking.
	PermissionAcceptEdits PermissionMode = "acceptEdits"
	// PermissionPlan lets the model plan without changing anything.
	PermissionPlan PermissionMode = "plan"
	// PermissionBypass runs every action without asking.
	PermissionBypass PermissionMode = "bypassPermissions"
	// PermissionReadOnly allows reading but not changing anything.
	PermissionReadOnly PermissionMode = "readOnly"
	// PermissionDontAsk never asks; actions that would ask are denied.
	PermissionDontAsk PermissionMode = "dontAsk"
	// PermissionAuto lets the harness decide each call automatically.
	PermissionAuto PermissionMode = "auto"
)

// Capabilities reports the uniform features a provider or session
// supports. A false field means the matching method returns an error
// wrapping ErrUnsupported or the matching Config field is ignored.
// Optional methods such as Rewind and Compact have no flag: check for them
// with a type assertion on the session, such as sess.(agent.Rewinder).
type Capabilities struct {
	Streaming        bool // text and reasoning arrive as deltas
	ReasoningStream  bool // reasoning deltas are emitted
	Images           bool // image input parts are accepted
	Steer            bool // input can be injected into a running turn
	Queue            bool // input sent during a turn is queued
	Interrupt        bool
	SetModel         bool
	SetEffort        bool
	PermissionModes  []PermissionMode // modes SetPermissionMode accepts
	PermissionPrompt bool             // PermissionRequest events are produced
	UserQuestions    bool             // UserInputRequest events are produced
	Elicitation      bool             // ElicitationRequest events are produced
	HostTools        bool             // Config.Tools is supported
	MCPServers       bool             // Config.MCPServers is supported
	Resume           bool
	Fork             bool
	SystemPrompt     bool
	OutputSchema     bool // per-turn Input.OutputSchema is supported
	Plans            bool // PlanUpdated events are produced
	Tasks            bool // TaskUpdated events are produced
	Subagents        bool // subagent tasks and nested items are reported
	CostReporting    bool // TurnResult.Usage.CostUSD is filled
	RateLimits       bool // RateLimitUpdated events are produced
	Hooks            bool // HookRun events are produced
	Goals            bool // GoalUpdated events are produced
	ForkAtTurn       bool // Config.ForkAtTurn is supported
}

// Model describes a model offered by a harness.
type Model struct {
	ID          string
	DisplayName string
	Description string
	Provider    string // upstream provider for multi-provider harnesses such as Pi
	IsDefault   bool
	// ResolvedID is, for an alias entry such as Claude's "default", the
	// concrete model it currently selects; empty otherwise.
	ResolvedID string
	Efforts    []Effort
	// DefaultEffort is empty when unknown.
	DefaultEffort Effort
	ContextWindow int64
	// ServiceTiers lists speed or priority tiers the model offers, such
	// as "fast"; DefaultServiceTier is empty when unknown.
	ServiceTiers       []string
	DefaultServiceTier string
	// InputModalities lists accepted input kinds, such as "text" and
	// "image"; empty when unknown.
	InputModalities []string
	Raw             json.RawMessage
}

// SessionInfo is the metadata of a live session.
type SessionInfo struct {
	ID       string
	Provider string
	Cwd      string
	// Model, Effort, and PermissionMode are what the harness uses, its
	// defaults included, as far as it reports them; empty when unknown.
	Model          string
	Effort         Effort
	PermissionMode PermissionMode
	Title          string
	// Version is the harness version when known.
	Version string
	// Tools lists the tool names the model can call, when known.
	Tools []string
	// Commands lists slash commands, skills, and prompt templates.
	Commands []SlashCommand
	// MCPServers reports configured MCP servers and their status.
	MCPServers []MCPServerStatus
	// Raw holds the native startup payload, such as the Claude initialize
	// response or the Codex thread/start response.
	Raw json.RawMessage
}

// SlashCommand is a slash command, skill, or prompt template the harness
// understands. Send "/" + Name as text to run it.
type SlashCommand struct {
	Name         string
	Description  string
	ArgumentHint string
	// Kind is "command", "skill", "prompt", or "extension".
	Kind string
	// Path is the file defining the command or skill, when known.
	Path string
	// Scope is where it is defined, such as "user", "project", or
	// "builtin", when known.
	Scope string
}

// MCPServerStatus reports an MCP server's connection state.
type MCPServerStatus struct {
	Name   string
	Status string // such as "connected", "failed", "pending", "needs-auth"
	Error  string
}

// ListSessionsOptions filters ListSessions.
type ListSessionsOptions struct {
	// Cwd limits results to sessions started in this directory. Empty
	// means all directories.
	Cwd string
	// Limit caps the number of results. Zero means no limit.
	Limit int
	// Offset skips this many of the newest sessions, so Limit and Offset
	// page through the list.
	Offset int
}

// SessionSummary describes a persisted session.
type SessionSummary struct {
	ID       string
	Provider string
	Title    string
	Cwd      string
	// CreatedAt and UpdatedAt are zero when unknown.
	CreatedAt time.Time
	UpdatedAt time.Time
	// Path is the transcript file when the harness stores one.
	Path string
	// Model and Effort are the latest settings recorded in the session,
	// when known, for resuming it the same way.
	Model  string
	Effort Effort
	// FirstPrompt and LastPrompt preview the first and latest user
	// prompts, when known.
	FirstPrompt string
	LastPrompt  string
	Raw         json.RawMessage
}

// ListSubagentsOptions controls ListSubagents.
type ListSubagentsOptions struct {
	// Cwd helps locate the transcript when the harness stores sessions
	// per directory. Empty searches all directories.
	Cwd string
}

// ReadHistoryOptions controls ReadHistory.
type ReadHistoryOptions struct {
	// Cwd helps locate the transcript when the harness stores sessions
	// per directory. Empty searches all directories.
	Cwd string
	// SubagentID reads the transcript of this subagent (a Task ID)
	// instead of the main conversation.
	SubagentID string
	// Limit returns only the newest Limit items, still in transcript
	// order. Zero means all items.
	Limit int
	// Offset skips this many of the newest items first, so Limit and
	// Offset page backwards through a long transcript.
	Offset int
}
