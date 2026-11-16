import "./agent.css";
import {
  Suspense,
  lazy,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
  type RefObject,
  type TextareaHTMLAttributes,
} from "react";
import { Link, useLocation } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowUp,
  ChevronDown,
  CircleHelp,
  CircleAlert,
  TriangleAlert,
  LoaderCircle,
  Plug,
  MessageSquarePlus,
  Sparkles,
  Square,
  Trash2,
  Wrench,
  X,
} from "lucide-react";
import { LOCAL } from "../../../shared/deployment";
import type {
  AgentPageContext,
  AgentRuntime,
  AgentThreadResponse,
  FoundryAgentAction,
  FoundryAgentTurn,
} from "../../../shared/wfe";
import { ApiError, sentence } from "../../api";
import {
  Button,
  ConfirmDialog,
  DropdownMenu,
  EmptyState,
  MenuItem,
  MenuLabel,
  MenuSeparator,
  SkeletonText,
  Tag,
  cx,
  readStorage,
  writeStorage,
} from "../../kit";
import type { DockSeed } from "../../shell/context";
import { activeSection } from "../../shell/nav";
import { recordQuestionAsked } from "../../shell/star-prompt";
import { useWorkspace } from "../../ui";
import { loadVersion } from "../ontology/model";
import { dashboardQueries } from "../dashboards/queries";
import { ontologyPaths } from "../ontology/paths";
import { ontologyQueries } from "../ontology/queries";
import {
  AiRefusal,
  SettingsLinked,
  useAiConfigured,
} from "../workspace/ai-gate";
import { aiQuery, assistantCheckText, inForce } from "../workspace/ai-api";
import { ActionCard } from "./cards";
import { AnswerSummary, QuestionFlow } from "../../questions/flow";
import { answerText, questionName, readQuestions } from "../../questions/model";
import { Command } from "./runtime-settings";
import {
  ASK_PERSON,
  CODEX_UPDATE,
  EMPTY_STREAM,
  HELPER_COMMANDS,
  RUNTIMES,
  MAX_MESSAGE,
  promptHeight,
  NOT_RUN_TEXT,
  STARTS_WORK,
  STOP_TEXT,
  stepLabel,
  agentRefusal,
  answerBlocks,
  connectLink,
  isLiveTurn,
  pageContext,
  pretty,
  questionOf,
  textParts,
  threadKey,
  threadRuntime,
  type RuntimeState,
  type ThreadRuntime,
  type AgentBlock,
  type TurnStream,
} from "./model";
import {
  EVENTS_POLL_MS,
  RUNTIME_POLL_MS,
  agentQueries,
  cancelAgentTurn,
  deleteAgentThread,
  sendAgentMessage,
  threadBusy,
  turnEvents,
} from "./queries";

// The Analyst's answer under the step that asked it: its card and the
// explore reads it needs load only once a thread holds one.
const InlineQuestion = lazy(() => import("../ontology/ask-inline"));

/**
 * Founder's dock: the person's threads with it, the open thread's
 * turns as they stream (polled), the actions it prepared as cards, and the
 * composer, which sends what page the person is on (ids only) unless they
 * take it off. Rendered by the shell outside the page's routes, so it reads
 * only the URL and the query cache. Everything the model wrote is plain
 * text; only paths into this app are links (`onNavigate` runs on each, to
 * close a phone's drawer). Left out of session replay whole.
 */
