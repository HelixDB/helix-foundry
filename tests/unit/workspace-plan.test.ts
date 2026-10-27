import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, clearCsrfToken } from "@/web/api";
import {
  CONFIRM_WINDOW_MS,
  PAID_TIERS,
  PLAN_PRICE,
  TIER_NAME,
  billingPage,
  billingPortalUrl,
  canManage,
  cancelPlan,
  checkoutFailure,
  forgetCheckout,
  isProRequired,
  pendingCheckout,
  planQuery,
  planSummary,
  rememberCheckout,
  resumePlan,
  startCheckout,
} from "@/web/areas/workspace/plan";
import type { FoundryTier } from "@/shared/wfe";

// The workspace area's plan logic: reading the WFE's FoundryPlan (Free, Pro
// or Max), the Pro gate's error check, one purchase per client token, and the
// checkout calls.

describe("plans", () => {
  it("sells Pro at $100 and Max at $500 a month, in that order", () => {
    expect(PAID_TIERS).toEqual(["pro", "max"]);
    expect(PLAN_PRICE).toEqual({ free: "$0", pro: "$100", max: "$500" });
    expect(TIER_NAME).toEqual({ free: "Free", pro: "Pro", max: "Max" });
  });
});

describe("planSummary", () => {
  it("is Free unless the WFE says PRO or MAX", () => {
    expect(planSummary(undefined)).toEqual({ tier: "free" });
    expect(planSummary({})).toEqual({ tier: "free" });
    expect(planSummary({ tier: "FOUNDRY_TIER_FREE" })).toEqual({
      tier: "free",
    });
    expect(planSummary({ tier: "FOUNDRY_TIER_UNSPECIFIED" })).toEqual({
      tier: "free",
    });
    // Paid-only fields on a Free plan mean nothing.
    expect(planSummary({ tier: "FOUNDRY_TIER_FREE", pastDue: true })).toEqual({
      tier: "free",
    });
    // A tier this page doesn't know yet, or a prototype key, is not paid.
    for (const tier of ["FOUNDRY_TIER_BUSINESS", "constructor", "pro"])
      expect(planSummary({ tier: tier as FoundryTier })).toEqual({
        tier: "free",
      });
  });

  it("reads Max as Pro is read", () => {
    const end = "2026-11-17T09:00:00Z";
    expect(
      planSummary({ tier: "FOUNDRY_TIER_MAX", currentPeriodEnd: end }),
    ).toEqual({ tier: "max", state: "renewing", periodEnd: new Date(end) });
    expect(
      planSummary({ tier: "FOUNDRY_TIER_MAX", cancelAtPeriodEnd: true }),
    ).toEqual({ tier: "max", state: "ending", periodEnd: null });
    expect(
      planSummary({
        tier: "FOUNDRY_TIER_MAX",
        pastDue: true,
        cancelAtPeriodEnd: true,
      }),
    ).toEqual({ tier: "max", state: "pastDue", periodEnd: null });
  });

  it("reads Pro's state, past due first", () => {
    const end = "2026-11-17T09:00:00Z";
    expect(
      planSummary({ tier: "FOUNDRY_TIER_PRO", currentPeriodEnd: end }),
    ).toEqual({ tier: "pro", state: "renewing", periodEnd: new Date(end) });
    expect(
      planSummary({
        tier: "FOUNDRY_TIER_PRO",
        cancelAtPeriodEnd: true,
        currentPeriodEnd: end,
      }),
    ).toEqual({ tier: "pro", state: "ending", periodEnd: new Date(end) });
    expect(
      planSummary({
        tier: "FOUNDRY_TIER_PRO",
        pastDue: true,
        cancelAtPeriodEnd: true,
      }),
    ).toEqual({ tier: "pro", state: "pastDue", periodEnd: null });
  });

  it("drops a period end it cannot read", () => {
    expect(
      planSummary({ tier: "FOUNDRY_TIER_PRO", currentPeriodEnd: "soon" }),
    ).toEqual({ tier: "pro", state: "renewing", periodEnd: null });
  });
});

