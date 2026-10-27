import { describe, expect, it } from "vitest";

import type { IndexCatalogEntry } from "@/web/areas/explore/explorer/lib/types";

import {
  ACTIVITY_LIMIT,
  CAP_NOTICE_ID,
  EMPTY_STORE,
  canvasRoom,
  connectBatches,
  connectCost,
  connectCostFloor,
  containsTargetsFor,
  edgeToJson,
  expansionKey,
  explorerReducer,
  hiddenNodeIds,
  indexLabels,
  initialExplorerState,
  labelKey,
  loadedLabelCounts,
  matchLoadedNodes,
  menuRelationships,
  mergeDegree,
  mergeGraph,
  mergeNode,
  needsEdgeCount,
  nodeSideSize,
  nodeSize,
  nodeToJson,
  planConnect,
  parseIdCandidate,
  pruneSelectionHistory,
  pushSelectionHistory,
  railLabels,
  relationshipGroups,
  removeNodes,
  sideCursorAfter,
  snippetFor,
  textIndexesFor,
  totalsAreFloors,
  degreeText,
  visibleGraph,
  type ExplorerState,
  type GraphStore,
} from "@/web/areas/explore/explorer/lib/graph-store";
import { EXPLORER_LIMITS } from "@/web/areas/explore/explorer/lib/queries";
import type {
  DegreeInfo,
  DegreeProbe,
  GraphEdge,
  GraphNode,
  LabelSchema,
  NeighborhoodResult,
} from "@/web/areas/explore/explorer/lib/types";

const node = (
  dbId: number,
  label = "User",
  properties: GraphNode["properties"] = {},
  extra: Partial<GraphNode> = {},
): GraphNode => ({
  id: `n:${dbId}`,
  dbId,
  label,
  properties,
  omitted: [],
  complete: false,
  ...extra,
});

const edge = (
  dbId: number,
  from: number,
  to: number,
  label = "FOLLOWS",
  properties: GraphEdge["properties"] = {},
): GraphEdge => ({
  id: `e:${dbId}`,
  dbId,
  label,
  source: `n:${from}`,
  target: `n:${to}`,
  properties,
});

function storeOf(nodes: GraphNode[], edges: GraphEdge[] = []): GraphStore {
  return mergeGraph(EMPTY_STORE, { nodes, edges }, { cap: 10_000 }).store;
}

const degree = (overrides: Partial<DegreeInfo> = {}): DegreeInfo => ({
  outDegree: 0,
  inDegree: 0,
  outByLabel: [],
  inByLabel: [],
  ...overrides,
});

describe("mergeNode", () => {
  it("returns the incoming node when there is nothing to merge with", () => {
    const incoming = node(1);
    expect(mergeNode(undefined, incoming)).toBe(incoming);
  });

  it("keeps properties a later partial fetch left out", () => {
    const full = node(
      1,
      "Document",
      { title: "A", body: "long" },
      { complete: true },
    );
    const partial = node(1, "Document", { title: "A" }, { omitted: ["body"] });
    const merged = mergeNode(full, partial);
    expect(merged.properties).toEqual({ title: "A", body: "long" });
    expect(merged.complete).toBe(true);
    expect(merged.omitted).toEqual([]);
  });

  it("drops omitted keys that have arrived and unions the rest", () => {
    const first = node(
      1,
      "Document",
      { title: "A" },
      { omitted: ["body", "embedding"] },
    );
    const second = node(
      1,
      "Document",
      { body: "text" },
      { omitted: ["embedding"] },
    );
    const merged = mergeNode(first, second);
    expect(merged.omitted).toEqual(["embedding"]);
    expect(merged.properties).toEqual({ title: "A", body: "text" });
  });

  it("returns the existing object when nothing changed", () => {
    const existing = node(1, "User", { name: "Ada" });
    expect(mergeNode(existing, node(1, "User", { name: "Ada" }))).toBe(
      existing,
    );
  });

  it("ignores a row with the same id and another label (an edge row, say)", () => {
    const company = node(37, "Company", { name: "Acme" }, { complete: true });
    const edgeRow = node(37, "WORKS_AT", { role: "Engineer", since: 2018 });
    expect(mergeNode(company, edgeRow)).toBe(company);
    const store = mergeGraph(
      storeOf([company]),
      { nodes: [edgeRow] },
      { cap: 10 },
    ).store;
    expect(store.nodes.get("n:37")).toBe(company);
  });

  it("takes the incoming omitted list as exact when asked", () => {
    // Loaded without properties: every key of the label listed as not loaded.
    const light = node(
      1,
      "Project",
      {},
      { omitted: ["description", "name", "notes"] },
    );
    // Refetched with the shape: notes was asked for and is absent on this node.
    const refetched = node(
      1,
      "Project",
      { name: "Helix", description: "Graph DB" },
      { omitted: [] },
    );
    expect(mergeNode(light, refetched).omitted).toEqual(["notes"]);
    expect(
      mergeNode(light, refetched, { replaceOmitted: true }).omitted,
    ).toEqual([]);
  });
});

