import type { AgentQuestionAnswer } from "../../shared/wfe";
import { isRecord } from "../../shared/json";

// Questions an agent asks the person, asked one at a time: the ontology
// agent's about a draft (a generation that needs input) and the
// Founder's (helix_foundry_ask_person), one shape (the WFE's
// AgentQuestion). This is the flow's pure half: reading the questions
// without trust, what the person has chosen so far, how each key and click
// moves it, what it sends, and what a tab keeps of it. Every string an
// agent wrote may quote the data: it is shown as plain text.

/** At most this many characters of the person's own words, once trimmed (the WFE's). */
export const MAX_OWN = 2000;
/** At most this many options a question offers (the WFE's). */
export const MAX_OPTIONS = 20;

/** One answer a question offers. */
export type QuestionOption = { label: string; description: string };

/**
 * A question as the flow asks it: a free answer the person writes (the
 * agent may suggest words), or a choice of 2 or more options, of which the
 * person picks one, or several when `multiple`, its recommended one first.
 * A choice always takes the person's own words too: instead of an option
 * (one) or beside them (several).
 */
export type Question = {
  id: string;
  // A few words naming it, or "".
  header: string;
  text: string;
  // Needs an answer; one that doesn't may be left to the agent.
  blocking: boolean;
  // The draft items it is about ("type.Customer"); a generation's only.
  items: string[];
  ask:
    | { kind: "free"; suggested: string }
    | {
        kind: "choice";
        multiple: boolean;
        // As shown: the recommended option first, the rest in the agent's order.
        options: QuestionOption[];
        recommended: string | null;
      };
};

const str = (x: unknown) => (typeof x === "string" ? x : "");

/**
 * One question from the WFE's JSON, or undefined when it breaks the WFE's
 * own rules (an id, text, 2 to MAX_OPTIONS options with distinct labels, the
 * recommended one among them, multiple only with options).
 *
 * @example
 * readQuestion({ id: "q1", text: "Which?", options: [{ label: "A" }, { label: "B" }], recommended: "B" })?.ask;
 * // { kind: "choice", multiple: false, options: [{ label: "B", description: "" }, { label: "A", description: "" }], recommended: "B" }
 * readQuestion({ id: "q1", text: "Which?", options: [{ label: "A" }] }); // undefined
 */
export function readQuestion(x: unknown): Question | undefined {
  if (!isRecord(x)) return undefined;
  const id = str(x.id),
    text = str(x.text),
    // A choice's names a label exactly as written, as the WFE compares it.
    recommended = str(x.recommended),
    raw = Array.isArray(x.options) ? x.options : [];
  if (!id || !text.trim()) return undefined;
  const options = raw.map((o) =>
    isRecord(o)
      ? { label: str(o.label), description: str(o.description) }
      : { label: "", description: "" },
  );
  const base = {
    id,
    header: str(x.header).trim(),
    text,
    blocking: x.blocking === true,
    items: Array.isArray(x.items)
      ? x.items.filter((i): i is string => typeof i === "string")
      : [],
  };
  if (!options.length)
    return x.multiple === true
      ? undefined
      : { ...base, ask: { kind: "free", suggested: recommended.trim() } };
  const labels = options.map((o) => o.label);
  if (
    options.length < 2 ||
    options.length > MAX_OPTIONS ||
    labels.some((l, i) => !l.trim() || labels.indexOf(l) !== i) ||
    (recommended && !labels.includes(recommended))
  )
    return undefined;
  return {
    ...base,
    ask: {
      kind: "choice",
      multiple: x.multiple === true,
      options: [
        ...options.filter((o) => o.label === recommended),
        ...options.filter((o) => o.label !== recommended),
      ],
      recommended: recommended || null,
    },
  };
}

/**
 * A round of questions from the WFE's JSON: every one, or undefined when
 * any can't be read (a question left out could be one only the person can
 * answer) or two share an id.
 */
export function readQuestions(x: unknown): Question[] | undefined {
  if (!Array.isArray(x) || !x.length) return undefined;
  const out = x.map(readQuestion);
  const ids = out.map((q) => q?.id);
  return out.every((q): q is Question => !!q) &&
    ids.every((id, i) => ids.indexOf(id) === i)
    ? out
    : undefined;
}

