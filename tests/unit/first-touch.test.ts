import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  FIRST_TOUCH_KEEP_MS,
  firstTouchOf,
  firstTouchProperties,
  noteFirstTouch,
  readFirstTouch,
  sanitiseTag,
  storedFirstTouch,
} from "@/web/first-touch";

// First-touch attribution (src/web/first-touch.ts): a page load's utm_ tags
// and external referrer, sanitised, kept for 30 days, and read back without
// trusting what storage holds.

const HOST = "foundry.helix-db.com";
const page = (search: string, referrer = "", pathname = "/login") => ({
  search,
  pathname,
  referrer,
  host: HOST,
});

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

describe("sanitiseTag", () => {
  it("keeps lowercase [a-z0-9._-], at most 40 characters", () => {
    expect(sanitiseTag("GitHub")).toBe("github");
    expect(sanitiseTag("Spring Sale!")).toBe("spring_sale");
    expect(sanitiseTag("a.b_c-d")).toBe("a.b_c-d");
    expect(sanitiseTag("<script>alert(1)</script>")).toBe(
      "script_alert_1_script",
    );
    expect(sanitiseTag("x".repeat(60))).toBe("x".repeat(40));
    expect(sanitiseTag(`${"x".repeat(39)} y`)).toBe("x".repeat(39));
  });

  it("is undefined when nothing is left", () => {
    expect(sanitiseTag("")).toBeUndefined();
    expect(sanitiseTag("  !! ")).toBeUndefined();
    expect(sanitiseTag(null)).toBeUndefined();
    expect(sanitiseTag(undefined)).toBeUndefined();
  });
});