export function AgentDock({
  onClose,
  onNavigate,
  seed,
}: {
  onClose: () => void;
  onNavigate?: () => void;
  // A compose (useShell().assistant.compose): a new thread whose composer
  // starts with its text; each one remounts the composer.
  seed?: DockSeed | null;
}) {
  const { id: ws } = useWorkspace(),
    qc = useQueryClient();
  const [threadId, setThreadId] = useState(() =>
    seed ? "" : readStorage(threadKey(ws)),
  );
  const open = (id: string) => {
    writeStorage(threadKey(ws), id || null);
    setThreadId(id);
  };
  useEffect(() => setThreadId(readStorage(threadKey(ws))), [ws]);
  const seeded = seed?.n;
  useEffect(() => {
    if (seeded) open("");
  }, [seeded]);
  const threads = useQuery(agentQueries.threads(ws));
  const thread = useQuery({
    ...agentQueries.thread(ws, threadId),
    enabled: !!threadId,
    retry: false,
  });
  // A thread deleted elsewhere opens a new one.
  const gone = thread.error instanceof ApiError && thread.error.status === 404;
  useEffect(() => {
    if (gone) open("");
  }, [gone]);
  const [deleting, setDeleting] = useState(false);
  const title =
    (threadId && thread.data?.thread.title) ||
    (threadId ? "Thread" : "New thread");
  const disabled = agentRefusal(threads.error) === "FOUNDRY_AGENT_DISABLED";
  const where = useThreadRuntime(threadId, thread.data);
  return (
    <div className="ag-dock" data-replay-block>
      <header className="ag-head">
        <Sparkles size={16} aria-hidden className="ag-head-icon" />
        <h2 className="ag-head-title">Founder</h2>
        <DropdownMenu
          label={`Threads: ${title}`}
          align="end"
          triggerClassName="hf-btn hf-btn--ghost hf-btn--sm ag-threads-trigger"
          trigger={
            <>
              <span className="ag-threads-title">{title}</span>
              <ChevronDown size={14} aria-hidden />
            </>
          }
        >
          <MenuItem icon={MessageSquarePlus} onSelect={() => open("")}>
            New thread
          </MenuItem>
          {!!threads.data?.threads?.length && (
            <>
              <MenuSeparator />
              <MenuLabel>Threads</MenuLabel>
              {threads.data.threads.map((t) => (
                <MenuItem
                  key={t.id}
                  checked={t.id === threadId}
                  onSelect={() => open(t.id)}
                >
                  {t.title || "Untitled"}
                </MenuItem>
              ))}
            </>
          )}
          {!!threadId && (
            <>
              <MenuSeparator />
              <MenuItem icon={Trash2} danger onSelect={() => setDeleting(true)}>
                Delete this thread
              </MenuItem>
            </>
          )}
        </DropdownMenu>
        <button
          type="button"
          className="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon"
          aria-label="Close Founder"
          title="Close Founder"
          onClick={onClose}
        >
          <X size={16} aria-hidden />
        </button>
      </header>
      {disabled ? (
        <EmptyState
          size="sm"
          icon={Sparkles}
          title="Founder isn’t available here"
          text="It is off for this Foundry."
        />
      ) : (
        <>
          {where && where.runtime !== "api" && (
            <RuntimeLine
              runtime={where.runtime}
              state={where.state}
              helper={where.helper}
              thread={!!threadId}
              onNavigate={onNavigate}
            />
          )}
          <Transcript
            data={threadId ? thread.data : undefined}
            loading={!!threadId && thread.isPending}
            failed={!!threadId && thread.isError && !gone}
            onRetry={() => void thread.refetch()}
            onNavigate={onNavigate}
          />
          <div className="ag-dock-composer">
            <Composer
              key={seed?.n ?? 0}
              initialText={seed?.text}
              threadId={threadId}
              thread={threadId ? thread.data : undefined}
              runtime={where?.runtime ?? "api"}
              hostState={where?.state ?? "ready"}
              minRows={threadId ? 1 : 2}
              onNavigate={onNavigate}
              onSent={(id) => {
                if (id !== threadId) open(id);
                void qc.invalidateQueries({
                  queryKey: [ws, "agent", "threads"],
                });
                void qc.invalidateQueries({
                  queryKey: [ws, "agent", "thread", id],
                });
              }}
            />
          </div>
        </>
      )}
      {deleting && (
        <ConfirmDialog
          title="Delete this thread?"
          description="Its messages and the actions it prepared go with it. An answer still being written stops."
          confirmLabel="Delete"
          tone="danger"
          onCancel={() => setDeleting(false)}
          onConfirm={async () => {
            await deleteAgentThread(ws, threadId);
            setDeleting(false);
            open("");
            await qc.invalidateQueries({ queryKey: [ws, "agent", "threads"] });
          }}
        />
      )}
    </div>
  );
}

/**
 * Where a thread runs (a new one, `threadId` empty: the workspace's setting,
 * which only a local install's owner reads), read often while its helper
 * isn't running; undefined until the thread is read.
 */
export function useThreadRuntime(
  threadId: string,
  thread?: AgentThreadResponse,
) {
  const { id: ws, role } = useWorkspace();
  const runtimeNow: AgentRuntime | undefined = threadId
    ? thread?.thread.runtime
    : undefined;
  const setting = useQuery({
    ...agentQueries.runtime(ws),
    enabled: LOCAL && role === "owner",
    retry: false,
    refetchInterval: (q) =>
      q.state.data && threadRuntime(runtimeNow, q.state.data).state !== "ready"
        ? RUNTIME_POLL_MS
        : false,
  });
  return !threadId || thread
    ? threadRuntime(runtimeNow, setting.data)
    : undefined;
}

/**
 * Where the open thread (or a new one) runs, when it is a helper's: on the
 * person's subscription; while none runs it, how to start the helper paired
 * for it, or, with none paired, to pair one in Settings (the WFE refuses to
 * send until one runs).
 */
export function RuntimeLine({
  runtime,
  state,
  helper,
  thread,
  onNavigate,
}: {
  runtime: Exclude<AgentRuntime, "api">;
  state: RuntimeState;
  helper?: ThreadRuntime["helper"];
  // An open thread's, not the workspace's for a new one.
  thread: boolean;
  onNavigate?: () => void;
}) {
  const r = RUNTIMES[runtime],
    which = thread ? "this thread" : "new threads";
  return (
    <div
      className={cx("ag-runtime", state !== "ready" && "is-offline")}
      role={state === "ready" ? undefined : "status"}
    >
      <p className="ag-runtime-line">
        {state !== "ready" ? (
          <TriangleAlert size={14} aria-hidden />
        ) : runtime === "codex" ? (
          <span className="ag-mark-openai" aria-hidden />
        ) : (
          <img src="/providers/claude.svg" alt="" />
        )}
        <span>
          {state === "ready" ? (
            `${thread ? "This thread runs" : "New threads run"} on ${r.on}, through ${r.cli} on your computer.`
          ) : state === "stopped" ? (
            `${r.cli}’s helper isn’t running, so ${which} can’t be sent. Start it on your computer:`
          ) : state === "outdated" ? (
            `The ${r.cli} on ${helper?.host ?? "your computer"}${helper?.version ? ` is ${helper.version},` : " is"} older than ${helper?.min || "Foundry runs"}, so ${which} can’t be sent. Update it there, then send again in a minute:`
          ) : state === "unstarted" ? (
            `The ${r.cli} on ${helper?.host ?? "your computer"} didn’t start, so ${which} can’t be sent. To see why, run this there:`
          ) : (
            <SettingsLinked
              text={`No helper running ${r.cli} is paired, so ${which} can’t be sent. Pair one in Settings › AI models, then start it.`}
              onNavigate={onNavigate}
            />
          )}
        </span>
      </p>
      {state === "outdated" && (
        <Command text={CODEX_UPDATE} label="update command" />
      )}
      {state === "unstarted" && (
        <Command text="codex --version" label="check command" />
      )}
      {state === "stopped" && (
        <>
          <Command text={HELPER_COMMANDS.run} label="run command" />
          <p className="ag-runtime-line">
            To keep it running at every login, use install in place of run.
          </p>
        </>
      )}
    </div>
  );
}

