import { describe, expect, it } from "vitest";
import type {
  Connector,
  FoundryDocument,
  FoundryDocumentSet,
  FoundryEventSourceMode,
} from "@/shared/wfe";
import {
  MAX_GENERATION_SETS,
  MAX_TABLES,
  eventSourceReadiness,
  generationPicks,
  generationSources,
  isEventSourceSet,
  pickerSets,
  setRoom,
  startingEventSources,
  startingSets,
  type DraftChoices,
} from "@/web/areas/ontology/model";
import { ontologyPaths } from "@/web/areas/ontology/paths";

// What a generation drafts from once API sources (connectors of kind events)
// are among its sources: generationSources leaves them out of the
// connectors and sends them in eventSources, each in its mode, read through
// mappings as its table or by entity extraction as its own set of pieces,
// which no set choice lists or sends (isEventSourceSet, pickerSets,
// startingSets); Try again and the setup split a generation's tables and
// sets by kind (generationPicks); Generate starts from a link's or the
// current version's generation's (startingEventSources), and links carry
// them (paths.generate). Each @example is checked here.

const STRUCTURED: FoundryEventSourceMode =
    "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED",
  EXTRACTION: FoundryEventSourceMode = "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION";

const synced = (id: string, name: string, tables = 1): Connector => ({
  id,
  kind: "stripe",
  name,
  tables: Array.from({ length: tables }, (_, i) => ({ name: `t${i}` })),
  lastSync: { id: "s" + id, status: "CONNECTOR_SYNC_STATUS_SUCCEEDED" },
});
// An API source whose table holds `rows` pieces; none: no table yet.
const source = (id: string, name: string, rows?: number): Connector => ({
  id,
  kind: "events",
  name,
  config: { events: { description: "What each piece is" } },
  ...(rows === undefined
    ? {}
    : { tables: [{ name: "events", rowCount: String(rows) }] }),
});
const tableFile = (id: string, setId: string): FoundryDocument => ({
  id,
  setId,
  mediaType: "text/csv",
  status: "uploaded",
  read: { state: "FOUNDRY_FILE_READ_STATE_READY" },
});
const picks = (...ids: string[]) =>
  new Map(ids.map((i): [string, FoundryEventSourceMode] => [i, STRUCTURED]));
const extracting = (...ids: string[]) =>
  new Map(ids.map((i): [string, FoundryEventSourceMode] => [i, EXTRACTION]));
// An API source with a text field, whose table holds `rows` pieces.
const texted = (id: string, name: string, rows = 3): Connector => ({
  ...source(id, name, rows),
  config: { events: { description: "A reply", textField: "reply" } },
});
// An API source's own set, holding `documents` pieces as documents.
const piecesOf = (connectorId: string, documents = 2): FoundryDocumentSet => ({
  id: "set_" + connectorId,
  name: "Pieces",
  documents,
  connector: {
    connectorId,
    connectorKind: "events",
    stream: "pieces",
    maxDocuments: 500,
  },
});
// A Granola connection's notes.
const notes: FoundryDocumentSet = {
  id: "set_notes",
  name: "Notes",
  documents: 4,
  connector: {
    connectorId: "gr",
    connectorKind: "granola",
    connectorName: "Team notes",
    stream: "notes",
  },
};
const draft = (
  over: Partial<Pick<DraftChoices, "connectorIds" | "eventSources" | "setIds">>,
) => ({
  connectorIds: null,
  eventSources: new Map(),
  setIds: new Set<string>(),
  ...over,
});
const facts = (connectors: Connector[], more = {}) => ({
  connectors,
  sets: [],
  documents: [],
  ...more,
});

