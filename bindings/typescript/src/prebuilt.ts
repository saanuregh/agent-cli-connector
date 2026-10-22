import { createHash } from "node:crypto";
import { existsSync } from "node:fs";
import { chmod, mkdir, rename, rm, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import { delimiter, join } from "node:path";
import { LIBRARY_VERSION } from "./protocol.ts";

/** The release platform name, or an error message explaining why the release does not cover this machine. */
function releasePlatform(): { name: string } | { error: string } {
  const arch = process.arch === "x64" ? "x86_64" : process.arch === "arm64" ? "aarch64" : undefined;
  const os = process.platform === "linux" ? "linux" : process.platform === "darwin" ? "macos" : process.platform === "win32" ? "windows" : undefined;
  const name = `${process.platform}-${process.arch}`;
  if (!arch || !os || (os === "windows" && arch !== "x86_64")) {
    return { error: `the release has no build for platform ${name}` };
  }
  if (os === "linux") {
    const header = (process.report?.getReport() as { header?: { glibcVersionRuntime?: string } } | undefined)?.header;
    if (!header?.glibcVersionRuntime) return { error: `the release covers glibc Linux only, and platform ${os}-${arch} uses musl` };
  }
  return { name: `${os}-${arch}` };
}

export interface PrebuiltFile {
  /** The environment variable that points at a local build. */
  envVar: string;
  /** The release file name for a platform. */
  releaseName: (platform: string) => string;
  executable: boolean;
}

export const sharedLibraryFile: PrebuiltFile = {
  envVar: "AGENT_CLI_CONNECTOR_LIB",
  releaseName: (p) =>
    process.platform === "win32" ? `agent-cli-connector-${p}.dll` : process.platform === "darwin" ? `libagent-cli-connector-${p}.dylib` : `libagent-cli-connector-${p}.so`,
  executable: false,
};

export const agentdFile: PrebuiltFile = {
  envVar: "AGENTD_PATH",
  releaseName: (p) => (process.platform === "win32" ? `agentd-${p}.exe` : `agentd-${p}`),
  executable: true,
};

function cacheRoot(): string {
  const env = process.env;
  if (process.platform === "win32") return env.LOCALAPPDATA || join(homedir(), "AppData", "Local");
  if (process.platform === "darwin") return join(homedir(), "Library", "Caches");
  return env.XDG_CACHE_HOME || join(homedir(), ".cache");
}

function cacheDir(platform: string): string {
  return join(cacheRoot(), "agent-cli-connector", LIBRARY_VERSION, platform);
}

/** The cached file, if the platform is covered and the file is already there. */
export function cachedPath(file: PrebuiltFile): string | undefined {
  const platform = releasePlatform();
  if (!("name" in platform)) return undefined;
  const path = join(cacheDir(platform.name), file.releaseName(platform.name));
  return existsSync(path) ? path : undefined;
}

/** `name` on PATH, if present. */
export function findOnPath(name: string): string | undefined {
  const exts = process.platform === "win32" ? (process.env.PATHEXT ?? ".EXE").split(";") : [""];
  for (const dir of (process.env.PATH ?? "").split(delimiter)) {
    if (!dir) continue;
    for (const ext of exts) {
      const path = join(dir, name + ext);
      if (existsSync(path)) return path;
    }
  }
  return undefined;
}

function releaseBase(): string {
  const base = process.env.AGENT_CLI_CONNECTOR_RELEASE_URL || `https://github.com/saanuregh/agent-cli-connector/releases/download/v${LIBRARY_VERSION}/`;
  return base.endsWith("/") ? base : base + "/";
}

async function get(url: string): Promise<Buffer> {
  const res = await fetch(url, { redirect: "follow" });
  if (!res.ok) throw new Error(`GET ${url}: HTTP ${res.status}`);
  return Buffer.from(await res.arrayBuffer());
}

/** One download per file per process; a failed download may be retried. */
const downloads = new Map<string, Promise<string>>();

/**
 * The cached release file, downloaded and checked against `SHA256SUMS` if absent.
 * Throws with the environment variable to set when downloading is off or impossible.
 */
export function fetchPrebuilt(file: PrebuiltFile): Promise<string> {
  const platform = releasePlatform();
  if (!("name" in platform)) {
    return Promise.reject(new Error(`${platform.error}; build the library with zig and set ${file.envVar}`));
  }
  const name = file.releaseName(platform.name);
  const dir = cacheDir(platform.name);
  const path = join(dir, name);
  if (existsSync(path)) return Promise.resolve(path);
  if (process.env.AGENT_CLI_CONNECTOR_NO_DOWNLOAD === "1") {
    return Promise.reject(
      new Error(`${name} for platform ${platform.name} is not built or cached, and AGENT_CLI_CONNECTOR_NO_DOWNLOAD=1 turns downloading off; set ${file.envVar}`),
    );
  }
  let pending = downloads.get(path);
  if (!pending) {
    pending = download(releaseBase(), name, dir, path, file.executable).catch((err: Error) => {
      downloads.delete(path);
      throw new Error(`downloading ${name} for platform ${platform.name} failed: ${err.message}; set ${file.envVar} to a local build instead`);
    });
    downloads.set(path, pending);
  }
  return pending;
}

async function download(base: string, name: string, dir: string, path: string, executable: boolean): Promise<string> {
  const sums = (await get(base + "SHA256SUMS")).toString("utf8");
  const line = sums.split("\n").find((l) => l.trim().split(/\s+\*?/)[1] === name);
  const expected = line?.trim().split(/\s+/)[0]?.toLowerCase();
  if (!expected || !/^[0-9a-f]{64}$/.test(expected)) throw new Error(`SHA256SUMS has no line for ${name}`);
  const body = await get(base + name);
  const actual = createHash("sha256").update(body).digest("hex");
  if (actual !== expected) throw new Error(`checksum mismatch for ${name}: expected ${expected}, got ${actual}`);
  await mkdir(dir, { recursive: true });
  const temp = `${path}.${process.pid}.${Date.now()}.tmp`;
  try {
    await writeFile(temp, body, { mode: executable ? 0o755 : 0o644 });
    if (executable) await chmod(temp, 0o755);
    await rename(temp, path);
  } catch (err) {
    await rm(temp, { force: true });
    throw err;
  }
  return path;
}
