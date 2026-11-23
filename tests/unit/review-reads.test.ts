import { describe, expect, it } from "vitest";
import { runQuery } from "../../mock/graph/query";
import { MockGraph } from "../../mock/graph/store";
import type { FoundryReviewLabel } from "@/shared/wfe";
import {
  documentQuotesRequest,
  parseDocumentQuotes,
  parseLoadReviews,
  parseNameLinks,
  parseQuoteFacts,
  quoteFactsRequest,
  reviewsAtRequest,
  reviewsByKeyRequest,
} from "@/web/areas/explore/model";
import {
  BANDS,
  bandOf,
  claimPlace,
  labelPlace,
  scoreText,
} from "@/web/areas/reviews/queue";

// The review walk's cards as the page reads them (reviews/queue.ts): bands,
// scores and places; and the reads of a load that render them
// (explore/model.ts), run over a mock graph.

describe("bands, scores and places", () => {
  it("bands a score by the tenths the spot checks draw from", () => {
    expect(bandOf(1)).toBe("0.9");
    expect(bandOf(0.9)).toBe("0.9");
    expect(bandOf(0.8999)).toBe("0.8");
    expect(bandOf(0.8)).toBe("0.8");
    expect(bandOf(0.7)).toBe("0.7");
    expect(bandOf(0.6)).toBe("0.6");
    expect(bandOf(0.59)).toBe("low");
    expect(bandOf(undefined)).toBe("none");
    expect(bandOf(Number.NaN)).toBe("none");
    // Approved with no score: written at 0.
    expect(bandOf(0)).toBe("none");
    expect(BANDS[0]).toBe("0.9");
  });
  it("shows a score cut to two decimals, never the next band's", () => {
    expect(scoreText(0.8999)).toBe("0.89");
    expect(scoreText(0.9)).toBe("0.90");
    expect(scoreText(0.72)).toBe("0.72");
  });
  it("names a claim by its document, place and link", () => {
    const x: FoundryReviewLabel = {
      id: "x",
      drawn: "random",
      docId: "doc-1",
      claimSection: 0,
      claimStart: 10,
      claimEnd: 15,
      link: "partner_of",
      verdict: "right",
    };
    expect(labelPlace(x)).toBe(
      claimPlace(
        { documentId: "doc-1", section: 0, start: 10, end: 15 },
        "partner_of",
      ),
    );
    expect(labelPlace(x)).not.toBe(labelPlace({ ...x, claimStart: 11 }));
    expect(labelPlace(x)).not.toBe(labelPlace({ ...x, link: "owns" }));
  });
});

// A load in a mock graph: two documents' sections, reviews of them and of a
// table's rows, accepted and held quotes, and a fact's ends.
function loadGraph() {
  const g = new MockGraph(),
    L = { foundry_load: "load-1" };
  const section = (doc: string, ordinal: number) =>
    g.addNode("FoundrySection", { ...L, doc_id: doc, ordinal });
  const s0 = section("doc-1", 0),
    s1 = section("doc-1", 1),
    s2 = section("doc-2", 0);
  const acme = g.addNode("Account", { ...L, norm_name: "acme", name: "Acme" }),
    globex = g.addNode("Account", {
      ...L,
      norm_name: "globex",
      name: "Globex",
    });
  g.addEdge("partner_of", acme, globex, { ...L, fact_key: "fk-1" });
  const quote = (s: number, doc: string, fk: string, confidence: number) => {
    const q = g.addNode("FoundryEvidence", {
      ...L,
      fact_key: fk,
      link: "partner_of",
      doc_id: doc,
      start: 3,
      end: 9,
      quote: `q ${fk}`,
      confidence,
    });
    g.addEdge("FOUNDRY_FROM_SECTION", q, s, L);
  };
  quote(s0, "doc-1", "fk-1", 0.95);
  quote(s1, "doc-1", "fk-2", 0.9);
  quote(s2, "doc-2", "fk-3", 0.72); // released: under the bar
  quote(s2, "doc-2", "fk-5", 0); // released with no score
  // A property's quote at the bar: no fact key, no fact to label.
  const prop = g.addNode("FoundryEvidence", {
    ...L,
    prop: "currency",
    value: "GBP",
    doc_id: "doc-1",
    start: 1,
    end: 4,
    confidence: 0.94,
  });
  g.addEdge("FOUNDRY_FROM_SECTION", prop, s0, L);
  // A row's evidence, scored, in no section.
  g.addNode("FoundryEvidence", {
    ...L,
    fact_key: "fk-4",
    source: "crm",
    confidence: 1,
  });
  const review = (props: Record<string, unknown>, s?: number) => {
    const r = g.addNode("FoundryReview", { ...L, ...props } as never);
    if (s !== undefined) g.addEdge("FOUNDRY_FROM_SECTION", r, s, L);
    return r;
  };
  const named = review(
    {
      review_key: "r-name",
      reason: "unresolved",
      doc_id: "doc-1",
      section: 1,
      start: 0,
      end: 4,
      quote: "Acme",
      type: "Account",
      norm_name: "acme",
      candidates: ["acme"],
    },
    s1,
  );
  g.addEdge("FOUNDRY_CONCERNS", named, acme, L);
  review({ review_key: "r-row", chunk_key: "c", kind: "row" });
  // Another load's review is not this load's.
  g.addNode("FoundryReview", {
    foundry_load: "load-0",
    review_key: "old",
    doc_id: "doc-1",
  });
  return g;
}

