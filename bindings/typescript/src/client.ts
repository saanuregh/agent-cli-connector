import { randomUUID } from "node:crypto";
import type {
  Capabilities,
  ClosedNotification,
  Config,
  DialogResponse,
  ElicitationResponse,
  ElicitationResponseAction,
  ErrorInfo,
  ErrorKind,
  Event,
  EventNotification,
  EventOf,
  Input,
  Methods,
  NativeResponse,
  PermissionDecision,
  PermissionResponse,
  ProviderInfo,
  RespondParams,
  SessionInfo,
  StartResult,
  ToolCallParams,
  ToolCancelParams,
  ToolCallRequest,
  ToolDef,
  ToolResult,
  TurnResult,
  UserInputResponse,
} from "./protocol.ts";
import { ClientMethods, SessionMethods } from "./methods.ts";
import { AgentdTransport, NativeTransport, resolveAgentdPath, resolveLibraryPath, type AgentdOptions, type Transport } from "./transport.ts";

export const PROTOCOL_VERSION = 1;

/** A failure reported by the library, a closed connection, or a closed session. Match on `kind`. */
export class AgentError extends Error {
  readonly info: ErrorInfo;
  /** The JSON-RPC error code, when the error came from a request. */
  readonly code: number | undefined;

  /** `info` may be partial; missing fields take their defaults. */
  constructor(info: Partial<ErrorInfo>, code?: number) {
    super(info.message || info.kind || "internal");
    this.name = "AgentError";
    this.info = { kind: "internal", rpc_code: 0, http_status: 0, resets_at_ms: 0, ...info };
    this.code = code;
  }

  get kind(): ErrorKind {
    return this.info.kind;
  }
}

function closedError(message: string, cause?: ErrorInfo): AgentError {
  return new AgentError({ ...cause, kind: "closed", message: cause?.message || message });
}

// ---------------------------------------------------------------- events

const requestTypes = ["permission_request", "user_input_request", "elicitation_request", "dialog_request", "native_request"] as const;
type RequestType = (typeof requestTypes)[number];

function isRequest(ev: Event): ev is EventOf<RequestType> {
  return (requestTypes as readonly string[]).includes(ev.type);
}

/** Set on a request event a session option's handler answers; the event is still delivered. */
export interface RequestHandledMarker {
  readonly handled?: true;
}

export interface PermissionRequestHelpers {
  respond(response: PermissionResponse): Promise<void>;
  /** Allows the call; `decision` defaults to "allow". */
  allow(response?: PermissionResponse): Promise<void>;
  deny(message?: string): Promise<void>;
}

export interface UserInputRequestHelpers {
  respond(response: UserInputResponse): Promise<void>;
  /** Answers by question ID. */
  answer(answers: Record<string, string | string[]>): Promise<void>;
  cancel(): Promise<void>;
}

export interface ElicitationRequestHelpers {
  respond(response: ElicitationResponse): Promise<void>;
  accept(content?: unknown): Promise<void>;
  decline(): Promise<void>;
  cancel(): Promise<void>;
}

export interface DialogRequestHelpers {
  respond(response: DialogResponse): Promise<void>;
  /** Completes the dialog with an optional result. */
  complete(result?: unknown): Promise<void>;
  cancel(): Promise<void>;
}

export interface NativeRequestHelpers {
  respond(response: NativeResponse): Promise<void>;
  /** Answers with a JSON-RPC result. */
  reply(result?: unknown): Promise<void>;
  /** Answers with a JSON-RPC error. */
  fail(code: number, message: string): Promise<void>;
}

export type PermissionRequestEvent = EventOf<"permission_request"> & PermissionRequestHelpers & RequestHandledMarker;
export type UserInputRequestEvent = EventOf<"user_input_request"> & UserInputRequestHelpers & RequestHandledMarker;
export type ElicitationRequestEvent = EventOf<"elicitation_request"> & ElicitationRequestHelpers & RequestHandledMarker;
export type DialogRequestEvent = EventOf<"dialog_request"> & DialogRequestHelpers & RequestHandledMarker;
export type NativeRequestEvent = EventOf<"native_request"> & NativeRequestHelpers & RequestHandledMarker;