describe("canManage", () => {
  it("is owners and admins", () => {
    expect(canManage("owner")).toBe(true);
    expect(canManage("admin")).toBe(true);
    expect(canManage("member")).toBe(false);
  });
});

describe("isProRequired", () => {
  const info = (reason: string) => ({
    "@type": "type.googleapis.com/google.rpc.ErrorInfo",
    reason,
    domain: "billing.helix-db.com",
  });

  it("finds FOUNDRY_PRO_REQUIRED in the WFE's details", () => {
    expect(
      isProRequired(
        new ApiError("PostgreSQL connectors need Foundry Pro", 400, 9, [
          info("FOUNDRY_PRO_REQUIRED"),
        ]),
      ),
    ).toBe(true);
  });

  it("is false for anything else", () => {
    expect(isProRequired(new ApiError("no", 400, 9))).toBe(false);
    expect(
      isProRequired(new ApiError("no", 400, 9, [info("WORKSPACE_SUSPENDED")])),
    ).toBe(false);
    expect(isProRequired(new ApiError("no", 400, 9, [null, "x", 3]))).toBe(
      false,
    );
    expect(isProRequired(new Error("FOUNDRY_PRO_REQUIRED"))).toBe(false);
    expect(isProRequired({ details: [info("FOUNDRY_PRO_REQUIRED")] })).toBe(
      false,
    );
  });
});

describe("checkoutFailure", () => {
  it("is refused when the BFF or the WFE said no", () => {
    // INVALID_ARGUMENT: a refused promotion code.
    expect(checkoutFailure(new ApiError("code", 400, 3))).toBe("refused");
    // FAILED_PRECONDITION: a declined card.
    expect(checkoutFailure(new ApiError("declined", 400, 9))).toBe("refused");
    expect(checkoutFailure(new ApiError("denied", 403, 7))).toBe("refused");
    expect(checkoutFailure(new ApiError("csrf", 403, "CSRF_REJECTED"))).toBe(
      "refused",
    );
  });

  it("is unsure when the charge may have gone through", () => {
    // UNAVAILABLE: Autumn saw the token already, or did not answer.
    expect(checkoutFailure(new ApiError("unsure", 503, 14))).toBe("unsure");
    expect(checkoutFailure(new ApiError("internal", 500, 13))).toBe("unsure");
    // A lost connection is not an ApiError at all.
    expect(checkoutFailure(new TypeError("fetch failed"))).toBe("unsure");
    expect(checkoutFailure(undefined)).toBe("unsure");
  });
});

describe("billingPage", () => {
  it("follows https, and http only on loopback", () => {
    expect(billingPage("https://checkout.stripe.com/c/pay/cs_1#x")).toBe(
      "https://checkout.stripe.com/c/pay/cs_1#x",
    );
    expect(billingPage("http://127.0.0.1:4010/mock/checkout?s=1")).toBe(
      "http://127.0.0.1:4010/mock/checkout?s=1",
    );
    expect(billingPage("http://localhost:3000/settings/billing")).toBe(
      "http://localhost:3000/settings/billing",
    );
  });

  it.each([
    "javascript:alert(1)",
    "data:text/html,hi",
    "http://checkout.stripe.com/c",
    "//evil.example/x",
    "/settings/billing",
    "",
  ])("refuses %j", (raw) => {
    expect(() => billingPage(raw)).toThrow("not valid");
  });
});

