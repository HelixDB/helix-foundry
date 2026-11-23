import type { FoundryReviewLabel } from "../../../shared/wfe";
import type { EntityReview } from "../explore/model";

// The review walk's cards, as the page renders them: each card the review
// cards index lists (./cards-api.ts), with the reviews it names read from the
// load (./cards.ts cardsOf). The index decides what a card is, its lane and
// its order (backend/reviewcards, and its twin mock/review-cards.ts); the
// page only renders and decides.
//
// What a decision writes:
// - a held claim whose ends are in the graph writes one fact, at once;
// - a name (a resolve decision of a document's name: the load matched it to
//   no row, several, or one on less than a name) writes, from the next
//   load, every accepted claim it blocks in that document, and lets each
//   held claim that waits on it be approved: those follow it on its card. A
//   name decision is per document;
// - a value of a property documents state differently writes one value.

/** A review as a card reads it: at its place in its document. */
export type QueueReview = EntityReview;

/** A score band of the spot checks, by the verifier's score. */
export type Band = "0.9" | "0.8" | "0.7" | "0.6" | "low" | "none";

/** The bands in the order spot checks take them, accepted facts first. */
export const BANDS: readonly Band[] = [
  "0.9",
  "0.8",
  "0.7",
  "0.6",
  "low",
  "none",
];

/** What a band says on a card and in the precision table. */
export const BAND_TEXT: Record<Band, string> = {
  "0.9": "0.9 and above",
  "0.8": "0.8 to 0.9",
  "0.7": "0.7 to 0.8",
  "0.6": "0.6 to 0.7",
  low: "below 0.6",
  none: "no score",
};

/**
 * A verifier's score's band: none for a claim with no score (unverified),
 * or for 0, a claim with no score that a person approved.
 *
 * @example
 * bandOf(0.93); // "0.9"
 * bandOf(0.85); // "0.8"
 * bandOf(undefined); // "none"
 * bandOf(0); // "none": approved with no score
 */
export function bandOf(confidence: number | undefined): Band {
  if (
    confidence === undefined ||
    !Number.isFinite(confidence) ||
    confidence <= 0
  )
    return "none";
  if (confidence >= 0.9) return "0.9";
  if (confidence >= 0.8) return "0.8";
  if (confidence >= 0.7) return "0.7";
  if (confidence >= 0.6) return "0.6";
  return "low";
}

/** A band's name as the index sends it; none for anything else. */
export const asBand = (b: string | undefined): Band | undefined =>
  BANDS.find((x) => x === b);

/**
 * A score as a card shows it: two decimals, cut rather than rounded, so it
 * never reads as the next band's (0.899 is 0.89, not 0.90).
 *
 * @example
 * scoreText(0.8999); // "0.89"
 * scoreText(0.9); // "0.90"
 */
export const scoreText = (confidence: number) =>
  (Math.floor(confidence * 100 + 1e-9) / 100).toFixed(2);

/** Where a card goes: to decide, or for information. */
export type Lane = "decide" | "fyi";

/** A held claim's card: its review and its score band. */
export type ClaimCard = {
  kind: "claim";
  key: string;
  review: QueueReview;
  band: Band;
};

/**
 * A name's card: the review it settles by (`review`; every review of the
 * name in its document shares the decision), the name's reviews read
 * (`reviews`, at most 200) of how many (`reviewCount`), how many accepted
 * claims it blocks there (`blocked`, written from the next load once it is
 * settled), and the open held claims that wait on it (`held`), each
 * approved on its own. A name only the model linked is for information.
 */
export type NameCard = {
  kind: "name";
  key: string;
  lane: Lane;
  review: QueueReview;
  reviews: QueueReview[];
  reviewCount: number;
  blocked: number;
  held: ClaimCard[];
};

/** A disputed value's card; one a table row keeps against documents is for information. */
export type ValueCard = {
  kind: "value";
  key: string;
  lane: Lane;
  review: QueueReview;
};

export type QueueCard = ClaimCard | NameCard | ValueCard;

/**
 * A claim at its place, as spot checks keep it apart from others: its
 * document, section, byte range and link. A held claim and the fact its
 * approval writes are one claim.
 *
 * @example
 * claimPlace({ documentId: "d", section: 0, start: 1, end: 5 }, "partner_of");
 */
export const claimPlace = (
  at: { documentId: string; section: number; start: number; end: number },
  link: string | undefined,
) => [at.documentId, at.section, at.start, at.end, link ?? ""].join("\u001f");

/** A label's claim (claimPlace). */
export const labelPlace = (
  l: Pick<
    FoundryReviewLabel,
    "docId" | "claimSection" | "claimStart" | "claimEnd" | "link"
  >,
) =>
  claimPlace(
    {
      documentId: l.docId,
      section: l.claimSection ?? 0,
      start: l.claimStart ?? 0,
      end: l.claimEnd ?? 0,
    },
    l.link,
  );

/**
 * The reasons whose review of a name blocks an accepted claim with its link:
 * the load still asks about the name (the backend's AsksAboutName), so a
 * resolve of it applies at once to every document asking.
 */
export const BLOCKING: ReadonlySet<string> = new Set([
  "unresolved",
  "ambiguous",
  "collision",
  "identifier_conflict",
]);

/** How often the spot checks of one band were right, and how often re-checks agreed. */
export type Precision = {
  bands: { band: Band; right: number; total: number }[];
  rechecks: { agreed: number; total: number };
};
