import { createHash, randomBytes } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import type {
  AuthorizeFoundryRelayLinkRequest,
  AuthorizeFoundryRelayLinkResponse,
  CompleteFoundryRelayLinkRequest,
  FoundryRelay,
  FoundryRelayLink,
  FoundryRelayResponse,
  ListFoundryRelayLinksResponse,
  StartFoundryRelayLinkResponse,
  UpdateFoundryRelayRequest,
} from "../../src/shared/wfe";
import { fail, id, now, ok, type MockReply, type MockRoute } from "../http";
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
import { relayUnlinked, startRelayedSlackInstall } from "./slack";

// Helix's Slack relay as the backend serves it (backend api/relay.go and
// api/relay_local.go), in memory, at both ends.
//
// Hosted (no MOCK_LOCAL_TOKEN): an owner or admin authorizes a local
// install's link on the site's /relay/link (POST .../relay-links:authorize:
// a one-time code bound to the install's PKCE challenge and its loopback
// Settings › Slack, ten minutes), lists the workspace's linked installs and
// revokes them. The install's redeem through Foundry's door is POST
// /__mock/relay/redeem { code, codeVerifier, teams? }: the code and its
// verifier make a link, serving a Slack team per name in `teams`; and its
// Add to Slack is POST /__mock/relay/slack-install { linkId }: the mock
// Slack page, which the site's /slack/installed finishes, sending the
// browser back to the install's Settings › Slack. A local
// API refuses these with FOUNDRY_RELAY_DISABLED, as does `relayAvailable:
// false` (Helix's relay off) or `slackAvailable: false`.
//
// Local (MOCK_LOCAL_TOKEN): the install's one link to Helix, for its owner.
// Start answers the mock's stand-in for Helix's /relay/link,
// /mock/relay/link, which Link sends back to the install's Settings › Slack
// with a code for the state, and Cancel with error=access_denied; Complete
// spends them. Unlinking forgets the link and its Slack installs
// (./slack.ts). Hosted refuses these with FOUNDRY_RELAY_LOCAL_ONLY.
//
// Knobs, through POST /__mock/config: `relayLinked: true` has the install
// linked already (until unlinked), `relayOnline: false` has Helix last
// answer ten minutes ago, and `relayLinkOutcome` makes Complete fail:
// "expired" (FOUNDRY_RELAY_LINK_EXPIRED) or "unavailable" (Helix can't be
// reached).

type Knobs = {
  relayAvailable?: boolean;
  relayLinked?: boolean;
  relayOnline?: boolean;
  relayLinkOutcome?: "expired" | "unavailable";
  slackAvailable?: boolean;
  slackInstallAllowed?: boolean;
};
const knobs = config as typeof config & Knobs;

const CODE_TTL_MS = 10 * 60_000;
/** Helix's origin a local install links to (local.relay.url's default). */
export const RELAY_ORIGIN = "https://ingest.helix-db.com";
/** The local install's name, as its owner's computer calls it. */
export const RELAY_NAME = "Ada’s laptop";

type Code = {
  workspaceId: string;
  userId: string;
  challenge: string;
  redirectUri: string;
  name: string;
  expires: number;
  used: boolean;
};
type Link = {
  id: string;
  workspaceId: string;
  name: string;
  createdByUserId: string;
  createdAt: string;
  // The install's Settings › Slack's origin.
  redirectOrigin: string;
  lastSeenAt: string | null;
  teams: { teamId: string; teamName: string; createdAt: string }[];
};
type Local = {
  accountEmail: string;
  linkedAt: string;
  approvals: boolean;
};

const state = {
  // Hosted: codes by their value, links by id.
  codes: new Map<string, Code>(),
  links: new Map<string, Link>(),
  // Local: the install's link, and the states it started links with, by
  // state: the code /mock/relay/link handed back, once it has.
  local: null as Local | null,
  pending: new Map<
    string,
    { expires: number; used: boolean; code: string | null }
  >(),
};
onReset(() => {
  state.codes.clear();
  state.links.clear();
  state.local = null;
  state.pending.clear();
  delete knobs.relayAvailable;
  delete knobs.relayLinked;
  delete knobs.relayOnline;
  delete knobs.relayLinkOutcome;
});

const manages = (m: Membership) =>
  m.role === "WORKSPACE_ROLE_OWNER" || m.role === "WORKSPACE_ROLE_ADMIN";
const sha256url = (s: string) =>
  createHash("sha256").update(s).digest("base64url");

/**
 * The origin of a local install's Settings › Slack, as Helix accepts it
 * (backend model.ParseRelayRedirect), or null.
 */
