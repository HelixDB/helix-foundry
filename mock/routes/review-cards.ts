import type {
  FoundryReviewCard,
  FoundryReviewClaimCard,
  FoundryReviewDecision,
  FoundryReviewDraw,
  FoundryReviewLabel,
  FoundryReviewNameDecisions,
  FoundryReviewPrecision,
  FoundryReviewRecheck,
  FoundryReviewSpotChecksResponse,
  FoundryReviewSummary,
  GetFoundryReviewSummaryResponse,
  ListFoundryReviewCardsResponse,
} from "../../src/shared/wfe";
import {
  FOUNDRY_EDGES as E,
  FOUNDRY_LOAD,
  FOUNDRY_NODES as N,
  isFoundryLabel,
} from "../../src/shared/foundry-labels";
import { fail, ok, type MockReply, type MockRoute } from "../http";
import { holdsOne, decisions, labels } from "../review-decisions";
import {
  BANDS,
  SPOT_CHECKS,
  bandOf,
  buildCards,
  decidesCard,
  kindOrder,
  nextDraw,
  quoteOrder,
  quoteTarget,
  rechecked,
  score,
  type Card,
  type Kind,
  type Pool,
  type Quote,
  type Review,
} from "../review-cards";
import { config, isReply, membership, onReset } from "../state";
import { tenantGraph } from "./databases";
import {
  appliedOf,
  currentLoadId,
  currentLoadVersionId,
  servableLoad,
} from "./loads";
import {
  isScope,
  ontologyOf,
  workspaceOntologies,
  type StoredOntology,
} from "./ontologies";
import { ontologyVersion } from "./ontology";
import { ontologyLabels } from "./reviews";
import { modelAdviceOf } from "../review-advice";

// The review cards routes, as the backend serves them (api/review_cards.go
// over store/foundry_review_cards.go): the open count of each current load
// (the summary), its open cards a page at a time in the walk's order, and
// its spot checks, drawn once and kept. The backend's graph worker keeps an
// index; the mock builds a load's cards from its graph on each call
// (../review-cards.ts, the index's rules), so it is never behind. A card is
// open while no decision decides it and no spot check drew it
// (openFoundryReviewCard). Draws are kept per load until a reset.
//
// Test knobs, through POST /__mock/config:
// - `reviewCardsPage` n: a page lists n cards, not 50;
// - `reviewIndexing`: the summary and the cards say the index is still
//   first being built, and the draw draws nothing;
// - `reviewModelAdvice` false: no card carries Founder's model's advice
//   (../review-advice.ts), as with foundry.reviews.advise off;
// - `reviewModelAdviceRationale`: every advice's rationale, as sent.
type Knobs = {
  reviewCardsPage?: number;
  reviewIndexing?: boolean;
  reviewModelAdvice?: boolean;
  reviewModelAdviceRationale?: string;
};
const knobs = config as typeof config & Knobs;
const PAGE = 50;
const COUNT_CAP = 10_000;
const RECHECKS = 20;
const DRAW_TRIES = 30;
const QUOTE_DOCUMENTS = 5;

type Draw = {
  seq: number;
  kind: "held" | "quote";
  band: string;
  docId: string;
  section: number;
  start: number;
  end: number;
  link?: string;
  confidence?: number;
  claimKey?: string;
  statementHash?: string;
  factKey?: string;
};
const draws = new Map<string, Draw[]>();
onReset(() => {
  draws.clear();
  delete knobs.reviewCardsPage;
  delete knobs.reviewIndexing;
  delete knobs.reviewModelAdvice;
  delete knobs.reviewModelAdviceRationale;
});

/** A load's cards and quotes of facts, as its index would hold them. */
type Index = {
  load: string;
  cards: Card[];
  // Each document's quotes of facts, at their places.
  quotes: Map<string, Quote[]>;
  // Every review, by its key, as the graph holds it.
  reviews: Map<string, Review>;
};

const text = (v: unknown) => (typeof v === "string" ? v : "");
const num = (v: unknown) => (typeof v === "number" ? v : null);

