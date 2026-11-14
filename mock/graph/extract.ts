import type {
  FoundryReviewDecision,
  ReviewDecision,
} from "../../src/shared/wfe";
import { createHash } from "node:crypto";
import {
  FOUNDRY_EDGES as E,
  FOUNDRY_NODES as N,
} from "../../src/shared/foundry-labels";
import { ENRICHMENT_SOURCE } from "../../src/web/areas/ontology/enrichment-fields";
import { isEmpty, type Scope } from "./scope";
import type { LoadGraph, Props, Value } from "./store";
import { onReset } from "../state";

// The mock's stand-in for the ontology agent's Ingest and Extract RPCs:
// deterministic, with no model, writing the graph layout the engine writes
// (oa crates/extraction/src/store/mod.rs, C3), Foundry's own labels prefixed
// (src/shared/foundry-labels.ts), so the website's reads see what they will
// see on Helix. Both write through one load's view of the graph
// (store.ts LoadGraph): everything they write carries `foundry_load`, and
// every lookup (a stored chunk, an entity by key, a document read before)
// sees that load only, so a new load writes a full copy beside the others.
//
// The load's marker, a FoundryLoad {foundry_load, ontology_version,
// node_labels, edge_labels, deleting}, names every label the load wrote.
//
// Ingest: rows of a mapped table become keyed entities {entity_key, name,
// props, <names prop>__nn, <identifier prop>__id, foundry_part:
// "<load>:rows"} (foundry_part only when a row creates it), each with a row
// FoundryEvidence {..., props: the props the row set} -FOUNDRY_ABOUT-> it
// and -FOUNDRY_FROM_CHUNK-> its FoundryChunk; a row that links two entities
// becomes a fact edge {fact_key, provenance: "stated", confidence 1,
// evidence_count} with its own row FoundryEvidence. A load that finds a
// chunk stored skips it.
//
// Extract, for one document, within its set's scope (./scope.ts):
// - FoundryDocument {doc_id, uri, media_type, content_hash,
//   ontology_version} and FoundrySections {section_id, doc_id, ordinal,
//   start, end, hash, text, artifact_key} -FOUNDRY_IN_DOCUMENT-> it: blocks
//   of lines split at blank lines (and before Markdown headings), packed up
//   to 4000 bytes, each an exact trimmed slice of the text. An empty scope
//   stops there: out of scope, nothing read.
// - Names: a keyed entity of a scope type with a `names` prop is found by
//   its full name, then by that name without its legal suffix ("Globex" for
//   "Globex Corporation"); only the short one found resolves by alias,
//   unless another entity shares it. An identifier prop's stored value
//   quoted in the text (a customer id, an email) finds it too, by
//   identifier. FoundryMentions {doc_id, start, end, surface, resolved_by}
//   -FOUNDRY_IN_SECTION-> FoundrySection, -FOUNDRY_MENTION_OF-> the entity,
//   at most 3 per entity and section.
// - A company-looking name that is no entity ("Umbrella Holdings") is a
//   FoundryReview {review_key, doc_id, section, start, end, quote, reason:
//   "unresolved", type, norm_name, candidates, suggested_link, confidence}
//   -FOUNDRY_FROM_SECTION-> FoundrySection, -FOUNDRY_CONCERNS-> its
//   candidates (stored customers sharing a word of it, by their stored
//   identity) and the Contract. Text never creates a keyed entity. With
//   `linkedBy`, its first candidate links it instead, and the review is that
//   rung's ("adjudicated", "legal_form", "part_name" or "own_name"), naming
//   that one only; only the model's link has a confidence.
// - When the scope lets documents create Contracts, a document naming a
//   company is a Contract {norm_name, name, foundry_part: "<load>:documents"}
//   named by its first heading (or file name), signed_with every company it
//   resolves when the scope has that link: one fact edge each, with quote
//   FoundryEvidence {fact_key, link, doc_id, start, end, quote, model,
//   verifier, prompt_version, confidence} -FOUNDRY_FROM_SECTION->
//   FoundrySection.
// - The Contract's props the scope lets documents fill, read from the text
//   (ATTRIBUTES): property FoundryEvidence {prop, value, norm_name, doc_id,
//   start, end, quote, ...} -FOUNDRY_ABOUT-> the Contract,
//   -FOUNDRY_FROM_SECTION-> its section. The stored value follows the set
//   of evidence, never the order documents came in: a
//   value rows set stays; evidence that agrees sets it; evidence that
//   disagrees leaves none. Either conflict is one "prop_conflict"
//   FoundryReview {type, entity_key, prop, values} -FOUNDRY_CONCERNS-> the
//   entity, whose values are the quotes against the row's value, or every
//   quote. A stored entity it names gets the props its scope lets documents
//   fill the same way (a customer's currency, "is billed in GBP").
// - A saved decision links a name to a stored entity or to none, and
//   sets a prop's value whatever the evidence says.
//
// Offsets are UTF-8 bytes; a mention's, quote's and review's are relative to
// its section's text.

export const MAX_SECTION_BYTES = 4000;
const MENTIONS_PER_SECTION = 3;
const SUFFIX =
  /\s+(?:Corporation|Corp\.?|Inc\.?|LLC|Ltd\.?|Limited|Holdings|GmbH|plc|Group)$/;
const COMPANY =
  /\b[A-Z][A-Za-z0-9&'’-]*(?: [A-Z][A-Za-z0-9&'’-]*)* (?:Corporation|Inc|LLC|Ltd|Limited|Holdings|GmbH|plc|Group)\b/g;
/**
 * Props the mock reads from text, by name: the sentence that states each,
 * and its value. Values are in force as of the document.
 */
const MONTHS = [
  "january",
  "february",
  "march",
  "april",
  "may",
  "june",
  "july",
  "august",
  "september",
  "october",
  "november",
  "december",
];
/**
 * Relationships the mock's ontologies have no link for, by what states one,
 * and the link a model suggests for each, as it spells it.
 */
const UNMATCHED: [RegExp, string][] = [
  [/\bsupplies\b/g, "supplier_of"],
  [/\bcompetes with\b/g, "competitor of"],
];
const ATTRIBUTES: Record<
  string,
  { pattern: RegExp; value: (m: RegExpExecArray) => Value | undefined }
> = {
  signed_on: {
    pattern: /\b(?:made|signed|dated) on (\d{1,2}) ([A-Z][a-z]+) (\d{4})\b/,
    value: (m) => {
      const month = MONTHS.indexOf(m[2]!.toLowerCase()) + 1;
      return month
        ? `${m[3]}-${String(month).padStart(2, "0")}-${m[1]!.padStart(2, "0")}`
        : undefined;
    },
  },
  term_months: {
    pattern: /\bruns for (\d+) months\b/,
    value: (m) => Number(m[1]),
  },
  // A customer's, in a section that names it.
  currency: {
    pattern: /\bis billed in ([A-Z]{3})\b/,
    value: (m) => m[1],
  },
};

/**
 * An identifier's normalized value by its kind, or
 * undefined when the kind rejects it.
 *
 * @example
 * normIdentifier("email", "John.Smith@Acme.com"); // "john.smith@acme.com"
 * normIdentifier("code", "INV-00123"); // "inv00123"
 */
