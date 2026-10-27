import { QueryClient, QueryObserver } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import contractsBundle from "../../mock/fixtures/agent-bundle-contracts.json";
import { runQuery } from "../../mock/graph/query";
import { MockGraph } from "../../mock/graph/store";
import type { OntologyVersion } from "@/shared/wfe";
import type {
  DatabaseQueryResult,
  QueryExecutor,
} from "@/web/areas/explore/database-query";
import { loadCountsQuery } from "@/web/areas/explore/load-counts";
import { ReadFailed, readOptions, startingOf } from "@/web/areas/explore/reads";

// What a load holds, per type and link of its version, as the Loads page and
// a compare read it: one query per load and version, not read for a load on
// its way out, and read again after the wait while a new database starts.

const VERSION: OntologyVersion = {
  id: "ver_3",
  format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
  definition: contractsBundle,
};
const LOAD = { id: "load_1", status: "ready" } as const;

const STARTING: DatabaseQueryResult = {
  ok: false,
  kind: "starting",
  status: 503,
  code: "rate_limit_unavailable",
  message: "Helix is still starting this new database.",
  retryInMs: 5_000,
  elapsedMs: 0,
};

// The database, over a mock graph, refusing its first `refusals` reads as a
// database still starting.
function database(graph: MockGraph, refusals = 0) {
  let calls = 0;
  const read: QueryExecutor = async (request) => {
    calls += 1;
    if (calls <= refusals) return STARTING;
    const answer = runQuery(graph, request);
    expect(answer.status).toBe(200);
    return {
      ok: true,
      status: 200,
      contentType: null,
      body: answer.body,
      bytes: 0,
      elapsedMs: 0,
    };
  };
  return { read, calls: () => calls };
}

// Two customers, an invoice billed to one, in the load; a customer of the
// team's own and one of another load, in neither count.
function graphWithLoad() {
  const graph = new MockGraph();
  const load = graph.view(LOAD.id);
  const c1 = load.addNode("Customer", { customer_id: "c1" });
  load.addNode("Customer", { customer_id: "c2" });
  const invoice = load.addNode("Invoice", { invoice_id: "i1" });
  load.addEdge("billed_to", invoice, c1);
  graph.addNode("Customer", { customer_id: "own" });
  graph.view("load_2").addNode("Customer", { customer_id: "c3" });
  return graph;
}

describe("loadCountsQuery", () => {
  afterEach(() => vi.useRealTimers());

  it("counts each type and link of the version in the load, keyed by load and version", async () => {
    const db = database(graphWithLoad());
    const q = loadCountsQuery("ws", "o", db.read, LOAD, VERSION);
    expect(q.queryKey).toEqual([
      "ws",
      "ontology",
      "o",
      "read",
      "load_1",
      "counts",
      "ver_3",
    ]);
    expect(q.enabled).toBe(true);
    const counts = await q.queryFn({});
    expect(Object.fromEntries(counts.nodes)).toEqual({
      Contract: 0,
      Customer: 2,
      Invoice: 1,
    });
    expect(Object.fromEntries(counts.edges)).toEqual({
      billed_to: 1,
      signed_with: 0,
    });
  });

  it("reads nothing without the load's version, or for a load on its way out", () => {
    const { read } = database(new MockGraph());
    expect(loadCountsQuery("ws", "o", read, LOAD, null).enabled).toBe(false);
    expect(loadCountsQuery("ws", "o", read, LOAD, undefined).enabled).toBe(
      false,
    );
    for (const status of ["deleting", "delete_failed"] as const)
      expect(
        loadCountsQuery("ws", "o", read, { id: "load_1", status }, VERSION)
          .enabled,
      ).toBe(false);
    for (const status of ["writing", "partial", "failed"] as const)
      expect(
        loadCountsQuery("ws", "o", read, { id: "load_1", status }, VERSION)
          .enabled,
      ).toBe(true);
  });

  it("fails as starting while a new database starts, and reads again after the transport's wait", async () => {
    const db = database(graphWithLoad(), 1);
    const q = loadCountsQuery("ws", "o", db.read, LOAD, VERSION);
    const error = await q.queryFn({}).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ReadFailed);
    expect(startingOf(error)).toMatchObject({ retryInMs: 5_000 });
    expect(q.retry).toBe(readOptions.retry);
    expect(q.retryDelay).toBe(readOptions.retryDelay);
    expect(q.retry(0, error as Error)).toBe(true);
    expect(q.retryDelay(0, error as Error)).toBe(5_000);
    // Any other failure is the page's error, not retried.
    const refused = new ReadFailed({
      ok: false,
      kind: "database",
      status: 503,
      code: "rate_limit_unavailable",
      message: "refused",
      elapsedMs: 0,
    });
    expect(q.retry(0, refused)).toBe(false);
  });

  it("stays pending and starting until the database answers, then gives the counts", async () => {
    vi.useFakeTimers();
    const db = database(graphWithLoad(), 2);
    const observer = new QueryObserver(
      new QueryClient(),
      loadCountsQuery("ws", "o", db.read, LOAD, VERSION),
    );
    const seen: { status: string; starting: boolean; customers?: number }[] =
      [];
    observer.subscribe((r) =>
      seen.push({
        status: r.status,
        starting: !!startingOf(r.error ?? r.failureReason),
        customers: r.data?.nodes.get("Customer") ?? undefined,
      }),
    );
    await vi.advanceTimersByTimeAsync(0);
    expect(db.calls()).toBe(1);
    expect(seen.at(-1)).toEqual({ status: "pending", starting: true });
    await vi.advanceTimersByTimeAsync(5_000);
    expect(db.calls()).toBe(2);
    expect(seen.at(-1)).toEqual({ status: "pending", starting: true });
    await vi.advanceTimersByTimeAsync(5_000);
    expect(seen.at(-1)).toEqual({
      status: "success",
      starting: false,
      customers: 2,
    });
    // From the first refusal on, every result says starting until the counts.
    const rest = seen.slice(seen.findIndex((r) => r.starting));
    expect(rest.slice(0, -1).every((r) => r.starting)).toBe(true);
    observer.destroy();
  });
});
