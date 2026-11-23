// The review walkthrough, the Facts tab's body (./index.tsx): the current
// load's cards one at a time, in the index's order, each with Founder's
// suggestion (./advice.ts) and the choices a person settles it with, every
// one also a key (./keys.ts). A choice saves at once, through the same
// settle call a document's Needs review sends, a few at a time, and the walk
// moves on; Z takes the last choice (or batch) back. The walk (./walk.ts) is
// held by the page (useWalk), so it keeps its place across a switch of tabs
// and a read of the cards, and says so when a new load becomes current.
// Names the model linked and values a table keeps are for information (FYI),
// names an earlier link settles are Covered, and spot checks, drawn at
// random, wait for the finish.
import {
  useEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useSearchParams } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import type {
  FoundryLoad,
  FoundryReviewChoice,
  FoundryReviewDecision,
} from "../../../shared/wfe";
import {
  Button,
  Dialog,
  FilterSelect,
  Segmented,
  SkeletonText,
  cx,
  formatNumber,
  plural,
} from "../../kit";
import { ApiError } from "../../api";
import { sizeBucket, trackEvent } from "../../analytics";
import { paths } from "../../paths";
import { useShell } from "../../shell/context";
import { canManage, useWorkspace } from "../../ui";
import { useDocumentNames } from "../documents/documents-api";
import { ontologyQueries } from "../ontology/queries";
import { useOntology } from "../ontology/scope";
import {
  cardName,
  effectOf,
  founderSeed,
  settleOf,
  type Advice,
  type Decidable,
  type Effect,
} from "./advice";
import { deleteReviewDecision, settleReview } from "./api";
import { BatchDialog } from "./batch-dialog";
import { ADVICE_PAGES, adjustSummary } from "./cards-api";
import { choicesOf, layoutChoices, type Choice } from "./choices";
import { CoveredCards, FyiCards } from "./fyi";
import type { ReviewCards } from "./index";
import { useWalkKeys, type KeyCommand } from "./keys";
import type { QueueCard } from "./queue";
import { KIND_TEXT, ReviewCard, cardLine } from "./review-card";
import { settleAll } from "./settle-pool";
import { SpotChecks } from "./spot-checks";
import {
  batchOf,
  batchTakes,
  current,
  drawnLike,
  linkedNow,
  notLinked,
  progress,
  unconfirmedNames,
  undecided,
  walkReducer,
  type How,
  type Walk,
  type WalkOrder,
} from "./walk";
import { WalkDone } from "./walk-done";

/** How many choices are sent at once; the backend writes those that land together in one job. */
const AT_ONCE = 4;
/** gRPC's FAILED_PRECONDITION: the claim reads differently now, or the load changed. */
const FAILED_PRECONDITION = 9;
/** How long a toast shows. */
const TOAST_MS = 6000;
/** How many cards the rail lists: two before the one shown, and the rest after. */
const RAIL = 12;
/** What the Suggestion filter calls each choice of Founder's suggestion (`advice`). */
const ADVICE_TEXT = {
  any: "Any",
  suggested: "Suggested",
  none: "Needs you",
} as const;

const START: Walk = {
  loadId: "",
  order: [],
  at: 0,
  outcomes: new Map(),
  undo: [],
};

/** A choice on a card, as the walk sends it: what it does is kept for the finish. */
type Sending = {
  card: Decidable;
  choice: FoundryReviewChoice;
  effect: Effect;
};

/** What the backend answered a choice. */
type Answer =
  | { kind: "saved"; decision: FoundryReviewDecision }
  | { kind: "refused"; message: string };

/** What taking a choice back says, by the decision it removed. */
const UNDONE: Record<FoundryReviewDecision["kind"], string> = {
  release: "Approval taken back: the claim is held for review again.",
  reject: "Rejection removed: the claim is listed again.",
  resolve: "Choice removed: it stops applying from the next load.",
  prop: "Value taken back from this load.",
  withdraw: "Choice removed.",
};

const same = (a: FoundryReviewChoice, b: FoundryReviewChoice) =>
  JSON.stringify(a) === JSON.stringify(b);

/**
 * The walk of the Review page, held by the page so it outlives the tab: its
 * state and the actions that send and take back choices. Sends go a few at a
 * time (AT_ONCE), whoever started them; the decisions are read again once
 * none is in flight. Undo waits for a choice still sending, then removes
 * what it saved. While a choice is sending, leaving the page asks first.
 */
