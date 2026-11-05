import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import { Readable } from "node:stream";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type * as wfe from "@/shared/wfe";
import type { MockReply } from "../../mock/http";
import { contractErrors } from "../../mock/agent-contract";
import { routes } from "../../mock/routes";
import { handleDocumentUpload } from "../../mock/routes/documents";
import { config, db, resetState } from "../../mock/state";

// The mock WFE's source grants behave as the backend's (backend/access,
// backend/api/access*.go): what a load holds and who reads it through
// grants, a grantee refused every route its rules don't admit and every
// ontology its grants don't cover, narrowed answers (its loads, version,
// questions and documents), restricted members, teams, the preview of a
// change, and the WFE's agents' keys.

const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));
async function call(
  method: HttpMethod,
  path: string,
  body?: unknown,
  userId = "usr_ada",
): Promise<MockReply & { body: any }> {
  const url = new URL(path, "http://mock");
  for (const r of compiled) {
    const params = r.method === method && matchTemplate(r.c, url.pathname);
    if (!params) continue;
    return (await r.handle({
      method,
      path: url.pathname,
      params,
      query: url.searchParams,
      body,
      principal: { user: db.users.get(userId)!, sessionId: "s" },
    })) as MockReply & { body: any };
  }
  throw new Error(`no mock route for ${method} ${path}`);
}

const WS = "/v1/workspaces/ws_acme",
  F = `${WS}/foundry`,
  A = `${F}/access`,
  O = `${F}/ontologies/ont_acme_contracts`;
const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
);
const later = (ms: number) => vi.setSystemTime(Date.now() + ms);
const refusal = (r: MockReply & { body: any }) => [r.status, r.body.message];
const eve = (method: HttpMethod, path: string, body?: unknown) =>
  call(method, path, body, "usr_eve");
const user = (id: string): wfe.FoundryPrincipal => ({ kind: "KIND_USER", id });
const stripe: wfe.FoundryAccessTarget = {
  kind: "KIND_CONNECTOR",
  connectorId: "conn_acme_stripe",
};
const general: wfe.FoundryAccessTarget = {
  kind: "KIND_DOCUMENT_SET",
  documentSetId: "set_acme_general",
};
const grant = (
  principal: wfe.FoundryPrincipal,
  targets: wfe.FoundryAccessTarget[],
) => call("PUT", `${A}/grants`, { principal, targets });

/** Uploads the seeded contract into General through its presigned PUT. */
async function uploadContract() {
  const sha = createHash("sha256").update(CONTRACT).digest("hex");
  const { document, upload } = (
    await call("POST", `${F}/documents`, {
      name: "contract.md",
      mediaType: "text/markdown",
      sizeBytes: String(CONTRACT.length),
      sha256: sha,
      clientToken: "c",
      setId: "set_acme_general",
    })
  ).body;
  const req = Object.assign(Readable.from([CONTRACT]), {
    method: "PUT",
    headers: Object.fromEntries(
      Object.entries(upload.headers as Record<string, string>).map(([k, v]) => [
        k.toLowerCase(),
        v,
      ]),
    ),
  }) as unknown as IncomingMessage;
  const res = {
    setHeader: () => res,
    writeHead: () => res,
    end: () => undefined,
  } as unknown as ServerResponse;
  await handleDocumentUpload(req, res, new URL(upload.url));
  await call("POST", `${F}/documents/${document.id}:complete`, {});
  return document.id as string;
}

/** Customer contracts' version 3 loaded with the contract, current. */
async function loaded() {
  Object.assign(config, { loadStepMs: 50 });
  const doc = await uploadContract();
  const load = (
    await call("POST", `${O}/loads`, {
      versionId: "ver_seed_3",
      clientToken: "l",
    })
  ).body.load;
  later(10_000);
  expect((await call("GET", `${O}/loads/${load.id}`)).body.load).toMatchObject({
    status: "ready",
    current: true,
  });
  return { load: load.id as string, doc };
}

const contractsAccess = async () =>
  (
    (await call("GET", A)).body as wfe.ListFoundryAccessResponse
  ).ontologies!.find((o) => o.ontologyId === "ont_acme_contracts");

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-19T12:00:00Z"));
  resetState();
});
afterEach(() => vi.useRealTimers());

