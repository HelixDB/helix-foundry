import { describe, expect, it } from "vitest";

import type { IndexCatalogEntry } from "@/web/areas/explore/explorer/lib/types";

import sampleFixture from "./__fixtures__/sample.json";
import { parseSample } from "@/web/areas/explore/explorer/lib/queries";
import {
  LabelSchemaBuilder,
  edgeLabelPairs,
  indexesByProperty,
  inferLabelSchemas,
  isHeavyValue,
  relationshipsForLabel,
} from "@/web/areas/explore/explorer/lib/schema";
import type {
  GraphData,
  GraphEdge,
  GraphNode,
  LabelSchema,
  NodeFetchShape,
} from "@/web/areas/explore/explorer/lib/types";

const node = (
  dbId: number,
  label: string,
  properties: GraphNode["properties"] = {},
  omitted: string[] = [],
): GraphNode => ({
  id: `n:${dbId}`,
  dbId,
  label,
  properties,
  omitted,
  complete: omitted.length === 0,
});

const edge = (
  dbId: number,
  label: string,
  from: number,
  to: number,
): GraphEdge => ({
  id: `e:${dbId}`,
  dbId,
  label,
  source: `n:${from}`,
  target: `n:${to}`,
  properties: {},
});

describe("isHeavyValue", () => {
  it("flags vectors, long strings and large objects", () => {
    expect(
      isHeavyValue(
        "embedding",
        Array.from({ length: 384 }, () => 0.1),
      ),
    ).toBe(true);
    expect(
      isHeavyValue(
        "ids",
        Array.from({ length: 20 }, (_, index) => index),
      ),
    ).toBe(false);
    expect(isHeavyValue("name", "x".repeat(2049))).toBe(true);
    expect(isHeavyValue("name", "x".repeat(2048))).toBe(false);
    expect(
      isHeavyValue("payload", {
        trace: Array.from(
          { length: 300 },
          (_, index) => `frame ${index}: planner::execute`,
        ),
      }),
    ).toBe(true);
    expect(isHeavyValue("meta", { a: 1 })).toBe(false);
    expect(isHeavyValue("count", 3)).toBe(false);
    expect(isHeavyValue("missing", null)).toBe(false);
  });

  it("is stricter for keys that usually hold long text", () => {
    expect(isHeavyValue("body", "x".repeat(600))).toBe(true);
    expect(isHeavyValue("page_content", "x".repeat(600))).toBe(true);
    expect(isHeavyValue("title", "x".repeat(600))).toBe(false);
    expect(isHeavyValue("body", "short")).toBe(false);
  });
});

describe("indexesByProperty", () => {
  it("maps catalog entries to index kinds per target and label", () => {
    const entries: IndexCatalogEntry[] = [
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
        property: "email",
        target: "node",
        type: "full_text",
        tenantProperty: null,
      },
      {
        indexId: "3",
        label: "User",
        property: "age",
        target: "node",
        type: "range",
        direction: "ascending",
      },
      {
        indexId: "4",
        label: "User",
        property: "since",
        target: "edge",
        type: "equality",
      },
    ];
    const result = indexesByProperty(entries);
    expect(result.get("node:User")?.get("email")).toEqual([
      "unique",
      "full_text",
    ]);
    expect(result.get("node:User")?.get("age")).toEqual(["range"]);
    expect(result.get("edge:User")?.get("since")).toEqual(["equality"]);
  });
});

describe("LabelSchemaBuilder", () => {
  it("tallies kinds by frequency and fill counts", () => {
    const builder = new LabelSchemaBuilder("node", "User");
    builder.addRow({ $id: 1, $label: "User", age: 3, name: "a" });
    builder.addRow({ $id: 2, $label: "User", age: 3.5, name: "b" });
    builder.addRow({ $id: 3, $label: "User", age: 4 });
    const schema = builder.build(new Map([["email", ["unique" as const]]]));
    expect(schema).toEqual({
      target: "node",
      label: "User",
      sampled: 3,
      properties: [
        {
          key: "age",
          kinds: ["integer", "float"],
          heavy: false,
          indexes: [],
          seen: 3,
        },
        { key: "name", kinds: ["string"], heavy: false, indexes: [], seen: 2 },
        { key: "email", kinds: [], heavy: false, indexes: ["unique"], seen: 0 },
      ],
    });
  });
});

