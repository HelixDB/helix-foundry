import type {
  DeleteReviewDecisionResponse,
  FoundryReviewDecision,
  ListReviewDecisionsResponse,
  ReviewDecisionResponse,
  SettleReviewRequest,
} from "../../../shared/wfe";
import type { QueryClient } from "@tanstack/react-query";
import { trackEvent } from "../../analytics";
import { api } from "../../api";
import { waiting } from "./model";
const enc = encodeURIComponent;
const base = (ws: string, o: string) =>
  `/v1/workspaces/${enc(ws)}/foundry/ontologies/${enc(o)}`;
/** How often the decisions are read while an approval waits for its job. */
export const APPLY_POLL_MS = 2000;
export const reviewQueries = {
  // Every decision of the ontology, a page (100) at a time: the backend lists them
  // with no filter. Pages show what is saved; settling never waits for it.
  // While an approval waits for the job that applies it to the current load
  // they are read again every APPLY_POLL_MS, and once one has been applied
  // every read of the ontology is: its claim is a fact now, or its card says
  // why not, without the page being loaded again.
  list: (ws: string, o: string) => ({
    queryKey: [ws, "ontology", o, "review-decisions"],
    staleTime: 30_000,
    refetchInterval: (q: { state: { data?: FoundryReviewDecision[] } }) =>
      waiting(q.state.data ?? []).length ? APPLY_POLL_MS : false,
    queryFn: async ({ client }: { client: QueryClient }) => {
      const key = [ws, "ontology", o, "review-decisions"];
      const before = waiting(
        client.getQueryData<FoundryReviewDecision[]>(key) ?? [],
      );
      const decisions: FoundryReviewDecision[] = [];
      const seen = new Set<string>();
      let token = "";
      do {
        const page = await api<ListReviewDecisionsResponse>(
          `${base(ws, o)}/review-decisions${token ? `?pageToken=${enc(token)}` : ""}`,
        );
        decisions.push(...(page.decisions || []));
        token = page.nextPageToken || "";
        // A token is the last id listed, so one never comes back.
        if (seen.has(token)) throw new Error("Decisions repeated a page.");
        seen.add(token);
      } while (token);
      const still = new Set(waiting(decisions).map((d) => d.id));
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
  trackEvent("review_settled", {
    kind: body.decision.prop
      ? "prop"
      : body.decision.release
        ? "release"
        : "resolve",
  });
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
