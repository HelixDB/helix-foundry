import { createHash } from "node:crypto";
import { normalizeName } from "../src/web/areas/explore/naming";
import { cardId } from "../src/web/areas/reviews/cards";
import type { ReviewDecision } from "../src/shared/wfe";

// The review cards index's rules, as the backend's reviewcards package has
// them (backend/reviewcards: build.go Build, draw.go Next, QuoteTarget,
// QuoteOrder and Rechecked): what each document's reviews make, one card a
// decision, and how a load's spot checks are drawn. The mock builds a load's
// cards from its graph on each call (./routes/review-cards.ts) rather than
// keeping an index, so these rules are the whole of it.
// backend/reviewcards/testdata/cards.json and names.json run against both
// (tests/unit/review-cards.test.ts), so the twins can't drift.

/** A FoundryReview as the graph holds it, nothing of which is trusted (reviewcards.Review). */
export type Review = {
  review_key?: string;
  reason?: string;
  type?: string;
  doc_id?: string;
  norm_name?: string;
  candidates?: string[];
  entity_key?: string;
  prop?: string;
  // The engine's PropQuote records, as JSON text.
  values?: string;
  suggested_link?: string;
  statement?: string;
  claim_key?: string;
  claim?: string;
  section?: number | null;
  start?: number | null;
  end?: number | null;
  confidence?: number | null;
  concerned?: { type: string; key: string; props?: Record<string, unknown> }[];
};

/** A saved release or reject of a held claim of the document (reviewcards.ClaimDecision). */
export type ClaimDecision = {
  kind: string;
  section: number;
  start: number;
  end: number;
  claim_key: string;
  statement_hash: string;
};

export type Lane = "decide" | "fyi" | "pool";
export type Kind = "resolve" | "prop" | "release";

/** One row of foundry_review_cards (reviewcards.Card). */
export type Card = {
  id: string;
  kind: Kind;
  lane: Lane;
  onCard?: string;
  docId: string;
  type?: string;
  subjectType?: string;
  objectType?: string;
  normName?: string;
  entityKey?: string;
  prop?: string;
  claimKey?: string;
  claimSection?: number;
  claimStart?: number;
  claimEnd?: number;
  statementHash?: string;
  link?: string;
  reviewKey: string;
  reviewKeys: string[];
  reviewCount: number;
  confidence?: number;
  band?: string;
  blocked: number;
  facts: number;
  perDecision: number;
  shuffle: number;
};

/**
 * Whether decision `d` decides card `c` (foundryDecidesCard), the one
 * definition the open cards and a settle that expects its card open read:
 * a held claim by a rejection at its place under its key or of the
 * statement it reads, or an approval of the statement it reads now; a name
 * or a value by a decision of its identity. Another document's link of a
 * name (an alias) decides no card.
 */
export function decidesCard(
  d: ReviewDecision,
  c: Pick<
    Card,
    | "kind"
    | "docId"
    | "type"
    | "normName"
    | "entityKey"
    | "prop"
    | "claimKey"
    | "claimSection"
    | "claimStart"
    | "claimEnd"
    | "statementHash"
  >,
): boolean {
  if (c.kind === "release")
    return (
      (d.kind === "release" || d.kind === "reject") &&
      d.docId === c.docId &&
      d.claimSection === c.claimSection &&
      d.claimStart === c.claimStart &&
      d.claimEnd === c.claimEnd &&
      (d.statementHash === c.statementHash ||
        (d.kind === "reject" && d.claimKey === c.claimKey))
    );
  return c.kind === "resolve"
    ? d.kind === "resolve" &&
        d.type === c.type &&
        d.docId === c.docId &&
        d.normName === c.normName
    : d.kind === "prop" &&
        d.type === c.type &&
        d.entityKey === c.entityKey &&
        d.prop === c.prop;
}

export const MAX_REVIEW_KEYS = 200;
/** How many undone spot checks a load keeps drawn. */
export const SPOT_CHECKS = 5;
export const BANDS = ["0.9", "0.8", "0.7", "0.6", "low", "none"] as const;

/** A kind's place among cards that rank and write alike: held claims, names, values. */
export const kindOrder = (k: Kind) =>
  k === "release" ? 0 : k === "resolve" ? 1 : 2;

/** A verifier's score's band, an index into BANDS (reviewcards.BandOf). */
export function bandOf(c: number | null | undefined): number {
  if (c === null || c === undefined || Number.isNaN(c) || c <= 0) return 5;
  if (c >= 0.9) return 0;
  if (c >= 0.8) return 1;
  if (c >= 0.7) return 2;
  if (c >= 0.6) return 3;
  return 4;
}

