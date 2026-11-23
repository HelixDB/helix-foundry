import { describe, expect, it } from "vitest";
import type {
  DecisionApplied,
  FoundryReviewChoice,
  FoundryReviewDecision,
  FoundryReviewDraw,
} from "@/shared/wfe";
import type { Advice } from "@/web/areas/reviews/advice";
import {
  batchOf,
  batchReason,
  batchTakes,
  changedElsewhere,
  current,
  drawnLike,
  linkedNow,
  notLinked,
  progress,
  unconfirmedNames,
  undecided,
  walkOrder,
  walkReducer,
  type How,
  type Outcome,
  type Walk,
  type WalkAction,
} from "@/web/areas/reviews/walk";
import {
  ctxOf,
  entity,
  heldCard,
  nameCard,
  resolveDecision,
  valueCard,
} from "./__fixtures__/review-cards";

// The review walkthrough's state (reviews/walk.ts): its lanes, the reducer
// over every action, progress and batches.

describe("walkOrder", () => {
  const umbrella = { key: "scus_1", entity: entity("scus_1", "Umbrella") };
  const waiting = heldCard(5, { named: false });
  const asking = nameCard("umbrella", {
    candidates: [umbrella],
    held: [waiting],
  });
  const askingToo = nameCard("umbrella", { doc: "doc-2" });
  const bound = nameCard("umbrella", { doc: "doc-3", reasons: ["part_name"] });
  const covered = nameCard("initech", {
    doc: "doc-4",
    candidates: [{ key: "scus_7", entity: entity("scus_7", "Initech") }],
  });
  const model = nameCard("globex", { reasons: ["adjudicated"] });
  const claim = heldCard(1, { doc: "doc-5", subjectType: "Person" });
  const value = valueCard([{ text: "a" }, { text: "a" }, { text: "a" }]);
  const table = valueCard([{ text: "a" }], { table: "b", id: "v-2" });
  // The decide lane's pages, then the FYI lane's, as the index lists them.
  const queue = [asking, claim, askingToo, bound, covered, value, model, table];
  const ctx = ctxOf({
    decisions: [
      resolveDecision("d-1", {
        doc: "doc-9",
        normName: "initech",
        entityKey: "scus_7",
        applied: "pending",
      }),
    ],
  });
  it("puts each card in its lane, in the index's order", () => {
    const o = walkOrder(queue, ctx, "any");
    expect(o.decide).toEqual([
      asking.key,
      waiting.key,
      claim.key,
      askingToo.key,
      bound.key,
      value.key,
    ]);
    expect(o.fyi).toEqual([model.key, table.key]);
    expect(o.covered).toEqual([covered.key]);
    // Every card, a held claim of a name too, by key.
    expect(o.cards.get(waiting.key)).toBe(waiting);
    // A card a page read again lists once.
    expect(walkOrder([...queue, claim], ctx, "any").decide).toEqual(o.decide);
  });
  it("advises the cards", () => {
    const o = walkOrder(queue, ctx, "any");
    expect(o.advice.get(asking.key)).toMatchObject({
      kind: "suggest",
      rule: "single_candidate",
    });
    expect(o.advice.get(waiting.key)).toEqual({
      kind: "none",
      why: "waits_on_name",
    });
    expect(o.advice.get(model.key)).toEqual({ kind: "none", why: "fyi" });
  });
  it("pairs the names the load asks about in other documents", () => {
    const o = walkOrder(queue, ctx, "any");
    expect(o.siblings.get(asking.key)).toEqual([askingToo.key]);
    expect(o.siblings.get(askingToo.key)).toEqual([asking.key]);
    expect(o.siblings.has(bound.key)).toBe(false);
    expect(o.siblings.has(covered.key)).toBe(false);
  });
  it("takes the cards to decide Founder has a suggestion for, or none for", () => {
    expect(walkOrder(queue, ctx, "suggested").decide).toEqual([
      asking.key,
      value.key,
    ]);
    expect(walkOrder(queue, ctx, "none").decide).toEqual([
      waiting.key,
      claim.key,
      askingToo.key,
      bound.key,
    ]);
    // Neither filter narrows the other lanes.
    expect(walkOrder(queue, ctx, "none").fyi).toEqual([model.key, table.key]);
  });
});

const approve: FoundryReviewChoice = { release: { statementHash: "h" } };
const decision = (id: string): FoundryReviewDecision => ({
  id,
  kind: "release",
  type: "",
  docId: "doc-1",
  claimKey: id,
  claimSection: 0,
  claimStart: 0,
  claimEnd: 1,
  statementHash: "h",
});
const walkOf = (
  order: string[],
  more: Partial<Walk> = {},
  outcomes: [string, Outcome][] = [],
): Walk => ({
  loadId: "L",
  order,
  at: 0,
  outcomes: new Map(outcomes),
  undo: [],
  ...more,
});
const run = (w: Walk, ...actions: WalkAction[]) =>
  actions.reduce(walkReducer, w);
