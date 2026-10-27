import { Fragment, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  FoundryLoad,
  FoundryReviewChoice,
  ReviewDecision,
} from "../../../shared/wfe";
import { Button, ButtonLink, Select } from "../../kit";
import { paths } from "../../paths";
import { ApiError } from "../../api";
import { useWorkspace } from "../../ui";
import { canManage } from "../ontology/parts";
import { useOntology } from "../ontology/scope";
import { valueText } from "../explore/model";
import { deleteReviewDecision, reviewQueries, settleReview } from "./api";
import {
  decisionFor,
  loadRead,
  matchesReview,
  propValueText,
  type Candidate,
  type Settleable,
} from "./model";

/** A review's candidates, each by name and linked to its page when the read found it. */
function Candidates({
  candidates,
  link,
}: {
  candidates: Candidate[];
  link: (path: string) => string;
}) {
  const { id: o } = useOntology();
  return candidates.map((c, i) => (
    <Fragment key={c.key}>
      {i > 0 && ", "}
      {c.entity ? (
        <Link to={link(paths.entity(o, c.entity.id))}>{c.entity.name}</Link>
      ) : (
        c.key
      )}
    </Fragment>
  ));
}

/**
 * Saves an override only. A load's graph and evidence are never edited.
 * `load` is the load the page shows; only the current one settles.
 */
export function SettleReview({
  review,
  load,
  link,
}: {
  review: Settleable & { reason: string };
  load?: FoundryLoad;
  link: (path: string) => string;
}) {
  const { id: ws, role } = useWorkspace();
  const { id: o } = useOntology();
  const qc = useQueryClient();
  const query = useQuery(reviewQueries.list(ws, o));
  const [choice, setChoice] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const s = review.settle;
  if (!s || s.kind === "stale")
    return (
      <>
        {review.candidates.length > 0 && (
          <p>
            Suggestions:{" "}
            <Candidates candidates={review.candidates} link={link} />
          </p>
        )}
        {s && canManage(role) && (
          <p className="on-muted">Load again to settle this review.</p>
        )}
      </>
    );
  const isProp = s.kind === "prop";
  const field = !isProp
    ? "Link this name to"
    : s.table !== undefined
      ? "Replace the table’s value with"
      : "Keep value";
  const decision =
    choice === ""
      ? undefined
      : isProp
        ? decisionFor(review, {
            value: s.values.find((v) => JSON.stringify(v.value) === choice)
              ?.value,
          })
        : decisionFor(review, {
            entityKey: choice === "none" ? null : choice.slice(4),
          });
  const editable = !!load?.current;
  const saved = query.data?.find((d) => matchesReview(d, s));
  const save = async (choice: FoundryReviewChoice) => {
    if (pending) return;
    setPending(true);
    setError("");
    try {
      await settleReview(ws, o, s.reviewId, { decision: choice });
      await qc.invalidateQueries({
        queryKey: reviewQueries.list(ws, o).queryKey,
      });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPending(false);
    }
  };
  // A name two candidates share reads with their keys.
  const label = (c: Candidate) => {
    const name = c.entity?.name ?? c.key;
    return review.candidates.some(
      (other) => other !== c && (other.entity?.name ?? other.key) === name,
    )
      ? `${name} (${c.key})`
      : name;
  };
  return (
    <div className="on-stack">
      {!isProp && review.candidates.length > 0 && (
        <p>
          Suggestions only:{" "}
          <Candidates candidates={review.candidates} link={link} />
        </p>
      )}
      {isProp && s.table !== undefined && (
        <p>
          The table’s value, <strong>{valueText(s.table)}</strong>, is kept.
          Documents state another:
        </p>
      )}
      {isProp && (
        <ul className="ex-evidence-list" aria-label="Conflicting values">
          {s.values.map((v, i) => (
            <li key={i}>
              <strong>{propValueText(v.value)}</strong>
              {" · "}
              <Link to={link(paths.document(o, v.documentId, "review", v))}>
                “{v.quote}”
              </Link>
            </li>
          ))}
        </ul>
      )}
      {!editable && (
        <p className="on-muted">Open the current load to settle this review.</p>
      )}
      {saved && (
        <p role="status">
          {!editable
            ? "A decision is saved for this review."
            : loadRead(saved, load?.createdAt)
              ? "This load read the saved decision, and the review is still open: what it chose isn’t in this load. Choose again or remove the decision."
              : "Decision saved. Reload to apply."}
        </p>
      )}
      {canManage(role) && editable && (
        <form
          className="on-stack"
          onSubmit={async (e) => {
            e.preventDefault();
            if (!decision || pending) return;
            await save(decision);
          }}
        >
          <label className="hf-field">
            <span>{field}</span>
            <Select
              label={field}
              value={choice}
              disabled={pending}
              onChange={setChoice}
              options={
                isProp
                  ? [
                      ...new Map(
                        s.values.map((v) => [JSON.stringify(v.value), v]),
                      ).values(),
                    ].map((v) => ({
                      value: JSON.stringify(v.value),
                      label: propValueText(v.value),
                    }))
                  : [
                      ...review.candidates.map((c) => ({
                        value: `key:${c.key}`,
                        label: label(c),
                      })),
                      { value: "none", label: "Not in data" },
                    ]
              }
            />
          </label>
          <p className="on-muted">
            {isProp && s.table !== undefined
              ? "A chosen value replaces the table’s in every later load, even after the table changes. "
              : ""}
            Applies from the next load. Reload to apply.
          </p>
          {(error || query.error) && (
            <p role="alert" className="hf-form-error">
              {error || query.error?.message}
            </p>
          )}
          <div className="hf-form-actions">
            {review.reason === "adjudicated" && (
              <Button
                size="sm"
                disabled={pending}
                pending={pending}
                onClick={() => void save({ resolve: { notInData: true } })}
              >
                Undo link
              </Button>
            )}
            <Button
              type="submit"
              size="sm"
              disabled={!decision || pending}
              pending={pending}
            >
              Settle review
            </Button>
          </div>
        </form>
      )}
    </div>
  );
}

