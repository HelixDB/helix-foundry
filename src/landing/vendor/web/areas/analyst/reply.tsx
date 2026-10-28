// One assistant turn in a chat: the analysis as it happens, then the answer
// (or the prepared changes, or what went wrong).
import {
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import {
  BookmarkPlus,
  Check,
  ChevronDown,
  Circle,
  CircleAlert,
  CircleMinus,
  CircleSlash,
  Copy,
  Database,
  FileDiff,
  LoaderCircle,
  RotateCcw,
  Sparkles,
  Square,
} from "lucide-react";
import { answerShape, type Resource } from "../../../shared";
import { api } from "../../api";
import { useWorkspace } from "../../ui";
import {
  Button,
  ButtonLink,
  cx,
  formatNumber,
  plural,
  useNow,
} from "../../kit";
import { paths } from "../../paths";
import {
  STEPS,
  canControl,
  compactSteps,
  currentStep,
  duration,
  endedAt,
  estimateRange,
  isAnswering,
  isWorking,
  listText,
  plainError,
  runSteps,
  startedAt,
  stepText,
  type Step,
  type StepState,
} from "./chat-data";
import { chatListKey } from "./launcher";
import {
  AnswerResult,
  AnswerStats,
  Emphasized,
  leadSentence,
} from "./answer-view";

export function Avatar() {
  return (
    <span className="an-avatar" aria-hidden>
      <Sparkles size={14} />
    </span>
  );
}

// Stop and retry: after either, the chat and the sidebar list refresh.
export function useRunControl(run?: Resource) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient(),
    [pending, setPending] = useState(false);
  const call = async (verb: "cancel" | "resume") => {
    if (!run) return;
    setPending(true);
    try {
      await api(`/workspaces/${id}/assistant/runs/${run.id}/${verb}`, {});
      await qc.invalidateQueries({ queryKey: chatListKey(id) });
    } catch (e) {
      notify((e as Error).message, true);
    } finally {
      setPending(false);
    }
  };
  return { pending, stop: () => call("cancel"), retry: () => call("resume") };
}

// The status line while a question is being sent, before it has a run.
export function Working({ text }: { text: string }) {
  return (
    <div className="an-working">
      <span className="an-dots" aria-hidden>
        <span />
        <span />
        <span />
      </span>
      <span className="an-working-text">{text}</span>
    </div>
  );
}

// Footer disclosures: one open at a time, shown under the row of toggles.
type Section = { id: string; label: string; content: ReactNode };
function Disclosures({
  sections,
  actions,
}: {
  sections: Section[];
  actions?: ReactNode;
}) {
  const [open, setOpen] = useState<string | null>(null),
    base = useId();
  const current = sections.find((s) => s.id === open);
  return (
    <>
      <div className="an-foot-row">
        {sections.map((s) => (
          <button
            key={s.id}
            type="button"
            className={cx("an-toggle", open === s.id && "is-open")}
            aria-expanded={open === s.id}
            aria-controls={open === s.id ? base + s.id : undefined}
            onClick={() => setOpen((v) => (v === s.id ? null : s.id))}
          >
            {s.label}
            <ChevronDown size={13} aria-hidden />
          </button>
        ))}
        {actions && <span className="an-foot-actions">{actions}</span>}
      </div>
      {current && (
        <div
          id={base + current.id}
          role="region"
          aria-label={current.label}
          className="an-foot-panel"
        >
          {current.content}
        </div>
      )}
    </>
  );
}

function CopyButton({ text }: { text: string }) {
  const { notify } = useWorkspace(),
    [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(t);
  }, [copied]);
  return (
    <>
      <Button
        size="sm"
        variant="ghost"
        icon={copied ? Check : Copy}
        className="an-copy"
        onClick={async () => {
          try {
            await navigator.clipboard.writeText(text);
            setCopied(true);
          } catch {
            notify("Couldn’t copy. Select the SQL and copy it instead.", true);
          }
        }}
      >
        {copied ? "Copied" : "Copy"}
      </Button>
      <span className="hf-sr-only" aria-live="polite">
        {copied ? "SQL copied" : ""}
      </span>
    </>
  );
}

