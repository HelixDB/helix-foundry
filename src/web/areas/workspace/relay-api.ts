import type {
  AuthorizeFoundryRelayLinkRequest,
  AuthorizeFoundryRelayLinkResponse,
  CompleteFoundryRelayLinkRequest,
  FoundryRelay,
  FoundryRelayLink,
  FoundryRelayResponse,
  FoundrySlackAccessResponse,
  FoundrySlackLink,
  ListFoundryRelayLinksResponse,
  SetFoundrySlackAccessRequest,
  StartFoundryRelayLinkResponse,
  UpdateFoundryRelayRequest,
} from "../../../shared/wfe";
import { trackBeforeLeaving, trackEvent } from "../../analytics";
import { api } from "../../api";
import { relayRedirect, slackKey, slackReason } from "./slack-api";
import type { SlackReturn } from "./slack-return";

// Helix's Slack relay, both ends of it. A local install's owner links it to
// a Helix workspace from its Settings › Slack (start, then complete with the
// code Helix hands back), chooses whether their Slack account may approve
// changes through Slack, unlinks it, and allows Slack users who asked to
// read and ask as them. On the hosted site, an owner or admin authorizes a
// link on /relay/link, and lists and revokes a workspace's linked installs
// in Settings › Slack.

const relayPath = (ws: string) =>
  `/v1/workspaces/${encodeURIComponent(ws)}/foundry/relay`;
const linksPath = (ws: string) =>
  `/v1/workspaces/${encodeURIComponent(ws)}/foundry/relay-links`;

/** Helix last answering a claim within this long reads as in contact. */
export const RELAY_ONLINE_MS = 90_000;

/**
 * Whether `name` is a local install's name as Helix keeps it (backend
 * model.ValidRelayName): not blank, at most 128 bytes of UTF-8, and no
 * control or bidi control character, which could make one row read as
 * another.
 *
 * @example
 * relayNameOk("Ada’s MacBook"); // true
 * relayNameOk("evil‮koobcam"); // false
 */
export const relayNameOk = (name: string) =>
  name.trim() !== "" &&
  new TextEncoder().encode(name).length <= 128 &&
  !/[\p{Cc}\p{Bidi_Control}]/u.test(name);

/** A local install's request to be linked, as /relay/link reads it. */
export type RelayLinkRequest = {
  state: string;
  codeChallenge: string;
  redirectUri: string;
  // Its Settings › Slack's origin, which the page names.
  origin: string;
  name: string;
};

/**
 * A kept /relay/link request (slack-return.ts) the page may act on: null
 * when its Settings › Slack isn't one on this computer, or its name isn't
 * one Helix keeps.
 *
 * @example
 * relayLinkRequest({ kind: "relay_link", state: "s".repeat(16), codeChallenge: "c".repeat(43), redirectUri: "http://localhost:3000/settings/slack", name: "Laptop", at: 0 })?.origin; // "http://localhost:3000"
 */
export function relayLinkRequest(
  kept: Extract<SlackReturn, { kind: "relay_link" }>,
): RelayLinkRequest | null {
  const origin = relayRedirect(kept.redirectUri);
  if (!origin || !relayNameOk(kept.name)) return null;
  const { state, codeChallenge, redirectUri, name } = kept;
  return { state, codeChallenge, redirectUri, origin, name };
}

/**
 * Where Cancel sends the browser: the install's Settings › Slack, saying it
 * was cancelled, with its state.
 *
 * @example
 * relayCancelUrl({ redirectUri: "http://localhost:3000/settings/slack", state: "s1" }); // "http://localhost:3000/settings/slack?error=access_denied&state=s1"
 */
export const relayCancelUrl = (
  r: Pick<RelayLinkRequest, "redirectUri" | "state">,
) =>
  `${r.redirectUri}?${new URLSearchParams({ error: "access_denied", state: r.state })}`;

/** Why authorizing a link failed, in a word for analytics. */
export type RelayLinkRefusal =
  | "not_offered"
  | "unavailable"
  | "invalid"
  | "error";

