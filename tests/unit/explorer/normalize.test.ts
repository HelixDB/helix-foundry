import { describe, expect, it } from "vitest";

import selfLoopFixture from "./__fixtures__/neighborhood-self-loop.json";
import {
  captionFor,
  classifyValue,
  edgeKey,
  extractGraphFromResult,
  nodeKey,
  toGraphEdge,
  toGraphEdges,
  toGraphNode,
  toGraphNodes,
  type ProbedFetchShape,
} from "@/web/areas/explore/explorer/lib/normalize";
import type {
  GraphNode,
  NodeFetchShape,
} from "@/web/areas/explore/explorer/lib/types";

const SHAPE: NodeFetchShape = {
  keys: ["name"],
  heavyKeys: ["embedding", "body"],
};

function node(
  properties: GraphNode["properties"],
  label = "User",
  dbId = 7,
): GraphNode {
  return {
    id: nodeKey(dbId),
    dbId,
    label,
    properties,
    omitted: [],
    complete: true,
  };
}

describe("keys", () => {
  it("keeps node and edge id spaces apart", () => {
    expect(nodeKey(0)).toBe("n:0");
    expect(edgeKey(0)).toBe("e:0");
  });
});

describe("toGraphNode", () => {
  it("strips virtual keys and records heavy keys the fetch left out", () => {
    expect(
      toGraphNode(
        { $id: 3, $label: "Doc", $score: 1.5, name: "a", body: "kept" },
        SHAPE,
      ),
    ).toEqual({
      id: "n:3",
      dbId: 3,
      label: "Doc",
      properties: { name: "a", body: "kept" },
      omitted: ["embedding"],
      complete: false,
    });
  });

  it("is complete for value_map rows", () => {
    const full = toGraphNode(
      { $id: 3, $label: "Doc", embedding: [1, 2] },
      { keys: null, heavyKeys: ["embedding"] },
    );
    expect(full).toMatchObject({ complete: true, omitted: [] });
  });

  it("uses per-label heavy keys when the shape has them", () => {
    const shape: ProbedFetchShape = {
      ...SHAPE,
      heavyByLabel: { Doc: ["body", "embedding"], User: [] },
    };
    expect(toGraphNode({ $id: 1, $label: "User" }, shape)?.omitted).toEqual([]);
    expect(toGraphNode({ $id: 2, $label: "Doc" }, shape)?.omitted).toEqual([
      "body",
      "embedding",
    ]);
    // Labels the probe never saw are not assumed to have anyone's heavy keys.
    expect(toGraphNode({ $id: 3, $label: "Other" }, shape)?.omitted).toEqual(
      [],
    );
  });

  it("lists a label's own light keys that a mixed-label fetch left out", () => {
    // description is long on Document, so the shared keys leave it out; it is light on Project.
    const shape: ProbedFetchShape = {
      keys: ["name", "title"],
      heavyKeys: ["description"],
      heavyByLabel: { Document: ["description"], Project: [] },
      keysByLabel: { Document: ["title"], Project: ["description", "name"] },
    };
    const sampled = toGraphNode(
      { $id: 1, $label: "Project", name: "Helix" },
      shape,
    );
    expect(sampled).toMatchObject({
      properties: { name: "Helix" },
      omitted: ["description"],
      complete: false,
    });
    // Label-scoped fetches (label pages, search) ask for the label's own keys.
    const paged = toGraphNode(
      { $id: 1, $label: "Project", name: "Helix", description: "Graph DB" },
      shape,
      {
        scoped: true,
      },
    );
    expect(paged?.omitted).toEqual([]);
    expect(
      toGraphNode({ $id: 2, $label: "Document", title: "T" }, shape)?.omitted,
    ).toEqual(["description"]);
  });

  it("lists every known key of the label when nothing but ids was fetched", () => {
    const light: ProbedFetchShape = {
      keys: [],
      heavyKeys: ["body"],
      edgeKeys: [],
      heavyByLabel: { Doc: ["body"] },
      keysByLabel: { Doc: ["title", "lang"] },
    };
    expect(toGraphNode({ $id: 1, $label: "Doc" }, light)?.omitted).toEqual([
      "body",
      "title",
      "lang",
    ]);
  });

  it("rejects edges, bad ids and missing labels", () => {
    expect(
      toGraphNode({ $id: 1, $label: "R", $from: 1, $to: 2 }, SHAPE),
    ).toBeNull();
    expect(toGraphNode({ $id: "1", $label: "A" }, SHAPE)).toBeNull();
    expect(toGraphNode({ $id: -1, $label: "A" }, SHAPE)).toBeNull();
    expect(toGraphNode({ $id: 1.5, $label: "A" }, SHAPE)).toBeNull();
    expect(toGraphNode({ $id: 1 }, SHAPE)).toBeNull();
    expect(toGraphNode([1], SHAPE)).toBeNull();
    expect(toGraphNode(null, SHAPE)).toBeNull();
  });

  it("dedupes rows and accepts a single at-most-one row or null", () => {
    expect(
      toGraphNodes(
        [{ $id: 1, $label: "A" }, { $id: 1, $label: "A" }, 4],
        SHAPE,
      ),
    ).toHaveLength(1);
    expect(toGraphNodes({ $id: 1, $label: "A" }, SHAPE)).toHaveLength(1);
    expect(toGraphNodes(null, SHAPE)).toEqual([]);
  });
});