const stateWords: Record<StepState, string> = {
  pending: "not started",
  running: "in progress",
  done: "done",
  skipped: "skipped",
  failed: "failed",
  stopped: "stopped",
};
function StepMark({ state }: { state: StepState }) {
  const Icon =
    state === "done"
      ? Check
      : state === "running"
        ? LoaderCircle
        : state === "failed"
          ? CircleAlert
          : state === "stopped"
            ? CircleSlash
            : state === "skipped"
              ? CircleMinus
              : Circle;
  return (
    <Icon
      size={13}
      className={cx("an-step-mark", state === "running" && "hf-spin")}
      aria-hidden
    />
  );
}
// One step, its name in bold and what it did (or is doing) on the same line.
// Compact rows stay on one line, with the whole text in a tooltip.
function StepRow({
  step,
  run,
  full,
}: {
  step: Step;
  run: Resource;
  full?: boolean;
}) {
  const label = STEPS[step.id].label,
    text = stepText(step, run);
  const took =
    full &&
    (step.state === "done" || step.state === "skipped") &&
    step.startedAt !== undefined &&
    step.endedAt !== undefined
      ? step.endedAt - step.startedAt
      : null;
  return (
    <li
      className={cx("an-step", `is-${step.state}`)}
      title={full ? undefined : `${label}: ${text}`}
    >
      <StepMark state={step.state} />
      <p className="an-step-text">
        <strong>{label}:</strong> {text}
        <span className="hf-sr-only"> ({stateWords[step.state]})</span>
      </p>
      {took !== null && (
        <span className="an-step-time hf-num">
          {took < 1000 ? "<1s" : duration(took)}
        </span>
      )}
    </li>
  );
}

// What the trace announces: a new step, a long wait, a lost connection and
// how it ended. Never the clock.
function announcement(
  phase: string,
  step: Step | undefined,
  answering: boolean,
) {
  const name = step ? STEPS[step.id].label : "";
  switch (phase) {
    case "working":
      return name;
    case "slow":
      return `Taking longer than usual. Still on: ${name}.`;
    case "offline":
      return "Lost the connection to Foundry. The analysis may still be running; reconnecting.";
    case "succeeded":
      return answering ? "Answer ready." : "Done.";
    case "awaiting_review":
      return "Ready for your review.";
    case "failed":
      return "The analysis stopped with an error.";
    case "canceled":
      return "Stopped.";
    default:
      return "";
  }
}

// The analysis as it happens and afterwards: up to three major steps in plain
// words, how long it has taken (and usually takes), and behind "Show steps"
// every step with its time and the query used. All of it comes from the run's
// events: nothing shows as done before the server reports it.
function Trace({
  run,
  offline,
  onStop,
  stopping,
}: {
  run: Resource;
  offline?: boolean;
  onStop?: () => void;
  stopping?: boolean;
}) {
  const d = run.data,
    working = isWorking(d.status),
    answering = isAnswering(d) || (!d.resolvedIntent && d.intent !== "build");
  const now = useNow(working ? 1000 : 3600000);
  const steps = useMemo(() => runSteps(run), [run]),
    compact = compactSteps(steps),
    current = currentStep(steps);
  const [open, setOpen] = useState(false),
    panel = useId();
  const since = startedAt(run),
    elapsed = (working ? now : endedAt(run)) - since;
  const range = answering ? estimateRange(d.estimate) : null,
    slow = working && !!range && elapsed > range.high;
  // The query: the answer's, else the latest one written (a failed run
  // keeps no saved plan, only the model's last reply).
  const sql: string | undefined =
    d.answer?.sql ||
    d.checkpoints?.["analysis-repair"]?.sql ||
    d.checkpoints?.analysis?.sql ||
    (["analysis", "analysis-repair"].includes(d.partialStage?.stage)
      ? d.partialStage.value?.sql
      : undefined);
  const phase =
    offline && working
      ? "offline"
      : working
        ? slow
          ? "slow"
          : "working"
        : d.status;
  const headline =
    phase === "offline"
      ? "Connection lost, reconnecting…"
      : phase === "slow"
        ? "Taking longer than usual"
        : working
          ? d.status === "queued" && !current
            ? "Getting started"
            : answering
              ? "Analyzing your question"
              : "Preparing changes"
          : d.status === "failed"
            ? answering
              ? "Analysis stopped with an error"
              : "Stopped with an error"
            : d.status === "canceled"
              ? answering
                ? "Analysis stopped"
                : "Stopped"
              : d.status === "awaiting_review"
                ? "Ready for your review"
                : answering
                  ? "Analysis complete"
                  : "Done";
  // Announced on change only, so opening a chat doesn't read every reply.
  const said = useRef<string | null>(null),
    [message, setMessage] = useState("");
  const key = `${phase}:${current?.id ?? ""}`;
  useEffect(() => {
    if (said.current !== null && said.current !== key)
      setMessage(announcement(phase, current, answering));
    said.current = key;
  }, [key]);
  const expandable = steps.length > 0 || !!sql;
  return (
    <section
      className={cx("an-trace", `is-${phase}`)}
      aria-label="Analysis steps"
      aria-live="off"
    >
      <div className="an-trace-head">
        <span className="an-trace-status">
          <span className="an-trace-dot" aria-hidden />
          {headline}
        </span>
        <span className="an-trace-tools">
          <span className="an-trace-time">
            {working ? (
              <>
                {range && (
                  <span>
                    {slow ? "Usually" : "About"} {range.text}
                  </span>
                )}
                {range && <span aria-hidden> · </span>}
                {/* A ticking number would be read out every second. */}
                <span className="hf-num" aria-hidden>
                  {duration(elapsed)}
                </span>
              </>
            ) : (
              elapsed > 0 && (
                <span className="hf-num">
                  {d.status === "failed" || d.status === "canceled"
                    ? `Stopped after ${duration(elapsed)}`
                    : answering
                      ? `Answered in ${duration(elapsed)}`
                      : `Took ${duration(elapsed)}`}
                </span>
              )
            )}
          </span>
          {expandable && (
            <button
              type="button"
              className={cx("an-toggle", open && "is-open")}
              aria-expanded={open}
              aria-controls={open ? panel : undefined}
              onClick={() => setOpen((v) => !v)}
            >
              {open ? "Hide steps" : "Show steps"}
              <ChevronDown size={13} aria-hidden />
            </button>
          )}
          {onStop && working && (
            <Button
              size="sm"
              variant="ghost"
              icon={Square}
              className="an-trace-stop"
              pending={stopping}
              onClick={onStop}
            >
              Stop
            </Button>
          )}
        </span>
      </div>
      {open ? (
        <div id={panel} className="an-trace-full">
          {steps.length > 0 && (
            <ol className="an-steps">
              {steps.map((s) => (
                <StepRow key={s.id} step={s} run={run} full />
              ))}
            </ol>
          )}
          {sql && (
            <details className="an-query">
              <summary>
                {d.answer
                  ? "Query used"
                  : working
                    ? "Query planned"
                    : "Query attempted"}
              </summary>
              <div className="an-sql">
                <div className="an-sql-head">
                  <span>Read-only query over your data snapshots</span>
                  <CopyButton text={sql} />
                </div>
                <pre tabIndex={0} aria-label="SQL query">
                  <code>{sql}</code>
                </pre>
              </div>
            </details>
          )}
          {(d.calls || 0) > 0 && (
            <p className="an-trace-note">
              {plural(d.calls, "model call")} · {formatNumber(d.tokens || 0)}{" "}
              tokens
            </p>
          )}
        </div>
      ) : (
        compact.length > 0 && (
          <ol className="an-steps is-compact">
            {compact.map((s) => (
              <StepRow key={s.id} step={s} run={run} />
            ))}
          </ol>
        )
      )}
      <span className="hf-sr-only" role="status">
        {message}
      </span>
    </section>
  );
}

