import assert from "node:assert/strict";
import { describe, test } from "node:test";
import { AgentError, Client, tool } from "../src/index.ts";

for (const transport of ["native", "agentd"] as const) {
  describe(`${transport} transport`, () => {
    test("runs a turn and streams its deltas", async () => {
      await using client = await Client.open({ transport, providers: { fake: {} } });
      assert.deepEqual(client.providers.map((p) => p.name), ["fake"]);
      await using session = await client.start("fake");

      const turn = await session.send("hello there world");
      let text = "";
      let completed = false;
      for await (const ev of turn.events()) {
        if (ev.type === "text_delta") text += ev.delta ?? "";
        if (ev.type === "turn_completed") completed = ev.result.status === "completed";
      }
      const result = await turn.wait();
      assert.equal(result.text, "hello there world");
      assert.equal(text, "hello there world");
      assert.ok(completed);

      const first = await session.events().next();
      assert.ok(!first.done && first.value.type === "session_started");
      assert.equal(session.info.id, session.id);
      assert.deepEqual(await session.fetchInfo(), session.info);
    });

    test("answers a permission request from the event and from the handler", async () => {
      await using client = await Client.open({ transport, providers: { fake: {} } });
      await using session = await client.start("fake");
      const turn = await session.send("this needs permission");
      for await (const ev of turn.events()) if (ev.type === "permission_request") await ev.deny("no");
      assert.equal((await turn.wait()).text, "permission denied");

      await using auto = await client.start("fake", { onPermission: async () => "allow" });
      assert.equal((await auto.run("this needs permission")).text, "this needs permission");
    });

    test("delivers auto-answered requests marked handled", async () => {
      await using client = await Client.open({ transport, providers: { fake: {} } });
      await using session = await client.start("fake", { onPermission: async () => "allow" });
      const events = session.events();
      const turn = await session.send("this needs permission");
      const fromTurn = [];
      for await (const ev of turn.events()) if (ev.type === "permission_request") fromTurn.push(ev);
      assert.equal(fromTurn.length, 1);
      assert.equal(fromTurn[0].handled, true);
      for await (const ev of events) {
        if (ev.type === "permission_request") assert.equal(ev.handled, true);
        if (ev.type === "turn_completed") break;
      }

      await using manual = await client.start("fake");
      const t2 = await manual.send("this needs permission");
      for await (const ev of t2.events()) if (ev.type === "permission_request") {
        assert.equal(ev.handled, undefined);
        await ev.allow();
      }
    });

    test("subscribers get every event; events() buffers from its first call", async () => {
      await using client = await Client.open({ transport, providers: { fake: {} } });
      await using session = await client.start("fake");
      const seen: string[] = [];
      const unsubscribe = session.subscribe((ev) => {
        seen.push(ev.type);
        throw new Error("ignored");
      });
      const other: string[] = [];
      session.subscribe((ev) => void other.push(ev.type));
      await session.run("first");
      assert.ok(seen.includes("turn_started") && seen.includes("turn_completed"));
      assert.deepEqual(other, seen);
      unsubscribe();
      const count = seen.length;

      const events = session.events();
      const turn = await session.send("second");
      await turn.wait();
      assert.equal(seen.length, count);
      const started = await events.next();
      assert.ok(!started.done && started.value.type === "session_started");
      const next = await events.next();
      assert.ok(!next.done && next.value.turn_id === turn.id, "other events before the first events() call are not kept");
    });

    test("answers dialog and native requests with helpers and handlers", async () => {
      await using client = await Client.open({ transport, providers: { fake: {} } });
      await using session = await client.start("fake");
      let turn = await session.send("dialog");
      for await (const ev of turn.events()) if (ev.type === "dialog_request") await ev.complete({ ok: 1 });
      assert.equal((await turn.wait()).text, 'dialog completed {"ok":1}');
      turn = await session.send("dialog");
      for await (const ev of turn.events()) if (ev.type === "dialog_request") await ev.cancel();
      assert.equal((await turn.wait()).text, "dialog cancelled");
      turn = await session.send("native");
      for await (const ev of turn.events()) if (ev.type === "native_request") await ev.reply("pong");
      assert.equal((await turn.wait()).text, 'native result "pong"');
      turn = await session.send("native");
      for await (const ev of turn.events()) if (ev.type === "native_request") await ev.fail(7, "nope");
      assert.equal((await turn.wait()).text, "native error 7 nope");

      await using auto = await client.start("fake", {
        onDialog: async (req) => ({ behavior: "completed", result: req.kind }),
        onNativeRequest: async (req) => ({ result: req.method }),
      });
      assert.equal((await auto.run("dialog")).text, 'dialog completed "fake_dialog"');
      assert.equal((await auto.run("native")).text, 'native result "fake/ping"');

      await using failing = await client.start("fake", {
        onDialog: async () => {
          throw new Error("broken");
        },
        onNativeRequest: async () => {
          throw new Error("broken");
        },
      });
      assert.equal((await failing.run("dialog")).text, "dialog cancelled");
      assert.equal((await failing.run("native")).text, "native error -32603 broken");
    });

    test("runs a host tool", async () => {
      await using client = await Client.open({ transport, providers: { fake: {} } });
      const lookup = tool<{ id?: string }>({
        name: "lookup",
        description: "Looks up a ticket",
        inputSchema: { type: "object", properties: { id: { type: "string" } } },
        run: async () => "ticket 42",
      });
      await using session = await client.start("fake", { tools: [lookup] });
      const result = await session.run("tool:lookup");
      assert.match(result.text ?? "", /ticket 42/);
    });

    test("interrupting aborts a running tool call", async () => {
      await using client = await Client.open({ transport, providers: { fake: {} } });
      let started!: () => void;
      const running = new Promise<void>((resolve) => (started = resolve));
      let aborted = false;
      const slow = tool({
        name: "slow",
        run: (_args, _call, signal) =>
          new Promise<string>((resolve) => {
            started();
            signal.addEventListener("abort", () => {
              aborted = true;
              resolve("stopped");
            });
          }),
      });
      await using session = await client.start("fake", { tools: [slow] });
      const turn = await session.send("tool:slow");
      await running;
      await session.interrupt();
      const result = await turn.wait();
      assert.equal(result.status, "interrupted");
      assert.ok(aborted);
    });

    test("reports errors with their kind", async () => {
      await using client = await Client.open({ transport, providers: { fake: {} } });
      await assert.rejects(client.start("missing"), (err: unknown) => err instanceof AgentError && err.kind === "not_found");
    });
  });
}

test("the package version names the release it downloads", async () => {
  const { LIBRARY_VERSION } = await import("../src/index.ts");
  const pkg = JSON.parse(await (await import("node:fs/promises")).readFile(new URL("../package.json", import.meta.url), "utf8"));
  assert.equal(pkg.version, LIBRARY_VERSION);
});
