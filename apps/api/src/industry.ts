// Industry playbooks (docs/industries): Foundry's opinion of how to read one
// industry's data, applied to this workspace in the background. A playbook
// names roles (order, customer, amount ...), never columns; code maps them
// onto the workspace's datasets, checks what the data covers and how well it
// links, and ranks what would help next. The workspace's business profile
// holds what is true for this business: the owner's own description of it,
// knowledge (targets, normal ranges, facts), and what people's questions show
// they care about. Nothing here asks anyone to choose anything.
import { readdirSync, readFileSync } from "node:fs";
import { z } from "zod";
import {
  datasetTable,
  type BusinessInsights,
  type BusinessKnowledge,
  type Resource,
} from "../../../packages/shared/src/index.js";
import { queryDatasets } from "./datasets.js";
import { q } from "./discovery.js";
import { aiReady, metricsDatasets, noun } from "./metrics.js";
import { digest } from "./security.js";
import { resource, transaction, type Store } from "./store.js";

type Role = {
  title: string;
  entity?: boolean;
  of?: string[];
  type?: "number" | "date" | "category" | "text" | "any";
  names: string[];
};
type Area = {
  id: string;
  title: string;
  keywords: string[];
  metrics: string[];
};
type Metric = { title: string; needs: string[]; hint?: string };
type Normal = {
  low: number;
  high: number;
  unit: "percent" | "number" | "days";
  text: string;
};
type Check =
  | {
      id: string;
      kind: "link";
      from: string;
      to: string;
      good: number;
      all?: boolean;
      text: string;
      fix?: string;
    }
  | {
      id: string;
      kind: "duplicate";
      of: string;
      by: string;
      good: number;
      text: string;
      fix?: string;
    }
  | { id: string; kind: "status"; of: string; sale: string[]; undo: string[] };
type Advice = {
  id: string;
  kind: "connect" | "change";
  area: string;
  missing?: string;
  low?: string;
  text: string;
  keywords: string[];
  note?: string;
};
export type Playbook = {
  id: string;
  title: string;
  version: number;
  match: string[];
  signals: { require: string[]; boost: string[]; min: number };
  roles: Record<string, Role>;
  areas: Area[];
  metrics: Record<string, Metric>;
  normal: Record<string, Normal>;
  checks: Check[];
  advice: Advice[];
  glossary: Record<string, string>;
  reading: string[];
  // How a number breaks down, top down, and what usually explains a change.
  drivers?: {
    id: string;
    title: string;
    equals: string;
    splits: string[];
    needs: string[];
    look: string;
  }[];
  intelligence?: { area: string; text: string }[];
};
type Mapped = { datasetId: string; column?: string };
type Mapping = Record<string, Mapped>;
type Profile = {
  description?: string;
  drafted?: { description: string; playbook: Playbook };
  draftRunId?: string;
  knowledge?: (BusinessKnowledge & { value?: number; at: string })[];
  hidden?: string[];
  // Per person: area -> decayed interest.
  interests?: Record<string, Record<string, { score: number; at: string }>>;
  // Per suggestion: questions that needed it.
  gaps?: Record<
    string,
    { count: number; examples: { q: string; at: string }[] }
  >;
  dismissed?: string[];
};

export const PROFILE_ID = "business_profile";
const DAY = 86400000;

// Playbooks ship as markdown with JSON frontmatter (valid YAML too), so code
// reads the same file people review.
let books: Playbook[] | undefined;
export function playbooks(): Playbook[] {
  if (books) return books;
  const dir = new URL("../../../docs/industries/", import.meta.url);
  books = readdirSync(dir)
    .filter((f) => f.endsWith(".md") && f !== "README.md")
    .sort()
    .map((f) => {
      const text = readFileSync(new URL(f, dir), "utf8");
      const m = /^---\n([\s\S]*?)\n---\n/.exec(text);
      if (!m) throw new Error(`docs/industries/${f} has no frontmatter`);
      return JSON.parse(m[1]) as Playbook;
    });
  return books;
}

const norm = (s: string) =>
  s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_+|_+$/g, "");
const singular = (w: string) =>
  w.endsWith("ies")
    ? w.slice(0, -3) + "y"
    : w.endsWith("sses")
      ? w.slice(0, -2)
      : w.endsWith("s") && !w.endsWith("ss")
        ? w.slice(0, -1)
        : w;
