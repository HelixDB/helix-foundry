import { beforeEach, describe, expect, it } from "vitest";
import shopBundle from "../../mock/fixtures/agent-bundle.json";
import { runQuery } from "../../mock/graph/query";
import { MockGraph } from "../../mock/graph/store";
import { namingOf } from "@/web/areas/explore/naming";
import type { QueryExecutor } from "@/web/areas/explore/database-query";
import type { Scope } from "@/web/areas/explore/load-scope";
import { ReadFailed } from "@/web/areas/explore/reads";
import type { RecordsView } from "@/web/areas/records/model";
import { recordQueries } from "@/web/areas/records/queries";

// Records' reads as query options: they run the model's requests through
// the transport, answer a search nothing can match without reading, stay
// off for a filter that isn't one, and throw ReadFailed on a failed read.

let graph: MockGraph, calls: number, fail: boolean;
// The database's shared reads, over the mock graph.
const read: QueryExecutor = async (request) => {
  calls += 1;
  if (fail)
    return {
      ok: false,
      kind: "forbidden",
      status: 403,
      code: null,
      message: "no access",
      elapsedMs: 1,
    };
  const answer = runQuery(graph, request);
  return {
    ok: true,
    status: answer.status,
    contentType: null,
    body: answer.body,
    bytes: 0,
    elapsedMs: 0,
  };
};
const LOAD = "load_1";
const SCOPE: Scope = { kind: "load", id: LOAD };

const naming = namingOf({
  id: "ver_2",
  format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
  definition: shopBundle,
});
const channel = naming.types.get("SalesChannel")!;
const view = (v: Partial<RecordsView> = {}): RecordsView => ({
  type: "SalesChannel",
  search: "",
  filter: null,
  sort: null,
  page: 0,
  record: "",
  ...v,
});

beforeEach(() => {
  graph = new MockGraph();
  for (const code of [7, 12])
    graph.view(LOAD).addNode("SalesChannel", { entity_key: `${code}`, code });
  calls = 0;
  fail = false;
});

describe("recordQueries", () => {
  it("counts the version's types", async () => {
    const q = recordQueries.counts("ws", "o", read, SCOPE, naming);
    expect(q.queryKey).toContain("ver_2");
    const counts = await q.queryFn({});
    expect(counts.get("SalesChannel")).toBe(2);
    expect(counts.get("Order")).toBe(0);
  });

  it("reads a page, and answers a search nothing can match without reading", async () => {
    const page = await recordQueries
      .page("ws", "o", read, SCOPE, naming, channel, view())
      .queryFn({});
    expect(page.rows.map((r) => r.name)).toEqual(["7", "12"]);
    expect(calls).toBe(1);
    const none = recordQueries.page(
      "ws",
      "o",
      read,
      SCOPE,
      naming,
      channel,
      view({ search: "web" }),
    );
    expect(none.enabled).toBe(true);
    expect(await none.queryFn({})).toEqual({
      rows: [],
      more: false,
      matched: { count: 0, capped: false },
    });
    expect(calls).toBe(1);
  });

  it("stays off for a filter that isn't one", async () => {
    const q = recordQueries.page(
      "ws",
      "o",
      read,
      SCOPE,
      naming,
      channel,
      view({ filter: { field: "code", value: "web" } }),
    );
    expect(q.enabled).toBe(false);
    await expect(q.queryFn({})).rejects.toThrow("Enter a whole number.");
    // A field with no value yet reads, and shares, the whole type's page.
    const picked = recordQueries.page(
      "ws",
      "o",
      read,
      SCOPE,
      naming,
      channel,
      view({ filter: { field: "code", value: " " } }),
    );
    expect(picked.enabled).toBe(true);
    expect(picked.queryKey).toEqual(
      recordQueries.page("ws", "o", read, SCOPE, naming, channel, view())
        .queryKey,
    );
  });

  it("throws the transport's failure", async () => {
    fail = true;
    await expect(
      recordQueries.counts("ws", "o", read, SCOPE, naming).queryFn({}),
    ).rejects.toBeInstanceOf(ReadFailed);
    await expect(
      recordQueries
        .page("ws", "o", read, SCOPE, naming, channel, view())
        .queryFn({}),
    ).rejects.toThrow("no access");
  });
});

describe("recordQueries in Other data", () => {
  it("counts a type's nodes no load wrote: its count less the loads'", async () => {
    // The team's own sales channel, beside the load's two.
    graph.addNode("SalesChannel", { entity_key: "99", code: 99 });
    const other: Scope = {
      kind: "other",
      loads: [LOAD],
      shared: { nodes: new Set(["SalesChannel"]), edges: new Set() },
    };
    const counts = await recordQueries
      .counts("ws", "o", read, other, naming)
      .queryFn({});
    expect(counts.get("SalesChannel")).toBe(1);
    const page = await recordQueries
      .page("ws", "o", read, other, naming, channel, view())
      .queryFn({});
    expect(page.rows.map((r) => r.name)).toEqual(["99"]);
    // Keys differ per load, so the views cache apart.
    expect(
      recordQueries.page("ws", "o", read, other, naming, channel, view())
        .queryKey,
    ).not.toEqual(
      recordQueries.page("ws", "o", read, SCOPE, naming, channel, view())
        .queryKey,
    );
  });
});
