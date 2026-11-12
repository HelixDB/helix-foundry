import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import type {
  DecisionApplied,
  FoundryPropValue,
  FoundryReviewChoice,
  FoundryReviewDecision,
  ReviewDecision,
} from "../../../shared/wfe";
import { reviewCopy, type ReviewReason } from "../documents/model";
import type { DocFact, EntityRow } from "../explore/model";
import { normalizeName } from "../explore/naming";
import { MAX_GOAL } from "../ontology/model";

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

/**
 * The reasons a person's release settles: a verifier doubted the claim, or,
 * from contract 4.14.0, it conflicts with what its link holds. An approval
 * of the latter is written only on a link that holds one value, where it
 * decides the link; on another link the conflict is a property that differs
 * from the fact the graph holds, which no approval changes (`conflict`).
 */
const RELEASE_REASONS: ReadonlySet<ReviewReason> = new Set<ReviewReason>([
  "low_confidence",
  "unverified",
  "conflict",
]);

/**
 * How a load links a name on less than the name itself or a verified
 * identifier: by its legal form, by its being part of one row's name, or by
 * an identifier the verifier held back that is the name the text calls the
 * entity by. Each such link has a review of that reason, which a resolve
 * decision confirms or overturns.
 */
export type WeakLink = "legal_form" | "part_name" | "own_name";
export const WEAK_LINKS: readonly WeakLink[] = [
  "legal_form",
  "part_name",
  "own_name",
];

/**
 * Whether `reason` (a review's, or a mention's `resolved_by`) is a link made
 * on less than a name.
 *
 * @example
 * isWeakLink("part_name"); // true
 * isWeakLink("adjudicated"); // false
 */
export const isWeakLink = (reason?: string): reason is WeakLink =>
  WEAK_LINKS.some((w) => w === reason);

/**
 * Whether `reason` is a link the load made that a person can undo: the
 * model's, or one made on less than a name. Its review lists the row it
 * linked, and its mentions link to that review.
 *
 * @example
 * isUndoableLink("adjudicated"); // true
 * isUndoableLink("unresolved"); // false
 */
export const isUndoableLink = (reason?: string) =>
  reason === "adjudicated" || isWeakLink(reason);

/** The reasons a resolve decision settles; `prop_conflict` takes a value. */
const RESOLVE_REASONS: ReadonlySet<ReviewReason> = new Set<ReviewReason>([
  "unresolved",
  "ambiguous",
  "collision",
  "identifier_conflict",
  "adjudicated",
  ...WEAK_LINKS,
]);

/**
 * An end of a held claim whose name the load linked on less than a name:
 * the name as the document writes it (the claim's statement), how it was
 * linked, and where that link's review is in the document's text (`at`,
 * when the page read its place).
 */
export type Unsettled = {
  name: string;
  by: WeakLink;
  at?: { section: number; start: number };
};

/**
 * The ends of a held claim (`unsettled`, in the order the engine refuses
 * them) for which the engine refuses an approval until a person settles the
 * name: always one linked by part of a name or by an own-name identifier,
 * and one linked by legal form only where the claim's link holds one value
 * (`single`). None: an approval can be written at once.
 *
 * @example
 * holdingEnds([{ name: "Westmere", by: "part_name" }], false).length; // 1
 * holdingEnds([{ name: "Globex Ltd", by: "legal_form" }], false).length; // 0
 * holdingEnds(undefined, true).length; // 0
 */
