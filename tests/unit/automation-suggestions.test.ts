import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import {
  AUTOMATION_SUGGESTIONS,
  readSuggestions,
  shownSuggestions,
  SUGGESTION_TONE,
  suggestionOntology,
} from "@/web/areas/automations/model";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import {
  fallbackSuggestions,
  questionTitle,
} from "../../mock/routes/automation-suggestions";
import { ontologyVersion } from "../../mock/routes/ontology";
import { config, db, resetState } from "../../mock/state";

// The new automation page's suggestions: which ontology they are for, what
// shows while they come, a response read without trust, and the mock's
// route, which answers as the backend's does.

const WATCH = "FOUNDRY_AUTOMATION_SUGGESTION_ICON_WATCH";

describe("suggestionOntology", () => {
  it("takes the one chosen here, else the one last opened, else the first", () => {
    expect(suggestionOntology(["a", "b", "c"], "c", "b")).toBe("c");
    expect(suggestionOntology(["a", "b", "c"], "", "b")).toBe("b");
    expect(suggestionOntology(["a", "b", "c"], "gone", "gone too")).toBe("a");
    expect(suggestionOntology([], "a", "a")).toBeUndefined();
  });
});

describe("readSuggestions", () => {
  it("keeps titles and prompts within the limits, trimmed, at most 4, icons mapped", () => {
    expect(
      readSuggestions({
        suggestions: [
          { title: " Renewal radar ", prompt: " Every day. ", icon: WATCH },
          { title: "", prompt: "No title" },
          { title: "No prompt", prompt: "  " },
          { title: "x".repeat(33), prompt: "Too long a title" },
          { title: "Too long a prompt", prompt: "x".repeat(401) },
          { title: "Unknown icon", prompt: "p", icon: "toString" as never },
          { title: "No icon", prompt: "p" },
          { title: "B", prompt: "p", icon: WATCH },
          { title: "C", prompt: "p", icon: WATCH },
        ],
      }),
    ).toEqual([
      { title: "Renewal radar", prompt: "Every day.", icon: "watch" },
      { title: "Unknown icon", prompt: "p", icon: "report" },
      { title: "No icon", prompt: "p", icon: "report" },
      { title: "B", prompt: "p", icon: "watch" },
    ]);
  });

  it("counts characters, not UTF-16 units, and takes anything else as none", () => {
    expect(
      readSuggestions({
        suggestions: [{ title: "é".repeat(32), prompt: "p" }],
      }),
    ).toHaveLength(1);
    expect(readSuggestions(undefined)).toEqual([]);
    expect(readSuggestions({ suggestions: "x" as never })).toEqual([]);
    expect(readSuggestions({ suggestions: [null, 3] as never })).toEqual([]);
  });

  it("gives every icon a tone of its own", () => {
    expect(new Set(Object.values(SUGGESTION_TONE)).size).toBe(6);
    expect(AUTOMATION_SUGGESTIONS.map((x) => SUGGESTION_TONE[x.icon])).toEqual([
      "info",
      "accent",
      "warning",
      "danger",
    ]);
  });
});

describe("shownSuggestions", () => {
  const fixed = {
    kind: "chips",
    chips: AUTOMATION_SUGGESTIONS,
    generating: false,
  };
  const one = { title: "A", prompt: "p", icon: WATCH } as const;
  it("is the fixed ones with no ontology, a failed read or nothing to show", () => {
    expect(
      shownSuggestions({ ontology: false, pending: true, failed: false }),
    ).toEqual(fixed);
    expect(
      shownSuggestions({ ontology: true, pending: false, failed: true }),
    ).toEqual(fixed);
    expect(
      shownSuggestions({
        ontology: true,
        pending: false,
        failed: false,
        data: {},
      }),
    ).toEqual(fixed);
  });
  it("waits while read, and while written with nothing meanwhile", () => {
    expect(
      shownSuggestions({ ontology: true, pending: true, failed: false }),
    ).toEqual({
      kind: "loading",
      generating: false,
    });
    expect(
      shownSuggestions({
        ontology: true,
        pending: false,
        failed: false,
        data: { generating: true },
      }),
    ).toEqual({ kind: "loading", generating: true });
  });
  it("keeps what was read when a later poll fails", () => {
    expect(
      shownSuggestions({
        ontology: true,
        pending: false,
        failed: true,
        data: { suggestions: [one], generating: true },
      }),
    ).toEqual({
      kind: "chips",
      chips: [{ title: "A", prompt: "p", icon: "watch" }],
      generating: true,
    });
  });
  it("shows the fallback while better ones are written, then those", () => {
    expect(
      shownSuggestions({
        ontology: true,
        pending: false,
        failed: false,
        data: { suggestions: [one], generating: true },
      }),
    ).toEqual({
      kind: "chips",
      chips: [{ title: "A", prompt: "p", icon: "watch" }],
      generating: true,
    });
  });
});

