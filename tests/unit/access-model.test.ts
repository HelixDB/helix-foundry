import { describe, expect, it } from "vitest";
import type * as wfe from "@/shared/wfe";
import {
  keyParam,
  addsSources,
  audienceRead,
  listNames,
  covered,
  EVERYTHING,
  everythingNote,
  grantKey,
  holdsEverything,
  SLACK_GRANT_NOTE,
  grantOf,
  granteeRedirect,
  grantsOf,
  heldGrants,
  leftOutBy,
  leftOutText,
  lossLines,
  missing,
  missingText,
  principalKey,
  holdsAny,
  readAccess,
  readsPart,
  readsText,
  readings,
  newlyRestricted,
  restrictable,
  restrictedHoldingNothing,
  setOwners,
  MAX_HELD_ITEMS,
  sourcePermissions,
  sourceState,
  sourceTree,
  syncedFrom,
  targetOf,
  targetsOf,
  toggle,
  workflowsOffBy,
  workflowsOn,
  type Grant,
} from "@/web/areas/workspace/access-model";
import { workspacePaths } from "@/web/areas/workspace/paths";

// Source grants as Settings › Access and restricted mode read them: grants
// to and from the WFE's targets, the editor's sources, what a load lacks,
// who reads what, losses in words, who can be restricted, and where a
// grantee may go.

const stripe: Grant = { kind: "connector", connectorId: "conn_stripe" };
const invoices: Grant = {
  kind: "table",
  connectorId: "conn_stripe",
  table: "invoices",
};
const general: Grant = { kind: "set", setId: "set_general" };
const notes: Grant = { kind: "set", setId: "set_notes" };
const enrichment: Grant = { kind: "enrichment" };
const eve: wfe.FoundryPrincipal = { kind: "KIND_USER", id: "usr_eve" };

describe("grants and targets", () => {
  it("reads each target kind and writes it back", () => {
    for (const g of [EVERYTHING, stripe, invoices, general, enrichment])
      expect(grantOf(targetOf(g))).toEqual(g);
    expect(targetOf(EVERYTHING)).toEqual({ kind: "KIND_WORKSPACE" });
    expect(grantKey(invoices)).toBe("table:conn_stripe:invoices");
    expect(grantKey(enrichment)).toBe("enrichment");
    expect(grantKey(EVERYTHING)).toBe("workspace");
    // The WFE fills the ids it doesn't name with "".
    expect(
      grantOf({
        kind: "KIND_WORKSPACE",
        connectorId: "",
        table: "",
        documentSetId: "",
      }),
    ).toEqual(EVERYTHING);
  });

  it("drops a target this build doesn't know or that misses what it names", () => {
    for (const t of [
      { kind: "KIND_UNSPECIFIED" },
      { kind: "KIND_CONNECTOR" },
      { kind: "KIND_TABLE", connectorId: "c" },
      { kind: "KIND_DOCUMENT_SET", documentSetId: "" },
      // Everything names nothing.
      { kind: "KIND_WORKSPACE", connectorId: "c" },
      { kind: "KIND_WORKSPACE", table: "t" },
      { kind: "KIND_WORKSPACE", documentSetId: "s" },
      { kind: "KIND_LATER" },
      {},
    ] as wfe.FoundryAccessTarget[])
      expect(grantOf(t)).toBeUndefined();
    // Ids the WFE fills with "" for the kinds that don't name them.
    expect(
      grantOf({
        kind: "KIND_CONNECTOR",
        connectorId: "c",
        table: "",
        documentSetId: "",
      }),
    ).toEqual({ kind: "connector", connectorId: "c" });
  });

  it("picks one principal's grants, once each, and a member's teams' too", () => {
    const grants: wfe.FoundryAccessGrant[] = [
      { principal: eve, target: targetOf(general) },
      {
        principal: { kind: "KIND_TEAM", id: "team_1" },
        target: targetOf(stripe),
      },
      { principal: eve, target: targetOf(general) },
      { principal: eve, target: { kind: "KIND_LATER" as never } },
      {
        principal: { kind: "KIND_USER", id: "usr_alan" },
        target: targetOf(notes),
      },
      { target: targetOf(notes) },
    ];
    expect(grantsOf(grants, { kind: "KIND_USER", id: "usr_eve" })).toEqual([
      general,
    ]);
    const access: wfe.ListFoundryAccessResponse = {
      grants,
      teams: [{ id: "team_1", memberUserIds: ["usr_eve"] }, { id: "team_2" }],
    };
    expect(heldGrants(access, { kind: "KIND_USER", id: "usr_eve" })).toEqual([
      general,
      stripe,
    ]);
    expect(heldGrants(access, { kind: "KIND_TEAM", id: "team_1" })).toEqual([
      stripe,
    ]);
    expect(
      heldGrants({}, { kind: "KIND_SERVICE_CREDENTIAL", id: "k" }),
    ).toEqual([]);
    expect(principalKey({})).toBe("KIND_UNSPECIFIED:");
  });

  it("saves each target once, leaving out what a connection's grant covers", () => {
    const owners = new Map([["set_notes", "conn_stripe"]]);
    expect(
      targetsOf(
        [stripe, invoices, notes, general, enrichment, general],
        owners,
      ),
    ).toEqual([targetOf(stripe), targetOf(general), targetOf(enrichment)]);
    expect(targetsOf([invoices, notes], owners)).toEqual([
      targetOf(invoices),
      targetOf(notes),
    ]);
  });

  it("saves everything in the workspace alone, with People enrichment if chosen", () => {
    const owners = new Map<string, string>();
    expect(
      targetsOf(
        [stripe, EVERYTHING, invoices, general, enrichment, enrichment],
        owners,
      ),
    ).toEqual([{ kind: "KIND_WORKSPACE" }, { kind: "KIND_ENRICHMENT" }]);
    expect(targetsOf([EVERYTHING, EVERYTHING], owners)).toEqual([
      { kind: "KIND_WORKSPACE" },
    ]);
    expect(holdsEverything([stripe, EVERYTHING])).toBe(true);
    expect(holdsEverything([stripe, enrichment])).toBe(false);
    expect(holdsEverything([])).toBe(false);
  });
});