const decide = (key: string, how: How = "accepted"): WalkAction => ({
  type: "decide",
  key,
  choice: approve,
  how,
});

describe("walkReducer", () => {
  it("decides a card and goes to the next to decide", () => {
    const w = run(walkOf(["a", "b", "c"]), decide("a"));
    expect(current(w)).toBe("b");
    expect(w.outcomes.get("a")).toEqual({
      kind: "sending",
      choice: approve,
      how: "accepted",
    });
    expect(w.undo).toEqual([["a"]]);
    // Past cards already decided.
    const past = run(
      walkOf(["a", "b", "c"], {}, [["b", { kind: "skipped" }]]),
      decide("a"),
    );
    expect(current(past)).toBe("c");
  });
  it("covers the cards a name's link covers, and uncovers them on undo", () => {
    const w = run(
      walkOf(["a", "b", "c", "d"], {}, [
        ["c", { kind: "skipped" }],
        ["d", { kind: "saved", decision: decision("d"), how: "chosen" }],
      ]),
      {
        type: "decide",
        key: "a",
        choice: approve,
        how: "chosen",
        covers: ["b", "c", "d"],
      },
    );
    expect(w.outcomes.get("b")).toEqual({ kind: "covered", by: "a" });
    expect(w.outcomes.get("c")).toEqual({ kind: "covered", by: "a" });
    // A card decided keeps its decision.
    expect(w.outcomes.get("d")?.kind).toBe("saved");
    expect(current(w)).toBeNull();
    const undone = run(w, { type: "undone", keys: ["a"] });
    expect([...undone.outcomes.keys()]).toEqual(["d"]);
    expect(current(undone)).toBe("a");
    expect(undone.undo).toEqual([]);
  });
  it("opens again what a link covered once the load refuses it in place", () => {
    const w = run(walkOf(["a", "b", "c", "d"]), {
      type: "decide",
      key: "a",
      choice: approve,
      how: "chosen",
      covers: ["b", "c"],
    });
    const uncovered = run(w, { type: "uncover", by: new Set(["a"]) });
    expect(uncovered.outcomes.has("b")).toBe(false);
    expect(uncovered.outcomes.has("c")).toBe(false);
    expect(uncovered.outcomes.get("a")?.kind).toBe("sending");
    expect(progress(uncovered).left).toBe(3);
    // Nothing more to open: the walk itself.
    expect(run(uncovered, { type: "uncover", by: new Set(["a"]) })).toBe(
      uncovered,
    );
    expect(run(w, { type: "uncover", by: new Set(["z"]) })).toBe(w);
  });
  it("ignores a choice on a card still sending, or not in the walk", () => {
    const w = run(walkOf(["a", "b"]), decide("a"));
    expect(run(w, decide("a"))).toBe(w);
    expect(run(w, decide("z"))).toBe(w);
  });
  it("decides a card again once its choice landed", () => {
    const w = run(
      walkOf(["a", "b"]),
      decide("a"),
      { type: "sent", key: "a", decision: decision("a") },
      { type: "back" },
      decide("a", "chosen"),
    );
    expect(w.outcomes.get("a")).toMatchObject({
      kind: "sending",
      how: "chosen",
    });
    expect(w.undo).toEqual([["a"], ["a"]]);
  });
  it("sends a batch as one undo, skipping cards still sending or not in the walk", () => {
    const w = run(walkOf(["a", "b", "c", "d"]), decide("b"), {
      type: "jump",
      key: "a",
    });
    const batched = run(w, {
      type: "batch",
      items: [
        { key: "a", choice: approve },
        { key: "b", choice: approve },
        { key: "c", choice: approve },
        { key: "z", choice: approve },
      ],
    });
    expect(batched.outcomes.get("a")).toEqual({
      kind: "sending",
      choice: approve,
      how: "batch",
    });
    expect(batched.outcomes.get("b")).toMatchObject({
      kind: "sending",
      how: "accepted",
    });
    expect(batched.undo).toEqual([["b"], ["a", "c"]]);
    expect(current(batched)).toBe("d");
    expect(
      run(batched, { type: "batch", items: [{ key: "a", choice: approve }] }),
    ).toBe(batched);
    // Saved, covered or decided elsewhere meanwhile: left as it is; a card
    // skipped is taken.
    const meanwhile = walkOf(["a", "b", "c", "d"], {}, [
      ["a", { kind: "saved", decision: decision("a"), how: "chosen" }],
      ["b", { kind: "covered", by: "x" }],
      ["c", { kind: "elsewhere" }],
      ["d", { kind: "skipped" }],
    ]);
    expect(
      ["a", "b", "c", "d", "z"].filter((k) => batchTakes(meanwhile, k)),
    ).toEqual(["d"]);
    const some = run(meanwhile, {
      type: "batch",
      items: ["a", "b", "c", "d"].map((key) => ({ key, choice: approve })),
    });
    expect(some.undo).toEqual([["d"]]);
    expect(some.outcomes.get("c")).toEqual({ kind: "elsewhere" });
    const undone = run(batched, { type: "undone", keys: ["a", "c"] });
    expect(undone.undo).toEqual([["b"]]);
    expect(current(undone)).toBe("a");
    expect(undone.outcomes.has("a")).toBe(false);
    expect(undone.outcomes.has("b")).toBe(true);
  });
  it("records what the backend answered, only for a card still sending", () => {
    const w = run(walkOf(["a", "b"]), decide("a"));
    const sent = run(w, { type: "sent", key: "a", decision: decision("a") });
    expect(sent.outcomes.get("a")).toEqual({
      kind: "saved",
      decision: decision("a"),
      how: "accepted",
    });
    expect(run(sent, { type: "sent", key: "a", decision: decision("x") })).toBe(
      sent,
    );
    expect(
      run(sent, { type: "refused", key: "a", message: "m", retry: true }),
    ).toBe(sent);
    expect(run(w, { type: "sent", key: "b", decision: decision("b") })).toBe(w);
  });
  it("keeps another person's decision when theirs came first, uncovering what the choice covered", () => {
    const w = run(walkOf(["a", "b", "c"]), {
      type: "decide",
      key: "a",
      choice: approve,
      how: "chosen",
      covers: ["b"],
    });
    const theirs = { ...decision("t"), decidedByUserId: "alan" };
    const decided = run(w, {
      type: "decided_elsewhere",
      key: "a",
      decision: theirs,
    });
    expect(decided.outcomes.get("a")).toEqual({
      kind: "elsewhere",
      decision: theirs,
    });
    expect(decided.outcomes.has("b")).toBe(false);
    expect(decided.undo).toEqual([]);
    expect(progress(decided).left).toBe(2);
    // Only a choice still sending.
    expect(run(decided, { type: "decided_elsewhere", key: "a" })).toBe(decided);
  });
  it("adopts the person's decision saved elsewhere on an open card, as one undo", () => {
    const mine = { ...decision("m"), decidedByUserId: "ada" };
    const w = run(walkOf(["a", "b", "c"]), {
      type: "adopt",
      key: "a",
      decision: mine,
    });
    expect(w.outcomes.get("a")).toEqual({
      kind: "saved",
      decision: mine,
      how: "chosen",
    });
    expect(current(w)).toBe("b");
    expect(w.undo).toEqual([["a"]]);
    // Not the card shown: the walk stays where it is.
    const other = run(walkOf(["a", "b", "c"]), {
      type: "adopt",
      key: "c",
      decision: mine,
    });
    expect(current(other)).toBe("a");
    // A skipped card is still open; a decided one, or one not in the walk, isn't.
    const skipped = run(
      walkOf(["a", "b"]),
      { type: "skip" },
      { type: "adopt", key: "a", decision: mine },
    );
    expect(skipped.outcomes.get("a")?.kind).toBe("saved");
    expect(run(w, { type: "adopt", key: "a", decision: mine })).toBe(w);
    expect(run(w, { type: "adopt", key: "z", decision: mine })).toBe(w);
    // Undo takes it back like any choice.
    const undone = run(w, { type: "undone", keys: ["a"] });
    expect(undone.outcomes.has("a")).toBe(false);
    expect(current(undone)).toBe("a");
  });
  it("keeps a refusal not to retry in place, uncovering what it covered", () => {
    const w = run(
      walkOf(["a", "b", "c"]),
      {
        type: "decide",
        key: "a",
        choice: approve,
        how: "accepted",
        covers: ["b"],
      },
      { type: "refused", key: "a", message: "no", retry: false },
    );
    expect(w.order).toEqual(["a", "b", "c"]);
    expect(w.outcomes.get("a")).toEqual({
      kind: "refused",
      choice: approve,
      how: "accepted",
      message: "no",
      retry: false,
    });
    expect(w.outcomes.has("b")).toBe(false);
    // Still to decide: the walk goes back to it.
    expect(progress(w).left).toBe(3);
  });
  it("puts a refusal to retry right after the card shown", () => {
    const w = run(walkOf(["a", "b", "c", "d"]), decide("a"), decide("b"));
    expect(current(w)).toBe("c");
    const r = run(w, {
      type: "refused",
      key: "a",
      message: "reads differently now",
      retry: true,
    });
    expect(r.order).toEqual(["b", "c", "a", "d"]);
    expect(current(r)).toBe("c");
    // Then it is next.
    expect(current(run(r, { type: "skip" }))).toBe("a");
  });
  it("keeps a refusal to retry shown when it is the card shown", () => {
    const w = run(walkOf(["a", "b", "c"]), decide("b"), {
      type: "jump",
      key: "b",
    });
    const r = run(w, { type: "refused", key: "b", message: "m", retry: true });
    expect(r.order).toEqual(["a", "b", "c"]);
    expect(current(r)).toBe("b");
  });
  it("shows a refusal to retry last once the walk is past its end", () => {
    const w = run(walkOf(["a", "b"]), decide("a"), decide("b"));
    expect(current(w)).toBeNull();
    const r = run(w, { type: "refused", key: "a", message: "m", retry: true });
    expect(r.order).toEqual(["b", "a"]);
    expect(current(r)).toBe("a");
  });
  it("skips, and does nothing past the end", () => {
    const w = run(walkOf(["a", "b"]), { type: "skip" });
    expect(w.outcomes.get("a")).toEqual({ kind: "skipped" });
    expect(current(w)).toBe("b");
    const end = run(w, { type: "skip" });
    expect(current(end)).toBeNull();
    expect(run(end, { type: "skip" })).toBe(end);
  });
  it("skips past a decided card without forgetting its decision", () => {
    const w = run(
      walkOf(["a", "b"]),
      decide("a"),
      { type: "back" },
      { type: "skip" },
    );
    expect(w.outcomes.get("a")?.kind).toBe("sending");
    expect(current(w)).toBe("b");
  });
  it("goes back, not before the first card", () => {
    const w = walkOf(["a", "b"], { at: 1 });
    expect(current(run(w, { type: "back" }))).toBe("a");
    const first = walkOf(["a", "b"]);
    expect(run(first, { type: "back" })).toBe(first);
  });
  it("jumps to a card of the walk", () => {
    const w = walkOf(["a", "b", "c"]);
    expect(current(run(w, { type: "jump", key: "c" }))).toBe("c");
    expect(run(w, { type: "jump", key: "a" })).toBe(w);
    expect(run(w, { type: "jump", key: "z" })).toBe(w);
  });
  it("keeps a link the current load holds taken back, with what it covered, on undo", () => {
    const w = run(walkOf(["a", "b", "c", "d"]), {
      type: "decide",
      key: "a",
      choice: approve,
      how: "accepted",
      covers: ["b", "c"],
    });
    const undone = run(w, {
      type: "undone",
      keys: ["a"],
      kept: new Set(["a"]),
    });
    for (const k of ["a", "b", "c"])
      expect(undone.outcomes.get(k)).toEqual({ kind: "taken_back" });
    expect(current(undone)).toBe("a");
    expect(undone.undo).toEqual([]);
    // Not to decide, so not counted, and the walk passes over them.
    expect(undecided(undone.order, undone.outcomes)).toBe(1);
    expect(progress(undone).left).toBe(1);
    expect(current(run(undone, { type: "skip" }))).toBe("d");
    // Gone from the queue once the load's job ran: still this walk's own.
    const read = run(undone, {
      type: "rebase",
      loadId: "L",
      order: ["d"],
      gone: new Set(["a", "b", "c"]),
    });
    for (const k of ["a", "b", "c"])
      expect(read.outcomes.get(k)).toEqual({ kind: "taken_back" });
    // Still in the queue meanwhile: still taken back.
    const still = run(undone, {
      type: "rebase",
      loadId: "L",
      order: ["a", "b", "c", "d"],
      gone: new Set(),
    });
    expect(still.outcomes.get("b")).toEqual({ kind: "taken_back" });
    // The next load asks about the name again.
    const next = run(undone, {
      type: "rebase",
      loadId: "L2",
      order: ["a", "b", "c", "d"],
      gone: new Set(),
    });
    expect(next.outcomes.size).toBe(0);
  });
  it("moves to the next card, then past the last, marking nothing", () => {
    const w = run(walkOf(["a", "b"]), { type: "next" });
    expect(current(w)).toBe("b");
    const end = run(w, { type: "next" });
    expect(current(end)).toBeNull();
    expect(end.at).toBe(2);
    expect(end.outcomes.size).toBe(0);
    expect(run(end, { type: "next" })).toBe(end);
  });
  it("forgets undone keys it never had", () => {
    const w = walkOf(["a", "b"], { at: 1 });
    expect(current(run(w, { type: "undone", keys: ["z"] }))).toBe("b");
  });
  it("rebases on the same load keeping every outcome, and gone cards in place", () => {
    const w = run(walkOf(["a", "b", "c", "e"]), decide("a"), { type: "skip" });
    expect(current(w)).toBe("c");
    const r = run(w, {
      type: "rebase",
      loadId: "L",
      order: ["c", "d"],
      // e left the walk (a filter) but not the queue.
      gone: new Set(["a", "b", "x"]),
    });
    expect(r.order).toEqual(["a", "b", "c", "d"]);
    expect(current(r)).toBe("c");
    // Decided here, then gone from the queue: still this walk's.
    expect(r.outcomes.get("a")?.kind).toBe("sending");
    // Skipped, then decided by someone else.
    expect(r.outcomes.get("b")).toEqual({ kind: "elsewhere" });
    expect(r.outcomes.has("x")).toBe(false);
    expect(r.undo).toEqual([["a"]]);
  });
  it("keeps a gone card after the card before it, shown when it was", () => {
    const w = walkOf(["a", "b", "c"], { at: 1 });
    const r = run(w, {
      type: "rebase",
      loadId: "L",
      order: ["c", "a"],
      gone: new Set(["b"]),
    });
    expect(r.order).toEqual(["c", "a", "b"]);
    expect(current(r)).toBe("b");
    expect(r.outcomes.get("b")).toEqual({ kind: "elsewhere" });
  });
  it("rebases on a new load keeping what is in its order or still sending", () => {
    const w = run(
      walkOf(["a", "b", "c", "d"]),
      decide("a"),
      { type: "sent", key: "a", decision: decision("a") },
      decide("b"),
      { type: "skip" },
    );
    const r = run(w, {
      type: "rebase",
      loadId: "L2",
      order: ["x", "a", "y"],
      // Another load's queue: nothing of the old order stays for being gone.
      gone: new Set(["c"]),
    });
    expect(r.loadId).toBe("L2");
    // Saved here, so not someone else's; b is still sending.
    expect(r.outcomes.get("a")?.kind).toBe("saved");
    expect(r.outcomes.get("b")?.kind).toBe("sending");
    expect(r.outcomes.has("c")).toBe(false);
    // Nothing of the old order from the card shown on: the first to decide.
    expect(current(r)).toBe("x");
    expect(r.undo).toEqual([["a"], ["b"]]);
    // The answer to a choice no longer in the order still lands, in place.
    const refused = run(r, {
      type: "refused",
      key: "b",
      message: "load changed",
      retry: true,
    });
    expect(refused.order).toEqual(["x", "a", "y"]);
    expect(refused.outcomes.get("b")?.kind).toBe("refused");
  });
  it("opens a card decided elsewhere again once it is back in the queue", () => {
    const w = walkOf(["a", "b", "c"], { at: 2 });
    const gone = run(w, {
      type: "rebase",
      loadId: "L",
      order: ["a", "c"],
      gone: new Set(["b"]),
    });
    expect(gone.outcomes.get("b")).toEqual({ kind: "elsewhere" });
    expect(undecided(gone.order, gone.outcomes)).toBe(2);
    // The other person's decision was removed, or no longer settles it.
    const back = run(gone, {
      type: "rebase",
      loadId: "L",
      order: ["a", "b", "c"],
      gone: new Set(),
    });
    expect(back.outcomes.has("b")).toBe(false);
    expect(back.order).toEqual(["a", "b", "c"]);
    expect(current(back)).toBe("c");
    expect(undecided(back.order, back.outcomes)).toBe(3);
    // Choices show again: deciding it sends.
    const decided = run(back, { type: "jump", key: "b" }, decide("b"));
    expect(decided.outcomes.get("b")?.kind).toBe("sending");
  });
  it("rebases onto the next card of the old order still in the new", () => {
    const w = walkOf(["a", "b", "c"], { at: 1 });
    const r = run(w, {
      type: "rebase",
      loadId: "L2",
      order: ["a", "c"],
      gone: new Set(),
    });
    expect(current(r)).toBe("c");
  });
});

