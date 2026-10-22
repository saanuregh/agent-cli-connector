# agent-cli-connector

One API for coding-agent CLIs: [Claude Code](https://github.com/anthropics/claude-agent-sdk-typescript),
[Codex](https://github.com/openai/codex), and [Pi](https://github.com/earendil-works/pi). Use it to build chat apps,
daemons, and orchestrators without learning three protocols. The library is written in Zig and runs inside your
program: from Zig directly, and from Go, Rust, and TypeScript through bindings over its C ABI.

```zig
var diag: agent.Diagnostics = .{}; // optional: receives error details
const provider = try agent.registry.create(env, "claude", null, &diag);
defer provider.deinit();
const session = try provider.start(.{ .config = .{ .cwd = dir } }, &diag);
defer session.deinit();

var turn = try session.send(.{ .text = "Add a test for parseConfig" }, &diag);
while (turn.nextEvent(.none)) |owned| {
    defer owned.destroy();
    switch (owned.event.payload) {
        .text_delta => |d| std.debug.print("{s}", .{d.delta}),
        .permission_request => |r| try session.respond(r.id, .{ .permission = .{ .decision = .allow } }, &diag),
        else => {},
    }
} else |_| {} // error.EndOfStream after the turn's turn_completed
const result = try turn.wait(arena, .none, &diag);
```

```go
client, _ := agent.Open(ctx)
sess, _ := client.Start(ctx, "claude", agent.Config{Cwd: dir},
	agent.WithHandlers(agent.Handlers{Permission: func(ctx context.Context, r *agent.PermissionRequest) agent.PermissionResponse {
		return agent.PermissionResponse{Decision: agent.PermissionDecisionAllow}
	}}))
res, err := sess.Run(ctx, agent.Text("Add a test for parseConfig"))
```

```rust
let client = Client::open(Options::default()).await?;
let session = client.start("claude", Config { cwd: dir.into(), ..Default::default() }).await?;
let result = session.run("Add a test for parseConfig").await?;
```

```ts
await using client = await Client.open();
const session = await client.start("claude", { cwd: dir, onPermission: async () => "allow" });
for await (const ev of (await session.send("Add a test for parseConfig")).events()) {
  if (ev.type === "text_delta") process.stdout.write(ev.delta);
}
```

Swap `"claude"` for `"codex"` or `"pi"` and nothing else changes.

## Build

Zig 0.17.0, on Linux, macOS, or Windows (Windows is tested under Wine). The harness CLIs must be installed (`claude`, `codex`, `pi`); [REFERENCES.md](REFERENCES.md) lists the
versions this library was verified against.

With Nix, `nix develop` gives a shell with Zig, Go, Rust, and Node at the versions CI uses, plus their language
servers (`flake.nix`).

```sh
zig build -Doptimize=ReleaseFast     # zig-out/lib (static + shared), zig-out/include/agent.h, zig-out/bin/agentd
zig build test                       # tests with scripted fake harnesses; no CLI or tokens needed
zig build -Dproviders=claude,pi      # leave providers out
zig build release                    # every platform's files into zig-out/release
```

### Prebuilt libraries

The bindings do not need a local Zig build: they fetch the library from the GitHub release for their version
(`v0.2.0` for 0.2.0), into the user cache, and check it against the release's `SHA256SUMS`. Releases carry the shared
and static library and `agentd` for `linux-x86_64`, `linux-aarch64` (glibc 2.28 or newer), `macos-x86_64`,
`macos-aarch64`, and `windows-x86_64`. Pushing a tag `v<version>` that matches `library_version` builds and
publishes them (`.github/workflows/release.yml`).

| Variable | Effect |
| --- | --- |
| `AGENT_CLI_CONNECTOR_LIB` | Shared library to load (Go, TypeScript). |
| `AGENT_CLI_CONNECTOR_LIB_DIR` | Directory with the static library to link (Rust). |
| `AGENTD_PATH` | `agentd` executable (TypeScript, Rust tests). |
| `AGENT_CLI_CONNECTOR_RELEASE_URL` | Base URL to download from instead of the GitHub release, such as a mirror. |
| `AGENT_CLI_CONNECTOR_NO_DOWNLOAD=1` | Never download; fail with what to set instead. |

| Language | Package | Links |
| --- | --- | --- |
| Zig | `build.zig.zon` dependency, module `agent` | from source |
| Go | `bindings/go` (`github.com/saanuregh/agent-cli-connector/bindings/go`) | the shared library through purego, without cgo: `AGENT_CLI_CONNECTOR_LIB`, else the user cache, else a download from the matching GitHub release checked against `SHA256SUMS` (`AGENT_CLI_CONNECTOR_NO_DOWNLOAD=1` turns it off) |
| Rust | `bindings/rust` (crate `agent-cli-connector`) | the static library from `AGENT_CLI_CONNECTOR_LIB_DIR`, then `zig-out/lib`, then `zig build` when Zig is on `PATH` in a repository checkout, then a download from the GitHub release for the crate version, checked against `SHA256SUMS` (`AGENT_CLI_CONNECTOR_RELEASE_URL` replaces the base URL; `AGENT_CLI_CONNECTOR_NO_DOWNLOAD=1` turns it off) |
| TypeScript | `bindings/typescript` (package `agent-cli-connector`, Node 24) | the shared library through `koffi` (`AGENT_CLI_CONNECTOR_LIB`), or the `agentd` subprocess (`AGENTD_PATH`); without a local build, `Client.open` downloads both from the matching GitHub release into the user cache, checked against `SHA256SUMS` (`AGENT_CLI_CONNECTOR_NO_DOWNLOAD=1` turns this off) |

Any other language can load the shared library (`include/agent.h`) or run `agentd`, which speaks the same protocol
over standard input and output, one JSON message per line.

## Concepts

| Concept | Zig | Purpose |
| --- | --- | --- |
| Provider | `agent.Provider` | Starts sessions for one harness; `check`, `listModels`, `listSessions`, `readHistory`, `deleteSession`, `listSubagents`. |
| Session | `*agent.Session` | One live conversation: `send`, `run`, `wait`, `interrupt`, `interruptTurn`, `setModel`, `setEffort`, `setPermissionMode`, `setTitle`, `compact`, `rewind`, `fork`, `stopTask`, `rateLimits`, `call`, `respond`, `close`. |
| Input | `types.Input` | Text and parts (images, files, skills, mentions), per-turn model/effort/output schema, delivery mode. |
| Turn | `agent.Turn`, which `send` returns | `nextEvent` streams the turn's own events; `wait` returns a `TurnResult`: status, final text, structured output, usage, cost, items, error; `interrupt`. |
| Events | `types.Event` | Typed stream: text/reasoning/tool deltas, transcript items, plans, tasks, usage, compaction, retries, notices, requests. |
| Requests | `permission_request`, `user_input_request`, `elicitation_request` events | Answered with `respond`, or by a `StartOptions.request_handler` callback instead of an event. The bindings add handler callbacks and `allow`/`deny`/`answer`/`accept`/`decline` on the request itself. |
| Host tools | `Config.tools` + `StartOptions.tool_handler` | Functions of your program the model can call, on every harness. Interrupting a turn cancels its running calls (Go: the handler's `ctx`; TypeScript: an `AbortSignal`; Rust: the `CancelToken` passed to the `Tool::new` handler). |
| Capabilities | `types.Capabilities` | One flag per feature; an unsupported call fails with `unsupported`. |

### Sending while a turn runs

`Input.delivery` decides: `queue` (the default) runs the input after the turn, `steer` injects it into the running
turn, `interrupt` stops the turn and runs it next. Each provider maps these to native mechanisms (Claude message
priorities, Codex `turn/steer`, Pi `steer`). `interrupt` stops the running turn and leaves queued input to run
afterwards; `interruptTurn` stops or drops one turn.

### Turn IDs

`send` returns the turn's ID before the harness sees the input. Every event of the turn and its `TurnResult` carry that
ID, even when the harness gives the turn its own ID (that one is in `TurnResult.native_turn_id`). A turn that never
runs, because it was cancelled or the session closed, still ends with a failed `turn_completed`.

### Errors

Fallible calls return an error named after its kind (`error.Unsupported`, `error.Closed`, `error.SteerRejected`, ...)
and take an optional `*agent.Diagnostics` as their last argument, the pattern `std.zon` uses: pass one to receive the
details as an `ErrorInfo` (`Diagnostics.of(&diag, err)`): `kind`, `message`, the native `code`, `rpc_code`,
`http_status`, `retryable`, `resets_at_ms`, the harness's `stderr` tail, and `data`. Pass `null` to skip them. The
details travel with the call, so they are right with any `std.Io` implementation, including ones that move tasks
between threads. The bindings raise
one error type carrying the same `ErrorInfo`. A turn that ran but failed or was interrupted is not an error: its
`TurnResult.status` says so and `TurnResult.error` has the details.

`Config` fields a harness cannot honor are listed in a `notice` with code `config_ignored` right after
`session_started`; set `Config.strict` to make `start` fail instead. A restriction a harness cannot enforce, such as
`disallowed_tools` on Codex, always fails `start`.

## Provider matrix

| Feature | Claude | Codex | Pi |
| --- | --- | --- | --- |
| Streaming text / reasoning | ✓ | ✓ | ✓ |
| Steer / queue / interrupt | ✓ native priorities | ✓ `turn/steer`, library queue | ✓ `steer`, library queue |
| Permission prompts | ✓ `can_use_tool` | ✓ command, file, grant approvals | ✓ via bridge gate |
| Permission modes | default, accept_edits, plan, bypass, dont_ask, auto | all, mapped to approval policy + sandbox | via bridge |
| Questions to the user | ✓ AskUserQuestion | ✓ `requestUserInput` | ✓ extension dialogs |
| MCP elicitation | ✓ | ✓ | – |
| Host tools | ✓ in-process MCP | ✓ dynamic tools | ✓ bridge |
| MCP servers | ✓ | ✓ | ✓ via bridge |
| Output schema (session / per turn) | session-wide only | ✓ both | ✓ both, emulated with a schema-validated tool |
| Cost reporting | ✓ | – (tokens only) | ✓ |
| Resume / fork / fork at turn | ✓ | ✓ | ✓ |
| Rewind | ✓ native `rewind_conversation` | ✓ `thread/revert` (fork for legacy threads) | ✓ in place via the bridge |
| List models / sessions / history, delete sessions | ✓ | ✓ | ✓ |
| Subagents | ✓ tasks + nested items | ✓ child threads as tasks + nested items | ✓ from the `subagent` tool or a subagent extension |
| Plan / todo list | ✓ | ✓ plus `plan_delta` | ✓ with a todo extension |
| Rate limits / hooks / goals | ✓ / ✓ / ✓ | ✓ / ✓ / ✓ | – |
| Set title / compact / stop task | ✓ / ✓ / ✓ | ✓ / ✓ (no instructions) / ✓ (subagent threads) | ✓ / ✓ / – |
| Check (install, version, auth) | ✓ | ✓ | ✓ (auth = has a usable model) |

`Capabilities` reports these at run time; a session's can be narrower than its provider's (such as Pi without its
bridge extension).

## Every native feature, too

- **`session.call(method, params)`** sends any native request and returns the answer: a Claude control request
  subtype (`get_context_usage`, `mcp_status`, ...), a Codex app-server method (`thread/read`, `review/start`, ...),
  or a Pi RPC command (`get_session_stats`, `bash`, ...), including ones added after this library was released.
- **Native events.** Native messages without a uniform meaning arrive as `native` events named
  `<provider>/<name>`, such as `codex/diff_updated` or `pi/queue_updated`. With `Config.include_raw`, every event keeps
  the native message in `raw`; `Config.wire_tap` emits every protocol line as a `wire` event.
- **Provider options.** `Config.provider_options` and `Input.provider_options` take each provider's `SessionOptions`
  and `TurnOptions` (see `src/providers/<name>/options.zig`), and the provider-level options are given to
  `registry.create` or the protocol's `initialize`. The bindings have them as types: `ClaudeOptions`,
  `ClaudeSessionOptions`, `ClaudeTurnOptions`, `CodexOptions`, `CodexSessionOptions`, `CodexTurnOptions`, `PiOptions`,
  `PiSessionOptions`.
- **Named settings.** Effort and permission mode are strings, so harness-native values pass through. The bindings
  type them (`Effort`, `PermissionMode`) with constants for the named values: Go `agent.EffortHigh`, Rust
  `Effort::HIGH`, TypeScript `Effort.high`.

## The protocol

The C ABI (`include/agent.h`: `agent_open`, `agent_send`, `agent_recv`, `agent_recv_batch`, `agent_free`,
`agent_close`, `agent_destroy`) and `agentd` carry JSON-RPC 2.0 messages. `schema/protocol.schema.json` describes
every message; the bindings' `protocol` and `methods` files are generated from the Zig types (`zig build bindgen`).
Clients name new sessions (`handle` in `session/start` and `session/fork`) and turns (`turn_id` in `session/send`)
themselves, so they can route events that arrive before the reply; the bindings do this.

- Requests: `initialize`, `provider/{check,listModels,listSessions,readHistory,deleteSession,listSubagents}`,
  `session/{start,fork,send,wait,info,interrupt,interruptTurn,setModel,setEffort,setPermissionMode,setTitle,compact,rewind,stopTask,rateLimits,pendingRequests,call,close}`,
  `request/respond`.
- Notifications: `session/event` (`{session, event}`), `session/closed` (`{session, error}`), `tool/cancel`
  (`{session, call_id}`: the turn running that tool call was interrupted or the session closed; the library no longer
  waits for its result).
- Requests from the library: `tool/call` (`{session, call}`, answered with a `ToolResult`).
- Wire format: snake_case fields; empty strings, empty lists, `false`, and null are left out; unions carry `"type"`;
  times are Unix milliseconds; binary data is base64. Failures are JSON-RPC errors whose `data` is an `ErrorInfo`.
- Requests that change a session run in the order they arrived; `session/wait`, `session/info`,
  `session/pendingRequests`, `session/interrupt`, `session/interruptTurn`, `session/close`, and `request/respond`
  run at once.

## Examples

| Example | Language | Shows |
| --- | --- | --- |
| `examples/oneshot` | Zig | The smallest program: start, send, stream the turn's events, wait, close. |
| `examples/chat` | Go | An explorer for the whole API in a terminal REPL. |
| `examples/daemon` | Go | Sessions served over HTTP, with Server-Sent Events and requests answered over HTTP. |
| `examples/orchestrator` | TypeScript | A planner splits a task, workers on several harnesses run in parallel, the planner merges their reports. |
| `bindings/rust/examples/basic.rs` | Rust | oneshot in async Rust, on tokio. |

```sh
zig build -Doptimize=ReleaseFast
./zig-out/bin/oneshot --provider codex "Summarize this repository"
git diff | ./zig-out/bin/oneshot "Review this diff:"

cd examples && go run ./chat -provider claude
cd examples && go run ./daemon -addr 127.0.0.1:8080
cd bindings/typescript && npm install && cd ../../examples/orchestrator && npm install && node main.ts --planner claude --workers claude,codex "Where is error handling inconsistent?"
cd bindings/rust && cargo run --example basic -- claude "Summarize this repository"
```

The daemon runs agents with your credentials. Loopback does not keep out other local users or web pages in your
browser, so every request needs the bearer token it prints at startup, and it refuses foreign `Host` and any `Origin`
header.

## Testing

The `fake` provider (`agent.fake`, or `{"providers": {"fake": {}}}` in `initialize`) is a scripted in-process harness
for testing code that uses this library, with no CLI or model: it streams the prompt back, asks for permission when the
prompt contains "permission", and calls host tool `<name>` for "tool:<name>".

```sh
zig build test --summary all                    # library and providers, against scripted fake harnesses
CODEX_LIVE=1 zig build test-codex               # live Codex check; spends tokens
zig build test -Dtarget=x86_64-windows -fwine   # the Windows build, under Wine
cd bindings/go && go test -race ./...           # bindings, against the fake provider
cd bindings/rust && cargo test
cd bindings/typescript && node --test
```

## License

MIT. See [LICENSE](LICENSE).
