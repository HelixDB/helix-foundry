import { describe, expect, it } from "vitest";
import * as model from "@/web/areas/reviews/model";
import type { EntityRow } from "@/web/areas/explore/model";
import type { FoundryPropValue, ReviewDecision } from "@/shared/wfe";

const customer = (key: string, name: string, dbId = 1): EntityRow => ({
  dbId,
  id: `id-${key}`,
  type: "Customer",
  name,
  identity: { type: "Customer", key },
  props: { entity_key: key, company_name: name, currency: "usd" },
});
const globex = customer("scus_GLOBEX01", "Globex Corporation");
// A resolve review as the engine stores it: identity, candidates as keys.
const name = {
  review_key: "review-key",
  reason: "unresolved",
  type: "Customer",
  doc_id: "doc",
  norm_name: "globexx corporation",
  candidates: ["scus_GLOBEX01", "scus_GLOBAL02"],
};
const quote: model.ReviewValue = {
  value: { text: "GBP" },
  documentId: "doc",
  quote: "Globex Corporation is billed in GBP.",
  section: 0,
  start: 0,
  end: 36,
};
// A PropQuote record as the engine writes it, its value's JSON text as given.
const stored = (value: string) =>
  `{"value":${value},"doc_id":"${quote.documentId}","span":{"section":0,"start":0,"end":36},"quote":"${quote.quote}","confidence":0.94}`;
// A property review: the entity, the prop and the quotes that disagree.
const prop = {
  review_key: "prop-key",
  reason: "prop_conflict",
  type: "Customer",
  entity_key: "scus_GLOBEX01",
  prop: "currency",
  values: `[${stored('"GBP"')}]`,
  candidates: [],
};

describe("what settles a review", () => {
  it.each([
    "unresolved",
    "ambiguous",
    "collision",
    "identifier_conflict",
    "adjudicated",
  ])("a %s review with its identity takes a resolve choice", (reason) => {
    expect(model.settleable({ ...name, reason }, [globex])).toEqual({
      candidates: [
        { key: "scus_GLOBEX01", entity: globex },
        { key: "scus_GLOBAL02" },
      ],
      settle: {
        kind: "resolve",
        reviewId: "review-key",
        type: "Customer",
        docId: "doc",
        normName: "globexx corporation",
      },
    });
  });
  it.each(["review_key", "type", "doc_id", "norm_name"])(
    "a resolve review without %s is stale: the backend refuses it",
    (field) => {
      const r = { ...name, [field]: undefined };
      expect(model.settleable(r, [globex]).settle).toEqual({ kind: "stale" });
      expect(model.settleable({ ...name, [field]: "" }, []).settle).toEqual({
        kind: "stale",
      });
    },
  );
  it("names candidates only by an entity of the review's type and identity", () => {
    const named: EntityRow = {
      ...globex,
      type: "Company",
      identity: { type: "Company", name: "globex" },
    };
    const other = { ...globex, type: "Supplier" };
    const r = model.settleable(
      { ...name, type: "Company", candidates: ["globex", "scus_GLOBEX01"] },
      [named, other],
    );
    expect(r.candidates).toEqual([
      { key: "globex", entity: named },
      { key: "scus_GLOBEX01" },
    ]);
  });
  it("keeps each candidate once and drops ones that aren't text", () => {
    expect(
      model.settleable(
        { ...name, candidates: ["a", "a", "", 4, null, "b"] },
        [],
      ).candidates,
    ).toEqual([{ key: "a" }, { key: "b" }]);
    expect(
      model.settleable({ ...name, candidates: "a" }, []).candidates,
    ).toEqual([]);
  });
  it.each([
    "low_confidence",
    "unverified",
    "violation",
    "unmatched",
    undefined,
  ])("nothing settles a %s review", (reason) => {
    expect(model.settleable({ ...name, reason }, [globex])).toEqual({
      candidates: [
        { key: "scus_GLOBEX01", entity: globex },
        { key: "scus_GLOBAL02" },
      ],
    });
  });
  it("a row conflict carries the table's value it would replace", () => {
    expect(model.settleable(prop, [globex])).toEqual({
      candidates: [],
      settle: {
        kind: "prop",
        reviewId: "prop-key",
        type: "Customer",
        entityKey: "scus_GLOBEX01",
        prop: "currency",
        values: [quote],
        table: "usd",
      },
    });
  });
  it("a documents-only conflict has no stored value", () => {
    const contract: EntityRow = {
      dbId: 2,
      id: "id-contract",
      type: "Contract",
      name: "MSA",
      identity: { type: "Contract", name: "msa" },
      props: { norm_name: "msa", name: "MSA", odd: [1] },
    };
    for (const p of ["term_months", "odd"]) {
      const s = model.settleable(
        { ...prop, type: "Contract", entity_key: "msa", prop: p },
        [contract],
      ).settle;
      expect(s).toMatchObject({ kind: "prop", entityKey: "msa" });
      expect(s).not.toHaveProperty("table");
    }
  });
  it.each([
    { review_key: undefined },
    { type: "" },
    { entity_key: undefined },
    { prop: undefined },
    { values: "[]" },
    { values: "{broken" },
    { entity_key: "scus_OTHER" },
  ])(
    "a property review missing its identity, values or entity is stale (%o)",
    (bad) => {
      expect(model.settleable({ ...prop, ...bad }, [globex]).settle).toEqual({
        kind: "stale",
      });
    },
  );
});