// "Shopify · order_items.csv" -> "order_item".
export const datasetKey = (d: Resource) => {
  const parts = norm(noun(d)).split("_");
  return [...parts.slice(0, -1), singular(parts.at(-1) || "")].join("_");
};
const temporal = (t: string) => /^(DATE|TIMESTAMP)/i.test(t);
const numeric = (t: string) =>
  /^(TINYINT|SMALLINT|INTEGER|BIGINT|HUGEINT|UTINYINT|USMALLINT|UINTEGER|UBIGINT|FLOAT|REAL|DOUBLE|DECIMAL|NUMERIC)/i.test(
    t,
  );
const fits = (type: Role["type"], t: string) =>
  !type || type === "any"
    ? true
    : type === "number"
      ? numeric(t)
      : type === "date"
        ? temporal(t) || /^VARCHAR/i.test(t)
        : /^(VARCHAR|BOOLEAN|ENUM)/i.test(t);
const columns = (
  d: Resource,
): { name: string; type: string; primaryKey?: boolean }[] =>
  d.data.profile?.columns || [];

// Each role onto the dataset (or column) that plays it. Merged datasets, which
// combine sources, win over their raw parts; then the larger one.
export function mapRoles(pb: Playbook, datasets: Resource[]): Mapping {
  const out: Mapping = {};
  const ordered = [...datasets].sort(
    (a, b) =>
      Number(!!b.data.generation) - Number(!!a.data.generation) ||
      (b.data.profile?.rows || 0) - (a.data.profile?.rows || 0),
  );
  for (const [id, role] of Object.entries(pb.roles)) {
    if (!role.entity) continue;
    const names = new Set(role.names.map((n) => singular(norm(n))));
    const d = ordered.find((d) => names.has(datasetKey(d)));
    if (d) out[id] = { datasetId: d.id };
  }
  const byId = new Map(datasets.map((d) => [d.id, d]));
  for (const [id, role] of Object.entries(pb.roles)) {
    if (role.entity) continue;
    for (const owner of role.of || []) {
      const d = out[owner] && byId.get(out[owner].datasetId);
      if (!d) continue;
      const cols = columns(d);
      const c = role.names
        .map((n) => cols.find((c) => norm(c.name) === norm(n)))
        .find((c) => c && fits(role.type, c.type));
      if (c) {
        out[id] = { datasetId: d.id, column: c.name };
        break;
      }
    }
  }
  return out;
}
const detected = (pb: Playbook, m: Mapping) =>
  pb.signals.require.every((r) => m[r]) &&
  pb.signals.boost.filter((r) => m[r]).length >= pb.signals.min;

// A word or phrase at a word start; "why+return" needs both.
const mentions = (text: string, keyword: string) =>
  keyword
    .toLowerCase()
    .split("+")
    .every((part) =>
      new RegExp(
        "\\b" + part.trim().replace(/[.*+?^${}()|[\]\\]/g, "\\$&"),
        "i",
      ).test(text),
    );
// "furniture retailer" names retail; "storage" doesn't name a store.
const names = (text: string, term: string) =>
  new RegExp(
    "\\b" + term.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + "(s|er|ers|ing)?\\b",
    "i",
  ).test(text);

export type Resolved = {
  playbook?: Playbook;
  source?: "inferred" | "described" | "drafted";
  mapping: Mapping;
  datasets: Resource[];
  profile: Profile;
  revision: number;
};
// The playbook for this workspace: the owner's description picks a built-in
// one or the playbook drafted for it; without a description, the data does.
export async function resolve(
  store: Store,
  scope: string,
  current?: Resource[],
): Promise<Resolved> {
  const [datasets, r] = await Promise.all([
    current || metricsDatasets(store, scope),
    store.get(scope, PROFILE_ID),
  ]);
  const profile: Profile = r?.data || {},
    description = (profile.description || "").trim();
  let playbook: Playbook | undefined, source: Resolved["source"];
  if (description) {
    playbook = playbooks().find((b) =>
      b.match.some((m) => names(description, m)),
    );
    if (playbook) source = "described";
    else if (profile.drafted?.description === description) {
      playbook = profile.drafted.playbook;
      source = "drafted";
    }
  } else
    for (const b of playbooks())
      if (detected(b, mapRoles(b, datasets))) {
        playbook = b;
        source = "inferred";
        break;
      }
  return {
    playbook,
    source,
    mapping: playbook ? mapRoles(playbook, datasets) : {},
    datasets,
    profile,
    revision: r?.revision || 0,
  };
}

