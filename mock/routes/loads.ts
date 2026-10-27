import {
  parseAgentEnrichments,
  parseAgentMappings,
  parseAgentOntology,
  ruleEvaluationRefusal,
  type AgentBinding,
} from "../../src/web/areas/ontology/model";
import { CREDITS_PER_PROFILE } from "../../src/web/areas/ontology/enrichment-fields";
import { decisionsFor } from "../review-decisions";
import { parse } from "smol-toml";
import type {
  FoundryEnrichmentStop,
  FoundryLoadEnrichment,
  FoundryLoadEnrichmentStatus,
  FoundryReviewDecision,
  CreateLoadRequest,
  DocumentReadOutcome,
  FoundryLoad,
  FoundryOntology,
  GraphJob,
  JobKind,
  JobPhase,
  JobStatus,
  ListLoadsResponse,
  LoadResponse,
  LoadStatus,
  ReadFoundryGraphRequest,
  ReadFoundryGraphResponse,
} from "../../src/shared/wfe";
import stripeRows from "../fixtures/stripe-rows.json";
import { PEOPLE, type PersonRow } from "../fixtures/sales-graph";
import {
  MODEL,
  VERIFIER,
  applyProfiles,
  extractDocument,
  ingestChunk,
  markLoad,
  type EnrichmentBinding,
  type ExtractOntology,
  type ProfileRow,
  type Row,
} from "../graph/extract";
import {
  answer,
  compile,
  evaluate,
  readsOf,
  sweepViolations,
} from "../graph/rules";
import { scopeFor } from "../graph/scope";
import { fail, id, now, ok, type MockRoute } from "../http";
import { PROD_STAMP, config, onReset } from "../state";
import { aiModel } from "./ai";
import { isFoundryPro } from "./billing";
import { workspaceConnectors } from "./data";
import { beforeRead, startingRefuses, tenantGraph } from "./databases";
import {
  dropExtractions,
  joinLoad,
  loadDocuments,
  setExtraction,
  spentAllowance,
  wakeReads,
} from "./documents";
import {
  charge,
  enrichmentAllowed,
  enrichmentFlagOn,
  enrichmentKnobs,
  fakeEnrich,
  headroom,
  lastTurnedOff,
  planLookups,
  previousResults,
  publishResults,
  reserve,
  type Lookup,
  type Person,
  type Result,
} from "./enrichment";
import { isScope, ontologyById, ontologyOf } from "./ontologies";
import { generationsEnded, ontologyVersion, versionScope } from "./ontology";
import { sourceId } from "./ontology-agent";

// An ontology's loads, as the backend's foundry-graph-worker runs them. A
// load is one write of a version
// into the ontology's database: its id is its load job's, and everything it
// writes carries that id as `foundry_load` (../graph/store.ts LoadGraph), so
// several loads, of one version or several, sit side by side.
//
// Jobs, one live per ontology:
// - load: ingests the version's mapped tables, then reads its documents
//   (the uploaded documents of the version's scope, ./documents.ts
//   joinLoad), then finishes, after reading any that joined meanwhile. It ends the load ready (and current), partial
//   (current only when nothing else is) or failed.
// - extract: reads documents the current load hasn't read, queued by an
//   upload or move into a set its version reads (documentArrived), by a
//   connector's sync writing one, or by making a load current. An API
//   source's documents (a set with an allowance) never join a load a
//   person's job is writing: its end owes the extract that reads them. A
//   job no person started holds back the documents of a set whose
//   allowance is spent (heldBack): it reads the others, then ends
//   budget_exhausted naming the API source, as the graph worker does, and
//   no extract is owed for them alone; a person's job reads them.
// - delete: removes one load that is deleting, oldest first, once nothing
//   else runs; a new load stops it and it runs again after.
// When a job ends: retention (the current load and the newest others, five
// in all, keep; the rest start deleting), then an extract owed to the
// current load (an upload or set-current while a job ran), else a delete.
//
// The tables load a fixed set of Acme's Stripe rows (64 customers and 65
// invoices: the three the seeded contract can name, then
// fixtures/stripe-rows.json) for mappings that read Stripe's customers and
// invoices; other tables load nothing. A PDF is read as plain text, the
// text its read gave; one whose read failed (a scanned PDF has no text) is
// recorded failed with the read's code, for good, without a model call, and
// one still being read waits for its read (./documents.ts wakeReads). Each
// phase takes `loadStepMs` (one step per document). Test knobs through POST /__mock/config: `loadOutcome` "fail"
// (the ingest fails), "sources_changed" (the tables were re-synced while the
// load ran) or "budget_exhausted" (the spending cap stops it after one
// document), `documentOutcome` "fail" (reading a document fails) and
// `deleteOutcome` "fail" (a delete fails for good: delete_failed). A
// database still starting (`startingQueries` in ./databases.ts) refuses an
// ingest's first write: the attempt fails, and the load waits, queued with
// the worker's words, to try again three steps later.
//
// A load of a version with an [enrichments] item looks people up at its
// first step: it plans from the people the mappings
// read (the Shop database's public.people, fixtures/sales-graph.ts), carrying
// forward what the ontology's newest results found; with nobody to look up,
// or lookups not allowed (the operators' flag off, no Pro, no credits), it
// goes on with what it carried. Otherwise the job parks, queued, while
// ./enrichment.ts's fake worker waits a step for a slot, then looks people
// up a batch a step (`enrichmentStepMs`), and wakes it with the profiles to
// apply. The tables load with them: each binds its person by email or
// LinkedIn URL. Without consent the step is `off`, and applies nothing.
// Stopping a queued step ends it at once, a running one after its batch.
//
// A load or extract that wrote ends with a sweep of the version's
// constraints (../graph/rules.ts), which brings the load's FoundryViolation
// records up to date. POST .../loads/{loadId}:read answers the engine's
// reads over the load with the version's rules evaluated, refusing reads
// that don't fit the engine's schema; `readErrors` ({"derived:covers":
// "budget"}, or "violations") fails that part of entity reads with that
// code (budget, hub or invalid).

type Outcome = "succeed" | "fail" | "sources_changed" | "budget_exhausted";
type Knobs = {
  loadStepMs?: number;
  loadOutcome?: Outcome;
  documentOutcome?: "succeed" | "fail";
  documentAdjudicated?: boolean;
  deleteOutcome?: "succeed" | "fail";
  readErrors?: Record<string, "budget" | "hub" | "invalid">;
  // ./ontologies.ts: the caller can't read the ontology's database.
  ontologyDatabaseHidden?: boolean;
};
const knobs = config as typeof config & Knobs;
/**
 * A workspace's month of loads (foundry.graph.monthly_usd): dev's $100, or
 * prod's $50 with MOCK_STAMP=prod.
 */
export const MONTH_CAP_MICROS = PROD_STAMP ? 50_000_000 : 100_000_000;
const STEP_MS = 1200,
  DOCUMENT_USD_MICROS = 18_400,
  // A load's cap (foundry.graph.max_job_usd): dev's $20, or prod's $10.
  LOAD_CAP_MICROS = PROD_STAMP ? 10_000_000 : 20_000_000,
  // The current load and the newest others, in all (foundry.graph.retain_loads).
  RETAIN_LOADS = 5;

