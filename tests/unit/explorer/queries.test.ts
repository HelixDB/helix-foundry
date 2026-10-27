import { describe, expect, it } from "vitest";

import type { DynamicQueryRequest } from "@/web/areas/explore/database-query";
import type { IndexCatalogEntry } from "@/web/areas/explore/explorer/lib/types";
import type { Scope } from "@/web/areas/explore/load-scope";

import connectFixture from "./__fixtures__/connect.json";
import discoveryFixture from "./__fixtures__/label-discovery.json";
import labelPageFixture from "./__fixtures__/label-page.json";
import scanFixture from "./__fixtures__/label-scan.json";
import labelsFixture from "./__fixtures__/labels.json";
import missingFixture from "./__fixtures__/neighborhood-missing.json";
import selfLoopFixture from "./__fixtures__/neighborhood-self-loop.json";
import supernodeFixture from "./__fixtures__/neighborhood-supernode.json";
import nodesFullFixture from "./__fixtures__/nodes-full.json";
import sampleFixture from "./__fixtures__/sample.json";
import probeFixture from "./__fixtures__/schema-probe.json";
import searchFixture from "./__fixtures__/search.json";
import type { ProbedFetchShape } from "@/web/areas/explore/explorer/lib/normalize";
import {
  EXPLORER_LIMITS,
  allocateSample,
  buildConnectRequest,
  buildDegreeProbeRequest,
  buildDegreeRequest,
  buildEdgeCountRequest,
  buildLabelCountsRequest,
  buildLabelDiscoveryRequest,
  buildLabelPageRequest,
  buildLabelScanRequest,
  buildNeighborhoodRequest,
  buildNodesByIdRequest,
  buildSampleRequest,
  buildSchemaProbeRequest,
  buildSearchRequest,
  countedLabels,
  emptyLabels,
  extendShape,
  fallbackShape,
  fetchesEdgeProperties,
  isLargeProbe,
  isLightShape,
  labelProbeIds,
  lightShape,
  mergeLabelStats,
  nextEdgeCursor,
  parseConnect,
  parseDegree,
  parseDegreeProbe,
  parseEdgeCount,
  parseLabelCounts,
  parseLabelDiscovery,
  parseLabelPage,
  parseLabelScan,
  parseNeighborhood,
  parseNodes,
  parseSample,
  parseSchemaProbe,
  parseSearch,
  withoutEdgeProperties,
} from "@/web/areas/explore/explorer/lib/queries";
import type {
  GraphNode,
  LabelCount,
  NodeFetchShape,
} from "@/web/areas/explore/explorer/lib/types";

// Fixtures are real responses captured from a local HelixDB v3 by
// queries.live.test.ts (HELIX_EXPLORER_CAPTURE=1). Each holds the builder
// arguments, the request they produced, and the database's response.

type Shaped = { shape: NodeFetchShape };
const shapeOf = (fixture: { args: unknown }) => (fixture.args as Shaped).shape;

// Other data with no load yet: every label reads plainly, as before loads
// were versioned, so the captured requests still apply.
const OTHER: Scope = {
  kind: "other",
  loads: [],
  shared: { nodes: new Set(), edges: new Set() },
};

const SHAPE: ProbedFetchShape = {
  keys: ["name", "title"],
  heavyKeys: ["body", "embedding"],
  heavyByLabel: { Document: ["body", "embedding"], User: [] },
  keysByLabel: { Document: ["title", "word_count"], User: ["name", "email"] },
};

/** Walks the operation tree collecting every `{ param }` reference. */
function paramsUsed(value: unknown, found = new Set<string>()): Set<string> {
  if (Array.isArray(value)) value.forEach((item) => paramsUsed(item, found));
  else if (value && typeof value === "object") {
    for (const [key, child] of Object.entries(value)) {
      if (key === "param" && typeof child === "string") found.add(child);
      else paramsUsed(child, found);
    }
  }
  return found;
}

/** Every operation node in a request, depth first. */
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

const ALL_NODES = JSON.stringify({ nodes: { reference: "all" } });
const ALL_EDGES = JSON.stringify({ edges: { reference: "all" } });

/**
 * Whether a request reads the whole graph: a count or group_count straight
 * over every node or edge (a bounded `limit` in between is fine).
 */
function scansWholeGraph(request: DynamicQueryRequest): boolean {
  return operations(request.query).some((op) => {
    const aggregate = (op.count ?? op.group_count) as
      | { input?: unknown }
      | undefined;
    if (!aggregate || typeof aggregate !== "object" || !("input" in aggregate))
      return false;
    const input = JSON.stringify(aggregate.input);
    return input === ALL_NODES || input === ALL_EDGES;
  });
}

/** Structural checks every explorer request must pass before the server sees it. */
function expectWellFormed(request: DynamicQueryRequest) {
  expect(request.request_type).toBe("read");
  expect(request.query_name).toMatch(/^explorer_[a-z_]+$/);
  const read = (
    request.query as {
      read: { entries: { query: { name: string } }[]; returns: string[] };
    }
  ).read;
  expect(read.entries.length).toBeGreaterThan(0);
  const names = read.entries.map((entry) => entry.query.name);
  expect(new Set(names).size).toBe(names.length);
  for (const name of read.returns) expect(names).toContain(name);
  expect(Object.keys(request.parameter_types ?? {}).sort()).toEqual(
    Object.keys(request.parameters ?? {}).sort(),
  );
  // Every declared parameter is used and every used one is declared.
  expect([...paramsUsed(request.query)].sort()).toEqual(
    Object.keys(request.parameters ?? {}).sort(),
  );
  // Stream bounds are literals: a parameter bound is applied only after the
  // whole input was read (see the note in queries.ts).
  for (const op of operations(request.query)) {
    for (const key of ["limit", "skip"]) {
      const bounded = op[key] as { count?: unknown } | undefined;
      if (bounded && typeof bounded === "object" && "count" in bounded) {
        expect(bounded.count).toEqual({ literal: expect.any(Number) });
      }
    }
  }
}

describe("captured requests", () => {
  // Builders must keep producing the exact requests the live database accepted.
  it.each([
    ["label scan", () => buildLabelScanRequest(), scanFixture],
    ["label discovery", () => buildLabelDiscoveryRequest(), discoveryFixture],
    [
      "label counts with a probe",
      () =>
        buildLabelCountsRequest(labelsFixture.args.labels, {
          scope: OTHER,
          probeRows: labelsFixture.args.probeRows,
        }),
      labelsFixture,
    ],
    [
      "schema probe",
      () => {
        const args = probeFixture.args;
        return buildSchemaProbeRequest(args.labels, {
          scope: OTHER,
          rowsPerLabel: args.rowsPerLabel,
        });
      },
      probeFixture,
    ],
    [
      "sample",
      () =>
        buildSampleRequest({
          ...(sampleFixture.args as Omit<
            Parameters<typeof buildSampleRequest>[0],
            "scope"
          >),
          scope: OTHER,
        }),
      sampleFixture,
    ],
    [
      "label page",
      () =>
        buildLabelPageRequest({
          ...(labelPageFixture.args as Omit<
            Parameters<typeof buildLabelPageRequest>[0],
            "scope"
          >),
          scope: OTHER,
        }),
      labelPageFixture,
    ],
    [
      "neighborhood",
      () =>
        buildNeighborhoodRequest(
          supernodeFixture.args as Parameters<
            typeof buildNeighborhoodRequest
          >[0],
        ),
      supernodeFixture,
    ],
    [
      "neighborhood (missing)",
      () =>
        buildNeighborhoodRequest(
          missingFixture.args as Parameters<typeof buildNeighborhoodRequest>[0],
        ),
      missingFixture,
    ],
    ["connect", () => buildConnectRequest(connectFixture.args), connectFixture],
    [
      "nodes",
      () =>
        buildNodesByIdRequest(
          nodesFullFixture.args as Parameters<typeof buildNodesByIdRequest>[0],
        ),
      nodesFullFixture,
    ],
    [
      "search",
      () =>
        buildSearchRequest({
          ...(searchFixture.args as Omit<
            Parameters<typeof buildSearchRequest>[0],
            "scope"
          >),
          scope: OTHER,
        }),
      searchFixture,
    ],
  ])("%s", (_name, build, fixture) => {
    const request = build()!;
    expect(request).toEqual(fixture.request);
    expectWellFormed(request);
  });
});

