import { Suspense, lazy, useState } from "react";
import { useParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleStop } from "lucide-react";
import type {
  AutomationNode,
  FoundryAutomationRun,
  FoundryAutomationRunNode,
} from "../../../shared/wfe";
import { ApiError, sentence as asSentence } from "../../api";
import {
  Button,
  ButtonLink,
  EmptyState,
  KeyValue,
  Page,
  PageHeader,
  SkeletonText,
  StatusBadge,
  formatDate,
} from "../../kit";
import { RefreshFailed, useWorkspace } from "../../ui";
import { ActionCard } from "../agent/cards";
import { agentQueries, threadBusy } from "../agent/queries";
import {
  eventText,
  parseEvent,
  parseOutput,
  parseSpec,
  runUsd,
  stepStates,
  triggerOf,
  usdText,
  type Names,
  type StepState,
} from "./model";
import { RUN_STATUS, STEP_STATUS, nodeIcon, stepTitle } from "./parts";
import { automationPaths as paths } from "./paths";
import { automationQueries, cancelAutomationRun, useNames } from "./queries";

// One run of an automation: what started it, how it went, its steps on the
// canvas coloured by how each went, and what each found, failed with or
// cost. Its owner sees the actions an Assistant step prepared and left for
// them, as the Assistant's cards, to run or dismiss here. Another member
// sees how far it went and what it cost; only its owner, and owners and
// admins, read what its steps found and why it failed.

const AutomationCanvas = lazy(() => import("./canvas"));

export function RunPage() {
  const { automationId = "", runId = "" } = useParams(),
    { id: ws } = useWorkspace(),
    qc = useQueryClient(),
    lists = useNames(ws);
  const automation = useQuery({
    ...automationQueries.automation(ws, automationId),
    refetchInterval: false,
  });
  const query = useQuery(automationQueries.run(ws, automationId, runId));
  const email = useQuery(automationQueries.list(ws)).data?.email ?? true;
  const [cancelling, setCancelling] = useState(false),
    [error, setError] = useState("");
  const name = automation.data?.automation.name || "Automation";
  const crumbs = [
    { label: "Automations", to: paths.automations() },
    { label: name, to: paths.automation(automationId) },
    { label: "Run" },
  ];
  const run = query.data?.run;
  if (query.isPending)
    return (
      <Page>
        <PageHeader title="Run" breadcrumbs={crumbs} />
        <SkeletonText lines={6} />
      </Page>
    );
  if (!run)
    return (
      <Page>
        <PageHeader title="Run" breadcrumbs={crumbs} />
        <EmptyState
          title={
            query.error instanceof ApiError && query.error.status === 404
              ? "This run is gone"
              : "Couldn’t load the run"
          }
          text={query.error?.message}
          action={
            <ButtonLink to={paths.automation(automationId)}>
              Back to {name}
            </ButtonLink>
          }
        />
      </Page>
    );
  const read = parseSpec(run.specJson || "");
  const spec = read.ok ? read.spec : undefined,
    states = spec && stepStates(spec, run),
    started = eventText(
      parseEvent(run.triggerEventJson),
      spec ? triggerOf(spec).config : undefined,
      lists.names,
    );
  const status = RUN_STATUS[run.status || "queued"];
  return (
    <Page width="wide">
      <PageHeader
        title={name}
        eyebrow="Run"
        documentTitle={`Run of ${name}`}
        breadcrumbs={crumbs}
        meta={<StatusBadge status={status.status} label={status.label} />}
        actions={
          run.canCancel && (
            <Button
              icon={CircleStop}
              pending={cancelling}
              onClick={async () => {
                setCancelling(true);
                setError("");
                try {
                  qc.setQueryData(
                    automationQueries.run(ws, automationId, runId).queryKey,
                    { run: await cancelAutomationRun(ws, automationId, runId) },
                  );
                  void qc.invalidateQueries({ queryKey: [ws, "automations"] });
                } catch (e) {
                  setError(asSentence((e as Error).message));
                } finally {
                  setCancelling(false);
                }
              }}
            >
              Cancel run
            </Button>
          )
        }
      />
      <RefreshFailed query={query} />
      {error && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
      <RunNotice run={run} />
      <KeyValue
        columns={3}
        className="au-run-facts"
        items={[
          { label: "What started it", value: started },
          {
            label: "Started",
            value: run.createdAt ? formatDate(run.createdAt) : "",
          },
          {
            label: "Cost",
            value: usdText(run.usdMicros),
            hint: "What its Assistant steps spent.",
          },
        ]}
      />
      {spec && states ? (
        <div className="au-run">
          <Suspense fallback={<div className="au-flow" aria-busy="true" />}>
            <AutomationCanvas
              spec={spec}
              names={lists.names}
              email={email}
              states={states}
              label="Its steps on a canvas, coloured by how each went"
            />
          </Suspense>
          <RunSteps
            run={run}
            order={read.ok ? read.order : []}
            nodes={spec.nodes}
            states={states}
            names={lists.names}
            email={email}
          />
        </div>
      ) : run.status === "skipped" ? (
        <p className="au-hint">It was skipped, so none of its steps ran.</p>
      ) : (
        <p className="au-hint">Its steps can’t be read.</p>
      )}
    </Page>
  );
}

/** Why a run failed, was cancelled or skipped, or retries: its owner's and managers' to read. */
function RunNotice({ run }: { run: FoundryAutomationRun }) {
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

/** Each step of the run, in the order it runs them: how it went and what it found. */
function RunSteps({
  run,
  order,
  nodes,
  states,
  names,
  email,
}: {
  run: FoundryAutomationRun;
  order: string[];
  nodes: AutomationNode[];
  states: ReadonlyMap<string, StepState>;
  names: Names;
  email: boolean;
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
            <div className="au-run-step-head">
              <Icon size={14} aria-hidden />
              <span className="au-run-step-title">
                {stepTitle(n, names, email)}
              </span>
              <StatusBadge
                size="sm"
                status={STEP_STATUS[state].status}
                label={STEP_STATUS[state].label}
              />
            </div>
            <StepOutcome node={n} step={started.get(id)} run={run} />
          </li>
        );
      })}
    </ol>
  );
}

function StepOutcome({
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
              ? "What the Assistant reported"
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
 * The actions an Assistant step prepared, as the Assistant's cards (only
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