describe("mergeGraph", () => {
  it("adds nodes and edges and reports what was new", () => {
    const result = mergeGraph(
      EMPTY_STORE,
      { nodes: [node(1), node(2)], edges: [edge(10, 1, 2)] },
      { cap: 100 },
    );
    expect(result.addedNodes).toEqual(["n:1", "n:2"]);
    expect(result.addedEdges).toEqual(["e:10"]);
    expect(result.store.nodes.size).toBe(2);
    expect(result.store.edges.size).toBe(1);
  });

  it("drops edges whose endpoints are not in the store (no dangling edges)", () => {
    const result = mergeGraph(
      EMPTY_STORE,
      { nodes: [node(1)], edges: [edge(10, 1, 404)] },
      { cap: 100 },
    );
    expect(result.store.edges.size).toBe(0);
    expect(result.droppedEdges).toBe(1);
  });

  it("accepts edges to nodes already in the store", () => {
    const store = storeOf([node(1)]);
    const result = mergeGraph(
      store,
      { nodes: [node(2)], edges: [edge(10, 1, 2)] },
      { cap: 100 },
    );
    expect(result.addedEdges).toEqual(["e:10"]);
  });

  it("refuses new nodes past the cap, in the order given, and their edges", () => {
    const store = storeOf([node(1), node(2)]);
    const result = mergeGraph(
      store,
      {
        nodes: [node(3), node(4), node(5)],
        edges: [edge(10, 1, 3), edge(11, 1, 5)],
      },
      { cap: 3 },
    );
    expect(result.addedNodes).toEqual(["n:3"]);
    expect(result.droppedNodes).toBe(2);
    expect([...result.store.edges.keys()]).toEqual(["e:10"]);
    expect(result.droppedEdges).toBe(1);
  });

  it("still updates existing nodes when the store is at the cap", () => {
    const store = storeOf([node(1, "User", { name: "Ada" })]);
    const result = mergeGraph(
      store,
      { nodes: [node(1, "User", { age: 36 })] },
      { cap: 1 },
    );
    expect(result.droppedNodes).toBe(0);
    expect(result.store.nodes.get("n:1")?.properties).toEqual({
      name: "Ada",
      age: 36,
    });
  });

  it("returns the same store object when nothing changed", () => {
    const store = storeOf(
      [node(1, "User", { name: "Ada" }), node(2)],
      [edge(10, 1, 2)],
    );
    const result = mergeGraph(
      store,
      { nodes: [node(1, "User", { name: "Ada" })], edges: [edge(10, 1, 2)] },
      { cap: 100 },
    );
    expect(result.store).toBe(store);
    expect(result.addedNodes).toEqual([]);
  });

  it("updates an edge whose properties changed", () => {
    const store = storeOf(
      [node(1), node(2)],
      [edge(10, 1, 2, "FOLLOWS", { since: 1 })],
    );
    const result = mergeGraph(
      store,
      { edges: [edge(10, 1, 2, "FOLLOWS", { since: 2 })] },
      { cap: 100 },
    );
    expect(result.store.edges.get("e:10")?.properties).toEqual({ since: 2 });
    expect(result.addedEdges).toEqual([]);
  });

  it("keeps edge properties a later fetch without them left out", () => {
    const store = storeOf(
      [node(1), node(2)],
      [edge(10, 1, 2, "SIMILAR", { score: 0.5, note: "x" })],
    );
    // Light edge keys only, then ids and endpoints only (a too-large retry).
    const light = mergeGraph(
      store,
      { edges: [edge(10, 1, 2, "SIMILAR", { score: 0.5 })] },
      { cap: 100 },
    );
    expect(light.store).toBe(store);
    const bare = mergeGraph(
      store,
      { edges: [edge(10, 1, 2, "SIMILAR")] },
      { cap: 100 },
    );
    expect(bare.store.edges.get("e:10")?.properties).toEqual({
      score: 0.5,
      note: "x",
    });
  });

  it("never mutates the input store", () => {
    const store = storeOf([node(1)]);
    mergeGraph(store, { nodes: [node(2)] }, { cap: 100 });
    expect(store.nodes.size).toBe(1);
  });
});

describe("removeNodes and visibleGraph", () => {
  const store = storeOf(
    [node(1), node(2, "Company"), node(3)],
    [edge(10, 1, 2, "WORKS_AT"), edge(11, 1, 3), edge(12, 3, 3)],
  );

  it("removes nodes with every edge that touches them", () => {
    const next = removeNodes(store, ["n:1"]);
    expect([...next.nodes.keys()]).toEqual(["n:2", "n:3"]);
    expect([...next.edges.keys()]).toEqual(["e:12"]);
  });

  it("returns the same store when no id is present", () => {
    expect(removeNodes(store, ["n:99"])).toBe(store);
  });

  it("filters hidden node labels, hidden nodes and hidden edge labels", () => {
    const hiddenCompany = visibleGraph(
      store,
      new Set([labelKey("node", "Company")]),
      new Set(),
    );
    expect(hiddenCompany.nodes.map((n) => n.id)).toEqual(["n:1", "n:3"]);
    expect(hiddenCompany.edges.map((e) => e.id)).toEqual(["e:11", "e:12"]);

    const hiddenNode = visibleGraph(store, new Set(), new Set(["n:3"]));
    expect(hiddenNode.edges.map((e) => e.id)).toEqual(["e:10"]);

    const hiddenEdges = visibleGraph(
      store,
      new Set([labelKey("edge", "FOLLOWS")]),
      new Set(),
    );
    expect(hiddenEdges.nodes).toHaveLength(3);
    expect(hiddenEdges.edges.map((e) => e.id)).toEqual(["e:10"]);
  });

  it("counts loaded labels", () => {
    const counts = loadedLabelCounts(store);
    expect(Object.fromEntries(counts.nodes)).toEqual({ User: 2, Company: 1 });
    expect(Object.fromEntries(counts.edges)).toEqual({
      WORKS_AT: 1,
      FOLLOWS: 2,
    });
  });
});

describe("degrees and expansion cursors", () => {
  it("keeps per-label degrees from the first page when a later page has none", () => {
    const first = degree({
      outDegree: 3,
      outByLabel: [{ label: "FOLLOWS", count: 3 }],
    });
    const later = degree({ outDegree: 4 });
    expect(mergeDegree(first, later, false)).toEqual({
      ...later,
      outByLabel: first.outByLabel,
      inByLabel: [],
    });
    expect(mergeDegree(first, later, true)).toBe(later);
    expect(mergeDegree(undefined, later, false)).toBe(later);
  });

  const result = (
    edges: GraphEdge[],
    truncated: boolean,
  ): NeighborhoodResult => ({
    center: node(1),
    nodes: [],
    edges,
    degree: degree(),
    truncated,
  });

  it("is done when the side came back under the limit", () => {
    expect(
      sideCursorAfter(result([edge(5, 1, 2), edge(6, 1, 3)], true), "out", 3),
    ).toBe("done");
  });

  it("is done when nothing was cut off", () => {
    expect(
      sideCursorAfter(result([edge(5, 1, 2), edge(6, 1, 3)], false), "out", 2),
    ).toBe("done");
  });

  it("pages after the largest edge id on that side", () => {
    const edges = [edge(5, 1, 2), edge(9, 1, 3), edge(7, 4, 1)];
    expect(sideCursorAfter(result(edges, true), "out", 2)).toBe(9);
    expect(sideCursorAfter(result(edges, true), "in", 2)).toBe("done");
  });

  it("keys expansions by node and edge label", () => {
    expect(expansionKey("n:1", null)).toBe("n:1|*");
    expect(expansionKey("n:1", "FOLLOWS")).toBe("n:1|FOLLOWS");
  });
});