describe("review choices", () => {
  const review = model.settleable(name, [globex]);
  // Each quote's value as the engine wrote it, and the arm that sends it.
  const stated = [
    ["0", { integer: "0" }],
    ["false", { boolean: false }],
    ['""', { text: "" }],
    ["36", { integer: "36" }],
    ["2.5", { number: 2.5 }],
    ["36.0", { number: 36 }],
    ["9007199254740993", { integer: "9007199254740993" }],
    ["-9223372036854775808", { integer: "-9223372036854775808" }],
  ] as const;
  const conflict = model.settleable(
    { ...prop, values: `[${stated.map(([raw]) => stored(raw)).join(",")}]` },
    [globex],
  );
  it("sends a listed candidate or explicit not-in-data", () => {
    expect(model.decisionFor(review, { entityKey: "scus_GLOBAL02" })).toEqual({
      resolve: { entityKey: "scus_GLOBAL02" },
    });
    expect(model.decisionFor(review, { entityKey: null })).toEqual({
      resolve: { notInData: true },
    });
  });
  it("refuses unlisted candidates and a value for a name", () => {
    expect(
      model.decisionFor(review, { entityKey: "invented" }),
    ).toBeUndefined();
    expect(
      model.decisionFor(review, { value: { integer: "36" } }),
    ).toBeUndefined();
  });
  it("sends the selected value in its arm, including false and zero, and an integer's every digit", () => {
    expect(
      conflict.settle?.kind === "prop" &&
        conflict.settle.values.map((v) => v.value),
    ).toEqual(stated.map(([, value]) => value));
    for (const [, value] of stated)
      expect(model.decisionFor(conflict, { value })).toEqual({ prop: value });
  });
  it("refuses an unlisted property value, one of another kind and a candidate for a property", () => {
    for (const value of [
      { integer: "48" },
      { text: "36" },
      { number: 0 },
      { integer: "9007199254740992" },
      undefined,
    ])
      expect(model.decisionFor(conflict, { value })).toBeUndefined();
    expect(
      model.decisionFor(conflict, { entityKey: "scus_GLOBEX01" }),
    ).toBeUndefined();
  });
  it("offers nothing for a stale review or one nothing settles", () => {
    const stale = model.settleable({ ...name, type: undefined }, []);
    expect(model.decisionFor(stale, { entityKey: null })).toBeUndefined();
    const other = model.settleable({ ...name, reason: "unverified" }, []);
    expect(model.decisionFor(other, { entityKey: null })).toBeUndefined();
  });
  it("matches overrides by ontology identity, not review or load id", () => {
    const s = review.settle!,
      p = conflict.settle!;
    const d: ReviewDecision = {
      kind: "resolve",
      type: "Customer",
      docId: "doc",
      normName: "globexx corporation",
      entityKey: "scus_GLOBEX01",
    };
    expect(model.matchesReview(d, s)).toBe(true);
    for (const other of [
      { docId: "other" },
      { type: "Other" },
      { normName: "other" },
    ])
      expect(model.matchesReview({ ...d, ...other }, s)).toBe(false);
    const v: ReviewDecision = {
      kind: "prop",
      type: "Customer",
      entityKey: "scus_GLOBEX01",
      prop: "currency",
      value: { text: "GBP" },
    };
    expect(model.matchesReview(v, p)).toBe(true);
    for (const other of [
      { prop: "other" },
      { entityKey: "other" },
      { type: "Other" },
    ])
      expect(model.matchesReview({ ...v, ...other }, p)).toBe(false);
    // A decision of one kind never stands for a review of the other.
    expect(model.matchesReview(v, s)).toBe(false);
    expect(model.matchesReview(d, p)).toBe(false);
    expect(model.matchesReview(d, { kind: "stale" })).toBe(false);
  });
});

