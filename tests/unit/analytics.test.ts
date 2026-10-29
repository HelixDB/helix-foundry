import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The hosted site's product analytics (src/web/analytics.ts): nothing is
// sent or kept without a client id; a sign-in is reported when the tab that
// started it is signed in; a job is reported when it starts and, once, when
// a read sees it end. src/web/telemetry.tsx keeps page titles out.

const sdk = vi.hoisted(() => ({
  track: vi.fn(),
  isTrackerAvailable: vi.fn(() => true),
  setGlobalProperties: vi.fn(),
}));
vi.mock("@databuddy/sdk", () => sdk);

/** A Storage that holds values in a Map, or throws like a blocked one. */
function fakeStorage(blocked = false) {
  const values = new Map<string, string>();
  const guard = () => {
    if (blocked) throw new DOMException("blocked", "SecurityError");
  };
  return {
    values,
    getItem: (k: string) => (guard(), values.get(k) ?? null),
    setItem: (k: string, v: string) => (guard(), void values.set(k, v)),
    removeItem: (k: string) => (guard(), void values.delete(k)),
  };
}

let local: ReturnType<typeof fakeStorage>,
  session: ReturnType<typeof fakeStorage>;

/** analytics.ts as a build with `clientId` (empty: none) loads it. */
const load = async (clientId = "client-test", blocked = false) => {
  vi.stubEnv("NEXT_PUBLIC_DATABUDDY_CLIENT_ID", clientId);
  local = fakeStorage(blocked);
  session = fakeStorage(blocked);
  vi.stubGlobal("localStorage", local);
  vi.stubGlobal("sessionStorage", session);
  vi.resetModules();
  return import("@/web/analytics");
};

const sent = () => sdk.track.mock.calls;
const PENDING = "foundry.analytics.pending.v1";

beforeEach(() => {
  sdk.track.mockReset();
  sdk.isTrackerAvailable.mockReset().mockReturnValue(true);
  sdk.setGlobalProperties.mockReset();
});
afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("trackEvent", () => {
  it("sends an event and its properties", async () => {
    const a = await load();
    a.trackEvent("connector_created", { kind: "postgres" });
    a.trackEvent("onboarding_shown");
    expect(sent()).toEqual([
      ["connector_created", { kind: "postgres" }],
      ["onboarding_shown", undefined],
    ]);
  });

  it("sends nothing without a client id", async () => {
    const a = await load("");
    expect(a.DATABUDDY_CLIENT_ID).toBe("");
    a.trackEvent("sample_clicked");
    await a.signInStarted("github");
    await a.trackBeforeLeaving("upgrade_clicked", {
      tier: "pro",
      from: "free",
    });
    a.jobStarted("load", "load_1");
    expect(sent()).toEqual([]);
    expect(sdk.isTrackerAvailable).not.toHaveBeenCalled();
    expect(local.values.size + session.values.size).toBe(0);
  });
});

describe("sign-in", () => {
  it("reports the sign-in this tab started, once", async () => {
    const a = await load();
    await a.signInStarted("google");
    a.signedIn("onboarding");
    a.signedIn("onboarding");
    expect(sent()).toEqual([
      ["sign_in_started", { method: "google" }],
      ["login_completed", { destination: "onboarding" }],
    ]);
    expect(session.values.size).toBe(0);
  });

  it("reports nothing for a session the tab didn't start", async () => {
    const a = await load();
    a.signedIn("workspace");
    expect(sent()).toEqual([]);
  });

  it("still reports the start when storage is blocked", async () => {
    const a = await load("client-test", true);
    await a.signInStarted("microsoft");
    a.signedIn("workspace");
    expect(sent()).toEqual([["sign_in_started", { method: "microsoft" }]]);
  });
});

describe("trackBeforeLeaving", () => {
  it("waits for the tracker to load before sending", async () => {
    vi.useFakeTimers();
    const a = await load();
    sdk.isTrackerAvailable.mockReturnValue(false);
    let left = false;
    void a.signInStarted("github").then(() => (left = true));
    await vi.advanceTimersByTimeAsync(200);
    expect(left).toBe(false);
    expect(sent()).toEqual([]);
    sdk.isTrackerAvailable.mockReturnValue(true);
    await vi.advanceTimersByTimeAsync(50);
    expect(left).toBe(true);
    expect(sent()).toEqual([["sign_in_started", { method: "github" }]]);
  });

  it("lets the page leave after a second without the tracker", async () => {
    vi.useFakeTimers();
    const a = await load();
    sdk.isTrackerAvailable.mockReturnValue(false);
    let left = false;
    void a
      .trackBeforeLeaving("upgrade_clicked", { tier: "max", from: "pro" })
      .then(() => (left = true));
    await vi.advanceTimersByTimeAsync(950);
    expect(left).toBe(false);
    await vi.advanceTimersByTimeAsync(100);
    expect(left).toBe(true);
    // Queued in the SDK, in case the tracker loads before the page goes.
    expect(sent()).toEqual([["upgrade_clicked", { tier: "max", from: "pro" }]]);
  });
});

