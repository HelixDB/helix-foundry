// A datetime prop's value as Helix holds it: its DateTime, UTC milliseconds
// since the epoch, which a read renders as `YYYY-MM-DDTHH:MM:SS.mmmZ` and a
// `date_time` parameter takes as RFC 3339 text (helix-db's
// DateTime::parse_rfc3339: chrono's parse_from_rfc3339, then
// timestamp_millis). The ontology agent loads every datetime prop so, and
// its own JSON writes the same instant with no fraction or three digits
// (`2026-03-14T08:30:00Z`); both are RFC 3339, so one reading serves both.

// chrono's parse_rfc3339: `T`, `t` or a space before the time, any number of
// fraction digits, `Z`, `z` or an offset of at most 23:59 (its minus may be
// U+2212). Nothing is trimmed.
const RFC3339 =
  /^(\d{4})-(\d{2})-(\d{2})[Tt ](\d{2}):(\d{2}):(\d{2})(?:\.(\d+))?(?:[Zz]|([+\-−])(\d{2}):(\d{2}))$/;

// The instants Helix renders: 0000-01-01T00:00:00.000Z to
// 9999-12-31T23:59:59.999Z.
const FIRST_MS = -62_167_219_200_000;
const LAST_MS = 253_402_300_799_999;

/**
 * `raw` as Helix's DateTime holds and renders it: UTC, truncated to the
 * millisecond as timestamp_millis floors, `YYYY-MM-DDTHH:MM:SS.mmmZ`; or
 * undefined when chrono wouldn't read it as RFC 3339, or its instant isn't
 * one Helix renders (outside 0000-01-01T00:00:00Z to
 * 9999-12-31T23:59:59.999Z, in UTC). A leap second (`:60`, at the end of
 * any minute, as chrono takes it) is the next second's millisecond, as
 * timestamp_millis counts it.
 *
 * @example
 * helixDateTime("2026-03-14T09:30:00+01:00"); // "2026-03-14T08:30:00.000Z"
 * helixDateTime("2026-03-14t08:30:00.25z"); // "2026-03-14T08:30:00.250Z"
 * helixDateTime("2026-03-14 08:30:00.0009Z"); // "2026-03-14T08:30:00.000Z"
 * helixDateTime("2016-12-31T23:59:60Z"); // "2017-01-01T00:00:00.000Z"
 * helixDateTime("2026-03-14"); // undefined
 * helixDateTime(" 2026-03-14T08:30:00Z"); // undefined
 */
export function helixDateTime(raw: string): string | undefined {
  const m = RFC3339.exec(raw);
  if (!m) return undefined;
  const [year, month, day, hour, minute, second] = m
    .slice(1, 7)
    .map(Number) as [number, number, number, number, number, number];
  const [oh, om] = [Number(m[9] ?? 0), Number(m[10] ?? 0)];
  const last = new Date(0);
  last.setUTCFullYear(year, month, 0);
  if (
    month < 1 ||
    month > 12 ||
    day < 1 ||
    day > last.getUTCDate() ||
    hour > 23 ||
    minute > 59 ||
    second > 60 ||
    oh > 23 ||
    om > 59
  )
    return undefined;
  const at = new Date(0);
  at.setUTCFullYear(year, month - 1, day);
  // A leap second is 59 and a second more.
  at.setUTCHours(hour, minute, Math.min(second, 59), 0);
  const offset = m[8] ? (m[8] === "+" ? 1 : -1) * (oh * 60 + om) : 0;
  const ms =
    at.getTime() -
    offset * 60_000 +
    (second === 60 ? 1000 : 0) +
    Number((m[7] || "").slice(0, 3).padEnd(3, "0"));
  return ms < FIRST_MS || ms > LAST_MS ? undefined : new Date(ms).toISOString();
}
