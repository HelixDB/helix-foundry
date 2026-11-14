import { useId, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { CircleHelp, Send, X } from "lucide-react";
import type {
  AnswerOntologyGenerationRequest,
  OntologyGeneration,
  OntologyGenerationQuestion,
} from "../../../shared/wfe";
import { sentence } from "../../api";
import {
  Button,
  ConfirmDialog,
  Panel,
  Tag,
  formatDate,
  formatNumber,
  formatRelative,
} from "../../kit";
import { canManage, useWorkspace } from "../../ui";
import {
  AiRefusal,
  SettingsLinked,
  useAiConfigured,
} from "../workspace/ai-gate";
import {
  MAX_ANSWER,
  answersRequest,
  startingAnswer,
  type QuestionAnswer,
} from "./model";
import { answerGeneration, cancelGeneration } from "./queries";
import { useOntology } from "./scope";

// The questions a generation waits on (status needs_input): the agent asked
// what only a person can answer, and drafts on once they are answered. Each
// is answered with one of its options (the agent's default chosen to start),
// in the person's own words, or left to the agent (answersRequest); Use the
// agent's defaults answers them all that way, and Cancel ends the
// generation without a draft. Questions and options are the model's words
// and may quote the data: React renders them as text, never markup.
// Members see the questions, without the form.

/** "Customer (type)" for the draft item "type.Customer". */
const itemText = (item: string) => {
  const [kind, ...rest] = item.split(".");
  return rest.length ? `${rest.join(".")} (${kind})` : item;
};

export function GenerationQuestions({
  generation: g,
}: {
  generation: OntologyGeneration;
}) {
  const { id, role } = useWorkspace(),
    { id: o } = useOntology(),
    qc = useQueryClient();
  const questions = g.questions || [];
  const [answers, setAnswers] = useState<Record<string, QuestionAnswer>>({}),
    [pending, setPending] = useState<"answers" | "defaults" | "cancel" | "">(
      "",
    ),
    [cancelling, setCancelling] = useState(false),
    [error, setError] = useState<unknown>(null);
  const aiReady = useAiConfigured(),
    manage = canManage(role);
  const checked = answersRequest(questions, answers);
  const run = async (
    kind: "answers" | "defaults" | "cancel",
    call: () => Promise<unknown>,
  ) => {
    setPending(kind);
    setError(null);
    try {
      await call();
      setCancelling(false);
    } catch (e) {
      setError(e);
    } finally {
      setPending("");
      // Answered, it is queued again and polled; cancelled, it ended.
      await qc.invalidateQueries({ queryKey: [id, "ontology", o] });
    }
  };
  const send = (
    kind: "answers" | "defaults",
    body: AnswerOntologyGenerationRequest,
  ) => run(kind, () => answerGeneration(id, o, g.id, body));
  const due = g.answersDueAt
    ? ` If no one answers by ${formatDate(g.answersDueAt)}, it takes its defaults.`
    : "";
  return (
    <Panel
      className="on-asked"
      title={
        <span className="on-asked-title">
          <CircleHelp size={16} aria-hidden />
          The agent has {questions.length === 1
            ? "a question"
            : "questions"}{" "}
          before it drafts
        </span>
      }
      subtitle={`${g.askedAt ? `Asked ${formatRelative(g.askedAt)}. ` : ""}It drafts on from your answers.${due}`}
    >
      <ol className="on-questions">
        {questions.map((q) => (
          <AskedQuestion
            key={q.id}
            q={q}
            answer={answers[q.id] ?? startingAnswer(q)}
            setAnswer={
              manage
                ? (a) => setAnswers((all) => ({ ...all, [q.id]: a }))
                : undefined
            }
            problem={
              !checked.ok && checked.questionId === q.id
                ? checked.reason
                : undefined
            }
          />
        ))}
      </ol>
      {manage ? (
        <div className="on-generate-foot">
          <AiRefusal action="Answering the agent" error={error} ready={aiReady}>
            {!!error && (
              <p className="hf-form-error" role="alert">
                <SettingsLinked text={sentence((error as Error).message)} />
              </p>
            )}
          </AiRefusal>
          <div className="hf-form-actions">
            <Button
              variant="ghost"
              icon={X}
              disabled={!!pending}
              onClick={() => setCancelling(true)}
            >
              Cancel generation
            </Button>
            <Button
              pending={pending === "defaults"}
              disabled={!!pending || aiReady === false}
              onClick={() =>
                void send("defaults", { answers: [], useDefaults: true })
              }
            >
              Use the agent’s defaults
            </Button>
            <Button
              variant="primary"
              icon={Send}
              pending={pending === "answers"}
              disabled={!checked.ok || !!pending || aiReady === false}
              onClick={() =>
                checked.ok && void send("answers", checked.request)
              }
            >
              Send answers
            </Button>
          </div>
        </div>
      ) : (
        <p className="on-muted">
          An owner or admin of this workspace answers them.
        </p>
      )}
      {cancelling && (
        <ConfirmDialog
          title="Cancel this generation?"
          description="Nothing is drafted, and the agent’s questions go unanswered. You can generate again."
          confirmLabel="Cancel generation"
          cancelLabel="Keep it"
          tone="danger"
          pending={pending === "cancel"}
          onConfirm={() => run("cancel", () => cancelGeneration(id, o, g.id))}
          onCancel={() => setCancelling(false)}
        />
      )}
    </Panel>
  );
}

/**
 * One question: its text, what it is about, and, for those who answer, a
 * radio per option, In my own words (a field once chosen) and, where the
 * agent's default isn't one of the options, the agent's default or Leave it
 * to the agent.
 */
function AskedQuestion({
  q,
  answer,
  setAnswer,
  problem,
}: {
  q: OntologyGenerationQuestion;
  answer: QuestionAnswer;
  // Unset for those who may not answer.
  setAnswer?: (a: QuestionAnswer) => void;
  problem?: string;
}) {
  const name = useId(),
    options = q.options || [],
    // The agent's own choice is an option of its own unless it is one.
    agentChoice = !(q.defaultAnswer && options.includes(q.defaultAnswer)),
    // The text written stays while another choice is picked.
    [own, setOwn] = useState(answer.kind === "own" ? answer.text : "");
  const head = (
    <div className="on-question-head">
      {q.blocking ? (
        <Tag tone="warning">Needs an answer</Tag>
      ) : (
        <Tag>Optional</Tag>
      )}
      {(q.items?.length || 0) > 0 && (
        <span className="on-muted">
          About {q.items!.map(itemText).join(", ")}
        </span>
      )}
    </div>
  );
  if (!setAnswer)
    return (
      <li className="on-question">
        {head}
        <p>{q.text}</p>
        {options.length > 0 && (
          <ul className="on-options" aria-label="Options">
            {options.map((o) => (
              <li
                key={o}
                className={o === q.defaultAnswer ? "is-default" : undefined}
              >
                {o}
                {o === q.defaultAnswer && (
                  <Tag tone="accent">Agent’s default</Tag>
                )}
              </li>
            ))}
          </ul>
        )}
        {!options.length && q.defaultAnswer && (
          <p className="on-muted">Agent’s default: {q.defaultAnswer}</p>
        )}
      </li>
    );
  const chars = [...own.trim()].length;
  return (
    <li className="on-question">
      {head}
      <p id={`${name}-text`}>{q.text}</p>
      <div
        className="on-answer"
        role="radiogroup"
        aria-labelledby={`${name}-text`}
      >
        {options.map((o) => (
          <label key={o} className="on-check on-answer-choice">
            <input
              type="radio"
              name={name}
              checked={answer.kind === "option" && answer.option === o}
              onChange={() => setAnswer({ kind: "option", option: o })}
            />
            <span>{o}</span>
            {o === q.defaultAnswer && <Tag tone="accent">Agent’s default</Tag>}
          </label>
        ))}
        <label className="on-check on-answer-choice">
          <input
            type="radio"
            name={name}
            checked={answer.kind === "own"}
            onChange={() => setAnswer({ kind: "own", text: own })}
          />
          <span>In my own words</span>
        </label>
        {agentChoice && (
          <label className="on-check on-answer-choice">
            <input
              type="radio"
              name={name}
              checked={answer.kind === "agent"}
              onChange={() => setAnswer({ kind: "agent" })}
            />
            <span>
              {q.defaultAnswer
                ? `The agent’s default: ${q.defaultAnswer}`
                : "Leave it to the agent"}
            </span>
          </label>
        )}
      </div>
      {answer.kind === "own" && (
        <label className="hf-field on-answer-own">
          <span className="hf-sr-only">Your answer</span>
          <textarea
            rows={3}
            value={own}
            aria-invalid={!!problem}
            aria-describedby={`${name}-hint`}
            onChange={(e) => {
              setOwn(e.target.value);
              setAnswer({ kind: "own", text: e.target.value });
            }}
          />
          <small id={`${name}-hint`}>
            {problem ??
              `${formatNumber(Math.max(0, MAX_ANSWER - chars))} characters left.`}
          </small>
        </label>
      )}
    </li>
  );
}

/** What a generation was asked and answered, once it carried on. */
export function AnsweredQuestions({
  generation: g,
}: {
  generation: OntologyGeneration;
}) {
  const answered = (g.questions || []).filter(
    (q) => typeof q.answer === "string",
  );
  if (!answered.length) return null;
  return (
    <Panel
      title="Your answers"
      subtitle="What the agent asked while it drafted, and what it was told."
    >
      <ol className="on-questions">
        {answered.map((q) => (
          <li key={q.id} className="on-question">
            <p>{q.text}</p>
            <p className="on-muted">Answer: {q.answer}</p>
          </li>
        ))}
      </ol>
    </Panel>
  );
}
