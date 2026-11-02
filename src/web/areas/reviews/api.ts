import type {
  DeleteReviewDecisionResponse,
  FoundryReviewDecision,
  ListReviewDecisionsResponse,
  ReviewDecisionResponse,
  SettleReviewRequest,
} from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import { api } from "../../api";
const enc = encodeURIComponent;
const base = (ws: string, o: string) =>
  `/v1/workspaces/${enc(ws)}/foundry/ontologies/${enc(o)}`;
export const reviewQueries = {
  // Every decision of the ontology, a page (100) at a time: the backend lists them
  // with no filter. Pages show what is saved; settling never waits for it.
  list: (ws: string, o: string) => ({
    queryKey: [ws, "ontology", o, "review-decisions"],
    staleTime: 30_000,
    queryFn: async () => {
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
    kind: body.decision.prop ? "prop" : "resolve",
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
