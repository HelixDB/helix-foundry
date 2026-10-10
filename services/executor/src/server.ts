import "dotenv/config";
import Fastify from "fastify";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { randomUUID, timingSafeEqual } from "node:crypto";
import { readdir, rm, stat } from "node:fs/promises";
import { resolve, join } from "node:path";
const dataDir = process.env.DATA_DIR || ".data";
const tempLimit = process.env.DUCKDB_TEMP_LIMIT || "2GB";
const units: Record<string, number> = {
  "": 1,
  b: 1,
  kb: 1e3,
  mb: 1e6,
  gb: 1e9,
  tb: 1e12,
  kib: 2 ** 10,
  mib: 2 ** 20,
  gib: 2 ** 30,
  tib: 2 ** 40,
};
const size = /^\s*(\d+(?:\.\d+)?)\s*([a-z]*)\s*$/i.exec(tempLimit);
const tempLimitBytes = size
  ? Number(size[1]) * (units[size[2].toLowerCase()] ?? NaN)
  : NaN;
if (!Number.isFinite(tempLimitBytes))
  throw new Error(`Invalid DUCKDB_TEMP_LIMIT: ${tempLimit}`);
// DuckDB's own max_temp_directory_size does not stop every kind of spill,
// so the server measures each job's directory and kills it when it is too big.
const directorySize = async (dir: string) => {
  const files = await readdir(dir).catch(() => []);
  const sizes = await Promise.all(
    files.map((f) =>
      stat(join(dir, f)).then(
        (s) => s.size,
        () => 0,
      ),
    ),
  );
  return sizes.reduce((a, b) => a + b, 0);
};
// Only this process starts jobs, so anything here was left by a killed job.
await rm(resolve(dataDir, "tmp"), { recursive: true, force: true });
const app = Fastify({ bodyLimit: 1024 * 1024, logger: true });
let active = 0;
const waiters: (() => void)[] = [];
app.get("/health", async () => ({ ok: true }));
app.post("/execute", async (req, reply) => {
  const key = String(req.headers["x-internal-key"] || ""),
    expected = process.env.INTERNAL_KEY || "";
  if (
    expected.length < 32 ||
    Buffer.byteLength(key) !== Buffer.byteLength(expected) ||
    !timingSafeEqual(Buffer.from(key), Buffer.from(expected))
  )
    return reply.code(401).send({ error: "Unauthorized" });
  if (active >= 2) {
    if (waiters.length >= 50)
      return reply.code(429).send({ error: "Executor queue is full" });
    await new Promise<void>((resolve) => waiters.push(resolve));
  }
  active++;
  const spill = resolve(dataDir, "tmp", randomUUID());
  try {
    return await new Promise((resolve) => {
      const child = spawn(
        process.execPath,
        [
          "--import",
          "tsx",
          fileURLToPath(new URL("./runner.ts", import.meta.url)),
        ],
        {
          env: {
            PATH: process.env.PATH,
            DATA_DIR: dataDir,
            DUCKDB_MEMORY: process.env.DUCKDB_MEMORY || "512MB",
            DUCKDB_TEMP_DIR: spill,
            DUCKDB_TEMP_LIMIT: tempLimit,
          },
          stdio: ["pipe", "pipe", "pipe"],
        },
      );
      let out = "",
        err = "",
        overDisk = false;
      const timer = setTimeout(() => child.kill("SIGKILL"), 120000);
      const diskWatch = setInterval(async () => {
        if ((await directorySize(spill)) <= tempLimitBytes) return;
        overDisk = true;
        child.kill("SIGKILL");
      }, 1000);
      child.stdout.on("data", (x) => {
        out += x;
        if (out.length > 10_000_000) child.kill("SIGKILL");
      });
      child.stderr.on("data", (x) => (err += x));
      child.on("error", (e) => {
        clearTimeout(timer);
        clearInterval(diskWatch);
        resolve(reply.code(500).send({ error: e.message }));
      });
      child.on("close", (code) => {
        clearTimeout(timer);
        clearInterval(diskWatch);
        if (code !== 0)
          return resolve(
            reply.code(422).send({
              error: overDisk
                ? `Query exceeded the temporary disk limit (DUCKDB_TEMP_LIMIT=${tempLimit})`
                : err.slice(0, 1500) || "Query exceeded execution limits",
            }),
          );
        try {
          resolve(JSON.parse(out));
        } catch {
          resolve(reply.code(500).send({ error: "Invalid executor response" }));
        }
      });
      child.stdin.end(JSON.stringify(req.body));
    });
  } finally {
    await rm(spill, { recursive: true, force: true });
    active--;
    waiters.shift()?.();
  }
});
await app.listen({
  port: Number(process.env.EXECUTOR_PORT || 3002),
  host: process.env.EXECUTOR_HOST || "127.0.0.1",
});
