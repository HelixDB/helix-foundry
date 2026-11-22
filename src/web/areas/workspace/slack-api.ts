import type {
  FoundrySlack,
  FoundrySlackInstall,
  FoundrySlackLink,
  FoundrySlackLinkRefusal,
  FoundrySlackLinkResponse,
  FoundrySlackResponse,
  ListFoundrySlackLinksResponse,
  UpdateFoundrySlackRequest,
  PreviewFoundrySlackLinkResponse,
  StartFoundrySlackInstallRequest,
  StartFoundrySlackInstallResponse,
} from "../../../shared/wfe";
import { trackBeforeLeaving, trackEvent } from "../../analytics";
import { api, ApiError } from "../../api";

// Settings › Slack's WFE calls and words (hosted only): Helix's own Slack
// app, Helix Foundry, which answers people in one Slack workspace as
// Founder, each linked Slack user as their own Foundry user. Owners and
// admins add it by Slack's OAuth and disconnect it; each person links their
// own Slack account from the one-time link the app sends them.

const path = (ws: string) =>
  `/v1/workspaces/${encodeURIComponent(ws)}/foundry/slack`;

/** What the page says about answers in Slack, wherever it shows the app. */
export const SLACK_NOTICE =
  "Answers in a channel are visible to everyone in it, guests included; ask in a DM for private answers. Helix Foundry doesn’t answer in channels shared with other organisations. AI answers can be wrong; check the cited sources.";

/** The reasons the Slack routes refuse with (backend api/slack.go). */
export const SLACK_REASONS = [
  "FOUNDRY_SLACK_DISABLED",
  "FOUNDRY_SLACK_NOT_OFFERED",
  "FOUNDRY_SLACK_INSTALLED",
  "FOUNDRY_AGENT_DISABLED",
  "FOUNDRY_SLACK_INSTALL_EXPIRED",
  "FOUNDRY_SLACK_REFUSED",
  "FOUNDRY_SLACK_ENTERPRISE_INSTALL",
  "FOUNDRY_SLACK_SCOPES",
  "FOUNDRY_SLACK_TEAM_TAKEN",
  "FOUNDRY_SLACK_DISCONNECTING",
  "FOUNDRY_SLACK_LINK_EXPIRED",
  "FOUNDRY_SLACK_LINK_REFUSED",
  "FOUNDRY_SLACK_BOT_HOSTED_ONLY",
] as const;
export type SlackReason = (typeof SLACK_REASONS)[number];

const errorInfo = (error: unknown) =>
  error instanceof ApiError
    ? (error.details || []).flatMap((d) => {
        if (!d || typeof d !== "object") return [];
        const { reason, metadata } = d as {
          reason?: unknown;
          metadata?: unknown;
        };
        const known = SLACK_REASONS.find((r) => r === reason);
        return known
          ? [
              {
                reason: known,
                metadata: (metadata && typeof metadata === "object"
                  ? metadata
                  : {}) as Record<string, unknown>,
              },
            ]
          : [];
      })[0]
    : undefined;

/**
 * The Slack reason a call was refused with (its ErrorInfo), if it gave one.
 *
 * @example
 * slackReason(new ApiError("taken", 400, 9, [{ reason: "FOUNDRY_SLACK_TEAM_TAKEN" }])); // "FOUNDRY_SLACK_TEAM_TAKEN"
 * slackReason(new Error("offline")); // undefined
 */
export const slackReason = (error: unknown): SlackReason | undefined =>
  errorInfo(error)?.reason;

/**
 * Whom an install's bot acts as: each linked Slack user as themselves, or
 * one shared bot, a workspace key no one holds, whose sources Settings ›
 * Access chooses (its `credentialId`; null when the WFE didn't say).
 * `broken`: the key was revoked, deleted, expired or given database
 * permissions, so the bot can't answer until bot mode is turned off and on.
 */
export type SlackMode =
  | { kind: "per_person" }
  | {
      kind: "bot";
      credentialId: string | null;
      credentialName: string;
      broken: boolean;
    };

/**
 * An install's mode as the WFE says it: BOT is the shared bot, anything
 * else (PER_PERSON, unset from a WFE before bot mode) per person.
 *
 * @example
 * slackMode({ identityMode: "FOUNDRY_SLACK_IDENTITY_MODE_BOT", bot: { credentialId: "sc_1", credentialName: "Slack bot (Acme)" } });
 * // { kind: "bot", credentialId: "sc_1", credentialName: "Slack bot (Acme)", broken: false }
 * slackMode({}); // { kind: "per_person" }
 */
