import { parse } from "smol-toml";
import type {
  AgentBundleDefinition,
  AgentDraftItem,
  AgentQuestion,
  AgentRuleCounts,
  AgentTurnStatus,
  Connector,
  FoundryDefinition,
  FoundryDocument,
  FoundryDocumentSet,
  FoundryOntology,
  FoundryEnrichmentStop,
  FoundryEventSourceMode,
  FoundryEventSourcePick,
  FoundryLoad,
  FoundryLoadEnrichment,
  GraphJob,
  JobPhase,
  OntologyGeneration,
  OntologyGenerationDocument,
  OntologyVersion,
  Project,
  Tenant,
} from "../../../shared/wfe";
import { LOCAL } from "../../../shared/deployment";
import { NO_WORKSPACE_SUBSCRIPTION } from "../../../shared/wfe";
import { ApiError } from "../../api";
import type { Status } from "../../kit";
import { formatNumber, plural } from "../../kit/util";
import {
  eventsConfig,
  eventsTable,
  isEventSource,
  isLiveSync,
  piecesSet,
} from "../data/model";
import {
  fileRead,
  readKindOf,
  readsAsText,
  setLabel,
} from "../documents/model";
import { mayBeStarting } from "../explore/database-query";
import { versionFormat } from "./format";
import { EVENT_SOURCE_MODE_WORDS } from "./paths";
import { isLive } from "./queries";

// Reads an ontology version's definition, in either stored format, into what
// the Ontology area shows. Nothing here trusts the definition's shape: every
// field is checked, so a version written by a newer backend or agent still
// renders what it can.
//
// - foundry_v1 is Definition v1: entities on one synced table each, with
//   relationships, SQL rules and constraints.
// - agent_bundle is the ontology agent's draft: ontology.toml and
//   mappings.toml (parsed here), its questions, summary and the draft's items.
//
// It also names a generation, says which databases an ontology can bind to,
// what a load of the ontology's database is doing, where the new-ontology
// wizard stands, and where an ontology's setup is (for its overview).

// ---------------------------------------------------------------- Helpers

type Table = Record<string, unknown>;
const isTable = (v: unknown): v is Table =>
  typeof v === "object" && v !== null && !Array.isArray(v);
const text = (v: unknown) => (typeof v === "string" ? v : "");
const texts = (v: unknown) =>
  Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
const tables = (v: unknown): Table[] =>
  Array.isArray(v) ? v.filter(isTable) : [];
const entries = (v: unknown): [string, Table][] =>
  isTable(v)
    ? Object.entries(v).filter((e): e is [string, Table] => isTable(e[1]))
    : [];

/** A parse that can fail: the error is what the parser said. */
export type Parsed<T> = { ok: true; value: T } | { ok: false; error: string };
function parseToml<T>(source: string, read: (t: Table) => T): Parsed<T> {
  try {
    return { ok: true, value: read(parse(source)) };
  } catch (e) {
    return { ok: false, error: (e as Error).message.split("\n")[0]! };
  }
}

// ---------------------------------------------------------------- Graph

/** Where a type's rows come from: a synced table, or an agent source id. */
export type SourceRef =
  | { kind: "table"; connectorId: string; table: string }
  | { kind: "agent"; sourceId: string; object?: string };

export type Prop = { name: string; type: string; description?: string };

/** One type (or entity) of a version, as the graph and schema draw it. */
export type GraphNode = {
  name: string;
  description: string;
  key: string[];
  props: Prop[];
  sources: SourceRef[];
};
/** One link (or relationship) between two types. */
export type GraphEdge = {
  name: string;
  from: string;
  to: string;
  description: string;
  // "one", "many" (agent links), or a Definition v1 cardinality.
  cardinality: string;
  required: boolean;
  // Definition v1: the property pair it joins on.
  via?: string;
};
export type Graph = { nodes: GraphNode[]; edges: GraphEdge[] };

/** Types, most-linked first, then by name. */
export function byDegree(graph: Graph): GraphNode[] {
  const degree = (n: string) =>
    graph.edges.filter((e) => e.from === n || e.to === n).length;
  return [...graph.nodes].sort(
    (a, b) => degree(b.name) - degree(a.name) || a.name.localeCompare(b.name),
  );
}

// ---------------------------------------------------------------- Definition v1

export type FoundryEntity = {
  name: string;
  description: string;
  source?: { connector_id: string; table: string };
  primary_key: string;
  properties: Prop[];
};
export type FoundryRelationship = {
  name: string;
  from: string;
  to: string;
  from_property: string;
  to_property: string;
  cardinality: string;
};
export type FoundryRule = {
  name: string;
  description: string;
  entity: string;
  expression: string;
};
export type FoundryConstraint = {
  name: string;
  entity: string;
  kind: string;
  properties: string[];
  expression: string;
};
export type FoundryView = {
  entities: FoundryEntity[];
  relationships: FoundryRelationship[];
  rules: FoundryRule[];
  constraints: FoundryConstraint[];
};

/** Definition v1, with every list present and every entry named. */
export function readFoundry(
  def?: Record<string, unknown> | FoundryDefinition | null,
): FoundryView {
  const d = (isTable(def) ? def : {}) as Record<string, unknown>;
  const named = (t: Table) => !!text(t.name);
  return {
    entities: tables(d.entities)
      .filter(named)
      .map((e) => ({
        name: text(e.name),
        description: text(e.description),
        source: isTable(e.source)
          ? {
              connector_id: text(e.source.connector_id),
              table: text(e.source.table),
            }
          : undefined,
        primary_key: text(e.primary_key),
        properties: tables(e.properties)
          .filter(named)
          .map((p) => ({
            name: text(p.name),
            type: text(p.type),
            description: text(p.description),
          })),
      })),
    relationships: tables(d.relationships)
      .filter(named)
      .map((r) => ({
        name: text(r.name),
        from: text(r.from),
        to: text(r.to),
        from_property: text(r.from_property),
        to_property: text(r.to_property),
        cardinality: text(r.cardinality),
      })),
    rules: tables(d.rules)
      .filter(named)
      .map((r) => ({
        name: text(r.name),
        description: text(r.description),
        entity: text(r.entity),
        expression: text(r.expression),
      })),
    constraints: tables(d.constraints)
      .filter(named)
      .map((c) => ({
        name: text(c.name),
        entity: text(c.entity),
        kind: text(c.kind),
        properties: texts(c.properties),
        expression: text(c.expression),
      })),
  };
}

export function foundryGraph(view: FoundryView): Graph {
  return {
    nodes: view.entities.map((e) => ({
      name: e.name,
      description: e.description || "",
      key: e.primary_key ? [e.primary_key] : [],
      props: e.properties,
      sources: e.source?.connector_id
        ? [
            {
              kind: "table",
              connectorId: e.source.connector_id,
              table: e.source.table || "",
            },
          ]
        : [],
    })),
    edges: view.relationships.map((r) => ({
      name: r.name,
      from: r.from,
      to: r.to,
      description: "",
      cardinality: r.cardinality || "",
      required: false,
      via:
        r.from_property && r.to_property
          ? `${r.from}.${r.from_property} = ${r.to}.${r.to_property}`
          : undefined,
    })),
  };
}

// ---------------------------------------------------------------- ontology.toml

export type AgentType = {
  name: string;
  description: string;
  props: Prop[];
  // Empty: identified by its name.
  key: string[];
  // String props that text names it by.
  names: string[];
  // Props whose value, quoted in text, identifies one entity, with how the
  // value is compared ("email", "code", "number" or "exact"), by prop.
  identifiers: { prop: string; kind: string }[];
};
export type AgentLink = {
  name: string;
  from: string;
  to: string;
  description: string;
  cardinality: "one" | "many";
  required: boolean;
  props: Prop[];
  // Declarations: inverse_of, implies, symmetric, transitive, chain.
  traits: string[];
};
export type AgentRelation = {
  name: string;
  description: string;
  traits: string[];
};
export type AgentRule = { name: string; description: string; datalog: string };
export type AgentConstraint = AgentRule & {
  severity: "block" | "warn" | "info";
};
export type AgentOntology = {
  types: AgentType[];
  links: AgentLink[];
  relations: AgentRelation[];
  rules: AgentRule[];
  constraints: AgentConstraint[];
};

const props = (v: unknown): Prop[] =>
  isTable(v)
    ? Object.entries(v).map(([name, type]) => ({ name, type: text(type) }))
    : [];
const traits = (t: Table) => [
  ...(text(t.inverse_of) ? [`inverse of ${t.inverse_of}`] : []),
  ...texts(t.implies).map((l) => `implies ${l}`),
  ...(t.symmetric === true ? ["symmetric"] : []),
  ...(t.transitive === true ? ["transitive"] : []),
  ...(Array.isArray(t.chain) ? t.chain : [])
    .map(texts)
    .filter((c) => c.length)
    .map((c) => `chain ${c.join(" → ")}`),
];

/**
 * ontology.toml's types, links, relations, rules and constraints, in name
 * order (TOML tables have no order the agent relies on).
 *
 * @example
 * const o = parseAgentOntology('version = "draft"\n[types.Order]\ndescription = "A purchase."\nprops = { number = "string" }\nkey = ["number"]\nidentifiers = { number = "code" }');
 * o.ok && o.value.types[0].key; // ["number"]
 * o.ok && o.value.types[0].identifiers; // [{ prop: "number", kind: "code" }]
 */
export function parseAgentOntology(source: string): Parsed<AgentOntology> {
  const sorted = <T extends { name: string }>(list: T[]) =>
    list.sort((a, b) => a.name.localeCompare(b.name));
  return parseToml(source, (doc) => ({
    types: sorted(
      entries(doc.types).map(([name, t]) => ({
        name,
        description: text(t.description),
        props: props(t.props),
        key: texts(t.key),
        names: texts(t.names),
        identifiers: (isTable(t.identifiers)
          ? Object.entries(t.identifiers)
          : []
        )
          .map(([prop, kind]) => ({ prop, kind: text(kind) }))
          .sort((a, b) => a.prop.localeCompare(b.prop)),
      })),
    ),
    links: sorted(
      entries(doc.links).map(([name, l]) => ({
        name,
        from: text(l.from),
        to: text(l.to),
        description: text(l.description),
        cardinality: l.cardinality === "one" ? "one" : "many",
        required: l.required === true,
        props: props(l.props),
        traits: traits(l),
      })),
    ),
    relations: sorted(
      entries(doc.relations).map(([name, r]) => ({
        name,
        description: text(r.description),
        traits: traits(r),
      })),
    ),
    rules: sorted(
      entries(doc.rules).map(([name, r]) => ({
        name,
        description: text(r.description),
        datalog: text(r.datalog).trim(),
      })),
    ),
    constraints: sorted(
      entries(doc.constraints).map(([name, c]) => ({
        name,
        description: text(c.description),
        datalog: text(c.datalog).trim(),
        severity:
          c.severity === "block" || c.severity === "info" ? c.severity : "warn",
      })),
    ),
  }));
}

// ---------------------------------------------------------------- mappings.toml

/** Where a value comes from: a column or JSON Pointer, transformed and parsed. */
export type FieldRef = { field: string; transform: string[]; parse?: string };
export type Identify =
  | { kind: "key"; fields: [string, FieldRef][] }
  | { kind: "lookup"; mapping: string; on: [string, FieldRef][] }
  | { kind: "name"; field: FieldRef };
export type AgentBinding = {
  alias: string;
  type: string;
  identify: Identify;
  props: [string, FieldRef][];
  // create: rows make the entity; report: rows only resolve it.
  missing: "create" | "report";
  optional: boolean;
};
export type AgentRows =
  | { kind: "source"; source: string; object?: string }
  | { kind: "nested"; parent: string; records: string };
export type AgentMapping = {
  name: string;
  rows: AgentRows;
  rowKey: string[];
  links: { link: string; from: string; to: string }[];
  bindings: AgentBinding[];
};

function parseText(p: unknown): string | undefined {
  if (typeof p === "string") return p;
  if (!isTable(p)) return undefined;
  const [kind, spec] = Object.entries(p)[0] || [];
  if (!kind) return undefined;
  if (typeof spec === "string") return `${kind} ${spec}`;
  if (isTable(spec) && kind === "datetime")
    return `datetime ${text(spec.format)}${spec.tz ? ` ${spec.tz}` : ""}`;
  return kind;
}
function fieldRef(v: unknown): FieldRef {
  if (!isTable(v)) return { field: text(v), transform: [] };
  return {
    field: text(v.field),
    transform: texts(v.transform),
    parse: parseText(v.parse),
  };
}
const fieldPairs = (v: unknown): [string, FieldRef][] =>
  isTable(v)
    ? Object.entries(v)
        .map(([k, f]): [string, FieldRef] => [k, fieldRef(f)])
        .sort((a, b) => a[0].localeCompare(b[0]))
    : [];

/**
 * mappings.toml's mappings, in name order, each with its bindings in alias
 * order.
 */
export function parseAgentMappings(source: string): Parsed<AgentMapping[]> {
  return parseToml(source, (doc) =>
    entries(doc.mappings)
      .map(([name, m]): AgentMapping => {
        const object = text(m.table) || text(m.records) || undefined;
        return {
          name,
          rows: text(m.parent)
            ? {
                kind: "nested",
                parent: text(m.parent),
                records: text(m.records),
              }
            : { kind: "source", source: text(m.source), object },
          rowKey: texts(m.row_key),
          links: tables(m.links).map((l) => ({
            link: text(l.link),
            from: text(l.from),
            to: text(l.to),
          })),
          bindings: entries(m.bind)
            .map(([alias, b]): AgentBinding => {
              const identify: Identify = isTable(b.lookup)
                ? {
                    kind: "lookup",
                    mapping: text(b.lookup.mapping),
                    on: fieldPairs(b.lookup.on),
                  }
                : b.name !== undefined && b.key === undefined
                  ? { kind: "name", field: fieldRef(b.name) }
                  : { kind: "key", fields: fieldPairs(b.key) };
              return {
                alias,
                type: text(b.type),
                identify,
                props: fieldPairs(b.props),
                missing: b.missing === "report" ? "report" : "create",
                optional: b.optional === true,
              };
            })
            .sort((a, b) => a.alias.localeCompare(b.alias)),
        };
      })
      .sort((a, b) => a.name.localeCompare(b.name)),
  );
}

/** The source a mapping's rows come from, following nested mappings to their root. */
export function mappingSource(
  mappings: AgentMapping[],
  m: AgentMapping,
): string | undefined {
  const seen = new Set<string>();
  let at: AgentMapping | undefined = m;
  while (at && !seen.has(at.name)) {
    if (at.rows.kind === "source") return at.rows.source;
    seen.add(at.name);
    const parent: string = at.rows.parent;
    at = mappings.find((x) => x.name === parent);
  }
  return undefined;
}

// ---------------------------------------------------------------- Document routes

/** Which documents a route reads: every set, or the sets it names. */
export type DocumentSelector =
  | { kind: "all" }
  | { kind: "sets"; ids: string[] };
