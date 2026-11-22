import type {
  FoundryReviewChoice,
  FoundryReviewDecision,
  ReviewDecision,
} from "../../../shared/wfe";
import {
  approvable,
  holdingEnds,
  propValueText,
  type Settle,
  type StatedValue,
} from "./model";
import {
  BLOCKING,
  type Band,
  type ClaimCard,
  type NameCard,
  type Precision,
  type ValueCard,
} from "./queue";

// Founder's suggestion for a card of the review walkthrough: a choice from
// fixed rules over what the card and the saved decisions already hold, at no
// model cost, computed whenever the queue is read and stored nowhere, so it
// follows the decisions as they change. Pure. A suggestion is only ever
// accepted by a person, through the same settle call a card sends.
//
// The rules, first that fires:
// - precedent (names): every resolve decision of the same type and name in
//   another document chose the same, an entity this card lists and the read
//   found, or Not in data. A repeat of the person's own choice may be
//   accepted in a batch;
// - single_candidate (names): the card lists one candidate, found in the
//   load. Not measured, so never batched;
// - confident_claim (held claims): Approve all would take it (approvable),
//   no name holds it, and its score band has random labels, mostly right.
//   Batched only when the band is measured: the Wilson lower bound of its
//   share right at least BATCH_LOWER over at least BATCH_LABELS labels;
// - majority_value (values): no table value, at least MIN_QUOTES quotes, at
//   least two thirds of them stating one value. Never batched.
// Otherwise the card says why there is none (NoAdvice).

/** A card a person decides in the walkthrough. */
export type Decidable = NameCard | ClaimCard | ValueCard;

export type RuleId =
  | "precedent"
  | "single_candidate"
  | "confident_claim"
  | "majority_value";

/**
 * How far a suggestion is to be trusted, from data, never self-reported: a
 * band's random labels, or how often the same choice was made before, by
 * this person alone (`you`) or by others too (`team`).
 */
export type Strength =
  | { kind: "measured"; right: number; total: number; lower: number }
  | { kind: "repeat"; decisions: number; by: "you" | "team" }
  | { kind: "unmeasured" };

/** What a suggestion stands on, which its card says in fixed words. */
export type Basis =
  | { kind: "precedent"; decisions: number; latest: string }
  | { kind: "only_candidate"; name: string }
  | { kind: "verifier"; score?: number; band: Band }
  | { kind: "majority"; agree: number; of: number };

/** Why a card has no suggestion: a closed set its card words. */
export type NoAdvice =
  // Lanes and cards no rule reads.
  | "fyi"
  // Names.
  | "tied_candidates"
  | "no_candidates"
  // Held claims.
  | "one_valued"
  | "differs"
  | "waits_on_name"
  | "read_again"
  | "unscored"
  | "mostly_wrong"
  // Values.
  | "values_split"
  | "few_quotes";

export type Advice =
  | {
      kind: "suggest";
      rule: RuleId;
      choice: FoundryReviewChoice;
      basis: Basis[];
      strength: Strength;
      // Cards of one batch key may be accepted together; null when the
      // strength doesn't allow it.
      batchKey: string | null;
    }
  | { kind: "none"; why: NoAdvice };

/**
 * Where a card goes: to decide, or for information. A link the model made
 * (adjudicated) and a value a table row holds against documents are in
 * their load already and block nothing; each can still be changed.
 */
export type Lane = "decide" | "fyi";

/** When what a choice saves takes effect (the backend's rules per kind). */
export type Effect =
  // A held claim's approval: a job writes it to the current load.
  | { kind: "writes_now"; facts: number }
  // A resolve of a name the load still asks about: every document asking
  // (an entity), or this one (Not in data), in the current load.
  | { kind: "applies_now"; documents: number }
  // A chosen value: a job sends it to the current load.
  | { kind: "value_now" }
  // A name the load links already, or a claim a name holds: a later load.
  | { kind: "next_load" }
  // A rejection: nothing is written.
  | { kind: "saved_only" };

type Resolve = Extract<ReviewDecision, { kind: "resolve" }> &
  FoundryReviewDecision;

/**
 * What the rules read, indexed once a queue so a card reads only its own:
 * resolve decisions by type and name (`names`), claim decisions by place
 * (`claims`), the bands' precision, which links hold one value, and the
 * person deciding (`user`).
 */
