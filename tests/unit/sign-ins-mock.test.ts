import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { Connector, ConnectorKind, ConnectorSignIn } from "@/shared/wfe";
import type { MockReply } from "../../mock/http";
import { dataRoutes } from "../../mock/routes/data";
import { signInRoutes } from "../../mock/routes/sign-ins";
import { config, db, resetState } from "../../mock/state";

// The mock's connector sign-ins follow the backend's rules (plan
// SIGN_IN_CONNECTORS.md), so the site's flows meet the refusals and states
// the WFE will have: only the starter authorizes and finishes, a finish
// ends it once, a code works once, and a connector signed in reads with an
// account in place of a key.

const ACME = "ws_acme";
const routes = [...dataRoutes, ...signInRoutes].map((r) => ({
  ...r,
  c: compileTemplate(r.path),
}));
process.env.NEXT_PUBLIC_WORKOS_REDIRECT_URI =
  "http://localhost:3100/login/callback";
process.env.MOCK_ORIGIN = "http://127.0.0.1:4110";
const CALLBACK = "http://localhost:3100/connect/callback";

function call(
  method: HttpMethod,
  path: string,
  body?: unknown,
  userId = "usr_ada",
): MockReply {
  const url = new URL(path, "http://mock");
  for (const r of routes) {
    const params = r.method === method && matchTemplate(r.c, url.pathname);
    if (!params) continue;
    const reply = r.handle({
      method,
      path: url.pathname,
      params,
      query: url.searchParams,
      body,
      principal: { user: db.users.get(userId)!, sessionId: "s" },
    });
    if (reply instanceof Promise) throw new Error("async mock route");
    return reply;
  }
  throw new Error(`no mock route for ${method} ${path}`);
}
const ok = <T>(reply: MockReply): T => {
  expect(reply.status, JSON.stringify(reply.body)).toBe(200);
  return reply.body as T;
};
const refusal = (reply: MockReply) => {
  const body = reply.body as {
    message: string;
    details: { reason?: string }[];
  };
  return {
    status: reply.status,
    message: body.message,
    reason: body.details[0]?.reason,
  };
};
const base = `/v1/workspaces/${ACME}/foundry`;
const start = (body: object, userId = "usr_ada") =>
  call("POST", `${base}/sign-ins`, body, userId);
const started = (body: object, userId = "usr_ada") =>
  ok<{ signIn: ConnectorSignIn }>(start(body, userId)).signIn;
const authorize = (id: string, userId = "usr_ada") =>
  call(
    "POST",
    `${base}/sign-ins/${id}:authorize`,
    { returnTo: CALLBACK },
    userId,
  );
const finish = (id: string, body: object, userId = "usr_ada") =>
  call("POST", `${base}/sign-ins/${id}:finish`, body, userId);
const signInOf = (id: string) =>
  ok<{ signIn: ConnectorSignIn }>(call("GET", `${base}/sign-ins/${id}`)).signIn;

// What the consent page does on Authorize: a code for `account`, the
// browser sent back to the callback with it.
async function consent(id: string, account = "", action = "authorize") {
  const { handleConnectPage } = await import("../../mock/routes/sign-ins");
  let location = "";
  const res = {
    writeHead: (_: number, headers?: Record<string, string>) => {
      location = headers?.Location || "";
      return res;
    },
    end: () => res,
  };
  handleConnectPage(
    { method: "POST" } as never,
    res as never,
    new URL("http://127.0.0.1:4110/mock/connect"),
    new URLSearchParams({ sign_in: id, account, action }).toString(),
  );
  return new URL(location);
}
/** Starts, authorizes and finishes a GitHub sign-in; the connector it made. */
async function connectGitHub(repos: string[] = [], account = "ada") {
  const s = started({
    kind: "github",
    name: "GitHub",
    config: { github: { repos, reviewDays: 30 } },
  });
  ok(authorize(s.id));
  const back = await consent(s.id, account);
  const done = ok<{ signIn: ConnectorSignIn }>(
    finish(s.id, { connectCode: back.searchParams.get("connect_code") }),
  ).signIn;
  expect(done.status).toBe("CONNECTOR_SIGN_IN_STATUS_CONNECTED");
  return done;
}
const connector = (id: string) =>
  ok<{ connector: Connector }>(call("GET", `${base}/connectors/${id}`))
    .connector;

