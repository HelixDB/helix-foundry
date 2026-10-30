import "./ask.css";
import {
  Fragment,
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
import { Link, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowDown,
  ArrowUp,
  Ban,
  Check,
  ChevronLeft,
  ChevronRight,
  Circle,
  CircleAlert,
  FileQuestion,
  Gauge,
  LoaderCircle,
  SearchX,
  Sparkles,
  CircleStop,
  X,
} from "lucide-react";
import type { EntityRef } from "../../../shared/foundry-read";
import type {
  AskUsage,
  FoundryQuestion,
  QuestionBudget,
} from "../../../shared/wfe";
import {
  Button,
  ButtonLink,
  Dialog,
  EmptyState,
  Skeleton,
  SkeletonText,
  Tag,
  cx,
  formatRelative,
  plural,
  useMedia,
  useTitle,
} from "../../kit";
import { sentence } from "../../api";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { useDocumentNames } from "../documents/documents-api";
import { formatUsd } from "../documents/model";
import { parseEntityId } from "../explore/model";
import { readQueries } from "../explore/reads";
import { useTenantReads } from "../explore/tenant-reads";
import { useNaming } from "../explore/use-naming";
import {
  AiRefusal,
  SettingsLinked,
  useAiConfigured,
} from "../workspace/ai-gate";
import {
  EMPTY_STREAM,
  MAX_QUESTION,
  answerView,
  askSuggestions,
  askView,
  citationHref,
  citationText,
  citedEntities,
  draftGoal,
  graphQuery,
  isEnding,
  isLiveQuestion,
  knownOf,
  queryText,
  questionFocus,
  questionView,
  reduceEvents,
  refusalText,
  resultLabel,
  resultText,
  stepHeading,
  stepInput,
  threadsOf,
  usageText,
  type AnswerPart,
  type AskStep,
  type AskStream,
  type CitationContext,
  type QuestionView,
} from "./ask-model";
import {
  LoadNotice,
  LoadPicker,
  NothingLoaded,
  useLoadSelection,
} from "./load-selection";
import { loadVersion, withLoad } from "./model";
import { Callout } from "./parts";
import {
  POLL_MS,
  askQuestion,
  cancelQuestion,
  ontologyQueries,
  questionEvents,
  questionQueries,
} from "./queries";
import { AskThreads } from "./ask-threads";
import { AskVisual } from "./ask-visual";
import { drawable, placeVisuals } from "./ask-visuals";
import { OntologyPicker, ontologyName, useOntology } from "./scope";
import { useSources } from "./sources";

// /ask/:ontologyId: questions about a load, answered by the
// ontology agent's read-only query agent, laid out as a
// chat app is and as helix-foundry's Analyst chat: the threads in the
// shell's sidebar under its sections (ask-threads.tsx); here, a slim bar with
// the thread's title and the Ontology and Load pickers, then a thread as a conversation
// in one centred column: each question as a bubble, then the reply, its
// numbered steps as they stream, each a tool call: the tool's name, its
// input in words (a query as a readable path) and what it returned, which
// opens on its Input (the query, a graph query's code, or its arguments)
// and Output (a query's result as a table or chart); once it ends, the query as the agent ran it, entity ids resolved; a
// refused answer says why, for a person) and its answer: the model's
// blocks as plain text with numbered citation chips that open the entity,
// fact, derived fact, table row, document span or rule break they cite, or
// light the step whose result they cite. The conversation follows the newest answer unless scrolled
// up, and the follow-up composer stays pinned under it. A new thread asks
// what the user wants to know, its composer centred, with the draft's
// competency questions as suggestions, which a click asks, when nothing
// has been asked yet or its version is already read.
// An answer's visuals (contract 3.11.0) follow the blocks the model put
// them after: query results drawn from the output the engine recorded
// (ask-visual.tsx), and each query step has its Result the same way (the
// read of what the load holds and a graph query have only their summary:
// their events carry no result). Lists leave outputs out, so an answered
// question near the view, or a step's Result opened, reads its question
// once with them.
// Nothing the model writes renders as a link, an image or HTML. The entity
// page's "Ask about this" opens the same flow in a drawer (AskDrawer).

const askable = (status?: string) => status === "ready" || status === "partial";
// How a shortcut is written: "⌘" on Apple devices, "Ctrl+" elsewhere.
const MOD =
  typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform)
    ? "⌘"
    : "Ctrl+";
// How close to the end of the conversation counts as at its newest answer.
const NEAR = 72;

/** The `?focus=` entity id as the ref a question carries. */
function focusOf(param: string | null): EntityRef | undefined {
  const e = param ? parseEntityId(param) : undefined;
  return e && { type: e.type, id: "key" in e ? e.key : e.name };
}

