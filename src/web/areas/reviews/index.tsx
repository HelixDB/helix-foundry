// The review queue, lazy-loaded by the Ontology area's route table under
// /ontology/:ontologyId/review: every open review of the ontology's current
// load on one page, the decisions that write the most facts first
// (./queue.ts buildQueue), with about one card in ten drawn at random from a
// score band to measure how often claims at each score are right. Each card
// shows the evidence (the quote, its document and where) before what it
// writes and the verifier's score, then the controls a document's Needs
// review has (./settle.tsx SettleReview): approve, reject, link a name,
// choose a value, each as the backend settles it, saying how the queue drew
// the card. A claim drawn at random that is already decided, or a fact the
// graph holds, takes Right or Wrong, a label that changes nothing. A few
// decisions and labels of other people come back as optional re-checks.
// There is no Approve all here: a person reads every card.
import "./reviews.css";
import { useMemo, useRef, useState, type ReactNode } from "react";
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
  SettleReviewLabelRequest,
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
import { useReadAccess } from "../explore/graph-reader";
import {
  QUEUE_READ,
  type EntityRow,
  type FactQuote,
  type Span,
} from "../explore/model";
import type { Naming } from "../explore/naming";
import { readQueries } from "../explore/reads";
import { useTenantReads } from "../explore/tenant-reads";
import { useNaming } from "../explore/use-naming";
import { ontologyQueries } from "../ontology/queries";
import { ontologyName, useOntology } from "../ontology/scope";
import { labelQueries, reviewQueries, settleLabel } from "./api";
import { approvalOf, rejectionOf, type Statement } from "./model";
import {
  BAND_RANGE,
  BAND_TEXT,
  bandOf,
  buildQueue,
  claimPlace,
  labelPlace,
  precision,
  quoteOffsets,
  rechecks,
  scoreText,
  type Band,
  type ClaimCard,
  type DecidedCard,
  type NameCard,
  type QueueCard,
  type QueueReview,
  type QuoteCard,
  type Recheck,
} from "./queue";
import { SettleReview } from "./settle";

/** How many cards the page shows at first, and how many more each time. */
const SHOWN = 20;
/** How many re-checks the page offers at once, of how many it reads. */
const RECHECKS_SHOWN = 5;
const RECHECKS_READ = 20;

/**
 * A review once, wherever pages read at different times put it: its key,
 * or for one no decision settles, all it says.
 */
const reviewKey = (r: QueueReview) =>
  r.settle && r.settle.kind !== "stale"
    ? r.settle.reviewId
    : JSON.stringify([
        r.documentId,
        r.section,
        r.start,
        r.end,
        r.reason,
        r.suggestedLink ?? null,
        r.prop ?? null,
        r.quote,
      ]);

/**
 * A load's pages of reviews as one: all of them once every page is read,
 * each review once (offset pages read at different times may overlap),
 * else the first failure. Stable, so TanStack combines again only when a
 * page changes.
 */
const combinePages = (results: UseQueryResult<QueueReview[]>[]) => {
  const seen = new Set<string>();
  return {
    data: results.every((r) => r.data)
      ? results
          .flatMap((r) => r.data ?? [])
          .filter((r) => {
            const key = reviewKey(r);
            if (seen.has(key)) return false;
            seen.add(key);
            return true;
          })
      : undefined,
    read: results.filter((r) => r.data).length,
    error: results.find((r) => r.error)?.error ?? null,
    refetch: () => results.forEach((r) => void r.refetch()),
  };
};

