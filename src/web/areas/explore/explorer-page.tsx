import { useCallback, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Database } from "lucide-react";
import {
  FOUNDRY_EDGE_PREFIX,
  FOUNDRY_LOAD,
  FOUNDRY_NODE_PREFIX,
} from "../../../shared/foundry-labels";
import { LOCAL } from "../../../shared/deployment";
import type { FoundryLoad } from "../../../shared/wfe";
import { ButtonLink, EmptyState, SkeletonText } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { DASHBOARD_URL } from "../workspace/members";
import {
  LoadNotice,
  NothingLoaded,
  useLoadSelection,
} from "../ontology/load-selection";
import { loadVersion, versionLayout } from "../ontology/model";
import { ontologyQueries } from "../ontology/queries";
import { useOntology } from "../ontology/scope";
import { DiffPanel } from "./compare-panel";
import type { KeyItem } from "./compare";
import type { DatabaseQueryTarget } from "./database-query";
import {
  DatabaseExplorer,
  type DatabaseExplorerProps,
} from "./explorer/DatabaseExplorer";
import type { GraphNode, LabelExclusion } from "./explorer/lib/types";
import type { Scope } from "./load-scope";
import { entityRow } from "./model";
import { isInternal, readable } from "./naming";
import { useTenantReads } from "./tenant-reads";
import { ExplorerTop } from "./top-bar";
import { useNaming } from "./use-naming";

// /explore/:ontologyId: helix-website's graph explorer on the
// ontology's database, reading with the user's session, within one load
// (`?load=`, the current one by default) or Other data (`?load=other`), and
// with `&compare=<load>` two loads side by side over their diff; with no
// load at all it reads nothing and says how to load one. Its labels,
// canvas, inspector and search are the explorer's own: on one load the page
// is the explorer, its labels in the section's panel beside the rail and its
// inspector the right-hand sidebar; a compare's two sides are each a box of
// their own. In a load the labels list the load's version, the first view
// is seeded from its hub types,
// and a node reached outside the load is drawn dashed. A node that is one of
// the ontology's entities also opens its page here, reads by the load's
// version's names (./naming.ts), and leaves the layout's own properties out
// of the inspector. Foundry's own labels stay hidden until the viewer shows
// them. ?node=<database id> selects a node.

const FOUNDRY_INTERNALS: LabelExclusion = {
  nodePrefix: FOUNDRY_NODE_PREFIX,
  edgePrefix: FOUNDRY_EDGE_PREFIX,
  notice:
    "It is one of Foundry’s own records, which the Explorer hides. Turn on Show Foundry internals under Labels to see it.",
};

type Focus = {
  label: string;
  property: "entity_key" | "norm_name";
  value: string;
  seq: number;
};

/** What finds an entity node in another load: its type and key. */
function identityOf(node: GraphNode): Omit<Focus, "seq"> | null {
  const key = node.properties.entity_key,
    name = node.properties.norm_name;
  if (typeof key === "string")
    return { label: node.label, property: "entity_key", value: key };
  if (typeof name === "string")
    return { label: node.label, property: "norm_name", value: name };
  return null;
}

