import { createHash } from "node:crypto";
import { stringify } from "smol-toml";
import type {
  AgentBundleDefinition,
  AgentDraftItem,
  AgentQuestion,
  Connector,
  SyncedTable,
} from "../../src/shared/wfe";
import { PROFILE_DEFAULTS } from "../../src/web/areas/ontology/enrichment-fields";
import { PEOPLE } from "../fixtures/sales-graph";
import { measureClues } from "./enrichment";

// The mock's stand-in for the ontology agent: a first draft from the chosen
// tables, in the agent_bundle shape the foundry-ontology-worker stores. One
// type per table keyed by its primary key, one link per declared foreign key
// between chosen tables, one mapping per table. A table is a connector's
// synced table, or a table file of a document set (fileTable), whose
// mappings name it by its document id. With no table, the sets' text is
// drafted as Contracts that their documents create, through one route.
// Deterministic, so tests can rely on what it drafts.
//
// With People enrichment allowed (`enrich`: the backend adds the
// foundry_enrichment source only then), a database's people table, whose
// rows the mock knows (fixtures/sales-graph.ts), is looked up when its
// people have a full name, a work email or LinkedIn URL, at least half of
// them usable and none shared: the type is named by its name column, its
// clues become identifiers, and an [enrichments] item fills the default
// profile fields, with the agent's measure of its clues. Otherwise a
// left_out entry says why.

/** The agent's id for a synced table, as the backend's ontologyagent names it. */
export const sourceId = (connectorId: string, table: string) =>
  "src_" +
  createHash("sha256")
    .update(`${connectorId}\0${table}`)
    .digest("hex")
    .slice(0, 16);

/**
 * A table to draft from: what holds it (a connector, or a file's set, which
 * has no kind), the table, and the source id its mapping names (a synced
 * table's, by default; a table file's document id).
 */
export type DraftTable = {
  connector: Pick<Connector, "id" | "name"> & Partial<Pick<Connector, "kind">>;
  table: SyncedTable;
  source?: string;
};

const column = (values: unknown[]) =>
  values.every((v) => typeof v === "number" && Number.isInteger(v))
    ? "bigint"
    : values.every((v) => typeof v === "number")
      ? "double"
      : values.every((v) => typeof v === "boolean")
        ? "boolean"
        : "text";

/**
 * A table file read as a table to draft from: its columns in the order they
 * first appear, typed by their values, keyed by the first whose values are
 * all there and all different. Its name is the file's, without the
 * extension.
 */
export function fileTable(
  set: { id: string; name: string },
  file: { id: string; name: string; rows: Record<string, unknown>[] },
): DraftTable {
  const names = [...new Set(file.rows.flatMap((r) => Object.keys(r)))];
  const key = names.find((n) => {
    const values = file.rows.map((r) => r[n]);
    return (
      values.every((v) => v !== undefined && v !== null && v !== "") &&
      new Set(values.map(String)).size === values.length
    );
  });
  return {
    connector: set,
    source: file.id,
    table: {
      name: file.name.replace(/\.[^.]*$/, ""),
      columns: names.map((name) => ({
        name,
        type: column(file.rows.map((r) => r[name]).filter((v) => v != null)),
        nullable: file.rows.some((r) => r[name] == null || r[name] === ""),
      })),
      primaryKey: key ? [key] : [],
      rowCount: String(file.rows.length),
    },
  };
}

const words = (table: string) =>
  table
    .split(".")
    .at(-1)!
    .split(/[^A-Za-z0-9]+/)
    .filter(Boolean)
    .map((w) => w.toLowerCase());
const singular = (w: string) =>
  /ies$/.test(w)
    ? w.slice(0, -3) + "y"
    : /(ss|us)$/.test(w)
      ? w
      : /(ches|shes|xes|sses)$/.test(w)
        ? w.slice(0, -2)
        : w.endsWith("s")
          ? w.slice(0, -1)
          : w;
const camel = (ws: string[]) =>
  ws.map((w) => w.charAt(0).toUpperCase() + w.slice(1)).join("");
const singularWords = (table: string) => {
  const ws = words(table);
  return [...ws.slice(0, -1), singular(ws.at(-1) || "record")];
};
const propType = (type = "") =>
  /int|serial/i.test(type)
    ? "integer"
    : /decimal|numeric|float|double|real/i.test(type)
      ? "float"
      : /bool/i.test(type)
        ? "boolean"
        : /timestamp|datetime/i.test(type)
          ? "datetime"
          : /^date$/i.test(type)
            ? "date"
            : "string";
const unique = (taken: Set<string>, name: string) => {
  let out = name;
  for (let i = 2; taken.has(out); i++) out = `${name}_${i}`;
  taken.add(out);
  return out;
};
const toml = (value: Record<string, unknown>) => stringify(value) + "\n";

