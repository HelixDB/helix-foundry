import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { FOUNDRY_EDGES, FOUNDRY_NODES } from "@/shared/foundry-labels";

import { extractDocument, ingestChunk } from "../../../mock/graph/extract";
import { scopeOf } from "../../../mock/graph/scope";
import { runQuery } from "../../../mock/graph/query";
import { MockGraph } from "../../../mock/graph/store";
import type { Scope } from "@/web/areas/explore/load-scope";
import {
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
  buildSearchRequest,
  isLargeProbe,
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
} from "@/web/areas/explore/explorer/lib/queries";
import {
  shownLabels,
  shownStats,
} from "@/web/areas/explore/explorer/lib/exclusion";
import { menuRelationships } from "@/web/areas/explore/explorer/lib/graph-store";

// The explorer's own requests, from its builders, run through the mock's v3
// interpreter over a database a load wrote (Stripe customers, a contract):
// what the e2e suite's explorer sees, and proof the mock answers every
// operation the explorer sends.

function database() {
  const g = new MockGraph();
  ingestChunk(
    g.view("load_1"),
    {
      source: "src_c",
      mapping: "customers",
      snapshot: "s",
      ontologyVersion: "v",
    },
    [
      ["cus_GLOBEX01", "Globex Corporation"],
      ["cus_INITECH2", "Initech LLC"],
    ].map(([key, name]) => ({
      type: "Customer",
      key: key!,
      name: name!,
      props: { customer_id: key!, company_name: name! },
      names: ["company_name"],
    })),
  );
  extractDocument(
    g.view("load_1"),
    {
      id: "doc_1",
      name: "contract.md",
      text: readFileSync(
        new URL("../../../mock/fixtures/contract.md", import.meta.url),
        "utf8",
      ),
      markdown: true,
      ontologyVersion: "v",
    },
    {
      types: new Set(["Contract", "Customer"]),
      names: new Map([["Customer", "company_name"]]),
    },
    // What the contracts route lets the document fill: Contracts, found
    // customers and signed_with.
    scopeOf({ Contract: { create: true }, Customer: {} }, ["signed_with"]),
  );
  return g;
}
// Every read is the explorer's in the load the fixture wrote.
const LOAD: Scope = { kind: "load", id: "load_1" };

const run = (g: MockGraph, request: unknown) => {
  const answer = runQuery(g, request);
  expect(answer.status, JSON.stringify(answer.body)).toBe(200);
  return answer.body;
};

