import {
  MAX_READS,
  entityRef,
  outcomeOf,
  parseEvidenceResult,
  parseQueryResult,
  parseViolationsResult,
  type EntityRef,
  type EvidenceItem,
  type FactRef,
  type Outcome,
  type QueryResult,
  type Read,
} from "../../../shared/foundry-read";
import type { OntologyQuery, QueryWhere } from "../../../shared/wfe";
import {
  RECORDS_LIMITS,
  RECORD_COLUMN,
  activeFilter,
  columnsOf,
  fieldName,
  filterOf,
  type RecordRow,
  type RecordsPage,
  type RecordsView,
} from "../records/model";
import {
  identityOf,
  readFailureText,
  readGraph,
  refOf,
  type ReadOptions,
} from "./graph-read";
import {
  LIMITS,
  entityId,
  type EntityGraph,
  type EntityIdentity,
  type EntityRow,
  type Evidence,
  type Fact,
  type PropEvidence,
  type SourceRow,
  type Violation,
} from "./model";
import { readable, type Naming, type TypeNaming } from "./naming";
import { RULE_BREAK_LIMITS, refKey } from "./rule-breaks";

// Reads of the one load a user's source grants cover, through the WFE's
// typed reads (POST .../loads/{loadId}:read, src/shared/foundry-read.ts),
// never the database: what a grantee, or a member who can't query the
// ontology's database, reads. Each page's reads are planned here from the
// load's version (its types, props and links: ./naming.ts) and their
// answers parsed into the shapes the database's reads give (./model.ts,
// ../records/model.ts), so a page shows either alike. A plan's reads go
// 20 to a request (runPlan). What typed reads can't give is left out:
// Foundry's own records (an entity's mentions and reviews, a document's
// marks) and facts' own props. Records pages with a read's `offset`. Someone
// who reads only part of the load (grants that hold only some of its sources)
// gets rows narrowed to those they hold where a read touches what is partly
// held: one read and no pages (where a page ended would say how much they can't
// see), `total` only the rows shown, and a refusal (`not_visible`) of a filter
// or a sort on what is partly held. Every answer is parsed without trust.

export const TYPED_LIMITS = Object.freeze({
  // Columns of one row read, the entity's own among them (read_request.json Rows).
  columns: 20,
  // Rows one read returns, cut into Records' pages here.
  rows: 200,
  // Link sides of a type read for one entity (each up to factsPerSide facts).
  links: 30,
  // Props whose quotes are read for one entity.
  propEvidence: 30,
  // Pieces of evidence per prop and per fact.
  evidence: 20,
  // Rows that state one entity.
  about: 100,
  // Facts whose evidence is read at once.
  evidenceFacts: 20,
  // Rule breaks read naming one entity.
  violations: 50,
});

/** Reads, and how to parse their outcomes, in their order, into a page's data. */
export type ReadPlan<T> = {
  reads: Read[];
  parse: (outcomes: readonly Outcome<unknown>[]) => T;
};

/**
 * Runs a plan against a load, MAX_READS reads to a request, one request
 * after another. Throws the WFE's error when it refuses one.
 */
export async function runPlan<T>(
  ws: string,
  o: string,
  loadId: string,
  plan: ReadPlan<T>,
  options: ReadOptions = {},
): Promise<T> {
  const outcomes: Outcome<unknown>[] = [];
  for (let i = 0; i < plan.reads.length; i += MAX_READS)
    outcomes.push(
      ...(await readGraph(
        ws,
        o,
        loadId,
        plan.reads.slice(i, i + MAX_READS),
        options,
      )),
    );
  return plan.parse(outcomes);
}

const isRow = (v: unknown): v is Record<string, unknown> =>
  !!v && typeof v === "object" && !Array.isArray(v);
const chunks = <T>(xs: readonly T[], n: number): T[][] =>
  Array.from({ length: Math.ceil(xs.length / n) }, (_, i) =>
    xs.slice(i * n, (i + 1) * n),
  );
/** The rows of a query's outcome, or undefined when it has none to give. */
const rowsOf = (o: Outcome<unknown> | undefined) => {
  const r = outcomeOf(o, parseQueryResult);
  return r.ok && r.value.kind === "rows" ? r.value : undefined;
};
const startAt = (i: EntityIdentity): OntologyQuery["start"] => ({
  type: i.type,
  ids: ["key" in i ? i.key : i.name],
});

