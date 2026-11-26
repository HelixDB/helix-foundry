import { describe, expect, it } from "vitest";
import type {
  Connector,
  FoundryCompanyResearch,
  FoundryDocument,
  FoundryDocumentSet,
} from "@/shared/wfe";
import {
  DOCUMENTS_PER_SET,
  MAX_GENERATION_SETS,
  MAX_GOAL,
  MAX_TABLES,
  companyDomain,
  companyFlow,
  databasePlan,
  firstOntologyName,
  registrableDomain,
  generationSources,
  newDatabase,
  newOntologyWizard,
  setSourcesText,
  type CompanyChoice,
  correctedSummary,
  type CompanyFlow,
  type CompanyFlowAction,
  type WizardDraft,
  type WizardFacts,
} from "@/web/areas/ontology/model";

const synced = (id: string, name: string, tables = 1): Connector => ({
  id,
  kind: "stripe",
  name,
  tables: Array.from({ length: tables }, (_, i) => ({ name: `t${i}` })),
  lastSync: { id: "s" + id, status: "CONNECTOR_SYNC_STATUS_SUCCEEDED" },
});
const firstSync = (
  id: string,
  name: string,
  status: "QUEUED" | "RUNNING" | "FAILED",
): Connector => ({
  id,
  kind: "postgres",
  name,
  lastSync: { id: "s" + id, status: `CONNECTOR_SYNC_STATUS_${status}` },
});
const set = (id: string, name: string): FoundryDocumentSet => ({ id, name });
const doc = (
  id: string,
  setId: string,
  mediaType: string,
  state?: "READY" | "READING" | "FAILED",
): FoundryDocument => ({
  id,
  setId,
  mediaType,
  status: "uploaded",
  ...(state ? { read: { state: `FOUNDRY_FILE_READ_STATE_${state}` } } : {}),
});

const draft: WizardDraft = {
  company: null,
  draftedName: "",
  name: "",
  database: null,
  connectorIds: null,
  eventSources: new Map(),
  setIds: new Set(),
  goal: "",
};
const facts: WizardFacts = {
  databases: [
    { tenantId: "t1", projectId: "p1", project: "Production", name: "orders" },
    {
      tenantId: "t2",
      projectId: "p1",
      project: "Production",
      name: "contracts",
      usedBy: "Customer contracts",
    },
  ],
  projects: [{ id: "p1", slug: "production", displayName: "Production" }],
  connectors: [synced("c1", "Stripe")],
  sets: [],
  documents: [],
};
const wiz = (d: WizardDraft, f: WizardFacts, step: string | null) =>
  newOntologyWizard(d, f, "Acme", step);
const state = (
  d: Partial<WizardDraft>,
  f: Partial<WizardFacts> = {},
  id = "review",
) =>
  wiz({ ...draft, ...d }, { ...facts, ...f }, "review").steps.find(
    (s) => s.id === id,
  )!.state;
const overCap = (n: number) =>
  `These sources hold ${n} tables and one draft reads at most ${MAX_TABLES}, table files included. Untick connectors or sets until it’s ${MAX_TABLES} or fewer.`;
// Globex's research, its questions answered by their guesses.
const globex: CompanyChoice = {
  research: {
    id: "res1",
    domain: "globex.com",
    companyName: "Globex",
    suggestedName: "Globex operations",
    questions: [
      { id: "q1", question: "Who asks?", bestGuess: "Ops" },
      { id: "q2", question: "What matters?", bestGuess: "" },
    ],
    fetched: true,
  },
  summary: "",
  answers: new Map([
    ["q1", "Ops"],
    ["q2", " "],
  ]),
};

