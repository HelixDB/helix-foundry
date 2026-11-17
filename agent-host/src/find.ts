// Finding the person's own `claude` and `codex`: the helper drives those
// executables, so their logins are theirs.

import { access, constants } from "node:fs/promises";
import { homedir } from "node:os";
import { delimiter, join } from "node:path";

/** A CLI the helper drives: its executable's name, and the variable that names one. */
export const CLIS = {
  claude: { env: "FOUNDRY_CLAUDE_PATH" },
  codex: { env: "FOUNDRY_CODEX_PATH" },
} as const;
export type CliName = keyof typeof CLIS;

/** Where the CLIs' installers put them, after PATH. */
function knownPlaces(name: CliName, home: string): string[] {
  return [
    ...(name === "claude" ? [join(home, ".claude", "local", "claude")] : []),
    join(home, ".local", "bin", name),
    join(home, ".npm-global", "bin", name),
    `/opt/homebrew/bin/${name}`,
    `/usr/local/bin/${name}`,
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
 * The CLI to run: the one named (--claude or --codex, else
 * FOUNDRY_CLAUDE_PATH or FOUNDRY_CODEX_PATH), else the first on PATH, else
 * where its installers put it; undefined when there is none.
 */
export async function findCli(
  name: CliName,
  named: string | undefined,
  env: NodeJS.ProcessEnv = process.env,
): Promise<string | undefined> {
  const chosen = named || env[CLIS[name].env];
  if (chosen) return (await executable(chosen)) ? chosen : undefined;
  const onPath = (env.PATH ?? "")
    .split(delimiter)
    .filter(Boolean)
    .map((dir) => join(dir, name));
  for (const candidate of [
    ...onPath,
    ...knownPlaces(name, env.HOME || homedir()),
  ]) {
    if (await executable(candidate)) return candidate;
  }
  return undefined;
}
