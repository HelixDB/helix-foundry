import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as toml from "smol-toml";
import fixture from "../../mock/fixtures/agent-bundle-contracts.json";
import { db, resetState, config } from "../../mock/state";
import { routes } from "../../mock/routes";
import {
  compileTemplate,
  matchTemplate,
  type HttpMethod,
} from "@/shared/route-template";
import {
  comparableSchema,
  reviseDefinition,
} from "../../mock/routes/ontology-revise";
import {
  editItems,
  identifierEdit,
  readRevisionResult,
} from "@/web/areas/ontology/edit-model";
import { readAgentBundle } from "@/web/areas/ontology/model";
import { salesGraphBundle } from "../../mock/fixtures/sales-graph";
import { sourceId } from "../../mock/routes/ontology-agent";
import type * as wfe from "@/shared/wfe";
const O = "/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts";
const bundle = readAgentBundle(fixture);
const constraintToml =
  '[constraints.large]\ndescription = "Large invoices need approval."\nseverity = "warn"\ndatalog = \'violation(i) :- Invoice(i), i.amount_due > 1000000.\'';
/** constraint.large, put with this TOML. */
const large = (toml: string): wfe.OntologyEdit => ({
  put: { itemId: "constraint.large", toml },
});
const constraint = large(constraintToml);
const routeDoc = toml.parse(editItems(bundle)["extraction.contracts"]!) as any;
routeDoc.extractions.contracts.documents = "all";
const routeToml = toml.stringify(routeDoc);
const route: wfe.OntologyEdit = {
  put: { itemId: "extraction.contracts", toml: routeToml },
};

async function call(
  method: HttpMethod,
  path: string,
  body?: unknown,
  user = "usr_ada",
) {
  const url = new URL(path, "http://mock");
  for (const r of routes) {
    const params =
      r.method === method &&
      matchTemplate(compileTemplate(r.path), url.pathname);
    if (params)
      return (await r.handle({
        method,
        path: url.pathname,
        params,
        query: url.searchParams,
        body,
        principal: { user: db.users.get(user)!, sessionId: "s" },
      })) as { status: number; body: any };
  }
  throw new Error(`Missing mock route: ${path}`);
}
async function revise(
  edits: wfe.OntologyEdit[] = [route],
  dryRun = true,
  outcome = "succeed",
) {
  Object.assign(config, { ontologyStepMs: 10, ontologyOutcome: outcome });
  const created = await call("POST", `${O}/versions/ver_seed_3:revise`, {
    edits,
    dryRun,
  });
  expect(created.status).toBe(200);
  vi.setSystemTime(Date.now() + 100);
  return (await call("GET", `${O}/generations/${created.body.generation.id}`))
    .body.generation as wfe.OntologyGeneration;
}
async function load() {
  Object.assign(config, { loadStepMs: 10 });
  await call("POST", `${O}:set-current`, {
    versionId: "ver_seed_3",
    expectedCurrentVersionId: "ver_seed_1",
  });
  const created = await call("POST", `${O}/loads`, {
    versionId: "ver_seed_3",
    clientToken: "l",
  });
  expect(created.status).toBe(200);
  for (let n = 0; n < 6; n++) {
    vi.setSystemTime(Date.now() + 1000);
    await call("GET", `${O}/loads`);
  }
}
beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-19T12:00:00Z"));
  resetState();
});
afterEach(() => vi.useRealTimers());

