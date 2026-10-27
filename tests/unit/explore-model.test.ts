import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import {
  extractDocument,
  ingestChunk,
  normIdentifier,
} from "../../mock/graph/extract";
import { scopeOf } from "../../mock/graph/scope";
import {
  FOUNDRY_EDGE_LABELS,
  FOUNDRY_EDGES,
  FOUNDRY_NODES,
} from "@/shared/foundry-labels";
import { runQuery } from "../../mock/graph/query";
import { MockGraph } from "../../mock/graph/store";
import {
  LIMITS,
  documentFactsRequest,
  documentRequest,
  entityId,
  factGroups,
  mentionsByDocument,
  provenanceText,
  snippet,
  valueText,
  entityRequest,
  entityRow,
  factEvidenceRequest,
  parseDocument,
  parseDocumentFacts,
  parseEntity,
  parseEntityId,
  parseFactEvidence,
  isLayoutProp,
  propertyViews,
  resolvedByText,
  sameValue,
  type PropEvidence,
} from "@/web/areas/explore/model";
import type { Scope } from "@/web/areas/explore/load-scope";

// The reads over C3, run through the mock's v3 interpreter over a graph the
// mock's load wrote: the seeded contract names two Stripe customers (Globex
// Corporation, Initech LLC) and a company that is neither (Umbrella
// Holdings).

const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
  "utf8",
);
const ontology = {
  types: new Set(["Contract", "Customer", "Invoice"]),
  names: new Map([["Customer", "company_name"]]),
  identifiers: new Map([["Customer", { customer_id: "code" }]]),
};
// The contracts route: documents create Contracts with their signing date
// and term, find customers and state signed_with.
const SCOPE_OF_DOCUMENTS = scopeOf(
  {
    Contract: { create: true, props: ["signed_on", "term_months"] },
    Customer: {},
  },
  ["signed_with"],
);
const chunk = {
  source: "src_customers",
  mapping: "customers",
  snapshot: "2026-10-17T10:00:00.000Z",
  ontologyVersion: "foundry-v3",
};

// The load the tests' reads are scoped to.
const LOAD = "load_1";
// Reads within that load.
const SCOPE: Scope = { kind: "load", id: LOAD };

/**
 * What one load of the contracts draft writes into `db`: three customers,
 * an invoice of Globex (`invoice`) and the document `text`.
 */
function loaded(
  db = new MockGraph(),
  load = LOAD,
  invoice = "in_1001",
  text = CONTRACT,
) {
  const g = db.view(load);
  ingestChunk(
    g,
    chunk,
    [
      ["cus_GLOBEX01", "Globex Corporation"],
      ["cus_INITECH2", "Initech LLC"],
      ["cus_HOOLI003", "Hooli Inc"],
    ].map(([key, name]) => ({
      type: "Customer",
      key: key!,
      name: name!,
      props: { customer_id: key!, company_name: name! },
      names: ["company_name"],
      identifiers: { customer_id: "code" },
    })),
  );
  ingestChunk(g, { ...chunk, source: "src_invoices", mapping: "invoices" }, [
    {
      type: "Invoice",
      key: invoice,
      name: invoice,
      props: { invoice_id: invoice, amount_due: 1200000 },
      links: [{ link: "billed_to", toType: "Customer", toKey: "cus_GLOBEX01" }],
    },
  ]);
  const outcome = extractDocument(
    g,
    {
      id: "doc_1",
      name: "contract.md",
      text,
      markdown: true,
      ontologyVersion: "foundry-v3",
    },
    ontology,
    SCOPE_OF_DOCUMENTS,
  );
  expect(outcome.kind).toBe("ingested");
  return db;
}
/** A load of the contracts draft that reads `texts` in that order. */
function loadedWith(db: MockGraph, texts: string[]) {
  const g = db.view(LOAD);
  ingestChunk(
    g,
    { ...chunk, snapshot: "2026-10-18T10:00:00.000Z" },
    [["cus_GLOBEX01", "Globex Corporation"]].map(([key, name]) => ({
      type: "Customer",
      key: key!,
      name: name!,
      props: { customer_id: key!, company_name: name! },
      names: ["company_name"],
    })),
  );
  texts.forEach((text, i) =>
    extractDocument(
      g,
      {
        id: `doc_${i + 1}`,
        name: `d${i + 1}.md`,
        text,
        markdown: true,
        ontologyVersion: "foundry-v3",
      },
      ontology,
      SCOPE_OF_DOCUMENTS,
    ),
  );
  return db;
}
const read = (g: MockGraph, req: unknown) => {
  const answer = runQuery(g, req);
  expect(answer.status, JSON.stringify(answer.body)).toBe(200);
  return answer.body;
};
const utf8 = (s: string) => new TextEncoder().encode(s);
const slice = (text: string, start: number, end: number) =>
  new TextDecoder().decode(utf8(text).slice(start, end));

