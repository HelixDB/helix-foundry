import "./ask.css";
import { useEffect, useRef } from "react";
import { useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ApiError, sentence } from "../../api";
import { Button, ButtonLink, SkeletonText } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { readAccess } from "../workspace/access-model";
import { QuestionCard } from "./ask";
import { withLoad } from "./model";
import { ontologyQueries, questionQueries } from "./queries";
import { InOntology } from "./scope";

/**
 * One Analyst question as /ask shows it, anywhere in the app (Founder's
 * dock, under the step that asked it): its steps, answer, citations and
 * visuals, without what was asked or the avatar, which the conversation
 * shows, then Open the full answer: its thread on Founder's page, read only
 * (founder-page.tsx `?thread=`). Read with GET of one, which
 * keeps the queries' outputs, and polled while it is live. `onNavigate`
 * runs whenever a link inside it changes the page (a citation, a source, a
 * chart's bar), so a phone's drawer closes. Default export: loaded lazily.
 */
export default function InlineQuestion({
  ontologyId,
  questionId,
  onNavigate,
}: {
  ontologyId: string;
  questionId: string;
  onNavigate?: () => void;
}) {
  const me = useWorkspace(),
    ws = me.id;
  const location = useLocation(),
    at = useRef(location.key);
  useEffect(() => {
    if (at.current === location.key) return;
    at.current = location.key;
    onNavigate?.();
  }, [location.key]);
  const ontology = useQuery(ontologyQueries.ontology(ws, ontologyId));
  const o = ontology.data?.ontology;
  const shut = !!o && readAccess(me, o) === "none";
  const question = useQuery({
    ...questionQueries.question(ws, ontologyId, questionId),
    enabled: !!o && !shut,
  });
  const q = question.data?.question;
  const failed = ontology.error ?? question.error,
    gone = failed instanceof ApiError && failed.status === 404;
  if (shut)
    return (
      <div className="ak-inline">
        <p className="ak-inline-note">
          You can’t read this ontology’s answers.
        </p>
      </div>
    );
  if (failed)
    return (
      <div className="ak-inline">
        <p className="ak-inline-note" role="alert">
          {gone
            ? "This question is gone."
            : `This answer couldn’t be read. ${sentence(failed.message)}`}
          {!gone && (
            <Button
              size="sm"
              variant="ghost"
              onClick={() =>
                void (ontology.error ? ontology.refetch() : question.refetch())
              }
            >
              Retry
            </Button>
          )}
        </p>
      </div>
    );
  if (!o || !q)
    return (
      <div className="ak-inline" aria-busy>
        <SkeletonText lines={3} />
      </div>
    );
  return (
    <div className="ak-inline">
      <InOntology ontology={o}>
        <QuestionCard question={q} asked={false} avatar={false} />
      </InOntology>
      <ButtonLink
        size="sm"
        className="ak-open"
        to={withLoad(
          paths.ask(ontologyId, { thread: q.threadId || q.id }),
          o.currentLoad?.id === q.loadId ? null : q.loadId || null,
        )}
      >
        Open the full answer
      </ButtonLink>
    </div>
  );
}