describe("the editor's sources", () => {
  const sets: wfe.FoundryDocumentSet[] = [
    { id: "set_general", name: "General" },
    {
      id: "set_notes",
      name: "Notes",
      connector: { connectorId: "conn_granola", connectorName: "Granola" },
    },
    { id: "set_orphan", connector: { connectorId: "conn_gone" } },
  ];
  it("lists connections with their tables and sets, then the other sets", () => {
    expect(
      sourceTree(
        [
          {
            id: "conn_stripe",
            kind: "stripe",
            name: "Stripe",
            tables: [
              { name: "customers" },
              { name: "invoices" },
            ] as wfe.SyncedTable[],
          },
          { id: "conn_granola", kind: "granola" },
        ],
        sets,
      ),
    ).toEqual({
      connections: [
        {
          id: "conn_stripe",
          name: "Stripe",
          kind: "stripe",
          tables: ["customers", "invoices"],
          sets: [],
        },
        {
          id: "conn_granola",
          name: "granola",
          kind: "granola",
          tables: [],
          sets: [{ id: "set_notes", name: "Notes" }],
        },
      ],
      sets: [
        { id: "set_general", name: "General" },
        { id: "set_orphan", name: "set_orphan" },
      ],
    });
    expect(setOwners(sets)).toEqual(
      new Map([
        ["set_notes", "conn_granola"],
        ["set_orphan", "conn_gone"],
      ]),
    );
  });

  it("shows a source chosen, covered by its connection, or off, and toggles it", () => {
    expect(sourceState([stripe], invoices)).toBe("covered");
    expect(sourceState([stripe], notes, "conn_stripe")).toBe("covered");
    expect(sourceState([stripe], stripe)).toBe("chosen");
    expect(sourceState([invoices], invoices)).toBe("chosen");
    expect(sourceState([], general)).toBe("off");
    expect(sourceState([], enrichment)).toBe("off");
    expect(toggle([general], stripe)).toEqual([general, stripe]);
    expect(toggle([general, stripe], general)).toEqual([stripe]);
  });

  it("replaces everything with a source chosen, and the sources with everything", () => {
    // Everything leaves each source off, to be chosen in its place.
    expect(sourceState([EVERYTHING], EVERYTHING)).toBe("chosen");
    expect(sourceState([], EVERYTHING)).toBe("off");
    expect(sourceState([EVERYTHING], stripe)).toBe("off");
    expect(sourceState([EVERYTHING], invoices)).toBe("off");
    expect(sourceState([EVERYTHING], notes, "conn_stripe")).toBe("off");
    expect(sourceState([EVERYTHING], enrichment)).toBe("off");
    // Choosing everything drops connectors, tables and sets, not enrichment.
    expect(toggle([stripe, invoices, general, enrichment], EVERYTHING)).toEqual(
      [enrichment, EVERYTHING],
    );
    expect(toggle([], EVERYTHING)).toEqual([EVERYTHING]);
    // Choosing a connector, table or set drops everything.
    expect(toggle([EVERYTHING, enrichment], stripe)).toEqual([
      enrichment,
      stripe,
    ]);
    expect(toggle([EVERYTHING], invoices)).toEqual([invoices]);
    expect(toggle([EVERYTHING], general)).toEqual([general]);
    // People enrichment goes beside it either way.
    expect(toggle([EVERYTHING], enrichment)).toEqual([EVERYTHING, enrichment]);
    expect(toggle([EVERYTHING, enrichment], enrichment)).toEqual([EVERYTHING]);
    // Turning everything off leaves People enrichment alone.
    expect(toggle([EVERYTHING, enrichment], EVERYTHING)).toEqual([enrichment]);
  });

  it("says what everything gives, to people, to a key and to an app", () => {
    expect(everythingNote("people")).toContain("including ones added later");
    expect(everythingNote("people")).toContain(
      "the channels each person is in",
    );
    // A key, like an app, reads only what a source shares with everyone.
    for (const audience of ["key", "app"] as const) {
      expect(everythingNote(audience)).toContain(
        "what the source shares with everyone",
      );
      expect(everythingNote(audience)).not.toContain("each person");
    }
    for (const audience of ["people", "key", "app"] as const)
      expect(everythingNote(audience)).toContain("not People enrichment");
  });
});