/**
 * The open thread's turns, following its end until the reader scrolls up:
 * the dock's, and a page's that holds one conversation (a new automation's).
 */
export function Transcript({
  data,
  loading,
  failed,
  onRetry,
  onNavigate,
}: {
  data?: AgentThreadResponse;
  loading: boolean;
  // The thread couldn't be read (not that it is gone).
  failed: boolean;
  onRetry: () => void;
  onNavigate?: () => void;
}) {
  const { grantee } = useWorkspace();
  const log = useRef<HTMLDivElement>(null),
    body = useRef<HTMLDivElement>(null),
    stick = useRef(true),
    lastTop = useRef(0),
    turnsSeen = useRef(0);
  const threadId = data?.thread.id,
    turns = data?.turns?.length ?? 0;
  useLayoutEffect(() => {
    const el = log.current,
      inner = body.current;
    if (!el || !inner || typeof ResizeObserver === "undefined") return;
    // Another thread opens at its end, wherever the last one was read.
    stick.current = true;
    lastTop.current = 0;
    const follow = () => {
      if (stick.current) el.scrollTop = el.scrollHeight;
    };
    follow();
    // The turns growing, and the log itself shrinking (the composer above
    // the keyboard, or the page it sends coming and going).
    const observer = new ResizeObserver(follow);
    observer.observe(inner);
    observer.observe(el);
    return () => observer.disconnect();
  }, [threadId]);
  // A turn starts with what the person did (a message, a card run, answers),
  // so a new one follows the end again, however far up they had read.
  useLayoutEffect(() => {
    const el = log.current;
    if (turns > turnsSeen.current && turnsSeen.current > 0 && el) {
      stick.current = true;
      el.scrollTop = el.scrollHeight;
    }
    turnsSeen.current = turns;
  }, [threadId, turns]);
  const busy = threadBusy(data);
  const actions = useMemo(
    () => new Map((data?.actions || []).map((a) => [a.id, a])),
    [data?.actions],
  );
  return (
    <div
      ref={log}
      className="ag-log"
      role="log"
      aria-label="Conversation"
      tabIndex={0}
      onScroll={(e) => {
        const el = e.currentTarget;
        // At the end it follows again; it stops only when its reader moves
        // up. Turns growing after a scroll to the end is not that: the
        // scroll's event can come after they grew, short of the new end.
        if (el.scrollHeight - el.scrollTop - el.clientHeight < 48)
          stick.current = true;
        else if (el.scrollTop < lastTop.current) stick.current = false;
        lastTop.current = el.scrollTop;
      }}
    >
      <div ref={body} className="ag-log-body">
        {failed ? (
          <EmptyState
            size="sm"
            icon={CircleAlert}
            title="This thread couldn’t be read"
            action={
              <Button size="sm" onClick={onRetry}>
                Try again
              </Button>
            }
          />
        ) : loading ? (
          <SkeletonText lines={4} />
        ) : !data?.turns?.length ? (
          <EmptyState
            size="sm"
            icon={Sparkles}
            title="Ask Founder"
            text={
              grantee
                ? "It answers questions about the data shared with you, with cited answers. It can’t change anything in this workspace."
                : "It answers questions about your data with cited answers, and works in your workspace. Changes wait as cards you click to run; a dashboard you ask for it makes at once, with Undo."
            }
          />
        ) : (
          data.turns.map((t, i, all) => (
            <TurnView
              key={t.id}
              turn={t}
              helper={(data.thread.runtime ?? "api") !== "api"}
              recent={all.length - i <= RECENT_TURNS}
              scroller={log}
              actions={actions}
              busy={busy}
              asked={
                !t.asked
                  ? "no"
                  : i === all.length - 1 && t.status === "done"
                    ? "open"
                    : all.some((x) => x.answers?.turnId === t.id)
                      ? "answered"
                      : "passed"
              }
              asking={
                t.answers
                  ? all.find((x) => x.id === t.answers!.turnId)
                  : undefined
              }
              onNavigate={onNavigate}
            />
          ))
        )}
      </div>
    </div>
  );
}

// The newest turns of a thread, whose events are read as it opens; older
// ones are read once they near the view, so a long thread opens with a few
// reads, not one per turn.
const RECENT_TURNS = 4;

/**
 * Whether el has come within a screen of scroller's view, once (at once
 * when `already`).
 */
function useNearView(
  el: RefObject<HTMLElement | null>,
  scroller: RefObject<HTMLElement | null>,
  already: boolean,
) {
  const [seen, setSeen] = useState(false);
  const near = already || seen;
  useEffect(() => {
    const node = el.current;
    if (near || !node) return;
    if (typeof IntersectionObserver === "undefined") {
      setSeen(true);
      return;
    }
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) setSeen(true);
      },
      { root: scroller.current, rootMargin: "100% 0px" },
    );
    observer.observe(node);
    return () => observer.disconnect();
  }, [near]);
  return near;
}

/**
 * A turn's events, read once `enabled` and polled after the last seq until
 * it is complete or a read fails. The thread is read again once it ends, or
 * a card shows, so its statuses and actions follow.
 */