/**
 * What the person has chosen for one question so far: an option of a
 * choice of one; their own words alone (a free question, or "Something
 * else" of a choice of one); or, of a choice of several, the options ticked
 * and their own words, `null` while that box is closed. A question with no
 * draft is unanswered: left to the agent, unless it blocks.
 */
export type Draft =
  | { kind: "option"; label: string }
  | { kind: "own"; text: string }
  | { kind: "many"; labels: readonly string[]; own: string | null };

/** How a question stands with its draft. */
export type Checked =
  | { kind: "unanswered" }
  // Chosen, but not yet an answer (own words blank or too long).
  | { kind: "incomplete"; reason: string }
  | { kind: "answered"; answer: AgentQuestionAnswer };

/** The person's own words, trimmed, or why they can't be sent. */
function words(text: string): { text: string } | { reason: string } {
  const t = text.trim(),
    n = [...t].length;
  if (!n) return { reason: "Write your answer, or choose an option." };
  if (n > MAX_OWN) return { reason: `${n - MAX_OWN} characters too long.` };
  return { text: t };
}

/**
 * The question's answer from its draft, as the WFE checks it: a free
 * question's words; a choice of one's option or words; a choice of
 * several's options, with words or without, or words alone. A box of own
 * words left blank beside options ticked is no words.
 *
 * @example
 * check(q1, { kind: "option", label: "A" }); // { kind: "answered", answer: { questionId: "q1", options: ["A"] } }
 * check(q1, { kind: "own", text: " " }); // { kind: "incomplete", reason: "Write your answer, or choose an option." }
 * check(q1, undefined); // { kind: "unanswered" }
 */
export function check(q: Question, draft: Draft | undefined): Checked {
  if (!draft) return { kind: "unanswered" };
  const offered = (l: string) =>
    q.ask.kind === "choice" && q.ask.options.some((o) => o.label === l);
  switch (draft.kind) {
    case "option":
      return q.ask.kind === "choice" && !q.ask.multiple && offered(draft.label)
        ? {
            kind: "answered",
            answer: { questionId: q.id, options: [draft.label] },
          }
        : { kind: "unanswered" };
    case "own": {
      if (q.ask.kind === "choice" && q.ask.multiple)
        return { kind: "unanswered" };
      const w = words(draft.text);
      return "reason" in w
        ? { kind: "incomplete", reason: w.reason }
        : {
            kind: "answered",
            answer: { questionId: q.id, options: [], text: w.text },
          };
    }
    case "many": {
      if (q.ask.kind !== "choice" || !q.ask.multiple)
        return { kind: "unanswered" };
      const labels = q.ask.options
        .map((o) => o.label)
        .filter((l) => draft.labels.includes(l));
      const own = draft.own === null ? null : words(draft.own);
      if (own && "reason" in own && (!labels.length || draft.own!.trim()))
        return { kind: "incomplete", reason: own.reason };
      if (!labels.length && !own) return { kind: "unanswered" };
      return {
        kind: "answered",
        answer: {
          questionId: q.id,
          options: labels,
          ...(own && "text" in own && { text: own.text }),
        },
      };
    }
  }
}

/** The step shown: a question's index, or the questions' count for Review. */
export type FlowState = {
  step: number;
  drafts: Readonly<Record<string, Draft>>;
  // The row of a choice's list the keyboard is on: an option, then
  // "Something else".
  cursor: number;
};

/** What a key or a click does to the flow. */
export type FlowAction =
  // A choice of one: choose an option, and go on (`advance`) or not.
  | { type: "choose"; label: string; advance: boolean }
  // A choice of several: tick or untick an option.
  | { type: "toggle"; label: string }
  // "Something else": a choice of one answers in words (again, while its
  // box is empty, closes it); a choice of several opens or closes its box.
  | { type: "own" }
  // The person's own words, as typed.
  | { type: "write"; text: string }
  // Unanswer the question shown (only one that doesn't block).
  | { type: "clear" }
  | { type: "next" }
  | { type: "back" }
  | { type: "go"; step: number }
  // To Review, every question left unanswered left to the agent.
  | { type: "rest" }
  | { type: "cursor"; to: number };

