import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Check,
  CircleCheck,
  KeyRound,
  LoaderCircle,
  Sparkles,
  TriangleAlert,
  type LucideIcon,
} from "lucide-react";
import { LOCAL } from "../../../shared/deployment";
import type {
  FoundryLoad,
  FoundryOntology,
  GraphJob,
  JobStatus,
  OntologyGeneration,
  OntologySource,
  OntologyVersion,
} from "../../../shared/wfe";
import {
  Button,
  ButtonLink,
  ConfirmDialog,
  StatusBadge,
  cx,
  plural,
  useNow,
  type Status,
} from "../../kit";
import { sentence } from "../../api";
import { paths } from "../../paths";
import { useWorkspace, type WorkspaceRole } from "../../ui";
import { SettingsLinked } from "../workspace/ai-gate";
import { DASHBOARD_URL } from "../workspace/members";
import {
  isLive,
  isRetrying,
  ontologyQueries,
  setCurrentVersion,
} from "./queries";
import { versionFormat } from "./format";
import {
  generationTitle,
  jobBadge,
  lookingUp,
  ruleEvaluationRefusal,
} from "./model";
import { ontologyName, useOntology } from "./scope";

// Pieces several Ontology pages share.

/** Owners and admins manage ontologies (workspace.ontology.manage). */
export const canManage = (role: WorkspaceRole) =>
  role === "owner" || role === "admin";

const statuses: Record<string, Status> = {
  ONTOLOGY_GENERATION_STATUS_QUEUED: "queued",
  ONTOLOGY_GENERATION_STATUS_RUNNING: "running",
  ONTOLOGY_GENERATION_STATUS_SUCCEEDED: "succeeded",
  ONTOLOGY_GENERATION_STATUS_FAILED: "failed",
};
export const generationStatus = (g?: OntologyGeneration | null): Status =>
  statuses[g?.status || ""] || "draft";
export function GenerationBadge({
  generation,
  size,
}: {
  generation?: OntologyGeneration;
  size?: "sm" | "md";
}) {
  const status = generationStatus(generation);
  return (
    <StatusBadge
      status={status}
      size={size}
      label={
        status === "draft"
          ? "Unknown"
          : isRetrying(generation)
            ? "Retrying"
            : undefined
      }
    />
  );
}

/** Why a retrying generation's last attempt failed, and when it runs next. */
export function retryText(g: OntologyGeneration, now: number) {
  const reason = g.error ? sentence(g.error) : "The last attempt failed.",
    minutes = Math.ceil((Date.parse(g.nextAttemptAt || "") - now) / 60_000);
  return `${reason} Next attempt ${
    !(minutes > 0)
      ? "any moment now"
      : minutes === 1
        ? "within a minute"
        : `in ${minutes} minutes`
  }.`;
}

const loadBadges: Record<JobStatus, { status: Status; label: string }> = {
  queued: { status: "queued", label: "Queued" },
  running: { status: "running", label: "Loading" },
  succeeded: { status: "succeeded", label: "Loaded" },
  budget_exhausted: { status: "stale", label: "Stopped at the cap" },
  failed: { status: "failed", label: "Failed" },
  cancelled: { status: "canceled", label: "Cancelled" },
};
/**
 * How an ontology's newest load job went, for its row and its overview.
 * The overview passes its live load (`live`, liveRead of its loads): while
 * that load's job is parked looking people up, it reads queued, and the
 * badge says it is looking people up. A row has
 * only the job's status, which can't tell.
 */
export const lastLoadBadge = (
  o: FoundryOntology,
  live?: { load: FoundryLoad; job: GraphJob },
): { status: Status; label: string } =>
  live && lookingUp(live.job, live.load.enrichment)
    ? jobBadge(live.job, live.load.enrichment)
    : (o.lastLoad?.status && loadBadges[o.lastLoad.status]) || {
        status: "draft",
        label: "Not loaded",
      };

export const sourceLabel = (s?: OntologySource) =>
  s === "ONTOLOGY_SOURCE_MANUAL" ? "Written by hand" : "Generated";

/**
 * What a generation drafts from, for its one-line summary: its synced
 * tables and table files ("6 tables and 1 table file"), else its document
 * sets ("2 document sets").
 */
export function sourcesText(g: OntologyGeneration) {
  const tables = g.tables?.length || 0,
    files = g.documents?.length || 0;
  return tables || files
    ? [
        ...(tables ? [plural(tables, "table")] : []),
        ...(files ? [plural(files, "table file")] : []),
      ].join(" and ")
    : plural(g.documentSetIds?.length || 0, "document set");
}