export function ReviewQueuePage() {
  const { id: ws, role, user } = useWorkspace(),
    { id: o, ontology } = useOntology();
  useTitle("Review queue");
  const qc = useQueryClient();
  const access = useReadAccess();
  const loads = useQuery(ontologyQueries.loads(ws, o));
  // Only the current load is settled, so the queue is always its.
  const load = loads.data?.loads?.find((l) => l.current);
  const loadId = load?.id ?? "";
  const scope = useMemo(() => ({ id: loadId }), [loadId]);
  const { read, readLater } = useTenantReads();
  const { naming, ready } = useNaming(load?.versionId || "");
  const reads = access === "database" && !!loadId && !!ontology.tenantId;
  const count = useQuery({
    ...readQueries.queueReviewCount(ws, o, read, scope),
    enabled: reads,
  });
  // The quotes of facts in each band, which the random slice draws from: a
  // walk of the load's sections no index serves, so it waits its turn, and
  // the ranked cards don't wait for it.
  const quotes = useQuery({
    ...readQueries.quoteCounts(ws, o, readLater, scope, BAND_RANGE),
    enabled: reads,
  });
  const total = count.data ?? 0,
    pages = Math.ceil(Math.min(total, QUEUE_READ.reviews) / QUEUE_READ.page);
  // The pages wait their turn at the database's limiter.
  const reviewPages = useQueries({
    queries: Array.from({ length: pages }, (_, page) => ({
      ...readQueries.queueReviews(ws, o, readLater, scope, page, naming),
      enabled: reads && ready && count.data !== undefined,
    })),
    combine: combinePages,
  });
  // A whole read is every page of a counted load; while pages are read
  // again (a job ended, or the count crossed a page), the last one stands.
  const whole = count.data !== undefined ? reviewPages.data : undefined;
  const last = useRef<{ load: string; reviews: QueueReview[] }>(undefined);
  if (whole) last.current = { load: loadId, reviews: whole };
  const reviews =
    whole ?? (last.current?.load === loadId ? last.current.reviews : undefined);
  const decisions = useQuery(reviewQueries.list(ws, o));
  const labels = useQuery(labelQueries.list(ws, o));
  // Left out of the random slice: claims labelled at random, and any this
  // person has said something of (a re-check too, which a label would
  // replace).
  const labelled = useMemo(
    () =>
      new Set(
        (labels.data ?? [])
          .filter((l) => l.drawn === "random" || l.decidedByUserId === user.id)
          .map(labelPlace),
      ),
    [labels.data, user.id],
  );
  // Each band's quotes not yet labelled at random.
  const unlabelled = useMemo(() => {
    if (!quotes.data) return {};
    const taken = new Map<string, number>();
    for (const l of labels.data ?? [])
      if (l.drawn === "random" && l.factKey) {
        const b = bandOf(l.confidence);
        taken.set(b, (taken.get(b) ?? 0) + 1);
      }
    return Object.fromEntries(
      Object.entries(quotes.data).map(([b, n]) => [
        b,
        Math.max(0, n - (taken.get(b) ?? 0)),
      ]),
    );
  }, [quotes.data, labels.data]);
  // The cards decided here, and those labelled here, keep their places
  // until the page is read again.
  const [keep, setKeep] = useState<ReadonlySet<string>>(new Set());
  const [labelledHere, setLabelledHere] = useState<ReadonlySet<string>>(
    new Set(),
  );
  const [shown, setShown] = useState(SHOWN);
  // The quote counts are waited for unless they failed: then the random
  // slice draws from held claims alone, and the page says so.
  const quotesSettled = !!quotes.data || !!quotes.error;
  const queue = useMemo(
    () =>
      reviews && decisions.data && labels.data && quotesSettled && loadId
        ? buildQueue({
            reviews,
            decisions: decisions.data,
            single: (link) => !naming.version || naming.oneValued.has(link),
            quotes: unlabelled,
            labelled,
            seed: loadId,
            keep,
            labelledHere,
          })
        : undefined,
    [
      reviews,
      decisions.data,
      labels.data,
      labelled,
      unlabelled,
      quotesSettled,
      loadId,
      keep,
      labelledHere,
      naming,
    ],
  );
  const refreshLabels = () =>
    void qc.invalidateQueries({ queryKey: labelQueries.list(ws, o).queryKey });
  const settled = (key: string) => {
    setKeep((now) => new Set(now).add(key));
    refreshLabels();
  };
  const labelledOn = (key: string) => {
    setLabelledHere((now) => new Set(now).add(key));
    refreshLabels();
  };
  const header = (
    <PageHeader
      breadcrumbs={[
        { label: "Ontologies", to: paths.ontologies() },
        { label: ontologyName(ontology), to: paths.ontology(o) },
        { label: "Review queue" },
      ]}
      title="Review queue"
      subtitle="Every open review of the current load, the decisions that write the most facts first. About one card in ten is drawn at random from a score band, to measure how often claims at each score are right."
    />
  );
  const shell = (body: ReactNode) => (
    <Page>
      {header}
      {body}
    </Page>
  );
  if (access !== "database")
    return shell(
      <EmptyState
        icon={ListChecks}
        title="Its reviews aren’t shared with you"
        text="The review queue reads the ontology’s database, which you don’t query."
      />,
    );
  if (loads.isPending) return shell(<SkeletonText lines={6} />);
  if (!loads.data)
    return shell(
      <EmptyState
        icon={ListChecks}
        title="Couldn’t read the loads"
        text={loads.error?.message}
        action={<Button onClick={() => void loads.refetch()}>Retry</Button>}
      />,
    );
  if (!ontology.tenantId)
    return shell(
      <EmptyState
        icon={ListChecks}
        title="Its database isn’t ready"
        text="The review queue reads the ontology’s database, which this page can’t reach yet."
      />,
    );
  if (!load)
    return shell(
      <EmptyState
        icon={ListChecks}
        title="Nothing to review yet"
        text="Load the ontology: its reviews are read from the current load."
        action={
          <Link to={paths.ontology(o)} className="hf-link">
            Overview
          </Link>
        }
      />,
    );
  const failed =
    count.error || reviewPages.error || decisions.error || labels.error;
  const retry = () => {
    void count.refetch();
    void quotes.refetch();
    reviewPages.refetch();
    void decisions.refetch();
    void labels.refetch();
  };
  if (!queue)
    return shell(
      failed && !count.isFetching ? (
        <EmptyState
          icon={ListChecks}
          title="Couldn’t read the reviews"
          text={failed.message}
          action={<Button onClick={retry}>Retry</Button>}
        />
      ) : (
        <>
          {pages > 1 && (
            <p role="status" className="on-muted">
              Reading the load’s reviews: {reviewPages.read} of {pages} pages.
            </p>
          )}
          <SkeletonText lines={6} />
        </>
      ),
    );
  const manage = canManage(role);
  return (
    <Page>
      {header}
      {failed && (
        <p role="status" className="hf-refresh-failed">
          Couldn’t read the latest reviews ({failed.message}): this is the last
          read.{" "}
          <Button size="sm" variant="ghost" onClick={retry}>
            Retry
          </Button>
        </p>
      )}
      {quotes.error && (
        <p role="status" className="hf-refresh-failed">
          Couldn’t count the facts the graph holds ({quotes.error.message}): the
          random checks draw from held claims alone.{" "}
          <Button
            size="sm"
            variant="ghost"
            onClick={() => void quotes.refetch()}
          >
            Retry
          </Button>
        </p>
      )}
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
              cards={queue.cards.slice(0, shown)}
              load={load}
              naming={naming}
              labels={labels.data ?? []}
              labelled={labelled}
              quotes={unlabelled}
              settled={settled}
              labelledOn={labelledOn}
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
        naming={naming}
        reviews={reviews ?? []}
        // Newest first, so claims long gone don't hold the places.
        items={rechecks(decisions.data ?? [], labels.data ?? [], user.id)
          .toReversed()
          .slice(0, RECHECKS_READ)}
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
          first {formatNumber(QUEUE_READ.reviews)} the database keeps, decided
          ones and those no decision settles included, and its random checks
          draw held claims from those alone. The rest show on their documents’
          pages.
        </p>
      )}
      {!manage && <p className="on-muted">Owners and admins settle reviews.</p>}
    </div>
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
 * The cards on the page. The facts drawn at random are read at the offsets
 * their draws give (quoteOffsets): each the first quote of its band no one
 * has labelled at random and no earlier card shows, kept once labelled here.
 */
