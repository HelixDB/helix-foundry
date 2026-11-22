import { describe, expect, it } from "vitest";
import {
  BATCH_LABELS,
  SEED_QUOTE,
  advise,
  asks,
  cardName,
  claimBatchKey,
  coveredBy,
  effectOf,
  founderSeed,
  laneOf,
  settleOf,
  wilsonLower,
  type Advice,
  type Decidable,
} from "@/web/areas/reviews/advice";
import { choicesOf } from "@/web/areas/reviews/choices";
import type { Candidate, Settleable } from "@/web/areas/reviews/model";
import type { Band, ClaimCard, Precision } from "@/web/areas/reviews/queue";
import {
  ctxOf,
  entity,
  heldCard,
  nameCard,
  resolveDecision,
  valueCard,
} from "./__fixtures__/review-cards";

// Founder's suggestions (reviews/advice.ts): the rules over what a card and
// the saved decisions hold, the lanes, coverage, effects and Ask Founder's
// seed.

const bands = (
  ...b: { band: Band; right: number; total: number }[]
): Precision => ({ bands: b, rechecks: { agreed: 0, total: 0 } });

/** Whether a card offers the choice suggested: a suggestion is only ever one of its choices. */
const offered = (r: Settleable, choice: unknown) =>
  choicesOf(r, (c) => c.key).some(
    (c) => JSON.stringify(c.decision) === JSON.stringify(choice),
  );

const suggested = (a: Advice) => {
  if (a.kind !== "suggest") throw new Error(`no suggestion: ${a.why}`);
  return a;
};

describe("wilsonLower", () => {
  it("bounds the share right from below", () => {
    expect(wilsonLower(0, 0)).toBe(0);
    expect(wilsonLower(48, 50)).toBeCloseTo(0.865, 3);
    expect(wilsonLower(30, 30)).toBeCloseTo(0.886, 3);
    expect(wilsonLower(300, 300)).toBeGreaterThan(0.98);
    expect(wilsonLower(0, 10)).toBeCloseTo(0, 10);
  });
});

describe("laneOf", () => {
  it("puts the model's links and table values for information", () => {
    expect(laneOf(nameCard("acme", { reasons: ["adjudicated"] }))).toBe("fyi");
    expect(laneOf(valueCard([{ text: "GBP" }], { table: "USD" }))).toBe("fyi");
  });
  it("puts everything else to decide", () => {
    expect(
      laneOf(nameCard("acme", { reasons: ["adjudicated", "unresolved"] })),
    ).toBe("decide");
    expect(laneOf(nameCard("acme", { reasons: ["part_name"] }))).toBe("decide");
    expect(laneOf(valueCard([{ text: "GBP" }]))).toBe("decide");
    expect(laneOf(heldCard(1))).toBe("decide");
  });
});

describe("settleOf", () => {
  it("refuses a card whose settle isn't its kind's", () => {
    const card = heldCard(1);
    const bad = {
      ...card,
      review: { ...card.review, settle: { kind: "stale" as const } },
    };
    expect(() => settleOf(bad)).toThrow("a claim card settles by a release");
  });
});

