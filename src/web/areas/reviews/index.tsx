// The review queue, the Review page's Facts tab (./page.tsx, at
// /ontology/:ontologyId/review): every open review of the ontology's current
// load, the decisions that write the most facts first (./queue.ts
// buildQueue), walked one card at a time (./walkthrough.tsx), each card with
// Founder's suggestion from fixed rules (./advice.ts) and settled as a
// document's Needs review settles it. About one card in ten is drawn at
// random from a score band to measure how often claims at each score are
// right: those wait for the finish, as spot checks (./spot-checks.tsx), with
// a few re-checks of other people's decisions. The page reads the load here
// and holds the walk (./walkthrough.tsx useWalk), so the tab's count and the
// walk keep their place across a switch of tabs.
import "./reviews.css";
import { useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import {
  useQueries,
  useQuery,
  useQueryClient,
  type UseQueryResult,
} from "@tanstack/react-query";
import { ListChecks } from "lucide-react";
import { Button, EmptyState, SkeletonText, formatNumber } from "../../kit";
import { paths } from "../../paths";
import { canManage, useWorkspace } from "../../ui";
import { useReadAccess } from "../explore/graph-reader";
import { QUEUE_READ } from "../explore/model";
import { readQueries } from "../explore/reads";
import { useTenantReads } from "../explore/tenant-reads";
import { useNaming } from "../explore/use-naming";
import { ontologyQueries } from "../ontology/queries";
import { useOntology } from "../ontology/scope";
import { adviceContext } from "./advice";
import { labelQueries, reviewQueries } from "./api";
import {
  BAND_RANGE,
  bandOf,
  buildQueue,
  labelPlace,
  precision,
  queuePins,
  type QueuePins,
  type QueueReview,
} from "./queue";
import { undecided, walkOrder, type WalkFilter } from "./walk";
import { ReviewWalkthrough, useWalk } from "./walkthrough";

/** The walk's filter that takes every card. */
const EVERY: WalkFilter = {
  kinds: new Set(["name", "claim", "value"]),
  advice: "any",
};

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

/**
 * The Facts tab's reads of the current load and its queue. The Review page
 * holds them, so the tab's count shows on either tab and what was decided
 * here keeps its place across a switch. The queue keeps its random draws
 * and ranked cards for the visit (queuePins), so the walk's cards stay
 * the same while decisions are read. `open` is the cards still to decide:
 * the walk's Decide lane without a filter (names, held claims and values;
 * not those for information, names a link covers or the spot checks),
 * less those decided in this walk; unset until the queue is read.
 * `suggested` is how many of them Founder has a suggestion for.
 */
export function useReviewQueue() {
  const { id: ws, user } = useWorkspace(),
    { id: o, ontology } = useOntology();
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
  // The quote counts are waited for unless they failed: then the random
  // slice draws from held claims alone, and the page says so.
  const quotesSettled = !!quotes.data || !!quotes.error;
  const single = useMemo(
    () => (link: string) => !naming.version || naming.oneValued.has(link),
    [naming],
  );
  // What this visit's reads of the load drew and ranked. Each read adds its
  // own, which a read of the same inputs would draw and rank again.
  const pins = useRef<{ load: string; pins: QueuePins }>(undefined);
  const queue = useMemo(() => {
    if (
      !reviews ||
      !decisions.data ||
      !labels.data ||
      !quotesSettled ||
      !loadId
    )
      return undefined;
    const earlier =
      pins.current?.load === loadId ? pins.current.pins : undefined;
    const built = buildQueue({
      reviews,
      decisions: decisions.data,
      single,
      quotes: unlabelled,
      labelled,
      seed: loadId,
      keep,
      labelledHere,
      pins: earlier,
    });
    pins.current = { load: loadId, pins: queuePins(built, earlier) };
    return built;
  }, [
    reviews,
    decisions.data,
    labels.data,
    labelled,
    unlabelled,
    quotesSettled,
    loadId,
    keep,
    labelledHere,
    single,
  ]);
  // What Founder's rules read, and the walk of every card.
  const ctx = useMemo(
    () =>
      adviceContext({
        decisions: decisions.data ?? [],
        precision: precision(labels.data ?? []),
        single,
        user: user.id,
      }),
    [decisions.data, labels.data, single, user.id],
  );
  const all = useMemo(
    () => (queue ? walkOrder(queue, ctx, EVERY) : undefined),
    [queue, ctx],
  );
  const refreshLabels = () =>
    void qc.invalidateQueries({ queryKey: labelQueries.list(ws, o).queryKey });
  // A walk's choices keep their cards from when they're sent: a read of the
  // decisions meanwhile would drop them.
  const keepCards = (keys: readonly string[]) =>
    setKeep((now) => new Set([...now, ...keys]));
  const settled = (key: string) => {
    keepCards([key]);
    refreshLabels();
  };
  const labelledOn = (key: string) => {
    setLabelledHere((now) => new Set(now).add(key));
    refreshLabels();
  };
  const failed =
    count.error || reviewPages.error || decisions.error || labels.error;
  const retry = () => {
    void count.refetch();
    void quotes.refetch();
    reviewPages.refetch();
    void decisions.refetch();
    void labels.refetch();
  };
  const walk = useWalk();
  const outcomes =
    walk.state.loadId === loadId ? walk.state.outcomes : new Map();
  const open = all && undecided(all.decide, outcomes);
  const suggested =
    all &&
    undecided(
      all.decide.filter((k) => all.advice.get(k)?.kind === "suggest"),
      outcomes,
    );
  return {
    access,
    loads,
    load,
    naming,
    count,
    quotes,
    total,
    pages,
    reviewPages,
    reviews,
    decisions,
    labels,
    labelled,
    unlabelled,
    queue,
    single,
    ctx,
    all,
    kept: keep,
    keepCards,
    settled,
    labelledOn,
    failed,
    retry,
    open,
    suggested,
    walk,
  };
}

export type ReviewQueue = ReturnType<typeof useReviewQueue>;

/** The Review page's Facts tab: the queue `useReviewQueue` read, walked. */
export function FactsReview({ queue: q }: { queue: ReviewQueue }) {
  const { role } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const {
    access,
    loads,
    load,
    count,
    quotes,
    total,
    pages,
    reviewPages,
    queue,
    failed,
    retry,
  } = q;
  if (access !== "database")
    return (
      <EmptyState
        icon={ListChecks}
        title="Its reviews aren’t shared with you"
        text="Its reviews are read from the ontology’s database, which you don’t query."
      />
    );
  if (loads.isPending) return <SkeletonText lines={6} />;
  if (!loads.data)
    return (
      <EmptyState
        icon={ListChecks}
        title="Couldn’t read the loads"
        text={loads.error?.message}
        action={<Button onClick={() => void loads.refetch()}>Retry</Button>}
      />
    );
  if (!ontology.tenantId)
    return (
      <EmptyState
        icon={ListChecks}
        title="Its database isn’t ready"
        text="Its reviews are read from the ontology’s database, which this page can’t reach yet."
      />
    );
  if (!load)
    return (
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
    );
  if (!queue)
    return failed && !count.isFetching ? (
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
    );
  return (
    <>
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
          spot checks draw from held claims alone.{" "}
          <Button
            size="sm"
            variant="ghost"
            onClick={() => void quotes.refetch()}
          >
            Retry
          </Button>
        </p>
      )}
      <QueueSummary queue={queue} total={total} manage={canManage(role)} />
      <ReviewWalkthrough queue={q} load={load} cards={queue} />
    </>
  );
}

/** What the queue leaves out, and who settles it. */
function QueueSummary({
  queue,
  total,
  manage,
}: {
  queue: ReturnType<typeof buildQueue>;
  total: number;
  manage: boolean;
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
  if (!out.length && total <= QUEUE_READ.reviews && manage) return null;
  return (
    <div className="rq-summary">
      {out.length > 0 && <p>Not in the queue: {out.join(" · ")}.</p>}
      {total > QUEUE_READ.reviews && (
        <p role="note">
          This load holds {formatNumber(total)} reviews: the queue reads the
          first {formatNumber(QUEUE_READ.reviews)} the database keeps, decided
          ones and those no decision settles included, and its spot checks draw
          held claims from those alone. The rest show on their documents’ pages.
        </p>
      )}
      {!manage && <p className="on-muted">Owners and admins settle reviews.</p>}
    </div>
  );
}