describe("newOntologyWizard", () => {
  it("follows its @example", () => {
    const example: WizardFacts = {
      databases: [],
      projects: [],
      connectors: [
        {
          id: "c1",
          kind: "stripe",
          name: "Stripe",
          tables: [{ name: "charges" }],
        },
      ],
      sets: [],
      documents: [],
    };
    expect(newOntologyWizard(draft, example, "Acme", null).current).toBe(
      "company",
    );
    expect(
      newOntologyWizard(draft, example, "Acme", "review").steps.map(
        (s) => s.state,
      ),
    ).toEqual([
      { kind: "skipped" },
      { kind: "ready", summary: "Stripe: 1 table" },
      { kind: "skipped" },
      { kind: "ready", summary: "" },
    ]);
    expect(newOntologyWizard(draft, example, "Acme", "review").request).toEqual(
      {
        name: "Stripe",
        database: {
          kind: "new",
          project: { kind: "new", name: "Acme" },
          name: "Stripe",
        },
        generate: { connectorIds: ["c1"], documentSetIds: [] },
      },
    );
    expect(
      newOntologyWizard(
        { ...draft, goal: "Which charges fail? By card." },
        example,
        "Acme",
        "review",
      ).derivedName,
    ).toBe("Which charges fail?");
  });

  it("lists the steps in order, with the optional ones marked", () => {
    const w = wiz(draft, facts, null);
    expect(w.steps.map((s) => [s.id, s.optional])).toEqual([
      ["company", true],
      ["sources", false],
      ["goal", true],
      ["review", false],
    ]);
  });

  it("shows the step asked for, the first one when it is missing or unknown", () => {
    expect(wiz(draft, facts, null).current).toBe("company");
    expect(wiz(draft, facts, "nope").current).toBe("company");
    // Steps no longer in it start at the first.
    expect(wiz(draft, facts, "name").current).toBe("company");
    expect(wiz(draft, facts, "database").current).toBe("company");
    expect(wiz(draft, facts, "goal").current).toBe("goal");
    // Asking for a step never needs the steps after it.
    expect(wiz(draft, { ...facts, connectors: [] }, "sources").current).toBe(
      "sources",
    );
  });

  it("stops at the first blocked step before the one asked for", () => {
    expect(wiz(draft, { ...facts, connectors: [] }, "review").current).toBe(
      "sources",
    );
    expect(
      wiz({ ...draft, goal: "x".repeat(MAX_GOAL + 1) }, facts, "review")
        .current,
    ).toBe("goal");
  });

  it("lets a loading step pass, and sends nothing until it loads", () => {
    const w = wiz(draft, { connectors: undefined }, "review");
    expect(w.current).toBe("review");
    expect(w.steps.find((s) => s.id === "sources")!.state).toEqual({
      kind: "loading",
    });
    expect(w.steps.at(-1)!.state).toEqual({ kind: "loading" });
    expect(w.request).toBeNull();
  });

  it("goes back and on one step at a time, stopping at either end", () => {
    const at = (step: string) => wiz(draft, facts, step);
    expect([at("company").back, at("company").next]).toEqual([null, "sources"]);
    expect([at("sources").back, at("sources").next]).toEqual([
      "company",
      "goal",
    ]);
    expect([at("review").back, at("review").next]).toEqual(["goal", null]);
  });

  it("skips the company until its site is researched, then counts the answers", () => {
    expect(state({}, {}, "company")).toEqual({ kind: "skipped" });
    // A blank answer is no answer.
    expect(state({ company: globex }, {}, "company")).toEqual({
      kind: "ready",
      summary: "Globex: 1 answer",
    });
    expect(
      state(
        {
          company: {
            research: { id: "r", domain: "initech.com" },
            summary: "",
            answers: new Map(),
          },
        },
        {},
        "company",
      ),
    ).toEqual({ kind: "ready", summary: "initech.com: 0 answers" });
    // Researched or not, the review goes on.
    expect(state({ company: globex })).toEqual({ kind: "ready", summary: "" });
  });

  it("needs a synced connector, every one by default", () => {
    const two = [synced("c1", "Stripe", 3), synced("c2", "Shop", 5)];
    expect(state({}, { connectors: two }, "sources")).toEqual({
      kind: "ready",
      summary: "Stripe and Shop: 8 tables",
    });
    expect(
      state({ connectorIds: new Set(["c2"]) }, { connectors: two }, "sources"),
    ).toEqual({ kind: "ready", summary: "Shop: 5 tables" });
    // A chosen connector that is gone, or none, chooses nothing.
    expect(
      state(
        { connectorIds: new Set(["gone"]) },
        { connectors: two },
        "sources",
      ),
    ).toEqual({
      kind: "blocked",
      reason:
        "Choose a synced connector, or a set holding documents Foundry reads.",
    });
    expect(state({}, { connectors: [] }, "sources")).toEqual({
      kind: "blocked",
      reason: "Connect your data or upload documents to continue.",
    });
    expect(state({}, { connectors: undefined }, "sources")).toEqual({
      kind: "loading",
    });
  });

  it("waits for a chosen connector's first sync", () => {
    const queued = firstSync("c3", "Warehouse", "QUEUED"),
      running = firstSync("c3", "Warehouse", "RUNNING"),
      failed = firstSync("c3", "Warehouse", "FAILED");
    const wait = {
      kind: "blocked",
      reason: "Syncing… you can continue once it’s synced.",
    };
    expect(state({}, { connectors: [queued] }, "sources")).toEqual(wait);
    expect(state({}, { connectors: [running] }, "sources")).toEqual(wait);
    // A failed one isn't waited for.
    expect(state({}, { connectors: [failed] }, "sources")).toEqual({
      kind: "blocked",
      reason: "Connect your data or upload documents to continue.",
    });
    // Not chosen, it isn't waited for either.
    expect(
      state(
        { connectorIds: new Set(["c1"]) },
        { connectors: [synced("c1", "Stripe"), running] },
        "sources",
      ),
    ).toEqual({ kind: "ready", summary: "Stripe: 1 table" });
    // With a synced one chosen too, the step is ready, and says it.
    const w = wiz(
      { ...draft, connectorIds: new Set(["c1", "c3"]) },
      { ...facts, connectors: [synced("c1", "Stripe"), running] },
      "review",
    );
    expect(w.steps[1]!.state).toEqual({
      kind: "ready",
      summary: "Stripe: 1 table",
      note: "Warehouse still syncing.",
    });
    // Only the synced one is sent.
    expect(w.request!.generate.connectorIds).toEqual(["c1"]);
  });

  it("caps the tables one generation drafts from", () => {
    const wide = [synced("c1", "Stripe", MAX_TABLES), synced("c2", "Shop", 1)];
    expect(state({}, { connectors: wide }, "sources")).toEqual({
      kind: "blocked",
      reason: overCap(MAX_TABLES + 1),
    });
    expect(
      state({ connectorIds: new Set(["c1"]) }, { connectors: wide }, "sources"),
    ).toEqual({ kind: "ready", summary: `Stripe: ${MAX_TABLES} tables` });
  });

  it("names a few connectors, then counts the rest", () => {
    const many = ["A", "B", "C", "D", "E"].map((n) => synced(n, n));
    expect(state({}, { connectors: many }, "sources")).toEqual({
      kind: "ready",
      summary: "A, B, C, and 2 more: 5 tables",
    });
    // A connector without a name goes by its kind.
    expect(
      state({}, { connectors: [{ ...synced("c1", ""), name: "" }] }, "sources"),
    ).toEqual({ kind: "ready", summary: "stripe: 1 table" });
  });

  it("takes document sets alongside connectors, up to the cap, dropping sets that are gone", () => {
    const sets = [set("g", "General"), set("b", "Board")],
      documents = [
        doc("d1", "g", "text/markdown"),
        doc("d2", "b", "text/plain"),
      ];
    expect(state({}, { sets, documents }, "sources")).toEqual({
      kind: "ready",
      summary: "Stripe: 1 table",
    });
    expect(
      state(
        { setIds: new Set(["b", "g", "gone"]) },
        { sets, documents },
        "sources",
      ),
    ).toEqual({
      kind: "ready",
      summary: "Stripe: 1 table; General and Board: 2 documents",
    });
    // A chosen set holding nothing Foundry reads is still named.
    expect(
      state({ setIds: new Set(["g"]) }, { sets, documents: [] }, "sources"),
    ).toEqual({ kind: "ready", summary: "Stripe: 1 table; General" });
    expect(state({}, { sets: undefined }, "sources")).toEqual({
      kind: "loading",
    });
    expect(state({}, { documents: undefined }, "sources")).toEqual({
      kind: "loading",
    });
    const lots = Array.from({ length: MAX_GENERATION_SETS + 1 }, (_, i) =>
      set("s" + i, "Set " + i),
    );
    expect(
      state(
        { setIds: new Set(lots.map((s) => s.id)) },
        { sets: lots },
        "sources",
      ),
    ).toEqual({
      kind: "blocked",
      reason: `A generation reads at most ${MAX_GENERATION_SETS} document sets. Uncheck some.`,
    });
  });

  it("generates from files alone, sending no connectors", () => {
    const sets = [set("g", "General")];
    const only = (documents: FoundryDocument[], connectors: Connector[] = []) =>
      wiz(
        { ...draft, setIds: new Set(["g"]) },
        { ...facts, connectors, sets, documents },
        "review",
      );
    // A table file that has been read.
    const csv = only([doc("f", "g", "text/csv", "READY")]);
    expect(csv.steps[1]!.state).toEqual({
      kind: "ready",
      summary: "General: 1 table file",
    });
    expect(csv.request!.generate).toEqual({
      noConnectors: true,
      documentSetIds: ["g"],
    });
    // Text alone.
    expect(only([doc("m", "g", "text/mdx")]).steps[1]!.state).toEqual({
      kind: "ready",
      summary: "General: 1 document",
    });
    // Both, counted apart.
    expect(
      only([
        doc("f", "g", "text/csv", "READY"),
        doc("m", "g", "text/markdown"),
        doc("n", "g", "text/markdown"),
      ]).steps[1]!.state,
    ).toEqual({
      kind: "ready",
      summary: "General: 1 table file and 2 documents",
    });
    // A connector not chosen leaves files alone, still sending no connectors.
    expect(
      wiz(
        { ...draft, connectorIds: new Set(), setIds: new Set(["g"]) },
        { ...facts, sets, documents: [doc("m", "g", "text/plain")] },
        "review",
      ).request!.generate,
    ).toEqual({ noConnectors: true, documentSetIds: ["g"] });
    // A PDF whose text is read is a document like any text.
    expect(
      only([doc("p", "g", "application/pdf", "READY")]).steps[1]!.state,
    ).toEqual({ kind: "ready", summary: "General: 1 document" });
    // A PDF whose text couldn't be read (a scanned one), or with no read,
    // a file that couldn't be read, or an upload that didn't finish is
    // nothing to draft from.
    for (const documents of [
      [doc("p", "g", "application/pdf")],
      [doc("p", "g", "application/pdf", "FAILED")],
      [doc("f", "g", "text/csv", "FAILED")],
      [
        {
          ...doc("m", "g", "text/markdown"),
          status: "pending_upload" as const,
        },
      ],
    ])
      expect(only(documents).steps[1]!.state).toEqual({
        kind: "blocked",
        reason:
          "Choose a synced connector, or a set holding documents Foundry reads.",
      });
    // Files in a set not chosen don't count.
    expect(
      wiz(
        draft,
        {
          ...facts,
          connectors: [],
          sets,
          documents: [doc("m", "g", "text/plain")],
        },
        "review",
      ).steps[1]!.state,
    ).toEqual({
      kind: "blocked",
      reason:
        "Choose a synced connector, or a set holding documents Foundry reads.",
    });
  });

  it("generates from a connection's set, counted by the set", () => {
    // Its documents are never in the uploads list: the set says how many.
    const notes: FoundryDocumentSet = {
        id: "n",
        name: "Notes",
        documents: 418,
        connector: {
          connectorId: "cg",
          stream: "notes",
          connectorName: "Granola",
        },
      },
      sets = [set("g", "General"), notes];
    const w = (setIds: string[], documents: FoundryDocument[] = []) =>
      wiz(
        { ...draft, connectorIds: new Set(), setIds: new Set(setIds) },
        { ...facts, sets, documents },
        "review",
      );
    expect(w(["n"]).steps[1]!.state).toEqual({
      kind: "ready",
      summary: "Granola / Notes: 418 documents",
    });
    expect(w(["n"]).request!.generate).toEqual({
      noConnectors: true,
      documentSetIds: ["n"],
    });
    // With the uploads of a set people made, counted together.
    expect(
      w(["g", "n"], [doc("m", "g", "text/markdown")]).steps[1]!.state,
    ).toEqual({
      kind: "ready",
      summary: "General and Granola / Notes: 419 documents",
    });
    // Empty until its first sync writes it: nothing to draft from.
    expect(
      wiz(
        { ...draft, connectorIds: new Set(), setIds: new Set(["n"]) },
        { ...facts, sets: [{ ...notes, documents: 0 }], documents: [] },
        "review",
      ).steps[1]!.state,
    ).toEqual({
      kind: "blocked",
      reason:
        "Choose a synced connector, or a set holding documents Foundry reads.",
    });
  });

  it("says files still being read, and waits for them while nothing else is ready", () => {
    const sets = [set("g", "General")];
    const reading = doc("r", "g", "text/csv", "READING");
    const w = (documents: FoundryDocument[], connectors: Connector[] = []) =>
      wiz(
        { ...draft, setIds: new Set(["g"]) },
        { ...facts, connectors, sets, documents },
        "review",
      ).steps[1]!.state;
    expect(w([reading])).toEqual({
      kind: "blocked",
      reason: "Reading its files… you can continue once they’re read.",
    });
    // A PDF's text, the same.
    expect(w([doc("p", "g", "application/pdf", "READING")])).toEqual({
      kind: "blocked",
      reason: "Reading its files… you can continue once they’re read.",
    });
    expect(w([reading, doc("f", "g", "text/csv", "READY")])).toEqual({
      kind: "ready",
      summary: "General: 1 table file",
      note: "1 file still being read: a generation started now leaves it out.",
    });
    expect(
      w(
        [reading, { ...reading, id: "r2" }],
        [synced("c1", "Stripe"), firstSync("c3", "Warehouse", "RUNNING")],
      ),
    ).toEqual({
      kind: "ready",
      summary: "Stripe: 1 table; General",
      note: "Warehouse still syncing; 2 files still being read: a generation started now leaves them out.",
    });
  });

  it("sums up what a set gives a generation", () => {
    expect(
      setSourcesText([
        doc("a", "g", "text/markdown"),
        doc("b", "g", "text/csv", "READING"),
      ]),
    ).toBe("1 document · 1 file still being read");
    // A PDF is a document once its text is read; until then a file being
    // read; a scanned one a file that couldn't be read.
    expect(
      setSourcesText([
        doc("a", "g", "application/pdf", "READY"),
        doc("b", "g", "application/pdf", "READING"),
        doc("c", "g", "application/pdf", "FAILED"),
        doc("d", "g", "text/csv", "READY"),
        doc("e", "g", "text/csv", "FAILED"),
      ]),
    ).toBe(
      "1 document · 1 table file · 1 file still being read · 2 files that couldn’t be read",
    );
    expect(
      setSourcesText(
        Array.from({ length: DOCUMENTS_PER_SET + 1 }, (_, i) =>
          doc(`m${i}`, "g", "text/plain"),
        ),
      ),
    ).toBe(
      `${DOCUMENTS_PER_SET + 1} documents; the agent reads the ${DOCUMENTS_PER_SET} newest`,
    );
    expect(
      setSourcesText([{ ...doc("a", "g", "text/plain"), status: "deleted" }]),
    ).toBe("Nothing Foundry reads yet");
  });

  it("counts table files toward the tables one generation drafts from", () => {
    const sets = [set("g", "General")];
    expect(
      wiz(
        { ...draft, setIds: new Set(["g"]) },
        {
          ...facts,
          connectors: [synced("c1", "Stripe", MAX_TABLES)],
          sets,
          documents: [doc("f", "g", "text/csv", "READY")],
        },
        "review",
      ).steps[1]!.state,
    ).toEqual({
      kind: "blocked",
      reason: overCap(MAX_TABLES + 1),
    });
  });

  it("takes a goal optionally, up to its length in characters", () => {
    expect(state({ goal: "  " }, {}, "goal")).toEqual({ kind: "skipped" });
    expect(
      state(
        { goal: "Which orders ship late? Group them by carrier." },
        {},
        "goal",
      ),
    ).toEqual({ kind: "ready", summary: "Which orders ship late?" });
    // Characters, not UTF-16 units: an emoji is one.
    expect(state({ goal: "😀".repeat(MAX_GOAL) }, {}, "goal").kind).toBe(
      "ready",
    );
    expect(state({ goal: "x".repeat(MAX_GOAL + 1) }, {}, "goal")).toEqual({
      kind: "blocked",
      reason: "The goal is 1 character too long.",
    });
    expect(state({ goal: "x".repeat(MAX_GOAL + 12) }, {}, "goal")).toEqual({
      kind: "blocked",
      reason: "The goal is 12 characters too long.",
    });
  });

  it("blocks the review until every step is done", () => {
    expect(state({ goal: "x".repeat(MAX_GOAL + 1) })).toEqual({
      kind: "blocked",
      reason: "Finish the steps above.",
    });
    expect(state({})).toEqual({ kind: "ready", summary: "" });
  });

  it("derives the name: the drafted goal's, the research's, the goal's title, then its sources", () => {
    const named = (d: Partial<WizardDraft>) =>
      wiz({ ...draft, ...d }, facts, "review").derivedName;
    const goal = "Which orders ship late? Group them by carrier.";
    expect(
      named({ company: globex, draftedName: " Globex fulfilment ", goal }),
    ).toBe("Globex fulfilment");
    expect(named({ company: globex, goal })).toBe("Globex operations");
    expect(
      named({
        company: {
          ...globex,
          research: { ...globex.research, suggestedName: " " },
        },
        goal,
      }),
    ).toBe("Which orders ship late?");
    expect(named({ goal })).toBe("Which orders ship late?");
    expect(named({})).toBe("Stripe");
    // Advanced's name wins, trimmed; the derived one stays its placeholder.
    const w = wiz({ ...draft, name: " Orders ", goal }, facts, "review");
    expect(w.request?.name).toBe("Orders");
    expect(w.derivedName).toBe("Which orders ship late?");
  });

  it("puts it on a new database named as it, in the first project, else a new one", () => {
    expect(wiz({ ...draft, name: "Orders" }, facts, "review").request).toEqual({
      name: "Orders",
      database: {
        kind: "new",
        project: { kind: "existing", id: "p1" },
        name: "Orders",
      },
      generate: { connectorIds: ["c1"], documentSetIds: [] },
    });
    expect(
      wiz(draft, { ...facts, projects: [] }, "review").request?.database,
    ).toEqual({
      kind: "new",
      project: { kind: "new", name: "Acme" },
      name: "Stripe",
    });
    // It waits for the projects.
    expect(
      wiz(draft, { ...facts, projects: undefined }, "review").request,
    ).toBeNull();
  });

  it("takes Advanced's database: a free one the workspace has, or a new one named", () => {
    const db = (database: WizardDraft["database"], f?: Partial<WizardFacts>) =>
      state({ database }, f);
    expect(db({ kind: "existing", tenantId: "" })).toEqual({
      kind: "blocked",
      reason: "Choose a database, or create a new one.",
    });
    expect(db({ kind: "existing", tenantId: "t2" })).toEqual({
      kind: "blocked",
      reason: "contracts is used by Customer contracts. Choose another.",
    });
    expect(db({ kind: "existing", tenantId: "gone" })).toEqual({
      kind: "blocked",
      reason: "That database is gone. Choose another.",
    });
    expect(db({ kind: "existing", tenantId: "t1" })).toEqual({
      kind: "ready",
      summary: "",
    });
    const inProject = { kind: "existing" as const, id: "p1" };
    expect(db({ kind: "new", project: inProject, name: " " })).toEqual({
      kind: "blocked",
      reason: "Name the new database.",
    });
    expect(
      db({ kind: "new", project: { kind: "existing", id: "gone" }, name: "x" }),
    ).toEqual({
      kind: "blocked",
      reason: "That project is gone. Choose another.",
    });
    expect(
      db({ kind: "new", project: { kind: "new", name: "" }, name: "" }),
    ).toEqual({ kind: "blocked", reason: "Name the new project." });
    // A blocked database holds the request back; a loading read too.
    expect(
      wiz(
        { ...draft, database: { kind: "existing", tenantId: "t2" } },
        facts,
        "review",
      ).request,
    ).toBeNull();
    expect(
      wiz(
        { ...draft, database: { kind: "existing", tenantId: "t1" } },
        { ...facts, databases: undefined },
        "review",
      ).request,
    ).toBeNull();
    expect(
      wiz(
        { ...draft, database: { kind: "new", project: inProject, name: "x" } },
        { ...facts, projects: undefined },
        "review",
      ).request,
    ).toBeNull();
    // The review waits, saying so, while what the database needs is read.
    expect(
      db({ kind: "existing", tenantId: "t1" }, { databases: undefined }),
    ).toEqual({ kind: "loading" });
    expect(state({}, { projects: undefined })).toEqual({ kind: "loading" });
    // A blocked step before the review says that, not the database.
    expect(
      state({
        database: { kind: "existing", tenantId: "t2" },
        goal: "x".repeat(MAX_GOAL + 1),
      }),
    ).toEqual({ kind: "blocked", reason: "Finish the steps above." });
  });

  it("sends what every step chose, trimmed", () => {
    const sets = [set("g", "General"), set("b", "Board")];
    const w = wiz(
      {
        ...draft,
        name: " Orders ",
        database: {
          kind: "new",
          project: { kind: "new", name: " Foundry " },
          name: " crm ",
        },
        connectorIds: new Set(["c2"]),
        setIds: new Set(["b", "gone"]),
        goal: " Trace orders ",
      },
      {
        ...facts,
        connectors: [synced("c1", "Stripe"), synced("c2", "Shop")],
        sets,
      },
      "review",
    );
    expect(w.request).toEqual({
      name: "Orders",
      database: {
        kind: "new",
        project: { kind: "new", name: "Foundry" },
        name: "crm",
      },
      generate: {
        goal: "Trace orders",
        connectorIds: ["c2"],
        documentSetIds: ["b"],
      },
    });
    // An existing database goes as it is.
    expect(
      wiz(
        {
          ...draft,
          name: "Orders",
          database: { kind: "existing", tenantId: "t1" },
        },
        facts,
        "review",
      ).request,
    ).toEqual({
      name: "Orders",
      database: { kind: "existing", tenantId: "t1" },
      generate: { connectorIds: ["c1"], documentSetIds: [] },
    });
  });
});

