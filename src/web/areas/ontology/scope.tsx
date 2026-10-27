import { createContext, useContext, useEffect, useRef } from "react";
import { Outlet, useNavigate, useParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Shapes } from "lucide-react";
import type {
  FoundryOntology,
  ListLoadsResponse,
  ListOntologiesResponse,
} from "../../../shared/wfe";
import { ApiError } from "../../api";
import {
  Button,
  ButtonLink,
  EmptyState,
  FilterSelect,
  Page,
  SkeletonText,
  cx,
} from "../../kit";
import { readStorage, writeStorage } from "../../kit/util";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { ontologyDatabase } from "../explore/reads";
import { TenantReadsProvider } from "../explore/tenant-reads";
import { ontologySetup, type OntologySetup } from "./model";
import {
  POLL_MS,
  isBusyLoad,
  jobEnded,
  jobsSeen,
  ontologyQueries,
  questionQueries,
  type JobsSeen,
} from "./queries";

// /ontology/:ontologyId/*, /explore/:ontologyId/* and /ask/:ontologyId:
// every page of one ontology runs inside its scope, which loads the
// ontology once (its database, current version, current load and live job)
// and hands it down. Pages read it with useOntology(), and its database
// through one shared limiter (useTenantReads()). When a job ends,
// everything read under the ontology is read again: an extract may have
// written into a load that is no longer current. The scope also remembers
// the ontology last opened in this browser, which /explore and /ask open,
// and, under them, that the ontology was explored, for where its setup is
// (useOntologySetup, for the overview).

export type OntologyScopeValue = {
  // The ontology's id, as in the URL.
  id: string;
  ontology: FoundryOntology;
};
const OntologyContext = createContext<OntologyScopeValue | null>(null);

/** The ontology the page is under. Only valid inside its context. */
export function useOntology(): OntologyScopeValue {
  const scope = useContext(OntologyContext);
  if (!scope) throw new Error("useOntology outside an ontology's pages");
  return scope;
}

/** The ontology's display name, or its id while it has none. */
export const ontologyName = (o: FoundryOntology) => o.name || o.id;

// Per workspace: ontology ids are the workspace's own.
const lastKey = (ws: string) => "foundry.ontology.last.v1:" + ws;
/** The id of the ontology last opened in this browser, or "". */
export const lastOntology = (ws: string) => readStorage(lastKey(ws));
const exploredKey = (ws: string, o: string) =>
  `foundry.ontology.explored.v1:${ws}:${o}`;
/**
 * Whether this browser opened the Explorer or Ask on the ontology while it
 * had a load: its setup ticks Explore or ask from it.
 */
export const wasExplored = (ws: string, o: string) =>
  readStorage(exploredKey(ws, o)) === "1";

/**
 * Where `o`'s setup is (ontologySetup), for its overview: its versions and
 * generations only while no version is current, and its questions only
 * while it has a load this browser hasn't explored. Undefined while the
 * lists it reads are first read. A list that failed reads as empty, and setup holds
 * still while it is read again: what setup shows reads the same lists, so a
 * setup that went back to loading would unmount them, and mounting them
 * again would read a failed list again, without end.
 */
export function useOntologySetup(
  ws: string,
  o: FoundryOntology,
): OntologySetup | undefined {
  const oid = o.id,
    drafting = !o.currentVersionId;
  const versions = useQuery({
      ...ontologyQueries.versions(ws, oid),
      enabled: drafting,
    }),
    generations = useQuery({
      ...ontologyQueries.generations(ws, oid),
      enabled: drafting,
    });
  const opened = wasExplored(ws, oid),
    asking = !!o.currentLoad && !opened;
  const questions = useQuery({
    ...questionQueries.list(ws, oid),
    enabled: asking,
  });
  // Only the lists this reads: a disabled query still shows its key's state,
  // which other pages fetch.
  if (
    [drafting && versions, drafting && generations, asking && questions].some(
      (q) => q && q.isLoading && !q.errorUpdateCount,
    )
  )
    return undefined;
  return ontologySetup({
    ontology: o,
    versions: versions.data?.versions || [],
    generations: generations.data?.generations || [],
    explored: opened || !!questions.data?.questions?.length,
  });
}

