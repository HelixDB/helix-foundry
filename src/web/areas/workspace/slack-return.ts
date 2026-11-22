// What Slack's and Helix's ways back to the site carry, kept in this tab
// before any call, and the URL then cleaned: Slack's return from adding the
// app (/slack/installed?code=&state=, or ?error=&state= when the person
// cancelled), the one-time link the app sends a person in Slack
// (/slack/link?workspace=<id>#t=<token>), a local install's request to link
// it to Helix (the hosted site's /relay/link?state=&code_challenge=
// &redirect_uri=&name=), and Helix's answer to that request back on the
// local install (/settings/slack?code=&state=, or ?error= when cancelled).
// The root layout runs SLACK_RETURN_BOOT in <head>, before any of the
// site's scripts, so a sign-in on the way (which returns to the page without
// its query or fragment) loses none of them, and none reaches the server's
// logs, a sign-in's return path or the tracker (which loads later, from the
// page). Small and free of the area's other modules: the root layout
// imports it.

/** The app's link tokens: "hx_sl_" and base64url (backend api/slack.go). */
export const SLACK_LINK_TOKEN_PREFIX = "hx_sl_";
/** A kept return is dropped after this: a link token and a state live ten minutes. */
export const SLACK_RETURN_TTL_MS = 10 * 60_000;
const KEY = "foundry.slack.return.v1";
// Longer than any code, state or token Slack or the app sends.
const MAX = 512;

/**
 * What a way back carried: a finished install, a cancelled one (with its
 * state when Slack sent it, so the WFE can say where the install was
 * started), a link, a
 * local install asking to be linked to Helix (its state, PKCE challenge,
 * Settings › Slack and name, as it sent them), or Helix's answer to it on
 * the local install: a code for its state, or a cancel.
 */
export type SlackReturn =
  | { kind: "installed"; code: string; state: string; at: number }
  | { kind: "install_error"; error: string; state?: string; at: number }
  | { kind: "link"; token: string; at: number }
  | {
      kind: "relay_link";
      state: string;
      codeChallenge: string;
      redirectUri: string;
      name: string;
      at: number;
    }
  | { kind: "relay_return"; code: string; state: string; at: number }
  | { kind: "relay_error"; error: string; at: number };

const fits = (v: unknown): v is string =>
  typeof v === "string" && v.length > 0 && v.length <= MAX;
/** Slack's OAuth error codes (access_denied, invalid_scope, ...). */
const ERROR_CODE = /^[a-z_]{1,64}$/;
/** A local install's state for its link (backend api relayStatePattern). */
const RELAY_STATE = /^[A-Za-z0-9._~-]{16,256}$/;
/** An S256 PKCE challenge: 43 base64url characters. */
const RELAY_CHALLENGE = /^[A-Za-z0-9_-]{43}$/;

/**
 * Keeps what a way back from Slack or Helix carried in `storage` and
 * replaces the URL with one without it (the link's ?workspace= stays, for
 * the app to open that workspace). Any other URL, or one already cleaned
 * (Settings › Slack without a code, state or error), is left alone.
 * Returns whether something was kept; a malformed return (an error that
 * isn't one of Slack's codes, a token without its prefix, a link request
 * whose state or challenge isn't one an install makes) is cleaned and kept
 * as nothing, which the page then says.
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
          ? ok(state)
            ? { kind: "install_error", error, state, at: now }
            : { kind: "install_error", error, at: now }
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
  } else if (url.pathname === "/relay/link") {
    if (!url.search) return false;
    const q = new URLSearchParams(url.search),
      state = q.get("state"),
      codeChallenge = q.get("code_challenge"),
      redirectUri = q.get("redirect_uri"),
      name = q.get("name");
    kept =
      ok(state) &&
      /^[A-Za-z0-9._~-]{16,256}$/.test(state) &&
      ok(codeChallenge) &&
      /^[A-Za-z0-9_-]{43}$/.test(codeChallenge) &&
      ok(redirectUri) &&
      ok(name)
        ? {
            kind: "relay_link",
            state,
            codeChallenge,
            redirectUri,
            name,
            at: now,
          }
        : null;
    replace("/relay/link");
  } else if (url.pathname === "/settings/slack") {
    const q = new URLSearchParams(url.search),
      code = q.get("code"),
      state = q.get("state"),
      error = q.get("error");
    // Settings › Slack's own links carry neither.
    if (!q.has("code") && !q.has("state") && !q.has("error")) return false;
    kept =
      ok(code) && ok(state)
        ? { kind: "relay_return", code, state, at: now }
        : ok(error) && /^[a-z_]{1,64}$/.test(error)
          ? { kind: "relay_error", error, at: now }
          : null;
    replace("/settings/slack");
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
 * The inline script the root layout runs in <head>, before
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
    r.kind === "installed" || r.kind === "relay_return"
      ? fits(r.code) && fits(r.state)
      : r.kind === "install_error" || r.kind === "relay_error"
        ? fits(r.error) &&
          ERROR_CODE.test(r.error) &&
          (r.kind === "relay_error" || r.state === undefined || fits(r.state))
        : r.kind === "relay_link"
          ? fits(r.state) &&
            RELAY_STATE.test(r.state) &&
            fits(r.codeChallenge) &&
            RELAY_CHALLENGE.test(r.codeChallenge) &&
            fits(r.redirectUri) &&
            fits(r.name)
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