describe("checkout memory", () => {
  let store: Map<string, string>;
  beforeEach(() => {
    store = new Map();
    vi.stubGlobal("sessionStorage", {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => void store.set(k, v),
      removeItem: (k: string) => void store.delete(k),
    });
  });
  afterEach(() => vi.unstubAllGlobals());

  it("remembers a checkout's plan for its workspace, for a while", () => {
    rememberCheckout("ws_1", "pro", 1000);
    expect(pendingCheckout("ws_1", 1000)).toBe("pro");
    expect(pendingCheckout("ws_2", 1000)).toBeNull();
    expect(pendingCheckout("ws_1", 1000 + CONFIRM_WINDOW_MS - 1)).toBe("pro");
    expect(pendingCheckout("ws_1", 1000 + CONFIRM_WINDOW_MS)).toBeNull();
    // The latest checkout wins.
    rememberCheckout("ws_1", "max", 2000);
    expect(pendingCheckout("ws_1", 2000)).toBe("max");
    forgetCheckout();
    expect(pendingCheckout("ws_1", 2000)).toBeNull();
  });

  it("ignores what it did not write", () => {
    store.set("foundry.checkout", "{not json");
    expect(pendingCheckout("ws_1")).toBeNull();
    store.set("foundry.checkout", JSON.stringify({ workspaceId: "ws_1" }));
    expect(pendingCheckout("ws_1")).toBeNull();
    // Without a plan it bought, or with one this page doesn't sell.
    for (const tier of [undefined, "free", "FOUNDRY_TIER_MAX", "constructor"]) {
      store.set(
        "foundry.checkout",
        JSON.stringify({ workspaceId: "ws_1", tier, now: Date.now() }),
      );
      expect(pendingCheckout("ws_1"), String(tier)).toBeNull();
    }
  });

  it("works without storage", () => {
    vi.stubGlobal("sessionStorage", {
      getItem: () => {
        throw new Error("blocked");
      },
      setItem: () => {
        throw new Error("blocked");
      },
      removeItem: () => {
        throw new Error("blocked");
      },
    });
    expect(() => rememberCheckout("ws_1", "max")).not.toThrow();
    expect(pendingCheckout("ws_1")).toBeNull();
    expect(() => forgetCheckout()).not.toThrow();
  });
});