describe("label scan (explicit only)", () => {
  it("parses exact label counts and totals", () => {
    const stats = parseLabelScan(scanFixture.response);
    expect(stats.nodeLabels).toContainEqual({ label: "Document", count: 300 });
    expect(stats.edgeLabels).toContainEqual({ label: "TAGGED", count: 1230 });
    expect(stats.nodeTotal).toBe(1540);
    expect(stats.edgeTotal).toBe(5605);
    expect(stats.partial).toBeUndefined();
  });

  it("tolerates junk", () => {
    expect(parseLabelScan(null)).toEqual({
      nodeLabels: [],
      edgeLabels: [],
      nodeTotal: null,
      edgeTotal: null,
    });
    expect(
      parseLabelScan({
        node_labels: [{ $label: "A", count: "x" }, { count: 2 }, 7],
        node_total: -1,
      }),
    ).toEqual({
      nodeLabels: [{ label: "A", count: null }],
      edgeLabels: [],
      nodeTotal: null,
      edgeTotal: null,
    });
  });

  it("is the only builder that reads the whole graph", () => {
    expect(scansWholeGraph(buildLabelScanRequest())).toBe(true);
  });
});

describe("label discovery", () => {
  it("parses a captured discovery: first rows and id probes, merged", () => {
    const found = parseLabelDiscovery(discoveryFixture.response);
    expect(found.nodes).toEqual([
      "Company",
      "Document",
      "Event",
      "Project",
      "User",
    ]);
    expect(found.edges).toEqual(
      expect.arrayContaining(["FOLLOWS", "TAGGED", "WORKS_AT"]),
    );
    // 500 rows came back, so there may be more: nothing proves the lists complete.
    expect(found).toMatchObject({
      nodesExhausted: false,
      edgesExhausted: false,
      empty: false,
    });
  });

  it("knows a short first read saw everything, and an empty one means no data", () => {
    const small = {
      first_nodes: [
        { $label: "A", count: 3 },
        { $label: "B", count: 1 },
      ],
      first_edges: [{ $label: "R", count: 2 }],
      probed_nodes: [],
      probed_edges: [],
    };
    expect(parseLabelDiscovery(small)).toEqual({
      nodes: ["A", "B"],
      edges: ["R"],
      nodesExhausted: true,
      edgesExhausted: true,
      empty: false,
    });
    expect(
      parseLabelDiscovery({ ...small, first_nodes: [], first_edges: [] }).empty,
    ).toBe(true);
    // An unreadable body proves nothing.
    expect(parseLabelDiscovery(null)).toEqual({
      nodes: [],
      edges: [],
      nodesExhausted: false,
      edgesExhausted: false,
      empty: false,
    });
  });

  it("reads a bounded prefix and point-reads ids spread over the id space", () => {
    const request = buildLabelDiscoveryRequest();
    expectWellFormed(request);
    expect(scansWholeGraph(request)).toBe(false);
    const json = JSON.stringify(request.query);
    expect(json).toContain(
      `"limit":{"input":${ALL_NODES},"count":{"literal":${EXPLORER_LIMITS.discoveryRows}}}`,
    );
    expect(json).toContain(
      `"limit":{"input":${ALL_EDGES},"count":{"literal":${EXPLORER_LIMITS.discoveryRows}}}`,
    );
    const ids = labelProbeIds();
    expect(request.parameters).toEqual({ probe_ids: ids });
    expect(ids[0]).toBe(512);
    expect(ids.every((id, index) => index === 0 || id > ids[index - 1])).toBe(
      true,
    );
    expect(Number.isSafeInteger(ids.at(-1)!)).toBe(true);
  });
});

describe("label counts", () => {
  const { labels } = labelsFixture.args;

  it("parses captured bitmap counts and drops labels that are gone", () => {
    const stats = parseLabelCounts(labelsFixture.response, labels, OTHER);
    expect(stats.nodeLabels).toEqual([
      { label: "Chunk", count: 40 },
      { label: "Company", count: 60 },
      { label: "Document", count: 300 },
      { label: "Event", count: 260 },
      { label: "Project", count: 200 },
      { label: "User", count: 600 },
    ]);
    expect(stats.edgeLabels.map((entry) => entry.label)).not.toContain("GONE");
    expect(stats.nodeTotal).toBe(1460);
    // Labels came from discovery, so the totals are floors (Tag was never seen).
    expect(stats.partial).toEqual({ nodes: true, edges: true });
    // The probe rows in the same response feed the schema probe.
    const { schemas, shape } = parseSchemaProbe(labelsFixture.response, {
      nodes: stats.nodeLabels.map((entry) => entry.label),
      edges: stats.edgeLabels.map((entry) => entry.label),
    });
    expect(schemas.find((schema) => schema.label === "Chunk")?.sampled).toBe(1);
    expect(shape.heavyKeys).toEqual(
      expect.arrayContaining(["embedding", "body"]),
    );
  });

  it("counts from label bitmaps and never reads the whole graph", () => {
    const request = buildLabelCountsRequest(
      { nodes: ["A", "B"], edges: ["R"] },
      { scope: OTHER },
    );
    expectWellFormed(request);
    expect(request.query_name).toBe("explorer_label_counts");
    expect(scansWholeGraph(request)).toBe(false);
    // Labels are constants, so the label bitmap serves the count on any engine (no parameter specialization needed).
    expect(request.parameters).toEqual({});
    expect(
      (request.query as { read: { returns: string[] } }).read.returns,
    ).toEqual(["node_count_0", "node_count_1", "edge_count_0"]);
    expect(JSON.stringify(request.query)).toContain(
      '"count":{"input":{"nodes_where":{"predicate":{"eq":{"left":{"property":"$label"},"right":{"constant":{"string":"A"}}}}}}}',
    );
    expect(JSON.stringify(request.query)).toContain(
      '"count":{"input":{"edges_where":{"predicate":{"eq":{"left":{"property":"$label"},"right":{"constant":{"string":"R"}}}}}}}',
    );
    expect(JSON.stringify(request.query)).not.toContain('"param"');
    const probed = buildLabelCountsRequest(
      { nodes: ["A"], edges: [] },
      { scope: OTHER, probeRows: 2 },
    );
    expectWellFormed(probed);
    expect(probed.query_name).toBe("explorer_labels");
    expect(
      (probed.query as { read: { returns: string[] } }).read.returns,
    ).toEqual(["node_count_0", "node_0"]);
    // Nothing to count still makes a valid (and free) request.
    const none = buildLabelCountsRequest(
      { nodes: [], edges: [] },
      { scope: OTHER },
    );
    expectWellFormed(none);
    expect(scansWholeGraph(none)).toBe(false);
  });

  it("caps the labels per request and leaves the rest uncounted", () => {
    const many = Array.from(
      { length: 70 },
      (_, index) => `L${String(index).padStart(2, "0")}`,
    );
    const capped = countedLabels(
      { nodes: [...many, "L00", ""], edges: [] },
      OTHER,
    );
    expect(capped.nodes).toHaveLength(EXPLORER_LIMITS.maxCountLabels);
    expect(capped.dropped.nodes).toEqual(
      many.slice(EXPLORER_LIMITS.maxCountLabels),
    );
    const request = buildLabelCountsRequest(
      { nodes: many, edges: [] },
      { scope: OTHER, probeRows: 1 },
    );
    const returns = (request.query as { read: { returns: string[] } }).read
      .returns;
    expect(
      returns.filter((name) => name.startsWith("node_count_")),
    ).toHaveLength(EXPLORER_LIMITS.maxCountLabels);
    expect(returns.filter((name) => /^node_\d+$/.test(name))).toHaveLength(
      EXPLORER_LIMITS.maxProbeLabels,
    );
    const body = Object.fromEntries(
      Array.from({ length: EXPLORER_LIMITS.maxCountLabels }, (_, index) => [
        `node_count_${index}`,
        1,
      ]),
    );
    const stats = parseLabelCounts(body, { nodes: many, edges: [] }, OTHER, {
      nodes: true,
      edges: true,
    });
    expect(
      stats.nodeLabels.filter((entry) => entry.count === null),
    ).toHaveLength(10);
    expect(stats.nodeTotal).toBe(EXPLORER_LIMITS.maxCountLabels);
    // Uncounted labels make the total a floor even when every label is known.
    expect(stats.partial).toEqual({ nodes: true, edges: false });
  });

  it("merges counts for labels seen later", () => {
    const stats = parseLabelCounts(
      { node_count_0: 5, node_count_1: 0 },
      { nodes: ["A", "B"], edges: [] },
      OTHER,
    );
    const merged = mergeLabelStats(
      {
        ...stats,
        nodeLabels: [...stats.nodeLabels, { label: "C", count: null }],
      },
      parseLabelCounts(
        { node_count_0: 7, edge_count_0: 2 },
        { nodes: ["C"], edges: ["R"] },
        OTHER,
      ),
    );
    expect(merged.nodeLabels).toEqual([
      { label: "A", count: 5 },
      { label: "C", count: 7 },
    ]);
    expect(merged.edgeLabels).toEqual([{ label: "R", count: 2 }]);
    expect(merged.nodeTotal).toBe(12);
    expect(merged.edgeTotal).toBe(2);
    expect(merged.partial).toEqual({ nodes: true, edges: true });
  });
});

