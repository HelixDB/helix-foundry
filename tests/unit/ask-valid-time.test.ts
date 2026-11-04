import Ajv2020 from "ajv/dist/2020";
import { describe, expect, it } from "vitest";
import { cannotAnswerKind } from "@/shared/foundry-ask";
import type { CitationTarget } from "@/shared/wfe";
import {
  CANNOT_ANSWER_LABELS,
  citationText,
  parseAskResult,
  parseTarget,
  type CitationContext,
} from "@/web/areas/ontology/ask-model";
import { formatDate } from "@/web/kit/util";
import { contractErrors } from "../../mock/agent-contract";
import schema from "./fixtures/ask-result-4.7.0.schema.json";

// P3 prework: schema copied verbatim with `git show rb/top:proto/ontology_agent/v1/schema/ask_result.json`
// at 9bb335b23013e0b5bc1b81a9e8c674573ec9f2ff. The deployed 4.4.0 mock stays on its own schema.
const validate = new Ajv2020({
  allErrors: true,
  validateFormats: false,
}).compile(schema);
const at = "2026-10-03T09:00:00Z";
const from = "2026-10-01T00:00:00Z";
const to = "2026-10-02T00:00:00Z";
const version = {
  tx: "opaque-tx",
  at,
  of: { record: { constraint: "required", key: "record" } },
};
const answered = (target: unknown) => ({
  status: "answered",
  answer: { blocks: [{ para: "A recorded state [c1]" }] },
  citations: [{ marker: "c1", evidence_id: "e1", target }],
  queries: [],
  usage: { input: 0, cache_read: 0, cache_write: 0, output: 0, usd: 0 },
});
const parsedTarget = (target: unknown) => {
  const fixture = answered(target);
  expect(validate(fixture), JSON.stringify(validate.errors)).toBe(true);
  const result = parseAskResult(JSON.stringify(fixture));
  expect(result?.status).toBe("answered");
  return result?.status === "answered"
    ? result.citations[0]?.target
    : undefined;
};

describe("P3 valid-time citations", () => {
  it.each([{ from, to }, { from }, { from, to: null }])(
    "keeps and renders the interval %j",
    (valid) => {
      const target = { version: { ...version, valid } };
      expect(
        contractErrors("ask_result", answered(target)).length,
      ).toBeGreaterThan(0);
      const parsed = parsedTarget(target)!;
      expect(parsed).toEqual({
        version: {
          ...version,
          valid: { from, ...(valid.to ? { to: valid.to } : {}) },
        },
      });
      expect(citationText(parsed, {} as CitationContext)).toBe(
        `As recorded ${formatDate(at)} · Valid from ${formatDate(from)}${valid.to ? ` until ${formatDate(valid.to)}` : ""}: a break of required`,
      );
    },
  );

  it.each([undefined, null])(
    "keeps a citation without valid time (%j)",
    (valid) => {
      const target = {
        version: { ...version, ...(valid === null ? { valid } : {}) },
      };
      expect(parsedTarget(target)).toEqual({ version });
      expect(citationText(parseTarget(target)!, {} as CitationContext)).toBe(
        `As recorded ${formatDate(at)}: a break of required`,
      );
    },
  );

  it.each([
    true,
    [],
    "time",
    {},
    { from: 1 },
    { from: "not-a-date" },
    { from: "2026-10-01" },
    { from: "2026-99-01T00:00:00Z" },
    { from, to: 1 },
    { from, to: "not-a-date" },
    { from, to: "2026-10-02" },
    { from, to: from },
    { from: to, to: from },
  ])(
    "drops malformed or empty intervals (%j) without losing the answer",
    (valid) => {
      const target = { version: { ...version, valid } };
      expect(parseTarget(target)).toBeUndefined();
      expect(parseAskResult(JSON.stringify(answered(target)))).toMatchObject({
        status: "answered",
        citations: [],
      });
    },
  );

  it("renders valid time for every historical subject", () => {
    const entity = { type: "Item", id: "one" };
    const ctx = { name: () => "One" } as unknown as CitationContext;
    for (const of of [
      { entity },
      { prop: { entity, prop: "state" } },
      { fact: { link: "uses", subject: entity, object: entity } },
      version.of,
    ]) {
      const target = { version: { ...version, of, valid: { from } } };
      expect(
        citationText(parsedTarget(target) as CitationTarget, ctx),
      ).toContain(`Valid from ${formatDate(from)}`);
    }
  });
});

describe("P3 cannot-answer kinds", () => {
  it.each(Object.entries(CANNOT_ANSWER_LABELS))(
    "keeps %s with its plain label",
    (kind, label) => {
      const fixture = {
        status: "cannot_answer",
        reason: "The source does not say.",
        gap: "A recorded state.",
        queries: [],
        usage: { input: 0, cache_read: 0, cache_write: 0, output: 0, usd: 0 },
        kind,
      };
      expect(validate(fixture), JSON.stringify(validate.errors)).toBe(true);
      expect(parseAskResult(JSON.stringify(fixture))).toMatchObject(fixture);
      expect(cannotAnswerKind(kind)).toBe(kind);
      expect(label).toBeTruthy();
    },
  );

  it.each([undefined, null, "unexpected", "<script>", 1, [], {}])(
    "omits an absent or unknown kind (%j)",
    (kind) => {
      expect(cannotAnswerKind(kind)).toBeUndefined();
      expect(
        parseAskResult(
          JSON.stringify({
            status: "cannot_answer",
            kind,
            reason: "r",
            gap: "g",
          }),
        ),
      ).toEqual({
        status: "cannot_answer",
        reason: "r",
        gap: "g",
        queries: [],
        usage: { input: 0, cache_read: 0, cache_write: 0, output: 0, usd: 0 },
      });
    },
  );
});
