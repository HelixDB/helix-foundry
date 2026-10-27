// A TypeScript port of the backend's filesource.Layout: how a folder's
// object keys become tables. It is a pure
// function of the keys, so the mock's S3 kind lists a fixture folder through
// it the way the WFE lists a bucket.

export type FileFormat = "parquet" | "json" | "csv";
/** One listed object, its key relative to the connector's folder. */
export type Listed = { key: string; size: number; archived?: boolean };
export type Dataset = { name: string; format: FileFormat; files: Listed[] };
/** A key or table Helix can't read, and why. */
export type Skipped = { name: string; reason: string };

const COMPRESSION = /\.(gz|gzip|zst|zstd|bz2)$/i;
const FORMATS: Record<string, FileFormat> = {
  parquet: "parquet",
  pq: "parquet",
  json: "json",
  jsonl: "json",
  ndjson: "json",
  csv: "csv",
  tsv: "csv",
};
const CODEC_INFIX = /\.(snappy|zstd|gz|lz4|brotli)$/i;
const UUID = /[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}/g;
const NOISE = new Set([
  "part",
  "c",
  "data",
  "chunk",
  "file",
  "run",
  "batch",
  "shard",
  "page",
]);
const HIVE = /^[^=]+=.*$/;
const DATE_ONLY = /^[0-9][0-9_.-]*$/;
const MAX_NAME_BYTES = 255;

// The folder a table format's metadata marks as one lake table, if any.
function lakeOf(key: string, all: ReadonlySet<string>) {
  const segments = key.split("/");
  const delta = segments.indexOf("_delta_log");
  if (delta >= 0)
    return {
      dir: segments.slice(0, delta).join("/"),
      reason: "Delta Lake tables are not supported yet",
    };
  const hudi = segments.indexOf(".hoodie");
  if (hudi >= 0)
    return {
      dir: segments.slice(0, hudi).join("/"),
      reason: "Hudi tables are not supported yet",
    };
  const meta = segments.lastIndexOf("metadata");
  if (
    meta >= 0 &&
    meta === segments.length - 2 &&
    /\.metadata\.json$/.test(key)
  )
    if (
      [...all].some((k) =>
        k.startsWith([...segments.slice(0, meta), "data", ""].join("/")),
      )
    )
      return {
        dir: segments.slice(0, meta).join("/"),
        reason: "Iceberg tables are not supported yet",
      };
  return undefined;
}

/** The base name without format, compression and codec extensions. */
function stemOf(base: string) {
  let name = base;
  while (COMPRESSION.test(name)) name = name.replace(COMPRESSION, "");
  return name.replace(/\.[^.]*$/, "").replace(CODEC_INFIX, "");
}

/** A file name's family: what is left once ids, dates and numbers go. */
export function family(stem: string) {
  return stem
    .toLowerCase()
    .replace(UUID, "")
    .replace(/\d+/g, "")
    .split(/[-_.\s]+/)
    .filter((t) => t && !NOISE.has(t))
    .join("_");
}

/** A key's format by extension, compression peeled first. */
export function formatOf(key: string): FileFormat | undefined {
  let name = key.split("/").at(-1)!;
  while (COMPRESSION.test(name)) name = name.replace(COMPRESSION, "");
  const ext = /\.([^.]+)$/.exec(name)?.[1]?.toLowerCase();
  return ext ? FORMATS[ext] : undefined;
}

/**
 * The tables in a folder's keys, by name with their files by key, and the
 * keys or tables Helix skips with why. `fallback` names files that sit at
 * the folder's root with no name of their own (the folder's last segment,
 * or the bucket).
 */
export function layout(
  fallback: string,
  objs: Listed[],
): { datasets: Dataset[]; skipped: Skipped[] } {
  const skipped: Skipped[] = [];
  const keys = new Set(objs.map((o) => o.key));
  const lakes = new Map<string, string>();
  for (const o of objs) {
    const lake = lakeOf(o.key, keys);
    if (lake && !lakes.has(lake.dir)) lakes.set(lake.dir, lake.reason);
  }
  for (const [dir, reason] of lakes)
    skipped.push({ name: dir || fallback, reason });
  // A lake at the folder's root holds every key.
  const inLake = (key: string) =>
    [...lakes.keys()].some((dir) => !dir || key.startsWith(dir + "/"));

  type Placed = {
    file: Listed;
    dir: string;
    family: string;
    format: FileFormat;
  };
  const placed: Placed[] = [];
  for (const o of objs) {
    if (inLake(o.key)) continue;
    const segments = o.key.split("/");
    if (
      o.key.endsWith("/") ||
      o.size === 0 ||
      /\.(crc|tmp)$/i.test(o.key) ||
      segments.some((s) => /^[_.]/.test(s) && !HIVE.test(s))
    )
      continue;
    const format = formatOf(o.key);
    if (!format) {
      skipped.push({ name: o.key, reason: "unsupported file type" });
      continue;
    }
    if (o.archived) {
      skipped.push({
        name: o.key,
        reason: "archived in Glacier; restore it first",
      });
      continue;
    }
    placed.push({
      file: o,
      dir: segments
        .slice(0, -1)
        .filter((s) => !HIVE.test(s) && !DATE_ONLY.test(s))
        .join("/"),
      family: family(stemOf(segments.at(-1)!)),
      format,
    });
  }

  const families = new Map<string, Set<string>>();
  for (const p of placed)
    families.set(p.dir, (families.get(p.dir) || new Set()).add(p.family));
  const tables = new Map<string, Placed[]>();
  for (const p of placed) {
    const several = families.get(p.dir)!.size > 1;
    const name = !p.dir
      ? p.family || fallback
      : several && p.family
        ? `${p.dir}/${p.family}`
        : p.dir;
    tables.set(name, [...(tables.get(name) || []), p]);
  }

  const datasets: Dataset[] = [];
  for (const [name, files] of tables) {
    const formats = [...new Set(files.map((f) => f.format))].sort();
    if (formats.length > 1)
      skipped.push({ name, reason: `mixed formats (${formats.join(", ")})` });
    else if (new TextEncoder().encode(name).length > MAX_NAME_BYTES)
      skipped.push({ name, reason: "name too long" });
    else
      datasets.push({
        name,
        format: formats[0]!,
        files: files
          .map((f) => f.file)
          .sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0)),
      });
  }
  datasets.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return { datasets, skipped };
}
