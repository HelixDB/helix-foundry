import { randomUUID } from "node:crypto";
import type { Resource } from "../../../packages/shared/src/index.js";
import { currentBundle } from "./assistant.js";
import { execute } from "./executor.js";
import { digest } from "./security.js";
import type { Store } from "./store.js";

// The record list of the active workspace. Records are the rows of each
// type's dataset snapshot that publication read, so the executor filters,
// sorts and pages them with DuckDB, and only the page is read from HelixDB.
// Records edited in the workspace, and records added by hand, are listed
// first: their values differ from the snapshot, and there are few of them.
export type RecordQuery = {
  type?: string;
  search?: string;
  field?: string;
  value?: string;
  sort?: string;
  dir?: "asc" | "desc";
  offset: number;
  limit: number;
};
const q = (s: string) => '"' + s.replaceAll('"', '""') + '"';
const lit = (s: string) => "'" + s.replaceAll("'", "''") + "'";
const blanks = ["", "None", "null", "NaN"];
const blank = (v: unknown) => v == null || blanks.includes(String(v).trim());
// A readable name for a record: a person's name, a name or title, an email;
// otherwise its type and a shortened key.
export function recordLabel(name: string, type: string, values: object) {
  const entries = Object.entries(values).filter(([, v]) => !blank(v));
  const find = (test: RegExp) => entries.find(([k]) => test.test(k))?.[1];
  const first = find(/(^|_)first_?name$/i),
    last = find(/(^|_)last_?name$/i);
  if (first || last) return [first, last].filter(Boolean).join(" ");
  const named =
    find(/^(name|full_?name|display_?name|title|subject|company(_?name)?)$/i) ??
    find(/(^|_)(name|title)$/i) ??
    // Only the record's own email: requester_email names someone else.
    find(/^e-?mail$/i);
  if (named) return String(named).slice(0, 80);
  const key = name.startsWith(type + " ") ? name.slice(type.length + 1) : name;
  return `${type} ${key.length > 14 ? key.slice(0, 12) + "…" : key}`;
}

const text = (c: string) =>
  `CASE WHEN trim(CAST(${c} AS VARCHAR)) IN (${blanks.map(lit).join(",")}) THEN NULL ELSE CAST(${c} AS VARCHAR) END`;
// recordLabel in SQL: first and last name, a name-like column or an email,
// else the type and a shortened key.
function labelSql(columns: string[], type: string, key: string) {
  const col = (c: string) => text("t." + q(c)),
    match = (re: RegExp) => columns.filter((c) => re.test(c));
  const first = match(/(^|_)first_?name$/i),
    last = match(/(^|_)last_?name$/i),
    named = [
      ...match(
        /^(name|full_?name|display_?name|title|subject|company(_?name)?)$/i,
      ),
      ...match(/(^|_)(name|title)$/i),
      ...match(/^e-?mail$/i),
    ];
  const k = `CAST(${key} AS VARCHAR)`;
  return `COALESCE(${[
    ...(first.length || last.length
      ? [
          `NULLIF(concat_ws(' ', ${[...first.slice(0, 1), ...last.slice(0, 1)].map(col).join(", ")}), '')`,
        ]
      : []),
    ...named.map((c) => `left(${col(c)}, 80)`),
    `${lit(type + " ")} || CASE WHEN length(${k}) > 14 THEN left(${k}, 12) || '…' ELSE ${k} END`,
  ].join(", ")})`;
}
const compareText = (a: string, b: string) =>
  a.localeCompare(b, undefined, { numeric: true });

