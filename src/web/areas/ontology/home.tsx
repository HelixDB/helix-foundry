import { useEffect, useMemo, useState } from "react";
import { Link, useLocation, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Database, FileText, Shapes } from "lucide-react";
import {
  Button,
  ButtonLink,
  EmptyState,
  Page,
  PageHeader,
  SkeletonText,
  StatusBadge,
  Tag,
  formatDate,
  formatRelative,
  plural,
} from "../../kit";
import { paths } from "../../paths";
import { RefreshFailed, useWorkspace } from "../../ui";
import { liveRead } from "../documents/model";
import { projectsQuery } from "./databases-api";
import { formatLabel, versionFormat } from "./format";
import { GraphView } from "./graph";
import {
  agentGraph,
  foundryGraph,
  readAgentBundle,
  readFoundry,
} from "./model";
import {
  ArrivalNotes,
  GenerateButton,
  LiveGeneration,
  lastLoadBadge,
  sourceLabel,
} from "./parts";
import { LiveLoad, LoadButton, LoadsSection, NotLoaded } from "./loads";
import { DeleteOntology } from "./ontologies";
import { ontologyQueries } from "./queries";
import { ontologyName, useOntology, useOntologySetup } from "./scope";
import { ReadyToReview, SetupFlow } from "./setup";
import { useSources } from "./sources";
import { VersionsSection } from "./versions";

// /ontology/:ontologyId: one ontology's current version drawn as a graph, as
// helix-foundry's Ontology page draws the published one, with what is
// generating or loading, whether its database holds the current version,
// and any newer version waiting for review above it. Until the ontology is
// set up (useOntologySetup), its setup's steps come
// first, in one card open where it is, with Set up with your agent in its
// head (./setup.tsx); without a current version they are the page. Its header says which database (and project) it is bound
// to, how its last load went and when it was created, and links to the
// documents it reads (the workspace's, in its version's sets). Below the
// graph, its schema versions (#versions, ./versions.tsx) and data loads
// (#loads, ./loads.tsx), each once it has one; ?new-load=1 opens the Load
// dialog.