/** A foundry_loads row; a deleted one stays for the month's spend. */
type StoredLoad = {
  id: string;
  workspaceId: string;
  ontologyId: string;
  tenantId: string;
  versionId: string;
  clientToken: string;
  status: LoadStatus | "deleted";
  createdAt: string;
  finishedAt: string | null;
  // The ontology's decisions when it was created: what every document read
  // into it applies.
  decisions: FoundryReviewDecision[];
  // Its People enrichment step, once its job planned one.
  enrichment?: StoredEnrichment;
};

/** How a step ends: done, partial for a reason, or cancelled (publishing nothing). */
type StepEnd =
  | { status: "done" | "cancelled" }
  | { status: "partial"; stoppedBy: FoundryEnrichmentStop };
/** Where a step is: live, or how it ended. Only a partial step has a reason. */
type StepState =
  | { status: Exclude<FoundryLoadEnrichmentStatus, StepEnd["status"]> }
  | StepEnd;

/**
 * A load's People enrichment step: what the WFE says of it (its int64
 * counts as numbers here), and the worker's state.
 */
type StoredEnrichment = {
  state: StepState;
  counts: Record<
    Exclude<
      keyof FoundryLoadEnrichment,
      "status" | "stoppedBy" | "credits" | "publishedAt"
    >,
    number
  >;
  credits: number;
  publishedAt: string | null;
  item: EnrichmentBinding;
  // Whom it still looks up, and the results: those carried first.
  queue: Lookup[];
  results: Result[];
  carriedResults: number;
  // The profiles the load applies, once published.
  profiles: ProfileRow[];
  // When the worker next acts, when an unclaimed step gives up (ms), and
  // the credits it holds back.
  at: number;
  claimBy: number;
  reserved: number;
  stop: boolean;
  stepMs: number;
  outcome?: (typeof enrichmentKnobs)["enrichmentOutcome"];
};

/**
 * Whose work a job is (its requested_by_user_id): a person's (a load, a
 * load's delete, or the extract an upload, a move or making a load current
 * queues), or no one's (the extract a job's end owes, a connector sync's, a
 * load without profiles, a delete a job's end chains).
 */
type RequestedBy = "person" | "system";
/**
 * Whose work a document's arrival is (model.FoundryDocumentFanOut): a
 * person's upload, move or read ("person"), a connector sync's ("sync"), or
 * a sync's of a set with an allowance ("allowance": SystemOnly), which is
 * no one's either and never joins a load a person's job is writing.
 */
export type Arrival = "person" | "sync" | "allowance";

type StoredJob = {
  id: string;
  loadId: string;
  workspaceId: string;
  ontologyId: string;
  tenantId: string;
  kind: JobKind;
  requestedBy: RequestedBy;
  versionId: string;
  status: JobStatus;
  phase: JobPhase;
  // When the current step started, in ms.
  at: number;
  chunksDone: number;
  documentsDone: number;
  documentsTotal: number;
  queue: string[];
  usdMicros: number;
  error: string;
  createdAt: string;
  startedAt: string | null;
  finishedAt: string | null;
  plan: {
    stepMs: number;
    outcome: Outcome;
    documentOutcome: "succeed" | "fail";
    deleteOutcome: "succeed" | "fail";
  };
};

const state = {
  loads: [] as StoredLoad[],
  jobs: [] as StoredJob[],
  // The current load of each ontology, by ontology id.
  current: new Map<string, string>(),
  // Ontologies owed an extract of their current load once the live job ends.
  owed: new Set<string>(),
  // Ontologies owed a load without profiles (consent was turned off).
  purgesOwed: new Set<string>(),
};
onReset(() => {
  state.loads = [];
  state.jobs = [];
  state.current.clear();
  state.owed.clear();
  state.purgesOwed.clear();
  delete knobs.loadStepMs;
  delete knobs.loadOutcome;
  delete knobs.documentOutcome;
  delete knobs.documentAdjudicated;
  delete knobs.deleteOutcome;
  delete knobs.readErrors;
});

const live = (j: StoredJob) => j.status === "queued" || j.status === "running";
const iso = (ms: number) => new Date(ms).toISOString();
const loadsOf = (ontologyId: string) =>
  state.loads.filter(
    (l) => l.ontologyId === ontologyId && l.status !== "deleted",
  );
const jobsOf = (ontologyId: string) =>
  state.jobs.filter((j) => j.ontologyId === ontologyId);
const loadById = (loadId: string) =>
  state.loads.find((l) => l.id === loadId && l.status !== "deleted");
const versionName = (j: { ontologyId: string; versionId: string }) =>
  `foundry-v${ontologyVersion(j.ontologyId, j.versionId)?.version ?? 0}`;
/**
 * Adds to a load the documents its version's scope holds now, or only
 * `documentId` of them.
 */
const joinScope = (l: StoredLoad, documentId?: string) =>
  joinLoad(
    l.id,
    l.workspaceId,
    versionScope(l.ontologyId, l.versionId),
    documentId,
  );

/** A load of the ontology that isn't deleted, as it stands now. */
export const ontologyLoad = (ontologyId: string, loadId: string) => {
  advanceLoads(ontologyId);
  return loadById(loadId)?.ontologyId === ontologyId
    ? loadById(loadId)
    : undefined;
};

/** An ontology's current load's id, if it has one. */
export const currentLoadId = (ontologyId: string) =>
  state.current.get(ontologyId);
/** The version of an ontology's current load, if it has one. */
export const currentLoadVersionId = (ontologyId: string) =>
  loadById(currentLoadId(ontologyId) || "")?.versionId;

/** The ontology's live job, of any kind. */
export const liveJob = (ontologyId: string) => {
  advanceLoads(ontologyId);
  return jobsOf(ontologyId).find(live);
};

/** What GET ontologies says of its loads. */
export function ontologyLoads(
  ontologyId: string,
): Pick<FoundryOntology, "currentLoad" | "lastLoad" | "activeJob"> {
  advanceLoads(ontologyId);
  const current = loadById(currentLoadId(ontologyId) || ""),
    last = jobsOf(ontologyId).find((j) => j.kind === "load"),
    active = jobsOf(ontologyId).find(live);
  return {
    currentLoad: current
      ? {
          id: current.id,
          version: versionName(current),
          finishedAt: current.finishedAt,
        }
      : null,
    lastLoad: last
      ? { jobId: last.id, status: last.status, finishedAt: last.finishedAt }
      : null,
    activeJob: active
      ? {
          kind: active.kind,
          loadId: active.loadId,
          phase: active.phase,
          progress: progressOf(active),
        }
      : null,
  };
}

// Acme's Stripe rows, as the customers and invoices tables hold them.
const STRIPE = "conn_acme_stripe";
const customers = [
  ["cus_GLOBEX01", "Globex Corporation", "ap@globex.example", "usd"],
  ["cus_INITECH2", "Initech LLC", "billing@initech.example", "eur"],
  ["cus_HOOLI003", "Hooli Inc", "finance@hooli.example", "usd"],
  ...(stripeRows.customers as [string, string, string, string][]),
];
const invoices = [
  ["in_1001", "cus_GLOBEX01", 1200000, "paid"],
  ["in_1002", "cus_GLOBEX01", 1200000, "open"],
  ["in_1003", "cus_INITECH2", 104200, "paid"],
  ["in_1004", "cus_HOOLI003", 54000, "void"],
  ...(stripeRows.invoices as [string, string, number, string][]),
] as const;

