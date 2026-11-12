import {
  FOUNDRY_EDGE_LABELS,
  FOUNDRY_EDGES as E,
  FOUNDRY_NODES as N,
  FOUNDRY_PROP_PREFIX,
  isFoundryLabel,
} from "../../../shared/foundry-labels";
import {
  readRequest,
  type Ast,
  type DynamicQueryRequest,
  type ReadEntry,
} from "./database-query";
import { scopedPredicates, type Scope } from "./load-scope";

import {
  WEAK_LINKS,
  markDiffering,
  settleable,
  type Settleable,
} from "../reviews/model";
import { isRecord } from "../../../shared/json";

// What the website reads from an ontology's database, as v3 dynamic read
// queries over the ontology agent's graph layout (C3), run with the user's
// session through POST /v1/databases:query-read. Foundry's WFE serves no
// graph reads of its own: a document's view and an entity's page are built
// here, from these queries and their parsers.
//
// The layout (oa crates/extraction/src/store/mod.rs), Foundry's own labels
// prefixed (../../../shared/foundry-labels.ts):
// - FoundryDocument {doc_id, ...}; FoundrySection {ordinal, text, ...}
//   -FOUNDRY_IN_DOCUMENT-> it.
// - An entity node per type: {norm_name, name} for a named type,
//   {entity_key, name, props...} for a keyed one.
// - FoundryMention {doc_id, start, end, surface, resolved_by}
//   -FOUNDRY_IN_SECTION-> FoundrySection, -FOUNDRY_MENTION_OF-> entity.
// - A fact is an edge labelled with its link, {fact_key, provenance,
//   confidence, evidence_count}, between two entities.
// - FoundryEvidence {fact_key, link, doc_id, start, end, quote, confidence}
//   -FOUNDRY_FROM_SECTION-> FoundrySection for a quote; {source, snapshot,
//   row_key, props, ...} -FOUNDRY_FROM_CHUNK-> FoundryChunk for a row, and
//   -FOUNDRY_ABOUT-> the entity a row states (`props`: the props it set).
//   A property's quote: {prop, value, doc_id, start, end, quote,
//   confidence} -FOUNDRY_ABOUT-> the entity, -FOUNDRY_FROM_SECTION-> its
//   section.
// - FoundryReview {review_key, doc_id, section, start, end, quote, reason,
//   candidates, ...} and what settles it ({type, norm_name} for a name,
//   {type, entity_key, prop, values} for a property) -FOUNDRY_FROM_SECTION->
//   FoundrySection, -FOUNDRY_CONCERNS-> the entities it is about (a
//   property's entity; a name's candidates, as stored identities);
//   FoundryViolation -FOUNDRY_CONCERNS-> the entities it names.
// - Foundry's stamps, `foundry_*`: every node and edge has its load's
//   `foundry_load`; what a request created, its FoundryTx's `foundry_tx`.
// Offsets (start, end) are UTF-8 bytes into the section's text.
//
// Every read is scoped to one load (./load-scope.ts): its lookups are
// P(label, L) and their key, and what they follow stays inside the load.
//
// Every read is bounded: a document reads its own sections and what points
// at them; an entity reads at most 200 facts each way, 50 mentions, 50
// reviews and 200 rows and quotes that state it; reviews read the entities
// they concern (a few each), and an entity's held claims the reviews of
// names linked on less than a name in their documents (at most 50
// documents); evidence is read for at most 50 facts at a time.

export const LIMITS = Object.freeze({
  factsPerSide: 200,
  mentions: 50,
  reviews: 50,
  about: 200,
  evidenceFacts: 50,
  evidencePerFact: 20,
  documentFacts: 100,
  // The quotes of one document a re-check reads.
  documentQuotes: 1000,
});

// ---------------------------------------------------------------- Entity ids

/** An entity's identity: its type, and its key or its normalized name. */
export type EntityIdentity =
  | { type: string; key: string }
  | { type: string; name: string };

const toBase64Url = (bytes: Uint8Array) =>
  btoa(String.fromCharCode(...bytes))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");

/**
 * The C3 entity id: unpadded base64url of the type, NUL, then "k:" and the
 * key or "n:" and the normalized name. The same across loads.
 *
 * @example
 * entityId({ type: "Customer", key: "cus_1" }); // "Q3VzdG9tZXIAazpjdXNfMQ"
 */
export function entityId(i: EntityIdentity): string {
  const rest = "key" in i ? `k:${i.key}` : `n:${i.name}`;
  return toBase64Url(new TextEncoder().encode(`${i.type}\0${rest}`));
}

/** An entity id's type and identity, or undefined when it isn't one. */
export function parseEntityId(id: string): EntityIdentity | undefined {
  let decoded: string;
  try {
    const b64 = id.replace(/-/g, "+").replace(/_/g, "/");
    decoded = new TextDecoder("utf-8", { fatal: true }).decode(
      Uint8Array.from(atob(b64), (c) => c.charCodeAt(0)),
    );
  } catch {
    return undefined;
  }
  const nul = decoded.indexOf("\0");
  if (nul < 1) return undefined;
  const type = decoded.slice(0, nul),
    rest = decoded.slice(nul + 1);
  if (rest.startsWith("k:") && rest.length > 2)
    return { type, key: rest.slice(2) };
  if (rest.startsWith("n:") && rest.length > 2)
    return { type, name: rest.slice(2) };
  return undefined;
}

/**
 * An entity as a read returns it. `dbId` is its node's id in this database;
 * `props` are the properties the read projected (what its name is made of).
 */
export type EntityRow = {
  dbId: number;
  id: string;
  type: string;
  name: string;
  identity: EntityIdentity;
  props: Record<string, unknown>;
};

type Row = Record<string, unknown>;
const rows = (v: unknown): Row[] =>
  Array.isArray(v) ? v.filter(isRecord) : isRecord(v) ? [v] : [];
const str = (v: unknown) => (typeof v === "string" ? v : undefined);
const num = (v: unknown) =>
  typeof v === "number" && Number.isFinite(v) ? v : undefined;
const int = (v: unknown) => {
  const n = num(v);
  return n !== undefined && Number.isInteger(n) ? n : undefined;
};

/** An entity node's row (`$id`, `$label`, entity_key or norm_name, name), or undefined. */
export function entityRow(r: Row): EntityRow | undefined {
  const dbId = int(r.$id),
    type = str(r.$label);
  if (dbId === undefined || !type) return undefined;
  if (isFoundryLabel("node", type)) return undefined;
  const key = str(r.entity_key),
    norm = str(r.norm_name);
  const identity: EntityIdentity | undefined = key
    ? { type, key }
    : norm
      ? { type, name: norm }
      : undefined;
  if (!identity) return undefined;
  return {
    dbId,
    id: entityId(identity),
    type,
    name: str(r.name) || key || norm!,
    identity,
    props: Object.fromEntries(
      Object.entries(r).filter(([k]) => !k.startsWith("$")),
    ),
  };
}

// ---------------------------------------------------------------- Request AST

