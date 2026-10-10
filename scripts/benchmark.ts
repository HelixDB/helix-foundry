import { performance } from "node:perf_hooks";
import { mkdir, rm, writeFile } from "node:fs/promises";
import { createReadStream, createWriteStream } from "node:fs";
import { once } from "node:events";
import { resolve } from "node:path";
import { createInterface } from "node:readline";
import { randomUUID } from "node:crypto";
import { execute as localExecute } from "../services/executor/src/engine.js";
import { resultFile } from "../services/executor/src/paths.js";
import { execute as remoteExecute } from "../apps/api/src/executor.js";
const execute = process.env.BENCH_HTTP === "1" ? remoteExecute : localExecute;
import { config } from "../apps/api/src/config.js";
const scope = "bench-" + randomUUID();
await mkdir(resolve(config.dataDir, "incoming", scope), { recursive: true });
const out = createWriteStream(
  resolve(config.dataDir, "incoming", scope, "million.jsonl"),
);
for (let i = 0; i < 1_000_000; i++)
  if (
    !out.write(
      JSON.stringify({
        id: i,
        region: ["NA", "EU", "APAC", "LATAM"][i % 4],
        amount: i % 1000,
        status: i % 5 ? "complete" : "pending",
      }) + "\n",
    )
  )
    await once(out, "drain");
out.end();
await once(out, "finish");
const start = performance.now();
const ingested = await execute({
  mode: "ingest",
  scope,
  version: "million",
  file: "million.jsonl",
});
const imported = performance.now();
const timings: number[] = [];
await Promise.all(
  Array.from({ length: 25 }, async (_, i) => {
    const s = performance.now();
    await execute({
      mode: "query",
      scope,
      version: "q" + i,
      inputs: [{ table: "orders", version: "million" }],
      sql: "SELECT region, sum(amount) revenue, count(*) orders FROM orders GROUP BY region",
    });
    timings.push(performance.now() - s);
  }),
);
// Publishing a type with a million records: one materialize query read back
// as JSON lines, against one profiled query per 500 rows before.
const orders = {
  scope,
  inputs: [{ table: "orders", version: "million" }],
  sql: "SELECT * FROM orders",
};
let s = performance.now();
await execute({ ...orders, mode: "query", version: "page", limit: 500 });
const pageMs = performance.now() - s;
s = performance.now();
await execute({ ...orders, mode: "materialize", version: "publication" });
const file = resultFile(config.dataDir, scope, "publication");
let read = 0;
for await (const line of createInterface({ input: createReadStream(file) }))
  if (JSON.parse(line)) read++;
await rm(file);
const publicationMs = performance.now() - s;
const result = {
  rows: ingested.profile.rows,
  importMs: imported - start,
  queries: 25,
  mode:
    process.env.BENCH_HTTP === "1"
      ? "25 concurrent HTTP executor clients, 2 execution slots"
      : "25 concurrent in-process analytical queries",
  medianQueryMs: timings.sort((a, b) => a - b)[12],
  maxQueryMs: Math.max(...timings),
  publication: {
    rows: read,
    materializeAndReadMs: publicationMs,
    pagedQueryMs: pageMs,
    pagedEstimateMs: pageMs * Math.ceil(read / 500),
  },
  inferenceIncluded: false,
  rssScope:
    "benchmark client process; excludes executor children and other services",
  rssMB: Math.round(process.memoryUsage().rss / 1024 / 1024),
  measuredAt: new Date().toISOString(),
};
await writeFile(
  resolve(config.dataDir, "benchmark.json"),
  JSON.stringify(result, null, 2),
);
console.log(result);
