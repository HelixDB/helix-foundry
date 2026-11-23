// The review walk, the Review page's Facts tab (./page.tsx, at
// /ontology/:ontologyId/review): the current load's open cards, as the
// review cards index lists them (./cards-api.ts: the decisions that write
// the most facts first, a page at a time, filtered by kind, type and
// document), walked one card at a time (./walkthrough.tsx), each card with
// Founder's suggestion from fixed rules (./advice.ts) and settled as a
// document's Needs review settles it. Each page's reviews are read from the
// load by key (./cards.ts). At the finish, a few spot checks drawn at
// random from every score band measure how often claims at each score are
// right (./spot-checks.tsx). The page holds the walk (./walkthrough.tsx
// useWalk), so the tab's count and the walk keep their place across a
// switch of tabs.
import "./reviews.css";
import { useEffect, useMemo } from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  useInfiniteQuery,
  useQueries,
  useQuery,
  type UseQueryResult,
} from "@tanstack/react-query";
import { ListChecks } from "lucide-react";
import type {
  FoundryReviewDecision,
  ListFoundryReviewCardsResponse,
} from "../../../shared/wfe";
import { Button, EmptyState, SkeletonText } from "../../kit";
import { paths } from "../../paths";
import { canManage, useWorkspace } from "../../ui";
import { useReadAccess } from "../explore/graph-reader";
import { readQueries } from "../explore/reads";
import { useTenantReads } from "../explore/tenant-reads";
import { useNaming } from "../explore/use-naming";
import { ontologyQueries } from "../ontology/queries";
import { useOntology } from "../ontology/scope";
import { adviceContext } from "./advice";
import {
  cardsOf,
  reviewReads,
  withOwnDecisions,
  type NameLink,
  type ReviewRead,
} from "./cards";
import {
  ADVICE_PAGES,
  cardQueries,
  useReviewSummary,
  type CardFilter,
  type CardKind,
} from "./cards-api";
import { asBand, type QueueCard, type QueueReview } from "./queue";
import { walkOrder, type AdviceFilter } from "./walk";
import { ReviewWalkthrough, useWalk } from "./walkthrough";

const KINDS: readonly CardKind[] = ["name", "claim", "value"];

type Read = UseQueryResult<{ reviews: QueueReview[]; links: NameLink[] }>;

/**
 * Each page's cards, once all its reviews are read, in order, and how many
 * pages that is: the pages read so far from the first, so the walk never
 * shows a later card before an earlier one, nor a walk before its first
 * page.
 */
function pageCards(
  pages: readonly ListFoundryReviewCardsResponse[],
  plan: readonly ReviewRead[],
  read: readonly Read[],
): { cards: QueueCard[]; pages: number } {
  const out: QueueCard[] = [];
  for (const [i, p] of pages.entries()) {
    const mine = plan.flatMap((r, j) => (r.page === i ? [read[j]] : []));
    if (mine.some((r) => !r?.data)) return { cards: out, pages: i };
    const reviews = new Map(
      mine.flatMap((r) =>
        r!.data!.reviews.map(
          (v) =>
            [
              v.settle && v.settle.kind !== "stale" ? v.settle.reviewId : "",
              v,
            ] as const,
        ),
      ),
    );
    out.push(
      ...cardsOf(
        p.cards ?? [],
        reviews,
        mine.flatMap((r) => r!.data!.links),
      ),
    );
  }
  return { cards: out, pages: pages.length };
}

/**
 * The Facts tab's reads of the current load: its cards from the index (the
 * Decide lane under the URL's filters, and the FYI lane), their reviews,
 * Founder's advice over them, the summary's count and the walk. The Review
 * page holds them, so the tab's count shows on either tab and what was
 * decided here keeps its place across a switch. `open` is the summary's
 * count of cards to decide, less the cards the walk closed since;
 * unset until the summary is read.
 */
