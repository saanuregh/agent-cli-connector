import { after, before, describe, test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { Agentd, AgentdError, type EventNotification, type AgentPermissionRequest } from "../src/index.ts";

const repoRoot = fileURLToPath(new URL("../../..", import.meta.url));
const buildDir = mkdtempSync(join(tmpdir(), "agentd-ts-"));
const fakeagentd = join(buildDir, "fakeagentd");

// A scripted agentd for wire conditions fakeagentd cannot produce. Its first argument is the mode.
const scriptedAgentd = join(buildDir, "scripted-agentd.mjs");
const scriptedAgentdSource = `
import { createInterface } from "node:readline";
const mode = process.argv[2];
const line = (m) => JSON.stringify(m) + "\\n";
const lines = createInterface({ input: process.stdin });
lines.on("line", (text) => {
  const m = JSON.parse(text);
  if (m.id === undefined) return;
  const reply = { jsonrpc: "2.0", id: m.id, result: {} };
  if (m.method === "initialize") reply.result = { ProtocolVersion: 1, Providers: [] };
  else if (m.method === "session/start") reply.result = { Session: "s1", Info: {}, Methods: [] };
  else if (m.method === "session/send") {
    reply.result = { TurnID: "t1" };
    if (mode === "closed-with-reply") {
      process.stdout.write(line(reply) + line({ jsonrpc: "2.0", method: "session/closed", params: { Session: "s1", Error: null } }));
      return;
    }
  } else if (m.method === "$/ping" && mode === "not-object") {
    process.stdout.write('null\\n[1]\\n"text"\\n{"jsonrpc":"2.0","method":"session/event","params":null}\\n');
  }
  process.stdout.write(line(reply));
});
lines.on("close", () => {
  if (mode === "ignore-eof") setTimeout(() => {}, 60_000);
});
`;

before(() => {
  execFileSync("go", ["build", "-o", fakeagentd, "./internal/agentd/fakeagentd"], { cwd: repoRoot, stdio: "inherit" });
  writeFileSync(scriptedAgentd, scriptedAgentdSource);
});
after(() => rmSync(buildDir, { recursive: true, force: true }));

const startScripted = (mode: string) => Agentd.start({ command: process.execPath, args: [scriptedAgentd, mode] });
const within = <T>(promise: Promise<T>, ms: number): Promise<T> =>
  Promise.race([
    promise,
    new Promise<never>((_, reject) => setTimeout(() => reject(new Error(`not settled within ${ms} ms`)), ms).unref()),
  ]);
const start = () => Agentd.start({ command: fakeagentd, providers: { fake: {} } });
const opts = { timeout: 20_000 };

describe("agentd client", () => {
  test("initialize and ping", opts, async () => {
    const agentd = await start();
    try {
      assert.equal(agentd.initializeResult.ProtocolVersion, 1);
      assert.ok(agentd.initializeResult.Providers?.some((p) => p.Name === "fake"));
      assert.deepEqual(await agentd.call("$/ping", {}), {});
    } finally {
      await agentd.close();
    }
  });

  test("run echo; events arrive in Seq order before turn/result", opts, async () => {
    const agentd = await start();
    try {
      const session = await agentd.startSession({ Provider: "fake" });
      assert.ok(session.id);
      // Listeners run as each line is read, so they see the wire order.
      const wire: EventNotification[] = [];
      let completedAtResult = false;
      const offEvents = agentd.on("session/event", (e) => {
        if (e.Session === session.id) wire.push(e);
      });
      const offResult = agentd.on("turn/result", (n) => {
        if (n.Session === session.id) completedAtResult = wire.some((e) => e.Type === "TurnCompleted");
      });
      const iterated: EventNotification[] = [];
      const collecting = (async () => {
        for await (const e of session.events()) iterated.push(e);
      })();
      const result = await session.run("echo hi");
      assert.equal(result.Text, "echo: hi");
      offEvents();
      offResult();
      // The turn's events, TurnCompleted last, come before turn/result;
      // session events such as going idle may follow it.
      assert.ok(completedAtResult, "turn/result came before the turn's TurnCompleted event");
      for (let i = 1; i < wire.length; i++) assert.ok(wire[i]!.Seq! > wire[i - 1]!.Seq!);
      await session.close();
      await collecting;
      assert.ok(iterated.some((e) => e.Type === "TurnCompleted"), "events() missed TurnCompleted");
    } finally {
      await agentd.close();
    }
  });

  test("permission through handler/permission", opts, async () => {
    const agentd = await start();
    try {
      let asked: AgentPermissionRequest | undefined;
      agentd.handle("handler/permission", async (params) => {
        asked = params.Request ?? undefined;
        return { Decision: "allow" };
      });
      const session = await agentd.startSession({ Provider: "fake", Handlers: { Permission: true } });
      const result = await session.run("permission");
      assert.equal(result.Text, "allow");
      assert.equal(asked?.Command, "ls");
    } finally {
      await agentd.close();
    }
  });

  test("undeclared permission arrives as an event and request/respond answers it", opts, async () => {
    const agentd = await start();
    try {
      const session = await agentd.startSession({ Provider: "fake" });
      const answering = (async () => {
        for await (const e of session.events()) {
          if (e.Type === "PermissionRequest") {
            const request = e.Event as AgentPermissionRequest;
            await agentd.call("request/respond", {
              Session: session.id,
              RequestID: request.ID,
              Permission: { Decision: "deny" },
            });
            return;
          }
        }
      })();
      const result = await session.run("permission");
      await answering;
      assert.equal(result.Text, "deny");
    } finally {
      await agentd.close();
    }
  });

  test("host tool through tool/call", opts, async () => {
    const agentd = await start();
    try {
      agentd.handle("tool/call", async (params) => {
        assert.equal(params.Tool, "greet");
        const args = params.Call?.Arguments as { name: string };
        return { Content: [{ Type: "text", Text: `hello ${args.name}` }] };
      });
      const session = await agentd.startSession({
        Provider: "fake",
        Config: { Tools: [{ Name: "greet", Description: "Greets", InputSchema: { type: "object" } }] },
      });
      const result = await session.run('tool greet {"name":"ada"}');
      assert.equal(result.Text, "hello ada");
    } finally {
      await agentd.close();
    }
  });

  test("unhandled reverse request is an error reply", opts, async () => {
    const agentd = await start();
    try {
      const session = await agentd.startSession({ Provider: "fake", Handlers: { Permission: true } });
      const result = await session.run("permission");
      assert.notEqual(result.Text, "allow");
    } finally {
      await agentd.close();
    }
  });

  test("failed turn rejects run", opts, async () => {
    const agentd = await start();
    try {
      const session = await agentd.startSession({ Provider: "fake" });
      await assert.rejects(session.run("fail"), (err: unknown) => {
        assert.ok(err instanceof AgentdError);
        assert.match(err.message, /scripted failure/);
        assert.equal(err.result?.Status, "failed");
        return true;
      });
    } finally {
      await agentd.close();
    }
  });

  test("interrupt ends a waiting turn", opts, async () => {
    const agentd = await start();
    try {
      const session = await agentd.startSession({ Provider: "fake" });
      const running = session.run("wait");
      await new Promise((r) => setTimeout(r, 200));
      await session.interrupt();
      assert.equal((await running).Status, "interrupted");
    } finally {
      await agentd.close();
    }
  });

  test("aborting a call rejects it and cancelling a finished call is a no-op", opts, async () => {
    const agentd = await start();
    try {
      const session = await agentd.startSession({ Provider: "fake" });
      const running = session.run("wait").catch(() => {});
      // fakeagentd has no slow method, so abort before the reply can arrive.
      const controller = new AbortController();
      const info = agentd.call("session/info", { Session: session.id }, { signal: controller.signal });
      controller.abort(new Error("stop"));
      await assert.rejects(info, /stop/);

      const done = new AbortController();
      assert.deepEqual(await agentd.call("$/ping", {}, { signal: done.signal }), {});
      done.abort();
      assert.deepEqual(await agentd.call("$/ping", {}), {});
      await session.interrupt();
      await running;
    } finally {
      await agentd.close();
    }
  });

  test("unknown session is not_found", opts, async () => {
    const agentd = await start();
    try {
      await assert.rejects(agentd.call("session/info", { Session: "nope" }), (err: unknown) => {
        assert.ok(err instanceof AgentdError);
        assert.equal(err.info?.Kind, "not_found");
        assert.equal(typeof err.code, "number");
        return true;
      });
    } finally {
      await agentd.close();
    }
  });

  test("closing the session settles a running turn", opts, async () => {
    const agentd = await start();
    try {
      const session = await agentd.startSession({ Provider: "fake" });
      const running = session.run("wait").catch((err: unknown) => {
        assert.ok(err instanceof AgentdError);
      });
      await new Promise((r) => setTimeout(r, 200));
      await session.close();
      await within(running, 2000);
    } finally {
      await agentd.close();
    }
  });

  test("session/closed in the same read as the send reply rejects run", opts, async () => {
    const agentd = await startScripted("closed-with-reply");
    try {
      const session = await agentd.startSession({ Provider: "fake" });
      await within(
        assert.rejects(session.run("hi"), (err: unknown) => {
          assert.ok(err instanceof AgentdError);
          assert.equal(err.info?.Kind, "closed");
          return true;
        }),
        2000,
      );
      await within(assert.rejects(session.run("again"), AgentdError), 2000);
    } finally {
      await agentd.close();
    }
  });

  test("aborting run interrupts the turn", opts, async () => {
    const agentd = await start();
    try {
      const session = await agentd.startSession({ Provider: "fake" });
      const interrupted = new Promise<string | undefined>((resolve) => {
        agentd.on("turn/result", (n) => {
          if (n.Session === session.id) resolve(n.Result?.Status);
        });
      });
      const controller = new AbortController();
      const running = session.run("wait", { signal: controller.signal });
      await new Promise((r) => setTimeout(r, 200));
      controller.abort(new Error("stop"));
      await within(assert.rejects(running, /stop/), 2000);
      assert.equal(await within(interrupted, 5000), "interrupted");
    } finally {
      await agentd.close();
    }
  });

  test("lines that are not JSON objects are ignored", opts, async () => {
    const agentd = await startScripted("not-object");
    try {
      assert.deepEqual(await agentd.call("$/ping", {}), {});
      assert.deepEqual(await agentd.call("$/ping", {}), {});
    } finally {
      await agentd.close();
    }
  });

  test("close kills an agentd that does not exit", opts, async () => {
    const agentd = await startScripted("ignore-eof");
    await within(agentd.close(), 10_000);
  });

  test("agentd exiting rejects pending calls", opts, async () => {
    const agentd = await start();
    const session = await agentd.startSession({ Provider: "fake" });
    const running = session.run("wait");
    const closing = agentd.close();
    await assert.rejects(running, AgentdError);
    await closing;
    await assert.rejects(agentd.call("$/ping", {}), AgentdError);
  });
});
