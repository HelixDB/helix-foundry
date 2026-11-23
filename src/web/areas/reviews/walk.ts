import type {
  DecisionApplied,
  FoundryReviewChoice,
  FoundryReviewDecision,
  FoundryReviewDraw,
} from "../../../shared/wfe";
import {
  advise,
  asks,
  coveredBy,
  laneOf,
  settleOf,
  type Advice,
  type AdviceContext,
} from "./advice";
import { formatNumber } from "../../kit/util";
import { BAND_TEXT, type QueueCard } from "./queue";

// The review walkthrough's state: one card at a time over the decide lane,
// in the index's order, a page at a time, and what became of each card.
// Pure: the page holds it in a reducer, sends what a decision saves, and
// tells the walk what the backend answered.

/** How a person decided a card: Founder's suggestion, another choice, or a batch. */
export type How = "accepted" | "chosen" | "batch";

/** What became of a card in this walk; a card with none is still to decide. */
export type Outcome =
  | { kind: "sending"; choice: FoundryReviewChoice; how: How }
  | { kind: "saved"; decision: FoundryReviewDecision; how: How }
  // A name's choice saved whose job failed before it said what the current
  // load made of it: still to decide, choosing again finishes it.
  | { kind: "unconfirmed"; decision: FoundryReviewDecision; how: How }
  | {
      kind: "refused";
      choice: FoundryReviewChoice;
      how: How;
      message: string;
      retry: boolean;
    }
  | { kind: "skipped" }
  // Settled by the choice on card `by`: a name the backend links in every
  // document asking about it at once.
  | { kind: "covered"; by: string }
  // Decided by someone else meanwhile.
  | { kind: "elsewhere" }
  // A link this walk took back that the current load keeps (a name's link
  // applies in place, and removing it only stops it from the next load): on
  // the card linked, and on the cards it covered.
  | { kind: "taken_back" };

/**
 * A walk of a load's cards: their keys in order, the card shown (`at`; the
 * order's length once past the last), what became of each, and the undo
 * stack, one entry per decision or batch.
 */
export type Walk = {
  loadId: string;
  order: readonly string[];
  at: number;
  outcomes: ReadonlyMap<string, Outcome>;
  undo: readonly (readonly string[])[];
};

export type WalkAction =
  // A choice on a card, and the cards it covers (a name linked in every
  // document asking about it).
  | {
      type: "decide";
      key: string;
      choice: FoundryReviewChoice;
      how: How;
      covers?: readonly string[];
    }
  | {
      type: "batch";
      items: readonly { key: string; choice: FoundryReviewChoice }[];
    }
  | { type: "sent"; key: string; decision: FoundryReviewDecision }
  | { type: "refused"; key: string; message: string; retry: boolean }
  | { type: "skip" }
  | { type: "back" }
  | { type: "jump"; key: string }
  // Moves to the next card, or past the last, marking nothing (a member's
  // Next).
  | { type: "next" }
  // The keys' choices taken back; `kept`, those whose link the current load
  // keeps.
  | { type: "undone"; keys: readonly string[]; kept?: ReadonlySet<string> }
  // The current load refused these keys' links in place (they apply from
  // the next load): the cards each covered are to decide again.
  | { type: "uncover"; by: ReadonlySet<string> }
  // The decisions read say the jobs of these keys' saved name choices
  // failed before saying what became of them (unconfirmedNames).
  | { type: "unconfirmed"; keys: ReadonlySet<string> }
  // The next page of cards read: its keys go after the order, the card shown
  // stays.
  | { type: "extend"; loadId: string; keys: readonly string[] }
  // The cards read again: a new order (another load's, or a filter's), and
  // the keys the index no longer lists (decided, here or elsewhere).
  | {
      type: "rebase";
      loadId: string;
      order: readonly string[];
      gone: ReadonlySet<string>;
    };

/**
 * Whether a walk takes cards Founder has a suggestion for, those it has
 * none for, or any. The index filters kinds, types and documents itself.
 */
