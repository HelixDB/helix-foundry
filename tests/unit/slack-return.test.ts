import { describe, expect, it, vi } from "vitest";
import {
  SLACK_LINK_TOKEN_PREFIX,
  SLACK_RETURN_BOOT,
  SLACK_RETURN_TTL_MS,
  captureSlackReturn,
  clearSlackReturn,
  readSlackReturn,
} from "@/web/areas/workspace/slack-return";

// Slack's ways back to the site (src/web/areas/workspace/slack-return.ts):
// what /slack/installed and /slack/link carry moves into the tab and leaves
// the URL before any call, so a sign-in on the way keeps it and it reaches
// no server log or tracker; the pages then read it while it is fresh.

class Memory implements Storage {
  private items = new Map<string, string>();
  get length() {
    return this.items.size;
  }
  clear() {
    this.items.clear();
  }
  getItem(key: string) {
    return this.items.get(key) ?? null;
  }
  key(i: number) {
    return [...this.items.keys()][i] ?? null;
  }
  removeItem(key: string) {
    this.items.delete(key);
  }
  setItem(key: string, value: string) {
    this.items.set(key, value);
  }
}
/** Storage the browser blocks: every call throws. */
const blocked = {
  getItem: () => {
    throw new Error("blocked");
  },
  setItem: () => {
    throw new Error("blocked");
  },
  removeItem: () => {
    throw new Error("blocked");
  },
} as unknown as Storage;

const NOW = 1_800_000_000_000,
  TOKEN = "hx_sl_" + "a".repeat(43);
const at = (pathname: string, search = "", hash = "") => ({
  pathname,
  search,
  hash,
});

describe("the link's token", () => {
  it("moves out of the fragment, keeping the workspace in the URL", () => {
    const storage = new Memory(),
      replace = vi.fn();
    expect(
      captureSlackReturn(
        at("/slack/link", "?workspace=ws_acme", `#t=${TOKEN}`),
        storage,
        replace,
        NOW,
      ),
    ).toBe(true);
    expect(replace).toHaveBeenCalledWith("/slack/link?workspace=ws_acme");
    expect(readSlackReturn(storage, "link", NOW + 1000)).toEqual({
      kind: "link",
      token: TOKEN,
      at: NOW,
    });
    // Another page's kind reads nothing.
    expect(readSlackReturn(storage, "installed", NOW)).toBeNull();
  });
  it("is dropped, and the fragment cleared, when it isn't the app's", () => {
    const storage = new Memory(),
      replace = vi.fn();
    captureSlackReturn(
      at("/slack/link", "", `#t=${TOKEN}`),
      storage,
      vi.fn(),
      NOW,
    );
    for (const hash of ["#t=nope", "#x=1", `#t=hx_sl_${"a".repeat(600)}`]) {
      expect(
        captureSlackReturn(at("/slack/link", "", hash), storage, replace, NOW),
        hash,
      ).toBe(false);
      expect(replace).toHaveBeenLastCalledWith("/slack/link");
      // A bad link forgets an earlier good one: the page says so.
      expect(readSlackReturn(storage, "link", NOW)).toBeNull();
    }
  });
  it("leaves a link without a fragment alone", () => {
    const storage = new Memory(),
      replace = vi.fn();
    captureSlackReturn(
      at("/slack/link", "", `#t=${TOKEN}`),
      storage,
      replace,
      NOW,
    );
    replace.mockClear();
    // Back from sign-in: the token kept before is still there.
    expect(
      captureSlackReturn(at("/slack/link", "?workspace=w"), storage, replace),
    ).toBe(false);
    expect(replace).not.toHaveBeenCalled();
    expect(readSlackReturn(storage, "link", NOW)?.token).toBe(TOKEN);
  });
});

