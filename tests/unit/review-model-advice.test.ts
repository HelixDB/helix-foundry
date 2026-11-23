import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import type {
  FoundryReviewChoice,
  FoundryReviewModelAdvice,
} from "@/shared/wfe";
import {
  MODEL_WHYS,
  adviceContext,
  advise,
  choiceKey,
  modelAdvised,
  plainRationale,
  type Advice,
} from "@/web/areas/reviews/advice";
import { cardsOf } from "@/web/areas/reviews/cards";
import { choicesOf, founderAsk } from "@/web/areas/reviews/choices";
import { reviewValues } from "@/web/areas/reviews/model";
import type { NameCard, ValueCard } from "@/web/areas/reviews/queue";
import { batchOf } from "@/web/areas/reviews/walk";
import { parseLoadReviews } from "@/web/areas/explore/model";
import { usageView } from "@/web/areas/workspace/usage";
import { modelAdviceOf } from "../../mock/review-advice";
import {
  ctxOf,
  entity,
  heldCard,
  nameCard,
  valueCard,
} from "./__fixtures__/review-cards";

// Founder's model's advice (reviews/advice.ts modelAdvised): where the rules
// abstain on a name or a value, the index's pick shows, unmeasured and never
// batched, only while the card offers what the model was offered. The
// backend's reviewadvice runs the same fixtures (abstain.json, values.json).

const fixture = <T>(name: string): T =>
  JSON.parse(
    readFileSync(
      new URL(`../../backend/reviewadvice/testdata/${name}`, import.meta.url),
      "utf8",
    ),
  ) as T;

const cite = { docId: "doc-1", span: { section: "1", start: "0", end: "4" } };

/** Advice picking `choice` among `offered`. */
const adviceOf = (
  choice: FoundryReviewChoice,
  offered: FoundryReviewChoice[],
  more: Partial<FoundryReviewModelAdvice> = {},
): FoundryReviewModelAdvice => ({
  choice,
  offered,
  rationale: "Quote 1 names the London office.",
  cites: [cite],
  ...more,
});

/** A name between two found candidates, the model picking the second. */
function tiedName(
  more: Partial<FoundryReviewModelAdvice> = {},
  keys = ["scus_1", "scus_2"],
): NameCard {
  const card = nameCard("globex", {
    candidates: keys.map((k) => ({ key: k, entity: entity(k, `Globex ${k}`) })),
  });
  return {
    ...card,
    modelAdvice: adviceOf(
      { resolve: { entityKey: "scus_2" } },
      [
        { resolve: { entityKey: "scus_1" } },
        { resolve: { entityKey: "scus_2" } },
        { resolve: { notInData: true } },
      ],
      more,
    ),
  };
}

/** A value documents split on, the model picking GBP. */
function splitValue(values = [{ text: "USD" }, { text: "GBP" }]): ValueCard {
  return {
    ...valueCard(values),
    modelAdvice: adviceOf({ prop: { text: "GBP" } }, [
      { prop: { text: "USD" } },
      { prop: { text: "GBP" } },
    ]),
  };
}

const suggested = (a: Advice) => {
  if (a.kind !== "suggest") throw new Error(`no suggestion: ${a.why}`);
  return a;
};