describe("the fallback, as the backend writes it", () => {
  // backend/automation/suggest/suggest_test.go's TestTitle and TestFallback.
  it.each([
    ["Which customers are under contract?", "Customers are under contract"],
    ["which  customers\tchurned last month?!", "Customers churned last month"],
    ["How many invoices are overdue?", "Invoices are overdue"],
    ["What is the total revenue?", "The total revenue"],
    [
      "What is each customer under contract billed?",
      "Each customer under contract",
    ],
    ["What are our largest invoices?", "Our largest invoices"],
    ["What is overdue?", "What is overdue"],
    ["What does each term cost?", "What does each term cost"],
    ["What plans do customers hold?", "Plans do customers hold"],
    ["When does each term end?", "When does each term end"],
    ["Who signed the contract?", "Who signed the contract"],
    ["Which?", "Which"],
    [
      "Which contracts renew in the next thirty days of the year",
      "Contracts renew in the next",
    ],
    [
      "Which customers are billed in a currency other than USD",
      "Customers are billed",
    ],
    [
      "Which ones have the most open support tickets with us",
      "Ones have the most open support",
    ],
    ["what's overdue", "What's overdue"],
    [
      "Supercalifragilisticexpialidociousnessly long word here",
      "Supercalifragilisticexpialidocio",
    ],
    ["élan of each account", "Élan of each account"],
    ["  ?  ", ""],
    ["", ""],
    ["Which, of these?", "Which, of these"],
  ])("titles %j as %j", (question, want) => {
    expect(questionTitle(question)).toBe(want);
    expect([...want].length).toBeLessThanOrEqual(32);
  });

  it("answers the first 4 questions with titles of their own every Monday by email", () => {
    const got = fallbackSuggestions("  Customer   contracts ", [
      "Which customers are under contract?",
      "Which customers are under contract?!",
      "How many invoices are overdue?",
      "  ",
      "When does each term end?",
      "What is the total revenue?",
      "Past the first four.",
    ]);
    expect(got[0]).toEqual({
      title: "Customers are under contract",
      prompt:
        'Every Monday, answer: "Which customers are under contract?" from Customer contracts, and email me the answer.',
      icon: "FOUNDRY_AUTOMATION_SUGGESTION_ICON_REPORT",
    });
    expect(got.map((x) => x.title)).toEqual([
      "Customers are under contract",
      "Invoices are overdue",
      "When does each term end",
      "The total revenue",
    ]);
    expect(fallbackSuggestions("Sales", [])).toEqual([]);
  });

  it("cuts a long question so the prompt fits, a long name first", () => {
    const [long] = fallbackSuggestions("n".repeat(300), [
      "Which " + "word ".repeat(200) + "?",
    ]);
    expect([...long!.prompt!]).toHaveLength(400);
    expect(long!.prompt).toContain(
      `…" from ${"n".repeat(99)}…, and email me the answer.`,
    );
  });
});