describe("Slack's return from adding the app", () => {
  it("keeps the code and state, and bares the URL", () => {
    const storage = new Memory(),
      replace = vi.fn();
    expect(
      captureSlackReturn(
        at("/slack/installed", "?code=c1&state=s1"),
        storage,
        replace,
        NOW,
      ),
    ).toBe(true);
    expect(replace).toHaveBeenCalledWith("/slack/installed");
    expect(
      readSlackReturn(storage, ["installed", "install_error"], NOW),
    ).toEqual({ kind: "installed", code: "c1", state: "s1", at: NOW });
  });
  it("keeps a cancel's error", () => {
    const storage = new Memory();
    captureSlackReturn(
      at("/slack/installed", "?error=access_denied&state=s1"),
      storage,
      vi.fn(),
      NOW,
    );
    expect(
      readSlackReturn(storage, ["installed", "install_error"], NOW),
    ).toEqual({ kind: "install_error", error: "access_denied", at: NOW });
  });
  it("keeps nothing of a malformed return, and still bares the URL", () => {
    const storage = new Memory(),
      replace = vi.fn();
    expect(
      captureSlackReturn(at("/slack/installed", "?code=c1"), storage, replace),
    ).toBe(false);
    expect(replace).toHaveBeenCalledWith("/slack/installed");
    expect(readSlackReturn(storage, ["installed", "install_error"])).toBeNull();
    expect(captureSlackReturn(at("/slack/installed"), storage, replace)).toBe(
      false,
    );
  });
});

describe("Slack's error", () => {
  it("is kept only as one of Slack's codes: a forged sentence is nothing", () => {
    const storage = new Memory(),
      replace = vi.fn();
    expect(
      captureSlackReturn(
        at("/slack/installed", "?error=invalid_scope"),
        storage,
        replace,
        NOW,
      ),
    ).toBe(true);
    expect(readSlackReturn(storage, "install_error", NOW)?.error).toBe(
      "invalid_scope",
    );
    for (const error of [
      "Your%20workspace%20is%20suspended",
      "Access_Denied",
      "a".repeat(65),
    ]) {
      expect(
        captureSlackReturn(
          at("/slack/installed", `?error=${error}`),
          storage,
          replace,
          NOW,
        ),
        error,
      ).toBe(false);
      expect(replace).toHaveBeenLastCalledWith("/slack/installed");
      expect(readSlackReturn(storage, "install_error", NOW)).toBeNull();
    }
    // Nor is one written by hand read back.
    storage.setItem(
      "foundry.slack.return.v1",
      JSON.stringify({ kind: "install_error", error: "call us", at: NOW }),
    );
    expect(readSlackReturn(storage, "install_error", NOW)).toBeNull();
  });
});

describe("the boot script in <head>", () => {
  /** Runs SLACK_RETURN_BOOT as the page's <head> does, on `href`. */
  function boot(href: string, storage: Storage = new Memory()) {
    const u = new URL(href, "https://foundry.example"),
      history = { state: { idx: 1 }, replaceState: vi.fn() };
    // Its own scope: it names nothing of this module's.
    new Function("location", "history", "sessionStorage", SLACK_RETURN_BOOT)(
      { pathname: u.pathname, search: u.search, hash: u.hash },
      history,
      storage,
    );
    return { storage, history };
  }
  it("moves the link's token out of the URL before anything else runs", () => {
    const { storage, history } = boot(
      `/slack/link?workspace=ws_acme#t=${TOKEN}`,
    );
    expect(TOKEN.startsWith(SLACK_LINK_TOKEN_PREFIX)).toBe(true);
    expect(history.replaceState).toHaveBeenCalledWith(
      { idx: 1 },
      "",
      "/slack/link?workspace=ws_acme",
    );
    expect(readSlackReturn(storage, "link")?.token).toBe(TOKEN);
  });
  it("moves Slack's code and state out of the URL", () => {
    const { storage, history } = boot("/slack/installed?code=c1&state=s1");
    expect(history.replaceState).toHaveBeenCalledWith(
      { idx: 1 },
      "",
      "/slack/installed",
    );
    expect(readSlackReturn(storage, "installed")).toMatchObject({
      code: "c1",
      state: "s1",
    });
  });
  it("leaves every other page alone, and never throws", () => {
    const { storage, history } = boot("/ask?code=c#t=x");
    expect(history.replaceState).not.toHaveBeenCalled();
    expect(storage.length).toBe(0);
    expect(() => boot(`/slack/link#t=${TOKEN}`, blocked)).not.toThrow();
  });
});

