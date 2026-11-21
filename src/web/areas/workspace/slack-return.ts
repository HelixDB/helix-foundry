// What Slack's two ways back to the site carry, kept in this tab before any
// call, and the URL then cleaned: Slack's return from adding the app
// (/slack/installed?code=&state=, or ?error= when the person cancelled), and
// the one-time link the app sends a person in Slack
// (/slack/link?workspace=<id>#t=<token>). The root layout runs
// SLACK_RETURN_BOOT in <head>, before any of the site's scripts, so a
// sign-in on the way (which returns to the page without its query or
// fragment) loses neither, and neither reaches the server's logs, a
// sign-in's return path or the tracker (which loads later, from the page).
// Small and free of the area's other modules: the root layout imports it.

/** The app's link tokens: "hx_sl_" and base64url (backend api/slack.go). */
export const SLACK_LINK_TOKEN_PREFIX = "hx_sl_";
/** A kept return is dropped after this: a link token and a state live ten minutes. */
export const SLACK_RETURN_TTL_MS = 10 * 60_000;
const KEY = "foundry.slack.return.v1";
// Longer than any code, state or token Slack or the app sends.
const MAX = 512;

/** What a way back carried: a finished install, a cancelled one, or a link. */
export type SlackReturn =
  | { kind: "installed"; code: string; state: string; at: number }
  | { kind: "install_error"; error: string; at: number }
  | { kind: "link"; token: string; at: number };

const fits = (v: unknown): v is string =>
  typeof v === "string" && v.length > 0 && v.length <= MAX;
/** Slack's OAuth error codes (access_denied, invalid_scope, ...). */
const ERROR_CODE = /^[a-z_]{1,64}$/;

/**
 * Keeps what a way back from Slack carried in `storage` and replaces the URL
 * with one without it (the link's ?workspace= stays, for the app to open
 * that workspace). Any other URL, or one already cleaned, is left alone.
 * Returns whether something was kept; a malformed return (an error that
 * isn't one of Slack's codes, a token without its prefix) is cleaned and
 * kept as nothing, which the page then says.
 *
 * It names nothing outside itself: SLACK_RETURN_BOOT is its own source,
 * run in the page's <head>, so its constants are written out (the tests
 * hold them to the module's).
 *
 * @example
 * captureSlackReturn({ pathname: "/slack/link", search: "?workspace=ws_1", hash: "#t=hx_sl_abc" }, sessionStorage, replace); // true; replace("/slack/link?workspace=ws_1")
 */
export function captureSlackReturn(
  url: { pathname: string; search: string; hash: string },
  storage: Storage | null,
  replace: (path: string) => void,
  now = Date.now(),
): boolean {
  const ok = (v: unknown): v is string =>
    typeof v === "string" && v.length > 0 && v.length <= 512;
  let kept: SlackReturn | null = null;
  if (url.pathname === "/slack/installed") {
    if (!url.search) return false;
    const q = new URLSearchParams(url.search),
      code = q.get("code"),
      state = q.get("state"),
      error = q.get("error");
    kept =
      ok(code) && ok(state)
        ? { kind: "installed", code, state, at: now }
        : ok(error) && /^[a-z_]{1,64}$/.test(error)
          ? { kind: "install_error", error, at: now }
          : null;
    replace("/slack/installed");
  } else if (url.pathname === "/slack/link") {
    if (!url.hash) return false;
    const token = new URLSearchParams(url.hash.slice(1)).get("t");
    kept =
      ok(token) && token.startsWith("hx_sl_")
        ? { kind: "link", token, at: now }
        : null;
    replace("/slack/link" + url.search);
  } else return false;
  // Without the tab's storage, or with it blocked, the page says there is
  // nothing to finish.
  if (!storage) return false;
  try {
    if (kept) storage.setItem("foundry.slack.return.v1", JSON.stringify(kept));
    else storage.removeItem("foundry.slack.return.v1");
  } catch {
    return false;
  }
  return !!kept;
}

/**
 * The inline script the root layout runs in <head> (hosted only), before
 * the tracker or the app can read the URL: captureSlackReturn on this page's
 * location and tab storage, cleaning the URL with history.replaceState.
 * Storage can throw (blocked); then nothing is kept and the page says so.
 */
export const SLACK_RETURN_BOOT = `try{(${captureSlackReturn.toString()})(location,(function(){try{return sessionStorage}catch(e){return null}})(),function(p){history.replaceState(history.state,"",p)})}catch(e){}`;

/**
 * This tab's kept return of `kind`, while it is fresh; null for none, an
 * old one, another kind's, or one this build can't read.
 *
 * @example
 * readSlackReturn(sessionStorage, "link"); // { kind: "link", token: "hx_sl_abc", at: 1767000000000 }
 */
export function readSlackReturn<K extends SlackReturn["kind"]>(
  storage: Storage | null,
  kind: K | readonly K[],
  now = Date.now(),
): Extract<SlackReturn, { kind: K }> | null {
  let raw: string | null = null;
  try {
    raw = storage?.getItem(KEY) ?? null;
  } catch {
    return null;
  }
  let value: unknown;
  try {
    value = JSON.parse(raw || "null");
  } catch {
    return null;
  }
  if (!value || typeof value !== "object") return null;
  const r = value as Record<string, unknown>,
    kinds: readonly string[] = typeof kind === "string" ? [kind] : kind;
  if (
    typeof r.at !== "number" ||
    now - r.at > SLACK_RETURN_TTL_MS ||
    r.at > now ||
    !kinds.includes(r.kind as string)
  )
    return null;
  const well =
    r.kind === "installed"
      ? fits(r.code) && fits(r.state)
      : r.kind === "install_error"
        ? fits(r.error) && ERROR_CODE.test(r.error)
        : fits(r.token) && r.token.startsWith(SLACK_LINK_TOKEN_PREFIX);
  return well ? (r as Extract<SlackReturn, { kind: K }>) : null;
}

/** Forgets this tab's kept return: spent, refused or abandoned. */
export function clearSlackReturn(storage: Storage | null) {
  try {
    storage?.removeItem(KEY);
  } catch {
    // Storage blocked: nothing was kept.
  }
}