/** Saved overrides stay available to remove after their review disappears. */
export function SavedDecisions({
  documentId,
  entity,
}:
  | { documentId: string; entity?: never }
  | { entity: { type: string; key: string }; documentId?: never }) {
  const { id: ws, role } = useWorkspace(),
    { id: o } = useOntology();
  const qc = useQueryClient();
  const query = useQuery(reviewQueries.list(ws, o));
  const [pending, setPending] = useState("");
  const [notice, setNotice] = useState("");
  const [error, setError] = useState("");
  const relevant = (d: ReviewDecision) =>
    documentId
      ? d.kind === "resolve" && d.docId === documentId
      : entity && d.type === entity.type && d.entityKey === entity.key;
  const own = query.data?.filter((d) => relevant(d)) || [];
  return (
    <div className="on-stack">
      {query.error && (
        <p role="alert" className="on-error-text">
          Couldn’t read saved decisions: {query.error.message}
        </p>
      )}
      {own.length > 0 && (
        <>
          <h3>Saved decisions</h3>
          <ul className="dc-doc-list" aria-label="Saved decisions">
            {own.map((d) => (
              <li key={d.id} className="dc-review">
                <p>
                  {d.type} ·{" "}
                  {d.kind === "resolve"
                    ? `${d.normName} → ${d.entityKey ?? "Not in data"}`
                    : `${d.prop}: ${propValueText(d.value)}`}
                </p>
                {canManage(role) && (
                  <Button
                    size="sm"
                    disabled={!!pending}
                    pending={pending === d.id}
                    onClick={async () => {
                      setPending(d.id);
                      setError("");
                      setNotice("");
                      try {
                        try {
                          await deleteReviewDecision(ws, o, d.id);
                        } catch (e) {
                          if (!(e instanceof ApiError && e.status === 404))
                            throw e;
                        }
                        await qc.invalidateQueries({
                          queryKey: reviewQueries.list(ws, o).queryKey,
                        });
                        setNotice("Decision removed. Reload to apply.");
                      } catch (e) {
                        setError((e as Error).message);
                      } finally {
                        setPending("");
                      }
                    }}
                  >
                    Remove decision
                  </Button>
                )}
              </li>
            ))}
          </ul>
          <p className="on-muted">Decisions apply to future loads.</p>
          <ButtonLink to={paths.loads(o)} size="sm">
            Loads
          </ButtonLink>
        </>
      )}
      {notice && <p role="status">{notice}</p>}
      {error && (
        <p role="alert" className="on-error-text">
          {error}
        </p>
      )}
    </div>
  );
}
