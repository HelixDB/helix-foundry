import { describe, expect, it } from "vitest";
import { runQuery } from "../../../mock/graph/query";
import { MockGraph, type LoadGraph } from "../../../mock/graph/store";
import type {
  DatabaseQueryResult,
  DynamicQueryRequest,
} from "@/web/areas/explore/database-query";
import { EXPLORER_LIMITS } from "@/web/areas/explore/explorer/lib/queries";
import {
  candidateShares,
  chooseSeeds,
  connectable,
  edgeCountPlan,
  hubTypes,
  moreRing,
  newSeedingState,
  planRing,
  ringSides,
  SEEDING,
  seedView,
  type SeedingIO,
  type VersionLayout,
} from "@/web/areas/explore/explorer/lib/seeding";
import type { SeedProbe } from "@/web/areas/explore/explorer/lib/queries";
import type {
  GraphEdge,
  GraphNode,
} from "@/web/areas/explore/explorer/lib/types";
import type { Scope } from "@/web/areas/explore/load-scope";

// A load's first view (lib/seeding.ts) over the mock database: a
// hub-and-spoke shop where customers place orders of shared products and
// invoices bill customers. The view must come out as one connected graph,
// with no node left alone when it has neighbors, within its budget, inside
// the load, from bounded reads only.

const LAYOUT: VersionLayout = {
  types: ["Customer", "Order", "Product", "Invoice"],
  links: [
    { name: "placed", from: "Customer", to: "Order" },
    { name: "contains", from: "Order", to: "Product" },
    { name: "billed", from: "Invoice", to: "Customer" },
  ],
};
const L1: Scope = { kind: "load", id: "L1" };

/** The shop, written by one load; `customers` hubs with their orders and invoices. */
function shop(g: LoadGraph, customers = 6, ordersEach = 6) {
  const products = Array.from({ length: 10 }, (_, i) =>
    g.addNode("Product", { entity_key: `p${i}`, name: `Product ${i}` }),
  );
  for (let c = 0; c < customers; c++) {
    const customer = g.addNode("Customer", {
      entity_key: `c${c}`,
      name: `Customer ${c}`,
    });
    for (let o = 0; o < ordersEach; o++) {
      const order = g.addNode("Order", { entity_key: `o${c}-${o}` });
      g.addEdge("placed", customer, order);
      g.addEdge("contains", order, products[(c + o) % products.length]!);
      g.addEdge(
        "contains",
        order,
        products[(c * 3 + o + 1) % products.length]!,
      );
      const invoice = g.addNode("Invoice", { entity_key: `i${c}-${o}` });
      g.addEdge("billed", invoice, customer);
    }
  }
}

/** The explorer's side of the seeding, over a mock graph: a canvas and the requests sent. */
function harness(db: MockGraph) {
  const nodes = new Map<string, GraphNode>(),
    edges = new Map<string, GraphEdge>();
  const requests: DynamicQueryRequest[] = [];
  const steps: string[] = [];
  const io: SeedingIO = {
    read: async (_title, request) => {
      requests.push(request);
      const answer = runQuery(db, request);
      expect(answer.status, JSON.stringify(answer.body)).toBe(200);
      return {
        ok: true,
        status: 200,
        contentType: null,
        body: answer.body,
        bytes: 0,
        elapsedMs: 0,
      } satisfies DatabaseQueryResult;
    },
    place: async (seeds) => {
      steps.push(`place:${seeds.length}`);
      for (const n of seeds) nodes.set(n.id, n);
    },
    merge: (graph) => {
      steps.push("merge");
      const added = (graph.nodes ?? []).filter((n) => !nodes.has(n.id));
      for (const n of added) nodes.set(n.id, n);
      for (const e of graph.edges ?? [])
        if (nodes.has(e.source) && nodes.has(e.target)) edges.set(e.id, e);
      return added;
    },
    probed: () => {},
    onCanvas: () => [...nodes.values()].map((n) => n.dbId),
    failed: (title) => {
      throw new Error(`failed: ${title}`);
    },
  };
  return { io, nodes, edges, requests, steps };
}