/** Load `load` of ontology `o`'s cards, built from its graph. */
function indexOf(o: StoredOntology, load: string): Index {
  const g = tenantGraph(o.tenantId);
  const definition = ontologyVersion(
    o.id,
    currentLoadVersionId(o.id) || "",
  )?.definition;
  const byDoc = new Map<string, Review[]>(),
    quotes = new Map<string, Quote[]>(),
    reviews = new Map<string, Review>();
  const sectionOf = (id: number) => {
    const at = (g.outgoing.get(id) || [])
      .map((e) => g.edges.get(e)!)
      .find((e) => e.label === E.FROM_SECTION)?.to;
    return at === undefined ? undefined : num(g.nodes.get(at)?.props.ordinal);
  };
  for (const n of g.nodes.values()) {
    if (n.props[FOUNDRY_LOAD] !== load) continue;
    const p = n.props,
      doc = text(p.doc_id);
    if (!doc) continue;
    if (n.label === N.Evidence && p.fact_key !== undefined && p.fact_key) {
      const section = sectionOf(n.id),
        start = num(p.start),
        end = num(p.end);
      if (section === null || section === undefined) continue;
      if (start === null || end === null || start >= end || !text(p.link))
        continue;
      const confidence = score(p.confidence);
      quotes.set(doc, [
        ...(quotes.get(doc) ?? []),
        {
          section,
          start,
          end,
          link: text(p.link),
          factKey: text(p.fact_key),
          ...(confidence !== undefined ? { confidence } : {}),
        },
      ]);
      continue;
    }
    if (n.label !== N.Review) continue;
    const concerned = (g.outgoing.get(n.id) || []).flatMap((e) => {
      const edge = g.edges.get(e)!;
      const to = edge.label === E.CONCERNS ? g.nodes.get(edge.to) : undefined;
      if (!to || isFoundryLabel("node", to.label)) return [];
      const key = text(to.props.entity_key),
        norm = text(to.props.norm_name);
      return !key === !norm
        ? []
        : [{ type: to.label, key: key || norm, props: to.props }];
    });
    const review: Review = {
      review_key: text(p.review_key),
      reason: text(p.reason),
      type: text(p.type),
      doc_id: doc,
      norm_name: text(p.norm_name),
      candidates: Array.isArray(p.candidates)
        ? p.candidates.filter((c): c is string => typeof c === "string")
        : [],
      entity_key: text(p.entity_key),
      prop: text(p.prop),
      // The engine keeps them as JSON text; the mock's loads, as values.
      values:
        typeof p.values === "string" ? p.values : JSON.stringify(p.values),
      suggested_link: text(p.suggested_link),
      statement: text(p.statement),
      claim_key: text(p.claim_key),
      claim: text(p.claim),
      section: num(p.section),
      start: num(p.start),
      end: num(p.end),
      confidence: score(p.confidence) ?? null,
      concerned,
    };
    byDoc.set(doc, [...(byDoc.get(doc) ?? []), review]);
    if (!reviews.has(review.review_key!))
      reviews.set(review.review_key!, review);
  }
  const saved = decisions.get(o.id) || [];
  const cards = [...byDoc].flatMap(
    ([doc, reviews]) =>
      buildCards(
        load,
        doc,
        reviews,
        saved.flatMap((d) =>
          (d.kind === "release" || d.kind === "reject") && d.docId === doc
            ? [
                {
                  kind: d.kind,
                  section: d.claimSection,
                  start: d.claimStart,
                  end: d.claimEnd,
                  claim_key: d.claimKey,
                  statement_hash: d.statementHash,
                },
              ]
            : [],
        ),
        (link) => holdsOne(definition, link),
      ).cards,
  );
  return { load, cards, quotes, reviews };
}

/**
 * Whether a card is open (openFoundryReviewCard): a held claim no
 * rejection at its place (under its key or of its statement) or approval of
 * its statement decides, and no spot check drew; a name or a value no
 * decision of its identity decides.
 */
function isOpen(
  c: Card,
  saved: readonly FoundryReviewDecision[],
  load: string,
) {
  return (
    !saved.some((d) => decidesCard(d, c)) &&
    !(
      c.kind === "release" &&
      (draws.get(load) ?? []).some(
        (w) =>
          w.kind === "held" &&
          w.docId === c.docId &&
          w.section === c.claimSection &&
          w.start === c.claimStart &&
          w.end === c.claimEnd &&
          w.claimKey === c.claimKey,
      )
    )
  );
}