describe("labels counted at zero", () => {
  it("names the counted labels a response found empty, and drops their uncounted entries from stats", () => {
    const asked = { nodes: ["Gone", "User"], edges: ["OLD"] };
    const body = { node_count_0: 0, node_count_1: 3, edge_count_0: 0 };
    expect(emptyLabels(body, asked, OTHER)).toEqual({
      nodes: ["Gone"],
      edges: ["OLD"],
    });
    const stats = {
      nodeLabels: [
        { label: "Gone", count: null },
        { label: "User", count: null },
        { label: "Company", count: 2 },
      ],
      edgeLabels: [{ label: "OLD", count: null }],
      nodeTotal: 2,
      edgeTotal: 0,
      partial: { nodes: true, edges: true },
    };
    const merged = mergeLabelStats(
      stats,
      parseLabelCounts(body, asked, OTHER),
      emptyLabels(body, asked, OTHER),
    );
    expect(merged.nodeLabels).toEqual([
      { label: "Company", count: 2 },
      { label: "User", count: 3 },
    ]);
    expect(merged.edgeLabels).toEqual([]);
    expect(merged.nodeTotal).toBe(5);
  });
});

describe("schema probe", () => {
  const { labels, indexes } = probeFixture.args;
  const parsed = parseSchemaProbe(
    probeFixture.response,
    labels,
    indexes as IndexCatalogEntry[],
  );
  const shape = parsed.shape as ProbedFetchShape;

  it("detects vectors, long text and heavy objects from real rows", () => {
    expect(shape.heavyKeys).toEqual(["body", "embedding", "payload"]);
    expect(shape.heavyByLabel).toEqual({
      Document: ["body", "embedding"],
      Event: ["payload"],
      User: [],
    });
    for (const key of shape.heavyKeys) expect(shape.keys).not.toContain(key);
    expect(shape.keys).toEqual(
      expect.arrayContaining(["title", "name", "email", "kind", "joined"]),
    );
    expect(shape.keysByLabel?.Document).toEqual(
      expect.arrayContaining(["title", "word_count", "metadata"]),
    );
  });

  it("builds label schemas with kinds and index badges", () => {
    const document = parsed.schemas.find(
      (schema) => schema.target === "node" && schema.label === "Document",
    )!;
    expect(document.sampled).toBe(1);
    const byKey = Object.fromEntries(
      document.properties.map((property) => [property.key, property]),
    );
    expect(byKey.embedding).toMatchObject({
      kinds: ["vector"],
      heavy: true,
      seen: 1,
    });
    expect(byKey.body).toMatchObject({ kinds: ["string"], heavy: true });
    expect(byKey.title).toMatchObject({
      kinds: ["string"],
      heavy: false,
      indexes: ["full_text"],
    });
    expect(byKey.word_count.kinds).toEqual(["integer"]);
    expect(byKey.draft.kinds).toEqual(["boolean"]);
    expect(byKey.metadata.kinds).toEqual(["object"]);
    const user = Object.fromEntries(
      parsed.schemas
        .find((schema) => schema.label === "User")!
        .properties.map((property) => [property.key, property]),
    );
    expect(user.email.indexes).toEqual(["unique"]);
    expect(user.joined.kinds).toEqual(["datetime"]);
    expect(user.score.kinds[0]).toMatch(/^(float|integer)$/);
    const follows = parsed.schemas.find(
      (schema) => schema.target === "edge" && schema.label === "FOLLOWS",
    )!;
    expect(follows.sampled).toBe(1);
    // Catalog labels outside the request are ignored.
    expect(
      parsed.schemas.find((schema) => schema.label === "Chunk"),
    ).toBeUndefined();
  });

  it("adds unprobed labels with what the catalog knows", () => {
    const { schemas, shape: withChunk } = parseSchemaProbe(
      probeFixture.response,
      { nodes: [...labels.nodes, "Chunk"], edges: labels.edges },
      indexes as IndexCatalogEntry[],
    );
    expect(schemas.find((schema) => schema.label === "Chunk")).toEqual({
      target: "node",
      label: "Chunk",
      sampled: 0,
      properties: [
        {
          key: "embedding",
          kinds: ["vector"],
          heavy: true,
          indexes: ["vector"],
          seen: 0,
        },
      ],
    });
    // Its vector is heavy although no Chunk row was probed.
    expect((withChunk as ProbedFetchShape).heavyByLabel?.Chunk).toEqual([
      "embedding",
    ]);
    expect((withChunk as ProbedFetchShape).keysByLabel?.Chunk).toBeUndefined();
  });

  it("falls back to full rows when nothing was probed", () => {
    expect(parseSchemaProbe({}, { nodes: ["A"], edges: [] }).shape).toEqual(
      fallbackShape(),
    );
    expect(parseSchemaProbe("oops", { nodes: [], edges: [] }).shape).toEqual(
      fallbackShape(),
    );
  });

  it("caps probed labels and always sends at least one entry", () => {
    const many = Array.from({ length: 50 }, (_, index) => `L${index}`);
    const request = buildSchemaProbeRequest(
      { nodes: many, edges: many },
      { scope: OTHER },
    );
    expectWellFormed(request);
    const entries = (request.query as { read: { entries: unknown[] } }).read
      .entries;
    expect(entries).toHaveLength(EXPLORER_LIMITS.maxProbeLabels * 2);
    expectWellFormed(
      buildSchemaProbeRequest({ nodes: [], edges: [] }, { scope: OTHER }),
    );
  });

  it("drops a key from the shared keys when it is heavy on any label", () => {
    const body = {
      node_0: [{ $id: 1, $label: "A", data: "x".repeat(5000), name: "a" }],
      node_1: [{ $id: 2, $label: "B", data: "short", name: "b" }],
    };
    const result = parseSchemaProbe(body, { nodes: ["A", "B"], edges: [] })
      .shape as ProbedFetchShape;
    expect(result.keys).toEqual(["name"]);
    expect(result.heavyByLabel).toEqual({ A: ["data"], B: [] });
    // B's own pages can still fetch it.
    const page = buildLabelPageRequest({
      scope: OTHER,
      label: "B",
      afterDbId: null,
      shape: result,
    });
    expect(JSON.stringify(page.query)).toContain(
      '"properties":["$id","$label","foundry_load","data","name"]',
    );
  });
});

