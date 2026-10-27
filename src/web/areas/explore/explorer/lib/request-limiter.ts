/**
 * Paces the explorer's own requests so they stay inside the gateway's
 * per-database rate limit. The default plan allows 1 request per second
 * with a burst of 2 (dev and prod alike); over it the gateway answers 429
 * `rate_limited` at once.
 *
 * Automatic requests (the boot, degree probes, connecting new nodes,
 * background label counts) wait for a slot (at most `maxInFlight` of them at
 * a time) and a token (refilled at `perSecond`, up to `burst`). Requests the
 * user asked for (query dock runs included) go right away, but still take a
 * token, so the automatic ones that follow them wait their turn instead of
 * tripping the limit.
 */

export interface RequestPacing {
  /** Automatic requests in flight at once. */
  maxInFlight: number;
  /** Tokens added per second. */
  perSecond: number;
  /** Most tokens kept (and the lowest the balance goes is -burst). */
  burst: number;
}

export const DEFAULT_PACING: Readonly<RequestPacing> = Object.freeze({
  maxInFlight: 2,
  perSecond: 1,
  burst: 2,
});

/** Waits before retrying an explorer read the database rate-limited (inner 429 `rate_limited`). */
export const RATE_LIMIT_RETRY_DELAYS_MS: readonly number[] = Object.freeze([
  1000, 2000, 4000,
]);

export interface RequestLimiter {
  /**
   * Resolves with a release function once the request may go, or null when
   * `signal` aborted first. Call release when the request settles.
   */
  acquire(opts: {
    automatic: boolean;
    signal?: AbortSignal;
  }): Promise<(() => void) | null>;
}

interface Waiter {
  resolve: (release: (() => void) | null) => void;
  signal?: AbortSignal;
  onAbort?: () => void;
}

export function createRequestLimiter(
  pacing: Partial<RequestPacing> = {},
  clock: {
    now: () => number;
    setTimeout: (run: () => void, ms: number) => unknown;
  } = {
    now: () => Date.now(),
    setTimeout: (run, ms) => setTimeout(run, ms),
  },
): RequestLimiter {
  const { maxInFlight, perSecond, burst } = { ...DEFAULT_PACING, ...pacing };
  let tokens = burst;
  let refilledAt = clock.now();
  let inFlight = 0;
  let timerPending = false;
  const queue: Waiter[] = [];

  const refill = () => {
    const now = clock.now();
    tokens = Math.min(burst, tokens + ((now - refilledAt) / 1000) * perSecond);
    refilledAt = now;
  };

  const release = () => {
    let done = false;
    return () => {
      if (done) return;
      done = true;
      inFlight = Math.max(0, inFlight - 1);
      pump();
    };
  };

  const pump = () => {
    refill();
    while (queue.length > 0 && inFlight < maxInFlight && tokens >= 1) {
      const waiter = queue.shift()!;
      if (waiter.onAbort)
        waiter.signal?.removeEventListener("abort", waiter.onAbort);
      tokens -= 1;
      inFlight += 1;
      waiter.resolve(release());
    }
    if (
      queue.length > 0 &&
      inFlight < maxInFlight &&
      tokens < 1 &&
      !timerPending
    ) {
      // Wake up when the next token is in.
      timerPending = true;
      const wait = Math.max(1, Math.ceil(((1 - tokens) / perSecond) * 1000));
      clock.setTimeout(() => {
        timerPending = false;
        pump();
      }, wait);
    }
  };

  return {
    acquire({ automatic, signal }) {
      if (signal?.aborted) return Promise.resolve(null);
      if (!automatic) {
        refill();
        tokens = Math.max(-burst, tokens - 1);
        return Promise.resolve(() => {});
      }
      return new Promise((resolve) => {
        const waiter: Waiter = { resolve, signal };
        if (signal) {
          waiter.onAbort = () => {
            const index = queue.indexOf(waiter);
            if (index >= 0) queue.splice(index, 1);
            resolve(null);
          };
          signal.addEventListener("abort", waiter.onAbort, { once: true });
        }
        queue.push(waiter);
        pump();
      });
    },
  };
}

/** A limiter that lets everything through (an injected transport without the gateway's limits). */
export function unlimited(): RequestLimiter {
  return {
    acquire: ({ signal }) => Promise.resolve(signal?.aborted ? null : () => {}),
  };
}

/** Resolves after `ms`, or early with false when `signal` aborts. */
export function delay(ms: number, signal?: AbortSignal): Promise<boolean> {
  if (signal?.aborted) return Promise.resolve(false);
  if (ms <= 0) return Promise.resolve(true);
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve(true);
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      resolve(false);
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}