const label = (name: string): Ast => ({
  eq: { left: { property: "$label" }, right: { constant: { string: name } } },
});
/** `$label == type` in the scope, and the reader's own predicates: P(type, L) and more. */
const nodesOf = (scope: Scope, type: string, ...more: Ast[]): Ast => ({
  nodes_where: {
    predicate: {
      and: { predicates: [...scopedPredicates(scope, type), ...more] },
    },
  },
});
const edgesOf = (scope: Scope, link: string, ...more: Ast[]): Ast => ({
  edges_where: {
    predicate: {
      and: { predicates: [...scopedPredicates(scope, link), ...more] },
    },
  },
});
const propIs = (property: string, param: string): Ast => ({
  eq: { left: { property }, right: { param } },
});
const v = (name: string): Ast => ({ nodes: { reference: { var: name } } });
const ve = (name: string): Ast => ({ edges: { reference: { var: name } } });
const limit = (input: Ast, n: number): Ast => ({
  limit: { input, count: { literal: n } },
});
const where = (input: Ast, predicate: Ast): Ast => ({
  where: { input, predicate },
});
const step = (op: string, input: Ast, edge?: string): Ast => ({
  [op]: edge ? { input, label: edge } : { input },
});
const dedup = (input: Ast): Ast => ({ dedup: { input } });
const values = (input: Ast, properties: string[]): Ast => ({
  values: { input, properties },
});
const valueMap = (input: Ast): Ast => ({ value_map: { input } });
const edgeProps = (input: Ast): Ast => ({ edge_properties: { input } });

const ENTITY = ["$id", "$label", "name", "entity_key", "norm_name"];
// What a review is read with: its place, its reason and what settles it.
const REVIEW_FIELDS = [
  "$id",
  "reason",
  "review_key",
  "claim_key",
  "claim",
  "statement",
  "section",
  "type",
  "doc_id",
  "norm_name",
  "entity_key",
  "values",
  "prop",
  "start",
  "end",
  "quote",
  "candidates",
  "suggested_link",
  "confidence",
];

// The entities `review_nodes` concern, every property each (a property
// review's stored value, a candidate's name), and which review each is of.
const concernedEntries: ReadEntry[] = [
  {
    name: "review_concerns",
    root: edgeProps(step("out_e", v("review_nodes"), E.CONCERNS)),
  },
  {
    name: "concerned",
    root: valueMap(dedup(step("out", v("review_nodes"), E.CONCERNS))),
  },
];

// ---------------------------------------------------------------- A document

export type DocSection = {
  dbId: number;
  ordinal: number;
  text: string;
  // Its UTF-8 byte range in the document as the load read it.
  start?: number;
  end?: number;
  model?: string;
  verifier?: string;
  promptVersion?: string;
};
/** A span of a section: UTF-8 byte offsets into its text. */
export type Span = { section: number; start: number; end: number };
export type DocMention = Span & {
  surface: string;
  resolvedBy?: string;
  entity: EntityRow;
};
export type DocEvidence = Span & {
  quote: string;
  link: string;
  factKey: string;
  confidence?: number;
};
export type DocReview = Span &
  Settleable & {
    quote: string;
    reason: string;
    // The property it is about, for a prop_conflict or an unsure value.
    prop?: string;
    suggestedLink?: string;
    confidence?: number;
  };
export type DocumentGraph = {
  // Whether a load has stored the document at all.
  stored: boolean;
  sections: DocSection[];
  mentions: DocMention[];
  evidence: DocEvidence[];
  reviews: DocReview[];
  // Every entity it names, by type, then name.
  entities: EntityRow[];
};

/**
 * Everything a load stored from one document, in one read. `names` are the
 * properties its entities' names are made of (naming.ts namingKeys).
 */
export function documentRequest(
  scope: Scope,
  documentId: string,
  names: string[] = [],
): DynamicQueryRequest {
  return readRequest(
    "foundry_document",
    [
      {
        name: "doc",
        root: limit(nodesOf(scope, N.Document, propIs("doc_id", "doc_id")), 1),
        returned: false,
      },
      { name: "document", root: values(v("doc"), ["$id", "doc_id"]) },
      {
        name: "secs",
        root: step("in", v("doc"), E.IN_DOCUMENT),
        returned: false,
      },
      {
        name: "sections",
        root: values(v("secs"), [
          "$id",
          "ordinal",
          "start",
          "end",
          "text",
          "model",
          "verifier",
          "prompt_version",
        ]),
      },
      {
        name: "mention_nodes",
        root: step("in", v("secs"), E.IN_SECTION),
        returned: false,
      },
      {
        name: "mentions",
        root: values(v("mention_nodes"), [
          "$id",
          "start",
          "end",
          "surface",
          "resolved_by",
        ]),
      },
      {
        name: "mention_in",
        root: edgeProps(step("in_e", v("secs"), E.IN_SECTION)),
      },
      {
        name: "mention_of",
        root: edgeProps(step("out_e", v("mention_nodes"), E.MENTION_OF)),
      },
      {
        name: "entities",
        root: values(dedup(step("out", v("mention_nodes"), E.MENTION_OF)), [
          ...ENTITY,
          ...names,
        ]),
      },
      {
        name: "quoted",
        root: step("in", v("secs"), E.FROM_SECTION),
        returned: false,
      },
      {
        name: "quoted_in",
        root: edgeProps(step("in_e", v("secs"), E.FROM_SECTION)),
      },
      {
        name: "evidence",
        root: values(where(v("quoted"), label(N.Evidence)), [
          "$id",
          "fact_key",
          "link",
          "start",
          "end",
          "quote",
          "confidence",
        ]),
      },
      {
        name: "review_nodes",
        root: where(v("quoted"), label(N.Review)),
        returned: false,
      },
      {
        name: "reviews",
        root: values(v("review_nodes"), REVIEW_FIELDS),
      },
      ...concernedEntries,
    ],
    { doc_id: { value: documentId, type: "string" } },
  );
}

const byOrder = (a: Span, b: Span) =>
  a.section - b.section || a.start - b.start || a.end - b.end;
const byEntity = (a: EntityRow, b: EntityRow) =>
  a.type.localeCompare(b.type) || a.name.localeCompare(b.name);

/** A from → to map of edge rows: which section a mention or quote is in. */
function edgeMap(value: unknown): Map<number, number> {
  const map = new Map<number, number>();
  for (const e of rows(value)) {
    const from = int(e.$from),
      to = int(e.$to);
    if (from !== undefined && to !== undefined) map.set(from, to);
  }
  return map;
}

/** The entities each review concerns (`concernedEntries`), by the review's database id. */
function concernedBy(data: Row): Map<number, EntityRow[]> {
  const entities = new Map(
    rows(data.concerned).flatMap((r) => {
      const e = entityRow(r);
      return e ? [[e.dbId, e] as const] : [];
    }),
  );
  const by = new Map<number, EntityRow[]>();
  for (const edge of rows(data.review_concerns)) {
    const from = int(edge.$from),
      to = entities.get(int(edge.$to) ?? -1);
    if (from !== undefined && to) by.set(from, [...(by.get(from) || []), to]);
  }
  return by;
}

