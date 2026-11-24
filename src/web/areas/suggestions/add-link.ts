// A finding's "Add this link", in place: the edits that add its link to the
// current version and route it for its set's documents, and where the move
// of the current load to the version that adds it stands. Making that
// version current with the finding's fingerprint (`fillFinding`) moves the
// load in place when the version only adds (contract 4.32.0's fold): no new
// load, no model call. Documents read from then on state the link; those the
// load holds keep their reviews until they are read again.
import * as toml from "smol-toml";
import type * as wfe from "../../../shared/wfe";
import { editItems } from "../ontology/edit-model";
import { routesReading, type AgentBundle } from "../ontology/model";

/** What a person adds: the link's name, its ends and its description. */
export type AddLink = {
  // The finding's canonical name, which the link is named.
  link: string;
  // The finding's document set, whose routes gain the link.
  setId: string;
  from: string;
  to: string;
  description: string;
};

/** The edits that add a link, or why none can. */
export type AddLinkEdits =
  | { kind: "edits"; edits: wfe.OntologyEdit[] }
  | { kind: "refused"; why: string };

/**
 * The edits that add `a.link` to the bundle and route it for `a.setId`'s
 * documents: the link's put, unless the bundle declares it already (a put of
 * a declared link would change it, which needs a new load), and the put of
 * the route that reads the set, with the link added to its `links`. The
 * route chosen names the set over one that reads every set, then by name.
 * Refused when a route already states it for the set, when no route reads
 * the set, or when the ends aren't types the bundle declares.
 *
 * @example
 * const b = readAgentBundle({ ontology_toml: '[types.Person]\ndescription = "A person."\n[types.Company]\ndescription = "A business."',
 *   mappings_toml: '[extractions.notes]\ndocuments = ["set_1"]\nlinks = []\n[extractions.notes.types.Person]\n[extractions.notes.types.Company]' });
 * addLinkEdits(b, { link: "ceo_of", setId: "set_1", from: "Person", to: "Company", description: "Runs it." }).kind; // "edits"
 */
export function addLinkEdits(bundle: AgentBundle, a: AddLink): AddLinkEdits {
  if (!bundle.ontology.ok || !bundle.routes.ok)
    return { kind: "refused", why: "This version can't be read." };
  const types = bundle.ontology.value.types.map((t) => t.name);
  if (!types.includes(a.from) || !types.includes(a.to))
    return { kind: "refused", why: "Choose the types the link joins." };
  if (!a.description.trim())
    return { kind: "refused", why: "Describe the link." };
  const routes = routesReading(bundle.routes.value, a.setId).sort(
    (x, y) =>
      Number(x.documents.kind === "all") - Number(y.documents.kind === "all") ||
      x.name.localeCompare(y.name),
  );
  if (routes.some((r) => r.links.includes(a.link)))
    return {
      kind: "refused",
      why: `Documents of this set already state “${a.link}”.`,
    };
  const route = routes[0];
  if (!route)
    return {
      kind: "refused",
      why: "No route reads this set's documents: add one before the link.",
    };
  const items = editItems(bundle);
  const edits: wfe.OntologyEdit[] = [];
  if (!items[`link.${a.link}`])
    edits.push({
      put: {
        itemId: `link.${a.link}`,
        toml: toml.stringify({
          links: {
            [a.link]: {
              from: a.from,
              to: a.to,
              description: a.description.trim(),
            },
          },
        }),
      },
    });
  const routeItem = `extraction.${route.name}`;
  const parsed = toml.parse(items[routeItem] || "") as {
    extractions?: Record<string, Record<string, unknown>>;
  };
  const table = parsed.extractions?.[route.name];
  if (!table)
    return { kind: "refused", why: "This version's route can't be read." };
  const links = Array.isArray(table.links) ? (table.links as string[]) : [];
  edits.push({
    put: {
      itemId: routeItem,
      toml: toml.stringify({
        extractions: { [route.name]: { ...table, links: [...links, a.link] } },
      }),
    },
  });
  return { kind: "edits", edits };
}

/** Where a finding's Add this link stands, as its card shows it. */
export type CardState =
  | { kind: "open" }
  | { kind: "covered"; version: number; versionId: string }
  | { kind: "moving"; version: number }
  | { kind: "moved"; version: number; links: string[] }
  | { kind: "needs_load"; version: number; versionId: string; why: string[] };

