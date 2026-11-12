// The review queue, lazy-loaded by the Ontology area's route table under
// /ontology/:ontologyId/review: every open review of the ontology's current
// load on one page, the decisions that write the most facts first
// (./queue.ts buildQueue), with about one card in ten drawn at random from a
// score band, the accepted facts included, to measure how often claims at
// each score are right. Each card shows the evidence (the quote, its
// document and where) before what it writes and the verifier's score, then
// the controls a document's Needs review has (./settle.tsx SettleReview):
// approve, reject, link a name, choose a value, each as the backend settles
// it, saying how the queue drew the card. An accepted fact drawn at random
// takes Right or Wrong, a label that changes nothing. A few decisions and
// labels of other people come back as optional re-checks. There is no
// Approve all here: a person reads every card.
import "./reviews.css";
import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  useQueries,
  useQuery,
  useQueryClient,
  type UseQueryResult,
} from "@tanstack/react-query";
import { ListChecks } from "lucide-react";
import type {
  FoundryLoad,
  FoundryReviewLabel,
  ReviewVerdict,
} from "../../../shared/wfe";
import {
  Button,
  EmptyState,
  Page,
  PageHeader,
  Panel,
  SkeletonText,
  Tag,
  formatNumber,
  plural,
  useTitle,
} from "../../kit";
import { paths } from "../../paths";
import { canManage, useWorkspace } from "../../ui";
import { useDocumentNames } from "../documents/documents-api";
import { QUEUE_READ, type AcceptedQuote, type Span } from "../explore/model";
import type { Naming } from "../explore/naming";
import { readQueries } from "../explore/reads";
import { useTenantReads } from "../explore/tenant-reads";
import { useNaming } from "../explore/use-naming";
import { ontologyQueries } from "../ontology/queries";
import { ontologyName, useOntology } from "../ontology/scope";
import { labelQueries, reviewQueries, settleLabel } from "./api";
import {
  BAND_TEXT,
  acceptedOffsets,
  bandOf,
  buildQueue,
  labelPlace,
  precision,
  rechecks,
  type ClaimCard,
  type NameCard,
  type QueueCard,
  type QueueReview,
  type Recheck,
} from "./queue";
import { SettleReview } from "./settle";

/**
 * A load's pages of reviews as one: all of them once every page is read,
 * else the first failure. Stable, so TanStack combines again only when a
 * page changes.
 */
const combinePages = (results: UseQueryResult<QueueReview[]>[]) => ({
  data: results.every((r) => r.data)
    ? results.flatMap((r) => r.data ?? [])
    : undefined,
  error: results.find((r) => r.error)?.error ?? null,
  refetch: () => results.forEach((r) => void r.refetch()),
});

/** How many cards the page shows at first, and how many more each time. */
const SHOWN = 20;
/** How many re-checks the page offers at once. */
const RECHECKS_SHOWN = 5;

