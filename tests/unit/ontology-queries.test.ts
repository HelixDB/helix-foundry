import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { FoundryLoad } from "@/shared/wfe";

// The loads an ontology keeps: one page (retention keeps a handful), polled
// while one is written or deleted; set-current and delete go to the load's
// own routes.

let calls: { url: string; method: string }[];
beforeEach(() => {
  calls = [];
  vi.resetModules();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      if (url === "/papi/csrf")
        return Response.json({ token: "t", expiresInSeconds: 600 });
      calls.push({ url, method: init?.method || "GET" });
      return Response.json(
        url.endsWith("/loads")
          ? {
              loads: [
                { id: "L2", status: "writing", jobs: [] },
                { id: "L1", status: "ready", current: true, jobs: [] },
              ],
            }
          : { load: { id: "L1", status: "ready" } },
      );
    }),
  );
});
afterEach(() => vi.unstubAllGlobals());

const L = "/papi/v1/workspaces/ws_1/foundry/ontologies/ont_1/loads";

describe("ontology load queries", () => {
  it("reads every load at once, and polls while one is written or deleted", async () => {
    const { ontologyQueries, POLL_MS } = await import(
      "@/web/areas/ontology/queries"
    );
    const q = ontologyQueries.loads("ws_1", "ont_1");
    const data = await q.queryFn();
    expect(calls.map((c) => c.url)).toEqual([L]);
    expect(data.loads?.map((l) => l.id)).toEqual(["L2", "L1"]);
    expect(q.refetchInterval({ state: { data } })).toBe(POLL_MS);
    const settled = (l: Partial<FoundryLoad>) => ({
      loads: [{ id: "x", ...l }],
    });
    expect(
      q.refetchInterval({ state: { data: settled({ status: "ready" }) } }),
    ).toBe(false);
    expect(
      q.refetchInterval({ state: { data: settled({ status: "deleting" }) } }),
    ).toBe(POLL_MS);
    // A ready load whose extract runs is busy too.
    expect(
      q.refetchInterval({
        state: {
          data: settled({
            status: "ready",
            jobs: [{ id: "j", kind: "extract", status: "queued" }],
          }),
        },
      }),
    ).toBe(POLL_MS);
    // The load's own key sits under the list's, so invalidating loads refreshes both.
    expect(
      ontologyQueries.load("ws_1", "ont_1", "L1").queryKey.slice(0, 4),
    ).toEqual(q.queryKey);
  });

  it("polls the ontology while a job is live", async () => {
    const { ontologyQueries, POLL_MS } = await import(
      "@/web/areas/ontology/queries"
    );
    const q = ontologyQueries.ontology("ws_1", "ont_1");
    expect(
      q.refetchInterval({
        state: {
          data: {
            ontology: { id: "o", activeJob: { kind: "delete", loadId: "L" } },
          },
        },
      }),
    ).toBe(POLL_MS);
    expect(
      q.refetchInterval({ state: { data: { ontology: { id: "o" } } } }),
    ).toBe(false);
  });

  it("tells each job's end, also when the next job of a chain starts at once", async () => {
    const { jobEnded, jobsSeen } = await import("@/web/areas/ontology/queries");
    const load = (id: string, ...jobs: [string, string][]): FoundryLoad => ({
      id,
      jobs: jobs.map(([j, status]) => ({ id: j, status: status as never })),
    });
    const at = (...loads: FoundryLoad[]) => jobsSeen(loads);
    const loading = at(
      load("L2", ["J2", "running"]),
      load("L1", ["J1", "succeeded"]),
    );
    // Nothing changed, or a job started: no end.
    expect(jobEnded(loading, loading)).toBe(false);
    expect(
      jobEnded(
        at(load("L1", ["J1", "succeeded"])),
        at(load("L2", ["J2", "queued"]), load("L1", ["J1", "succeeded"])),
      ),
    ).toBe(false);
    // The load ended and its extract was queued in the same step: the
    // ontology was never idle, and the end still counts.
    const extracting = at(
      load("L2", ["J3", "queued"], ["J2", "succeeded"]),
      load("L1", ["J1", "succeeded"]),
    );
    expect(jobEnded(loading, extracting)).toBe(true);
    // A retention delete chained after it: the extract's end counts.
    const deleting = at(
      load("L2", ["J3", "succeeded"], ["J2", "succeeded"]),
      load("L1", ["J4", "running"], ["J1", "succeeded"]),
    );
    expect(jobEnded(extracting, deleting)).toBe(true);
    // The delete ended: its load left the list, jobs and all.
    const deleted = at(load("L2", ["J3", "succeeded"], ["J2", "succeeded"]));
    expect(jobEnded(deleting, deleted)).toBe(true);
    // A job that ran between two reads shows finished at once.
    expect(
      jobEnded(
        deleted,
        at(
          load(
            "L2",
            ["J5", "failed"],
            ["J3", "succeeded"],
            ["J2", "succeeded"],
          ),
        ),
      ),
    ).toBe(true);
    // A load whose delete was never seen live left the list.
    expect(jobEnded(at(load("L2"), load("L1")), at(load("L2")))).toBe(true);
    expect(jobsSeen()).toEqual({ live: [], done: [], loads: [] });
  });

  it("makes a load current and deletes one on its own routes", async () => {
    const { setCurrentLoad, deleteLoad } = await import(
      "@/web/areas/ontology/queries"
    );
    await setCurrentLoad("ws_1", "ont_1", "L1");
    await deleteLoad("ws_1", "ont_1", "L 2");
    expect(calls).toEqual([
      { url: `${L}/L1:set-current`, method: "POST" },
      { url: `${L}/L%202`, method: "DELETE" },
    ]);
  });
});
