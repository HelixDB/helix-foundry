import { isFoundryStamp } from "../../../shared/foundry-labels";
import type { OntologyVersion } from "../../../shared/wfe";
import { versionFormat } from "../ontology/format";
import { readAgentBundle, readFoundry } from "../ontology/model";

// How the ontology's current version names what its database holds: every
// place an entity shows (the Explorer's canvas, tooltips, inspector and
// search, Records, an entity's page, a document's view) labels it with
// readable() and leaves out what isInternal() says the layout keeps for
// itself. Nothing here reads the database: the rules come from the version
// alone, and a label the version doesn't know keeps its raw label and id.
//
// A readable name is, in order: the first non-empty `names` prop of the
// type, else its key props joined, else the stored `name` (a type known by
// its name has nothing else), else a short id.

/** A type as the version declares it: its props, key props and names props. */
export type TypeNaming = {
  name: string;
  props: { name: string; type: string }[];
  // Empty: the type is known by its name.
  key: string[];
  // String props text names it by; a keyed type keeps `<prop>__nn` beside each.
  names: string[];
};

/** The rules of one version. `version` is null when there is none to go by. */
export type Naming = {
  version: string | null;
  types: ReadonlyMap<string, TypeNaming>;
  // The version's link names: fact edges between its types.
  links: ReadonlySet<string>;
};

/** No version: every label keeps its raw label and id. */
export const NO_NAMING: Naming = {
  version: null,
  types: new Map(),
  links: new Set(),
};

/**
 * The naming rules of a version, in either format: an agent draft's types
 * with their key and names, or Definition v1's entities keyed by their
 * primary key (it declares no names). No version, or one that doesn't
 * parse, is NO_NAMING.
 *
 * @example
 * namingOf(null) === NO_NAMING; // true
 */
export function namingOf(v?: OntologyVersion | null): Naming {
  if (!v) return NO_NAMING;
  if (versionFormat(v) === "agent_bundle") {
    const bundle = readAgentBundle(v.definition);
    if (!bundle.ontology.ok) return { ...NO_NAMING, version: v.id };
    const o = bundle.ontology.value;
    return {
      version: v.id,
      types: new Map(
        o.types.map((t) => [
          t.name,
          { name: t.name, props: t.props, key: t.key, names: t.names },
        ]),
      ),
      links: new Set(o.links.map((l) => l.name)),
    };
  }
  const view = readFoundry(v.definition);
  return {
    version: v.id,
    types: new Map(
      view.entities.map((e) => [
        e.name,
        {
          name: e.name,
          props: e.properties.map((p) => ({ name: p.name, type: p.type })),
          key: e.primary_key ? [e.primary_key] : [],
          names: [],
        },
      ]),
    ),
    links: new Set(view.relationships.map((r) => r.name)),
  };
}

/** How a node reads: its name and type, and what the name was made of. */
export type Readable = {
  name: string;
  // The type the version names, or the raw label it doesn't know.
  type: string;
  known: boolean;
  by: "names" | "key" | "name" | "id";
};

/** A node's short id, for a node with nothing better to show. */
export const shortId = (dbId: number) => `#${dbId}`;

// A value as name text: trimmed strings and numbers; anything else is none.
const text = (v: unknown) =>
  typeof v === "string"
    ? v.replace(/\s+/g, " ").trim()
    : typeof v === "number" && Number.isFinite(v)
      ? String(v)
      : "";

/**
 * A node's readable name by the version's rules, from its label, its stored
 * properties and its database id.
 *
 * @example
 * const naming = { ...NO_NAMING, version: "v", types: new Map([["OrderLine", { name: "OrderLine", props: [], key: ["order_number", "line_no"], names: [] }]]) };
 * readable(naming, "OrderLine", { order_number: "ORD-1001", line_no: 2 }, 7).name; // "ORD-1001 · 2"
 * readable(naming, "Account", { email: "ada@acme.example" }, 3); // { name: "#3", type: "Account", known: false, by: "id" }
 */
