import { afterEach, describe, expect, it, vi } from "vitest";
import type {
  Connector,
  FoundryDocumentSet,
  FoundryOntology,
  OntologyGeneration,
} from "@/shared/wfe";
import {
  DRAFT_STEPS,
  ONTOLOGY_STEPS,
  STEP_COPY,
  draftWizard,
  ontologySetup,
  stepSummary,
  type DraftChoices,
  type SetupFacts,
} from "@/web/areas/ontology/model";
import {
  forgetNewOntology,
  readNewOntology,
} from "@/web/areas/ontology/new-ontology";

// Where an ontology's setup is (ontologySetup, for the overview), the
// overview's sources, Goal and Generate
// steps (draftWizard) and the steps' names. Each @example is checked here.

const facts = (
  ontology: Omit<FoundryOntology, "id"> = {},
  more: Partial<SetupFacts> = {},
): SetupFacts => ({
  ontology: { id: "o", ...ontology },
  versions: [],
  generations: [],
  explored: false,
  ...more,
});
const generation = (
  status: OntologyGeneration["status"],
  more: Partial<OntologyGeneration> = {},
): OntologyGeneration => ({ id: "g", status, ...more });
const current = { currentVersionId: "v1" };

describe("ontologySetup", () => {
  it("follows its @example", () => {
    expect(ontologySetup(facts())).toEqual({ step: "draft", state: "todo" });
    const running = generation("ONTOLOGY_GENERATION_STATUS_RUNNING"),
      failed = generation("ONTOLOGY_GENERATION_STATUS_FAILED");
    expect(ontologySetup(facts({}, { generations: [running] }))).toEqual({
      step: "draft",
      state: "running",
      generation: running,
    });
    expect(ontologySetup(facts({}, { generations: [failed] }))).toEqual({
      step: "draft",
      state: "failed",
      generation: failed,
    });
    expect(ontologySetup(facts({}, { versions: [{ id: "v1" }] }))).toEqual({
      step: "current",
      state: "todo",
      versionId: "v1",
    });
    expect(ontologySetup(facts(current))).toEqual({
      step: "load",
      state: "todo",
    });
    expect(
      ontologySetup(facts({ ...current, lastLoad: { status: "succeeded" } })),
    ).toEqual({ step: "explore", state: "todo" });
  });

  it("drafts until a generation is live, then shows it running, queued or not", () => {
    for (const status of [
      "ONTOLOGY_GENERATION_STATUS_QUEUED",
      "ONTOLOGY_GENERATION_STATUS_RUNNING",
    ] as const) {
      // A retry waits queued: it is still the live one.
      const live = generation(status, { attempts: 1 });
      expect(ontologySetup(facts({}, { generations: [live] }))).toEqual({
        step: "draft",
        state: "running",
        generation: live,
      });
    }
  });

  it("says the newest generation failed, and only the newest", () => {
    const failed = generation("ONTOLOGY_GENERATION_STATUS_FAILED", {
      error: "the agent declined",
    });
    expect(ontologySetup(facts({}, { generations: [failed] }))).toMatchObject({
      state: "failed",
      generation: { error: "the agent declined" },
    });
    // An older failure under a newer one that drafted nothing is history.
    expect(
      ontologySetup(
        facts(
          {},
          {
            generations: [
              generation("ONTOLOGY_GENERATION_STATUS_SUCCEEDED"),
              failed,
            ],
          },
        ),
      ),
    ).toEqual({ step: "draft", state: "todo" });
    // A live one after a failure is running.
    expect(
      ontologySetup(
        facts(
          {},
          {
            generations: [
              generation("ONTOLOGY_GENERATION_STATUS_QUEUED"),
              failed,
            ],
          },
        ),
      ),
    ).toMatchObject({ step: "draft", state: "running" });
  });

  it("asks for the newest draft to be made current, even with another live", () => {
    expect(
      ontologySetup(
        facts(
          {},
          {
            versions: [{ id: "v2" }, { id: "v1" }],
            generations: [generation("ONTOLOGY_GENERATION_STATUS_RUNNING")],
          },
        ),
      ),
    ).toEqual({ step: "current", state: "todo", versionId: "v2" });
    // A generation that stored its draft since the versions were read.
    expect(
      ontologySetup(
        facts(
          {},
          {
            generations: [
              generation("ONTOLOGY_GENERATION_STATUS_RUNNING"),
              generation("ONTOLOGY_GENERATION_STATUS_SUCCEEDED", {
                resultVersionId: "v1",
              }),
            ],
          },
        ),
      ),
    ).toEqual({ step: "current", state: "todo", versionId: "v1" });
  });

  it("asks a current version to be loaded, running while a load runs", () => {
    expect(
      ontologySetup(facts({ ...current, activeJob: { kind: "load" } })),
    ).toEqual({ step: "load", state: "running" });
    // An extract isn't a load, and a failed load is to do again.
    expect(
      ontologySetup(facts({ ...current, activeJob: { kind: "extract" } })),
    ).toEqual({ step: "load", state: "todo" });
    expect(
      ontologySetup(facts({ ...current, lastLoad: { status: "failed" } })),
    ).toEqual({ step: "load", state: "todo" });
    // Explored before, but not loaded now: loading comes first.
    expect(ontologySetup(facts(current, { explored: true }))).toEqual({
      step: "load",
      state: "todo",
    });
  });

  it("is done once a loaded ontology was explored", () => {
    const loaded = {
      ...current,
      currentLoad: { id: "l", version: "foundry-v1" },
    };
    expect(ontologySetup(facts(loaded))).toEqual({
      step: "explore",
      state: "todo",
    });
    expect(ontologySetup(facts(loaded, { explored: true }))).toEqual({
      step: "done",
    });
  });
});

