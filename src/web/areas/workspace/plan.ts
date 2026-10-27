import {
  FOUNDRY_PRO_REQUIRED,
  type CreateFoundryProCheckoutRequest,
  type CreateFoundryProCheckoutResponse,
  type FoundryPaidTier,
  type FoundryPlan,
  type FoundryPlanResponse,
  type OpenBillingPortalResponse,
} from "../../../shared/wfe";
import { ApiError, api } from "../../api";
import type { WorkspaceRole } from "../../ui";

// Foundry Free, Pro and Max.
// Autumn is the system of record and the WFE reads it live on every call, so
// the plan is cached for a minute here. Every stamp's plans offer every
// connector kind; a kind a plan leaves out is refused with
// FAILED_PRECONDITION and reason FOUNDRY_PRO_REQUIRED, at create, test and
// sync time. Max grants everything Pro grants, so every Pro gate passes it.

/** Foundry's paid plans, in price order. */
export const PAID_TIERS = ["pro", "max"] as const;
export type PaidTier = (typeof PAID_TIERS)[number];
export type Tier = "free" | PaidTier;

/** What each plan is called after "Foundry". */
export const TIER_NAME: Record<Tier, string> = {
  free: "Free",
  pro: "Pro",
  max: "Max",
};

/**
 * The monthly prices. The WFE does not return them; these are the Autumn
 * plans foundry-pro's and foundry-max's.
 */
export const PLAN_PRICE: Record<Tier, string> = {
  free: "$0",
  pro: "$100",
  max: "$500",
};

const WFE_TIER: Record<PaidTier, FoundryPaidTier> = {
  pro: "FOUNDRY_TIER_PRO",
  max: "FOUNDRY_TIER_MAX",
};

/** What a workspace's plan means for the page, one case per state. */
export type PlanSummary =
  | { tier: "free" }
  | {
      tier: PaidTier;
      // renewing: paid and renews at periodEnd. ending: cancelled, paid until
      // periodEnd. pastDue: an invoice is unpaid.
      state: "renewing" | "ending" | "pastDue";
      periodEnd: Date | null;
    };

/**
 * Reads the WFE's plan. Anything but PRO or MAX is Free, as the backend
 * defines it.
 *
 * @example
 * planSummary({ tier: "FOUNDRY_TIER_MAX", cancelAtPeriodEnd: true });
 * // { tier: "max", state: "ending", periodEnd: null }
 */
export function planSummary(plan: FoundryPlan | undefined): PlanSummary {
  const tier = PAID_TIERS.find((t) => WFE_TIER[t] === plan?.tier);
  if (!tier || !plan) return { tier: "free" };
  const end = plan.currentPeriodEnd ? new Date(plan.currentPeriodEnd) : null;
  return {
    tier,
    state: plan.pastDue
      ? "pastDue"
      : plan.cancelAtPeriodEnd
        ? "ending"
        : "renewing",
    periodEnd: end && !Number.isNaN(end.getTime()) ? end : null,
  };
}

/** Owners and admins hold workspace.billing.manage and workspace.members.manage. */
export const canManage = (role: WorkspaceRole) =>
  role === "owner" || role === "admin";

export const planKey = (workspaceId: string) => [workspaceId, "foundry-plan"];
export const planQuery = (workspaceId: string) => ({
  queryKey: planKey(workspaceId),
  queryFn: async () =>
    (
      await api<FoundryPlanResponse>(
        "/v1/foundry/plan?workspaceId=" + encodeURIComponent(workspaceId),
      )
    ).plan || {},
  staleTime: 60_000,
});

/**
 * True when the WFE refused an action because the workspace is on Free
 * (google.rpc.ErrorInfo reason FOUNDRY_PRO_REQUIRED). Show <ProGate/> then.
 */
export function isProRequired(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    (error.details || []).some(
      (d) =>
        !!d &&
        typeof d === "object" &&
        (d as { reason?: unknown }).reason === FOUNDRY_PRO_REQUIRED,
    )
  );
}

/**
 * What a failed checkout means. "refused": the BFF or the WFE said no (a 4xx:
 * a bad promotion code, a declined card, no permission, CSRF) and nothing was
 * charged, so the next click is a new purchase. "unsure": no answer, or a 5xx
 * such as UNAVAILABLE for a token Autumn already saw; the charge may have gone
 * through, so read the plan before offering checkout again. Autumn refuses a
 * second attach of a held plan, so a new purchase never charges twice.
 *
 * @example
 * checkoutFailure(new ApiError("bad code", 400, 3)); // "refused"
 * checkoutFailure(new TypeError("fetch failed")); // "unsure"
 */