describe("the mock's route", () => {
  const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));
  async function call(
    method: "GET" | "POST",
    path: string,
    body?: unknown,
    at = { compiled, users: db.users },
  ): Promise<MockReply & { body: any }> {
    const url = new URL(path, "http://mock");
    const r = at.compiled.find(
      (r) => r.method === method && matchTemplate(r.c, url.pathname),
    )!;
    return (await r.handle({
      method,
      path: url.pathname,
      params: matchTemplate(r.c, url.pathname) as Record<string, string>,
      query: url.searchParams,
      body,
      principal: { user: at.users.get("usr_ada")!, sessionId: "s" },
    })) as MockReply & { body: any };
  }
  const get = (ontologyId: string, ws = "ws_acme") =>
    call(
      "GET",
      `/v1/workspaces/${ws}/foundry/automation-suggestions?ontologyId=${ontologyId}`,
    );
  const later = (ms: number) => vi.setSystemTime(Date.now() + ms);
  const titles = (r: { body: any }) =>
    (r.body.suggestions || []).map((x: { title: string }) => x.title);

  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-11-16T08:00:00Z"));
    resetState();
  });
  afterEach(() => vi.useRealTimers());

  it("answers the competency questions while it writes, then Founder's", async () => {
    const first = await get("ont_acme_renewals");
    expect(first.body).toMatchObject({
      source: "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_QUESTIONS",
      generating: true,
      versionId: "ver_renewals_1",
    });
    expect(titles(first)).toEqual([
      "Customers are under contract",
      "Each customer under contract",
    ]);
    later(1500);
    const ready = await get("ont_acme_renewals");
    expect(ready.body).toMatchObject({
      source: "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_MODEL",
      generating: false,
    });
    expect(titles(ready)).toEqual([
      "Renewal radar",
      "Contract check",
      "Invoice watch",
      "Monday contracts brief",
    ]);
    expect(ready.body.suggestions[0].prompt).toContain("in Renewals");
  });

  it("writes people's for a graph of people, at once with no wait", async () => {
    Object.assign(config, { automationSuggestionsMs: 0 });
    expect(titles(await get("ont_acme_sales"))).toEqual([
      "New contacts brief",
      "Job change alert",
      "Account map refresh",
      "Weekly people report",
    ]);
  });

  it("keeps every suggestion it writes within the limits", async () => {
    Object.assign(config, { automationSuggestionsMs: 0 });
    for (const o of [
      "ont_acme_sales",
      "ont_acme_renewals",
      "ont_acme_contracts",
    ])
      for (const x of (await get(o)).body.suggestions) {
        expect([...x.title].length).toBeLessThanOrEqual(32);
        expect([...x.prompt].length).toBeLessThanOrEqual(400);
      }
  });

  it("answers the fallback, not generating, when the writing fails", async () => {
    Object.assign(config, { automationSuggestionsOutcome: "fail" });
    expect((await get("ont_acme_renewals")).body.generating).toBe(true);
    later(1500);
    const r = await get("ont_acme_renewals");
    expect(r.body).toMatchObject({
      source: "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_QUESTIONS",
      generating: false,
    });
    expect(titles(r)).toHaveLength(2);
  });

  it.each([[{ automationSuggestionsAi: false }], [{ agentEnabled: false }]])(
    "never writes them with %o",
    async (knob) => {
      Object.assign(config, knob);
      const r = await get("ont_acme_renewals");
      expect(r.body).toMatchObject({
        source: "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_QUESTIONS",
        generating: false,
      });
      later(5000);
      expect((await get("ont_acme_renewals")).body.generating).toBe(false);
    },
  );

  it("queues a failed one again an hour after it failed", async () => {
    Object.assign(config, { automationSuggestionsOutcome: "fail" });
    await get("ont_acme_renewals");
    later(1500);
    expect((await get("ont_acme_renewals")).body.generating).toBe(false);
    delete (config as Record<string, unknown>).automationSuggestionsOutcome;
    // Within the hour of its failing (not of its asking), it stays failed.
    later(3_600_000 - 1000);
    expect((await get("ont_acme_renewals")).body.generating).toBe(false);
    later(1001);
    const again = await get("ont_acme_renewals");
    expect(again.body).toMatchObject({
      source: "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_QUESTIONS",
      generating: true,
    });
    later(1500);
    expect(titles(await get("ont_acme_renewals"))).toContain("Renewal radar");
  });

  it("fails a version with nothing to write from, never ready with none", async () => {
    ontologyVersion("ont_acme_contracts", "ver_seed_1")!.definition = {};
    Object.assign(config, { automationSuggestionsMs: 0 });
    expect((await get("ont_acme_contracts")).body).toMatchObject({
      suggestions: [],
      source: "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_UNSPECIFIED",
      generating: false,
    });
  });

  it("has none for an ontology without a current version, and writes none", async () => {
    const made = await call(
      "POST",
      "/v1/workspaces/ws_acme/foundry/ontologies",
      {
        name: "Empty",
        tenantId: "ten_acme_sandbox",
        clientToken: "empty",
      },
    );
    const id = made.body.ontology.id as string;
    for (let i = 0; i < 2; i++) {
      expect((await get(id)).body).toEqual({
        suggestions: [],
        source: "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_UNSPECIFIED",
        generating: false,
        versionId: "",
      });
      later(5000);
    }
  });

  it("has none for another workspace's ontology", async () => {
    db.workspaces.set("ws_other", {
      ...db.workspaces.get("ws_acme")!,
      id: "ws_other",
      slug: "other",
    });
    db.memberships.push({
      id: "mem_ada_other",
      workspaceId: "ws_other",
      userId: "usr_ada",
      role: "WORKSPACE_ROLE_OWNER",
      createdAt: "2026-01-01T00:00:00Z",
    });
    const r = await get("ont_acme_renewals", "ws_other");
    expect(r.status).toBe(404);
    expect(r.body.code).toBe(5);
  });

  it("has none to give a Definition v1 until written", async () => {
    const r = await get("ont_acme_contracts");
    expect(r.body).toMatchObject({
      suggestions: [],
      source: "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_UNSPECIFIED",
      generating: true,
      versionId: "ver_seed_1",
    });
    later(1500);
    expect(titles(await get("ont_acme_contracts"))).toContain("Invoice watch");
  });

  it("refuses what the backend refuses", async () => {
    expect((await get("")).status).toBe(400);
    expect((await get("ont_nope")).status).toBe(404);
    Object.assign(config, { automationsDisabled: true });
    const off = await get("ont_acme_renewals");
    expect(off.body.details?.[0]?.reason).toBe("FOUNDRY_AUTOMATIONS_DISABLED");
  });
});

