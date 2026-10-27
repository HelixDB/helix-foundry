import { useMemo, useState } from "react";
import {
  ArrowRight,
  ArrowUpRight,
  Check,
  Copy,
  Eye,
  EyeOff,
  Loader2,
  LocateFixed,
  MoreHorizontal,
  PanelRightClose,
  Pin,
  PinOff,
  Plus,
  SquareArrowOutUpRight,
  Undo2,
  Waypoints,
  X,
} from "lucide-react";

import { Select, Skeleton } from "../../../kit";
import { cn } from "./ui";

import { ReadOnly } from "./ExplorerBar";
import { ScanLabelsControl, type LabelFocus } from "./LabelRail";
import {
  degreeText,
  edgeToJson,
  labelKey,
  nodeToJson,
  relationshipGroups,
  totalsAreFloors,
  type RelationshipGroup,
} from "./lib/graph-store";
import type { EdgeLabelPairs } from "./lib/schema";
import { relationshipsForLabel } from "./lib/schema";
import { EXPLORER_LIMITS } from "./lib/queries";
import type {
  DegreeInfo,
  GraphEdge,
  GraphNode,
  LabelSchema,
  PropertySchema,
  Selection,
} from "./lib/types";
import { IndexBadges, PropertyList, type PropertyRow } from "./PropertyList";
import { InlineMenu, MenuItem } from "./query/InlineMenu";
import { useCopyFeedback } from "./query/useCopyFeedback";
import type { ExplorerGraph } from "./useExplorerGraph";
import {
  formatCount,
  KBD,
  MONO,
  OUTLINE_BUTTON,
  plural,
  SECTION_TITLE,
  SMALL_ICON_BUTTON,
  TEXT_BUTTON,
} from "./ui";

const EXPLORER_THRESHOLD = EXPLORER_LIMITS.degreeThreshold;

export type InspectorSubject =
  | { kind: "overview" }
  | { kind: "label"; focus: LabelFocus }
  | { kind: "node"; node: GraphNode }
  | { kind: "edge"; edge: GraphEdge };

export interface InspectorProps {
  subject: InspectorSubject;
  databaseName: string;
  explorer: ExplorerGraph;
  /** Label schemas: probed, merged with what's loaded and the index catalog. */
  schemas: LabelSchema[];
  pairs: EdgeLabelPairs[];
  colorFor: (label: string) => string;
  captionOf: (node: GraphNode) => string;
  captionKeys: Readonly<Record<string, string>>;
  onCaptionKeyChange: (label: string, key: string | null) => void;
  /** Properties left out of the lists: the layout's own, not the data's. */
  internalProperty?: (
    target: "node" | "edge",
    label: string,
    key: string,
  ) => boolean;
  /** The previous selection, when there is one to go back to. */
  back: { title: string } | null;
  onBack: () => void;
  onSelect: (selection: Selection) => void;
  onFocusLabel: (focus: LabelFocus | null) => void;
  onFocusNode: (nodeId: string) => void;
  /** Opens a node's page in Foundry, for an ontology's entities; unset for other nodes. */
  onOpenPage?: (node: GraphNode) => void;
  /**
   * Where a node came from, in words ("Load v3 · 18 Oct", "Other data"),
   * and whether that is outside what the explorer reads.
   */
  provenance?: (node: GraphNode) => { text: string; outside: boolean } | null;
  primaryModifier: string;
  /** Touch device: tips talk about taps and long-presses. */
  touch?: boolean;
  /** The first view is still loading. */
  booting?: boolean;
  onCollapse?: () => void;
  /** "close" (an X) for sheets and overlays; "panel" for the docked inspector. */
  collapseIcon?: "panel" | "close";
  className?: string;
}

const NEIGHBOR_PREVIEW = 5;

export function Inspector(props: InspectorProps) {
  const { subject, onCollapse, collapseIcon = "panel", className } = props;
  const title =
    subject.kind === "node"
      ? "Node"
      : subject.kind === "edge"
        ? "Edge"
        : subject.kind === "label"
          ? "Label"
          : "Overview";
  return (
    <div className={cn("ex-panel", className)}>
      <div className="ex-panel-head">
        <h2 className={SECTION_TITLE}>{title}</h2>
        {onCollapse && (
          <button
            type="button"
            onClick={onCollapse}
            aria-label={
              collapseIcon === "close" ? "Close inspector" : "Hide inspector"
            }
            title={collapseIcon === "close" ? "Close" : "Hide inspector (])"}
            className={SMALL_ICON_BUTTON}
          >
            {collapseIcon === "close" ? (
              <X aria-hidden="true" size={16} />
            ) : (
              <PanelRightClose aria-hidden="true" size={16} />
            )}
          </button>
        )}
      </div>
      {/* relative: keeps absolutely positioned descendants (sr-only live regions) inside the scroller. */}
      <div className="ex-panel-body is-flush">
        {subject.kind === "node" ? (
          <NodeInspector key={subject.node.id} node={subject.node} {...props} />
        ) : subject.kind === "edge" ? (
          <EdgeInspector key={subject.edge.id} edge={subject.edge} {...props} />
        ) : subject.kind === "label" ? (
          <LabelInspector
            key={`${subject.focus.target}:${subject.focus.label}`}
            focus={subject.focus}
            {...props}
          />
        ) : (
          <DatabaseOverview {...props} />
        )}
      </div>
    </div>
  );
}

