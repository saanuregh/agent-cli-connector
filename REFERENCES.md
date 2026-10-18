# References

The versions this library was designed and verified against. Update this file when re-checking parity or
re-running the live tests against newer releases.

## Harness CLIs (live tests)

| Harness | Version | Protocol source used |
| --- | --- | --- |
| Claude Code | 2.1.301 | stream-json control protocol; the library reports itself as the SDK version in `claude.ProtocolVersion` |
| Codex | codex-cli 0.163.0 | `codex app-server generate-json-schema --experimental` |
| Pi | 1.3.0 | RPC mode docs shipped with Pi (`docs/rpc.md`, `rpc-commands.md`, `json.md`, `extensions.md`, `mcp.md`, `session-format.md`) |
| @tintinweb/pi-subagents (Pi extension) | 0.22.0 | `subagents:*` event bus, used by the Pi subagent live test |
| OpenCode (`@opencode/cli`) | 2.0.32 | `/openapi.json` of `opencode serve`; `@opencode/client` and `@opencode/protocol` 2.0.32 (npm) |

Go toolchain: go1.26.8.

Live-test models: Claude `haiku` (`CLAUDE_TEST_MODEL`), the Codex account default at low effort (`CODEX_TEST_MODEL`,
`CODEX_TEST_EFFORT`), Pi `deepseek/deepseek-flash` (`PI_TEST_MODEL`; Pi's default OpenRouter model was
rate-limited upstream during the last run), and OpenCode `opencode/deepseek-v4.1-flash-free`
(`OPENCODE_TEST_MODEL`; a free model that needs no sign-in).

## Reference implementations (parity audits)

| Project | Version | What was compared |
| --- | --- | --- |
| [t3code](https://github.com/pingdotgg/t3code) | `v0.1.1-nightly.20261016.3218` (commit `e58a4857`); harness changes re-checked through `v0.1.1-nightly.20261017.3238` | Claude, Codex, and Pi adapters and provider services |
| [paseo](https://github.com/getpaseo/paseo) | `main` at commit `30122b1` (2026-10-16); harness changes re-checked through `fa70b21` | Claude, Codex, and Pi providers |
| [t3code](https://github.com/pingdotgg/t3code) (OpenCode) | same commit | `packages/provider-opencode/src/server/v2/{OpenCode2Server,OpenCode2Client,adapter}.ts`, `../{OpenCodeRuntime,OpenCodeServerOwner,toolItems}.ts`, `docs/user/providers-opencode.md` |
| [paseo](https://github.com/getpaseo/paseo) (OpenCode) | same commit | `packages/server/src/server/agent/providers/opencode/v2/`, `docs/opencode-global-event-baseline.md` |
| [claude-agent-sdk-typescript](https://github.com/anthropics/claude-agent-sdk-typescript) | 0.3.302 | Every option, `Query` method, control request, and message type |

The projects above pin their own SDK versions, which were read for wire details: t3code uses
`@anthropic-ai/claude-agent-sdk` 0.3.276 and paseo uses 0.3.246. For OpenCode, t3code pins `@opencode/client`
2.0.28 and paseo 2.0.10.
