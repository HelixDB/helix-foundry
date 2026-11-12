import { describe, expect, it } from "vitest";
import { runQuery } from "../../mock/graph/query";
import { MockGraph } from "../../mock/graph/store";
import type {
  FoundryReviewDecision,
  FoundryReviewLabel,
  ReviewDecision,
} from "@/shared/wfe";
import {
  documentQuotesRequest,
  loadReviewCountsRequest,
  loadReviewsRequest,
  parseDocumentQuotes,
  parseLoadReviewCounts,
  parseLoadReviews,
  parseQuoteFacts,
  parseRangeQuotes,
  quoteFactsRequest,
  rangeQuotesRequest,
} from "@/web/areas/explore/model";
import { statementHash, type Unsettled } from "@/web/areas/reviews/model";
import {
  BANDS,
  BAND_RANGE,
  RANKED_PER_RANDOM,
  RECHECK_SHARE,
  bandOf,
  buildQueue,
  claimPlace,
  facts,
  hashUnit,
  labelPlace,
  perDecision,
  precision,
  quoteOffsets,
  rechecks,
  scoreText,
  type NameCard,
  type QueueCard,
  type QueueReview,
} from "@/web/areas/reviews/queue";

// The review queue (reviews/queue.ts): which reviews it holds, in the order
// of the facts each decision writes, with one card in ten drawn at random
// from a score band; and the reads of a load it is built from
// (explore/model.ts).

const key = (n: number) => n.toString(16).padStart(64, "0");

/** A held claim of `doc`, the nth, stating subject → link → object. */
function held(
  n: number,
  {
    doc = "doc-1",
    // null: no score (unverified).
    confidence = 0.75 as number | null,
    subject = { type: "Account", name: `Acme ${n}` },
    object = { type: "Account", name: `Globex ${n}` },
    link = "partner_of",
    named = true,
    reason = "low_confidence",
    unsettled = undefined as Unsettled[] | undefined,
  } = {},
): QueueReview {
  const stored = JSON.stringify({ subject, object, props: {} });
  return {
    documentId: doc,
    section: 0,
    start: n * 10,
    end: n * 10 + 8,
    quote: `quote ${n}`,
    reason,
    ...(confidence === null ? {} : { confidence }),
    suggestedLink: link,
    candidates: [],
    claim: { place: [key(n), 0, n * 10, n * 10 + 8].join("\u001f"), stored },
    settle: {
      kind: "release",
      reviewId: `held-${n}`,
      docId: doc,
      claimKey: key(n),
      section: 0,
      start: n * 10,
      end: n * 10 + 8,
      link,
      statement: { subject, object, props: {} },
      stored,
      hash: statementHash(link, stored),
      named,
      ...(unsettled ? { unsettled } : {}),
      ...(reason === "conflict" ? { conflict: true as const } : {}),
    },
  };
}

/** A review of a name in `doc`: of reason, blocking a claim of `link` if given. */
function name(
  id: string,
  normName: string,
  {
    doc = "doc-1",
    type = "Account",
    reason = "unresolved",
    link = undefined as string | undefined,
    at = 0,
  } = {},
): QueueReview {
  return {
    documentId: doc,
    section: 1,
    start: at,
    end: at + 4,
    quote: `name ${normName}`,
    reason,
    ...(link ? { suggestedLink: link } : {}),
    candidates: [{ key: "acct_1" }],
    settle: { kind: "resolve", reviewId: id, type, docId: doc, normName },
  };
}

function value(id: string): QueueReview {
  return {
    documentId: "doc-1",
    section: 2,
    start: 0,
    end: 5,
    quote: "billed in GBP",
    reason: "prop_conflict",
    prop: "currency",
    candidates: [],
    settle: {
      kind: "prop",
      reviewId: id,
      type: "Customer",
      entityKey: "c1",
      prop: "currency",
      values: [],
    },
  };
}

const many = () => false;
const build = (
  reviews: QueueReview[],
  more: Partial<Parameters<typeof buildQueue>[0]> = {},
) =>
  buildQueue({
    reviews,
    decisions: [],
    single: many,
    quotes: {},
    seed: "load-1",
    ...more,
  });