/** Reads documentRequest's answer; nothing in it is trusted. */
export function parseDocument(body: unknown): DocumentGraph {
  const data = isRecord(body) ? body : {};
  const sections = rows(data.sections)
    .flatMap((r): DocSection[] => {
      const dbId = int(r.$id),
        ordinal = int(r.ordinal);
      return dbId === undefined || ordinal === undefined
        ? []
        : [
            {
              dbId,
              ordinal,
              text: str(r.text) || "",
              start: int(r.start),
              end: int(r.end),
              model: str(r.model),
              verifier: str(r.verifier),
              promptVersion: str(r.prompt_version),
            },
          ];
    })
    .sort((a, b) => a.ordinal - b.ordinal);
  const ordinalOf = new Map(sections.map((s) => [s.dbId, s.ordinal]));
  const entities = new Map<number, EntityRow>();
  for (const r of rows(data.entities)) {
    const e = entityRow(r);
    if (e) entities.set(e.dbId, e);
  }
  const span = (r: Row, inSection: Map<number, number>): Span | undefined => {
    const id = int(r.$id),
      start = int(r.start),
      end = int(r.end);
    const section = id === undefined ? undefined : inSection.get(id);
    const ordinal = section === undefined ? undefined : ordinalOf.get(section);
    if (ordinal === undefined || start === undefined || end === undefined)
      return undefined;
    return { section: ordinal, start, end };
  };
  const mentionIn = edgeMap(data.mention_in),
    mentionOf = edgeMap(data.mention_of),
    quotedIn = edgeMap(data.quoted_in);
  const mentions = rows(data.mentions)
    .flatMap((r): DocMention[] => {
      const s = span(r, mentionIn),
        entity = entities.get(mentionOf.get(int(r.$id) ?? -1) ?? -1);
      return s && entity
        ? [
            {
              ...s,
              surface: str(r.surface) || "",
              resolvedBy: str(r.resolved_by),
              entity,
            },
          ]
        : [];
    })
    .sort(byOrder);
  const evidence = rows(data.evidence)
    .flatMap((r): DocEvidence[] => {
      const s = span(r, quotedIn),
        factKey = str(r.fact_key),
        link = str(r.link);
      return s && factKey && link
        ? [
            {
              ...s,
              quote: str(r.quote) || "",
              link,
              factKey,
              confidence: num(r.confidence),
            },
          ]
        : [];
    })
    .sort(byOrder);
  const concerned = concernedBy(data);
  const reviews = rows(data.reviews)
    .flatMap((r): DocReview[] => {
      const s = span(r, quotedIn);
      return s
        ? [
            {
              ...s,
              ...settleable(r, concerned.get(int(r.$id) ?? -1) || []),
              quote: str(r.quote) || "",
              reason: str(r.reason) || "unverified",
              prop: str(r.prop),
              suggestedLink: str(r.suggested_link),
              confidence: num(r.confidence),
            },
          ]
        : [];
    })
    .sort(byOrder);
  return {
    stored: rows(data.document).length > 0,
    sections,
    mentions,
    evidence,
    reviews: markDiffering(reviews),
    entities: [...entities.values()].sort(byEntity),
  };
}

/** A fact a document states: its edge, its ends and the quotes for it. */
export type DocFact = {
  link: string;
  factKey: string;
  from?: EntityRow;
  to?: EntityRow;
  provenance?: string;
  confidence?: number;
  evidenceCount?: number;
  quotes: DocEvidence[];
  // Past what a read takes: whether it has an edge isn't known.
  unread?: boolean;
};

/** The facts behind a document's quotes, by fact key (index-backed). Null when it has none. */
export function documentFactsRequest(
  scope: Scope,
  evidence: DocEvidence[],
): DynamicQueryRequest | null {
  const facts = [
    ...new Map(evidence.map((e) => [e.factKey, e.link])).entries(),
  ].slice(0, LIMITS.documentFacts);
  if (!facts.length) return null;
  return readRequest(
    "foundry_document_facts",
    facts.map(([, link], i) => ({
      name: `fact_${i}`,
      root: edgeProps(
        limit(edgesOf(scope, link, propIs("fact_key", `key_${i}`)), 1),
      ),
    })),
    Object.fromEntries(
      facts.map(([key], i) => [`key_${i}`, { value: key, type: "string" }]),
    ),
  );
}

/**
 * The document's facts, from documentFactsRequest's answer: each with the
 * entities it joins (the document names both) and its quotes. A quote whose
 * fact is gone (a later load replaced it) still shows, without its ends.
 */
export function parseDocumentFacts(
  body: unknown,
  doc: DocumentGraph,
): DocFact[] {
  const data = isRecord(body) ? body : {};
  const byKey = new Map<string, DocFact>();
  const entities = new Map(doc.entities.map((e) => [e.dbId, e]));
  const read = new Set(
    [...new Set(doc.evidence.map((e) => e.factKey))].slice(
      0,
      LIMITS.documentFacts,
    ),
  );
  for (const e of doc.evidence) {
    const f = byKey.get(e.factKey) || {
      link: e.link,
      factKey: e.factKey,
      quotes: [],
      ...(read.has(e.factKey) ? {} : { unread: true }),
    };
    f.quotes.push(e);
    byKey.set(e.factKey, f);
  }
  for (const [name, value] of Object.entries(data)) {
    if (!/^fact_\d+$/.test(name)) continue;
    for (const r of rows(value)) {
      const f = byKey.get(str(r.fact_key) || "");
      if (!f) continue;
      f.from = entities.get(int(r.$from) ?? -1);
      f.to = entities.get(int(r.$to) ?? -1);
      f.provenance = str(r.provenance);
      f.confidence = num(r.confidence);
      f.evidenceCount = int(r.evidence_count);
    }
  }
  return [...byKey.values()].sort((a, b) =>
    byOrder(a.quotes[0]!, b.quotes[0]!),
  );
}

// ---------------------------------------------------------------- An entity

export type Fact = {
  link: string;
  // "out": the entity is the link's from end.
  direction: "out" | "in";
  factKey?: string;
  other?: EntityRow;
  provenance?: string;
  confidence?: number;
  evidenceCount?: number;
  // The link's own properties.
  props: Record<string, unknown>;
};
export type EntityMention = Span & {
  documentId: string;
  surface: string;
  resolvedBy?: string;
  // The section's text, for a snippet.
  text: string;
};
export type EntityReview = DocReview & { documentId: string };
/** A stored rule break of the entity: the entities it names, in order. */
export type Violation = {
  constraint: string;
  severity?: string;
  entities: EntityRow[];
};
/** A table row that states the entity: the agent's source id of its table. */
export type SourceRow = {
  source: string;
  snapshot?: string;
  mapping?: string;
  rowKey?: string;
  // The props the row set; unknown for a load from before rows said.
  props?: string[];
};
/** A quote in a document that states one of the entity's props. */
export type PropEvidence = Span & {
  documentId: string;
  prop: string;
  value: unknown;
  quote: string;
  confidence?: number;
};
export type EntityGraph = {
  entity:
    | (EntityRow & {
        props: Record<string, unknown>;
        // The part of its load that made it: table rows or documents;
        // unknown for a load from before parts.
        madeBy?: "rows" | "documents";
      })
    | null;
  facts: Fact[];
  mentions: EntityMention[];
  reviews: EntityReview[];
  violations: Violation[];
  rows: SourceRow[];
  propEvidence: PropEvidence[];
  // More than the limits allow: some facts, mentions, reviews or what
  // states it aren't shown.
  truncated: {
    facts: boolean;
    mentions: boolean;
    reviews: boolean;
    about: boolean;
  };
};

