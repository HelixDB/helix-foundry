import type {
  FoundryPropValue,
  FoundryReviewChoice,
  FoundryReviewDecision,
  ReviewDecision,
} from "../../../shared/wfe";
import type { ReviewReason } from "../documents/model";
import type { DocFact, EntityRow } from "../explore/model";
import { normalizeName } from "../explore/naming";

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

const isOffset = (v: unknown): v is number =>
  typeof v === "number" && Number.isSafeInteger(v) && v >= 0;
const isScalar = (v: unknown): v is string | number | boolean =>
  typeof v === "string" ||
  typeof v === "boolean" ||
  (typeof v === "number" && Number.isFinite(v));

/** The reasons a person's release settles: a verifier doubted the claim. */
const RELEASE_REASONS: ReadonlySet<ReviewReason> = new Set<ReviewReason>([
  "low_confidence",
  "unverified",
]);

/** The reasons a resolve decision settles; `prop_conflict` takes a value. */
const RESOLVE_REASONS: ReadonlySet<ReviewReason> = new Set<ReviewReason>([
  "unresolved",
  "ambiguous",
  "collision",
  "identifier_conflict",
  "adjudicated",
  "legal_form",
]);

/**
 * An entity a resolve review suggests: its identity as the graph stores it
 * (`entity_key`, the key's tagged encoding, or a named type's `norm_name`)
 * and the entity, when the read found it among those the review concerns.
 */
export type Candidate = { key: string; entity?: EntityRow };

/** An end of what a held claim states, as the document names it. */
export type StatedEnd = { type: string; name: string };
/**
 * What approving a held claim states: its ends in order, and its properties,
 * each as stored, to the digit: it is what the engine writes.
 */
export type Statement = {
  subject: StatedEnd;
  object: StatedEnd;
  props: Record<string, string>;
};

/**
 * What a person can settle a review with:
 * - `resolve`: link the document's name to a candidate, or say it is not in
 *   the data;
 * - `prop`: choose a value of an entity's property that documents state
 *   differently. `table` is the value stored for it, if any: only a table
 *   row's value survives a conflict, so it is the table's, and a
 *   chosen document value replaces it in every later load;
 * - `release`: approve one review of a claim code holds back, so the next
 *   load writes what it states as a fact. `statement` is what that is, and
 *   `stored` it as the review keeps it, which its hash reads. `named` is
 *   whether the review names both its ends: if not, an end isn't in the graph
 *   yet. `differs` is whether another review at its place states something
 *   else: no approval says which, so the engine refuses both. `formed` is an
 *   end's name a legal form linked, which a person settles before a link that
 *   holds one value is written;
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
  | {
      kind: "release";
      reviewId: string;
      docId: string;
      claimKey: string;
      section: number;
      start: number;
      end: number;
      link: string;
      statement: Statement;
      /** The statement as the review stores it, which its hash reads. */
      stored: string;
      named: boolean;
      differs?: boolean;
      formed?: string;
    }
  | { kind: "stale" };

/**
 * A review's candidates and what settles it (none for a reason no decision
 * settles). `claim` is the place and statement of a review of a held claim,
 * whatever its reason: the engine compares all of them at a release.
 */
export type Settleable = {
  candidates: Candidate[];
  settle?: Settle;
  claim?: { place: string; stored?: string };
};

const text = (v: unknown) => (typeof v === "string" && v ? v : undefined);
/** Whether an entity row is the graph's entity of this type and stored identity. */
const isEntity = (e: EntityRow, type: string, key: string) =>
  e.type === type &&
  ("key" in e.identity ? e.identity.key : e.identity.name) === key;

/** How many entities a held claim's stored JSON (`claim`) names; none if it doesn't read. */
function ends(claim: unknown): number {
  try {
    const about = (JSON.parse(String(claim)) as { about?: unknown }).about;
    return Array.isArray(about) ? about.length : 0;
  } catch {
    return 0;
  }
}

/**
 * A review's stored `statement`, nothing in which is trusted. Each property
 * reads as stored: a number from its JSON text, as a parsed one may round.
 */
