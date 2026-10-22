import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import koffi, { type LibraryHandle } from "koffi";

type KoffiFunction = ReturnType<LibraryHandle["func"]>;

/**
 * A JSON-RPC message pipe to the library: each message is one JSON text.
 * Messages are delivered in arrival order on the main thread.
 */
export interface Transport {
  /** Starts reading. `onClose` runs once, after the last message. */
  start(onMessage: (text: string) => void, onClose: (error?: Error) => void): void;
  send(text: string): void;
  /** Closes the connection and resolves once reading stopped. */
  close(): Promise<void>;
}

const repoRoot = fileURLToPath(new URL("../../../", import.meta.url));

/** How long one `agent_recv_batch` call blocks before the loop checks again. */
const recvTimeoutMs = 1000;

interface NativeLibrary {
  open: KoffiFunction;
  send: KoffiFunction;
  recv: KoffiFunction;
  free: KoffiFunction;
  close: KoffiFunction;
  destroy: KoffiFunction;
}

const loaded = new Map<string, NativeLibrary>();

/** The shared library: `AGENT_CLI_CONNECTOR_LIB`, else this repository's `zig-out`. */
export function defaultLibraryPath(): string {
  const fromEnv = process.env.AGENT_CLI_CONNECTOR_LIB;
  if (fromEnv) return fromEnv;
  const candidates =
    process.platform === "win32"
      ? ["zig-out/bin/agent-cli-connector.dll", "zig-out/lib/agent-cli-connector.dll"]
      : process.platform === "darwin"
        ? ["zig-out/lib/libagent-cli-connector.dylib"]
        : ["zig-out/lib/libagent-cli-connector.so"];
  const paths = candidates.map((c) => repoRoot + c);
  return paths.find((p) => existsSync(p)) ?? paths[0]!;
}

/** The agentd executable: `AGENTD_PATH`, else this repository's `zig-out/bin`, else `agentd` on PATH. */
export function defaultAgentdPath(): string {
  const fromEnv = process.env.AGENTD_PATH;
  if (fromEnv) return fromEnv;
  const local = repoRoot + (process.platform === "win32" ? "zig-out/bin/agentd.exe" : "zig-out/bin/agentd");
  return existsSync(local) ? local : "agentd";
}

function loadLibrary(path: string): NativeLibrary {
  let lib = loaded.get(path);
  if (lib) return lib;
  const handle = koffi.load(path);
  lib = {
    open: handle.func("void *agent_open()"),
    send: handle.func("int agent_send(void *conn, const char *msg, size_t len)"),
    // `void **` keeps the messages as a pointer, so they can be freed after decoding.
    recv: handle.func("int agent_recv_batch(void *conn, int timeout_ms, _Out_ void **out, _Out_ size_t *len)"),
    free: handle.func("void agent_free(void *msg)"),
    close: handle.func("void agent_close(void *conn)"),
    destroy: handle.func("void agent_destroy(void *conn)"),
  };
  loaded.set(path, lib);
  return lib;
}

/** The library loaded in process through koffi. `agent_recv_batch` runs on koffi's worker threads. */
export class NativeTransport implements Transport {
  readonly #lib: NativeLibrary;
  #conn: unknown;
  #closing = false;
  #stopped: Promise<void>;
  #resolveStopped!: () => void;

  constructor(libraryPath = defaultLibraryPath()) {
    this.#lib = loadLibrary(libraryPath);
    this.#conn = this.#lib.open();
    if (!this.#conn) throw new Error("agent_open failed");
    this.#stopped = new Promise((resolve) => (this.#resolveStopped = resolve));
  }

  start(onMessage: (text: string) => void, onClose: (error?: Error) => void): void {
    const lib = this.#lib;
    const loop = () => {
      const out: unknown[] = [null];
      const len: number[] = [0];
      lib.recv.async(this.#conn, recvTimeoutMs, out, len, (err: unknown, status: number) => {
        if (err || status < 0) {
          lib.destroy(this.#conn);
          this.#conn = null;
          onClose(err ? (err as Error) : undefined);
          this.#resolveStopped();
          return;
        }
        if (status === 1) {
          const batch = Buffer.from(koffi.view(out[0], Number(len[0]))).toString("utf8");
          lib.free(out[0]);
          for (const text of batch.split("\n")) if (text !== "") onMessage(text);
        }
        loop();
      });
    };
    loop();
  }

  send(text: string): void {
    if (!this.#conn || this.#closing) throw new Error("connection closed");
    const bytes = Buffer.from(text);
    this.#lib.send(this.#conn, bytes, bytes.length);
  }

  close(): Promise<void> {
    if (!this.#closing && this.#conn) {
      this.#closing = true;
      // Closing waits for sessions to stop; keep it off the main thread.
      this.#lib.close.async(this.#conn, () => {});
    }
    return this.#stopped;
  }
}

export interface AgentdOptions {
  /** The executable; defaults to `defaultAgentdPath()`. */
  path?: string;
  args?: string[];
  env?: NodeJS.ProcessEnv;
}

const stderrLimit = 8192;
/** How long close waits for agentd to exit after its input closes before killing it. */
const exitTimeoutMs = 5000;

/** agentd as a child process, one JSON message per line on its standard input and output. */
export class AgentdTransport implements Transport {
  readonly #child: ChildProcessWithoutNullStreams;
  #stderr = "";
  readonly #exited: Promise<void>;
  #resolveExited!: () => void;

  constructor(options: AgentdOptions = {}) {
    this.#child = spawn(options.path ?? defaultAgentdPath(), options.args ?? [], {
      stdio: ["pipe", "pipe", "pipe"],
      env: options.env ?? process.env,
    });
    this.#exited = new Promise((resolve) => (this.#resolveExited = resolve));
    // Writes after agentd exits fail with EPIPE; the exit is reported by onClose.
    this.#child.stdin.on("error", () => {});
    this.#child.stderr.setEncoding("utf8");
    this.#child.stderr.on("data", (chunk: string) => {
      this.#stderr = (this.#stderr + chunk).slice(-stderrLimit);
    });
  }

  start(onMessage: (text: string) => void, onClose: (error?: Error) => void): void {
    let buffer = "";
    const child = this.#child;
    child.stdout.setEncoding("utf8");
    child.stdout.on("data", (chunk: string) => {
      // The unread rest holds no newline, so the search starts at the new chunk.
      const from = buffer.length;
      buffer += chunk;
      let start = 0;
      let newline = buffer.indexOf("\n", from);
      while (newline >= 0) {
        const line = buffer.slice(start, newline).trim();
        start = newline + 1;
        if (line !== "") onMessage(line);
        newline = buffer.indexOf("\n", start);
      }
      buffer = buffer.slice(start);
    });
    let finished = false;
    const finish = (detail?: string) => {
      if (finished) return;
      finished = true;
      const stderr = this.#stderr.trim();
      onClose(detail ? new Error(`agentd ${detail}${stderr ? `: ${stderr}` : ""}`) : undefined);
      this.#resolveExited();
    };
    child.on("error", (err) => finish(`failed: ${err.message}`));
    child.on("close", (code, signal) => finish(code === 0 ? undefined : `exited (${signal ? `signal ${signal}` : `code ${code}`})`));
  }

  send(text: string): void {
    this.#child.stdin.write(text + "\n");
  }

  async close(): Promise<void> {
    this.#child.stdin.end();
    const timer = setTimeout(() => this.#child.kill(), exitTimeoutMs);
    await this.#exited;
    clearTimeout(timer);
  }
}
