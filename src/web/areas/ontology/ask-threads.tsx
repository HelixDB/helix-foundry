import "./ask.css";
import { Link, useLocation, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Gauge, SquarePen } from "lucide-react";
import {
  ButtonLink,
  SkeletonText,
  cx,
  formatRelative,
  useMedia,
  useNow,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { formatUsd } from "../documents/model";
import { askOntologyId, askView, threadGroups, threadsOf } from "./ask-model";
import { withLoad } from "./model";
import { questionQueries } from "./queries";

// Ask's threads, in the shell's sidebar under the sections (and, on a narrow
// screen, as the Ask page itself until a thread is chosen): New thread, the
// threads grouped by recency as chat apps list them, and the month's spend
// on questions. The shell renders it
// outside the ontology's scope, so it reads the ontology from the path and
// the threads from the questions list the page shares; its links keep the
// page's `?load=`.

export function AskThreads({ onNavigate }: { onNavigate?: () => void }) {
  const o = askOntologyId(useLocation().pathname);
  if (!o)
    return (
      <p className="hf-section-panel-note">
        Threads show here once there’s an ontology to ask about.
      </p>
    );
  return <Threads key={o} ontologyId={o} onNavigate={onNavigate} />;
}

function Threads({
  ontologyId: o,
  onNavigate,
}: {
  ontologyId: string;
  onNavigate?: () => void;
}) {
  const { id: ws } = useWorkspace(),
    [params] = useSearchParams(),
    now = useNow(60_000);
  const list = useQuery(questionQueries.list(ws, o));
  const threads = threadsOf(list.data?.questions || []),
    budget = list.data?.budget;
  // What the page shows: on a narrow screen without a thread chosen, these
  // threads, and a new thread about an entity isn't New thread's page.
  const view = askView(
      params,
      useMedia("(max-width: 800px)"),
      list.data ? threads.length : undefined,
    ),
    thread = view.kind === "thread" ? view.id : "",
    fresh = view.kind === "new" && !params.get("focus");
  const link = (to: { thread?: string; new?: boolean }) =>
    withLoad(paths.ask(o, to), params.get("load"));
  return (
    <div className="ak-panel">
      <div className="ak-panel-head">
        <ButtonLink
          to={link({ new: true })}
          icon={SquarePen}
          className="ak-new"
          aria-current={fresh ? "page" : undefined}
          onClick={onNavigate}
        >
          New thread
        </ButtonLink>
      </div>
      <nav className="ak-threads" aria-label="Threads">
        {list.isPending ? (
          <div className="ak-threads-note" aria-busy="true">
            <SkeletonText lines={3} />
          </div>
        ) : list.isError ? (
          <p className="ak-threads-note" role="alert">
            Couldn’t read the threads: {list.error.message}{" "}
            <button
              type="button"
              className="ak-retry"
              onClick={() => void list.refetch()}
            >
              Retry
            </button>
          </p>
        ) : !threads.length ? (
          <p className="ak-threads-note">
            Nothing asked yet. Questions and their answers stay here.
          </p>
        ) : (
          threadGroups(threads, new Date(now)).map((g) => (
            <section key={g.label} className="ak-group">
              <h2>{g.label}</h2>
              <ul aria-label={g.label}>
                {g.threads.map((t) => {
                  const first = t.questions[0]!,
                    n = t.questions.length;
                  return (
                    <li key={t.id}>
                      <Link
                        to={link({ thread: t.id })}
                        onClick={onNavigate}
                        className={cx(
                          "ak-thread",
                          t.id === thread && "is-active",
                        )}
                        aria-current={t.id === thread ? "page" : undefined}
                        title={first.question}
                      >
                        <span className="ak-thread-title">
                          {first.question}
                        </span>
                        <small>
                          {t.live && (
                            <span className="ak-thread-dot" aria-hidden />
                          )}
                          {t.live
                            ? "Answering…"
                            : `${n > 1 ? `${n} questions · ` : ""}${formatRelative(t.last, now)}`}
                        </small>
                      </Link>
                    </li>
                  );
                })}
              </ul>
            </section>
          ))
        )}
      </nav>
      {budget && (
        <details className="ak-budget" aria-label="Questions budget">
          <summary>
            <Gauge size={14} aria-hidden />
            <span>
              <strong>{formatUsd(budget.spentUsdMicros || 0)}</strong> of{" "}
              {formatUsd(budget.monthlyUsdMicros || 0)} this month
            </span>
          </summary>
          <p>
            Spent on questions by the workspace this month
            {Number(budget.reservedUsdMicros || 0) > 0 &&
              `, and ${formatUsd(budget.reservedUsdMicros)} held for questions running`}
            . A question spends at most{" "}
            {formatUsd(budget.questionUsdMicros || 0)}, {budget.maxSteps ?? 20}{" "}
            steps and {Math.round((budget.maxWallSecs ?? 240) / 60)} minutes;{" "}
            {budget.concurrentLimit ?? 2} run at a time and{" "}
            {budget.hourlyLimit ?? 30} an hour per workspace.
          </p>
        </details>
      )}
    </div>
  );
}