type Definition = { ontology_toml?: unknown; mappings_toml?: unknown };
function ontologyOfVersion(def: Definition): ExtractOntology & {
  links: Map<string, { from: string; to: string }>;
} {
  let doc: {
    types?: Record<
      string,
      { names?: string[]; identifiers?: Record<string, string> }
    >;
    links?: Record<string, { from?: string; to?: string }>;
  } = {};
  try {
    doc = parse(String(def.ontology_toml || "")) as typeof doc;
  } catch {
    // An unreadable draft loads nothing.
  }
  const types = doc.types || {};
  return {
    types: new Set(Object.keys(types)),
    names: new Map(
      Object.entries(types).flatMap(([t, v]) =>
        v.names?.length ? [[t, v.names[0]!] as const] : [],
      ),
    ),
    identifiers: new Map(
      Object.entries(types).map(([t, v]) => [t, v.identifiers || {}]),
    ),
    links: new Map(
      Object.entries(doc.links || {}).map(([l, v]) => [
        l,
        { from: String(v.from || ""), to: String(v.to || "") },
      ]),
    ),
  };
}

function ingest(j: StoredJob) {
  const def = (ontologyVersion(j.ontologyId, j.versionId)?.definition ||
    {}) as Definition;
  const mappings = String(def.mappings_toml || "");
  const { names, identifiers } = ontologyOfVersion(def);
  const stripe = workspaceConnectors(j.workspaceId).find(
    (c) => c.id === STRIPE,
  );
  const table = (name: string) => {
    const synced = stripe?.tables?.find((t) => t.name === name);
    const source = sourceId(STRIPE, name);
    return synced && mappings.includes(source)
      ? { source, snapshot: synced.syncedAt || j.createdAt }
      : undefined;
  };
  const g = tenantGraph(j.tenantId).view(j.loadId),
    ontologyVersionName = versionName(j);
  const people = table("customers"),
    bills = table("invoices");
  const chunk = (
    t: { source: string; snapshot: string },
    mapping: string,
    rows: Row[],
  ) => {
    if (
      ingestChunk(
        g,
        { ...t, mapping, ontologyVersion: ontologyVersionName },
        rows,
      )
    )
      j.chunksDone += 1;
  };
  if (people)
    chunk(
      people,
      "customers",
      customers.map(([key, name, email, currency]) => ({
        type: "Customer",
        key,
        name,
        props: { customer_id: key, company_name: name, email, currency },
        names: names.has("Customer") ? [names.get("Customer")!] : [],
        identifiers: identifiers?.get("Customer"),
      })),
    );
  if (bills)
    chunk(
      bills,
      "invoices",
      invoices.map(([key, customer, amount, status]) => ({
        type: "Invoice",
        key,
        name: key,
        props: { invoice_id: key, amount_due: amount, status },
        links: [{ link: "billed_to", toType: "Customer", toKey: customer }],
      })),
    );
  // public.people, through the mapping that reads it: each binding's
  // entities, the ends of its links first.
  const m = peopleMapping(mappings),
    synced = workspaceConnectors(j.workspaceId)
      .find((c) => c.id === SHOP)
      ?.tables?.find((t) => t.name === "people");
  if (m && synced) {
    const ends = new Set(m.links.map((l) => l.to));
    const rowOf = (b: AgentBinding, r: PersonRow): Row | undefined => {
      const key = bindingKey(b, r);
      if (key === undefined || b.identify.kind !== "key") return undefined;
      const props = Object.fromEntries(
        [...b.identify.fields, ...b.props].flatMap(([prop, f]) => {
          const v = column(r, f.field, f.transform);
          return v === undefined ? [] : [[prop, v]];
        }),
      );
      const named = names.get(b.type);
      return {
        type: b.type,
        key,
        name: String((named && props[named]) || key),
        props,
        names: named ? [named] : [],
        identifiers: identifiers?.get(b.type),
      };
    };
    const rows: Row[] = [];
    for (const b of m.bindings.filter((b) => ends.has(b.alias)))
      for (const r of PEOPLE) {
        const x = rowOf(b, r);
        if (x && !rows.some((y) => y.type === x.type && y.key === x.key))
          rows.push(x);
      }
    for (const b of m.bindings.filter((b) => !ends.has(b.alias)))
      for (const r of PEOPLE) {
        const x = rowOf(b, r);
        if (!x) continue;
        x.links = m.links
          .filter((l) => l.from === b.alias)
          .flatMap((l) => {
            const to = m.bindings.find((t) => t.alias === l.to),
              y = to && rowOf(to, r);
            return y ? [{ link: l.link, toType: y.type, toKey: y.key }] : [];
          });
        rows.push(x);
      }
    chunk(
      {
        source: sourceId(SHOP, "people"),
        snapshot: synced.syncedAt || j.createdAt,
      },
      m.name,
      rows,
    );
  }
  // The profiles its enrichment step published, beside the tables.
  const e = loadById(j.loadId)?.enrichment;
  if (e?.profiles.length) {
    const applied = applyProfiles(
      g,
      { ontologyVersion: ontologyVersionName },
      e.item,
      e.profiles,
    );
    if (applied) Object.assign(e.counts, applied);
  }
}

// ---------------------------------------------------------------- People enrichment

const SHOP = "conn_acme_shop";

/** The version's mapping of public.people, if it has one. */
function peopleMapping(mappingsToml: string) {
  const parsed = parseAgentMappings(mappingsToml);
  return parsed.ok
    ? parsed.value.find(
        (m) =>
          m.rows.kind === "source" &&
          m.rows.source === sourceId(SHOP, "people"),
      )
    : undefined;
}
/** A row's value of a column, transformed; undefined for none. */
function column(r: PersonRow, field: string, transform: string[] = []) {
  const v = r[field as keyof PersonRow];
  if (v === null || v === undefined || v === "") return undefined;
  return transform.includes("lower") ? v.toLowerCase() : v;
}
/** The key a binding gives a row's entity, or undefined without one. */
function bindingKey(b: AgentBinding, r: PersonRow) {
  if (b.identify.kind !== "key") return undefined;
  const parts = b.identify.fields.map(([, f]) =>
    column(r, f.field, f.transform),
  );
  return parts.every((p) => p !== undefined) ? parts.join("/") : undefined;
}

/**
 * Plans a load's People enrichment step, as the graph worker's prepare
 * does; undefined for a version without an [enrichments] item. Ends at once
 * (and the load goes on) without consent, with nobody to look up, off Pro or
 * without credits; otherwise it is queued for the worker.
 */
