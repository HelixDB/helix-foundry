// /analyst/:threadId — one chat: the questions asked and their answers, with
// a composer for follow-ups pinned to the bottom. New chats start on Home.
import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Navigate, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowDown,
  ArrowUp,
  CircleAlert,
  Ellipsis,
  LoaderCircle,
  MessageSquareText,
  Square,
  SquarePen,
  Trash2,
} from "lucide-react";
import type { Resource } from "../../../../../packages/shared/src";
import { api } from "../../api";
import { useWorkspace } from "../../ui";
import {
  Button,
  ButtonLink,
  ConfirmDialog,
  DropdownMenu,
  EmptyState,
  MenuItem,
  Skeleton,
  SkeletonText,
  cx,
  formatRelative,
  plural,
  useNow,
  useTitle,
} from "../../kit";
import { paths } from "../../paths";
import {
  canControl,
  chatKey,
  deleteChat,
  fetchChat,
  isWorking,
  readDraft,
  resolveRun,
  writeDraft,
  type Chat,
} from "./chat-data";
import { chatListKey, chatListQuery } from "./launcher";
import { PromptInput } from "./prompt-input";
import { Avatar, Reply, Working, useRunControl } from "./reply";
import { VoiceOrb, useVoice, voiceHint } from "./voice";

const anyWorking = (runs?: Resource[]) =>
  !!runs?.some((r) => isWorking(r.data.status));

export function ChatPage({ chatId }: { chatId: string }) {
  const { id } = useWorkspace();
  const chat = useQuery({
    queryKey: chatKey(id, chatId),
    queryFn: () => fetchChat(id, chatId),
    // The live event stream refreshes a finished chat; polling covers the
    // progress in between.
    refetchInterval: (q) => (anyWorking(q.state.data?.runs) ? 2000 : false),
  });
  if (chat.isPending) return <ChatSkeleton />;
  // A failed refresh keeps showing the chat; only a failed first load says so.
  if (chat.data === undefined)
    return (
      <ChatMessage
        icon={CircleAlert}
        title="Couldn’t open this chat"
        text={chat.error.message}
        action={<Button onClick={() => chat.refetch()}>Try again</Button>}
      />
    );
  if (!chat.data) return <RunFallback chatId={chatId} />;
  return <Conversation chat={chat.data} />;
}

// Not a chat id: a follow-up's run id (an old /runs link) opens its chat.
function RunFallback({ chatId }: { chatId: string }) {
  const { id } = useWorkspace();
  const found = useQuery({
    queryKey: [...chatKey(id, chatId), "run"],
    queryFn: () => resolveRun(id, chatId),
    refetchInterval: (q) =>
      isWorking(q.state.data?.run.data.status) ? 2000 : false,
  });
  if (found.isPending) return <ChatSkeleton />;
  if (found.data === undefined)
    return (
      <ChatMessage
        icon={CircleAlert}
        title="Couldn’t open this chat"
        text={found.error.message}
        action={<Button onClick={() => found.refetch()}>Try again</Button>}
      />
    );
  if (!found.data)
    return (
      <ChatMessage
        icon={MessageSquareText}
        title="Chat not found"
        text="It may have been deleted. Ask a new question on Home."
        action={
          <ButtonLink to={paths.home()} variant="primary">
            Go to Home
          </ButtonLink>
        }
      />
    );
  if (found.data.chatId !== chatId)
    return <Navigate to={paths.thread(found.data.chatId)} replace />;
  // The first question of a chat the API could not list: show it alone.
  const run = found.data.run;
  return (
    <Conversation
      chat={{ id: chatId, title: run.data.goal || run.name, runs: [run] }}
    />
  );
}

function ChatMessage({
  icon,
  title,
  text,
  action,
}: {
  icon: typeof CircleAlert;
  title: string;
  text?: string;
  action?: ReactNode;
}) {
  useTitle(title);
  return (
    <div className="an-chat">
      <div className="an-chat-empty">
        <EmptyState icon={icon} title={title} text={text} action={action} />
      </div>
    </div>
  );
}