/** The canvas's connected components, by node id. */
function components(
  nodes: Map<string, GraphNode>,
  edges: Map<string, GraphEdge>,
) {
  const next = new Map<string, string[]>();
  for (const e of edges.values()) {
    next.set(e.source, [...(next.get(e.source) ?? []), e.target]);
    next.set(e.target, [...(next.get(e.target) ?? []), e.source]);
  }
  const seen = new Set<string>();
  const out: string[][] = [];
  for (const id of nodes.keys()) {
    if (seen.has(id)) continue;
    const part: string[] = [];
    const queue = [id];
    seen.add(id);
    while (queue.length) {
      const at = queue.shift()!;
      part.push(at);
      for (const other of next.get(at) ?? [])
        if (!seen.has(other)) {
          seen.add(other);
          queue.push(other);
        }
    }
    out.push(part);
  }
  return out;
}

const operations = (value: unknown, found: Record<string, unknown>[] = []) => {
  if (Array.isArray(value)) value.forEach((v) => operations(v, found));
  else if (value && typeof value === "object") {
    found.push(value as Record<string, unknown>);
    for (const child of Object.values(value)) operations(child, found);
  }
  return found;
};

/** Nodes a request probes the size of (none for any other read). */
const probedNodes = (request: DynamicQueryRequest) =>
  new Set(JSON.stringify(request).match(/"probe_out_\d+"/g) ?? []).size;