function prepareEnrichment(
  j: StoredJob,
  l: StoredLoad,
  due: number,
): StoredEnrichment | undefined {
  const def = (ontologyVersion(j.ontologyId, j.versionId)?.definition ||
    {}) as Definition;
  const items = parseAgentEnrichments(String(def.mappings_toml || "")),
    ontology = parseAgentOntology(String(def.ontology_toml || ""));
  const item = items.ok ? items.value[0] : undefined;
  const type =
    item && ontology.ok
      ? ontology.value.types.find((t) => t.name === item.type)
      : undefined;
  if (!item || !type || !ontology.ok) return undefined;
  const link = ontology.value.links.find(
      (x) => x.name === item.workHistory?.link,
    ),
    to = ontology.value.types.find((t) => t.name === link?.to);
  const e: StoredEnrichment = {
    state: { status: "off" },
    counts: {
      people: 0,
      noName: 0,
      noClue: 0,
      shared: 0,
      skipped: 0,
      carried: 0,
      toLookUp: 0,
      lookedUp: 0,
      found: 0,
      held: 0,
      notFound: 0,
      uncertain: 0,
      applied: 0,
      unmatched: 0,
    },
    credits: 0,
    publishedAt: null,
    item: {
      type: type.name,
      names: type.names,
      clues: item.clues,
      props: item.props,
      workHistory:
        item.workHistory && link && to
          ? {
              link: link.name,
              to: to.name,
              match: item.workHistory.match,
              key: to.key.length === 1 && to.key[0] === item.workHistory.match,
              props: item.workHistory.props,
            }
          : undefined,
    },
    queue: [],
    results: [],
    carriedResults: 0,
    profiles: [],
    at: due,
    claimBy: due,
    reserved: 0,
    stop: false,
    stepMs: enrichmentKnobs.enrichmentStepMs ?? STEP_MS,
    outcome: enrichmentKnobs.enrichmentOutcome,
  };
  if (!enrichmentAllowed(j.workspaceId)) {
    e.publishedAt = iso(due);
    return e;
  }
  // Who the mappings make a person of the type, with their clues.
  const m = peopleMapping(String(def.mappings_toml || ""));
  const own = m?.bindings.find(
    (b) =>
      b.type === type.name &&
      b.missing === "create" &&
      b.identify.kind === "key",
  );
  const fieldOf = (prop?: string) =>
    prop
      ? [
          ...(own?.identify.kind === "key" ? own.identify.fields : []),
          ...(own?.props || []),
        ].find(([p]) => p === prop)?.[1]
      : undefined;
  const people: Person[] = own
    ? PEOPLE.flatMap((r) => {
        const key = bindingKey(own, r),
          value = (prop?: string) => {
            const f = fieldOf(prop);
            return f ? column(r, f.field, f.transform) : undefined;
          };
        return key === undefined
          ? []
          : [
              {
                key,
                name: type.names
                  .map((n) => value(n) || "")
                  .join(" ")
                  .trim(),
                email: value(item.clues.email),
                linkedin: value(item.clues.linkedinUrl),
              },
            ];
      })
    : [];
  const plan = planLookups(people, previousResults(j.ontologyId), due);
  Object.assign(e.counts, plan.counts, { toLookUp: plan.calls.length });
  e.results = plan.carried;
  e.carriedResults = plan.carried.length;
  e.queue = plan.calls;
  const end: StepEnd | undefined = !plan.calls.length
    ? { status: "done" }
    : !enrichmentFlagOn()
      ? { status: "partial", stoppedBy: "flag" }
      : !isFoundryPro(j.workspaceId)
        ? { status: "partial", stoppedBy: "pro" }
        : headroom(j.workspaceId, l.id) < CREDITS_PER_PROFILE
          ? { status: "partial", stoppedBy: "cap" }
          : undefined;
  if (end) {
    endEnrichment(j, l, e, due, end);
    return e;
  }
  Object.assign(e, {
    state: { status: "queued" },
    at: due + e.stepMs,
    claimBy: due + 3 * e.stepMs,
  });
  return e;
}

const enrichmentLive = (e?: StoredEnrichment) =>
  e?.state.status === "queued" || e?.state.status === "running";

/**
 * Ends a step: publishes its results (done or partial; cancelled publishes
 * nothing), lets its credits go and wakes the load's job at `at`. A parked
 * job stays queued, due at `at`, until the graph worker picks it up again,
 * as the WFE's wake leaves it.
 */
function endEnrichment(
  j: StoredJob | undefined,
  l: StoredLoad,
  e: StoredEnrichment,
  at: number,
  end: StepEnd,
) {
  if (end.status === "cancelled") {
    Object.assign(e, { state: end, queue: [], profiles: [] });
  } else {
    e.state = end;
    e.profiles = publishResults(l.ontologyId, e.results);
    // Duplicates publish held: count again. Every earlier result reused is
    // carried, a no match too.
    const own = e.results.slice(e.carriedResults),
      count = (outcome: string) =>
        own.filter((r) => r.outcome === outcome).length;
    Object.assign(e.counts, {
      carried: e.carriedResults,
      found: count("accepted"),
      held: count("held"),
      notFound: count("not_found"),
      uncertain: count("uncertain"),
    });
  }
  e.publishedAt = iso(at);
  e.reserved = 0;
  reserve(l.workspaceId, l.id, 0);
  if (j && live(j)) j.at = at;
}

/**
 * The enrichment worker's clock for a parked load: claims the step after a
 * step's wait (never with "busy", which gives up at `claimBy`), then looks
 * up a batch a step, within the credits it reserved. True once it woke the
 * job.
 */
function lookUp(j: StoredJob, l: StoredLoad, e: StoredEnrichment): boolean {
  const BATCH = 2;
  while (enrichmentLive(e)) {
    if (Date.now() < e.at) return false;
    const at = e.at;
    e.at += e.stepMs;
    if (e.stop) {
      endEnrichment(j, l, e, at, { status: "partial", stoppedBy: "stopped" });
      break;
    }
    if (e.state.status === "queued") {
      if (e.outcome === "busy") {
        if (at >= e.claimBy)
          endEnrichment(j, l, e, at, { status: "partial", stoppedBy: "busy" });
        continue;
      }
      // The worker checks the flag and Pro again as it claims the step.
      if (!enrichmentFlagOn()) {
        endEnrichment(j, l, e, at, { status: "partial", stoppedBy: "flag" });
        break;
      }
      if (!isFoundryPro(l.workspaceId)) {
        endEnrichment(j, l, e, at, { status: "partial", stoppedBy: "pro" });
        break;
      }
      e.state = { status: "running" };
      e.reserved = Math.min(
        e.queue.length * CREDITS_PER_PROFILE,
        headroom(l.workspaceId, l.id),
      );
      reserve(l.workspaceId, l.id, e.reserved);
      continue;
    }
    const n = Math.min(
      BATCH,
      e.queue.length,
      Math.floor(e.reserved / CREDITS_PER_PROFILE),
    );
    if (!n) {
      endEnrichment(j, l, e, at, { status: "partial", stoppedBy: "cap" });
      break;
    }
    let credits = 0;
    for (const person of e.queue.splice(0, n)) {
      const { credits: spent, ...result } = fakeEnrich(person);
      credits += spent;
      e.results.push({ ...result, checkedAt: iso(at) });
      e.counts.lookedUp += 1;
      if (result.outcome === "accepted") e.counts.found += 1;
    }
    charge(l.workspaceId, l.id, credits);
    e.credits += credits;
    e.reserved -= n * CREDITS_PER_PROFILE;
    reserve(l.workspaceId, l.id, e.reserved);
    if (e.outcome && e.outcome !== "busy" && e.queue.length) {
      endEnrichment(j, l, e, at, { status: "partial", stoppedBy: e.outcome });
      break;
    }
    if (!e.queue.length) endEnrichment(j, l, e, at, { status: "done" });
  }
  return true;
}

/**
 * Stops a load's lookups, as the WFE does: a queued step ends at once, a
 * running one after its batch, and an ended one stays as it is. The load, or
 * undefined for a load with no step.
 */
