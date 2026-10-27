import type {
  FoundryPropValue,
  FoundryReviewChoice,
  FoundryReviewDecision,
  ReviewDecision,
} from "../../../shared/wfe";
import type { ReviewReason } from "../documents/model";
import type { EntityRow } from "../explore/model";

// What a FoundryReview lets a person settle, read once
// from the stored record, and the choice the backend's SettleFoundryReview takes. The
// backend reads the review's identity from the current load itself; the
// browser sends only a choice, so nothing here is identity it trusts.

/** A value a document states: what settling a property review sends. */
export type StatedValue = Exclude<FoundryPropValue, { null: null }>;

/** A document's quote stating a value of a property a review is about. */
export type ReviewValue = {
  value: StatedValue;
  documentId: string;
  quote: string;
  section: number;
  start: number;
  end: number;
};

/**
 * A quote's JSON value as SettleFoundryReview takes it: a string as text
 * (string, date and datetime props), an integer literal as integer with
 * its exact digits, any other number as number.
 */
function statedValue(v: unknown): StatedValue | undefined {
  if (typeof v === "string") return { text: v };
  if (typeof v === "boolean") return { boolean: v };
  if (typeof v === "bigint") return { integer: v.toString() };
  if (typeof v === "number" && Number.isFinite(v)) return { number: v };
  return undefined;
}

/**
 * Engine `values` is JSON text containing PropQuote records, never markup.
 * A value written as an integer is read from its JSON text, so one past
 * 2^53 keeps every digit.
 *
 * @example
 * reviewValues('[{"value": 9007199254740993, "doc_id": "d", "quote": "q", "span": {"section": 0, "start": 0, "end": 1}}]')[0].value;
 * // { integer: "9007199254740993" }
 */
export function reviewValues(raw: unknown): ReviewValue[] {
  if (typeof raw !== "string") return [];
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw, (key, v, context?: { source?: string }) =>
      key === "value" &&
      typeof v === "number" &&
      /^-?\d+$/.test(context?.source ?? "")
        ? BigInt(context!.source!)
        : v,
    );
  } catch {
    return [];
  }
  if (!Array.isArray(parsed)) return [];
  return parsed.flatMap((v): ReviewValue[] => {
    if (!v || typeof v !== "object" || !v.span || typeof v.span !== "object")
      return [];
    const { section, start, end } = v.span;
    const value = statedValue(v.value);
    return value &&
      typeof v.doc_id === "string" &&
      !!v.doc_id &&
      typeof v.quote === "string" &&
      [section, start, end].every((n) => Number.isSafeInteger(n) && n >= 0) &&
      end > start
      ? [
          {
            value,
            documentId: v.doc_id,
            quote: v.quote,
            section,
            start,
            end,
          },
        ]
      : [];
  });
}

const isScalar = (v: unknown): v is string | number | boolean =>
  typeof v === "string" ||
  typeof v === "boolean" ||
  (typeof v === "number" && Number.isFinite(v));

/** The reasons a resolve decision settles; `prop_conflict` takes a value. */
const RESOLVE_REASONS: ReadonlySet<ReviewReason> = new Set<ReviewReason>([
  "unresolved",
  "ambiguous",
  "collision",
  "identifier_conflict",
  "adjudicated",
]);

/**
 * An entity a resolve review suggests: its identity as the graph stores it
 * (`entity_key`, the key's tagged encoding, or a named type's `norm_name`)
 * and the entity, when the read found it among those the review concerns.
 */
export type Candidate = { key: string; entity?: EntityRow };

/**
 * What a person can settle a review with:
 * - `resolve`: link the document's name to a candidate, or say it is not in
 *   the data;
 * - `prop`: choose a value of an entity's property that documents state
 *   differently. `table` is the value stored for it, if any: only a table
 *   row's value survives a conflict, so it is the table's, and a
 *   chosen document value replaces it in every later load;
 * - `stale`: a reason a decision settles, on a review written without the
 *   identity the backend reads (a load from before decisions) or whose
 *   entity this read doesn't find. A new load writes it again.
 */
export type Settle =
  | {
      kind: "resolve";
      reviewId: string;
      type: string;
      docId: string;
      normName: string;
    }
  | {
      kind: "prop";
      reviewId: string;
      type: string;
      entityKey: string;
      prop: string;
      values: ReviewValue[];
      table?: string | number | boolean;
    }
  | { kind: "stale" };

/** A review's candidates and what settles it (none for a reason no decision settles). */
export type Settleable = { candidates: Candidate[]; settle?: Settle };

const text = (v: unknown) => (typeof v === "string" && v ? v : undefined);
/** Whether an entity row is the graph's entity of this type and stored identity. */
const isEntity = (e: EntityRow, type: string, key: string) =>
  e.type === type &&
  ("key" in e.identity ? e.identity.key : e.identity.name) === key;

/**
 * Reads a FoundryReview record, given the entities it concerns
 * (FOUNDRY_CONCERNS: a resolve review's candidates and a fact's ends, a
 * property review's entity). Nothing in either is trusted.
 *
 * @example
 * settleable({ review_key: "r", reason: "unresolved", type: "Customer", doc_id: "d", norm_name: "globexx", candidates: ["scus_1"] }, []);
 * // { candidates: [{ key: "scus_1" }], settle: { kind: "resolve", reviewId: "r", type: "Customer", docId: "d", normName: "globexx" } }
 * settleable({ review_key: "r", reason: "unresolved", candidates: [] }, []).settle; // { kind: "stale" }
 * settleable({ reason: "low_confidence" }, []).settle; // undefined
 */
