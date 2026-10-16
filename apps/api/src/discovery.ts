import {
  datasetTable,
  type Bundle,
  type ColumnProfile,
  type OntologyConfirm,
  type Resource,
  type SuggestedEntity,
  type SuggestedLink,
  type SuggestedOntology,
} from "../../../packages/shared/src/index.js";
import { pipelineId, queryDatasets } from "./datasets.js";
import { assert, digest } from "./security.js";
import type { Store } from "./store.js";

// Suggests a condensed ontology from the connected data: datasets that describe the
// same real-world thing in different systems are merged into one entity, and value
// overlap between key columns becomes links. Deterministic; one executor call.

type Info = {
  d: Resource;
  index: number;
  table: string;
  source: string;
  system: string;
  columns: ColumnProfile[];
  rows: number;
  key?: string;
  alt: Set<string>;
};
type Col = {
  i: number;
  ds: Info;
  name: string;
  column: string;
  field?: string;
  integer: boolean;
  unique: boolean;
  key: boolean;
};
type Edge = {
  A: Col;
  B: Col;
  identity: boolean;
  extension: boolean;
  declared: boolean;
  evidence: SuggestedLink["evidence"];
  exact: number;
  matched: number;
  total: number;
  caseRows: number;
};
type Draft = {
  members: SuggestedEntity["members"];
  anchor: Info;
  key: string;
  kind: SuggestedEntity["kind"];
  degree: number;
  name: string;
  id: string;
};

const labels: Record<string, string> = {
  planetscale: "PlanetScale",
  workos: "WorkOS",
  posthog: "PostHog",
  stripe: "Stripe",
  s3: "S3",
  postgres: "PostgreSQL",
  mysql: "MySQL",
  neo4j: "Neo4j",
  sqlite: "SQLite",
  rest: "REST",
  upload: "Upload",
};
const acronyms = new Set(
  "sso api url crm sku ip utm mrr arr kpi csv s3 sms vat ssn id".split(" "),
);
// Qualifiers that name a department or staging layer rather than the thing itself.
const qualifiers = new Set("marketing raw stg dim fact tbl app".split(" "));
export const q = (s: string) => '"' + s.replaceAll('"', '""') + '"';
// Code-point order, so results do not depend on the server's locale.
const cmp = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);
export const lit = (s: string) => "'" + s.replaceAll("'", "''") + "'";
const words = (s: string) =>
  s
    .replace(/([a-z0-9])([A-Z])/g, "$1_$2")
    .toLowerCase()
    .split(/[^a-z0-9]+/)
    .filter(Boolean);
const irregular: Record<string, string> = {
  movies: "movie",
  cookies: "cookie",
  ties: "tie",
  pies: "pie",
  calories: "calorie",
  selfies: "selfie",
  zombies: "zombie",
  rookies: "rookie",
  series: "series",
  species: "species",
  news: "news",
  people: "person",
  children: "child",
  statuses: "status",
  buses: "bus",
  analyses: "analysis",
  campuses: "campus",
  bonuses: "bonus",
  viruses: "virus",
};
const singular = (w: string) =>
  irregular[w] ??
  (/[^aeiou]ies$/.test(w)
    ? w.slice(0, -3) + "y"
    : /(ss|us|is|ics)$/.test(w)
      ? w
      : /(x|ch|sh|ss)es$/.test(w)
        ? w.slice(0, -2)
        : /s$/.test(w)
          ? w.slice(0, -1)
          : w);
const plurals = Object.fromEntries(
  Object.entries(irregular).map(([many, one]) => [one, many]),
);
export const plural = (phrase: string) =>
  phrase.replace(/(\w+)$/, (w) =>
    plurals[w.toLowerCase()]
      ? w.charAt(0) + plurals[w.toLowerCase()].slice(1)
      : /[^aeiou]y$/.test(w)
        ? w.slice(0, -1) + "ies"
        : /(s|x|ch|sh)$/.test(w)
          ? w + "es"
          : w + "s",
  );
const count = (n: number, one: string) =>
  `${n.toLocaleString("en-US")} ${n === 1 ? one : plural(one)}`;
const human = (ws: string[]) =>
  ws.map((w) => (acronyms.has(w) ? w.toUpperCase() : w)).join(" ");
const lowerFirst = (s: string) =>
  /^[A-Z]{2}/.test(s) ? s : s.charAt(0).toLowerCase() + s.slice(1);
const slug = (s: string) => words(s).join("_") || "entity";
const list = (items: string[]) =>
  items.length < 2
    ? items.join("")
    : items.slice(0, -1).join(", ") + " and " + items.at(-1);
export const sourceLabel = (source: string) =>
  labels[source] || source.charAt(0).toUpperCase() + source.slice(1);
const databases = new Set(
  "planetscale postgres postgresql mysql neon supabase sqlite".split(" "),
);
// "S3 · exports/2024/orders.csv" -> orders. Database tables may be schema-qualified
// (public.orders); in a file name the later dots are part of the name.
export const tableName = (name: string, source = "") => {
  const file = name.split(" · ").at(-1)!.split("/").at(-1)!;
  const base = file.replace(
    /\.(csv|tsv|txt|jsonl?|ndjson|parquet)(\.gz)?$/i,
    "",
  );
  const parts = base.split(".").filter(Boolean);
  return (
    (databases.has(source)
      ? parts.at(-1)
      : parts.find((p) => /[a-z]/i.test(p))) ||
    base ||
    file
  );
};
export function entityName(table: string) {
  let ws = words(table);
  if (ws.length > 1 && qualifiers.has(ws[0])) ws = ws.slice(1);
  if (!ws.length) return "Record";
  ws[ws.length - 1] = singular(ws[ws.length - 1]);
  const name = human(ws);
  return (name.charAt(0).toUpperCase() + name.slice(1)).slice(0, 60);
}
// "WorkOS organizations"; uploads have no system to name.
const datasetLabel = (x: { source: string; table: string }, n = 2) => {
  const ws = words(x.table);
  if (n === 1 && ws.length) ws[ws.length - 1] = singular(ws[ws.length - 1]);
  const table = human(ws);
  return x.source === "upload" ? table : `${sourceLabel(x.source)} ${table}`;
};
const keyType = (t: string) =>
  /^(VARCHAR|UUID|U?(TINY|SMALL|BIG|HUGE)?INT(EGER)?)$/.test(t);
