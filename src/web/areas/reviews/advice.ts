import type {
  FoundryReviewChoice,
  FoundryReviewDecision,
  FoundryReviewNameDecisions,
  ReviewDecision,
} from "../../../shared/wfe";
import { choicesOf } from "./choices";
import type { Span } from "../explore/model";
import {
  approvable,
  holdingEnds,
  type Settle,
  type StatedValue,
} from "./model";
import { normalizeName } from "../explore/naming";
import {
  BLOCKING,
  type Band,
  type ClaimCard,
  type Lane,
  type NameCard,
  type Precision,
  type ValueCard,
} from "./queue";

// Founder's suggestion for a card of the review walkthrough: a choice from
// fixed rules over what the card and the saved decisions already hold, at no
// model cost, computed whenever the walk's cards are read and stored
// nowhere, so it follows the decisions as they change. The index sends what
// the rules read with each page of cards: each name's decisions across
// documents (how many, how many choices, the newest), the decisions at each
// held claim's place, and, on the first page, the spot checks' precision. Pure. A suggestion is only ever
// accepted by a person, through the same settle call a card sends.
//
// The rules, first that fires:
// - precedent (names): every resolve decision of the same type and name in
//   another document chose the same (one choice), an entity this card lists
//   and the read found, or Not in data. A repeat of the person's own choice
//   (every decision theirs) may be accepted in a batch;
// - single_candidate (names): the card lists one candidate, found in the
//   load. Not measured, so never batched;
// - confident_claim (held claims): Approve all would take it (approvable),
//   no name holds it, and its score band has random labels, mostly right.
//   Batched only when the band is measured: the Wilson lower bound of its
//   share right at least BATCH_LOWER over at least BATCH_LABELS labels;
// - majority_value (values): no table value, at least MIN_QUOTES quotes, at
//   least two thirds of them stating one value. Never batched.
// Otherwise the card says why there is none (NoAdvice), unless the rules
// abstain where Founder's model may advise (MODEL_WHYS: a name between
// candidates, a value the documents split on or state too rarely) and the
// index sent the model's pick (`modelAdvice`, the graph worker's, written
// from the quotes): it shows only while the card offers exactly the
// choices the model was offered, as "model", unmeasured and never batched.
// This is the only gate on what model advice shows; the backend's port of
// these abstentions (backend/reviewadvice Why) decides only what it pays
// for, so whatever it sends anywhere else is ignored.

/** A card a person decides in the walkthrough. */
export type Decidable = NameCard | ClaimCard | ValueCard;

export type RuleId =
  | "precedent"
  | "single_candidate"
  | "confident_claim"
  | "majority_value"
  // Founder's model, where the rules abstain (MODEL_WHYS).
  | "model";

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
  | { kind: "majority"; agree: number; of: number }
  // The model's own words (plain text, never markup) and the quotes it
  // cites.
  | { kind: "model"; rationale: string; cites: ModelCite[] };

/** A place in a document Founder's model cites. */
export type ModelCite = Span & { documentId: string };

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
 * The resolve decisions of one name across the ontology's documents: how
 * many, how many different choices, and the newest (at most 20).
 */
export type NameDecisions = {
  total: number;
  choices: number;
  newest: readonly Resolve[];
};

/**
 * What the rules read, indexed once a page of cards so a card reads only
 * its own: each name's decisions by type and name (`names`), claim
 * decisions by place (`claims`), the bands' precision, which links hold one
 * value, and the person deciding (`user`).
 */
export type AdviceContext = {
  names: ReadonlyMap<string, NameDecisions>;
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

const nameAt = (type: string, normName: string) =>
  JSON.stringify([type, normName]);
const claimAt = (docId: string, section: number, start: number, end: number) =>
  JSON.stringify([docId, section, start, end]);

/**
 * The context of the index's `names` and the claim decisions its cards
 * carry (`claims`), indexed.
 *
 * @example
 * adviceContext({ names: [], claims: [], precision: { bands: [], rechecks: { agreed: 0, total: 0 } }, single: () => false, user: "u" }).names.size; // 0
 */
export function adviceContext({
  names,
  claims,
  precision,
  single,
  user,
}: {
  names: readonly FoundryReviewNameDecisions[];
  claims: readonly FoundryReviewDecision[];
  precision: Precision;
  single: (link: string) => boolean;
  user: string;
}): AdviceContext {
  const byPlace = new Map<string, FoundryReviewDecision[]>();
  for (const d of claims)
    if (d.kind !== "resolve" && d.kind !== "prop") {
      const at = claimAt(d.docId, d.claimSection, d.claimStart, d.claimEnd);
      if (!byPlace.get(at)?.some((x) => x.id === d.id))
        byPlace.set(at, [...(byPlace.get(at) ?? []), d]);
    }
  return {
    names: new Map(
      names.map((n) => [
        nameAt(n.type, n.normName),
        {
          total: n.total ?? 0,
          choices: n.choices ?? 0,
          newest: (n.newest ?? []).filter(
            (d): d is Resolve => d.kind === "resolve",
          ),
        },
      ]),
    ),
    claims: byPlace,
    precision,
    single,
    user,
  };
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
 * A card's settle: cardsOf (./cards.ts) makes a name's a resolve, a held
 * claim's a release and a value's a prop, and keeps stale reviews off every
 * card.
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
 * A card's lane, as the index lays it out: a name the model linked, and a
 * value a table holds against documents, are in their load already and
 * block nothing, so they are for information (each can still be changed);
 * everything else is to decide.
 *
 * @example
 * laneOf(adjudicatedName); // "fyi"
 * laneOf(heldClaim); // "decide"
 */
export const laneOf = (card: Decidable): Lane =>
  card.kind === "claim" ? "decide" : card.lane;

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
    (ctx.names.get(nameAt(s.type, s.normName))?.newest ?? []).find(
      (d) =>
        d.docId !== s.docId &&
        (d.applied?.status === "pending" || d.applied?.status === "written") &&
        card.review.candidates.some((c) => c.key === d.entityKey && !!c.entity),
    ) ?? null
  );
}

