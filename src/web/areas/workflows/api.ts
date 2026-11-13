import type {
  CreateFoundryWorkflowRequest,
  FoundryPrincipal,
  FoundryWorkflowInsights,
  FoundryWorkflowRun,
  FoundryWorkflowResponse,
  FoundryWorkflowRunResponse,
  ListFoundryWorkflowRunAccessesResponse,
  ListFoundryWorkflowRunsResponse,
  ListFoundryWorkflowsResponse,
} from "../../../shared/wfe";
import { api } from "../../api";

// The Workflows pages' routes: an ontology's workflows, one, its runs, and
// what a run recorded. Runs are started and finished by agents, never here.
const enc = encodeURIComponent;
const base = (ws: string, o: string) =>
  `/v1/workspaces/${enc(ws)}/foundry/ontologies/${enc(o)}/workflows`;
const runs = (ws: string, o: string, w: string) =>
  `${base(ws, o)}/${enc(w)}/runs`;

// How often an open run's pages ask again: the agent records as it goes. A
// workflow's runs are asked again as often while one is open, and every
// RUNS_POLL_MS otherwise, so a run an agent starts shows without a reload.
export const OPEN_RUN_POLL_MS = 3000;
export const RUNS_POLL_MS = 10_000;

const isOpen = (r?: { status?: string }) => r?.status === "open";
// A run is live while it records, or while its assessment runs.
const isLive = (r?: FoundryWorkflowRun) =>
  isOpen(r) ||
  r?.assessment?.status === "queued" ||
  r?.assessment?.status === "running";

export const workflowQueries = {
  list: (ws: string, o: string) => ({
    queryKey: [ws, "ontology", o, "workflows"],
    queryFn: () => api<ListFoundryWorkflowsResponse>(base(ws, o)),
  }),
  // A workflow, with its runs' verdicts: read again as its page opens and
  // as often as its runs, since a run's assessment ends on its own.
  workflow: (ws: string, o: string, w: string) => ({
    queryKey: [ws, "ontology", o, "workflows", w],
    queryFn: () => api<FoundryWorkflowResponse>(`${base(ws, o)}/${enc(w)}`),
    refetchOnMount: "always" as const,
    refetchInterval: RUNS_POLL_MS,
  }),
  // What the window's runs came to (of one workflow, or all of the
  // ontology's; of one who started them, when `startedBy` names them), asked
  // again as often as runs are, so the trends follow new runs.
  insights: (
    ws: string,
    o: string,
    q: { workflowId?: string; days: number; startedBy?: FoundryPrincipal },
  ) => ({
    queryKey: [
      ws,
      "ontology",
      o,
      "workflows",
      "insights",
      q.workflowId || "",
      q.days,
      q.startedBy?.kind || "",
      q.startedBy?.id || "",
    ],
    queryFn: () => {
      const params = new URLSearchParams({ days: String(q.days) });
      if (q.workflowId) params.set("workflowId", q.workflowId);
      if (q.startedBy?.kind && q.startedBy.id) {
        params.set("startedBy.kind", q.startedBy.kind);
        params.set("startedBy.id", q.startedBy.id);
      }
      return api<FoundryWorkflowInsights>(`${base(ws, o)}:insights?${params}`);
    },
    refetchInterval: RUNS_POLL_MS,
    placeholderData: (previous?: FoundryWorkflowInsights) => previous,
  }),
  // A workflow's newest runs, asked again so new ones show.
  runs: (ws: string, o: string, w: string) => ({
    queryKey: [ws, "ontology", o, "workflows", w, "runs"],
    queryFn: () => api<ListFoundryWorkflowRunsResponse>(runs(ws, o, w)),
    refetchInterval: (q: {
      state: { data?: ListFoundryWorkflowRunsResponse };
    }) => (q.state.data?.runs?.some(isLive) ? OPEN_RUN_POLL_MS : RUNS_POLL_MS),
  }),
  run: (ws: string, o: string, w: string, r: string) => ({
    queryKey: [ws, "ontology", o, "workflows", w, "runs", r],
    queryFn: () =>
      api<FoundryWorkflowRunResponse>(`${runs(ws, o, w)}/${enc(r)}`),
    refetchInterval: (q: { state: { data?: FoundryWorkflowRunResponse } }) =>
      isLive(q.state.data?.run) ? OPEN_RUN_POLL_MS : false,
  }),
  // Every call a run recorded, a page of 100 at a time.
  accesses: (ws: string, o: string, w: string, r: string) => ({
    queryKey: [ws, "ontology", o, "workflows", w, "runs", r, "accesses"],
    queryFn: async () => {
      const all: NonNullable<
        ListFoundryWorkflowRunAccessesResponse["accesses"]
      > = [];
      for (;;) {
        const after = all.at(-1)?.seq || 0;
        const page = await api<ListFoundryWorkflowRunAccessesResponse>(
          `${runs(ws, o, w)}/${enc(r)}/accesses?afterSeq=${after}`,
        );
        all.push(...(page.accesses || []));
        if (!page.more || !page.accesses?.length) return all;
      }
    },
  }),
};

export const createWorkflow = (
  ws: string,
  o: string,
  body: CreateFoundryWorkflowRequest,
) => api<FoundryWorkflowResponse>(base(ws, o), body);

export const deleteWorkflow = (ws: string, o: string, w: string) =>
  api<object>(`${base(ws, o)}/${enc(w)}`, undefined, "DELETE");

// Queues a finished run's assessment again (owners and admins).
export const assessRun = (ws: string, o: string, w: string, r: string) =>
  api<FoundryWorkflowRunResponse>(`${runs(ws, o, w)}/${enc(r)}:assess`, {});