// Changes Home's metrics signature when the industry does, so metrics are
// picked again for it; empty without one, keeping the old signature.
export const industryKey = (r: Resolved) =>
  r.playbook ? `${r.playbook.id}@${r.playbook.version}` : "";

const supported = (pb: Playbook, m: Mapping, metric: string) =>
  !!pb.metrics[metric] && pb.metrics[metric].needs.every((n) => m[n]);

// How the AI should pick Home's metrics for this industry: the playbook's
// metrics, in its order, under its titles, using only what the data has.
export function metricsGuide(r: Resolved) {
  const pb = r.playbook;
  if (!pb) return "";
  const wanted = pb.areas
    .flatMap((a) => a.metrics)
    .filter((id) => supported(pb, r.mapping, id))
    .map((id) => {
      const m = pb.metrics[id];
      return m.title + (m.hint ? ` (${m.hint})` : "");
    });
  if (!wanted.length) return "";
  const definitions =
    r.profile.knowledge?.filter((k) => k.kind === "definition") || [];
  return (
    ` This business is ${pb.title.toLowerCase()}${r.profile.description ? ` (${r.profile.description})` : ""}. Prefer these metrics, in this order, using these exact titles where the data supports them: ` +
    wanted.join("; ") +
    "." +
    (definitions.length
      ? " Business definitions: " +
        definitions.map((d) => d.text).join("; ") +
        "."
      : "")
  );
}

const pct = (n: number) =>
  `${Math.round(n * 1000) / 10}`.replace(/\.0$/, "") + "%";
const range = (n: Normal) =>
  n.unit === "percent"
    ? `${Math.round(n.low * 100)}–${Math.round(n.high * 100)}%`
    : n.unit === "days"
      ? `${n.low}–${n.high} days`
      : `${n.low}–${n.high}`;
const metricFor = (pb: Playbook, title: string) =>
  Object.entries(pb.metrics).find(
    ([, m]) => m.title.toLowerCase() === title.trim().toLowerCase(),
  )?.[0];
// What a Home metric is judged against: the owner's target, else the typical
// range for the industry.
export function referenceFor(r: Resolved, title: string) {
  const pb = r.playbook;
  if (!pb) return undefined;
  const id = metricFor(pb, title);
  if (!id) return undefined;
  const target = r.profile.knowledge?.find(
    (k) => k.kind === "target" && k.metric === id && k.value != null,
  );
  if (target)
    return `your target ${target.value! <= 1 && pb.normal[id]?.unit !== "days" ? pct(target.value!) : target.value}`;
  const n = pb.normal[id];
  if (n && !r.profile.hidden?.includes("typical:" + id))
    return `typical ${range(n)}`;
  return undefined;
}

// Knowledge: the owner's and learned entries, then the playbook's typical
// ranges the owner hasn't removed.
export function knowledgeOf(r: Resolved): BusinessKnowledge[] {
  const own = (r.profile.knowledge || []).map(
    ({ id, kind, text, source, metric }) => ({
      id,
      kind,
      text,
      source,
      metric,
    }),
  );
  const typical: BusinessKnowledge[] = r.playbook
    ? Object.entries(r.playbook.normal)
        .filter(([id]) => !r.profile.hidden?.includes("typical:" + id))
        .map(([id, n]) => ({
          id: "typical:" + id,
          kind: "normal" as const,
          text: n.text,
          source: "typical" as const,
          metric: id,
        }))
    : [];
  return [...own, ...typical];
}