describe("entity ids", () => {
  it("round-trips keys and names, unicode included", () => {
    for (const i of [
      { type: "Customer", key: "cus_1" },
      { type: "Contract", name: "master services agreement" },
      { type: "Société", name: "café ☕ crème" },
    ]) {
      const id = entityId(i);
      expect(id).toMatch(/^[A-Za-z0-9_-]+$/);
      expect(parseEntityId(id)).toEqual(i);
    }
    expect(entityId({ type: "Customer", key: "cus_1" })).toBe(
      "Q3VzdG9tZXIAazpjdXNfMQ",
    );
  });

  it("refuses what isn't one", () => {
    for (const id of [
      "",
      "!!",
      btoa("no-nul"),
      btoa("\0k:x"),
      btoa("T\0x:y"),
      btoa("T\0k:"),
      "_w",
    ])
      expect(parseEntityId(id), id).toBeUndefined();
  });

  it("reads entity rows, never Foundry's own nodes", () => {
    expect(
      entityRow({ $id: 4, $label: "Customer", entity_key: "c", name: "C" }),
    ).toMatchObject({ dbId: 4, type: "Customer", name: "C" });
    expect(
      entityRow({ $id: 4, $label: "Contract", norm_name: "msa" }),
    ).toMatchObject({
      name: "msa",
      identity: { type: "Contract", name: "msa" },
    });
    for (const label of [FOUNDRY_NODES.Mention, "FoundryLater"])
      expect(entityRow({ $id: 4, $label: label, norm_name: "x" })).toBe(
        undefined,
      );
    // A type named as Foundry's labels were before their prefix is the
    // ontology's own.
    expect(
      entityRow({ $id: 4, $label: "Mention", norm_name: "x" }),
    ).toMatchObject({ type: "Mention", name: "x" });
    expect(entityRow({ $id: "4", $label: "Customer", entity_key: "c" })).toBe(
      undefined,
    );
    expect(entityRow({ $id: 4, $label: "Customer" })).toBe(undefined);
  });
});

