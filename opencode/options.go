// Package opencode drives OpenCode 2 (the @opencode/cli package) through
// its HTTP API and server-sent event stream.
//
// OpenCode 2 is a server, not a stdio protocol. By default every session
// starts a private server with "opencode serve --stdio" through the
// configured agent.Spawner: the server prints its URL on standard output and
// exits when its standard input closes, so Close never leaves it running.
// The server is protected by a random password the library passes in
// OPENCODE_PASSWORD. Options.SharedServer lets the sessions of one Provider
// share a single private server, and Options.ServerURL attaches to a server
// that is already running, such as "opencode serve" or the background
// service, without starting a process.
//
// A remote Spawner, such as one that runs OpenCode in a container or over
// SSH, must make the server's URL reachable from this process: the server
// listens on Options.Hostname of the remote machine, and Options.MapURL can
// rewrite the URL it prints, for example to a forwarded port. Host tools are
// served from this process over HTTP, so the server must also reach the
// URL Options.HostToolsURL returns.
//
// Provider calls that need a server, such as ListModels or ReadHistory,
// start a short-lived private server unless Options.ServerURL is set or a
// shared server runs. It runs in Options.HelperDir, or in a private
// folder under the user cache directory, with project config turned off
// (OPENCODE_DISABLE_PROJECT_CONFIG), because OpenCode otherwise loads
// configs and plugins from its working directory and every parent. The
// global config in the user's config directory still loads. Session
// servers load the project config of Config.Cwd as usual, so a session
// must not run below a folder other users can write.
//
// Config.WireTap sees every request and response body and every event. It
// is not redacted: model and provider listings carry provider settings,
// the host tool registration carries its access token, and on a shared
// server PUT /environment carries this process's whole environment.
//
// The uniform agent.Session API covers everyday use. *Session adds typed
// methods for OpenCode features without a uniform equivalent (agents,
// permission rules, the inbox, instructions, shell commands, diffs, export,
// side questions) and Call, an escape hatch for any HTTP route, including
// routes added after this package was written. Every event built from an
// OpenCode event keeps the event's JSON in EventMeta.Raw; events without a
// uniform meaning arrive as agent.RawEvent with the native type.
//
// Mapping of agent.Config:
//
//   - Model is "provider/model", optionally with "#variant"; Effort selects
//     the model variant, such as "low" or "high". A model the server does
//     not list is refused.
//   - PermissionMode is mapped to session permission rules and agents:
//     default asks before shell commands, edits, and access outside the
//     working directory; acceptEdits does not ask before edits; plan
//     switches to the "plan" agent, which only writes its plan; readOnly
//     denies every action except reading files (read, glob, grep), skills,
//     questions, subagents, host tools marked ReadOnly, and shell commands
//     and access outside the working directory, which ask as in
//     acceptEdits; it also denies AllowedTools and MCP tools other than
//     those; bypassPermissions allows everything the agent and the
//     OpenCode config do not deny. No mode lifts an agent or config deny.
//     An empty mode keeps the rules the session had when it was opened.
//     Subagent sessions get the same mode for their own agent. An
//     AllowSession grant allows the asked resource, or OpenCode's save
//     pattern for it when that is provably no broader than the rule that
//     asked; it never lifts a deny or another rule's ask. AllowAlways is
//     OpenCode's project-wide saved approval, which outranks every ask but
//     no deny. rules.go documents the whole model. SetAgent switches to or
//     from plan mode with its rules; leaving plan restores the mode before
//     it, or default for a session opened in plan. An agent switch by
//     another client changes no mode; the mode's rules are written again
//     for the new agent. A session without SessionOptions.Agent runs the
//     server's default agent until an agent switch or plan mode pins one.
//     A config reload rewrites the rules. Shell and read are never denied by a
//     mode, because OpenCode's free models refuse sessions whose rules deny
//     them.
//   - AllowedTools and DisallowedTools become allow and deny rules for the
//     named permission actions, such as "shell", "edit", "webfetch", or
//     "<mcp-server>_<tool>". AdditionalDirectories allow external_directory
//     access below each directory.
//   - SystemPrompt replaces the system prompt of the build and plan agents
//     (and SessionOptions.Agent) through OPENCODE_CONFIG_CONTENT, so it needs
//     a private, unshared server. AppendSystemPrompt is a session
//     instruction entry and works everywhere.
//   - Tools are served by an MCP server this process runs over HTTP and
//     registers with OpenCode for the session's directory. Rules keep each
//     session to its own host tools; on a shared or attached server every
//     session denies the host tools of the others. Sessions this library
//     did not create carry no such rule, so on an attached server they can
//     call the host tools of a session in the same directory; Tools there
//     needs Options.AllowHostToolsOnAttachedServer.
//   - MCPServers are registered for the session's directory through the
//     server's MCP API. Sessions of one Provider share a registration, and
//     the last of them to close removes it from a shared or attached
//     server. A server name the directory already configures otherwise is
//     left alone. AlwaysLoad turns off
//     OpenCode's code mode for the server, ApprovedTools become allow rules,
//     and EnabledTools denies the server's other tools. Tool actions are
//     "<server>_<tool>" with OpenCode's cleaned names, so a server whose
//     rules could also decide another server's tools is refused: with
//     EnabledTools, any server, of the session or the directory, whose
//     cleaned name equals or extends its own with "_", or that its own
//     extends; with only ApprovedTools, a server whose cleaned name and "_"
//     begin one of the approved actions.
//   - Env and UnsetEnv set the private server's environment. On a shared
//     private server they set the session environment instead, which
//     OpenCode replaces as a whole: it is this process's environment with
//     Options.Env and Env applied and UnsetEnv removed. An attached server
//     (Options.ServerURL) refuses them with agent.ErrUnsupported, because
//     that would send this process's secrets and overwrite the remote
//     server's PATH and HOME.
//   - Ephemeral deletes a session this library created, a new session or a
//     fork, on Close: OpenCode always persists sessions. A resumed session
//     is never deleted.
//   - Effort without Model applies to the server's default model.
//   - SessionID, Title, Resume, Fork, and ForkAtTurn map to the session API.
//
// Behavior worth knowing:
//
//   - A turn is one OpenCode execution. Its ID is the ID of the user
//     message that started it, which Rewind and ForkAtTurn accept, as they
//     accept the message IDs in ReadHistory items.
//   - DeliverSteer sends the input with OpenCode's steer delivery into the
//     running execution. When the execution ends before OpenCode delivered
//     the steer, OpenCode runs another execution for it, which belongs to the
//     same turn. DeliverQueue is kept by this library and runs as a new
//     turn: OpenCode's own queue delivery would join the running execution.
//     A steer into a turn that is interrupted or fails before OpenCode
//     delivered it is taken back, and its turn fails with
//     agent.ErrCancelled. Input.Model and Input.Effort apply when the input
//     starts or joins a run.
//   - Interrupt stops the running execution and the background subagents
//     still running, and takes back steers OpenCode has not delivered yet;
//     input queued by this library runs afterwards. InterruptTurn stops one
//     turn: queued input is dropped. Close drops queued input.
//   - PermissionResponse.UpdatedInput cannot change a call's arguments, so an
//     allowing answer that sets it is refused.
//   - Resuming a session adopts an execution it is running, the requests
//     it waits on, and its running subagents.
//   - Sending "/compact" or "/summarize" runs a compaction, which fails
//     when OpenCode reports the compaction failed; "/name args" runs the
//     slash command name when the server lists it, or the skill name with
//     args as the prompt.
//   - Subagents are OpenCode child sessions. Their Task ID is the child
//     session ID, which ReadHistoryOptions.SubagentID and StopTask accept.
//     A background subagent outlives the turn that started it: its items
//     and requests stay open after that turn ends.
//   - Rewind reports the items it dropped with agent.ItemsRemoved. Only
//     items this session saw live are listed; items from before a resume
//     or from while the event stream was down are not.
//   - The same MCP server name registered by two sessions of a provider
//     with different configurations keeps the first one and emits a
//     Notice.
//   - OpenCode has no todo tool, output schemas, rate-limit reports, or
//     hooks, so Input.OutputSchema is refused, Config.OutputSchema is
//     ignored with a Notice (an error under Config.Strict), and the
//     matching events never arrive.
//   - Images are sent inline as data URIs; file parts are sent as file://
//     URIs that the server reads, so their paths must exist where the server
//     runs. Remote image URLs are refused, as OpenCode refuses them.
//   - OpenCode 1.x is not supported: its API differs completely. Check and
//     Start report a server older than 2.0 as an error.
package opencode

