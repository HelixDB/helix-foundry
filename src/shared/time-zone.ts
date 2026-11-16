// IANA time zone ids in words. The ids (America/New_York) are what specs and
// answers carry; people read the city and its offset now ("New York ·
// GMT−5").

let known: ReadonlySet<string> | undefined;

/**
 * Whether `id` is a time zone this runtime names (`Intl.supportedValuesOf`),
 * or UTC. A runtime without the list knows only UTC.
 *
 * @example
 * isTimeZone("America/New_York"); // true
 * isTimeZone("New York"); // false
 */
export function isTimeZone(id: string): boolean {
  known ??= new Set([
    "UTC",
    ...((
      Intl as { supportedValuesOf?: (key: "timeZone") => string[] }
    ).supportedValuesOf?.("timeZone") ?? []),
  ]);
  return known.has(id);
}

/**
 * A zone's place: its last part, underscores as spaces; UTC as it is.
 *
 * @example
 * zoneCity("America/Argentina/Buenos_Aires"); // "Buenos Aires"
 * zoneCity("UTC"); // "UTC"
 */
export const zoneCity = (id: string) =>
  (id.split("/").at(-1) ?? id).replaceAll("_", " ");

/**
 * A zone's offset from GMT at `at`, with a true minus sign: "GMT−5",
 * "GMT+5:30", "GMT".
 *
 * @example
 * zoneOffset("Asia/Kolkata", new Date("2026-01-15T12:00:00Z")); // "GMT+5:30"
 */
export function zoneOffset(id: string, at: Date = new Date()): string {
  const name = new Intl.DateTimeFormat("en-US", {
    timeZone: id,
    timeZoneName: "shortOffset",
  })
    .formatToParts(at)
    .find((p) => p.type === "timeZoneName")?.value;
  return (name ?? "GMT").replace("-", "−");
}

/**
 * A time zone id in words, its city and its offset at `at`; undefined for
 * anything that isn't a zone id, so a caller shows that as it is. UTC is
 * "UTC", and a fixed offset (Etc/GMT+5, whose sign POSIX inverts) is its
 * offset alone.
 *
 * @example
 * zoneLabel("America/New_York", new Date("2026-01-15T12:00:00Z")); // "New York · GMT−5"
 * zoneLabel("UTC"); // "UTC"
 * zoneLabel("Europe/Atlantis"); // undefined
 */
export function zoneLabel(
  id: string,
  at: Date = new Date(),
): string | undefined {
  if (!isTimeZone(id)) return undefined;
  if (id === "UTC") return "UTC";
  const offset = zoneOffset(id, at);
  return id.startsWith("Etc/") ? offset : `${zoneCity(id)} · ${offset}`;
}

/**
 * A zone as a time is said in it: "New York time", "UTC", or a fixed
 * offset's "GMT−5". Any id at all: one this runtime doesn't name reads by
 * its city.
 *
 * @example
 * `09:00 ${zoneTime("Europe/London")}`; // "09:00 London time"
 */
export const zoneTime = (id: string) =>
  id === "UTC"
    ? "UTC"
    : id.startsWith("Etc/") && isTimeZone(id)
      ? zoneOffset(id)
      : `${zoneCity(id)} time`;
