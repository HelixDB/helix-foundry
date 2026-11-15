import "./agent.css";
import {
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
  type RefObject,
} from "react";
import { Link, useLocation } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowUp,
  ChevronDown,
  CircleHelp,
  CircleAlert,
  TriangleAlert,
  ExternalLink,
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
import { activeSection } from "../../shell/nav";
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
  EMPTY_STREAM,
  HELPER_COMMANDS,
  RUNTIMES,
  MAX_MESSAGE,
  NOT_RUN_TEXT,
  STARTS_WORK,
  STOP_TEXT,
  TOOL_LABELS,
  agentRefusal,
  analystLink,
  connectLink,
  isLiveTurn,
  pageContext,
  pretty,
  textParts,
  threadKey,
  threadRuntime,
  type RuntimeState,
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

/**
 * The Assistant's dock: the person's threads with it, the open thread's
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
}: {
  onClose: () => void;
  onNavigate?: () => void;
}) {
  const { id: ws, role } = useWorkspace(),
    qc = useQueryClient();
  const [threadId, setThreadId] = useState(() => readStorage(threadKey(ws)));
  useEffect(() => setThreadId(readStorage(threadKey(ws))), [ws]);
  const open = (id: string) => {
    writeStorage(threadKey(ws), id || null);
    setThreadId(id);
  };
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
  // Where the open thread runs (a new one: the workspace's setting, which
  // only a local install's owner reads), read often while its helper isn't
  // running. Unknown until the open thread is read.
  const runtimeNow: AgentRuntime | undefined = threadId
    ? thread.data?.thread.runtime
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
  const where =
    !threadId || thread.data
      ? threadRuntime(runtimeNow, setting.data)
      : undefined;
  return (
    <div className="ag-dock" data-replay-block>
      <header className="ag-head">
        <Sparkles size={16} aria-hidden className="ag-head-icon" />
        <h2 className="ag-head-title">Assistant</h2>
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
          aria-label="Close the Assistant"
          title="Close the Assistant"
          onClick={onClose}
        >
          <X size={16} aria-hidden />
        </button>
      </header>
      {disabled ? (
        <EmptyState
          size="sm"
          icon={Sparkles}
          title="The Assistant isn’t available here"
          text="It is off for this Foundry."
        />
      ) : (
        <>
          {where && where.runtime !== "api" && (
            <RuntimeLine
              runtime={where.runtime}
              state={where.state}
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
          <Composer
            threadId={threadId}
            thread={threadId ? thread.data : undefined}
            runtime={where?.runtime ?? "api"}
            hostState={where?.state ?? "ready"}
            onNavigate={onNavigate}
            onSent={(id) => {
              if (id !== threadId) open(id);
              void qc.invalidateQueries({ queryKey: [ws, "agent", "threads"] });
              void qc.invalidateQueries({
                queryKey: [ws, "agent", "thread", id],
              });
            }}
          />
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
 * Where the open thread (or a new one) runs, when it is a helper's: on the
 * person's subscription; while none runs it, how to start the helper paired
 * for it, or, with none paired, to pair one in Settings (the WFE refuses to
 * send until one runs).
 */
function RuntimeLine({
  runtime,
  state,
  thread,
  onNavigate,
}: {
  runtime: Exclude<AgentRuntime, "api">;
  state: RuntimeState;
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
          ) : (
            <SettingsLinked
              text={`No helper running ${r.cli} is paired, so ${which} can’t be sent. Pair one in Settings › AI models, then start it.`}
              onNavigate={onNavigate}
            />
          )}
        </span>
      </p>
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

