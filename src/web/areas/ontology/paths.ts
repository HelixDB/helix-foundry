// Ontology area URLs, merged into `paths` (src/web/paths.ts). Owned by the
// ontology area; do not import ../../paths here (it imports this file). An
// ontology's own pages sit under /ontology/{ontologyId}; what reads its
// database has a section of its own: the Explorer under
// /explore/{ontologyId} and Ask under /ask/{ontologyId}.
import type {
  FoundryEventSourceMode,
  FoundryEventSourcePick,
  OntologyDocumentStatus,
} from "../../../shared/wfe";

/**
 * How a link (`?event_sources=<id>:structured,<id>:extraction`) writes the
 * mode an ontology reads an API source in; model.ts's startingEventSources
 * reads it back.
 */
export const EVENT_SOURCE_MODE_WORDS: Record<FoundryEventSourceMode, string> = {
  FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED: "structured",
  FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION: "extraction",
};

const enc = encodeURIComponent;
const at = (ontologyId: string) => "/ontology/" + enc(ontologyId),
  explorer = (ontologyId: string) => "/explore/" + enc(ontologyId);
export const ontologyPaths = {
  // The workspace's ontologies, and creating one.
  ontologies: () => "/ontology",
  newOntology: () => "/ontology/new",
  // An ontology: its current version, drawn, then its schema versions and
  // data loads, each a section of the page.
  ontology: (ontologyId: string) => at(ontologyId),
  versions: (ontologyId: string) => at(ontologyId) + "#versions",
  // A version; `tab` is one of its viewer's tabs, and `item` the item its
  // Edit tab opens ("type.Person").
  version: (ontologyId: string, id: string, tab?: string, item?: string) => {
    const q = new URLSearchParams();
    if (tab) q.set("tab", tab);
    if (item) q.set("item", item);
    return at(ontologyId) + "/versions/" + enc(id) + (q.size ? "?" + q : "");
  },
  // A generation: a draft's progress, or why it failed. Its list is the
  // versions' section.
  generation: (ontologyId: string, id: string) =>
    at(ontologyId) + "/generations/" + enc(id),
  // The generate form, prefilled from an earlier generation's goal,
  // connectors (none for an empty list; every synced one when left out),
  // API sources with their modes (none for an empty list; the current
  // version's generation's when left out) and document sets.
  generate: (
    ontologyId: string,
    prefill?: {
      goal?: string;
      connectorIds?: string[];
      eventSources?: FoundryEventSourcePick[];
      documentSetIds?: string[];
    },
  ) => {
    const q = new URLSearchParams();
    if (prefill?.goal) q.set("goal", prefill.goal);
    if (prefill?.connectorIds)
      q.set("connectors", prefill.connectorIds.join(","));
    if (prefill?.eventSources)
      q.set(
        "event_sources",
        prefill.eventSources
          .map((p) => `${p.connectorId}:${EVENT_SOURCE_MODE_WORDS[p.mode]}`)
          .join(","),
      );
    if (prefill?.documentSetIds)
      q.set("sets", prefill.documentSetIds.join(","));
    return at(ontologyId) + "/generate" + (q.size ? "?" + q : "");
  },
  // Loads of the ontology's database (the overview's section), the overview
  // with its Load dialog open, and one of them.
  loads: (ontologyId: string) => at(ontologyId) + "#loads",
  newLoad: (ontologyId: string) => at(ontologyId) + "?new-load=1",
  load: (ontologyId: string, id: string) =>
    at(ontologyId) + "/loads/" + enc(id),
  // What a load's version's constraints found in it: one constraint's
  // breaks, from the record `key` (an Ask citation's).
  loadRuleBreaks: (
    ontologyId: string,
    id: string,
    constraint?: string,
    key?: string,
  ) =>
    at(ontologyId) +
    "/loads/" +
    enc(id) +
    "/rule-breaks" +
    (constraint ? "?constraint=" + enc(constraint) : "") +
    (constraint && key ? "&key=" + enc(key) : ""),
  // What the ontology reads of the workspace's documents, those its current
  // load holds as `status` says, and one; `tab` is one of its page's tabs,
  // and `span` a span of one of its sections (UTF-8 byte offsets into the
  // section's text) the page marks and opens on.
  documents: (ontologyId: string, status?: OntologyDocumentStatus) =>
    at(ontologyId) + "/documents" + (status ? "?status=" + enc(status) : ""),
  document: (
    ontologyId: string,
    id: string,
    tab?: string,
    span?: { section: number; start: number; end: number },
  ) =>
    at(ontologyId) +
    "/documents/" +
    enc(id) +
    (tab ? "?tab=" + enc(tab) : "") +
    (span ? `#s${span.section}-${span.start}-${span.end}` : ""),
  // The database's graph explorer, selecting a node by its database id.
  explore: (ontologyId: string, node?: number) =>
    explorer(ontologyId) + (node === undefined ? "" : "?node=" + node),
  // Its records by type, opening on `type`; the page keeps its view in the
  // query string (areas/records/model.ts readView).
  records: (ontologyId: string, type?: string) =>
    explorer(ontologyId) + "/records" + (type ? "?type=" + enc(type) : ""),
  // Questions about a load: a thread, or a new one, optionally about one
  // entity (its entity id); `new` opens the new thread where a narrow screen
  // would show the threads. The page keeps `?load=` as the other views do.
  ask: (
    ontologyId: string,
    view?: { thread?: string; focus?: string; new?: boolean },
  ) => {
    const q = new URLSearchParams();
    if (view?.thread) q.set("thread", view.thread);
    if (view?.focus) q.set("focus", view.focus);
    if (view?.new) q.set("new", "1");
    return "/ask/" + enc(ontologyId) + (q.size ? "?" + q : "");
  },
  // One entity of the ontology, in the Explorer.
  entity: (ontologyId: string, id: string, tab?: string) =>
    explorer(ontologyId) +
    "/entities/" +
    enc(id) +
    (tab ? "?tab=" + enc(tab) : ""),
};
