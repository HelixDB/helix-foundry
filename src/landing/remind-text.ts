// The words of the landing page's reminder form (./remind.tsx), apart from
// the page so they can be tested.

/** What the form answers when the reminder wasn't set up. */
export type RemindFailure = "invalid" | "rate_limit" | "error";

export const FAILURE_TEXT: Record<RemindFailure, string> = {
  invalid: "That doesn’t look like an email address. Check it and try again.",
  rate_limit: "Too many reminders asked for just now. Try again in a minute.",
  error: "We couldn’t set up your reminder. Try again in a moment.",
};

/** The time zone the browser reports, or UTC when it can't say. */
export function browserTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

// The calendar date of `at` in `timeZone`, as days since 1970-01-01.
function dayNumber(at: Date, timeZone: string) {
  const parts = Object.fromEntries(
    new Intl.DateTimeFormat("en-US", {
      timeZone,
      year: "numeric",
      month: "numeric",
      day: "numeric",
    })
      .formatToParts(at)
      .map((p) => [p.type, p.value]),
  );
  return (
    Date.UTC(Number(parts.year), Number(parts.month) - 1, Number(parts.day)) /
    86_400_000
  );
}

// "9am", or "9:30am" off the hour, in `timeZone`.
function clock(at: Date, timeZone: string) {
  const parts = Object.fromEntries(
    new Intl.DateTimeFormat("en-US", {
      timeZone,
      hour: "numeric",
      minute: "2-digit",
      hour12: true,
    })
      .formatToParts(at)
      .map((p) => [p.type, p.value]),
  );
  const minutes = parts.minute === "00" ? "" : `:${parts.minute}`;
  return `${parts.hour}${minutes}${String(parts.dayPeriod).toLowerCase()}`;
}

/**
 * When the first reminder goes, as the form says it: "today at 9am",
 * "tomorrow at 9am", "on Monday at 9am" within the week, else "on 17
 * November at 9am", all in `timeZone`, the zone the reminder was set for.
 *
 * @example
 * whenText(new Date("2026-11-11T09:00:00Z"), "Europe/London", new Date("2026-11-10T20:00:00Z"));
 * // "tomorrow at 9am"
 */
export function whenText(at: Date, timeZone: string, now: Date): string {
  const days = dayNumber(at, timeZone) - dayNumber(now, timeZone);
  const day =
    days <= 0
      ? "today"
      : days === 1
        ? "tomorrow"
        : days < 7
          ? `on ${new Intl.DateTimeFormat("en-GB", { timeZone, weekday: "long" }).format(at)}`
          : `on ${new Intl.DateTimeFormat("en-GB", { timeZone, day: "numeric", month: "long" }).format(at)}`;
  return `${day} at ${clock(at, timeZone)}`;
}
