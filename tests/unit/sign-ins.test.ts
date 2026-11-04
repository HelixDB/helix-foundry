import { describe, expect, it } from "vitest";
import type { Connector, ConnectorAccount } from "@/shared/wfe";
import { ApiError } from "@/web/api";
import { formatDate } from "@/web/kit/util";
import { paths } from "@/web/paths";
import { connectorBadge } from "@/web/areas/data/model";
import {
  CONNECT_KEY,
  accountLine,
  breakKey,
  brokenNotice,
  brokenWhy,
  callbackUrl,
  captureCallback,
  codeStale,
  connectedPath,
  failureCode,
  isProviderUrl,
  needsSignIn,
  readConnect,
  readPicks,
  signInEnded,
  signInFailure,
  startAgainPath,
  startAndAuthorize,
  isSignInKind,
  keyInstead,
  providerName,
  widenRefusal,
  widensReach,
  widensRepos,
  withConnected,
  writeConnect,
  type ConnectEntry,
  type ReturnPicks,
} from "@/web/areas/data/sign-ins";

// Connecting by signing in at the provider, in the browser: what the tab
// keeps for the callback, the callback's URL clean-up before any call,
// where it returns, and the words for each outcome and for an account that
// needs signing in again (src/web/areas/data/sign-ins.ts).

/** A Storage that holds values in a Map, or throws like a blocked one. */
function storage(blocked = false): Storage {
  const values = new Map<string, string>();
  const guard = () => {
    if (blocked) throw new DOMException("blocked", "SecurityError");
  };
  return {
    get length() {
      return values.size;
    },
    clear: () => values.clear(),
    key: (i) => [...values.keys()][i] ?? null,
    getItem: (k) => (guard(), values.get(k) ?? null),
    setItem: (k, v) => (guard(), void values.set(k, v)),
    removeItem: (k) => (guard(), void values.delete(k)),
  };
}

const ENTRY: ConnectEntry = {
  workspaceId: "ws_acme",
  signInId: "csi_1",
  kind: "github",
  returnPath: "/ontology/new?step=sources",
};

describe("the tab's sign-in", () => {
  it("reads back what it wrote, and nothing malformed", () => {
    const s = storage();
    expect(readConnect(s)).toBeNull();
    writeConnect(s, ENTRY);
    expect(readConnect(s)).toEqual({ ...ENTRY });
    for (const bad of [
      "not json",
      "[]",
      JSON.stringify({ workspaceId: "ws", kind: "github" }),
      JSON.stringify({ ...ENTRY, signInId: 7 }),
    ]) {
      s.setItem(CONNECT_KEY, bad);
      expect(readConnect(s), bad).toBeNull();
    }
    // Fields of the wrong type are dropped, not trusted.
    s.setItem(CONNECT_KEY, JSON.stringify({ ...ENTRY, at: "now", code: 1 }));
    expect(readConnect(s)).toEqual({ ...ENTRY });
  });

  it("does without storage", () => {
    const s = storage(true);
    writeConnect(s, ENTRY);
    expect(readConnect(s)).toBeNull();
  });
});

describe("the callback's URL", () => {
  const capture = (url: string, s: Storage, now = 1000) => {
    const replaced: string[] = [];
    const u = new URL(url, "https://foundry.helix-db.com");
    const held = captureCallback(u, s, (p) => replaced.push(p), now);
    return { held, replaced };
  };

  it("moves the code into the tab's sign-in with the time, and leaves a bare path", () => {
    const s = storage();
    writeConnect(s, ENTRY);
    const { held, replaced } = capture(
      "/connect/callback?connect_code=abc123",
      s,
      5000,
    );
    expect(held).toBe(true);
    expect(replaced).toEqual(["/connect/callback"]);
    expect(readConnect(s)).toEqual({ ...ENTRY, code: "abc123", at: 5000 });
  });

  it("keeps an answer without a code as its error", () => {
    const s = storage();
    writeConnect(s, { ...ENTRY, code: "old", at: 1 });
    capture("/connect/callback?error=access_denied&state=x", s, 9);
    expect(readConnect(s)).toEqual({
      ...ENTRY,
      error: "access_denied",
      at: 9,
    });
  });

  it("cleans the URL of a tab that started no sign-in, keeping nothing", () => {
    const s = storage();
    const { replaced } = capture("/connect/callback?connect_code=abc", s);
    expect(replaced).toEqual(["/connect/callback"]);
    expect(readConnect(s)).toBeNull();
  });

  it("leaves every other page, and a bare callback, alone", () => {
    const s = storage();
    writeConnect(s, ENTRY);
    for (const url of [
      "/data?connect_code=abc",
      "/connect/callbacks?connect_code=abc",
      "/connect/callback",
    ])
      expect(capture(url, s).replaced, url).toEqual([]);
    expect(readConnect(s)).toEqual(ENTRY);
  });

  it("says when its code is older than a code lasts", () => {
    expect(codeStale({ ...ENTRY, code: "c", at: 0 }, 90_000)).toBe(false);
    expect(codeStale({ ...ENTRY, code: "c", at: 0 }, 90_001)).toBe(true);
    expect(codeStale({ ...ENTRY, error: "access_denied", at: 0 }, 1e9)).toBe(
      false,
    );
  });
});

