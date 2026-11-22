import { randomBytes } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import type {
  FinishFoundrySlackInstallRequest,
  FoundrySlackAccessRequest,
  FoundrySlackAccessResponse,
  FoundrySlack,
  FoundrySlackInstall,
  FoundrySlackLink,
  FoundrySlackLinkRefusal,
  FoundrySlackLinkResponse,
  FoundrySlackLinkTokenRequest,
  FoundrySlackResponse,
  ListFoundrySlackLinksResponse,
  PreviewFoundrySlackLinkResponse,
  SetFoundrySlackAccessRequest,
  StartFoundrySlackInstallRequest,
  StartFoundrySlackInstallResponse,
  UpdateFoundrySlackRequest,
} from "../../src/shared/wfe";
import { fail, now, ok, type MockReply, type MockRoute } from "../http";
import { escape, page, send } from "../pages";
import {
  config,
  db,
  displayName,
  isReply,
  LOCAL_TOKEN,
  PROD_STAMP,
  membership,
  onReset,
  type Membership,
} from "../state";
import { liveKey, makeBotKey, revokeKey } from "./access";
import { foundryPrecondition } from "./ai";
import { relayLinked } from "./relay";

// Helix Foundry for Slack as the backend serves it (backend api/slack.go),
// in memory: one install per workspace, the OAuth states an install is
// started with, the one-time link tokens the app sends people in Slack, and
// who is linked. Slack itself is the mock's own page:
// GET /mock/slack/authorize stands in for Slack's page that asks to add the
// app, and returns to the site's /slack/installed with a code and the state
// (Allow) or error=access_denied (Cancel).
//
// The app's message to a person in Slack is POST /__mock/slack/link-token
// { email, slackUser?, name?, handle?, workspaceId?, guest?, unconfirmed? }:
// a link token of the workspace's install (Acme's by default) for that Slack
// user, answered { token, url } with the site's /slack/link URL. Its email
// decides whether they may be linked: the caller's, confirmed, and no guest.
//
// Knobs, through POST /__mock/config: `slackAvailable: false` is
// foundry.slack off (every route FOUNDRY_SLACK_DISABLED), `slackInstallAllowed:
// false` takes the workspace off the install list (FOUNDRY_SLACK_NOT_OFFERED),
// `slackTeamTaken: true` has another Foundry workspace hold the Slack
// workspace an install finishes in (FOUNDRY_SLACK_TEAM_TAKEN), and
// `slackEmailMismatch: true` makes every Slack user's email someone else's
// (EMAIL_MISMATCH). A local API serves them through Helix's relay (below).
//
// The Slack data connector on the app (foundry.slack.feed, on here, but on
// prod's stamp, unless `slackFeedOff: true`; ./data.ts binds connections to the install): an
// install says the permissions it lacks (missingScopes: with
// `slackScopesOld: true` it lacks the history scopes, as one made before
// the feed was on), whether a Slack admin or owner made it
// (`slackInstallerMember: true`: not), and when Slack last held back the
// team's events (`slackEventsLimited: true`: an hour ago). Each is read as
// the install is, so turning one off stands for adding the app again.
// Start installs again while the install lacks a permission or a Slack
// admin's install, and carries a return path back through Finish.
// Disconnecting remembers the team, which a connection that read it says
// it was removed from, and when.
//
// Bot mode (PATCH .../foundry/slack, owners and admins): BOT makes the
// shared bot a key of Settings › Access (./access.ts's makeBotKey: no
// grants, its token no one's), named "Slack bot (<team>)" cut to 64
// characters; PER_PERSON revokes it with its grants, as a disconnect does.
// The bot is `broken` once its key is revoked in Access, or with
// `slackBotBroken: true` (its last turn refused, as the backend reads it).
//
// A local API (MOCK_LOCAL_TOKEN) serves the same routes as a local install
// does, through Helix's relay (./relay.ts): Start needs the install linked
// to Helix (FOUNDRY_RELAY_NOT_LINKED), and the mock Slack page's Allow
// stands for Helix finishing the install, going back to the install's
// Settings › Slack (FOUNDRY_ORIGIN) with no /slack/installed of its own.
// The Slack account that added it (U0ADA) is the owner: only it gets a
// link, which links it as the owner with no email to compare (another Slack
// user's link token is FOUNDRY_SLACK_LINK_EXPIRED, as the bot never sends
// one). Anyone else who messages the app asks to use it: POST
// /__mock/slack/access-request { slackUser, name, workspaceId? }, which the
// owner allows read-only or drops (PUT .../links/{slackUserId}/access).
// There is no bot mode (FOUNDRY_SLACK_BOT_HOSTED_ONLY). Unlinking from Helix
// forgets the install, its links and requests (relayUnlinked). Hosted Helix
// refuses PUT .../access with FOUNDRY_SLACK_LOCAL_ONLY.