describe("generationSources with API sources", () => {
  it("follows its @example", () => {
    const f = {
      connectors: [],
      sets: [{ id: "g", name: "General" }],
      documents: [tableFile("d", "g")],
    };
    expect(generationSources(draft({ setIds: new Set(["g"]) }), f)).toEqual({
      state: { kind: "ready", summary: "General: 1 table file" },
      request: { noConnectors: true, documentSetIds: ["g"] },
    });
    const replies = {
      id: "e1",
      kind: "events",
      name: "Agent replies",
      tables: [{ name: "events", rowCount: "3" }],
    };
    expect(
      generationSources(draft({ eventSources: picks("e1") }), {
        ...f,
        connectors: [replies],
      }),
    ).toEqual({
      state: { kind: "ready", summary: "Agent replies (mappings)" },
      request: {
        noConnectors: true,
        eventSources: [{ connectorId: "e1", mode: STRUCTURED }],
        documentSetIds: [],
      },
    });
  });

  it("leaves them out of every synced connector, and of those chosen", () => {
    const all = facts([synced("c1", "Stripe"), source("e1", "Replies", 3)]);
    // Every synced one is the connectors', not the API sources'.
    expect(generationSources(draft({}), all)).toEqual({
      state: { kind: "ready", summary: "Stripe: 1 table" },
      request: { connectorIds: ["c1"], documentSetIds: [] },
    });
    // An API source's id among the connectors chosen picks nothing.
    expect(
      generationSources(draft({ connectorIds: new Set(["c1", "e1"]) }), all)
        .request,
    ).toEqual({ connectorIds: ["c1"], documentSetIds: [] });
    expect(
      generationSources(draft({ connectorIds: new Set(["e1"]) }), all).state,
    ).toEqual({
      kind: "blocked",
      reason:
        "Choose a synced connector, an API source with pieces, or a set holding documents Foundry reads.",
    });
  });

  it("sends each chosen one in its mode, in the workspace's order", () => {
    const all = facts(
      [
        synced("c1", "Stripe"),
        source("e1", "Replies", 3),
        texted("e2", "Orders", 1),
        source("e3", "Unchosen", 9),
      ],
      { sets: [piecesOf("e2")] },
    );
    const chosen = new Map<string, FoundryEventSourceMode>([
      ["e2", EXTRACTION],
      ["e1", STRUCTURED],
    ]);
    const { state, request } = generationSources(
      draft({ eventSources: chosen }),
      all,
    );
    // Its own set is the extraction pick's, never in documentSetIds.
    expect(request).toEqual({
      connectorIds: ["c1"],
      eventSources: [
        { connectorId: "e1", mode: STRUCTURED },
        { connectorId: "e2", mode: EXTRACTION },
      ],
      documentSetIds: [],
    });
    expect(state).toEqual({
      kind: "ready",
      summary: "Stripe: 1 table; Replies (mappings) and Orders (extraction)",
    });
  });

  it("sends noConnectors when only API sources and sets are chosen", () => {
    const all = facts([synced("c1", "Stripe"), source("e1", "Replies", 3)], {
      sets: [{ id: "g", name: "General" }],
      documents: [tableFile("d", "g")],
    });
    expect(
      generationSources(
        draft({
          connectorIds: new Set(),
          eventSources: picks("e1"),
          setIds: new Set(["g"]),
        }),
        all,
      ),
    ).toEqual({
      state: {
        kind: "ready",
        summary: "Replies (mappings); General: 1 table file",
      },
      request: {
        noConnectors: true,
        eventSources: [{ connectorId: "e1", mode: STRUCTURED }],
        documentSetIds: ["g"],
      },
    });
    // With no other connector, every synced one is none.
    expect(
      generationSources(
        draft({ eventSources: picks("e1") }),
        facts([source("e1", "Replies", 3)]),
      ).request,
    ).toEqual({
      noConnectors: true,
      eventSources: [{ connectorId: "e1", mode: STRUCTURED }],
      documentSetIds: [],
    });
  });

  it("counts each one as a table toward MAX_TABLES", () => {
    const wide = [
      synced("c1", "Warehouse", MAX_TABLES - 1),
      source("e1", "Replies", 3),
      source("e2", "Orders", 1),
    ];
    expect(
      generationSources(draft({ eventSources: picks("e1") }), facts(wide))
        .state,
    ).toEqual({
      kind: "ready",
      summary: `Warehouse: ${MAX_TABLES - 1} tables; Replies (mappings)`,
    });
    expect(
      generationSources(draft({ eventSources: picks("e1", "e2") }), facts(wide))
        .state,
    ).toEqual({
      kind: "blocked",
      reason: `One generation covers at most ${MAX_TABLES} tables, table files included. Choose fewer connectors or sets.`,
    });
  });

  it("blocks on a chosen one with no pieces in its table", () => {
    const all = facts([
      synced("c1", "Stripe"),
      source("e1", "Replies", 0),
      source("e2", "Orders"),
      source("e3", "Lines", 2),
    ]);
    expect(
      generationSources(draft({ eventSources: picks("e1", "e3") }), all).state,
    ).toEqual({ kind: "blocked", reason: "Replies has no pieces yet." });
    // No table yet is no pieces either; several are named together.
    expect(
      generationSources(draft({ eventSources: picks("e1", "e2") }), all).state,
    ).toEqual({
      kind: "blocked",
      reason: "Replies and Orders have no pieces yet.",
    });
    // The table caps come first.
    expect(
      generationSources(
        draft({ eventSources: picks("e1") }),
        facts([synced("c1", "Wide", MAX_TABLES), source("e1", "Replies", 0)]),
      ).state,
    ).toEqual({
      kind: "blocked",
      reason: `One generation covers at most ${MAX_TABLES} tables, table files included. Choose fewer connectors or sets.`,
    });
  });

  it("drops ids that are gone or not an API source's", () => {
    const all = facts([synced("c1", "Stripe"), source("e1", "Replies", 3)]);
    expect(
      generationSources(
        draft({ connectorIds: new Set(), eventSources: picks("gone", "c1") }),
        all,
      ),
    ).toEqual({
      state: {
        kind: "blocked",
        reason:
          "Choose a synced connector, an API source with pieces, or a set holding documents Foundry reads.",
      },
      request: { noConnectors: true, documentSetIds: [] },
    });
  });

  it("says what there is to choose from", () => {
    // Only an API source without pieces: nothing to choose yet.
    expect(
      generationSources(draft({}), facts([source("e1", "Replies", 0)])).state,
    ).toEqual({
      kind: "blocked",
      reason: "Connect your data or upload documents to continue.",
    });
    // A synced connector, unchosen, and no source with pieces.
    expect(
      generationSources(
        draft({ connectorIds: new Set() }),
        facts([synced("c1", "Stripe"), source("e1", "Replies")]),
      ).state,
    ).toEqual({
      kind: "blocked",
      reason:
        "Choose a synced connector, or a set holding documents Foundry reads.",
    });
  });

  it("never waits on an API source as on a first sync", () => {
    const live: Connector = {
      ...source("e1", "Replies"),
      lastSync: { id: "s", status: "CONNECTOR_SYNC_STATUS_RUNNING" },
    };
    expect(
      generationSources(draft({}), facts([synced("c1", "Stripe"), live])),
    ).toEqual({
      state: { kind: "ready", summary: "Stripe: 1 table" },
      request: { connectorIds: ["c1"], documentSetIds: [] },
    });
  });

  it("is loading until the connectors are read", () => {
    expect(
      generationSources(draft({ eventSources: picks("e1") }), {
        sets: [],
        documents: [],
      }).state,
    ).toEqual({ kind: "loading" });
  });
});