export function AskPage() {
  const { id: ws } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const [params, setParams] = useSearchParams();
  const focus = focusOf(params.get("focus"));
  const selection = useLoadSelection();
  const list = useQuery(questionQueries.list(ws, o));
  const threads = threadsOf(list.data?.questions || []);
  const load =
    selection.choice.kind === "load" ? selection.choice.load : undefined;
  // A narrow screen has no panel beside the page: it shows the threads
  // until one (or a new one) is chosen.
  const view = askView(
    params,
    useMedia("(max-width: 800px)"),
    list.data ? threads.length : undefined,
  );
  // The version the load holds names its suggestions. A version can be
  // large: it is read for them only while nobody has asked anything yet,
  // else only if a page already read it.
  const version = useQuery({
    ...ontologyQueries.version(ws, o, load?.versionId || ""),
    enabled: !!load?.versionId && list.data !== undefined && !threads.length,
  }).data?.version;
  const suggestions = useMemo(() => askSuggestions(version), [version]);
  useTitle(view.kind === "thread" ? undefined : "Analyst");
  const setThread = (id: string) =>
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        next.delete("focus");
        next.delete("new");
        if (id) next.set("thread", id);
        else next.delete("thread");
        return next;
      },
      { replace: false },
    );
  const picker = (
    <>
      <OntologyPicker ontologyId={o} to={paths.ask} />
      <LoadPicker selection={selection} />
    </>
  );
  if (!ontology.tenantId)
    return (
      <div className="ak-chat">
        <ChatBar title="Analyst">{picker}</ChatBar>
        <div className="ak-scroll ak-chat--empty">
          <EmptyState
            icon={Ban}
            title="You can’t read this ontology’s database"
            text="Questions read the database a load wrote; ask someone who manages its project for access."
          />
        </div>
      </div>
    );
  if (view.kind === "threads")
    return (
      <div className="ak-chat">
        <ChatBar title="Analyst">{picker}</ChatBar>
        <div className="ak-scroll">
          <AskThreads />
        </div>
      </div>
    );
  if (view.kind === "thread")
    return (
      <ThreadView
        key={view.id}
        thread={view.id}
        title={threads.find((t) => t.id === view.id)?.questions[0]?.question}
        load={load}
        budget={list.data?.budget}
        picker={picker}
        notice={<LoadNotice selection={selection} />}
        onBack={() => setThread("")}
      />
    );
  if (selection.choice.kind === "none" && !selection.pending)
    return (
      <div className="ak-chat">
        <ChatBar
          title="Analyst"
          onBack={threads.length > 0 ? () => setThread("") : undefined}
        >
          {picker}
        </ChatBar>
        <div className="ak-scroll ak-chat--empty">
          <NothingLoaded purpose="ask about its entities and facts" />
        </div>
      </div>
    );
  return (
    <div className="ak-chat">
      <ChatBar onBack={threads.length > 0 ? () => setThread("") : undefined}>
        {picker}
      </ChatBar>
      <div className="ak-scroll">
        <div className="ak-start">
          <LoadNotice selection={selection} />
          <h1 className="ak-hello">
            What do you want to know about {ontologyName(ontology)}?
          </h1>
          <Composer
            load={load}
            focus={focus}
            suggestions={suggestions}
            onAsked={(q) => setThread(q.threadId || q.id)}
          />
        </div>
      </div>
    </div>
  );
}

/**
 * The conversation's slim bar: on a narrow screen, where the threads are a
 * page of their own, the way back; the thread's title; and the Ontology and
 * Load pickers.
 */
function ChatBar({
  title,
  onBack,
  children,
}: {
  title?: string;
  onBack?: () => void;
  children: ReactNode;
}) {
  return (
    <header className="ak-bar">
      {onBack && (
        <Button
          size="sm"
          variant="ghost"
          icon={ChevronLeft}
          className="ak-back"
          onClick={onBack}
        >
          Threads
        </Button>
      )}
      {title ? (
        <h1 className="ak-bar-title" title={title}>
          {title}
        </h1>
      ) : (
        <span className="ak-bar-title" />
      )}
      {children}
    </header>
  );
}

type Load =
  | { id: string; status?: string; version?: string; current?: boolean }
  | undefined;

/**
 * A thread's questions, oldest first, following the newest answer unless
 * scrolled up, and the follow-up composer pinned under them.
 */