beforeEach(() => resetState());
afterEach(() => vi.useRealTimers());

describe("connector kinds", () => {
  it("offers every kind Pipes signs in to, with GitHub's install link", () => {
    const kinds = ok<{ kinds: ConnectorKind[] }>(
      call("GET", `${base}/connector-kinds`),
    ).kinds;
    expect(kinds.filter((k) => k.signIn).map((k) => k.kind)).toEqual([
      "stripe",
      "posthog",
      "github",
      "linear",
      "calcom",
      "slack",
      "notion",
      "google",
    ]);
    expect(kinds.find((k) => k.kind === "github")?.github?.installUrl).toMatch(
      /^https:\/\/github\.com\/apps\//,
    );
  });

  it("offers none with the switch off, and refuses a start", () => {
    Object.assign(config, { signIn: false });
    const kinds = ok<{ kinds: ConnectorKind[] }>(
      call("GET", `${base}/connector-kinds`),
    ).kinds;
    expect(kinds.some((k) => k.signIn)).toBe(false);
    expect(refusal(start({ kind: "github" }))).toMatchObject({
      status: 400,
      reason: "SIGN_IN_UNAVAILABLE",
    });
  });
});

describe("starting a sign-in", () => {
  it("is pending for 15 minutes, its starter's, with its settings", () => {
    const s = started({
      kind: "github",
      name: "Acme repos",
      config: { github: { repos: ["acme/api"], reviewDays: 30 } },
    });
    expect(s).toMatchObject({
      kind: "github",
      name: "Acme repos",
      status: "CONNECTOR_SIGN_IN_STATUS_PENDING",
      mine: true,
      config: { github: { repos: ["acme/api"], reviewDays: 30 } },
    });
    expect(Date.parse(s.expiresAt!) - Date.parse(s.createdAt!)).toBeGreaterThan(
      14 * 60_000,
    );
  });

  it("refuses a credential, a member and other settings", () => {
    expect(
      refusal(start({ kind: "github", config: { github: { token: "x" } } })),
    ).toMatchObject({ status: 400 });
    expect(
      refusal(start({ kind: "linear", config: { github: {} } })).status,
    ).toBe(400);
    expect(refusal(start({ kind: "github" }, "usr_alan")).status).toBe(403);
  });

  it("returns what a client token started", () => {
    const a = started({ kind: "linear", clientToken: "t1" }),
      b = started({ kind: "linear", clientToken: "t1" });
    expect(b.id).toBe(a.id);
  });

  it("returns it run out too, which Authorize refuses", () => {
    const a = started({ kind: "linear", clientToken: "t1" });
    // signInsRunOut runs every sign-in out at once, as 15 minutes would.
    Object.assign(config, { signInsRunOut: true });
    expect(started({ kind: "linear", clientToken: "t1" })).toMatchObject({
      id: a.id,
      status: "CONNECTOR_SIGN_IN_STATUS_EXPIRED",
    });
    expect(refusal(authorize(a.id)).reason).toBe("SIGN_IN_ENDED");
    // Once: one started after it lasts.
    const b = started({ kind: "linear", clientToken: "t2" });
    expect(b.status).toBe("CONNECTOR_SIGN_IN_STATUS_PENDING");
    ok(authorize(b.id));
  });

  it("allows five live sign-ins a user", () => {
    for (let i = 0; i < 5; i++) started({ kind: "linear" });
    expect(refusal(start({ kind: "linear" }))).toEqual({
      status: 429,
      message:
        "too many sign-ins: at most 5 at once in a workspace, and 20 an hour",
      reason: undefined,
    });
  });

  it("allows a user 20 starts an hour across every workspace", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    db.workspaces.set("ws_globex", {
      id: "ws_globex",
      slug: "globex",
      displayName: "Globex",
      status: "RESOURCE_STATUS_ACTIVE",
    });
    db.memberships.push({
      id: "mem_ada_globex",
      workspaceId: "ws_globex",
      userId: "usr_ada",
      role: "WORKSPACE_ROLE_OWNER",
      createdAt: "",
    });
    const startIn = (ws: string) =>
      call("POST", `/v1/workspaces/${ws}/foundry/sign-ins`, { kind: "linear" });
    // Five live in each workspace, twice, each five running out first.
    for (let round = 0; round < 2; round++) {
      for (let i = 0; i < 5; i++) {
        ok(startIn(ACME));
        ok(startIn("ws_globex"));
      }
      vi.setSystemTime(Date.now() + 16 * 60_000);
    }
    // None is live, but 20 started within the hour.
    expect(refusal(startIn(ACME)).status).toBe(429);
    vi.setSystemTime(Date.now() + 30 * 60_000);
    ok(startIn(ACME));
  });

  it("checks what a start is for before a client token returns what it started", () => {
    started({ kind: "linear", clientToken: "t1" });
    expect(
      refusal(
        start({
          kind: "linear",
          clientToken: "t1",
          config: { linear: { apiKey: "lin_api_123" } },
        }),
      ),
    ).toMatchObject({ status: 400 });
    expect(
      refusal(
        start({ kind: "github", connectorId: "conn_gone", clientToken: "t1" }),
      ).status,
    ).toBe(404);
  });

  it("keeps a connector's name and settings when it is signed in again", async () => {
    const { connectorId } = await connectGitHub();
    for (const body of [
      { name: "Renamed" },
      { config: { github: { repos: ["acme/api"] } } },
    ])
      expect(
        refusal(start({ kind: "github", connectorId, ...body })),
      ).toMatchObject({
        status: 400,
        message:
          "signing a connector in again keeps its name and settings; change them on its page",
      });
  });
});

