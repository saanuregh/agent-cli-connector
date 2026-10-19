// Package codex drives the OpenAI Codex CLI through its app-server
// JSON-RPC protocol ("codex app-server").
//
// A Client is one app-server process. It can host many threads; each
// thread is a Session. By default the provider starts one app-server per
// session; set Options.SharedServer to multiplex every session over one
// process, which suits daemons that run many conversations.
//
// The uniform agent.Session API covers everyday use. *Session adds every
// thread operation (review, compact, rewind, goals, shell commands, item
// injection, settings) and *Client exposes every app-server method,
// including Call for methods added to Codex after this package was
// written.
//
// Mapping of agent.Config:
//
//   - Model, Cwd, SystemPrompt (baseInstructions), AppendSystemPrompt
//     (developerInstructions), Resume, Fork, ForkAtTurn (lastTurnId),
//     Ephemeral, Title, MCPServers, and AdditionalDirectories (runtime
//     workspace roots) map to thread parameters.
//   - Effort is sent with each turn and stays in effect.
//   - PermissionMode maps to an approval policy and sandbox:
//     default → on-request + workspace-write (the Codex default), acceptEdits → on-request +
//     workspace-write, readOnly → on-request + read-only, dontAsk → never +
//     workspace-write, bypassPermissions → never + danger-full-access,
//     auto → on-request with the auto reviewer, plan → plan collaboration
//     mode + on-request + read-only, so planning changes nothing. Any
//     other mode is sent as the native approval policy, such as
//     "untrusted", and Codex refuses ones it does not know.
//   - Tools are served as dynamic tools.
//   - AllowedTools has no Codex equivalent and is ignored: it is listed in a
//     Notice with Code "config_ignored", and Config.Strict refuses it.
//     DisallowedTools is refused, because a restriction is never ignored.
//     SessionID is not supported (Codex assigns thread IDs). Tools are
//     ignored the same way when Options.DisableExperimentalAPI is set.
//   - MCPServer.EnabledTools maps to enabled_tools and ApprovedTools to
//     tools.<name>.approval_mode = "approve" in the server's mcp_servers
//     table; AlwaysLoad has no Codex equivalent and is ignored.
//   - Env and UnsetEnv apply to a dedicated app-server only; a shared
//     server uses Options.Env and Options.UnsetEnv. On a shared server Env
//     is ignored like AllowedTools, and UnsetEnv is refused, because it
//     is a restriction (such as stripping secrets) the session cannot
//     apply.
//   - OutputSchema applies to every turn without Input.OutputSchema. Both
//     are normalized to OpenAI strict form; see StrictOutputSchema and
//     SessionOptions.DisableStrictOutputSchema. A steer joins the running
//     turn and keeps its schema.
//
// Behavior worth knowing:
//
//   - DeliverSteer uses turn/steer. When the turn has just ended the input
//     runs as a new turn; when Codex refuses the steer otherwise, Send fails
//     with agent.ErrSteerRejected. DeliverQueue is kept by this library and
//     runs as the next turn.
//   - Interrupt stops the running turn and its subagents' turns; queued input
//     still runs. InterruptTurn drops one queued input or interrupts the
//     running turn.
//   - PermissionResponse.UpdatedInput cannot change a call's arguments, so an
//     allowing answer that sets it is refused.
//   - A file part is sent as text holding its absolute path, because
//     Codex input has no file type; a file part without Path is refused.
package codex

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Name is the provider name.
const Name = "codex"

// Options configures the provider and its app-server processes.
type Options struct {
	// Path is the codex executable. Empty means "codex" on PATH.
	Path string
	// Spawner starts the process. Nil means agent.LocalSpawner.
	Spawner agent.Spawner
	// Env is added to the app-server environment, and UnsetEnv names
	// inherited variables to remove from it.
	Env      map[string]string
	UnsetEnv []string
	// Config holds "-c key=value" overrides for the app-server, with
	// values in TOML syntax.
	Config map[string]string
	// Enable and Disable toggle Codex features (--enable/--disable).
	Enable  []string
	Disable []string
	// ExtraArgs are appended to the app-server command line.
	ExtraArgs []string
	// StartupTimeout bounds the initialize handshake. Zero means 60s.
	StartupTimeout time.Duration
	// ShutdownGrace is how long Close waits for a clean exit. Zero means
	// 5s.
	ShutdownGrace time.Duration
	// ClientName, ClientTitle, and ClientVersion identify the host to
	// Codex. A ClientName also becomes CODEX_INTERNAL_ORIGINATOR_OVERRIDE
	// unless Env sets it, so Codex tailors its model catalog to the host.
	// The reserved name "codex_app_server_daemon" makes the host
	// non-originating: model requests keep Codex's own client identity, and
	// the originator variable is not set.
	ClientName    string
	ClientTitle   string
	ClientVersion string
	// SharedServer runs every session on one app-server process.
	SharedServer bool
	// DisableExperimentalAPI turns off experimental app-server methods and
	// fields. Dynamic tools need them, so Config.Tools is then reported as
	// ignored and Capabilities.HostTools is false.
	DisableExperimentalAPI bool
	// OptOutNotifications lists notification methods the server should
	// not send.
	OptOutNotifications []string
	// InitializeCapabilities is merged into the initialize capabilities,
	// such as {"extensions": {...}} for MCP Apps. The "openai/form" MCP
	// extension is declared by default, so form elicitations may arrive
	// with ElicitationRequest.Mode "openai/form"; an "extensions" map given
	// here is merged with it, and setting "openai/form" to nil removes it.
	InitializeCapabilities map[string]any
	// OnAuthTokensRefresh answers account/chatgptAuthTokens/refresh
	// requests for hosts that manage ChatGPT tokens themselves.
	OnAuthTokensRefresh func(ctx context.Context, params json.RawMessage) (any, error)
	// Logger and WireTap serve a shared server. With SharedServer the
	// per-session Config.Env, Config.UnsetEnv, Config.Logger, and
	// Config.WireTap are ignored, because one process serves every
	// session; dedicated servers use them.
	Logger  *slog.Logger
	WireTap func(dir agent.WireDirection, line []byte)
}

