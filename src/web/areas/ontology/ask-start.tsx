import "./ask.css";
import "./ontology.css";
import { Suspense, lazy, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CornerDownRight, LoaderCircle } from "lucide-react";
import type { FoundryLoad } from "../../../shared/wfe";
import { sentence } from "../../api";
import { trackEvent } from "../../analytics";
import { ButtonLink, SkeletonText } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { recordQuestionAsked } from "../../shell/star-prompt";
import { useReadAccess } from "../explore/graph-reader";
import {
  AiRefusal,
  SettingsLinked,
  useAiConfigured,
} from "../workspace/ai-gate";
import { startQuestions } from "./ask-model";
import { withLoad } from "./model";
import { askQuestion, ontologyQueries } from "./queries";
import { useOntology } from "./scope";

const InlineQuestion = lazy(() => import("./ask-inline"));

// Once a load is ready, asking about it is one click: the setup's Explore or
// ask step and the current load's page offer the questions its version was
// drafted to answer (or, without any, a few its schema answers:
// startQuestions), each asked about the load at once and answered here
// (./ask-inline.tsx), with Ask your own question beside them. Who can't read
// the load's database gets only the links.

type Asking =
  | { kind: "idle" }
  | { kind: "asking"; question: string }
  | { kind: "failed"; error: Error };

export function AskStart({
  load,
  where,
  asked,
  onAsked,
}: {
  load: Pick<FoundryLoad, "id" | "versionId" | "status" | "current">;
  where: "setup" | "load";
  // The question asked from here, once one is: the caller keeps it, so it
  // stays shown when the setup it was asked from ends.
  asked: string | null;
  onAsked: (questionId: string) => void;
}) {
  const { id: ws } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    access = useReadAccess(),
    qc = useQueryClient(),
    aiReady = useAiConfigured();
  const [asking, setAsking] = useState<Asking>({ kind: "idle" });
  const version = useQuery({
    ...ontologyQueries.version(ws, o, load.versionId || ""),
    enabled: !!load.versionId,
  });
  const start = startQuestions(version.data?.version);
  // Through grants, Ask reads the load; with neither, nothing is asked here.
  const canAsk =
    (load.status === "ready" || load.status === "partial") &&
    access !== "none" &&
    (access !== "database" || !!ontology.tenantId);
  const ask = async (question: string) => {
    if (!start) return;
    setAsking({ kind: "asking", question });
    try {
      const r = await askQuestion(ws, o, {
        loadId: load.id,
        question,
        clientToken: crypto.randomUUID(),
      });
      trackEvent("start_question_asked", { where, source: start.source });
      void recordQuestionAsked();
      void qc.invalidateQueries({ queryKey: [ws, "ontology", o, "questions"] });
      setAsking({ kind: "idle" });
      onAsked(r.question.id);
    } catch (e) {
      setAsking({ kind: "failed", error: e as Error });
    }
  };
  const loadOf = load.current ? null : load.id;
  return (
    <section className="on-ask-start" aria-label="Ask about your data">
      {canAsk && (
        <>
          <p className="on-muted">
            {start?.source === "schema"
              ? "A few questions any load of this ontology answers. One click asks, and the answer shows here."
              : "The questions this version was drafted to answer. One click asks, and the answer shows here."}
          </p>
          {start ? (
            <div
              className="ak-suggestions"
              role="group"
              aria-label="Suggested questions"
            >
              {start.questions.map((q) => (
                <button
                  key={q}
                  type="button"
                  className="ak-suggestion"
                  disabled={asking.kind === "asking" || aiReady === false}
                  onClick={() => void ask(q)}
                >
                  {asking.kind === "asking" && asking.question === q ? (
                    <LoaderCircle size={14} className="hf-spin" aria-hidden />
                  ) : (
                    <CornerDownRight size={14} aria-hidden />
                  )}
                  <span>{q}</span>
                </button>
              ))}
            </div>
          ) : (
            version.isPending && !!load.versionId && <SkeletonText lines={2} />
          )}
          <AiRefusal
            action="Asking questions"
            error={asking.kind === "failed" ? asking.error : null}
            ready={aiReady}
          >
            {asking.kind === "failed" && (
              <p className="hf-form-error" role="alert">
                <SettingsLinked text={sentence(asking.error.message)} />
              </p>
            )}
          </AiRefusal>
          {asked && (
            <Suspense fallback={<SkeletonText lines={3} />}>
              <InlineQuestion ontologyId={o} questionId={asked} />
            </Suspense>
          )}
        </>
      )}
      <span className="on-card-actions">
        <ButtonLink
          size="sm"
          variant={canAsk ? "secondary" : "primary"}
          to={withLoad(paths.ask(o), loadOf)}
        >
          Ask your own question
        </ButtonLink>
        {where === "setup" && (
          <ButtonLink size="sm" to={withLoad(paths.explore(o), loadOf)}>
            Open the Explorer
          </ButtonLink>
        )}
      </span>
    </section>
  );
}
