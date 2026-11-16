import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type {
  FoundryAutomationSuggestion,
  FoundryAutomationSuggestionKind,
  FoundryAutomationSuggestionSource,
  ListAutomationSuggestionsResponse,
} from "@/shared/wfe";
import { readSuggestions } from "@/web/areas/automations/model";
import {
  chosenKind,
  founderSuggestions,
  founderSuggestionsView,
  readFounderSuggestions,
  SUGGESTION_KINDS,
  type FounderSuggestion,
  type SuggestionKind,
} from "@/web/areas/ontology/founder-model";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import { messageIntent } from "../../mock/routes/agent";
import {
  modelSuggestions,
  strict,
} from "../../mock/routes/automation-suggestions";
import { config, db, resetState } from "../../mock/state";

// Founder's new chat's suggestions: the four kinds and their cards
// (founder-model.ts), read without trust; the mock's route, which answers
// as the backend's does (each kind filled to its count from its sources);
// and the mock Founder's routing of every suggestion it gives, so each
// kind's prompt does what its kind says.

const K = (k: string) =>
  `FOUNDRY_AUTOMATION_SUGGESTION_KIND_${k}` as FoundryAutomationSuggestionKind;
const S = (s: string) =>
  `FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_${s}` as FoundryAutomationSuggestionSource;
const wire = (
  kind: string,
  source: string,
  prompt: string,
  topic = "Topic",
): FoundryAutomationSuggestion =>
  ({
    title: prompt.slice(0, 20),
    prompt,
    topic,
    kind: K(kind),
    source: S(source),
  }) as FoundryAutomationSuggestion;
const kinds = (v: ReturnType<typeof founderSuggestionsView>) =>
  v.kind === "tabs" ? v.tabs.map((t) => t.kind) : [];
const base = {
  grantee: false,
  questions: [] as string[],
  name: "Renewals",
  versioned: true,
  pending: false,
  failed: false,
};

describe("readFounderSuggestions", () => {
  it("does as its example says", () => {
    expect(
      readFounderSuggestions({
        suggestions: [
          {
            title: "Unpaid",
            prompt: " Which invoices are unpaid? ",
            kind: K("QUESTION"),
            topic: "Revenue",
            source: S("MODEL"),
          },
          { title: "Brief", prompt: "Every Monday, email me a brief." },
          { title: "", prompt: "x", kind: K("DASHBOARD") },
        ],
      }),
    ).toEqual([
      {
        kind: "question",
        title: "Unpaid",
        prompt: "Which invoices are unpaid?",
        topic: "Revenue",
        source: "model",
      },
      {
        kind: "workflow",
        title: "Brief",
        prompt: "Every Monday, email me a brief.",
        topic: "Workflow",
        source: "fixed",
      },
    ]);
  });

  it("drops what isn't within the limits or of a known kind", () => {
    const got = readFounderSuggestions({
      suggestions: [
        { title: "x".repeat(33), prompt: "p", kind: K("QUESTION") },
        { title: "t", prompt: "x".repeat(401), kind: K("QUESTION") },
        { title: "t", prompt: "line\nbreak", kind: K("QUESTION") },
        { title: "t", prompt: "p", kind: "toString" as never },
        { title: "t", prompt: "p", kind: K("FUTURE") as never },
        null as never,
        {
          title: "t",
          prompt: "p",
          kind: K("UNSPECIFIED"),
          topic: "x".repeat(25),
          source: "constructor" as never,
        },
        { title: "é".repeat(32), prompt: "p", kind: K("ONTOLOGY") },
      ],
    });
    expect(got).toEqual([
      {
        kind: "workflow",
        title: "t",
        prompt: "p",
        topic: "Workflow",
        source: "fixed",
      },
      {
        kind: "ontology",
        title: "é".repeat(32),
        prompt: "p",
        topic: "Ontology",
        source: "fixed",
      },
    ]);
    expect(readFounderSuggestions(undefined)).toEqual([]);
    expect(readFounderSuggestions({ suggestions: "x" as never })).toEqual([]);
  });
});

