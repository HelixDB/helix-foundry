import { useMemo, useState } from "react";
import {
  Eye,
  EyeOff,
  Loader2,
  PanelLeftClose,
  Plus,
  RefreshCw,
  Search,
  X,
} from "lucide-react";

import { Skeleton } from "../../../kit";
import { cn } from "./ui";

import { labelKey } from "./lib/graph-store";
import type { ElementTarget, LabelCount } from "./lib/types";
import {
  ACTIVE_ROW,
  compactCount,
  formatCount,
  SECTION_TITLE,
  SMALL_ICON_BUTTON,
  TEXT_BUTTON,
} from "./ui";

export interface LabelFocus {
  target: ElementTarget;
  label: string;
}

export interface LabelRailProps {
  nodeLabels: LabelCount[];
  edgeLabels: LabelCount[];
  /** Counts are on their way and nothing is known yet: show skeleton rows. */
  loading: boolean;
  /** Counting labels failed, so counts are unknown; offer a retry (the cheap counts, never a scan). */
  countsFailed: boolean;
  countsRetrying: boolean;
  onRetryCounts: () => void;
  loaded: {
    nodes: ReadonlyMap<string, number>;
    edges: ReadonlyMap<string, number>;
  };
  hiddenLabels: ReadonlySet<string>;
  hiddenCount: number;
  focus: LabelFocus | null;
  colorFor: (label: string) => string;
  isLoadingLabel: (label: string) => boolean;
  isLabelExhausted: (label: string) => boolean;
  onFocus: (focus: LabelFocus | null) => void;
  onLoad: (label: string) => void;
  onToggleHidden: (
    target: ElementTarget,
    label: string,
    hidden: boolean,
  ) => void;
  onShowAll: () => void;
  /**
   * Foundry's own labels: the "Show Foundry internals" toggle, and which
   * rows are theirs (listed after the data's, as internal). Omit for none.
   */
  internals?: {
    shown: boolean;
    onShownChange: (shown: boolean) => void;
    isInternal: (target: ElementTarget, label: string) => boolean;
  };
  /** Collapse button in the header (desktop); omitted in sheets, which have their own close. */
  onCollapse?: () => void;
  /** "close" (an X) for sheets and flyouts; "panel" for the docked rail. */
  collapseIcon?: "panel" | "close";
  /** Touch layouts: keep row actions visible instead of revealing them on hover. */
  touch?: boolean;
  className?: string;
}

