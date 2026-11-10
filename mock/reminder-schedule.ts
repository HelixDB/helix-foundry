// When the landing page's reminders go, as Foundry's backend schedules them
// (backend/reminder): every one at 09:00 on a weekday in the address's time
// zone. tests/unit/__fixtures__/reminder-schedule.json is the backend's golden
// table (backend/reminder/testdata/schedule.json): copy it again when it
// changes, and tests/unit/reminders-mock.test.ts holds this to it.

const DAY_MS = 86_400_000;
const HOUR = 9;

// The wall clock in `zone` at `at`.
function wall(at: number, zone: string) {
  const parts = Object.fromEntries(
    new Intl.DateTimeFormat("en-US", {
      timeZone: zone,
      hourCycle: "h23",
      year: "numeric",
      month: "numeric",
      day: "numeric",
      hour: "numeric",
      minute: "numeric",
      second: "numeric",
    })
      .formatToParts(at)
      .map((p) => [p.type, Number(p.value)]),
  );
  return Date.UTC(
    parts.year!,
    parts.month! - 1,
    parts.day!,
    parts.hour!,
    parts.minute!,
    parts.second!,
  );
}

// The instant of `day` (days since 1970-01-01, a date in `zone`) at
// `hour`:00 in `zone`.
function at(day: number, hour: number, zone: string) {
  const want = day * DAY_MS + hour * 3_600_000;
  let guess = want;
  for (let i = 0; i < 3; i++) {
    const off = wall(guess, zone) - want;
    if (!off) break;
    guess -= off;
  }
  return guess;
}

// The date of `instant` in `zone`, as days since 1970-01-01.
const dayOf = (instant: number, zone: string) =>
  Math.floor(wall(instant, zone) / DAY_MS);

// 1970-01-01 was a Thursday: Saturday and Sunday are days 2 and 3 of 7.
const weekend = (day: number) => {
  const d = (((day + 4) % 7) + 7) % 7;
  return d === 0 || d === 6;
};

/** Whether `zone` is a time zone the browser knows (the backend's, in effect). */
export function knownZone(zone: string) {
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: zone });
    return true;
  } catch {
    return false;
  }
}

/** The first reminder: the earliest weekday 09:00 at least an hour away. */
export function first(now: Date, zone: string): Date {
  const start = now.getTime() + 3_600_000;
  for (let day = dayOf(start, zone); ; day++) {
    if (weekend(day)) continue;
    const t = at(day, HOUR, zone);
    if (t >= start) return new Date(t);
  }
}

/**
 * The earliest weekday 09:00 on or after the date `days` after `last`'s, in
 * `zone`: the next reminder after one sent at `last`.
 */
export function after(last: Date, days: number, zone: string): Date {
  let day = dayOf(last.getTime(), zone) + days;
  while (weekend(day)) day++;
  return new Date(at(day, HOUR, zone));
}