/**
 * One entity with its facts, mentions, reviews, stored rule breaks (each
 * with the entities it names, in order) and the rows that state it. `names`
 * are the properties the other entities are named by.
 */
export function entityRequest(
  scope: Scope,
  identity: EntityIdentity,
  names: string[] = [],
): DynamicQueryRequest {
  const [prop, value] =
    "key" in identity
      ? ["entity_key", identity.key]
      : ["norm_name", identity.name];
  const sides = (["out", "in"] as const).flatMap((side): ReadEntry[] => {
    const edges = step(`${side}_e`, v("e"));
    return [
      {
        name: `${side}_rows`,
        root: limit(
          side === "in"
            ? where(edges, {
                not: {
                  predicate: {
                    is_in: {
                      value: { property: "$label" },
                      values: { param: "foundry_edges" },
                    },
                  },
                },
              })
            : edges,
          LIMITS.factsPerSide + 1,
        ),
        returned: false,
      },
      { name: `${side}_facts`, root: edgeProps(ve(`${side}_rows`)) },
      {
        name: `${side}_ends`,
        root: values(
          dedup(step(side === "out" ? "out_n" : "in_n", ve(`${side}_rows`))),
          [...ENTITY, ...names],
        ),
      },
    ];
  });
  return readRequest(
    "foundry_entity",
    [
      {
        name: "e",
        root: limit(nodesOf(scope, identity.type, propIs(prop, "identity")), 1),
        returned: false,
      },
      { name: "entity", root: valueMap(v("e")) },
      ...sides,
      {
        name: "mention_nodes",
        root: limit(step("in", v("e"), E.MENTION_OF), LIMITS.mentions + 1),
        returned: false,
      },
      {
        name: "mentions",
        root: values(v("mention_nodes"), [
          "$id",
          "doc_id",
          "start",
          "end",
          "surface",
          "resolved_by",
        ]),
      },
      {
        name: "mention_in",
        root: edgeProps(step("out_e", v("mention_nodes"), E.IN_SECTION)),
      },
      {
        name: "mention_sections",
        root: values(dedup(step("out", v("mention_nodes"), E.IN_SECTION)), [
          "$id",
          "ordinal",
          "text",
        ]),
      },
      {
        name: "concerns",
        root: limit(step("in", v("e"), E.CONCERNS), LIMITS.reviews + 1),
        returned: false,
      },
      {
        name: "review_nodes",
        root: where(v("concerns"), label(N.Review)),
        returned: false,
      },
      {
        name: "reviews",
        root: values(v("review_nodes"), [
          "$id",
          "doc_id",
          "section",
          "start",
          "end",
          "quote",
          "reason",
          "review_key",
          "claim_key",
          "claim",
          "statement",
          "type",
          "norm_name",
          "entity_key",
          "values",
          "prop",
          "candidates",
          "suggested_link",
          "confidence",
        ]),
      },
      ...concernedEntries,
      {
        name: "violation_nodes",
        root: where(v("concerns"), label(N.Violation)),
        returned: false,
      },
      {
        name: "violations",
        root: values(v("violation_nodes"), ["$id", "constraint", "severity"]),
      },
      // Each record's entities, by the position it names them at.
      {
        name: "violation_concerns",
        root: edgeProps(step("out_e", v("violation_nodes"), E.CONCERNS)),
      },
      {
        name: "violation_ends",
        root: values(dedup(step("out", v("violation_nodes"), E.CONCERNS)), [
          ...ENTITY,
          ...names,
        ]),
      },
      // What states it: table rows, and quotes of its props.
      {
        name: "about_nodes",
        root: limit(step("in", v("e"), E.ABOUT), LIMITS.about + 1),
        returned: false,
      },
      {
        name: "about",
        root: values(v("about_nodes"), [
          "$id",
          "source",
          "snapshot",
          "mapping",
          "row_key",
          "props",
          "prop",
          "value",
          "doc_id",
          "start",
          "end",
          "quote",
          "confidence",
        ]),
      },
      {
        name: "about_in",
        root: edgeProps(step("out_e", v("about_nodes"), E.FROM_SECTION)),
      },
      {
        name: "about_sections",
        root: values(dedup(step("out", v("about_nodes"), E.FROM_SECTION)), [
          "$id",
          "ordinal",
        ]),
      },
    ],
    {
      identity: { value, type: "string" },
      foundry_edges: {
        value: [...FOUNDRY_EDGE_LABELS],
        type: { array: "string" },
      },
    },
  );
}

/**
 * The documents of the held claims among `reviews`, each once, in order.
 *
 * @example
 * heldClaimDocs(page.reviews); // ["doc_1"]
 */
export const heldClaimDocs = (reviews: readonly Settleable[]) =>
  [
    ...new Set(
      reviews.flatMap(({ settle: s }) =>
        s?.kind === "release" ? [s.docId] : [],
      ),
    ),
  ].sort();

/**
 * A read of the reviews of names a load linked on less than a name
 * (`WEAK_LINKS`) in `docs`, the documents of a page's held claims
 * (heldClaimDocs), which mark the claims they hold (markDiffering). An
 * entity's page needs it: it reads only the reviews that concern its entity,
 * and such a review concerns only the row it linked, so the name at a held
 * claim's other end has its review elsewhere. Each document is one lookup of
 * the load's reviews by `doc_id`, which the engine indexes, and holds one such
 * review per name it linked so. None for no document.
 *
 * @example
 * nameLinksRequest(scope, heldClaimDocs(page.reviews)); // a read, or null with no held claim
 */
export function nameLinksRequest(
  scope: Scope,
  docs: readonly string[],
): DynamicQueryRequest | null {
  if (!docs.length) return null;
  return readRequest(
    "foundry_name_links",
    docs.map((_, i) => ({
      name: `links_${i}`,
      root: values(
        nodesOf(scope, N.Review, propIs("doc_id", `doc_${i}`), {
          is_in: {
            value: { property: "reason" },
            values: { param: "weak_links" },
          },
        }),
        [
          "doc_id",
          "section",
          "start",
          "reason",
          "review_key",
          "type",
          "norm_name",
        ],
      ),
    })),
    {
      ...Object.fromEntries(
        docs.map((d, i) => [`doc_${i}`, { value: d, type: "string" }]),
      ),
      weak_links: { value: [...WEAK_LINKS], type: { array: "string" } },
    },
  );
}

/**
 * The reviews nameLinksRequest read, each with its reason, its place and
 * what settles it, as markDiffering takes them; nothing in them is trusted.
 */
export function parseNameLinks(
  body: unknown,
): (Settleable & { reason: string; section: number; start: number })[] {
  const data = isRecord(body) ? body : {};
  return Object.keys(data)
    .filter((k) => /^links_\d+$/.test(k))
    .flatMap((k) => rows(data[k]))
    .flatMap((r) => {
      const reason = str(r.reason),
        section = int(r.section),
        start = int(r.start);
      return reason && section !== undefined && start !== undefined
        ? [{ reason, section, start, ...settleable(r, []) }]
        : [];
    });
}