describe("anything else", () => {
  it("is left alone", () => {
    const storage = new Memory(),
      replace = vi.fn();
    for (const url of [
      at("/settings/slack", "?code=c&state=s", `#t=${TOKEN}`),
      at("/slack/linked", "", `#t=${TOKEN}`),
      at("/connect/callback", "?code=c&state=s"),
    ])
      expect(captureSlackReturn(url, storage, replace)).toBe(false);
    expect(replace).not.toHaveBeenCalled();
    expect(storage.length).toBe(0);
  });
});

describe("reading a kept return", () => {
  const kept = (value: unknown) => {
    const storage = new Memory();
    storage.setItem("foundry.slack.return.v1", JSON.stringify(value));
    return storage;
  };
  it("lasts ten minutes, and never from the future", () => {
    const storage = new Memory();
    captureSlackReturn(
      at("/slack/link", "", `#t=${TOKEN}`),
      storage,
      vi.fn(),
      NOW,
    );
    expect(
      readSlackReturn(storage, "link", NOW + SLACK_RETURN_TTL_MS),
    ).not.toBeNull();
    expect(
      readSlackReturn(storage, "link", NOW + SLACK_RETURN_TTL_MS + 1),
    ).toBeNull();
    expect(readSlackReturn(storage, "link", NOW - 1)).toBeNull();
  });
  it("refuses what this build didn't write", () => {
    const good = { kind: "installed", code: "c", state: "s", at: NOW };
    for (const value of [
      null,
      "text",
      { ...good, at: "now" },
      { ...good, code: "" },
      { ...good, state: ["s"] },
      { kind: "install_error", error: 1, at: NOW },
      { kind: "link", token: "abc", at: NOW },
      { kind: "other", at: NOW },
    ])
      expect(
        readSlackReturn(
          kept(value),
          ["installed", "install_error", "link"],
          NOW,
        ),
        JSON.stringify(value),
      ).toBeNull();
    const storage = new Memory();
    storage.setItem("foundry.slack.return.v1", "{not json");
    expect(readSlackReturn(storage, "link", NOW)).toBeNull();
    expect(readSlackReturn(null, "link", NOW)).toBeNull();
    expect(readSlackReturn(kept(good), "installed", NOW)).toEqual(good);
  });
  it("is forgotten once spent", () => {
    const storage = new Memory();
    captureSlackReturn(
      at("/slack/link", "", `#t=${TOKEN}`),
      storage,
      vi.fn(),
      NOW,
    );
    clearSlackReturn(storage);
    expect(readSlackReturn(storage, "link", NOW)).toBeNull();
    clearSlackReturn(null);
  });
});

describe("blocked storage", () => {
  it("keeps nothing and reads nothing, but still cleans the URL", () => {
    const replace = vi.fn();
    expect(
      captureSlackReturn(
        at("/slack/link", "", `#t=${TOKEN}`),
        blocked,
        replace,
        NOW,
      ),
    ).toBe(false);
    expect(replace).toHaveBeenCalledWith("/slack/link");
    expect(readSlackReturn(blocked, "link", NOW)).toBeNull();
    expect(() => clearSlackReturn(blocked)).not.toThrow();
  });
  it("without a tab's storage, still cleans the URL", () => {
    const replace = vi.fn();
    expect(
      captureSlackReturn(
        at("/slack/installed", "?code=c&state=s"),
        null,
        replace,
        NOW,
      ),
    ).toBe(false);
    expect(replace).toHaveBeenCalledWith("/slack/installed");
  });
});