describe("coveredBy", () => {
  const umbrella = { key: "scus_1", entity: entity("scus_1", "Umbrella") };
  const linked = resolveDecision("d-1", {
    entityKey: "scus_1",
    applied: "pending",
  });
  const card = nameCard("umbrella holdings", { candidates: [umbrella] });
  it("finds a link of the name in another document while the load asks", () => {
    expect(coveredBy(card, ctxOf({ decisions: [linked] }))).toBe(linked);
    const written = resolveDecision("d-5", {
      entityKey: "scus_1",
      applied: "written",
    });
    expect(coveredBy(card, ctxOf({ decisions: [written] }))).toBe(written);
  });
  it("ignores a link that can't apply to this load now", () => {
    for (const applied of [
      undefined,
      "",
      "refused",
      "missed",
      "unconfirmed",
    ] as const)
      expect(
        coveredBy(
          card,
          ctxOf({
            decisions: [
              resolveDecision("d-1", { entityKey: "scus_1", applied }),
            ],
          }),
        ),
      ).toBeNull();
    // An entity the read didn't find, or one the card doesn't list.
    for (const candidates of [
      [{ key: "scus_1" }],
      [],
      [{ key: "scus_2", entity: entity("scus_2", "Other") }],
    ])
      expect(
        coveredBy(
          nameCard("umbrella holdings", { candidates }),
          ctxOf({ decisions: [linked] }),
        ),
      ).toBeNull();
  });
  it("ignores Not in data, bound names, the same document and other types", () => {
    expect(
      coveredBy(
        card,
        ctxOf({ decisions: [resolveDecision("d-2", { applied: "pending" })] }),
      ),
    ).toBeNull();
    expect(
      coveredBy(
        nameCard("umbrella holdings", {
          reasons: ["legal_form"],
          candidates: [umbrella],
        }),
        ctxOf({ decisions: [linked] }),
      ),
    ).toBeNull();
    expect(
      coveredBy(
        card,
        ctxOf({
          decisions: [
            resolveDecision("d-3", {
              doc: "doc-1",
              entityKey: "scus_1",
              applied: "pending",
            }),
          ],
        }),
      ),
    ).toBeNull();
    expect(
      coveredBy(
        card,
        ctxOf({
          decisions: [
            resolveDecision("d-4", {
              type: "Vendor",
              entityKey: "scus_1",
              applied: "pending",
            }),
          ],
        }),
      ),
    ).toBeNull();
  });
  it("reads the reason of the review the card settles, not its others", () => {
    // The card settles by its legal_form review (the one with candidates):
    // the backend saves that for the next load, so nothing is covered.
    const bound = nameCard("umbrella holdings", {
      reasons: ["legal_form", "unresolved"],
      candidates: [umbrella],
    });
    expect(asks(bound)).toBe(false);
    expect(coveredBy(bound, ctxOf({ decisions: [linked] }))).toBeNull();
    expect(
      effectOf(bound, { resolve: { entityKey: "scus_1" } }, 2, () => false),
    ).toEqual({ kind: "next_load" });
  });
});

describe("advise: names", () => {
  const umbrella = { key: "scus_1", entity: entity("scus_1", "Umbrella") };
  const other = { key: "scus_2", entity: entity("scus_2", "Umbrella Ltd") };
  const bound = (candidates: Candidate[] = [umbrella, other]) =>
    nameCard("umbrella holdings", { reasons: ["legal_form"], candidates });
  it("repeats a choice every other document agrees on", () => {
    const decisions = [
      resolveDecision("d-1", { entityKey: "scus_1", at: "2026-11-01" }),
      resolveDecision("d-2", {
        doc: "doc-3",
        entityKey: "scus_1",
        at: "2026-11-03",
      }),
    ];
    const a = suggested(advise(bound(), ctxOf({ decisions })));
    expect(a).toMatchObject({
      rule: "precedent",
      choice: { resolve: { entityKey: "scus_1" } },
      basis: [{ kind: "precedent", decisions: 2, latest: "2026-11-03" }],
      strength: { kind: "repeat", decisions: 2, by: "you" },
    });
    expect(a.batchKey).toBe(
      'precedent:["Customer","umbrella holdings","scus_1"]',
    );
    expect(offered(bound().review, a.choice)).toBe(true);
  });
  it("doesn't batch a repeat of someone else's choice", () => {
    const decisions = [
      resolveDecision("d-1", { entityKey: "scus_1" }),
      resolveDecision("d-2", {
        doc: "doc-3",
        entityKey: "scus_1",
        user: "u-2",
      }),
    ];
    const a = suggested(advise(bound(), ctxOf({ decisions })));
    expect(a.strength).toEqual({ kind: "repeat", decisions: 2, by: "team" });
    expect(a.batchKey).toBeNull();
  });
  it("repeats Not in data, which needs no candidate", () => {
    const decisions = [
      { ...resolveDecision("d-1"), decidedAt: null },
      resolveDecision("d-2", { doc: "doc-3" }),
    ];
    const card = nameCard("umbrella holdings");
    const a = suggested(advise(card, ctxOf({ decisions })));
    expect(a.choice).toEqual({ resolve: { notInData: true } });
    expect(a.batchKey).toBe('precedent:["Customer","umbrella holdings",null]');
    expect(offered(card.review, a.choice)).toBe(true);
  });
  it("suggests nothing from decisions that disagree, or name an entity it can't offer", () => {
    expect(
      advise(
        bound(),
        ctxOf({
          decisions: [
            resolveDecision("d-1", { entityKey: "scus_1" }),
            resolveDecision("d-2", { doc: "doc-3", entityKey: "scus_2" }),
          ],
        }),
      ),
    ).toEqual({ kind: "none", why: "tied_candidates" });
    expect(
      advise(
        bound(),
        ctxOf({ decisions: [resolveDecision("d-1", { entityKey: "scus_9" })] }),
      ),
    ).toEqual({ kind: "none", why: "tied_candidates" });
    // Listed, but not read in the load.
    expect(
      advise(
        bound([{ key: "scus_1" }, other]),
        ctxOf({ decisions: [resolveDecision("d-1", { entityKey: "scus_1" })] }),
      ),
    ).toEqual({ kind: "none", why: "tied_candidates" });
  });
  it("links the one candidate the load holds", () => {
    const card = nameCard("umbrella holdings", { candidates: [umbrella] });
    const a = suggested(advise(card, ctxOf()));
    expect(a).toMatchObject({
      rule: "single_candidate",
      choice: { resolve: { entityKey: "scus_1" } },
      basis: [{ kind: "only_candidate", name: "Umbrella" }],
      strength: { kind: "unmeasured" },
      batchKey: null,
    });
    expect(offered(card.review, a.choice)).toBe(true);
  });
  it("says why it can't pick a candidate", () => {
    expect(
      advise(nameCard("x", { candidates: [{ key: "scus_1" }] }), ctxOf()),
    ).toEqual({ kind: "none", why: "no_candidates" });
    expect(advise(nameCard("x"), ctxOf())).toEqual({
      kind: "none",
      why: "no_candidates",
    });
    expect(
      advise(nameCard("x", { candidates: [umbrella, other] }), ctxOf()),
    ).toEqual({ kind: "none", why: "tied_candidates" });
  });
  it("leaves the model's links alone", () => {
    expect(
      advise(
        nameCard("x", { reasons: ["adjudicated"], candidates: [umbrella] }),
        ctxOf(),
      ),
    ).toEqual({ kind: "none", why: "fyi" });
  });
});

