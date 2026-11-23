import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import type {
  FoundryReviewCard,
  ListFoundryReviewCardsResponse,
} from "../../../shared/wfe";
import { REVIEWS_BY_KEY } from "../explore/model";
import { markDiffering, type Settle, type Settleable } from "./model";
import {
  asBand,
  bandOf,
  type ClaimCard,
  type QueueCard,
  type QueueReview,
} from "./queue";

// What the walk renders of a page of the review cards index: each listed
// card with the reviews it names, as the load holds them now. Pure: the page
// reads the cards (./cards-api.ts) and their reviews by key
// (../explore/model.ts reviewsByKeyRequest), and renders what this returns.

/**
 * A card's id as the index computes it (backend/reviewcards cardID): the
 * SHA-256 of its decision's identity, its parts joined by U+001F, in
 * hexadecimal.
 *
 * @example
 * cardId("release", "doc_1", 0, 10, 20, "ab…"); // 64 hex digits
 */
export const cardId = (...parts: (string | number)[]): string =>
  bytesToHex(sha256(new TextEncoder().encode(parts.join("\u001f"))));

type Release = Extract<Settle, { kind: "release" }>;

/**
 * A held claim's card id (the index's ClaimID): its document, place and
 * claim key. The index lists a held claim on its name's card by review key
 * only, so the walk keys it by this, the id its own card has once the name
 * is decided.
 */
export const heldCardId = (s: Release): string =>
  cardId("release", s.docId, s.section, s.start, s.end, s.claimKey);

/** A review that names a link of less than a name, as markDiffering reads it. */
export type NameLink = Settleable & {
  reason: string;
  section: number;
  start: number;
};

/**
 * The walk's cards of `items`, in their order, from the reviews read by key
 * (`reviews`) and the reviews of names the load linked on less than a name
 * in the held claims' documents (`links`), which say what holds a claim
 * (markDiffering). A card whose review the load no longer holds as the
 * index built it (read since, or another kind's) is left out until the
 * index rebuilds it; so is a held claim on a name's card.
 *
 * @example
 * cardsOf(page.cards, reviewsByKey, links); // [{ kind: "name", key: "9f…", … }]
 */
export function cardsOf(
  items: readonly FoundryReviewCard[],
  reviews: ReadonlyMap<string, QueueReview>,
  links: readonly NameLink[],
): QueueCard[] {
  const of = (key: string, kind: NonNullable<Settle["kind"]>) => {
    const r = reviews.get(key);
    return r?.settle?.kind === kind ? r : undefined;
  };
  const claim = (
    key: string,
    band: string | undefined,
    names: readonly NameLink[] | readonly QueueReview[],
  ): ClaimCard | undefined => {
    const r = of(key, "release");
    if (!r) return undefined;
    const [marked] = markDiffering([r], [...links, ...names]);
    const s = marked!.settle as Release;
    return {
      kind: "claim",
      key: heldCardId(s),
      review: marked!,
      band: asBand(band) ?? bandOf(r.confidence),
    };
  };
  return items.flatMap((c): QueueCard[] => {
    const lane = c.lane === "FOUNDRY_REVIEW_LANE_FYI" ? "fyi" : "decide";
    if (c.name) {
      const review = of(c.name.reviewKey, "resolve");
      if (!review) return [];
      const all = (c.name.reviewKeys ?? [c.name.reviewKey])
        .flatMap((k) => {
          const r = reviews.get(k);
          return r ? [r] : [];
        })
        .sort((a, b) => a.section - b.section || a.start - b.start);
      return [
        {
          kind: "name",
          key: c.cardId,
          lane,
          review,
          reviews: all,
          reviewCount: Math.max(c.name.reviewCount ?? 0, all.length),
          blocked: c.name.blocked ?? 0,
          held: (c.name.held ?? []).flatMap((h) => {
            const card = claim(h.reviewKey, h.band, all);
            return card ? [card] : [];
          }),
        },
      ];
    }
    if (c.claim) {
      const card = claim(c.claim.reviewKey, c.claim.band, []);
      return card ? [card] : [];
    }
    const review = of(c.value.reviewKey, "prop");
    return review ? [{ kind: "value", key: c.cardId, lane, review }] : [];
  });
}

/**
 * The review keys a page of cards names, each once: what the walk reads of
 * the load to render them.
 *
 * @example
 * cardKeys(page.cards); // ["r2", "r1", "h1"]
 */
export const cardKeys = (items: readonly FoundryReviewCard[]): string[] => [
  ...new Set(
    items.flatMap((c) =>
      c.name
        ? [
            ...(c.name.reviewKeys ?? [c.name.reviewKey]),
            ...(c.name.held ?? []).map((h) => h.reviewKey),
          ]
        : [(c.claim ?? c.value).reviewKey],
    ),
  ),
];

/**
 * The documents of a page's held claims, each once, sorted: whose name
 * links the walk reads with their reviews.
 */
export const claimDocs = (items: readonly FoundryReviewCard[]): string[] =>
  [
    ...new Set(
      items.flatMap((c) => (c.claim || c.name?.held?.length ? [c.docId] : [])),
    ),
  ].sort();

/** One read of a page's reviews: at most REVIEWS_BY_KEY keys. */
export type ReviewRead = { page: number; keys: string[]; docs: string[] };

/**
 * The reads of each page's reviews: its keys REVIEWS_BY_KEY at a time, the
 * first with the documents of its held claims, whose name links say what
 * holds them.
 *
 * @example
 * reviewReads([page]); // [{ page: 0, keys: ["r1", …], docs: ["doc_1"] }]
 */
export function reviewReads(
  pages: readonly ListFoundryReviewCardsResponse[],
): ReviewRead[] {
  return pages.flatMap((p, page) => {
    const keys = cardKeys(p.cards ?? []),
      docs = claimDocs(p.cards ?? []);
    return Array.from(
      { length: Math.max(1, Math.ceil(keys.length / REVIEWS_BY_KEY)) },
      (_, i) => ({
        page,
        keys: keys.slice(i * REVIEWS_BY_KEY, (i + 1) * REVIEWS_BY_KEY),
        docs: i === 0 ? docs : [],
      }),
    );
  });
}