const keys = (cards: QueueCard[]) => cards.map((c) => c.key);

describe("bands and hashes", () => {
  it("bands a score by the tenths the random slice draws from", () => {
    expect(bandOf(1)).toBe("0.9");
    expect(bandOf(0.9)).toBe("0.9");
    expect(bandOf(0.8999)).toBe("0.8");
    expect(bandOf(0.8)).toBe("0.8");
    expect(bandOf(0.7)).toBe("0.7");
    expect(bandOf(0.6)).toBe("0.6");
    expect(bandOf(0.59)).toBe("low");
    expect(bandOf(undefined)).toBe("none");
    expect(bandOf(Number.NaN)).toBe("none");
    expect(BANDS[0]).toBe("0.9");
  });
  it("hashes the same parts to the same number in [0, 1)", () => {
    const a = hashUnit("load-1", "x", 3);
    expect(hashUnit("load-1", "x", 3)).toBe(a);
    expect(a).toBeGreaterThanOrEqual(0);
    expect(a).toBeLessThan(1);
    expect(hashUnit("load-2", "x", 3)).not.toBe(a);
    // Spread out: a thousand draws fill every tenth.
    const tenths = new Set(
      Array.from({ length: 1000 }, (_, i) => Math.floor(hashUnit("s", i) * 10)),
    );
    expect(tenths.size).toBe(10);
  });
});