// ============ Shared pieces ============

function Section({
  title,
  aside,
  children,
  className,
}: {
  title: string;
  aside?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <section className={cn("ex-section", className)}>
      <div className="ex-section-head">
        <h3 className={SECTION_TITLE}>{title}</h3>
        {aside}
      </div>
      {children}
    </section>
  );
}

function Swatch({ color, className }: { color: string; className?: string }) {
  return (
    <span
      aria-hidden="true"
      className={cn("ex-swatch is-round", className)}
      style={{ background: color }}
    />
  );
}

function CopyId({ dbId, prefix }: { dbId: number; prefix: string }) {
  const { copied, copy } = useCopyFeedback();
  return (
    <button
      type="button"
      onClick={() => copy("id", String(dbId))}
      title="Copy id"
      className="ex-text-btn"
    >
      <span className={cn(MONO, "ex-num")}>
        {prefix} {dbId}
      </span>
      {copied === "id" ? (
        <Check aria-hidden="true" size={14} className="ex-ok" />
      ) : (
        <Copy aria-hidden="true" size={14} />
      )}
      <span className="hf-sr-only">
        {copied === "id" ? "Copied" : "Copy id"}
      </span>
    </button>
  );
}

function BackButton({
  back,
  onBack,
}: {
  back: { title: string } | null;
  onBack: () => void;
}) {
  if (!back) return null;
  return (
    <button
      type="button"
      onClick={onBack}
      title={`Back to ${back.title} (Alt+←)`}
      className="ex-text-btn ex-back"
    >
      <Undo2 aria-hidden="true" size={14} />
      <span className="ex-truncate">Back to {back.title}</span>
    </button>
  );
}

function propertyRows(
  properties: Record<string, GraphNode["properties"][string]>,
  schema: LabelSchema | undefined,
  hidden?: (key: string) => boolean,
): PropertyRow[] {
  const indexes = new Map(
    schema?.properties.map((property) => [property.key, property.indexes]) ??
      [],
  );
  return Object.keys(properties)
    .filter((key) => !hidden?.(key))
    .sort((a, b) => a.localeCompare(b))
    .map((key) => ({
      key,
      value: properties[key],
      indexes: indexes.get(key) ?? [],
    }));
}

function schemaFor(
  schemas: LabelSchema[],
  target: "node" | "edge",
  label: string,
): LabelSchema | undefined {
  return schemas.find(
    (schema) => schema.target === target && schema.label === label,
  );
}

// ============ Node ============

