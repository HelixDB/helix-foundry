import { readFileSync } from "node:fs";
import { beforeEach, describe, expect, it } from "vitest";
import contractsBundle from "../../mock/fixtures/agent-bundle-contracts.json";
import { extractDocument, ingestChunk } from "../../mock/graph/extract";
import { scopeOf } from "../../mock/graph/scope";
import { runQuery } from "../../mock/graph/query";
import { MockGraph } from "../../mock/graph/store";
import { documentRequest, entityRequest } from "@/web/areas/explore/model";
import type { QueryExecutor } from "@/web/areas/explore/database-query";
import type { Scope } from "@/web/areas/explore/load-scope";
import { readQueries } from "@/web/areas/explore/reads";
import { NO_NAMING, namingKeys, namingOf } from "@/web/areas/explore/naming";
import { refKey } from "@/web/areas/explore/rule-breaks";

// The document and entity reads name their entities by the current version:
// they project the props names are made of, and an entity of a type the
// version knows takes its readable name, while any other keeps what the
// database stored.

let graph: MockGraph;
// The load the tests' reads are scoped to.
const LOAD = "load_1";
const SCOPE: Scope = { kind: "load", id: LOAD };
// The database's shared reads, over the mock graph.
const read: QueryExecutor = async (request) => {
  const answer = runQuery(graph, request);
  return answer.status === 200
    ? {
        ok: true,
        status: 200,
        contentType: null,
        body: answer.body,
        bytes: 0,
        elapsedMs: 0,
      }
    : {
        ok: false,
        kind: "database",
        status: answer.status,
        code: null,
        message: "no",
        elapsedMs: 0,
      };
};

const naming = namingOf({
  id: "ver_3",
  format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
  definition: contractsBundle,
});
const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
  "utf8",
);
const chunk = {
  source: "src_customers",
  mapping: "customers",
  snapshot: "2026-10-17T10:00:00.000Z",
  ontologyVersion: "foundry-v3",
};

beforeEach(() => {
  graph = new MockGraph();
  // The stored display name is from an older sync; company_name is current.
  ingestChunk(graph.view(LOAD), chunk, [
    {
      type: "Customer",
      key: "cus_GLOBEX01",
      name: "GLOBEX CORP (old)",
      props: {
        customer_id: "cus_GLOBEX01",
        company_name: "Globex Corporation",
      },
      names: ["company_name"],
    },
  ]);
  ingestChunk(
    graph.view(LOAD),
    { ...chunk, source: "src_invoices", mapping: "inv" },
    [
      {
        type: "Invoice",
        key: "in_1001",
        name: "Invoice 1001",
        props: { invoice_id: "in_1001", amount_due: 1200000 },
        links: [
          { link: "billed_to", toType: "Customer", toKey: "cus_GLOBEX01" },
        ],
      },
    ],
  );
  // A label the version doesn't know, linked to the customer.
  const partner = graph.view(LOAD).addNode("Partner", {
    entity_key: "p1",
    name: "Partner One",
  });
  graph
    .view(LOAD)
    .addEdge(
      "refers",
      partner,
      graph.find("Customer", "entity_key", "cus_GLOBEX01")!.id,
    );
  extractDocument(
    graph.view(LOAD),
    {
      id: "doc_1",
      name: "contract.md",
      text: CONTRACT,
      markdown: true,
      ontologyVersion: "foundry-v3",
    },
    {
      types: new Set(["Contract", "Customer", "Invoice"]),
      names: new Map([["Customer", "company_name"]]),
    },
    // What the contracts route lets the document fill: Contracts, found
    // customers and signed_with.
    scopeOf({ Contract: { create: true }, Customer: {} }, ["signed_with"]),
  );
});