/**
 * The card's state from the finding's `state`, read without trust: none for
 * a finding that names no relationship to add (or a backend before 4.32.0),
 * and open for one the backend says nothing of past that.
 *
 * @example
 * cardState({ moved: { versionId: "v8", version: "8", linksAdded: ["ceo_of"] } });
 * // { kind: "moved", version: 8, links: ["ceo_of"] }
 */
export function cardState(
  state: wfe.FoundryHealFindingState | undefined,
): CardState | undefined {
  if (!state) return undefined;
  const n = (v?: wfe.FoundryHealVersion) => {
    const x = Number(v?.version);
    return Number.isSafeInteger(x) && x > 0 ? x : 0;
  };
  if (state.covered)
    return {
      kind: "covered",
      version: n(state.covered),
      versionId: state.covered.versionId || "",
    };
  if (state.moving) return { kind: "moving", version: n(state.moving) };
  if (state.moved)
    return {
      kind: "moved",
      version: n(state.moved),
      links: state.moved.linksAdded || [],
    };
  if (state.needsLoad)
    return {
      kind: "needs_load",
      version: n(state.needsLoad),
      versionId: state.needsLoad.versionId || "",
      why: (state.needsLoad.why || []).map(whyText),
    };
  return { kind: "open" };
}

/** Each kind of reason a version needs a load, in words. */
const WHY: Record<string, (item: string) => string> = {
  link_removed: (i) => `it removes ${i}`,
  link_changed: (i) => `it changes ${i}`,
  type_added: (i) => `it adds ${i}`,
  type_changed: (i) => `it changes ${i}`,
  prop_added: (i) => `it adds ${i}`,
  routed_description: (i) => `it rewords ${i}, which documents are asked`,
  route_changed: (i) => `it changes ${i}`,
  resolve_type_added: (i) => `${i} finds a type it didn't`,
  relation: (i) => `it changes ${i}`,
  enrichment: (i) => `it changes ${i}`,
  rule: (i) => `it changes ${i}`,
  constraint: (i) => `it changes ${i}`,
  mapping: (i) => `it changes ${i}`,
  mapping_header: (i) => `its mappings name another ontology (${i})`,
  link_required: (i) => `${i} is required`,
  link_derived: (i) => `${i} derives other links`,
  link_read_by_mapping: (i) => `${i} is stated by a table`,
  link_referenced: (i) => `rules or other links read ${i}`,
  load_moved: (i) => `the load moved to version ${i} for another suggestion`,
  move_failed: () => "moving the load failed",
  document_sets: () => "it reads other document sets",
  sources: (i) =>
    `it reads other ${i === "table_files" ? "table files" : "tables"}`,
  not_a_draft: () => "it isn't an agent draft",
  marker_elsewhere: (i) => `the load holds ${i}`,
};

/**
 * One reason a version needs a load, in words: the engine's or Foundry's
 * kind and its item, or both as said for a kind a later contract adds.
 *
 * @example
 * whyText({ kind: "type_added", item: "types.Deal" }); // "it adds types.Deal"
 */
export function whyText(w: wfe.FoundryFoldWhy): string {
  const kind = w.kind || "",
    item = w.item || "";
  const said = WHY[kind];
  return said ? said(item) : [kind.replaceAll("_", " "), item].join(" ").trim();
}

/**
 * The line a card shows for its state; none for open, whose buttons say it.
 *
 * @example
 * stateText({ kind: "moving", version: 8 }); // "Updating the current load to version 8…"
 */
export function stateText(s: CardState): string | undefined {
  const v = s.kind === "open" ? 0 : s.version;
  const version = v ? `version ${v}` : "the new version";
  switch (s.kind) {
    case "open":
      return undefined;
    case "covered":
      return `${version[0].toUpperCase()}${version.slice(1)} adds this link: make it current to update the current load.`;
    case "moving":
      return `Updating the current load to ${version}…`;
    case "moved":
      return `The current load is on ${version} now: documents read from now on state this link. The reviews here stay until their documents are read again.`;
    case "needs_load":
      return `Needs a new load: ${version} changes more than a link${s.why.length ? `: ${s.why.join("; ")}` : ""}.`;
  }
}
