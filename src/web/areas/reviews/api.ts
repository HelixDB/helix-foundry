import type {
  DeleteReviewDecisionResponse,
  FoundryReviewDecision,
  FoundryReviewLabel,
  ListReviewDecisionsResponse,
  ListReviewLabelsResponse,
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
// Every label of the ontology, a page (100) at a time: what the review queue
// measures precision from, and leaves out of its draws.
export const labelQueries = {
  list: (ws: string, o: string) => ({
    queryKey: [ws, "ontology", o, "review-labels"],
    staleTime: 30_000,
    queryFn: async () => {
      const labels: FoundryReviewLabel[] = [];
      const seen = new Set<string>();
      let token = "";
      do {
        const page = await api<ListReviewLabelsResponse>(
          `${base(ws, o)}/review-labels${token ? `?pageToken=${enc(token)}` : ""}`,
        );
        labels.push(...(page.labels || []));
        token = page.nextPageToken || "";
        if (seen.has(token)) throw new Error("Labels repeated a page.");
        seen.add(token);
      } while (token);
      return labels;
    },
  }),
};
/** Keeps a person's word on a claim the review queue drew (or re-checks). */
export const settleLabel = async (
  ws: string,
  o: string,
  body: SettleReviewLabelRequest,
) => {
  return api<ReviewLabelResponse>(`${base(ws, o)}/review-labels`, body);
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
        : body.decision.reject
          ? "reject"
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
