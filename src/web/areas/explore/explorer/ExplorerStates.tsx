import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import {
  ArrowUpRight,
  Ban,
  CreditCard,
  DatabaseZap,
  Loader2,
  Lock,
  RefreshCw,
  Waypoints,
  WifiOff,
} from "lucide-react";

import { cn } from "./ui";

import { queriesPaused } from "../database-query";
import type { QueryFailure } from "./lib/graph-store";
import type { LabelCount } from "./lib/types";
import { describeFailure } from "./query/failure";
import { compactCount, MONO, OUTLINE_BUTTON, PRIMARY_BUTTON } from "./ui";

/** The canvas while the first sample loads. */
export function CanvasLoading({
  databaseName,
  step,
}: {
  databaseName: string;
  step: string;
}) {
  return (
    <div role="status" className="ex-loading">
      <Loader2 aria-hidden="true" size={20} className="hf-spin ex-busy" />
      <p>
        {step === "sample"
          ? `Loading a sample of ${databaseName}…`
          : `Reading ${databaseName}’s labels…`}
      </p>
    </div>
  );
}

function StateCard({
  icon,
  tone = "neutral",
  title,
  children,
  actions,
  detail,
  className,
  status = false,
}: {
  icon: ReactNode;
  tone?: "neutral" | "red" | "amber";
  title: string;
  children?: ReactNode;
  actions?: ReactNode;
  detail?: ReactNode;
  className?: string;
  /** Announced as a status: something the explorer is waiting on. */
  status?: boolean;
}) {
  return (
    <div
      role={tone === "red" ? "alert" : status ? "status" : undefined}
      className={cn("ex-state", `is-${tone}`, className)}
    >
      <span className="ex-state-icon" aria-hidden="true">
        {icon}
      </span>
      <h2>{title}</h2>
      {children && <div className="ex-state-text">{children}</div>}
      {actions && <div className="ex-state-actions">{actions}</div>}
      {detail}
    </div>
  );
}

function FailureDetail({ failure }: { failure: QueryFailure }) {
  return (
    <p className={cn(MONO, "ex-state-detail")}>
      {[failure.status || null, failure.code ?? failure.kind]
        .filter(Boolean)
        .join(" · ")}
      {failure.message ? ` — ${failure.message}` : ""}
    </p>
  );
}

export function NoAccessState({
  databaseName,
  membersHref,
  failure,
}: {
  databaseName: string;
  membersHref?: string;
  failure?: QueryFailure | null;
}) {
  return (
    <StateCard
      icon={<Lock size={22} />}
      tone="red"
      title="You don’t have read access to this database"
      actions={
        membersHref ? (
          <a
            href={membersHref}
            target="_blank"
            rel="noreferrer"
            className={OUTLINE_BUTTON}
          >
            Members in the Helix dashboard
            <ArrowUpRight aria-hidden="true" size={14} />
          </a>
        ) : null
      }
      detail={failure ? <FailureDetail failure={failure} /> : null}
    >
      Exploring {databaseName} runs read queries, which need Query read on its
      project. Ask a workspace owner or admin to grant it.
    </StateCard>
  );
}

/**
 * The workspace's Helix billing paused the database's queries: why, and the
 * dashboard's billing (Choose a plan when the workspace has none). No Retry:
 * reading again changes nothing until the plan does.
 */
export function BillingPausedState({
  billingHref,
  failure,
}: {
  billingHref?: string;
  failure: QueryFailure;
}) {
  const noPlan = queriesPaused(failure) === "no_plan";
  return (
    <StateCard
      icon={<CreditCard size={22} />}
      tone="amber"
      title={
        noPlan
          ? "Queries are paused: this workspace has no active Helix plan"
          : "Queries are paused for billing"
      }
      actions={
        billingHref ? (
          <a
            href={billingHref}
            target="_blank"
            rel="noreferrer"
            className={PRIMARY_BUTTON}
          >
            {noPlan ? "Choose a plan" : "Open Helix billing"}
            <ArrowUpRight aria-hidden="true" size={14} />
          </a>
        ) : null
      }
      detail={<FailureDetail failure={failure} />}
    >
      {noPlan
        ? "Helix runs a database’s queries only while its workspace has a plan. Choose one in the Helix dashboard, then reload the explorer."
        : "The workspace’s Helix plan has paused this database’s queries. Add credit or change the plan, then reload the explorer."}
    </StateCard>
  );
}

