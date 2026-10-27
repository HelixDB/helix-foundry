import {
  FOUNDRY_EDGES as E,
  FOUNDRY_NODES as N,
} from "../../../shared/foundry-labels";
import type { EntityRef } from "../../../shared/foundry-read";
import type { DynamicQueryRequest, JsonValue } from "./database-query";
import { MAX_COUNTS, scopedPredicates } from "./load-scope";
import { entityRow, type EntityRow } from "./model";
import { namingKeys, readable, type Naming } from "./naming";

// A load's rule breaks: the FoundryViolation records its sweeps and commits
// keep, one per open violation of a constraint, -FOUNDRY_CONCERNS
// {position}-> each entity it names. Read from the database with the
// user's session (POST /v1/databases:query-read), like every other read of
// a load (./load-scope.ts), never through the rules:
// - counts per constraint: bitmap counts of `$label`, `foundry_load` and
//   `constraint` (equality-indexed), the constraint a constant;
// - a page of one constraint's breaks in `violation_key` order (range-
//   indexed), after the last key of the page before, or from a key a
//   citation names: one row more than a page, to know whether another
//   follows, with the entities each names;
// - the names of the entities the rules' reads name (they carry only a
//   type and an id): one lookup per type, by entity_key or norm_name.

export const RULE_BREAK_LIMITS = Object.freeze({
  // Breaks on a page.
  page: 50,
  // Entities named per lookup.
  names: 100,
  // Constraints counted per request.
  counts: MAX_COUNTS,
});

type Ast = { [key: string]: JsonValue };
const constant = (property: string, value: string): Ast => ({
  eq: { left: { property }, right: { constant: { string: value } } },
});
const breaksOf = (loadId: string, constraint: string, ...more: Ast[]): Ast => ({
  nodes_where: {
    predicate: {
      and: {
        predicates: [
          ...scopedPredicates(
            { kind: "load", id: loadId },
            "node",
            N.Violation,
          ),
          constant("constraint", constraint),
          ...more,
        ],
      },
    },
  },
});
const v = (name: string): Ast => ({ nodes: { reference: { var: name } } });

function request(
  name: string,
  entries: { name: string; root: Ast; returned?: boolean }[],
  params: Record<string, { value: JsonValue; type: JsonValue }> = {},
): DynamicQueryRequest {
  return {
    request_type: "read",
    query_name: name,
    query: {
      read: {
        entries: entries.map((e) => ({
          query: { name: e.name, root: e.root },
        })),
        returns: entries.filter((e) => e.returned !== false).map((e) => e.name),
      },
    },
    parameters: Object.fromEntries(
      Object.entries(params).map(([k, p]) => [k, p.value]),
    ),
    parameter_types: Object.fromEntries(
      Object.entries(params).map(([k, p]) => [k, p.type]),
    ),
  };
}

/**
 * The count of each constraint's breaks in the load, in requests of at most
 * RULE_BREAK_LIMITS.counts constraints.
 */
export function ruleBreakCountRequests(
  loadId: string,
  constraints: readonly string[],
): { request: DynamicQueryRequest; constraints: string[] }[] {
  const out: { request: DynamicQueryRequest; constraints: string[] }[] = [];
  for (let i = 0; i < constraints.length; i += RULE_BREAK_LIMITS.counts) {
    const batch = constraints.slice(i, i + RULE_BREAK_LIMITS.counts);
    out.push({
      request: request(
        "foundry_rule_break_counts",
        batch.map((c, k) => ({
          name: `count_${k}`,
          root: { count: { input: breaksOf(loadId, c) } },
        })),
      ),
      constraints: batch,
    });
  }
  return out;
}

/** Each constraint's count from the answers; null when one is missing. */
export function parseRuleBreakCounts(
  bodies: readonly unknown[],
  batches: readonly { constraints: string[] }[],
): Map<string, number | null> {
  const counts = new Map<string, number | null>();
  batches.forEach((batch, b) => {
    const body = bodies[b];
    const data =
      body && typeof body === "object" && !Array.isArray(body)
        ? (body as Record<string, unknown>)
        : {};
    batch.constraints.forEach((c, k) => {
      const n = data[`count_${k}`];
      counts.set(
        c,
        typeof n === "number" && Number.isInteger(n) && n >= 0 ? n : null,
      );
    });
  });
  return counts;
}

/** Where a page starts: after a key (null: the first page), or at one. */
export type PageAt = { after: string | null } | { from: string };

/**
 * One page of a constraint's breaks, from `at`. `names` are the props
 * entities are named by (naming.ts namingKeys).
 */
export function ruleBreakPageRequest(
  loadId: string,
  constraint: string,
  at: PageAt,
  names: string[] = [],
): DynamicQueryRequest {
  const key = "from" in at ? at.from : at.after;
  const later: Ast[] = key
    ? [
        {
          ["from" in at ? "gte" : "gt"]: {
            left: { property: "violation_key" },
            right: { param: "after" },
          },
        },
      ]
    : [];
  return request(
    "foundry_rule_breaks",
    [
      {
        name: "vs",
        root: {
          limit: {
            input: {
              order_by: {
                input: breaksOf(loadId, constraint, ...later),
                property: "violation_key",
                order: "asc",
              },
            },
            count: { literal: RULE_BREAK_LIMITS.page + 1 },
          },
        },
        returned: false,
      },
      {
        name: "breaks",
        root: {
          values: {
            input: v("vs"),
            properties: ["$id", "violation_key", "severity"],
          },
        },
      },
      {
        name: "concerns",
        root: {
          edge_properties: {
            input: { out_e: { input: v("vs"), label: E.CONCERNS } },
          },
        },
      },
      {
        name: "ends",
        root: {
          values: {
            input: {
              dedup: { input: { out: { input: v("vs"), label: E.CONCERNS } } },
            },
            properties: [
              "$id",
              "$label",
              "name",
              "entity_key",
              "norm_name",
              ...names,
            ],
          },
        },
      },
    ],
    key ? { after: { value: key, type: "string" } } : {},
  );
}

