// Spot checks, at the walkthrough's finish (./walk-done.tsx): a few claims
// drawn at random from every score band, to measure how often claims at
// each score are right, which is what lets Founder suggest approving a
// band's held claims and, once measured, all of them at once. The index
// draws them (./cards-api.ts drawSpotChecks, sent once as the finish opens),
// the same for everyone, and keeps each at its place. A held claim drawn so
// is approved or rejected (a decision, kept as a label too); a decided
// claim, one no approval writes, or a fact the graph holds takes Right or
// Wrong, a label that changes nothing. Then a few decisions and labels of
// other people as optional re-checks, and what the random checks found so
// far.
import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  FoundryLoad,
  FoundryReviewDecision,
  FoundryReviewDraw,
  FoundryReviewLabel,
  FoundryReviewPrecision,
  ReviewVerdict,
  SettleReviewLabelRequest,
} from "../../../shared/wfe";
import { Button, Panel, SkeletonText, formatNumber } from "../../kit";
import { canManage, useWorkspace } from "../../ui";
import type { FactQuote } from "../explore/model";
import { readQueries } from "../explore/reads";
import { useTenantReads } from "../explore/tenant-reads";
import { useOntology } from "../ontology/scope";
import { settleLabel } from "./api";
import { cardQueries, drawSpotChecks } from "./cards-api";
import type { ReviewCards } from "./index";
import { approvalOf, markDiffering, rejectionOf } from "./model";
import {
  BAND_TEXT,
  asBand,
  claimPlace,
  labelPlace,
  type Band,
  type QueueReview,
} from "./queue";
import {
  CardHead,
  ClaimLine,
  Evidence,
  HeldHow,
  Score,
  statementLine,
} from "./review-card";
import { SettleReview } from "./settle";

/** How many re-checks the page offers at once. */
const RECHECKS_SHOWN = 5;

/** A place in a document, as draws, decisions and labels name it. */
type Place = {
  documentId: string;
  section: number;
  start: number;
  end: number;
};

/** A decision or a label a second person may re-check, and what it said. */
type Recheck =
  | {
      kind: "decision";
      key: string;
      decision: Extract<FoundryReviewDecision, { kind: "release" | "reject" }>;
      against: ReviewVerdict;
    }
  | {
      kind: "label";
      key: string;
      label: FoundryReviewLabel;
      against: ReviewVerdict;
    };

const drawAt = (d: FoundryReviewDraw): Place => ({
  documentId: d.docId,
  section: Number(d.span?.section ?? 0),
  start: Number(d.span?.start ?? 0),
  end: Number(d.span?.end ?? 0),
});
const recheckAt = (r: Recheck): Place =>
  r.kind === "decision"
    ? {
        documentId: r.decision.docId,
        section: r.decision.claimSection,
        start: r.decision.claimStart,
        end: r.decision.claimEnd,
      }
    : {
        documentId: r.label.docId,
        section: r.label.claimSection ?? 0,
        start: r.label.claimStart ?? 0,
        end: r.label.claimEnd ?? 0,
      };
const samePlace = (a: Place, b: Place) =>
  a.documentId === b.documentId &&
  a.section === b.section &&
  a.start === b.start &&
  a.end === b.end;

/**
 * The finish's spot checks: the load's draws not yet labelled (drawn once
 * as the finish opens), with those labelled on this page keeping their
 * places; the re-checks offered to this person; and the random checks'
 * precision so far.
 */