function ThreadView({
  thread,
  title,
  load,
  budget,
  picker,
  notice,
  onBack,
}: {
  thread: string;
  // Its title as the page's list has it, while the thread is read.
  title?: string;
  load: Load;
  budget?: QuestionBudget | null;
  picker: ReactNode;
  notice: ReactNode;
  onBack: () => void;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const list = useQuery(questionQueries.list(ws, o, thread));
  const questions = threadsOf(list.data?.questions || [])[0]?.questions || [];
  const first = questions[0],
    last = questions[questions.length - 1];
  useTitle(first?.question);
  // The conversation follows its end until the reader scrolls up, and
  // again once they are back at it: content growing under a scroll the
  // page made is not the reader leaving.
  const scroller = useRef<HTMLDivElement>(null),
    log = useRef<HTMLDivElement>(null),
    stick = useRef(true),
    top = useRef(0),
    [away, setAway] = useState(false);
  const toEnd = (smooth = false) =>
    scroller.current?.scrollTo({
      top: scroller.current.scrollHeight,
      behavior: smooth ? "smooth" : "auto",
    });
  const fromEnd = (el: HTMLElement) =>
    el.scrollHeight - el.scrollTop - el.clientHeight;
  // Steps stream in and answers arrive: the end stays in view while the
  // reader is there, as does a window or composer resized.
  const shown = questions.length > 0;
  useLayoutEffect(() => {
    if (!shown) return;
    toEnd();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(() => {
      if (stick.current) toEnd();
      else setAway(fromEnd(scroller.current!) >= NEAR);
    });
    observer.observe(log.current!);
    observer.observe(scroller.current!);
    return () => observer.disconnect();
  }, [shown]);
  if (!first || !last)
    return (
      <div className="ak-chat" aria-busy={list.isPending || undefined}>
        <ChatBar title={list.isPending ? title : undefined} onBack={onBack}>
          {picker}
        </ChatBar>
        {list.isPending ? (
          <div className="ak-scroll">
            <div className="ak-log">
              <SkeletonText lines={6} />
            </div>
          </div>
        ) : (
          <div className="ak-scroll ak-chat--empty">
            <EmptyState
              icon={SearchX}
              title={
                list.isError ? "Couldn’t read this thread" : "No such thread"
              }
              text={list.error?.message}
              action={<Button onClick={onBack}>New thread</Button>}
            />
          </div>
        )}
      </div>
    );
  return (
    <div className="ak-chat">
      <ChatBar title={first.question || "Thread"} onBack={onBack}>
        {picker}
      </ChatBar>
      <div
        className="ak-scroll"
        ref={scroller}
        onScroll={(e) => {
          const el = e.currentTarget,
            near = fromEnd(el) < NEAR;
          if (near) stick.current = true;
          else if (el.scrollTop < top.current) stick.current = false;
          top.current = el.scrollTop;
          setAway(!stick.current);
        }}
      >
        <div
          className="ak-log"
          ref={log}
          onClick={(e) => {
            // A disclosure or step the reader opens or closes stays where
            // it is.
            if ((e.target as Element).closest("summary, [aria-expanded]"))
              stick.current = false;
          }}
        >
          {notice}
          {questions.map((q) => (
            <QuestionCard key={q.id} question={q} budget={budget} />
          ))}
        </div>
      </div>
      <div className="ak-dock">
        {away && (
          <button
            type="button"
            className="ak-jump"
            onClick={() => {
              stick.current = true;
              toEnd(true);
            }}
          >
            <ArrowDown size={14} aria-hidden />
            Jump to latest
          </button>
        )}
        <Composer
          docked
          load={load}
          thread={thread}
          focus={questionFocus(last)}
          busy={isLiveQuestion(last)}
          onAsked={() => {
            stick.current = true;
          }}
        />
      </div>
    </div>
  );
}

/**
 * A question's events, polled after the last seq until the stream is
 * complete: the WFE says the question ended and its events reach its last
 * seq.
 */
function useStream(q: FoundryQuestion): AskStream {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology(),
    qc = useQueryClient();
  const key = [ws, "ontology", o, "questions", "events", q.id];
  const stream = useQuery({
    queryKey: key,
    queryFn: async () => {
      const prev = qc.getQueryData<AskStream>(key) ?? EMPTY_STREAM;
      return reduceEvents(
        prev,
        await questionEvents(ws, o, q.id, prev.lastSeq),
      );
    },
    refetchInterval: (s) => (s.state.data?.complete ? false : POLL_MS),
    staleTime: (s) => (s.state.data?.complete ? Infinity : 0),
  });
  return stream.data ?? EMPTY_STREAM;
}

/**
 * What a page needs to name and open a question's citations, and `named`:
 * the names the pages of `cited` give them, by refKey, read in the
 * background with one lookup in the question's load. Entity names and
 * result labels come from its steps and `named` (knownOf: `name`,
 * `result`), which the caller adds.
 */
function useCitations(
  q: FoundryQuestion,
  cited: readonly EntityRef[],
): Omit<CitationContext, "name" | "result"> & {
  named: ReadonlyMap<string, string>;
} {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology(),
    { readLater } = useTenantReads();
  const loads = useQuery(ontologyQueries.loads(ws, o));
  const loadId = q.loadId || "";
  const versionId =
    q.versionId ||
    loads.data?.loads?.find((l) => l.id === loadId)?.versionId ||
    "";
  const { naming } = useNaming(versionId);
  const names = useQuery(
    readQueries.names(ws, o, readLater, loadId, cited, naming),
  );
  // A citation names any document of the workspace, a connector's too.
  const docName = useDocumentNames(ws, "a document");
  const sources = useSources();
  return {
    named: names.data ?? new Map(),
    ontologyId: o,
    loadId,
    keyed: (type) => {
      const t = naming.types.get(type);
      return t ? t.key.length > 0 : undefined;
    },
    docName,
    table: (source) => {
      const t = sources.byAgentId(source);
      return t && { to: t.to, table: t.table, connector: t.connector };
    },
  };
}

/**
 * How a question's steps are headed: the phase (the dot's colour), what it
 * is doing or how it ended, and a note under the steps for a question that
 * waits.
 */
function traceOf(
  view: QuestionView,
  steps: number,
  budget?: QuestionBudget | null,
): { phase: string; headline: string; detail?: string; note?: string } {
  switch (view.kind) {
    case "waiting":
      return {
        phase: "waiting",
        headline: "Waiting for this month’s questions budget",
        note: `${formatUsd(budget?.spentUsdMicros || 0)} of ${formatUsd(budget?.monthlyUsdMicros || 0)} is spent. It runs once the month can hold what a question may spend, or cancel it.`,
      };
    case "queued":
      return {
        phase: "working",
        headline: "Queued",
        detail: "It starts in a moment.",
      };
    case "retrying":
      return {
        phase: "waiting",
        headline: "An attempt failed. Helix will try again",
        note: view.error,
      };
    case "running":
      return {
        phase: "working",
        headline: "Answering",
        detail: `${plural(steps, "step")} so far`,
      };
    case "cancelled":
      return { phase: "cancelled", headline: "Cancelled" };
    case "failed":
      return { phase: "failed", headline: "Stopped with an error" };
    case "budget_exhausted":
      return { phase: "failed", headline: "Stopped at its limit" };
    case "cannot_answer":
      return { phase: "unanswered", headline: "Couldn’t answer" };
    case "answered":
      return { phase: "answered", headline: "Answered" };
  }
}

/** The reply's mark: helix-foundry's Analyst avatar. */
function Avatar() {
  return (
    <span className="ak-avatar" aria-hidden>
      <Sparkles size={14} />
    </span>
  );
}

/** A step's mark: done, running, failed, or not reached. */
function StepMark({ step, live }: { step: AskStep; live: boolean }) {
  const Icon = step.result
      ? step.result.kind === "failed"
        ? CircleAlert
        : Check
      : live
        ? LoaderCircle
        : Circle,
    running = !step.result && live;
  return (
    <Icon
      size={13}
      className={cx("ak-step-mark", running && "hf-spin")}
      aria-hidden
    />
  );
}

/**
 * Whether an element has come within a screen of the view, once it has:
 * the conversation's scroller's view (`.ak-scroll`), else the window's.
 */
function useSeen(ref: RefObject<Element | null>) {
  const [seen, setSeen] = useState(
    () => typeof IntersectionObserver === "undefined",
  );
  useEffect(() => {
    const el = ref.current;
    if (seen || !el) return;
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) setSeen(true);
      },
      { root: el.closest(".ak-scroll"), rootMargin: "100% 0px" },
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, [seen]);
  return seen;
}

