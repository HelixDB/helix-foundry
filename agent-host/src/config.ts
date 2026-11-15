// The helper's state on this computer: the Foundry it is paired with and its
// token, in a file only this user reads, and an empty directory per thread
// that Claude Code runs in.

import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";

/** What pairing keeps: where Foundry is, and the helper's hxh_ token. */
export type Paired = {
  /** The install's integration listener, such as http://127.0.0.1:8081. */
  foundry: string;
  hostId: string;
  name: string;
  token: string;
};

/** The file pairing writes: FOUNDRY_AGENT_HOST_CONFIG, else XDG's. */
export function configPath(env: NodeJS.ProcessEnv = process.env): string {
  if (env.FOUNDRY_AGENT_HOST_CONFIG) return env.FOUNDRY_AGENT_HOST_CONFIG;
  const base = env.XDG_CONFIG_HOME || join(homedir(), ".config");
  return join(base, "helix-foundry", "agent-host.json");
}

/** Where a thread's empty working directory lives: XDG's state directory. */
export function stateDir(env: NodeJS.ProcessEnv = process.env): string {
  if (env.FOUNDRY_AGENT_HOST_STATE) return env.FOUNDRY_AGENT_HOST_STATE;
  const base = env.XDG_STATE_HOME || join(homedir(), ".local", "state");
  return join(base, "helix-foundry", "agent-host");
}

/**
 * The thread's working directory: empty (Claude Code runs with no tool
 * that writes), and the same for every turn of the thread, since Claude
 * Code keeps a session under its working directory and resumes it there.
 */
export async function threadDir(
  threadId: string,
  env: NodeJS.ProcessEnv = process.env,
): Promise<string> {
  if (!/^[0-9A-Za-z_-]{1,64}$/.test(threadId))
    throw new Error(`not a thread id: ${threadId}`);
  const dir = join(stateDir(env), "threads", threadId);
  await mkdir(dir, { recursive: true, mode: 0o700 });
  return dir;
}

/** Writes what pairing keeps, readable by this user alone. */
export async function savePaired(
  paired: Paired,
  path = configPath(),
): Promise<void> {
  await mkdir(join(path, ".."), { recursive: true, mode: 0o700 });
  const tmp = `${path}.${process.pid}.tmp`;
  await writeFile(tmp, JSON.stringify(paired, null, 2) + "\n", { mode: 0o600 });
  await rename(tmp, path);
}

/** What pairing kept, or undefined when the helper isn't paired. */
export async function loadPaired(
  path = configPath(),
): Promise<Paired | undefined> {
  let raw: string;
  try {
    raw = await readFile(path, "utf8");
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw e;
  }
  const p = JSON.parse(raw) as Partial<Paired>;
  if (
    typeof p.foundry !== "string" ||
    typeof p.hostId !== "string" ||
    typeof p.name !== "string" ||
    typeof p.token !== "string" ||
    !p.token.startsWith("hxh_")
  ) {
    throw new Error(`${path} isn't a pairing this helper wrote: pair again`);
  }
  return { foundry: p.foundry, hostId: p.hostId, name: p.name, token: p.token };
}