describe("progress", () => {
  it("counts accepted, chosen, skipped and left, over the order", () => {
    const w = walkOf(["a", "b", "c", "d", "e", "f", "g"], {}, [
      ["a", { kind: "sending", choice: approve, how: "accepted" }],
      ["b", { kind: "saved", decision: decision("b"), how: "batch" }],
      ["c", { kind: "saved", decision: decision("c"), how: "chosen" }],
      ["d", { kind: "skipped" }],
      ["e", { kind: "covered", by: "a" }],
      [
        "f",
        {
          kind: "refused",
          choice: approve,
          how: "chosen",
          message: "m",
          retry: true,
        },
      ],
      ["z", { kind: "skipped" }],
    ]);
    expect(progress(w)).toEqual({
      total: 7,
      accepted: 2,
      chosen: 1,
      skipped: 1,
      covered: 1,
      left: 2,
    });
  });
});

describe("undecided", () => {
  it("counts the cards no choice settles yet: skipped and refused ones too, not covered or elsewhere", () => {
    const outcomes = new Map<string, Outcome>([
      ["a", { kind: "sending", choice: approve, how: "accepted" }],
      ["b", { kind: "saved", decision: decision("b"), how: "batch" }],
      ["c", { kind: "skipped" }],
      [
        "d",
        {
          kind: "refused",
          choice: approve,
          how: "chosen",
          message: "m",
          retry: true,
        },
      ],
      ["e", { kind: "covered", by: "a" }],
      ["f", { kind: "elsewhere" }],
    ]);
    expect(undecided(["a", "b", "c", "d", "e", "f", "g"], outcomes)).toBe(3);
    expect(undecided([], outcomes)).toBe(0);
    expect(undecided(["g", "h"], new Map())).toBe(2);
  });
  it("is the Facts tab's count, the walk's Decide lane without a filter", () => {
    const cards = [heldCard(1), heldCard(2), nameCard("acme")];
    const order = walkOrder(cards, ctxOf(), "any");
    const w = walkOf(order.decide, {}, [
      [
        "held-1",
        { kind: "saved", decision: decision("held-1"), how: "accepted" },
      ],
    ]);
    expect(undecided(order.decide, w.outcomes)).toBe(2);
    expect(undecided(w.order, w.outcomes)).toBe(2);
  });
});

