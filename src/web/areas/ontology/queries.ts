import type {
  AskQuestionRequest,
  ReviseOntologyVersionRequest,
  CreateLoadRequest,
  CreateOntologyRequest,
  FoundryLoad,
  GenerateOntologyRequest,
  GraphJob,
  ListLoadsResponse,
  ListQuestionEventsResponse,
  ListQuestionsResponse,
  ListOntologiesResponse,
  ListOntologyGenerationsResponse,
  ListOntologyVersionsResponse,
  LoadResponse,
  OntologyGeneration,
  OntologyGenerationResponse,
  OntologyResponse,
  OntologyVersionResponse,
  QuestionResponse,
  SetCurrentOntologyVersionRequest,
  StopLoadEnrichmentResponse,
} from "../../../shared/wfe";
import { jobSeen, jobStarted } from "../../analytics";
import { api } from "../../api";

// The Ontology area's WFE calls, as TanStack Query options keyed by
// workspace, then ontology: [ws, "ontology", ontologyId, ...]. Every route
// here is on the /papi allowlist's Ontology section; the connectors a
// generation drafts from are the Data area's query.

const enc = encodeURIComponent;
const base = (ws: string) => `/v1/workspaces/${enc(ws)}/foundry/ontologies`;
const at = (ws: string, o: string) => `${base(ws)}/${enc(o)}`;
// A generation or a load is polled this often while it is live.
export const POLL_MS = 1500;

export const isLive = (g?: OntologyGeneration | null) =>
  g?.status === "ONTOLOGY_GENERATION_STATUS_QUEUED" ||
  g?.status === "ONTOLOGY_GENERATION_STATUS_RUNNING";

/** A queued generation that has run before: an attempt failed and Helix retries it. */
export const isRetrying = (g?: OntologyGeneration | null) =>
  g?.status === "ONTOLOGY_GENERATION_STATUS_QUEUED" && (g.attempts || 0) > 0;

/** A job that is queued or running. */
export const isLiveJob = (j?: GraphJob | null) =>
  j?.status === "queued" || j?.status === "running";

/** A load something is still happening to: written, or deleted. */
export const isBusyLoad = (l?: FoundryLoad | null) =>
  l?.status === "writing" ||
  l?.status === "deleting" ||
  !!l?.jobs?.some(isLiveJob);

/**
 * What one read of the loads list says of an ontology's jobs, to tell when
 * one ends: the jobs queued or running, the jobs finished, and the loads.
 */
export type JobsSeen = { live: string[]; done: string[]; loads: string[] };
export const jobsSeen = (loads: readonly FoundryLoad[] = []): JobsSeen => ({
  live: loads.flatMap((l) => (l.jobs || []).filter(isLiveJob).map((j) => j.id)),
  done: loads.flatMap((l) =>
    (l.jobs || []).filter((j) => !isLiveJob(j)).map((j) => j.id),
  ),
  loads: loads.map((l) => l.id),
});

/**
 * Whether a job ended between two reads of the loads list: one live before
 * is finished or gone, one shows finished that didn't before (it ran between
 * two reads), or a load left the list (its delete ended). Jobs chain (a
 * retention delete or an owed extract starts as one ends), so something is
 * live throughout a chain; every end in it counts.
 */
export const jobEnded = (before: JobsSeen, now: JobsSeen) =>
  before.live.some((id) => !now.live.includes(id)) ||
  now.done.some((id) => !before.done.includes(id)) ||
  before.loads.some((id) => !now.loads.includes(id));

/** Every page of a list route (they page by pageToken), up to `max` items. */
async function all<T>(
  path: string,
  pick: (page: any) => T[] | undefined,
  max = 500,
): Promise<T[]> {
  const items: T[] = [];
  let token = "";
  do {
    const page = await api(
      path +
        (token
          ? `${path.includes("?") ? "&" : "?"}pageToken=${enc(token)}`
          : ""),
    );
    items.push(...(pick(page) || []));
    token = page?.nextPageToken || "";
  } while (token && items.length < max);
  return items;
}