function useTurnStream(turn: FoundryAgentTurn, enabled: boolean) {
  const { id: ws } = useWorkspace(),
    qc = useQueryClient();
  const key = [ws, "agent", "events", turn.id];
  const stream = useQuery({
    queryKey: key,
    queryFn: () =>
      turnEvents(ws, turn.id, qc.getQueryData<TurnStream>(key) ?? EMPTY_STREAM),
    enabled,
    refetchInterval: (s) =>
      s.state.error || s.state.data?.complete ? false : EVENTS_POLL_MS,
    staleTime: (s) => (s.state.data?.complete ? Infinity : 0),
  });
  const data = stream.data ?? EMPTY_STREAM;
  // What the thread was last read after: a turn that ends, or a new card,
  // reads it again; a turn read once it ended doesn't.
  const seen =
      stream.data &&
      `${data.complete}:${data.blocks.filter((b) => b.kind === "card").length}`,
    last = useRef(seen);
  useEffect(() => {
    const was = last.current;
    last.current = seen;
    if (!was || !seen || was === seen) return;
    void qc.invalidateQueries({
      queryKey: [ws, "agent", "thread", turn.threadId],
    });
  }, [seen]);
  return {
    data,
    loading: enabled && stream.isPending,
    failed: stream.isError,
    retry: () => void stream.refetch(),
  };
}

function TurnView({
  turn,
  helper,
  recent,
  scroller,
  actions,
  busy,
  asked,
  asking,
  onNavigate,
}: {
  turn: FoundryAgentTurn;
  // Whether it asked its person, and how that stands: open (the thread's
  // newest turn, done: answered here), answered by a later turn, or passed
  // (the conversation went on).
  asked: AskedState;
  // An answers turn's: the turn whose questions it answers.
  asking?: FoundryAgentTurn;
  // Its thread runs on a helper: a queued turn waits for one to take it.
  helper: boolean;
  // One of the thread's newest: read as it opens.
  recent: boolean;
  // The conversation's scrolling box.
  scroller: RefObject<HTMLElement | null>;
  actions: ReadonlyMap<string, FoundryAgentAction>;
  busy: boolean;
  onNavigate?: () => void;
}) {
  const answer = useRef<HTMLDivElement>(null),
    near = useNearView(answer, scroller, recent || isLiveTurn(turn));
  const read = useTurnStream(turn, near),
    stream = read.data,
    live = isLiveTurn(turn) && !stream.complete;
  const failed =
    stream.ended?.status === "failed" || turn.status === "failed"
      ? stream.ended?.error || turn.error || "it failed"
      : "";
  const stop =
    stream.ended && stream.ended.status === "done"
      ? STOP_TEXT[stream.ended.reason]
      : null;
  // An Analyst question shows once, under the last step that returned it.
  const answers = useMemo(() => answerBlocks(stream.blocks), [stream.blocks]);
  return (
    <>
      {turn.kind === "answers" ? (
        <div className="ag-msg ag-msg--person">
          <span className="ag-who">You answered</span>
          <AnswersOf turn={turn} asking={asking} />
        </div>
      ) : (
        turn.kind !== "action_result" && (
          <div className="ag-msg ag-msg--person">
            <span className="ag-who">You</span>
            <p className="ag-text">{turn.text}</p>
          </div>
        )
      )}
      <div
        ref={answer}
        className="ag-msg ag-msg--agent"
        aria-busy={live || read.loading || undefined}
      >
        <span className="ag-who">Founder</span>
        {read.loading && !stream.blocks.length && <SkeletonText lines={2} />}
        {read.failed && (
          <p className="ag-card-error">
            <CircleAlert size={14} aria-hidden /> This answer couldn’t be read.{" "}
            <Button size="sm" variant="ghost" onClick={read.retry}>
              Try again
            </Button>
          </p>
        )}
        {stream.blocks.map((b, i) =>
          b.kind === "asked" ? (
            <AskedBlock
              key={i}
              block={b}
              turn={turn}
              state={asked}
              busy={busy}
              onNavigate={onNavigate}
            />
          ) : (
            <Block
              key={i}
              block={b}
              answer={answers.has(i)}
              actions={actions}
              busy={busy}
              onNavigate={onNavigate}
            />
          ),
        )}
        {stream.live && (
          <LiveText key={stream.live.step} text={stream.live.text} />
        )}
        {live && !read.failed && !stream.live && (
          <p className="ag-working">
            <LoaderCircle size={14} className="hf-spin" aria-hidden />
            {turn.status === "queued" && (turn.attempts || 0) > 0
              ? `Retrying${turn.error ? `: ${turn.error}` : ""}…`
              : turn.status === "queued"
                ? helper
                  ? "Waiting for the helper on your computer to take it…"
                  : "Waiting to start…"
                : "Working…"}
          </p>
        )}
        {turn.status === "cancelled" && <p className="ag-note">Stopped.</p>}
        {failed && (
          <p className="ag-card-error">
            <CircleAlert size={14} aria-hidden />
            <span>
              <SettingsLinked text={sentence(failed)} onNavigate={onNavigate} />
            </span>
          </p>
        )}
        {stop && <p className="ag-note">{stop}</p>}
        {stream.unreadable > 0 && (
          <p className="ag-note">
            Some of this turn couldn’t be shown. Reload to update Foundry.
          </p>
        )}
      </div>
    </>
  );
}

