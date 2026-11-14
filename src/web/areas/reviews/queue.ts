import { sha256 } from "@noble/hashes/sha2.js";
import type {
  FoundryReviewDecision,
  FoundryReviewLabel,
  ReviewDecision,
  ReviewDrawn,
  ReviewVerdict,
} from "../../../shared/wfe";
import type { EntityReview } from "../explore/model";
import { normalizeName } from "../explore/naming";
import {
  holdingEnds,
  markDiffering,
  matchesReview,
  type Settle,
  type Unsettled,
} from "./model";

// The review queue: one list of an ontology's open reviews in its current
// load, in the order of the facts each decision writes, with a slice of its
// cards drawn at random from every score band to measure precision. Pure:
// the page reads the load (../explore/model.ts loadReviewsRequest) and
// renders what this returns.
//
// What a decision writes:
// - a held claim whose ends are in the graph writes one fact, at once;
// - a name (a resolve decision of a document's name: the load matched it to
//   no row, several, or one on less than a name) writes, from the next
//   load, every accepted claim it blocks in that document (each a review of
//   the name with the claim's link), and lets each held claim that waits on
//   it be approved: those need a decision each too, so a name's card ranks
//   by facts per decision, (blocked + held) / (1 + held), and its held
//   claims follow it on its card. A name decision is per document: the same
//   name in another document is another decision (until names carry across
//   documents);
// - a value of a property documents state differently writes one value.
// Cards go by facts per decision, then facts, then held claims before
// names before values, then in an order drawn from the load's id.
//
// The random slice: about one card in ten (one after every nine ranked) is
// a claim drawn at random from a score band, the bands taken in turn. A
// band's claims are every claim the load holds at a place with a score in
// it, held or written: its held claims (decided, undecided, or such that no
// approval writes them) and its documents' quotes of facts (accepted ones at
// the accept bar and above; approved ones at the score their review had, one
// with no score at 0, which is the no-score band). A decision moves a claim
// from held to written and leaves it in its band, so whatever people decide
// elsewhere, and in whatever order, a draw is from the whole band; only a
// claim someone has already labelled at random, or this person has said
// anything of, is left out. The held claims are those of the reviews read
// (the first QUEUE_READ.reviews of the load). A person's word on a drawn
// claim measures precision there: an undecided held claim is approved or
// rejected (a decision, kept as a label too); a decided one, or a fact the
// graph holds, takes Right or Wrong (a label). Draws come from a hash of the
// load's id and what is drawn, so a page shows the same draws until they're
// labelled, and a reload changes none.

/** A review as the queue reads it: at its place in its document. */
export type QueueReview = EntityReview;

/** A score band of the random slice, by the verifier's score. */
export type Band = "0.9" | "0.8" | "0.7" | "0.6" | "low" | "none";

/** The bands in the order the random slice takes them, accepted facts first. */
export const BANDS: readonly Band[] = [
  "0.9",
  "0.8",
  "0.7",
  "0.6",
  "low",
  "none",
];

/**
 * The scores of a band's quotes of facts: from (inclusive) and below. A
 * claim the verifier gave no score, approved, is written at 0 (the engine's
 * Confidence::ZERO), so a quote at 0 is in the no-score band, as its review
 * was; no other quote is scored 0, as the verifier refutes anything under 0.6.
 */
export const BAND_RANGE: Record<Band, { from?: number; below?: number }> = {
  "0.9": { from: 0.9 },
  "0.8": { from: 0.8, below: 0.9 },
  "0.7": { from: 0.7, below: 0.8 },
  "0.6": { from: 0.6, below: 0.7 },
  low: { from: 0.000001, below: 0.6 },
  none: { from: 0, below: 0.000001 },
};

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
 * A verifier's score's band: none for a claim with no score (unverified).
 *
 * @example
 * bandOf(0.93); // "0.9"
 * bandOf(0.85); // "0.8"
 * bandOf(undefined); // "none"
 * bandOf(0); // "none": approved with no score
 */