/** One break: its record's key and severity, and the entities it names, in order. */
export type RuleBreak = {
  key: string;
  severity?: string;
  entities: EntityRow[];
};
export type RuleBreakPage = {
  breaks: RuleBreak[];
  // The `after` of the next page; null on the last.
  next: string | null;
};

type Row = Record<string, unknown>;
const rows = (v: unknown): Row[] =>
  Array.isArray(v)
    ? v.filter(
        (r): r is Row => !!r && typeof r === "object" && !Array.isArray(r),
      )
    : [];
const int = (v: unknown) =>
  typeof v === "number" && Number.isInteger(v) ? v : undefined;

/** Reads ruleBreakPageRequest's answer; nothing in it is trusted. */
export function parseRuleBreakPage(body: unknown): RuleBreakPage {
  const data =
    body && typeof body === "object" && !Array.isArray(body)
      ? (body as Row)
      : {};
  const ends = new Map<number, EntityRow>();
  for (const r of rows(data.ends)) {
    const e = entityRow(r);
    if (e) ends.set(e.dbId, e);
  }
  const named = new Map<number, { position: number; entity: EntityRow }[]>();
  for (const r of rows(data.concerns)) {
    const from = int(r.$from),
      position = int(r.position),
      entity = ends.get(int(r.$to) ?? -1);
    if (from === undefined || position === undefined || !entity) continue;
    named.set(from, [...(named.get(from) || []), { position, entity }]);
  }
  const all = rows(data.breaks).flatMap((r): RuleBreak[] => {
    const id = int(r.$id),
      key = r.violation_key;
    if (id === undefined || typeof key !== "string" || !key) return [];
    return [
      {
        key,
        severity: typeof r.severity === "string" ? r.severity : undefined,
        entities: (named.get(id) || [])
          .sort((a, b) => a.position - b.position)
          .map((x) => x.entity),
      },
    ];
  });
  const breaks = all.slice(0, RULE_BREAK_LIMITS.page);
  return {
    breaks,
    next: all.length > RULE_BREAK_LIMITS.page ? breaks.at(-1)!.key : null,
  };
}

/** An EntityRef's key in maps of names. */
export const refKey = (r: EntityRef) => `${r.type}\u0000${r.id}`;

/**
 * The names of entities of the load, at most RULE_BREAK_LIMITS.names: one
 * entry per type the version knows, matching its key or normalized name.
 * Null when none of them is of a type the version knows.
 */
export function entityNamesRequest(
  loadId: string,
  refs: readonly EntityRef[],
  naming: Naming,
): DynamicQueryRequest | null {
  const byType = new Map<string, string[]>();
  for (const r of refs.slice(0, RULE_BREAK_LIMITS.names)) {
    if (!naming.types.has(r.type)) continue;
    byType.set(r.type, [...new Set([...(byType.get(r.type) || []), r.id])]);
  }
  if (!byType.size) return null;
  const types = [...byType];
  return request(
    "foundry_entity_names",
    types.map(([type], i) => ({
      name: `t_${i}`,
      root: {
        values: {
          input: {
            nodes_where: {
              predicate: {
                and: {
                  predicates: [
                    ...scopedPredicates(
                      { kind: "load", id: loadId },
                      "node",
                      type,
                    ),
                    {
                      is_in: {
                        value: {
                          property: naming.types.get(type)!.key.length
                            ? "entity_key"
                            : "norm_name",
                        },
                        values: { param: `ids_${i}` },
                      },
                    },
                  ],
                },
              },
            },
          },
          properties: [
            "$id",
            "$label",
            "name",
            "entity_key",
            "norm_name",
            ...namingKeys(naming),
          ],
        },
      },
    })),
    Object.fromEntries(
      types.map(([, ids], i) => [
        `ids_${i}`,
        { value: ids, type: { array: "string" } },
      ]),
    ),
  );
}

/** Each entity's readable name by its refKey, from entityNamesRequest's answer. */
export function parseEntityNames(
  body: unknown,
  naming: Naming,
): Map<string, string> {
  const data =
    body && typeof body === "object" && !Array.isArray(body)
      ? (body as Row)
      : {};
  const names = new Map<string, string>();
  for (const [k, v] of Object.entries(data)) {
    if (!k.startsWith("t_")) continue;
    for (const r of rows(v)) {
      const e = entityRow(r);
      if (!e) continue;
      const id = "key" in e.identity ? e.identity.key : e.identity.name;
      const named = readable(naming, e.type, e.props, e.dbId);
      names.set(
        refKey({ type: e.type, id }),
        named.known ? named.name : e.name,
      );
    }
  }
  return names;
}