describe("a document's view", () => {
  it("reads its sections, mentions, quotes and reviews", () => {
    const g = loaded();
    const doc = parseDocument(read(g, documentRequest(SCOPE, "doc_1")));
    expect(doc.stored).toBe(true);
    // A heading starts a section, and the paragraphs after it join it.
    expect(doc.sections.map((s) => s.ordinal)).toEqual([0, 1, 2, 3]);
    expect(doc.sections[0]!.text).toMatch(
      /^# Master services agreement\n\nThis/,
    );
    expect(doc.sections[0]!.model).toBe("claude-haiku-5-5");
    // Every span is UTF-8 bytes into its section, and holds what it says.
    for (const m of doc.mentions)
      expect(slice(doc.sections[m.section]!.text, m.start, m.end)).toBe(
        m.surface,
      );
    for (const q of [...doc.evidence, ...doc.reviews])
      expect(slice(doc.sections[q.section]!.text, q.start, q.end)).toBe(
        q.quote,
      );
    expect(doc.entities.map((e) => `${e.type}:${e.name}`)).toEqual([
      "Contract:Master services agreement",
      "Customer:Globex Corporation",
      "Customer:Initech LLC",
    ]);
    const globex = doc.mentions.filter((m) =>
      m.entity.name.startsWith("Globex"),
    );
    // Each mention says how it resolved: the short name by alias.
    expect(globex.map((m) => [m.surface, m.resolvedBy])).toEqual([
      ["Globex Corporation", "name"],
      ["Globex", "alias"],
    ]);
    // The document created the Contract its heading names.
    expect(
      doc.mentions.find((m) => m.entity.type === "Contract")?.resolvedBy,
    ).toBe("created");
    expect(doc.reviews).toHaveLength(1);
    expect(doc.reviews[0]).toMatchObject({
      reason: "unresolved",
      suggestedLink: "signed_with",
      candidates: [],
    });
    expect(doc.reviews[0]!.quote).toContain("Umbrella Holdings");
    // Text never creates a keyed entity.
    expect(doc.entities.some((e) => e.name.includes("Umbrella"))).toBe(false);
  });

  it("reads what settles each review, naming its candidates by the entities it concerns", () => {
    const db = new MockGraph(),
      g = db.view(LOAD);
    ingestChunk(
      g,
      chunk,
      [
        ["cus_GLOBEX01", "Globex Corporation", "usd"],
        ["cus_INITECH2", "Initech LLC", "eur"],
      ].map(([key, name, currency]) => ({
        type: "Customer",
        key: key!,
        name: name!,
        props: { customer_id: key!, company_name: name!, currency: currency! },
        names: ["company_name"],
      })),
    );
    extractDocument(
      g,
      {
        id: "doc_1",
        name: "billing.md",
        text: "# Billing\n\nGlobexx Corporation signed. Globex Corporation is billed in GBP.\n",
        markdown: true,
        ontologyVersion: "foundry-v3",
      },
      ontology,
      scopeOf(
        {
          Contract: { create: true },
          Customer: { props: ["currency"] },
        },
        ["signed_with"],
      ),
    );
    const doc = parseDocument(read(db, documentRequest(SCOPE, "doc_1")));
    const name = doc.reviews.find((r) => r.reason === "unresolved")!;
    // The candidate's stored identity, and the entity it is.
    expect(name.candidates).toEqual([
      {
        key: "cus_GLOBEX01",
        entity: expect.objectContaining({
          type: "Customer",
          name: "Globex Corporation",
          identity: { type: "Customer", key: "cus_GLOBEX01" },
        }),
      },
    ]);
    expect(name.settle).toEqual({
      kind: "resolve",
      reviewId: expect.any(String),
      type: "Customer",
      docId: "doc_1",
      normName: "globexx corporation",
    });
    // The table's value stays; the review carries it and the quote against it.
    const conflict = doc.reviews.find((r) => r.reason === "prop_conflict")!;
    expect(conflict.settle).toMatchObject({
      kind: "prop",
      type: "Customer",
      entityKey: "cus_GLOBEX01",
      prop: "currency",
      table: "usd",
      values: [
        expect.objectContaining({
          value: { text: "GBP" },
          documentId: "doc_1",
        }),
      ],
    });
    // The entity's page reads the same reviews the same way.
    const page = parseEntity(
      read(db, entityRequest(SCOPE, { type: "Customer", key: "cus_GLOBEX01" })),
    );
    expect(
      page.reviews.map((r) => [r.reason, r.settle?.kind, r.candidates.length]),
    ).toEqual(
      expect.arrayContaining([
        ["unresolved", "resolve", 1],
        ["prop_conflict", "prop", 0],
      ]),
    );
    expect(
      page.reviews.find((r) => r.reason === "prop_conflict")!.settle,
    ).toMatchObject({ table: "usd" });
  });

  it("joins its quotes to their facts by key", () => {
    const g = loaded();
    const doc = parseDocument(read(g, documentRequest(SCOPE, "doc_1")));
    const req = documentFactsRequest(SCOPE, doc.evidence)!;
    const facts = parseDocumentFacts(read(g, req), doc);
    expect(facts.map((f) => `${f.from?.name} ${f.link} ${f.to?.name}`)).toEqual(
      [
        "Master services agreement signed_with Globex Corporation",
        "Master services agreement signed_with Initech LLC",
      ],
    );
    expect(facts[0]).toMatchObject({
      provenance: "extracted",
      evidenceCount: 1,
      confidence: 0.93,
    });
  });

  it("is empty before a load stores it, and has no facts request", () => {
    const g = loaded();
    const doc = parseDocument(read(g, documentRequest(SCOPE, "doc_missing")));
    expect(doc).toEqual({
      stored: false,
      sections: [],
      mentions: [],
      evidence: [],
      reviews: [],
      entities: [],
    });
    expect(documentFactsRequest(SCOPE, doc.evidence)).toBeNull();
  });

  it("caps the facts it reads", () => {
    const evidence = Array.from(
      { length: LIMITS.documentFacts + 5 },
      (_, i) => ({
        section: 0,
        start: 0,
        end: 1,
        quote: "x",
        link: "signed_with",
        factKey: `k${i}`,
      }),
    );
    const req = documentFactsRequest(SCOPE, evidence)!;
    expect((req.query.read as { entries: unknown[] }).entries).toHaveLength(
      LIMITS.documentFacts,
    );
  });

  it("drops rows it can't place, and survives junk", () => {
    expect(parseDocument(null).sections).toEqual([]);
    expect(parseDocument("text").mentions).toEqual([]);
    const doc = parseDocument({
      document: [{ $id: 1 }],
      sections: [{ $id: 2, ordinal: 0, text: "Hi Globex" }, { $id: 3 }],
      mentions: [
        { $id: 10, start: 3, end: 9, surface: "Globex" },
        { $id: 11, start: 0, end: 2, surface: "Hi" },
      ],
      mention_in: [{ $from: 10, $to: 2 }],
      mention_of: [
        { $from: 10, $to: 20 },
        { $from: 11, $to: 21 },
      ],
      entities: [{ $id: 20, $label: "Customer", entity_key: "g", name: "G" }],
      evidence: [{ $id: 30, fact_key: "f", link: "l", start: 0, end: 2 }],
      reviews: "nope",
    });
    expect(doc.sections).toHaveLength(1);
    expect(doc.mentions.map((m) => m.surface)).toEqual(["Globex"]);
    expect(doc.evidence).toEqual([]);
    expect(doc.reviews).toEqual([]);
  });
});