describe("the table cap", () => {
  it("names how many tables the sources hold, and counts them for the picker", () => {
    // Six connectors holding 65 tables: every synced one by default.
    const six = [10, 10, 10, 10, 10, 15].map((n, i) =>
      synced(`c${i}`, `Source ${i}`, n),
    );
    const sources = (connectorIds: ReadonlySet<string> | null) =>
      generationSources(
        { connectorIds, eventSources: new Map(), setIds: new Set() },
        { connectors: six, sets: [], documents: [] },
      );
    expect(sources(null)).toMatchObject({
      state: { kind: "blocked", reason: overCap(65) },
      tables: 65,
    });
    // Unticking one brings it to 50 or fewer.
    const fewer = sources(new Set(six.slice(0, 5).map((c) => c.id)));
    expect(fewer.tables).toBe(50);
    expect(fewer.state.kind).toBe("ready");
    // Nothing chosen counts nothing.
    expect(sources(new Set()).tables).toBe(0);
    // Large counts are written with separators.
    expect(
      generationSources(
        { connectorIds: null, eventSources: new Map(), setIds: new Set() },
        {
          connectors: [synced("w", "Warehouse", 1200)],
          sets: [],
          documents: [],
        },
      ).state,
    ).toEqual({
      kind: "blocked",
      reason: overCap(1200).replace("1200", "1,200"),
    });
  });

  it("holds the wizard at the sources step until it fits", () => {
    const wide = [synced("c1", "Stripe", 40), synced("c2", "Shop", 25)];
    const w = wiz(draft, { ...facts, connectors: wide }, "review");
    expect(w.current).toBe("sources");
    expect(w.request).toBeNull();
    expect(
      wiz(
        { ...draft, connectorIds: new Set(["c1"]) },
        { ...facts, connectors: wide },
        "review",
      ).request?.generate.connectorIds,
    ).toEqual(["c1"]);
  });
});