function NodeInspector({
  node,
  explorer,
  schemas,
  colorFor,
  captionOf,
  back,
  onBack,
  onSelect,
  onFocusNode,
  onOpenPage,
  internalProperty,
  provenance,
}: InspectorProps & { node: GraphNode }) {
  const { state } = explorer;
  const degree = state.degrees.get(node.id);
  const degreeStatus = state.degreeStatus.get(node.id);
  const expanding = state.pending.has(`expand:${node.id}`);
  const loadingFull = state.pending.has(`full:${node.id}`);
  const pinned = state.pinned.has(node.id);
  const hidden =
    state.hiddenNodes.has(node.id) ||
    state.hiddenLabels.has(labelKey("node", node.label));
  const schema = schemaFor(schemas, "node", node.label);
  const rows = useMemo(
    () =>
      propertyRows(node.properties, schema, (key) =>
        Boolean(internalProperty?.("node", node.label, key)),
      ),
    [node.properties, node.label, schema, internalProperty],
  );
  const groups = useMemo(
    () => relationshipGroups(state.store, node.id, degree),
    [state.store, node.id, degree],
  );
  const { copied, copy } = useCopyFeedback();
  const caption = captionOf(node);

  const copyLink = () => {
    const url = new URL(window.location.href);
    url.searchParams.set("node", String(node.dbId));
    copy("link", url.toString());
  };

  return (
    <div>
      <div className="ex-subject">
        <div className="ex-subject-top">
          {/* Labels are case-sensitive identifiers: show them exactly as stored. */}
          <span className="ex-subject-kind">
            <Swatch color={colorFor(node.label)} />
            <span className={cn(MONO, "ex-truncate")}>{node.label}</span>
          </span>
          <BackButton back={back} onBack={onBack} />
        </div>
        <p className="ex-subject-title ex-truncate" title={caption}>
          {caption}
        </p>
        <div className="ex-subject-meta">
          <CopyId dbId={node.dbId} prefix="id" />
          {(() => {
            const from = provenance?.(node);
            return from ? (
              <span
                className={from.outside ? "ex-warn" : "ex-dim"}
                data-testid="node-load"
              >
                {from.text}
              </span>
            ) : null;
          })()}
          {pinned && <span className="ex-dim">Pinned</span>}
          {hidden && <span className="ex-warn">Hidden on canvas</span>}
        </div>
        <div className="ex-subject-actions">
          <ExpandMenu
            node={node}
            degree={degree}
            explorer={explorer}
            busy={expanding}
          />
          <button
            type="button"
            onClick={() => onFocusNode(node.id)}
            disabled={hidden}
            aria-label="Focus"
            title="Focus: center the canvas on this node"
            className={cn(OUTLINE_BUTTON, "is-square")}
          >
            <LocateFixed aria-hidden="true" size={16} />
          </button>
          <button
            type="button"
            onClick={() =>
              hidden ? explorer.revealNode(node) : explorer.hideNodes([node.id])
            }
            aria-label={hidden ? "Show on the canvas" : "Hide from the canvas"}
            title={hidden ? "Show on the canvas" : "Hide from the canvas (H)"}
            className={cn(OUTLINE_BUTTON, "is-square")}
          >
            {hidden ? (
              <Eye aria-hidden="true" size={16} />
            ) : (
              <EyeOff aria-hidden="true" size={16} />
            )}
          </button>
          <button
            type="button"
            onClick={() => explorer.setPinned(node.id, !pinned)}
            aria-pressed={pinned}
            aria-label={pinned ? "Unpin" : "Pin in place"}
            title={
              pinned
                ? "Unpin: let the layout move this node (P)"
                : "Pin: keep this node in place (P)"
            }
            className={cn(OUTLINE_BUTTON, "is-square", pinned && "is-on")}
          >
            {pinned ? (
              <PinOff aria-hidden="true" size={16} />
            ) : (
              <Pin aria-hidden="true" size={16} />
            )}
          </button>
          <InlineMenu
            label={<span className="hf-sr-only">More actions</span>}
            icon={
              copied ? (
                <Check aria-hidden="true" size={14} className="ex-ok" />
              ) : (
                <MoreHorizontal aria-hidden="true" size={16} />
              )
            }
            ariaLabel="More actions"
            title="More actions"
            hideChevron
            triggerClassName="is-square"
            menuLabel="Node actions"
            align="end"
          >
            {(close) => (
              <>
                {onOpenPage && (
                  <MenuItem
                    onSelect={() => {
                      close();
                      onOpenPage(node);
                    }}
                  >
                    <span className="ex-menu-item-label">
                      <SquareArrowOutUpRight aria-hidden="true" size={14} />{" "}
                      Open its page
                    </span>
                    <span className="ex-dim">
                      Its facts, documents and reviews in Foundry.
                    </span>
                  </MenuItem>
                )}
                <MenuItem
                  onSelect={() => {
                    close();
                    copy("json", JSON.stringify(nodeToJson(node), null, 2));
                  }}
                >
                  Copy as JSON
                  {!node.complete && node.omitted.length > 0 && (
                    <span className="ex-dim">
                      Without {node.omitted.join(", ")}, which aren’t loaded.
                    </span>
                  )}
                </MenuItem>
                <MenuItem
                  onSelect={() => {
                    close();
                    copy("id", String(node.dbId));
                  }}
                >
                  Copy id
                </MenuItem>
                <MenuItem
                  onSelect={() => {
                    close();
                    copyLink();
                  }}
                >
                  Copy link to this node
                </MenuItem>
              </>
            )}
          </InlineMenu>
          <span className="hf-sr-only" aria-live="polite">
            {copied ? `Copied ${copied}` : ""}
          </span>
        </div>
      </div>

      <Section
        title="Properties"
        aside={
          <span className="ex-num ex-dim">{formatCount(rows.length)}</span>
        }
      >
        <PropertyList rows={rows} />
        {!node.complete && (node.omitted.length > 0 || rows.length === 0) && (
          <div className="ex-omitted">
            <p className="ex-dim">
              {node.omitted.length > 0 ? (
                <>
                  Not loaded:{" "}
                  {node.omitted.map((key, index) => (
                    <span key={key}>
                      {index > 0 && ", "}
                      <span className={MONO}>{key}</span>
                    </span>
                  ))}
                  . Large values like vectors and long text are left out of
                  routine loads.
                </>
              ) : (
                "This node’s properties haven’t been loaded yet."
              )}
            </p>
            <button
              type="button"
              onClick={() => void explorer.loadFullNode(node.id)}
              disabled={loadingFull}
              className={OUTLINE_BUTTON}
            >
              {loadingFull && (
                <Loader2 aria-hidden="true" size={14} className="hf-spin" />
              )}
              Load full node
            </button>
          </div>
        )}
      </Section>

      <Section
        title="Relationships"
        aside={
          degree ? (
            <span className="ex-num ex-dim">
              {degreeText(degree.outDegree, degree.atLeast?.out)} out ·{" "}
              {degreeText(degree.inDegree, degree.atLeast?.in)} in
            </span>
          ) : degreeStatus === "loading" ? (
            <Skeleton width={80} height={12} />
          ) : null
        }
      >
        {degree && (degree.atLeast?.out || degree.atLeast?.in) && (
          <CountRelationshipsControl
            counting={explorer.state.pending.has(`count:${node.id}`)}
            onCount={() => void explorer.countRelationships(node.id)}
          />
        )}
        <RelationshipList
          node={node}
          groups={groups}
          degree={degree}
          degreeStatus={degreeStatus}
          explorer={explorer}
          colorFor={colorFor}
          captionOf={captionOf}
          onSelect={onSelect}
        />
      </Section>
    </div>
  );
}

/** "12 edges", or "1,000+ edges" for a floor. */
function edgesText(count: number, atLeast: boolean): string {
  return atLeast ? `${degreeText(count, true)} edges` : plural(count, "edge");
}

/**
 * Exact relationship counts for a large node: reading every one of its edges,
 * so only on request and after a warning (like the label scan).
 */