describe("a load's first view", () => {
  it("is one connected graph from the hub types, inside the load and its budget", async () => {
    const db = new MockGraph();
    shop(db.view("L1"));
    // Another load of the same version, and the team's own data.
    shop(db.view("L2"), 2, 2);
    // A customer with no orders or invoices, only Foundry's own evidence
    // about it: nothing a ring around it would show.
    const lonely = db
      .view("L1")
      .addNode("Customer", { entity_key: "c-lonely", name: "Lonely" });
    for (let i = 0; i < 5; i++)
      db.view("L1").addEdge(
        "FOUNDRY_ABOUT",
        db.view("L1").addNode("FoundryEvidence", { row_key: `r${i}` }),
        lonely,
      );
    const note = db.addNode("Note", { text: "call c0" });
    db.addEdge(
      "about",
      note,
      db.view("L1").find("Customer", "entity_key", "c0")!.id,
    );

    const { io, nodes, edges, requests, steps } = harness(db);
    const state = newSeedingState();
    const outcome = await seedView(io, state, {
      scope: L1,
      layout: LAYOUT,
      holding: new Set(LAYOUT.types),
      shape: { keys: null, heavyKeys: [] },
    });
    expect(outcome).toEqual({ kind: "seeded" });

    // Seeds go on the canvas before anything else lands.
    expect(steps[0]).toMatch(/^place:\d+$/);
    expect(steps.length).toBeGreaterThan(1);

    // The lonely customer isn't a seed: its neighbors are Foundry's.
    expect(
      [...nodes.values()].some((n) => n.properties.entity_key === "c-lonely"),
    ).toBe(false);
    // Inside the load, nothing of the other load's or the team's own.
    for (const n of nodes.values())
      expect(n.properties.foundry_load).toBe("L1");
    expect(nodes.size).toBeLessThanOrEqual(SEEDING.budget);
    expect(nodes.size).toBeGreaterThan(SEEDING.seeds);

    // One component, and no node left alone.
    expect(components(nodes, edges)).toHaveLength(1);
    const touched = new Set(
      [...edges.values()].flatMap((e) => [e.source, e.target]),
    );
    for (const id of nodes.keys()) expect(touched.has(id), id).toBe(true);

    // Every read was bounded and scoped: no whole-graph source, and every
    // label source carries the load.
    for (const request of requests)
      for (const op of operations(request.query)) {
        expect(JSON.stringify(op.nodes ?? op.edges ?? null)).not.toContain(
          '"reference":"all"',
        );
        const source = (op.nodes_where ?? op.edges_where) as
          | { predicate: unknown }
          | undefined;
        if (source)
          expect(JSON.stringify(source.predicate)).toContain(
            '{"eq":{"left":{"property":"foundry_load"},"right":{"constant":{"string":"L1"}}}}',
          );
      }

    // Connects are planned from counted edges, within the edge budget, and
    // edges are counted only for nodes probed small.
    const connects = requests.filter(
      (r) => r.query_name === "explorer_connect",
    );
    expect(connects.length).toBeGreaterThan(0);
    for (const r of connects) {
      const ids = (r.parameters as { new_ids: number[] }).new_ids;
      const edges = ids.map((id) => state.probes.get(id)?.edges);
      for (const e of edges) expect(e).toBeDefined();
      expect(
        edges.reduce((n, e) => n + (e ? e.out + e.in : 0), 0),
      ).toBeLessThanOrEqual(EXPLORER_LIMITS.connectEdgeBudget);
    }
    const counted = requests.filter(
      (r) => r.query_name === "explorer_edge_count",
    );
    expect(counted.length).toBeGreaterThan(0);
    for (const r of counted)
      for (const [, id] of JSON.stringify(r).matchAll(/"ids":\[(\d+)\]/g))
        expect(state.probes.get(Number(id))).toBeDefined();

    // Sizes are probed a few nodes at a time.
    const probed = requests.map(probedNodes).filter((n) => n > 0);
    expect(probed.length).toBeGreaterThan(1);
    for (const n of probed) expect(n).toBeLessThanOrEqual(SEEDING.probeBatch);

    // Show more reads the next ring, still inside the load.
    const before = nodes.size;
    const ring = await moreRing(
      io,
      state,
      LAYOUT,
      { keys: null, heavyKeys: [] },
      200,
    );
    expect(ring!.length).toBeGreaterThan(0);
    expect(nodes.size).toBe(before + ring!.length);
    for (const n of nodes.values())
      expect(n.properties.foundry_load).toBe("L1");
    expect(components(nodes, edges)).toHaveLength(1);
  });

  it("starts with no seeds when no hub type holds anything, and never reads around a large node", async () => {
    const db = new MockGraph();
    const { io } = harness(db);
    expect(
      await seedView(io, newSeedingState(), {
        scope: L1,
        layout: LAYOUT,
        holding: new Set(),
        shape: { keys: null, heavyKeys: [] },
      }),
    ).toEqual({ kind: "none" });

    const big: SeedProbe = {
      out: EXPLORER_LIMITS.degreeThreshold + 1,
      in: 0,
      linked: 5,
    };
    const node = (dbId: number, label = "Customer"): GraphNode => ({
      id: `n:${dbId}`,
      dbId,
      label,
      properties: {},
      omitted: [],
      complete: true,
    });
    const probes = new Map<number, SeedProbe>([
      [1, big],
      [2, { out: 3, in: 1, linked: 4, edges: { out: 3, in: 1 } }],
      [3, { out: 0, in: 0, linked: 0, edges: { out: 0, in: 0 } }],
      // Few neighbors, but parallel edges make it large.
      [
        5,
        {
          out: 2,
          in: 0,
          linked: 2,
          edges: { out: EXPLORER_LIMITS.degreeThreshold + 1, in: 0 },
        },
      ],
      // Small, its edges not counted yet.
      [6, { out: 2, in: 0, linked: 2 }],
    ]);
    expect(chooseSeeds([node(1), node(2), node(3), node(4)], probes)).toEqual([
      node(2),
      node(3),
    ]);
    const all = [1, 2, 3, 5, 6].map((id) => node(id));
    expect(
      planRing(all, probes, new Set(), LAYOUT, 8, 100).map((r) => r.dbId),
    ).toEqual([2]);
    expect(connectable(all, probes)).toEqual([2]);
    // The connect budget counts edges, not neighbors.
    expect(connectable([node(2)], probes, 3)).toEqual([]);
    const parallel = new Map<number, SeedProbe>([
      [7, { out: 1, in: 0, linked: 1, edges: { out: 40, in: 0 } }],
    ]);
    expect(connectable([node(7)], parallel, 39)).toEqual([]);
    expect(connectable([node(7)], parallel, 40)).toEqual([7]);
    // Only probed small nodes not counted yet are counted; one with no
    // neighbors needs no request.
    expect(
      edgeCountPlan(
        [...all, node(4), node(6), node(8)],
        new Map([...probes, [8, { out: 0, in: 0, linked: 0 }]]),
      ),
    ).toEqual({ batches: [[6]], empty: [8] });
  });

  it("counts edges in bounded batches", () => {
    const node = (dbId: number): GraphNode => ({
      id: `n:${dbId}`,
      dbId,
      label: "Order",
      properties: {},
      omitted: [],
      complete: true,
    });
    const ids = Array.from({ length: 120 }, (_, i) => i + 1);
    const plan = edgeCountPlan(
      ids.map(node),
      new Map(ids.map((id) => [id, { out: 1, in: 1, linked: 2 }])),
    );
    expect(plan.batches.map((b) => b.length)).toEqual([50, 50, 20]);
    // Neighbors summed stay within the budget, a lone node over it alone.
    const heavy = new Map(
      [1, 2, 3].map((id) => [
        id,
        { out: 1000, in: 1000, linked: 1 } satisfies SeedProbe,
      ]),
    );
    expect(edgeCountPlan([1, 2, 3].map(node), heavy).batches).toEqual([
      [1, 2],
      [3],
    ]);
  });
});