/** What a route lets documents do with one type. */
export type RouteType = {
  name: string;
  // Documents may create entities of it; else they only find existing ones.
  create: boolean;
  // Props documents may fill, in name order.
  props: string[];
};
/** One `[extractions.<name>]` of mappings.toml: what documents may fill. */
export type AgentRoute = {
  name: string;
  documents: DocumentSelector;
  types: RouteType[];
  links: string[];
};

/**
 * mappings.toml's document routes, in name order. `documents` is "all" or a
 * list of set ids (deduplicated); anything else reads no set.
 *
 * @example
 * const r = parseAgentRoutes('[extractions.c]\ndocuments = ["set_1", "set_1"]\nlinks = ["signed_with"]\n[extractions.c.types.Contract]\ncreate = true\nprops = ["signed_on"]');
 * r.ok && r.value[0].documents; // { kind: "sets", ids: ["set_1"] }
 * r.ok && r.value[0].types; // [{ name: "Contract", create: true, props: ["signed_on"] }]
 */
export function parseAgentRoutes(source: string): Parsed<AgentRoute[]> {
  return parseToml(source, (doc) =>
    entries(doc.extractions)
      .map(
        ([name, r]): AgentRoute => ({
          name,
          documents:
            r.documents === "all"
              ? { kind: "all" }
              : { kind: "sets", ids: [...new Set(texts(r.documents))] },
          types: entries(r.types)
            .map(([type, t]) => ({
              name: type,
              create: t.create === true,
              props: [...new Set(texts(t.props))].sort(),
            }))
            .sort((a, b) => a.name.localeCompare(b.name)),
          links: [...new Set(texts(r.links))].sort(),
        }),
      )
      .sort((a, b) => a.name.localeCompare(b.name)),
  );
}

// ---------------------------------------------------------------- People enrichment

/**
 * One `[enrichments.<name>]` of mappings.toml: the
 * people of `type` are looked up on context.dev by the props their clues
 * name, and each profile found fills `props` and, with work history, links
 * the person to the employers already in the graph.
 */
export type AgentEnrichment = {
  name: string;
  type: string;
  // The props holding each person's work email and LinkedIn URL.
  clues: { email?: string; linkedinUrl?: string };
  // Target prop ← profile field (./enrichment-fields.ts), in file order.
  props: [string, string][];
  workHistory?: {
    link: string;
    // The `to` type's prop holding a bare lowercase domain.
    match: string;
    // Link prop ← experience field, in file order.
    props: [string, string][];
  };
};

const fieldMap = (v: unknown): [string, string][] =>
  (isTable(v) ? Object.entries(v) : []).filter(
    (e): e is [string, string] => typeof e[1] === "string",
  );

/**
 * mappings.toml's enrichment items, in name order (the engine allows one).
 *
 * @example
 * const e = parseAgentEnrichments('[enrichments.people]\ntype = "Person"\nclues = { email = "email" }\nprops = { country = "location_country" }');
 * e.ok && e.value[0].clues; // { email: "email", linkedinUrl: undefined }
 * e.ok && e.value[0].props; // [["country", "location_country"]]
 */
export function parseAgentEnrichments(
  source: string,
): Parsed<AgentEnrichment[]> {
  return parseToml(source, (doc) =>
    entries(doc.enrichments)
      .map(([name, e]): AgentEnrichment => {
        const clues = isTable(e.clues) ? e.clues : {},
          history = isTable(e.work_history) ? e.work_history : undefined;
        return {
          name,
          type: text(e.type),
          clues: {
            email: text(clues.email) || undefined,
            linkedinUrl: text(clues.linkedin_url) || undefined,
          },
          props: fieldMap(e.props),
          workHistory: history && {
            link: text(history.link),
            match: text(history.match),
            props: fieldMap(history.props),
          },
        };
      })
      .sort((a, b) => a.name.localeCompare(b.name)),
  );
}

/** The clues an item looks people up by, as words: "email, LinkedIn". */
export const cluesText = (e: AgentEnrichment) =>
  [
    ...(e.clues.email ? ["email"] : []),
    ...(e.clues.linkedinUrl ? ["LinkedIn"] : []),
  ].join(", ");

/** The agent's measure of an item's clues (AgentEnrichmentClues), checked. */
export type EnrichmentClues = {
  people: number;
  usable: number;
  shared: number;
  noName: number;
};

/**
 * An item's stored `clues`, or undefined when it has none (a user-added
 * item, or one Revise changed) or they don't add up.
 *
 * @example
 * readEnrichmentClues({ people: 980, usable: 902, email: 870, linkedin_url: 412, no_name: 13, shared: 0, conflicting: 2 }); // { people: 980, usable: 902, shared: 0, noName: 13 }
 * readEnrichmentClues({ people: 2, usable: 3 }); // undefined
 */
export function readEnrichmentClues(v: unknown): EnrichmentClues | undefined {
  if (!isTable(v)) return undefined;
  const count = (x: unknown) =>
    typeof x === "number" && Number.isInteger(x) && x >= 0 ? x : undefined;
  const people = count(v.people),
    usable = count(v.usable),
    shared = count(v.shared),
    noName = count(v.no_name);
  return people !== undefined &&
    usable !== undefined &&
    shared !== undefined &&
    noName !== undefined &&
    usable <= people
    ? { people, usable, shared, noName }
    : undefined;
}

/**
 * How sure the agent is that looking an item's people up pays: High when at
 * least 80% of them have a usable email or LinkedIn URL and none shares one,
 * Medium from 50%, else Low. The percent is rounded, but kept below a
 * threshold the share didn't reach. Unmeasured for an item without clues.
 *
 * @example
 * confidenceText({ people: 980, usable: 902, shared: 0, noName: 13 }); // "Confidence: High · 902 of 980 people have a usable email or LinkedIn (92%) · 0 shared · 13 without a full name"
 * confidenceText({ people: 980, usable: 610, shared: 0, noName: 0 }); // "Confidence: Medium · 610 of 980 usable (62%)"
 * confidenceText({ people: 250, usable: 199, shared: 0, noName: 0 }); // "Confidence: Medium · 199 of 250 usable (79%)"
 * confidenceText(undefined); // "Confidence: not measured. Load to see who can be looked up."
 */
export function confidenceText(c: EnrichmentClues | undefined): string {
  if (!c) return "Confidence: not measured. Load to see who can be looked up.";
  // Rounded, but never up to a threshold the share didn't reach: 79.6% reads
  // 79%, not a Medium 80%.
  const share = c.people ? c.usable / c.people : 0,
    percent = Math.min(
      Math.round(share * 100),
      share < 0.5 ? 49 : share < 0.8 ? 79 : 100,
    );
  if (share >= 0.8 && !c.shared)
    return `Confidence: High · ${formatNumber(c.usable)} of ${formatNumber(c.people)} people have a usable email or LinkedIn (${percent}%) · 0 shared · ${formatNumber(c.noName)} without a full name`;
  return `Confidence: ${share >= 0.5 ? "Medium" : "Low"} · ${formatNumber(c.usable)} of ${formatNumber(c.people)} usable (${percent}%)${c.shared ? ` · ${formatNumber(c.shared)} shared` : ""}`;
}

/** Whether a route reads documents of the set. */
export const routeReads = (r: AgentRoute, setId: string) =>
  r.documents.kind === "all" || r.documents.ids.includes(setId);

/**
 * The routes that read documents of a set: none means a document in it is
 * stored, not read.
 *
 * @example
 * routesReading([{ name: "c", documents: { kind: "all" }, types: [], links: [] }], "set_1").length; // 1
 */
export const routesReading = (routes: AgentRoute[], setId: string) =>
  routes.filter((r) => routeReads(r, setId));

// ---------------------------------------------------------------- Document scope

/** The most document sets one generation reads (the WFE refuses more). */
export const MAX_GENERATION_SETS = 20;

/**
 * How many of MAX_GENERATION_SETS a generation has left for the sets
 * people choose, once each API source of `connectors` that `eventSources`
 * reads by extraction has taken one for its own set; below 0 when they take
 * more than all. A chosen source `connectors` lacks (gone) takes none, as
 * generationSources drops it.
 *
 * @example
 * setRoom(new Map([["e1", "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION"]]), [{ id: "e1", kind: "events" }]); // 19
 * setRoom(new Map([["e1", "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED"]]), [{ id: "e1", kind: "events" }]); // 20
 */
export const setRoom = (
  eventSources: ReadonlyMap<string, FoundryEventSourceMode> | null,
  connectors: readonly Connector[] | undefined,
) =>
  MAX_GENERATION_SETS -
  (connectors || []).filter(
    (c) =>
      isEventSource(c.kind) &&
      eventSources?.get(c.id) === "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION",
  ).length;

/** The documents of a set the agent reads (the WFE stages the newest). */
export const DOCUMENTS_PER_SET = 5;

/**
 * Whether a set is an API source's own (its Pieces, kind events): a
 * generation reads it only through that source's extraction pick, and the
 * WFE refuses it in documentSetIds (INVALID_ARGUMENT), so no set choice
 * lists or keeps it.
 *
 * @example
 * isEventSourceSet({ id: "p", connector: { connectorId: "e1", connectorKind: "events", stream: "pieces" } }); // true
 * isEventSourceSet({ id: "n", connector: { connectorId: "c", connectorKind: "granola" } }); // false
 */
export const isEventSourceSet = (s: FoundryDocumentSet) =>
  isEventSource(s.connector?.connectorKind || "");

/**
 * The document sets Generate starts with: those a `?sets=` link names (an
 * earlier generation's, "" for none), else the current version's scope,
 * none without a current version. Ids of sets the workspace no longer has
 * are dropped, as are API sources' own sets (isEventSourceSet: their
 * sources' extraction picks read them, startingEventSources), and only the
 * first `max` (in the sets' order) are kept.
 *
 * @example
 * startingSets(null, ["s1", "gone"], [{ id: "s1" }, { id: "s2" }]); // ["s1"]
 * startingSets("s2", ["s1"], [{ id: "s1" }, { id: "s2" }]); // ["s2"]
 * startingSets("s1,s2", undefined, [{ id: "s1" }, { id: "s2" }], 1); // ["s1"]
 */
export function startingSets(
  prefill: string | null,
  scope: readonly string[] | undefined,
  sets: readonly FoundryDocumentSet[],
  max = MAX_GENERATION_SETS,
): string[] {
  const wanted =
    prefill === null ? scope || [] : prefill.split(",").filter(Boolean);
  return sets
    .filter((s) => wanted.includes(s.id) && !isEventSourceSet(s))
    .slice(0, max)
    .map((s) => s.id);
}

/** A connection's own document sets, as Generate's picker groups them. */
export type OwnedSets = {
  connectorId: string;
  // As its page names it: its name, else the sets' word for it, else its
  // kind's label.
  name: string;
  // Its kind, once the connectors are read: "" until then.
  kind: string;
  sets: FoundryDocumentSet[];
};

/**
 * The document sets as Generate's picker lists them: those people made
 * (`made`), then each connection's own under it (`owned`), the
 * connections in the order of their first set, and every set in the order
 * shown (`ordered`: Select all picks the first MAX_GENERATION_SETS). An API
 * source's own set is never listed (isEventSourceSet): the API sources'
 * picker chooses it, as the source read by extraction. `label` names a
 * kind.
 *
 * @example
 * pickerSets(
 *   [{ id: "n", name: "Notes", connector: { connectorId: "c", connectorName: "Team" } }, { id: "g", name: "General" }],
 *   [],
 * ).ordered.map((s) => s.id); // ["g", "n"]
 */
export function pickerSets(
  all: readonly FoundryDocumentSet[],
  connectors: readonly Connector[],
  label: (kind: string) => string = (kind) => kind,
): {
  made: FoundryDocumentSet[];
  owned: OwnedSets[];
  ordered: FoundryDocumentSet[];
} {
  const sets = all.filter((s) => !isEventSourceSet(s)),
    made = sets.filter((s) => !s.connector);
  const owned = [
    ...new Set(sets.flatMap((s) => s.connector?.connectorId || [])),
  ].map((connectorId): OwnedSets => {
    const own = sets.filter((s) => s.connector?.connectorId === connectorId),
      c = connectors.find((x) => x.id === connectorId);
    return {
      connectorId,
      name:
        c?.name ||
        own[0]?.connector?.connectorName ||
        (c ? label(c.kind) : "A connection"),
      kind: c?.kind || "",
      sets: own,
    };
  });
  return { made, owned, ordered: [...made, ...owned.flatMap((o) => o.sets)] };
}

/**
 * A version's scope by set name, in the workspace's set order, a
 * connector's set with its connection ("Granola / Notes"); a set it names
 * that was deleted since is said as such, once.
 *
 * @example
 * scopeNames(["s2", "gone"], [{ id: "s1", name: "General" }, { id: "s2", name: "Board" }]); // ["Board", "a deleted set"]
 */
export function scopeNames(
  ids: readonly string[],
  sets: readonly FoundryDocumentSet[],
): string[] {
  const known = sets.filter((s) => ids.includes(s.id));
  const gone = new Set(ids).size - known.length;
  return [
    ...known.map(setLabel),
    ...(gone === 1
      ? ["a deleted set"]
      : gone > 1
        ? [`${gone} deleted sets`]
        : []),
  ];
}

/** A table file a version's mappings read that a load can't any more. */
export type MissingFile = { id: string; name?: string };

/**
 * What a load of a version reads of the workspace's documents.
 * - Text (Markdown, MDX, text, and PDFs whose text is read) in its scope
 *   (`total`, with the other PDFs): those its routes read for what they
 *   hold (`routed`), those it only stores (`stored`), and how many the
 *   current load hasn't read (`unread`: `read` holds the ids it has).
 *   `documents` are the uploads; a connector's sets (`owned`) are counted
 *   by set: each of their documents is text, and the current load has
 *   read `read` of a set's.
 * - PDFs in its scope whose text is still being read (`reading`), read by
 *   the load once it is, and those whose text couldn't be read (`unreadable`:
 *   a scanned PDF has none), which the load records as such.
 * - Table files join no load: its mappings read them as tables. Of those
 *   its generation drafted from (`recorded`), the ones its mappings read
 *   (`mapped`, source ids) that are no longer uploaded, read and in its
 *   scope are `missing`: the load fails until the version is revised or the
 *   file is back.
 *
 * @example
 * loadDocumentPlan(
 *   [{ id: "d1", setId: "g", status: "uploaded" }, { id: "d2", setId: "b", status: "uploaded" }],
 *   ["g"],
 *   [],
 *   new Set(),
 * ); // { total: 1, routed: 0, stored: 1, reading: 0, unreadable: 0, unread: 1, missing: [] }
 */