/**
 * Whether an entity's property is the layout's, not the data's: its
 * identity, normalized names and identifiers (`<prop>__nn`, `<prop>__id`),
 * and Foundry's stamps (`foundry_*`: the load and request that wrote it, the
 * part of that load that made it).
 *
 * @example
 * isLayoutProp("email__id"); // true
 * isLayoutProp("foundry_tx"); // true
 * isLayoutProp("email"); // false
 */
export const isLayoutProp = (k: string) =>
  k.startsWith("$") ||
  k.endsWith("__nn") ||
  k.endsWith("__id") ||
  k === "entity_key" ||
  k === "norm_name" ||
  k.startsWith(FOUNDRY_PROP_PREFIX);

// A fact's layout props; Foundry's stamps (`foundry_*`) go by their prefix.
const LAYOUT_PROPS = new Set([
  "fact_key",
  "provenance",
  "confidence",
  "evidence_count",
  "ontology_version",
]);

/** Reads entityRequest's answer; nothing in it is trusted. */
export function parseEntity(body: unknown): EntityGraph {
  const data = isRecord(body) ? body : {};
  const self = rows(data.entity)[0];
  const row = self && entityRow(self);
  const facts = (["out", "in"] as const).flatMap((direction) => {
    const ends = new Map<number, EntityRow>();
    for (const r of rows(data[`${direction}_ends`])) {
      const e = entityRow(r);
      if (e) ends.set(e.dbId, e);
    }
    return rows(data[`${direction}_facts`])
      .slice(0, LIMITS.factsPerSide)
      .flatMap((r): Fact[] => {
        const link = str(r.$label);
        if (!link || isFoundryLabel("edge", link)) return [];
        const other = ends.get(
          int(direction === "out" ? r.$to : r.$from) ?? -1,
        );
        return [
          {
            link,
            direction,
            factKey: str(r.fact_key),
            other,
            provenance: str(r.provenance),
            confidence: num(r.confidence),
            evidenceCount: int(r.evidence_count),
            props: Object.fromEntries(
              Object.entries(r).filter(
                ([k]) =>
                  !k.startsWith("$") &&
                  !k.startsWith(FOUNDRY_PROP_PREFIX) &&
                  !LAYOUT_PROPS.has(k),
              ),
            ),
          },
        ];
      });
  });
  const sections = new Map<number, { ordinal: number; text: string }>();
  for (const r of rows(data.mention_sections)) {
    const id = int(r.$id),
      ordinal = int(r.ordinal);
    if (id !== undefined && ordinal !== undefined)
      sections.set(id, { ordinal, text: str(r.text) || "" });
  }
  const inSection = edgeMap(data.mention_in);
  const mentionRows = rows(data.mentions);
  const mentions = mentionRows
    .slice(0, LIMITS.mentions)
    .flatMap((r): EntityMention[] => {
      const s = sections.get(inSection.get(int(r.$id) ?? -1) ?? -1),
        documentId = str(r.doc_id),
        start = int(r.start),
        end = int(r.end);
      return s && documentId && start !== undefined && end !== undefined
        ? [
            {
              documentId,
              section: s.ordinal,
              start,
              end,
              surface: str(r.surface) || "",
              resolvedBy: str(r.resolved_by),
              text: s.text,
            },
          ]
        : [];
    })
    .sort((a, b) => a.documentId.localeCompare(b.documentId) || byOrder(a, b));
  const reviewRows = rows(data.reviews);
  const concerned = concernedBy(data);
  const reviews = reviewRows.flatMap((r): EntityReview[] => {
    const documentId = str(r.doc_id),
      section = int(r.section),
      start = int(r.start),
      end = int(r.end);
    return documentId &&
      section !== undefined &&
      start !== undefined &&
      end !== undefined
      ? [
          {
            documentId,
            section,
            start,
            end,
            ...settleable(r, concerned.get(int(r.$id) ?? -1) || []),
            quote: str(r.quote) || "",
            reason: str(r.reason) || "unverified",
            prop: str(r.prop),
            suggestedLink: str(r.suggested_link),
            confidence: num(r.confidence),
          },
        ]
      : [];
  });
  // Each violation record's entities, by its id, with their positions.
  const violationEnds = new Map<number, EntityRow>();
  for (const r of rows(data.violation_ends)) {
    const e = entityRow(r);
    if (e) violationEnds.set(e.dbId, e);
  }
  const named = new Map<number, { position: number; entity: EntityRow }[]>();
  for (const r of rows(data.violation_concerns)) {
    const from = int(r.$from),
      position = int(r.position),
      entity = violationEnds.get(int(r.$to) ?? -1);
    if (from === undefined || position === undefined || !entity) continue;
    named.set(from, [...(named.get(from) || []), { position, entity }]);
  }
  const aboutRows = rows(data.about);
  const aboutIn = edgeMap(data.about_in);
  const aboutOrdinals = new Map<number, number>();
  for (const r of rows(data.about_sections)) {
    const id = int(r.$id),
      ordinal = int(r.ordinal);
    if (id !== undefined && ordinal !== undefined)
      aboutOrdinals.set(id, ordinal);
  }
  const about = aboutRows.slice(0, LIMITS.about);
  const part = str(self?.foundry_part);
  return {
    entity: row
      ? {
          ...row,
          props: Object.fromEntries(
            Object.entries(self!).filter(([k]) => !isLayoutProp(k)),
          ),
          madeBy: part?.endsWith(":documents")
            ? "documents"
            : part?.endsWith(":rows")
              ? "rows"
              : undefined,
        }
      : null,
    facts,
    mentions,
    // Marked once the names their documents linked on less than a name are
    // read too (nameLinksRequest).
    reviews: reviews.slice(0, LIMITS.reviews),
    violations: rows(data.violations).flatMap((r): Violation[] => {
      const constraint = str(r.constraint);
      return constraint
        ? [
            {
              constraint,
              severity: str(r.severity),
              entities: (named.get(int(r.$id) ?? -1) || [])
                .sort((a, b) => a.position - b.position)
                .map((x) => x.entity),
            },
          ]
        : [];
    }),
    rows: about.flatMap((r): SourceRow[] => {
      const source = str(r.source);
      return source && r.prop === undefined
        ? [
            {
              source,
              snapshot: str(r.snapshot),
              mapping: str(r.mapping),
              rowKey: str(r.row_key),
              props: Array.isArray(r.props)
                ? r.props.filter((p): p is string => typeof p === "string")
                : undefined,
            },
          ]
        : [];
    }),
    propEvidence: about
      .flatMap((r): PropEvidence[] => {
        const prop = str(r.prop),
          documentId = str(r.doc_id),
          start = int(r.start),
          end = int(r.end);
        const section = aboutOrdinals.get(aboutIn.get(int(r.$id) ?? -1) ?? -1);
        return prop &&
          documentId &&
          section !== undefined &&
          start !== undefined &&
          end !== undefined
          ? [
              {
                prop,
                value: r.value,
                documentId,
                section,
                start,
                end,
                quote: str(r.quote) || "",
                confidence: num(r.confidence),
              },
            ]
          : [];
      })
      .sort(
        (a, b) =>
          a.prop.localeCompare(b.prop) ||
          a.documentId.localeCompare(b.documentId) ||
          byOrder(a, b),
      ),
    truncated: {
      facts: (["out", "in"] as const).some(
        (d) => rows(data[`${d}_facts`]).length > LIMITS.factsPerSide,
      ),
      mentions: mentionRows.length > LIMITS.mentions,
      reviews: reviewRows.length > LIMITS.reviews,
      about: aboutRows.length > LIMITS.about,
    },
  };
}