describe("coverage", () => {
  const inputs: wfe.FoundryLoadAccessInputs = {
    tables: [
      { connectorId: "conn_stripe", table: "customers" },
      { connectorId: "conn_stripe", table: "invoices" },
    ],
    documentSets: [
      { documentSetId: "set_general", connectorId: "" },
      { documentSetId: "set_notes", connectorId: "conn_granola" },
    ],
    enrichment: true,
  };
  const none = { tables: [], sets: [], enrichment: false, uncoverable: false };

  it("lists what grants leave out, as the backend's Grants.Missing", () => {
    expect(missing([], inputs)).toEqual({
      tables: [
        { connectorId: "conn_stripe", table: "customers" },
        { connectorId: "conn_stripe", table: "invoices" },
      ],
      sets: [
        { setId: "set_general", connectorId: "" },
        { setId: "set_notes", connectorId: "conn_granola" },
      ],
      enrichment: true,
      uncoverable: false,
    });
    // A table by itself or its connector; a set by itself or its owner.
    expect(
      missing(
        [invoices, general, { kind: "connector", connectorId: "conn_granola" }],
        inputs,
      ),
    ).toEqual({
      ...none,
      tables: [{ connectorId: "conn_stripe", table: "customers" }],
      enrichment: true,
    });
    const all = missing([stripe, general, notes, enrichment], inputs);
    expect(all).toEqual(none);
    expect(covered(all)).toBe(true);
    expect(covered(missing([], {}))).toBe(true);
    // A document in no set, and a load nothing covers.
    expect(
      missing([stripe], { documentSets: [{ documentSetId: "" }] }).sets,
    ).toEqual([{ setId: "", connectorId: "" }]);
    expect(covered(missing([stripe], { uncoverable: true }))).toBe(false);
  });

  it("gives every table and set with everything, never People enrichment", () => {
    // Any connector's tables and sets, uploaded or a connection's, ones it
    // has never seen included.
    expect(missing([EVERYTHING], inputs)).toEqual({
      ...none,
      enrichment: true,
    });
    expect(covered(missing([EVERYTHING, enrichment], inputs))).toBe(true);
    expect(
      missing([EVERYTHING], {
        tables: [{ connectorId: "conn_added_later", table: "t" }],
        documentSets: [
          { documentSetId: "set_added_later", connectorId: "conn_added_later" },
        ],
      }),
    ).toEqual(none);
    // A document in no set, and a load nothing covers, stay uncovered.
    expect(
      missing([EVERYTHING], { documentSets: [{ documentSetId: "" }] }).sets,
    ).toEqual([{ setId: "", connectorId: "" }]);
    expect(covered(missing([EVERYTHING], { uncoverable: true }))).toBe(false);
    expect(holdsAny([EVERYTHING], { uncoverable: true, tables: [{}] })).toBe(
      false,
    );
    // An enrichment load is read in part.
    expect(holdsAny([EVERYTHING], inputs)).toBe(true);
    expect(holdsAny([EVERYTHING], { enrichment: true })).toBe(false);
    expect(
      readings({ ontologies: [{ ontologyId: "o", inputs }] }, [EVERYTHING]).map(
        (r) => r.reach,
      ),
    ).toEqual(["part"]);
  });

  it("says what is missing in words", () => {
    const names = {
      connection: (id: string) => ({ conn_stripe: "Stripe" })[id] || id,
      set: (id: string) => ({ set_notes: "Granola / Notes" })[id] || id,
    };
    expect(
      missingText(
        {
          tables: [{ connectorId: "conn_stripe", table: "invoices" }],
          sets: [
            { setId: "set_notes", connectorId: "conn_granola" },
            { setId: "", connectorId: "" },
          ],
          enrichment: true,
          uncoverable: false,
        },
        names,
      ),
    ).toEqual([
      "Stripe › invoices",
      "Granola / Notes",
      "A document in no set",
      "People enrichment",
    ]);
    expect(
      missingText({ ...none, uncoverable: true, enrichment: true }, names),
    ).toEqual(["Load again to share"]);
  });

  it("puts the ontologies grants cover first", () => {
    const access: wfe.ListFoundryAccessResponse = {
      ontologies: [
        {
          ontologyId: "o_sales",
          inputs: { tables: [{ connectorId: "conn_shop", table: "people" }] },
        },
        { ontologyId: "o_contracts", inputs },
        { ontologyId: "o_empty" },
      ],
    };
    expect(
      readings(access, [stripe, general, notes, enrichment]).map((r) => [
        r.ontologyId,
        covered(r.missing),
      ]),
    ).toEqual([
      ["o_contracts", true],
      ["o_empty", true],
      ["o_sales", false],
    ]);
    expect(readings({}, [])).toEqual([]);
  });
});