export function SpotChecks({
  queue: q,
  load,
}: {
  queue: ReviewCards;
  load: FoundryLoad;
}) {
  const { id: ws, role } = useWorkspace(),
    { id: o } = useOntology();
  const qc = useQueryClient();
  const { readLater } = useTenantReads();
  const scope = { id: load.id };
  const spotKey = cardQueries.spot(ws, o, load.id).queryKey;
  // Drawn once a load, as the finish opens: the index tops the load's
  // undone draws up to five.
  const asked = useRef("");
  const [drawError, setDrawError] = useState("");
  useEffect(() => {
    if (asked.current === load.id || !load.current) return;
    asked.current = load.id;
    drawSpotChecks(ws, o, load.id).then(
      (r) => qc.setQueryData(spotKey, r),
      (e: Error) => setDrawError(e.message),
    );
  }, [load.id]);
  const spot = q.spot.data?.loadId === load.id ? q.spot.data : undefined;
  // A draw labelled on this page keeps its place.
  const [here, setHere] = useState<ReadonlySet<string>>(new Set());
  const draws = (spot?.draws ?? []).filter((d) => !d.done || here.has(d.seq));
  const rechecks = (spot?.rechecks ?? []).flatMap((r): Recheck[] =>
    r.decision
      ? r.decision.kind === "release" || r.decision.kind === "reject"
        ? [
            {
              kind: "decision",
              key: `decision-${r.decision.id}`,
              decision: r.decision,
              against: r.decision.kind === "release" ? "right" : "wrong",
            },
          ]
        : []
      : [
          {
            kind: "label",
            key: `label-${r.label.id}`,
            label: r.label,
            against: r.label.verdict,
          },
        ],
  );
  // The reviews at each held draw's and re-check's place, while the load
  // holds the claim back.
  const places = [
    ...draws.flatMap((d) => (d.held ? [drawAt(d)] : [])),
    ...rechecks.map(recheckAt),
  ];
  const docs = [...new Set(places.map((p) => p.documentId))].sort(),
    starts = [...new Set(places.map((p) => p.start))].sort((a, b) => a - b);
  const reviewsAt = useQuery({
    ...readQueries.reviewsAt(ws, o, readLater, scope, docs, starts, q.naming),
    enabled: docs.length > 0,
  });
  const reviews = markDiffering(reviewsAt.data ?? []);
  const heldReview = (d: FoundryReviewDraw) =>
    d.held?.reviewKey
      ? reviews.find(
          (r) =>
            r.settle?.kind === "release" &&
            r.settle.reviewId === d.held!.reviewKey &&
            samePlace(r, drawAt(d)),
        )
      : undefined;
  // A re-check's claim while the load holds it back, stating what was
  // decided (a rebuild may read the place otherwise).
  const recheckReview = (r: Recheck) =>
    reviews.find(
      (v) =>
        v.settle?.kind === "release" &&
        samePlace(v, recheckAt(r)) &&
        v.settle.hash ===
          (r.kind === "decision"
            ? r.decision.statementHash
            : r.label.statementHash),
    );
  // Else the fact's quote at its place: a quote drawn, a held claim whose
  // approval wrote it, or a re-check's.
  const bare = [
    ...draws.flatMap((d) =>
      d.quote || (reviewsAt.data && !heldReview(d)) ? [d.docId] : [],
    ),
    ...(reviewsAt.data
      ? rechecks.flatMap((r) =>
          recheckReview(r) ? [] : [recheckAt(r).documentId],
        )
      : []),
  ];
  const quoteDocs = [...new Set(bare)].sort();
  const quotes = useQuery({
    ...readQueries.documentQuotes(ws, o, readLater, scope, quoteDocs),
    enabled: quoteDocs.length > 0,
  });
  const quoteAt = (at: Place, link: string | undefined, factKey?: string) => {
    const here = (quotes.data ?? []).filter(
      (x) =>
        samePlace(x, at) &&
        (factKey ? x.factKey === factKey : !link || x.link === link),
    );
    // A decision keeps no link: its quote is the place's only one, or none.
    return new Set(here.map((x) => x.link)).size === 1 ? here[0] : undefined;
  };
  const drawQuote = (d: FoundryReviewDraw) =>
    heldReview(d) ? undefined : quoteAt(drawAt(d), d.link, d.quote?.factKey);
  const shownRechecks = rechecks
    .map((r) => {
      const review = recheckReview(r);
      return {
        r,
        review,
        quote: review
          ? undefined
          : quoteAt(
              recheckAt(r),
              r.kind === "label" ? r.label.link : undefined,
            ),
      };
    })
    .filter((x) => x.review || x.quote)
    .slice(0, RECHECKS_SHOWN);
  const facts = [
    ...draws.flatMap((d) => {
      const x = drawQuote(d);
      return x ? [x] : [];
    }),
    ...shownRechecks.flatMap((x) => (x.quote ? [x.quote] : [])),
  ];
  const ends = useQuery({
    ...readQueries.quoteFacts(ws, o, readLater, scope, facts, q.naming),
    enabled: facts.length > 0,
  });
  const endsOf = (x: FactQuote) => {
    const i = facts.indexOf(x);
    return {
      from: ends.data?.[i]?.from,
      to: ends.data?.[i]?.to,
      ends: ends.isPending ? "reading" : ends.error ? "failed" : "read",
    } as const;
  };
  const labelled = (d: FoundryReviewDraw) => {
    setHere((now) => new Set(now).add(d.seq));
    void qc.invalidateQueries({ queryKey: spotKey });
    // The precision table reads the FYI lane's first page.
    void qc.invalidateQueries({
      queryKey: [ws, "ontology", o, "review-cards", load.id],
    });
  };
  const labelOf = (d: FoundryReviewDraw) =>
    (spot?.labels ?? []).find(
      (l) =>
        l.drawn === "random" && labelPlace(l) === claimPlace(drawAt(d), d.link),
    );
  const reading = reviewsAt.isPending && docs.length > 0;
  return (
    <>
      {drawError && (
        <p role="alert" className="hf-form-error">
          Couldn’t draw the spot checks: {drawError}
        </p>
      )}
      {!spot ? (
        q.spot.error ? (
          <p className="on-muted">
            Couldn’t read the spot checks: {q.spot.error.message}
          </p>
        ) : (
          <SkeletonText lines={3} />
        )
      ) : (
        draws.length > 0 && (
          <ol className="rq-list" aria-label="Spot checks">
            {draws.map((d) => {
              const band = asBand(d.band) ?? "none";
              const review = heldReview(d),
                quote = drawQuote(d);
              const s = review?.settle;
              if (review && s?.kind === "release") {
                const decided =
                  !!approvalOf(spot.decisions ?? [], s, true).saved ||
                  !!rejectionOf(spot.decisions ?? [], s);
                const blocked =
                  !!s.differs || (!!s.conflict && !q.single(s.link));
                return decided || blocked ? (
                  <DecidedItem
                    key={d.seq}
                    review={review}
                    blocked={blocked}
                    decisions={spot.decisions ?? []}
                    label={labelOf(d)}
                    onLabelled={() => labelled(d)}
                  />
                ) : (
                  <li key={d.seq} className="rq-card" data-drawn="random">
                    <CardHead title="A held claim" random />
                    <Evidence at={review} quote={review.quote} />
                    <p className="rq-writes">{RANDOM_NOTE}</p>
                    <Score confidence={review.confidence} />
                    <HeldHow review={review} />
                    <SettleReview
                      review={review}
                      load={load}
                      link={(path) => path}
                      naming={q.naming}
                      drawn="random"
                      compact
                      onSettled={() => labelled(d)}
                    />
                  </li>
                );
              }
              return (
                <li key={d.seq} className="rq-card">
                  <CardHead
                    title={
                      d.held
                        ? "An approved claim"
                        : band === "0.9"
                          ? "An accepted fact"
                          : "An approved fact"
                    }
                    random
                  />
                  {quote ? (
                    <QuoteFact
                      quote={quote}
                      {...endsOf(quote)}
                      band={band}
                      label={labelOf(d)}
                      onLabelled={() => labelled(d)}
                    />
                  ) : reading || quotes.isPending ? (
                    <SkeletonText lines={2} />
                  ) : (
                    <p className="on-muted">
                      {quotes.error
                        ? `Couldn’t read this draw: ${quotes.error.message}`
                        : "The load no longer holds this claim at its place."}
                    </p>
                  )}
                </li>
              );
            })}
          </ol>
        )
      )}
      {shownRechecks.length > 0 && (
        <Rechecks
          items={shownRechecks}
          endsOf={endsOf}
          manage={canManage(role)}
        />
      )}
      <PrecisionTable
        precision={
          q.fyi.data?.pages[0]?.precision ?? q.decide.data?.pages[0]?.precision
        }
      />
    </>
  );
}

