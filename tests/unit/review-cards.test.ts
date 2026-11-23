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
import { heldCard, nameCard, valueCard } from "./__fixtures__/review-cards";

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
  it("reads a page's reviews 200 keys at a time, the documents once", () => {
    const many: FoundryReviewCard[] = Array.from({ length: 250 }, (_, i) => ({
      cardId: `c${i}`,
      docId: `doc-${i % 2}`,
      claim: { reviewKey: `k${i}`, band: "0.9" },
    }));
    const plan = reviewReads([{ loadId: "L", cards: many }, { loadId: "L" }]);
    expect(plan.map((r) => [r.page, r.keys.length, r.docs])).toEqual([
      [0, 200, ["doc-0", "doc-1"]],
      [0, 50, []],
      [1, 0, []],
    ]);
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
