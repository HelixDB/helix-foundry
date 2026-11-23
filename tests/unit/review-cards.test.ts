import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import type { FoundryReviewCard } from "@/shared/wfe";
import { normalizeName } from "@/web/areas/explore/naming";
import {
  cardId,
  cardKeys,
  cardsOf,
  claimDocs,
  heldCardId,
  reviewReads,
  windowCards,
  withOwnDecisions,
  type ReviewRead,
} from "@/web/areas/reviews/cards";
import { cardsQuery } from "@/web/areas/reviews/cards-api";
import type { QueueReview } from "@/web/areas/reviews/queue";
import {
  buildCards,
  hashUnit,
  nextDraw,
  quoteOrder,
  quoteTarget,
  rechecked,
  shuffle,
  statementHash,
  type ClaimDecision,
  type Review,
} from "../../mock/review-cards";
import {
  heldCard,
  nameCard,
  resolveDecision,
  valueCard,
} from "./__fixtures__/review-cards";

// The review cards index's rules have two homes: the backend's
// (backend/reviewcards) and the mock's (mock/review-cards.ts). The
// backend's own table of cases runs against both, so they can't drift.

type Case = {
  name: string;
  load: string;
  doc: string;
  single: string[];
  decisions: ClaimDecision[];
  reviews: Review[];
  invalid: number;
  want: {
    kind: string;
    lane: string;
    review_key: string;
    review_keys: string[];
    review_count: number;
    blocked: number;
    facts: number;
    per_decision: number;
    band?: string;
    on?: string[];
  }[];
};
const read = (name: string) =>
  JSON.parse(
    readFileSync(
      new URL(`../../backend/reviewcards/testdata/${name}`, import.meta.url),
      "utf8",
    ),
  ) as unknown;
const order = (a: Case["want"][number], b: Case["want"][number]) =>
  a.kind.localeCompare(b.kind) || a.review_key.localeCompare(b.review_key);

describe("the index's rules, as the backend's table has them", () => {
  for (const c of read("cards.json") as Case[])
    it(c.name, () => {
      const decisions = c.decisions.map((d) => {
        const [link, ...stored] = d.statement_hash.startsWith("HASH:")
          ? d.statement_hash.slice(5).split(":")
          : [];
        return link === undefined
          ? d
          : { ...d, statement_hash: statementHash(link, stored.join(":")) };
      });
      const built = buildCards(c.load, c.doc, c.reviews, decisions, (l) =>
        c.single.includes(l),
      );
      expect(built.invalid).toBe(c.invalid);
      const got = built.cards.map((card) => {
        const on = built.cards.find((n) => n.id === card.onCard);
        expect(card.shuffle).toBe(shuffle(c.load, card.id));
        return {
          kind: card.kind,
          lane: card.lane,
          review_key: card.reviewKey,
          review_keys: card.reviewKeys,
          review_count: card.reviewCount,
          blocked: card.blocked,
          facts: card.facts,
          per_decision: card.perDecision,
          ...(card.band ? { band: card.band } : {}),
          ...(on ? { on: [on.type!, on.normName!] } : {}),
        };
      });
      expect(got.sort(order)).toEqual(c.want.toSorted(order));
    });

  it("normalizes names as the backend does", () => {
    for (const [raw, want] of read("names.json") as [string, string][])
      expect(normalizeName(raw), raw).toBe(want);
  });

  it("keys a card by its decision's identity, not its review's key", () => {
    const doc = "D0000000000000000000000001";
    const reviews = (k: string): Review[] => [
      {
        review_key: `${k}-name`,
        reason: "unresolved",
        doc_id: doc,
        type: "Customer",
        norm_name: "globex",
      },
    ];
    const [a] = buildCards("L", doc, reviews("a"), [], () => false).cards;
    const [b] = buildCards("L", doc, reviews("b"), [], () => false).cards;
    expect(a!.id).toBe(b!.id);
    expect(a!.id).toBe(cardId("name", "Customer", doc, "globex"));
    expect(a!.reviewKey).not.toBe(b!.reviewKey);
  });

  it("names at most 200 of a name's reviews, the settling one first", () => {
    const doc = "D0000000000000000000000001";
    const many: Review[] = Array.from({ length: 205 }, (_, i) => ({
      review_key: `r${String(i).padStart(3, "0")}`,
      reason: "ambiguous",
      doc_id: doc,
      type: "Customer",
      norm_name: "globex",
      start: i,
      ...(i === 150 ? { candidates: ["scus_1"] } : {}),
    }));
    const [name] = buildCards("L", doc, many, [], () => false).cards;
    expect(name!.reviewCount).toBe(205);
    expect(name!.reviewKeys).toHaveLength(200);
    expect(name!.reviewKeys.slice(0, 2)).toEqual(["r150", "r000"]);
  });
});