/** One explorer of the page, on one load (or Other data). */
function LoadExplorer({
  frame,
  scope,
  load,
  focus,
  onSelectNode,
  syncUrl,
  loads,
  onShowOtherData,
}: {
  frame: DatabaseExplorerProps["frame"];
  scope: Scope;
  load: FoundryLoad | null;
  focus?: Focus | null;
  onSelectNode?: (node: GraphNode | null) => void;
  syncUrl: boolean;
  loads: readonly FoundryLoad[];
  onShowOtherData: () => void;
}) {
  const { id: ws, slug, notify } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    navigate = useNavigate(),
    { limiter } = useTenantReads();
  const tenantId = ontology.tenantId || "",
    createdAt = ontology.createdAt;
  const target = useMemo<DatabaseQueryTarget>(
    () => ({ kind: "tenant", tenantId, createdAt }),
    [tenantId, createdAt],
  );
  // The load's version names what shows and lays out the rail and first view.
  const versionId =
    load?.versionId ||
    loads.find((l) => l.current)?.versionId ||
    ontology.currentVersionId ||
    "";
  const { naming } = useNaming(versionId);
  const version = useQuery({
    ...ontologyQueries.version(ws, o, versionId),
    enabled: !!versionId && scope.kind === "load",
  });
  const layout = useMemo(
    () =>
      scope.kind === "load" ? versionLayout(version.data?.version) : undefined,
    [scope.kind, version.data],
  );
  // A local build has no members or billing to send anyone to.
  const links = useMemo(
    () =>
      LOCAL
        ? { load: paths.ontology(o) }
        : {
            members: `${DASHBOARD_URL}/${encodeURIComponent(slug)}/members`,
            billing: `${DASHBOARD_URL}/${encodeURIComponent(slug)}/billing`,
            load: paths.ontology(o),
          },
    [slug, o],
  );
  const loadParam =
    scope.kind === "load" ? (load?.current ? null : scope.id) : "other";
  // An entity node (a type of the ontology with its key or name) has a page.
  const openPage = useCallback(
    (node: GraphNode) => {
      const row = entityRow({
        $id: node.dbId,
        $label: node.label,
        ...node.properties,
      });
      if (!row) return undefined;
      const page = paths.entity(o, row.id);
      return () =>
        navigate(
          loadParam ? `${page}?load=${encodeURIComponent(loadParam)}` : page,
        );
    },
    [navigate, o, loadParam],
  );
  // Nodes of the version's types read by its names; others keep the
  // explorer's own caption (a name or title it finds, else label and id).
  const caption = useCallback(
    (node: GraphNode) => {
      const r = readable(naming, node.label, node.properties, node.dbId);
      return r.known ? r.name : undefined;
    },
    [naming],
  );
  const internalProperty = useCallback(
    (target: "node" | "edge", label: string, key: string) =>
      isInternal(naming, target, label, key),
    [naming],
  );
  // "Load v3 · 18 Oct" for a node a load wrote; "Other data" for the rest.
  const provenance = useCallback(
    (node: GraphNode) => {
      const id = node.properties[FOUNDRY_LOAD];
      if (typeof id !== "string") return "Other data";
      const l = loads.find((x) => x.id === id);
      if (!l) return "A deleted load";
      const day = l.createdAt
        ? new Date(l.createdAt).toLocaleDateString(undefined, {
            day: "numeric",
            month: "short",
          })
        : "";
      return `Load ${loadVersion(l)}${day ? ` · ${day}` : ""}`;
    },
    [loads],
  );
  // A load's explorer waits for its version: the labels and the first view
  // come from it.
  if (scope.kind === "load" && versionId && version.isPending)
    return frame.kind === "page" ? (
      <>
        {frame.header(null)}
        <div className="ex-frame">
          <SkeletonText lines={8} />
        </div>
      </>
    ) : (
      <SkeletonText lines={8} />
    );
  return (
    <DatabaseExplorer
      key={`${tenantId}|${scope.kind === "load" ? scope.id : "other"}`}
      frame={frame}
      target={target}
      scope={scope}
      layout={layout}
      limiter={limiter}
      textOverFetch={loads.length + 1}
      onShowOtherData={onShowOtherData}
      provenance={provenance}
      onSelectNode={onSelectNode}
      focus={focus}
      syncUrl={syncUrl}
      databaseName={ontology.databaseName || tenantId}
      canRead={null}
      links={links}
      notify={notify}
      openPage={openPage}
      caption={caption}
      internalProperty={internalProperty}
      internalLabels={FOUNDRY_INTERNALS}
    />
  );
}