/** The props of a type that name its entities: its names, then its key. */
const namingProps = (t?: TypeNaming) =>
  [...new Set([...(t?.names || []), ...(t?.key || [])])]
    .filter((p) => p !== "name")
    .slice(0, TYPED_LIMITS.columns - 2);

/**
 * An entity of a row: its cell (`{type, id, name}`) and the props read
 * beside it, as the database's reads give an entity. Undefined for a cell
 * that isn't one, or of a type the version doesn't know.
 */
function entityOf(
  cell: unknown,
  naming: Naming,
  props: Record<string, unknown> = {},
): EntityRow | undefined {
  const ref = entityRef(cell),
    identity = ref && identityOf(ref, naming);
  if (!ref || !identity) return undefined;
  const stored = isRow(cell) && typeof cell.name === "string" ? cell.name : "";
  return {
    dbId: 0,
    id: entityId(identity),
    type: ref.type,
    name: stored || ref.id,
    identity,
    props: { ...props, ...(stored ? { name: stored } : {}) },
  };
}
/** A row's values of `columns`, from its cell `from` on; absent ones left out. */
const valuesOf = (cells: unknown[], columns: string[], from: number) =>
  Object.fromEntries(
    columns.flatMap((c, i) => {
      const v = cells[from + i];
      return v === null || v === undefined ? [] : [[c, v]];
    }),
  );

// ---------------------------------------------------------------- Facts

/**
 * A fact's key here: its link and ends, which read its evidence. Typed reads
 * know no fact_key; the page only needs one to tell its facts apart.
 *
 * @example
 * factOfKey(typedFactKey({ link: "billed_to", subject: { type: "Invoice", id: "in_1" }, object: { type: "Customer", id: "cus_1" } }))?.link; // "billed_to"
 */
export const typedFactKey = (f: FactRef) =>
  JSON.stringify([
    f.link,
    f.subject.type,
    f.subject.id,
    f.object.type,
    f.object.id,
  ]);
/** The fact typedFactKey keys, or undefined for a key it didn't make. */
export function factOfKey(key: string): FactRef | undefined {
  let v: unknown;
  try {
    v = JSON.parse(key);
  } catch {
    return undefined;
  }
  return Array.isArray(v) &&
    v.length === 5 &&
    v.every((x) => typeof x === "string")
    ? {
        link: v[0],
        subject: { type: v[1], id: v[2] },
        object: { type: v[3], id: v[4] },
      }
    : undefined;
}

const evidenceOf = (e: EvidenceItem): Evidence =>
  e.kind === "row"
    ? {
        kind: "row",
        source: e.source,
        mapping: e.mapping,
        rowKey: e.rowKey,
        confidence: e.confidence,
      }
    : {
        kind: "quote",
        documentId: e.documentId,
        section: e.section,
        start: e.start,
        end: e.end,
        quote: e.quote,
        confidence: e.confidence,
      };

/**
 * What states each of up to TYPED_LIMITS.evidenceFacts facts (keyed by
 * typedFactKey), by key: its quotes, then its rows. A fact whose read failed
 * has none.
 */
export function factEvidencePlan(
  facts: readonly Fact[],
): ReadPlan<Map<string, Evidence[]>> {
  const keys = [
    ...new Set(facts.flatMap((f) => (f.factKey ? [f.factKey] : []))),
  ].flatMap((k) => {
    const fact = factOfKey(k);
    return fact ? [[k, fact] as const] : [];
  });
  const read = keys.slice(0, TYPED_LIMITS.evidenceFacts);
  return {
    reads: read.map(([, fact]) => ({
      evidence: { fact, limit: TYPED_LIMITS.evidence },
    })),
    parse: (outcomes) =>
      new Map(
        read.map(([k], i) => {
          const r = outcomeOf(outcomes[i], parseEvidenceResult);
          return [k, r.ok ? r.value.items.map(evidenceOf) : []];
        }),
      ),
  };
}

// ---------------------------------------------------------------- An entity

/**
 * One entity's page from typed reads: its props (a row read of the entity
 * by its id, 19 props a read), its facts (a read per side of each of its
 * type's links, the other end with the props that name it, up to
 * factsPerSide each), the rows that state it and, per prop, the rows that
 * set it and the quotes stating a value of it, and the rule breaks naming
 * it. Facts only rules give are the Derived tab's; mentions and reviews are
 * Foundry's own records, which typed reads don't give.
 */