// A callout card, as helix-foundry's Analyst cards (an-card).
export function Callout({
  icon: Icon,
  title,
  text,
  action,
  tone,
  live,
}: {
  icon: LucideIcon;
  title: ReactNode;
  text?: ReactNode;
  action?: ReactNode;
  tone?: "danger" | "muted";
  live?: boolean;
}) {
  return (
    <div
      className={cx("on-card", tone && `is-${tone}`)}
      role={tone === "danger" ? "alert" : "status"}
    >
      <span className="on-card-icon" aria-hidden>
        <Icon size={16} className={live ? "hf-spin" : undefined} />
      </span>
      <div className="on-card-text">
        <strong>{title}</strong>
        {text && <p>{text}</p>}
      </div>
      {action}
    </div>
  );
}

/**
 * One step of a step-by-step flow: the new-ontology wizard's and an
 * unfinished ontology's overview's. Done, it is
 * a line of what it chose (`summary`), with Edit when `onEdit` is given;
 * current or running, it is open on `children`; upcoming, it says what it
 * asks.
 */
export function FlowStep({
  number,
  title,
  text,
  optional,
  state,
  summary,
  onEdit,
  children,
}: {
  number: number;
  title: string;
  text: string;
  optional?: boolean;
  state: "done" | "current" | "running" | "upcoming";
  summary?: string;
  onEdit?: () => void;
  children?: ReactNode;
}) {
  const done = state === "done",
    open = state === "current" || state === "running";
  return (
    <li
      className={cx("on-wizard-step", `is-${state}`)}
      aria-current={open ? "step" : undefined}
    >
      <div className="on-wizard-head">
        <span className="on-wizard-number" aria-hidden>
          {done ? (
            <Check size={12} />
          ) : state === "running" ? (
            <LoaderCircle size={12} className="hf-spin" />
          ) : (
            number
          )}
        </span>
        <span className="on-wizard-text">
          <strong>
            {title}
            {optional && <small>Optional</small>}
            {done && <span className="hf-sr-only"> (done)</span>}
            {state === "running" && (
              <span className="hf-sr-only"> (in progress)</span>
            )}
          </strong>
          <small>{done ? summary : text}</small>
        </span>
        {done && onEdit && (
          <Button
            size="sm"
            variant="ghost"
            aria-label={`Edit ${title}`}
            onClick={onEdit}
          >
            Edit
          </Button>
        )}
      </div>
      {open && <div className="on-wizard-body">{children}</div>}
    </li>
  );
}

/** The ontology's queued or running generation, if any, as a callout. */
export function LiveGeneration() {
  const { id } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    qc = useQueryClient();
  const list = useQuery(ontologyQueries.generations(id, o));
  const live = list.data?.generations?.find(isLive);
  const now = useNow();
  // When it ends, its version (or its failure) shows up on this page.
  const seen = useRef("");
  useEffect(() => {
    if (live) seen.current = live.id;
    else if (seen.current) {
      seen.current = "";
      void qc.invalidateQueries({ queryKey: [id, "ontology", o] });
    }
  }, [live?.id]);
  if (!live) return null;
  if (live.revision)
    return (
      <Callout
        icon={LoaderCircle}
        live
        title={
          live.revision.dryRun ? "Checking edits" : "Creating an edited version"
        }
        text={
          isRetrying(live)
            ? retryText(live, now)
            : "Your current version stays as it is."
        }
        action={
          <ButtonLink size="sm" to={paths.generation(o, live.id)}>
            View progress
          </ButtonLink>
        }
      />
    );
  const title = generationTitle(live, "");
  return (
    <Callout
      icon={LoaderCircle}
      live
      title={
        live.status === "ONTOLOGY_GENERATION_STATUS_RUNNING"
          ? "Drafting a new ontology"
          : isRetrying(live)
            ? "Retrying a new ontology"
            : "A new ontology is queued"
      }
      text={`From ${sourcesText(live)}${title ? `, for “${title}”` : ""}. ${
        isRetrying(live)
          ? retryText(live, now)
          : ontology.currentVersionId
            ? "Your current version stays as it is."
            : "You review the draft before it is used."
      }`}
      action={
        <ButtonLink size="sm" to={paths.generation(o, live.id)}>
          View progress
        </ButtonLink>
      }
    />
  );
}

