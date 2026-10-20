# agent-cli-connector for TypeScript

A Node client for [agentd](../../README.md#other-languages-agentd), the JSON-RPC server that runs
coding-agent CLIs. It has no runtime dependencies and runs as TypeScript directly on Node 23.6 or later
(type stripping), so there is no build step. The package is not published.

## Install

Install agentd, then add this directory as a dependency:

```sh
go install github.com/saanuregh/agent-cli-connector/cmd/agentd@latest
npm install /path/to/agent-cli-connector/clients/typescript
# or from git; npm cannot install a subdirectory, so clone and install from the path above
```

## Example

```ts
import { Agentd } from "agent-cli-connector";

const agentd = await Agentd.start({ providers: { claude: {} } });
agentd.handle("handler/permission", async ({ Request }) => {
  console.log("allowing", Request?.ToolName);
  return { Decision: "allow" };
});
const session = await agentd.startSession({
  Provider: "claude",
  Config: { Cwd: process.cwd() },
  Handlers: { Permission: true },
});
const result = await session.run("Summarize this repository in one sentence");
console.log(result.Text);
await agentd.close();
```

- `agentd.call(method, params, { signal })` sends any method in `Methods`; aborting sends `$/cancelRequest`.
- `agentd.on(notification, callback)` subscribes and returns an unsubscribe function.
- `agentd.handle(reverseRequest, fn)` answers agentd's requests; a thrown error replies with an error,
  and unhandled requests get a method-not-found error. Either means deny, cancel, or no decision.
- `session.events()` yields the session's events from the time of the call; `session.send` starts a turn
  without waiting; `session.run` waits for its `turn/result`.
- `session.run(input, { signal })`: aborting after the turn started sends `session/interruptTurn` and
  rejects with the signal's reason without waiting for the turn to end. When the session closes or agentd
  exits before the turn's result arrives, `run` rejects with `info.Kind` `"closed"`.
- `agentd.close()` closes agentd's standard input and kills agentd if it has not exited five seconds later.
- Errors are `AgentdError` with `code` (JSON-RPC) and `info` (`ErrorInfo`, such as `info.Kind`).

`src/protocol.ts` is generated from `cmd/agentd/protocol.schema.json` by `go generate ./cmd/agentd`;
do not edit it.

## Check and test

The tests build `internal/agentd/fakeagentd` with Go, so Go must be installed.

```sh
npm install
npm run check   # tsc --noEmit
npm test        # node --test
```