describe("an entity's page", () => {
  it("reads a Stripe customer named in a contract as one entity", () => {
    const g = loaded();
    const page = parseEntity(
      read(g, entityRequest(SCOPE, { type: "Customer", key: "cus_GLOBEX01" })),
    );
    expect(page.entity).toMatchObject({
      type: "Customer",
      name: "Globex Corporation",
      props: {
        customer_id: "cus_GLOBEX01",
        company_name: "Globex Corporation",
      },
    });
    // The normalized name and identifier the text resolves by, and the part
    // of the load that made it, stay out of its properties.
    expect(Object.keys(page.entity!.props).sort()).toEqual([
      "company_name",
      "customer_id",
      "name",
    ]);
    expect(page.entity!.madeBy).toBe("rows");
    // What the load stored beside them: the identifier's normalized value
    // and the part of the load that made it.
    expect(
      [...g.nodes.values()].find((n) => n.props.entity_key === "cus_GLOBEX01")
        ?.props,
    ).toMatchObject({
      customer_id__id: "cusglobex01",
      foundry_part: `${LOAD}:rows`,
    });
    expect(
      page.facts.map((f) => `${f.direction} ${f.link} ${f.other?.name}`).sort(),
    ).toEqual([
      "in billed_to in_1001",
      "in signed_with Master services agreement",
    ]);
    expect(page.mentions.map((m) => m.surface)).toEqual([
      "Globex Corporation",
      "Globex",
    ]);
    for (const m of page.mentions)
      expect(slice(m.text, m.start, m.end)).toBe(m.surface);
    // The row says which props it set: they show it as their source.
    expect(page.rows).toEqual([
      expect.objectContaining({
        source: "src_customers",
        rowKey: "cus_GLOBEX01",
        props: ["company_name", "customer_id"],
      }),
    ]);
    expect(page.propEvidence).toEqual([]);
    expect(
      propertyViews(page.entity!.props, page.rows, page.propEvidence).map(
        (v) => [v.prop, v.rows.length, v.quotes.length],
      ),
    ).toEqual([
      ["company_name", 1, 0],
      ["customer_id", 1, 0],
    ]);
    expect(page.truncated).toEqual({
      facts: false,
      mentions: false,
      reviews: false,
      about: false,
    });
  });

  it("reads a fact's evidence: a row, or a quote with its section", () => {
    const g = loaded();
    const page = parseEntity(
      read(g, entityRequest(SCOPE, { type: "Customer", key: "cus_GLOBEX01" })),
    );
    const evidence = parseFactEvidence(
      read(g, factEvidenceRequest(SCOPE, page.facts)!),
    );
    const by = (link: string) =>
      evidence.get(page.facts.find((f) => f.link === link)!.factKey!)!;
    expect(by("billed_to")).toEqual([
      expect.objectContaining({
        kind: "row",
        source: "src_invoices",
        rowKey: "in_1001",
      }),
    ]);
    const [quote] = by("signed_with");
    expect(quote).toMatchObject({
      kind: "quote",
      documentId: "doc_1",
      section: 0,
    });
    expect(quote!.kind === "quote" && quote.quote).toContain(
      "Globex Corporation",
    );
  });

  it("reads a named entity with the reviews about it", () => {
    const g = loaded();
    const page = parseEntity(
      read(
        g,
        entityRequest(SCOPE, {
          type: "Contract",
          name: "master services agreement",
        }),
      ),
    );
    expect(page.entity?.name).toBe("Master services agreement");
    expect(page.facts.filter((f) => f.direction === "out")).toHaveLength(2);
    expect(page.reviews).toEqual([
      expect.objectContaining({ reason: "unresolved", documentId: "doc_1" }),
    ]);
  });

  it("reads the props a document filled with the quotes that state them", () => {
    const g = loaded();
    const page = parseEntity(
      read(
        g,
        entityRequest(SCOPE, {
          type: "Contract",
          name: "master services agreement",
        }),
      ),
    );
    expect(page.entity).toMatchObject({
      madeBy: "documents",
      props: { signed_on: "2026-10-01", term_months: 24 },
    });
    expect(page.rows).toEqual([]);
    const sections = parseDocument(
      read(g, documentRequest(SCOPE, "doc_1")),
    ).sections;
    expect(
      page.propEvidence.map((e) => [e.prop, e.value, e.documentId]),
    ).toEqual([
      ["signed_on", "2026-10-01", "doc_1"],
      ["term_months", 24, "doc_1"],
    ]);
    // Each quote is the span of its section the document link opens on.
    for (const e of page.propEvidence)
      expect(slice(sections[e.section]!.text, e.start, e.end)).toBe(e.quote);
    expect(page.propEvidence[1]!.quote).toBe(
      "The Agreement runs for 24 months and renews for 12 months unless a party gives 90 days’ notice.",
    );
    const views = propertyViews(
      page.entity!.props,
      page.rows,
      page.propEvidence,
    );
    expect(
      views.map((v) => [v.prop, v.value, v.quotes.length, v.others.length]),
    ).toEqual([
      ["signed_on", "2026-10-01", 1, 0],
      ["term_months", 24, 1, 0],
    ]);
  });

  it("leaves a value documents disagree on unset, whichever came first, and shows every value", () => {
    const AMENDED =
      "# Master services agreement\n\nWith Globex Corporation. The Agreement runs for 36 months.\n";
    for (const order of [["contract"], ["amended"]]) {
      const g = loadedWith(
        new MockGraph(),
        order[0] === "amended" ? [AMENDED, CONTRACT] : [CONTRACT, AMENDED],
      );
      const page = parseEntity(
        read(
          g,
          entityRequest(SCOPE, {
            type: "Contract",
            name: "master services agreement",
          }),
        ),
      );
      expect(page.entity!.props.term_months, order[0]).toBeUndefined();
      expect(page.entity!.props.signed_on).toBe("2026-10-01");
      const v = propertyViews(
        page.entity!.props,
        page.rows,
        page.propEvidence,
      ).find((x) => x.prop === "term_months")!;
      expect(v.value).toBeUndefined();
      expect(v.others.map((o) => [o.value, o.quotes.length])).toEqual(
        order[0] === "amended"
          ? [
              [36, 1],
              [24, 1],
            ]
          : [
              [24, 1],
              [36, 1],
            ],
      );
      expect(page.reviews.filter((r) => r.reason === "prop_conflict")).toEqual([
        expect.objectContaining({ prop: "term_months" }),
      ]);
    }
  });

  it("reads the ontology's own Document type and ABOUT link beside Foundry's", () => {
    // Before the prefix, these labels were Foundry's: a database holding the
    // team's documents mixed them with a load's.
    const g = loaded();
    ingestChunk(
      g.view(LOAD),
      { ...chunk, source: "src_docs", mapping: "docs" },
      [
        {
          type: "Document",
          key: "memo_1",
          name: "Pricing memo",
          props: { document_id: "memo_1", title: "Pricing memo" },
          links: [{ link: "ABOUT", toType: "Customer", toKey: "cus_GLOBEX01" }],
        },
      ],
    );
    const memo = parseEntity(
      read(g, entityRequest(SCOPE, { type: "Document", key: "memo_1" })),
    );
    expect(memo.entity).toMatchObject({
      type: "Document",
      name: "Pricing memo",
    });
    expect(memo.facts.map((f) => `${f.direction} ${f.link}`)).toEqual([
      "out ABOUT",
    ]);
    expect(memo.rows).toEqual([
      expect.objectContaining({ source: "src_docs", rowKey: "memo_1" }),
    ]);
    const globex = parseEntity(
      read(g, entityRequest(SCOPE, { type: "Customer", key: "cus_GLOBEX01" })),
    );
    expect(
      globex.facts.map((f) => `${f.direction} ${f.link} ${f.other?.name}`),
    ).toContain("in ABOUT Pricing memo");
    // The document view reads only what the load wrote for the file.
    const doc = parseDocument(read(g, documentRequest(SCOPE, "doc_1")));
    expect(doc.stored).toBe(true);
    expect(doc.entities.map((e) => e.type)).not.toContain("Document");
    expect(
      parseDocument(read(g, documentRequest(SCOPE, "memo_1"))).stored,
    ).toBe(false);
  });

  it("leaves Foundry's edges out of an entity's incoming facts in the read itself", () => {
    const req = entityRequest(SCOPE, { type: "Customer", key: "c" });
    expect(req.parameters?.foundry_edges).toEqual([...FOUNDRY_EDGE_LABELS]);
    expect(req.parameter_types?.foundry_edges).toEqual({ array: "string" });
  });

  it("is empty for an entity the database doesn't hold", () => {
    const g = loaded();
    const page = parseEntity(
      read(g, entityRequest(SCOPE, { type: "Customer", key: "cus_NOPE" })),
    );
    expect(page.entity).toBeNull();
    expect(page.facts).toEqual([]);
    expect(factEvidenceRequest(SCOPE, page.facts)).toBeNull();
  });

  it("reads each stored rule break with the entities it names, in order", () => {
    const end = (id: number, key: string) => ({
      $id: id,
      $label: "Person",
      entity_key: key,
      name: key,
    });
    const page = parseEntity({
      violations: [
        { $id: 10, constraint: "signer.cardinality", severity: "block" },
        { $id: 11, constraint: "signer.cardinality" },
        // A record without its constraint isn't one.
        { $id: 12 },
      ],
      // Out of position order, an edge to an end the read doesn't hold,
      // one without its position, and a record that names nothing.
      violation_concerns: [
        { $from: 10, $to: 3, position: 2 },
        { $from: 10, $to: 1, position: 0 },
        { $from: 10, $to: 2, position: 1 },
        { $from: 11, $to: 99, position: 0 },
        { $from: 11, $to: 2 },
      ],
      violation_ends: [
        { $id: 1, $label: "Contract", norm_name: "msa", name: "MSA" },
        end(2, "p1"),
        end(3, "p2"),
      ],
    });
    expect(
      page.violations.map((v) => [
        v.constraint,
        v.severity,
        v.entities.map((e) => e.name),
      ]),
    ).toEqual([
      ["signer.cardinality", "block", ["MSA", "p1", "p2"]],
      ["signer.cardinality", undefined, []],
    ]);
    expect(page.violations[0]!.entities[1]!.identity).toEqual({
      type: "Person",
      key: "p1",
    });
  });

  it("says when it holds more than it reads", () => {
    const facts = Array.from({ length: LIMITS.factsPerSide + 1 }, (_, i) => ({
      $id: i,
      $label: "billed_to",
      $from: 1,
      $to: 2,
    }));
    const page = parseEntity({
      entity: [{ $id: 2, $label: "Customer", entity_key: "c" }],
      in_facts: facts,
      mentions: Array.from({ length: LIMITS.mentions + 1 }, () => ({})),
      about: Array.from({ length: LIMITS.about + 1 }, (_, i) => ({
        $id: i,
        source: "src_x",
      })),
    });
    expect(page.facts).toHaveLength(LIMITS.factsPerSide);
    expect(page.rows).toHaveLength(LIMITS.about);
    expect(page.truncated).toEqual({
      facts: true,
      mentions: true,
      reviews: false,
      about: true,
    });
    // Foundry's edges are never facts, whatever the agent adds later; an
    // ontology link named as one was before its prefix is.
    expect(
      parseEntity({
        out_facts: [
          { $id: 1, $label: FOUNDRY_EDGES.MENTION_OF },
          { $id: 2, $label: "FOUNDRY_LATER" },
          { $id: 3, $label: "MENTION_OF" },
        ],
      }).facts.map((f) => f.link),
    ).toEqual(["MENTION_OF"]);
  });

  it("keeps Foundry's stamps out of an entity's and its facts' props", () => {
    const stamps = {
      foundry_load: "L1",
      foundry_tx: "01JTX",
      foundry_later: "x",
    };
    const fact = (id: number, from: number, to: number) => ({
      $id: id,
      $label: "billed_to",
      $from: from,
      $to: to,
      fact_key: `f${id}`,
      provenance: "rows",
      confidence: 1,
      evidence_count: 1,
      ontology_version: "foundry-v3",
      amount_due: 1200,
      ...stamps,
    });
    const page = parseEntity({
      entity: [
        {
          $id: 2,
          $label: "Customer",
          entity_key: "c",
          customer_id: "c",
          foundry_part: "L1:rows",
          ...stamps,
        },
      ],
      out_facts: [fact(10, 2, 3)],
      in_facts: [fact(11, 4, 2)],
    });
    expect(page.entity!.props).toEqual({ customer_id: "c" });
    // The stamps still say what made it.
    expect(page.entity!.madeBy).toBe("rows");
    expect(page.facts.map((f) => [f.direction, f.props])).toEqual([
      ["out", { amount_due: 1200 }],
      ["in", { amount_due: 1200 }],
    ]);
    expect(page.facts[0]).toMatchObject({
      factKey: "f10",
      provenance: "rows",
      confidence: 1,
      evidenceCount: 1,
    });
    // A prop that only contains the prefix is the data's.
    expect(
      parseEntity({
        entity: [
          { $id: 2, $label: "Customer", entity_key: "c", my_foundry_tx: "y" },
        ],
        out_facts: [{ ...fact(10, 2, 3), Foundry_tx: "z" }],
      }),
    ).toMatchObject({
      entity: { props: { my_foundry_tx: "y" } },
      facts: [{ props: { amount_due: 1200, Foundry_tx: "z" } }],
    });
  });

  it("keeps only property quotes it can open, in document order", () => {
    const quote = (id: number, start: number, extra = {}) => ({
      $id: id,
      prop: "amount",
      value: 10,
      doc_id: "doc_1",
      start,
      end: start + 4,
      quote: "ten.",
      ...extra,
    });
    const page = parseEntity({
      entity: [{ $id: 9, $label: "Contract", norm_name: "msa" }],
      about: [
        quote(1, 40),
        quote(2, 8),
        // In a section no ordinal is read for, or in none: not openable.
        quote(3, 0),
        quote(4, 0),
        // No document, or no span.
        quote(5, 0, { doc_id: null }),
        quote(6, 0, { end: "x" }),
        // A row, with props that aren't all strings.
        { $id: 7, source: "src_x", props: ["amount", 3] },
      ],
      about_in: [1, 2, 3, 5, 6].map((id) => ({
        $from: id,
        $to: id === 3 ? 102 : 101,
      })),
      about_sections: [{ $id: 101, ordinal: 0 }, { $id: 102 }],
    });
    expect(page.propEvidence.map((e) => [e.start, e.section])).toEqual([
      [8, 0],
      [40, 0],
    ]);
    expect(page.rows).toEqual([
      expect.objectContaining({ source: "src_x", props: ["amount"] }),
    ]);
  });
});