export const holdingEnds = (
  unsettled: readonly Unsettled[] | undefined,
  single: boolean,
): Unsettled[] =>
  (unsettled ?? []).filter((u) => u.by !== "legal_form" || single);

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
 *   differently, which the current load is sent at once. `table` is the
 *   value stored for it, if any: only a table row's value survives a
 *   conflict, so it is the table's, and a chosen document value replaces it
 *   in this load and every later one;
 * - `release`: approve one review of a claim code holds back, so the current
 *   load writes what it states as a fact, at once, or reject it, which hides
 *   its card and writes nothing. `statement` is what that is, and
 *   `stored` it as the review keeps it, which its hash reads (`hash`,
 *   statementHash). `named` is
 *   whether the review names both its ends: if not, an end isn't in the graph
 *   yet. `differs` is whether another review at its place states something
 *   else: no approval says which, so the engine refuses both. `unsettled` are
 *   the ends whose names the load linked on less than a name, in the order
 *   the engine refuses them, which a person settles before the claim is
 *   written where they hold it (holdingEnds). `conflict` is a claim held as
 *   conflicting with what its link holds: approving it decides that, on a
 *   link that holds one value only (contract 4.14.0), and on any other link
 *   nothing settles it;
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
      /** statementHash of `link` and `stored`: what an approval of it binds. */
      hash: string;
      named: boolean;
      differs?: boolean;
      // Never empty: a claim with no such end has none.
      unsettled?: Unsettled[];
      conflict?: true;
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
    // A conflict a load wrote before claims kept statements was never one a
    // person settles, and on a link that holds many values none is.
    if (reason === "conflict" && !stored) return { candidates };
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
        hash: statementHash(link, stored),
        named: ends(r.claim) >= 2,
        ...(reason === "conflict" ? { conflict: true as const } : {}),
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
 * Synchronous, as a page reads its reviews: a saved decision names a claim
 * by it as well as by its key (ofClaim).
 *
 * @example
 * statementHash("partner_of", stored); // 64 lowercase hexadecimal digits
 */
export function statementHash(link: string, stored: string): string {
  return bytesToHex(sha256(new TextEncoder().encode(`${link}\u001f${stored}`)));
}

/** A review as markDiffering reads it: what settles it, its reason and its place. */
type Placed = Settleable & {
  reason?: string;
  section?: number;
  start?: number;
};

/**
 * Marks the held claims of `reviews` whose approval the engine would refuse or
 * hold: those that share a place and a key with a review of any reason that
 * states something else, as no approval says which (`differs`), and those
 * naming an entity by a name the load linked on less than a name in that
 * document (`unsettled`), found among `reviews` and `names` (the reviews of
 * such links a page read apart: an entity's page lists only the reviews that
 * concern its entity, and such a review concerns only the row it linked).
 * Ends go in the order the engine refuses them: a link by part of a name or
 * by an own-name identifier before one by legal form, subject before object.
 *
 * @example
 * markDiffering([one, sameKeyAndPlaceOtherStatement]); // both settle with `differs: true`
 * markDiffering([heldClaimOfWestmere], [partNameReviewOfWestmere])[0].settle.unsettled;
 * // [{ name: "Westmere", by: "part_name", at: { section: 0, start: 12 } }]
 */
export function markDiffering<T extends Placed>(
  reviews: T[],
  names: readonly Placed[] = [],
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
  const linked = new Map(
    [...reviews, ...names].flatMap((r) =>
      isWeakLink(r.reason) && r.settle?.kind === "resolve"
        ? [
            [
              end(r.settle.docId, r.settle.type, r.settle.normName),
              {
                by: r.reason,
                ...(r.section !== undefined && r.start !== undefined
                  ? { at: { section: r.section, start: r.start } }
                  : {}),
              },
            ] as const,
          ]
        : [],
    ),
  );
  return reviews.map((r) => {
    const s = r.settle;
    if (s?.kind !== "release") return r;
    const differs = (stated.get(r.claim?.place ?? "")?.size ?? 0) > 1;
    // Each name once: a claim of an entity with itself names it twice.
    const unsettled = [
      ...new Map(
        [s.statement.subject, s.statement.object].flatMap(({ type, name }) => {
          const key = end(s.docId, type, normalizeName(name)),
            link = linked.get(key);
          return link ? [[key, { name, ...link }] as const] : [];
        }),
      ).values(),
    ].sort(
      // Stable: subject before object within each.
      (a, b) => Number(a.by === "legal_form") - Number(b.by === "legal_form"),
    );
    return differs || unsettled.length
      ? {
          ...r,
          settle: {
            ...s,
            ...(differs ? { differs } : {}),
            ...(unsettled.length ? { unsettled } : {}),
          },
        }
      : r;
  });
}

/**
 * Needs review's groups in order: held claims to approve (those a verifier
 * doubted, then those that conflict with what their link holds), names linked on
 * less than a name (by legal form, by part of a name, by an own-name
 * identifier), values in conflict, then links the ontology doesn't have. Any
 * other reason follows, in the order it first appears.
 */
const GROUP_ORDER: readonly string[] = [
  "low_confidence",
  "unverified",
  "conflict",
  ...WEAK_LINKS,
  "prop_conflict",
  "unmatched",
];