export function OntologyHome() {
  const { id } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    { hash } = useLocation(),
    [params, setParams] = useSearchParams();
  const setup = useOntologySetup(id, ontology);
  // `?new-load=1` (Load data where nothing is loaded yet) opens the Load
  // dialog once: the URL drops it, so Back and a refresh don't open it again.
  const [newLoad] = useState(() => params.has("new-load"));
  useEffect(() => {
    if (!params.has("new-load")) return;
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        next.delete("new-load");
        return next;
      },
      { replace: true },
    );
  }, [params, setParams]);
  const currentId = ontology.currentVersionId || "";
  const current = useQuery({
      ...ontologyQueries.version(id, o, currentId),
      enabled: !!currentId,
    }),
    versions = useQuery(ontologyQueries.versions(id, o)),
    // LiveLoad's, for the badge: a load looking people up says so.
    loads = useQuery(ontologyQueries.loads(id, o));
  const v = currentId ? current.data?.version : null;
  const generation = useQuery({
    ...ontologyQueries.generation(id, o, v?.generationId || ""),
    enabled: !!v?.generationId,
  });
  const sources = useSources(generation.data?.generation?.tables);
  const graph = useMemo(() => {
    if (!v) return undefined;
    return versionFormat(v) === "agent_bundle"
      ? agentGraph(readAgentBundle(v.definition))
      : foundryGraph(readFoundry(v.definition));
  }, [v]);
  const list = versions.data?.versions || [];
  // #versions and #loads name sections that show once their lists and the
  // graph above them are in: scroll to them then.
  const settled =
    !versions.isPending &&
    !loads.isPending &&
    !(currentId && current.isPending);
  useEffect(() => {
    if (!hash || !settled) return;
    document
      .getElementById(decodeURIComponent(hash.slice(1)))
      ?.scrollIntoView({ block: "start" });
  }, [hash, settled]);
  // The newest version, when it is newer than the current one: a draft to review.
  const newest = list[0];
  const review =
    newest && !newest.isCurrent && (newest.version || 0) > (v?.version || 0)
      ? newest
      : undefined;
  const project = useQuery({
      ...projectsQuery(id),
      enabled: !!ontology.projectId,
    }).data?.find((p) => p.id === ontology.projectId),
    load = lastLoadBadge(ontology, liveRead(loads.data?.loads)),
    database =
      ontology.databaseName ||
      ontology.tenantId ||
      "Database not visible to you";
  return (
    <Page width={currentId ? "wide" : "narrow"}>
      <PageHeader
        breadcrumbs={[{ label: "Ontologies", to: paths.ontologies() }]}
        title={ontologyName(ontology)}
        subtitle={`The types, links and rules drawn from your data, loaded into ${ontology.databaseName || "its database"}.`}
        meta={
          <>
            <span className="on-db-cell">
              <Database size={14} aria-hidden />
              <span>
                {database}
                {project && (
                  <small> · {project.displayName || project.slug}</small>
                )}
              </span>
            </span>
            <StatusBadge status={load.status} label={load.label} />
            {ontology.lastLoad?.finishedAt && (
              <span title={formatDate(ontology.lastLoad.finishedAt)}>
                Last load {formatRelative(ontology.lastLoad.finishedAt)}
              </span>
            )}
            {ontology.createdAt && (
              <span title={formatDate(ontology.createdAt)}>
                Created {formatRelative(ontology.createdAt)}
              </span>
            )}
          </>
        }
        actions={
          <>
            <ButtonLink icon={FileText} to={paths.documents(o)}>
              {plural(ontology.counts?.documents || 0, "document")}
            </ButtonLink>
            {v && (
              <LoadButton
                version={{ ...v, isCurrent: true }}
                variant="secondary"
                opened={newLoad}
              />
            )}
            {setup && setup.step !== "draft" && <GenerateButton />}
            <DeleteOntology />
          </>
        }
      />
      <div className="on-stack">
        <ArrivalNotes />
        {/* What the setup's open step shows, it shows there instead. */}
        {setup?.step !== "draft" && <LiveGeneration />}
        {setup?.step !== "load" && <LiveLoad />}
        {v && setup?.step !== "load" && <NotLoaded version={v} />}
        {review && setup?.step !== "current" && (
          <ReadyToReview versionId={review.id} current={v} />
        )}
        {setup && setup.step !== "done" && (
          <SetupFlow
            setup={setup}
            current={v}
            database={`${database}${project ? ` · ${project.displayName || project.slug}` : ""}`}
          />
        )}
        {current.data && <RefreshFailed query={current} />}
        {currentId && current.isPending ? (
          <SkeletonText lines={6} />
        ) : currentId && !current.data ? (
          <EmptyState
            icon={Shapes}
            title="Couldn’t load the ontology"
            text={current.error?.message}
            action={
              <Button onClick={() => void current.refetch()}>Retry</Button>
            }
          />
        ) : v && graph ? (
          <section aria-labelledby="on-current-title">
            <div className="on-current-head">
              <h2 id="on-current-title">
                <Link to={paths.version(o, v.id)}>Version {v.version}</Link>
              </h2>
              <Tag tone="success">Current</Tag>
              <Tag>{formatLabel[versionFormat(v)]}</Tag>
              <span className="on-muted">
                {sourceLabel(v.source)}{" "}
                {v.createdAt && formatRelative(v.createdAt)}
              </span>
            </div>
            <GraphView
              graph={graph}
              sources={sources}
              versionId={v.id}
              tab={versionFormat(v) === "agent_bundle" ? "types" : "entities"}
            />
          </section>
        ) : (
          !setup && <SkeletonText lines={6} />
        )}
        <VersionsSection />
        <LoadsSection />
      </div>
    </Page>
  );
}
