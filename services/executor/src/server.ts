import "dotenv/config";
import Fastify from "fastify";
import { resolve } from "node:path";
import { timingSafeEqual } from "node:crypto";
import { staleScratch, stopJobs, submit } from "./jobs.js";
import { checkTimeLimits } from "./limits.js";
// An invalid EXECUTOR_QUERY_TIMEOUT or EXECUTOR_JOB_TIMEOUT stops the executor
// here, with a message that names it, rather than failing every job later.
checkTimeLimits();
const root = resolve(process.env.DATA_DIR || ".data");
const app = Fastify({ bodyLimit: 1024 * 1024, logger: true });
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
  const r = await submit(req.body, root);
  return reply.code(r.status).send(r.body);
});
// Compose stops a container with SIGTERM and kills it 10 seconds later, which
// would leave running jobs' spill files on the data volume, and in a backup
// taken meanwhile. Ending the jobs first removes them.
for (const signal of ["SIGTERM", "SIGINT"] as const)
  process.once(signal, () => void stopJobs().then(() => process.exit(0)));
const sweep = await staleScratch(root);
await app.listen({
  port: Number(process.env.EXECUTOR_PORT || 3002),
  host: process.env.EXECUTOR_HOST || "127.0.0.1",
});
await sweep();