export function entityPlan(
  identity: EntityIdentity,
  naming: Naming,
): ReadPlan<EntityGraph> {
  const t = naming.types.get(identity.type),
    ref = refOf(identity);
  const props = (t?.props || []).map((p) => p.name).filter((p) => p !== "name");
  const groups = props.length
    ? chunks(props, TYPED_LIMITS.columns - 1)
    : [[] as string[]];
  const sides = [...naming.ends].flatMap(([link, e]) => [
    ...(e.from === identity.type
      ? [{ link, direction: "out" as const, other: e.to }]
      : []),
    ...(e.to === identity.type
      ? [{ link, direction: "in" as const, other: e.from }]
      : []),
  ]);
  const read = sides.slice(0, TYPED_LIMITS.links),
    quoted = props.slice(0, TYPED_LIMITS.propEvidence);
  const reads: Read[] = [
    ...groups.map(
      (g): Read => ({
        query: {
          start: startAt(identity),
          return: {
            rows: {
              columns: [{ at: 0 }, ...g.map((prop) => ({ at: 0, prop }))],
              limit: 1,
            },
          },
        },
      }),
    ),
    ...read.map(
      (s): Read => ({
        query: {
          start: startAt(identity),
          steps: [{ via: s.link, dir: s.direction, to: s.other }],
          return: {
            rows: {
              columns: [
                { at: 0 },
                { at: 1 },
                ...namingProps(naming.types.get(s.other)).map((prop) => ({
                  at: 1,
                  prop,
                })),
              ],
              limit: LIMITS.factsPerSide,
            },
          },
        },
      }),
    ),
    { evidence: { entity: ref, limit: TYPED_LIMITS.about } },
    ...quoted.map(
      (prop): Read => ({
        evidence: { entity: ref, prop, limit: TYPED_LIMITS.evidence },
      }),
    ),
    { violations: { entity: ref, limit: TYPED_LIMITS.violations } },
  ];
  const parse = (outcomes: readonly Outcome<unknown>[]): EntityGraph => {
    let at = 0;
    const next = () => outcomes[at++];
    const found = groups.map((g) => {
      const r = outcomeOf(next(), parseQueryResult);
      // The entity's own read failing is the page's failure.
      if (!r.ok) throw new Error(readFailureText(r.error));
      const row = r.value.kind === "rows" ? r.value.rows[0] : undefined;
      return row && { cell: row.cells[0], values: valuesOf(row.cells, g, 1) };
    });
    const entity =
      found[0] &&
      entityOf(
        found[0].cell,
        naming,
        Object.assign({}, ...found.map((f) => f?.values)),
      );
    if (!entity)
      return {
        entity: null,
        facts: [],
        mentions: [],
        reviews: [],
        violations: [],
        rows: [],
        propEvidence: [],
        truncated: {
          facts: false,
          mentions: false,
          reviews: false,
          about: false,
        },
      };
    let cutFacts = read.length < sides.length;
    const facts = read.flatMap((s): Fact[] => {
      const r = rowsOf(next());
      if (!r) {
        cutFacts = true;
        return [];
      }
      // A narrowed read's total is the rows shown, so a full side may be cut.
      if (r.total > r.rows.length || r.rows.length >= LIMITS.factsPerSide)
        cutFacts = true;
      const naming_ = namingProps(naming.types.get(s.other));
      return r.rows.flatMap((row): Fact[] => {
        const edge = row.edges.find((e) => e.step === 0 && "fact" in e);
        const other = entityOf(
          row.cells[1],
          naming,
          valuesOf(row.cells, naming_, 2),
        );
        return edge && "fact" in edge && other
          ? [
              {
                link: s.link,
                direction: s.direction,
                factKey: typedFactKey(edge.fact),
                other,
                props: {},
              },
            ]
          : [];
      });
    });
    const about = outcomeOf(next(), parseEvidenceResult);
    const byProp = quoted.map(
      (prop) => [prop, outcomeOf(next(), parseEvidenceResult)] as const,
    );
    const breaks = outcomeOf(next(), parseViolationsResult);
    // A row is the same row in every read: its source, mapping and key.
    const rowKey = (e: { source: string; mapping: string; rowKey: string }) =>
      JSON.stringify([e.source, e.mapping, e.rowKey]);
    const rows = new Map<string, SourceRow>();
    const items = about.ok ? about.value.items : [];
    for (const e of items)
      if (e.kind === "row")
        rows.set(rowKey(e), {
          source: e.source,
          mapping: e.mapping,
          rowKey: e.rowKey,
          props: [],
        });
    const propEvidence: PropEvidence[] = [];
    for (const [prop, r] of byProp)
      for (const e of r.ok ? r.value.items : []) {
        if (e.kind === "quote") {
          if (e.value !== undefined)
            propEvidence.push({
              documentId: e.documentId,
              section: e.section,
              start: e.start,
              end: e.end,
              prop,
              value: e.value,
              quote: e.quote,
              confidence: e.confidence,
            });
          continue;
        }
        const k = rowKey(e),
          known = rows.get(k);
        if (known) known.props = [...(known.props || []), prop];
        else
          rows.set(k, {
            source: e.source,
            mapping: e.mapping,
            rowKey: e.rowKey,
            props: [prop],
          });
      }
    // The entities a rule break names, by the names this page has.
    const names = new Map([
      [refKey(ref), entity],
      ...facts.flatMap((f) =>
        f.other ? [[refKey(refOf(f.other.identity)), f.other] as const] : [],
      ),
    ]);
    const violations = (breaks.ok ? breaks.value.records : []).map(
      (v): Violation => ({
        constraint: v.constraint,
        severity: v.severity,
        entities: v.entities.flatMap(
          (r) =>
            names.get(refKey(r)) ||
            entityOf({ ...r, name: r.id }, naming) ||
            [],
        ),
      }),
    );
    return {
      entity,
      facts,
      mentions: [],
      reviews: [],
      violations,
      rows: [...rows.values()],
      propEvidence,
      truncated: {
        facts: cutFacts,
        mentions: false,
        reviews: false,
        about:
          !about.ok ||
          items.length >= TYPED_LIMITS.about ||
          quoted.length < props.length,
      },
    };
  };
  return { reads, parse };
}