function isTimeout(failure: QueryFailure): boolean {
  return (
    (failure.kind === "unavailable" && failure.status === 504) ||
    (failure.kind === "database" &&
      (failure.code === "query_timeout" ||
        failure.code === "query_deadline_exceeded" ||
        failure.status === 408))
  );
}

function isTooLarge(failure: QueryFailure): boolean {
  return (
    failure.kind === "too_large" ||
    (failure.kind === "database" &&
      (failure.code === "payload_too_large" || failure.status === 413))
  );
}

/**
 * The first view failed. At the sample, size and time failures offer a
 * smaller sample. At the labels step (a few cheap requests: discovery and
 * bitmap counts), a failure says the database or its gateway is in trouble,
 * so the card says that and offers only a retry.
 */
export function LoadFailedState({
  failure,
  stage = "sample",
  onRetry,
  onSmaller,
}: {
  failure: QueryFailure;
  stage?: "labels" | "sample";
  onRetry: () => void;
  onSmaller: () => void;
}) {
  const description = describeFailure(failure, "read");
  const timeout = isTimeout(failure);
  if (
    stage === "labels" &&
    failure.kind !== "unauthenticated" &&
    failure.kind !== "network"
  ) {
    return (
      <StateCard
        icon={<RefreshCw size={22} />}
        tone={description.tone === "amber" ? "amber" : "red"}
        title={
          timeout ? "The database didn’t answer in time" : description.title
        }
        actions={
          <button type="button" onClick={onRetry} className={OUTLINE_BUTTON}>
            <RefreshCw aria-hidden="true" size={14} />
            Retry
          </button>
        }
        detail={<FailureDetail failure={failure} />}
      >
        {timeout
          ? "Even finding and counting labels timed out, and those requests are small, so the database is likely overloaded or unavailable. The explorer stopped instead of adding load. Try again in a minute."
          : (description.hint ??
            "The explorer stopped instead of retrying. Try again in a minute.")}
      </StateCard>
    );
  }
  const sizeOrTime = timeout || isTooLarge(failure);
  const icon =
    failure.kind === "network" ? (
      <WifiOff size={22} />
    ) : failure.kind === "not_found" ? (
      <Ban size={22} />
    ) : (
      <RefreshCw size={22} />
    );
  return (
    <StateCard
      icon={icon}
      tone={description.tone === "amber" ? "amber" : "red"}
      title={
        timeout
          ? "The first view took too long"
          : sizeOrTime
            ? "The first view was too big to load"
            : description.title
      }
      actions={
        failure.kind === "unauthenticated" ? (
          <Link to="/login" className={PRIMARY_BUTTON}>
            Sign in again
          </Link>
        ) : (
          <>
            {sizeOrTime && (
              <button
                type="button"
                onClick={onSmaller}
                className={PRIMARY_BUTTON}
              >
                Load a smaller sample
              </button>
            )}
            <button type="button" onClick={onRetry} className={OUTLINE_BUTTON}>
              <RefreshCw aria-hidden="true" size={14} />
              Retry
            </button>
          </>
        )
      }
      detail={<FailureDetail failure={failure} />}
    >
      {sizeOrTime
        ? "A smaller sample usually works. From there, load labels and expand nodes as you go."
        : description.hint}
    </StateCard>
  );
}

/**
 * The database is new and its gateway doesn't take reads yet: the explorer
 * boots again by itself, and keeps this on screen while it does.
 */
