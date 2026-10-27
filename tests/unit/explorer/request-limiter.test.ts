import { describe, expect, it } from "vitest";

import {
  createRequestLimiter,
  delay,
  unlimited,
} from "@/web/areas/explore/explorer/lib/request-limiter";

/** A clock the test moves by hand; timers fire when it passes them. */
function fakeClock() {
  let now = 0;
  const timers: { at: number; run: () => void }[] = [];
  return {
    now: () => now,
    setTimeout: (run: () => void, ms: number) =>
      timers.push({ at: now + ms, run }),
    async advance(ms: number) {
      now += ms;
      for (const timer of timers.filter((entry) => entry.at <= now)) {
        timers.splice(timers.indexOf(timer), 1);
        timer.run();
      }
      await Promise.resolve();
      await Promise.resolve();
    },
  };
}

const flush = async () => {
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
};

describe("createRequestLimiter", () => {
  it("lets a burst of 2 through, then paces automatic requests at 1 per second", async () => {
    const clock = fakeClock();
    const limiter = createRequestLimiter(
      { maxInFlight: 5, perSecond: 1, burst: 2 },
      clock,
    );
    const started: number[] = [];
    for (let index = 0; index < 4; index += 1) {
      void limiter.acquire({ automatic: true }).then((release) => {
        started.push(index);
        release?.();
      });
    }
    await flush();
    expect(started).toEqual([0, 1]);
    await clock.advance(999);
    expect(started).toEqual([0, 1]);
    await clock.advance(1);
    await flush();
    expect(started).toEqual([0, 1, 2]);
    await clock.advance(1000);
    await flush();
    expect(started).toEqual([0, 1, 2, 3]);
  });

  it("keeps at most maxInFlight automatic requests in flight", async () => {
    const clock = fakeClock();
    const limiter = createRequestLimiter(
      { maxInFlight: 2, perSecond: 100, burst: 10 },
      clock,
    );
    const releases: (() => void)[] = [];
    for (let index = 0; index < 3; index += 1) {
      void limiter
        .acquire({ automatic: true })
        .then((release) => release && releases.push(release));
    }
    await flush();
    expect(releases).toHaveLength(2);
    releases[0]();
    await flush();
    expect(releases).toHaveLength(3);
  });

  it("sends a user's request at once, but charges it, so the automatic ones after it wait", async () => {
    const clock = fakeClock();
    const limiter = createRequestLimiter(
      { maxInFlight: 2, perSecond: 1, burst: 2 },
      clock,
    );
    const user = await limiter.acquire({ automatic: false });
    expect(user).not.toBeNull();
    await limiter.acquire({ automatic: false });
    let automatic = false;
    void limiter.acquire({ automatic: true }).then(() => {
      automatic = true;
    });
    await flush();
    expect(automatic).toBe(false);
    await clock.advance(1000);
    await flush();
    expect(automatic).toBe(true);
  });

  it("drops a waiting request whose signal aborts", async () => {
    const clock = fakeClock();
    const limiter = createRequestLimiter(
      { maxInFlight: 1, perSecond: 1, burst: 1 },
      clock,
    );
    const first = await limiter.acquire({ automatic: true });
    const controller = new AbortController();
    const waiting = limiter.acquire({
      automatic: true,
      signal: controller.signal,
    });
    controller.abort();
    await expect(waiting).resolves.toBeNull();
    first?.();
    await expect(
      limiter.acquire({ automatic: false, signal: controller.signal }),
    ).resolves.toBeNull();
  });
});

describe("unlimited and delay", () => {
  it("lets everything through and waits as asked", async () => {
    await expect(unlimited().acquire({ automatic: true })).resolves.toBeTypeOf(
      "function",
    );
    await expect(delay(0)).resolves.toBe(true);
    const controller = new AbortController();
    const waiting = delay(10_000, controller.signal);
    controller.abort();
    await expect(waiting).resolves.toBe(false);
  });
});
