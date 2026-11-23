// The review walk, the Review page's Facts tab (./page.tsx, at
// /ontology/:ontologyId/review): the current load's open cards, as the
// review cards index lists them (./cards-api.ts: the decisions that write
// the most facts first, a page at a time, filtered by kind, type and
// document), walked one card at a time (./walkthrough.tsx), each card with
// Founder's suggestion from fixed rules (./advice.ts), or from Founder's
// model where the rules abstain on a name or a value, and settled as a
// document's Needs review settles it. The cards' reviews are read from the
// load by key, ten cards at a time (./cards.ts). At the finish, a few spot checks drawn at
// random from every score band measure how often claims at each score are
// right (./spot-checks.tsx). The page holds the walk (./walkthrough.tsx
// useWalk), so the tab's count and the walk keep their place across a
// switch of tabs.
import "./reviews.css";
import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  useInfiniteQuery,
  useQueries,
  useQuery,
  type UseQueryResult,
} from "@tanstack/react-query";
import { ListChecks } from "lucide-react";
import type { FoundryReviewDecision } from "../../../shared/wfe";
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
  reviewReads,
  windowCards,
  withOwnDecisions,
  type ReviewRead,
  type ReviewsRead,
} from "./cards";
import {
  ADVICE_PAGES,
  cardQueries,
  useReviewSummary,
  type CardFilter,
  type CardKind,
} from "./cards-api";
import { asBand } from "./queue";
import { walkOrder, type AdviceFilter, type WalkOrder } from "./walk";
import { ReviewWalkthrough, useWalk } from "./walkthrough";

const KINDS: readonly CardKind[] = ["name", "claim", "value"];

/** How many windows of cards the walk reads before it starts. */
const FIRST_WINDOWS = 2;

type Read = UseQueryResult<ReviewsRead>;

/**
 * The Facts tab's reads of the current load: its cards from the index (the
 * Decide lane under the URL's filters, and the FYI lane), their reviews a
 * window of cards at a time (as the walk nears the last window read, the
 * next; with an advice filter, every window of the pages it reads),
 * Founder's advice over them, the summary's count and the walk. The Review
 * page holds them, so the tab's count shows on either tab and what was
 * decided here keeps its place across a switch. `open` is the summary's
 * count of cards to decide, less the cards the walk closed since; unset
 * until the summary is read. While cards it showed are read again under
 * other keys (a page read again holds other cards), the walk keeps what it
 * showed until they land.
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
  const view = JSON.stringify([loadId, filter]);
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
  // How many of the Decide lane's windows to read: a few to start, every
  // one under an advice filter (it reads whole pages); FYI's, every one.
  const [reach, setReach] = useState({ view: "", windows: FIRST_WINDOWS });
  const decideReach =
    advice !== "any"
      ? Infinity
      : reach.view === view
        ? reach.windows
        : FIRST_WINDOWS;
  // Each waits its turn at the database's limiter.
  const reviewQuery = (r: ReviewRead, upTo: number) => ({
    ...readQueries.reviewsByKey(
      ws,
      o,
      readLater,
      scope,
      r.keys,
      r.docs,
      naming,
    ),
    enabled: reads && ready && r.window < upTo,
  });
  const decideReads = useQueries({
    queries: plan.decide.map((r) => reviewQuery(r, decideReach)),
  }) as Read[];
  const fyiReads = useQueries({
    queries: plan.fyi.map((r) => reviewQuery(r, Infinity)),
  }) as Read[];
  const settled = (rs: readonly Read[]) =>
    rs.map((r) => r.dataUpdatedAt).join();
  const decideCards = useMemo(
    () =>
      windowCards(
        decidePages ?? [],
        plan.decide,
        decideReads.map((r) => r.data),
        decideReach,
      ),
    // The reads' data, by when each was read.
    [decidePages, plan.decide, settled(decideReads), decideReach],
  );
  const fyiCards = useMemo(
    () =>
      windowCards(
        fyiPages ?? [],
        plan.fyi,
        fyiReads.map((r) => r.data),
        Infinity,
      ),
    [fyiPages, plan.fyi, settled(fyiReads)],
  );
  const single = useMemo(
    () => (link: string) => !naming.version || naming.oneValued.has(link),
    [naming],
  );
  const walk = useWalk();
  // Decisions the walk's FYI and spot checks saved, read by id with its own.
  const [elsewhere, setElsewhere] = useState({
    loadId: "",
    ids: [] as readonly string[],
  });
  // The walk's own decisions, read by id for what became of each.
  const ids = useMemo(
    () =>
      walk.state.loadId === loadId
        ? [
            ...new Set([
              ...[...walk.state.outcomes.values()].flatMap((x) =>
                x.kind === "saved" || x.kind === "unconfirmed"
                  ? [x.decision.id]
                  : [],
              ),
              ...(elsewhere.loadId === loadId ? elsewhere.ids : []),
            ]),
          ].sort()
        : [],
    [walk.state.outcomes, walk.state.loadId, loadId, elsewhere],
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
  // The walk starts once each lane's first page is read and its first
  // window too (or it has none); a later window joins it once its reviews
  // are read.
  const fresh = useMemo(
    () =>
      decidePages?.length &&
      fyiPages?.length &&
      (decideCards.shown || !decideCards.windows) &&
      (fyiCards.shown || !fyiCards.windows)
        ? walkOrder([...decideCards.cards, ...fyiCards.cards], ctx, advice)
        : undefined,
    [decidePages, fyiPages, decideCards, fyiCards, ctx, advice],
  );
  // A window asked for whose reads haven't landed: the last walk shown
  // stands until they do.
  const rereading =
    decideCards.shown < Math.min(decideReach, decideCards.windows) ||
    fyiCards.shown < fyiCards.windows;
  const shown = useRef<{ view: string; all: WalkOrder }>(undefined);
  const viewed = JSON.stringify([view, advice]);
  const all =
    rereading && shown.current?.view === viewed ? shown.current.all : fresh;
  useEffect(() => {
    if (fresh && !rereading) shown.current = { view: viewed, all: fresh };
  }, [fresh, rereading, viewed]);
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
  // Within ten cards of the last shown, once the windows asked for are
  // shown, the next window is read, and on the last window of the pages
  // read, the next page too.
  const toGo = walk.state.order.length - walk.state.at;
  const asked = Math.min(decideReach, decideCards.windows);
  useEffect(() => {
    if (
      walk.state.loadId !== loadId ||
      advice !== "any" ||
      toGo > 10 ||
      decideCards.shown < asked
    )
      return;
    if (decideReach < decideCards.windows)
      setReach({ view, windows: decideReach + 1 });
    if (
      decideReach + 1 >= decideCards.windows &&
      decide.hasNextPage &&
      !decide.isFetchingNextPage
    )
      void decide.fetchNextPage();
  }, [
    toGo,
    advice,
    asked,
    decideCards.shown,
    decideCards.windows,
    decideReach,
    decide.hasNextPage,
    decide.isFetchingNextPage,
    loadId,
    view,
  ]);
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
    // More cards than the walk has: pages not read yet, or windows not
    // shown.
    hasMore: !!decide.hasNextPage || decideCards.shown < decideCards.windows,
    decided: decided as UseQueryResult<FoundryReviewDecision[]>,
    // Reads a decision the walk's FYI lane or spot checks saved by id, with
    // the walk's own, for what became of it.
    addDecided: (id: string) =>
      setElsewhere((e) => ({
        loadId,
        ids: e.loadId === loadId ? [...e.ids, id] : [id],
      })),
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