type Knobs = {
  slackAvailable?: boolean;
  slackInstallAllowed?: boolean;
  slackTeamTaken?: boolean;
  slackEmailMismatch?: boolean;
  slackFeedOff?: boolean;
  slackScopesOld?: boolean;
  slackInstallerMember?: boolean;
  slackEventsLimited?: boolean;
  slackBotBroken?: boolean;
  // Founder off (./agent.ts): an install can't start.
  agentEnabled?: boolean;
};
const knobs = config as typeof config & Knobs;

const STATE_TTL_MS = 10 * 60_000,
  TOKEN_TTL_MS = 10 * 60_000,
  TOKEN_PREFIX = "hx_sl_";

type Install = Required<
  Pick<
    FoundrySlackInstall,
    | "teamId"
    | "teamName"
    | "botUserId"
    | "installedByUserId"
    | "installedByName"
    | "createdAt"
    | "updatedAt"
  >
>;
type Link = {
  slackUserId: string;
  userId: string;
  createdAt: string;
  // A local install's Slack user the owner allowed: reads and asks as them.
  readOnly?: boolean;
};
/** The Slack account that adds the app on a local install: its owner. */
export const LOCAL_INSTALLER = "U0ADA";
type LinkToken = {
  workspaceId: string;
  slackUserId: string;
  name: string;
  handle: string;
  email: string;
  confirmed: boolean;
  member: boolean;
  expires: number;
  used: boolean;
};

const state = {
  installs: new Map<string, Install>(),
  // By workspace, oldest first.
  links: new Map<string, Link[]>(),
  states: new Map<
    string,
    {
      workspaceId: string;
      userId: string;
      returnPath: string;
      expires: number;
      used: boolean;
      // A local install's, started through Helix's relay.
      relayed?: boolean;
      // Hosted: one a linked local install started through Foundry's door
      // (startRelayedSlackInstall): Finish records the Slack team on the
      // link, not the workspace, and sends the browser here.
      returnTo?: string;
      onInstalled?: (teamId: string, teamName: string) => void;
    }
  >(),
  // The Slack workspace each Foundry workspace was last disconnected from,
  // and when.
  removed: new Map<string, { teamId: string; teamName: string; at: number }>(),
  // The shared bot's key, by workspace, while its install is in bot mode.
  bots: new Map<string, string>(),
  // Codes the mock Slack page gave, by code: the state they answer.
  codes: new Map<string, string>(),
  tokens: new Map<string, LinkToken>(),
  // A local install's Slack users who asked to use it, by workspace,
  // oldest first.
  requests: new Map<string, Required<FoundrySlackAccessRequest>[]>(),
};
onReset(() => {
  state.requests.clear();
  state.installs.clear();
  state.links.clear();
  state.states.clear();
  state.codes.clear();
  state.tokens.clear();
  state.removed.clear();
  state.bots.clear();
  delete knobs.slackAvailable;
  delete knobs.slackInstallAllowed;
  delete knobs.slackTeamTaken;
  delete knobs.slackEmailMismatch;
  delete knobs.slackFeedOff;
  delete knobs.slackScopesOld;
  delete knobs.slackInstallerMember;
  delete knobs.slackEventsLimited;
  delete knobs.slackBotBroken;
});

/** The scopes the feed needs beyond the bot's (backend slack.HistoryScopes). */
const HISTORY_SCOPES = ["channels:history", "groups:history"];
/** The storage notice binding a connection takes (backend slackFeedNotice). */
export const SLACK_NOTICE_VERSION = 1;
/** How long a removed install's connections keep their data (feed.orphan_retention). */
export const SLACK_ORPHAN_MS = 7 * 86_400_000;

/**
 * Whether a Slack connection may read through the app here
 * (foundry.slack.feed): off in a local API and on prod's stamp (pnpm
 * docs:media), as prod has it until Slack agrees.
 */
export const slackFeedOn = () =>
  !LOCAL_TOKEN &&
  !PROD_STAMP &&
  knobs.slackAvailable !== false &&
  !knobs.slackFeedOff;

/**
 * The workspace's install as the backend shows it: with the permissions it
 * lacks under the feed, whether a Slack admin or owner made it, and when
 * Slack last held back its events (within a day); undefined without one.
 */