// Names are compared as words, so "Customer ID", customer-id and customerId all
// read as customer_id. Natural keys (sku, order_number, handle) count as keys too.
const idWords = new Set(["id", "uuid", "guid"]);
const keyWords = new Set(
  "id uuid guid key code number no num sku slug handle username".split(" "),
);
const email = (n: string) => /e-?mail/i.test(n);
const keyName = (n: string) => keyWords.has(words(n).at(-1) || "") || email(n);
const idName = (n: string) => idWords.has(words(n).at(-1) || "");
const nested = (t: string) => /STRUCT|MAP\(|\[\]|UNION\(|^LIST/i.test(t);
// The noun a key column refers to: ticket_id -> ticket; a bare id or sku -> its table.
const stem = (c: { name: string; ds: Info }) => {
  const ws = words(c.name);
  return singular(
    (ws.length < 2 ? words(c.ds.table) : ws.slice(0, -1)).at(-1) || "",
  );
};
// Integer keys only pair with keys for the same noun; a shortened noun (org_id for
// organizations) still counts. The overlap query joins on the first letters.
const sameStem = (a: string, b: string) =>
  a === b ||
  (Math.min(a.length, b.length) >= 3 && (a.startsWith(b) || b.startsWith(a)));
const noun = (s: string) => singular(words(s).at(-1) || "");

// A dataset's own key: a unique non-null column named for the table (ticket_id in
// support_tickets). The profile flag alone also marks names and timestamps, so any
// other unique column is only a last resort.
function ownKey(x: Info) {
  const unique = x.columns.filter((c) => c.primaryKey && keyType(c.type));
  const nouns = words(x.table).map(singular);
  return (
    unique.find(
      (c) =>
        words(c.name).join("_") === "id" ||
        (idName(c.name) && nouns.includes(stem({ name: c.name, ds: x }))),
    ) ??
    unique.find((c) => idName(c.name)) ??
    unique.find((c) => keyName(c.name) && !email(c.name)) ??
    unique.find((c) => !/INT/.test(c.type) && !email(c.name)) ??
    unique.find((c) => /INT/.test(c.type))
  )?.name;
}
function declaredKeys(infos: Info[], sources: Map<string, Resource>) {
  const byTable = new Map<string, Info>();
  for (const x of infos) {
    const s = x.d.data.sourceId && sources.get(x.d.data.sourceId);
    if (s) byTable.set(x.system + "\u0000" + s.name.toLowerCase(), x);
  }
  const find = (system: string, table?: string) =>
    table
      ? (byTable.get(system + "\u0000" + table.toLowerCase()) ??
        [...byTable.entries()].find(
          ([k]) =>
            k.startsWith(system + "\u0000") &&
            k.endsWith("." + table.toLowerCase()),
        )?.[1])
      : undefined;
  const out = new Map<string, [Info, string, Info, string]>();
  for (const x of infos) {
    const s = x.d.data.sourceId && sources.get(x.d.data.sourceId);
    const found = s?.data.discovery;
    if (!found) continue;
    for (const k of [...(found.foreignKeys || []), ...(found.keys || [])]) {
      const from = find(x.system, k.tableName ?? k.table_name),
        to = find(x.system, k.referencedTable ?? k.referenced_table),
        fromColumn = k.columnName ?? k.column_name,
        toColumn = k.referencedColumn ?? k.referenced_column;
      if (from && to && from !== to && fromColumn && toColumn)
        out.set([from.d.id, fromColumn, to.d.id, toColumn].join("\u0000"), [
          from,
          fromColumn,
          to,
          toColumn,
        ]);
    }
  }
  return [...out.values()];
}
function structFields(type: string) {
  const m = /^STRUCT\((.*)\)$/.exec(type);
  if (!m) return [];
  const parts: string[] = [];
  let depth = 0,
    quoted = false,
    current = "";
  for (const ch of m[1]) {
    if (ch === '"') quoted = !quoted;
    if (!quoted && ch === "(") depth++;
    if (!quoted && ch === ")") depth--;
    if (!quoted && !depth && ch === ",") {
      parts.push(current.trim());
      current = "";
    } else current += ch;
  }
  parts.push(current.trim());
  return parts.flatMap((p) => {
    const f = /^("(?:[^"]|"")+"|\S+)\s+(.+)$/.exec(p);
    if (!f || f[2] !== "VARCHAR") return [];
    return [f[1].replace(/^"|"$/g, "").replaceAll('""', '"')];
  });
}

export async function suggestOntology(
  store: Store,
  scope: string,
  input: { datasets: Resource[]; sources: Resource[]; signature: string },
): Promise<SuggestedOntology> {
  const sources = new Map(input.sources.map((s) => [s.id, s]));
  const infos: Info[] = input.datasets
    .filter((d) => d.data.activeVersion && d.data.profile?.columns)
    .sort((a, b) => cmp(a.name, b.name) || cmp(a.id, b.id))
    .map((d, index) => {
      const s = d.data.sourceId && sources.get(d.data.sourceId);
      const [source, system] = d.data.sampleSource
        ? [d.data.sampleSource, "sample:" + d.data.sampleSource]
        : s
          ? [s.data.kind, "connection:" + (s.data.connectionId || s.id)]
          : ["upload", "dataset:" + d.id];
      return {
        d,
        index,
        table: tableName(d.name, source),
        source,
        system,
        columns: d.data.profile.columns,
        rows: d.data.profile.rows,
        alt: new Set<string>(),
      };
    });
  for (const x of infos) {
    x.key = ownKey(x);
    for (const c of x.columns)
      if (
        c.name !== x.key &&
        c.primaryKey &&
        keyType(c.type) &&
        keyName(c.name) &&
        !idName(c.name)
      )
        x.alt.add(c.name);
  }
  const declared = declaredKeys(infos, sources);
  const cols: Col[] = [];
  const column = (x: Info, c: ColumnProfile, field?: string) => {
    const name = field ? c.name + "." + field : c.name;
    const found = cols.find((y) => y.ds === x && y.name === name);
    if (found) return found;
    const col: Col = {
      i: cols.length,
      ds: x,
      name,
      column: c.name,
      field,
      integer: !field && /INT/.test(c.type),
      unique: !field && c.distinct > 0 && c.distinct === x.rows - c.nulls,
      key: !field && c.primaryKey,
    };
    cols.push(col);
    return col;
  };
  // A column named for another table (orders.customer, Stripe's invoice.subscription)
  // references it just like a *_id column.
  const refersTo = (x: Info, name: string) =>
    infos.some((y) => y !== x && noun(y.table) === noun(name));
  for (const x of infos)
    for (const c of x.columns) {
      if (
        keyType(c.type) &&
        (keyName(c.name) || refersTo(x, c.name)) &&
        c.distinct >= 2
      )
        column(x, c);
      for (const f of structFields(c.type)) if (keyName(f)) column(x, c, f);
    }
  const declaredPairs = new Set<string>();
  for (const [from, fromColumn, to, toColumn] of declared) {
    const a = from.columns.find((c) => c.name === fromColumn),
      b = to.columns.find((c) => c.name === toColumn);
    if (a && b && keyType(a.type) && keyType(b.type))
      declaredPairs.add(column(from, a).i + ":" + column(to, b).i);
  }
  const rows =
    cols.length > 1 ? await overlap(store, scope, cols, declaredPairs) : [];

  let edges: Edge[] = [];
  for (const r of rows) {
    const A = cols[r.a],
      B = cols[r.b];
    if (!A || !B || B.field) continue;
    const isDeclared = declaredPairs.has(A.i + ":" + B.i),
      bOwn = B.ds.key === B.name,
      bAlt = B.ds.alt.has(B.name),
      aOwn = !A.field && A.ds.key === A.name,
      aKey = aOwn || (!A.field && A.ds.alt.has(A.name));
    // Targets are a table's own key (or an alternate email/code key), which drops
    // transitive overlap such as invoices.customer_id -> subscriptions.customer_id.
    if (
      isDeclared
        ? !B.key
        : !(bOwn || bAlt) ||
          (bAlt && aOwn) ||
          ((A.integer || B.integer) && !sameStem(stem(A), stem(B)))
    )
      continue;
    const ci = email(A.name) || r.a_rows_exact < r.a_rows_ci;
    const m = ci ? r.m_ci : r.m_exact,
      distinct = ci ? r.a_distinct_ci : r.a_distinct,
      matched = ci ? r.a_rows_ci : r.a_rows_exact;
    if (
      !(isDeclared ? m > 0 : m / distinct >= 0.8 || matched / r.a_rows >= 0.9)
    )
      continue;
    const aw = words(A.name),
      bw = words(B.name);
    // Two uploads are one system to the user: a conventional foreign key between
    // them (subscriptions.customer_id) is a link, while a key named for another
    // system (stripe_customer_id, external_id) can still mean the same record.
    const conventional =
      aw.join("_") === bw.join("_") ||
      (aw.length > 1 &&
        idName(A.name) &&
        aw.slice(0, -1).map(singular).join("_") === noun(B.ds.table));
    // Merging needs exact matches, so the combined record joins without lower().
    const exact =
      r.m_exact / r.a_distinct >= 0.8 || r.a_rows_exact / r.a_rows >= 0.9;
    const identity =
      !A.field &&
      A.unique &&
      B.key &&
      A.ds.system !== B.ds.system &&
      !(A.ds.source === "upload" && B.ds.source === "upload" && conventional) &&
      exact;
    // A table keyed by another table's key (user_profiles.user_id) extends it 1:1.
    const extension =
      aOwn &&
      bOwn &&
      !identity &&
      A.ds.system === B.ds.system &&
      (isDeclared || m / distinct >= 0.8) &&
      noun(A.ds.table) !== stem(A) &&
      noun(B.ds.table) === stem(A);
    if (aKey && !identity && !extension) continue;
    const nameMatch =
      aw.join("_") === bw.join("_") ||
      aw.map(singular).includes(stem(B)) ||
      (bw.length === 1 && aw.at(-1) === bw[0]);
    edges.push({
      A,
      B,
      identity,
      extension,
      declared: isDeclared,
      evidence: isDeclared ? "declared" : nameMatch ? "name" : "values",
      exact: r.m_exact,
      matched,
      total: r.a_rows,
      caseRows: ci ? r.a_rows_ci - r.a_rows_exact : 0,
    });
  }
  // Serial integer ids overlap by chance, so two tables keyed by `id` are only the
  // same records when a declared key or a non-integer match between them agrees.
  const serial = (e: Edge) =>
    e.identity &&
    !e.declared &&
    e.A.integer &&
    e.B.integer &&
    e.A.ds.key === e.A.name &&
    e.B.ds.key === e.B.name;
  edges = edges.filter(
    (e) =>
      !serial(e) ||
      edges.some(
        (o) =>
          (o.identity || o.A.field) &&
          !o.A.integer &&
          ((o.A.ds === e.A.ds && o.B.ds === e.B.ds) ||
            (o.A.ds === e.B.ds && o.B.ds === e.A.ds)),
      ),
  );
  const rank = { declared: 2, name: 1, values: 0 };
  const strength = (a: Edge, b: Edge) =>
    b.exact - a.exact ||
    rank[b.evidence] - rank[a.evidence] ||
    b.matched / b.total - a.matched / a.total ||
    cmp(
      a.A.ds.d.name + a.A.name + a.B.ds.d.name + a.B.name,
      b.A.ds.d.name + b.A.name + b.B.ds.d.name + b.B.name,
    );
  edges.sort(strength);

  // Union-find over 1:1 cross-system links; an entity holds at most one dataset per
  // system, which stops an email match from merging accounts with users.
  const parent = infos.map((_, i) => i),
    systems = infos.map((x) => new Set([x.system]));
  const find = (i: number): number =>
    parent[i] === i ? i : (parent[i] = find(parent[i]));
  const tree: Edge[] = [],
    corroborating: Edge[] = [];
  for (const e of edges.filter((e) => e.identity)) {
    const a = find(e.A.ds.index),
      b = find(e.B.ds.index);
    if (a === b) corroborating.push(e);
    else if ([...systems[b]].some((s) => systems[a].has(s))) e.identity = false;
    else {
      parent[b] = a;
      systems[a] = new Set([...systems[a], ...systems[b]]);
      tree.push(e);
    }
  }
  const candidates = edges.filter(
    (e) =>
      !e.identity &&
      !e.A.field &&
      (e.extension || !(e.A.ds.key === e.A.name || e.A.ds.alt.has(e.A.name))),
  );
  // A key matching several tables refers to the one named for it: user_id points
  // at users, not at user_profiles, which is keyed by user_id too.
  const named = (e: Edge) => noun(e.B.ds.table) === stem(e.A);
  const references = candidates.filter(
    (e) =>
      e.declared ||
      named(e) ||
      !candidates.some((o) => o.A === e.A && o.B.ds !== e.B.ds && named(o)),
  );
  const degree = new Map<Info, number>();
  for (const e of [...tree, ...corroborating, ...references])
    for (const x of [e.A.ds, e.B.ds]) degree.set(x, (degree.get(x) || 0) + 1);
  const groups = new Map<number, Info[]>();
  for (const x of infos)
    groups.set(find(x.index), [...(groups.get(find(x.index)) || []), x]);

  const drafts: Draft[] = [],
    unplaced: string[] = [],
    entityOf = new Map<Info, Draft>();
  const appDb = (x: Info) =>
    ["planetscale", "postgres", "mysql", "sqlite"].includes(x.source) ? 1 : 0;
  for (const group of groups.values()) {
    const anchor = group
      .filter((x) => x.key || x.alt.size)
      .sort(
        (a, b) =>
          appDb(b) - appDb(a) ||
          (degree.get(b) || 0) - (degree.get(a) || 0) ||
          b.rows - a.rows ||
          cmp(a.d.name, b.d.name),
      )[0];
    if (!anchor) {
      unplaced.push(...group.map((x) => x.d.id));
      continue;
    }
    const member = (
      x: Info,
      via: SuggestedEntity["members"][number]["via"],
    ) => ({
      datasetId: x.d.id,
      datasetName: x.d.name,
      source: x.source,
      key: x.key || via?.column || [...x.alt][0],
      rows: x.rows,
      matched: x.rows,
      via,
    });
    const draft: Draft = {
      members: [member(anchor, null)],
      anchor,
      key: anchor.key || [...anchor.alt][0],
      kind: "record",
      degree: 0,
      name: "",
      id: "",
    };
    const joined = new Set([anchor]);
    for (let grew = true; grew; ) {
      grew = false;
      for (const e of tree) {
        const [inner, outer] = joined.has(e.A.ds)
          ? [e.A, e.B]
          : joined.has(e.B.ds)
            ? [e.B, e.A]
            : [];
        if (!inner || !outer || joined.has(outer.ds)) continue;
        joined.add(outer.ds);
        draft.members.push({
          ...member(outer.ds, {
            column: outer.name,
            targetDatasetId: inner.ds.d.id,
            targetColumn: inner.name,
          }),
          matched: e.exact,
        });
        grew = true;
      }
    }
    for (const x of group) entityOf.set(x, draft);
    drafts.push(draft);
  }

  // One link per entity pair and column; prefer the target entity's anchor dataset.
  const best = new Map<string, { e: Edge; from: Draft; to: Draft }>();
  for (const e of references) {
    const from = entityOf.get(e.A.ds),
      to = entityOf.get(e.B.ds);
    if (!from || !to || from === to) continue;
    const k = [drafts.indexOf(from), drafts.indexOf(to), e.A.name].join(":");
    const cur = best.get(k);
    const score = (x: Edge) => [
      x.B.ds === to.anchor ? 1 : 0,
      rank[x.evidence],
      x.matched,
    ];
    const [x, y] = [score(e), cur ? score(cur.e) : []],
      i = x.findIndex((v, j) => v !== y[j]);
    if (!cur || (i >= 0 && x[i] > y[i])) best.set(k, { e, from, to });
  }
  const refs = await withoutImplied(store, scope, [...best.values()]);
  const hasTime = (x: Info) =>
    x.columns.some((c) => /^(TIMESTAMP|DATE)/.test(c.type));
  // Large event streams feed metrics but are not stored as records. A large orders
  // table is still a record, so the stream must also look like one.
  const eventLike = (x: Info) =>
    x.source === "posthog" ||
    x.rows > 50000 ||
    words(x.table)
      .map(singular)
      .some((w) =>
        "event activity log track tracking click pageview visit session audit"
          .split(" ")
          .includes(w),
      );
  const streams = new Set(
    drafts.filter(
      (d) =>
        d.members.length === 1 &&
        d.anchor.rows > 2000 &&
        hasTime(d.anchor) &&
        eventLike(d.anchor) &&
        refs.some((r) => r.from === d) &&
        !refs.some((r) => r.to === d),
    ),
  );
  for (const d of streams)
    if (refs.some((r) => r.from === d && !streams.has(r.to)))
      d.kind = "activity";
  for (const r of refs) {
    r.from.degree++;
    r.to.degree++;
  }
  drafts.sort(
    (a, b) =>
      (a.kind === "activity" ? 1 : 0) - (b.kind === "activity" ? 1 : 0) ||
      b.degree - a.degree ||
      b.anchor.rows - a.anchor.rows ||
      cmp(a.anchor.d.name, b.anchor.d.name),
  );
  const names = new Set<string>(),
    ids = new Set<string>();
  for (const d of drafts) {
    let name = entityName(d.anchor.table);
    if (names.has(name.toLowerCase()))
      name =
        `${name} (${d.anchor.source === "upload" ? "upload" : sourceLabel(d.anchor.source)})`.slice(
          0,
          60,
        );
    for (let n = 2; names.has(name.toLowerCase()); n++)
      name = `${entityName(d.anchor.table)} ${n}`;
    names.add(name.toLowerCase());
    let id = slug(name);
    for (let n = 2; ids.has(id); n++) id = slug(name) + "_" + n;
    ids.add(id);
    d.name = name;
    d.id = id;
  }
  const byDataset = new Map(infos.map((x) => [x.d.id, x.d]));
  const plain = (d: Draft) => lowerFirst(d.name);
  const entities: SuggestedEntity[] = drafts.map((d) => {
    const targets = refs.filter((r) => r.from === d).map((r) => r.to);
    const others = d.members.slice(1).map((m) => {
      const x = infos.find((i) => i.d.id === m.datasetId)!;
      return datasetLabel(x);
    });
    const description =
      `${count(d.anchor.rows, plain(d))} from ${d.anchor.source === "upload" ? "an upload" : sourceLabel(d.anchor.source)}` +
      (others.length
        ? `, joined with ${list(others)}.`
        : d.kind === "activity"
          ? `, linked to ${list([...new Set(targets.map((t) => plural(plain(t))))])}.`
          : ".");
    const entity: SuggestedEntity = {
      id: d.id,
      name: d.name,
      description,
      kind: d.kind,
      anchorDatasetId: d.anchor.d.id,
      key: d.key,
      rows: d.anchor.rows,
      members: d.members,
      properties: [],
    };
    entity.properties = entityColumns(entity, byDataset)
      .filter((c) => !nested(c.type))
      .map((c) => c.as);
    return entity;
  });
  const order = new Map(drafts.map((d, i) => [d, i]));
  refs.sort(
    (a, b) =>
      order.get(a.from)! - order.get(b.from)! ||
      order.get(a.to)! - order.get(b.to)! ||
      cmp(a.e.A.name, b.e.A.name),
  );
  const linkIds = new Set<string>();
  const links: SuggestedLink[] = refs.map(({ e, from, to }) => {
    let id = `${from.id}-${slug(e.A.name)}-${to.id}`;
    for (let n = 2; linkIds.has(id); n++)
      id = `${from.id}-${slug(e.A.name)}-${to.id}-${n}`;
    linkIds.add(id);
    return {
      id,
      name: e.extension ? "extends" : linkName(e.A.name),
      from: from.id,
      to: to.id,
      fromDatasetId: e.A.ds.d.id,
      fromColumn: e.A.name,
      toDatasetId: e.B.ds.d.id,
      toColumn: e.B.name,
      totalRows: e.total,
      matchedRows: e.matched,
      caseInsensitive: e.caseRows > 0,
      evidence: e.evidence,
    };
  });

  await throughAnchor(store, scope, links, entities);

  const notes: SuggestedOntology["notes"] = [];
  const info = (id: string) => infos.find((x) => x.d.id === id)!;
  // Other 1:1 matches inside an entity, struct fields included, confirm the merge;
  // the join's own column pair, read in reverse, is not a second match.
  const confirmedBy = (d: Draft, datasetId: string) => {
    const via = d.members.find((m) => m.datasetId === datasetId)?.via;
    const join = new Set(
      via
        ? [
            [datasetId, via.column, via.targetDatasetId, via.targetColumn],
            [via.targetDatasetId, via.targetColumn, datasetId, via.column],
          ].map((p) => p.join("\u0000"))
        : [],
    );
    return [
      ...new Set(
        [...corroborating, ...edges.filter((e) => e.A.field)]
          .filter(
            (e) =>
              entityOf.get(e.A.ds) === d &&
              entityOf.get(e.B.ds) === d &&
              (e.A.ds.d.id === datasetId || e.B.ds.d.id === datasetId) &&
              !join.has(
                [e.A.ds.d.id, e.A.name, e.B.ds.d.id, e.B.name].join("\u0000"),
              ),
          )
          .map((e) => (e.A.ds.d.id === datasetId ? e.A.name : e.B.name)),
      ),
    ];
  };
  for (const d of drafts.filter((d) => d.members.length > 1))
    notes.push({
      kind: "merged",
      entities: [d.id],
      text: `${d.name} combines ${datasetLabel(d.anchor)} with ${list(
        d.members.slice(1).map((m) => {
          const also = confirmedBy(d, m.datasetId);
          return `${datasetLabel(info(m.datasetId))} (${m.matched.toLocaleString("en-US")} of ${m.rows.toLocaleString("en-US")} match${also.length ? ", confirmed by " + list(also) : ""})`;
        }),
      )}.`,
    });
  for (const d of drafts)
    for (const m of d.members.slice(1))
      if (m.rows > m.matched) {
        const n = m.rows - m.matched;
        notes.push({
          kind: "unmatched",
          entities: [d.id],
          text: `${n.toLocaleString("en-US")} ${datasetLabel(info(m.datasetId), n)} ${n === 1 ? "doesn't" : "don't"} match any ${plain(d)}.`,
        });
      }
  for (const [i, { e, to }] of refs.entries()) {
    const n = e.total - links[i].matchedRows,
      about = { entities: [links[i].from, links[i].to], links: [links[i].id] };
    if (n > 0)
      notes.push({
        kind: "unmatched",
        ...about,
        text: `${n.toLocaleString("en-US")} ${datasetLabel(e.A.ds, n)} ${n === 1 ? "has" : "have"} a ${e.A.name} that doesn't match any ${plain(to)}.`,
      });
    if (links[i].caseInsensitive) {
      const what = email(e.A.name)
        ? human(words(e.A.name.replace(/_?e-?mail$/i, "")).concat(["email"]))
        : `${plain(to)} code`;
      notes.push({
        kind: "case",
        ...about,
        text: `${count(e.caseRows, what)} only ${e.caseRows === 1 ? "matches" : "match"} when ignoring case.`,
      });
    }
  }
  if (drafts.length > 1)
    for (const d of drafts)
      if (!d.degree)
        notes.push({
          kind: "unlinked",
          entities: [d.id],
          text: `${d.name} isn't linked to the other data.`,
        });
  for (const id of unplaced)
    notes.push({
      kind: "unlinked",
      text: `${byDataset.get(id)!.name} has no unique column to identify its rows, so it isn't shown as an entity.`,
    });

  // Uploads count as one source, however many files there are.
  const summary = summarize(
    entities,
    infos.length,
    new Set(infos.map((x) => (x.source === "upload" ? "upload" : x.system)))
      .size,
  );
  const hash = digest(
    JSON.stringify({
      entities: entities.map((e) => [
        e.id,
        e.kind,
        e.anchorDatasetId,
        e.key,
        e.members.map((m) => [m.datasetId, m.key, m.via]),
      ]),
      links: links.map((l) => [
        l.id,
        l.from,
        l.to,
        l.fromDatasetId,
        l.fromColumn,
        l.toDatasetId,
        l.toColumn,
        l.caseInsensitive,
      ]),
    }),
  );
  return {
    signature: input.signature,
    hash,
    summary,
    described: false,
    entities,
    links,
    notes,
    unplaced,
  };
}

// "7 entities and 1 activity stream from 11 datasets across 5 sources. Accounts
// combine PlanetScale, WorkOS and Stripe."
function summarize(
  entities: SuggestedEntity[],
  datasets: number,
  sources: number,
) {
  const records = entities.filter((e) => e.kind === "record").length,
    streams = entities.length - records;
  const merged = entities
    .filter((e) => e.members.length > 1)
    .map(
      (e) =>
        `${plural(lowerFirst(e.name))} combine ${list([
          ...new Set(
            e.members.map((m) =>
              m.source === "upload"
                ? human(words(tableName(m.datasetName, m.source)))
                : sourceLabel(m.source),
            ),
          ),
        ])}`,
    );
  return (
    `${count(records, "entity")}${streams ? ` and ${count(streams, "activity stream")}` : ""} from ${count(datasets, "dataset")} across ${count(sources, "source")}.` +
    (merged.length
      ? " " + merged.join("; ").replace(/^./, (c) => c.toUpperCase()) + "."
      : "")
  );
}

// A link into a merged entity's other dataset (subscriptions -> Stripe customers,
// part of Account) only resolves for the rows that joined the anchor, so it is
// counted the way the built relationship will be. One executor call.
async function throughAnchor(
  store: Store,
  scope: string,
  links: SuggestedLink[],
  entities: SuggestedEntity[],
) {
  const byId = new Map(entities.map((e) => [e.id, e]));
  const text = (alias: string, column: string, ci = false) =>
    ci
      ? `lower(trim(CAST(${alias}.${q(column)} AS VARCHAR)))`
      : `CAST(${alias}.${q(column)} AS VARCHAR)`;
  const ids = new Set<string>();
  // Values of a member column for the member rows that reach the anchor.
  const joined = (
    e: SuggestedEntity,
    datasetId: string,
    column: string,
    depth: number,
  ): string | undefined => {
    const m = e.members.find((x) => x.datasetId === datasetId);
    if (!m || depth > e.members.length) return undefined;
    ids.add(datasetId);
    const alias = "j" + depth,
      from = `SELECT ${text(alias, column)} AS v FROM ${q(datasetTable(datasetId))} ${alias}`;
    if (!m.via) return from;
    const target = joined(
      e,
      m.via.targetDatasetId,
      m.via.targetColumn,
      depth + 1,
    );
    return (
      target && `${from} WHERE ${text(alias, m.via.column)} IN (${target})`
    );
  };
  const counts = links.flatMap((l, i) => {
    const to = byId.get(l.to);
    if (!to || l.toDatasetId === to.anchorDatasetId) return [];
    const values = joined(to, l.toDatasetId, l.toColumn, 0);
    if (!values) return [];
    ids.add(l.fromDatasetId);
    const ci = l.caseInsensitive;
    return [
      `SELECT ${i} AS k, count(*) AS n FROM ${q(datasetTable(l.fromDatasetId))} a WHERE ${text("a", l.fromColumn, ci)} IN (SELECT ${ci ? "lower(trim(v))" : "v"} FROM (${values}) AS t)`,
    ];
  });
  if (!counts.length) return;
  const r = await queryDatasets(
    store,
    scope,
    [...ids],
    counts.join(" UNION ALL "),
    {
      limit: 1000,
    },
  );
  for (const row of r.rows) {
    const l = links[Number(row.k)];
    l.matchedRows = Math.min(l.matchedRows, Number(row.n));
  }
}

type Ref = { e: Edge; from: Draft; to: Draft };
// A direct link A -> C that a two-hop path A -> B -> C already gives (invoice ->
// subscription -> account) is left to traversal, once the data shows both routes
// reach the same record. Checked in one executor call.
async function withoutImplied(store: Store, scope: string, refs: Ref[]) {
  const paths = refs.flatMap((direct) =>
    refs.flatMap((first) =>
      first === direct || first.from !== direct.from || first.to === direct.to
        ? []
        : refs
            .filter(
              (second) =>
                second !== direct &&
                second.from === first.to &&
                second.to === direct.to &&
                // Comparable row by row: both routes start on the same table, the
                // first hop lands where the second starts, and both end on one column.
                first.e.A.ds === direct.e.A.ds &&
                first.e.B.ds === second.e.A.ds &&
                second.e.B.ds === direct.e.B.ds &&
                second.e.B.name === direct.e.B.name,
            )
            .map((second) => ({ direct, first, second })),
    ),
  );
  if (!paths.length) return refs;
  const value = (expr: string, ci: boolean) =>
    ci ? `lower(trim(CAST(${expr} AS VARCHAR)))` : `CAST(${expr} AS VARCHAR)`;
  const sql = paths
    .map(({ direct, first, second }, i) => {
      const hop = first.e.caseRows > 0,
        end = direct.e.caseRows > 0 || second.e.caseRows > 0,
        d = `a.${q(direct.e.A.name)}`,
        via = `b.${q(second.e.A.name)}`;
      // Every row with the direct key counts: one whose path is missing (no first hop,
      // or no second key) would lose its only route to the target.
      return `SELECT ${i} AS k, count(*) AS n, count(*) FILTER (WHERE ${via} IS NOT NULL AND ${value(d, end)} = ${value(via, end)}) AS agree
FROM ${q(datasetTable(direct.e.A.ds.d.id))} a LEFT JOIN ${q(datasetTable(first.e.B.ds.d.id))} b
ON ${value(`a.${q(first.e.A.name)}`, hop)} = ${value(`b.${q(first.e.B.name)}`, hop)}
WHERE ${d} IS NOT NULL`;
    })
    .join(" UNION ALL ");
  const ids = [
    ...new Set(
      paths.flatMap((p) => [p.direct.e.A.ds.d.id, p.first.e.B.ds.d.id]),
    ),
  ];
  const r = await queryDatasets(store, scope, ids, sql, { limit: 1000 });
  const agreed = new Set(
    r.rows
      .filter(
        (row) => Number(row.n) > 0 && Number(row.agree) >= 0.95 * Number(row.n),
      )
      .map((row) => Number(row.k)),
  );
  // Paths must stay intact: never drop a hop of a dropped link, or drop a link
  // through a hop that was dropped.
  const implied = new Set<Ref>(),
    hops = new Set<Ref>();
  for (const [i, p] of paths.entries())
    if (
      agreed.has(i) &&
      !hops.has(p.direct) &&
      !implied.has(p.first) &&
      !implied.has(p.second)
    ) {
      implied.add(p.direct);
      hops.add(p.first).add(p.second);
    }
  return refs.filter((ref) => !implied.has(ref));
}

// Value index over every key-like column, self-joined to count shared values for
// each column pair (exact and ignoring case), in one executor call. Integer columns
// only pair with columns for a similar noun, which the join enforces so serial ids
// never multiply out across unrelated tables.
async function overlap(
  store: Store,
  scope: string,
  cols: Col[],
  declared: Set<string>,
) {
  const values = cols
    .map((c) => {
      const e = c.field
        ? `struct_extract(${q(c.column)}, ${lit(c.field)})`
        : q(c.column);
      return `SELECT ${c.i} AS c, ${c.ds.index} AS d, ${c.integer ? 1 : 0} AS i, ${lit(stem(c).slice(0, 3))} AS s, CAST(${e} AS VARCHAR) AS v FROM ${q(datasetTable(c.ds.d.id))} WHERE ${e} IS NOT NULL`;
    })
    .join(" UNION ALL ");
  const pairs = [...declared].map((p) => p.split(":").map(Number));
  // Declared keys pair whatever their names.
  const matching = (t: string, agg: string) =>
    [
      `SELECT a.c AS a, b.c AS b, ${agg} FROM ${t} a JOIN ${t} b ON a.v = b.v AND a.d <> b.d AND a.i = 0 AND b.i = 0 GROUP BY a.c, b.c`,
      `SELECT a.c AS a, b.c AS b, ${agg} FROM ${t} a JOIN ${t} b ON a.v = b.v AND a.s = b.s AND a.d <> b.d AND (a.i = 1 OR b.i = 1) GROUP BY a.c, b.c`,
      ...(pairs.length
        ? [
            `SELECT a.c AS a, b.c AS b, ${agg} FROM (SELECT * FROM ${t} WHERE c IN (${pairs.map((p) => p[0]).join(", ")})) a JOIN (SELECT * FROM ${t} WHERE c IN (${pairs.map((p) => p[1]).join(", ")})) b ON a.v = b.v AND a.d <> b.d AND a.s <> b.s AND (a.i = 1 OR b.i = 1) WHERE ${pairs.map(([x, y]) => `(a.c = ${x} AND b.c = ${y})`).join(" OR ")} GROUP BY a.c, b.c`,
          ]
        : []),
    ].join(" UNION ALL ");
  const sql = `WITH vals AS (SELECT c, d, i, s, v FROM (${values}) AS x WHERE trim(v) <> ''),
ex AS (SELECT c, d, i, s, v, count(*) AS n FROM vals GROUP BY c, d, i, s, v),
ci AS (SELECT c, d, i, s, lower(trim(v)) AS v, count(*) AS n FROM vals GROUP BY c, d, i, s, lower(trim(v))),
cnt AS (SELECT c, count(*) AS d_exact, sum(n) AS rows_nonnull FROM ex GROUP BY c),
cnt_ci AS (SELECT c, count(*) AS d_ci FROM ci GROUP BY c),
pe AS (${matching("ex", "count(*) AS m_exact, sum(a.n) AS a_rows_exact")}),
pc AS (${matching("ci", "count(*) AS m_ci, sum(a.n) AS a_rows_ci")})
SELECT pc.a, pc.b, coalesce(pe.m_exact, 0) AS m_exact, pc.m_ci, coalesce(pe.a_rows_exact, 0) AS a_rows_exact, pc.a_rows_ci,
  ca.d_exact AS a_distinct, cca.d_ci AS a_distinct_ci, ca.rows_nonnull AS a_rows
FROM pc LEFT JOIN pe ON pe.a = pc.a AND pe.b = pc.b
JOIN cnt ca ON ca.c = pc.a JOIN cnt_ci cca ON cca.c = pc.a JOIN cnt_ci ccb ON ccb.c = pc.b
WHERE pc.m_ci >= 0.5 * least(cca.d_ci, ccb.d_ci) OR pc.m_ci >= 20${pairs.map(([x, y]) => ` OR (pc.a = ${x} AND pc.b = ${y})`).join("")}
ORDER BY pc.m_ci DESC, pc.a, pc.b`;
  const ids = [...new Set(cols.map((c) => c.ds.d.id))];
  // The executor returns at most 1000 rows a call.
  const rows: Record<string, unknown>[] = [];
  for (let total = Infinity; rows.length < total; ) {
    const r = await queryDatasets(store, scope, ids, sql, {
      limit: 1000,
      offset: rows.length,
    });
    total = Number(r.profile?.rows ?? r.rows.length);
    if (!r.rows.length) break;
    rows.push(...r.rows);
  }
  return rows.map(
    (row) =>
      Object.fromEntries(
        Object.entries(row).map(([k, v]) => [k, Number(v)]),
      ) as Record<
        | "a"
        | "b"
        | "m_exact"
        | "m_ci"
        | "a_rows_exact"
        | "a_rows_ci"
        | "a_distinct"
        | "a_distinct_ci"
        | "a_rows",
        number
      >,
  );
}

function linkName(column: string) {
  const c = column.toLowerCase();
  if (/requester|reporter|submitted_by|author/.test(c)) return "requested by";
  if (/campaign|utm/.test(c)) return "came from";
  if (/subscription/.test(c)) return "for subscription";
  if (/customer|billing/.test(c)) return "billed to";
  if (
    /account|organi[sz]ation|(^|_)org(_|$)|company|tenant|workspace|team/.test(
      c,
    )
  )
    return "belongs to";
  if (/owner/.test(c)) return "owned by";
  if (/user|distinct_id|person|actor|member/.test(c)) return "performed by";
  return "references";
}

// Output columns of an entity: the anchor's columns, then each merged dataset's
// columns prefixed with its system (workos_name), skipping the join column.
export function entityColumns(
  entity: Pick<SuggestedEntity, "anchorDatasetId" | "members">,
  datasets: Map<string, Resource>,
) {
  const prefixes = new Map<string, string>(),
    taken = new Set<string>();
  for (const m of entity.members) {
    if (m.datasetId === entity.anchorDatasetId) continue;
    const table = slug(entityName(tableName(m.datasetName, m.source)));
    let p = m.source === "upload" ? table : slug(m.source);
    if (taken.has(p)) p = `${slug(m.source)}_${table}`;
    for (let n = 2, base = p; taken.has(p); n++) p = `${base}_${n}`;
    taken.add(p);
    prefixes.set(m.datasetId, p);
  }
  const used = new Set<string>(),
    out: { datasetId: string; column: string; as: string; type: string }[] = [];
  const ordered = [
    ...entity.members.filter((m) => m.datasetId === entity.anchorDatasetId),
    ...entity.members.filter((m) => m.datasetId !== entity.anchorDatasetId),
  ];
  for (const m of ordered)
    for (const c of datasets.get(m.datasetId)?.data.profile?.columns || []) {
      if (m.via && c.name === m.via.column) continue;
      const as = prefixes.has(m.datasetId)
        ? `${prefixes.get(m.datasetId)}_${c.name}`
        : c.name;
      if (used.has(as.toLowerCase())) continue;
      used.add(as.toLowerCase());
      out.push({ datasetId: m.datasetId, column: c.name, as, type: c.type });
    }
  return out;
}
// The entity output column holding a dataset column's values, following join keys
// to the dataset they were merged on.
export function entityColumn(
  entity: Pick<SuggestedEntity, "anchorDatasetId" | "members">,
  datasets: Map<string, Resource>,
  datasetId: string,
  column: string,
  depth = 0,
): string | undefined {
  const m = entity.members.find((x) => x.datasetId === datasetId);
  if (!m || depth > entity.members.length) return undefined;
  if (m.via && m.via.column === column)
    return entityColumn(
      entity,
      datasets,
      m.via.targetDatasetId,
      m.via.targetColumn,
      depth + 1,
    );
  return entityColumns(entity, datasets).find(
    (c) => c.datasetId === datasetId && c.column === column,
  )?.as;
}

// Applies the user's review: drops excluded entities (and their links) and renames.
export function confirmOntology(
  s: SuggestedOntology,
  edits: OntologyConfirm,
): SuggestedOntology {
  const ids = new Set(s.entities.map((e) => e.id));
  for (const id of [...edits.excluded, ...Object.keys(edits.names)])
    assert(ids.has(id), "Unknown entity in the suggested ontology");
  const excluded = new Set(edits.excluded);
  const entities = s.entities
    .filter((e) => !excluded.has(e.id))
    .map((e) => {
      const name = edits.names[e.id]?.trim();
      return name === undefined ? e : { ...e, name };
    });
  assert(
    entities.every((e) => e.name.length > 0),
    "Entity names cannot be empty",
  );
  assert(
    new Set(entities.map((e) => e.name.toLowerCase())).size === entities.length,
    "Entity names must be unique",
  );
  // Data without a recognisable key still builds dashboards, so setup never stalls.
  assert(
    entities.some((e) => e.kind === "record") ||
      !s.entities.some((e) => e.kind === "record"),
    "Keep at least one entity to build records from",
  );
  const links = s.links.filter(
    (l) => !excluded.has(l.from) && !excluded.has(l.to),
  );
  const kept = new Set(links.map((l) => l.id));
  const renamed = Object.keys(edits.names).length > 0;
  const members = entities.flatMap((e) => e.members);
  return {
    ...s,
    summary:
      excluded.size || (renamed && !s.described)
        ? summarize(
            entities,
            new Set(members.map((m) => m.datasetId)).size + s.unplaced.length,
            new Set(members.map((m) => m.source)).size,
          )
        : s.summary,
    entities,
    links,
    notes: s.notes.filter(
      (n) =>
        !n.entities?.some((id) => excluded.has(id)) &&
        !n.links?.some((id) => !kept.has(id)),
    ),
  };
}
export const ontologyGoal = (o: SuggestedOntology) =>
  `Give an overview of ${list(o.entities.map((e) => plural(lowerFirst(e.name))))}, and how they connect.`.slice(
    0,
    4000,
  );

// Bundle definitions for a confirmed ontology: a pipeline per merged entity (the
// anchor LEFT JOIN its merged datasets), object types, and relationships. Links that
// match only ignoring case, or across column types, join on lower()-normalized key
// columns added by pipelines; a link that cannot be expressed is left out.
export function ontologyBundle(
  o: SuggestedOntology,
  datasets: Map<string, Resource>,
): Pick<
  Bundle,
  | "summary"
  | "assumptions"
  | "catalog"
  | "pipelines"
  | "ontology"
  | "relationships"
> {
  const records = o.entities
    .filter(
      (e) =>
        e.kind === "record" &&
        e.members.every((m) => datasets.has(m.datasetId)),
    )
    .slice(0, 20);
  const byId = new Map(records.map((e) => [e.id, e]));
  const type = (datasetId: string, column: string) =>
    datasets
      .get(datasetId)
      ?.data.profile.columns.find((c: ColumnProfile) => c.name === column)
      ?.type;
  const outputType = (e: SuggestedEntity, as: string) =>
    entityColumns(e, datasets).find((c) => c.as === as)?.type;
  const planned = o.links.flatMap((l) => {
    const from = byId.get(l.from),
      to = byId.get(l.to);
    const fromColumn =
        from && entityColumn(from, datasets, l.fromDatasetId, l.fromColumn),
      toColumn = to && entityColumn(to, datasets, l.toDatasetId, l.toColumn);
    if (!from || !to || !fromColumn || !toColumn) return [];
    const normalize =
      l.caseInsensitive ||
      outputType(from, fromColumn) !== outputType(to, toColumn);
    return [{ l, from, to, fromColumn, toColumn, normalize }];
  });
  const keysFor = new Map<string, Set<string>>();
  for (const p of planned)
    if (p.normalize)
      for (const [e, c] of [
        [p.from, p.fromColumn],
        [p.to, p.toColumn],
      ] as const)
        keysFor.set(e.id, new Set([...(keysFor.get(e.id) || []), c]));
  const piped = new Set(
    records
      .filter((e) => e.members.length > 1)
      .slice(0, 10)
      .map((e) => e.id),
  );
  for (const e of records)
    if (keysFor.has(e.id) && piped.size < 10) piped.add(e.id);
  const pipelines: Bundle["pipelines"] = [],
    ontology: Bundle["ontology"] = [],
    available = new Map<string, Set<string>>(),
    keyColumns = new Map<string, Map<string, string>>();
  for (const e of records) {
    const anchorOnly = {
      anchorDatasetId: e.anchorDatasetId,
      members: e.members.filter((m) => m.datasetId === e.anchorDatasetId),
    };
    const cols = entityColumns(piped.has(e.id) ? e : anchorOnly, datasets);
    const names = new Set(cols.map((c) => c.as));
    let datasetId = e.anchorDatasetId;
    if (piped.has(e.id)) {
      const alias = new Map(e.members.map((m, i) => [m.datasetId, `m${i}`]));
      alias.set(e.anchorDatasetId, "a");
      const keys = new Map<string, string>(),
        select = cols.map(
          (c) =>
            `${alias.get(c.datasetId)}.${q(c.column)}` +
            (c.as === c.column ? "" : ` AS ${q(c.as)}`),
        );
      for (const as of keysFor.get(e.id) || []) {
        const c = cols.find((x) => x.as === as)!;
        let key = `${as}_key`;
        for (
          let n = 2;
          [...names].some((x) => x.toLowerCase() === key.toLowerCase());
          n++
        )
          key = `${as}_key_${n}`;
        names.add(key);
        keys.set(as, key);
        select.push(
          `lower(trim(CAST(${alias.get(c.datasetId)}.${q(c.column)} AS VARCHAR))) AS ${q(key)}`,
        );
      }
      keyColumns.set(e.id, keys);
      const joined = new Set([e.anchorDatasetId]),
        joins: string[] = [];
      for (let grew = true; grew; ) {
        grew = false;
        for (const m of e.members)
          if (
            m.via &&
            !joined.has(m.datasetId) &&
            joined.has(m.via.targetDatasetId)
          ) {
            const cast =
              type(m.datasetId, m.via.column) !==
              type(m.via.targetDatasetId, m.via.targetColumn);
            const side = (a: string, c: string) =>
              cast ? `CAST(${a}.${q(c)} AS VARCHAR)` : `${a}.${q(c)}`;
            joins.push(
              `LEFT JOIN ${q(datasetTable(m.datasetId))} ${alias.get(m.datasetId)} ON ${side(alias.get(m.datasetId)!, m.via.column)} = ${side(alias.get(m.via.targetDatasetId)!, m.via.targetColumn)}`,
            );
            joined.add(m.datasetId);
            grew = true;
          }
      }
      const name = `${plural(e.name)} (${[...new Set(e.members.filter((m) => joined.has(m.datasetId)).map((m) => sourceLabel(m.source)))].join(" + ")})`;
      datasetId = pipelineId(name);
      pipelines.push({
        name,
        description: e.description,
        inputs: [...joined],
        sql: `SELECT ${select.join(", ")} FROM ${q(datasetTable(e.anchorDatasetId))} a ${joins.join(" ")}`.trim(),
        nodes: [],
        checks: [
          {
            name: `One row per ${lowerFirst(e.name)}`,
            sql: `SELECT count(*) = count(DISTINCT ${q(e.key)}) AND count(${q(e.key)}) = count(*) AS ok FROM ${q(datasetTable(datasetId))}`,
          },
        ],
      });
    }
    available.set(e.id, names);
    ontology.push({
      name: e.name,
      datasetId,
      primaryKey: e.key,
      properties: [
        ...new Set([e.key, ...e.properties.filter((p) => names.has(p))]),
      ],
      description: e.description,
    });
  }
  const relationships: Bundle["relationships"] = [];
  for (const p of planned) {
    const fromColumn = p.normalize
        ? keyColumns.get(p.from.id)?.get(p.fromColumn)
        : p.fromColumn,
      toColumn = p.normalize
        ? keyColumns.get(p.to.id)?.get(p.toColumn)
        : p.toColumn;
    if (
      !fromColumn ||
      !toColumn ||
      !available.get(p.from.id)!.has(fromColumn) ||
      !available.get(p.to.id)!.has(toColumn)
    )
      continue;
    relationships.push({
      name: p.l.name,
      fromType: p.from.name,
      toType: p.to.name,
      fromColumn,
      toColumn,
    });
  }
  // Relationship names are identities, so repeated verbs name their subject.
  const repeated = (r: (typeof relationships)[number]) =>
    relationships.filter((x) => x.name === r.name).length > 1;
  const named = relationships.map((r) =>
    repeated(r) ? { ...r, name: `${r.fromType} ${r.name}` } : { ...r },
  );
  const seen = new Set<string>();
  for (const r of named) {
    const base = seen.has(r.name) ? `${r.name} ${r.toType}` : r.name;
    let name = base;
    for (let n = 2; seen.has(name); n++) name = `${base} ${n}`;
    seen.add(name);
    r.name = name;
  }
  const catalog = [
    ...o.entities.flatMap((e) =>
      e.members
        .filter((m) => datasets.has(m.datasetId))
        .map((m) => ({
          datasetId: m.datasetId,
          description:
            datasets.get(m.datasetId)!.description ||
            `${datasetLabel({ source: m.source, table: tableName(m.datasetName, m.source) })}: ${e.name} ${e.kind === "activity" ? "activity" : "records"}.`,
          tags: [slug(e.name), m.source],
        })),
    ),
    // Tables outside the schema still feed dashboards.
    ...o.unplaced.flatMap((id) => {
      const d = datasets.get(id);
      return d
        ? [
            {
              datasetId: id,
              description: d.description || `${d.name}: rows kept as a table.`,
              tags: [slug(tableName(d.name, d.data.sampleSource || ""))],
            },
          ]
        : [];
    }),
  ];
  return {
    summary: o.summary,
    assumptions: o.notes
      .filter((n) => n.kind === "case" || n.kind === "unmatched")
      .map((n) =>
        n.kind === "case"
          ? `${n.text} They are linked on lower-cased values.`
          : n.text,
      ),
    catalog,
    pipelines,
    ontology,
    relationships: named.slice(0, 30),
  };
}

// A dashboard computed from a confirmed ontology, for a build whose model-designed
// widgets all fail: the hub entity's records, records per entity, and new records
// per month. Plain counts over the source tables, so it validates without a model.
export function overviewDashboard(
  o: SuggestedOntology,
  datasets: Map<string, Resource>,
): Bundle["dashboards"][number] {
  const degree = (e: SuggestedEntity) =>
    o.links.filter((l) => l.from === e.id || l.to === e.id).length;
  const records = o.entities
    .filter((e) => e.kind === "record" && datasets.has(e.anchorDatasetId))
    .sort((a, b) => degree(b) - degree(a))
    .slice(0, 20);
  // Without records, each table counts for itself.
  const groups = (
    records.length
      ? records.map((e) => ({
          name: plural(e.name),
          datasetId: e.anchorDatasetId,
        }))
      : [...datasets.values()].map((d) => ({
          name: plural(
            entityName(tableName(d.name, d.data.sampleSource || "")),
          ),
          datasetId: d.id,
        }))
  ).slice(0, 20);
  assert(groups.length, "Connect or upload a dataset first");
  const from = (id: string) => q(datasetTable(id));
  const counts = groups
    .map(
      (g) =>
        `SELECT ${lit(g.name)} AS "type", count(*) AS "records" FROM ${from(g.datasetId)}`,
    )
    .join(" UNION ALL ");
  const inputs = [...new Set(groups.map((g) => g.datasetId))];
  const widgets: Bundle["dashboards"][number]["widgets"] = [
    records.length
      ? {
          title: groups[0].name,
          type: "metric",
          sql: `SELECT count(*) AS "records" FROM ${from(groups[0].datasetId)}`,
          inputs: [groups[0].datasetId],
        }
      : {
          title: "Records",
          type: "metric",
          sql: `SELECT CAST(sum("records") AS BIGINT) AS "records" FROM (${counts})`,
          inputs,
        },
  ];
  if (groups.length > 1)
    widgets.push({
      title: "Records by type",
      type: "bar",
      sql: `SELECT * FROM (${counts}) ORDER BY "records" DESC, "type"`,
      inputs,
      x: "type",
      y: "records",
    });
  const temporal = (c: ColumnProfile) => /^(DATE|TIMESTAMP)/i.test(c.type);
  const dated = [/creat/i, /date|start|sign.?up|joined/i]
    .flatMap((pattern) =>
      records.flatMap((e) => {
        const c = datasets
          .get(e.anchorDatasetId)!
          .data.profile?.columns.find(
            (c: ColumnProfile) => temporal(c) && pattern.test(c.name),
          );
        return c ? [{ e, column: q(c.name) }] : [];
      }),
    )
    .at(0);
  if (dated)
    widgets.push({
      title: `New ${plural(lowerFirst(dated.e.name))} per month`,
      type: "line",
      sql: `SELECT strftime(CAST(${dated.column} AS TIMESTAMP), '%Y-%m') AS "month", count(*) AS "records" FROM ${from(dated.e.anchorDatasetId)} WHERE ${dated.column} IS NOT NULL GROUP BY 1 ORDER BY 1`,
      inputs: [dated.e.anchorDatasetId],
      x: "month",
      y: "records",
    });
  return { name: "Overview", description: o.summary, widgets };
}