describe("sample", () => {
  const shape = shapeOf(sampleFixture);

  it("parses a captured per-label sample", () => {
    const sample = parseSample(sampleFixture.response, shape);
    expect(sample.truncated).toBe(true);
    expect(sample.nodes.length).toBeLessThanOrEqual(20);
    expect(
      new Set(sample.nodes.map((node) => node.label)).size,
    ).toBeGreaterThanOrEqual(3);
    const ids = new Set(sample.nodes.map((node) => node.id));
    for (const edge of sample.edges)
      expect(ids.has(edge.source) && ids.has(edge.target)).toBe(true);
    for (const node of sample.nodes)
      expect(node.properties).not.toHaveProperty("embedding");
  });

  it("seeds every node label and connects them with a few edges of every edge label, never expanding a node", () => {
    const request = buildSampleRequest({
      scope: OTHER,
      nodeLabels: [
        { label: "A", count: 1000 },
        { label: "B", count: null },
      ],
      edgeLabels: [
        { label: "R", count: 5000 },
        { label: "S", count: 2 },
      ],
      shape: SHAPE,
    });
    expectWellFormed(request);
    const json = JSON.stringify(request.query);
    expect(request.parameters).toEqual({});
    expect(scansWholeGraph(request)).toBe(false);
    // No per-node edge reads: a supernode seed would cost its whole degree.
    expect(json).not.toMatch(/"(out_e|in_e|union)"/);
    // Seeds and edges are literal limits over their label bitmaps (constant labels).
    const takes = (source: string) =>
      [
        ...json.matchAll(
          new RegExp(
            `"${source}":\\{"predicate":\\{"eq":\\{"left":\\{"property":"\\$label"\\},"right":\\{"constant":\\{"string":"(\\w+)"\\}\\}\\}\\}\\}\\},"count":\\{"literal":(\\d+)\\}`,
            "g",
          ),
        ),
      ].map((match) => [match[1], Number(match[2])] as const);
    const seeds = takes("nodes_where");
    const edgeTakes = takes("edges_where");
    // A label without a count is unknown, not huge: it comes after the counted one and gets the minimum.
    expect(seeds.map(([label]) => label)).toEqual(["A", "B"]);
    expect(seeds[1][1]).toBe(EXPLORER_LIMITS.sampleMinPerLabel);
    expect(edgeTakes).toEqual([
      ["S", 2],
      ["R", 158],
    ]);
    const edges = edgeTakes.map(([, take]) => take);
    // Seeds plus both ends of every edge fit the node budget.
    expect(
      seeds[0][1] + seeds[1][1] + 2 * (edges[0] + edges[1]),
    ).toBeLessThanOrEqual(EXPLORER_LIMITS.sampleNodes);
    expect(
      (request.query as { read: { returns: string[] } }).read.returns,
    ).toEqual(["nodes", "edges"]);
  });

  it("never reads the whole graph: no labels, no rows", () => {
    const request = buildSampleRequest({
      scope: OTHER,
      nodeLabels: null,
      shape: fallbackShape(),
      nodeBudget: 50,
    });
    expectWellFormed(request);
    const json = JSON.stringify(request.query);
    expect(json).not.toContain(ALL_NODES);
    expect(json).not.toContain(ALL_EDGES);
    expect(request.parameters).toEqual({ no_ids: [] });
    expect(json).toContain('"value_map"');
  });

  it("drops edges whose endpoints are missing", () => {
    const body = {
      nodes: [{ $id: 1, $label: "A" }],
      edges: [
        { $id: 3, $label: "R", $from: 1, $to: 2 },
        { $id: 4, $label: "R", $from: 1, $to: 1 },
      ],
    };
    const sample = parseSample(body, SHAPE);
    expect(sample.edges.map((edge) => edge.id)).toEqual(["e:4"]);
    expect(sample.truncated).toBe(true);
  });
});

describe("allocateSample", () => {
  const counts = (entries: [string, number | null][]): LabelCount[] =>
    entries.map(([label, count]) => ({ label, count }));

  it("splits evenly and hands small labels' leftovers to large ones", () => {
    const allocation = allocateSample(
      counts([
        ["A", 2],
        ["B", 1000],
        ["C", 1000],
      ]),
      30,
    );
    expect(Object.fromEntries(allocation)).toEqual({ A: 2, B: 14, C: 14 });
  });

  it("treats a label without a count as unknown, not huge: after every counted label, with the minimum", () => {
    const allocation = allocateSample(
      counts([
        ["Gone", null],
        ["A", 2],
        ["B", 1000],
      ]),
      30,
    );
    expect(Object.fromEntries(allocation)).toEqual({
      A: 2,
      B: 25,
      Gone: EXPLORER_LIMITS.sampleMinPerLabel,
    });
    // With room for only some labels, counted ones win over unknown ones of any name.
    const tight = allocateSample(
      counts([
        ["Aaa", null],
        ["Zzz", 5],
        ["Yyy", 4],
      ]),
      2 * EXPLORER_LIMITS.sampleMinPerLabel,
    );
    expect([...tight.keys()].sort()).toEqual(["Yyy", "Zzz"]);
  });

  it("skips empty labels and keeps the largest when there are too many", () => {
    const labels = counts(
      Array.from(
        { length: 20 },
        (_, index) => [`L${index}`, index] as [string, number],
      ),
    );
    const allocation = allocateSample(labels, 12);
    expect(allocation.has("L0")).toBe(false);
    expect([...allocation.keys()].sort()).toEqual(["L16", "L17", "L18", "L19"]);
    expect(
      [...allocation.values()].every(
        (take) => take >= EXPLORER_LIMITS.sampleMinPerLabel,
      ),
    ).toBe(true);
  });
});

describe("label pages", () => {
  it("parses a captured first page", () => {
    const page = parseLabelPage(
      labelPageFixture.response,
      shapeOf(labelPageFixture),
      3,
    );
    expect(page.nodes.map((node) => node.label)).toEqual(["Tag", "Tag", "Tag"]);
    expect(page.total).toBe(80);
    expect(page.nextAfter).toBe(page.nodes[2].dbId);
  });

  it("asks for one extra row and counts (from the bitmap) only on the first page", () => {
    const first = buildLabelPageRequest({
      scope: OTHER,
      label: "User",
      afterDbId: null,
      pageSize: 10,
      shape: SHAPE,
    });
    expectWellFormed(first);
    expect(first.parameters).toEqual({});
    expect(JSON.stringify(first.query)).toContain(
      '"right":{"constant":{"string":"User"}}',
    );
    expect(
      (first.query as { read: { returns: string[] } }).read.returns,
    ).toEqual(["page", "total"]);
    expect(JSON.stringify(first.query)).toContain('"count":{"literal":11}');
    expect(JSON.stringify(first.query)).not.toContain('"skip"');
    const next = buildLabelPageRequest({
      scope: OTHER,
      label: "User",
      afterDbId: 41,
      offset: 20,
      pageSize: 10,
      shape: SHAPE,
    });
    expectWellFormed(next);
    expect(
      (next.query as { read: { returns: string[] } }).read.returns,
    ).toEqual(["page"]);
    // Later pages skip what was paged past; no filter sits in front of the limit.
    const json = JSON.stringify(next.query);
    expect(json).toContain('"skip":{"input":{"nodes_where"');
    expect(json).toContain('"count":{"literal":20}');
    expect(json).not.toContain('"where"');
    // Label-scoped keys: its own light keys plus the shared ones.
    expect(json).toContain(
      '"properties":["$id","$label","foundry_load","name","email","title"]',
    );
  });

  it("reports no next page on the last one, and never repeats a node", () => {
    const rows = [
      { $id: 5, $label: "User" },
      { $id: 9, $label: "User" },
    ];
    expect(parseLabelPage({ page: rows }, SHAPE, 2)).toEqual({
      nodes: expect.any(Array),
      nextAfter: null,
      total: null,
      consumed: 2,
    });
    expect(
      parseLabelPage({ page: [...rows, { $id: 12, $label: "User" }] }, SHAPE, 2)
        .nextAfter,
    ).toBe(9);
    // Rows at or below the previous page's last id are dropped (a shifted offset).
    const shifted = parseLabelPage(
      { page: [{ $id: 4, $label: "User" }, ...rows] },
      SHAPE,
      2,
      4,
    );
    expect(shifted.nodes.map((node) => node.dbId)).toEqual([5]);
    expect(shifted.nextAfter).toBe(5);
    expect(shifted.consumed).toBe(2);
  });
});

