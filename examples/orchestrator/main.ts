// Orchestrator splits a question across several agents on different
// harnesses and merges their findings.
//
//   node main.ts "How are errors handled across this repository, and where is it inconsistent?"
//   node main.ts --planner codex --workers claude,pi --max 3 "Map the public API"
//
// It runs in three steps:
//
//  1. A planner breaks the task into independent subtasks, answering with
//     structured output.
//  2. Workers, spread round-robin over the --workers providers, research
//     the subtasks in parallel. They may read but not change anything.
//  3. The planner's harness writes the final answer from their reports.
//
// Every session is ephemeral, so nothing is left in the harnesses' session
// lists.
import { parseArgs } from "node:util";
import { Client, PermissionMode, type Session, type SessionOptions, type TurnResult, type Usage } from "agent-cli-connector";

interface Subtask {
  title: string;
  prompt: string;
}

/** What one worker found. */
interface Report extends Subtask {
  provider: string;
  result?: TurnResult;
  error?: unknown;
}

const planSchema = {
  type: "object",
  properties: {
    subtasks: {
      type: "array",
      items: {
        type: "object",
        properties: { title: { type: "string" }, prompt: { type: "string" } },
        required: ["title", "prompt"],
        additionalProperties: false,
      },
    },
  },
  required: ["subtasks"],
  additionalProperties: false,
};

const { values: flags, positionals } = parseArgs({
  allowPositionals: true,
  options: {
    planner: { type: "string", default: "claude" },
    workers: { type: "string", default: "claude,codex" },
    max: { type: "string", default: "4" },
    cwd: { type: "string", default: "" },
    timeout: { type: "string", default: "600" },
    model: { type: "string", default: "" },
    transport: { type: "string", default: "native" },
  },
});
const task = positionals.join(" ");
const workers = splitList(flags.workers);
const maxTasks = Number(flags.max);
const timeoutMs = Number(flags.timeout) * 1000;
if (!task || workers.length === 0 || !(maxTasks >= 1) || !(timeoutMs > 0)) {
  console.error(
    "usage: node main.ts [--planner claude] [--workers claude,codex] [--max 4] [--cwd dir] [--timeout seconds]\n" +
      "                    [--model name] [--transport native|agentd] <task>",
  );
  process.exit(2);
}
if (flags.transport !== "native" && flags.transport !== "agentd") {
  console.error("--transport is native or agentd");
  process.exit(2);
}

const providers = Object.fromEntries([flags.planner, ...workers].map((name) => [name, {}]));
const client = await Client.open({ transport: flags.transport, providers });
process.once("SIGINT", () => {
  log("interrupted");
  void client.close().then(() => process.exit(130));
});

try {
  await orchestrate(task, flags.planner, workers);
} catch (err) {
  console.error("error:", err instanceof Error ? err.message : err);
  process.exitCode = 1;
} finally {
  await client.close();
}

async function orchestrate(task: string, planner: string, workers: string[]): Promise<void> {
  log(`planning on ${planner}`);
  const plan = await makePlan(planner, task);
  plan.forEach((s, i) => log(`  ${i + 1}. ${s.title} → ${workers[i % workers.length]}`));

  const reports: Report[] = plan.map((s, i) => ({ ...s, provider: workers[i % workers.length]! }));
  await Promise.all(
    reports.map(async (r, i) => {
      try {
        r.result = await work(i + 1, r.provider, task, r);
      } catch (err) {
        r.error = err;
      }
    }),
  );

  log(`writing the answer on ${planner}`);
  let final: TurnResult | undefined;
  try {
    final = await synthesize(planner, task, reports);
  } finally {
    summary(reports, final);
  }
}

/**
 * Asks for subtasks as structured output. A harness without structured
 * output ignores the schema, so its plain-text reply is parsed as JSON
 * instead; a reply that is not the requested JSON runs the task as one
 * subtask.
 */
async function makePlan(provider: string, task: string): Promise<Subtask[]> {
  await using session = await start(provider, { output_schema: planSchema });
  const res = await session.run(
    `Split this task into at most ${maxTasks} independent research subtasks that separate agents can do in parallel ` +
      "by reading the files in the working directory. Each prompt must stand alone, because its agent sees nothing else. " +
      `Answer with JSON only: {"subtasks": [{"title": "...", "prompt": "..."}]}.\n\nTask: ${task}`,
  );
  if (res.status !== "completed") throw new Error(`the planner's turn ${res.status}: ${res.error?.message ?? ""}`);
  let answer = res.structured_output;
  if (answer === undefined) {
    try {
      answer = JSON.parse(res.text ?? "");
    } catch {
      log("the plan is not the requested JSON; running the task as one subtask");
      return [{ title: "The whole task", prompt: task }];
    }
  }
  const subtasks = (answer as { subtasks?: Subtask[] })?.subtasks ?? [];
  if (subtasks.length === 0) throw new Error("the planner returned no subtasks");
  return subtasks.slice(0, maxTasks);
}

