import { afterEach, describe, expect, it, vi } from "vitest";
import {
  EVENTS_URL,
  forwardUsage,
  telemetryEnabled,
  type UsageForwarding,
} from "@/server/telemetry";
import { USAGE_LIMITS, type UsageBatch } from "@/shared/usage";

// A local install's usage analytics (src/server/telemetry.ts): on unless its
// owner turned them off, as GET /telemetry answers the page; its pages post
// their events to POST /telemetry, which forwards them to Databuddy.

describe("telemetryEnabled", () => {
  it.each([
    [{}, true],
    [{ FOUNDRY_TELEMETRY: "" }, true],
    [{ FOUNDRY_TELEMETRY: "on" }, true],
    [{ FOUNDRY_TELEMETRY: "off" }, false],
    [{ FOUNDRY_TELEMETRY: " OFF " }, false],
    [{ FOUNDRY_TELEMETRY: "false" }, false],
    [{ FOUNDRY_TELEMETRY: "no" }, false],
    [{ FOUNDRY_TELEMETRY: "0" }, false],
    [{ DO_NOT_TRACK: "1" }, false],
    [{ DO_NOT_TRACK: "true" }, false],
    [{ DO_NOT_TRACK: "0" }, true],
    [{ DO_NOT_TRACK: "false" }, true],
    [{ DO_NOT_TRACK: "" }, true],
    [{ FOUNDRY_TELEMETRY: "on", DO_NOT_TRACK: "1" }, false],
  ])("reads %o as %s", (env, enabled) => {
    expect(telemetryEnabled(env)).toBe(enabled);
  });
});

describe("GET /telemetry", () => {
  afterEach(() => vi.unstubAllEnvs());

  it("answers the site's environment when asked, never cached", async () => {
    const { GET } = await import("@/app/telemetry/route");
    vi.stubEnv("FOUNDRY_TELEMETRY", "");
    vi.stubEnv("DO_NOT_TRACK", "");
    let res = GET();
    expect(res.headers.get("cache-control")).toBe("no-store");
    expect(await res.json()).toEqual({ enabled: true });
    vi.stubEnv("FOUNDRY_TELEMETRY", "off");
    res = GET();
    expect(await res.json()).toEqual({ enabled: false });
  });
});

const ORIGIN = "http://localhost:3000";
const CONFIG: UsageForwarding = {
  origin: ORIGIN,
  clientId: "client-local",
  enabled: true,
};
const BATCH: UsageBatch = {
  visitor: "8d1f6a3e-0000-4000-8000-000000000001",
  session: "8d1f6a3e-0000-4000-8000-000000000002",
  events: [
    {
      id: "8d1f6a3e-0000-4000-8000-000000000003",
      name: "page_viewed",
      properties: { workspace_id: "ws_1" },
      path: "/ask/ont_1",
      at: 1_700_000_000_000,
    },
  ],
};

/** A post to the site's /telemetry, from its own page unless `headers` say otherwise. */
const post = (body: unknown, headers: Record<string, string> = {}) =>
  new Request(`${ORIGIN}/telemetry`, {
    method: "POST",
    headers: {
      host: "localhost:3000",
      origin: ORIGIN,
      "user-agent": "Mozilla/5.0 Test",
      ...headers,
    },
    body: typeof body === "string" ? body : JSON.stringify(body),
  });

/** Databuddy's events API, answering `answer`; what it was sent. */
const databuddy = (
  answer: () => Promise<Response> = async () => new Response("{}"),
) => {
  const calls: { url: string; init: RequestInit }[] = [];
  const upstream = vi.fn(
    async (url: string | URL | Request, init?: RequestInit) => {
      calls.push({ url: String(url), init: init ?? {} });
      return answer();
    },
  ) as unknown as typeof fetch;
  return { upstream, calls };
};

