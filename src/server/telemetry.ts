/**
 * Whether a local install reports how it is used (src/web/telemetry.tsx asks
 * /telemetry before it loads the tracker): yes, unless its owner turned it
 * off with FOUNDRY_TELEMETRY (off, false, no or 0) or DO_NOT_TRACK (any value
 * but 0, false or no, as the convention reads it). Read when asked, so a
 * change takes effect at the site's next start.
 *
 * @example
 * telemetryEnabled({}); // true
 * telemetryEnabled({ FOUNDRY_TELEMETRY: "off" }); // false
 * telemetryEnabled({ DO_NOT_TRACK: "1" }); // false
 * telemetryEnabled({ DO_NOT_TRACK: "0" }); // true
 */
export function telemetryEnabled(
  env: Record<string, string | undefined>,
): boolean {
  const no = (value: string) => /^(off|false|no|0)$/i.test(value);
  const telemetry = env.FOUNDRY_TELEMETRY?.trim() || "",
    doNotTrack = env.DO_NOT_TRACK?.trim() || "";
  return !(telemetry && no(telemetry)) && !(doNotTrack && !no(doNotTrack));
}