/**
 * The card `cardId` of the ontology's servable current load, and whether it
 * is open (GetFoundryReviewCard); undefined when that load has none.
 */
export function reviewCardOf(
  o: StoredOntology,
  cardId: string,
): { card: Card; open: boolean } | undefined {
  const load = servableLoad(o.id)?.loadId;
  if (!load) return undefined;
  const card = indexOf(o, load).cards.find((c) => c.id === cardId);
  return card && { card, open: isOpen(card, decisions.get(o.id) || [], load) };
}

/** The walk's order: per decision and facts, descending; kind, shuffle and id. */
const walkOrder = (a: Card, b: Card) =>
  b.perDecision - a.perDecision ||
  b.facts - a.facts ||
  kindOrder(a.kind) - kindOrder(b.kind) ||
  a.shuffle - b.shuffle ||
  (a.id < b.id ? -1 : a.id > b.id ? 1 : 0);

/** The ontology's current load, servable, or the backend's refusal. */
function reviewLoad(o: StoredOntology): string | MockReply {
  const load = servableLoad(o.id)?.loadId;
  if (load) return load;
  return currentLoadId(o.id)
    ? fail("FAILED_PRECONDITION", "the load is not ready")
    : fail(
        "FAILED_PRECONDITION",
        "load the ontology before reading its reviews",
      );
}

const withApplied = (o: string, d: FoundryReviewDecision) => {
  const applied = appliedOf(o, d);
  return applied ? { ...d, applied } : d;
};

/** A place's link, as places compare (COALESCE(link, '')). */
const at = (
  x: { docId: string; section?: number; start?: number; end?: number },
  link: string | undefined,
) =>
  [x.docId, x.section ?? 0, x.start ?? 0, x.end ?? 0, link ?? ""].join(
    "\u001f",
  );
const labelAt = (l: FoundryReviewLabel) =>
  at(
    {
      docId: l.docId,
      section: l.claimSection,
      start: l.claimStart,
      end: l.claimEnd,
    },
    l.link,
  );

/** Whether a random label of the ontology (any load) is at a draw's place, its link included. */
const done = (o: string, d: Draw) =>
  (labels.get(o) || []).some(
    (l) => l.drawn === "random" && labelAt(l) === at(d, d.link),
  );

/** The places of load `load` a spot check or a random label of `o` holds. */
const taken = (o: string, load: string) =>
  new Set([
    ...(draws.get(load) ?? []).map((d) => at(d, d.link)),
    ...(labels.get(o) || []).flatMap((l) =>
      l.drawn === "random" ? [labelAt(l)] : [],
    ),
  ]);

const placeOf = (c: Card) =>
  at(
    {
      docId: c.docId,
      section: c.claimSection,
      start: c.claimStart,
      end: c.claimEnd,
    },
    c.link,
  );

/** Each band's pool (FoundryReviewDrawPools). */
function pools(o: string, index: Index): Pool[] {
  const held = taken(o, index.load);
  const drawn = draws.get(index.load) ?? [];
  return BANDS.map((band, b) => ({
    held: index.cards.filter(
      (c) => c.kind === "release" && c.band === band && !held.has(placeOf(c)),
    ).length,
    quotes: Math.max(
      0,
      [...index.quotes.values()]
        .flat()
        .filter((q) => bandOf(q.confidence) === b).length -
        drawn.filter((d) => d.kind === "quote" && d.band === band).length,
    ),
  }));
}

/** Draws spot check `seq` of a band's held claims; false when it has none free. */
function drawHeld(o: string, index: Index, seq: number, band: number) {
  const held = taken(o, index.load);
  const [c] = index.cards
    .filter(
      (c) =>
        c.kind === "release" && c.band === BANDS[band] && !held.has(placeOf(c)),
    )
    .sort(
      (a, b) =>
        a.shuffle - b.shuffle || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0),
    );
  if (!c) return false;
  draws.set(index.load, [
    ...(draws.get(index.load) ?? []),
    {
      seq,
      kind: "held",
      band: c.band!,
      docId: c.docId,
      section: c.claimSection!,
      start: c.claimStart!,
      end: c.claimEnd!,
      link: c.link,
      ...(c.confidence !== undefined ? { confidence: c.confidence } : {}),
      claimKey: c.claimKey,
      statementHash: c.statementHash,
    },
  ]);
  return true;
}