describe("buildQueue", () => {
  it("ranks held claims, names by facts per decision, then values", () => {
    // A name blocking three accepted claims writes three with one decision;
    // one with only a held claim on it writes one with two.
    const blocking = [0, 1, 2].map((i) =>
      name(`n-big-${i}`, "initech", { link: "partner_of", at: i * 10 }),
    );
    const lonely = name("n-small", "acme 9");
    const waiting = held(9, { named: false });
    const claims = [1, 2].map((n) => held(n));
    // An accepted fact is the one random draw (the top band, which holds
    // no held claim here), so the ranking keeps every card.
    const queue = build(
      [...claims, ...blocking, lonely, waiting, value("v1")],
      {
        quotes: { "0.9": 100 },
      },
    );
    const ranked = queue.cards.filter((c) => c.drawn === "ranked");
    const big = ranked.find(
      (c): c is NameCard => c.kind === "name" && c.blocked === 3,
    )!;
    // One card per name: its decision settles every review of it there.
    expect(big.reviews).toHaveLength(3);
    expect(facts(big)).toBe(3);
    expect(perDecision(big)).toBe(3);
    const small = ranked.find(
      (c): c is NameCard => c.kind === "name" && c.blocked === 0,
    )!;
    expect(small.held.map((c) => c.key)).toEqual(["held-9"]);
    expect(perDecision(small)).toBe(0.5);
    // The big name first, held claims next (one fact each, before the value
    // at one value), the name of two decisions for one fact last.
    expect(ranked[0]).toBe(big);
    expect(ranked.slice(1, 3).map((c) => c.kind)).toEqual(["claim", "claim"]);
    expect(ranked[3]!.kind).toBe("value");
    expect(ranked.at(-1)).toBe(small);
    // A held claim that waits on a name is on its card, not its own.
    expect(
      queue.cards.some((c) => c.kind === "claim" && c.key === "held-9"),
    ).toBe(false);
  });

  it("puts a held claim on the card of a name linked on less than a name, where that holds it", () => {
    const part = name("n-part", "westmere", { reason: "part_name" });
    const legal = name("n-legal", "globex ltd", { reason: "legal_form" });
    const onPart = held(1, {
      subject: { type: "Account", name: "Westmere" },
      unsettled: [{ name: "Westmere", by: "part_name" }],
    });
    const onLegal = held(2, {
      object: { type: "Account", name: "Globex Ltd" },
      unsettled: [{ name: "Globex Ltd", by: "legal_form" }],
    });
    // A legal form holds a claim only of a link that holds one value.
    let queue = build([part, legal, onPart, onLegal], {
      quotes: { "0.9": 100 },
    });
    const card = (k: string) =>
      queue.cards.find((c): c is NameCard => c.key.includes(k))!;
    expect(card("westmere").held.map((c) => c.key)).toEqual(["held-1"]);
    expect(card("globex").held).toEqual([]);
    expect(keys(queue.cards)).toContain("held-2");
    queue = build([part, legal, onPart, onLegal], {
      single: () => true,
      quotes: { "0.9": 100 },
    });
    expect(card("globex").held.map((c) => c.key)).toEqual(["held-2"]);
  });

  it("keeps a name per document: the same name elsewhere is another decision", () => {
    const queue = build([
      name("a", "initech", { doc: "doc-1", link: "partner_of" }),
      name("b", "initech", { doc: "doc-2", link: "partner_of" }),
    ]);
    expect(queue.cards.filter((c) => c.kind === "name")).toHaveLength(2);
  });

  it("leaves out what is decided, settles nothing here, or writes nothing", () => {
    const approved = held(1),
      rejected = held(2),
      movedKey = held(3),
      left = held(4);
    const s = (r: QueueReview) =>
      r.settle as Extract<QueueReview["settle"], { kind: "release" }>;
    const claim = (
      r: QueueReview,
      kind: "release" | "reject",
      claimKey = s(r).claimKey,
    ): ReviewDecision => ({
      kind,
      type: "",
      docId: s(r).docId,
      claimKey,
      claimSection: s(r).section,
      claimStart: s(r).start,
      claimEnd: s(r).end,
      statementHash: s(r).hash,
    });
    const decisions: ReviewDecision[] = [
      claim(approved, "release"),
      claim(rejected, "reject"),
      // Saved under the key a rebuild moved, of what it reads now.
      claim(movedKey, "release", key(99)),
      { kind: "resolve", type: "Account", docId: "doc-1", normName: "acme" },
      {
        kind: "prop",
        type: "Customer",
        entityKey: "c1",
        prop: "currency",
        value: { text: "GBP" },
      },
    ];
    const twoWays = held(5);
    (twoWays.settle as { differs?: boolean }).differs = true;
    const unmatched: QueueReview = {
      ...held(6),
      reason: "unmatched",
      settle: undefined,
    };
    const stale: QueueReview = { ...held(7), settle: { kind: "stale" } };
    const conflict = held(8, { reason: "conflict" });
    // An approval of what the claim said before (its key, another
    // statement): the load refuses it, and the card asks to be read again.
    const older = held(9);
    decisions.push(claim(older, "release"));
    (decisions.at(-1) as { statementHash: string }).statementHash = key(42);
    const queue = build(
      [
        approved,
        rejected,
        movedKey,
        left,
        older,
        name("n", "acme"),
        value("v"),
        twoWays,
        unmatched,
        stale,
        conflict,
      ],
      // The random slot's draw is a fact, from the top band.
      { decisions, quotes: { "0.9": 100 } },
    );
    expect(
      queue.cards.filter((c) => c.drawn === "ranked").map((c) => c.key),
    ).toEqual(expect.arrayContaining(["held-4", "held-9"]));
    expect(queue.cards.filter((c) => c.drawn === "ranked")).toHaveLength(2);
    expect(queue.left).toEqual({
      decided: 5,
      unsettled: 1,
      // Read two ways, and a conflict of a link that holds many values.
      blocked: 2,
      stale: 1,
    });
    // A conflict of a link that holds one value is approved as a claim.
    const one = build([conflict], { single: () => true });
    expect(keys(one.cards)).toEqual(["held-8"]);
    expect(one.cards[0]!.drawn).toBe("random");
    // Decided here, kept in place until the page reads again.
    const kept = build([approved, left], {
      decisions,
      keep: new Set(["held-1"]),
      quotes: { "0.9": 100 },
    });
    expect(
      kept.cards
        .filter((c) => c.kind === "claim")
        .map((c) => c.key)
        .toSorted(),
    ).toEqual(["held-1", "held-4"]);
    // A name's card is kept by its own key: every review of the name.
    const nameCard = build([name("n", "acme")]).cards.find(
      (c) => c.kind === "name",
    )!;
    expect(
      keys(
        build([name("n", "acme"), name("n2", "acme", { at: 20 })], {
          decisions,
          keep: new Set([nameCard.key]),
        }).cards,
      ),
    ).toEqual([nameCard.key]);
  });

  it("draws one card in ten at random, a band at a time, after every nine ranked", () => {
    // 27 held claims across the bands, nothing else.
    const scores = [0.95, 0.85, 0.75, 0.65, 0.5, null];
    const reviews = Array.from({ length: 27 }, (_, n) =>
      held(n, { confidence: scores[n % scores.length] }),
    );
    const queue = build(reviews);
    const random = queue.cards.filter((c) => c.drawn === "random");
    // ceil(27 / 9) = 3 drawn from 27, leaving 24 ranked.
    expect(random).toHaveLength(3);
    expect(queue.cards).toHaveLength(27);
    // Each after nine ranked cards: positions 9, 19, then 26 at the end.
    expect(
      queue.cards.flatMap((c, i) => (c.drawn === "random" ? [i] : [])),
    ).toEqual([RANKED_PER_RANDOM, 2 * RANKED_PER_RANDOM + 1, 26]);
    // Three bands, in turn from the top.
    const band = (c: QueueCard) =>
      c.kind === "claim" || c.kind === "quote" || c.kind === "decided"
        ? c.band
        : undefined;
    expect(random.map(band)).toEqual(["0.9", "0.8", "0.7"]);
    // A claim drawn is only there.
    const ids = keys(queue.cards);
    expect(new Set(ids).size).toBe(ids.length);
    // The same load draws the same; another load others.
    expect(keys(build(reviews).cards)).toEqual(ids);
    expect(keys(build(reviews, { seed: "load-2" }).cards)).not.toEqual(ids);
    expect(queue.pool).toMatchObject({
      "0.9": { held: 5, quotes: 0 },
      "0.8": { held: 5, quotes: 0 },
      none: { held: 4, quotes: 0 },
    });
  });

  it("takes the bands in turn, passing over one with nothing left", () => {
    // 10 at the top, 1 at 0.8, 10 at 0.7: three draws, one a band.
    const reviews = Array.from({ length: 21 }, (_, n) =>
      held(n, { confidence: n < 10 ? 0.95 : n === 10 ? 0.85 : 0.75 }),
    );
    const bands = build(reviews)
      .cards.filter((c) => c.drawn === "random")
      .map((c) => (c.kind === "claim" ? c.band : undefined));
    expect(bands).toEqual(["0.9", "0.8", "0.7"]);
  });

  it("draws from the whole band, decided claims and facts the graph holds included, but those labelled at random", () => {
    // Nine held claims at 0.7 a person approved elsewhere (their reviews
    // still in the load, as approvals refused or waiting are), and one ranked.
    const decidedElsewhere = Array.from({ length: 9 }, (_, n) =>
      held(n, { confidence: 0.75 }),
    );
    const s = (r: QueueReview) =>
      r.settle as Extract<QueueReview["settle"], { kind: "release" }>;
    const decisions: ReviewDecision[] = decidedElsewhere.map((r) => ({
      kind: "reject",
      type: "",
      docId: s(r).docId,
      claimKey: s(r).claimKey,
      claimSection: s(r).section,
      claimStart: s(r).start,
      claimEnd: s(r).end,
      statementHash: s(r).hash,
    }));
    const open = held(20, { confidence: 0.75 });
    const queue = build([...decidedElsewhere, open], { decisions });
    // One ranked card (the open claim), so one draw, from all ten.
    expect(queue.pool["0.7"]).toEqual({ held: 10, quotes: 0 });
    const random = queue.cards.find((c) => c.drawn === "random")!;
    expect(["decided", "claim"]).toContain(random.kind);
    // Over many loads, the decided nine are drawn about nine times in ten.
    const decided = Array.from({ length: 400 }, (_, i) =>
      build([...decidedElsewhere, open], { decisions, seed: `load-${i}` }),
    ).filter((q) =>
      q.cards.some((c) => c.drawn === "random" && c.kind === "decided"),
    ).length;
    expect(decided).toBeGreaterThan(320);
    expect(decided).toBeLessThan(390);
    // A claim labelled at random is out of the pool; one labelled on this
    // page keeps its place.
    const labelled = new Set(
      decidedElsewhere.map((r) => claimPlace(r, r.suggestedLink)),
    );
    const left = build([...decidedElsewhere, open], {
      decisions,
      labelled,
    });
    expect(left.pool["0.7"]).toEqual({ held: 1, quotes: 0 });
    const kept = build([...decidedElsewhere, open], {
      decisions,
      labelled,
      keep: new Set(["held-3"]),
    });
    expect(kept.pool["0.7"]).toEqual({ held: 2, quotes: 0 });
    // Facts the graph holds in a band are drawn as their share of it.
    const facts = build([open, held(21, { confidence: 0.75 })], {
      quotes: { "0.7": 1000 },
    });
    expect(facts.cards.find((c) => c.drawn === "random")).toMatchObject({
      kind: "quote",
      band: "0.7",
      draw: 0,
    });
  });

  it("draws accepted facts in the top band, in proportion to its held claims", () => {
    const reviews = Array.from({ length: 9 }, (_, n) =>
      held(n, { confidence: 0.7 }),
    );
    // No held claim at the top: its draw is an accepted fact.
    const queue = build(reviews, { quotes: { "0.9": 40 } });
    const random = queue.cards.filter((c) => c.drawn === "random");
    expect(random).toHaveLength(1);
    expect(random[0]).toMatchObject({ kind: "quote", draw: 0, band: "0.9" });
    // Many draws: mostly facts where they outnumber the held claims there.
    const top = Array.from({ length: 90 }, (_, n) =>
      held(n, { confidence: n < 2 ? 0.95 : 0.7 }),
    );
    const mixed = build(top, { quotes: { "0.9": 1000 } }).cards.filter(
      (c) =>
        c.drawn === "random" &&
        (c.kind === "claim" || c.kind === "quote") &&
        c.band === "0.9",
    );
    expect(mixed.filter((c) => c.kind === "quote").length).toBeGreaterThan(
      mixed.filter((c) => c.kind === "claim").length,
    );
    // Draws number on.
    expect(mixed.flatMap((c) => (c.kind === "quote" ? [c.draw] : []))).toEqual(
      Array.from(
        { length: mixed.filter((c) => c.kind === "quote").length },
        (_, i) => i,
      ),
    );
  });

  it("draws from a name's held claims too, taking them off its card", () => {
    const n = name("n", "acme 1");
    const waiting = held(1, { named: false, confidence: 0.95 });
    const queue = build([n, waiting]);
    // One ranked card (the name), so one random: the held claim on it.
    const random = queue.cards.find((c) => c.drawn === "random");
    expect(random?.key).toBe("held-1");
    expect(
      queue.cards.find((c): c is NameCard => c.kind === "name")?.held,
    ).toEqual([]);
  });

  it("draws nothing where there is nothing ranked, or no pool", () => {
    expect(build([]).cards).toEqual([]);
    expect(build([value("v")]).cards.map((c) => c.drawn)).toEqual(["ranked"]);
  });
});

