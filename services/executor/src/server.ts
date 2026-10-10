import "dotenv/config";
import Fastify from "fastify";
import { resolve } from "node:path";
import { timingSafeEqual } from "node:crypto";
import { runJob, sweepScratch } from "./jobs.js";
const root = resolve(process.env.DATA_DIR || ".data");
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
  try {
    const r = await runJob(JSON.stringify(req.body), root);
    if (r.failed) return reply.code(500).send({ error: r.failed });
    if (r.code !== 0)
      return reply.code(422).send({
        error: r.err.slice(0, 1500) || "Query exceeded execution limits",
      });
    try {
      return JSON.parse(r.out);
    } catch {
      return reply.code(500).send({ error: "Invalid executor response" });
    }
  } finally {
    active--;
    waiters.shift()?.();
  }
});
await sweepScratch(root);
await app.listen({
  port: Number(process.env.EXECUTOR_PORT || 3002),
  host: process.env.EXECUTOR_HOST || "127.0.0.1",
});
