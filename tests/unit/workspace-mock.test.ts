import type { IncomingMessage, ServerResponse } from "node:http";
import { beforeEach, describe, expect, it } from "vitest";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { MockReply, MockRequest } from "../../mock/http";
import { routes } from "../../mock/routes";
import {
  handleBillingPage,
  isFoundryPro,
  setFoundryPlan,
} from "../../mock/routes/billing";
import { config, db, resetState, type MockUser } from "../../mock/state";

// The mock WFE's workspace routes keep the Helix backend's rules, so the
// UI is tested against the same refusals it meets on the dev stamp.

process.env.MOCK_ORIGIN = "http://127.0.0.1:4999";
process.env.NEXT_PUBLIC_WORKOS_REDIRECT_URI =
  "http://localhost:3999/login/callback";

const user = (id: string): MockUser => db.users.get(id)!;
async function call(
  method: string,
  path: string,
  as: string,
  body?: unknown,
): Promise<MockReply> {
  const [pathname, search = ""] = path.split("?");
  for (const r of routes) {
    const params =
      r.method === method && matchTemplate(compileTemplate(r.path), pathname!);
    if (!params) continue;
    const req: MockRequest = {
      method: r.method,
      path: pathname!,
      params,
      query: new URLSearchParams(search),
      body,
      principal: { user: user(as), sessionId: "s" },
    };
    return r.handle(req);
  }
  throw new Error(`no mock route for ${method} ${path}`);
}
const body = (r: MockReply) => r.body as any;

beforeEach(() => resetState());

describe("workspaces", () => {
  it("lists the caller's workspaces in pages", async () => {
    for (const n of [1, 2, 3, 4]) {
      db.workspaces.set(`ws_${n}`, { id: `ws_${n}`, slug: `w${n}` });
      db.memberships.push({
        id: `mw${n}`,
        workspaceId: `ws_${n}`,
        userId: "usr_ada",
        role: "WORKSPACE_ROLE_MEMBER",
        createdAt: "2026-10-01T00:00:00Z",
      });
    }
    const first = await call("GET", "/v1/workspaces?page.size=2", "usr_ada");
    expect(body(first).workspaces.map((w: any) => w.id)).toEqual([
      "ws_acme",
      "ws_1",
    ]);
    expect(body(first).nextPageToken).toBe("2");
    const last = await call(
      "GET",
      "/v1/workspaces?page.size=2&page.token=4",
      "usr_ada",
    );
    expect(body(last).workspaces.map((w: any) => w.id)).toEqual(["ws_4"]);
    expect(body(last).nextPageToken).toBeUndefined();
    // Unset or over 200 is 50, as the backend.
    const all = await call("GET", "/v1/workspaces?page.size=500", "usr_ada");
    expect(body(all).workspaces).toHaveLength(5);
    expect(
      (await call("GET", "/v1/workspaces?page.token=x", "usr_ada")).status,
    ).toBe(400);
  });
});