/** The open thread's turns, following its end until the reader scrolls up. */
function Transcript({
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
  const log = useRef<HTMLDivElement>(null),
    body = useRef<HTMLDivElement>(null),
    stick = useRef(true);
  useLayoutEffect(() => {
    const el = log.current,
      inner = body.current;
    if (!el || !inner || typeof ResizeObserver === "undefined") return;
    const follow = () => {
      if (stick.current) el.scrollTop = el.scrollHeight;
    };
    follow();
    const observer = new ResizeObserver(follow);
    observer.observe(inner);
    return () => observer.disconnect();
  }, [data?.thread.id]);
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
        stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48;
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
            title="Ask the Assistant"
            text="It reads your workspace with Foundry’s tools. What would change anything it prepares as a card, which runs only when you click."
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
        <span className="ag-who">Assistant</span>
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
  actions,
  busy,
  onNavigate,
}: {
  block: AgentBlock;
  actions: ReadonlyMap<string, FoundryAgentAction>;
  busy: boolean;
  onNavigate?: () => void;
}) {
  switch (block.kind) {
    case "text":
      return <PlainText text={block.text} onNavigate={onNavigate} />;
    case "step": {
      const s = block.step,
        analyst = analystLink(s),
        connect = connectLink(s);
      // Asking shows as its questions; only a refusal shows as a step.
      if (s.name === ASK_PERSON && s.result?.kind !== "failed") return null;
      return (
        <>
          <details className="ag-step">
            <summary>
              <Wrench size={13} aria-hidden />
              <span>{TOOL_LABELS[s.name] || s.name}</span>
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
              {analyst && (
                <Link className="ag-link" to={analyst} onClick={onNavigate}>
                  Open in Analyst <ExternalLink size={12} aria-hidden />
                </Link>
              )}
            </div>
          </details>
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
        <ActionCard action={action} busy={busy} onNavigate={onNavigate} />
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
          Reported to its automation: {block.summary}
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
        The Assistant asked questions this page can’t show. Reload to update
        Foundry.
      </p>
    );
  if (state !== "open" || moved)
    return (
      <p className="ag-ran">
        <CircleHelp size={13} aria-hidden /> Asked you {what}
        {state === "answered"
          ? "."
          : moved
            ? ": the draft no longer waits on them here (answered on its page, or it moved on). "
            : ", which went unanswered."}
        {moved && draftLink}
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
        agent={relayed ? "the agent" : "the Assistant"}
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
 * asked it; a question left out was left to the Assistant.
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
            : `Left to ${asking?.asked?.generation ? "the agent" : "the Assistant"}`,
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
 * turn.
 */
function Composer({
  threadId,
  thread,
  runtime,
  hostState,
  onSent,
  onNavigate,
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
}) {
  const { id: ws, role } = useWorkspace(),
    location = useLocation(),
    qc = useQueryClient();
  const page = pageContext(location.pathname, location.search),
    pageKey = page
      ? `${page.ontologyId || ""}/${page.loadId || ""}/${page.dashboardId || ""}`
      : "";
  const [dropped, setDropped] = useState("");
  const context = page && dropped !== pageKey ? page : null;
  const [text, setText] = useState(""),
    [token, setToken] = useState(() => crypto.randomUUID()),
    [pending, setPending] = useState(false),
    [error, setError] = useState<unknown>(null);
  const onModel = runtime === "api",
    aiReady = useAiConfigured(),
    modelReady = onModel ? aiReady : true;
  const setting = useQuery(aiQuery(ws)).data,
    now = setting && inForce(setting),
    checkFailed =
      onModel && now?.source === "workspace" && assistantCheckText(now.config);
  const hint = useId(),
    input = useRef<HTMLTextAreaElement>(null),
    sending = useRef(false);
  // The dock opens ready to write.
  useEffect(() => input.current?.focus(), []);
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
            : "The Assistant can’t run on this workspace’s AI model. An owner can fix this in Settings › AI models."
        }
        onNavigate={onNavigate}
      />
    </span>
  ) : tooLong ? (
    `${length - MAX_MESSAGE} characters too long.`
  ) : busy ? (
    "You can send once the Assistant has finished."
  ) : (
    "Enter to send, Shift+Enter for a new line. Nothing runs until you click a card’s button."
  );
  return (
    <form
      className="ag-composer"
      aria-label="Message the Assistant"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      {context && (
        <PageChip context={context} onDrop={() => setDropped(pageKey)} />
      )}
      <div className="ag-input-row">
        <textarea
          ref={input}
          className="ag-input"
          aria-label="Message"
          aria-describedby={hint}
          rows={2}
          value={text}
          placeholder="Ask about your workspace, or ask it to do something…"
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
          <Button
            size="sm"
            icon={Square}
            aria-label="Stop"
            title="Stop"
            onClick={async () => {
              await cancelAgentTurn(ws, live.id).catch(() => undefined);
              await qc.invalidateQueries({
                queryKey: [ws, "agent", "thread", threadId],
              });
            }}
          />
        ) : (
          <Button
            type="submit"
            variant="primary"
            size="sm"
            icon={ArrowUp}
            aria-label="Send"
            title="Send"
            pending={pending}
            disabled={blocked}
          />
        )}
      </div>
      <p
        id={hint}
        className={cx("ag-hint", (tooLong || !!checkFailed) && "is-error")}
      >
        {caption}
      </p>
      <AiRefusal action="The Assistant" error={error} ready={modelReady}>
        {!!error && (
          <p className="ag-card-error" role="alert">
            {agentRefusal(error) !== "FOUNDRY_AGENT_HOST_OFFLINE" ? (
              sentence((error as Error).message)
            ) : hostState === "unpaired" ? (
              <SettingsLinked
                text="No helper is paired to run it: pair one in Settings › AI models, start it, then send again."
                onNavigate={onNavigate}
              />
            ) : (
              `Its helper isn’t running on your computer: start it with ${HELPER_COMMANDS.run}, then send again.`
            )}
          </p>
        )}
      </AiRefusal>
    </form>
  );
}

/** "Looking at: <ontology> / <load>" or "<dashboard>", named from what the app has read. */
function PageChip({
  context,
  onDrop,
}: {
  context: AgentPageContext;
  onDrop: () => void;
}): ReactNode {
  const { id: ws } = useWorkspace();
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
        Looking at: {name}
        {context.loadId
          ? ` / ${load ? `${loadVersion(load)} load` : "a load"}`
          : ""}
      </Tag>
      <button
        type="button"
        className="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon"
        aria-label="Don’t send this page"
        title="Don’t send this page"
        onClick={onDrop}
      >
        <X size={13} aria-hidden />
      </button>
    </span>
  );
}