export function readable(
  naming: Naming,
  label: string,
  props: Readonly<Record<string, unknown>>,
  dbId: number,
): Readable {
  const t = naming.types.get(label);
  const stored = text(props.name);
  if (!t)
    return stored
      ? { name: stored, type: label, known: false, by: "name" }
      : { name: shortId(dbId), type: label, known: false, by: "id" };
  for (const n of t.names) {
    const value = text(props[n]);
    if (value) return { name: value, type: label, known: true, by: "names" };
  }
  const key = t.key.map((k) => text(props[k])).filter(Boolean);
  if (key.length)
    return { name: key.join(" · "), type: label, known: true, by: "key" };
  return stored
    ? { name: stored, type: label, known: true, by: "name" }
    : { name: shortId(dbId), type: label, known: true, by: "id" };
}

// What the layout keeps on an entity of the version's types: its identity
// and the version that wrote it, which the type's own props already show.
const ENTITY_INTERNAL = new Set([
  "entity_key",
  "norm_name",
  "ontology_version",
]);
const FACT_INTERNAL = new Set(["fact_key", "ontology_version"]);

/**
 * Whether a property is the layout's own rather than the data's: a
 * normalized name or identifier (`<prop>__nn`, `<prop>__id`) and the load
 * anything was written by (`foundry_load`) on anything, Foundry's other
 * stamps (`foundry_*`: the request that created it, the part of its load
 * that made it) on all but Foundry's own labels, an entity's identity on the
 * version's types and a fact's key on its links. Property lists leave these
 * out.
 *
 * @example
 * isInternal(NO_NAMING, "node", "Customer", "company_name__nn"); // true
 * isInternal(NO_NAMING, "node", "Customer", "email__id"); // true
 * isInternal(NO_NAMING, "edge", "billed_to", "foundry_tx"); // true
 * isInternal(NO_NAMING, "node", "FoundryTx", "foundry_tx"); // false
 */
export function isInternal(
  naming: Naming,
  target: "node" | "edge",
  label: string,
  key: string,
): boolean {
  // Foundry's stamps include the load, which the Inspector says apart.
  if (
    key.endsWith("__nn") ||
    key.endsWith("__id") ||
    isFoundryStamp(target, label, key)
  )
    return true;
  return target === "node"
    ? naming.types.has(label) && ENTITY_INTERNAL.has(key)
    : naming.links.has(label) && FACT_INTERNAL.has(key);
}

/** The most properties a read projects for names: it stays one bounded row per node. */
export const MAX_NAMING_KEYS = 100;

/**
 * Every key and names prop of the version's types, so a read of nodes of
 * any type carries what their names are made of. At most MAX_NAMING_KEYS.
 *
 * @example
 * namingKeys(NO_NAMING); // []
 */
export function namingKeys(naming: Naming): string[] {
  const keys = new Set<string>();
  for (const t of naming.types.values())
    for (const k of [...t.names, ...t.key])
      if (k !== "name" && !k.startsWith("$")) keys.add(k);
  return [...keys].sort().slice(0, MAX_NAMING_KEYS);
}

/**
 * A name folded the way the ontology agent stores `<prop>__nn` and
 * `norm_name` (oa extraction::text::norm_name): compatibility forms folded,
 * lowercase letter by letter (a final sigma is a sigma), every run of other
 * characters one space, trimmed. Symbols that tell names apart (`+ # & @ $`)
 * are kept, and so is a dot before a letter or digit that starts a word
 * (".NET") or follows two or more of them ("Node.js"); dots after initials
 * or a symbol separate. It is what searches and name filters compare.
 *
 * @example
 * normalizeName("  Helio Biosciences, Inc. "); // "helio biosciences inc"
 * normalizeName("J.K. Rowling") === normalizeName("J. K. Rowling"); // true
 */
export function normalizeName(name: string): string {
  return (
    name
      .normalize("NFKC")
      .toLowerCase()
      // "İ" lowercases to i and a combining dot; the agent folds it to i.
      .replace(/i̇/g, "i")
      // The agent lowercases letter by letter: a final sigma is a sigma.
      .replace(/ς/g, "σ")
      // A dot starts a word after a character that becomes a space, not
      // after a kept symbol ("C#.NET" is "c# net").
      .replace(
        /(?<=[\p{L}\p{N}]{2}|^|[^\p{L}\p{N}\p{M}+#&@$])\.(?=[\p{L}\p{N}])/gu,
        "\uE000",
      )
      .replace(/[^\p{L}\p{N}\p{M}+#&@$\uE000]+/gu, " ")
      .replace(/\uE000/g, ".")
      .trim()
  );
}
