import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import type {
  AgentInput,
  AgentSessionInfo,
  ErrorInfo,
  EventNotification,
  InitializeResult,
  Methods,
  Notifications,
  ProviderInit,
  ReverseRequests,
  StartParams,
  StartResult,
  TurnResultNotification,
  TurnResultWire,
} from "./protocol.ts";

export const PROTOCOL_VERSION = 1;

/** JSON-RPC error codes the client uses or receives. */
export const ErrorCodes = {
  MethodNotFound: -32601,
  InternalError: -32603,
  RequestCancelled: -32800,
} as const;

/** An error reply from agentd, a failed turn, or agentd exiting. */
export class AgentdError extends Error {
  /** The JSON-RPC error code; undefined for a failed turn or agentd exiting. */
  readonly code: number | undefined;
  readonly info: ErrorInfo | undefined;
  /** The failed turn's result, when a turn ended with an error. */
  readonly result: TurnResultWire | undefined;

  constructor(message: string, code?: number, info?: ErrorInfo | null, result?: TurnResultWire) {
    super(message);
    this.name = "AgentdError";
    this.code = code;
    this.info = info ?? undefined;
    this.result = result;
  }
}

export interface StartOptions {
  /** The agentd executable; defaults to "agentd" on PATH. */
  command?: string;
  args?: string[];
  providers?: Record<string, ProviderInit>;
  includeRaw?: boolean;
  /** The child's environment; defaults to this process's. */
  env?: NodeJS.ProcessEnv;
  cwd?: string;
}

export interface CallOptions {
  signal?: AbortSignal;
}

type ReverseHandler<R extends keyof ReverseRequests> = (
  params: ReverseRequests[R]["params"],
  signal: AbortSignal,
) => Promise<ReverseRequests[R]["result"]>;

interface Pending {
  resolve: (value: unknown) => void;
  reject: (err: Error) => void;
}

interface Message {
  jsonrpc?: string;
  id?: number | string | null;
  method?: string;
  params?: unknown;
  result?: unknown;
  error?: { code: number; message: string; data?: ErrorInfo };
}

const stderrLimit = 8192;
/** How long close() waits for agentd to exit after closing its standard input before killing it. */
const closeTimeoutMs = 5000;

/** Per Agentd, the callbacks to run once when it exits; sessions register here. */
const exitCallbacks = new WeakMap<Agentd, Set<(err: AgentdError) => void>>();

function closedError(message: string, info?: ErrorInfo | null): AgentdError {
  return new AgentdError(message, undefined, { ...info, Kind: "closed", Message: message });
}

/** A running agentd child process speaking JSON-RPC over its standard input and output. */
export class Agentd {
  /** The reply to initialize. */
  initializeResult: InitializeResult = {};

  readonly #child: ChildProcessWithoutNullStreams;
  #nextId = 1;
  readonly #pending = new Map<number, Pending>();
  readonly #listeners = new Map<string, Set<(params: any) => void>>();
  readonly #handlers = new Map<string, (params: any, signal: AbortSignal) => Promise<unknown>>();
  readonly #reverse = new Map<string, AbortController>();
  #exitError: AgentdError | undefined;
  readonly #exited: Promise<void>;
  #stderr = "";