describe("jobs", () => {
  it("reports a generation's start once and its end with its source", async () => {
    const a = await load();
    a.jobStarted("generation", "gen_1", "sample");
    a.jobStarted("generation", "gen_1", "sample");
    a.jobStarted("generation", "gen_2", "ai");
    a.jobSeen("generation", [
      { id: "gen_1", status: "ONTOLOGY_GENERATION_STATUS_RUNNING" },
      { id: "gen_2", status: "ONTOLOGY_GENERATION_STATUS_FAILED" },
    ]);
    a.jobSeen("generation", [
      { id: "gen_1", status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" },
      { id: "gen_2", status: "ONTOLOGY_GENERATION_STATUS_FAILED" },
    ]);
    expect(sent()).toEqual([
      ["generation_started", { source: "sample" }],
      ["generation_started", { source: "ai" }],
      ["generation_failed", { source: "ai" }],
      ["generation_succeeded", { source: "sample" }],
    ]);
    expect(local.values.has(PENDING)).toBe(false);
  });

  it("reports a ready or partial load, and forgets a failed one", async () => {
    const a = await load();
    for (const id of ["load_1", "load_2", "load_3"]) a.jobStarted("load", id);
    a.jobSeen("load", [
      { id: "load_1", status: "ready" },
      { id: "load_2", status: "partial" },
      { id: "load_3", status: "failed" },
    ]);
    expect(sent()).toEqual([
      ["load_started", undefined],
      ["load_started", undefined],
      ["load_started", undefined],
      ["load_ready", { status: "ready" }],
      ["load_ready", { status: "partial" }],
    ]);
    a.jobSeen("load", [{ id: "load_3", status: "ready" }]);
    expect(sent()).toHaveLength(5);
  });

  it("reports an answered question, and nothing for one that wasn't", async () => {
    const a = await load();
    a.jobStarted("question", "q_1");
    a.jobStarted("question", "q_2");
    a.jobSeen("question", [
      { id: "q_1", status: "queued" },
      { id: "q_2", status: "cannot_answer" },
    ]);
    a.jobSeen("question", [{ id: "q_1", status: "answered" }]);
    expect(sent().map(([name]) => name)).toEqual([
      "question_asked",
      "question_asked",
      "question_answered",
    ]);
  });

  it("reports only jobs started here, as the job they were started as", async () => {
    const a = await load();
    a.jobStarted("load", "x_1");
    a.jobSeen("question", [{ id: "x_1", status: "answered" }]);
    a.jobSeen("load", [
      { id: "someone_elses", status: "ready" },
      undefined,
      null,
      { status: "ready" },
      { id: "x_1" },
    ]);
    expect(sent()).toEqual([["load_started", undefined]]);
    a.jobSeen("load", [{ id: "x_1", status: "ready" }]);
    expect(sent()).toHaveLength(2);
  });

  it("ignores a job without an id", async () => {
    const a = await load();
    a.jobStarted("load", undefined);
    expect(sent()).toEqual([]);
  });

  it("reports a job that ends after a reload", async () => {
    const first = await load();
    first.jobStarted("generation", "gen_1", "ai");
    const kept = local.values.get(PENDING)!;
    const second = await load();
    local.values.set(PENDING, kept);
    second.jobSeen("generation", [
      { id: "gen_1", status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" },
    ]);
    expect(sent().at(-1)).toEqual(["generation_succeeded", { source: "ai" }]);
  });

  it("drops jobs a week old unreported", async () => {
    vi.useFakeTimers();
    const a = await load();
    a.jobStarted("load", "old");
    vi.advanceTimersByTime(8 * 24 * 60 * 60 * 1000);
    a.jobStarted("load", "new");
    expect(Object.keys(JSON.parse(local.values.get(PENDING)!))).toEqual([
      "new",
    ]);
  });

  it("reads what storage holds without trusting it", async () => {
    const a = await load();
    local.values.set(PENDING, "not json");
    a.jobSeen("load", [{ id: "load_1", status: "ready" }]);
    local.values.set(PENDING, "null");
    a.jobStarted("load", "load_1");
    // Entries that aren't jobs are dropped, never read.
    local.values.set(
      PENDING,
      JSON.stringify({ a: null, b: 5, c: { job: "load" }, d: [] }),
    );
    a.jobSeen("load", [{ id: "a", status: "ready" }]);
    a.jobStarted("load", "load_2");
    expect(Object.keys(JSON.parse(local.values.get(PENDING)!))).toEqual([
      "load_2",
    ]);
    // A source that isn't one of its words reads as the agent's.
    local.values.set(
      PENDING,
      JSON.stringify({
        gen_1: { job: "generation", source: "<b>", at: Date.now() },
      }),
    );
    a.jobSeen("generation", [
      { id: "gen_1", status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" },
    ]);
    expect(sent()).toEqual([
      ["load_started", undefined],
      ["load_started", undefined],
      ["generation_succeeded", { source: "ai" }],
    ]);
  });

  it("still reports the start when storage is blocked", async () => {
    const a = await load("client-test", true);
    a.jobStarted("question", "q_1");
    a.jobSeen("question", [{ id: "q_1", status: "answered" }]);
    expect(sent()).toEqual([["question_asked", undefined]]);
  });
});

describe("withoutTitle", () => {
  it("drops the page title from page views, and from page exits", async () => {
    vi.resetModules();
    const { withoutTitle } = await import("@/web/telemetry");
    const event = { name: "screen_view", path: "/ask/*", title: "Q · Foundry" };
    expect(withoutTitle(event)).toBe(true);
    expect(event).toEqual({ name: "screen_view", path: "/ask/*" });
    expect(sdk.setGlobalProperties).toHaveBeenCalledWith({ title: undefined });
  });
});
