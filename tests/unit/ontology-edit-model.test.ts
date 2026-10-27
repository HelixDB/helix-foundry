import { describe, expect, it } from "vitest";
import * as toml from "smol-toml";
import fixture from "../../mock/fixtures/agent-bundle-contracts.json";
import { readAgentBundle } from "@/web/areas/ontology/model";
import * as edit from "@/web/areas/ontology/edit-model";
import type * as wfe from "@/shared/wfe";
const bundle = readAgentBundle(fixture);
const items = edit.editItems(bundle);
const put = (itemId: string, toml: string): wfe.OntologyEdit => ({
  put: { itemId, toml },
});
const rule = put(
  "rule.large",
  "[rules.large]\ndescription = \"Large invoices.\"\ndatalog = 'large(i) :- Invoice(i), i.amount_due > 1000000.'",
);

describe("per-item editing", () => {
  it("reads full nested items from files even when the draft has partial snippets", () => {
    expect(items["extraction.contracts"]).toContain("signed_on");
    expect(items["type.Customer"]).toContain("identifiers");
    expect(items["mapping.customers"]).toBeUndefined();
    expect(
      edit.editItems(
        readAgentBundle({
          format: "agent_bundle",
          ontology_toml: "",
          mappings_toml: "",
        }),
      ),
    ).toEqual({});
  });
  it.each(Object.entries(items))(
    "accepts the unchanged %s item",
    (id, source) => {
      expect(edit.validateEdit(bundle, put(id, source))).toBeUndefined();
    },
  );
  it("accepts value rules and constraints, including date values and null", () => {
    expect(edit.validateEdit(bundle, rule)).toBeUndefined();
    expect(
      edit.validateEdit(
        bundle,
        put(
          "rule.overdue",
          "[rules.overdue]\ndescription = \"Signed over 30 days ago.\"\ndatalog = 'overdue(c) :- Contract(c), c.signed_on < today - 30d.'",
        ),
      ),
    ).toBeUndefined();
    expect(
      edit.validateEdit(
        bundle,
        put(
          "constraint.dated",
          '[constraints.dated]\ndescription = "Signed contracts are dated."\nseverity = "warn"\ndatalog = \'violation(c) :- Contract(c), c.signed_on = null.\'',
        ),
      ),
    ).toBeUndefined();
  });
  it.each([
    "mapping.customers",
    "link.signed_with",
    "relation.place",
    "cq.CQ1",
    "type.Customer.props",
    "extraction.",
    "rule.bad name",
    "rule.a.b",
  ])("refuses disallowed id %s", (id) => {
    expect(edit.validateEdit(bundle, put(id, ""))).toContain("EditNotAllowed");
  });
  it.each([
    ["type.Customer", "[types.Customer]\nprops = {}"],
    ["type.New", "[types.New]\nidentifiers = {}"],
    ["rule.large", '[rules.large]\ndatalog = "x"\n[types.New]'],
    ["rule.large", '[rules.other]\ndatalog = "x"'],
    ["rule.large", '[constraints.large]\nseverity = "warn"'],
    ["rule.large", 'rules = "x"'],
    ["rule.large", 'rules = { large = "x" }'],
  ])("refuses a boundary/schema change for %s", (id, source) =>
    expect(edit.validateEdit(bundle, put(id, source))).toContain(
      "EditNotAllowed",
    ),
  );
  it("reports malformed TOML and malformed original files", () => {
    expect(edit.validateEdit(bundle, put("rule.large", "["))).toContain(
      "Invalid TOML",
    );
    expect(edit.validateEdit({ ...bundle, mappingsToml: "[" }, rule)).toContain(
      "Invalid TOML",
    );
  });
  it("allows removal only for an existing non-type item", () => {
    expect(
      edit.validateEdit(bundle, { remove: { itemId: "extraction.contracts" } }),
    ).toBeUndefined();
    expect(
      edit.validateEdit(bundle, { remove: { itemId: "type.Customer" } }),
    ).toContain("EditNotAllowed");
    expect(
      edit.validateEdit(bundle, { remove: { itemId: "rule.missing" } }),
    ).toContain("does not exist");
  });
  it("changes identifiers without losing fields or depending on TOML order", () => {
    const e = edit.identifierEdit(bundle, "Customer", { email: "exact" });
    expect(edit.validateEdit(bundle, e)).toBeUndefined();
    const doc = toml.parse(e.put!.toml);
    expect(doc.types).toMatchObject({
      Customer: {
        key: ["customer_id"],
        names: ["company_name"],
        identifiers: { email: "exact" },
      },
    });
    expect(
      edit.validateEdit(bundle, edit.identifierEdit(bundle, "Customer", {})),
    ).toBeUndefined();
    expect(
      edit.validateEdit(
        bundle,
        edit.identifierEdit(bundle, "Invoice", { amount_due: "number" }),
      ),
    ).toBeUndefined();
  });
  it.each([
    [{ missing: "code" }, "UnknownProp"],
    [{ email: "other" }, "Unknown identifier"],
    [{ email: "number" }, "IdentifierType"],
  ] as const)("rejects invalid identifiers %j", (ids, message) =>
    expect(
      edit.validateEdit(bundle, edit.identifierEdit(bundle, "Customer", ids)),
    ).toContain(message),
  );
  it("rejects identifiers when a type has no property map", () => {
    const empty = readAgentBundle({
      format: "agent_bundle",
      ontology_toml: "[types.Empty]",
      mappings_toml: "",
    });
    expect(
      edit.validateEdit(
        empty,
        edit.identifierEdit(empty, "Empty", { missing: "exact" }),
      ),
    ).toContain("UnknownProp");
  });
  it("rejects a non-map identifiers field", () => {
    const doc = toml.parse(items["type.Customer"]!) as any;
    doc.types.Customer.identifiers = "email";
    expect(
      edit.validateEdit(bundle, put("type.Customer", toml.stringify(doc))),
    ).toContain("map properties");
    delete doc.types.Customer.identifiers;
    expect(
      edit.validateEdit(bundle, put("type.Customer", toml.stringify(doc))),
    ).toBeUndefined();
  });
  it.each([
    ["documents = []\n[extractions.x.types.Customer]", "nonempty"],
    ["documents = [1]\n[extractions.x.types.Customer]", "nonempty"],
    ['documents = [""]\n[extractions.x.types.Customer]', "nonempty"],
    ['documents = "all"', "scoped types"],
    ['documents = "all"\n[extractions.x.types.Missing]', "Unknown type"],
    ['documents = "all"\ntypes = { Customer = "wrong" }', "Unknown type"],
    [
      'documents = "all"\n[extractions.x.types.Customer]\ncreate = "yes"',
      "Create",
    ],
    [
      'documents = "all"\n[extractions.x.types.Customer]\nprops = ["missing"]',
      "UnknownProp",
    ],
    [
      'documents = "all"\n[extractions.x.types.Customer]\nprops = "email"',
      "UnknownProp",
    ],
    [
      'documents = "all"\nlinks = ["signed_with"]\n[extractions.x.types.Customer]',
      "both ends",
    ],
    [
      'documents = "all"\nlinks = ["missing"]\n[extractions.x.types.Customer]',
      "both ends",
    ],
    [
      'documents = "all"\nlinks = "x"\n[extractions.x.types.Customer]',
      "both ends",
    ],
    [
      'documents = "all"\nextra = true\n[extractions.x.types.Customer]',
      "Unknown route",
    ],
    [
      'documents = "all"\n[extractions.x.types.Customer]\nextra = true',
      "Unknown route",
    ],
  ])("rejects invalid route scope %#", (source, message) =>
    expect(
      edit.validateEdit(
        bundle,
        put("extraction.x", `[extractions.x]\n${source}`),
      ),
    ).toContain(message),
  );
  it("accepts a route with all documents, implicit defaults and scoped link ends", () => {
    expect(
      edit.validateEdit(
        bundle,
        put(
          "extraction.x",
          '[extractions.x]\ndocuments = "all"\nlinks = ["signed_with"]\n[extractions.x.types.Customer]\n[extractions.x.types.Contract]\ncreate = true\nprops = ["signed_on"]',
        ),
      ),
    ).toBeUndefined();
    expect(
      edit.validateEdit(
        { ...bundle, ontology: { ok: false, error: "bad" } },
        put(
          "extraction.x",
          '[extractions.x]\ndocuments = "all"\n[extractions.x.types.Customer]',
        ),
      ),
    ).toContain("Unknown type");
  });
  it.each([
    // The engine's RuleShape and ConstraintShape require a description.
    ["rule.x", '[rules.x]\ndatalog = "x"', "description"],
    ["rule.x", '[rules.x]\ndescription = " "\ndatalog = "x"', "description"],
    ["rule.x", '[rules.x]\ndescription = 1\ndatalog = "x"', "description"],
    [
      "constraint.x",
      '[constraints.x]\nseverity = "warn"\ndatalog = "x"',
      "description",
    ],
    ["rule.x", '[rules.x]\ndescription = "d"', "Datalog"],
    ["rule.x", '[rules.x]\ndescription = "d"\ndatalog = " "', "Datalog"],
    ["rule.x", '[rules.x]\ndescription = "d"\ndatalog = 1', "Datalog"],
    [
      "rule.x",
      '[rules.x]\ndescription = "d"\ndatalog = "x"\nseverity = "warn"',
      "Unknown rule",
    ],
    [
      "constraint.x",
      '[constraints.x]\ndescription = "d"\ndatalog = "x"\nseverity = "danger"',
      "severity",
    ],
    [
      "constraint.x",
      '[constraints.x]\ndescription = "d"\ndatalog = "x"',
      "severity",
    ],
  ])("rejects invalid rule fields %#", (id, source, message) =>
    expect(edit.validateEdit(bundle, put(id, source))).toContain(message),
  );
  it.each(["block", "warn", "info"])("accepts %s constraints", (severity) =>
    expect(
      edit.validateEdit(
        bundle,
        put(
          "constraint.x",
          `[constraints.x]\ndescription = "d"\ndatalog = 'violation(i) :- Invoice(i). '\nseverity = "${severity}"`,
        ),
      ),
    ).toBeUndefined(),
  );
  it("stages one operation per item, removes an operation on revert and preserves other edits", () => {
    const changed = edit.identifierEdit(bundle, "Customer", { email: "exact" });
    const edits = edit.stageEdit(bundle, [rule], changed);
    expect(edits).toEqual([rule, changed]);
    expect(
      edit.stageEdit(
        bundle,
        edits,
        put("type.Customer", items["type.Customer"]!),
      ),
    ).toEqual([rule]);
    expect(
      edit.stageEdit(bundle, edits, {
        remove: { itemId: "extraction.contracts" },
      }),
    ).toHaveLength(3);
    expect(
      edit.stageEdit(bundle, [rule], {
        put: { ...rule.put!, toml: rule.put!.toml + "\n" },
      }),
    ).toHaveLength(1);
  });
  it("stages nothing for an untouched type without identifiers, and removes them all by leaving the key out", () => {
    const untouched = edit.identifierEdit(bundle, "Invoice", {});
    expect(untouched.put?.toml).not.toContain("identifiers");
    expect(edit.stageEdit(bundle, [], untouched)).toEqual([]);
    const none = edit.identifierEdit(bundle, "Customer", {});
    expect(toml.parse(none.put!.toml).types).toEqual({
      Customer: expect.not.objectContaining({ identifiers: expect.anything() }),
    });
    expect(edit.validateEdit(bundle, none)).toBeUndefined();
    expect(edit.stageEdit(bundle, [], none)).toEqual([none]);
  });
});

