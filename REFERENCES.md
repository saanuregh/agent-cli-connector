# References

The versions this library was designed and verified against. Update this file when re-checking parity or
re-running the live tests against newer releases.

## Harness CLIs (live tests)

| Harness | Version | Protocol source used |
| --- | --- | --- |
| Claude Code | 2.1.306 | stream-json control protocol |
| Codex | codex-cli 0.163.0 | `codex app-server generate-json-schema --experimental` |
| Pi | 1.3.0 | RPC mode docs shipped with Pi (`docs/rpc.md`, `rpc-commands.md`, `json.md`, `extensions.md`, `mcp.md`, `session-format.md`) |
| @tintinweb/pi-subagents (Pi extension) | 0.22.0 | `subagents:*` event bus, used by the Pi subagent live test |

Zig toolchain: 0.17.0. Bindings: Go 1.26, Rust 1.99, Node 24.

Hand-check models: Claude `haiku`, the Codex account default at low effort, and Pi `openrouter/~deepseek/deepseek-flash-latest` (a bare `deepseek/deepseek-flash` is ambiguous in Pi: it can resolve to the direct `deepseek` provider, which needs its own API key).

## Reference implementations (parity audits)

| Project | Version | What was compared |
| --- | --- | --- |
| [t3code](https://github.com/pingdotgg/t3code) | `v0.1.1-nightly.20261016.3218` (commit `e58a4857`); harness changes re-checked through `v0.1.1-nightly.20261017.3238` | Claude, Codex, and Pi adapters and provider services |
| [paseo](https://github.com/getpaseo/paseo) | `main` at commit `30122b1` (2026-10-16); harness changes re-checked through `fa70b21` | Claude, Codex, and Pi providers |
| [claude-agent-sdk-typescript](https://github.com/anthropics/claude-agent-sdk-typescript) | 0.3.302 | Every option, `Query` method, control request, and message type |

The projects above pin their own SDK versions, which were read for wire details: t3code uses
`@anthropic-ai/claude-agent-sdk` 0.3.276 and paseo uses 0.3.246.