export function checkoutFailure(error: unknown): "refused" | "unsure" {
  return error instanceof ApiError && error.status >= 400 && error.status < 500
    ? "refused"
    : "unsure";
}

/**
 * A billing page the WFE sent the browser to (Stripe Checkout, the billing
 * portal), refused unless it is https, or http on a loopback host (the mock).
 * The browser navigates there, so nothing else (javascript:, data:) passes.
 *
 * @example
 * billingPage("https://checkout.stripe.com/c/pay/cs_1"); // the same URL
 * billingPage("javascript:alert(1)"); // throws
 */
export function billingPage(raw: string): string {
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    throw new Error("The billing page’s address is not valid.");
  }
  const loopback = ["localhost", "127.0.0.1", "[::1]"].includes(url.hostname);
  if (url.protocol === "https:" || (url.protocol === "http:" && loopback))
    return url.toString();
  throw new Error("The billing page’s address is not valid.");
}

export type CheckoutResult =
  // Finish paying on Stripe Checkout, which returns to /settings/billing.
  | { kind: "redirect"; url: string }
  // A card on file was charged; the plan may read Free for a moment.
  | { kind: "charged"; plan: FoundryPlan };

/**
 * Buys `tier`, or switches to it from the other paid plan (one Autumn group,
 * so the new plan replaces the old). Asking for the plan already held
 * attaches nothing and answers it as "charged".
 */
export async function startCheckout({
  tier,
  ...req
}: Omit<CreateFoundryProCheckoutRequest, "tier"> & {
  tier: PaidTier;
}): Promise<CheckoutResult> {
  const promoCode = req.promoCode?.trim();
  const res = await api<CreateFoundryProCheckoutResponse>(
    "/v1/foundry/plan:checkout",
    { ...req, promoCode: promoCode || undefined, tier: WFE_TIER[tier] },
  );
  return res.checkoutUrl
    ? { kind: "redirect", url: billingPage(res.checkoutUrl) }
    : { kind: "charged", plan: res.plan || {} };
}

/** Cancels or resumes whichever paid plan the workspace holds. */
export const cancelPlan = async (workspaceId: string) =>
  (await api<FoundryPlanResponse>("/v1/foundry/plan:cancel", { workspaceId }))
    .plan || {};
export const resumePlan = async (workspaceId: string) =>
  (await api<FoundryPlanResponse>("/v1/foundry/plan:resume", { workspaceId }))
    .plan || {};

/** The hosted billing portal's URL (invoices, payment methods). */
export async function billingPortalUrl(workspaceId: string) {
  const { portalUrl } = await api<OpenBillingPortalResponse>(
    "/v1/billing/portal",
    { workspaceId },
  );
  if (!portalUrl) throw new Error("Couldn’t open the billing portal.");
  return billingPage(portalUrl);
}

// A checkout that left for Stripe (or charged a card) is remembered for this
// tab, with the plan it bought, so /settings/billing keeps reading the plan
// until that plan shows up.
const CHECKOUT_KEY = "foundry.checkout";
// How long after a checkout the billing page keeps checking.
export const CONFIRM_WINDOW_MS = 10 * 60_000;

export function rememberCheckout(
  workspaceId: string,
  tier: PaidTier,
  now = Date.now(),
) {
  try {
    sessionStorage.setItem(
      CHECKOUT_KEY,
      JSON.stringify({ workspaceId, tier, now }),
    );
  } catch {
    // Storage blocked: the page just shows the plan as the WFE reads it.
  }
}
/**
 * The plan this tab recently started a checkout of for `workspaceId`, or null.
 *
 * @example
 * rememberCheckout("ws_1", "max", 0);
 * pendingCheckout("ws_1", 1000); // "max"
 * pendingCheckout("ws_2", 1000); // null
 */
export function pendingCheckout(
  workspaceId: string,
  now = Date.now(),
): PaidTier | null {
  try {
    const raw = JSON.parse(sessionStorage.getItem(CHECKOUT_KEY) || "null") as {
      workspaceId?: unknown;
      tier?: unknown;
      now?: unknown;
    } | null;
    const tier = PAID_TIERS.find((t) => t === raw?.tier);
    return tier &&
      raw?.workspaceId === workspaceId &&
      typeof raw.now === "number" &&
      now - raw.now < CONFIRM_WINDOW_MS
      ? tier
      : null;
  } catch {
    return null;
  }
}
export function forgetCheckout() {
  try {
    sessionStorage.removeItem(CHECKOUT_KEY);
  } catch {
    // Nothing to forget.
  }
}