/** A verifier's score kept only within [0, 1] (reviewcards' score). */
export const score = (c: unknown): number | undefined =>
  typeof c === "number" && c >= 0 && c <= 1 ? c : undefined;

const hashWord = (...parts: (string | number)[]) =>
  createHash("sha256").update(parts.join("\u001f")).digest().readUInt32BE(0);

/** A hash of parts as a number in [0, 1) (reviewcards.HashUnit). */
export const hashUnit = (...parts: (string | number)[]) =>
  hashWord(...parts) / 2 ** 32;

/** What an approval binds: the SHA-256 of its link and statement as stored. */
export const statementHash = (link: string, stored: string) =>
  createHash("sha256").update(`${link}\u001f${stored}`).digest("hex");

/** A card's place among cards that rank alike (reviewcards.Shuffle). */
export const shuffle = (load: string, id: string) =>
  hashWord(load, "order", id);

const RESOLVE = new Set([
  "unresolved",
  "ambiguous",
  "collision",
  "identifier_conflict",
  "adjudicated",
  "legal_form",
  "part_name",
  "own_name",
]);
const RELEASE = new Set(["low_confidence", "unverified", "conflict"]);
const BLOCKING = new Set([
  "unresolved",
  "ambiguous",
  "collision",
  "identifier_conflict",
]);
const WEAK = new Set(["legal_form", "part_name", "own_name"]);
const MAX_UINT32 = 2 ** 32 - 1;

const fits = (s: string, chars: number) => [...s].length <= chars;
const hex64 = (s: string) => /^[0-9a-fA-F]{64}$/.test(s);
const offset = (n: number | null | undefined): n is number =>
  typeof n === "number" && n >= 0;
const placed = (r: Review) =>
  offset(r.section) && offset(r.start) && offset(r.end);
const scalar = (v: unknown) =>
  typeof v === "string" ||
  typeof v === "boolean" ||
  (typeof v === "number" && Number.isFinite(v));

/** Whether a prop_conflict's values hold a value a person can choose. */
function hasValues(raw: string | undefined): boolean {
  let quotes: unknown;
  try {
    quotes = JSON.parse(raw ?? "");
  } catch {
    return false;
  }
  return (
    Array.isArray(quotes) &&
    quotes.some((q: Record<string, unknown> | null) => {
      const s = (q?.span ?? {}) as Record<string, unknown>;
      const n = (v: unknown): v is number => typeof v === "number" && v >= 0;
      return (
        !!q &&
        scalar(q.value) &&
        typeof q.doc_id === "string" &&
        !!q.doc_id &&
        typeof q.quote === "string" &&
        n(s.section) &&
        n(s.start) &&
        n(s.end) &&
        s.end > s.start
      );
    })
  );
}

/** How many entities a held claim's stored JSON names. */
function ends(claim: string | undefined): number {
  try {
    const about = (JSON.parse(claim ?? "") as { about?: unknown })?.about;
    return Array.isArray(about) ? about.length : 0;
  } catch {
    return 0;
  }
}

type End = { type: string; name: string };
/** A held claim's stored statement's ends; none when they don't read. */
function statementOf(raw: string): { subject: End; object: End } | undefined {
  let s: unknown;
  try {
    s = JSON.parse(raw);
  } catch {
    return undefined;
  }
  const end = (v: unknown): End | undefined => {
    const e = v as Partial<End> | null | undefined;
    return typeof e?.type === "string" &&
      typeof e.name === "string" &&
      e.type &&
      e.name
      ? { type: e.type, name: e.name }
      : undefined;
  };
  const o = s as { subject?: unknown; object?: unknown } | null;
  const subject = end(o?.subject),
    object = end(o?.object);
  return subject && object ? { subject, object } : undefined;
}

/**
 * The cards of one document's reviews of load `load` (reviewcards.Build),
 * given the saved decisions of its held claims and which links hold one
 * value. `invalid` counts reviews dropped for a field no column holds.
 */