describe("firstTouchOf", () => {
  it("reads the utm_ tags and the landing path, masked", () => {
    expect(
      firstTouchOf(
        page(
          "?utm_source=GitHub&utm_medium=readme&utm_campaign=nav&utm_term=x&q=secret",
          "",
          "/ask/ont_123",
        ),
        5,
      ),
    ).toEqual({
      source: "github",
      medium: "readme",
      campaign: "nav",
      landing_path: "/ask/*",
      at: 5,
    });
  });

  it("takes an external referrer's host, with or without tags", () => {
    expect(
      firstTouchOf(page("", "https://News.ycombinator.com/item?id=1"), 0),
    ).toEqual({
      referrer_host: "news.ycombinator.com",
      landing_path: "/login",
      at: 0,
    });
    expect(
      firstTouchOf(page("?utm_source=www", "https://www.helix-db.com/"), 0),
    ).toMatchObject({ source: "www", referrer_host: "www.helix-db.com" });
  });

  it("ignores this site's own referrer, and one that isn't a URL", () => {
    expect(firstTouchOf(page("", `https://${HOST}/ask`), 0)).toBeNull();
    expect(
      firstTouchOf(
        { ...page("", "http://localhost:3000/"), host: "localhost:3000" },
        0,
      ),
    ).toBeNull();
    expect(firstTouchOf(page("", "not a url"), 0)).toBeNull();
    expect(firstTouchOf(page(""), 0)).toBeNull();
    expect(firstTouchOf(page("?utm_source=%20%21"), 0)).toBeNull();
  });

  it("caps and sanitises the landing path", () => {
    const long = `/${"Seg ".repeat(20)}`;
    const touch = firstTouchOf(page("?utm_source=x", "", long), 0);
    expect(touch?.landing_path).toMatch(/^[a-z0-9._/*-]{1,40}$/);
  });
});

describe("readFirstTouch", () => {
  const touch = { source: "github", landing_path: "/login", at: 1_000 };

  it("reads a touch younger than 30 days", () => {
    const raw = JSON.stringify(touch);
    expect(readFirstTouch(raw, 1_000)).toMatchObject(touch);
    expect(readFirstTouch(raw, 1_000 + FIRST_TOUCH_KEEP_MS - 1)).toMatchObject(
      touch,
    );
  });

  it("drops one 30 days old, or from the future", () => {
    const raw = JSON.stringify(touch);
    expect(readFirstTouch(raw, 1_000 + FIRST_TOUCH_KEEP_MS)).toBeNull();
    expect(readFirstTouch(raw, 999)).toBeNull();
  });

  it("checks what storage holds rather than trusting it", () => {
    expect(readFirstTouch(null, 0)).toBeNull();
    expect(readFirstTouch("{", 0)).toBeNull();
    expect(readFirstTouch("null", 0)).toBeNull();
    expect(readFirstTouch('"x"', 0)).toBeNull();
    expect(readFirstTouch(JSON.stringify({ source: "x" }), 0)).toBeNull();
    expect(readFirstTouch(JSON.stringify({ at: 0 }), 0)).toBeNull();
    expect(
      readFirstTouch(
        JSON.stringify({
          source: "Ada Lovelace <ada@example.com>",
          medium: 7,
          landing_path: 9,
          at: 0,
        }),
        0,
      ),
    ).toEqual({
      source: "ada_lovelace_ada_example.com",
      medium: undefined,
      campaign: undefined,
      referrer_host: undefined,
      landing_path: "/",
      at: 0,
    });
  });
});

describe("firstTouchProperties", () => {
  it("names each part first_*, leaving out what's absent", () => {
    expect(firstTouchProperties(null)).toEqual({});
    expect(
      firstTouchProperties({
        source: "github",
        medium: "readme",
        campaign: "nav",
        referrer_host: "github.com",
        landing_path: "/login",
        at: 0,
      }),
    ).toEqual({
      first_source: "github",
      first_medium: "readme",
      first_campaign: "nav",
      first_referrer: "github.com",
      first_landing: "/login",
    });
    expect(
      firstTouchProperties({ source: "x", landing_path: "/", at: 0 }),
    ).toEqual({ first_source: "x", first_landing: "/" });
  });
});

describe("noteFirstTouch", () => {
  let local: ReturnType<typeof fakeStorage>,
    session: ReturnType<typeof fakeStorage>;
  const KEY = "foundry.analytics.first-touch.v1";
  const stub = (blocked = false) => {
    local = fakeStorage(blocked);
    session = fakeStorage(blocked);
    vi.stubGlobal("localStorage", local);
    vi.stubGlobal("sessionStorage", session);
  };
  beforeEach(() => stub());
  afterEach(() => vi.unstubAllGlobals());

  it("keeps the first touch, not a later one", () => {
    noteFirstTouch(page("?utm_source=github"), 0);
    noteFirstTouch(page("?utm_source=www"), 10);
    expect(storedFirstTouch(10)).toEqual({
      first_source: "github",
      first_landing: "/login",
    });
  });

  it("takes a newer touch once the first is 30 days old", () => {
    noteFirstTouch(page("?utm_source=github"), 0);
    expect(storedFirstTouch(FIRST_TOUCH_KEEP_MS)).toEqual({});
    noteFirstTouch(page("?utm_source=www"), FIRST_TOUCH_KEEP_MS);
    expect(storedFirstTouch(FIRST_TOUCH_KEEP_MS)).toMatchObject({
      first_source: "www",
    });
  });

  it("counts a referrer only on the tab's first page load", () => {
    noteFirstTouch(page(""), 0);
    // Back from a provider's sign-in, in the same tab.
    noteFirstTouch(page("", "https://github.com/"), 1);
    expect(local.values.has(KEY)).toBe(false);
    // Tags still count on a later load.
    noteFirstTouch(page("?utm_medium=email", "https://github.com/"), 2);
    expect(storedFirstTouch(2)).toEqual({
      first_medium: "email",
      first_landing: "/login",
    });
  });

  it("keeps a referrer from a new tab's first load", () => {
    noteFirstTouch(page("", "https://news.ycombinator.com/"), 0);
    expect(storedFirstTouch(0)).toEqual({
      first_referrer: "news.ycombinator.com",
      first_landing: "/login",
    });
  });

  it("keeps and reads nothing when storage is blocked", () => {
    stub(true);
    expect(() => noteFirstTouch(page("?utm_source=github"), 0)).not.toThrow();
    expect(storedFirstTouch(0)).toEqual({});
  });
});