describe("a load's first view when reads go wrong", () => {
  const shape = { keys: null, heavyKeys: [] };
  const opts = {
    scope: L1,
    layout: LAYOUT,
    holding: new Set(LAYOUT.types),
    shape,
  };
  /** The harness, its reads cut off (null) or failed from the nth on. */
  function cut(db: MockGraph, n: number, how: "stale" | "fail") {
    const h = harness(db);
    const failures: string[] = [];
    let calls = 0;
    const read = h.io.read;
    h.io.read = async (title, request) => {
      calls += 1;
      if (calls < n) return read(title, request);
      if (how === "stale") return null;
      return {
        ok: false,
        kind: "database",
        status: 500,
        code: null,
        message: "boom",
        elapsedMs: 0,
      };
    };
    h.io.failed = (title) => failures.push(title);
    return { ...h, failures };
  }
  const shopDb = () => {
    const db = new MockGraph();
    shop(db.view("L1"));
    return db;
  };

  it("stops quietly when a newer boot takes over, at any step", async () => {
    for (let n = 1; n <= 6; n++) {
      const { io } = cut(shopDb(), n, "stale");
      expect(await seedView(io, newSeedingState(), opts), `read ${n}`).toEqual({
        kind: "stale",
      });
    }
    // Show more, cut off after its first read.
    const db = shopDb();
    const later = cut(db, 1000, "stale");
    const state = newSeedingState();
    expect(await seedView(later.io, state, opts)).toEqual({ kind: "seeded" });
    let reads = 0;
    later.io.read = async () => {
      reads += 1;
      return null;
    };
    expect(await moreRing(later.io, state, LAYOUT, shape, 100)).toBeNull();
    expect(reads).toBe(1);
  });

  it("fails the first view only when its first read fails; later failures are reported", async () => {
    const first = cut(shopDb(), 1, "fail");
    expect(await seedView(first.io, newSeedingState(), opts)).toMatchObject({
      kind: "failed",
      failure: { message: "boom" },
    });
    // The probes failing leaves sizes unknown: no seed is small, so the
    // candidates stand alone and nothing is read around them.
    const probes = cut(shopDb(), 2, "fail");
    expect(await seedView(probes.io, newSeedingState(), opts)).toEqual({
      kind: "seeded",
    });
    expect(probes.steps).toEqual([`place:${SEEDING.seeds}`]);
    // A ring failing is reported, and the view keeps what it has.
    const clean = harness(shopDb());
    await seedView(clean.io, newSeedingState(), opts);
    const ringAt =
      clean.requests.findIndex((r) => JSON.stringify(r).includes('"ring_')) + 1;
    const ring = cut(shopDb(), ringAt, "fail");
    expect(await seedView(ring.io, newSeedingState(), opts)).toEqual({
      kind: "seeded",
    });
    expect(ring.failures[0]).toMatch(/^Load the neighbors of/);
  });

  it("stops probing a ring at a slow batch, and reads around only what it probed", async () => {
    const h = harness(shopDb());
    const read = h.io.read;
    h.io.read = async (title, request) => {
      const result = await read(title, request);
      // Every probe batch runs slow, as one meeting hubs would.
      return result && probedNodes(request) > 0
        ? { ...result, elapsedMs: 2000 }
        : result;
    };
    const state = newSeedingState();
    expect(await seedView(h.io, state, opts)).toEqual({ kind: "seeded" });
    // One batch for the candidates, one for the first ring.
    expect(h.requests.filter((r) => probedNodes(r) > 0)).toHaveLength(2);
    expect(state.probes.size).toBeLessThanOrEqual(2 * SEEDING.probeBatch);
    // The seeds come from the probed candidates, and only probed nodes
    // were read around.
    for (const id of state.expanded) expect(state.probes.has(id)).toBe(true);
  });

  it("has no seeds when the hub types hold nothing in the load", async () => {
    const db = new MockGraph();
    shop(db.view("L2"));
    const { io } = harness(db);
    expect(await seedView(io, newSeedingState(), opts)).toEqual({
      kind: "none",
    });
  });
});

