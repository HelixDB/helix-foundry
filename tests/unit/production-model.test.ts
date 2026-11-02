import { describe, expect, it } from "vitest";
import {
  cardStartsMinimised,
  hasOwnOntology,
  parseProductionState,
  productionKey,
  productionProgress,
  promptOntology,
  stripShown,
  type ProductionState,
} from "@/web/areas/ontology/production-model";

const EMPTY: ProductionState = {
  copied: false,
  dismissed: false,
  minimised: false,
};

describe("productionKey", () => {
  it("is per workspace", () => {
    expect(productionKey("ws1")).toBe("foundry.production.v1:ws1");
  });
});

describe("parseProductionState", () => {
  it("reads what it wrote", () => {
    const state = { copied: true, dismissed: true, minimised: true };
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
      '{"copied":"yes","dismissed":1,"minimised":null}',
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

  it("keeps a prompt copied before the ontology done", () => {
    const p = productionProgress(false, true);
    expect(p.done).toBe(1);
    expect(p.current).toBe("ontology");
    expect(states(p)).toEqual(["current", "done"]);
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

describe("stripShown", () => {
  const open = productionProgress(true, false);

  it("shows on every other page", () => {
    for (const path of [
      "/data",
      "/data/documents/d1",
      "/settings/billing",
      "/ontology",
      "/ontology/o1",
      "/ontology/newer",
      "/ontology/sample/more",
      "/explore/o1",
      "/explore/o1/records",
      "/asking",
    ])
      expect(stripShown(path, false, EMPTY, open), path).toBe(true);
  });

  it("leaves the Analyst and the pages that make an ontology alone", () => {
    for (const path of [
      "/",
      "/ask",
      "/ask/o1",
      "/ontology/new",
      "/ontology/new/",
      "/ontology/sample",
    ])
      expect(stripShown(path, false, EMPTY, open), path).toBe(false);
  });

  it("doesn't show during onboarding", () => {
    expect(stripShown("/settings", true, EMPTY, open)).toBe(false);
  });

  it("goes once dismissed", () => {
    expect(
      stripShown("/data", false, { ...EMPTY, dismissed: true }, open),
    ).toBe(false);
  });

  it("goes once both steps are done", () => {
    expect(
      stripShown("/data", false, EMPTY, productionProgress(true, true)),
    ).toBe(false);
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

describe("promptOntology", () => {
  const list = [{ id: "s" }, { id: "a" }, { id: "b/c" }];

  it("names the ontology the page has open, the sample too", () => {
    expect(promptOntology("/ontology/a/loads", list, "b/c", "s")?.id).toBe("a");
    expect(promptOntology("/explore/b%2Fc/records", list, "a", "s")?.id).toBe(
      "b/c",
    );
    expect(promptOntology("/ontology/s", list, "a", "s")?.id).toBe("s");
  });

  it("falls back on the one last opened, unless it's the sample", () => {
    expect(promptOntology("/data", list, "b/c", "s")?.id).toBe("b/c");
    expect(promptOntology("/data", list, "s", "s")?.id).toBe("a");
  });

  it("ignores a last or open ontology the list doesn't have", () => {
    expect(promptOntology("/ontology/gone", list, "gone", "s")?.id).toBe("a");
  });

  it("names the first without a sample, and none with only the sample", () => {
    expect(promptOntology("/data", list, "", undefined)?.id).toBe("s");
    expect(promptOntology("/data", [{ id: "s" }], "", "s")).toBeUndefined();
    expect(promptOntology("/data", [], "", undefined)).toBeUndefined();
  });
});
