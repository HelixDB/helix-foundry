import { describe, expect, it } from "vitest";
import type { FoundryFileReadState } from "@/shared/wfe";
import agentBundle from "../../mock/fixtures/agent-bundle.json";
import contractsBundle from "../../mock/fixtures/agent-bundle-contracts.json";
import foundryDefinition from "../../mock/fixtures/foundry-definition.json";
import {
  draftFromTables,
  fileTable,
  sourceId,
} from "../../mock/routes/ontology-agent";
import {
  agentGraph,
  agentSourceId,
  byDegree,
  fileMapping,
  fieldText,
  foundryGraph,
  generationTitle,
  loadDocumentPlan,
  loadDocumentsText,
  loadSources,
  MAX_GENERATION_SETS,
  mappingSource,
  parseAgentMappings,
  parseAgentOntology,
  parseAgentRoutes,
  pickerSets,
  readAgentBundle,
  readFoundry,
  routesReading,
  scopeNames,
  sourceMatrix,
  startingSets,
  splitSourceText,
  TITLE_MAX,
  titleFromGoal,
  versionRows,
} from "@/web/areas/ontology/model";
import { versionFormat } from "@/web/areas/ontology/format";

describe("versionFormat", () => {
  it("reads the WFE's format first", () => {
    expect(
      versionFormat({ id: "v", format: "ONTOLOGY_FORMAT_AGENT_BUNDLE" }),
    ).toBe("agent_bundle");
    expect(
      versionFormat({
        id: "v",
        format: "ONTOLOGY_FORMAT_FOUNDRY_V1",
        definition: { format: "agent_bundle" },
      }),
    ).toBe("foundry_v1");
  });
  it("falls back to the definition's own format, then Definition v1", () => {
    expect(
      versionFormat({ id: "v", definition: { format: "agent_bundle" } }),
    ).toBe("agent_bundle");
    expect(
      versionFormat({ id: "v", format: "ONTOLOGY_FORMAT_UNSPECIFIED" }),
    ).toBe("foundry_v1");
    expect(versionFormat({ id: "v" })).toBe("foundry_v1");
  });
});

describe("Definition v1", () => {
  const view = readFoundry(foundryDefinition);
  it("reads entities, relationships, rules and constraints", () => {
    expect(view.entities.map((e) => e.name)).toEqual([
      "Customer",
      "Invoice",
      "Subscription",
      "Charge",
    ]);
    expect(view.entities[0]!.properties[0]).toEqual({
      name: "email",
      type: "string",
      description: "Billing email.",
    });
    expect(view.relationships).toHaveLength(4);
    expect(view.rules.map((r) => r.name)).toEqual([
      "unpaid_invoice",
      "churn_risk",
    ]);
    expect(view.constraints.find((c) => c.kind === "check")).toMatchObject({
      properties: ["amount"],
      expression: "amount >= 0",
    });
  });
  it("draws one node per entity and one edge per relationship", () => {
    const graph = foundryGraph(view);
    expect(graph.nodes[1]).toMatchObject({
      name: "Invoice",
      key: ["invoice_id"],
      sources: [
        { kind: "table", connectorId: "conn_acme_stripe", table: "invoices" },
      ],
    });
    expect(graph.edges.find((e) => e.name === "pays")).toMatchObject({
      from: "Charge",
      to: "Invoice",
      cardinality: "many_to_one",
      via: "Charge.invoice_id = Invoice.invoice_id",
    });
    expect(byDegree(graph)[0]!.name).toBe("Customer");
  });
  it("skips what it cannot read rather than failing", () => {
    const view = readFoundry({
      entities: [
        { name: "A", properties: [{ type: "string" }, { name: "x" }] },
        { description: "no name" },
        "nonsense",
      ],
      relationships: "nope",
      rules: [{ name: "r" }],
    });
    expect(view.entities).toHaveLength(1);
    expect(view.entities[0]!.properties).toEqual([
      { name: "x", type: "", description: "" },
    ]);
    expect(view.relationships).toEqual([]);
    expect(view.rules).toEqual([
      { name: "r", description: "", entity: "", expression: "" },
    ]);
    expect(foundryGraph(view).nodes[0]!.sources).toEqual([]);
    expect(readFoundry(null)).toEqual(readFoundry());
    expect(readAgentBundle(null).ontology).toEqual({
      ok: true,
      value: {
        types: [],
        links: [],
        relations: [],
        rules: [],
        constraints: [],
      },
    });
    expect(readFoundry()).toEqual({
      entities: [],
      relationships: [],
      rules: [],
      constraints: [],
    });
  });
});