/**
 * One question as a turn of the conversation: what was asked, then the
 * reply: its steps, then how it ended. An ended question from a list has
 * no outputs: once it is near the view and its answer has visuals, or a
 * step's Result is opened, it is read once with them (questionQueries.answer).
 */
export function QuestionCard({
  question: listed,
  budget,
}: {
  question: FoundryQuestion;
  budget?: QuestionBudget | null;
}) {
  const { id: ws, notify } = useWorkspace(),
    { id: o } = useOntology(),
    qc = useQueryClient();
  const card = useRef<HTMLElement>(null);
  const seen = useSeen(card);
  const [opened, setOpened] = useState(false);
  // Parsed once per question as listed (a list keeps an unchanged
  // question's object), and once more as read with its outputs.
  const first = useMemo(() => questionView(listed, budget), [listed, budget]);
  const settled = "result" in first && first.result ? first.result : undefined;
  // Outputs to read: none came with the question, and a visual or an
  // opened Result wants them.
  const wanted =
    !!settled &&
    settled.queries.length > 0 &&
    !settled.queries.some((r) => r.output) &&
    (opened ||
      (seen &&
        settled.status === "answered" &&
        (settled.answer.visuals?.length ?? 0) > 0));
  const full = useQuery({
    ...questionQueries.answer(ws, o, listed.id),
    enabled: wanted,
  });
  const read = full.data?.question.resultJson;
  const q = read ? { ...listed, resultJson: read } : listed;
  // Outputs that may still come: a visual or an opened Result holds its
  // place for them until they are read (or can't be).
  const reading =
    !!settled &&
    !settled.queries.some((r) => r.output) &&
    !full.isSuccess &&
    !full.isError;
  const stream = useStream(q);
  const view = useMemo(
    () =>
      read ? questionView({ ...listed, resultJson: read }, budget) : first,
    [read, listed, budget, first],
  );
  const citations = view.kind === "answered" ? view.result.citations : [];
  // The cited entities are named as their pages name them, once near the
  // view: a long thread doesn't read the names of every answer it holds.
  const { named, ...links } = useCitations(
    q,
    seen ? citedEntities(citations) : [],
  );
  const focus = questionFocus(q);
  const [lit, setLit] = useState("");
  // The steps opened to their input and output, by index.
  const [shown, setShown] = useState<ReadonlySet<number>>(new Set());
  const [cancelling, setCancelling] = useState(false);
  const stepId = (evidence: string) => `${q.id}-${evidence}`;
  // A citation with no page opens the step that returned it, and reads the
  // outputs its Output shows.
  const showStep = (evidence: string) => {
    setLit("");
    setOpened(true);
    requestAnimationFrame(() => {
      setLit(evidence);
      document
        .getElementById(stepId(evidence))
        ?.scrollIntoView({ block: "center", behavior: "smooth" });
    });
  };
  const cancel = async () => {
    setCancelling(true);
    try {
      await cancelQuestion(ws, o, q.id);
      await qc.invalidateQueries({
        queryKey: [ws, "ontology", o, "questions"],
      });
    } catch (e) {
      notify((e as Error).message, true);
    } finally {
      setCancelling(false);
    }
  };
  const live = isLiveQuestion(q);
  const steps = stream.steps;
  const trace = traceOf(view, steps.length, budget);
  // The queries as the agent ran them, and their outputs, by the evidence
  // they were recorded as.
  const records = new Map(
    ("result" in view && view.result ? view.result.queries : []).map((r) => [
      r.evidence_id,
      r,
    ]),
  );
  const ran = new Map([...records].map(([e, r]) => [e, r.query]));
  const known = knownOf(steps, citations, named);
  const ctx: CitationContext = {
    ...links,
    name: known.name,
    result: (evidence) => resultLabel(steps, evidence, ran),
  };
  // An entity a visual shows opens its page, reading the question's load.
  const entityHref = (entity: EntityRef) => citationHref({ entity }, ctx);
  return (
    <article
      className="ak-turn"
      aria-label={`Question: ${q.question}`}
      ref={card}
    >
      <div className="ak-question">
        <span className="hf-sr-only">You asked: </span>
        <p>{q.question}</p>
      </div>
      <p className="ak-question-meta">
        {focus && `About ${focus.type} ${focus.id} · `}
        {formatRelative(q.createdAt || "")}
      </p>
      <div className="ak-reply">
        <Avatar />
        <div className="ak-reply-body">
          <section className={cx("ak-trace", `is-${trace.phase}`)}>
            <div className="ak-trace-head">
              <span className="ak-trace-status" role="status">
                <span className="ak-trace-dot" aria-hidden />
                {trace.headline}
              </span>
              <span className="ak-trace-tools">
                <span className="ak-trace-time">
                  {live
                    ? trace.detail
                    : [formatUsd(q.usdMicros || 0), stream.model]
                        .filter(Boolean)
                        .join(" · ")}
                </span>
                {live && (
                  <Button
                    size="sm"
                    variant="ghost"
                    icon={CircleStop}
                    pending={cancelling}
                    onClick={() => void cancel()}
                  >
                    Cancel
                  </Button>
                )}
              </span>
            </div>
            {steps.length > 0 && (
              <ol className="ak-steps ak-calls" aria-label="Steps">
                {steps.map((s, i) => {
                  const evidence =
                    s.result?.kind === "recorded"
                      ? s.result.evidenceId
                      : undefined;
                  const record =
                    s.tool === "query" && evidence
                      ? records.get(evidence)
                      : undefined;
                  const query = record?.query || s.query;
                  const t = query && queryText(query);
                  const graph = graphQuery(s);
                  const failed = s.result?.kind === "failed";
                  const result = s.result ? resultText(s) : live && "Working…";
                  const said = t ? undefined : stepInput(s, known);
                  // A refused answer: why, for a person; the engine's words
                  // to the model stay behind a disclosure.
                  const refusal =
                    s.tool === "answer" && s.result?.kind === "failed"
                      ? s.result.error
                      : undefined;
                  const ended = s.result?.kind === "ended" && isEnding(s.tool);
                  // Its arguments, unless they show as a query or graph
                  // code already, or hold nothing (the load's overview).
                  const raw = query ?? s.input;
                  const args =
                    !ended &&
                    !t &&
                    graph === undefined &&
                    raw !== null &&
                    JSON.stringify(raw) !== "{}"
                      ? JSON.stringify(raw, null, 2)
                      : undefined;
                  const input = !!t || graph !== undefined || !!args;
                  const opens = input || !!record || refusal !== undefined;
                  const open =
                    opens && (shown.has(i) || (!!evidence && evidence === lit));
                  const body = `${q.id}-step-${i}`;
                  const toggle = () => {
                    if (open && evidence === lit) setLit("");
                    if (!open && record) setOpened(true);
                    setShown((was) => {
                      const next = new Set(was);
                      if (open) next.delete(i);
                      else next.add(i);
                      return next;
                    });
                  };
                  // The call that ended the question closes the list in
                  // words: how it ended, no call to open.
                  if (ended)
                    return (
                      <li key={i} className="ak-call is-done is-end">
                        <div className="ak-call-head">
                          <StepMark step={s} live={live} />
                          <span className="ak-call-what">
                            <strong>{stepHeading(s)}</strong>
                          </span>
                        </div>
                      </li>
                    );
                  const head = (
                    <>
                      <StepMark step={s} live={live} />
                      <code className="ak-call-tool">{s.tool || "result"}</code>
                      <span className="ak-call-what" title={stepHeading(s)}>
                        {t?.path ??
                          said ??
                          (graph !== undefined
                            ? "A graph query"
                            : s.tool === "answer"
                              ? "Drafted an answer"
                              : stepHeading(s))}
                      </span>
                      {result && (
                        <span className="ak-call-result">{result}</span>
                      )}
                      {opens && (
                        <ChevronRight
                          size={14}
                          className="ak-call-chevron"
                          aria-hidden
                        />
                      )}
                    </>
                  );
                  return (
                    <li
                      key={i}
                      id={evidence && stepId(evidence)}
                      aria-label={`Step ${i + 1}: ${stepHeading(s)}`}
                      className={cx(
                        "ak-call",
                        s.result
                          ? failed
                            ? "is-error"
                            : "is-done"
                          : live
                            ? "is-running"
                            : "is-stopped",
                        open && "is-open",
                        !!evidence && evidence === lit && "is-lit",
                      )}
                    >
                      {opens ? (
                        <button
                          type="button"
                          className="ak-call-head"
                          aria-expanded={open}
                          aria-controls={body}
                          onClick={toggle}
                        >
                          {head}
                        </button>
                      ) : (
                        <div className="ak-call-head">{head}</div>
                      )}
                      {refusal !== undefined && (
                        <p className="ak-call-note">
                          {refusalText(
                            refusal,
                            steps
                              .slice(i + 1)
                              .some(
                                (x) =>
                                  isEnding(x.tool) &&
                                  x.result?.kind === "ended",
                              ),
                          )}
                        </p>
                      )}
                      {open && (
                        <div className="ak-call-body" id={body}>
                          {input && (
                            <section
                              className="ak-call-part"
                              aria-label="Input"
                            >
                              <h4>Input</h4>
                              {t ? (
                                <div className="ak-query" aria-label="Query">
                                  <code>{t.path}</code>
                                  {t.filters.map((f) => (
                                    <span key={f}>{f}</span>
                                  ))}
                                  <span>{t.returns}</span>
                                </div>
                              ) : (
                                <pre
                                  className="on-code"
                                  aria-label={
                                    graph !== undefined
                                      ? "Graph query"
                                      : "Arguments"
                                  }
                                >
                                  <code>{graph ?? args}</code>
                                </pre>
                              )}
                            </section>
                          )}
                          {refusal !== undefined && (
                            <section
                              className="ak-call-part"
                              aria-label="What the agent was told"
                            >
                              <h4>What the agent was told</h4>
                              <pre className="on-code ak-told">
                                <code>{refusal}</code>
                              </pre>
                            </section>
                          )}
                          {record && (
                            <section
                              className="ak-call-part ak-step-result"
                              aria-label="Output"
                            >
                              <h4>Output</h4>
                              {record.output ? (
                                <AskVisual
                                  output={record.output}
                                  query={record.query}
                                  href={entityHref}
                                  untitled
                                />
                              ) : reading ? (
                                <Skeleton width="100%" height={120} />
                              ) : (
                                <p className="ak-call-note">
                                  {full.isError
                                    ? "Its result couldn’t be read."
                                    : "Its result wasn’t kept with the answer."}
                                </p>
                              )}
                            </section>
                          )}
                        </div>
                      )}
                    </li>
                  );
                })}
              </ol>
            )}
            {stream.unreadable > 0 && (
              <p className="ak-trace-note">
                {stream.unreadable === 1
                  ? "1 step couldn’t be read."
                  : `${stream.unreadable} steps couldn’t be read.`}
              </p>
            )}
            {trace.note && <p className="ak-trace-note">{trace.note}</p>}
          </section>
          <Outcome
            q={q}
            view={view}
            ctx={ctx}
            budget={budget}
            showStep={showStep}
            entityHref={entityHref}
            reading={reading}
          />
        </div>
      </div>
    </article>
  );
}