describe("partial access", () => {
  const principal = { kind: "KIND_USER" as const, id: "usr_eve" };
  const both: wfe.FoundryLoadAccessInputs = {
    tables: [{ connectorId: "conn_stripe", table: "customers" }],
    documentSets: [{ documentSetId: "set_1" }],
  };
  it("tells what grants hold anything of", () => {
    const set = { kind: "set" as const, setId: "set_1" };
    expect(holdsAny([set], both)).toBe(true);
    expect(
      holdsAny([{ kind: "connector", connectorId: "conn_stripe" }], both),
    ).toBe(true);
    expect(holdsAny([{ kind: "set", setId: "other" }], both)).toBe(false);
    expect(holdsAny([], both)).toBe(false);
    expect(holdsAny([{ kind: "enrichment" }], { enrichment: true })).toBe(true);
    expect(holdsAny([{ kind: "enrichment" }], {})).toBe(false);
    // What nothing covers shares nothing.
    expect(holdsAny([set], { ...both, uncoverable: true })).toBe(false);
  });
  it("says what each ontology gives: all, part or nothing", () => {
    const access: wfe.ListFoundryAccessResponse = {
      ontologies: [
        {
          ontologyId: "none",
          inputs: { documentSets: [{ documentSetId: "x" }] },
        },
        { ontologyId: "part", inputs: both },
        {
          ontologyId: "all",
          inputs: { documentSets: [{ documentSetId: "set_1" }] },
        },
        {
          ontologyId: "told",
          inputs: { ...both, uncoverable: true },
          partialReaders: [principal],
        },
      ],
    };
    const grants = [{ kind: "set" as const, setId: "set_1" }];
    expect(
      readings(access, grants, principal).map((r) => [r.ontologyId, r.reach]),
    ).toEqual([
      ["all", "all"],
      ["part", "part"],
      ["told", "part"],
      ["none", "none"],
    ]);
    // Without the WFE's word for it, what nothing covers is nothing.
    expect(
      readings(access, grants).find((r) => r.ontologyId === "told")?.reach,
    ).toBe("none");
    expect(readsText(readings(access, grants, principal), (o) => o)).toBe(
      "Reads all, part of part, part of told",
    );
    expect(readsText([], (o) => o)).toBe("Reads nothing yet");
  });
  it("reads part only through grants that hold part", () => {
    const o = { id: "o", projectId: "p" };
    expect(
      readsPart({ grantee: true, granted: ["o"], partial: ["o"] }, o),
    ).toBe(true);
    expect(readsPart({ grantee: true, granted: ["o"], partial: [] }, o)).toBe(
      false,
    );
    // Whoever queries the database reads it all, whatever grants say.
    expect(
      readsPart(
        {
          grantee: false,
          granted: ["o"],
          partial: ["o"],
          queryRead: { mode: "all" },
        },
        o,
      ),
    ).toBe(false);
    expect(
      readsPart(
        { grantee: false, granted: ["o"], partial: ["o"], queryRead: {} },
        o,
      ),
    ).toBe(true);
  });
});

describe("losses", () => {
  it("says who keeps part of what they lose", () => {
    expect(
      lossLines(
        [
          { principal: eve, ontologyId: "o_1", keepsPart: true },
          { principal: eve, ontologyId: "o_2" },
        ],
        () => "Eve Adler",
        (id) => id,
      ),
    ).toEqual(["Eve Adler: o_1 (keeps the part their sources hold), o_2"]);
  });

  it("says who stops reading what, one line per principal", () => {
    const team: wfe.FoundryPrincipal = { kind: "KIND_TEAM", id: "team_1" };
    expect(
      lossLines(
        [
          { principal: eve, ontologyId: "o_1" },
          { principal: team, ontologyId: "o_1" },
          { principal: eve, ontologyId: "o_2" },
          { principal: eve, ontologyId: "o_2" },
          {},
        ],
        (p) =>
          ({ usr_eve: "Eve Adler", team_1: "Sales" })[p.id || ""] || "Someone",
        (id) =>
          ({ o_1: "Customer contracts", o_2: "Renewals" })[id] || "An ontology",
      ),
    ).toEqual([
      "Eve Adler: Customer contracts, Renewals",
      "Sales: Customer contracts",
      "Someone: An ontology",
    ]);
  });
});