describe("the fixed examples", () => {
  it("give more than each kind's cards, named for the ontology, each saying what routes it", () => {
    for (const versioned of [true, false]) {
      const all = founderSuggestions("Renewals", versioned);
      expect(all.map((x) => x.kind)).toEqual([
        ...Array(3).fill("question"),
        ...Array(3).fill("dashboard"),
        ...Array(3).fill("ontology"),
        ...Array(5).fill("workflow"),
      ]);
      for (const x of all) {
        expect(x.prompt).toContain("Renewals");
        expect(x.source).toBe("fixed");
        expect(x.topic).toBe(
          {
            question: "Question",
            dashboard: "Dashboard",
            ontology: "Ontology",
            workflow: "Workflow",
          }[x.kind],
        );
        expect([...x.title].length).toBeLessThanOrEqual(32);
        expect([...x.prompt].length).toBeLessThanOrEqual(400);
        if (x.kind === "dashboard") expect(x.prompt).toMatch(/\bdashboard\b/);
        if (x.kind === "ontology")
          expect(x.prompt).toMatch(/\b(draft|revise)\b/i);
      }
    }
    expect(
      founderSuggestions("Renewals", true).find((x) => x.kind === "dashboard")
        ?.prompt,
    ).toBe(
      "Build me a dashboard of Renewals at a glance: how many of each type it holds and what was added most recently.",
    );
    expect(
      founderSuggestions("Renewals", false).find((x) => x.kind === "ontology")
        ?.prompt,
    ).toBe("Help me connect my data and draft the first version of Renewals.");
  });

  it("cuts a long name and names a blank one", () => {
    const [long] = founderSuggestions("n".repeat(200), true);
    expect(long!.prompt).toContain(`${"n".repeat(99)}…`);
    expect(founderSuggestions("  ", true)[0]!.prompt).toContain(
      "this ontology",
    );
  });
});

describe("founderSuggestionsView", () => {
  const read = (
    suggestions: FoundryAutomationSuggestion[],
    generating = false,
  ): ListAutomationSuggestionsResponse => ({ suggestions, generating });
  const all = read([
    wire("QUESTION", "QUESTIONS", "Q1?"),
    wire("QUESTION", "QUESTIONS", "Q2?"),
    wire("DASHBOARD", "MODEL", "Build me a dashboard 1"),
    wire("DASHBOARD", "MODEL", "Build me a dashboard 2"),
    wire("ONTOLOGY", "SETUP", "Draft 1"),
    wire("ONTOLOGY", "SETUP", "Draft 2"),
    wire("WORKFLOW", "MODEL", "Every day 1"),
    wire("WORKFLOW", "MODEL", "Every day 2"),
    wire("WORKFLOW", "MODEL", "Every day 3"),
    wire("WORKFLOW", "MODEL", "Every day 4"),
  ]);

  it("gives a grantee the load's competency questions, else nothing", () => {
    expect(
      founderSuggestionsView({ ...base, grantee: true, data: all }),
    ).toEqual({ kind: "none" });
    const v = founderSuggestionsView({
      ...base,
      grantee: true,
      questions: ["Who renews?"],
    });
    expect(v).toEqual({
      kind: "questions",
      cards: [
        {
          kind: "question",
          title: "Who renews?",
          prompt: "Who renews?",
          topic: "Question",
          source: "questions",
        },
      ],
    });
  });

  it("is placeholders of every kind while first read", () => {
    expect(founderSuggestionsView({ ...base, pending: true })).toEqual({
      kind: "tabs",
      tabs: SUGGESTION_KINDS.map((kind) => ({ kind, cards: undefined })),
    });
  });

  it("is the fixed examples after a failed first read, without workflows when refused", () => {
    const failed = founderSuggestionsView({ ...base, failed: true });
    expect(kinds(failed)).toEqual(SUGGESTION_KINDS);
    expect(
      failed.kind === "tabs" && failed.tabs.every((t) => t.cards?.length === 2),
    ).toBe(true);
    const refused = founderSuggestionsView({
      ...base,
      failed: true,
      refused: true,
    });
    expect(kinds(refused)).toEqual(["question", "dashboard", "ontology"]);
    const first = founderSuggestionsView({
      ...base,
      versioned: false,
      failed: true,
    });
    expect(first.kind === "tabs" && first.tabs[2]!.cards![0]!.prompt).toContain(
      "connect my data",
    );
  });

  it("keeps what was read when a later read fails", () => {
    const v = founderSuggestionsView({ ...base, failed: true, data: all });
    expect(v.kind === "tabs" && v.tabs[0]!.cards![0]!.prompt).toBe("Q1?");
  });

  it("shows each kind's first 2, and no Workflows tab without workflows", () => {
    const v = founderSuggestionsView({ ...base, data: all });
    expect(kinds(v)).toEqual(SUGGESTION_KINDS);
    expect(
      v.kind === "tabs" && v.tabs.map((t) => t.cards!.map((x) => x.prompt)),
    ).toEqual([
      ["Q1?", "Q2?"],
      ["Build me a dashboard 1", "Build me a dashboard 2"],
      ["Draft 1", "Draft 2"],
      ["Every day 1", "Every day 2"],
    ]);
    const off = founderSuggestionsView({
      ...base,
      data: read(all.suggestions!.slice(0, 6)),
    });
    expect(kinds(off)).toEqual(["question", "dashboard", "ontology"]);
  });

  it("fills a question, dashboard or change the response lacks from the fixed examples", () => {
    // A WFE before kinds: workflows only.
    const v = founderSuggestionsView({
      ...base,
      data: read([{ title: "Brief", prompt: "Every Monday, a brief." }]),
    });
    expect(kinds(v)).toEqual(SUGGESTION_KINDS);
    const tabs = v.kind === "tabs" ? v.tabs : [];
    expect(tabs[0]!.cards!.every((x) => x.source === "fixed")).toBe(true);
    expect(tabs[3]!.cards!.map((x) => x.prompt)).toEqual([
      "Every Monday, a brief.",
    ]);
  });

  it("while Founder writes, shows only kinds whose cards are final", () => {
    const writing = read(
      [
        wire("QUESTION", "QUESTIONS", "Q1?"),
        wire("QUESTION", "QUESTIONS", "Q2?"),
        wire(
          "DASHBOARD",
          "QUESTIONS",
          "Build me a dashboard that answers: Q1?",
        ),
        wire("DASHBOARD", "FIXED", "Build me a dashboard 2"),
        wire("ONTOLOGY", "SETUP", "Draft 1"),
        wire("ONTOLOGY", "FIXED", "Draft 2"),
        wire("WORKFLOW", "QUESTIONS", "Every Monday, answer: Q1?"),
      ],
      true,
    );
    const v = founderSuggestionsView({ ...base, data: writing });
    expect(v.kind === "tabs" && v.tabs.map((t) => [t.kind, !!t.cards])).toEqual(
      [
        ["question", true],
        ["dashboard", false],
        ["ontology", false],
        ["workflow", false],
      ],
    );
    const setup = founderSuggestionsView({
      ...base,
      data: read(
        [
          wire("QUESTION", "FIXED", "Q?"),
          wire("ONTOLOGY", "SETUP", "Draft 1"),
          wire("ONTOLOGY", "SETUP", "Draft 2"),
        ],
        true,
      ),
    });
    expect(
      setup.kind === "tabs" && setup.tabs.map((t) => [t.kind, !!t.cards]),
    ).toEqual([
      ["question", false],
      ["dashboard", false],
      ["ontology", true],
    ]);
  });
});