export function normIdentifier(kind: string, value: Value): string | undefined {
  const text = String(value ?? "").normalize("NFKC");
  switch (kind) {
    case "email": {
      const v = text.trim().toLowerCase(),
        [, domain = "", ...more] = v.split("@");
      return v.split("@").length === 2 && !more.length && domain.includes(".")
        ? v
        : undefined;
    }
    case "code": {
      const v = text.toLowerCase().replace(/[^\p{L}\p{N}]/gu, "");
      return v.length >= 4 && /\p{N}/u.test(v) ? v : undefined;
    }
    case "number": {
      const v = text.replace(/[,_\s]/g, "");
      return /^-?\d+$/.test(v) && v.replace(/^-?0*/, "").length >= 4
        ? String(Number(v))
        : undefined;
    }
    case "exact": {
      const v = text.trim();
      return v || undefined;
    }
    default:
      return undefined;
  }
}

// Helix's Claude's reading models, which a load reads with unless the
// workspace chose others (mock/routes/ai.ts).
export const MODEL = "claude-haiku-5-5",
  VERIFIER = "claude-sonnet-5-5",
  PROMPT_VERSION = "7";

const bytes = (s: string) => Buffer.byteLength(s, "utf8");
const sha256 = (s: string) => createHash("sha256").update(s).digest("hex");
/**
 * How names are compared, as the engine's norm_name folds them: NFKC,
 * lowercase, other characters collapsed to one space, keeping `+ # & @ $`
 * and a dot that starts a word or follows a longer run.
 */
