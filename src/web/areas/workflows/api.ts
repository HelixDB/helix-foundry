import type {
  CreateFoundryWorkflowRequest,
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

export const workflowQueries = {
  list: (ws: string, o: string) => ({
    queryKey: [ws, "ontology", o, "workflows"],
    queryFn: () => api<ListFoundryWorkflowsResponse>(base(ws, o)),
  }),
  workflow: (ws: string, o: string, w: string) => ({
    queryKey: [ws, "ontology", o, "workflows", w],
    queryFn: () => api<FoundryWorkflowResponse>(`${base(ws, o)}/${enc(w)}`),
  }),
  // A workflow's newest runs, asked again so new ones show.
  runs: (ws: string, o: string, w: string) => ({
    queryKey: [ws, "ontology", o, "workflows", w, "runs"],
    queryFn: () => api<ListFoundryWorkflowRunsResponse>(runs(ws, o, w)),
    refetchInterval: (q: {
      state: { data?: ListFoundryWorkflowRunsResponse };
    }) => (q.state.data?.runs?.some(isOpen) ? OPEN_RUN_POLL_MS : RUNS_POLL_MS),
  }),
  run: (ws: string, o: string, w: string, r: string) => ({
    queryKey: [ws, "ontology", o, "workflows", w, "runs", r],
    queryFn: () =>
      api<FoundryWorkflowRunResponse>(`${runs(ws, o, w)}/${enc(r)}`),
    refetchInterval: (q: { state: { data?: FoundryWorkflowRunResponse } }) =>
      isOpen(q.state.data?.run) ? OPEN_RUN_POLL_MS : false,
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
