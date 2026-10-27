// Runs one job per key at a time: concurrent callers with the same key share
// the in-flight promise. A settled value that `keep` accepts is reused for
// `graceMs` too, so a caller that arrives just after the job (still holding
// the same key) gets the same result instead of running it again.
//
// Used for token refresh: WorkOS refresh tokens are single use and Foundry
// access tokens live 300 s, so parallel /papi calls at expiry must spend the
// refresh token once. The state is per server instance; see AGENTS.md.

export function singleFlight<T>({
  graceMs,
  maxKept = 5000,
  keep,
}: {
  graceMs: number;
  maxKept?: number;
  keep: (value: T) => boolean;
}) {
  const running = new Map<string, Promise<T>>(),
    kept = new Map<string, { value: T; until: number }>();
  return function run(
    key: string,
    job: () => Promise<T>,
    now = Date.now,
  ): Promise<T> {
    const hit = kept.get(key);
    if (hit && hit.until > now()) return Promise.resolve(hit.value);
    if (hit) kept.delete(key);
    const pending = running.get(key);
    if (pending) return pending;
    const promise = job()
      .then((value) => {
        if (keep(value)) {
          // Oldest first: Map keeps insertion order.
          for (const [k, v] of kept)
            if (v.until <= now() || kept.size >= maxKept) kept.delete(k);
            else break;
          kept.set(key, { value, until: now() + graceMs });
        }
        return value;
      })
      .finally(() => running.delete(key));
    running.set(key, promise);
    return promise;
  };
}