export function slackMode(install: FoundrySlackInstall): SlackMode {
  if (install.identityMode !== "FOUNDRY_SLACK_IDENTITY_MODE_BOT")
    return { kind: "per_person" };
  const bot = install.bot || {};
  return {
    kind: "bot",
    credentialId: bot.credentialId || null,
    credentialName: bot.credentialName || "",
    broken: !!bot.broken,
  };
}

/** Settings › Slack's state: what the workspace has and what the caller may do. */
export type SlackView =
  // Helix Foundry for Slack is off where this site's API runs.
  | { kind: "unavailable" }
  // Helix doesn't offer it to this workspace yet (its install list).
  | { kind: "not_offered"; canManage: boolean }
  | { kind: "not_installed"; canManage: boolean }
  | {
      kind: "installed";
      canManage: boolean;
      install: FoundrySlackInstall;
      mode: SlackMode;
      myLink: FoundrySlackLink | null;
    };

/**
 * The page's state from GET .../foundry/slack's answer: an install shows
 * whether or not the workspace is still on the install list (it stays until
 * disconnected).
 *
 * @example
 * slackView({ installAllowed: true, canManage: true }); // { kind: "not_installed", canManage: true }
 * slackView({ installAllowed: false }); // { kind: "not_offered", canManage: false }
 */
export function slackView(s: FoundrySlack | undefined): SlackView {
  const canManage = !!s?.canManage;
  if (s?.install)
    return {
      kind: "installed",
      canManage,
      install: s.install,
      mode: slackMode(s.install),
      myLink: s.myLink || null,
    };
  return {
    kind: s?.installAllowed ? "not_installed" : "not_offered",
    canManage,
  };
}

export const slackKey = (ws: string) => [ws, "foundry-slack"];
/** The workspace's Slack; FOUNDRY_SLACK_DISABLED reads as unavailable. */
export const slackQuery = (ws: string) => ({
  queryKey: slackKey(ws),
  queryFn: async (): Promise<SlackView> => {
    try {
      return slackView((await api<FoundrySlackResponse>(path(ws))).slack);
    } catch (e) {
      if (slackReason(e) === "FOUNDRY_SLACK_DISABLED")
        return { kind: "unavailable" };
      throw e;
    }
  },
});

/** Who is linked, oldest first: owners and admins only. */
export const slackLinksQuery = (ws: string) => ({
  queryKey: [...slackKey(ws), "links"],
  queryFn: async () =>
    (await api<ListFoundrySlackLinksResponse>(path(ws) + "/links")).links || [],
});

/**
 * Where an install may return to once finished, as the WFE allows it:
 * Settings › Slack (its own path, or empty), the Data area's Slack form,
 * or a connection's page; null for anything else, which could send the
 * person elsewhere.
 *
 * @example
 * slackReturnTo("/data?add=slack"); // "/data?add=slack"
 * slackReturnTo("/data/connections/conn_1"); // "/data/connections/conn_1"
 * slackReturnTo(""); // "/settings/slack"
 * slackReturnTo("https://evil.example/"); // null
 */
export function slackReturnTo(path: string | undefined): string | null {
  if (!path || path === "/settings/slack") return "/settings/slack";
  if (path === "/data?add=slack") return path;
  // The WFE's ids are letters and digits; the mock's have an underscore.
  return /^\/data\/connections\/[0-9A-Za-z_]{1,64}$/.test(path) ? path : null;
}

/**
 * Starts adding the app, or adding it again for the permissions it lacks:
 * Slack's page to send the browser to, an http(s) address only, coming
 * back to `returnPath` once finished (Settings › Slack by default). The
 * event goes before the page leaves.
 */
export async function startSlackInstall(
  ws: string,
  returnPath?: string,
): Promise<string> {
  const { authorizeUrl } = await api<StartFoundrySlackInstallResponse>(
    path(ws) + ":start-install",
    (returnPath
      ? { returnPath }
      : {}) satisfies StartFoundrySlackInstallRequest,
  );
  let url: URL;
  try {
    url = new URL(authorizeUrl || "");
  } catch {
    throw new Error("Slack’s page didn’t come back. Try again.");
  }
  if (url.protocol !== "https:" && url.protocol !== "http:")
    throw new Error("Slack’s page didn’t come back. Try again.");
  await trackBeforeLeaving("slack_install_started");
  return url.toString();
}

