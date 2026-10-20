// Package pi drives the Pi coding agent through its RPC mode
// ("pi --mode rpc").
//
// Pi has no permission system, host tools, or MCP flags of its own. This
// package loads a small bridge extension (bridge.ts, embedded) that adds
// them through Pi's extension API: host tools are registered as Pi tools,
// tool calls are gated through the uniform PermissionRequest flow, and MCP
// servers are registered with pi.registerMcpServer. The bridge talks to
// the host over the RPC extension UI channel, so it works wherever the
// RPC protocol works.
//
// The uniform agent.Session API covers everyday use. *Session adds every
// RPC command (the follow-up queue, session tree navigation,
// forking, compaction, bash, stats, export) and Command, an escape hatch
// for commands added to Pi after this package was written.
//
// Mapping of agent.Config:
//
//   - Model ("provider/model" or a pattern), Effort (thinking level),
//     SystemPrompt, AppendSystemPrompt, Resume, Fork, SessionID, Ephemeral,
//     Title, and DisallowedTools map to CLI flags.
//   - PermissionMode is enforced by the bridge: an empty mode keeps Pi's
//     default of running every tool, which Info reports as
//     bypassPermissions; default asks before tools that are not read-only,
//     except AllowedTools, MCP ApprovedTools, host tools without
//     NeedsApproval, and AllowSession grants; acceptEdits also runs edit
//     and write without asking; readOnly and plan refuse every tool that
//     is not read-only, whatever AllowedTools and grants say; dontAsk runs
//     read-only tools, AllowedTools, and MCP ApprovedTools, and refuses
//     anything else; bypassPermissions runs everything. Without the
//     bridge, or when the bridge did not load, only an empty mode and
//     bypassPermissions are accepted. When a session switch leaves the
//     bridge unloaded under a gating mode, a Notice with Code
//     "bridge_unloaded" is sent and Send refuses turns until the bridge
//     is back or the mode is bypassPermissions.
//   - An AllowSession answer approves the same bash command, the same
//     path for edit and write, and the same tool otherwise. Grants end
//     with NewSession and SwitchSession.
//   - AdditionalDirectories is ignored, because Pi does not sandbox file
//     access; a Notice with Code "config_ignored" says so, and
//     Config.Strict refuses it.
//   - On Resume, Model is not passed on the command line. It is applied
//     after start with set_model, and only when get_available_models lists
//     it; otherwise the session keeps its recorded model. "provider:id" is
//     accepted as well as "provider/id".
//   - MCPServers are registered by the bridge. Names may contain only
//     letters, digits, "_", and "-", and names that differ only in "-" and
//     "_" collide. A server of the same name in Pi's own mcp.json files
//     overrides the registered one. AlwaysLoad maps to Pi's "direct"
//     exposure; EnabledTools hides the server's other tools through Pi's
//     toolExposure; ApprovedTools run without asking.
//   - UnsetEnv removes variables from the environment Pi starts with.
//
// Behavior worth knowing:
//
//   - Images are refused with an error wrapping agent.ErrUnsupported when
//     the current model does not list "image" input.
//   - With the bridge loaded, Rewind moves the session back in place, so
//     the session ID stays; live user message items carry their Pi entry
//     ID as Item.ID, which Rewind and ForkAtTurn accept. Without the
//     bridge, Rewind forks at the turn's prompt and the session ID changes.
//   - Live assistant text and reasoning items have IDs unique to the
//     Session value; ReadHistory gives them "<entryID>/<index>" IDs
//     instead, because Pi assigns entry IDs only after a message ends.
//     Tool call items carry Pi's tool call ID in both.
//   - Interrupt clears Pi's queued steering and follow-up messages before
//     it aborts; input queued by this library still runs afterwards.
//   - NewSession, SwitchSession, ForkAt, Clone, and Compact hold the
//     session like a turn: they fail while a turn runs or input is queued,
//     and input sent while they run waits for them.
//   - Send runs "/compact [instructions]" and "/autocompact [on|off|toggle]"
//     through RPC, since Pi runs its built-in commands only in its terminal
//     UI.
//   - Custom messages that extensions send are ItemOther items when they
//     set display, as Pi's terminal UI shows them; others arrive as
//     RawEvent values of type "custom_message".
//   - With a todo extension, such as Pi's example todo.ts or rpiv-todo,
//     todo tool results become PlanUpdated events.
//   - Input.OutputSchema needs the bridge. Pi has no structured output, so
//     the bridge gives that turn a structured_output tool whose parameters
//     are the schema (wrapped in a "value" property unless it is an object
//     schema). Pi validates the call and returns validation errors to the
//     model; the tool ends the run, and a run that ends without calling it
//     gets up to two reminders. A turn that still has no output fails with
//     TurnError code "no_structured_output". Such input is never steered
//     into a running turn: it is queued instead. Commands cannot take a
//     schema.
//   - Config.OutputSchema applies the same way to every turn without
//     Input.OutputSchema, except commands. Without the bridge or with
//     NoTools it is ignored with a "config_ignored" Notice, and
//     Config.Strict refuses it.
package pi

