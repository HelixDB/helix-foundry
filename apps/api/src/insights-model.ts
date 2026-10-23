// Home insights, part 1: the business model. From the accepted ontology (the
// published definitions, plus the activity streams the owner confirmed in
// setup, which publication leaves out of the definitions) and the datasets'
// profiles, it finds activities (records with an event time) and what each
// one supports: participants, money measures, categories and recorded links
// to a later outcome. Labels only propose; the data decides later whether a
// check is meaningful. No query runs here.
import type {
  Bundle,
  InsightCheck,
  InsightDriver,
  InsightDriverKind,
  Resource,
  SuggestedOntology,
} from "../../../packages/shared/src/index.js";
import { entityColumn, plural } from "./discovery.js";
import { nested, numericType, temporal } from "./metrics.js";

export type Column = {
  name: string;
  type: string;
  nulls?: number;
  distinct?: number;
  sensitive?: boolean;
  primaryKey?: boolean;
};
type Entity = {
  id: string;
  name: string;
  kind: "record" | "activity";
  dataset: Resource;
  key?: string;
};
type Link = { from: Entity; to: Entity; fromColumn: string; toColumn: string };

export type Participant = {
  entity: string;
  // The participant type's own dataset (for its size).
  datasetId: string;
  // actor: who acts (a customer, account, patient); contributor: what the
  // activity is about (a product, a subscription).
  role: "actor" | "contributor";
  // The activity's column holding the participant's key, or, through `via`,
  // the key of a record that holds it (invoice -> subscription -> customer).
  column: string;
  via?: { datasetId: string; key: string; column: string; entity: string };
};
export type Measure = {
  column: string;
  scale: "none" | "cents";
  currency?: "USD" | "EUR" | "GBP";
  // up: more is better (revenue); down: less is better (refunds, cost).
  direction: "up" | "down";
};
export type Dimension = {
  column: string;
  label: string;
  role: "source" | "segment";
};
// A recorded step from an activity to a later outcome: an upstream row is
// converted when a downstream row links to it, when it links to a downstream
// row, or when its boolean flag says so.
export type Funnel = {
  upstream: string;
  downstream: string;
  link:
    | { side: "downstream"; column: string; key: string }
    | { side: "upstream"; column: string; key: string }
    | { side: "flag"; column: string };
};
export type Activity = {
  id: string;
  name: string;
  one: string;
  many: string;
  datasetId: string;
  datasetName: string;
  key?: string;
  time: { column: string; zoned: boolean };
  rows: number;
  // Whether more of the activity reads as good (orders), bad (tickets) or
  // can't be told.
  direction: "up" | "down" | "unknown";
  participants: Participant[];
  measures: Measure[];
  dimensions: Dimension[];
  funnels: Funnel[];
};
export type Driver = {
  id: string;
  kind: InsightDriverKind;
  label: string;
  activity: Activity;
  measure?: Measure;
  actor?: Participant;
  contributor?: Participant;
  dimensions: Dimension[];
  funnel?: Funnel;
  // From metadata alone; the scan multiplies in what the data supports.
  prior: number;
  reasons: string[];
  checks: Record<InsightCheck, string | null>;
};

export const columnsOf = (d: Resource): Column[] =>
  d.data.profile?.columns || [];
const rowsOf = (d: Resource): number => d.data.profile?.rows || 0;
const words = (s: string) =>
  s
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .toLowerCase()
    .split(/[^a-z0-9]+/)
    .filter(Boolean);
// "Order" -> "order"; acronyms keep their case ("SSO connection").
export const lowerName = (s: string) =>
  /^[A-Z]{2}/.test(s) ? s : s.charAt(0).toLowerCase() + s.slice(1);
export const capital = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
export const humanize = (column: string) => column.replace(/[_.]+/g, " ");

// Who acts, by the ontology's own label.
const ACTOR =
  /^(customers?|clients?|accounts?|users?|members?|buyers?|shoppers?|patients?|students?|learners?|subscribers?|organi[sz]ations?|compan(y|ies)|tenants?|merchants?|sellers?|suppliers?|vendors?|partners?|contacts?|persons?|people|visitors?|guests?|leads?|donors?|employees?|drivers?|riders?|players?|readers?|viewers?|households?|families|famil(y)|teams?|workspaces?)$/;
const isActor = (name: string) => words(name).some((w) => ACTOR.test(w));
const EXPERIENCE =
  /^(tickets?|cases?|complaints?|incidents?|issues?|reviews?|feedbacks?|surveys?|nps|csat|ratings?|disputes?|chargebacks?)$/;