/** Points the ontology at `version`, from the current version the page saw. */
export function MakeCurrent({
  version,
  currentId,
  variant = "primary",
  size,
}: {
  version: OntologyVersion;
  // The current version's id, as loaded; "" for none.
  currentId: string;
  variant?: "primary" | "secondary";
  size?: "sm" | "md";
}) {
  const { id, role, notify } = useWorkspace(),
    { id: o } = useOntology(),
    qc = useQueryClient();
  const [confirming, setConfirming] = useState(false),
    [pending, setPending] = useState(false);
  if (!canManage(role) || version.isCurrent) return null;
  // Lists omit definitions. Open the immutable version to check its rules,
  // instead of fetching a potentially large definition for every list row.
  if (versionFormat(version) === "agent_bundle" && !version.definition)
    return (
      <ButtonLink
        to={paths.version(o, version.id)}
        variant={variant}
        size={size}
      >
        Review version
      </ButtonLink>
    );
  const refusal = ruleEvaluationRefusal(version.definition);
  if (refusal) return <p className="on-muted">{refusal}</p>;
  const run = async () => {
    setPending(true);
    try {
      await setCurrentVersion(id, o, {
        versionId: version.id,
        expectedCurrentVersionId: currentId,
      });
      notify(`Version ${version.version} is now current.`);
      setConfirming(false);
    } catch (e) {
      // A stale pointer (someone moved it) says so; the reload shows why.
      const message = (e as Error).message;
      notify(message.charAt(0).toUpperCase() + message.slice(1), true);
      setConfirming(false);
    } finally {
      setPending(false);
      await qc.invalidateQueries({ queryKey: [id, "ontology", o] });
    }
  };
  return (
    <>
      <Button
        variant={variant}
        size={size}
        icon={CircleCheck}
        onClick={() => setConfirming(true)}
      >
        Make current
      </Button>
      {confirming && (
        <ConfirmDialog
          title={`Make version ${version.version} current?`}
          description={
            currentId
              ? "The current version is replaced in this ontology. You can switch back at any time."
              : "The ontology starts using this version."
          }
          confirmLabel="Make current"
          pending={pending}
          onConfirm={run}
          onCancel={() => setConfirming(false)}
        />
      )}
    </>
  );
}

/** "Generate" in a page header, for those who may. */
export function GenerateButton({
  variant = "primary",
}: {
  variant?: "primary" | "secondary";
}) {
  const { role } = useWorkspace(),
    { id: o } = useOntology();
  if (!canManage(role)) return null;
  return (
    <ButtonLink variant={variant} icon={Sparkles} to={paths.generate(o)}>
      Generate
    </ButtonLink>
  );
}

/** A link to a version, by number. */
export function VersionLink({ id, version }: { id: string; version?: number }) {
  const { id: o } = useOntology();
  return <Link to={paths.version(o, id)}>Version {version ?? "?"}</Link>;
}

/**
 * What the new-ontology wizard leaves on the page it lands on, the
 * generation's or, when the draft didn't start, the ontology's: that it
 * created the database (whose first key nobody was shown), and why the
 * draft didn't start. What it asked for stays in this tab for the
 * overview's setup (./setup.tsx), which carries on from it.
 */
export type Arrival = {
  newDatabase?: boolean;
  generateFailed?: { message: string };
};

/** The wizard's notes (Arrival) as callouts, until dismissed. */
export function ArrivalNotes() {
  const { slug } = useWorkspace(),
    { ontology } = useOntology(),
    navigate = useNavigate();
  const arrival = (useLocation().state || {}) as Arrival,
    failed = arrival.generateFailed;
  const dismiss = (key: keyof Arrival) =>
    navigate(".", { replace: true, state: { ...arrival, [key]: undefined } });
  return (
    <>
      {failed && (
        <Callout
          icon={TriangleAlert}
          tone="danger"
          title={`${ontologyName(ontology)} is created, but its draft didn’t start`}
          text={
            <SettingsLinked
              text={`${sentence(failed.message)} What you chose is kept below: generate it again from there.`}
            />
          }
          action={
            <Button
              size="sm"
              variant="ghost"
              onClick={() => dismiss("generateFailed")}
            >
              Dismiss
            </Button>
          }
        />
      )}
      {/* A local build's database keys are never served, and it has no
          dashboard to revoke them in, so it says nothing. */}
      {!LOCAL && arrival.newDatabase && (
        <Callout
          icon={KeyRound}
          tone="muted"
          title={`Helix gave ${ontology.databaseName || "the new database"} a first API key`}
          text="Foundry doesn’t keep it: it reads the database with your session. If nothing else will use the key, revoke it in the Helix dashboard."
          action={
            <span className="on-card-actions">
              {ontology.projectId && ontology.tenantId && (
                <a
                  className="hf-btn hf-btn--secondary hf-btn--sm"
                  href={`${DASHBOARD_URL}/${encodeURIComponent(slug)}/${encodeURIComponent(ontology.projectId)}/database/${encodeURIComponent(ontology.tenantId)}`}
                  target="_blank"
                  rel="noreferrer"
                >
                  Open its keys
                </a>
              )}
              <Button
                size="sm"
                variant="ghost"
                onClick={() => dismiss("newDatabase")}
              >
                Dismiss
              </Button>
            </span>
          }
        />
      )}
    </>
  );
}
