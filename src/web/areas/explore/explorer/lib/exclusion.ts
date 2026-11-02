import type { ElementTarget, GraphData, LabelExclusion } from "./types";

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
