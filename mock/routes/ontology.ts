import type {
  AgentBundleDefinition,
  OntologyEdit,
  ReviseOntologyVersionRequest,
  GenerateOntologyRequest,
  ListOntologyGenerationsResponse,
  ListOntologyVersionsResponse,
  OntologyGeneration,
  OntologyGenerationResponse,
  OntologyVersion,
  OntologyVersionResponse,
  SetCurrentOntologyVersionRequest,
} from "../../src/shared/wfe";
// A real ontology-agent result (its bench run on an orders fixture: a shop
// database, a catalogue, returns and a warehouse export), its sources renamed
// to the Shop database's tables as Foundry names them, and its rule and
// constraint given fixture evidence, as a finished draft has (the run
// predates evaluation); and a Definition v1 version on the seeded Stripe
// account.
import agentBundle from "../fixtures/agent-bundle.json";
// An agent draft for contracts with the Stripe customers, with a Contract
// type that only documents fill: what the graph mock loads (./graph.ts).
import contractsBundle from "../fixtures/agent-bundle-contracts.json";
import foundryDefinition from "../fixtures/foundry-definition.json";
// The Sales graph: public.people, and version 2 enriching its people.
import { PEOPLE, salesGraphBundle } from "../fixtures/sales-graph";
import { fail, id, now, ok, type MockRoute } from "../http";
import { pruneDecisions } from "../review-decisions";
import { config, onReset } from "../state";
import { routedSets } from "../graph/scope";
import { connectionKind, workspaceConnectors } from "./data";
import {
  ACME_GENERAL,
  documentSets,
  readTableFiles,
  setName,
  textDocumentCount,
} from "./documents";
import {
  enrichmentAllowed,
  enrichmentFlagOn,
  enrichmentUnavailable,
  measureClues,
} from "./enrichment";
import {
  isScope,
  ontologyOf,
  RENEWALS,
  SALES_ONTOLOGY,
  SEED_ONTOLOGY,
} from "./ontologies";
import {
  draftFromTables,
  draftTitle,
  fileTable,
  sourceId,
  type DraftTable,
} from "./ontology-agent";

// An ontology's versions and generations, as the backend serves them
// (api/ontolog*.go): immutable versions numbered
// per ontology, a compare-and-set current pointer, and one live generation
// per ontology that the mock's agent drafts as an agent_bundle from the Data
// area's synced tables (workspaceConnectors).
//
// A generation drafts from the chosen connectors' tables (none with
// `noConnectors`; never an API source's, which is chosen in `eventSources`
// with a mode: read through mappings as its table once it has rows, or by
// extraction as its own set of pieces once it has a text field and a piece
// there, never named in documentSetIds) and records the workspace's
// document sets it was given, those sets included, oldest first: none when
// none was. Each set's table files that have been
// read are tables of their own, recorded as its `documents`; with no table
// at all, the mock's agent drafts the sets' text as Contracts through one
// route. It needs one source: a synced table, a read table file or a set
// holding text. Every version has a scope,
// the sets its loads read (documentSetIds): a generated version's are its
// generation's sets, a revised one's its parent version's scope plus the
// sets its routes name; storing a version keeps the live ones, once each.
// A generation reads at most 20 sets.
// It is queued for `ontologyStepMs`, runs for as long again, then
// succeeds or fails as `ontologyOutcome` says. "retry" fails the first attempt
// in a way Helix retries, as the worker does: queued again with the error and
// a later nextAttemptAt (one step), then a second attempt that succeeds.
// "fail" is the model declining, and "auth" the provider refusing the key,
// worded as the WFE words them for the provider in force (./ai.ts; use
// "auth" with a workspace's own key: the house's reads as unavailable). A
// draft records the drafting model in force. Both
// are test knobs, set with POST /__mock/config and reset with the mock, as is
// `reviseOutcome` "budget": a revision measuring rules on a comparable load
// is refused for its budget (./ontology-revise.ts).

import { aiModel, aiRefusal, draftFailure } from "./ai";
import { currentLoadVersionId } from "./loads";
import { comparableSchema, reviseDefinition } from "./ontology-revise";

type Outcome = "succeed" | "fail" | "retry" | "auth";
type Knobs = {
  ontologyDatabaseHidden?: boolean;
  ontologyStepMs?: number;
  ontologyOutcome?: Outcome;
  // "budget": measuring a revision's rules on a comparable load takes past
  // its budget.
  reviseOutcome?: "budget";
};
const knobs = config as typeof config & Knobs;
const STEP_MS = 1500,
  MAX_TABLES = 50,
  MAX_SETS = 20,
  MAX_GOAL = 2000,
  MODEL = "claude-opus-5-5";

type StoredVersion = OntologyVersion & {
  workspaceId: string;
  ontologyId: string;
  version: number;
  definition: Record<string, unknown>;
  documentSetIds: string[];
};
type StoredGeneration = OntologyGeneration & { ontologyId: string };
type Plan = {
  stepMs: number;
  outcome: Outcome;
  revision?: ReviseOntologyVersionRequest & { overBudget: boolean };
};