describe("where the provider returns", () => {
  it("is the stamp's callback, on its WorkOS redirect URI's origin", () => {
    expect(
      callbackUrl(
        "https://dev.foundry.helix-db.com/login/callback",
        "https://foundry-git-x.vercel.app",
      ),
    ).toBe("https://dev.foundry.helix-db.com/connect/callback");
    expect(callbackUrl("", "http://localhost:3100")).toBe(
      "http://localhost:3100/connect/callback",
    );
  });

  it("is opened over https only, or the mock's loopback page", () => {
    const page = "https://foundry.helix-db.com/data";
    expect(isProviderUrl("https://github.com/login/oauth/x", page)).toBe(true);
    expect(isProviderUrl("http://github.com/login/oauth/x", page)).toBe(false);
    expect(isProviderUrl("javascript:alert(1)", page)).toBe(false);
    expect(isProviderUrl("not a url", page)).toBe(false);
    expect(
      isProviderUrl(
        "http://127.0.0.1:4110/mock/connect",
        "http://localhost:3100/data",
      ),
    ).toBe(true);
    expect(isProviderUrl("http://127.0.0.1:4110/mock/connect", page)).toBe(
      false,
    );
    expect(
      isProviderUrl("http://evil.example/x", "http://localhost:3100/data"),
    ).toBe(false);
  });
});

describe("starting a form's sign-in", () => {
  const refused = (reason: string) =>
    new ApiError("refused", 400, 9, [
      { "@type": "type.googleapis.com/google.rpc.ErrorInfo", reason },
    ]);

  /**
   * The WFE's start and Authorize: a client token seen before returns the
   * sign-in it started, whatever its state, and an ended one isn't
   * authorized.
   */
  function wfe(ended: string[] = []) {
    const byToken = new Map<string, string>(),
      calls: string[] = [];
    let n = 0;
    return {
      calls,
      ended,
      start: async (token?: string) => {
        calls.push(`start ${token ?? "-"}`);
        const id = (token && byToken.get(token)) || `csi_${++n}`;
        if (token) byToken.set(token, id);
        return id;
      },
      authorize: async (id: string) => {
        calls.push(`authorize ${id}`);
        if (ended.includes(id)) throw refused("SIGN_IN_ENDED");
        return { url: `https://provider.example/${id}` };
      },
    };
  }
  const renewing = () => {
    let n = 0;
    return () => `token-${++n}`;
  };

  it("tells an ended sign-in's refusal from others", () => {
    expect(signInEnded(refused("SIGN_IN_ENDED"))).toBe(true);
    expect(signInEnded(refused("SIGN_IN_IN_PROGRESS"))).toBe(false);
    expect(signInEnded(new ApiError("gone", 400))).toBe(false);
    expect(signInEnded(new Error("SIGN_IN_ENDED"))).toBe(false);
  });

  it("finds the sign-in its client token started, while it lasts", async () => {
    const w = wfe(),
      renew = renewing();
    await startAndAuthorize("form", w.start, w.authorize, renew);
    const again = await startAndAuthorize("form", w.start, w.authorize, renew);
    expect(again).toEqual({
      signInId: "csi_1",
      authorized: { url: "https://provider.example/csi_1" },
    });
    expect(w.calls).toEqual([
      "start form",
      "authorize csi_1",
      "start form",
      "authorize csi_1",
    ]);
  });

  it("starts one under a new token, once, after its sign-in ended", async () => {
    const w = wfe(),
      renew = renewing();
    await startAndAuthorize("form", w.start, w.authorize, renew);
    // Its 15 minutes ran out while the form stayed open.
    w.ended.push("csi_1");
    expect(
      await startAndAuthorize("form", w.start, w.authorize, renew),
    ).toEqual({
      signInId: "csi_2",
      authorized: { url: "https://provider.example/csi_2" },
    });
    expect(w.calls.slice(2)).toEqual([
      "start form",
      "authorize csi_1",
      "start token-1",
      "authorize csi_2",
    ]);
    // Only once: a second ended sign-in is the refusal.
    w.ended.push("csi_2", "csi_3");
    await expect(
      startAndAuthorize("token-1", w.start, w.authorize, renew),
    ).rejects.toSatisfy(signInEnded);
    expect(w.calls.slice(6)).toEqual([
      "start token-1",
      "authorize csi_2",
      "start token-2",
      "authorize csi_3",
    ]);
  });

  it("retries nothing else, nor a start without a token", async () => {
    const w = wfe(["csi_1"]),
      renew = renewing();
    await expect(
      startAndAuthorize(undefined, w.start, w.authorize, renew),
    ).rejects.toSatisfy(signInEnded);
    expect(w.calls).toEqual(["start -", "authorize csi_1"]);
    const busy = refused("SIGN_IN_IN_PROGRESS");
    await expect(
      startAndAuthorize(
        "form",
        w.start,
        async () => {
          throw busy;
        },
        renew,
      ),
    ).rejects.toBe(busy);
    expect(w.calls).toEqual(["start -", "authorize csi_1", "start form"]);
  });
});