// ---------------------------------------------------------------- Names

/**
 * The names of entities a page knows only by type and id, as their pages
 * name them, by refKey: a row read per type of their ids, with the props
 * that name them. At most RULE_BREAK_LIMITS.names entities, of types the
 * version knows; null when there are none.
 */
export function namesPlan(
  refs: readonly EntityRef[],
  naming: Naming,
): ReadPlan<Map<string, string>> | null {
  const byType = new Map<string, string[]>();
  for (const r of refs.slice(0, RULE_BREAK_LIMITS.names))
    if (naming.types.has(r.type))
      byType.set(r.type, [...new Set([...(byType.get(r.type) || []), r.id])]);
  if (!byType.size) return null;
  const types = [...byType];
  return {
    reads: types.map(([type, ids]) => ({
      query: {
        start: { type, ids },
        return: {
          rows: {
            columns: [
              { at: 0 },
              ...namingProps(naming.types.get(type)).map((prop) => ({
                at: 0,
                prop,
              })),
            ],
            limit: ids.length,
          },
        },
      },
    })),
    parse: (outcomes) =>
      new Map(
        types.flatMap(([type], i) => {
          const props = namingProps(naming.types.get(type));
          return (rowsOf(outcomes[i])?.rows || []).flatMap((row) => {
            const e = entityOf(
              row.cells[0],
              naming,
              valuesOf(row.cells, props, 1),
            );
            return e
              ? [
                  [
                    refKey(refOf(e.identity)),
                    readable(naming, e.type, e.props, e.dbId).name,
                  ] as const,
                ]
              : [];
          });
        }),
      ),
  };
}

// ---------------------------------------------------------------- Records

/** Each type's count in the load: a count read per type. Null where one failed. */
export function countsPlan(
  types: readonly string[],
): ReadPlan<Map<string, number | null>> {
  const read = types.slice(0, RECORDS_LIMITS.maxTypes);
  return {
    reads: read.map((type) => ({
      query: { start: { type }, return: { count: {} } },
    })),
    parse: (outcomes) =>
      new Map(
        read.map((type, i) => {
          const r = outcomeOf(outcomes[i], parseQueryResult);
          return [
            type,
            r.ok && r.value.kind === "count" ? r.value.count : null,
          ];
        }),
      ),
  };
}