const state = {
  versions: [] as StoredVersion[],
  current: new Map<string, string>(),
  generations: [] as StoredGeneration[],
  plans: new Map<string, Plan>(),
};

const at = (iso: string, ms: number) =>
  new Date(Date.parse(iso) + ms).toISOString();

function seed() {
  const ws = "ws_acme",
    ont = SEED_ONTOLOGY,
    user = "usr_ada";
  // Data seeds Acme's Stripe account and Shop database. The agent draft also
  // read returns and shipments, which the database's newest sync no longer
  // has: a version keeps naming the tables its generation drafted from.
  const stripe = ["customers", "invoices", "subscriptions", "charges"].map(
    (table) => ({ connectorId: "conn_acme_stripe", table }),
  );
  const shop = [
    "accounts",
    "orders",
    "order_items",
    "products",
    "returns",
    "shipments",
  ].map((table) => ({ connectorId: "conn_acme_shop", table }));
  const t0 = "2026-09-28T10:00:00.000Z",
    t1 = "2026-10-02T10:00:00.000Z",
    t2 = "2026-10-09T14:20:00.000Z",
    t3 = "2026-10-16T16:40:00.000Z",
    t4 = "2026-10-17T15:20:00.000Z";
  state.generations = [
    {
      id: "gen_seed_4",
      workspaceId: ws,
      ontologyId: ont,
      status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
      goal: "Contracts with our customers: who we signed with and what they are billed.",
      title: "Customer contracts and billing",
      tables: [stripe[0]!, stripe[1]!],
      // It picked a set deleted since: its version reads General only.
      documentSetIds: [ACME_GENERAL, "set_acme_old_board"],
      baseVersionId: "ver_seed_1",
      attempts: 1,
      resultVersionId: "ver_seed_3",
      model: MODEL,
      requestedByUserId: user,
      createdAt: t4,
      startedAt: at(t4, 3000),
      finishedAt: at(t4, 141000),
      nextAttemptAt: t4,
    },
    {
      id: "gen_seed_3",
      workspaceId: ws,
      ontologyId: ont,
      status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
      goal: "Help customer service trace an order from the customer through its shipments and returns.",
      title: "Order tracing through shipments and returns",
      tables: shop,
      baseVersionId: "ver_seed_1",
      attempts: 1,
      resultVersionId: "ver_seed_2",
      model: MODEL,
      requestedByUserId: user,
      createdAt: t3,
      startedAt: at(t3, 4000),
      finishedAt: at(t3, 460000),
      nextAttemptAt: t3,
    },
    {
      id: "gen_seed_2",
      workspaceId: ws,
      ontologyId: ont,
      status: "ONTOLOGY_GENERATION_STATUS_FAILED",
      goal: "Which orders ship late, and why?",
      title: "Late order shipments",
      tables: [shop[1]!, shop[5]!],
      baseVersionId: "ver_seed_1",
      attempts: 1,
      error:
        "none of the tables to generate from are synced any more; sync a connector and try again",
      requestedByUserId: user,
      createdAt: t2,
      startedAt: at(t2, 3000),
      finishedAt: at(t2, 9000),
      nextAttemptAt: t2,
    },
    {
      id: "gen_seed_1",
      workspaceId: ws,
      ontologyId: ont,
      status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
      goal: "Customer billing health",
      // From before generations had titles: the pages name it from its goal.
      tables: stripe,
      attempts: 1,
      resultVersionId: "ver_seed_1",
      model: MODEL,
      requestedByUserId: user,
      createdAt: t1,
      startedAt: at(t1, 5000),
      finishedAt: at(t1, 192000),
      nextAttemptAt: t1,
    },
    // Renewals' newest draft failed.
    {
      id: "gen_renewals_2",
      workspaceId: ws,
      ontologyId: RENEWALS,
      status: "ONTOLOGY_GENERATION_STATUS_FAILED",
      goal: "Which subscriptions renew next quarter, and at what price?",
      title: "Subscription renewals next quarter",
      tables: [stripe[0]!, stripe[2]!],
      documentSetIds: [ACME_GENERAL],
      baseVersionId: "ver_renewals_1",
      attempts: 1,
      error:
        "the agent found no renewal dates in the tables it read; pick a table that holds them and try again",
      requestedByUserId: user,
      createdAt: "2026-10-05T09:15:00.000Z",
      startedAt: "2026-10-05T09:15:03.000Z",
      finishedAt: "2026-10-05T09:16:10.000Z",
      nextAttemptAt: "2026-10-05T09:15:00.000Z",
    },
    {
      id: "gen_renewals_1",
      workspaceId: ws,
      ontologyId: RENEWALS,
      status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
      goal: "Which contracts come up for renewal, and what their customers are billed.",
      title: "Contract renewals",
      tables: [stripe[0]!, stripe[1]!],
      documentSetIds: [ACME_GENERAL],
      attempts: 1,
      resultVersionId: "ver_renewals_1",
      model: MODEL,
      requestedByUserId: user,
      createdAt: t0,
      startedAt: at(t0, 3000),
      finishedAt: at(t0, 120000),
      nextAttemptAt: t0,
    },
  ];
  state.versions = [
    {
      id: "ver_seed_1",
      workspaceId: ws,
      ontologyId: ont,
      version: 1,
      format: "ONTOLOGY_FORMAT_FOUNDRY_V1",
      definition: structuredClone(foundryDefinition),
      source: "ONTOLOGY_SOURCE_GENERATED",
      generationId: "gen_seed_1",
      createdByUserId: user,
      createdAt: at(t1, 192000),
      documentSetIds: [],
    },
    {
      id: "ver_seed_2",
      workspaceId: ws,
      ontologyId: ont,
      version: 2,
      format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
      definition: {
        ...structuredClone(agentBundle),
        // Its rule and constraint, both evaluated.
        rules: { count: 2, evaluated: 2 },
      },
      source: "ONTOLOGY_SOURCE_GENERATED",
      generationId: "gen_seed_3",
      createdByUserId: user,
      createdAt: at(t3, 460000),
      documentSetIds: [],
    },
    {
      id: "ver_seed_3",
      workspaceId: ws,
      ontologyId: ont,
      version: 3,
      format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
      definition: {
        ...structuredClone(contractsBundle),
        // Its four rules and constraint, all evaluated.
        rules: { count: 5, evaluated: 5 },
      },
      source: "ONTOLOGY_SOURCE_GENERATED",
      generationId: "gen_seed_4",
      createdByUserId: user,
      createdAt: at(t4, 141000),
      documentSetIds: [ACME_GENERAL],
    },
    // Renewals reads Acme's General set too, with the same contracts draft:
    // an upload into General fans out to both ontologies once they load.
    {
      id: "ver_renewals_1",
      workspaceId: ws,
      ontologyId: RENEWALS,
      version: 1,
      format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
      definition: {
        ...structuredClone(contractsBundle),
        rules: { count: 5, evaluated: 5 },
      },
      source: "ONTOLOGY_SOURCE_GENERATED",
      generationId: "gen_renewals_1",
      createdByUserId: user,
      createdAt: at(t0, 120000),
      documentSetIds: [ACME_GENERAL],
    },
  ];
  // The Sales graph: version 1 maps public.people; version 2, current, also
  // looks its people up, as the agent suggested with consent on.
  const people = [{ connectorId: "conn_acme_shop", table: "people" }],
    t5 = "2026-10-01T10:00:00.000Z",
    t6 = "2026-10-08T11:00:00.000Z";
  const sales = (n: 1 | 2, createdAt: string, goal: string, title: string) => ({
    generation: {
      id: `gen_sales_${n}`,
      workspaceId: ws,
      ontologyId: SALES_ONTOLOGY,
      status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" as const,
      goal,
      title,
      tables: people,
      ...(n === 2 ? { baseVersionId: "ver_sales_1" } : {}),
      attempts: 1,
      resultVersionId: `ver_sales_${n}`,
      model: MODEL,
      requestedByUserId: user,
      createdAt,
      startedAt: at(createdAt, 3000),
      finishedAt: at(createdAt, 95000),
      nextAttemptAt: createdAt,
    },
    version: {
      id: `ver_sales_${n}`,
      workspaceId: ws,
      ontologyId: SALES_ONTOLOGY,
      version: n,
      format: "ONTOLOGY_FORMAT_AGENT_BUNDLE" as const,
      definition: salesGraphBundle(
        sourceId("conn_acme_shop", "people"),
        n === 2
          ? measureClues(
              PEOPLE.map((r) => ({
                key: r.contact_id,
                name: r.full_name || "",
                email: r.email,
                linkedin: r.linkedin_url,
              })),
            )
          : undefined,
      ),
      source: "ONTOLOGY_SOURCE_GENERATED" as const,
      generationId: `gen_sales_${n}`,
      createdByUserId: user,
      createdAt: at(createdAt, 95000),
      documentSetIds: [],
    },
  });
  const first = sales(
      1,
      t5,
      "Our contacts and their accounts.",
      "Contacts and accounts",
    ),
    second = sales(
      2,
      t6,
      "Our contacts' current titles and where they worked before.",
      "Contacts' titles and work history",
    );
  state.generations.push(second.generation, first.generation);
  state.versions.push(first.version, second.version);
  state.current = new Map([
    [ont, "ver_seed_1"],
    [SALES_ONTOLOGY, "ver_sales_2"],
    [RENEWALS, "ver_renewals_1"],
  ]);
  state.plans.clear();
  delete knobs.ontologyStepMs;
  delete knobs.ontologyOutcome;
  delete knobs.reviseOutcome;
}
onReset(seed);

