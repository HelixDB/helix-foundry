import { describe, expect, it } from "vitest";
import {
  cardStartsMinimised,
  hasOwnOntology,
  parseProductionState,
  productionKey,
  productionProgress,
  type ProductionState,
} from "@/web/areas/ontology/production-model";

const EMPTY: ProductionState = {
  copied: false,
  minimised: false,
};

describe("productionKey", () => {
  it("is per workspace", () => {
    expect(productionKey("ws1")).toBe("foundry.production.v1:ws1");
  });
});

describe("parseProductionState", () => {
  it("reads what it wrote", () => {
    const state = { copied: true, minimised: true };
    expect(parseProductionState(JSON.stringify(state))).toEqual(state);
  });

  it("reads a field left out as not set", () => {
    expect(parseProductionState('{"copied":true}')).toEqual({
      ...EMPTY,
      copied: true,
    });
  });

  it("tolerates anything else", () => {
    for (const raw of [
      "",
      "{not json",
      "null",
      "1",
      '"copied"',
      "[true]",
      '{"copied":"yes","minimised":null}',
    ])
      expect(parseProductionState(raw), raw).toEqual(EMPTY);
  });
});

describe("hasOwnOntology", () => {
  const sample = { id: "s" };

  it("can't tell before the list and the sample are read", () => {
    expect(hasOwnOntology(undefined, null)).toBeUndefined();
    expect(hasOwnOntology(undefined, sample)).toBeUndefined();
    expect(hasOwnOntology([{ id: "o" }], undefined)).toBeUndefined();
  });

  it("doesn't count the sample", () => {
    expect(hasOwnOntology([sample], sample)).toBe(false);
    expect(hasOwnOntology([sample, { id: "o" }], sample)).toBe(true);
  });

  it("counts any ontology without a sample, and none is none", () => {
    expect(hasOwnOntology([{ id: "o" }], null)).toBe(true);
    expect(hasOwnOntology([], null)).toBe(false);
  });
});

describe("productionProgress", () => {
  const states = (p: ReturnType<typeof productionProgress>) =>
    p.steps.map((s) => s.state);

  it("starts on the ontology, with the query to come", () => {
    const p = productionProgress(false, false);
    expect(p.done).toBe(0);
    expect(p.current).toBe("ontology");
    expect(states(p)).toEqual(["current", "upcoming"]);
  });

  it("moves on to the query once there's an ontology", () => {
    const p = productionProgress(true, false);
    expect(p.done).toBe(1);
    expect(p.current).toBe("query");
    expect(states(p)).toEqual(["done", "current"]);
  });

  it("keeps the query to come until there's an ontology, even once copied", () => {
    const p = productionProgress(false, true);
    expect(p.done).toBe(0);
    expect(p.current).toBe("ontology");
    expect(states(p)).toEqual(["current", "upcoming"]);
  });

  it("has no current step once both are done", () => {
    const p = productionProgress(true, true);
    expect(p.done).toBe(2);
    expect(p.current).toBeNull();
    expect(states(p)).toEqual(["done", "done"]);
  });

  it("names the steps", () => {
    expect(productionProgress(false, false).steps.map((s) => s.title)).toEqual([
      "Create an ontology",
      "Query it through the Helix MCP",
    ]);
  });
});

describe("cardStartsMinimised", () => {
  it("starts open only where it fits, with a step left, not left minimised", () => {
    expect(cardStartsMinimised(false, false, true)).toBe(false);
  });

  it("starts minimised for any of the three", () => {
    expect(cardStartsMinimised(true, false, true)).toBe(true);
    expect(cardStartsMinimised(false, true, true)).toBe(true);
    expect(cardStartsMinimised(false, false, false)).toBe(true);
  });
});