describe("advise: held claims", () => {
  const measured = bands({ band: "0.9", right: 80, total: 80 });
  it("approves a claim of a band measured past the gate, as a batch", () => {
    const card = heldCard(1);
    const a = suggested(advise(card, ctxOf({ precision: measured })));
    expect(a).toMatchObject({
      rule: "confident_claim",
      choice: { release: { statementHash: settleOf(card).hash } },
      basis: [{ kind: "verifier", score: 0.95, band: "0.9" }],
      strength: { kind: "measured", right: 80, total: 80 },
    });
    expect(a.strength.kind === "measured" && a.strength.lower).toBeGreaterThan(
      0.95,
    );
    expect(a.batchKey).toBe('claim:["partner_of","0.9"]');
  });
  it("approves one of a band measured below the gate, alone", () => {
    for (const precision of [
      bands({ band: "0.9", right: 48, total: 50 }),
      // Every label right, but too few.
      bands({ band: "0.9", right: BATCH_LABELS - 1, total: BATCH_LABELS - 1 }),
    ]) {
      const a = suggested(advise(heldCard(1), ctxOf({ precision })));
      expect(a.rule).toBe("confident_claim");
      expect(a.batchKey).toBeNull();
    }
  });
  it("approves an unscored claim by its band's labels, with no score", () => {
    const a = suggested(
      advise(
        heldCard(1, { confidence: null }),
        ctxOf({ precision: bands({ band: "none", right: 3, total: 4 }) }),
      ),
    );
    expect(a.basis).toEqual([{ kind: "verifier", band: "none" }]);
  });
  it("says why it suggests nothing", () => {
    const why = (card: ClaimCard, more = {}) =>
      advise(card, ctxOf({ precision: measured, ...more }));
    expect(why(heldCard(1), { single: () => true })).toEqual({
      kind: "none",
      why: "one_valued",
    });
    expect(why(heldCard(1, { differs: true }))).toEqual({
      kind: "none",
      why: "differs",
    });
    expect(why(heldCard(1, { conflict: true }))).toEqual({
      kind: "none",
      why: "differs",
    });
    expect(why(heldCard(1, { named: false }))).toEqual({
      kind: "none",
      why: "waits_on_name",
    });
    expect(
      why(heldCard(1, { unsettled: [{ name: "Acme 1", by: "part_name" }] })),
    ).toEqual({ kind: "none", why: "waits_on_name" });
    // An approval of what it said before.
    const card = heldCard(1),
      s = settleOf(card);
    expect(
      why(card, {
        decisions: [
          {
            id: "d-1",
            kind: "release",
            type: "",
            docId: s.docId,
            claimKey: s.claimKey,
            claimSection: s.section,
            claimStart: s.start,
            claimEnd: s.end,
            statementHash: "old",
          },
          // Another claim's, elsewhere: read nowhere.
          {
            id: "d-2",
            kind: "reject",
            type: "",
            docId: "doc-9",
            claimKey: "k",
            claimSection: 0,
            claimStart: 0,
            claimEnd: 1,
            statementHash: "h",
          },
          {
            id: "d-3",
            kind: "prop",
            type: "Customer",
            entityKey: "c1",
            prop: "currency",
            value: { text: "GBP" },
          },
        ],
      }),
    ).toEqual({ kind: "none", why: "read_again" });
    expect(advise(heldCard(1), ctxOf())).toEqual({
      kind: "none",
      why: "unscored",
    });
    expect(
      advise(
        heldCard(1),
        ctxOf({ precision: bands({ band: "0.9", right: 5, total: 10 }) }),
      ),
    ).toEqual({ kind: "none", why: "mostly_wrong" });
  });
});

