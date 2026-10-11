import { randomUUID } from "node:crypto";
import { Readable } from "node:stream";
import { createReadStream, type ReadStream } from "node:fs";
import { readdir, rm, stat } from "node:fs/promises";
import { resolve } from "node:path";
import { createInterface } from "node:readline";
import type { Execution } from "../../../services/executor/src/engine.js";
import { resultFile } from "../../../services/executor/src/paths.js";
import { config } from "./config.js";
import { execute } from "./executor.js";
// Runs a query once in the executor, which writes every result row to the
// shared data volume, then reads the rows back in batches and removes the
// file. Rows are the same JSON values a query returns. DuckDB stays in the
// executor; this only reads lines. A failure names what was being read.
export async function* resultBatches(
  job: Pick<Execution, "scope" | "inputs" | "sql">,
  what: string,
  size = 500,
) {
  const version = randomUUID(),
    file = resultFile(config.dataDir, job.scope, version);
  let input: ReadStream | undefined;
  try {
    const { count } = await execute({ ...job, mode: "materialize", version });
    input = createReadStream(file);
    let read = 0,
      batch: Record<string, any>[] = [];
    for await (const line of createInterface({ input, crlfDelay: Infinity })) {
      batch.push(JSON.parse(line));
      read++;
      if (batch.length === size) {
        yield batch;
        batch = [];
      }
    }
    if (batch.length) yield batch;
    if (read !== count) throw new Error(`Read ${read} of ${count} result rows`);
  } catch (e) {
    if (e instanceof Error) e.message = `Could not read ${what}. ${e.message}`;
    throw e;
  } finally {
    input?.destroy();
    // Never hides the error above; the sweep removes a file left behind.
    await rm(file, { force: true }).catch(() => {});
  }
}
// A reader opens its file as soon as the executor has written it, and removing
// an open file does not disturb the reader, so older files are orphans of a
// stopped worker or a lost executor reply. One that cannot be removed is left
// for the next sweep.
export async function sweepResults(maxAge = 10 * 60000) {
  const root = resolve(config.dataDir, "results");
  for (const scope of await readdir(root).catch(() => []))
    for (const name of await readdir(resolve(root, scope)).catch(() => [])) {
      const file = resolve(root, scope, name),
        info = await stat(file).catch(() => undefined);
      if (info && Date.now() - info.mtimeMs > maxAge)
        await rm(file, { force: true }).catch(() => {});
    }
}

// CSV is a download of every row, using the same immutable snapshot and JSON
// conversions as queries. Quote every non-null value so empty strings remain
// distinct from nulls; nested values are JSON inside the CSV field.
const csvValue = (value: unknown) => {
  if (value === null || value === undefined) return "";
  const text =
    typeof value === "object" ? JSON.stringify(value) : String(value);
  return '"' + text.replaceAll('"', '""') + '"';
};
export async function datasetCsv(
  job: Pick<Execution, "scope" | "inputs" | "sql">,
  columns: string[],
) {
  const batches = resultBatches(job, "CSV export");
  // Start execution before sending download headers, so a missing snapshot or
  // an executor limit returns the normal API error instead of a broken 200.
  const first = await batches.next();
  const stream = Readable.from(
    (async function* () {
      try {
        yield columns.map(csvValue).join(",") + "\r\n";
        let next = first;
        while (!next.done) {
          yield next.value
            .map(
              (row) => columns.map((c) => csvValue(row[c])).join(",") + "\r\n",
            )
            .join("");
          next = await batches.next();
        }
      } finally {
        await batches.return(undefined);
      }
    })(),
  );
  // A caller may close the download before the first read. The batch reader
  // has already opened its file; close it even when the generator never starts.
  stream.once("close", () => {
    void batches.return(undefined).catch(() => {});
  });
  return stream;
}