function CountRelationshipsControl({
  counting,
  onCount,
}: {
  counting: boolean;
  onCount: () => void;
}) {
  const [confirming, setConfirming] = useState(false);
  if (counting) {
    return (
      <p role="status" className="ex-inline-status">
        <Loader2 aria-hidden="true" size={14} className="hf-spin ex-busy" />
        Counting every relationship…
      </p>
    );
  }
  if (!confirming) {
    return (
      <div className="ex-foot-row is-wrap">
        <p className="ex-dim">
          More than {formatCount(EXPLORER_THRESHOLD)} on a side: counts by edge
          label aren’t loaded on their own.
        </p>
        <button
          type="button"
          onClick={() => setConfirming(true)}
          className={TEXT_BUTTON}
        >
          Count relationships…
        </button>
      </div>
    );
  }
  return (
    <div
      role="group"
      aria-label="Confirm counting relationships"
      className="ex-confirm"
    >
      <p className="ex-confirm-title">Count every relationship?</p>
      <p className="ex-dim">
        This reads each of this node’s edges to count them by label. On a node
        with very many it can take a while, and it slows other queries while it
        runs.
      </p>
      <div className="ex-row-actions">
        <button
          type="button"
          onClick={() => {
            setConfirming(false);
            onCount();
          }}
          className={OUTLINE_BUTTON}
        >
          Count anyway
        </button>
        {/* Focus lands on the safe choice. */}
        <button
          type="button"
          autoFocus
          onClick={() => setConfirming(false)}
          className={TEXT_BUTTON}
        >
          Cancel
        </button>
      </div>
    </div>
  );
}

function ExpandMenu({
  node,
  degree,
  explorer,
  busy,
}: {
  node: GraphNode;
  degree: DegreeInfo | undefined;
  explorer: ExplorerGraph;
  busy: boolean;
}) {
  const edgeLabels = useMemo(() => {
    const labels = new Map<string, { out: number; in: number }>();
    for (const { label, count } of degree?.outByLabel ?? [])
      labels.set(label, { out: count ?? 0, in: labels.get(label)?.in ?? 0 });
    for (const { label, count } of degree?.inByLabel ?? [])
      labels.set(label, { out: labels.get(label)?.out ?? 0, in: count ?? 0 });
    return [...labels.entries()].sort((a, b) => a[0].localeCompare(b[0]));
  }, [degree]);
  return (
    <InlineMenu
      label="Expand"
      icon={
        busy ? (
          <Loader2 aria-hidden="true" size={14} className="hf-spin ex-busy" />
        ) : (
          <Waypoints aria-hidden="true" size={14} />
        )
      }
      title="Load neighbors (E, or double-click the node)"
      menuLabel="Expand"
    >
      {(close) => (
        <>
          <MenuItem
            onSelect={() => {
              close();
              void explorer.expand(node.id);
            }}
          >
            All neighbors
            {degree && (
              <span className="ex-num ex-dim">
                {edgesText(
                  degree.outDegree + degree.inDegree,
                  Boolean(degree.atLeast?.out || degree.atLeast?.in),
                )}
              </span>
            )}
          </MenuItem>
          <MenuItem
            disabled={degree?.outDegree === 0}
            onSelect={() => {
              close();
              void explorer.expand(node.id, { direction: "out" });
            }}
          >
            Outgoing
            {degree && (
              <span className="ex-num ex-dim">
                {edgesText(degree.outDegree, Boolean(degree.atLeast?.out))}
              </span>
            )}
          </MenuItem>
          <MenuItem
            disabled={degree?.inDegree === 0}
            onSelect={() => {
              close();
              void explorer.expand(node.id, { direction: "in" });
            }}
          >
            Incoming
            {degree && (
              <span className="ex-num ex-dim">
                {edgesText(degree.inDegree, Boolean(degree.atLeast?.in))}
              </span>
            )}
          </MenuItem>
          {edgeLabels.length > 0 && (
            <div role="separator" className="ex-menu-sep" />
          )}
          {edgeLabels.map(([label, counts]) => (
            <MenuItem
              key={label}
              onSelect={() => {
                close();
                void explorer.expand(node.id, { edgeLabel: label });
              }}
            >
              <span className={MONO}>{label}</span>
              <span className="ex-num ex-dim">
                {[
                  counts.out ? `${formatCount(counts.out)} out` : null,
                  counts.in ? `${formatCount(counts.in)} in` : null,
                ]
                  .filter(Boolean)
                  .join(" · ")}
              </span>
            </MenuItem>
          ))}
        </>
      )}
    </InlineMenu>
  );
}

function RelationshipList({
  node,
  groups,
  degree,
  degreeStatus,
  explorer,
  colorFor,
  captionOf,
  onSelect,
}: {
  node: GraphNode;
  groups: RelationshipGroup[];
  degree: DegreeInfo | undefined;
  degreeStatus: "loading" | "error" | undefined;
  explorer: ExplorerGraph;
  colorFor: (label: string) => string;
  captionOf: (node: GraphNode) => string;
  onSelect: (selection: Selection) => void;
}) {
  if (groups.length === 0) {
    if (degreeStatus === "loading") {
      return (
        <div className="ex-skeletons">
          <Skeleton width={160} height={16} />
          <Skeleton width={128} height={16} />
        </div>
      );
    }
    if (degree && degree.inDegree + degree.outDegree === 0) {
      return <p className="ex-empty-note">No relationships.</p>;
    }
    return (
      <div className="ex-foot-row">
        <p className="ex-dim">
          {degreeStatus === "error"
            ? "Couldn’t count relationships."
            : "Nothing loaded yet."}
        </p>
        <button
          type="button"
          onClick={() => void explorer.expand(node.id)}
          className={OUTLINE_BUTTON}
        >
          <Waypoints aria-hidden="true" size={14} /> Expand
        </button>
      </div>
    );
  }
  return (
    <div className="ex-rel-groups">
      {groups.map((group) => (
        <RelationshipGroupView
          key={`${group.direction}:${group.edgeLabel}`}
          node={node}
          group={group}
          explorer={explorer}
          colorFor={colorFor}
          captionOf={captionOf}
          onSelect={onSelect}
        />
      ))}
    </div>
  );
}

