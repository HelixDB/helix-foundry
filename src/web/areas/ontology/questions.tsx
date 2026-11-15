import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { CircleHelp, X } from "lucide-react";
import type { OntologyGeneration } from "../../../shared/wfe";
import { sentence } from "../../api";
import {
  Button,
  ConfirmDialog,
  Panel,
  formatDate,
  formatRelative,
} from "../../kit";
import { canManage, useWorkspace } from "../../ui";
import {
  AnswerSummary,
  QuestionFlow,
  QuestionList,
} from "../../questions/flow";
import {
  answerText,
  leftText,
  questionName,
  readQuestion,
  readQuestions,
} from "../../questions/model";
import {
  AiRefusal,
  SettingsLinked,
  useAiConfigured,
} from "../workspace/ai-gate";
import { answerGeneration, cancelGeneration } from "./queries";
import { useOntology } from "./scope";

// The questions a generation waits on (status needs_input): the agent asked
// what only a person can answer, and drafts on once they are answered. An
// owner or admin answers them one at a time (QuestionFlow); a question left
// to the agent (useDefaults) is its to decide, as its own choice, taking
// its recommended answer. Cancel ends the generation without a draft. Members read the
// questions. The agent's words may quote the data: plain text only.

export function GenerationQuestions({
  generation: g,
}: {
  generation: OntologyGeneration;
}) {
  const { id, role } = useWorkspace(),
    { id: o } = useOntology(),
    qc = useQueryClient();
  const questions = readQuestions(g.questions),
    round = g.questionRound || 1;
  const [pending, setPending] = useState<"answers" | "cancel" | "">(""),
    [cancelling, setCancelling] = useState(false),
    // What the last answer or cancel failed with; a cancel's shows in its
    // dialog, which stays open.
    [error, setError] = useState<{
      kind: "answer" | "cancel";
      error: unknown;
    } | null>(null);
  const aiReady = useAiConfigured(),
    manage = canManage(role);
  // Pending until the generation is read again: answered, it is queued and
  // polled (and these questions go); cancelled, it ended. Re-enabled
  // before, a second click would be refused.
  const run = async (
    kind: "answers" | "cancel",
    call: () => Promise<unknown>,
  ) => {
    setPending(kind);
    setError(null);
    let ok = false;
    try {
      await call();
      ok = true;
      setCancelling(false);
    } catch (e) {
      setError({ kind: kind === "cancel" ? "cancel" : "answer", error: e });
    }
    await qc.invalidateQueries({ queryKey: [id, "ontology", o] });
    setPending("");
    return ok;
  };
  const answerError = error?.kind === "answer" ? error.error : null,
    cancelError = error?.kind === "cancel" ? error.error : null;
  const due = g.answersDueAt
    ? ` If no one answers by ${formatDate(g.answersDueAt)}, it takes its recommended answers.`
    : "";
  const count = g.questions?.length || 0;
  return (
    <Panel
      className="on-asked"
      title={
        <span className="on-asked-title">
          <CircleHelp size={16} aria-hidden />
          The agent has {count === 1 ? "a question" : `${count} questions`}{" "}
          before it drafts
        </span>
      }
      subtitle={`${g.askedAt ? `Asked ${formatRelative(g.askedAt)}. ` : ""}It drafts on from your answers.${due}`}
      actions={
        manage && (
          <Button
            variant="ghost"
            size="sm"
            icon={X}
            disabled={!!pending}
            onClick={() => setCancelling(true)}
          >
            Cancel generation
          </Button>
        )
      }
    >
      {!questions ? (
        <p className="on-muted">
          These questions can’t be shown here. Reload to update Foundry.
        </p>
      ) : manage ? (
        <AiRefusal
          action="Answering the agent"
          error={answerError}
          ready={aiReady}
        >
          <QuestionFlow
            // A new round starts afresh.
            key={round}
            questions={questions}
            storageKey={`generation:${g.id}:${round}`}
            pending={pending === "answers"}
            disabled={!!pending}
            error={
              !!answerError && (
                <p className="hf-form-error" role="alert">
                  <SettingsLinked
                    text={sentence((answerError as Error).message)}
                  />
                </p>
              )
            }
            onSend={(answers, left) =>
              run("answers", () =>
                answerGeneration(id, o, g.id, {
                  answers,
                  round,
                  ...(left.length > 0 && { useDefaults: true }),
                }),
              )
            }
          />
        </AiRefusal>
      ) : (
        <>
          <QuestionList questions={questions} />
          <p className="on-muted">
            An owner or admin of this workspace answers them.
          </p>
        </>
      )}
      {cancelling && (
        <ConfirmDialog
          title="Cancel this generation?"
          description="Nothing is drafted, and the agent’s questions go unanswered. You can generate again."
          confirmLabel="Cancel generation"
          cancelLabel="Keep it"
          tone="danger"
          pending={pending === "cancel"}
          onConfirm={async () => {
            await run("cancel", () => cancelGeneration(id, o, g.id));
          }}
          onCancel={() => {
            setCancelling(false);
            setError(null);
          }}
        >
          {!!cancelError && (
            <p className="hf-form-error" role="alert">
              {sentence((cancelError as Error).message)}
            </p>
          )}
        </ConfirmDialog>
      )}
    </Panel>
  );
}

/** What a generation was asked and answered, once it carried on. */
export function AnsweredQuestions({
  generation: g,
}: {
  generation: OntologyGeneration;
}) {
  const rows = (g.questions || []).flatMap((x, i) => {
    const q = readQuestion(x);
    if (!q || (!x.answer && !x.leftToAgent)) return [];
    return [
      {
        name: questionName(q, i),
        question: q.text,
        ...(x.answer
          ? { answer: answerText(x.answer) }
          : { answer: leftText(q), left: true }),
      },
    ];
  });
  if (!rows.length) return null;
  return (
    <Panel
      title="Your answers"
      subtitle="What the agent asked while it drafted, and what it was told."
    >
      <AnswerSummary rows={rows} label="Your answers" />
    </Panel>
  );
}