/**
 * Draws spot check `seq` of a band's quotes: from the document holding the
 * band's target quote (documents by id, each weighted by its band's
 * quotes), then those after it, then those before, at most five, each its
 * first free quote of the band by the load's order.
 */
function drawQuote(
  o: string,
  index: Index,
  seq: number,
  band: number,
  count: number,
) {
  const target = quoteTarget(index.load, band, seq, count);
  let upto = 0;
  const docs = [...index.quotes]
    .map(([doc, qs]) => ({
      doc,
      n: qs.filter((q) => bandOf(q.confidence) === band).length,
    }))
    .filter((d) => d.n > 0)
    .sort((a, b) => (a.doc < b.doc ? -1 : 1))
    .map((d) => ({ doc: d.doc, upto: (upto += d.n) }));
  const ordered = [
    ...docs.filter((d) => d.upto > target),
    ...docs.filter((d) => d.upto <= target),
  ].slice(0, QUOTE_DOCUMENTS);
  const held = taken(o, index.load);
  for (const { doc } of ordered) {
    const q = quoteOrder(index.load, doc, index.quotes.get(doc) ?? []).find(
      (q) =>
        bandOf(q.confidence) === band &&
        !held.has(at({ docId: doc, ...q }, q.link)),
    );
    if (!q) continue;
    draws.set(index.load, [
      ...(draws.get(index.load) ?? []),
      {
        seq,
        kind: "quote",
        band: BANDS[band],
        docId: doc,
        section: q.section,
        start: q.start,
        end: q.end,
        link: q.link,
        ...(q.confidence !== undefined ? { confidence: q.confidence } : {}),
        factKey: q.factKey,
      },
    ]);
    return true;
  }
  return false;
}

/** Load `index.load`'s spot checks as the response reads them, for `user`. */
function spotChecks(
  o: StoredOntology,
  index: Index,
  user: string,
): FoundryReviewSpotChecksResponse {
  const drawn = (draws.get(index.load) ?? []).toSorted((a, b) => a.seq - b.seq);
  const saved = decisions.get(o.id) || [];
  const all = ontologyLabels(o.id, user);
  const atDraw = (doc: string, s: number, b: number, e: number) =>
    drawn.some(
      (d) => d.docId === doc && d.section === s && d.start === b && d.end === e,
    );
  const mine = all.filter((l) => l.decidedByUserId === user);
  const said = (
    doc: string,
    s?: number,
    b?: number,
    e?: number,
    link?: string | null,
  ) =>
    mine.some(
      (m) =>
        m.docId === doc &&
        (m.claimSection ?? 0) === (s ?? 0) &&
        (m.claimStart ?? 0) === (b ?? 0) &&
        (m.claimEnd ?? 0) === (e ?? 0) &&
        (link === null || (m.link ?? "") === (link ?? "")),
    );
  const byId = (a: { id: string }, b: { id: string }) =>
    a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  const rechecks: FoundryReviewRecheck[] = [
    ...saved
      .filter(
        (d) =>
          (d.kind === "release" || d.kind === "reject") &&
          !!d.drawn &&
          d.decidedByUserId !== user &&
          rechecked(d.id) &&
          !said(d.docId, d.claimSection, d.claimStart, d.claimEnd, null),
      )
      .toSorted(byId)
      .slice(0, RECHECKS)
      .map((d) => ({ decision: withApplied(o.id, d) })),
    ...all
      .filter(
        (l) =>
          l.drawn === "random" &&
          !l.decisionId &&
          l.decidedByUserId !== user &&
          rechecked(l.id) &&
          !said(l.docId, l.claimSection, l.claimStart, l.claimEnd, l.link),
      )
      .toSorted(byId)
      .slice(0, RECHECKS)
      .map((label) => ({ label })),
  ];
  const card = (d: Draw) =>
    index.cards.find(
      (c) =>
        c.kind === "release" &&
        c.docId === d.docId &&
        c.claimSection === d.section &&
        c.claimStart === d.start &&
        c.claimEnd === d.end &&
        c.claimKey === d.claimKey,
    );
  return {
    loadId: index.load,
    draws: drawn.map(
      (d): FoundryReviewDraw => ({
        seq: String(d.seq),
        band: d.band,
        docId: d.docId,
        span: {
          section: String(d.section),
          start: String(d.start),
          end: String(d.end),
        },
        ...(d.link ? { link: d.link } : {}),
        ...(d.confidence !== undefined ? { confidence: d.confidence } : {}),
        done: done(o.id, d),
        ...(d.kind === "held"
          ? {
              held: {
                claimKey: d.claimKey!,
                statementHash: d.statementHash!,
                reviewKey: card(d)?.reviewKey ?? "",
              },
            }
          : { quote: { factKey: d.factKey! } }),
      }),
    ),
    rechecks,
    decisions: saved
      .filter(
        (d) =>
          (d.kind === "release" || d.kind === "reject") &&
          atDraw(d.docId, d.claimSection, d.claimStart, d.claimEnd),
      )
      .map((d) => withApplied(o.id, d)),
    labels: all.filter((l) => drawn.some((d) => labelAt(l) === at(d, d.link))),
  };
}

