import { Fragment, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { CircleAlert, Play, RefreshCw, X, Zap } from "lucide-react";
import type { FoundryAgentAction } from "../../../shared/wfe";
import { sentence } from "../../api";
import { Button, StatusBadge, formatDate, useNow } from "../../kit";
import { useWorkspace } from "../../ui";
import { dashboardPaths } from "../dashboards/paths";
import { dashboardQueries } from "../dashboards/queries";
import {
  cardState,
  cardView,
  parseCard,
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
 * words of its operation (cardView), what Foundry names it acts on, what it
 * costs where it costs, a warning when the person never named that
 * themselves, and the whole request it runs. Only
 * the person whose thread it is sees it (threads are private), and Run is
 * theirs alone: the WFE answers anyone else NOT_FOUND. Run waits while the
 * thread's turn is live (`busy`); an expired confirmation offers Refresh.
 */
export function ActionCard({
  action,
  busy,
  onNavigate,
}: {
  action: FoundryAgentAction;
  busy: boolean;
  onNavigate?: () => void;
}) {
  const { id: ws } = useWorkspace(),
    qc = useQueryClient(),
    navigate = useNavigate(),
    { pathname } = useLocation();
  const now = useNow(5000),
    state = cardState(action, now),
    card = parseCard(action.operation || "", action.displayJson),
    view = card && cardView(card);
  const [pending, setPending] = useState<"run" | "refresh" | "dismiss" | null>(
      null,
    ),
    [error, setError] = useState("");
  const settle = () =>
    qc.invalidateQueries({
      queryKey: [ws, "agent", "thread", action.threadId],
    });
  // A dashboard action ran: every page and list showing dashboards reads
  // them again, and a deleted one's page, if open, gives way to the list.
  const ranOnDashboards = (ran: FoundryAgentAction) => {
    const id =
      card && "dashboard" in card.display ? card.display.dashboard.id : "";
    if (ran.status === "succeeded" && action.operation === "delete_dashboard") {
      qc.removeQueries({
        queryKey: dashboardQueries.dashboard(ws, id).queryKey,
      });
      if (pathname === dashboardPaths.dashboard(id))
        navigate(dashboardPaths.dashboards(), { replace: true });
    }
    void qc.invalidateQueries({
      queryKey: dashboardQueries.list(ws).queryKey,
    });
    if (id)
      void qc.invalidateQueries({
        queryKey: dashboardQueries.dashboard(ws, id).queryKey,
      });
  };
  const act = async (which: "run" | "refresh" | "dismiss") => {
    setPending(which);
    setError("");
    try {
      if (which === "run") {
        const ran = await executeAgentAction(ws, action);
        if (action.operation?.endsWith("_dashboard")) ranOnDashboards(ran.action);
        // Where the Assistant runs, as the dock and the settings show it.
        if (
          action.operation === "set_assistant_runtime" ||
          action.operation === "revoke_assistant_helper"
        )
          void qc.invalidateQueries({ queryKey: [ws, "agent", "runtime"] });
      } else if (which === "refresh") await refreshAgentAction(ws, action.id);
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
  const title = view
    ? view.title
    : `An action this page can’t show (${action.operation || "unnamed"})`;
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
            {view ? `“${view.target}”` : "this target"}. Check it is the one you
            mean.
          </span>
        </p>
      )}
      {view && (
        <dl className="ag-card-facts">
          {view.facts.map((f) => (
            <Fragment key={f.label}>
              <dt>{f.label}</dt>
              <dd>
                {"value" in f
                  ? f.value
                  : f.entities.map((e, i) => (
                      <Fragment key={i}>
                        {i > 0 && ", "}
                        {e.to ? (
                          <Link
                            className="ag-link"
                            to={e.to}
                            onClick={onNavigate}
                          >
                            {e.text}
                          </Link>
                        ) : (
                          e.text
                        )}
                      </Fragment>
                    ))}
              </dd>
            </Fragment>
          ))}
        </dl>
      )}
      {view?.quote && (
        <blockquote className="ag-card-quote">
          <span className="ag-label">The document says (its words)</span>
          <q>{view.quote}</q>
        </blockquote>
      )}
      {view && (
        <p className="ag-card-text">
          {view.does}
          {view.cost && ` ${view.cost}`}
        </p>
      )}
      {view?.caution && state.kind === "waiting" && (
        <p className="ag-card-warn" role="note">
          <CircleAlert size={14} aria-hidden />
          <span>{view.caution}</span>
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
              {cardView(state.card).run}
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
              : `Nothing runs until you click ${cardView(state.card).run}. Expires ${formatDate(action.expiresAt)}.`}
          </p>
        </>
      )}
      {(state.kind === "expired" || state.kind === "unreadable") && (
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
          <Link className="ag-link" to={link.to} onClick={onNavigate}>
            {link.label}
          </Link>
        </p>
      )}
      {state.kind === "ended" &&
        state.status !== "succeeded" &&
        action.error && (
          <p className="ag-card-error">{sentence(action.error)}</p>
        )}
      {state.kind === "ended" && state.status === "unknown" && (
        <p className="ag-card-muted">
          Check what it was to change before asking again: it is never run
          twice.
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