describe("advise: values", () => {
  it("chooses what two thirds of the quotes state", () => {
    const card = valueCard([
      { text: "USD" },
      { text: "GBP" },
      { text: "GBP" },
      { text: "GBP" },
    ]);
    const a = suggested(advise(card, ctxOf()));
    expect(a).toMatchObject({
      rule: "majority_value",
      choice: { prop: { text: "GBP" } },
      basis: [{ kind: "majority", agree: 3, of: 4 }],
      strength: { kind: "unmeasured" },
      batchKey: null,
    });
    expect(offered(card.review, a.choice)).toBe(true);
    expect(
      suggested(
        advise(
          valueCard([{ text: "a" }, { text: "a" }, { text: "b" }]),
          ctxOf(),
        ),
      ).choice,
    ).toEqual({ prop: { text: "a" } });
  });
  it("keeps an integer's digits", () => {
    const a = suggested(
      advise(
        valueCard([
          { integer: "9007199254740993" },
          { integer: "9007199254740993" },
          { integer: "9007199254740993" },
        ]),
        ctxOf(),
      ),
    );
    expect(a.choice).toEqual({ prop: { integer: "9007199254740993" } });
  });
  it("says why it suggests nothing", () => {
    expect(advise(valueCard([{ text: "a" }, { text: "a" }]), ctxOf())).toEqual({
      kind: "none",
      why: "few_quotes",
    });
    expect(
      advise(
        valueCard([{ text: "a" }, { text: "a" }, { text: "b" }, { text: "b" }]),
        ctxOf(),
      ),
    ).toEqual({ kind: "none", why: "values_split" });
    expect(
      advise(
        valueCard([{ text: "a" }, { text: "a" }, { text: "a" }], {
          table: "b",
        }),
        ctxOf(),
      ),
    ).toEqual({ kind: "none", why: "fyi" });
  });
});

