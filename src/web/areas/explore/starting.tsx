import { useQueryClient, type QueryKey } from "@tanstack/react-query";
import { CreditCard, LoaderCircle } from "lucide-react";
import { Button, ButtonAnchor, EmptyState } from "../../kit";
import { useWorkspace } from "../../ui";
import { Callout } from "../ontology/parts";
import { useOntology } from "../ontology/scope";
import { DASHBOARD_URL } from "../workspace/members";
import { queriesPaused, type DatabaseQueryFailure } from "./database-query";

// What a read of an ontology's database shows in place of its error while
// the database is still starting (startingOf in ./reads.ts): calm words and
// a Retry, while the query retries by itself (readOptions). A callout for a
// page or a panel; inline for a line within one. The explorer has its own
// card in its canvas (explorer/ExplorerStates.tsx StartingState). And while
// the workspace's Helix billing pauses its queries (pausedOf), why, with the
// dashboard's billing and no Retry (the explorer's: BillingPausedState).

export function DatabaseStarting({
  queryKey,
  inline = false,
}: {
  /** The starting read's query. */
  queryKey: QueryKey;
  inline?: boolean;
}) {
  const { ontology } = useOntology();
  const client = useQueryClient();
  const name = ontology.databaseName || "the database";
  // The query is waiting to retry: a refetch would only join that wait, so
  // Retry starts it over and it reads now.
  const retry = (
    <Button
      size="sm"
      variant={inline ? "ghost" : "secondary"}
      onClick={() => void client.resetQueries({ queryKey, exact: true })}
    >
      Retry
    </Button>
  );
  if (inline)
    return (
      <p role="status" className="on-muted">
        The new database is still starting; this reads it again by itself.{" "}
        {retry}
      </p>
    );
  return (
    <Callout
      icon={LoaderCircle}
      live
      title="Your new database is starting"
      text={`Helix is still getting ${name} ready, which can take a few minutes after it’s created. This reads it again by itself.`}
      action={retry}
    />
  );
}

export function DatabasePaused({ failure }: { failure: DatabaseQueryFailure }) {
  const { slug } = useWorkspace();
  const noPlan = queriesPaused(failure) === "no_plan";
  return (
    <EmptyState
      icon={CreditCard}
      title={
        noPlan
          ? "Queries are paused: this workspace has no active Helix plan"
          : "Queries are paused for billing"
      }
      text={
        noPlan
          ? "Helix runs a database’s queries only while its workspace has a plan. Choose one in the Helix dashboard, then come back."
          : failure.message
      }
      action={
        <ButtonAnchor
          variant="primary"
          icon={CreditCard}
          href={`${DASHBOARD_URL}/${encodeURIComponent(slug)}/billing`}
          target="_blank"
          rel="noreferrer"
        >
          {noPlan ? "Choose a plan" : "Open Helix billing"}
        </ButtonAnchor>
      }
    />
  );
}