export function stopLoadEnrichment(
  ontologyId: string,
  loadId: string,
): FoundryLoad | undefined {
  advanceLoads(ontologyId);
  const l = loadById(loadId);
  const e = l?.ontologyId === ontologyId ? l.enrichment : undefined;
  if (!l || !e) return undefined;
  if (!enrichmentLive(e)) return toLoad(l);
  e.stop = true;
  if (e.state.status === "queued")
    endEnrichment(
      jobsOf(ontologyId).find((j) => j.loadId === l.id && live(j)),
      l,
      e,
      Date.now(),
      { status: "partial", stoppedBy: "stopped" },
    );
  return toLoad(l);
}

/** Consent was turned off: the workspace's live steps end, publishing nothing. */
export function cancelEnrichment(workspaceId: string) {
  for (const l of state.loads.filter(
    (x) => x.workspaceId === workspaceId && enrichmentLive(x.enrichment),
  )) {
    advanceLoads(l.ontologyId);
    if (enrichmentLive(l.enrichment))
      endEnrichment(
        jobsOf(l.ontologyId).find((j) => j.loadId === l.id && live(j)),
        l,
        l.enrichment!,
        Date.now(),
        { status: "cancelled" },
      );
  }
}

/**
 * Whether a load of the ontology that isn't deleting, current or not,
 * applied profiles, or will once its tables load (its step published some):
 * turning consent off purges it, as the WFE does for every load with an
 * applied file. A live step's profiles don't count: turning consent off
 * cancels it first.
 */
export function ontologyHasProfiles(ontologyId: string) {
  advanceLoads(ontologyId);
  return loadsOf(ontologyId).some(
    (l) => l.status !== "deleting" && !!l.enrichment?.profiles.length,
  );
}

/**
 * Whether a load holds profiles its workspace has since withdrawn: it
 * published some, and consent was turned off after it was created (the
 * WFE's purged step). It can't be made current, and is deleted once a clean
 * load is current.
 */
function purgedProfiles(l: StoredLoad) {
  const off = lastTurnedOff(l.workspaceId);
  return !!off && l.createdAt < off && !!l.enrichment?.profiles.length;
}

/**
 * Whether the ontology's purge is done: it is gone, or no purge is owed and
 * its current load (if any) holds no withdrawn profiles.
 */
export function purgeLoaded(ontologyId: string) {
  advanceLoads(ontologyId);
  const current = loadById(currentLoadId(ontologyId) || "");
  return (
    !ontologyById(ontologyId) ||
    (!state.purgesOwed.has(ontologyId) && !(current && purgedProfiles(current)))
  );
}

/**
 * Purges the ontology's withdrawn profiles as the WFE's sweeper does, now
 * or once its live job ends (which may make a load holding some current):
 * a current load holding them is loaded again without them, and the other
 * loads holding them are deleted once a clean load is current. A purge
 * isn't refused for the month's spend.
 */
export function queuePurgeLoad(ontologyId: string) {
  advanceLoads(ontologyId);
  state.purgesOwed.add(ontologyId);
  if (jobsOf(ontologyId).some(live)) return;
  retain(ontologyId);
  chain(ontologyId, Date.now());
}
/** Loads the current version again without profiles if the current load holds withdrawn ones; true if it did. */
function purge(ontologyId: string, at: number) {
  const current = loadById(currentLoadId(ontologyId) || "");
  if (!current || !purgedProfiles(current)) return false;
  const l: StoredLoad = {
    ...current,
    id: id("load"),
    clientToken: id("purge"),
    status: "writing",
    createdAt: iso(at),
    finishedAt: null,
    decisions: decisionsFor(ontologyId),
    enrichment: undefined,
  };
  state.loads.unshift(l);
  joinScope(l);
  create(ontologyId, l, "load", "system", at);
  return true;
}

function read(j: StoredJob, documentId: string, at: string) {
  const doc = loadDocuments(j.loadId).find((d) => d.id === documentId);
  if (!doc) return;
  const empty = { sections: 0, entities: 0, mentions: 0, facts: 0, reviews: 0 };
  // Never sent to the engine: the stage records a PDF whose text couldn't
  // be read with its read's code, for good (the read says why).
  if (doc.refused) {
    setExtraction(j.loadId, doc.id, {
      status: "failed",
      error: { code: doc.refused, retryable: false },
      ...empty,
      usdMicros: "0",
      finishedAt: at,
    });
    return;
  }
  if (j.plan.documentOutcome === "fail") {
    setExtraction(j.loadId, doc.id, {
      status: "failed",
      error: { code: "model", retryable: true },
      ...empty,
      usdMicros: "0",
      finishedAt: at,
    });
    return;
  }
  const def = (ontologyVersion(j.ontologyId, j.versionId)?.definition ||
    {}) as Definition;
  const ontology = ontologyOfVersion(def);
  // What the version's routes let the set it joined the load with fill.
  const scope = scopeFor(
    String(def.mappings_toml || ""),
    doc.setId,
    ontology.links,
  );
  const outcome = extractDocument(
    tenantGraph(j.tenantId).view(j.loadId),
    {
      id: doc.id,
      name: doc.name,
      text: doc.text,
      markdown: doc.markdown,
      ontologyVersion: versionName(j),
    },
    ontology,
    scope,
    loadById(j.loadId)?.decisions || [],
    knobs.documentAdjudicated === true,
    {
      model: aiModel(j.workspaceId, "extract") || MODEL,
      verifier: aiModel(j.workspaceId, "verify") || VERIFIER,
    },
  );
  if (outcome.kind === "already_ingested") return;
  // Stored, not read: no model call, nothing spent.
  if (outcome.kind === "out_of_scope") {
    setExtraction(j.loadId, doc.id, {
      status: "out_of_scope",
      ...outcome.counts,
      usdMicros: "0",
      finishedAt: at,
    });
    return;
  }
  if (outcome.kind === "duplicate") {
    setExtraction(j.loadId, doc.id, {
      status: "duplicate",
      duplicateOf: outcome.of,
      ...empty,
      usdMicros: "0",
      finishedAt: at,
    });
    return;
  }
  j.usdMicros += DOCUMENT_USD_MICROS;
  setExtraction(j.loadId, doc.id, {
    status: "extracted",
    ...outcome.counts,
    usdMicros: String(DOCUMENT_USD_MICROS),
    finishedAt: at,
  });
}

/**
 * Documents a load hasn't read: never read into it, or failed in a way a
 * load retries (the WFE's waiting(L)).
 */
const waiting = (loadId: string) =>
  loadDocuments(loadId)
    .filter(
      (d) =>
        !d.extraction ||
        (d.extraction.status === "failed" && d.extraction.error?.retryable),
    )
    .map((d) => d.id);
/**
 * The documents of a load that a job no person started holds back unsent,
 * by id, each with the API source whose spent allowance holds it
 * (./documents.ts spentAllowance), as the graph worker holds them; a
 * person's job holds none.
 */