export function loadDocumentPlan(
  documents: readonly FoundryDocument[],
  scope: readonly string[],
  routes: AgentRoute[],
  read: ReadonlySet<string>,
  mapped: readonly string[] = [],
  recorded: readonly OntologyGenerationDocument[] = [],
  owned: readonly { id: string; documents: number; read: number }[] = [],
) {
  const inScope = documents.filter(
    (d) => d.status === "uploaded" && scope.includes(d.setId || ""),
  );
  const sets = owned.filter((s) => scope.includes(s.id)),
    count = (list: typeof sets) => list.reduce((n, s) => n + s.documents, 0),
    ownedRouted = count(
      sets.filter((s) => routesReading(routes, s.id).length > 0),
    ),
    ownedUnread = sets.reduce(
      (n, s) => n + Math.max(0, s.documents - s.read),
      0,
    );
  const loaded = inScope.filter((d) => readKindOf(d) !== "table");
  // A type this page doesn't know may be read: no claim either way.
  const reads = loaded.filter((d) => readKindOf(d) !== "pdf" || readsAsText(d)),
    reading = loaded.filter(
      (d) => readKindOf(d) === "pdf" && fileRead(d).state === "reading",
    ).length;
  const routed = reads.filter(
    (d) => routesReading(routes, d.setId || "").length > 0,
  ).length;
  const missing = recorded.flatMap((r): MissingFile[] => {
    if (!r.id || !mapped.includes(r.id)) return [];
    const d = documents.find((x) => x.id === r.id);
    return d && inScope.includes(d) && fileRead(d).state === "ready"
      ? []
      : [{ id: r.id, name: d?.name }];
  });
  return {
    total: loaded.length + count(sets),
    routed: routed + ownedRouted,
    stored: reads.length - routed + count(sets) - ownedRouted,
    reading,
    unreadable: loaded.length - reads.length - reading,
    unread: reads.filter((d) => !read.has(d.id)).length + ownedUnread,
    missing,
  };
}

/**
 * What a load reads of the documents in its scope (`scope`, the sets'
 * names), as `loadDocumentPlan` counts them, for its dialog. Reading the
 * same text with the same models again replays what earlier loads paid
 * for, so once the current load has read every routed document, reading
 * them again costs nothing. But the models are told about each person with
 * the profiles a load applies, so the documents that mention people are
 * read again, and paid for, whenever those change:
 * when the version enriches people (`enriches`), and when it doesn't but
 * the current load's did (`current.enrichment`, set for a load whose
 * version enriches), as after People enrichment is turned off. `current` is
 * the ontology's current load: null without one, undefined while it isn't
 * known, when nothing is said of what reading again costs.
 *
 * @example
 * loadDocumentsText({ routed: 2, stored: 0, reading: 0, unreadable: 0, unread: 0 }, "General", false, {});
 * // "From General: 2 documents, read for the names and facts they hold; reading the same text with the same models again costs nothing. Documents added later are read as they arrive."
 */
export function loadDocumentsText(
  plan: Pick<
    ReturnType<typeof loadDocumentPlan>,
    "routed" | "stored" | "reading" | "unreadable" | "unread"
  >,
  scope: string,
  enriches: boolean,
  current: Pick<FoundryLoad, "enrichment"> | null | undefined,
): string {
  const { routed, stored, reading, unreadable, unread } = plan;
  const replay =
    enriches || current?.enrichment
      ? "; those that mention people are read again, and paid for, whenever the applied profiles change"
      : unread || current === undefined
        ? ""
        : "; reading the same text with the same models again costs nothing";
  return [
    `From ${scope}: ${routed ? `${plural(routed, "document")}, read for the names and facts they hold${replay}.` : "none read."}`,
    ...(stored
      ? [
          `${plural(stored, "document")} in sets no route reads ${stored === 1 ? "is" : "are"} stored, not read.`,
        ]
      : []),
    ...(reading
      ? [
          `${plural(reading, "PDF")} ${reading === 1 ? "is" : "are"} still being read: the load reads ${reading === 1 ? "it" : "them"} once ${reading === 1 ? "its" : "their"} text is.`,
        ]
      : []),
    ...(unreadable
      ? [
          `${plural(unreadable, "PDF")} ${unreadable === 1 ? "has" : "have"} no text Foundry could read, such as a scanned one.`,
        ]
      : []),
    "Documents added later are read as they arrive.",
  ].join(" ");
}

/**
 * How an ontology's current version reads a table file of set `setId`:
 * - mapped: its mappings name the file, so its loads load the file's rows;
 * - unmapped: it reads the set (its scope), but no mapping names the file.
 *   `revise`: its generation drafted from the file, so a revision can map
 *   it; otherwise only a new generation reads it;
 * - none: it neither maps the file nor reads its set (a Definition v1
 *   version maps no file).
 */
export type FileMapping =
  | { kind: "mapped" }
  | { kind: "unmapped"; revise: boolean }
  | { kind: "none" };

/**
 * How `version`, drafted by `generation` (once read), reads the table file
 * `documentId` in set `setId`.
 *
 * @example
 * fileMapping("doc_1", "g", { id: "v", documentSetIds: ["g"] }); // { kind: "unmapped", revise: false }
 * fileMapping("doc_1", "b", { id: "v", documentSetIds: ["g"] }); // { kind: "none" }
 */
export function fileMapping(
  documentId: string,
  setId: string,
  version: OntologyVersion,
  generation?: OntologyGeneration,
): FileMapping {
  const bundle =
    versionFormat(version) === "agent_bundle"
      ? readAgentBundle(version.definition)
      : undefined;
  if (
    bundle?.mappings.ok &&
    loadSources(bundle.mappings.value).includes(documentId)
  )
    return { kind: "mapped" };
  if (!(version.documentSetIds || []).includes(setId)) return { kind: "none" };
  return {
    kind: "unmapped",
    revise:
      !!bundle && !!generation?.documents?.some((x) => x.id === documentId),
  };
}

// ---------------------------------------------------------------- Sources matrix

/**
 * A column of the Sources matrix: one source's mappings (by the table their
 * rows come from), or one document route.
 */
export type SourceColumn =
  | { kind: "table"; sourceId: string; mappings: string[] }
  | { kind: "route"; route: string; documents: DocumentSelector };
/** What a column does with a type, and the props it fills. */
export type SourceCell = {
  role: "creates" | "looks_up" | "resolves";
  props: string[];
};
/**
 * Per type and link, what each source fills (the source
 * view, derived, never stored): table bindings and document routes.
 */
export type SourceMatrix = {
  columns: SourceColumn[];
  types: {
    name: string;
    cells: (SourceCell | null)[];
    // Some column creates it: without one, nothing ever makes one.
    created: boolean;
  }[];
  links: { name: string; cells: boolean[] }[];
};

const union = (lists: string[][]) => [...new Set(lists.flat())].sort();

/**
 * The Sources matrix: a column per source table (its mappings, nested ones
 * under their root's source, in mapping order) then per route; a row per
 * type and per link.
 *
 * @example
 * const m = sourceMatrix(
 *   { types: [{ name: "Contract", description: "", props: [], key: [], names: [], identifiers: [] }], links: [], relations: [], rules: [], constraints: [] },
 *   [],
 *   [{ name: "contracts", documents: { kind: "all" }, types: [{ name: "Contract", create: true, props: ["signed_on"] }], links: [] }],
 * );
 * m.types[0].cells; // [{ role: "creates", props: ["signed_on"] }]
 */
export function sourceMatrix(
  ontology: AgentOntology,
  mappings: AgentMapping[],
  routes: AgentRoute[],
): SourceMatrix {
  const tables = new Map<string, AgentMapping[]>();
  for (const m of mappings) {
    const source = mappingSource(mappings, m) || "";
    tables.set(source, [...(tables.get(source) || []), m]);
  }
  const columns: SourceColumn[] = [
    ...[...tables].map(
      ([sourceId, list]): SourceColumn => ({
        kind: "table",
        sourceId,
        mappings: list.map((m) => m.name),
      }),
    ),
    ...routes.map(
      (r): SourceColumn => ({
        kind: "route",
        route: r.name,
        documents: r.documents,
      }),
    ),
  ];
  const tableCell = (list: AgentMapping[], type: string): SourceCell | null => {
    const binds = list
      .flatMap((m) => m.bindings)
      .filter((b) => b.type === type);
    if (!binds.length) return null;
    return {
      role: binds.some((b) => b.missing === "create") ? "creates" : "looks_up",
      props: union(
        binds.map((b) => [
          ...b.props.map(([p]) => p),
          ...(b.identify.kind === "key"
            ? b.identify.fields.map(([p]) => p)
            : []),
        ]),
      ),
    };
  };
  const cells = (type: string) =>
    [...tables.values()]
      .map((list) => tableCell(list, type))
      .concat(
        routes.map((r) => {
          const t = r.types.find((x) => x.name === type);
          return t
            ? { role: t.create ? "creates" : "resolves", props: t.props }
            : null;
        }),
      );
  return {
    columns,
    types: ontology.types.map((t) => {
      const row = cells(t.name);
      return {
        name: t.name,
        cells: row,
        created: row.some((c) => c?.role === "creates"),
      };
    }),
    links: ontology.links.map((l) => ({
      name: l.name,
      cells: [
        ...[...tables.values()].map((list) =>
          list.some((m) => m.links.some((x) => x.link === l.name)),
        ),
        ...routes.map((r) => r.links.includes(l.name)),
      ],
    })),
  };
}

// ---------------------------------------------------------------- agent_bundle

export type CompetencyQuestion = {
  id: string;
  question: string;
  path: string[];
};
export type Gap = { id: string; description: string };

export type AgentBundle = {
  status?: AgentTurnStatus;
  summary: string;
  questions: AgentQuestion[];
  usage: NonNullable<AgentBundleDefinition["usage"]>;
  ontologyToml: string;
  mappingsToml: string;
  ontology: Parsed<AgentOntology>;
  mappings: Parsed<AgentMapping[]>;
  // mappings.toml's document routes: what each document set may fill.
  routes: Parsed<AgentRoute[]>;
  // mappings.toml's People enrichment item, if it has one.
  enrichments: Parsed<AgentEnrichment[]>;
  // The draft's items by id ("type.Order"), for their rationale.
  items: Record<string, AgentDraftItem>;
  competency: CompetencyQuestion[];
  gaps: Gap[];
  leftOut: { what: string; reason: string }[];
};

const STATUSES: AgentTurnStatus[] = ["done", "needs_input", "budget_exhausted"];

/** An agent_bundle definition, with both files parsed. */
export function readAgentBundle(
  given?: Record<string, unknown> | null,
): AgentBundle {
  const def: Table = isTable(given) ? given : {};
  const draftView = isTable(def.draft) ? def.draft : {},
    draft = isTable(draftView.draft) ? draftView.draft : {};
  const items: Record<string, AgentDraftItem> = Object.fromEntries(
    entries(draft.items)
      .filter(([, i]) => typeof i.toml === "string")
      .map(([id, i]) => [id, i as unknown as AgentDraftItem]),
  );
  const ontologyToml = text(def.ontology_toml) || text(draftView.ontology_toml),
    mappingsToml = text(def.mappings_toml) || text(draftView.mappings_toml);
  const questions = (
    Array.isArray(def.questions) ? def.questions : tables(draft.questions)
  )
    .filter((q): q is Table => isTable(q) && !!text(q.text))
    .map((q) => ({
      id: text(q.id),
      items: texts(q.items),
      text: text(q.text),
      options: texts(q.options),
      default: text(q.default) || null,
      blocking: q.blocking === true,
    }));
  // Competency questions and gaps are review data, in no file: read them
  // from their items' snippets ("[cq.CQ1]", "[gap.G1]").
  const review = <T extends { id: string }>(
    kind: "cq" | "gap",
    read: (id: string, t: Table) => T,
  ) =>
    Object.entries(items)
      .filter(([id]) => id.startsWith(kind + "."))
      .flatMap(([id, item]) => {
        const name = id.slice(kind.length + 1);
        const parsed = parseToml(item.toml, (doc) => {
          const own = isTable(doc[kind]) ? doc[kind][name] : undefined;
          return isTable(own) ? read(name, own) : undefined;
        });
        return parsed.ok && parsed.value ? [parsed.value] : [];
      })
      .sort((a, b) => a.id.localeCompare(b.id, undefined, { numeric: true }));
  const usage = isTable(def.usage) ? def.usage : {};
  const num = (v: unknown) => (typeof v === "number" ? v : 0);
  return {
    status: STATUSES.find((s) => s === def.status),
    summary: text(def.summary),
    questions,
    usage: {
      input: num(usage.input),
      cache_read: num(usage.cache_read),
      cache_write: num(usage.cache_write),
      output: num(usage.output),
      usd: num(usage.usd),
    },
    ontologyToml,
    mappingsToml,
    ontology: parseAgentOntology(ontologyToml),
    mappings: parseAgentMappings(mappingsToml),
    routes: parseAgentRoutes(mappingsToml),
    enrichments: parseAgentEnrichments(mappingsToml),
    items,
    competency: review("cq", (id, t) => ({
      id,
      question: text(t.question),
      path: texts(t.path),
    })),
    gaps: review("gap", (id, t) => ({ id, description: text(t.description) })),
    leftOut: tables(draft.left_out)
      .map((l) => ({ what: text(l.what), reason: text(l.reason) }))
      .filter((l) => l.what),
  };
}

export function agentGraph(bundle: AgentBundle): Graph {
  const ontology = bundle.ontology.ok ? bundle.ontology.value : undefined,
    mappings = bundle.mappings.ok ? bundle.mappings.value : [];
  // A type's sources are the mappings whose rows create it.
  const sourcesOf = (type: string): SourceRef[] => {
    const refs = mappings.flatMap((m) => {
      if (!m.bindings.some((b) => b.type === type && b.missing === "create"))
        return [];
      const source = mappingSource(mappings, m);
      return source
        ? [
            {
              kind: "agent" as const,
              sourceId: source,
              object:
                m.rows.kind === "nested"
                  ? m.rows.records
                  : m.rows.object || undefined,
            },
          ]
        : [];
    });
    return refs.filter(
      (r, i) =>
        refs.findIndex(
          (x) => x.sourceId === r.sourceId && x.object === r.object,
        ) === i,
    );
  };
  return {
    nodes: (ontology?.types || []).map((t) => ({
      name: t.name,
      description: t.description,
      key: t.key,
      props: t.props,
      sources: sourcesOf(t.name),
    })),
    edges: (ontology?.links || []).map((l) => ({
      name: l.name,
      from: l.from,
      to: l.to,
      description: l.description,
      cardinality: l.cardinality,
      required: l.required,
    })),
  };
}

// ---------------------------------------------------------------- Sources

/**
 * The agent's id for a synced table: "src_" and the first 8 bytes of
 * SHA-256(connector id, NUL, table), in hex, as the backend's
 * ontologyagent generator names sources.
 */
export async function agentSourceId(connectorId: string, table: string) {
  const bytes = new TextEncoder().encode(`${connectorId}\0${table}`);
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  return (
    "src_" +
    [...digest.slice(0, 8)].map((b) => b.toString(16).padStart(2, "0")).join("")
  );
}