export function draftFromTables(
  picked: DraftTable[],
  goal: string,
  // The chosen sets holding text, routed to Contracts when there's no table.
  texts: { id: string; name: string }[] = [],
  enrich = false,
): AgentBundleDefinition {
  // Types first, so links and lookups can name them.
  const typeNames = new Set<string>(),
    mappingNames = new Set<string>();
  const plan = picked.map(({ connector, table, source }) => {
    const name = table.name || "table";
    // A clash takes the connector's name, then a number.
    const own = camel(singularWords(name)),
      prefixed = camel(words(connector.name || "")) + own;
    let type = typeNames.has(own) ? prefixed : own;
    for (let i = 2; typeNames.has(type); i++) type = `${prefixed}${i}`;
    typeNames.add(type);
    const columns = table.columns || [];
    const key = (table.primaryKey || []).filter((k) =>
      columns.some((c) => c.name === k),
    );
    return {
      connector,
      table,
      name,
      type,
      alias: singularWords(name).join("_"),
      mapping: unique(mappingNames, words(name).join("_")),
      key,
      source: source || sourceId(connector.id, name),
    };
  });
  const byTable = new Map(plan.map((p) => [`${p.connector.id}/${p.name}`, p]));

  const types: Record<string, Record<string, unknown>> = {},
    links: Record<string, Record<string, unknown>> = {},
    mappings: Record<string, Record<string, unknown>> = {},
    enrichments: Record<string, Record<string, unknown>> = {},
    items: Record<string, AgentDraftItem> = {},
    questions: AgentQuestion[] = [],
    leftOut: { what: string; reason: string }[] = [],
    linkNames = new Set<string>();

  for (const p of plan) {
    const columns = p.table.columns || [],
      foreign = new Set((p.table.foreignKeys || []).map((f) => f.column));
    const props: Record<string, string> = {};
    for (const c of columns) {
      if (!c.name || foreign.has(c.name)) continue;
      if (/json/i.test(c.type || "")) {
        leftOut.push({
          what: `${p.source} file.${c.name}`,
          reason: "Nested JSON that no question reads yet.",
        });
        continue;
      }
      if (Object.keys(props).length < 8 || p.key.includes(c.name))
        props[c.name] = propType(c.type);
    }
    const keyed = p.key.length > 0;
    // People enrichment: the people table of a database, whose rows the
    // mock knows.
    const people =
      enrich &&
      keyed &&
      ["postgres", "mysql"].includes(p.connector.kind || "") &&
      p.name === "people" &&
      !Object.keys(enrichments).length
        ? enrichmentOf(p.source, props)
        : undefined;
    if (people && "reason" in people) leftOut.push(people.reason);
    const enriched = people && "item" in people ? people : undefined;
    const type: Record<string, unknown> = {
      description: `One row of ${p.connector.name}’s ${p.name}${keyed ? `, identified by its ${p.key.join(" and ")}` : ""}.`,
      ...(keyed
        ? {
            props: { ...props, ...enriched?.props },
            key: p.key,
            ...(enriched
              ? { names: enriched.names, identifiers: enriched.identifiers }
              : {}),
          }
        : {}),
    };
    types[p.type] = type;
    if (enriched) {
      const item = { type: p.type, ...enriched.item };
      enrichments[p.mapping] = item;
      items[`enrichment.${p.mapping}`] = {
        toml: toml({ enrichments: { [p.mapping]: item } }),
        status: "proposed",
        rationale: `${p.name} holds each person’s ${Object.keys(enriched.identifiers).join(" and ")}, so People enrichment can look up their current title and location.`,
        serves: ["key"],
        clues: enriched.clues,
      };
    }
    items[`type.${p.type}`] = {
      toml: toml({ types: { [p.type]: type } }),
      status: "proposed",
      rationale: keyed
        ? `${p.name} declares ${p.key.join(" and ")} as its primary key, so each row is one ${p.type}.`
        : `${p.name} declares no primary key; until you say which field identifies a row, a ${p.type} is known by its name.`,
      serves: keyed ? ["key"] : [],
    };
    if (!keyed)
      questions.push({
        id: `Q${questions.length + 1}`,
        items: [`type.${p.type}`, `mapping.${p.mapping}`],
        text: `Which field identifies a row of ${p.name}?`,
        options: columns.slice(0, 3).map((c) => c.name || ""),
        default: null,
        blocking: false,
      });

    // The row's own entity, then one per foreign key into a chosen table.
    const own = keyed
      ? {
          type: p.type,
          key: Object.fromEntries(p.key.map((k) => [k, k])),
          props: Object.fromEntries(
            Object.keys(props)
              .filter((k) => !p.key.includes(k))
              .map((k) => [k, k]),
          ),
          missing: "create",
        }
      : {
          type: p.type,
          name: columns.find((c) => propType(c.type) === "string")?.name || "",
          missing: "create",
        };
    const bind: Record<string, Record<string, unknown>> = { [p.alias]: own },
      rowLinks: Record<string, string>[] = [];
    for (const fk of p.table.foreignKeys || []) {
      const target = byTable.get(`${p.connector.id}/${fk.referencedTable}`);
      const column = fk.column || "";
      if (!target) {
        questions.push({
          id: `Q${questions.length + 1}`,
          items: [`mapping.${p.mapping}`],
          text: `${p.name}.${column} refers to ${fk.referencedTable}, which this draft didn’t read. Should the next draft include it?`,
          options: ["Yes, include it", "No, leave it out"],
          default: "No, leave it out",
          blocking: false,
        });
        continue;
      }
      const stem = column.replace(/_id$/, "") || target.alias;
      const link = unique(linkNames, stem);
      const nullable = !!p.table.columns?.find((c) => c.name === column)
        ?.nullable;
      const def: Record<string, unknown> = {
        from: p.type,
        to: target.type,
        description: `The ${singularWords(p.name).join(" ")} refers to its ${stem.replace(/_/g, " ")}.`,
        cardinality: "one",
        ...(nullable ? {} : { required: true }),
      };
      links[link] = def;
      items[`link.${link}`] = {
        toml: toml({ links: { [link]: def } }),
        status: "proposed",
        rationale: `Declared as a foreign key: ${p.name}.${column} → ${fk.referencedTable}.${fk.referencedColumn}.`,
        serves: ["join"],
      };
      const alias = bind[target.alias]
        ? `${target.alias}_${stem}`
        : target.alias;
      bind[alias] =
        target.key.length === 1 && target.key[0] === fk.referencedColumn
          ? {
              type: target.type,
              key: { [target.key[0]]: column },
              missing: "report",
              ...(nullable ? { optional: true } : {}),
            }
          : {
              type: target.type,
              lookup: {
                mapping: target.mapping,
                on: { [fk.referencedColumn || "id"]: column },
              },
              missing: "report",
              ...(nullable ? { optional: true } : {}),
            };
      rowLinks.push({ link, from: p.alias, to: alias });
    }
    const mapping = {
      source: p.source,
      ...(keyed ? {} : { row_key: [columns[0]?.name || "id"] }),
      ...(rowLinks.length ? { links: rowLinks } : {}),
      bind,
    };
    mappings[p.mapping] = mapping;
    items[`mapping.${p.mapping}`] = {
      toml: toml({ mappings: { [p.mapping]: mapping } }),
      status: "proposed",
      rationale: `Loads every row of ${p.connector.name} ${p.name} as a ${p.type}${rowLinks.length ? `, linked through ${rowLinks.map((l) => l.link).join(", ")}` : ""}.`,
      serves: ["key", ...(rowLinks.length ? ["join"] : [])],
    };
  }

  // From text alone: Contracts its documents create, with when each was
  // signed and how long it runs.
  const extractions: Record<string, Record<string, unknown>> = {};
  if (!picked.length && texts.length) {
    const contract = {
      description: "An agreement its documents describe, known by its title.",
      props: { signed_on: "date", term_months: "integer" },
    };
    types.Contract = contract;
    items["type.Contract"] = {
      toml: toml({ types: { Contract: contract } }),
      status: "proposed",
      rationale: "The documents read as agreements, each one Contract.",
      serves: [],
    };
    const route = {
      documents: texts.map((t) => t.id),
      types: {
        Contract: { create: true, props: ["signed_on", "term_months"] },
      },
    };
    extractions.contracts = route;
    items["extraction.contracts"] = {
      toml: toml({ extractions: { contracts: route } }),
      status: "proposed",
      rationale: `Reads a Contract from each document of ${texts.map((t) => t.name).join(" and ")}.`,
      serves: [],
    };
  }
  const ontologyToml = toml({ version: "draft", types, links }),
    mappingsToml = toml({
      version: "draft",
      ontology: "draft",
      mappings,
      ...(Object.keys(extractions).length ? { extractions } : {}),
      ...(Object.keys(enrichments).length ? { enrichments } : {}),
    });
  const connectors = [...new Set(picked.map((p) => p.connector.name))];
  const summary = [
    picked.length
      ? `A first draft from ${picked.length} ${picked.length === 1 ? "table" : "tables"} in ${connectors.join(" and ")}${goal ? `, for: “${goal}”` : ""}.`
      : `A first draft from the documents of ${texts.map((t) => t.name).join(" and ")}${goal ? `, for: “${goal}”` : ""}.`,
    `**Types and keys**\n${[...plan.map((p) => `- ${p.type}${p.key.length ? `, keyed by ${p.key.join(" and ")}` : ", with no key yet"}.`), ...(types.Contract && !plan.length ? ["- Contract, created from each document."] : [])].join("\n")}`,
    `**Links**\n- ${Object.keys(links).length} ${Object.keys(links).length === 1 ? "link" : "links"}, each from a declared foreign key.`,
  ].join("\n\n");
  const tokens = 4000 + 1500 * picked.length;
  return {
    format: "agent_bundle",
    rules: { count: 0, evaluated: 0 },
    status: questions.length ? "needs_input" : "done",
    ontology_toml: ontologyToml,
    mappings_toml: mappingsToml,
    draft: {
      ontology_toml: ontologyToml,
      mappings_toml: mappingsToml,
      draft: {
        rev: 1,
        base_version: null,
        items,
        left_out: leftOut,
        questions,
      },
    },
    questions,
    summary,
    usage: {
      input: 12,
      cache_read: tokens * 6,
      cache_write: tokens,
      output: Math.round(tokens / 4),
      usd: Math.round(tokens * 0.000012 * 1e4) / 1e4,
    },
  };
}