describe("agent_bundle", () => {
  const bundle = readAgentBundle(agentBundle);
  it("parses ontology.toml", () => {
    expect(bundle.ontology.ok).toBe(true);
    if (!bundle.ontology.ok) return;
    const { types, links, rules, constraints } = bundle.ontology.value;
    expect(types.map((t) => t.name)).toEqual([
      "Customer",
      "Order",
      "OrderLine",
      "Parcel",
      "Product",
      "Return",
      "SalesChannel",
      "Shipment",
    ]);
    expect(types.find((t) => t.name === "OrderLine")).toMatchObject({
      key: ["order_number", "line_no"],
      props: [
        { name: "line_no", type: "integer" },
        { name: "order_number", type: "string" },
        { name: "qty", type: "integer" },
      ],
    });
    expect(types.find((t) => t.name === "Product")!.names).toEqual(["title"]);
    expect(links.find((l) => l.name === "placed_by")).toMatchObject({
      from: "Order",
      to: "Customer",
      cardinality: "one",
      required: true,
    });
    expect(links.find((l) => l.name === "for_product")!.required).toBe(false);
    expect(rules[0]!.datalog).toContain("on_order(o, p)");
    expect(constraints[0]).toMatchObject({
      name: "return_was_ordered",
      severity: "warn",
    });
  });
  it("parses mappings.toml per source and object", () => {
    expect(bundle.mappings.ok).toBe(true);
    if (!bundle.mappings.ok) return;
    const mappings = bundle.mappings.value;
    expect(mappings.map((m) => m.name)).toEqual([
      "customers",
      "order_lines",
      "orders",
      "parcels",
      "products",
      "returns",
      "shipments",
    ]);
    const orders = mappings.find((m) => m.name === "orders")!;
    expect(orders.rows).toEqual({
      kind: "source",
      source: "src_4cfe9b3e7852efa5",
      object: undefined,
    });
    expect(orders.links).toEqual([
      { link: "placed_by", from: "order", to: "customer" },
      { link: "sold_through", from: "order", to: "channel" },
    ]);
    const customer = orders.bindings.find((b) => b.alias === "customer")!;
    expect(customer).toMatchObject({ type: "Customer", missing: "report" });
    expect(customer.identify).toEqual({
      kind: "lookup",
      mapping: "customers",
      on: [["id", { field: "customer_id", transform: [], parse: undefined }]],
    });
    const order = orders.bindings.find((b) => b.alias === "order")!;
    expect(order.identify).toEqual({
      kind: "key",
      fields: [
        [
          "order_number",
          {
            field: "order_number",
            transform: ["upper", "collapse_ws"],
            parse: undefined,
          },
        ],
      ],
    });
    const parcels = mappings.find((m) => m.name === "parcels")!;
    expect(parcels.rows).toEqual({
      kind: "nested",
      parent: "shipments",
      records: "/packages",
    });
    expect(mappingSource(mappings, parcels)).toBe("src_f16a79166c0c7a62");
    const returned = mappings
      .find((m) => m.name === "returns")!
      .bindings.find((b) => b.alias === "ret")!;
    expect(returned.props.find(([p]) => p === "returned_at")![1]).toEqual({
      field: "returned_at",
      transform: [],
      parse: "datetime %Y-%m-%d %H:%M UTC",
    });
    expect(
      mappings
        .find((m) => m.name === "order_lines")!
        .bindings.find((b) => b.alias === "product")!.optional,
    ).toBe(true);
  });
  it("keeps the agent's questions, summary, review and usage", () => {
    expect(bundle.status).toBe("done");
    expect(bundle.questions.map((q) => q.id)).toEqual(["Q1", "Q2", "Q3"]);
    expect(bundle.questions[0]).toMatchObject({
      items: ["type.Product", "mapping.products"],
      blocking: false,
    });
    expect(bundle.questions[1]!.options).toEqual([]);
    expect(bundle.summary).toMatch(/^This is a first draft/);
    expect(bundle.competency.map((c) => c.id)).toEqual([
      "CQ1",
      "CQ2",
      "CQ3",
      "CQ4",
      "CQ5",
      "CQ6",
      "CQ7",
    ]);
    expect(bundle.competency[0]).toEqual({
      id: "CQ1",
      question: "Who placed each order?",
      path: ["Order", "placed_by>", "Customer"],
    });
    expect(bundle.gaps).toEqual([
      {
        id: "G1",
        description:
          "A return is accepted only if it comes back within 30 days of the order being placed.",
      },
    ]);
    expect(bundle.leftOut).toHaveLength(12);
    expect(bundle.items["type.Order"]!.rationale).toMatch(/Every CQ/);
    expect(bundle.usage.usd).toBeCloseTo(1.153, 3);
  });
  it("draws types fed by the mappings that create them", () => {
    const graph = agentGraph(bundle);
    expect(graph.nodes).toHaveLength(8);
    expect(graph.edges).toHaveLength(8);
    // Orders are created by the orders mapping only; the others report them.
    expect(graph.nodes.find((n) => n.name === "Order")!.sources).toEqual([
      { kind: "agent", sourceId: "src_4cfe9b3e7852efa5", object: undefined },
    ]);
    expect(graph.nodes.find((n) => n.name === "Parcel")!.sources).toEqual([
      { kind: "agent", sourceId: "src_f16a79166c0c7a62", object: "/packages" },
    ]);
    expect(byDegree(graph)[0]!.name).toBe("Order");
  });
  it("reports a file it cannot parse and keeps the rest", () => {
    const broken = readAgentBundle({
      format: "agent_bundle",
      ontology_toml: "[types.Order\n",
      mappings_toml: 'version = "draft"\nontology = "draft"\n',
      questions: [{ id: "Q1", text: "Why?" }, { id: "Q2" }],
      status: "approved",
    });
    expect(broken.ontology).toMatchObject({ ok: false });
    expect(broken.ontology.ok || broken.ontology.error).toBeTruthy();
    expect(broken.mappings).toEqual({ ok: true, value: [] });
    expect(broken.questions.map((q) => q.id)).toEqual(["Q1"]);
    expect(broken.status).toBeUndefined();
    expect(agentGraph(broken)).toEqual({ nodes: [], edges: [] });
  });
  it("falls back to the draft view's files and questions", () => {
    const view = readAgentBundle({
      format: "agent_bundle",
      draft: {
        ontology_toml: '[types.A]\ndescription = "a"\n',
        mappings_toml: "",
        draft: { questions: [{ id: "Q9", text: "Which?", blocking: true }] },
      },
    });
    expect(view.ontology.ok && view.ontology.value.types[0]!.key).toEqual([]);
    expect(view.questions[0]).toMatchObject({ id: "Q9", blocking: true });
  });
});

describe("ontology.toml declarations", () => {
  it("reads link traits and relations", () => {
    const parsed = parseAgentOntology(`
version = "draft"
[links.within]
from = "Place"
to = "Place"
description = "Inside."
transitive = true
implies = ["located_in"]
[links.employs]
from = "Company"
to = "Person"
description = "Employs."
inverse_of = "works_at"
symmetric = true
chain = [["based_in", "within"]]
[relations.located_in]
description = "Located."
transitive = true
[constraints.c]
description = "x"
severity = "block"
datalog = "violation(x) :- a(x)."
`);
    expect(parsed.ok).toBe(true);
    if (!parsed.ok) return;
    expect(parsed.value.links.map((l) => [l.name, l.traits])).toEqual([
      [
        "employs",
        ["inverse of works_at", "symmetric", "chain based_in → within"],
      ],
      ["within", ["implies located_in", "transitive"]],
    ]);
    expect(parsed.value.relations[0]!.traits).toEqual(["transitive"]);
    expect(parsed.value.constraints[0]!.severity).toBe("block");
  });
  it("reads name-identified bindings and nested parses", () => {
    const parsed = parseAgentMappings(`
version = "draft"
ontology = "draft"
[mappings.places]
source = "src_0000000000000000"
table = "public.places"
[mappings.places.bind.place]
type = "Place"
name = { field = "city", transform = ["trim"] }
missing = "create"
props = { on = { field = "on", parse = { date = "%d/%m/%Y" } }, ok = { field = "ok", parse = { boolean = { true = ["y"] } } } }
`);
    expect(parsed.ok).toBe(true);
    if (!parsed.ok) return;
    const [m] = parsed.value;
    expect(m!.rows).toEqual({
      kind: "source",
      source: "src_0000000000000000",
      object: "public.places",
    });
    expect(m!.bindings[0]!.identify).toEqual({
      kind: "name",
      field: { field: "city", transform: ["trim"], parse: undefined },
    });
    expect(m!.bindings[0]!.props.map(([, f]) => f.parse)).toEqual([
      "boolean",
      "date %d/%m/%Y",
    ]);
  });
  it("stops at a nested mapping whose parents loop", () => {
    const parsed = parseAgentMappings(`
version = "draft"
ontology = "draft"
[mappings.a]
parent = "b"
records = "/x"
[mappings.b]
parent = "a"
records = "/y"
`);
    expect(parsed.ok).toBe(true);
    if (!parsed.ok) return;
    expect(mappingSource(parsed.value, parsed.value[0]!)).toBeUndefined();
  });
});

