// Friendly wording for failed dock runs. The transport has already sorted the
// four error layers (papi, WFE, gateway, database) into a kind plus the stable
// code; this maps them to a title and a next step. The raw code and message
// are always shown next to it, so nothing here needs to be exhaustive.
//
// A write that fails after it may have reached the database must never be
// described as safe to run again: running it twice can apply it twice.

import { queriesPaused, type DatabaseQueryResult } from "../../database-query";

import type { QueryMode } from "./types";

export type QueryFailure = Extract<DatabaseQueryResult, { ok: false }>;

export interface FailureDescription {
  title: string;
  hint: string | null;
  /** amber: transient or fixable by waiting/adjusting; red: hard failure; neutral: user cancelled. */
  tone: "red" | "amber" | "neutral";
}

const DATABASE_CODES: Record<string, FailureDescription> = {
  query_timeout: {
    title: "Query took too long",
    hint: "Lower the limit or scope the scan to one label.",
    tone: "amber",
  },
  rate_limited: {
    title: "Rate limited",
    hint: "Wait a moment and run it again.",
    tone: "amber",
  },
  payload_too_large: {
    title: "Request over 2 MiB",
    hint: "Send fewer ids or smaller parameters.",
    tone: "red",
  },
  transaction_conflict: {
    title: "Transaction conflict",
    hint: "Another write touched the same data. Reload what you need and run it again.",
    tone: "amber",
  },
  index_not_found: {
    title: "Missing index",
    hint: "Search needs an index on that label and property. Create it first (see the Create text index template).",
    tone: "red",
  },
  invalid_query_json: {
    title: "The database rejected the request",
    hint: "Check operation names, field shapes and parameter types.",
    tone: "red",
  },
  invalid_query: {
    title: "Invalid query",
    hint: "Check that every parameter the query uses is set, with the right type.",
    tone: "red",
  },
  invalid_batch_arity: {
    title: "Empty batch",
    hint: "Add at least one entry.",
    tone: "red",
  },
  internal_planner_error: {
    title: "The planner rejected the query",
    hint: "Check that every name in returns matches an entry.",
    tone: "red",
  },
  tenant_disabled: {
    title: "Queries are paused for billing",
    hint: "Add credit or update the plan to resume queries.",
    tone: "amber",
  },
  // The transport's message names the code ("... (backend_unavailable)"), and
  // surfaces show it next to these, so the hints don't repeat it.
  backend_unavailable: {
    title: "Database unavailable",
    hint: "Its gateway can't reach it; it may be starting or failing over. This is a platform problem, not your query. Try again in a minute.",
    tone: "amber",
  },
  rate_limit_unavailable: {
    title: "The database's gateway is refusing queries",
    hint: "This is a platform problem, not your query. Try again in a few minutes, and contact support if it keeps happening.",
    tone: "amber",
  },
  FAILOVER_WAIT_UNAVAILABLE: {
    title: "Write not applied",
    hint: "The database was failing over. Try again shortly.",
    tone: "amber",
  },
  WRITE_OUTCOME_UNKNOWN: {
    title: "Write outcome unknown",
    hint: "The write may or may not have been applied. Check the data before running it again.",
    tone: "red",
  },
};

const WRITE_OUTCOME_UNKNOWN = DATABASE_CODES.WRITE_OUTCOME_UNKNOWN;

/**
 * Write failures where nothing says whether the write was applied: the
 * database or gateway said so (`retryable: false` is only sent when the commit
 * outcome is unknown), the connection failed, the broker or a proxy failed or
 * timed out (it may have forwarded the write first), or the broker answered
 * 2xx with a body that couldn't be read.
 */
export function isWriteOutcomeUnknown(failure: QueryFailure): boolean {
  // The transport flags these itself; the checks below cover failures it didn't flag.
  if (
    failure.outcomeUnknown ||
    failure.retryable === false ||
    failure.code === "WRITE_OUTCOME_UNKNOWN"
  )
    return true;
  switch (failure.kind) {
    case "network":
    case "malformed":
      return true;
    case "unavailable":
      // 405 is papi refusing the route: the write never left the dashboard.
      return failure.status >= 500;
    case "database":
      // A 5xx without the database's own error code came from something in between.
      return failure.status >= 500 && failure.code === null;
    default:
      return false;
  }
}