/**
 * What the agent proposes for a people table's type (`props`, from its
 * columns): its names, its clues as identifiers, the props the profiles fill
 * and the item; or, when its people can't be looked up well, why.
 */
function enrichmentOf(
  source: string,
  props: Record<string, string>,
):
  | {
      names: string[];
      identifiers: Record<string, string>;
      props: Record<string, string>;
      item: Record<string, unknown>;
      clues: ReturnType<typeof measureClues>;
    }
  | { reason: { what: string; reason: string } } {
  // The table, as the version page names it: "Shop database · people".
  const what = source;
  const name = ["full_name", "name"].find((c) => props[c] === "string"),
    email = ["email", "work_email"].find((c) => props[c] === "string"),
    linkedin = ["linkedin_url", "linkedin"].find((c) => props[c] === "string");
  if (!name || (!email && !linkedin))
    return {
      reason: {
        what,
        reason:
          "People enrichment needs a full name, and a work email or LinkedIn URL to look people up by.",
      },
    };
  const clues = measureClues(
    PEOPLE.map((r) => ({
      key: r.contact_id,
      name: r.full_name || "",
      email: email ? r.email : undefined,
      linkedin: linkedin ? r.linkedin_url : undefined,
    })),
  );
  if (clues.usable * 2 < clues.people || clues.shared)
    return {
      reason: {
        what,
        reason: `People enrichment needs half the people to have a usable email or LinkedIn URL, none shared: ${clues.usable} of ${clues.people} have one${clues.shared ? `, ${clues.shared} shared` : ""}.`,
      },
    };
  return {
    names: [name],
    identifiers: {
      ...(email ? { [email]: "email" } : {}),
      ...(linkedin ? { [linkedin]: "exact" } : {}),
    },
    props: Object.fromEntries(PROFILE_DEFAULTS.map((d) => [d.prop, "string"])),
    item: {
      clues: {
        ...(email ? { email } : {}),
        ...(linkedin ? { linkedin_url: linkedin } : {}),
      },
      props: Object.fromEntries(PROFILE_DEFAULTS.map((d) => [d.prop, d.field])),
    },
    clues,
  };
}