describe("Revise contract mock", () => {
  it("stores a dry-run result without a new version or changing current", async () => {
    const g = await revise();
    expect(g).toMatchObject({
      baseVersionId: "",
      revision: {
        parentVersionId: "ver_seed_3",
        dryRun: true,
        resultJson: JSON.stringify({
          status: "revised",
          findings: [],
          cards: [],
          rules: { count: 5, evaluated: 5 },
        }),
      },
      status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
      resultVersionId: "",
    });
    // Lists leave the result out: only a read of the generation has it.
    expect(
      (await call("GET", `${O}/generations`)).body.generations[0],
    ).toMatchObject({
      id: g.id,
      revision: { parentVersionId: "ver_seed_3", dryRun: true },
    });
    expect(
      (await call("GET", `${O}/generations`)).body.generations[0].revision,
    ).not.toHaveProperty("resultJson");
    expect(readRevisionResult(g.revision?.resultJson)).not.toHaveProperty(
      "definition",
    );
    expect((await call("GET", `${O}/versions`)).body.versions).toHaveLength(3);
    expect((await call("GET", O)).body.ontology.currentVersionId).toBe(
      "ver_seed_1",
    );
  });
  it("creates an immutable derived version, retains tables, rules/cards and leaves pointers unchanged", async () => {
    await load();
    const before = (await call("GET", `${O}/versions/ver_seed_3`)).body.version;
    const preview = await revise([route, constraint]);
    // A comparable load, of which the mock reads no entity.
    expect(readRevisionResult(preview.revision?.resultJson)).toMatchObject({
      status: "revised",
      findings: [],
      cards: [
        {
          item: "constraint.large",
          verdict: "warn",
          card: { unmeasured: "entities" },
        },
      ],
    });
    expect(preview.resultVersionId).toBe("");
    const g = await revise([route, constraint], false);
    expect(g.tables).toEqual([
      { connectorId: "conn_acme_stripe", table: "customers" },
      { connectorId: "conn_acme_stripe", table: "invoices" },
    ]);
    // It reads its parent version's scope, not the sets its parent's
    // generation picked (one of them deleted since).
    expect(g.documentSetIds).toEqual(["set_acme_general"]);
    const next = (await call("GET", `${O}/versions/${g.resultVersionId}`)).body
      .version;
    expect(next).toMatchObject({
      version: 4,
      generationId: g.id,
      source: "ONTOLOGY_SOURCE_MANUAL",
      definition: { parent_version_id: "ver_seed_3" },
      isCurrent: false,
    });
    expect(next.documentSetIds).toEqual(["set_acme_general"]);
    expect(next.definition.ontology_toml).toContain("[constraints.large]");
    expect(toml.parse(next.definition.mappings_toml).extractions).toMatchObject(
      { contracts: { documents: "all" } },
    );
    expect(
      readAgentBundle(next.definition).items["constraint.large"],
    ).toMatchObject({
      verdict: "warn",
      card: { unmeasured: "entities", instances: 0 },
      status: "user_edited",
    });
    expect(
      (await call("GET", `${O}/versions/ver_seed_3`)).body.version,
    ).toEqual(before);
    expect((await call("GET", O)).body.ontology.currentVersionId).toBe(
      "ver_seed_3",
    );
    expect((await call("GET", `${O}/versions`)).body.versions).toHaveLength(4);
    expect(next.definition.rules).toEqual({ count: 6, evaluated: 6 });
    expect(readRevisionResult(g.revision?.resultJson)?.rules).toEqual({
      count: 6,
      evaluated: 6,
    });
    const loaded = await call("POST", `${O}/loads`, {
      versionId: next.id,
      clientToken: "unevaluated",
    });
    expect(loaded.status).toBe(200);
  });
  it("withholds what Revise measured on the load from one who can't query its database", async () => {
    const g = await revise([constraint], false);
    const own = await call("GET", `${O}/versions/${g.resultVersionId}`);
    expect(own.body.version).not.toHaveProperty("evidenceWithheld");
    Object.assign(config, { ontologyDatabaseHidden: true });
    const read = (await call("GET", `${O}/generations/${g.id}`)).body
      .generation as wfe.OntologyGeneration;
    expect(read.revision).toEqual({
      parentVersionId: "ver_seed_3",
      dryRun: false,
      resultWithheld: true,
    });
    // A revised version's items go without their cards, and say so.
    const hidden = (await call("GET", `${O}/versions/${g.resultVersionId}`))
      .body.version as wfe.OntologyVersion;
    expect(hidden.evidenceWithheld).toBe(true);
    const items = readAgentBundle(hidden.definition).items;
    expect(Object.values(items).some((i) => "card" in i)).toBe(false);
    expect(items["constraint.large"]).toMatchObject({ verdict: "warn" });
    expect(
      (
        await call("POST", `${O}:set-current`, {
          versionId: g.resultVersionId,
          expectedCurrentVersionId: "ver_seed_1",
        })
      ).body.version,
    ).toMatchObject({ evidenceWithheld: true, isCurrent: true });
    // A generated draft's cards measure no load: they stay.
    const seeded = (await call("GET", `${O}/versions/ver_seed_3`)).body
      .version as wfe.OntologyVersion;
    expect(seeded).not.toHaveProperty("evidenceWithheld");
    expect(
      Object.values(readAgentBundle(seeded.definition).items).some(
        (i) => i.card != null,
      ),
    ).toBe(true);
    // The stored version keeps them.
    Object.assign(config, { ontologyDatabaseHidden: false });
    expect(
      (await call("GET", `${O}/versions/${g.resultVersionId}`)).body.version
        .definition,
    ).toEqual(own.body.version.definition);
  });
  it("caps constraints without a comparable load at warn and counts them as evaluated", async () => {
    const g = await revise([constraint], false);
    expect(readRevisionResult(g.revision?.resultJson)).toMatchObject({
      status: "revised",
      findings: [{ code: "no_comparable_load" }],
      cards: [
        {
          item: "constraint.large",
          verdict: "warn",
          card: { unmeasured: "load", instances: 0 },
        },
      ],
      rules: { count: 6, evaluated: 6 },
    });
    expect(g.resultVersionId).toBeTruthy();
    expect(g.status).toBe("ONTOLOGY_GENERATION_STATUS_SUCCEEDED");
  });
  it("refuses disallowed edits from direct callers without writing a version", async () => {
    const g = await revise(
      [{ put: { itemId: "mapping.customers", toml: "" } }],
      false,
    );
    expect(readRevisionResult(g.revision?.resultJson)).toMatchObject({
      status: "refused",
      findings: [{ code: "EditNotAllowed" }],
    });
    expect((await call("GET", `${O}/versions`)).body.versions).toHaveLength(3);
  });
  it("keeps queued/running/retry semantics and enforces one live generation", async () => {
    Object.assign(config, { ontologyStepMs: 100, ontologyOutcome: "retry" });
    const body = { edits: [route], dryRun: true };
    const created = (
      await call("POST", `${O}/versions/ver_seed_3:revise`, body)
    ).body.generation;
    expect(created.status).toBe("ONTOLOGY_GENERATION_STATUS_QUEUED");
    expect(
      (await call("POST", `${O}/versions/ver_seed_3:revise`, body)).status,
    ).toBe(409);
    vi.setSystemTime(Date.now() + 110);
    expect(
      (await call("GET", `${O}/generations/${created.id}`)).body.generation
        .status,
    ).toBe("ONTOLOGY_GENERATION_STATUS_RUNNING");
    vi.setSystemTime(Date.now() + 100);
    // A call a retry may clear leaves it queued, with the backend's reason.
    expect(
      (await call("GET", `${O}/generations/${created.id}`)).body.generation,
    ).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_QUEUED",
      attempts: 1,
      error:
        "the ontology agent could not finish the revision; Helix will retry",
      revision: { parentVersionId: "ver_seed_3", dryRun: true },
    });
    expect(
      (await call("GET", `${O}/generations/${created.id}`)).body.generation
        .revision,
    ).not.toHaveProperty("resultJson");
    vi.setSystemTime(Date.now() + 200);
    expect(
      (await call("GET", `${O}/generations/${created.id}`)).body.generation,
    ).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
      attempts: 2,
    });
  });
  it("refuses a revision measured past its budget, and resets the knob", async () => {
    await load();
    Object.assign(config, { reviseOutcome: "budget" });
    const g = await revise([constraint], false);
    expect(g).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_FAILED",
      resultVersionId: "",
    });
    expect(readRevisionResult(g.revision?.resultJson)).toEqual({
      status: "refused",
      findings: [expect.objectContaining({ code: "budget", item_id: null })],
      cards: [],
    });
    resetState();
    expect(config).not.toHaveProperty("reviseOutcome");
  });
  it("names no revision: the backend copies the parent's goal and writes no title", async () => {
    const g = await revise();
    expect(g).toMatchObject({
      revision: { parentVersionId: "ver_seed_3" },
      title: "",
      goal: "Contracts with our customers: who we signed with and what they are billed.",
    });
    const generated = (await call("GET", `${O}/generations`)).body
      .generations as wfe.OntologyGeneration[];
    expect(
      generated.filter((x) => x.id !== g.id).map((x) => x.revision),
    ).toEqual([null, null, null, null]);
  });
  it("revises seeded version 2, whose rule counts match its evaluated items", async () => {
    const v2 = (await call("GET", `${O}/versions/ver_seed_2`)).body.version;
    const shop = readAgentBundle(v2.definition);
    const gates = Object.entries(shop.items).filter(([id]) =>
      /^(rule|constraint)\./.test(id),
    );
    expect(gates.map(([id, it]) => [id, it.verdict, it.pending])).toEqual([
      ["rule.order_products", "rule", null],
      ["constraint.return_was_ordered", "rule", null],
    ]);
    expect(v2.definition.rules).toEqual({ count: 2, evaluated: 2 });
    Object.assign(config, { ontologyStepMs: 10 });
    const created = await call("POST", `${O}/versions/ver_seed_2:revise`, {
      edits: [identifierEdit(shop, "Customer", { customer_number: "code" })],
      dryRun: true,
    });
    expect(created.status).toBe(200);
    vi.setSystemTime(Date.now() + 100);
    const g = (
      await call("GET", `${O}/generations/${created.body.generation.id}`)
    ).body.generation;
    expect(readRevisionResult(g.revision?.resultJson)).toMatchObject({
      status: "revised",
      rules: { count: 2, evaluated: 2 },
    });
  });
  it("surfaces a worker failure without storing a version", async () => {
    expect(await revise([route], false, "fail")).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_FAILED",
      resultVersionId: "",
    });
  });
  it.each([
    {},
    { edits: [], dryRun: true },
    { edits: [route, route], dryRun: true },
    { edits: [route], dryRun: "true" },
    {
      edits: Array.from({ length: 101 }, (_, i) => ({
        put: { itemId: `extraction.r${i}`, toml: routeToml },
      })),
    },
    { edits: [{ put: { itemId: " ", toml: routeToml } }] },
    { edits: [{ put: { toml: routeToml } }] },
    { edits: [{ put: { itemId: "é".repeat(129), toml: routeToml } }] },
    {
      edits: [
        {
          put: {
            itemId: "extraction.contracts",
            toml: "é".repeat((1 << 19) + 1),
          },
        },
      ],
    },
    { edits: [{ op: { itemId: "extraction.contracts", toml: routeToml } }] },
    { edits: [{}] },
    { edits: [null], dryRun: false },
    // The old flat shape.
    {
      edits: [{ operation: "OPERATION_REMOVE", itemId: "rule.x" }],
      dryRun: true,
    },
    {
      edits: [
        { put: { itemId: "rule.x", toml: "" }, remove: { itemId: "rule.x" } },
      ],
      dryRun: true,
    },
    { edits: [{ remove: { itemId: "rule.x", toml: "" } }], dryRun: true },
    { edits: [{ remove: "rule.x" }], dryRun: true },
    { edits: [{ put: { itemId: "rule.x", toml: 1 } }], dryRun: true },
  ])("rejects malformed requests %#", async (body) =>
    expect(
      (await call("POST", `${O}/versions/ver_seed_3:revise`, body)).status,
    ).toBe(400),
  );
  it("defaults omitted dryRun to false and preserves inherited inputs across revisions", async () => {
    Object.assign(config, { ontologyStepMs: 10 });
    const created = await call("POST", `${O}/versions/ver_seed_3:revise`, {
      edits: [route],
    });
    expect(created.status).toBe(200);
    expect(created.body.generation).toMatchObject({
      revision: { parentVersionId: "ver_seed_3", dryRun: false },
      baseVersionId: "",
    });
    vi.setSystemTime(Date.now() + 100);
    const g = (
      await call("GET", `${O}/generations/${created.body.generation.id}`)
    ).body.generation;
    expect(g.status).toBe("ONTOLOGY_GENERATION_STATUS_SUCCEEDED");
    expect(g.resultVersionId).toBeTruthy();
    const again = await call(
      "POST",
      `${O}/versions/${g.resultVersionId}:revise`,
      { edits: [route], dryRun: true },
    );
    expect(again.status).toBe(200);
    expect(again.body.generation).toMatchObject({
      tables: g.tables,
      documentSetIds: g.documentSetIds,
      revision: { parentVersionId: g.resultVersionId },
    });
  });
  it("enforces scope, membership, management rights and format", async () => {
    const body = { edits: [route], dryRun: true };
    expect(
      (await call("POST", `${O}/versions/ver_seed_3:revise`, body, "usr_alan"))
        .status,
    ).toBe(403);
    expect(
      (
        await call(
          "POST",
          `${O.replace("ws_acme", "ws_other")}/versions/ver_seed_3:revise`,
          body,
        )
      ).status,
    ).toBe(403);
    expect(
      (await call("POST", `${O}/versions/missing:revise`, body)).status,
    ).toBe(404);
    expect(
      (await call("POST", `${O}/versions/ver_seed_1:revise`, body)).status,
    ).toBe(400);
    const other = await call(
      "POST",
      "/v1/workspaces/ws_acme/foundry/ontologies",
      { name: "Other", tenantId: "ten_acme_sandbox", clientToken: "other" },
    );
    expect(other.status).toBe(200);
    expect(
      (
        await call(
          "POST",
          `${O.replace("ont_acme_contracts", other.body.ontology.id)}/versions/ver_seed_3:revise`,
          body,
        )
      ).status,
    ).toBe(404);
  });
});

