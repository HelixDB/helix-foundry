// The labels of Foundry's own bookkeeping in an ontology's database: what a
// load writes beside the ontology's entities and facts (the ontology agent's
// graph layout, oa crates/extraction/src/store/mod.rs). Every node label
// starts with "Foundry" and every edge label with "FOUNDRY_", and the agent
// refuses ontology types and links named so, so a label alone says whether
// it is the ontology's or Foundry's. The SPA's reads and the mock's writes
// both take their labels from here.

/** Foundry's node labels, by what they were called before the prefix. */
export const FOUNDRY_NODES = Object.freeze({
  Document: "FoundryDocument",
  Section: "FoundrySection",
  Mention: "FoundryMention",
  Evidence: "FoundryEvidence",
  Chunk: "FoundryChunk",
  Review: "FoundryReview",
  Swept: "FoundrySwept",
  Violation: "FoundryViolation",
  // One per request that created elements: its `foundry_tx`, when it
  // committed, its origin and its run.
  Tx: "FoundryTx",
  // The state a request overwrote or closed, by its `foundry_tx`.
  Version: "FoundryVersion",
  // One per load: its version and every label it wrote, for deleting it.
  Load: "FoundryLoad",
} as const);

/** Foundry's edge labels, by what they were called before the prefix. */
export const FOUNDRY_EDGES = Object.freeze({
  IN_DOCUMENT: "FOUNDRY_IN_DOCUMENT",
  IN_SECTION: "FOUNDRY_IN_SECTION",
  MENTION_OF: "FOUNDRY_MENTION_OF",
  FROM_SECTION: "FOUNDRY_FROM_SECTION",
  FROM_CHUNK: "FOUNDRY_FROM_CHUNK",
  CONCERNS: "FOUNDRY_CONCERNS",
  ABOUT: "FOUNDRY_ABOUT",
} as const);

export type FoundryNodeLabel =
  (typeof FOUNDRY_NODES)[keyof typeof FOUNDRY_NODES];
export type FoundryEdgeLabel =
  (typeof FOUNDRY_EDGES)[keyof typeof FOUNDRY_EDGES];

/** Every node label Foundry writes today. */
export const FOUNDRY_NODE_LABELS: readonly FoundryNodeLabel[] = Object.freeze(
  Object.values(FOUNDRY_NODES),
);
/** Every edge label Foundry writes today. */
export const FOUNDRY_EDGE_LABELS: readonly FoundryEdgeLabel[] = Object.freeze(
  Object.values(FOUNDRY_EDGES),
);

/**
 * The property every node and edge a load writes carries: the load's id.
 * Equality-indexed per label, so a label's nodes in one load are a bitmap
 * read. Nodes and edges without it are not Foundry's (Other data).
 */
export const FOUNDRY_LOAD = "foundry_load";

/**
 * The prefix of every property Foundry stamps on what a load writes: its
 * load (`foundry_load`), the request that created it (`foundry_tx`), the
 * part of the load that made an entity (`foundry_part`). On the ontology's
 * entities and facts they are the layout's, never the data's.
 */
export const FOUNDRY_PROP_PREFIX = "foundry_";

export const FOUNDRY_NODE_PREFIX = "Foundry";
export const FOUNDRY_EDGE_PREFIX = "FOUNDRY_";

/**
 * Whether a label is Foundry's own rather than the ontology's: a node label
 * starting with "Foundry", an edge label starting with "FOUNDRY_". The
 * prefix decides, so a label a later agent adds is Foundry's too.
 *
 * @example
 * isFoundryLabel("node", "FoundryMention"); // true
 * isFoundryLabel("edge", "signed_with"); // false
 */
export function isFoundryLabel(
  target: "node" | "edge",
  label: string,
): boolean {
  return label.startsWith(
    target === "node" ? FOUNDRY_NODE_PREFIX : FOUNDRY_EDGE_PREFIX,
  );
}

/**
 * Whether a property is a stamp Foundry put on an element rather than its
 * data: the load (`foundry_load`) on anything, any other `foundry_*` on all
 * but Foundry's own labels, where it is the data (a FoundryTx is its
 * `foundry_tx`). Property lists and the Explorer's search leave stamps out.
 *
 * @example
 * isFoundryStamp("node", "Customer", "foundry_tx"); // true
 * isFoundryStamp("node", "FoundryTx", "foundry_tx"); // false
 * isFoundryStamp("node", "FoundryTx", "foundry_load"); // true
 */
export function isFoundryStamp(
  target: "node" | "edge",
  label: string,
  key: string,
): boolean {
  return (
    key.startsWith(FOUNDRY_PROP_PREFIX) &&
    (key === FOUNDRY_LOAD || !isFoundryLabel(target, label))
  );
}