describe("document routes", () => {
  it("reads each [extractions.*] with its sets, types and links", () => {
    const r = parseAgentRoutes(`
[extractions.notes]
documents = "all"
[extractions.contracts]
documents = ["set_b", "set_a", "set_b"]
links = ["signed_with", "party_to", "signed_with"]
[extractions.contracts.types.Customer]
[extractions.contracts.types.Contract]
create = true
props = ["term_months", "amount", "amount"]
`);
    expect(r).toEqual({
      ok: true,
      value: [
        {
          name: "contracts",
          // Deduplicated, in the order written.
          documents: { kind: "sets", ids: ["set_b", "set_a"] },
          types: [
            {
              name: "Contract",
              create: true,
              props: ["amount", "term_months"],
            },
            // create = false and no props unless said: find and link only.
            { name: "Customer", create: false, props: [] },
          ],
          links: ["party_to", "signed_with"],
        },
        { name: "notes", documents: { kind: "all" }, types: [], links: [] },
      ],
    });
  });

  it("reads no set from a selector it doesn't understand, and none from a draft without routes", () => {
    for (const documents of ['"some"', "3", "[]", "[1, 2]"]) {
      const r = parseAgentRoutes(`[extractions.x]\ndocuments = ${documents}`);
      expect(r.ok && r.value[0]!.documents, documents).toEqual({
        kind: "sets",
        ids: [],
      });
    }
    expect(parseAgentRoutes('[extractions.x]\nlinks = ["l"]')).toMatchObject({
      ok: true,
      value: [{ documents: { kind: "sets", ids: [] } }],
    });
    expect(parseAgentRoutes(String(agentBundle.mappings_toml))).toEqual({
      ok: true,
      value: [],
    });
    expect(parseAgentRoutes("[extractions.x")).toMatchObject({ ok: false });
  });

  it("says which routes read a set", () => {
    const routes = [
      {
        name: "all",
        documents: { kind: "all" as const },
        types: [],
        links: [],
      },
      {
        name: "some",
        documents: { kind: "sets" as const, ids: ["set_1"] },
        types: [],
        links: [],
      },
    ];
    expect(routesReading(routes, "set_1").map((r) => r.name)).toEqual([
      "all",
      "some",
    ]);
    expect(routesReading(routes, "set_2").map((r) => r.name)).toEqual(["all"]);
    expect(routesReading(routes.slice(1), "set_2")).toEqual([]);
  });

  it("starts Generate on the current version's sets, unless a link names others", () => {
    const sets = [{ id: "g" }, { id: "b" }, { id: "n" }];
    // A first generation reads none.
    expect(startingSets(null, undefined, sets)).toEqual([]);
    expect(startingSets(null, [], sets)).toEqual([]);
    // The current version's, in the sets' order, without the deleted ones.
    expect(startingSets(null, ["b", "gone", "g"], sets)).toEqual(["g", "b"]);
    // A link's prefill wins, "" for none.
    expect(startingSets("n", ["g"], sets)).toEqual(["n"]);
    expect(startingSets("", ["g"], sets)).toEqual([]);
    expect(startingSets("n,,gone", ["g"], sets)).toEqual(["n"]);
  });

  it("names a version's scope, saying a set that is gone", () => {
    const sets = [
      { id: "g", name: "General" },
      { id: "b", name: "Board" },
    ];
    expect(scopeNames(["b", "g"], sets)).toEqual(["General", "Board"]);
    expect(scopeNames([], sets)).toEqual([]);
    expect(scopeNames(["g", "x"], sets)).toEqual(["General", "a deleted set"]);
    expect(scopeNames(["x", "y", "y"], sets)).toEqual(["2 deleted sets"]);
  });

  it("says what a load reads of its documents, and when reading again costs nothing", () => {
    const plan = {
      routed: 2,
      stored: 0,
      reading: 0,
      unreadable: 0,
      unread: 0,
    };
    const LATER = " Documents added later are read as they arrive.";
    const FREE =
      "From General: 2 documents, read for the names and facts they hold; reading the same text with the same models again costs nothing." +
      LATER;
    const READ =
      "From General: 2 documents, read for the names and facts they hold." +
      LATER;
    // Read before with the same models, by a current load that enriched no
    // one: replayed.
    expect(loadDocumentsText(plan, "General", false, {})).toBe(FREE);
    expect(
      loadDocumentsText(plan, "General", false, { enrichment: null }),
    ).toBe(FREE);
    expect(
      loadDocumentsText({ ...plan, unread: 1 }, "General", false, {}),
    ).toBe(READ);
    // No current load: nothing was read before.
    expect(
      loadDocumentsText({ ...plan, unread: 2 }, "General", false, null),
    ).toBe(READ);
    // The current load not known yet: no word on what reading again costs.
    expect(loadDocumentsText(plan, "General", false, undefined)).toBe(READ);
    // Enriching people, the documents that mention them are paid for again
    // whenever the profiles applied change, read before or not.
    const AGAIN =
      "From General: 2 documents, read for the names and facts they hold; those that mention people are read again, and paid for, whenever the applied profiles change." +
      LATER;
    expect(loadDocumentsText(plan, "General", true, {})).toBe(AGAIN);
    expect(
      loadDocumentsText({ ...plan, unread: 2 }, "General", true, null),
    ).toBe(AGAIN);
    expect(loadDocumentsText(plan, "General", true, undefined)).toBe(AGAIN);
    // A version that enriches no one, loaded after a load that applied
    // profiles (People enrichment turned off): the people it described with
    // them are described without, so it isn't free.
    const enriched = { enrichment: { status: "done" as const, applied: "3" } };
    expect(loadDocumentsText(plan, "General", false, enriched)).toBe(AGAIN);
    expect(
      loadDocumentsText(plan, "General", false, {
        enrichment: { status: "off" as const },
      }),
    ).toBe(AGAIN);
    // What it stores, not reads, and the PDFs it reads once their text is,
    // or can't read.
    expect(
      loadDocumentsText(
        { routed: 0, stored: 1, reading: 2, unreadable: 2, unread: 1 },
        "General and Board",
        true,
        {},
      ),
    ).toBe(
      "From General and Board: none read. 1 document in sets no route reads is stored, not read. 2 PDFs are still being read: the load reads them once their text is. 2 PDFs have no text Foundry could read, such as a scanned one." +
        LATER,
    );
    expect(
      loadDocumentsText(
        { routed: 1, stored: 2, reading: 1, unreadable: 1, unread: 1 },
        "General",
        false,
        {},
      ),
    ).toBe(
      "From General: 1 document, read for the names and facts they hold. 2 documents in sets no route reads are stored, not read. 1 PDF is still being read: the load reads it once its text is. 1 PDF has no text Foundry could read, such as a scanned one." +
        LATER,
    );
  });

  it("counts what a load of a version reads of the documents", () => {
    const routes = [
      {
        name: "contracts",
        documents: { kind: "sets" as const, ids: ["g"] },
        types: [],
        links: [],
      },
    ];
    const docs = [
      { id: "d1", setId: "g", status: "uploaded" as const },
      { id: "d2", setId: "b", status: "uploaded" as const },
      { id: "d3", setId: "g", status: "pending_upload" as const },
      { id: "d4", setId: "n", status: "uploaded" as const },
    ];
    // Only its scope's uploaded documents: routed ones read, others stored.
    expect(loadDocumentPlan(docs, ["g", "b"], routes, new Set(["d1"]))).toEqual(
      {
        total: 2,
        routed: 1,
        stored: 1,
        reading: 0,
        unreadable: 0,
        unread: 1,
        missing: [],
      },
    );
    expect(loadDocumentPlan(docs, [], routes, new Set())).toEqual({
      total: 0,
      routed: 0,
      stored: 0,
      reading: 0,
      unreadable: 0,
      unread: 0,
      missing: [],
    });
    // A PDF is read as text once its text is read, waits while it is
    // being read, and is neither read nor waiting when it couldn't be (a
    // scanned one); a table file joins no load (its mappings read it); a
    // type the page doesn't know may be read.
    const pdf = (id: string, state?: FoundryFileReadState) => ({
      id,
      setId: "g",
      status: "uploaded" as const,
      mediaType: "application/pdf",
      read: state && { state },
    });
    expect(
      loadDocumentPlan(
        [
          {
            id: "m",
            setId: "g",
            status: "uploaded",
            mediaType: "text/markdown",
          },
          { id: "c", setId: "g", status: "uploaded", mediaType: "text/csv" },
          pdf("p1", "FOUNDRY_FILE_READ_STATE_READY"),
          pdf("p2", "FOUNDRY_FILE_READ_STATE_READING"),
          pdf("p3", "FOUNDRY_FILE_READ_STATE_FAILED"),
          // No read the WFE said.
          pdf("p4"),
          { ...pdf("p5", "FOUNDRY_FILE_READ_STATE_READY"), setId: "b" },
          { id: "x", setId: "b", status: "uploaded", mediaType: "image/png" },
        ],
        ["g", "b"],
        routes,
        new Set(["c", "p1"]),
      ),
    ).toEqual({
      total: 7,
      routed: 2,
      stored: 2,
      reading: 1,
      unreadable: 2,
      unread: 3,
      missing: [],
    });
    expect(loadDocumentPlan(docs, ["g"], routes, new Set(["d1"])).unread).toBe(
      0,
    );
  });

  it("counts a connection's sets in scope by set, all text", () => {
    const routes = [
      {
        name: "notes",
        documents: { kind: "sets" as const, ids: ["n"] },
        types: [],
        links: [],
      },
    ];
    const uploads = [{ id: "d1", setId: "g", status: "uploaded" as const }],
      owned = [
        // Routed, 400 of 418 read by the current load.
        { id: "n", documents: 418, read: 400 },
        // No route reads it: stored. More read than it holds now (some were
        // replaced since): none unread.
        { id: "t", documents: 10, read: 12 },
        // Out of scope.
        { id: "x", documents: 99, read: 0 },
      ];
    expect(
      loadDocumentPlan(
        uploads,
        ["g", "n", "t"],
        routes,
        new Set(["d1"]),
        [],
        [],
        owned,
      ),
    ).toEqual({
      total: 429,
      routed: 418,
      stored: 11,
      reading: 0,
      unreadable: 0,
      unread: 18,
      missing: [],
    });
    // None in scope: the uploads alone.
    expect(
      loadDocumentPlan(uploads, ["g"], routes, new Set(), [], [], owned),
    ).toMatchObject({ total: 1, routed: 0, stored: 1, unread: 1 });
  });

  it("names a connection's set in a scope with its connection", () => {
    expect(
      scopeNames(
        ["n", "g"],
        [
          { id: "g", name: "General" },
          {
            id: "n",
            name: "Notes",
            connector: { connectorId: "cg", connectorName: "Granola" },
          },
        ],
      ),
    ).toEqual(["General", "Granola / Notes"]);
  });

  it("groups a connection's sets under it for Generate's picker", () => {
    const notes = (id: string, connectorId: string, connectorName = "") => ({
      id,
      name: id,
      connector: { connectorId, connectorName },
    });
    const sets = [
      notes("n1", "cg", "Granola (team)"),
      { id: "g", name: "General" },
      notes("n2", "cx"),
      notes("t1", "cg", "Granola (team)"),
      { id: "b", name: "Board" },
    ];
    const connectors = [
      { id: "cg", kind: "granola", name: "Team notes" },
      { id: "cx", kind: "granola" },
    ];
    const label = (kind: string) => (kind === "granola" ? "Granola" : kind);
    const picker = pickerSets(sets, connectors, label);
    // People's sets first, in the WFE's order.
    expect(picker.made.map((s) => s.id)).toEqual(["g", "b"]);
    // Each connection once, in the order of its first set, named as its
    // page names it: its name, else its kind's label.
    expect(
      picker.owned.map((o) => [o.name, o.kind, o.sets.map((s) => s.id)]),
    ).toEqual([
      ["Team notes", "granola", ["n1", "t1"]],
      ["Granola", "granola", ["n2"]],
    ]);
    expect(picker.ordered.map((s) => s.id)).toEqual([
      "g",
      "b",
      "n1",
      "t1",
      "n2",
    ]);
    // Before the connectors are read: the sets' word for it, else none.
    expect(pickerSets(sets, []).owned.map((o) => [o.name, o.kind])).toEqual([
      ["Granola (team)", ""],
      ["A connection", ""],
    ]);
    // Only people's sets: no groups.
    expect(pickerSets(sets.slice(1, 2), connectors).owned).toEqual([]);
    expect(
      pickerSets(
        [
          {
            id: "n",
            name: "Notes",
            connector: { connectorId: "c", connectorName: "Team" },
          },
          { id: "g", name: "General" },
        ],
        [],
      ).ordered.map((s) => s.id),
    ).toEqual(["g", "n"]);
  });

  it("keeps the document scope helpers' examples", () => {
    expect(
      startingSets(null, ["s1", "gone"], [{ id: "s1" }, { id: "s2" }]),
    ).toEqual(["s1"]);
    expect(startingSets("s2", ["s1"], [{ id: "s1" }, { id: "s2" }])).toEqual([
      "s2",
    ]);
    expect(
      startingSets("s1,s2", undefined, [{ id: "s1" }, { id: "s2" }], 1),
    ).toEqual(["s1"]);
    // A generation reads at most 20 sets: a longer scope starts with its
    // first 20.
    const many = Array.from({ length: 25 }, (_, i) => ({ id: `s${i}` }));
    expect(
      startingSets(
        null,
        many.map((x) => x.id),
        many,
      ),
    ).toHaveLength(MAX_GENERATION_SETS);
    expect(
      scopeNames(
        ["s2", "gone"],
        [
          { id: "s1", name: "General" },
          { id: "s2", name: "Board" },
        ],
      ),
    ).toEqual(["Board", "a deleted set"]);
    expect(
      loadDocumentPlan(
        [
          { id: "d1", setId: "g", status: "uploaded" },
          { id: "d2", setId: "b", status: "uploaded" },
        ],
        ["g"],
        [],
        new Set(),
      ),
    ).toEqual({
      total: 1,
      routed: 0,
      stored: 1,
      reading: 0,
      unreadable: 0,
      unread: 1,
      missing: [],
    });
  });

  it("names the mapped table files a load can't read any more", () => {
    const ready = { state: "FOUNDRY_FILE_READ_STATE_READY" as const };
    const files = [
      // Read, in scope: fine.
      {
        id: "f1",
        name: "a.csv",
        setId: "g",
        status: "uploaded" as const,
        mediaType: "text/csv",
        read: ready,
      },
      // Moved out of the version's sets.
      {
        id: "f2",
        name: "b.csv",
        setId: "x",
        status: "uploaded" as const,
        mediaType: "text/csv",
        read: ready,
      },
      // Still being read.
      {
        id: "f3",
        name: "c.csv",
        setId: "g",
        status: "uploaded" as const,
        mediaType: "text/csv",
        read: { state: "FOUNDRY_FILE_READ_STATE_READING" as const },
      },
      // Recorded by the generation, but not mapped.
      {
        id: "f5",
        name: "e.csv",
        setId: "x",
        status: "uploaded" as const,
        mediaType: "text/csv",
        read: ready,
      },
    ];
    const plan = loadDocumentPlan(
      files,
      ["g"],
      [],
      new Set(),
      ["f1", "f2", "f3", "f4"],
      [
        { id: "f1" },
        { id: "f2" },
        { id: "f3" },
        { id: "f4" },
        { id: "f5" },
        {},
      ],
    );
    expect(plan.missing).toEqual([
      { id: "f2", name: "b.csv" },
      { id: "f3", name: "c.csv" },
      // Deleted: no name left.
      { id: "f4", name: undefined },
    ]);
    // Table files are never counted as documents a load reads.
    expect(plan.total).toBe(0);
  });

  it("tells how a version reads a table file", () => {
    const bundle = draftFromTables(
      [
        fileTable(
          { id: "g", name: "General" },
          { id: "doc_1", name: "orders.csv", rows: [{ id: 1, total: 2.5 }] },
        ),
      ],
      "",
    );
    const mappings = readAgentBundle(bundle).mappings;
    expect(mappings.ok ? loadSources(mappings.value) : []).toContain("doc_1");
    const version = {
      id: "v",
      generationId: "gen",
      documentSetIds: ["g"],
      definition: bundle,
    };
    expect(fileMapping("doc_1", "g", version)).toEqual({ kind: "mapped" });
    // Mapped by id even out of its sets: the load fails, but it is mapped.
    expect(fileMapping("doc_1", "x", version)).toEqual({ kind: "mapped" });
    expect(fileMapping("doc_2", "g", version)).toEqual({
      kind: "unmapped",
      revise: false,
    });
    expect(
      fileMapping("doc_2", "g", version, {
        id: "gen",
        documents: [{ id: "doc_2", setId: "g" }],
      }),
    ).toEqual({ kind: "unmapped", revise: true });
    expect(fileMapping("doc_2", "x", version)).toEqual({ kind: "none" });
    // Definition v1 maps no file, and a revision can't map one.
    expect(
      fileMapping(
        "doc_2",
        "g",
        { id: "v1", documentSetIds: ["g"], definition: foundryDefinition },
        { id: "gen", documents: [{ id: "doc_2" }] },
      ),
    ).toEqual({ kind: "unmapped", revise: false });
    expect(
      fileMapping("doc_1", "g", { id: "v", documentSetIds: ["g"] }),
    ).toEqual({ kind: "unmapped", revise: false });
    expect(
      fileMapping("doc_1", "b", { id: "v", documentSetIds: ["g"] }),
    ).toEqual({ kind: "none" });
  });

  it("reads typed identifiers on a type", () => {
    const b = readAgentBundle(contractsBundle);
    const customer = b.ontology.ok
      ? b.ontology.value.types.find((t) => t.name === "Customer")
      : undefined;
    expect(customer?.identifiers).toEqual([
      { prop: "customer_id", kind: "code" },
      { prop: "email", kind: "email" },
    ]);
    expect(b.routes).toEqual({
      ok: true,
      value: [
        {
          name: "contracts",
          documents: { kind: "sets", ids: ["set_acme_general"] },
          types: [
            {
              name: "Contract",
              create: true,
              props: ["signed_on", "term_months"],
            },
            { name: "Customer", create: false, props: ["currency"] },
          ],
          links: ["signed_with"],
        },
      ],
    });
  });
});