describe("chosenKind", () => {
  it("does as its examples say, whatever was stored", () => {
    expect(chosenKind("dashboard", ["question", "dashboard"])).toBe(
      "dashboard",
    );
    expect(chosenKind("workflow", ["question", "dashboard"])).toBe("question");
    expect(chosenKind("toString", ["question"])).toBe("question");
    expect(chosenKind("", ["dashboard"])).toBe("dashboard");
    expect(chosenKind("QUESTION", ["question", "ontology"])).toBe("question");
    expect(chosenKind("question", [])).toBeUndefined();
  });
});

describe("the new workflow page reads only workflows", () => {
  it("keeps kind WORKFLOW and none, and leaves the other kinds and the fixed ones", () => {
    expect(
      readSuggestions({
        suggestions: [
          wire("QUESTION", "QUESTIONS", "Q?"),
          wire("DASHBOARD", "MODEL", "D"),
          wire("ONTOLOGY", "SETUP", "O"),
          { title: "Old", prompt: "Every day, old." },
          { ...wire("WORKFLOW", "FIXED", "Fixed"), title: "Fixed" },
          { ...wire("WORKFLOW", "MODEL", "W1"), title: "W1" },
          { ...wire("UNSPECIFIED", "MODEL", "W2"), title: "W2" },
          { ...wire("WORKFLOW", "MODEL", "W3"), title: "W3" },
          { ...wire("WORKFLOW", "MODEL", "W4"), title: "W4" },
        ],
      }).map((x) => x.prompt),
    ).toEqual(["Every day, old.", "W1", "W2", "W3"]);
  });
});

// ---------------------------------------------------------------- the mock