describe("spot checks' draws", () => {
  const empty = { held: 0, quotes: 0 };
  it("takes the bands in turn, passing over an empty one", () => {
    const pools = [empty, { held: 2, quotes: 0 }, empty, empty, empty, empty];
    for (const n of [0, 1, 2, 5]) expect(nextDraw("L", n, pools)?.band).toBe(1);
    expect(nextDraw("L", 0, Array(6).fill(empty))).toBeUndefined();
  });
  it("draws a quote or a held claim in proportion to the band's", () => {
    const only = (held: number, quotes: number) =>
      nextDraw("L", 0, [{ held, quotes }, empty, empty, empty, empty, empty]);
    expect(only(0, 3)).toEqual({ band: 0, quote: true });
    expect(only(3, 0)).toEqual({ band: 0, quote: false });
    expect(only(1, 1)!.quote).toBe(hashUnit("L", "pick", 0) >= 0.5);
  });
  it("aims a quote within the band's count, and orders a document's quotes by the load", () => {
    for (let n = 0; n < 20; n++) {
      const t = quoteTarget("L", 2, n, 7);
      expect(t >= 0 && t < 7).toBe(true);
    }
    expect(quoteTarget("L", 2, 0, 0)).toBe(0);
    const qs = [0, 1, 2, 3].map((i) => ({
      section: 0,
      start: i * 10,
      end: i * 10 + 5,
      link: "signed_with",
      factKey: `f${i}`,
    }));
    const ordered = quoteOrder("L", "D", qs);
    expect(ordered.map((q) => q.factKey).sort()).toEqual([
      "f0",
      "f1",
      "f2",
      "f3",
    ]);
    const unit = (q: (typeof qs)[number]) =>
      hashUnit("L", "D", q.section, q.start, q.end, q.link);
    for (let i = 1; i < ordered.length; i++)
      expect(unit(ordered[i - 1]!)).toBeLessThanOrEqual(unit(ordered[i]!));
  });
  it("samples about one in twenty for a re-check", () => {
    const n = Array.from({ length: 2000 }, (_, i) => `id${i}`).filter(
      rechecked,
    ).length;
    expect(n).toBeGreaterThan(50);
    expect(n).toBeLessThan(150);
    for (const id of ["a", "b", "decision_1"])
      expect(rechecked(id)).toBe(hashUnit("recheck", id) < 0.05);
  });
});