function heldBack(loadId: string, by: RequestedBy) {
  return new Map(
    by === "person"
      ? []
      : loadDocuments(loadId).flatMap((d) => {
          const held = spentAllowance(d.setId);
          return held ? [[d.id, held] as const] : [];
        }),
  );
}
/** A job's load's documents waiting, but those it holds back (heldBack). */
function sendable(j: StoredJob) {
  const held = heldBack(j.loadId, j.requestedBy);
  return waiting(j.loadId).filter((d) => !held.has(d));
}
// Documents uploaded while a job ran, which it reads before it finishes.
const unread = (j: StoredJob) =>
  loadDocuments(j.loadId)
    .filter((d) => !d.extraction)
    .map((d) => d.id);

/** What the workspace's jobs created this UTC month spent, in µ$. */
export const monthSpend = (workspaceId: string) => {
  const month = new Date().toISOString().slice(0, 7);
  return state.jobs
    .filter(
      (j) => j.workspaceId === workspaceId && j.createdAt.startsWith(month),
    )
    .reduce((n, j) => n + j.usdMicros, 0);
};

/**
 * Retention: the current load and the newest others keep; the rest delete.
 * Once a clean load is current (or none is), the loads holding withdrawn
 * profiles delete too.
 */
function retain(ontologyId: string) {
  const kept = new Set<string>();
  const current = loadById(currentLoadId(ontologyId) || "");
  if (current) kept.add(current.id);
  const finished = loadsOf(ontologyId).filter((l) =>
    ["ready", "partial", "failed"].includes(l.status),
  );
  for (const l of finished) if (kept.size < RETAIN_LOADS) kept.add(l.id);
  const clean = !current || !purgedProfiles(current);
  const targeted = new Set(
    jobsOf(ontologyId)
      .filter(live)
      .map((j) => j.loadId),
  );
  for (const l of finished)
    if (
      (!kept.has(l.id) || (clean && l !== current && purgedProfiles(l))) &&
      !targeted.has(l.id)
    )
      l.status = "deleting";
}

/**
 * Once no job is live: the extract owed to the current load, else a delete
 * of the oldest load that is deleting. Jobs start at `at`, the moment the
 * one before ended.
 */
function chain(ontologyId: string, at: number) {
  if (jobsOf(ontologyId).some(live)) return;
  if (state.purgesOwed.delete(ontologyId) && purge(ontologyId, at)) return;
  const o = ontologyById(ontologyId);
  const owed = state.owed.delete(ontologyId);
  const current = loadById(currentLoadId(ontologyId) || "");
  if (owed && current) joinScope(current);
  // Only documents it may send: one held back once is not tried again.
  const held = current ? heldBack(current.id, "system") : new Map();
  if (
    o &&
    owed &&
    current &&
    waiting(current.id).some((d) => !held.has(d)) &&
    monthSpend(o.workspaceId) < MONTH_CAP_MICROS
  ) {
    create(ontologyId, current, "extract", "system", at);
    return;
  }
  const deleting = loadsOf(ontologyId)
    .filter((l) => l.status === "deleting")
    .at(-1);
  if (deleting) create(ontologyId, deleting, "delete", "system", at);
}

/** The model of a load's version's rules over what the load holds. */
function rulesOf(
  l: Pick<StoredLoad, "ontologyId" | "versionId" | "tenantId" | "id">,
) {
  const def = (ontologyVersion(l.ontologyId, l.versionId)?.definition ||
    {}) as Definition;
  return evaluate(
    tenantGraph(l.tenantId).view(l.id),
    compile(String(def.ontology_toml || "")),
  );
}

/** Ends a job: its load's status, retention, and what runs next. */
function finish(j: StoredJob, status: JobStatus, at: number, error = "") {
  Object.assign(j, { status, error, finishedAt: iso(at) });
  // What it wrote is swept: the load's violation records come up to date.
  if (j.kind !== "delete" && status !== "failed") {
    sweepViolations(rulesOf(j), "draft");
    markLoad(tenantGraph(j.tenantId).view(j.loadId), versionName(j));
  }
  const l = loadById(j.loadId);
  if (l && j.kind === "load") {
    l.finishedAt = iso(at);
    if (status === "succeeded") {
      l.status = "ready";
      state.current.set(j.ontologyId, l.id);
    } else if (status === "budget_exhausted") {
      l.status = "partial";
      if (!loadById(currentLoadId(j.ontologyId) || ""))
        state.current.set(j.ontologyId, l.id);
    } else l.status = "failed";
  }
  retain(j.ontologyId);
  chain(j.ontologyId, at);
}

// Moves a job along the clock, a step at a time.
function advance(j: StoredJob) {
  const step = j.plan.stepMs;
  while (live(j)) {
    // Parked while the enrichment worker looks people up.
    const l = j.kind === "load" ? loadById(j.loadId) : undefined;
    if (l?.enrichment && enrichmentLive(l.enrichment)) {
      if (!lookUp(j, l, l.enrichment)) return;
      continue;
    }
    const due = j.at + step;
    if (j.status === "running" && j.phase === "documents" && !j.queue.length) {
      j.phase = "finishing";
      continue;
    }
    if (Date.now() < due) return;
    j.at = due;
    switch (j.status === "queued" ? "queued" : j.phase) {
      case "queued":
        // A job picked up again keeps when it first started.
        Object.assign(j, {
          status: "running",
          startedAt: j.startedAt || iso(due),
        });
        if (j.kind === "extract") {
          j.queue = sendable(j);
          j.documentsTotal = j.queue.length;
        }
        break;
      case "provisioning": {
        // A version with People enrichment plans its step, and parks when
        // there are people to look up.
        const l = loadById(j.loadId);
        if (j.kind === "load" && l && !l.enrichment) {
          l.enrichment = prepareEnrichment(j, l, due);
          if (l.enrichment?.state.status === "queued") {
            j.status = "queued";
            break;
          }
        }
        j.phase = "ingest";
        break;
      }
      case "ingest":
        if (startingRefuses(j.tenantId)) {
          Object.assign(j, {
            status: "queued",
            error: "the database refused a write; Helix will retry",
            at: due + 2 * step,
          });
          break;
        }
        if (j.plan.outcome === "fail")
          return finish(
            j,
            "failed",
            due,
            "the version's mappings don't fit its tables: customers has no column id",
          );
        if (j.plan.outcome === "sources_changed")
          return finish(
            j,
            "failed",
            due,
            "the tables were synced again while this load was written; start a new load",
          );
        markLoad(tenantGraph(j.tenantId).view(j.loadId), versionName(j));
        ingest(j);
        j.queue = sendable(j);
        j.documentsTotal = j.queue.length;
        j.phase = "documents";
        break;
      case "documents": {
        read(j, j.queue.shift()!, iso(due));
        markLoad(tenantGraph(j.tenantId).view(j.loadId), versionName(j));
        j.documentsDone += 1;
        const spent =
          j.usdMicros >= LOAD_CAP_MICROS ||
          (j.plan.outcome === "budget_exhausted" && j.queue.length > 0);
        if (spent)
          return finish(
            j,
            "budget_exhausted",
            due,
            `the load spent its $${(LOAD_CAP_MICROS / 1e6).toFixed(2)} cap`,
          );
        break;
      }
      case "finishing": {
        // Documents uploaded while it ran are read into the load before it
        // finishes, when that load is (or now becomes) current.
        const current =
          j.kind === "load" || currentLoadId(j.ontologyId) === j.loadId;
        const held = heldBack(j.loadId, j.requestedBy);
        const late = current ? unread(j).filter((d) => !held.has(d)) : [];
        if (late.length) {
          j.queue = late;
          j.documentsTotal += late.length;
          j.phase = "documents";
          break;
        }
        // What it held back waits for next month or a person's load.
        const stop = waiting(j.loadId)
          .map((d) => held.get(d))
          .find(Boolean);
        if (stop)
          return finish(
            j,
            "budget_exhausted",
            due,
            `the API source ${JSON.stringify(stop.source)} spent its $${(stop.max / 1e6).toFixed(2)} of this month's extraction by work no one started; its newest pieces wait for next month or a load a person starts`,
          );
        return finish(j, "succeeded", due);
      }
      case "deleting": {
        const l = loadById(j.loadId);
        if (j.plan.deleteOutcome === "fail") {
          if (l) l.status = "delete_failed";
          return finish(
            j,
            "failed",
            due,
            "the database refused the delete; try again",
          );
        }
        tenantGraph(j.tenantId).dropLoad(j.loadId);
        dropExtractions(j.loadId);
        if (l) l.status = "deleted";
        return finish(j, "succeeded", due);
      }
    }
  }
}

