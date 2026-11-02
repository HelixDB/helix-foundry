// Getting into production: the two steps from a workspace trying Foundry to
// its own app or agent querying an ontology, which the Analyst's card shows
// (./production.tsx). This browser remembers them per workspace.

export const PRODUCTION_STEPS = [
  { id: "ontology", title: "Create an ontology" },
  { id: "query", title: "Query it through the Helix MCP" },
] as const;

export type ProductionStepId = (typeof PRODUCTION_STEPS)[number]["id"];

/** What this browser remembers of a workspace's steps. */
export type ProductionState = {
  // The prompt was copied: the query step is done.
  copied: boolean;
  // The card was minimised.
  minimised: boolean;
};

export const productionKey = (workspace: string) =>
  `foundry.production.v1:${workspace}`;

/**
 * What localStorage holds, as state: anything but `true` in a field, or
 * anything but JSON, reads as not set.
 *
 * @example
 * parseProductionState('{"copied":true}').copied; // true
 * parseProductionState("not json").copied; // false
 */
export function parseProductionState(raw: string): ProductionState {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    // Nothing kept.
  }
  const kept = (value ?? {}) as Partial<Record<keyof ProductionState, unknown>>;
  return {
    copied: kept.copied === true,
    minimised: kept.minimised === true,
  };
}

/**
 * Whether the workspace has an ontology other than the sample, `sample` being
 * useSampleOntology's: undefined until that can be told, while the list or
 * the sample is being read.
 *
 * @example
 * hasOwnOntology([{ id: "o1" }], { id: "o1" }); // false
 * hasOwnOntology([{ id: "o1" }], null); // true
 * hasOwnOntology([{ id: "o1" }], undefined); // undefined
 */
export const hasOwnOntology = (
  ontologies: readonly { id: string }[] | undefined,
  sample: { id: string } | null | undefined,
): boolean | undefined =>
  ontologies && sample !== undefined
    ? ontologies.some((o) => o.id !== sample?.id)
    : undefined;

export type ProductionStepState = "done" | "current" | "upcoming";

export type ProductionProgress = {
  steps: readonly {
    id: ProductionStepId;
    title: string;
    state: ProductionStepState;
  }[];
  done: number;
  // The first step not done; null once both are.
  current: ProductionStepId | null;
};

/**
 * Where the steps are, in order: the query waits for an ontology, so a prompt
 * copied with none yet doesn't count.
 */
export function productionProgress(
  ownOntology: boolean,
  copied: boolean,
): ProductionProgress {
  const done: Record<ProductionStepId, boolean> = {
    ontology: ownOntology,
    query: ownOntology && copied,
  };
  const current = PRODUCTION_STEPS.find((s) => !done[s.id])?.id ?? null;
  return {
    steps: PRODUCTION_STEPS.map((s) => ({
      ...s,
      state: done[s.id] ? "done" : s.id === current ? "current" : "upcoming",
    })),
    done: PRODUCTION_STEPS.filter((s) => done[s.id]).length,
    current,
  };
}

/**
 * Whether the card starts minimised: when it was left so, once both steps are
 * done, and where the page is too narrow for it to start open (`fits`). Opened
 * in those last two cases, it is for that visit.
 */
export const cardStartsMinimised = (
  minimised: boolean,
  bothDone: boolean,
  fits: boolean,
) => minimised || bothDone || !fits;