const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));
async function call(
  method: "GET" | "POST",
  path: string,
  body?: unknown,
): Promise<MockReply & { body: any }> {
  const url = new URL(path, "http://mock");
  const r = compiled.find(
    (r) => r.method === method && matchTemplate(r.c, url.pathname),
  )!;
  return (await r.handle({
    method,
    path: url.pathname,
    params: matchTemplate(r.c, url.pathname) as Record<string, string>,
    query: url.searchParams,
    body,
    principal: { user: db.users.get("usr_ada")!, sessionId: "s" },
  })) as MockReply & { body: any };
}
const get = async (ontologyId: string) =>
  (
    await call(
      "GET",
      `/v1/workspaces/ws_acme/foundry/automation-suggestions?ontologyId=${ontologyId}`,
    )
  ).body as ListAutomationSuggestionsResponse;
const later = (ms: number) => vi.setSystemTime(Date.now() + ms);
const SHORT: Record<string, SuggestionKind> = {
  [K("QUESTION")]: "question",
  [K("DASHBOARD")]: "dashboard",
  [K("ONTOLOGY")]: "ontology",
  [K("WORKFLOW")]: "workflow",
};
/** Each suggestion as kind, source and prompt. */
const rows = (r: ListAutomationSuggestionsResponse) =>
  (r.suggestions || []).map((x) => ({
    kind: SHORT[x.kind!]!,
    source: x.source!.replace(S(""), "").toLowerCase(),
    prompt: x.prompt!,
    topic: x.topic!,
  }));
const countOf = (r: ListAutomationSuggestionsResponse) =>
  rows(r).map((x) => x.kind);
const ONTOLOGIES = [
  "ont_acme_contracts",
  "ont_acme_renewals",
  "ont_acme_sales",
] as const;
const FULL = [
  "question",
  "question",
  "dashboard",
  "dashboard",
  "ontology",
  "ontology",
  "workflow",
  "workflow",
  "workflow",
  "workflow",
];

async function newOntology(name = "Empty") {
  const made = await call("POST", "/v1/workspaces/ws_acme/foundry/ontologies", {
    name,
    tenantId: "ten_acme_sandbox",
    clientToken: name,
  });
  return made.body.ontology.id as string;
}

