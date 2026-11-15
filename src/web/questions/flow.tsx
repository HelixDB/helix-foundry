import "./questions.css";
import {
  useEffect,
  useId,
  useReducer,
  useRef,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
} from "react";
import { ArrowLeft, ArrowRight, Check, PenLine, Send } from "lucide-react";
import type { AgentQuestionAnswer } from "../../shared/wfe";
import { Button, Tag, cx } from "../kit";
import {
  MAX_OWN,
  answerText,
  answersOf,
  canLeave,
  canLeaveRest,
  canReview,
  check,
  flowKey,
  keyAction,
  leftText,
  questionName,
  reduceFlow,
  restoreFlow,
  type FlowAction,
  type FlowState,
  type Question,
} from "./model";

// The question-by-question flow, shared by a generation's page, an
// unfinished ontology's setup and the Assistant's dock: a strip of the
// questions (each answered one showing its answer, and opened again with a
// click) and Review; one question at a time, its options a list the arrows
// move through (radios for one, checkboxes for several, the recommended
// one first), "Something else" opening a box for the person's own words;
// Back and Next; and Review, which sends. A half-answered round is kept in
// this tab (sessionStorage, `storageKey`) until it is sent. What the agents
// wrote is plain text.

const read = (key: string) => {
  try {
    return sessionStorage.getItem(flowKey(key));
  } catch {
    return null;
  }
};
const keep = (key: string, s: FlowState | null) => {
  try {
    if (s) sessionStorage.setItem(flowKey(key), JSON.stringify(s));
    else sessionStorage.removeItem(flowKey(key));
  } catch {
    /* a convenience: the flow works without it */
  }
};

/** "Customer (type)" for the draft item "type.Customer". */
const itemText = (item: string) => {
  const [kind, ...rest] = item.split(".");
  return rest.length ? `${rest.join(".")} (${kind})` : item;
};