describe("quoteOffsets and scores", () => {
  it("gives up to eight distinct offsets in range, the same for the same draw", () => {
    const a = quoteOffsets("load-1", "0.9", 0, 40);
    expect(a).toHaveLength(8);
    expect(new Set(a).size).toBe(8);
    expect(a.every((o) => o >= 0 && o < 40 && Number.isInteger(o))).toBe(true);
    expect(quoteOffsets("load-1", "0.9", 0, 40)).toEqual(a);
    expect(quoteOffsets("load-1", "0.9", 1, 40)).not.toEqual(a);
    expect(quoteOffsets("load-1", "0.8", 0, 40)).not.toEqual(a);
    // Fewer quotes than offsets: every one, once.
    expect(quoteOffsets("load-1", "0.9", 0, 3).toSorted()).toEqual([0, 1, 2]);
    expect(quoteOffsets("load-1", "0.9", 0, 0)).toEqual([]);
  });
  it("shows a score cut to two decimals, never the next band's", () => {
    expect(scoreText(0.8999)).toBe("0.89");
    expect(scoreText(0.9)).toBe("0.90");
    expect(scoreText(0.72)).toBe("0.72");
    expect(bandOf(0.8999)).toBe("0.8");
  });
});

// Each label of a claim of its own, at its own place.
let placed = 0;
const label = (
  id: string,
  over: Partial<FoundryReviewLabel> = {},
): FoundryReviewLabel => {
  placed += 10;
  return {
    id,
    drawn: "random",
    docId: "doc-1",
    claimSection: 0,
    claimStart: placed,
    claimEnd: placed + 5,
    factKey: `fact-${id}`,
    link: "partner_of",
    verdict: "right",
    decidedByUserId: "ann",
    ...over,
  };
};