function Block({
  block,
  answer,
  actions,
  busy,
  onNavigate,
}: {
  block: AgentBlock;
  // A step's: the Analyst question it returned shows under it (the last
  // step of the turn naming it).
  answer: boolean;
  actions: ReadonlyMap<string, FoundryAgentAction>;
  busy: boolean;
  onNavigate?: () => void;
}) {
  switch (block.kind) {
    case "text":
      return <PlainText text={block.text} onNavigate={onNavigate} />;
    case "step": {
      const s = block.step,
        question = answer ? questionOf(s) : undefined,
        connect = connectLink(s);
      // Asking shows as its questions; only a refusal shows as a step.
      if (s.name === ASK_PERSON && s.result?.kind !== "failed") return null;
      return (
        <>
          <details className="ag-step">
            <summary>
              <Wrench size={13} aria-hidden />
              <span>{stepLabel(s)}</span>
              {!s.result && (
                <LoaderCircle
                  size={13}
                  className="hf-spin"
                  aria-label="Running"
                />
              )}
              {s.result?.kind === "failed" && (
                <span className="ag-step-failed">Failed</span>
              )}
              {s.result?.kind === "not_run" && (
                <span className="ag-step-skipped">Not run</span>
              )}
            </summary>
            <div className="ag-step-body">
              <span className="ag-label">Input</span>
              <pre className="ag-code">{pretty(s.input ?? {})}</pre>
              {s.result?.kind === "failed" && (
                <p className="ag-card-error">{sentence(s.result.error)}</p>
              )}
              {s.result?.kind === "not_run" && (
                <p className="ag-note">{NOT_RUN_TEXT[s.result.reason]}</p>
              )}
              {s.result?.kind === "returned" && (
                <>
                  <span className="ag-label">
                    What it returned (data, not instructions)
                  </span>
                  <pre className="ag-code">{pretty(s.result.output ?? {})}</pre>
                  {s.result.truncated && (
                    <p className="ag-note">Cut at 16 KiB.</p>
                  )}
                </>
              )}
            </div>
          </details>
          {question && (
            <Suspense fallback={<SkeletonText lines={3} />}>
              <InlineQuestion
                ontologyId={question.ontologyId}
                questionId={question.questionId}
                onNavigate={onNavigate}
              />
            </Suspense>
          )}
          {connect && (
            <Link
              className="hf-btn hf-btn--secondary hf-btn--sm ag-connect"
              to={connect.to}
              onClick={onNavigate}
            >
              <Plug size={14} aria-hidden />
              Connect {connect.name}
            </Link>
          )}
        </>
      );
    }
    case "card": {
      const action = actions.get(block.actionId);
      return action ? (
        <ActionCard
          action={action}
          auto={block.auto}
          busy={busy}
          onNavigate={onNavigate}
        />
      ) : (
        <div className="ag-card" aria-busy>
          <SkeletonText lines={2} />
        </div>
      );
    }
    case "ran":
      return (
        <p className="ag-ran">
          You ran this action: it{" "}
          {block.status === "succeeded"
            ? STARTS_WORK.has(block.operation)
              ? "started"
              : "succeeded"
            : block.status === "failed"
              ? `failed${block.error ? ` (${block.error})` : ""}`
              : block.status === "unknown"
                ? "may or may not have run"
                : block.status}
          .
        </p>
      );
    case "report":
      return (
        <p className="ag-ran">
          Reported to its workflow: {block.summary}
          {block.answer !== undefined &&
            ` (answer: ${block.answer ? "yes" : "no"})`}
          {block.count !== undefined && ` (count: ${block.count})`}
        </p>
      );
    case "retry":
      return <p className="ag-note">Trying again (attempt {block.attempt}).</p>;
    case "asked":
      // Shown by TurnView, which knows how the questions stand.
      return null;
  }
}

/** How a turn's questions stand: see TurnView's `asked`. */
type AskedState = "no" | "open" | "answered" | "passed";

/**
 * The questions a turn asked its person: while they are open, the
 * question-by-question flow, whose answers go as the thread's next turn;
 * after, a line saying so (the answers show as the person's message). A
 * relayed generation's answers go to its draft as they are sent, so its
 * questions close once the draft no longer waits on that round: answered
 * on its page, or moved on.
 */
