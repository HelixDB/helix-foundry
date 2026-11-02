import { parse, stringify } from "smol-toml";
import type { OntologyQuery } from "../../src/shared/wfe";

// What one reader may see of a load, as the ontology agent's `mapping::view`
// works it out: a reader's Visibility (the sources it holds) and a version's
// two files say who could have written each type, property and link. All its
// writers held, it is `full`, and read as anyone reads it; some, `partial`,
// and each value shows only through a row or quote the load keeps that the
// reader holds (./rules.ts); none, and it is left out of the projected
// ontology, so nothing can name it. A rule or constraint is kept only when
// everything its body reads is `full`. A query can't test, order, group or
// count what is partial (`admit`), must return every position that needs
// checking, and can't be paged: where a page ends would say how much it can't
// see. Such a query (`narrows`) reads further than it asked (`widen`), so its
// limit says nothing of the rows left out.

export type Reach = "full" | "partial";

/** What a reader holds of a load (read_request.json's Visibility). */
export type Visibility = {
  sources: string[];
  documents: "all" | { only: string[] };
  sets: string[];
  enrichment: boolean;
  withheld: { type: string; prop: string }[];
};

export const ENRICHMENT_SOURCE = "foundry_enrichment";
/** The most rows of one narrowed read that are checked. */
export const MAX_CHECKED_ROWS = 1000;
/** The same, when a step's link is partial: each fact takes a read of its own. */
export const MAX_CHECKED_FACTS = 100;
/** The most rows a narrowed read returns when a column's values take reads. */
export const MAX_SETTLED_ROWS = 100;
/** The most ids a narrowed read starts from. */
export const MAX_NARROWED_IDS = 100;

export type View = {
  // Every writer held: every read is as anyone's, and the files are the version's.
  whole: boolean;
  ontologyToml: string;
  mappingsToml: string;
  types: ReadonlyMap<string, Reach>;
  // By `${type}\u001f${prop}`.
  props: ReadonlyMap<string, Reach>;
  links: ReadonlyMap<string, Reach>;
  // Types with a key: an entity's id is its key, which only a row says.
  keyed: ReadonlySet<string>;
  // Types, `Type.prop`s and links it sees only some of, in that order.
  partial: string[];
  holdsSource: (source: string) => boolean;
  holdsDocument: (documentId: string) => boolean;
};

type Table = Record<string, unknown>;
type Writers = { held: boolean; notHeld: boolean };

const isTable = (v: unknown): v is Table =>
  typeof v === "object" && v !== null && !Array.isArray(v);
const entries = (v: unknown): [string, Table][] =>
  isTable(v)
    ? Object.entries(v).filter((e): e is [string, Table] => isTable(e[1]))
    : [];
const texts = (v: unknown) =>
  Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
const key = (type: string, prop: string) => `${type}\u001f${prop}`;

const reachOf = (w: Writers | undefined): Reach | undefined =>
  !w?.held ? undefined : w.notHeld ? "partial" : "full";
function add(map: Map<string, Writers>, at: string, w: Writers) {
  const had = map.get(at) || { held: false, notHeld: false };
  map.set(at, { held: had.held || w.held, notHeld: had.notHeld || w.notHeld });
}

