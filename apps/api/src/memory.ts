import { readFileSync } from "node:fs";
import { totalmem } from "node:os";

// HelixDB, the bundled model and the app share Docker's memory. Below this,
// local AI and a large workspace do not fit together (docs/OPERATIONS.md#memory).
// Docker Desktop reports a little over 8e9 bytes for its "8 GB" setting.
export const localAiMinimumMemory = 8_000_000_000;

export type LocalAiMemory = {
  totalBytes: number;
  minimumBytes: number;
  sufficient: boolean;
};

// Only the Compose `ollama` service shares memory with HelixDB. Native Ollama
// (MODEL_ENDPOINT) and development setups run outside Docker's memory.
export function localAiMemory(
  modelUrl: string,
  total = totalmem(),
): LocalAiMemory | null {
  let host: string;
  try {
    host = new URL(modelUrl).hostname;
  } catch {
    return null;
  }
  if (host !== "ollama") return null;
  // In a container this is the Docker VM's memory, not the container limit.
  return {
    totalBytes: total,
    minimumBytes: localAiMinimumMemory,
    sufficient: total >= localAiMinimumMemory,
  };
}

// The kernel's count of processes killed for running out of memory. /proc/vmstat
// is not namespaced, so inside a container it counts the whole Docker VM,
// including HelixDB. Null where it cannot be read (macOS, Windows).
export function oomKills(
  read: () => string = () => readFileSync("/proc/vmstat", "utf8"),
): number | null {
  try {
    const m = /^oom_kill (\d+)$/m.exec(read());
    return m ? Number(m[1]) : null;
  } catch {
    return null;
  }
}

// Tells whether the kernel killed something for memory in the last few minutes,
// so "cannot reach the database" can say why. HelixDB takes a while to come
// back, and every failing request in that time should get the same answer.
export function oomWatcher(
  read?: () => string,
  windowMs = 180_000,
  clock = Date.now,
) {
  let seen = oomKills(read),
    killedAt = -Infinity;
  return () => {
    const current = oomKills(read);
    if (current !== null && seen !== null && current > seen) killedAt = clock();
    if (current !== null) seen = current;
    return clock() - killedAt < windowMs;
  };
}
