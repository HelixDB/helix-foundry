// The review walkthrough, the Facts tab's body (./index.tsx): the current
// load's cards one at a time, in the queue's order, each with Founder's
// suggestion (./advice.ts) and the choices a person settles it with, every
// one also a key (./keys.ts). A choice saves at once, through the same
// settle call a document's Needs review sends, a few at a time, and the walk
// moves on; Z takes the last choice (or batch) back. The walk (./walk.ts) is
// held by the page (useWalk), so it keeps its place across a switch of tabs
// and a read of the queue, and says so when a new load becomes current.
// Names the model linked and values a table keeps are for information (FYI),
// names an earlier link settles are Covered, and the random draws wait for
// the finish as spot checks.
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
import { paths } from "../../paths";
import { useShell } from "../../shell/context";
import { canManage, useWorkspace } from "../../ui";
import { useDocumentNames } from "../documents/documents-api";
import { ontologyQueries } from "../ontology/queries";
import { useOntology } from "../ontology/scope";
import {
  adviceContext,
  effectOf,
  founderSeed,
  settleOf,
  type Advice,
  type Decidable,
  type Effect,
} from "./advice";
import { deleteReviewDecision, reviewQueries, settleReview } from "./api";
import { BatchDialog } from "./batch-dialog";
import { choicesOf, type Choice } from "./choices";
import { CoveredCards, FyiCards } from "./fyi";
import type { ReviewQueue } from "./index";
import { useWalkKeys, type KeyCommand } from "./keys";
import { precision, type Queue, type QueueCard } from "./queue";
import { ReviewCard, cardLine } from "./review-card";
import { settleAll } from "./settle-pool";
import { SpotChecks } from "./spot-checks";
import {
  batchOf,
  current,
  progress,
  walkOrder,
  walkReducer,
  type How,
  type Walk,
  type WalkFilter,
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

/** What a toast says of a choice, by its kind. */
const CHOSEN: Record<Choice["kind"], string> = {
  link: "Linked",
  not_in_data: "Marked not in data",
  value: "Value chosen",
  approve: "Approved",
  reject: "Rejected",
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
  const undoing = useRef(false);
  // The choices still sending when a new load became current.
  const [changed, setChanged] = useState<readonly string[] | null>(null);
  const sending = [...state.outcomes.values()].some(
    (x) => x.kind === "sending",
  );
  useEffect(() => {
    if (!sending) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [sending]);
  const refresh = (all: boolean) =>
    Promise.all(
      [
        reviewQueries.list(ws, o).queryKey,
        ...(all
          ? [
              ontologyQueries.ontology(ws, o).queryKey,
              ontologyQueries.loads(ws, o).queryKey,
            ]
          : []),
      ].map((queryKey) => qc.invalidateQueries({ queryKey, exact: true })),
    );
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
    /** Sends a batch as one undo; what Stop kept from going is open again. */
    batch: async (
      items: readonly Sending[],
      signal: AbortSignal,
      onProgress: (done: number) => void,
    ) => {
      dispatch({
        type: "batch",
        items: items.map((s) => ({ key: s.card.key, choice: s.choice })),
      });
      const results = await settleAll(items, send, {
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
     * choice still sending first finishes) and forgets it. What it says, or
     * null with nothing to take back or an undo already running.
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
              out?.kind === "saved"
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
        if (gone.length) dispatch({ type: "undone", keys: gone });
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
     * Takes the queue's new walk order (its cards to decide; every card of
     * the queue, with Founder's advice): on another load, says which choices
     * were still sending; on the same load, cards gone from the queue keep
     * their places.
     */
    rebase: (
      loadId: string,
      { decide: order, cards: inQueue, advice }: WalkOrder,
    ) => {
      const w = now.current,
        moved = w.loadId !== "" && w.loadId !== loadId;
      if (moved) {
        setChanged(
          [...w.outcomes].flatMap(([k, x]) =>
            x.kind === "sending" ? [k] : [],
          ),
        );
        seen.current.clear();
      }
      for (const [k, card] of inQueue)
        seen.current.set(k, { card, advice: advice.get(k) });
      dispatch({
        type: "rebase",
        loadId,
        order,
        gone: new Set(
          w.loadId === loadId ? w.order.filter((k) => !inQueue.has(k)) : [],
        ),
      });
    },
  };
}

export type WalkApi = ReturnType<typeof useWalk>;

const KINDS = ["name", "claim", "value"] as const;
const KIND_TEXT: Record<(typeof KINDS)[number], string> = {
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
 * The Facts tab's walk of the queue (`cards`) of the current load: progress,
 * filters (in the URL: `kind`, `advice`, `type`, `doc`) and lanes (`lane`),
 * the rail of next cards, then the card shown, or the finish.
 */
export function ReviewWalkthrough({
  queue: q,
  load,
  cards: queue,
}: {
  queue: ReviewQueue;
  load: FoundryLoad;
  cards: Queue;
}) {
  const { id: ws, role, user } = useWorkspace(),
    { id: o } = useOntology();
  const shell = useShell();
  const docName = useDocumentNames(ws, "A document");
  const { naming, walk: api } = q;
  const w = api.state;
  const [params, setParams] = useSearchParams();
  const kindParam = params.get("kind") ?? "",
    adviceParam = params.get("advice") ?? "",
    type = params.get("type") ?? "",
    doc = params.get("doc") ?? "";
  const laneParam = params.get("lane");
  const lane: Lane =
    laneParam === "fyi" || laneParam === "covered" ? laneParam : "decide";
  const filter = useMemo((): WalkFilter => {
    const asked = KINDS.filter((k) => kindParam.split(",").includes(k));
    return {
      kinds: new Set(asked.length ? asked : KINDS),
      advice:
        adviceParam === "suggested" || adviceParam === "none"
          ? adviceParam
          : "any",
      ...(type ? { type } : {}),
      ...(doc ? { documentId: doc } : {}),
    };
  }, [kindParam, adviceParam, type, doc]);
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
  const single = useMemo(
    () => (link: string) => !naming.version || naming.oneValued.has(link),
    [naming],
  );
  const ctx = useMemo(
    () =>
      adviceContext({
        decisions: q.decisions.data ?? [],
        precision: precision(q.labels.data ?? []),
        single,
        user: user.id,
      }),
    [q.decisions.data, q.labels.data, single, user.id],
  );
  const order = useMemo(
    () => walkOrder(queue, ctx, filter),
    [queue, ctx, filter],
  );
  useEffect(() => {
    api.rebase(load.id, order);
  }, [load.id, order]);
  // The types and documents the filters offer: every card's to decide.
  const offered = useMemo(() => {
    const types = new Set<string>(),
      docs = new Set<string>();
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
  }, [order]);

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
    !outcome || outcome.kind === "refused" || outcome.kind === "skipped";
  const siblings = (card && order.siblings.get(card.key)) ?? [];
  const effectFor = (c: Decidable, choice: FoundryReviewChoice) =>
    effectOf(c, choice, (order.siblings.get(c.key) ?? []).length, single);
  const suggestion =
    advice?.kind === "suggest" &&
    choices.some((c) => same(c.decision, advice.choice))
      ? advice.choice
      : undefined;
  const shownEffect = !card
    ? null
    : suggestion
      ? effectFor(card, suggestion)
      : choices[0]
        ? effectFor(card, choices[0].decision)
        : null;
  const batch = useMemo(() => {
    if (!key || !manage) return null;
    const openKeys = w.order.filter((k) => {
      const x = w.outcomes.get(k);
      return !x || x.kind === "refused";
    });
    return batchOf(openKeys, order.advice, key);
  }, [key, manage, w, order.advice]);
  const p = progress(w);

  const [dialog, setDialog] = useState<
    | { kind: "help" }
    | { kind: "batch"; batch: NonNullable<typeof batch>; advice: Advice }
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
  const heading = useRef<HTMLHeadingElement>(null);
  const moved = useRef(false);
  useEffect(() => {
    if (!moved.current) return;
    moved.current = false;
    heading.current?.focus();
  }, [key, synced]);

  const undo = async () => {
    const r = await api.undo();
    if (!r) return;
    moved.current = true;
    say(r.text, false, r.error);
  };
  const decide = (choice: FoundryReviewChoice, how: How) => {
    if (!card || !manage || !open) return;
    const covers = choice.resolve?.entityKey ? siblings : [];
    q.keepCards([card.key]);
    api.decide({ card, choice, how, covers, effect: effectFor(card, choice) });
    moved.current = true;
    const kind = choices.find((c) => same(c.decision, choice))?.kind;
    say(
      `${kind ? CHOSEN[kind] : "Saved"}${covers.length ? `, with ${plural(covers.length, "other document")} asking about this name` : ""}.`,
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
        return choose(choices[c.index]);
      case "not_in_data":
      case "reject":
        return choose(choices.find((x) => x.kind === c.kind));
      case "skip": {
        moved.current = true;
        const next = w.order[w.at + 1];
        // A member only reads: the next card, marking nothing skipped.
        if (manage) api.dispatch({ type: "skip" });
        else if (next !== undefined) api.dispatch({ type: "jump", key: next });
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
        if (batch && advice) setDialog({ kind: "batch", batch, advice });
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
  useWalkKeys(
    on,
    manage && synced && lane === "decide" && !dialog,
    choices.length,
  );

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
  const elsewhere = [...w.outcomes.values()].filter(
    (x) => x.kind === "elsewhere",
  ).length;
  const start = Math.max(0, w.at - 2);
  const rail = w.order.slice(start, start + RAIL).flatMap((k) => {
    const c = cardOf(k);
    return c
      ? [{ key: k, card: c, state: w.outcomes.get(k)?.kind ?? "open" }]
      : [];
  });
  const jump = (k: string) => {
    moved.current = true;
    api.dispatch({ type: "jump", key: k });
  };
  const lanes: { value: Lane; label: string; count: number }[] = [
    { value: "decide", label: "Decide", count: p.left },
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
        <p role="status" className="rv-progress">
          {formatNumber(p.total - p.left)} of {formatNumber(p.total)} ·{" "}
          {formatNumber(p.accepted)} accepted · {formatNumber(p.chosen)} changed
          · {formatNumber(p.skipped)} skipped
          {elsewhere > 0 && ` · ${formatNumber(elsewhere)} decided elsewhere`}
        </p>
        <div className="rv-meter" aria-hidden>
          <span
            className="rv-meter-done"
            style={{ flexGrow: p.total - p.left - p.skipped }}
          />
          <span className="rv-meter-skipped" style={{ flexGrow: p.skipped }} />
          <span className="rv-meter-left" style={{ flexGrow: p.left }} />
        </div>
        <div className="rv-filters">
          <div className="rv-chips" role="group" aria-label="Kinds">
            {KINDS.map((k) => {
              const pressed = filter.kinds.has(k);
              const next = KINDS.filter((x) =>
                x === k ? !pressed : filter.kinds.has(x),
              );
              return (
                <button
                  key={k}
                  type="button"
                  className="rv-chip"
                  aria-pressed={pressed}
                  // At least one kind stays on.
                  disabled={pressed && filter.kinds.size === 1}
                  onClick={() =>
                    setParam(
                      "kind",
                      next.length === KINDS.length ? "" : next.join(","),
                    )
                  }
                >
                  {KIND_TEXT[k]}
                </button>
              );
            })}
          </div>
          <Segmented
            label="Founder’s suggestion"
            value={filter.advice}
            onChange={(v) => setParam("advice", v === "any" ? "" : v)}
            options={[
              { value: "any", label: "All" },
              { value: "suggested", label: "Suggested" },
              { value: "none", label: "Needs you" },
            ]}
          />
          {offered.types.length > 1 && (
            <FilterSelect
              label="Type"
              value={type}
              onChange={(v) => setParam("type", v)}
              options={[
                { value: "", label: "Any type" },
                ...offered.types.map((t) => ({ value: t, label: t })),
              ]}
            />
          )}
          {offered.docs.length > 1 && (
            <FilterSelect
              label="Document"
              value={doc}
              onChange={(v) => setParam("doc", v)}
              options={[
                { value: "", label: "Any document" },
                ...offered.docs.map((d) => ({ value: d, label: docName(d) })),
              ]}
            />
          )}
        </div>
        <Segmented
          label="Lane"
          value={lane}
          onChange={(v) => setParam("lane", v === "decide" ? "" : v)}
          options={lanes}
        />
      </div>
      {!synced ? (
        <SkeletonText lines={6} />
      ) : lane === "fyi" ? (
        <FyiCards
          keys={order.fyi}
          cards={order.cards}
          load={load}
          naming={naming}
          settled={q.settled}
        />
      ) : lane === "covered" ? (
        <CoveredCards
          keys={covered}
          cardOf={cardOf}
          ctx={ctx}
          coveredBy={coveredBy}
        />
      ) : (
        <div className="rv-walk-body">
          {rail.length > 0 && (
            <nav className="rv-rail" aria-label="Next cards">
              <ol>
                {rail.map((r) => (
                  <li key={r.key}>
                    <button
                      type="button"
                      aria-current={r.key === key ? "step" : undefined}
                      onClick={() => jump(r.key)}
                    >
                      <span
                        className="rv-dot"
                        data-state={r.state}
                        aria-hidden
                      />
                      <span className="rv-rail-line">{cardLine(r.card)}</span>
                    </button>
                  </li>
                ))}
              </ol>
            </nav>
          )}
          <div className="rv-walk-main">
            {rail.length > 1 && (
              <div className="rv-rail-select">
                <FilterSelect
                  label="Card"
                  value={key ?? ""}
                  onChange={jump}
                  options={rail.map((r) => ({
                    value: r.key,
                    label: cardLine(r.card),
                  }))}
                />
              </div>
            )}
            {card && advice ? (
              <ReviewCard
                key={card.key}
                card={card}
                advice={advice}
                choices={choices}
                effect={shownEffect}
                outcome={outcome}
                coveredBy={coveredBy(card.key)}
                readOnly={!manage}
                batch={batch?.items.length ?? 0}
                founder={shell.assistant.available}
                naming={naming}
                headingRef={heading}
                onCommand={on}
              />
            ) : (
              <WalkDone
                walk={w}
                made={api.made}
                empty={!w.order.length}
                headingRef={heading}
                onBack={() => on({ kind: "back" })}
                onReopen={(keys) => {
                  moved.current = true;
                  api.dispatch({ type: "undone", keys });
                }}
                onJump={jump}
                spot={
                  <SpotChecks
                    queue={q}
                    load={load}
                    keys={order.spot}
                    cards={order.cards}
                  />
                }
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
            q.keepCards(items.map((s) => s.card.key));
            moved.current = true;
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
            {SHORTCUTS.map(([keys, what]) => (
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

const SHORTCUTS: readonly (readonly [ReactNode, string])[] = [
  [<kbd>⏎</kbd>, "Accept Founder’s suggestion"],
  [<kbd>1–9</kbd>, "Choose that option, saved at once"],
  [<kbd>N</kbd>, "Not in data"],
  [<kbd>X</kbd>, "Reject"],
  [
    <>
      <kbd>S</kbd> <kbd>→</kbd>
    </>,
    "Skip",
  ],
  [<kbd>←</kbd>, "Back"],
  [<kbd>Z</kbd>, "Undo the last choice"],
  [<kbd>G</kbd>, "Accept all like this"],
  [<kbd>O</kbd>, "Open the document"],
  [<kbd>F</kbd>, "Ask Founder"],
  [<kbd>?</kbd>, "These shortcuts"],
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