/**
 * The columns a type's records read: its key, names and other props in
 * column order (../records/model.ts columnsOf), as many as one read returns
 * beside the entity.
 *
 * @example
 * typedColumns({ name: "Customer", props: [{ name: "email", type: "string" }, { name: "id", type: "string" }], key: ["id"], names: [] }); // ["id", "email"]
 */
export const typedColumns = (t: TypeNaming) =>
  columnsOf(t)
    .filter((c) => c.role !== "record")
    .map((c) => c.key)
    .slice(0, TYPED_LIMITS.columns - 1);

/**
 * The rows of one typed read of a type's records, and what it says of the rest.
 * A read refused as not_visible reads none: `refusedBy` says what asked too
 * much, and `refused` says so in words.
 */
export type TypedRecords = Omit<RecordsPage, "more"> & {
  // Rows of the read passed over before these.
  offset: number;
  // How many there are in all; null for a read narrowed to what its reader
  // holds, which says nothing of the rest: it is all there is to show.
  total: number | null;
  more: boolean;
  // "paging": a later window refused because the read turned out narrowed.
  refusedBy?: "sort" | "filter" | "search" | "paging";
};

/** A records read, or why there is none. */
export type TypedRecordsRead =
  | { kind: "read"; plan: ReadPlan<TypedRecords> }
  | { kind: "none" }
  | { kind: "refused"; message: string };

const INTEGER = /^[+-]?\d+$/;

/**
 * A view's search as a query's start: a text search of the type's names
 * (a type with names props, or known by its name), else a test of its
 * first key prop (contains for text, equals for a number); "none" when the
 * text can't match anything of the type.
 */
function searchStart(
  t: TypeNaming,
  text: string,
): Pick<OntologyQuery["start"], "text" | "where"> | "none" {
  const q = text.trim();
  if (!q) return {};
  if (t.names.length || !t.key.length)
    return /[\p{L}\p{N}]/u.test(q) ? { text: q.slice(0, 256) } : "none";
  const key = t.key[0]!,
    type = t.props.find((p) => p.name === key)?.type || "string",
    n = Number(q);
  if (type === "integer")
    return INTEGER.test(q) && Number.isSafeInteger(n)
      ? { where: [{ prop: key, op: "=", value: n }] }
      : "none";
  if (type === "float")
    return Number.isFinite(n)
      ? { where: [{ prop: key, op: "=", value: n }] }
      : "none";
  return { where: [{ prop: key, op: "contains", value: q }] };
}

/**
 * Whether a read of a type's records is narrowed to what its reader holds: the
 * type, or a column it returns, is one the load's view lists as partly held.
 *
 * @example
 * typedNarrows({ name: "Customer", props: [{ name: "email", type: "string" }], key: [], names: [] }, ["Customer.email"]); // true
 * typedNarrows({ name: "Invoice", props: [{ name: "email", type: "string" }], key: [], names: [] }, ["Customer.email"]); // false
 */
export const typedNarrows = (t: TypeNaming, partial: readonly string[]) =>
  partial.includes(t.name) ||
  typedColumns(t).some(
    (c) => c !== "name" && partial.includes(`${t.name}.${c}`),
  );

/**
 * Which read's rows a page of Records comes from: pages are cut from reads of
 * TYPED_LIMITS.rows; a narrowed read is the one read there is.
 *
 * @example
 * typedOffset(5, false); // 200
 * typedOffset(5, true); // 0
 */
export const typedOffset = (page: number, narrowed: boolean) =>
  narrowed
    ? 0
    : Math.floor((page * RECORDS_LIMITS.page) / TYPED_LIMITS.rows) *
      TYPED_LIMITS.rows;

/**
 * The read of a type's records for a view: the rows of the page's read
 * (typedOffset) with the type's columns, in the view's order, and how many
 * there are in all. A filter compares the prop's value as typed (no folding of
 * names). What a reader holds only part of refuses a filter, search or sort on
 * it, which `parse` says rather than throws: the page goes on without a sort,
 * and says so of the others. `narrowed` (typedNarrows): one read of the first
 * rows the reader holds, with no total.
 */