export function ReviewQueuePage() {
  const { id: ws, role, user } = useWorkspace(),
    { id: o, ontology } = useOntology();
  useTitle("Review queue");
  const qc = useQueryClient();
  const loads = useQuery(ontologyQueries.loads(ws, o));
  // Only the current load is settled, so the queue is always its.
  const load = loads.data?.loads?.find((l) => l.current);
  const scope = useMemo(() => (load ? { id: load.id } : null), [load]);
  const { read } = useTenantReads();
  const { naming, ready } = useNaming(load?.versionId || "");
  const counts = useQuery({
    ...readQueries.queueCounts(ws, o, read, scope ?? { id: "" }),
    enabled: !!scope && !!ontology.tenantId,
  });
  const total = counts.data?.reviews ?? 0,
    pages = Math.ceil(Math.min(total, QUEUE_READ.reviews) / QUEUE_READ.page);
  // Every page, as one: the reviews once all are read, else the first
  // failure.
  const reviewPages = useQueries({
    queries: Array.from({ length: pages }, (_, page) => ({
      ...readQueries.queueReviews(
        ws,
        o,
        read,
        scope ?? { id: "" },
        page,
        naming,
      ),
      enabled: !!scope && ready,
    })),
    combine: combinePages,
  });
  const decisions = useQuery(reviewQueries.list(ws, o));
  const labels = useQuery(labelQueries.list(ws, o));
  // The cards decided here keep their places until the page is read again.
  const [keep, setKeep] = useState<ReadonlySet<string>>(new Set());
  const [shown, setShown] = useState(SHOWN);
  const reviews = reviewPages.data;
  const single = (link: string) =>
    !naming.version || naming.oneValued.has(link);
  const queue = useMemo(
    () =>
      reviews && decisions.data && load
        ? buildQueue({
            reviews,
            decisions: decisions.data,
            single,
            accepted: counts.data?.accepted ?? 0,
            seed: load.id,
            keep,
          })
        : undefined,
    [reviews, decisions.data, load, counts.data, keep, naming],
  );
  const settled = (key: string) => {
    setKeep((now) => new Set(now).add(key));
    void qc.invalidateQueries({ queryKey: labelQueries.list(ws, o).queryKey });
  };
  const header = (
    <PageHeader
      breadcrumbs={[
        { label: "Ontologies", to: paths.ontologies() },
        { label: ontologyName(ontology), to: paths.ontology(o) },
        { label: "Review queue" },
      ]}
      title="Review queue"
      subtitle="Every open review of the current load, the decisions that write the most facts first. About one card in ten is drawn at random from a score band, accepted facts included, to measure how often claims at each score are right."
    />
  );
  if (loads.isPending || (scope && (counts.isPending || !ready)))
    return (
      <Page>
        {header}
        <SkeletonText lines={6} />
      </Page>
    );
  if (!load)
    return (
      <Page>
        {header}
        <EmptyState
          icon={ListChecks}
          title="Nothing to review yet"
          text="Load the ontology: its reviews are read from the current load."
          action={
            <Link to={paths.ontology(o)} className="hf-link">
              Overview
            </Link>
          }
        />
      </Page>
    );
  const failed =
    counts.error || reviewPages.error || decisions.error || labels.error;
  if (failed && !queue)
    return (
      <Page>
        {header}
        <EmptyState
          icon={ListChecks}
          title="Couldn’t read the reviews"
          text={failed.message}
          action={
            <Button
              onClick={() => {
                void counts.refetch();
                reviewPages.refetch();
                void decisions.refetch();
                void labels.refetch();
              }}
            >
              Retry
            </Button>
          }
        />
      </Page>
    );
  if (!queue)
    return (
      <Page>
        {header}
        <SkeletonText lines={6} />
      </Page>
    );
  const manage = canManage(role);
  const cards = queue.cards.slice(0, shown);
  return (
    <Page>
      {header}
      <QueueSummary
        queue={queue}
        total={total}
        manage={manage}
        random={queue.cards.filter((c) => c.drawn === "random").length}
      />
      {queue.cards.length === 0 ? (
        <EmptyState
          icon={ListChecks}
          title="Nothing left to decide"
          text="Every review of the current load is decided, or no decision here settles it."
        />
      ) : (
        <>
          <ol className="rq-list" aria-label="Review queue">
            <QueueCards
              cards={cards}
              load={load}
              naming={naming}
              labels={labels.data ?? []}
              accepted={counts.data?.accepted ?? 0}
              settled={settled}
            />
          </ol>
          {queue.cards.length > shown && (
            <Button onClick={() => setShown(shown + SHOWN)}>
              Show {Math.min(SHOWN, queue.cards.length - shown)} more of{" "}
              {formatNumber(queue.cards.length - shown)}
            </Button>
          )}
        </>
      )}
      <Rechecks
        load={load}
        reviews={reviews ?? []}
        items={rechecks(decisions.data ?? [], labels.data ?? [], user.id).slice(
          0,
          RECHECKS_SHOWN,
        )}
        manage={manage}
      />
      <PrecisionTable labels={labels.data ?? []} />
    </Page>
  );
}

/** What the queue holds, how much of it is random, and what it leaves out. */
function QueueSummary({
  queue,
  total,
  manage,
  random,
}: {
  queue: ReturnType<typeof buildQueue>;
  total: number;
  manage: boolean;
  random: number;
}) {
  const { left } = queue;
  const out = [
    left.decided && `${formatNumber(left.decided)} decided`,
    left.unsettled &&
      `${formatNumber(left.unsettled)} no decision settles here (a relationship the ontology lacks, or a check held for a person)`,
    left.blocked &&
      `${formatNumber(left.blocked)} no approval writes (read two ways, or a property that differs from its fact)`,
    left.stale && `${formatNumber(left.stale)} to settle after the next load`,
  ].filter(Boolean);
  return (
    <div className="rq-summary">
      <p>
        {plural(queue.cards.length, "card")}, {plural(random, "random check")}.
        {out.length > 0 && ` Not in the queue: ${out.join(" · ")}.`}
      </p>
      {total > QUEUE_READ.reviews && (
        <p role="note">
          This load holds {formatNumber(total)} reviews: the queue reads the
          first {formatNumber(QUEUE_READ.reviews)}. Decide those, and the next
          load lists the rest.
        </p>
      )}
      {!manage && <p className="on-muted">Owners and admins settle reviews.</p>}
    </div>
  );
}

