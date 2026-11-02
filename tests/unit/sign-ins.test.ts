import { describe, expect, it } from "vitest";
import type { Connector, ConnectorAccount } from "@/shared/wfe";
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
  isProviderUrl,
  needsSignIn,
  readConnect,
  signInFailure,
  startAgainPath,
  writeConnect,
  type ConnectEntry,
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
