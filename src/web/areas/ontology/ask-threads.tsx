import "./ask.css";
import { Link, useLocation, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { SquarePen } from "lucide-react";
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
import { askOntologyId, askView, threadGroups, threadsOf } from "./ask-model";
import { withLoad } from "./model";
import { readAccess } from "../workspace/access-model";
import { ontologyQueries, questionQueries } from "./queries";

// Ask's threads, in the shell's sidebar under the sections (and, on a narrow
// screen, as the Ask page itself until a thread is chosen): New thread and
// the threads grouped by recency as chat apps list them. The shell renders it
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
  // Someone who can't read the ontology (readAccess) has no threads of it to
  // list. Through grants, the budget is the asker's own.
  const me = useWorkspace(),
    { id: ws } = me,
    [params] = useSearchParams(),
    now = useNow(60_000);
  const read = useQuery(ontologyQueries.ontology(ws, o)),
    ontology = read.data?.ontology;
  const unread = !!ontology && readAccess(me, ontology) === "none";
  const list = useQuery({
    ...questionQueries.list(ws, o),
    enabled: !!ontology && !unread,
  });
  const threads = threadsOf(list.data?.questions || []);
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
        {unread ? (
          <p className="ak-threads-note">Not shared with you.</p>
        ) : !ontology && read.isError ? (
          <p className="ak-threads-note" role="alert">
            Couldn’t read the threads: {read.error.message}{" "}
            <button
              type="button"
              className="ak-retry"
              onClick={() => void read.refetch()}
            >
              Retry
            </button>
          </p>
        ) : list.isPending ? (
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
    </div>
  );
}