describe("generationSources with entity extraction", () => {
  const all = (more: { sets?: FoundryDocumentSet[] } = {}) =>
    facts([synced("c1", "Stripe"), texted("e1", "Replies")], {
      sets: [piecesOf("e1")],
      ...more,
    });

  it("follows its @example", () => {
    const replies = {
      id: "e1",
      kind: "events",
      name: "Agent replies",
      tables: [{ name: "events", rowCount: "3" }],
    };
    expect(
      generationSources(draft({ eventSources: extracting("e1") }), {
        connectors: [replies],
        sets: [{ id: "g", name: "General" }],
        documents: [tableFile("d", "g")],
      }).state,
    ).toEqual({ kind: "blocked", reason: "Agent replies has no text field." });
  });

  it("drafts from one alone, its own set as its pick, with noConnectors", () => {
    expect(
      generationSources(
        draft({ connectorIds: new Set(), eventSources: extracting("e1") }),
        all(),
      ),
    ).toEqual({
      state: { kind: "ready", summary: "Replies (extraction)" },
      request: {
        noConnectors: true,
        eventSources: [{ connectorId: "e1", mode: EXTRACTION }],
        documentSetIds: [],
      },
    });
    // Its table needn't hold rows: its documents are what it reads.
    expect(
      generationSources(
        draft({ connectorIds: new Set(), eventSources: extracting("e1") }),
        facts([texted("e1", "Replies", 0)], { sets: [piecesOf("e1")] }),
      ).state,
    ).toEqual({ kind: "ready", summary: "Replies (extraction)" });
  });

  it("reads two sources, one each way, beside a connector and a set", () => {
    const { state, request } = generationSources(
      draft({
        eventSources: new Map([
          ["e2", STRUCTURED],
          ["e1", EXTRACTION],
        ]),
        setIds: new Set(["g"]),
      }),
      facts(
        [
          synced("c1", "Stripe"),
          texted("e1", "Replies"),
          source("e2", "Orders", 2),
        ],
        {
          sets: [{ id: "g", name: "General" }, piecesOf("e1")],
          documents: [tableFile("d", "g")],
        },
      ),
    );
    expect(request).toEqual({
      connectorIds: ["c1"],
      eventSources: [
        { connectorId: "e1", mode: EXTRACTION },
        { connectorId: "e2", mode: STRUCTURED },
      ],
      documentSetIds: ["g"],
    });
    expect(state).toEqual({
      kind: "ready",
      summary:
        "Stripe: 1 table; Replies (extraction) and Orders (mappings); General: 1 table file",
    });
  });

  it("blocks on a chosen one with no text field, or no pieces as documents", () => {
    const blocked = (reason: string) => ({ kind: "blocked", reason });
    // [the sources, their sets, what the step says]
    const cases: [
      Connector[],
      FoundryDocumentSet[],
      ReturnType<typeof blocked>,
    ][] = [
      [[source("e1", "Replies", 3)], [], blocked("Replies has no text field.")],
      [
        [source("e1", "Replies", 3), source("e2", "Orders", 1)],
        [],
        blocked("Replies and Orders have no text field."),
      ],
      // No set yet, or one its sync hasn't filled (or emptied).
      [
        [texted("e1", "Replies")],
        [],
        blocked("Replies has no pieces as documents yet."),
      ],
      [
        [texted("e1", "Replies")],
        [piecesOf("e1", 0)],
        blocked("Replies has no pieces as documents yet."),
      ],
      // Another source's set is not its own.
      [
        [texted("e1", "Replies"), texted("e2", "Orders")],
        [piecesOf("e2")],
        blocked("Replies has no pieces as documents yet."),
      ],
    ];
    for (const [sources, sets, state] of cases)
      expect(
        generationSources(
          draft({ eventSources: extracting(...sources.map((c) => c.id)) }),
          facts([synced("c1", "Stripe"), ...sources], { sets }),
        ).state,
      ).toEqual(state);
  });

  it("says a structured one's missing pieces first, then text fields, then documents", () => {
    const sources = [
      source("e1", "Empty", 0),
      source("e2", "Untexted", 3),
      texted("e3", "Unsynced"),
    ];
    const state = (eventSources: Map<string, FoundryEventSourceMode>) =>
      generationSources(draft({ eventSources }), facts(sources)).state;
    const all = new Map<string, FoundryEventSourceMode>([
      ["e1", STRUCTURED],
      ["e2", EXTRACTION],
      ["e3", EXTRACTION],
    ]);
    expect(state(all)).toEqual({
      kind: "blocked",
      reason: "Empty has no pieces yet.",
    });
    all.delete("e1");
    expect(state(all)).toEqual({
      kind: "blocked",
      reason: "Untexted has no text field.",
    });
    all.delete("e2");
    expect(state(all)).toEqual({
      kind: "blocked",
      reason: "Unsynced has no pieces as documents yet.",
    });
  });

  it("counts each one as a set toward MAX_GENERATION_SETS, not as a table", () => {
    const made = Array.from(
      { length: MAX_GENERATION_SETS },
      (_, i): FoundryDocumentSet => ({
        id: `s${i}`,
        name: `Set ${i}`,
        documents: 1,
      }),
    );
    const sets = (n: number) => new Set(made.slice(0, n).map((x) => x.id));
    const state = (
      n: number,
      eventSources: Map<string, FoundryEventSourceMode>,
    ) =>
      generationSources(
        draft({ eventSources, setIds: sets(n) }),
        facts([texted("e1", "Replies")], { sets: [...made, piecesOf("e1")] }),
      ).state;
    // 19 sets and one extraction: 20.
    expect(state(MAX_GENERATION_SETS - 1, extracting("e1")).kind).toBe("ready");
    // 20 and one: 21.
    expect(state(MAX_GENERATION_SETS, extracting("e1"))).toEqual({
      kind: "blocked",
      reason: `A generation reads at most ${MAX_GENERATION_SETS} document sets, each API source read by extraction among them. Uncheck some.`,
    });
    // Read through mappings, it is a table, not a set.
    expect(state(MAX_GENERATION_SETS, picks("e1")).kind).toBe("ready");
    // A full table count leaves room for extraction.
    expect(
      generationSources(
        draft({ eventSources: extracting("e1") }),
        facts([synced("c1", "Wide", MAX_TABLES), texted("e1", "Replies")], {
          sets: [piecesOf("e1")],
        }),
      ).state,
    ).toEqual({
      kind: "ready",
      summary: `Wide: ${MAX_TABLES} tables; Replies (extraction)`,
    });
  });

  it("leaves the set choice what extraction doesn't take (setRoom)", () => {
    expect(setRoom(extracting("e1"), [{ id: "e1", kind: "events" }])).toBe(
      MAX_GENERATION_SETS - 1,
    );
    expect(setRoom(picks("e1"), [{ id: "e1", kind: "events" }])).toBe(
      MAX_GENERATION_SETS,
    );
    const sources = [texted("e1", "Replies"), texted("e2", "Orders")];
    const cases: [
      string,
      Map<string, FoundryEventSourceMode> | null,
      number,
    ][] = [
      ["none chosen yet", null, MAX_GENERATION_SETS],
      ["nothing chosen", new Map(), MAX_GENERATION_SETS],
      ["one by extraction", extracting("e1"), MAX_GENERATION_SETS - 1],
      ["two by extraction", extracting("e1", "e2"), MAX_GENERATION_SETS - 2],
      [
        "one each way",
        new Map([
          ["e1", EXTRACTION],
          ["e2", STRUCTURED],
        ]),
        MAX_GENERATION_SETS - 1,
      ],
      // Gone, or not an API source: generationSources drops it.
      ["a source that is gone", extracting("e9"), MAX_GENERATION_SETS],
      ["a connector of another kind", extracting("c1"), MAX_GENERATION_SETS],
    ];
    for (const [what, chosen, want] of cases)
      expect(setRoom(chosen, [...sources, synced("c1", "Stripe")]), what).toBe(
        want,
      );
    expect(setRoom(extracting("e1"), undefined)).toBe(MAX_GENERATION_SETS);
    // More extraction picks than sets: below 0, which generationSources
    // blocks with no set chosen.
    const many = Array.from({ length: MAX_GENERATION_SETS + 1 }, (_, i) =>
      texted(`e${i}`, `Source ${i}`),
    );
    const every = extracting(...many.map((c) => c.id));
    expect(setRoom(every, many)).toBe(-1);
    expect(
      generationSources(
        draft({ eventSources: every }),
        facts(many, { sets: many.map((c) => piecesOf(c.id)) }),
      ).state,
    ).toEqual({
      kind: "blocked",
      reason: `A generation reads at most ${MAX_GENERATION_SETS} document sets, each API source read by extraction among them. Uncheck some.`,
    });
    // A gone source's pick takes no set from a full choice.
    const made = Array.from(
      { length: MAX_GENERATION_SETS },
      (_, i): FoundryDocumentSet => ({ id: `s${i}`, documents: 1 }),
    );
    expect(
      generationSources(
        draft({
          eventSources: extracting("gone"),
          setIds: new Set(made.map((x) => x.id)),
        }),
        facts([], { sets: made, documents: [tableFile("d", "s0")] }),
      ).state.kind,
    ).toBe("ready");
  });

  it("never sends an API source's own set among the sets", () => {
    // Kept from somewhere that didn't drop it: it goes as nothing.
    expect(
      generationSources(
        draft({ setIds: new Set(["set_e1", "set_notes"]) }),
        all({ sets: [piecesOf("e1"), notes] }),
      ),
    ).toEqual({
      state: {
        kind: "ready",
        summary: "Stripe: 1 table; Team notes / Notes: 4 documents",
      },
      request: { connectorIds: ["c1"], documentSetIds: ["set_notes"] },
    });
    expect(
      generationSources(
        draft({ connectorIds: new Set(), setIds: new Set(["set_e1"]) }),
        all(),
      ).state,
    ).toEqual({
      kind: "blocked",
      reason:
        "Choose a synced connector, an API source with pieces, or a set holding documents Foundry reads.",
    });
  });

  it("is loading until the sets are read", () => {
    expect(
      generationSources(draft({ eventSources: extracting("e1") }), {
        connectors: [texted("e1", "Replies")],
        documents: [],
      }).state,
    ).toEqual({ kind: "loading" });
  });
});