describe("route document sets", () => {
  const sets = new Map([
    ["set_acme_general", "General"],
    ["set_acme_board", "Board papers"],
  ]);
  const route = (documents: string) =>
    put(
      "extraction.contracts",
      `[extractions.contracts]\ndocuments = ${documents}\n[extractions.contracts.types.Contract]\ncreate = true`,
    );
  it("accepts set ids the ontology has, and all documents", () => {
    expect(
      edit.routeSetRefusal(
        route('["set_acme_general", "set_acme_board"]'),
        sets,
      ),
    ).toBeUndefined();
    expect(edit.routeSetRefusal(route('"all"'), sets)).toBeUndefined();
    expect(
      edit.routeSetRefusal(
        put("extraction.contracts", items["extraction.contracts"]!),
        sets,
      ),
    ).toBeUndefined();
  });
  it("refuses a set's name for its id, and an id no set has", () => {
    expect(edit.routeSetRefusal(route('["General"]'), sets)).toBe(
      "General is a set's name. Use its id: set_acme_general.",
    );
    expect(
      edit.routeSetRefusal(
        route('["set_acme_general", "set_acme_genral"]'),
        sets,
      ),
    ).toBe("No document set has the id set_acme_genral.");
    expect(edit.routeSetRefusal(route('["set_acme_general"]'), new Map())).toBe(
      "No document set has the id set_acme_general.",
    );
  });
  it("checks only a route being put", () => {
    expect(edit.routeSetRefusal(rule, new Map())).toBeUndefined();
    expect(
      edit.routeSetRefusal(
        { remove: { itemId: "extraction.contracts" } },
        new Map(),
      ),
    ).toBeUndefined();
  });
});