function RelationshipGroupView({
  node,
  group,
  explorer,
  colorFor,
  captionOf,
  onSelect,
}: {
  node: GraphNode;
  group: RelationshipGroup;
  explorer: ExplorerGraph;
  colorFor: (label: string) => string;
  captionOf: (node: GraphNode) => string;
  onSelect: (selection: Selection) => void;
}) {
  const [showAll, setShowAll] = useState(false);
  const { store } = explorer.state;
  const busy = explorer.state.pending.has(`expand:${node.id}`);
  const more = group.total !== null && group.total > group.loaded;
  const complete = group.total !== null && group.total <= group.loaded;
  const neighbors = showAll
    ? group.neighbors
    : group.neighbors.slice(0, NEIGHBOR_PREVIEW);
  const arrow = group.direction === "out" ? "→" : "←";
  const directionName = group.direction === "out" ? "outgoing" : "incoming";

  return (
    <div className="ex-rel-group">
      <div className="ex-rel-head">
        <span aria-hidden="true" className="ex-arrow">
          {arrow}
        </span>
        <span
          className={cn(MONO, "ex-truncate ex-rel-label")}
          title={`${group.edgeLabel} (${directionName})`}
        >
          {group.edgeLabel}
        </span>
        <span className="hf-sr-only">{directionName}</span>
        <span className="ex-num">
          {formatCount(group.loaded)}
          {more && <> of {formatCount(group.total ?? 0)}</>}
        </span>
        <button
          type="button"
          onClick={() =>
            void explorer.loadMore(node.id, group.direction, group.edgeLabel)
          }
          disabled={busy || complete}
          aria-label={
            complete
              ? `Every ${directionName} ${group.edgeLabel} edge is loaded`
              : `Load ${directionName} ${group.edgeLabel} neighbors`
          }
          title={
            complete
              ? "All loaded"
              : `Load ${directionName} ${group.edgeLabel} neighbors`
          }
          className={cn(
            SMALL_ICON_BUTTON,
            "ex-push",
            complete && "is-complete",
          )}
        >
          {complete ? (
            <Check aria-hidden="true" size={14} />
          ) : (
            <Plus aria-hidden="true" size={14} />
          )}
        </button>
      </div>
      {neighbors.length > 0 && (
        <ul className="ex-rel-list">
          {neighbors.map(({ edgeId, nodeId }) => {
            const neighbor = store.nodes.get(nodeId);
            if (!neighbor) return null;
            return (
              <li key={edgeId}>
                <button
                  type="button"
                  onClick={() => onSelect({ kind: "node", id: nodeId })}
                  className="ex-rel-row"
                >
                  <Swatch color={colorFor(neighbor.label)} />
                  <span className="ex-truncate">
                    {nodeId === node.id
                      ? `${captionOf(neighbor)} (itself)`
                      : captionOf(neighbor)}
                  </span>
                  <span className="ex-push ex-dim">{neighbor.label}</span>
                  <ArrowUpRight
                    aria-hidden="true"
                    size={14}
                    className="ex-dim"
                  />
                </button>
              </li>
            );
          })}
        </ul>
      )}
      {(group.neighbors.length > NEIGHBOR_PREVIEW || more) && (
        <div className="ex-rel-more">
          {group.neighbors.length > NEIGHBOR_PREVIEW && (
            <button
              type="button"
              onClick={() => setShowAll((current) => !current)}
              className={TEXT_BUTTON}
            >
              {showAll
                ? "Show fewer"
                : `Show all ${formatCount(group.neighbors.length)}`}
            </button>
          )}
          {more && (
            <button
              type="button"
              onClick={() =>
                void explorer.loadMore(
                  node.id,
                  group.direction,
                  group.edgeLabel,
                )
              }
              disabled={busy}
              className={TEXT_BUTTON}
            >
              Load{" "}
              {formatCount(Math.min(50, (group.total ?? 0) - group.loaded))}{" "}
              more
            </button>
          )}
        </div>
      )}
    </div>
  );
}

// ============ Edge ============

function EndpointCard({
  role,
  node,
  fallbackId,
  colorFor,
  captionOf,
  onSelect,
}: {
  role: string;
  node: GraphNode | undefined;
  fallbackId: string;
  colorFor: (label: string) => string;
  captionOf: (node: GraphNode) => string;
  onSelect: (selection: Selection) => void;
}) {
  return (
    <button
      type="button"
      disabled={!node}
      onClick={() => onSelect({ kind: "node", id: fallbackId })}
      className="ex-endpoint"
    >
      <span className="ex-endpoint-role">{role}</span>
      {node ? (
        <>
          <Swatch color={colorFor(node.label)} />
          <span className="ex-search-text">
            <span className="ex-search-title ex-truncate">
              {captionOf(node)}
            </span>
            <span className="ex-search-meta ex-truncate">
              {node.label} ·{" "}
              <span className={cn(MONO, "ex-num")}>{node.dbId}</span>
            </span>
          </span>
          <ArrowUpRight aria-hidden="true" size={14} className="ex-dim" />
        </>
      ) : (
        <span className={cn(MONO, "ex-dim")}>{fallbackId}</span>
      )}
    </button>
  );
}