export function typedRecordsRead(
  t: TypeNaming,
  view: RecordsView,
  naming: Naming,
  narrowed = false,
): TypedRecordsRead {
  const search = searchStart(t, view.search);
  if (search === "none") return { kind: "none" };
  const where: QueryWhere[] = [...(search.where || [])];
  const filter = activeFilter(view);
  if (filter) {
    const f = filterOf(t, filter.field, filter.value);
    if (!f.ok) return { kind: "refused", message: f.error };
    where.push(
      f.property.endsWith("__nn")
        ? { prop: filter.field, op: "=", value: filter.value.trim() }
        : { prop: f.property, op: "=", value: f.value },
    );
  }
  const columns = typedColumns(t);
  const by_ = view.sort
    ? view.sort.column === RECORD_COLUMN
      ? t.names[0] || t.key[0] || "name"
      : view.sort.column
    : undefined;
  const searching = !!view.search.trim() || !!filter;
  const offset = typedOffset(view.page, narrowed);
  const refusal = (pointer = ""): TypedRecords => {
    const by = pointer.includes("/offset")
      ? "paging"
      : pointer.includes("/order_by")
        ? "sort"
        : filter && pointer === `/start/where/${where.length - 1}/prop`
          ? "filter"
          : "search";
    return {
      rows: [],
      matched: null,
      offset,
      total: null,
      more: false,
      refusedBy: by,
      refused: `You can’t ${
        by === "sort"
          ? `sort by ${fieldName(by_ || "")}`
          : by === "filter"
            ? `filter by ${fieldName(filter?.field || "")}`
            : `search ${t.name} records`
      }: only part of it is in the sources shared with you.`,
    };
  };
  return {
    kind: "read",
    plan: {
      reads: [
        {
          query: {
            start: {
              type: t.name,
              ...(search.text ? { text: search.text } : {}),
              ...(where.length ? { where } : {}),
            },
            return: {
              rows: {
                columns: [
                  { at: 0 },
                  ...columns.map((prop) => ({ at: 0, prop })),
                ],
                ...(by_
                  ? {
                      order_by: {
                        at: 0,
                        prop: by_,
                        desc: view.sort?.dir === "desc",
                      },
                    }
                  : {}),
                limit: TYPED_LIMITS.rows,
                ...(offset ? { offset } : {}),
              },
            },
          },
        },
      ],
      parse: ([outcome]) => {
        const r = outcomeOf(outcome, parseQueryResult);
        if (!r.ok && r.error.code === "not_visible")
          return refusal(r.error.pointer);
        if (!r.ok) throw new Error(readFailureText(r.error));
        const result = r.value as Extract<QueryResult, { kind: "rows" }>;
        if (result.kind !== "rows")
          throw new Error("The read’s answer is malformed.");
        const rows = result.rows.flatMap((row): RecordRow[] => {
          const props = valuesOf(row.cells, columns, 1);
          const e = entityOf(row.cells[0], naming, props);
          if (!e) return [];
          const name = readable(naming, t.name, e.props, 0);
          return [
            {
              dbId: 0,
              entity: { id: e.id, identity: e.identity },
              name: name.name,
              by: name.by,
              props,
            },
          ];
        });
        return {
          rows,
          offset,
          total: narrowed ? null : result.total,
          more: !narrowed && offset + result.rows.length < result.total,
          matched:
            searching && !narrowed
              ? { count: result.total, capped: false }
              : null,
        };
      },
    },
  };
}

/**
 * One page of the records a typed read returned: RECORDS_LIMITS.page rows from
 * `page`, and whether another follows, in those rows or a later read's; or, of a
 * narrowed read (no total), all its rows.
 *
 * @example
 * typedRecordsPage({ rows: [], matched: null, offset: 0, total: 0, more: false }, 0).more; // false
 */
export function typedRecordsPage(
  read: TypedRecords,
  page: number,
): RecordsPage {
  if (read.total === null) return read;
  const from = page * RECORDS_LIMITS.page - read.offset;
  return {
    ...read,
    // A read of another page's (shown while this one loads) holds none of it.
    rows: from < 0 ? [] : read.rows.slice(from, from + RECORDS_LIMITS.page),
    more: read.rows.length > from + RECORDS_LIMITS.page || read.more,
  };
}
