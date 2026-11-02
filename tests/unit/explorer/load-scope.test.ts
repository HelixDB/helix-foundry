import { describe, expect, it } from "vitest";
import type { DynamicQueryRequest } from "@/web/areas/explore/database-query";
import {
  documentFactsRequest,
  documentRequest,
  entityRequest,
  factEvidenceRequest,
} from "@/web/areas/explore/model";
import type { TypeNaming } from "@/web/areas/explore/naming";
import {
  pageRead,
  typeCountsRequests,
  type RecordsView,
} from "@/web/areas/records/model";
import {
  buildIdentityRequest,
  buildLabelCountsRequest,
  buildLabelPageRequest,
  buildSampleRequest,
  buildSchemaProbeRequest,
  buildSearchRequest,
  EXPLORER_LIMITS,
  fallbackShape,
  parseSearch,
} from "@/web/areas/explore/explorer/lib/queries";
import type { NodeFetchShape } from "@/web/areas/explore/explorer/lib/types";
import { holds, scopedLabel, type Scope } from "@/web/areas/explore/load-scope";

// The Explorer's reads in a load (load-versioning plan, section 3): every
// label source is `$label == x and foundry_load == L` (two bitmaps), nothing
// reads the whole graph.

const LOAD: Scope = { id: "01JDLOAD" };
const SHAPE: NodeFetchShape = { keys: ["name"], heavyKeys: [] };

/** Every object in a request, depth first. */
function operations(
  value: unknown,
  found: Record<string, unknown>[] = [],
): Record<string, unknown>[] {
  if (Array.isArray(value)) value.forEach((item) => operations(item, found));
  else if (value && typeof value === "object") {
    found.push(value as Record<string, unknown>);
    for (const child of Object.values(value)) operations(child, found);
  }
  return found;
}

const P_LOAD = JSON.stringify({
  eq: {
    left: { property: "foundry_load" },
    right: { constant: { string: "01JDLOAD" } },
  },
});

const CUSTOMER: TypeNaming = {
  name: "Customer",
  props: [
    { name: "id", type: "string" },
    { name: "name", type: "string" },
    { name: "since", type: "integer" },
  ],
  key: ["id"],
  names: ["name"],
};
const VIEW: RecordsView = {
  type: "Customer",
  search: "",
  filter: null,
  sort: null,
  page: 0,
  record: "",
};
const page = (scope: Scope, view: Partial<RecordsView>) => {
  const read = pageRead(scope, CUSTOMER, { ...VIEW, ...view });
  if (read.kind !== "read") throw new Error(`no read: ${read.kind}`);
  return read.request;
};

/**
 * The reads of rows (not counts) of every view: the Explorer's identity
 * lookup and label page, Records' pages, an entity's page and a document's
 * view.
 */
const reads = (scope: Scope): DynamicQueryRequest[] => [
  buildIdentityRequest({
    scope,
    label: "Customer",
    property: "entity_key",
    value: "cus_1",
    shape: SHAPE,
  }),
  buildLabelPageRequest({
    scope,
    label: "Customer",
    afterDbId: null,
    shape: SHAPE,
  }),
  page(scope, {}),
  page(scope, { page: 2, sort: { column: "since", dir: "desc" } }),
  page(scope, { search: "glob", filter: { field: "since", value: "2020" } }),
  entityRequest(scope, { type: "Customer", key: "cus_1" }, ["name"]),
  entityRequest(scope, { type: "Customer", name: "globex" }),
  documentRequest(scope, "doc_1", ["name"]),
  documentFactsRequest(scope, [
    {
      section: 0,
      start: 0,
      end: 5,
      quote: "Globex",
      link: "billed_to",
      factKey: "f1",
    },
  ])!,
  factEvidenceRequest(scope, [
    { link: "billed_to", direction: "out", factKey: "f1", props: {} },
  ])!,
];