describe("relationshipGroups", () => {
  const store = storeOf(
    [node(1), node(2), node(3), node(4, "Company")],
    [
      edge(10, 1, 2),
      edge(11, 1, 3),
      edge(12, 3, 1),
      edge(13, 1, 4, "WORKS_AT"),
      edge(14, 1, 1),
    ],
  );

  it("groups loaded edges by direction and label, out first", () => {
    const groups = relationshipGroups(store, "n:1");
    expect(groups.map((g) => [g.direction, g.edgeLabel, g.loaded])).toEqual([
      ["out", "FOLLOWS", 3],
      ["out", "WORKS_AT", 1],
      ["in", "FOLLOWS", 2],
    ]);
    expect(groups[0].neighbors.map((n) => n.nodeId)).toEqual([
      "n:2",
      "n:3",
      "n:1",
    ]);
  });

  it("adds totals from the degree, including groups with nothing loaded", () => {
    const groups = relationshipGroups(
      store,
      "n:1",
      degree({
        outByLabel: [{ label: "FOLLOWS", count: 12 }],
        inByLabel: [{ label: "TAGGED", count: 340 }],
      }),
    );
    expect(
      groups.find((g) => g.direction === "out" && g.edgeLabel === "FOLLOWS")
        ?.total,
    ).toBe(12);
    const tagged = groups.find((g) => g.edgeLabel === "TAGGED");
    expect(tagged).toMatchObject({
      direction: "in",
      loaded: 0,
      total: 340,
      neighbors: [],
    });
  });
});

describe("JSON copies", () => {
  it("copies a node without anything the canvas added", () => {
    const simulated = {
      ...node(7, "User", { name: "Ada" }),
      x: 1,
      y: 2,
      vx: 0,
      fx: 4,
    } as GraphNode;
    expect(nodeToJson(simulated)).toEqual({
      $id: 7,
      $label: "User",
      name: "Ada",
    });
  });

  it("copies an edge with endpoint ids, not node objects", () => {
    const store = storeOf(
      [node(1), node(2)],
      [edge(10, 1, 2, "FOLLOWS", { since: 2020 })],
    );
    expect(edgeToJson(store.edges.get("e:10") as GraphEdge, store)).toEqual({
      $id: 10,
      $label: "FOLLOWS",
      $from: 1,
      $to: 2,
      since: 2020,
    });
  });
});

describe("selection history", () => {
  it("pushes the previous selection when the selection changes", () => {
    const a = { kind: "node", id: "n:1" } as const;
    const b = { kind: "node", id: "n:2" } as const;
    expect(pushSelectionHistory([], null, a)).toEqual([]);
    expect(pushSelectionHistory([], a, b)).toEqual([a]);
    expect(pushSelectionHistory([a], a, a)).toEqual([a]);
    expect(pushSelectionHistory([a, b], a, b)).toEqual([b, a]);
  });

  it("drops entries that left the store", () => {
    const store = storeOf([node(1)]);
    const history = [
      { kind: "node", id: "n:1" },
      { kind: "node", id: "n:2" },
      { kind: "edge", id: "e:1" },
    ] as const;
    expect(pruneSelectionHistory([...history], store)).toEqual([
      { kind: "node", id: "n:1" },
    ]);
    const kept = [{ kind: "node" as const, id: "n:1" }];
    expect(pruneSelectionHistory(kept, store)).toBe(kept);
  });
});

describe("matchLoadedNodes", () => {
  const nodes = [
    node(1, "User", {
      name: "Ada Lovelace",
      profile: { city: "London", tags: ["math"] },
    }),
    node(2, "User", {
      name: "Grace Hopper",
      nested: { deep: { deeper: { deepest: { leaf: "hidden" } } } },
    }),
    node(3, "Document", {
      title: "Graph objects",
      embedding: Array.from({ length: 64 }, (_, i) => i),
    }),
    node(42, "User", { name: "Alan" }),
  ];
  const caption = (n: GraphNode) =>
    String(n.properties.name ?? n.properties.title ?? n.id);

  it("ranks exact and prefix caption matches before property matches", () => {
    const hits = matchLoadedNodes(nodes, "ada lovelace", caption);
    expect(hits[0].node.id).toBe("n:1");
    expect(hits[0].matchedKey).toBeNull();
  });

  it("matches nested property values by path, case-insensitively", () => {
    const [hit] = matchLoadedNodes(nodes, "LONDON", caption);
    expect(hit).toMatchObject({
      node: { id: "n:1" },
      matchedKey: "profile.city",
      snippet: "London",
    });
  });

  it("does not match the word object against object-valued properties", () => {
    const hits = matchLoadedNodes(nodes, "object", caption);
    expect(hits.map((h) => h.node.id)).toEqual(["n:3"]);
  });

  it("matches ids, with or without #", () => {
    expect(matchLoadedNodes(nodes, "42", caption)[0].node.id).toBe("n:42");
    expect(matchLoadedNodes(nodes, "#42", caption)[0].node.id).toBe("n:42");
  });

  it("skips vectors and values nested too deep", () => {
    expect(matchLoadedNodes(nodes, "63", caption)).toEqual([]);
    expect(matchLoadedNodes(nodes, "hidden", caption)).toEqual([]);
  });

  it("does not search Foundry's stamps, but does Foundry's own data", () => {
    // ULIDs a request wrote share their start, so a stamp matched by a
    // short needle would bring every node it wrote.
    const stamped = [
      node(7, "Customer", {
        name: "Acme",
        foundry_tx: "01J9ZQ8R4T",
        foundry_load: "load-01J9",
      }),
      node(8, "FoundryTx", {
        foundry_tx: "01J9ZQ8R4T",
        foundry_load: "load-01J9",
      }),
    ];
    const hits = matchLoadedNodes(stamped, "01j9", caption);
    expect(hits).toEqual([
      { node: stamped[1], matchedKey: "foundry_tx", snippet: "01J9ZQ8R4T" },
    ]);
    // The load is a stamp on Foundry's own labels too.
    expect(matchLoadedNodes(stamped, "load-", caption)).toEqual([]);
    // Look-alike keys are the data's.
    const [own] = matchLoadedNodes(
      [node(9, "Customer", { name: "Zed", my_foundry_tx: "01J9" })],
      "01j9",
      caption,
    );
    expect(own.matchedKey).toBe("my_foundry_tx");
  });

  it("caps the number of hits and ignores blank input", () => {
    expect(matchLoadedNodes(nodes, "a", caption, 2)).toHaveLength(2);
    expect(matchLoadedNodes(nodes, "   ", caption)).toEqual([]);
  });

  it("excerpts long values around the match", () => {
    const text = `${"x".repeat(100)} needle ${"y".repeat(100)}`;
    const snippet = snippetFor(text, "NEEDLE");
    expect(snippet.startsWith("…")).toBe(true);
    expect(snippet.endsWith("…")).toBe(true);
    expect(snippet).toContain("needle");
    expect(snippetFor("short text", "absent")).toBe("short text");
  });
});

