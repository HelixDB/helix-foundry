import { existsSync, readFileSync } from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import { beforeEach, describe, expect, it } from "vitest";
import { MOCK_REMINDERS_SECRET } from "../../mock/env";
import { after, first } from "../../mock/reminder-schedule";
import {
  handleReminders,
  mockReminders,
  normalize,
} from "../../mock/reminders";
import { resetState } from "../../mock/state";

// The mock's schedule of the landing page's reminders, held to the backend's
// (backend/reminder): its golden table, copied into __fixtures__, and a few
// cases of its own.

const GOLDENS = new URL(
  "./__fixtures__/reminder-schedule.json",
  import.meta.url,
);

type Goldens = {
  first: { name: string; now: string; zone: string; want: string }[];
  after: {
    name: string;
    last: string;
    zone: string;
    days: number;
    want: string;
  }[];
};
const goldens: Goldens = existsSync(GOLDENS)
  ? JSON.parse(readFileSync(GOLDENS, "utf8"))
  : { first: [], after: [] };

const iso = (d: Date) => d.toISOString().replace(".000Z", "Z");
const same = (a: string) => new Date(a).toISOString().replace(".000Z", "Z");

describe("the backend's golden schedule", () => {
  it("is copied from backend/reminder/testdata", () => {
    expect(goldens.first.length).toBeGreaterThan(0);
    expect(goldens.after.length).toBeGreaterThan(0);
  });
  it.each(goldens.first.map((g) => [g.name, g] as const))("first: %s", (_, g) =>
    expect(iso(first(new Date(g.now), g.zone))).toBe(same(g.want)),
  );
  it.each(goldens.after.map((g) => [g.name, g] as const))("after: %s", (_, g) =>
    expect(iso(after(new Date(g.last), g.days, g.zone))).toBe(same(g.want)),
  );
});

// The addresses backend/reminder's Normalize keeps and refuses, copied from
// its testdata/addresses.json.
const ADDRESSES = new URL(
  "./__fixtures__/reminder-addresses.json",
  import.meta.url,
);
const addresses: {
  kept: { in: string; want: string }[];
  refused: string[];
} = JSON.parse(readFileSync(ADDRESSES, "utf8"));

describe("the backend's addresses", () => {
  it.each(addresses.kept.map((a) => [a.in, a.want] as const))(
    "keeps %j as %j",
    (address, want) => expect(normalize(address)).toBe(want),
  );
  it.each(addresses.refused.map((a) => [a] as const))("refuses %j", (address) =>
    expect(normalize(address)).toBeNull(),
  );
});

describe("the first reminder", () => {
  it.each([
    // Tuesday in London, GMT in November.
    [
      "before 8am: the same morning",
      "2026-11-10T07:30:00Z",
      "Europe/London",
      "2026-11-10T09:00:00Z",
    ],
    [
      "after 8am: the next",
      "2026-11-10T08:30:00Z",
      "Europe/London",
      "2026-11-11T09:00:00Z",
    ],
    [
      "a Friday evening: Monday",
      "2026-11-13T20:00:00Z",
      "Europe/London",
      "2026-11-16T09:00:00Z",
    ],
    [
      "a Saturday: Monday",
      "2026-11-14T10:00:00Z",
      "Europe/London",
      "2026-11-16T09:00:00Z",
    ],
    // New York moves to EDT on Sunday 14 March 2027.
    [
      "across the spring change",
      "2027-03-12T23:00:00Z",
      "America/New_York",
      "2027-03-15T13:00:00Z",
    ],
  ])("%s", (_, now, zone, want) =>
    expect(iso(first(new Date(now), zone))).toBe(want),
  );
});

describe("the next reminder", () => {
  it.each([
    // London leaves BST on Sunday 25 October 2026: 9am stays 9am.
    [
      "a week on, across the autumn change",
      "2026-10-23T08:00:00Z",
      7,
      "Europe/London",
      "2026-10-30T09:00:00Z",
    ],
    [
      "two weeks on",
      "2026-11-16T09:00:00Z",
      14,
      "Europe/London",
      "2026-11-30T09:00:00Z",
    ],
    [
      "onto a weekend: the Monday",
      "2026-11-12T09:00:00Z",
      2,
      "Europe/London",
      "2026-11-16T09:00:00Z",
    ],
  ])("%s", (_, last, days, zone, want) =>
    expect(iso(after(new Date(last), days, zone))).toBe(want),
  );
});

// The mock's door, as the backend's: what a create may leave out, and the
// caps of the hour.
describe("the mock's door", () => {
  const ask = (body: Record<string, unknown>, path = "/v1/reminders") => {
    const req = {
      method: "POST",
      headers: { authorization: `Bearer ${MOCK_REMINDERS_SECRET}` },
    } as unknown as IncomingMessage;
    let status = 0;
    let text = "";
    const res = {
      writeHead: (code: number) => {
        status = code;
      },
      end: (out: string) => {
        text = out;
      },
    } as unknown as ServerResponse;
    handleReminders(
      req,
      res,
      new URL(`http://mock${path}`),
      JSON.stringify(body),
    );
    return { status, body: JSON.parse(text) };
  };
  const remind = (email: string, extra: Record<string, unknown> = {}) =>
    ask({ email, consent: "remind-v1", ...extra });

  beforeEach(() => resetState());

  it("takes a create without a zone, in UTC, as the door does", () => {
    expect(remind("ada@example.com").status).toBe(200);
    expect(mockReminders()[0]).toMatchObject({ timeZone: "UTC", client: "" });
  });

  it("refuses an address the backend refuses", () =>
    expect(remind('a"b@example.com').status).toBe(400));

  it("starts at most 5 schedules an hour for one visitor", () => {
    for (let i = 0; i < 5; i++)
      expect(remind(`p${i}@example.com`, { client: "visitor-1" }).status).toBe(
        200,
      );
    expect(remind("p5@example.com", { client: "visitor-1" }).status).toBe(429);
    expect(remind("p5@example.com", { client: "visitor-2" }).status).toBe(200);
    expect(remind("p0@example.com", { client: "visitor-1" }).body).toEqual(
      expect.objectContaining({ again: true }),
    );
    expect(remind("p6@example.com", { client: "a b" }).status).toBe(400);
  });
});