/**
 * The cards on the page, the accepted facts drawn at random among them read
 * at the offsets their draws give: each the first quote no one has labelled
 * at random and no earlier card shows, kept once labelled here.
 */
function QueueCards({
  cards,
  load,
  naming,
  labels,
  accepted,
  settled,
}: {
  cards: QueueCard[];
  load: FoundryLoad;
  naming: Naming;
  labels: readonly FoundryReviewLabel[];
  accepted: number;
  settled: (key: string) => void;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const { read } = useTenantReads();
  const scope = { id: load.id };
  const draws = cards.flatMap((c) => (c.kind === "accepted" ? [c] : []));
  const offsets = draws.flatMap((c) =>
    acceptedOffsets(load.id, c.draw, accepted),
  );
  const quotes = useQuery({
    ...readQueries.acceptedQuotes(ws, o, read, scope, offsets),
    enabled: offsets.length > 0,
  });
  // The quotes labelled here keep their cards; any other a label names is
  // left for another draw.
  const [mine, setMine] = useState<ReadonlyMap<string, string>>(new Map());
  const taken = new Set(
    labels.filter((l) => l.drawn === "random" && l.factKey).map(labelPlace),
  );
  const chosen = new Map<string, AcceptedQuote>();
  if (quotes.data) {
    const used = new Set<string>();
    let at = 0;
    for (const c of draws) {
      const candidates = acceptedOffsets(load.id, c.draw, accepted).map(
        () => quotes.data[at++],
      );
      const kept = mine.get(c.key);
      const pick =
        candidates.find((q) => q && placeOf(q) === kept) ??
        candidates.find(
          (q) => q && !used.has(placeOf(q)) && !taken.has(placeOf(q)),
        );
      if (pick) {
        chosen.set(c.key, pick);
        used.add(placeOf(pick));
      }
    }
  }
  const picked = [...chosen.values()];
  const ends = useQuery({
    ...readQueries.acceptedFacts(ws, o, read, scope, picked, naming),
    enabled: picked.length > 0,
  });
  const link = (path: string) => path;
  return cards.map((card) => {
    switch (card.kind) {
      case "claim":
        return (
          <ClaimItem
            key={card.key}
            card={card}
            load={load}
            naming={naming}
            link={link}
            settled={settled}
          />
        );
      case "name":
        return (
          <NameItem
            key={card.key}
            card={card}
            load={load}
            naming={naming}
            link={link}
            settled={settled}
          />
        );
      case "value":
        return (
          <li key={card.key} className="rq-card">
            <CardHead title="A value documents state differently" />
            <Evidence review={card.review} />
            <p className="rq-writes">
              Writes one value of {card.review.prop || "a property"}.
            </p>
            <SettleReview
              review={card.review}
              load={load}
              link={link}
              naming={naming}
              drawn="ranked"
              onSettled={() => settled(card.key)}
            />
          </li>
        );
      case "accepted": {
        const q = chosen.get(card.key);
        const fact = q ? ends.data?.[picked.indexOf(q)] : undefined;
        return (
          <li key={card.key} className="rq-card">
            <CardHead title="An accepted fact" random />
            {q ? (
              <AcceptedFact
                quote={q}
                from={fact?.from?.name}
                to={fact?.to?.name}
                label={labels.find(
                  (l) => l.drawn === "random" && labelPlace(l) === placeOf(q),
                )}
                onLabelled={() => {
                  setMine((now) => new Map(now).set(card.key, placeOf(q)));
                  settled(card.key);
                }}
              />
            ) : quotes.isPending ? (
              <SkeletonText lines={2} />
            ) : (
              <p className="on-muted">
                {quotes.error
                  ? `Couldn’t read this draw: ${quotes.error.message}`
                  : "Every accepted fact this draw found is labelled already."}
              </p>
            )}
          </li>
        );
      }
    }
  });
}

/** An accepted quote's place and fact, as a label names it. */
const placeOf = (q: AcceptedQuote) =>
  labelPlace({
    docId: q.documentId,
    claimSection: q.section,
    claimStart: q.start,
    claimEnd: q.end,
    factKey: q.factKey,
  });

function CardHead({ title, random }: { title: string; random?: boolean }) {
  return (
    <div className="rq-head">
      <h3>{title}</h3>
      {random && <Tag tone="accent">Random check</Tag>}
    </div>
  );
}

/**
 * Where a card's claim is, before anything is said of it: its quote, its
 * document and its section, linked to the document's review at the quote.
 */
function Evidence({
  review,
  quote,
}: {
  review?: Pick<QueueReview, "documentId" | "quote"> & Span;
  quote?: AcceptedQuote;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const name = useDocumentNames(ws, "a document");
  const at = review ?? quote!,
    documentId = review?.documentId ?? quote!.documentId;
  const text = (review?.quote ?? quote?.quote ?? "").trim();
  return (
    <div className="rq-evidence">
      {text ? <blockquote>“{text}”</blockquote> : null}
      <p>
        <Link to={paths.document(o, documentId, "review", at)}>
          {name(documentId)}
        </Link>{" "}
        · section {at.section + 1}
      </p>
    </div>
  );
}

/** The verifier's score, after the evidence: what the model said. */
function Score({ confidence }: { confidence?: number }) {
  return (
    <p className="rq-score on-muted">
      {confidence === undefined
        ? "The verifier gave it no score."
        : `Verifier score ${confidence.toFixed(2)} (${BAND_TEXT[bandOf(confidence)]}).`}
    </p>
  );
}

const RANDOM_NOTE =
  "Drawn at random from its score band, to measure how often claims scored so are right: approve it only if the quote says it, reject it if not.";

function ClaimItem({
  card,
  load,
  naming,
  link,
  settled,
  nested,
}: {
  card: ClaimCard;
  load: FoundryLoad;
  naming: Naming;
  link: (path: string) => string;
  settled: (key: string) => void;
  nested?: boolean;
}) {
  const random = card.drawn === "random";
  const Item = nested ? "div" : "li";
  return (
    <Item className="rq-card" data-drawn={card.drawn}>
      <CardHead title="A held claim" random={random} />
      <Evidence review={card.review} />
      {random ? (
        <p className="rq-writes">{RANDOM_NOTE}</p>
      ) : (
        <p className="rq-writes">
          {nested
            ? "Approve it once the name is settled: the first load after that writes it."
            : "Writes one fact once approved."}
        </p>
      )}
      <Score confidence={card.review.confidence} />
      <SettleReview
        review={card.review}
        load={load}
        link={link}
        naming={naming}
        drawn={card.drawn}
        onSettled={() => settled(card.key)}
      />
    </Item>
  );
}

function NameItem({
  card,
  load,
  naming,
  link,
  settled,
}: {
  card: NameCard;
  load: FoundryLoad;
  naming: Naming;
  link: (path: string) => string;
  settled: (key: string) => void;
}) {
  const held = card.held.length;
  return (
    <li className="rq-card">
      <CardHead title="A name in a document" />
      <Evidence review={card.review} />
      <p className="rq-writes">
        {card.blocked > 0 &&
          `Settling it writes ${plural(card.blocked, "fact")} the load holds back for it. `}
        {held > 0 &&
          `${plural(held, "held claim")} ${held > 1 ? "wait" : "waits"} on it, each approved below. `}
        {!card.blocked && !held && "No claim waits on it. "}A name’s decision
        applies from the next load, not this one: that load writes what it
        unblocks.
      </p>
      <SettleReview
        review={card.review}
        load={load}
        link={link}
        naming={naming}
        drawn="ranked"
        onSettled={() => settled(card.key)}
      />
      {card.held.length > 0 && (
        <div className="rq-held" aria-label="Claims that wait on this name">
          {card.held.map((c) => (
            <ClaimItem
              key={c.key}
              card={c}
              load={load}
              naming={naming}
              link={link}
              settled={settled}
              nested
            />
          ))}
        </div>
      )}
    </li>
  );
}

/** An accepted fact drawn at random: Right or Wrong, a label of it. */
function AcceptedFact({
  quote,
  from,
  to,
  label,
  onLabelled,
}: {
  quote: AcceptedQuote;
  from?: string;
  to?: string;
  label?: FoundryReviewLabel;
  onLabelled: () => void;
}) {
  const { id: ws, role, user } = useWorkspace(),
    { id: o } = useOntology();
  const qc = useQueryClient();
  const [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const own = label?.decidedByUserId === user.id ? label : undefined;
  const say = async (verdict: ReviewVerdict) => {
    setPending(true);
    setError("");
    try {
      await settleLabel(ws, o, {
        fact: {
          docId: quote.documentId,
          section: quote.section,
          start: quote.start,
          end: quote.end,
          factKey: quote.factKey,
        },
        verdict,
        drawn: "random",
      });
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
      <Evidence quote={quote} />
      <p className="rq-writes">
        The graph holds it as a fact: <strong>{from ?? "…"}</strong>{" "}
        <span aria-label="to">→</span> <code>{quote.link}</code>{" "}
        <span aria-label="to">→</span> <strong>{to ?? "…"}</strong>. Drawn at
        random from accepted facts, to measure how often they’re right. Your
        word is kept as a label: it changes nothing in the graph.
      </p>
      <Score confidence={quote.confidence} />
      {own ? (
        <p role="status">
          You said it’s {own.verdict}. It’s counted in the random checks below.
        </p>
      ) : (
        canManage(role) && (
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
        )
      )}
      {error && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
    </>
  );
}

/**
 * The re-checks offered to this person: another's decision or label, drawn
 * at random (rechecks), each with its quote, what was said of it, and Right
 * or Wrong, a label that changes nothing.
 */
function Rechecks({
  load,
  reviews,
  items,
  manage,
}: {
  load: FoundryLoad;
  reviews: readonly QueueReview[];
  items: Recheck[];
  manage: boolean;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const { read } = useTenantReads();
  // A decision's quote is its review's while the load holds it; else (an
  // approval written) the load's quote at its place.
  const placeAt = (r: Recheck) =>
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
  const reviewOf = (r: Recheck) => reviews.find((v) => same(v, placeAt(r)));
  const documents = [
    ...new Set(
      items.filter((r) => !reviewOf(r)).map((r) => placeAt(r).documentId),
    ),
  ];
  const quotes = useQuery({
    ...readQueries.documentQuotes(ws, o, read, { id: load.id }, documents),
    enabled: documents.length > 0,
  });
  if (!items.length) return null;
  return (
    <Panel
      title="Re-checks"
      subtitle="Optional: a few of other people’s random checks and queue decisions, drawn at random, for a second person to say whether they’re right. Your word changes nothing; it measures how often people agree."
    >
      <ol className="rq-list" aria-label="Re-checks">
        {items.map((r) => {
          const review = reviewOf(r),
            quote = review
              ? undefined
              : quotes.data?.find((q) => same(q, placeAt(r)));
          return (
            <li key={r.key} className="rq-card">
              <CardHead title="A re-check" />
              {review ? (
                <Evidence review={review} />
              ) : quote ? (
                <Evidence quote={quote} />
              ) : quotes.isPending && documents.length ? (
                <SkeletonText lines={2} />
              ) : (
                <p className="on-muted">
                  This load holds no quote at its place any more.
                </p>
              )}
              <p className="rq-writes">
                {r.kind === "decision"
                  ? r.decision.kind === "release"
                    ? "Someone approved this claim."
                    : "Someone rejected this claim."
                  : `Someone said this accepted fact is ${r.label.verdict}.`}{" "}
                Is the claim right?
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
  const qc = useQueryClient();
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
          : {
              fact: {
                docId: item.label.docId,
                section: item.label.claimSection ?? 0,
                start: item.label.claimStart ?? 0,
                end: item.label.claimEnd ?? 0,
                factKey: item.label.factKey ?? "",
              },
              verdict,
              drawn: "recheck",
            },
      );
      setSaid(verdict);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPending(false);
    }
  };
  // Read again once this person has moved on: the re-check leaves the list.
  useEffect(
    () => () => {
      if (said)
        void qc.invalidateQueries({
          queryKey: labelQueries.list(ws, o).queryKey,
        });
    },
    [said, qc, ws, o],
  );
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
      subtitle="What people said of the claims drawn at random, by the verifier’s score. Only these measure precision: the ranked cards are not a fair sample."
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