function redirectOrigin(uri: unknown): string | null {
  if (typeof uri !== "string" || uri.length > 255) return null;
  const m =
    /^http:\/\/(localhost|127\.0\.0\.1)(?::([1-9][0-9]{0,4}))?\/settings\/slack$/.exec(
      uri,
    );
  return m && Number(m[2] ?? 1) <= 65535
    ? uri.slice(0, -"/settings/slack".length)
    : null;
}
const STATE = /^[A-Za-z0-9._~-]{16,256}$/;
const nameOk = (name: unknown): name is string =>
  typeof name === "string" &&
  name.trim() !== "" &&
  Buffer.byteLength(name) <= 128 &&
  !/[\p{Cc}\p{Bidi_Control}]/u.test(name);

/** The caller's membership where hosted's relay answers, or its refusal. */
function relayMember(
  workspaceId: string,
  userId: string,
): Membership | MockReply {
  const m = membership(workspaceId, userId);
  if (isReply(m)) return m;
  if (
    LOCAL_TOKEN ||
    knobs.relayAvailable === false ||
    knobs.slackAvailable === false
  )
    return foundryPrecondition(
      workspaceId,
      "FOUNDRY_RELAY_DISABLED",
      "Helix's Slack relay isn't available here",
    );
  return m;
}
const denied = () => fail("PERMISSION_DENIED", "permission denied");

/** The local install's link, for its owner, or the backend's refusal. */
function localOwner(workspaceId: string, userId: string): MockReply | null {
  const m = membership(workspaceId, userId);
  if (isReply(m)) return m;
  if (!LOCAL_TOKEN)
    return foundryPrecondition(
      workspaceId,
      "FOUNDRY_RELAY_LOCAL_ONLY",
      "a Foundry install on your computer links to Helix's relay; Helix has no relay of its own",
    );
  return manages(m) ? null : denied();
}

/** The local install's link as it stands: `relayLinked` seeds one. */
function localLink(): Local | null {
  if (!state.local && knobs.relayLinked)
    state.local = {
      accountEmail: "ada@example.com",
      linkedAt: new Date(Date.now() - 86_400_000).toISOString(),
      approvals: true,
    };
  return state.local;
}

/** Whether the local install is linked to Helix (./slack.ts's start). */
export const relayLinked = () => !!localLink();

const relayOf = (): FoundryRelay => {
  const l = localLink();
  if (!l) return { linked: false };
  return {
    linked: true,
    hostedOrigin: RELAY_ORIGIN,
    accountEmail: l.accountEmail,
    linkedAt: l.linkedAt,
    lastOkAt: new Date(
      Date.now() - (knobs.relayOnline === false ? 10 * 60_000 : 5_000),
    ).toISOString(),
    slackApprovals: l.approvals,
  };
};
const relayReply = () =>
  ok({ relay: relayOf() } satisfies FoundryRelayResponse);

const linkOf = (l: Link): FoundryRelayLink => {
  const user = db.users.get(l.createdByUserId);
  return {
    id: l.id,
    name: l.name,
    createdByUserId: l.createdByUserId,
    createdByName: user ? displayName(user) : "",
    createdAt: l.createdAt,
    ...(l.lastSeenAt && { lastSeenAt: l.lastSeenAt }),
    installs: l.teams,
    deliveriesToday: String(l.teams.length * 12),
    callsToday: String(l.teams.length * 30),
    refusedToday: "0",
    feedRowsToday: "0",
  };
};

const W = "/v1/workspaces/{workspaceId}/foundry";