describe("source matrix", () => {
  it("puts each source table, then each route, against each type and link", () => {
    const b = readAgentBundle(contractsBundle);
    if (!b.ontology.ok || !b.mappings.ok || !b.routes.ok)
      throw new Error("the fixture parses");
    const m = sourceMatrix(b.ontology.value, b.mappings.value, b.routes.value);
    expect(m.columns).toEqual([
      {
        kind: "table",
        sourceId: "src_d15a4b332975dac0",
        mappings: ["customers"],
      },
      {
        kind: "table",
        sourceId: "src_07e1ecea14a36bd9",
        mappings: ["invoices"],
      },
      {
        kind: "route",
        route: "contracts",
        documents: { kind: "sets", ids: ["set_acme_general"] },
      },
    ]);
    const row = (name: string) => m.types.find((t) => t.name === name)!;
    // Only documents create a Contract, with the props the route lets them fill.
    expect(row("Contract")).toEqual({
      name: "Contract",
      cells: [
        null,
        null,
        { role: "creates", props: ["signed_on", "term_months"] },
      ],
      created: true,
    });
    // Customers: the customers table creates them (key and props), invoices
    // look them up, documents find them and state their currency.
    expect(row("Customer").cells).toEqual([
      {
        role: "creates",
        props: ["company_name", "currency", "customer_id", "email"],
      },
      { role: "looks_up", props: ["customer_id"] },
      { role: "resolves", props: ["currency"] },
    ]);
    expect(row("Invoice").cells).toEqual([
      null,
      { role: "creates", props: ["amount_due", "invoice_id", "status"] },
      null,
    ]);
    expect(m.links).toEqual([
      { name: "billed_to", cells: [false, true, false] },
      { name: "signed_with", cells: [false, false, true] },
    ]);
  });

  it("flags a type nothing creates, and groups nested mappings under their root's table", () => {
    const o = parseAgentOntology(`
[types.Order]
props = { number = "string" }
key = ["number"]
[types.Line]
props = { sku = "string" }
key = ["sku"]
[types.Note]
`);
    const maps = parseAgentMappings(`
[mappings.orders]
source = "src_1"
[mappings.orders.bind.order]
type = "Order"
key = { number = "number" }
[mappings.lines]
parent = "orders"
records = "lines"
[mappings.lines.bind.line]
type = "Line"
key = { sku = "sku" }
missing = "report"
[mappings.notes]
parent = "gone"
records = "notes"
[mappings.notes.bind.note]
type = "Note"
name = "title"
props = { body = "text" }
`);
    if (!o.ok || !maps.ok) throw new Error("parses");
    const m = sourceMatrix(o.value, maps.value, []);
    // A mapping whose rows lead to no source gets a column of its own.
    expect(m.columns).toEqual([
      { kind: "table", sourceId: "src_1", mappings: ["lines", "orders"] },
      { kind: "table", sourceId: "", mappings: ["notes"] },
    ]);
    expect(
      m.types.map((t) => [t.name, t.created, t.cells.map((c) => c?.role)]),
    ).toEqual([
      ["Line", false, ["looks_up", undefined]],
      ["Note", true, [undefined, "creates"]],
      ["Order", true, ["creates", undefined]],
    ]);
    // A binding by name fills its props, and no key.
    expect(m.types[1]!.cells[1]).toEqual({ role: "creates", props: ["body"] });
  });
});

