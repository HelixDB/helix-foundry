import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "@/web/api";
import * as reviews from "@/web/areas/reviews/api";
vi.mock("@/web/api", () => ({ api: vi.fn() }));
const call = vi.mocked(api);
beforeEach(() => call.mockReset());
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
    expect(await q.queryFn()).toEqual([{ id: "one" }, { id: "two" }]);
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
    await expect(
      reviews.reviewQueries.list("ws", "o").queryFn(),
    ).rejects.toThrow("Decisions repeated a page.");
    expect(call).toHaveBeenCalledTimes(2);
  });
  it("handles an omitted empty list and propagates failed reads", async () => {
    call.mockResolvedValueOnce({});
    expect(await reviews.reviewQueries.list("ws", "o").queryFn()).toEqual([]);
    call.mockRejectedValueOnce(new Error("unavailable"));
    await expect(
      reviews.reviewQueries.list("ws", "o").queryFn(),
    ).rejects.toThrow("unavailable");
  });
});