export const relayRoutes: MockRoute[] = [
  // Hosted: Helix's side.
  {
    method: "POST",
    path: `${W}/relay-links:authorize`,
    handle: ({ params, body, principal: { user } }) => {
      const m = relayMember(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      const req = (body || {}) as Partial<AuthorizeFoundryRelayLinkRequest>;
      if (typeof req.state !== "string" || !STATE.test(req.state))
        return fail(
          "INVALID_ARGUMENT",
          "state is 16 to 256 URL-safe characters",
        );
      if (!manages(m)) return denied();
      if (knobs.slackInstallAllowed === false)
        return foundryPrecondition(
          m.workspaceId,
          "FOUNDRY_SLACK_NOT_OFFERED",
          "Helix Foundry for Slack isn't offered to this workspace yet",
        );
      if (
        typeof req.codeChallenge !== "string" ||
        !/^[A-Za-z0-9_-]{43}$/.test(req.codeChallenge)
      )
        return fail(
          "INVALID_ARGUMENT",
          "a relay code's challenge is an S256 PKCE challenge",
        );
      if (!redirectOrigin(req.redirectUri))
        return fail(
          "INVALID_ARGUMENT",
          "a relay redirect is http://localhost:<port>/settings/slack or http://127.0.0.1:<port>/settings/slack",
        );
      if (!nameOk(req.name))
        return fail(
          "INVALID_ARGUMENT",
          "a relay link's name is 1 to 128 bytes of text",
        );
      const code = randomBytes(32).toString("base64url");
      state.codes.set(code, {
        workspaceId: m.workspaceId,
        userId: user.id,
        challenge: req.codeChallenge,
        redirectUri: req.redirectUri!,
        name: req.name,
        expires: Date.now() + CODE_TTL_MS,
        used: false,
      });
      return ok({
        redirectUrl: `${req.redirectUri}?${new URLSearchParams({ code, state: req.state })}`,
      } satisfies AuthorizeFoundryRelayLinkResponse);
    },
  },
  {
    method: "GET",
    path: `${W}/relay-links`,
    handle: ({ params, principal: { user } }) => {
      const m = relayMember(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      if (!manages(m)) return denied();
      return ok({
        links: [...state.links.values()]
          .filter((l) => l.workspaceId === m.workspaceId)
          .reverse()
          .map(linkOf),
      } satisfies ListFoundryRelayLinksResponse);
    },
  },
  {
    method: "POST",
    path: `${W}/relay-links/{linkId}:revoke`,
    handle: ({ params, principal: { user } }) => {
      const m = relayMember(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      if (!manages(m)) return denied();
      const link = state.links.get(params.linkId!);
      if (!link || link.workspaceId !== m.workspaceId)
        return fail("NOT_FOUND", "no such linked install");
      state.links.delete(link.id);
      return ok({});
    },
  },

  // Local: the install's side.
  {
    method: "GET",
    path: `${W}/relay`,
    handle: ({ params, principal: { user } }) =>
      localOwner(params.workspaceId!, user.id) || relayReply(),
  },
  {
    method: "POST",
    path: `${W}/relay:start-link`,
    handle: ({ params, principal: { user } }) => {
      const refused = localOwner(params.workspaceId!, user.id);
      if (refused) return refused;
      const s = randomBytes(32).toString("base64url"),
        verifier = randomBytes(32).toString("base64url");
      state.pending.set(s, {
        expires: Date.now() + CODE_TTL_MS,
        used: false,
        code: null,
      });
      const url = new URL("/mock/relay/link", process.env.MOCK_ORIGIN);
      url.searchParams.set("state", s);
      url.searchParams.set("code_challenge", sha256url(verifier));
      url.searchParams.set(
        "redirect_uri",
        `${process.env.FOUNDRY_ORIGIN || "http://localhost:3000"}/settings/slack`,
      );
      url.searchParams.set("name", RELAY_NAME);
      return ok({
        authorizeUrl: url.toString(),
      } satisfies StartFoundryRelayLinkResponse);
    },
  },
  {
    method: "POST",
    path: `${W}/relay:complete-link`,
    handle: ({ params, body, principal: { user } }) => {
      const refused = localOwner(params.workspaceId!, user.id);
      if (refused) return refused;
      const req = (body || {}) as Partial<CompleteFoundryRelayLinkRequest>;
      if (!req.code || typeof req.state !== "string" || !STATE.test(req.state))
        return fail(
          "INVALID_ARGUMENT",
          "code and state are what Helix handed back",
        );
      if (knobs.relayLinkOutcome === "unavailable")
        return fail(
          "UNAVAILABLE",
          "finish linking to Helix: Helix can't be reached",
        );
      const pending = state.pending.get(req.state);
      if (
        knobs.relayLinkOutcome === "expired" ||
        !pending ||
        pending.used ||
        pending.expires < Date.now() ||
        pending.code !== req.code
      )
        return foundryPrecondition(
          params.workspaceId!,
          "FOUNDRY_RELAY_LINK_EXPIRED",
          "this link expired or was finished already: link to Helix again",
        );
      pending.used = true;
      // It replaces any link the install had, and its Slack installs.
      if (state.local) relayUnlinked();
      state.local = {
        accountEmail: user.email,
        linkedAt: now(),
        approvals: true,
      };
      return relayReply();
    },
  },
  {
    method: "PATCH",
    path: `${W}/relay`,
    handle: ({ params, body, principal: { user } }) => {
      const refused = localOwner(params.workspaceId!, user.id);
      if (refused) return refused;
      const l = localLink();
      if (!l) return fail("NOT_FOUND", "this Foundry isn't linked to Helix");
      l.approvals = !!(body as Partial<UpdateFoundryRelayRequest> | undefined)
        ?.slackApprovals;
      return relayReply();
    },
  },
  {
    method: "POST",
    path: `${W}/relay:unlink`,
    handle: ({ params, principal: { user } }) => {
      const refused = localOwner(params.workspaceId!, user.id);
      if (refused) return refused;
      if (localLink()) relayUnlinked();
      state.local = null;
      delete knobs.relayLinked;
      return relayReply();
    },
  },
];

/**
 * The install's redeem through Foundry's door (POST /__mock/relay/redeem):
 * the code Helix handed back and its PKCE verifier make a link, once, within
 * ten minutes, serving a Slack team per name in `teams`.
 */
export function redeemRelayCode(body: {
  code?: string;
  codeVerifier?: string;
  teams?: string[];
}): MockReply {
  const c = state.codes.get(body.code || "");
  if (
    !c ||
    c.used ||
    c.expires < Date.now() ||
    !body.codeVerifier ||
    sha256url(body.codeVerifier) !== c.challenge
  )
    return fail(
      "NOT_FOUND",
      "this link expired or was used already: link the install to Helix again",
    );
  c.used = true;
  const at = now(),
    link: Link = {
      id: id("rl"),
      workspaceId: c.workspaceId,
      name: c.name,
      createdByUserId: c.userId,
      createdAt: at,
      redirectOrigin: redirectOrigin(c.redirectUri)!,
      lastSeenAt: at,
      teams: (body.teams || []).map((teamName, i) => ({
        teamId: `T0RELAY${i}`,
        teamName,
        createdAt: at,
      })),
    };
  state.links.set(link.id, link);
  return ok({ linkId: link.id });
}

/**
 * A linked install adding the app through Foundry's door (POST
 * /__mock/relay/slack-install { linkId }): Slack's page, as its link's
 * creator, which the site's /slack/installed finishes, adding the Slack
 * team to the link and going back to the install's Settings › Slack.
 */
export function relaySlackInstall(body: { linkId?: string }): MockReply {
  const link = state.links.get(body.linkId || "");
  if (!link) return fail("UNAUTHENTICATED", "this relay token isn't live");
  return ok({
    authorizeUrl: startRelayedSlackInstall(
      link.workspaceId,
      link.createdByUserId,
      link.redirectOrigin + "/settings/slack",
      (teamId, teamName) => {
        if (!link.teams.some((t) => t.teamId === teamId))
          link.teams.push({ teamId, teamName, createdAt: now() });
      },
    ),
  });
}

/**
 * Helix's /relay/link as a local install meets it (/mock/relay/link): Link
 * sends the browser back to the install's Settings › Slack with a code for
 * its state, Cancel with error=access_denied. It goes back only to a
 * loopback Settings › Slack, as Helix's page does.
 */
export function handleRelayPage(
  req: IncomingMessage,
  res: ServerResponse,
  url: URL,
  raw: string,
): boolean {
  if (url.pathname !== "/mock/relay/link") return false;
  if (req.method === "GET") {
    const s = url.searchParams.get("state") || "",
      back = url.searchParams.get("redirect_uri") || "",
      name = url.searchParams.get("name") || "";
    const waiting = state.pending.get(s);
    send(
      res,
      waiting && redirectOrigin(back) ? 200 : 404,
      waiting && redirectOrigin(back)
        ? page(
            "Link to Helix",
            `<h1>Helix</h1><p>Mock Helix. Link the Foundry on ${escape(name)} to Acme. Slack messages for that install will pass through Helix, and Helix vouches for who clicked in Slack.</p>
<form method="post" action="/mock/relay/link"><input type="hidden" name="state" value="${escape(s)}"><input type="hidden" name="redirect_uri" value="${escape(back)}"><button type="submit" name="choice" value="link">Link</button><button type="submit" class="secondary" name="choice" value="cancel">Cancel</button></form>`,
          )
        : page("Mock Helix", "<h1>This request has expired</h1>"),
    );
    return true;
  }
  const form = new URLSearchParams(raw),
    s = form.get("state") || "",
    redirect = form.get("redirect_uri") || "",
    waiting = state.pending.get(s);
  if (!waiting || !redirectOrigin(redirect)) {
    send(res, 404, page("Mock Helix", "<h1>This request has expired</h1>"));
    return true;
  }
  const back = new URL(redirect);
  if (form.get("choice") === "link") {
    waiting.code = randomBytes(16).toString("hex");
    back.searchParams.set("code", waiting.code);
  } else back.searchParams.set("error", "access_denied");
  back.searchParams.set("state", s);
  res.writeHead(303, { Location: back.toString() });
  res.end();
  return true;
}