function SaveAsView({ run }: { run: Resource }) {
  const { id, notify } = useWorkspace(),
    navigate = useNavigate(),
    [pending, setPending] = useState(false);
  return (
    <Button
      size="sm"
      variant="ghost"
      icon={BookmarkPlus}
      pending={pending}
      onClick={async () => {
        setPending(true);
        try {
          const p = await api<{ id: string }>(
            `/workspaces/${id}/assistant/runs/${run.id}/save-analysis`,
            {},
          );
          navigate(paths.proposals(p.id));
        } catch (e) {
          notify((e as Error).message, true);
          setPending(false);
        }
      }}
    >
      Save as view
    </Button>
  );
}

// The answer leads with its statement (the summary's first sentence), with
// the numbers the rows back in bold, then the rows as a chart or a table.
// Without a summary, its title leads.
function Answer({ run }: { run: Resource }) {
  const { role } = useWorkspace();
  const a = run.data.answer,
    rows: Record<string, unknown>[] = a.rows || [],
    citations: { datasetId: string; name: string; version?: string }[] =
      a.citations || [],
    assumptions: string[] = a.assumptions || [];
  const total: number = typeof a.total === "number" ? a.total : rows.length;
  const names = [...new Set(citations.map((c) => c.name))];
  const shape = useMemo(
    () =>
      a.check ??
      answerShape(rows, { hint: [run.data.goal || "", a.title || ""] }),
    [a, rows],
  );
  const lead = a.summary ? leadSentence(a.summary) : null,
    rest = lead ? lead.rest : a.summary;
  const title = a.title || "Answer";
  const count =
    total > rows.length
      ? `First ${formatNumber(rows.length)} of ${plural(total, "row")}`
      : plural(total, "row");
  const single = rows.length === 1 && Object.keys(rows[0]).length <= 6;
  const sections: Section[] = [];
  if (assumptions.length)
    sections.push({
      id: "assumptions",
      label: `Assumptions (${assumptions.length})`,
      content: (
        <ul className="an-assumptions">
          {assumptions.map((v) => (
            <li key={v}>{v}</li>
          ))}
        </ul>
      ),
    });
  return (
    <div className="an-answer">
      <div className="an-answer-head">
        <h2>{lead?.statement || title}</h2>
        {rest && (
          <p className="an-lead">
            <Emphasized text={rest} rows={rows} total={total} />
          </p>
        )}
        {a.highlights?.length > 0 && (
          <ul className="an-highlights">
            {a.highlights.map((h: string) => (
              <li key={h}>
                <Emphasized text={h} rows={rows} total={total} />
              </li>
            ))}
          </ul>
        )}
        <p className="an-answer-meta">
          {count}
          {names.length > 0 && <> from {listText(names)}</>}
        </p>
      </div>
      {!rows.length ? (
        <p className="an-note">The query ran, but no rows matched.</p>
      ) : single ? (
        <AnswerStats row={rows[0]} shape={shape} />
      ) : (
        <AnswerResult
          rows={rows}
          total={total}
          label={title}
          shape={shape}
          heading={lead ? title : undefined}
        />
      )}
      <div className="an-foot">
        {citations.length > 0 && (
          <div className="an-sources">
            <span className="an-foot-label">Sources</span>
            {citations.map((c) => (
              <Link
                key={c.datasetId}
                className="an-chip"
                to={paths.dataset(c.datasetId)}
                title={
                  c.version ? `Snapshot ${c.version.slice(0, 8)}` : undefined
                }
              >
                <Database size={12} aria-hidden />
                {c.name}
              </Link>
            ))}
          </div>
        )}
        {(sections.length > 0 || role !== "viewer") && (
          <Disclosures
            sections={sections}
            actions={role !== "viewer" && <SaveAsView run={run} />}
          />
        )}
      </div>
    </div>
  );
}