describe("cardsOf", () => {
  const held = heldCard(1),
    name = nameCard("globex", { held: [] }),
    value = valueCard([{ text: "GBP" }, { text: "USD" }]);
  const byKey = new Map<string, QueueReview>(
    [held.review, ...name.reviews, value.review].map((r) => [
      (r.settle as { reviewId: string }).reviewId,
      r,
    ]),
  );
  const items: FoundryReviewCard[] = [
    {
      cardId: "n1",
      lane: "FOUNDRY_REVIEW_LANE_DECIDE",
      docId: "doc-1",
      name: {
        reviewKey: "doc-1-globex-0",
        reviewKeys: ["doc-1-globex-0"],
        reviewCount: 3,
        blocked: 2,
        held: [{ reviewKey: "held-1", band: "0.9" }],
        type: "Customer",
        normName: "globex",
      },
    },
    {
      cardId: "c1",
      docId: "doc-1",
      claim: { reviewKey: "held-1", band: "0.9" },
    },
    {
      cardId: "v1",
      lane: "FOUNDRY_REVIEW_LANE_FYI",
      docId: "doc-1",
      value: { reviewKey: "v-1" },
    },
    // Its review left the load since the index built it.
    { cardId: "gone", docId: "doc-1", value: { reviewKey: "missing" } },
    // Another kind's review: no card.
    { cardId: "odd", docId: "doc-1", claim: { reviewKey: "v-1", band: "low" } },
  ];
  it("renders each card from its reviews, in order, keying held claims by their own card's id", () => {
    const cards = cardsOf(items, byKey, []);
    expect(cards.map((c) => c.kind)).toEqual(["name", "claim", "value"]);
    const [n, c, v] = cards;
    expect(n).toMatchObject({
      key: "n1",
      lane: "decide",
      blocked: 2,
      reviewCount: 3,
      held: [{ kind: "claim", band: "0.9" }],
    });
    const s = held.review.settle as Parameters<typeof heldCardId>[0];
    expect(c!.key).toBe(heldCardId(s));
    expect(c!.key).toBe(
      cardId("release", s.docId, s.section, s.start, s.end, s.claimKey),
    );
    expect(n!.kind === "name" && n!.held[0]!.key).toBe(c!.key);
    expect(v).toMatchObject({ key: "v1", lane: "fyi" });
  });
  it("marks what holds a held claim from its document's name links", () => {
    const linked = heldCard(2);
    const s = linked.review.settle as Parameters<typeof heldCardId>[0];
    const [card] = cardsOf(
      [
        {
          cardId: "c",
          docId: "doc-1",
          claim: { reviewKey: "held-2", band: "0.9" },
        },
      ],
      new Map([["held-2", linked.review]]),
      [
        {
          reason: "part_name",
          section: 0,
          start: 0,
          candidates: [],
          settle: {
            kind: "resolve",
            reviewId: "n",
            type: s.statement.subject.type,
            docId: s.docId,
            normName: normalizeName(s.statement.subject.name),
          },
        },
      ],
    );
    expect(card!.kind === "claim" && card!.review.settle).toMatchObject({
      unsettled: [{ by: "part_name" }],
    });
  });
  it("lists the keys a page names, and its held claims' documents", () => {
    expect(cardKeys(items)).toEqual([
      "doc-1-globex-0",
      "held-1",
      "v-1",
      "missing",
    ]);
    expect(claimDocs(items)).toEqual(["doc-1"]);
    expect(claimDocs([items[2]!])).toEqual([]);
  });
  it("reads ten cards at a time, at most 200 keys a read, the documents once a window", () => {
    const claims = (n: number, from = 0): FoundryReviewCard[] =>
      Array.from({ length: n }, (_, i) => ({
        cardId: `c${from + i}`,
        docId: `doc-${(from + i) % 2}`,
        claim: { reviewKey: `k${from + i}`, band: "0.9" },
      }));
    const plan = reviewReads([
      { loadId: "L", cards: claims(25) },
      { loadId: "L" },
      { loadId: "L", cards: claims(3, 25) },
    ]);
    expect(
      plan.map((r) => [r.window, r.page, r.from, r.to, r.keys.length, r.docs]),
    ).toEqual([
      [0, 0, 0, 10, 10, ["doc-0", "doc-1"]],
      [1, 0, 10, 20, 10, ["doc-0", "doc-1"]],
      [2, 0, 20, 25, 5, ["doc-0", "doc-1"]],
      // An empty page has no window.
      [3, 2, 0, 3, 3, ["doc-0", "doc-1"]],
    ]);
    // A name whose keys alone pass 200 is a window of its own, read in
    // several; the cards beside it fill windows of their own.
    const big: FoundryReviewCard = {
      cardId: "n",
      docId: "doc-9",
      name: {
        reviewKey: "r0",
        reviewKeys: Array.from({ length: 200 }, (_, i) => `r${i}`),
        held: [{ reviewKey: "h", band: "0.9" }],
        type: "Customer",
        normName: "globex",
      },
    };
    const split = reviewReads([
      { loadId: "L", cards: [...claims(2), big, ...claims(1, 2)] },
    ]);
    expect(
      split.map((r) => [r.window, r.from, r.to, r.keys.length, r.docs]),
    ).toEqual([
      [0, 0, 2, 2, ["doc-0", "doc-1"]],
      [1, 2, 3, 200, ["doc-9"]],
      [1, 2, 3, 1, []],
      [2, 3, 4, 1, ["doc-0"]],
    ]);
  });
  it("shows the windows read, in order, stopping at the first not read", () => {
    const page = { loadId: "L", cards: items.slice(0, 3) };
    const plan: ReviewRead[] = [
      { window: 0, page: 0, from: 0, to: 2, keys: [], docs: [] },
      { window: 0, page: 0, from: 0, to: 2, keys: [], docs: [] },
      { window: 1, page: 0, from: 2, to: 3, keys: [], docs: [] },
    ];
    const all = { reviews: [...byKey.values()], links: [] };
    // The first window's second read hasn't landed: nothing shows.
    expect(windowCards([page], plan, [all, undefined, all], 9)).toMatchObject({
      cards: [],
      shown: 0,
      windows: 2,
    });
    const both = windowCards([page], plan, [all, all, all], 9);
    expect(both.cards.map((c) => c.kind)).toEqual(["name", "claim", "value"]);
    expect(both.shown).toBe(2);
    // Only the windows asked for.
    const one = windowCards([page], plan, [all, all, all], 1);
    expect([one.cards.length, one.shown, one.windows]).toEqual([2, 1, 2]);
    expect(windowCards([], [], [], 2)).toEqual({
      cards: [],
      shown: 0,
      windows: 0,
    });
  });
});