// What the Analyst should know about the business: short, and only what
// changes how a question is read.
export function businessContext(r: Resolved) {
  const pb = r.playbook,
    knowledge = (r.profile.knowledge || []).map((k) => k.text);
  if (!pb && !r.profile.description && !knowledge.length) return undefined;
  return {
    industry: pb?.title,
    description: r.profile.description || undefined,
    howToRead: pb?.reading,
    glossary: pb?.glossary,
    // For "why" questions: break the number down level by level, with the
    // levels this data can't reach marked, so they're named, not guessed.
    drivers: pb?.drivers?.map((d) => ({
      level: d.title,
      equals: d.equals,
      lookFor: d.look,
      ...(d.needs.every((n) => r.mapping[n]) ? {} : { notInThisData: true }),
    })),
    industryRulesOfThumb: pb?.intelligence?.map((i) => i.text),
    knownAboutTheBusiness: knowledge.length ? knowledge : undefined,
  };
}

// Data checks, computed with bounded aggregate queries.
type CheckResult = BusinessInsights["checks"][number];
const keyOf = (d: Resource, role: string, pb: Playbook) => {
  const cols = columns(d);
  return (
    cols.find((c) => c.primaryKey) ||
    cols.find((c) => norm(c.name) === `${datasetKey(d)}_id`) ||
    cols.find((c) =>
      pb.roles[role].names.some(
        (n) => norm(c.name) === `${singular(norm(n))}_id`,
      ),
    ) ||
    cols.find((c) => norm(c.name) === "id")
  )?.name;
};
const foreignKey = (
  from: Resource,
  to: Resource,
  key: string,
  role: string,
  pb: Playbook,
) => {
  const cols = columns(from);
  const wanted = new Set(
    [
      norm(key) === "id" ? "" : norm(key),
      `${datasetKey(to)}_id`,
      ...pb.roles[role].names.map((n) => `${singular(norm(n))}_id`),
    ].filter(Boolean),
  );
  const own =
    cols.find((c) => c.primaryKey)?.name ??
    cols.find((c) => norm(c.name) === `${datasetKey(from)}_id`)?.name;
  return cols.find((c) => wanted.has(norm(c.name)) && c.name !== own)?.name;
};
const count = (v: unknown) => Number(v ?? 0) || 0;
async function runChecks(
  store: Store,
  scope: string,
  r: Resolved,
): Promise<CheckResult[]> {
  const pb = r.playbook!,
    byId = new Map(r.datasets.map((d) => [d.id, d])),
    out: CheckResult[] = [];
  for (const c of pb.checks) {
    try {
      if (c.kind === "link") {
        const a = r.mapping[c.from] && byId.get(r.mapping[c.from].datasetId),
          b = r.mapping[c.to] && byId.get(r.mapping[c.to].datasetId);
        if (!a || !b || a.id === b.id) continue;
        const key = keyOf(b, c.to, pb),
          fk = key && foreignKey(a, b, key, c.to, pb);
        if (!key || !fk) continue;
        const sql = `SELECT count(*) AS "total", count(b."__k") AS "matched" FROM ${q(datasetTable(a.id))} a LEFT JOIN (SELECT DISTINCT CAST(${q(key)} AS VARCHAR) AS "__k" FROM ${q(datasetTable(b.id))} WHERE ${q(key)} IS NOT NULL) b ON CAST(a.${q(fk)} AS VARCHAR) = b."__k"${c.all ? "" : ` WHERE a.${q(fk)} IS NOT NULL`}`;
        const row = (
          await queryDatasets(store, scope, [a.id, b.id], sql, { limit: 1 })
        ).rows[0];
        const total = count(row?.total);
        if (!total) continue;
        const rate = count(row.matched) / total,
          ok = rate >= c.good;
        out.push({
          id: c.id,
          kind: "link",
          text: `${c.text}: ${pct(rate)}.` + (!ok && c.fix ? " " + c.fix : ""),
          rate,
          ok,
        });
      } else if (c.kind === "duplicate") {
        const m = r.mapping[c.by];
        if (!m?.column) continue;
        const sql = `SELECT count(*) AS "total", count(DISTINCT lower(trim(CAST(${q(m.column)} AS VARCHAR)))) AS "unique" FROM ${q(datasetTable(m.datasetId))} WHERE ${q(m.column)} IS NOT NULL AND trim(CAST(${q(m.column)} AS VARCHAR)) <> ''`;
        const row = (
          await queryDatasets(store, scope, [m.datasetId], sql, { limit: 1 })
        ).rows[0];
        const total = count(row?.total);
        if (!total) continue;
        const rate = count(row.unique) / total,
          ok = rate >= c.good;
        out.push({
          id: c.id,
          kind: "duplicate",
          text: `${c.text}: ${pct(rate)}.` + (!ok && c.fix ? " " + c.fix : ""),
          rate,
          ok,
        });
      } else if (c.kind === "status") {
        const m = r.mapping[c.of];
        if (!m?.column) continue;
        const sql = `SELECT DISTINCT lower(CAST(${q(m.column)} AS VARCHAR)) AS "v" FROM ${q(datasetTable(m.datasetId))} WHERE ${q(m.column)} IS NOT NULL LIMIT 50`;
        const values = (
          await queryDatasets(store, scope, [m.datasetId], sql, { limit: 50 })
        ).rows.map((x: any) => String(x.v));
        const undo = values.filter((v) => c.undo.includes(v));
        if (!undo.length || !values.some((v) => c.sale.includes(v))) continue;
        const list =
          undo.length === 1
            ? undo[0]
            : undo.slice(0, -1).join(", ") + " and " + undo.at(-1);
        out.push({
          id: c.id,
          kind: "definition",
          text: `${list.charAt(0).toUpperCase() + list.slice(1)} orders are counted apart from sales.`,
          ok: true,
        });
      }
    } catch {
      // A check that can't run says nothing; it never breaks the page.
    }
  }
  return out;
}