export async function listRecords(
  store: Store,
  scope: string,
  query: RecordQuery,
): Promise<{ items: Resource[]; total: number }> {
  const generation = (await store.get(scope, "workspace"))?.data
    .activeGeneration;
  if (!generation) return { items: [], total: 0 };
  const all = (await store.list(scope, "objectType")).filter(
    (t) => t.data.generation === generation,
  );
  const types = query.type ? all.filter((t) => t.name === query.type) : all;
  const dir = query.dir === "desc" ? "DESC" : "ASC",
    sign = dir === "DESC" ? -1 : 1,
    field = query.sort && !["record", "links"].includes(query.sort);
  // Edited and hand-added records of these types, with their workspace values.
  const overlays = new Map(
    (await store.list(scope, "overlay")).map((o) => [
      generation + "_" + o.id.slice("overlay_".length),
      o.data.values || {},
    ]),
  );
  const ids = [
    ...overlays.keys(),
    ...(await store.list(scope, "manualObject")).map(
      (m) => generation + "_" + m.data.logicalId,
    ),
  ];
  const pinned = (await store.getMany(scope, [...new Set(ids)]))
    .filter(
      (o): o is Resource =>
        !!o &&
        o.kind === "object" &&
        o.data.generation === generation &&
        (!query.type || o.data.type === query.type),
    )
    .map((o): Resource => {
      const effective = { ...o.data.base, ...overlays.get(o.id) };
      return {
        ...o,
        data: {
          ...o.data,
          effective,
          edits: overlays.get(o.id) || {},
          label: recordLabel(o.name, o.data.type, effective),
        },
      };
    })
    .filter(
      (o) =>
        (!query.search ||
          JSON.stringify([o.name, o.description, o.data.effective])
            .toLowerCase()
            .includes(query.search.toLowerCase())) &&
        (!query.field ||
          query.value === undefined ||
          (Object.hasOwn(o.data.effective, query.field) &&
            String(o.data.effective[query.field]) === String(query.value))),
    );
  // As the snapshot's rows sort: by a field (numbers as numbers), by link
  // count, or by name, with empty values last.
  const pinnedLinks = await store.linkCounts(
    scope,
    pinned.map((o) => o.id),
  );
  const valueOf = (o: Resource) =>
    query.sort === "links"
      ? pinnedLinks.get(o.id) || 0
      : o.data.effective[query.sort!];
  const byValue = (x: Resource, y: Resource) => {
    const a = valueOf(x),
      b = valueOf(y);
    if (blank(a) || blank(b)) return Number(blank(a)) - Number(blank(b));
    const n = Number(a) - Number(b);
    return (
      sign *
      (Number.isFinite(n) && String(a).trim() !== "" && String(b).trim() !== ""
        ? n
        : compareText(String(a), String(b)))
    );
  };
  pinned.sort(
    (x, y) =>
      (field || query.sort === "links" ? byValue(x, y) : 0) ||
      (query.sort === "record" ? sign : 1) *
        compareText(x.data.label, y.data.label) ||
      x.id.localeCompare(y.id),
  );
  // One SELECT per type, over the snapshot publication read.
  const bundle = await currentBundle(store, scope),
    { inputs, tableOf } = await snapshotInputs(store, scope, all);
  const datasets = new Map(
    (
      await store.getMany(
        scope,
        all.map((t) => t.data.datasetId),
      )
    )
      .filter((d): d is Resource => !!d)
      .map((d) => [d.id, d]),
  );
  const columnsOf = (t: Resource): string[] =>
    (datasets.get(t.data.datasetId)?.data.profile?.columns || []).map(
      (c: any) => c.name,
    );
  const selects = types.flatMap((t) => {
    const table = tableOf.get(t.name),
      columns = columnsOf(t),
      key = "t." + q(t.data.primaryKey);
    if (!table || !columns.includes(t.data.primaryKey)) return [];
    if (
      query.field &&
      query.value !== undefined &&
      !columns.includes(query.field)
    )
      return [];
    const where = [
      ...(query.search
        ? [
            `strpos(lower(concat_ws(' ', ${lit(t.name + " ")} || CAST(${key} AS VARCHAR), ${columns.map((c) => `CAST(t.${q(c)} AS VARCHAR)`).join(", ")})), ${lit(query.search.toLowerCase())}) > 0`,
          ]
        : []),
      ...(query.field && query.value !== undefined
        ? [`CAST(t.${q(query.field)} AS VARCHAR) = ${lit(String(query.value))}`]
        : []),
      ...(pinned.some((o) => o.data.type === t.name)
        ? [
            `CAST(${key} AS VARCHAR) NOT IN (${pinned
              .filter((o) => o.data.type === t.name)
              .map((o) => lit(String(o.data.key)))
              .join(",")})`,
          ]
        : []),
    ];
    // Links per record, counted from the relationships' joins as publication
    // linked them, for sorting by links.
    const links: { alias: string; join: string }[] = [];
    if (query.sort === "links")
      bundle.relationships.forEach((rel, i) => {
        const from = all.find((x) => x.name === rel.fromType),
          to = all.find((x) => x.name === rel.toType),
          a = from && tableOf.get(from.name),
          b = to && tableOf.get(to.name);
        if (!from || !to || !a || !b) return;
        const join = `FROM ${q(a)} a JOIN ${q(b)} b ON a.${q(rel.fromColumn)} = b.${q(rel.toColumn)}`;
        if (from.name === t.name)
          links.push({
            alias: "o" + i,
            join: `LEFT JOIN (SELECT a.${q(from.data.primaryKey)} AS k, count(DISTINCT b.${q(to.data.primaryKey)}) AS n ${join} GROUP BY 1) o${i} ON o${i}.k = ${key}`,
          });
        if (to.name === t.name)
          links.push({
            alias: "i" + i,
            join: `LEFT JOIN (SELECT b.${q(to.data.primaryKey)} AS k, count(DISTINCT a.${q(from.data.primaryKey)}) AS n ${join} GROUP BY 1) i${i} ON i${i}.k = ${key}`,
          });
      });
    const linkSum = links.length
      ? links.map((l) => `COALESCE(${l.alias}.n, 0)`).join(" + ")
      : "0";
    const sortValue =
      field && columns.includes(query.sort!) ? `t.${q(query.sort!)}` : "NULL";
    return [
      `SELECT ${lit(t.name)} AS __type, ${types.length > 1 ? `CAST(${key} AS VARCHAR)` : key} AS __key, ${labelSql(columns, t.name, key)} AS __label, TRY_CAST(${sortValue} AS DOUBLE) AS __num, ${text(sortValue)} AS __text, ${linkSum} AS __links FROM ${q(table)} t ${links.map((l) => l.join).join(" ")}${where.length ? " WHERE " + where.join(" AND ") : ""}`,
    ];
  });
  const offset = Math.max(0, query.offset),
    fromPinned = pinned.slice(offset, offset + query.limit),
    sqlOffset = Math.max(0, offset - pinned.length),
    sqlLimit = query.limit - fromPinned.length;
  let rows: Record<string, any>[] = [],
    counted = 0;
  if (selects.length) {
    const order = [
      ...(field
        ? [`__text IS NULL`, `__num ${dir} NULLS LAST`, `lower(__text) ${dir}`]
        : []),
      ...(query.sort === "links" ? [`__links ${dir}`] : []),
      `lower(__label) ${query.sort === "record" ? dir : "ASC"}`,
      "__type",
      "__key",
    ].join(", ");
    const result = await execute({
      mode: "query",
      scope,
      inputs,
      version: randomUUID(),
      sql: `SELECT *, count(*) OVER () AS __total FROM (${selects.join(" UNION ALL ")}) AS records ORDER BY ${order} LIMIT ${Math.max(1, sqlLimit)} OFFSET ${sqlOffset}`,
      limit: 500,
    });
    rows = sqlLimit > 0 ? result.rows : [];
    counted = Number(result.rows[0]?.__total ?? 0);
    // Past the last row there is no row to carry the total.
    if (!result.rows.length && sqlOffset > 0) {
      const all = await execute({
        mode: "query",
        scope,
        inputs,
        version: randomUUID(),
        sql: `SELECT count(*) AS n FROM (${selects.join(" UNION ALL ")}) AS records`,
        limit: 1,
      });
      counted = Number(all.rows[0]?.n ?? 0);
    }
  }
  const typeIds = new Map(all.map((t) => [t.name, t.data.typeId as string]));
  const idOf = (r: Record<string, any>) =>
    generation +
    "_" +
    digest(typeIds.get(r.__type) + ":" + String(r.__key)).slice(0, 32);
  const objects = await store.getMany(scope, rows.map(idOf));
  const page = [
    ...fromPinned,
    ...objects
      .filter((o): o is Resource => !!o)
      .map((o) => ({
        ...o,
        data: {
          ...o.data,
          effective: { ...o.data.base },
          edits: {},
          label: recordLabel(o.name, o.data.type, o.data.base || {}),
        },
      })),
  ];
  // Sorting by links already counted them in the snapshot, the way
  // publication linked them. HelixDB walks every link of a record to count
  // them, which takes a second for one with tens of thousands. Records whose
  // links were edited no longer follow the joins, so HelixDB counts those.
  const snapshotLinks =
    query.sort === "links"
      ? new Map(rows.map((r) => [idOf(r), Number(r.__links) || 0]))
      : new Map<string, number>();
  for (const id of await relationEditedIds(store, scope, generation))
    snapshotLinks.delete(id);
  const links = await store.linkCounts(
    scope,
    page.map((o) => o.id).filter((id) => !snapshotLinks.has(id)),
  );
  return {
    items: page.map((o) => ({
      ...o,
      data: {
        ...o.data,
        links: links.get(o.id) ?? snapshotLinks.get(o.id) ?? 0,
      },
    })),
    total: pinned.length + counted,
  };
}