describe("drawnLike", () => {
  const draw = (
    seq: number,
    band: string,
    { link = "partner_of", done = false, quote = false } = {},
  ): FoundryReviewDraw => ({
    seq: String(seq),
    band,
    docId: "doc-1",
    link,
    done,
    ...(quote
      ? { quote: { factKey: "f" } }
      : { held: { claimKey: "k", statementHash: "h", reviewKey: "r" } }),
  });
  it("counts the undone held claims drawn that a batch of the same link and band would take", () => {
    const draws = [
      draw(0, "0.7"),
      draw(1, "0.9"),
      draw(2, "0.7", { done: true }),
      draw(3, "0.7", { quote: true }),
      draw(4, "0.7", { link: "signed_with" }),
    ];
    expect(drawnLike(draws, 'claim:["partner_of","0.7"]')).toBe(1);
    expect(drawnLike(draws, 'claim:["partner_of","0.9"]')).toBe(1);
    expect(drawnLike(draws, 'claim:["signed_with","0.7"]')).toBe(1);
    expect(drawnLike(draws, 'claim:["signed_with","0.9"]')).toBe(0);
    expect(drawnLike([], 'claim:["partner_of","0.7"]')).toBe(0);
  });
});

describe("extend", () => {
  it("adds a page's new keys after the order, the card shown staying", () => {
    const w = walkOf(["a", "b"], { at: 1 });
    const next = walkReducer(w, {
      type: "extend",
      loadId: w.loadId,
      keys: ["b", "c", "d"],
    });
    expect(next.order).toEqual(["a", "b", "c", "d"]);
    expect(next.at).toBe(1);
    // Past the end, the first new card shows.
    const done = walkOf(["a"], { at: 1 });
    expect(
      walkReducer(done, { type: "extend", loadId: done.loadId, keys: ["c"] }),
    ).toMatchObject({ order: ["a", "c"], at: 1 });
    // Nothing new, or another load's page: the walk itself.
    expect(
      walkReducer(w, { type: "extend", loadId: w.loadId, keys: ["a"] }),
    ).toBe(w);
    expect(
      walkReducer(w, { type: "extend", loadId: "other", keys: ["z"] }),
    ).toBe(w);
  });
});

