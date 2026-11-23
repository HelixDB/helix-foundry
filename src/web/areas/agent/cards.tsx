import "./agent.css";
import { Fragment, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  CircleAlert,
  LoaderCircle,
  Play,
  RefreshCw,
  Undo2,
  X,
  Zap,
} from "lucide-react";
import type { FoundryAgentAction } from "../../../shared/wfe";
import { LOCAL } from "../../../shared/deployment";
import { isRecord } from "../../../shared/json";
import { ApiError, sentence } from "../../api";
import { Button, ButtonLink, StatusBadge, formatDate, useNow } from "../../kit";
import { useWorkspace } from "../../ui";
import { automationPaths } from "../automations/paths";
import { parseSpec } from "../automations/model";
import { StepsPreview } from "../automations/parts";
import { announceSettled, settledDecision } from "../reviews/settled";
import { useNames } from "../automations/queries";
import { dashboardPaths } from "../dashboards/paths";
import { dashboardQueries } from "../dashboards/queries";
import {
  autoState,
  cardState,
  cardView,
  madeDashboard,
  parseCard,
  payloadText,
  resultLink,
  type AutoState,
  type CardState,
  type CardView,
} from "./model";
import {
  dismissAgentAction,
  executeAgentAction,
  refreshAgentAction,
  undoAgentDashboard,
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

const AUTO_BADGE: Record<
  AutoState,
  {
    badge: "succeeded" | "failed" | "canceled" | "running";
    label: string;
  }
> = {
  making: { badge: "running", label: "Making" },
  made: { badge: "succeeded", label: "Made" },
  undone: { badge: "canceled", label: "Undone" },
  failed: { badge: "failed", label: "Failed" },
  unknown: { badge: "failed", label: "Outcome unknown" },
};

/**
 * An action of Founder's in its thread: one it prepared, which runs only
 * when its person clicks (PreparedCard), or one it ran at once (`auto`, a
 * new dashboard the person's own message asked for), which says what it
 * made and offers Undo (AutoCard).
 */
export function ActionCard(props: {
  action: FoundryAgentAction;
  // It ran at once, without a click: its turn's card event says so.
  auto?: boolean;
  busy: boolean;
  onNavigate?: () => void;
}) {
  return props.auto ? <AutoCard {...props} /> : <PreparedCard {...props} />;
}

/**
 * An action Founder prepared, as its person decides on it: the fixed
 * words of its operation (cardView), what Foundry names it acts on, what it
 * costs where it costs, a warning when the person never named that
 * themselves, and the whole request it runs. Only
 * the person whose thread it is sees it (threads are private), and Run is
 * theirs alone: the WFE answers anyone else NOT_FOUND. Run waits while the
 * thread's turn is live (`busy`); an expired confirmation offers Refresh.
 */
function PreparedCard({
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
  // An automation action ran: its lists and pages read again, and a
  // deleted one's page, if open, gives way to the list.
  const ranOnAutomations = (ran: FoundryAgentAction) => {
    const id =
      card && "automation" in card.display ? card.display.automation.id : "";
    if (
      ran.status === "succeeded" &&
      action.operation === "delete_workflow" &&
      id &&
      pathname.startsWith(automationPaths.automation(id))
    )
      navigate(automationPaths.automations(), { replace: true });
    void qc.invalidateQueries({ queryKey: [ws, "automations"] });
  };
  // A review was settled: the review walk, if open, takes the person's own
  // decision as its choice (reviews/settled.ts), and its cards and
  // decisions are read again.
  const ranOnReview = (ran: FoundryAgentAction) => {
    const display = card && "review" in card.display ? card.display : undefined;
    const decision = settledDecision(ran.resultJson);
    if (!display || !("ontology" in display) || ran.status !== "succeeded")
      return;
    const o = display.ontology.id;
    if (decision)
      announceSettled({
        workspaceId: ws,
        ontologyId: o,
        reviewId: display.review.review_key,
        decision,
      });
    for (const key of ["review-cards", "review-decided", "review-decisions"])
      void qc.invalidateQueries({ queryKey: [ws, "ontology", o, key] });
  };
  const act = async (which: "run" | "refresh" | "dismiss") => {
    setPending(which);
    setError("");
    try {
      if (which === "run") {
        const ran = await executeAgentAction(ws, action);
        if (action.operation?.endsWith("_dashboard"))
          ranOnDashboards(ran.action);
        if (action.operation?.endsWith("_workflow"))
          ranOnAutomations(ran.action);
        if (action.operation === "settle_review") ranOnReview(ran.action);
        // Where Founder runs, as the dock and the settings show it.
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
            Not named by you: Founder chose{" "}
            {view ? `“${view.target}”` : "this target"}. Check it is the one you
            mean.
          </span>
        </p>
      )}
      {view && <Facts facts={view.facts} onNavigate={onNavigate} />}
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
      {(action.operation === "create_workflow" ||
        action.operation === "update_workflow") && (
        <AutomationProposal payloadJson={action.payloadJson || ""} />
      )}
      <FullRequest action={action} />
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
              ? "You can run it once Founder has finished."
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

/**
 * An action Founder ran at once, without a click: a new dashboard the
 * person's own message asked for, made as an approved one is (as them,
 * unpinned and unpublished). It says what was made and opens it; Undo
 * deletes it with the dashboards' own DELETE, once the turn has finished.
 * A dashboard gone since (undone here, or deleted elsewhere: its read is
 * NOT_FOUND) reads as undone.
 */
function AutoCard({
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
  const card = parseCard(action.operation || "", action.displayJson),
    view = card && cardView(card, true),
    made = madeDashboard(action),
    link = resultLink(action);
  const [pending, setPending] = useState(false),
    [undone, setUndone] = useState(false),
    [error, setError] = useState("");
  const read = useQuery({
    ...dashboardQueries.dashboard(ws, made || ""),
    enabled: action.status === "succeeded" && !!made && !undone,
    retry: false,
  });
  const state = autoState(
    action,
    undone || (read.error instanceof ApiError && read.error.status === 404),
  );
  // Undo deletes it (one already gone counts): every list showing
  // dashboards reads them again, and its page, if open, gives way to the
  // list.
  const undo = async () => {
    if (!made) return;
    setPending(true);
    setError("");
    try {
      await undoAgentDashboard(ws, made).catch((e: unknown) => {
        if (!(e instanceof ApiError && e.status === 404)) throw e;
      });
      setUndone(true);
      qc.removeQueries({
        queryKey: dashboardQueries.dashboard(ws, made).queryKey,
        exact: true,
      });
      void qc.invalidateQueries({
        queryKey: dashboardQueries.list(ws).queryKey,
        exact: true,
      });
      if (pathname === dashboardPaths.dashboard(made))
        navigate(dashboardPaths.dashboards(), { replace: true });
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      setPending(false);
    }
  };
  const title = view
    ? view.title
    : `An action this page can’t show (${action.operation || "unnamed"})`;
  return (
    <section className="ag-card" aria-label={`Action: ${title}`}>
      <header className="ag-card-head">
        <Zap size={15} aria-hidden className="ag-card-icon" />
        <strong className="ag-card-title">{title}</strong>
        <StatusBadge
          size="sm"
          status={AUTO_BADGE[state].badge}
          label={AUTO_BADGE[state].label}
        />
      </header>
      {view && <Facts facts={view.facts} onNavigate={onNavigate} />}
      {state === "making" && (
        <p className="ag-card-muted">
          <LoaderCircle size={14} className="hf-spin" aria-hidden />
          Making the dashboard…
        </p>
      )}
      {state === "made" && view && <p className="ag-card-text">{view.does}</p>}
      {state === "undone" && (
        <p className="ag-card-muted">Undone: the dashboard was deleted.</p>
      )}
      {state === "made" && (link || made) && (
        <div className="ag-card-actions">
          {link && (
            <ButtonLink size="sm" to={link.to} onClick={onNavigate}>
              {link.label}
            </ButtonLink>
          )}
          {made && (
            <Button
              variant="ghost"
              size="sm"
              icon={Undo2}
              pending={pending}
              disabled={busy || pending}
              onClick={() => void undo()}
            >
              Undo
            </Button>
          )}
        </div>
      )}
      {state === "made" && made && busy && (
        <p className="ag-card-muted">
          You can undo it once Founder has finished.
        </p>
      )}
      {(state === "failed" || state === "unknown") && action.error && (
        <p className="ag-card-error">{sentence(action.error)}</p>
      )}
      {state === "unknown" && (
        <p className="ag-card-muted">
          Check the dashboards before asking again: it is never run twice.
        </p>
      )}
      <FullRequest action={action} />
      {error && (
        <p className="ag-card-error" role="alert">
          {error}
        </p>
      )}
    </section>
  );
}

/** A card's facts: words, or entities each opening its page. */
function Facts({
  facts,
  onNavigate,
}: {
  facts: CardView["facts"];
  onNavigate?: () => void;
}) {
  return (
    <dl className="ag-card-facts">
      {facts.map((f) => (
        <Fragment key={f.label}>
          <dt>{f.label}</dt>
          <dd>
            {"value" in f
              ? f.value
              : f.entities.map((e, i) => (
                  <Fragment key={i}>
                    {i > 0 && ", "}
                    {e.to ? (
                      <Link className="ag-link" to={e.to} onClick={onNavigate}>
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
  );
}

/** The whole request an action runs, as Execute was shown it. */
function FullRequest({ action }: { action: FoundryAgentAction }) {
  return (
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
  );
}

/**
 * What a create or an edit of an automation saves: its steps in words, each
 * Founder step with what it may do without asking, which the person's
 * click approves. An edit without steps (a new name or description)
 * approves nothing, and says so: the WFE leaves its approval as it was. A
 * spec that doesn't read says so: the WFE refuses it.
 */
function AutomationProposal({ payloadJson }: { payloadJson: string }) {
  const { id: ws } = useWorkspace(),
    { names } = useNames(ws);
  let payload: unknown;
  try {
    payload = JSON.parse(payloadJson);
  } catch {
    payload = undefined;
  }
  const spec = isRecord(payload) ? payload.spec : undefined;
  const read = spec === undefined ? undefined : parseSpec(spec);
  return (
    <div className="ag-card-proposal">
      {read?.ok && (
        <>
          <p className="ag-card-text">
            Your click approves these steps, which then run as you, unattended,
            each time it starts.
          </p>
          <StepsPreview
            spec={read.spec}
            names={names}
            email={!LOCAL}
            label="Steps it approves"
            showAll
          />
        </>
      )}
      {!read && (
        <p className="ag-card-text">
          Its steps don’t change, so this approves nothing: it stays on, paused
          or waiting for your approval, as it is.
        </p>
      )}
      {read && !read.ok && (
        <p className="ag-card-error">
          Its steps don’t read: {sentence(read.error)}
        </p>
      )}
    </div>
  );
}