/** How an ended question ended, under its steps; nothing while it runs. */
function Outcome({
  q,
  view,
  ctx,
  budget,
  showStep,
  entityHref,
  reading,
}: {
  q: FoundryQuestion;
  view: QuestionView;
  ctx: CitationContext;
  budget?: QuestionBudget | null;
  showStep: (evidence: string) => void;
  entityHref: (r: EntityRef) => string | undefined;
  reading: boolean;
}) {
  const { id: o } = useOntology();
  switch (view.kind) {
    case "waiting":
    case "queued":
    case "retrying":
    case "running":
      return null;
    case "cancelled":
      return (
        <Callout
          icon={Ban}
          tone="muted"
          title="Cancelled"
          text="It stopped before it answered."
        />
      );
    case "failed":
      return (
        <Callout
          icon={CircleAlert}
          tone="danger"
          title="It couldn’t answer"
          text={
            <>
              <SettingsLinked text={view.error} />
              {view.result && <Spent usage={view.result.usage} />}
            </>
          }
        />
      );
    case "budget_exhausted":
      return (
        <Callout
          icon={Gauge}
          tone="danger"
          title="Stopped at this question’s limit"
          text={
            <>
              {`It used what a question may (${formatUsd(budget?.questionUsdMicros || 500_000)}, ${budget?.maxSteps ?? 20} steps or ${Math.round((budget?.maxWallSecs ?? 240) / 60)} minutes) before it could answer. Ask something narrower.`}
              <Spent usage={view.result.usage} />
            </>
          }
        />
      );
    case "cannot_answer":
      return (
        <Callout
          icon={FileQuestion}
          tone="muted"
          title="This ontology can’t answer that"
          text={
            <>
              {view.result.reason || "What it holds doesn’t say."}
              {view.result.gap && <> It lacks: {view.result.gap}</>}
              <Spent usage={view.result.usage} />
              <span className="ak-draft">
                <ButtonLink
                  size="sm"
                  to={paths.generate(o, {
                    goal: draftGoal(q.question || "", view.result),
                  })}
                >
                  Draft a new version with this
                </ButtonLink>
              </span>
            </>
          }
        />
      );
    case "answered":
      return (
        <Answer
          q={q}
          view={view}
          ctx={ctx}
          showStep={showStep}
          entityHref={entityHref}
          reading={reading}
        />
      );
  }
}