export function useReviewCards() {
  const { id: ws, user } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const access = useReadAccess();
  const loads = useQuery(ontologyQueries.loads(ws, o));
  // Only the current load is settled, so the cards are always its.
  const load = loads.data?.loads?.find((l) => l.current);
  const loadId = load?.id ?? "";
  const scope = useMemo(() => ({ id: loadId }), [loadId]);
  const { readLater } = useTenantReads();
  const { naming, ready } = useNaming(load?.versionId || "");
  const reads = access === "database" && !!loadId && !!ontology.tenantId;
  // The filters, in the URL: one kind, a type and a document, which the
  // index applies, and Founder's advice, which the page does.
  const [params] = useSearchParams();
  const kind = KINDS.find((k) => k === params.get("kind")),
    type = params.get("type") ?? "",
    doc = params.get("doc") ?? "",
    adviceParam = params.get("advice");
  const advice: AdviceFilter =
    adviceParam === "suggested" || adviceParam === "none" ? adviceParam : "any";
  const filter = useMemo(
    (): CardFilter => ({
      lane: "decide",
      kinds: kind ? [kind] : [],
      ...(type ? { type } : {}),
      ...(doc ? { documentId: doc } : {}),
    }),
    [kind, type, doc],
  );
  const decide = useInfiniteQuery({
    ...cardQueries.pages(ws, o, loadId, filter),
    enabled: reads,
  });
  const fyi = useInfiniteQuery({
    ...cardQueries.pages(ws, o, loadId, { lane: "fyi", kinds: [] }),
    enabled: reads,
  });
  const decidePages = decide.data?.pages,
    fyiPages = fyi.data?.pages;
  const plan = useMemo(
    () => ({
      decide: reviewReads(decidePages ?? []),
      fyi: reviewReads(fyiPages ?? []),
    }),
    [decidePages, fyiPages],
  );
  // Each waits its turn at the database's limiter.
  const reviewQuery = (r: ReviewRead) => ({
    ...readQueries.reviewsByKey(
      ws,
      o,
      readLater,
      scope,
      r.keys,
      r.docs,
      naming,
    ),
    enabled: reads && ready,
  });
  const decideReads = useQueries({
    queries: plan.decide.map(reviewQuery),
  }) as Read[];
  const fyiReads = useQueries({ queries: plan.fyi.map(reviewQuery) }) as Read[];
  const settled = (rs: readonly Read[]) =>
    rs.map((r) => r.dataUpdatedAt).join();
  const decideCards = useMemo(
    () => pageCards(decidePages ?? [], plan.decide, decideReads),
    // The reads' data, by when each was read.
    [decidePages, plan.decide, settled(decideReads)],
  );
  const fyiCards = useMemo(
    () => pageCards(fyiPages ?? [], plan.fyi, fyiReads),
    [fyiPages, plan.fyi, settled(fyiReads)],
  );
  const single = useMemo(
    () => (link: string) => !naming.version || naming.oneValued.has(link),
    [naming],
  );
  const walk = useWalk();
  // The walk's own decisions, read by id for what became of each.
  const ids = useMemo(
    () =>
      walk.state.loadId === loadId
        ? [...walk.state.outcomes.values()]
            .flatMap((x) =>
              x.kind === "saved" || x.kind === "unconfirmed"
                ? [x.decision.id]
                : [],
            )
            .sort()
        : [],
    [walk.state.outcomes, walk.state.loadId, loadId],
  );
  const decided = useQuery(cardQueries.decided(ws, o, ids));
  const own = useMemo(() => decided.data ?? [], [decided.data]);
  // What Founder's rules read: the index's names and claim decisions of the
  // cards read, and the first page's precision.
  const first = decidePages?.[0];
  const ctx = useMemo(
    () =>
      adviceContext({
        names: withOwnDecisions(
          (decidePages ?? []).flatMap((p) => p.names ?? []),
          own,
        ),
        claims: [
          ...(decidePages ?? []).flatMap((p) =>
            (p.cards ?? []).flatMap((c) => [
              ...(c.claim?.decisions ?? []),
              ...(c.name?.held ?? []).flatMap((h) => h.decisions ?? []),
            ]),
          ),
          ...own,
        ],
        precision: {
          bands: (first?.precision?.bands ?? []).flatMap((b) => {
            const band = asBand(b.band);
            return band
              ? [{ band, right: b.right ?? 0, total: b.total ?? 0 }]
              : [];
          }),
          rechecks: {
            agreed: first?.precision?.rechecksAgreed ?? 0,
            total: first?.precision?.rechecksTotal ?? 0,
          },
        },
        single,
        user: user.id,
      }),
    [decidePages, first, single, user.id, own],
  );
  // The walk starts once each lane's first page is read; a later page joins
  // it once its reviews are.
  const all = useMemo(
    () =>
      decideCards.pages && fyiCards.pages
        ? walkOrder([...decideCards.cards, ...fyiCards.cards], ctx, advice)
        : undefined,
    [decideCards, fyiCards, ctx, advice],
  );
  // The advice filters read a few pages, then say where they stopped.
  const pageCount = decidePages?.length ?? 0;
  useEffect(() => {
    if (
      advice !== "any" &&
      decide.hasNextPage &&
      !decide.isFetchingNextPage &&
      pageCount < ADVICE_PAGES
    )
      void decide.fetchNextPage();
  }, [advice, decide.hasNextPage, decide.isFetchingNextPage, pageCount]);
  // Within ten cards of the last read, the next page is read.
  const toGo = walk.state.order.length - walk.state.at;
  useEffect(() => {
    if (
      walk.state.loadId === loadId &&
      advice === "any" &&
      toGo <= 10 &&
      decide.hasNextPage &&
      !decide.isFetchingNextPage
    )
      void decide.fetchNextPage();
  }, [toGo, advice, decide.hasNextPage, decide.isFetchingNextPage, loadId]);
  const spot = useQuery({
    ...cardQueries.spot(ws, o, loadId),
    enabled: reads,
  });
  const summary = useReviewSummary().get(o);
  const counted = summary?.loadId === loadId ? summary : undefined;
  const indexing = !!decide.data?.pages.at(-1)?.indexing || !!counted?.indexing;
  const failed: Error | null =
    decide.error ??
    fyi.error ??
    [...decideReads, ...fyiReads].find((r) => r.error)?.error ??
    null;
  const retry = () => {
    void decide.refetch();
    void fyi.refetch();
    for (const r of [...decideReads, ...fyiReads])
      if (r.error) void r.refetch();
  };
  return {
    access,
    loads,
    load,
    naming,
    single,
    ctx,
    all,
    decide,
    fyi,
    filters: { kind, type, doc, advice },
    // The advice filters stopped at ADVICE_PAGES with more to read.
    adviceCapped:
      advice !== "any" && !!decide.hasNextPage && pageCount >= ADVICE_PAGES,
    // More cards than the walk has: pages not read yet, or not rendered.
    hasMore:
      !!decide.hasNextPage || decideCards.pages < (decidePages?.length ?? 0),
    decided: decided as UseQueryResult<FoundryReviewDecision[]>,
    spot,
    indexing,
    open: counted?.open,
    more: !!counted?.more,
    failed,
    retry,
    walk,
  };
}

export type ReviewCards = ReturnType<typeof useReviewCards>;

/** The Review page's Facts tab: the cards `useReviewCards` read, walked. */
export function FactsReview({ queue: q }: { queue: ReviewCards }) {
  const { role } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const { access, loads, load, all, failed, retry } = q;
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
  if (!all)
    return failed ? (
      <EmptyState
        icon={ListChecks}
        title="Couldn’t read the reviews"
        text={failed.message}
        action={<Button onClick={retry}>Retry</Button>}
      />
    ) : (
      <SkeletonText lines={6} />
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
      {(q.indexing || !canManage(role)) && (
        <div className="rq-summary">
          {q.indexing && (
            <p role="note">
              Counting… the load’s reviews are still being sorted into cards:
              more may come.
            </p>
          )}
          {!canManage(role) && (
            <p className="on-muted">Owners and admins settle reviews.</p>
          )}
        </div>
      )}
      <ReviewWalkthrough queue={q} load={load} />
    </>
  );
}