describe("batchOf", () => {
  const suggest = (batchKey: string | null): Advice => ({
    kind: "suggest",
    rule: "confident_claim",
    choice: approve,
    basis: [],
    strength: { kind: "unmeasured" },
    batchKey,
  });
  const advice = new Map<string, Advice>([
    ["a", suggest("claim:x")],
    ["b", suggest("claim:y")],
    ["c", suggest("claim:x")],
    ["d", suggest(null)],
    ["e", { kind: "none", why: "unscored" }],
  ]);
  it("gathers the cards of the same batch key, in order", () => {
    expect(batchOf(["a", "b", "c", "d", "e"], advice, "c")).toEqual({
      batchKey: "claim:x",
      items: [
        { key: "a", choice: approve },
        { key: "c", choice: approve },
      ],
    });
  });
  it("makes no batch of a card that isn't batchable, or of one", () => {
    expect(batchOf(["a", "b", "c", "d"], advice, "d")).toBeNull();
    expect(batchOf(["a", "b", "c", "d"], advice, "e")).toBeNull();
    expect(batchOf(["a", "b", "c", "d"], advice, "z")).toBeNull();
    expect(batchOf(["a", "b", "c", "d"], advice, "b")).toBeNull();
  });
});

describe("batchReason", () => {
  const suggest = (
    strength: Extract<Advice, { kind: "suggest" }>["strength"],
    basis: Extract<Advice, { kind: "suggest" }>["basis"] = [],
  ): Advice => ({
    kind: "suggest",
    rule: "confident_claim",
    choice: approve,
    basis,
    strength,
    batchKey: "claim:x",
  });
  it("says the choice, how many, and the random checks that measure it", () => {
    expect(
      batchReason(
        suggest({ kind: "measured", right: 80, total: 80, lower: 0.96 }, [
          { kind: "verifier", score: 0.72, band: "0.7" },
        ]),
        "Approve",
        3,
      ),
    ).toBe(
      "Founder suggests Approve for all 3: random checks of claims scored 0.7 to 0.8 were right 80 of 80 times.",
    );
    // Claims with no score: no band to name.
    expect(
      batchReason(
        suggest({ kind: "measured", right: 1200, total: 1210, lower: 0.98 }, [
          { kind: "verifier", band: "none" },
        ]),
        "Approve",
        1,
      ),
    ).toBe(
      "Founder suggests Approve for it: random checks of claims were right 1,200 of 1,210 times.",
    );
  });
  it("says whose earlier choice it repeats, and how often", () => {
    expect(
      batchReason(
        suggest({ kind: "repeat", decisions: 1, by: "you" }),
        "Not a Customer we have",
        2,
      ),
    ).toBe(
      "Founder suggests Not a Customer we have for all 2: you chose it for this name once before.",
    );
    expect(
      batchReason(
        suggest({ kind: "repeat", decisions: 4, by: "team" }),
        undefined,
        5,
      ),
    ).toBe(
      "Founder suggests the same choice for all 5: the team chose it for this name 4 times before.",
    );
  });
  it("says only the choice when unmeasured, and nothing without a suggestion", () => {
    expect(batchReason(suggest({ kind: "unmeasured" }), "Approve", 2)).toBe(
      "Founder suggests Approve for all 2.",
    );
    expect(batchReason({ kind: "none", why: "unscored" }, "Approve", 2)).toBe(
      null,
    );
  });
});

