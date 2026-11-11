import { QueryClient } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "@/web/api";
import * as reviews from "@/web/areas/reviews/api";
import { APPLY_POLL_MS } from "@/web/areas/reviews/model";
vi.mock("@/web/api", () => ({ api: vi.fn() }));
const call = vi.mocked(api);
beforeEach(() => call.mockReset());
/** The decisions of ws's ontology o, read with `client`'s cache. */
const read = (client = new QueryClient()) =>
  reviews.reviewQueries.list("ws", "o").queryFn({ client });
describe("review API boundary", () => {
  it("encodes every path segment and sends only the resolve choice", async () => {
    const body = { decision: { resolve: { entityKey: "customer" } } };
    await reviews.settleReview("ws /", "o ?", "review/#", body);
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws%20%2F/foundry/ontologies/o%20%3F/reviews/review%2F%23:settle",
      body,
    );
  });
  it("keeps false as a property choice and uses DELETE for removal", async () => {
    await reviews.settleReview("ws", "o", "review", {
      decision: { prop: { boolean: false } },
    });
    expect(call).toHaveBeenLastCalledWith(
      "/v1/workspaces/ws/foundry/ontologies/o/reviews/review:settle",
      { decision: { prop: { boolean: false } } },
    );
    await reviews.deleteReviewDecision("ws", "o", "id /#");
    expect(call).toHaveBeenLastCalledWith(
      "/v1/workspaces/ws/foundry/ontologies/o/review-decisions/id%20%2F%23",
      undefined,
      "DELETE",
    );
  });
  it("reads every page using the opaque last-decision token", async () => {
    call
      .mockResolvedValueOnce({
        decisions: [{ id: "one" }],
        nextPageToken: "one /#",
      })
      .mockResolvedValueOnce({ decisions: [{ id: "two" }] });
    const q = reviews.reviewQueries.list("ws", "o");
    expect(q.queryKey).toEqual(["ws", "ontology", "o", "review-decisions"]);
    expect(await read()).toEqual([{ id: "one" }, { id: "two" }]);
    expect(call).toHaveBeenNthCalledWith(
      2,
      "/v1/workspaces/ws/foundry/ontologies/o/review-decisions?pageToken=one%20%2F%23",
    );
  });
  it("stops when a page token comes back, instead of reading forever", async () => {
    call.mockResolvedValue({
      decisions: [{ id: "one" }],
      nextPageToken: "one",
    });
    await expect(read()).rejects.toThrow("Decisions repeated a page.");
    expect(call).toHaveBeenCalledTimes(2);
  });
  it("handles an omitted empty list and propagates failed reads", async () => {
    call.mockResolvedValueOnce({});
    expect(await read()).toEqual([]);
    call.mockRejectedValueOnce(new Error("unavailable"));
    await expect(read()).rejects.toThrow("unavailable");
  });
});