/** An ontology's version with its definition, for other mocks. */
export function ontologyVersion(
  ontologyId: string,
  versionId: string,
): StoredVersion | undefined {
  return state.versions.find(
    (v) => v.ontologyId === ontologyId && v.id === versionId,
  );
}
/**
 * A version's scope as the WFE stores it: the sets given that are live in
 * the workspace, in their order, once each.
 */
const liveScope = (workspaceId: string, ids: readonly string[]) =>
  [...new Set(ids)].filter((id) =>
    documentSets(workspaceId).some((x) => x.id === id),
  );
/** The document sets a version of the ontology reads; none for no version. */
export const versionScope = (ontologyId: string, versionId: string) =>
  ontologyVersion(ontologyId, versionId)?.documentSetIds || [];
/** An ontology's current version id, or undefined. */
export const currentVersionOf = (ontologyId: string) =>
  state.current.get(ontologyId);

const live = (g: OntologyGeneration) =>
  g.status === "ONTOLOGY_GENERATION_STATUS_QUEUED" ||
  g.status === "ONTOLOGY_GENERATION_STATUS_RUNNING";

/** An ontology is deleted: its live generation fails, as the backend ends it. */
export function generationsEnded(ontologyId: string) {
  for (const g of state.generations)
    if (g.ontologyId === ontologyId && live(g)) {
      advance(g);
      if (live(g))
        Object.assign(g, {
          status: "ONTOLOGY_GENERATION_STATUS_FAILED",
          error: "the ontology was deleted",
          finishedAt: now(),
        });
    }
}