export type AdviceFilter = "any" | "suggested" | "none";

/**
 * The walk's cards by lane, in the index's order (cards to decide, those
 * for information, and names an earlier decision covers), every card by
 * key, Founder's advice for each, and for a name the load asks about, the
 * other documents' cards of the same name a link covers (`siblings`).
 */
export type WalkOrder = {
  decide: string[];
  fyi: string[];
  covered: string[];
  cards: ReadonlyMap<string, QueueCard>;
  advice: ReadonlyMap<string, Advice>;
  siblings: ReadonlyMap<string, readonly string[]>;
};

/**
 * The walk of the index's `cards` (decide and FYI) under an advice filter.
 * Each card goes to its lane, a name's held claims right after it to
 * decide; a name an earlier link covers goes to Covered. The filter takes
 * the cards to decide that Founder has a suggestion for, or none for.
 *
 * @example
 * walkOrder(cards, ctx, "any").decide;
 */
export function walkOrder(
  list: readonly QueueCard[],
  ctx: AdviceContext,
  f: AdviceFilter,
): WalkOrder {
  const cards = new Map<string, QueueCard>(),
    advice = new Map<string, Advice>();
  const lanes = {
    decide: [] as string[],
    fyi: [] as string[],
    covered: [] as string[],
  };
  const taken = (a: Advice) =>
    f === "any" || (f === "suggested") === (a.kind === "suggest");
  // Names the load asks about and no decision covers, by type and name.
  const asking = new Map<string, string[]>();
  for (const c of list) {
    if (cards.has(c.key)) continue;
    const lane =
      c.kind === "name" && coveredBy(c, ctx)
        ? "covered"
        : laneOf(c) === "fyi"
          ? "fyi"
          : "decide";
    if (c.kind === "name" && lane === "decide" && asks(c)) {
      const s = settleOf(c),
        at = JSON.stringify([s.type, s.normName]);
      asking.set(at, [...(asking.get(at) ?? []), c.key]);
    }
    for (const d of c.kind === "name" ? [c, ...c.held] : [c]) {
      const a = advise(d, ctx);
      cards.set(d.key, d);
      advice.set(d.key, a);
      const into = d === c ? lane : "decide";
      if (into !== "decide" || taken(a)) lanes[into].push(d.key);
    }
  }
  const siblings = new Map(
    [...asking.values()].flatMap((keys) =>
      keys.map((k) => [k, keys.filter((other) => other !== k)] as const),
    ),
  );
  return { ...lanes, cards, advice, siblings };
}

/**
 * Whether a card is still to decide: no outcome, its choice refused, or a
 * name's choice whose job failed before saying what became of it.
 */
const open = (o: Outcome | undefined) =>
  !o || o.kind === "refused" || o.kind === "unconfirmed";

/**
 * How many of `keys` are still undecided by the walk's `outcomes`: no
 * choice saved or sending (a choice refused, or the card skipped, is still
 * undecided), not covered by a link, not decided elsewhere. Every count of
 * what is left to decide (the Decide lane, the Facts tab, the Ontologies
 * panel's Review) is this, over the walk's cards to decide.
 *
 * @example
 * undecided(["a", "b", "c"], new Map([["a", { kind: "skipped" }], ["b", { kind: "covered", by: "x" }]])); // 2
 */
export function undecided(
  keys: readonly string[],
  outcomes: ReadonlyMap<string, Outcome>,
): number {
  return keys.filter((k) => {
    const o = outcomes.get(k);
    return open(o) || o?.kind === "skipped";
  }).length;
}

/**
 * How many held claims among the load's undone spot checks (`draws`) a
 * batch of `batchKey` would take but leaves out: drawn at random to measure
 * their score band, so a person decides each on its own, at the finish.
 *
 * @example
 * drawnLike(spot.draws, 'claim:["signed_with","0.7"]'); // 1
 */