  private constructor(child: ChildProcessWithoutNullStreams) {
    this.#child = child;
    const onExit = new Set<(err: AgentdError) => void>();
    exitCallbacks.set(this, onExit);
    let buffer = "";
    child.stdout.setEncoding("utf8");
    child.stdout.on("data", (chunk: string) => {
      buffer += chunk;
      let newline: number;
      while ((newline = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, newline).trim();
        buffer = buffer.slice(newline + 1);
        if (line !== "") this.#receive(line);
      }
    });
    child.stderr.setEncoding("utf8");
    child.stderr.on("data", (chunk: string) => {
      this.#stderr = (this.#stderr + chunk).slice(-stderrLimit);
    });
    // Writes after agentd exits fail with EPIPE; the exit handler reports it.
    child.stdin.on("error", () => {});
    this.#exited = new Promise((resolve) => {
      const finish = (detail: string) => {
        if (this.#exitError) return;
        const stderr = this.#stderr.trim();
        this.#exitError = closedError(`agentd exited (${detail})${stderr ? `: ${stderr}` : ""}`);
        for (const pending of this.#pending.values()) pending.reject(this.#exitError);
        this.#pending.clear();
        for (const controller of this.#reverse.values()) controller.abort(this.#exitError);
        this.#reverse.clear();
        for (const callback of onExit) callback(this.#exitError);
        exitCallbacks.delete(this);
        resolve();
      };
      child.on("error", (err) => finish(err.message));
      child.on("close", (code, signal) => finish(signal ? `signal ${signal}` : `code ${code}`));
    });
  }

  /** Spawns agentd and initializes it. */
  static async start(opts: StartOptions = {}): Promise<Agentd> {
    const child = spawn(opts.command ?? "agentd", opts.args ?? [], {
      stdio: ["pipe", "pipe", "pipe"],
      env: opts.env ?? process.env,
      cwd: opts.cwd,
    });
    const agentd = new Agentd(child);
    try {
      agentd.initializeResult = await agentd.call("initialize", {
        ProtocolVersion: PROTOCOL_VERSION,
        Providers: opts.providers ?? {},
        IncludeRaw: opts.includeRaw ?? false,
      });
    } catch (err) {
      await agentd.close();
      throw err;
    }
    return agentd;
  }

  /** Sends a request and resolves with its result. Aborting the signal cancels the request. */
  call<M extends keyof Methods>(
    method: M,
    params: Methods[M]["params"],
    opts: CallOptions = {},
  ): Promise<Methods[M]["result"]> {
    if (this.#exitError) return Promise.reject(this.#exitError);
    const signal = opts.signal;
    if (signal?.aborted) return Promise.reject(signal.reason);
    const id = this.#nextId++;
    return new Promise((resolve, reject) => {
      const onAbort = () => {
        if (!this.#pending.delete(id)) return;
        this.notify("$/cancelRequest", { id });
        reject(signal!.reason);
      };
      this.#pending.set(id, {
        resolve: (value) => {
          signal?.removeEventListener("abort", onAbort);
          resolve(value as Methods[M]["result"]);
        },
        reject: (err) => {
          signal?.removeEventListener("abort", onAbort);
          reject(err);
        },
      });
      signal?.addEventListener("abort", onAbort, { once: true });
      this.#write({ jsonrpc: "2.0", id, method, params });
    });
  }

  /** Sends a notification to agentd. */
  notify(method: "$/cancelRequest", params: { id: number }): void {
    this.#write({ jsonrpc: "2.0", method, params });
  }

  /** Subscribes to a notification; returns a function that unsubscribes. */
  on<N extends keyof Notifications>(method: N, callback: (params: Notifications[N]["params"]) => void): () => void {
    let set = this.#listeners.get(method);
    if (!set) this.#listeners.set(method, (set = new Set()));
    set.add(callback);
    return () => {
      set.delete(callback);
    };
  }

  /**
   * Answers a request agentd sends. A thrown error replies with an error, which agentd reads as deny,
   * cancel, or no decision. The signal aborts when agentd cancels the request. Replaces any earlier
   * handler for the method.
   */
  handle<R extends keyof ReverseRequests>(method: R, fn: ReverseHandler<R>): () => void {
    this.#handlers.set(method, fn);
    return () => {
      if (this.#handlers.get(method) === fn) this.#handlers.delete(method);
    };
  }

  /** Starts a session. */
  async startSession(params: StartParams, opts?: CallOptions): Promise<Session> {
    return this.#session(await this.call("session/start", params, opts));
  }

  /** Forks a session into a new one. */
  async forkSession(params: Methods["session/fork"]["params"], opts?: CallOptions): Promise<Session> {
    return this.#session(await this.call("session/fork", params, opts));
  }

  #session(result: StartResult): Session {
    return new Session(this, result);
  }

  /**
   * Closes agentd's standard input, which closes every session, and waits for it to exit. Kills agentd
   * if it has not exited five seconds later.
   */
  async close(): Promise<void> {
    this.#child.stdin.end();
    let timer: NodeJS.Timeout | undefined;
    const timedOut = await Promise.race([
      this.#exited.then(() => false),
      new Promise<boolean>((resolve) => (timer = setTimeout(() => resolve(true), closeTimeoutMs))),
    ]);
    clearTimeout(timer);
    if (timedOut) {
      this.#child.kill("SIGKILL");
      await this.#exited;
    }
  }

  /** Resolves when agentd has exited. */
  get exited(): Promise<void> {
    return this.#exited;
  }

  #write(message: Message): void {
    if (this.#exitError || !this.#child.stdin.writable) return;
    this.#child.stdin.write(JSON.stringify(message) + "\n");
  }

  #receive(line: string): void {
    let message: Message;
    try {
      message = JSON.parse(line);
    } catch {
      return;
    }
    if (typeof message !== "object" || message === null || Array.isArray(message)) return;
    const hasId = message.id !== undefined && message.id !== null;
    if (message.method !== undefined && hasId) {
      void this.#answer(message.id!, message.method, message.params);
    } else if (message.method !== undefined) {
      if (message.method === "$/cancelRequest") {
        const id = (message.params as { id?: unknown } | undefined)?.id;
        this.#reverse.get(String(id))?.abort(new AgentdError("cancelled by agentd", ErrorCodes.RequestCancelled));
      }
      for (const callback of this.#listeners.get(message.method) ?? []) {
        try {
          callback(message.params);
        } catch (err) {
          queueMicrotask(() => {
            throw err;
          });
        }
      }
    } else if (typeof message.id === "number") {
      const pending = this.#pending.get(message.id);
      if (!pending) return;
      this.#pending.delete(message.id);
      if (message.error) {
        const info = message.error.data;
        pending.reject(new AgentdError(info?.Message || message.error.message, message.error.code, info));
      } else {
        pending.resolve(message.result);
      }
    }
  }

  async #answer(id: number | string, method: string, params: unknown): Promise<void> {
    const handler = this.#handlers.get(method);
    if (!handler) {
      this.#write({
        jsonrpc: "2.0",
        id,
        error: { code: ErrorCodes.MethodNotFound, message: `no handler for ${method}` },
      });
      return;
    }
    const key = String(id);
    const controller = new AbortController();
    this.#reverse.set(key, controller);
    try {
      const result = await handler(params, controller.signal);
      if (!controller.signal.aborted) this.#write({ jsonrpc: "2.0", id, result: result ?? null });
    } catch (err) {
      if (!controller.signal.aborted) {
        const message = err instanceof Error ? err.message : String(err);
        this.#write({ jsonrpc: "2.0", id, error: { code: ErrorCodes.InternalError, message } });
      }
    } finally {
      this.#reverse.delete(key);
    }
  }
}