export const ontologyQueries = {
  // The workspace's ontologies, newest first.
  list: (ws: string) => ({
    queryKey: [ws, "ontologies"],
    queryFn: async (): Promise<ListOntologiesResponse> => ({
      ontologies: await all(base(ws), (p) => p.ontologies),
    }),
  }),
  // One ontology: its database, current version, current load and live
  // job. Polled while a job is live, so pages see it end.
  ontology: (ws: string, o: string) => ({
    queryKey: [ws, "ontology", o],
    queryFn: () => api<OntologyResponse>(at(ws, o)),
    refetchInterval: (q: { state: { data?: OntologyResponse } }) =>
      q.state.data?.ontology.activeJob ? POLL_MS : false,
  }),
  versions: (ws: string, o: string) => ({
    queryKey: [ws, "ontology", o, "versions"],
    queryFn: () =>
      api<ListOntologyVersionsResponse>(`${at(ws, o)}/versions?limit=100`),
  }),
  version: (ws: string, o: string, id: string) => ({
    queryKey: [ws, "ontology", o, "version", id],
    queryFn: () =>
      api<OntologyVersionResponse>(`${at(ws, o)}/versions/${enc(id)}`),
    // Versions are immutable; only isCurrent changes, through set-current.
    staleTime: 60_000,
  }),
  generations: (ws: string, o: string) => ({
    queryKey: [ws, "ontology", o, "generations"],
    queryFn: async () => {
      const r = await api<ListOntologyGenerationsResponse>(
        `${at(ws, o)}/generations?limit=100`,
      );
      jobSeen("generation", r.generations || []);
      return r;
    },
    refetchInterval: (q: {
      state: { data?: ListOntologyGenerationsResponse };
    }) => (q.state.data?.generations?.some(isLive) ? POLL_MS : false),
  }),
  generation: (ws: string, o: string, id: string) => ({
    queryKey: [ws, "ontology", o, "generation", id],
    queryFn: async () => {
      const r = await api<OntologyGenerationResponse>(
        `${at(ws, o)}/generations/${enc(id)}`,
      );
      jobSeen("generation", [r.generation]);
      return r;
    },
    // Only while it is live: a refused read (gone, or no access) stops.
    refetchInterval: (q: { state: { data?: OntologyGenerationResponse } }) =>
      isLive(q.state.data?.generation) ? POLL_MS : false,
  }),
  // Every load not deleted yet, newest first, with its jobs: retention keeps
  // a handful, so there is one page. Polled while one is written or deleted.
  loads: (ws: string, o: string) => ({
    queryKey: [ws, "ontology", o, "loads"],
    queryFn: async () => {
      const r = await api<ListLoadsResponse>(`${at(ws, o)}/loads`);
      jobSeen("load", r.loads || []);
      return r;
    },
    refetchInterval: (q: { state: { data?: ListLoadsResponse } }) =>
      q.state.data?.loads?.some(isBusyLoad) ? POLL_MS : false,
  }),
  // One load, polled while something happens to it. Gone once deleted.
  load: (ws: string, o: string, id: string) => ({
    queryKey: [ws, "ontology", o, "loads", id],
    queryFn: async () => {
      const r = await api<LoadResponse>(`${at(ws, o)}/loads/${enc(id)}`);
      jobSeen("load", [r.load]);
      return r;
    },
    refetchInterval: (q: { state: { data?: LoadResponse } }) =>
      isBusyLoad(q.state.data?.load) ? POLL_MS : false,
    retry: false,
  }),
};

/** Creates an ontology on a database; the same client token is the same ontology. */
export const createOntology = (ws: string, body: CreateOntologyRequest) =>
  api<OntologyResponse>(base(ws), body);

/**
 * Deletes Foundry's rows for an ontology and, in the background, its loads
 * from its database; the database's other data stays.
 */
export const deleteOntology = (ws: string, o: string) =>
  api<OntologyResponse>(at(ws, o), undefined, "DELETE");

export const generateOntology = async (
  ws: string,
  o: string,
  body: GenerateOntologyRequest,
) => {
  const r = await api<OntologyGenerationResponse>(
    `${at(ws, o)}:generate`,
    body,
  );
  jobStarted("generation", r.generation?.id, body.sample ? "sample" : "ai");
  return r;
};

export const setCurrentVersion = (
  ws: string,
  o: string,
  body: SetCurrentOntologyVersionRequest,
) => api<OntologyVersionResponse>(`${at(ws, o)}:set-current`, body);