export function slackInstallOf(
  workspaceId: string,
): FoundrySlackInstall | undefined {
  const install = state.installs.get(workspaceId);
  if (!install) return undefined;
  const bot = state.bots.get(workspaceId),
    key = bot ? liveKey(workspaceId, bot) : undefined;
  return {
    ...install,
    identityMode: bot
      ? "FOUNDRY_SLACK_IDENTITY_MODE_BOT"
      : "FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON",
    bot: bot
      ? {
          credentialId: bot,
          credentialName: key?.name || "",
          broken: !key || !!knobs.slackBotBroken,
        }
      : null,
    missingScopes: slackFeedOn() && knobs.slackScopesOld ? HISTORY_SCOPES : [],
    installerAdmin: !knobs.slackInstallerMember,
    ...(knobs.slackEventsLimited && {
      eventsLimitedAt: new Date(Date.now() - 3_600_000).toISOString(),
    }),
  };
}

/** The Slack workspace `workspaceId` was last disconnected from, and when. */
export const slackRemoved = (workspaceId: string) =>
  state.removed.get(workspaceId);

/** Whether `m` may add the app (again) now: an owner or admin, offered it, with Founder on. */
export const canInstallSlack = (m: Membership) => !installRefusal(m);

/**
 * Where an install may return once finished (backend slackReturnPath):
 * Settings › Slack (""), the Data area's Slack form or a connection's
 * page; null for any other. The mock's connector ids have an underscore.
 */
function returnPathOf(p: unknown): string | null {
  if (p === undefined || p === "" || p === "/settings/slack") return "";
  if (p === "/data?add=slack") return p;
  return typeof p === "string" &&
    /^\/data\/connections\/[0-9A-Za-z_]{1,64}$/.test(p)
    ? p
    : null;
}

const BASE = "/v1/workspaces/{workspaceId}/foundry/slack";
const manages = (m: Membership) =>
  m.role === "WORKSPACE_ROLE_OWNER" || m.role === "WORKSPACE_ROLE_ADMIN";
const site = () =>
  process.env.NEXT_PUBLIC_WORKOS_REDIRECT_URI || "http://localhost:3000";

/**
 * The caller's membership of a workspace whose Slack routes answer, or the
 * backend's refusal: a local API's, foundry.slack off, or no member.
 */
function slackMember(
  workspaceId: string,
  userId: string,
): Membership | MockReply {
  if (knobs.slackAvailable === false)
    return foundryPrecondition(
      workspaceId,
      "FOUNDRY_SLACK_DISABLED",
      "Helix Foundry for Slack isn't available here",
    );
  return membership(workspaceId, userId);
}

const linkOf = (l: Link): FoundrySlackLink => {
  const user = db.users.get(l.userId);
  return {
    slackUserId: l.slackUserId,
    userId: l.userId,
    userName: user ? displayName(user) : l.userId,
    createdAt: l.createdAt,
    access: l.readOnly
      ? "FOUNDRY_SLACK_LINK_ACCESS_READ"
      : "FOUNDRY_SLACK_LINK_ACCESS_FULL",
  };
};

/** The workspace's Slack as the caller sees it. */
function view(m: Membership): FoundrySlack {
  const install = state.installs.get(m.workspaceId),
    mine = (state.links.get(m.workspaceId) || []).find(
      (l) => l.userId === m.userId && !l.readOnly,
    );
  return {
    workspaceId: m.workspaceId,
    installAllowed: knobs.slackInstallAllowed !== false,
    canManage: manages(m),
    install: slackInstallOf(m.workspaceId) || null,
    myLink: install && mine ? linkOf(mine) : null,
  };
}

/**
 * Records the workspace's install as `userId` finishes it: the same Slack
 * workspace again is a reinstall, which keeps its links.
 */
function install(workspaceId: string, userId: string) {
  const ws = db.workspaces.get(workspaceId),
    user = db.users.get(userId),
    at = now(),
    was = state.installs.get(workspaceId);
  state.installs.set(workspaceId, {
    teamId: was?.teamId || "T0" + workspaceId.toUpperCase().slice(3, 9),
    teamName: `${ws?.displayName || ws?.slug || "Mock"} Slack`,
    botUserId: "U0HELIXBOT",
    installedByUserId: userId,
    installedByName: user ? displayName(user) : userId,
    createdAt: was?.createdAt || at,
    updatedAt: at,
  });
}

/**
 * Starts adding the app for a linked local install, as the door's
 * /v1/relay/slack-install does for the link's creator: Slack's page, which
 * returns to the site's /slack/installed; Finish then calls `installed`
 * with the Slack team and sends the browser to `returnTo`.
 */
export function startRelayedSlackInstall(
  workspaceId: string,
  userId: string,
  returnTo: string,
  installed: (teamId: string, teamName: string) => void,
): string {
  const s = randomBytes(32).toString("base64url");
  state.states.set(s, {
    workspaceId,
    userId,
    returnPath: "",
    expires: Date.now() + STATE_TTL_MS,
    used: false,
    returnTo,
    onInstalled: installed,
  });
  const url = new URL("/mock/slack/authorize", process.env.MOCK_ORIGIN);
  url.searchParams.set("state", s);
  return url.toString();
}