export function useWalk() {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const qc = useQueryClient();
  const [state, dispatch] = useReducer(walkReducer, START);
  // The walk as last rendered, for actions that finish after a render.
  const now = useRef(state);
  now.current = state;
  const gate = useRef({ flying: 0, waiting: [] as (() => void)[] });
  const sends = useRef(new Map<string, Promise<Answer>>());
  // Each card decided in this walk, and what its choice does.
  const made = useRef(new Map<string, { card: Decidable; effect: Effect }>());
  // Every card of the load's queue read in this walk, so one that leaves the
  // queue (decided, here or elsewhere) still shows in its place.
  const seen = useRef(new Map<string, { card: QueueCard; advice?: Advice }>());
  // The order last taken, which a page read after it extends.
  const last = useRef<{ loadId: string; order: readonly string[] }>(undefined);
  const undoing = useRef(false);
  // The choices still sending when a new load became current.
  const [changed, setChanged] = useState<readonly string[] | null>(null);
  const sending = [...state.outcomes.values()].some(
    (x) => x.kind === "sending",
  );
  // The walk's end, once per load: past its last card, having taken some.
  const finished =
    state.loadId !== "" && state.order.length > 0 && current(state) === null;
  const reported = useRef("");
  useEffect(() => {
    if (!finished || reported.current === state.loadId) return;
    reported.current = state.loadId;
    const p = progress(state);
    trackEvent("review_walk_finished", {
      size_bucket: sizeBucket(p.accepted + p.chosen),
    });
  }, [finished, state.loadId]);
  useEffect(() => {
    if (!sending) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [sending]);
  // The walk's own decisions, by id, and with `all` the ontology and its
  // loads (an undo may take back what a load wrote).
  const decidedKey = [ws, "ontology", o, "review-decided"];
  const refresh = (all: boolean) =>
    Promise.all([
      qc.invalidateQueries({ queryKey: decidedKey }),
      ...(all
        ? [
            ontologyQueries.ontology(ws, o).queryKey,
            ontologyQueries.loads(ws, o).queryKey,
          ].map((queryKey) => qc.invalidateQueries({ queryKey, exact: true }))
        : []),
    ]);
  const send = (s: Sending): Promise<Answer> => {
    const key = s.card.key,
      g = gate.current;
    made.current.set(key, { card: s.card, effect: s.effect });
    const answer = (async (): Promise<Answer> => {
      // A slot freed is handed to the next waiting, so `flying` stays.
      if (g.flying < AT_ONCE) g.flying += 1;
      else await new Promise<void>((go) => g.waiting.push(go));
      try {
        const { decision } = await settleReview(
          ws,
          o,
          settleOf(s.card).reviewId,
          { decision: s.choice, drawn: "ranked" },
        );
        dispatch({ type: "sent", key, decision });
        // One card fewer to decide, until the summary is counted again.
        adjustSummary(qc, ws, o, now.current.loadId, 1);
        return { kind: "saved", decision };
      } catch (e) {
        const message = (e as Error).message;
        dispatch({
          type: "refused",
          key,
          message,
          retry:
            !(e instanceof ApiError) ||
            e.status >= 500 ||
            e.code === FAILED_PRECONDITION,
        });
        return { kind: "refused", message };
      } finally {
        const next = g.waiting.shift();
        if (next) next();
        else g.flying -= 1;
        if (!g.flying) void refresh(false);
      }
    })();
    sends.current.set(key, answer);
    return answer;
  };
  return {
    state,
    dispatch,
    made: made.current,
    seen: seen.current,
    changed,
    dismissChanged: () => setChanged(null),
    /** Sends a choice on a card the walk shows, covering `covers`. */
    decide: (s: Sending & { how: How; covers: readonly string[] }) => {
      dispatch({
        type: "decide",
        key: s.card.key,
        choice: s.choice,
        how: s.how,
        covers: s.covers,
      });
      void send(s);
    },
    /**
     * Sends a batch as one undo: only the items the walk takes now
     * (batchTakes, as its reducer does), so a card decided or dropped from
     * the walk while the list was read isn't sent; what Stop kept from going
     * is open again.
     */
    batch: async (
      items: readonly Sending[],
      signal: AbortSignal,
      onProgress: (done: number) => void,
    ) => {
      const taken = items.filter((s) => batchTakes(now.current, s.card.key));
      dispatch({
        type: "batch",
        items: taken.map((s) => ({ key: s.card.key, choice: s.choice })),
      });
      const results = await settleAll(taken, send, {
        atOnce: AT_ONCE,
        signal,
        onProgress,
      });
      const stopped = results.flatMap((r) =>
        r.kind === "stopped" ? [r.item.card.key] : [],
      );
      if (stopped.length) dispatch({ type: "undone", keys: stopped });
      return results;
    },
    /**
     * Takes the last choice or batch back: removes what each saved (a
     * choice still sending first finishes) and forgets it, but a name's link
     * the current load holds (linkedNow, by the decisions as last read)
     * stays in that load, so its cards and those it covered show it taken
     * back rather than to decide. What it says, or null with nothing to take
     * back or an undo already running.
     */
    undo: async (): Promise<{ text: string; error: boolean } | null> => {
      const keys = now.current.undo.at(-1);
      if (!keys || undoing.current) return null;
      undoing.current = true;
      try {
        const removed = await Promise.all(
          keys.map(async (key) => {
            const out = now.current.outcomes.get(key);
            const answer =
              out?.kind === "saved" || out?.kind === "unconfirmed"
                ? ({ kind: "saved", decision: out.decision } as const)
                : await sends.current.get(key);
            if (answer?.kind !== "saved") return { key, ok: true } as const;
            try {
              await deleteReviewDecision(ws, o, answer.decision.id);
              return { key, ok: true, decision: answer.decision } as const;
            } catch (e) {
              return e instanceof ApiError && e.status === 404
                ? ({ key, ok: true, decision: answer.decision } as const)
                : ({ key, ok: false, error: e as Error } as const);
            }
          }),
        );
        const gone = removed.flatMap((r) => (r.ok ? [r.key] : []));
        adjustSummary(
          qc,
          ws,
          o,
          now.current.loadId,
          -removed.filter((r) => r.ok && r.decision).length,
        );
        const read = new Map(
          qc
            .getQueriesData<FoundryReviewDecision[]>({ queryKey: decidedKey })
            .flatMap(([, data]) => data ?? [])
            .map((d) => [d.id, d.applied]),
        );
        const kept = new Set(
          removed.flatMap((r) =>
            r.ok &&
            r.decision &&
            linkedNow(
              r.decision,
              read,
              made.current.get(r.key)?.effect.kind === "applies_now",
            )
              ? [r.key]
              : [],
          ),
        );
        if (gone.length) {
          const out = now.current.outcomes.get(gone[0]!);
          trackEvent("review_undone", {
            kind:
              out && "how" in out && out.how === "batch" ? "batch" : "choice",
          });
          dispatch({ type: "undone", keys: gone, kept });
        }
        await refresh(true);
        const failed = removed.flatMap((r) => (r.ok ? [] : [r.error]));
        if (failed.length)
          return {
            text: `Couldn’t take back ${plural(failed.length, "choice")}: ${failed[0]!.message}`,
            error: true,
          };
        const [one] = removed;
        return {
          text:
            removed.length > 1
              ? `Took back ${plural(removed.length, "choice")}.`
              : one?.ok && one.decision
                ? UNDONE[one.decision.kind]
                : "Choice cancelled.",
          error: false,
        };
      } finally {
        undoing.current = false;
      }
    },
    /**
     * Takes the cards' new walk order (its cards to decide; every card read,
     * with Founder's advice), and whether the index lists more after them:
     * the next page's cards go after the order (extend); otherwise, on
     * another load, it says which choices were still sending, and on the
     * same load, cards the index no longer lists, or now covered by a link,
     * keep their places. A card missing past the last the read still lists
     * may only have moved to a page not read yet, so it is dropped, not
     * marked decided elsewhere.
     */
    rebase: (
      loadId: string,
      { decide: order, cards: inQueue, advice, covered }: WalkOrder,
      more: boolean,
    ) => {
      const w = now.current,
        moved = w.loadId !== "" && w.loadId !== loadId,
        cover = new Set(covered);
      const before = last.current;
      last.current = { loadId, order };
      for (const [k, card] of inQueue)
        seen.current.set(k, { card, advice: advice.get(k) });
      if (
        before?.loadId === loadId &&
        w.loadId === loadId &&
        before.order.length < order.length &&
        before.order.every((k, i) => order[i] === k)
      ) {
        dispatch({
          type: "extend",
          loadId,
          keys: order.slice(before.order.length),
        });
        return;
      }
      if (moved) {
        setChanged(
          [...w.outcomes].flatMap(([k, x]) =>
            x.kind === "sending" ? [k] : [],
          ),
        );
        seen.current.clear();
        for (const [k, card] of inQueue)
          seen.current.set(k, { card, advice: advice.get(k) });
      }
      const listed = w.order.findLastIndex((k) => inQueue.has(k));
      dispatch({
        type: "rebase",
        loadId,
        order,
        // Cards a link now covers have left the walk too, here or elsewhere.
        gone: new Set(
          w.loadId === loadId
            ? w.order.filter(
                (k, i) =>
                  (!inQueue.has(k) && (!more || i < listed)) || cover.has(k),
              )
            : [],
        ),
      });
    },
  };
}

export type WalkApi = ReturnType<typeof useWalk>;

const KINDS = ["name", "claim", "value"] as const;
/** What the Kind filter calls each kind. */
const KIND_FILTER: Record<(typeof KINDS)[number], string> = {
  name: "Names",
  claim: "Held claims",
  value: "Values",
};
type Lane = "decide" | "fyi" | "covered";

/** A card the walk decides, of a queue card. */
const decidable = (c: QueueCard | undefined): Decidable | undefined =>
  c && (c.kind === "name" || c.kind === "claim" || c.kind === "value")
    ? c
    : undefined;

/**
 * The Facts tab's walk of the current load's cards (`queue`): the lanes
 * (`lane`, in the URL) and the filters first (in the URL: `kind`, one kind;
 * `advice`, `type`, `doc`), then the progress, then the rail of next cards
 * (the cards still to decide, at the finish) beside the card shown, or the
 * finish.
 */
export function ReviewWalkthrough({
  queue: q,
  load,
}: {
  queue: ReviewCards;
  load: FoundryLoad;
}) {
  const { id: ws, role } = useWorkspace(),
    { id: o } = useOntology();
  const qc = useQueryClient();
  const shell = useShell();
  const docName = useDocumentNames(ws, "A document");
  const { naming, walk: api } = q;
  const w = api.state;
  const [params, setParams] = useSearchParams();
  const { kind, type, doc, advice: adviceFilter } = q.filters;
  const laneParam = params.get("lane");
  const lane: Lane =
    laneParam === "fyi" || laneParam === "covered" ? laneParam : "decide";
  const setParam = (name: string, value: string) =>
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        if (value) next.set(name, value);
        else next.delete(name);
        return next;
      },
      { replace: true },
    );
  const { single, ctx } = q;
  const order = q.all!;
  useEffect(() => {
    api.rebase(load.id, order, q.hasMore);
  }, [load.id, order]);
  // The types and documents the filters offer: every one this visit's reads
  // of the load had a card of, so the row stays as it was while cards are
  // decided and leave the index, to the finish.
  const seenOffered = useRef<{
    load: string;
    types: Set<string>;
    docs: Set<string>;
  }>(undefined);
  const offered = useMemo(() => {
    if (seenOffered.current?.load !== load.id)
      seenOffered.current = {
        load: load.id,
        types: new Set(),
        docs: new Set(),
      };
    const { types, docs } = seenOffered.current;
    for (const k of order.advice.keys()) {
      const c = decidable(order.cards.get(k));
      if (!c) continue;
      docs.add(c.review.documentId);
      if (c.kind === "claim") {
        const { subject, object } = settleOf(c).statement;
        types.add(subject.type).add(object.type);
      } else types.add(settleOf(c).type);
    }
    return { types: [...types].sort(), docs: [...docs] };
  }, [order, load.id]);

  const synced = w.loadId === load.id;
  const key = synced ? current(w) : null;
  // A card of the queue, or one that left it during this walk.
  const cardOf = (k: string) =>
    decidable(order.cards.get(k) ?? api.seen.get(k)?.card);
  const card = key ? cardOf(key) : undefined;
  const advice: Advice | undefined =
    card && (order.advice.get(card.key) ?? api.seen.get(card.key)?.advice);
  const choices = useMemo(
    () => (card ? choicesOf(card.review, (c) => c.entity?.name ?? c.key) : []),
    [card],
  );
  const outcome = key ? w.outcomes.get(key) : undefined;
  const manage = canManage(role) && !!load.current;
  const open =
    !outcome ||
    outcome.kind === "refused" ||
    outcome.kind === "skipped" ||
    outcome.kind === "unconfirmed";
  const siblings = (card && order.siblings.get(card.key)) ?? [];
  const effectFor = (c: Decidable, choice: FoundryReviewChoice) =>
    effectOf(c, choice, (order.siblings.get(c.key) ?? []).length, single);
  // Founder's suggestion among the card's choices, laid out first.
  const layout = useMemo(
    () =>
      layoutChoices(
        choices,
        advice?.kind === "suggest"
          ? choices.find((c) => same(c.decision, advice.choice))
          : undefined,
      ),
    [choices, advice],
  );
  const suggestion = layout.primary?.decision;
  // What the suggestion does; with none, what every choice does alike, and
  // nothing when choices differ (no line reads as a recommendation).
  const shownEffect = (() => {
    if (!card) return null;
    if (suggestion) return effectFor(card, suggestion);
    const [first, ...rest] = choices.map((c) => effectFor(card, c.decision));
    return first &&
      rest.every((e) => JSON.stringify(e) === JSON.stringify(first))
      ? first
      : null;
  })();
  const batch = useMemo(() => {
    if (!key || !manage || !open) return null;
    // As the batch takes them (batchTakes): skipped cards too.
    const openKeys = w.order.filter((k) => undecided([k], w.outcomes) > 0);
    return batchOf(openKeys, order.advice, key);
  }, [key, manage, open, w, order.advice]);
  const p = progress(w);
  // What the current load made of each choice saved in this walk, by the
  // decisions read since (the decision a settle answers is read before its
  // job runs). A name's link it refused in place (a load read before names
  // could be settled so) applies from the next load (`notInPlace`, for the
  // finish); a link it doesn't hold, refused or not (its job failed),
  // settles none of the other documents now: their cards are to decide
  // again.
  const appliedOf = useMemo(
    () => new Map((q.decided.data ?? []).map((d) => [d.id, d.applied])),
    [q.decided.data],
  );
  const notInPlace = useMemo(
    () =>
      new Set(
        [...w.outcomes].flatMap(([k, x]) =>
          x.kind === "saved" &&
          x.decision.kind === "resolve" &&
          appliedOf.get(x.decision.id)?.status === "refused"
            ? [k]
            : [],
        ),
      ),
    [w.outcomes, appliedOf],
  );
  const uncover = useMemo(
    () => notLinked(w.outcomes, appliedOf),
    [w.outcomes, appliedOf],
  );
  useEffect(() => {
    if (uncover.size) api.dispatch({ type: "uncover", by: uncover });
  }, [uncover]);
  // A name's choice whose job failed before saying what became of it is to
  // choose again: the card offers its choices, and counts as still to decide.
  const unconfirmed = useMemo(
    () => unconfirmedNames(w.outcomes, q.decided.data ?? []),
    [w.outcomes, q.decided.data],
  );
  useEffect(() => {
    if (unconfirmed.size)
      api.dispatch({ type: "unconfirmed", keys: unconfirmed });
  }, [unconfirmed]);

  const [dialog, setDialog] = useState<
    | { kind: "help" }
    | {
        kind: "batch";
        batch: NonNullable<typeof batch>;
        advice: Advice;
        // The suggested choice's words, as the card showed them.
        label?: string;
      }
    | null
  >(null);
  const toasts = useRef(0);
  const [toast, setToast] = useState<{
    text: string;
    undo: boolean;
    error: boolean;
    n: number;
  } | null>(null);
  const say = (text: string, undo: boolean, error = false) =>
    setToast({ text, undo, error, n: (toasts.current += 1) });
  useEffect(() => {
    if (!toast) return;
    const t = setTimeout(() => setToast(null), TOAST_MS);
    return () => clearTimeout(t);
  }, [toast?.n]);
  // Focus goes to the card shown after the person moves the walk, so a
  // screen reader reads the next question; the progress line says how far.
  // A move made from a dialog (a batch) waits for it to close: the page is
  // inert behind it, and its own focus return names a card gone.
  const heading = useRef<HTMLHeadingElement>(null);
  const moved = useRef(false);
  useEffect(() => {
    if (!moved.current || dialog) return;
    moved.current = false;
    heading.current?.focus();
  }, [key, synced, dialog]);

  const undo = async () => {
    const r = await api.undo();
    if (!r) return;
    moved.current = true;
    say(r.text, false, r.error);
  };
  const decide = (choice: FoundryReviewChoice, how: "accepted" | "chosen") => {
    if (!card || !manage || !open) return;
    const covers = choice.resolve?.entityKey ? siblings : [];
    api.decide({ card, choice, how, covers, effect: effectFor(card, choice) });
    trackEvent("review_decided", {
      how,
      kind: card.kind,
      rule: advice?.kind === "suggest" ? advice.rule : "none",
    });
    moved.current = true;
    say(
      `${choices.find((c) => same(c.decision, choice))?.done ?? "Saved"}${covers.length ? `, with ${plural(covers.length, "other document")} asking about this name` : ""}.`,
      true,
    );
  };
  const choose = (c: Choice | undefined) =>
    c &&
    decide(
      c.decision,
      suggestion && same(suggestion, c.decision) ? "accepted" : "chosen",
    );
  const on = (c: KeyCommand) => {
    switch (c.kind) {
      case "accept":
        if (suggestion) decide(suggestion, "accepted");
        return;
      case "choose":
        return choose(layout.numbered[c.index]);
      case "not_in_data":
      case "reject":
        return choose(choices.find((x) => x.kind === c.kind));
      case "skip": {
        moved.current = true;
        // A member only reads: the next card (the finish after the last),
        // marking nothing skipped.
        api.dispatch({ type: manage ? "skip" : "next" });
        return;
      }
      case "back":
        moved.current = true;
        api.dispatch({ type: "back" });
        return;
      case "undo":
        void undo();
        return;
      case "batch":
        if (batch && advice)
          setDialog({
            kind: "batch",
            batch,
            advice,
            label: layout.primary?.label,
          });
        return;
      case "open":
        if (card)
          window.open(
            paths.document(o, card.review.documentId, "review", card.review),
            "_blank",
            "noopener",
          );
        return;
      case "founder":
        if (card && shell.assistant.available)
          shell.assistant.compose(
            founderSeed(card, docName(card.review.documentId)),
            "review",
          );
        return;
      case "help":
        setDialog((d) => (d?.kind === "help" ? null : { kind: "help" }));
        return;
    }
  };
  // Members move, open and ask with keys too; deciding needs `manage`.
  useWalkKeys(on, synced && lane === "decide", layout.numbered.length);

  const covered = [
    ...new Set([
      ...order.covered,
      ...w.order.filter((k) => w.outcomes.get(k)?.kind === "covered"),
    ]),
  ];
  const coveredBy = (k: string) => {
    const x = w.outcomes.get(k);
    const by = x?.kind === "covered" ? api.made.get(x.by)?.card : undefined;
    return by ? `in ${docName(by.review.documentId)}` : undefined;
  };
  const ofKind = (kind: "elsewhere" | "taken_back") =>
    [...w.outcomes.values()].filter((x) => x.kind === kind).length;
  const elsewhere = ofKind("elsewhere"),
    takenBack = ofKind("taken_back");
  // Around the card shown; past the last, only the cards still to decide
  // (skipped ones too), none once every card is decided.
  const start = Math.max(0, w.at - 2);
  const rail = (
    key
      ? w.order.slice(start, start + RAIL)
      : w.order.filter((k) => undecided([k], w.outcomes) > 0).slice(0, RAIL)
  ).flatMap((k) => {
    const c = cardOf(k);
    return c
      ? [{ key: k, card: c, state: w.outcomes.get(k)?.kind ?? "open" }]
      : [];
  });
  const jump = (k: string) => {
    moved.current = true;
    api.dispatch({ type: "jump", key: k });
  };
  // How many filters are on, and clearing them all.
  const filters = [kind, adviceFilter !== "any", type, doc].filter(
    Boolean,
  ).length;
  const clearFilters = () =>
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        for (const k of ["kind", "advice", "type", "doc"]) next.delete(k);
        return next;
      },
      { replace: true },
    );
  // Decide counts what the index counts open, less this walk's choices,
  // while no filter narrows it; else the cards read still to decide.
  const lanes: { value: Lane; label: string; count: number }[] = [
    {
      value: "decide",
      label: "Decide",
      count:
        !filters && q.open !== undefined
          ? q.open
          : undecided(w.order, w.outcomes),
    },
    { value: "fyi", label: "FYI", count: order.fyi.length },
    { value: "covered", label: "Covered", count: covered.length },
  ];

  return (
    <div className="rv-walk">
      {api.changed && (
        <LoadChanged
          keys={api.changed}
          walk={w}
          made={api.made}
          onDismiss={api.dismissChanged}
        />
      )}
      <div className="rv-walk-head">
        <div className="rv-walk-bar">
          <Segmented
            label="Lane"
            value={lane}
            onChange={(v) => setParam("lane", v === "decide" ? "" : v)}
            options={lanes}
          />
          <div className="rv-filters">
            <FilterSelect
              label="Kind"
              value={kind ?? ""}
              onChange={(v) => setParam("kind", v)}
              options={[
                { value: "", label: "Any" },
                ...KINDS.map((k) => ({ value: k, label: KIND_FILTER[k] })),
              ]}
            />
            <FilterSelect
              label="Suggestion"
              value={adviceFilter}
              onChange={(v) => setParam("advice", v === "any" ? "" : v)}
              options={(["any", "suggested", "none"] as const).map((a) => ({
                value: a,
                label: ADVICE_TEXT[a],
              }))}
            />
            {/* Each shows while it offers a choice, or while it filters (a
                value the queue no longer has stays, to clear). */}
            {(offered.types.length > 1 || type) && (
              <FilterSelect
                label="Type"
                value={type}
                onChange={(v) => setParam("type", v)}
                options={[
                  "",
                  ...new Set([...offered.types, ...(type ? [type] : [])]),
                ].map((t) => ({ value: t, label: t || "Any" }))}
              />
            )}
            {(offered.docs.length > 1 || doc) && (
              <FilterSelect
                label="Document"
                align="end"
                value={doc}
                onChange={(v) => setParam("doc", v)}
                options={[
                  "",
                  ...new Set([...offered.docs, ...(doc ? [doc] : [])]),
                ].map((d) => ({ value: d, label: d ? docName(d) : "Any" }))}
              />
            )}
            {filters > 0 && (
              <Button size="sm" variant="ghost" onClick={clearFilters}>
                Clear filters
              </Button>
            )}
          </div>
        </div>
        <p role="status" className="rv-progress">
          {formatNumber(p.total - p.left)} of {formatNumber(p.total)}
          {q.hasMore && "+"} · {formatNumber(p.accepted)} accepted ·{" "}
          {formatNumber(p.chosen)} changed · {formatNumber(p.skipped)} skipped
          {p.covered > 0 && ` · ${formatNumber(p.covered)} covered`}
          {elsewhere > 0 && ` · ${formatNumber(elsewhere)} decided elsewhere`}
          {takenBack > 0 && ` · ${formatNumber(takenBack)} taken back`}
        </p>
        {q.adviceCapped && (
          <p role="note" className="on-muted">
            {adviceFilter === "suggested"
              ? "Founder’s suggestions"
              : "Cards without a suggestion"}{" "}
            among the next {formatNumber(ADVICE_PAGES * 50)} cards. Clear the
            filter to walk the rest.
          </p>
        )}
        <div className="rv-meter" aria-hidden>
          <span
            className="rv-meter-done"
            style={{ flexGrow: p.total - p.left - p.skipped }}
          />
          <span className="rv-meter-skipped" style={{ flexGrow: p.skipped }} />
          <span className="rv-meter-left" style={{ flexGrow: p.left }} />
        </div>
      </div>
      {!synced ? (
        <SkeletonText lines={6} />
      ) : lane === "fyi" ? (
        <>
          <FyiCards
            keys={order.fyi}
            cards={order.cards}
            load={load}
            naming={naming}
            settled={() =>
              void qc.invalidateQueries({
                queryKey: [ws, "ontology", o, "review-cards", load.id],
              })
            }
          />
          {q.fyi.hasNextPage && (
            <Button
              size="sm"
              pending={q.fyi.isFetchingNextPage}
              onClick={() => void q.fyi.fetchNextPage()}
            >
              Show more
            </Button>
          )}
        </>
      ) : lane === "covered" ? (
        <CoveredCards
          keys={covered}
          cardOf={cardOf}
          ctx={ctx}
          coveredBy={coveredBy}
        />
      ) : (
        <div className={cx("rv-walk-body", !rail.length && "is-solo")}>
          {rail.length > 0 && (
            <nav className="rv-rail" aria-label="Next cards">
              <ol>
                {rail.map((r) => (
                  <li key={r.key}>
                    <button
                      type="button"
                      aria-current={r.key === key ? "step" : undefined}
                      title={cardLine(r.card)}
                      onClick={() => jump(r.key)}
                    >
                      <span
                        className="rv-dot"
                        data-state={r.state}
                        aria-hidden
                      />
                      <span className="rv-rail-text">
                        <span className="rv-rail-line">
                          {r.card.kind === "claim"
                            ? settleOf(r.card).statement.object.name
                            : r.card.kind === "name"
                              ? `“${cardName(r.card)}”`
                              : cardLine(r.card)}
                        </span>
                        <span className="rv-rail-meta">
                          {KIND_TEXT[r.card.kind]} ·{" "}
                          {docName(r.card.review.documentId)}
                        </span>
                      </span>
                    </button>
                  </li>
                ))}
              </ol>
            </nav>
          )}
          <div className="rv-walk-main">
            {card && advice ? (
              <ReviewCard
                key={card.key}
                card={card}
                advice={advice}
                choices={choices}
                layout={layout}
                effect={shownEffect}
                outcome={outcome}
                applied={
                  outcome?.kind === "saved"
                    ? appliedOf.get(outcome.decision.id)
                    : undefined
                }
                coveredBy={coveredBy(card.key)}
                readOnly={!manage}
                batch={batch?.items.length ?? 0}
                founder={shell.assistant.available}
                naming={naming}
                headingRef={heading}
                onCommand={on}
                onChoose={choose}
              />
            ) : (
              <WalkDone
                walk={w}
                made={api.made}
                notInPlace={notInPlace}
                member={!canManage(role)}
                empty={!w.order.length}
                indexing={q.indexing}
                onClearFilters={filters ? clearFilters : undefined}
                headingRef={heading}
                onBack={() => on({ kind: "back" })}
                onReopen={(keys) => {
                  moved.current = true;
                  api.dispatch({ type: "undone", keys });
                }}
                onJump={jump}
                spot={<SpotChecks queue={q} load={load} />}
              />
            )}
          </div>
        </div>
      )}
      {toast && (
        <div
          role="status"
          className={cx("hf-toast", "rv-toast", toast.error && "is-error")}
        >
          <span>{toast.text}</span>
          {toast.undo && w.undo.length > 0 && (
            <button
              type="button"
              className="rv-undo"
              onClick={() => void undo()}
            >
              Undo <kbd aria-hidden>Z</kbd>
            </button>
          )}
        </div>
      )}
      {dialog?.kind === "batch" && (
        <BatchDialog
          items={dialog.batch.items}
          cards={order.cards}
          advice={dialog.advice}
          label={dialog.label}
          drawn={drawnLike(q.spot.data?.draws ?? [], dialog.batch.batchKey)}
          run={async (keys, signal, onProgress) => {
            const chosen = new Set(keys);
            const items = dialog.batch.items.flatMap((it) => {
              const c = cardOf(it.key);
              return c && chosen.has(it.key)
                ? [
                    {
                      card: c,
                      choice: it.choice,
                      effect: effectFor(c, it.choice),
                    },
                  ]
                : [];
            });
            moved.current = true;
            if (dialog.advice.kind === "suggest")
              trackEvent("review_batch_accepted", {
                size_bucket: sizeBucket(items.length),
                rule: dialog.advice.rule,
              });
            const results = await api.batch(items, signal, onProgress);
            return results.map((r) => ({
              key: r.item.card.key,
              ...(r.kind === "stopped"
                ? { stopped: true }
                : r.kind === "failed"
                  ? { failed: r.error.message }
                  : r.value.kind === "refused"
                    ? { failed: r.value.message }
                    : {}),
            }));
          }}
          undo={async () => {
            const r = await api.undo();
            if (r) say(r.text, false, r.error);
          }}
          onClose={() => setDialog(null)}
        />
      )}
      {dialog?.kind === "help" && (
        <Dialog
          title="Keyboard shortcuts"
          size="sm"
          onClose={() => setDialog(null)}
        >
          <dl className="rv-keys">
            {SHORTCUTS.filter(
              ([, what, decides]) =>
                (manage || !decides) &&
                (shell.assistant.available || what !== ASK_FOUNDER),
            ).map(([keys, what]) => (
              <div key={what}>
                <dt>{keys}</dt>
                <dd>{what}</dd>
              </div>
            ))}
          </dl>
        </Dialog>
      )}
    </div>
  );
}