/**
 * An answer: plain text with citation chips, each visual right after the
 * block it follows (outside the text's measure), then its sources. While
 * the outputs are read (`reading`), a visual's place holds a skeleton; one
 * whose output never comes is left out.
 */
function Answer({
  q,
  view,
  ctx,
  showStep,
  entityHref,
  reading,
}: {
  q: FoundryQuestion;
  view: Extract<QuestionView, { kind: "answered" }>;
  ctx: CitationContext;
  showStep: (evidence: string) => void;
  entityHref: (r: EntityRef) => string | undefined;
  reading: boolean;
}) {
  const result = view.result;
  const { blocks, sources } = answerView(
    result.answer.blocks,
    result.citations,
  );
  const shown = new Map(blocks.map((b) => [b.at, b]));
  const drawn = new Map(drawable(result).map((d) => [d.visual.evidence_id, d]));
  const placed = placeVisuals(
    (result.answer.visuals ?? []).map((visual) => ({ visual })),
    result.answer.blocks.length,
  );
  const byNumber = new Map(sources.map((s) => [s.n, s.citation]));
  const chip = (n: number, key: number) => {
    const c = byNumber.get(n)!,
      label = citationText(c.target, ctx),
      href = citationHref(c.target, ctx),
      name = `Source ${n}: ${label}`;
    if (href)
      return (
        <Link
          key={key}
          className="ak-chip"
          to={href}
          aria-label={name}
          title={label}
        >
          {n}
        </Link>
      );
    // With no page to open, a chip opens the step its target came from.
    const evidence =
      "result" in c.target ? c.target.result.evidence_id : c.evidence_id;
    return (
      <button
        key={key}
        type="button"
        className="ak-chip"
        aria-label={name}
        title={label}
        disabled={!evidence}
        onClick={() => showStep(evidence)}
      >
        {n}
      </button>
    );
  };
  const run = (parts: AnswerPart[]): ReactNode[] =>
    parts.map((p, i) =>
      "cite" in p ? (
        chip(p.cite, i)
      ) : (
        <span key={i}>
          {i > 0 && !/^[.,;:!?)’”]/.test(p.text) ? " " : ""}
          {p.text}
        </span>
      ),
    );
  const block = (b: (typeof blocks)[number]) =>
    b.kind === "para" ? (
      <p>{run(b.parts)}</p>
    ) : (
      <ul>
        {b.items.map((item, j) => (
          <li key={j}>{run(item)}</li>
        ))}
      </ul>
    );
  return (
    <section className="ak-answer" aria-label="Answer">
      {placed.map((after, i) => (
        <Fragment key={i}>
          {shown.has(i) && block(shown.get(i)!)}
          {after.map(({ visual }) => {
            const d = drawn.get(visual.evidence_id);
            if (!d)
              return reading ? (
                <Skeleton
                  key={visual.evidence_id}
                  className="ak-visual-wait"
                  width="100%"
                  height={248}
                />
              ) : null;
            return (
              <AskVisual
                key={visual.evidence_id}
                output={d.output}
                query={d.query}
                hint={visual}
                href={entityHref}
                onShowQuery={() => showStep(visual.evidence_id)}
              />
            );
          })}
        </Fragment>
      ))}
      <div className="ak-foot">
        {sources.length > 0 && (
          <>
            <span className="ak-foot-label" aria-hidden>
              Sources
            </span>
            <ol className="ak-sources" aria-label="Sources">
              {sources.map(({ n, citation }) => {
                const label = citationText(citation.target, ctx),
                  href = citationHref(citation.target, ctx);
                return (
                  <li key={n} value={n} data-n={n}>
                    {href ? <Link to={href}>{label}</Link> : label}
                  </li>
                );
              })}
            </ol>
          </>
        )}
        <p className="ak-answer-meta">
          {q.finishedAt && `Answered ${formatRelative(q.finishedAt)} · `}
          {usageText(view.result.usage)}
        </p>
      </div>
    </section>
  );
}

