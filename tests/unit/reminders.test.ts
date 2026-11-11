import { afterEach, describe, expect, it, vi } from "vitest";
import { failureText, whenText } from "@/landing/remind-text";
import {
  clientKey,
  handleRemind,
  parseRemindForm,
  readCapped,
  remindersConfig,
  stopReminders,
  type RemindersConfig,
} from "@/server/reminders";

// The landing page's reminders (src/shared/reminders.ts): the words the form
// says, and POST /remind on the server, which checks the form and asks
// Foundry's door to remind the address.

const SECRET = "0a1b2c3d".repeat(8);
const CONFIG: RemindersConfig = {
  ingestOrigin: "https://ingest.example.test",
  secret: SECRET,
};
const SITE = "https://foundry.example.test";

const form = (
  body: unknown,
  origin: string | null = SITE,
  headers: Record<string, string> = {},
) =>
  new Request(`${SITE}/remind`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...(origin ? { Origin: origin } : {}),
      ...headers,
    },
    body: typeof body === "string" ? body : JSON.stringify(body),
  });

const GOOD = {
  email: "  Ada@Example.com ",
  timeZone: "Europe/London",
  consent: "remind-v1",
};

afterEach(() => vi.unstubAllGlobals());

describe("whenText", () => {
  it.each([
    [
      "the same morning",
      "2026-11-10T07:00:00Z",
      "2026-11-10T09:00:00Z",
      "Europe/London",
      "today at 9am",
    ],
    [
      "the next morning",
      "2026-11-10T20:00:00Z",
      "2026-11-11T09:00:00Z",
      "Europe/London",
      "tomorrow at 9am",
    ],
    [
      // 21:00 on the 10th in New York, though the 11th in UTC.
      "tomorrow by the zone's own date",
      "2026-11-11T02:00:00Z",
      "2026-11-11T14:00:00Z",
      "America/New_York",
      "tomorrow at 9am",
    ],
    [
      "a Friday evening's Monday",
      "2026-11-13T20:00:00Z",
      "2026-11-16T09:00:00Z",
      "Europe/London",
      "on Monday at 9am",
    ],
    [
      "a week or more away",
      "2026-11-10T09:00:00Z",
      "2026-11-24T09:00:00Z",
      "Europe/London",
      "on 24 November at 9am",
    ],
    [
      "off the hour",
      "2026-11-10T12:00:00Z",
      "2026-11-11T04:00:00Z",
      "Asia/Kolkata",
      "tomorrow at 9:30am",
    ],
  ])("says %s", (_, now, at, zone, text) =>
    expect(whenText(new Date(at), zone, new Date(now))).toBe(text),
  );
});

describe("failureText", () => {
  it.each([
    ["900", "Too many reminders asked for just now. Try again in 15 minutes."],
    ["60", "Too many reminders asked for just now. Try again in a minute."],
    ["5", "Too many reminders asked for just now. Try again in a minute."],
    [null, "Too many reminders asked for just now. Try again later."],
    ["soon", "Too many reminders asked for just now. Try again later."],
  ])("says a rate limit's Retry-After %s", (wait, text) =>
    expect(failureText("rate_limit", wait)).toBe(text),
  );
  it("says the rest as they are", () =>
    expect(failureText("invalid", "900")).toMatch(/^That doesn’t look/));
});