export type SnapshotLink = { from: string; to: string; name: string };

// The records linked to each seed, found in the snapshot by the published
// relationships' joins rather than by walking the seed's links in HelixDB,
// which takes a second for a record with tens of thousands of links whatever
// the limit. Seeds share `room` records in proportion to their links. Links
// are those publication made: a pair with a relation edit is left out, and an
// edit that adds a link to a seed is included. A seed the snapshot does not
// find is returned in `unmatched`, for the caller to walk in HelixDB.
export async function snapshotNeighbours(
  store: Store,
  scope: string,
  seeds: { record: Resource; links: number }[],
  room: number,
): Promise<{
  ids: string[];
  links: SnapshotLink[];
  truncated: boolean;
  unmatched: string[];
}> {
  const generation = (await store.get(scope, "workspace"))?.data
    .activeGeneration;
  const linked = seeds.filter((s) => s.links > 0);
  if (!generation || !linked.length)
    return { ids: [], links: [], truncated: false, unmatched: [] };
  const all = (await store.list(scope, "objectType")).filter(
    (t) => t.data.generation === generation,
  );
  const byName = new Map(all.map((t) => [t.name, t])),
    bundle = await currentBundle(store, scope),
    { inputs, tableOf } = await snapshotInputs(store, scope, all);
  const logicalId = (type: string, key: string) =>
    digest(byName.get(type)?.data.typeId + ":" + key).slice(0, 32);
  const total = linked.reduce((n, s) => n + s.links, 0);
  const share = (s: { links: number }) =>
    Math.min(s.links, Math.max(1, Math.floor((room * s.links) / total)));
  const selects: string[] = [];
  for (const rel of bundle.relationships) {
    const from = byName.get(rel.fromType),
      to = byName.get(rel.toType),
      a = from && tableOf.get(from.name),
      b = to && tableOf.get(to.name);
    if (!from || !to || !a || !b) continue;
    const fromKey = `CAST(a.${q(from.data.primaryKey)} AS VARCHAR)`,
      toKey = `CAST(b.${q(to.data.primaryKey)} AS VARCHAR)`;
    // A seed at either end: its own key, the other end's type and key.
    for (const [seedType, seedKey, otherType, otherKey, outgoing] of [
      [from, fromKey, to, toKey, true],
      [to, toKey, from, fromKey, false],
    ] as const) {
      const keys = linked
        .filter((s) => s.record.data.type === seedType.name)
        .map((s) => lit(String(s.record.data.key)));
      if (keys.length)
        selects.push(
          `SELECT DISTINCT ${lit(seedType.name)} AS seed_type, ${seedKey} AS seed_key, ${lit(otherType.name)} AS other_type, ${otherKey} AS other_key, ${lit(rel.name)} AS name, ${outgoing} AS outgoing FROM ${q(a)} a JOIN ${q(b)} b ON a.${q(rel.fromColumn)} = b.${q(rel.toColumn)} WHERE ${seedKey} IN (${keys.join(", ")})`,
        );
    }
  }
  if (!selects.length)
    return {
      ids: [],
      links: [],
      truncated: false,
      unmatched: linked.map((s) => s.record.id),
    };
  const shares = linked
    .map(
      (s) =>
        `(${lit(s.record.data.type)}, ${lit(String(s.record.data.key))}, ${share(s)})`,
    )
    .join(", ");
  // One row per seed, so the result stays within the executor's row limit.
  const result = await execute({
    mode: "query",
    scope,
    inputs,
    version: randomUUID(),
    sql: `SELECT seed_type, seed_key, count(*) AS found, list({other_type: other_type, other_key: other_key, name: name, outgoing: outgoing} ORDER BY rank) FILTER (WHERE rank <= n) AS near FROM (SELECT p.*, s.n, row_number() OVER (PARTITION BY p.seed_type, p.seed_key ORDER BY p.other_type, p.other_key, p.name) AS rank FROM (${selects.join(" UNION ALL ")}) p JOIN (VALUES ${shares}) s(seed_type, seed_key, n) USING (seed_type, seed_key)) GROUP BY seed_type, seed_key`,
    limit: linked.length,
  });
  const edits = await store.list(scope, "relationEdit"),
    edited = new Set(
      edits.map((e) =>
        [e.data.fromLogicalId, e.data.toLogicalId, e.name].join(":"),
      ),
    );
  const ids: string[] = [],
    links: SnapshotLink[] = [],
    found = new Set<string>();
  let truncated = false;
  for (const row of result.rows) {
    const seed = logicalId(row.seed_type, String(row.seed_key));
    found.add(generation + "_" + seed);
    const near: any[] = row.near || [];
    if (Number(row.found) > near.length) truncated = true;
    for (const n of near) {
      const other = logicalId(n.other_type, String(n.other_key)),
        [from, to] = n.outgoing ? [seed, other] : [other, seed];
      if (edited.has([from, to, n.name].join(":"))) continue;
      ids.push(generation + "_" + other);
      links.push({
        from: generation + "_" + from,
        to: generation + "_" + to,
        name: n.name,
      });
    }
  }
  const seedIds = new Set(linked.map((s) => s.record.data.logicalId));
  for (const e of edits)
    if (
      e.data.enabled &&
      (seedIds.has(e.data.fromLogicalId) || seedIds.has(e.data.toLogicalId))
    ) {
      const other = seedIds.has(e.data.fromLogicalId)
        ? e.data.toLogicalId
        : e.data.fromLogicalId;
      ids.push(generation + "_" + other);
      links.push({
        from: generation + "_" + e.data.fromLogicalId,
        to: generation + "_" + e.data.toLogicalId,
        name: e.name,
      });
    }
  return {
    ids: [...new Set(ids)],
    links,
    truncated: truncated || linked.some((s) => s.links > share(s)),
    unmatched: linked.map((s) => s.record.id).filter((id) => !found.has(id)),
  };
}