describe("companyDomain", () => {
  it("follows its @example", () => {
    expect(companyDomain("grace@Globex-Industries.com")).toBe(
      "globex-industries.com",
    );
    expect(companyDomain("grace@gmail.com")).toBe("");
    expect(companyDomain("grace@yahoo.co.uk")).toBe("");
    expect(companyDomain("owner@localhost")).toBe("");
  });

  it("leaves out consumer mail, by domain and by brand", () => {
    for (const domain of [
      "googlemail.com",
      "icloud.com",
      "proton.me",
      "qq.com",
      "web.de",
      "hotmail.fr",
      "outlook.com",
      "gmx.de",
      "yandex.ru",
      "aol.com",
    ])
      expect(companyDomain(`a@${domain}`)).toBe("");
  });

  it("takes a company's registrable domain, without its subdomains or a trailing dot", () => {
    expect(companyDomain(" ada@mail.acme.co.uk. ")).toBe("acme.co.uk");
    expect(companyDomain("ada@eu.Globex.com")).toBe("globex.com");
    expect(companyDomain("ada@example.com")).toBe("example.com");
  });

  it("checks consumer mail on the registrable domain, as gate.go's FreeMail", () => {
    // A brand only as the registrable domain's first label.
    expect(companyDomain("jo@live.ucl.ac.uk")).toBe("ucl.ac.uk");
    expect(companyDomain("jo@outlook.globex.com")).toBe("globex.com");
    for (const email of [
      "jo@mail.yahoo.co.uk",
      "jo@eu.gmail.com",
      "jo@live.com.au",
      "jo@hotmail.co.jp",
    ])
      expect(companyDomain(email), email).toBe("");
  });

  it("names none for an address without a dotted domain", () => {
    for (const email of [
      "",
      "ada",
      "ada@",
      "@",
      "ada@acme",
      "ada@acme..com",
      "ada@ac me.com",
      "ada@10.0.0.1",
    ])
      expect(companyDomain(email)).toBe("");
  });
});