describe("whether a load read a decision", () => {
  const at = "2026-10-20T11:00:00.000Z";
  it("reads one saved before it started, not after", () => {
    expect(model.loadRead({ decidedAt: "2026-10-20T10:59:59Z" }, at)).toBe(
      true,
    );
    expect(model.loadRead({ decidedAt: "2026-10-20T11:00:01Z" }, at)).toBe(
      false,
    );
    expect(model.loadRead({ decidedAt: at }, at)).toBe(false);
  });
  it("reads none when either time is unknown", () => {
    expect(model.loadRead({ decidedAt: at }, undefined)).toBe(false);
    expect(model.loadRead({ decidedAt: at }, null)).toBe(false);
    expect(model.loadRead({ decidedAt: null }, at)).toBe(false);
    expect(model.loadRead({}, at)).toBe(false);
    expect(model.loadRead({ decidedAt: "soon" }, at)).toBe(false);
  });
});

describe("stored property review quotes", () => {
  const raw = {
    value: 24,
    doc_id: "doc",
    span: { section: 0, start: 0, end: 9 },
    quote: "24 months",
    confidence: 0.94,
  };
  const quote: model.ReviewValue = {
    value: { integer: "24" },
    documentId: "doc",
    quote: "24 months",
    section: 0,
    start: 0,
    end: 9,
  };
  it("parses engine JSON text and keeps every quote", () => {
    expect(
      model.reviewValues(
        JSON.stringify([
          raw,
          { ...raw, value: false },
          { ...raw, value: "<b>untrusted</b>" },
        ]),
      ),
    ).toEqual([
      quote,
      { ...quote, value: { boolean: false } },
      { ...quote, value: { text: "<b>untrusted</b>" } },
    ]);
  });
  it("reads an integer from its JSON text, past 2^53 too, and only a quote's value so", () => {
    const [big] = model.reviewValues(
      '[{"value":12345678901234567,"doc_id":"doc","span":{"section":0,"start":0,"end":9},"quote":"24 months","confidence":0.94}]',
    );
    expect(big).toEqual({ ...quote, value: { integer: "12345678901234567" } });
    // JSON.parse alone would round it.
    expect(String(JSON.parse("12345678901234567"))).toBe("12345678901234568");
    expect(
      model.reviewValues(JSON.stringify([{ ...raw, value: 1e21 }]))[0]?.value,
    ).toEqual({ number: 1e21 });
  });
  it.each([undefined, [], "{broken", "null", "{}", "false"])(
    "ignores malformed values %s",
    (raw) => {
      expect(model.reviewValues(raw)).toEqual([]);
    },
  );
  it("drops bad records individually", () => {
    const bad = [
      null,
      false,
      {},
      { ...raw, span: null },
      { ...raw, value: [] },
      { ...raw, value: null },
      { ...raw, doc_id: "" },
      { ...raw, doc_id: 4 },
      { ...raw, quote: false },
      { ...raw, span: { section: -1, start: 0, end: 9 } },
      { ...raw, span: { section: 0, start: 2, end: 1 } },
      { ...raw, span: { section: 0.5, start: 0, end: 9 } },
    ];
    expect(model.reviewValues(JSON.stringify([...bad, raw]))).toEqual([quote]);
  });
});

describe("a prop decision's value as text", () => {
  it.each([
    [{ integer: "9007199254740993" }, "9,007,199,254,740,993"],
    [{ integer: "-3" }, "-3"],
    [{ number: 2.5 }, "2.5"],
    [{ number: 1200000 }, "1,200,000"],
    [{ boolean: true }, "Yes"],
    [{ boolean: false }, "No"],
    [{ text: "GBP" }, "GBP"],
    [{ null: null }, ""],
  ] satisfies [FoundryPropValue, string][])("%o reads %s", (v, text) =>
    expect(model.propValueText(v)).toBe(text),
  );
});