export function buildCards(
  load: string,
  doc: string,
  reviews: readonly Review[],
  decisions: readonly ClaimDecision[],
  single: (link: string) => boolean,
): { cards: Card[]; invalid: number } {
  let invalid = 0;
  const sorted = reviews.toSorted((a, b) =>
    (a.review_key ?? "") < (b.review_key ?? "")
      ? -1
      : (a.review_key ?? "") > (b.review_key ?? "")
        ? 1
        : 0,
  );
  const placeOf = (r: Review) =>
    JSON.stringify([r.claim_key, r.section, r.start, r.end]);
  const stated = new Map<string, Set<string>>();
  for (const r of sorted)
    if (r.doc_id === doc && r.claim_key && placed(r))
      stated.set(
        placeOf(r),
        (stated.get(placeOf(r)) ?? new Set()).add(r.statement ?? ""),
      );
  const nameKey = (type: string, norm: string) => JSON.stringify([type, norm]);
  const linked = new Map<string, string>();
  const names = new Map<
    string,
    {
      type: string;
      norm: string;
      reviews: Review[];
      weak: boolean;
      blocking: boolean;
    }
  >();
  const helds: {
    card: Card;
    review: Review;
    subject: End;
    object: End;
    named: boolean;
    conflict: boolean;
    unsettled: { norm: string; by: string }[];
  }[] = [];
  const values: Card[] = [];
  for (const r of sorted) {
    if (r.doc_id !== doc) continue;
    const reason = r.reason ?? "",
      key = r.review_key ?? "";
    if (RESOLVE.has(reason)) {
      if (!key || !r.type || !r.norm_name) continue;
      if (!fits(r.type, 120) || !fits(r.norm_name, 512) || !fits(key, 256)) {
        invalid += 1;
        continue;
      }
      const at = nameKey(r.type, r.norm_name);
      const n = names.get(at) ?? {
        type: r.type,
        norm: r.norm_name,
        reviews: [],
        weak: false,
        blocking: false,
      };
      n.reviews.push(r);
      n.weak ||= reason !== "adjudicated" && !BLOCKING.has(reason);
      n.blocking ||= BLOCKING.has(reason);
      names.set(at, n);
      if (WEAK.has(reason)) linked.set(at, reason);
    } else if (RELEASE.has(reason)) {
      if (reason === "conflict" && !r.statement) continue;
      const link = r.suggested_link ?? "",
        stmt = r.statement ? statementOf(r.statement) : undefined;
      if (!link || !stmt || !key || !r.claim_key || !placed(r)) continue;
      const [section, start, end] = [r.section!, r.start!, r.end!];
      if (
        !fits(key, 256) ||
        !fits(link, 255) ||
        !hex64(r.claim_key) ||
        !fits(stmt.subject.type, 120) ||
        !fits(stmt.object.type, 120) ||
        section > MAX_UINT32 ||
        end > MAX_UINT32 ||
        start >= end
      ) {
        invalid += 1;
        continue;
      }
      const confidence = score(r.confidence);
      helds.push({
        card: {
          id: cardId("release", doc, section, start, end, r.claim_key),
          kind: "release",
          lane: "decide",
          docId: doc,
          subjectType: stmt.subject.type,
          objectType: stmt.object.type,
          claimKey: r.claim_key,
          claimSection: section,
          claimStart: start,
          claimEnd: end,
          statementHash: statementHash(link, r.statement!),
          link,
          reviewKey: key,
          reviewKeys: [key],
          reviewCount: 1,
          ...(confidence !== undefined ? { confidence } : {}),
          band: BANDS[bandOf(r.confidence)],
          blocked: 0,
          facts: 1,
          perDecision: 1,
          shuffle: 0,
        },
        review: r,
        ...stmt,
        named: ends(r.claim) >= 2,
        conflict: reason === "conflict",
        unsettled: [],
      });
    } else if (reason === "prop_conflict") {
      const entity = r.concerned?.find(
        (e) => e.type === r.type && e.key === r.entity_key,
      );
      if (
        !key ||
        !r.type ||
        !r.entity_key ||
        !r.prop ||
        !entity ||
        !hasValues(r.values)
      )
        continue;
      if (
        !fits(key, 256) ||
        !fits(r.type, 120) ||
        !fits(r.entity_key, 512) ||
        !fits(r.prop, 120)
      ) {
        invalid += 1;
        continue;
      }
      values.push({
        id: cardId("prop", doc, r.type, r.entity_key, r.prop),
        kind: "prop",
        // Only a table row's value survives a conflict: the table's stands.
        lane: scalar(entity.props?.[r.prop]) ? "fyi" : "decide",
        docId: doc,
        type: r.type,
        entityKey: r.entity_key,
        prop: r.prop,
        reviewKey: key,
        reviewKeys: [key],
        reviewCount: 1,
        blocked: 0,
        facts: 0,
        perDecision: 1,
        shuffle: 0,
      });
    }
  }
  const nameIds = new Map(
    [...names].map(([at, n]) => [at, cardId("name", n.type, doc, n.norm)]),
  );
  const waiting = new Map<string, number>();
  for (const h of helds) {
    for (const end of [h.subject, h.object]) {
      const norm = normalizeName(end.name),
        by = linked.get(nameKey(end.type, norm));
      if (by && !h.unsettled.some((u) => u.norm === norm))
        h.unsettled.push({ norm, by });
    }
    const differs = (stated.get(placeOf(h.review))?.size ?? 0) > 1;
    if (differs || (h.conflict && !single(h.card.link!))) {
      h.card.lane = "pool";
      continue;
    }
    // The name the claim waits on: the first end (subject, then object)
    // whose name holds it.
    const holding = new Set(
      h.unsettled
        .filter((u) => u.by !== "legal_form" || single(h.card.link!))
        .map((u) => u.norm),
    );
    const on = [h.subject, h.object].find((end) => {
      const norm = normalizeName(end.name),
        n = names.get(nameKey(end.type, norm));
      return !!n && (n.weak ? holding.has(norm) : n.blocking && !h.named);
    });
    if (!on) continue;
    const id = nameIds.get(nameKey(on.type, normalizeName(on.name)))!;
    h.card.onCard = id;
    const c = h.card;
    if (
      !decisions.some(
        (d) =>
          d.section === c.claimSection &&
          d.start === c.claimStart &&
          d.end === c.claimEnd &&
          (d.kind === "reject"
            ? d.claim_key === c.claimKey || d.statement_hash === c.statementHash
            : d.kind === "release" && d.statement_hash === c.statementHash),
      )
    )
      waiting.set(id, (waiting.get(id) ?? 0) + 1);
  }
  const cards: Card[] = [];
  const seen = new Set<string>();
  const add = (c: Card) => {
    if (seen.has(c.id)) return;
    seen.add(c.id);
    cards.push({ ...c, shuffle: shuffle(load, c.id) });
  };
  const at = (n: number | null | undefined) => n ?? 0;
  for (const [key, n] of names) {
    const rs = n.reviews.toSorted(
      (a, b) => at(a.section) - at(b.section) || at(a.start) - at(b.start),
    );
    const settling =
      rs.find((r) => !!r.candidates?.length && BLOCKING.has(r.reason!)) ??
      rs.find((r) => !!r.candidates?.length) ??
      rs.find((r) => BLOCKING.has(r.reason!)) ??
      rs[0]!;
    const keys = [
      settling.review_key!,
      ...rs.filter((r) => r !== settling).map((r) => r.review_key!),
    ].slice(0, MAX_REVIEW_KEYS);
    const blocked = rs.filter(
      (r) => BLOCKING.has(r.reason!) && !!r.suggested_link,
    ).length;
    const id = nameIds.get(key)!,
      h = waiting.get(id) ?? 0;
    add({
      id,
      kind: "resolve",
      lane: rs.every((r) => r.reason === "adjudicated") ? "fyi" : "decide",
      docId: doc,
      type: n.type,
      normName: n.norm,
      reviewKey: settling.review_key!,
      reviewKeys: keys,
      reviewCount: rs.length,
      blocked,
      facts: blocked + h,
      perDecision: (blocked + h) / (1 + h),
      shuffle: 0,
    });
  }
  for (const h of helds) add(h.card);
  for (const c of values) add(c);
  return { cards, invalid };
}