/** Brings an ontology's jobs up to now, chained ones included. */
export function advanceLoads(ontologyId: string) {
  // PDFs read since: their sets' readers read them now.
  wakeReads();
  for (let seen = -1; seen !== state.jobs.length; ) {
    seen = state.jobs.length;
    jobsOf(ontologyId).forEach(advance);
  }
}
beforeRead.push((tenantId) => {
  const ontologies = new Set(
    state.jobs.filter((j) => j.tenantId === tenantId).map((j) => j.ontologyId),
  );
  ontologies.forEach(advanceLoads);
});

function create(
  ontologyId: string,
  load: StoredLoad,
  kind: JobKind,
  requestedBy: RequestedBy,
  at = Date.now(),
): StoredJob {
  const j: StoredJob = {
    id: kind === "load" ? load.id : id(kind),
    loadId: load.id,
    workspaceId: load.workspaceId,
    ontologyId,
    tenantId: load.tenantId,
    kind,
    requestedBy,
    versionId: load.versionId,
    status: "queued",
    phase:
      kind === "extract"
        ? "documents"
        : kind === "delete"
          ? "deleting"
          : "provisioning",
    at,
    chunksDone: 0,
    documentsDone: 0,
    documentsTotal: 0,
    queue: [],
    usdMicros: 0,
    error: "",
    createdAt: iso(at),
    startedAt: null,
    finishedAt: null,
    plan: {
      stepMs: knobs.loadStepMs ?? STEP_MS,
      outcome: knobs.loadOutcome ?? "succeed",
      documentOutcome: knobs.documentOutcome ?? "succeed",
      deleteOutcome: knobs.deleteOutcome ?? "succeed",
    },
  };
  state.jobs.unshift(j);
  return j;
}

/**
 * A document finished uploading into a set, moved into one or was written
 * by a connector's sync, `by` saying whose work that is: what the ontology
 * does about it, as the WFE's fan-out does, or undefined when neither its
 * live job's load nor its current load reads the set (their versions'
 * scopes don't name it). It alone joins the first of those loads, when
 * that one reads the set; a live job reads it, or its end queues the
 * extract that does (OWED); with none live, an extract of the current load
 * reads it (QUEUED, the requester's work) unless that load holds it already
 * (HELD) or the month's budget is spent (NO_BUDGET). A document of a set
 * with an allowance never joins a load a person's job is writing: that
 * person never pays for it, and the job's end owes the extract that reads
 * it, no one's work. A PDF whose text is still being read (`reading`) only
 * joins (READING): the read's end has it read (./documents.ts wakeReads). A
 * caller who may not write the ontology's database changes nothing
 * (SKIPPED).
 */
export function documentArrived(
  ontologyId: string,
  documentId: string,
  setId: string,
  canWrite: boolean,
  reading = false,
  by: Arrival = "person",
): DocumentReadOutcome | undefined {
  advanceLoads(ontologyId);
  const o = ontologyById(ontologyId);
  if (!o) return undefined;
  const running = jobsOf(ontologyId).find(live);
  const readers = [
    running && running.kind !== "delete" ? loadById(running.loadId) : undefined,
    loadById(currentLoadId(ontologyId) || ""),
  ].filter((l): l is StoredLoad => !!l);
  if (
    !readers.some((l) =>
      versionScope(l.ontologyId, l.versionId).includes(setId),
    )
  )
    return undefined;
  if (!canWrite) return "FOUNDRY_DOCUMENT_READ_OUTCOME_SKIPPED";
  const personWrites =
    running?.kind !== "delete" && running?.requestedBy === "person";
  if (!(by === "allowance" && personWrites)) joinScope(readers[0]!, documentId);
  if (reading) return "FOUNDRY_DOCUMENT_READ_OUTCOME_READING";
  if (running) {
    state.owed.add(ontologyId);
    return "FOUNDRY_DOCUMENT_READ_OUTCOME_OWED";
  }
  const current = readers[0]!;
  if (!waiting(current.id).includes(documentId))
    return "FOUNDRY_DOCUMENT_READ_OUTCOME_HELD";
  if (monthSpend(o.workspaceId) >= MONTH_CAP_MICROS)
    return "FOUNDRY_DOCUMENT_READ_OUTCOME_NO_BUDGET";
  create(ontologyId, current, "extract", by === "person" ? "person" : "system");
  return "FOUNDRY_DOCUMENT_READ_OUTCOME_QUEUED";
}

/**
 * An ontology is deleted: its live job and generation stop, nothing is
 * current, and every load is deleted from the database (at once here; the WFE drains them
 * with delete jobs).
 */
export function ontologyDeleted(ontologyId: string) {
  advanceLoads(ontologyId);
  generationsEnded(ontologyId);
  for (const l of loadsOf(ontologyId))
    if (enrichmentLive(l.enrichment))
      endEnrichment(undefined, l, l.enrichment!, Date.now(), {
        status: "cancelled",
      });
  state.purgesOwed.delete(ontologyId);
  for (const j of jobsOf(ontologyId).filter(live))
    Object.assign(j, {
      status: "cancelled",
      error: "the ontology was deleted",
      finishedAt: now(),
    });
  state.current.delete(ontologyId);
  state.owed.delete(ontologyId);
  for (const l of loadsOf(ontologyId)) {
    tenantGraph(l.tenantId).dropLoad(l.id);
    dropExtractions(l.id);
    l.status = "deleted";
  }
}

const progressOf = (j: StoredJob) => ({
  chunksDone: j.chunksDone,
  documentsDone: j.documentsDone,
  documentsTotal: j.documentsTotal,
});

const toJob = (j: StoredJob): GraphJob => ({
  id: j.id,
  kind: j.kind,
  loadId: j.loadId,
  versionId: j.versionId,
  status: j.status,
  phase: j.phase,
  progress: progressOf(j),
  usdMicros: String(j.usdMicros),
  error: j.error,
  createdAt: j.createdAt,
  startedAt: j.startedAt,
  finishedAt: j.finishedAt,
});

