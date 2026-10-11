// One assistant turn in a chat: progress while it works, then the answer (or
// the prepared changes, or what went wrong).
import { useEffect, useId, useState, type ReactNode } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import {
  BookmarkPlus,
  Check,
  ChevronDown,
  CircleAlert,
  CircleSlash,
  Copy,
  Database,
  FileDiff,
  Info,
  RotateCcw,
  Sparkles,
  Square,
} from "lucide-react";
import type { Resource } from "../../../../../packages/shared/src";
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
  canControl,
  isWorking,
  listText,
  plainError,
  stageText,
  startedAt,
} from "./chat-data";
import { chatListKey } from "./launcher";
import {
  AnswerChart,
  AnswerStats,
  AnswerTable,
  chartPlan,
} from "./answer-view";

export function Avatar() {
  return (
    <span className="an-avatar" aria-hidden>
      <Sparkles size={14} />
    </span>
  );
}

const duration = (ms: number) => {
  const s = Math.max(0, Math.round(ms / 1000));
  return s < 60
    ? `${s}s`
    : `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
};

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

// The status line while a question is being worked on.
export function Working({
  text,
  since,
  onStop,
  stopping,
}: {
  text: string;
  since?: number;
  onStop?: () => void;
  stopping?: boolean;
}) {
  const now = useNow(1000);
  return (
    <div className="an-working">
      <span className="an-dots" aria-hidden>
        <span />
        <span />
        <span />
      </span>
      <span className="an-working-text">{text}</span>
      {since !== undefined && (
        // Hidden from screen readers: a ticking number would be read out every second.
        <span className="an-working-time hf-num" aria-hidden>
          {duration(now - since)}
        </span>
      )}
      {onStop && (
        <Button
          size="sm"
          variant="ghost"
          icon={Square}
          className="an-working-stop"
          pending={stopping}
          onClick={onStop}
        >
          Stop
        </Button>
      )}
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

function Details({ run }: { run: Resource }) {
  const d = run.data,
    took = Date.parse(run.updatedAt) - startedAt(run);
  const events: { at: string; message: string }[] = d.events || [];
  return (
    <>
      <p className="an-details-meta">
        {plural(d.calls || 0, "model call")} · {formatNumber(d.tokens || 0)}{" "}
        tokens
        {!isWorking(d.status) && took > 0 && <> · took {duration(took)}</>}
      </p>
      {events.length > 0 && (
        <ol className="an-events">
          {events.map((e, i) => (
            <li key={i}>
              <time dateTime={e.at} className="hf-num">
                {new Date(e.at).toLocaleTimeString(undefined, {
                  hour: "2-digit",
                  minute: "2-digit",
                  second: "2-digit",
                })}
              </time>
              <span title={e.message}>{e.message}</span>
            </li>
          ))}
        </ol>
      )}
    </>
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

function Answer({ run }: { run: Resource }) {
  const { role } = useWorkspace();
  const a = run.data.answer;
  // Something said about the business, saved instead of queried.
  if (a.saved)
    return (
      <div className="an-answer">
        <p className="an-note">{a.summary}</p>
      </div>
    );
  return <AnswerBody run={run} role={role} />;
}

function AnswerBody({ run, role }: { run: Resource; role: string }) {
  const a = run.data.answer,
    rows: Record<string, unknown>[] = a.rows || [],
    citations: { datasetId: string; name: string; version?: string }[] =
      a.citations || [],
    assumptions: string[] = a.assumptions || [];
  const total: number = typeof a.total === "number" ? a.total : rows.length;
  const names = [...new Set(citations.map((c) => c.name))];
  const count =
    total > rows.length
      ? `First ${formatNumber(rows.length)} of ${plural(total, "row")}`
      : plural(total, "row");
  const plan = rows.length > 1 ? chartPlan(rows) : null,
    single = rows.length === 1 && Object.keys(rows[0]).length <= 4;
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
  if (a.sql)
    sections.push({
      id: "sql",
      label: "SQL",
      content: (
        <div className="an-sql">
          <div className="an-sql-head">
            <span>Read-only query over the cited snapshots</span>
            <CopyButton text={a.sql} />
          </div>
          <pre tabIndex={0} aria-label="SQL query">
            <code>{a.sql}</code>
          </pre>
        </div>
      ),
    });
  sections.push({
    id: "details",
    label: "Details",
    content: <Details run={run} />,
  });
  return (
    <div className="an-answer">
      <div className="an-answer-head">
        <h2>{a.title || "Answer"}</h2>
        <p>
          {count}
          {names.length > 0 && <> from {listText(names)}</>}
        </p>
      </div>
      {a.summary && (
        <div className="an-summary">
          <p>{a.summary}</p>
          {a.highlights?.length > 0 && (
            <ul>
              {a.highlights.map((h: string) => (
                <li key={h}>{h}</li>
              ))}
            </ul>
          )}
        </div>
      )}
      {!rows.length ? (
        <p className="an-note">The query ran, but no rows matched.</p>
      ) : single ? (
        <AnswerStats row={rows[0]} />
      ) : (
        <>
          {plan && <AnswerChart plan={plan} />}
          <AnswerTable rows={rows} label={a.title || "Answer rows"} />
        </>
      )}
      {a.note && (
        <p className="an-limit">
          <Info size={14} aria-hidden />
          <span>
            {a.note} <Link to={paths.ontology()}>Suggestions</Link>
          </span>
        </p>
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
        <Disclosures
          sections={sections}
          actions={role !== "viewer" && <SaveAsView run={run} />}
        />
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

export function Reply({ run }: { run: Resource }) {
  const { role, user } = useWorkspace(),
    control = useRunControl(run),
    d = run.data,
    allowed = canControl(run, role, user.id);
  const failed = d.status === "failed" || d.status === "canceled";
  return (
    <div className="an-reply">
      <Avatar />
      <div className="an-reply-body">
        {isWorking(d.status) ? (
          <Working
            text={stageText(d)}
            since={startedAt(run)}
            onStop={allowed ? control.stop : undefined}
            stopping={control.pending}
          />
        ) : (
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
            {!d.answer && (
              <div className="an-foot">
                <Disclosures
                  sections={[
                    {
                      id: "details",
                      label: "Details",
                      content: <Details run={run} />,
                    },
                  ]}
                />
              </div>
            )}
          </>
        )}
      </div>
    </div>
  );
}
