import { Link } from "react-router-dom";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import type {
  AutomationNode,
  AutomationTrigger,
  FoundryAutomation,
  FoundryAutomationRun,
  FoundryAutomationRunNode,
  ListAutomationRunsResponse,
} from "../../../shared/wfe";
import { api, sentence as asSentence } from "../../api";
import {
  Button,
  EmptyState,
  SkeletonText,
  StatusBadge,
  StatusDot,
  cx,
  formatRelative,
  useNow,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { ActionCard } from "../agent/cards";
import { agentQueries, threadBusy } from "../agent/queries";
import {
  eventText,
  isLiveRun,
  parseEvent,
  parseOutput,
  runUsd,
  usdText,
  type Names,
  type StepState,
} from "./model";
import { RUN_STATUS, STEP_STATUS, nodeIcon, stepTitle } from "./parts";
import { automationPaths as paths } from "./paths";
import { ACTIVE_POLL_MS, RUN_POLL_MS, automationQueries } from "./queries";

// An automation's runs on its canvas page: the list beside the canvas
// (newest first, 50 a page), and what the inspector says of the run open
// and of each of its steps: what it found, failed with or cost, and, to its
// owner, the actions a Founder step prepared and left for them. Another
// member sees how far a run went and what it cost; only its owner, and
// owners and admins, read what its steps found and why it failed.

/** A run's time, from start to end, in words. */
export const took = (r: FoundryAutomationRun) => {
  const from = Date.parse(r.startedAt || ""),
    to = Date.parse(r.finishedAt || "");
  if (!Number.isFinite(from) || !Number.isFinite(to)) return "—";
  const s = Math.max(0, Math.round((to - from) / 1000));
  return s < 60 ? `${s} s` : `${Math.round(s / 60)} min`;
};

/** The automation's runs, newest first, read again while one is live or it is on; none for a draft. */
export function useRuns(a?: FoundryAutomation) {
  const { id: ws } = useWorkspace();
  const id = a?.id || "";
  const runs = useInfiniteQuery({
    queryKey: automationQueries.runs(ws, id).queryKey,
    queryFn: ({ pageParam }) =>
      api<ListAutomationRunsResponse>(
        `/v1/workspaces/${encodeURIComponent(ws)}/foundry/automations/${encodeURIComponent(id)}/runs` +
          (pageParam ? `?pageToken=${encodeURIComponent(pageParam)}` : ""),
      ),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextPageToken || undefined,
    enabled: !!a,
    // A live run in the list is read again as often as its own page, so
    // the list says it ended when the page does, not up to a poll later.
    refetchInterval: (q) =>
      isLiveRun(a?.lastRun) ||
      q.state.data?.pages.some((p) => p.runs?.some(isLiveRun))
        ? RUN_POLL_MS
        : a?.status === "active"
          ? ACTIVE_POLL_MS
          : false,
  });
  return {
    query: runs,
    all: runs.data?.pages.flatMap((p) => p.runs || []) || [],
  };
}

/** The runs list beside the canvas: each opens on the canvas, coloured. */
export function RunsList({
  automation,
  runs,
  open,
  trigger,
  names,
}: {
  automation: FoundryAutomation;
  runs: ReturnType<typeof useRuns>;
  // The run open, if any.
  open?: string;
  trigger?: AutomationTrigger;
  names: Names;
}) {
  const now = useNow(30_000);
  if (runs.query.isPending)
    return (
      <div className="au-runs-note" aria-busy="true">
        <SkeletonText lines={4} />
      </div>
    );
  if (runs.query.isError && !runs.all.length)
    return (
      <p className="au-runs-note" role="alert">
        Couldn’t read its runs: {runs.query.error.message}{" "}
        <button
          type="button"
          className="au-retry"
          onClick={() => void runs.query.refetch()}
        >
          Retry
        </button>
      </p>
    );
  if (!runs.all.length)
    return (
      <EmptyState
        size="sm"
        title="No runs yet"
        text={
          automation.status === "active"
            ? "It runs each time it starts. You can also run it now."
            : automation.canRun
              ? "Run it now to try it."
              : "It hasn’t run yet."
        }
      />
    );
  return (
    <>
      <ol className="au-runs-list" aria-label="Runs">
        {runs.all.map((r) => {
          const s = RUN_STATUS[r.status || "queued"];
          return (
            <li key={r.id}>
              <Link
                to={paths.automationRun(automation.id, r.id)}
                className={cx("au-run-row", r.id === open && "is-active")}
                aria-current={r.id === open ? "page" : undefined}
              >
                <span className="au-run-row-head">
                  <StatusDot status={s.status} label={s.label} />
                  <span className="au-run-row-what">
                    {eventText(parseEvent(r.triggerEventJson), trigger, names)}
                  </span>
                </span>
                <small>
                  {s.label}
                  {r.createdAt && ` · ${formatRelative(r.createdAt, now)}`}
                  {r.finishedAt && ` · ${took(r)}`}
                  {runUsd(r.usdMicros) > 0 && ` · ${usdText(r.usdMicros)}`}
                </small>
              </Link>
            </li>
          );
        })}
      </ol>
      {runs.query.hasNextPage && (
        <Button
          size="sm"
          className="au-more"
          pending={runs.query.isFetchingNextPage}
          onClick={() => void runs.query.fetchNextPage()}
        >
          Show older runs
        </Button>
      )}
    </>
  );
}

/** Why a run failed, was cancelled or skipped, or retries: its owner's and managers' to read. */
export function RunNotice({ run }: { run: FoundryAutomationRun }) {
  const why = run.error;
  switch (run.status) {
    case "failed":
      return (
        <div className="au-callout is-danger" role="status">
          <p>
            {why
              ? `It failed. ${asSentence(why)}`
              : "It failed. Its owner, and the workspace’s owners and admins, see why."}
          </p>
          <p>The steps after the one that failed didn’t run.</p>
        </div>
      );
    case "skipped":
    case "cancelled":
      return why ? (
        <div className="au-callout is-muted" role="status">
          <p>{asSentence(why)}</p>
        </div>
      ) : null;
    case "queued":
      return (run.attempts || 0) > 0 ? (
        <div className="au-callout is-muted" role="status">
          <p>
            Foundry will try it again shortly
            {why ? `: ${asSentence(why)}` : "."}
          </p>
        </div>
      ) : null;
    default:
      return null;
  }
}

/**
 * Each step of the run, in the order it runs them: how it went and what it
 * found. Selecting one shows it alone on the canvas's inspector.
 */
export function RunSteps({
  run,
  order,
  nodes,
  states,
  names,
  email,
  onSelect,
}: {
  run: FoundryAutomationRun;
  order: string[];
  nodes: AutomationNode[];
  states: ReadonlyMap<string, StepState>;
  names: Names;
  email: boolean;
  onSelect: (id: string) => void;
}) {
  const byId = new Map(nodes.map((n) => [n.id, n])),
    started = new Map((run.nodes || []).map((n) => [n.nodeId, n]));
  return (
    <ol className="au-run-steps" aria-label="What each step did">
      {order.map((id) => {
        const n = byId.get(id)!,
          state = states.get(id) ?? "waiting",
          Icon = nodeIcon(n);
        return (
          <li key={id} className={`au-run-step is-${state}`}>
            <button
              type="button"
              className="au-run-step-head"
              onClick={() => onSelect(id)}
            >
              <Icon size={14} aria-hidden />
              <span className="au-run-step-title">
                {stepTitle(n, names, email)}
              </span>
              <StatusBadge
                size="sm"
                status={STEP_STATUS[state].status}
                label={STEP_STATUS[state].label}
              />
            </button>
            <StepOutcome node={n} step={started.get(id)} run={run} />
          </li>
        );
      })}
    </ol>
  );
}

/** What one step of a run found, failed with or cost, and what it left waiting. */
export function StepOutcome({
  node,
  step,
  run,
}: {
  node: AutomationNode;
  step?: FoundryAutomationRunNode;
  run: FoundryAutomationRun;
}) {
  if (!step) return null;
  const out = parseOutput(step.outputJson),
    usd = runUsd(step.usdMicros);
  return (
    <div className="au-run-step-body">
      {out.summary && (
        <blockquote className="au-found">
          <span className="au-found-label">
            {node.kind === "assistant"
              ? "What Founder reported"
              : "What happened"}
          </span>
          <span className="au-found-text">{out.summary}</span>
        </blockquote>
      )}
      {out.answer !== undefined && (
        <p className="au-run-step-fact">Answer: {out.answer ? "yes" : "no"}</p>
      )}
      {out.count !== undefined && (
        <p className="au-run-step-fact">Count: {out.count.toLocaleString()}</p>
      )}
      {out.branch !== undefined && node.kind === "condition" && (
        <p className="au-run-step-fact">
          {out.branch
            ? "It was met: it went the “When met” way."
            : "It wasn’t met: it went the “Otherwise” way."}
        </p>
      )}
      {step.error && (
        <p className="au-run-step-error">{asSentence(step.error)}</p>
      )}
      {usd > 0 && (
        <p className="au-run-step-fact">Cost: {usdText(step.usdMicros)}</p>
      )}
      {node.kind === "assistant" && run.threadId && step.turnId && (
        <Waiting threadId={run.threadId} turnId={step.turnId} />
      )}
    </div>
  );
}

/**
 * The actions a Founder step prepared, as Founder's cards (only
 * the automation's owner reads the run's thread): those it ran because its
 * owner approved them, and those that wait for them, which Run runs as them
 * now and Dismiss drops.
 */
function Waiting({ threadId, turnId }: { threadId: string; turnId: string }) {
  const { id: ws } = useWorkspace();
  const thread = useQuery(agentQueries.thread(ws, threadId));
  const actions = (thread.data?.actions || []).filter(
    (a) => a.turnId === turnId,
  );
  if (!actions.length) return null;
  const waiting = actions.filter((a) => a.status === "prepared").length;
  return (
    <div className="au-waiting">
      <p className="au-run-step-fact">
        {waiting
          ? waiting === 1
            ? "One action it prepared waits for you:"
            : `${waiting} actions it prepared wait for you:`
          : "What it prepared:"}
      </p>
      {actions.map((a) => (
        <ActionCard key={a.id} action={a} busy={threadBusy(thread.data)} />
      ))}
    </div>
  );
}