export const normName = (s: string) =>
  s
    .normalize("NFKC")
    .toLowerCase()
    .replace(/i̇/g, "i")
    .replace(/ς/g, "σ")
    .replace(
      /(?<=[\p{L}\p{N}]{2}|^|[^\p{L}\p{N}\p{M}+#&@$])\.(?=[\p{L}\p{N}])/gu,
      "\uE000",
    )
    .replace(/[^\p{L}\p{N}\p{M}+#&@$\uE000]+/gu, " ")
    .replace(/\uE000/g, ".")
    .trim();
type Block = { start: number; end: number; heading: boolean };

/** The document's sections: their text and UTF-16 range in it. */
export function sectionsOf(text: string, markdown: boolean) {
  const blocks: Block[] = [];
  let open: Block | null = null,
    pos = 0;
  for (const line of text.split("\n")) {
    const at = pos;
    pos += line.length + 1;
    const body = line.trim();
    if (!body) {
      open = null;
      continue;
    }
    const start = at + line.length - line.trimStart().length,
      end = at + line.trimEnd().length,
      heading = markdown && /^#{1,6}\s/.test(body);
    if (open && !heading) {
      open.end = end;
      continue;
    }
    open = { start, end, heading };
    blocks.push(open);
    // A heading is a block of its own.
    if (heading) open = null;
  }
  const sections: { start: number; end: number }[] = [];
  for (const b of blocks) {
    const last = sections.at(-1);
    if (
      last &&
      !b.heading &&
      bytes(text.slice(last.start, b.end)) <= MAX_SECTION_BYTES
    )
      last.end = b.end;
    else sections.push({ start: b.start, end: b.end });
  }
  return sections.map((s, ordinal) => ({
    ordinal,
    from: s.start,
    to: s.end,
    text: text.slice(s.start, s.end),
  }));
}

/** Every whole-word occurrence of `needle` in `text`, as UTF-16 indexes. */
function occurrences(text: string, needle: string): number[] {
  const found: number[] = [],
    word = /[\p{L}\p{N}]/u;
  for (let i = text.indexOf(needle); i >= 0; i = text.indexOf(needle, i + 1))
    if (
      !word.test(text[i - 1] || "") &&
      !word.test(text[i + needle.length] || "")
    )
      found.push(i);
  return found;
}

/** The sentence around UTF-16 index `i`: from the last stop to the next. */
function sentenceAt(text: string, i: number): [number, number] {
  let start = 0;
  for (const m of text.slice(0, i).matchAll(/[.!?]\s+|\n/g))
    start = m.index + m[0].length;
  const next = /[.!?](?=\s|$)|\n/.exec(text.slice(i));
  return [
    start,
    next ? i + next.index + (next[0] === "\n" ? 0 : 1) : text.length,
  ];
}

// ---------------------------------------------------------------- Ingest

/** One row of a mapped table: the entity it states, and the links it makes. */
export type Row = {
  type: string;
  key: string;
  name: string;
  props: Props;
  // `names` props: their normalized value is stored beside them.
  names?: string[];
  // Identifier props and their kind: the normalized value is stored beside
  // them as `<prop>__id`.
  identifiers?: Record<string, string>;
  links?: { link: string; toType: string; toKey: string }[];
};

/**
 * Creates the load's marker, or refreshes the labels it lists from what the
 * load holds. Undefined when the load's marker is for another version.
 */
export function markLoad(
  g: LoadGraph,
  ontologyVersion: string,
): { stored: string } | undefined {
  const marker = g.find(N.Load, "foundry_load", g.load);
  if (marker && marker.props.ontology_version !== ontologyVersion)
    return { stored: String(marker.props.ontology_version) };
  const nodeLabels = new Set<string>([N.Load]),
    edgeLabels = new Set<string>();
  for (const n of g.nodes()) nodeLabels.add(n.label);
  for (const e of g.graph.edges.values())
    if (e.props.foundry_load === g.load) edgeLabels.add(e.label);
  const labels = {
    node_labels: [...nodeLabels].sort(),
    edge_labels: [...edgeLabels].sort(),
  };
  if (marker) Object.assign(marker.props, labels);
  else
    g.addNode(N.Load, {
      ontology_version: ontologyVersion,
      ...labels,
      deleting: false,
    });
  return undefined;
}

/** Writes a chunk of rows unless it is stored already; false when skipped. */
export function ingestChunk(
  g: LoadGraph,
  chunk: {
    source: string;
    mapping: string;
    snapshot: string;
    ontologyVersion: string;
  },
  rows: Row[],
): boolean {
  const chunkKey = sha256(
    [chunk.source, chunk.mapping, chunk.snapshot, "0"].join("\u001f"),
  );
  if (g.find(N.Chunk, "chunk_key", chunkKey)) return false;
  const c = g.addNode(N.Chunk, {
    chunk_key: chunkKey,
    stream: `${chunk.source}\u001f${chunk.mapping}`,
    source: chunk.source,
    mapping: chunk.mapping,
    mapping_version: "draft",
    snapshot: chunk.snapshot,
    pass: "create",
    chunk: 0,
    contents: sha256(JSON.stringify(rows)),
    ontology_version: chunk.ontologyVersion,
  });
  const entity = (type: string, key: string) =>
    g.find(type, "entity_key", key)?.id;
  const evidence = (props: Props) =>
    g.addNode(N.Evidence, {
      source: chunk.source,
      snapshot: chunk.snapshot,
      mapping: chunk.mapping,
      mapping_version: "draft",
      confidence: 1,
      ...props,
    });
  for (const r of rows) {
    const nn = Object.fromEntries(
      (r.names || []).map((p) => [`${p}__nn`, normName(String(r.props[p]))]),
    );
    const ids = Object.fromEntries(
      Object.entries(r.identifiers || {}).flatMap(([p, kind]) => {
        const v = normIdentifier(kind, r.props[p] ?? null);
        return v === undefined ? [] : [[`${p}__id`, v]];
      }),
    );
    let e = entity(r.type, r.key);
    // foundry_part is create-only: the part that made the node.
    if (e === undefined)
      e = g.addNode(r.type, {
        entity_key: r.key,
        name: r.name,
        foundry_part: `${g.load}:rows`,
      });
    Object.assign(g.node(e)!.props, r.props, nn, ids);
    const ev = evidence({
      row_key: r.key,
      row_hash: sha256(r.key),
      props: Object.keys(r.props).sort(),
    });
    g.addEdge(E.FROM_CHUNK, ev, c);
    g.addEdge(E.ABOUT, ev, e);
    for (const l of r.links || []) {
      const to = entity(l.toType, l.toKey);
      if (to === undefined) continue;
      const factKey = sha256([l.link, r.type, r.key, l.toKey].join("\u001f"));
      const stored = g.findEdge(l.link, "fact_key", factKey);
      if (stored)
        stored.props.evidence_count = Number(stored.props.evidence_count) + 1;
      else
        g.addEdge(l.link, e, to, {
          fact_key: factKey,
          provenance: "stated",
          confidence: 1,
          evidence_count: 1,
          ontology_version: chunk.ontologyVersion,
        });
      const fev = evidence({
        fact_key: factKey,
        link: l.link,
        fact_source: `${factKey}\u001f${chunk.source}`,
        row_key: r.key,
        row_hash: sha256(r.key),
      });
      g.addEdge(E.FROM_CHUNK, fev, c);
    }
  }
  return true;
}

// ---------------------------------------------------------------- People enrichment

/** One `experience` entry of a profile. */
export type Experience = {
  id: number;
  title: string;
  organization: { name: string; domain: string };
  location: string;
  start_date: string;
  end_date: string | null;
  is_current: boolean;
};
/** One profile row of foundry_enrichment: its id, the clues it was found by, its fields. */
export type ProfileRow = {
  profile_id: string;
  clue_email?: string;
  clue_linkedin_url?: string;
  clue_name: string;
  fields: Record<string, Value>;
  experience: Experience[];
};
/** An [enrichments] item, as Ingest binds and states its profiles. */
export type EnrichmentBinding = {
  type: string;
  // The type's names props, whose values joined are the person's name.
  names: string[];
  clues: { email?: string; linkedinUrl?: string };
  // Target prop ← profile field.
  props: [string, string][];
  workHistory?: {
    link: string;
    to: string;
    // The `to` type's key prop, or another prop holding the domain.
    match: string;
    key: boolean;
    props: [string, string][];
  };
};

const ENRICHMENT_MAPPING = "enrichment_people",
  ENRICHMENT_TRUST = 0.7;

/**
 * Ingests the load's profiles as the engine's expansion of an [enrichments]
 * item does: each binds the one entity of the type
 * whose clue identifiers it was found by agree and whose name is its
 * clue_name (the name guard), fills the item's props with row evidence
 * {source: foundry_enrichment, row_key: profile id, props, confidence 0.7}
 * and, with work history, links the person to each employer already in the
 * graph at confidence 0.7. Never creates an entity. A profile that binds
 * nobody is `unmatched`, as is an employer the graph lacks (on the chunk).
 * The profiles are a staged JSONL file, which the engine reads pinned: the
 * chunk's and the evidence's snapshot is the file's fingerprint
 * ("sha256:…"), not a date. A load that has its chunk stored skips it.
 */
export function applyProfiles(
  g: LoadGraph,
  chunk: { ontologyVersion: string },
  item: EnrichmentBinding,
  profiles: readonly ProfileRow[],
): { applied: number; unmatched: number } | undefined {
  const snapshot = `sha256:${sha256(profiles.map((p) => JSON.stringify(p)).join("\n"))}`;
  const chunkKey = sha256(
    [ENRICHMENT_SOURCE, ENRICHMENT_MAPPING, snapshot, "0"].join("\u001f"),
  );
  if (g.find(N.Chunk, "chunk_key", chunkKey)) return undefined;
  const people = [...g.nodes()].filter((n) => n.label === item.type);
  const by = (prop: string | undefined, kind: string, value?: string) => {
    const v = prop && value ? normIdentifier(kind, value) : undefined;
    return v === undefined
      ? undefined
      : people.filter((n) => n.props[`${prop}__id`] === v);
  };
  let applied = 0,
    unmatched = 0,
    dangling = 0;
  const bound = profiles.map((p) => {
    const hits = [
      by(item.clues.email, "email", p.clue_email),
      by(item.clues.linkedinUrl, "exact", p.clue_linkedin_url),
    ].filter((h): h is typeof people => h !== undefined);
    // Each clue that hits must name one entity, the same one.
    const one = hits.every((h) => h.length === 1) ? hits[0]?.[0] : undefined;
    const agree = !!one && hits.every((h) => h[0] === one);
    const name = one
      ? item.names.map((n) => String(one.props[n] ?? "")).join(" ")
      : "";
    return agree && name === p.clue_name ? one : undefined;
  });
  const c = g.addNode(N.Chunk, {
    chunk_key: chunkKey,
    stream: `${ENRICHMENT_SOURCE}\u001f${ENRICHMENT_MAPPING}`,
    source: ENRICHMENT_SOURCE,
    mapping: ENRICHMENT_MAPPING,
    mapping_version: "draft",
    snapshot,
    pass: "resolve",
    chunk: 0,
    contents: sha256(JSON.stringify(profiles)),
    ontology_version: chunk.ontologyVersion,
  });
  const evidence = (props: Props) =>
    g.addNode(N.Evidence, {
      source: ENRICHMENT_SOURCE,
      snapshot,
      mapping: ENRICHMENT_MAPPING,
      mapping_version: "draft",
      confidence: ENRICHMENT_TRUST,
      ...props,
    });
  profiles.forEach((p, i) => {
    const person = bound[i];
    if (!person) {
      unmatched += 1;
      return;
    }
    applied += 1;
    const values = Object.fromEntries(
      item.props.flatMap(([prop, field]) =>
        p.fields[field] === undefined || p.fields[field] === null
          ? []
          : [[prop, p.fields[field]!]],
      ),
    );
    Object.assign(person.props, values);
    const ev = evidence({
      row_key: p.profile_id,
      row_hash: sha256(p.profile_id),
      props: Object.keys(values).sort(),
    });
    g.addEdge(E.FROM_CHUNK, ev, c);
    g.addEdge(E.ABOUT, ev, person.id);
    const history = item.workHistory;
    if (!history) return;
    for (const job of p.experience) {
      const domain = job.organization.domain.toLowerCase();
      const employer = history.key
        ? g.find(history.to, "entity_key", domain)
        : g.find(history.to, history.match, domain);
      if (!employer) {
        dangling += 1;
        continue;
      }
      const factKey = sha256(
        [history.link, item.type, person.props.entity_key, domain, job.id].join(
          "\u001f",
        ),
      );
      g.addEdge(history.link, person.id, employer.id, {
        // An experience field is its key, or a JSON Pointer into it
        // ("/organization/name").
        ...Object.fromEntries(
          history.props.map(([prop, field]) => [
            prop,
            ((field.startsWith("/")
              ? field
                  .slice(1)
                  .split("/")
                  .map((k) => k.replace(/~1/g, "/").replace(/~0/g, "~"))
              : [field]
            ).reduce<unknown>(
              (at, k) =>
                at && typeof at === "object"
                  ? (at as Record<string, unknown>)[k]
                  : undefined,
              job,
            ) ?? null) as Value,
          ]),
        ),
        fact_key: factKey,
        provenance: "stated",
        confidence: ENRICHMENT_TRUST,
        evidence_count: 1,
        ontology_version: chunk.ontologyVersion,
      });
      const fev = evidence({
        fact_key: factKey,
        link: history.link,
        fact_source: `${factKey}\u001f${ENRICHMENT_SOURCE}`,
        row_key: `${p.profile_id}/experience/${job.id}`,
        row_hash: sha256(`${p.profile_id}/${job.id}`),
      });
      g.addEdge(E.FROM_CHUNK, fev, c);
    }
  });
  g.node(c)!.props.unmatched = unmatched + dangling;
  return { applied, unmatched };
}

// ---------------------------------------------------------------- Extract

/**
 * What reading a document stored: the counts its document row keeps, and
 * `unresolved`, the names it matched to no record, which only its load's
 * totals show.
 */
export type ExtractCounts = {
  sections: number;
  entities: number;
  mentions: number;
  facts: number;
  // Values written: property values it stated, written as evidence.
  attributes: number;
  reviews: number;
  unresolved: number;
};

/**
 * How two values of a prop compare: strings NFKC,
 * trimmed, whitespace collapsed and case-insensitive; anything else exact.
 */
const sameValue = (a: Value, b: Value) =>
  typeof a === "string" && typeof b === "string"
    ? a.normalize("NFKC").trim().replace(/\s+/g, " ").toLowerCase() ===
      b.normalize("NFKC").trim().replace(/\s+/g, " ").toLowerCase()
    : JSON.stringify(a) === JSON.stringify(b);

/**
 * The value a table row set of an entity's prop, kept aside while a decision
 * replaces it, as the engine keeps the row's statement: what the rows and
 * quotes give without the decision, and the value again once it is removed.
 * Keyed by the entity's node.
 */
const rowValues = new WeakMap<object, Map<string, Value | undefined>>();

/**
 * Sets an entity's prop from the whole set of evidence about it, so the
 * order documents came in never matters: a decision's value (null removes
 * it) wins; a value a row set stays; evidence that agrees sets the value
 * of the most confident (ties by document id); evidence that disagrees
 * removes it. The quotes a conflict's review lists, as the engine's
 * attribute::settle gives them from contract 4.14.0, weighed without the
 * decision: those disagreeing with a row's value, or every quote when
 * documents disagree; none without a conflict. So a load lists the same
 * review before a decision, with it and after it is removed.
 */
function settle(
  g: LoadGraph,
  entity: number,
  prop: string,
  decisions: ReviewDecision[],
): Props[] {
  const node = g.node(entity)!;
  const decision = decisions.find(
    (d) =>
      d.kind === "prop" &&
      d.type === node.label &&
      d.entityKey === (node.props.entity_key ?? node.props.norm_name) &&
      d.prop === prop,
  );
  const about = [...g.graph.edges.values()]
    .filter((e) => e.label === E.ABOUT && e.to === entity)
    .flatMap((e) => {
      const n = g.node(e.from);
      return n && n.label === N.Evidence ? [n] : [];
    });
  const rowSet = about.some(
    (n) =>
      Array.isArray(n.props.props) && (n.props.props as Value[]).includes(prop),
  );
  const quotes = about
    .filter((n) => n.props.prop === prop)
    .sort(
      (a, b) =>
        Number(b.props.confidence) - Number(a.props.confidence) ||
        String(a.props.doc_id).localeCompare(String(b.props.doc_id)),
    );
  const kept = rowValues.get(node);
  const row = kept?.has(prop) ? kept.get(prop) : node.props[prop];
  const agree = quotes.every((q) =>
    sameValue(q.props.value!, quotes[0]!.props.value!),
  );
  const disagreeing = rowSet
    ? quotes.filter((q) => row === undefined || !sameValue(q.props.value!, row))
    : agree
      ? []
      : quotes;
  if (decision?.kind === "prop") {
    if (rowSet && !kept?.has(prop))
      rowValues.set(node, (kept ?? new Map()).set(prop, row));
    const v = decision.value;
    if (v.null !== undefined) delete node.props[prop];
    else if (v.integer !== undefined) node.props[prop] = Number(v.integer);
    else node.props[prop] = v.text ?? v.number ?? v.boolean!;
  } else if (kept?.has(prop)) {
    // The decision is gone: the row's value again.
    if (row === undefined) delete node.props[prop];
    else node.props[prop] = row;
    kept.delete(prop);
  } else if (rowSet) {
    // The row's value stays.
  } else if (agree && quotes.length) node.props[prop] = quotes[0]!.props.value!;
  else delete node.props[prop];
  return disagreeing.map((q) => q.props);
}

/**
 * Applies a load's value decisions to the entity of `type` and `key` (its
 * `entity_key`, or a named type's `norm_name`), as the engine does when an
 * Extract carries them: a decision of `prop` sets its value (null removes
 * it), and with none the prop is what its evidence gives: a value a table
 * row set is the row's again at once, as the engine restores it. False when
 * the load holds no such entity.
 */
export function decideValue(
  g: LoadGraph,
  type: string,
  key: string,
  prop: string,
  decisions: ReviewDecision[],
): boolean {
  for (const n of g.nodes())
    if (n.label === type && (n.props.entity_key ?? n.props.norm_name) === key) {
      settle(g, n.id, prop, decisions);
      return true;
    }
  return false;
}

/**
 * An entity text can name: a keyed entity of a scope type, by its `names`
 * prop or by the stored values of its identifier props.
 */
type Nameable = { id: number; name?: string; ids: string[] };

/** A release decision, as the mock ranks it: when it was decided, and its id. */
type Approval = Partial<Pick<FoundryReviewDecision, "id" | "decidedAt">>;
/** The approval each link that holds one value holds, by load, link and subject. */
const approvals = new Map<string, { to: number; at: string; id: string }>();
onReset(() => approvals.clear());

/**
 * From contract 4.14.0 the last decided of the approvals of a link that holds
 * one value (by `decidedAt`, then id, as the engine ranks them) is its
 * decision, whichever was written first. The engine dates it from its
 * quote's date, else its document's, else when it was decided, and a
 * statement dated later still beats it; the mock's sentences and documents
 * give no date, so it beats every other statement: `from`'s edges of `link`
 * to any other object go, an earlier approval's included, and their evidence
 * stays. `approval` (decided `to`) is written; whether it holds the link is
 * the decision's to say.
 */
function decide(
  g: LoadGraph,
  link: string,
  from: number,
  to: number,
  approval: Approval,
): Displaced[] {
  const slot = [g.load, link, from].join("\u001f"),
    at = approval.decidedAt ?? "",
    was = approvals.get(slot);
  const newest =
    !was || at > was.at || (at === was.at && (approval.id ?? "") > was.id)
      ? { to, at, id: approval.id ?? "" }
      : was;
  approvals.set(slot, newest);
  const displaced: Displaced[] = [];
  for (const e of [...g.graph.edges.values()])
    if (e.label === link && e.from === from && e.to !== newest.to) {
      displaced.push({ from: e.from, to: e.to, props: { ...e.props } });
      g.graph.removeEdge(e.id);
    }
  return displaced;
}

/** An edge an approval displaced, as it was. */
type Displaced = { from: number; to: number; props: Props };

/**
 * What a release wrote in a load, to take it back (withdrawRelease): the
 * review it took, as it was, the evidence it added, the fact it created or
 * added to, and the edges its approval displaced on a link that holds one
 * value.
 */
export type ReleaseWrite = {
  review: { props: Props; section: number; concerns: number[] };
  link: string;
  factKey: string;
  created: boolean;
  evidence: number;
  displaced: Displaced[];
  approval?: { slot: string; id: string };
};

/**
 * Takes back what a release wrote (`w`), as the engine's withdraw does
 * (contract 4.16.0): its evidence goes, and its fact unless another quote
 * holds it, the review comes back as the release took it, and the edges its
 * approval displaced come back, so the load holds what one that never had
 * the approval holds. The approval no longer decides its link.
 */
export function withdrawRelease(g: LoadGraph, w: ReleaseWrite) {
  g.graph.removeNode(w.evidence);
  const fact = g.findEdge(w.link, "fact_key", w.factKey);
  if (fact && Number(fact.props.evidence_count) > 1)
    fact.props.evidence_count = Number(fact.props.evidence_count) - 1;
  else if (fact && w.created) g.graph.removeEdge(fact.id);
  const r = g.graph.addNode(N.Review, w.review.props);
  g.addEdge(E.FROM_SECTION, r, w.review.section);
  for (const end of w.review.concerns) g.addEdge(E.CONCERNS, r, end);
  for (const e of w.displaced)
    if (
      g.node(e.from) &&
      g.node(e.to) &&
      ![...g.graph.edges.values()].some(
        (x) => x.label === w.link && x.from === e.from && x.to === e.to,
      )
    )
      g.graph.addEdge(w.link, e.from, e.to, e.props);
  if (w.approval && approvals.get(w.approval.slot)?.id === w.approval.id)
    approvals.delete(w.approval.slot);
}

/**
 * Writes what a held claim's review states as its fact, as the engine's
 * release does in a load that holds the review's document: the edge, or one
 * more piece of evidence of it, the evidence at the review's quote, at the
 * verifier's confidence (none for a claim no verifier judged), and the review
 * gone. Its ends are the entities the review concerns, in the link's order.
 * On a link that holds one value, `one` is the approval, which decides the
 * link if it is the newest (decide).
 */
export function releaseReview(
  g: LoadGraph,
  review: number,
  written: { ontologyVersion: string; model: string; verifier: string },
  one?: Approval,
): ReleaseWrite | undefined {
  const r = g.node(review);
  if (!r) return;
  const out = (g.graph.outgoing.get(review) || []).map(
    (id) => g.graph.edges.get(id)!,
  );
  const [from, to] = out.filter((e) => e.label === E.CONCERNS).map((e) => e.to);
  const section = out.find((e) => e.label === E.FROM_SECTION)?.to;
  const link = String(r.props.suggested_link);
  if (from === undefined || to === undefined || section === undefined) return;
  const confidence = Number(r.props.confidence ?? 0);
  const factKey = sha256([link, String(from), String(to)].join("\u001f"));
  const stored = g.findEdge(link, "fact_key", factKey);
  const concerns = out.filter((e) => e.label === E.CONCERNS).map((e) => e.to);
  const props = { ...r.props };
  if (stored)
    stored.props.evidence_count = Number(stored.props.evidence_count) + 1;
  else
    g.addEdge(link, from, to, {
      fact_key: factKey,
      provenance: "extracted",
      confidence,
      evidence_count: 1,
      ontology_version: written.ontologyVersion,
    });
  const ev = g.addNode(N.Evidence, {
    fact_key: factKey,
    link,
    doc_id: r.props.doc_id!,
    start: r.props.start!,
    end: r.props.end!,
    quote: r.props.quote!,
    model: written.model,
    verifier: written.verifier,
    prompt_version: PROMPT_VERSION,
    confidence,
  });
  g.addEdge(E.FROM_SECTION, ev, section);
  g.graph.removeNode(review);
  return {
    review: { props, section, concerns },
    link,
    factKey,
    created: !stored,
    evidence: ev,
    displaced: one ? decide(g, link, from, to, one) : [],
    ...(one
      ? {
          approval: {
            slot: [g.load, link, from].join("\u001f"),
            id: one.id ?? "",
          },
        }
      : {}),
  };
}

export type ExtractOutcome =
  | { kind: "ingested"; counts: ExtractCounts }
  | { kind: "out_of_scope"; counts: ExtractCounts }
  | { kind: "already_ingested" }
  | { kind: "duplicate"; of: string };

/**
 * A link on less than a name or a verified identifier, as the engine's
 * resolution names it: each has a review a person can overturn, and holds an
 * approval at its end.
 */
export type WeakLink = "legal_form" | "part_name" | "own_name";
const WEAK_LINKS: readonly WeakLink[] = ["legal_form", "part_name", "own_name"];
/** A link the load made that a person can undo: the model's, or a weak one. */
export type UndoableLink = "adjudicated" | WeakLink;

/** What extraction reads of the version's ontology.toml. */
export type ExtractOntology = {
  types: Set<string>;
  // The links that hold one value (`cardinality = "one"`).
  oneValued?: Set<string>;
  // A type's first `names` prop.
  names: Map<string, string>;
  // A type's identifier props and their kind.
  identifiers?: Map<string, Record<string, string>>;
};

export function extractDocument(
  g: LoadGraph,
  doc: {
    id: string;
    name: string;
    text: string;
    markdown: boolean;
    ontologyVersion: string;
  },
  ontology: ExtractOntology,
  scope: Scope,
  decisions: (ReviewDecision & Approval)[] = [],
  // How a name no row holds exactly links to its first candidate, if at all.
  linkedBy?: UndoableLink,
  // What the engine would make of a hedged claim, for a page to show:
  // "refused" holds it however it's approved; "legal_form", "part_name" and
  // "own_name" too, with its name linked that way and that link's review;
  // "differs" gives its place a review stating something else, and refuses
  // it; "unverified" is one no verifier judged, written at no confidence;
  // "one_end" names one of its ends; "no_statement" is one an older load
  // held, with no statement to approve; "conflict" is one held as
  // conflicting with what its link holds, which an approval writes on a link
  // that holds one value (contract 4.14.0), displacing what it held.
  held?:
    | "refused"
    | WeakLink
    | "differs"
    | "unverified"
    | "one_end"
    | "no_statement"
    | "conflict",
  // The models that read and check it.
  readers: { model: string; verifier: string } = {
    model: MODEL,
    verifier: VERIFIER,
  },
  // Whether a sentence stating a relationship no link covers (UNMATCHED) is
  // an "unmatched" review, as the engine reports one.
  unmatched = false,
): ExtractOutcome {
  const { model, verifier } = readers;
  const contentHash = sha256(doc.text);
  if (g.find(N.Document, "doc_id", doc.id)) return { kind: "already_ingested" };
  const same = g.find(N.Document, "content_hash", contentHash);
  if (same) return { kind: "duplicate", of: String(same.props.doc_id) };

  const d = g.addNode(N.Document, {
    doc_id: doc.id,
    uri: `s3://foundry/documents/${doc.id}/original`,
    media_type: doc.markdown ? "markdown" : "plain_text",
    content_hash: contentHash,
    ontology_version: doc.ontologyVersion,
  });
  const artifactKey = sha256(
    [contentHash, model, verifier, PROMPT_VERSION].join("\u001f"),
  );
  const sections = sectionsOf(doc.text, doc.markdown).map((s) => {
    const node = g.addNode(N.Section, {
      section_id: `${doc.id}#${s.ordinal}`,
      doc_id: doc.id,
      ordinal: s.ordinal,
      start: bytes(doc.text.slice(0, s.from)),
      end: bytes(doc.text.slice(0, s.to)),
      hash: sha256(s.text),
      text: s.text,
      artifact_key: artifactKey,
      slice_hash: sha256(s.text),
      roster_hash: sha256(doc.id),
      model,
      verifier,
      prompt_version: PROMPT_VERSION,
    });
    g.addEdge(E.IN_DOCUMENT, node, d);
    return { ...s, node };
  });
  const span = (section: string, from: number, to: number) => ({
    start: bytes(section.slice(0, from)),
    end: bytes(section.slice(0, to)),
  });
  const empty = { sections: sections.length, entities: 0, mentions: 0 };
  // A set no route reads: stored with its sections, nothing read.
  if (isEmpty(scope))
    return {
      kind: "out_of_scope",
      counts: { ...empty, facts: 0, attributes: 0, reviews: 0, unresolved: 0 },
    };

  // Keyed entities of the scope's types text can name: by their `names`
  // prop's value, or by an identifier's stored value.
  const entities: Nameable[] = [...g.nodes()].flatMap((n) => {
    if (!scope.types.has(n.label) || n.props.entity_key === undefined)
      return [];
    const prop = ontology.names.get(n.label);
    const value = prop && n.props[prop];
    const ids = Object.entries(ontology.identifiers?.get(n.label) || {})
      .filter(([p, kind]) => kind !== "number" && n.props[p] !== undefined)
      .map(([p]) => String(n.props[p]))
      .filter((v) => v.length >= 4);
    const name = typeof value === "string" ? value : undefined;
    return name || ids.length ? [{ id: n.id, name, ids }] : [];
  });
  const alias = (e: Nameable) => (e.name || "").replace(SUFFIX, "");
  const sharedAlias = (e: Nameable) =>
    entities.some(
      (o) =>
        o.id !== e.id && !!o.name && normName(alias(o)) === normName(alias(e)),
    );
  const found = entities.flatMap((e) => {
    const name = e.name || "",
      short = name && alias(e) !== name ? alias(e) : "";
    const hits = sections.flatMap((s) => {
      const full = name
        ? occurrences(s.text, name).map((i) => ({
            s,
            i,
            surface: name,
            by: "name",
          }))
        : [];
      const brief = short
        ? occurrences(s.text, short)
            .filter((i) => !full.some((f) => i >= f.i && i < f.i + name.length))
            .map((i) => ({ s, i, surface: short, by: "alias" }))
        : [];
      const lower = s.text.toLowerCase();
      const quoted = e.ids.flatMap((v) =>
        occurrences(lower, v.toLowerCase()).map((i) => ({
          s,
          i,
          surface: s.text.slice(i, i + v.length),
          by: "identifier",
        })),
      );
      return [...full, ...brief, ...quoted].sort((a, b) => a.i - b.i);
    });
    // By its full name or an identifier, else by a short name no other has.
    const sure = hits.find((h) => h.by !== "alias");
    if (!hits.length || (!sure && sharedAlias(e))) return [];
    return [{ e, hits, resolvedBy: sure?.by || "alias" }];
  });
  const known = new Set(
    entities.flatMap((e) =>
      e.name ? [normName(e.name), normName(alias(e))] : [],
    ),
  );
  const unknown = sections.flatMap((s) =>
    [...s.text.matchAll(COMPANY)]
      .filter(
        (m) =>
          !known.has(normName(m[0])) &&
          !known.has(normName(m[0].replace(SUFFIX, ""))),
      )
      .map((m) => ({ s, i: m.index, name: m[0] })),
  );

  const resolution = (surface: string, type: string) =>
    decisions.find(
      (d) =>
        d.kind === "resolve" &&
        d.docId === doc.id &&
        d.type === type &&
        d.normName === normName(surface),
    );
  for (const match of [...found]) {
    match.hits = match.hits.filter((h) => {
      const type = g.node(match.e.id)!.label;
      const d =
        resolution(match.e.name || h.surface, type) ??
        resolution(h.surface, type);
      if (d?.kind !== "resolve") return true;
      if (d.entityKey !== g.node(match.e.id)!.props.entity_key) {
        const target = entities.find(
          (e) =>
            g.node(e.id)?.label === d.type &&
            g.node(e.id)?.props.entity_key === d.entityKey,
        );
        if (target) {
          const existing = found.find((x) => x.e.id === target.id);
          if (existing) existing.hits.push({ ...h, by: "user" });
          else
            found.push({
              e: target,
              hits: [{ ...h, by: "user" }],
              resolvedBy: "user",
            });
        }
        return false;
      }
      h.by = "user";
      return true;
    });
  }
  // A variant the exact ladder missed can link only by a saved decision.
  const candidates = (name: string) =>
    entities
      .filter(
        (e) =>
          g.node(e.id)?.label === "Customer" &&
          g.node(e.id)?.props.foundry_part === `${g.load}:rows` &&
          normName(e.name || "")
            .split(" ")
            .some((w) => normName(name).split(" ").includes(w)),
      )
      .slice(0, 5);
  const adjudicated: typeof unknown = [];
  const unresolved = unknown.filter((u) => {
    const d = resolution(u.name, "Customer");
    if (d?.kind !== "resolve") {
      const e = linkedBy ? candidates(u.name)[0] : undefined;
      if (!e) return true;
      found.push({
        e,
        hits: [{ s: u.s, i: u.i, surface: u.name, by: linkedBy! }],
        resolvedBy: linkedBy!,
      });
      adjudicated.push(u);
      return false;
    }
    if (d.entityKey === undefined) return false;
    const e = entities.find(
      (e) =>
        g.node(e.id)?.props.entity_key === d.entityKey &&
        g.node(e.id)?.label === d.type,
    );
    if (!e) return true;
    found.push({
      e,
      hits: [{ s: u.s, i: u.i, surface: u.name, by: "user" }],
      resolvedBy: "user",
    });
    return false;
  });

  const counts: ExtractCounts = {
    ...empty,
    facts: 0,
    attributes: 0,
    reviews: 0,
    unresolved: unresolved.length,
  };
  const mention = (
    s: (typeof sections)[number],
    i: number,
    surface: string,
    entity: number,
    resolvedBy: string,
  ) => {
    const m = g.addNode(N.Mention, {
      doc_id: doc.id,
      ...span(s.text, i, i + surface.length),
      surface,
      resolved_by: resolvedBy,
    });
    g.addEdge(E.IN_SECTION, m, s.node);
    g.addEdge(E.MENTION_OF, m, entity);
    counts.mentions += 1;
  };
  const quote = (s: (typeof sections)[number], i: number) => {
    const [from, to] = sentenceAt(s.text, i);
    return { ...span(s.text, from, to), quote: s.text.slice(from, to) };
  };
  // A prop's value the text states: its evidence, then the stored value
  // the whole set of evidence decides.
  const attribute = (
    entity: number,
    prop: string,
    value: Value,
    s: (typeof sections)[number],
    i: number,
  ) => {
    const node = g.node(entity)!;
    const ev = g.addNode(N.Evidence, {
      prop,
      value,
      ...(node.props.entity_key !== undefined
        ? { entity_key: node.props.entity_key }
        : { norm_name: node.props.norm_name ?? null }),
      doc_id: doc.id,
      section: s.ordinal,
      ...quote(s, i),
      model,
      verifier,
      prompt_version: PROMPT_VERSION,
      confidence: 0.94,
    });
    g.addEdge(E.ABOUT, ev, entity);
    g.addEdge(E.FROM_SECTION, ev, s.node);
    counts.attributes += 1;
    const disagreeing = settle(g, entity, prop, decisions);
    // Only a document that adds to a conflict writes its review: one per
    // entity and prop, its values updated as later documents add quotes.
    if (!disagreeing.some((q) => q.doc_id === doc.id)) return;
    const values = JSON.stringify(
      disagreeing.map((q) => ({
        value: q.value,
        doc_id: q.doc_id,
        span: { section: q.section, start: q.start, end: q.end },
        quote: q.quote,
        confidence: q.confidence,
      })),
    );
    const held = [...g.graph.edges.values()].find(
      (e) =>
        e.label === E.CONCERNS &&
        e.to === entity &&
        g.node(e.from)?.props.reason === "prop_conflict" &&
        g.node(e.from)?.props.prop === prop,
    );
    if (held) {
      g.node(held.from)!.props.values = values;
      return;
    }
    const r = g.addNode(N.Review, {
      review_key: sha256(`prop_conflict\u001f${entity}\u001f${prop}`),
      doc_id: doc.id,
      section: s.ordinal,
      ...quote(s, i),
      reason: "prop_conflict",
      type: node.label,
      entity_key: node.props.entity_key ?? node.props.norm_name ?? "",
      values,
      prop,
      candidates: [],
      confidence: 0.94,
    });
    g.addEdge(E.FROM_SECTION, r, s.node);
    g.addEdge(E.CONCERNS, r, entity);
    counts.reviews += 1;
  };

  // The Contract the document is, when the scope lets documents create one.
  let contract: number | undefined;
  const contracts = scope.types.get("Contract");
  if (
    ontology.types.has("Contract") &&
    contracts?.create &&
    (found.length || unknown.length)
  ) {
    const heading = doc.markdown
      ? /^#{1,6}\s+(.+)$/m.exec(doc.text)?.[1]?.trim()
      : undefined;
    const title = heading || doc.name.replace(/\.[^.]+$/, "");
    const stored = g.find("Contract", "norm_name", normName(title))?.id;
    contract =
      stored ??
      g.addNode("Contract", {
        norm_name: normName(title),
        name: title,
        foundry_part: `${g.load}:documents`,
      });
    counts.entities += 1;
    const titled = heading
      ? sections.find((s) => s.text.includes(heading))
      : undefined;
    if (titled)
      mention(
        titled,
        titled.text.indexOf(heading!),
        heading!,
        contract,
        stored === undefined ? "created" : "name",
      );
    for (const prop of contracts.props) {
      const read = ATTRIBUTES[prop];
      for (const s of read ? sections : []) {
        const m = read!.pattern.exec(s.text);
        const value = m && read!.value(m);
        if (!m || value === undefined) continue;
        attribute(contract, prop, value, s, m.index);
        break;
      }
    }
  }
  // Props of the stored entities it names that the scope lets it fill,
  // from a section naming the entity.
  for (const { e, hits } of found) {
    for (const prop of scope.types.get(g.node(e.id)!.label)?.props || []) {
      const read = ATTRIBUTES[prop];
      const at = sections
        .filter((s) => hits.some((h) => h.s === s))
        .flatMap((s) => {
          const m = read?.pattern.exec(s.text);
          const value = m && read!.value(m);
          return m && value !== undefined ? [{ s, m, value }] : [];
        })[0];
      if (at) attribute(e.id, prop, at.value, at.s, at.m.index);
    }
  }
  const linked = contract !== undefined && scope.links.has("signed_with");
  // The customer a person's approval gives a contract's signed_with, where
  // that link holds one value: the newest approval of the document's, which
  // decides the link if it is the newest the load has written (decide).
  let approved: { to: number; release: Approval } | undefined;
  for (const { e, hits, resolvedBy } of found) {
    if (!hits.length) continue;
    counts.entities += 1;
    for (const s of sections)
      for (const h of hits
        .filter((x) => x.s === s)
        .slice(0, MENTIONS_PER_SECTION))
        mention(s, h.i, h.surface, e.id, h.by);
    if (!linked) continue;
    const first = hits[0]!,
      confidence =
        resolvedBy === "identifier" ||
        resolvedBy === "adjudicated" ||
        WEAK_LINKS.some((w) => w === resolvedBy)
          ? 0.95
          : resolvedBy === "name"
            ? 0.93
            : 0.81;
    // A hedged sentence is a claim a verifier doubts: held, until a person
    // releases it by its key, the same in every load, and written at the
    // verifier's confidence.
    let stated = confidence;
    // A claim a person's approval released: its edge stands until the
    // document's decide says which approval holds the link.
    let approval = false;
    const [from, to] = sentenceAt(first.s.text, first.i);
    if (/\bmay\b/.test(first.s.text.slice(from, to))) {
      const about = [
        g.node(contract!)!.props.norm_name,
        g.node(e.id)!.props.entity_key ?? g.node(e.id)!.props.norm_name,
      ];
      const claim = JSON.stringify({
        link: "signed_with",
        about: held === "one_end" ? about.slice(1) : about,
      });
      const claimKey = sha256(`${doc.id}\u001f${claim}`);
      const place = quote(first.s, first.i);
      const statement = JSON.stringify({
        subject: { type: "Contract", name: g.node(contract!)!.props.name },
        object: { type: g.node(e.id)!.label, name: e.name || first.surface },
        props: {},
      });
      // By its key, or, as the engine binds one whose key names no review
      // of the document (contract 4.14.0), by its place and statement.
      const at = (d: ReviewDecision) =>
        d.kind === "release" &&
        d.docId === doc.id &&
        d.claimSection === first.s.ordinal &&
        d.claimStart === place.start &&
        d.claimEnd === place.end &&
        d.statementHash === sha256(`signed_with\u001f${statement}`);
      const release =
        decisions.find(
          (d) => d.kind === "release" && d.claimKey === claimKey && at(d),
        ) ?? decisions.find(at);
      const released = !!release;
      // The link its name stands on, where that holds the claim.
      const weak = WEAK_LINKS.find((w) => w === held);
      const refused = held === "refused" || !!weak || held === "differs";
      if (!released || refused) {
        const r = g.addNode(N.Review, {
          review_key: sha256(`${doc.id}\u001f${claimKey}`),
          doc_id: doc.id,
          section: first.s.ordinal,
          ...place,
          reason:
            held === "unverified" || held === "conflict"
              ? held
              : "low_confidence",
          claim_key: claimKey,
          claim,
          ...(held === "no_statement" ? {} : { statement }),
          suggested_link: "signed_with",
          ...(held === "unverified" ? {} : { confidence: 0.72 }),
        });
        g.addEdge(E.FROM_SECTION, r, first.s.node);
        g.addEdge(E.CONCERNS, r, contract!);
        g.addEdge(E.CONCERNS, r, e.id);
        counts.reviews += 1;
        if (held === "differs") {
          const other = g.addNode(N.Review, {
            review_key: sha256(`${doc.id}\u001f${claimKey}\u001fother`),
            doc_id: doc.id,
            section: first.s.ordinal,
            ...place,
            reason: "conflict",
            claim_key: claimKey,
            claim,
            statement: statement.replace('"props":{}', '"props":{"term":"24"}'),
            suggested_link: "signed_with",
          });
          g.addEdge(E.FROM_SECTION, other, first.s.node);
          g.addEdge(E.CONCERNS, other, e.id);
          counts.reviews += 1;
        }
        // At the name's first mention, as the engine writes a link's
        // review: not at the claim's sentence.
        if (weak) {
          const key = String(g.node(e.id)!.props.entity_key);
          const linked = g.addNode(N.Review, {
            review_key: sha256(`${doc.id}\u001f${claimKey}\u001fform`),
            doc_id: doc.id,
            section: first.s.ordinal,
            ...span(first.s.text, first.i, first.i + first.surface.length),
            quote: first.surface,
            reason: weak,
            type: "Customer",
            norm_name: normName(e.name || first.surface),
            entity_key: key,
            candidates: [key],
          });
          g.addEdge(E.FROM_SECTION, linked, first.s.node);
          g.addEdge(E.CONCERNS, linked, e.id);
          counts.reviews += 1;
        }
        continue;
      }
      stated = held === "unverified" ? 0 : 0.72;
      approval = true;
      const r = release!;
      if (
        ontology.oneValued?.has("signed_with") &&
        (!approved ||
          (r.decidedAt ?? "") > (approved.release.decidedAt ?? "") ||
          ((r.decidedAt ?? "") === (approved.release.decidedAt ?? "") &&
            (r.id ?? "") > (approved.release.id ?? "")))
      )
        approved = { to: e.id, release: r };
    }
    const factKey = sha256(
      ["signed_with", String(contract), String(e.id)].join("\u001f"),
    );
    const stored = g.findEdge("signed_with", "fact_key", factKey);
    // Where an approval the load wrote decides the contract's link, a plain
    // statement of another customer loses to it whichever commits first:
    // its quote is evidence alone (decide). An approval's own edge is
    // written, and decide keeps the newest approval's.
    const holder = ontology.oneValued?.has("signed_with")
      ? approvals.get([g.load, "signed_with", contract].join("\u001f"))
      : undefined;
    if (stored)
      stored.props.evidence_count = Number(stored.props.evidence_count) + 1;
    else if (approval || !holder || holder.to === e.id)
      g.addEdge("signed_with", contract!, e.id, {
        fact_key: factKey,
        provenance: "extracted",
        confidence: stated,
        evidence_count: 1,
        ontology_version: doc.ontologyVersion,
      });
    const ev = g.addNode(N.Evidence, {
      fact_key: factKey,
      link: "signed_with",
      doc_id: doc.id,
      ...quote(first.s, first.i),
      model,
      verifier,
      prompt_version: PROMPT_VERSION,
      confidence: stated,
    });
    g.addEdge(E.FROM_SECTION, ev, first.s.node);
    counts.facts += 1;
  }
  if (approved)
    decide(g, "signed_with", contract!, approved.to, approved.release);
  [...unresolved, ...adjudicated].forEach((u, k) => {
    const q = quote(u.s, u.i);
    const r = g.addNode(N.Review, {
      review_key: sha256(`${doc.id}\u001f${k}\u001f${u.name}`),
      doc_id: doc.id,
      section: u.s.ordinal,
      ...q,
      reason: adjudicated.includes(u) ? linkedBy! : "unresolved",
      ...(adjudicated.includes(u)
        ? {
            entity_key: String(
              g.node(candidates(u.name)[0]!.id)!.props.entity_key,
            ),
          }
        : {}),
      type: "Customer",
      norm_name: normName(u.name),
      // The stored identities: an adjudicated review lists its choice only.
      candidates: candidates(u.name)
        .slice(0, adjudicated.includes(u) ? 1 : undefined)
        .map((e) => String(g.node(e.id)!.props.entity_key)),
      suggested_link: "signed_with",
      // A link on less than a name was judged by no model.
      ...(!adjudicated.includes(u)
        ? { confidence: 0.88 }
        : linkedBy === "adjudicated"
          ? { confidence: 0.95 }
          : {}),
    });
    g.addEdge(E.FROM_SECTION, r, u.s.node);
    if (contract !== undefined) g.addEdge(E.CONCERNS, r, contract);
    for (const key of g.node(r)!.props.candidates as string[]) {
      const candidate = g.find("Customer", "entity_key", key);
      if (candidate) g.addEdge(E.CONCERNS, r, candidate.id);
    }
    counts.reviews += 1;
  });
  // A review of the sentence, naming the link a model would suggest, which
  // the version may not have: nothing it states is stored.
  if (unmatched)
    for (const s of sections)
      for (const [pattern, link] of UNMATCHED)
        for (const m of s.text.matchAll(pattern)) {
          const q = quote(s, m.index);
          const r = g.addNode(N.Review, {
            review_key: sha256(
              [doc.id, "unmatched", s.ordinal, q.start, link].join("\u001f"),
            ),
            doc_id: doc.id,
            section: s.ordinal,
            ...q,
            reason: "unmatched",
            suggested_link: link,
            confidence: 0.8,
          });
          g.addEdge(E.FROM_SECTION, r, s.node);
          counts.reviews += 1;
        }
  return { kind: "ingested", counts };
}