describe("toGraphEdge", () => {
  it("reads endpoints from $from/$to", () => {
    expect(
      toGraphEdge({
        $id: 4,
        $label: "FOLLOWS",
        $from: 1,
        $to: 0,
        since: "2024-03-09T16:00:00.000Z",
      }),
    ).toEqual({
      id: "e:4",
      dbId: 4,
      label: "FOLLOWS",
      source: "n:1",
      target: "n:0",
      properties: { since: "2024-03-09T16:00:00.000Z" },
    });
  });

  it("accepts SDK-style $from.$id and nested endpoint objects", () => {
    expect(
      toGraphEdge({ $id: 1, $label: "R", "$from.$id": 2, "$to.$id": 3 }),
    ).toMatchObject({ source: "n:2", target: "n:3" });
    expect(
      toGraphEdge({ $id: 1, $label: "R", $from: { $id: 2 }, $to: { $id: 3 } }),
    ).toMatchObject({ source: "n:2", target: "n:3" });
  });

  it("needs both endpoints", () => {
    // value_map on edges has no endpoints; such rows cannot be drawn.
    expect(toGraphEdge({ $id: 1, $label: "R" })).toBeNull();
    expect(toGraphEdge({ $id: 1, $label: "R", $from: 2 })).toBeNull();
  });

  it("dedupes across sources", () => {
    const row = { $id: 1, $label: "R", $from: 2, $to: 2 };
    expect(toGraphEdges([row], [row, { nope: true }])).toHaveLength(1);
  });
});