describe("search planning", () => {
  it("reads id candidates", () => {
    expect(parseIdCandidate("977")).toBe(977);
    expect(parseIdCandidate(" #12 ")).toBe(12);
    expect(parseIdCandidate("12a")).toBeNull();
    expect(parseIdCandidate("")).toBeNull();
    expect(parseIdCandidate("99999999999999999")).toBeNull();
  });

  it("uses node full_text indexes only, once each", () => {
    const indexes: IndexCatalogEntry[] = [
      {
        indexId: "1",
        label: "Document",
        property: "title",
        target: "node",
        type: "full_text",
        tenantProperty: null,
      },
      {
        indexId: "2",
        label: "Document",
        property: "title",
        target: "node",
        type: "full_text",
        tenantProperty: null,
      },
      {
        indexId: "3",
        label: "Document",
        property: "embedding",
        target: "node",
        type: "vector",
        tenantProperty: null,
      },
      {
        indexId: "4",
        label: "NOTE",
        property: "text",
        target: "edge",
        type: "full_text",
        tenantProperty: null,
      },
    ];
    expect(textIndexesFor(indexes)).toEqual([
      { label: "Document", property: "title" },
    ]);
    expect(textIndexesFor(undefined)).toEqual([]);
  });

  it("leaves out text indexes partitioned by a tenant property", () => {
    // Searching one needs a tenant value; without it the database fails the whole request.
    const indexes: IndexCatalogEntry[] = [
      {
        indexId: "1",
        label: "Document",
        property: "body",
        target: "node",
        type: "full_text",
        tenantProperty: "tenant_id",
      },
      {
        indexId: "2",
        label: "Note",
        property: "text",
        target: "node",
        type: "full_text",
        tenantProperty: null,
      },
    ];
    expect(textIndexesFor(indexes)).toEqual([
      { label: "Note", property: "text" },
    ]);
  });

  const schema = (
    label: string,
    properties: [string, string, boolean?][],
  ): LabelSchema => ({
    target: "node",
    label,
    sampled: 2,
    properties: properties.map(([key, kind, heavy]) => ({
      key,
      kinds: [kind as "string"],
      heavy: heavy ?? false,
      indexes: [],
      seen: 2,
    })),
  });

  it("picks light string keys per label, naming keys first, skipping text-indexed ones", () => {
    const targets = containsTargetsFor(
      [
        schema("User", [
          ["bio", "string"],
          ["email", "string"],
          ["name", "string"],
          ["age", "integer"],
          ["notes", "string"],
        ]),
        schema("Document", [
          ["title", "string"],
          ["body", "string", true],
        ]),
        { ...schema("FOLLOWS", [["note", "string"]]), target: "edge" },
      ],
      [{ label: "Document", property: "title" }],
    );
    expect(targets).toEqual([
      { label: "User", keys: ["name", "email", "bio"] },
    ]);
  });

  it("leaves Foundry's stamps out of the scan, so the data's keys get the slots", () => {
    const targets = containsTargetsFor([
      schema("Customer", [
        ["foundry_load", "string"],
        ["foundry_tx", "string"],
        ["foundry_part", "string"],
        ["city", "string"],
        ["status", "string"],
      ]),
      schema("FoundryTx", [
        ["foundry_load", "string"],
        ["foundry_tx", "string"],
      ]),
      schema("Invoice", [["foundry_tx", "string"]]),
    ]);
    // A FoundryTx's `foundry_tx` is its data; its load is a stamp.
    expect(targets).toEqual([
      { label: "Customer", keys: ["city", "status"] },
      { label: "FoundryTx", keys: ["foundry_tx"] },
    ]);
  });
});