/** The spot checks' precision (FoundryReviewPrecision). */
function precision(o: string, user: string): FoundryReviewPrecision {
  const all = ontologyLabels(o, user);
  const first = new Map<string, FoundryReviewLabel>();
  for (const l of all
    .filter((l) => l.drawn === "random")
    .toSorted(
      (a, b) =>
        (a.decidedAt ?? "").localeCompare(b.decidedAt ?? "") ||
        a.id.localeCompare(b.id),
    ))
    if (!first.has(labelAt(l))) first.set(labelAt(l), l);
  const checks = all.filter((l) => l.drawn === "recheck" && !!l.against);
  return {
    bands: BANDS.flatMap((band, b) => {
      const of = [...first.values()].filter((l) => bandOf(l.confidence) === b);
      return of.length
        ? [
            {
              band,
              right: of.filter((l) => l.verdict === "right").length,
              total: of.length,
            },
          ]
        : [];
    }),
    rechecksAgreed: checks.filter((l) => l.verdict === l.against).length,
    rechecksTotal: checks.length,
  };
}

const LANES = {
  "": "decide",
  FOUNDRY_REVIEW_LANE_UNSPECIFIED: "decide",
  FOUNDRY_REVIEW_LANE_DECIDE: "decide",
  FOUNDRY_REVIEW_LANE_FYI: "fyi",
} as const;
const KINDS: Record<string, Kind> = {
  FOUNDRY_REVIEW_CARD_KIND_NAME: "resolve",
  FOUNDRY_REVIEW_CARD_KIND_CLAIM: "release",
  FOUNDRY_REVIEW_CARD_KIND_VALUE: "prop",
};
type Cursor = { p: number; f: number; k: number; s: number; i: string };

const BASE = "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}";