function statement(raw: string): Statement | undefined {
  let parsed: Partial<Statement> | null;
  let exact: Partial<Statement> | null;
  try {
    parsed = JSON.parse(raw) as Partial<Statement> | null;
    exact = JSON.parse(raw, (_, v: unknown, context?: { source?: string }) =>
      typeof v === "number" ? (context?.source ?? String(v)) : v,
    ) as Partial<Statement> | null;
  } catch {
    return undefined;
  }
  const end = (e?: StatedEnd) =>
    e && text(e.type) && text(e.name)
      ? { type: e.type, name: e.name }
      : undefined;
  const subject = end(parsed?.subject),
    object = end(parsed?.object);
  if (!subject || !object) return undefined;
  const props = Object.fromEntries(
    Object.entries(parsed?.props ?? {}).map(([name, v]) => [
      name,
      isScalar(v) ? String(exact?.props?.[name]) : JSON.stringify(v),
    ]),
  );
  return { subject, object, props };
}

/**
 * Reads a FoundryReview record, given the entities it concerns
 * (FOUNDRY_CONCERNS: a resolve review's candidates and a fact's ends, a
 * property review's entity). Nothing in either is trusted.
 *
 * @example
 * settleable({ review_key: "r", reason: "unresolved", type: "Customer", doc_id: "d", norm_name: "globexx", candidates: ["scus_1"] }, []);
 * // { candidates: [{ key: "scus_1" }], settle: { kind: "resolve", reviewId: "r", type: "Customer", docId: "d", normName: "globexx" } }
 * settleable({ review_key: "r", reason: "unresolved", candidates: [] }, []).settle; // { kind: "stale" }
 * settleable({ review_key: "r", reason: "low_confidence", doc_id: "d", claim_key: "k", section: 0, start: 4, end: 9, claim: '{"about":[1,2]}' }, []).settle;
 * // { kind: "release", reviewId: "r", docId: "d", claimKey: "k", section: 0, start: 4, end: 9, named: true }
 * settleable({ reason: "low_confidence" }, []).settle; // { kind: "stale" }
 */
export function settleable(
  r: Record<string, unknown>,
  concerned: readonly EntityRow[],
): Settleable {
  const settled = settleOf(r, concerned),
    claimKey = text(r.claim_key),
    { section, start, end } = r;
  if (!claimKey || !isOffset(section) || !isOffset(start) || !isOffset(end))
    return settled;
  const stored = text(r.statement);
  return {
    ...settled,
    claim: {
      place: [claimKey, section, start, end].join("\u001f"),
      ...(stored ? { stored } : {}),
    },
  };
}