const none = (why: NoAdvice): Advice => ({ kind: "none", why });

/**
 * The rules' abstentions Founder's model may advise on: a name between
 * candidates, a value the documents split on or state too rarely.
 */
export const MODEL_WHYS: ReadonlySet<NoAdvice> = new Set<NoAdvice>([
  "tied_candidates",
  "values_split",
  "few_quotes",
]);

/**
 * Founder's suggestion for a card, by the first rule that fires; else, where
 * the rules abstain on a name or a value (MODEL_WHYS), Founder's model's
 * (modelAdvised); none, with why, when there is neither. A card for
 * information gets none.
 *
 * @example
 * advise(heldClaimOfAMeasuredBand, ctx); // { kind: "suggest", rule: "confident_claim", … }
 * advise(tiedNameWithModelAdvice, ctx); // { kind: "suggest", rule: "model", strength: { kind: "unmeasured" }, batchKey: null, … }
 */
export function advise(card: Decidable, ctx: AdviceContext): Advice {
  if (laneOf(card) === "fyi") return none("fyi");
  const rules =
    card.kind === "name"
      ? adviseName(card, ctx)
      : card.kind === "claim"
        ? adviseClaim(card, ctx)
        : adviseValue(card);
  return rules.kind === "none" && MODEL_WHYS.has(rules.why)
    ? (modelAdvised(card) ?? rules)
    : rules;
}

/**
 * A choice's identity, as two choices are compared: its kind and value (an
 * integer by its digits, a number by its value), or undefined for one this
 * page doesn't know.
 *
 * @example
 * choiceKey({ prop: { integer: "012" } }) === choiceKey({ prop: { integer: "12" } }); // true
 * choiceKey({ prop: { integer: "12" } }) === choiceKey({ prop: { number: 12 } }); // false
 */
export function choiceKey(
  c: FoundryReviewChoice | undefined,
): string | undefined {
  if (c?.resolve)
    return c.resolve.entityKey
      ? `entity:${c.resolve.entityKey}`
      : c.resolve.notInData
        ? "not_in_data"
        : undefined;
  const v = c?.prop;
  if (!v) return undefined;
  if (typeof v.text === "string") return `text:${v.text}`;
  if (v.integer !== undefined && /^-?\d+$/.test(String(v.integer)))
    return `integer:${BigInt(String(v.integer)).toString()}`;
  if (typeof v.number === "number" && Number.isFinite(v.number))
    return `number:${v.number}`;
  if (typeof v.boolean === "boolean") return `boolean:${v.boolean}`;
  return undefined;
}

/**
 * A model's rationale as it may show, as plain text: control characters and
 * line breaks made spaces, bidi marks, embeddings, overrides and isolates
 * removed, trimmed; "" when nothing is left or more than 300 characters
 * are. The backend sanitizes it so too; this keeps the page safe whatever
 * it is sent.
 *
 * @example
 * plainRationale("Quote 2 names\n\u202eGlobex\u202c Ltd."); // "Quote 2 names Globex Ltd."
 */
export function plainRationale(s: unknown): string {
  if (typeof s !== "string") return "";
  const out = s
    .replace(/[\u200e\u200f\u202a-\u202e\u2066-\u2069]/gu, "")
    .replace(/[\p{Cc}\s\u2028\u2029]+/gu, " ")
    .trim();
  return [...out].length > 300 ? "" : out;
}

/**
 * Founder's model's suggestion on a name or value card, when the index sent
 * one that still fits: the card offers exactly the choices the model was
 * offered (none gained or lost since), its pick is one of them, and it has
 * a rationale and a quote it cites. It is unmeasured and never batched, as
 * no random checks measure it. Null otherwise.
 *
 * @example
 * modelAdvised(tiedName); // { kind: "suggest", rule: "model", choice: { resolve: { entityKey: "scus_2" } }, … }
 * modelAdvised(tiedNameThatGainedACandidate); // null
 */