const synced = (id: string, name: string, tables = 1): Connector => ({
  id,
  kind: "stripe",
  name,
  tables: Array.from({ length: tables }, (_, i) => ({ name: `t${i}` })),
  lastSync: { id: "s" + id, status: "CONNECTOR_SYNC_STATUS_SUCCEEDED" },
});
const none: DraftChoices = {
  connectorIds: null,
  eventSources: new Map(),
  setIds: new Set(),
  goal: "",
};
const workspace = {
  connectors: [synced("c1", "Stripe")],
  sets: [],
  documents: [],
};

describe("draftWizard", () => {
  it("follows its @example", () => {
    expect(draftWizard(none, workspace, null).current).toBe("sources");
    expect(draftWizard(none, workspace, "draft").request).toEqual({
      connectorIds: ["c1"],
      documentSetIds: [],
    });
    expect(
      draftWizard({ ...none, connectorIds: new Set() }, workspace, "draft")
        .current,
    ).toBe("sources");
  });

  it("is the wizard's sources and Goal, then Generate a draft", () => {
    const w = draftWizard(none, workspace, "draft");
    expect(w.steps.map((s) => [s.id, s.optional])).toEqual([
      ["sources", false],
      ["goal", true],
      ["draft", false],
    ]);
    expect(w.steps.map((s) => s.state)).toEqual([
      { kind: "ready", summary: "Stripe: 1 table" },
      { kind: "skipped" },
      { kind: "ready", summary: "" },
    ]);
    expect([w.back, w.next]).toEqual(["goal", null]);
    expect(draftWizard(none, workspace, "nope").current).toBe("sources");
    expect(draftWizard(none, workspace, null).back).toBeNull();
  });

  it("generates nothing while a step is blocked or loading", () => {
    const blocked = draftWizard(none, { ...workspace, connectors: [] }, null);
    expect(blocked.steps.at(-1)!.state).toEqual({
      kind: "blocked",
      reason: "Finish the steps above.",
    });
    expect(blocked.request).toBeNull();
    const loading = draftWizard(none, { sets: [], documents: [] }, "draft");
    expect(loading.current).toBe("draft");
    expect(loading.steps.at(-1)!.state).toEqual({ kind: "loading" });
    expect(loading.request).toBeNull();
  });

  it("sends what was chosen, trimmed, dropping sets that are gone", () => {
    const w = draftWizard(
      {
        connectorIds: new Set(["c2"]),
        eventSources: new Map(),
        setIds: new Set(["b", "gone"]),
        goal: " Trace orders ",
      },
      {
        connectors: [synced("c1", "Stripe"), synced("c2", "Shop")],
        sets: [
          { id: "g", name: "General" },
          { id: "b", name: "Board" },
        ],
        documents: [],
      },
      "draft",
    );
    expect(w.request).toEqual({
      goal: "Trace orders",
      connectorIds: ["c2"],
      documentSetIds: ["b"],
    });
    expect(w.steps.map((s) => stepSummary(s.state))).toEqual([
      "Shop: 1 table; Board",
      "Trace orders",
      "",
    ]);
  });
});