describe("an approval waiting for its job", () => {
  const key = ["ws", "ontology", "o", "review-decisions"];
  const pending = { id: "a", applied: { status: "pending" } },
    written = { id: "a", applied: { status: "written" } };

  it("has the decisions read again every two seconds, and only while it waits", () => {
    const { refetchInterval } = reviews.reviewQueries.list("ws", "o");
    expect(refetchInterval({ state: { data: [pending] } } as never)).toBe(
      APPLY_POLL_MS,
    );
    expect(APPLY_POLL_MS).toBe(2000);
    expect(refetchInterval({ state: { data: [written] } } as never)).toBe(
      false,
    );
    expect(refetchInterval({ state: {} } as never)).toBe(false);
  });

  it("never has them read again for one nothing is coming for", () => {
    const { refetchInterval } = reviews.reviewQueries.list("ws", "o");
    for (const applied of [
      // Its job failed before saying what became of it.
      { status: "unconfirmed", reason: "job_failed" },
      { status: "refused", reason: "document_unread" },
      { status: "missed" },
      // The load read it as it was created.
      { status: "" },
      // The current load's list doesn't hold it: its document isn't read there.
      undefined,
    ])
      expect(
        refetchInterval({ state: { data: [{ id: "a", applied }] } } as never),
      ).toBe(false);
  });

  it("reads less often the longer one waits, and quickly again when another begins to", async () => {
    vi.useFakeTimers();
    try {
      vi.setSystemTime(new Date("2026-11-11T10:00:00Z"));
      const client = new QueryClient();
      const { refetchInterval } = reviews.reviewQueries.list("ws", "o");
      const every = () =>
        refetchInterval({ state: { data: client.getQueryData(key) } } as never);
      const reads = async (decisions: unknown[]) => {
        call.mockResolvedValueOnce({ decisions });
        client.setQueryData(key, await read(client));
      };
      vi.spyOn(client, "invalidateQueries").mockResolvedValue();
      await reads([pending]);
      expect(every()).toBe(2000);
      vi.advanceTimersByTime(29_000);
      await reads([pending]);
      expect(every()).toBe(2000);
      // Behind another job: half a minute on, every ten seconds.
      vi.advanceTimersByTime(2_000);
      await reads([pending]);
      expect(every()).toBe(10_000);
      // Five minutes on, once a minute, for as long as it waits.
      vi.advanceTimersByTime(5 * 60_000);
      await reads([pending]);
      expect(every()).toBe(60_000);
      vi.advanceTimersByTime(3 * 60 * 60_000);
      await reads([pending]);
      expect(every()).toBe(60_000);
      // Another approval lands: its job is seconds away.
      await reads([pending, { id: "b", applied: { status: "pending" } }]);
      expect(every()).toBe(2000);
      // None waits: nothing is read again, and the next wait starts afresh.
      await reads([written]);
      expect(every()).toBe(false);
      vi.advanceTimersByTime(60 * 60_000);
      await reads([pending]);
      expect(every()).toBe(2000);
      // Another ontology's wait is its own.
      expect(
        reviews.reviewQueries
          .list("ws", "other")
          .refetchInterval({ state: { data: [pending] } } as never),
      ).toBe(2000);
    } finally {
      vi.useRealTimers();
    }
  });

  it("has every read of the ontology made again once it is applied, but this one", async () => {
    const client = new QueryClient();
    client.setQueryData(key, [pending, { id: "b" }]);
    const invalidate = vi
      .spyOn(client, "invalidateQueries")
      .mockResolvedValue();
    call.mockResolvedValueOnce({ decisions: [written, { id: "b" }] });
    expect(await read(client)).toEqual([written, { id: "b" }]);
    expect(invalidate).toHaveBeenCalledTimes(1);
    const filter = invalidate.mock.calls[0]![0]!;
    expect(filter.queryKey).toEqual(["ws", "ontology", "o"]);
    const matches = (queryKey: unknown[]) =>
      filter.predicate!({ queryKey } as never);
    expect(matches(key)).toBe(false);
    expect(matches(["ws", "ontology", "o", "read", "load", "document"])).toBe(
      true,
    );
    expect(matches(["ws", "ontology", "o"])).toBe(true);
  });

  it("says so for one refused, missed or taken back too", async () => {
    for (const now of [
      [{ id: "a", applied: { status: "refused", reason: "unresolved" } }],
      [{ id: "a", applied: { status: "missed" } }],
      [],
    ]) {
      const client = new QueryClient();
      client.setQueryData(key, [pending]);
      const invalidate = vi
        .spyOn(client, "invalidateQueries")
        .mockResolvedValue();
      call.mockResolvedValueOnce({ decisions: now });
      await read(client);
      expect(invalidate).toHaveBeenCalledTimes(1);
    }
  });

  it("changes nothing while it still waits, or when none did", async () => {
    for (const before of [[pending], [written], undefined]) {
      const client = new QueryClient();
      if (before) client.setQueryData(key, before);
      const invalidate = vi
        .spyOn(client, "invalidateQueries")
        .mockResolvedValue();
      call.mockResolvedValueOnce({ decisions: before ?? [pending] });
      await read(client);
      expect(invalidate).not.toHaveBeenCalled();
    }
  });
});