const DOWN =
  /^(tickets?|cases?|complaints?|incidents?|issues?|bugs?|errors?|outages?|refunds?|returns?|disputes?|chargebacks?|cancell?ations?|churns?|failures?|defects?|escalations?|noshows?)$/;
const UP =
  /^(orders?|sales?|purchases?|sessions?|visits?|pageviews?|views?|events?|signups?|registrations?|bookings?|appointments?|subscriptions?|payments?|invoices?|transactions?|enrol+ments?|lessons?|shipments?|deliveries|delivery|trips?|rides?|plays?|streams?|downloads?|installs?|logins?|messages?|donations?|applications?|deals?|carts?|checkouts?)$/;
const volumeDirection = (name: string): Activity["direction"] =>
  words(name).some((w) => DOWN.test(w))
    ? "down"
    : words(name).some((w) => UP.test(w))
      ? "up"
      : "unknown";
const SOURCE =
  /(^|_)(source|channel|medium|utm|referr\w*|campaign|platform|device|origin|acquisition|landing)(_|$)/i;
const STATE = /(^|_)(status|state|stage|outcome|result)(_|$)/i;
const KEYISH = /(^|_)(id|key|code|uuid|number|no)$/i;
const MONEY: [RegExp, Measure["direction"]][] = [
  [
    /^(spend|costs?|expenses?|refunds?|discount)(_(usd|eur|gbp|amount|total|cents))?$/i,
    "down",
  ],
  [
    /^(net|net_amount|net_revenue|net_sales|revenue|amount_paid|paid_amount|gmv|sales|total_paid)$/i,
    "up",
  ],
  [
    /^(amount|total|total_amount|order_total|subtotal|value|order_value|grand_total|price|charge|fee|fees|mrr|arr)$/i,
    "up",
  ],
  [/(_amount|_total|_revenue|_value|_usd|_eur|_gbp|_cents|_price)$/i, "up"],
];
const BOOLEAN_OUTCOME =
  /^(is_|has_|was_)?(converted|purchased|ordered|won|activated|subscribed|retained|completed|booked)$/i;

// The column saying when the activity happened: an event time first, then a
// start or creation time; never an update, end or cancellation time.
export function eventTime(cols: Column[], rows: number) {
  const score = (name: string) =>
    /(^|_)(occurred|event_?time|event_?date|timestamp|ts)(_|$)/i.test(name)
      ? 5
      : /(order|purchas|placed|paid|book|visit|start|open|submit|receiv|schedul|sent|issued)/i.test(
            name,
          )
        ? 4
        : /(creat|sign.?up|join|regist|enrol)/i.test(name)
          ? 3
          : /(date|time|day|_at$|_on$)/i.test(name)
            ? 2
            : 1;
  return cols
    .filter(
      (c) =>
        temporal(c.type) &&
        !/(updat|modif|delet|(^|_)end|ended|cancel|expir|closed|resolv|due|until|(^|_)last|refund|birth|dob)/i.test(
          c.name,
        ) &&
        (c.nulls || 0) <= rows / 2,
    )
    .map((c) => ({ c, s: score(c.name) }))
    .sort((a, b) => b.s - a.s || (a.c.nulls || 0) - (b.c.nulls || 0))[0]?.c;
}

// Business context the owner wrote in setup, not the goal setup generates.
export function businessContext(workspace: Resource | undefined) {
  const goal: string = workspace?.data.onboarding?.goal || "";
  return /^Give an overview of /.test(goal) ? "" : goal;
}

// The published definitions of the active generation.
export async function publishedBundle(
  list: (kind: string) => Promise<Resource[]>,
  generation: string,
): Promise<Bundle | undefined> {
  return (await list("proposal")).find(
    (p) => p.data.publishedGeneration === generation,
  )?.data.bundle;
}