function EdgeInspector({
  edge,
  explorer,
  schemas,
  colorFor,
  captionOf,
  back,
  onBack,
  onSelect,
  internalProperty,
}: InspectorProps & { edge: GraphEdge }) {
  const { store } = explorer.state;
  const source = store.nodes.get(edge.source);
  const target = store.nodes.get(edge.target);
  const schema = schemaFor(schemas, "edge", edge.label);
  const rows = useMemo(
    () =>
      propertyRows(edge.properties, schema, (key) =>
        Boolean(internalProperty?.("edge", edge.label, key)),
      ),
    [edge.properties, edge.label, schema, internalProperty],
  );
  const { copied, copy } = useCopyFeedback();
  const hidden = explorer.state.hiddenLabels.has(labelKey("edge", edge.label));

  return (
    <div>
      <div className="ex-subject">
        <div className="ex-subject-top">
          <span className="ex-subject-kind is-caps">
            <span aria-hidden="true" className="ex-edge-swatch" />
            Edge
          </span>
          <BackButton back={back} onBack={onBack} />
        </div>
        <p
          className={cn(MONO, "ex-subject-title ex-truncate")}
          title={edge.label}
        >
          {edge.label}
        </p>
        <CopyId dbId={edge.dbId} prefix="id" />
        <div className="ex-subject-actions">
          <button
            type="button"
            onClick={() =>
              copy("json", JSON.stringify(edgeToJson(edge, store), null, 2))
            }
            className={OUTLINE_BUTTON}
          >
            {copied === "json" ? (
              <Check aria-hidden="true" size={14} className="ex-ok" />
            ) : (
              <Copy aria-hidden="true" size={14} />
            )}
            Copy as JSON
          </button>

          <button
            type="button"
            onClick={() => explorer.setLabelHidden("edge", edge.label, !hidden)}
            className={OUTLINE_BUTTON}
          >
            {hidden ? (
              <Eye aria-hidden="true" size={14} />
            ) : (
              <EyeOff aria-hidden="true" size={14} />
            )}
            {hidden ? `Show ${edge.label}` : `Hide ${edge.label}`}
          </button>
        </div>
      </div>
      <Section title="Connects">
        <div className="ex-endpoints">
          <EndpointCard
            role="From"
            node={source}
            fallbackId={edge.source}
            colorFor={colorFor}
            captionOf={captionOf}
            onSelect={onSelect}
          />
          <div aria-hidden="true" className="ex-endpoint-arrow">
            <ArrowRight size={14} />
          </div>
          <EndpointCard
            role="To"
            node={target}
            fallbackId={edge.target}
            colorFor={colorFor}
            captionOf={captionOf}
            onSelect={onSelect}
          />
        </div>
      </Section>
      <Section
        title="Properties"
        aside={
          <span className="ex-num ex-dim">{formatCount(rows.length)}</span>
        }
      >
        <PropertyList rows={rows} />
      </Section>
    </div>
  );
}

// ============ Label ============

function kindLabel(property: PropertySchema): string {
  return property.kinds.length
    ? property.kinds.join(" | ")
    : property.heavy
      ? "large value"
      : "unknown";
}

