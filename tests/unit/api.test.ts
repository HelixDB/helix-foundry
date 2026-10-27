import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The browser's API client: CSRF on unsafe methods (renewed once when the BFF
// refuses it), sign-in on 401, and the WFE's error message on failure.

type Call = { url: string; init: RequestInit };
let calls: Call[], replies: Response[], assigned: string[];

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });

beforeEach(() => {
  calls = [];
  replies = [];
  assigned = [];
  vi.resetModules();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      calls.push({ url, init });
      return replies.shift() ?? json(500, { message: "no reply queued" });
    }),
  );
  vi.stubGlobal("window", {
    location: {
      pathname: "/data",
      search: "?tab=tables",
      hash: "",
      assign: (to: string) => assigned.push(to),
    },
  });
});
afterEach(() => vi.unstubAllGlobals());

const load = () => import("@/web/api");
const header = (c: Call, name: string) =>
  (c.init.headers as Record<string, string> | undefined)?.[name];

describe("api()", () => {
  it("sends GETs to /papi without a CSRF token", async () => {
    const { api } = await load();
    replies.push(json(200, { user: { id: "u" } }));
    expect(await api("/v1/whoami")).toEqual({ user: { id: "u" } });
    expect(calls).toHaveLength(1);
    expect(calls[0]!.url).toBe("/papi/v1/whoami");
    expect(calls[0]!.init.method).toBe("GET");
    expect(header(calls[0]!, "x-helix-csrf")).toBeUndefined();
  });

  it("fetches one CSRF token and sends it with every unsafe request", async () => {
    const { api } = await load();
    replies.push(
      json(200, { token: "t1", expiresInSeconds: 3600 }),
      json(200, { id: "ws_1" }),
      json(200, { id: "ws_2" }),
    );
    await api("/v1/workspaces", { slug: "a" });
    await api("/v1/workspaces", { slug: "b" });
    expect(calls.map((c) => c.url)).toEqual([
      "/papi/csrf",
      "/papi/v1/workspaces",
      "/papi/v1/workspaces",
    ]);
    expect(header(calls[1]!, "x-helix-csrf")).toBe("t1");
    expect(header(calls[2]!, "x-helix-csrf")).toBe("t1");
    expect(calls[1]!.init.body).toBe(JSON.stringify({ slug: "a" }));
  });

  it("renews a refused CSRF token once", async () => {
    const { api } = await load();
    replies.push(
      json(200, { token: "old", expiresInSeconds: 3600 }),
      json(403, { code: "CSRF_REJECTED", message: "CSRF validation failed." }),
      json(200, { token: "new", expiresInSeconds: 3600 }),
      json(200, { ok: true }),
    );
    expect(await api("/v1/workspaces", {})).toEqual({ ok: true });
    expect(header(calls[3]!, "x-helix-csrf")).toBe("new");
  });

  it("does not retry other refusals", async () => {
    const { api, ApiError } = await load();
    replies.push(
      json(200, { token: "t", expiresInSeconds: 3600 }),
      json(403, { code: 7, message: "permission denied" }),
    );
    const error = await api("/v1/workspaces", {}).catch((e) => e);
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({
      status: 403,
      code: 7,
      message: "permission denied",
    });
    expect(calls).toHaveLength(2);
  });

  it("keeps the WFE's error details", async () => {
    const { api } = await load();
    const details = [
      {
        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
        reason: "FOUNDRY_PRO_REQUIRED",
      },
    ];
    replies.push(
      json(400, { code: 9, message: "needs Pro", details }),
      json(400, { code: 9, message: "no details", details: "x" }),
    );
    await expect(api("/v1/foundry/plan")).rejects.toMatchObject({
      code: 9,
      details,
    });
    const error = await api("/v1/foundry/plan").catch((e) => e);
    expect(error.details).toBeUndefined();
  });

  it("goes to sign-in on 401, back to the current page", async () => {
    const { api } = await load();
    replies.push(json(401, { code: "UNAUTHENTICATED" }));
    await expect(api("/v1/workspaces")).rejects.toMatchObject({ status: 401 });
    expect(assigned).toEqual(["/login?return_to=%2Fdata%3Ftab%3Dtables"]);
  });

  it("goes to sign-in when the CSRF handshake finds no session", async () => {
    const { api } = await load();
    replies.push(json(401, { code: "UNAUTHENTICATED", message: "Sign in." }));
    await expect(api("/v1/workspaces", {})).rejects.toMatchObject({
      status: 401,
    });
    expect(assigned).toEqual(["/login?return_to=%2Fdata%3Ftab%3Dtables"]);
  });

  it("leaves a 401 to the caller when asked", async () => {
    const { api } = await load();
    replies.push(json(401, { code: "UNAUTHENTICATED" }));
    await expect(
      api("/v1/whoami", undefined, "GET", undefined, { signIn: false }),
    ).rejects.toMatchObject({ status: 401 });
    expect(assigned).toEqual([]);
  });

  it("accepts an empty 204", async () => {
    const { api } = await load();
    replies.push(
      json(200, { token: "t", expiresInSeconds: 3600 }),
      new Response(null, { status: 204 }),
    );
    expect(await api("/v1/x", undefined, "DELETE")).toBeUndefined();
  });
});