// ---------------------------------------------------------------- Property provenance

/**
 * How two values of a property compare: text NFKC,
 * trimmed, with runs of space as one and case aside; numbers exact, or
 * within a billionth of each other once either has a fraction; anything
 * else as JSON.
 *
 * @example
 * sameValue(" Signed ", "signed"); // true
 * sameValue(24, 36); // false
 */
export function sameValue(a: unknown, b: unknown): boolean {
  const text = (v: string) =>
    v.normalize("NFKC").trim().replace(/\s+/g, " ").toLowerCase();
  if (typeof a === "string" && typeof b === "string")
    return text(a) === text(b);
  if (typeof a === "number" && typeof b === "number")
    return (
      a === b ||
      ((!Number.isInteger(a) || !Number.isInteger(b)) &&
        Math.abs(a - b) <= 1e-9 * Math.max(Math.abs(a), Math.abs(b)))
    );
  return JSON.stringify(a) === JSON.stringify(b);
}

/** One property of an entity, with what states its value and what disagrees. */
export type PropertyView = {
  prop: string;
  // The stored value; undefined when it has none.
  value: unknown;
  // The rows that set it, and the quotes that agree with it.
  rows: SourceRow[];
  quotes: PropEvidence[];
  // Values documents state that aren't the stored one, each with its
  // quotes: a table's value kept over them, or documents that disagree
  // leaving no value.
  others: { value: unknown; quotes: PropEvidence[] }[];
};

/**
 * Each property of an entity with its provenance: the stored props (except
 * `name`, the page's title) and any prop only quotes state, in name order.
 * A row from before rows listed their props is taken to set every prop no
 * quote states.
 *
 * @example
 * propertyViews({ term_months: 24 }, [], [{ prop: "term_months", value: 24, documentId: "d", section: 0, start: 0, end: 9, quote: "24 months" }])[0].quotes.length; // 1
 */
export function propertyViews(
  props: Record<string, unknown>,
  rows: SourceRow[],
  evidence: PropEvidence[],
): PropertyView[] {
  const names = [
    ...new Set([
      ...Object.keys(props).filter((k) => k !== "name"),
      ...evidence.map((e) => e.prop),
    ]),
  ].sort();
  return names.flatMap((prop): PropertyView[] => {
    const value = props[prop];
    const stored = value !== undefined && value !== null && value !== "";
    const quotes = evidence.filter((e) => e.prop === prop);
    const agree = stored ? quotes.filter((q) => sameValue(q.value, value)) : [];
    const others: PropertyView["others"] = [];
    for (const q of quotes) {
      if (agree.includes(q)) continue;
      const same = others.find((o) => sameValue(o.value, q.value));
      if (same) same.quotes.push(q);
      else others.push({ value: q.value, quotes: [q] });
    }
    if (!stored && !others.length) return [];
    return [
      {
        prop,
        value: stored ? value : undefined,
        rows: stored
          ? rows.filter((r) =>
              r.props ? r.props.includes(prop) : !agree.length,
            )
          : [],
        quotes: agree,
        others,
      },
    ];
  });
}

/**
 * How a document's mention found its entity, in a few words; nothing for
 * the plain case (by name) or a way this page doesn't know.
 *
 * @example
 * resolvedByText("identifier"); // "By an identifier"
 */
export function resolvedByText(r?: string): string | undefined {
  switch (r) {
    case "identifier":
      return "By an identifier";
    case "alias":
      return "By a short name";
    case "created":
      return "Created from the text";
    case "user":
      return "Linked by a person";
    case "adjudicated":
      return "Matched by the model";
    case "legal_form":
      return "By its legal form";
    case "part_name":
      return "By part of its name";
    case "own_name":
      return "By an identifier used as its name";
    default:
      return undefined;
  }
}

/** What states a fact: a table row, or a quote in a document. */
export type Evidence =
  | (SourceRow & { kind: "row"; confidence?: number })
  | (Span & {
      kind: "quote";
      documentId: string;
      quote: string;
      confidence?: number;
    });

/** The evidence of up to 50 facts, by fact key (index-backed). Null when none has a key. */
export function factEvidenceRequest(
  scope: Scope,
  facts: Fact[],
): DynamicQueryRequest | null {
  const keys = [
    ...new Set(facts.flatMap((f) => (f.factKey ? [f.factKey] : []))),
  ].slice(0, LIMITS.evidenceFacts);
  if (!keys.length) return null;
  return readRequest(
    "foundry_fact_evidence",
    keys.flatMap((_, i): ReadEntry[] => [
      {
        name: `ev_${i}`,
        root: limit(
          nodesOf(scope, N.Evidence, propIs("fact_key", `key_${i}`)),
          LIMITS.evidencePerFact,
        ),
        returned: false,
      },
      {
        name: `evidence_${i}`,
        root: values(v(`ev_${i}`), [
          "$id",
          "fact_key",
          "doc_id",
          "start",
          "end",
          "quote",
          "confidence",
          "source",
          "snapshot",
          "mapping",
          "row_key",
        ]),
      },
      {
        name: `in_${i}`,
        root: edgeProps(step("out_e", v(`ev_${i}`), E.FROM_SECTION)),
      },
      {
        name: `sections_${i}`,
        root: values(dedup(step("out", v(`ev_${i}`), E.FROM_SECTION)), [
          "$id",
          "ordinal",
        ]),
      },
    ]),
    Object.fromEntries(
      keys.map((key, i) => [`key_${i}`, { value: key, type: "string" }]),
    ),
  );
}

/** Each fact key's evidence, from factEvidenceRequest's answer. */
export function parseFactEvidence(body: unknown): Map<string, Evidence[]> {
  const data = isRecord(body) ? body : {};
  const out = new Map<string, Evidence[]>();
  for (const name of Object.keys(data)) {
    const i = /^evidence_(\d+)$/.exec(name)?.[1];
    if (i === undefined) continue;
    const inSection = edgeMap(data[`in_${i}`]);
    const ordinals = new Map<number, number>();
    for (const r of rows(data[`sections_${i}`])) {
      const id = int(r.$id),
        ordinal = int(r.ordinal);
      if (id !== undefined && ordinal !== undefined) ordinals.set(id, ordinal);
    }
    for (const r of rows(data[name])) {
      const key = str(r.fact_key);
      if (!key) continue;
      const documentId = str(r.doc_id),
        source = str(r.source);
      const section = ordinals.get(inSection.get(int(r.$id) ?? -1) ?? -1);
      const start = int(r.start),
        end = int(r.end);
      const e: Evidence | undefined =
        documentId &&
        section !== undefined &&
        start !== undefined &&
        end !== undefined
          ? {
              kind: "quote",
              documentId,
              section,
              start,
              end,
              quote: str(r.quote) || "",
              confidence: num(r.confidence),
            }
          : source
            ? {
                kind: "row",
                source,
                snapshot: str(r.snapshot),
                mapping: str(r.mapping),
                rowKey: str(r.row_key),
                confidence: num(r.confidence),
              }
            : undefined;
      if (e) out.set(key, [...(out.get(key) || []), e]);
    }
  }
  return out;
}