describe("neighborhood", () => {
  it("parses a captured supernode expansion", () => {
    const shape = shapeOf(supernodeFixture);
    const hood = parseNeighborhood(supernodeFixture.response, 897, shape, {
      limit: 3,
    });
    expect(hood.center).toMatchObject({
      dbId: 897,
      label: "Tag",
      properties: { name: "general" },
    });
    expect(hood.edges).toHaveLength(3);
    expect(hood.truncated).toBe(true);
    expect(hood.degree).toBeNull();
    // The extra (limit + 1) edge's node is dropped with it.
    const sources = new Set(hood.edges.map((edge) => edge.source));
    expect(hood.nodes.every((node) => sources.has(node.id))).toBe(true);
    expect(hood.nodes).toHaveLength(3);
  });

  it("keeps a self-loop once and leaves the center out of the neighbors", () => {
    const hood = parseNeighborhood(
      selfLoopFixture.response,
      97,
      shapeOf(selfLoopFixture),
    );
    expect(
      hood.edges.filter(
        (edge) => edge.source === "n:97" && edge.target === "n:97",
      ),
    ).toHaveLength(1);
    expect(hood.nodes.some((node) => node.id === "n:97")).toBe(false);
    expect(new Set(hood.edges.map((edge) => edge.id)).size).toBe(
      hood.edges.length,
    );
  });

  it("returns an empty result for a missing node", () => {
    expect(
      parseNeighborhood(missingFixture.response, 5, shapeOf(missingFixture)),
    ).toEqual({
      center: null,
      nodes: [],
      edges: [],
      degree: null,
      truncated: false,
    });
    expect(parseNeighborhood(undefined, 5, SHAPE).center).toBeNull();
  });

  it("narrows by direction and edge label, and pages by edge id", () => {
    const request = buildNeighborhoodRequest({
      dbId: 7,
      direction: "out",
      edgeLabel: "FOLLOWS",
      afterEdgeDbId: 120,
      limit: 10,
      shape: SHAPE,
    });
    expectWellFormed(request);
    expect(request.parameters).toEqual({ node_id: 7, after: 120 });
    // A later page doesn't count the degree again (the first page did).
    const returns = (request.query as { read: { returns: string[] } }).read
      .returns;
    expect(returns).toEqual(["node", "out_edges", "out_nodes"]);
    expect(JSON.stringify(request.query)).toContain(
      '"out_e":{"input":{"nodes":{"reference":{"var":"center"}}},"label":"FOLLOWS"}',
    );
    expect(JSON.stringify(request.query)).toContain('"count":{"literal":11}');
    expect(
      parseNeighborhood({ node: [{ $id: 7, $label: "A" }] }, 7, SHAPE).degree,
    ).toBeNull();
  });

  it("never counts the degree: that would read every edge of the node again", () => {
    const request = buildNeighborhoodRequest({ dbId: 7, shape: SHAPE });
    expectWellFormed(request);
    expect(request.parameters).toEqual({ node_id: 7 });
    const returns = (request.query as { read: { returns: string[] } }).read
      .returns;
    expect(returns).toEqual([
      "node",
      "out_edges",
      "out_nodes",
      "in_edges",
      "in_nodes",
    ]);
    expect(JSON.stringify(request.query)).not.toContain("group_count");
    // No cursor filter on the first page.
    expect(JSON.stringify(request.query)).not.toContain('"where"');
  });
});

describe("nextEdgeCursor", () => {
  it("continues one direction from its largest edge id", () => {
    const shape = shapeOf(supernodeFixture);
    const hood = parseNeighborhood(supernodeFixture.response, 897, shape, {
      limit: 3,
    });
    expect(nextEdgeCursor(hood, "in")).toBe(
      Math.max(...hood.edges.map((edge) => edge.dbId)),
    );
    expect(nextEdgeCursor(hood, "out")).toBeNull();
    expect(nextEdgeCursor({ ...hood, truncated: false }, "in")).toBeNull();
    expect(
      nextEdgeCursor(
        parseNeighborhood(missingFixture.response, 5, shape),
        "in",
      ),
    ).toBeNull();
  });
});

describe("connect", () => {
  it("parses captured edges in both directions, deduplicated", () => {
    const edges = parseConnect(connectFixture.response);
    expect(edges.length).toBeGreaterThan(0);
    expect(new Set(edges.map((edge) => edge.id)).size).toBe(edges.length);
  });

  it("includes the new ids among the visible ones and drops invalid ids", () => {
    const request = buildConnectRequest({
      newDbIds: [3, 3, -1, 1.5],
      visibleDbIds: [1, 2],
      edgeLimit: 5,
    });
    expect(request.parameters).toEqual({
      new_ids: [3],
      visible_ids: [1, 2, 3],
    });
    expect(request.parameter_types).toEqual({
      new_ids: { array: "i64" },
      visible_ids: { array: "i64" },
    });
    expect(JSON.stringify(request.query)).toContain('"count":{"literal":5}');
  });
});

describe("nodes by id", () => {
  it("parses a captured full row", () => {
    const [node] = parseNodes(nodesFullFixture.response, fallbackShape());
    expect(node.complete).toBe(true);
    expect(node.properties.embedding).toHaveLength(384);
    expect(typeof node.properties.body).toBe("string");
  });

  it("fetches the shape's keys, or everything without one", () => {
    expect(
      JSON.stringify(buildNodesByIdRequest({ dbIds: [1], shape: SHAPE }).query),
    ).toContain('"values"');
    expect(
      JSON.stringify(
        buildNodesByIdRequest({ dbIds: [1], shape: fallbackShape() }).query,
      ),
    ).toContain('"value_map"');
  });
});