export function drawnLike(
  draws: readonly FoundryReviewDraw[],
  batchKey: string,
): number {
  return draws.filter(
    (d) =>
      !d.done &&
      !!d.held &&
      `claim:${JSON.stringify([d.link ?? "", d.band])}` === batchKey,
  ).length;
}

/**
 * Whether a batch takes the card `key` now: in the walk's order and still to
 * decide (or skipped). A card decided meanwhile, here or elsewhere, covered
 * by a link, or no longer in the walk is left as it is.
 *
 * @example
 * batchTakes({ loadId: "L", order: ["a"], at: 0, outcomes: new Map(), undo: [] }, "a"); // true
 */
export function batchTakes(w: Walk, key: string): boolean {
  const o = w.outcomes.get(key);
  return w.order.includes(key) && (open(o) || o?.kind === "skipped");
}

/** The first card still to decide from `from` on; the order's length if none. */
const nextOpen = (
  order: readonly string[],
  outcomes: ReadonlyMap<string, Outcome>,
  from: number,
) => {
  const i = order.findIndex((k, j) => j >= from && open(outcomes.get(k)));
  return i < 0 ? order.length : i;
};

/**
 * The walk after an action:
 * - `decide` sends a choice on a card (not one still sending), covers the
 *   open cards it names, and goes to the next card to decide after it;
 * - `batch` sends each item the batch takes (batchTakes), as one undo, and
 *   goes to the next card to decide from the current one;
 * - `sent` and `refused` say what became of a card still sending; a refusal
 *   to retry puts the card right after the current one (or makes it current
 *   when it is, or the walk is past the end), and a refusal uncovers what
 *   the choice covered;
 * - `skip` marks the current card skipped (unless decided) and goes to the
 *   next to decide; `next`, `back` and `jump` move without changing
 *   anything;
 * - `undone` forgets the outcomes of the keys (and what they covered), or,
 *   for those `kept`, marks them and what they covered taken back, drops
 *   them from the undo stack and shows the first of them again;
 * - `uncover` opens again the cards that the keys' choices covered;
 * - `extend` adds a page's keys after the order (those not in it), the
 *   card shown staying; past the end, the first of them is shown;
 * - `rebase` takes a new order: on the same load every outcome stays (a
 *   filter, or a refresh), and each card of the old order gone from the
 *   queue (`gone`) keeps its place, after the card before it that the new
 *   order has, marked decided elsewhere unless this walk decided it, and a
 *   card decided elsewhere that is back in the order is to decide again; on
 *   another load only the outcomes of keys in the order and of choices
 *   still sending, whose answers are yet to come, stay, less links taken
 *   back (the new load asks again); the card shown
 *   stays if it is still in the order, else the next of the old order that
 *   is, else the first to decide.
 * An action that changes nothing returns the walk itself.
 *
 * @example
 * walkReducer(walk, { type: "skip" }).at; // the next card to decide
 */
