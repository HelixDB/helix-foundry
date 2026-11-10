import { Fragment, useState } from "react";
import { Link } from "react-router-dom";
import { skipToken, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  FoundryLoad,
  FoundryReviewChoice,
  ReviewDecision,
} from "../../../shared/wfe";
import { Button, ButtonLink, Select } from "../../kit";
import { paths } from "../../paths";
import { ApiError } from "../../api";
import { canManage, useWorkspace } from "../../ui";
import { useOntology } from "../ontology/scope";
import { valueText } from "../explore/model";
import { deleteReviewDecision, reviewQueries, settleReview } from "./api";
import type { Naming } from "../explore/naming";
import {
  approvalOf,
  decisionFor,
  loadRead,
  matchesReview,
  propValueText,
  statementHash,
  type Candidate,
  type ReleaseStatus,
  type Settle,
  type Settleable,
} from "./model";

type ReleaseSettle = Extract<Settle, { kind: "release" }>;

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
 * What approving a held claim does: its ends in order, its link, its
 * properties as stored. On a link that holds one value (`single`, also when
 * the version isn't known) a date decides, so it promises no edge and hides no
 * replacement.
 */
function ClaimWritten({
  settle,
  single,
}: {
  settle: ReleaseSettle;
  single: boolean;
}) {
  const { link, statement: s, named, differs, formed } = settle;
  const props = Object.entries(s.props);
  return (
    <>
      <p>
        {single ? "Approving states" : "Approving writes"}:{" "}
        <strong>{s.subject.name}</strong> ({s.subject.type}){" "}
        <span aria-label="to">→</span> <code>{link}</code>{" "}
        <span aria-label="to">→</span> <strong>{s.object.name}</strong> (
        {s.object.type})
      </p>
      {single && (
        <p role="note">
          This link holds one value, and the latest dated statement wins. If
          this one is newest it replaces what the link holds now; if not, it’s
          kept as evidence and no link is written.
        </p>
      )}
      {single && formed && (
        <p role="note">
          “{formed}” is linked only by its legal form. Settle that name too, or
          this stays held.
        </p>
      )}
      {props.length > 0 && (
        <ul aria-label="Written with it">
          {props.map(([name, value]) => (
            <li key={name}>
              {name}: {value}
            </li>
          ))}
        </ul>
      )}
      {differs && (
        <p role="note">
          This sentence is read more than one way, and no approval can say
          which, so it stays held.
        </p>
      )}
      {!named && !differs && (
        <p className="on-muted">
          An end isn’t in the graph yet: approving creates it if the ontology
          lets documents create it. If it’s a name your data doesn’t hold, the
          claim stays held until that name is settled.
        </p>
      )}
    </>
  );
}

/**
 * Saves an override only. A load's graph and evidence are never edited.
 * `load` is the load the page shows; only the current one settles.
 */
export function SettleReview({
  review,
  load,
  link,
  naming,
}: {
  review: Settleable & { reason: string };
  load?: FoundryLoad;
  link: (path: string) => string;
  // The rules of the page's version: which links hold one value.
  naming: Naming;
}) {
  const { id: ws, role } = useWorkspace();
  const { id: o } = useOntology();
  const qc = useQueryClient();
  const query = useQuery(reviewQueries.list(ws, o));
  const [choice, setChoice] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const s = review.settle;
  const editable = !!load?.current;
  const held = s?.kind === "release" ? s : undefined;
  const read = useQuery({
    queryKey: ["statement-hash", held?.link, held?.stored],
    queryFn: held ? () => statementHash(held.link, held.stored) : skipToken,
    staleTime: Infinity,
  });
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
          <p className="on-muted">
            {editable
              ? "Load again to settle this review."
              : "Open the current load to settle this review."}
          </p>
        )}
      </>
    );
  const isProp = s.kind === "prop",
    isRelease = s.kind === "release";
  const field = !isProp
    ? "Link this name to"
    : s.table !== undefined
      ? "Replace the table’s value with"
      : "Keep value";
  const decision =
    isRelease || choice === ""
      ? undefined
      : isProp
        ? decisionFor(review, {
            value: s.values.find((v) => JSON.stringify(v.value) === choice)
              ?.value,
          })
        : decisionFor(review, {
            entityKey: choice === "none" ? null : choice.slice(4),
          });
  // Also when the version isn't known: a card promises no edge it can't.
  const single = isRelease && (!naming.version || naming.oneValued.has(s.link));
  const approval = isRelease
    ? approvalOf(query.data ?? [], s, read.data, editable)
    : undefined;
  const saved = approval?.saved ?? query.data?.find((d) => matchesReview(d, s));
  const moved = !!approval?.moved,
    earlier = !!approval?.earlier;
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
      {isRelease && <ClaimWritten settle={s} single={single} />}
      {!isProp && !isRelease && review.candidates.length > 0 && (
        <p>
          Suggestions only:{" "}
          <Candidates candidates={review.candidates} link={link} />
          <span className="on-muted"> · for the name “{s.normName}”</span>
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
      {moved && (
        <p role="status">
          This claim reads differently from what was approved, so it stays held.
          Read it again before approving it.
        </p>
      )}
      {saved && !moved && (
        <p role="status">
          {!editable
            ? "A decision is saved for this review."
            : loadRead(saved, load?.createdAt)
              ? isRelease
                ? s.formed && single
                  ? `This load read the approval, and the claim stays held: “${s.formed}” is linked only by its legal form, which doesn’t write a link that holds one value. Settle that name, and the next load writes this.`
                  : "This load read the approval, and the claim stays held: a name in it isn’t in your data or is linked only by its legal form, the sentence is read more than one way, it conflicts with what the graph holds, it would break a rule, or checking that took more reading than a load allows. If it no longer stands, remove the decision on the document’s page."
                : "This load read the saved decision, and the review is still open: what it chose isn’t in this load. Choose again or remove the decision."
              : "Decision saved. Reload to apply."}
        </p>
      )}
      {isRelease && !saved && earlier && (
        <p role="status">
          An earlier approval of this statement didn’t apply: what’s known of
          the names in it has changed since. Approve it again to write it.
        </p>
      )}
      {canManage(role) &&
        editable &&
        isRelease &&
        (!saved || moved) &&
        !s.differs && (
          <div className="on-stack">
            <p className="on-muted">
              On your word, from this sentence alone. Applies from the next
              load. Reload to apply.
            </p>
            {(error || query.error) && (
              <p role="alert" className="hf-form-error">
                {error || query.error?.message}
              </p>
            )}
            <div className="hf-form-actions">
              <Button
                size="sm"
                disabled={pending || !read.data}
                pending={pending}
                onClick={() =>
                  void (
                    read.data && save({ release: { statementHash: read.data } })
                  )
                }
              >
                Approve
              </Button>
            </div>
          </div>
        )}
      {canManage(role) && editable && !isRelease && (
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
            {(review.reason === "adjudicated" ||
              review.reason === "legal_form") && (
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

// A saved approval, as what became of it reads.
const APPROVED: Record<ReleaseStatus, string> = {
  written: "Approved",
  unknown: "Approved",
  held: "Approved, not written",
  no_link: "Approved, no link written, as a later statement holds it",
};

/** Saved overrides stay available to remove after their review disappears. */
export function SavedDecisions({
  documentId,
  quoteOf,
  statusOf,
  entity,
}:
  | {
      documentId: string;
      /** The text a release decision's place holds, if the page has it. */
      quoteOf?: (
        section: number,
        start: number,
        end: number,
      ) => string | undefined;
      /** What became of a release decision, if the page's reads say. */
      statusOf?: (
        d: Extract<ReviewDecision, { kind: "release" }>,
      ) => ReleaseStatus;
      entity?: never;
    }
  | {
      entity: { type: string; key: string };
      documentId?: never;
      quoteOf?: never;
      statusOf?: never;
    }) {
  const { id: ws, role } = useWorkspace(),
    { id: o } = useOntology();
  const qc = useQueryClient();
  const query = useQuery(reviewQueries.list(ws, o));
  const [pending, setPending] = useState("");
  const [notice, setNotice] = useState("");
  const [error, setError] = useState("");
  const relevant = (d: ReviewDecision) =>
    documentId
      ? (d.kind === "resolve" || d.kind === "release") && d.docId === documentId
      : entity &&
        d.kind !== "release" &&
        d.type === entity.type &&
        d.entityKey === entity.key;
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
                  {d.kind === "release"
                    ? `${APPROVED[statusOf?.(d) ?? "unknown"]}: “${quoteOf?.(d.claimSection, d.claimStart, d.claimEnd) ?? `section ${d.claimSection + 1}`}”`
                    : `${d.type} · ${
                        d.kind === "resolve"
                          ? `${d.normName} → ${d.entityKey ?? "Not in data"}`
                          : `${d.prop}: ${propValueText(d.value)}`
                      }`}
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