/** What a band's spot checks are drawn from (reviewcards.Pool). */
export type Pool = { held: number; quotes: number };

/**
 * Load `load`'s draw n (reviewcards.Next): the bands in turn from
 * BANDS[n mod 6], passing over an empty one, then a held claim or a quote
 * in proportion to how many of each the band holds. None when every band
 * is empty.
 */
export function nextDraw(
  load: string,
  n: number,
  pools: readonly Pool[],
): { band: number; quote: boolean } | undefined {
  for (let i = 0; i < BANDS.length; i++) {
    const band = (n + i) % BANDS.length,
      p = pools[band]!;
    if (p.held + p.quotes <= 0) continue;
    return {
      band,
      quote:
        p.quotes > 0 &&
        hashUnit(load, "pick", n) * (p.held + p.quotes) >= p.held,
    };
  }
  return undefined;
}

/** Which of a band's quotes draw n aims at (reviewcards.QuoteTarget). */
export const quoteTarget = (
  load: string,
  band: number,
  n: number,
  quotes: number,
) =>
  Math.min(
    Math.floor(hashUnit(load, "quote", BANDS[band]!, n) * quotes),
    Math.max(quotes - 1, 0),
  );

/** A quote of a fact in a document, at its place (reviewcards.Quote). */
export type Quote = {
  section: number;
  start: number;
  end: number;
  link: string;
  factKey: string;
  confidence?: number;
};

/** The order a document's quotes are drawn in (reviewcards.QuoteOrder). */
export const quoteOrder = (load: string, doc: string, quotes: Quote[]) =>
  quotes
    .map((q) => ({
      q,
      u: hashUnit(load, doc, q.section, q.start, q.end, q.link),
    }))
    .sort((a, b) => a.u - b.u)
    .map((x) => x.q);

/** Whether a decision or label of `id` is one a second person re-checks (reviewcards.Rechecked). */
export const rechecked = (id: string) => hashWord("recheck", id) < 214748365;