const holds = (a: Advice, m: Mapping, checks: CheckResult[]) =>
  a.missing
    ? !m[a.missing]
    : a.low
      ? checks.some((c) => c.id === a.low && !c.ok)
      : false;

// Interest decays by half every 30 days, so the profile follows what the
// business cares about now.
const decayed = (
  v: { score: number; at: string } | undefined,
  at = Date.now(),
) => (v ? v.score * 0.5 ** ((at - Date.parse(v.at)) / (30 * DAY)) : 0);

type Computed = { key: string; checks: CheckResult[] };
const computed = new Map<string, Computed>();
const inflight = new Map<string, Promise<Computed>>();
async function checksFor(store: Store, scope: string, r: Resolved) {
  const key = JSON.stringify([
    industryKey(r),
    r.datasets.map((d) => [d.id, d.data.activeVersion]),
  ]);
  const hit = computed.get(scope);
  if (hit?.key === key) return hit.checks;
  const k = scope + key;
  if (!inflight.has(k))
    inflight.set(
      k,
      runChecks(store, scope, r)
        .then((checks) => {
          const c = { key, checks };
          computed.set(scope, c);
          return c;
        })
        .finally(() => inflight.delete(k)),
    );
  return (await inflight.get(k)!).checks;
}

// Everything the Ontology page, Home and Settings show about the business.
export async function insights(
  store: Store,
  scope: string,
  userId: string,
): Promise<BusinessInsights> {
  const r = await resolve(store, scope),
    pb = r.playbook,
    description = r.profile.description || "",
    draftRun =
      r.profile.draftRunId && (await store.get(scope, r.profile.draftRunId)),
    drafting =
      !!draftRun && ["queued", "running"].includes(draftRun.data.status);
  if (!pb)
    return {
      industry: null,
      description,
      drafting,
      areas: [],
      checks: [],
      suggestions: [],
      knowledge: knowledgeOf(r),
    };
  const checks = await checksFor(store, scope, r);
  const interests = r.profile.interests?.[userId] || {},
    gaps = r.profile.gaps || {},
    dismissed = new Set(r.profile.dismissed || []),
    order = new Map(pb.areas.map((a, i) => [a.id, i]));
  const suggestions = pb.advice
    .map((a, i) => ({ a, i }))
    .filter(({ a }) => !dismissed.has(a.id) && holds(a, r.mapping, checks))
    .map(({ a, i }) => ({
      a,
      score:
        (gaps[a.id]?.count || 0) * 20 +
        decayed(interests[a.area]) * 10 -
        (order.get(a.area) ?? 9) -
        i / 100,
    }))
    .sort((x, y) => y.score - x.score)
    .map(({ a }) => ({
      id: a.id,
      kind: a.kind,
      area: a.area,
      text: a.text,
      ...(gaps[a.id]?.count ? { asked: gaps[a.id].count } : {}),
    }));
  return {
    industry: { id: pb.id, title: pb.title, source: r.source! },
    description,
    drafting,
    areas: pb.areas.map((a) => {
      const metrics = a.metrics
        .filter((id) => pb.metrics[id])
        .map((id) => ({
          id,
          title: pb.metrics[id].title,
          supported: supported(pb, r.mapping, id),
        }));
      const n = metrics.filter((m) => m.supported).length;
      return {
        id: a.id,
        title: a.title,
        status: n === metrics.length ? "ready" : n ? "partial" : "missing",
        metrics,
      };
    }),
    checks,
    suggestions,
    knowledge: knowledgeOf(r),
  };
}