/** What an ended question spent, after what its callout says. */
function Spent({ usage }: { usage: AskUsage }) {
  return (
    <small className="ak-spent" aria-label="Spent">
      {usageText(usage)}
    </small>
  );
}

/**
 * A textarea that grows with its text from `minRows` to `maxRows` lines,
 * then scrolls down (never sideways): helix-foundry's Analyst PromptInput,
 * without its suggestions.
 */
function PromptInput({
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
      const style = getComputedStyle(el),
        line = parseFloat(style.lineHeight) || 20,
        pad = parseFloat(style.paddingTop) + parseFloat(style.paddingBottom);
      const min = line * minRows + pad,
        max = line * maxRows + pad;
      el.style.height = "auto";
      const full = el.scrollHeight;
      el.style.height = Math.max(min, Math.min(full, max)) + "px";
      el.style.overflowY = full > max + 1 ? "auto" : "hidden";
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
 * Asks a question: a new thread (optionally about one entity) or, docked
 * under a thread, a follow-up in `thread`. Only a ready or partial load can
 * be asked about.
 */
function Composer({
  load,
  thread,
  focus,
  busy,
  docked,
  suggestions = [],
  onAsked,
}: {
  load: Load;
  thread?: string;
  focus?: EntityRef;
  busy?: boolean;
  docked?: boolean;
  // Questions under the box that a click asks at once.
  suggestions?: string[];
  onAsked?: (q: FoundryQuestion) => void;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology(),
    qc = useQueryClient();
  const [text, setText] = useState(""),
    [about, setAbout] = useState(focus),
    [token, setToken] = useState(() => crypto.randomUUID()),
    [pending, setPending] = useState(false),
    [error, setError] = useState<unknown>(null);
  // Locally, questions wait for an AI provider: the WFE refuses them until
  // an owner chooses one.
  const aiReady = useAiConfigured();
  const input = useRef<HTMLTextAreaElement>(null),
    hint = useId();
  const ready = !!load && askable(load.status);
  const length = [...text.trim()].length,
    tooLong = length > MAX_QUESTION;
  const submit = async (question = text.trim()) => {
    if (!load) return;
    setPending(true);
    setError(null);
    try {
      const r = await askQuestion(ws, o, {
        loadId: load.id,
        question,
        ...(thread ? { threadId: thread } : {}),
        ...(about ? { focusJson: JSON.stringify(about) } : {}),
        clientToken: token,
      });
      setText("");
      setToken(crypto.randomUUID());
      await qc.invalidateQueries({
        queryKey: [ws, "ontology", o, "questions"],
      });
      onAsked?.(r.question);
    } catch (e) {
      setError(e);
    } finally {
      setPending(false);
    }
  };
  // The load as the area names it: "the current load (v3)", or "the v3 load".
  const named =
    load &&
    (load.current
      ? `the current load (${loadVersion(load)})`
      : `the ${loadVersion(load)} load`);
  const caption = tooLong
    ? `${length - MAX_QUESTION} characters too long.`
    : !load
      ? "Pick a load to ask about."
      : !ready
        ? `${named!.charAt(0).toUpperCase() + named!.slice(1)} can’t be asked about until it’s written.`
        : busy
          ? "You can ask a follow-up once this answer is ready."
          : `Answers from ${named}${thread ? ", using this thread’s earlier answers" : ""} · ${MOD}Enter to ask`;
  return (
    <form
      className="ak-ask"
      aria-label={thread ? "Ask a follow-up" : "Ask a question"}
      aria-busy={pending || undefined}
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      {about && (
        <span className="ak-focus">
          <Tag>
            About {about.type} {about.id}
          </Tag>
          {!thread && (
            <button
              type="button"
              className="ak-focus-clear"
              aria-label="Ask about anything"
              title="Ask about anything"
              onClick={() => setAbout(undefined)}
            >
              <X size={13} aria-hidden />
            </button>
          )}
        </span>
      )}
      <div
        className="ak-composer"
        onClick={(e) => {
          if (e.target === e.currentTarget) input.current?.focus();
        }}
      >
        <PromptInput
          inputRef={input}
          className="ak-input"
          aria-label="Question"
          aria-describedby={hint}
          aria-keyshortcuts="Meta+Enter Control+Enter"
          minRows={docked ? 1 : 2}
          maxRows={docked ? 6 : 8}
          value={text}
          data-autofocus
          placeholder={
            thread
              ? "Ask a follow-up…"
              : "e.g. Which customers have contracts over 100,000 signed this year, and who signed them?"
          }
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
              e.preventDefault();
              e.currentTarget.form?.requestSubmit();
            }
          }}
        />
        <button
          type="submit"
          className="ak-send"
          aria-label="Ask"
          title="Ask"
          disabled={
            pending ||
            !ready ||
            busy ||
            !text.trim() ||
            tooLong ||
            aiReady === false
          }
        >
          {pending ? (
            <LoaderCircle size={17} className="hf-spin" aria-hidden />
          ) : (
            <ArrowUp size={17} aria-hidden />
          )}
        </button>
      </div>
      <p id={hint} className={cx("ak-hint", tooLong && "is-error")}>
        {caption}
      </p>
      <AiRefusal action="Asking questions" error={error} ready={aiReady}>
        {!!error && (
          <p className="ak-hint is-error" role="alert">
            <SettingsLinked text={sentence((error as Error).message)} />
          </p>
        )}
      </AiRefusal>
      {suggestions.length > 0 && (
        <div
          className="ak-suggestions"
          role="group"
          aria-label="Suggested questions"
        >
          {suggestions.map((q) => (
            <button
              key={q}
              type="button"
              className="ak-suggestion"
              disabled={pending || !ready || busy || aiReady === false}
              onClick={() => void submit(q)}
            >
              {q}
            </button>
          ))}
        </div>
      )}
    </form>
  );
}