describe("search", () => {
  const args = {
    ...(searchFixture.args as Omit<
      Parameters<typeof buildSearchRequest>[0],
      "scope"
    >),
    scope: OTHER,
  };

  it("parses captured hits in priority order without duplicates", () => {
    const hits = parseSearch(searchFixture.response, args.shape, args);
    expect(hits[0]).toMatchObject({ via: "id", node: { dbId: 977 } });
    const text = hits.filter((hit) => hit.via === "text");
    expect(text.length).toBeGreaterThan(0);
    expect(
      text.every(
        (hit) => hit.matchedKey === "title" && typeof hit.score === "number",
      ),
    ).toBe(true);
    const scores = text.map((hit) => hit.score!);
    expect([...scores].sort((a, b) => b - a)).toEqual(scores);
    expect(new Set(hits.map((hit) => hit.node.id)).size).toBe(hits.length);
    // The id hit is also a text match; it stays an id hit.
    expect(hits.filter((hit) => hit.node.dbId === 977)).toHaveLength(1);
  });

  it("returns null when there is nothing to run", () => {
    const base = { textIndexes: [], containsTargets: [], shape: SHAPE };
    expect(
      buildSearchRequest({
        scope: OTHER,
        ...base,
        text: "x",
        idCandidate: null,
      }),
    ).toBeNull();
    expect(
      buildSearchRequest({
        scope: OTHER,
        ...base,
        text: "  ",
        idCandidate: null,
        textIndexes: [{ label: "A", property: "b" }],
        containsTargets: [{ label: "A", keys: ["b"] }],
      }),
    ).toBeNull();
    expect(
      buildSearchRequest({
        scope: OTHER,
        ...base,
        text: "x",
        idCandidate: null,
        containsTargets: [{ label: "A", keys: [] }],
      }),
    ).toBeNull();
  });

  it("runs only the id lookup for a bare number with no text sources", () => {
    const request = buildSearchRequest({
      scope: OTHER,
      text: "42",
      idCandidate: 42,
      textIndexes: [],
      containsTargets: [],
      shape: SHAPE,
    })!;
    expectWellFormed(request);
    expect(request.parameters).toEqual({ id: 42 });
  });

  it("projects $score with the shape keys and full rows without one", () => {
    const opts = {
      text: "hi",
      idCandidate: null,
      textIndexes: [{ label: "Doc", property: "title" }],
      containsTargets: [],
    };
    expect(
      JSON.stringify(
        buildSearchRequest({ scope: OTHER, ...opts, shape: SHAPE })!.query,
      ),
    ).toContain(
      '"properties":["$id","$label","foundry_load","$score","title","name"]',
    );
    expect(
      JSON.stringify(
        buildSearchRequest({ scope: OTHER, ...opts, shape: fallbackShape() })!
          .query,
      ),
    ).toContain('"value_map"');
  });

  it("leaves a heavy indexed property out of text hits and fetches the label's own keys", () => {
    const shape: ProbedFetchShape = {
      keys: ["name", "title"],
      heavyKeys: ["body"],
      heavyByLabel: { Document: ["body"] },
      keysByLabel: { Document: ["title", "lang"] },
    };
    const request = buildSearchRequest({
      scope: OTHER,
      text: "graph",
      idCandidate: null,
      textIndexes: [
        { label: "Document", property: "body" },
        { label: "Document", property: "title" },
      ],
      containsTargets: [],
      shape,
    })!;
    const json = JSON.stringify(request.query);
    expect(json).toContain('"property":"body"');
    expect(json).toContain(
      '"properties":["$id","$label","foundry_load","$score","title","lang","name"]',
    );
    expect(
      json
        .match(/"properties":\[[^\]]*\]/g)
        ?.some((list) => list.includes('"body"')),
    ).toBe(false);
    // A text hit lists the heavy body as not loaded, so the inspector can fetch it.
    const [hit] = parseSearch(
      { text_0: [{ $id: 4, $label: "Document", $score: 2, title: "Graphs" }] },
      shape,
      {
        text: "graph",
        textIndexes: [{ label: "Document", property: "body" }],
      },
    );
    expect(hit).toMatchObject({
      via: "text",
      matchedKey: "body",
      node: { omitted: ["body"] },
    });
  });

  it("names a request with only contains scans, so the cheap sources can run apart from it", () => {
    const base = { text: "Ada", textIndexes: [], shape: SHAPE };
    const scan = buildSearchRequest({
      scope: OTHER,
      ...base,
      idCandidate: null,
      containsTargets: [{ label: "User", keys: ["name"] }],
    })!;
    expect(scan.query_name).toBe("explorer_search_contains");
    expectWellFormed(scan);
    const indexed = buildSearchRequest({
      scope: OTHER,
      ...base,
      idCandidate: 7,
      textIndexes: [{ label: "User", property: "name" }],
      containsTargets: [],
    })!;
    expect(indexed.query_name).toBe("explorer_search");
    expect(JSON.stringify(indexed.query)).not.toContain('"contains"');
  });

  it("dedupes in id > text > contains order and fills matchedKey", () => {
    const body = {
      by_id: [{ $id: 1, $label: "A", name: "one" }],
      text_0: [
        { $id: 2, $label: "A", $score: 1, title: "beta" },
        { $id: 1, $label: "A", $score: 9, title: "alpha" },
      ],
      text_1: [{ $id: 3, $label: "B", $score: 5, name: "gamma" }],
      contains_0: [
        { $id: 3, $label: "B", name: "gamma" },
        { $id: 4, $label: "B", name: "alphabet" },
      ],
      contains_10: [{ $id: 5, $label: "C", note: "alpha" }],
      contains_2: [{ $id: 6, $label: "C" }],
    };
    const hits = parseSearch(body, SHAPE, {
      text: "alpha",
      textIndexes: [
        { label: "A", property: "title" },
        { label: "B", property: "name" },
      ],
    });
    expect(
      hits.map((hit) => [hit.node.dbId, hit.via, hit.matchedKey, hit.score]),
    ).toEqual([
      [1, "id", undefined, undefined],
      [3, "text", "name", 5],
      [2, "text", "title", 1],
      [4, "contains", "name", undefined],
      [6, "contains", undefined, undefined],
      [5, "contains", "note", undefined],
    ]);
  });

  it("reports the searched key a contains hit matched", () => {
    const body = {
      contains_0: [{ $id: 4, $label: "B", alias: "alphabet", name: "alpha" }],
    };
    expect(parseSearch(body, SHAPE, { text: "alpha" })[0].matchedKey).toBe(
      "alias",
    );
    expect(
      parseSearch(body, SHAPE, {
        text: "alpha",
        containsTargets: [{ label: "B", keys: ["name"] }],
      })[0].matchedKey,
    ).toBe("name");
  });
});

describe("edge fetch shape", () => {
  const vector = Array.from({ length: 64 }, (_, index) => index / 64);
  const body = {
    node_0: [{ $id: 1, $label: "Doc", title: "a" }],
    edge_0: [
      {
        $id: 9,
        $label: "SIMILAR",
        $from: 1,
        $to: 2,
        score: 0.5,
        embedding: vector,
      },
    ],
    edge_1: [
      { $id: 10, $label: "CITES", $from: 1, $to: 3, since: 2020, note: "x" },
    ],
  };
  const labels = { nodes: ["Doc"], edges: ["SIMILAR", "CITES"] };
  const shape = parseSchemaProbe(body, labels).shape as ProbedFetchShape;
  // Every row carries its load, so nodes and edges outside the scope are told apart.
  const edgeProjection =
    '"properties":["$id","$label","$from","$to","foundry_load"';
  const neighborhood = (edgeShape: NodeFetchShape) =>
    JSON.stringify(
      buildNeighborhoodRequest({ dbId: 1, shape: edgeShape }).query,
    );

  it("projects light edge keys with endpoints once an edge label carries heavy values", () => {
    expect(shape.edgeKeys?.sort()).toEqual(["note", "score", "since"]);
    expect(fetchesEdgeProperties(shape)).toBe(true);
    const requests = [
      buildSampleRequest({
        scope: OTHER,
        nodeLabels: [{ label: "Doc", count: 3 }],
        shape,
      }),
      buildSampleRequest({
        scope: OTHER,
        nodeLabels: [{ label: "Doc", count: 10_000 }],
        shape,
      }),
      buildNeighborhoodRequest({ dbId: 1, shape }),
      buildConnectRequest({ newDbIds: [1], visibleDbIds: [2], shape }),
    ];
    for (const request of requests) {
      expectWellFormed(request);
      const json = JSON.stringify(request.query);
      expect(json).not.toContain("edge_properties");
      expect(json).not.toContain('"embedding"');
      expect(json).toContain(edgeProjection);
    }
    // values rows carry $from/$to, so they parse as edges with their light properties.
    const [edge] = parseConnect({
      from_new: [{ $id: 9, $label: "SIMILAR", $from: 1, $to: 2, score: 0.5 }],
      into_new: [],
    });
    expect(edge).toMatchObject({
      id: "e:9",
      source: "n:1",
      target: "n:2",
      properties: { score: 0.5 },
    });
  });

  it("keeps every edge property when no edge value is heavy", () => {
    const light = parseSchemaProbe(
      { ...body, edge_0: [] },
      { nodes: ["Doc"], edges: ["CITES"] },
    ).shape as ProbedFetchShape;
    expect(light.edgeKeys).toBeUndefined();
    expect(neighborhood(light)).toContain("edge_properties");
    expect(
      JSON.stringify(
        buildConnectRequest({ newDbIds: [1], visibleDbIds: [] }).query,
      ),
    ).toContain("edge_properties");
    // Growing the shape from full nodes keeps the edge keys.
    const grown = extendShape(shape, [
      {
        id: "n:1",
        dbId: 1,
        label: "Doc",
        properties: { title: "a", extra: 1 },
        omitted: [],
        complete: true,
      },
    ]) as ProbedFetchShape;
    expect(grown.edgeKeys).toEqual(shape.edgeKeys);
  });

  it("drops edge properties for the too-large retries", () => {
    const edgesOnly = withoutEdgeProperties(shape);
    expect(fetchesEdgeProperties(edgesOnly)).toBe(false);
    expect(edgesOnly.keys).toEqual(shape.keys);
    expect(neighborhood(edgesOnly)).toContain(`${edgeProjection}]`);
    expect(fetchesEdgeProperties(withoutEdgeProperties(fallbackShape()))).toBe(
      false,
    );

    const bare = lightShape(shape);
    expect(isLightShape(bare)).toBe(true);
    expect(isLightShape(shape)).toBe(false);
    expect(isLightShape(fallbackShape())).toBe(false);
    const sample = JSON.stringify(
      buildSampleRequest({
        scope: OTHER,
        nodeLabels: [{ label: "Doc", count: 3 }],
        shape: bare,
      }).query,
    );
    expect(sample).toContain('"properties":["$id","$label","foundry_load"]');
    expect(sample).toContain(`${edgeProjection}]`);
    // Nodes loaded this way list their label's keys as not loaded.
    expect(
      parseSample({ nodes: [{ $id: 1, $label: "Doc" }], edges: [] }, bare)
        .nodes[0].omitted,
    ).toEqual(["title"]);
  });
});

