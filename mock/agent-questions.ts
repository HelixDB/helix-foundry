import { isRecord } from "../src/shared/json";
import type { AgentQuestion, AgentQuestionAnswer } from "../src/shared/wfe";

// The backend's agentquestion package: a question an agent asks the person
// (a generation's, the Assistant's) and the person's answers, checked as
// Check checks them, with its words for what is wrong. Both the ontology
// routes and the Assistant's take answers through it.

/** At most this many characters of the person's own words, once trimmed. */
export const MAX_ANSWER_CHARS = 2000;

const chars = (s: string) => [...s].length;

/**
 * An answer as Check returns it: the person's, or, with `useDefaults`, a
 * question left to the agent (agentquestion.Answer's Left), which takes its
 * recommended answer, or decides, as its own choice.
 */
export type CheckedAnswer =
  | AgentQuestionAnswer
  | { questionId: string; left: true };

/** One answer to q, as agentquestion's checkOne: the answer as kept, or why not. */
function checkOne(
  q: AgentQuestion,
  a: { options: string[]; text: string },
): AgentQuestionAnswer | string {
  const text = a.text.trim();
  if (a.text !== "" && (!text || chars(text) > MAX_ANSWER_CHARS))
    return `the words answering question ${q.id} are 1 to ${MAX_ANSWER_CHARS} characters`;
  const options = q.options || [];
  if (!options.length) {
    if (a.options.length || !text)
      return `question ${q.id} takes an answer in words, and no options`;
    return { questionId: q.id, options: [], text };
  }
  const chosen: string[] = [];
  for (const label of a.options) {
    if (!options.some((o) => o.label === label))
      return `question ${q.id} offers no option ${JSON.stringify(label)}`;
    if (chosen.includes(label))
      return `question ${q.id}'s option ${JSON.stringify(label)} is chosen twice`;
    chosen.push(label);
  }
  const index = (l: string) => options.findIndex((o) => o.label === l);
  chosen.sort((x, y) => index(x) - index(y));
  if (!chosen.length && !text)
    return `question ${q.id} needs an option chosen or an answer in words`;
  if (!q.multiple && chosen.length > 1)
    return `question ${q.id} takes one option, not ${chosen.length}`;
  if (!q.multiple && chosen.length === 1 && text)
    return `question ${q.id} takes one option or an answer in words, not both`;
  return { questionId: q.id, options: chosen, ...(text && { text }) };
}

/**
 * The person's answers to `asked`, as agentquestion.Check takes them, in
 * the order asked: each names a question asked, once; with `useDefaults`
 * every question left out is left to the agent, without every blocking one
 * must be answered. Given as the REST body has them (camelCase), or as an
 * MCP payload (snake_case `question_id`). A string says what is wrong.
 */
export function checkAnswers(
  asked: readonly AgentQuestion[],
  given: unknown,
  useDefaults: boolean,
): CheckedAnswer[] | string {
  if (!Array.isArray(given))
    return "answers is a list of {questionId, options, text}";
  const byId = new Map<string, AgentQuestionAnswer>();
  for (const raw of given) {
    if (!isRecord(raw))
      return "answers is a list of {questionId, options, text}";
    const qid = String(raw.questionId ?? raw.question_id ?? ""),
      options = raw.options ?? [],
      text = raw.text ?? "";
    if (
      !Array.isArray(options) ||
      options.some((o) => typeof o !== "string") ||
      typeof text !== "string"
    )
      return "an answer's options are labels, and its text a string";
    const q = asked.find((x) => x.id === qid);
    if (!q) return `no question ${JSON.stringify(qid)} was asked`;
    if (byId.has(qid)) return `question ${qid} is answered twice`;
    const checked = checkOne(q, { options: options as string[], text });
    if (typeof checked === "string") return checked;
    byId.set(qid, checked);
  }
  const out: CheckedAnswer[] = [];
  for (const q of asked) {
    const a = byId.get(q.id);
    if (a) out.push(a);
    else if (useDefaults) out.push({ questionId: q.id, left: true });
    else if (q.blocking) return `question ${q.id} needs an answer`;
  }
  return out;
}

/** An answer in words, as agentquestion.Words writes it for an agent. */
export const answerWords = (a: AgentQuestionAnswer) =>
  [
    ...(a.options || []),
    ...(a.text ? [`in their words: ${JSON.stringify(a.text)}`] : []),
  ].join("; ");