/** Whether the question can be left behind: answered, or unanswered and not blocking. */
export const canLeave = (q: Question, draft: Draft | undefined) => {
  const c = check(q, draft);
  return c.kind === "answered" || (c.kind === "unanswered" && !q.blocking);
};

/** Whether Review can be shown: every question answered, or left to the agent. */
export const canReview = (
  questions: readonly Question[],
  drafts: FlowState["drafts"],
) => questions.every((q) => canLeave(q, drafts[q.id]));

/**
 * Whether "Let the agent decide the rest" applies at `step`: everything
 * can go to Review and a question from `step` on is unanswered.
 */
export const canLeaveRest = (
  questions: readonly Question[],
  drafts: FlowState["drafts"],
  step: number,
) =>
  step < questions.length &&
  canReview(questions, drafts) &&
  questions
    .slice(step)
    .some((q) => check(q, drafts[q.id]).kind === "unanswered");

/** A choice's rows: its options as shown, then "Something else". */
export const rowsOf = (q: Question) =>
  q.ask.kind === "choice" ? q.ask.options.length + 1 : 0;

/** The row a step's cursor starts on: its answer's first, else the first. */
function cursorAt(q: Question | undefined, draft: Draft | undefined) {
  if (!q || q.ask.kind !== "choice" || !draft) return 0;
  const options = q.ask.options;
  const label =
    draft.kind === "option"
      ? draft.label
      : draft.kind === "many"
        ? options.find((o) => draft.labels.includes(o.label))?.label
        : undefined;
  const i = options.findIndex((o) => o.label === label);
  return i >= 0 ? i : draft.kind === "own" ? options.length : 0;
}

/** Where the flow starts: the first question, nothing chosen. */
export const startFlow = (): FlowState => ({ step: 0, drafts: {}, cursor: 0 });

/**
 * The flow after one action. A step moves only where it may: Next and
 * Review past a question that can be left (canLeave, canReview), and a
 * choice of one's option chosen with `advance` goes on as Next does. An
 * action that doesn't apply to the question shown changes nothing.
 *
 * @example
 * const s = reduceFlow([q1, q2], startFlow(), { type: "choose", label: "A", advance: true });
 * s.step; // 1
 * s.drafts.q1; // { kind: "option", label: "A" }
 */
export function reduceFlow(
  questions: readonly Question[],
  s: FlowState,
  action: FlowAction,
): FlowState {
  const n = questions.length,
    q = questions[s.step],
    draft = q ? s.drafts[q.id] : undefined;
  const at = (step: number, drafts = s.drafts): FlowState => {
    const to = Math.max(0, Math.min(n, step));
    return to === s.step && drafts === s.drafts
      ? s
      : {
          step: to,
          drafts,
          cursor:
            to === s.step
              ? s.cursor
              : cursorAt(questions[to], drafts[questions[to]?.id ?? ""]),
        };
  };
  const set = (d: Draft | undefined) => {
    const drafts = { ...s.drafts };
    if (d) drafts[q!.id] = d;
    else delete drafts[q!.id];
    return drafts;
  };
  // On from a question that can be left: to the next, or, from the last,
  // to Review, else back to the first question that can't be left yet.
  const next = (drafts: FlowState["drafts"]) => {
    if (!q || !canLeave(q, drafts[q.id]))
      return drafts === s.drafts ? s : { ...s, drafts };
    if (s.step + 1 < n || canReview(questions, drafts))
      return at(s.step + 1, drafts);
    return at(
      questions.findIndex((x) => !canLeave(x, drafts[x.id])),
      drafts,
    );
  };
  switch (action.type) {
    case "choose": {
      if (q?.ask.kind !== "choice" || q.ask.multiple) return s;
      const i = q.ask.options.findIndex((o) => o.label === action.label);
      if (i < 0) return s;
      const drafts = set({ kind: "option", label: action.label });
      return action.advance ? next(drafts) : { ...s, drafts, cursor: i };
    }
    case "toggle": {
      if (q?.ask.kind !== "choice" || !q.ask.multiple) return s;
      const i = q.ask.options.findIndex((o) => o.label === action.label);
      if (i < 0) return s;
      const was = draft?.kind === "many" ? draft : null,
        labels = was?.labels.includes(action.label)
          ? was.labels.filter((l) => l !== action.label)
          : [...(was?.labels ?? []), action.label],
        own = was?.own ?? null;
      return {
        ...s,
        cursor: i,
        drafts: set(
          labels.length || own !== null
            ? { kind: "many", labels, own }
            : undefined,
        ),
      };
    }
    case "own": {
      if (q?.ask.kind !== "choice") return s;
      const cursor = q.ask.options.length;
      if (!q.ask.multiple)
        return {
          ...s,
          cursor,
          drafts:
            draft?.kind !== "own"
              ? set({ kind: "own", text: "" })
              : draft.text.trim()
                ? s.drafts
                : set(undefined),
        };
      const was = draft?.kind === "many" ? draft : null,
        labels = was?.labels ?? [];
      return {
        ...s,
        cursor,
        drafts: set(
          was?.own != null
            ? labels.length
              ? { kind: "many", labels, own: null }
              : undefined
            : { kind: "many", labels, own: was?.own ?? "" },
        ),
      };
    }
    case "write": {
      if (!q) return s;
      if (q.ask.kind === "free")
        return {
          ...s,
          drafts: set(
            action.text ? { kind: "own", text: action.text } : undefined,
          ),
        };
      if (!q.ask.multiple)
        return draft?.kind === "own"
          ? { ...s, drafts: set({ kind: "own", text: action.text }) }
          : s;
      return draft?.kind === "many" && draft.own !== null
        ? { ...s, drafts: set({ ...draft, own: action.text }) }
        : s;
    }
    case "clear":
      return q && !q.blocking && draft
        ? { ...s, drafts: set(undefined), cursor: 0 }
        : s;
    case "next":
      return next(s.drafts);
    case "back":
      return at(s.step - 1);
    case "go":
      return Number.isInteger(action.step) &&
        action.step >= 0 &&
        (action.step < n || canReview(questions, s.drafts))
        ? at(action.step)
        : s;
    case "rest":
      return canLeaveRest(questions, s.drafts, s.step) ? at(n) : s;
    case "cursor": {
      const rows = q ? rowsOf(q) : 0;
      return rows ? { ...s, cursor: ((action.to % rows) + rows) % rows } : s;
    }
  }
}