describe("session refresh", () => {
  // A reply the test releases by hand, so it can see which calls are in flight.
  const held = () => {
    const pending: { url: string; release: (r: Response) => void }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(
        (url: string) =>
          new Promise<Response>((release) => pending.push({ url, release })),
      ),
    );
    return pending;
  };
  const expiresIn = (seconds: number) =>
    new Response("{}", {
      headers: {
        "content-type": "application/json",
        "x-foundry-session-expires-in": String(seconds),
      },
    });
  const settle = () => new Promise((r) => setTimeout(r, 0));

  it("sends calls one at a time until one reports a fresh token", async () => {
    const { api } = await load();
    const pending = held();
    const a = api("/v1/whoami"),
      b = api("/v1/workspaces");
    await settle();
    // The first may refresh the session; the second waits for it.
    expect(pending.map((p) => p.url)).toEqual(["/papi/v1/whoami"]);
    pending[0]!.release(expiresIn(300));
    await a;
    await settle();
    expect(pending).toHaveLength(2);
    pending[1]!.release(expiresIn(299));
    await b;
    // Known fresh: calls go together.
    const c = api("/v1/whoami"),
      d = api("/v1/workspaces");
    await settle();
    expect(pending).toHaveLength(4);
    pending[2]!.release(expiresIn(298));
    pending[3]!.release(expiresIn(298));
    await Promise.all([c, d]);
  });

  it("takes turns again when the token is nearly out or the session ends", async () => {
    const { api } = await load();
    const pending = held();
    const a = api("/v1/whoami");
    await settle();
    // Five seconds left is inside the margin: still one at a time.
    pending[0]!.release(expiresIn(5));
    await a;
    const b = api("/v1/whoami"),
      c = api("/v1/whoami");
    await settle();
    expect(pending).toHaveLength(2);
    pending[1]!.release(expiresIn(300));
    await b;
    await settle();
    pending[2]!.release(new Response(null, { status: 401 }));
    await expect(c).rejects.toMatchObject({ status: 401 });
    // A 401 forgets the token: the next calls take turns again.
    const d = api("/v1/whoami", undefined, "GET", undefined, { signIn: false }),
      e = api("/v1/whoami", undefined, "GET", undefined, { signIn: false });
    await settle();
    expect(pending).toHaveLength(4);
    pending[3]!.release(expiresIn(300));
    await d;
    await settle();
    pending[4]!.release(expiresIn(300));
    await e;
  });

  it("lets an older token's late answer not shorten a newer one", async () => {
    const { api } = await load();
    const pending = held();
    const a = api("/v1/whoami");
    await settle();
    pending[0]!.release(expiresIn(300));
    await a;
    const b = api("/v1/whoami"),
      c = api("/v1/whoami");
    await settle();
    pending[2]!.release(expiresIn(300));
    pending[1]!.release(expiresIn(5));
    await Promise.all([b, c]);
    const d = api("/v1/whoami"),
      e = api("/v1/whoami");
    await settle();
    expect(pending).toHaveLength(5);
    pending[3]!.release(expiresIn(300));
    pending[4]!.release(expiresIn(300));
    await Promise.all([d, e]);
  });
});

describe("a local build", () => {
  beforeEach(() => vi.stubEnv("NEXT_PUBLIC_FOUNDRY_MODE", "local"));
  afterEach(() => vi.unstubAllEnvs());

  it("sends calls together: its token never runs out", async () => {
    const { api } = await load();
    const pending: ((r: Response) => void)[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise<Response>((release) => pending.push(release))),
    );
    const a = api("/v1/whoami"),
      b = api("/v1/workspaces");
    await new Promise((r) => setTimeout(r, 0));
    expect(pending).toHaveLength(2);
    for (const release of pending) release(json(200, {}));
    await Promise.all([a, b]);
  });

  it("reports a 401 without leaving the page: there is no sign-in", async () => {
    const { api } = await load();
    replies.push(json(401, { code: 16, message: "unauthenticated" }));
    await expect(api("/v1/workspaces")).rejects.toMatchObject({
      status: 401,
      message: "unauthenticated",
    });
    replies.push(json(401, { code: "UNAUTHENTICATED" }));
    await expect(api("/v1/workspaces", { slug: "a" })).rejects.toMatchObject({
      status: 401,
    });
    expect(assigned).toEqual([]);
  });
});

describe("signOut()", () => {
  it("posts with the CSRF token and follows the WorkOS logout URL", async () => {
    const { signOut } = await load();
    replies.push(
      json(200, { token: "t", expiresInSeconds: 3600 }),
      json(200, { logoutUrl: "https://api.workos.test/logout" }),
    );
    await signOut();
    expect(calls[1]!.url).toBe("/auth/logout");
    expect(header(calls[1]!, "x-helix-csrf")).toBe("t");
    expect(assigned).toEqual(["https://api.workos.test/logout"]);
  });

  it("still lands on sign-in without a session", async () => {
    const { signOut } = await load();
    replies.push(json(401, { code: "UNAUTHENTICATED" }));
    await signOut();
    expect(assigned).toEqual(["/login"]);
  });
});

describe("sentence()", () => {
  it("makes the WFE's fragments sentences, and keeps its sentences as written", async () => {
    const { sentence } = await load();
    expect(sentence("the load is gone")).toBe("The load is gone.");
    expect(sentence("Claude is unavailable; Helix will retry")).toBe(
      "Claude is unavailable; Helix will retry.",
    );
    // A workspace provider's copy may start with a model's name.
    for (const written of [
      "claude-opus-5-5 can’t do what this task needs (tool calls or structured answers).",
      "Claude refused the API key. An owner can enter a new one in Settings › AI models.",
      "Is it there?",
      "It stopped!",
    ])
      expect(sentence(written)).toBe(written);
  });
});