export type AdviceContext = {
  names: ReadonlyMap<string, readonly Resolve[]>;
  claims: ReadonlyMap<string, readonly FoundryReviewDecision[]>;
  precision: Precision;
  single: (link: string) => boolean;
  user: string;
};

/** A batch of measured claims needs this lower bound of the share right… */
export const BATCH_LOWER = 0.95;
/** …over at least this many random labels. */
export const BATCH_LABELS = 30;
/** A value's majority needs at least this many quotes. */
export const MIN_QUOTES = 3;
/** Founder's seed quotes at most this many characters of a review's quote. */
export const SEED_QUOTE = 200;

const nameAt = (type: string, normName: string) =>
  JSON.stringify([type, normName]);
const claimAt = (docId: string, section: number, start: number, end: number) =>
  JSON.stringify([docId, section, start, end]);

/**
 * The context of `decisions`, indexed.
 *
 * @example
 * adviceContext({ decisions: [], precision: { bands: [], rechecks: { agreed: 0, total: 0 } }, single: () => false, user: "u" }).names.size; // 0
 */
export function adviceContext({
  decisions,
  precision,
  single,
  user,
}: {
  decisions: readonly FoundryReviewDecision[];
  precision: Precision;
  single: (link: string) => boolean;
  user: string;
}): AdviceContext {
  const names = new Map<string, Resolve[]>(),
    claims = new Map<string, FoundryReviewDecision[]>();
  for (const d of decisions)
    if (d.kind === "resolve") {
      const at = nameAt(d.type, d.normName);
      names.set(at, [...(names.get(at) ?? []), d]);
    } else if (d.kind !== "prop") {
      const at = claimAt(d.docId, d.claimSection, d.claimStart, d.claimEnd);
      claims.set(at, [...(claims.get(at) ?? []), d]);
    }
  return { names, claims, precision, single, user };
}

const SETTLE_KIND = {
  name: "resolve",
  claim: "release",
  value: "prop",
} as const;
type SettleOf<C extends Decidable> = Extract<
  Settle,
  { kind: (typeof SETTLE_KIND)[C["kind"]] }
>;

/**
 * A card's settle: buildQueue makes a name's a resolve, a held claim's a
 * release and a value's a prop, and keeps stale reviews off every card.
 */
export function settleOf<C extends Decidable>(card: C): SettleOf<C> {
  const s = card.review.settle;
  if (s?.kind !== SETTLE_KIND[card.kind])
    throw new Error(
      `a ${card.kind} card settles by a ${SETTLE_KIND[card.kind]}`,
    );
  return s as SettleOf<C>;
}

/**
 * The Wilson score interval's lower bound of `right` of `total` (z 1.96: 95%
 * two-sided); 0 for none.
 *
 * @example
 * wilsonLower(48, 50); // ≈ 0.865
 * wilsonLower(0, 0); // 0
 */
export function wilsonLower(right: number, total: number, z = 1.96): number {
  if (total <= 0) return 0;
  const p = right / total,
    z2 = z * z;
  return (
    (p +
      z2 / (2 * total) -
      z * Math.sqrt((p * (1 - p)) / total + z2 / (4 * total * total))) /
    (1 + z2 / total)
  );
}

/**
 * Whether a name card's choice applies at once: the backend reads the
 * reason of the review it settles (the card's `review`), and applies a
 * resolve of a name the load still asks about (BLOCKING) to the current
 * load, any other from the next load.
 *
 * @example
 * asks(nameCard("acme", { reasons: ["unresolved"] })); // true
 */
export const asks = (card: NameCard): boolean =>
  BLOCKING.has(card.review.reason);

/**
 * A card's lane: a name the model linked, and a value a table holds against
 * documents, are for information; everything else is to decide.
 *
 * @example
 * laneOf(adjudicatedName); // "fyi"
 * laneOf(heldClaim); // "decide"
 */
export function laneOf(card: Decidable): Lane {
  switch (card.kind) {
    case "name":
      return card.reviews.every((r) => r.reason === "adjudicated")
        ? "fyi"
        : "decide";
    case "value":
      return settleOf(card).table === undefined ? "decide" : "fyi";
    case "claim":
      return "decide";
  }
}