describe("authorizing and finishing", () => {
  it("connects GitHub: a connector reading with the account, its first sync queued", async () => {
    const s = await connectGitHub(["acme/api"]);
    const c = connector(s.connectorId!);
    expect(c).toMatchObject({
      kind: "github",
      name: "GitHub",
      secretHint: "",
      account: {
        label: "ada",
        state: "CONNECTOR_ACCOUNT_STATE_CONNECTED",
        userId: "usr_ada",
        connectedBy: "Ada Lovelace",
      },
      config: { github: { repos: ["acme/api"], reviewDays: 30 } },
    });
    expect(c.lastSync?.status).toBe("CONNECTOR_SYNC_STATUS_QUEUED");
    expect(s.discoveryProblem).toBe("");
  });

  it("finishes once: a repeat returns it as it stands and spends no code", async () => {
    const s = started({ kind: "linear" });
    ok(authorize(s.id));
    const code = (await consent(s.id)).searchParams.get("connect_code");
    const first = ok<{ signIn: ConnectorSignIn }>(
      finish(s.id, { connectCode: code }),
    ).signIn;
    const again = ok<{ signIn: ConnectorSignIn }>(
      finish(s.id, { connectCode: code }),
    ).signIn;
    expect(again).toEqual(first);
    expect(
      ok<{ connectors: Connector[] }>(call("GET", `${base}/connectors`))
        .connectors,
    ).toHaveLength(3);
  });

  it("declines on Cancel and authorizes the same sign-in again", async () => {
    const s = started({ kind: "linear" });
    ok(authorize(s.id));
    const back = await consent(s.id, "", "cancel");
    expect(back.toString()).toBe(`${CALLBACK}?error=access_denied`);
    expect(
      ok<{ signIn: ConnectorSignIn }>(finish(s.id, { error: "access_denied" }))
        .signIn.status,
    ).toBe("CONNECTOR_SIGN_IN_STATUS_DECLINED");
    ok(authorize(s.id));
    const code = (await consent(s.id)).searchParams.get("connect_code");
    expect(
      ok<{ signIn: ConnectorSignIn }>(finish(s.id, { connectCode: code }))
        .signIn.status,
    ).toBe("CONNECTOR_SIGN_IN_STATUS_CONNECTED");
    expect(refusal(authorize(s.id)).reason).toBe("SIGN_IN_ENDED");
  });

  it("fails a lapsed code, and a callback's other errors", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const s = started({ kind: "linear" });
    ok(authorize(s.id));
    const code = (await consent(s.id)).searchParams.get("connect_code");
    vi.setSystemTime(Date.now() + 91_000);
    expect(
      ok<{ signIn: ConnectorSignIn }>(finish(s.id, { connectCode: code }))
        .signIn,
    ).toMatchObject({
      status: "CONNECTOR_SIGN_IN_STATUS_FAILED",
      failure: "CONNECTOR_SIGN_IN_FAILURE_CODE_EXPIRED",
    });
    ok(authorize(s.id));
    expect(
      ok<{ signIn: ConnectorSignIn }>(finish(s.id, { error: "server_error" }))
        .signIn.failure,
    ).toBe("CONNECTOR_SIGN_IN_FAILURE_PROVIDER_ERROR");
  });

  it("is its starter's alone, though any owner or admin reads it", () => {
    const s = started({ kind: "linear" });
    // A member manages no connectors.
    expect(refusal(authorize(s.id, "usr_alan")).status).toBe(403);
    Object.assign(config, { alanRole: "WORKSPACE_ROLE_ADMIN" });
    expect(refusal(authorize(s.id, "usr_alan")).reason).toBe(
      "SIGN_IN_NOT_YOURS",
    );
    expect(refusal(finish(s.id, { error: "x" }, "usr_alan")).reason).toBe(
      "SIGN_IN_NOT_YOURS",
    );
    expect(
      ok<{ signIn: ConnectorSignIn }>(
        call("GET", `${base}/sign-ins/${s.id}`, undefined, "usr_alan"),
      ).signIn.mine,
    ).toBe(false);
    expect(signInOf(s.id).mine).toBe(true);
  });

  it("lets one member sign a connector in again at a time", async () => {
    const { connectorId } = await connectGitHub();
    Object.assign(config, { alanRole: "WORKSPACE_ROLE_ADMIN" });
    started({ kind: "github", connectorId });
    expect(
      refusal(start({ kind: "github", connectorId }, "usr_alan")),
    ).toMatchObject({
      reason: "SIGN_IN_IN_PROGRESS",
      message:
        "Ada Lovelace is signing this connector in already; try again once they finish",
    });
  });

  it("runs out 15 minutes after its last Authorize", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const s = started({ kind: "linear" });
    vi.setSystemTime(Date.now() + 10 * 60_000);
    ok(authorize(s.id));
    vi.setSystemTime(Date.now() + 10 * 60_000);
    expect(signInOf(s.id).status).toBe("CONNECTOR_SIGN_IN_STATUS_AUTHORIZING");
    vi.setSystemTime(Date.now() + 6 * 60_000);
    expect(signInOf(s.id).status).toBe("CONNECTOR_SIGN_IN_STATUS_EXPIRED");
    expect(refusal(authorize(s.id)).reason).toBe("SIGN_IN_ENDED");
  });

  it("ends a sign-in whose connector was deleted", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const deleted = async () => {
      const { connectorId } = await connectGitHub();
      // Its first sync ends before it can be deleted.
      vi.setSystemTime(Date.now() + 5000);
      return connectorId!;
    };
    // Authorize is refused once it's gone.
    const first = await deleted();
    const pending = started({ kind: "github", connectorId: first });
    ok(call("DELETE", `${base}/connectors/${first}`));
    expect(refusal(authorize(pending.id))).toMatchObject({
      reason: "SIGN_IN_ENDED",
      message: "the connector this sign-in was for was deleted",
    });
    // Finish fails it, its code unspent.
    const second = await deleted();
    const s = started({ kind: "github", connectorId: second });
    ok(authorize(s.id));
    const code = (await consent(s.id)).searchParams.get("connect_code");
    ok(call("DELETE", `${base}/connectors/${second}`));
    expect(
      ok<{ signIn: ConnectorSignIn }>(finish(s.id, { connectCode: code }))
        .signIn,
    ).toMatchObject({
      status: "CONNECTOR_SIGN_IN_STATUS_FAILED",
      failure: "CONNECTOR_SIGN_IN_FAILURE_UNAVAILABLE",
    });
  });

  it("changes no identity for the same account in another case", async () => {
    const { connectorId } = await connectGitHub([], "ada");
    const again = started({ kind: "github", connectorId });
    ok(authorize(again.id));
    const code = (await consent(again.id, "ADA")).searchParams.get(
      "connect_code",
    );
    expect(
      ok<{ signIn: ConnectorSignIn }>(finish(again.id, { connectCode: code }))
        .signIn.identityChanged,
    ).toBe(false);
  });

  it("connects a repository the app can't see, saying so", async () => {
    const s = await connectGitHub(["acme/missing"]);
    expect(s.discoveryProblem).toMatch(
      /^Helix Foundry can't see acme\/missing: install it on acme \(https:\/\/github\.com\/apps\/.*\), then Sync now$/,
    );
  });
});