// Entities and links of the accepted ontology over the current datasets.
export function acceptedOntology(
  workspace: Resource,
  bundle: Bundle | undefined,
  datasets: Resource[],
) {
  const byId = new Map(datasets.map((d) => [d.id, d]));
  const has = (d: Resource, column: string) =>
    columnsOf(d).some((c) => c.name === column);
  const entities: Entity[] = [],
    types = new Map<string, Entity>();
  for (const t of bundle?.ontology || []) {
    const d = byId.get(t.datasetId);
    if (!d || types.has(t.name.toLowerCase())) continue;
    const e: Entity = {
      id: "type:" + t.name,
      name: t.name,
      kind: "record",
      dataset: d,
      key: t.primaryKey,
    };
    entities.push(e);
    types.set(t.name.toLowerCase(), e);
  }
  const confirmed: SuggestedOntology | undefined =
    workspace.data.onboarding?.ontology?.value;
  const streams = new Map<string, Entity>();
  for (const e of confirmed?.entities || []) {
    const d = byId.get(e.anchorDatasetId);
    if (e.kind !== "activity" || !d || types.has(e.name.toLowerCase()))
      continue;
    const x: Entity = {
      id: "stream:" + e.id,
      name: e.name,
      kind: "activity",
      dataset: d,
      key: e.key,
    };
    entities.push(x);
    streams.set(e.id, x);
  }
  const links: Link[] = [];
  for (const r of bundle?.relationships || []) {
    const from = types.get(r.fromType.toLowerCase()),
      to = types.get(r.toType.toLowerCase());
    if (
      from &&
      to &&
      from !== to &&
      has(from.dataset, r.fromColumn) &&
      has(to.dataset, r.toColumn)
    )
      links.push({ from, to, fromColumn: r.fromColumn, toColumn: r.toColumn });
  }
  // A stream's links name source datasets; a record side may since have been
  // merged into a pipeline output, where the column can carry a prefix.
  const confirmedById = new Map(
    (confirmed?.entities || []).map((e) => [e.id, e]),
  );
  const side = (entityId: string, datasetId: string, column: string) => {
    const stream = streams.get(entityId);
    if (stream)
      return stream.dataset.id === datasetId && has(stream.dataset, column)
        ? { entity: stream, column }
        : undefined;
    const c = confirmedById.get(entityId),
      t = c && types.get(c.name.toLowerCase());
    if (!c || !t) return undefined;
    const mapped =
      t.dataset.id === datasetId
        ? column
        : entityColumn(c, byId, datasetId, column);
    return mapped && has(t.dataset, mapped)
      ? { entity: t, column: mapped }
      : undefined;
  };
  for (const l of confirmed?.links || []) {
    if (!streams.has(l.from) && !streams.has(l.to)) continue;
    const a = side(l.from, l.fromDatasetId, l.fromColumn),
      b = side(l.to, l.toDatasetId, l.toColumn);
    if (a && b && a.entity !== b.entity)
      links.push({
        from: a.entity,
        to: b.entity,
        fromColumn: a.column,
        toColumn: b.column,
      });
  }
  return { entities, links };
}

const isCents = (d: Resource, column: string) =>
  /_cents$/i.test(column) ||
  ((/^stripe\b/i.test(d.name) || d.data.sampleSource === "stripe") &&
    /^amount/i.test(column)) ||
  /^stripe_.*amount/i.test(column);