describe("parseRemindForm", () => {
  it("keeps the trimmed address and the zone", () =>
    expect(parseRemindForm(JSON.stringify(GOOD))).toEqual({
      email: "Ada@Example.com",
      timeZone: "Europe/London",
    }));

  // The backend keeps a zone it doesn't know as UTC: this server's ICU may
  // be older than the browser's, and a zone is never why an address fails.
  it.each([
    ["a zone this server doesn't know", "Mars/Olympus", "Mars/Olympus"],
    ["no zone", "", ""],
    ["a zone past 64 characters", "Z".repeat(65), ""],
  ])("passes on %s", (_, timeZone, want) =>
    expect(parseRemindForm(JSON.stringify({ ...GOOD, timeZone }))).toEqual({
      email: "Ada@Example.com",
      timeZone: want,
    }),
  );

  it.each([
    ["not JSON", "{"],
    ["an array", "[]"],
    ["no consent", JSON.stringify({ ...GOOD, consent: undefined })],
    ["another consent", JSON.stringify({ ...GOOD, consent: "remind-v0" })],
    ["another field", JSON.stringify({ ...GOOD, name: "Ada" })],
    ["no @", JSON.stringify({ ...GOOD, email: "ada.example.com" })],
    ["a space", JSON.stringify({ ...GOOD, email: "ada lovelace@example.com" })],
    ["too long", JSON.stringify({ ...GOOD, email: `${"a".repeat(250)}@b.co` })],
    ["a zone not a string", JSON.stringify({ ...GOOD, timeZone: 1 })],
    ["a body over 2 KiB", JSON.stringify({ ...GOOD, pad: "x".repeat(3000) })],
  ])("refuses %s", (_, raw) => expect(parseRemindForm(raw)).toBeNull());
});

describe("remindersConfig", () => {
  const events = "https://ingest.helix-db.com";
  it("takes the secret and the stamp's door", () =>
    expect(
      remindersConfig({ FOUNDRY_REMINDERS_SECRET: SECRET }, events, false),
    ).toEqual({ ingestOrigin: events, secret: SECRET }));

  it("prefers HELIX_INGEST_ORIGIN, as the mock sets it", () =>
    expect(
      remindersConfig(
        {
          FOUNDRY_REMINDERS_SECRET: SECRET,
          HELIX_INGEST_ORIGIN: "http://127.0.0.1:4110",
        },
        events,
        false,
      )?.ingestOrigin,
    ).toBe("http://127.0.0.1:4110"));

  it.each([
    ["no secret", {}, events, false],
    [
      "a short secret",
      { FOUNDRY_REMINDERS_SECRET: "0a".repeat(16) },
      events,
      false,
    ],
    [
      // Its SHA-256 is in git: a passphrase could be guessed from it.
      "a passphrase",
      { FOUNDRY_REMINDERS_SECRET: "correct horse battery staple ".repeat(3) },
      events,
      false,
    ],
    [
      "upper-case hex",
      { FOUNDRY_REMINDERS_SECRET: SECRET.toUpperCase() },
      events,
      false,
    ],
    ["no door", { FOUNDRY_REMINDERS_SECRET: SECRET }, "", false],
    [
      "plain http off loopback",
      { FOUNDRY_REMINDERS_SECRET: SECRET },
      "http://ingest.helix-db.com",
      false,
    ],
    [
      "a door with a path",
      { FOUNDRY_REMINDERS_SECRET: SECRET },
      `${events}/v1`,
      false,
    ],
    ["a local build", { FOUNDRY_REMINDERS_SECRET: SECRET }, events, true],
  ])("is off with %s", (_, env, url, local) =>
    expect(remindersConfig(env, url, local)).toBeNull(),
  );
});