describe("registrableDomain", () => {
  it("follows its @example", () => {
    expect(registrableDomain("www.Globex.com.")).toBe("globex.com");
    expect(registrableDomain("mail.acme.co.uk")).toBe("acme.co.uk");
    expect(registrableDomain("co.uk")).toBe("");
    expect(registrableDomain("10.0.0.1")).toBe("");
  });

  it("takes two labels as the suffix under a country code's second level", () => {
    for (const [host, domain] of [
      ["a.b.acme.com.au", "acme.com.au"],
      ["dept.ucl.ac.uk", "ucl.ac.uk"],
      ["shop.acme.co.jp", "acme.co.jp"],
      ["acme.ne.jp", "acme.ne.jp"],
      // Not under a country code: one label.
      ["acme.co.com", "co.com"],
      ["acme.io", "acme.io"],
      ["x.acme.de", "acme.de"],
    ])
      expect(registrableDomain(host!), host).toBe(domain);
  });

  it("reads none from what isn't a dotted host name", () => {
    for (const host of [
      "",
      "localhost",
      "com",
      "com.au",
      "acme..com",
      "a b.com",
      "acme.com/x",
      "1.2.3",
    ])
      expect(registrableDomain(host), host).toBe("");
  });
});

describe("databasePlan", () => {
  const made = {
    project: { id: "p1", name: "Acme" },
    database: { id: "t1", projectId: "p1", name: "Stripe" },
  };
  const newIn = (
    project: { kind: "new"; name: string } | { kind: "existing"; id: string },
    name: string,
  ) => ({ kind: "new", project, name }) as const;

  it("follows its @example", () => {
    const pick = newIn({ kind: "new", name: "Acme" }, "Globex operations");
    expect(databasePlan(pick, made, false)).toEqual({
      kind: "use",
      tenantId: "t1",
    });
    expect(databasePlan(pick, made, true)).toEqual({
      kind: "create",
      project: { kind: "existing", id: "p1" },
      name: "Globex operations",
    });
    expect(
      databasePlan({ kind: "existing", tenantId: "t2" }, made, true),
    ).toEqual({
      kind: "use",
      tenantId: "t2",
    });
  });

  it("reuses the database made while the default stands, whatever its name or project", () => {
    for (const project of [
      { kind: "new", name: "Other" } as const,
      { kind: "existing", id: "p9" } as const,
    ])
      expect(databasePlan(newIn(project, "Renamed"), made, false)).toEqual({
        kind: "use",
        tenantId: "t1",
      });
  });

  it("reuses a chosen new database only for the same project and name", () => {
    expect(
      databasePlan(newIn({ kind: "new", name: "Acme" }, "Stripe"), made, true),
    ).toEqual({
      kind: "use",
      tenantId: "t1",
    });
    expect(
      databasePlan(newIn({ kind: "existing", id: "p1" }, "Stripe"), made, true),
    ).toEqual({
      kind: "use",
      tenantId: "t1",
    });
    expect(
      databasePlan(newIn({ kind: "existing", id: "p2" }, "Stripe"), made, true),
    ).toEqual({
      kind: "create",
      project: { kind: "existing", id: "p2" },
      name: "Stripe",
    });
    // A new project named otherwise is made afresh.
    expect(
      databasePlan(
        newIn({ kind: "new", name: "Acme 2" }, "Stripe"),
        made,
        true,
      ),
    ).toEqual({
      kind: "create",
      project: { kind: "new", name: "Acme 2" },
      name: "Stripe",
    });
  });

  it("creates what nothing made yet, reusing a project made", () => {
    const pick = newIn({ kind: "new", name: "Acme" }, "Stripe");
    expect(databasePlan(pick, {}, false)).toEqual({
      kind: "create",
      project: { kind: "new", name: "Acme" },
      name: "Stripe",
    });
    expect(databasePlan(pick, { project: made.project }, false)).toEqual({
      kind: "create",
      project: { kind: "existing", id: "p1" },
      name: "Stripe",
    });
  });
});

