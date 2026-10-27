import { useEffect, useMemo, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { FolderOpen, Shapes } from "lucide-react";
import type { OntologyEdit, OntologyVersion } from "../../../shared/wfe";
import { ApiError } from "../../api";
import {
  ButtonLink,
  EmptyState,
  Page,
  PageHeader,
  SkeletonText,
  StatusBadge,
  TabPanel,
  Tabs,
  Tag,
  formatRelative,
  useTabParam,
  type TabItem,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { documentSetQueries } from "../documents/documents-api";
import { andList } from "../documents/model";
import { formatLabel, versionFormat } from "./format";
import { AgentEdit, type RevisionRun, type TryBase } from "./version-edit";
import { GraphView } from "./graph";
import {
  agentGraph,
  foundryGraph,
  generationTitle,
  readAgentBundle,
  readFoundry,
  scopeNames,
  sourceMatrix,
  type AgentBundle,
  type FoundryView,
  type Graph,
} from "./model";
import { LoadButton } from "./loads";
import { MakeCurrent, sourceLabel } from "./parts";
import { ontologyQueries } from "./queries";
import { ontologyName, useOntology } from "./scope";
import { useSources, type Sources } from "./sources";
import {
  AgentLinks,
  AgentMappings,
  AgentQuestions,
  AgentRules,
  AgentSources,
  AgentSummary,
  AgentToml,
  AgentTypes,
  ParseProblem,
} from "./version-agent";
import {
  FoundryConstraints,
  FoundryEntities,
  FoundryJson,
  FoundryRelationships,
  FoundryRules,
} from "./version-foundry";

// /ontology/:ontologyId/versions/:versionId: one version, read-only, in its
// own format, with the document sets it reads (its scope) by name.
// Both formats open on the graph; the other tabs list what it defines.

type Read =
  | { format: "agent_bundle"; bundle: AgentBundle; graph: Graph }
  | { format: "foundry_v1"; view: FoundryView; graph: Graph };

function read(v: OntologyVersion): Read {
  if (versionFormat(v) === "agent_bundle") {
    const bundle = readAgentBundle(v.definition);
    return { format: "agent_bundle", bundle, graph: agentGraph(bundle) };
  }
  const view = readFoundry(v.definition);
  return { format: "foundry_v1", view, graph: foundryGraph(view) };
}

function tabsOf(r: Read): TabItem[] {
  if (r.format === "foundry_v1")
    return [
      { id: "graph", label: "Graph" },
      { id: "entities", label: "Entities", count: r.view.entities.length },
      {
        id: "relationships",
        label: "Relationships",
        count: r.view.relationships.length,
      },
      { id: "rules", label: "Rules", count: r.view.rules.length },
      {
        id: "constraints",
        label: "Constraints",
        count: r.view.constraints.length,
      },
      { id: "definition", label: "Definition" },
    ];
  const o = r.bundle.ontology.ok ? r.bundle.ontology.value : undefined,
    m = r.bundle.mappings.ok ? r.bundle.mappings.value : undefined,
    routes = r.bundle.routes.ok ? r.bundle.routes.value : undefined;
  return [
    { id: "graph", label: "Graph" },
    { id: "types", label: "Types", count: o?.types.length },
    { id: "links", label: "Links", count: o?.links.length },
    { id: "mappings", label: "Mappings", count: m?.length },
    {
      id: "sources",
      label: "Sources",
      count: o && m && routes && sourceMatrix(o, m, routes).columns.length,
    },
    {
      id: "rules",
      label: "Rules",
      count: o && o.rules.length + o.constraints.length + o.relations.length,
    },
    { id: "questions", label: "Questions", count: r.bundle.questions.length },
    { id: "toml", label: "TOML" },
    { id: "edit", label: "Edit" },
  ];
}

export function VersionPage({ versionId }: { versionId: string }) {
  const { id } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const version = useQuery(ontologyQueries.version(id, o, versionId));
  // The tables its generation drafted from name its agent sources.
  const generationId = version.data?.version?.generationId || "";
  const generation = useQuery({
    ...ontologyQueries.generation(id, o, generationId),
    enabled: !!generationId,
  });
  const sources = useSources(generation.data?.generation?.tables);
  const sets = useQuery(documentSetQueries.list(id));
  // Make current and load (the Edit tab's Save as version) lands here with
  // the load dialog open; the history entry forgets it once read.
  const location = useLocation(),
    navigate = useNavigate();
  const openLoad =
    (location.state as { openLoad?: boolean } | null)?.openLoad === true;
  useEffect(() => {
    if (openLoad)
      navigate(location.pathname + location.search, {
        replace: true,
        state: null,
      });
  }, [openLoad]);
  // Its generation's title, once loaded, when it has one or a goal; a
  // revision's links as its edits.
  const revised = !!generation.data?.generation?.revision;
  const generationName =
    generation.data?.generation && !revised
      ? generationTitle(generation.data.generation, "")
      : "";
  const crumbs = [
    { label: "Ontologies", to: paths.ontologies() },
    { label: ontologyName(ontology), to: paths.ontology(o) },
    { label: "Schema versions", to: paths.versions(o) },
  ];
  const v = version.data?.version;
  const r = useMemo(() => (v ? read(v) : undefined), [v]);
  if (version.isPending)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Version" />
        <SkeletonText lines={6} />
      </Page>
    );
  if (!v || !r)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Version unavailable" />
        <EmptyState
          icon={Shapes}
          title={
            version.error instanceof ApiError && version.error.status === 404
              ? "This version doesn’t exist in this ontology"
              : "Couldn’t load this version"
          }
          text={version.error?.message}
          action={<ButtonLink to={paths.versions(o)}>All versions</ButtonLink>}
        />
      </Page>
    );
  // The WFE's current pointer, as loaded: set-current compares against it.
  const currentId = ontology.currentVersionId || "";
  const isCurrent = currentId === v.id;
  const title = `Version ${v.version ?? "?"}`;
  return (
    <Page>
      <PageHeader
        breadcrumbs={[...crumbs, { label: title }]}
        eyebrow="Ontology version"
        title={title}
        meta={
          <>
            {isCurrent ? (
              <StatusBadge status="succeeded" label="Current" />
            ) : (
              <StatusBadge status="draft" label="Not current" />
            )}
            <Tag>{formatLabel[r.format]}</Tag>
            <span>
              {sourceLabel(v.source)}{" "}
              {v.createdAt && formatRelative(v.createdAt)}
            </span>
            {sets.data && (
              <Tag icon={FolderOpen}>
                <span title="The document sets its loads read">
                  {v.documentSetIds?.length
                    ? `Reads ${andList(scopeNames(v.documentSetIds, sets.data))}`
                    : "Reads no documents"}
                </span>
              </Tag>
            )}
            {v.generationId &&
              (generationName ? (
                <span>
                  For{" "}
                  <Link
                    to={paths.generation(o, v.generationId)}
                    className="on-link"
                    title="See its generation"
                  >
                    {generationName}
                  </Link>
                </span>
              ) : (
                <Link to={paths.generation(o, v.generationId)}>
                  {revised ? "See its edits" : "See its generation"}
                </Link>
              ))}
          </>
        }
        actions={
          <>
            <MakeCurrent version={{ ...v, isCurrent }} currentId={currentId} />
            <LoadButton
              key={v.id}
              version={{ ...v, isCurrent }}
              opened={openLoad}
            />
          </>
        }
        tabs={<Tabs label="Version" items={tabsOf(r)} />}
      />
      <VersionTabs key={v.id} r={r} v={v} sources={sources} />
    </Page>
  );
}

