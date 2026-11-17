import { afterEach, describe, expect, it, vi } from "vitest";
import {
  remindersConfig,
  stopReminders,
  type RemindersConfig,
} from "@/server/reminders";

// src/server/reminders.ts: a sign-in tells Foundry's door to stop reminding
// the address that signed in.

const SECRET = "0a1b2c3d".repeat(8);
const CONFIG: RemindersConfig = {
  ingestOrigin: "https://ingest.example.test",
  secret: SECRET,
};

afterEach(() => vi.unstubAllGlobals());

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
