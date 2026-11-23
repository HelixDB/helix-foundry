// The review walk's and Approve all's outbox: choices go to the backend in
// batches (reviews:batchSettle), one request at a time, so choices made
// while one is in flight go together in the next. Pure of React and of the
// API: `send` is the call.

/** A push taken back by `cancel` before it was sent. */
export class Cancelled extends Error {
  constructor() {
    super("Cancelled before it was sent.");
    this.name = "Cancelled";
  }
}

export type Outbox<T, R> = {
  /**
   * Queues an item: it goes in the next request with at most `max - 1`
   * others pushed before or with it. Resolves with its own answer; rejects
   * with the request's error when the request fails, or with Cancelled.
   */
  push(item: T): Promise<R>;
  /**
   * Takes back the queued items `taken` picks, never one in flight, and
   * says which: each push rejects with Cancelled.
   */
  cancel(taken: (item: T) => boolean): T[];
  /** How many items are queued or in flight. */
  pending(): number;
};

/**
 * An outbox over `send`, which answers each item of a request in its order.
 * One request is in flight at a time, of at most `max` items, in the order
 * pushed; items pushed in one tick go together, and those pushed while a
 * request is in flight wait for the next. An answer of another length
 * rejects every item of its request, as a failed request does.
 *
 * @example
 * const box = createOutbox(async (ns: readonly number[]) => ns.map((n) => n * 2), { max: 25 });
 * await Promise.all([box.push(1), box.push(2)]); // [2, 4], in one request
 */
export function createOutbox<T, R>(
  send: (items: readonly T[]) => Promise<readonly R[]>,
  o: { max: number },
): Outbox<T, R> {
  if (o.max < 1)
    throw new Error("An outbox sends at least one item a request.");
  type Queued = {
    item: T;
    resolve: (r: R) => void;
    reject: (e: unknown) => void;
  };
  const queue: Queued[] = [];
  let flying = 0,
    scheduled = false;
  const flush = async () => {
    if (flying || !queue.length) return;
    const batch = queue.splice(0, o.max);
    flying = batch.length;
    try {
      const answers = await send(batch.map((q) => q.item));
      if (answers.length !== batch.length)
        throw new Error("The answer didn’t match what was sent.");
      batch.forEach((q, i) => q.resolve(answers[i]!));
    } catch (e) {
      for (const q of batch) q.reject(e);
    } finally {
      flying = 0;
      void flush();
    }
  };
  return {
    push: (item) =>
      new Promise<R>((resolve, reject) => {
        queue.push({ item, resolve, reject });
        if (scheduled) return;
        scheduled = true;
        queueMicrotask(() => {
          scheduled = false;
          void flush();
        });
      }),
    cancel: (taken) => {
      const out: T[] = [];
      for (let i = 0; i < queue.length; ) {
        const q = queue[i]!;
        if (!taken(q.item)) {
          i += 1;
          continue;
        }
        queue.splice(i, 1);
        q.reject(new Cancelled());
        out.push(q.item);
      }
      return out;
    },
    pending: () => queue.length + flying,
  };
}