describe("explorerReducer", () => {
  const ready = (store: GraphStore): ExplorerState => ({
    ...initialExplorerState(),
    phase: "ready",
    store,
  });

  it("boots into loading (or no-access) and keeps activity and hidden labels", () => {
    const before = {
      ...ready(storeOf([node(1)])),
      hiddenLabels: new Set(["node:User"]),
      activity: [
        {
          id: "a",
          title: "x",
          request: { request_type: "read" as const, query: {} },
          at: 1,
          ok: true,
          status: 200,
          elapsedMs: 3,
        },
      ],
    };
    const booted = explorerReducer(before, { type: "boot", noAccess: false });
    expect(booted.phase).toBe("loading");
    expect(booted.store).toBe(EMPTY_STORE);
    expect([...booted.hiddenLabels]).toEqual(["node:User"]);
    expect(booted.activity).toHaveLength(1);
    expect(
      explorerReducer(before, { type: "boot", noAccess: true }).phase,
    ).toBe("no-access");
    expect([
      ...explorerReducer(before, {
        type: "boot",
        noAccess: false,
        hiddenLabels: ["edge:X"],
      }).hiddenLabels,
    ]).toEqual(["edge:X"]);
  });

  it("treats a forbidden failure as no access", () => {
    const failure = {
      ok: false as const,
      kind: "forbidden" as const,
      status: 403,
      code: null,
      message: "no",
      elapsedMs: 1,
    };
    expect(
      explorerReducer(initialExplorerState(), { type: "boot/failed", failure })
        .phase,
    ).toBe("no-access");
    expect(
      explorerReducer(initialExplorerState(), {
        type: "sample/failed",
        failure,
      }).phase,
    ).toBe("no-access");
    const timeout = {
      ...failure,
      kind: "database" as const,
      status: 408,
      code: "query_timeout",
    };
    const failed = explorerReducer(initialExplorerState(), {
      type: "sample/failed",
      failure: timeout,
    });
    expect(failed.phase).toBe("error");
    expect(failed.bootError).toBe(timeout);
  });

  it("adds a cap notice when a merge drops nodes, and recovers a failed first view", () => {
    const failed = { ...initialExplorerState(), phase: "error" as const };
    const merge = mergeGraph(
      EMPTY_STORE,
      { nodes: [node(1), node(2)] },
      { cap: 1 },
    );
    const next = explorerReducer(failed, { type: "merged", merge, cap: 1 });
    expect(next.phase).toBe("ready");
    expect(next.notices.map((n) => n.id)).toEqual([CAP_NOTICE_ID]);
    expect(next.notices[0].message).toContain("1 node wasn't added");
  });

  it("keeps state identity for merges that change nothing", () => {
    const state = ready(storeOf([node(1)]));
    const merge = mergeGraph(state.store, { nodes: [node(1)] }, { cap: 10 });
    expect(explorerReducer(state, { type: "merged", merge, cap: 10 })).toBe(
      state,
    );
  });

  it("tracks label pages", () => {
    let state = explorerReducer(initialExplorerState(), {
      type: "labelPage/loading",
      label: "User",
    });
    expect(state.labelPages.User.status).toBe("loading");
    state = explorerReducer(state, {
      type: "labelPage/loaded",
      label: "User",
      nextAfter: 86,
      total: 600,
      count: 50,
    });
    expect(state.labelPages.User).toEqual({
      nextAfter: 86,
      exhausted: false,
      loaded: 50,
      status: "idle",
      total: 600,
    });
    state = explorerReducer(state, {
      type: "labelPage/loaded",
      label: "User",
      nextAfter: null,
      total: null,
      count: 20,
    });
    expect(state.labelPages.User).toMatchObject({
      exhausted: true,
      loaded: 70,
      total: 600,
    });
    state = explorerReducer(state, { type: "labelPage/failed", label: "User" });
    expect(state.labelPages.User.status).toBe("error");
  });

  it("caches degrees and their status", () => {
    let state = explorerReducer(initialExplorerState(), {
      type: "degree/loading",
      nodeId: "n:1",
    });
    expect(state.degreeStatus.get("n:1")).toBe("loading");
    state = explorerReducer(state, { type: "degree/cancelled", nodeId: "n:1" });
    expect(state.degreeStatus.has("n:1")).toBe(false);
    state = explorerReducer(state, {
      type: "degree/loaded",
      nodeId: "n:1",
      degree: degree({ outDegree: 2 }),
      hasLabels: true,
    });
    expect(state.degrees.get("n:1")?.outDegree).toBe(2);
  });

  it("only ends the pending operation it started", () => {
    let state = explorerReducer(initialExplorerState(), {
      type: "pending/start",
      op: "expand:n:1",
      id: "a",
    });
    state = explorerReducer(state, {
      type: "pending/start",
      op: "expand:n:1",
      id: "b",
    });
    state = explorerReducer(state, {
      type: "pending/end",
      op: "expand:n:1",
      id: "a",
    });
    expect(state.pending.get("expand:n:1")).toBe("b");
    state = explorerReducer(state, {
      type: "pending/end",
      op: "expand:n:1",
      id: "b",
    });
    expect(state.pending.size).toBe(0);
  });

  it("logs activity newest first, capped, and updates entries in place", () => {
    let state = initialExplorerState();
    for (let i = 0; i < ACTIVITY_LIMIT + 5; i++) {
      state = explorerReducer(state, {
        type: "activity/start",
        entry: {
          id: `a${i}`,
          title: `Run ${i}`,
          request: { request_type: "read", query: {} },
          at: i,
          ok: null,
          status: null,
          elapsedMs: null,
        },
      });
    }
    expect(state.activity).toHaveLength(ACTIVITY_LIMIT);
    expect(state.activity[0].id).toBe(`a${ACTIVITY_LIMIT + 4}`);
    state = explorerReducer(state, {
      type: "activity/end",
      id: "a10",
      ok: true,
      status: 200,
      elapsedMs: 9,
    });
    expect(state.activity.find((entry) => entry.id === "a10")).toMatchObject({
      ok: true,
      status: 200,
      elapsedMs: 9,
    });
  });

  it("hides and shows nodes and labels", () => {
    let state = ready(storeOf([node(1), node(2)]));
    state = explorerReducer(state, {
      type: "nodes/hide",
      ids: ["n:1", "n:404"],
    });
    expect([...state.hiddenNodes]).toEqual(["n:1"]);
    state = explorerReducer(state, {
      type: "label/hidden",
      key: "node:User",
      hidden: true,
    });
    expect(state.hiddenLabels.has("node:User")).toBe(true);
    expect(
      explorerReducer(state, {
        type: "label/hidden",
        key: "node:User",
        hidden: true,
      }),
    ).toBe(state);
    state = explorerReducer(state, { type: "nodes/show", ids: ["n:1"] });
    expect(state.hiddenNodes.size).toBe(0);
    state = explorerReducer(state, { type: "hidden/reset" });
    expect(state.hiddenLabels.size).toBe(0);
  });

  it("forgets per-node state for removed nodes", () => {
    let state = ready(storeOf([node(1), node(2)], [edge(10, 1, 2)]));
    state = explorerReducer(state, { type: "pin", id: "n:1", pinned: true });
    state = explorerReducer(state, { type: "nodes/hide", ids: ["n:1"] });
    state = explorerReducer(state, { type: "expansion", key: "n:1|*", out: 5 });
    state = explorerReducer(state, { type: "nodes/remove", ids: ["n:1"] });
    expect(state.store.nodes.has("n:1")).toBe(false);
    expect(state.store.edges.size).toBe(0);
    expect(state.pinned.size).toBe(0);
    expect(state.hiddenNodes.size).toBe(0);
    expect(state.expansions.size).toBe(0);
  });

  it("clears the canvas, its paging and the cap notice but keeps the phase", () => {
    let state = ready(storeOf([node(1)]));
    state = explorerReducer(state, {
      type: "labelPage/loaded",
      label: "User",
      nextAfter: 3,
      total: 9,
      count: 1,
    });
    state = explorerReducer(state, {
      type: "notice/add",
      notice: { id: CAP_NOTICE_ID, tone: "warning", message: "full" },
    });
    state = explorerReducer(state, { type: "canvas/clear" });
    expect(state.store.nodes.size).toBe(0);
    expect(state.labelPages).toEqual({});
    expect(state.notices).toEqual([]);
    expect(state.phase).toBe("ready");
  });

  it("does not pin nodes that aren't loaded", () => {
    const state = ready(storeOf([node(1)]));
    expect(
      explorerReducer(state, { type: "pin", id: "n:9", pinned: true }),
    ).toBe(state);
  });

  it("merges expansion cursors per side", () => {
    let state = explorerReducer(initialExplorerState(), {
      type: "expansion",
      key: "n:1|*",
      out: 12,
      in: "done",
    });
    state = explorerReducer(state, {
      type: "expansion",
      key: "n:1|*",
      out: "done",
    });
    expect(state.expansions.get("n:1|*")).toEqual({ out: "done", in: "done" });
  });
});

describe("canvas room", () => {
  const state = (hiddenNodes: string[] = [], hiddenLabels: string[] = []) => ({
    store: storeOf([node(1), node(2), node(3, "Tag"), node(4, "Tag")]),
    hiddenNodes: new Set(hiddenNodes),
    hiddenLabels: new Set(hiddenLabels),
  });

  it("counts hidden nodes as room, since they are removed to make it", () => {
    expect(canvasRoom(state(), 4)).toBe(0);
    expect(canvasRoom(state(["n:1"]), 4)).toBe(1);
    expect(canvasRoom(state(["n:1"], ["node:Tag"]), 4)).toBe(3);
  });

  it("evicts nodes hidden one by one first, then nodes of hidden labels, never the kept ones", () => {
    expect(hiddenNodeIds(state(["n:2"], ["node:Tag"]))).toEqual([
      "n:2",
      "n:3",
      "n:4",
    ]);
    expect(
      hiddenNodeIds(state(["n:2", "n:9"], ["node:Tag"]), new Set(["n:3"])),
    ).toEqual(["n:2", "n:4"]);
    expect(hiddenNodeIds(state([], ["edge:Tag"]))).toEqual([]);
  });
});