describe("a signed-in connector", () => {
  it("lets only its account's member widen what it reads", async () => {
    const { connectorId } = await connectGitHub(["acme/api", "acme/web"]);
    // Let its first sync end.
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.now() + 5000);
    connector(connectorId!);
    const patch = (repos: string[], userId = "usr_ada") =>
      call(
        "PATCH",
        `${base}/connectors/${connectorId}`,
        { config: { github: { token: "", repos, reviewDays: 30 } } },
        userId,
      );
    expect(
      refusal(
        call("PATCH", `${base}/connectors/${connectorId}`, {
          config: { github: { token: "ghp_abc", repos: [] } },
        }),
      ).message,
    ).toMatch(/sign it in again instead/);
    Object.assign(config, { alanRole: "WORKSPACE_ROLE_ADMIN" });
    // Another admin narrows it, but neither adds nor clears.
    for (const repos of [["acme/api", "acme/docs"], []])
      expect(refusal(patch(repos, "usr_alan"))).toMatchObject({
        reason: "ACCOUNT_OWNER_ONLY",
        message:
          "Only Ada Lovelace can add repositories, or read every one: GitHub reads with their GitHub access. You can narrow them",
      });
    ok(patch(["acme/api"], "usr_alan"));
    // A change of settings syncs: the next waits for it.
    vi.setSystemTime(Date.now() + 5000);
    ok(patch(["acme/api", "acme/docs"]));
    expect(connector(connectorId!).config).toEqual({
      github: { token: "", repos: ["acme/api", "acme/docs"], reviewDays: 30 },
    });
  });

  it("lets another admin narrow every repository to some: Ada left it blank; Alan restricts it to acme/api", async () => {
    const { connectorId } = await connectGitHub([]);
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.now() + 5000);
    Object.assign(config, { alanRole: "WORKSPACE_ROLE_ADMIN" });
    const patch = (repos: string[]) =>
      call(
        "PATCH",
        `${base}/connectors/${connectorId}`,
        { config: { github: { token: "", repos, reviewDays: 30 } } },
        "usr_alan",
      );
    ok(patch(["acme/api"]));
    expect(connector(connectorId!).config).toMatchObject({
      github: { repos: ["acme/api"] },
    });
    vi.setSystemTime(Date.now() + 5000);
    // Back to every repository, or another one, widens it again.
    expect(refusal(patch([])).reason).toBe("ACCOUNT_OWNER_ONLY");
    expect(refusal(patch(["acme/api", "acme/web"])).reason).toBe(
      "ACCOUNT_OWNER_ONLY",
    );
    ok(patch(["ACME/API"]));
  });

  it("breaks: frozen until it is signed in again, as another account", async () => {
    const { connectorId } = await connectGitHub();
    Object.assign(config, { accountBroken: { [connectorId!]: "member_left" } });
    const c = connector(connectorId!);
    expect(c.account).toMatchObject({
      state: "CONNECTOR_ACCOUNT_STATE_NEEDS_SIGN_IN",
      broken: "CONNECTOR_ACCOUNT_BROKEN_MEMBER_LEFT",
    });
    expect(c.lastSync?.status).toBe("CONNECTOR_SYNC_STATUS_FAILED");
    expect(c.lastSync?.error).toMatch(
      /^GitHub needs signing in again: the member who connected it left the workspace\./,
    );
    expect(
      refusal(call("POST", `${base}/connectors/${connectorId}:sync`, {}))
        .reason,
    ).toBe("ACCOUNT_NEEDS_SIGN_IN");
    // The banner's read: those that need signing in, without their tables.
    const broken = ok<{ connectors: Connector[] }>(
      call("GET", `${base}/connectors?needsSignIn=true`),
    ).connectors;
    expect(broken.map((x) => [x.id, x.tables, x.lastSync])).toEqual([
      [connectorId, [], null],
    ]);

    const again = started({ kind: "github", connectorId });
    expect(started({ kind: "github", connectorId }).id).toBe(again.id);
    ok(authorize(again.id));
    const code = (await consent(again.id, "ada-other")).searchParams.get(
      "connect_code",
    );
    const done = ok<{ signIn: ConnectorSignIn }>(
      finish(again.id, { connectCode: code }),
    ).signIn;
    expect(done).toMatchObject({
      status: "CONNECTOR_SIGN_IN_STATUS_CONNECTED",
      connectorId,
      identityChanged: true,
    });
    const fixed = connector(connectorId!);
    expect(fixed.account).toMatchObject({
      label: "ada-other",
      state: "CONNECTOR_ACCOUNT_STATE_CONNECTED",
    });
    expect(fixed.lastSync?.status).toBe("CONNECTOR_SYNC_STATUS_QUEUED");
    expect(
      ok<{ connectors: Connector[] }>(
        call("GET", `${base}/connectors?needsSignIn=true`),
      ).connectors,
    ).toEqual([]);
  });

  it("is a key connector's to keep: signing one in again is refused", () => {
    expect(
      refusal(start({ kind: "github", connectorId: "conn_acme_stripe" }))
        .status,
    ).toBe(400);
  });
});

