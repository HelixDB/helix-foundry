import "./ontology.css";
import { useEffect, useState, type ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { CircleAlert, CircleCheck, LoaderCircle, X } from "lucide-react";
import type { FoundryLoad, OntologyGeneration } from "../../../shared/wfe";
import { ApiError } from "../../api";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { liveRead } from "../documents/model";
import {
  ProgressBar,
  useGenerationProgress,
  useLoadJobProgress,
} from "./job-progress";
import { generationTitle } from "./model";
import { isBusyLoad, isLive, isLiveJob, ontologyQueries } from "./queries";
import { ontologyName } from "./scope";
import { useSampleOntology } from "./use-sample-ontology";
import { useWatchedJobs } from "./use-watched-jobs";
import {
  followJob,
  shownOn,
  unwatchJob,
  type WatchedJob,
} from "./watched-jobs";

// The job dock: at the sidebar's foot on every page, a card for each
// generation and load this browser started (./watched-jobs.ts) while the
// page open doesn't show it, with its step, bar and time left, then how it
// ended and where to go next. In the sidebar, it never covers the page.
// A first draft that queues its load (loadWhenReady) hands its card to that
// load. Dismissing a card, opening where it points once it ended, or
// seeing it end on a page that shows it stops following it; a job the WFE
// says is gone or not the person's to read is dropped.

export function JobDock() {
  const { id } = useWorkspace(),
    { pathname } = useLocation(),
    jobs = useWatchedJobs(id),
    sample = useSampleOntology()?.id;
  if (!jobs.length) return null;
  return (
    <section className="on-job-dock" aria-label="Jobs in progress">
      {jobs.map((job) => {
        const hidden = shownOn(job, pathname, sample);
        return job.kind === "generation" ? (
          <GenerationCard
            key={job.kind + job.id}
            ws={id}
            job={job}
            hidden={hidden}
          />
        ) : (
          <LoadCard key={job.kind + job.id} ws={id} job={job} hidden={hidden} />
        );
      })}
    </section>
  );
}

type Ending = {
  tone: "done" | "failed";
  title: string;
  go: string;
  label: string;
};

function Card({
  ws,
  job,
  hidden,
  name,
  title,
  ending,
  children,
}: {
  ws: string;
  job: WatchedJob;
  // The page open shows the job: no card, and once it ends there, seen.
  hidden: boolean;
  name: string;
  // While it runs.
  title: string;
  ending: Ending | null;
  children?: ReactNode;
}) {
  const seen = hidden && !!ending;
  useEffect(() => {
    if (seen) unwatchJob(ws, job.kind, job.id);
  }, [seen]);
  if (hidden) return null;
  const stop = () => {
      unwatchJob(ws, job.kind, job.id);
      // The card goes: Feedback, right under the dock, takes its focus.
      document.querySelector<HTMLElement>(".hf-feedback-button")?.focus();
    },
    Icon = !ending
      ? LoaderCircle
      : ending.tone === "done"
        ? CircleCheck
        : CircleAlert;
  return (
    <section
      className={"on-job-card" + (ending ? ` is-${ending.tone}` : "")}
      aria-label={ending ? ending.title : title}
      role={ending?.tone === "failed" ? "alert" : "status"}
    >
      <span className="on-job-card-icon" aria-hidden>
        <Icon size={16} className={ending ? undefined : "hf-spin"} />
      </span>
      <div className="on-job-card-text">
        <strong>{ending ? ending.title : title}</strong>
        <span className="on-job-card-name">{name}</span>
        {!ending && children}
        <Link
          className="on-job-card-link"
          to={ending ? ending.go : jobPath(job)}
          onClick={ending ? stop : undefined}
        >
          {ending ? ending.label : "View progress"}
        </Link>
      </div>
      <button
        type="button"
        className="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon"
        aria-label={`Dismiss ${ending ? ending.title : title}`}
        title="Dismiss"
        onClick={stop}
      >
        <X size={14} aria-hidden />
      </button>
    </section>
  );
}

// A job the WFE says is gone (deleted, with its ontology) or no longer the
// person's to read; any other failure is read again.
const gone = (e: unknown) =>
  e instanceof ApiError && (e.status === 404 || e.status === 403);

const jobPath = (job: WatchedJob) =>
  job.kind === "generation"
    ? paths.generation(job.ontologyId, job.id)
    : paths.load(job.ontologyId, job.id);

/** The ontology's name, from its read or the cached list. */
function useOntologyLabel(ws: string, o: string) {
  const one = useQuery(ontologyQueries.ontology(ws, o));
  return one.data ? ontologyName(one.data.ontology) : "";
}

function GenerationCard({
  ws,
  job,
  hidden,
}: {
  ws: string;
  job: WatchedJob;
  hidden: boolean;
}) {
  const o = job.ontologyId,
    read = useQuery(ontologyQueries.generation(ws, o, job.id)),
    g = read.data?.generation,
    name = useOntologyLabel(ws, o),
    progress = useGenerationProgress(ws, o, isLive(g) ? g : undefined);
  const succeeded = g?.status === "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
    // A first draft's load was queued as it was stored: its card follows
    // that load, once a read of the loads started after the draft was seen
    // done says (until then it hasn't ended, here or on a page showing it).
    handoff = succeeded && !!g?.loadWhenReady,
    [fresh, setFresh] = useState(false);
  const loads = useQuery({
    ...ontologyQueries.loads(ws, o),
    enabled: handoff,
  });
  useEffect(() => {
    // A read already under way may predate the load: this one replaces it.
    if (handoff) void loads.refetch().then(() => setFresh(true));
  }, [handoff]);
  const next = fresh ? liveRead(loads.data?.loads) : undefined;
  useEffect(() => {
    if (next) followJob(ws, job, { kind: "load", id: next.load.id });
  }, [next && next.load.id]);
  useEffect(() => {
    if (gone(read.error)) unwatchJob(ws, job.kind, job.id);
  }, [read.error]);
  if (!g || next) return null;
  return (
    <Card
      ws={ws}
      job={job}
      hidden={hidden}
      name={name}
      title={
        g.revision
          ? "Creating an edited version"
          : `Drafting ${generationTitle(g, "") ? `“${generationTitle(g, "")}”` : "an ontology"}`
      }
      ending={handoff && !fresh ? null : generationEnding(g, o)}
    >
      {progress && (
        <ProgressBar progress={progress} label={`Progress of ${name}`} />
      )}
    </Card>
  );
}

function generationEnding(g: OntologyGeneration, o: string): Ending | null {
  if (isLive(g)) return null;
  if (g.status === "ONTOLOGY_GENERATION_STATUS_SUCCEEDED")
    return {
      tone: "done",
      title: g.revision ? "Edited version ready" : "Draft ready to review",
      go: g.resultVersionId
        ? paths.version(o, g.resultVersionId)
        : paths.generation(o, g.id),
      label: "Review it",
    };
  return {
    tone: "failed",
    title: g.revision ? "The edited version failed" : "The draft failed",
    go: paths.generation(o, g.id),
    label: "See why",
  };
}

function LoadCard({
  ws,
  job,
  hidden,
}: {
  ws: string;
  job: WatchedJob;
  hidden: boolean;
}) {
  const o = job.ontologyId,
    read = useQuery(ontologyQueries.load(ws, o, job.id)),
    l = read.data?.load,
    live = l?.jobs?.find(isLiveJob),
    name = useOntologyLabel(ws, o),
    progress = useLoadJobProgress(live);
  useEffect(() => {
    if (gone(read.error)) unwatchJob(ws, job.kind, job.id);
  }, [read.error]);
  if (!l) return null;
  return (
    <Card
      ws={ws}
      job={job}
      hidden={hidden}
      name={name}
      title={live?.status === "queued" ? "Load queued" : "Loading the database"}
      ending={loadEnding(l, o)}
    >
      {progress && (
        <ProgressBar progress={progress} label={`Progress of ${name}’s load`} />
      )}
    </Card>
  );
}

function loadEnding(l: FoundryLoad, o: string): Ending | null {
  if (isBusyLoad(l)) return null;
  return l.status === "ready" || l.status === "partial"
    ? {
        tone: "done",
        title: l.status === "ready" ? "Data loaded" : "Data loaded, in part",
        go: paths.explore(o),
        label: "Explore it",
      }
    : {
        tone: "failed",
        title: "The load failed",
        go: paths.load(o, l.id),
        label: "See why",
      };
}
