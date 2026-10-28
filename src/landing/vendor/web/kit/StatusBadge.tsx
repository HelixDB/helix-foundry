import type { ReactNode } from "react";
import type { LucideIcon } from "lucide-react";
import { cx } from "./util";

export type Status =
  | "healthy"
  | "syncing"
  | "running"
  | "failed"
  | "paused"
  | "stale"
  | "queued"
  | "draft"
  | "succeeded"
  | "canceled"
  | "review";
export type Tone =
  | "neutral"
  | "accent"
  | "success"
  | "warning"
  | "danger"
  | "info";

const meta: Record<Status, { label: string; tone: Tone; live?: boolean }> = {
  healthy: { label: "Healthy", tone: "success" },
  succeeded: { label: "Succeeded", tone: "success" },
  syncing: { label: "Syncing", tone: "info", live: true },
  running: { label: "Running", tone: "info", live: true },
  queued: { label: "Queued", tone: "neutral" },
  failed: { label: "Failed", tone: "danger" },
  stale: { label: "Stale", tone: "warning" },
  review: { label: "Needs review", tone: "warning" },
  paused: { label: "Paused", tone: "neutral" },
  canceled: { label: "Canceled", tone: "neutral" },
  draft: { label: "Draft", tone: "neutral" },
};
export const statusTone = (s: Status) => meta[s].tone;
export const statusLabel = (s: Status) => meta[s].label;
export const isLive = (s: Status) => !!meta[s].live || s === "queued";

// Maps raw job, run, proposal and sync states onto badge statuses.
export function toStatus(raw?: string | null): Status {
  switch (raw) {
    case "queued":
    case "waiting":
      return "queued";
    case "running":
    case "starting":
    case "publishing":
      return "running";
    case "checking":
    case "syncing":
    case "live":
      return "syncing";
    case "succeeded":
    case "published":
    case "complete":
      return "succeeded";
    case "ready":
    case "healthy":
      return "healthy";
    case "awaiting_review":
      return "review";
    case "failed":
    case "error":
    case "retrying":
      return "failed";
    case "canceled":
    case "cancelled":
    case "rejected":
      return "canceled";
    case "paused":
    case "stale":
    case "draft":
      return raw;
    default:
      return "draft";
  }
}

export function StatusBadge({
  status,
  label,
  size = "md",
  className,
}: {
  status: Status;
  // Overrides the default label ("Healthy", "Failed", ...).
  label?: ReactNode;
  size?: "sm" | "md";
  className?: string;
}) {
  const m = meta[status];
  return (
    <span
      className={cx(
        "hf-status",
        `hf-status--${status}`,
        `hf-tone--${m.tone}`,
        size === "sm" && "hf-status--sm",
        className,
      )}
    >
      <span className={cx("hf-status-dot", m.live && "is-live")} aria-hidden />
      {label ?? m.label}
    </span>
  );
}
// Just the colored dot, with an accessible label.
export function StatusDot({
  status,
  label,
}: {
  status: Status;
  label?: string;
}) {
  const m = meta[status];
  return (
    <span
      className={cx("hf-status-dot", `hf-tone--${m.tone}`, m.live && "is-live")}
      role="img"
      aria-label={label ?? m.label}
      title={label ?? m.label}
    />
  );
}
// A neutral or toned pill for categories and counts.
export function Tag({
  tone = "neutral",
  icon: Icon,
  children,
  className,
}: {
  tone?: Tone;
  icon?: LucideIcon;
  children: ReactNode;
  className?: string;
}) {
  return (
    <span className={cx("hf-tag", `hf-tone--${tone}`, className)}>
      {Icon && <Icon size={12} aria-hidden />}
      {children}
    </span>
  );
}
