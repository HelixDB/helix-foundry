import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { config, db, resetState } from "../../mock/state";
import { startSession, verify } from "../../mock/tokens";

// The mock's access tokens live at least the configured TTL, so the e2e
// suite's one-second tokens reach the WFE before they run out.

describe("mock access tokens", () => {
  beforeEach(() => {
    resetState();
    vi.useFakeTimers({ toFake: ["Date"] });
  });
  afterEach(() => {
    vi.useRealTimers();
    resetState();
  });

  it("keep a one-second token for a whole second, even minted late in one", async () => {
    config.accessTokenTtlSeconds = 1;
    vi.setSystemTime(new Date("2026-10-22T16:01:19.950Z"));
    const ada = db.users.get("usr_ada")!;
    const { accessToken } = await startSession(ada);
    vi.setSystemTime(new Date("2026-10-22T16:01:20.900Z"));
    expect((await verify(accessToken))?.user.id).toBe("usr_ada");
    vi.setSystemTime(new Date("2026-10-22T16:01:21.000Z"));
    expect(await verify(accessToken)).toBeNull();
  });

  it("keep the default token for its 300 seconds", async () => {
    vi.setSystemTime(new Date("2026-10-22T16:00:00.000Z"));
    const { accessToken } = await startSession(db.users.get("usr_ada")!);
    vi.setSystemTime(new Date("2026-10-22T16:04:59.999Z"));
    expect(await verify(accessToken)).not.toBeNull();
    vi.setSystemTime(new Date("2026-10-22T16:05:00.000Z"));
    expect(await verify(accessToken)).toBeNull();
  });
});