// Moves a generation along its plan: queued, running, then done; a retry
// runs a second time after waiting a step.
function advance(g: StoredGeneration) {
  const plan = state.plans.get(g.id);
  if (!plan || !live(g)) return;
  const step = plan.stepMs,
    elapsed = Date.now() - Date.parse(g.createdAt!);
  const run = (attempts: number, from: number) => {
    if (elapsed >= from && (g.attempts || 0) < attempts)
      Object.assign(g, {
        status: "ONTOLOGY_GENERATION_STATUS_RUNNING",
        attempts,
        startedAt: g.startedAt || at(g.createdAt!, from),
      });
  };
  run(1, step);
  if (elapsed < 2 * step) return;
  let end = 2 * step;
  if (plan.outcome === "retry") {
    // A revision's engine call that a retry may clear (UNAVAILABLE) leaves
    // no result: the generation stays queued for its next attempt.
    if ((g.attempts || 0) < 2)
      Object.assign(g, {
        status: "ONTOLOGY_GENERATION_STATUS_QUEUED",
        error: plan.revision
          ? "the ontology agent could not finish the revision; Helix will retry"
          : "the ontology agent is unavailable; Helix will retry",
        nextAttemptAt: at(g.createdAt!, 3 * step),
      });
    run(2, 3 * step);
    if (elapsed < 4 * step) return;
    end = 4 * step;
  }
  const finishedAt = at(g.createdAt!, end);
  if (plan.outcome === "fail" || plan.outcome === "auth") {
    Object.assign(g, {
      status: "ONTOLOGY_GENERATION_STATUS_FAILED",
      error: draftFailure(
        g.workspaceId!,
        plan.outcome === "fail" ? "refused" : "auth",
      ),
      finishedAt,
    });
    return;
  }
  if (plan.revision && g.revision) {
    const parent = ontologyVersion(g.ontologyId, g.revision.parentVersionId!);
    const loaded = ontologyVersion(
      g.ontologyId,
      currentLoadVersionId(g.ontologyId) || "",
    );
    const result = reviseDefinition(
      parent!.definition,
      plan.revision.edits,
      !loaded || !comparableSchema(parent!.definition, loaded.definition)
        ? "none"
        : plan.revision.overBudget
          ? "over_budget"
          : "comparable",
    );
    // The backend stores refusal evidence, then fails permanently without retry.
    const definition = result.definition;
    delete result.definition;
    g.revision.resultJson = JSON.stringify(result);
    Object.assign(g, {
      status:
        result.status === "refused"
          ? "ONTOLOGY_GENERATION_STATUS_FAILED"
          : "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
      error:
        result.status === "refused"
          ? "the ontology agent refused these edits; see findings"
          : "",
      finishedAt,
    });
    if (result.status === "revised" && !plan.revision.dryRun) {
      const version: StoredVersion = {
        id: id("ver"),
        workspaceId: g.workspaceId!,
        ontologyId: g.ontologyId,
        version:
          Math.max(
            0,
            ...state.versions
              .filter((v) => v.ontologyId === g.ontologyId)
              .map((v) => v.version),
          ) + 1,
        format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
        definition: { ...definition!, parent_version_id: parent!.id },
        source: "ONTOLOGY_SOURCE_MANUAL",
        generationId: g.id,
        createdByUserId: g.requestedByUserId,
        createdAt: finishedAt,
        // Its parent's scope (the revision's sets) and the sets its revised
        // routes name, the live ones.
        documentSetIds: liveScope(g.workspaceId!, [
          ...(g.documentSetIds || []),
          ...routedSets(String(definition!.mappings_toml || "")),
        ]),
      };
      state.versions.push(version);
      g.resultVersionId = version.id;
    }
    return;
  }
  const connectors = workspaceConnectors(g.workspaceId!);
  const scope = liveScope(g.workspaceId!, g.documentSetIds || []);
  // The table files it recorded that are still read and in its sets.
  const files = readTableFiles(g.workspaceId!, scope).filter((f) =>
    (g.documents || []).some((d) => d.id === f.id),
  );
  const picked = [
    ...(g.tables || []).flatMap((t): DraftTable[] => {
      const connector = connectors.find((c) => c.id === t.connectorId),
        table = connector?.tables?.find((x) => x.name === t.table);
      return connector && table ? [{ connector, table }] : [];
    }),
    ...files.map((f) => fileTable({ id: f.setId, name: setName(f.setId) }, f)),
  ];
  const texts = scope
    .filter((id) => textDocumentCount(g.workspaceId!, [id]) > 0)
    .map((id) => ({ id, name: setName(id) }));
  // As the agent generator: nothing left to read fails for good.
  if (!picked.length && !texts.length) {
    Object.assign(g, {
      status: "ONTOLOGY_GENERATION_STATUS_FAILED",
      error: (g.tables || []).length
        ? "none of the tables to generate from are synced any more; sync a connector and try again"
        : "nothing to generate from is left: the files and documents chosen were deleted or moved; choose others and try again",
      finishedAt,
    });
    return;
  }
  const version: StoredVersion = {
    id: id("ver"),
    workspaceId: g.workspaceId!,
    ontologyId: g.ontologyId,
    version:
      Math.max(
        0,
        ...state.versions
          .filter((v) => v.ontologyId === g.ontologyId)
          .map((v) => v.version),
      ) + 1,
    format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
    // With the operators' flag and the owner's consent, the agent sees
    // People enrichment.
    definition: draftFromTables(
      picked,
      g.goal || "",
      texts,
      enrichmentFlagOn() && enrichmentAllowed(g.workspaceId!),
    ),
    source: "ONTOLOGY_SOURCE_GENERATED",
    generationId: g.id,
    createdByUserId: g.requestedByUserId,
    createdAt: finishedAt,
    documentSetIds: liveScope(g.workspaceId!, g.documentSetIds || []),
  };
  state.versions.push(version);
  Object.assign(g, {
    status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
    error: "",
    resultVersionId: version.id,
    model: aiModel(g.workspaceId!, "draft") || MODEL,
    finishedAt,
  });
}