/** `explores`: the Explorer's and Ask's scope, which tick Explore or Ask. */
export function OntologyScope({ explores = false }: { explores?: boolean }) {
  const { id: ws } = useWorkspace();
  const { ontologyId = "" } = useParams();
  const query = useQuery(ontologyQueries.ontology(ws, ontologyId));
  // The loads list, with every load's jobs, is polled while the ontology has
  // a live job or a load is being written or deleted. The end of each job,
  // even one the next job of a chain follows at once, refreshes every read.
  const activeJob = !!query.data?.ontology.activeJob;
  const loads = useQuery({
    ...ontologyQueries.loads(ws, ontologyId),
    enabled: !!query.data,
    refetchInterval: (q: { state: { data?: ListLoadsResponse } }) =>
      activeJob || q.state.data?.loads?.some(isBusyLoad) ? POLL_MS : false,
  });
  const qc = useQueryClient(),
    seen = useRef<{ ontologyId: string; jobs: JobsSeen } | null>(null);
  useEffect(() => {
    if (!loads.data) return;
    const jobs = jobsSeen(loads.data.loads),
      before = seen.current;
    seen.current = { ontologyId, jobs };
    if (before?.ontologyId === ontologyId && jobEnded(before.jobs, jobs))
      void qc.invalidateQueries({ queryKey: [ws, "ontology", ontologyId] });
  }, [loads.data, ontologyId]);
  // The Ontologies panel's list isn't polled: each fresh read of this
  // ontology (its last load, name and database) replaces its row there, so
  // the row's status dot follows a load as it ends.
  const fresh = query.data?.ontology;
  useEffect(() => {
    if (!fresh) return;
    qc.setQueryData<ListOntologiesResponse>(
      ontologyQueries.list(ws).queryKey,
      (old) =>
        old && {
          ...old,
          ontologies: old.ontologies?.map((o) =>
            o.id === fresh.id ? fresh : o,
          ),
        },
    );
  }, [fresh, ws]);
  const found = !!query.data,
    loaded = !!query.data?.ontology.currentLoad;
  useEffect(() => {
    if (found) writeStorage(lastKey(ws), ontologyId);
  }, [found, ws, ontologyId]);
  useEffect(() => {
    if (explores && loaded) writeStorage(exploredKey(ws, ontologyId), "1");
  }, [explores, loaded, ws, ontologyId]);
  if (query.isPending)
    return (
      <Page>
        <SkeletonText lines={6} />
      </Page>
    );
  if (!query.data)
    return (
      <Page width="narrow">
        <EmptyState
          icon={Shapes}
          title={
            query.error instanceof ApiError && query.error.status === 404
              ? "This ontology doesn’t exist"
              : "Couldn’t load the ontology"
          }
          text={
            query.error instanceof ApiError && query.error.status === 404
              ? "It may have been deleted, or it belongs to another workspace."
              : query.error?.message
          }
          action={
            <span className="on-card-actions">
              <ButtonLink to={paths.ontologies()}>Go to Ontologies</ButtonLink>
              {!(
                query.error instanceof ApiError && query.error.status === 404
              ) && <Button onClick={() => void query.refetch()}>Retry</Button>}
            </span>
          }
        />
      </Page>
    );
  return (
    <OntologyContext.Provider
      value={{ id: ontologyId, ontology: query.data.ontology }}
    >
      {/* Keyed: a page switched to another ontology starts afresh. */}
      <TenantReadsProvider
        key={ontologyId}
        database={ontologyDatabase(query.data.ontology)}
      >
        <Outlet />
      </TenantReadsProvider>
    </OntologyContext.Provider>
  );
}

/**
 * The ontology picker of the Explorer's and Ask's headers: the
 * workspace's ontologies by name, `ontologyId` chosen. Choosing one opens
 * `to` of it, which leaves this one's `?load=` and the rest of the page's
 * view behind.
 */
export function OntologyPicker({
  ontologyId: o,
  to,
  className,
}: {
  ontologyId: string;
  to: (ontologyId: string) => string;
  className?: string;
}) {
  const { id: ws } = useWorkspace(),
    navigate = useNavigate();
  const list = useQuery(ontologyQueries.list(ws)).data?.ontologies || [];
  const own = useQuery(ontologyQueries.ontology(ws, o)).data?.ontology;
  // The page's own until the list is read (or if it doesn't name it yet).
  const ontologies = list.some((x) => x.id === o)
    ? list
    : [{ ...own, id: o }, ...list];
  return (
    <FilterSelect
      label="Ontology"
      className={cx("ex-load-picker", className)}
      value={o}
      onChange={(id) => navigate(to(id))}
      options={ontologies.map((x) => ({ value: x.id, label: ontologyName(x) }))}
    />
  );
}