describe("sources", () => {
  it("names a synced table as the backend's agent generator does", async () => {
    expect(await agentSourceId("conn_acme_shop", "orders")).toBe(
      "src_4cfe9b3e7852efa5",
    );
    expect(await agentSourceId("conn_acme_shop", "orders")).toBe(
      sourceId("conn_acme_shop", "orders"),
    );
  });
  it("splits a left-out field from its source", () => {
    expect(splitSourceText("src_66578f9aca73ac6c file.email")).toEqual({
      sourceId: "src_66578f9aca73ac6c",
      rest: "email",
    });
    expect(splitSourceText("src_66578f9aca73ac6c")).toEqual({
      sourceId: "src_66578f9aca73ac6c",
      rest: "",
    });
    expect(splitSourceText("clicks_jsonl")).toEqual({
      sourceId: undefined,
      rest: "clicks_jsonl",
    });
  });
  it("describes a field as read", () => {
    expect(fieldText({ field: "sku", transform: [] })).toBe("sku");
    expect(
      fieldText({ field: "/at", transform: ["trim"], parse: "date %Y" }),
    ).toBe("/at → trim → parse date %Y");
  });
});

describe("the mock agent's drafts", () => {
  it("names clashing tables apart", () => {
    const orders = (id: string, name: string) => ({
      connector: { id, name, kind: "postgres", tables: [] },
      table: {
        name: "orders",
        columns: [{ name: "id", type: "bigint" }],
        primaryKey: ["id"],
      },
    });
    const bundle = readAgentBundle(
      draftFromTables(
        [orders("a", "Shop"), orders("b", "Shop"), orders("c", "Shop")],
        "",
      ),
    );
    expect(agentGraph(bundle).nodes.map((n) => n.name)).toEqual([
      "Order",
      "ShopOrder",
      "ShopOrder2",
    ]);
  });

  it("parse as the viewer reads them", () => {
    const connector = {
      id: "con_x",
      name: "Billing",
      kind: "postgres",
      tables: [],
    };
    const def = draftFromTables(
      [
        {
          connector,
          table: {
            name: "public.customers",
            columns: [
              { name: "id", type: "bigint" },
              { name: "email", type: "text" },
              { name: "meta", type: "jsonb" },
            ],
            primaryKey: ["id"],
          },
        },
        {
          connector,
          table: {
            name: "public.invoices",
            columns: [
              { name: "id", type: "bigint" },
              { name: "customer_id", type: "bigint" },
              { name: "plan_id", type: "bigint", nullable: true },
              { name: "total", type: "numeric" },
              { name: "issued_on", type: "date" },
            ],
            primaryKey: ["id"],
            foreignKeys: [
              {
                column: "customer_id",
                referencedTable: "public.customers",
                referencedColumn: "id",
              },
              {
                column: "plan_id",
                referencedTable: "public.plans",
                referencedColumn: "id",
              },
            ],
          },
        },
        {
          connector,
          table: {
            name: "audit_log",
            columns: [{ name: "line", type: "text" }],
          },
        },
      ],
      "Billing health",
    );
    const bundle = readAgentBundle(def);
    expect(bundle.ontology.ok && bundle.mappings.ok).toBe(true);
    const graph = agentGraph(bundle);
    expect(graph.nodes.map((n) => [n.name, n.key])).toEqual([
      ["AuditLog", []],
      ["Customer", ["id"]],
      ["Invoice", ["id"]],
    ]);
    expect(graph.edges).toEqual([
      expect.objectContaining({
        name: "customer",
        from: "Invoice",
        to: "Customer",
        cardinality: "one",
        required: true,
      }),
    ]);
    expect(
      graph.nodes.find((n) => n.name === "Invoice")!.props.map((p) => p.type),
    ).toEqual(["integer", "float", "date"]);
    expect(bundle.status).toBe("needs_input");
    expect(bundle.questions.map((q) => q.text)).toEqual([
      expect.stringContaining("public.plans"),
      "Which field identifies a row of audit_log?",
    ]);
    expect(bundle.leftOut).toEqual([
      {
        what: `${sourceId("con_x", "public.customers")} file.meta`,
        reason: "Nested JSON that no question reads yet.",
      },
    ]);
    expect(bundle.summary).toContain("“Billing health”");
  });
});