/**
 * What a key does in a choice's list, or null for a key it leaves alone:
 * the arrows (and Home, End) move between rows; Space chooses (one) or
 * ticks (several) the row, or opens "Something else"; Enter chooses and
 * goes on (one), goes on (several), or opens "Something else"; a digit
 * picks that row as a click would.
 *
 * @example
 * keyAction(q1, { step: 0, drafts: {}, cursor: 0 }, "ArrowDown"); // { type: "cursor", to: 1 }
 * keyAction(q1, { step: 0, drafts: {}, cursor: 0 }, "Enter"); // { type: "choose", label: "A", advance: true }
 */
export function keyAction(
  q: Question,
  s: FlowState,
  key: string,
): FlowAction | null {
  if (q.ask.kind !== "choice") return null;
  const { options, multiple } = q.ask,
    rows = options.length + 1;
  const pick = (row: number, enter: boolean): FlowAction => {
    const o = options[row];
    if (!o) return { type: "own" };
    if (multiple)
      return enter ? { type: "next" } : { type: "toggle", label: o.label };
    return { type: "choose", label: o.label, advance: enter };
  };
  switch (key) {
    case "ArrowDown":
    case "ArrowRight":
      return { type: "cursor", to: s.cursor + 1 };
    case "ArrowUp":
    case "ArrowLeft":
      return { type: "cursor", to: s.cursor - 1 };
    case "Home":
      return { type: "cursor", to: 0 };
    case "End":
      return { type: "cursor", to: rows - 1 };
    case " ":
      return pick(s.cursor, false);
    case "Enter":
      return pick(s.cursor, true);
  }
  const digit = /^[1-9]$/.test(key) ? Number(key) - 1 : -1;
  if (digit < 0 || digit >= rows) return null;
  // A digit picks as a click does: a choice of one chooses and goes on.
  return digit === options.length
    ? { type: "own" }
    : multiple
      ? { type: "toggle", label: options[digit]!.label }
      : { type: "choose", label: options[digit]!.label, advance: true };
}

/**
 * What the flow sends once at Review: the answers given, in the questions'
 * order, and the ids left to the agent; undefined while a question can't
 * be left (canReview).
 */