describe("planning", () => {
  it("ranks hub types by link ends and splits the candidates over them", () => {
    expect(hubTypes(LAYOUT)).toEqual([
      "Customer",
      "Order",
      "Invoice",
      "Product",
    ]);
    expect(candidateShares(LAYOUT)).toEqual([
      { type: "Customer", take: 16 },
      { type: "Order", take: 16 },
      { type: "Invoice", take: 16 },
    ]);
    expect(candidateShares({ types: ["A"], links: [] })).toEqual([
      { type: "A", take: 48 },
    ]);
    expect(candidateShares({ types: [], links: [] })).toEqual([]);
  });

  it("spreads a node's neighbors over its links, at least one each", () => {
    expect(ringSides(LAYOUT, "Customer", 8)).toEqual([
      { label: "placed", side: "out", take: 4 },
      { label: "billed", side: "in", take: 4 },
    ]);
    expect(ringSides(LAYOUT, "Order", 3)).toEqual([
      { label: "placed", side: "in", take: 2 },
      { label: "contains", side: "out", take: 2 },
    ]);
    expect(ringSides(LAYOUT, "Nope", 8)).toEqual([]);
  });

  it("takes seeds in turn from each type, the best connected first", () => {
    const node = (dbId: number, label: string): GraphNode => ({
      id: `n:${dbId}`,
      dbId,
      label,
      properties: {},
      omitted: [],
      complete: true,
    });
    const probes = new Map<number, SeedProbe>([
      [1, { out: 1, in: 0, linked: 1 }],
      [2, { out: 5, in: 0, linked: 5 }],
      [3, { out: 2, in: 0, linked: 2 }],
      [4, { out: 9, in: 0, linked: 9 }],
    ]);
    const seeds = chooseSeeds(
      [node(1, "A"), node(2, "A"), node(3, "B"), node(4, "B")],
      probes,
      3,
    );
    expect(seeds.map((n) => n.dbId)).toEqual([2, 4, 1]);
  });

  it("stops a ring at its budget", () => {
    const node = (dbId: number): GraphNode => ({
      id: `n:${dbId}`,
      dbId,
      label: "Customer",
      properties: {},
      omitted: [],
      complete: true,
    });
    const probes = new Map<number, SeedProbe>(
      [1, 2, 3].map((id) => [
        id,
        { out: 10, in: 10, linked: 20, edges: { out: 10, in: 10 } },
      ]),
    );
    // Each customer adds up to 8 (4 per link): two fit in 16.
    expect(
      planRing(
        [node(1), node(2), node(3)],
        probes,
        new Set([3]),
        LAYOUT,
        8,
        16,
      ).map((r) => r.dbId),
    ).toEqual([1, 2]);
  });
});