function VersionTabs({
  r,
  v,
  sources,
}: {
  r: Read;
  v: OntologyVersion;
  sources: Sources;
}) {
  // This owner stays mounted when a tab panel is hidden, so the staged
  // edits, a running revision and the base of a staged Enrich people try
  // outlive the Edit tab. The version key resets them when the user opens
  // another immutable version.
  const [edits, setEdits] = useState<OntologyEdit[]>([]);
  const [run, setRun] = useState<RevisionRun | null>(null);
  const [tryBase, setTryBase] = useState<TryBase | null>(null);
  const items = tabsOf(r);
  const [tab, setTab] = useTabParam(items.map((t) => t.id));
  if (r.format === "foundry_v1")
    return (
      <>
        <TabPanel id="graph" value={tab}>
          <GraphView
            graph={r.graph}
            sources={sources}
            versionId={v.id}
            tab="entities"
          />
        </TabPanel>
        <TabPanel id="entities" value={tab}>
          <FoundryEntities view={r.view} sources={sources} />
        </TabPanel>
        <TabPanel id="relationships" value={tab}>
          <FoundryRelationships view={r.view} />
        </TabPanel>
        <TabPanel id="rules" value={tab}>
          <FoundryRules view={r.view} />
        </TabPanel>
        <TabPanel id="constraints" value={tab}>
          <FoundryConstraints view={r.view} />
        </TabPanel>
        <TabPanel id="definition" value={tab}>
          <FoundryJson definition={v.definition || {}} />
        </TabPanel>
      </>
    );
  const { bundle, graph } = r;
  return (
    <>
      <TabPanel id="graph" value={tab}>
        <AgentSummary bundle={bundle} onQuestions={() => setTab("questions")} />
        {bundle.ontology.ok ? (
          <GraphView
            graph={graph}
            sources={sources}
            versionId={v.id}
            tab="types"
          />
        ) : (
          <ParseProblem file="ontology.toml" error={bundle.ontology.error} />
        )}
      </TabPanel>
      <TabPanel id="types" value={tab}>
        <AgentTypes
          bundle={bundle}
          graph={graph}
          sources={sources}
          versionId={v.id}
        />
      </TabPanel>
      <TabPanel id="links" value={tab}>
        <AgentLinks bundle={bundle} />
      </TabPanel>
      <TabPanel id="mappings" value={tab}>
        <AgentMappings bundle={bundle} sources={sources} />
      </TabPanel>
      <TabPanel id="sources" value={tab}>
        <AgentSources bundle={bundle} sources={sources} />
      </TabPanel>
      <TabPanel id="rules" value={tab}>
        <AgentRules bundle={bundle} evidenceWithheld={v.evidenceWithheld} />
      </TabPanel>
      <TabPanel id="questions" value={tab}>
        <AgentQuestions bundle={bundle} sources={sources} />
      </TabPanel>
      <TabPanel id="edit" value={tab}>
        <AgentEdit
          bundle={bundle}
          version={v}
          edits={edits}
          setEdits={setEdits}
          run={run}
          setRun={setRun}
          tryBase={tryBase}
          setTryBase={setTryBase}
        />
      </TabPanel>
      <TabPanel id="toml" value={tab}>
        <AgentToml bundle={bundle} />
      </TabPanel>
    </>
  );
}