/**
 * Forgets a local install's Slack, as unlinking it from Helix does
 * (./relay.ts): its installs, their links, requests, tokens and states.
 */
export function relayUnlinked() {
  state.installs.clear();
  state.links.clear();
  state.requests.clear();
  state.tokens.clear();
  state.states.clear();
}

/**
 * `reply` with `returnTo` (a linked local install's Settings › Slack) as
 * its ErrorInfo's metadata return_to, as the backend's slackReturnTo adds
 * it: to the ErrorInfo it has, else one of FOUNDRY_SLACK_INSTALL_FAILED.
 */
function withReturnTo(reply: MockReply, returnTo: string): MockReply {
  const body = (reply.body || {}) as { details?: unknown[] };
  const details = [...(body.details || [])];
  const at = details.findIndex(
    (d) =>
      !!d &&
      typeof d === "object" &&
      (d as { "@type"?: unknown })["@type"] ===
        "type.googleapis.com/google.rpc.ErrorInfo",
  );
  const info = (
    at >= 0
      ? details[at]
      : {
          "@type": "type.googleapis.com/google.rpc.ErrorInfo",
          reason: "FOUNDRY_SLACK_INSTALL_FAILED",
          domain: "foundry.helix-db.com",
        }
  ) as { metadata?: Record<string, string> };
  const withIt = {
    ...info,
    metadata: { ...info.metadata, return_to: returnTo },
  };
  if (at >= 0) details[at] = withIt;
  else details.push(withIt);
  return { ...reply, body: { ...body, details } };
}

/** What may stop an install, for an owner or admin, now. */
function installRefusal(m: Membership): MockReply | null {
  if (!manages(m)) return fail("PERMISSION_DENIED", "permission denied");
  if (knobs.slackInstallAllowed === false)
    return foundryPrecondition(
      m.workspaceId,
      "FOUNDRY_SLACK_NOT_OFFERED",
      "Helix Foundry for Slack isn't offered to this workspace yet",
    );
  if (knobs.agentEnabled === false)
    return foundryPrecondition(
      m.workspaceId,
      "FOUNDRY_AGENT_DISABLED",
      "Founder isn't available here, so it can't answer in Slack",
    );
  return null;
}

/** Why the token's Slack user can't be linked to `email`, if they can't. */
function refusalFor(t: LinkToken, email: string): FoundrySlackLinkRefusal {
  if (!t.member) return "FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER";
  // A local install's owner, the app's installer: no email to compare.
  if (LOCAL_TOKEN) return "FOUNDRY_SLACK_LINK_REFUSAL_UNSPECIFIED";
  if (
    knobs.slackEmailMismatch ||
    t.email.trim().toLowerCase() !== email.trim().toLowerCase()
  )
    return "FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH";
  if (!t.confirmed) return "FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_UNCONFIRMED";
  return "FOUNDRY_SLACK_LINK_REFUSAL_UNSPECIFIED";
}

const maskEmail = (email: string) => {
  const at = email.lastIndexOf("@");
  return at <= 0 ? "" : email[0] + "•••" + email.slice(at);
};

/** The link token `raw` names in the caller's workspace, or the backend's refusal. */
function liveToken(
  workspaceId: string,
  raw: unknown,
): { token: LinkToken; hash: string } | MockReply {
  if (typeof raw !== "string" || !raw.startsWith(TOKEN_PREFIX))
    return fail(
      "INVALID_ARGUMENT",
      "token is the one Helix Foundry's link carries",
    );
  const token = state.tokens.get(raw);
  if (
    !state.installs.has(workspaceId) ||
    !token ||
    token.workspaceId !== workspaceId ||
    token.used ||
    token.expires < Date.now() ||
    // A local install links only the Slack account that added the app.
    (LOCAL_TOKEN && token.slackUserId !== LOCAL_INSTALLER)
  )
    return foundryPrecondition(
      workspaceId,
      "FOUNDRY_SLACK_LINK_EXPIRED",
      "this link expired or was used: message Helix Foundry in Slack for a new one",
    );
  return { token, hash: raw };
}

const isToken = (
  x: { token: LinkToken; hash: string } | MockReply,
): x is { token: LinkToken; hash: string } => "token" in x;

const LINK_REFUSALS: Record<string, string> = {
  FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH:
    "this Slack account's email isn't your Foundry email: sign in to Foundry as the person the Slack account is",
  FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_UNCONFIRMED:
    "Slack hasn't confirmed this Slack account's email: confirm it in Slack, then open the link again",
  FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER:
    "this Slack account isn't a full member of the connected Slack workspace: guests and people of other organisations can't use Helix Foundry",
};