describe("once connected", () => {
  it("returns where it was started, with the connector chosen, in its workspace", () => {
    expect(connectedPath(ENTRY, "conn_9", paths.connection)).toBe(
      "/ontology/new?step=sources&connected=conn_9&workspace=ws_acme",
    );
    expect(
      connectedPath(
        {
          ...ENTRY,
          returnPath: "/ontology/ont_1/generate?goal=Orders&connectors=conn_1",
        },
        "conn_9",
        paths.connection,
      ),
    ).toBe(
      "/ontology/ont_1/generate?goal=Orders&connectors=conn_1&connected=conn_9&workspace=ws_acme",
    );
  });

  it("opens the connector's page from Data, or for an unsafe return", () => {
    for (const returnPath of [
      undefined,
      "https://evil.example/x",
      "//evil.example",
      "/connect/callback",
      "/login?return_to=/x",
    ])
      expect(
        connectedPath({ ...ENTRY, returnPath }, "conn_9", paths.connection),
        String(returnPath),
      ).toBe("/data/connections/conn_9?workspace=ws_acme");
  });

  it("starts again where it can", () => {
    expect(startAgainPath(ENTRY, paths)).toBe(
      "/ontology/new?step=sources&workspace=ws_acme",
    );
    expect(startAgainPath({ ...ENTRY, connectorId: "conn_2" }, paths)).toBe(
      "/data/connections/conn_2?workspace=ws_acme",
    );
    expect(
      startAgainPath({ ...ENTRY, returnPath: "/connect/callback" }, paths),
    ).toBe("/data?add=github&workspace=ws_acme");
  });
});