export function answersOf(
  questions: readonly Question[],
  drafts: FlowState["drafts"],
): { answers: AgentQuestionAnswer[]; left: string[] } | undefined {
  if (!canReview(questions, drafts)) return undefined;
  const answers: AgentQuestionAnswer[] = [],
    left: string[] = [];
  for (const q of questions) {
    const c = check(q, drafts[q.id]);
    if (c.kind === "answered") answers.push(c.answer);
    else left.push(q.id);
  }
  return { answers, left };
}

/**
 * An answer in words, for the strip, Review and summaries: the options
 * chosen, then the person's own words, quoted.
 *
 * @example
 * answerText({ options: ["Monthly", "Yearly"], text: "and quarterly" }); // "Monthly, Yearly, and “and quarterly”"
 * answerText({ options: ["Monthly"], text: "yearly" }); // "Monthly and “yearly”"
 * answerText({ options: [], text: "60 days" }); // "“60 days”"
 */
export function answerText(a: { options?: readonly string[]; text?: string }) {
  const parts = [...(a.options ?? [])];
  if (a.text) parts.push(`“${a.text}”`);
  if (parts.length < 3) return parts.join(" and ");
  return `${parts.slice(0, -1).join(", ")}, and ${parts.at(-1)}`;
}

/** What a question left to `agent` takes, in words. */
export const leftText = (q: Question, agent = "the agent") => {
  const left = `Left to ${agent}`;
  if (q.ask.kind === "choice")
    return q.ask.recommended
      ? `${left}: it recommends ${q.ask.recommended}`
      : left;
  return q.ask.suggested ? `${left}: it suggests “${q.ask.suggested}”` : left;
};

/** A question's short name: its header, else "Question N". */
export const questionName = (q: Question, i: number) =>
  q.header || `Question ${i + 1}`;

/** Where a tab keeps a half-answered round (sessionStorage). */
export const flowKey = (key: string) => `foundry.questions.v1:${key}`;

/**
 * A kept flow, read back without trust: drafts for questions of this
 * round that fit them (options it offers, the kind it takes), and the step,
 * if it may still be shown; the start for anything else.
 *
 * @example
 * restoreFlow([q1], '{"step":0,"drafts":{"q1":{"kind":"option","label":"A"}}}').drafts; // { q1: { kind: "option", label: "A" } }
 * restoreFlow([q1], "not json"); // the start
 */
export function restoreFlow(
  questions: readonly Question[],
  raw: string | null,
): FlowState {
  let x: unknown;
  try {
    x = JSON.parse(raw || "null");
  } catch {
    return startFlow();
  }
  if (!isRecord(x)) return startFlow();
  const drafts: Record<string, Draft> = {};
  const kept = isRecord(x.drafts) ? x.drafts : {};
  for (const q of questions) {
    const d = kept[q.id];
    if (!isRecord(d)) continue;
    const draft: Draft | undefined =
      d.kind === "option" && typeof d.label === "string"
        ? { kind: "option", label: d.label }
        : d.kind === "own" && typeof d.text === "string"
          ? { kind: "own", text: d.text }
          : d.kind === "many" &&
              Array.isArray(d.labels) &&
              d.labels.every((l) => typeof l === "string") &&
              (d.own === null || typeof d.own === "string")
            ? {
                kind: "many",
                labels: [...new Set(d.labels as string[])],
                own: d.own as string | null,
              }
            : undefined;
    const fits =
      draft &&
      (draft.kind === "own"
        ? q.ask.kind === "free" || !q.ask.multiple
        : q.ask.kind === "choice" &&
          q.ask.multiple === (draft.kind === "many") &&
          (draft.kind === "option" ? [draft.label] : draft.labels).every((l) =>
            (q.ask as { options: QuestionOption[] }).options.some(
              (o) => o.label === l,
            ),
          ));
    if (fits) drafts[q.id] = draft;
  }
  const step =
    typeof x.step === "number" &&
    Number.isInteger(x.step) &&
    x.step >= 0 &&
    x.step <= questions.length &&
    (x.step < questions.length || canReview(questions, drafts))
      ? x.step
      : 0;
  return {
    step,
    drafts,
    cursor: cursorAt(questions[step], drafts[questions[step]?.id ?? ""]),
  };
}