describe("eventSourceReadiness", () => {
  it("follows its @example", () => {
    const replies: Connector = {
      id: "e1",
      kind: "events",
      config: { events: { textField: "reply" } },
      tables: [{ name: "events", rowCount: "3" }],
    };
    expect(eventSourceReadiness(replies, STRUCTURED, [])).toBe("ready");
    expect(eventSourceReadiness(replies, EXTRACTION, [])).toBe("documents");
    expect(
      eventSourceReadiness(replies, EXTRACTION, [
        {
          id: "p",
          documents: 3,
          connector: { connectorId: "e1", stream: "pieces" },
        },
      ]),
    ).toBe("ready");
  });

  it("says what each mode still needs", () => {
    // [source, mode, sets, readiness]
    const cases: [
      Connector,
      FoundryEventSourceMode,
      FoundryDocumentSet[] | undefined,
      ReturnType<typeof eventSourceReadiness>,
    ][] = [
      [source("e1", "R"), STRUCTURED, [], "pieces"],
      [source("e1", "R", 0), STRUCTURED, [], "pieces"],
      [source("e1", "R", 1), STRUCTURED, undefined, "ready"],
      [source("e1", "R", 3), EXTRACTION, [piecesOf("e1")], "text-field"],
      [
        { ...source("e1", "R", 3), config: { events: { textField: "" } } },
        EXTRACTION,
        [piecesOf("e1")],
        "text-field",
      ],
      [{ id: "e1", kind: "events" }, EXTRACTION, [], "text-field"],
      [texted("e1", "R"), EXTRACTION, undefined, "documents"],
      [texted("e1", "R"), EXTRACTION, [piecesOf("e1", 0)], "documents"],
      [texted("e1", "R"), EXTRACTION, [piecesOf("e2")], "documents"],
      // Granola's notes are no API source's pieces.
      [
        texted("e1", "R"),
        EXTRACTION,
        [{ ...notes, connector: { connectorId: "e1", stream: "notes" } }],
        "documents",
      ],
      [texted("e1", "R"), EXTRACTION, [piecesOf("e1", 1)], "ready"],
      [texted("e1", "R", 0), EXTRACTION, [piecesOf("e1", 1)], "ready"],
    ];
    for (const [c, mode, sets, want] of cases)
      expect(
        eventSourceReadiness(c, mode, sets),
        JSON.stringify([c.config, c.tables, mode, sets]),
      ).toBe(want);
  });
});