/**
 * Reviews whose cards say the same of their reason: its title and
 * explanation, and each review with its index in the list it came from.
 * `reason` is its first review's.
 */
export type ReviewGroup<R> = {
  reason: string;
  title: string;
  text: string;
  items: { review: R; index: number }[];
};

/**
 * Groups reviews by what their card says of their reason (documents' reviewCopy),
 * so an ambiguous name with one candidate is apart from one with several, and
 * every reason this site doesn't know is one group. Groups go in GROUP_ORDER,
 * then as they first appear; a group keeps its reviews' order.
 *
 * @example
 * reviewGroups([
 *   { reason: "unresolved", candidates: [] },
 *   { reason: "unmatched", candidates: [] },
 *   { reason: "low_confidence", candidates: [] },
 * ]).map((g) => [g.reason, g.items.map((i) => i.index)]);
 * // [["low_confidence", [2]], ["unmatched", [1]], ["unresolved", [0]]]
 */
export function reviewGroups<
  R extends { reason: string; candidates: readonly unknown[] },
>(reviews: readonly R[]): ReviewGroup<R>[] {
  const groups = new Map<string, ReviewGroup<R>>();
  reviews.forEach((review, index) => {
    const copy = reviewCopy(review.reason, review.candidates.length);
    const group = groups.get(copy.title) ?? {
      reason: review.reason,
      ...copy,
      items: [],
    };
    // Appended in place: a copy per review would be quadratic in a long document.
    group.items.push({ review, index });
    groups.set(copy.title, group);
  });
  const rank = ({ reason }: ReviewGroup<R>) =>
    GROUP_ORDER.includes(reason)
      ? GROUP_ORDER.indexOf(reason)
      : GROUP_ORDER.length;
  // Array sort is stable: groups of the same rank keep their first appearance.
  return [...groups.values()].sort((a, b) => rank(a) - rank(b));
}

/**
 * Whether a saved decision names this held claim's review: a release or a
 * reject at its document and place, under its key or, as a rebuild may key
 * the same sentence otherwise, of the statement it reads (`hash`). From
 * contract 4.14.0 the engine binds a release whose key names no review of its
 * document so, and the backend keeps a claim's approval and rejection apart
 * the same way.
 */
const ofClaim = (d: ReviewDecision, s: Extract<Settle, { kind: "release" }>) =>
  (d.kind === "release" || d.kind === "reject") &&
  d.docId === s.docId &&
  d.claimSection === s.section &&
  d.claimStart === s.start &&
  d.claimEnd === s.end &&
  (d.claimKey === s.claimKey || d.statementHash === s.hash);

/**
 * `reviews` without the held claims a person rejected: each review of a held
 * claim that a saved reject names (ofClaim: by its document and its quote's
 * place, and its claim's key or the statement it reads). A rejection changes
 * no load, so the review is still in the graph: leaving it out here is what
 * hides its card, drops it from a page's counts and keeps Approve all from
 * it, in this load and every later one, until the decision is removed. By its
 * key it hides the claim however it reads now: a person said that claim at
 * that place is wrong. By what it reads it hides that statement there under
 * the key a rebuild gave it.
 *
 * @example
 * unrejected([held, other], [{ kind: "reject", ...placeOf(held) }]); // [other]
 */
export function unrejected<R extends Settleable>(
  reviews: readonly R[],
  decisions: readonly ReviewDecision[],
): R[] {
  const rejects = decisions.filter((d) => d.kind === "reject");
  if (!rejects.length) return [...reviews];
  return reviews.filter(({ settle: s }) => {
    if (s?.kind !== "release") return true;
    return !rejects.some((d) => ofClaim(d, s));
  });
}

/**
 * The held claims of `reviews` that Approve all approves: each settled by a
 * release a person could approve on its card now, with no decision saved for
 * it (ofClaim: not a rejection, nor an approval, of what it reads under any
 * key or of what it said before: that card asks to be read again), not read
 * two ways (`differs`), and not naming a name the load linked on less than a
 * name where the engine holds the claim for it (holdingEnds): the engine
 * would refuse it until that name is settled, and only a later load can
 * write it. A stale review settles by no release, so it's never one of them.
 *
 * None of a link that holds one value (`single`): from contract 4.14.0 an
 * approval there decides what the link holds, and replaces what it holds of
 * no date or dated up to when the approval takes effect, whoever put it
 * there: a person's approval, a table's row or another document. A page
 * can't tell what such a link holds or why, so a person approves each of
 * those on its card, where it says so.
 *
 * @example
 * approvable([held, readTwoWays, alreadyApproved], decisions, () => false); // [held.settle]
 */
