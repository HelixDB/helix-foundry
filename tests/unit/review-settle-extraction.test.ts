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