const profileUpdate = (
  store: Store,
  scope: string,
  fn: (p: Profile) => Profile,
) =>
  transaction(store, scope, async (tx) => {
    const r = await tx.get(PROFILE_ID),
      data = fn(structuredClone(r?.data || {}));
    return r
      ? tx.update(r, data)
      : tx.put(
          resource(
            scope,
            "businessProfile",
            "Business profile",
            data,
            PROFILE_ID,
          ),
        );
  });

// A question teaches the profile what this person cares about, and which
// missing data keeps getting in the way. Returns a one-line note for the
// answer when missing data limited it.
export async function noteQuestion(
  store: Store,
  scope: string,
  userId: string,
  question: string,
) {
  const r = await resolve(store, scope),
    pb = r.playbook;
  if (!pb) return undefined;
  const checks = computed.get(scope)?.checks || [];
  const areas = pb.areas.filter((a) =>
    a.keywords.some((k) => mentions(question, k)),
  );
  const hits = pb.advice.filter(
    (a) =>
      a.keywords.some((k) => mentions(question, k)) &&
      holds(a, r.mapping, checks),
  );
  if (!areas.length && !hits.length) return undefined;
  const at = new Date().toISOString();
  await profileUpdate(store, scope, (p) => {
    const mine = { ...(p.interests?.[userId] || {}) };
    for (const a of areas) mine[a.id] = { score: decayed(mine[a.id]) + 1, at };
    const gaps = { ...(p.gaps || {}) };
    for (const a of hits) {
      const g = gaps[a.id] || { count: 0, examples: [] };
      gaps[a.id] = {
        count: g.count + 1,
        // Short evidence, kept 90 days.
        examples: [{ q: question.slice(0, 140), at }, ...g.examples]
          .filter((e) => Date.now() - Date.parse(e.at) < 90 * DAY)
          .slice(0, 3),
      };
    }
    return {
      ...p,
      interests: { ...(p.interests || {}), [userId]: mine },
      gaps,
    };
  });
  return hits.find((a) => a.note)?.note;
}

// "Our target return rate is 8%", "remember that we pause ads in August": a
// statement about the business, not a question.
export function knowledgeStatement(text: string) {
  const t = text.trim();
  if (!t || t.endsWith("?") || t.length > 300) return undefined;
  const m =
    /^(?:please\s+)?(?:remember|note|fyi|for context)\b[:,]?\s*(?:that\s+)?(.+)$/i.exec(
      t,
    );
  if (m) return m[1].trim();
  if (
    /\bour\s+(?:normal|usual|typical|target|goal)\b|\b(?:target|goal)\s+(?:is|should be)\b/i.test(
      t,
    )
  )
    return t;
  return undefined;
}
// Reads the kind, the metric and a number from a sentence.
export function parseKnowledge(text: string, pb?: Playbook) {
  const t = text.toLowerCase();
  const kind: BusinessKnowledge["kind"] = /\b(target|goal|aim)\b/.test(t)
    ? "target"
    : /\b(normal|typical|usual|usually)\b/.test(t)
      ? "normal"
      : /\b(season|black friday|christmas|holiday|peak|every (jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)|summer|winter)/.test(
            t,
          )
        ? "seasonality"
        : /\b(aren't|isn't|is not|are not|count as|counts as|don't count|doesn't count|means)\b/.test(
              t,
            )
          ? "definition"
          : "fact";
  const metric = pb
    ? Object.entries(pb.metrics).find(([id, m]) =>
        [m.title, id.replace(/_/g, " ")].some((n) =>
          t.includes(n.toLowerCase()),
        ),
      )?.[0]
    : undefined;
  const n = /(-?\d+(?:\.\d+)?)\s*(%|percent|days?)?/.exec(t);
  const value = n
    ? n[2]?.startsWith("%") || n[2] === "percent"
      ? Number(n[1]) / 100
      : Number(n[1])
    : undefined;
  return { kind, metric, value };
}
export async function addKnowledge(store: Store, scope: string, text: string) {
  const r = await resolve(store, scope),
    clean = text.trim().replace(/\s+/g, " ").slice(0, 300),
    parsed = parseKnowledge(clean, r.playbook),
    entry = {
      id: "k_" + digest(clean + Date.now()).slice(0, 12),
      text: clean.charAt(0).toUpperCase() + clean.slice(1),
      source: "you" as const,
      at: new Date().toISOString(),
      ...parsed,
    };
  await profileUpdate(store, scope, (p) => ({
    ...p,
    knowledge: [...(p.knowledge || []), entry],
  }));
  return entry;
}
export async function removeKnowledge(store: Store, scope: string, id: string) {
  await profileUpdate(store, scope, (p) =>
    id.startsWith("typical:")
      ? { ...p, hidden: [...new Set([...(p.hidden || []), id])] }
      : { ...p, knowledge: (p.knowledge || []).filter((k) => k.id !== id) },
  );
}
export async function dismissSuggestion(
  store: Store,
  scope: string,
  id: string,
) {
  await profileUpdate(store, scope, (p) => ({
    ...p,
    dismissed: [...new Set([...(p.dismissed || []), id])],
  }));
}