export const reviewCardRoutes: MockRoute[] = [
  {
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/review-summary",
    handle: (req) => {
      const m = membership(req.params.workspaceId!, req.principal.user.id);
      if (isReply(m)) return m;
      // A restricted member, who reads none of the cards, gets none.
      if (req.grantee) return ok({} satisfies GetFoundryReviewSummaryResponse);
      const ontologies = workspaceOntologies(m.workspaceId).flatMap(
        (o): FoundryReviewSummary[] => {
          const load = servableLoad(o.id)?.loadId;
          if (!load) return [];
          const saved = decisions.get(o.id) || [];
          const open = indexOf(o, load).cards.filter(
            (c) => c.lane === "decide" && isOpen(c, saved, load),
          ).length;
          return [
            {
              ontologyId: o.id,
              loadId: load,
              open: Math.min(open, COUNT_CAP),
              more: open > COUNT_CAP,
              indexing: !!knobs.reviewIndexing,
            },
          ];
        },
      );
      return ok({ ontologies } satisfies GetFoundryReviewSummaryResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/review-cards`,
    handle: (req) => {
      const s = ontologyOf(req, false);
      if (!isScope(s)) return s;
      const lane = LANES[(req.query.get("lane") ?? "") as keyof typeof LANES];
      if (!lane) return fail("INVALID_ARGUMENT", "lane is DECIDE or FYI");
      const kinds = new Set<Kind>();
      for (const k of req.query.getAll("kind")) {
        if (!KINDS[k])
          return fail("INVALID_ARGUMENT", "kind is NAME, CLAIM or VALUE");
        kinds.add(KINDS[k]);
      }
      const type = req.query.get("type") ?? "",
        doc = req.query.get("documentId") ?? "",
        token = req.query.get("pageToken") ?? "";
      let after: Cursor | undefined;
      const [tokenLoad, rest] = token ? token.split(/:(.*)/s) : ["", ""];
      if (token) {
        try {
          after = JSON.parse(
            Buffer.from(rest ?? "", "base64url").toString(),
          ) as Cursor;
        } catch {
          after = undefined;
        }
        if (!tokenLoad || !after || typeof after.i !== "string")
          return fail(
            "INVALID_ARGUMENT",
            "page_token is not one this list returned",
          );
      }
      const load = reviewLoad(s.o);
      if (typeof load !== "string") return load;
      if (tokenLoad && tokenLoad !== load)
        return fail(
          "FAILED_PRECONDITION",
          "another load became current since the first page: list the cards again",
        );
      const index = indexOf(s.o, load),
        saved = decisions.get(s.o.id) || [];
      const open = (c: Card) => isOpen(c, saved, load);
      const names = !kinds.size || kinds.has("resolve");
      const byId = new Map(index.cards.map((c) => [c.id, c]));
      const listed = index.cards
        .filter(
          (c) =>
            c.lane === lane &&
            open(c) &&
            (!kinds.size || kinds.has(c.kind)) &&
            (!names ||
              !c.onCard ||
              !byId.get(c.onCard) ||
              !open(byId.get(c.onCard)!)) &&
            (!type ||
              c.type === type ||
              c.subjectType === type ||
              c.objectType === type) &&
            (!doc || c.docId === doc),
        )
        .sort(walkOrder)
        .filter(
          (c) =>
            !after ||
            c.perDecision < after.p ||
            (c.perDecision === after.p &&
              (c.facts < after.f ||
                (c.facts === after.f &&
                  (kindOrder(c.kind) > after.k ||
                    (kindOrder(c.kind) === after.k &&
                      (c.shuffle > after.s ||
                        (c.shuffle === after.s && c.id > after.i))))))),
        );
      const size = knobs.reviewCardsPage || PAGE,
        page = listed.slice(0, size),
        last = page.at(-1);
      const claim = (c: Card): FoundryReviewClaimCard => ({
        reviewKey: c.reviewKey,
        band: c.band!,
        decisions: saved
          .filter(
            (d) =>
              (d.kind === "release" || d.kind === "reject") &&
              d.docId === c.docId &&
              d.claimSection === c.claimSection &&
              d.claimStart === c.claimStart &&
              d.claimEnd === c.claimEnd,
          )
          .map((d) => withApplied(s.o.id, d)),
      });
      const cards = page.map(
        (c): FoundryReviewCard => ({
          cardId: c.id,
          lane:
            c.lane === "fyi"
              ? "FOUNDRY_REVIEW_LANE_FYI"
              : "FOUNDRY_REVIEW_LANE_DECIDE",
          docId: c.docId,
          facts: c.facts,
          ...(c.kind === "resolve"
            ? {
                name: {
                  reviewKey: c.reviewKey,
                  reviewKeys: c.reviewKeys,
                  reviewCount: c.reviewCount,
                  blocked: c.blocked,
                  held: index.cards
                    .filter((h) => h.onCard === c.id && open(h))
                    .sort(
                      (a, b) => a.shuffle - b.shuffle || (a.id < b.id ? -1 : 1),
                    )
                    .map(claim),
                  type: c.type!,
                  normName: c.normName!,
                },
              }
            : c.kind === "release"
              ? { claim: claim(c) }
              : { value: { reviewKey: c.reviewKey } }),
          ...(() => {
            const advice =
              knobs.reviewModelAdvice === false
                ? undefined
                : modelAdviceOf(
                    c,
                    index.reviews.get(c.reviewKey),
                    knobs.reviewModelAdviceRationale,
                  );
            return advice ? { modelAdvice: advice } : {};
          })(),
        }),
      );
      const nameKeys = [
        ...new Set(
          page.flatMap((c) =>
            c.kind === "resolve" ? [JSON.stringify([c.type, c.normName])] : [],
          ),
        ),
      ];
      const nameDecisions = nameKeys.flatMap(
        (k): FoundryReviewNameDecisions[] => {
          const [ty, norm] = JSON.parse(k) as [string, string];
          const of = saved.filter(
            (d) => d.kind === "resolve" && d.type === ty && d.normName === norm,
          );
          return of.length
            ? [
                {
                  type: ty,
                  normName: norm,
                  total: of.length,
                  choices: new Set(
                    of.map((d) => (d.kind === "resolve" && d.entityKey) || ""),
                  ).size,
                  newest: of
                    .toSorted(
                      (a, b) =>
                        (b.decidedAt ?? "").localeCompare(a.decidedAt ?? "") ||
                        b.id.localeCompare(a.id),
                    )
                    .slice(0, 20)
                    .map((d) => withApplied(s.o.id, d)),
                },
              ]
            : [];
        },
      );
      return ok({
        loadId: load,
        indexing: !!knobs.reviewIndexing,
        cards,
        names: nameDecisions,
        ...(token
          ? {}
          : { precision: precision(s.o.id, req.principal.user.id) }),
        nextPageToken:
          listed.length > size && last
            ? `${load}:${Buffer.from(
                JSON.stringify({
                  p: last.perDecision,
                  f: last.facts,
                  k: kindOrder(last.kind),
                  s: last.shuffle,
                  i: last.id,
                } satisfies Cursor),
              ).toString("base64url")}`
            : "",
      } satisfies ListFoundryReviewCardsResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/review-spot-checks`,
    handle: (req) => {
      const s = ontologyOf(req, false);
      if (!isScope(s)) return s;
      const load = reviewLoad(s.o);
      if (typeof load !== "string") return load;
      return ok(spotChecks(s.o, indexOf(s.o, load), req.principal.user.id));
    },
  },
  {
    method: "POST",
    path: `${BASE}/review-spot-checks:draw`,
    handle: (req) => {
      const s = ontologyOf(req, false);
      if (!isScope(s)) return s;
      const load = reviewLoad(s.o);
      if (typeof load !== "string") return load;
      if (req.body?.loadId !== load)
        return fail(
          "FAILED_PRECONDITION",
          "load_id is not the current load: read the spot checks again",
        );
      const index = indexOf(s.o, load);
      // Nothing is drawn while the index is first built, as the backend.
      if (knobs.reviewIndexing)
        return ok(spotChecks(s.o, index, req.principal.user.id));
      // The bands this request found none of, whatever the counts say.
      const noHeld = new Set<number>(),
        noQuotes = new Set<number>();
      for (let t = 0; t < DRAW_TRIES; t++) {
        const drawn = draws.get(load) ?? [];
        if (drawn.filter((d) => !done(s.o.id, d)).length >= SPOT_CHECKS) break;
        const seq = Math.max(-1, ...drawn.map((d) => d.seq)) + 1;
        const p = pools(s.o.id, index).map((x, b) => ({
          held: noHeld.has(b) ? 0 : x.held,
          quotes: noQuotes.has(b) ? 0 : x.quotes,
        }));
        const next = nextDraw(load, seq, p);
        if (!next) break;
        if (!next.quote) {
          if (!drawHeld(s.o.id, index, seq, next.band)) noHeld.add(next.band);
        } else if (
          !drawQuote(s.o.id, index, seq, next.band, p[next.band]!.quotes)
        )
          noQuotes.add(next.band);
      }
      return ok(spotChecks(s.o, index, req.principal.user.id));
    },
  },
];