describe("mock source grants", () => {
  it("seeds Eve restricted to General and an agent's key holding nothing", async () => {
    const list = (await call("GET", A)).body as wfe.ListFoundryAccessResponse;
    expect(list.restrictedUserIds).toEqual(["usr_eve"]);
    expect(list.teams).toEqual([]);
    expect(list.grants).toEqual([
      expect.objectContaining({
        principal: user("usr_eve"),
        target: { ...general, connectorId: "", table: "" },
      }),
    ]);
    // Nothing is loaded yet: no ontology to read.
    expect(list.ontologies).toEqual([]);
    const keys = (await call("GET", `${WS}/service-credentials`)).body
      .credentials as wfe.ServiceCredential[];
    expect(keys.map((k) => [k.name, k.grants, k.revokedAt])).toEqual([
      ["Support agent", [], null],
    ]);
    // The members' project policies: Eve queries nothing, Alan does.
    const members = (await call("GET", `${WS}/members`)).body
      .members as wfe.WorkspaceMember[];
    const policy = (email: string) =>
      members.find((m) => m.user?.email === email)?.projectAccess;
    expect(policy("eve@example.com")).toMatchObject({
      write: { mode: "none" },
      queryRead: { mode: "none" },
    });
    expect(policy("alan@example.com")).toMatchObject({
      queryRead: { mode: "all" },
    });
    Object.assign(config, { memberQueryRead: false });
    const after = (await call("GET", `${WS}/members`)).body
      .members as wfe.WorkspaceMember[];
    expect(
      after.find((m) => m.user?.email === "alan@example.com")?.projectAccess
        ?.queryRead,
    ).toEqual({ mode: "none" });
  });

  it("refuses a restricted member every Foundry route its rules don't admit", async () => {
    const admitted = new Set([
      `GET ${F}/access/me`,
      `GET ${F}/ontologies`,
      `GET ${F}/ontologies/{ontologyId}`,
      `GET ${F}/ontologies/{ontologyId}/versions/{id}`,
      `GET ${F}/ontologies/{ontologyId}/loads`,
      `GET ${F}/ontologies/{ontologyId}/loads/{id}`,
      `POST ${F}/ontologies/{ontologyId}/loads/{loadId}:read`,
      `POST ${F}/ontologies/{ontologyId}/questions`,
      `GET ${F}/ontologies/{ontologyId}/questions`,
      `GET ${F}/ontologies/{ontologyId}/questions/{id}`,
      `GET ${F}/ontologies/{ontologyId}/questions/{id}/events`,
      `POST ${F}/ontologies/{ontologyId}/questions/{id}:cancel`,
      `GET ${F}/documents`,
      `GET ${F}/documents/{id}:content`,
      `GET ${F}/ontologies/{ontologyId}/documents`,
      `GET ${F}/ontologies/{ontologyId}/documents/{id}`,
    ]);
    const foundry = routes.filter((r) =>
      /^\/v1\/(foundry|workspaces\/\{workspaceId\}\/foundry)/.test(r.path),
    );
    expect(foundry.length).toBeGreaterThan(60);
    for (const r of foundry) {
      const path = r.path
        .replace("{workspaceId}", "ws_acme")
        .replace(/\{\w+\}/g, "x");
      const key = `${r.method} ${r.path.replace("{workspaceId}", "ws_acme")}`;
      const reply = await eve(
        r.method,
        path + (path.startsWith("/v1/foundry") ? "?workspaceId=ws_acme" : ""),
        { workspaceId: "ws_acme" },
      );
      if (admitted.has(key)) continue;
      expect(refusal(reply), key).toEqual([
        403,
        "source grants don't allow this call",
      ]);
    }
    // An ontology its grants don't cover isn't there; documents only by id.
    expect((await eve("GET", O)).status).toBe(404);
    expect(refusal(await eve("GET", `${F}/documents`))).toEqual([
      403,
      "source grants let a grantee name documents only by id",
    ]);
    expect((await eve("GET", `${F}/ontologies`)).body.ontologies).toEqual([]);
    expect((await eve("GET", `${A}/me`)).body).toEqual({
      grantee: true,
      hasGrants: true,
      ontologyIds: [],
      partialOntologyIds: [],
    });
    // The WFE's own routes answer as for any member, but query-read: Eve
    // holds no query access.
    expect((await eve("GET", `${WS}/members`)).status).toBe(200);
    expect(
      (
        await eve("POST", "/v1/databases:query-read", {
          database: { tenantId: "ten_acme_contracts" },
          queryJson: "{}",
        })
      ).status,
    ).toBe(403);
    // Ada and Alan aren't grantees.
    expect((await call("GET", `${A}/me`)).body.grantee).toBe(false);
    expect(
      (await call("GET", `${F}/connectors`, undefined, "usr_alan")).status,
    ).toBe(200);
  });

  it("covers a load once every table and set it holds is granted", async () => {
    const { load, doc } = await loaded();
    expect(await contractsAccess()).toEqual({
      ontologyId: "ont_acme_contracts",
      loadId: load,
      inputs: {
        tables: [
          { connectorId: "conn_acme_stripe", table: "customers" },
          { connectorId: "conn_acme_stripe", table: "invoices" },
        ],
        documentSets: [{ documentSetId: "set_acme_general", connectorId: "" }],
        enrichment: false,
        uncoverable: false,
      },
      readers: [],
      // Eve's General is part of what it holds.
      partialReaders: [user("usr_eve")],
    });
    // One table isn't both: she holds part of it.
    await grant(user("usr_eve"), [
      general,
      {
        kind: "KIND_TABLE",
        connectorId: "conn_acme_stripe",
        table: "customers",
      },
    ]);
    expect((await eve("GET", `${A}/me`)).body).toMatchObject({
      ontologyIds: ["ont_acme_contracts"],
      partialOntologyIds: ["ont_acme_contracts"],
    });
    await grant(user("usr_eve"), [
      general,
      {
        kind: "KIND_TABLE",
        connectorId: "conn_acme_stripe",
        table: "customers",
      },
      {
        kind: "KIND_TABLE",
        connectorId: "conn_acme_stripe",
        table: "invoices",
      },
    ]);
    expect((await eve("GET", `${A}/me`)).body).toMatchObject({
      ontologyIds: ["ont_acme_contracts"],
      partialOntologyIds: [],
    });
    // The connector covers every table; the grant kept keeps its id.
    const before = (await call("GET", A)).body
      .grants as wfe.FoundryAccessGrant[];
    const set = await grant(user("usr_eve"), [general, stripe]);
    expect(
      set.body.grants.map((g: wfe.FoundryAccessGrant) => g.target!.kind),
    ).toEqual(["KIND_DOCUMENT_SET", "KIND_CONNECTOR"]);
    expect(set.body.grants[0].id).toBe(before[0]!.id);
    expect(await contractsAccess()).toMatchObject({
      readers: [user("usr_eve")],
      partialReaders: [],
    });

    // What Eve sees: the ontology without its database or versions, its
    // one load without jobs, its version without cards.
    const list = (await eve("GET", `${F}/ontologies`)).body.ontologies;
    expect(list).toEqual([
      {
        id: "ont_acme_contracts",
        name: "Customer contracts",
        createdAt: expect.any(String),
        currentLoad: {
          id: load,
          version: "foundry-v3",
          finishedAt: expect.any(String),
        },
      },
    ]);
    expect((await eve("GET", O)).body.ontology).toEqual(list[0]);
    const loads = (await eve("GET", `${O}/loads`)).body.loads;
    expect(loads).toEqual([
      expect.objectContaining({
        id: load,
        versionId: "ver_seed_3",
        jobs: [],
        usdMicros: "0",
        enrichment: null,
      }),
    ]);
    expect((await eve("GET", `${O}/loads/other`)).status).toBe(404);
    expect((await eve("GET", `${O}/versions/ver_seed_3`)).status).toBe(200);
    expect((await eve("GET", `${O}/versions/ver_seed_1`)).status).toBe(404);
    expect(
      (await eve("POST", `${O}/loads/other:read`, { readsJson: "[]" })).status,
    ).toBe(404);
    // Renewals has no load: not there.
    expect((await eve("GET", `${F}/ontologies/ont_acme_renewals`)).status).toBe(
      404,
    );
    // Documents by id, in sets it holds.
    expect(
      (await eve("GET", `${F}/documents?ids=${doc}`)).body.documents.map(
        (d: wfe.FoundryDocument) => d.id,
      ),
    ).toEqual([doc]);
    // The documents its load holds: their text and their page, never
    // their bytes.
    expect((await eve("GET", `${F}/documents/${doc}:content`)).status).toBe(
      200,
    );
    expect((await eve("GET", `${F}/documents/${doc}:bytes`)).status).toBe(403);
    expect(
      (await eve("GET", `${O}/documents`)).body.documents.map(
        (d: wfe.FoundryDocument) => d.id,
      ),
    ).toEqual([doc]);
    expect((await eve("GET", `${O}/documents/${doc}`)).status).toBe(200);
    await grant(user("usr_eve"), [stripe]);
    expect(
      (await eve("GET", `${F}/documents?ids=${doc}`)).body.documents,
    ).toEqual([]);
    expect((await eve("GET", `${F}/documents/${doc}:content`)).status).toBe(
      404,
    );
    // Stripe alone still holds part of it, without the document.
    expect((await eve("GET", `${F}/ontologies`)).body.ontologies).toEqual([
      expect.objectContaining({
        id: "ont_acme_contracts",
        partialAccess: true,
      }),
    ]);
    expect((await eve("GET", `${O}/documents`)).body.documents).toEqual([]);
    expect((await eve("GET", `${O}/documents/${doc}`)).status).toBe(404);
    await grant(user("usr_eve"), []);
    expect((await eve("GET", `${F}/ontologies`)).body.ontologies).toEqual([]);
  });

  it("lets a grantee ask its load and see only its own questions", async () => {
    const { load } = await loaded();
    await grant(user("usr_eve"), [general, stripe]);
    const Q = `${O}/questions`;
    const mine = await eve("POST", Q, {
      question: "Who signed?",
      clientToken: "e1",
    });
    expect(mine.body.question).toMatchObject({ loadId: load });
    expect(
      refusal(
        await eve("POST", Q, {
          question: "x",
          clientToken: "e2",
          loadId: "load_other",
        }),
      ),
    ).toEqual([404, "load not found"]);
    const ada = (
      await call("POST", Q, { question: "Totals?", clientToken: "a1" })
    ).body.question;
    const ids = async (as: "usr_ada" | "usr_eve") =>
      (await call("GET", Q, undefined, as)).body.questions.map(
        (q: wfe.FoundryQuestion) => q.id,
      );
    expect(await ids("usr_eve")).toEqual([mine.body.question.id]);
    expect(await ids("usr_ada")).toEqual([ada.id, mine.body.question.id]);
    expect((await eve("GET", `${Q}/${ada.id}`)).status).toBe(404);
    expect((await eve("GET", `${Q}/${ada.id}/events`)).status).toBe(404);
    expect((await eve("POST", `${Q}/${ada.id}:cancel`, {})).status).toBe(404);
    // Another's thread isn't one to add to, nor its client token.
    expect(
      refusal(
        await eve("POST", Q, {
          question: "And?",
          clientToken: "e3",
          threadId: ada.id,
        }),
      ),
    ).toEqual([404, "thread not found"]);
    expect(
      (await eve("POST", Q, { question: "Totals?", clientToken: "a1" })).status,
    ).toBe(400);
    expect(
      (await eve("POST", `${Q}/${mine.body.question.id}:cancel`, {})).status,
    ).toBe(200);
  });

  it("holds a team's grants for its members", async () => {
    await loaded();
    const team = (
      await call("POST", `${A}/teams`, {
        name: " Sales ",
        memberUserIds: ["usr_eve", "usr_eve"],
      })
    ).body.team as wfe.FoundryTeam;
    expect(team).toMatchObject({ name: "Sales", memberUserIds: ["usr_eve"] });
    await grant({ kind: "KIND_TEAM", id: team.id! }, [stripe]);
    // Eve holds General herself and Stripe through Sales; neither principal's
    // own grants cover the load.
    expect((await eve("GET", `${A}/me`)).body.ontologyIds).toEqual([
      "ont_acme_contracts",
    ]);
    expect((await contractsAccess())!.readers).toEqual([]);
    expect(
      refusal(
        await call("POST", `${A}/teams`, { name: "sales", memberUserIds: [] }),
      ),
    ).toEqual([409, "the workspace has a team of that name"]);
    expect(
      refusal(
        await call("POST", `${A}/teams`, { name: " ", memberUserIds: [] }),
      ),
    ).toEqual([400, "a team's name is 1 to 120 characters"]);
    expect(
      refusal(
        await call("POST", `${A}/teams`, {
          name: "Ops",
          memberUserIds: ["usr_nobody"],
        }),
      ),
    ).toEqual([400, "usr_nobody is not a member of the workspace"]);
    // Renamed with its members replaced; then deleted with its grants.
    const renamed = await call("PUT", `${A}/teams/${team.id}`, {
      name: "Revenue",
      memberUserIds: [],
    });
    expect(renamed.body.team).toMatchObject({
      name: "Revenue",
      memberUserIds: [],
    });
    // Her own General is still part of it.
    expect((await eve("GET", `${A}/me`)).body.partialOntologyIds).toEqual([
      "ont_acme_contracts",
    ]);
    expect((await call("DELETE", `${A}/teams/${team.id}`)).status).toBe(200);
    expect(
      ((await call("GET", A)).body.grants as wfe.FoundryAccessGrant[]).map(
        (g) => g.principal!.kind,
      ),
    ).toEqual(["KIND_USER"]);
    expect((await call("DELETE", `${A}/teams/${team.id}`)).status).toBe(404);
    expect(
      (
        await call("PUT", `${A}/teams/${team.id}`, {
          name: "X",
          memberUserIds: [],
        })
      ).status,
    ).toBe(404);
  });

  describe("a grantee who holds part of a load", () => {
    const customersTable: wfe.FoundryAccessTarget = {
      kind: "KIND_TABLE",
      connectorId: "conn_acme_stripe",
      table: "customers",
    };
    const invoicesTable: wfe.FoundryAccessTarget = {
      kind: "KIND_TABLE",
      connectorId: "conn_acme_stripe",
      table: "invoices",
    };
    /** Reads as Eve, each outcome held to read_result.json. */
    const read = async (load: string, reads: unknown[]) => {
      const r = await eve("POST", `${O}/loads/${load}:read`, {
        readsJson: JSON.stringify(reads),
      });
      expect(r.status).toBe(200);
      const body = JSON.parse(r.body.resultJson);
      expect(contractErrors("read_result", body)).toEqual([]);
      return body.results as any[];
    };
    const rows = (
      type: string,
      columns: unknown[],
      more: Record<string, unknown> = {},
    ) => ({
      query: { start: { type }, return: { rows: { columns, ...more } } },
    });

    it("sees the version as its sources fill it", async () => {
      await loaded();
      await grant(user("usr_eve"), [customersTable]);
      const v = (await eve("GET", `${O}/versions/ver_seed_3`)).body.version;
      expect(v.definition.ontology_toml).toContain("[types.Customer]");
      for (const gone of [
        "types.Invoice",
        "types.Contract",
        "billed_to",
        "signed_with",
        "under_contract",
        "invoice_under_contract",
      ])
        expect(v.definition.ontology_toml).not.toContain(gone);
      // Only what names the files, and nothing else of the definition.
      expect(v.definition.mappings_toml).not.toContain("mappings.");
      expect(v.definition.mappings_toml).not.toContain("extractions.");
      expect(Object.keys(v.definition).sort()).toEqual([
        "format",
        "mappings_toml",
        "ontology_toml",
        "status",
      ]);
      expect(v.generationId).toBe("");
      expect(v.documentSetIds).toEqual([]);
      const list = (await eve("GET", `${F}/ontologies`)).body.ontologies;
      expect(list[0].partialAccess).toBe(true);
      // Holding it all, the version is the version, cards aside.
      await grant(user("usr_eve"), [general, stripe]);
      const whole = (await eve("GET", `${O}/versions/ver_seed_3`)).body.version;
      expect(whole.definition.ontology_toml).toContain("[types.Invoice]");
      expect(whole.generationId).toBe("gen_seed_4");
      // Without what drafting it found and cost.
      for (const gone of ["summary", "usage", "rules", "parent_version_id"])
        expect(whole.definition[gone]).toBeUndefined();
      for (const item of Object.values<Record<string, unknown>>(
        whole.definition.draft?.draft?.items || {},
      )) {
        expect(item.rationale).toBeUndefined();
        expect(item.card).toBeUndefined();
      }
      expect(
        (await eve("GET", `${F}/ontologies`)).body.ontologies[0].partialAccess,
      ).toBeUndefined();
    });

    it("reads only what it holds, and can't filter or count the rest", async () => {
      Object.assign(config, { invoiceOnlyCustomers: 90 });
      const { load } = await loaded();
      await grant(user("usr_eve"), [customersTable]);
      const [view, first, paged, count, byId, byIdSort, gone, hidden] =
        await read(load, [
          { view: {} },
          rows("Customer", [{ at: 0 }, { at: 0, prop: "currency" }], {
            limit: 100,
          }),
          rows("Customer", [{ at: 0 }], { limit: 100, offset: 100 }),
          { query: { start: { type: "Customer" }, return: { count: {} } } },
          {
            query: {
              start: {
                type: "Customer",
                where: [{ prop: "customer_id", op: "=", value: "x" }],
              },
              return: { rows: { columns: [{ at: 0 }] } },
            },
          },
          rows("Customer", [{ at: 0 }], {
            order_by: { at: 0, prop: "customer_id" },
          }),
          { entity: { type: "Customer", id: "cus_00001" } },
          rows("Invoice", [{ at: 0 }]),
        ]);
      expect(view.ok.partial).toEqual(
        expect.arrayContaining(["Customer", "Customer.customer_id"]),
      );
      // The first 90 of Customer are the invoices table's alone, and the read
      // reads past them: its limit says nothing of what it left out.
      expect(first.ok.result.rows).toHaveLength(64);
      expect(first.ok.result.total).toBe(64);
      expect(first.ok.result.more).toBeUndefined();
      expect(first.ok.stats).toMatchObject({ matched: 0, patterns: 0 });
      expect(first.ok.result.rows[0].cells[0].id).not.toMatch(/^cus_0/);
      // Its name is its id: its stored name may be another source's.
      expect(first.ok.result.rows[0].cells[0].name).toBe(
        first.ok.result.rows[0].cells[0].id,
      );
      // Where a page ended would say how much it can't see.
      expect(paged.error).toMatchObject({
        code: "not_visible",
        pointer: "/return/rows/offset",
      });
      expect(count.error).toMatchObject({
        code: "not_visible",
        pointer: "/start/type",
      });
      expect(byId.error).toMatchObject({
        code: "not_visible",
        pointer: "/start/where/0/prop",
      });
      expect(byIdSort.error).toMatchObject({
        code: "not_visible",
        pointer: "/return/rows/order_by/prop",
      });
      expect(gone.error.code).toBe("unknown_entity");
      expect(hidden.error.code).toBe("invalid");
    });

    it("settles a value from what it holds", async () => {
      const { load } = await loaded();
      // General alone: Customer isn't created by what it holds; Contract is.
      await grant(user("usr_eve"), [general]);
      const [contracts, customers] = await read(load, [
        rows("Contract", [{ at: 0 }, { at: 0, prop: "term_months" }]),
        rows("Customer", [{ at: 0 }]),
      ]);
      expect(contracts.ok.result.rows.length).toBeGreaterThan(0);
      expect(customers.error.code).toBe("invalid");
      // The customers table alone: its row's currency stands, what documents
      // say of it aside, and the row's rows only.
      await grant(user("usr_eve"), [customersTable]);
      const [row, evidence] = await read(load, [
        rows("Customer", [{ at: 0 }, { at: 0, prop: "currency" }], {
          limit: 1,
        }),
        {
          evidence: {
            entity: { type: "Customer", id: "cus_GLOBEX01" },
            prop: "currency",
          },
        },
      ]);
      expect(row.ok.result.rows[0].cells[1]).toEqual(expect.any(String));
      expect(
        evidence.ok.items.every((i: { kind: string }) => i.kind === "row"),
      ).toBe(true);
    });

    it("reads a column it settles at most 100 rows at a time", async () => {
      Object.assign(config, { invoiceOnlyCustomers: 150 });
      const { load } = await loaded();
      await grant(user("usr_eve"), [invoicesTable]);
      const [settling, plain] = await read(load, [
        rows("Customer", [{ at: 0 }, { at: 0, prop: "customer_id" }], {
          limit: 200,
        }),
        rows("Customer", [{ at: 0 }], { limit: 200 }),
      ]);
      expect(settling.ok.result.rows).toHaveLength(100);
      expect(settling.ok.result.total).toBe(100);
      expect(plain.ok.result.rows.length).toBeGreaterThan(100);
      expect(plain.ok.result.total).toBe(plain.ok.result.rows.length);
    });

    it("shows a keyed entity by a row it holds, never by a document alone", async () => {
      Object.assign(config, { invoiceOnlyCustomers: 5 });
      const { load } = await loaded();
      // Invoices and documents: a customer only the customers table states
      // stays out, whatever a document says of it.
      await grant(user("usr_eve"), [invoicesTable, general]);
      const [shown, globex] = await read(load, [
        rows("Customer", [{ at: 0 }], { limit: 200 }),
        { entity: { type: "Customer", id: "cus_GLOBEX01" } },
      ]);
      const ids = shown.ok.result.rows.map(
        (r: { cells: { id: string }[] }) => r.cells[0]!.id,
      );
      expect(ids).toContain("cus_00001");
      expect(ids).not.toContain("cus_GLOBEX01");
      expect(globex.error.code).toBe("unknown_entity");
      // The customers table states it.
      await grant(user("usr_eve"), [customersTable]);
      const [held] = await read(load, [
        { entity: { type: "Customer", id: "cus_GLOBEX01" } },
      ]);
      expect(held.ok).toBeDefined();
    });

    it("gives a grantee its own budget, and refusals that say nothing of the workspace", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: false, now: Date.now() });
      await loaded();
      await grant(user("usr_eve"), [customersTable]);
      const ask = (who: typeof eve, clientToken: string) =>
        who("POST", `${O}/questions`, { question: "Who?", clientToken });
      const budget = async () =>
        (await eve("GET", `${O}/questions`)).body.budget;
      expect(await budget()).toMatchObject({
        monthlyUsdMicros: "10000000",
        spentUsdMicros: "0",
        hourlyLimit: 10,
      });
      // Someone else's spend isn't in it.
      Object.assign(config, { askSpentUsdMicros: 1_000_000 });
      expect((await budget()).spentUsdMicros).toBe("0");
      Object.assign(config, { askSpentUsdMicros: 0 });
      // Its own hour.
      for (const i of Array.from({ length: 10 }, (_, n) => n))
        expect((await ask(eve, `own${i}`)).status).toBe(200);
      const own = await ask(eve, "own10");
      expect(own.status).toBe(429);
      expect(own.body.message).toBe(
        "you have asked 10 questions in the last hour; try again later",
      );
      // Its own month, its live questions at their caps.
      later(3_700_000);
      Object.assign(config, { askGranteeSpentUsdMicros: 9_800_000 });
      expect(Number((await budget()).spentUsdMicros)).toBeGreaterThan(
        9_800_000,
      );
      const spent = await ask(eve, "own11");
      expect(spent.status).toBe(400);
      expect(spent.body.message).toBe(
        "you have spent this month's $10.00 of questions",
      );
      Object.assign(config, { askGranteeSpentUsdMicros: 0 });
      // The workspace's month, in words that don't say what it spent.
      Object.assign(config, { askSpentUsdMicros: 30_000_000 });
      const month = await ask(eve, "own12");
      expect(month.status).toBe(400);
      expect(month.body.message).toBe(
        "this workspace's questions are used up for the month",
      );
      expect((await ask(call as typeof eve, "ada1")).body.message).toContain(
        "the workspace has spent this month's",
      );
      Object.assign(config, { askSpentUsdMicros: 0 });
      // The workspace's hour, likewise.
      later(3_700_000);
      for (const i of Array.from({ length: 30 }, (_, n) => n))
        expect((await ask(call as typeof eve, `ada-hour${i}`)).status).toBe(
          200,
        );
      const hour = await ask(eve, "own13");
      expect(hour.status).toBe(429);
      expect(hour.body.message).toBe(
        "too many questions have been asked here in the last hour; try again later",
      );
      expect(
        (await ask(call as typeof eve, "ada-past")).body.message,
      ).toContain("the workspace has asked 30 questions");
    });

    it("answers a question from what it holds, and keeps what it read", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: false, now: Date.now() });
      Object.assign(config, { askStepMs: 10 });
      const { load } = await loaded();
      await grant(user("usr_eve"), [customersTable]);
      const q = (
        await eve("POST", `${O}/questions`, {
          question: "Who are they?",
          clientToken: "part",
        })
      ).body.question;
      expect(q.loadId).toBe(load);
      later(60_000);
      const asked = (await eve("GET", `${O}/questions/${q.id}`)).body.question;
      expect(asked.status).toBe("answered");
      const result = JSON.parse(asked.resultJson);
      expect(contractErrors("ask_result", result)).toEqual([]);
      expect(JSON.stringify(result)).not.toContain("Invoice");
      const events = (await eve("GET", `${O}/questions/${q.id}/events`)).body
        .events;
      const tools = events
        .map((e: { eventJson: string }) => JSON.parse(e.eventJson))
        .filter((e: { event: string }) => e.event === "tool_call")
        .map((e: { tool: string }) => e.tool);
      // It reads no past (4.7.0): no history, no changes, no valid times.
      expect(tools).not.toContain("helix_query");
      expect(tools).not.toContain("history_of");
      expect(tools).not.toContain("changes");
      expect(JSON.stringify(result)).not.toContain('"valid"');
      expect(tools).toContain("query");
      // Its answer goes with what it read.
      await grant(user("usr_eve"), [general]);
      expect((await eve("GET", `${O}/questions/${q.id}`)).status).toBe(404);
      await grant(user("usr_eve"), [customersTable]);
      expect((await eve("GET", `${O}/questions/${q.id}`)).status).toBe(200);
    });

    it("asks through a view, with no history, when it holds all of the load", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: false, now: Date.now() });
      Object.assign(config, { askStepMs: 10 });
      await loaded();
      await grant(user("usr_eve"), [general, stripe]);
      const q = (
        await eve("POST", `${O}/questions`, {
          question: "Who signed it?",
          clientToken: "whole",
        })
      ).body.question;
      later(60_000);
      const asked = (await eve("GET", `${O}/questions/${q.id}`)).body.question;
      expect(asked.status).toBe("answered");
      const result = JSON.parse(asked.resultJson);
      expect(contractErrors("ask_result", result)).toEqual([]);
      // No tool reads the graph as stored under any view, whole ones too.
      const tools = (
        await eve("GET", `${O}/questions/${q.id}/events`)
      ).body.events
        .map((e: { eventJson: string }) => JSON.parse(e.eventJson))
        .filter((e: { event: string }) => e.event === "tool_call")
        .map((e: { tool: string }) => e.tool);
      expect(tools).not.toContain("helix_query");
      expect(tools).not.toContain("history_of");
      expect(tools).not.toContain("changes");
      expect(JSON.stringify(result)).not.toContain('"version"');
      expect(
        result.citations.map((c: { marker: string }) => c.marker),
      ).not.toContain("c11");
      expect(tools.at(-1)).toBe("answer");
    });

    it("declines a question about another time as uncovered", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: false, now: Date.now() });
      Object.assign(config, { askStepMs: 10, askOutcome: "cannot_answer" });
      await loaded();
      await grant(user("usr_eve"), [customersTable]);
      const q = (
        await eve("POST", `${O}/questions`, {
          question: "Which customers did we have last March?",
          clientToken: "part-past",
        })
      ).body.question;
      later(60_000);
      const asked = (await eve("GET", `${O}/questions/${q.id}`)).body.question;
      expect(asked.status).toBe("cannot_answer");
      const result = JSON.parse(asked.resultJson);
      expect(contractErrors("ask_result", result)).toEqual([]);
      expect(result).toMatchObject({ kind: "uncovered" });
      const declined = (
        await eve("GET", `${O}/questions/${q.id}/events`)
      ).body.events
        .map((e: { eventJson: string }) => JSON.parse(e.eventJson))
        .find((e: { tool?: string }) => e.tool === "cannot_answer");
      expect(declined.input).toMatchObject({ kind: "uncovered" });
    });

    it("pages a whole type by offset, with its total", async () => {
      Object.assign(config, { invoiceOnlyCustomers: 150 });
      const { load } = await loaded();
      await grant(user("usr_eve"), [general, stripe]);
      const [first, last, tooDeep] = await read(load, [
        rows("Customer", [{ at: 0 }], { limit: 200 }),
        rows("Customer", [{ at: 0 }], { limit: 200, offset: 200 }),
        rows("Customer", [{ at: 0 }], { offset: 10_001 }),
      ]);
      expect(first.ok.result.rows).toHaveLength(200);
      expect(first.ok.result.total).toBe(214);
      expect(first.ok.result.more).toBeUndefined();
      expect(last.ok.result.rows).toHaveLength(14);
      expect(tooDeep.error).toMatchObject({
        code: "invalid",
        pointer: "/return/rows/offset",
      });
    });
  });

  it("previews who a change cuts off", async () => {
    const { doc } = await loaded();
    const preview = (change: unknown) =>
      call("POST", `${A}:preview`, { change });
    // Eve's General alone reads only part of it: nobody loses all of it. With
    // no grant at all, there is nothing to tell.
    expect(
      (await preview({ deleteConnector: "conn_acme_stripe" })).body,
    ).toEqual({ losses: [] });
    await grant(user("usr_eve"), []);
    expect(
      (await preview({ deleteConnector: "conn_acme_stripe" })).body,
    ).toEqual({});
    await grant(user("usr_eve"), [general, stripe]);
    const loss = {
      principal: user("usr_eve"),
      ontologyId: "ont_acme_contracts",
      keepsPart: false,
    };
    // Without Stripe, her General is still part of it.
    expect(
      (await preview({ deleteConnector: "conn_acme_stripe" })).body.losses,
    ).toEqual([{ ...loss, keepsPart: true }]);
    expect(
      (await preview({ deleteConnector: "conn_acme_shop" })).body.losses,
    ).toEqual([]);
    // Version 2 reads the Shop database's tables, which Eve doesn't hold.
    expect(
      (
        await preview({
          loadVersion: {
            ontologyId: "ont_acme_contracts",
            versionId: "ver_seed_2",
          },
        })
      ).body.losses,
    ).toEqual([loss]);
    expect(
      (
        await preview({
          loadVersion: {
            ontologyId: "ont_acme_contracts",
            versionId: "ver_seed_3",
          },
        })
      ).body.losses,
    ).toEqual([]);
    // Moving the contract to a set she doesn't hold: Stripe stays hers.
    const set = (await call("POST", `${F}/document-sets`, { name: "Board" }))
      .body.set;
    expect(
      (
        await preview({
          moveDocument: { documentId: doc, documentSetId: set.id },
        })
      ).body.losses,
    ).toEqual([{ ...loss, keepsPart: true }]);
    expect(
      refusal(
        await preview({
          moveDocument: { documentId: doc, documentSetId: "set_gone" },
        }),
      ),
    ).toEqual([404, "document set not found"]);
    expect(
      refusal(
        await preview({
          loadVersion: {
            ontologyId: "ont_acme_contracts",
            versionId: "ver_gone",
          },
        }),
      ),
    ).toEqual([404, "version not found"]);
    expect(refusal(await preview({ deleteConnector: "conn_gone" }))).toEqual([
      404,
      "connector not found",
    ]);
    expect(refusal(await preview({}))).toEqual([400, "name a change"]);
    // Members can't ask.
    expect(
      (await call("POST", `${A}:preview`, { change: {} }, "usr_alan")).status,
    ).toBe(403);
  });

  it("checks grants, and drops those of a connector that is gone", async () => {
    expect(refusal(await grant({ kind: "KIND_USER" }, []))).toEqual([
      400,
      "name a principal: a user, a service credential or a team, and its id",
    ]);
    expect(
      refusal(
        await grant(user("usr_eve"), [
          { kind: "KIND_TABLE", connectorId: "conn_acme_stripe" },
        ]),
      ),
    ).toEqual([
      400,
      `access: a table grant can't name connector "conn_acme_stripe", table "" and set ""`,
    ]);
    expect(
      refusal(
        await grant(user("usr_eve"), [
          { kind: "KIND_CONNECTOR", connectorId: "conn_gone" },
        ]),
      ),
    ).toEqual([
      404,
      "a connector or document set to grant is not in the workspace",
    ]);
    // An empty list takes them all away.
    expect((await grant(user("usr_eve"), [])).body.grants).toEqual([]);
    await grant(user("usr_eve"), [stripe, { kind: "KIND_ENRICHMENT" }]);
    expect(
      (await call("DELETE", `${F}/connectors/conn_acme_stripe`)).status,
    ).toBe(200);
    expect(
      ((await call("GET", A)).body.grants as wfe.FoundryAccessGrant[]).map(
        (g) => g.target!.kind,
      ),
    ).toEqual(["KIND_ENRICHMENT"]);
    // Only owners and admins manage access.
    expect((await call("GET", A, undefined, "usr_alan")).status).toBe(403);
    expect((await grant(user("usr_eve"), [])).status).toBe(200);
  });

  it("restricts whoever sources reach, and taking them away widens nothing", async () => {
    Object.assign(config, { memberQueryRead: false });
    const lift = (userId: string) =>
        call("PUT", `${A}/members/${userId}`, { restricted: false }),
      restricted = async () =>
        (await call("GET", A)).body.restrictedUserIds as string[];
    await grant(user("usr_alan"), [stripe]);
    expect(await restricted()).toContain("usr_alan");
    expect(refusal(await lift("usr_alan"))).toEqual([
      400,
      "this member holds sources, their own or a team's; take those away first",
    ]);
    await grant(user("usr_alan"), []);
    expect(await restricted()).toContain("usr_alan");
    expect((await lift("usr_alan")).body).toEqual({ restricted: false });
    expect(await restricted()).not.toContain("usr_alan");

    // A team: its members when it gets sources, and whoever joins after.
    const team = (
      await call("POST", `${A}/teams`, {
        name: "Finance",
        memberUserIds: ["usr_alan", "usr_ada"],
      })
    ).body.team as wfe.FoundryTeam;
    expect(await restricted()).not.toContain("usr_alan");
    const principal: wfe.FoundryPrincipal = { kind: "KIND_TEAM", id: team.id! };
    await grant(principal, [stripe]);
    expect(await restricted()).toContain("usr_alan");
    expect(refusal(await lift("usr_alan"))?.[0]).toBe(400);
    await call("PUT", `${A}/teams/${team.id}`, {
      name: "Finance",
      memberUserIds: ["usr_eve"],
    });
    expect(await restricted()).toContain("usr_alan");
    expect((await lift("usr_alan")).body).toEqual({ restricted: false });

    expect(
      refusal(await grant({ kind: "KIND_TEAM", id: "team_none" }, [stripe])),
    ).toEqual([404, "the workspace has no such team"]);
  });

  it("gives sources only to whom they can confine", async () => {
    expect(refusal(await grant(user("usr_ada"), [stripe]))).toEqual([
      400,
      "only a member can be restricted: owners and admins see everything",
    ]);
    expect(refusal(await grant(user("usr_alan"), [stripe]))?.[1]).toContain(
      "take that access away in the Helix dashboard first",
    );
    expect(refusal(await grant(user("usr_nobody"), [stripe]))).toEqual([
      404,
      "member not found",
    ]);
    // Taking away is always allowed, even some of what one given raw access
    // since holds; adding isn't.
    expect((await grant(user("usr_alan"), [])).body.grants).toEqual([]);
    Object.assign(config, { memberQueryRead: false });
    await grant(user("usr_alan"), [stripe, general]);
    Object.assign(config, { memberQueryRead: true });
    expect((await grant(user("usr_alan"), [stripe])).body.grants).toHaveLength(
      1,
    );
    expect(refusal(await grant(user("usr_alan"), [general]))?.[1]).toContain(
      "take that access away in the Helix dashboard first",
    );
    // A connector down to one of its tables is narrowing too.
    expect(
      (
        await grant(user("usr_alan"), [
          { ...stripe, kind: "KIND_TABLE", table: "customers" },
        ])
      ).status,
    ).toBe(200);
    await grant(user("usr_alan"), []);

    const team = (
      await call("POST", `${A}/teams`, {
        name: "Finance",
        memberUserIds: ["usr_alan"],
      })
    ).body.team as wfe.FoundryTeam;
    const principal: wfe.FoundryPrincipal = { kind: "KIND_TEAM", id: team.id! };
    expect(refusal(await grant(principal, [stripe]))?.[1]).toContain(
      "a member of this team can write a project",
    );
    await call("PUT", `${A}/teams/${team.id}`, {
      name: "Finance",
      memberUserIds: ["usr_eve"],
    });
    await grant(principal, [stripe]);
    // Joining with raw access is refused; someone given it since doesn't stop a rename.
    expect(
      refusal(
        await call("PUT", `${A}/teams/${team.id}`, {
          name: "Finance",
          memberUserIds: ["usr_eve", "usr_alan"],
        }),
      )?.[1],
    ).toContain("a member of this team can write a project");
    Object.assign(config, { memberQueryRead: false });
    await call("PUT", `${A}/teams/${team.id}`, {
      name: "Finance",
      memberUserIds: ["usr_eve", "usr_alan"],
    });
    Object.assign(config, { memberQueryRead: true });
    expect(
      (
        await call("PUT", `${A}/teams/${team.id}`, {
          name: "Renamed",
          memberUserIds: ["usr_eve", "usr_alan"],
        })
      ).status,
    ).toBe(200);
  });

  it("restricts plain members without query access only", async () => {
    const restrict = (userId: string, restricted = true) =>
      call("PUT", `${A}/members/${userId}`, { restricted });
    expect(refusal(await restrict("usr_ada"))).toEqual([
      400,
      "only a member can be restricted: owners and admins see everything",
    ]);
    expect(refusal(await restrict("usr_alan"))).toEqual([
      400,
      "this member can write a project or query its databases outside Foundry; take that access away in the Helix dashboard first",
    ]);
    expect(refusal(await restrict("usr_nobody"))).toEqual([
      404,
      "member not found",
    ]);
    Object.assign(config, { memberQueryRead: false });
    expect((await restrict("usr_alan")).body).toEqual({ restricted: true });
    expect(
      (await call("GET", `${A}/me`, undefined, "usr_alan")).body.grantee,
    ).toBe(true);
    // Made an admin, the restriction no longer holds.
    Object.assign(config, { alanRole: "WORKSPACE_ROLE_ADMIN" });
    expect(
      (await call("GET", `${A}/me`, undefined, "usr_alan")).body.grantee,
    ).toBe(false);
    Object.assign(config, { alanRole: "WORKSPACE_ROLE_MEMBER" });
    await grant(user("usr_eve"), []);
    expect((await restrict("usr_eve", false)).body).toEqual({
      restricted: false,
    });
    expect((await call("GET", A)).body.restrictedUserIds).toEqual(["usr_alan"]);
    expect((await eve("GET", `${F}/connectors`)).status).toBe(200);
  });

  it("sends a member without query access through grants on Ask and the graph", async () => {
    const { load } = await loaded();
    Object.assign(config, { memberQueryRead: false });
    const alan = (method: HttpMethod, path: string, body?: unknown) =>
      call(method, path, body, "usr_alan");
    // Everything else as before: every ontology, with its database.
    const list = (await alan("GET", `${F}/ontologies`)).body.ontologies;
    expect(list.map((o: wfe.FoundryOntology) => o.databaseName)).toContain(
      "contracts",
    );
    expect((await alan("GET", `${A}/me`)).body).toEqual({
      grantee: false,
      hasGrants: false,
      ontologyIds: [],
      partialOntologyIds: [],
    });
    // Asking and reading need grants; review decisions and query-read are refused.
    // The WFE's own refusal, not an ontology it can't see: it lists this one.
    const refused = await alan("GET", `${O}/questions`);
    expect(refused.status).toBe(403);
    expect(refused.body.message).toBe("permission denied");
    expect(
      (
        await alan("POST", `${O}/loads/${load}:read`, {
          readsJson: JSON.stringify([{ view: {} }]),
        })
      ).status,
    ).toBe(403);
    const decisions = await alan("GET", `${O}/review-decisions`);
    expect(decisions.status).toBe(403);
    expect(decisions.body.message).toBe("permission denied");
    expect(
      (
        await alan("POST", "/v1/databases:query-read", {
          database: { tenantId: "ten_acme_contracts" },
          queryJson: "{}",
        })
      ).status,
    ).toBe(403);
    // Given sources, he is restricted to them.
    await grant(user("usr_alan"), [general, stripe]);
    expect((await alan("GET", `${A}/me`)).body).toEqual({
      grantee: true,
      hasGrants: true,
      ontologyIds: ["ont_acme_contracts"],
      partialOntologyIds: [],
    });
    expect(
      (
        await alan("POST", `${O}/questions`, {
          question: "Who?",
          clientToken: "x",
        })
      ).body.question,
    ).toMatchObject({
      loadId: load,
    });
    expect(
      (
        await alan("POST", `${O}/loads/${load}:read`, {
          readsJson: JSON.stringify([
            { entity: { type: "Customer", id: "cus_GLOBEX01" } },
          ]),
        })
      ).status,
    ).toBe(200);
  });

  it("makes agents' keys with their token shown once, and revokes them with their grants", async () => {
    const made = await call("POST", `${WS}/service-credentials`, {
      name: " Billing bot ",
    });
    expect(made.body.token).toMatch(/^hlxs_[0-9a-f]{48}$/);
    const key = made.body.credential as wfe.ServiceCredential;
    expect(key).toMatchObject({
      name: "Billing bot",
      grants: [],
      revokedAt: null,
    });
    expect(made.body.token.startsWith(key.tokenPrefix!)).toBe(true);
    expect(
      JSON.stringify((await call("GET", `${WS}/service-credentials`)).body),
    ).not.toContain(made.body.token);
    expect(
      refusal(await call("POST", `${WS}/service-credentials`, { name: "" })),
    ).toEqual([400, "name must be 1 to 100 characters"]);
    await grant({ kind: "KIND_SERVICE_CREDENTIAL", id: key.id }, [stripe]);
    expect(
      (await call("DELETE", `${WS}/service-credentials/${key.id}`)).status,
    ).toBe(200);
    expect(
      ((await call("GET", A)).body.grants as wfe.FoundryAccessGrant[]).some(
        (g) => g.principal!.id === key.id,
      ),
    ).toBe(false);
    const listed = (await call("GET", `${WS}/service-credentials`)).body
      .credentials;
    expect(
      listed.find((c: wfe.ServiceCredential) => c.id === key.id).revokedAt,
    ).toEqual(expect.any(String));
    expect(
      (await call("DELETE", `${WS}/service-credentials/${key.id}`)).status,
    ).toBe(404);
    expect(
      (await call("GET", `${WS}/service-credentials`, undefined, "usr_alan"))
        .status,
    ).toBe(403);
  });
});