// Activities of the ontology and what each supports.
export function findActivities(
  onto: ReturnType<typeof acceptedOntology>,
): Activity[] {
  const out: Activity[] = [];
  const small = (e: Entity, column?: Column) =>
    rowsOf(e.dataset) <= 12 || (column?.distinct ?? 99) <= 12;
  for (const e of onto.entities) {
    const rows = rowsOf(e.dataset),
      columns = columnsOf(e.dataset),
      col = (name: string) => columns.find((c) => c.name === name);
    const time = eventTime(columns, rows);
    if (!time || rows < 2) continue;
    const outgoing = onto.links.filter((l) => l.from === e),
      linked = new Set(
        onto.links.flatMap((l) => [
          ...(l.from === e ? [l.fromColumn] : []),
          ...(l.to === e ? [l.toColumn] : []),
        ]),
      );
    const participants: Participant[] = [],
      dimensions: Dimension[] = [];
    for (const l of outgoing) {
      const c = col(l.fromColumn);
      if (small(l.to, c)) {
        if (!c?.sensitive && !dimensions.some((d) => d.column === l.fromColumn))
          dimensions.push({
            column: l.fromColumn,
            label: l.to.name,
            role:
              SOURCE.test(l.fromColumn) || SOURCE.test(l.to.name)
                ? "source"
                : "segment",
          });
        continue;
      }
      participants.push({
        entity: l.to.name,
        datasetId: l.to.dataset.id,
        role: isActor(l.to.name) ? "actor" : "contributor",
        column: l.fromColumn,
      });
    }
    // An actor two hops away, when none links directly.
    if (!participants.some((p) => p.role === "actor"))
      for (const l of outgoing)
        for (const l2 of onto.links)
          if (
            l2.from === l.to &&
            l2.to !== e &&
            isActor(l2.to.name) &&
            !small(l2.to)
          )
            participants.push({
              entity: l2.to.name,
              datasetId: l2.to.dataset.id,
              role: "actor",
              column: l.fromColumn,
              via: {
                datasetId: l.to.dataset.id,
                key: l.toColumn,
                column: l2.fromColumn,
                entity: l.to.name,
              },
            });
    participants.sort(
      (a, b) =>
        Number(b.role === "actor") - Number(a.role === "actor") ||
        Number(!!a.via) - Number(!!b.via),
    );
    for (const c of columns)
      if (
        /^(VARCHAR|ENUM)/i.test(c.type) &&
        !nested(c.type) &&
        !c.sensitive &&
        !c.primaryKey &&
        !linked.has(c.name) &&
        !KEYISH.test(c.name) &&
        !STATE.test(c.name) &&
        (c.distinct ?? 99) >= 2 &&
        (c.distinct ?? 99) <= 12 &&
        (c.nulls || 0) <= rows / 2
      )
        dimensions.push({
          column: c.name,
          label: capital(humanize(c.name)),
          role: SOURCE.test(c.name) ? "source" : "segment",
        });
    dimensions.sort(
      (a, b) => Number(b.role === "source") - Number(a.role === "source"),
    );
    const measures: (Measure & { rank: number })[] = [];
    for (const c of columns) {
      if (
        !numericType(c.type) ||
        c.sensitive ||
        c.primaryKey ||
        linked.has(c.name) ||
        KEYISH.test(c.name) ||
        /(qty|quantity|count|number|rank|score|age|year|lat|lon|percent|pct|rate)/i.test(
          c.name,
        )
      )
        continue;
      const rank = MONEY.findIndex(([re]) => re.test(c.name));
      if (rank >= 0)
        measures.push({
          column: c.name,
          scale: isCents(e.dataset, c.name) ? "cents" : "none",
          currency: /_(usd|eur|gbp)(_|$)/i
            .exec(c.name)?.[1]
            .toUpperCase() as Measure["currency"],
          direction: MONEY[rank][1],
          rank,
        });
    }
    // Matching cost names first decides their direction, not whether they
    // outrank an independently supported revenue measure on the same activity.
    measures.sort(
      (a, b) =>
        Number(b.direction === "up") - Number(a.direction === "up") ||
        a.rank - b.rank,
    );
    const zoned = /WITH TIME ZONE|TIMESTAMPTZ/i.test(time.type);
    const qualifies =
      e.kind === "activity" ||
      participants.length > 0 ||
      dimensions.some((d) => d.role === "source");
    if (!qualifies) continue;
    const one = lowerName(e.name);
    out.push({
      id: e.id,
      name: e.name,
      one,
      many: plural(one),
      datasetId: e.dataset.id,
      datasetName: e.dataset.name,
      key: e.key && col(e.key) ? e.key : undefined,
      time: { column: time.name, zoned },
      rows,
      direction: volumeDirection(e.name + " " + e.dataset.name),
      participants,
      measures: measures.map(({ rank: _, ...m }) => m),
      dimensions: dimensions.slice(0, 3),
      funnels: [],
    });
  }
  // Recorded steps between two activities, such as an order naming the
  // session it came from. The funnel narrows, so the side with clearly more
  // rows is upstream. A participant is never a step, and the link must look
  // like one conversion per row: unique on the side that records it and, when
  // the upstream row records it, mostly empty (only converted rows carry it).
  // Anything else (events naming their user) is a participant link.
  const byEntity = new Map(out.map((a) => [a.id, a]));
  const profile = (e: Entity, column: string) =>
    columnsOf(e.dataset).find((c) => c.name === column);
  const unique = (c: Column | undefined, rows: number) =>
    !!c && (c.distinct ?? 0) >= 0.8 * Math.max(1, rows - (c.nulls || 0));
  for (const l of onto.links) {
    const a = byEntity.get(l.from.id),
      b = byEntity.get(l.to.id);
    if (!a || !b || l.toColumn !== b.key) continue;
    if (isActor(a.name) || isActor(b.name)) continue;
    const fk = profile(l.from, l.fromColumn);
    const funnel: Funnel | undefined =
      a.rows >= 1.5 * b.rows &&
      (fk?.nulls || 0) >= a.rows / 2 &&
      unique(fk, a.rows)
        ? {
            upstream: a.id,
            downstream: b.id,
            link: { side: "upstream", column: l.fromColumn, key: l.toColumn },
          }
        : b.rows >= 1.5 * a.rows && b.key && unique(fk, a.rows)
          ? {
              upstream: b.id,
              downstream: a.id,
              link: { side: "downstream", column: l.fromColumn, key: b.key },
            }
          : undefined;
    if (funnel) {
      a.funnels.push(funnel);
      b.funnels.push(funnel);
    }
  }
  for (const a of out)
    for (const c of columnsOf(
      onto.entities.find((e) => e.id === a.id)!.dataset,
    ))
      if (/^BOOL/i.test(c.type) && BOOLEAN_OUTCOME.test(c.name))
        a.funnels.push({
          upstream: a.id,
          downstream: a.id,
          link: { side: "flag", column: c.name },
        });
  return out;
}