// SessionOptions holds Codex-specific thread settings. Pass a
// *SessionOptions in agent.Config.ProviderOptions.
type SessionOptions struct {
	// ApprovalPolicy overrides the policy derived from PermissionMode:
	// "untrusted", "on-request", "never", or a granular
	// policy object.
	ApprovalPolicy any
	// Sandbox overrides the sandbox derived from PermissionMode:
	// "read-only", "workspace-write", or "danger-full-access".
	Sandbox string
	// ApprovalsReviewer routes approvals to "user", "auto_review", or
	// "guardian_subagent".
	ApprovalsReviewer string
	// Permissions selects a named permissions profile instead of Sandbox.
	Permissions string
	// ModelProvider selects a model provider from the Codex config.
	// Provider.ListSessions filters by Options.Config["model_provider"]
	// instead, because it has no session options.
	ModelProvider string
	// ServiceTier selects a service tier such as "flex".
	ServiceTier string
	// ReasoningSummary is "auto", "concise", "detailed", or "none".
	ReasoningSummary string
	// Personality selects a response style where the model supports it.
	Personality string
	// Config holds config overrides for this thread, keyed by dotted path.
	Config map[string]any
	// HistoryMode selects the persisted history contract.
	HistoryMode string
	// ServiceName labels the thread for analytics.
	ServiceName string
	// ExtraThreadParams adds fields to thread/start, thread/resume, and
	// thread/fork.
	ExtraThreadParams map[string]any
	// ExtraTurnParams adds fields to every turn/start.
	ExtraTurnParams map[string]any
	// DisableStrictOutputSchema sends Config.OutputSchema and
	// Input.OutputSchema unchanged instead of normalizing them with
	// StrictOutputSchema.
	DisableStrictOutputSchema bool
	// DisableContextRestore stops re-sending the last
	// TurnOptions.AdditionalContext after Codex compacts the context.
	// Codex drops it on compaction and resends it only when it changes.
	DisableContextRestore bool
}

// TurnOptions holds Codex-specific per-turn settings. Pass a
// *TurnOptions in agent.Input.ProviderOptions.
type TurnOptions struct {
	// CollaborationMode selects a preset mode such as plan mode.
	CollaborationMode *CollaborationMode
	// ReasoningSummary overrides the summary setting.
	ReasoningSummary string
	// ServiceTier overrides the service tier for this turn only.
	ServiceTier string
	// StickyServiceTier sets the service tier for this and later turns.
	StickyServiceTier string
	// ApprovalPolicy, SandboxPolicy, ApprovalsReviewer, and Cwd override
	// the thread settings for this and later turns. ApprovalPolicy is
	// "untrusted", "on-request", "never", or a granular policy object.
	// SandboxPolicy is "read-only", "workspace-write",
	// "danger-full-access", or a native SandboxPolicy object.
	ApprovalPolicy    any
	SandboxPolicy     any
	ApprovalsReviewer string
	Cwd               string
	// AdditionalContext adds keyed context fragments. Each value is an
	// entry such as {"kind": "application", "value": "..."}; kind is
	// "application" or "untrusted".
	AdditionalContext map[string]any
	// Extra adds fields to turn/start.
	Extra map[string]any
}

// CollaborationMode is a preset turn mode.
type CollaborationMode struct {
	// Mode is "plan" or "default".
	Mode     string                    `json:"mode"`
	Settings CollaborationModeSettings `json:"settings"`
}

// CollaborationModeSettings configures a collaboration mode.
type CollaborationModeSettings struct {
	Model                 string  `json:"model"`
	ReasoningEffort       *string `json:"reasoning_effort"`
	DeveloperInstructions *string `json:"developer_instructions"`
}