describe("titleFromGoal", () => {
  const length = (s: string) => [...s].length;

  it("is empty for an empty goal", () => {
    expect(titleFromGoal("")).toBe("");
    expect(titleFromGoal("  \n\t\n ")).toBe("");
    expect(titleFromGoal("-\n*\n1.")).toBe("");
  });

  it("keeps a short goal whole, without its closing full stop", () => {
    expect(titleFromGoal("Customer billing health")).toBe(
      "Customer billing health",
    );
    expect(titleFromGoal("  Customer billing health...  ")).toBe(
      "Customer billing health",
    );
    expect(titleFromGoal("Which orders ship late, and why?")).toBe(
      "Which orders ship late, and why?",
    );
  });

  it("reads the first sentence", () => {
    expect(
      titleFromGoal("Which orders ship late, and why? Group them by carrier."),
    ).toBe("Which orders ship late, and why?");
    expect(titleFromGoal("Find churn risk! Then upsell.")).toBe(
      "Find churn risk!",
    );
    expect(titleFromGoal("Billing health. Contracts too.")).toBe(
      "Billing health",
    );
  });

  it("doesn't end a sentence at an abbreviation, a number or a dot in a word", () => {
    expect(titleFromGoal("Trace orders, e.g. by carrier. Then returns.")).toBe(
      "Trace orders, e.g. by carrier",
    );
    expect(titleFromGoal("Sales in the U.S. by region")).toBe(
      "Sales in the U.S. by region",
    );
    expect(titleFromGoal("Revenue for 2025. By month")).toBe(
      "Revenue for 2025. By month",
    );
    expect(titleFromGoal("Usage of app.example.com pages")).toBe(
      "Usage of app.example.com pages",
    );
    expect(titleFromGoal("Compare Stripe vs. Shopify revenue by month")).toBe(
      "Compare Stripe vs. Shopify revenue by month",
    );
    expect(titleFromGoal("Contracts with Acme Inc. and Globex")).toBe(
      "Contracts with Acme Inc. and Globex",
    );
    expect(titleFromGoal("Contracts with Acme Co. Then invoices.")).toBe(
      "Contracts with Acme Co. Then invoices",
    );
    expect(titleFromGoal("Orders, returns, refunds etc. for the shop")).toBe(
      "Orders, returns, refunds etc. for the shop",
    );
    expect(titleFromGoal("Mr. Jones's orders")).toBe("Mr. Jones's orders");
    expect(titleFromGoal("Dr. Smith's patients. Then billing.")).toBe(
      "Dr. Smith's patients",
    );
    // An abbreviation is a whole word: "first" and "items" end a sentence.
    expect(titleFromGoal("Orders first. Then items. Then refunds.")).toBe(
      "Orders first",
    );
  });

  it("ends a sentence at . only before a capital, but at ! or ? before anything", () => {
    expect(titleFromGoal("Find late orders. group them by carrier")).toBe(
      "Find late orders. group them by carrier",
    );
    expect(titleFromGoal("Which orders ship late? group by carrier")).toBe(
      "Which orders ship late?",
    );
    expect(titleFromGoal("Find churn! then upsell")).toBe("Find churn!");
    expect(titleFromGoal('Find late orders. "Late" means a day.')).toBe(
      "Find late orders",
    );
    expect(titleFromGoal("Find late orders. (By carrier.)")).toBe(
      "Find late orders",
    );
  });

  it("ends a sentence after a closing bracket or quote", () => {
    expect(titleFromGoal("Track orders (and returns). Then refunds.")).toBe(
      "Track orders (and returns)",
    );
    expect(titleFromGoal("Orders “late”? Then refunds.")).toBe(
      "Orders “late”?",
    );
    expect(titleFromGoal('Find "late". Then refunds.')).toBe('Find "late"');
    expect(titleFromGoal("Orders [and returns]! Then refunds.")).toBe(
      "Orders [and returns]!",
    );
  });

  it("reads the first line, without a list or heading marker", () => {
    expect(titleFromGoal("Contracts we signed\nand what they bill")).toBe(
      "Contracts we signed",
    );
    expect(titleFromGoal("\n\n- Contracts we signed\n- What they bill")).toBe(
      "Contracts we signed",
    );
    expect(titleFromGoal("1. Orders\n2. Returns")).toBe("Orders");
    expect(titleFromGoal("2) Returns")).toBe("Returns");
    expect(titleFromGoal("## Billing\r\nInvoices and charges")).toBe("Billing");
    expect(titleFromGoal("> Quoted goal")).toBe("Quoted goal");
    expect(titleFromGoal("-\n  * Orders")).toBe("Orders");
    // A marker needs a space after it: these are words.
    expect(titleFromGoal("#1 priority: churn")).toBe("#1 priority: churn");
    expect(titleFromGoal("-5% churn")).toBe("-5% churn");
  });

  it("reads past lines that lead into the next with a colon", () => {
    expect(titleFromGoal("Goal:\nTrace orders")).toBe("Trace orders");
    expect(titleFromGoal("Goal:\r\n\r\nTrace orders. By carrier.")).toBe(
      "Trace orders",
    );
    expect(
      titleFromGoal(
        "We need to answer these questions:\n- where is my order?\n- why is it late?",
      ),
    ).toBe("where is my order?");
    expect(titleFromGoal("## Goal:\nContext:\n1. Trace orders")).toBe(
      "Trace orders",
    );
    // A colon inside the line is the title's own.
    expect(titleFromGoal("Contracts: who signed\nand what they bill")).toBe(
      "Contracts: who signed",
    );
    // With nothing after it, the lead-in is all there is.
    expect(titleFromGoal("Trace orders:")).toBe("Trace orders");
    expect(titleFromGoal("Goal:\nContext:")).toBe("Goal");
  });

  it("drops a closing colon, semicolon or comma as it does a full stop", () => {
    expect(titleFromGoal("Orders and returns;")).toBe("Orders and returns");
    expect(titleFromGoal("Orders and returns,\nrefunds")).toBe(
      "Orders and returns",
    );
    expect(titleFromGoal("Orders and returns.:")).toBe("Orders and returns");
    expect(titleFromGoal("Orders and returns?")).toBe("Orders and returns?");
  });

  it("collapses runs of white space", () => {
    expect(titleFromGoal("Orders   and\treturns")).toBe("Orders and returns");
  });

  it("keeps a sentence of exactly TITLE_MAX characters whole", () => {
    const goal = "Orders and returns " + "x".repeat(TITLE_MAX - 19);
    expect(length(goal)).toBe(TITLE_MAX);
    expect(titleFromGoal(goal)).toBe(goal);
  });

  it("cuts a longer one at a word boundary, with an ellipsis", () => {
    expect(
      titleFromGoal(
        "Contracts with our customers: who we signed with and what they are billed.",
      ),
    ).toBe("Contracts with our customers: who we signed with and what…");
    expect(
      titleFromGoal(
        "Help customer service trace an order from the customer through its shipments and returns.",
      ),
    ).toBe("Help customer service trace an order from the customer…");
  });

  it("drops the punctuation a cut leaves at the end", () => {
    expect(
      titleFromGoal(
        "Orders, shipments, returns, refunds, exchanges, carriers, warehouses, stock levels",
      ),
    ).toBe("Orders, shipments, returns, refunds, exchanges, carriers…");
    expect(
      titleFromGoal(
        "Orders and the shipments that carry them to customers — returns and refunds",
      ),
    ).toBe("Orders and the shipments that carry them to customers…");
  });

  it("keeps a whole word that ends right at the limit", () => {
    // The 60th character is a space: the 59 before it are whole words.
    const words = "Orders " + "x".repeat(TITLE_MAX - 8);
    expect(length(words)).toBe(TITLE_MAX - 1);
    expect(titleFromGoal(words + " and more")).toBe(words + "…");
    // The 61st is: the 60 before it don't leave room for the ellipsis.
    const longer = "Orders " + "x".repeat(TITLE_MAX - 7);
    expect(titleFromGoal(longer + " and more")).toBe("Orders…");
  });

  it("cuts inside one long word", () => {
    expect(titleFromGoal("x".repeat(80))).toBe("x".repeat(TITLE_MAX - 1) + "…");
  });

  it("counts characters, not UTF-16 units", () => {
    const title = titleFromGoal("𝒜".repeat(70));
    expect(title).toBe("𝒜".repeat(TITLE_MAX - 1) + "…");
    expect(title).not.toMatch(/\uFFFD/);
  });

  it("is never longer than TITLE_MAX characters", () => {
    const words = [
      "a",
      "orders",
      "shipment",
      "é",
      "𝒜𝒜",
      "returns,",
      "x".repeat(30),
    ];
    for (let n = 1; n < 200; n++) {
      const goal = Array.from(
        { length: n },
        (_, i) => words[(i * 7 + n) % words.length],
      ).join(" ");
      expect(length(titleFromGoal(goal))).toBeLessThanOrEqual(TITLE_MAX);
    }
  });
});