describe("effectOf", () => {
  const never = () => false;
  it("says when each choice takes effect", () => {
    const asking = nameCard("umbrella holdings");
    expect(
      effectOf(asking, { resolve: { entityKey: "scus_1" } }, 2, never),
    ).toEqual({ kind: "applies_now", documents: 3 });
    expect(
      effectOf(asking, { resolve: { notInData: true } }, 2, never),
    ).toEqual({ kind: "applies_now", documents: 1 });
    expect(
      effectOf(
        nameCard("x", { reasons: ["part_name"] }),
        { resolve: { entityKey: "scus_1" } },
        2,
        never,
      ),
    ).toEqual({ kind: "next_load" });
    const card = heldCard(1),
      hash = settleOf(card).hash;
    expect(
      effectOf(card, { reject: { statementHash: hash } }, 0, never),
    ).toEqual({ kind: "saved_only" });
    expect(
      effectOf(card, { release: { statementHash: hash } }, 0, never),
    ).toEqual({ kind: "writes_now", facts: 1 });
    expect(
      effectOf(valueCard([{ text: "a" }]), { prop: { text: "a" } }, 0, never),
    ).toEqual({ kind: "value_now" });
  });
  it("waits for a name that holds a claim, by whether its link holds one value", () => {
    const legal = heldCard(1, {
      unsettled: [{ name: "Acme 1", by: "legal_form" }],
    });
    const release = { release: { statementHash: settleOf(legal).hash } };
    expect(effectOf(legal, release, 0, never)).toEqual({
      kind: "writes_now",
      facts: 1,
    });
    expect(effectOf(legal, release, 0, () => true)).toEqual({
      kind: "next_load",
    });
  });
  it("reads a choice that isn't its card's as what it is", () => {
    expect(
      effectOf(
        nameCard("x"),
        { release: { statementHash: "h" } },
        0,
        () => false,
      ),
    ).toEqual({ kind: "writes_now", facts: 1 });
    expect(
      effectOf(heldCard(1), { resolve: { entityKey: "k" } }, 0, () => false),
    ).toEqual({ kind: "next_load" });
  });
});

describe("cardName", () => {
  const quoted = (quote: string) => {
    const c = nameCard("umbrella holdings");
    return { ...c, review: { ...c.review, quote } };
  };
  it("quotes the name without the stop of a sentence it ends", () => {
    expect(cardName(quoted("Umbrella Holdings signed order form 3."))).toBe(
      "Umbrella Holdings signed order form 3",
    );
    expect(cardName(quoted("  Umbrella Holdings?! … "))).toBe(
      "Umbrella Holdings",
    );
    expect(cardName(quoted("Initech LLC"))).toBe("Initech LLC");
    // A stop inside the name stays.
    expect(cardName(quoted("Acme Co. Ltd"))).toBe("Acme Co. Ltd");
  });
  it("falls back to the normalized name for a quote of punctuation alone", () => {
    expect(cardName(quoted(""))).toBe("umbrella holdings");
    expect(cardName(quoted(" . "))).toBe("umbrella holdings");
  });
});

describe("claimBatchKey", () => {
  it("keys a held claim by its link and score band", () => {
    expect(claimBatchKey(heldCard(1, { confidence: 0.72 }))).toBe(
      'claim:["partner_of","0.7"]',
    );
    expect(claimBatchKey(heldCard(2, { confidence: 0.75 }))).toBe(
      claimBatchKey(heldCard(1, { confidence: 0.72 })),
    );
    expect(claimBatchKey(heldCard(1, { link: "signed_with" }))).not.toBe(
      claimBatchKey(heldCard(1)),
    );
  });
});

describe("founderSeed", () => {
  it("names the review and clips its quote", () => {
    const long = "x".repeat(500);
    const seed = founderSeed(heldCard(1, { quote: long }), "Contract-17.md");
    expect(seed).toContain("review held-1");
    expect(seed).toContain("“Contract-17.md”");
    expect(seed).toContain(
      "Acme 1 (Account) → partner_of → Globex 1 (Account)",
    );
    expect(seed).not.toContain("x".repeat(SEED_QUOTE));
    expect(seed).toContain(`${"x".repeat(SEED_QUOTE - 1)}…`);
    expect(seed).toContain("don’t settle it");
  });
  it("asks each kind's question", () => {
    const umbrella = { key: "scus_1", entity: entity("scus_1", "Umbrella") };
    const cards: [Decidable, string][] = [
      [
        nameCard("umbrella holdings", {
          candidates: [umbrella, { key: "scus_2" }],
        }),
        "Which Customer is “umbrella holdings”? Candidates: Umbrella (scus_1), scus_2 (scus_2).",
      ],
      [nameCard("nobody"), "No candidates."],
      [
        valueCard([{ text: "GBP" }, { text: "USD" }, { text: "GBP" }]),
        "Which currency of Customer c1 is right: GBP or USD?",
      ],
    ];
    for (const [card, text] of cards)
      expect(founderSeed(card, "doc.md")).toContain(text);
    expect(founderSeed(valueCard([{ text: "GBP" }]), "d")).toContain(
      "“billed in GBP”",
    );
  });
});
