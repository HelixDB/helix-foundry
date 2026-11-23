import { describe, expect, it } from "vitest";
import { Cancelled, createOutbox } from "@/web/areas/reviews/outbox";

// The outbox the review walk and Approve all send choices through
// (reviews/outbox.ts): one request in flight, the next carrying what was
// pushed meanwhile.

/** A send that records each request and answers when told. */
function recorder() {
  const requests: number[][] = [];
  const pending: {
    resolve: (r: string[]) => void;
    reject: (e: Error) => void;
  }[] = [];
  const send = (items: readonly number[]) => {
    requests.push([...items]);
    return new Promise<string[]>((resolve, reject) =>
      pending.push({ resolve, reject }),
    );
  };
  const answer = (i: number) =>
    pending[i]!.resolve(requests[i]!.map((n) => `ok ${n}`));
  return { requests, pending, send, answer };
}
const tick = () => new Promise((r) => setTimeout(r, 0));

describe("createOutbox", () => {
  it("sends the items pushed in one tick together, in order", async () => {
    const r = recorder();
    const box = createOutbox(r.send, { max: 25 });
    const answers = [1, 2, 3].map((n) => box.push(n));
    await tick();
    expect(r.requests).toEqual([[1, 2, 3]]);
    expect(box.pending()).toBe(3);
    r.answer(0);
    expect(await Promise.all(answers)).toEqual(["ok 1", "ok 2", "ok 3"]);
    await tick();
    expect(box.pending()).toBe(0);
  });
  it("keeps one request in flight; the next carries what came meanwhile, at most max", async () => {
    const r = recorder();
    const box = createOutbox(r.send, { max: 2 });
    const first = box.push(1);
    await tick();
    const later = [2, 3, 4].map((n) => box.push(n));
    await tick();
    expect(r.requests).toEqual([[1]]);
    r.answer(0);
    await first;
    await tick();
    expect(r.requests).toEqual([[1], [2, 3]]);
    r.answer(1);
    await tick();
    expect(r.requests).toEqual([[1], [2, 3], [4]]);
    r.answer(2);
    expect(await Promise.all(later)).toEqual(["ok 2", "ok 3", "ok 4"]);
  });
  it("rejects each item of a failed request, and goes on with the next", async () => {
    const r = recorder();
    const box = createOutbox(r.send, { max: 25 });
    const failed = [box.push(1), box.push(2)];
    await tick();
    const next = box.push(3);
    r.pending[0]!.reject(new Error("offline"));
    for (const f of failed) await expect(f).rejects.toThrow("offline");
    await tick();
    expect(r.requests).toEqual([[1, 2], [3]]);
    r.answer(1);
    expect(await next).toBe("ok 3");
  });
  it("rejects every item of an answer of another length", async () => {
    const box = createOutbox(async () => ["one"], { max: 25 });
    const both = [box.push(1), box.push(2)];
    for (const p of both) await expect(p).rejects.toThrow("didn’t match");
  });
  it("cancels only queued items, never one in flight", async () => {
    const r = recorder();
    const box = createOutbox(r.send, { max: 25 });
    const flying = box.push(1);
    await tick();
    const queued = [box.push(2), box.push(3), box.push(4)];
    expect(box.cancel((n) => n === 1 || n === 3)).toEqual([3]);
    await expect(queued[1]).rejects.toBeInstanceOf(Cancelled);
    expect(box.pending()).toBe(3);
    r.answer(0);
    expect(await flying).toBe("ok 1");
    await tick();
    expect(r.requests).toEqual([[1], [2, 4]]);
    expect(box.cancel(() => true)).toEqual([]);
  });
  it("never sends two items of one key together: the later waits its turn, in order", async () => {
    const r = recorder();
    // Odd and even numbers share a key apiece, except 5, which has its own.
    const box = createOutbox(r.send, {
      max: 3,
      key: (n) => (n === 5 ? "five" : String(n % 2)),
    });
    const answers = [1, 3, 2, 4, 5, 6].map((n) => box.push(n));
    await tick();
    expect(r.requests).toEqual([[1, 2, 5]]);
    r.answer(0);
    await tick();
    expect(r.requests).toEqual([
      [1, 2, 5],
      [3, 4],
    ]);
    r.answer(1);
    await tick();
    expect(r.requests).toEqual([[1, 2, 5], [3, 4], [6]]);
    r.answer(2);
    expect(await Promise.all(answers)).toEqual(
      [1, 3, 2, 4, 5, 6].map((n) => `ok ${n}`),
    );
  });
  it("sends at least one item a request", () => {
    expect(() => createOutbox(async () => [], { max: 0 })).toThrow();
  });
});
