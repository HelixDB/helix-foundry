import { useState } from "react";
import { Link } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { CircleAlert, Play, RefreshCw, X, Zap } from "lucide-react";
import type { FoundryAgentAction } from "../../../shared/wfe";
import { sentence } from "../../api";
import { Button, StatusBadge, formatDate, useNow } from "../../kit";
import { useWorkspace } from "../../ui";
import {
  OPERATIONS,
  cardState,
  isAgentOperation,
  parseDisplay,
  payloadText,
  resultLink,
  type CardState,
} from "./model";
import {
  dismissAgentAction,
  executeAgentAction,
  refreshAgentAction,
} from "./queries";

const ENDED: Record<
  Extract<CardState, { kind: "ended" }>["status"],
  { badge: "succeeded" | "failed" | "canceled"; label: string }
> = {
  succeeded: { badge: "succeeded", label: "Ran" },
  failed: { badge: "failed", label: "Failed" },
  unknown: { badge: "failed", label: "Outcome unknown" },
  dismissed: { badge: "canceled", label: "Dismissed" },
};

/**
 * An action the Assistant prepared, as its person decides on it: the fixed
 * words of its operation, what Foundry names it acts on, a warning when the
 * person never named that themselves, and the whole request it runs. Only
 * the person whose thread it is sees it (threads are private), and Run is
 * theirs alone: the WFE answers anyone else NOT_FOUND. Run waits while the
 * thread's turn is live (`busy`); an expired confirmation offers Refresh.
 */
export function ActionCard({
  action,
  busy,
}: {
  action: FoundryAgentAction;
  busy: boolean;
}) {
  const { id: ws } = useWorkspace(),
    qc = useQueryClient();
  const now = useNow(5000),
    state = cardState(action, now),
    display = parseDisplay(action.displayJson),
    copy = isAgentOperation(action.operation || "")
      ? OPERATIONS[action.operation as keyof typeof OPERATIONS]
      : undefined;
  const [pending, setPending] = useState<"run" | "refresh" | "dismiss" | null>(
      null,
    ),
    [error, setError] = useState("");
  const settle = () =>
    qc.invalidateQueries({
      queryKey: [ws, "agent", "thread", action.threadId],
    });
  const act = async (which: "run" | "refresh" | "dismiss") => {
    setPending(which);
    setError("");
    try {
      if (which === "run") await executeAgentAction(ws, action);
      else if (which === "refresh") await refreshAgentAction(ws, action.id);
      else await dismissAgentAction(ws, action);
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      // The card as the WFE now holds it: ran, refreshed, or why it wasn't.
      await settle();
      setPending(null);
    }
  };
  const link = resultLink(action);
  const title = copy
    ? copy.title(display)
    : `An action this page can’t run (${action.operation || "unnamed"})`;
  return (
    <section className="ag-card" aria-label={`Action: ${title}`}>
      <header className="ag-card-head">
        <Zap size={15} aria-hidden className="ag-card-icon" />
        <strong className="ag-card-title">{title}</strong>
        {state.kind === "ended" && (
          <StatusBadge
            size="sm"
            status={ENDED[state.status].badge}
            label={ENDED[state.status].label}
          />
        )}
        {state.kind === "running" && (
          <StatusBadge size="sm" status="running" label="Running" />
        )}
        {state.kind === "expired" && (
          <StatusBadge size="sm" status="stale" label="Expired" />
        )}
      </header>
      {!action.targetNamedByUser && (
        <p className="ag-card-warn" role="note">
          <CircleAlert size={14} aria-hidden />
          <span>
            Not named by you: the Assistant chose{" "}
            {display.ontology_name
              ? `“${display.ontology_name}”`
              : "this target"}
            . Check it is the one you mean.
          </span>
        </p>
      )}
      <dl className="ag-card-facts">
        {display.ontology_name && (
          <>
            <dt>Ontology</dt>
            <dd>{display.ontology_name}</dd>
          </>
        )}
        {display.version && (
          <>
            <dt>Version</dt>
            <dd>v{display.version}</dd>
          </>
        )}
      </dl>
      {copy && (
        <p className="ag-card-text">
          {copy.does} {copy.cost}
        </p>
      )}
      <details className="ag-disclosure">
        <summary>Full request</summary>
        <dl className="ag-card-facts">
          <dt>Operation</dt>
          <dd>
            <code>{action.operation}</code>
          </dd>
          <dt>Target</dt>
          <dd>
            <code>{action.target}</code>
          </dd>
        </dl>
        <pre className="ag-code">{payloadText(action)}</pre>
        <p className="ag-card-muted">
          Payload hash <code>{action.payloadHash}</code>
        </p>
      </details>
      {state.kind === "waiting" && (
        <>
          <div className="ag-card-actions">
            <Button
              variant="primary"
              size="sm"
              icon={Play}
              pending={pending === "run"}
              disabled={busy || !!pending}
              onClick={() => void act("run")}
            >
              {OPERATIONS[state.operation].run}
            </Button>
            <Button
              variant="ghost"
              size="sm"
              icon={X}
              pending={pending === "dismiss"}
              disabled={!!pending}
              onClick={() => void act("dismiss")}
            >
              Dismiss
            </Button>
          </div>
          <p className="ag-card-muted">
            {busy
              ? "You can run it once the Assistant has finished."
              : `Nothing runs until you click ${OPERATIONS[state.operation].run}. Expires ${formatDate(action.expiresAt)}.`}
          </p>
        </>
      )}
      {(state.kind === "expired" || state.kind === "unknown_operation") && (
        <div className="ag-card-actions">
          {state.kind === "expired" && (
            <Button
              size="sm"
              icon={RefreshCw}
              pending={pending === "refresh"}
              disabled={!!pending}
              onClick={() => void act("refresh")}
            >
              Refresh
            </Button>
          )}
          <Button
            variant="ghost"
            size="sm"
            icon={X}
            pending={pending === "dismiss"}
            disabled={!!pending}
            onClick={() => void act("dismiss")}
          >
            Dismiss
          </Button>
        </div>
      )}
      {state.kind === "expired" && (
        <p className="ag-card-muted">
          Its confirmation expired. Refresh prepares the same request again.
        </p>
      )}
      {state.kind === "ended" && state.status === "succeeded" && link && (
        <p className="ag-card-muted">
          <Link to={link}>Open the load</Link>
        </p>
      )}
      {state.kind === "ended" &&
        state.status !== "succeeded" &&
        action.error && (
          <p className="ag-card-error">{sentence(action.error)}</p>
        )}
      {state.kind === "ended" && state.status === "unknown" && (
        <p className="ag-card-muted">
          Check the ontology before asking again: it is never run twice.
        </p>
      )}
      {error && (
        <p className="ag-card-error" role="alert">
          {error}
        </p>
      )}
    </section>
  );
}