export function LabelRail({
  nodeLabels,
  edgeLabels,
  loading,
  countsFailed,
  countsRetrying,
  onRetryCounts,
  loaded,
  hiddenLabels,
  hiddenCount,
  focus,
  colorFor,
  isLoadingLabel,
  isLabelExhausted,
  onFocus,
  onLoad,
  onToggleHidden,
  onShowAll,
  internals,
  onCollapse,
  collapseIcon = "panel",
  touch = false,
  className,
}: LabelRailProps) {
  const [filter, setFilter] = useState("");
  const needle = filter.trim().toLowerCase();
  const nodes = useMemo(
    () => filterLabels(nodeLabels, needle),
    [nodeLabels, needle],
  );
  const edges = useMemo(
    () => filterLabels(edgeLabels, needle),
    [edgeLabels, needle],
  );
  const empty = !loading && nodeLabels.length === 0 && edgeLabels.length === 0;

  return (
    <div className={cn("ex-panel", className)}>
      <div className="ex-panel-head">
        <h2 className={SECTION_TITLE}>Labels</h2>
        {onCollapse && (
          <button
            type="button"
            onClick={onCollapse}
            aria-label={
              collapseIcon === "close" ? "Close labels" : "Hide labels panel"
            }
            title={collapseIcon === "close" ? "Close" : "Hide labels panel ([)"}
            className={SMALL_ICON_BUTTON}
          >
            {collapseIcon === "close" ? (
              <X aria-hidden="true" size={16} />
            ) : (
              <PanelLeftClose aria-hidden="true" size={16} />
            )}
          </button>
        )}
      </div>

      <div className="ex-panel-filter">
        <label className="ex-field">
          <span className="hf-sr-only">Filter labels</span>
          <Search aria-hidden="true" size={14} className="ex-field-icon" />
          <input
            value={filter}
            onChange={(event) => setFilter(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Escape" && filter) {
                event.stopPropagation();
                setFilter("");
              }
            }}
            placeholder="Filter labels"
          />
          {filter && (
            <button
              type="button"
              onClick={() => setFilter("")}
              aria-label="Clear filter"
              title="Clear filter"
              className="ex-field-clear"
            >
              <X aria-hidden="true" size={14} />
            </button>
          )}
        </label>
        {internals && (
          <label
            className="ex-check"
            title="Foundry’s own records of what loads read: documents, sections, mentions, evidence and reviews. Turning this on or off reloads the graph."
          >
            <input
              type="checkbox"
              checked={internals.shown}
              onChange={(event) =>
                internals.onShownChange(event.target.checked)
              }
            />
            Show Foundry internals
          </label>
        )}
      </div>

      <div className="ex-panel-body">
        {loading ? (
          <div role="status" className="ex-skeletons">
            <span className="hf-sr-only">Loading labels</span>
            {Array.from({ length: 6 }).map((_, index) => (
              <Skeleton key={index} height={28} />
            ))}
          </div>
        ) : empty ? (
          <p className="ex-empty-note">No labels yet.</p>
        ) : (
          <>
            <LabelGroup
              title="Node labels"
              target="node"
              entries={nodes}
              filtered={needle !== "" && nodes.length === 0}
              loaded={loaded.nodes}
              hiddenLabels={hiddenLabels}
              focus={focus}
              colorFor={colorFor}
              isLoadingLabel={isLoadingLabel}
              isLabelExhausted={isLabelExhausted}
              onFocus={onFocus}
              onLoad={onLoad}
              onToggleHidden={onToggleHidden}
              isInternal={internals?.isInternal}
              touch={touch}
            />
            <LabelGroup
              title="Edge labels"
              target="edge"
              entries={edges}
              filtered={needle !== "" && edges.length === 0}
              loaded={loaded.edges}
              hiddenLabels={hiddenLabels}
              focus={focus}
              colorFor={colorFor}
              isLoadingLabel={isLoadingLabel}
              isLabelExhausted={isLabelExhausted}
              onFocus={onFocus}
              onLoad={onLoad}
              onToggleHidden={onToggleHidden}
              isInternal={internals?.isInternal}
              touch={touch}
            />
          </>
        )}
      </div>

      {(countsFailed || hiddenCount > 0) && (
        <div className="ex-panel-foot">
          {countsFailed && (
            <div className="ex-foot-row is-warning">
              <span>Counts unavailable</span>
              <button
                type="button"
                onClick={onRetryCounts}
                disabled={countsRetrying}
                className={TEXT_BUTTON}
              >
                {countsRetrying ? (
                  <Loader2 aria-hidden="true" size={14} className="hf-spin" />
                ) : (
                  <RefreshCw aria-hidden="true" size={14} />
                )}
                Retry
              </button>
            </div>
          )}
          {hiddenCount > 0 && (
            <div className="ex-foot-row">
              <span className="ex-num">{formatCount(hiddenCount)} hidden</span>
              <button type="button" onClick={onShowAll} className={TEXT_BUTTON}>
                Show all
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function filterLabels(entries: LabelCount[], needle: string): LabelCount[] {
  return needle
    ? entries.filter((entry) => entry.label.toLowerCase().includes(needle))
    : entries;
}

function LabelGroup({
  title,
  target,
  entries,
  filtered,
  loaded,
  hiddenLabels,
  focus,
  colorFor,
  isLoadingLabel,
  isLabelExhausted,
  onFocus,
  onLoad,
  onToggleHidden,
  isInternal,
  touch,
}: {
  title: string;
  target: ElementTarget;
  entries: LabelCount[];
  filtered: boolean;
  loaded: ReadonlyMap<string, number>;
  hiddenLabels: ReadonlySet<string>;
  focus: LabelFocus | null;
  colorFor: (label: string) => string;
  isLoadingLabel: (label: string) => boolean;
  isLabelExhausted: (label: string) => boolean;
  onFocus: (focus: LabelFocus | null) => void;
  onLoad: (label: string) => void;
  onToggleHidden: (
    target: ElementTarget,
    label: string,
    hidden: boolean,
  ) => void;
  isInternal?: (target: ElementTarget, label: string) => boolean;
  touch: boolean;
}) {
  if (entries.length === 0 && !filtered) return null;
  const internal = (entry: LabelCount) =>
    isInternal?.(target, entry.label) ?? false;
  const own = entries.filter((entry) => !internal(entry));
  const foundry = entries.filter(internal);
  const row = (entry: LabelCount) => (
    <LabelRow
      key={entry.label}
      target={target}
      entry={entry}
      loadedCount={loaded.get(entry.label) ?? 0}
      hidden={hiddenLabels.has(labelKey(target, entry.label))}
      selected={focus?.target === target && focus.label === entry.label}
      color={target === "node" ? colorFor(entry.label) : null}
      loading={target === "node" && isLoadingLabel(entry.label)}
      exhausted={target === "node" && isLabelExhausted(entry.label)}
      internal={internal(entry)}
      onFocus={onFocus}
      onLoad={onLoad}
      onToggleHidden={onToggleHidden}
      touch={touch}
    />
  );
  return (
    <section className="ex-label-group">
      <h3>{title}</h3>
      {filtered ? (
        <p className="ex-empty-note">No matches</p>
      ) : (
        <>
          {own.length > 0 && <ul>{own.map(row)}</ul>}
          {foundry.length > 0 && (
            <>
              <h4 className="ex-label-subhead">Foundry internals</h4>
              <ul aria-label={`Foundry internal ${target} labels`}>
                {foundry.map(row)}
              </ul>
            </>
          )}
        </>
      )}
    </section>
  );
}

function LabelRow({
  target,
  entry,
  loadedCount,
  hidden,
  selected,
  color,
  loading,
  exhausted,
  internal,
  onFocus,
  onLoad,
  onToggleHidden,
  touch,
}: {
  target: ElementTarget;
  entry: LabelCount;
  loadedCount: number;
  hidden: boolean;
  selected: boolean;
  color: string | null;
  loading: boolean;
  exhausted: boolean;
  /** One of Foundry's own labels: listed apart and muted. */
  internal: boolean;
  onFocus: (focus: LabelFocus | null) => void;
  onLoad: (label: string) => void;
  onToggleHidden: (
    target: ElementTarget,
    label: string,
    hidden: boolean,
  ) => void;
  touch: boolean;
}) {
  const kind = target === "node" ? "node" : "edge";
  const total = entry.count === null ? "—" : compactCount(entry.count);
  const countTitle = `${formatCount(loadedCount)} loaded · ${
    entry.count === null
      ? "count unknown"
      : `${formatCount(entry.count)} in the database`
  }`;
  const loadTitle = exhausted
    ? `Every ${entry.label} node is loaded`
    : `Load 50 more ${entry.label} nodes (or double-click the row)`;
  const showActions = touch || loading;

  return (
    <li
      className={cn(
        "ex-label-row",
        selected && ACTIVE_ROW,
        showActions && "shows-actions",
        internal && "is-internal",
      )}
    >
      <button
        type="button"
        aria-pressed={selected}
        onClick={() =>
          onFocus(selected ? null : { target, label: entry.label })
        }
        onDoubleClick={() => {
          if (target === "node" && !exhausted) onLoad(entry.label);
        }}
        title={`${entry.label}: ${countTitle}`}
        className={cn("ex-label-main", hidden && "is-hidden")}
      >
        {color ? (
          <span
            aria-hidden="true"
            className="ex-swatch is-round"
            style={{ background: color }}
          />
        ) : (
          <span aria-hidden="true" className="ex-edge-swatch" />
        )}
        <span className="ex-label-name ex-truncate">{entry.label}</span>
        <span className="hf-sr-only">
          {internal ? `Foundry internal ${kind}` : kind} label, {countTitle}
          {hidden ? ", hidden" : ""}
        </span>
        <span aria-hidden="true" className="ex-label-counts ex-num">
          {loadedCount > 0 && <span>{compactCount(loadedCount)}</span>}
          {loadedCount > 0 && <span className="ex-dim">/</span>}
          <span className="ex-dim">{total}</span>
        </span>
      </button>
      <div className="ex-label-actions">
        {target === "node" && (
          <button
            type="button"
            onClick={() => onLoad(entry.label)}
            disabled={loading || exhausted}
            aria-label={loadTitle}
            title={loadTitle}
            className={SMALL_ICON_BUTTON}
          >
            {loading ? (
              <Loader2
                aria-hidden="true"
                size={14}
                className="hf-spin ex-busy"
              />
            ) : (
              <Plus aria-hidden="true" size={14} />
            )}
          </button>
        )}
        <button
          type="button"
          onClick={() => onToggleHidden(target, entry.label, !hidden)}
          aria-label={
            hidden
              ? `Show ${entry.label} on the canvas`
              : `Hide ${entry.label} on the canvas`
          }
          title={hidden ? `Show ${entry.label}` : `Hide ${entry.label}`}
          className={SMALL_ICON_BUTTON}
        >
          {hidden ? (
            <EyeOff aria-hidden="true" size={14} />
          ) : (
            <Eye aria-hidden="true" size={14} />
          )}
        </button>
      </div>
    </li>
  );
}
