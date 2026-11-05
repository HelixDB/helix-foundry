import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The hosted site's product analytics (src/web/analytics.ts): nothing is
// sent or kept without a client id; a sign-in is reported when the tab that
// started it is signed in; a job is reported when it starts and, once, when
// a read sees it end. src/web/telemetry.tsx keeps page titles out and
// turns on everything the tracker records, replays with the paths masked.

const sdk = vi.hoisted(() => ({
  track: vi.fn(),
  isTrackerAvailable: vi.fn(() => true),
  getTracker: vi.fn(() => ({
    getTrackingIds: (): { anonId: string | null; sessionId: string | null } =>
      sdk.ids,
  })),
  ids: { anonId: "anon_1", sessionId: "sess_1" } as {
    anonId: string | null;
    sessionId: string | null;
  },
  profile: null as string | null,
  getProfileId: vi.fn((): string | null => sdk.profile),
  identify: vi.fn(),
  clearProfile: vi.fn(),
  trackError: vi.fn(),
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
  vi.stubGlobal("location", {
    origin: "https://foundry.helix-db.com",
    pathname: "/login",
  });
  vi.resetModules();
  return import("@/web/analytics");
};

const sent = () => sdk.track.mock.calls;
const PENDING = "foundry.analytics.pending.v1";

beforeEach(() => {
  sdk.track.mockReset();
  sdk.getTracker.mockClear();
  sdk.isTrackerAvailable.mockReset().mockReturnValue(true);
  sdk.ids = { anonId: "anon_1", sessionId: "sess_1" };
  sdk.setGlobalProperties.mockReset();
  sdk.identify.mockClear();
  sdk.clearProfile.mockClear();
  sdk.trackError.mockClear();
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
    a.jobStarted({ job: "load" }, "load_1");
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

describe("the signed-in user", () => {
  it("is identified by the backend's id with their email and name, and forgotten after signing out", async () => {
    const a = await load();
    vi.stubGlobal("navigator", { sendBeacon: () => true });
    a.identifyUser("user_1", { email: "ada@example.com", name: "Ada" });
    // Details it lacks stay off the profile.
    a.identifyUser("user_1", { email: "", name: undefined });
    a.identifyUser("user_1");
    a.identifyUser("", { email: "ada@example.com" });
    expect(sdk.identify.mock.calls).toEqual([
      ["user_1", { email: "ada@example.com", name: "Ada" }],
      ["user_1", {}],
      ["user_1", {}],
    ]);
    // Events carry the email too, for the team's dashboard.
    expect(sdk.setGlobalProperties.mock.calls).toEqual([
      [{ user_email: "ada@example.com", workspace_owner: undefined }],
    ]);
    await a.signedOut();
    expect(sdk.clearProfile).toHaveBeenCalledOnce();
    expect(sdk.setGlobalProperties).toHaveBeenLastCalledWith({
      workspace_id: undefined,
      workspace_slug: undefined,
      workspace_name: undefined,
      workspace_role: undefined,
      workspace_owner: undefined,
      user_email: undefined,
    });
  });

  it("tags every event with the workspace open, its owner while it's them, and drops both at sign-out", async () => {
    const a = await load();
    vi.stubGlobal("navigator", { sendBeacon: () => true });
    const acme = {
      id: "ws_1",
      slug: "acme",
      name: "Acme",
      role: "owner",
    } as const;
    const globex = {
      id: "ws_2",
      slug: "globex",
      name: "Globex",
      role: "member",
    } as const;
    // The workspace may open before the user is identified (child effects
    // run first): the owner follows whichever comes last.
    a.workspaceOpened(acme);
    a.identifyUser("user_1", { email: "ada@example.com" });
    a.workspaceOpened(globex);
    expect(sdk.setGlobalProperties.mock.calls).toEqual([
      [
        {
          workspace_id: "ws_1",
          workspace_slug: "acme",
          workspace_name: "Acme",
          workspace_role: "owner",
          workspace_owner: undefined,
        },
      ],
      [
        {
          user_email: "ada@example.com",
          workspace_owner: "acme ada@example.com",
        },
      ],
      [
        {
          workspace_id: "ws_2",
          workspace_slug: "globex",
          workspace_name: "Globex",
          workspace_role: "member",
          workspace_owner: undefined,
        },
      ],
    ]);
    await a.signedOut();
    expect(sdk.setGlobalProperties).toHaveBeenLastCalledWith({
      workspace_id: undefined,
      workspace_slug: undefined,
      workspace_name: undefined,
      workspace_role: undefined,
      workspace_owner: undefined,
      user_email: undefined,
    });
  });

  it("is neither identified nor forgotten without a client id", async () => {
    const a = await load("");
    a.identifyUser("user_1");
    a.workspaceOpened({
      id: "ws_1",
      slug: "acme",
      name: "Acme",
      role: "owner",
    });
    await a.signedOut();
    expect(sdk.identify).not.toHaveBeenCalled();
    expect(sdk.clearProfile).not.toHaveBeenCalled();
    expect(sdk.setGlobalProperties).not.toHaveBeenCalled();
  });
});

describe("pageCrashed", () => {
  it("reports the crash and its error, or nothing without a client id", async () => {
    const error = new TypeError("x is undefined");
    (await load("")).pageCrashed(error, false);
    expect(sent()).toEqual([]);
    const a = await load();
    a.pageCrashed(error, false);
    a.pageCrashed(new Error("ChunkLoadError"), true);
    expect(sent()).toEqual([
      ["page_crashed", { cause: "error" }],
      ["page_crashed", { cause: "stale_build" }],
    ]);
    expect(sdk.trackError).toHaveBeenCalledWith("x is undefined", {
      error_type: "TypeError",
      stack: error.stack,
    });
  });
});

describe("trackBeforeLeaving", () => {
  /** navigator.sendBeacon, answering `queued`; what it was given. */
  const beacon = (queued = true) => {
    const beacons: { url: string; body: Blob }[] = [];
    vi.stubGlobal("navigator", {
      sendBeacon: (url: string, body: Blob) => (
        beacons.push({ url, body }),
        queued
      ),
    });
    return beacons;
  };

  it("beacons the event to the events API, as the tracker's visitor", async () => {
    const a = await load();
    const beacons = beacon();
    await a.signInStarted("github");
    expect(sent()).toEqual([]);
    expect(beacons).toHaveLength(1);
    expect(beacons[0].url).toBe(
      "https://basket.databuddy.cc/track?website_id=client-test",
    );
    expect(beacons[0].body.type).toBe("text/plain;charset=utf-8");
    expect(JSON.parse(await beacons[0].body.text())).toEqual({
      eventId: expect.any(String),
      name: "sign_in_started",
      properties: { method: "github" },
      anonymousId: "anon_1",
      sessionId: "sess_1",
      anonymizeVisitorIds: true,
      path: "https://foundry.helix-db.com/login",
      source: "browser",
      websiteId: "client-test",
    });
  });

  it("beacons as the signed-in user, in the workspace open", async () => {
    const a = await load();
    const beacons = beacon();
    sdk.profile = "user_1";
    a.identifyUser("user_1", { email: "ada@example.com" });
    a.workspaceOpened({
      id: "ws_1",
      slug: "acme",
      name: "Acme",
      role: "owner",
    });
    await a.trackBeforeLeaving("upgrade_clicked", {
      tier: "pro",
      from: "free",
    });
    sdk.profile = null;
    expect(JSON.parse(await beacons[0].body.text())).toMatchObject({
      name: "upgrade_clicked",
      properties: {
        user_email: "ada@example.com",
        workspace_id: "ws_1",
        workspace_slug: "acme",
        workspace_name: "Acme",
        workspace_role: "owner",
        workspace_owner: "acme ada@example.com",
        tier: "pro",
        from: "free",
      },
      profileId: "user_1",
    });
  });

  it("names a fixed path in place of a page's that names ids", async () => {
    const a = await load();
    vi.stubGlobal("location", {
      origin: "https://foundry.helix-db.com",
      pathname: "/data/connections/conn_1",
    });
    const beacons = beacon();
    await a.trackBeforeLeavingAt("/data", "connector_sign_in_started", {
      kind: "github",
    });
    expect(JSON.parse(await beacons[0].body.text())).toMatchObject({
      name: "connector_sign_in_started",
      properties: { kind: "github" },
      path: "https://foundry.helix-db.com/data",
    });
  });

  it("goes through the tracker when the beacon is refused", async () => {
    const a = await load();
    beacon(false);
    await a.trackBeforeLeaving("setup_prompt_copied", { setup: "local" });
    expect(sent()).toEqual([["setup_prompt_copied", { setup: "local" }]]);
  });

  // The tracker, which drops it, has no visitor while the visitor opts out.
  it("goes through the tracker when it has no visitor", async () => {
    const a = await load();
    const beacons = beacon();
    sdk.ids = { anonId: null, sessionId: null };
    await a.trackBeforeLeaving("setup_prompt_copied", { setup: "agent" });
    expect(beacons).toEqual([]);
    expect(sent()).toEqual([["setup_prompt_copied", { setup: "agent" }]]);
  });

  // Never a visitor id the SDK reads from storage, which it would before the
  // tracker loads, or when it is blocked.
  it("goes through the tracker when it never loads", async () => {
    vi.useFakeTimers();
    const a = await load();
    const beacons = beacon();
    sdk.isTrackerAvailable.mockReturnValue(false);
    sdk.getTracker.mockReturnValueOnce(null as never);
    const done = a.trackBeforeLeaving("upgrade_clicked", {
      tier: "pro",
      from: "free",
    });
    await vi.advanceTimersByTimeAsync(1100);
    await done;
    expect(beacons).toEqual([]);
    expect(sent()).toEqual([
      ["upgrade_clicked", { tier: "pro", from: "free" }],
    ]);
  });

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
    a.jobStarted({ job: "generation", source: "sample" }, "gen_1");
    a.jobStarted({ job: "generation", source: "sample" }, "gen_1");
    a.jobStarted({ job: "generation", source: "ai" }, "gen_2");
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

  it("reports a revision's start and end with its mode, apart from generations", async () => {
    const a = await load();
    a.jobStarted({ job: "revision", mode: "preview" }, "rev_1");
    a.jobStarted({ job: "revision", mode: "save" }, "rev_2");
    a.jobStarted({ job: "revision", mode: "save" }, "rev_3");
    // Revisions are generations: a read of them sees both kinds.
    const seen = [
      { id: "rev_1", status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" },
      { id: "rev_2", status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" },
      { id: "rev_3", status: "ONTOLOGY_GENERATION_STATUS_FAILED" },
    ] as const;
    a.jobSeen("generation", seen);
    a.jobSeen("revision", seen);
    a.jobSeen("revision", seen);
    expect(sent()).toEqual([
      ["revision_started", { mode: "preview" }],
      ["revision_started", { mode: "save" }],
      ["revision_started", { mode: "save" }],
      ["revision_succeeded", { mode: "preview" }],
      ["revision_succeeded", { mode: "save" }],
      ["revision_failed", { mode: "save" }],
    ]);
    expect(local.values.has(PENDING)).toBe(false);
  });

  it("reports a ready or partial load, and a failed one with why", async () => {
    const a = await load();
    const ids = ["load_1", "load_2", "load_3", "load_4", "load_5"];
    for (const id of ids) a.jobStarted({ job: "load" }, id);
    a.jobSeen("load", [
      { id: "load_1", status: "ready" },
      { id: "load_2", status: "partial" },
      { id: "load_3", status: "failed", jobs: [{ status: "failed" }] },
      // Its newest job says why.
      {
        id: "load_4",
        status: "failed",
        jobs: [{ status: "budget_exhausted" }, { status: "succeeded" }],
      },
      {
        id: "load_5",
        status: "failed",
        jobs: [
          {
            status: "failed",
            error: "its tables were synced again while this load was written",
          },
        ],
      },
    ]);
    expect(sent().slice(ids.length)).toEqual([
      ["load_ready", { status: "ready" }],
      ["load_ready", { status: "partial" }],
      ["load_failed", { reason: "error" }],
      ["load_failed", { reason: "budget" }],
      ["load_failed", { reason: "sources_changed" }],
    ]);
    a.jobSeen("load", [{ id: "load_3", status: "ready" }]);
    expect(sent()).toHaveLength(ids.length + 5);
  });

  it("reports an answered question, and nothing for one that wasn't", async () => {
    const a = await load();
    a.jobStarted({ job: "question" }, "q_1");
    a.jobStarted({ job: "question" }, "q_2");
    a.jobSeen("question", [
      { id: "q_1", status: "queued" },
      { id: "q_2", status: "cannot_answer" },
    ]);
    a.jobSeen("question", [{ id: "q_1", status: "answered" }]);
    a.jobStarted({ job: "question" }, "q_3");
    a.jobStarted({ job: "question" }, "q_4");
    a.jobSeen("question", [
      { id: "q_3", status: "failed" },
      { id: "q_4", status: "cancelled" },
    ]);
    expect(sent()).toEqual([
      ["question_asked", undefined],
      ["question_asked", undefined],
      ["question_unanswered", { status: "cannot_answer" }],
      ["question_answered", undefined],
      ["question_asked", undefined],
      ["question_asked", undefined],
      ["question_unanswered", { status: "failed" }],
    ]);
  });

  it.each([
    "uncovered",
    "after_as_of",
    "undeclared_relation",
    "undeclared_property",
    "absent",
  ])("reports only the known gap kind %s", async (kind) => {
    const a = await load();
    a.jobStarted({ job: "question" }, "q_kind");
    a.jobSeen("question", [
      {
        id: "q_kind",
        status: "cannot_answer",
        resultJson: JSON.stringify({
          status: "cannot_answer",
          kind,
          reason: "private text",
          gap: "private gap",
        }),
      },
    ]);
    expect(sent().at(-1)).toEqual([
      "question_unanswered",
      { status: "cannot_answer", kind },
    ]);
    a.jobSeen("question", [
      {
        id: "q_kind",
        status: "cannot_answer",
        resultJson: JSON.stringify({ status: "cannot_answer", kind }),
      },
    ]);
    expect(sent()).toHaveLength(2);
  });

  it.each([
    "",
    "broken",
    "null",
    "[]",
    '{"status":"cannot_answer","kind":"private text"}',
    '{"status":"answered","kind":"absent"}',
    '{"status":"cannot_answer","kind":null}',
  ])("does not send untrusted gap text (%s)", async (resultJson) => {
    const a = await load();
    a.jobStarted({ job: "question" }, "q_unknown");
    a.jobSeen("question", [
      { id: "q_unknown", status: "cannot_answer", resultJson },
    ]);
    expect(sent().at(-1)).toEqual([
      "question_unanswered",
      { status: "cannot_answer" },
    ]);
  });

  it("reports a sync's start, and its end with the connector's kind", async () => {
    const a = await load();
    a.jobStarted({ job: "sync", from: "tree" }, "sync_1");
    a.jobStarted({ job: "sync", from: "created" }, "sync_2");
    a.jobSeen("sync", [
      { id: "sync_1", status: "CONNECTOR_SYNC_STATUS_RUNNING", kind: "stripe" },
      { id: "sync_2", status: "CONNECTOR_SYNC_STATUS_FAILED", kind: "other" },
    ]);
    a.jobSeen("sync", [
      null,
      {
        id: "sync_1",
        status: "CONNECTOR_SYNC_STATUS_SUCCEEDED",
        kind: "stripe",
      },
    ]);
    expect(sent()).toEqual([
      ["connector_sync_started", { from: "tree" }],
      ["connector_sync_started", { from: "created" }],
      ["connector_sync_failed", { kind: "other" }],
      ["connector_synced", { kind: "stripe" }],
    ]);
  });

  it("reports a file read that failed, with its format and why", async () => {
    const a = await load();
    for (const id of ["d_1", "d_2", "d_3", "d_4"])
      a.jobStarted({ job: "read" }, id);
    a.jobSeen("read", [
      {
        id: "d_1",
        status: "FOUNDRY_FILE_READ_STATE_READY",
        format: "text/csv",
      },
      {
        id: "d_2",
        status: "FOUNDRY_FILE_READ_STATE_FAILED",
        format: "application/pdf",
        code: "unsupported",
      },
      {
        id: "d_3",
        status: "FOUNDRY_FILE_READ_STATE_FAILED",
        code: "too_large",
      },
      {
        id: "d_4",
        status: "FOUNDRY_FILE_READ_STATE_FAILED",
        format: "text/csv",
        code: "storage",
      },
    ]);
    // A read's start is its upload's event.
    expect(sent()).toEqual([
      [
        "document_read_failed",
        { format: "application/pdf", reason: "unsupported" },
      ],
      ["document_read_failed", { reason: "too_large" }],
      ["document_read_failed", { format: "text/csv", reason: "error" }],
    ]);
    expect(local.values.has(PENDING)).toBe(false);
  });

  it("reports only jobs started here, as the job they were started as", async () => {
    const a = await load();
    a.jobStarted({ job: "load" }, "x_1");
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
    a.jobStarted({ job: "load" }, undefined);
    expect(sent()).toEqual([]);
  });

  it("reports a job that ends after a reload", async () => {
    const first = await load();
    first.jobStarted({ job: "generation", source: "ai" }, "gen_1");
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
    a.jobStarted({ job: "load" }, "old");
    vi.advanceTimersByTime(8 * 24 * 60 * 60 * 1000);
    a.jobStarted({ job: "load" }, "new");
    expect(Object.keys(JSON.parse(local.values.get(PENDING)!))).toEqual([
      "new",
    ]);
  });

  it("reads what storage holds without trusting it", async () => {
    const a = await load();
    local.values.set(PENDING, "not json");
    a.jobSeen("load", [{ id: "load_1", status: "ready" }]);
    local.values.set(PENDING, "null");
    a.jobStarted({ job: "load" }, "load_1");
    // Entries that aren't jobs are dropped, never read.
    local.values.set(
      PENDING,
      JSON.stringify({ a: null, b: 5, c: { job: "load" }, d: [] }),
    );
    a.jobSeen("load", [{ id: "a", status: "ready" }]);
    a.jobStarted({ job: "load" }, "load_2");
    expect(Object.keys(JSON.parse(local.values.get(PENDING)!))).toEqual([
      "load_2",
    ]);
    // A source or mode that isn't one of its words reads as the agent's, or
    // a save.
    local.values.set(
      PENDING,
      JSON.stringify({
        gen_1: { job: "generation", source: "<b>", at: Date.now() },
        rev_1: { job: "revision", mode: "<b>", at: Date.now() },
      }),
    );
    a.jobSeen("generation", [
      { id: "gen_1", status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" },
    ]);
    a.jobSeen("revision", [
      { id: "rev_1", status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" },
    ]);
    expect(sent()).toEqual([
      ["load_started", undefined],
      ["load_started", undefined],
      ["generation_succeeded", { source: "ai" }],
      ["revision_succeeded", { mode: "save" }],
    ]);
  });

  it("still reports the start when storage is blocked", async () => {
    const a = await load("client-test", true);
    a.jobStarted({ job: "question" }, "q_1");
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

describe("TRACKER", () => {
  it("records everything, replays with the tracker's path masks", async () => {
    vi.stubEnv("NEXT_PUBLIC_DATABUDDY_CLIENT_ID", "client-test");
    vi.resetModules();
    const { MASK_PATTERNS, TRACKER, withoutTitle } = await import(
      "@/web/telemetry"
    );
    // replay.js masks the URLs it records with the tracker's maskPatterns.
    expect(TRACKER).toEqual({
      clientId: "client-test",
      maskPatterns: MASK_PATTERNS,
      filter: withoutTitle,
      trackWebVitals: true,
      trackErrors: true,
      trackOutgoingLinks: true,
      trackInteractions: true,
      replay: true,
    });
  });

  it("loads the SDK's own tracker and recorder, checked by SRI", async () => {
    vi.stubEnv("NEXT_PUBLIC_DATABUDDY_CLIENT_ID", "client-test");
    vi.resetModules();
    const { TRACKER } = await import("@/web/telemetry");
    const { createScript, createReplayScript } =
      await vi.importActual<typeof import("@databuddy/sdk")>("@databuddy/sdk");
    vi.stubGlobal("window", {});
    vi.stubGlobal("document", {
      querySelector: () => null,
      createElement: () => ({ setAttribute: () => {} }),
    });
    // Each SDK release pins its scripts' date-versioned URLs and SHA-384
    // hashes, so a change on Databuddy's CDN can't run here; a `scriptUrl`
    // in TRACKER would load bytes nothing checks.
    for (const script of [createScript(TRACKER), createReplayScript(TRACKER)])
      expect(script).toMatchObject({
        src: expect.stringMatching(
          /^https:\/\/cdn\.databuddy\.cc\/(databuddy|replay)\.\d{4}-\d{2}-\d{2}\.\d+\.js$/,
        ),
        integrity: expect.stringMatching(/^sha384-[A-Za-z0-9+/]{64}$/),
        crossOrigin: "anonymous",
      });
  });
});

describe("Helix's own people", () => {
  it("are never identified, and their browser stops sending", async () => {
    const a = await load();
    const optOut = vi.fn();
    vi.stubGlobal("window", { databuddyOptOut: optOut });
    a.identifyUser("user_1", { email: "Ada@Helix-DB.com", name: "Ada" });
    expect(sdk.identify).not.toHaveBeenCalled();
    expect(optOut).toHaveBeenCalledOnce();
    expect(a.trackingAllowed("")).toBe(false);
    // A customer on a look-alike domain is identified.
    a.identifyUser("user_2", { email: "bo@nothelix-db.com" });
    expect(sdk.identify).toHaveBeenCalledWith("user_2", {
      email: "bo@nothelix-db.com",
    });
  });

  it("are marked by ?internal=1 on any page, and unmarked by ?internal=0", async () => {
    const a = await load();
    expect(a.trackingAllowed("")).toBe(true);
    expect(a.trackingAllowed("?internal=1")).toBe(false);
    expect(a.trackingAllowed("?workspace=ws_1")).toBe(false);
    local.values.set("databuddy_opt_out", "true");
    expect(a.trackingAllowed("?internal=0")).toBe(true);
    // Unmarking undoes the tracker's own opt-out, which marking set.
    expect(local.values.has("databuddy_opt_out")).toBe(false);
    expect(a.trackingAllowed("")).toBe(true);
  });

  it("can't be told apart with storage blocked: the browser is tracked", async () => {
    const a = await load("client-test", true);
    expect(a.trackingAllowed("?internal=1")).toBe(true);
  });
});