describe("mock engine boundary", () => {
  it("removes routes and rules while preserving other nested items and identifiers", () => {
    const result = reviseDefinition(
      fixture,
      [
        { remove: { itemId: "extraction.contracts" } },
        identifierEdit(bundle, "Customer", { email: "exact" }),
      ],
      "none",
    );
    expect(result.status).toBe("revised");
    const b = readAgentBundle(result.definition);
    expect(b.routes).toEqual({ ok: true, value: [] });
    expect(b.items["extraction.contracts"]).toBeUndefined();
    expect(
      b.ontology.ok &&
        b.ontology.value.types.find((t) => t.name === "Customer")?.identifiers,
    ).toEqual([{ prop: "email", kind: "exact" }]);
    expect(b.mappings.ok && b.mappings.value).toHaveLength(2);
  });
  it("refuses a time-dependent rule it can't measure, and direct and transitively time-dependent constraints", () => {
    const time: wfe.OntologyEdit = {
      put: {
        itemId: "rule.overdue",
        toml: "[rules.overdue]\ndescription = \"Signed over 30 days ago.\"\ndatalog = 'overdue(c) :- Contract(c), c.signed_on < today - 30d.'",
      },
    };
    const unmeasured = reviseDefinition(fixture, [time], "comparable");
    expect(unmeasured.status).toBe("refused");
    expect(unmeasured).not.toHaveProperty("rules");
    expect(unmeasured).not.toHaveProperty("definition");
    expect(unmeasured.findings).toContainEqual(
      expect.objectContaining({ code: "Evaluation" }),
    );
    const indirect = large(
      '[constraints.large]\ndescription = "Overdue contracts."\nseverity = "warn"\ndatalog = \'violation(c) :- overdue(c).\'',
    );
    expect(
      reviseDefinition(fixture, [time, indirect], "comparable").findings,
    ).toContainEqual(
      expect.objectContaining({
        code: "Validation",
        message: expect.stringContaining("TimeInConstraint"),
      }),
    );
    expect(
      reviseDefinition(
        fixture,
        [
          large(
            constraintToml.replace(
              "i.amount_due > 1000000",
              "i.amount_due < today",
            ),
          ),
        ],
        "comparable",
      ).status,
    ).toBe("refused");
  });
  it("refuses a type or route edit that breaks the People enrichment item, against the item", () => {
    const sales = salesGraphBundle(sourceId("conn_acme_shop", "people"), {
        people: 16,
        usable: 9,
        email: 9,
        linkedin_url: 2,
        no_name: 1,
        shared: 2,
        conflicting: 0,
      }),
      b = readAgentBundle(sales);
    const route: wfe.OntologyEdit = {
      put: {
        itemId: "extraction.bios",
        toml: '[extractions.bios]\ndocuments = "all"\n[extractions.bios.types.Person]\nprops = ["current_title"]',
      },
    };
    for (const [edits, message] of [
      [
        [identifierEdit(b, "Person", { linkedin_url: "exact" })],
        "EnrichmentClueKind: email must be an email identifier.",
      ],
      [
        [route],
        "EnrichmentSharedOwner: current_title is already filled by documents (bios).",
      ],
    ] as const)
      expect(reviseDefinition(sales, [...edits], "none")).toEqual({
        status: "refused",
        findings: [
          { code: "Validation", item_id: "enrichment.people", message },
        ],
        cards: [],
      });
    // Removing the item with it, the edit stands.
    expect(
      reviseDefinition(
        sales,
        [
          identifierEdit(b, "Person", { linkedin_url: "exact" }),
          { remove: { itemId: "enrichment.people" } },
        ],
        "none",
      ).status,
    ).toBe("revised");
  });
  it("compares structural schemas independent of identifiers, rules and key order", () => {
    const ids = reviseDefinition(
      fixture,
      [identifierEdit(bundle, "Customer", {})],
      "comparable",
    ).definition!;
    expect(comparableSchema(fixture, ids)).toBe(true);
    expect(
      comparableSchema(fixture, { ...fixture, format: "foundry_v1" }),
    ).toBe(false);
    expect(
      comparableSchema({ ...fixture, format: "foundry_v1" }, fixture),
    ).toBe(false);
    expect(
      comparableSchema(fixture, {
        ...fixture,
        ontology_toml: fixture.ontology_toml.replace(
          'signed_on = "date"',
          'signed_on = "string"',
        ),
      }),
    ).toBe(false);
    expect(
      comparableSchema(fixture, {
        ...fixture,
        ontology_toml: fixture.ontology_toml.replace(
          'to = "Customer"',
          'to = "Invoice"',
        ),
      }),
    ).toBe(false);
  });
});