describe("the mock's route", () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-11-16T08:00:00Z"));
    resetState();
  });
  afterEach(() => vi.useRealTimers());

  it("gives every kind its count, in order, while Founder writes and after", async () => {
    for (const o of ONTOLOGIES) {
      const first = await get(o);
      expect(countOf(first)).toEqual(FULL);
      expect(first.generating).toBe(true);
    }
    later(1500);
    for (const o of ONTOLOGIES) {
      const ready = await get(o);
      expect(countOf(ready)).toEqual(FULL);
      expect(ready.generating).toBe(false);
      for (const x of ready.suggestions!) {
        expect(strict(x)).toBe(true);
        expect(
          x.icon === "FOUNDRY_AUTOMATION_SUGGESTION_ICON_UNSPECIFIED",
        ).toBe(x.kind !== K("WORKFLOW"));
      }
    }
  });

  it("asks the competency questions first, then dashboards of the later ones, then Founder's", async () => {
    const writing = rows(await get("ont_acme_renewals"));
    expect(writing.filter((x) => x.kind === "question")).toEqual([
      {
        kind: "question",
        source: "questions",
        prompt:
          "Which customers are under contract, and which contracts are they signed with?",
        topic: "Question",
      },
      {
        kind: "question",
        source: "questions",
        prompt: "What is each customer under contract billed?",
        topic: "Question",
      },
    ]);
    // Only two questions: the dashboards wrap round to them.
    expect(
      writing.filter((x) => x.kind === "dashboard").map((x) => x.prompt),
    ).toEqual([
      "Build me a dashboard that answers: Which customers are under contract, and which contracts are they signed with?",
      "Build me a dashboard that answers: What is each customer under contract billed?",
    ]);
    expect(writing.find((x) => x.kind === "workflow")).toMatchObject({
      source: "questions",
      prompt: expect.stringMatching(/^Every Monday, answer: /),
    });
    later(1500);
    const ready = await get("ont_acme_renewals");
    expect(ready.source).toBe(S("MODEL"));
    const by = (k: SuggestionKind) => rows(ready).filter((x) => x.kind === k);
    // Questions keep the competency questions; the rest are Founder's.
    expect(by("question").map((x) => x.source)).toEqual([
      "questions",
      "questions",
    ]);
    expect(by("dashboard").map((x) => x.source)).toEqual(["model", "model"]);
    expect(by("workflow").map((x) => x.topic)).toEqual([
      "Renewals",
      "Data quality",
      "Revenue",
      "Briefing",
    ]);
  });

  it("suggests the links the audit found missing first, by support", async () => {
    // Once Customer contracts has a load, its audit gives the findings.
    Object.assign(config, { loadStepMs: 50 });
    const load = await call(
      "POST",
      "/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts/loads",
      { versionId: "ver_seed_3", clientToken: "l" },
    );
    expect(load.status).toBe(200);
    later(10_000);
    await call(
      "GET",
      `/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts/loads/${load.body.load.id}`,
    );
    const r = rows(await get("ont_acme_contracts")).filter(
      (x) => x.kind === "ontology",
    );
    expect(r).toEqual([
      {
        kind: "ontology",
        source: "setup",
        prompt:
          "Draft a new version of Customer contracts with a link for “supplier of”, which 12 claims in your documents state.",
        topic: "Missing link",
      },
      {
        kind: "ontology",
        source: "setup",
        prompt:
          "Draft a new version of Customer contracts with a link for “subsidiary of”, which 9 claims in your documents state.",
        topic: "Missing link",
      },
    ]);
    expect(messageIntent("ws_acme", r[0]!.prompt, "ont_acme_contracts")).toBe(
      "generate_ontology",
    );
    // Renewals has no load to audit: the connection its version doesn't
    // read, then the fixed change.
    expect(
      rows(await get("ont_acme_renewals"))
        .filter((x) => x.kind === "ontology")
        .map((x) => [x.source, x.topic]),
    ).toEqual([
      ["setup", "New source"],
      ["fixed", "Ontology"],
    ]);
  });

  it("suggests the connections a version doesn't read once the audit gives none", async () => {
    Object.assign(config, { healEnabled: false });
    const r = rows(await get("ont_acme_contracts")).filter(
      (x) => x.kind === "ontology",
    );
    // Its version's generation read Stripe, not the Shop database; General
    // holds no documents yet.
    expect(r).toEqual([
      {
        kind: "ontology",
        source: "setup",
        prompt:
          "Draft a new version of Customer contracts that also reads Shop database.",
        topic: "New source",
      },
      {
        kind: "ontology",
        source: "fixed",
        prompt:
          "Draft a new version of Customer contracts that adds the properties people ask about most but it doesn't hold yet.",
        topic: "Ontology",
      },
    ]);
  });

  it("fills from the fallbacks where Founder can't write", async () => {
    for (const knob of [
      { automationSuggestionsOutcome: "fail" },
      { automationSuggestionsAi: false },
      { agentEnabled: false },
    ]) {
      resetState();
      Object.assign(config, knob, { automationSuggestionsMs: 0 });
      const r = await get("ont_acme_contracts");
      expect(r.generating).toBe(false);
      expect(countOf(r)).toEqual(FULL);
      // Customer contracts' version has no competency questions: fixed.
      expect(
        rows(r)
          .filter((x) => x.kind !== "ontology")
          .every((x) => x.source === "fixed"),
      ).toBe(true);
      expect(r.source).toBe(S("FIXED"));
    }
  });

  it("gives the fixed examples without a current version, never generating", async () => {
    const id = await newOntology();
    for (let i = 0; i < 2; i++) {
      const r = await get(id);
      expect(r).toMatchObject({ generating: false, versionId: "" });
      expect(countOf(r)).toEqual(FULL);
      expect(rows(r).every((x) => x.source === "fixed")).toBe(true);
      expect(rows(r)[4]!.prompt).toBe(
        "Help me connect my data and draft the first version of Empty.",
      );
      later(5000);
    }
  });

  it("leaves workflows out while they are off, and refuses only what members' routes refuse", async () => {
    Object.assign(config, { automationsDisabled: true });
    const r = await get("ont_acme_renewals");
    expect(countOf(r)).toEqual(FULL.slice(0, 6));
    expect(r.source).toBe(S("UNSPECIFIED"));
    const missing = await call(
      "GET",
      "/v1/workspaces/ws_acme/foundry/automation-suggestions",
    );
    expect(missing.status).toBe(400);
    const gone = await call(
      "GET",
      "/v1/workspaces/ws_acme/foundry/automation-suggestions?ontologyId=ont_nope",
    );
    expect(gone.status).toBe(404);
  });

  it("checks every prompt strictly", () => {
    const ok = {
      kind: K("QUESTION"),
      title: "Unpaid",
      prompt: "Which invoices are unpaid?",
      topic: "Revenue",
    };
    expect(strict(ok)).toBe(true);
    for (const prompt of [
      "Open https://example.com",
      "Read www.example.com",
      "Look at ont_acme_contracts",
      "Use <b>bold</b>",
      "A [link](x)",
      "A `code` span",
      "Some **bold**",
      "# A heading",
      "Look at src_1a2b3c4d",
      "Two\nlines",
      "An invisible\u200bspace",
      "x".repeat(401),
      "",
    ])
      expect(strict({ ...ok, prompt }), prompt).toBe(false);
    // Whitespace is collapsed, as the backend cleans it.
    expect(strict({ ...ok, prompt: " Which  invoices? " })).toBe(true);
    // Each kind says what routes it; a workflow has an icon.
    expect(strict({ ...ok, kind: K("DASHBOARD") })).toBe(false);
    expect(
      strict({ ...ok, kind: K("DASHBOARD"), prompt: "Build me a dashboard." }),
    ).toBe(true);
    expect(strict({ ...ok, kind: K("ONTOLOGY") })).toBe(false);
    expect(
      strict({ ...ok, kind: K("ONTOLOGY"), prompt: "Revise it, please." }),
    ).toBe(true);
    expect(strict({ ...ok, kind: K("WORKFLOW") })).toBe(false);
    expect(
      strict({
        ...ok,
        kind: K("WORKFLOW"),
        icon: "FOUNDRY_AUTOMATION_SUGGESTION_ICON_REPORT",
      }),
    ).toBe(true);
    expect(strict({ ...ok, topic: "x".repeat(25) })).toBe(false);
    expect(strict({ ...ok, title: "x".repeat(33) })).toBe(false);
  });
});