export function settleable(
  r: Record<string, unknown>,
  concerned: readonly EntityRow[],
): Settleable {
  const reason = text(r.reason) as ReviewReason | undefined,
    reviewId = text(r.review_key),
    type = text(r.type);
  const keys = Array.isArray(r.candidates)
    ? [...new Set(r.candidates.filter((c): c is string => !!text(c)))]
    : [];
  const candidates = keys.map((key): Candidate => {
    const entity = type && concerned.find((e) => isEntity(e, type, key));
    return entity ? { key, entity } : { key };
  });
  if (reason && RESOLVE_REASONS.has(reason)) {
    const docId = text(r.doc_id),
      normName = text(r.norm_name);
    return {
      candidates,
      settle:
        reviewId && type && docId && normName
          ? { kind: "resolve", reviewId, type, docId, normName }
          : { kind: "stale" },
    };
  }
  if (reason !== "prop_conflict") return { candidates };
  const entityKey = text(r.entity_key),
    prop = text(r.prop),
    values = reviewValues(r.values);
  const entity =
    type && entityKey && concerned.find((e) => isEntity(e, type, entityKey));
  if (!reviewId || !type || !entityKey || !prop || !entity || !values.length)
    return { candidates, settle: { kind: "stale" } };
  const stored = entity.props[prop];
  return {
    candidates,
    settle: {
      kind: "prop",
      reviewId,
      type,
      entityKey,
      prop,
      values,
      ...(isScalar(stored) ? { table: stored } : {}),
    },
  };
}

/**
 * The choice to send for a review: a candidate it lists or "not in data"
 * (`null`) for a resolve review, a value one of its quotes states for a
 * property review. Anything else is refused.
 *
 * @example
 * decisionFor(review, { entityKey: "scus_1" }); // { resolve: { entityKey: "scus_1" } }
 * decisionFor(review, { entityKey: null }); // { resolve: { notInData: true } }
 * decisionFor(conflict, { value: { integer: "36" } }); // { prop: { integer: "36" } }
 */
export function decisionFor(
  r: Settleable,
  choice: { entityKey: string | null } | { value: StatedValue | undefined },
): FoundryReviewChoice | undefined {
  switch (r.settle?.kind) {
    case "resolve": {
      if (!("entityKey" in choice)) return;
      const key = choice.entityKey;
      if (key === null) return { resolve: { notInData: true } };
      return r.candidates.some((c) => c.key === key)
        ? { resolve: { entityKey: key } }
        : undefined;
    }
    case "prop": {
      if (!("value" in choice)) return;
      const value = choice.value;
      return value &&
        r.settle.values.some(
          (v) => JSON.stringify(v.value) === JSON.stringify(value),
        )
        ? { prop: value }
        : undefined;
    }
    default:
      return undefined;
  }
}

/** Whether a saved decision is the one for this review's identity. */
export function matchesReview(d: ReviewDecision, s: Settle) {
  switch (s.kind) {
    case "resolve":
      return (
        d.kind === "resolve" &&
        d.type === s.type &&
        d.docId === s.docId &&
        d.normName === s.normName
      );
    case "prop":
      return (
        d.kind === "prop" &&
        d.type === s.type &&
        d.entityKey === s.entityKey &&
        d.prop === s.prop
      );
    case "stale":
      return false;
  }
}

/**
 * Whether a load read a decision. A load reads the decisions saved when it
 * starts (the backend snapshots them), and replacing one saves it anew, so one saved
 * before the load started is in it, and one saved after waits for the next
 * load. A load whose start isn't known read none.
 *
 * @example
 * loadRead({ decidedAt: "2026-10-20T10:00:00Z" }, "2026-10-20T11:00:00Z"); // true
 * loadRead({ decidedAt: "2026-10-20T12:00:00Z" }, "2026-10-20T11:00:00Z"); // false
 */
export function loadRead(
  d: Pick<FoundryReviewDecision, "decidedAt">,
  loadCreatedAt?: string | null,
): boolean {
  const decided = Date.parse(d.decidedAt || ""),
    started = Date.parse(loadCreatedAt || "");
  return decided < started;
}

/**
 * A prop decision's value as the page shows it, as `valueText` shows a
 * scalar; a removal shows nothing. An integer keeps every digit.
 *
 * @example
 * propValueText({ integer: "9007199254740993" }); // "9,007,199,254,740,993"
 * propValueText({ boolean: false }); // "No"
 * propValueText({ null: null }); // ""
 */
export function propValueText(v: FoundryPropValue): string {
  if (v.text !== undefined) return v.text;
  if (v.boolean !== undefined) return v.boolean ? "Yes" : "No";
  const n = v.integer ?? v.number;
  // Intl formats a numeric string exactly.
  return n === undefined
    ? ""
    : new Intl.NumberFormat("en-US").format(
        n as number | Intl.StringNumericLiteral,
      );
}