describe("a sign-in that didn't connect", () => {
  const f = (
    status: Parameters<typeof signInFailure>[0]["status"],
    failure?: Parameters<typeof signInFailure>[0]["failure"],
    stale = false,
  ) => signInFailure({ kind: "github", status, failure }, stale);

  it("is worded from its code, never the provider's text", () => {
    expect(f("CONNECTOR_SIGN_IN_STATUS_DECLINED")).toMatchObject({
      title: "GitHub wasn’t authorized",
      retry: "again",
    });
    expect(f("CONNECTOR_SIGN_IN_STATUS_EXPIRED")).toMatchObject({
      title: "This sign-in ran out: start again",
      retry: "start",
    });
    expect(
      f(
        "CONNECTOR_SIGN_IN_STATUS_FAILED",
        "CONNECTOR_SIGN_IN_FAILURE_CODE_EXPIRED",
      ).title,
    ).toBe("The sign-in took too long");
    expect(
      f(
        "CONNECTOR_SIGN_IN_STATUS_FAILED",
        "CONNECTOR_SIGN_IN_FAILURE_PROVIDER_ERROR",
        true,
      ).title,
    ).toBe("The sign-in took too long");
    expect(
      f(
        "CONNECTOR_SIGN_IN_STATUS_FAILED",
        "CONNECTOR_SIGN_IN_FAILURE_SETTINGS_INVALID",
      ).retry,
    ).toBe("start");
    for (const failure of [
      "CONNECTOR_SIGN_IN_FAILURE_PROVIDER_ERROR",
      "CONNECTOR_SIGN_IN_FAILURE_NOT_THIS_WORKSPACE",
      "CONNECTOR_SIGN_IN_FAILURE_TOKEN_REJECTED",
      "CONNECTOR_SIGN_IN_FAILURE_UNAVAILABLE",
      "CONNECTOR_SIGN_IN_FAILURE_UNSPECIFIED",
    ] as const) {
      const w = f("CONNECTOR_SIGN_IN_STATUS_FAILED", failure);
      expect(w.retry, failure).toBe("again");
      expect(w.text, failure).toMatch(/\.$/);
    }
    expect(f("CONNECTOR_SIGN_IN_STATUS_AUTHORIZING").title).toBe(
      "The sign-in didn’t finish",
    );
  });
});

describe("an account", () => {
  const github = (account: ConnectorAccount, name = "Acme GitHub") =>
    ({ id: "conn_1", kind: "github", name, account }) satisfies Connector;
  const broken = (
    broken: ConnectorAccount["broken"],
    connectedBy = "Ada Lovelace",
  ): ConnectorAccount => ({
    state: "CONNECTOR_ACCOUNT_STATE_NEEDS_SIGN_IN",
    broken,
    brokenAt: "2026-11-02T14:05:00Z",
    connectedBy,
    label: "ada-l",
  });

  it("says whose access it reads with", () => {
    expect(
      accountLine(github({ connectedBy: "Ada Lovelace", label: "ada-l" })),
    ).toBe("Reads with Ada Lovelace’s GitHub access (ada-l)");
    expect(
      accountLine({
        kind: "linear",
        account: { connectedBy: "a former member", label: "" },
      }),
    ).toBe("Reads with a former member’s Linear access");
    expect(accountLine({ kind: "github" })).toBe("");
  });

  it("needs signing in again, and says why", () => {
    expect(
      needsSignIn(github(broken("CONNECTOR_ACCOUNT_BROKEN_EXPIRED"))),
    ).toBe(true);
    expect(
      needsSignIn(github({ state: "CONNECTOR_ACCOUNT_STATE_CONNECTED" })),
    ).toBe(false);
    expect(needsSignIn({})).toBe(false);
    expect(
      brokenWhy(github(broken("CONNECTOR_ACCOUNT_BROKEN_MEMBER_LEFT"))),
    ).toBe("Ada Lovelace, who connected it, left the workspace.");
    expect(
      brokenWhy(
        github(
          broken("CONNECTOR_ACCOUNT_BROKEN_MEMBER_LEFT", "a former member"),
        ),
      ),
    ).toBe("The member who connected it left the workspace.");
    expect(
      brokenWhy(github(broken("CONNECTOR_ACCOUNT_BROKEN_ACCESS_REVOKED"))),
    ).toBe("GitHub no longer accepts the access it was given.");
    expect(
      brokenWhy(github(broken("CONNECTOR_ACCOUNT_BROKEN_ACCOUNT_MISSING"))),
    ).toBe("Its sign-in with GitHub no longer exists.");
  });

  it("is one notice for one connector, and one for several", () => {
    const one = brokenNotice([
      github(broken("CONNECTOR_ACCOUNT_BROKEN_MEMBER_LEFT")),
    ]);
    expect(one.title).toBe("Acme GitHub stopped syncing");
    expect(one.text).toMatch(
      /^Ada Lovelace, who connected it, left the workspace\. Its data is frozen as of .+\.$/,
    );
    const several = brokenNotice([
      github(broken("CONNECTOR_ACCOUNT_BROKEN_MEMBER_LEFT")),
      {
        ...github(broken("CONNECTOR_ACCOUNT_BROKEN_EXPIRED")),
        id: "c2",
        kind: "linear",
        name: "",
      },
      {
        ...github(broken("CONNECTOR_ACCOUNT_BROKEN_EXPIRED")),
        id: "c3",
        name: "Docs",
      },
    ]);
    expect(several).toEqual({
      title: "3 connections stopped syncing",
      text: "Acme GitHub, Linear and Docs need signing in again. Their data is frozen as of their last syncs.",
    });
  });

  it("dates frozen data by its last sync, not by the break", () => {
    const c = {
      ...github(broken("CONNECTOR_ACCOUNT_BROKEN_ACCESS_REVOKED")),
      tables: [
        { name: "issues", syncedAt: "2026-11-01T09:00:00Z" },
        { name: "projects", syncedAt: "2026-11-02T08:30:00Z" },
      ],
    };
    const { text } = brokenNotice([c]);
    expect(text).toContain(
      `frozen as of ${formatDate("2026-11-02T08:30:00Z")}.`,
    );
    expect(text).not.toContain(formatDate("2026-11-02T14:05:00Z"));
    expect(
      brokenNotice([github(broken("CONNECTOR_ACCOUNT_BROKEN_EXPIRED"))]).text,
    ).toMatch(/frozen as of its last sync\.$/);
  });

  it("breaks anew each time it breaks", () => {
    const c = github(broken("CONNECTOR_ACCOUNT_BROKEN_EXPIRED"));
    expect(breakKey(c)).toBe("conn_1:2026-11-02T14:05:00Z");
    expect(
      breakKey({
        ...c,
        account: { ...c.account, brokenAt: "2026-11-03T09:00:00Z" },
      }),
    ).not.toBe(breakKey(c));
  });

  it("shows as needing signing in again in place of its sync's status", () => {
    expect(
      connectorBadge({
        lastSync: { id: "s", status: "CONNECTOR_SYNC_STATUS_SUCCEEDED" },
        account: broken("CONNECTOR_ACCOUNT_BROKEN_EXPIRED"),
      }),
    ).toEqual({ status: "stale", label: "Needs signing in again" });
    expect(
      connectorBadge({
        lastSync: { id: "s", status: "CONNECTOR_SYNC_STATUS_SUCCEEDED" },
        account: { state: "CONNECTOR_ACCOUNT_STATE_CONNECTED" },
      }),
    ).toEqual({ status: "succeeded", label: "Synced" });
  });
});