// ---------------------------------------------------------------- An entity's page

/** An entity's facts grouped by link and direction, in link order. */
export function factGroups(facts: Fact[]) {
  const groups = new Map<
    string,
    { link: string; direction: "out" | "in"; facts: Fact[] }
  >();
  for (const f of facts) {
    const key = `${f.link}\0${f.direction}`;
    const g = groups.get(key) || {
      link: f.link,
      direction: f.direction,
      facts: [],
    };
    g.facts.push(f);
    groups.set(key, g);
  }
  return [...groups.values()].sort(
    (a, b) =>
      a.link.localeCompare(b.link) || b.direction.localeCompare(a.direction),
  );
}

/**
 * Where a fact came from, in words.
 *
 * @example
 * provenanceText("extracted"); // "From a document"
 */
export const provenanceText = (p?: string) =>
  p === "extracted" ? "From a document" : "From a table";

/** An entity's mentions grouped by document, in text order. */
export function mentionsByDocument(mentions: EntityMention[]) {
  const docs = new Map<string, EntityMention[]>();
  for (const m of mentions)
    docs.set(m.documentId, [...(docs.get(m.documentId) || []), m]);
  return [...docs].map(([documentId, list]) => ({
    documentId,
    mentions: [...list].sort(byOrder),
  }));
}

/**
 * A property's value as text: strings as they are, numbers grouped, true and
 * false as Yes and No, anything else as JSON. Empty values are "".
 *
 * @example
 * valueText(1200000); // "1,200,000"
 */
export function valueText(v: unknown): string {
  if (v === null || v === undefined || v === "") return "";
  if (typeof v === "number") return new Intl.NumberFormat("en-US").format(v);
  if (typeof v === "boolean") return v ? "Yes" : "No";
  if (typeof v === "string") return v;
  return JSON.stringify(v);
}

/**
 * The sentence-sized piece of a section around a UTF-8 byte span, with the
 * span's UTF-16 range in it: for a mention's snippet.
 *
 * @example
 * snippet("Paid €5 to Globex. Next.", 13, 19); // { before: "Paid €5 to ", match: "Globex", after: "." }
 */
export function snippet(text: string, start: number, end: number) {
  const bytes = new TextEncoder().encode(text),
    decode = (a: number, b: number) =>
      new TextDecoder().decode(bytes.slice(a, b));
  const head = decode(0, start),
    match = decode(start, end),
    tail = decode(end, bytes.length);
  const from = Math.max(0, head.search(/[^.!?\n]*$/)),
    stop = /[.!?\n]/.exec(tail);
  return {
    before: head.slice(from).trimStart(),
    match,
    after: stop ? tail.slice(0, stop.index + 1).replace(/\n$/, "") : tail,
  };
}

// ---------------------------------------------------------------- A load's reviews

/**
 * The review queue's reads of one load (../reviews/queue.ts): every review
 * the load holds, a page at a time in the database's order, each with the
 * entities it concerns, as a document's reviews are read; and its documents'
 * quotes of facts (a fact's evidence in a section) in a score range,
 * counted, and read one at a time at offsets the queue draws at random.
 * Offset paging reads every row before its page, so the reviews read stop
 * at `reviews`, and the page says how many it left.
 */
export const QUEUE_READ = Object.freeze({
  page: 500,
  reviews: 5000,
});

/** A score range: from (inclusive) and below (exclusive), either open. */
export type ScoreRange = { from?: number; below?: number };

const sectionsIn = (scope: Scope): Ast => nodesOf(scope, N.Section);
/**
 * The load's quotes of facts in documents, scored in `range`: a fact's
 * evidence (it has a fact key, where a property's quote has none) in a
 * section. `from` and `below` name the parameters the range reads.
 */
const quotesIn = (
  scope: Scope,
  range: ScoreRange,
  from: string,
  below: string,
): Ast =>
  where(step("in", sectionsIn(scope), E.FROM_SECTION), {
    and: {
      predicates: [
        label(N.Evidence),
        { not: { predicate: { is_null: { property: "fact_key" } } } },
        ...(range.from === undefined
          ? []
          : [
              {
                gte: {
                  left: { property: "confidence" },
                  right: { param: from },
                },
              },
            ]),
        ...(range.below === undefined
          ? []
          : [
              {
                lt: {
                  left: { property: "confidence" },
                  right: { param: below },
                },
              },
            ]),
      ],
    },
  });
const rangeParams = (range: ScoreRange, from: string, below: string) => ({
  ...(range.from === undefined
    ? {}
    : { [from]: { value: range.from, type: "f64" } }),
  ...(range.below === undefined
    ? {}
    : { [below]: { value: range.below, type: "f64" } }),
});

/**
 * How many reviews a load holds (a bitmap count), and how many quotes of
 * facts in each of `ranges` (by name): a count of the quotes in its
 * sections.
 */
export function loadReviewCountsRequest(
  scope: Scope,
  ranges: Readonly<Record<string, ScoreRange>> = {},
): DynamicQueryRequest {
  const named = Object.entries(ranges);
  return readRequest(
    "foundry_queue_counts",
    [
      { name: "reviews", root: { count: { input: nodesOf(scope, N.Review) } } },
      ...named.map(([, range], i) => ({
        name: `quotes_${i}`,
        root: {
          count: { input: quotesIn(scope, range, `from_${i}`, `below_${i}`) },
        },
      })),
    ],
    Object.assign(
      {},
      ...named.map(([, range], i) =>
        rangeParams(range, `from_${i}`, `below_${i}`),
      ),
    ),
  );
}

/** loadReviewCountsRequest's answer, its ranges by name; a count missing reads as none. */
export function parseLoadReviewCounts(
  body: unknown,
  ranges: readonly string[] = [],
): { reviews: number; quotes: Record<string, number> } {
  const data = isRecord(body) ? body : {};
  const count = (v: unknown) => {
    const n = int(v);
    return n !== undefined && n >= 0 ? n : 0;
  };
  return {
    reviews: count(data.reviews),
    quotes: Object.fromEntries(
      ranges.map((name, i) => [name, count(data[`quotes_${i}`])]),
    ),
  };
}

/** One page (from 0) of a load's reviews, QUEUE_READ.page at a time, with the entities they concern. */
export function loadReviewsRequest(
  scope: Scope,
  page: number,
): DynamicQueryRequest {
  const start = Math.max(0, Math.floor(page)) * QUEUE_READ.page;
  return readRequest("foundry_queue_reviews", [
    {
      name: "review_nodes",
      root: {
        range: {
          input: nodesOf(scope, N.Review),
          start: { literal: start },
          end: { literal: start + QUEUE_READ.page },
        },
      },
      returned: false,
    },
    { name: "reviews", root: values(v("review_nodes"), REVIEW_FIELDS) },
    ...concernedEntries,
  ]);
}

/**
 * loadReviewsRequest's answer: the reviews of documents, each at its
 * stored place, with what settles it (a review of a table's rows has no
 * document, and is left out). Nothing in it is trusted.
 */