describe("forwardUsage", () => {
  it("forwards a page's events to Databuddy's events API, as the tracker would send them", async () => {
    const { upstream, calls } = databuddy();
    const res = await forwardUsage(post(BATCH), CONFIG, upstream);
    expect(res.status).toBe(204);
    expect(calls).toHaveLength(1);
    expect(calls[0].url).toBe(`${EVENTS_URL}?website_id=client-local`);
    expect(calls[0].init.method).toBe("POST");
    expect(calls[0].init.headers).toEqual({
      "content-type": "application/json",
      "user-agent": "Mozilla/5.0 Test",
    });
    expect(JSON.parse(calls[0].init.body as string)).toEqual([
      {
        eventId: BATCH.events[0].id,
        name: "page_viewed",
        properties: { workspace_id: "ws_1" },
        // Masked again: whatever the page sent, no id leaves the machine.
        path: `${ORIGIN}/ask/*`,
        anonymousId: BATCH.visitor,
        sessionId: BATCH.session,
        anonymizeVisitorIds: true,
        timestamp: 1_700_000_000_000,
        source: "browser",
      },
    ]);
  });

  it("sends no User-Agent of its own when the page sent none", async () => {
    const { upstream, calls } = databuddy();
    const request = post(BATCH);
    request.headers.delete("user-agent");
    await forwardUsage(request, CONFIG, upstream);
    expect(calls[0].init.headers).toEqual({
      "content-type": "application/json",
    });
  });

  it.each<[string, Record<string, string>, string?]>([
    ["another host", { host: "127.0.0.1:3000" }],
    ["another site's page", { origin: "https://evil.example" }],
    ["no origin", {}, "origin"],
  ])("refuses a post from %s", async (_, headers, without) => {
    const { upstream, calls } = databuddy();
    const request = post(BATCH, headers);
    if (without) request.headers.delete(without);
    expect((await forwardUsage(request, CONFIG, upstream)).status).toBe(403);
    expect(calls).toEqual([]);
  });

  it("forwards nothing once its owner turned reporting off", async () => {
    const { upstream, calls } = databuddy();
    const res = await forwardUsage(
      post(BATCH),
      { ...CONFIG, enabled: false },
      upstream,
    );
    expect(res.status).toBe(204);
    expect(calls).toEqual([]);
  });

  it("refuses a post over its size, by its length or its body", async () => {
    const { upstream, calls } = databuddy();
    const big = "x".repeat(USAGE_LIMITS.bytes + 1);
    expect(
      (
        await forwardUsage(
          post("{}", { "content-length": String(USAGE_LIMITS.bytes + 1) }),
          CONFIG,
          upstream,
        )
      ).status,
    ).toBe(413);
    expect((await forwardUsage(post(big), CONFIG, upstream)).status).toBe(413);
    expect(calls).toEqual([]);
  });

  it.each([
    ["not JSON", "{"],
    ["not a batch", { ...BATCH, visitor: "" }],
    ["an event that isn't one", { ...BATCH, events: [{ name: "x" }] }],
  ])("refuses %s", async (_, body) => {
    const { upstream, calls } = databuddy();
    expect((await forwardUsage(post(body), CONFIG, upstream)).status).toBe(400);
    expect(calls).toEqual([]);
  });

  it("forwards nothing for no events", async () => {
    const { upstream, calls } = databuddy();
    const res = await forwardUsage(
      post({ ...BATCH, events: [] }),
      CONFIG,
      upstream,
    );
    expect(res.status).toBe(204);
    expect(calls).toEqual([]);
  });

  it("answers the page the same when Databuddy fails or refuses", async () => {
    for (const answer of [
      async () => {
        throw new TypeError("fetch failed");
      },
      async () => new Response("{}", { status: 400 }),
    ]) {
      const { upstream, calls } = databuddy(answer);
      expect((await forwardUsage(post(BATCH), CONFIG, upstream)).status).toBe(
        204,
      );
      expect(calls).toHaveLength(1);
    }
  });
});

describe("POST /telemetry", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.unstubAllGlobals();
    vi.resetModules();
  });

  /** route.ts as a build of `mode` with `clientId` loads it, the site set up as a local install's. */
  const route = async (mode: string, clientId: string) => {
    vi.stubEnv("NEXT_PUBLIC_FOUNDRY_MODE", mode);
    vi.stubEnv("NEXT_PUBLIC_DATABUDDY_CLIENT_ID", clientId);
    vi.stubEnv("HELIX_WFE_ORIGIN", "http://pod:8081");
    vi.stubEnv("FOUNDRY_ORIGIN", ORIGIN);
    vi.stubEnv("FOUNDRY_LOCAL_TOKEN", "t".repeat(32));
    vi.stubEnv("FOUNDRY_TELEMETRY", "");
    vi.stubEnv("DO_NOT_TRACK", "");
    vi.resetModules();
    const { upstream, calls } = databuddy();
    vi.stubGlobal("fetch", upstream);
    return { POST: (await import("@/app/telemetry/route")).POST, calls };
  };

  it("forwards a local install's events under its client id", async () => {
    const { POST, calls } = await route("local", "client-local");
    expect((await POST(post(BATCH))).status).toBe(204);
    expect(calls.map((call) => call.url)).toEqual([
      `${EVENTS_URL}?website_id=client-local`,
    ]);
  });

  it("forwards nothing once its owner turned reporting off", async () => {
    const { POST, calls } = await route("local", "client-local");
    vi.stubEnv("DO_NOT_TRACK", "1");
    expect((await POST(post(BATCH))).status).toBe(204);
    expect(calls).toEqual([]);
  });

  it.each([
    ["the hosted site", "", "client-local"],
    ["a local build without a client id", "local", ""],
  ])("isn't there on %s", async (_, mode, clientId) => {
    const { POST, calls } = await route(mode, clientId);
    expect((await POST(post(BATCH))).status).toBe(404);
    expect(calls).toEqual([]);
  });

  it("isn't there while the local install isn't set up", async () => {
    const { POST } = await route("local", "client-local");
    vi.stubEnv("FOUNDRY_ORIGIN", "");
    expect((await POST(post(BATCH))).status).toBe(404);
  });
});