describe("preview contract", () => {
  const batch = [rule],
    snapshot = JSON.stringify(batch);
  const result = JSON.stringify({
    status: "revised",
    findings: [],
    cards: [],
    rules: { count: 0, evaluated: 0 },
  });
  const g: wfe.OntologyGeneration = {
    id: "g",
    status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
    revision: { parentVersionId: "v", dryRun: true, resultJson: result },
  };
  const revision = (r: { dryRun?: boolean; resultJson?: string }) => ({
    ...g,
    revision: { parentVersionId: "v", dryRun: true, resultJson: result, ...r },
  });
  // Without query access to the ontology's database, the result is withheld.
  const withheld: wfe.OntologyGeneration = {
    ...g,
    revision: { parentVersionId: "v", dryRun: true, resultWithheld: true },
  };
  it("allows only a completed successful dry run of the exact batch", () =>
    expect(edit.canCreateRevision(g, snapshot, batch)).toBe(true));
  it("allows a successful dry run whose result is withheld: Revise fails a refusal", () => {
    expect(edit.canCreateRevision(withheld, snapshot, batch)).toBe(true);
    for (const status of [
      "ONTOLOGY_GENERATION_STATUS_FAILED",
      "ONTOLOGY_GENERATION_STATUS_RUNNING",
    ] as const)
      expect(
        edit.canCreateRevision({ ...withheld, status }, snapshot, batch),
      ).toBe(false);
    expect(edit.canCreateRevision(withheld, "stale", batch)).toBe(false);
  });
  it.each([
    undefined,
    null,
    revision({ dryRun: false }),
    { ...g, revision: null },
    { ...g, resultVersionId: "unexpected" },
    revision({ resultJson: "invalid" }),
    { ...g, status: "ONTOLOGY_GENERATION_STATUS_RUNNING" },
    { ...g, status: "ONTOLOGY_GENERATION_STATUS_FAILED" },
    revision({
      resultJson: JSON.stringify({
        status: "refused",
        findings: [],
        cards: [],
      }),
    }),
    // A list's revision has no result: only a read of the generation does.
    revision({ resultJson: "" }),
  ] satisfies (wfe.OntologyGeneration | null | undefined)[])(
    "refuses an incomplete or refused preview %#",
    (g) => expect(edit.canCreateRevision(g, snapshot, batch)).toBe(false),
  );
  it("invalidates preview on any batch change or empty batch", () => {
    expect(edit.canCreateRevision(g, "stale", batch)).toBe(false);
    expect(edit.canCreateRevision(g, "[]", [])).toBe(false);
  });
});