export function QuestionFlow({
  questions,
  storageKey,
  onSend,
  pending,
  disabled,
  error,
  sendLabel = "Send answers",
  agent = "the agent",
}: {
  questions: Question[];
  // Names the round in this tab's storage: a generation's round, a turn.
  storageKey: string;
  // Sends the answers and the ids left to the agent; resolves true once
  // sent, when the kept round is forgotten.
  onSend: (answers: AgentQuestionAnswer[], left: string[]) => Promise<boolean>;
  pending: boolean;
  // Sending can't be done now (no AI provider, the thread busy).
  disabled?: boolean;
  // Why the last send failed, shown on Review.
  error?: ReactNode;
  sendLabel?: string;
  // Who a question left unanswered is left to, in words.
  agent?: string;
}) {
  const [s, dispatch] = useReducer(
    (state: FlowState, a: FlowAction) => reduceFlow(questions, state, a),
    undefined,
    () => restoreFlow(questions, read(storageKey)),
  );
  useEffect(() => keep(storageKey, s), [storageKey, s]);
  const n = questions.length,
    q = questions[s.step],
    id = useId();
  const root = useRef<HTMLDivElement>(null),
    // Focus follows the step and the cursor once the person has acted, never on load.
    acted = useRef(false),
    ownBox = useRef<HTMLTextAreaElement>(null),
    toOwn = useRef(false);
  const act = (a: FlowAction) => {
    acted.current = true;
    toOwn.current = a.type === "own";
    dispatch(a);
  };
  // "Something else" opened: its box takes the focus.
  useEffect(() => {
    if (!toOwn.current || !ownBox.current) return;
    toOwn.current = false;
    ownBox.current.focus();
  });
  useEffect(() => {
    if (!acted.current || toOwn.current) return;
    const el = root.current;
    const target =
      el?.querySelector<HTMLElement>(`[data-row="${s.cursor}"]`) ??
      el?.querySelector<HTMLElement>("[data-step-focus]");
    if (target && !target.contains(document.activeElement)) target.focus();
  }, [s.step, s.cursor]);
  const send = async () => {
    const out = answersOf(questions, s.drafts);
    if (!out || pending || disabled) return;
    if (await onSend(out.answers, out.left)) keep(storageKey, null);
  };
  return (
    <div ref={root} className="qf">
      <nav className="qf-strip" aria-label="Questions">
        <ol>
          {questions.map((x, i) => {
            const c = check(x, s.drafts[x.id]);
            return (
              <li key={x.id}>
                <button
                  type="button"
                  className={cx(
                    "qf-strip-step",
                    c.kind === "answered" && "is-answered",
                  )}
                  aria-current={i === s.step ? "step" : undefined}
                  onClick={() => act({ type: "go", step: i })}
                >
                  <span className="qf-strip-mark" aria-hidden>
                    {c.kind === "answered" ? <Check size={11} /> : i + 1}
                  </span>
                  <span className="qf-strip-text">
                    <span className="qf-strip-name">{questionName(x, i)}</span>
                    <span className="qf-strip-answer">
                      {c.kind === "answered"
                        ? answerText(c.answer)
                        : c.kind === "incomplete"
                          ? "Not finished"
                          : x.blocking
                            ? "Needs an answer"
                            : "Optional"}
                    </span>
                  </span>
                </button>
              </li>
            );
          })}
          <li>
            <button
              type="button"
              className="qf-strip-step"
              aria-current={s.step === n ? "step" : undefined}
              disabled={!canReview(questions, s.drafts)}
              onClick={() => act({ type: "go", step: n })}
            >
              <span className="qf-strip-mark" aria-hidden>
                <Send size={10} />
              </span>
              <span className="qf-strip-text">
                <span className="qf-strip-name">Review</span>
              </span>
            </button>
          </li>
        </ol>
      </nav>
      <p className="hf-sr-only" aria-live="polite">
        {q
          ? `Question ${s.step + 1} of ${n}: ${questionName(q, s.step)}`
          : `Reviewing ${n === 1 ? "your answer" : `${n} answers`} before they are sent`}
      </p>
      {q ? (
        <Step
          key={q.id}
          id={id}
          q={q}
          index={s.step}
          count={n}
          s={s}
          act={act}
          ownBox={ownBox}
          rest={canLeaveRest(questions, s.drafts, s.step)}
          agent={agent}
          last={s.step === n - 1}
        />
      ) : (
        <div className="qf-review">
          <p className="qf-review-title" tabIndex={-1} data-step-focus>
            Review your answers
          </p>
          <ol className="qf-review-list">
            {questions.map((x, i) => {
              const c = check(x, s.drafts[x.id]);
              return (
                <li key={x.id}>
                  <div className="qf-review-text">
                    <span className="qf-review-name">{questionName(x, i)}</span>
                    <span className="qf-review-q">{x.text}</span>
                    <span
                      className={cx(
                        "qf-review-a",
                        c.kind !== "answered" && "is-left",
                      )}
                    >
                      {c.kind === "answered"
                        ? answerText(c.answer)
                        : leftText(x, agent)}
                    </span>
                  </div>
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-label={`Change ${questionName(x, i)}`}
                    onClick={() => act({ type: "go", step: i })}
                  >
                    Change
                  </Button>
                </li>
              );
            })}
          </ol>
          {error}
          <div className="qf-nav">
            <Button
              variant="ghost"
              icon={ArrowLeft}
              onClick={() => act({ type: "back" })}
            >
              Back
            </Button>
            <Button
              variant="primary"
              icon={Send}
              pending={pending}
              disabled={disabled}
              onClick={() => void send()}
            >
              {sendLabel}
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}

/** One question: its text, its options or a box for words, Back and Next. */
function Step({
  id,
  q,
  index,
  count,
  s,
  act,
  ownBox,
  rest,
  agent,
  last,
}: {
  id: string;
  q: Question;
  index: number;
  count: number;
  s: FlowState;
  act: (a: FlowAction) => void;
  ownBox: RefObject<HTMLTextAreaElement | null>;
  // "Let the agent decide the rest" applies.
  rest: boolean;
  agent: string;
  last: boolean;
}) {
  const draft = s.drafts[q.id],
    c = check(q, draft),
    textId = `${id}-text`,
    hintId = `${id}-hint`;
  const own =
    draft?.kind === "own"
      ? draft.text
      : draft?.kind === "many" && draft.own !== null
        ? draft.own
        : null;
  const ownChars = [...(own ?? "").trim()].length;
  const next = () => act({ type: "next" });
  const box = (label: string) => (
    <label className="hf-field qf-own">
      <span className="hf-sr-only">{label}</span>
      <textarea
        ref={ownBox}
        rows={3}
        value={own ?? ""}
        data-step-focus={q.ask.kind === "free" || undefined}
        aria-labelledby={q.ask.kind === "free" ? textId : undefined}
        aria-invalid={c.kind === "incomplete" && ownChars > 0}
        aria-describedby={hintId}
        placeholder={
          q.ask.kind === "free" && q.ask.suggested
            ? `The agent suggests: ${q.ask.suggested}`
            : "Write your answer"
        }
        onChange={(e) => act({ type: "write", text: e.target.value })}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
            e.preventDefault();
            next();
          }
        }}
      />
    </label>
  );
  const onKey = (e: KeyboardEvent) => {
    if (e.altKey || e.ctrlKey || e.metaKey) return;
    const a = keyAction(q, s, e.key);
    if (!a) return;
    e.preventDefault();
    act(a);
  };
  const multiple = q.ask.kind === "choice" && q.ask.multiple,
    suggested = q.ask.kind === "free" ? q.ask.suggested : "";
  return (
    <div className="qf-step">
      <div className="qf-meta">
        <span>
          Question {index + 1} of {count}
        </span>
        {q.blocking ? (
          <Tag tone="warning">Needs an answer</Tag>
        ) : (
          <Tag>Optional</Tag>
        )}
        {multiple && <span>Choose any that apply</span>}
      </div>
      <p id={textId} className="qf-text">
        {q.text}
      </p>
      {q.items.length > 0 && (
        <p className="qf-about">About {q.items.map(itemText).join(", ")}</p>
      )}
      {q.ask.kind === "choice" ? (
        <>
          <div
            className="qf-options"
            role={multiple ? "group" : "radiogroup"}
            aria-labelledby={textId}
            aria-describedby={hintId}
            aria-required={(q.blocking && !multiple) || undefined}
            onKeyDown={onKey}
          >
            {q.ask.options.map((o, i) => {
              const checked =
                draft?.kind === "option"
                  ? draft.label === o.label
                  : draft?.kind === "many" && draft.labels.includes(o.label);
              return (
                <div
                  key={o.label}
                  data-row={i}
                  role={multiple ? "checkbox" : "radio"}
                  aria-checked={checked}
                  tabIndex={s.cursor === i ? 0 : -1}
                  className={cx("qf-option", checked && "is-checked")}
                  onClick={() =>
                    act(
                      multiple
                        ? { type: "toggle", label: o.label }
                        : { type: "choose", label: o.label, advance: true },
                    )
                  }
                >
                  <span className="qf-key" aria-hidden>
                    {i < 9 ? i + 1 : ""}
                  </span>
                  <span className="qf-option-body">
                    <span className="qf-option-label">
                      {o.label}
                      {o.label ===
                        (q.ask as { recommended: string | null })
                          .recommended && <Tag tone="accent">Recommended</Tag>}
                    </span>
                    {o.description && (
                      <span className="qf-option-desc">{o.description}</span>
                    )}
                  </span>
                  <span
                    className={cx("qf-mark", multiple ? "is-box" : "is-dot")}
                    aria-hidden
                  >
                    {checked && multiple && <Check size={12} />}
                  </span>
                </div>
              );
            })}
            <div
              data-row={q.ask.options.length}
              role={multiple ? "checkbox" : "radio"}
              aria-checked={own !== null}
              tabIndex={s.cursor === q.ask.options.length ? 0 : -1}
              className={cx("qf-option", own !== null && "is-checked")}
              onClick={() => act({ type: "own" })}
            >
              <span className="qf-key" aria-hidden>
                {q.ask.options.length < 9 ? q.ask.options.length + 1 : ""}
              </span>
              <span className="qf-option-body">
                <span className="qf-option-label">Something else</span>
                <span className="qf-option-desc">
                  {multiple
                    ? "Add your own words to what you choose"
                    : "Answer in your own words"}
                </span>
              </span>
              <span
                className={cx("qf-mark", multiple ? "is-box" : "is-dot")}
                aria-hidden
              >
                {own !== null && multiple && <Check size={12} />}
              </span>
            </div>
          </div>
          {own !== null && box("Your own words")}
        </>
      ) : (
        <>
          {box("Your answer")}
          {suggested && !own && (
            <Button
              size="sm"
              variant="ghost"
              icon={PenLine}
              className="qf-suggest"
              onClick={() => act({ type: "write", text: suggested })}
            >
              Use “{suggested}”
            </Button>
          )}
        </>
      )}
      <p
        id={hintId}
        className={cx(
          "qf-hint",
          c.kind === "incomplete" && ownChars > 0 && "is-error",
        )}
      >
        {c.kind === "incomplete" && ownChars > 0
          ? c.reason
          : own !== null
            ? `${Math.max(0, MAX_OWN - ownChars)} characters left. Enter to go on, Shift+Enter for a new line.`
            : q.ask.kind === "choice"
              ? multiple
                ? "Arrows move, Space ticks, Enter goes on."
                : "Arrows move, Enter chooses, or press a number."
              : ""}
      </p>
      <div className="qf-nav">
        <Button
          variant="ghost"
          icon={ArrowLeft}
          disabled={index === 0}
          onClick={() => act({ type: "back" })}
        >
          Back
        </Button>
        <span className="qf-nav-end">
          {rest && (
            <Button variant="ghost" onClick={() => act({ type: "rest" })}>
              Let {agent} decide the rest
            </Button>
          )}
          <Button
            variant="primary"
            iconRight={ArrowRight}
            disabled={!canLeave(q, draft)}
            onClick={next}
          >
            {c.kind === "unanswered" ? "Skip" : last ? "Review" : "Next"}
          </Button>
        </span>
      </div>
    </div>
  );
}

