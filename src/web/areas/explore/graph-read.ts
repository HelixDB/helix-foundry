import {
  MAX_READS,
  outcomeOf,
  parseEntityResult,
  parseExplainResult,
  parseReadResult,
  predString,
  type EntityRef,
  type LiveViolation,
  type Outcome,
  type Pred,
  type Read,
  type ReadFailure,
} from "../../../shared/foundry-read";
import type {
  OntologyVersion,
  ReadFoundryGraphRequest,
  ReadFoundryGraphResponse,
} from "../../../shared/wfe";
import { api } from "../../api";
import { versionFormat } from "../ontology/format";
import { readAgentBundle, type AgentOntology } from "../ontology/model";
import type { EntityIdentity, Violation } from "./model";
import type { Naming } from "./naming";

// Reads of a load with its version's rules evaluated as they are read
// (POST .../loads/{loadId}:read, src/shared/foundry-read.ts): an entity's
// derived facts, flags and live violations, and why one holds. The WFE runs them through the ontology agent, at the database's
// rate, at most four at once per workspace, so a page asks for them only
// when the load's version has rules, and each answer is cached under the
// load like the database's own reads ([ws, "ontology", o, "read", load,
// ...]), refreshed when a job ends.

/** An entity's identity as the engine names it. */
export const refOf = (i: EntityIdentity): EntityRef => ({
  type: i.type,
  id: "key" in i ? i.key : i.name,
});

/**
 * The identity an EntityRef names: a keyed type of the version takes its
 * id as the key, any other its normalized name. Undefined for a type the
 * version doesn't know: no page can be told apart for it.
 *
 * @example
 * identityOf({ type: "Contract", id: "msa" }, naming); // { type: "Contract", name: "msa" }
 */
export function identityOf(
  ref: EntityRef,
  naming: Naming,
): EntityIdentity | undefined {
  const t = naming.types.get(ref.type);
  if (!t) return undefined;
  return t.key.length
    ? { type: ref.type, key: ref.id }
    : { type: ref.type, name: ref.id };
}

/** The same entity: one type, one id. */
export const sameRef = (a: EntityRef, b: EntityRef) =>
  a.type === b.type && a.id === b.id;

/**
 * The violation the rules find now that a stored rule break is: of its
 * constraint, naming its entities in its order. Undefined when the rules no
 * longer find it, or when it names another entity first (an entity's read
 * lists the violations naming it first): its page then has no proof for it.
 */
export const liveViolationOf = (
  live: readonly LiveViolation[],
  stored: Violation,
): LiveViolation | undefined =>
  live.find(
    (l) =>
      l.stored &&
      l.constraint === stored.constraint &&
      l.entities.length === stored.entities.length &&
      l.entities.every((r, k) =>
        sameRef(r, refOf(stored.entities[k]!.identity)),
      ),
  );

/**
 * Whether a version's rules could derive anything: an agent draft with
 * rules, constraints, relations or link declarations. Anything else has
 * nothing to evaluate, so no page asks the WFE.
 */
export function rulesOf(v?: OntologyVersion | null): AgentOntology | null {
  if (!v || versionFormat(v) !== "agent_bundle") return null;
  const o = readAgentBundle(v.definition).ontology;
  if (!o.ok) return null;
  const { rules, constraints, relations, links } = o.value;
  return rules.length ||
    constraints.length ||
    relations.length ||
    links.some((l) => l.traits.length)
    ? o.value
    : null;
}

/**
 * Why a read (or one pred of it) has no answer, for a person.
 *
 * @example
 * readFailureText({ code: "budget", message: "m", limit: 64 }); // "Too deep to compute live"
 * readFailureText({ code: "too_broad", message: "m", limit: 20000, count: 31200 }); // "Covers too many entities to compute live: 31,200, past 20,000"
 */
export function readFailureText(f: ReadFailure): string {
  switch (f.code) {
    case "budget":
      return "Too deep to compute live";
    case "hub":
      return "An entity it passes through has too many facts to compute live";
    case "too_broad":
      return `Covers too many entities to compute live: ${f.count.toLocaleString("en-US")}, past ${f.limit.toLocaleString("en-US")}`;
    case "unknown_entity":
      return "This load doesn’t hold it";
    case "invalid":
      return `The rules can’t answer this${f.pointer ? ` (at ${f.pointer})` : ""}`;
  }
}

/** A pred as a person reads it: its name, with spaces for underscores. */
export const predLabel = (p: { name: string }) => p.name.replace(/_/g, " ");

/**
 * Runs reads against a load, in one request; each outcome in the reads'
 * order. Throws the WFE's error (ApiError) when it refuses them all.
 */
export async function readGraph(
  ws: string,
  o: string,
  loadId: string,
  reads: Read[],
  signal?: AbortSignal,
): Promise<Outcome<unknown>[]> {
  if (!reads.length || reads.length > MAX_READS)
    throw new Error(`1 to ${MAX_READS} reads go in one request`);
  const enc = encodeURIComponent;
  const res = await api<ReadFoundryGraphResponse>(
    `/v1/workspaces/${enc(ws)}/foundry/ontologies/${enc(o)}/loads/${enc(loadId)}:read`,
    { readsJson: JSON.stringify(reads) } satisfies ReadFoundryGraphRequest,
    "POST",
    undefined,
    { signal },
  );
  const result = parseReadResult(res?.resultJson || "");
  if (!result) throw new Error("The read’s answer is malformed.");
  return result.results;
}

const key = (ws: string, o: string, loadId: string, ...rest: unknown[]) => [
  ws,
  "ontology",
  o,
  "read",
  loadId,
  "rules",
  ...rest,
];

export const graphReadQueries = {
  // An entity's derived facts, flags, live violations and failed preds.
  entity: (
    ws: string,
    o: string,
    loadId: string,
    identity: EntityIdentity,
  ) => ({
    queryKey: key(ws, o, loadId, "entity", identity),
    queryFn: async ({ signal }: { signal?: AbortSignal }) => {
      const [r] = await readGraph(
        ws,
        o,
        loadId,
        [{ entity: refOf(identity) }],
        signal,
      );
      return outcomeOf(r, parseEntityResult);
    },
    retry: false,
    staleTime: 30_000,
  }),
  // Why a pred holds of these args: a proof, premises first.
  explain: (
    ws: string,
    o: string,
    loadId: string,
    pred: Pred,
    args: EntityRef[],
  ) => ({
    queryKey: key(ws, o, loadId, "explain", predString(pred), args),
    queryFn: async ({ signal }: { signal?: AbortSignal }) => {
      const [r] = await readGraph(
        ws,
        o,
        loadId,
        [{ explain: { pred: predString(pred), args } }],
        signal,
      );
      return outcomeOf(r, parseExplainResult);
    },
    retry: false,
    staleTime: 60_000,
  }),
};
