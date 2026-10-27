import { describe, expect, it } from "vitest";
import { runQuery } from "../../mock/graph/query";
import { MockGraph } from "../../mock/graph/store";
import {
  COMPARE_LIMITS,
  countRows,
  diffKeys,
  keysRequest,
  parseKeys,
} from "@/web/areas/explore/compare";
import {
  loadCountRequests,
  parseLoadLabelCounts,
} from "@/web/areas/explore/load-scope";

// Comparing two loads: counts per type and link from bitmaps, and a type's
// entities by key, read only for the type opened.

const keysOf = (db: MockGraph, load: string, type: string) => {
  const answer = runQuery(db, keysRequest(load, type));
  expect(answer.status).toBe(200);
  return parseKeys(answer.body);
};

describe("compare", () => {
  it("diffs a type's entities by key: added, removed and renamed", () => {
    const db = new MockGraph();
    const a = db.view("A"),
      b = db.view("B");
    for (const [key, name] of [
      ["c1", "globex"],
      ["c2", "initech"],
      ["c3", "hooli"],
    ])
      a.addNode("Customer", { entity_key: key, norm_name: name });
    for (const [key, name] of [
      ["c1", "globex"],
      ["c2", "initech llc"],
      ["c4", "umbrella"],
    ])
      b.addNode("Customer", { entity_key: key, norm_name: name });
    // A type known by its name.
    a.addNode("Contract", { norm_name: "msa" });
    b.addNode("Contract", { norm_name: "msa" });
    b.addNode("Contract", { norm_name: "nda" });
    // The team's own customer is in neither load.
    db.addNode("Customer", { entity_key: "c9", norm_name: "own" });

    const customers = diffKeys(
      keysOf(db, "A", "Customer"),
      keysOf(db, "B", "Customer"),
    )!;
    expect(customers.added.map((i) => i.value)).toEqual(["c4"]);
    expect(customers.removed.map((i) => i.value)).toEqual(["c3"]);
    expect(customers.changed).toEqual([
      { property: "entity_key", value: "c2", name: "initech llc" },
    ]);
    expect(customers.counts).toEqual({ added: 1, removed: 1, changed: 1 });
    const contracts = diffKeys(
      keysOf(db, "A", "Contract"),
      keysOf(db, "B", "Contract"),
    )!;
    expect(contracts.added).toEqual([
      { property: "norm_name", value: "nda", name: "nda" },
    ]);
    expect(contracts.removed).toEqual([]);
  });

  it("reads at most keysPerSide + 1 keys, from the type's bitmap in the load, and only counts past it", () => {
    const json = JSON.stringify(keysRequest("A", "Customer").query);
    expect(json).toContain(
      `"count":{"literal":${COMPARE_LIMITS.keysPerSide + 1}}`,
    );
    expect(json).toContain(
      '{"eq":{"left":{"property":"foundry_load"},"right":{"constant":{"string":"A"}}}}',
    );
    expect(json).toContain('"properties":["entity_key","norm_name"]');
    const capped = parseKeys({
      keys: Array.from({ length: COMPARE_LIMITS.keysPerSide + 1 }, (_, i) => ({
        entity_key: `k${i}`,
      })),
    });
    expect(capped.capped).toBe(true);
    expect(capped.items.size).toBe(COMPARE_LIMITS.keysPerSide);
    expect(diffKeys(capped, parseKeys({ keys: [] }))).toBeNull();
    expect(parseKeys(null)).toEqual({ items: new Map(), capped: false });
    // Lists stop at `listed`, the counts don't.
    const many = parseKeys({
      keys: Array.from({ length: 70 }, (_, i) => ({ entity_key: `k${i}` })),
    });
    const diff = diffKeys(parseKeys({ keys: [] }), many)!;
    expect(diff.added).toHaveLength(COMPARE_LIMITS.listed);
    expect(diff.counts.added).toBe(70);
  });

  it("counts each type and link in both loads, marking what one version lacks", () => {
    const db = new MockGraph();
    const a = db.view("A");
    const x = a.addNode("Customer", {}),
      y = a.addNode("Order", {});
    a.addEdge("placed", x, y);
    const b = db.view("B");
    b.addNode("Customer", {});
    b.addNode("Customer", {});
    const count = (load: string, nodes: string[], edges: string[]) => {
      const batches = loadCountRequests(load, { nodes, edges });
      return parseLoadLabelCounts(
        batches.map((batch) => runQuery(db, batch.request).body),
        batches,
      );
    };
    const rows = countRows(
      count("A", ["Customer", "Order"], ["placed"]),
      count("B", ["Customer", "Invoice"], []),
    );
    expect(rows).toEqual([
      { target: "node", label: "Customer", a: 1, b: 2, change: "both" },
      { target: "node", label: "Invoice", a: null, b: 0, change: "added" },
      { target: "node", label: "Order", a: 1, b: null, change: "removed" },
      { target: "edge", label: "placed", a: 1, b: null, change: "removed" },
    ]);
    // Over 60 labels: several requests.
    const wide = loadCountRequests("A", {
      nodes: Array.from({ length: 70 }, (_, i) => `T${i}`),
      edges: [],
    });
    expect(wide).toHaveLength(2);
  });
});
