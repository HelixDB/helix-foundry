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
// names before values, then in an order drawn from the load's id: so the
// claims the queue leaves for later are a random part of their rank, and a
// random draw from what is left is still a fair sample.
//
// The random slice: about one card in ten (one after every nine ranked)
// is a held claim or an accepted fact drawn at random from a score band,
// the bands taken in turn, the accepted facts at the accept bar and above
// included. A person's word on it measures precision there: an approval or
// a rejection of a held claim (a decision, kept as a label too), Right or
// Wrong on an accepted fact (a label). Draws come from a hash of the load's
// id and what is drawn, so a page shows the same draws until they're
// decided, and a reload changes none.

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
 */
export function bandOf(confidence: number | undefined): Band {
  if (confidence === undefined || !Number.isFinite(confidence)) return "none";
  if (confidence >= 0.9) return "0.9";
  if (confidence >= 0.8) return "0.8";
  if (confidence >= 0.7) return "0.7";
  if (confidence >= 0.6) return "0.6";
  return "low";
}

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
 * An accepted fact drawn at random: the `draw`th of the queue's draws of
 * the load's accepted quotes, which the page reads (acceptedOffsets).
 */
export type AcceptedCard = {
  kind: "accepted";
  key: string;
  draw: number;
  drawn: "random";
  band: "0.9";
};

