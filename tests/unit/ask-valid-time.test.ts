import { afterEach, describe, expect, it } from "vitest";
import { cannotAnswerKind } from "@/shared/foundry-ask";
import type { CitationTarget } from "@/shared/wfe";
import {
  CANNOT_ANSWER_LABELS,
  citationText,
  parseAskResult,
  parseTarget,
  parseValid,
  type CitationContext,
} from "@/web/areas/ontology/ask-model";
import { formatDate } from "@/web/kit/util";
import { contractErrors } from "../../mock/agent-contract";

// Contract 4.7.0's valid time and decline kinds, held to the vendored
// schema (contract/ontology_agent/ask_result.json).
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
  const result = parseAskResult(JSON.stringify(answered(target)));
  expect(result?.status).toBe("answered");
  return result?.status === "answered"
    ? result.citations[0]?.target
    : undefined;
};
const utc = (t: string) => formatDate(t, true, "UTC");

describe("valid-time citations", () => {
  it.each([
    [{ from, to }, `Valid from ${utc(from)} until ${utc(to)}`],
    [{ from }, `Valid from ${utc(from)}`],
    [{ from, to: null }, `Valid from ${utc(from)}`],
    [{ from, to: null, day: null }, `Valid from ${utc(from)}`],
    // A source's day with no time of day: shown as that day, not the
    // midnight after it that `from` is.
    [
      { from: "2026-09-30T00:00:00Z", day: "2026-09-29" },
      `Valid from ${formatDate("2026-09-29", false)}`,
    ],
    [
      { from: "2026-09-30T00:00:00Z", to, day: "2026-09-29" },
      `Valid from ${formatDate("2026-09-29", false)} until ${utc(to)}`,
    ],
  ])("keeps and renders the interval %j", (valid, when) => {
    const target = { version: { ...version, valid } };
    expect(contractErrors("ask_result", answered(target))).toEqual([]);
    const parsed = parsedTarget(target)!;
    expect(parsed).toEqual({
      version: {
        ...version,
        valid: Object.fromEntries(
          Object.entries(valid).filter(([, v]) => v !== null),
        ),
      },
    });
    expect(citationText(parsed, {} as CitationContext)).toBe(
      `As recorded ${utc(at)} · ${when}: a break of required`,
    );
  });

  it.each([undefined, null])(
    "keeps a citation without valid time (%j)",
    (valid) => {
      const target = {
        version: { ...version, ...(valid === null ? { valid } : {}) },
      };
      expect(contractErrors("ask_result", answered(target))).toEqual([]);
      expect(parsedTarget(target)).toEqual({ version });
      expect(citationText(parseTarget(target)!, {} as CitationContext)).toBe(
        `As recorded ${utc(at)}: a break of required`,
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
    { from, day: 1 },
    { from, day: "2026-10-01T00:00:00Z" },
    { from, day: "1 October 2026" },
    { from, day: "2026-02-30" },
  ])("drops a malformed interval (%j) but keeps its citation", (valid) => {
    const target = { version: { ...version, valid } };
    expect(parseValid(valid)).toBeUndefined();
    expect(parseTarget(target)).toEqual({ version });
    expect(parseAskResult(JSON.stringify(answered(target)))).toMatchObject({
      status: "answered",
      citations: [{ marker: "c1", target: { version } }],
    });
    expect(citationText(parseTarget(target)!, {} as CitationContext)).toBe(
      `As recorded ${utc(at)}: a break of required`,
    );
  });

  // The engine refuses unknown fields on both, so a decoder of 4.6.0 refuses
  // any answer citing `valid`: the lockstep the vendored schema records.
  it.each([
    { version: { ...version, valid: { from, until: to } } },
    { version: { ...version, valid: { to } } },
    { version: { ...version, valid: { from }, held: true } },
  ])("is refused by the contract with an unknown or missing field", (t) => {
    expect(contractErrors("ask_result", answered(t))).not.toEqual([]);
  });

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
      expect(contractErrors("ask_result", answered(target))).toEqual([]);
      expect(
        citationText(parsedTarget(target) as CitationTarget, ctx),
      ).toContain(`Valid from ${utc(from)}`);
    }
  });

  describe("in any time zone", () => {
    const zone = process.env.TZ;
    afterEach(() => {
      if (zone === undefined) delete process.env.TZ;
      else process.env.TZ = zone;
    });
    const shown = (tz: string) => {
      process.env.TZ = tz;
      return {
        local: formatDate(from),
        utc: utc(from),
        day: formatDate("2026-09-29", false),
      };
    };

    it("shows a valid time in UTC, named, and a source's day as itself", () => {
      const east = shown("Pacific/Kiritimati"),
        west = shown("America/Los_Angeles");
      expect(east.utc).toBe(west.utc);
      expect(east.utc).toContain("UTC");
      expect(east.local).not.toBe(west.local);
      expect(east.day).toBe(west.day);
      expect(east.day).toContain("29");
    });
  });
});

describe("cannot-answer kinds", () => {
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
      expect(contractErrors("ask_result", fixture)).toEqual([]);
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