/** Runs one subtask and prints the worker's tool calls as they start. */
async function work(n: number, provider: string, task: string, s: Subtask): Promise<TurnResult> {
  let session: Session;
  try {
    session = await start(provider);
  } catch (err) {
    log(`[${n} ${provider}] could not start: ${message(err)}`);
    throw err;
  }
  await using _ = session;
  const turn = await session.send(
    `You are one of several agents working on: ${task}\n\nYour part: ${s.prompt}\n\n` +
      "Do not change any file. Report concrete findings with file paths, briefly.",
  );
  // A timeout interrupts the turn, which ends its events.
  const done = turn.wait({ timeoutMs });
  done.catch(() => {});
  for await (const ev of turn.events()) {
    const tool = ev.type === "item_started" && !ev.item.parent_id ? ev.item.tool : undefined;
    if (tool) {
      const subject = tool.command || tool.path || tool.query || tool.url || tool.name || "";
      log(`[${n} ${provider}] ${tool.kind ?? "other"} ${oneLine(subject, 70)}`);
    }
  }
  const res = await done;
  log(`[${n} ${provider}] ${res.status} in ${seconds(res.duration_ms)}`);
  return res;
}

/** Merges the reports and streams the answer to stdout. */
async function synthesize(provider: string, task: string, reports: Report[]): Promise<TurnResult> {
  let prompt =
    `Several agents researched parts of this task: ${task}\n\nTheir reports follow. Write the final answer: merge what they found, ` +
    "resolve contradictions, and say what is still unknown. Do not redo their research.\n";
  reports.forEach((r, i) => {
    prompt += `\n## Report ${i + 1}: ${r.title} (${r.provider})\n`;
    if (r.result?.text) prompt += r.result.text;
    else if (r.error !== undefined) prompt += `The agent failed: ${message(r.error)}`;
    else prompt += "The agent returned nothing.";
    prompt += "\n";
  });

  await using session = await start(provider);
  const turn = await session.send(prompt);
  process.stdout.write("\n");
  for await (const ev of turn.events()) {
    if (ev.type === "text_delta" && !ev.parent_id) process.stdout.write(ev.delta ?? "");
  }
  process.stdout.write("\n");
  return turn.wait();
}

/**
 * Starts an ephemeral session that cannot change anything: the harness's
 * read-only mode where it has one, and for the rest a handler that refuses
 * every tool call the harness asks about.
 */
function start(provider: string, extra: SessionOptions = {}): Promise<Session> {
  const modes = client.providers.find((p) => p.name === provider)?.capabilities.permission_modes ?? [];
  return client.start(provider, {
    cwd: flags.cwd || undefined,
    model: flags.model || undefined,
    ephemeral: true,
    permission_mode: modes.includes(PermissionMode.readOnly) ? PermissionMode.readOnly : undefined,
    onPermission: async () => ({ decision: "deny", message: "You may only read. Report what you would change instead." }),
    ...extra,
  });
}

function summary(reports: Report[], final: TurnResult | undefined): void {
  const total = { input_tokens: 0, output_tokens: 0, cost_usd: 0 };
  const add = (u?: Usage) => {
    if (!u) return;
    total.input_tokens += u.input_tokens;
    total.output_tokens += u.output_tokens;
    total.cost_usd += u.cost_usd;
  };
  log("");
  reports.forEach((r, i) => {
    let status = `failed: ${message(r.error)}`;
    if (r.result) {
      const u = r.result.usage;
      status = `${r.result.status}, ${u.input_tokens} in / ${u.output_tokens} out tokens, ${seconds(r.result.duration_ms)}`;
      add(u);
    }
    log(`worker ${i + 1} (${r.provider}) ${r.title}: ${status}`);
  });
  add(final?.usage);
  let line = `total ${total.input_tokens} in / ${total.output_tokens} out tokens`;
  if (total.cost_usd > 0) line += `, $${total.cost_usd.toFixed(4)} where reported`;
  log(line);
}

function log(line: string): void {
  process.stderr.write(line + "\n");
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function seconds(ms = 0): string {
  return `${Math.round(ms / 1000)}s`;
}

function splitList(s: string): string[] {
  return s
    .split(",")
    .map((v) => v.trim())
    .filter((v) => v !== "");
}

function oneLine(s: string, n: number): string {
  const line = s.trim().split("\n", 1)[0] ?? "";
  const chars = [...line];
  return chars.length > n ? chars.slice(0, n).join("") + "…" : line;
}