export function ExplorerPage() {
  const { id: ws } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const selection = useLoadSelection();
  const { choice, compare, loads } = selection;
  const tenantId = ontology.tenantId || "";
  // Compare: selecting an entity on one side focuses it on the other.
  const [focus, setFocus] = useState<{ a: Focus | null; b: Focus | null }>({
    a: null,
    b: null,
  });
  const seq = useRef(0);
  const follow = (side: "a" | "b") => (node: GraphNode | null) => {
    const id = node && identityOf(node);
    if (!id) return;
    const other = side === "a" ? "b" : "a";
    setFocus((f) => {
      const current = f[other];
      if (current && current.label === id.label && current.value === id.value)
        return f;
      return { ...f, [other]: { ...id, seq: (seq.current += 1) } };
    });
  };
  const focusBoth = (type: string, item: KeyItem) => {
    const id = { label: type, property: item.property, value: item.value };
    seq.current += 1;
    setFocus({
      a: { ...id, seq: seq.current },
      b: { ...id, seq: seq.current },
    });
  };
  const versionOf = (l: FoundryLoad | null) => l?.versionId || "";
  const versionA = useQuery({
    ...ontologyQueries.version(
      ws,
      o,
      choice.kind === "load" ? versionOf(choice.load) : "",
    ),
    enabled: !!compare && choice.kind === "load",
  });
  const versionB = useQuery({
    ...ontologyQueries.version(ws, o, versionOf(compare)),
    enabled: !!compare,
  });
  // The page's bar (with the explorer's tools, once it has them) and what
  // it says about the load.
  const top = (tools?: ReactNode) => (
    <>
      <ExplorerTop view="graph" selection={selection}>
        {tools}
      </ExplorerTop>
      <LoadNotice selection={selection} />
    </>
  );
  if (!tenantId)
    return (
      <div className="ex-view">
        <ExplorerTop view="graph" />
        <div className="ex-frame">
          <EmptyState
            icon={Database}
            title="You can’t read the ontology’s database"
            text="It lives in a project you can’t see. Ask a workspace admin for access to that project."
            action={
              <ButtonLink to={paths.ontologies()}>Go to Ontologies</ButtonLink>
            }
          />
        </div>
      </div>
    );
  // A load that is gone, or loads that couldn't be read, say so; with no
  // load at all there is nothing to read; else the page waits for its scope.
  const stuck =
    (choice.kind === "missing" && !selection.pending) || !!selection.failed;
  const none = choice.kind === "none" && !selection.pending;
  if (stuck || none || !selection.scope)
    return (
      <div className="ex-view">
        <ExplorerTop view="graph" selection={selection} />
        <div className="ex-frame">
          {stuck ? (
            <LoadNotice selection={selection} />
          ) : none ? (
            <NothingLoaded purpose="explore its entities and facts" />
          ) : (
            <SkeletonText lines={8} />
          )}
        </div>
      </div>
    );
  const showOther = () => selection.select("other");
  if (compare && choice.kind === "load")
    return (
      <div className="ex-view is-compare">
        {top()}
        <div className="ex-frame">
          <div className="ex-compare">
            <section
              className="ex-compare-side"
              aria-label={`Load ${loadVersion(choice.load)}`}
            >
              <h2>
                {loadVersion(choice.load)}
                {choice.load.current ? " · current" : ""}
              </h2>
              <LoadExplorer
                frame={{ kind: "side" }}
                scope={selection.scope}
                load={choice.load}
                loads={loads}
                focus={focus.a}
                onSelectNode={follow("a")}
                syncUrl
                onShowOtherData={showOther}
              />
            </section>
            <section
              className="ex-compare-side"
              aria-label={`Load ${loadVersion(compare)}`}
            >
              <h2>
                {loadVersion(compare)}
                {compare.current ? " · current" : ""}
              </h2>
              <LoadExplorer
                frame={{ kind: "side" }}
                scope={{ kind: "load", id: compare.id }}
                load={compare}
                loads={loads}
                focus={focus.b}
                onSelectNode={follow("b")}
                syncUrl={false}
                onShowOtherData={showOther}
              />
            </section>
          </div>
          <DiffPanel
            a={choice.load}
            b={compare}
            versions={{ a: versionA.data?.version, b: versionB.data?.version }}
            onFocus={focusBoth}
          />
        </div>
      </div>
    );
  return (
    <div className="ex-view is-graph">
      <LoadExplorer
        frame={{ kind: "page", header: top }}
        scope={selection.scope}
        load={choice.kind === "load" ? choice.load : null}
        loads={loads}
        syncUrl
        onShowOtherData={showOther}
      />
    </div>
  );
}
