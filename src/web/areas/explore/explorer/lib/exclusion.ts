import type {
  ElementTarget,
  GraphData,
  LabelCount,
  LabelExclusion,
  LabelStats,
} from "./types";

// What the reads bring back, without the labels the explorer leaves out
// (types.ts LabelExclusion). Without an exclusion each returns its input.

/**
 * Whether the exclusion leaves a label out: it starts with its side's prefix.
 *
 * @example
 * const foundry = { nodePrefix: "Foundry", edgePrefix: "FOUNDRY_", notice: "" };
 * excludes(foundry, "node", "FoundryChunk"); // true
 * excludes(foundry, "edge", "signed_with"); // false
 * excludes(undefined, "node", "FoundryChunk"); // false
 */
export function excludes(
  exclusion: LabelExclusion | undefined,
  target: ElementTarget,
  label: string,
): boolean {
  return (
    !!exclusion &&
    label.startsWith(
      target === "node" ? exclusion.nodePrefix : exclusion.edgePrefix,
    )
  );
}

/** Label lists without the excluded labels: what the explorer counts. */
export function shownLabels(
  labels: { nodes: readonly string[]; edges: readonly string[] },
  exclusion: LabelExclusion | undefined,
): { nodes: string[]; edges: string[] } {
  return {
    nodes: labels.nodes.filter((l) => !excludes(exclusion, "node", l)),
    edges: labels.edges.filter((l) => !excludes(exclusion, "edge", l)),
  };
}

/**
 * A graph without excluded nodes and edges. An edge to an excluded node is
 * dropped too, when the node comes along; one whose node doesn't is
 * dropped when merged (the node is never in the store).
 */
export function shownGraph(
  graph: Partial<GraphData>,
  exclusion: LabelExclusion | undefined,
): Partial<GraphData> {
  if (!exclusion) return graph;
  const dropped = new Set(
    (graph.nodes ?? [])
      .filter((n) => excludes(exclusion, "node", n.label))
      .map((n) => n.id),
  );
  return {
    ...(graph.nodes && {
      nodes: graph.nodes.filter((n) => !dropped.has(n.id)),
    }),
    ...(graph.edges && {
      edges: graph.edges.filter(
        (e) =>
          !excludes(exclusion, "edge", e.label) &&
          !dropped.has(e.source) &&
          !dropped.has(e.target),
      ),
    }),
  };
}

/**
 * Label stats without the excluded labels. A total loses what they counted:
 * the sum of the other labels' counts, or a scanned total less theirs. A
 * label whose count is unknown added nothing to it, so takes nothing away.
 */
export function shownStats(
  stats: LabelStats,
  exclusion: LabelExclusion | undefined,
): LabelStats {
  if (!exclusion) return stats;
  const side = (
    target: ElementTarget,
    entries: LabelCount[],
    total: number | null,
  ) => {
    const less = entries
      .filter((e) => excludes(exclusion, target, e.label))
      .reduce((sum, e) => sum + (e.count ?? 0), 0);
    return {
      entries: entries.filter((e) => !excludes(exclusion, target, e.label)),
      total: total === null ? null : Math.max(0, total - less),
    };
  };
  const nodes = side("node", stats.nodeLabels, stats.nodeTotal);
  const edges = side("edge", stats.edgeLabels, stats.edgeTotal);
  return {
    ...stats,
    nodeLabels: nodes.entries,
    edgeLabels: edges.entries,
    nodeTotal: nodes.total,
    edgeTotal: edges.total,
  };
}
