import { describe, expect, it } from "vitest";
import type * as wfe from "@/shared/wfe";
import {
  covered,
  grantKey,
  grantOf,
  granteeRedirect,
  grantsOf,
  heldGrants,
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
  setOwners,
  sourceState,
  sourceTree,
  targetOf,
  targetsOf,
  toggle,
  type Grant,
} from "@/web/areas/workspace/access-model";

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
    for (const g of [stripe, invoices, general, enrichment])
      expect(grantOf(targetOf(g))).toEqual(g);
    expect(grantKey(invoices)).toBe("table:conn_stripe:invoices");
    expect(grantKey(enrichment)).toBe("enrichment");
  });

  it("drops a target this build doesn't know or that misses what it names", () => {
    for (const t of [
      { kind: "KIND_UNSPECIFIED" },
      { kind: "KIND_CONNECTOR" },
      { kind: "KIND_TABLE", connectorId: "c" },
      { kind: "KIND_DOCUMENT_SET", documentSetId: "" },
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
    ])
      expect(granteeRedirect(path), path).toBeUndefined();
    for (const path of [
      "/data",
      "/data/connections/c",
      "/settings",
      "/settings/access",
      "/ontology/new",
      "/ontology/sample",
    ])
      expect(granteeRedirect(path), path).toBe("/ask");
    expect(granteeRedirect("/ontology/o/loads/l")).toBe("/ontology/o");
    expect(granteeRedirect("/ontology/o/versions/v")).toBe("/ontology/o");
    expect(granteeRedirect("/ontology/o/documents")).toBe("/ontology/o");
    // The graph isn't theirs: Records is their Explorer.
    expect(granteeRedirect("/explore/o")).toBe("/explore/o/records");
    expect(granteeRedirect("/explore/o/entities")).toBe("/explore/o/records");
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