describe("the backend's fixtures", () => {
  type Case = {
    name: string;
    kind: "name" | "value";
    lane: "decide" | "fyi";
    key: string;
    read: unknown;
    names?: {
      total: number;
      choices: number;
      newest: { docId: string; entityKey?: string }[];
    };
    settles: boolean;
    why: string | null;
  };
  it("abstain where the backend pays for advice, and nowhere else", () => {
    const { cases } = fixture<{ cases: Case[] }>("abstain.json");
    expect(cases.length).toBeGreaterThan(10);
    for (const c of cases) {
      const review = parseLoadReviews(c.read).find(
        (r) => r.settle?.kind !== "stale" && r.settle?.reviewId === c.key,
      );
      if (!c.settles) {
        expect(review, c.name).toBeUndefined();
        continue;
      }
      expect(review, c.name).toBeDefined();
      const card =
        c.kind === "name"
          ? ({
              kind: "name",
              key: "card",
              lane: c.lane,
              review: review!,
              reviews: [review!],
              reviewCount: 1,
              blocked: 0,
              held: [],
            } satisfies NameCard)
          : ({
              kind: "value",
              key: "card",
              lane: c.lane,
              review: review!,
            } satisfies ValueCard);
      const settle = review!.settle as { type: string; normName?: string };
      const ctx = adviceContext({
        names: c.names
          ? [
              {
                type: settle.type,
                normName: settle.normName ?? "",
                total: c.names.total,
                choices: c.names.choices,
                newest: c.names.newest.map((d, i) => ({
                  id: `d${i}`,
                  ontologyId: "o",
                  kind: "resolve",
                  type: settle.type,
                  docId: d.docId,
                  normName: settle.normName ?? "",
                  ...(d.entityKey ? { entityKey: d.entityKey } : {}),
                  decidedByUserId: "someone",
                  decidedAt: "2026-11-01T00:00:00Z",
                })),
              },
            ]
          : [],
        claims: [],
        precision: { bands: [], rechecks: { agreed: 0, total: 0 } },
        single: () => false,
        user: "me",
      });
      const a = advise(card, ctx);
      const why = a.kind === "none" && MODEL_WHYS.has(a.why) ? a.why : null;
      expect(why, c.name).toBe(c.why);
    }
  });

  it("read values to the same choices", () => {
    const { cases } = fixture<{
      cases: {
        name: string;
        raw: string;
        quotes: number;
        choices: unknown[];
      }[];
    }>("values.json");
    for (const c of cases) {
      const values = reviewValues(c.raw);
      expect(values, c.name).toHaveLength(c.quotes);
      const choices = values.length
        ? choicesOf(
            {
              candidates: [],
              settle: {
                kind: "prop",
                reviewId: "r",
                type: "T",
                entityKey: "e",
                prop: "p",
                values,
              },
            },
            (x) => x.key,
          ).map((x) => x.decision.prop)
        : [];
      expect(choices, c.name).toEqual(c.choices);
    }
  });
});