export const slackRoutes: MockRoute[] = [
  {
    method: "GET",
    path: BASE,
    handle: ({ params, principal: { user } }) => {
      const m = slackMember(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      return ok({ slack: view(m) } satisfies FoundrySlackResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}:start-install`,
    handle: ({ params, body, principal: { user } }) => {
      const m = slackMember(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      const returnPath = returnPathOf(
        (body as Partial<StartFoundrySlackInstallRequest> | undefined)
          ?.returnPath,
      );
      if (returnPath === null)
        return fail(
          "INVALID_ARGUMENT",
          "return_path is /settings/slack, /data?add=slack or a Slack connection's page",
        );
      const refused = installRefusal(m);
      if (refused) return refused;
      // Added again while it lacks what the feed needs: a permission, or a
      // Slack admin or owner's install.
      const install = slackInstallOf(m.workspaceId);
      if (
        install &&
        !install.missingScopes?.length &&
        (!slackFeedOn() || install.installerAdmin)
      )
        return foundryPrecondition(
          m.workspaceId,
          "FOUNDRY_SLACK_INSTALLED",
          "this workspace is connected to Slack already; disconnect it first",
        );
      if (LOCAL_TOKEN && !relayLinked())
        return foundryPrecondition(
          m.workspaceId,
          "FOUNDRY_RELAY_NOT_LINKED",
          "link this Foundry to Helix first: Slack reaches it through Helix",
        );
      const s = randomBytes(32).toString("base64url");
      state.states.set(s, {
        workspaceId: m.workspaceId,
        userId: user.id,
        returnPath,
        expires: Date.now() + STATE_TTL_MS,
        used: false,
        relayed: !!LOCAL_TOKEN,
      });
      const url = new URL("/mock/slack/authorize", process.env.MOCK_ORIGIN);
      url.searchParams.set("client_id", "mock-client");
      url.searchParams.set(
        "scope",
        "app_mentions:read,im:history,im:write,chat:write,assistant:write,users:read,users:read.email,channels:read,groups:read,mpim:read",
      );
      url.searchParams.set(
        "redirect_uri",
        new URL("/slack/installed", site()).toString(),
      );
      url.searchParams.set("state", s);
      return ok({
        authorizeUrl: url.toString(),
      } satisfies StartFoundrySlackInstallResponse);
    },
  },
  {
    method: "POST",
    path: "/v1/foundry/slack:finish-install",
    handle: ({ body, principal: { user } }) => {
      if (LOCAL_TOKEN)
        return fail("FAILED_PRECONDITION", "Helix finishes a relayed install");
      if (knobs.slackAvailable === false)
        return foundryPrecondition(
          "",
          "FOUNDRY_SLACK_DISABLED",
          "Helix Foundry for Slack isn't available here",
        );
      const req = (body || {}) as Partial<
        FinishFoundrySlackInstallRequest & { code: string; error: string }
      >;
      const cancelled =
        !req.code &&
        typeof req.error === "string" &&
        /^[a-z_]{1,64}$/.test(req.error);
      if ((!req.code && !cancelled) || (req.code && req.error) || !req.state)
        return fail(
          "INVALID_ARGUMENT",
          "state and a code, or Slack's error code, are what Slack returned, at most 256 characters",
        );
      const s = state.states.get(req.state);
      if (!s || s.used || s.expires < Date.now() || s.userId !== user.id)
        return foundryPrecondition(
          "",
          "FOUNDRY_SLACK_INSTALL_EXPIRED",
          "this install expired or was finished already: add Helix Foundry to Slack again from Settings › Slack",
        );
      s.used = true;
      // Past the state, a local install's refusal says where it goes back.
      const back = (reply: MockReply) =>
        s.returnTo ? withReturnTo(reply, s.returnTo) : reply;
      if (cancelled)
        return back(
          foundryPrecondition(
            s.workspaceId,
            "FOUNDRY_SLACK_CANCELLED",
            "Helix Foundry wasn't added: adding it to Slack was cancelled",
          ),
        );
      const m = membership(s.workspaceId, user.id);
      if (isReply(m)) return back(m);
      const refused = installRefusal(m);
      if (refused) return back(refused);
      if (state.codes.get(req.code!) !== req.state)
        return back(
          foundryPrecondition(
            m.workspaceId,
            "FOUNDRY_SLACK_REFUSED",
            "Slack refused the install (invalid_code): add Helix Foundry to Slack again",
          ),
        );
      state.codes.delete(req.code!);
      if (knobs.slackTeamTaken)
        return back(
          foundryPrecondition(
            m.workspaceId,
            "FOUNDRY_SLACK_TEAM_TAKEN",
            "this Slack workspace is connected to another Foundry workspace: disconnect it there, or a Slack admin can remove Helix Foundry in Slack's app settings",
          ),
        );
      if (s.returnTo) {
        s.onInstalled?.(
          "T0LOCAL" + req.state.slice(0, 4).toUpperCase(),
          "Laptop Slack",
        );
        return ok({
          slack: view(m),
          returnTo: s.returnTo,
        } satisfies FoundrySlackResponse);
      }
      install(m.workspaceId, user.id);
      return ok({
        slack: view(m),
        returnPath: s.returnPath,
      } satisfies FoundrySlackResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}:disconnect`,
    handle: ({ params, principal: { user } }) => {
      const m = slackMember(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      if (!manages(m)) return fail("PERMISSION_DENIED", "permission denied");
      const install = state.installs.get(m.workspaceId);
      if (!install)
        return fail("NOT_FOUND", "this workspace isn't connected to Slack");
      state.removed.set(m.workspaceId, {
        teamId: install.teamId,
        teamName: install.teamName,
        at: Date.now(),
      });
      state.installs.delete(m.workspaceId);
      state.links.delete(m.workspaceId);
      state.requests.delete(m.workspaceId);
      const bot = state.bots.get(m.workspaceId);
      if (bot) revokeKey(m.workspaceId, bot);
      state.bots.delete(m.workspaceId);
      for (const [raw, t] of state.tokens)
        if (t.workspaceId === m.workspaceId) state.tokens.delete(raw);
      return ok({ slack: view(m) } satisfies FoundrySlackResponse);
    },
  },
  {
    method: "PATCH",
    path: BASE,
    handle: ({ params, body, principal: { user } }) => {
      const m = slackMember(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      const mode = (body as Partial<UpdateFoundrySlackRequest> | undefined)
        ?.identityMode;
      if (
        mode !== "FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON" &&
        mode !== "FOUNDRY_SLACK_IDENTITY_MODE_BOT"
      )
        return fail("INVALID_ARGUMENT", "identity_mode is PER_PERSON or BOT");
      if (!manages(m)) return fail("PERMISSION_DENIED", "permission denied");
      const install = state.installs.get(m.workspaceId);
      if (!install)
        return fail("NOT_FOUND", "this workspace isn't connected to Slack");
      if (LOCAL_TOKEN && mode === "FOUNDRY_SLACK_IDENTITY_MODE_BOT")
        return foundryPrecondition(
          m.workspaceId,
          "FOUNDRY_SLACK_BOT_HOSTED_ONLY",
          "the shared bot is Helix's: a Foundry on your computer answers as its owner",
        );
      const bot = state.bots.get(m.workspaceId);
      if (mode === "FOUNDRY_SLACK_IDENTITY_MODE_BOT" && !bot) {
        // service_credentials.name holds 64 characters.
        const name = [...`Slack bot (${install.teamName})`];
        state.bots.set(
          m.workspaceId,
          makeBotKey(
            m.workspaceId,
            name.length > 64
              ? [...name.slice(0, 62), "…)"].join("")
              : name.join(""),
            user.id,
          ),
        );
      } else if (mode === "FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON" && bot) {
        revokeKey(m.workspaceId, bot);
        state.bots.delete(m.workspaceId);
      }
      return ok({ slack: view(m) } satisfies FoundrySlackResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/links`,
    handle: ({ params, principal: { user } }) => {
      const m = slackMember(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      if (!manages(m)) return fail("PERMISSION_DENIED", "permission denied");
      return ok({
        links: (state.links.get(m.workspaceId) || []).map(linkOf),
        requests: state.requests.get(m.workspaceId) || [],
      } satisfies ListFoundrySlackLinksResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/links:preview`,
    handle: ({ params, body, principal: { user } }) => {
      const m = slackMember(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      const found = liveToken(
        m.workspaceId,
        (body as Partial<FoundrySlackLinkTokenRequest> | undefined)?.token,
      );
      if (!isToken(found)) return found;
      const t = found.token,
        refusal = refusalFor(t, user.email);
      return ok({
        teamName: state.installs.get(m.workspaceId)!.teamName,
        slackUserId: t.slackUserId,
        slackName: t.name,
        slackHandle: t.handle,
        // Helix's relay keeps Slack's emails from a local install.
        slackEmailMasked: LOCAL_TOKEN
          ? ""
          : maskEmail(
              knobs.slackEmailMismatch ? "someone@elsewhere.example" : t.email,
            ),
        matchesYou: refusal === "FOUNDRY_SLACK_LINK_REFUSAL_UNSPECIFIED",
        refusal,
      } satisfies PreviewFoundrySlackLinkResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/links`,
    handle: ({ params, body, principal: { user } }) => {
      const m = slackMember(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      const found = liveToken(
        m.workspaceId,
        (body as Partial<FoundrySlackLinkTokenRequest> | undefined)?.token,
      );
      if (!isToken(found)) return found;
      const t = found.token,
        refusal = refusalFor(t, user.email);
      if (refusal !== "FOUNDRY_SLACK_LINK_REFUSAL_UNSPECIFIED") {
        const reply = fail("FAILED_PRECONDITION", LINK_REFUSALS[refusal]!);
        return {
          ...reply,
          body: {
            ...(reply.body as object),
            details: [
              {
                "@type": "type.googleapis.com/google.rpc.ErrorInfo",
                reason: "FOUNDRY_SLACK_LINK_REFUSED",
                domain: "foundry.helix-db.com",
                metadata: { workspace_id: m.workspaceId, refusal },
              },
            ],
          },
        };
      }
      t.used = true;
      // It replaces the Slack user's earlier link, and the caller's.
      const link: Link = {
        slackUserId: t.slackUserId,
        userId: user.id,
        createdAt: now(),
      };
      state.links.set(m.workspaceId, [
        ...(state.links.get(m.workspaceId) || []).filter(
          (l) =>
            l.slackUserId !== t.slackUserId &&
            (l.userId !== user.id || l.readOnly),
        ),
        link,
      ]);
      state.requests.set(
        m.workspaceId,
        (state.requests.get(m.workspaceId) || []).filter(
          (r) => r.slackUserId !== t.slackUserId,
        ),
      );
      return ok({ link: linkOf(link) } satisfies FoundrySlackLinkResponse);
    },
  },
  {
    method: "DELETE",
    path: `${BASE}/links/{slackUserId}`,
    handle: ({ params, principal: { user } }) => {
      const m = slackMember(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      const links = state.links.get(m.workspaceId) || [],
        link = links.find((l) => l.slackUserId === params.slackUserId);
      if (!state.installs.has(m.workspaceId) || !link)
        return fail("NOT_FOUND", "that Slack account isn't linked");
      if (link.userId !== user.id && !manages(m))
        return fail("PERMISSION_DENIED", "permission denied");
      state.links.set(
        m.workspaceId,
        links.filter((l) => l !== link),
      );
      return ok({});
    },
  },
  {
    method: "PUT",
    path: `${BASE}/links/{slackUserId}/access`,
    handle: ({ params, body, principal: { user } }) => {
      const ws = params.workspaceId!,
        slackUser = params.slackUserId!,
        access = (body as Partial<SetFoundrySlackAccessRequest> | undefined)
          ?.access;
      if (!/^[UW][A-Z0-9]{2,31}$/.test(slackUser))
        return fail(
          "INVALID_ARGUMENT",
          "workspace_id and slack_user_id are required",
        );
      if (
        access !== "FOUNDRY_SLACK_LINK_ACCESS_READ" &&
        access !== "FOUNDRY_SLACK_LINK_ACCESS_NONE"
      )
        return fail("INVALID_ARGUMENT", "access is READ or NONE");
      const m = slackMember(ws, user.id);
      if (isReply(m)) return m;
      if (!LOCAL_TOKEN)
        return foundryPrecondition(
          ws,
          "FOUNDRY_SLACK_LOCAL_ONLY",
          "on Helix each person links their own Slack account",
        );
      if (!manages(m)) return fail("PERMISSION_DENIED", "permission denied");
      if (!state.installs.has(ws))
        return fail("NOT_FOUND", "this workspace isn't connected to Slack");
      const links = state.links.get(ws) || [],
        link = links.find((l) => l.slackUserId === slackUser),
        requests = state.requests.get(ws) || [];
      if (link && !link.readOnly)
        return foundryPrecondition(
          ws,
          "FOUNDRY_SLACK_OWNER_LINK",
          "that is the owner's own Slack account, which unlinks from Settings › Slack",
        );
      if (!link && !requests.some((r) => r.slackUserId === slackUser))
        return fail(
          "NOT_FOUND",
          "that Slack user hasn't asked to use this Foundry",
        );
      state.requests.set(
        ws,
        requests.filter((r) => r.slackUserId !== slackUser),
      );
      if (access === "FOUNDRY_SLACK_LINK_ACCESS_NONE") {
        state.links.set(
          ws,
          links.filter((l) => l !== link),
        );
        return ok({} satisfies FoundrySlackAccessResponse);
      }
      const allowed: Link = link || {
        slackUserId: slackUser,
        userId: user.id,
        createdAt: now(),
        readOnly: true,
      };
      if (!link) state.links.set(ws, [...links, allowed]);
      return ok({
        link: linkOf(allowed),
      } satisfies FoundrySlackAccessResponse);
    },
  },
];

/**
 * A Slack user asking to use a local install, as the app records them when
 * they message it (POST /__mock/slack/access-request): neither the owner
 * nor someone already allowed. Asking again keeps the first request.
 */
export function slackAccessRequest(body: {
  slackUser?: string;
  name?: string;
  workspaceId?: string;
}): MockReply {
  const ws = body.workspaceId || "ws_acme";
  if (
    !LOCAL_TOKEN ||
    !state.installs.has(ws) ||
    !body.slackUser ||
    !/^[UW][A-Z0-9]{2,31}$/.test(body.slackUser) ||
    body.slackUser === LOCAL_INSTALLER
  )
    return fail(
      "FAILED_PRECONDITION",
      "a local install's Slack user, other than its owner, asks to use it",
    );
  const requests = state.requests.get(ws) || [];
  if ((state.links.get(ws) || []).some((l) => l.slackUserId === body.slackUser))
    return ok({ requested: false });
  if (!requests.some((r) => r.slackUserId === body.slackUser))
    state.requests.set(ws, [
      ...requests,
      {
        slackUserId: body.slackUser,
        slackName: body.name || body.slackUser,
        createdAt: now(),
      },
    ]);
  return ok({ requested: true });
}

/**
 * The app's message to a person in Slack: a link token of the workspace's
 * install for that Slack user, and the site's link URL (POST
 * /__mock/slack/link-token).
 */
export function slackLinkToken(body: {
  email?: string;
  slackUser?: string;
  name?: string;
  handle?: string;
  workspaceId?: string;
  guest?: boolean;
  unconfirmed?: boolean;
}): MockReply {
  const workspaceId = body.workspaceId || "ws_acme";
  if (!state.installs.has(workspaceId) || !body.email)
    return fail(
      "FAILED_PRECONDITION",
      "the workspace has no Slack install, or no email was given",
    );
  const token = TOKEN_PREFIX + randomBytes(32).toString("base64url"),
    handle = body.handle || body.email.split("@")[0]!;
  const slackUserId =
    body.slackUser ||
    (LOCAL_TOKEN ? LOCAL_INSTALLER : "U0" + handle.toUpperCase().slice(0, 8));
  // At most one live token per Slack user.
  for (const [raw, t] of state.tokens)
    if (t.workspaceId === workspaceId && t.slackUserId === slackUserId)
      state.tokens.delete(raw);
  state.tokens.set(token, {
    workspaceId,
    slackUserId,
    name: body.name || handle,
    handle,
    email: body.email,
    confirmed: !body.unconfirmed,
    member: !body.guest,
    expires: Date.now() + TOKEN_TTL_MS,
    used: false,
  });
  return ok({
    token,
    url: `/slack/link?workspace=${encodeURIComponent(workspaceId)}#t=${token}`,
  });
}

/**
 * Slack's page that asks to add the app (/mock/slack/authorize): Allow
 * returns to the site's /slack/installed with a code and the state, Cancel
 * with error=access_denied. It returns only to the site's own address.
 */
export function handleSlackPage(
  req: IncomingMessage,
  res: ServerResponse,
  url: URL,
  raw: string,
): boolean {
  if (url.pathname !== "/mock/slack/authorize") return false;
  const back = new URL("/slack/installed", site());
  if (req.method === "GET") {
    const s = url.searchParams.get("state") || "",
      waiting = state.states.get(s);
    const ws = db.workspaces.get(waiting?.workspaceId || "");
    send(
      res,
      waiting && ws ? 200 : 404,
      waiting && ws
        ? page(
            "Add Helix Foundry to Slack",
            `<h1>Helix Foundry</h1><p>Mock Slack. Helix Foundry asks to be added to ${escape(ws.displayName || ws.slug || ws.id)} Slack, to read messages that mention it and its direct messages, and to answer them.</p>
<form method="post" action="/mock/slack/authorize"><input type="hidden" name="state" value="${escape(s)}"><button type="submit" name="choice" value="allow">Allow</button><button type="submit" class="secondary" name="choice" value="cancel">Cancel</button></form>`,
          )
        : page("Mock Slack", "<h1>This request has expired</h1>"),
    );
    return true;
  }
  const form = new URLSearchParams(raw),
    s = form.get("state") || "",
    relayed = state.states.get(s);
  // A local install's: Helix finishes it and goes back to the install's
  // Settings › Slack.
  if (relayed?.relayed && !relayed.used) {
    relayed.used = true;
    if (form.get("choice") === "allow")
      install(relayed.workspaceId, relayed.userId);
    res.writeHead(303, {
      Location: `${process.env.FOUNDRY_ORIGIN || "http://localhost:3000"}/settings/slack`,
    });
    res.end();
    return true;
  }
  back.searchParams.set("state", s);
  if (form.get("choice") === "allow") {
    const code = randomBytes(16).toString("hex");
    state.codes.set(code, s);
    back.searchParams.set("code", code);
  } else back.searchParams.set("error", "access_denied");
  res.writeHead(303, { Location: back.toString() });
  res.end();
  return true;
}