describe("widening a signed-in connector", () => {
  it("is what the WFE's widensReach says of GitHub: Ada left it blank, so Grace may restrict it to acme/api", () => {
    expect(widensRepos([], ["acme/api"])).toBe(false);
    expect(widensRepos([], [])).toBe(false);
    // Back to every repository, or one not named before, widens it.
    expect(widensRepos(["acme/api"], [])).toBe(true);
    expect(widensRepos(["acme/api"], ["acme/api", "acme/web"])).toBe(true);
    // Fewer, or the same in another case, doesn't.
    expect(widensRepos(["acme/api", "acme/web"], ["ACME/API"])).toBe(false);
    expect(
      widensReach(
        { github: { repos: ["acme/api"] } },
        { github: { repos: ["acme/api", "acme/web"] } },
      ),
    ).toBe(true);
  });

  it("is a PostHog project changed", () => {
    const was = { posthog: { region: "us", projectId: "12345" } };
    expect(
      widensReach(was, { posthog: { region: "us", projectId: "12345" } }),
    ).toBe(false);
    expect(
      widensReach(was, { posthog: { region: "us", projectId: "67890" } }),
    ).toBe(true);
    expect(widensReach(was, { posthog: { region: "us" } })).toBe(true);
    // Its event history is anyone's.
    expect(
      widensReach(was, {
        posthog: { region: "us", projectId: "12345", eventDays: 90 },
      }),
    ).toBe(false);
  });

  it("is never another kind's settings", () => {
    expect(
      widensReach(
        { slack: { historyDays: 30 } },
        { slack: { historyDays: 365 } },
      ),
    ).toBe(false);
    expect(
      widensReach(
        { google: { eventDays: 30 } },
        { google: { eventDays: 365 } },
      ),
    ).toBe(false);
    expect(widensReach(null, { notion: {} })).toBe(false);
  });

  it("names who may, never as a former member", () => {
    expect(
      widenRefusal({
        kind: "github",
        account: { connectedBy: "Ada Lovelace" },
      }),
    ).toBe(
      "Only Ada Lovelace, whose GitHub access it reads with, can add repositories or read every one. You can remove some.",
    );
    expect(
      widenRefusal({
        kind: "github",
        account: { connectedBy: "a former member" },
      }),
    ).toBe(
      "Only the member who connected it, whose GitHub access it reads with, can add repositories or read every one. You can remove some.",
    );
    expect(
      widenRefusal({
        kind: "posthog",
        account: { connectedBy: "Ada Lovelace" },
      }),
    ).toBe(
      "Only Ada Lovelace, whose PostHog access it reads with, can change its project.",
    );
  });
});