export function ChatSkeleton() {
  return (
    <div className="an-chat" aria-busy="true">
      <header className="an-chat-head">
        <div className="an-chat-head-inner">
          <div className="an-chat-title">
            <Skeleton width="48%" height={18} />
            <Skeleton width={150} height={10} />
          </div>
        </div>
      </header>
      <div className="an-scroll">
        <div className="an-log" role="status" aria-label="Loading chat">
          <div className="an-turn">
            <div className="an-question is-loading">
              <Skeleton width={220} height={12} />
            </div>
            <div className="an-reply">
              <Avatar />
              <div className="an-reply-body">
                <SkeletonText lines={3} />
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

function ChatHeader({ chat, turns }: { chat: Chat; turns: number }) {
  const { id, role, notify } = useWorkspace(),
    qc = useQueryClient(),
    navigate = useNavigate(),
    now = useNow();
  const [confirming, setConfirming] = useState(false),
    [deleting, setDeleting] = useState(false);
  const busy = anyWorking(chat.runs);
  const updated = chat.runs
    .map((r) => r.updatedAt)
    .sort()
    .at(-1);
  const remove = async () => {
    setDeleting(true);
    try {
      await deleteChat(id, chat.id);
    } catch (e) {
      notify((e as Error).message, true);
      setDeleting(false);
      return;
    }
    // Continue in the next most recent chat, or on Home when none is left.
    let next: string | undefined;
    try {
      const list = await qc.fetchQuery({ ...chatListQuery(id), staleTime: 0 });
      next = list.items.find((c) => c.id !== chat.id)?.id;
    } catch {
      void qc.invalidateQueries({ queryKey: chatListKey(id), exact: true });
    }
    navigate(next ? paths.thread(next) : paths.home(), { replace: true });
    // Once this page has gone, so the deleted chat is not fetched again.
    setTimeout(() => qc.removeQueries({ queryKey: chatKey(id, chat.id) }));
    notify("Chat deleted");
  };
  return (
    <header className="an-chat-head">
      <div className="an-chat-head-inner">
        <div className="an-chat-title">
          <h1 title={chat.title}>{chat.title}</h1>
          <p>
            {plural(turns, "question")}
            {updated && <> · updated {formatRelative(updated, now)}</>}
          </p>
        </div>
        <div className="an-chat-actions">
          <ButtonLink
            to={paths.home()}
            size="sm"
            variant="ghost"
            icon={SquarePen}
            className="an-new-chat"
            title="New chats start on Home"
          >
            <span className="an-new-chat-label">New chat</span>
          </ButtonLink>
          {role !== "viewer" && (
            <DropdownMenu
              label="Chat options"
              align="end"
              triggerClassName="hf-btn hf-btn--ghost hf-btn--md hf-btn--icon"
              trigger={<Ellipsis size={16} aria-hidden />}
            >
              <MenuItem
                icon={Trash2}
                danger
                disabled={busy}
                hint={busy ? "Still answering" : undefined}
                onSelect={() => setConfirming(true)}
              >
                Delete chat
              </MenuItem>
            </DropdownMenu>
          )}
        </div>
      </div>
      {confirming && (
        <ConfirmDialog
          title="Delete this chat?"
          description={`“${chat.title}” and its ${plural(chat.runs.length, "answer")} will be deleted. This can’t be undone.`}
          confirmLabel="Delete chat"
          tone="danger"
          pending={deleting}
          onConfirm={remove}
          onCancel={() => setConfirming(false)}
        />
      )}
    </header>
  );
}

// How close to the end counts as "at the latest message".
const NEAR = 72;

function Conversation({ chat }: { chat: Chat }) {
  const { id, role, user, notify } = useWorkspace(),
    qc = useQueryClient();
  useTitle(chat.title);
  const runs = chat.runs,
    latest = runs.at(-1);
  const latestWorking = isWorking(latest?.data.status);
  const control = useRunControl(latest);
  const draftKey = `foundry.chat.draft.v1:${id}:${chat.id}`;
  const [text, setText] = useState(() => readDraft(draftKey)),
    [pending, setPending] = useState<string | null>(null);
  const input = useRef<HTMLTextAreaElement>(null);
  // Hands-free voice in this chat: the user's words show in the composer
  // until GPT-Live hands them to the Analyst; typing pauses the microphone.
  const voice = useVoice(),
    voiceHere = voice.status !== "off" && voice.chatId === chat.id,
    showHeard = voiceHere && !text && !voice.paused && !!voice.heard,
    hint =
      voiceHere && text && voice.heard && !voice.paused
        ? // A typed draft is kept; what was heard shows here instead.
          `Heard: ${voice.heard}`
        : voiceHere || voice.error
          ? voiceHint(voice)
          : null;
  useEffect(() => {
    if (voiceHere && voice.paused && !text) voice.resumeListening();
  }, [voiceHere, voice.paused, text]);

  // Follows the newest message unless the reader has scrolled up.
  const scroller = useRef<HTMLDivElement>(null),
    stick = useRef(true),
    [atEnd, setAtEnd] = useState(true);
  const toEnd = (smooth = false) => {
    const el = scroller.current;
    if (el)
      el.scrollTo({
        top: el.scrollHeight,
        behavior: smooth ? "smooth" : "auto",
      });
  };
  const onScroll = () => {
    const el = scroller.current;
    if (!el) return;
    const near = el.scrollHeight - el.scrollTop - el.clientHeight < NEAR;
    stick.current = near;
    setAtEnd(near);
  };
  const signature = [
    runs.length,
    latest?.id,
    latest?.data.status,
    latest?.data.stage,
    pending ?? "",
  ].join(":");
  useLayoutEffect(() => {
    if (stick.current) toEnd();
  }, [signature]);
  // The composer growing (or the window resizing) keeps the end in view.
  useEffect(() => {
    const el = scroller.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(() => {
      if (stick.current) toEnd();
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  const send = async () => {
    const goal = text.trim();
    if (goal.length < 5 || pending || latestWorking || !latest) return;
    stick.current = true;
    setPending(goal);
    setText("");
    writeDraft(draftKey, "");
    try {
      const run = await api<Resource>(`/workspaces/${id}/assistant/runs`, {
        goal,
        intent: "auto",
        // Asked about the latest answer, so it joins this chat.
        context: { page: window.location.pathname, resourceId: latest.id },
      });
      qc.setQueryData<Chat | null>(chatKey(id, chat.id), (c) =>
        c ? { ...c, runs: [...c.runs.filter((r) => r.id !== run.id), run] } : c,
      );
      if (voiceHere) voice.noteTyped(goal);
      void qc.invalidateQueries({ queryKey: chatListKey(id) });
    } catch (e) {
      notify((e as Error).message, true);
      setText((t) => t || goal);
      writeDraft(draftKey, goal);
    } finally {
      setPending(null);
    }
  };
  const canStop = !!latest && canControl(latest, role, user.id);
  const ready = text.trim().length >= 5 && !pending;

  return (
    <div className="an-chat">
      <ChatHeader chat={chat} turns={runs.length + (pending ? 1 : 0)} />
      <div className="an-scroll" ref={scroller} onScroll={onScroll}>
        <div
          className="an-log"
          role="log"
          aria-live="polite"
          aria-label="Conversation"
        >
          {runs.map((r) => (
            <div key={r.id} className="an-turn">
              <div className="an-question">
                <span className="hf-sr-only">You asked: </span>
                <p>{r.data.goal}</p>
              </div>
              <Reply run={r} />
            </div>
          ))}
          {pending && (
            <div className="an-turn">
              <div className="an-question is-sending">
                <span className="hf-sr-only">You asked: </span>
                <p>{pending}</p>
              </div>
              <div className="an-reply">
                <Avatar />
                <div className="an-reply-body">
                  <Working text="Sending…" />
                </div>
              </div>
            </div>
          )}
        </div>
      </div>
      <div className="an-dock">
        <div className="an-dock-inner">
          {!atEnd && (
            <button
              type="button"
              className="an-jump"
              onClick={() => {
                stick.current = true;
                toEnd(true);
              }}
            >
              <ArrowDown size={14} aria-hidden />
              Jump to latest
            </button>
          )}
          {voice.available || voiceHere ? (
            <div className="an-voice">
              <VoiceOrb chatId={chat.id} placement="chat" />
            </div>
          ) : null}
          <form
            className="an-composer"
            onSubmit={(e) => {
              e.preventDefault();
              void send();
            }}
            onClick={(e) => {
              if (e.target === e.currentTarget) input.current?.focus();
            }}
          >
            <PromptInput
              inputRef={input}
              className={cx("an-input", showHeard && "is-voice")}
              aria-label="Ask a follow-up"
              aria-describedby="an-composer-hint"
              placeholder={
                voiceHere && !voice.paused ? "Listening…" : "Ask a follow-up…"
              }
              maxLength={4000}
              value={showHeard ? voice.heard : text}
              onValueChange={(v) => {
                // Typing (or correcting what was heard) takes the composer
                // back from voice until it is sent or cleared.
                if (voiceHere && v) voice.pauseForTyping();
                setText(v);
                writeDraft(draftKey, v);
              }}
              onSubmit={() => void send()}
            />
            {latestWorking ? (
              <button
                type="button"
                className="an-send is-stop"
                aria-label="Stop answering"
                title="Stop"
                disabled={!canStop || control.pending}
                onClick={control.stop}
              >
                <Square size={13} fill="currentColor" aria-hidden />
              </button>
            ) : (
              <button
                type="submit"
                className="an-send"
                aria-label="Send follow-up"
                title="Send"
                disabled={!ready}
              >
                {pending ? (
                  <LoaderCircle size={17} className="hf-spin" aria-hidden />
                ) : (
                  <ArrowUp size={17} aria-hidden />
                )}
              </button>
            )}
          </form>
          <p
            id="an-composer-hint"
            className={cx(
              "an-hint",
              hint && !voice.error && "is-voice",
              voice.error && "is-error",
            )}
            role={voice.error ? "alert" : undefined}
          >
            {hint ||
              (latestWorking
                ? "You can send a follow-up once this answer is ready."
                : "Enter to send · Shift+Enter for a new line")}
          </p>
        </div>
      </div>
    </div>
  );
}