function AskedBlock({
  block,
  turn,
  state,
  busy,
  onNavigate,
}: {
  block: Extract<AgentBlock, { kind: "asked" }>;
  turn: FoundryAgentTurn;
  state: AskedState;
  busy: boolean;
  onNavigate?: () => void;
}) {
  const { id: ws } = useWorkspace(),
    location = useLocation(),
    qc = useQueryClient();
  const [pending, setPending] = useState(false),
    [error, setError] = useState<unknown>(null),
    [token] = useState(() => crypto.randomUUID());
  // The turn's own record, once read, else what its event showed.
  const questions = readQuestions(turn.asked?.questions ?? block.questions);
  const relayed = turn.asked?.generation,
    draft = useQuery({
      ...ontologyQueries.generation(
        ws,
        relayed?.ontologyId ?? "",
        relayed?.generationId ?? "",
      ),
      enabled: state === "open" && !!relayed?.generationId,
    });
  const read = draft.data?.generation;
  const moved =
    !!relayed &&
    !!read &&
    (read.status !== "ONTOLOGY_GENERATION_STATUS_NEEDS_INPUT" ||
      (read.questionRound || 1) !== (relayed.round || 1));
  const draftLink = relayed?.ontologyId && relayed.generationId && (
    <Link
      to={ontologyPaths.generation(relayed.ontologyId, relayed.generationId)}
      onClick={onNavigate}
    >
      Open the draft
    </Link>
  );
  const what = block.generation
    ? `the ontology agent’s questions about ${block.generation}`
    : questions?.length === 1
      ? "a question"
      : `${questions?.length ?? "some"} questions`;
  if (!questions)
    return (
      <p className="ag-note">
        Founder asked questions this page can’t show. Reload to update Foundry.
      </p>
    );
  if (state !== "open" || moved)
    return (
      <p className="ag-ran">
        <CircleHelp size={13} aria-hidden />
        <span>
          Asked you {what}
          {state === "answered"
            ? ". "
            : moved
              ? ": the draft no longer waits on them here (answered on its page, or it moved on). "
              : ", which went unanswered."}
          {moved && draftLink}
        </span>
      </p>
    );
  return (
    <section className="ag-asked" aria-label="Questions for you">
      <p className="ag-asked-title">
        <CircleHelp size={15} aria-hidden />
        {block.generation
          ? `The ontology agent asks about ${block.generation}`
          : "Questions for you"}
      </p>
      <QuestionFlow
        questions={questions}
        storageKey={`agent:${turn.id}`}
        agent={relayed ? "the agent" : "Founder"}
        sendLabel={relayed ? "Send answers to the draft" : "Send answers"}
        pending={pending}
        disabled={busy}
        error={
          !!error && (
            <p className="ag-card-error" role="alert">
              <span>
                {agentRefusal(error) === "FOUNDRY_AGENT_QUESTIONS_CLOSED" ? (
                  relayed ? (
                    <>
                      These questions are closed: the draft no longer waits on
                      them (answered on its page, or it moved on). {draftLink}
                    </>
                  ) : (
                    "These questions are closed: they were answered, or the conversation went on."
                  )
                ) : (
                  <SettingsLinked
                    text={sentence((error as Error).message)}
                    onNavigate={onNavigate}
                  />
                )}
              </span>
            </p>
          )
        }
        onSend={async (answers) => {
          setPending(true);
          setError(null);
          try {
            await sendAgentMessage(
              ws,
              {
                threadId: turn.threadId || "",
                answers: { turnId: turn.id, answers },
                clientToken: token,
              },
              activeSection(location.pathname) ?? "none",
            );
            return true;
          } catch (e) {
            setError(e);
            return false;
          } finally {
            setPending(false);
            await qc.invalidateQueries({
              queryKey: [ws, "agent", "thread", turn.threadId],
            });
            // The draft carries on from them.
            if (relayed?.ontologyId)
              await qc.invalidateQueries({
                queryKey: [ws, "ontology", relayed.ontologyId],
              });
          }
        }}
      />
    </section>
  );
}

/**
 * An answers turn's answers, each beside its question as the asking turn
 * asked it; a question left out was left to Founder.
 */
function AnswersOf({
  turn,
  asking,
}: {
  turn: FoundryAgentTurn;
  asking?: FoundryAgentTurn;
}) {
  const given = turn.answers?.answers ?? [],
    questions = readQuestions(asking?.asked?.questions) ?? [];
  const rows = questions.length
    ? questions.map((q, i) => {
        const a = given.find((x) => x.questionId === q.id);
        return {
          name: questionName(q, i),
          question: q.text,
          answer: a
            ? answerText(a)
            : `Left to ${asking?.asked?.generation ? "the agent" : "Founder"}`,
          left: !a,
        };
      })
    : given.map((a, i) => ({
        name: `Question ${i + 1}`,
        question: "",
        answer: answerText(a),
      }));
  return <AnswerSummary rows={rows} label="Your answers" />;
}

/**
 * Text the model is writing: each poll's deltas flow in over the next
 * poll's time, a few characters a frame, rather than in jumps (at once for
 * reduced motion). Plain, with no links until the step's text replaces it.
 */
function LiveText({ text }: { text: string }) {
  const [shown, setShown] = useState(0),
    at = useRef(0);
  useEffect(() => {
    const from = at.current,
      left = text.length - from;
    if (left <= 0) return;
    if (window.matchMedia?.("(prefers-reduced-motion: reduce)").matches) {
      at.current = text.length;
      setShown(text.length);
      return;
    }
    const start = performance.now();
    let frame = requestAnimationFrame(function reveal(now) {
      let n = Math.min(
        text.length,
        from + Math.ceil((left * Math.max(0, now - start)) / EVENTS_POLL_MS),
      );
      // Never between a surrogate pair's halves.
      if (/[\uD800-\uDBFF]/.test(text[n - 1] || "")) n += 1;
      at.current = n;
      setShown(n);
      if (n < text.length) frame = requestAnimationFrame(reveal);
    });
    return () => cancelAnimationFrame(frame);
  }, [text]);
  return <p className="ag-text ag-live">{text.slice(0, shown).trimEnd()}</p>;
}

/** The model's words as plain text, with only paths into this app as links. */
function PlainText({
  text,
  onNavigate,
}: {
  text: string;
  onNavigate?: () => void;
}) {
  const parts = textParts(text, window.location.origin);
  return (
    <p className="ag-text">
      {parts.map((p, i) =>
        p.kind === "link" ? (
          <Link key={i} to={p.to} onClick={onNavigate}>
            {p.text}
          </Link>
        ) : (
          <span key={i}>{p.text}</span>
        ),
      )}
    </p>
  );
}

/**
 * What the person writes, with the page they're on as a chip they can
 * take off. Send waits while a turn of the thread is live or an action of
 * it runs, and while the setting's tool check failed (its turns would fail
 * at once, each a message of the hour's), saying why; Stop ends the live
 * turn. A page that holds a conversation gives its own `placeholder` and
 * may start it with `initialText` (a suggestion's prompt) and `about`, the
 * ontology that suggestion was written for, which joins the page context
 * where the page names none (a new workflow's page), on the chip too; a new
 * conversation's box starts taller (`minRows`) than a reply's. A grantee's
 * Founder only reads and asks about what its grants cover, so its default
 * says so, and the workspace's AI setting (which it can't read) isn't read.
 */
