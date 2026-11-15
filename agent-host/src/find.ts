// Finding the person's own `claude`: the helper drives that executable, so
// its login is theirs.

import { access, constants } from "node:fs/promises";
import { homedir } from "node:os";
import { delimiter, join } from "node:path";

/** Where Claude Code's installers put `claude`, after PATH. */
function knownPlaces(home: string): string[] {
  return [
    join(home, ".claude", "local", "claude"),
    join(home, ".local", "bin", "claude"),
    "/opt/homebrew/bin/claude",
    "/usr/local/bin/claude",
  ];
}

async function executable(path: string): Promise<boolean> {
  try {
    await access(path, constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

/**
 * The `claude` to run: the one named (--claude, else FOUNDRY_CLAUDE_PATH),
 * else the first on PATH, else where its installers put it; undefined when
 * there is none.
 */
export async function findClaude(
  named: string | undefined,
  env: NodeJS.ProcessEnv = process.env,
): Promise<string | undefined> {
  const chosen = named || env.FOUNDRY_CLAUDE_PATH;
  if (chosen) return (await executable(chosen)) ? chosen : undefined;
  const onPath = (env.PATH ?? "")
    .split(delimiter)
    .filter(Boolean)
    .map((dir) => join(dir, "claude"));
  for (const candidate of [...onPath, ...knownPlaces(env.HOME || homedir())]) {
    if (await executable(candidate)) return candidate;
  }
  return undefined;
}