export function walkReducer(w: Walk, a: WalkAction): Walk {
  switch (a.type) {
    case "decide": {
      const i = w.order.indexOf(a.key);
      if (i < 0 || w.outcomes.get(a.key)?.kind === "sending") return w;
      const outcomes = new Map(w.outcomes).set(a.key, {
        kind: "sending",
        choice: a.choice,
        how: a.how,
      });
      for (const k of a.covers ?? [])
        if (open(outcomes.get(k)) || outcomes.get(k)?.kind === "skipped")
          outcomes.set(k, { kind: "covered", by: a.key });
      return {
        ...w,
        outcomes,
        at: nextOpen(w.order, outcomes, i + 1),
        undo: [...w.undo, [a.key]],
      };
    }
    case "batch": {
      const items = a.items.filter((it) => batchTakes(w, it.key));
      if (!items.length) return w;
      const outcomes = new Map(w.outcomes);
      for (const it of items)
        outcomes.set(it.key, {
          kind: "sending",
          choice: it.choice,
          how: "batch",
        });
      return {
        ...w,
        outcomes,
        at: nextOpen(w.order, outcomes, w.at),
        undo: [...w.undo, items.map((it) => it.key)],
      };
    }
    case "sent": {
      const o = w.outcomes.get(a.key);
      if (o?.kind !== "sending") return w;
      return {
        ...w,
        outcomes: new Map(w.outcomes).set(a.key, {
          kind: "saved",
          decision: a.decision,
          how: o.how,
        }),
      };
    }
    case "refused": {
      const o = w.outcomes.get(a.key);
      if (o?.kind !== "sending") return w;
      const outcomes = new Map(
        [...w.outcomes].filter(
          ([, c]) => !(c.kind === "covered" && c.by === a.key),
        ),
      ).set(a.key, {
        kind: "refused",
        choice: o.choice,
        how: o.how,
        message: a.message,
        retry: a.retry,
      });
      const from = w.order.indexOf(a.key);
      if (!a.retry || from < 0) return { ...w, outcomes };
      const current = w.order[w.at];
      const without = w.order.filter((k) => k !== a.key);
      // Right after the card shown; where it is when it is the card shown,
      // and last, shown, when the walk is past its end.
      const place =
        current === a.key
          ? from
          : current === undefined
            ? without.length
            : without.indexOf(current) + 1;
      const order = [
        ...without.slice(0, place),
        a.key,
        ...without.slice(place),
      ];
      return {
        ...w,
        outcomes,
        order,
        at:
          current === undefined || current === a.key
            ? place
            : order.indexOf(current),
      };
    }
    case "skip": {
      const key = w.order[w.at];
      if (key === undefined) return w;
      const outcomes = open(w.outcomes.get(key))
        ? new Map(w.outcomes).set(key, { kind: "skipped" })
        : w.outcomes;
      return { ...w, outcomes, at: nextOpen(w.order, outcomes, w.at + 1) };
    }
    case "next":
      return w.at < w.order.length ? { ...w, at: w.at + 1 } : w;
    case "back":
      return w.at > 0 ? { ...w, at: w.at - 1 } : w;
    case "jump": {
      const at = w.order.indexOf(a.key);
      return at < 0 || at === w.at ? w : { ...w, at };
    }
    case "undone": {
      const gone = new Set(a.keys),
        kept = a.kept ?? new Set<string>();
      const outcomes = new Map(
        [...w.outcomes].flatMap(([k, o]): [string, Outcome][] => {
          const of = (keys: ReadonlySet<string>) =>
            keys.has(k) || (o.kind === "covered" && keys.has(o.by));
          return of(kept)
            ? [[k, { kind: "taken_back" }]]
            : of(gone)
              ? []
              : [[k, o]];
        }),
      );
      const first = w.order.findIndex((k) => gone.has(k));
      return {
        ...w,
        outcomes,
        at: first < 0 ? w.at : first,
        undo: w.undo
          .map((e) => e.filter((k) => !gone.has(k)))
          .filter((e) => e.length),
      };
    }
    case "unconfirmed": {
      const outcomes = new Map(w.outcomes);
      for (const k of a.keys) {
        const o = outcomes.get(k);
        if (o?.kind === "saved")
          outcomes.set(k, {
            kind: "unconfirmed",
            decision: o.decision,
            how: o.how,
          });
      }
      return { ...w, outcomes };
    }
    case "uncover": {
      const outcomes = new Map(
        [...w.outcomes].filter(
          ([, o]) => !(o.kind === "covered" && a.by.has(o.by)),
        ),
      );
      return outcomes.size === w.outcomes.size ? w : { ...w, outcomes };
    }
    case "extend": {
      if (a.loadId !== w.loadId) return w;
      const has = new Set(w.order);
      const more = a.keys.filter((k) => !has.has(k));
      return more.length ? { ...w, order: [...w.order, ...more] } : w;
    }
    case "rebase": {
      const same = a.loadId === w.loadId,
        inOrder = new Set(a.order);
      const outcomes = new Map(
        [...w.outcomes].filter(
          ([k, o]) =>
            (same || inOrder.has(k) || o.kind === "sending") &&
            (same || o.kind !== "taken_back"),
        ),
      );
      // Back in the queue: someone else's decision was removed, or no
      // longer settles it.
      for (const k of a.order)
        if (outcomes.get(k)?.kind === "elsewhere") outcomes.delete(k);
      // The cards gone from the queue, by the card of the new order they
      // follow (null: before the first).
      const stays = new Map<string | null, string[]>();
      let anchor: string | null = null;
      for (const k of same ? w.order : []) {
        if (inOrder.has(k)) anchor = k;
        else if (a.gone.has(k)) {
          const after = stays.get(anchor);
          if (after) after.push(k);
          else stays.set(anchor, [k]);
          const o = outcomes.get(k);
          if (open(o) || o?.kind === "skipped")
            outcomes.set(k, { kind: "elsewhere" });
        }
      }
      const order = [
        ...(stays.get(null) ?? []),
        ...a.order.flatMap((k) => [k, ...(stays.get(k) ?? [])]),
      ];
      const inNew = new Set(order);
      const kept = w.order.slice(w.at).find((k) => inNew.has(k));
      return {
        loadId: a.loadId,
        order,
        at:
          kept === undefined
            ? nextOpen(order, outcomes, 0)
            : order.indexOf(kept),
        outcomes,
        undo: w.undo
          .map((e) => e.filter((k) => outcomes.has(k)))
          .filter((e) => e.length),
      };
    }
  }
}