describe("extendShape", () => {
  const full = (
    label: string,
    properties: GraphNode["properties"],
  ): GraphNode => ({
    id: "n:1",
    dbId: 1,
    label,
    properties,
    omitted: [],
    complete: true,
  });

  it("learns sparse keys and newly heavy values from full nodes", () => {
    const next = extendShape(SHAPE, [
      full("User", { name: "a", nickname: "b", avatar: "x".repeat(5000) }),
    ]) as ProbedFetchShape;
    expect(next.keys).toEqual(["name", "title", "nickname"]);
    expect(next.heavyKeys).toEqual(["avatar", "body", "embedding"]);
    expect(next.heavyByLabel?.User).toEqual(["avatar"]);
    expect(next.keysByLabel?.User).toEqual(["name", "email", "nickname"]);
  });

  it("returns the same shape when nothing is new", () => {
    expect(extendShape(SHAPE, [full("User", { name: "a", email: "e" })])).toBe(
      SHAPE,
    );
    expect(
      extendShape(SHAPE, [{ ...full("User", { other: 1 }), complete: false }]),
    ).toBe(SHAPE);
    const fallback = fallbackShape();
    expect(extendShape(fallback, [full("User", { other: 1 })])).toBe(fallback);
  });
});

describe("degree probe", () => {
  it("counts neighbors per side through out/in with a literal bound, reading no edges", () => {
    const request = buildDegreeProbeRequest({ dbIds: [5, 6, 5, -1] });
    expectWellFormed(request);
    expect(request.query_name).toBe("explorer_degree_probe");
    expect(request.parameters).toEqual({});
    const json = JSON.stringify(request.query);
    // Node to node (one adjacency key per node), never node to edges.
    expect(json).not.toMatch(/"(out_e|in_e|group_count|edge_properties)"/);
    const cap = EXPLORER_LIMITS.degreeThreshold + 1;
    expect(json).toContain(
      `{"count":{"input":{"limit":{"input":{"out":{"input":{"nodes":{"reference":{"ids":[5]}}}}},"count":{"literal":${cap}}}}}}`,
    );
    expect(json).toContain(
      `{"count":{"input":{"limit":{"input":{"in":{"input":{"nodes":{"reference":{"ids":[6]}}}}},"count":{"literal":${cap}}}}}}`,
    );
    expect(
      (request.query as { read: { returns: string[] } }).read.returns,
    ).toEqual(["probe_out_0", "probe_in_0", "probe_out_1", "probe_in_1"]);
  });

  it("probes at most probeBatch nodes per request", () => {
    const ids = Array.from(
      { length: EXPLORER_LIMITS.probeBatch + 10 },
      (_, index) => index + 1,
    );
    const returns = (
      buildDegreeProbeRequest({ dbIds: ids }).query as {
        read: { returns: string[] };
      }
    ).read.returns;
    expect(returns).toHaveLength(EXPLORER_LIMITS.probeBatch * 2);
  });

  it("parses probes and leaves nodes the response didn't answer unknown", () => {
    const probes = parseDegreeProbe(
      { probe_out_0: 3, probe_in_0: 1001, probe_out_1: "x" },
      [5, 6],
    );
    expect(probes.get(5)).toEqual({ out: 3, in: 1001 });
    expect(probes.has(6)).toBe(false);
    expect(isLargeProbe({ out: 3, in: 1001 })).toBe(true);
    expect(isLargeProbe({ out: 3, in: 1001 }, "out")).toBe(false);
    expect(isLargeProbe({ out: 3, in: 1001 }, "in")).toBe(true);
    expect(isLargeProbe({ out: 1000, in: 1000 })).toBe(false);
    // Few neighbors but more edges than the threshold (parallel edges) is large too.
    expect(
      isLargeProbe({ out: 2, in: 2, edges: { out: 1001, in: 2 } }, "out"),
    ).toBe(true);
    expect(
      isLargeProbe({ out: 2, in: 2, edges: { out: 1001, in: 2 } }, "in"),
    ).toBe(false);
    expect(
      isLargeProbe({ out: 2, in: 2, edges: { out: 1000, in: 1000 } }),
    ).toBe(false);
  });

  it("counts edges per side with a literal bound, reading nothing per edge", () => {
    const request = buildEdgeCountRequest({ dbIds: [5, 6, 5, -1] });
    expectWellFormed(request);
    expect(request.query_name).toBe("explorer_edge_count");
    expect(request.parameters).toEqual({});
    const json = JSON.stringify(request.query);
    const cap = EXPLORER_LIMITS.degreeThreshold + 1;
    expect(json).toContain(
      `{"count":{"input":{"limit":{"input":{"out_e":{"input":{"nodes":{"reference":{"ids":[5]}}}}},"count":{"literal":${cap}}}}}}`,
    );
    expect(json).toContain(
      `{"count":{"input":{"limit":{"input":{"in_e":{"input":{"nodes":{"reference":{"ids":[6]}}}}},"count":{"literal":${cap}}}}}}`,
    );
    // No filter (it would read each edge's endpoints or record) and no projection of edges.
    expect(json).not.toMatch(/"(where|edge_properties|values|group_count)"/);
    expect(
      (request.query as { read: { returns: string[] } }).read.returns,
    ).toEqual(["edges_out_0", "edges_in_0", "edges_out_1", "edges_in_1"]);
    const many = Array.from(
      { length: EXPLORER_LIMITS.edgeCountBatch + 5 },
      (_, index) => index + 1,
    );
    expect(
      (
        buildEdgeCountRequest({ dbIds: many }).query as {
          read: { returns: string[] };
        }
      ).read.returns,
    ).toHaveLength(EXPLORER_LIMITS.edgeCountBatch * 2);
    const counts = parseEdgeCount(
      { edges_out_0: 1001, edges_in_0: 4, edges_out_1: null },
      [5, 6],
    );
    expect(counts.get(5)).toEqual({ out: 1001, in: 4 });
    expect(counts.has(6)).toBe(false);
  });

  it("rides along on a nodes request when asked, for the first probeBatch ids", () => {
    const request = buildNodesByIdRequest({
      dbIds: [9, 10],
      shape: SHAPE,
      probeDegree: true,
    });
    expectWellFormed(request);
    expect(request.parameters).toEqual({ ids: [9, 10] });
    expect(
      (request.query as { read: { returns: string[] } }).read.returns,
    ).toEqual([
      "nodes",
      "probe_out_0",
      "probe_in_0",
      "probe_out_1",
      "probe_in_1",
    ]);
    const plain = buildNodesByIdRequest({ dbIds: [9], shape: SHAPE });
    expect(
      (plain.query as { read: { returns: string[] } }).read.returns,
    ).toEqual(["nodes"]);
  });
});

