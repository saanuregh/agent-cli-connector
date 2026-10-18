# agent-cli-connector

One Go API for coding-agent CLIs: [Claude Code](https://github.com/anthropics/claude-agent-sdk-typescript),
[Codex](https://github.com/openai/codex), [Pi](https://github.com/earendil-works/pi), and
[OpenCode 2](https://opencode.ai). Use it to build chat apps, daemons, and orchestrators without learning four
protocols.

```go
sess, err := claude.New(claude.Options{}).Start(ctx, agent.Config{Cwd: dir})
if err != nil { return err }
defer sess.Close()

go func() {
	for ev := range sess.Events() {
		switch ev := ev.(type) {
		case *agent.TextDelta:
			fmt.Print(ev.Delta)
		case *agent.PermissionRequest:
			ev.Allow(ctx)
		}
	}
}()

res, err := sess.Run(ctx, agent.Text("Add a test for parseConfig"))
if err != nil {
	log.Fatal(err) // includes a turn that failed or was interrupted
}
fmt.Println(res.Status, res.Usage.CostUSD)
```

Swap `claude.New` for `codex.New`, `pi.New`, or `opencode.New` and nothing else changes.

## Install

```sh
go get github.com/saanuregh/agent-cli-connector
```

The harness CLIs must be installed (`claude`, `codex`, `pi`, `opencode` 2.x) or reachable through a custom `agent.Spawner`.
No third-party Go dependencies. [REFERENCES.md](REFERENCES.md) lists the CLI versions and reference projects this
library was verified against.

## Concepts

| Concept | Type | Purpose |
| --- | --- | --- |
| Provider | `agent.Provider` | Starts sessions for one harness (`claude.New`, `codex.New`, `pi.New`, `opencode.New`). |
| Session | `agent.Session` | One live conversation: `Send`, `Run`, `Interrupt`, `SetModel`, `SetEffort`, `SetPermissionMode`, `Close`. |
| Input | `agent.Input` | Text, images, file references, per-turn model/effort/output schema, delivery mode. |
| Turn | `*agent.Turn` | Handle for submitted input; `Wait` returns a `TurnResult` (status, final text, structured output, usage, cost, items). |
| Events | `agent.Event` | Typed stream: text/reasoning/tool deltas, transcript items, plans, tasks, usage, compaction, retries, notices. `agent.EventType` names an event's type for logs and wire formats. |
| Requests | `PermissionRequest`, `UserInputRequest`, `ElicitationRequest` | Events with `Respond` methods, or answered by `Config.Handlers` callbacks. |
| Host tools | `agent.Tool` | Go functions the model can call, on every harness. |
| Transcript | `agent.Transcript` | Folds events into a renderable item list for UIs. |

### Sending while a turn runs

`Input.Delivery` decides: `DeliverQueue` (the default when empty) runs the input after the turn, `DeliverSteer` injects it into
the running turn, `DeliverInterrupt` stops the turn and runs it next. Each provider maps these to native
mechanisms (Claude message priorities `later`/`next`/`now`, Codex `turn/steer`, Pi `steer`). `Send` fails for any
other `Delivery` value and for an unknown `Part.Type`. `Session.Interrupt` stops the running turn and leaves queued
input to run afterwards; `InterruptTurn` drops one queued input.

### One turn's events

`Turn.Events()` yields the events of one turn, from its first event to its `TurnCompleted`, so showing a reply
needs no filtering of the session stream. Every event still goes to `Session.Events()` too.

```go
turn, err := sess.Send(ctx, agent.Text("Explain this repository."))
for ev := range turn.Events() {
	if d, ok := ev.(*agent.TextDelta); ok {
		fmt.Print(d.Delta)
	}
}
```

### Requests: events or callbacks

```go
cfg.Handlers.Permission = func(ctx context.Context, r *agent.PermissionRequest) agent.PermissionResponse {
	if r.Kind == agent.PermissionCommand && strings.HasPrefix(r.Command, "rm ") {
		return agent.PermissionResponse{Decision: agent.DecisionDeny, Message: "no deletes"}
	}
	return agent.PermissionResponse{Decision: agent.DecisionAllow}
}
```

Without a handler the request arrives as an event, in `Session.Events()` and the turn's `Turn.Events()`; answer
it whenever you like (a UI can wait for a click). `Session.PendingRequests()` lists unanswered requests so a
reconnecting UI can show them again. An answer that cannot be used, such as one with an empty decision, is refused
with `agent.ErrInvalidAnswer`; a handler's unusable answer denies the request instead, so a mistake never allows
an action.

### Subagents and tasks

Work beside the main conversation is reported as `TaskUpdated` events carrying the full `agent.Task`: kind
(`subagent`, `shell`, `workflow`), status, description, progress summary, final output or error, tool-use count,
duration, usage, and for subagents an `agent.Subagent` (type, prompt, model, effort, background, depth). Items a
subagent produces carry `SubagentID` (the task ID) and `ParentID` (the item that spawned it), so a UI can nest a
subagent's transcript; permission, question, and elicitation requests a subagent makes carry its `SubagentID` too. Background subagents often finish after the turn that started them; their `TaskUpdated`
arrives outside the turn.

### Shared controls

Optional interfaces cover features most harnesses share; check with a type assertion (they have no
`Capabilities` flags):

```go
if f, ok := sess.(agent.Forker); ok {
	branch, err := f.ForkSession(ctx, agent.Config{})
	// ...
}
```

`agent.Titler` (`SetTitle`), `agent.Compacter` (`Compact`), `agent.Forker` (`ForkSession`; set `Config.ForkAtTurn` to
keep history only through one turn), `agent.Rewinder` (`Rewind` drops a turn and everything after it),
`agent.TaskStopper` (`StopTask`), `agent.RateLimitReader` (`RateLimits`), `agent.TurnInterrupter` (`InterruptTurn`
drops one queued input or interrupts one running turn; `Run` uses it when its context ends). Events with one shape everywhere:
`RateLimitUpdated`, `HookRun`, `GoalUpdated`, `PlanUpdated`, `PlanDelta`, `ItemsRemoved`, `ConversationReset`.

Providers also implement optional interfaces, found with a type assertion on the provider (there are no capability
flags for them): `agent.Checker` (`Check(ctx)` reports whether the CLI is installed, its version, and how it is
signed in, without starting a session), `agent.ModelLister`, `agent.SessionLister`, `agent.HistoryReader`,
`agent.SessionDeleter`, and `agent.SubagentLister` (the subagents a stored session ran; pass a task ID as
`ReadHistoryOptions.SubagentID` to read one subagent's transcript). History items carry `TurnID` and `Time`, so a
resumed session can `Rewind` to them.

```go
if lister, ok := provider.(agent.SessionLister); ok {
	sessions, err := lister.ListSessions(ctx, agent.ListSessionsOptions{Cwd: dir, Limit: 20})
	// ...
}
```

Listing and history page with `Limit` and `Offset`. Sessions come newest first. History offsets count back from the
newest item, so a chat view loads the latest page first and older pages after it:

```go
latest, _ := reader.ReadHistory(ctx, id, agent.ReadHistoryOptions{Limit: 50})
older, _ := reader.ReadHistory(ctx, id, agent.ReadHistoryOptions{Limit: 50, Offset: 50})
```

### Errors

Failures match sentinels with `errors.Is`: `agent.ErrUnsupported` (a feature the harness lacks),
`agent.ErrSessionNotFound` (a `Resume` target the harness does not have), `agent.ErrSteerRejected` (the input was
not injected and can be sent again), `agent.ErrClosed`, `agent.ErrCancelled` (queued input dropped before it
ran, such as by `InterruptTurn`), `agent.ErrRequestDone` (a request was already answered or withdrawn), `agent.ErrInvalidAnswer` (an
empty or unknown permission decision or elicitation action; nothing was sent), and `agent.ErrInterrupted` (the
turn was interrupted). A turn that ran but failed or was interrupted makes `Run` and `Turn.Wait` return both the
result and its `*agent.TurnError`, which carries the native code, HTTP status, whether a retry may succeed, and when
an exhausted usage limit resets:

```go
res, err := sess.Run(ctx, agent.Text("Fix the build"))
var te *agent.TurnError
switch {
case errors.Is(err, agent.ErrInterrupted):
	// res holds what the turn produced before it stopped.
case errors.As(err, &te) && te.Retryable:
	// Send again after te.ResetsAt.
case err != nil && res == nil:
	// The turn never ran, for example because the session closed.
}
```

A request's `Outcome()` (`RequestAnswered`, `RequestWithdrawn`, or `RequestPending`) tells how it ended; the
`RequestResolved` event carries the same value. `Check` reports harness problems (missing executable, unsupported
version, failed sign-in check) in `ProviderStatus.Err` and returns an error only when the check could not run.
An error answer from the harness to a request is an `*agent.ProtocolError` with the numeric `Code`, the harness's
error class in `ErrorCode`, and any JSON detail in `Data`.

`Start` fails when `Config.ProviderOptions` holds another provider's options, and `Send` fails when
`Input.ProviderOptions` does. `Config` fields a harness cannot honor
are listed in a `Notice` with Code `"config_ignored"` right after `SessionStarted`; set `Config.Strict` to make
`Start` fail instead. A restriction a harness cannot enforce, such as `DisallowedTools` on Codex, always fails
`Start`.

### Host tools

```go
type ticketArgs struct {
	ID string `json:"id"`
}
cfg.Tools = []agent.Tool{agent.NewTool("lookup_ticket", "Fetches a ticket by ID.",
	json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	func(ctx context.Context, args ticketArgs) (agent.ToolResult, error) {
		return agent.JSONResult(fetchTicket(args.ID))
	})}
```

`NewTool` decodes the arguments into the type you name; arguments that do not decode reach the model as an error
result. Fill `agent.Tool` directly to handle the raw `ToolCallRequest` or to set fields such as `NeedsApproval`.

Served as an in-process MCP server for Claude, as dynamic tools for Codex, through a bridge extension for Pi, and as
an in-process HTTP MCP server for OpenCode.

## Provider matrix

| Feature | Claude | Codex | Pi | OpenCode |
| --- | --- | --- | --- | --- |
| Streaming text / reasoning | ✓ | ✓ | ✓ | ✓ |
| Steer / queue / interrupt | ✓ native priorities | ✓ `turn/steer`, library queue | ✓ `steer`, library queue | ✓ steer delivery, library queue |
| Permission prompts | ✓ `can_use_tool` | ✓ command, file, grant approvals | ✓ via bridge gate | ✓ `permission.asked` |
| Permission modes | default, acceptEdits, plan, bypass, dontAsk, auto | all, mapped to approval policy + sandbox | via bridge | default, acceptEdits, plan (`plan` agent), readOnly, bypass (allows what the agent and config do not deny), as session permission rules; runs the server's default agent unless `SessionOptions.Agent` is set |
| Questions to the user | ✓ AskUserQuestion | ✓ `requestUserInput` | ✓ extension dialogs | ✓ question forms |
| MCP elicitation | ✓ | ✓ | – | – |
| Host tools | ✓ in-process MCP | ✓ dynamic tools | ✓ bridge | ✓ in-process HTTP MCP |
| MCP servers | ✓ | ✓ | ✓ via bridge | ✓ via the server API |
| Output schema (`Config.OutputSchema`, per-turn `Input.OutputSchema`) | session-wide only | ✓ both | ✓ both, emulated with a schema-validated tool (bridge) | – (ignored with a notice) |
| Cost reporting | ✓ | – (tokens only) | ✓ | ✓ |
| Resume / fork | ✓ | ✓ | ✓ | ✓ |
| List models / sessions / history | ✓ | ✓ | ✓ | ✓ |
| Delete sessions | ✓ transcript files | ✓ `thread/delete` | ✓ session file | ✓ |
| Subagents | ✓ tasks + nested items | ✓ child threads as tasks + nested items | ✓ tasks from the `subagent` tool or a subagent extension (e.g. pi-subagents) | ✓ child sessions as tasks + nested items |
| Plan / todo list | ✓ TodoWrite, TaskCreate/TaskUpdate | ✓ `turn/plan/updated`, `PlanDelta` | ✓ with a todo extension (Pi's example `todo`, rpiv-todo) | – (plan mode via the `plan` agent) |
| Rate limits / hooks / goals | ✓ / ✓ / ✓ | ✓ / ✓ / ✓ | – | – |
| SetTitle / Compact / ForkSession | ✓ / ✓ / ✓ | ✓ / ✓ (no instructions) / ✓ | ✓ / ✓ / ✓ | ✓ / ✓ (no instructions) / ✓ |
| ForkAtTurn / Rewind | ✓ `--resume-session-at` / ✓ native `rewind_conversation` (process restart as fallback) | ✓ `lastTurnId` / ✓ `thread/revert` (fork with a new ID for legacy threads) | ✓ fork at entry / ✓ in place via the bridge (fork with a new session ID without it) | ✓ fork before the next prompt / ✓ in place via `revert` |
| StopTask | ✓ | ✓ (subagent threads) | – | ✓ (subagent sessions) |
| InterruptTurn (one turn) | ✓ `cancel_async_message`, else interrupt | ✓ | ✓ | ✓ |
| RateLimits on demand | ✓ `get_usage` | ✓ `account/rateLimits/read` | – | – |
| Check (install, version, auth) | ✓ | ✓ | ✓ (auth = has a usable model) | ✓ (auth = has a usable model) |

`Capabilities()` reports the flag-backed features at run time (a session's can be narrower than its provider's,
such as Pi without its bridge extension), and a type assertion finds the optional methods
(SetTitle, Compact, ForkSession, Rewind, StopTask, RateLimits, InterruptTurn); unsupported calls return errors wrapping
`agent.ErrUnsupported`.

## Every native feature, too

The uniform API never hides a harness feature:

- **Concrete session types.** `*claude.Session`, `*codex.Session`, `*pi.Session`, and `*opencode.Session` expose
  every native control:
  Claude MCP management, file rewind, context usage, background tasks, settings layers, side questions, title
  generation, hooks; Codex review, compaction, rewind, goals, shell commands, item injection, fork, archive, and
  the full app-server client; Pi follow-up queue, session tree, fork, clone, bash, stats, HTML export;
  OpenCode agents, permission rules, instructions, inbox, shell, diff, export.
- **Escape hatches for new protocol features.** `claude.Session.Control(ctx, subtype, payload)`,
  `codex.Client.Call(ctx, method, params, result)`, `pi.Session.Command(ctx, type, payload)`, and
  `opencode.Session.Call(ctx, method, path, body, out)` send any
  request, including ones added to a harness after this library was released.
- **Raw messages.** Every event built from a native message keeps it in `Meta().Raw`. Native messages without a
  uniform meaning arrive as `*agent.RawEvent` or provider events (`*codex.DiffUpdated`, `*pi.QueueUpdated`, …). `Config.WireTap` sees every protocol line.
- **Provider options.** `Config.ProviderOptions` takes `*claude.SessionOptions`, `*codex.SessionOptions`,
  `*pi.SessionOptions`, or `*opencode.SessionOptions` for settings with no uniform equivalent. For settings newer
  than the library, Claude and Pi take `ExtraArgs` (and Claude `InitializeExtra`), Codex `ExtraThreadParams` and
  `ExtraTurnParams`, and OpenCode `Config`; every provider's `Options` also has `ExtraArgs`.

## Daemons and orchestrators

- `codex.Options{SharedServer: true}` multiplexes every Codex session over one app-server process, which runs
  until `Provider.Close`; `opencode.Options{SharedServer: true}` does the same for OpenCode, whose shared server
  stops with its last session. `opencode.Options{ServerURL: ...}` attaches to a running OpenCode server (host tools there need `AllowHostToolsOnAttachedServer`, because other clients'
  sessions in the same directory could call them).
- `agent.Spawner` runs harnesses anywhere: containers, remote hosts, sandboxes.
- `agent.Registry` picks a provider by name at run time.
- `Config.UnsetEnv` removes inherited variables, such as the ones that mark this process as running inside another
  agent session; the Claude provider removes those by default.
- Sessions never block the protocol reader: events queue without limit until read, or up to
  `Config.EventBufferLimit` with an `EventsDropped` marker.

## Adding a harness

Implement `agent.Provider` and `agent.Session` on top of `providerkit`: `providerkit.Runtime` handles event
delivery, turn tracking, request bookkeeping, and handlers, and for harnesses that run one turn at a time it also
runs the input queue, the steer gate, and interrupt targeting (`SetStarter`, `StartIdle`, `Enqueue`, `Accepted`,
`RunStarted`, `Interrupt`); `providerkit.Process` handles the subprocess, line framing, stderr capture, and
graceful shutdown. `providerkit` also has what only implementations need: request constructors
(`NewPermissionRequest`, `NewUserInputRequest`, `NewElicitationRequest`), `WithdrawRequest`, `Unsupported`,
`SessionOptions` and `TurnOptions` (read `Config.ProviderOptions` and `Input.ProviderOptions`), `CheckInput`
(refuses an unknown `Delivery` or `Part.Type`), `CheckUpdatedInput` (for harnesses that cannot replace a call's
arguments), `IgnoredConfig` (for fields you cannot honor; refuse a restriction you cannot enforce with
`Unsupported` instead), `CallTool`, `VersionStatus` and `CheckFailed` for `Check`, and environment helpers.

## Examples

| Example | Shows |
| --- | --- |
| `examples/oneshot` | The smallest program: start, send, stream the turn's events, wait, close. |
| `examples/chat` | An explorer for the whole API in a terminal REPL. |
| `examples/daemon` | Sessions served over HTTP, with Server-Sent Events, replay on reconnect, and requests answered over HTTP. |
| `examples/orchestrator` | A planner splits a task with structured output, workers on several harnesses run in parallel, and the planner merges their reports. |

```sh
go run ./examples/oneshot -provider codex "Summarize this repository"
git diff | go run ./examples/oneshot "Review this diff:"

go run ./examples/chat -provider claude
go run ./examples/chat -provider codex -prompt "Summarize this repository" -yes

go run ./examples/daemon -addr 127.0.0.1:8080
go run ./examples/orchestrator -planner claude -workers claude,codex "Where is error handling inconsistent?"
```

The daemon has no authentication and runs agents with your credentials. It listens on loopback; its doc comment lists
the endpoints with `curl` calls.

`examples/chat`: type to send (input typed during a turn is queued); `/steer` and `/now` use the other deliveries, and
Ctrl+C interrupts. Requests are answered with `y`/`n`, `/allow`, `/deny`, `/answer`, `/accept`, and `/decline`. Input
beyond text uses `/image`, `/file`, `/skill`, `/json` (a JSON Schema for structured output), and `/with` (a model or
effort from this turn on). Other commands cover the session (`/info`, `/caps`, `/model`, `/effort`, `/mode`, `/turns`,
`/fork`, `/rewind`, `/compact`, `/title`, `/tasks`, `/stoptask`, `/limits`, `/transcript`, `/usage`), the provider
(`/check`, `/models`, `/sessions`, `/history`, `/subagents`, `/delete`), switching (`/new`, `/resume`, `/provider`),
every event as JSON (`/events on`, `/events TaskUpdated,PlanUpdated`, or `-events`), and raw protocol lines (`/wire`,
or `-wire file`). Commands that choose something list the choices when given no argument, and a number picks one
(`/model 3`, `/resume 1`, `/rewind 2`). The harness's own slash commands (`/commands`) are sent as typed, and `//name`
forces one through. Flags set the rest of `Config`: `-system`, `-append-system`, `-schema`, `-mcp`, `-allow-tools`,
`-deny-tools`, `-add-dir`, `-title`, `-session-id`, `-fork`, `-fork-at`, `-strict`, and `-buffer`. `/help` and
`-help` list them all.

## Testing

`agenttest` is a scripted fake provider for testing code that uses this library, with no CLI or model:

```go
p := agenttest.New(func(t *agenttest.Turn) error {
	if t.Permission(agent.PermissionRequest{ToolName: "bash", Command: "ls"}).Decision == agent.DecisionAllow {
		t.ToolCall(agent.ToolShell, "bash", `{"command":"ls"}`, "main.go")
	}
	t.Text("There is one file.")
	return nil
})
sess, _ := p.Start(ctx, agent.Config{})
```

The library's own tests:

```sh
go test ./... -race                       # protocol tests against scripted fake harnesses
go test -tags live ./claude ./codex ./pi ./opencode  # real CLIs; spends tokens
```

Live tests delete the sessions they create (`internal/livetest`), so they leave nothing in the harnesses' session
stores.