/** An event of a session. Narrow on `type`; request events carry answering helpers. */
export type SessionEvent =
  | Exclude<Event, { type: RequestType }>
  | PermissionRequestEvent
  | UserInputRequestEvent
  | ElicitationRequestEvent
  | DialogRequestEvent
  | NativeRequestEvent;

/** The read items a channel drops at once, so a long backlog does not copy on every read. */
const channelCompactAfter = 1024;

/** An unbounded queue read with `for await`. */
class Channel<T> {
  #items: T[] = [];
  #head = 0;
  #wake: (() => void)[] = [];
  #done = false;

  push(item: T): void {
    if (this.#done) return;
    this.#items.push(item);
    this.#wakeReaders();
  }

  end(): void {
    this.#done = true;
    this.#wakeReaders();
  }

  async *read(): AsyncGenerator<T, void, undefined> {
    while (true) {
      if (this.#head < this.#items.length) yield this.#take();
      else if (this.#done) return;
      else await new Promise<void>((resolve) => this.#wake.push(resolve));
    }
  }

  #wakeReaders(): void {
    if (this.#wake.length > 0) this.#wake.splice(0).forEach((w) => w());
  }

  #take(): T {
    const item = this.#items[this.#head++];
    if (this.#head === this.#items.length) {
      this.#items = [];
      this.#head = 0;
    } else if (this.#head >= channelCompactAfter && this.#head * 2 >= this.#items.length) {
      this.#items = this.#items.slice(this.#head);
      this.#head = 0;
    }
    return item;
  }
}

// ---------------------------------------------------------------- tools and handlers

export interface ToolOptions<Args> extends Omit<ToolDef, "input_schema"> {
  /** JSON Schema of the arguments. */
  inputSchema?: unknown;
  /**
   * Runs the tool. A string becomes a text result; a thrown error becomes an error result.
   * `signal` aborts when the turn is interrupted or the session closes.
   */
  run(args: Args, call: ToolCallRequest, signal: AbortSignal): Promise<string | ToolResult>;
}

/** A host tool the agent can call; create with `tool()`. */
export interface Tool {
  readonly definition: ToolDef;
  call(call: ToolCallRequest, signal: AbortSignal): Promise<ToolResult>;
}

/** Defines a host tool. `Args` types the decoded arguments; they are not validated. */
export function tool<Args = any>(options: ToolOptions<Args>): Tool {
  const { inputSchema, run, ...rest } = options;
  const definition: ToolDef = { ...rest, input_schema: inputSchema };
  return {
    definition,
    async call(call, signal) {
      try {
        const out = await run((call.arguments ?? {}) as Args, call, signal);
        return typeof out === "string" ? { content: [{ type: "text", text: out }] } : out;
      } catch (err) {
        return { content: [{ type: "text", text: err instanceof Error ? err.message : String(err) }], is_error: true };
      }
    },
  };
}

// Handlers return only promises: with TypeScript 7, a `T | Promise<T>` return
// type does not contextually type the literals an async function returns.
export interface SessionOptions extends Omit<Config, "tools"> {
  tools?: Tool[];
  /** Answers permission requests. Without it they wait for `respond` or the event's helpers. */
  onPermission?: (request: PermissionRequestEvent) => Promise<PermissionDecision | PermissionResponse>;
  onUserInput?: (request: UserInputRequestEvent) => Promise<UserInputResponse>;
  onElicitation?: (request: ElicitationRequestEvent) => Promise<ElicitationResponseAction | ElicitationResponse>;
  /** Answers dialog requests. A thrown error cancels the dialog. */
  onDialog?: (request: DialogRequestEvent) => Promise<DialogResponse>;
  /** Answers provider-native requests. A thrown error answers with JSON-RPC error -32603 and its message. */
  onNativeRequest?: (request: NativeRequestEvent) => Promise<NativeResponse>;
}

export interface WaitOptions {
  /** Gives up after this many milliseconds; `run` then interrupts the turn. */
  timeoutMs?: number;
}

// ---------------------------------------------------------------- client

interface Message {
  jsonrpc?: string;
  id?: number | string | null;
  method?: string;
  params?: any;
  result?: unknown;
  error?: { code: number; message: string; data?: ErrorInfo };
}

/** Module-private hooks between Client and Session. */
const deliver = Symbol("deliver");
const closeLocal = Symbol("closeLocal");
const write = Symbol("write");
const openSession = Symbol("openSession");
const started = Symbol("started");

export interface ClientOptions {
  /** "native" (default) loads the library in process; "agentd" runs the agentd executable. */
  transport?: "native" | "agentd" | Transport;
  /** Providers to offer, by name, with their options. Omitted offers every real harness. */
  providers?: Record<string, unknown>;
  /** The shared library for the native transport; defaults to `AGENT_CLI_CONNECTOR_LIB`, this repository's build, or the downloaded release. */
  libraryPath?: string;
  /** Options for the agentd transport; the path defaults to `AGENTD_PATH`, this repository's build, `PATH`, or the downloaded release. */
  agentd?: AgentdOptions;
}

/** One connection to the library: its providers and sessions. */
export class Client extends ClientMethods {
  providers: ProviderInfo[] = [];
  libraryVersion = "";

  readonly #transport: Transport;
  #nextId = 1;
  readonly #pending = new Map<number, { resolve: (v: any) => void; reject: (e: Error) => void }>();
  readonly #sessions = new Map<string, Session>();
  #closed: AgentError | undefined;

  private constructor(transport: Transport) {
    super();
    this.#transport = transport;
  }

  /** Connects and initializes. */
  static async open(options: ClientOptions = {}): Promise<Client> {
    const t = options.transport ?? "native";
    const transport =
      t === "native"
        ? new NativeTransport(options.libraryPath ?? (await resolveLibraryPath()))
        : t === "agentd"
          ? new AgentdTransport({ ...options.agentd, path: options.agentd?.path ?? (await resolveAgentdPath()) })
          : t;
    const client = new Client(transport);
    transport.start(
      (text) => client.#receive(text),
      (err) => client.#onClose(err),
    );
    try {
      const res = await client.request("initialize", { protocol_version: PROTOCOL_VERSION, providers: options.providers });
      client.providers = res.providers;
      client.libraryVersion = res.library_version;
    } catch (err) {
      await client.close();
      throw err;
    }
    return client;
  }

  /** Sends any protocol request. */
  request<M extends keyof Methods>(method: M, params: Methods[M]["params"]): Promise<Methods[M]["result"]> {
    if (this.#closed) return Promise.reject(this.#closed);
    const id = this.#nextId++;
    return new Promise((resolve, reject) => {
      this.#pending.set(id, { resolve, reject });
      try {
        this[write]({ jsonrpc: "2.0", id, method, params });
      } catch (err) {
        this.#pending.delete(id);
        reject(err);
      }
    });
  }

  /** Starts a session on a provider. */
  start(provider: string, options: SessionOptions = {}): Promise<Session> {
    return this[openSession]("session/start", { provider }, provider, options);
  }

  /** Closes every session and the connection. */
  async close(): Promise<void> {
    await this.#transport.close();
  }

  async [Symbol.asyncDispose](): Promise<void> {
    await this.close();
  }

  [write](msg: Message): void {
    this.#transport.send(JSON.stringify(msg));
  }

  async [openSession](
    method: "session/start" | "session/fork",
    target: { provider: string } | { session: string },
    provider: string,
    options: SessionOptions,
  ): Promise<Session> {
    const { tools = [], onPermission, onUserInput, onElicitation, onDialog, onNativeRequest, ...config } = options;
    const fullConfig: Config = { ...config, tools: tools.length > 0 ? tools.map((t) => t.definition) : undefined };
    // The session exists before the request names its handle, so every message finds it.
    const session = new Session(this, randomUUID(), provider, options);
    this.#sessions.set(session.handle, session);
    let res: StartResult;
    try {
      res = await this.request(method, { ...target, config: fullConfig, handle: session.handle } as any);
      if (res.session !== session.handle) throw new AgentError({ kind: "protocol", message: `invalid ${method} result` });
    } catch (err) {
      this.#sessions.delete(session.handle);
      // The library may have started the session before the failure; nobody owns it now.
      // Closing an unknown handle fails harmlessly.
      this.request("session/close", { session: session.handle }).catch(() => {});
      throw err;
    }
    session[started](res);
    return session;
  }

  #receive(text: string): void {
    let msg: Message;
    try {
      msg = JSON.parse(text);
    } catch {
      return;
    }
    if (msg.method === undefined) {
      if (typeof msg.id !== "number") return;
      const pending = this.#pending.get(msg.id);
      if (!pending) return;
      this.#pending.delete(msg.id);
      if (msg.error) pending.reject(new AgentError(msg.error.data ?? { message: msg.error.message }, msg.error.code));
      else pending.resolve(msg.result ?? null);
      return;
    }
    const handle: unknown = msg.params?.session;
    if (typeof handle === "string" && (msg.method.startsWith("session/") || msg.method.startsWith("tool/"))) {
      this.#route(handle, msg);
    } else if (msg.id !== undefined && msg.id !== null) {
      this[write]({ jsonrpc: "2.0", id: msg.id, error: { code: -32601, message: `unknown method ${msg.method}` } });
    }
  }

  #route(handle: string, msg: Message): void {
    const session = this.#sessions.get(handle);
    if (session) {
      if (msg.method === "session/closed") this.#sessions.delete(handle);
      session[deliver](msg);
    } else if (msg.id !== undefined && msg.id !== null) {
      const result: ToolResult = { content: [{ type: "text", text: "unknown session" }], is_error: true };
      this[write]({ jsonrpc: "2.0", id: msg.id, result });
    }
  }

  #onClose(err?: Error): void {
    this.#closed = closedError(err ? err.message : "connection closed");
    for (const p of this.#pending.values()) p.reject(this.#closed);
    this.#pending.clear();
    for (const s of this.#sessions.values()) s[closeLocal](this.#closed.info);
    this.#sessions.clear();
  }
}

// ---------------------------------------------------------------- session

/** Runs a request handler off the reader and sends its answer, or `fallback` if it throws. */
function answerInBackground<T>(
  handler: () => Promise<T>,
  respond: (v: T) => Promise<void>,
  fallback: (err: unknown) => Promise<void>,
): void {
  Promise.resolve()
    .then(handler)
    .then(respond, fallback)
    // The request may have been withdrawn or answered elsewhere meanwhile.
    .catch(() => {});
}

/** A running conversation with one harness. */
export class Session extends SessionMethods {
  /** The library's handle for the session, used on the wire. */
  readonly handle: string;
  readonly provider: string;
  /** Resolves when the session closes, with the error that closed it, if any. */
  readonly closed: Promise<ErrorInfo | undefined>;

  readonly #client: Client;
  readonly #options: SessionOptions;
  readonly #tools: Map<string, Tool>;
  /** Running tool calls by ID, aborted by `tool/cancel`. */
  readonly #toolCalls = new Map<string, AbortController>();
  #capabilities: Capabilities = {};
  #info: SessionInfo = { context_window: 0 };
  /** Set once an event reported info, which is then newer than the start result. */
  #infoReported = false;
  /** The `events()` stream, created by its first call. */
  #events: Channel<SessionEvent> | undefined;
  /** Kept for the first `events()` call, which yields it first. */
  #startedEvent: SessionEvent | undefined;
  readonly #listeners = new Set<(event: SessionEvent) => void>();
  /** The events of the turns `send` started, by ID, from before the request until the turn ends. */
  readonly #turns = new Map<string, Channel<SessionEvent>>();
  #resolveClosed!: (info: ErrorInfo | undefined) => void;
  #isClosed = false;

  /** Use `Client.start` or `Session.fork`. */
  constructor(client: Client, handle: string, provider: string, options: SessionOptions) {
    super();
    this.#client = client;
    this.handle = handle;
    this.provider = provider;
    this.#options = options;
    this.#tools = new Map((options.tools ?? []).map((t) => [t.definition.name, t]));
    this.closed = new Promise((resolve) => (this.#resolveClosed = resolve));
  }

  get capabilities(): Capabilities {
    return this.#capabilities;
  }

  [started](res: StartResult): void {
    this.#capabilities = res.capabilities;
    if (!this.#infoReported) this.#info = res.info;
  }

  /** The harness's own session ID; it can change, as after a reset. */
  get id(): string {
    return this.#info.id ?? "";
  }

  /** The latest session info, from the start result, session events, or `fetchInfo`. */
  get info(): SessionInfo {
    return this.#info;
  }

  /**
   * Every event of the session in arrival order: `session_started` first,
   * then every event from the first call on, buffered until read; other
   * earlier events are not kept. One stream: concurrent
   * readers share it. Ends when the session closes.
   */
  events(): AsyncGenerator<SessionEvent, void, undefined> {
    if (!this.#events) {
      this.#events = new Channel<SessionEvent>();
      if (this.#startedEvent) this.#events.push(this.#startedEvent);
      this.#startedEvent = undefined;
      if (this.#isClosed) this.#events.end();
    }
    return this.#events.read();
  }

  /**
   * Calls `listener` with every later event synchronously, in order. Errors it
   * throws are ignored. Returns a function that unsubscribes it.
   */
  subscribe(listener: (event: SessionEvent) => void): () => void {
    const entry = (event: SessionEvent) => listener(event);
    this.#listeners.add(entry);
    return () => void this.#listeners.delete(entry);
  }

  /** Sends input; the turn runs now or after the queued ones. */
  async send(input: string | Input): Promise<Turn> {
    const turnId = randomUUID();
    const channel = new Channel<SessionEvent>();
    if (this.#isClosed) channel.end();
    else this.#turns.set(turnId, channel);
    try {
      await this.#request("session/send", { input: typeof input === "string" ? { text: input } : input, turn_id: turnId });
    } catch (err) {
      this.#turns.delete(turnId);
      throw err;
    }
    return new Turn(this, turnId, channel);
  }

  /** Sends input and waits for its turn to end. A failed turn resolves; check `status`. */
  async run(input: string | Input, options: WaitOptions = {}): Promise<TurnResult> {
    return (await this.send(input)).wait(options);
  }

  /** Waits for a turn to end. A failed or interrupted turn resolves; check `status`. */
  async wait(turnId: string, options: WaitOptions = {}): Promise<TurnResult> {
    return await this.#request("session/wait", { turn_id: turnId, timeout_ms: options.timeoutMs });
  }

  /** Forks the conversation into a new session. */
  fork(options: SessionOptions = {}): Promise<Session> {
    return this.#client[openSession]("session/fork", { session: this.handle }, this.provider, options);
  }

  /** Asks the library for the session info and updates `info`. */
  async fetchInfo(): Promise<SessionInfo> {
    this.#info = await this.#request("session/info", {});
    return this.#info;
  }

  /** Requests still waiting for an answer, with answering helpers. */
  async pendingRequests(): Promise<SessionEvent[]> {
    return (await this.#request("session/pendingRequests", {})).map((ev) => this.#bind(ev));
  }

  /** Answers a request; set the field matching its kind. */
  async respond(requestId: string, response: Omit<RespondParams, "session" | "request_id">): Promise<void> {
    await this.#request("request/respond", { request_id: requestId, ...response });
  }

  /** Closes the session and waits for it to end. Closing twice is fine. */
  async close(): Promise<void> {
    if (!this.#isClosed) {
      try {
        await this.#request("session/close", {});
      } catch (err) {
        if (!(err instanceof AgentError && (err.kind === "session_not_found" || err.kind === "closed"))) throw err;
        this[closeLocal](undefined);
      }
    }
    await this.closed;
  }

  async [Symbol.asyncDispose](): Promise<void> {
    await this.close();
  }

  #request<M extends keyof Methods>(method: M, params: Omit<Methods[M]["params"], "session">): Promise<Methods[M]["result"]> {
    return this.#client.request(method, { ...params, session: this.handle } as Methods[M]["params"]);
  }

  protected sessionRequest<M extends keyof Methods>(
    method: M,
    params: Omit<Methods[M]["params"], "session">,
  ): Promise<Methods[M]["result"]> {
    return this.#request(method, params);
  }

  [deliver](msg: Message): void {
    switch (msg.method) {
      case "session/event":
        return this.#onEvent((msg.params as EventNotification).event);
      case "session/closed":
        return this[closeLocal]((msg.params as ClosedNotification).error);
      case "tool/call":
        return void this.#onToolCall(msg.id!, msg.params as ToolCallParams);
      case "tool/cancel":
        return this.#toolCalls.get((msg.params as ToolCancelParams).call_id)?.abort();
    }
  }

  [closeLocal](error: ErrorInfo | undefined): void {
    if (this.#isClosed) return;
    this.#isClosed = true;
    this.#events?.end();
    for (const c of this.#turns.values()) c.end();
    this.#turns.clear();
    this.#resolveClosed(error);
  }

  #onEvent(raw: Event): void {
    const ev = this.#bind(raw);
    if (ev.type === "session_started" || ev.type === "session_updated") {
      this.#info = ev.info;
      this.#infoReported = true;
    }
    if (ev.type === "session_started" && !this.#events) this.#startedEvent = ev;
    const handled = this.#autoAnswer(ev);
    if (handled) Object.defineProperty(ev, "handled", { value: true, enumerable: false });
    this.#events?.push(ev);
    for (const listener of this.#listeners) {
      try {
        listener(ev);
      } catch {
        // A failing subscriber must not stop delivery to the others.
      }
    }
    const channel = ev.turn_id ? this.#turns.get(ev.turn_id) : undefined;
    if (channel) {
      channel.push(ev);
      if (ev.type === "turn_completed") {
        channel.end();
        this.#turns.delete(ev.turn_id!);
      }
    }
  }

  /** Starts answering `ev` with the matching session option's handler; returns whether one exists. */
  #autoAnswer(ev: SessionEvent): boolean {
    if (!isRequest(ev)) return false;
    const o = this.#options;
    if (ev.type === "permission_request" && o.onPermission) {
      const h = o.onPermission;
      answerInBackground(
        () => h(ev),
        (v) => ev.respond(typeof v === "string" ? { decision: v } : v),
        (err) => ev.deny(err instanceof Error ? err.message : String(err)),
      );
    } else if (ev.type === "user_input_request" && o.onUserInput) {
      const h = o.onUserInput;
      answerInBackground(() => h(ev), (v) => ev.respond(v), () => ev.cancel());
    } else if (ev.type === "elicitation_request" && o.onElicitation) {
      const h = o.onElicitation;
      answerInBackground(
        () => h(ev),
        (v) => ev.respond(typeof v === "string" ? { action: v } : v),
        () => ev.cancel(),
      );
    } else if (ev.type === "dialog_request" && o.onDialog) {
      const h = o.onDialog;
      answerInBackground(() => h(ev), (v) => ev.respond(v), () => ev.cancel());
    } else if (ev.type === "native_request" && o.onNativeRequest) {
      const h = o.onNativeRequest;
      answerInBackground(
        () => h(ev),
        (v) => ev.respond(v),
        (err) => ev.fail(-32603, err instanceof Error ? err.message : String(err)),
      );
    } else {
      return false;
    }
    return true;
  }

  /** Adds the answering helpers to request events. They are not enumerable, so the event still serializes as wire JSON. */
  #bind(ev: Event): SessionEvent {
    if (!isRequest(ev)) return ev;
    const id = ev.id ?? "";
    let helpers:
      | PermissionRequestHelpers
      | UserInputRequestHelpers
      | ElicitationRequestHelpers
      | DialogRequestHelpers
      | NativeRequestHelpers;
    if (ev.type === "permission_request") {
      const respond = (permission: PermissionResponse) => this.respond(id, { permission });
      helpers = {
        respond,
        allow: (r = {}) => respond({ ...r, decision: r.decision ?? "allow" }),
        deny: (message) => respond({ decision: "deny", message }),
      };
    } else if (ev.type === "user_input_request") {
      const respond = (user_input: UserInputResponse) => this.respond(id, { user_input });
      helpers = {
        respond,
        answer: (answers) =>
          respond({ answers: Object.fromEntries(Object.entries(answers).map(([k, v]) => [k, Array.isArray(v) ? v : [v]])) }),
        cancel: () => respond({ cancelled: true }),
      };
    } else if (ev.type === "dialog_request") {
      const respond = (dialog: DialogResponse) => this.respond(id, { dialog });
      helpers = {
        respond,
        complete: (result) => respond({ behavior: "completed", result }),
        cancel: () => respond({ behavior: "cancelled" }),
      };
    } else if (ev.type === "native_request") {
      const respond = (native: NativeResponse) => this.respond(id, { native });
      helpers = {
        respond,
        reply: (result) => respond({ result }),
        fail: (code, message) => respond({ error: { code, message } }),
      };
    } else {
      const respond = (elicitation: ElicitationResponse) => this.respond(id, { elicitation });
      helpers = {
        respond,
        accept: (content) => respond({ action: "accept", content }),
        decline: () => respond({ action: "decline" }),
        cancel: () => respond({ action: "cancel" }),
      };
    }
    for (const [name, fn] of Object.entries(helpers)) Object.defineProperty(ev, name, { value: fn, enumerable: false });
    return ev as SessionEvent;
  }

  async #onToolCall(id: number | string, params: ToolCallParams): Promise<void> {
    const t = this.#tools.get(params.call.name ?? "");
    const controller = new AbortController();
    const callId = params.call.id ?? "";
    this.#toolCalls.set(callId, controller);
    let result: ToolResult;
    try {
      result = t
        ? await t.call(params.call, controller.signal)
        : { content: [{ type: "text", text: `unknown tool ${params.call.name}` }], is_error: true };
    } finally {
      this.#toolCalls.delete(callId);
    }
    try {
      this.#client[write]({ jsonrpc: "2.0", id, result });
    } catch {
      // The connection closed; the library already gave up on the call.
    }
  }
}

// ---------------------------------------------------------------- turn

/** One input's turn. */
export class Turn {
  readonly id: string;
  readonly session: Session;
  readonly #channel: Channel<SessionEvent>;

  /** Use `Session.send`. */
  constructor(session: Session, id: string, channel: Channel<SessionEvent>) {
    this.session = session;
    this.id = id;
    this.#channel = channel;
  }

  /** The turn's events, from its first, ending after `turn_completed`. Read it once. */
  events(): AsyncGenerator<SessionEvent, void, undefined> {
    return this.#channel.read();
  }

  /**
   * Waits for the turn to end. A failed or interrupted turn resolves; check
   * `status`. On timeout the turn is interrupted and an AgentError thrown.
   */
  async wait(options: WaitOptions = {}): Promise<TurnResult> {
    try {
      return await this.session.wait(this.id, options);
    } catch (err) {
      if (err instanceof AgentError && err.kind === "timeout") await this.interrupt().catch(() => {});
      else this.#channel.end();
      throw err;
    }
  }

  async interrupt(): Promise<void> {
    await this.session.interruptTurn(this.id);
  }
}
