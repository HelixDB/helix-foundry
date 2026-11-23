// Spot checks, at the walkthrough's finish (./walk-done.tsx): a few claims
// drawn at random from a score band (./queue.ts buildQueue), to measure how
// often claims at each score are right, which is what lets Founder suggest
// approving a band's held claims and, once measured, all of them at once. A
// held claim drawn so is approved or rejected (a decision, kept as a label
// too); a decided claim, or a fact the graph holds, takes Right or Wrong, a
// label that changes nothing. Then a few decisions and labels of other
// people as optional re-checks, and what the random checks found so far.
// Moved here out of the queue so they no longer break a run of accepts.
import { useState } from "react";
import { useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  FoundryLoad,
  FoundryReviewLabel,
  ReviewVerdict,
  SettleReviewLabelRequest,
} from "../../../shared/wfe";
import { Button, Panel, SkeletonText, formatNumber } from "../../kit";
import { canManage, useWorkspace } from "../../ui";
import type { EntityRow, FactQuote, Span } from "../explore/model";
import type { Naming } from "../explore/naming";
import { readQueries } from "../explore/reads";
import { useTenantReads } from "../explore/tenant-reads";
import { useOntology } from "../ontology/scope";
import { labelQueries, reviewQueries, settleLabel } from "./api";
import type { ReviewQueue } from "./index";
import { approvalOf, rejectionOf } from "./model";
import {
  BAND_RANGE,
  BAND_TEXT,
  claimPlace,
  labelPlace,
  precision,
  quoteOffsets,
  rechecks,
  type Band,
  type ClaimCard,
  type DecidedCard,
  type QueueCard,
  type QueueReview,
  type QuoteCard,
  type Recheck,
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
import { SPOT_CHECKS } from "./walk";

/** How many re-checks the page offers at once, of how many it reads. */
const RECHECKS_SHOWN = 5;
const RECHECKS_READ = 20;

/**
 * The finish's spot checks: the first SPOT_CHECKS of the queue's random
 * draws (`keys`, which keep their places once labelled here), the re-checks
 * offered to this person, and the random checks' precision so far.
 */
export function SpotChecks({
  queue: q,
  load,
  keys,
  cards,
}: {
  queue: ReviewQueue;
  load: FoundryLoad;
  keys: readonly string[];
  cards: ReadonlyMap<string, QueueCard>;
}) {
  const { role, user } = useWorkspace();
  const drawn = keys.slice(0, SPOT_CHECKS).flatMap((k) => {
    const c = cards.get(k);
    return c &&
      (c.kind === "claim" || c.kind === "decided" || c.kind === "quote")
      ? [c]
      : [];
  });
  return (
    <>
      {drawn.length > 0 && (
        <ol className="rq-list" aria-label="Spot checks">
          <SpotCards
            cards={drawn}
            load={load}
            naming={q.naming}
            labels={q.labels.data ?? []}
            labelled={q.labelled}
            quotes={q.unlabelled}
            settled={q.settled}
            labelledOn={q.labelledOn}
          />
        </ol>
      )}
      <Rechecks
        load={load}
        naming={q.naming}
        reviews={q.reviews ?? []}
        // Newest first, so claims long gone don't hold the places.
        items={rechecks(q.decisions.data ?? [], q.labels.data ?? [], user.id)
          .toReversed()
          .slice(0, RECHECKS_READ)}
        manage={canManage(role)}
      />
      <PrecisionTable labels={q.labels.data ?? []} />
    </>
  );
}

/** A quote card's fact: its quote, and its ends as the read found them. */
type Pick = {
  quote: FactQuote;
  from?: EntityRow;
  to?: EntityRow;
  ends: "read" | "reading" | "failed";
};

/**
 * The random draws. The facts drawn are read at the offsets their draws
 * give (quoteOffsets): each the first quote of its band no one has labelled
 * at random and no earlier card shows, kept once labelled here.
 */
function SpotCards({
  cards,
  load,
  naming,
  labels,
  labelled,
  quotes,
  settled,
  labelledOn,
}: {
  cards: (ClaimCard | DecidedCard | QuoteCard)[];
  load: FoundryLoad;
  naming: Naming;
  labels: readonly FoundryReviewLabel[];
  labelled: ReadonlySet<string>;
  quotes: Partial<Record<string, number>>;
  settled: (key: string) => void;
  labelledOn: (key: string) => void;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const { readLater } = useTenantReads();
  const scope = { id: load.id };
  const draws = cards.filter((c): c is QuoteCard => c.kind === "quote");
  const offsetsOf = (c: QuoteCard) =>
    quoteOffsets(load.id, c.band, c.draw, quotes[c.band] ?? 0);
  // One read a card.
  const reads = useQueries({
    queries: draws.map((c) => ({
      ...readQueries.rangeQuotes(
        ws,
        o,
        readLater,
        scope,
        BAND_RANGE[c.band],
        offsetsOf(c),
      ),
      enabled: offsetsOf(c).length > 0,
    })),
  });
  // The quotes labelled here keep their cards, whatever is read since.
  const [mine, setMine] = useState<ReadonlyMap<string, FactQuote>>(new Map());
  const chosen = new Map<string, FactQuote>();
  const used = new Set<string>();
  draws.forEach((c, i) => {
    const pick =
      mine.get(c.key) ??
      (reads[i]?.data ?? []).find((q): q is FactQuote => {
        if (!q) return false;
        const at = claimPlace(q, q.link);
        return !used.has(at) && !labelled.has(at);
      });
    if (pick) {
      chosen.set(c.key, pick);
      used.add(claimPlace(pick, pick.link));
    }
  });
  const picked = [...chosen.values()];
  const ends = useQuery({
    ...readQueries.quoteFacts(ws, o, readLater, scope, picked, naming),
    enabled: picked.length > 0,
  });
  return cards.map((card) => {
    switch (card.kind) {
      case "claim":
        return (
          <li key={card.key} className="rq-card" data-drawn={card.drawn}>
            <CardHead title="A held claim" random />
            <Evidence at={card.review} quote={card.review.quote} />
            <p className="rq-writes">{RANDOM_NOTE}</p>
            <Score confidence={card.review.confidence} />
            <HeldHow review={card.review} />
            <SettleReview
              review={card.review}
              load={load}
              link={(path) => path}
              naming={naming}
              drawn={card.drawn}
              onSettled={() => settled(card.key)}
            />
          </li>
        );
      case "decided":
        return (
          <DecidedItem
            key={card.key}
            card={card}
            labels={labels}
            labelledOn={labelledOn}
          />
        );
      case "quote": {
        const i = draws.indexOf(card),
          q = chosen.get(card.key);
        const fact = q ? ends.data?.[picked.indexOf(q)] : undefined;
        return (
          <li key={card.key} className="rq-card">
            <CardHead
              title={
                card.band === "0.9" ? "An accepted fact" : "An approved fact"
              }
              random
            />
            {q ? (
              <QuoteFact
                pick={{
                  quote: q,
                  from: fact?.from,
                  to: fact?.to,
                  ends: ends.isPending
                    ? "reading"
                    : ends.error
                      ? "failed"
                      : "read",
                }}
                band={card.band}
                label={labels.find(
                  (l) =>
                    l.drawn === "random" &&
                    labelPlace(l) === claimPlace(q, q.link),
                )}
                onLabelled={() => {
                  setMine((now) => new Map(now).set(card.key, q));
                  labelledOn(card.key);
                }}
              />
            ) : reads[i]?.isPending ? (
              <SkeletonText lines={2} />
            ) : (
              <p className="on-muted">
                {reads[i]?.error
                  ? `Couldn’t read this draw: ${reads[i]!.error!.message}`
                  : "Every fact this draw read is labelled already, or shown above."}
              </p>
            )}
          </li>
        );
      }
    }
  });
}

const RANDOM_NOTE =
  "Drawn at random from its score band, to measure how often claims scored so are right: approve it only if the quote says it, reject it if not.";

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
  const qc = useQueryClient();
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
      await qc.invalidateQueries({
        queryKey: labelQueries.list(ws, o).queryKey,
      });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPending(false);
    }
  };
  return (
    <>
      <div className="hf-form-actions">
        <Button
          size="sm"
          variant="ghost"
          disabled={pending}
          onClick={() => void say("wrong")}
        >
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

/** A fact the graph holds, drawn at random: Right or Wrong, a label of it. */
function QuoteFact({
  pick,
  band,
  label,
  onLabelled,
}: {
  pick: Pick;
  band: Band;
  label?: FoundryReviewLabel;
  onLabelled: () => void;
}) {
  const { user } = useWorkspace();
  const { quote: q } = pick;
  const gone = pick.ends === "read" && !pick.from && !pick.to;
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
            <ClaimLine
              from={pick.from?.name}
              link={q.link}
              to={pick.to?.name}
            />
            .
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
      {pick.ends === "failed" && (
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
 * A held claim someone decided, drawn at random: what it states, what was
 * decided, and Right or Wrong, a label of it; the decision stands.
 */
function DecidedItem({
  card,
  labels,
  labelledOn,
}: {
  card: DecidedCard;
  labels: readonly FoundryReviewLabel[];
  labelledOn: (key: string) => void;
}) {
  const { id: ws, user } = useWorkspace(),
    { id: o } = useOntology();
  const decisions = useQuery(reviewQueries.list(ws, o));
  const s = card.review.settle;
  if (s?.kind !== "release") return null;
  const approved = approvalOf(decisions.data ?? [], s, true).saved,
    rejected = rejectionOf(decisions.data ?? [], s);
  const place = claimPlace(card.review, card.review.suggestedLink);
  return (
    <li className="rq-card">
      <CardHead
        title={card.blocked ? "A claim no approval writes" : "A decided claim"}
        random
      />
      <Evidence at={card.review} quote={card.review.quote} />
      <p className="rq-writes">
        It states {statementLine(s.statement, s.link)}.{" "}
        {card.blocked
          ? "It is read more than one way here, or a property of it differs from the fact the graph holds, so no approval writes it."
          : rejected
            ? "Someone rejected it."
            : approved
              ? "Someone approved it."
              : "It is decided."}{" "}
        Drawn at random from its score band, to measure how often claims scored
        so are right. Your word is kept as a label, and changes nothing else.
      </p>
      <Score confidence={card.review.confidence} />
      <LabelButtons
        own={labels.find(
          (l) =>
            l.drawn === "random" &&
            l.decidedByUserId === user.id &&
            labelPlace(l) === place,
        )}
        request={(verdict) => ({
          reviewId: s.reviewId,
          verdict,
          drawn: "random",
        })}
        onLabelled={() => labelledOn(card.key)}
      />
    </li>
  );
}

/**
 * The re-checks offered to this person: another's decision or label, drawn
 * at random (rechecks), each with its quote and what the claim states, what
 * was said of it, and Right or Wrong, a label that changes nothing. Only
 * those whose claim the current load still holds are offered.
 */
function Rechecks({
  load,
  naming,
  reviews,
  items,
  manage,
}: {
  load: FoundryLoad;
  naming: Naming;
  reviews: readonly QueueReview[];
  items: Recheck[];
  manage: boolean;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const { readLater } = useTenantReads();
  const scope = { id: load.id };
  const at = (r: Recheck) =>
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
  const same = (
    a: Span & { documentId: string },
    b: Span & { documentId: string },
  ) =>
    a.documentId === b.documentId &&
    a.section === b.section &&
    a.start === b.start &&
    a.end === b.end;
  // The review of a decision's claim, or of a held claim's label, while the
  // load holds it back, stating what was decided (a rebuild may read the
  // place otherwise); else the fact's quote at its place.
  const reviewOf = (r: Recheck) =>
    reviews.find(
      (v) =>
        v.settle?.kind === "release" &&
        same(v, at(r)) &&
        v.settle.hash ===
          (r.kind === "decision"
            ? r.decision.statementHash
            : r.label.statementHash),
    );
  const documents = [
    ...new Set(items.filter((r) => !reviewOf(r)).map((r) => at(r).documentId)),
  ];
  const quotes = useQuery({
    ...readQueries.documentQuotes(ws, o, readLater, scope, documents),
    enabled: documents.length > 0,
  });
  // A decision keeps no link: its quote is the place's only one, or none.
  const quoteOf = (r: Recheck) => {
    const here = (quotes.data ?? []).filter(
      (q) =>
        same(q, at(r)) &&
        (r.kind === "label" && r.label.link ? q.link === r.label.link : true),
    );
    return new Set(here.map((q) => q.link)).size === 1 ? here[0] : undefined;
  };
  const shown = items
    .map((r) => {
      const review = reviewOf(r);
      return { r, review, quote: review ? undefined : quoteOf(r) };
    })
    .filter((x) => x.review || x.quote)
    .slice(0, RECHECKS_SHOWN);
  const facts = shown.flatMap((x) => (x.quote ? [x.quote] : []));
  const ends = useQuery({
    ...readQueries.quoteFacts(ws, o, readLater, scope, facts, naming),
    enabled: facts.length > 0,
  });
  if (!shown.length) return null;
  return (
    <Panel
      title="Re-checks"
      subtitle="Optional: a few of other people’s queue decisions and random checks, drawn at random, for a second person to say whether they’re right. Your word changes nothing; it measures how often people agree."
    >
      <ol className="rq-list" aria-label="Re-checks">
        {shown.map(({ r, review, quote }) => {
          const s =
            review?.settle?.kind === "release" ? review.settle : undefined;
          const fact = quote ? ends.data?.[facts.indexOf(quote)] : undefined;
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
      <div className="hf-form-actions">
        <Button
          size="sm"
          variant="ghost"
          disabled={pending}
          onClick={() => void say("wrong")}
        >
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
function PrecisionTable({ labels }: { labels: readonly FoundryReviewLabel[] }) {
  const p = precision(labels);
  if (!p.bands.length && !p.rechecks.total) return null;
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
          {p.bands.map((b) => (
            <tr key={b.band}>
              <th scope="row">{BAND_TEXT[b.band]}</th>
              <td>{formatNumber(b.right)}</td>
              <td>{formatNumber(b.total)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {p.rechecks.total > 0 && (
        <p className="on-muted">
          Re-checks: a second person agreed {formatNumber(p.rechecks.agreed)} of{" "}
          {formatNumber(p.rechecks.total)} times.
        </p>
      )}
    </Panel>
  );
}