describe("the mock's route on a local install without an AI provider", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.resetModules();
    vi.useRealTimers();
  });

  it("answers the competency questions and never writes them", async () => {
    vi.resetModules();
    vi.stubEnv("MOCK_LOCAL_TOKEN", "local-token");
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-11-16T08:00:00Z"));
    const local = await import("../../mock/state"),
      { routes: localRoutes } = await import("../../mock/routes"),
      { aiSource } = await import("../../mock/routes/ai");
    local.resetState();
    Object.assign(local.config, { aiDefault: false });
    expect(aiSource("ws_acme")).toBe("none");
    const compiled = localRoutes.map((r) => ({
      ...r,
      c: compileTemplate(r.path),
    }));
    const path =
      "/v1/workspaces/ws_acme/foundry/automation-suggestions?ontologyId=ont_acme_renewals";
    const url = new URL(path, "http://mock");
    const r = compiled.find(
      (r) => r.method === "GET" && matchTemplate(r.c, url.pathname),
    )!;
    const get = async () =>
      (await r.handle({
        method: "GET",
        path: url.pathname,
        params: matchTemplate(r.c, url.pathname) as Record<string, string>,
        query: url.searchParams,
        body: undefined,
        principal: { user: local.db.users.get("usr_ada")!, sessionId: "s" },
      })) as MockReply & { body: any };
    for (let i = 0; i < 2; i++) {
      const got = await get();
      expect(got.body).toMatchObject({
        source: "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_QUESTIONS",
        generating: false,
      });
      expect(got.body.suggestions).toHaveLength(2);
      vi.setSystemTime(Date.now() + 5000);
    }
  });
});
