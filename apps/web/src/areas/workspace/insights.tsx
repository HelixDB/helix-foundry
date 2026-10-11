import {
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowLeft,
  ArrowRight,
  ChevronDown,
  CircleAlert,
  CircleCheck,
  LoaderCircle,
  MessageSquare,
  OctagonAlert,
  TriangleAlert,
  X,
  type LucideIcon,
} from "lucide-react";
import type {
  HomeInsight,
  HomeInsights,
  InsightSeverity,
} from "../../../../../packages/shared/src";
import { api } from "../../api";
import { useWorkspace } from "../../ui";
import {
  Button,
  ButtonLink,
  IconButton,
  Skeleton,
  cx,
  formatRelative,
  useNow,
} from "../../kit";
import { paths } from "../../paths";
import { useAskAnalyst } from "../analyst/launcher";

// Labelled, not coloured alone: the word says what the icon shows.
const severities: Record<InsightSeverity, { label: string; icon: LucideIcon }> =
  {
    positive: { label: "Positive", icon: CircleCheck },
    warning: { label: "Warning", icon: TriangleAlert },
    issue: { label: "Issue", icon: OctagonAlert },
  };
const checkStates: Record<
  HomeInsights["coverage"][number]["checks"][number]["state"],
  string
> = {
  found: "Found",
  steady: "No major change",
  skipped: "Not checked",
  set_aside: "Set aside",
};
const busy = (d?: HomeInsights) =>
  d?.scan.state === "queued" || d?.scan.state === "running";
const list = (xs: string[]) =>
  xs.length < 2
    ? xs.join("")
    : xs.slice(0, -1).join(", ") + " and " + xs.at(-1);
// Below this width the cards become a carousel showing one at a time.
const CAROUSEL_BELOW = 640;

function useWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null),
    [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    setWidth(el.clientWidth);
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(([entry]) =>
      setWidth(entry.contentRect.width),
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, []);
  return [ref, width] as const;
}

// Sent without a JSON content type: the API rejects an empty JSON body.
async function restoreFinding(workspaceId: string, findingId: string) {
  const res = await fetch(
    `/api/v1/workspaces/${workspaceId}/home/insights/${encodeURIComponent(findingId)}/dismiss`,
    { method: "DELETE", credentials: "same-origin" },
  );
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || "Couldn’t restore the finding");
  return data as HomeInsights;
}