describe("draftWizard with API sources", () => {
  const replies: Connector = {
    id: "e1",
    kind: "events",
    name: "Agent replies",
    tables: [{ name: "events", rowCount: "4" }],
  };
  it("drafts from an API source alone, read through mappings", () => {
    const w = draftWizard(
      {
        connectorIds: null,
        eventSources: new Map([["e1", "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED"]]),
        setIds: new Set(),
        goal: "",
      },
      { connectors: [replies], sets: [], documents: [] },
      "draft",
    );
    expect(w.current).toBe("draft");
    expect(stepSummary(w.steps[0]!.state)).toBe("Agent replies (mappings)");
    expect(w.request).toEqual({
      noConnectors: true,
      eventSources: [
        { connectorId: "e1", mode: "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED" },
      ],
      documentSetIds: [],
    });
  });
  it("stops at the sources while a chosen one has no pieces", () => {
    const w = draftWizard(
      {
        connectorIds: null,
        eventSources: new Map([["e1", "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED"]]),
        setIds: new Set(),
        goal: "",
      },
      {
        connectors: [
          synced("c1", "Stripe"),
          { ...replies, tables: [{ name: "events", rowCount: "0" }] },
        ],
        sets: [],
        documents: [],
      },
      "draft",
    );
    expect(w.current).toBe("sources");
    expect(w.steps[0]!.state).toEqual({
      kind: "blocked",
      reason: "Agent replies has no pieces yet.",
    });
    expect(w.request).toBeNull();
  });

  describe("read by entity extraction", () => {
    const texted: Connector = {
      ...replies,
      config: { events: { textField: "reply" } },
    };
    const pieces: FoundryDocumentSet = {
      id: "set_pieces",
      name: "Pieces",
      documents: 4,
      connector: {
        connectorId: "e1",
        connectorKind: "events",
        stream: "pieces",
        maxDocuments: 500,
      },
    };
    const extraction = (setIds: string[] = []) => ({
      connectorIds: null,
      eventSources: new Map([
        ["e1", "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION" as const],
      ]),
      setIds: new Set(setIds),
      goal: "",
    });

    it("drafts from its own set, sent as its pick and never as a set", () => {
      const w = draftWizard(
        // Its set kept among the sets goes as nothing.
        extraction(["set_pieces", "g"]),
        {
          connectors: [texted],
          sets: [{ id: "g", name: "General", documents: 1 }, pieces],
          documents: [],
        },
        "draft",
      );
      expect(w.current).toBe("draft");
      expect(stepSummary(w.steps[0]!.state)).toBe(
        "Agent replies (extraction); General",
      );
      expect(w.request).toEqual({
        noConnectors: true,
        eventSources: [
          { connectorId: "e1", mode: "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION" },
        ],
        documentSetIds: ["g"],
      });
    });

    it("stops at the sources without a text field or pieces as documents", () => {
      // [its connector, the sets, why]
      const cases: [Connector, FoundryDocumentSet[], string][] = [
        [replies, [pieces], "Agent replies has no text field."],
        [texted, [], "Agent replies has no pieces as documents yet."],
        [
          texted,
          [{ ...pieces, documents: 0 }],
          "Agent replies has no pieces as documents yet.",
        ],
      ];
      for (const [c, sets, reason] of cases) {
        const w = draftWizard(
          extraction(),
          { connectors: [c], sets, documents: [] },
          "draft",
        );
        expect(w.current).toBe("sources");
        expect(w.steps[0]!.state).toEqual({ kind: "blocked", reason });
        expect(w.request).toBeNull();
      }
    });
  });
});

