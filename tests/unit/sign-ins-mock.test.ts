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
  it("offers GitHub and Linear by signing in, with GitHub's install link", () => {
    const kinds = ok<{ kinds: ConnectorKind[] }>(
      call("GET", `${base}/connector-kinds`),
    ).kinds;
    expect(kinds.filter((k) => k.signIn).map((k) => k.kind)).toEqual([
      "github",
      "linear",
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

  it("allows five live sign-ins a user", () => {
    for (let i = 0; i < 5; i++) started({ kind: "linear" });
    expect(refusal(start({ kind: "linear" })).status).toBe(429);
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
        "Ada Lovelace is signing GitHub in again; wait for them to finish",
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
          "only Ada Lovelace, whose GitHub access it reads with, can add repositories or read every one",
      });
    ok(patch(["acme/api"], "usr_alan"));
    // A change of settings syncs: the next waits for it.
    vi.setSystemTime(Date.now() + 5000);
    ok(patch(["acme/api", "acme/docs"]));
    expect(connector(connectorId!).config).toEqual({
      github: { token: "", repos: ["acme/api", "acme/docs"], reviewDays: 30 },
    });
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