describe("degree", () => {
  it("counts at most degreeThreshold + 1 edges per side on its own, and every edge only when asked", () => {
    const cap = EXPLORER_LIMITS.degreeThreshold + 1;
    const bounded = buildDegreeRequest({ dbId: 7, sides: ["out"] });
    expectWellFormed(bounded);
    expect(bounded.query_name).toBe("explorer_degree");
    expect(
      (bounded.query as { read: { returns: string[] } }).read.returns,
    ).toEqual(["out_by_label"]);
    expect(JSON.stringify(bounded.query)).toContain(
      `"group_count":{"input":{"limit":{"input":{"out_e":{"input":{"nodes":{"reference":{"var":"center"}}}}},"count":{"literal":${cap}}}}`,
    );
    const full = buildDegreeRequest({ dbId: 7, bounded: false });
    expectWellFormed(full);
    expect(full.query_name).toBe("explorer_degree_full");
    expect(JSON.stringify(full.query)).not.toContain('"limit"');
    expect(
      (full.query as { read: { returns: string[] } }).read.returns,
    ).toEqual(["out_by_label", "in_by_label"]);
  });

  it("makes a side a floor when the bounded count reaches the cap or the probe found it large", () => {
    const threshold = EXPLORER_LIMITS.degreeThreshold;
    const capped = parseDegree(
      {
        out_by_label: [
          { $label: "A", count: 600 },
          { $label: "B", count: 401 },
        ],
      },
      { sides: ["out"], probe: { out: 900, in: 0 } },
    );
    // 1,001 edges among 900 neighbors (parallel edges): more than the explorer counts on its own.
    expect(capped).toEqual({
      outDegree: threshold,
      inDegree: 0,
      outByLabel: [],
      inByLabel: [],
      atLeast: { out: true, in: false },
    });
    const hub = parseDegree(
      { out_by_label: [{ $label: "A", count: 5 }] },
      { sides: ["out"], probe: { out: 5, in: threshold + 1 } },
    );
    expect(hub).toEqual({
      outDegree: 5,
      inDegree: threshold,
      outByLabel: [{ label: "A", count: 5 }],
      inByLabel: [],
      atLeast: { out: false, in: true },
    });
    // The explicit count is exact whatever the size.
    expect(
      parseDegree(
        {
          in_by_label: [{ $label: "VIEWED", count: 67_506 }],
          out_by_label: [],
        },
        { bounded: false },
      ),
    ).toEqual({
      outDegree: 0,
      inDegree: 67_506,
      outByLabel: [],
      inByLabel: [{ label: "VIEWED", count: 67_506 }],
    });
  });

  it("counts edges per direction and label without fetching rows", () => {
    const request = buildDegreeRequest({ dbId: 897 });
    expect(request.parameters).toEqual({ node_id: 897 });
    expect(request.parameter_types).toEqual({ node_id: "i64" });
    const read = (request.query as { read: { returns: string[] } }).read;
    // One pass over the node's edges per side: the per-label counts add up to the degree.
    expect(read.returns).toEqual(["out_by_label", "in_by_label"]);
    expect(JSON.stringify(request.query)).not.toMatch(
      /value_map|values|edge_properties/,
    );
    expect(
      parseDegree({
        in_by_label: [
          { $label: "A", count: 3 },
          { $label: "B", count: 4 },
        ],
        out_by_label: [],
      }),
    ).toEqual({
      outDegree: 0,
      inDegree: 7,
      outByLabel: [],
      inByLabel: [
        { label: "A", count: 3 },
        { label: "B", count: 4 },
      ],
    });
  });

  it("parses the live response shape", () => {
    // Captured from the local seed: the Tag "general" supernode.
    const body = {
      in_by_label: [{ $label: "TAGGED", count: 630 }],
      in_degree: 630,
      out_by_label: [],
      out_degree: 0,
    };
    expect(parseDegree(body)).toEqual({
      outDegree: 0,
      inDegree: 630,
      outByLabel: [],
      inByLabel: [{ label: "TAGGED", count: 630 }],
    });
  });
});

describe("every builder", () => {
  it("sends typed parameters and explorer_ names", () => {
    const requests = [
      buildLabelScanRequest(),
      buildLabelDiscoveryRequest(),
      buildLabelCountsRequest({ nodes: ["A"], edges: ["R"] }, { scope: OTHER }),
      buildLabelCountsRequest(
        { nodes: ["A"], edges: ["R"] },
        { scope: OTHER, probeRows: 2 },
      ),
      buildSchemaProbeRequest({ nodes: ["A"], edges: ["R"] }, { scope: OTHER }),
      buildSampleRequest({
        scope: OTHER,
        nodeLabels: [{ label: "A", count: 10_000 }],
        shape: SHAPE,
      }),
      buildSampleRequest({ scope: OTHER, nodeLabels: [], shape: SHAPE }),
      buildLabelPageRequest({
        scope: OTHER,
        label: "A",
        afterDbId: null,
        shape: fallbackShape(),
      }),
      buildNeighborhoodRequest({ dbId: 1, shape: SHAPE }),
      buildConnectRequest({ newDbIds: [1], visibleDbIds: [] }),
      buildNodesByIdRequest({ dbIds: [], shape: SHAPE }),
      buildDegreeRequest({ dbId: 7 }),
      buildDegreeRequest({ dbId: 7, bounded: false }),
      buildDegreeProbeRequest({ dbIds: [7, 8] }),
      buildNodesByIdRequest({ dbIds: [7], shape: SHAPE, probeDegree: true }),
      buildSearchRequest({
        scope: OTHER,
        text: "q",
        idCandidate: 3,
        textIndexes: [{ label: "A", property: "t" }],
        containsTargets: [{ label: "A", keys: ["t", "u"] }],
        shape: SHAPE,
      })!,
      buildSearchRequest({
        scope: OTHER,
        text: "q",
        idCandidate: null,
        textIndexes: [],
        containsTargets: [{ label: "A", keys: ["t"] }],
        shape: SHAPE,
      })!,
    ];
    for (const request of requests) expectWellFormed(request);
    // Only the explicit label scan reads the whole graph.
    expect(
      requests.filter(scansWholeGraph).map((request) => request.query_name),
    ).toEqual(["explorer_label_scan"]);
    expect(new Set(requests.map((request) => request.query_name))).toEqual(
      new Set([
        "explorer_label_scan",
        "explorer_label_discovery",
        "explorer_label_counts",
        "explorer_labels",
        "explorer_schema_probe",
        "explorer_sample",
        "explorer_label_page",
        "explorer_neighborhood",
        "explorer_connect",
        "explorer_nodes",
        "explorer_search",
        "explorer_search_contains",
        "explorer_degree",
        "explorer_degree_full",
        "explorer_degree_probe",
      ]),
    );
    // Labels are constants everywhere (the label bitmap then serves every engine); no label is a parameter.
    for (const request of requests) {
      expect(
        JSON.stringify(request.query),
        request.query_name ?? "",
      ).not.toMatch(/"\$label"\},"right":\{"param"/);
    }
  });

  it("parsers never throw on unexpected bodies", () => {
    const bodies = [
      null,
      undefined,
      0,
      "text",
      [],
      {},
      { nodes: "x", edges: [1, null, {}], page: {}, node: 5 },
    ];
    for (const body of bodies) {
      expect(() => {
        parseLabelScan(body);
        parseLabelDiscovery(body);
        parseLabelCounts(body, { nodes: ["A"], edges: ["R"] }, OTHER);
        parseSchemaProbe(body, { nodes: ["A"], edges: ["R"] });
        parseSample(body, SHAPE);
        parseLabelPage(body, SHAPE);
        parseNeighborhood(body, 1, SHAPE);
        parseConnect(body);
        parseNodes(body, SHAPE);
        parseSearch(body, SHAPE);
        parseDegree(body);
      }).not.toThrow();
    }
  });
});