/**
 * The decision that already settles a name card the load still asks about:
 * a resolve to an entity of the same type and name in another document,
 * which the backend applies at once to every document asking, while it can
 * still apply to this load: its job is yet to run (`pending`) or it linked
 * the name in place (`written`), and its entity is one of this card's
 * candidates that the read found. One the load read as it was created and
 * still asks about, refused in place (a load read before names could be
 * settled so: the next load applies it), or naming an entity the load no
 * longer holds settles nothing now, so its card stays to decide. Not in
 * data is per document, and a name the load links already isn't asked
 * about, so neither is covered.
 *
 * @example
 * coveredBy(umbrellaInDocB, ctxWithUmbrellaLinkedInDocA); // that decision
 */
export function coveredBy(
  card: NameCard,
  ctx: Pick<AdviceContext, "names">,
): Resolve | null {
  if (!asks(card)) return null;
  const s = settleOf(card);
  return (
    (ctx.names.get(nameAt(s.type, s.normName)) ?? []).find(
      (d) =>
        d.docId !== s.docId &&
        (d.applied?.status === "pending" || d.applied?.status === "written") &&
        card.review.candidates.some((c) => c.key === d.entityKey && !!c.entity),
    ) ?? null
  );
}

const none = (why: NoAdvice): Advice => ({ kind: "none", why });

/**
 * Founder's suggestion for a card, by the first rule that fires; none, with
 * why, when none does. A card for information gets none.
 *
 * @example
 * advise(heldClaimOfAMeasuredBand, ctx); // { kind: "suggest", rule: "confident_claim", … }
 */
export function advise(card: Decidable, ctx: AdviceContext): Advice {
  if (laneOf(card) === "fyi") return none("fyi");
  switch (card.kind) {
    case "name":
      return adviseName(card, ctx);
    case "claim":
      return adviseClaim(card, ctx);
    case "value":
      return adviseValue(card);
  }
}

function adviseName(card: NameCard, ctx: AdviceContext): Advice {
  const s = settleOf(card),
    { candidates } = card.review;
  // A resolve's entity must be in the load: one the read didn't find isn't
  // offered.
  const found = (key: string) =>
    candidates.some((c) => c.key === key && !!c.entity);
  const earlier = (ctx.names.get(nameAt(s.type, s.normName)) ?? []).filter(
    (d) => d.docId !== s.docId,
  );
  const chosen = new Set(earlier.map((d) => d.entityKey ?? null));
  const [only] = chosen;
  if (chosen.size === 1 && (only === null || found(only))) {
    const by = earlier.every((d) => d.decidedByUserId === ctx.user)
      ? "you"
      : "team";
    return {
      kind: "suggest",
      rule: "precedent",
      choice:
        only === null
          ? { resolve: { notInData: true } }
          : { resolve: { entityKey: only } },
      basis: [
        {
          kind: "precedent",
          decisions: earlier.length,
          latest: earlier
            .map((d) => d.decidedAt ?? "")
            .reduce((a, b) => (b > a ? b : a), ""),
        },
      ],
      strength: { kind: "repeat", decisions: earlier.length, by },
      batchKey:
        by === "you"
          ? `precedent:${JSON.stringify([s.type, s.normName, only])}`
          : null,
    };
  }
  if (candidates.length > 1) return none("tied_candidates");
  const [one] = candidates;
  if (!one?.entity) return none("no_candidates");
  return {
    kind: "suggest",
    rule: "single_candidate",
    choice: { resolve: { entityKey: one.key } },
    basis: [{ kind: "only_candidate", name: one.entity.name }],
    strength: { kind: "unmeasured" },
    batchKey: null,
  };
}

function adviseClaim(card: ClaimCard, ctx: AdviceContext): Advice {
  const s = settleOf(card);
  if (ctx.single(s.link)) return none("one_valued");
  if (s.differs || s.conflict) return none("differs");
  // A name holds it, or an end isn't in the graph: whether an approval
  // writes it turns on a name a person settles first.
  if (!s.named || holdingEnds(s.unsettled, false).length)
    return none("waits_on_name");
  // A decision of the claim the queue still shows: an approval of what it
  // said before, or one taken back. Its card says which.
  const atPlace = ctx.claims.get(claimAt(s.docId, s.section, s.start, s.end));
  if (!approvable([card.review], atPlace ?? [], ctx.single).length)
    return none("read_again");
  const band = ctx.precision.bands.find((b) => b.band === card.band);
  if (!band) return none("unscored");
  if (band.right * 2 <= band.total) return none("mostly_wrong");
  const lower = wilsonLower(band.right, band.total);
  return {
    kind: "suggest",
    rule: "confident_claim",
    choice: { release: { statementHash: s.hash } },
    basis: [
      {
        kind: "verifier",
        ...(card.review.confidence !== undefined
          ? { score: card.review.confidence }
          : {}),
        band: card.band,
      },
    ],
    strength: { kind: "measured", right: band.right, total: band.total, lower },
    batchKey:
      lower >= BATCH_LOWER && band.total >= BATCH_LABELS
        ? `claim:${JSON.stringify([s.link, card.band])}`
        : null,
  };
}

