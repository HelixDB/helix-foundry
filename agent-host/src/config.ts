// The helper's state on this computer: the Foundry it is paired with and its
// token, in a file only this user reads, and an empty directory per thread
// that its CLI runs in (threads/<workspace>/<thread>), under which the CLI
// keeps the thread's session (Claude Code in ~/.claude/projects/..., Codex
// in its CODEX_HOME's sessions, by working directory). A thread's session
// holds what Foundry's tools answered in it, so the helper deletes it once
// the thread is gone (forgetThread).

import {
  mkdir,
  readdir,
  readFile,
  rename,
  rm,
  writeFile,
} from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";

import type { ThreadRef } from "./foundry.ts";

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

/** An id Foundry gives: what a directory may be named. */
const ID = /^[0-9A-Za-z_-]{1,64}$/;

/** Where a thread's working directory is, made or not. */
export function threadPath(
  t: ThreadRef,
  env: NodeJS.ProcessEnv = process.env,
): string {
  if (!ID.test(t.workspaceId) || !ID.test(t.threadId))
    throw new Error(`not a thread: ${t.workspaceId}/${t.threadId}`);
  return join(stateDir(env), "threads", t.workspaceId, t.threadId);
}

/**
 * The thread's working directory: empty (its CLI runs with no tool that
 * writes, or one that asks and is declined), and the same for every turn of
 * the thread, since each CLI finds its sessions by working directory.
 */
export async function threadDir(
  t: ThreadRef,
  env: NodeJS.ProcessEnv = process.env,
): Promise<string> {
  const dir = threadPath(t, env);
  await mkdir(dir, { recursive: true, mode: 0o700 });
  return dir;
}

/** The threads this helper keeps a directory (and so maybe a session) of. */
export async function listThreads(
  env: NodeJS.ProcessEnv = process.env,
): Promise<ThreadRef[]> {
  const root = join(stateDir(env), "threads");
  const names = async (dir: string) => {
    try {
      return (await readdir(dir, { withFileTypes: true }))
        .filter((e) => e.isDirectory() && ID.test(e.name))
        .map((e) => e.name)
        .sort();
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code === "ENOENT") return [];
      throw e;
    }
  };
  const out: ThreadRef[] = [];
  for (const workspaceId of await names(root))
    for (const threadId of await names(join(root, workspaceId)))
      out.push({ workspaceId, threadId });
  return out;
}

/** A CLI's sessions kept under a working directory: the Agent SDK's, Codex's app-server's, or a test's. */
export type Sessions = {
  list(dir: string): Promise<string[]>;
  delete(id: string, dir: string): Promise<void>;
};

/**
 * Deletes what the helper keeps of a thread: each CLI's sessions under its
 * directory (each with what Foundry's tools answered), then the directory.
 * Every CLI is tried, one failing or not; the directory goes only once each
 * deleted its sessions, so a later sweep finds the thread and tries again,
 * and the first failure is thrown after.
 */
export async function forgetThread(
  t: ThreadRef,
  sessions: Sessions[],
  env: NodeJS.ProcessEnv = process.env,
): Promise<void> {
  const dir = threadPath(t, env);
  const failed: unknown[] = [];
  for (const s of sessions) {
    try {
      for (const id of await s.list(dir)) await s.delete(id, dir);
    } catch (e) {
      failed.push(e);
    }
  }
  if (failed.length) throw failed[0];
  await rm(dir, { recursive: true, force: true });
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