function settleOf(
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
  if (reason && RELEASE_REASONS.has(reason)) {
    const docId = text(r.doc_id),
      claimKey = text(r.claim_key),
      link = text(r.suggested_link),
      stored = text(r.statement),
      stated = stored ? statement(stored) : undefined,
      { section, start, end } = r;
    // An approval is of what its card shows: a review of a load before
    // statements shows nothing to approve.
    if (
      !link ||
      !stored ||
      !stated ||
      !reviewId ||
      !docId ||
      !claimKey ||
      !isOffset(section) ||
      !isOffset(start) ||
      !isOffset(end)
    )
      return { candidates, settle: { kind: "stale" } };
    return {
      candidates,
      settle: {
        kind: "release",
        reviewId,
        docId,
        claimKey,
        section,
        start,
        end,
        link,
        statement: stated,
        stored,
        named: ends(r.claim) >= 2,
      },
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
 * What binds an approval to the claim a person read: the SHA-256 of what its
 * card shows, as the backend and the engine hash it (held::Statement::hash).
 *
 * @example
 * await statementHash("partner_of", stored); // 64 lowercase hexadecimal digits
 */
export async function statementHash(
  link: string,
  stored: string,
): Promise<string> {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(`${link}\u001f${stored}`),
  );
  return [...new Uint8Array(digest)]
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

/**
 * Marks the held claims of `reviews` whose approval the engine would refuse or
 * hold: those that share a place and a key with a review of any reason that
 * states something else, as no approval says which (`differs`), and those
 * naming an entity by a name a legal form linked in that document (`formed`).
 *
 * @example
 * markDiffering([one, sameKeyAndPlaceOtherStatement]); // both settle with `differs: true`
 */
export function markDiffering<T extends Settleable & { reason?: string }>(
  reviews: T[],
): T[] {
  const stated = new Map<string, Set<string>>();
  for (const { claim } of reviews) {
    if (!claim) continue;
    const seen = stated.get(claim.place) ?? new Set<string>();
    stated.set(claim.place, seen.add(claim.stored ?? ""));
  }
  // A name resolves per document and type, so a link in another holds nothing.
  const end = (docId: string, type: string, name: string) =>
    JSON.stringify([docId, type, name]);
  const linked = new Set(
    reviews.flatMap((r) =>
      r.reason === "legal_form" && r.settle?.kind === "resolve"
        ? [end(r.settle.docId, r.settle.type, r.settle.normName)]
        : [],
    ),
  );
  return reviews.map((r) => {
    const s = r.settle;
    if (s?.kind !== "release") return r;
    const differs = (stated.get(r.claim?.place ?? "")?.size ?? 0) > 1,
      formed = [s.statement.subject, s.statement.object]
        .map(({ type, name }) => ({ type, name: normalizeName(name) }))
        .find(({ type, name }) => linked.has(end(s.docId, type, name)))?.name;
    return differs || formed
      ? {
          ...r,
          settle: {
            ...s,
            ...(differs ? { differs } : {}),
            ...(formed ? { formed } : {}),
          },
        }
      : r;
  });
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
    case "release":
      return (
        d.kind === "release" &&
        d.docId === s.docId &&
        d.claimKey === s.claimKey &&
        d.claimSection === s.section &&
        d.claimStart === s.start &&
        d.claimEnd === s.end
      );
    case "stale":
      return false;
  }
}

/**
 * How a person's saved approvals stand toward a held claim's card, `read`
 * being the hash of what it shows. `saved` is the approval of this review.
 * On the load a person can settle, `moved` is that it approved what the claim
 * said before: the engine refuses it. `earlier` is that this statement was
 * approved at this place under another key, which it held before: a sibling
 * claim of the sentence states something else. An older load's statement may
 * not be the current one, so it says neither.
 *
 * @example
 * approvalOf([], release, "abc", true); // { saved: undefined, moved: false, earlier: false }
 */
export function approvalOf<D extends ReviewDecision>(
  decisions: readonly D[],
  s: Extract<Settle, { kind: "release" }>,
  read: string | undefined,
  editable: boolean,
): { saved?: D; moved: boolean; earlier: boolean } {
  const saved = decisions.find((d) => matchesReview(d, s));
  return {
    saved,
    moved:
      editable &&
      !!read &&
      saved?.kind === "release" &&
      saved.statementHash !== read,
    earlier:
      editable &&
      !!read &&
      !saved &&
      decisions.some(
        (d) =>
          d.kind === "release" &&
          d.docId === s.docId &&
          d.claimKey !== s.claimKey &&
          d.claimSection === s.section &&
          d.claimStart === s.start &&
          d.claimEnd === s.end &&
          d.statementHash === read,
      ),
  };
}

/** What became of a release decision, as far as a page's reads say. */
export type ReleaseStatus = "written" | "no_link" | "held" | "unknown";

/**
 * What became of `d` on a document's page, from its reviews and its facts:
 * its review is still held, or its quote stands with an edge, or without one,
 * as a later statement holds that link. Otherwise the reads don't say.
 *
 * @example
 * releaseStatus(decision, reviews, facts); // "held"
 */
export function releaseStatus(
  d: Extract<ReviewDecision, { kind: "release" }>,
  reviews: readonly Settleable[],
  facts: readonly DocFact[],
): ReleaseStatus {
  const here = (at: { section: number; start: number; end: number }) =>
    at.section === d.claimSection &&
    at.start === d.claimStart &&
    at.end === d.claimEnd;
  const held = reviews.some(
    ({ settle: s }) =>
      s?.kind === "release" && s.claimKey === d.claimKey && here(s),
  );
  if (held) return "held";
  const fact = facts.find((f) => !f.unread && f.quotes.some(here));
  if (!fact) return "unknown";
  return fact.from ? "written" : "no_link";
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