describe("linkedNow and notLinked", () => {
  const link = resolveDecision("d-1", { entityKey: "scus_1" });
  const notInData = resolveDecision("d-2");
  const saved = (d: FoundryReviewDecision): Outcome => ({
    kind: "saved",
    decision: d,
    how: "accepted",
  });
  it("says whether the current load links the name now, as last read or foreseen", () => {
    const read = (status?: DecisionApplied["status"]) =>
      new Map<string, DecisionApplied | null>([
        [link.id, status === undefined ? null : { status }],
      ]);
    expect(linkedNow(link, read("pending"), false)).toBe(true);
    expect(linkedNow(link, read("written"), false)).toBe(true);
    for (const status of ["refused", "missed", "unconfirmed", ""] as const)
      expect(linkedNow(link, read(status), true)).toBe(false);
    // Listed, with nothing the load made of it: not held.
    expect(linkedNow(link, read(undefined), true)).toBe(false);
    // Not read yet: as foreseen.
    expect(linkedNow(link, new Map(), true)).toBe(true);
    expect(linkedNow(link, new Map(), false)).toBe(false);
    // Not in data, or another decision, links nothing.
    expect(
      linkedNow(
        notInData,
        new Map([[notInData.id, { status: "written" }]]),
        true,
      ),
    ).toBe(false);
    expect(linkedNow(decision("r"), new Map(), true)).toBe(false);
  });
  it("names the saved links the load doesn't hold, once read", () => {
    const other = resolveDecision("d-3", { entityKey: "scus_2" });
    const outcomes = new Map<string, Outcome>([
      ["a", saved(link)],
      ["b", saved(other)],
      ["c", saved(notInData)],
      ["d", { kind: "sending", choice: approve, how: "chosen" }],
      ["e", saved(resolveDecision("d-4", { entityKey: "scus_3" }))],
    ]);
    const read = new Map([
      [link.id, { status: "unconfirmed" as const }],
      [other.id, { status: "written" as const }],
      [notInData.id, { status: "refused" as const }],
    ]);
    // e isn't read yet: it still covers.
    expect([...notLinked(outcomes, read)]).toEqual(["a"]);
    // Uncovering what a covers opens it again.
    const covered = walkOf(["a", "x", "y"], {}, [
      ["a", saved(link)],
      ["x", { kind: "covered", by: "a" }],
    ]);
    const opened = run(covered, {
      type: "uncover",
      by: notLinked(covered.outcomes, read),
    });
    expect(opened.outcomes.has("x")).toBe(false);
    expect(undecided(opened.order, opened.outcomes)).toBe(2);
  });
  it("opens a name's saved choice again once its job is read as unconfirmed", () => {
    const outcomes = new Map<string, Outcome>([
      ["a", saved(link)],
      ["b", saved(notInData)],
      ["c", saved(resolveDecision("d-3", { entityKey: "scus_2" }))],
      ["d", saved(decision("r"))],
      ["e", { kind: "sending", choice: approve, how: "chosen" }],
    ]);
    const read = [
      { ...link, applied: { status: "unconfirmed" as const } },
      { ...notInData, applied: { status: "unconfirmed" as const } },
      resolveDecision("d-3", { entityKey: "scus_2", applied: "written" }),
      { ...decision("r"), applied: { status: "unconfirmed" as const } },
    ];
    // Names only, a link or none, whose read is of that very save.
    expect([...unconfirmedNames(outcomes, read)]).toEqual(["a", "b"]);
    // Chosen again, the save is a new one: a read from before is no word on it.
    const again = new Map(outcomes).set(
      "a",
      saved({ ...link, decidedAt: "2026-11-02T00:00:00Z" }),
    );
    expect([...unconfirmedNames(again, read)]).toEqual(["b"]);
    expect(unconfirmedNames(outcomes, []).size).toBe(0);

    const w = walkOf(["a", "b", "x"], { at: 3 }, [
      ["a", saved(link)],
      ["b", saved(notInData)],
      ["x", { kind: "skipped" }],
    ]);
    expect(progress(w).left).toBe(0);
    const reopened = run(w, { type: "unconfirmed", keys: new Set(["a", "x"]) });
    expect(reopened.outcomes.get("a")).toEqual({
      kind: "unconfirmed",
      decision: link,
      how: "accepted",
    });
    // Only a saved choice turns: x stays skipped, b stays saved.
    expect(reopened.outcomes.get("x")).toEqual({ kind: "skipped" });
    expect(reopened.outcomes.get("b")?.kind).toBe("saved");
    // Still to decide: counted so, and the batch and the walk take it.
    expect(progress(reopened)).toMatchObject({ accepted: 1, left: 1 });
    expect(undecided(reopened.order, reopened.outcomes)).toBe(2);
    expect(batchTakes(reopened, "a")).toBe(true);
    // Choosing again sends it, and moves on past it.
    const chosen = run(
      { ...reopened, at: 0 },
      { type: "decide", key: "a", choice: approve, how: "chosen" },
    );
    expect(chosen.outcomes.get("a")?.kind).toBe("sending");
    expect(chosen.at).toBe(3);
  });
  it("takes a choice removed or replaced elsewhere as decided there, and opens what its link covered", () => {
    const other = resolveDecision("d-3", { entityKey: "scus_2" });
    const outcomes = new Map<string, Outcome>([
      ["a", saved(link)],
      ["b", saved(other)],
      ["c", { kind: "unconfirmed", decision: notInData, how: "chosen" }],
      ["d", { kind: "sending", choice: approve, how: "chosen" }],
      ["x", { kind: "covered", by: "a" }],
    ]);
    // a's decision is gone (removed, or replaced: a new id), c's read back
    // as a withdraw; b's is there.
    const read = [
      other,
      { ...notInData, kind: "withdraw" } as unknown as FoundryReviewDecision,
    ];
    const changed = changedElsewhere(outcomes, read);
    expect([...changed]).toEqual(["a", "c"]);
    expect(changedElsewhere(outcomes, [link, other, notInData]).size).toBe(0);

    const w = walkOf(
      ["a", "x", "b", "c", "d"],
      { at: 4, undo: [["a"], ["b", "c"]] },
      [...outcomes],
    );
    const after = run(w, { type: "changed", keys: changed });
    expect(after.outcomes.get("a")).toEqual({ kind: "elsewhere" });
    expect(after.outcomes.get("c")).toEqual({ kind: "elsewhere" });
    expect(after.outcomes.get("b")?.kind).toBe("saved");
    // What a's link covered is to decide again.
    expect(after.outcomes.has("x")).toBe(false);
    // Nothing to undo of a choice no longer this walk's.
    expect(after.undo).toEqual([["b"]]);
    expect(after.at).toBe(4);
  });
});