describe("isEventSourceSet", () => {
  it("follows its @example", () => {
    expect(
      isEventSourceSet({
        id: "p",
        connector: {
          connectorId: "e1",
          connectorKind: "events",
          stream: "pieces",
        },
      }),
    ).toBe(true);
    expect(
      isEventSourceSet({
        id: "n",
        connector: { connectorId: "c", connectorKind: "granola" },
      }),
    ).toBe(false);
  });

  it("is only a set an API source's syncs write", () => {
    expect(isEventSourceSet({ id: "g", name: "General" })).toBe(false);
    expect(isEventSourceSet({ id: "g", connector: null })).toBe(false);
    expect(
      isEventSourceSet({ id: "x", connector: { connectorId: "e1" } }),
    ).toBe(false);
    expect(isEventSourceSet(piecesOf("e1"))).toBe(true);
  });
});

describe("pickerSets and startingSets", () => {
  const general: FoundryDocumentSet = { id: "g", name: "General" };
  const connectors: Connector[] = [
    { id: "gr", kind: "granola", name: "Team notes" },
    texted("e1", "Replies"),
  ];

  it("never lists an API source's own set", () => {
    const picker = pickerSets([general, piecesOf("e1"), notes], connectors);
    expect(picker.made).toEqual([general]);
    expect(
      picker.owned.map((o) => [o.connectorId, o.sets.map((x) => x.id)]),
    ).toEqual([["gr", ["set_notes"]]]);
    expect(picker.ordered.map((x) => x.id)).toEqual(["g", "set_notes"]);
    // A workspace whose only set is a source's lists none.
    expect(pickerSets([piecesOf("e1")], connectors)).toEqual({
      made: [],
      owned: [],
      ordered: [],
    });
  });

  it("never starts with one, from a link or the current version's scope", () => {
    const sets = [general, piecesOf("e1"), notes];
    expect(startingSets(null, ["set_e1", "g", "set_notes"], sets)).toEqual([
      "g",
      "set_notes",
    ]);
    expect(startingSets("set_e1,g", undefined, sets)).toEqual(["g"]);
    expect(startingSets("set_e1", ["g"], sets)).toEqual([]);
    // Dropped before the cap is counted.
    expect(startingSets("set_e1,g", undefined, sets, 1)).toEqual(["g"]);
  });
});