export function modelAdvised(card: Decidable): Advice | null {
  const a = card.kind === "claim" ? undefined : card.modelAdvice;
  if (!a) return null;
  const choices = choicesOf(card.review, (c) => c.key);
  const offered = (a.offered ?? []).map(choiceKey);
  const has = new Set(choices.map((c) => choiceKey(c.decision)));
  if (
    offered.some((k) => k === undefined) ||
    new Set(offered).size !== has.size ||
    offered.some((k) => !has.has(k))
  )
    return null;
  const chosen = choices.find(
    (c) => choiceKey(c.decision) === choiceKey(a.choice),
  );
  const rationale = plainRationale(a.rationale);
  const cites = (a.cites ?? []).flatMap((c): ModelCite[] => {
    const [section, start, end] = [
      c.span?.section,
      c.span?.start,
      c.span?.end,
    ].map((n) => Number(n ?? 0));
    return c.docId &&
      [section, start, end].every((n) => Number.isSafeInteger(n) && n! >= 0) &&
      end! > start!
      ? [{ documentId: c.docId, section: section!, start: start!, end: end! }]
      : [];
  });
  if (!chosen || !rationale || !cites.length) return null;
  return {
    kind: "suggest",
    rule: "model",
    choice: chosen.decision,
    basis: [{ kind: "model", rationale, cites }],
    strength: { kind: "unmeasured" },
    batchKey: null,
  };
}

function adviseName(card: NameCard, ctx: AdviceContext): Advice {
  const s = settleOf(card),
    { candidates } = card.review;
  // A resolve's entity must be in the load: one the read didn't find isn't
  // offered.
  const found = (key: string) =>
    candidates.some((c) => c.key === key && !!c.entity);
  // The name's decisions are of other documents: one of this document's
  // would decide the card. One choice across all of them is a precedent.
  const n = ctx.names.get(nameAt(s.type, s.normName));
  const earlier = (n?.newest ?? []).filter((d) => d.docId !== s.docId);
  const only = earlier[0]?.entityKey ?? null;
  if (
    n?.choices === 1 &&
    earlier.length > 0 &&
    (only === null || found(only))
  ) {
    // Theirs alone only when every decision is read, and theirs.
    const by =
      n.total === n.newest.length &&
      earlier.every((d) => d.decidedByUserId === ctx.user)
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
          decisions: n.total,
          latest: earlier
            .map((d) => d.decidedAt ?? "")
            .reduce((a, b) => (b > a ? b : a), ""),
        },
      ],
      strength: { kind: "repeat", decisions: n.total, by },
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
        ? claimBatchKey(card)
        : null,
  };
}

/**
 * The batch key a measured held claim's suggestion has: its link and score
 * band, which every claim it may be accepted with shares.
 *
 * @example
 * claimBatchKey(heldClaimAt072); // 'claim:["signed_with","0.7"]'
 */
export const claimBatchKey = (card: ClaimCard): string =>
  `claim:${JSON.stringify([settleOf(card).link, card.band])}`;

/**
 * A name card's name: the words of the review's quote that are the name it
 * identifies (its normalized name, as the document spells it), else the
 * normalized name. The quote is the sentence the name is in, so it stays
 * the card's evidence and never its name.
 *
 * @example
 * cardName(nameOf("umbrella holdings", "Umbrella Holdings signed order form 3.")); // "Umbrella Holdings"
 * cardName(nameOf("umbrella holdings", "The holding company signed.")); // "umbrella holdings"
 */
export function cardName(card: NameCard): string {
  const { normName } = settleOf(card),
    { quote } = card.review;
  const words = [...quote.matchAll(/\S+/gu)];
  // The shortest run of the quote's words that folds to the name: a name
  // folds to as many words as it has, give or take what the fold joins or
  // splits.
  const most = normName.split(" ").length + 2;
  for (let n = 1; n <= most; n++)
    for (let i = 0; i + n <= words.length; i++) {
      const from = words[i]!.index,
        last = words[i + n - 1]!,
        run = quote.slice(from, last.index + last[0].length);
      if (normalizeName(run) === normName)
        return run
          .replace(/^[^\p{L}\p{N}+#&@$.]+/u, "")
          .replace(/[\s.,;:!?…"'”’)\]]+$/u, "");
    }
  return normName;
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
 * Whether a suggestion already says the verifier's score, so its card
 * needn't say it again.
 *
 * @example
 * statesScore({ kind: "none", why: "unscored" }); // false
 */
export const statesScore = (advice: Advice) =>
  advice.kind === "suggest" &&
  advice.basis.some((b) => b.kind === "verifier" && b.score !== undefined);

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
