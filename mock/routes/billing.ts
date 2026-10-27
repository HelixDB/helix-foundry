import type { IncomingMessage, ServerResponse } from "node:http";
import type {
  CreateFoundryProCheckoutRequest,
  CreateFoundryProCheckoutResponse,
  FoundryPaidTier,
  FoundryPlan,
  FoundryPlanChangeRequest,
  FoundryPlanResponse,
  OpenBillingPortalRequest,
  OpenBillingPortalResponse,
} from "../../src/shared/wfe";
import { fail, id, ok, type MockReply, type MockRoute } from "../http";
import {
  LOCAL_TOKEN,
  config,
  db,
  isReply,
  membership,
  onReset,
} from "../state";
import { manages } from "./workspaces";

// Foundry Free, Pro and Max as the WFE serves them: the plan is read live, checkout returns a
// Stripe Checkout page (here the mock's own /mock/checkout) or charges a card
// on file, and cancel and resume act at the period end. Pro and Max are one
// Autumn group: checkout of the tier held attaches nothing, and checkout of
// the other one switches at once, charging the card on file. The billing
// portal is /mock/billing-portal.
//
// This is the mock's one plan: the Data routes' Pro gate reads isFoundryPro,
// which Max passes. Acme starts on Pro (its seeded PostgreSQL database needs
// it); a new workspace starts on Free. A local API passes every workspace.
// `foundryPeriodEnded: true` (POST /__mock/config) brings a cancelled plan's
// period end: the workspace reads Free from then on.

type Plan = {
  tier: FoundryPaidTier;
  periodEnd?: string;
  cancelAtPeriodEnd: boolean;
  pastDue: boolean;
  // What the next read answers instead, as Autumn does for a moment after a
  // card on file was charged: Free after a purchase, the old plan after a
  // switch.
  staleRead?: FoundryPlan;
};
const plans = new Map<string, Plan>();
const knobs = config as typeof config & { foundryPeriodEnded?: boolean };

/** The workspace's plan, unless it was cancelled and its period has ended. */
function held(workspaceId: string): Plan | undefined {
  if (plans.get(workspaceId)?.cancelAtPeriodEnd && knobs.foundryPeriodEnded)
    plans.delete(workspaceId);
  return plans.get(workspaceId);
}
// Checkout sessions waiting on the mock's checkout page, by session ID.
const sessions = new Map<
  string,
  { workspaceId: string; tier: FoundryPaidTier }
>();
// Client tokens already used per workspace: Autumn refuses a repeat (409).
const tokens = new Set<string>();
export const PROMO_CODE = "FOUNDRYTEST50";
const NAME: Record<FoundryPaidTier, string> = {
  FOUNDRY_TIER_PRO: "Foundry Pro",
  FOUNDRY_TIER_MAX: "Foundry Max",
};
const PAID = Object.keys(NAME) as FoundryPaidTier[];

onReset(() => {
  delete knobs.foundryPeriodEnded;
  plans.clear();
  sessions.clear();
  tokens.clear();
  setFoundryPlan("ws_acme", "FOUNDRY_TIER_PRO");
});

function view(p: Plan | undefined, read = false): FoundryPlan {
  if (!p) return { tier: "FOUNDRY_TIER_FREE" };
  if (read && p.staleRead) {
    const stale = p.staleRead;
    p.staleRead = undefined;
    return stale;
  }
  return {
    tier: p.tier,
    pastDue: p.pastDue,
    currentPeriodEnd: p.periodEnd,
    cancelAtPeriodEnd: p.cancelAtPeriodEnd,
  };
}

/**
 * True when the workspace holds Foundry Pro or Max (Max grants what Pro
 * grants), as the WFE's Pro gate reads it.
 */
export const isFoundryPro = (workspaceId: string) =>
  LOCAL_TOKEN !== null || held(workspaceId) !== undefined;

/** FailedPrecondition with the ErrorInfo the WFE's billingPrecondition adds. */
export function proRequired(workspaceId: string, message: string): MockReply {
  const reply = fail("FAILED_PRECONDITION", message);
  return {
    ...reply,
    body: {
      ...(reply.body as object),
      details: [
        {
          "@type": "type.googleapis.com/google.rpc.ErrorInfo",
          reason: "FOUNDRY_PRO_REQUIRED",
          domain: "billing.helix-db.com",
          metadata: { workspace_id: workspaceId },
        },
      ],
    },
  };
}

/**
 * Puts a workspace on Pro or Max, renewing in 30 days, or back on Free (as
 * when a cancelled plan reaches its period end).
 */