const ask = (g: MockGraph, request: unknown) => {
  const answer = runQuery(g, request);
  expect(answer.status).toBe(200);
  return answer.body;
};

describe("a load's reviews, as the walk reads them", () => {
  const scope = { id: "load-1" };
  it("reads reviews by key, with what settles them, and the name links of documents", () => {
    const g = loadGraph();
    g.addNode("FoundryReview", {
      foundry_load: "load-1",
      review_key: "r-part",
      reason: "part_name",
      doc_id: "doc-1",
      section: 0,
      start: 20,
      end: 24,
      type: "Account",
      norm_name: "acme",
    });
    const body = ask(
      g,
      reviewsByKeyRequest(scope, ["r-name", "old", "missing"], ["doc-1"]),
    );
    const reviews = parseLoadReviews(body);
    // Another load's review of the same key isn't this load's.
    expect(reviews).toHaveLength(1);
    expect(reviews[0]).toMatchObject({
      documentId: "doc-1",
      section: 1,
      reason: "unresolved",
      candidates: [{ key: "acme", entity: { name: "Acme" } }],
      settle: { kind: "resolve", reviewId: "r-name", normName: "acme" },
    });
    expect(parseNameLinks(body)).toMatchObject([
      { reason: "part_name", settle: { kind: "resolve", normName: "acme" } },
    ]);
    // Without documents, no links; without keys, no read.
    expect(
      parseNameLinks(ask(g, reviewsByKeyRequest(scope, ["r-name"]))),
    ).toEqual([]);
    expect(reviewsByKeyRequest(scope, [])).toBeNull();
    expect(parseLoadReviews(undefined)).toEqual([]);
  });
  it("reads the reviews of documents at the places asked", () => {
    const g = loadGraph();
    const at = parseLoadReviews(
      ask(g, reviewsAtRequest(scope, ["doc-1", "doc-2"], [0]))!,
    );
    expect(at.map((r) => [r.documentId, r.start])).toEqual([["doc-1", 0]]);
    expect(
      parseLoadReviews(ask(g, reviewsAtRequest(scope, ["doc-1"], [5]))!),
    ).toEqual([]);
    expect(reviewsAtRequest(scope, [], [0])).toBeNull();
  });
  it("reads a document's quotes at their places, and their facts' ends", () => {
    const g = loadGraph();
    const quotes = parseDocumentQuotes(
      ask(g, documentQuotesRequest(scope, ["doc-2", "doc-1"])!),
      ["doc-2", "doc-1"],
    );
    expect(quotes.map((q) => [q.documentId, q.factKey, q.section])).toEqual([
      ["doc-2", "fk-3", 0],
      ["doc-2", "fk-5", 0],
      ["doc-1", "fk-1", 0],
      ["doc-1", "fk-2", 1],
    ]);
    expect(documentQuotesRequest(scope, [])).toBeNull();
    const read = quotes.filter((q) => q.documentId === "doc-1");
    const ends = parseQuoteFacts(
      ask(g, quoteFactsRequest(scope, read)!),
      read.length,
    );
    expect(ends[0]?.from?.name).toBe("Acme");
    expect(ends[0]?.to?.name).toBe("Globex");
    // fk-2 has no edge in this load.
    expect(ends[1]).toEqual({});
    expect(quoteFactsRequest(scope, [])).toBeNull();
  });
});