describe("cardsQuery", () => {
  it("says the lane, each kind, the type and the document, and the page", () => {
    expect(cardsQuery({ lane: "decide", kinds: [] }, "")).toBe(
      "lane=FOUNDRY_REVIEW_LANE_DECIDE",
    );
    expect(
      new URLSearchParams(
        cardsQuery(
          {
            lane: "fyi",
            kinds: ["name", "value"],
            type: "Customer",
            documentId: "doc_1",
          },
          "L:abc",
        ),
      ).getAll("kind"),
    ).toEqual([
      "FOUNDRY_REVIEW_CARD_KIND_NAME",
      "FOUNDRY_REVIEW_CARD_KIND_VALUE",
    ]);
    expect(cardsQuery({ lane: "fyi", kinds: [], type: "A B" }, "L:x")).toBe(
      "lane=FOUNDRY_REVIEW_LANE_FYI&type=A+B&pageToken=L%3Ax",
    );
  });
});

describe("withOwnDecisions", () => {
  const read = [
    {
      type: "Customer",
      normName: "umbrella holdings",
      total: 1,
      choices: 1,
      newest: [resolveDecision("d-1", { entityKey: "scus_1" })],
    },
  ];
  it("folds the walk's own decisions into the names read, newest first", () => {
    const same = resolveDecision("d-2", { doc: "doc-3", entityKey: "scus_1" });
    expect(withOwnDecisions(read, [same])).toEqual([
      { ...read[0], total: 2, choices: 1, newest: [same, read[0]!.newest[0]] },
    ]);
    const other = resolveDecision("d-3", { doc: "doc-3" });
    expect(withOwnDecisions(read, [other])[0]).toMatchObject({
      total: 2,
      choices: 2,
    });
  });
  it("adds a name the read had none of, and reads one it has again in place", () => {
    const mine = resolveDecision("d-9", { normName: "initech" });
    expect(withOwnDecisions(read, [mine])[1]).toEqual({
      type: "Customer",
      normName: "initech",
      total: 1,
      choices: 1,
      newest: [mine],
    });
    const applied = {
      ...read[0]!.newest[0]!,
      applied: { status: "written" as const },
    };
    expect(withOwnDecisions(read, [applied])).toEqual([
      { ...read[0], newest: [applied] },
    ]);
    // Only resolves; the read is left as it was.
    expect(
      withOwnDecisions(read, [{ ...applied, kind: "prop" } as never]),
    ).toEqual(read);
    expect(read[0]!.total).toBe(1);
  });
});