/** One agent session in agentd. */
export class Session {
  readonly agentd: Agentd;
  readonly id: string;
  readonly info: AgentSessionInfo;
  /** The optional methods this session supports. */
  readonly methods: string[];

  /** Set once the session has ended; later runs fail with it. */
  #ended: AgentdError | undefined;
  readonly #iterators = new Set<EventQueue>();
  readonly #waiters = new Map<string, Pending>();
  /** Results that arrive while a run's send has not yet returned its TurnID. */
  readonly #early = new Map<string, TurnResultNotification>();
  #sending = 0;
  readonly #unsubscribe: Array<() => void>;
  readonly #onExit = (err: AgentdError) => this.#finish(err);

  constructor(agentd: Agentd, result: StartResult) {
    this.agentd = agentd;
    this.id = result.Session ?? "";
    this.info = result.Info ?? {};
    this.methods = result.Methods ?? [];
    this.#unsubscribe = [
      agentd.on("session/event", (n) => {
        if (n.Session !== this.id) return;
        for (const queue of this.#iterators) queue.push(n);
      }),
      agentd.on("turn/result", (n) => {
        if (n.Session !== this.id || !n.TurnID) return;
        const waiter = this.#waiters.get(n.TurnID);
        if (waiter) {
          this.#waiters.delete(n.TurnID);
          waiter.resolve(n);
        } else if (this.#sending > 0) {
          this.#early.set(n.TurnID, n);
        }
      }),
      agentd.on("session/closed", (n) => {
        if (n.Session === this.id) this.#finish(closedError(n.Error?.Message || "session closed", n.Error));
      }),
    ];
    const onExit = exitCallbacks.get(agentd);
    if (onExit) onExit.add(this.#onExit);
    else this.#finish(closedError("agentd exited"));
  }

  /** Yields this session's events from the time of the call until the session closes. */
  events(): AsyncIterable<EventNotification> {
    const queue = new EventQueue(() => this.#iterators.delete(queue));
    if (this.#ended) queue.end();
    else this.#iterators.add(queue);
    return queue;
  }

  /** Starts a turn and returns its TurnID; its events and turn/result follow. */
  async send(input: AgentInput | string, opts?: CallOptions): Promise<string> {
    const result = await this.agentd.call("session/send", { Session: this.id, Input: toInput(input) }, opts);
    return result.TurnID ?? "";
  }

  /**
   * Runs a turn and resolves with its result, including an interrupted one (Status "interrupted").
   * Rejects with an AgentdError when the turn has no result or failed (its `result` property then
   * holds the result), or with `info.Kind` "closed" when the session closes or agentd exits first.
   * Aborting the signal after the turn started sends session/interruptTurn and rejects with the
   * signal's reason without waiting for the turn to end.
   */
  async run(input: AgentInput | string, opts?: CallOptions): Promise<TurnResultWire> {
    this.#sending++;
    let turnId: string;
    try {
      turnId = await this.send(input, opts);
    } finally {
      this.#sending--;
    }
    let notification = this.#early.get(turnId);
    this.#early.delete(turnId);
    if (this.#sending === 0) this.#early.clear();
    if (!notification) {
      if (this.#ended) throw this.#ended;
      notification = await this.#wait(turnId, opts?.signal);
    }
    const result = notification.Result;
    if (!result) {
      throw new AgentdError(notification.Error?.Message ?? "turn ended without a result", undefined, notification.Error ?? undefined);
    }
    // An interrupted turn still returns its result; its Status says so.
    if (result.Status === "failed") {
      throw new AgentdError(result.Err?.Message ?? "turn failed", undefined, result.Err ?? undefined, result);
    }
    return result;
  }

  #wait(turnId: string, signal: AbortSignal | undefined): Promise<TurnResultNotification> {
    return new Promise((resolve, reject) => {
      const onAbort = () => {
        if (!this.#waiters.delete(turnId)) return;
        this.agentd.call("session/interruptTurn", { Session: this.id, TurnID: turnId }).catch(() => {});
        reject(signal!.reason);
      };
      this.#waiters.set(turnId, {
        resolve: (value) => {
          signal?.removeEventListener("abort", onAbort);
          resolve(value as TurnResultNotification);
        },
        reject: (err) => {
          signal?.removeEventListener("abort", onAbort);
          reject(err);
        },
      });
      if (signal?.aborted) onAbort();
      else signal?.addEventListener("abort", onAbort, { once: true });
    });
  }

  /** Interrupts the running turn. */
  async interrupt(opts?: CallOptions): Promise<void> {
    await this.agentd.call("session/interrupt", { Session: this.id }, opts);
  }

  /** Closes the session; runs still waiting reject. */
  async close(opts?: CallOptions): Promise<void> {
    await this.agentd.call("session/close", { Session: this.id }, opts);
    this.#finish(closedError("session closed"));
  }

  #finish(err: AgentdError): void {
    if (this.#ended) return;
    this.#ended = err;
    exitCallbacks.get(this.agentd)?.delete(this.#onExit);
    for (const unsubscribe of this.#unsubscribe) unsubscribe();
    for (const queue of this.#iterators) queue.end();
    this.#iterators.clear();
    const waiters = [...this.#waiters.values()];
    this.#waiters.clear();
    this.#early.clear();
    for (const waiter of waiters) waiter.reject(err);
  }
}

function toInput(input: AgentInput | string): AgentInput {
  return typeof input === "string" ? { Parts: [{ Type: "text", Text: input }] } : input;
}

class EventQueue implements AsyncIterable<EventNotification>, AsyncIterator<EventNotification> {
  readonly #items: EventNotification[] = [];
  #wake: ((r: IteratorResult<EventNotification>) => void) | undefined;
  #done = false;
  readonly #detach: () => void;

  constructor(detach: () => void) {
    this.#detach = detach;
  }

  push(item: EventNotification): void {
    if (this.#done) return;
    if (this.#wake) {
      const wake = this.#wake;
      this.#wake = undefined;
      wake({ value: item, done: false });
    } else {
      this.#items.push(item);
    }
  }

  end(): void {
    this.#done = true;
    if (this.#wake) {
      const wake = this.#wake;
      this.#wake = undefined;
      wake({ value: undefined, done: true });
    }
  }

  next(): Promise<IteratorResult<EventNotification>> {
    const item = this.#items.shift();
    if (item) return Promise.resolve({ value: item, done: false });
    if (this.#done) return Promise.resolve({ value: undefined, done: true });
    return new Promise((resolve) => (this.#wake = resolve));
  }

  return(): Promise<IteratorResult<EventNotification>> {
    this.#items.length = 0;
    this.#detach();
    this.end();
    return Promise.resolve({ value: undefined, done: true });
  }

  [Symbol.asyncIterator](): AsyncIterator<EventNotification> {
    return this;
  }
}