// Records at either end of a relation edit, whose links differ from the
// snapshot's joins.
async function relationEditedIds(
  store: Store,
  scope: string,
  generation: string,
) {
  return (await store.list(scope, "relationEdit")).flatMap((e) => [
    generation + "_" + e.data.fromLogicalId,
    generation + "_" + e.data.toLogicalId,
  ]);
}

// Each published type's snapshot, named for one executor job: the version its
// records were published from.
async function snapshotInputs(store: Store, scope: string, all: Resource[]) {
  const inputs: { table: string; version: string }[] = [],
    tableOf = new Map<string, string>();
  const versionOf = async (t: Resource) => {
    if (t.data.sourceVersion) return t.data.sourceVersion as string;
    // Published before types recorded it: the version the dataset had when
    // publication began.
    const ds = await store.get(scope, t.data.datasetId);
    if (!ds) return undefined;
    if (ds.data.generation) return ds.data.activeVersion as string;
    const versions = (await store.getMany(scope, ds.data.versions || []))
      .filter((v): v is Resource => !!v && v.createdAt <= t.createdAt)
      .sort((a, b) => b.createdAt.localeCompare(a.createdAt));
    return (versions[0]?.id ?? ds.data.activeVersion) as string;
  };
  for (const t of all) {
    const version = await versionOf(t);
    if (!version) continue;
    const table = "t" + inputs.length;
    inputs.push({ table, version });
    tableOf.set(t.name, table);
  }
  return { inputs, tableOf };
}