describe("generationTitle", () => {
  it("is the WFE's title, on one line", () => {
    expect(
      generationTitle({
        title: "Late shipments",
        goal: "Which orders ship late?",
      }),
    ).toBe("Late shipments");
    expect(generationTitle({ title: "  Late\n shipments " })).toBe(
      "Late shipments",
    );
  });

  it("cuts a WFE title over TITLE_MAX characters as it cuts a goal", () => {
    const exact = "Orders and returns " + "x".repeat(TITLE_MAX - 19);
    expect(generationTitle({ title: exact })).toBe(exact);
    expect(
      generationTitle({
        title:
          "Help customer service trace an order from the customer through its shipments and returns.",
        goal: "Trace orders",
      }),
    ).toBe("Help customer service trace an order from the customer…");
    expect(generationTitle({ title: "x".repeat(200) })).toBe(
      "x".repeat(TITLE_MAX - 1) + "…",
    );
    expect(generationTitle({ title: "𝒜".repeat(70) })).toBe(
      "𝒜".repeat(TITLE_MAX - 1) + "…",
    );
  });

  it("derives one from the goal when the WFE has none", () => {
    expect(
      generationTitle({ goal: "Which orders ship late? By carrier." }),
    ).toBe("Which orders ship late?");
    // The gateway sends an unset title as "".
    expect(generationTitle({ title: "", goal: "Billing health." })).toBe(
      "Billing health",
    );
    expect(generationTitle({ title: " \n ", goal: "Billing health" })).toBe(
      "Billing health",
    );
  });

  it("names a generation with neither by what it does", () => {
    expect(generationTitle({})).toBe("An ontology from your data");
    expect(generationTitle({ title: "", goal: "  " })).toBe(
      "An ontology from your data",
    );
    // Or leaves it unnamed, where a page names it only when it can.
    expect(generationTitle({ title: "", goal: "- " }, "")).toBe("");
    expect(generationTitle({ goal: "Billing health" }, "")).toBe(
      "Billing health",
    );
  });
});

