import { parse } from "smol-toml";

// What a version lets documents of one set fill, as the ontology agent's
// `Mappings::scope_for` builds it from mappings.toml's document routes:
//
//   [extractions.contracts]
//   documents = ["set_…"]               # set ids, or "all"
//   links = ["signed_with"]
//   [extractions.contracts.types.Contract]
//   create = true
//   props = ["signed_on"]
//
// A document's scope is the union of every route whose `documents` is "all"
// or names its set: `create` OR-ed and `props` unioned per type, links
// unioned. A link whose ends aren't both in the scope's types is dropped.
// An empty scope reads nothing: the document is stored, out of scope.

export type TypeScope = { create: boolean; props: Set<string> };
export type Scope = { types: Map<string, TypeScope>; links: Set<string> };

type Table = Record<string, unknown>;
const isTable = (v: unknown): v is Table =>
  typeof v === "object" && v !== null && !Array.isArray(v);
const strings = (v: unknown) =>
  Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];

/** The routes of a mappings.toml, by name; none when it doesn't parse. */
function routes(mappingsToml: string): [string, Table][] {
  try {
    const doc: Table = parse(mappingsToml);
    return isTable(doc.extractions)
      ? Object.entries(doc.extractions as Table).filter(
          (e): e is [string, Table] => isTable(e[1]),
        )
      : [];
  } catch {
    return [];
  }
}

const reads = (route: Table, setId: string) =>
  route.documents === "all" || strings(route.documents).includes(setId);

/** Whether any route of the version reads documents of the set. */
export const isRouted = (mappingsToml: string, setId: string) =>
  routes(mappingsToml).some(([, r]) => reads(r, setId));

/** The set ids the version's routes name ("all" names none). */
export const routedSets = (mappingsToml: string) => [
  ...new Set(routes(mappingsToml).flatMap(([, r]) => strings(r.documents))),
];

/**
 * The scope of a document in `setId`, given the ends of each link (from
 * ontology.toml) so links whose ends fall outside are dropped.
 */
export function scopeFor(
  mappingsToml: string,
  setId: string,
  linkEnds: Map<string, { from: string; to: string }>,
): Scope {
  const types = new Map<string, TypeScope>(),
    links = new Set<string>();
  for (const [, r] of routes(mappingsToml)) {
    if (!reads(r, setId)) continue;
    for (const [type, t] of Object.entries(isTable(r.types) ? r.types : {})) {
      const scope = types.get(type) || { create: false, props: new Set() };
      if (isTable(t)) {
        scope.create ||= t.create === true;
        for (const p of strings(t.props)) scope.props.add(p);
      }
      types.set(type, scope);
    }
    for (const l of strings(r.links)) links.add(l);
  }
  for (const l of links) {
    const ends = linkEnds.get(l);
    if (!ends || !types.has(ends.from) || !types.has(ends.to)) links.delete(l);
  }
  return { types, links };
}

/** Whether a scope lets documents fill nothing. */
export const isEmpty = (s: Scope) => !s.types.size && !s.links.size;

/**
 * A scope written out, for tests and fixtures: each type with whether
 * documents create it and the props they fill, and the links they state.
 *
 * @example
 * scopeOf({ Contract: { create: true, props: ["signed_on"] }, Customer: {} }, ["signed_with"]);
 */
export const scopeOf = (
  types: Record<string, { create?: boolean; props?: string[] }>,
  links: string[] = [],
): Scope => ({
  types: new Map(
    Object.entries(types).map(([t, s]) => [
      t,
      { create: s.create === true, props: new Set(s.props || []) },
    ]),
  ),
  links: new Set(links),
});