describe("railLabels", () => {
  const stats = {
    nodeLabels: [{ label: "User", count: 3 }],
    edgeLabels: [{ label: "FOLLOWS", count: 2 }],
    nodeTotal: 3,
    edgeTotal: 2,
  };

  it("adds labels loaded after the counts, with an unknown count", () => {
    const store = storeOf(
      [node(1), node(2), node(9, "Session")],
      [edge(5, 9, 1, "OPENED_BY")],
    );
    const labels = railLabels({
      overview: { status: "ready", stats, error: null },
      store,
      schemas: [],
      labelPages: {},
    });
    expect(labels.nodes).toEqual([
      { label: "Session", count: null },
      { label: "User", count: 3 },
    ]);
    expect(labels.edges).toEqual([
      { label: "FOLLOWS", count: 2 },
      { label: "OPENED_BY", count: null },
    ]);
  });

  it("fills a count the label counts left out from a label page's total", () => {
    const labels = railLabels({
      overview: {
        status: "ready",
        stats: {
          ...stats,
          nodeLabels: [...stats.nodeLabels, { label: "Order", count: null }],
        },
        error: null,
      },
      store: storeOf([node(9, "Session")]),
      schemas: [],
      labelPages: {
        Order: {
          nextAfter: 1,
          exhausted: false,
          loaded: 1,
          status: "idle",
          total: 12,
        },
        Session: {
          nextAfter: null,
          exhausted: true,
          loaded: 1,
          status: "idle",
          total: 1,
        },
      },
    });
    expect(labels.nodes).toEqual([
      { label: "Order", count: 12 },
      { label: "Session", count: 1 },
      { label: "User", count: 3 },
    ]);
  });

  it("lists loaded and probed labels without counts, using a label page's total", () => {
    const labels = railLabels({
      overview: { status: "error", stats: null, error: null },
      store: storeOf([node(1)]),
      schemas: [{ target: "node", label: "Tag", sampled: 1, properties: [] }],
      labelPages: {
        User: {
          nextAfter: 1,
          exhausted: false,
          loaded: 1,
          status: "idle",
          total: 40,
        },
      },
    });
    expect(labels.nodes).toEqual([
      { label: "Tag", count: null },
      { label: "User", count: 40 },
    ]);
  });
});

describe("indexLabels", () => {
  it("lists the catalog's node and edge labels once each", () => {
    const indexes: IndexCatalogEntry[] = [
      {
        indexId: "1",
        label: "User",
        property: "email",
        target: "node",
        type: "equality",
        unique: true,
      },
      {
        indexId: "2",
        label: "User",
        property: "bio",
        target: "node",
        type: "full_text",
        tenantProperty: null,
      },
      {
        indexId: "3",
        label: "FOLLOWS",
        property: "since",
        target: "edge",
        type: "equality",
      },
      {
        indexId: "4",
        label: "",
        property: "x",
        target: "node",
        type: "equality",
        unique: false,
      },
    ];
    expect(indexLabels(indexes)).toEqual({
      nodes: ["User"],
      edges: ["FOLLOWS"],
    });
    expect(indexLabels(undefined)).toEqual({ nodes: [], edges: [] });
  });
});

describe("explorerReducer label counts", () => {
  it("folds counts for labels seen later into the stats", () => {
    let state = explorerReducer(initialExplorerState(), {
      type: "overview/loaded",
      stats: {
        nodeLabels: [{ label: "User", count: 3 }],
        edgeLabels: [],
        nodeTotal: 3,
        edgeTotal: 0,
        partial: { nodes: true, edges: true },
      },
    });
    state = explorerReducer(state, {
      type: "overview/merged",
      stats: {
        nodeLabels: [{ label: "Order", count: 7 }],
        edgeLabels: [],
        nodeTotal: 7,
        edgeTotal: 0,
      },
    });
    expect(state.overview.stats).toEqual({
      nodeLabels: [
        { label: "Order", count: 7 },
        { label: "User", count: 3 },
      ],
      edgeLabels: [],
      nodeTotal: 10,
      edgeTotal: 0,
      partial: { nodes: true, edges: true },
    });
  });

  it("records which step a failed boot stopped at", () => {
    const failure = {
      ok: false as const,
      kind: "database" as const,
      status: 503,
      code: "rate_limit_unavailable",
      message: "m",
      elapsedMs: 1,
    };
    const labels = explorerReducer(initialExplorerState(), {
      type: "boot/failed",
      failure,
      stage: "labels",
    });
    expect(labels).toMatchObject({ phase: "error", bootFailedAt: "labels" });
    const sample = explorerReducer(initialExplorerState(), {
      type: "sample/failed",
      failure,
    });
    expect(sample.bootFailedAt).toBe("sample");
    // A fresh boot clears it.
    expect(
      explorerReducer(labels, { type: "boot", noAccess: false }).bootFailedAt,
    ).toBeNull();
  });
});