describe("POST /remind", () => {
  const door = (status: number, body: unknown, headers?: HeadersInit) => {
    const fetch = vi.fn(async () => Response.json(body, { status, headers }));
    vi.stubGlobal("fetch", fetch);
    return fetch;
  };

  it("asks the door with the secret, and says when the first goes", async () => {
    const fetch = door(200, {
      next_send_at: "2026-11-11T09:00:00Z",
      again: false,
    });
    const res = await handleRemind(form(GOOD), { config: CONFIG });
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({
      nextSendAt: "2026-11-11T09:00:00Z",
      again: false,
    });
    const [url, init] = fetch.mock.calls[0] as unknown as [URL, RequestInit];
    expect(String(url)).toBe("https://ingest.example.test/v1/reminders");
    expect(new Headers(init.headers).get("Authorization")).toBe(
      `Bearer ${SECRET}`,
    );
    expect(JSON.parse(String(init.body))).toEqual({
      email: "Ada@Example.com",
      time_zone: "Europe/London",
      consent: "remind-v1",
      client: "",
    });
  });

  it("names the visitor by a key of their address, for the backend's cap", async () => {
    const fetch = door(200, { next_send_at: "2026-11-11T09:00:00Z" });
    await handleRemind(form(GOOD, SITE, { "x-real-ip": "203.0.113.9" }), {
      config: CONFIG,
    });
    const [, init] = fetch.mock.calls[0] as unknown as [URL, RequestInit];
    const { client } = JSON.parse(String(init.body));
    expect(client).toMatch(/^[A-Za-z0-9_-]{22}$/);
    expect(client).toBe(
      clientKey(form(GOOD, SITE, { "x-real-ip": "203.0.113.9" }), SECRET),
    );
  });

  it("refuses a body past 2 KiB without reading it", async () => {
    const fetch = door(200, {});
    const big = form({ ...GOOD, pad: "x".repeat(4000) });
    expect((await handleRemind(big, { config: CONFIG })).status).toBe(400);
    expect(fetch).not.toHaveBeenCalled();
  });

  it("passes on that the address already had them", async () => {
    door(200, { next_send_at: "2026-11-11T09:00:00Z", again: true });
    const res = await handleRemind(form(GOOD), { config: CONFIG });
    expect(await res.json()).toMatchObject({ again: true });
  });

  it("is not there without its settings", async () => {
    const fetch = door(200, {});
    const res = await handleRemind(form(GOOD), { config: null });
    expect(res.status).toBe(404);
    expect(fetch).not.toHaveBeenCalled();
  });

  it.each([
    ["another origin", "https://evil.example.test"],
    ["no origin", null],
  ])("refuses %s", async (_, origin) => {
    const fetch = door(200, {});
    const res = await handleRemind(form(GOOD, origin), { config: CONFIG });
    expect(res.status).toBe(403);
    expect(fetch).not.toHaveBeenCalled();
  });

  it("refuses a bot, and a check that fails", async () => {
    const fetch = door(200, {});
    for (const isBot of [
      async () => true,
      async () => {
        throw new Error("BotID is away");
      },
    ]) {
      const res = await handleRemind(form(GOOD), { config: CONFIG, isBot });
      expect(res.status).toBe(403);
    }
    expect(fetch).not.toHaveBeenCalled();
  });

  it("refuses what isn't an address before asking the door", async () => {
    const fetch = door(200, {});
    const res = await handleRemind(form({ ...GOOD, email: "ada" }), {
      config: CONFIG,
    });
    expect(res.status).toBe(400);
    expect(await res.json()).toEqual({ error: "invalid" });
    expect(fetch).not.toHaveBeenCalled();
  });

  it.each([
    [400, {}, 400, { error: "invalid" }],
    [429, { "Retry-After": "60" }, 429, { error: "rate_limit" }],
    [401, {}, 502, { error: "error" }],
    [404, {}, 502, { error: "error" }],
    [503, {}, 502, { error: "error" }],
  ])("answers the door's %i as %i", async (status, headers, want, body) => {
    door(status, { error: { code: "x", message: "y" } }, headers);
    const res = await handleRemind(form(GOOD), { config: CONFIG });
    expect(res.status).toBe(want);
    expect(await res.json()).toEqual(body);
    if (status === 429) expect(res.headers.get("Retry-After")).toBe("60");
  });

  it.each([400, 429])("lets the door's %i body go unread", async (status) => {
    const cancel = vi.fn();
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(new ReadableStream({ cancel }), {
            status,
            headers: { "Retry-After": "900" },
          }),
      ),
    );
    await handleRemind(form(GOOD), { config: CONFIG });
    expect(cancel).toHaveBeenCalled();
  });

  it("fails when the door can't be reached", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("fetch failed");
      }),
    );
    const res = await handleRemind(form(GOOD), { config: CONFIG });
    expect(res.status).toBe(502);
  });
});

