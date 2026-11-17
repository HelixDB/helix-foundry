import "./automations.css";
import { Link } from "react-router-dom";
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
  longText,
  nodeText,
  parseSpec,
  type Kind,
  type Names,
  type StepState,
  type Template,
  type TextLength,
  TEMPLATES,
} from "./model";
import { automationPaths as paths } from "./paths";
import type { useNames } from "./queries";

// What the automations' pages share: how an automation, a run and a step
// show their state, each kind's icon, and the steps as they will run
// unattended, which an owner reads before approving them (the builder's
// review, an unapproved automation's page, Founder's card).

/** An automation's state, as a dot and its words. */
export function automationStatus(a: FoundryAutomation): {
  dot: Status;
  label: string;
} {
  switch (a.status) {
    case "active":
      return { dot: "healthy", label: "Live" };
    case "paused":
      return a.pausedReason === "failures"
        ? { dot: "failed", label: "Paused after failures" }
        : a.pausedReason === "owner_lost_access"
          ? { dot: "failed", label: "Paused" }
          : { dot: "paused", label: "Paused" };
    default:
      return { dot: "review", label: "Needs approval" };
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

/**
 * A step in words, as a heading: its first letter upper case. Brief, its
 * instruction or subject is cut to a line (model.ts brief()).
 */
export const stepTitle = (
  n: AutomationNode,
  names: Names,
  email: boolean,
  length: TextLength = "full",
): string => {
  const t = nodeText(n, names, email, length);
  return t.charAt(0).toUpperCase() + t.slice(1);
};

/**
 * The steps as they run unattended, in the order a run takes them: each in
 * a line (its instruction or subject cut short, model.ts brief()), the
 * whole of a longer one in a block of its own that opens (`showAll` opens
 * them all at first: what an owner approves), a Founder step with what it
 * may do without asking (else that anything it prepares waits for its
 * owner), a condition with where its yes and no lead. With `onSelect`, a
 * step's line opens it.
 */
export function StepsPreview({
  spec,
  names,
  email,
  label = "Steps",
  showAll = false,
  onSelect,
}: {
  spec: AutomationSpec;
  names: Names;
  email: boolean;
  label?: string;
  showAll?: boolean;
  onSelect?: (id: string) => void;
}) {
  const read = parseSpec(spec);
  const order = read.ok ? read.order : spec.nodes.map((n) => n.id);
  const byId = new Map(spec.nodes.map((n) => [n.id, n]));
  return (
    <ol className="au-preview" aria-label={label}>
      {order.map((id) => {
        const n = byId.get(id)!,
          Icon = nodeIcon(n),
          title = stepTitle(n, names, email, "brief"),
          whole = longText(n);
        const leads = (branch: "true" | "false") => {
          const to = edgesOutOf(spec, n.id).find((e) => e.branch === branch);
          const next = to && byId.get(to.to);
          return next ? stepTitle(next, names, email, "brief") : "stop";
        };
        return (
          <li key={id} className="au-preview-step">
            <span className="au-preview-icon" aria-hidden>
              <Icon size={14} />
            </span>
            <div className="au-preview-body">
              {onSelect ? (
                <button
                  type="button"
                  className="au-preview-title au-preview-open"
                  title="Open this step"
                  onClick={() => onSelect(id)}
                >
                  {title}
                </button>
              ) : (
                <span className="au-preview-title">{title}</span>
              )}
              {whole && (
                <details className="au-preview-full" open={showAll}>
                  <summary>
                    {n.kind === "assistant"
                      ? "Its whole instruction"
                      : "Its whole subject"}
                  </summary>
                  <p>{whole}</p>
                </details>
              )}
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
                  When met: {leads("true")}. Otherwise: {leads("false")}.
                </span>
              )}
            </div>
          </li>
        );
      })}
    </ol>
  );
}

const NEEDS: Record<Template["needs"][number], string> = {
  connector: "a connection",
  ontology: "an ontology",
  set: "a document set",
};

/** The templates a new automation starts from on the canvas, each saying what it needs that the workspace lacks. */
export function Templates({ lists }: { lists: ReturnType<typeof useNames> }) {
  const has = {
    connector: !!lists.connectors.length,
    ontology: !!lists.ontologies.length,
    set: !!lists.sets.length,
  };
  return (
    <ul className="au-templates" aria-label="Templates">
      {TEMPLATES.map((t) => {
        const lacks = t.needs.filter((n) => !has[n]);
        return (
          <li key={t.id}>
            <Link className="au-template" to={paths.newAutomation(t.id)}>
              <strong>{t.title}</strong>
              <span>{t.description}</span>
              {lists.ready && !!lacks.length && (
                <small>
                  Needs {lacks.map((n) => NEEDS[n]).join(" and ")}, which this
                  workspace hasn’t yet.
                </small>
              )}
            </Link>
          </li>
        );
      })}
    </ul>
  );
}
