import {
  infiniteQueryOptions,
  useQuery,
  type QueryClient,
} from "@tanstack/react-query";
import type {
  DrawFoundryReviewSpotChecksRequest,
  FoundryReviewDecision,
  FoundryReviewSpotChecksResponse,
  FoundryReviewSummary,
  GetFoundryReviewSummaryResponse,
  ListFoundryReviewCardsResponse,
  ListReviewDecisionsResponse,
} from "../../../shared/wfe";
import { api } from "../../api";
import { useWorkspace } from "../../ui";
import { APPLY_POLL_MS, waiting } from "./model";

// The review cards index's reads (backend/reviewcards, served by
// api/review_cards.go): each current load's open count (the summary), the
// walk's cards a page at a time, its spot checks, and the walk's own
// decisions by id. Counting and ordering are the index's, so no page reads
// a load's reviews to count them.

const enc = encodeURIComponent;
const base = (ws: string, o: string) =>
  `/v1/workspaces/${enc(ws)}/foundry/ontologies/${enc(o)}`;

/** The kinds of card the walk filters by. */
export type CardKind = "name" | "claim" | "value";
const KIND = {
  name: "FOUNDRY_REVIEW_CARD_KIND_NAME",
  claim: "FOUNDRY_REVIEW_CARD_KIND_CLAIM",
  value: "FOUNDRY_REVIEW_CARD_KIND_VALUE",
} as const;

/** Which cards a list reads: a lane, and the index's filters (none: every card). */
export type CardFilter = {
  lane: "decide" | "fyi";
  kinds: readonly CardKind[];
  type?: string;
  documentId?: string;
};

/**
 * A list's query string: its lane, each kind (none: every kind), the type
 * and document, and the page.
 *
 * @example
 * cardsQuery({ lane: "decide", kinds: ["name"] }, ""); // "lane=FOUNDRY_REVIEW_LANE_DECIDE&kind=FOUNDRY_REVIEW_CARD_KIND_NAME"
 */
export function cardsQuery(f: CardFilter, pageToken: string): string {
  const q = new URLSearchParams({
    lane:
      f.lane === "fyi"
        ? "FOUNDRY_REVIEW_LANE_FYI"
        : "FOUNDRY_REVIEW_LANE_DECIDE",
  });
  for (const k of f.kinds) q.append("kind", KIND[k]);
  if (f.type) q.set("type", f.type);
  if (f.documentId) q.set("documentId", f.documentId);
  if (pageToken) q.set("pageToken", pageToken);
  return q.toString();
}

const summaryKey = (ws: string) => [ws, "review-summary"];

/** How many pages the Suggested and Needs you filters read before they stop. */
export const ADVICE_PAGES = 3;

export const cardQueries = {
  // What each current load asks a person to decide, counted by the index.
  // Polled by the badge, the nudge and the job dock, so read for a minute;
  // a job's end reads it again (../ontology/scope.tsx, the job dock), and
  // the walk takes off the cards it closes as it closes them (adjustSummary),
  // so a settle doesn't read it.
  summary: (ws: string) => ({
    queryKey: summaryKey(ws),
    staleTime: 60_000,
    queryFn: () =>
      api<GetFoundryReviewSummaryResponse>(
        `/v1/workspaces/${enc(ws)}/foundry/review-summary`,
      ),
  }),
  // The walk's cards of one load under a filter, 50 a page in its order.
  // Under the ontology, so a job's end reads the pages again.
  pages: (ws: string, o: string, loadId: string, f: CardFilter) =>
    infiniteQueryOptions({
      queryKey: [ws, "ontology", o, "review-cards", loadId, f],
      initialPageParam: "",
      staleTime: 30_000,
      queryFn: ({ pageParam }) =>
        api<ListFoundryReviewCardsResponse>(
          `${base(ws, o)}/review-cards?${cardsQuery(f, pageParam)}`,
        ),
      getNextPageParam: (last) => last.nextPageToken || undefined,
    }),
  // The current load's spot checks, as drawn; drawSpotChecks draws them.
  spot: (ws: string, o: string, loadId: string) => ({
    queryKey: [ws, "ontology", o, "review-spot-checks", loadId],
    staleTime: 30_000,
    queryFn: () =>
      api<FoundryReviewSpotChecksResponse>(`${base(ws, o)}/review-spot-checks`),
  }),
  // The walk's own decisions, by id (100 a read), with what became of each
  // in the current load: read again every two seconds while one waits for
  // its job. One missing was removed, or replaced by another choice.
  decided: (ws: string, o: string, ids: readonly string[]) => ({
    queryKey: [ws, "ontology", o, "review-decided", ids],
    enabled: ids.length > 0,
    // A new decision is a new read: what the last said stands meanwhile.
    placeholderData: (previous?: FoundryReviewDecision[]) => previous,
    refetchInterval: (q: { state: { data?: FoundryReviewDecision[] } }) =>
      waiting(q.state.data ?? []).length ? APPLY_POLL_MS : false,
    queryFn: async () => {
      const read = await Promise.all(
        Array.from({ length: Math.ceil(ids.length / 100) }, (_, i) =>
          api<ListReviewDecisionsResponse>(
            `${base(ws, o)}/review-decisions?includeWithdraws=true&${ids
              .slice(i * 100, i * 100 + 100)
              .map((id) => `ids=${enc(id)}`)
              .join("&")}`,
          ),
        ),
      );
      return read.flatMap((r) => r.decisions ?? []);
    },
  }),
};

/**
 * Tops the current load's spot checks up to five undone, the same for
 * everyone: sent once when the walk's finish opens. Refused
 * (FAILED_PRECONDITION) when `loadId` is no longer current.
 */
export const drawSpotChecks = (ws: string, o: string, loadId: string) =>
  api<FoundryReviewSpotChecksResponse>(
    `${base(ws, o)}/review-spot-checks:draw`,
    { loadId } satisfies DrawFoundryReviewSpotChecksRequest,
  );

/**
 * The summary of the workspace's current loads, for owners, admins and
 * members; a restricted member reads none of the cards, so it isn't read.
 */
export function useReviewSummary(): ReadonlyMap<string, FoundryReviewSummary> {
  const { id: ws, grantee } = useWorkspace();
  const read = useQuery({ ...cardQueries.summary(ws), enabled: !grantee });
  return new Map((read.data?.ontologies ?? []).map((s) => [s.ontologyId, s]));
}

/**
 * Takes `n` cards a walk decided (or, negative, took back) off the open
 * count of ontology `o`'s load `loadId`, until the summary is read again.
 */
export function adjustSummary(
  qc: QueryClient,
  ws: string,
  o: string,
  loadId: string,
  n: number,
) {
  qc.setQueryData<GetFoundryReviewSummaryResponse>(
    summaryKey(ws),
    (s) =>
      s && {
        ...s,
        ontologies: s.ontologies?.map((x) =>
          x.ontologyId === o && x.loadId === loadId
            ? { ...x, open: Math.max(0, (x.open ?? 0) - n) }
            : x,
        ),
      },
  );
}

/** Reads the summary again: a job ended. */
export const refreshSummary = (qc: QueryClient, ws: string) =>
  qc.invalidateQueries({ queryKey: summaryKey(ws) });