export function parseLoadReviews(body: unknown): EntityReview[] {
  const data = isRecord(body) ? body : {};
  const concerned = concernedBy(data);
  return rows(data.reviews).flatMap((r): EntityReview[] => {
    const documentId = str(r.doc_id),
      section = int(r.section),
      start = int(r.start),
      end = int(r.end);
    return documentId &&
      section !== undefined &&
      start !== undefined &&
      end !== undefined
      ? [
          {
            documentId,
            section,
            start,
            end,
            ...settleable(r, concerned.get(int(r.$id) ?? -1) || []),
            quote: str(r.quote) || "",
            reason: str(r.reason) || "unverified",
            prop: str(r.prop),
            suggestedLink: str(r.suggested_link),
            confidence: num(r.confidence),
          },
        ]
      : [];
  });
}

/** A quote of a fact in a document: where it is, its fact and the verifier's score. */
export type FactQuote = Span & {
  documentId: string;
  factKey: string;
  link: string;
  quote: string;
  confidence?: number;
};

/**
 * The quotes of facts scored in `range` at `offsets` among the load's (in
 * the order its sections are read), each with its section's ordinal: one
 * entry each.
 */
export function rangeQuotesRequest(
  scope: Scope,
  range: ScoreRange,
  offsets: readonly number[],
): DynamicQueryRequest {
  return readRequest(
    "foundry_queue_quotes_at",
    offsets.flatMap((at, i): ReadEntry[] => {
      const o = Math.max(0, Math.floor(at));
      return [
        {
          name: `q_${i}`,
          root: {
            range: {
              input: quotesIn(scope, range, "from", "below"),
              start: { literal: o },
              end: { literal: o + 1 },
            },
          },
          returned: false,
        },
        {
          name: `quote_${i}`,
          root: values(v(`q_${i}`), [
            "doc_id",
            "fact_key",
            "link",
            "start",
            "end",
            "quote",
            "confidence",
          ]),
        },
        {
          name: `section_${i}`,
          root: values(step("out", v(`q_${i}`), E.FROM_SECTION), ["ordinal"]),
        },
      ];
    }),
    rangeParams(range, "from", "below"),
  );
}

/** rangeQuotesRequest's answer, one per offset: undefined where none read. */
export function parseRangeQuotes(
  body: unknown,
  count: number,
): (FactQuote | undefined)[] {
  const data = isRecord(body) ? body : {};
  return Array.from({ length: count }, (_, i) => {
    const q = rows(data[`quote_${i}`])[0],
      section = int(rows(data[`section_${i}`])[0]?.ordinal);
    if (!q || section === undefined) return undefined;
    const documentId = str(q.doc_id),
      factKey = str(q.fact_key),
      link = str(q.link),
      start = int(q.start),
      end = int(q.end);
    return documentId &&
      factKey &&
      link &&
      start !== undefined &&
      end !== undefined &&
      start < end
      ? {
          documentId,
          factKey,
          link,
          section,
          start,
          end,
          quote: str(q.quote) || "",
          confidence: num(q.confidence),
        }
      : undefined;
  });
}

/**
 * The ends of each quote's fact (by its link and key, index-backed), named
 * by `names` (naming.ts namingKeys). Null when there are none.
 */
export function quoteFactsRequest(
  scope: Scope,
  quotes: readonly FactQuote[],
  names: string[] = [],
): DynamicQueryRequest | null {
  if (!quotes.length) return null;
  return readRequest(
    "foundry_queue_quote_facts",
    quotes.flatMap(({ link }, i): ReadEntry[] => [
      {
        name: `e_${i}`,
        root: limit(edgesOf(scope, link, propIs("fact_key", `key_${i}`)), 1),
        returned: false,
      },
      {
        name: `from_${i}`,
        root: values(step("in_n", ve(`e_${i}`)), [...ENTITY, ...names]),
      },
      {
        name: `to_${i}`,
        root: values(step("out_n", ve(`e_${i}`)), [...ENTITY, ...names]),
      },
    ]),
    Object.fromEntries(
      quotes.map(({ factKey }, i) => [
        `key_${i}`,
        { value: factKey, type: "string" },
      ]),
    ),
  );
}

/** quoteFactsRequest's answer: each quote's fact's ends, where read. */
export function parseQuoteFacts(
  body: unknown,
  count: number,
): { from?: EntityRow; to?: EntityRow }[] {
  const data = isRecord(body) ? body : {};
  return Array.from({ length: count }, (_, i) => {
    const end = (v: unknown) => {
      const r = rows(v)[0];
      return r ? entityRow(r) : undefined;
    };
    const from = end(data[`from_${i}`]),
      to = end(data[`to_${i}`]);
    return { ...(from ? { from } : {}), ...(to ? { to } : {}) };
  });
}

/**
 * The quotes the load holds of each document of `documents`, in its
 * sections, by the document's id (index-backed): what a re-check of a
 * decision shows when the claim's review is gone (an approval it wrote).
 */
export function documentQuotesRequest(
  scope: Scope,
  documents: readonly string[],
): DynamicQueryRequest | null {
  if (!documents.length) return null;
  return readRequest(
    "foundry_queue_quotes",
    documents.flatMap((_, i): ReadEntry[] => [
      {
        name: `q_${i}`,
        root: limit(
          nodesOf(scope, N.Evidence, propIs("doc_id", `doc_${i}`)),
          LIMITS.documentQuotes,
        ),
        returned: false,
      },
      {
        name: `quotes_${i}`,
        root: values(v(`q_${i}`), [
          "$id",
          "fact_key",
          "link",
          "start",
          "end",
          "quote",
          "confidence",
        ]),
      },
      {
        name: `in_${i}`,
        root: edgeProps(step("out_e", v(`q_${i}`), E.FROM_SECTION)),
      },
      {
        name: `sections_${i}`,
        root: values(dedup(step("out", v(`q_${i}`), E.FROM_SECTION)), [
          "$id",
          "ordinal",
        ]),
      },
    ]),
    Object.fromEntries(
      documents.map((d, i) => [`doc_${i}`, { value: d, type: "string" }]),
    ),
  );
}

/** documentQuotesRequest's answer: every quote of each document, at its place. */
export function parseDocumentQuotes(
  body: unknown,
  documents: readonly string[],
): FactQuote[] {
  const data = isRecord(body) ? body : {};
  return documents.flatMap((documentId, i) => {
    const ordinal = new Map(
      rows(data[`sections_${i}`]).flatMap((r) => {
        const id = int(r.$id),
          o = int(r.ordinal);
        return id !== undefined && o !== undefined ? [[id, o] as const] : [];
      }),
    );
    const section = new Map(
      rows(data[`in_${i}`]).flatMap((e) => {
        const from = int(e.$from),
          at = ordinal.get(int(e.$to) ?? -1);
        return from !== undefined && at !== undefined
          ? [[from, at] as const]
          : [];
      }),
    );
    return rows(data[`quotes_${i}`]).flatMap((q): FactQuote[] => {
      const at = section.get(int(q.$id) ?? -1),
        factKey = str(q.fact_key),
        link = str(q.link),
        start = int(q.start),
        end = int(q.end);
      return at !== undefined &&
        factKey &&
        link &&
        start !== undefined &&
        end !== undefined
        ? [
            {
              documentId,
              factKey,
              link,
              section: at,
              start,
              end,
              quote: str(q.quote) || "",
              confidence: num(q.confidence),
            },
          ]
        : [];
    });
  });
}