const WEIGHT: Record<InsightDriverKind, number> = {
  revenue: 1,
  customers: 0.9,
  acquisition: 0.8,
  experience: 0.7,
  activity: 0.45,
};
const CONTEXT: Record<InsightDriverKind, RegExp> = {
  revenue:
    /revenue|sales|income|billing|mrr|arr|gmv|order value|spend|monetiz/i,
  customers:
    /customer|client|retention|retain|churn|loyal|engag|active|repeat|at risk|patient|member|user/i,
  acquisition:
    /traffic|acquisition|marketing|campaign|channel|visit|lead|signup|sign-up|growth|source/i,
  experience:
    /support|ticket|satisfaction|nps|csat|experience|complain|service level|sla/i,
  activity: /$^/,
};

const checkLabels: Record<InsightCheck, string> = {
  cohort: "Activity of a fixed core cohort",
  mix: "Mix and dependency",
  unit: "Value or output per activity",
  conversion: "Progression and conversion",
};
export const checkLabel = (c: InsightCheck) => checkLabels[c];

// Candidate business drivers, best first: at most one per lens, plus plain
// volume for activities no lens covers.
export function proposeDrivers(
  activities: Activity[],
  datasets: Resource[],
  context: string,
): Driver[] {
  const rowsById = new Map(datasets.map((d) => [d.id, rowsOf(d)]));
  const out: Driver[] = [];
  const add = (
    kind: InsightDriverKind,
    a: Activity,
    label: string,
    reasons: string[],
    opts: { measure?: Measure; prior?: number } = {},
  ) => {
    const actor = a.participants.find((p) => p.role === "actor"),
      // Acquisition compares sources, not who contributes most.
      contributor =
        kind === "acquisition" ? undefined : actor || a.participants[0],
      funnel =
        kind === "revenue"
          ? a.funnels.find((f) => f.downstream === a.id && f.upstream !== a.id)
          : a.funnels.find((f) => f.upstream === a.id);
    const dimensions =
      kind === "acquisition"
        ? a.dimensions.filter((d) => d.role === "source")
        : a.dimensions;
    const checks: Driver["checks"] = {
      cohort: actor
        ? "Regular-cohort alerts need matched historical cohorts to distinguish a decline from normal churn; this check is deferred."
        : `No link from ${a.many} to a customer, account or other participant.`,
      mix:
        dimensions.length || contributor
          ? null
          : `No source, segment or participant to compare ${a.many} by.`,
      unit: opts.measure
        ? null
        : `No money amount or other outcome is measured per ${a.one}.`,
      conversion:
        funnel?.link.side === "flag"
          ? "The outcome flag has no event time to bound both observation periods."
          : funnel
            ? null
            : `No recorded link between ${a.many} and a later outcome.`,
    };
    const eligible = Object.values(checks).filter((c) => c === null).length;
    if (!eligible) return;
    const matches = !!context && CONTEXT[kind].test(context);
    out.push({
      id: `${kind}:${a.id}`,
      kind,
      label,
      activity: a,
      measure: opts.measure,
      actor,
      contributor,
      dimensions,
      funnel,
      checks,
      reasons: [
        ...reasons,
        ...(matches ? ["Your setup goal mentions it"] : []),
      ],
      prior:
        WEIGHT[kind] *
        (0.6 + 0.1 * eligible) *
        (0.7 + 0.3 * Math.min(1, Math.log10(a.rows + 1) / 5)) *
        (opts.prior ?? 1) *
        (matches ? 1.25 : 1),
    });
  };
  for (const a of activities) {
    const actor = a.participants.find((p) => p.role === "actor"),
      money = a.measures[0];
    if (money?.direction === "up" && a.direction !== "down")
      add(
        "revenue",
        a,
        /sales/i.test(money.column) ? "Sales" : "Revenue",
        [
          `${capital(a.many)} carry a money amount (${money.column})`,
          ...(actor ? [`Linked to ${lowerName(actor.entity)}`] : []),
        ],
        // Line items repeat their order's amount; prefer whole records.
        {
          measure: money,
          prior: /\b(items?|lines?)\b/i.test(a.name) ? 0.7 : 1,
        },
      );
    if (actor) {
      // Engagement is about repeat activity per participant.
      const per = a.rows / Math.max(1, rowsById.get(actor.datasetId) || a.rows);
      add(
        "customers",
        a,
        `${capital(lowerName(actor.entity))} engagement`,
        [
          `${capital(a.many)} link to ${lowerName(actor.entity)}`,
          ...(per >= 1.5
            ? [
                `About ${Math.round(per)} ${a.many} per ${lowerName(actor.entity)}`,
              ]
            : []),
        ],
        { prior: Math.min(1, 0.45 + 0.35 * Math.log10(1 + per)) },
      );
    }
    // Arrivals by source; complaints by channel belong to experience.
    const source = a.dimensions.find((d) => d.role === "source");
    if (source && a.direction !== "down")
      add(
        "acquisition",
        a,
        /session|visit|pageview|traffic/i.test(a.name)
          ? "Traffic"
          : "Acquisition",
        [`${capital(a.many)} are tagged by ${source.column}`],
      );
    if (words(a.name).some((w) => EXPERIENCE.test(w)))
      add("experience", a, "Customer experience", [
        `${capital(a.many)} record how customers fare`,
      ]);
    add("activity", a, capital(a.many), [
      `${capital(a.many)} happen over time`,
    ]);
  }
  const ranked = (xs: Driver[]) =>
    xs.sort((a, b) => b.prior - a.prior || a.id.localeCompare(b.id));
  const kept: Driver[] = [];
  for (const d of ranked(out.filter((d) => d.kind !== "activity")))
    if (!kept.some((k) => k.kind === d.kind)) kept.push(d);
  for (const d of ranked(out.filter((d) => d.kind === "activity")))
    if (!kept.some((k) => k.activity.id === d.activity.id)) kept.push(d);
  return ranked(kept);
}

