import { describe, expect, it } from "vitest";
import { runQuery } from "../../mock/graph/query";
import { MockGraph } from "../../mock/graph/store";

// The mock's order_by sorts as Helix does: a missing value first ascending
// and last descending, ties in id order whichever the direction.

const graph = () => {
  const g = new MockGraph();
  g.addNode("T", { name: "b", n: 2 });
  g.addNode("T", { name: "a", n: 2 });
  g.addNode("T", { n: 1 });
  g.addNode("T", { name: "c" });
  g.addNode("U", { name: "z" });
  return g;
};
const read = (root: unknown) => ({
  request_type: "read",
  query: {
    read: { entries: [{ query: { name: "r", root } }], returns: ["r"] },
  },
});
const ofT = {
  nodes_where: {
    predicate: {
      eq: {
        left: { property: "$label" },
        right: { constant: { string: "T" } },
      },
    },
  },
};
const ids = (body: unknown) =>
  ((body as { r: { $id: number }[] }).r || []).map((r) => r.$id);

describe("mock order_by", () => {
  it("sorts by one property, missing first ascending and last descending", () => {
    const g = graph();
    const by = (order: string) =>
      ids(
        runQuery(
          g,
          read({
            values: {
              input: { order_by: { input: ofT, property: "name", order } },
              properties: ["$id"],
            },
          }),
        ).body,
      );
    expect(by("asc")).toEqual([2, 1, 0, 3]);
    expect(by("desc")).toEqual([3, 0, 1, 2]);
  });

  it("breaks ties by the next key, then by id", () => {
    const g = graph();
    const by = (orderings: [string, string][]) =>
      ids(
        runQuery(
          g,
          read({
            values: {
              input: {
                range: {
                  input: { order_by_multiple: { input: ofT, orderings } },
                  start: { literal: 0 },
                  end: { literal: 3 },
                },
              },
              properties: ["$id"],
            },
          }),
        ).body,
      );
    expect(
      by([
        ["n", "desc"],
        ["name", "asc"],
      ]),
    ).toEqual([1, 0, 2]);
    // Equal keys keep id order, descending too.
    expect(by([["n", "desc"]])).toEqual([0, 1, 2]);
  });

  it("refuses an ordering Helix wouldn't decode", () => {
    for (const root of [
      { order_by: { input: ofT, property: "name", order: "up" } },
      { order_by_multiple: { input: ofT, orderings: [] } },
      {
        order_by_multiple: {
          input: ofT,
          orderings: [
            ["n", "asc"],
            ["n", "desc"],
          ],
        },
      },
    ])
      expect(runQuery(graph(), read(root)).status).toBe(400);
  });
});