describe("the mock Founder's routing of every suggestion", () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-11-16T08:00:00Z"));
    resetState();
  });
  afterEach(() => vi.useRealTimers());

  /** What each kind's prompt may route to in the mock's Founder. */
  const ROUTES: Record<SuggestionKind, readonly string[]> = {
    question: ["ask"],
    dashboard: ["create_dashboard", "ask_dashboard"],
    ontology: ["generate_ontology", "revise_version"],
    workflow: ["create_workflow"],
  };
  const routed = (o: string, x: Pick<FounderSuggestion, "kind" | "prompt">) => {
    const intent = messageIntent("ws_acme", x.prompt, o);
    expect
      .soft(ROUTES[x.kind], `${x.kind} “${x.prompt}” went to ${intent}`)
      .toContain(intent);
  };

  it("sends each kind's to what its kind does, Founder's and the fallbacks'", async () => {
    const seen = new Set<string>();
    for (const knobs of [
      {},
      { automationSuggestionsOutcome: "fail" },
      { healEnabled: false },
    ]) {
      resetState();
      Object.assign(config, knobs, { automationSuggestionsMs: 0 });
      for (const o of ONTOLOGIES)
        for (const x of rows(await get(o))) {
          routed(o, x);
          seen.add(`${x.kind}/${x.source}`);
        }
    }
    expect([...seen].sort()).toEqual(
      expect.arrayContaining([
        "dashboard/fixed",
        "dashboard/model",
        "dashboard/questions",
        "ontology/setup",
        "question/model",
        "question/questions",
        "workflow/model",
        "workflow/questions",
      ]),
    );
  });

  it("sends every one the mock's model writes to what its kind does", () => {
    for (const [o, types] of [
      ["ont_acme_renewals", ["Contract", "Invoice"]],
      ["ont_acme_sales", ["Person"]],
    ] as const) {
      const name = o === "ont_acme_sales" ? "Sales graph" : "Renewals";
      const made = modelSuggestions(name, types)!;
      expect(made.map((x) => SHORT[x.kind!])).toEqual(FULL);
      for (const x of made)
        routed(o, { kind: SHORT[x.kind!]!, prompt: x.prompt! });
    }
    expect(modelSuggestions("Orders", ["Order"])).toBeUndefined();
  });

  it("sends every fixed example to what its kind does", () => {
    for (const o of ONTOLOGIES) {
      const name = {
        ont_acme_contracts: "Customer contracts",
        ont_acme_renewals: "Renewals",
        ont_acme_sales: "Sales graph",
      }[o];
      for (const x of founderSuggestions(name, true)) routed(o, x);
      // Without a version, the first change connects data.
      const [connect, ...first] = founderSuggestions(name, false).filter(
        (x) => x.kind === "ontology",
      );
      expect(messageIntent("ws_acme", connect!.prompt, o)).toBe("connect");
      for (const x of first) routed(o, x);
    }
  });
});