describe("the wizard's kept choices", () => {
  const store = new Map<string, string>();
  const stub = () =>
    vi.stubGlobal("sessionStorage", {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => void store.set(k, v),
      removeItem: (k: string) => void store.delete(k),
    });
  const key = "foundry.new-ontology.v1:ws1";
  afterEach(() => {
    store.clear();
    vi.unstubAllGlobals();
  });

  it("keeps the API sources chosen, with their modes", () => {
    stub();
    const chosen = new Map([
      ["e1", "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED"],
      ["e2", "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION"],
    ] as const);
    // As the page keeps them: the map's entries.
    store.set(
      key,
      JSON.stringify({
        name: "Orders",
        database: null,
        connectorIds: ["c1"],
        eventSources: [...chosen],
        setIds: [],
        goal: "",
        token: "t1",
        made: {},
      }),
    );
    const read = readNewOntology("ws1")!;
    expect(new Map(read.eventSources)).toEqual(chosen);
    expect(read.connectorIds).toEqual(["c1"]);
    forgetNewOntology("ws1");
    expect(readNewOntology("ws1")).toBeNull();
  });

  it("reads choices kept before API sources as none, and drops what isn't a pair", () => {
    stub();
    store.set(key, JSON.stringify({ name: "Orders", token: "t1" }));
    expect(readNewOntology("ws1")!.eventSources).toEqual([]);
    store.set(
      key,
      JSON.stringify({
        token: "t1",
        eventSources: [
          ["e1", "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED"],
          ["", "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED"],
          ["e2", "FOUNDRY_EVENT_SOURCE_MODE_UNSPECIFIED"],
          ["e3"],
          ["e4", "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED", "x"],
          [4, "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED"],
          "e5",
          null,
        ],
      }),
    );
    expect(readNewOntology("ws1")!.eventSources).toEqual([
      ["e1", "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED"],
    ]);
    store.set(key, JSON.stringify({ token: "t1", eventSources: { e1: 1 } }));
    expect(readNewOntology("ws1")!.eventSources).toEqual([]);
  });
});

describe("draftWizard from files alone", () => {
  it("drafts an existing ontology from a set's files, with no connector", () => {
    const w = draftWizard(
      {
        connectorIds: null,
        eventSources: new Map(),
        setIds: new Set(["g"]),
        goal: "",
      },
      {
        connectors: [],
        sets: [{ id: "g", name: "General" }],
        documents: [
          {
            id: "f",
            setId: "g",
            mediaType: "text/csv",
            status: "uploaded",
            read: { state: "FOUNDRY_FILE_READ_STATE_READY", rows: "3" },
          },
        ],
      },
      "draft",
    );
    expect(w.current).toBe("draft");
    expect(w.request).toEqual({ noConnectors: true, documentSetIds: ["g"] });
  });
});

describe("STEP_COPY.sources", () => {
  it("is one step, any kind of source", () => {
    expect(STEP_COPY.sources.title).toBe(
      "Connect your data or upload documents",
    );
    expect(STEP_COPY.sources.text).toBe(
      "What Foundry’s agent drafts from: synced tables, pieces your agents send, table files, and folders of contracts, notes and other text. Any will do.",
    );
  });
});

describe("stepSummary", () => {
  it("follows its @example", () => {
    expect(stepSummary({ kind: "ready", summary: "Orders" })).toBe("Orders");
    expect(stepSummary({ kind: "skipped" })).toBe("Skipped");
    expect(stepSummary({ kind: "loading" })).toBe("…");
    expect(stepSummary({ kind: "blocked", reason: "Name it." })).toBe("…");
  });
});

describe("STEP_COPY", () => {
  it("names every step of the wizard and the overview", () => {
    for (const id of [...DRAFT_STEPS, ...ONTOLOGY_STEPS])
      expect(STEP_COPY[id].title).toBeTruthy();
    expect(STEP_COPY.draft.title).toBe("Generate a draft");
  });
});