// When two selected lenses look at the same activity and measure, or follow
// the same funnel, each check runs once, under the lens it belongs to; the
// other says where it was checked. Sources belong to acquisition.
const owners: Record<InsightCheck, InsightDriverKind[]> = {
  cohort: ["customers", "revenue", "experience", "activity", "acquisition"],
  unit: ["revenue", "customers", "experience", "activity", "acquisition"],
  mix: ["acquisition", "customers", "revenue", "experience", "activity"],
  conversion: ["acquisition", "customers", "activity", "experience", "revenue"],
};
export function shareChecks(selected: Driver[]): Driver[] {
  const out = selected.map((d) => ({
    ...d,
    checks: { ...d.checks },
    dimensions: [...d.dimensions],
  }));
  const before = (c: InsightCheck, a: Driver, b: Driver) =>
    owners[c].indexOf(a.kind) < owners[c].indexOf(b.kind);
  for (const d of out)
    for (const o of out) {
      if (d === o) continue;
      const same =
        d.activity.id === o.activity.id &&
        (d.measure?.column ?? null) === (o.measure?.column ?? null);
      for (const c of ["cohort", "unit"] as const)
        if (
          same &&
          d.checks[c] === null &&
          o.checks[c] === null &&
          before(c, o, d)
        )
          d.checks[c] = `Checked under ${o.label}.`;
      if (
        d.funnel &&
        d.funnel === o.funnel &&
        d.checks.conversion === null &&
        o.checks.conversion === null &&
        before("conversion", o, d)
      )
        d.checks.conversion = `Checked under ${o.label}.`;
      if (same && o.kind === "acquisition" && d.kind !== "acquisition") {
        d.dimensions = d.dimensions.filter((x) => x.role !== "source");
        if (!d.dimensions.length && !d.contributor && d.checks.mix === null)
          d.checks.mix = `Checked under ${o.label}.`;
      }
    }
  return out;
}

export const driverSummary = (d: Driver): InsightDriver => ({
  id: d.id,
  kind: d.kind,
  label: d.label,
  activity: d.activity.name,
  measure: d.measure
    ? `${d.label} (${d.measure.column})`
    : capital(d.activity.many),
  reasons: d.reasons,
});