function PreparedChanges({ run }: { run: Resource }) {
  const published = run.data.stage === "published";
  return (
    <div className="an-card">
      <span className="an-card-icon" aria-hidden>
        <FileDiff size={16} />
      </span>
      <div className="an-card-text">
        <strong>{published ? "Changes published" : "Prepared changes"}</strong>
        <p>
          {published
            ? "These changes are live in your workspace."
            : "Nothing changes until the proposal is reviewed and published."}
        </p>
      </div>
      <ButtonLink
        size="sm"
        variant={published ? "secondary" : "primary"}
        to={paths.proposals(run.data.proposalId)}
      >
        {published ? "View published changes" : "Review"}
      </ButtonLink>
    </div>
  );
}

function Problem({
  run,
  onRetry,
  retrying,
}: {
  run: Resource;
  onRetry?: () => void;
  retrying: boolean;
}) {
  const canceled = run.data.status === "canceled",
    error = canceled ? null : plainError(run.data.error);
  return (
    <div className={cx("an-card", canceled ? "is-muted" : "is-danger")}>
      <span className="an-card-icon" aria-hidden>
        {canceled ? <CircleSlash size={16} /> : <CircleAlert size={16} />}
      </span>
      <div className="an-card-text">
        <strong>{canceled ? "Stopped" : "Couldn’t answer this"}</strong>
        <p>
          {canceled
            ? "This question was stopped before it finished."
            : error!.text}
        </p>
        {error?.detail && (
          <details className="an-card-detail">
            <summary>Technical details</summary>
            <pre>{error.detail}</pre>
          </details>
        )}
      </div>
      {onRetry && (
        <Button size="sm" icon={RotateCcw} pending={retrying} onClick={onRetry}>
          Retry
        </Button>
      )}
    </div>
  );
}

// `offline`: the latest refresh of the chat failed, so the progress shown is
// the last known, not a failure.
export function Reply({ run, offline }: { run: Resource; offline?: boolean }) {
  const { role, user } = useWorkspace(),
    control = useRunControl(run),
    d = run.data,
    allowed = canControl(run, role, user.id);
  const failed = d.status === "failed" || d.status === "canceled";
  return (
    <div className="an-reply">
      <Avatar />
      <div className="an-reply-body">
        <Trace
          run={run}
          offline={offline}
          onStop={allowed ? control.stop : undefined}
          stopping={control.pending}
        />
        {!isWorking(d.status) && (
          <>
            {d.answer && <Answer run={run} />}
            {d.proposalId && <PreparedChanges run={run} />}
            {failed && (
              <Problem
                run={run}
                onRetry={allowed ? control.retry : undefined}
                retrying={control.pending}
              />
            )}
            {!d.answer && !d.proposalId && !failed && (
              <p className="an-note">
                {d.status === "awaiting_review"
                  ? "Ready for your review."
                  : "Done."}
              </p>
            )}
          </>
        )}
      </div>
    </div>
  );
}