// The owner's words for their business. A built-in playbook that fits is used
// at once; otherwise, with AI ready, one is drafted for this workspace.
export async function describeBusiness(
  store: Store,
  scope: string,
  description: string,
  actor: string,
) {
  const text = description.trim().replace(/\s+/g, " ").slice(0, 200);
  const builtIn = playbooks().some((b) => b.match.some((m) => names(text, m)));
  const current = (await store.get(scope, PROFILE_ID))?.data as
    | Profile
    | undefined;
  const draft =
    !!text &&
    !builtIn &&
    current?.drafted?.description !== text &&
    (await aiReady(store, scope));
  const runId = draft
    ? "playbook_" +
      digest(
        JSON.stringify([
          text,
          (await metricsDatasets(store, scope)).map((d) => d.id),
        ]),
      )
    : undefined;
  await transaction(store, scope, async (tx) => {
    const r = await tx.get(PROFILE_ID),
      data: Profile = {
        ...(r?.data || {}),
        description: text || undefined,
        ...(runId ? { draftRunId: runId } : {}),
      };
    if (r) tx.update(r, data);
    else
      tx.put(
        resource(
          scope,
          "businessProfile",
          "Business profile",
          data,
          PROFILE_ID,
        ),
      );
    if (runId && !(await tx.get(runId)))
      tx.put(
        resource(
          scope,
          "run",
          "Learn about this business",
          {
            goal: text,
            intent: "build",
            task: "draft_playbook",
            background: true,
            authority: { userId: actor, readOnly: false },
            status: "queued",
            stage: "queued",
            events: [],
            calls: 0,
            tokens: 0,
          },
          runId,
        ),
      );
  });
}