function adviseValue(card: ValueCard): Advice {
  const { values } = settleOf(card);
  if (values.length < MIN_QUOTES) return none("few_quotes");
  const counts = new Map<string, { value: StatedValue; n: number }>();
  for (const { value } of values) {
    const at = JSON.stringify(value);
    counts.set(at, { value, n: (counts.get(at)?.n ?? 0) + 1 });
  }
  const top = [...counts.values()].reduce((a, b) => (b.n > a.n ? b : a));
  // At least two thirds: never a tie.
  if (top.n * 3 < values.length * 2) return none("values_split");
  return {
    kind: "suggest",
    rule: "majority_value",
    choice: { prop: top.value },
    basis: [{ kind: "majority", agree: top.n, of: values.length }],
    strength: { kind: "unmeasured" },
    batchKey: null,
  };
}

/**
 * When a choice on a card takes effect: a held claim's approval is written
 * at once unless a name holds it (holdingEnds, by whether its link holds one
 * value: `single`); a rejection only saves; a value is sent at once; a
 * resolve of a name the load still asks about applies at once, an entity to
 * this document and its `siblings` (the other documents asking about the
 * name), Not in data to this one; any other resolve from the next load.
 *
 * @example
 * effectOf(askingName, { resolve: { entityKey: "scus_1" } }, 2, () => false); // { kind: "applies_now", documents: 3 }
 */
export function effectOf(
  card: Decidable,
  choice: FoundryReviewChoice,
  siblings: number,
  single: (link: string) => boolean,
): Effect {
  if (choice.reject) return { kind: "saved_only" };
  if (choice.prop) return { kind: "value_now" };
  if (choice.release) {
    if (card.kind !== "claim") return { kind: "writes_now", facts: 1 };
    const s = settleOf(card);
    return holdingEnds(s.unsettled, single(s.link)).length
      ? { kind: "next_load" }
      : { kind: "writes_now", facts: 1 };
  }
  if (card.kind !== "name" || !asks(card)) return { kind: "next_load" };
  return {
    kind: "applies_now",
    documents: choice.resolve?.entityKey ? 1 + siblings : 1,
  };
}

/**
 * What Ask Founder puts in the assistant's composer for a card: the review
 * by its key and document, the question, at most SEED_QUOTE characters of
 * its quote, and a request to recommend one choice without settling it.
 * Nothing is sent until the person sends it.
 *
 * @example
 * founderSeed(card, "Contract-17.md"); // "Help me decide review r-1 in “Contract-17.md”. …"
 */
export function founderSeed(card: Decidable, documentName: string): string {
  const { quote } = card.review;
  const clipped =
    quote.length > SEED_QUOTE ? `${quote.slice(0, SEED_QUOTE - 1)}…` : quote;
  const question = (() => {
    switch (card.kind) {
      case "name": {
        const s = settleOf(card);
        const names = card.review.candidates.map(
          (c) => `${c.entity?.name ?? c.key} (${c.key})`,
        );
        return `Which ${s.type} is “${s.normName}”? ${
          names.length ? `Candidates: ${names.join(", ")}.` : "No candidates."
        }`;
      }
      case "claim": {
        const { statement: st, link } = settleOf(card);
        return `Is this claim right: ${st.subject.name} (${st.subject.type}) → ${link} → ${st.object.name} (${st.object.type})?`;
      }
      case "value": {
        const s = settleOf(card);
        const values = [
          ...new Set(s.values.map((v) => propValueText(v.value))),
        ];
        return `Which ${s.prop} of ${s.type} ${s.entityKey} is right: ${values.join(" or ")}?`;
      }
    }
  })();
  return `Help me decide review ${settleOf(card).reviewId} in “${documentName}”. ${question} The document says: “${clipped}”. Recommend one choice and say why; don’t settle it.`;
}