const ASK_FOUNDER = "Ask Founder";
/** The walk's keys, what each does, and whether it decides (owners and admins only). */
const SHORTCUTS: readonly (readonly [ReactNode, string, boolean])[] = [
  [<kbd>⏎</kbd>, "Accept Founder’s suggestion", true],
  [<kbd>1–9</kbd>, "Choose that option, saved at once", true],
  [<kbd>N</kbd>, "Not a record of that type we have", true],
  [<kbd>X</kbd>, "Reject", true],
  [
    <>
      <kbd>S</kbd> <kbd>→</kbd>
    </>,
    "Skip, or the next card",
    false,
  ],
  [<kbd>←</kbd>, "Back", false],
  [<kbd>Z</kbd>, "Undo the last choice", true],
  [<kbd>G</kbd>, "Accept all like this", true],
  [<kbd>O</kbd>, "Open the document", false],
  [<kbd>F</kbd>, ASK_FOUNDER, false],
  [<kbd>?</kbd>, "These shortcuts", false],
];

/**
 * A new load became current mid-walk: decisions saved before it started
 * were read by it, and the walk now follows it. The choices still sending
 * then are listed with what became of them (the backend refuses a choice
 * on a load that is no longer current).
 */
function LoadChanged({
  keys,
  walk,
  made,
  onDismiss,
}: {
  keys: readonly string[];
  walk: Walk;
  made: ReadonlyMap<string, { card: Decidable }>;
  onDismiss: () => void;
}) {
  return (
    <div role="status" className="rv-banner">
      <p>
        A new load is current. Choices saved before it started were read by it,
        and the walk now follows it.
        {keys.length > 0 &&
          ` ${plural(keys.length, "choice was", "choices were")} still being sent:`}
      </p>
      {keys.length > 0 && (
        <ul>
          {keys.map((k) => {
            const x = walk.outcomes.get(k),
              c = made.get(k)?.card;
            return (
              <li key={k}>
                {c ? cardLine(c) : "A card"}:{" "}
                {x?.kind === "saved"
                  ? "saved"
                  : x?.kind === "refused"
                    ? `refused (${x.message})`
                    : "sending"}
              </li>
            );
          })}
        </ul>
      )}
      <Button size="sm" variant="ghost" onClick={onDismiss}>
        Dismiss
      </Button>
    </div>
  );
}