describe("rechecks and precision", () => {
  const decision = (
    id: string,
    over: Partial<Extract<FoundryReviewDecision, { kind: "release" }>> = {},
  ): FoundryReviewDecision => ({
    id,
    kind: "release",
    type: "",
    docId: "doc-1",
    claimKey: key(Number.parseInt(id.replace(/\D/g, "") || "0")),
    claimSection: 0,
    claimStart: 1,
    claimEnd: 5,
    statementHash: key(7),
    decidedByUserId: "ann",
    drawn: "ranked",
    ...over,
  });
  it("offers a second person about one in twenty of others' queue words", () => {
    const decisions = Array.from({ length: 2000 }, (_, i) => decision(`d${i}`));
    const offered = rechecks(decisions, [], "bob");
    const share = offered.length / decisions.length;
    expect(share).toBeGreaterThan(RECHECK_SHARE / 2);
    expect(share).toBeLessThan(RECHECK_SHARE * 2);
    expect(offered[0]).toMatchObject({ kind: "decision", against: "right" });
    // Never their own, nor a decision made outside the queue, nor a name's.
    expect(rechecks(decisions, [], "ann")).toEqual([]);
    expect(
      rechecks(
        decisions.map((d) => ({ ...d, drawn: "" as const })),
        [],
        "bob",
      ),
    ).toEqual([]);
    // A rejection checks "wrong".
    const sampled = offered[0]!.key.replace("decision-", "");
    const rejected = rechecks(
      [decision(sampled, { kind: "reject" } as never)],
      [],
      "bob",
    );
    expect(rejected[0]).toMatchObject({ against: "wrong" });
  });
  it("leaves out a claim the person has said something of", () => {
    const labels = Array.from({ length: 2000 }, (_, i) => label(`l${i}`));
    const offered = rechecks([], labels, "bob");
    expect(offered.length).toBeGreaterThan(0);
    expect(offered[0]).toMatchObject({ kind: "label", against: "right" });
    const first = offered[0]!.kind === "label" ? offered[0]!.label : undefined;
    const mine = label("mine", {
      ...first,
      id: "mine",
      decidedByUserId: "bob",
    });
    expect(
      rechecks([], [...labels, mine], "bob").map((r) => r.key),
    ).not.toContain(offered[0]!.key);
    // A label that came with a decision is that decision's to re-check.
    expect(
      rechecks(
        [],
        labels.map((l) => ({ ...l, decisionId: `d-${l.id}` })),
        "bob",
      ),
    ).toEqual([]);
    // A held claim's label with no decision (a decided claim drawn at
    // random) is re-checked as a fact's is.
    expect(
      rechecks(
        [],
        labels.map((l) => ({ ...l, factKey: undefined, claimKey: key(1) })),
        "bob",
      ),
    ).toHaveLength(offered.length);
    // Nor a decision of a claim the person labelled, at its place.
    const d = rechecks(
      Array.from({ length: 2000 }, (_, i) => decision(`d${i}`)),
      [],
      "bob",
    )[0]!;
    const at =
      d.kind === "decision"
        ? label("bobs", {
            decidedByUserId: "bob",
            claimStart: d.decision.claimStart,
            claimEnd: d.decision.claimEnd,
          })
        : undefined;
    expect(
      rechecks(
        Array.from({ length: 2000 }, (_, i) => decision(`d${i}`)),
        [at!],
        "bob",
      ),
    ).toEqual([]);
  });
  it("counts the random slice's word per band, and the re-checks' agreement", () => {
    const b = label("b", {
      confidence: 0.92,
      verdict: "wrong",
      decidedAt: "2026-11-12T10:00:00Z",
    });
    const p = precision([
      label("a", { confidence: 0.95 }),
      b,
      // The same claim labelled again by another, later: counted once.
      {
        ...b,
        id: "b2",
        verdict: "right",
        decidedByUserId: "cat",
        decidedAt: "2026-11-12T11:00:00Z",
      },
      label("c", { confidence: 0.85 }),
      label("d", { confidence: undefined }),
      label("e", { drawn: "recheck", against: "right", verdict: "right" }),
      label("f", { drawn: "recheck", against: "right", verdict: "wrong" }),
    ]);
    expect(p.bands).toEqual([
      { band: "0.9", right: 1, total: 2 },
      { band: "0.8", right: 1, total: 1 },
      { band: "none", right: 1, total: 1 },
    ]);
    expect(p.rechecks).toEqual({ agreed: 1, total: 2 });
    expect(precision([])).toEqual({
      bands: [],
      rechecks: { agreed: 0, total: 0 },
    });
  });
  it("names a claim by its document, place and link", () => {
    const x = label("x");
    expect(labelPlace(x)).toBe(
      claimPlace(
        {
          documentId: "doc-1",
          section: 0,
          start: x.claimStart!,
          end: x.claimEnd!,
        },
        "partner_of",
      ),
    );
    expect(labelPlace(x)).not.toBe(labelPlace(label("y")));
    expect(labelPlace(x)).not.toBe(labelPlace({ ...x, link: "owns" }));
  });
});

