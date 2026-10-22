import type { Event, Item, ItemKind, ToolCall } from "./protocol.ts";

/** What `Transcript.apply` changed: the updated item, or the IDs it removed. */
export type TranscriptChange = { item: Item } | { removed: string[] };

/**
 * Folds events into the current list of transcript items, assembling
 * streamed text, reasoning, plan, and tool output as it arrives. A chat UI
 * can render `items()` after each `apply` instead of tracking deltas itself.
 *
 * Items are replaced, never mutated, so an item returned earlier and the
 * items of events stay unchanged.
 */
export class Transcript {
  #items: Item[] = [];
  #index = new Map<string, number>();

  /** Updates the transcript with `event`; returns undefined when it changed nothing. */
  apply(event: Event): TranscriptChange | undefined {
    switch (event.type) {
      case "item_started":
      case "item_completed":
        return { item: this.#put(event.item, false) };
      case "item_updated":
        return { item: this.#put(event.item, true) };
      case "text_delta":
        return this.#appendText(event, "assistant_message");
      case "reasoning_delta":
        return this.#appendText(event, "reasoning");
      case "plan_delta":
        return this.#appendText(event, "plan");
      case "tool_output_delta": {
        const [it, i] = this.#ensure(event, "tool_call");
        // A new ToolCall: the old one may be shared with an event or a caller.
        const tool: ToolCall = { kind: "other", duration_ms: 0, ...it.tool, output: (it.tool?.output ?? "") + (event.delta ?? "") };
        return { item: (this.#items[i] = { ...it, tool }) };
      }
      case "items_removed":
        return this.#remove(event.ids ?? []);
      case "conversation_reset": {
        const removed = this.#items.map((it) => it.id ?? "");
        this.reset();
        return removed.length > 0 ? { removed } : undefined;
      }
    }
    return undefined;
  }

  /** A copy of the items in order of first appearance. */
  items(): Item[] {
    return this.#items.slice();
  }

  item(id: string): Item | undefined {
    const i = this.#index.get(id);
    return i === undefined ? undefined : this.#items[i];
  }

  /** The items grouped by `turn_id` in order of first appearance; items without a turn are under "". */
  byTurn(): Map<string, Item[]> {
    const groups = new Map<string, Item[]>();
    for (const it of this.#items) {
      const key = it.turn_id ?? "";
      const group = groups.get(key);
      if (group) group.push(it);
      else groups.set(key, [it]);
    }
    return groups;
  }

  /** Replaces the content with `items`, as returned by `readHistory`. */
  load(items: Item[]): void {
    this.reset();
    for (const it of items) this.#put(it, false);
  }

  /** Removes every item. */
  reset(): void {
    this.#items = [];
    this.#index.clear();
  }

  // String `+=` builds a rope in JavaScript engines, so appending deltas stays linear.
  #appendText(event: Extract<Event, { type: "text_delta" | "reasoning_delta" | "plan_delta" }>, kind: ItemKind): TranscriptChange {
    const [it, i] = this.#ensure(event, kind);
    return { item: (this.#items[i] = { ...it, text: (it.text ?? "") + (event.delta ?? "") }) };
  }

  /** Stores `item`. With merge, or while it runs, text and tool output streamed earlier are kept when it carries none. */
  #put(item: Item, merge: boolean): Item {
    const id = item.id ?? "";
    const i = this.#index.get(id);
    if (i === undefined) {
      this.#index.set(id, this.#items.length);
      this.#items.push(item);
      return item;
    }
    const old = this.#items[i];
    let next = item;
    if (merge || item.status === "in_progress") {
      if (!item.text && old.text) next = { ...next, text: old.text };
      if (item.tool && old.tool && !item.tool.output && old.tool.output) next = { ...next, tool: { ...item.tool, output: old.tool.output } };
    }
    this.#items[i] = next;
    return next;
  }

  #ensure(
    event: { item_id?: string; parent_id?: string; subagent_id?: string; turn_id?: string; time_ms: number },
    kind: ItemKind,
  ): [Item, number] {
    const id = event.item_id ?? "";
    const i = this.#index.get(id);
    if (i !== undefined) return [this.#items[i], i];
    const it: Item = { id, kind, status: "in_progress", time_ms: event.time_ms };
    if (event.parent_id) it.parent_id = event.parent_id;
    if (event.subagent_id) it.subagent_id = event.subagent_id;
    if (event.turn_id) it.turn_id = event.turn_id;
    this.#index.set(id, this.#items.length);
    this.#items.push(it);
    return [it, this.#items.length - 1];
  }

  #remove(ids: string[]): TranscriptChange | undefined {
    const removed = ids.filter((id) => this.#index.has(id));
    if (removed.length === 0) return undefined;
    const drop = new Set(removed);
    this.#items = this.#items.filter((it) => !drop.has(it.id ?? ""));
    this.#index.clear();
    this.#items.forEach((it, i) => this.#index.set(it.id ?? "", i));
    return { removed };
  }
}