export function setFoundryPlan(
  workspaceId: string,
  tier: FoundryPaidTier | "FOUNDRY_TIER_FREE",
) {
  if (tier === "FOUNDRY_TIER_FREE") {
    plans.delete(workspaceId);
    return;
  }
  plans.set(workspaceId, {
    tier,
    periodEnd: new Date(Date.now() + 30 * 86_400_000).toISOString(),
    cancelAtPeriodEnd: false,
    pastDue: false,
  });
}

/** The caller's membership, refused unless they hold workspace.billing.manage. */
function billingManager(
  workspaceId: string | undefined,
  userId: string,
): string | MockReply {
  if (!workspaceId) return fail("INVALID_ARGUMENT", "workspace_id is required");
  const m = membership(workspaceId, userId);
  if (isReply(m)) return m;
  return manages(m)
    ? m.workspaceId
    : fail("PERMISSION_DENIED", "permission denied");
}

// Where Autumn sends the browser after checkout: the stamp's
// billing.autumn.foundry.return_url, https://<foundry-host>/settings/billing.
const returnUrl = () =>
  new URL(
    "/settings/billing",
    process.env.NEXT_PUBLIC_WORKOS_REDIRECT_URI || "http://localhost:3000",
  ).toString();

export const billingRoutes: MockRoute[] = [
  {
    method: "GET",
    path: "/v1/foundry/plan",
    handle: ({ query, principal: { user } }) => {
      const workspaceId = query.get("workspaceId");
      if (!workspaceId)
        return fail("INVALID_ARGUMENT", "workspace_id is required");
      const m = membership(workspaceId, user.id);
      if (isReply(m)) return m;
      return ok({
        plan: view(held(m.workspaceId), true),
      } satisfies FoundryPlanResponse);
    },
  },
  {
    method: "POST",
    path: "/v1/foundry/plan:checkout",
    handle: ({ body, principal: { user } }) => {
      const req = (body || {}) as Partial<CreateFoundryProCheckoutRequest>;
      const workspaceId = billingManager(req.workspaceId, user.id);
      if (typeof workspaceId !== "string") return workspaceId;
      if (!req.clientToken)
        return fail("INVALID_ARGUMENT", "client_token is required");
      // Unset is Pro (proto3 reads UNSPECIFIED, the zero value, as unset).
      const asked: string = req.tier || "FOUNDRY_TIER_UNSPECIFIED";
      const tier =
        asked === "FOUNDRY_TIER_UNSPECIFIED"
          ? "FOUNDRY_TIER_PRO"
          : PAID.find((t) => t === asked);
      if (!tier)
        return fail(
          "INVALID_ARGUMENT",
          "tier must be FOUNDRY_TIER_PRO or FOUNDRY_TIER_MAX",
        );
      const promo = (req.promoCode || "").trim();
      if (promo && promo !== PROMO_CODE)
        return fail(
          "INVALID_ARGUMENT",
          `the promotion code is invalid or does not apply to ${NAME[tier]}`,
        );
      const current = held(workspaceId);
      if (current?.tier === tier) return ok({ plan: view(current) });
      const key = `${workspaceId}:${req.clientToken}`;
      if (tokens.has(key))
        return fail(
          "UNAVAILABLE",
          "we could not confirm whether your payment went through; check the plan before trying again",
        );
      tokens.add(key);
      if (current || config.checkoutCardOnFile) {
        // Charged without a page (a switch always has a card on file); the
        // plan reads as it was for a moment.
        const before = view(current);
        setFoundryPlan(workspaceId, tier);
        plans.get(workspaceId)!.staleRead = before;
        return ok({
          plan: before,
        } satisfies CreateFoundryProCheckoutResponse);
      }
      const session = id("cs");
      sessions.set(session, { workspaceId, tier });
      const url = new URL("/mock/checkout", process.env.MOCK_ORIGIN);
      url.searchParams.set("session", session);
      return ok({
        checkoutUrl: url.toString(),
        plan: { tier: "FOUNDRY_TIER_FREE" },
      } satisfies CreateFoundryProCheckoutResponse);
    },
  },
  {
    method: "POST",
    path: "/v1/foundry/plan:cancel",
    handle: ({ body, principal: { user } }) => {
      const req = (body || {}) as Partial<FoundryPlanChangeRequest>;
      const workspaceId = billingManager(req.workspaceId, user.id);
      if (typeof workspaceId !== "string") return workspaceId;
      const p = held(workspaceId);
      if (!p)
        return fail(
          "FAILED_PRECONDITION",
          "the workspace is not on Foundry Pro or Max",
        );
      p.cancelAtPeriodEnd = true;
      return ok({ plan: view(p) } satisfies FoundryPlanResponse);
    },
  },
  {
    method: "POST",
    path: "/v1/foundry/plan:resume",
    handle: ({ body, principal: { user } }) => {
      const req = (body || {}) as Partial<FoundryPlanChangeRequest>;
      const workspaceId = billingManager(req.workspaceId, user.id);
      if (typeof workspaceId !== "string") return workspaceId;
      const p = held(workspaceId);
      if (!p)
        return fail(
          "FAILED_PRECONDITION",
          "the workspace is not on Foundry Pro or Max",
        );
      p.cancelAtPeriodEnd = false;
      return ok({ plan: view(p) } satisfies FoundryPlanResponse);
    },
  },
  {
    method: "POST",
    path: "/v1/billing/portal",
    handle: ({ body, principal: { user } }) => {
      const req = (body || {}) as Partial<OpenBillingPortalRequest>;
      const workspaceId = billingManager(req.workspaceId, user.id);
      if (typeof workspaceId !== "string") return workspaceId;
      const url = new URL("/mock/billing-portal", process.env.MOCK_ORIGIN);
      url.searchParams.set("workspace", workspaceId);
      return ok({
        portalUrl: url.toString(),
      } satisfies OpenBillingPortalResponse);
    },
  },
];

