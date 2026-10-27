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
  emptyLabels,
  EXPLORER_LIMITS,
  fallbackShape,
  parseLabelCounts,
  parseSearch,
} from "@/web/areas/explore/explorer/lib/queries";
import type { NodeFetchShape } from "@/web/areas/explore/explorer/lib/types";
import {
  holds,
  isShared,
  otherCount,
  scopedLabel,
  type Scope,
} from "@/web/areas/explore/load-scope";

// The Explorer's reads in a load (load-versioning plan, section 3): every
// label source is `$label == x and foundry_load == L` (two bitmaps), nothing
// reads the whole graph, and Other data is the labels' counts less the
// loads'.

const LOAD: Scope = { kind: "load", id: "01JDLOAD" };
const OTHER: Scope = {
  kind: "other",
  loads: ["L1", "L2"],
  shared: { nodes: new Set(["Customer"]), edges: new Set(["billed_to"]) },
};
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
 * view. In Other data each reads a shared label by its missing load.
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
    expect(scopedLabel(LOAD, "node", "Customer")).toEqual({
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

const IS_NULL = JSON.stringify({ is_null: { property: "foundry_load" } });

/** The labels a source's predicate reads, as constants. */
const labelsOf = (predicate: unknown) =>
  [
    ...JSON.stringify(predicate).matchAll(
      /"left":\{"property":"\$label"\},"right":\{"constant":\{"string":"([^"]+)"/g,
    ),
  ].map((m) => m[1]!);

describe("Other data", () => {
  it("reads a shared label's rows by their missing load, in every view", () => {
    let shared = 0;
    for (const request of reads(OTHER))
      for (const op of operations(request.query)) {
        const target = op.nodes_where ? "node" : op.edges_where ? "edge" : null;
        if (!target) continue;
        const { predicate } = (op.nodes_where ?? op.edges_where) as {
          predicate: unknown;
        };
        const labels = labelsOf(predicate);
        expect(labels.length, request.query_name ?? "").toBeGreaterThan(0);
        const json = JSON.stringify(predicate);
        // Never a load's: no foundry_load constant under Other data.
        expect(json).not.toContain('"property":"foundry_load"},"right"');
        if (
          labels.some((l) =>
            isShared(OTHER as Extract<Scope, { kind: "other" }>, target, l),
          )
        ) {
          shared += 1;
          expect(json, request.query_name ?? "").toContain(IS_NULL);
        }
      }
    expect(shared).toBeGreaterThan(5);
  });

  it("is a label's count less every load's, from bitmaps only", () => {
    expect(otherCount(120, [50, 50])).toBe(20);
    expect(otherCount(10, [5, 6])).toBe(0);
    expect(otherCount(null, [1])).toBeNull();
    expect(otherCount(10, [1, null])).toBeNull();
    const labels = { nodes: ["Customer", "Note"], edges: ["billed_to"] };
    const request = buildLabelCountsRequest(labels, { scope: OTHER });
    const returns = (request.query as { read: { returns: string[] } }).read
      .returns;
    // Customer is shared: its count and one per load. Note is the team's own.
    expect(returns).toEqual([
      "node_count_0",
      "node_count_0_load_0",
      "node_count_0_load_1",
      "node_count_1",
      "edge_count_0",
      "edge_count_0_load_0",
      "edge_count_0_load_1",
    ]);
    expect(JSON.stringify(request.query)).not.toContain("is_null");
    expect(JSON.stringify(request.query)).not.toContain("is_in");
    const body = {
      node_count_0: 130,
      node_count_0_load_0: 64,
      node_count_0_load_1: 64,
      node_count_1: 7,
      edge_count_0: 138,
      edge_count_0_load_0: 69,
      edge_count_0_load_1: 69,
    };
    const stats = parseLabelCounts(body, labels, OTHER);
    expect(stats.nodeLabels).toEqual([
      { label: "Customer", count: 2 },
      { label: "Note", count: 7 },
    ]);
    // billed_to holds nothing but the loads' edges.
    expect(stats.edgeLabels).toEqual([]);
    expect(emptyLabels(body, labels, OTHER)).toEqual({
      nodes: [],
      edges: ["billed_to"],
    });
  });

  it("pages a shared label by its missing load and other labels plainly", () => {
    const shared = JSON.stringify(
      buildLabelPageRequest({
        scope: OTHER,
        label: "Customer",
        afterDbId: null,
        shape: SHAPE,
      }).query,
    );
    expect(shared).toContain('{"is_null":{"property":"foundry_load"}}');
    const own = JSON.stringify(
      buildLabelPageRequest({
        scope: OTHER,
        label: "Note",
        afterDbId: null,
        shape: SHAPE,
      }).query,
    );
    expect(own).not.toContain('foundry_load"}}');
    expect(own).not.toContain("is_null");
  });

  it("leaves out a shared label's total, which no index can count", () => {
    const returns = (label: string) =>
      (
        buildLabelPageRequest({
          scope: OTHER,
          label,
          afterDbId: null,
          shape: SHAPE,
        }).query as { read: { returns: string[] } }
      ).read.returns;
    expect(returns("Customer")).toEqual(["page"]);
    expect(returns("Note")).toEqual(["page", "total"]);
  });

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

  it("counts within maxCountLabels per request: a shared label takes one count per load", () => {
    const many = {
      nodes: Array.from({ length: 30 }, (_, i) => `Customer${i}`),
      edges: [],
    };
    const scope: Scope = {
      ...OTHER,
      shared: { nodes: new Set(many.nodes), edges: new Set() },
    };
    const request = buildLabelCountsRequest(many, { scope });
    const returns = (request.query as { read: { returns: string[] } }).read
      .returns;
    // 3 counts each (the label, two loads): 20 labels fit in 60.
    expect(returns).toHaveLength(60);
    expect(
      parseLabelCounts({}, many, scope).nodeLabels.filter(
        (l) => l.count === null,
      ),
    ).toHaveLength(30);
  });

  it("tells Foundry's labels and the versions' from the team's own", () => {
    expect(
      isShared(
        OTHER as Extract<Scope, { kind: "other" }>,
        "node",
        "FoundryMention",
      ),
    ).toBe(true);
    expect(
      isShared(OTHER as Extract<Scope, { kind: "other" }>, "edge", "billed_to"),
    ).toBe(true);
    expect(
      isShared(OTHER as Extract<Scope, { kind: "other" }>, "node", "Note"),
    ).toBe(false);
    expect(holds(OTHER, { name: "x" })).toBe(true);
    expect(holds(OTHER, { foundry_load: null })).toBe(true);
    expect(holds(OTHER, { foundry_load: "L1" })).toBe(false);
    expect(holds(LOAD, { foundry_load: "01JDLOAD" })).toBe(true);
    expect(holds(LOAD, {})).toBe(false);
  });
});
