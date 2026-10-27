import type { ReactNode } from "react";
import {
  AlertTriangle,
  ImageDown,
  Info,
  Loader2,
  Maximize,
  Minimize2,
  Minus,
  Plus,
  RotateCcw,
  Scan,
  Waypoints,
  X,
} from "lucide-react";

import { cn, formatCount, ICON_BUTTON, plural, TEXT_BUTTON } from "./ui";

/** Top-left: what's on the canvas, with Clear and a way back from hiding. */
export function CanvasChip({
  nodes,
  edges,
  hidden,
  onShowAll,
  onClear,
  more,
}: {
  nodes: number;
  edges: number;
  hidden: number;
  onShowAll: () => void;
  onClear: () => void;
  /** A load's first view: read the next ring around its outermost nodes. */
  more?: { onShowMore: () => void; pending: boolean };
}) {
  return (
    <div className="ex-chip">
      <span className="ex-num">
        {plural(nodes, "node")} · {plural(edges, "edge")}
      </span>
      {hidden > 0 && (
        <>
          <span aria-hidden="true" className="ex-sep">
            |
          </span>
          <button
            type="button"
            onClick={onShowAll}
            className={TEXT_BUTTON}
            title="Show everything you hid"
          >
            {formatCount(hidden)} hidden · show
          </button>
        </>
      )}
      {more && (
        <>
          <span aria-hidden="true" className="ex-sep">
            |
          </span>
          <button
            type="button"
            onClick={more.onShowMore}
            disabled={more.pending}
            className={TEXT_BUTTON}
            title="Read the next ring of neighbors around the outermost nodes"
          >
            {more.pending ? "Showing more…" : "Show more"}
          </button>
        </>
      )}
      {nodes + hidden > 0 && (
        <>
          <span aria-hidden="true" className="ex-sep">
            |
          </span>
          <button
            type="button"
            onClick={onClear}
            className={TEXT_BUTTON}
            title="Remove everything from the canvas"
          >
            Clear
          </button>
        </>
      )}
    </div>
  );
}

export interface CanvasNotice {
  id: string;
  tone: "info" | "warning" | "error";
  message: ReactNode;
  action?: { label: string; onClick: () => void };
}

export function CanvasNotices({
  notices,
  onDismiss,
}: {
  notices: CanvasNotice[];
  onDismiss: (id: string) => void;
}) {
  if (notices.length === 0) return null;
  return (
    <div className="ex-notices" aria-live="polite">
      {notices.map((notice) => {
        const Icon = notice.tone === "info" ? Info : AlertTriangle;
        return (
          <div
            key={notice.id}
            role={notice.tone === "error" ? "alert" : "status"}
            className={cn("ex-notice", `is-${notice.tone}`)}
          >
            <Icon aria-hidden="true" size={14} />
            <span className="ex-notice-text">
              {notice.message}
              {notice.action && (
                <>
                  {" "}
                  <button
                    type="button"
                    onClick={notice.action.onClick}
                    className={cn(TEXT_BUTTON, "is-current")}
                  >
                    {notice.action.label}
                  </button>
                </>
              )}
            </span>
            <button
              type="button"
              onClick={() => onDismiss(notice.id)}
              aria-label="Dismiss"
              title="Dismiss"
              className="ex-notice-close"
            >
              <X aria-hidden="true" size={14} />
            </button>
          </div>
        );
      })}
    </div>
  );
}

/** Top-right: something is loading in the background. */
export function ActivityIndicator({ label }: { label: string | null }) {
  if (!label) return null;
  return (
    <div role="status" className="ex-chip ex-activity">
      <Loader2 aria-hidden="true" size={14} className="hf-spin ex-busy" />
      <span className="ex-truncate">{label}</span>
    </div>
  );
}

function ToolbarButton({
  label,
  onClick,
  children,
  disabled,
}: {
  label: string;
  onClick: () => void;
  children: ReactNode;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      title={label}
      disabled={disabled}
      className={ICON_BUTTON}
    >
      {children}
    </button>
  );
}

/** Bottom-left view controls. */
export function CanvasToolbar({
  onZoomIn,
  onZoomOut,
  onFit,
  onRelayout,
  onResetView,
  onExport,
  onToggleFullscreen,
  fullscreen,
  fullscreenAvailable,
  disabled,
  className,
}: {
  onZoomIn: () => void;
  onZoomOut: () => void;
  onFit: () => void;
  onRelayout: () => void;
  onResetView: () => void;
  onExport: () => void;
  onToggleFullscreen: () => void;
  fullscreen: boolean;
  fullscreenAvailable: boolean;
  disabled: boolean;
  className?: string;
}) {
  return (
    <div className={className}>
      <div role="toolbar" aria-label="Canvas controls" className="ex-toolbar">
        <ToolbarButton
          label="Zoom in (+)"
          onClick={onZoomIn}
          disabled={disabled}
        >
          <Plus aria-hidden="true" size={16} />
        </ToolbarButton>
        <ToolbarButton
          label="Zoom out (−)"
          onClick={onZoomOut}
          disabled={disabled}
        >
          <Minus aria-hidden="true" size={16} />
        </ToolbarButton>
        <ToolbarButton
          label="Fit to view (F)"
          onClick={onFit}
          disabled={disabled}
        >
          <Scan aria-hidden="true" size={16} />
        </ToolbarButton>
        <ToolbarButton
          label="Re-run layout (R)"
          onClick={onRelayout}
          disabled={disabled}
        >
          <Waypoints aria-hidden="true" size={16} />
        </ToolbarButton>
        <ToolbarButton
          label="Reset zoom (0)"
          onClick={onResetView}
          disabled={disabled}
        >
          <RotateCcw aria-hidden="true" size={16} />
        </ToolbarButton>
        <span aria-hidden="true" className="ex-toolbar-sep" />
        <ToolbarButton
          label="Export PNG"
          onClick={onExport}
          disabled={disabled}
        >
          <ImageDown aria-hidden="true" size={16} />
        </ToolbarButton>
        {fullscreenAvailable && (
          <ToolbarButton
            label={fullscreen ? "Exit full screen" : "Full screen"}
            onClick={onToggleFullscreen}
          >
            {fullscreen ? (
              <Minimize2 aria-hidden="true" size={16} />
            ) : (
              <Maximize aria-hidden="true" size={16} />
            )}
          </ToolbarButton>
        )}
      </div>
    </div>
  );
}