export function Composer({
  threadId,
  thread,
  runtime,
  hostState,
  onSent,
  onNavigate,
  placeholder,
  initialText = "",
  about,
  minRows = 2,
}: {
  // The open thread; empty for a new one. `thread` once it is read.
  threadId: string;
  thread?: AgentThreadResponse;
  // Where its turns run: a helper's run on its owner's subscription, so
  // the workspace's AI setting doesn't hold them back.
  runtime: AgentRuntime;
  // Whether a helper runs them now (always ready on the worker).
  hostState: RuntimeState;
  onSent: (threadId: string) => void;
  onNavigate?: () => void;
  placeholder?: string;
  initialText?: string;
  // An ontology id.
  about?: string;
  minRows?: number;
}) {
  const { id: ws, role, grantee } = useWorkspace(),
    location = useLocation(),
    qc = useQueryClient();
  const page = pageContext(location.pathname, location.search, about),
    pageKey = page
      ? `${page.ontologyId || ""}/${page.loadId || ""}/${page.dashboardId || ""}/${page.newAutomation ? "new" : ""}`
      : "";
  const [dropped, setDropped] = useState("");
  const context = page && dropped !== pageKey ? page : null;
  const [text, setText] = useState(initialText),
    [token, setToken] = useState(() => crypto.randomUUID()),
    [pending, setPending] = useState(false),
    [error, setError] = useState<unknown>(null);
  const onModel = runtime === "api",
    aiReady = useAiConfigured(),
    modelReady = onModel ? aiReady : true;
  const setting = useQuery({ ...aiQuery(ws), enabled: !grantee }).data,
    now = setting && inForce(setting),
    checkFailed =
      onModel && now?.source === "workspace" && assistantCheckText(now.config);
  const hint = useId(),
    input = useRef<HTMLTextAreaElement>(null),
    sending = useRef(false);
  // The dock opens ready to write, the caret after any text it starts with
  // and that end in view: before paint, and again once the field has sized
  // itself to its text (PromptInput), which a resize may redo after.
  useLayoutEffect(() => {
    const end = () => {
      const el = input.current;
      if (!el) return;
      el.focus();
      el.setSelectionRange(el.value.length, el.value.length);
      el.scrollTop = el.scrollHeight;
    };
    end();
    const frame = requestAnimationFrame(end);
    return () => cancelAnimationFrame(frame);
  }, []);
  const live = thread?.turns?.find(isLiveTurn);
  const busy = threadBusy(thread) || (!!threadId && !thread);
  const length = [...text.trim()].length,
    tooLong = length > MAX_MESSAGE;
  const blocked =
    busy ||
    !!live ||
    !text.trim() ||
    tooLong ||
    modelReady === false ||
    !!checkFailed;
  const submit = async () => {
    // Enter submits past a disabled Send, and twice before a render: the
    // same checks, and one send at a time.
    if (blocked || sending.current) return;
    sending.current = true;
    setPending(true);
    setError(null);
    try {
      const r = await sendAgentMessage(
        ws,
        {
          ...(threadId ? { threadId } : {}),
          text: text.trim(),
          ...(context ? { pageContext: context } : {}),
          clientToken: token,
        },
        activeSection(location.pathname) ?? "none",
      );
      setText("");
      setToken(crypto.randomUUID());
      void recordQuestionAsked();
      onSent(r.thread.id);
    } catch (e) {
      setError(e);
      // Nothing was queued: read whether its helper runs again.
      if (agentRefusal(e) === "FOUNDRY_AGENT_HOST_OFFLINE")
        void qc.invalidateQueries({
          queryKey: agentQueries.runtime(ws).queryKey,
        });
    } finally {
      sending.current = false;
      setPending(false);
    }
  };
  const caption: ReactNode = checkFailed ? (
    // One span, so the hint's flex row wraps the sentence as text.
    <span>
      <SettingsLinked
        text={
          role === "owner"
            ? `${checkFailed} Open Settings › AI models.`
            : "Founder can’t run on this workspace’s AI model. An owner can fix this in Settings › AI models."
        }
        onNavigate={onNavigate}
      />
    </span>
  ) : tooLong ? (
    `${length - MAX_MESSAGE} characters too long.`
  ) : busy ? (
    "You can send once Founder has finished."
  ) : // Nothing to say while idle; the line keeps its height so nothing jumps.
  null;
  return (
    <form
      className="ag-composer"
      aria-label="Message Founder"
      aria-busy={pending || undefined}
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <div
        className="ag-box"
        onClick={(e) => {
          if (e.target === e.currentTarget) input.current?.focus();
        }}
      >
        {context && (
          <PageChip context={context} onDrop={() => setDropped(pageKey)} />
        )}
        <PromptInput
          inputRef={input}
          className="ag-input"
          aria-label="Message"
          aria-describedby={hint}
          minRows={minRows}
          maxRows={8}
          value={text}
          placeholder={
            placeholder ??
            (grantee
              ? "Ask about the data shared with you…"
              : "Ask about your workspace, or ask it to do something…")
          }
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (
              e.key === "Enter" &&
              !e.shiftKey &&
              !e.nativeEvent.isComposing
            ) {
              e.preventDefault();
              e.currentTarget.form?.requestSubmit();
            }
          }}
        />
        {live ? (
          <button
            type="button"
            className="ag-send"
            aria-label="Stop"
            title="Stop"
            onClick={async () => {
              await cancelAgentTurn(ws, live.id).catch(() => undefined);
              await qc.invalidateQueries({
                queryKey: [ws, "agent", "thread", threadId],
              });
            }}
          >
            <Square size={13} fill="currentColor" aria-hidden />
          </button>
        ) : (
          <button
            type="submit"
            className="ag-send"
            aria-label="Send"
            title="Send"
            disabled={blocked || pending}
          >
            {pending ? (
              <LoaderCircle size={17} className="hf-spin" aria-hidden />
            ) : (
              <ArrowUp size={17} aria-hidden />
            )}
          </button>
        )}
      </div>
      <p
        id={hint}
        className={cx("ag-status", (tooLong || !!checkFailed) && "is-error")}
      >
        {caption}
      </p>
      <AiRefusal action="Founder" error={error} ready={modelReady}>
        {!!error && (
          <p className="ag-status is-error" role="alert">
            {agentRefusal(error) !== "FOUNDRY_AGENT_HOST_OFFLINE" ? (
              sentence((error as Error).message)
            ) : hostState === "unpaired" ? (
              <SettingsLinked
                text="No helper is paired to run it: pair one in Settings › AI models, start it, then send again."
                onNavigate={onNavigate}
              />
            ) : hostState === "outdated" ? (
              `Its helper’s Codex is too old: update it with ${CODEX_UPDATE}, then send again in a minute.`
            ) : hostState === "unstarted" ? (
              "Its helper’s Codex didn’t start: run codex --version on that computer to see why, then send again in a minute."
            ) : (
              `Its helper isn’t running on your computer: start it with ${HELPER_COMMANDS.run}, then send again.`
            )}
          </p>
        )}
      </AiRefusal>
    </form>
  );
}

