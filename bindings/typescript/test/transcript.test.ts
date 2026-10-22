import assert from "node:assert/strict";
import { test } from "node:test";
import { Transcript, type Event, type Item } from "../src/index.ts";

const meta = { time_ms: 0 };
const item = (fields: Partial<Item> & { id: string }): Item => ({ kind: "other", status: "completed", time_ms: 0, ...fields });

test("folds text, reasoning, plan, and tool output deltas", () => {
  const t = new Transcript();
  const events: Event[] = [
    { type: "text_delta", item_id: "a", delta: "hel", parent_id: "p", subagent_id: "s", turn_id: "t1", ...meta },
    { type: "text_delta", item_id: "a", delta: "lo", ...meta },
    { type: "reasoning_delta", item_id: "r", delta: "think", ...meta },
    { type: "plan_delta", item_id: "p1", delta: "step", ...meta },
    { type: "tool_output_delta", item_id: "c", delta: "out", ...meta },
    { type: "tool_output_delta", item_id: "c", delta: "put", ...meta },
  ];
  for (const ev of events) assert.ok(t.apply(ev));
  const [a, r, p, c] = t.items();
  assert.deepEqual([a.kind, a.text, a.parent_id, a.subagent_id, a.status, a.turn_id], ["assistant_message", "hello", "p", "s", "in_progress", "t1"]);
  assert.deepEqual([r.kind, r.text], ["reasoning", "think"]);
  assert.deepEqual([p.kind, p.text], ["plan", "step"]);
  assert.deepEqual([c.kind, c.tool?.output], ["tool_call", "output"]);
  assert.equal(t.apply({ type: "usage_updated", usage: {}, ...meta } as Event), undefined);
});

test("item_updated keeps streamed text and tool output when the update has none", () => {
  const t = new Transcript();
  const tool = { kind: "shell" as const, duration_ms: 0 };
  t.apply({ type: "item_started", item: item({ id: "c", kind: "tool_call", status: "in_progress", tool }), ...meta });
  t.apply({ type: "tool_output_delta", item_id: "c", delta: "out", ...meta });
  assert.equal(tool.hasOwnProperty("output"), false, "the event's tool object is not mutated");
  t.apply({ type: "text_delta", item_id: "c", delta: "txt", ...meta });
  const change = t.apply({ type: "item_updated", item: item({ id: "c", kind: "tool_call", status: "in_progress", tool }), ...meta });
  assert.ok(change && "item" in change);
  assert.equal(change.item.text, "txt");
  assert.equal(change.item.tool?.output, "out");
  t.apply({ type: "item_completed", item: item({ id: "c", kind: "tool_call", text: "final", tool: { ...tool, output: "done" } }), ...meta });
  assert.equal(t.item("c")?.text, "final");
  assert.equal(t.item("c")?.tool?.output, "done");
});

test("items_removed and conversation_reset drop items", () => {
  const t = new Transcript();
  for (const id of ["a", "b", "c"]) t.apply({ type: "item_completed", item: item({ id }), ...meta });
  assert.deepEqual(t.apply({ type: "items_removed", ids: ["b", "zz"], ...meta }), { removed: ["b"] });
  assert.deepEqual(t.items().map((i) => i.id), ["a", "c"]);
  assert.equal(t.item("b"), undefined);
  assert.equal(t.apply({ type: "items_removed", ids: ["zz"], ...meta }), undefined);
  t.apply({ type: "text_delta", item_id: "c", delta: "x", ...meta });
  assert.equal(t.item("c")?.text, "x");
  assert.deepEqual(t.apply({ type: "conversation_reset", ...meta }), { removed: ["a", "c"] });
  assert.deepEqual(t.items(), []);
});

test("load replaces content and byTurn groups items by turn in order", () => {
  const t = new Transcript();
  t.apply({ type: "item_completed", item: item({ id: "old" }), ...meta });
  t.load([item({ id: "u1", turn_id: "t1" }), item({ id: "a1", turn_id: "t1" }), item({ id: "u2", turn_id: "t2" }), item({ id: "x" })]);
  assert.equal(t.item("old"), undefined);
  const groups = t.byTurn();
  assert.deepEqual([...groups.keys()], ["t1", "t2", ""]);
  assert.deepEqual(groups.get("t1")?.map((i) => i.id), ["u1", "a1"]);
  t.reset();
  assert.equal(t.items().length, 0);
});