/** Why adding the app failed, in a word for analytics. */
export type SlackInstallFailure =
  | "cancelled"
  | "expired"
  | "refused"
  | "enterprise"
  | "scopes"
  | "team_taken"
  | "disconnecting"
  | "installed"
  | "not_offered"
  | "agent_disabled"
  | "unavailable"
  | "error";

/**
 * Why a finish (or a start) was refused, in a word.
 *
 * @example
 * installFailure(new ApiError("taken", 400, 9, [{ reason: "FOUNDRY_SLACK_TEAM_TAKEN" }])); // "team_taken"
 * installFailure(new TypeError("Failed to fetch")); // "error"
 */
export function installFailure(error: unknown): SlackInstallFailure {
  switch (slackReason(error)) {
    case "FOUNDRY_SLACK_INSTALL_EXPIRED":
      return "expired";
    case "FOUNDRY_SLACK_REFUSED":
      return "refused";
    case "FOUNDRY_SLACK_ENTERPRISE_INSTALL":
      return "enterprise";
    case "FOUNDRY_SLACK_SCOPES":
      return "scopes";
    case "FOUNDRY_SLACK_TEAM_TAKEN":
      return "team_taken";
    case "FOUNDRY_SLACK_DISCONNECTING":
      return "disconnecting";
    case "FOUNDRY_SLACK_INSTALLED":
      return "installed";
    case "FOUNDRY_SLACK_NOT_OFFERED":
      return "not_offered";
    case "FOUNDRY_AGENT_DISABLED":
      return "agent_disabled";
    case "FOUNDRY_SLACK_DISABLED":
      return "unavailable";
    default:
      return "error";
  }
}

/**
 * What Slack's return said when it carried no code: the person cancelled
 * (access_denied), or Slack refused.
 *
 * @example
 * slackReturnFailure("access_denied"); // "cancelled"
 */
export const slackReturnFailure = (error: string): SlackInstallFailure =>
  error === "access_denied" ? "cancelled" : "refused";

/**
 * What /slack/installed says of a return that carried no code, in words of
 * its own: Slack's `error` is never shown, since anyone can send a link
 * with any text in it.
 *
 * @example
 * slackReturnCopy("access_denied").title; // "Helix Foundry wasn’t added"
 * slackReturnCopy("invalid_scope").text; // "Slack didn’t add Helix Foundry. Add it again from Settings › Slack."
 */
export const slackReturnCopy = (
  error: string,
): { title: string; text: string } =>
  slackReturnFailure(error) === "cancelled"
    ? {
        title: "Helix Foundry wasn’t added",
        text: "Adding it to Slack was cancelled. Add it again from Settings › Slack whenever you’re ready.",
      }
    : {
        title: "Slack didn’t add Helix Foundry",
        text: "Slack didn’t add Helix Foundry. Add it again from Settings › Slack.",
      };

/** Reports a failed install's reason, once, as the page shows it. */
export const installFailed = (reason: SlackInstallFailure) =>
  trackEvent("slack_install_failed", { reason });

/**
 * Finishes adding the app with what Slack returned: the workspace's Slack
 * after it, which names the workspace the state was for, and where the
 * install was started from (slackReturnTo).
 */
export async function finishSlackInstall(
  code: string,
  state: string,
): Promise<{ slack: FoundrySlack; returnPath: string }> {
  try {
    const { slack, returnPath } = await api<FoundrySlackResponse>(
      "/v1/foundry/slack:finish-install",
      { code, state },
    );
    trackEvent("slack_installed");
    return {
      slack: slack || {},
      returnPath: slackReturnTo(returnPath) || "/settings/slack",
    };
  } catch (e) {
    installFailed(installFailure(e));
    throw e;
  }
}

/** Disconnects the workspace's install: owners and admins. */
export async function disconnectSlack(ws: string): Promise<SlackView> {
  const { slack } = await api<FoundrySlackResponse>(
    path(ws) + ":disconnect",
    {},
  );
  trackEvent("slack_disconnected");
  return slackView(slack);
}

/**
 * Sets whom the install acts as, for owners and admins: "bot" makes the
 * shared bot's key, "per_person" revokes it and deletes the bot's threads.
 * The mode it has already changes nothing.
 */
export async function updateSlackMode(
  ws: string,
  mode: SlackMode["kind"],
): Promise<SlackView> {
  const { slack } = await api<FoundrySlackResponse>(
    path(ws),
    {
      identityMode:
        mode === "bot"
          ? "FOUNDRY_SLACK_IDENTITY_MODE_BOT"
          : "FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON",
    } satisfies UpdateFoundrySlackRequest,
    "PATCH",
  );
  trackEvent("slack_mode_changed", { mode });
  return slackView(slack);
}