describe("restricting members", () => {
  const member = (
    role: wfe.WorkspaceRole,
    projectAccess?: wfe.ProjectAccess,
  ): wfe.WorkspaceMember => ({ workspaceId: "ws", role, projectAccess });
  it("names who saving sources would restrict", () => {
    const person = (
      id: string,
      role: wfe.WorkspaceRole,
    ): wfe.WorkspaceMember => ({
      workspaceId: "ws",
      role,
      user: { id },
    });
    const people = [
      person("eve", "WORKSPACE_ROLE_MEMBER"),
      person("alan", "WORKSPACE_ROLE_MEMBER"),
      person("ada", "WORKSPACE_ROLE_OWNER"),
    ];
    const access: wfe.ListFoundryAccessResponse = {
      restrictedUserIds: ["eve"],
      teams: [{ id: "t", memberUserIds: ["eve", "alan", "ada"] }],
    };
    const ids = (p: Parameters<typeof newlyRestricted>[2]) =>
      newlyRestricted(access, people, p).map((m) => m.user?.id);
    expect(ids({ kind: "KIND_USER", id: "alan" })).toEqual(["alan"]);
    expect(ids({ kind: "KIND_USER", id: "eve" })).toEqual([]);
    expect(ids({ kind: "KIND_USER", id: "ada" })).toEqual([]);
    expect(ids({ kind: "KIND_TEAM", id: "t" })).toEqual(["alan"]);
    expect(ids({ kind: "KIND_TEAM", id: "gone" })).toEqual([]);
    expect(ids({ kind: "KIND_SERVICE_CREDENTIAL", id: "alan" })).toEqual([]);
  });
  it("restricts only a plain member reaching no database outside Foundry", () => {
    expect(restrictable(member("WORKSPACE_ROLE_OWNER"))).toEqual({
      ok: false,
      why: "Owners and admins see everything.",
    });
    expect(restrictable(member("WORKSPACE_ROLE_ADMIN")).ok).toBe(false);
    expect(restrictable(member("WORKSPACE_ROLE_MEMBER")).ok).toBe(true);
    expect(
      restrictable(
        member("WORKSPACE_ROLE_MEMBER", {
          read: { mode: "all" },
          write: { mode: "none" },
          queryRead: { mode: "none" },
          queryWrite: {},
        }),
      ).ok,
    ).toBe(true);
    for (const reach of ["write", "queryRead", "queryWrite"] as const)
      expect(
        restrictable(
          member("WORKSPACE_ROLE_MEMBER", {
            [reach]: { mode: "selected", projectIds: ["p"] },
          }),
        ),
      ).toMatchObject({
        ok: false,
        why: expect.stringMatching(/^Writes a project/),
      });
  });
});

describe("teams synced from a directory", () => {
  const RAW: wfe.ProjectAccess = { queryRead: { mode: "all" } };
  const person = (
    id: string,
    role: wfe.WorkspaceRole,
    projectAccess?: wfe.ProjectAccess,
  ): wfe.WorkspaceMember => ({
    workspaceId: "ws",
    role,
    user: { id, displayName: id },
    projectAccess,
  });
  const people = [
    person("eve", "WORKSPACE_ROLE_MEMBER"),
    person("grace", "WORKSPACE_ROLE_MEMBER"),
    person("alan", "WORKSPACE_ROLE_MEMBER", RAW),
    person("ada", "WORKSPACE_ROLE_OWNER", RAW),
    person("kat", "WORKSPACE_ROLE_ADMIN", RAW),
  ];
  const directory = { directoryId: "d", groupId: "g", provider: "Okta" };
  const access: wfe.ListFoundryAccessResponse = {
    restrictedUserIds: ["eve"],
    teams: [
      {
        id: "synced",
        memberUserIds: ["eve", "grace", "alan", "ada", "kat"],
        directory,
      },
      { id: "manual", memberUserIds: ["grace", "alan"] },
    ],
  };
  const team = (id: string) => ({ kind: "KIND_TEAM" as const, id });
  const ids = (ms: readonly wfe.WorkspaceMember[]) => ms.map((m) => m.user?.id);

  it("names its directory, else says your directory; a team made by hand has none", () => {
    expect(syncedFrom({ directory })).toBe("Okta");
    expect(syncedFrom({ directory: { groupId: "g" } })).toBe("your directory");
    expect(syncedFrom({ name: "Finance" })).toBeUndefined();
  });

  it("leaves out its plain members with raw access instead of restricting them", () => {
    expect(ids(leftOutBy(access, people, team("synced")))).toEqual(["alan"]);
    // Owners and admins stay on it as they are; Eve is restricted already.
    expect(ids(newlyRestricted(access, people, team("synced")))).toEqual([
      "grace",
    ]);
    // A team made by hand leaves out no one: the WFE refuses its grants.
    expect(leftOutBy(access, people, team("manual"))).toEqual([]);
    expect(ids(newlyRestricted(access, people, team("manual")))).toEqual([
      "grace",
      "alan",
    ]);
    expect(
      leftOutBy(access, people, { kind: "KIND_USER", id: "alan" }),
    ).toEqual([]);
    expect(leftOutBy(access, people, team("gone"))).toEqual([]);
  });

  it("says whom it left out", () => {
    expect(leftOutText([])).toBe("");
    expect(leftOutText(["Alan Turing"])).toBe(
      "Left out: Alan Turing, who writes a project or queries its databases outside Foundry, so the team’s sources can’t confine them.",
    );
    expect(leftOutText(["Alan Turing", "Grace Hopper"])).toMatch(
      /^Left out: Alan Turing, Grace Hopper, who write a project or query its/,
    );
  });

  it("checks the grantee only for sources that are new", () => {
    expect(addsSources([], [general])).toBe(true);
    expect(addsSources([general, stripe], [general])).toBe(false);
    expect(addsSources([general], [])).toBe(false);
    // A table its saved connector covers is nothing new; another set is.
    expect(addsSources([stripe], [invoices])).toBe(false);
    expect(addsSources([invoices], [stripe])).toBe(true);
    expect(addsSources([general], [general, notes])).toBe(true);
    // Everything holds every connector, table and set, never enrichment.
    expect(addsSources([EVERYTHING], [stripe, invoices, general])).toBe(false);
    expect(addsSources([EVERYTHING], [EVERYTHING])).toBe(false);
    expect(addsSources([EVERYTHING], [enrichment])).toBe(true);
    expect(addsSources([stripe, general], [EVERYTHING])).toBe(true);
  });
});