/** "src_… file.email" (a left-out field): the source id and what follows. */
export function splitSourceText(what: string) {
  const m = /^(src_[0-9a-f]{16})(?:\s+(.*))?$/.exec(what);
  return m
    ? { sourceId: m[1]!, rest: (m[2] || "").replace(/^file\./, "") }
    : { sourceId: undefined, rest: what };
}

/**
 * A field as read: the column or pointer, its transforms, then its parse.
 *
 * @example
 * fieldText({ field: "sku", transform: ["upper"] }); // "sku → upper"
 */
export function fieldText(f: FieldRef) {
  return [
    f.field,
    ...f.transform,
    ...(f.parse ? [`parse ${f.parse}`] : []),
  ].join(" → ");
}

/** A link name as words: "placed_by" reads "placed by". */
export const linkWords = (name: string) => name.replace(/_/g, " ");

// ---------------------------------------------------------------- Generations

/**
 * The most characters a generation's title has: the WFE's own cap, which
 * generationTitle holds its titles to as well, since a model writes them.
 */
export const TITLE_MAX = 60;

/**
 * `text` whole when it has at most TITLE_MAX characters (not UTF-16 units),
 * else TITLE_MAX - 1 at most and an ellipsis: up to the last space within
 * TITLE_MAX characters, without the punctuation that leaves at the end, or a
 * hard cut inside one long word.
 */
function clipTitle(text: string) {
  const chars = [...text];
  if (chars.length <= TITLE_MAX) return text;
  const head = chars.slice(0, TITLE_MAX).join(""),
    space = head.lastIndexOf(" ");
  const kept =
    space > 0 ? head.slice(0, space) : chars.slice(0, TITLE_MAX - 1).join("");
  return kept.replace(/[\s,;:\-–—]+$/u, "") + "…";
}

/**
 * A title for a generation without one (from before titles, or from a WFE
 * without them): the goal's first sentence, or its first line when that ends
 * sooner, without a list marker or a closing full stop, colon, semicolon or
 * comma, cut at a word boundary to TITLE_MAX characters with an ellipsis. A
 * line ending in a colon only leads into the next, so the first line that
 * doesn't is read. "" for an empty goal.
 *
 * @example
 * titleFromGoal("Which orders ship late, and why? Group them by carrier."); // "Which orders ship late, and why?"
 * titleFromGoal("- Contracts we signed\n- What they bill"); // "Contracts we signed"
 * titleFromGoal("Goal:\nTrace orders"); // "Trace orders"
 */
