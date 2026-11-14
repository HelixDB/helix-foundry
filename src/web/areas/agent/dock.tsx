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
  CircleAlert,
  ExternalLink,
  LoaderCircle,
  MessageSquarePlus,
  Sparkles,
  Square,
  Trash2,
  Wrench,
  X,
} from "lucide-react";
import type {
  AgentPageContext,
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
import { ontologyQueries } from "../ontology/queries";
import { AiRefusal, useAiConfigured } from "../workspace/ai-gate";
import { aiQuery } from "../workspace/ai-api";
import { paths } from "../../paths";
import { ActionCard } from "./cards";
import {
  EMPTY_STREAM,
  MAX_MESSAGE,
  STOP_TEXT,
  TOOL_LABELS,
  agentRefusal,
  analystLink,
  isLiveTurn,
  onClaude,
  pageContext,
  pretty,
  textParts,
  threadKey,
  type AgentBlock,
  type TurnStream,
} from "./model";
import {
  EVENTS_POLL_MS,
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
  const { id: ws } = useWorkspace(),
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
              recent={all.length - i <= RECENT_TURNS}
              scroller={log}
              actions={actions}
              busy={busy}
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
  recent,
  scroller,
  actions,
  busy,
  onNavigate,
}: {
  turn: FoundryAgentTurn;
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
      {turn.kind !== "action_result" && (
        <div className="ag-msg ag-msg--person">
          <span className="ag-who">You</span>
          <p className="ag-text">{turn.text}</p>
        </div>
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
        {stream.blocks.map((b, i) => (
          <Block
            key={i}
            block={b}
            actions={actions}
            busy={busy}
            onNavigate={onNavigate}
          />
        ))}
        {live && !read.failed && (
          <p className="ag-working">
            <LoaderCircle size={14} className="hf-spin" aria-hidden />
            {turn.status === "queued" && (turn.attempts || 0) > 0
              ? `Retrying${turn.error ? `: ${turn.error}` : ""}…`
              : turn.status === "queued"
                ? "Waiting to start…"
                : "Working…"}
          </p>
        )}
        {turn.status === "cancelled" && <p className="ag-note">Stopped.</p>}
        {failed && (
          <p className="ag-card-error">
            <CircleAlert size={14} aria-hidden /> {sentence(failed)}
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
        analyst = analystLink(s);
      return (
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
          </summary>
          <div className="ag-step-body">
            <span className="ag-label">Input</span>
            <pre className="ag-code">{pretty(s.input ?? {})}</pre>
            {s.result?.kind === "failed" && (
              <p className="ag-card-error">{sentence(s.result.error)}</p>
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
      );
    }
    case "card": {
      const action = actions.get(block.actionId);
      return action ? (
        <ActionCard action={action} busy={busy} />
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
            ? "started"
            : block.status === "failed"
              ? `failed${block.error ? ` (${block.error})` : ""}`
              : block.status === "unknown"
                ? "may or may not have run"
                : block.status}
          .
        </p>
      );
    case "retry":
      return <p className="ag-note">Trying again (attempt {block.attempt}).</p>;
  }
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
 * it runs; Stop ends the live turn.
 */
function Composer({
  threadId,
  thread,
  onSent,
}: {
  // The open thread; empty for a new one. `thread` once it is read.
  threadId: string;
  thread?: AgentThreadResponse;
  onSent: (threadId: string) => void;
}) {
  const { id: ws, role } = useWorkspace(),
    location = useLocation(),
    qc = useQueryClient();
  const page = pageContext(location.pathname, location.search),
    pageKey = page ? `${page.ontologyId}/${page.loadId || ""}` : "";
  const [dropped, setDropped] = useState("");
  const context = page && dropped !== pageKey ? page : null;
  const [text, setText] = useState(""),
    [token, setToken] = useState(() => crypto.randomUUID()),
    [pending, setPending] = useState(false),
    [error, setError] = useState<unknown>(null);
  const aiReady = useAiConfigured(),
    ai = useQuery(aiQuery(ws)),
    claude = ai.data ? onClaude(ai.data) : undefined;
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
    aiReady === false ||
    claude === false;
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
    } finally {
      sending.current = false;
      setPending(false);
    }
  };
  const caption = tooLong
    ? `${length - MAX_MESSAGE} characters too long.`
    : busy
      ? "You can send once the Assistant has finished."
      : "Enter to send, Shift+Enter for a new line. Nothing runs until you click a card’s button.";
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
          placeholder="Ask about your ontologies, or ask it to load one…"
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
      {claude === false ? (
        <p id={hint} className="ag-card-error" role="note">
          <CircleAlert size={14} aria-hidden /> The Assistant runs on Claude for
          now.{" "}
          {role === "owner" ? (
            <>
              Choose Claude in{" "}
              <Link to={paths.settings("ai")}>Settings › AI models</Link>.
            </>
          ) : (
            "An owner can choose Claude in Settings › AI models."
          )}
        </p>
      ) : (
        <p id={hint} className={cx("ag-hint", tooLong && "is-error")}>
          {caption}
        </p>
      )}
      <AiRefusal action="The Assistant" error={error} ready={aiReady}>
        {!!error && (
          <p className="ag-card-error" role="alert">
            {sentence((error as Error).message)}
          </p>
        )}
      </AiRefusal>
    </form>
  );
}

/** "Looking at: <ontology> / <load>", named from what the app has read. */
function PageChip({
  context,
  onDrop,
}: {
  context: AgentPageContext;
  onDrop: () => void;
}): ReactNode {
  const { id: ws } = useWorkspace();
  const ontologies = useQuery(ontologyQueries.list(ws));
  const loads = useQuery({
    ...ontologyQueries.loads(ws, context.ontologyId || ""),
    enabled: !!context.loadId,
  });
  const name =
    ontologies.data?.ontologies?.find((o) => o.id === context.ontologyId)
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