describe("a failure's code", () => {
  it("is the failure without its prefix, for analytics", () => {
    expect(failureCode("CONNECTOR_SIGN_IN_FAILURE_CODE_EXPIRED")).toBe(
      "code_expired",
    );
    expect(failureCode("CONNECTOR_SIGN_IN_FAILURE_SETTINGS_INVALID")).toBe(
      "settings_invalid",
    );
    expect(failureCode("CONNECTOR_SIGN_IN_FAILURE_UNSPECIFIED")).toBe(
      "unknown",
    );
    expect(failureCode(undefined)).toBe("unknown");
  });
});

describe("the kinds that sign in", () => {
  it("are every kind Pipes signs in to, each with its provider and its way back to a key", () => {
    expect(
      [
        "github",
        "linear",
        "slack",
        "notion",
        "posthog",
        "calcom",
        "stripe",
        "google",
      ].map((k) => isSignInKind(k) && [providerName(k), keyInstead(k)]),
    ).toEqual([
      ["GitHub", "Use a personal access token instead"],
      ["Linear", "Use a personal API key instead"],
      ["Slack", "Use a bot token instead"],
      ["Notion", "Use an integration secret instead"],
      ["PostHog", "Use a personal API key instead"],
      ["Cal.com", "Use an API key instead"],
      ["Stripe", "Use a restricted key instead"],
      ["Google", "Use a service account key instead"],
    ]);
    for (const k of [
      "granola",
      "workos",
      "postgres",
      "mysql",
      "s3",
      "events",
      "constructor",
    ])
      expect(isSignInKind(k), k).toBe(false);
  });
});

describe("a page's picks across the round trip", () => {
  const PICKS: ReturnPicks = {
    goal: "顧客".repeat(200),
    connectorIds: ["conn_1"],
    eventSources: [["conn_e", "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION"]],
    setIds: ["set_1"],
  };

  it("keeps them in the tab's entry, however long the goal", () => {
    const s = storage();
    writeConnect(s, { ...ENTRY, picks: PICKS });
    expect(readConnect(s)?.picks).toEqual(PICKS);
    // A captured answer keeps them too.
    const replaced: string[] = [];
    captureCallback(
      new URL("https://foundry.helix-db.com/connect/callback?connect_code=c"),
      s,
      (p) => replaced.push(p),
    );
    expect(readConnect(s)?.picks).toEqual(PICKS);
  });

  it("reads them without trusting them", () => {
    expect(readPicks(null)).toBeUndefined();
    expect(readPicks({ connectorIds: [] })).toBeUndefined();
    expect(
      readPicks({
        goal: "g",
        connectorIds: "conn_1",
        eventSources: [
          ["c", "MODE_OTHER"],
          ["", "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED"],
        ],
        setIds: [1],
      }),
    ).toEqual({
      goal: "g",
      connectorIds: null,
      eventSources: [],
      setIds: null,
    });
  });

  it("adds the connector a sign-in made to a choice of some", () => {
    expect(withConnected(PICKS, "conn_9").connectorIds).toEqual([
      "conn_1",
      "conn_9",
    ]);
    expect(withConnected(PICKS, "conn_1").connectorIds).toEqual(["conn_1"]);
    // Every synced one includes it already.
    expect(
      withConnected({ ...PICKS, connectorIds: null }, "conn_9").connectorIds,
    ).toBeNull();
  });
});