describe("companyFlow", () => {
  const research = (
    id: string,
    domain = "globex.com",
  ): FoundryCompanyResearch => ({
    id,
    domain,
    companyName: "Globex",
    summary: "Globex sells widgets.",
    questions: [
      { id: "q1", question: "Who asks?", bestGuess: "Ops" },
      { id: "q2", question: "What matters?" },
    ],
  });
  const start: CompanyFlow = {
    domain: "globex.com",
    company: null,
    goal: "",
    drafted: null,
    research: { kind: "idle" },
    draft: { kind: "idle" },
    autoDrafted: null,
  };
  const run = (flow: CompanyFlow, ...actions: CompanyFlowAction[]) =>
    actions.reduce(companyFlow, flow);
  // Researched globex.com, with a draft of its goal landed.
  const researched = run(
    start,
    { type: "research-start" },
    { type: "research-done", domain: "globex.com", research: research("r1") },
  );
  const drafted = run(
    researched,
    { type: "draft-start", auto: true },
    {
      type: "draft-done",
      researchId: "r1",
      goal: "Globex ops.",
      name: "Globex operations",
    },
  );

  it("follows its @example", () => {
    expect(companyFlow(start, { type: "research-start" }).research).toEqual({
      kind: "pending",
      domain: "globex.com",
    });
    const typed = run(
      start,
      { type: "research-start" },
      { type: "domain", domain: "initech.com" },
    );
    expect(
      companyFlow(typed, {
        type: "research-done",
        domain: "globex.com",
        research: { id: "r1" },
      }),
    ).toBe(typed);
  });

  it("starts research once, for a domain, and answers each question with its best guess", () => {
    expect(
      companyFlow({ ...start, domain: " " }, { type: "research-start" }),
    ).toEqual({
      ...start,
      domain: " ",
    });
    const pending = companyFlow(
      { ...start, domain: " globex.com " },
      { type: "research-start" },
    );
    expect(pending.research).toEqual({ kind: "pending", domain: "globex.com" });
    // A second Research while it runs (the step left and come back) is nothing.
    expect(companyFlow(pending, { type: "research-start" })).toBe(pending);
    expect(researched.research).toEqual({ kind: "idle" });
    expect(researched.company!.research.id).toBe("r1");
    expect([...researched.company!.answers]).toEqual([
      ["q1", "Ops"],
      ["q2", ""],
    ]);
    // What it does starts as the research's summary, and may be corrected.
    expect(researched.company!.summary).toBe("Globex sells widgets.");
    expect(
      run(researched, { type: "summary", summary: "Globex makes sensors." })
        .company!.summary,
    ).toBe("Globex makes sensors.");
    expect(companyFlow(start, { type: "summary", summary: "x" })).toBe(start);
  });

  it("sends what the company does only as a correction", () => {
    const of = (summary: string) => ({ ...researched.company!, summary });
    expect(correctedSummary(of("Globex sells widgets."))).toBeUndefined();
    expect(correctedSummary(of("  Globex sells widgets. "))).toBeUndefined();
    expect(correctedSummary(of(" "))).toBeUndefined();
    expect(correctedSummary(of(" Globex makes sensors. "))).toBe(
      "Globex makes sensors.",
    );
    expect(
      correctedSummary({
        ...of("Globex makes sensors."),
        research: { id: "r" },
      }),
    ).toBe("Globex makes sensors.");
    expect(
      run(researched, {
        type: "answer",
        questionId: "q2",
        answer: "Churn",
      }).company!.answers.get("q2"),
    ).toBe("Churn");
    expect(
      companyFlow(start, { type: "answer", questionId: "q1", answer: "x" }),
    ).toBe(start);
  });

  it("drops research for a domain edited since, and says why one failed", () => {
    const typed = run(
      start,
      { type: "research-start" },
      { type: "domain", domain: "initech.com" },
    );
    expect(typed.research).toEqual({ kind: "idle" });
    expect(
      companyFlow(typed, {
        type: "research-failed",
        domain: "globex.com",
        error: "x",
      }),
    ).toBe(typed);
    // Research again for the new domain: only its answer lands.
    const again = companyFlow(typed, { type: "research-start" });
    expect(
      companyFlow(again, {
        type: "research-done",
        domain: "globex.com",
        research: research("r1"),
      }),
    ).toBe(again);
    expect(
      companyFlow(again, {
        type: "research-done",
        domain: "initech.com",
        research: research("r2"),
      }).company!.research.id,
    ).toBe("r2");
    const failed = companyFlow(again, {
      type: "research-failed",
      domain: "initech.com",
      error: "down",
    });
    expect(failed.research).toEqual({ kind: "failed", error: "down" });
    // Editing the domain clears the failure.
    expect(
      companyFlow(failed, { type: "domain", domain: "initech.co" }).research,
    ).toEqual({ kind: "idle" });
  });

  it("drops the research, and the goal its draft wrote unless edited, when the domain changes", () => {
    const edited = companyFlow(drafted, {
      type: "domain",
      domain: "initech.com",
    });
    expect(edited).toMatchObject({
      domain: "initech.com",
      company: null,
      goal: "",
      drafted: null,
    });
    const own = run(
      drafted,
      { type: "goal", goal: "My own goal." },
      { type: "domain", domain: "initech.com" },
    );
    expect(own).toMatchObject({
      company: null,
      goal: "My own goal.",
      drafted: null,
    });
  });

  it("drops the last research's unedited goal when new research lands", () => {
    const next = run(
      drafted,
      { type: "research-start" },
      { type: "research-done", domain: "globex.com", research: research("r2") },
    );
    expect(next).toMatchObject({
      goal: "",
      drafted: null,
      draft: { kind: "idle" },
    });
    expect(next.company!.research.id).toBe("r2");
    // Its goal step drafts on its own again: a new research.
    expect(
      companyFlow(next, { type: "draft-start", auto: true }).draft,
    ).toEqual({
      kind: "pending",
      researchId: "r2",
    });
  });

  it("drafts on its own once per research, into an empty goal, and Write it again any time", () => {
    expect(companyFlow(start, { type: "draft-start", auto: false })).toBe(
      start,
    );
    const pending = companyFlow(researched, {
      type: "draft-start",
      auto: true,
    });
    expect(pending).toMatchObject({
      draft: { kind: "pending", researchId: "r1" },
      autoDrafted: "r1",
    });
    // Not twice at once.
    expect(companyFlow(pending, { type: "draft-start", auto: false })).toBe(
      pending,
    );
    // Once per research on its own, and never over a goal typed.
    const cleared = companyFlow(drafted, { type: "goal", goal: "" });
    expect(companyFlow(cleared, { type: "draft-start", auto: true })).toBe(
      cleared,
    );
    const typed = companyFlow(researched, { type: "goal", goal: "Mine." });
    expect(companyFlow(typed, { type: "draft-start", auto: true })).toBe(typed);
    expect(
      companyFlow(cleared, { type: "draft-start", auto: false }).draft,
    ).toEqual({
      kind: "pending",
      researchId: "r1",
    });
  });

  it("writes the goal and name a draft lands with, until the goal is edited", () => {
    expect(drafted).toMatchObject({
      goal: "Globex ops.",
      drafted: { goal: "Globex ops.", name: "Globex operations" },
      draft: { kind: "done" },
    });
    expect(
      companyFlow(drafted, { type: "goal", goal: "Globex ops!" }).draft,
    ).toEqual({ kind: "idle" });
    const pending = companyFlow(researched, {
      type: "draft-start",
      auto: true,
    });
    expect(
      companyFlow(pending, {
        type: "draft-failed",
        researchId: "r1",
        error: "down",
      }).draft,
    ).toEqual({ kind: "failed", error: "down" });
  });

  it("drops a draft that lands after its research was left", () => {
    const pending = companyFlow(researched, {
      type: "draft-start",
      auto: true,
    });
    const late = {
      type: "draft-done",
      researchId: "r1",
      goal: "Globex ops.",
      name: "Globex operations",
    } as const;
    // The domain edited: no company, and the old draft names nothing.
    const left = companyFlow(pending, {
      type: "domain",
      domain: "initech.com",
    });
    expect(left.draft).toEqual({ kind: "idle" });
    expect(companyFlow(left, late)).toBe(left);
    // Other research chosen, and a draft of it running.
    const other = run(
      left,
      { type: "research-start" },
      {
        type: "research-done",
        domain: "initech.com",
        research: research("r2", "initech.com"),
      },
      { type: "draft-start", auto: true },
    );
    expect(companyFlow(other, late)).toBe(other);
    expect(
      companyFlow(other, {
        type: "draft-failed",
        researchId: "r1",
        error: "x",
      }),
    ).toBe(other);
    // Nothing running: a stray answer is nothing.
    expect(companyFlow(researched, late)).toBe(researched);
  });
});