describe("extractGraphFromResult", () => {
  it("collects nodes and edges from a real neighborhood body", () => {
    const result = extractGraphFromResult(selfLoopFixture.response)!;
    const ids = new Set(result.graph.nodes.map((entry) => entry.id));
    expect(ids.has("n:97")).toBe(true);
    expect(result.graph.edges.length).toBeGreaterThan(0);
    expect(result.missingNodeDbIds).toEqual([]);
    for (const entry of result.graph.nodes) expect(entry.complete).toBe(false);
  });

  it("walks nested rows, merges duplicate nodes and reports missing endpoints", () => {
    const body = {
      people: [{ $id: 1, $label: "User", name: "Ada" }],
      nested: [[{ wrapper: { $id: 1, $label: "User", age: 36 } }]],
      edges: [{ $id: 9, $label: "KNOWS", $from: 1, $to: 2 }],
      count: 3,
    };
    const result = extractGraphFromResult(body)!;
    expect(result.graph.nodes).toEqual([
      expect.objectContaining({
        id: "n:1",
        properties: { name: "Ada", age: 36 },
      }),
    ]);
    expect(result.graph.edges.map((edge) => edge.id)).toEqual(["e:9"]);
    expect(result.missingNodeDbIds).toEqual([2]);
  });

  it("skips edge rows without endpoints instead of turning them into nodes", () => {
    // value_map over edges: no $from/$to, and edge ids overlap node ids.
    const body = {
      rows: [
        { $id: 0, $label: "WORKS_AT", role: "Engineer", since: 2018 },
        { $id: 37, $label: "WORKS_AT", role: "CTO" },
        { $id: 5, $label: "User", name: "Ada" },
      ],
    };
    const result = extractGraphFromResult(body, {
      nodeLabels: ["User", "Company"],
      edgeLabels: ["WORKS_AT"],
    })!;
    expect(result.graph.nodes.map((entry) => entry.id)).toEqual(["n:5"]);
    expect(result.skippedEdgeRows).toBe(2);
    // Only edge rows: nothing to draw, but the caller learns why.
    const edgesOnly = extractGraphFromResult(
      { rows: body.rows.slice(0, 2) },
      { edgeLabels: ["WORKS_AT"] },
    )!;
    expect(edgesOnly.graph).toEqual({ nodes: [], edges: [] });
    expect(edgesOnly.skippedEdgeRows).toBe(2);
    // Without known labels such a row still parses as a node; the hook checks it against the database.
    expect(
      extractGraphFromResult({ rows: body.rows.slice(0, 1) })?.graph.nodes,
    ).toHaveLength(1);
  });

  it("keeps the first of two rows with one id and different labels", () => {
    const body = {
      a: [{ $id: 3, $label: "User", name: "Ada" }],
      b: [{ $id: 3, $label: "KNOWS", since: 1 }],
    };
    expect(extractGraphFromResult(body)!.graph.nodes).toEqual([
      expect.objectContaining({
        id: "n:3",
        label: "User",
        properties: { name: "Ada" },
      }),
    ]);
  });

  it("returns null when nothing is graph-like", () => {
    expect(
      extractGraphFromResult({ n: 3, ids: [1, 2], rows: [{ $id: 1 }] }),
    ).toBeNull();
    expect(extractGraphFromResult("text")).toBeNull();
    expect(extractGraphFromResult(null)).toBeNull();
  });
});

describe("captionFor", () => {
  it("prefers the chosen key, then common naming keys", () => {
    expect(captionFor(node({ name: "Ada", email: "ada@x.io" }), "email")).toBe(
      "ada@x.io",
    );
    expect(captionFor(node({ email: "ada@x.io", Title: "Dr" }))).toBe("Dr");
    expect(captionFor(node({ username: "ada", age: 3 }))).toBe("ada");
    expect(captionFor(node({ name: 42 }))).toBe("User #7");
    expect(captionFor(node({ name: "Ada" }), "missing")).toBe("Ada");
  });

  it("falls back to id-like strings, then label and id", () => {
    expect(
      captionFor(node({ kind: "deploy", event_id: "evt_01" }, "Event")),
    ).toBe("evt_01");
    expect(captionFor(node({ orderId: "A-1" }))).toBe("A-1");
    expect(captionFor(node({ count: 3 }, "Tag", 12))).toBe("Tag #12");
  });

  it("skips long values and truncates long captions", () => {
    expect(captionFor(node({ name: "x".repeat(500), slug: "short" }))).toBe(
      "short",
    );
    const caption = captionFor(node({ title: "word ".repeat(20) }));
    expect(caption.length).toBeLessThanOrEqual(48);
    expect(caption.endsWith("…")).toBe(true);
    expect(captionFor(node({ name: "  spaced \n out  " }))).toBe("spaced out");
  });
});

describe("classifyValue", () => {
  it.each([
    [null, "null"],
    [true, "boolean"],
    [3, "integer"],
    [-42, "integer"],
    [0.5, "float"],
    ["hello", "string"],
    ["2023-11-14T22:13:20.000Z", "datetime"],
    ["2023-11-14T22:13:20+02:00", "datetime"],
    ["2023-11-14", "string"],
    ["2023-13-45T99:99:99Z", "string"],
    [[1, 2, 3], "array"],
    [Array.from({ length: 33 }, (_, index) => index / 10), "vector"],
    [Array.from({ length: 32 }, () => 0.1), "array"],
    [["a", 1, false], "array"],
    [{ nested: { deep: 7 } }, "object"],
  ] as const)("%j is %s", (value, kind) => {
    expect(classifyValue(value as never)).toBe(kind);
  });
});