describe("the explorer over the mock database", () => {
  it("boots: discovers labels, counts and probes them, then samples", () => {
    const g = database();
    const discovery = parseLabelDiscovery(run(g, buildLabelDiscoveryRequest()));
    expect(discovery.empty).toBe(false);
    expect(discovery.nodes).toEqual(
      expect.arrayContaining([
        "Customer",
        "Contract",
        FOUNDRY_NODES.Document,
        FOUNDRY_NODES.Section,
        FOUNDRY_NODES.Mention,
        FOUNDRY_NODES.Review,
      ]),
    );
    expect(discovery.edges).toEqual(
      expect.arrayContaining(["signed_with", FOUNDRY_EDGES.MENTION_OF]),
    );
    expect(discovery.nodesExhausted).toBe(true);

    const labels = { nodes: discovery.nodes, edges: discovery.edges };
    const countsBody = run(
      g,
      buildLabelCountsRequest(labels, { scope: LOAD, probeRows: 2 }),
    );
    const stats = parseLabelCounts(countsBody, labels, LOAD, {
      nodes: true,
      edges: true,
    });
    expect(stats.nodeLabels.find((l) => l.label === "Customer")?.count).toBe(2);
    expect(stats.edgeLabels.find((l) => l.label === "signed_with")?.count).toBe(
      2,
    );
    expect(stats.nodeTotal).toBe(g.nodes.size);

    const { schemas, shape } = parseSchemaProbe(countsBody, labels);
    expect(
      schemas.find((s) => s.label === "Customer")?.properties.map((p) => p.key),
    ).toEqual(
      expect.arrayContaining([
        "entity_key",
        "company_name",
        "company_name__nn",
      ]),
    );
    const sample = parseSample(
      run(
        g,
        buildSampleRequest({
          scope: LOAD,
          nodeLabels: stats.nodeLabels,
          edgeLabels: stats.edgeLabels,
          shape,
        }),
      ),
      shape,
    );
    expect(sample.nodes.length).toBe(g.nodes.size);
    expect(sample.edges.length).toBe(g.edges.size);
    const globex = sample.nodes.find(
      (n) => n.properties.company_name === "Globex Corporation",
    )!;
    expect(globex.label).toBe("Customer");
  });

  it("expands, probes, counts, connects, pages, scans and searches", () => {
    const g = database();
    const shape = { keys: null, heavyKeys: [] };
    const contract = [...g.nodes.values()].find((n) => n.label === "Contract")!;
    const around = parseNeighborhood(
      run(g, buildNeighborhoodRequest({ dbId: contract.id, shape })),
      contract.id,
      shape,
    );
    expect(around.center?.label).toBe("Contract");
    expect(around.edges.filter((e) => e.label === "signed_with")).toHaveLength(
      2,
    );

    const probes = parseDegreeProbe(
      run(g, buildDegreeProbeRequest({ dbIds: [contract.id] })),
      [contract.id],
    );
    expect(probes.get(contract.id)?.out).toBe(2);
    const degree = parseDegree(
      run(g, buildDegreeRequest({ dbId: contract.id })),
      {},
    );
    expect(degree.outByLabel).toEqual([{ label: "signed_with", count: 2 }]);

    const customers = [...g.nodes.values()]
      .filter((n) => n.label === "Customer")
      .map((n) => n.id);
    const edges = parseConnect(
      run(
        g,
        buildConnectRequest({
          newDbIds: customers,
          visibleDbIds: [contract.id],
        }),
      ),
    );
    expect(edges.filter((e) => e.label === "signed_with")).toHaveLength(2);

    const page = parseLabelPage(
      run(
        g,
        buildLabelPageRequest({
          scope: LOAD,
          label: "Customer",
          afterDbId: null,
          shape,
        }),
      ),
      shape,
    );
    expect(page.nodes).toHaveLength(2);
    expect(page.total).toBe(2);

    const scan = parseLabelScan(run(g, buildLabelScanRequest()));
    expect(scan.nodeTotal).toBe(g.nodes.size);

    const nodes = parseNodes(
      run(g, buildNodesByIdRequest({ dbIds: customers, shape })),
      shape,
    );
    expect(nodes.map((n) => n.properties.company_name).sort()).toEqual([
      "Globex Corporation",
      "Initech LLC",
    ]);

    const req = buildSearchRequest({
      scope: LOAD,
      text: "Globex",
      idCandidate: null,
      textIndexes: [],
      containsTargets: [{ label: "Customer", keys: ["company_name"] }],
      shape,
    })!;
    const hits = parseSearch(run(g, req), shape);
    expect(hits.map((h) => h.node.properties.company_name)).toEqual([
      "Globex Corporation",
    ]);
  });

  it("leaves Foundry's labels out of counts, samples, pages and relationship counts", () => {
    const g = database();
    const foundry = {
      nodePrefix: "Foundry",
      edgePrefix: "FOUNDRY_",
      notice: "",
    };
    const internal = (label: string) => /^(Foundry|FOUNDRY_)/.test(label);
    const discovery = parseLabelDiscovery(run(g, buildLabelDiscoveryRequest()));
    const labels = shownLabels(discovery, foundry);
    expect(labels.nodes.some(internal)).toBe(false);
    expect(labels.edges.some(internal)).toBe(false);
    expect(labels.nodes).toEqual(
      expect.arrayContaining(["Customer", "Contract"]),
    );
    const stats = parseLabelCounts(
      run(g, buildLabelCountsRequest(labels, { scope: LOAD })),
      labels,
      LOAD,
      { nodes: true, edges: true },
    );
    const own = [...g.nodes.values()].filter((n) => !internal(n.label));
    expect(stats.nodeTotal).toBe(own.length);
    const sample = parseSample(
      run(
        g,
        buildSampleRequest({
          scope: LOAD,
          nodeLabels: stats.nodeLabels,
          edgeLabels: stats.edgeLabels,
          shape: { keys: null, heavyKeys: [] },
        }),
      ),
      { keys: null, heavyKeys: [] },
    );
    expect(sample.nodes).toHaveLength(own.length);
    expect(sample.edges.map((e) => e.label)).toEqual([
      "signed_with",
      "signed_with",
    ]);
    // A scan lists everything; its stats lose Foundry's labels and counts.
    expect(
      shownStats(parseLabelScan(run(g, buildLabelScanRequest())), foundry)
        .nodeTotal,
    ).toBe(own.length);

    // A customer's page of neighbors: its mentions and evidence point at it.
    const shape = { keys: null, heavyKeys: [] };
    const globex = own.find(
      (n) => n.props.company_name === "Globex Corporation",
    )!;
    const page = (excludeEdgePrefix?: string, afterEdgeDbId?: number) =>
      parseNeighborhood(
        run(
          g,
          buildNeighborhoodRequest({
            dbId: globex.id,
            shape,
            excludeEdgePrefix,
            afterEdgeDbId,
            direction: "in",
            limit: 1,
          }),
        ),
        globex.id,
        shape,
        { limit: 1 },
      );
    const all = parseNeighborhood(
      run(g, buildNeighborhoodRequest({ dbId: globex.id, shape })),
      globex.id,
      shape,
    );
    expect(all.edges.some((e) => internal(e.label))).toBe(true);
    const first = page("FOUNDRY_");
    expect(first.edges.map((e) => e.label)).toEqual(["signed_with"]);
    expect(first.nodes.map((n) => n.label)).toEqual(["Contract"]);
    // The cursor and the exclusion filter together: nothing after it.
    expect(page("FOUNDRY_", first.edges[0]!.dbId).edges).toEqual([]);

    const degree = parseDegree(
      run(
        g,
        buildDegreeRequest({ dbId: globex.id, excludeEdgePrefix: "FOUNDRY_" }),
      ),
      {},
    );
    expect(degree.inByLabel).toEqual([{ label: "signed_with", count: 1 }]);
    expect(degree.inDegree).toBe(1);
    expect(
      parseDegree(
        run(g, buildDegreeRequest({ dbId: globex.id })),
        {},
      ).inByLabel.map((l) => l.label),
    ).toEqual(expect.arrayContaining(["FOUNDRY_ABOUT", "FOUNDRY_MENTION_OF"]));
  });

  it("counts a customer's relationships in its menu as the inspector does, without Foundry's edges while hidden", () => {
    const g = database();
    const shape = { keys: null, heavyKeys: [] };
    const globex = [...g.nodes.values()].find(
      (n) => n.props.company_name === "Globex Corporation",
    )!;
    // What the explorer knows of it after a connect: the neighbor probe and
    // the bounded edge count, both over every edge (they budget reads).
    const probe = {
      ...parseDegreeProbe(
        run(g, buildDegreeProbeRequest({ dbIds: [globex.id] })),
        [globex.id],
      ).get(globex.id)!,
      edges: parseEdgeCount(
        run(g, buildEdgeCountRequest({ dbIds: [globex.id] })),
        [globex.id],
      ).get(globex.id)!,
    };
    expect(probe.edges.in).toBe(4);
    expect(menuRelationships(undefined, probe, false).in).toBe("4");

    // Hidden: the menu says nothing from the probe and reads the degree as
    // the inspector does (fetchDegree: the probe's small, non-empty sides,
    // without Foundry's edges).
    expect(menuRelationships(undefined, probe, true)).toEqual({
      empty: { out: false, in: false },
    });
    const sides = (["out", "in"] as const).filter(
      (side) => !isLargeProbe(probe, side) && probe[side] > 0,
    );
    const degree = parseDegree(
      run(
        g,
        buildDegreeRequest({
          dbId: globex.id,
          sides,
          excludeEdgePrefix: "FOUNDRY_",
        }),
      ),
      { sides, probe },
    );
    const menu = menuRelationships(degree, probe, true);
    expect(menu.in).toBe("1");
    expect(menu.empty.in).toBe(false);
    // And expanding its incoming side adds that many edges.
    const incoming = parseNeighborhood(
      run(
        g,
        buildNeighborhoodRequest({
          dbId: globex.id,
          shape,
          direction: "in",
          excludeEdgePrefix: "FOUNDRY_",
        }),
      ),
      globex.id,
      shape,
    );
    expect(String(incoming.edges.length)).toBe(menu.in);
  });

  it("refuses what it can't run, as a database refuses a request it can't decode", () => {
    const g = database();
    const answer = runQuery(g, {
      request_type: "read",
      query: {
        read: {
          entries: [{ query: { name: "x", root: { shortest_path: {} } } }],
          returns: ["x"],
        },
      },
    });
    expect(answer).toEqual({
      status: 400,
      body: {
        error: "invalid_query_json",
        msg: "unsupported operation shortest_path",
      },
    });
    expect(
      runQuery(g, { request_type: "write", query: { write: {} } }).status,
    ).toBe(400);
    expect(
      runQuery(g, {
        request_type: "read",
        query: {
          read: {
            entries: [
              {
                query: {
                  name: "x",
                  root: { nodes: { reference: { param: "p" } } },
                },
              },
            ],
            returns: ["x"],
          },
        },
      }).body,
    ).toEqual({ error: "invalid_query_json", msg: "unknown parameter p" });
  });
});
