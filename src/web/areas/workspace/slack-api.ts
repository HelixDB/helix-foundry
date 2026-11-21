import type {
  FoundrySlack,
  FoundrySlackInstall,
  FoundrySlackLink,
  FoundrySlackLinkRefusal,
  FoundrySlackLinkResponse,
  FoundrySlackResponse,
  ListFoundrySlackLinksResponse,
  PreviewFoundrySlackLinkResponse,
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
 * Starts adding the app: Slack's page to send the browser to, an http(s)
 * address only. The event goes before the page leaves.
 */
export async function startSlackInstall(ws: string): Promise<string> {
  const { authorizeUrl } = await api<StartFoundrySlackInstallResponse>(
    path(ws) + ":start-install",
    {},
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

/** Reports a failed install's reason, once, as the page shows it. */
export const installFailed = (reason: SlackInstallFailure) =>
  trackEvent("slack_install_failed", { reason });

/**
 * Finishes adding the app with what Slack returned: the workspace's Slack
 * after it, which names the workspace the state was for.
 */
export async function finishSlackInstall(
  code: string,
  state: string,
): Promise<FoundrySlack> {
  try {
    const { slack } = await api<FoundrySlackResponse>(
      "/v1/foundry/slack:finish-install",
      { code, state },
    );
    trackEvent("slack_installed");
    return slack || {};
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