function LabelInspector({
  focus,
  explorer,
  schemas,
  pairs,
  colorFor,
  captionKeys,
  onCaptionKeyChange,
  onFocusLabel,
  internalProperty,
}: InspectorProps & { focus: LabelFocus }) {
  const { state, labels } = explorer;
  const isNode = focus.target === "node";
  const entry = (isNode ? labels.nodes : labels.edges).find(
    (candidate) => candidate.label === focus.label,
  );
  const page = state.labelPages[focus.label];
  const total = entry?.count ?? page?.total ?? null;
  const loaded = useMemo(() => {
    let count = 0;
    if (isNode)
      for (const node of state.store.nodes.values())
        count += node.label === focus.label ? 1 : 0;
    else
      for (const edge of state.store.edges.values())
        count += edge.label === focus.label ? 1 : 0;
    return count;
  }, [isNode, focus.label, state.store]);
  const schema = schemaFor(schemas, focus.target, focus.label);
  const hidden = state.hiddenLabels.has(labelKey(focus.target, focus.label));
  const loading = state.pending.has(`page:${focus.label}`);
  const relationships = useMemo(
    () => (isNode ? relationshipsForLabel(pairs, focus.label) : []),
    [isNode, pairs, focus.label],
  );
  const edgePairs = useMemo(
    () =>
      isNode
        ? []
        : (pairs.find((pair) => pair.edgeLabel === focus.label)?.pairs ?? []),
    [isNode, pairs, focus.label],
  );
  const captionCandidates = useMemo(
    () =>
      (schema?.properties ?? [])
        .filter(
          (property) =>
            !property.heavy &&
            !internalProperty?.("node", focus.label, property.key) &&
            (property.kinds.includes("string") ||
              property.kinds.includes("integer")),
        )
        .map((property) => property.key),
    [schema, internalProperty, focus.label],
  );
  const captionKey = captionKeys[focus.label] ?? "";

  return (
    <div>
      <div className="ex-subject">
        <div className="ex-subject-top">
          <span className="ex-subject-kind is-caps">
            {isNode ? (
              <Swatch color={colorFor(focus.label)} />
            ) : (
              <span aria-hidden="true" className="ex-edge-swatch" />
            )}
            {isNode ? "Node label" : "Edge label"}
          </span>
          <button
            type="button"
            onClick={() => onFocusLabel(null)}
            className={TEXT_BUTTON}
          >
            Clear
          </button>
        </div>
        <p className={cn(MONO, "ex-subject-title ex-truncate")}>
          {focus.label}
        </p>
        <dl className="ex-tiles">
          <div className="ex-tile">
            <dt>In the database</dt>
            <dd className="ex-num">
              {total === null ? "—" : formatCount(total)}
            </dd>
          </div>
          <div className="ex-tile">
            <dt>Loaded</dt>
            <dd className="ex-num">{formatCount(loaded)}</dd>
          </div>
        </dl>
        <div className="ex-subject-actions">
          {isNode && (
            <button
              type="button"
              onClick={() => void explorer.loadLabelPage(focus.label)}
              disabled={loading || page?.exhausted}
              className={OUTLINE_BUTTON}
            >
              {loading ? (
                <Loader2 aria-hidden="true" size={14} className="hf-spin" />
              ) : (
                <Plus aria-hidden="true" size={14} />
              )}
              {page?.exhausted ? "All loaded" : "Load 50 more"}
            </button>
          )}
          <button
            type="button"
            onClick={() =>
              explorer.setLabelHidden(focus.target, focus.label, !hidden)
            }
            className={OUTLINE_BUTTON}
          >
            {hidden ? (
              <Eye aria-hidden="true" size={14} />
            ) : (
              <EyeOff aria-hidden="true" size={14} />
            )}
            {hidden ? "Show on canvas" : "Hide on canvas"}
          </button>
        </div>
      </div>

      {isNode && (
        <Section title="Caption">
          <label className="ex-field is-select">
            <Select
              label={`Caption property for ${focus.label}`}
              value={captionKey}
              onChange={(key) => onCaptionKeyChange(focus.label, key || null)}
              options={[
                { value: "", label: "Automatic (name, title, …)" },
                ...captionCandidates.map((key) => ({ value: key, label: key })),
              ]}
            />
          </label>
          <p className="ex-dim ex-note">
            Shown on the canvas, in search and in lists.
          </p>
        </Section>
      )}

      <Section
        title="Properties"
        aside={
          schema?.sampled ? (
            <span
              className="ex-dim"
              title="Properties and fill rates come from the loaded and probed rows"
            >
              from {plural(schema.sampled, "row")}
            </span>
          ) : null
        }
      >
        {schema && schema.properties.length > 0 ? (
          <ul className="ex-schema">
            {schema.properties.map((property) => (
              <li key={property.key}>
                <span className={cn(MONO, "ex-truncate")} title={property.key}>
                  {property.key}
                </span>
                <IndexBadges kinds={property.indexes} />
                {property.heavy && (
                  <span
                    title="Large values are left out of routine loads; open a node and load it in full to see them"
                    className="ex-value-chip"
                  >
                    large
                  </span>
                )}
                <span className="ex-push ex-dim">{kindLabel(property)}</span>
                {schema.sampled > 0 && (
                  <span
                    className="ex-num ex-dim ex-fill"
                    title="Share of sampled rows that have it"
                  >
                    {Math.round(
                      (Math.min(property.seen, schema.sampled) /
                        schema.sampled) *
                        100,
                    )}
                    %
                  </span>
                )}
              </li>
            ))}
          </ul>
        ) : (
          <p className="ex-empty-note">No properties seen yet.</p>
        )}
      </Section>

      {isNode ? (
        <Section title="Relationships">
          {relationships.length > 0 ? (
            <ul className="ex-pairs">
              {relationships.map((relationship) => (
                <li key={`${relationship.direction}:${relationship.edgeLabel}`}>
                  <span aria-hidden="true" className="ex-arrow">
                    {relationship.direction === "out" ? "→" : "←"}
                  </span>
                  <button
                    type="button"
                    onClick={() =>
                      onFocusLabel({
                        target: "edge",
                        label: relationship.edgeLabel,
                      })
                    }
                    className={cn(MONO, "ex-text-btn ex-truncate")}
                  >
                    {relationship.edgeLabel}
                  </button>
                  <span className="ex-truncate ex-dim">
                    {relationship.direction === "out" ? "to" : "from"}{" "}
                    {relationship.otherLabels.join(", ")}
                  </span>
                  <span className="ex-push ex-num ex-dim">
                    {formatCount(relationship.count)}
                  </span>
                </li>
              ))}
            </ul>
          ) : (
            <p className="ex-empty-note">None among the loaded edges yet.</p>
          )}
          <p className="ex-dim ex-note">Counted from the loaded edges.</p>
        </Section>
      ) : (
        <Section title="Connects">
          {edgePairs.length > 0 ? (
            <ul className="ex-pairs">
              {edgePairs.map((pair) => (
                <li key={`${pair.sourceLabel}:${pair.targetLabel}`}>
                  <Swatch
                    color={colorFor(pair.sourceLabel)}
                    className="is-sm"
                  />
                  <span>{pair.sourceLabel}</span>
                  <span aria-hidden="true" className="ex-dim">
                    →
                  </span>
                  <Swatch
                    color={colorFor(pair.targetLabel)}
                    className="is-sm"
                  />
                  <span>{pair.targetLabel}</span>
                  <span className="ex-push ex-num ex-dim">
                    {formatCount(pair.count)}
                  </span>
                </li>
              ))}
            </ul>
          ) : (
            <p className="ex-empty-note">None loaded yet.</p>
          )}
        </Section>
      )}
    </div>
  );
}