/**
 * Whether the current load links a saved resolve's name to its record now:
 * pending or written by the decisions last read (`read`: each one's
 * `applied`, by id), or, before they hold it, as foreseen (`foreseen`: its
 * choice applies now). A name marked not in data, or another decision,
 * links nothing.
 *
 * @example
 * linkedNow(resolveDecision, new Map([[resolveDecision.id, { status: "written" }]]), false); // true
 * linkedNow(resolveDecision, new Map(), true); // true: not read yet, foreseen
 */
export function linkedNow(
  decision: FoundryReviewDecision,
  read: ReadonlyMap<string, DecisionApplied | null | undefined>,
  foreseen: boolean,
): boolean {
  if (decision.kind !== "resolve" || !decision.entityKey) return false;
  if (!read.has(decision.id)) return foreseen;
  const status = read.get(decision.id)?.status;
  return status === "pending" || status === "written";
}

/**
 * The cards whose saved link the decisions read since (`read`) say the
 * current load doesn't hold (refused, missed, its job failed, or not held
 * at all), so what each covered is to decide again. A link not read yet
 * still covers.
 *
 * @example
 * notLinked(walk.outcomes, new Map([[decision.id, { status: "unconfirmed" }]])); // Set { "a" }
 */
export function notLinked(
  outcomes: ReadonlyMap<string, Outcome>,
  read: ReadonlyMap<string, DecisionApplied | null | undefined>,
): Set<string> {
  return new Set(
    [...outcomes].flatMap(([k, o]) =>
      o.kind === "saved" &&
      o.decision.kind === "resolve" &&
      o.decision.entityKey &&
      read.has(o.decision.id) &&
      !linkedNow(o.decision, read, false)
        ? [k]
        : [],
    ),
  );
}

/**
 * The cards whose saved name choice the decisions read (`decisions`) say is
 * unconfirmed: its job failed before it said what the current load made of
 * it, so the card is to choose again. Only the read of that very save
 * counts (the same `decidedAt`): choosing again saves the decision anew,
 * and a read from before then is no word on it.
 *
 * @example
 * unconfirmedNames(walk.outcomes, [{ ...decision, applied: { status: "unconfirmed" } }]); // Set { "a" }
 */
