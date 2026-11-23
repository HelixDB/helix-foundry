import type { FoundryReviewDecision } from "../../../shared/wfe";
import { isRecord } from "../../../shared/json";

// Reviews settled outside the walk that the walk takes as the person's own:
// Founder's settle_review cards, run from the dock beside the walk
// (agent/cards.tsx announces each that ran; the walk listens while open).

/** A review settled from Founder's dock: which, and the decision saved. */
export type Settled = {
  workspaceId: string;
  ontologyId: string;
  reviewId: string;
  decision: FoundryReviewDecision;
};

const listeners = new Set<(s: Settled) => void>();

/** Tells every listener (an open walk) that a review was settled. */
export const announceSettled = (s: Settled) => {
  for (const l of listeners) l(s);
};

/** Listens for reviews settled elsewhere until the returned function is called. */
export const onSettled = (l: (s: Settled) => void) => {
  listeners.add(l);
  return () => void listeners.delete(l);
};

const camel = (k: string) =>
  k.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase());

/**
 * The decision a settle_review action's result holds (the MCP's snake_case
 * decision: `{result: {id, kind, decided_by_user_id, …}}`), as the WFE
 * writes one; undefined for anything else. Read without trust: only an id
 * and a known kind make one.
 *
 * @example
 * settledDecision('{"result":{"id":"d1","kind":"release","doc_id":"doc"}}'); // { id: "d1", kind: "release", docId: "doc" }
 */
export function settledDecision(
  resultJson: string | undefined,
): FoundryReviewDecision | undefined {
  let out: unknown;
  try {
    out = JSON.parse(resultJson || "");
  } catch {
    return undefined;
  }
  const r = isRecord(out) && isRecord(out.result) ? out.result : undefined;
  if (
    !r ||
    typeof r.id !== "string" ||
    !r.id ||
    !["resolve", "prop", "release", "reject"].includes(String(r.kind))
  )
    return undefined;
  return Object.fromEntries(
    Object.entries(r).map(([k, v]) => [camel(k), v]),
  ) as FoundryReviewDecision;
}