// A load in a mock graph: two documents' sections, reviews of them and of a
// table's rows, accepted and held quotes, and a fact's ends.
function loadGraph() {
  const g = new MockGraph(),
    L = { foundry_load: "load-1" };
  const section = (doc: string, ordinal: number) =>
    g.addNode("FoundrySection", { ...L, doc_id: doc, ordinal });
  const s0 = section("doc-1", 0),
    s1 = section("doc-1", 1),
    s2 = section("doc-2", 0);
  const acme = g.addNode("Account", { ...L, norm_name: "acme", name: "Acme" }),
    globex = g.addNode("Account", {
      ...L,
      norm_name: "globex",
      name: "Globex",
    });
  g.addEdge("partner_of", acme, globex, { ...L, fact_key: "fk-1" });
  const quote = (s: number, doc: string, fk: string, confidence: number) => {
    const q = g.addNode("FoundryEvidence", {
      ...L,
      fact_key: fk,
      link: "partner_of",
      doc_id: doc,
      start: 3,
      end: 9,
      quote: `q ${fk}`,
      confidence,
    });
    g.addEdge("FOUNDRY_FROM_SECTION", q, s, L);
  };
  quote(s0, "doc-1", "fk-1", 0.95);
  quote(s1, "doc-1", "fk-2", 0.9);
  quote(s2, "doc-2", "fk-3", 0.72); // released: under the bar
  // A property's quote at the bar: no fact key, no fact to label.
  const prop = g.addNode("FoundryEvidence", {
    ...L,
    prop: "currency",
    value: "GBP",
    doc_id: "doc-1",
    start: 1,
    end: 4,
    confidence: 0.94,
  });
  g.addEdge("FOUNDRY_FROM_SECTION", prop, s0, L);
  // A row's evidence, scored, in no section.
  g.addNode("FoundryEvidence", {
    ...L,
    fact_key: "fk-4",
    source: "crm",
    confidence: 1,
  });
  const review = (props: Record<string, unknown>, s?: number) => {
    const r = g.addNode("FoundryReview", { ...L, ...props } as never);
    if (s !== undefined) g.addEdge("FOUNDRY_FROM_SECTION", r, s, L);
    return r;
  };
  const named = review(
    {
      review_key: "r-name",
      reason: "unresolved",
      doc_id: "doc-1",
      section: 1,
      start: 0,
      end: 4,
      quote: "Acme",
      type: "Account",
      norm_name: "acme",
      candidates: ["acme"],
    },
    s1,
  );
  g.addEdge("FOUNDRY_CONCERNS", named, acme, L);
  review({ review_key: "r-row", chunk_key: "c", kind: "row" });
  // Another load's review is not this load's.
  g.addNode("FoundryReview", {
    foundry_load: "load-0",
    review_key: "old",
    doc_id: "doc-1",
  });
  return g;
}