describe("restricted members holding nothing", () => {
  const person = (
    id: string,
    role: wfe.WorkspaceRole = "WORKSPACE_ROLE_MEMBER",
  ): wfe.WorkspaceMember => ({ workspaceId: "ws", role, user: { id } });
  const people = [
    person("eve"),
    person("grace"),
    person("alan"),
    person("bea"),
    person("ada", "WORKSPACE_ROLE_ADMIN"),
  ];
  const held = (principal: wfe.FoundryPrincipal): wfe.FoundryAccessGrant => ({
    principal,
    target: { kind: "KIND_DOCUMENT_SET", documentSetId: "set_general" },
  });

  it("lists restricted plain members with no sources of their own or a team's", () => {
    const access: wfe.ListFoundryAccessResponse = {
      // Ada was restricted as a member, and is an admin now.
      restrictedUserIds: ["eve", "grace", "alan", "ada"],
      grants: [
        held({ kind: "KIND_USER", id: "eve" }),
        held({ kind: "KIND_TEAM", id: "t" }),
      ],
      teams: [
        { id: "t", memberUserIds: ["grace"] },
        { id: "empty", memberUserIds: ["alan"] },
      ],
    };
    expect(
      restrictedHoldingNothing(access, people).map((m) => m.user?.id),
    ).toEqual(["alan"]);
    expect(restrictedHoldingNothing({}, people)).toEqual([]);
  });
});

