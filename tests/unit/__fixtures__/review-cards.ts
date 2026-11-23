import type {
  DecisionApplied,
  FoundryPropValue,
  FoundryReviewDecision,
  FoundryReviewNameDecisions,
} from "@/shared/wfe";
import type { EntityRow } from "@/web/areas/explore/model";
import { adviceContext } from "@/web/areas/reviews/advice";
import { statementHash, type Unsettled } from "@/web/areas/reviews/model";
import {
  bandOf,
  type ClaimCard,
  type NameCard,
  type Precision,
  type QueueReview,
  type ValueCard,
} from "@/web/areas/reviews/queue";

// Cards and decisions of the review walkthrough's unit tests.

const key = (n: number) => n.toString(16).padStart(64, "0");

export function heldCard(
  n: number,
  {
    doc = "doc-1",
    // null: no score (unverified).
    confidence = 0.95 as number | null,
    link = "partner_of",
    named = true,
    differs = false,
    conflict = false,
    unsettled = undefined as Unsettled[] | undefined,
    quote = `quote ${n}`,
    subjectType = "Account",
  } = {},
): ClaimCard {
  const subject = { type: subjectType, name: `Acme ${n}` },
    object = { type: "Account", name: `Globex ${n}` };
  const stored = JSON.stringify({ subject, object, props: {} });
  const review: QueueReview = {
    documentId: doc,
    section: 0,
    start: n * 10,
    end: n * 10 + 8,
    quote,
    reason: "low_confidence",
    ...(confidence === null ? {} : { confidence }),
    suggestedLink: link,
    candidates: [],
    claim: { place: [key(n), 0, n * 10, n * 10 + 8].join("\u001f"), stored },
    settle: {
      kind: "release",
      reviewId: `held-${n}`,
      docId: doc,
      claimKey: key(n),
      section: 0,
      start: n * 10,
      end: n * 10 + 8,
      link,
      statement: { subject, object, props: {} },
      stored,
      hash: statementHash(link, stored),
      named,
      ...(differs ? { differs } : {}),
      ...(unsettled ? { unsettled } : {}),
      ...(conflict ? { conflict: true as const } : {}),
    },
  };
  return {
    kind: "claim",
    key: `held-${n}`,
    review,
    band: bandOf(confidence ?? undefined),
  };
}

export const entity = (k: string, name: string): EntityRow => ({
  dbId: 1,
  id: `e-${k}`,
  type: "Customer",
  name,
  identity: { type: "Customer", key: k },
  props: {},
});

export function nameCard(
  normName: string,
  {
    doc = "doc-1",
    type = "Customer",
    reasons = ["unresolved"],
    candidates = [] as { key: string; entity?: EntityRow }[],
    held = [] as ClaimCard[],
  } = {},
): NameCard {
  const reviews = reasons.map(
    (reason, i): QueueReview => ({
      documentId: doc,
      section: 1,
      start: i * 10,
      end: i * 10 + 4,
      quote: `${normName} signed`,
      reason,
      candidates,
      settle: {
        kind: "resolve",
        reviewId: `${doc}-${normName}-${i}`,
        type,
        docId: doc,
        normName,
      },
    }),
  );
  return {
    kind: "name",
    key: JSON.stringify([type, doc, normName]),
    // As the index lays it out: a name only the model linked is for
    // information.
    lane: reasons.every((r) => r === "adjudicated") ? "fyi" : "decide",
    review: reviews[0]!,
    reviews,
    reviewCount: reviews.length,
    blocked: 0,
    held,
  };
}

export function valueCard(
  values: FoundryPropValue[],
  { table = undefined as string | undefined, id = "v-1" } = {},
): ValueCard {
  return {
    kind: "value",
    key: id,
    // A value a table keeps against documents is for information.
    lane: table === undefined ? "decide" : "fyi",
    review: {
      documentId: "doc-1",
      section: 2,
      start: 0,
      end: 5,
      quote: "billed in GBP",
      reason: "prop_conflict",
      prop: "currency",
      candidates: [],
      settle: {
        kind: "prop",
        reviewId: id,
        type: "Customer",
        entityKey: "c1",
        prop: "currency",
        values: values.map((value, i) => ({
          value: value as Exclude<FoundryPropValue, { null: null }>,
          documentId: `doc-${i}`,
          quote: `q${i}`,
          section: 0,
          start: 0,
          end: 1,
        })),
        ...(table !== undefined ? { table } : {}),
      },
    },
  };
}

export const resolveDecision = (
  id: string,
  {
    doc = "doc-2",
    type = "Customer",
    normName = "umbrella holdings",
    entityKey = undefined as string | undefined,
    user = "u-1",
    at = "2026-11-01T00:00:00Z",
    // What the current load made of it; unset: not in its list.
    applied = undefined as DecisionApplied["status"] | undefined,
  } = {},
): FoundryReviewDecision => ({
  id,
  kind: "resolve",
  type,
  docId: doc,
  normName,
  ...(entityKey ? { entityKey } : {}),
  decidedByUserId: user,
  decidedAt: at,
  ...(applied !== undefined ? { applied: { status: applied } } : {}),
});

export const noBands: Precision = {
  bands: [],
  rechecks: { agreed: 0, total: 0 },
};

/**
 * The index's names of `decisions` (mock/routes/review-cards.ts's): each
 * name's resolve decisions, how many choices they make, newest first.
 */
export const nameDecisions = (
  decisions: readonly FoundryReviewDecision[],
): FoundryReviewNameDecisions[] => {
  const by = new Map<string, FoundryReviewDecision[]>();
  for (const d of decisions)
    if (d.kind === "resolve") {
      const at = JSON.stringify([d.type, d.normName]);
      by.set(at, [...(by.get(at) ?? []), d]);
    }
  return [...by].map(([at, of]) => {
    const [type, normName] = JSON.parse(at) as [string, string];
    return {
      type,
      normName,
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
        .slice(0, 20),
    };
  });
};

export const ctxOf = ({
  decisions = [] as FoundryReviewDecision[],
  precision = noBands,
  single = (_: string) => false,
  user = "u-1",
} = {}) =>
  adviceContext({
    names: nameDecisions(decisions),
    claims: decisions,
    precision,
    single,
    user,
  });