/** Why a person couldn't link their Slack account, in a word for analytics. */
export type SlackLinkFailure =
  | "email_mismatch"
  | "email_unconfirmed"
  | "not_a_member"
  | "expired"
  | "error";

const refusalFailure: Record<
  Exclude<FoundrySlackLinkRefusal, "FOUNDRY_SLACK_LINK_REFUSAL_UNSPECIFIED">,
  SlackLinkFailure
> = {
  FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH: "email_mismatch",
  FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_UNCONFIRMED: "email_unconfirmed",
  FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER: "not_a_member",
};

/**
 * A refusal's word, or undefined for none (UNSPECIFIED, unset, or one this
 * build doesn't know).
 *
 * @example
 * refusalOfLink("FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER"); // "not_a_member"
 * refusalOfLink("FOUNDRY_SLACK_LINK_REFUSAL_UNSPECIFIED"); // undefined
 */
export const refusalOfLink = (
  refusal: string | undefined,
): SlackLinkFailure | undefined =>
  Object.hasOwn(refusalFailure, refusal || "")
    ? refusalFailure[refusal as keyof typeof refusalFailure]
    : undefined;

/**
 * Why a link call failed, in a word: a refusal (its metadata names which),
 * an expired or spent token, or anything else.
 *
 * @example
 * linkFailure(new ApiError("used", 400, 9, [{ reason: "FOUNDRY_SLACK_LINK_EXPIRED" }])); // "expired"
 */
export function linkFailure(error: unknown): SlackLinkFailure {
  const info = errorInfo(error);
  if (info?.reason === "FOUNDRY_SLACK_LINK_EXPIRED") return "expired";
  if (info?.reason === "FOUNDRY_SLACK_LINK_REFUSED")
    return (
      refusalOfLink(
        typeof info.metadata.refusal === "string"
          ? info.metadata.refusal
          : undefined,
      ) || "error"
    );
  return "error";
}

/**
 * What linking the token's Slack user to the caller would do. A refusal, or
 * a token that has expired, is reported as the page shows it.
 */
export async function previewSlackLink(
  ws: string,
  token: string,
): Promise<PreviewFoundrySlackLinkResponse> {
  try {
    const preview = await api<PreviewFoundrySlackLinkResponse>(
      path(ws) + "/links:preview",
      { token },
    );
    const refused = preview.matchesYou
      ? undefined
      : refusalOfLink(preview.refusal) || "error";
    if (refused) trackEvent("slack_link_refused", { reason: refused });
    return preview;
  } catch (e) {
    const reason = linkFailure(e);
    if (reason === "expired") trackEvent("slack_link_refused", { reason });
    throw e;
  }
}

/** Links the token's Slack user to the caller, spending the token. */
export async function createSlackLink(
  ws: string,
  token: string,
): Promise<FoundrySlackLink> {
  try {
    const { link } = await api<FoundrySlackLinkResponse>(path(ws) + "/links", {
      token,
    });
    trackEvent("slack_linked");
    return link || {};
  } catch (e) {
    trackEvent("slack_link_refused", { reason: linkFailure(e) });
    throw e;
  }
}

/** Unlinks a Slack user: the caller's own, or, for owners and admins, anyone's. */
export const deleteSlackLink = (ws: string, slackUserId: string) =>
  api<void>(
    path(ws) + "/links/" + encodeURIComponent(slackUserId),
    undefined,
    "DELETE",
  );

/**
 * Why a preview says the Slack user can't be linked to the caller, in
 * words; null when they can be.
 *
 * @example
 * linkRefusalText({ matchesYou: true }); // null
 * linkRefusalText({ matchesYou: false, refusal: "FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_UNCONFIRMED" }); // "Slack hasn’t confirmed this Slack account’s email. Confirm it in Slack, then open the link again."
 */
export function linkRefusalText(
  preview: PreviewFoundrySlackLinkResponse,
): string | null {
  if (preview.matchesYou) return null;
  switch (refusalOfLink(preview.refusal)) {
    case "email_unconfirmed":
      return "Slack hasn’t confirmed this Slack account’s email. Confirm it in Slack, then open the link again.";
    case "not_a_member":
      return "This Slack account isn’t a full member of the connected Slack workspace. Guests and people of other organisations can’t use Helix Foundry.";
    case "email_mismatch":
    default:
      return "This Slack account’s email isn’t your Foundry email. Sign in to Foundry as the person the Slack account is.";
  }
}