export type QueueCard = ClaimCard | NameCard | ValueCard | AcceptedCard;

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
  // The held claims each band holds now, which its random draws come from.
  pool: Record<Band, number>;
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
    else if (d.kind === "resolve")
      add(others, nameKey(d.type, d.docId, d.normName), d);
    else add(others, JSON.stringify([d.type, d.entityKey, d.prop]), d);
  return (s: Settle): boolean => {
    switch (s.kind) {
      case "release":
        // An approval or a rejection of the claim at its place, under its key
        // or of what it reads (a rebuild moves keys): its card is decided.
        return (
          claims.get([s.docId, s.section, s.start, s.end].join("\u001f")) ?? []
        ).some(
          (d) =>
            (d.kind === "release" || d.kind === "reject") &&
            (d.claimKey === s.claimKey || d.statementHash === s.hash),
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
 * in the graph (`named` false).
 */
function waitsOn(
  s: Release,
  names: ReadonlyMap<string, { weak: boolean }>,
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
    if (name.weak ? holding.has(norm) : !s.named) return key;
  }
  return undefined;
}

/**
 * The queue of a load's `reviews` (all of them, read load-wide), given the
 * ontology's saved `decisions`, which links hold one value (`single`), how
 * many accepted quotes of documents the load holds (`accepted`), the load's
 * id (`seed`), and the review keys decided on this page (`keep`), which keep
 * their places until the page is read again so their cards can say what
 * became of them.
 */
export function buildQueue({
  reviews,
  decisions,
  single,
  accepted,
  seed,
  keep = new Set(),
}: {
  reviews: readonly QueueReview[];
  decisions: readonly ReviewDecision[];
  single: (link: string) => boolean;
  accepted: number;
  seed: string;
  keep?: ReadonlySet<string>;
}): Queue {
  const decided = decidedIndex(decisions);
  const left: Left = { decided: 0, unsettled: 0, blocked: 0, stale: 0 };
  const claims: QueueReview[] = [],
    values: QueueReview[] = [],
    names = new Map<string, { reviews: QueueReview[]; weak: boolean }>();
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
    if (decided(s) && !keep.has(s.reviewId)) {
      left.decided += 1;
      continue;
    }
    if (s.kind === "release") {
      // Nothing an approval writes: read two ways, or a property of a fact
      // that differs, on a link that holds many values.
      if (s.differs || (s.conflict && !single(s.link))) left.blocked += 1;
      else claims.push(r);
    } else if (s.kind === "prop") values.push(r);
    else {
      const key = nameKey(s.type, s.docId, s.normName),
        name = names.get(key) ?? { reviews: [], weak: false };
      name.reviews.push(r);
      name.weak ||= r.reason !== "adjudicated" && !BLOCKING.has(r.reason);
      names.set(key, name);
    }
  }
  const order = (key: string) => hashUnit(seed, "order", key);
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
  // The random slice, drawn from the held claims of each band and the
  // load's accepted facts, before the ranking: a claim drawn is its card
  // there and nowhere else.
  const byBand = new Map<Band, ClaimCard[]>(BANDS.map((b) => [b, []]));
  for (const c of [...own, ...[...held.values()].flat()])
    byBand.get(c.band)!.push(c);
  for (const pool of byBand.values())
    pool.sort((a, b) => order(a.key) - order(b.key));
  const pool = Object.fromEntries(
    BANDS.map((b) => [b, byBand.get(b)!.length]),
  ) as Record<Band, number>;
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
  const randoms = Math.ceil(ranked().length / RANKED_PER_RANDOM);
  const drawn: (ClaimCard | AcceptedCard)[] = [];
  let acceptedLeft = accepted,
    draws = 0;
  for (let slot = 0; drawn.length < randoms; slot++) {
    const open = BANDS.filter(
      (b) => byBand.get(b)!.length > 0 || (b === "0.9" && acceptedLeft > 0),
    );
    if (!open.length) break;
    const band = open[slot % open.length]!,
      inBand = byBand.get(band)!;
    // In the top band, a held claim or an accepted fact, in proportion to
    // how many of each there are.
    const fact =
      band === "0.9" &&
      acceptedLeft > 0 &&
      hashUnit(seed, "pick", slot) * (inBand.length + acceptedLeft) >=
        inBand.length;
    if (fact) {
      drawn.push({
        kind: "accepted",
        key: `accepted-${draws}`,
        draw: draws,
        drawn: "random",
        band: "0.9",
      });
      draws += 1;
      acceptedLeft -= 1;
      continue;
    }
    const card = inBand.shift()!;
    drawn.push({ ...card, drawn: "random" });
    // Out of its rank, or off its name's card.
    const at = own.indexOf(card);
    if (at >= 0) own.splice(at, 1);
    for (const n of nameCards) n.held = n.held.filter((c) => c !== card);
  }
  // One random card after every RANKED_PER_RANDOM ranked, the rest at the end.
  const cards: QueueCard[] = [];
  const rest = ranked();
  rest.forEach((c, i) => {
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

/**
 * The offsets among `accepted` quotes the `draw`th accepted card reads, in
 * order: the first that no one has labelled at random, and no earlier card
 * shows, is its fact.
 *
 * @example
 * acceptedOffsets("load-1", 0, 40).length; // 5
 */
export function acceptedOffsets(
  seed: string,
  draw: number,
  accepted: number,
): number[] {
  if (accepted <= 0) return [];
  return Array.from({ length: Math.min(5, accepted) }, (_, t) =>
    Math.floor(hashUnit(seed, "accepted", draw, t) * accepted),
  );
}

/** A claim's place and key, as a label names it. */
export const labelPlace = (
  l: Pick<
    FoundryReviewLabel,
    | "docId"
    | "claimSection"
    | "claimStart"
    | "claimEnd"
    | "claimKey"
    | "factKey"
  >,
) =>
  JSON.stringify([
    l.docId,
    l.claimSection ?? 0,
    l.claimStart ?? 0,
    l.claimEnd ?? 0,
    l.claimKey ?? null,
    l.factKey ?? null,
  ]);

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

/**
 * What `user` may re-check: the share RECHECK_SHARE, drawn by id, of the
 * decisions on held claims made from the queue, and of the labels of
 * accepted facts drawn at random, made by someone else, of a claim `user`
 * has given no word on. A re-check changes nothing: it measures how often
 * people agree.
 *
 * @example
 * rechecks([], [], "user-1"); // []
 */
export function rechecks(
  decisions: readonly FoundryReviewDecision[],
  labels: readonly FoundryReviewLabel[],
  user: string,
): Recheck[] {
  const mine = new Set(
    labels.filter((l) => l.decidedByUserId === user).map(labelPlace),
  );
  const sampled = (id: string) => hashUnit("recheck", id) < RECHECK_SHARE;
  const ofDecisions = decisions.flatMap((d): Recheck[] =>
    (d.kind === "release" || d.kind === "reject") &&
    !!d.drawn &&
    d.decidedByUserId !== user &&
    sampled(d.id) &&
    !mine.has(labelPlace(d))
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
    !!l.factKey &&
    l.decidedByUserId !== user &&
    sampled(l.id) &&
    !mine.has(labelPlace(l))
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
 * The random slice's word per band, from the labels: each a claim drawn at
 * random (a held claim's approval or rejection, or an accepted fact's Right
 * or Wrong), with the score it was read at; and the re-checks' agreement.
 *
 * @example
 * precision([]).bands.length; // 0
 */
export function precision(labels: readonly FoundryReviewLabel[]): Precision {
  const random = labels.filter((l) => l.drawn === "random");
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