// Lists leave the definition out, as the WFE does.
// Responses carry every field, as the WFE's gateway emits unpopulated ones:
// "" strings, 0, null timestamps and messages. Lists leave the definition out.
const listed = (v: StoredVersion): OntologyVersion => ({
  id: v.id,
  workspaceId: v.workspaceId,
  ontologyId: v.ontologyId,
  version: v.version,
  definition: null,
  source: v.source || "ONTOLOGY_SOURCE_UNSPECIFIED",
  generationId: v.generationId || "",
  createdByUserId: v.createdByUserId || "",
  createdAt: v.createdAt || null,
  isCurrent: state.current.get(v.ontologyId) === v.id,
  format: v.format || "ONTOLOGY_FORMAT_UNSPECIFIED",
  documentSetIds: [...v.documentSetIds],
});
// For a caller who can't query the ontology's database (the
// `ontologyDatabaseHidden` knob), a revised version's draft items go without
// their cards, which Revise measured on its load, as the backend answers.
const full = (v: StoredVersion): OntologyVersion => {
  const definition = structuredClone(v.definition) as AgentBundleDefinition &
    Record<string, unknown>;
  const items = Object.values(definition.draft?.draft?.items || {});
  const hidden =
    !!knobs.ontologyDatabaseHidden && !!definition.parent_version_id;
  const withheld = hidden && items.some((i) => i.card != null);
  if (hidden) for (const i of items) delete i.card;
  return {
    ...listed(v),
    definition,
    ...(withheld ? { evidenceWithheld: true } : {}),
  };
};
// A revision's result comes with GET .../generations/{id} only, and is
// withheld from a caller who can't query the ontology's database.
const generationOut = (
  g: StoredGeneration,
  { listed = false } = {},
): OntologyGeneration => ({
  id: g.id,
  workspaceId: g.workspaceId || "",
  ontologyId: g.ontologyId,
  status: g.status || "ONTOLOGY_GENERATION_STATUS_UNSPECIFIED",
  goal: g.goal || "",
  title: g.title || "",
  tables: (g.tables || []).map((t) => ({ ...t })),
  baseVersionId: g.baseVersionId || "",
  attempts: g.attempts || 0,
  error: g.error || "",
  resultVersionId: g.resultVersionId || "",
  model: g.model || "",
  requestedByUserId: g.requestedByUserId || "",
  createdAt: g.createdAt || null,
  startedAt: g.startedAt || null,
  finishedAt: g.finishedAt || null,
  nextAttemptAt: g.nextAttemptAt || null,
  documentSetIds: [...(g.documentSetIds || [])],
  documents: (g.documents || []).map((d) => ({ ...d })),
  revision: g.revision
    ? {
        parentVersionId: g.revision.parentVersionId || "",
        dryRun: !!g.revision.dryRun,
        ...(listed || !g.revision.resultJson
          ? {}
          : knobs.ontologyDatabaseHidden
            ? { resultWithheld: true }
            : { resultJson: g.revision.resultJson }),
      }
    : null,
});
const limitOf = (query: URLSearchParams) => {
  const n = Number(query.get("limit") || 0);
  return Math.min(n > 0 ? n : 20, 100);
};
const BASE = "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}";

