import "./automations.css";
import {
  CalendarClock,
  GitBranch,
  Mail,
  Play,
  Sparkles,
  Zap,
  type LucideIcon,
} from "lucide-react";
import type {
  AutomationNode,
  AutomationRunStatus,
  AutomationSpec,
  FoundryAutomation,
} from "../../../shared/wfe";
import type { Status } from "../../kit";
import {
  actionText,
  edgesOutOf,
  nodeText,
  parseSpec,
  type Kind,
  type Names,
  type StepState,
} from "./model";

// What the automations' pages share: how an automation, a run and a step
// show their state, each kind's icon, and the steps as they will run
// unattended, which an owner reads before approving them (the builder's
// review, an unapproved automation's page, the Assistant's card).

/** The browser's time zone: a schedule's default, and left out of its words. */
export const here = () => Intl.DateTimeFormat().resolvedOptions().timeZone;

/** An automation's state, as a dot and its words. */
export function automationStatus(a: FoundryAutomation): {
  dot: Status;
  label: string;
} {
  switch (a.status) {
    case "active":
      return { dot: "healthy", label: "On" };
    case "paused":
      return a.pausedReason === "failures" ||
        a.pausedReason === "owner_lost_access"
        ? { dot: "failed", label: "Paused" }
        : { dot: "paused", label: "Paused" };
    default:
      return { dot: "review", label: "Waits for approval" };
  }
}

/** A run's state, as a badge. */
export const RUN_STATUS: Record<
  AutomationRunStatus,
  { status: Status; label: string }
> = {
  queued: { status: "queued", label: "Queued" },
  running: { status: "running", label: "Running" },
  succeeded: { status: "succeeded", label: "Succeeded" },
  failed: { status: "failed", label: "Failed" },
  cancelled: { status: "canceled", label: "Cancelled" },
  skipped: { status: "paused", label: "Skipped" },
};

/** A step's state in a run, as a badge. */
export const STEP_STATUS: Record<StepState, { status: Status; label: string }> =
  {
    running: { status: "running", label: "Running" },
    succeeded: { status: "succeeded", label: "Done" },
    failed: { status: "failed", label: "Failed" },
    skipped: { status: "paused", label: "Not taken" },
    blocked: { status: "canceled", label: "Not run" },
    waiting: { status: "queued", label: "Not reached" },
  };

export const KIND_ICONS: Record<Kind, LucideIcon> = {
  trigger: Zap,
  assistant: Sparkles,
  action: Play,
  condition: GitBranch,
  notify: Mail,
};
/** A step's icon: a schedule's is a clock. */
export const nodeIcon = (n: AutomationNode): LucideIcon =>
  n.kind === "trigger" && n.config.kind === "schedule"
    ? CalendarClock
    : KIND_ICONS[n.kind];

/** A step in words, as a heading: its first letter upper case. */
export const stepTitle = (
  n: AutomationNode,
  names: Names,
  email: boolean,
): string => {
  const t = nodeText(n, names, email, here());
  return t.charAt(0).toUpperCase() + t.slice(1);
};

/**
 * The steps as they run unattended, in the order a run takes them: each in
 * words, an Assistant step with what it may do without asking (else that
 * anything it prepares waits for its owner), a condition with where its
 * yes and no lead. What an owner approves.
 */
export function StepsPreview({
  spec,
  names,
  email,
  label = "Steps",
}: {
  spec: AutomationSpec;
  names: Names;
  email: boolean;
  label?: string;
}) {
  const read = parseSpec(spec);
  const order = read.ok ? read.order : spec.nodes.map((n) => n.id);
  const byId = new Map(spec.nodes.map((n) => [n.id, n]));
  return (
    <ol className="au-preview" aria-label={label}>
      {order.map((id) => {
        const n = byId.get(id)!,
          Icon = nodeIcon(n);
        const leads = (branch: "true" | "false") => {
          const to = edgesOutOf(spec, n.id).find((e) => e.branch === branch);
          const next = to && byId.get(to.to);
          return next ? stepTitle(next, names, email) : "stop";
        };
        return (
          <li key={id} className="au-preview-step">
            <span className="au-preview-icon" aria-hidden>
              <Icon size={14} />
            </span>
            <div className="au-preview-body">
              <span className="au-preview-title">
                {stepTitle(n, names, email)}
              </span>
              {n.kind === "assistant" &&
                (n.config.allow?.length ? (
                  <span className="au-preview-note">
                    May do without asking you:{" "}
                    {n.config.allow.map((a) => actionText(a, names)).join("; ")}
                    . Anything else it prepares waits for you.
                  </span>
                ) : (
                  <span className="au-preview-note">
                    Changes nothing without asking you: what it prepares waits
                    for you on the run’s page.
                  </span>
                ))}
              {n.kind === "action" && (
                <span className="au-preview-note">
                  Runs without asking you, as you.
                </span>
              )}
              {n.kind === "condition" && (
                <span className="au-preview-note">
                  Yes: {leads("true")}. No: {leads("false")}.
                </span>
              )}
            </div>
          </li>
        );
      })}
    </ol>
  );
}