import (
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Name is the provider name.
const Name = "pi"

// Options configures the provider.
type Options struct {
	// Path is the pi executable. Empty means "pi" on PATH.
	Path string
	// Spawner starts the process. Nil means agent.LocalSpawner.
	Spawner agent.Spawner
	// Env is added to every session's environment.
	Env map[string]string
	// ExtraArgs are appended to every session's command line.
	ExtraArgs []string
	// StartupTimeout bounds startup. Zero means 60s.
	StartupTimeout time.Duration
	// ShutdownGrace is how long Close waits for a clean exit. Zero means
	// 5s.
	ShutdownGrace time.Duration
	// BridgePath is where the bridge extension is found by the pi process.
	// Empty means the library writes it to the user cache directory and
	// lists it in agent.Command.Files, so a Spawner that runs pi elsewhere
	// can write it at the same path. Set it when bridge.ts (see
	// BridgeSource) is already in place on the far side.
	BridgePath string
	// DisableBridge runs Pi without the bridge extension. Host tools,
	// permission modes, MCP servers, output schemas, and in-place Rewind
	// are then unavailable.
	DisableBridge bool
	// SessionDir is where sessions are stored and looked up: the default
	// of SessionOptions.SessionDir, and the directory ListSessions,
	// ReadHistory, and ListSubagents read. Empty means Pi's own
	// resolution: PI_CODING_AGENT_SESSION_DIR, then sessionDir in the
	// project's .pi/settings.json, then in the agent directory's
	// settings.json, then the agent directory's sessions folder. Relative
	// paths resolve from the working directory.
	SessionDir string
}

// SessionOptions holds Pi-specific session settings. Pass a
// *SessionOptions in agent.Config.ProviderOptions.
type SessionOptions struct {
	// Provider selects the model provider for Model (--provider).
	Provider string
	// Models limits model cycling to these patterns (--models).
	Models []string
	// Extensions loads extension files (--extension).
	Extensions []string
	// NoExtensions disables extension discovery (--no-extensions).
	NoExtensions bool
	// Skills loads skill files or directories (--skill).
	Skills []string
	// NoSkills disables skill discovery (--no-skills).
	NoSkills bool
	// PromptTemplates loads prompt templates (--prompt-template).
	PromptTemplates []string
	// NoPromptTemplates disables prompt template discovery.
	NoPromptTemplates bool
	// NoContextFiles skips AGENTS.md and CLAUDE.md files.
	NoContextFiles bool
	// Tools is an allowlist of enabled tools (--tools). It controls which
	// tools exist; agent.Config.AllowedTools controls which run without
	// asking.
	Tools []string
	// NoTools disables all tools; NoBuiltinTools disables only built-in
	// tools.
	NoTools        bool
	NoBuiltinTools bool
	// NoMCP disables Pi's built-in MCP support.
	NoMCP bool
	// SessionDir stores sessions in this directory (--session-dir).
	// Empty means Options.SessionDir.
	SessionDir string
	// TrustProject decides whether project-local files load: true passes
	// --approve, false passes --no-approve, nil leaves Pi's setting.
	TrustProject *bool
	// Offline disables startup network operations.
	Offline bool
	// SteeringMode and FollowUpMode are "all" or "one-at-a-time".
	SteeringMode string
	FollowUpMode string
	// AutoCompaction and AutoRetry override Pi's settings when not nil.
	AutoCompaction *bool
	AutoRetry      *bool
	// DisableBridge runs this session without the bridge extension, as
	// Options.DisableBridge does for every session.
	DisableBridge bool
	// ExtraArgs are appended to the command line.
	ExtraArgs []string
}