function Evidence({ f }: { f: HomeInsight }) {
  const ask = useAskAnalyst(),
    [asking, setAsking] = useState(false);
  const d = f.details,
    numeric = (i: number) => i > 0;
  return (
    <div className="wk-finding-evidence">
      {f.returned && <p className="wk-finding-returned">{f.returned}</p>}
      <p className="wk-finding-why">
        <strong>Why it matters:</strong> {d.why}
      </p>
      {d.bars && (
        <div
          className="wk-finding-bars"
          role="img"
          aria-label={`Share: ${d.bars.map((b) => `${b.label.toLowerCase()} ${b.text}`).join(", ")}`}
        >
          {d.bars.map((b) => (
            <div key={b.label} className="wk-finding-bar" aria-hidden>
              <span>{b.label}</span>
              <span className="wk-finding-track">
                <span
                  className={cx("wk-finding-fill", b.previous && "is-previous")}
                  style={{
                    width: `${Math.max(0, Math.min(1, b.share)) * 100}%`,
                  }}
                />
              </span>
              <strong className="hf-num">{b.text}</strong>
            </div>
          ))}
        </div>
      )}
      <div className="wk-finding-table">
        <table>
          <caption className="hf-sr-only">{d.table.caption}</caption>
          <thead>
            <tr>
              {d.table.columns.map((c, i) => (
                <th key={c} scope="col" className={cx(numeric(i) && "is-num")}>
                  {c}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {d.table.rows.map((row, r) => (
              <tr key={r}>
                {row.map((cell, i) =>
                  i === 0 ? (
                    <th key={i} scope="row">
                      {cell}
                    </th>
                  ) : (
                    <td key={i} className="is-num hf-num">
                      {cell}
                    </td>
                  ),
                )}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p>
        <strong>What changed:</strong> {d.contribution}
      </p>
      <p>
        <strong>Compared:</strong> {d.comparison}
      </p>
      <p>
        <strong>Definition:</strong> {d.definition}
      </p>
      <p>
        <strong>Evidence:</strong> {d.evidence}
      </p>
      {d.limitations.length > 0 && (
        <p>
          <strong>Limitations:</strong> {d.limitations.join(" ")}
        </p>
      )}
      <details className="wk-finding-query">
        <summary>
          Query
          <ChevronDown size={12} aria-hidden />
        </summary>
        <pre>
          <code>{d.query}</code>
        </pre>
      </details>
      <div className="wk-finding-links">
        <Button
          size="sm"
          variant="ghost"
          icon={MessageSquare}
          pending={asking}
          onClick={async () => {
            setAsking(true);
            try {
              await ask({ prompt: f.ask });
            } finally {
              setAsking(false);
            }
          }}
        >
          Ask Analyst
        </Button>
        {d.sources.map((s) => (
          <ButtonLink
            key={s.datasetId}
            size="sm"
            variant="ghost"
            to={paths.dataset(s.datasetId)}
          >
            Open {s.name}
          </ButtonLink>
        ))}
      </div>
    </div>
  );
}

function FindingCard({
  f,
  slide,
  hidden,
  onDismiss,
}: {
  f: HomeInsight;
  slide?: { index: number; count: number };
  hidden: boolean;
  onDismiss: () => void;
}) {
  const s = severities[f.severity],
    heading = useId();
  return (
    <article
      id={"finding-" + f.id}
      className={cx("wk-finding", "is-" + f.severity)}
      hidden={hidden}
      tabIndex={-1}
      {...(slide
        ? {
            role: "group",
            "aria-roledescription": "slide",
            "aria-label": `Finding ${slide.index + 1} of ${slide.count}`,
          }
        : { "aria-labelledby": heading })}
    >
      <p className="wk-finding-kind">
        <s.icon size={13} aria-hidden />
        {s.label}
        {f.returned && <span className="wk-finding-back">· Back</span>}
      </p>
      <h3 id={heading}>{f.headline}</h3>
      <p className="wk-finding-support">
        {f.support.figureFirst ? (
          <>
            <strong className="hf-num">{f.support.figure}</strong>{" "}
            {f.support.text}
          </>
        ) : (
          <>
            {f.support.text}{" "}
            <strong className="hf-num">{f.support.figure}</strong>
          </>
        )}
      </p>
      <details className="wk-finding-details">
        <summary>
          Details
          <span className="hf-sr-only">: {f.headline}</span>
          <ChevronDown size={12} aria-hidden />
        </summary>
        <Evidence f={f} />
      </details>
      <div className="wk-finding-actions">
        <button
          type="button"
          className="wk-finding-dismiss"
          onClick={onDismiss}
          aria-label={`Dismiss: ${f.headline}`}
        >
          Dismiss
          <X size={12} aria-hidden />
        </button>
      </div>
    </article>
  );
}

// Up to three candidate priorities an owner can confirm when the data fits
// several about equally well.
function Priorities({ data }: { data: HomeInsights }) {
  const { id, notify } = useWorkspace();
  const qc = useQueryClient();
  const [chosen, setChosen] = useState<string[]>(() =>
    data.priorities.selected.map((d) => d.id),
  );
  const [pending, setPending] = useState(false);
  const save = async (drivers: string[]) => {
    setPending(true);
    try {
      qc.setQueryData(
        [id, "home-insights"],
        await api<HomeInsights>(
          `/workspaces/${id}/home/insights/priorities`,
          { drivers },
          "PUT",
        ),
      );
    } catch (e) {
      notify((e as Error).message, true);
    } finally {
      setPending(false);
    }
  };
  return (
    <fieldset className="wk-noticed-priorities">
      <legend>
        {data.priorities.confirmed
          ? "Priorities you confirmed"
          : "Several priorities fit your data about equally well. Choose up to three."}
      </legend>
      {data.priorities.candidates.map((c) => {
        const on = chosen.includes(c.id);
        return (
          <label key={c.id}>
            <input
              type="checkbox"
              checked={on}
              disabled={pending || (!on && chosen.length >= 3)}
              onChange={() =>
                setChosen((x) =>
                  on ? x.filter((y) => y !== c.id) : [...x, c.id],
                )
              }
            />
            <span>
              <strong>{c.label}</strong> · {c.reasons.join(" · ")}
            </span>
          </label>
        );
      })}
      <div className="wk-noticed-priority-actions">
        <Button
          size="sm"
          pending={pending}
          disabled={!chosen.length}
          onClick={() => void save(chosen)}
        >
          Confirm priorities
        </Button>
        {data.priorities.confirmed && (
          <Button
            size="sm"
            variant="ghost"
            disabled={pending}
            onClick={() => void save([])}
          >
            Choose automatically
          </Button>
        )}
      </div>
    </fieldset>
  );
}

// "Checked: …" or the step in progress, and what was checked on demand.
function ScanLine({ data }: { data: HomeInsights }) {
  const { role } = useWorkspace();
  const now = useNow(30000);
  const scanning = busy(data),
    active = data.scan.steps.find((s) => s.state === "active");
  const names = data.priorities.selected.map((d) => d.label);
  let status: ReactNode = null;
  if (scanning)
    status = (
      <>
        <LoaderCircle size={13} className="hf-spin" aria-hidden />
        <strong>Scanning:</strong>{" "}
        {active ? `${active.label}…` : "Waiting to start…"}
      </>
    );
  else if (data.checkedAt)
    status = (
      <>
        <strong>Checked:</strong> {names.length ? list(names) : "Your activity"}{" "}
        ·{" "}
        <time dateTime={data.checkedAt}>
          {formatRelative(data.checkedAt, now)}
        </time>
      </>
    );
  return (
    <div className={cx("wk-noticed-scan", scanning && "is-busy")}>
      <p className="wk-noticed-status" role="status">
        {status}
      </p>
      <details className="wk-noticed-checks">
        <summary>
          What we checked
          <ChevronDown size={12} aria-hidden />
        </summary>
        <div className="wk-noticed-checks-body">
          <ol className="wk-noticed-steps">
            {data.scan.steps.map((s) => (
              <li key={s.id} className={"is-" + s.state}>
                {s.state === "active" ? (
                  <LoaderCircle size={13} className="hf-spin" aria-hidden />
                ) : s.state === "done" ? (
                  <CircleCheck size={13} aria-hidden />
                ) : (
                  <span className="wk-noticed-step-dot" aria-hidden />
                )}
                <span>
                  <strong>{s.label}:</strong> {s.detail}
                  <span className="hf-sr-only">
                    {s.state === "active"
                      ? " (in progress)"
                      : s.state === "waiting"
                        ? " (waiting)"
                        : ""}
                  </span>
                </span>
              </li>
            ))}
          </ol>
          {data.coverage.map((c) => (
            <div key={c.driver} className="wk-noticed-driver">
              <h3>{c.label}</h3>
              <ul>
                {c.checks.map((k) => (
                  <li key={k.check}>
                    <span className={cx("wk-noticed-state", "is-" + k.state)}>
                      {checkStates[k.state]}
                    </span>
                    <span>
                      <strong>{k.label}.</strong> {k.note}
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          ))}
          {data.priorities.unavailable.length > 0 && (
            <div className="wk-noticed-driver">
              <h3>Not enough data yet</h3>
              <ul>
                {data.priorities.unavailable.map((u) => (
                  <li key={u.id}>
                    <span>
                      <strong>{u.label}.</strong> {u.reason}
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          )}
          {role === "owner" &&
            (data.priorities.ambiguous || data.priorities.confirmed) && (
              <Priorities
                key={data.priorities.selected.map((d) => d.id).join()}
                data={data}
              />
            )}
        </div>
      </details>
    </div>
  );
}

function NoticedSkeleton() {
  return (
    <div
      className="wk-noticed-cards"
      role="status"
      aria-label="Loading findings"
    >
      {[0, 1, 2].map((i) => (
        <div key={i} className="wk-finding is-loading">
          <Skeleton width={64} />
          <Skeleton width="80%" height={14} />
          <Skeleton width={110} />
        </div>
      ))}
    </div>
  );
}

// Home's "Some things we noticed": up to three business shifts beneath the
// headline totals, side by side, or one at a time on a narrow screen. The
// server checks in the background; this only shows what it found.
export function Noticed() {
  const { id, notify } = useWorkspace();
  const qc = useQueryClient(),
    key = [id, "home-insights"];
  const query = useQuery({
    queryKey: key,
    queryFn: () => api<HomeInsights>(`/workspaces/${id}/home/insights`),
    refetchInterval: (q) => (busy(q.state.data) ? 2500 : 60000),
  });
  const data = query.data;
  const [box, width] = useWidth<HTMLElement>();
  const carousel = width > 0 && width < CAROUSEL_BELOW;
  const [index, setIndex] = useState(0),
    [gone, setGone] = useState<string[]>([]),
    [undo, setUndo] = useState<HomeInsight | null>(null),
    [retrying, setRetrying] = useState(false);
  const undoButton = useRef<HTMLButtonElement>(null),
    focusAfter = useRef<string | null>(null);
  const findings = (data?.findings || []).filter((f) => !gone.includes(f.id));
  const count = findings.length;
  useEffect(() => {
    if (index > 0 && index >= count) setIndex(Math.max(0, count - 1));
  }, [count, index]);
  useEffect(() => {
    if (undo) undoButton.current?.focus();
  }, [undo]);
  useEffect(() => {
    const target = focusAfter.current;
    if (!target) return;
    focusAfter.current = null;
    document.getElementById("finding-" + target)?.focus();
  });
  if (data?.status === "unsupported") return null;
  if (!data && query.error)
    return (
      <section className="wk-noticed" aria-labelledby="wk-noticed-title">
        <h2 id="wk-noticed-title">Some things we noticed</h2>
        <p className="wk-noticed-note">
          <CircleAlert size={14} aria-hidden /> Couldn’t load findings.{" "}
          <Button size="sm" variant="ghost" onClick={() => query.refetch()}>
            Retry
          </Button>
        </p>
      </section>
    );
  const dismiss = async (f: HomeInsight) => {
    setGone((g) => [...g, f.id]);
    setUndo(f);
    try {
      qc.setQueryData(
        key,
        await api<HomeInsights>(
          `/workspaces/${id}/home/insights/${encodeURIComponent(f.id)}/dismiss`,
          { version: f.version },
        ),
      );
    } catch (e) {
      setGone((g) => g.filter((x) => x !== f.id));
      setUndo(null);
      notify((e as Error).message, true);
    }
  };
  const restore = async () => {
    const f = undo;
    if (!f) return;
    setUndo(null);
    try {
      const next = await restoreFinding(id, f.id);
      qc.setQueryData(key, next);
      setGone((g) => g.filter((x) => x !== f.id));
      const at = next.findings.findIndex((x) => x.id === f.id);
      if (at >= 0) setIndex(at);
      focusAfter.current = f.id;
    } catch (e) {
      setUndo(f);
      notify((e as Error).message, true);
    }
  };
  const retry = async () => {
    setRetrying(true);
    try {
      qc.setQueryData(
        key,
        await api<HomeInsights>(`/workspaces/${id}/home/insights/scan`, {}),
      );
    } catch (e) {
      notify((e as Error).message, true);
    } finally {
      setRetrying(false);
    }
  };
  const failed = data?.scan.state === "failed";
  return (
    <section
      ref={box}
      className="wk-noticed"
      aria-labelledby="wk-noticed-title"
      aria-busy={busy(data) || undefined}
    >
      <div className="wk-noticed-head">
        <h2 id="wk-noticed-title">Some things we noticed</h2>
      </div>
      {!data ? (
        <NoticedSkeleton />
      ) : (
        <>
          <ScanLine data={data} />
          {failed && (
            <div className="wk-noticed-error" role="alert">
              <CircleAlert size={14} aria-hidden />
              <span>
                Couldn’t refresh these findings: {data.scan.error}
                {data.checkedAt &&
                  ` Showing what was found ${formatRelative(data.checkedAt)}.`}
              </span>
              <Button
                size="sm"
                variant="ghost"
                pending={retrying}
                onClick={() => void retry()}
              >
                Retry
              </Button>
            </div>
          )}
          {!count && busy(data) && !data.checkedAt && <NoticedSkeleton />}
          {count > 0 && (
            <div
              className={cx("wk-noticed-cards", carousel && "is-carousel")}
              {...(carousel
                ? {
                    role: "region",
                    "aria-roledescription": "carousel",
                    "aria-label": "Business findings",
                  }
                : {})}
            >
              {findings.map((f, i) => (
                <FindingCard
                  key={f.id}
                  f={f}
                  slide={carousel ? { index: i, count } : undefined}
                  hidden={carousel && i !== index}
                  onDismiss={() => void dismiss(f)}
                />
              ))}
            </div>
          )}
          {carousel && count > 1 && (
            <div className="wk-noticed-nav">
              <IconButton
                size="sm"
                icon={ArrowLeft}
                label="Previous finding"
                disabled={index === 0}
                onClick={() => setIndex((i) => Math.max(0, i - 1))}
              />
              <span aria-live="polite" className="hf-num">
                {index + 1} of {count}
              </span>
              <IconButton
                size="sm"
                icon={ArrowRight}
                label="Next finding"
                disabled={index >= count - 1}
                onClick={() => setIndex((i) => Math.min(count - 1, i + 1))}
              />
            </div>
          )}
          {!count && (data.dismissed > 0 || gone.length > 0) && (
            <p className="wk-noticed-empty">All findings dismissed.</p>
          )}
          {undo && (
            <p className="wk-noticed-undo">
              <span role="status">Finding dismissed.</span>
              <button
                ref={undoButton}
                type="button"
                className="hf-btn hf-btn--ghost hf-btn--sm"
                onClick={() => void restore()}
              >
                Undo
              </button>
            </p>
          )}
        </>
      )}
    </section>
  );
}