describe("advise: Founder's model", () => {
  it("suggests the model's pick where the rules abstain, unmeasured and unbatched", () => {
    const name = suggested(advise(tiedName(), ctxOf()));
    expect(name).toEqual({
      kind: "suggest",
      rule: "model",
      choice: { resolve: { entityKey: "scus_2" } },
      basis: [
        {
          kind: "model",
          rationale: "Quote 1 names the London office.",
          cites: [{ documentId: "doc-1", section: 1, start: 0, end: 4 }],
        },
      ],
      strength: { kind: "unmeasured" },
      batchKey: null,
    });
    // A value the documents split on, and one too few quotes state.
    expect(suggested(advise(splitValue(), ctxOf())).choice).toEqual({
      prop: { text: "GBP" },
    });
    const split = splitValue([
      { text: "USD" },
      { text: "GBP" },
      { text: "USD" },
      { text: "GBP" },
    ]);
    expect(suggested(advise(split, ctxOf())).rule).toBe("model");
  });

  it("never batches it: G and Approve all take none", () => {
    const advice = new Map<string, Advice>([
      ["a", advise(tiedName(), ctxOf())],
      ["b", advise({ ...tiedName(), key: "b" }, ctxOf())],
    ]);
    expect(batchOf(["a", "b"], advice, "a")).toBeNull();
  });

  it("lets the rules win where they suggest, and ignores it wherever they don't abstain for it", () => {
    // A rule suggests: the one candidate.
    const one = nameCard("globex", {
      candidates: [{ key: "scus_1", entity: entity("scus_1", "Globex") }],
    });
    const withAdvice: NameCard = {
      ...one,
      modelAdvice: adviceOf({ resolve: { notInData: true } }, [
        { resolve: { entityKey: "scus_1" } },
        { resolve: { notInData: true } },
      ]),
    };
    expect(suggested(advise(withAdvice, ctxOf())).rule).toBe(
      "single_candidate",
    );
    // No candidates: the rules abstain, but not for the model.
    const none: NameCard = {
      ...nameCard("globex"),
      modelAdvice: adviceOf({ resolve: { notInData: true } }, [
        { resolve: { notInData: true } },
      ]),
    };
    expect(advise(none, ctxOf())).toEqual({
      kind: "none",
      why: "no_candidates",
    });
    // For information.
    const fyi: ValueCard = {
      ...valueCard([{ text: "USD" }, { text: "GBP" }], { table: "EUR" }),
      modelAdvice: splitValue().modelAdvice,
    };
    expect(advise(fyi, ctxOf())).toEqual({ kind: "none", why: "fyi" });
    // A held claim never takes it, whatever is sent (read_again,
    // waits_on_name and the rest).
    const held = { ...heldCard(1), modelAdvice: splitValue().modelAdvice };
    expect(advise(held, ctxOf()).kind === "suggest").toBe(false);
    expect(modelAdvised(held)).toBeNull();
  });

  it("hides advice once the card offers other choices than the model was offered", () => {
    // The value conflict gained a value.
    expect(
      advise(
        {
          ...splitValue([{ text: "USD" }, { text: "GBP" }, { text: "EUR" }]),
        },
        ctxOf(),
      ),
    ).toEqual({ kind: "none", why: "values_split" });
    // The name gained a candidate.
    expect(
      advise(tiedName({}, ["scus_1", "scus_2", "scus_3"]), ctxOf()),
    ).toEqual({ kind: "none", why: "tied_candidates" });
    // A pick that isn't among them, a choice it doesn't know, no rationale,
    // no cite it can link.
    for (const more of [
      { choice: { resolve: { entityKey: "scus_9" } } },
      { offered: [{ resolve: {} } as FoundryReviewChoice] },
      { rationale: "‮\u0007 " },
      { rationale: "x".repeat(301) },
      { cites: [] },
      {
        cites: [
          { docId: "doc-1", span: { section: "1", start: "4", end: "4" } },
        ],
      },
    ] satisfies Partial<FoundryReviewModelAdvice>[])
      expect(advise(tiedName(more), ctxOf()).kind, JSON.stringify(more)).toBe(
        "none",
      );
  });

  it("compares choices by what they choose", () => {
    expect(choiceKey({ prop: { integer: "012" } })).toBe(
      choiceKey({ prop: { integer: "12" } }),
    );
    expect(choiceKey({ prop: { integer: "12" } })).not.toBe(
      choiceKey({ prop: { number: 12 } }),
    );
    expect(choiceKey({ prop: { boolean: false } })).toBe("boolean:false");
    expect(choiceKey({ resolve: { notInData: true } })).toBe("not_in_data");
    expect(choiceKey(undefined)).toBeUndefined();
  });

  it("shows a rationale as plain text only", () => {
    expect(plainRationale("Quote 2 names\n‮Globex‬⁦ Ltd.⁩\u0000")).toBe(
      "Quote 2 names Globex Ltd.",
    );
    expect(plainRationale("<b>bold</b> https://x.test")).toBe(
      "<b>bold</b> https://x.test",
    );
    expect(plainRationale(42)).toBe("");
  });

  it("words F's ask by where the suggestion came from", () => {
    const [, gbp] = choicesOf(splitValue().review, (c) => c.key);
    expect(founderAsk(gbp, "model")).toBe(
      "Founder’s model suggests the value GBP. Help me decide this review.",
    );
    expect(founderAsk(gbp)).toBe(
      "The rule suggests the value GBP. Help me decide this review.",
    );
  });
});

describe("cardsOf", () => {
  it("carries a name's and a value's model advice", () => {
    const advice = adviceOf({ resolve: { notInData: true } }, [
      { resolve: { notInData: true } },
    ]);
    const name = nameCard("globex"),
      value = valueCard([{ text: "USD" }, { text: "GBP" }]);
    const reviews = new Map([
      [(name.review.settle as { reviewId: string }).reviewId, name.review],
      [(value.review.settle as { reviewId: string }).reviewId, value.review],
    ]);
    const cards = cardsOf(
      [
        {
          cardId: "n",
          docId: "doc-1",
          name: {
            reviewKey: (name.review.settle as { reviewId: string }).reviewId,
            type: "Customer",
            normName: "globex",
          },
          modelAdvice: advice,
        },
        {
          cardId: "v",
          docId: "doc-1",
          value: {
            reviewKey: (value.review.settle as { reviewId: string }).reviewId,
          },
          modelAdvice: advice,
        },
        {
          cardId: "w",
          docId: "doc-1",
          value: {
            reviewKey: (value.review.settle as { reviewId: string }).reviewId,
          },
        },
      ],
      reviews,
      [],
    );
    expect(cards.map((c) => c.kind !== "claim" && c.modelAdvice)).toEqual([
      advice,
      advice,
      undefined,
    ]);
  });
});