export function unconfirmedNames(
  outcomes: ReadonlyMap<string, Outcome>,
  decisions: readonly FoundryReviewDecision[],
): Set<string> {
  const read = new Map(decisions.map((d) => [d.id, d]));
  return new Set(
    [...outcomes].flatMap(([k, o]) => {
      if (o.kind !== "saved" || o.decision.kind !== "resolve") return [];
      const d = read.get(o.decision.id);
      return d?.applied?.status === "unconfirmed" &&
        d.decidedAt === o.decision.decidedAt
        ? [k]
        : [];
    }),
  );
}

/** The key of the card shown; null past the last. */
export const current = (w: Walk): string | null => w.order[w.at] ?? null;

/**
 * How far a walk is: its cards, those decided by accepting Founder's
 * suggestion (alone or in a batch) or by another choice, those skipped,
 * those a link settled (covered), and those left to decide (a refused
 * choice is still to decide; a card covered or decided elsewhere isn't).
 *
 * @example
 * progress(walk); // { total: 214, accepted: 30, chosen: 4, skipped: 3, covered: 2, left: 175 }
 */
export function progress(w: Walk): {
  total: number;
  accepted: number;
  chosen: number;
  skipped: number;
  covered: number;
  left: number;
} {
  const outcomes = w.order.map((k) => w.outcomes.get(k));
  const decided = (how: (h: How) => boolean) =>
    outcomes.filter(
      (o) => (o?.kind === "sending" || o?.kind === "saved") && how(o.how),
    ).length;
  return {
    total: w.order.length,
    accepted: decided((h) => h !== "chosen"),
    chosen: decided((h) => h === "chosen"),
    skipped: outcomes.filter((o) => o?.kind === "skipped").length,
    covered: outcomes.filter((o) => o?.kind === "covered").length,
    left: outcomes.filter(open).length,
  };
}

/**
 * "Accept all like this" for the card `key`: every card of `keys` whose
 * suggestion shares its batch key, with the choice each one suggests, in
 * order. None when the card's suggestion isn't batchable or it would be a
 * batch of one.
 *
 * @example
 * batchOf(openKeys, advice, current(walk)!); // { batchKey: "claim:…", items: [...] } or null
 */
export function batchOf(
  keys: readonly string[],
  advice: ReadonlyMap<string, Advice>,
  key: string,
): {
  batchKey: string;
  items: { key: string; choice: FoundryReviewChoice }[];
} | null {
  const of = advice.get(key);
  if (of?.kind !== "suggest" || of.batchKey === null) return null;
  const batchKey = of.batchKey;
  const items = keys.flatMap((k) => {
    const a = advice.get(k);
    return a?.kind === "suggest" && a.batchKey === batchKey
      ? [{ key: k, choice: a.choice }]
      : [];
  });
  return items.length > 1 ? { batchKey, items } : null;
}

/**
 * What Accept all like this says of Founder's suggestion for its `n` cards: the
 * choice (`label`, the card's word for it) and what it stands on, in one
 * sentence; null without a suggestion.
 *
 * @example
 * batchReason(measured, "Approve", 3);
 * // "Founder suggests Approve for all 3: random checks of claims scored 0.7 to 0.8 were right 80 of 80 times."
 */
export function batchReason(
  advice: Advice,
  label: string | undefined,
  n: number,
): string | null {
  if (advice.kind !== "suggest") return null;
  const head = `Founder suggests ${label ?? "the same choice"} for ${n === 1 ? "it" : `all ${formatNumber(n)}`}`;
  const s = advice.strength;
  switch (s.kind) {
    case "measured": {
      const [band] = advice.basis.flatMap((b) =>
        b.kind === "verifier" && b.band !== "none" ? [b.band] : [],
      );
      return `${head}: random checks of claims${band ? ` scored ${BAND_TEXT[band]}` : ""} were right ${formatNumber(s.right)} of ${formatNumber(s.total)} times.`;
    }
    case "repeat":
      return `${head}: ${s.by === "you" ? "you" : "the team"} chose it for this name ${s.decisions === 1 ? "once" : `${formatNumber(s.decisions)} times`} before.`;
    case "unmeasured":
      return `${head}.`;
  }
}
