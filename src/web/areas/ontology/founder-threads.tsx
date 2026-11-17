import "./ask.css";
import { Link, useLocation, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { SquarePen } from "lucide-react";
import { FOUNDER } from "../../../shared/deployment";
import {
  ButtonLink,
  SkeletonText,
  Tag,
  cx,
  formatRelative,
  readStorage,
  useMedia,
  useNow,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { threadKey } from "../agent/model";
import { agentQueries } from "../agent/queries";
import { readAccess } from "../workspace/access-model";
import { AskThreads } from "./ask-threads";
import { askOntologyId, threadGroups, threadsOf } from "./ask-model";
import { founderThreads, founderView } from "./founder-model";
import { withLoad } from "./model";
import { ontologyQueries, questionQueries } from "./queries";

// Ask's threads in the shell's sidebar, under the sections (and, on a narrow
// screen, Founder's page itself until a chat is chosen). Where the build has
// Founder: New chat, Founder's threads by recency (the dock's list: one list
// wherever Founder is asked; a grantee's only those it made whose grants it
// still holds), then the open ontology's Analyst threads from before Founder (a
// grantee’s own questions), each marked Earlier question and opened read only.
// Else, and where the WFE has Founder off, the Analyst's threads
// (ask-threads.tsx). Rendered outside the ontology's scope: it reads the
// ontology from the path, and its links keep the page's `?load=`.

/** The ask section's sidebar content: Founder's threads, or the Analyst's. */
export function AskSidebar({ onNavigate }: { onNavigate?: () => void }) {
  return FOUNDER ? (
    <FounderThreads onNavigate={onNavigate} />
  ) : (
    <AskThreads onNavigate={onNavigate} />
  );
}

export function FounderThreads({ onNavigate }: { onNavigate?: () => void }) {
  const { id: ws } = useWorkspace(),
    { pathname } = useLocation(),
    [params] = useSearchParams(),
    narrow = useMedia("(max-width: 800px)"),
    now = useNow(60_000);
  const o = askOntologyId(pathname);
  const threads = useQuery(agentQueries.threads(ws));
  if (threads.data === "off") return <AskThreads onNavigate={onNavigate} />;
  const list = threads.data?.threads;
  const view = founderView(params, narrow, list, readStorage(threadKey(ws)));
  // Off an ontology's page, /ask opens the last one opened and keeps the
  // query.
  const link = (to: { chat?: string; new?: boolean }) =>
    o
      ? withLoad(paths.ask(o, to), params.get("load"))
      : `/ask?${to.chat ? `chat=${encodeURIComponent(to.chat)}` : "new=1"}`;
  const open = view.kind === "chat" ? view.id : "";
  return (
    <div className="ak-panel">
      <div className="ak-panel-head">
        <ButtonLink
          to={link({ new: true })}
          icon={SquarePen}
          className="ak-new"
          aria-current={view.kind === "new" ? "page" : undefined}
          onClick={onNavigate}
        >
          New chat
        </ButtonLink>
      </div>
      <nav className="ak-threads" aria-label="Threads">
        {threads.isPending ? (
          <div className="ak-threads-note" aria-busy="true">
            <SkeletonText lines={3} />
          </div>
        ) : threads.isError ? (
          <p className="ak-threads-note" role="alert">
            Couldn’t read the threads: {threads.error.message}{" "}
            <button
              type="button"
              className="ak-retry"
              onClick={() => void threads.refetch()}
            >
              Retry
            </button>
          </p>
        ) : !list?.length ? (
          <p className="ak-threads-note">
            Nothing asked yet. Your chats with Founder stay here.
          </p>
        ) : (
          threadGroups(founderThreads(list), new Date(now)).map((g) => (
            <section key={g.label} className="ak-group">
              <h2>{g.label}</h2>
              <ul aria-label={g.label}>
                {g.threads.map((t) => (
                  <li key={t.id}>
                    <Link
                      to={link({ chat: t.id })}
                      onClick={onNavigate}
                      className={cx("ak-thread", t.id === open && "is-active")}
                      aria-current={t.id === open ? "page" : undefined}
                      title={t.title}
                    >
                      <span className="ak-thread-title">{t.title}</span>
                      <small>{formatRelative(t.last, now)}</small>
                    </Link>
                  </li>
                ))}
              </ul>
            </section>
          ))
        )}
        {o && (
          <AnalystThreads
            key={o}
            ontologyId={o}
            open={view.kind === "analyst" ? view.id : ""}
            now={now}
            onNavigate={onNavigate}
          />
        )}
      </nav>
    </div>
  );
}

/**
 * The ontology's Analyst threads from before Founder, read only: nothing
 * while they're read or when there are none, as they are history.
 */
function AnalystThreads({
  ontologyId: o,
  open,
  now,
  onNavigate,
}: {
  ontologyId: string;
  open: string;
  now: number;
  onNavigate?: () => void;
}) {
  const me = useWorkspace(),
    { id: ws } = me,
    [params] = useSearchParams();
  const ontology = useQuery(ontologyQueries.ontology(ws, o)).data?.ontology;
  const unread = !!ontology && readAccess(me, ontology) === "none";
  const list = useQuery({
    ...questionQueries.list(ws, o),
    enabled: !!ontology && !unread,
  });
  const threads = threadsOf(list.data?.questions || []);
  if (list.isError)
    return (
      <p className="ak-threads-note" role="alert">
        Couldn’t read the earlier questions: {list.error.message}{" "}
        <button
          type="button"
          className="ak-retry"
          onClick={() => void list.refetch()}
        >
          Retry
        </button>
      </p>
    );
  if (!threads.length) return null;
  return (
    <section className="ak-group">
      <h2>Earlier questions</h2>
      <ul aria-label="Earlier questions">
        {threads.map((t) => {
          const first = t.questions[0]!;
          return (
            <li key={t.id}>
              <Link
                to={withLoad(
                  paths.ask(o, { thread: t.id }),
                  params.get("load"),
                )}
                onClick={onNavigate}
                className={cx("ak-thread", t.id === open && "is-active")}
                aria-current={t.id === open ? "page" : undefined}
                title={first.question}
              >
                <span className="ak-thread-title">{first.question}</span>
                <small>
                  <Tag>Earlier question</Tag>
                  {t.live ? "Answering…" : formatRelative(t.last, now)}
                </small>
              </Link>
            </li>
          );
        })}
      </ul>
    </section>
  );
}
