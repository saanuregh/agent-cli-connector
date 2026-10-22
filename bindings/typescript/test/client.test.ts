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

      const seen = [];
      for await (const ev of session.events()) {
        seen.push(ev.type);
        if (ev.type === "turn_completed") break;
      }
      assert.equal(seen[0], "session_started");
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