describe("restricted mode", () => {
  it("keeps a grantee on Ask, its ontologies' overviews and their shared pages", () => {
    for (const path of [
      "/",
      "/ask",
      "/ask/o?thread=q",
      "/ontology",
      "/ontology/o",
      "/ontology/o/documents/d",
      "/explore",
      "/explore/o/records",
      "/explore/o/entities/e",
      "/workspaces",
      "/nowhere",
      // Settings › Slack: the install, and unlinking their own account.
      "/settings/slack",
      // A partner app's consent page, which tells them only owners and
      // admins connect apps.
      "/oauth/authorize",
    ])
      expect(granteeRedirect(path), path).toBeUndefined();
    for (const path of [
      "/data",
      "/data/connections/c",
      "/settings",
      "/settings/access",
      "/settings/apps",
      "/settings/members",
      "/settings/slack/more",
      "/ontology/new",
      "/ontology/sample",
      "/workflows",
      "/workflows/new",
      "/workflows/a/runs/r",
      "/automations",
      "/automations/a/runs/r",
    ])
      expect(granteeRedirect(path), path).toBe("/ask");
    expect(granteeRedirect("/ontology/o/loads/l")).toBe("/ontology/o");
    expect(granteeRedirect("/ontology/o/versions/v")).toBe("/ontology/o");
    expect(granteeRedirect("/ontology/o/documents")).toBe("/ontology/o");
    // Review, on either tab, and the old Suggestions address.
    expect(granteeRedirect("/ontology/o/review")).toBe("/ontology/o");
    expect(granteeRedirect("/ontology/o/suggestions")).toBe("/ontology/o");
    // The graph isn't theirs: Records is their Explorer.
    expect(granteeRedirect("/explore/o")).toBe("/explore/o/records");
    expect(granteeRedirect("/explore/o/entities")).toBe("/explore/o/records");
    // Usage only with the Usage permission, and its old /workflows
    // addresses, which redirect there.
    for (const path of [
      "/ontology/o/usage",
      "/ontology/o/usage/w",
      "/ontology/o/usage/w/runs/r",
      "/ontology/o/workflows",
      "/ontology/o/workflows/w/runs/r",
    ]) {
      expect(granteeRedirect(path), path).toBe("/ontology/o");
      expect(granteeRedirect(path, true), path).toBeUndefined();
    }
    expect(granteeRedirect("/ontology/o/loads/l", true)).toBe("/ontology/o");
  });

  it("reads the Usage permission: on unless turned off, and off through a team", () => {
    const access: wfe.ListFoundryAccessResponse = {
      teams: [
        { id: "t1", name: "Support", memberUserIds: ["u1"] },
        { id: "t2", name: "Sales", memberUserIds: ["u1", "u2"] },
      ],
      workflowsOff: [
        { kind: "KIND_TEAM", id: "t1" },
        { kind: "KIND_SERVICE_CREDENTIAL", id: "k1" },
      ],
    };
    expect(
      workflowsOn(access, { kind: "KIND_SERVICE_CREDENTIAL", id: "k1" }),
    ).toBe(false);
    expect(
      workflowsOn(access, { kind: "KIND_SERVICE_CREDENTIAL", id: "k2" }),
    ).toBe(true);
    expect(workflowsOn(access, { kind: "KIND_USER", id: "u1" })).toBe(true);
    expect(workflowsOn({}, { kind: "KIND_TEAM", id: "t1" })).toBe(true);
    expect(
      workflowsOffBy(access, { kind: "KIND_USER", id: "u1" }).map(
        (t) => t.name,
      ),
    ).toEqual(["Support"]);
    expect(workflowsOffBy(access, { kind: "KIND_USER", id: "u2" })).toEqual([]);
    expect(workflowsOffBy(access, { kind: "KIND_TEAM", id: "t2" })).toEqual([]);
  });

  it("reads through grants where the user can't query the database", () => {
    const o = { id: "o", projectId: "p" };
    // A grantee reads only what its grants cover.
    expect(readAccess({ grantee: true, granted: ["o"] }, o)).toBe("grants");
    expect(readAccess({ grantee: true, granted: [] }, o)).toBe("none");
    // Query access not known (an owner, or a WFE that doesn't say): as today.
    expect(readAccess({ grantee: false, granted: [] }, o)).toBe("database");
    for (const queryRead of [
      { mode: "all" },
      { mode: "selected", projectIds: ["q", "p"] },
    ])
      expect(readAccess({ grantee: false, granted: ["o"], queryRead }, o)).toBe(
        "database",
      );
    // A member who can't query the project reads what grants cover.
    for (const queryRead of [
      {},
      { mode: "none" },
      { mode: "selected", projectIds: ["q"] },
    ]) {
      expect(readAccess({ grantee: false, granted: ["o"], queryRead }, o)).toBe(
        "grants",
      );
      expect(readAccess({ grantee: false, granted: [], queryRead }, o)).toBe(
        "none",
      );
    }
    // An ontology whose project they can't see can't be one they query.
    expect(
      readAccess(
        {
          grantee: false,
          granted: [],
          queryRead: { mode: "selected", projectIds: ["p"] },
        },
        { id: "o" },
      ),
    ).toBe("none");
  });
});

