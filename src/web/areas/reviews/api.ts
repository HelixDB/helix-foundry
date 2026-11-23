import type {
  BatchSettleReviewsRequest,
  BatchSettleReviewsResponse,
  DeleteReviewDecisionResponse,
  FoundryReviewChoice,
  FoundryReviewSettleRefusal,
  FoundryReviewSettleResult,
  ReviewSettleRefusalReason,
  FoundryReviewDecision,
  ListReviewDecisionsResponse,
  ReviewDecisionResponse,
  ReviewLabelResponse,
  SettleReviewLabelRequest,
  SettleReviewRequest,
} from "../../../shared/wfe";
import type { QueryClient } from "@tanstack/react-query";
import { trackEvent } from "../../analytics";
import { api } from "../../api";
import { applyPollMs, waiting } from "./model";
const enc = encodeURIComponent;
const base = (ws: string, o: string) =>
  `/v1/workspaces/${enc(ws)}/foundry/ontologies/${enc(o)}`;
// When this page first saw a decision of each ontology wait for its job
// (an approval or a chosen value), since one last began to: what the reads'
// back-off counts from. Gone once none waits.
const waitingSince = new Map<string, number>();
export const reviewQueries = {
  // Every decision of the ontology, a page (100) at a time: the backend lists them
  // with no filter. Pages show what is saved; settling never waits for it.
  // While an approval or a chosen value waits for the job that applies it to
  // the current load they are read again, every two seconds at first and
  // less often the longer one waits (applyPollMs), and once one has been
  // applied every read of the ontology is: its claim is a fact now, or its
  // card says why not, or its value shows, without the page being loaded
  // again.
  list: (ws: string, o: string) => ({
    queryKey: [ws, "ontology", o, "review-decisions"],
    staleTime: 30_000,
    refetchInterval: (q: { state: { data?: FoundryReviewDecision[] } }) =>
      waiting(q.state.data ?? []).length
        ? applyPollMs(
            Date.now() - (waitingSince.get(`${ws}\u001f${o}`) ?? Date.now()),
          )
        : false,
    queryFn: async ({ client }: { client: QueryClient }) => {
      const key = [ws, "ontology", o, "review-decisions"];
      const before = waiting(
        client.getQueryData<FoundryReviewDecision[]>(key) ?? [],
      );
      const decisions: FoundryReviewDecision[] = [];
      const seen = new Set<string>();
      let token = "";
      do {
        // With the current load's withdraws (contract 4.16.0), which a
        // client must ask for: one before them couldn't show one.
        const page = await api<ListReviewDecisionsResponse>(
          `${base(ws, o)}/review-decisions?includeWithdraws=true${token ? `&pageToken=${enc(token)}` : ""}`,
        );
        decisions.push(...(page.decisions || []));
        token = page.nextPageToken || "";
        // A token is the last id listed, so one never comes back.
        if (seen.has(token)) throw new Error("Decisions repeated a page.");
        seen.add(token);
      } while (token);
      const still = new Set(waiting(decisions).map((d) => d.id));
      // One that began to wait since the last read starts the quick reads again.
      const since = `${ws}\u001f${o}`,
        was = new Set(before.map((d) => d.id));
      if (!still.size) waitingSince.delete(since);
      else if (
        !waitingSince.has(since) ||
        [...still].some((id) => !was.has(id))
      )
        waitingSince.set(since, Date.now());
      if (before.some((d) => !still.has(d.id)))
        void client.invalidateQueries({
          queryKey: [ws, "ontology", o],
          // This read is the one that found it.
          predicate: (q) => q.queryKey[3] !== "review-decisions",
        });
      return decisions;
    },
  }),
};
/** Keeps a person's word on a claim the review walk drew (or re-checks). */
export const settleLabel = async (
  ws: string,
  o: string,
  body: SettleReviewLabelRequest,
) => {
  return api<ReviewLabelResponse>(`${base(ws, o)}/review-labels`, body);
};
const kindOf = (d: FoundryReviewChoice) =>
  d.prop ? "prop" : d.release ? "release" : d.reject ? "reject" : "resolve";
/**
 * Settles up to 100 reviews of the current load `loadId` in one call, each
 * as settleReview settles one; with `expectOpen`, one another person decided
 * is refused (DECIDED). The results, one an item in the items' order: the
 * decision saved, or why it was refused (refusalReason). A batch the API
 * refuses whole (another current load, bad fields) throws.
 */
export const settleReviews = async (
  ws: string,
  o: string,
  body: BatchSettleReviewsRequest,
): Promise<FoundryReviewSettleResult[]> => {
  const r = await api<BatchSettleReviewsResponse>(
    `${base(ws, o)}/reviews:batchSettle`,
    body,
  );
  const results = r.results ?? [];
  if (results.length !== body.items.length)
    throw new Error("The settle answered for other reviews than it was sent.");
  results.forEach((x, i) => {
    if (x.decision)
      trackEvent("review_settled", { kind: kindOf(body.items[i]!.decision) });
  });
  return results;
};
/**
 * Why an item of a batch settle was refused, by its reason alone (never its
 * message); one this build doesn't know reads as INVALID, which shows the
 * message on its card.
 *
 * @example
 * refusalReason({ reason: "FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_DECIDED" }); // "DECIDED"
 */
export function refusalReason(
  r: FoundryReviewSettleRefusal | undefined,
): ReviewSettleRefusalReason {
  const name = (r?.reason ?? "").replace(
    "FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_",
    "",
  );
  return name === "STALE" ||
    name === "DECIDED" ||
    name === "FULL" ||
    name === "NOT_FOUND"
    ? name
    : "INVALID";
}
export const settleReview = async (
  ws: string,
  o: string,
  reviewId: string,
  body: SettleReviewRequest,
) => {
  const r = await api<ReviewDecisionResponse>(
    `${base(ws, o)}/reviews/${enc(reviewId)}:settle`,
    body,
  );
  trackEvent("review_settled", { kind: kindOf(body.decision) });
  return r;
};
export const deleteReviewDecision = async (
  ws: string,
  o: string,
  id: string,
) => {
  const r = await api<DeleteReviewDecisionResponse>(
    `${base(ws, o)}/review-decisions/${enc(id)}`,
    undefined,
    "DELETE",
  );
  trackEvent("review_decision_removed");
  return r;
};