export function approvable(
  reviews: readonly Settleable[],
  decisions: readonly ReviewDecision[],
  single: (link: string) => boolean,
): Extract<Settle, { kind: "release" }>[] {
  return reviews.flatMap(({ settle: s }) =>
    s?.kind === "release" &&
    !single(s.link) &&
    !s.conflict &&
    !s.differs &&
    !holdingEnds(s.unsettled, false).length &&
    !decisions.some((d) => ofClaim(d, s))
      ? [s]
      : [],
  );
}

/**
 * How many of `reviews` Approve all leaves to their cards for their link
 * alone: those it would approve if every link held many values whose link
 * holds one (`single`).
 *
 * @example
 * leftToCards([heldOfOneValuedLink, heldOfManyValuedLink], [], (l) => l === "signed_with"); // 1
 */
export function leftToCards(
  reviews: readonly Settleable[],
  decisions: readonly ReviewDecision[],
  single: (link: string) => boolean,
): number {
  return approvable(reviews, decisions, () => false).filter((s) =>
    single(s.link),
  ).length;
}

/**
 * What Approve all says before it approves, leaving `left` claims to their
 * cards (leftToCards).
 *
 * @example
 * approveAllText(2); // "This load writes each one it can, at once. … 2 claims of a link that holds one value aren’t included: …"
 */
export function approveAllText(left: number) {
  return `This load writes each one it can, at once. Any it can't stays held, and its card says why.${
    left
      ? ` ${left === 1 ? "1 claim" : `${left} claims`} of a link that holds one value ${left > 1 ? "aren’t" : "isn’t"} included: an approval decides what such a link holds, so approve each on its card.`
      : ""
  }`;
}

const LINKS_GOAL = "Add links the documents state that the ontology lacks: ";
/**
 * The most a goal takes as a URL query value. Generate opens with it in its
 * address, and a host refuses a long address (Vercel's limit is 14 KB): a
 * goal of non-Latin text can take nine bytes a character once encoded.
 */
const MAX_GOAL_URL = 8000;

/**
 * The goal Add these links fills Generate with, from reviews of relationships
 * the ontology doesn't have: each link they suggest once (spellings differing
 * only in case, spaces, underscores or hyphens are one link, named as first
 * spelled), the most suggested first, then up to two quotes each, every
 * link's first before any second. It stays within MAX_GOAL characters (code
 * points, as Generate counts them) and MAX_GOAL_URL encoded: links that don't
 * fit are left out, and then quotes that don't. None when no review names a
 * link.
 *
 * @example
 * linksGoal([{ suggestedLink: "supplier_of", quote: "Globex supplies Initech." }]);
 * // "Add links the documents state that the ontology lacks: supplier_of (“Globex supplies Initech.”)."
 */