describe("explorerReducer recoveries", () => {
  it("leaves the empty state once nodes land (written, then shown in the graph)", () => {
    const empty = explorerReducer(
      { ...initialExplorerState(), phase: "loading" },
      { type: "boot/empty" },
    );
    expect(empty.phase).toBe("empty");
    const merge = mergeGraph(empty.store, { nodes: [node(1)] }, { cap: 10 });
    const next = explorerReducer(empty, { type: "merged", merge, cap: 10 });
    expect(next.phase).toBe("ready");
    expect(next.store.nodes.size).toBe(1);
    // Edges alone (nothing new to draw) don't count.
    const nothing = mergeGraph(
      empty.store,
      { edges: [edge(1, 1, 2)] },
      { cap: 10 },
    );
    expect(
      explorerReducer(empty, { type: "merged", merge: nothing, cap: 10 }).phase,
    ).toBe("empty");
  });

  it("puts back the counts' failed state when a retry is cancelled", () => {
    const failure = {
      ok: false as const,
      kind: "database" as const,
      status: 408,
      code: "query_timeout",
      message: "slow",
      elapsedMs: 1,
    };
    let state = explorerReducer(initialExplorerState(), {
      type: "overview/failed",
      failure,
    });
    state = explorerReducer(state, { type: "overview/loading" });
    expect(state.overview.status).toBe("loading");
    state = explorerReducer(state, {
      type: "overview/restore",
      status: "error",
      error: failure,
    });
    expect(state.overview).toMatchObject({ status: "error", error: failure });
    // Only a count still loading is restored.
    const loaded = explorerReducer(state, {
      type: "overview/loaded",
      stats: { nodeLabels: [], edgeLabels: [], nodeTotal: 0, edgeTotal: 0 },
    });
    expect(
      explorerReducer(loaded, {
        type: "overview/restore",
        status: "error",
        error: failure,
      }),
    ).toBe(loaded);
  });

  it("keeps nodes merged while the sample loaded, since the sample merges into the store", () => {
    // The hook builds the sample's merge from the current store.
    const during = { ...initialExplorerState(), phase: "loading" as const };
    const page = mergeGraph(
      during.store,
      { nodes: [node(100), node(101)] },
      { cap: 10 },
    );
    let state = explorerReducer(during, {
      type: "merged",
      merge: page,
      cap: 10,
    });
    state = explorerReducer(state, {
      type: "labelPage/loaded",
      label: "User",
      nextAfter: 101,
      total: 9,
      count: 2,
    });
    const sample = mergeGraph(
      state.store,
      { nodes: [node(1), node(2)] },
      { cap: 10 },
    );
    state = explorerReducer(state, {
      type: "sample/loaded",
      merge: sample,
      truncated: true,
      light: false,
    });
    expect([...state.store.nodes.keys()].sort()).toEqual([
      "n:1",
      "n:100",
      "n:101",
      "n:2",
    ]);
    expect(state.labelPages.User.nextAfter).toBe(101);
  });
});

describe("node sizes", () => {
  const threshold = EXPLORER_LIMITS.degreeThreshold;
  const sized = (
    degrees: [string, DegreeInfo][],
    probes: [string, DegreeProbe][],
  ) => ({
    degrees: new Map(degrees),
    probes: new Map(probes),
  });
  const degree = (
    outDegree: number,
    inDegree: number,
    atLeast?: DegreeInfo["atLeast"],
  ): DegreeInfo => ({
    outDegree,
    inDegree,
    outByLabel: [],
    inByLabel: [],
    ...(atLeast ? { atLeast } : {}),
  });

  it("knows a side from the degree first, then the probe, else not at all", () => {
    const state = sized(
      [
        ["n:1", degree(3, threshold, { out: false, in: true })],
        ["n:2", degree(50_000, 0)],
      ],
      [
        ["n:3", { out: threshold + 1, in: 2 }],
        ["n:1", { out: 0, in: 0 }],
      ],
    );
    expect(nodeSideSize(state, "n:1", "out")).toBe("small");
    expect(nodeSideSize(state, "n:1", "in")).toBe("large");
    // An exact count over the threshold is large too.
    expect(nodeSize(state, "n:2")).toBe("large");
    expect(nodeSideSize(state, "n:3", "out")).toBe("large");
    expect(nodeSideSize(state, "n:3", "in")).toBe("small");
    expect(nodeSize(state, "n:3")).toBe("large");
    expect(nodeSize(state, "n:4")).toBe("unknown");
    expect(
      nodeSize(sized([], [["n:5", { out: threshold, in: threshold }]]), "n:5"),
    ).toBe("small");
  });

  it("counts a side with more edges than the threshold as large, however few its neighbors", () => {
    // Two counterparties, thousands of parallel edges (transactions, repeated events).
    const state = sized(
      [],
      [["n:1", { out: 2, in: 1, edges: { out: threshold + 1, in: 4 } }]],
    );
    expect(nodeSideSize(state, "n:1", "out")).toBe("large");
    expect(nodeSideSize(state, "n:1", "in")).toBe("small");
    expect(nodeSize(state, "n:1")).toBe("large");
    expect(connectCost(state, "n:1")).toBeNull();
  });

  it("needs an edge count only for nodes small by their neighbors whose edges were never counted", () => {
    const state = sized(
      [["n:degree", degree(2, 3)]],
      [
        ["n:uncounted", { out: 3, in: 3 }],
        ["n:counted", { out: 3, in: 3, edges: { out: 30, in: 3 } }],
        ["n:hub", { out: threshold + 1, in: 0 }],
      ],
    );
    expect(needsEdgeCount(state, "n:uncounted")).toBe(true);
    expect(needsEdgeCount(state, "n:counted")).toBe(false);
    // The bounded degree already counts edges; a hub's edges are never counted on their own.
    expect(needsEdgeCount(state, "n:degree")).toBe(false);
    expect(needsEdgeCount(state, "n:hub")).toBe(false);
    expect(needsEdgeCount(state, "n:unknown")).toBe(false);
    expect(connectCost(state, "n:counted")).toBe(33);
    expect(connectCost(state, "n:degree")).toBe(5);
    expect(connectCost(state, "n:uncounted")).toBeNull();
    // A floor on what connecting them reads: counted edges, else neighbors; large and unknown add nothing.
    const all = [
      "n:degree",
      "n:uncounted",
      "n:counted",
      "n:hub",
      "n:unknown",
      "n:counted",
    ];
    expect(connectCostFloor(state, all)).toBe(5 + 6 + 33);
    expect(connectCostFloor(state, all, { countedOnly: true })).toBe(5 + 33);
  });

  it("connects only small nodes with counted edges, cheapest first, within an edge budget", () => {
    const state = sized(
      [["n:d", degree(2, 3)]],
      [
        ["n:hub", { out: 0, in: threshold + 1 }],
        ["n:a", { out: 900, in: 900, edges: { out: 900, in: 900 } }],
        ["n:b", { out: 1, in: 1, edges: { out: 1, in: 1 } }],
        // Few neighbors, but 800 edges: budgeted by its edges, not its neighbors.
        ["n:c", { out: 4, in: 4, edges: { out: 400, in: 400 } }],
        ["n:par", { out: 2, in: 0, edges: { out: threshold + 1, in: 0 } }],
        ["n:uncounted", { out: 3, in: 3 }],
      ],
    );
    const plan = planConnect(
      state,
      [
        "n:hub",
        "n:a",
        "n:b",
        "n:c",
        "n:par",
        "n:uncounted",
        "n:d",
        "n:unknown",
      ],
      1000,
    );
    expect(plan.connect).toEqual(["n:b", "n:d", "n:c"]);
    expect(plan.skipped.sort()).toEqual([
      "n:a",
      "n:hub",
      "n:par",
      "n:uncounted",
      "n:unknown",
    ]);
    expect(plan.large.sort()).toEqual(["n:hub", "n:par"]);
    expect(plan.unknown.sort()).toEqual(["n:uncounted", "n:unknown"]);
    // The cheapest small node always fits, even over the budget.
    expect(planConnect(state, ["n:a"], 10).connect).toEqual(["n:a"]);

    // An explicit connect sends them all, in requests within the budget; the rest need asking.
    expect(
      connectBatches(
        state,
        ["n:hub", "n:a", "n:b", "n:c", "n:par", "n:uncounted", "n:d"],
        1000,
      ),
    ).toEqual({
      batches: [["n:b", "n:d", "n:c"], ["n:a"]],
      large: ["n:hub", "n:par"],
      unknown: ["n:uncounted"],
    });
  });

  it("keeps a node's edge counts when it is probed again, and only adds them to probed nodes", () => {
    let state = explorerReducer(initialExplorerState(), {
      type: "probe/loaded",
      probes: new Map([["n:1", { out: 2, in: 2 }]]),
    });
    state = explorerReducer(state, {
      type: "probe/edges",
      edges: new Map([
        ["n:1", { out: 7, in: 2 }],
        ["n:gone", { out: 1, in: 1 }],
      ]),
    });
    expect(state.probes.get("n:1")).toEqual({
      out: 2,
      in: 2,
      edges: { out: 7, in: 2 },
    });
    expect(state.probes.has("n:gone")).toBe(false);
    state = explorerReducer(state, {
      type: "probe/loaded",
      probes: new Map([["n:1", { out: 2, in: 3 }]]),
    });
    expect(state.probes.get("n:1")).toEqual({
      out: 2,
      in: 3,
      edges: { out: 7, in: 2 },
    });
  });

  it("writes floors with a plus", () => {
    expect(degreeText(1000, true)).toBe("1,000+");
    expect(degreeText(12)).toBe("12");
  });
});