describe("versionRows", () => {
  const day = (d: number) => `2026-10-${String(d).padStart(2, "0")}T00:00:00Z`;
  const ids = (rows: ReturnType<typeof versionRows>) =>
    rows.map((r) =>
      r.kind === "version" ? r.version.id : `failed ${r.generation.id}`,
    );

  it("is the doc example", () => {
    expect(
      versionRows(
        [{ id: "v1", createdAt: "2026-10-01T00:00:00Z", generationId: "g1" }],
        [
          {
            id: "g2",
            status: "ONTOLOGY_GENERATION_STATUS_FAILED",
            createdAt: "2026-10-02T00:00:00Z",
          },
          {
            id: "g1",
            status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
            resultVersionId: "v1",
          },
        ],
      ).map((r) => r.kind),
    ).toEqual(["failed", "version"]);
  });

  it("puts failed generations among the versions, newest first", () => {
    const rows = versionRows(
      [
        { id: "v3", createdAt: day(17), generationId: "g4" },
        { id: "v2", createdAt: day(16), generationId: "g3" },
        { id: "v1", createdAt: day(2), generationId: "g1" },
      ],
      [
        { id: "g4", status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" },
        { id: "g3", status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" },
        {
          id: "g2",
          status: "ONTOLOGY_GENERATION_STATUS_FAILED",
          createdAt: day(9),
        },
        { id: "g1", status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" },
      ],
    );
    expect(ids(rows)).toEqual(["v3", "v2", "failed g2", "v1"]);
  });

  it("gives each version the generation that drafted it", () => {
    const drafted = {
      id: "g1",
      status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
      title: "Billing",
    } as const;
    expect(
      versionRows(
        [
          { id: "v2", source: "ONTOLOGY_SOURCE_MANUAL" },
          { id: "v1", generationId: "g1" },
          // Its generation is older than the list.
          { id: "v0", generationId: "g0" },
        ],
        [drafted],
      ),
    ).toEqual([
      {
        kind: "version",
        version: { id: "v2", source: "ONTOLOGY_SOURCE_MANUAL" },
        generation: undefined,
      },
      {
        kind: "version",
        version: { id: "v1", generationId: "g1" },
        generation: drafted,
      },
      {
        kind: "version",
        version: { id: "v0", generationId: "g0" },
        generation: undefined,
      },
    ]);
  });

  it("lists a failed revision but no preview and nothing live", () => {
    const rows = versionRows(
      [],
      [
        {
          id: "queued",
          status: "ONTOLOGY_GENERATION_STATUS_QUEUED",
          createdAt: day(6),
        },
        {
          id: "running",
          status: "ONTOLOGY_GENERATION_STATUS_RUNNING",
          createdAt: day(5),
        },
        {
          id: "retrying",
          status: "ONTOLOGY_GENERATION_STATUS_QUEUED",
          attempts: 2,
          error: "timed out",
          createdAt: day(4),
        },
        {
          id: "preview",
          status: "ONTOLOGY_GENERATION_STATUS_FAILED",
          revision: { parentVersionId: "v1", dryRun: true },
          createdAt: day(3),
        },
        {
          id: "revision",
          status: "ONTOLOGY_GENERATION_STATUS_FAILED",
          revision: { parentVersionId: "v1", dryRun: false },
          createdAt: day(2),
        },
        {
          id: "unknown",
          status: "ONTOLOGY_GENERATION_STATUS_UNSPECIFIED",
          createdAt: day(1),
        },
        // A preview stores no version, failed or not.
        {
          id: "checked",
          status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
          revision: { parentVersionId: "v1", dryRun: true },
        },
      ],
    );
    expect(ids(rows)).toEqual(["failed revision"]);
  });

  it("sorts a row without a time last, keeping versions first on a tie", () => {
    const rows = versionRows(
      [
        { id: "undated" },
        { id: "v2", createdAt: day(2) },
        { id: "v1", createdAt: day(1) },
        { id: "bad", createdAt: "not a time" },
      ],
      [
        {
          id: "g2",
          status: "ONTOLOGY_GENERATION_STATUS_FAILED",
          createdAt: day(2),
        },
        { id: "gx", status: "ONTOLOGY_GENERATION_STATUS_FAILED" },
      ],
    );
    expect(ids(rows)).toEqual([
      "v2",
      "failed g2",
      "v1",
      "undated",
      "bad",
      "failed gx",
    ]);
  });

  it("is empty for an ontology with neither", () => {
    expect(versionRows([], [])).toEqual([]);
  });
});