export function linksGoal(
  reviews: readonly { suggestedLink?: string; quote: string }[],
): string | undefined {
  const links = new Map<
    string,
    { name: string; quotes: string[]; count: number }
  >();
  for (const r of reviews) {
    const name = r.suggestedLink?.trim();
    if (!name) continue;
    const key = name.toLowerCase().replace(/[\s_-]+/g, "_"),
      quote = r.quote.replace(/\s+/g, " ").trim(),
      link = links.get(key) ?? { name, quotes: [], count: 0 };
    link.count += 1;
    if (quote && link.quotes.length < 2 && !link.quotes.includes(quote))
      link.quotes.push(quote);
    links.set(key, link);
  }
  type Part = { name: string; quotes: string[] };
  const goal = (parts: readonly Part[]) =>
    `${LINKS_GOAL}${parts
      .map(({ name, quotes }) =>
        quotes.length
          ? `${name} (${quotes.map((q) => `“${q}”`).join(", ")})`
          : name,
      )
      .join(", ")}.`;
  const fits = (parts: readonly Part[]) => {
    const text = goal(parts);
    return (
      [...text].length <= MAX_GOAL &&
      encodeURIComponent(text).length <= MAX_GOAL_URL
    );
  };
  // Stable: links suggested as often keep their first appearance.
  const ranked = [...links.values()].sort((a, b) => b.count - a.count);
  // Every link that fits is named first; quotes take the room left.
  const named = ranked.reduce<(Part & { from: string[] })[]>(
    (parts, { name, quotes }) => {
      const next = [...parts, { name, quotes: [], from: quotes }];
      return fits(next) ? next : parts;
    },
    [],
  );
  const quoted = [0, 1]
    .flatMap((k) => named.map((_, i) => [k, i] as const))
    .reduce((parts, [k, i]) => {
      const quote = parts[i]!.from[k];
      if (!quote) return parts;
      const next = parts.map((p, j) =>
        j === i ? { ...p, quotes: [...p.quotes, quote] } : p,
      );
      return fits(next) ? next : parts;
    }, named);
  return quoted.length ? goal(quoted) : undefined;
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
 * How a person's saved approvals stand toward a held claim's card. `saved`
 * is the approval of this review: by its key and of what it reads; on the
 * load a person can settle, an approval of what it reads at its place under
 * another key, which the claim held before (a rebuild moves keys): from
 * contract 4.14.0 the engine matches a release whose key it holds no review
 * under by its place and statement hash, so that approval is this claim's
 * and says what became of it, and approving the card approves it again
 * (the backend keeps its time); else the approval under its key of what it
 * said before. A sibling claim of the sentence states something else, and
 * isn't. On that load `moved` is that the approval of this review approved
 * what the claim said before: the engine refuses it. An older load's
 * statement may not be the current one, so it says neither.
 *
 * @example
 * approvalOf([], release, true); // { saved: undefined, moved: false }
 */
export function approvalOf<D extends ReviewDecision>(
  decisions: readonly D[],
  s: Extract<Settle, { kind: "release" }>,
  editable: boolean,
): { saved?: D; moved: boolean } {
  const read = (d: D) =>
    d.kind === "release" &&
    d.docId === s.docId &&
    d.claimSection === s.section &&
    d.claimStart === s.start &&
    d.claimEnd === s.end &&
    d.statementHash === s.hash;
  const saved =
    decisions.find(
      (d) => d.kind === "release" && d.claimKey === s.claimKey && read(d),
    ) ??
    (editable ? decisions.find(read) : undefined) ??
    decisions.find((d) => matchesReview(d, s));
  return {
    saved,
    moved:
      editable && saved?.kind === "release" && saved.statementHash !== s.hash,
  };
}

/**
 * The approvals and chosen values a job has yet to apply to the current
 * load: while there are any, the page reads the decisions again.
 *
 * @example
 * waiting([{ applied: { status: "pending" } }, { applied: { status: "written" } }]).length; // 1
 */
export function waiting<D extends Pick<FoundryReviewDecision, "applied">>(
  decisions: readonly D[],
): D[] {
  return decisions.filter((d) => d.applied?.status === "pending");
}

/**
 * How a name is linked on less than a name, as a card's sentence says it.
 *
 * @example
 * WEAK_LINK_TEXT.part_name; // "linked only by part of a row’s name"
 */
export const WEAK_LINK_TEXT: Record<WeakLink, string> = {
  legal_form: "linked only by its legal form",
  part_name: "linked only by part of a row’s name",
  own_name: "linked only by a code the document calls it by",
};

/**
 * Why an approved claim stays held, for its card, from the reason the load's
 * job gave a refusal (FoundryDecisionApplied.reason): what a person can do
 * about it where there is something. `ends` are the claim's names that hold
 * it, as far as the card knows them (holdingEnds, and those linked the way
 * the refusal says): it names those linked the way it says, as the engine
 * doesn't say which end it refused for, and no name where none was; any
 * other is a name to settle too before a load writes it. A reason this site
 * doesn't know reads as it came.
 *
 * @example
 * refusalText("unresolved"); // "A name in it isn’t matched to one record in your data. …"
 * refusalText("part_name", [{ name: "Westmere", by: "part_name" }]); // "“Westmere” is linked only by part of a row’s name, …"
 * refusalText("part_name", [{ name: "Globex Ltd", by: "legal_form" }]); // "A name in it is linked only by part of a row’s name, … Settle that name and “Globex Ltd”, which holds it too, …"
 * refusalText("a_new_reason"); // "The load kept it held (a_new_reason)."
 */
export function refusalText(
  reason: string | undefined,
  ends: readonly Unsettled[] = [],
) {
  const quoted = (by: (u: Unsettled) => boolean) =>
    ends.filter(by).map((u) => `“${u.name}”`);
  const names = quoted((u) => u.by === reason),
    others = quoted((u) => u.by !== reason);
  const named = names.length
      ? `${names.join(" and ")} ${names.length > 1 ? "are each" : "is"}`
      : "A name in it is",
    too = others.length
      ? ` and ${others.join(" and ")}, which ${others.length > 1 ? "hold" : "holds"} it too`
      : "",
    settle = `Settle ${names.length > 1 ? "those names" : "that name"}${too}, and the next load writes this.`;
  switch (reason) {
    case "unresolved":
      return "A name in it isn’t matched to one record in your data. Settle that name and load again; if it’s still held then, approve it there.";
    case "legal_form":
      return `${named} ${WEAK_LINK_TEXT.legal_form}, which doesn’t write a link that holds one value. ${settle}`;
    case "part_name":
    case "own_name":
      // With no review of the link in this load (an engine before these
      // links made it, or the row came after the document), it lists the
      // name as one it couldn't match.
      return `${named} ${WEAK_LINK_TEXT[reason]}, and an approval doesn’t stand on such a link. ${settle}${
        names.length
          ? ""
          : " Where this load has no review of such a link, it lists that name as not in your data, or as one that could mean more than one entity: settle it there."
      }`;
    case "changed":
      return "It reads differently now from what was approved. Read it again and approve what it says.";
    case "differs":
      return "Its sentence is read more than one way, and no approval can say which.";
    // From contract 4.14.0 an approval of a link that holds one value
    // decides what the link holds, so this is all that's left of it, besides
    // an engine before 4.14.0's word: each new load sends every approval
    // again, and the card can't tell which engine refused it, so it offers
    // no approval again, which would change nothing in the first two cases.
    case "conflict":
      return "It conflicts with what the graph holds for this link: a property of it differs from the one the graph holds, which no approval or new load changes; or it gives a date still to come, and a load sent it after that date writes it; or a load built before statements were kept wrote the link, or a version before approvals could decide a link refused it, and then the next load writes it.";
    case "blocked":
      return "Writing it would break a rule the ontology enforces, or checking that took more reading than a load allows.";
    case "no_statement":
      return "Its review keeps nothing to write. Load again to read it afresh.";
    case "not_stated":
      return "The document’s saved reading no longer states it at this place. Load again to read it afresh.";
    case "too_large":
      return "Its document holds more held claims than one approval can be matched among.";
    case "not_replayed":
      return "This document’s saved reading couldn’t be reused, as the models or the reading changed since, so writing it now would cost a model call. Load again to apply it.";
    case "document_gone":
      return "Its document was deleted, or is no longer part of this load.";
    case "document_unread":
      return "This load holds its document, but has no finished reading of it to write from. Load again to apply it.";
    case "document_failed":
      return "The load couldn’t read its document again. Load again to apply it.";
    default:
      return `The load kept it held${reason ? ` (${reason})` : ""}.`;
  }
}

/**
 * What a card says while an approval, or a chosen value (`kind` "prop"),
 * waits for its job. One job at a time writes an ontology's database, so
 * with another job live (`live`, the kind of the ontology's active job) the
 * decision waits for that job's end rather than being applied now; behind a
 * new load it is applied to whichever load is current when that one ends.
 *
 * @example
 * waitingText(); // "Approved. Writing it to this load…"
 * waitingText("extract"); // "Approved. Another job is running, so this waits: …"
 * waitingText(null, "prop"); // "Decision saved. Sending it to this load…"
 */
export function waitingText(
  live?: string | null,
  kind: "release" | "prop" = "release",
) {
  const value = kind === "prop";
  switch (live || "resend") {
    case "resend":
      return value
        ? "Decision saved. Sending it to this load…"
        : "Approved. Writing it to this load…";
    case "load":
      return value
        ? "Decision saved. A load is running, so this waits: it’s applied when that ends, to the load that is current then."
        : "Approved. A load is running, so this waits: it’s written when that ends, to the load that is current then.";
    default:
      return value
        ? "Decision saved. Another job is running, so this waits: it’s applied to this load when that ends."
        : "Approved. Another job is running, so this waits: it’s written to this load when that ends.";
  }
}

/**
 * Whether the value an entity stores for a property (`stored`) is the one a
 * decision chose: the load holds the decision, so the stored value is no
 * table's to keep, and its card offers another choice rather than to replace
 * a table's value. An integer past 2^53 compares as the number it rounds to.
 *
 * @example
 * holdsDecided({ integer: "36" }, 36); // true
 * holdsDecided({ text: "GBP" }, "usd"); // false
 */
export function holdsDecided(
  chosen: FoundryPropValue,
  stored: string | number | boolean | undefined,
): boolean {
  if (stored === undefined || chosen.null !== undefined) return false;
  if (chosen.text !== undefined) return chosen.text === stored;
  if (chosen.boolean !== undefined) return chosen.boolean === stored;
  return Number(chosen.integer ?? chosen.number) === stored;
}

/**
 * What a property review's card says of the value saved for it (`value`, as
 * the page shows one), from what the current load made of the decision
 * (`applied`; unset when that load's list doesn't hold it) and whether the
 * entity holds that value now (`holds`: holdsDecided, or no value stored for
 * a decision of none). From contract 4.14.0
 * every load lists the values its documents dispute, decided or not, so the
 * review stays listed and the decision is what settles it. The load says
 * only that it was sent the value: a document dated after the decision
 * still wins, which is why a load may hold another.
 *
 * @example
 * decidedText({ status: "applied" }, "36", null, true); // "Decided: 36. This load holds it. …"
 * decidedText(undefined, "36"); // "A decision is saved (36), and this load hasn’t read it. …"
 */
export function decidedText(
  applied: DecisionApplied | null | undefined,
  value: string,
  live?: string | null,
  holds = false,
) {
  const shown = value || "no value";
  const settled =
    "The documents’ values stay listed here, as in every load: this decision settles them.";
  switch (applied?.status) {
    case "pending":
      return waitingText(live, "prop");
    case "applied":
    case "":
      return holds
        ? `Decided: ${shown}. This load holds it. ${settled}`
        : applied.status === "applied"
          ? `Decided: ${shown}. This load was sent the decision, and holds that value unless a document dated after the decision states another. ${settled}`
          : `Decided: ${shown}. This load read the decision and holds another value: a document dated after the decision states it. Choose again or remove the decision.`;
    case "unconfirmed":
      return `Decided: ${shown}, but the job that sends it to this load failed. Choose it again to send it.`;
    default:
      return `A decision is saved (${shown}), and this load hasn’t read it. Choose it again to apply it now.`;
  }
}

/** How often the decisions are read while an approval has just begun to wait. */
export const APPLY_POLL_MS = 2000;

/**
 * How long until the decisions are read again while an approval waits, by how
 * long the page has seen one wait (`waitedMs`). An approval is written within
 * seconds, so the first half minute reads every two seconds; one that waits
 * longer is behind another job, which may run for hours, so reads slow to
 * every ten seconds, then to once a minute after five minutes. Each read
 * lists every decision of the ontology, so the slow rate is what an open
 * page costs while a long job runs.
 *
 * @example
 * applyPollMs(0); // 2000
 * applyPollMs(60_000); // 10000
 * applyPollMs(600_000); // 60000
 */
export function applyPollMs(waitedMs: number) {
  return waitedMs < 30_000
    ? APPLY_POLL_MS
    : waitedMs < 5 * 60_000
      ? 10_000
      : 60_000;
}

/** What became of a release decision, as far as a page's reads say. */
export type ReleaseStatus = "written" | "no_link" | "held" | "unknown";

/**
 * What became of `d` on a document's page, from its reviews and its facts:
 * its review is still held (at its place, under its key or stating what it
 * approved there, as a rebuild may key it otherwise), or its quote stands
 * with an edge, or without one, as a later statement holds that link.
 * Otherwise the reads don't say.
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
      s?.kind === "release" &&
      here(s) &&
      (s.claimKey === d.claimKey || s.hash === d.statementHash),
  );
  if (held) return "held";
  const fact = facts.find((f) => !f.unread && f.quotes.some(here));
  if (!fact) return "unknown";
  return fact.from ? "written" : "no_link";
}

/**
 * Whether a load read a resolve decision. A load reads the decisions saved
 * when it starts (the backend snapshots them), and replacing one saves it
 * anew, so one saved before the load started is in it, and one saved after
 * waits for the next load. A load whose start isn't known read none. (A
 * release or a chosen value says what its load made of it:
 * FoundryReviewDecision.applied.)
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