describe("members", () => {
  it("lists a workspace's members in pages", async () => {
    for (const n of [1, 2, 3]) {
      db.users.set(`u${n}`, {
        id: `u${n}`,
        workosId: `w${n}`,
        email: `u${n}@example.com`,
        firstName: `User${n}`,
        lastName: "",
        createdAt: "2026-10-01T00:00:00Z",
      });
      db.memberships.push({
        id: `m${n}`,
        workspaceId: "ws_acme",
        userId: `u${n}`,
        role: "WORKSPACE_ROLE_MEMBER",
        createdAt: "2026-10-01T00:00:00Z",
      });
    }
    const first = await call(
      "GET",
      "/v1/workspaces/ws_acme/members?page.size=3",
      "usr_ada",
    );
    expect(first.status).toBe(200);
    expect(body(first).members.map((m: any) => m.user.email)).toEqual([
      "ada@example.com",
      "alan@example.com",
      "u1@example.com",
    ]);
    expect(body(first).nextPageToken).toBe("3");
    const rest = await call(
      "GET",
      "/v1/workspaces/ws_acme/members?page.size=3&page.token=3",
      "usr_ada",
    );
    expect(body(rest).members).toHaveLength(2);
    expect(body(rest).nextPageToken).toBeUndefined();
    expect(body(rest).members[0]).toMatchObject({
      workspaceId: "ws_acme",
      role: "WORKSPACE_ROLE_MEMBER",
      user: { id: "u2", displayName: "User2" },
    });
  });

  it("refuses a page it did not hand out", async () => {
    for (const q of ["page.size=-5", "page.token=abc", "page.size=1.5"])
      expect(
        (await call("GET", `/v1/workspaces/ws_acme/members?${q}`, "usr_ada"))
          .status,
        q,
      ).toBe(400);
  });

  it("refuses a workspace the caller is not in", async () => {
    db.users.set("usr_eve", { ...user("usr_ada"), id: "usr_eve" });
    const r = await call("GET", "/v1/workspaces/ws_acme/members", "usr_eve");
    expect(r.status).toBe(403);
  });

  it("makes Alan an admin of Acme with the alanRole knob, until a reset", async () => {
    const alan = async () =>
      body(
        await call("GET", "/v1/workspaces/ws_acme/members", "usr_ada"),
      ).members.find((m: any) => m.user.id === "usr_alan").role;
    const invite = () =>
      call("POST", "/v1/workspaces/ws_acme/members:invite", "usr_alan", {
        email: "kay@example.com",
        role: "WORKSPACE_ROLE_MEMBER",
      });
    expect(await alan()).toBe("WORKSPACE_ROLE_MEMBER");
    expect((await invite()).status).toBe(403);
    Object.assign(config, { alanRole: "WORKSPACE_ROLE_ADMIN" });
    expect(await alan()).toBe("WORKSPACE_ROLE_ADMIN");
    expect((await invite()).status).toBe(200);
    resetState();
    expect(await alan()).toBe("WORKSPACE_ROLE_MEMBER");
  });
});

describe("invitations", () => {
  const invite = (as: string, email: string, role?: string) =>
    call("POST", "/v1/workspaces/ws_acme/members:invite", as, { email, role });

  it("sends one invitation per email", async () => {
    expect(
      (await invite("usr_ada", " Grace@Example.com ", "WORKSPACE_ROLE_MEMBER"))
        .status,
    ).toBe(200);
    const again = await invite(
      "usr_ada",
      "grace@example.com",
      "WORKSPACE_ROLE_ADMIN",
    );
    expect(again.status).toBe(409);
    expect(body(again).message).toContain("pending invitation");
    const member = await invite(
      "usr_ada",
      "alan@example.com",
      "WORKSPACE_ROLE_MEMBER",
    );
    expect(member.status).toBe(409);
    expect(body(member).message).toContain("already a member");
  });

  it("checks the email, the role and who is asking", async () => {
    expect(
      (await invite("usr_ada", "not-an-email", "WORKSPACE_ROLE_MEMBER")).status,
    ).toBe(400);
    expect((await invite("usr_ada", "g@example.com")).status).toBe(400);
    expect(
      (await invite("usr_ada", "g@example.com", "WORKSPACE_ROLE_UNSPECIFIED"))
        .status,
    ).toBe(400);
    // Members cannot invite; admins cannot invite owners; owners can.
    expect(
      (await invite("usr_alan", "g@example.com", "WORKSPACE_ROLE_MEMBER"))
        .status,
    ).toBe(403);
    Object.assign(config, { alanRole: "WORKSPACE_ROLE_ADMIN" });
    expect(
      (await invite("usr_alan", "g@example.com", "WORKSPACE_ROLE_OWNER"))
        .status,
    ).toBe(403);
    expect(
      (await invite("usr_ada", "g@example.com", "WORKSPACE_ROLE_OWNER")).status,
    ).toBe(200);
  });
});

