import { Fragment, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  FoundryLoad,
  FoundryReviewChoice,
  FoundryReviewDecision,
  ReviewDecision,
} from "../../../shared/wfe";
import { Button, ButtonLink, Select } from "../../kit";
import { paths } from "../../paths";
import { ApiError } from "../../api";
import { canManage, useWorkspace } from "../../ui";
import { ontologyQueries } from "../ontology/queries";
import { useOntology } from "../ontology/scope";
import { spanAnchor } from "../documents/model";
import { valueText } from "../explore/model";
import { deleteReviewDecision, reviewQueries, settleReview } from "./api";
import type { Naming } from "../explore/naming";
import {
  approvalOf,
  decidedText,
  decisionFor,
  holdingEnds,
  holdsDecided,
  isUndoableLink,
  isWeakLink,
  loadRead,
  matchesReview,
  propValueText,
  refusalText,
  waitingText,
  WEAK_LINK_TEXT,
  type Candidate,
  type ReleaseStatus,
  type Settle,
  type Settleable,
  type Unsettled,
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
 * A link to the review of each of a held claim's names (`ends`) whose place
 * the page read: the document's Needs review (`docId`), at the name. One
 * name's reads "Review that name"; of two, each says which.
 */
function NameReviews({
  ends,
  docId,
  link,
}: {
  ends: readonly Unsettled[];
  docId: string;
  link: (path: string) => string;
}) {
  const { id: o } = useOntology();
  return ends.map(({ name, at }) =>
    at ? (
      <Fragment key={`${at.section}-${at.start}`}>
        {" "}
        <Link
          to={`${link(paths.document(o, docId, "review"))}#${spanAnchor(at.section, at.start)}`}
        >
          {ends.length > 1 ? `Review “${name}”` : "Review that name"}
        </Link>
      </Fragment>
    ) : null,
  );
}

/**
 * What approving a held claim does: its ends in order, its link, its
 * properties as stored. On a link that holds one value (`single`, also when
 * the version isn't known) the approval decides the link (contract 4.14.0),
 * as the engine dates it: from the date its quote gives, else its
 * document's, else the day it is first approved; one of a date still to come
 * beats nothing until then: where the link holds another value the engine
 * refuses it as `conflict`, and a load or resend after the date writes it. It
 * replaces what the link holds of no date or dated up to then, which a
 * claim held as conflicting with it (`conflict`) says; a statement dated
 * later, approved or not, still holds the link, so it promises no edge; of
 * two approvals the one first decided later decides (approving again keeps
 * the first time). The
 * ends whose names the load linked on less than a name, where that holds the
 * claim (`holding`, holdingEnds), are named as the document writes them,
 * each with the way to its review.
 */
function ClaimWritten({
  settle,
  single,
  holding,
  link: to,
}: {
  settle: ReleaseSettle;
  single: boolean;
  holding: readonly Unsettled[];
  link: (path: string) => string;
}) {
  const { link, statement: s, named, differs } = settle;
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
          This link holds one value. An approval makes this its value from the
          date this sentence gives, else its document’s date, else the day it is
          first approved; a sentence dated ahead replaces nothing until that
          date, and stays held meanwhile if the link holds another value. It
          replaces what the link holds of no date or dated up to then, whose
          evidence stays. A statement dated later, approved or not, still holds
          the link, and then this is kept as evidence only. Of two approvals,
          the one first approved later decides.
          {settle.conflict &&
            " It conflicts with what the graph holds: approving replaces that value unless it is dated later, though not a property of the same fact that differs, which keeps it held."}
        </p>
      )}
      {holding.length > 0 && (
        <p role="note">
          {holding
            .map((u) => `“${u.name}” is ${WEAK_LINK_TEXT[u.by]}. `)
            .join("")}
          Settle {holding.length > 1 ? "those names" : "that name"} too, or this
          stays held.
          <NameReviews ends={holding} docId={settle.docId} link={to} />
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
 * Settles a review of the load the page shows (`load`); only the current one
 * settles. A name is saved as an override for the next load. An approval of
 * a held claim is written to the current load at once, by a job of its own:
 * its card says it is being written, or what it waits behind when another
 * job is running, then the claim is a fact and the card goes, or the card
 * says why the load kept it held. One the engine holds for a name the load
 * linked on less than a name (holdingEnds) is the exception: no approval
 * writes it before that name is settled, which only a later load reads, so
 * its card promises the first load after that, and offers no approval a
 * load is sure to refuse once one is saved. A chosen value goes the same way: the job
 * sends the load its decisions, the card says so, and the value shows
 * wherever the page reads it, with no reload. A held claim can be rejected
 * instead: that only saves the person's word, and the page that lists the
 * card leaves it out from then on (`unrejected`).
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
  const { id: o, ontology } = useOntology();
  const qc = useQueryClient();
  const query = useQuery(reviewQueries.list(ws, o));
  const [choice, setChoice] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const s = review.settle;
  const editable = !!load?.current;
  const held = s?.kind === "release" ? s : undefined;
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
  // A value this load holds by a decision is no table's: its card offers
  // another choice, and says nothing of a table.
  const decided = query.data?.find((d) => matchesReview(d, s)),
    holds =
      isProp &&
      editable &&
      decided?.kind === "prop" &&
      holdsDecided(decided.value, s.table);
  const table = isProp && s.table !== undefined && !holds;
  const field = !isProp
    ? "Link this name to"
    : holds
      ? "Choose another value"
      : table
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
  // A claim held as conflicting with what its link holds, where the version
  // says the link holds many values: a property of the fact differs from the
  // one the graph holds, which no approval changes (contract 4.14.0), so
  // nothing settles it here. Where the version isn't known the backend,
  // which reads it, decides.
  const fixed = isRelease && !!s.conflict && !single;
  // The names of the claim the load linked on less than a name that hold it
  // in this load, in the order the engine refuses them.
  const unsettled = isRelease ? holdingEnds(s.unsettled, single) : [];
  const approval = isRelease
    ? approvalOf(query.data ?? [], s, editable)
    : undefined;
  const saved = approval?.saved ?? query.data?.find((d) => matchesReview(d, s));
  const moved = !!approval?.moved;
  // What the current load made of a saved approval: nothing yet when its
  // list doesn't hold it (saved while another load was current), and
  // nothing to write when it found no claim at the approval's place. Either
  // way approving this card binds it to this load's review, at once. So
  // does approving one whose job failed before saying what became of it.
  const applied = isRelease && editable ? saved?.applied : undefined,
    unread = isRelease && editable && !!saved && !applied,
    missed = applied?.status === "missed",
    retry = applied?.status === "unconfirmed";
  // Approve where a card could be approved now, or, held for a name, where
  // that saves an approval of what it says for a later load: none is saved,
  // or the one saved approved what it said before. Not where a load refused
  // it, whatever the reason: sent again to the same load it is refused
  // again. A `conflict` refusal of a link that holds one value by an engine
  // before contract 4.14.0 is the one exception, and the card can't tell it
  // from a later one (a property of the fact differs, or a load built before
  // statements were kept wrote the link), which nothing approved here
  // changes; the next load sends every approval again and writes the first,
  // and the card says so (refusalText). Reject on any held claim that isn't
  // being written or written: its job may have sent it. Neither until the
  // saved decisions are read: one may be a rejection of this claim, which
  // an approval would replace, or the approval it has.
  const approves =
      isRelease &&
      !!query.data &&
      !fixed &&
      !s.differs &&
      (!saved || moved || (!unsettled.length && (unread || missed || retry))),
    rejects =
      isRelease &&
      !!query.data &&
      !fixed &&
      applied?.status !== "pending" &&
      applied?.status !== "written";
  // Why the load holds a saved approval, where a name may: its word, or
  // where it gave none, the way the engine refuses the claim's names first.
  // The card names, and links to, the names linked that way, and every
  // other name that holds the claim: each is settled before a load writes it.
  const heldFor =
      !editable || !saved || moved || applied?.status === "written"
        ? undefined
        : applied?.status === "refused"
          ? applied.reason
          : unsettled[0]?.by,
    heldBy =
      isRelease && isWeakLink(heldFor)
        ? (s.unsettled ?? []).filter(
            (u) => u.by === heldFor || unsettled.includes(u),
          )
        : [];
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
      {isRelease && !fixed && (
        <ClaimWritten
          settle={s}
          single={single}
          holding={unsettled}
          link={link}
        />
      )}
      {fixed && (
        <p role="note">
          It conflicts with a property of a fact the graph holds of this link,
          which no approval changes.
        </p>
      )}
      {!isProp && !isRelease && review.candidates.length > 0 && (
        <p>
          {isUndoableLink(review.reason) ? "Linked to:" : "Suggestions only:"}{" "}
          <Candidates candidates={review.candidates} link={link} />
          <span className="on-muted"> · for the name “{s.normName}”</span>
        </p>
      )}
      {isProp && table && (
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
            : isRelease
              ? applied?.status === "pending"
                ? unsettled.length
                  ? `Approved. This load keeps it held: the first load after ${unsettled.length > 1 ? "those names are" : "that name is"} settled writes it.`
                  : waitingText(ontology.activeJob?.kind)
                : applied?.status === "written"
                  ? "Approved, and written to this load."
                  : applied?.status === "refused"
                    ? `Approved, and this load kept it held. ${refusalText(applied.reason, heldBy)}`
                    : unsettled[0]
                      ? `An approval of this is saved, and the claim stays held. ${refusalText(unsettled[0].by, heldBy)}`
                      : retry
                        ? "Approved, but the job that writes it failed before it said whether this was written. Approve it again to finish."
                        : unread
                          ? "An approval of this is saved, and this load hasn’t read it. Approve it to write it now."
                          : missed
                            ? "An approval of this is saved, and it found no claim at its place in this load. Approve it to write it now."
                            : "This load read the approval, and the claim stays held: a name in it isn’t in your data or matches a row only loosely, the sentence is read more than one way, it conflicts with what the graph holds, it would break a rule, or checking that took more reading than a load allows. If it no longer stands, remove the decision on the document’s page."
              : saved.kind === "prop"
                ? decidedText(
                    saved.applied,
                    propValueText(saved.value),
                    ontology.activeJob?.kind,
                    saved.value.null !== undefined
                      ? s.kind === "prop" && s.table === undefined
                      : holds,
                  )
                : loadRead(saved, load?.createdAt)
                  ? "This load read the saved decision, and the review is still open: what it chose isn’t in this load. Choose again or remove the decision."
                  : "Decision saved. Reload to apply."}
          {held && heldBy.length > 0 && (
            <NameReviews ends={heldBy} docId={held.docId} link={link} />
          )}
        </p>
      )}
      {canManage(role) && editable && (approves || rejects) && (
        <div className="on-stack">
          {approves && (
            <p className="on-muted">
              On your word, from this sentence alone.{" "}
              {unsettled.length
                ? `This load keeps it held: the first load after ${unsettled.length > 1 ? "those names are" : "that name is"} settled writes it.`
                : "It’s written to this load at once."}{" "}
              Later loads read the approval too, and each says what it made of
              it.
            </p>
          )}
          {(error || query.error) && (
            <p role="alert" className="hf-form-error">
              {error || query.error?.message}
            </p>
          )}
          <div className="hf-form-actions">
            {rejects && (
              <Button
                size="sm"
                variant="ghost"
                disabled={pending}
                onClick={() =>
                  void (held && save({ reject: { statementHash: held.hash } }))
                }
              >
                Reject
              </Button>
            )}
            {approves && (
              <Button
                size="sm"
                disabled={pending}
                pending={pending}
                onClick={() =>
                  void (held && save({ release: { statementHash: held.hash } }))
                }
              >
                Approve
              </Button>
            )}
          </div>
          {rejects && (
            <p className="on-muted">
              Reject it if it’s wrong: nothing is written, and it leaves Needs
              review in this load and later ones. Remove the decision under
              Saved decisions to list it again.
            </p>
          )}
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
            {isProp
              ? `${
                  table
                    ? "A chosen value replaces the table’s in this load and every later one, even after the table changes. "
                    : ""
                }It’s sent to this load at once, and later loads read it too. A document dated after your decision still wins.`
              : "Applies from the next load. Reload to apply."}
          </p>
          {(error || query.error) && (
            <p role="alert" className="hf-form-error">
              {error || query.error?.message}
            </p>
          )}
          <div className="hf-form-actions">
            {isUndoableLink(review.reason) && (
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

/**
 * A saved value's line: what the current load made of it, where it says
 * (FoundryReviewDecision.applied). The load says only that it was sent it.
 */
const chosenLine = (
  d: Extract<ReviewDecision, { kind: "prop" }> & FoundryReviewDecision,
) => {
  const line = `${d.type} · ${d.prop}: ${propValueText(d.value)}`;
  switch (d.applied?.status) {
    case "pending":
      return `${line} · being sent to this load`;
    case "applied":
      return `${line} · sent to this load`;
    case "unconfirmed":
      return `${line} · not confirmed as sent to this load`;
    default:
      return line;
  }
};

// A saved approval, as what became of it reads.
const APPROVED: Record<ReleaseStatus, string> = {
  written: "Approved",
  unknown: "Approved",
  held: "Approved, not written",
  no_link:
    "Approved, no link written: a statement dated later, or a later approval, holds it",
};

/**
 * A saved approval's line: being written while its job has yet to run, else
 * what the page's reads say became of it, which tell a statement kept as
 * evidence alone from one that made a link. Where the reads don't say, the
 * load's word does: held when it refused it, and not confirmed when its job
 * failed before saying, which is no claim that it stays held.
 */
const approvedLine = (
  d: Extract<ReviewDecision, { kind: "release" }> & FoundryReviewDecision,
  read: ReleaseStatus,
) =>
  d.applied?.status === "pending"
    ? "Approved, being written"
    : read !== "unknown"
      ? APPROVED[read]
      : d.applied?.status === "refused"
        ? APPROVED.held
        : d.applied?.status === "unconfirmed"
          ? "Approved, not confirmed as written"
          : APPROVED.unknown;

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
      ? d.kind !== "prop" && d.docId === documentId
      : entity &&
        (d.kind === "resolve" || d.kind === "prop") &&
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
                  {d.kind === "release" || d.kind === "reject"
                    ? `${d.kind === "reject" ? "Rejected" : approvedLine(d, statusOf?.(d) ?? "unknown")}: “${quoteOf?.(d.claimSection, d.claimStart, d.claimEnd) ?? `section ${d.claimSection + 1}`}”`
                    : d.kind === "resolve"
                      ? `${d.type} · ${d.normName} → ${d.entityKey ?? "Not in data"}`
                      : chosenLine(d)}
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
                        // A value is taken back from the current load by a
                        // job: the ontology's live job is read again, and
                        // the page's reads follow when it ends.
                        if (d.kind === "prop")
                          await Promise.all(
                            [ontologyQueries.ontology, ontologyQueries.loads]
                              .map((q) => q(ws, o).queryKey)
                              .map((queryKey) =>
                                qc.invalidateQueries({ queryKey, exact: true }),
                              ),
                          );
                        setNotice(
                          d.kind === "prop"
                            ? "Decision removed. This load is taking the value back now; later loads won’t read it."
                            : d.kind === "reject"
                              ? "Decision removed. The claim is listed for review again."
                              : "Decision removed. Reload to apply.",
                        );
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
          <p className="on-muted">
            An approval or a chosen value applies to the current load at once,
            and removing a chosen value takes it back at once. A linked name
            applies to future loads, and so does an approval held for a name
            until that name is settled. A rejected claim stays out of Needs
            review until its decision is removed.
          </p>
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
