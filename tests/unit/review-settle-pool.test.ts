import { describe, expect, it } from "vitest";
import { settleAll } from "@/web/areas/reviews/settle-pool";

// The pool that sends a batch of decisions (reviews/settle-pool.ts).

/** A send that waits until released, keeping count of those in flight. */
function gate() {
  let flying = 0,
    most = 0;
  const waiting: (() => void)[] = [];
  const send = async (n: number) => {
    flying += 1;
    most = Math.max(most, flying);
    await new Promise<void>((resolve) => waiting.push(resolve));
    flying -= 1;
    return n * 10;
  };
  // Lets every waiting send go, then the next turns run.
  const release = async () => {
    while (waiting.length) waiting.shift()!();
    await new Promise((r) => setTimeout(r, 0));
  };
  return { send, release, most: () => most, flying: () => flying };
}

describe("settleAll", () => {
  it("never has more than atOnce in flight, and keeps the items' order", async () => {
    const g = gate();
    const progress: number[] = [];
    const done = settleAll([1, 2, 3, 4, 5], g.send, {
      atOnce: 2,
      onProgress: (n) => progress.push(n),
    });
    await new Promise((r) => setTimeout(r, 0));
    expect(g.flying()).toBe(2);
    for (let i = 0; i < 5; i++) await g.release();
    expect(await done).toEqual(
      [1, 2, 3, 4, 5].map((n) => ({
        item: n,
        kind: "sent",
        value: n * 10,
      })),
    );
    expect(g.most()).toBe(2);
    expect(progress).toEqual([1, 2, 3, 4, 5]);
  });
  it("says why an item failed, as an Error whatever was thrown", async () => {
    const results = await settleAll(
      [1, -2, 3],
      async (n) => {
        if (n === 3) throw "plain";
        if (n < 0) throw new Error(`refused ${n}`);
        return n;
      },
      { atOnce: 4 },
    );
    expect(results[0]).toEqual({ item: 1, kind: "sent", value: 1 });
    expect(results[1]).toMatchObject({ item: -2, kind: "failed" });
    expect(results[1]!.kind === "failed" && results[1]!.error.message).toBe(
      "refused -2",
    );
    expect(results[2]!.kind === "failed" && results[2]!.error.message).toBe(
      "plain",
    );
  });
  it("starts no send once stopped, and finishes those in flight", async () => {
    const g = gate();
    const stop = new AbortController();
    const done = settleAll([1, 2, 3, 4], g.send, {
      atOnce: 2,
      signal: stop.signal,
    });
    await new Promise((r) => setTimeout(r, 0));
    stop.abort();
    await g.release();
    expect(await done).toEqual([
      { item: 1, kind: "sent", value: 10 },
      { item: 2, kind: "sent", value: 20 },
      { item: 3, kind: "stopped" },
      { item: 4, kind: "stopped" },
    ]);
    expect(g.most()).toBe(2);
  });
  it("sends nothing of nothing, and at least one at a time", async () => {
    expect(await settleAll([], async () => 1, { atOnce: 3 })).toEqual([]);
    const g = gate();
    const done = settleAll([1, 2], g.send, { atOnce: 0 });
    await g.release();
    await g.release();
    expect(await done).toHaveLength(2);
    expect(g.most()).toBe(1);
  });
});
