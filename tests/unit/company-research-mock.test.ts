import { afterEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type {
  DraftGoalResponse,
  FoundryCompanyResearch,
  ResearchCompanyResponse,
} from "@/shared/wfe";
import type { MockReply } from "../../mock/http";

// The mock's company research answers as the WFE's ResearchFoundryCompany
// and DraftFoundryGoal do: owners and admins only, the workspace's AI
// first, a domain normalised (free mail and non-domains refused), the
// fixtures its header names, and drafting a goal from research of the same
// workspace with answers to its own questions.

const PATH = "/v1/workspaces/ws_acme/foundry/company-research";

/** The mock's routes and state, loaded afresh: hosted, or a local API. */
async function load(local = false) {
  vi.resetModules();
  vi.stubEnv("MOCK_LOCAL_TOKEN", local ? "local-token" : "");
  vi.stubEnv("MOCK_AI_DEFAULT", local ? "anthropic" : "");
  const { companyResearchRoutes } = await import(
    "../../mock/routes/company-research"
  );
  const state = await import("../../mock/state");
  state.resetState();
  Object.assign(state.config, { companyResearchMs: 0 });
  const compiled = companyResearchRoutes.map((r) => ({
    ...r,
    c: compileTemplate(r.path),
  }));
  const call = async (
    method: HttpMethod,
    path: string,
    body?: unknown,
    userId = "usr_ada",
  ): Promise<MockReply> => {
    for (const r of compiled) {
      const params = r.method === method && matchTemplate(r.c, path);
      if (!params) continue;
      return r.handle({
        method,
        path,
        params,
        query: new URLSearchParams(),
        body,
        principal: {
          user: {
            id: userId,
            workosId: "w",
            email: "e",
            firstName: "",
            lastName: "",
            createdAt: "",
          },
          sessionId: "s",
        },
      });
    }
    throw new Error(`no mock route for ${method} ${path}`);
  };
  const research = async (domain: unknown, userId?: string) =>
    call("POST", PATH, { domain }, userId);
  const config = (knobs: Record<string, unknown>) =>
    Object.assign(state.config, knobs);
  return { call, research, config, state };
}

const code = (r: MockReply) => (r.body as { code: number }).code;
const message = (r: MockReply) => (r.body as { message: string }).message;
const reason = (r: MockReply) =>
  (r.body as { details: { reason?: string }[] }).details[0]?.reason;
const researched = (r: MockReply) =>
  (r.body as ResearchCompanyResponse).research!;
const draftPath = (id: string) => `${PATH}/${id}:draft-goal`;

afterEach(() => {
  vi.unstubAllEnvs();
});

describe("company research in the mock", () => {
  it("researches a company's site: its name, summary, pages and questions", async () => {
    const { research } = await load();
    const r = await research(
      " https://www.Globex-Industries.com:443/about?x=1 ",
    );
    expect(r.status).toBe(200);
    const res = researched(r);
    expect(res).toMatchObject({
      domain: "globex-industries.com",
      companyName: "Globex Industries",
      suggestedName: "Globex Industries operations",
      sources: [
        "https://globex-industries.com/",
        "https://globex-industries.com/about",
      ],
      fetched: true,
    });
    expect(res.id).toMatch(/^res_/);
    expect(res.createdAt).toBeTruthy();
    expect(res.summary!.split(". ").length).toBe(2);
    expect([...res.summary!].length).toBeLessThanOrEqual(600);
    expect(res.questions!.map((q) => q.id)).toEqual(["q1", "q2", "q3", "q4"]);
    for (const q of res.questions!) {
      expect(q.bestGuess).toBeTruthy();
      expect(q.options!.length).toBeGreaterThan(0);
      expect(q.options!.length).toBeLessThanOrEqual(4);
    }
    // The same every time, but for its id.
    const again = researched(await research("globex-industries.com"));
    expect({ ...again, id: "", createdAt: "" }).toEqual({
      ...res,
      id: "",
      createdAt: "",
    });
    expect(again.id).not.toBe(res.id);
  });

  it("guesses from the domain alone when the site can't be read", async () => {
    const { research } = await load();
    const res = researched(await research("unreadable-co.com"));
    expect(res.fetched).toBe(false);
    expect(res.sources).toEqual([]);
    expect(res.summary).toContain("couldn’t be read");
    expect(res.questions!.length).toBe(4);
  });

  it("refuses what isn't a company's domain", async () => {
    const { research } = await load();
    for (const domain of ["", "acme", "10.0.0.1", "not a domain", 4, null]) {
      const r = await research(domain);
      expect(code(r)).toBe(3);
      expect(message(r)).toBe("enter your company’s domain, such as acme.com");
    }
    for (const domain of [
      "gmail.com",
      "https://outlook.com/",
      "yahoo.co.uk",
      "mail.yahoo.co.uk",
      "eu.gmail.com",
    ]) {
      const r = await research(domain);
      expect(code(r)).toBe(3);
      expect(message(r)).toMatch(/is a personal mail provider/);
    }
  });

  it("reduces a host to its registrable domain, as the client's companyDomain", async () => {
    const { research } = await load();
    for (const [raw, domain] of [
      ["eu.Globex.com", "globex.com"],
      ["https://shop.acme.co.uk/x", "acme.co.uk"],
      ["live.ucl.ac.uk", "ucl.ac.uk"],
    ])
      expect(researched(await research(raw)).domain, raw).toBe(domain);
    expect(code(await research("co.uk"))).toBe(3);
  });

  it("fails as the model failing on a domain containing fail", async () => {
    const { research } = await load();
    const r = await research("failco.com");
    expect(r.status).toBe(503);
    expect(code(r)).toBe(14);
    expect(reason(r)).toBe("FOUNDRY_RESEARCH_FAILED");
  });

  it("is for owners and admins of the workspace", async () => {
    const { research } = await load();
    expect(code(await research("globex.com", "usr_alan"))).toBe(7);
    expect(
      (await research("globex.com", "usr_nobody")).status,
    ).toBeGreaterThanOrEqual(400);
  });

  it("needs the workspace's AI", async () => {
    const { research, config } = await load(true);
    expect((await research("globex.com")).status).toBe(200);
    config({ aiDefault: false });
    const r = await research("globex.com");
    expect(code(r)).toBe(9);
    expect(reason(r)).toBe("FOUNDRY_AI_NOT_CONFIGURED");
  });

  it("fails every call as its knob says", async () => {
    const { research, call, config } = await load();
    const res = researched(await research("globex.com"));
    const outcomes: [string, number, string | undefined][] = [
      ["fail", 14, "FOUNDRY_RESEARCH_FAILED"],
      ["budget", 9, "FOUNDRY_RESEARCH_BUDGET"],
      ["rate_limit", 8, undefined],
    ];
    for (const [outcome, grpc, why] of outcomes) {
      config({ companyResearchOutcome: outcome });
      for (const r of [
        await research("globex.com"),
        await call("POST", draftPath(res.id), { answers: [], data: "" }),
      ]) {
        expect(code(r)).toBe(grpc);
        expect(reason(r)).toBe(why);
      }
    }
  });

  it("takes as long as companyResearchMs says", async () => {
    const { research, config } = await load();
    config({ companyResearchMs: 50 });
    const started = Date.now();
    expect((await research("globex.com")).status).toBe(200);
    expect(Date.now() - started).toBeGreaterThanOrEqual(45);
  });
});

describe("drafting a goal from company research in the mock", () => {
  const setup = async () => {
    const loaded = await load();
    const res: FoundryCompanyResearch = researched(
      await loaded.research("globex.com"),
    );
    const draft = (body: unknown, id = res.id, userId?: string) =>
      loaded.call("POST", draftPath(id), body, userId);
    return { ...loaded, res, draft };
  };

  it("writes a goal from the summary, the answers in order and the data, named as suggested", async () => {
    const { res, draft } = await setup();
    const r = await draft({
      answers: [
        { questionId: "q3", answer: "Billing" },
        { questionId: "q1", answer: " Finance " },
        { questionId: "q2", answer: "" },
      ],
      data: "Intro line\n\n**Shop** (PostgreSQL), 2 tables:\n- `orders`.\n- `customers`.\n- `refunds`.",
    });
    expect(r.status).toBe(200);
    const { goal, name } = r.body as DraftGoalResponse;
    expect(name).toBe("Globex operations");
    const lines = goal!.split("\n");
    expect(lines[0]).toContain(res.summary!);
    const q = (id: string) => res.questions!.find((x) => x.id === id)!.question;
    expect(lines.slice(2, 4)).toEqual([
      `${q("q1")} Finance`,
      `${q("q3")} Billing`,
    ]);
    expect(goal).toContain(
      "It reads:\n**Shop** (PostgreSQL), 2 tables:\n- `orders`.\n- `customers`.",
    );
    expect(goal).not.toContain("refunds");
    expect(goal).not.toContain(q("q2"));
  });

  it("caps the goal at 2,000 characters", async () => {
    const { draft } = await setup();
    const long = "x".repeat(500);
    const { goal } = (
      await draft({
        answers: ["q1", "q2", "q3", "q4"].map((questionId) => ({
          questionId,
          answer: long,
        })),
        data: "",
      })
    ).body as DraftGoalResponse;
    expect([...goal!].length).toBe(2000);
  });

  it("refuses research it doesn't hold, or another workspace's", async () => {
    const { draft, call, state, res } = await setup();
    expect(code(await draft({ answers: [], data: "" }, "res_gone"))).toBe(5);
    // Ada owns another workspace, which can't read Acme's research.
    state.db.workspaces.set("ws_other", {
      ...state.db.workspaces.get("ws_acme")!,
      id: "ws_other",
      slug: "other",
    });
    state.db.memberships.push({
      id: "mem_ada_other",
      workspaceId: "ws_other",
      userId: "usr_ada",
      role: "WORKSPACE_ROLE_OWNER",
      createdAt: "2026-01-01T00:00:00Z",
    });
    const r = await call(
      "POST",
      `/v1/workspaces/ws_other/foundry/company-research/${res.id}:draft-goal`,
      { answers: [], data: "" },
    );
    expect(code(r)).toBe(5);
    expect(message(r)).toBe("company research not found");
  });

  it("refuses answers to no question of it, and answers or data too long", async () => {
    const { draft } = await setup();
    const unknown = await draft({
      answers: [{ questionId: "q9", answer: "x" }],
      data: "",
    });
    expect(code(unknown)).toBe(3);
    expect(message(unknown)).toBe('answers: no question "q9" in this research');
    // An answer that isn't a question id and a string is refused, not thrown on.
    for (const answer of [
      { questionId: "q1", answer: 4 },
      { questionId: "q1" },
      { questionId: 1, answer: "x" },
      "q1",
      null,
    ]) {
      const r = await draft({ answers: [answer], data: "" });
      expect(code(r), JSON.stringify(answer)).toBe(3);
      expect(message(r)).toBe(
        "answers: each is a questionId and an answer, both strings",
      );
    }
    expect(
      code(
        await draft({
          answers: [{ questionId: "q1", answer: "x".repeat(501) }],
          data: "",
        }),
      ),
    ).toBe(3);
    expect(code(await draft({ answers: [], data: "x".repeat(12_001) }))).toBe(
      3,
    );
    // At the limits it is fine.
    expect(
      (
        await draft({
          answers: [{ questionId: "q1", answer: "😀".repeat(500) }],
          data: "x".repeat(12_000),
        })
      ).status,
    ).toBe(200);
  });

  it("is for owners and admins", async () => {
    const { draft, res } = await setup();
    expect(
      code(await draft({ answers: [], data: "" }, res.id, "usr_alan")),
    ).toBe(7);
  });
});