describe("generationPicks", () => {
  const connectors: Connector[] = [
    { id: "c1", kind: "stripe" },
    { id: "e1", kind: "events" },
    { id: "e2", kind: "events" },
  ];
  const sets = [{ id: "g" }, piecesOf("e2"), notes];
  const tables = (...ids: string[]) =>
    ids.map((connectorId) => ({ connectorId, table: "t" }));

  it("follows its @example", () => {
    expect(
      generationPicks(
        {
          tables: [
            { connectorId: "c1", table: "charges" },
            { connectorId: "c1", table: "refunds" },
            { connectorId: "e1", table: "events" },
          ],
          documentSetIds: ["g", "p"],
        },
        connectors,
        [
          { id: "g" },
          {
            id: "p",
            connector: {
              connectorId: "e2",
              connectorKind: "events",
              stream: "pieces",
            },
          },
        ],
      ),
    ).toEqual({
      connectorIds: ["c1"],
      eventSources: [
        { connectorId: "e1", mode: STRUCTURED },
        { connectorId: "e2", mode: EXTRACTION },
      ],
      documentSetIds: ["g"],
    });
  });

  it("turns an API source's own set into its extraction pick", () => {
    // [what it read, what it asks for again]
    const cases: [
      Parameters<typeof generationPicks>[0],
      ReturnType<typeof generationPicks>,
    ][] = [
      [
        { documentSetIds: ["set_e2"] },
        {
          connectorIds: [],
          eventSources: [{ connectorId: "e2", mode: EXTRACTION }],
          documentSetIds: [],
        },
      ],
      [
        { tables: tables("c1"), documentSetIds: ["set_notes", "set_e2", "g"] },
        {
          connectorIds: ["c1"],
          eventSources: [{ connectorId: "e2", mode: EXTRACTION }],
          documentSetIds: ["set_notes", "g"],
        },
      ],
      // A set the workspace no longer has stays a set (the step drops it).
      [
        { documentSetIds: ["gone"] },
        { connectorIds: [], eventSources: [], documentSetIds: ["gone"] },
      ],
      [undefined, { connectorIds: [], eventSources: [], documentSetIds: [] }],
      [{}, { connectorIds: [], eventSources: [], documentSetIds: [] }],
    ];
    for (const [g, want] of cases)
      expect(generationPicks(g, connectors, sets)).toEqual(want);
    // Before the sets are read, it counts as a set.
    expect(
      generationPicks({ documentSetIds: ["set_e2"] }, connectors, []),
    ).toEqual({
      connectorIds: [],
      eventSources: [],
      documentSetIds: ["set_e2"],
    });
  });

  it("keeps a deleted connector a connector, and reads nothing from none", () => {
    expect(
      generationPicks(
        {
          tables: [
            { connectorId: "gone", table: "t" },
            { connectorId: "e1", table: "events" },
            { table: "orphan" },
          ],
        },
        connectors,
        sets,
      ),
    ).toEqual({
      connectorIds: ["gone"],
      eventSources: [{ connectorId: "e1", mode: STRUCTURED }],
      documentSetIds: [],
    });
    // Before the connectors are read, an API source counts as a connector.
    expect(
      generationPicks(
        { tables: [{ connectorId: "e1", table: "events" }] },
        [],
        [],
      ),
    ).toEqual({ connectorIds: ["e1"], eventSources: [], documentSetIds: [] });
  });

  it("is what Try again links to: its API sources apart, none for none", () => {
    const link = (g: Parameters<typeof generationPicks>[0]) =>
      new URL(
        "https://foundry.example.com" +
          ontologyPaths.generate("o1", {
            goal: "Trace orders",
            ...generationPicks(g, connectors, sets),
          }),
      ).searchParams;
    const both = link({
      tables: [
        { connectorId: "c1", table: "charges" },
        { connectorId: "e1", table: "events" },
      ],
      documentSetIds: ["g", "set_e2"],
    });
    expect(both.get("connectors")).toBe("c1");
    expect(both.get("event_sources")).toBe("e1:structured,e2:extraction");
    expect(both.get("sets")).toBe("g");
    // Without one, the link says none, so Generate doesn't start from the
    // current version's.
    const none = link({ tables: [{ connectorId: "c1", table: "charges" }] });
    expect(none.get("event_sources")).toBe("");
    expect(none.get("sets")).toBe("");
    // Generate reads the link back as the generation read.
    expect(
      startingEventSources(
        both.get("event_sources"),
        undefined,
        connectors,
        sets,
      ),
    ).toEqual(
      new Map([
        ["e1", STRUCTURED],
        ["e2", EXTRACTION],
      ]),
    );
    expect(startingSets(both.get("sets"), undefined, sets)).toEqual(["g"]);
  });
});