describe("two loads and the team's own data side by side", () => {
  // Two loads wrote the same customer, each with its own invoice and its
  // own reading of the document; the team keeps a Customer of its own.
  const db = loaded();
  loaded(db, "load_2", "in_2001", "# Renewal\n\nGlobex Corporation renewed.");
  db.addNode("Customer", {
    entity_key: "cus_GLOBEX01",
    name: "Globex (the team's own)",
  });
  const L1: Scope = { kind: "load", id: LOAD },
    L2: Scope = { kind: "load", id: "load_2" },
    OTHER: Scope = {
      kind: "other",
      loads: [LOAD, "load_2"],
      shared: {
        nodes: new Set(["Contract", "Customer", "Invoice"]),
        edges: new Set(["billed_to", "signed_with"]),
      },
    };
  const globex = { type: "Customer", key: "cus_GLOBEX01" };

  it("reads an entity's page from its load only", () => {
    const facts = (scope: Scope) =>
      parseEntity(read(db, entityRequest(scope, globex)))
        .facts.map((f) => `${f.direction} ${f.link} ${f.other?.name}`)
        .sort();
    expect(facts(L1)).toEqual([
      "in billed_to in_1001",
      "in signed_with Master services agreement",
    ]);
    expect(facts(L2)).toEqual([
      "in billed_to in_2001",
      "in signed_with Renewal",
    ]);
    const own = parseEntity(read(db, entityRequest(OTHER, globex)));
    expect(own.entity?.name).toBe("Globex (the team's own)");
    expect(own.facts).toEqual([]);
    // A fact's evidence is its load's.
    const page = parseEntity(read(db, entityRequest(L2, globex)));
    const evidence = parseFactEvidence(
      read(db, factEvidenceRequest(L2, page.facts)!),
    );
    expect(
      [...evidence.values()]
        .flat()
        .map((e) => (e.kind === "row" ? e.rowKey : e.quote)),
    ).toEqual(["in_2001", "Globex Corporation renewed."]);
  });

  it("reads a document's view from its load only", () => {
    const first = (scope: Scope) =>
      parseDocument(read(db, documentRequest(scope, "doc_1")));
    expect(first(L1).sections[0]!.text).toMatch(/^# Master services/);
    expect(first(L2).sections.map((s) => s.text)).toEqual([
      "# Renewal\n\nGlobex Corporation renewed.",
    ]);
    expect(first(L2).entities.map((e) => e.name)).toEqual([
      "Renewal",
      "Globex Corporation",
    ]);
    // Documents are always a load's: none in the team's own data.
    expect(first(OTHER).stored).toBe(false);
    // A quote's facts are its load's: the first load's, read in the
    // second, aren't there.
    const facts = (scope: Scope, evidence: Scope) => {
      const doc = first(evidence);
      // Found facts come with their ends.
      return parseDocumentFacts(
        read(db, documentFactsRequest(scope, doc.evidence)!),
        doc,
      ).filter((f) => f.from && f.to).length;
    };
    expect(facts(L1, L1)).toBeGreaterThan(0);
    expect(facts(L2, L2)).toBeGreaterThan(0);
    expect(facts(L2, L1)).toBe(0);
  });
});

describe("an entity's page, laid out", () => {
  it("groups facts by link and direction, outgoing first", () => {
    const fact = (link: string, direction: "out" | "in") => ({
      link,
      direction,
      props: {},
    });
    expect(
      factGroups([
        fact("signed_with", "in"),
        fact("billed_to", "in"),
        fact("signed_with", "out"),
        fact("signed_with", "in"),
      ]).map((g) => [g.link, g.direction, g.facts.length]),
    ).toEqual([
      ["billed_to", "in", 1],
      ["signed_with", "out", 1],
      ["signed_with", "in", 2],
    ]);
    expect(provenanceText("extracted")).toBe("From a document");
    expect(provenanceText("stated")).toBe("From a table");
    expect(provenanceText(undefined)).toBe("From a table");
  });

  it("groups mentions by document, in text order", () => {
    const m = (documentId: string, section: number, start: number) => ({
      documentId,
      section,
      start,
      end: start + 1,
      surface: "x",
      text: "x",
    });
    expect(
      mentionsByDocument([
        m("b", 1, 0),
        m("a", 2, 5),
        m("a", 0, 9),
        m("a", 2, 1),
      ]).map((d) => [
        d.documentId,
        d.mentions.map((x) => `${x.section}:${x.start}`),
      ]),
    ).toEqual([
      ["b", ["1:0"]],
      ["a", ["0:9", "2:1", "2:5"]],
    ]);
  });

  it("tells each property's source: rows, agreeing quotes, and values that differ", () => {
    const q = (
      prop: string,
      value: unknown,
      documentId = "d1",
    ): PropEvidence => ({
      prop,
      value,
      documentId,
      section: 0,
      start: 0,
      end: 1,
      quote: `${prop} is ${value}`,
    });
    const views = propertyViews(
      {
        name: "Globex",
        email: "ap@globex.example",
        status: "Signed",
        plan: "pro",
      },
      [
        { source: "src_c", props: ["email"] },
        // A row from before rows listed their props sets what no quote does.
        { source: "src_old" },
      ],
      [
        q("email", "billing@globex.example"),
        q("status", "signed "),
        q("industry", "Retail", "d1"),
        q("industry", "retail", "d2"),
        q("tier", "gold", "d1"),
        q("tier", "silver", "d2"),
      ],
    );
    expect(
      views.map((v) => [
        v.prop,
        v.value,
        v.rows.map((r) => r.source),
        v.quotes.length,
        v.others.map((o) => [o.value, o.quotes.length]),
      ]),
    ).toEqual([
      // A row's value kept over a document that says otherwise.
      [
        "email",
        "ap@globex.example",
        ["src_c", "src_old"],
        0,
        [["billing@globex.example", 1]],
      ],
      // Only quotes state it, and they agree once case aside: no stored
      // value yet (a later load writes it), nothing differs.
      ["industry", undefined, [], 0, [["Retail", 2]]],
      ["plan", "pro", ["src_old"], 0, []],
      // A quote that agrees, spacing and case aside.
      ["status", "Signed", [], 1, []],
      // Documents that disagree: no value, every value with its quote.
      [
        "tier",
        undefined,
        [],
        0,
        [
          ["gold", 1],
          ["silver", 1],
        ],
      ],
    ]);
    expect(propertyViews({ name: "x", note: "" }, [], [])).toEqual([]);
  });

  it("compares values as the load does, and keeps the layout's props out", () => {
    expect(sameValue(" A  b ", "a b")).toBe(true);
    expect(sameValue("ﬁle", "file")).toBe(true);
    expect(sameValue(24, 24)).toBe(true);
    expect(sameValue(24, 36)).toBe(false);
    expect(sameValue(0.1 + 0.2, 0.3)).toBe(true);
    expect(sameValue(1, 1.0000001)).toBe(false);
    expect(sameValue("24", 24)).toBe(false);
    expect(sameValue(null, undefined)).toBe(false);
    for (const k of [
      "email__id",
      "name__nn",
      "entity_key",
      "norm_name",
      "foundry_part",
      "foundry_load",
      "foundry_tx",
      // Any stamp Foundry adds later.
      "foundry_later",
      "$id",
    ])
      expect(isLayoutProp(k), k).toBe(true);
    // The prefix is case-sensitive and needs its underscore.
    for (const k of ["email", "id", "part", "load", "tx", "Foundry_tx"])
      expect(isLayoutProp(k), k).toBe(false);
    for (const k of ["foundry", "foundrytx", "my_foundry_tx"])
      expect(isLayoutProp(k), k).toBe(false);
  });

  it("normalizes identifiers by kind, as the mock's load stores them", () => {
    expect(normIdentifier("email", "John.Smith@Acme.com")).toBe(
      "john.smith@acme.com",
    );
    expect(normIdentifier("email", "johnsmith@acme.com")).not.toBe(
      normIdentifier("email", "John.Smith@Acme.com"),
    );
    for (const bad of ["a@b", "a@@b.com", "ab.com"])
      expect(normIdentifier("email", bad), bad).toBeUndefined();
    expect(normIdentifier("code", "INV-00123")).toBe("inv00123");
    expect(normIdentifier("code", "AB")).toBeUndefined();
    expect(normIdentifier("code", "ABCDE")).toBeUndefined();
    expect(normIdentifier("number", "1,234_5")).toBe("12345");
    expect(normIdentifier("number", 100)).toBeUndefined();
    expect(normIdentifier("exact", "  ")).toBeUndefined();
    expect(normIdentifier("exact", " X-1 ")).toBe("X-1");
    expect(normIdentifier("later", "x")).toBeUndefined();
  });

  it("finds an entity by an identifier the text quotes", () => {
    const db = loadedWith(new MockGraph(), [
      "# Order form\n\nThe customer cus_GLOBEX01 orders 3 seats.\n",
    ]);
    const doc = parseDocument(read(db, documentRequest(SCOPE, "doc_1")));
    expect(doc.mentions.map((m) => [m.surface, m.resolvedBy])).toEqual([
      ["Order form", "created"],
      ["cus_GLOBEX01", "identifier"],
    ]);
  });

  it("says how a mention found its entity", () => {
    expect(resolvedByText("identifier")).toBe("By an identifier");
    expect(resolvedByText("alias")).toBe("By a short name");
    expect(resolvedByText("created")).toBe("Created from the text");
    expect(resolvedByText("user")).toBe("Linked by a person");
    expect(resolvedByText("adjudicated")).toBe("Matched by the model");
    for (const r of ["name", "key", undefined, "later"])
      expect(resolvedByText(r)).toBeUndefined();
  });

  it("reads values and snippets", () => {
    expect(valueText(1200000)).toBe("1,200,000");
    expect(valueText(true)).toBe("Yes");
    expect(valueText(false)).toBe("No");
    expect(valueText("x")).toBe("x");
    expect(valueText(["a", 1])).toBe('["a",1]');
    for (const empty of [null, undefined, ""])
      expect(valueText(empty)).toBe("");
    // UTF-8 byte offsets: € is 3 bytes.
    expect(snippet("Paid €5 to Globex. Next.", 13, 19)).toEqual({
      before: "Paid €5 to ",
      match: "Globex",
      after: ".",
    });
    expect(snippet("One. Two Globex three\nNext", 9, 15)).toEqual({
      before: "Two ",
      match: "Globex",
      after: " three",
    });
    expect(snippet("Globex", 0, 6)).toEqual({
      before: "",
      match: "Globex",
      after: "",
    });
  });
});
