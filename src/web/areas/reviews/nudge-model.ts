import type { FoundryReviewSummary } from "../../../shared/wfe";

// When the sidebar's review nudge shows (./nudge.tsx). Pure.

/**
 * What Later hid, by ontology: the load it was pressed on and how many
 * cards were open then.
 */
export type NudgeState = Readonly<
  Record<string, { loadId: string; open: number }>
>;

/** How much open must grow past what Later saw for the nudge to come back. */
export const NUDGE_GROWTH = 1.25;

/**
 * The stored state (localStorage `foundry.review.nudge.v1:<ws>`), anything
 * that doesn't read as one being none.
 *
 * @example
 * parseNudgeState('{"o1":{"loadId":"L","open":40}}'); // { o1: { loadId: "L", open: 40 } }
 * parseNudgeState("oops"); // {}
 */
export function parseNudgeState(raw: string): NudgeState {
  try {
    const v: unknown = JSON.parse(raw);
    if (typeof v !== "object" || v === null || Array.isArray(v)) return {};
    return Object.fromEntries(
      Object.entries(v).flatMap(([o, x]) => {
        const { loadId, open } = (x ?? {}) as Record<string, unknown>;
        return typeof loadId === "string" &&
          typeof open === "number" &&
          Number.isFinite(open)
          ? [[o, { loadId, open }]]
          : [];
      }),
    );
  } catch {
    return {};
  }
}

/**
 * The ontology the nudge points at: of the current loads with cards to
 * decide (or still being counted), the one with the most, but those Later
 * hid on the same load until open grows by NUDGE_GROWTH. None when every
 * one is hidden or has nothing to decide.
 *
 * @example
 * nudgeShown({}, [{ ontologyId: "o1", loadId: "L", open: 12 }]); // the o1 summary
 * nudgeShown({ o1: { loadId: "L", open: 12 } }, [{ ontologyId: "o1", loadId: "L", open: 14 }]); // undefined
 */
export function nudgeShown(
  state: NudgeState,
  summaries: readonly FoundryReviewSummary[],
): FoundryReviewSummary | undefined {
  return summaries
    .filter((s) => {
      const open = s.open ?? 0;
      if (!open && !s.indexing) return false;
      const later = state[s.ontologyId];
      return (
        !later ||
        later.loadId !== s.loadId ||
        open >= Math.max(1, later.open * NUDGE_GROWTH)
      );
    })
    .toSorted(
      (a, b) =>
        (b.open ?? 0) - (a.open ?? 0) ||
        a.ontologyId.localeCompare(b.ontologyId),
    )[0];
}

/** The state once Later is pressed on `s`. */
export const nudgeLater = (
  state: NudgeState,
  s: FoundryReviewSummary,
): NudgeState => ({
  ...state,
  [s.ontologyId]: { loadId: s.loadId, open: s.open ?? 0 },
});