/** What rows and documents write, by who could have written it. */
function written(mappings: Table, ontology: Table, visibility: Visibility) {
  const types = new Map<string, Writers>(),
    props = new Map<string, Writers>(),
    links = new Map<string, Writers>();
  const held = (is: boolean): Writers => ({ held: is, notHeld: !is });
  const own = entries(mappings.mappings);
  const rootOf = (name: string) => {
    let at = own.find(([n]) => n === name)?.[1];
    for (let i = 0; at && i <= own.length; i++) {
      if (typeof at.source === "string") return at.source;
      const parent = at.parent;
      at = own.find(([n]) => n === parent)?.[1];
    }
    return undefined;
  };
  for (const [name, m] of own) {
    const writer = held(visibility.sources.includes(rootOf(name) || ""));
    for (const [, b] of entries(m.bind)) {
      const type = String(b.type);
      if (b.missing === "create") add(types, type, writer);
      for (const prop of Object.keys(isTable(b.props) ? b.props : {}))
        add(props, key(type, prop), writer);
    }
    for (const l of Array.isArray(m.links) ? m.links : [])
      if (isTable(l)) add(links, String(l.link), writer);
  }
  // People enrichment's item: its profiles fill props and state its link.
  for (const [, e] of entries(mappings.enrichments)) {
    const writer = held(visibility.enrichment),
      type = String(e.type);
    for (const prop of Object.keys(isTable(e.props) ? e.props : {}))
      add(props, key(type, prop), writer);
    const history = e.work_history;
    if (isTable(history)) add(links, String(history.link), writer);
  }
  for (const [, r] of entries(mappings.extractions)) {
    const writer = routeWriters(r, visibility);
    for (const [type, t] of entries(r.types)) {
      if (t.create === true) add(types, type, writer);
      for (const prop of texts(t.props)) add(props, key(type, prop), writer);
    }
    for (const link of texts(r.links)) add(links, link, writer);
  }
  // Whoever creates an entity writes its key.
  for (const [type, t] of entries(ontology.types)) {
    const creators = types.get(type);
    if (creators)
      for (const prop of texts(t.key)) add(props, key(type, prop), creators);
  }
  return { types, props, links };
}

/** Who a route's values come from, for the reader. */
function routeWriters(route: Table, v: Visibility): Writers {
  const all = v.documents === "all";
  const readsHeld =
    route.documents === "all" ||
    all ||
    texts(route.documents).some((s) => v.sets.includes(s));
  if (!readsHeld) return { held: false, notHeld: true };
  if (all) return { held: true, notHeld: false };
  return (v.documents as { only: string[] }).only.length
    ? { held: true, notHeld: true }
    : { held: false, notHeld: true };
}

/** A rule's clauses: a `.` ends one before a space or the end, never `x.prop`. */
const clausesOf = (datalog: string) => datalog.split(/\.(?:\s+|$)/);
/** The names a clause's body reads: identifiers, whatever they turn out to be. */
const bodyNames = (datalog: string) =>
  clausesOf(datalog).flatMap(
    (c) => c.split(":-")[1]?.match(/[A-Za-z_][A-Za-z0-9_]*/g) || [],
  );
