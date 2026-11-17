import { expect, it } from "vitest";
import * as extraction from "../../mock/graph/extract";
import * as store from "../../mock/graph/store";
import { scopeOf } from "../../mock/graph/scope";
import { FOUNDRY_NODES as N } from "@/shared/foundry-labels";

it.each(["two", undefined])(
  "a decision for a full name overrides both name and alias matches (%s)",
  (entityKey) => {
    const g = new store.MockGraph().view("load");
    extraction.ingestChunk(
      g,
      {
        source: "table",
        mapping: "customers",
        snapshot: "snap",
        ontologyVersion: "v",
      },
      [
        {
          type: "Customer",
          key: "one",
          name: "Alpha Corporation",
          props: { company_name: "Alpha Corporation" },
          names: ["company_name"],
        },
        {
          type: "Customer",
          key: "two",
          name: "Beta Corporation",
          props: { company_name: "Beta Corporation" },
          names: ["company_name"],
        },
      ],
    );
    extraction.extractDocument(
      g,
      {
        id: "doc",
        name: "contract.txt",
        text: "Alpha Corporation signed. Alpha renewed.",
        markdown: false,
        ontologyVersion: "v",
      },
      {
        types: new Set(["Customer"]),
        names: new Map([["Customer", "company_name"]]),
      },
      scopeOf({ Customer: {} }, []),
      [
        {
          kind: "resolve",
          type: "Customer",
          docId: "doc",
          normName: "alpha corporation",
          ...(entityKey ? { entityKey } : {}),
        },
      ],
    );
    const mentions = [...g.nodes()].filter((n) => n.label === N.Mention);
    expect(mentions.length).toBe(entityKey ? 2 : 0);
    for (const m of mentions) {
      expect(m.props.resolved_by).toBe("user");
      const edge = [...g.graph.edges.values()].find(
        (e) => e.from === m.id && e.label === "FOUNDRY_MENTION_OF",
      )!;
      expect(g.node(edge.to)?.props.entity_key).toBe("two");
    }
  },
);

// Contract 4.22.0: another document's resolve that chose an entity links a
// name this one leaves for a person (an alias), and each name decision says
// what came of it.
it("carries another document's choice to a name, and says what came of each", () => {
  const g = new store.MockGraph().view("load");
  extraction.ingestChunk(
    g,
    {
      source: "table",
      mapping: "customers",
      snapshot: "snap",
      ontologyVersion: "v",
    },
    [
      {
        type: "Customer",
        key: "one",
        name: "Alpha Corporation",
        props: { company_name: "Alpha Corporation" },
        names: ["company_name"],
      },
      {
        type: "Customer",
        key: "two",
        name: "Beta Corporation",
        props: { company_name: "Beta Corporation" },
        names: ["company_name"],
      },
    ],
  );
  const n = extraction.normName;
  const resolve = (
    id: string,
    docId: string,
    name: string,
    entityKey?: string,
    decidedAt = "2026-11-01T00:00:00Z",
  ) => ({
    kind: "resolve" as const,
    id,
    type: "Customer",
    docId,
    normName: n(name),
    decidedAt,
    ...(entityKey ? { entityKey } : {}),
  });
  const out = extraction.extractDocument(
    g,
    {
      id: "doc",
      name: "contract.txt",
      text: "Gamma Holdings signed, and Delta Holdings signed, and Eta Holdings paid, and Theta Holdings paid, and Iota Holdings paid, and Kappa Holdings paid.",
      markdown: false,
      ontologyVersion: "v",
    },
    {
      types: new Set(["Customer"]),
      names: new Map([["Customer", "company_name"]]),
    },
    scopeOf({ Customer: {} }, []),
    [
      // Two documents chose one entity: one alias, the earlier.
      resolve("d2", "other2", "Gamma Holdings", "one", "2026-11-02T00:00:00Z"),
      resolve("d1", "other", "Gamma Holdings", "one"),
      // Two chose different entities: neither carries.
      resolve("d3", "x", "Delta Holdings", "one"),
      resolve("d4", "y", "Delta Holdings", "two"),
      // Its own: one it doesn't name, one written, one whose entity isn't
      // in the load.
      resolve("d5", "doc", "Epsilon Holdings", "two"),
      resolve("d6", "doc", "Eta Holdings", "two"),
      resolve("d7", "doc", "Theta Holdings", "nine"),
      // Carried to an entity the load doesn't hold.
      resolve("d8", "z", "Iota Holdings", "nine"),
      // Not in data never carries.
      resolve("d9", "w", "Kappa Holdings"),
    ],
  );
  if (out.kind !== "ingested") throw new Error(out.kind);
  expect(out.carried).toBe(1);
  expect(out.names).toEqual([
    {
      decision: "resolve",
      type: "Customer",
      normName: n("Epsilon Holdings"),
      decisionId: "d5",
      outcome: "not_named",
    },
    {
      decision: "resolve",
      type: "Customer",
      normName: n("Eta Holdings"),
      decisionId: "d6",
      outcome: "written",
    },
    {
      decision: "resolve",
      type: "Customer",
      normName: n("Theta Holdings"),
      decisionId: "d7",
      outcome: "refused",
      reason: "absent",
    },
    {
      decision: "alias",
      type: "Customer",
      normName: n("Gamma Holdings"),
      decisionId: "d1",
      outcome: "written",
    },
    ...["d3", "d4"].map((decisionId) => ({
      decision: "alias",
      type: "Customer",
      normName: n("Delta Holdings"),
      decisionId,
      outcome: "refused",
      reason: "two_rows",
    })),
    {
      decision: "alias",
      type: "Customer",
      normName: n("Iota Holdings"),
      decisionId: "d8",
      outcome: "refused",
      reason: "absent",
    },
  ]);
  const mentions = [...g.nodes()].filter((m) => m.label === N.Mention);
  expect(
    mentions.map((m) => [m.props.surface, m.props.resolved_by]).sort(),
  ).toEqual([
    ["Eta Holdings", "user"],
    ["Gamma Holdings", "carried"],
  ]);
  // The names nothing settled are left for a person.
  expect(
    [...g.nodes()]
      .filter((r) => r.label === N.Review && r.props.reason === "unresolved")
      .map((r) => r.props.norm_name)
      .sort(),
  ).toEqual(
    ["Delta Holdings", "Iota Holdings", "Kappa Holdings", "Theta Holdings"]
      .map(n)
      .sort(),
  );
});