export function bandOf(confidence: number | undefined): Band {
  // 0 is a claim with no score that a person approved (BAND_RANGE).
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

/** One card in ten is drawn at random: one after every RANKED_PER_RANDOM ranked. */
export const RANKED_PER_RANDOM = 9;

type Release = Extract<Settle, { kind: "release" }>;

/** A held claim's card: its review, and how the queue drew it. */
export type ClaimCard = {
  kind: "claim";
  key: string;
  review: QueueReview;
  drawn: ReviewDrawn;
  band: Band;
};

/**
 * A held claim someone has decided, drawn at random: it takes Right or
 * Wrong, a label of the claim, as its decision is already made.
 */
export type DecidedCard = {
  kind: "decided";
  key: string;
  review: QueueReview;
  drawn: "random";
  band: Band;
  // No approval writes it: read two ways at its place, or a property of a
  // fact that differs on a link that holds many values. Still a claim of
  // its band, and labelled as any decided one is.
  blocked?: true;
};

/**
 * A name's card: the review it settles by (`review`; every review of the
 * name in its document shares the decision), how many accepted claims it
 * blocks there (`blocked`, written from the next load once it is settled),
 * and the held claims that wait on it (`held`), each approved on its own.
 */
export type NameCard = {
  kind: "name";
  key: string;
  review: QueueReview;
  reviews: QueueReview[];
  blocked: number;
  held: ClaimCard[];
  drawn: "ranked";
};

/** A disputed value's card. */
export type ValueCard = {
  kind: "value";
  key: string;
  review: QueueReview;
  drawn: "ranked";
};

/**
 * A fact the graph holds, drawn at random from a band: the `draw`th of the
 * queue's draws of the band's quotes, which the page reads (quoteOffsets).
 */
export type QuoteCard = {
  kind: "quote";
  key: string;
  band: Band;
  draw: number;
  drawn: "random";
};

export type QueueCard =
  | ClaimCard
  | DecidedCard
  | NameCard
  | ValueCard
  | QuoteCard;

/** Why a review of the load isn't in the queue, with how many. */
export type Left = {
  // A decision is saved for it.
  decided: number;
  // A review no decision settles here: a relationship the ontology lacks
  // (Add these links, on its document's page), or one held for a check
  // nothing here settles.
  unsettled: number;
  // A held claim read two ways at its place, or one of a property that
  // differs from its fact's: no approval writes it.
  blocked: number;
  // A review written before decisions: a new load writes it again.
  stale: number;
};

export type Queue = {
  cards: QueueCard[];
  left: Left;
  // Each band's claims the random slice draws from: its held claims, and
  // its quotes of facts.
  pool: Record<Band, { held: number; quotes: number }>;
};

/**
 * What a hash of `parts` makes of them: a number in [0, 1), the same for
 * the same parts wherever it is computed.
 *
 * @example
 * hashUnit("load-1", "x") === hashUnit("load-1", "x"); // true
 */
export function hashUnit(...parts: (string | number)[]): number {
  const bytes = sha256(new TextEncoder().encode(parts.join("\u001f")));
  const word =
    ((bytes[0]! << 24) | (bytes[1]! << 16) | (bytes[2]! << 8) | bytes[3]!) >>>
    0;
  return word / 2 ** 32;
}

/**
 * A claim at its place, as the random slice keeps it apart from others: its
 * document, section, byte range and link. A held claim and the fact its
 * approval writes are one claim; two claims of one link in one sentence are
 * one too.
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

/** A review's own key: its review_key, else its place and reason. */
const keyOf = (r: QueueReview) =>
  r.settle && r.settle.kind !== "stale"
    ? r.settle.reviewId
    : [r.documentId, r.section, r.start, r.end, r.reason].join("\u001f");

/** A name's decision identity: its type, document and normalized name. */
const nameKey = (type: string, docId: string, normName: string) =>
  JSON.stringify([type, docId, normName]);

/** The reasons whose review of a name blocks an accepted claim with its link. */
const BLOCKING = new Set([
  "unresolved",
  "ambiguous",
  "collision",
  "identifier_conflict",
]);

/**
 * The saved decisions, indexed: a claim's by its place, a name's and a
 * value's by their identity.
 */
function decidedIndex(decisions: readonly ReviewDecision[]) {
  const claims = new Map<string, ReviewDecision[]>(),
    others = new Map<string, ReviewDecision[]>();
  const add = (
    map: Map<string, ReviewDecision[]>,
    key: string,
    d: ReviewDecision,
  ) => map.set(key, [...(map.get(key) ?? []), d]);
  for (const d of decisions)
    if (d.kind === "release" || d.kind === "reject")
      add(
        claims,
        [d.docId, d.claimSection, d.claimStart, d.claimEnd].join("\u001f"),
        d,
      );
    // An approval taken back (contract 4.16.0) decides nothing: its claim
    // is held again, and is one to decide.
    else if (d.kind === "withdraw") continue;
    else if (d.kind === "resolve")
      add(others, nameKey(d.type, d.docId, d.normName), d);
    else add(others, JSON.stringify([d.type, d.entityKey, d.prop]), d);
  return (s: Settle): boolean => {
    switch (s.kind) {
      case "release":
        // A rejection of the claim at its place, under its key or of what it
        // reads (a rebuild moves keys), or an approval of what it reads now:
        // its card is decided. An approval of what it said before is not:
        // the load refuses it, and the card asks to be read again.
        return (
          claims.get([s.docId, s.section, s.start, s.end].join("\u001f")) ?? []
        ).some((d) =>
          d.kind === "reject"
            ? d.claimKey === s.claimKey || d.statementHash === s.hash
            : d.kind === "release" && d.statementHash === s.hash,
        );
      case "resolve":
        return (others.get(nameKey(s.type, s.docId, s.normName)) ?? []).some(
          (d) => matchesReview(d, s),
        );
      case "prop":
        return (
          others.get(JSON.stringify([s.type, s.entityKey, s.prop])) ?? []
        ).some((d) => matchesReview(d, s));
      case "stale":
        return false;
    }
  };
}

/**
 * The name a held claim waits on, by its key (nameKey), if any: the first of
 * its ends (subject, then object) whose name has a review in its document
 * that holds it. One the load linked on less than a name holds it where the
 * engine holds the claim for it (holdingEnds: part of a name and an own-name
 * identifier always, a legal form on a link that holds one value); one the
 * load matched to no row, or several, holds a claim an end of which isn't
 * in the graph (`named` false). A name the model linked (adjudicated) holds
 * nothing: it is in the graph.
 */
function waitsOn(
  s: Release,
  names: ReadonlyMap<string, { weak: boolean; blocking: boolean }>,
  single: boolean,
): string | undefined {
  const holding = new Set(
    holdingEnds(s.unsettled, single).map((u: Unsettled) =>
      normalizeName(u.name),
    ),
  );
  for (const end of [s.statement.subject, s.statement.object]) {
    const norm = normalizeName(end.name),
      key = nameKey(end.type, s.docId, norm),
      name = names.get(key);
    if (!name) continue;
    if (name.weak ? holding.has(norm) : name.blocking && !s.named) return key;
  }
  return undefined;
}

/**
 * The queue of a load's `reviews` (all of them, read load-wide), given the
 * ontology's saved `decisions`, which links hold one value (`single`), how
 * many quotes of facts the load holds in each band, those already labelled
 * at random taken off (`quotes`), the claims to leave out of the random
 * slice (`labelled`, claimPlace: those labelled at random, and those this
 * person has said anything of), the load's id (`seed`), the keys of the
 * cards decided on this page (`keep`) and of those labelled on it
 * (`labelledHere`), which keep their places and their kinds until the page
 * is read again so their cards can say what became of them.
 */
export function buildQueue({
  reviews,
  decisions,
  single,
  quotes = {},
  labelled = new Set(),
  seed,
  keep = new Set(),
  labelledHere = new Set(),
}: {
  reviews: readonly QueueReview[];
  decisions: readonly ReviewDecision[];
  single: (link: string) => boolean;
  quotes?: Partial<Record<Band, number>>;
  labelled?: ReadonlySet<string>;
  seed: string;
  keep?: ReadonlySet<string>;
  labelledHere?: ReadonlySet<string>;
}): Queue {
  const decided = decidedIndex(decisions);
  const orders = new Map<string, number>();
  // Computed once a key: a sort calls it on every comparison.
  const order = (key: string) => {
    let o = orders.get(key);
    if (o === undefined) orders.set(key, (o = hashUnit(seed, "order", key)));
    return o;
  };
  const left: Left = { decided: 0, unsettled: 0, blocked: 0, stale: 0 };
  const claims: QueueReview[] = [],
    done: QueueReview[] = [],
    blocked: QueueReview[] = [],
    values: QueueReview[] = [],
    names = new Map<
      string,
      { reviews: QueueReview[]; weak: boolean; blocking: boolean }
    >();
  for (const r of markDiffering([...reviews])) {
    const s = r.settle;
    if (!s) {
      left.unsettled += 1;
      continue;
    }
    if (s.kind === "stale") {
      left.stale += 1;
      continue;
    }
    // Nothing an approval writes: read two ways, or a property of a fact
    // that differs, on a link that holds many values.
    // Still one of its band's claims, for the random slice.
    if (
      s.kind === "release" &&
      (s.differs || (s.conflict && !single(s.link)))
    ) {
      left.blocked += 1;
      blocked.push(r);
      continue;
    }
    // A card decided on the page keeps its place: a claim's or a value's by
    // its review, a name's by its decision (every review of the name).
    const kept =
      keep.has(s.reviewId) ||
      (s.kind === "resolve" && keep.has(nameKey(s.type, s.docId, s.normName)));
    if (decided(s) && !kept) {
      left.decided += 1;
      // Still one of its band's claims, for the random slice.
      if (s.kind === "release") done.push(r);
      continue;
    }
    if (s.kind === "release") claims.push(r);
    else if (s.kind === "prop") values.push(r);
    else {
      const key = nameKey(s.type, s.docId, s.normName),
        name = names.get(key) ?? { reviews: [], weak: false, blocking: false };
      name.reviews.push(r);
      name.weak ||= r.reason !== "adjudicated" && !BLOCKING.has(r.reason);
      name.blocking ||= BLOCKING.has(r.reason);
      names.set(key, name);
    }
  }
  // Each held claim on its own, or on the card of the name it waits on.
  const held = new Map<string, ClaimCard[]>();
  const own: ClaimCard[] = [];
  for (const r of claims) {
    const s = r.settle as Release;
    const card: ClaimCard = {
      kind: "claim",
      key: s.reviewId,
      review: r,
      drawn: "ranked",
      band: bandOf(r.confidence),
    };
    const on = waitsOn(s, names, single(s.link));
    if (on) held.set(on, [...(held.get(on) ?? []), card]);
    else own.push(card);
  }
  const nameCards = [...names.entries()].map(([key, n]): NameCard => {
    const sorted = n.reviews.toSorted(
      (a, b) => a.section - b.section || a.start - b.start,
    );
    return {
      kind: "name",
      key,
      // A review with a candidate to choose, else the first.
      review: sorted.find((r) => r.candidates.length > 0) ?? sorted[0]!,
      reviews: sorted,
      blocked: sorted.filter((r) => BLOCKING.has(r.reason) && !!r.suggestedLink)
        .length,
      held: held.get(key) ?? [],
      drawn: "ranked",
    };
  });
  const valueCards = values.map(
    (r): ValueCard => ({
      kind: "value",
      key: keyOf(r),
      review: r,
      drawn: "ranked",
    }),
  );
  const ranked = (): (ClaimCard | NameCard | ValueCard)[] =>
    [...own, ...nameCards, ...valueCards].sort(
      (a, b) =>
        perDecision(b) - perDecision(a) ||
        facts(b) - facts(a) ||
        KIND_ORDER[a.kind] - KIND_ORDER[b.kind] ||
        order(a.key) - order(b.key),
    );
  // The random slice's pools: each band's held claims, decided or not, but
  // those labelled at random, in an order drawn from the load's id; and how
  // many quotes of facts the band holds.
  // A card decided or labelled on this page (a random draw) keeps its place.
  const open = (r: QueueReview) =>
    !labelled.has(claimPlace(r, r.suggestedLink)) ||
    keep.has(keyOf(r)) ||
    labelledHere.has(keyOf(r));
  type Drawable = ClaimCard | DecidedCard;
  const pools = new Map<Band, Drawable[]>(BANDS.map((b) => [b, []]));
  for (const c of [...own, ...[...held.values()].flat()])
    if (open(c.review)) pools.get(c.band)!.push(c);
  for (const [list, isBlocked] of [
    [done, false],
    [blocked, true],
  ] as const)
    for (const r of list)
      if (open(r))
        pools.get(bandOf(r.confidence))!.push({
          kind: "decided",
          key: (r.settle as Release).reviewId,
          review: r,
          drawn: "random",
          band: bandOf(r.confidence),
          ...(isBlocked ? { blocked: true as const } : {}),
        });
  for (const p of pools.values()) p.sort((a, b) => order(a.key) - order(b.key));
  const quotesLeft = new Map<Band, number>(
    BANDS.map((b) => [b, Math.max(0, quotes[b] ?? 0)]),
  );
  const pool = Object.fromEntries(
    BANDS.map((b) => [
      b,
      { held: pools.get(b)!.length, quotes: quotesLeft.get(b)! },
    ]),
  ) as Queue["pool"];
  const randoms = Math.ceil(ranked().length / RANKED_PER_RANDOM);
  const drawn: (Drawable | QuoteCard)[] = [];
  const draws = new Map<Band, number>();
  // The bands in turn, from the top, passing over one with nothing left.
  let next = 0;
  while (drawn.length < randoms) {
    const at = BANDS.findIndex(
      (_, i) =>
        pools.get(BANDS[(next + i) % BANDS.length]!)!.length > 0 ||
        quotesLeft.get(BANDS[(next + i) % BANDS.length]!)! > 0,
    );
    if (at < 0) break;
    const band = BANDS[(next + at) % BANDS.length]!;
    next = (next + at + 1) % BANDS.length;
    const inBand = pools.get(band)!,
      quotesIn = quotesLeft.get(band)!;
    // A held claim or a fact, in proportion to how many of each there are.
    const fact =
      quotesIn > 0 &&
      hashUnit(seed, "pick", drawn.length) * (inBand.length + quotesIn) >=
        inBand.length;
    if (fact) {
      const n = draws.get(band) ?? 0;
      drawn.push({
        kind: "quote",
        key: `quote-${band}-${n}`,
        band,
        draw: n,
        drawn: "random",
      });
      draws.set(band, n + 1);
      quotesLeft.set(band, quotesIn - 1);
      continue;
    }
    const card = inBand.shift()!;
    drawn.push(card.kind === "claim" ? { ...card, drawn: "random" } : card);
    // Out of its rank, or off its name's card.
    const i = own.indexOf(card as ClaimCard);
    if (i >= 0) own.splice(i, 1);
    for (const n of nameCards) n.held = n.held.filter((c) => c !== card);
  }
  // One random card after every RANKED_PER_RANDOM ranked, the rest at the end.
  const cards: QueueCard[] = [];
  ranked().forEach((c, i) => {
    cards.push(c);
    if ((i + 1) % RANKED_PER_RANDOM === 0 && drawn.length)
      cards.push(drawn.shift()!);
  });
  cards.push(...drawn);
  return { cards, left, pool };
}

const KIND_ORDER = { claim: 0, name: 1, value: 2 } as const;

/**
 * The facts a card's decisions write: one for a held claim; a name's
 * blocked claims and the held claims on its card; none for a value, which
 * writes a value.
 *
 * @example
 * facts({ kind: "name", blocked: 3, held: [claim] }); // 4
 */
export function facts(card: ClaimCard | NameCard | ValueCard): number {
  switch (card.kind) {
    case "claim":
      return 1;
    case "name":
      return card.blocked + card.held.length;
    case "value":
      return 0;
  }
}

/**
 * What a card ranks by: the facts written per decision made. A held claim
 * writes one with one; a name writes its blocked claims with one, and each
 * held claim on its card with one more; a value writes a value, ranked as a
 * claim is.
 *
 * @example
 * perDecision({ kind: "name", blocked: 3, held: [claim] }); // 2
 */
export function perDecision(card: ClaimCard | NameCard | ValueCard): number {
  switch (card.kind) {
    case "claim":
    case "value":
      return 1;
    case "name":
      return (card.blocked + card.held.length) / (1 + card.held.length);
  }
}

/** How many offsets a quote card reads: the first unlabelled one is its fact. */
export const QUOTE_CANDIDATES = 8;

/**
 * The offsets among a band's `count` quotes the `draw`th quote card of the
 * band reads, in order, each once: the first that no one has labelled at
 * random, and no earlier card shows, is its fact. Drawn from a hash of the
 * load's id, so they are the same until the band's count changes.
 *
 * @example
 * quoteOffsets("load-1", "0.9", 0, 40).length; // 8
 * quoteOffsets("load-1", "0.9", 0, 3).toSorted(); // [0, 1, 2]
 */
export function quoteOffsets(
  seed: string,
  band: Band,
  draw: number,
  count: number,
): number[] {
  if (count <= 0) return [];
  const want = Math.min(QUOTE_CANDIDATES, count);
  const out: number[] = [];
  for (let t = 0; out.length < want; t++) {
    let o = Math.floor(hashUnit(seed, "quote", band, draw, t) * count);
    // Each offset once: the next free one after a repeat.
    while (out.includes(o)) o = (o + 1) % count;
    out.push(o);
  }
  return out;
}

/** Of each re-check's candidates, this share is drawn for a second person. */
export const RECHECK_SHARE = 0.05;

/** A decision or a label a second person may re-check, and what it said. */
export type Recheck =
  | {
      kind: "decision";
      key: string;
      decision: Extract<FoundryReviewDecision, { kind: "release" | "reject" }>;
      against: ReviewVerdict;
    }
  | {
      kind: "label";
      key: string;
      label: FoundryReviewLabel;
      against: ReviewVerdict;
    };

/** A decision's claim, by its place (a decision keeps no link). */
const decisionAt = (
  d: Extract<FoundryReviewDecision, { kind: "release" | "reject" }>,
) => [d.docId, d.claimSection, d.claimStart, d.claimEnd].join("\u001f");

/**
 * What `user` may re-check: the share RECHECK_SHARE, drawn by id, of the
 * decisions on held claims made from the queue, and of the labels drawn at
 * random that came with no decision (a fact's, or a decided claim's), made
 * by someone else, of a claim `user` has given no word on. A re-check
 * changes nothing: it measures how often people agree.
 *
 * @example
 * rechecks([], [], "user-1"); // []
 */
export function rechecks(
  decisions: readonly FoundryReviewDecision[],
  labels: readonly FoundryReviewLabel[],
  user: string,
): Recheck[] {
  const mine = labels.filter((l) => l.decidedByUserId === user);
  const said = new Set(mine.map(labelPlace)),
    saidAt = new Set(
      mine.map((l) =>
        [l.docId, l.claimSection ?? 0, l.claimStart ?? 0, l.claimEnd ?? 0].join(
          "\u001f",
        ),
      ),
    );
  const sampled = (id: string) => hashUnit("recheck", id) < RECHECK_SHARE;
  const ofDecisions = decisions.flatMap((d): Recheck[] =>
    (d.kind === "release" || d.kind === "reject") &&
    !!d.drawn &&
    d.decidedByUserId !== user &&
    sampled(d.id) &&
    !saidAt.has(decisionAt(d))
      ? [
          {
            kind: "decision",
            key: `decision-${d.id}`,
            decision: d,
            against: d.kind === "release" ? "right" : "wrong",
          },
        ]
      : [],
  );
  const ofLabels = labels.flatMap((l): Recheck[] =>
    l.drawn === "random" &&
    !l.decisionId &&
    l.decidedByUserId !== user &&
    sampled(l.id) &&
    !said.has(labelPlace(l))
      ? [{ kind: "label", key: `label-${l.id}`, label: l, against: l.verdict }]
      : [],
  );
  return [...ofDecisions, ...ofLabels];
}

/** How often the random slice's claims of one band were right, and how often re-checks agreed. */
export type Precision = {
  bands: { band: Band; right: number; total: number }[];
  rechecks: { agreed: number; total: number };
};

/**
 * The random slice's word per band, from the labels: one per claim (the
 * first said, where two people labelled one at once), each a claim drawn at
 * random with the score it was read at; and the re-checks' agreement.
 *
 * @example
 * precision([]).bands.length; // 0
 */
export function precision(labels: readonly FoundryReviewLabel[]): Precision {
  const first = new Map<string, FoundryReviewLabel>();
  for (const l of labels.toSorted(
    (a, b) =>
      (a.decidedAt ?? "").localeCompare(b.decidedAt ?? "") ||
      a.id.localeCompare(b.id),
  ))
    if (l.drawn === "random" && !first.has(labelPlace(l)))
      first.set(labelPlace(l), l);
  const random = [...first.values()];
  const bands = BANDS.flatMap((band) => {
    const of = random.filter((l) => bandOf(l.confidence) === band);
    return of.length
      ? [
          {
            band,
            right: of.filter((l) => l.verdict === "right").length,
            total: of.length,
          },
        ]
      : [];
  });
  const checks = labels.filter((l) => l.drawn === "recheck" && !!l.against);
  return {
    bands,
    rechecks: {
      agreed: checks.filter((l) => l.verdict === l.against).length,
      total: checks.length,
    },
  };
}
