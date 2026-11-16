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
    ["What is the total revenue?", "What is the total revenue"],
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
      "What is the total revenue",
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
  async function get(
    ontologyId: string,
    userId = "usr_ada",
    ws = "ws_acme",
  ): Promise<MockReply & { body: any }> {
    const url = new URL(
      `/v1/workspaces/${ws}/foundry/automation-suggestions?ontologyId=${ontologyId}`,
      "http://mock",
    );
    const r = compiled.find(
      (r) => r.method === "GET" && matchTemplate(r.c, url.pathname),
    )!;
    return (await r.handle({
      method: "GET",
      path: url.pathname,
      params: matchTemplate(r.c, url.pathname) as Record<string, string>,
      query: url.searchParams,
      body: undefined,
      principal: { user: db.users.get(userId)!, sessionId: "s" },
    })) as MockReply & { body: any };
  }
  const later = (ms: number) => vi.setSystemTime(Date.now() + ms);
  const titles = (r: { body: any }) =>
    (r.body.suggestions || []).map((x: { title: string }) => x.title);

  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-11-16T08:00:00Z"));
    resetState();
  });
  afterEach(() => vi.useRealTimers());

  it("answers the competency questions while it writes, then the Assistant's", async () => {
    const first = await get("ont_acme_renewals");
    expect(first.body).toMatchObject({
      source: "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_QUESTIONS",
      generating: true,
      versionId: "ver_renewals_1",
    });
    expect(titles(first)).toEqual([
      "Customers are under contract",
      "What is each customer under",
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