function QueueCards({
  cards,
  load,
  naming,
  labels,
  labelled,
  quotes,
  settled,
  labelledOn,
}: {
  cards: QueueCard[];
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
  // One read a card, so showing more reads only the new ones.
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
      case "decided":
        return (
          <DecidedItem
            key={card.key}
            card={card}
            labels={labels}
            labelledOn={labelledOn}
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
            <Evidence at={card.review} quote={card.review.quote} />
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
  at,
  quote,
}: {
  at: Span & { documentId: string };
  quote: string;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const name = useDocumentNames(ws, "a document");
  const text = quote.trim();
  return (
    <div className="rq-evidence">
      {text ? <blockquote>“{text}”</blockquote> : null}
      <p>
        <Link to={paths.document(o, at.documentId, "review", at)}>
          {name(at.documentId)}
        </Link>{" "}
        · section {at.section + 1}
      </p>
    </div>
  );
}

/** What a claim states: its ends, as named, and its link. */
function ClaimLine({
  from,
  link,
  to,
}: {
  from?: string;
  link: string;
  to?: string;
}) {
  return (
    <>
      <strong>{from ?? "…"}</strong> <span aria-label="to">→</span>{" "}
      <code>{link}</code> <span aria-label="to">→</span>{" "}
      <strong>{to ?? "…"}</strong>
    </>
  );
}
const statementLine = (s: Statement, link: string) => (
  <ClaimLine from={s.subject.name} link={link} to={s.object.name} />
);

/** The verifier's score, after the evidence: what the model said. */
function Score({ confidence }: { confidence?: number }) {
  return (
    <p className="rq-score on-muted">
      {confidence === undefined || bandOf(confidence) === "none"
        ? "The verifier gave it no score."
        : `Verifier score ${scoreText(confidence)} (${BAND_TEXT[bandOf(confidence)]}).`}
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
      <Evidence at={card.review} quote={card.review.quote} />
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
      <Evidence at={card.review} quote={card.review.quote} />
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
      {held > 0 && (
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
