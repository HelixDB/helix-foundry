import { config } from "../apps/api/src/config.js";
import { localAiMemory } from "../apps/api/src/memory.js";
const checks: [string, string][] = [
  ["HelixDB", config.helix + "/readyz"],
  ["DuckDB executor", config.executor + "/health"],
  ["Local model", config.ollama + "/api/tags"],
];
let errors = 0;
for (const [name, url] of checks) {
  try {
    const r = await fetch(url, { signal: AbortSignal.timeout(5000) });
    if (!r.ok) throw new Error("HTTP " + r.status);
    console.log("✓ " + name + " available");
  } catch (e) {
    errors++;
    console.log("✗ " + name + ": " + (e as Error).message);
  }
}
if (config.encryptionKey.length < 32 || config.internalKey.length < 32) {
  errors++;
  console.log("✗ Secrets missing: run scripts/setup.sh");
}
// A warning, not a failure: hosted AI works with any memory size.
const memory = localAiMemory(config.ollama);
if (memory) {
  const gb = (n: number) => (n / 1e9).toFixed(1) + " GB";
  console.log(
    (memory.sufficient ? "✓ " : "! ") +
      `Docker memory ${gb(memory.totalBytes)}` +
      (memory.sufficient
        ? ""
        : `: local AI and the database need at least ${gb(memory.minimumBytes)}. Give Docker more memory, or use Claude or OpenAI.`),
  );
}
process.exitCode = errors ? 1 : 0;