describe("startingEventSources", () => {
  const connectors: Connector[] = [
    { id: "c1", kind: "stripe" },
    { id: "e1", kind: "events" },
    { id: "e2", kind: "events" },
    { id: "a:b", kind: "events" },
  ];

  it("follows its @example", () => {
    const sets = [
      { id: "p", connector: { connectorId: "e1", connectorKind: "events" } },
    ];
    expect(
      startingEventSources(
        "e1:structured,c1:structured,gone:structured",
        undefined,
        connectors,
        [],
      ),
    ).toEqual(new Map([["e1", STRUCTURED]]));
    expect(
      startingEventSources(
        null,
        { tables: [{ connectorId: "e1", table: "events" }] },
        connectors,
        [],
      ),
    ).toEqual(new Map([["e1", STRUCTURED]]));
    expect(
      startingEventSources(null, { documentSetIds: ["p"] }, connectors, sets),
    ).toEqual(new Map([["e1", EXTRACTION]]));
    expect(
      startingEventSources(
        "",
        { tables: [{ connectorId: "e1", table: "events" }] },
        connectors,
        [],
      ),
    ).toEqual(new Map());
  });

  it("starts from the current version's generation without a link", () => {
    expect(
      startingEventSources(
        null,
        {
          tables: [
            { connectorId: "c1", table: "charges" },
            { connectorId: "e2", table: "events" },
            { connectorId: "gone", table: "events" },
          ],
          documentSetIds: ["set_a:b", "set_gone"],
        },
        connectors,
        [piecesOf("a:b"), piecesOf("gone")],
      ),
    ).toEqual(
      new Map([
        ["e2", STRUCTURED],
        ["a:b", EXTRACTION],
      ]),
    );
    // No current version, or one no generation drafted: none.
    expect(startingEventSources(null, undefined, connectors, [])).toEqual(
      new Map(),
    );
  });

  it("reads a link's modes, dropping what it can't spell", () => {
    expect(
      startingEventSources(
        "e2:extraction,e1:structured,e1:extraction,a:b:structured,e1,:structured,e2:unspecified,e1:Structured,e2:Extraction",
        undefined,
        connectors,
        [],
      ),
    ).toEqual(
      new Map([
        ["e2", EXTRACTION],
        ["e1", STRUCTURED],
        ["a:b", STRUCTURED],
      ]),
    );
  });

  it("reads back what paths.generate writes", () => {
    const sent = [
      { connectorId: "e1", mode: STRUCTURED },
      { connectorId: "a:b", mode: EXTRACTION },
      { connectorId: "e2", mode: EXTRACTION },
    ];
    const param = (eventSources?: typeof sent) =>
      new URL(
        "https://foundry.example.com" +
          ontologyPaths.generate("o1", { eventSources }),
      ).searchParams.get("event_sources");
    expect(param(sent)).toBe("e1:structured,a:b:extraction,e2:extraction");
    expect(
      startingEventSources(param(sent), undefined, connectors, []),
    ).toEqual(new Map(sent.map((p) => [p.connectorId, p.mode])));
    expect(param([])).toBe("");
    expect(
      startingEventSources(
        param([]),
        { tables: [{ connectorId: "e1", table: "events" }] },
        connectors,
        [],
      ),
    ).toEqual(new Map());
    // Left out, the link names none, and Generate starts from the version.
    expect(param(undefined)).toBeNull();
    expect(ontologyPaths.generate("o1", { goal: "x" })).toBe(
      "/ontology/o1/generate?goal=x",
    );
  });
});