export function describeFailure(
  failure: QueryFailure,
  mode: QueryMode,
): FailureDescription {
  if (
    mode === "write" &&
    failure.kind !== "aborted" &&
    isWriteOutcomeUnknown(failure)
  ) {
    if (failure.kind === "network") {
      return {
        ...WRITE_OUTCOME_UNKNOWN,
        hint: "The connection failed, so the write may or may not have been applied. Check the data before running it again.",
      };
    }
    return WRITE_OUTCOME_UNKNOWN;
  }
  switch (failure.kind) {
    case "starting":
      return {
        title: "Your new database is starting",
        hint: "Helix is still getting it ready, which can take a few minutes after it’s created. Try again shortly.",
        tone: "neutral",
      };
    case "aborted":
      return mode === "write"
        ? {
            title: "Write cancelled",
            hint: "The database may still apply it. Check the data before running it again.",
            tone: "amber",
          }
        : { title: "Query cancelled", hint: null, tone: "neutral" };
    case "network":
      return {
        title: "Couldn't reach Helix",
        hint: "Check your connection and run it again.",
        tone: "red",
      };
    case "unauthenticated":
      return {
        title: "Your session ended",
        hint: "Sign in again to keep querying.",
        tone: "red",
      };
    case "forbidden":
      return {
        title: "You don't have query access to this database",
        hint: `Ask a workspace admin for database.query.${mode} on this project.`,
        tone: "red",
      };
    case "not_found":
      return {
        title: "Database not found",
        hint: "It may have been deleted.",
        tone: "red",
      };
    case "invalid":
      return {
        title: "The control plane rejected the request",
        hint: null,
        tone: "red",
      };
    case "disabled":
      return queriesPaused(failure) === "no_plan"
        ? {
            title:
              "Queries are paused: this workspace has no active Helix plan",
            hint: "Choose a plan in the Helix dashboard to resume them.",
            tone: "amber",
          }
        : DATABASE_CODES.tenant_disabled;
    case "too_large":
      if (mode === "write" && failure.status !== 413) {
        return {
          title: "Request or response too large",
          hint: "If the response was the part over the limit, the write already ran. Check the data before running it again, and return less.",
          tone: "amber",
        };
      }
      return {
        title: "Request or response too large",
        hint: "Responses are capped at about 4 MB. Add a limit or return fewer properties.",
        tone: "amber",
      };
    case "unavailable":
      if (failure.status === 429) {
        return {
          title: "Rate limited",
          hint: "Helix is limiting how many queries this database takes. Wait a moment, then run it again.",
          tone: "amber",
        };
      }
      if (failure.status === 504) {
        return {
          title: "Query timed out",
          hint: "Helix gave up waiting for the database. Add a limit or narrow the label; if small queries time out too, the database is overloaded.",
          tone: "amber",
        };
      }
      return {
        title: "Database unavailable",
        hint: "Try again shortly.",
        tone: "amber",
      };
    case "malformed":
      return {
        title: "Unexpected response",
        hint: "The query service answered in a shape the dashboard doesn't understand.",
        tone: "red",
      };
    case "database":
      break;
  }
  if (failure.code && DATABASE_CODES[failure.code])
    return DATABASE_CODES[failure.code];
  if (failure.status === 401 || failure.status === 403) {
    // Inner 401/403: the gateway rejected the broker's key, a platform fault.
    return {
      title: "The database refused the control plane",
      hint: "This is on Helix's side. Try again shortly.",
      tone: "red",
    };
  }
  if (failure.status >= 500) {
    return {
      title: "The database failed to run the query",
      hint: null,
      tone: "red",
    };
  }
  return { title: "Query failed", hint: null, tone: "red" };
}