/**
 * Authorizes linking the install to workspace `ws`, for an owner or admin:
 * where to send the browser, which must be the install's own Settings ›
 * Slack with the code and its state (anything else is refused here). The
 * event goes before the page leaves.
 */
export async function authorizeRelayLink(
  ws: string,
  r: RelayLinkRequest,
): Promise<string> {
  let redirectUrl: string | undefined;
  try {
    ({ redirectUrl } = await api<AuthorizeFoundryRelayLinkResponse>(
      linksPath(ws) + ":authorize",
      {
        state: r.state,
        codeChallenge: r.codeChallenge,
        redirectUri: r.redirectUri,
        name: r.name,
      } satisfies AuthorizeFoundryRelayLinkRequest,
    ));
  } catch (e) {
    const reason = slackReason(e);
    trackEvent("relay_link_refused", {
      reason:
        reason === "FOUNDRY_SLACK_NOT_OFFERED"
          ? "not_offered"
          : reason === "FOUNDRY_RELAY_DISABLED" ||
              reason === "FOUNDRY_SLACK_DISABLED"
            ? "unavailable"
            : "error",
    });
    throw e;
  }
  const back = redirectUrl?.startsWith(r.redirectUri + "?")
    ? new URLSearchParams(redirectUrl.slice(r.redirectUri.length + 1))
    : null;
  if (!back?.get("code") || back.get("state") !== r.state) {
    trackEvent("relay_link_refused", { reason: "invalid" });
    throw new Error(
      "Helix’s answer didn’t go back to that Foundry. Try again.",
    );
  }
  await trackBeforeLeaving("relay_link_authorized");
  return redirectUrl!;
}

export const relayLinksKey = (ws: string) => [...slackKey(ws), "relay-links"];
/**
 * The workspace's linked installs, newest first, for owners and admins;
 * null where Helix's relay is off (FOUNDRY_RELAY_DISABLED), which the page
 * then doesn't mention.
 */
export const relayLinksQuery = (ws: string) => ({
  queryKey: relayLinksKey(ws),
  queryFn: async (): Promise<FoundryRelayLink[] | null> => {
    try {
      return (
        (await api<ListFoundryRelayLinksResponse>(linksPath(ws))).links || []
      );
    } catch (e) {
      const reason = slackReason(e);
      if (
        reason === "FOUNDRY_RELAY_DISABLED" ||
        reason === "FOUNDRY_SLACK_DISABLED"
      )
        return null;
      throw e;
    }
  },
});

/** Revokes a linked install: its token stops working, and its Slack installs leave Slack. */
export async function revokeRelayLink(ws: string, linkId: string) {
  await api<void>(`${linksPath(ws)}/${encodeURIComponent(linkId)}:revoke`, {});
  trackEvent("relay_link_revoked");
}

/**
 * A local install's link to Helix as its Settings › Slack shows it:
 * unlinked, or linked to the Helix account `accountEmail`, with when Helix
 * last answered (null until it has) and whether the owner's Slack account
 * may approve changes through Slack.
 */
export type RelayView =
  | { kind: "unlinked" }
  | {
      kind: "linked";
      hostedOrigin: string;
      accountEmail: string;
      linkedAt: string | null;
      lastOkAt: string | null;
      approvals: boolean;
    };

/**
 * The link from the WFE's answer: anything but `linked` true is unlinked.
 *
 * @example
 * relayView({ linked: true, accountEmail: "ada@example.com", slackApprovals: true }).kind; // "linked"
 * relayView(undefined); // { kind: "unlinked" }
 */
export function relayView(r: FoundryRelay | undefined): RelayView {
  if (!r?.linked) return { kind: "unlinked" };
  return {
    kind: "linked",
    hostedOrigin: r.hostedOrigin || "",
    accountEmail: r.accountEmail || "",
    linkedAt: r.linkedAt || null,
    lastOkAt: r.lastOkAt || null,
    approvals: !!r.slackApprovals,
  };
}