describe("inferLabelSchemas", () => {
  it("counts omitted keys as present and keeps probed heavy flags and indexes", () => {
    const probed: LabelSchema[] = [
      {
        target: "node",
        label: "Doc",
        sampled: 2,
        properties: [
          {
            key: "embedding",
            kinds: ["vector"],
            heavy: true,
            indexes: ["vector"],
            seen: 2,
          },
          {
            key: "title",
            kinds: ["string"],
            heavy: false,
            indexes: ["full_text"],
            seen: 2,
          },
        ],
      },
    ];
    const graph: GraphData = {
      nodes: [
        node(1, "Doc", { title: "a", pages: 3 }, ["embedding"]),
        node(2, "Doc", { title: "b" }, ["embedding"]),
        node(3, "Doc", { title: "c" }, ["embedding"]),
        node(4, "User", { name: "u" }),
      ],
      edges: [
        {
          ...edge(1, "WROTE", 4, 1),
          properties: { at: "2024-01-01T00:00:00Z" },
        },
      ],
    };
    const schemas = inferLabelSchemas(graph, probed);
    expect(schemas.map((schema) => `${schema.target}:${schema.label}`)).toEqual(
      ["node:Doc", "node:User", "edge:WROTE"],
    );
    const doc = schemas[0];
    expect(doc.sampled).toBe(3);
    expect(doc.properties).toEqual([
      {
        key: "embedding",
        kinds: ["vector"],
        heavy: true,
        indexes: ["vector"],
        seen: 3,
      },
      {
        key: "title",
        kinds: ["string"],
        heavy: false,
        indexes: ["full_text"],
        seen: 3,
      },
      { key: "pages", kinds: ["integer"], heavy: false, indexes: [], seen: 1 },
    ]);
    expect(schemas[2].properties[0]).toMatchObject({
      key: "at",
      kinds: ["datetime"],
    });
  });

  it("works on a captured sample", () => {
    const shape = (sampleFixture.args as { shape: NodeFetchShape }).shape;
    const schemas = inferLabelSchemas(
      parseSample(sampleFixture.response, shape),
    );
    const document = schemas.find((schema) => schema.label === "Document");
    if (document) {
      expect(
        document.properties.find((property) => property.key === "embedding"),
      ).toMatchObject({ heavy: true });
    }
    expect(schemas.length).toBeGreaterThan(0);
  });
});

describe("edgeLabelPairs", () => {
  const graph: GraphData = {
    nodes: [
      node(1, "User"),
      node(2, "User"),
      node(3, "Company"),
      node(4, "Doc"),
    ],
    edges: [
      edge(1, "WORKS_AT", 1, 3),
      edge(2, "WORKS_AT", 2, 3),
      edge(3, "FOLLOWS", 1, 2),
      edge(4, "FOLLOWS", 1, 1),
      edge(5, "WROTE", 1, 4),
      edge(6, "WROTE", 3, 4),
      edge(7, "WROTE", 1, 99),
    ],
  };

  it("counts source and target label pairs per edge label", () => {
    expect(edgeLabelPairs(graph)).toEqual([
      {
        edgeLabel: "FOLLOWS",
        count: 2,
        pairs: [{ sourceLabel: "User", targetLabel: "User", count: 2 }],
      },
      {
        edgeLabel: "WORKS_AT",
        count: 2,
        pairs: [{ sourceLabel: "User", targetLabel: "Company", count: 2 }],
      },
      {
        edgeLabel: "WROTE",
        count: 3,
        pairs: [
          { sourceLabel: "Company", targetLabel: "Doc", count: 1 },
          { sourceLabel: "User", targetLabel: "Doc", count: 1 },
        ],
      },
    ]);
  });

  it("lists a label's relationships by direction", () => {
    const pairs = edgeLabelPairs(graph);
    expect(relationshipsForLabel(pairs, "User")).toEqual([
      {
        edgeLabel: "FOLLOWS",
        direction: "out",
        otherLabels: ["User"],
        count: 2,
      },
      {
        edgeLabel: "FOLLOWS",
        direction: "in",
        otherLabels: ["User"],
        count: 2,
      },
      {
        edgeLabel: "WORKS_AT",
        direction: "out",
        otherLabels: ["Company"],
        count: 2,
      },
      { edgeLabel: "WROTE", direction: "out", otherLabels: ["Doc"], count: 1 },
    ]);
    expect(relationshipsForLabel(pairs, "Doc")).toEqual([
      {
        edgeLabel: "WROTE",
        direction: "in",
        otherLabels: ["Company", "User"],
        count: 2,
      },
    ]);
  });
});