// What the AI writes for an industry with no built-in playbook. Plain
// required fields, so small local models can fill them.
const slug = z.string().regex(/^[a-z][a-z0-9_]{0,40}$/);
export const DraftSchema = z.object({
  title: z.string().max(60),
  roles: z
    .array(
      z.object({
        id: slug,
        title: z.string().max(60),
        entity: z.boolean(),
        of: z.string(),
        type: z.enum(["number", "date", "category", "text", "any"]),
        names: z.array(z.string().max(40)).min(1).max(8),
      }),
    )
    .min(2)
    .max(20),
  areas: z
    .array(
      z.object({
        id: slug,
        title: z.string().max(40),
        keywords: z.array(z.string().max(30)).max(10),
        metrics: z
          .array(
            z.object({
              id: slug,
              title: z.string().max(40),
              needs: z.array(z.string()).min(1).max(4),
              hint: z.string().max(200),
            }),
          )
          .min(1)
          .max(5),
      }),
    )
    .min(2)
    .max(6),
  advice: z
    .array(
      z.object({
        id: slug,
        kind: z.enum(["connect", "change"]),
        area: z.string(),
        missing: z.string(),
        text: z.string().max(200),
        keywords: z.array(z.string().max(30)).max(8),
      }),
    )
    .max(10),
  normal: z
    .array(
      z.object({
        metric: z.string(),
        low: z.number(),
        high: z.number(),
        unit: z.enum(["percent", "number", "days"]),
        text: z.string().max(120),
      }),
    )
    .max(8),
});
export const DRAFT_PROMPT =
  "The goal is the owner's description of their business. Write an opinionated playbook for reading this kind of business's data, like an experienced analyst in that industry would. roles: the business concepts that matter (entities such as orders, bookings, patients, shipments, and fields such as amount or date). An entity role has entity true, of empty and type any; a field role has entity false, of = the id of its entity role, and a type. names: likely table or column names for the role, snake_case. Include roles the data doesn't have yet when they matter for this industry: missing ones become advice. areas: 3 to 5 business areas in priority order (for example revenue, customers, operations, cost), each with keywords people use when asking about it and 1 to 4 metrics; a metric's needs lists the role ids it needs; hint says how to compute it. advice: up to 6 concrete things the owner could connect (kind connect) or change in their own systems (kind change) to understand their customers better, each for the role id it would add (missing), in one plain sentence. normal: typical ranges for a few metrics in this industry, as indicative rules of thumb; percentages as fractions (0.02 for 2%); text says it plainly, starting with the metric. Use the datasets only to see what exists; never copy values.";

// Keeps only what holds together: roles with real owners, metrics whose
// needs exist, advice for real roles and areas, sane ranges.
export function fromDraft(d: z.infer<typeof DraftSchema>): Playbook {
  const roles: Record<string, Role> = {};
  for (const r of d.roles)
    if (r.entity && !roles[r.id])
      roles[r.id] = {
        title: r.title,
        entity: true,
        names: r.names,
        type: "any",
      };
  for (const r of d.roles)
    if (!r.entity && !roles[r.id] && roles[r.of]?.entity)
      roles[r.id] = {
        title: r.title,
        of: [r.of],
        type: r.type,
        names: r.names,
      };
  const metrics: Record<string, Metric> = {},
    areas: Area[] = [];
  for (const a of d.areas) {
    const ids: string[] = [];
    for (const m of a.metrics) {
      const needs = m.needs.filter((n) => roles[n]);
      if (!needs.length || metrics[m.id]) continue;
      metrics[m.id] = { title: m.title, needs, hint: m.hint || undefined };
      ids.push(m.id);
    }
    if (ids.length && !areas.some((x) => x.id === a.id))
      areas.push({
        id: a.id,
        title: a.title,
        keywords: a.keywords,
        metrics: ids,
      });
  }
  const areaIds = new Set(areas.map((a) => a.id));
  const advice: Advice[] = d.advice
    .filter((a) => roles[a.missing] && areaIds.has(a.area))
    .map((a) => ({
      id: a.id,
      kind: a.kind,
      area: a.area,
      missing: a.missing,
      text: a.text,
      keywords: a.keywords,
    }));
  const normal: Record<string, Normal> = {};
  for (const n of d.normal) {
    if (!metrics[n.metric] || !(n.low <= n.high)) continue;
    const scale = n.unit === "percent" && n.high > 1 ? 100 : 1;
    if (n.unit === "percent" && n.high / scale > 1) continue;
    normal[n.metric] = {
      low: n.low / scale,
      high: n.high / scale,
      unit: n.unit,
      text: n.text,
    };
  }
  const entities = Object.keys(roles).filter((id) => roles[id].entity);
  return {
    id: "drafted",
    title: d.title,
    version: 1,
    match: [],
    // Drafted for this workspace on request, so no detection threshold.
    signals: { require: [], boost: entities, min: 0 },
    roles,
    areas,
    metrics,
    normal,
    checks: [],
    advice,
    glossary: {},
    reading: [],
  };
}
export async function saveDraft(
  store: Store,
  scope: string,
  description: string,
  playbook: Playbook,
) {
  let saved = false;
  await profileUpdate(store, scope, (p) => {
    saved = (p.description || "") === description;
    return saved ? { ...p, drafted: { description, playbook } } : p;
  });
  return saved;
}