/**
 * Whether Helix is answering the install: it never has yet, it did within
 * RELAY_ONLINE_MS, or it last did at `since` (a time from the future reads
 * as in contact, a clock apart).
 *
 * @example
 * relayContact(null, 0); // { kind: "never" }
 * relayContact("1970-01-01T00:00:00Z", 1000); // { kind: "online" }
 */
export function relayContact(
  lastOkAt: string | null,
  now: number,
): { kind: "never" } | { kind: "online" } | { kind: "offline"; since: string } {
  const at = lastOkAt ? Date.parse(lastOkAt) : NaN;
  if (Number.isNaN(at)) return { kind: "never" };
  return now - at <= RELAY_ONLINE_MS
    ? { kind: "online" }
    : { kind: "offline", since: lastOkAt! };
}

export const relayKey = (ws: string) => [...slackKey(ws), "relay"];
/** A local install's link to Helix, for its owner. */
export const relayQuery = (ws: string) => ({
  queryKey: relayKey(ws),
  queryFn: async () =>
    relayView((await api<FoundryRelayResponse>(relayPath(ws))).relay),
});

/**
 * Starts linking the install to Helix: Helix's /relay/link page to send the
 * browser to, an http(s) address only.
 */
export async function startRelayLink(ws: string): Promise<string> {
  const { authorizeUrl } = await api<StartFoundryRelayLinkResponse>(
    relayPath(ws) + ":start-link",
    {},
  );
  let url: URL;
  try {
    url = new URL(authorizeUrl || "");
  } catch {
    throw new Error("Helix’s page didn’t come back. Try again.");
  }
  if (url.protocol !== "https:" && url.protocol !== "http:")
    throw new Error("Helix’s page didn’t come back. Try again.");
  trackEvent("relay_link_started");
  return url.toString();
}

/** Why linking to Helix failed, in a word for analytics. */
export type RelayLinkFailure = "cancelled" | "expired" | "error";

/** Finishes linking with what Helix handed back. */
export async function completeRelayLink(
  ws: string,
  code: string,
  state: string,
): Promise<RelayView> {
  try {
    const { relay } = await api<FoundryRelayResponse>(
      relayPath(ws) + ":complete-link",
      { code, state } satisfies CompleteFoundryRelayLinkRequest,
    );
    trackEvent("relay_linked");
    return relayView(relay);
  } catch (e) {
    trackEvent("relay_link_failed", {
      reason:
        slackReason(e) === "FOUNDRY_RELAY_LINK_EXPIRED" ? "expired" : "error",
    });
    throw e;
  }
}

/** Turns approving changes from Slack on or off; off, Slack only reads and asks. */
export async function setSlackApprovals(
  ws: string,
  on: boolean,
): Promise<RelayView> {
  const { relay } = await api<FoundryRelayResponse>(
    relayPath(ws),
    { slackApprovals: on } satisfies UpdateFoundryRelayRequest,
    "PATCH",
  );
  trackEvent("slack_approvals_changed", { approvals: on ? "on" : "off" });
  return relayView(relay);
}

/** Unlinks the install: Helix revokes its link and its Slack installs. */
export async function unlinkRelay(ws: string): Promise<RelayView> {
  const { relay } = await api<FoundryRelayResponse>(
    relayPath(ws) + ":unlink",
    {},
  );
  trackEvent("relay_unlinked");
  return relayView(relay);
}

/**
 * Lets a Slack user who asked read and ask as the owner ("read"), or
 * removes them, or their request ("none"): their link once allowed.
 */
export async function setSlackAccess(
  ws: string,
  slackUserId: string,
  access: "read" | "none",
): Promise<FoundrySlackLink | null> {
  const { link } = await api<FoundrySlackAccessResponse>(
    `/v1/workspaces/${encodeURIComponent(ws)}/foundry/slack/links/${encodeURIComponent(slackUserId)}/access`,
    {
      access:
        access === "read"
          ? "FOUNDRY_SLACK_LINK_ACCESS_READ"
          : "FOUNDRY_SLACK_LINK_ACCESS_NONE",
    } satisfies SetFoundrySlackAccessRequest,
    "PUT",
  );
  trackEvent("slack_access_changed", { access });
  return link || null;
}