/** Every scoped builder, in a load. */
const builders = (scope: Scope): DynamicQueryRequest[] => [
  ...reads(scope),
  ...typeCountsRequests(scope, ["Customer", "Invoice"]).map((b) => b.request),
  buildLabelCountsRequest(
    { nodes: ["Customer", "Invoice"], edges: ["billed_to"] },
    { scope },
  ),
  buildLabelCountsRequest(
    { nodes: ["Customer"], edges: ["billed_to"] },
    { scope, probeRows: 2 },
  ),
  buildSchemaProbeRequest(
    { nodes: ["Customer"], edges: ["billed_to"] },
    { scope },
  ),
  buildSampleRequest({
    scope,
    nodeLabels: [
      { label: "Customer", count: 900 },
      { label: "Invoice", count: 30 },
    ],
    edgeLabels: [{ label: "billed_to", count: 30 }],
    shape: SHAPE,
  }),
  buildSampleRequest({ scope, nodeLabels: null, shape: fallbackShape() }),
  buildLabelPageRequest({
    scope,
    label: "Customer",
    afterDbId: null,
    shape: SHAPE,
  }),
  buildLabelPageRequest({
    scope,
    label: "Customer",
    afterDbId: 50,
    offset: 50,
    shape: SHAPE,
  }),
  buildSearchRequest({
    scope,
    text: "globex",
    idCandidate: 12,
    textIndexes: [{ label: "Customer", property: "name" }],
    containsTargets: [{ label: "Invoice", keys: ["number"] }],
    overFetch: 3,
    shape: SHAPE,
  })!,
];

describe("reads in a load", () => {
  it("never read the whole graph", () => {
    for (const request of builders(LOAD)) {
      const json = JSON.stringify(request.query);
      expect(json, request.query_name ?? "").not.toContain('"reference":"all"');
    }
  });

  it("carry the load in every nodes_where and edges_where", () => {
    for (const request of builders(LOAD))
      for (const op of operations(request.query)) {
        const source = (op.nodes_where ?? op.edges_where) as
          | { predicate: unknown }
          | undefined;
        if (!source) continue;
        expect(
          JSON.stringify(source.predicate),
          request.query_name ?? "",
        ).toContain(P_LOAD);
      }
  });

  it("read a label as P(label, L): both constants, so both bitmaps serve it", () => {
    expect(scopedLabel(LOAD, "Customer")).toEqual({
      and: {
        predicates: [
          {
            eq: {
              left: { property: "$label" },
              right: { constant: { string: "Customer" } },
            },
          },
          JSON.parse(P_LOAD),
        ],
      },
    });
    const counts = buildLabelCountsRequest(
      { nodes: ["Customer"], edges: [] },
      { scope: LOAD },
    );
    expect(counts.parameters).toEqual({});
  });

  it("check an id hit against the load, and over-fetch text hits to filter them", () => {
    const request = buildSearchRequest({
      scope: LOAD,
      text: "globex",
      idCandidate: 12,
      textIndexes: [{ label: "Customer", property: "name" }],
      containsTargets: [],
      k: 2,
      overFetch: 3,
      shape: SHAPE,
    })!;
    const json = JSON.stringify(request.query);
    expect(json).toContain(
      `"where":{"input":{"nodes":{"reference":{"param":"id"}}},"predicate":${P_LOAD}}`,
    );
    expect(json).toContain('"k":{"literal":6}');
    const row = (id: number, load?: string) => ({
      $id: id,
      $label: "Customer",
      ...(load ? { foundry_load: load } : {}),
      name: "Globex",
      $score: 10 - id,
    });
    const hits = parseSearch(
      {
        text_0: [
          row(1, "other"),
          row(2, "01JDLOAD"),
          row(3),
          row(4, "01JDLOAD"),
          row(5, "01JDLOAD"),
        ],
      },
      SHAPE,
      {
        scope: LOAD,
        k: 2,
        text: "globex",
        textIndexes: [{ label: "Customer", property: "name" }],
      },
    );
    expect(hits.map((hit) => hit.node.dbId)).toEqual([2, 4]);
  });
});

describe("a load's rows", () => {
  it("projects the load within the shape's key cap", () => {
    const keys = Array.from({ length: 300 }, (_, i) => `k${i}`);
    const json = JSON.stringify(
      buildLabelPageRequest({
        scope: LOAD,
        label: "Customer",
        afterDbId: null,
        shape: { keys, heavyKeys: [] },
      }).query,
    );
    const properties = JSON.parse(
      /"properties":(\[[^\]]*\])/.exec(json)![1]!,
    ) as string[];
    expect(properties.slice(0, 3)).toEqual(["$id", "$label", "foundry_load"]);
    expect(properties).toHaveLength(2 + EXPLORER_LIMITS.maxShapeKeys);
  });

  it("holds only what carries the load's id", () => {
    expect(holds(LOAD, { foundry_load: "01JDLOAD" })).toBe(true);
    expect(holds(LOAD, { foundry_load: "L1" })).toBe(false);
    expect(holds(LOAD, { foundry_load: null })).toBe(false);
    expect(holds(LOAD, {})).toBe(false);
  });
});
