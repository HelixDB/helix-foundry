import { describe, expect, it } from "vitest";
import type { FoundryReviewChoice, FoundryReviewDecision } from "@/shared/wfe";
import type { Advice } from "@/web/areas/reviews/advice";
import type {
  DecidedCard,
  Queue,
  QueueCard,
  QuoteCard,
} from "@/web/areas/reviews/queue";
import {
  batchOf,
  current,
  progress,
  walkOrder,
  walkReducer,
  type How,
  type Outcome,
  type Walk,
  type WalkAction,
  type WalkFilter,
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

const queueOf = (cards: QueueCard[]): Queue => ({
  cards,
  left: { decided: 0, unsettled: 0, blocked: 0, stale: 0 },
  pool: {
    "0.9": { held: 0, quotes: 0 },
    "0.8": { held: 0, quotes: 0 },
    "0.7": { held: 0, quotes: 0 },
    "0.6": { held: 0, quotes: 0 },
    low: { held: 0, quotes: 0 },
    none: { held: 0, quotes: 0 },
  },
});

const all: WalkFilter = {
  kinds: new Set(["name", "claim", "value"]),
  advice: "any",
};

describe("walkOrder", () => {
  const umbrella = { key: "scus_1", entity: entity("scus_1", "Umbrella") };
  const waiting = heldCard(5, { named: false });
  const asking = nameCard("umbrella", {
    candidates: [umbrella],
    held: [waiting],
  });
  const askingToo = nameCard("umbrella", { doc: "doc-2" });
  const bound = nameCard("umbrella", { doc: "doc-3", reasons: ["part_name"] });
  const covered = nameCard("initech", { doc: "doc-4" });
  const model = nameCard("globex", { reasons: ["adjudicated"] });
  const claim = heldCard(1, { doc: "doc-5", subjectType: "Person" });
  const value = valueCard([{ text: "a" }, { text: "a" }, { text: "a" }]);
  const table = valueCard([{ text: "a" }], { table: "b", id: "v-2" });
  const randomClaim = { ...heldCard(2), drawn: "random" as const };
  const decided: DecidedCard = {
    ...heldCard(3),
    kind: "decided",
    drawn: "random",
  };
  const quote: QuoteCard = {
    kind: "quote",
    key: "quote-0.9-0",
    band: "0.9",
    draw: 0,
    drawn: "random",
  };
  const queue = queueOf([
    asking,
    claim,
    randomClaim,
    askingToo,
    bound,
    covered,
    model,
    decided,
    value,
    table,
    quote,
  ]);
  const ctx = ctxOf({
    decisions: [
      resolveDecision("d-1", {
        doc: "doc-9",
        normName: "initech",
        entityKey: "scus_7",
      }),
    ],
  });
  it("puts each card in its lane, in the queue's order", () => {
    const o = walkOrder(queue, ctx, all);
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
    expect(o.spot).toEqual([randomClaim.key, decided.key, quote.key]);
    // Every card, a held claim of a name too, by key.
    expect(o.cards.get(waiting.key)).toBe(waiting);
    expect(o.cards.get(quote.key)).toBe(quote);
  });
  it("advises the cards to decide, and no random draw", () => {
    const o = walkOrder(queue, ctx, all);
    expect(o.advice.get(asking.key)).toMatchObject({
      kind: "suggest",
      rule: "single_candidate",
    });
    expect(o.advice.get(waiting.key)).toEqual({
      kind: "none",
      why: "waits_on_name",
    });
    expect(o.advice.get(model.key)).toEqual({ kind: "none", why: "fyi" });
    expect(o.advice.has(randomClaim.key)).toBe(false);
    expect(o.advice.has(quote.key)).toBe(false);
  });
  it("pairs the names the load asks about in other documents", () => {
    const o = walkOrder(queue, ctx, all);
    expect(o.siblings.get(asking.key)).toEqual([askingToo.key]);
    expect(o.siblings.get(askingToo.key)).toEqual([asking.key]);
    expect(o.siblings.has(bound.key)).toBe(false);
    expect(o.siblings.has(covered.key)).toBe(false);
  });
  it("filters by kind, advice, type and document, never the spot checks", () => {
    const claims = walkOrder(queue, ctx, { ...all, kinds: new Set(["claim"]) });
    expect(claims.decide).toEqual([waiting.key, claim.key]);
    expect(claims.fyi).toEqual([]);
    expect(claims.covered).toEqual([]);
    expect(claims.spot).toEqual([randomClaim.key, decided.key, quote.key]);
    expect(
      walkOrder(queue, ctx, { ...all, advice: "suggested" }).decide,
    ).toEqual([asking.key, value.key]);
    expect(walkOrder(queue, ctx, { ...all, advice: "none" }).decide).toEqual([
      waiting.key,
      claim.key,
      askingToo.key,
      bound.key,
    ]);
    expect(walkOrder(queue, ctx, { ...all, type: "Person" }).decide).toEqual([
      claim.key,
    ]);
    expect(walkOrder(queue, ctx, { ...all, type: "Customer" }).decide).toEqual([
      asking.key,
      askingToo.key,
      bound.key,
      value.key,
    ]);
    expect(
      walkOrder(queue, ctx, { ...all, documentId: "doc-2" }).decide,
    ).toEqual([askingToo.key]);
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
  it("forgets undone keys it never had", () => {
    const w = walkOf(["a", "b"], { at: 1 });
    expect(current(run(w, { type: "undone", keys: ["z"] }))).toBe("b");
  });
  it("rebases on the same load keeping every outcome", () => {
    const w = run(walkOf(["a", "b", "c"]), decide("a"), { type: "skip" });
    expect(current(w)).toBe("c");
    const r = run(w, {
      type: "rebase",
      loadId: "L",
      order: ["c", "d"],
      elsewhere: new Set(["b", "d"]),
    });
    expect(r.order).toEqual(["c", "d"]);
    expect(current(r)).toBe("c");
    expect(r.outcomes.get("a")?.kind).toBe("sending");
    // Skipped, then decided by someone else.
    expect(r.outcomes.get("b")).toEqual({ kind: "elsewhere" });
    expect(r.outcomes.get("d")).toEqual({ kind: "elsewhere" });
    expect(r.undo).toEqual([["a"]]);
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
      elsewhere: new Set(["a"]),
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
  it("rebases onto the next card of the old order still in the new", () => {
    const w = walkOf(["a", "b", "c"], { at: 1 });
    const r = run(w, {
      type: "rebase",
      loadId: "L2",
      order: ["a", "c"],
      elsewhere: new Set(),
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
      left: 2,
    });
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