// As the WFE: the newest 20 jobs, with the count and spend of every job.
export const LISTED_JOBS = 20;
const toLoad = (l: StoredLoad): FoundryLoad => {
  const jobs = state.jobs.filter((j) => j.loadId === l.id);
  return {
    id: l.id,
    versionId: l.versionId,
    version: versionName(l),
    status: l.status as LoadStatus,
    current: currentLoadId(l.ontologyId) === l.id,
    createdAt: l.createdAt,
    finishedAt: l.finishedAt,
    jobs: jobs.slice(0, LISTED_JOBS).map(toJob),
    jobCount: jobs.length,
    usdMicros: String(jobs.reduce((n, j) => n + j.usdMicros, 0)),
    // As protojson writes it: int64 counts as strings, stoppedBy (proto3
    // optional) only on a partial step, and no step for a version that
    // enriches no one.
    enrichment: l.enrichment
      ? {
          ...l.enrichment.state,
          ...Object.fromEntries(
            Object.entries(l.enrichment.counts).map(([k, v]) => [k, String(v)]),
          ),
          credits: String(l.enrichment.credits),
          publishedAt: l.enrichment.publishedAt,
        }
      : null,
  };
};

const BASE =
  "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/loads" as const;

export const loadRoutes: MockRoute[] = [
  {
    method: "POST",
    path: BASE,
    handle: (req) => {
      const s = ontologyOf(req, true);
      if (!isScope(s)) return s;
      const body = (req.body || {}) as Partial<CreateLoadRequest>;
      const clientToken = body.clientToken || "";
      if (!clientToken || clientToken.length > 128)
        return fail(
          "INVALID_ARGUMENT",
          "clientToken must be 1 to 128 characters",
        );
      advanceLoads(s.o.id);
      const again = state.loads.find(
        (l) => l.ontologyId === s.o.id && l.clientToken === clientToken,
      );
      if (again) return ok({ load: toLoad(again) } satisfies LoadResponse);
      const v = ontologyVersion(s.o.id, body.versionId || "");
      if (!v) return fail("NOT_FOUND", "ontology version not found");
      const def = v.definition as { format?: string; status?: string };
      if (def.format !== "agent_bundle")
        return fail(
          "FAILED_PRECONDITION",
          "only an ontology agent draft can be loaded; generate one",
        );
      if (def.status !== "done")
        return fail(
          "FAILED_PRECONDITION",
          `this draft ended as ${def.status}; only a finished draft can be loaded`,
        );
      const evaluation = ruleEvaluationRefusal(v.definition);
      if (evaluation) return fail("FAILED_PRECONDITION", evaluation);
      const running = jobsOf(s.o.id).find(live);
      if (running && running.kind !== "delete")
        return fail(
          "FAILED_PRECONDITION",
          "a load or extract of this ontology is already queued or running",
        );
      if (monthSpend(s.o.workspaceId) >= MONTH_CAP_MICROS)
        return fail(
          "FAILED_PRECONDITION",
          `the workspace has spent this month's $${(MONTH_CAP_MICROS / 1e6).toFixed(2)} of loads`,
        );
      // A new load stops a delete; the load stays deleting and its delete
      // runs again once the new load ends.
      if (running)
        Object.assign(running, {
          status: "cancelled",
          error: "stopped for a new load; it runs again after",
          finishedAt: now(),
        });
      const l: StoredLoad = {
        id: id("load"),
        workspaceId: s.o.workspaceId,
        ontologyId: s.o.id,
        tenantId: s.o.tenantId,
        versionId: v.id,
        clientToken,
        status: "writing",
        createdAt: now(),
        finishedAt: null,
        decisions: decisionsFor(s.o.id),
      };
      state.loads.unshift(l);
      joinScope(l);
      create(s.o.id, l, "load", "person");
      return ok({ load: toLoad(l) } satisfies LoadResponse);
    },
  },
  {
    method: "GET",
    path: BASE,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      advanceLoads(s.o.id);
      return ok({
        loads: loadsOf(s.o.id).map(toLoad),
      } satisfies ListLoadsResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{id}`,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      advanceLoads(s.o.id);
      const l = loadsOf(s.o.id).find((x) => x.id === req.params.id);
      return l
        ? ok({ load: toLoad(l) } satisfies LoadResponse)
        : fail("NOT_FOUND", "load not found");
    },
  },
  {
    method: "POST",
    path: `${BASE}/{id}:set-current`,
    handle: (req) => {
      const s = ontologyOf(req, true);
      if (!isScope(s)) return s;
      advanceLoads(s.o.id);
      const l = loadsOf(s.o.id).find((x) => x.id === req.params.id);
      if (!l) return fail("NOT_FOUND", "load not found");
      if (l.status !== "ready" && l.status !== "partial")
        return fail(
          "FAILED_PRECONDITION",
          "only a ready or partial load can be made current",
        );
      // Its profiles were deleted when People enrichment was turned off
      // (the WFE's FoundryLoadPurged).
      if (purgedProfiles(l))
        return fail(
          "FAILED_PRECONDITION",
          "this load holds People enrichment profiles the workspace no longer allows; load the version again",
        );
      state.current.set(s.o.id, l.id);
      // The documents its scope holds that it misses are read into it: now,
      // or after the live job.
      joinScope(l);
      if (waiting(l.id).length) {
        if (jobsOf(s.o.id).some(live)) state.owed.add(s.o.id);
        else if (monthSpend(s.o.workspaceId) < MONTH_CAP_MICROS)
          create(s.o.id, l, "extract", "person");
      }
      return ok({ load: toLoad(l) } satisfies LoadResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/{loadId}:read`,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      if (knobs.ontologyDatabaseHidden)
        return fail("PERMISSION_DENIED", "permission denied");
      const reads = readsOf(
        (req.body as Partial<ReadFoundryGraphRequest> | undefined)?.readsJson,
      );
      if (typeof reads === "string") return fail("INVALID_ARGUMENT", reads);
      advanceLoads(s.o.id);
      const l = loadsOf(s.o.id).find((x) => x.id === req.params.loadId);
      if (!l) return fail("NOT_FOUND", "load not found");
      if (l.status === "deleting" || l.status === "delete_failed")
        return fail("FAILED_PRECONDITION", "the load is being deleted");
      const model = rulesOf(l);
      return ok({
        resultJson: JSON.stringify({
          as_of: now().slice(0, 10),
          results: reads.map((r) =>
            answer(model, r, { errors: knobs.readErrors }),
          ),
          stats: { instances: model.facts.size, wall_ms: 1 },
        }),
      } satisfies ReadFoundryGraphResponse);
    },
  },
  {
    method: "DELETE",
    path: `${BASE}/{id}`,
    handle: (req) => {
      const s = ontologyOf(req, true);
      if (!isScope(s)) return s;
      advanceLoads(s.o.id);
      const l = loadsOf(s.o.id).find((x) => x.id === req.params.id);
      if (!l) return fail("NOT_FOUND", "load not found");
      if (currentLoadId(s.o.id) === l.id)
        return fail(
          "FAILED_PRECONDITION",
          "the current load can't be deleted; make another load current first",
        );
      if (
        l.status === "writing" ||
        jobsOf(s.o.id).some((j) => live(j) && j.loadId === l.id)
      )
        return fail(
          "FAILED_PRECONDITION",
          "a job is writing this load; delete it once the job ends",
        );
      if (l.status !== "deleting") l.status = "deleting";
      if (!jobsOf(s.o.id).some(live)) create(s.o.id, l, "delete", "person");
      return ok({ load: toLoad(l) } satisfies LoadResponse);
    },
  },
];