const escape = (s: string) =>
  s.replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ]!,
  );

const page = (title: string, body: string) =>
  `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>${escape(title)}</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#f8f7f3;color:#292a26;font:14px Inter,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
main{display:grid;gap:12px;width:360px}h1{margin:0 0 4px;font:400 26px Georgia,serif}p{margin:0 0 8px;color:#65675e}
button,a{display:grid;place-items:center;height:40px;border:0;border-radius:8px;background:#343932;color:#fbfaf6;font:500 14px inherit;text-decoration:none;cursor:pointer}
a{background:#eceae2;color:#292a26}</style></head><body><main>${body}</main></body></html>`;

function send(res: ServerResponse, status: number, html: string) {
  res.writeHead(status, {
    "Content-Type": "text/html; charset=utf-8",
    "Cache-Control": "no-store",
  });
  res.end(html);
}

/**
 * The hosted pages billing links open: Stripe Checkout (/mock/checkout) and
 * the billing portal (/mock/billing-portal). False if it is neither.
 */
export function handleBillingPage(
  req: IncomingMessage,
  res: ServerResponse,
  url: URL,
  raw: string,
): boolean {
  if (url.pathname === "/mock/billing-portal" && req.method === "GET") {
    const ws = db.workspaces.get(url.searchParams.get("workspace") || "");
    send(
      res,
      ws ? 200 : 404,
      page(
        "Mock billing portal",
        `<h1>Mock billing portal</h1><p>Invoices and payment methods for ${escape(ws?.displayName || "an unknown workspace")}.</p><a href="${escape(returnUrl())}">Return to Helix Foundry</a>`,
      ),
    );
    return true;
  }
  if (url.pathname !== "/mock/checkout") return false;
  if (req.method === "GET") {
    const session = url.searchParams.get("session") || "";
    const waiting = sessions.get(session);
    const ws = db.workspaces.get(waiting?.workspaceId || "");
    send(
      res,
      ws && waiting ? 200 : 404,
      ws && waiting
        ? page(
            "Mock checkout",
            `<h1>${NAME[waiting.tier]}</h1><p>Mock Stripe Checkout for ${escape(ws.displayName || ws.slug || ws.id)}. Nothing is charged.</p>
<form method="post" action="/mock/checkout"><input type="hidden" name="session" value="${escape(session)}"><button type="submit">Pay and subscribe</button></form>
<a href="${escape(returnUrl())}">Back to Helix Foundry</a>`,
          )
        : page("Mock checkout", "<h1>This checkout has expired</h1>"),
    );
    return true;
  }
  const session = new URLSearchParams(raw).get("session") || "";
  const waiting = sessions.get(session);
  if (!waiting) {
    send(res, 404, page("Mock checkout", "<h1>This checkout has expired</h1>"));
    return true;
  }
  sessions.delete(session);
  setFoundryPlan(waiting.workspaceId, waiting.tier);
  res.writeHead(303, { Location: returnUrl() });
  res.end();
  return true;
}
