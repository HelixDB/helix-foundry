import type { ReactNode } from "react";
import { Database, Keyboard, Loader2, Lock, RefreshCw } from "lucide-react";

import { formatCount, ICON_BUTTON } from "./ui";

export interface ExplorerBarProps {
  databaseName: string;
  /** The search box. */
  search: ReactNode;
  /**
   * Shown on the canvas now, and in the database when known. `atLeast` marks
   * a total that is a floor (labels found by sampling, not a full scan).
   */
  stats: {
    nodes: number;
    edges: number;
    totalNodes: number | null;
    totalEdges: number | null;
    atLeast?: { nodes: boolean; edges: boolean };
  } | null;
  onRefresh: () => void;
  refreshing: boolean;
  onShortcuts: () => void;
  /** Tablet: the button that opens the labels sheet. */
  leading?: ReactNode;
}

function countText(
  loaded: number,
  total: number | null,
  noun: string,
  atLeast = false,
): string {
  const unit = `${noun}${(total ?? loaded) === 1 ? "" : "s"}`;
  if (total === null || (total === loaded && !atLeast))
    return `${formatCount(loaded)} ${unit}`;
  // A floor below what is loaded says nothing useful.
  if (atLeast && total <= loaded) return `${formatCount(loaded)} ${unit}`;
  return `${formatCount(loaded)} of ${atLeast ? "≥" : ""}${formatCount(total)} ${unit}`;
}

/** What the canvas holds, and reload and shortcuts: the end of either bar. */
function Actions({
  stats,
  onRefresh,
  refreshing,
  onShortcuts,
}: Pick<
  ExplorerBarProps,
  "stats" | "onRefresh" | "refreshing" | "onShortcuts"
>) {
  return (
    <>
      {stats && (
        <span className="ex-bar-stats" aria-live="polite">
          {countText(
            stats.nodes,
            stats.totalNodes,
            "node",
            stats.atLeast?.nodes,
          )}{" "}
          ·{" "}
          {countText(
            stats.edges,
            stats.totalEdges,
            "edge",
            stats.atLeast?.edges,
          )}
        </span>
      )}
      <button
        type="button"
        onClick={onRefresh}
        disabled={refreshing}
        aria-label="Reload from the database"
        title="Reload from the database (clears the canvas)"
        className={ICON_BUTTON}
      >
        {refreshing ? (
          <Loader2 aria-hidden="true" size={16} className="hf-spin" />
        ) : (
          <RefreshCw aria-hidden="true" size={16} />
        )}
      </button>
      <button
        type="button"
        onClick={onShortcuts}
        aria-label="Keyboard shortcuts"
        title="Keyboard shortcuts (?)"
        className={`${ICON_BUTTON} ex-wide-only`}
      >
        <Keyboard aria-hidden="true" size={16} />
      </button>
    </>
  );
}

/** Foundry never writes from the browser: said beside the database's name. */
export const ReadOnly = () => (
  <span
    className="ex-pill"
    title="Exploring is read only: queries run with your Helix permissions, which reads need."
  >
    <Lock aria-hidden="true" size={13} />
    Read only
  </span>
);

/**
 * The explorer's own bar, as one side of a compare: the database, the
 * search, what the canvas holds, and reload and shortcuts.
 */
export function ExplorerBar({
  databaseName,
  search,
  leading,
  ...actions
}: ExplorerBarProps) {
  return (
    <div className="ex-bar">
      <div className="ex-bar-side">
        <span className="ex-bar-db" title={`Database ${databaseName}`}>
          <Database aria-hidden="true" size={15} />
          <span className="ex-truncate">{databaseName}</span>
        </span>
        {leading}
      </div>
      <div className="ex-bar-search">{search}</div>
      <div className="ex-bar-side is-end">
        <Actions {...actions} />
      </div>
    </div>
  );
}

/**
 * The same tools in the page's bar, when the explorer is the page: the
 * search, what the canvas holds, reload and shortcuts, then `trailing` (the
 * inspector's toggle). The database is the overview's.
 */
export function ExplorerTools({
  search,
  trailing,
  ...actions
}: Omit<ExplorerBarProps, "databaseName" | "leading"> & {
  trailing?: ReactNode;
}) {
  return (
    <>
      <div className="ex-top-search">{search}</div>
      <Actions {...actions} />
      {trailing}
    </>
  );
}