describe("menuRelationships", () => {
  const nothing = { empty: { out: false, in: false } };
  const degree: DegreeInfo = {
    outDegree: 0,
    inDegree: 1,
    outByLabel: [],
    inByLabel: [{ label: "signed_with", count: 1 }],
  };

  it("goes by the degree when it is known, whatever the probe says", () => {
    const counted = { out: 0, in: 2, edges: { out: 0, in: 4 } };
    for (const edgesHidden of [false, true]) {
      expect(menuRelationships(degree, counted, edgesHidden)).toEqual({
        total: "1",
        out: "0",
        in: "1",
        empty: { out: true, in: false },
      });
    }
    expect(
      menuRelationships(
        {
          ...degree,
          outDegree: EXPLORER_LIMITS.degreeThreshold,
          atLeast: { out: true, in: false },
        },
        undefined,
        false,
      ),
    ).toEqual({
      total: "1,001+",
      out: "1,000+",
      in: "1",
      empty: { out: false, in: false },
    });
  });

  it("says what the probe can while every edge shows", () => {
    // Neighbors only: a small side's neighbor count isn't its edge count.
    expect(menuRelationships(undefined, { out: 0, in: 2 }, false)).toEqual({
      empty: { out: true, in: false },
    });
    expect(
      menuRelationships(
        undefined,
        { out: 0, in: 2, edges: { out: 0, in: 4 } },
        false,
      ),
    ).toEqual({
      total: "4",
      out: "0",
      in: "4",
      empty: { out: true, in: false },
    });
    expect(
      menuRelationships(
        undefined,
        { out: EXPLORER_LIMITS.degreeThreshold + 1, in: 3 },
        false,
      ),
    ).toEqual({
      total: "1,000+",
      out: "1,000+",
      empty: { out: false, in: false },
    });
    expect(menuRelationships(undefined, undefined, false)).toEqual(nothing);
  });

  it("says nothing from the probe while edges are hidden: it counts them too", () => {
    for (const probe of [
      { out: 0, in: 2, edges: { out: 0, in: 4 } },
      { out: 0, in: 0 },
      { out: EXPLORER_LIMITS.degreeThreshold + 1, in: 3 },
    ])
      expect(menuRelationships(undefined, probe, true)).toEqual(nothing);
  });
});

describe("totalsAreFloors", () => {
  it("says at least when labels were sampled or any listed label is uncounted, per side", () => {
    const labels = {
      nodes: [{ label: "A", count: 3 }],
      edges: [{ label: "R", count: null }],
    };
    const stats = {
      nodeLabels: [],
      edgeLabels: [],
      nodeTotal: 3,
      edgeTotal: 0,
      partial: { nodes: false, edges: false },
    };
    expect(totalsAreFloors(stats, labels)).toEqual({
      nodes: false,
      edges: true,
    });
    expect(
      totalsAreFloors(
        { ...stats, partial: { nodes: true, edges: false } },
        labels,
      ),
    ).toEqual({ nodes: true, edges: true });
    expect(totalsAreFloors(null, { nodes: [], edges: [] })).toEqual({
      nodes: false,
      edges: false,
    });
  });
});

describe("explorerReducer probes and label removal", () => {
  it("keeps degree probes per node", () => {
    const state = explorerReducer(initialExplorerState(), {
      type: "probe/loaded",
      probes: new Map([["n:1", { out: 2, in: 3 }]]),
    });
    expect(state.probes.get("n:1")).toEqual({ out: 2, in: 3 });
    expect(
      explorerReducer(state, { type: "probe/loaded", probes: new Map() }),
    ).toBe(state);
  });

  it("drops the offers to connect or expand nodes when the canvas is cleared", () => {
    let state = explorerReducer(initialExplorerState(), {
      type: "notice/add",
      notice: {
        id: "connect-skipped",
        tone: "info",
        message: "m",
        action: { kind: "connect", label: "Connect", nodeIds: ["n:1"] },
      },
    });
    state = explorerReducer(state, {
      type: "notice/add",
      notice: { id: "other", tone: "info", message: "kept" },
    });
    state = explorerReducer(state, { type: "canvas/clear" });
    expect(state.notices.map((notice) => notice.id)).toEqual(["other"]);
  });

  it("drops an uncounted label that a late count found empty", () => {
    let state = explorerReducer(initialExplorerState(), {
      type: "overview/loaded",
      stats: {
        nodeLabels: [
          { label: "Gone", count: null },
          { label: "User", count: 2 },
        ],
        edgeLabels: [],
        nodeTotal: 2,
        edgeTotal: 0,
        partial: { nodes: true, edges: true },
      },
    });
    state = explorerReducer(state, {
      type: "overview/merged",
      stats: { nodeLabels: [], edgeLabels: [], nodeTotal: 0, edgeTotal: 0 },
      gone: { nodes: ["Gone"] },
    });
    expect(state.overview.stats?.nodeLabels).toEqual([
      { label: "User", count: 2 },
    ]);
  });
});