const ask = (g: MockGraph, request: unknown) => {
  const answer = runQuery(g, request);
  expect(answer.status).toBe(200);
  return answer.body;
};

describe("a load's reviews, as the queue reads them", () => {
  const scope = { id: "load-1" };
  it("counts its reviews, and its documents' quotes of facts in each score band", () => {
    const g = loadGraph();
    const names = Object.keys(BAND_RANGE);
    expect(
      parseLoadReviewCounts(
        ask(g, loadReviewCountsRequest(scope, BAND_RANGE)),
        names,
      ),
    ).toEqual({
      reviews: 2,
      // Two accepted, one approved at 0.72; no property's quote, no row's.
      quotes: { "0.9": 2, "0.8": 0, "0.7": 1, "0.6": 0, low: 0 },
    });
    expect(parseLoadReviewCounts(null, names)).toEqual({
      reviews: 0,
      quotes: { "0.9": 0, "0.8": 0, "0.7": 0, "0.6": 0, low: 0 },
    });
  });
  it("reads its reviews a page at a time, those of documents with what settles them", () => {
    const g = loadGraph();
    const reviews = parseLoadReviews(ask(g, loadReviewsRequest(scope, 0)));
    expect(reviews).toHaveLength(1);
    expect(reviews[0]).toMatchObject({
      documentId: "doc-1",
      section: 1,
      reason: "unresolved",
      candidates: [{ key: "acme", entity: { name: "Acme" } }],
      settle: { kind: "resolve", reviewId: "r-name", normName: "acme" },
    });
    expect(parseLoadReviews(ask(g, loadReviewsRequest(scope, 1)))).toEqual([]);
    expect(parseLoadReviews(undefined)).toEqual([]);
  });
  it("reads a band's quotes at offsets, each with its section, and their facts' ends", () => {
    const g = loadGraph();
    const quotes = parseRangeQuotes(
      ask(g, rangeQuotesRequest(scope, BAND_RANGE["0.9"], [0, 1, 5])),
      3,
    );
    expect(quotes[0]).toMatchObject({
      documentId: "doc-1",
      factKey: "fk-1",
      section: 0,
      start: 3,
      end: 9,
      confidence: 0.95,
    });
    expect(quotes[1]).toMatchObject({ factKey: "fk-2", section: 1 });
    expect(quotes[2]).toBeUndefined();
    const read = quotes.filter((q) => !!q);
    const ends = parseQuoteFacts(
      ask(g, quoteFactsRequest(scope, read)!),
      read.length,
    );
    expect(ends[0]?.from?.name).toBe("Acme");
    expect(ends[0]?.to?.name).toBe("Globex");
    // fk-2 has no edge in this load.
    expect(ends[1]).toEqual({});
    expect(quoteFactsRequest(scope, [])).toBeNull();
    // A band below the top: its quotes only.
    expect(
      parseRangeQuotes(
        ask(g, rangeQuotesRequest(scope, BAND_RANGE["0.7"], [0, 1])),
        2,
      ).map((q) => q?.factKey),
    ).toEqual(["fk-3", undefined]);
  });
  it("reads a document's quotes at their places", () => {
    const g = loadGraph();
    const quotes = parseDocumentQuotes(
      ask(g, documentQuotesRequest(scope, ["doc-2", "doc-1"])!),
      ["doc-2", "doc-1"],
    );
    expect(quotes.map((q) => [q.documentId, q.factKey, q.section])).toEqual([
      ["doc-2", "fk-3", 0],
      ["doc-1", "fk-1", 0],
      ["doc-1", "fk-2", 1],
    ]);
    expect(documentQuotesRequest(scope, [])).toBeNull();
  });
});