/** Starts a load of `versionId`; the same client token is the same load. */
export const createLoad = async (
  ws: string,
  o: string,
  body: CreateLoadRequest,
) => {
  const r = await api<LoadResponse>(`${at(ws, o)}/loads`, body);
  jobStarted("load", r.load.id);
  return r;
};

/**
 * Makes a ready or partial load the one reads go to; the documents it hasn't
 * read are read into it.
 */
export const setCurrentLoad = (ws: string, o: string, id: string) =>
  api<LoadResponse>(`${at(ws, o)}/loads/${enc(id)}:set-current`, {});

/**
 * Stops a load's People enrichment lookups (owners and admins): a queued
 * step ends at once, a running one after its batch, and the load goes on
 * with what was found.
 */
export const stopLoadEnrichment = (ws: string, o: string, id: string) =>
  api<StopLoadEnrichmentResponse>(
    `${at(ws, o)}/loads/${enc(id)}:stop-enrichment`,
    {},
  );

/**
 * Deletes a load that is neither current nor being written, with the edges
 * anyone added to its nodes; a delete_failed one is tried again.
 */
export const deleteLoad = (ws: string, o: string, id: string) =>
  api<LoadResponse>(`${at(ws, o)}/loads/${enc(id)}`, undefined, "DELETE");

/** Poll the response with ontologyQueries.generation, as for agent drafts. */
export const reviseOntologyVersion = (
  ws: string,
  o: string,
  versionId: string,
  body: ReviseOntologyVersionRequest,
) =>
  api<OntologyGenerationResponse>(
    `${at(ws, o)}/versions/${enc(versionId)}:revise`,
    body,
  );

// ---------------------------------------------------------------- questions

const questions = (ws: string, o: string) => `${at(ws, o)}/questions`;
const isLiveQuestion = (q?: { status?: string } | null) =>
  q?.status === "queued" || q?.status === "running";

export const questionQueries = {
  // The newest questions (every thread, or one), with the workspace's
  // questions budget. Polled while one is live.
  list: (ws: string, o: string, thread = "") => ({
    queryKey: [ws, "ontology", o, "questions", thread],
    queryFn: async () => {
      const r = await api<ListQuestionsResponse>(
        questions(ws, o) + (thread ? `?threadId=${enc(thread)}` : ""),
      );
      jobSeen("question", r.questions || []);
      return r;
    },
    refetchInterval: (q: { state: { data?: ListQuestionsResponse } }) =>
      q.state.data?.questions?.some(isLiveQuestion) ? POLL_MS : false,
  }),
  // One question, polled while it is live.
  question: (ws: string, o: string, id: string) => ({
    queryKey: [ws, "ontology", o, "questions", "one", id],
    queryFn: async () => {
      const r = await api<QuestionResponse>(`${questions(ws, o)}/${enc(id)}`);
      jobSeen("question", [r.question]);
      return r;
    },
    refetchInterval: (q: { state: { data?: QuestionResponse } }) =>
      isLiveQuestion(q.state.data?.question) ? POLL_MS : false,
  }),
  // An ended question as GET of one gives it: its result with the queries'
  // outputs (up to 512 KiB), which lists leave out. It never changes once
  // ended, so it is read once: its key is outside the ontology's, which
  // asking and the end of a job invalidate.
  answer: (ws: string, o: string, id: string) => ({
    queryKey: [ws, "ontology-answers", o, id],
    queryFn: () => api<QuestionResponse>(`${questions(ws, o)}/${enc(id)}`),
    staleTime: Infinity,
  }),
};

/**
 * Up to 200 of a question's events after `afterSeq`, in seq order, with its
 * status and last seq (see reduceEvents).
 */
export const questionEvents = (
  ws: string,
  o: string,
  id: string,
  afterSeq: number,
) =>
  api<ListQuestionEventsResponse>(
    `${questions(ws, o)}/${enc(id)}/events?afterSeq=${afterSeq}`,
  );

/** Asks about a load; the same client token is the same question. */
export const askQuestion = async (
  ws: string,
  o: string,
  body: AskQuestionRequest,
) => {
  const r = await api<QuestionResponse>(questions(ws, o), body);
  jobStarted("question", r.question.id);
  return r;
};

/** Stops a queued or running question; a running one is charged what it reserved. */
export const cancelQuestion = (ws: string, o: string, id: string) =>
  api<QuestionResponse>(`${questions(ws, o)}/${enc(id)}:cancel`, {});