describe("newDatabase", () => {
  const project = { id: "p1", slug: "prod" };
  it("names a new database and project from the ontology and workspace", () => {
    expect(newDatabase(" Customer contracts ", " Acme ", [])).toEqual({
      kind: "new",
      project: { kind: "new", name: "Acme" },
      name: "Customer contracts",
    });
  });
  it("puts it in the workspace's first project when it has one", () => {
    expect(
      newDatabase("Orders", "Acme", [project, { id: "p2", slug: "dev" }]),
    ).toEqual({
      kind: "new",
      project: { kind: "existing", id: "p1" },
      name: "Orders",
    });
  });
  it("cuts each name to 63 characters, never half a surrogate pair", () => {
    expect(newDatabase("x".repeat(80), "y".repeat(80), []).name).toBe(
      "x".repeat(63),
    );
    expect(newDatabase("x".repeat(80), "y".repeat(80), []).project).toEqual({
      kind: "new",
      name: "y".repeat(63),
    });
    expect(newDatabase("x".repeat(62) + "😀", "Acme", []).name).toBe(
      "x".repeat(62),
    );
  });
});

describe("firstOntologyName", () => {
  const f = {
    connectors: [
      synced("c1", "Stripe"),
      synced("c2", " Shop "),
      { id: "c3", kind: "postgres" },
      { id: "e1", kind: "events", name: "Agent notes" },
    ],
    sets: [set("s1", "Contracts"), set("s2", "Stripe")],
  };

  it("lists what it drafts from: connectors, API sources, then sets", () => {
    expect(
      firstOntologyName(
        {
          connectorIds: ["c1", "c2"],
          eventSources: [
            {
              connectorId: "e1",
              mode: "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED",
            },
          ],
          documentSetIds: ["s1"],
        },
        f,
        "Acme",
      ),
    ).toBe("Stripe, Shop, Agent notes, and 1 more");
  });

  it("names a connector without a name by its kind, and each name once", () => {
    expect(
      firstOntologyName(
        { connectorIds: ["c3", "c1"], documentSetIds: ["s2"] },
        f,
        "Acme",
      ),
    ).toBe("postgres and Stripe");
  });

  it("falls back to the workspace's name, then a fixed one", () => {
    const none = { noConnectors: true as const, documentSetIds: ["gone"] };
    expect(firstOntologyName(none, f, " Acme ")).toBe("Acme");
    expect(firstOntologyName(none, f, " ")).toBe("My ontology");
  });

  it("cuts a long name to 100 characters", () => {
    const long = "x".repeat(150);
    expect(
      firstOntologyName(
        { connectorIds: ["c1"], documentSetIds: [] },
        { connectors: [synced("c1", long)], sets: [] },
        "Acme",
      ),
    ).toBe("x".repeat(100));
  });
});