describe("every kind that signs in", () => {
  /** Starts, authorizes as `account` (its kind's own by default) and finishes. */
  async function connect(body: object, account = "") {
    const s = started(body);
    ok(authorize(s.id));
    const code = (await consent(s.id, account)).searchParams.get(
      "connect_code",
    );
    return ok<{ signIn: ConnectorSignIn }>(finish(s.id, { connectCode: code }))
      .signIn;
  }

  it("connects each, reading with its provider's identity", async () => {
    const labels: Record<string, string> = {};
    for (const [kind, config] of [
      ["slack", { slack: { historyDays: 30 } }],
      ["notion", { notion: {} }],
      ["posthog", { posthog: { region: "us", eventDays: 30 } }],
      ["calcom", { calcom: {} }],
      ["stripe", { stripe: {} }],
      ["google", { google: { eventDays: 30 } }],
    ] as const) {
      const s = await connect({ kind, config });
      expect(s.status, kind).toBe("CONNECTOR_SIGN_IN_STATUS_CONNECTED");
      const c = connector(s.connectorId!);
      expect(c.secretHint, kind).toBe("");
      labels[kind] = c.account!.label!;
    }
    expect(labels).toEqual({
      slack: "Acme · @Helix Foundry",
      notion: "Acme",
      posthog: "ada@example.com (Acme)",
      calcom: "ada",
      stripe: "Acme",
      google: "ada@example.com",
    });
  });

  it("keeps only the settings a sign-in has", async () => {
    const google = await connect({
      kind: "google",
      config: { google: { eventDays: 45 } },
    });
    expect(connector(google.connectorId!).config).toEqual({
      google: { serviceAccountKey: "", subject: "", eventDays: 45 },
    });
    // PostHog signs in to US Cloud only: EU Cloud and self-hosted keep keys.
    for (const posthog of [
      { region: "eu" },
      { region: "custom", host: "https://posthog.example.com" },
    ])
      expect(
        refusal(start({ kind: "posthog", config: { posthog } })),
      ).toMatchObject({
        status: 400,
        message:
          "PostHog signs in to US Cloud only; EU Cloud and self-hosted PostHog connect with a personal API key",
      });
    // A credential of any kind's is refused.
    for (const config of [
      { slack: { botToken: "xoxb-1" } },
      { google: { serviceAccountKey: "{}" } },
      { notion: { token: "ntn_1" } },
    ])
      expect(
        refusal(start({ kind: Object.keys(config)[0], config })).status,
      ).toBe(400);
  });

  it("refuses a user for a signed-in Google Workspace to act as", async () => {
    const actsAs =
      "a signed-in Google Workspace connector reads as the user who signed in, and acts as no other";
    const invalid = (reply: MockReply) => {
      expect(reply.status, JSON.stringify(reply.body)).toBe(400);
      expect(refusal(reply).message).toBe(actsAs);
      // INVALID_ARGUMENT, as the backend's Prepare refusals map.
      expect((reply.body as { code: number }).code).toBe(3);
    };
    invalid(
      start({
        kind: "google",
        config: { google: { subject: "grace@acme.com", eventDays: 30 } },
      }),
    );
    // A blank one is none: the backend trims it.
    const s = await connect({
      kind: "google",
      config: { google: { subject: " ", eventDays: 30 } },
    });
    expect(s.status).toBe("CONNECTOR_SIGN_IN_STATUS_CONNECTED");
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.now() + 5000);
    const patch = (google: object) =>
      call("PATCH", `${base}/connectors/${s.connectorId}`, {
        config: { google },
      });
    invalid(patch({ subject: "Grace@Acme.com", eventDays: 30 }));
    expect(connector(s.connectorId!).config).toEqual({
      google: { serviceAccountKey: "", subject: "", eventDays: 30 },
    });
    ok(patch({ eventDays: 60 }));
  });

  it("refuses a Slack user token the broker vends", async () => {
    const s = await connect({ kind: "slack" }, "Ada (xoxp user token)");
    expect(s).toMatchObject({
      status: "CONNECTOR_SIGN_IN_STATUS_FAILED",
      failure: "CONNECTOR_SIGN_IN_FAILURE_SETTINGS_INVALID",
    });
  });

  it("lets only the account's member change a PostHog project", async () => {
    const s = await connect({
      kind: "posthog",
      config: { posthog: { region: "us", projectId: "12345", eventDays: 30 } },
    });
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.now() + 5000);
    Object.assign(config, { alanRole: "WORKSPACE_ROLE_ADMIN" });
    const patch = (posthog: object, userId = "usr_alan") =>
      call(
        "PATCH",
        `${base}/connectors/${s.connectorId}`,
        { config: { posthog: { apiKey: "", ...posthog } } },
        userId,
      );
    expect(
      refusal(patch({ region: "us", projectId: "67890", eventDays: 30 })),
    ).toMatchObject({
      reason: "ACCOUNT_OWNER_ONLY",
      message:
        "Only Ada Lovelace can change its project: PostHog reads with their PostHog access",
    });
    // Its region stays US Cloud, for anyone.
    expect(
      refusal(
        patch({ region: "eu", projectId: "12345", eventDays: 30 }, "usr_ada"),
      ).status,
    ).toBe(400);
    // Its event history is anyone's to change; the project is Ada's.
    ok(patch({ region: "us", projectId: "12345", eventDays: 60 }));
    vi.setSystemTime(Date.now() + 5000);
    ok(patch({ region: "us", projectId: "67890", eventDays: 60 }, "usr_ada"));
  });

  it("refuses a key on any signed-in connector's edit", async () => {
    const s = await connect({ kind: "slack", config: { slack: {} } });
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.now() + 5000);
    expect(
      refusal(
        call("PATCH", `${base}/connectors/${s.connectorId}`, {
          config: { slack: { botToken: "xoxb-123", historyDays: 30 } },
        }),
      ).message,
    ).toBe("this connector signs in with Slack: sign it in again instead");
    ok(
      call("PATCH", `${base}/connectors/${s.connectorId}`, {
        config: { slack: { historyDays: 30 } },
      }),
    );
  });
});