const RANDOM_NOTE =
  "Drawn at random from its score band, to measure how often claims scored so are right. Approve it only if the quote says it.";

/**
 * Right and Wrong on a claim, a label (`request`), or what this person said
 * of it (`own`). It changes nothing in the graph or the decisions.
 */
function LabelButtons({
  request,
  own,
  onLabelled,
}: {
  request: (verdict: ReviewVerdict) => SettleReviewLabelRequest;
  own?: FoundryReviewLabel;
  onLabelled: () => void;
}) {
  const { id: ws, role } = useWorkspace(),
    { id: o } = useOntology();
  const [pending, setPending] = useState(false),
    [error, setError] = useState("");
  if (own)
    return (
      <p role="status">
        You said it’s {own.verdict}. It’s counted in the random checks below.
      </p>
    );
  if (!canManage(role)) return null;
  const say = async (verdict: ReviewVerdict) => {
    setPending(true);
    setError("");
    try {
      await settleLabel(ws, o, request(verdict));
      onLabelled();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPending(false);
    }
  };
  return (
    <>
      <div className="rq-verdict">
        <Button size="sm" disabled={pending} onClick={() => void say("wrong")}>
          Wrong
        </Button>
        <Button
          size="sm"
          disabled={pending}
          pending={pending}
          onClick={() => void say("right")}
        >
          Right
        </Button>
      </div>
      {error && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
    </>
  );
}

/** A fact the graph holds at a drawn place: Right or Wrong, a label of it. */
function QuoteFact({
  quote: q,
  from,
  to,
  ends,
  band,
  label,
  onLabelled,
}: {
  quote: FactQuote;
  from?: { name: string };
  to?: { name: string };
  ends: "read" | "reading" | "failed";
  band: Band;
  label?: FoundryReviewLabel;
  onLabelled: () => void;
}) {
  const { user } = useWorkspace();
  const gone = ends === "read" && !from && !to;
  return (
    <>
      <Evidence at={q} quote={q.quote} />
      <p className="rq-writes">
        {gone ? (
          <>
            It states <code>{q.link}</code>, and the graph holds it as evidence
            only now: a later statement holds the link.
          </>
        ) : (
          <>
            The graph holds it as a fact:{" "}
            <ClaimLine from={from?.name} link={q.link} to={to?.name} />.
          </>
        )}{" "}
        {band === "0.9"
          ? "Drawn at random from accepted facts"
          : band === "none"
            ? "Drawn at random from claims with no score that people approved"
            : "Drawn at random from facts people approved at this score"}
        , to measure how often they’re right. Your word is kept as a label: it
        changes nothing in the graph.
      </p>
      {ends === "failed" && (
        <p className="on-muted">Couldn’t read what the fact joins.</p>
      )}
      <Score confidence={q.confidence} />
      <LabelButtons
        own={label?.decidedByUserId === user.id ? label : undefined}
        request={(verdict) => ({
          fact: {
            docId: q.documentId,
            section: q.section,
            start: q.start,
            end: q.end,
            factKey: q.factKey,
          },
          verdict,
          drawn: "random",
        })}
        onLabelled={onLabelled}
      />
    </>
  );
}

/**
 * A held claim drawn at random that someone decided, or that no approval
 * writes: what it states, what was decided, and Right or Wrong, a label of
 * it; the decision stands.
 */
function DecidedItem({
  review,
  blocked,
  decisions,
  label,
  onLabelled,
}: {
  review: QueueReview;
  blocked: boolean;
  decisions: readonly FoundryReviewDecision[];
  label?: FoundryReviewLabel;
  onLabelled: () => void;
}) {
  const { user } = useWorkspace();
  const s = review.settle;
  if (s?.kind !== "release") return null;
  const approved = approvalOf(decisions, s, true).saved,
    rejected = rejectionOf(decisions, s);
  return (
    <li className="rq-card">
      <CardHead
        title={blocked ? "A claim no approval writes" : "A decided claim"}
        random
      />
      <Evidence at={review} quote={review.quote} />
      <p className="rq-writes">
        It states {statementLine(s.statement, s.link)}.{" "}
        {blocked
          ? "It is read more than one way here, or a property of it differs from the fact the graph holds, so no approval writes it."
          : rejected
            ? "Someone rejected it."
            : approved
              ? "Someone approved it."
              : "It is decided."}{" "}
        Drawn at random from its score band, to measure how often claims scored
        so are right. Your word is kept as a label, and changes nothing else.
      </p>
      <Score confidence={review.confidence} />
      <LabelButtons
        own={label?.decidedByUserId === user.id ? label : undefined}
        request={(verdict) => ({
          reviewId: s.reviewId,
          verdict,
          drawn: "random",
        })}
        onLabelled={onLabelled}
      />
    </li>
  );
}

/**
 * The re-checks offered to this person: another's decision or label, drawn
 * at random by the index, each with its quote and what the claim states,
 * what was said of it, and Right or Wrong, a label that changes nothing.
 * Only those whose claim the current load still holds are offered.
 */
function Rechecks({
  items,
  endsOf,
  manage,
}: {
  items: { r: Recheck; review?: QueueReview; quote?: FactQuote }[];
  endsOf: (q: FactQuote) => { from?: { name: string }; to?: { name: string } };
  manage: boolean;
}) {
  return (
    <Panel
      title="Re-checks"
      subtitle="Optional: a few of other people’s queue decisions and random checks, drawn at random, for a second person to say whether they’re right. Your word changes nothing; it measures how often people agree."
    >
      <ol className="rq-list" aria-label="Re-checks">
        {items.map(({ r, review, quote }) => {
          const s =
            review?.settle?.kind === "release" ? review.settle : undefined;
          const fact = quote ? endsOf(quote) : undefined;
          return (
            <li key={r.key} className="rq-card">
              <CardHead title="A re-check" />
              {review ? (
                <Evidence at={review} quote={review.quote} />
              ) : (
                <Evidence at={quote!} quote={quote!.quote} />
              )}
              <p className="rq-writes">
                It states{" "}
                {s ? (
                  statementLine(s.statement, s.link)
                ) : (
                  <ClaimLine
                    from={fact?.from?.name}
                    link={quote!.link}
                    to={fact?.to?.name}
                  />
                )}
                .{" "}
                {r.kind === "decision"
                  ? r.decision.kind === "release"
                    ? "Someone approved it."
                    : "Someone rejected it."
                  : `Someone said it’s ${r.label.verdict}.`}{" "}
                Is it right?
              </p>
              {manage && <RecheckButtons item={r} />}
            </li>
          );
        })}
      </ol>
    </Panel>
  );
}

function RecheckButtons({ item }: { item: Recheck }) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const [pending, setPending] = useState(false),
    [error, setError] = useState(""),
    [said, setSaid] = useState<ReviewVerdict>();
  const say = async (verdict: ReviewVerdict) => {
    setPending(true);
    setError("");
    try {
      await settleLabel(
        ws,
        o,
        item.kind === "decision"
          ? { decisionId: item.decision.id, verdict, drawn: "recheck" }
          : { labelId: item.label.id, verdict, drawn: "recheck" },
      );
      setSaid(verdict);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPending(false);
    }
  };
  if (said)
    return (
      <p role="status">
        You said it’s {said}
        {said === item.against
          ? ", as they did."
          : ", where they said otherwise."}
      </p>
    );
  return (
    <>
      <div className="rq-verdict">
        <Button size="sm" disabled={pending} onClick={() => void say("wrong")}>
          Wrong
        </Button>
        <Button
          size="sm"
          disabled={pending}
          pending={pending}
          onClick={() => void say("right")}
        >
          Right
        </Button>
      </div>
      {error && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
    </>
  );
}

/** How often the random checks of each band were right, and how often re-checks agreed. */
function PrecisionTable({
  precision: p,
}: {
  precision?: FoundryReviewPrecision;
}) {
  const bands = (p?.bands ?? []).flatMap((b) => {
    const band = asBand(b.band);
    return band ? [{ band, right: b.right ?? 0, total: b.total ?? 0 }] : [];
  });
  const total = p?.rechecksTotal ?? 0;
  if (!bands.length && !total) return null;
  return (
    <Panel
      title="Random checks"
      subtitle="What people said of the claims drawn at random, one word a claim, by the verifier’s score. Only these measure precision: the ranked cards are not a fair sample."
    >
      <table className="rq-precision">
        <thead>
          <tr>
            <th scope="col">Score</th>
            <th scope="col">Right</th>
            <th scope="col">Checked</th>
          </tr>
        </thead>
        <tbody>
          {bands.map((b) => (
            <tr key={b.band}>
              <th scope="row">{BAND_TEXT[b.band]}</th>
              <td>{formatNumber(b.right)}</td>
              <td>{formatNumber(b.total)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {total > 0 && (
        <p className="on-muted">
          Re-checks: a second person agreed{" "}
          {formatNumber(p?.rechecksAgreed ?? 0)} of {formatNumber(total)} times.
        </p>
      )}
    </Panel>
  );
}