describe("stored revision result", () => {
  it.each([
    undefined,
    "",
    "{",
    "null",
    "[]",
    "1",
    '"revised"',
    "{}",
    '{"status":"other","findings":[],"cards":[]}',
    '{"status":"revised","cards":[]}',
    '{"status":"revised","findings":[],"cards":{}}',
    '{"status":"revised","findings":[],"cards":[]}',
    '{"status":"revised","findings":[],"cards":[],"rules":{"count":1,"evaluated":2}}',
  ])("refuses missing or malformed envelopes: %s", (raw) =>
    expect(edit.readRevisionResult(raw)).toBeUndefined(),
  );
  it.each(["revised", "refused"] as const)(
    "preserves the engine's finding and card envelopes for %s",
    (status) => {
      const result = {
        status,
        ...(status === "revised" ? { rules: { count: 1, evaluated: 1 } } : {}),
        findings: [
          {
            code: "Lint",
            item_id: null,
            message: "<script>untrusted</script>",
          },
        ],
        cards: [
          {
            item: "constraint.check",
            verdict: "warn",
            card: { unmeasured: "load", reason: "untrusted" },
          },
        ],
      };
      expect(edit.readRevisionResult(JSON.stringify(result))).toEqual(result);
    },
  );
  it.each(["documents", "load", "entities", null])(
    "reads a card unmeasured on %s",
    (unmeasured) => {
      const result = {
        status: "refused",
        findings: [],
        cards: [
          { item: "constraint.check", verdict: "warn", card: { unmeasured } },
        ],
      };
      expect(edit.readRevisionResult(JSON.stringify(result))).toEqual(result);
    },
  );
  it("reads a refusal for measuring past the budget: one finding, no cards", () => {
    const result = {
      status: "refused",
      findings: [
        {
          code: "budget",
          item_id: null,
          message:
            "Measuring the revision on the load needs more than its budget.",
        },
      ],
      cards: [],
    };
    expect(edit.readRevisionResult(JSON.stringify(result))).toEqual(result);
  });
  it.each([
    { status: "revised", rules: { count: 2, evaluated: 1 } },
    // A budget finding never accompanies an accepted revision.
    {
      status: "revised",
      rules: { count: 0, evaluated: 0 },
      findings: [{ code: "budget", item_id: null, message: "x" }],
    },
    { status: "refused", rules: { count: 0, evaluated: 0 } },
    { status: "refused", rules: null },
    { status: "refused", definition: null },
    { status: "refused", definition: {} },
    ...[
      null,
      {},
      { code: "future", item_id: null, message: "x" },
      { code: "Lint", message: "x" },
      { code: "Lint", item_id: 1, message: "x" },
      { code: "Lint", item_id: null, message: null },
    ].map((f) => ({ status: "refused", findings: [f] })),
    ...[
      null,
      {},
      { item_id: "rule.a", verdict: "rule", card: {} },
      { item: "rule.a", verdict: "future", card: {} },
      { item: "rule.a", verdict: "rule", card: [] },
      { item: "rule.a", verdict: "warn", card: { unmeasured: "future" } },
      { item: "rule.a", verdict: "warn", card: { unmeasured: 1 } },
    ].map((c) => ({ status: "refused", cards: [c] })),
  ])("rejects invalid engine evidence and unions %#", (value) => {
    expect(
      edit.readRevisionResult(
        JSON.stringify({ findings: [], cards: [], ...value }),
      ),
    ).toBeUndefined();
  });
});
