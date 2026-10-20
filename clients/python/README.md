# agentd Python client

An asyncio client for [agentd](../../README.md#other-languages-agentd), using only the standard library (Python 3.11+).
Types in `agentd/protocol.py` are generated from `cmd/agentd/protocol.schema.json` by `go generate ./cmd/agentd`.

```sh
pip install ./clients/python
```

```python
import asyncio
from agentd import Agentd

async def main():
    async with await Agentd.start("agentd", providers={"claude": {}}) as agentd:
        async def permission(params):
            print("allowing", params["Request"]["ToolName"])
            return {"Decision": "allow"}

        agentd.handle("handler/permission", permission)
        session = await agentd.start_session(
            {"Provider": "claude", "Config": {"Cwd": "."}, "Handlers": {"Permission": True}})
        result = await session.run("Add a test")
        print(result["Text"])

asyncio.run(main())
```

- `Agentd.call(method, params)` sends any method; cancelling the awaiting task sends `$/cancelRequest`.
- `Agentd.on(method, callback)` subscribes to notifications; `Agentd.handle(method, handler)` answers reverse
  requests. An unhandled reverse request gets error -32601, which agentd reads as deny, cancel, or no decision.
- `Session.run` raises `TurnFailed` when the turn has an `Error`, has no `Result`, or its `Result.Status` is
  `failed`. An interrupted turn returns its `Result`.
- `Session.events()` subscribes when called, so `it = session.events(); await session.send(...)` misses
  nothing. Close the iterator with `aclose()` to unsubscribe before the session closes.
- Cancelling the task awaiting `Session.run` after the turn started sends `session/interruptTurn` without
  waiting for its reply, then raises `CancelledError`.
- When agentd exits, pending calls and runs raise `AgentdError` with `kind == "closed"`. So do runs whose
  session closes before their turn's result arrives.
- `Agentd.close()` closes agentd's stdin and kills agentd if it has not exited 5 seconds later
  (`agentd.client.CLOSE_TIMEOUT`).

## Tests

The tests build `internal/agentd/fakeagentd`, so they need Go.

```sh
cd clients/python && python3 -m unittest discover -s tests -v
```