it("rewrites an unmeasured block constraint to warn in both files and draft", () => {
  const result = reviseDefinition(
    fixture,
    [large(constraintToml.replace('"warn"', '"block"'))],
    "none",
  );
  expect(result.status).toBe("revised");
  expect(result).toMatchObject({
    rules: { count: 6, evaluated: 6 },
    cards: [
      {
        item: "constraint.large",
        verdict: "warn",
        card: { unmeasured: "load" },
      },
    ],
  });
  const bundle = readAgentBundle(result.definition);
  expect(toml.parse(bundle.ontologyToml).constraints).toMatchObject({
    large: { severity: "warn" },
  });
  expect(bundle.items["constraint.large"]).toMatchObject({
    verdict: "warn",
    card: result.cards[0]!.card,
  });
  expect(bundle.items["constraint.large"]!.toml).toContain('severity = "warn"');
  const ids = reviseDefinition(
    result.definition!,
    [identifierEdit(bundle, "Customer", {})],
    "none",
  );
  expect(ids).toMatchObject({
    status: "revised",
    rules: { count: 6, evaluated: 6 },
    cards: [],
  });
  expect(
    readAgentBundle(ids.definition).items["constraint.large"]!.card,
  ).toEqual(result.cards[0]!.card);
});
it.each([
  ["none", "load"],
  ["comparable", "entities"],
] as const)(
  "refuses unmeasured derived rules on load %s and omits definition/rules",
  (load, unmeasured) => {
    const result = reviseDefinition(
      fixture,
      [
        {
          put: {
            itemId: "rule.large",
            toml: '[rules.large]\ndescription = "Large invoices."\ndatalog = "large(i) :- Invoice(i)."',
          },
        },
      ],
      load,
    );
    expect(result.status).toBe("refused");
    expect(result).not.toHaveProperty("definition");
    expect(result).not.toHaveProperty("rules");
    expect(result.findings).toContainEqual(
      expect.objectContaining({ code: "Evaluation" }),
    );
    expect(result.cards).toMatchObject([
      { item: "rule.large", verdict: "refused", card: { unmeasured } },
    ]);
  },
);
it("caps a constraint on a comparable load whose entities it didn't read at warn", () => {
  const result = reviseDefinition(
    fixture,
    [large(constraintToml.replace('"warn"', '"block"'))],
    "comparable",
  );
  expect(result).toMatchObject({
    status: "revised",
    findings: [],
    cards: [
      {
        item: "constraint.large",
        verdict: "warn",
        card: { unmeasured: "entities" },
      },
    ],
    rules: { count: 6, evaluated: 6 },
  });
  expect(
    readRevisionResult(JSON.stringify({ ...result, definition: undefined })),
  ).toMatchObject({ status: "revised" });
});
it("refuses past its budget with one finding and no cards, only when rules are measured", () => {
  const result = reviseDefinition(fixture, [constraint], "over_budget");
  expect(result).toEqual({
    status: "refused",
    findings: [
      {
        code: "budget",
        item_id: null,
        message: expect.stringContaining("budget"),
      },
    ],
    cards: [],
  });
  expect(readRevisionResult(JSON.stringify(result))).toEqual(result);
  // Nothing to measure: routes and identifiers revise as usual.
  expect(reviseDefinition(fixture, [route], "over_budget").status).toBe(
    "revised",
  );
  // An invalid edit is refused for what is wrong with it first.
  expect(
    reviseDefinition(
      fixture,
      [large('[constraints.large]\nseverity = "warn"')],
      "over_budget",
    ).findings,
  ).toMatchObject([{ code: "InvalidEdit" }]);
});
it("includes stored keys and link identities in structural comparability", () => {
  for (const part of ["key", "identity"]) {
    const ontology = toml.parse(fixture.ontology_toml) as any;
    if (part === "key") ontology.types.Customer.key = ["email"];
    else (Object.values(ontology.links)[0] as any).identity = "pair";
    expect(
      comparableSchema(fixture, {
        ...fixture,
        ontology_toml: toml.stringify(ontology),
      }),
    ).toBe(false);
  }
});

it("retains full-draft declaration counts and refuses pending evidence", () => {
  const accepted = reviseDefinition(fixture, [constraint], "none");
  const definition = accepted.definition! as wfe.AgentBundleDefinition;
  const draft = definition.draft!.draft!;
  const ontology = toml.parse(definition.ontology_toml!) as any;
  ontology.links.signed_with.symmetric = true;
  definition.ontology_toml = toml.stringify(ontology);
  definition.draft!.ontology_toml = definition.ontology_toml;
  draft.items!["link.signed_with.symmetric"] = {
    toml: "[links.signed_with]\nsymmetric = true",
    verdict: "rule",
    card: accepted.cards[0]!.card,
  };
  const edit = identifierEdit(readAgentBundle(definition), "Customer", {});
  const revised = reviseDefinition(definition, [edit], "none");
  expect(revised).toMatchObject({
    status: "revised",
    rules: { count: 7, evaluated: 7 },
  });
  draft.items!["link.signed_with.symmetric"]!.pending = "rule_evidence";
  const refused = reviseDefinition(definition, [edit], "none");
  expect(refused.status).toBe("refused");
  expect(refused).not.toHaveProperty("rules");
  expect(refused).not.toHaveProperty("definition");
});