export function titleFromGoal(goal: string): string {
  const lines = goal
    .split("\n")
    .map((l) => l.replace(/^\s*(?:[-*+#>]+|\d+[.)])(?:\s+|$)/, "").trim())
    .filter(Boolean);
  const line = lines.find((l) => !l.endsWith(":")) ?? lines[0] ?? "";
  // A sentence ends at ! or ? after a word of two letters or more, or a
  // closing bracket or quote. A . there ends one only before a capital or the
  // line's end, and not after an abbreviation: "e.g.", "1.", "etc. for" and
  // "vs. Shopify" don't end one.
  const end = [
    ...line.matchAll(
      /(?:\p{L}{2}|[)\]"'”’])(?:[!?](?=\s|$)|\.(?=\s+["“‘(]?\p{Lu}|$))/gu,
    ),
  ].find(
    (m) =>
      !m[0].endsWith(".") ||
      !/\b(?:vs|etc|inc|ltd|co|corp|dr|mrs?|ms|st|approx|incl)$/i.test(
        line.slice(0, m.index + m[0].length - 1),
      ),
  );
  return clipTitle(
    (end ? line.slice(0, end.index + end[0].length) : line)
      .replace(/[.:;,]+$/, "")
      .replace(/\s+/g, " "),
  );
}

/**
 * What a generation is called wherever it shows. A revision (the backend writes it no
 * title, and its parent's goal) is named by what it does. Any other is named
 * by the WFE's title on one line, cut to TITLE_MAX characters should it break
 * that contract, else one from its goal (titleFromGoal), else `untitled`, by
 * default what a generation without a goal does ("" where it is better left
 * unnamed).
 *
 * @example
 * generationTitle({ title: "Late shipments", goal: "Which orders ship late?" }); // "Late shipments"
 * generationTitle({ title: "", goal: "Which orders ship late?" }); // "Which orders ship late?"
 * generationTitle({ revision: { dryRun: true }, goal: "Which orders ship late?" }); // "Preview edits"
 * generationTitle({ revision: { dryRun: false } }); // "Edited version"
 * generationTitle({}); // "An ontology from your data"
 * generationTitle({}, ""); // ""
 */
export const generationTitle = (
  g: Pick<OntologyGeneration, "title" | "goal" | "revision">,
  untitled = "An ontology from your data",
) =>
  g.revision
    ? g.revision.dryRun
      ? "Preview edits"
      : "Edited version"
    : clipTitle(g.title?.replace(/\s+/g, " ").trim() || "") ||
      titleFromGoal(g.goal || "") ||
      untitled;

/**
 * One row of an ontology's schema versions: a stored version, with the
 * generation that drafted it when the generations list has it, or a
 * generation that failed before storing one.
 */
export type VersionRow =
  | {
      kind: "version";
      version: OntologyVersion;
      generation: OntologyGeneration | undefined;
    }
  | { kind: "failed"; generation: OntologyGeneration };

/**
 * An ontology's versions and its failed generations as one list, newest
 * first. A version is matched to its generation by `generationId`. A preview
 * (a dry-run revision) stores nothing and a live generation has nothing to
 * list yet, so neither is a row: the page's callout shows the live one until
 * its version is stored. A row without `createdAt` sorts last, and a tie
 * keeps versions first.
 *
 * @example
 * versionRows(
 *   [{ id: "v1", createdAt: "2026-10-01T00:00:00Z", generationId: "g1" }],
 *   [
 *     { id: "g2", status: "ONTOLOGY_GENERATION_STATUS_FAILED", createdAt: "2026-10-02T00:00:00Z" },
 *     { id: "g1", status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED", resultVersionId: "v1" },
 *   ],
 * ).map((r) => r.kind); // ["failed", "version"]
 */
export function versionRows(
  versions: OntologyVersion[],
  generations: OntologyGeneration[],
): VersionRow[] {
  const generation = new Map(generations.map((g) => [g.id, g])),
    at = (r: VersionRow) =>
      Date.parse(
        (r.kind === "version" ? r.version : r.generation).createdAt || "",
      ) || 0;
  return [
    ...versions.map(
      (version): VersionRow => ({
        kind: "version",
        version,
        generation: generation.get(version.generationId || ""),
      }),
    ),
    ...generations
      .filter(
        (g) =>
          g.status === "ONTOLOGY_GENERATION_STATUS_FAILED" &&
          !g.revision?.dryRun,
      )
      .map((g): VersionRow => ({ kind: "failed", generation: g })),
  ].sort((a, b) => at(b) - at(a));
}

// ---------------------------------------------------------------- Loads

// The stamp the site is built for (next.config.ts): prod's load caps are
// half dev's.
const PROD = process.env.NEXT_PUBLIC_APP_ENV?.trim().toLowerCase() === "prod";

/**
 * What one load may spend and hold: the backend's limits (foundry.graph's
 * max_job_usd and monthly_usd, $10 and $50 on prod, $20 and $100 on dev),
 * shown before a load starts.
 * No route serves them.
 */
export const LOAD_LIMITS = {
  jobUsd: PROD ? 10 : 20,
  monthUsd: PROD ? 50 : 100,
  documents: 200,
  // Each document's cap (foundry.documents.max_bytes).
  documentBytes: 1_048_576,
} as const;

/**
 * What a load that stopped at a spending cap (budget_exhausted) says of it:
 * the job's own error when it has one, which names the cap it reached (an
 * API source's allowance names the source, and says its newest pieces wait
 * for next month or a load a person starts), else the caps it may have
 * reached, with what it read and what waits.
 *
 * @example
 * budgetStopText({ id: "j", status: "budget_exhausted", error: "the load spent its $20.00 cap" });
 * // "The load spent its $20.00 cap."
 */
export function budgetStopText(j: GraphJob): string {
  if (j.error) return j.error.charAt(0).toUpperCase() + j.error.slice(1) + ".";
  return `It read ${plural(j.progress?.documentsDone || 0, "document")} before reaching a spending cap ($${LOAD_LIMITS.jobUsd} a load, $${LOAD_LIMITS.monthUsd} a workspace a month). What it read is in the database; the documents it didn’t reach are read once it is current and the month has budget.`;
}

/**
 * Whether a version can be loaded: only an agent draft whose status is done
 * (its mappings were checked to the end). Otherwise, why not.
 *
 * @example
 * loadRefusal("agent_bundle", "needs_input"); // "The agent is waiting for answers to its questions."
 */
export function loadRefusal(
  format: "foundry_v1" | "agent_bundle",
  status: AgentTurnStatus | undefined,
): string | undefined {
  if (format !== "agent_bundle")
    return "Only an agent draft can be loaded. Generate one from your tables.";
  if (status === "needs_input")
    return "The agent is waiting for answers to its questions.";
  if (status === "budget_exhausted")
    return "The agent stopped before finishing this draft.";
  return status === "done" ? undefined : "This draft isn’t finished.";
}

/** Read the engine's uint32 counts. Missing or inconsistent counts cannot permit a load. */
export function readRuleCounts(value: unknown): AgentRuleCounts | undefined {
  if (!isTable(value)) return undefined;
  const { count, evaluated } = value;
  if (
    typeof count !== "number" ||
    !Number.isInteger(count) ||
    count < 0 ||
    count > 0xffffffff ||
    typeof evaluated !== "number" ||
    !Number.isInteger(evaluated) ||
    evaluated < 0 ||
    evaluated > count
  )
    return undefined;
  return { count, evaluated };
}

/** The backend requires every executable item to have an accepted engine evaluation. */
export function ruleEvaluationRefusal(
  definition?: Record<string, unknown> | null,
): string | undefined {
  if (definition?.format !== "agent_bundle") return undefined;
  const rules = readRuleCounts(definition.rules);
  if (!rules || rules.evaluated !== rules.count)
    return "This version is not loadable until its rules are evaluated.";
}

/** The tables a draft's mappings read, by source id, each once. */
export const loadSources = (mappings: AgentMapping[]) => [
  ...new Set(
    mappings.flatMap((m) => (m.rows.kind === "source" ? [m.rows.source] : [])),
  ),
];

const phases: JobPhase[] = ["provisioning", "ingest", "documents", "finishing"];
const phaseLabels: Record<JobPhase, string> = {
  provisioning: "Preparing",
  ingest: "Loading tables",
  documents: "Reading documents",
  finishing: "Finishing",
  deleting: "Deleting",
};
/**
 * What a job does, in words: a load reads the tables then the documents, an
 * extract (queued by an upload or by making a load current) only the
 * documents its load hasn't read, a delete removes the load.
 */
export const kindLabel = (j: GraphJob) =>
  j.kind === "extract"
    ? "New documents"
    : j.kind === "delete"
      ? "Delete"
      : "Tables and documents";

/** The step a job is at, in words. */
export const phaseLabel = (j: GraphJob) =>
  phaseLabels[j.phase || "provisioning"] || "Preparing";

/**
 * How many jobs a load has had and what they spent (millionths of a dollar).
 * The WFE lists a load's newest 20 jobs and gives the totals beside them; a
 * response without the totals is summed from its list.
 */
export const jobTotals = (l: FoundryLoad) => ({
  count: l.jobCount ?? l.jobs?.length ?? 0,
  usdMicros: Number(
    l.usdMicros ??
      (l.jobs || []).reduce((n, j) => n + Number(j.usdMicros || 0), 0),
  ),
});

/**
 * Whether a load's job is looking people up: live at its first step while
 * its People enrichment step is queued or running. The job may read queued
 * then, parked until the lookups end. (A `preparing` step is the job finding
 * its people, before it parks.)
 */
export const lookingUp = (j: GraphJob, e?: FoundryLoadEnrichment | null) =>
  (j.status === "queued" || j.status === "running") &&
  (j.phase || "provisioning") === "provisioning" &&
  (e?.status === "queued" || e?.status === "running");

/**
 * A day of the month in the reader's words ("Nov 1"), in UTC: credits are
 * charged by the calendar month in UTC, so the cap resets at its start.
 */
export const utcDay = (d: Date) =>
  d.toLocaleDateString(undefined, {
    day: "numeric",
    month: "short",
    timeZone: "UTC",
  });

// An int64 count of a load's People enrichment step; 0 when unset.
const count = (x?: string) => Number(x || 0);

// Why a partial step left people for the next load, by `stoppedBy`: every
// reason this build knows. A reason from a newer WFE reads as stopped early.
const WAIT_REASONS: Record<FoundryEnrichmentStop, string> = {
  cap: "this month’s credit cap was reached",
  provider_credits: "context.dev stopped answering",
  provider: "context.dev stopped answering",
  time: "the time limit was reached",
  busy: "lookups were busy",
  stopped: "the lookups were stopped",
  pro: "the workspace isn’t on Foundry Pro",
  flag: "Helix paused lookups",
};

/**
 * Whether a load stopped waiting for its People enrichment step (past its
 * deadline and the grace): the load applied only profiles found before,
 * and what the step finds (`found`) is for the next load. The WFE doesn't
 * say so. A step still queued or running tells it while the load's job is
 * past its first step or has ended (its load job past the newest 20 jobs
 * ended long before). One that published tells it only once it published
 * after the load ended: one that published while the load went on reads as
 * on time, since neither the times nor the counts tell it apart (an ingest
 * whose report didn't count the profiles leaves `applied` 0 too).
 *
 * @example
 * stoppedWaiting({ id: "l", finishedAt: "2026-10-20T12:00:00Z", enrichment: { status: "done", publishedAt: "2026-10-20T13:00:00Z" } }); // true
 * stoppedWaiting({ id: "l", jobs: [{ id: "j", kind: "load", status: "running", phase: "ingest" }], enrichment: { status: "running" } }); // true
 */
export function stoppedWaiting(l: FoundryLoad): boolean {
  const e = l.enrichment;
  if (e?.status === "queued" || e?.status === "running") {
    const job = l.jobs?.find((x) => x.kind === "load");
    return job ? !lookingUp(job, e) : !!l.jobs?.length;
  }
  return Date.parse(e?.publishedAt || "") > Date.parse(l.finishedAt || "");
}

/**
 * When a load's People enrichment profiles were looked up, for their
 * evidence ("profiles as of 20 Oct"), or "" when the load can't say. A step
 * publishes every profile it applies with its own time, those it reused from
 * an earlier load included, and nothing is refreshed, so the time dates
 * the profiles only when the step's lookups found them all: it found some
 * and reused no earlier result (`carried` 0), and the load waited for it.
 *
 * @example
 * profilesDate({ id: "l", finishedAt: "2026-10-20T15:00:00Z", enrichment: { status: "done", found: "4", carried: "0", publishedAt: "2026-10-20T14:00:00Z" } }); // "20 Oct", in the reader's locale
 * profilesDate({ id: "l", finishedAt: "2026-12-01T15:00:00Z", enrichment: { status: "done", found: "0", carried: "4", publishedAt: "2026-12-01T14:00:00Z" } }); // ""
 */
export function profilesDate(l: FoundryLoad): string {
  const e = l.enrichment;
  return e && count(e.found) > 0 && count(e.carried) === 0 && !stoppedWaiting(l)
    ? shortDate(e.publishedAt)
    : "";
}

/**
 * A load's People enrichment step in words: finding its people, waiting for
 * a lookup slot, how far its lookups got, or how they ended, each
 * `stoppedBy` with its own reason. "" for a status this build doesn't know.
 * A cap's reset is the first of the month after the step published (or
 * `now`). A stopped step can't say who stopped it ("Stopped by Ada
 * Lovelace after 380"): v1FoundryLoadEnrichment carries no user. When the
 * load stopped waiting for it (`late`, stoppedWaiting), every profile the
 * load applied was found before: a step still queued or running says so
 * without its counts, and one that published found its profiles for the
 * next load.
 *
 * @example
 * enrichmentStepText({ status: "running", toLookUp: "612", lookedUp: "380", found: "104" }); // "380 of 612 looked up · 104 found"
 * enrichmentStepText({ status: "done", lookedUp: "612", found: "172", held: "38", applied: "409", unmatched: "3", credits: "4240" }); // "612 looked up · 172 found (38 held) · 240 found before · 4,240 credits"
 * enrichmentStepText({ status: "partial", stoppedBy: "time", lookedUp: "3600" }); // "Time limit reached after 3,600; the rest next load"
 * enrichmentStepText({ status: "partial", stoppedBy: "busy", applied: "238", unmatched: "2" }); // "Lookups were busy; 240 profiles found before applied, the rest next load"
 * enrichmentStepText({ status: "done", lookedUp: "612", found: "172", held: "38", applied: "240", credits: "4240" }, Date.now(), true); // "612 looked up · 172 found (38 held) for the next load · 240 found before · 4,240 credits"
 * enrichmentStepText({ status: "running", lookedUp: "380", found: "104", applied: "240" }, Date.now(), true); // "Still looking up after the load stopped waiting; 240 profiles found before applied, the rest next load"
 */
export function enrichmentStepText(
  e: FoundryLoadEnrichment,
  now = Date.now(),
  late = false,
): string {
  const n = (x?: string | number) => formatNumber(Number(x || 0)),
    looked = count(e.lookedUp),
    planned = count(e.toLookUp);
  // Profiles found before that the load applied: known once its tables are
  // in (a step that looked no one up found none of its own). Until then, and
  // with none, the step doesn't count them.
  const before =
      count(e.applied) + count(e.unmatched) - (late ? 0 : count(e.found)),
    foundBefore =
      before > 0
        ? `${plural(before, "profile")} found before applied`
        : "profiles found before applied";
  if (late && (e.status === "queued" || e.status === "running"))
    return `Still looking up after the load stopped waiting; ${foundBefore}, the rest next load`;
  switch (e.status) {
    case "preparing":
      return "Finding the people to look up";
    case "queued":
      return "Waiting for a lookup slot (up to 5 min)";
    case "running":
      return `${n(looked)} of ${n(planned)} looked up · ${n(e.found)} found`;
    case "done":
      return `${n(looked)} looked up · ${n(e.found)} found${count(e.held) ? ` (${n(e.held)} held)` : ""}${late ? " for the next load" : ""}${before > 0 ? ` · ${n(before)} found before` : ""} · ${n(e.credits)} credits`;
    case "off":
      return "Off for this workspace: no profiles applied";
    case "failed":
      return "Lookups failed: profiles found before applied";
    case "cancelled":
      return "Cancelled: no profiles published";
    case "partial":
      break;
    default:
      return "";
  }
  switch (e.stoppedBy) {
    case "cap": {
      const from = new Date(Date.parse(e.publishedAt || "") || now);
      const reset = new Date(
        Date.UTC(from.getUTCFullYear(), from.getUTCMonth() + 1, 1),
      );
      return `${n(looked)} of ${n(planned)} looked up · credit cap reached; ${n(planned - looked)} wait until it resets on ${utcDay(reset)}`;
    }
    case "provider_credits":
    case "provider":
      return `context.dev stopped answering after ${n(looked)}; the rest next load`;
    case "time":
      return `Time limit reached after ${n(looked)}; the rest next load`;
    case "busy":
      return `Lookups were busy; ${foundBefore}, the rest next load`;
    case "stopped":
      return `Stopped after ${n(looked)}; the rest next load`;
    case "pro":
      return `Not on Foundry Pro: ${foundBefore}`;
    case "flag":
      return "Lookups paused by Helix: profiles found before applied";
    default:
      return `${n(looked)} of ${n(planned)} looked up; the rest next load`;
  }
}

/**
 * What a load's People enrichment applied, for its page once its tables are
 * in: its profiles, new and found before, how many applied (a profile whose
 * person changed since the lookup isn't), and for a partial step who waits
 * for the next load and why; `cap` when the credit cap stopped it. A step
 * that applied nothing says how it ended. Undefined while it runs, unless
 * the load stopped waiting for it (`late`, stoppedWaiting): then it says the
 * load applied the profiles found before. The profiles are what the load
 * applied, bound or not (`carried` counts every earlier result reused, not
 * found included). A step that published `late` applied only profiles found
 * before, and says its own apply from the next load.
 *
 * @example
 * enrichmentOutcome({ status: "done", found: "172", carried: "272", applied: "409", unmatched: "3" })?.text; // "412 profiles (172 new, 240 found before) · 409 applied, 3 not: the person changed since the lookup."
 * enrichmentOutcome({ status: "done", found: "172", applied: "240" }, Date.now(), true)?.text; // "240 profiles (0 new, 240 found before) · 240 applied. 172 profiles found after the load stopped waiting apply from the next load."
 */
export function enrichmentOutcome(
  e: FoundryLoadEnrichment,
  now = Date.now(),
  late = false,
): { text: string; cap: boolean } | undefined {
  const looking = e.status === "queued" || e.status === "running";
  if (!e.status || e.status === "preparing" || (looking && !late))
    return undefined;
  if (e.status !== "done" && e.status !== "partial") {
    const text = enrichmentStepText(e, now, late);
    return text ? { text: `${text}.`, cap: false } : undefined;
  }
  const n = (x?: string | number) => formatNumber(Number(x || 0)),
    found = count(e.found),
    applied = count(e.applied) + count(e.unmatched),
    fresh = late ? 0 : found,
    profiles = Math.max(fresh, applied),
    waiting = count(e.toLookUp) - count(e.lookedUp),
    partial = e.status === "partial" && waiting > 0;
  return {
    text:
      `${plural(profiles, "profile")} (${n(fresh)} new, ${n(profiles - fresh)} found before) · ${n(e.applied)} applied${count(e.unmatched) ? `, ${n(e.unmatched)} not: the person changed since the lookup` : ""}.` +
      (late && found
        ? ` ${plural(found, "profile")} found after the load stopped waiting apply from the next load.`
        : "") +
      (partial
        ? ` ${waiting === 1 ? "1 person waits" : `${n(waiting)} people wait`} for the next load: ${(e.stoppedBy && WAIT_REASONS[e.stoppedBy]) || "the lookups stopped early"}.`
        : ""),
    cap: partial && e.stoppedBy === "cap",
  };
}

/**
 * The People enrichment step of the version's newest load that has one
 * (`loads` newest first), for the version's pages: who it could look up
 * (everyone with a full name and a usable clue) and the new profiles, or
 * why it looked no one up. Undefined while that step runs, or with none: an
 * older load's counts never stand in for it.
 *
 * @example
 * lastLoadText([{ versionId: "v", enrichment: { status: "done", people: "980", skipped: "56", found: "172" } }], "v"); // "Last load: 924 of 980 could be looked up · 172 found"
 */
export function lastLoadText(
  loads: readonly FoundryLoad[],
  versionId: string,
): string | undefined {
  const e = loads.find(
    (l) => l.versionId === versionId && l.enrichment,
  )?.enrichment;
  switch (e?.status) {
    case "done":
    case "partial":
      return `Last load: ${formatNumber(count(e.people) - count(e.skipped))} of ${formatNumber(count(e.people))} could be looked up · ${formatNumber(count(e.found))} found`;
    case "off":
      return "Last load: off for this workspace, so no profiles applied";
    case "failed":
      return "Last load: lookups failed, so profiles found before applied";
    case "cancelled":
      return "Last load: cancelled, so no profiles published";
    default:
      return undefined;
  }
}

/** A job as a status badge; one looking people up says so. */
export function jobBadge(
  j?: GraphJob | null,
  e?: FoundryLoadEnrichment | null,
): {
  status: Status;
  label: string;
} {
  if (j && lookingUp(j, e))
    return { status: "running", label: "Looking up people" };
  switch (j?.status) {
    case "queued":
      return { status: "queued", label: "Queued" };
    case "running":
      return { status: "running", label: phaseLabel(j) };
    case "succeeded":
      return {
        status: "succeeded",
        label: j.kind === "delete" ? "Deleted" : "Loaded",
      };
    case "budget_exhausted":
      return { status: "stale", label: "Stopped at the cap" };
    case "failed":
      return { status: "failed", label: "Failed" };
    case "cancelled":
      return { status: "canceled", label: "Cancelled" };
    default:
      return { status: "draft", label: "Unknown" };
  }
}

/**
 * A load waiting for Helix to try it again (queued, with the worker's words
 * for its last attempt): the new database still starting, when the attempt
 * failed at the database within the database's first minutes
 * (mayBeStarting, from its ontology's creation); otherwise why it failed.
 * The worker words a write the gateway refused as starting
 * (rate_limit_unavailable) as any refused write, so the words alone can't
 * tell; the window can.
 */
export type LoadRetry =
  | { kind: "starting" }
  | { kind: "failed"; reason: string };

// The worker's words for an attempt the database refused. Its "database
// can't be reached" is about Helix's own records of the database (it never
// asks the gateway), so it is not the database starting.
const AT_THE_DATABASE = /rate_limit_unavailable|database refused a write/i;

/**
 * What a load waiting to be tried again says; null for one that isn't.
 *
 * @example
 * loadRetry({ id: "j", status: "queued", error: "the database refused a write; Helix will retry" }, createdAt, now); // { kind: "starting" } in the window
 */
export function loadRetry(
  j: GraphJob,
  ontologyCreatedAt: string | null | undefined,
  now: number,
): LoadRetry | null {
  if (j.status !== "queued" || !j.error) return null;
  return AT_THE_DATABASE.test(j.error) && mayBeStarting(ontologyCreatedAt, now)
    ? { kind: "starting" }
    : { kind: "failed", reason: j.error };
}

/** A load as a status badge: written, ready, or going. */
export function loadBadge(l?: FoundryLoad | null): {
  status: Status;
  label: string;
} {
  switch (l?.status) {
    case "writing":
      return { status: "running", label: "Writing" };
    case "ready":
      return { status: "succeeded", label: "Ready" };
    case "partial":
      return { status: "stale", label: "Partial" };
    case "failed":
      return { status: "failed", label: "Failed" };
    case "deleting":
      return { status: "paused", label: "Deleting" };
    case "delete_failed":
      return { status: "failed", label: "Delete failed" };
    default:
      return { status: "draft", label: "Unknown" };
  }
}

/**
 * A load's version in a picker or a row: "v3", from the graph's
 * "foundry-v3"; anything else as it is.
 *
 * @example
 * loadVersion({ version: "foundry-v3" }); // "v3"
 */
export const loadVersion = (l: { version?: string }) => {
  const n = /(\d+)$/.exec(l.version || "")?.[1];
  return n ? `v${n}` : l.version || "v?";
};

/** What may be done to a load, by its status and whether it is current. */
export function loadActions(l: FoundryLoad): {
  setCurrent: boolean;
  remove: boolean;
  retryDelete: boolean;
} {
  const busy = !!l.jobs?.some(
    (j) => j.status === "queued" || j.status === "running",
  );
  return {
    setCurrent:
      !l.current && !busy && (l.status === "ready" || l.status === "partial"),
    remove:
      !l.current &&
      !busy &&
      (l.status === "ready" || l.status === "partial" || l.status === "failed"),
    retryDelete: !l.current && !busy && l.status === "delete_failed",
  };
}

// ---------------------------------------------------------------- Choosing a load

/**
 * The load a page reads, from its `?load=`: the named load, Other data
 * ("other"), or by default the current load, else the newest one while none
 * is current (even one still being written). With no load at all there is
 * nothing to read but Other data, which only `?load=other` asks for: the
 * pages say to load the ontology instead. A load the list doesn't hold
 * (deleted, or never this ontology's) is missing.
 */
export type LoadChoice =
  | { kind: "load"; load: FoundryLoad; explicit: boolean }
  | { kind: "other" }
  | { kind: "missing"; id: string }
  | { kind: "none" };

/**
 * @example
 * chooseLoad([], undefined, null); // { kind: "none" }
 * chooseLoad([], undefined, "other"); // { kind: "other" }
 */
export function chooseLoad(
  loads: readonly FoundryLoad[],
  currentId: string | undefined,
  param: string | null,
): LoadChoice {
  if (param === "other") return { kind: "other" };
  const [newest] = loads;
  if (!newest) return { kind: "none" };
  if (param) {
    const load = loads.find((l) => l.id === param);
    return load
      ? { kind: "load", load, explicit: true }
      : { kind: "missing", id: param };
  }
  const load = loads.find((l) => l.id === currentId) ?? newest;
  return { kind: "load", load, explicit: false };
}

/** Whether a load can be read from a picker: it finished writing and isn't going. */
export const isPickable = (l: FoundryLoad) =>
  l.status === "ready" || l.status === "partial" || l.status === "failed";

/** A day in the reader's words ("Oct 18"), local; "" for no date. */
export const shortDate = (iso?: string | null) => {
  const d = iso ? new Date(iso) : null;
  return d && !Number.isNaN(d.getTime())
    ? d.toLocaleDateString(undefined, { day: "numeric", month: "short" })
    : "";
};

/**
 * A load in a picker: "Current · v3" for the current one, else its version,
 * date and anything amiss ("v3 · 18 Oct · partial"); one being written or
 * deleted says how far it got.
 */
export function loadOptionLabel(l: FoundryLoad): string {
  if (l.current) return `Current · ${loadVersion(l)}`;
  const parts = [loadVersion(l), shortDate(l.createdAt)].filter(Boolean);
  const live = l.jobs?.find(
    (j) => j.status === "queued" || j.status === "running",
  );
  const p = live?.progress;
  switch (l.status) {
    case "writing":
      parts.push(
        p?.documentsTotal
          ? `writing, ${p.documentsDone || 0} of ${p.documentsTotal} documents`
          : "writing",
      );
      break;
    case "deleting":
      parts.push("deleting");
      break;
    case "partial":
    case "failed":
      parts.push(l.status);
      break;
    case "delete_failed":
      parts.push("delete failed");
      break;
  }
  return parts.join(" · ");
}

/**
 * The labels some load's version names (its types and links), for telling
 * Other data from the loads' under them; versions are immutable, so each is
 * read once.
 */
export function versionLabels(
  namings: readonly {
    types: ReadonlyMap<string, unknown>;
    links: ReadonlySet<string>;
  }[],
): { nodes: Set<string>; edges: Set<string> } {
  return {
    nodes: new Set(namings.flatMap((n) => [...n.types.keys()])),
    edges: new Set(namings.flatMap((n) => [...n.links])),
  };
}

/**
 * A version as a load's Explorer needs it: its types, most linked first
 * (the hubs a first view starts from), and its links with their ends.
 * Either format; one that doesn't parse has none.
 */
export function versionLayout(v?: OntologyVersion | null): {
  types: string[];
  links: { name: string; from: string; to: string }[];
} {
  if (!v) return { types: [], links: [] };
  let graph: Graph;
  if (versionFormat(v) === "agent_bundle") {
    const bundle = readAgentBundle(v.definition);
    if (!bundle.ontology.ok) return { types: [], links: [] };
    graph = agentGraph(bundle);
  } else graph = foundryGraph(readFoundry(v.definition));
  return {
    types: byDegree(graph).map((n) => n.name),
    links: graph.edges.map((e) => ({ name: e.name, from: e.from, to: e.to })),
  };
}

/**
 * A path with the page's `?load=` (and nothing else of its query) carried
 * over, so the views a page links to read the same load. No param: the
 * default (the current load) needs none.
 *
 * @example
 * withLoad("/explore/o/records?type=A", "L1"); // "/explore/o/records?type=A&load=L1"
 */
export function withLoad(path: string, load: string | null): string {
  if (!load) return path;
  const [base, hash = ""] = path.split("#");
  return `${base}${base!.includes("?") ? "&" : "?"}load=${encodeURIComponent(load)}${hash ? `#${hash}` : ""}`;
}

/** How long a load ran (or has run): "42s", "3m 5s"; "" before it started. */
export function jobDuration(j: GraphJob, now: number) {
  if (!j.startedAt) return "";
  const s = Math.max(
    0,
    Math.round(
      ((j.finishedAt ? Date.parse(j.finishedAt) : now) -
        Date.parse(j.startedAt)) /
        1000,
    ),
  );
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
}

export type LoadStep = { label: string; status: "pending" | "active" | "done" };
/**
 * A load's steps for its data feed: picked up, the database (or, for a
 * version with People enrichment, looking people up), the tables, the
 * documents, then finishing; an extract skips the first two. A job that
 * stopped shows where. A job its ended lookups woke reads queued at its
 * first step until the graph worker picks it up again: it has started, and
 * the step shows how the lookups ended.
 */
export function loadSteps(
  j: GraphJob,
  tables: number,
  e?: FoundryLoadEnrichment | null,
  late = false,
): LoadStep[] {
  const at = phases.indexOf(j.phase || "provisioning"),
    looking = lookingUp(j, e),
    // Still looking people up after its job went on: the load stopped
    // waiting for it.
    behind =
      late || (!looking && (e?.status === "queued" || e?.status === "running")),
    lookup = e?.status && enrichmentStepText(e, Date.now(), behind),
    woken = !looking && j.status === "queued" && at === 0 && !!lookup,
    queued = j.status === "queued" && !looking && !woken,
    running = j.status === "running" || looking || woken,
    succeeded = j.status === "succeeded";
  const step = (i: number, label: string): LoadStep => ({
    label,
    status:
      succeeded || (!queued && !looking && at > i)
        ? "done"
        : running && at === i
          ? "active"
          : "pending",
  });
  const p = j.progress || {};
  return [
    {
      label: queued ? "Waiting to start" : "Started",
      status: queued ? "active" : "done",
    },
    ...(j.kind === "extract"
      ? []
      : [
          step(
            0,
            !lookup
              ? "Connecting to the database"
              : looking && e?.status === "running"
                ? `Looking up people · ${lookup}`
                : looking
                  ? lookup
                  : `People enrichment · ${lookup}`,
          ),
          step(1, `Loading ${tables} ${tables === 1 ? "table" : "tables"}`),
        ]),
    step(
      2,
      p.documentsTotal
        ? `Reading ${p.documentsDone || 0} of ${p.documentsTotal} ${p.documentsTotal === 1 ? "document" : "documents"}`
        : "Reading documents",
    ),
    step(3, "Finishing"),
  ];
}

// ---------------------------------------------------------------- Databases

/**
 * A slug for a project or database from its name: lowercase letters, digits
 * and dashes, at most 63, never empty.
 *
 * @example
 * slugify("Customer Contracts — 2026!"); // "customer-contracts-2026"
 */
export function slugify(name: string, fallback = "ontology") {
  const slug = name
    .normalize("NFKD")
    .replace(/[\u0300-\u036f]/g, "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 63)
    .replace(/-+$/, "");
  return slug || fallback;
}

/** One live database an ontology can bind to, with the ontology using it. */
export type DatabaseChoice = {
  tenantId: string;
  projectId: string;
  project: string;
  name: string;
  createdAt?: string;
  // The ontology already on it: one ontology per database.
  usedBy?: string;
};

/**
 * Every live database of the workspace's projects, by project then name,
 * with the ontology each one backs, if any.
 */
export function databaseChoices(
  projects: { project: Project; databases: Tenant[] }[],
  ontologies: FoundryOntology[],
): DatabaseChoice[] {
  const used = new Map(ontologies.map((o) => [o.tenantId, o.name || o.id]));
  return projects
    .flatMap(({ project, databases }) =>
      databases
        .filter((t) => t.status === "RESOURCE_STATUS_ACTIVE")
        .map((t) => ({
          tenantId: t.id,
          projectId: project.id,
          project: project.displayName || project.slug || project.id,
          name: t.name || t.slug || t.id,
          createdAt: t.timestamps?.createdAt,
          usedBy: used.get(t.id),
        })),
    )
    .sort(
      (a, b) =>
        a.project.localeCompare(b.project) || a.name.localeCompare(b.name),
    );
}

/** Whether the WFE refused a database for want of a Helix subscription. */
export function isNoSubscription(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    (error.details || []).some(
      (d) =>
        !!d &&
        typeof d === "object" &&
        (d as { reason?: unknown }).reason === NO_WORKSPACE_SUBSCRIPTION,
    )
  );
}

// ---------------------------------------------------------------- New ontology

/** The steps of /ontology/new, in order. */
export const WIZARD_STEPS = [
  "name",
  "database",
  "sources",
  "goal",
  "review",
] as const;
export type WizardStepId = (typeof WIZARD_STEPS)[number];
/**
 * The steps of a workspace's first ontology of its own (the sample's
 * doesn't count): its name and its new database come from what it drafts
 * from (firstOntologyWizard), so it asks only that and the goal.
 */
export const FIRST_WIZARD_STEPS = ["sources", "goal", "review"] as const;

/**
 * The steps that draft an existing ontology's first version on its
 * overview, in order: the wizard's choices, then generating.
 */
export const DRAFT_STEPS = ["sources", "goal", "draft"] as const;
export type DraftStepId = (typeof DRAFT_STEPS)[number];

/** The most characters (code points, as the WFE counts) a goal has. */
export const MAX_GOAL = 2000;
/**
 * The most tables one generation drafts from: synced tables, table files
 * and API sources (one table each) together.
 */
export const MAX_TABLES = 50;

/**
 * The database a new ontology binds to: a live one of the workspace's, or a
 * new one on the shared cluster, in a project the workspace has or a new
 * one.
 */
export type DatabasePick =
  | { kind: "existing"; tenantId: string }
  | {
      kind: "new";
      project: { kind: "existing"; id: string } | { kind: "new"; name: string };
      name: string;
    };

/**
 * The new database the wizard's Database step starts with: named as the
 * ontology, in the workspace's first project, else in a new project named
 * as the workspace. Each name is cut to the 63 characters its field takes.
 *
 * @example
 * newDatabase("Customer contracts", "Acme", []);
 * // { kind: "new", project: { kind: "new", name: "Acme" }, name: "Customer contracts" }
 * newDatabase(" Orders ", "Acme", [{ id: "p1", slug: "prod" }]);
 * // { kind: "new", project: { kind: "existing", id: "p1" }, name: "Orders" }
 */
export function newDatabase(
  ontology: string,
  workspace: string,
  projects: readonly Project[],
): Extract<DatabasePick, { kind: "new" }> {
  // A cut that splits a surrogate pair drops its first half.
  const fit = (name: string) =>
    name
      .trim()
      .slice(0, 63)
      .replace(/[\uD800-\uDBFF]$/, "");
  return {
    kind: "new",
    project: projects[0]
      ? { kind: "existing", id: projects[0].id }
      : { kind: "new", name: fit(workspace) },
    name: fit(ontology),
  };
}

/** What a generation's steps (its sources and goal) have chosen so far. */
export type DraftChoices = {
  // The connectors chosen; null is every synced one. Never an API source's:
  // those are in eventSources, and every synced one leaves them out.
  connectorIds: ReadonlySet<string> | null;
  // The API sources chosen, by id, each with how the ontology reads it.
  eventSources: ReadonlyMap<string, FoundryEventSourceMode>;
  setIds: ReadonlySet<string>;
  goal: string;
};

/** What /ontology/new has chosen so far. */
export type WizardDraft = DraftChoices & {
  name: string;
  database: DatabasePick | null;
};

/**
 * What a draft is checked against, each undefined while it loads: the
 * databases an ontology can bind to, the workspace's projects, its
 * connectors, its document sets and its documents.
 */
export type WizardFacts = {
  databases?: readonly DatabaseChoice[];
  projects?: readonly Project[];
  connectors?: readonly Connector[];
  sets?: readonly FoundryDocumentSet[];
  documents?: readonly FoundryDocument[];
};

/**
 * Where a step stands: chosen (what it chose, in a line, and a `note` to
 * heed before going on), an optional one left empty, waiting on a read, or
 * blocked (why, in a sentence).
 */
export type StepState =
  | { kind: "ready"; summary: string; note?: string }
  | { kind: "skipped" }
  | { kind: "loading" }
  | { kind: "blocked"; reason: string };

/**
 * What a done step says in its one line.
 *
 * @example
 * stepSummary({ kind: "ready", summary: "Orders" }); // "Orders"
 * stepSummary({ kind: "skipped" }); // "Skipped"
 */
export const stepSummary = (s: StepState) =>
  s.kind === "ready" ? s.summary : s.kind === "skipped" ? "Skipped" : "…";

export type WizardStep<Id extends string = WizardStepId> = {
  id: Id;
  optional: boolean;
  state: StepState;
};

/**
 * What a generation drafts from, as GenerateOntology takes it: the chosen
 * connectors by id, or none at all (`noConnectors`: the WFE reads an empty
 * list as every connector), the chosen API sources with their modes (left
 * out when there are none), and the chosen document sets.
 */
export type GenerateSources = {
  eventSources?: [FoundryEventSourcePick, ...FoundryEventSourcePick[]];
  documentSetIds: string[];
} & (
  | { connectorIds: [string, ...string[]]; noConnectors?: never }
  | { noConnectors: true; connectorIds?: never }
);

/** What a generation is asked to draft from. */
export type GenerateChoices = GenerateSources & { goal?: string };

/** What creating the ontology and its first generation sends. */
export type WizardRequest = {
  name: string;
  database: DatabasePick;
  generate: GenerateChoices;
};

/** Where a step-by-step flow stands. */
type Walk<Id extends string> = {
  steps: WizardStep<Id>[];
  // The step shown: the one asked for, unless a step before it is blocked.
  current: Id;
  // Where Back and Continue go from it; null at either end.
  back: Id | null;
  next: Id | null;
};

export type Wizard = Walk<WizardStepId> & {
  // Set once every step before the review is ready or skipped.
  request: WizardRequest | null;
};

export type DraftWizard = Walk<DraftStepId> & {
  // Set once every step before the draft is ready or skipped.
  request: GenerateChoices | null;
};

/**
 * What each step of an ontology's setup is called, and asks while it is to
 * do: the wizard's and an unfinished ontology's overview's, so both say the
 * same.
 */
export const STEP_COPY: Record<
  WizardStepId | DraftStepId | OntologyStepId,
  { title: string; text: string }
> = {
  name: { title: "Name", text: "What people will call it." },
  database: {
    title: "Database",
    text: "Where its entities and facts are written: a Helix database you have, or a new one.",
  },
  sources: {
    title: "Connect your data or upload documents",
    // A local build offers no API source (data-api.ts's connectorKindsQuery).
    text: LOCAL
      ? "What Foundry’s agent drafts from: synced tables, table files, and folders of contracts, notes and other text. Any will do."
      : "What Foundry’s agent drafts from: synced tables, pieces your agents send, table files, and folders of contracts, notes and other text. Any will do.",
  },
  goal: { title: "Goal", text: "What it should serve, in your words." },
  review: {
    title: "Review and create",
    text: "Foundry creates it, then its agent drafts a first version.",
  },
  draft: {
    title: "Generate a draft",
    text: "Foundry’s agent drafts the types, links and mappings.",
  },
  current: {
    title: "Review and make a version current",
    text: "Check the draft, then make it the version the ontology uses.",
  },
  load: {
    title: "Load it",
    text: "Write the current version and your documents into the database.",
  },
  explore: {
    title: "Explore or ask",
    text: "Browse what it holds, or ask questions about it.",
  },
};

// "Stripe", "Stripe and Shop", "a, b, c and 2 more".
const listText = (names: string[]) =>
  new Intl.ListFormat("en", { style: "long", type: "conjunction" }).format(
    names.length > 3
      ? [...names.slice(0, 3), `${names.length - 3} more`]
      : names,
  );
const countText = (n: number, one: string) =>
  `${n.toLocaleString("en-US")} ${n === 1 ? one : one + "s"}`;
const hasTables = (c: Connector) => (c.tables || []).length > 0;

/**
 * Whether a generation can read an API source in `mode`, or why not: through
 * mappings once its table holds pieces (`pieces`: the WFE refuses an empty
 * one); by entity extraction once it has a text field (`text-field`) and its
 * own set (Pieces, data's piecesSet) holds a piece as a document
 * (`documents`: the source's next sync writes them). `sets` are the
 * workspace's, undefined while they load (no documents yet).
 *
 * @example
 * const replies = { id: "e1", kind: "events", config: { events: { textField: "reply" } }, tables: [{ name: "events", rowCount: "3" }] };
 * eventSourceReadiness(replies, "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED", []); // "ready"
 * eventSourceReadiness(replies, "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION", []); // "documents"
 * eventSourceReadiness(replies, "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION", [{ id: "p", documents: 3, connector: { connectorId: "e1", stream: "pieces" } }]); // "ready"
 */
export function eventSourceReadiness(
  c: Connector,
  mode: FoundryEventSourceMode,
  sets: readonly FoundryDocumentSet[] | undefined,
): "ready" | "pieces" | "text-field" | "documents" {
  if (mode === "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED")
    return Number(eventsTable(c)?.rowCount || 0) > 0 ? "ready" : "pieces";
  if (!eventsConfig(c).textField) return "text-field";
  return (piecesSet(sets, c.id)?.documents || 0) > 0 ? "ready" : "documents";
}

/**
 * What a generation reads of documents, by kind: table files whose read
 * succeeded (each a table), text documents its routes may read (Markdown,
 * MDX, text, and PDFs whose text is read), files still being read (table
 * files and PDFs, left out) and those that couldn't be read.
 */
function documentSources(documents: readonly FoundryDocument[]) {
  const uploaded = documents.filter((d) => d.status === "uploaded"),
    files = uploaded.filter((d) => readKindOf(d) !== "text"),
    state = (s: string) => files.filter((d) => fileRead(d).state === s).length;
  return {
    files: files.filter(
      (d) => readKindOf(d) === "table" && fileRead(d).state === "ready",
    ).length,
    reading: state("reading"),
    failed: state("failed"),
    text: uploaded.filter(readsAsText).length,
  };
}

/**
 * What a set's documents give a generation, in a line: its text documents
 * (the agent reads the DOCUMENTS_PER_SET newest; a PDF once its text is
 * read), its table files read, files (table files and PDFs) still being
 * read, and those that couldn't be read.
 *
 * @example
 * setSourcesText([
 *   { id: "a", mediaType: "text/markdown", status: "uploaded" },
 *   { id: "b", mediaType: "text/csv", status: "uploaded", read: { state: "FOUNDRY_FILE_READ_STATE_READING" } },
 * ]); // "1 document · 1 file still being read"
 */
export function setSourcesText(documents: readonly FoundryDocument[]) {
  const d = documentSources(documents);
  const parts = [
    ...(d.text
      ? [
          countText(d.text, "document") +
            (d.text > DOCUMENTS_PER_SET
              ? `; the agent reads the ${DOCUMENTS_PER_SET} newest`
              : ""),
        ]
      : []),
    ...(d.files ? [countText(d.files, "table file")] : []),
    ...(d.reading ? [`${countText(d.reading, "file")} still being read`] : []),
    ...(d.failed
      ? [`${countText(d.failed, "file")} that couldn’t be read`]
      : []),
  ];
  return parts.length ? parts.join(" · ") : "Nothing Foundry reads yet";
}

/**
 * A generation's sources checked against the workspace's connectors, sets
 * and documents, and what generating from them sends. It needs one source,
 * any kind: a synced connector, an API source it can read, a table file
 * that has been read, or a chosen set holding text. Tables, table files and
 * API sources read through mappings (one table each) together count toward
 * MAX_TABLES; sets and API sources read by extraction (their own set each)
 * toward MAX_GENERATION_SETS. A chosen connector on its first sync, or a
 * file of a chosen set still being read, is waited for while nothing else
 * is ready, and said in a `note` once something is: a generation started
 * now leaves it out. None chosen of the connectors sends `noConnectors`.
 * `documents` are the uploads: a connector's set holds text only, counted
 * by the set.
 *
 * API sources (kind events) are chosen in `eventSources` only, each with
 * its mode: every synced connector, and `connectorIds`, leave them out, and
 * `setIds` never sends their own sets (isEventSourceSet). One is read
 * through mappings (STRUCTURED) as its table, or by entity extraction
 * (EXTRACTION) as its own set of pieces; a chosen source the WFE would
 * refuse in its mode (eventSourceReadiness: no pieces in its table, no text
 * field, no pieces as documents) blocks the step. Ids of sources that are
 * gone are dropped.
 *
 * @example
 * const facts = { connectors: [], sets: [{ id: "g", name: "General" }], documents: [{ id: "d", setId: "g", mediaType: "text/csv", status: "uploaded", read: { state: "FOUNDRY_FILE_READ_STATE_READY" } }] };
 * generationSources({ connectorIds: null, eventSources: new Map(), setIds: new Set(["g"]) }, facts);
 * // { state: { kind: "ready", summary: "General: 1 table file" }, request: { noConnectors: true, documentSetIds: ["g"] } }
 * const replies = { id: "e1", kind: "events", name: "Agent replies", tables: [{ name: "events", rowCount: "3" }] };
 * generationSources({ connectorIds: null, eventSources: new Map([["e1", "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED"]]), setIds: new Set() }, { ...facts, connectors: [replies] });
 * // { state: { kind: "ready", summary: "Agent replies (mappings)" },
 * //   request: { noConnectors: true, eventSources: [{ connectorId: "e1", mode: "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED" }], documentSetIds: [] } }
 * generationSources({ connectorIds: null, eventSources: new Map([["e1", "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION"]]), setIds: new Set() }, { ...facts, connectors: [replies] }).state;
 * // { kind: "blocked", reason: "Agent replies has no text field." }
 */
export function generationSources(
  draft: Pick<DraftChoices, "connectorIds" | "eventSources" | "setIds">,
  facts: Pick<WizardFacts, "connectors" | "sets" | "documents">,
): { state: StepState; request: GenerateSources } {
  const blocked = (reason: string): StepState => ({ kind: "blocked", reason });
  const all = facts.connectors || [],
    connectors = all.filter((c) => !isEventSource(c.kind)),
    sources = all.filter((c) => isEventSource(c.kind)),
    chosen = (c: Connector) =>
      !draft.connectorIds || draft.connectorIds.has(c.id),
    pieces = (c: Connector) => Number(eventsTable(c)?.rowCount || 0);
  // The chosen connectors that are synced, and those still on a first sync.
  const picked = connectors.filter((c) => hasTables(c) && chosen(c)),
    syncing = connectors.filter(
      (c) => !hasTables(c) && isLiveSync(c.lastSync) && chosen(c),
    );
  // The chosen API sources with their modes, those read by extraction, and
  // those the WFE would refuse, by why.
  const events = sources.flatMap((c) => {
      const mode = draft.eventSources.get(c.id);
      return mode ? [{ c, mode }] : [];
    }),
    extracted = events.filter(
      (e) => e.mode === "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION",
    ),
    unready = (why: ReturnType<typeof eventSourceReadiness>) =>
      events
        .filter((e) => eventSourceReadiness(e.c, e.mode, facts.sets) === why)
        .map((e) => e.c);
  const synced = picked.reduce((n, c) => n + (c.tables || []).length, 0),
    tables = synced + events.length - extracted.length;
  const sets = (facts.sets || []).filter(
      (s) => draft.setIds.has(s.id) && !isEventSourceSet(s),
    ),
    uploads = documentSources(
      (facts.documents || []).filter((d) => draft.setIds.has(d.setId || "")),
    ),
    docs = {
      ...uploads,
      text:
        uploads.text +
        sets.reduce((n, s) => n + (s.connector ? s.documents || 0 : 0), 0),
    };
  const [first, ...rest] = picked.map((c) => c.id),
    [pick, ...picks] = events.map(
      (e): FoundryEventSourcePick => ({ connectorId: e.c.id, mode: e.mode }),
    );
  const request: GenerateSources = {
    ...(first ? { connectorIds: [first, ...rest] } : { noConnectors: true }),
    ...(pick ? { eventSources: [pick, ...picks] } : {}),
    documentSetIds: sets.map((s) => s.id),
  };
  const label = (c: Connector) => c.name || c.kind || c.id,
    // "Replies has no text field." / "Replies and Orders have no ..."
    lacking = (cs: Connector[], what: string) =>
      `${listText(cs.map(label))} ${cs.length === 1 ? "has" : "have"} ${what}.`;
  const empty = unready("pieces"),
    unnamed = unready("text-field"),
    undocumented = unready("documents");
  const waiting = [
    ...(syncing.length
      ? [`${listText(syncing.map(label))} still syncing`]
      : []),
    ...(docs.reading
      ? [
          `${countText(docs.reading, "file")} still being read: a generation started now leaves ${docs.reading === 1 ? "it" : "them"} out`,
        ]
      : []),
  ];
  const state: StepState =
    !facts.connectors || !facts.sets || !facts.documents
      ? { kind: "loading" }
      : sets.length > setRoom(draft.eventSources, all)
        ? blocked(
            extracted.length
              ? `A generation reads at most ${MAX_GENERATION_SETS} document sets, each API source read by extraction among them. Uncheck some.`
              : `A generation reads at most ${MAX_GENERATION_SETS} document sets. Uncheck some.`,
          )
        : tables + docs.files > MAX_TABLES
          ? blocked(
              `One generation covers at most ${MAX_TABLES} tables, table files included. Choose fewer connectors or sets.`,
            )
          : empty.length
            ? blocked(lacking(empty, "no pieces yet"))
            : unnamed.length
              ? blocked(lacking(unnamed, "no text field"))
              : undocumented.length
                ? blocked(lacking(undocumented, "no pieces as documents yet"))
                : !picked.length && !events.length && !docs.files && !docs.text
                  ? blocked(
                      syncing.length || docs.reading
                        ? `${syncing.length ? "Syncing" : "Reading its files"}… you can continue once ${syncing.length ? "it’s synced" : "they’re read"}.`
                        : connectors.some(hasTables) ||
                            sources.some(pieces) ||
                            facts.sets.length
                          ? `Choose a synced connector, ${sources.some(pieces) ? "an API source with pieces, " : ""}or a set holding documents Foundry reads.`
                          : "Connect your data or upload documents to continue.",
                    )
                  : {
                      kind: "ready",
                      summary: [
                        ...(picked.length
                          ? [
                              `${listText(picked.map(label))}: ${countText(synced, "table")}`,
                            ]
                          : []),
                        ...(events.length
                          ? [
                              listText(
                                events.map(
                                  (e) =>
                                    `${label(e.c)} (${e.mode === "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION" ? "extraction" : "mappings"})`,
                                ),
                              ),
                            ]
                          : []),
                        ...(sets.length
                          ? [
                              `${listText(sets.map(setLabel))}${
                                docs.files || docs.text
                                  ? `: ${[
                                      ...(docs.files
                                        ? [countText(docs.files, "table file")]
                                        : []),
                                      ...(docs.text
                                        ? [countText(docs.text, "document")]
                                        : []),
                                    ].join(" and ")}`
                                  : ""
                              }`,
                            ]
                          : []),
                      ].join("; "),
                      ...(waiting.length
                        ? {
                            note:
                              waiting
                                .join("; ")
                                .replace(/^./, (c) => c.toUpperCase()) + ".",
                          }
                        : {}),
                    };
  return { state, request };
}

/**
 * What a generation read, as Try again, Generate and an unfinished
 * ontology's setup ask for it again: the connectors whose tables it read
 * (`connectorIds`), the API sources it read (`eventSources`: one's table is
 * a STRUCTURED pick, one's own set an EXTRACTION pick), which never go in
 * `connectorIds`, and the document sets it read (`documentSetIds`) but
 * those API sources' own sets (isEventSourceSet), which never go there. A
 * connector `connectors` doesn't hold (deleted, or not read yet) stays a
 * connector, and a set `sets` doesn't hold stays a set, which the sources
 * step drops as gone: read both before asking.
 *
 * @example
 * generationPicks(
 *   { tables: [{ connectorId: "c1", table: "charges" }, { connectorId: "c1", table: "refunds" }, { connectorId: "e1", table: "events" }], documentSetIds: ["g", "p"] },
 *   [{ id: "c1", kind: "stripe" }, { id: "e1", kind: "events" }, { id: "e2", kind: "events" }],
 *   [{ id: "g" }, { id: "p", connector: { connectorId: "e2", connectorKind: "events", stream: "pieces" } }],
 * );
 * // { connectorIds: ["c1"],
 * //   eventSources: [{ connectorId: "e1", mode: "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED" }, { connectorId: "e2", mode: "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION" }],
 * //   documentSetIds: ["g"] }
 */
export function generationPicks(
  g: Pick<OntologyGeneration, "tables" | "documentSetIds"> | undefined,
  connectors: readonly Connector[],
  sets: readonly FoundryDocumentSet[],
): {
  connectorIds: string[];
  eventSources: FoundryEventSourcePick[];
  documentSetIds: string[];
} {
  const ids = [
      ...new Set((g?.tables || []).map((t) => t.connectorId || "")),
    ].filter(Boolean),
    isSource = (id: string) =>
      connectors.some((c) => c.id === id && isEventSource(c.kind)),
    // The API source whose own set `id` is, if it is one.
    owner = (id: string) =>
      sets.find((s) => s.id === id && isEventSourceSet(s))?.connector
        ?.connectorId,
    setIds = g?.documentSetIds || [];
  return {
    connectorIds: ids.filter((id) => !isSource(id)),
    eventSources: [
      ...ids.filter(isSource).map(
        (connectorId): FoundryEventSourcePick => ({
          connectorId,
          mode: "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED",
        }),
      ),
      ...setIds.flatMap((id): FoundryEventSourcePick[] => {
        const connectorId = owner(id);
        return connectorId
          ? [{ connectorId, mode: "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION" }]
          : [];
      }),
    ],
    documentSetIds: setIds.filter((id) => !owner(id)),
  };
}

/**
 * The API sources Generate starts with, each with its mode: those an
 * `?event_sources=` link names (paths.generate's `<id>:structured` or
 * `<id>:extraction`, "" for none), else those the current version's
 * generation read (`drafted`, through its tables and its sets;
 * generationPicks, with the workspace's `sets`), none without one. Ids
 * that aren't an API source of the workspace, and modes a link doesn't
 * spell, are dropped; an id named twice keeps its first mode.
 *
 * @example
 * const connectors = [{ id: "c1", kind: "stripe" }, { id: "e1", kind: "events" }];
 * startingEventSources("e1:structured,c1:structured,gone:structured", undefined, connectors, []);
 * // Map { "e1" => "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED" }
 * startingEventSources(null, { tables: [{ connectorId: "e1", table: "events" }] }, connectors, []);
 * // Map { "e1" => "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED" }
 * startingEventSources(null, { documentSetIds: ["p"] }, connectors, [{ id: "p", connector: { connectorId: "e1", connectorKind: "events" } }]);
 * // Map { "e1" => "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION" }
 * startingEventSources("", { tables: [{ connectorId: "e1", table: "events" }] }, connectors, []); // Map {}
 */
export function startingEventSources(
  prefill: string | null,
  drafted: Pick<OntologyGeneration, "tables" | "documentSetIds"> | undefined,
  connectors: readonly Connector[],
  sets: readonly FoundryDocumentSet[],
): Map<string, FoundryEventSourceMode> {
  const modes = Object.entries(EVENT_SOURCE_MODE_WORDS) as [
    FoundryEventSourceMode,
    string,
  ][];
  const wanted: [string, FoundryEventSourceMode][] =
    prefill === null
      ? generationPicks(drafted, connectors, sets).eventSources.map((p) => [
          p.connectorId,
          p.mode,
        ])
      : prefill
          .split(",")
          .flatMap((entry): [string, FoundryEventSourceMode][] => {
            const at = entry.lastIndexOf(":"),
              mode = modes.find(
                ([, word]) => word === entry.slice(at + 1),
              )?.[0];
            return at > 0 && mode ? [[entry.slice(0, at), mode]] : [];
          });
  return new Map(
    wanted.filter(
      ([id], i) =>
        wanted.findIndex(([other]) => other === id) === i &&
        connectors.some((c) => c.id === id && isEventSource(c.kind)),
    ),
  );
}

// The sources and goal checked against the workspace, and what generating
// from them sends.
function choiceSteps(
  draft: DraftChoices,
  facts: Pick<WizardFacts, "connectors" | "sets" | "documents">,
): {
  steps: WizardStep<WizardStepId & DraftStepId>[];
  generate: GenerateChoices;
} {
  const sources = generationSources(draft, facts);
  const goal = draft.goal.trim(),
    over = [...goal].length - MAX_GOAL;
  const goalState: StepState =
    over > 0
      ? {
          kind: "blocked",
          reason: `The goal is ${countText(over, "character")} too long.`,
        }
      : goal
        ? { kind: "ready", summary: titleFromGoal(goal) }
        : { kind: "skipped" };

  return {
    steps: [
      { id: "sources", optional: false, state: sources.state },
      { id: "goal", optional: true, state: goalState },
    ],
    generate: { ...(goal ? { goal } : {}), ...sources.request },
  };
}

// `before` and the last of `ids` after them, which is ready once every step
// before it is ready or skipped; then the step shown for `requested` (a
// `?step=` value; the first while it is missing or unknown) unless one
// before it is blocked, and where Back and Continue go from it.
function walk<Id extends string>(
  ids: readonly Id[],
  before: WizardStep<Id>[],
  requested: string | null,
): Walk<Id> & { done: boolean } {
  const done = before.every(
    (s) => s.state.kind === "ready" || s.state.kind === "skipped",
  );
  const steps: WizardStep<Id>[] = [
    ...before,
    {
      id: ids[before.length]!,
      optional: false,
      state: done
        ? { kind: "ready", summary: "" }
        : before.some((s) => s.state.kind === "blocked")
          ? { kind: "blocked", reason: "Finish the steps above." }
          : { kind: "loading" },
    },
  ];
  const asked = ids.indexOf(requested as Id),
    stop = steps.findIndex((s, i) => i < asked && s.state.kind === "blocked"),
    at = stop >= 0 ? stop : Math.max(asked, 0);
  return {
    steps,
    current: ids[at]!,
    back: ids[at - 1] ?? null,
    next: ids[at + 1] ?? null,
    done,
  };
}

/**
 * /ontology/new's steps checked against what the workspace holds: whether
 * each lets the user on and what it chose, the step to show for `requested`
 * (a `?step=` value; the first while it is missing or unknown), where Back
 * and Continue go, and, once everything is chosen, what creating sends.
 *
 * - Name: required.
 * - Database: one not used by another ontology, or a new one, named, in a
 *   project the workspace has or a new one, named.
 * - Connect your data or upload documents: at least one source, any kind
 *   (generationSources): a synced connector, an API source with pieces, a
 *   table file that has been read, or a set holding text; at most
 *   MAX_TABLES tables, table files and API sources and MAX_GENERATION_SETS
 *   sets. Ids of sets and API sources that are gone are dropped.
 * - Goal: optional; at most MAX_GOAL characters.
 *
 * A step whose reads are loading is passable: the one asked for still
 * shows, and nothing is sent until they load.
 *
 * @example
 * const draft = { name: "Orders", database: { kind: "existing", tenantId: "t1" }, connectorIds: null, eventSources: new Map(), setIds: new Set<string>(), goal: "" };
 * const facts = {
 *   databases: [{ tenantId: "t1", projectId: "p1", project: "Production", name: "orders" }],
 *   connectors: [{ id: "c1", kind: "stripe", name: "Stripe", tables: [{ name: "charges" }] }],
 *   sets: [],
 *   documents: [],
 * };
 * newOntologyWizard(draft, facts, "review").current; // "review"
 * newOntologyWizard(draft, facts, "review").steps.map((s) => s.state);
 * // [{ kind: "ready", summary: "Orders" }, { kind: "ready", summary: "orders in Production" },
 * //  { kind: "ready", summary: "Stripe: 1 table" }, { kind: "skipped" },
 * //  { kind: "ready", summary: "" }]
 * newOntologyWizard({ ...draft, name: " " }, facts, "review").current; // "name"
 * newOntologyWizard(draft, facts, null).next; // "database"
 */
export function newOntologyWizard(
  draft: WizardDraft,
  facts: WizardFacts,
  requested: string | null,
): Wizard {
  const ready = (summary: string): StepState => ({ kind: "ready", summary }),
    blocked = (reason: string): StepState => ({ kind: "blocked", reason }),
    loading: StepState = { kind: "loading" };
  const name = draft.name.trim();

  const database = ((d): StepState => {
    if (!d) return blocked("Choose a database, or create one.");
    if (d.kind === "existing") {
      if (!facts.databases) return loading;
      const c = facts.databases.find((x) => x.tenantId === d.tenantId);
      if (!c) return blocked("That database is gone. Choose another.");
      if (c.usedBy)
        return blocked(`${c.name} is used by ${c.usedBy}. Choose another.`);
      return ready(`${c.name} in ${c.project}`);
    }
    const db = d.name.trim(),
      project = d.project;
    if (project.kind === "new" && !project.name.trim())
      return blocked("Name the new project.");
    if (!db) return blocked("Name the new database.");
    if (project.kind === "new")
      return ready(
        `A new database, ${db}, in a new project, ${project.name.trim()}`,
      );
    if (!facts.projects) return loading;
    const p = facts.projects.find((x) => x.id === project.id);
    if (!p) return blocked("That project is gone. Choose another.");
    return ready(
      `A new database, ${db}, in ${p.displayName || p.slug || p.id}`,
    );
  })(draft.database);

  const choices = choiceSteps(draft, facts);
  const { done, ...at } = walk(
    WIZARD_STEPS,
    [
      {
        id: "name",
        optional: false,
        state: name ? ready(name) : blocked("Name the ontology."),
      },
      { id: "database", optional: false, state: database },
      ...choices.steps,
    ],
    requested,
  );
  return {
    ...at,
    request:
      done && draft.database
        ? {
            name,
            database:
              draft.database.kind === "existing"
                ? draft.database
                : {
                    kind: "new",
                    project:
                      draft.database.project.kind === "new"
                        ? {
                            kind: "new",
                            name: draft.database.project.name.trim(),
                          }
                        : draft.database.project,
                    name: draft.database.name.trim(),
                  },
            generate: choices.generate,
          }
        : null,
  };
}

/**
 * The name a first ontology gets, which its wizard doesn't ask for: what
 * it drafts from, by name (its connectors, then its API sources, then its
 * document sets), else the workspace's, cut to the 100 characters a name
 * takes.
 *
 * @example
 * const facts = {
 *   connectors: [{ id: "c1", kind: "stripe", name: "Stripe" }, { id: "c2", kind: "postgres", name: "Shop" }],
 *   sets: [{ id: "s1", name: "Contracts" }],
 * };
 * firstOntologyName({ connectorIds: ["c1", "c2"], documentSetIds: ["s1"] }, facts, "Acme"); // "Stripe, Shop, and Contracts"
 * firstOntologyName({ noConnectors: true, documentSetIds: ["s1"] }, facts, "Acme"); // "Contracts"
 * firstOntologyName({ noConnectors: true, documentSetIds: [] }, facts, "Acme"); // "Acme"
 */
export function firstOntologyName(
  sources: GenerateSources,
  facts: Pick<WizardFacts, "connectors" | "sets">,
  workspace: string,
): string {
  const connector = (id: string) => {
    const c = facts.connectors?.find((x) => x.id === id);
    return c && (c.name || c.kind || c.id);
  };
  const names = [
    ...(sources.connectorIds || []).map(connector),
    ...(sources.eventSources || []).map((e) => connector(e.connectorId)),
    ...sources.documentSetIds.map((id) => {
      const set = facts.sets?.find((x) => x.id === id);
      return set && setLabel(set);
    }),
  ]
    .map((n) => n?.trim())
    .filter((n): n is string => !!n);
  const name = names.length
    ? listText([...new Set(names)])
    : workspace.trim() || "My ontology";
  return [...name].slice(0, 100).join("").trim();
}

/**
 * /ontology/new for a workspace's first ontology of its own: the sources
 * and Goal steps, checked as newOntologyWizard checks them, then the
 * review. It is named from what it drafts from (firstOntologyName) and
 * goes on a new database named as it, in the workspace's first project,
 * else a new one named as the workspace (newDatabase); its request waits
 * for the projects to be read.
 *
 * @example
 * const facts = { projects: [], connectors: [{ id: "c1", kind: "stripe", name: "Stripe", tables: [{ name: "charges" }] }], sets: [], documents: [] };
 * const draft = { connectorIds: null, eventSources: new Map(), setIds: new Set<string>(), goal: "" };
 * firstOntologyWizard(draft, facts, "Acme", null).current; // "sources"
 * firstOntologyWizard(draft, facts, "Acme", "review").request?.name; // "Stripe"
 * firstOntologyWizard(draft, facts, "Acme", "review").request?.database;
 * // { kind: "new", project: { kind: "new", name: "Acme" }, name: "Stripe" }
 */
export function firstOntologyWizard(
  draft: DraftChoices,
  facts: WizardFacts,
  workspace: string,
  requested: string | null,
): Wizard {
  const choices = choiceSteps(draft, facts);
  const { done, ...at } = walk(FIRST_WIZARD_STEPS, choices.steps, requested);
  if (!done || !facts.projects) return { ...at, request: null };
  const name = firstOntologyName(choices.generate, facts, workspace);
  return {
    ...at,
    request: {
      name,
      database: newDatabase(name, workspace, facts.projects),
      generate: choices.generate,
    },
  };
}

/**
 * The new-ontology wizard's sources and Goal steps for an ontology that
 * exists but has no draft yet (its overview), checked as the wizard checks
 * them, then Generate a draft: ready once they are, with what generating
 * sends. The step shown is `requested`'s, as in the wizard.
 *
 * @example
 * const facts = { connectors: [{ id: "c1", kind: "stripe", name: "Stripe", tables: [{ name: "charges" }] }], sets: [], documents: [] };
 * const draft = { connectorIds: null, eventSources: new Map(), setIds: new Set<string>(), goal: "" };
 * draftWizard(draft, facts, null).current; // "sources"
 * draftWizard(draft, facts, "draft").request; // { connectorIds: ["c1"], documentSetIds: [] }
 * draftWizard({ ...draft, connectorIds: new Set<string>() }, facts, "draft").current; // "sources"
 */
export function draftWizard(
  draft: DraftChoices,
  facts: Pick<WizardFacts, "connectors" | "sets" | "documents">,
  requested: string | null,
): DraftWizard {
  const choices = choiceSteps(draft, facts);
  const { done, ...at } = walk(DRAFT_STEPS, choices.steps, requested);
  return { ...at, request: done ? choices.generate : null };
}

// ---------------------------------------------------------------- Setup

/**
 * An ontology's own setup steps once it exists, in order: a draft
 * generated, a version made current, a load, then exploring or asking.
 */
export const ONTOLOGY_STEPS = ["draft", "current", "load", "explore"] as const;
export type OntologyStepId = (typeof ONTOLOGY_STEPS)[number];

/**
 * What an ontology's setup is read from: the ontology, its versions and
 * generations, newest first (they matter only while no version is current,
 * so they may be empty then), and whether anyone explored it: it has a
 * question, or this browser opened its Explorer or Ask on a load.
 */
export type SetupFacts = {
  ontology: FoundryOntology;
  versions: readonly OntologyVersion[];
  generations: readonly OntologyGeneration[];
  explored: boolean;
};

/**
 * Where an ontology's setup is: the first of its steps (ONTOLOGY_STEPS) not
 * done and how it stands, or done once every one is.
 *
 * - draft: nothing drafted. To do; running while a generation is queued or
 *   running; failed when the newest generation failed (`error` says why).
 * - current: drafted, no version current; `versionId` is the newest draft.
 * - load: a version is current, but no load is current and the last one
 *   didn't succeed; running while a load job runs.
 * - explore: loaded, and nobody has explored it or asked about it.
 */
export type OntologySetup =
  | { step: "draft"; state: "todo" }
  | {
      step: "draft";
      state: "running" | "failed";
      generation: OntologyGeneration;
    }
  | { step: "current"; state: "todo"; versionId: string }
  | { step: "load"; state: "todo" | "running" }
  | { step: "explore"; state: "todo" }
  | { step: "done" };

/**
 * Where an ontology's setup is, for its overview. A
 * generation that stored its draft since the versions were read counts as
 * drafted, even with a newer one live.
 *
 * @example
 * const o = { id: "o" }, none = { versions: [], generations: [], explored: false };
 * ontologySetup({ ontology: o, ...none }); // { step: "draft", state: "todo" }
 * ontologySetup({ ontology: o, ...none, generations: [{ id: "g", status: "ONTOLOGY_GENERATION_STATUS_RUNNING" }] });
 * // { step: "draft", state: "running", generation: { id: "g", ... } }
 * ontologySetup({ ontology: o, ...none, generations: [{ id: "g", status: "ONTOLOGY_GENERATION_STATUS_FAILED" }] });
 * // { step: "draft", state: "failed", generation: { id: "g", ... } }
 * ontologySetup({ ontology: o, ...none, versions: [{ id: "v1" }] });
 * // { step: "current", state: "todo", versionId: "v1" }
 * ontologySetup({ ontology: { id: "o", currentVersionId: "v1" }, ...none }); // { step: "load", state: "todo" }
 * ontologySetup({ ontology: { id: "o", currentVersionId: "v1", lastLoad: { status: "succeeded" } }, ...none });
 * // { step: "explore", state: "todo" }
 */
export function ontologySetup({
  ontology: o,
  versions,
  generations,
  explored,
}: SetupFacts): OntologySetup {
  if (!o.currentVersionId) {
    const draft =
      versions[0]?.id ||
      generations.find((g) => g.resultVersionId)?.resultVersionId;
    if (draft) return { step: "current", state: "todo", versionId: draft };
    const live = generations.find(isLive);
    if (live) return { step: "draft", state: "running", generation: live };
    const last = generations[0];
    return last?.status === "ONTOLOGY_GENERATION_STATUS_FAILED"
      ? { step: "draft", state: "failed", generation: last }
      : { step: "draft", state: "todo" };
  }
  if (!o.currentLoad && o.lastLoad?.status !== "succeeded")
    return {
      step: "load",
      state: o.activeJob?.kind === "load" ? "running" : "todo",
    };
  return explored ? { step: "done" } : { step: "explore", state: "todo" };
}