/** The predicates a rule's clauses derive. */
const heads = (datalog: string) =>
  clausesOf(datalog).flatMap(
    (c) => /^\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(/.exec(c)?.[1] || [],
  );

/**
 * The reader's view of the version these two files are; throws what can't be
 * read of them, and the reader gets nothing.
 */
export function viewOf(
  visibility: Visibility,
  ontologyToml: string,
  mappingsToml: string,
): View {
  const ontology = parse(ontologyToml) as Table,
    mappings = parse(mappingsToml || "") as Table;
  const writes = written(mappings, ontology, visibility);
  const typeTables = entries(ontology.types),
    linkTables = entries(ontology.links),
    relationTables = entries(ontology.relations),
    ruleTables = entries(ontology.rules),
    constraintTables = entries(ontology.constraints);
  const withheld = (type: string, prop: string) =>
    visibility.withheld.some((w) => w.type === type && w.prop === prop);

  const types = new Map<string, Reach>();
  for (const [type] of typeTables) {
    const r = reachOf(writes.types.get(type));
    if (r) types.set(type, r);
  }
  const props = new Map<string, Reach>();
  for (const [type, t] of typeTables)
    if (types.has(type))
      for (const prop of Object.keys(isTable(t.props) ? t.props : {})) {
        const r = reachOf(writes.props.get(key(type, prop)));
        if (r && !withheld(type, prop)) props.set(key(type, prop), r);
      }

  // A link some declaration or rule derives: its facts only rules give have
  // no evidence to tell them by.
  const derived = new Set<string>();
  for (const [name, t] of [...linkTables, ...relationTables]) {
    for (const target of texts(t.implies)) derived.add(target);
    if (typeof t.inverse_of === "string") {
      derived.add(t.inverse_of);
      derived.add(name);
    }
    if (t.symmetric === true || t.transitive === true) derived.add(name);
  }
  for (const [, r] of ruleTables)
    for (const head of heads(String(r.datalog || ""))) derived.add(head);
  const links = new Map<string, Reach>();
  for (const [link, l] of linkTables) {
    if (!types.has(String(l.from)) || !types.has(String(l.to))) continue;
    const w = writes.links.get(link);
    const r = !w
      ? derived.has(link)
        ? "full"
        : undefined
      : reachOf(w) === "partial" && derived.has(link)
        ? undefined
        : reachOf(w);
    if (r) links.set(link, r);
  }

  // Which rules and constraints stay: those whose bodies read only what is
  // full, given which stay, until nothing changes.
  const propFull = (prop: string) =>
    typeTables
      .filter(([, t]) => isTable(t.props) && prop in t.props)
      .every(([type]) => props.get(key(type, prop)) === "full");
  const declared = new Set(
    typeTables.flatMap(([, t]) => Object.keys(isTable(t.props) ? t.props : {})),
  );
  const relationsKept = new Set(
    relationTables
      .filter(([, t]) =>
        [t.from, t.to].every(
          (end) => typeof end !== "string" || types.has(end),
        ),
      )
      .map(([n]) => n),
  );
  for (const [name, t] of linkTables)
    for (const target of texts(t.implies))
      if (links.get(name) !== "full") relationsKept.delete(target);
  const rulesKept = new Set(ruleTables.map(([n]) => n)),
    constraintsKept = new Set(constraintTables.map(([n]) => n));
  for (let changed = true; changed; ) {
    changed = false;
    const derives = (pred: string) =>
      ruleTables.filter(([, r]) =>
        heads(String(r.datalog || "")).includes(pred),
      );
    const readable = (name: string): boolean => {
      if (typeTables.some(([t]) => t === name))
        return types.get(name) === "full";
      if (linkTables.some(([l]) => l === name))
        return (
          links.get(name) === "full" &&
          derives(name).every(([r]) => rulesKept.has(r))
        );
      if (relationTables.some(([r]) => r === name))
        return (
          relationsKept.has(name) &&
          derives(name).every(([r]) => rulesKept.has(r))
        );
      if (declared.has(name)) return propFull(name);
      const rules = derives(name);
      return rules.every(([r]) => rulesKept.has(r));
    };
    for (const [name, r] of ruleTables)
      if (
        rulesKept.has(name) &&
        !bodyNames(String(r.datalog || "")).every(readable)
      ) {
        rulesKept.delete(name);
        changed = true;
      }
    for (const [name, c] of constraintTables)
      if (
        constraintsKept.has(name) &&
        !bodyNames(String(c.datalog || "")).every(readable)
      ) {
        constraintsKept.delete(name);
        changed = true;
      }
    for (const [name] of relationTables)
      if (
        relationsKept.has(name) &&
        !derives(name).every(([r]) => rulesKept.has(r))
      ) {
        relationsKept.delete(name);
        changed = true;
      }
  }

  const keyed = new Set(
    typeTables.filter(([, t]) => texts(t.key).length > 0).map(([type]) => type),
  );
  const whole =
    !visibility.withheld.length &&
    [writes.types, writes.props, writes.links].every((m) =>
      [...m.values()].every((w) => !w.notHeld),
    );
  const documents = visibility.documents;
  const holdsDocument = (d: string) =>
    documents === "all" || documents.only.includes(d);
  const holdsSource = (s: string) =>
    s === ENRICHMENT_SOURCE
      ? visibility.enrichment
      : visibility.sources.includes(s);
  const partial = [
    ...[...types].filter(([, r]) => r === "partial").map(([t]) => t),
    ...[...props]
      .filter(([, r]) => r === "partial")
      .map(([k]) => k.replace("\u001f", ".")),
    ...[...links].filter(([, r]) => r === "partial").map(([l]) => l),
  ];
  if (whole)
    return {
      whole,
      ontologyToml,
      mappingsToml,
      types,
      props,
      links,
      keyed,
      partial,
      holdsDocument,
      holdsSource,
    };

  // The ontology without what the reader doesn't see.
  const out = structuredClone(ontology);
  const items = (section: string) =>
    (isTable(out[section]) ? out[section] : {}) as Record<string, Table>;
  const outTypes = items("types");
  for (const type of Object.keys(outTypes)) {
    const t = outTypes[type]!;
    if (!types.has(type)) {
      delete outTypes[type];
      continue;
    }
    if (isTable(t.props))
      for (const prop of Object.keys(t.props))
        if (!props.has(key(type, prop))) delete t.props[prop];
    // A lookup by name or identifier tests the value: only what is whole.
    const whole_ = (p: string) => props.get(key(type, p)) === "full";
    if (Array.isArray(t.names)) t.names = texts(t.names).filter(whole_);
    if (isTable(t.identifiers))
      for (const p of Object.keys(t.identifiers))
        if (!whole_(p)) delete t.identifiers[p];
  }
  const outLinks = items("links"),
    outRelations = items("relations");
  for (const link of Object.keys(outLinks))
    if (!links.has(link)) delete outLinks[link];
  for (const name of Object.keys(outRelations))
    if (!relationsKept.has(name)) delete outRelations[name];
  const exists = (name: string) => links.has(name) || relationsKept.has(name);
  for (const [name, l] of Object.entries(outLinks)) {
    if (Array.isArray(l.implies)) {
      l.implies = texts(l.implies).filter(exists);
      if (!(l.implies as string[]).length) delete l.implies;
    }
    if (typeof l.inverse_of === "string" && !exists(l.inverse_of))
      delete l.inverse_of;
    // Its checks read its ends: only what is whole keeps them.
    const checked =
      links.get(name) === "full" &&
      types.get(String(l.from)) === "full" &&
      types.get(String(l.to)) === "full";
    if (!checked) {
      delete l.required;
      delete l.cardinality;
    }
  }
  const outRules = items("rules"),
    outConstraints = items("constraints");
  for (const name of Object.keys(outRules))
    if (!rulesKept.has(name)) delete outRules[name];
  for (const name of Object.keys(outConstraints))
    if (!constraintsKept.has(name)) delete outConstraints[name];

  // Which mappings and routes it holds would say which sources do: only what
  // names the files is left.
  const outMappings = Object.fromEntries(
    Object.entries(mappings).filter(
      ([k]) => k === "version" || k === "ontology",
    ),
  );

  return {
    whole,
    ontologyToml: stringify(out),
    mappingsToml: stringify(outMappings),
    types,
    props,
    links,
    keyed,
    partial,
    holdsDocument,
    holdsSource,
  };
}

const refused = (pointer: string, what: string) => ({
  code: "not_visible" as const,
  message: `${pointer}: ${what} is only partly in the sources you can read`,
  pointer,
});
const mustReturn = (what: string, positions: string) => ({
  code: "not_visible" as const,
  message: `/return/rows/columns: ${what} is only partly in the sources you can read: return ${positions}`,
  pointer: "/return/rows/columns",
});

const positionsOf = (q: OntologyQuery) => [
  q.start.type,
  ...(q.steps || []).map((s) => s.to),
];

/**
 * Whether `q` reads something partly held: its answer is narrowed to what the
 * reader holds, and says nothing of what it left out (`View::narrows`).
 */
export function narrows(view: View, q: OntologyQuery) {
  if (view.whole) return false;
  const steps = q.steps || [],
    positions = positionsOf(q);
  const columnPartial =
    "rows" in q.return &&
    q.return.rows.columns.some(
      (c) =>
        !!c.prop &&
        c.prop !== "name" &&
        view.props.get(key(positions[c.at]!, c.prop)) === "partial",
    );
  return (
    columnPartial ||
    positions.some((t) => view.types.get(t) === "partial") ||
    steps.some((s) => view.links.get(s.via) === "partial")
  );
}

/** How many rows a narrowed read reads before it filters them (`View::widen`). */
export const widened = (view: View, q: OntologyQuery) =>
  (q.steps || []).some((s) => view.links.get(s.via) === "partial")
    ? MAX_CHECKED_FACTS
    : MAX_CHECKED_ROWS;

/** Whether a column of a narrowed read takes its values from evidence. */
export const settles = (view: View, type: string, prop?: string | null) =>
  !!prop && prop !== "name" && view.props.get(key(type, prop)) === "partial";

/** What an entity's stored name can't be shown as: its key is a row's to say. */
export const nameUnsafe = (view: View, type: string) =>
  view.keyed.has(type) && view.types.get(type) !== "full";

/**
 * Why `q`, which the projected ontology admits, asks too much of what the
 * reader sees only some of; undefined when it doesn't (`View::admit`).
 */
export function admit(view: View, q: OntologyQuery) {
  if (view.whole) return undefined;
  const steps = q.steps || [],
    positions = positionsOf(q),
    narrowed = narrows(view, q);
  const fullProp = (type: string | undefined, prop: string) =>
    !!type &&
    (prop === "name"
      ? view.types.get(type) === "full"
      : view.props.get(key(type, prop)) === "full");
  const conditions: [string, string, { prop: string }[]][] = [
    ["/start", q.start.type, q.start.where || []],
    ...steps.map((s, i): [string, string, { prop: string }[]] => [
      `/steps/${i}`,
      s.to,
      s.where || [],
    ]),
  ];
  for (const [at, type, where] of conditions)
    for (const [i, c] of where.entries())
      if (!fullProp(type, c.prop))
        return refused(`${at}/where/${i}/prop`, `\`${type}.${c.prop}\``);
  if (narrowed && (q.start.ids?.length || 0) > MAX_NARROWED_IDS)
    return {
      code: "not_visible" as const,
      message: `/start/ids: at most ${MAX_NARROWED_IDS} ids, of what is only partly in the sources you can read`,
      pointer: "/start/ids",
    };
  const wholePath = () => {
    for (const [i, type] of positions.entries())
      if (view.types.get(type) !== "full")
        return refused(i ? `/steps/${i - 1}/to` : "/start/type", `\`${type}\``);
    for (const [i, s] of steps.entries())
      if (view.links.get(s.via) === "partial")
        return refused(`/steps/${i}/via`, `\`${s.via}\``);
    return undefined;
  };
  const ret = q.return;
  if ("rows" in ret) {
    const { columns, order_by, offset = 0 } = ret.rows;
    if (narrowed && offset > 0)
      return refused(
        "/return/rows/offset",
        `\`${positions.find((t) => view.types.get(t) === "partial") || positions[0]}\`, which can't be paged,`,
      );
    if (order_by && !fullProp(positions[order_by.at], order_by.prop))
      return refused("/return/rows/order_by/prop", `\`${order_by.prop}\``);
    const named = (at: number) => columns.some((c) => c.at === at);
    for (const [at, type] of positions.entries())
      if (view.types.get(type) === "partial" && !named(at))
        return mustReturn(`\`${type}\``, `position ${at}`);
    for (const [i, s] of steps.entries())
      if (view.links.get(s.via) === "partial" && !(named(i) && named(i + 1)))
        return mustReturn(`\`${s.via}\``, `positions ${i} and ${i + 1}`);
    return undefined;
  }
  if ("count" in ret) return wholePath();
  if ("group_count" in ret) {
    const path = wholePath();
    if (path) return path;
    const g = ret.group_count;
    return fullProp(positions[g.at], g.prop)
      ? undefined
      : refused("/return/group_count/prop", `\`${g.prop}\``);
  }
  const path = wholePath();
  if (path) return path;
  const a = ret.aggregate;
  if (!fullProp(positions[a.at], a.prop))
    return refused("/return/aggregate/prop", `\`${a.prop}\``);
  return a.group_by && !fullProp(positions[a.group_by.at], a.group_by.prop)
    ? refused("/return/aggregate/group_by/prop", `\`${a.group_by.prop}\``)
    : undefined;
}

/**
 * What a property of an entity is for the reader: the stored value when a row
 * it holds sets it, whatever other rows do, else what the quotes it holds agree
 * on, else nothing (`View::settled`).
 */
export function settled(
  view: View,
  rows: readonly string[],
  quotes: readonly { documentId: string; value: unknown }[],
): { from: "stored" } | { from: "quoted"; value: unknown } | { from: "none" } {
  if (rows.some(view.holdsSource)) return { from: "stored" };
  const said = quotes
    .filter((q) => view.holdsDocument(q.documentId))
    .map((q) => q.value);
  return said.length &&
    said.every((v) => JSON.stringify(v) === JSON.stringify(said[0]))
    ? { from: "quoted", value: said[0] }
    : { from: "none" };
}
