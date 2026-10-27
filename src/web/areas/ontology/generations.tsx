import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowRight,
  CircleCheck,
  History,
  RotateCcw,
  TriangleAlert,
} from "lucide-react";
import type { OntologyGeneration } from "../../../shared/wfe";
import { ApiError, sentence } from "../../api";
import {
  ButtonLink,
  EmptyState,
  KeyValue,
  Page,
  PageHeader,
  Panel,
  SkeletonText,
  cx,
  formatDate,
  formatRelative,
  useNow,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { connectorsQuery } from "../data/data-api";
import { documentSetQueries } from "../documents/documents-api";
import { SettingsLinked } from "../workspace/ai-gate";
import { generationPicks, generationTitle } from "./model";
import { RevisionEvidence } from "./version-edit";
import { DataFeed, type FeedItem, type FeedStep } from "./ontology-scene";
import {
  ArrivalNotes,
  Callout,
  GenerationBadge,
  canManage,
  retryText,
  sourcesText,
} from "./parts";
import { isLive, isRetrying, ontologyQueries } from "./queries";
import { ontologyName, useOntology } from "./scope";
import { useSources } from "./sources";

// /ontology/:ontologyId/generations/:generationId: one request to draft a
// version, polled while it is queued or running, shown as helix-foundry's
// data feed. Generations are listed among the versions (versions.tsx), named
// by their one-line title (generationTitle); the goal shows whole only on a
// generation's own page.

const duration = (g: OntologyGeneration, now: number) => {
  if (!g.startedAt) return "";
  const s = Math.max(
    0,
    Math.round(
      ((g.finishedAt ? Date.parse(g.finishedAt) : now) -
        Date.parse(g.startedAt)) /
        1000,
    ),
  );
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
};

// Live times tick every second; finished ones only need minutes.
const tick = (live: boolean) => (live ? 1000 : 30_000);

export function GenerationPage({ generationId }: { generationId: string }) {
  const { id, role } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    qc = useQueryClient();
  const query = useQuery(ontologyQueries.generation(id, o, generationId));
  const g = query.data?.generation;
  const now = useNow(tick(isLive(g)));
  const sources = useSources(g?.tables),
    // Try again tells API sources from connectors by their kind, and their
    // own sets from other sets.
    connectors = useQuery(connectorsQuery(id)),
    sets = useQuery(documentSetQueries.list(id));
  const result = useQuery({
    ...ontologyQueries.version(id, o, g?.resultVersionId || ""),
    enabled: !!g?.resultVersionId,
  });
  // A generation that just finished changes the versions and the list.
  const wasLive = useRef(false);
  useEffect(() => {
    if (isLive(g)) wasLive.current = true;
    else if (g && wasLive.current) {
      wasLive.current = false;
      void qc.invalidateQueries({ queryKey: [id, "ontology", o] });
    }
  }, [g?.status]);
  const crumbs = [
    { label: "Ontologies", to: paths.ontologies() },
    { label: ontologyName(ontology), to: paths.ontology(o) },
    { label: "Schema versions", to: paths.versions(o) },
  ];
  if (query.isPending)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Generation" />
        <SkeletonText lines={6} />
      </Page>
    );
  if (!g)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Generation unavailable" />
        <EmptyState
          icon={History}
          title={
            query.error instanceof ApiError && query.error.status === 404
              ? "This generation doesn’t exist in this ontology"
              : "Couldn’t load this generation"
          }
          text={query.error?.message}
          action={<ButtonLink to={paths.versions(o)}>All versions</ButtonLink>}
        />
      </Page>
    );
  if (g.revision)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title={generationTitle(g)} />
        {g.revision.parentVersionId && (
          <ButtonLink to={paths.version(o, g.revision.parentVersionId, "edit")}>
            Parent version
          </ButtonLink>
        )}
        <RevisionEvidence generation={g} />
        {g.resultVersionId && (
          <ButtonLink to={paths.version(o, g.resultVersionId)}>
            View new version
          </ButtonLink>
        )}
      </Page>
    );
  const failed = g.status === "ONTOLOGY_GENERATION_STATUS_FAILED",
    succeeded = g.status === "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
    title = generationTitle(g);
  // The tables it drafts from, under their connectors, then its table
  // files, under their sets.
  const status =
    g.status === "ONTOLOGY_GENERATION_STATUS_QUEUED" ? "pending" : "ready";
  const items: FeedItem[] = [
    ...(g.tables || []).map((t): FeedItem => {
      const info = sources.resolve({
        kind: "table",
        connectorId: t.connectorId || "",
        table: t.table || "",
      });
      return {
        id: `${t.connectorId}/${t.table}`,
        name: t.table || "",
        source: info?.kind || "upload",
        group: info?.connector || t.connectorId,
        rows: info?.rows,
        status,
      };
    }),
    ...(g.documents || []).map((d): FeedItem => {
      const info = sources.byAgentId(d.id || "");
      return {
        id: d.id || "",
        name: info?.table || "A deleted file",
        source: "upload",
        group: info?.connector || "Documents",
        rows: info?.rows,
        status,
      };
    }),
  ];
  const step = (label: string, done: boolean, active: boolean): FeedStep => ({
    label,
    status: done ? "done" : active ? "active" : "pending",
  });
  const running = g.status === "ONTOLOGY_GENERATION_STATUS_RUNNING",
    retrying = isRetrying(g),
    waiting = g.status === "ONTOLOGY_GENERATION_STATUS_QUEUED" && !retrying;
  // A running draft reads its tables for its first few seconds, then drafts.
  const drafting =
    running && now - Date.parse(g.startedAt || g.createdAt || "") >= 4000;
  const steps: FeedStep[] = [
    step(
      waiting ? "Waiting for the agent" : "Picked up by the agent",
      running || succeeded || retrying || (failed && !!g.startedAt),
      waiting,
    ),
    step(
      `Reading ${sourcesText(g)}`,
      succeeded || drafting,
      running && !drafting,
    ),
    step("Drafting types, links and mappings", succeeded, drafting),
    step(
      succeeded && result.data?.version
        ? `Stored as version ${result.data.version.version}`
        : "Storing the draft as a new version",
      succeeded,
      false,
    ),
  ];
  return (
    <Page>
      <PageHeader
        className="on-titled"
        breadcrumbs={[...crumbs, { label: title }]}
        eyebrow="Ontology generation"
        title={
          <span className="on-title" title={title}>
            {title}
          </span>
        }
        documentTitle={title}
        meta={
          <>
            <GenerationBadge generation={g} />
            <span title={formatDate(g.createdAt)}>
              Requested {formatRelative(g.createdAt || "", now)}
            </span>
            <span>{sourcesText(g)}</span>
            {(g.attempts || 0) > 1 && <span>{g.attempts} attempts</span>}
          </>
        }
        actions={
          succeeded && g.resultVersionId ? (
            <ButtonLink
              variant="primary"
              iconRight={ArrowRight}
              to={paths.version(o, g.resultVersionId)}
            >
              Review version {result.data?.version?.version ?? ""}
            </ButtonLink>
          ) : failed && canManage(role) && connectors.data && sets.data ? (
            <ButtonLink
              icon={RotateCcw}
              to={paths.generate(o, {
                goal: g.goal,
                // Its connectors, its API sources apart, each in the mode
                // it read, and the other sets it read; none read none.
                ...generationPicks(
                  g,
                  connectors.data.connectors || [],
                  sets.data,
                ),
              })}
            >
              Try again
            </ButtonLink>
          ) : undefined
        }
      />
      <div className="on-stack">
        <ArrivalNotes />
        {retrying && (
          <Callout
            icon={RotateCcw}
            title="An attempt failed. Helix will try again"
            text={retryText(g, now)}
          />
        )}
        {failed && (
          <Callout
            icon={TriangleAlert}
            tone="danger"
            title="The draft failed"
            text={
              g.error ? (
                // The WFE's copy may say where an owner fixes it.
                <SettingsLinked text={sentence(g.error)} />
              ) : (
                "The agent stopped without saying why."
              )
            }
          />
        )}
        {succeeded && (
          <Callout
            icon={CircleCheck}
            tone="muted"
            title={`Drafted version ${result.data?.version?.version ?? ""}`}
            text="It isn’t current yet. Review it, then make it current to use it in this workspace."
          />
        )}
        <DataFeed items={items} steps={steps} />
        {g.goal?.trim() && <Goal goal={g.goal.trim()} />}
        <Panel title="Details">
          <KeyValue
            columns={2}
            items={[
              {
                label: "Started",
                value: g.startedAt && formatDate(g.startedAt),
              },
              {
                label: "Finished",
                value: g.finishedAt && formatDate(g.finishedAt),
              },
              { label: "Took", value: duration(g, now) },
              {
                // The agent drafts afresh: it does not build on this version.
                label: "Current when requested",
                value: g.baseVersionId ? (
                  <Link to={paths.version(o, g.baseVersionId)}>
                    See that version
                  </Link>
                ) : (
                  "No version was current"
                ),
              },
              { label: "Model", value: g.model, mono: true },
              { label: "Generation ID", value: g.id, mono: true },
            ]}
          />
        </Panel>
      </div>
    </Page>
  );
}

/** The goal as it was written, four lines of it until the rest is asked for. */
function Goal({ goal }: { goal: string }) {
  const [open, setOpen] = useState(false),
    [long, setLong] = useState(false),
    text = useRef<HTMLParagraphElement>(null);
  // Whether the clamp hides any of it, at the panel's width.
  useLayoutEffect(() => {
    const el = text.current;
    if (!el || open) return;
    const measure = () => setLong(el.scrollHeight > el.clientHeight + 1);
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [goal, open]);
  return (
    <Panel title="Goal">
      <p
        ref={text}
        id="on-goal-text"
        className={cx("on-goal-text", !open && "is-clamped")}
      >
        {goal}
      </p>
      {long && (
        <button
          type="button"
          className="on-link on-more"
          aria-expanded={open}
          aria-controls="on-goal-text"
          onClick={() => setOpen(!open)}
        >
          {open ? "Show less" : "Show more"}
        </button>
      )}
    </Panel>
  );
}
