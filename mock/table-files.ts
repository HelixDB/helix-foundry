import { parquetMetadata, parquetReadObjects } from "hyparquet";
import { compressors } from "hyparquet-compressors";
import Papa from "papaparse";
import type { DocumentMediaType } from "../src/shared/wfe";

// The mock's stand-in for the WFE's document worker reading a table file
// once, when its upload completes (the backend's document/read): CSV and TSV split by their header,
// JSON Lines one object a line, JSON's first array of records (depth
// first, as the engine catalogs arrays by pointer), Parquet by hyparquet.
// What it reads is the file's rows, which a generation drafts from and a
// load ingests; a file that isn't what its type says fails as
// `unsupported`, with what to fix. Values are JSON's: a Parquet integer past
// 2^53 is its digits, a date its ISO text.

export type Row = Record<string, unknown>;
export type TableRead =
  | { ok: true; rows: Row[] }
  | { ok: false; code: "too_large" | "unsupported"; message: string };

const MAX_DEPTH = 64;
const unsupported = (message: string): TableRead => ({
  ok: false,
  code: "unsupported",
  message,
});
const isRecord = (v: unknown): v is Row =>
  !!v && typeof v === "object" && !Array.isArray(v);

/** JSON's value as rows hold it: bigints and dates made JSON. */
function plain(v: unknown): unknown {
  if (typeof v === "bigint")
    return Number.isSafeInteger(Number(v)) ? Number(v) : v.toString();
  if (v instanceof Date) return v.toISOString();
  if (Array.isArray(v)) return v.map(plain);
  if (isRecord(v))
    return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, plain(x)]));
  return v;
}

/** The first array of records in a JSON value, depth first; none past MAX_DEPTH. */
function records(v: unknown, depth = 0): Row[] | undefined {
  if (depth > MAX_DEPTH) return undefined;
  if (Array.isArray(v)) {
    if (v.length && v.every(isRecord)) return v;
    for (const x of v) {
      const found = records(x, depth + 1);
      if (found) return found;
    }
    return undefined;
  }
  if (isRecord(v))
    for (const x of Object.values(v)) {
      const found = records(x, depth + 1);
      if (found) return found;
    }
  return undefined;
}

/** A table file's rows, or why it can't be read. */
export async function readTableFile(
  mediaType: DocumentMediaType,
  bytes: Buffer,
): Promise<TableRead> {
  switch (mediaType) {
    case "text/csv":
    case "text/tab-separated-values": {
      const name = mediaType === "text/csv" ? "CSV" : "TSV";
      const parsed = Papa.parse<string[]>(bytes.toString("utf8"), {
        delimiter: mediaType === "text/csv" ? "," : "\t",
        skipEmptyLines: "greedy",
      });
      const [header, ...body] = parsed.data;
      if (!header?.length || parsed.errors.some((e) => e.type === "Quotes"))
        return unsupported(`It isn’t ${name}: no header row could be read.`);
      const ragged = body.findIndex((r) => r.length !== header.length);
      if (ragged >= 0)
        return unsupported(
          `Row ${ragged + 2} has ${body[ragged]!.length} fields and the header ${header.length}. Fix the row and upload the file again.`,
        );
      return {
        ok: true,
        rows: body.map((r) =>
          Object.fromEntries(header.map((h, i) => [h.trim(), r[i]])),
        ),
      };
    }
    case "application/x-ndjson": {
      const rows: Row[] = [];
      const lines = bytes.toString("utf8").split(/\r?\n/);
      for (const [i, line] of lines.entries()) {
        if (!line.trim()) continue;
        let v: unknown;
        try {
          v = JSON.parse(line);
        } catch {
          return unsupported(
            `Line ${i + 1} isn’t JSON. Fix it and upload the file again.`,
          );
        }
        if (!isRecord(v))
          return unsupported(
            `Line ${i + 1} isn’t a JSON object: each line of a JSONL file is one record.`,
          );
        rows.push(v);
      }
      return { ok: true, rows };
    }
    case "application/json": {
      let v: unknown;
      try {
        v = JSON.parse(bytes.toString("utf8"));
      } catch {
        return unsupported("It isn’t JSON. Fix it and upload the file again.");
      }
      const rows = records(v);
      return rows
        ? { ok: true, rows }
        : unsupported(
            "It holds no array of records (objects), which is what loads read from JSON.",
          );
    }
    case "application/vnd.apache.parquet": {
      try {
        const file = bytes.buffer.slice(
          bytes.byteOffset,
          bytes.byteOffset + bytes.byteLength,
        ) as ArrayBuffer;
        const rows = await parquetReadObjects({
          file,
          metadata: parquetMetadata(file),
          compressors,
        });
        return { ok: true, rows: rows.map((r) => plain(r) as Row) };
      } catch {
        return unsupported(
          "It isn’t Parquet that Foundry can read. Write it again and upload it.",
        );
      }
    }
    case "text/markdown":
    case "text/mdx":
    case "text/plain":
    case "application/pdf":
      return unsupported("It isn’t a table file.");
  }
}