export function StartingState({
  databaseName,
  onRetry,
}: {
  databaseName: string;
  onRetry: () => void;
}) {
  return (
    <StateCard
      icon={<Loader2 size={22} className="hf-spin" />}
      title="Your new database is starting"
      status
      actions={
        <button type="button" onClick={onRetry} className={OUTLINE_BUTTON}>
          <RefreshCw aria-hidden="true" size={14} />
          Retry
        </button>
      }
    >
      Helix is still getting {databaseName} ready, which can take a few minutes
      after it’s created. The explorer reads it again by itself.
    </StateCard>
  );
}

/** The database holds nothing yet: a load fills it. */
export function EmptyDatabaseState({
  databaseName,
  loadHref,
  internalsHidden = false,
  other = false,
}: {
  databaseName: string;
  loadHref?: string;
  /** Foundry's own records are left out, so the database may hold those. */
  internalsHidden?: boolean;
  /** Other data: what no load wrote, so the loads' data is elsewhere. */
  other?: boolean;
}) {
  if (other)
    return (
      <StateCard icon={<Waypoints size={22} />} title="No other data">
        Everything in {databaseName} was written by a load. Pick a load to see
        it.
      </StateCard>
    );
  return (
    <StateCard
      icon={<Waypoints size={22} />}
      title="Nothing in the database yet"
      actions={
        loadHref ? (
          <Link to={loadHref} className={PRIMARY_BUTTON}>
            <DatabaseZap aria-hidden="true" size={14} />
            Load the ontology
          </Link>
        ) : null
      }
    >
      {databaseName} has {internalsHidden ? "no entities" : "no nodes"}. Load
      the ontology’s current version to write its tables and documents into it.
      {internalsHidden &&
        " Foundry’s own records stay hidden until Show Foundry internals, under Labels, lists them."}
    </StateCard>
  );
}

/** The database has data but the canvas is empty (after Clear, or everything hidden). */
export function EmptyCanvasState({
  databaseName,
  labels,
  colorFor,
  everythingHidden,
  onShowAll,
  onLoadSample,
  loadingSample,
  onLoadLabel,
}: {
  databaseName: string;
  labels: LabelCount[];
  colorFor: (label: string) => string;
  everythingHidden: boolean;
  onShowAll: () => void;
  onLoadSample: () => void;
  loadingSample: boolean;
  onLoadLabel: (label: string) => void;
}) {
  if (everythingHidden) {
    return (
      <StateCard
        icon={<Waypoints size={22} />}
        title="Everything is hidden"
        actions={
          <button type="button" onClick={onShowAll} className={PRIMARY_BUTTON}>
            Show all
          </button>
        }
      >
        The loaded nodes are hidden by label or one by one.
      </StateCard>
    );
  }
  return (
    <StateCard
      icon={<Waypoints size={22} />}
      title={`Start exploring ${databaseName}`}
      actions={
        <button
          type="button"
          onClick={onLoadSample}
          disabled={loadingSample}
          className={PRIMARY_BUTTON}
        >
          {loadingSample && (
            <Loader2 aria-hidden="true" size={14} className="hf-spin" />
          )}
          Load a sample
        </button>
      }
      detail={
        labels.length > 0 ? (
          <div className="ex-state-labels">
            {labels.slice(0, 12).map((entry) => (
              <button
                key={entry.label}
                type="button"
                onClick={() => onLoadLabel(entry.label)}
                title={`Load 50 ${entry.label} nodes`}
                className="ex-label-chip"
              >
                <span
                  aria-hidden="true"
                  className="ex-swatch"
                  style={{ background: colorFor(entry.label) }}
                />
                {entry.label}
                {entry.count !== null && (
                  <span className="ex-num ex-dim">
                    {compactCount(entry.count)}
                  </span>
                )}
              </button>
            ))}
          </div>
        ) : null
      }
    >
      Pick a label, or jump to a node with the search.
    </StateCard>
  );
}
