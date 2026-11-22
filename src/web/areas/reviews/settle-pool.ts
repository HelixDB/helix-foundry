/**
 * What became of one item of a pool: sent (with what the send answered),
 * failed (with why), or never sent because the pool was stopped.
 */
export type Sent<T, R> = { item: T } & (
  | { kind: "sent"; value: R }
  | { kind: "failed"; error: Error }
  | { kind: "stopped" }
);

/**
 * Sends every item, at most `atOnce` at a time, and answers what became of
 * each, in the items' order. Once `signal` aborts, no send starts (those in
 * flight finish); `onProgress` hears how many have ended after each one. A
 * few senders share one queue, so a long batch doesn't go all at once: the
 * backend applies the decisions that land together in one job.
 *
 * @example
 * await settleAll([1, 2, 3], async (n) => n * 2, { atOnce: 2 });
 * // [{ item: 1, kind: "sent", value: 2 }, { item: 2, kind: "sent", value: 4 }, { item: 3, kind: "sent", value: 6 }]
 */
export async function settleAll<T, R>(
  items: readonly T[],
  send: (t: T) => Promise<R>,
  o: {
    atOnce: number;
    signal?: AbortSignal;
    onProgress?: (done: number) => void;
  },
): Promise<Sent<T, R>[]> {
  const results: Sent<T, R>[] = items.map((item) => ({
    item,
    kind: "stopped",
  }));
  let next = 0,
    done = 0;
  // A loop per sender, each taking the next item until none is left; a
  // loop's own index is kept, as `next` moves on while it waits.
  const sender = async () => {
    while (next < items.length && !o.signal?.aborted) {
      const i = next++;
      const item = items[i]!;
      try {
        results[i] = { item, kind: "sent", value: await send(item) };
      } catch (e) {
        results[i] = {
          item,
          kind: "failed",
          error: e instanceof Error ? e : new Error(String(e)),
        };
      }
      done += 1;
      o.onProgress?.(done);
    }
  };
  await Promise.all(
    Array.from(
      { length: Math.min(Math.max(1, o.atOnce), items.length) },
      sender,
    ),
  );
  return results;
}