// Request words a title leaves out ("Trace orders through returns" is titled
// "Orders through returns"), and the small words it doesn't end on.
const REQUEST =
  /^(?:(?:please|help|us|me|our|my|we|i|to|want|need|would|like|trace|track|show|find|see|know|understand|which|what|how|who|why|when|where|the|a|an)\s+)+/i;
const DANGLING =
  /(?:\s+(?:and|or|of|for|from|to|with|in|on|by|the|a|an|its|their|our|who|that|which))+$/i;

/**
 * The mock's stand-in for the model that titles a generation when it is
 * created: the first clause of the goal's first sentence, without its request
 * words or closing punctuation, in sentence case and whole words within 60
 * characters; "" for no goal. Deterministic, and unlike the pages' own
 * fallback for an untitled generation, so tests can tell the two apart.
 */
export function draftTitle(goal: string): string {
  const first = goal
    .trim()
    .split(/(?<=[.!?])\s|\n/)[0]!
    .trim();
  const phrase = (first.replace(REQUEST, "") || first)
    .split(/\s*[,;:\u2013\u2014]\s+|\s+-\s+/)[0]!
    .replace(/[\s.!?:;,]+$/, "");
  // Whole words while they fit; one longer word is cut.
  const ws = phrase.split(/\s+/),
    over = ws.findIndex(
      (_, i) => [...ws.slice(0, i + 1).join(" ")].length > 60,
    );
  const kept = [...(over < 0 ? ws : ws.slice(0, Math.max(over, 1))).join(" ")]
    .slice(0, 60)
    .join("")
    .replace(DANGLING, "");
  return kept.charAt(0).toUpperCase() + kept.slice(1);
}