/**
 * "Ask about this": a new thread about one entity of the load the page
 * reads, in a drawer; once asked, the question streams here, and Ask holds
 * the thread.
 */
export function AskDrawer({
  focus,
  label,
  loadId,
  onClose,
}: {
  focus: EntityRef;
  label: string;
  loadId: string;
  onClose: () => void;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const loads = useQuery(ontologyQueries.loads(ws, o));
  const load = loads.data?.loads?.find((l) => l.id === loadId);
  const [asked, setAsked] = useState<FoundryQuestion | null>(null);
  const live = useQuery({
    ...questionQueries.question(ws, o, asked?.id || ""),
    enabled: !!asked,
  });
  const q = live.data?.question ?? asked;
  return (
    <Dialog
      variant="drawer"
      size="lg"
      title={`Ask about ${label}`}
      onClose={onClose}
    >
      <div className="ak-drawer">
        {q ? (
          <>
            <QuestionCard question={q} />
            <ButtonLink
              className="ak-open"
              to={withLoad(
                paths.ask(o, { thread: q.threadId || q.id }),
                load?.current ? null : loadId,
              )}
            >
              Open in Analyst
            </ButtonLink>
          </>
        ) : (
          <Composer load={load} focus={focus} onAsked={setAsked} />
        )}
      </div>
    </Dialog>
  );
}