export const ontologyRoutes: MockRoute[] = [
  {
    method: "POST",
    path: `${BASE}/versions/{versionId}:revise`,
    handle: (req) => {
      const scope = ontologyOf(req, true);
      if (!isScope(scope)) return scope;
      const parent = ontologyVersion(scope.o.id, req.params.versionId!);
      if (!parent) return fail("NOT_FOUND", "ontology version not found");
      const inputs = state.generations.find(
        (g) => g.id === parent.generationId,
      );
      if (parent.format !== "ONTOLOGY_FORMAT_AGENT_BUNDLE" || !inputs)
        return fail(
          "FAILED_PRECONDITION",
          "only an agent version with generation inputs can be revised",
        );
      if (parent.definition.status !== "done" || !parent.definition.draft)
        return fail(
          "FAILED_PRECONDITION",
          "only a finished agent draft can be revised",
        );
      const body = req.body as ReviseOntologyVersionRequest;
      if (
        !body ||
        !Array.isArray(body.edits) ||
        !body.edits.length ||
        body.edits.length > 100 ||
        (body.dryRun !== undefined && typeof body.dryRun !== "boolean")
      )
        return fail("INVALID_ARGUMENT", "provide 1 to 100 edits");
      // As the backend reads each edit, after protobuf's JSON: a put or a remove (a
      // oneof, with no unknown fields; unset strings are ""), with distinct
      // item ids of at most 256 bytes and at most 1 MiB of TOML.
      const fields = (v: unknown, allowed: string[]) =>
        !!v &&
        typeof v === "object" &&
        !Array.isArray(v) &&
        Object.entries(v).every(
          ([k, x]) => allowed.includes(k) && typeof x === "string",
        );
      if (
        !body.edits.every((e: unknown) => {
          if (!e || typeof e !== "object") return false;
          const set = Object.keys(e);
          const [kind] = set,
            v = (e as Record<string, unknown>)[kind!];
          return (
            set.length === 1 &&
            (kind === "put"
              ? fields(v, ["itemId", "toml"])
              : kind === "remove" && fields(v, ["itemId"]))
          );
        })
      )
        return fail("INVALID_ARGUMENT", "each edit is a put or a remove");
      const edits = body.edits.map(
        (e): OntologyEdit =>
          e.put
            ? { put: { itemId: e.put.itemId || "", toml: e.put.toml || "" } }
            : { remove: { itemId: e.remove.itemId || "" } },
      );
      const ids = edits.map((e) => (e.put ?? e.remove)!.itemId);
      if (
        ids.some((i) => !i.trim() || Buffer.byteLength(i) > 256) ||
        new Set(ids).size !== ids.length
      )
        return fail(
          "INVALID_ARGUMENT",
          "edits require distinct item ids of at most 256 bytes",
        );
      if (
        edits.reduce(
          (bytes, e) => bytes + Buffer.byteLength(e.put?.toml || ""),
          0,
        ) >
        1 << 20
      )
        return fail("INVALID_ARGUMENT", "edit TOML exceeds 1 MiB");
      // The backend's gate: enrichment edits need the operators' flag.
      if (!enrichmentFlagOn() && ids.some((i) => i.startsWith("enrichment.")))
        return enrichmentUnavailable();
      const mine = state.generations.filter((g) => g.ontologyId === scope.o.id);
      mine.forEach(advance);
      if (mine.some(live))
        return fail(
          "ALREADY_EXISTS",
          "an ontology generation is already queued or running",
        );
      const createdAt = now();
      const g: StoredGeneration = {
        id: id("gen"),
        workspaceId: scope.m.workspaceId,
        ontologyId: scope.o.id,
        status: "ONTOLOGY_GENERATION_STATUS_QUEUED",
        // The backend copies the parent's goal and writes no title; a revision has
        // no base version, its parent is the revision's.
        revision: { parentVersionId: parent.id, dryRun: body.dryRun ?? false },
        goal: inputs.goal,
        tables: structuredClone(inputs.tables || []),
        // A revision reads its parent version's scope, and its table files.
        documentSetIds: [...parent.documentSetIds],
        documents: structuredClone(inputs.documents || []),
        requestedByUserId: req.principal.user.id,
        createdAt,
        nextAttemptAt: createdAt,
      };
      state.generations.unshift(g);
      state.plans.set(g.id, {
        stepMs: knobs.ontologyStepMs ?? STEP_MS,
        outcome: knobs.ontologyOutcome ?? "succeed",
        revision: {
          edits,
          dryRun: body.dryRun ?? false,
          overBudget: knobs.reviseOutcome === "budget",
        },
      });
      return ok({
        generation: generationOut(g),
      } satisfies OntologyGenerationResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/versions`,
    handle: (req) => {
      const scope = ontologyOf(req);
      if (!isScope(scope)) return scope;
      const versions = state.versions
        .filter((v) => v.ontologyId === scope.o.id)
        .sort((a, b) => b.version - a.version)
        .slice(0, limitOf(req.query))
        .map(listed);
      return ok({ versions } satisfies ListOntologyVersionsResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/versions/{id}`,
    handle: (req) => {
      const scope = ontologyOf(req);
      if (!isScope(scope)) return scope;
      const v = ontologyVersion(scope.o.id, req.params.id!);
      return v
        ? ok({ version: full(v) } satisfies OntologyVersionResponse)
        : fail("NOT_FOUND", "ontology version not found");
    },
  },
  {
    method: "POST",
    path: `${BASE}:set-current`,
    handle: (req) => {
      const scope = ontologyOf(req, true);
      if (!isScope(scope)) return scope;
      const body = (req.body ||
        {}) as Partial<SetCurrentOntologyVersionRequest>;
      if (!body.versionId)
        return fail("INVALID_ARGUMENT", "version id is required");
      const v = ontologyVersion(scope.o.id, body.versionId);
      if (!v) return fail("NOT_FOUND", "ontology version not found");
      // Compare-and-set: re-choosing the current version is a no-op.
      const current = state.current.get(scope.o.id) || "",
        expected = body.expectedCurrentVersionId || "";
      if (current !== expected)
        return fail(
          "FAILED_PRECONDITION",
          "the current ontology changed since you loaded it; reload and try again",
        );
      state.current.set(scope.o.id, v.id);
      pruneDecisions(scope.o.id, v);
      return ok({ version: full(v) } satisfies OntologyVersionResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}:generate`,
    handle: (req) => {
      const scope = ontologyOf(req, true);
      if (!isScope(scope)) return scope;
      const { m, o } = scope;
      const unset = aiRefusal(m.workspaceId);
      if (unset) return unset;
      const body = (req.body || {}) as Partial<GenerateOntologyRequest>;
      const goal = (body.goal || "").trim(),
        wanted = body.connectorIds || [],
        none = body.noConnectors === true;
      if ([...goal].length > MAX_GOAL)
        return fail(
          "INVALID_ARGUMENT",
          `goal is longer than ${MAX_GOAL} characters`,
        );
      if (none && wanted.length)
        return fail(
          "INVALID_ARGUMENT",
          "connector_ids must be empty with no_connectors",
        );
      // As the WFE checks: a live generation, the connectors and their
      // tables, then the sets.
      const mine = state.generations.filter((g) => g.ontologyId === o.id);
      mine.forEach(advance);
      if (mine.some(live))
        return fail(
          "ALREADY_EXISTS",
          "an ontology generation is already queued or running",
        );
      const connectors = workspaceConnectors(m.workspaceId);
      const unknown = wanted.find((w) => !connectors.some((c) => c.id === w));
      if (unknown) return fail("NOT_FOUND", `connector ${unknown} not found`);
      const named = connectors.find(
        (c) => c.kind === "events" && wanted.includes(c.id),
      );
      if (named)
        return fail(
          "INVALID_ARGUMENT",
          `connector ${named.id} is an API source: choose it in event_sources, with a mode`,
        );
      const tables = connectors
        .filter(
          (c) =>
            c.kind !== "events" &&
            !none &&
            (!wanted.length || wanted.includes(c.id)),
        )
        .flatMap((c) =>
          (c.tables || []).map((t) => ({ connectorId: c.id, table: t.name })),
        );
      // Each API source chosen, once, with a mode: read through mappings,
      // its table joins the tables once it holds rows (an empty table is no
      // proof of data); by extraction, its own set joins the sets once it
      // has a text field and a piece as a document.
      const picks = body.eventSources || [];
      const sets = documentSets(m.workspaceId),
        extracted: string[] = [];
      for (const [i, pick] of picks.entries()) {
        const c = connectors.find((x) => x.id === pick.connectorId);
        if (!c)
          return fail("NOT_FOUND", `connector ${pick.connectorId} not found`);
        if (c.kind !== "events")
          return fail(
            "INVALID_ARGUMENT",
            `connector ${c.id} is not an API source: choose it in connector_ids`,
          );
        if (picks.findIndex((x) => x.connectorId === c.id) < i)
          return fail(
            "INVALID_ARGUMENT",
            `event_sources names the API source ${c.id} twice`,
          );
        if (pick.mode === "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION") {
          if (!(c.config && "events" in c.config && c.config.events.textField))
            return fail(
              "FAILED_PRECONDITION",
              `the API source ${JSON.stringify(c.name)} has no text field: choose the field that holds each piece's text on its page in Foundry, then wait for its next sync`,
            );
          const own = sets.find(
            (x) =>
              x.connector?.connectorId === c.id &&
              x.connector.stream === "pieces",
          );
          if (!own || !textDocumentCount(m.workspaceId, [own.id]))
            return fail(
              "FAILED_PRECONDITION",
              `the API source ${JSON.stringify(c.name)} has no pieces as documents yet: send some, and wait for its next sync (about a minute)`,
            );
          extracted.push(own.id);
          continue;
        }
        if (pick.mode !== "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED")
          return fail(
            "INVALID_ARGUMENT",
            `choose how this ontology reads the API source ${c.id}: structured mappings or entity extraction`,
          );
        if (!Number(c.tables?.find((t) => t.name === "events")?.rowCount))
          return fail(
            "FAILED_PRECONDITION",
            `the API source ${JSON.stringify(c.name)} has no pieces in its table yet: send some, and wait for its first sync (about a minute)`,
          );
        tables.push({ connectorId: c.id, table: "events" });
      }
      const wantedSets = body.documentSetIds || [];
      const noSet = wantedSets.find((s) => !sets.some((x) => x.id === s));
      if (noSet) return fail("NOT_FOUND", `document set ${noSet} not found`);
      // An API source's set is read only through its extraction pick.
      const pieces = sets.find(
        (x) =>
          wantedSets.includes(x.id) &&
          x.connector &&
          connectionKind(x.connector.connectorId) === "events",
      );
      if (pieces)
        return fail(
          "INVALID_ARGUMENT",
          `document set ${pieces.id} holds the API source ${pieces.connector!.connectorId}'s pieces: choose that source in event_sources with mode extraction`,
        );
      // As the WFE records them: the sets in their order, with the sets of
      // the API sources picked for extraction; none asked for reads none.
      const documentSetIds = sets
        .filter((x) => wantedSets.includes(x.id) || extracted.includes(x.id))
        .map((x) => x.id);
      if (documentSetIds.length > MAX_SETS)
        return fail(
          "INVALID_ARGUMENT",
          `a generation reads at most ${MAX_SETS} document sets; choose fewer`,
        );
      // Each set's table files that have been read, as the WFE records them.
      const documents = readTableFiles(m.workspaceId, documentSetIds).map(
        (f) => ({ id: f.id, setId: f.setId }),
      );
      const count = tables.length + documents.length;
      if (!count && !textDocumentCount(m.workspaceId, documentSetIds))
        return fail(
          "FAILED_PRECONDITION",
          "connect your data or upload documents first: a generation reads at least one synced table, one CSV, TSV, JSON, JSONL or Parquet file that has been read, or one document set holding Markdown, MDX, text or a PDF whose text has been read",
        );
      if (count > MAX_TABLES)
        return fail(
          "INVALID_ARGUMENT",
          `these connectors and sets have ${count} tables (${tables.length} synced, ${documents.length} files) and one generation covers at most ${MAX_TABLES}; choose fewer connectors or sets`,
        );
      const createdAt = now();
      const g: StoredGeneration = {
        id: id("gen"),
        workspaceId: m.workspaceId,
        ontologyId: o.id,
        status: "ONTOLOGY_GENERATION_STATUS_QUEUED",
        ...(goal ? { goal, title: draftTitle(goal) } : {}),
        tables,
        documentSetIds,
        documents,
        baseVersionId: state.current.get(o.id) || "",
        attempts: 0,
        requestedByUserId: req.principal.user.id,
        createdAt,
        nextAttemptAt: createdAt,
      };
      state.generations.unshift(g);
      state.plans.set(g.id, {
        stepMs: knobs.ontologyStepMs ?? STEP_MS,
        outcome: knobs.ontologyOutcome ?? "succeed",
      });
      return ok({
        generation: generationOut(g),
      } satisfies OntologyGenerationResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/generations`,
    handle: (req) => {
      const scope = ontologyOf(req);
      if (!isScope(scope)) return scope;
      const generations = state.generations
        .filter((g) => g.ontologyId === scope.o.id)
        .slice(0, limitOf(req.query));
      generations.forEach(advance);
      return ok({
        generations: generations.map((g) => generationOut(g, { listed: true })),
      } satisfies ListOntologyGenerationsResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/generations/{id}`,
    handle: (req) => {
      const scope = ontologyOf(req);
      if (!isScope(scope)) return scope;
      const g = state.generations.find(
        (x) => x.ontologyId === scope.o.id && x.id === req.params.id,
      );
      if (!g) return fail("NOT_FOUND", "ontology generation not found");
      advance(g);
      return ok({
        generation: generationOut(g),
      } satisfies OntologyGenerationResponse);
    },
  },
];