describe("the reads' names", () => {
  it("project the props names are made of", () => {
    const keys = namingKeys(naming);
    const doc = JSON.stringify(documentRequest(SCOPE, "doc_1", keys));
    const entity = JSON.stringify(
      entityRequest(SCOPE, { type: "Customer", key: "c" }, keys),
    );
    for (const k of keys) {
      expect(doc).toContain(`"${k}"`);
      expect(entity).toContain(`"${k}"`);
    }
    expect(JSON.stringify(documentRequest(SCOPE, "doc_1"))).not.toContain(
      "company_name",
    );
  });

  it("name an entity's page and its facts' ends by the version", async () => {
    const q = readQueries.entity(
      "ws",
      "o",
      read,
      SCOPE,
      { type: "Customer", key: "cus_GLOBEX01" },
      naming,
    );
    expect(q.queryKey).toContain("ver_3");
    const page = await q.queryFn({});
    expect(page.entity?.name).toBe("Globex Corporation");
    const ends = Object.fromEntries(
      page.facts.map((f) => [f.link, f.other?.name]),
    );
    expect(ends).toEqual({
      billed_to: "in_1001",
      signed_with: "Master services agreement",
      // Not the version's: it keeps its stored name.
      refers: "Partner One",
    });
  });

  it("keep the stored names without a version", async () => {
    const page = await readQueries
      .entity("ws", "o", read, SCOPE, { type: "Customer", key: "cus_GLOBEX01" })
      .queryFn({});
    expect(page.entity?.name).toBe("GLOBEX CORP (old)");
    expect(page.facts.find((f) => f.link === "billed_to")?.other?.name).toBe(
      "Invoice 1001",
    );
  });

  it("name a document's entities and mentions", async () => {
    const q = readQueries.document("ws", "o", read, SCOPE, "doc_1", naming);
    const doc = await q.queryFn({});
    expect(doc.entities.map((e) => e.name)).toContain("Globex Corporation");
    expect(doc.entities.map((e) => e.name)).not.toContain("GLOBEX CORP (old)");
    expect(
      doc.mentions.find((m) => m.entity.type === "Customer")?.entity.name,
    ).toBe("Globex Corporation");
    const facts = readQueries.documentFacts(
      "ws",
      "o",
      read,
      SCOPE,
      "doc_1",
      doc,
      naming,
    );
    expect(facts.queryKey).toContain("ver_3");
    const list = await facts.queryFn({});
    expect(list[0]?.to?.name).toBe("Globex Corporation");
    const plain = await readQueries
      .document("ws", "o", read, SCOPE, "doc_1", NO_NAMING)
      .queryFn({});
    expect(plain.entities.map((e) => e.name)).toContain("GLOBEX CORP (old)");
  });
});

describe("the names of entities a page knows by type and id", () => {
  const globex = { type: "Customer", id: "cus_GLOBEX01" },
    msa = { type: "Contract", id: "master services agreement" },
    invoice = { type: "Invoice", id: "in_1001" },
    partner = { type: "Partner", id: "p1" },
    missing = { type: "Customer", id: "cus_NOPE" };

  it("names them as their pages do, in one lookup of the load", async () => {
    const q = readQueries.names(
      "ws",
      "o",
      read,
      LOAD,
      [globex, msa, invoice, partner, missing, globex],
      naming,
    );
    expect(q.enabled).toBe(true);
    // Each entity once, in order, whatever order the page found them in.
    expect(q.queryKey).toEqual([
      "ws",
      "ontology",
      "o",
      "read",
      LOAD,
      "names",
      [msa, globex, missing, invoice, partner].map(refKey),
      "ver_3",
    ]);
    const names = await q.queryFn({});
    // A names prop over the stored name, a named type's stored name, a
    // keyed type's key props; a type the version doesn't know, and an
    // entity the load doesn't hold, aren't named.
    expect(Object.fromEntries(names)).toEqual({
      [refKey(globex)]: "Globex Corporation",
      [refKey(msa)]: "Master services agreement",
      [refKey(invoice)]: "in_1001",
    });
    // The name the entity's page shows.
    const page = await readQueries
      .entity(
        "ws",
        "o",
        read,
        SCOPE,
        { type: "Customer", key: globex.id },
        naming,
      )
      .queryFn({});
    expect(names.get(refKey(globex))).toBe(page.entity?.name);
  });

  it("reads nothing without a load, or with no entity of the version's types", () => {
    const names = (loadId: string, refs = [globex], n = naming) =>
      readQueries.names("ws", "o", read, loadId, refs, n).enabled;
    expect(names(LOAD)).toBe(true);
    expect(names("")).toBe(false);
    expect(names(LOAD, [])).toBe(false);
    expect(names(LOAD, [partner])).toBe(false);
    expect(names(LOAD, [globex], NO_NAMING)).toBe(false);
  });
});