describe("clientKey", () => {
  const visitor = (headers: Record<string, string>) =>
    new Request(`${SITE}/remind`, { method: "POST", headers });

  it("is one key per address and secret, which doesn't hold the address", () => {
    const key = clientKey(visitor({ "x-real-ip": "203.0.113.9" }), SECRET);
    expect(key).toMatch(/^[A-Za-z0-9_-]{22}$/);
    expect(key).not.toContain("203");
    expect(
      clientKey(
        visitor({ "x-forwarded-for": "203.0.113.9, 10.0.0.1" }),
        SECRET,
      ),
    ).toBe(key);
    expect(
      clientKey(visitor({ "x-real-ip": "203.0.113.10" }), SECRET),
    ).not.toBe(key);
    expect(
      clientKey(visitor({ "x-real-ip": "203.0.113.9" }), "f".repeat(64)),
    ).not.toBe(key);
  });

  it("is none for a request that names no address", () =>
    expect(clientKey(visitor({}), SECRET)).toBe(""));
});

describe("readCapped", () => {
  const stream = (chunks: string[]) =>
    new Request(`${SITE}/remind`, {
      method: "POST",
      body: new ReadableStream({
        start(controller) {
          for (const chunk of chunks)
            controller.enqueue(new TextEncoder().encode(chunk));
          controller.close();
        },
      }),
      duplex: "half",
    } as RequestInit);

  it("reads a body within the cap", async () =>
    expect(await readCapped(stream(["ab", "cd"]), 4)).toBe("abcd"));
  it("stops at the cap, with no Content-Length to say so", async () =>
    expect(await readCapped(stream(["ab", "cde"]), 4)).toBeNull());
});

describe("stopReminders", () => {
  it("tells the door the address signed in", async () => {
    const fetch = vi.fn(async () => Response.json({}));
    vi.stubGlobal("fetch", fetch);
    await stopReminders(CONFIG, "ada@example.com");
    const [url, init] = fetch.mock.calls[0] as unknown as [URL, RequestInit];
    expect(String(url)).toBe("https://ingest.example.test/v1/reminders/stop");
    expect(JSON.parse(String(init.body))).toEqual({
      email: "ada@example.com",
    });
  });

  it("does nothing without settings or an address, and never throws", async () => {
    const fetch = vi.fn(async () => {
      throw new TypeError("fetch failed");
    });
    vi.stubGlobal("fetch", fetch);
    await stopReminders(null, "ada@example.com");
    await stopReminders(CONFIG, "");
    expect(fetch).not.toHaveBeenCalled();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    await expect(
      stopReminders(CONFIG, "ada@example.com", async () => {}),
    ).resolves.toBeUndefined();
    expect(fetch).toHaveBeenCalledTimes(3);
    expect(warn).toHaveBeenCalledTimes(1);
    expect(String(warn.mock.calls[0][0])).not.toContain("ada");
    warn.mockRestore();
  });

  it("asks again while the door is away or busy, as long as it asks", async () => {
    const answers = [
      new Response(null, { status: 503 }),
      new Response(null, { status: 429, headers: { "Retry-After": "3" } }),
      Response.json({}),
    ];
    const fetch = vi.fn(async () => answers.shift()!);
    vi.stubGlobal("fetch", fetch);
    const waits: number[] = [];
    await stopReminders(CONFIG, "ada@example.com", async (ms) => {
      waits.push(ms);
    });
    expect(fetch).toHaveBeenCalledTimes(3);
    expect(waits).toEqual([1000, 3000]);
  });

  it("doesn't ask again what the door refused", async () => {
    const fetch = vi.fn(async () => new Response(null, { status: 401 }));
    vi.stubGlobal("fetch", fetch);
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    await stopReminders(CONFIG, "ada@example.com", async () => {});
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(warn).toHaveBeenCalledTimes(1);
    warn.mockRestore();
  });
});