/**
 * A textarea that grows with its text from `minRows` to `maxRows` lines,
 * then scrolls down, never sideways (promptHeight): Founder's composer's
 * and the Analyst's. It sets its own height, so a long placeholder (a
 * suggestion previewed) never moves the page.
 */
export function PromptInput({
  value,
  minRows,
  maxRows,
  inputRef,
  ...rest
}: {
  value: string;
  minRows: number;
  maxRows: number;
  inputRef: RefObject<HTMLTextAreaElement | null>;
} & Omit<TextareaHTMLAttributes<HTMLTextAreaElement>, "value" | "rows">) {
  useLayoutEffect(() => {
    const el = inputRef.current;
    if (!el) return;
    const fit = () => {
      const style = getComputedStyle(el);
      el.style.height = "auto";
      const { height, scrolls } = promptHeight({
        line: parseFloat(style.lineHeight) || 22,
        padding: parseFloat(style.paddingTop) + parseFloat(style.paddingBottom),
        content: el.scrollHeight,
        minRows,
        maxRows,
      });
      el.style.height = `${height}px`;
      el.style.overflowY = scrolls ? "auto" : "hidden";
    };
    fit();
    if (typeof ResizeObserver === "undefined") return;
    // A new width rewraps the text.
    let width = el.clientWidth;
    const observer = new ResizeObserver(() => {
      if (width === el.clientWidth) return;
      width = el.clientWidth;
      fit();
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, [value, minRows, maxRows]);
  return <textarea {...rest} ref={inputRef} rows={minRows} value={value} />;
}

/**
 * "Looking at: <ontology> / <load>" or "<dashboard>", or "Making a new
 * workflow" (for an ontology, a suggestion's), named from what the app has
 * read.
 */
function PageChip({
  context,
  onDrop,
}: {
  context: AgentPageContext;
  onDrop: () => void;
}): ReactNode {
  const { id: ws } = useWorkspace();
  if (context.newAutomation && !context.ontologyId)
    return (
      <span className="ag-chip">
        <Tag>Making a new workflow</Tag>
        <DropChip onDrop={onDrop} />
      </span>
    );
  return <NamedChip context={context} onDrop={onDrop} ws={ws} />;
}

/** The chip's way to leave the page out of the message. */
const DropChip = ({ onDrop }: { onDrop: () => void }) => (
  <button
    type="button"
    className="ag-chip-drop"
    aria-label="Don’t send this page"
    title="Don’t send this page"
    onClick={onDrop}
  >
    <X size={13} aria-hidden />
  </button>
);

/** An ontology's or a dashboard's chip, named from what the app has read. */
function NamedChip({
  context,
  onDrop,
  ws,
}: {
  context: AgentPageContext;
  onDrop: () => void;
  ws: string;
}): ReactNode {
  const ontologies = useQuery({
    ...ontologyQueries.list(ws),
    enabled: !!context.ontologyId,
  });
  const loads = useQuery({
    ...ontologyQueries.loads(ws, context.ontologyId || ""),
    enabled: !!context.loadId,
  });
  const dashboards = useQuery({
    ...dashboardQueries.list(ws),
    enabled: !!context.dashboardId,
  });
  const name = context.dashboardId
    ? dashboards.data?.dashboards?.find((d) => d.id === context.dashboardId)
        ?.name || "this dashboard"
    : ontologies.data?.ontologies?.find((o) => o.id === context.ontologyId)
        ?.name || "this ontology";
  const load = loads.data?.loads?.find((l) => l.id === context.loadId);
  return (
    <span className="ag-chip">
      <Tag>
        {context.newAutomation
          ? `Making a new workflow for ${name}`
          : `Looking at: ${name}${
              context.loadId
                ? ` / ${load ? `${loadVersion(load)} load` : "a load"}`
                : ""
            }`}
      </Tag>
      <DropChip onDrop={onDrop} />
    </span>
  );
}