describe("source permissions", () => {
  const names: Record<string, string> = {
    ada: "Ada Lovelace",
    grace: "Grace Hopper",
    bob: "bob@elsewhere.example",
  };
  const name = (userId: string) => names[userId] || "Unknown user";
  const relative = (at: string) => `at ${at}`;
  const slack: wfe.FoundrySourceAudience = {
    connectorId: "c",
    kind: "slack",
    readAt: "2026-11-20T10:00:00Z",
    stale: false,
    everyoneItems: 5,
    membersItems: 1,
    grantees: 2,
  };

  it("tells a read that worked from a stale one and none", () => {
    expect(audienceRead(slack)).toEqual({
      kind: "fresh",
      at: "2026-11-20T10:00:00Z",
    });
    expect(audienceRead({ ...slack, stale: true })).toEqual({
      kind: "stale",
      at: "2026-11-20T10:00:00Z",
    });
    // Stale without a read is one that never worked.
    expect(audienceRead({ stale: true })).toEqual({ kind: "never" });
    expect(audienceRead({})).toEqual({ kind: "never" });
  });

  it("says what a Slack connection follows, when it was read, and whom of the people it narrows it matches", () => {
    expect(sourcePermissions(slack, name, relative)).toEqual({
      follows: "Follows Slack channel membership",
      line: "Read at 2026-11-20T10:00:00Z · 5 public channels, 1 private · 2 of 2 restricted people matched",
      notes: [],
    });
    expect(
      sourcePermissions(
        {
          ...slack,
          everyoneItems: 1,
          membersItems: 2,
          grantees: 4,
          unmatchedUserIds: ["bob", "nobody"],
          guestUserIds: ["grace"],
        },
        name,
        relative,
      ),
    ).toEqual({
      follows: "Follows Slack channel membership",
      line: "Read at 2026-11-20T10:00:00Z · 1 public channel, 2 private · 2 of 4 restricted people matched",
      notes: [
        "Not in Slack by their email: bob@elsewhere.example, Unknown user. Restricted, they read none of its channels, and none of its users.",
        "A guest in Slack: Grace Hopper. Restricted, they read only the private channels that list them, and none of its users.",
      ],
    });
    expect(
      sourcePermissions(
        { ...slack, guestUserIds: ["grace", "ada"] },
        name,
        relative,
      ).notes,
    ).toEqual([
      "Guests in Slack: Grace Hopper, Ada Lovelace. Restricted, they read only the private channels that list them, and none of its users.",
    ]);
    expect(
      sourcePermissions({ ...slack, grantees: 1 }, name, relative).line,
    ).toContain("1 of 1 restricted person matched");
  });

  it("lists five names, then counts the rest", () => {
    expect(listNames([])).toBe("");
    expect(listNames(["Ada", "Eve"])).toBe("Ada, Eve");
    expect(listNames(["A", "B", "C", "D", "E"])).toBe("A, B, C, D, E");
    expect(listNames(["A", "B", "C", "D", "E", "F", "G"])).toBe(
      "A, B, C, D, E and 2 more",
    );
    const many = Array.from({ length: 10_000 }, (_, i) => `u${i}`);
    expect(
      sourcePermissions(
        { ...slack, grantees: many.length, unmatchedUserIds: many },
        (id) => id,
        relative,
      ).notes[0],
    ).toBe(
      "Not in Slack by their email: u0, u1, u2, u3, u4 and 9,995 more. Restricted, they read none of its channels, and none of its users.",
    );
  });

  it("says a connection with more channels than a restricted person may read the tables of", () => {
    const over = { ...slack, everyoneItems: MAX_HELD_ITEMS, membersItems: 1 };
    expect(sourcePermissions(over, name, relative).notes).toEqual([
      "Over 1,000 channels: someone restricted who is in more than 1,000 reads its documents, but none of its tables.",
    ]);
    expect(
      sourcePermissions({ ...over, membersItems: 0 }, name, relative).notes,
    ).toEqual([]);
  });

  it("reads counts the WFE leaves out as none", () => {
    expect(
      sourcePermissions(
        { kind: "slack", readAt: "2026-11-20T10:00:00Z" },
        name,
        relative,
      ).line,
    ).toBe(
      "Read at 2026-11-20T10:00:00Z · 0 public channels, 0 private · 0 of 0 restricted people matched",
    );
  });

  it("says when a read is stale or never worked, and why the last one failed", () => {
    expect(
      sourcePermissions({ ...slack, stale: true }, name, relative).line,
    ).toBe(
      "Last read at 2026-11-20T10:00:00Z, too long ago: only owners, admins and members who aren’t restricted read it until a sync reads them again · 5 public channels, 1 private · 2 of 2 restricted people matched",
    );
    const never = sourcePermissions(
      {
        connectorId: "c",
        kind: "slack",
        stale: true,
        grantees: 1,
        unmatchedUserIds: ["ada"],
        problem: {
          message: "add the users:read.email scope to the app and reinstall it",
          at: "2026-11-21T10:00:00Z",
        },
      },
      name,
      relative,
    );
    expect(never.line).toBe(
      "Not read yet: only owners, admins and members who aren’t restricted read it until a sync reads them again · 0 of 1 restricted person matched",
    );
    expect(never.notes).toEqual([
      "Not in Slack by their email: Ada Lovelace. Restricted, they read none of its channels, and none of its users.",
      "The last read of who it shares what with failed: add the users:read.email scope to the app and reinstall it.",
    ]);
    expect(SLACK_GRANT_NOTE).toBe(
      "only the channels each person is in, now and later",
    );
  });
});

describe("a link to a key's sources", () => {
  it("names a service credential, and nothing else", () => {
    const link = new URL(workspacePaths.keySources("sc 1"), "http://x");
    expect(link.pathname).toBe("/settings/access");
    expect(keyParam(link.searchParams.get("principal"))).toBe("sc 1");
    expect(keyParam("service_credential:sc_1")).toBe("sc_1");
    for (const raw of [
      null,
      "",
      "service_credential:",
      "user:usr_1",
      "team:t_1",
      "KIND_SERVICE_CREDENTIAL:sc_1",
    ])
      expect(keyParam(raw)).toBeNull();
  });
});