// ============ Overview ============

function DatabaseOverview({
  databaseName,
  explorer,
  colorFor,
  onFocusLabel,
  touch,
  booting,
}: InspectorProps) {
  const { state, labels, visible } = explorer;
  const stats = state.overview.stats;
  const loading =
    !stats &&
    (state.overview.status === "loading" ||
      (booting && labels.nodes.length === 0));
  const maxCount = Math.max(
    1,
    ...labels.nodes.map((entry) => entry.count ?? 0),
  );
  const scanning = state.pending.has("scan");
  // The same test as the bar and the sample notice: sampled labels, or labels without a count.
  const atLeast = totalsAreFloors(stats, labels);

  return (
    <div>
      <div className="ex-subject">
        <div className="ex-subject-top">
          <p className="ex-subject-title ex-truncate" title={databaseName}>
            {databaseName}
          </p>
          <ReadOnly />
        </div>
        <dl className="ex-tiles">
          {[
            {
              label: "Nodes",
              value: stats?.nodeTotal ?? null,
              atLeast: atLeast.nodes,
            },
            {
              label: "Edges",
              value: stats?.edgeTotal ?? null,
              atLeast: atLeast.edges,
            },
          ].map((tile) => (
            <div key={tile.label} className="ex-tile">
              <dt>{tile.label}</dt>
              <dd
                className="ex-num"
                title={
                  tile.atLeast && tile.value !== null
                    ? `At least ${formatCount(tile.value)}`
                    : undefined
                }
              >
                {loading ? (
                  <Skeleton width={48} height={16} />
                ) : tile.value === null ? (
                  "—"
                ) : tile.atLeast ? (
                  <>
                    <span aria-hidden="true" className="ex-dim">
                      ≥
                    </span>
                    <span className="hf-sr-only">at least </span>
                    {formatCount(tile.value)}
                  </>
                ) : (
                  formatCount(tile.value)
                )}
              </dd>
            </div>
          ))}
        </dl>
        <p className="ex-num ex-dim">
          On the canvas: {plural(visible.nodes.length, "node")} ·{" "}
          {plural(visible.edges.length, "edge")}
        </p>
        {!loading &&
          stats &&
          (atLeast.nodes ||
            atLeast.edges ||
            scanning ||
            explorer.scanBlockedUntil !== null) && (
            <div className="ex-stack-sm">
              <p className="ex-dim">
                Totals add up the labels found so far, by sampling rather than
                reading the whole database, so they are floors.
              </p>
              <ScanLabelsControl
                scanning={scanning}
                blockedUntil={explorer.scanBlockedUntil}
                onScan={() => void explorer.scanLabels()}
              />
            </div>
          )}
      </div>

      <Section title="Node labels">
        {loading ? (
          <div className="ex-skeletons">
            {Array.from({ length: 4 }).map((_, index) => (
              <Skeleton key={index} height={16} />
            ))}
          </div>
        ) : labels.nodes.length === 0 ? (
          <p className="ex-empty-note">No labels yet.</p>
        ) : (
          <ul className="ex-bars">
            {labels.nodes.map((entry) => (
              <li key={entry.label}>
                <button
                  type="button"
                  onClick={() =>
                    onFocusLabel({ target: "node", label: entry.label })
                  }
                  className="ex-bar-row"
                >
                  <Swatch color={colorFor(entry.label)} />
                  <span className="ex-bar-name ex-truncate">{entry.label}</span>
                  <span
                    aria-hidden="true"
                    className={cn(
                      "ex-bar-track",
                      entry.count !== null && "has-count",
                    )}
                  >
                    {entry.count !== null && (
                      <span
                        className="ex-bar-fill"
                        style={{
                          width: `${Math.max(2, (entry.count / maxCount) * 100)}%`,
                          background: colorFor(entry.label),
                          opacity: 0.6,
                        }}
                      />
                    )}
                  </span>
                  <span className="ex-num ex-dim ex-bar-count">
                    {entry.count === null ? "—" : formatCount(entry.count)}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        )}
      </Section>

      <Section title="Tips">
        <ul className="ex-tips">
          <li>
            {touch ? (
              <span className="ex-dim">Search</span>
            ) : (
              <kbd className={KBD}>/</kbd>
            )}{" "}
            Jump to a node by name, id or text
          </li>
          <li>
            <span className="ex-dim">
              {touch ? "Double-tap" : "Double-click"}
            </span>{" "}
            a node to load its neighbors
          </li>
          <li>
            <span className="ex-dim">
              {touch ? "Long-press" : "Right-click"}
            </span>{" "}
            a node for every action
          </li>
          {!touch && (
            <li>
              <kbd className={KBD}>?</kbd> All keyboard shortcuts
            </li>
          )}
        </ul>
      </Section>
    </div>
  );
}