/**
 * A round of questions to read, not answer (a member's view): each with
 * its options, the recommended one marked, or the words the agent suggests.
 */
export function QuestionList({ questions }: { questions: Question[] }) {
  return (
    <ol className="qf-list">
      {questions.map((q, i) => (
        <li key={q.id} className="qf-list-item">
          <div className="qf-meta">
            <span>{questionName(q, i)}</span>
            {q.blocking ? (
              <Tag tone="warning">Needs an answer</Tag>
            ) : (
              <Tag>Optional</Tag>
            )}
            {q.ask.kind === "choice" && q.ask.multiple && (
              <span>Any that apply</span>
            )}
          </div>
          <p className="qf-text">{q.text}</p>
          {q.items.length > 0 && (
            <p className="qf-about">About {q.items.map(itemText).join(", ")}</p>
          )}
          {q.ask.kind === "choice" ? (
            <ul className="qf-list-options" aria-label="Options">
              {q.ask.options.map((o) => (
                <li key={o.label}>
                  <span className="qf-option-label">
                    {o.label}
                    {o.label ===
                      (q.ask as { recommended: string | null }).recommended && (
                      <Tag tone="accent">Recommended</Tag>
                    )}
                  </span>
                  {o.description && (
                    <span className="qf-option-desc">{o.description}</span>
                  )}
                </li>
              ))}
            </ul>
          ) : (
            q.ask.suggested && (
              <p className="qf-about">The agent suggests: {q.ask.suggested}</p>
            )
          )}
        </li>
      ))}
    </ol>
  );
}

/**
 * Questions and their answers, compact: each question's name and text
 * beside its answer in words.
 */
export function AnswerSummary({
  rows,
  label = "Answers",
}: {
  rows: { name: string; question: string; answer: string; left?: boolean }[];
  label?: string;
}) {
  return (
    <dl className="qf-summary" aria-label={label}>
      {rows.map((r, i) => (
        <div key={i} className="qf-summary-row">
          <dt>
            <span className="qf-review-name">{r.name}</span>
            <span className="qf-review-q">{r.question}</span>
          </dt>
          <dd className={cx(r.left && "is-left")}>{r.answer}</dd>
        </div>
      ))}
    </dl>
  );
}
