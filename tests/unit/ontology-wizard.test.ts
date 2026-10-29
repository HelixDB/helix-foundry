import { describe, expect, it } from "vitest";
import type {
  Connector,
  FoundryDocument,
  FoundryDocumentSet,
} from "@/shared/wfe";
import {
  DOCUMENTS_PER_SET,
  MAX_GENERATION_SETS,
  MAX_GOAL,
  MAX_TABLES,
  firstOntologyName,
  firstOntologyWizard,
  newDatabase,
  newOntologyWizard,
  setSourcesText,
  type DraftChoices,
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
  name: "Orders",
  database: { kind: "existing", tenantId: "t1" },
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
const state = (
  d: Partial<WizardDraft>,
  f: Partial<WizardFacts> = {},
  id = "review",
) =>
  newOntologyWizard(
    { ...draft, ...d },
    { ...facts, ...f },
    "review",
  ).steps.find((s) => s.id === id)!.state;

describe("newOntologyWizard", () => {
  it("follows its @example", () => {
    const example: WizardFacts = {
      databases: [
        {
          tenantId: "t1",
          projectId: "p1",
          project: "Production",
          name: "orders",
        },
      ],
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
    expect(newOntologyWizard(draft, example, "review").current).toBe("review");
    expect(
      newOntologyWizard(draft, example, "review").steps.map((s) => s.state),
    ).toEqual([
      { kind: "ready", summary: "Orders" },
      { kind: "ready", summary: "orders in Production" },
      { kind: "ready", summary: "Stripe: 1 table" },
      { kind: "skipped" },
      { kind: "ready", summary: "" },
    ]);
    expect(
      newOntologyWizard({ ...draft, name: " " }, example, "review").current,
    ).toBe("name");
    expect(newOntologyWizard(draft, example, null).next).toBe("database");
  });

  it("lists the steps in order, with the optional ones marked", () => {
    const w = newOntologyWizard(draft, facts, null);
    expect(w.steps.map((s) => [s.id, s.optional])).toEqual([
      ["name", false],
      ["database", false],
      ["sources", false],
      ["goal", true],
      ["review", false],
    ]);
  });

  it("shows the step asked for, the first one when it is missing or unknown", () => {
    expect(newOntologyWizard(draft, facts, null).current).toBe("name");
    expect(newOntologyWizard(draft, facts, "nope").current).toBe("name");
    expect(newOntologyWizard(draft, facts, "goal").current).toBe("goal");
    // Asking for a step never needs the steps after it.
    expect(
      newOntologyWizard({ ...draft, database: null }, facts, "database")
        .current,
    ).toBe("database");
  });

  it("stops at the first blocked step before the one asked for", () => {
    const empty = { ...draft, name: "", database: null };
    expect(newOntologyWizard(empty, facts, "review").current).toBe("name");
    expect(
      newOntologyWizard({ ...draft, database: null }, facts, "goal").current,
    ).toBe("database");
    expect(
      newOntologyWizard(draft, { ...facts, connectors: [] }, "review").current,
    ).toBe("sources");
  });

  it("lets a loading step pass, and sends nothing until it loads", () => {
    const w = newOntologyWizard(draft, { connectors: undefined }, "review");
    expect(w.current).toBe("review");
    expect(w.steps.find((s) => s.id === "sources")!.state).toEqual({
      kind: "loading",
    });
    expect(w.steps.at(-1)!.state).toEqual({ kind: "loading" });
    expect(w.request).toBeNull();
  });

  it("goes back and on one step at a time, stopping at either end", () => {
    const at = (step: string) => newOntologyWizard(draft, facts, step);
    expect([at("name").back, at("name").next]).toEqual([null, "database"]);
    expect([at("sources").back, at("sources").next]).toEqual([
      "database",
      "goal",
    ]);
    expect([at("review").back, at("review").next]).toEqual(["goal", null]);
  });

  it("needs a name", () => {
    expect(state({ name: "  Orders " }, {}, "name")).toEqual({
      kind: "ready",
      summary: "Orders",
    });
    expect(state({ name: "\n" }, {}, "name")).toEqual({
      kind: "blocked",
      reason: "Name the ontology.",
    });
  });

  it("needs a free database the workspace has, or a new one named", () => {
    const db = (database: WizardDraft["database"], f?: Partial<WizardFacts>) =>
      state({ database }, f, "database");
    expect(db(null)).toEqual({
      kind: "blocked",
      reason: "Choose a database, or create one.",
    });
    expect(db({ kind: "existing", tenantId: "t2" })).toEqual({
      kind: "blocked",
      reason: "contracts is used by Customer contracts. Choose another.",
    });
    expect(db({ kind: "existing", tenantId: "gone" })).toEqual({
      kind: "blocked",
      reason: "That database is gone. Choose another.",
    });
    expect(
      db({ kind: "existing", tenantId: "t1" }, { databases: undefined }),
    ).toEqual({ kind: "loading" });

    const inProject = { kind: "existing" as const, id: "p1" };
    expect(db({ kind: "new", project: inProject, name: " crm " })).toEqual({
      kind: "ready",
      summary: "A new database, crm, in Production",
    });
    expect(db({ kind: "new", project: inProject, name: " " })).toEqual({
      kind: "blocked",
      reason: "Name the new database.",
    });
    expect(
      db({
        kind: "new",
        project: { kind: "existing", id: "gone" },
        name: "crm",
      }),
    ).toEqual({
      kind: "blocked",
      reason: "That project is gone. Choose another.",
    });
    expect(
      db(
        { kind: "new", project: inProject, name: "crm" },
        { projects: undefined },
      ),
    ).toEqual({ kind: "loading" });
    // A new project needs no read, but a name, asked for first.
    expect(
      db(
        {
          kind: "new",
          project: { kind: "new", name: " Foundry " },
          name: "crm",
        },
        { projects: undefined },
      ),
    ).toEqual({
      kind: "ready",
      summary: "A new database, crm, in a new project, Foundry",
    });
    expect(
      db({ kind: "new", project: { kind: "new", name: "" }, name: "" }),
    ).toEqual({ kind: "blocked", reason: "Name the new project." });
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
    const w = newOntologyWizard(
      { ...draft, connectorIds: new Set(["c1", "c3"]) },
      { ...facts, connectors: [synced("c1", "Stripe"), running] },
      "review",
    );
    expect(w.steps[2]!.state).toEqual({
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
      reason: `One generation covers at most ${MAX_TABLES} tables, table files included. Choose fewer connectors or sets.`,
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
      newOntologyWizard(
        { ...draft, setIds: new Set(["g"]) },
        { ...facts, connectors, sets, documents },
        "review",
      );
    // A table file that has been read.
    const csv = only([doc("f", "g", "text/csv", "READY")]);
    expect(csv.steps[2]!.state).toEqual({
      kind: "ready",
      summary: "General: 1 table file",
    });
    expect(csv.request!.generate).toEqual({
      noConnectors: true,
      documentSetIds: ["g"],
    });
    // Text alone.
    expect(only([doc("m", "g", "text/mdx")]).steps[2]!.state).toEqual({
      kind: "ready",
      summary: "General: 1 document",
    });
    // Both, counted apart.
    expect(
      only([
        doc("f", "g", "text/csv", "READY"),
        doc("m", "g", "text/markdown"),
        doc("n", "g", "text/markdown"),
      ]).steps[2]!.state,
    ).toEqual({
      kind: "ready",
      summary: "General: 1 table file and 2 documents",
    });
    // A connector not chosen leaves files alone, still sending no connectors.
    expect(
      newOntologyWizard(
        { ...draft, connectorIds: new Set(), setIds: new Set(["g"]) },
        { ...facts, sets, documents: [doc("m", "g", "text/plain")] },
        "review",
      ).request!.generate,
    ).toEqual({ noConnectors: true, documentSetIds: ["g"] });
    // A PDF whose text is read is a document like any text.
    expect(
      only([doc("p", "g", "application/pdf", "READY")]).steps[2]!.state,
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
      expect(only(documents).steps[2]!.state).toEqual({
        kind: "blocked",
        reason:
          "Choose a synced connector, or a set holding documents Foundry reads.",
      });
    // Files in a set not chosen don't count.
    expect(
      newOntologyWizard(
        draft,
        {
          ...facts,
          connectors: [],
          sets,
          documents: [doc("m", "g", "text/plain")],
        },
        "review",
      ).steps[2]!.state,
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
      newOntologyWizard(
        { ...draft, connectorIds: new Set(), setIds: new Set(setIds) },
        { ...facts, sets, documents },
        "review",
      );
    expect(w(["n"]).steps[2]!.state).toEqual({
      kind: "ready",
      summary: "Granola / Notes: 418 documents",
    });
    expect(w(["n"]).request!.generate).toEqual({
      noConnectors: true,
      documentSetIds: ["n"],
    });
    // With the uploads of a set people made, counted together.
    expect(
      w(["g", "n"], [doc("m", "g", "text/markdown")]).steps[2]!.state,
    ).toEqual({
      kind: "ready",
      summary: "General and Granola / Notes: 419 documents",
    });
    // Empty until its first sync writes it: nothing to draft from.
    expect(
      newOntologyWizard(
        { ...draft, connectorIds: new Set(), setIds: new Set(["n"]) },
        { ...facts, sets: [{ ...notes, documents: 0 }], documents: [] },
        "review",
      ).steps[2]!.state,
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
      newOntologyWizard(
        { ...draft, setIds: new Set(["g"]) },
        { ...facts, connectors, sets, documents },
        "review",
      ).steps[2]!.state;
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
      newOntologyWizard(
        { ...draft, setIds: new Set(["g"]) },
        {
          ...facts,
          connectors: [synced("c1", "Stripe", MAX_TABLES)],
          sets,
          documents: [doc("f", "g", "text/csv", "READY")],
        },
        "review",
      ).steps[2]!.state,
    ).toEqual({
      kind: "blocked",
      reason: `One generation covers at most ${MAX_TABLES} tables, table files included. Choose fewer connectors or sets.`,
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
    expect(state({ name: "" })).toEqual({
      kind: "blocked",
      reason: "Finish the steps above.",
    });
    expect(state({})).toEqual({ kind: "ready", summary: "" });
  });

  it("sends what every step chose, trimmed", () => {
    const sets = [set("g", "General"), set("b", "Board")];
    const w = newOntologyWizard(
      {
        name: " Orders ",
        database: {
          kind: "new",
          project: { kind: "new", name: " Foundry " },
          name: " crm ",
        },
        connectorIds: new Set(["c2"]),
        eventSources: new Map(),
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
    // No goal sends none; an existing database goes as it is.
    expect(newOntologyWizard(draft, facts, "review").request).toEqual({
      name: "Orders",
      database: { kind: "existing", tenantId: "t1" },
      generate: { connectorIds: ["c1"], documentSetIds: [] },
    });
    expect(
      newOntologyWizard({ ...draft, database: null }, facts, "review").request,
    ).toBeNull();
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
  it("leaves the step blocked on what it can't fill", () => {
    const wizard = newOntologyWizard(
      {
        name: "Orders",
        database: newDatabase("Orders", "", []),
        connectorIds: null,
        eventSources: new Map(),
        setIds: new Set(),
        goal: "",
      },
      { databases: [], projects: [] },
      "database",
    );
    expect(wizard.steps[1]!.state).toEqual({
      kind: "blocked",
      reason: "Name the new project.",
    });
  });
  it("is ready as filled in", () => {
    const wizard = newOntologyWizard(
      {
        name: "Orders",
        database: newDatabase("Orders", "Acme", []),
        connectorIds: null,
        eventSources: new Map(),
        setIds: new Set(),
        goal: "",
      },
      { databases: [], projects: [] },
      "database",
    );
    expect(wizard.steps[1]!.state).toEqual({
      kind: "ready",
      summary: "A new database, Orders, in a new project, Acme",
    });
  });
});

describe("firstOntologyWizard", () => {
  const choices: DraftChoices = {
    connectorIds: null,
    eventSources: new Map(),
    setIds: new Set<string>(),
    goal: "",
  };
  const first = (
    d: Partial<DraftChoices> = {},
    f: Partial<WizardFacts> = {},
    id: string | null = "review",
  ) =>
    firstOntologyWizard({ ...choices, ...d }, { ...facts, ...f }, "Acme", id);

  it("asks only for the sources and the goal, then the review", () => {
    const w = first({}, {}, null);
    expect(w.steps.map((s) => s.id)).toEqual(["sources", "goal", "review"]);
    expect(w.current).toBe("sources");
    expect(w.back).toBeNull();
    expect(w.next).toBe("goal");
    expect(first({}, {}, "name").current).toBe("sources");
  });

  it("names it from its sources and puts it on a new database", () => {
    expect(first().request).toEqual({
      name: "Stripe",
      database: {
        kind: "new",
        project: { kind: "existing", id: "p1" },
        name: "Stripe",
      },
      generate: { connectorIds: ["c1"], documentSetIds: [] },
    });
    // A workspace without a project gets one named as the workspace.
    expect(first({}, { projects: [] }).request?.database).toEqual({
      kind: "new",
      project: { kind: "new", name: "Acme" },
      name: "Stripe",
    });
  });

  it("holds the request while the projects or sources are read, or a step is blocked", () => {
    expect(first({}, { projects: undefined }).request).toBeNull();
    expect(first({}, { connectors: undefined }).request).toBeNull();
    expect(first({ connectorIds: new Set() }).request).toBeNull();
    expect(first({ connectorIds: new Set() }).current).toBe("sources");
    expect(first({ goal: "x".repeat(MAX_GOAL + 1) }).current).toBe("goal");
  });

  it("sends the goal", () => {
    expect(first({ goal: " Track renewals " }).request?.generate).toEqual({
      goal: "Track renewals",
      connectorIds: ["c1"],
      documentSetIds: [],
    });
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