describe("the mock's advice", () => {
  const review = {
    review_key: "r",
    reason: "ambiguous",
    type: "Customer",
    doc_id: "doc-1",
    norm_name: "globex london",
    candidates: ["scus_1", "scus_2", "scus_1", ""],
    section: 1,
    start: 4,
    end: 10,
    concerned: [
      { type: "Customer", key: "scus_1", props: { name: "Globex Inc" } },
      { type: "Customer", key: "scus_2", props: { name: "Globex London Ltd" } },
    ],
  };
  const card = (kind: "resolve" | "prop", lane: "decide" | "fyi" = "decide") =>
    ({
      id: "c",
      kind,
      lane,
      docId: "doc-1",
      reviewKey: "r",
      reviewKeys: ["r"],
      reviewCount: 1,
      blocked: 0,
      facts: 1,
      perDecision: 1,
      shuffle: 0,
    }) as Parameters<typeof modelAdviceOf>[0];

  it("picks the candidate sharing the most words, offering every choice", () => {
    expect(modelAdviceOf(card("resolve"), review)).toEqual({
      choice: { resolve: { entityKey: "scus_2" } },
      offered: [
        { resolve: { entityKey: "scus_1" } },
        { resolve: { entityKey: "scus_2" } },
        { resolve: { notInData: true } },
      ],
      rationale:
        "Globex London Ltd shares the most words with the name the document uses.",
      cites: [
        { docId: "doc-1", span: { section: "1", start: "4", end: "10" } },
      ],
    });
    expect(modelAdviceOf(card("resolve", "fyi"), review)).toBeUndefined();
    expect(modelAdviceOf(card("resolve"), undefined)).toBeUndefined();
  });

  it("picks a value's last quote, as the site reads the values", () => {
    const values = JSON.stringify([
      {
        value: "12 months",
        doc_id: "a",
        quote: "q",
        span: { section: 0, start: 0, end: 2 },
      },
      {
        value: 12,
        doc_id: "b",
        quote: "q",
        span: { section: 3, start: 1, end: 5 },
      },
    ]);
    const a = modelAdviceOf(
      card("prop"),
      { ...review, reason: "prop_conflict", values },
      "Override.",
    );
    expect(a).toEqual({
      choice: { prop: { integer: "12" } },
      offered: [{ prop: { text: "12 months" } }, { prop: { integer: "12" } }],
      rationale: "Override.",
      cites: [{ docId: "b", span: { section: "3", start: "1", end: "5" } }],
    });
    const one = JSON.stringify([
      {
        value: "x",
        doc_id: "a",
        quote: "q",
        span: { section: 0, start: 0, end: 2 },
      },
    ]);
    expect(
      modelAdviceOf(card("prop"), { ...review, values: one }),
    ).toBeUndefined();
  });
});

describe("usage", () => {
  it("shows review suggestions, and ignores a task it doesn't know", () => {
    const view = usageView({
      resetsAt: "2026-12-01T00:00:00Z",
      totalUsdMicros: "250000",
      tasks: [
        {
          task: "FOUNDRY_USAGE_TASK_REVIEW_ADVICE",
          spentUsdMicros: "250000",
          capUsdMicros: "10000000",
        },
        {
          task: "FOUNDRY_USAGE_TASK_SOMETHING_NEW" as "FOUNDRY_USAGE_TASK_LOADS",
          spentUsdMicros: "1",
        },
      ],
    });
    expect(view.rows.map((r) => [r.label, r.spent, r.cap?.of])).toEqual([
      ["Review suggestions", "$0.25", "of $10.00"],
    ]);
  });
});
