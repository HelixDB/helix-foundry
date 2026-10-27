import { describe, expect, it } from "vitest";
import {
  readRuleCounts,
  ruleEvaluationRefusal,
} from "@/web/areas/ontology/model";

describe("rule evaluation boundary", () => {
  it.each([
    undefined,
    null,
    [],
    1,
    "rules",
    {},
    { count: 0 },
    { count: "1", evaluated: 0 },
    { count: -1, evaluated: 0 },
    { count: 0.5, evaluated: 0 },
    { count: 0x100000000, evaluated: 0 },
    { count: Infinity, evaluated: 0 },
    { count: 1, evaluated: "1" },
    { count: 1, evaluated: -1 },
    { count: 1, evaluated: 0.5 },
    { count: 1, evaluated: 2 },
    { count: 1, evaluated: NaN },
  ])("refuses missing or invalid counts %#", (rules) => {
    expect(readRuleCounts(rules)).toBeUndefined();
    expect(ruleEvaluationRefusal({ format: "agent_bundle", rules })).toContain(
      "not loadable until its rules are evaluated",
    );
  });
  it.each([
    { count: 0, evaluated: 0 },
    { count: 3, evaluated: 3 },
    { count: 0xffffffff, evaluated: 0xffffffff },
  ])("allows fully evaluated counts %#", (rules) => {
    expect(readRuleCounts(rules)).toEqual(rules);
    expect(
      ruleEvaluationRefusal({ format: "agent_bundle", rules }),
    ).toBeUndefined();
  });
  it("keeps partial evaluation visible but refuses loading", () => {
    const rules = { count: 3, evaluated: 2 };
    expect(readRuleCounts(rules)).toEqual(rules);
    expect(ruleEvaluationRefusal({ format: "agent_bundle", rules })).toContain(
      "not loadable",
    );
  });
  it("leaves other definition formats to their own load checks", () => {
    expect(ruleEvaluationRefusal()).toBeUndefined();
    expect(ruleEvaluationRefusal(null)).toBeUndefined();
    expect(ruleEvaluationRefusal({})).toBeUndefined();
    expect(ruleEvaluationRefusal({ format: "foundry_v1" })).toBeUndefined();
  });
});