describe("plan calls", () => {
  type Call = { url: string; init: RequestInit };
  let calls: Call[], replies: Response[];
  const json = (status: number, body: unknown) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": "application/json" },
    });
  beforeEach(() => {
    calls = [];
    // Each test starts without a CSRF token, so its first unsafe call asks.
    clearCsrfToken();
    replies = [json(200, { token: "csrf", expiresInSeconds: 3600 })];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init: RequestInit = {}) => {
        calls.push({ url, init });
        return replies.shift() ?? json(500, { message: "no reply queued" });
      }),
    );
    vi.stubGlobal("window", {
      location: { pathname: "/", search: "", hash: "", assign: () => {} },
    });
  });
  afterEach(() => vi.unstubAllGlobals());
  const sent = (i: number) => JSON.parse(String(calls[i]!.init.body));
  const posts = () => calls.filter((c) => c.url !== "/papi/csrf");

  it("reads the plan for the workspace", async () => {
    replies = [json(200, { plan: { tier: "FOUNDRY_TIER_PRO" } })];
    const q = planQuery("ws 1");
    expect(q.queryKey).toEqual(["ws 1", "foundry-plan"]);
    expect(await q.queryFn()).toEqual({ tier: "FOUNDRY_TIER_PRO" });
    expect(calls[0]!.url).toBe("/papi/v1/foundry/plan?workspaceId=ws%201");
    replies = [json(200, {})];
    expect(await q.queryFn()).toEqual({});
  });

  it("redirects to Stripe Checkout, or reports a charged card", async () => {
    replies.push(
      json(200, {
        checkoutUrl: "https://checkout.stripe.test/c",
        plan: { tier: "FOUNDRY_TIER_FREE" },
      }),
      json(200, { plan: { tier: "FOUNDRY_TIER_FREE" } }),
      json(200, {}),
    );
    expect(
      await startCheckout({
        workspaceId: "ws_1",
        clientToken: "t1",
        promoCode: "  FOUNDRYTEST50 ",
        tier: "pro",
      }),
    ).toEqual({ kind: "redirect", url: "https://checkout.stripe.test/c" });
    expect(posts()[0]!.url).toBe("/papi/v1/foundry/plan:checkout");
    // The tier is always sent, Pro included.
    expect(sent(1)).toEqual({
      workspaceId: "ws_1",
      clientToken: "t1",
      promoCode: "FOUNDRYTEST50",
      tier: "FOUNDRY_TIER_PRO",
    });
    expect(
      await startCheckout({
        workspaceId: "ws_1",
        clientToken: "t2",
        promoCode: " ",
        tier: "max",
      }),
    ).toEqual({ kind: "charged", plan: { tier: "FOUNDRY_TIER_FREE" } });
    // A blank code is not sent.
    expect(sent(2)).toEqual({
      workspaceId: "ws_1",
      clientToken: "t2",
      tier: "FOUNDRY_TIER_MAX",
    });
    expect(
      await startCheckout({
        workspaceId: "ws_1",
        clientToken: "t3",
        tier: "max",
      }),
    ).toEqual({ kind: "charged", plan: {} });
  });

  it("switches by asking for the other tier, answered with the plan", async () => {
    replies.push(json(200, { plan: { tier: "FOUNDRY_TIER_PRO" } }));
    expect(
      await startCheckout({
        workspaceId: "ws_1",
        clientToken: "t",
        tier: "max",
      }),
    ).toEqual({ kind: "charged", plan: { tier: "FOUNDRY_TIER_PRO" } });
    expect(sent(1)).toEqual({
      workspaceId: "ws_1",
      clientToken: "t",
      tier: "FOUNDRY_TIER_MAX",
    });
  });

  it("refuses to send the browser to anything but a billing page", async () => {
    replies.push(json(200, { checkoutUrl: "javascript:alert(1)" }));
    await expect(
      startCheckout({ workspaceId: "ws_1", clientToken: "t", tier: "pro" }),
    ).rejects.toThrow("not valid");
  });

  it("cancels and resumes whichever plan is held, returning it", async () => {
    replies.push(
      json(200, {
        plan: { tier: "FOUNDRY_TIER_MAX", cancelAtPeriodEnd: true },
      }),
      json(200, { plan: { tier: "FOUNDRY_TIER_MAX" } }),
      json(200, {}),
    );
    expect(await cancelPlan("ws_1")).toEqual({
      tier: "FOUNDRY_TIER_MAX",
      cancelAtPeriodEnd: true,
    });
    expect(await resumePlan("ws_1")).toEqual({ tier: "FOUNDRY_TIER_MAX" });
    expect(await resumePlan("ws_1")).toEqual({});
    expect(posts().map((c) => c.url)).toEqual([
      "/papi/v1/foundry/plan:cancel",
      "/papi/v1/foundry/plan:resume",
      "/papi/v1/foundry/plan:resume",
    ]);
    expect(sent(1)).toEqual({ workspaceId: "ws_1" });
  });

  it("opens the billing portal, or says it could not", async () => {
    replies.push(
      json(200, { portalUrl: "https://billing.stripe.test/p" }),
      json(200, {}),
      json(200, { portalUrl: "javascript:alert(1)" }),
      json(400, { code: 9, message: "billing account is not linked yet" }),
    );
    expect(await billingPortalUrl("ws_1")).toBe(
      "https://billing.stripe.test/p",
    );
    expect(posts()[0]!.url).toBe("/papi/v1/billing/portal");
    await expect(billingPortalUrl("ws_1")).rejects.toThrow(
      "Couldn’t open the billing portal.",
    );
    await expect(billingPortalUrl("ws_1")).rejects.toThrow("not valid");
    await expect(billingPortalUrl("ws_1")).rejects.toMatchObject({
      status: 400,
      code: 9,
      message: "billing account is not linked yet",
    });
  });

  it("surfaces a Pro refusal the gate can read", async () => {
    replies.push(
      json(400, {
        code: 9,
        message: "PostgreSQL connectors need Foundry Pro",
        details: [
          {
            "@type": "type.googleapis.com/google.rpc.ErrorInfo",
            reason: "FOUNDRY_PRO_REQUIRED",
          },
        ],
      }),
    );
    const error = await startCheckout({
      workspaceId: "ws_1",
      clientToken: "t",
      tier: "pro",
    }).catch((e) => e);
    expect(isProRequired(error)).toBe(true);
  });
});