describe("Foundry plan", () => {
  const plan = async (as = "usr_ada") =>
    body(await call("GET", "/v1/foundry/plan?workspaceId=ws_acme", as)).plan;
  const post = (path: string, data: unknown, as = "usr_ada") =>
    call("POST", path, as, data);

  it("seeds Acme on Pro, which the Data routes' gate reads", async () => {
    expect(await plan()).toMatchObject({
      tier: "FOUNDRY_TIER_PRO",
      cancelAtPeriodEnd: false,
    });
    expect(isFoundryPro("ws_acme")).toBe(true);
    // Max passes the gate as Pro does.
    setFoundryPlan("ws_acme", "FOUNDRY_TIER_MAX");
    expect(isFoundryPro("ws_acme")).toBe(true);
    expect((await plan()).tier).toBe("FOUNDRY_TIER_MAX");
    setFoundryPlan("ws_acme", "FOUNDRY_TIER_FREE");
    expect(isFoundryPro("ws_acme")).toBe(false);
    expect(await plan()).toEqual({ tier: "FOUNDRY_TIER_FREE" });
  });

  describe("from Free", () => {
    // Acme is seeded on Pro; these start it on Free.
    beforeEach(() => setFoundryPlan("ws_acme", "FOUNDRY_TIER_FREE"));

    it("starts Free and needs a workspace", async () => {
      expect(await plan()).toEqual({ tier: "FOUNDRY_TIER_FREE" });
      // Members can read it.
      expect(await plan("usr_alan")).toEqual({ tier: "FOUNDRY_TIER_FREE" });
      expect((await call("GET", "/v1/foundry/plan", "usr_ada")).status).toBe(
        400,
      );
    });

    it("checks out through the checkout page and returns to billing", async () => {
      const r = await post("/v1/foundry/plan:checkout", {
        workspaceId: "ws_acme",
        clientToken: "t1",
        promoCode: "FOUNDRYTEST50",
      });
      const url = new URL(body(r).checkoutUrl);
      expect(url.origin + url.pathname).toBe(
        "http://127.0.0.1:4999/mock/checkout",
      );
      expect(body(r).plan).toEqual({ tier: "FOUNDRY_TIER_FREE" });

      const page = fakeResponse();
      expect(
        handleBillingPage(
          { method: "GET" } as IncomingMessage,
          page.res,
          url,
          "",
        ),
      ).toBe(true);
      expect(page.status).toBe(200);
      expect(page.body).toContain("<h1>Foundry Pro</h1>");
      expect(page.body).toContain("Pay and subscribe");

      const paid = fakeResponse();
      handleBillingPage(
        { method: "POST" } as IncomingMessage,
        paid.res,
        new URL("/mock/checkout", process.env.MOCK_ORIGIN),
        "session=" + url.searchParams.get("session"),
      );
      expect(paid.status).toBe(303);
      expect(paid.headers.Location).toBe(
        "http://localhost:3999/settings/billing",
      );
      expect(await plan()).toMatchObject({
        tier: "FOUNDRY_TIER_PRO",
        cancelAtPeriodEnd: false,
        pastDue: false,
      });
      // A session pays once.
      const again = fakeResponse();
      handleBillingPage(
        { method: "POST" } as IncomingMessage,
        again.res,
        new URL("/mock/checkout", process.env.MOCK_ORIGIN),
        "session=" + url.searchParams.get("session"),
      );
      expect(again.status).toBe(404);
      // Already Pro: checkout attaches nothing.
      const held = await post("/v1/foundry/plan:checkout", {
        workspaceId: "ws_acme",
        clientToken: "t2",
      });
      expect(body(held).checkoutUrl).toBeUndefined();
      expect(body(held).plan.tier).toBe("FOUNDRY_TIER_PRO");
    });

    it("buys Max through the checkout page", async () => {
      const r = await post("/v1/foundry/plan:checkout", {
        workspaceId: "ws_acme",
        clientToken: "t1",
        tier: "FOUNDRY_TIER_MAX",
      });
      const url = new URL(body(r).checkoutUrl);
      const page = fakeResponse();
      handleBillingPage(
        { method: "GET" } as IncomingMessage,
        page.res,
        url,
        "",
      );
      expect(page.body).toContain("<h1>Foundry Max</h1>");
      handleBillingPage(
        { method: "POST" } as IncomingMessage,
        fakeResponse().res,
        new URL("/mock/checkout", process.env.MOCK_ORIGIN),
        "session=" + url.searchParams.get("session"),
      );
      expect(await plan()).toMatchObject({
        tier: "FOUNDRY_TIER_MAX",
        cancelAtPeriodEnd: false,
      });
      expect(isFoundryPro("ws_acme")).toBe(true);
    });

    it("reads an unset or UNSPECIFIED tier as Pro and refuses others", async () => {
      config.checkoutCardOnFile = true;
      for (const tier of [
        "FOUNDRY_TIER_FREE",
        "FOUNDRY_TIER_BUSINESS",
        "constructor",
      ]) {
        const r = await post("/v1/foundry/plan:checkout", {
          workspaceId: "ws_acme",
          clientToken: `t-${tier}`,
          tier,
        });
        expect(r.status, tier).toBe(400);
        expect(body(r).message).toBe(
          "tier must be FOUNDRY_TIER_PRO or FOUNDRY_TIER_MAX",
        );
      }
      await post("/v1/foundry/plan:checkout", {
        workspaceId: "ws_acme",
        clientToken: "t1",
        tier: "FOUNDRY_TIER_UNSPECIFIED",
      });
      await plan();
      expect((await plan()).tier).toBe("FOUNDRY_TIER_PRO");
    });

    it("refuses a bad code, a repeated token and non-managers", async () => {
      const bad = await post("/v1/foundry/plan:checkout", {
        workspaceId: "ws_acme",
        clientToken: "t1",
        promoCode: "NOPE",
      });
      expect(bad.status).toBe(400);
      expect(
        (await post("/v1/foundry/plan:checkout", { workspaceId: "ws_acme" }))
          .status,
      ).toBe(400);
      // The refused code charged nothing, so its token is still unused.
      expect(
        (
          await post("/v1/foundry/plan:checkout", {
            workspaceId: "ws_acme",
            clientToken: "t1",
          })
        ).status,
      ).toBe(200);
      const repeat = await post("/v1/foundry/plan:checkout", {
        workspaceId: "ws_acme",
        clientToken: "t1",
      });
      expect(repeat.status).toBe(503);
      for (const path of [
        "/v1/foundry/plan:checkout",
        "/v1/foundry/plan:cancel",
        "/v1/foundry/plan:resume",
        "/v1/billing/portal",
      ])
        expect(
          (
            await post(
              path,
              { workspaceId: "ws_acme", clientToken: "t9" },
              "usr_alan",
            )
          ).status,
          path,
        ).toBe(403);
    });

    it("charges a card on file, reading Free once", async () => {
      config.checkoutCardOnFile = true;
      const r = await post("/v1/foundry/plan:checkout", {
        workspaceId: "ws_acme",
        clientToken: "t1",
      });
      expect(body(r)).toEqual({ plan: { tier: "FOUNDRY_TIER_FREE" } });
      expect((await plan()).tier).toBe("FOUNDRY_TIER_FREE");
      expect((await plan()).tier).toBe("FOUNDRY_TIER_PRO");
    });

    it("cancels and resumes a paid plan only", async () => {
      const ws = { workspaceId: "ws_acme" };
      expect((await post("/v1/foundry/plan:cancel", ws)).status).toBe(400);
      expect((await post("/v1/foundry/plan:resume", ws)).status).toBe(400);
      config.checkoutCardOnFile = true;
      await post("/v1/foundry/plan:checkout", { ...ws, clientToken: "t" });
      const cancelled = await post("/v1/foundry/plan:cancel", ws);
      expect(body(cancelled).plan).toMatchObject({
        tier: "FOUNDRY_TIER_PRO",
        cancelAtPeriodEnd: true,
      });
      const resumed = await post("/v1/foundry/plan:resume", ws);
      expect(body(resumed).plan.cancelAtPeriodEnd).toBe(false);
    });
  });

  describe("switching between Pro and Max", () => {
    const checkout = (clientToken: string, tier?: string) =>
      post("/v1/foundry/plan:checkout", {
        workspaceId: "ws_acme",
        clientToken,
        tier,
      });

    it("switches Pro to Max and back at once, reading the old plan once", async () => {
      const up = await checkout("t1", "FOUNDRY_TIER_MAX");
      // A switch charges the card on file: no checkout page.
      expect(body(up).checkoutUrl).toBeUndefined();
      expect(body(up).plan.tier).toBe("FOUNDRY_TIER_PRO");
      expect((await plan()).tier).toBe("FOUNDRY_TIER_PRO");
      expect(await plan()).toMatchObject({
        tier: "FOUNDRY_TIER_MAX",
        cancelAtPeriodEnd: false,
      });
      expect(isFoundryPro("ws_acme")).toBe(true);
      // The same token again is one purchase, not a second switch.
      expect((await checkout("t1", "FOUNDRY_TIER_PRO")).status).toBe(503);

      const down = await checkout("t2", "FOUNDRY_TIER_PRO");
      expect(body(down).checkoutUrl).toBeUndefined();
      expect(body(down).plan.tier).toBe("FOUNDRY_TIER_MAX");
      await plan();
      expect((await plan()).tier).toBe("FOUNDRY_TIER_PRO");
      expect(isFoundryPro("ws_acme")).toBe(true);
    });

    it("attaches nothing for the tier held, Pro when unset", async () => {
      const pro = await checkout("t1");
      expect(body(pro)).toEqual({ plan: await plan() });
      expect(body(pro).plan.tier).toBe("FOUNDRY_TIER_PRO");
      setFoundryPlan("ws_acme", "FOUNDRY_TIER_MAX");
      const max = await checkout("t2", "FOUNDRY_TIER_MAX");
      expect(body(max).checkoutUrl).toBeUndefined();
      expect(body(max).plan.tier).toBe("FOUNDRY_TIER_MAX");
      // Neither used its token.
      expect((await checkout("t1", "FOUNDRY_TIER_PRO")).status).toBe(200);
    });

    it("cancels and resumes Max, and a switch renews a cancelled plan", async () => {
      setFoundryPlan("ws_acme", "FOUNDRY_TIER_MAX");
      const ws = { workspaceId: "ws_acme" };
      expect(
        body(await post("/v1/foundry/plan:cancel", ws)).plan,
      ).toMatchObject({
        tier: "FOUNDRY_TIER_MAX",
        cancelAtPeriodEnd: true,
      });
      expect(
        body(await post("/v1/foundry/plan:resume", ws)).plan,
      ).toMatchObject({
        tier: "FOUNDRY_TIER_MAX",
        cancelAtPeriodEnd: false,
      });
      await post("/v1/foundry/plan:cancel", ws);
      await checkout("t1", "FOUNDRY_TIER_PRO");
      await plan();
      expect(await plan()).toMatchObject({
        tier: "FOUNDRY_TIER_PRO",
        cancelAtPeriodEnd: false,
      });
    });

    it("refuses members", async () => {
      expect(
        (
          await post(
            "/v1/foundry/plan:checkout",
            {
              workspaceId: "ws_acme",
              clientToken: "t",
              tier: "FOUNDRY_TIER_MAX",
            },
            "usr_alan",
          )
        ).status,
      ).toBe(403);
      expect((await plan()).tier).toBe("FOUNDRY_TIER_PRO");
    });
  });

  it("opens the billing portal page", async () => {
    const r = await post("/v1/billing/portal", { workspaceId: "ws_acme" });
    const url = new URL(body(r).portalUrl);
    const page = fakeResponse();
    expect(
      handleBillingPage(
        { method: "GET" } as IncomingMessage,
        page.res,
        url,
        "",
      ),
    ).toBe(true);
    expect(page.body).toContain("Acme");
    expect(
      handleBillingPage(
        { method: "GET" } as IncomingMessage,
        fakeResponse().res,
        new URL("/mock/elsewhere", process.env.MOCK_ORIGIN),
        "",
      ),
    ).toBe(false);
  });
});

function fakeResponse() {
  const out = {
    status: 0,
    headers: {} as Record<string, string>,
    body: "",
    res: undefined as unknown as ServerResponse,
  };
  out.res = {
    writeHead(status: number, headers: Record<string, string> = {}) {
      out.status = status;
      out.headers = headers;
      return this;
    },
    end(chunk?: string) {
      out.body = chunk || "";
      return this;
    },
  } as unknown as ServerResponse;
  return out;
}
