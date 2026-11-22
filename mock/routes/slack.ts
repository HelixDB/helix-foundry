import { randomBytes } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import type {
  FinishFoundrySlackInstallRequest,
  FoundrySlack,
  FoundrySlackInstall,
  FoundrySlackLink,
  FoundrySlackLinkRefusal,
  FoundrySlackLinkResponse,
  FoundrySlackLinkTokenRequest,
  FoundrySlackResponse,
  ListFoundrySlackLinksResponse,
  PreviewFoundrySlackLinkResponse,
  StartFoundrySlackInstallResponse,
} from "../../src/shared/wfe";
import { fail, now, ok, type MockReply, type MockRoute } from "../http";
import { escape, page, send } from "../pages";
import {
  config,
  db,
  displayName,
  isReply,
  LOCAL_TOKEN,
  membership,
  onReset,
  type Membership,
} from "../state";
import { foundryPrecondition } from "./ai";

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
// (EMAIL_MISMATCH). A local API refuses every route, as the backend does.

type Knobs = {
  slackAvailable?: boolean;
  slackInstallAllowed?: boolean;
  slackTeamTaken?: boolean;
  slackEmailMismatch?: boolean;
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
type Link = { slackUserId: string; userId: string; createdAt: string };
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
    { workspaceId: string; userId: string; expires: number; used: boolean }
  >(),
  // Codes the mock Slack page gave, by code: the state they answer.
  codes: new Map<string, string>(),
  tokens: new Map<string, LinkToken>(),
};
onReset(() => {
  state.installs.clear();
  state.links.clear();
  state.states.clear();
  state.codes.clear();
  state.tokens.clear();
  delete knobs.slackAvailable;
  delete knobs.slackInstallAllowed;
  delete knobs.slackTeamTaken;
  delete knobs.slackEmailMismatch;
});

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
  if (LOCAL_TOKEN)
    return fail(
      "FAILED_PRECONDITION",
      "Slack reaches a local install through Helix's relay",
    );
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
  };
};

/** The workspace's Slack as the caller sees it. */
function view(m: Membership): FoundrySlack {
  const install = state.installs.get(m.workspaceId),
    mine = (state.links.get(m.workspaceId) || []).find(
      (l) => l.userId === m.userId,
    );
  return {
    workspaceId: m.workspaceId,
    installAllowed: knobs.slackInstallAllowed !== false,
    canManage: manages(m),
    install: install || null,
    myLink: install && mine ? linkOf(mine) : null,
  };
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
    token.expires < Date.now()
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
    handle: ({ params, principal: { user } }) => {
      const m = slackMember(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      const refused = installRefusal(m);
      if (refused) return refused;
      if (state.installs.has(m.workspaceId))
        return foundryPrecondition(
          m.workspaceId,
          "FOUNDRY_SLACK_INSTALLED",
          "this workspace is connected to Slack already; disconnect it first",
        );
      const s = randomBytes(32).toString("base64url");
      state.states.set(s, {
        workspaceId: m.workspaceId,
        userId: user.id,
        expires: Date.now() + STATE_TTL_MS,
        used: false,
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
      const req = (body || {}) as Partial<FinishFoundrySlackInstallRequest>;
      if (!req.code || !req.state)
        return fail(
          "INVALID_ARGUMENT",
          "code and state are what Slack returned, at most 512 characters",
        );
      const s = state.states.get(req.state);
      if (!s || s.used || s.expires < Date.now() || s.userId !== user.id)
        return foundryPrecondition(
          "",
          "FOUNDRY_SLACK_INSTALL_EXPIRED",
          "this install expired or was finished already: add Helix Foundry to Slack again from Settings › Slack",
        );
      s.used = true;
      const m = membership(s.workspaceId, user.id);
      if (isReply(m)) return m;
      const refused = installRefusal(m);
      if (refused) return refused;
      if (state.codes.get(req.code) !== req.state)
        return foundryPrecondition(
          m.workspaceId,
          "FOUNDRY_SLACK_REFUSED",
          "Slack refused the install (invalid_code): add Helix Foundry to Slack again",
        );
      state.codes.delete(req.code);
      if (knobs.slackTeamTaken)
        return foundryPrecondition(
          m.workspaceId,
          "FOUNDRY_SLACK_TEAM_TAKEN",
          "this Slack workspace is connected to another Foundry workspace: disconnect it there, or a Slack admin can remove Helix Foundry in Slack's app settings",
        );
      const ws = db.workspaces.get(m.workspaceId),
        at = now(),
        was = state.installs.get(m.workspaceId);
      // The same Slack workspace again is a reinstall, which keeps its links.
      state.installs.set(m.workspaceId, {
        teamId: was?.teamId || "T0" + m.workspaceId.toUpperCase().slice(3, 9),
        teamName: `${ws?.displayName || ws?.slug || "Mock"} Slack`,
        botUserId: "U0HELIXBOT",
        installedByUserId: user.id,
        installedByName: displayName(user),
        createdAt: was?.createdAt || at,
        updatedAt: at,
      });
      return ok({ slack: view(m) } satisfies FoundrySlackResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}:disconnect`,
    handle: ({ params, principal: { user } }) => {
      const m = slackMember(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      if (!manages(m)) return fail("PERMISSION_DENIED", "permission denied");
      if (!state.installs.has(m.workspaceId))
        return fail("NOT_FOUND", "this workspace isn't connected to Slack");
      state.installs.delete(m.workspaceId);
      state.links.delete(m.workspaceId);
      for (const [raw, t] of state.tokens)
        if (t.workspaceId === m.workspaceId) state.tokens.delete(raw);
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
        slackEmailMasked: maskEmail(
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
          (l) => l.slackUserId !== t.slackUserId && l.userId !== user.id,
        ),
        link,
      ]);
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
];

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
  const slackUserId = body.slackUser || "U0" + handle.toUpperCase().slice(0, 8);
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
    s = form.get("state") || "";
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