import (
	"net/http"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Name is the provider name.
const Name = "opencode"

// Options configures the provider.
type Options struct {
	// Path is the opencode executable. Empty means "opencode" on PATH.
	Path string
	// Spawner starts the server process. Nil means agent.LocalSpawner.
	Spawner agent.Spawner
	// Env is added to every private server's environment.
	Env map[string]string
	// ExtraArgs are appended to the "serve" command line.
	ExtraArgs []string
	// StartupTimeout bounds starting a server and a session. Zero means
	// 60s.
	StartupTimeout time.Duration
	// ShutdownGrace is how long Close waits for a private server to exit
	// after closing its input. Zero means 5s.
	ShutdownGrace time.Duration
	// Hostname and Port are where a private server listens. Empty means
	// 127.0.0.1, and zero means any free port.
	Hostname string
	Port     int
	// MapURL rewrites the URL a private server prints before it is used,
	// for spawners that run the server on another machine.
	MapURL func(url string) (string, error)

	// ServerURL attaches every session to a running OpenCode 2 server
	// instead of starting one. Username defaults to "opencode", the only
	// user OpenCode accepts.
	ServerURL string
	Username  string
	Password  string

	// SharedServer runs one private server for all sessions of this
	// provider. It starts with the first session and stops when the last
	// one closes. Env and UnsetEnv of a session then do not change the
	// server's environment, and SystemPrompt is refused.
	SharedServer bool

	// HTTPClient sends API requests. Nil means a client without a timeout;
	// each call is bounded by its context.
	HTTPClient *http.Client

	// HelperDir is the working directory of the servers provider calls
	// start, such as for ListModels or ReadHistory. Those servers load no
	// project config. Empty means agent-cli-connector/opencode under the
	// user cache directory. The folder must be a plain directory you own;
	// on Unix it is kept at mode 0700.
	HelperDir string

	// AllowHostToolsOnAttachedServer allows Config.Tools on a server named
	// by ServerURL. Host tools are an MCP server registered for the
	// session's directory, and this library keeps its own sessions apart
	// with permission rules, but sessions it did not create on that server,
	// such as another client's in the same directory, can call them. A
	// session with Tools on an attached server is refused unless this is
	// set.
	AllowHostToolsOnAttachedServer bool

	// HostToolsAddr is the address the host tool MCP server listens on.
	// Empty means 127.0.0.1 on any free port.
	HostToolsAddr string
	// HostToolsURL returns the URL OpenCode uses to reach a session's host
	// tool MCP server, given the address it listens on. Nil means
	// "http://<address>/mcp".
	HostToolsURL func(addr string) string
}

// SessionOptions holds OpenCode-specific session settings. Pass a
// *SessionOptions in agent.Config.ProviderOptions.
type SessionOptions struct {
	// Agent is the primary agent for turns outside plan mode. Empty means
	// "build".
	Agent string
	// Permissions are appended after the rules the permission mode and
	// the other settings produce; the last matching rule wins.
	Permissions []PermissionRule
	// Metadata is stored on a new session.
	Metadata map[string]any
	// Instructions adds session instruction entries by key, which OpenCode
	// appends to the system prompt.
	Instructions map[string]string
	// Config is merged into OPENCODE_CONFIG_CONTENT of a private, unshared
	// server, for settings such as providers or agents.
	Config map[string]any
}

// TurnOptions holds OpenCode-specific settings for one input. Pass a
// *TurnOptions in agent.Input.ProviderOptions.
type TurnOptions struct {
	// Agents attaches agents to the prompt, as "@agent" mentions do.
	Agents []string
	// Metadata is stored on the user message.
	Metadata map[string]any
}

// PermissionRule is one OpenCode permission rule. Action is a permission
// action such as "shell", "edit", "read", "external_directory", or an MCP
// tool's "<server>_<tool>"; Resource is a glob such as "*" or a path
// pattern; Effect is "allow", "deny", or "ask".
type PermissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// PermissionReply is a native answer to a permission request. Pass it as
// agent.PermissionResponse.ProviderResponse. Decision is "once", "always",
// or "reject"; a reject without Message stops the execution.
type PermissionReply struct {
	Decision string `json:"decision"`
	Message  string `json:"message,omitempty"`
}
