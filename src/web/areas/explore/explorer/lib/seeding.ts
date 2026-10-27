import type {
  DatabaseQueryResult,
  DynamicQueryRequest,
} from "../../database-query";
import type { Scope } from "../../load-scope";
import {
  buildBoundedProbeRequest,
  buildConnectRequest,
  buildEdgeCountRequest,
  buildRingRequest,
  buildSeedCandidatesRequest,
  EXPLORER_LIMITS,
  isLargeProbe,
  parseBoundedProbe,
  parseConnect,
  parseEdgeCount,
  parseRing,
  parseSeedCandidates,
  PROBE_SLOW_MS,
  type RingSide,
  type SeedProbe,
} from "./queries";
import type {
  DegreeProbe,
  GraphData,
  GraphNode,
  NodeFetchShape,
} from "./types";

/**
 * A load's first view, planned from its version (Foundry's ontology), so it
 * opens on a connected, readable graph rather than scattered pairs:
 *
 * 1. Seeds: candidates from the hub types (the types with the most link
 *    ends), a literal limit of each type's bitmap in the load; a bounded
 *    probe of each (neighbors counted up to `degreeThreshold` + 1 over every
 *    label and over each of its type's links, no edges read); the ones with
 *    the most neighbors over their links that aren't large, taken in turn
 *    from each hub type.
 * 2. A ring around the seeds: each seed's links in both directions, a
 *    literal limit per link so its neighbors spread over them.
 * 3. A second ring around the first one's nodes, those the probe found
 *    small, while the budget lasts.
 * 4. The edges among what loaded, read from the small nodes, at most
 *    `connectEdgeBudget` edges.
 * Before a node is read around or connected, its edges are counted
 * (bounded, and only once the probe found it small), as the explorer plans
 * its own connects: the neighbor count is only a floor on the edge count,
 * and both reads cost time in the node's edges.
 * Each step lands on the canvas as it comes; "Show more" runs the next ring
 * within another budget. Every request is bounded by literal limits and
 * probes, and a large node is never read around on its own.
 */

/** A load's version, as the first view needs it. */
export interface VersionLayout {
  /** Its types (node labels). */
  types: readonly string[];
  /** Its links (edge labels) and the types they join. */
  links: readonly { name: string; from: string; to: string }[];
}

export const SEEDING = Object.freeze({
  /** Hub types seeds come from. */
  hubTypes: 3,
  /** Candidates read across the hub types, to choose seeds among. */
  candidates: 48,
  /** Seeds kept. */
  seeds: 20,
  /** Neighbors per seed in the first ring. */
  perNode: 8,
  /** Neighbors per node in the rings after it. */
  outerPerNode: 3,
  /** Nodes in the first view. */
  budget: 320,
  /** Nodes one "Show more" adds at most. */
  moreBudget: 200,
  /** Nodes one ring request reads around. */
  batch: 100,
  /**
   * Nodes one probe request checks the size of. Each costs time in its
   * adjacency size (about a second for a hub with millions of neighbors),
   * so a batch stays well within Helix's 30 s; twice the explorer's own
   * `probeBatch`, since the seeds wait on these probes at 1 request a
   * second, and a slow batch stops the rest.
   */
  probeBatch: 20,
});

/**
 * Types by how many link ends they hold, most first, then by name; types no
 * link touches come last.
 *
 * @example
 * hubTypes({ types: ["A", "B", "C"], links: [{ name: "r", from: "A", to: "B" }, { name: "s", from: "B", to: "C" }] });
 * // ["B", "A", "C"]
 */
export function hubTypes(layout: VersionLayout): string[] {
  const ends = new Map<string, number>(layout.types.map((t) => [t, 0]));
  for (const link of layout.links)
    for (const end of [link.from, link.to])
      if (ends.has(end)) ends.set(end, ends.get(end)! + 1);
  return [...ends.entries()]
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([type]) => type);
}

/**
 * How many candidates to read from each hub type: the candidate budget split
 * over the first `hubTypes` types that have links (or the first types when
 * none has), at least a few each.
 */
export function candidateShares(
  layout: VersionLayout,
): { type: string; take: number }[] {
  const ordered = hubTypes(layout);
  const linked = ordered.filter((t) =>
    layout.links.some((l) => l.from === t || l.to === t),
  );
  const hubs = (linked.length ? linked : ordered).slice(0, SEEDING.hubTypes);
  if (!hubs.length) return [];
  const each = Math.max(4, Math.floor(SEEDING.candidates / hubs.length));
  return hubs.map((type) => ({ type, take: each }));
}

/** Neighbors over every label: what counting the node's edges costs. */
const degree = (p: DegreeProbe | undefined) => (p ? p.out + p.in : 0);
/** Edges over every label, once counted: what reading them costs. */
const edgeCount = (p: DegreeProbe | undefined) =>
  p?.edges ? p.edges.out + p.edges.in : null;
/** Neighbors over its type's links: what a ring around it shows. */
const linked = (p: SeedProbe | undefined) => p?.linked ?? 0;

/**
 * Seeds: candidates the probe found small, the most neighbors over their
 * links first, taken in turn from each type so every hub type shows; a
 * candidate without such neighbors comes after every connected one, and
 * large or unprobed ones are left out.
 */
export function chooseSeeds(
  candidates: readonly GraphNode[],
  probes: ReadonlyMap<number, SeedProbe>,
  count: number = SEEDING.seeds,
): GraphNode[] {
  const usable = candidates.filter((n) => {
    const p = probes.get(n.dbId);
    return p !== undefined && !isLargeProbe(p);
  });
  const byType = new Map<string, GraphNode[]>();
  for (const n of usable)
    byType.set(n.label, [...(byType.get(n.label) ?? []), n]);
  for (const list of byType.values())
    list.sort(
      (a, b) =>
        linked(probes.get(b.dbId)) - linked(probes.get(a.dbId)) ||
        a.dbId - b.dbId,
    );
  const pick = (connected: boolean) => {
    const lists = [...byType.values()].map((list) =>
      list.filter((n) => linked(probes.get(n.dbId)) > 0 === connected),
    );
    const out: GraphNode[] = [];
    for (let i = 0; lists.some((l) => i < l.length); i++)
      for (const list of lists) if (i < list.length) out.push(list[i]!);
    return out;
  };
  return [...pick(true), ...pick(false)].slice(0, count);
}

/**
 * The links (and directions) to read around a node of `type`, `perNode`
 * neighbors spread over them, at least one each.
 */
export function ringSides(
  layout: VersionLayout,
  type: string,
  perNode: number,
): RingSide[] {
  const sides = layout.links.flatMap((l) => [
    ...(l.from === type ? [{ label: l.name, side: "out" as const }] : []),
    ...(l.to === type ? [{ label: l.name, side: "in" as const }] : []),
  ]);
  if (!sides.length) return [];
  const take = Math.max(1, Math.ceil(perNode / sides.length));
  return sides.map((s) => ({ ...s, take }));
}

/**
 * The next ring: nodes the probe found small, whose edges are counted and
 * not large, and not read around yet, each with its sides, while `budget`
 * new nodes last (a node may add as many as its sides take).
 */
export function planRing(
  frontier: readonly GraphNode[],
  probes: ReadonlyMap<number, SeedProbe>,
  expanded: ReadonlySet<number>,
  layout: VersionLayout,
  perNode: number,
  budget: number,
): { dbId: number; sides: RingSide[] }[] {
  const ring: { dbId: number; sides: RingSide[] }[] = [];
  let left = budget;
  for (const node of frontier) {
    if (left <= 0) break;
    const p = probes.get(node.dbId);
    if (
      expanded.has(node.dbId) ||
      !p ||
      edgeCount(p) === null ||
      isLargeProbe(p) ||
      linked(p) === 0
    )
      continue;
    const sides = ringSides(layout, node.label, perNode);
    const adds = Math.min(
      linked(p),
      sides.reduce((n, s) => n + s.take, 0),
    );
    if (!sides.length || adds > left) continue;
    ring.push({ dbId: node.dbId, sides });
    left -= adds;
  }
  return ring;
}

/**
 * Nodes whose edges to the rest are read, within the connect budget: small
 * ones whose edges are counted, each costing its edge count (connecting
 * reads every edge's endpoints). The neighbor count alone is only a floor:
 * parallel edges can give a node with few neighbors many edges.
 */
export function connectable(
  nodes: readonly GraphNode[],
  probes: ReadonlyMap<number, DegreeProbe>,
  budget: number = EXPLORER_LIMITS.connectEdgeBudget,
): number[] {
  const ids: number[] = [];
  let left = budget;
  for (const node of nodes) {
    const p = probes.get(node.dbId);
    const cost = edgeCount(p);
    if (!p || cost === null || isLargeProbe(p) || cost === 0 || cost > left)
      continue;
    ids.push(node.dbId);
    left -= cost;
  }
  return ids;
}

/**
 * The nodes to count the edges of, in requests: probed small ones not
 * counted yet, at most `edgeCountBatch` a request and `edgeCountBudget`
 * neighbors summed (one pair lookup each), as the explorer counts them. A
 * node with no neighbors needs no request: `empty` lists them.
 */
export function edgeCountPlan(
  nodes: readonly GraphNode[],
  probes: ReadonlyMap<number, DegreeProbe>,
): { batches: number[][]; empty: number[] } {
  const ids = [
    ...new Set(
      nodes
        .map((n) => n.dbId)
        .filter((id) => {
          const p = probes.get(id);
          return p && !p.edges && !isLargeProbe(p);
        }),
    ),
  ];
  const out: number[][] = [];
  let batch: number[] = [],
    lookups = 0;
  for (const id of ids.filter((id) => degree(probes.get(id)) > 0)) {
    const cost = degree(probes.get(id));
    if (
      batch.length === EXPLORER_LIMITS.edgeCountBatch ||
      (batch.length > 0 && lookups + cost > EXPLORER_LIMITS.edgeCountBudget)
    ) {
      out.push(batch);
      batch = [];
      lookups = 0;
    }
    batch.push(id);
    lookups += cost;
  }
  if (batch.length) out.push(batch);
  return {
    batches: out,
    empty: ids.filter((id) => degree(probes.get(id)) === 0),
  };
}

/** Splits a list into batches of `size`. */
export function batches<T>(
  list: readonly T[],
  size: number = SEEDING.batch,
): T[][] {
  const out: T[][] = [];
  for (let i = 0; i < list.length; i += size) out.push(list.slice(i, i + size));
  return out;
}

// ---------------------------------------------------------------- Running it

/** What the seeding has done so far: sizes probed, nodes read around, the outermost ring. */
export type SeedingState = {
  probes: Map<number, SeedProbe>;
  expanded: Set<number>;
  frontier: GraphNode[];
};

export const newSeedingState = (): SeedingState => ({
  probes: new Map(),
  expanded: new Set(),
  frontier: [],
});

type Failure = Extract<DatabaseQueryResult, { ok: false }>;

/** How the seeding reads and what it does with what it read: the explorer's side of it. */
export interface SeedingIO {
  /** Runs a read; null when its answer no longer matters (cancelled, or a newer boot). */
  read(
    title: string,
    request: DynamicQueryRequest,
  ): Promise<DatabaseQueryResult | null>;
  /** Puts the seeds on the canvas: the first view shows. */
  place(seeds: GraphNode[]): Promise<void>;
  /** Adds nodes and edges to the canvas; returns the nodes that were new. */
  merge(graph: Partial<GraphData>): GraphNode[];
  /** Sizes probed, for the rest of the explorer. */
  probed(probes: ReadonlyMap<number, DegreeProbe>): void;
  /** Database ids of the nodes on the canvas. */
  onCanvas(): number[];
  /** A read past the first view failed: the rest goes on without it. */
  failed(title: string, failure: Failure): void;
}

/**
 * Probes the nodes not probed yet, `SEEDING.probeBatch` per read; false
 * when stale. A batch that runs slow likely met hubs, so the rest stay
 * unknown, and unknown nodes are never read around or connected. A large
 * node alone doesn't stop it, as it does the explorer's own probing: the
 * seeds' candidates and a ring mix types, so the nodes after one aren't
 * likely more of the same.
 */
export async function probeAll(
  io: SeedingIO,
  state: SeedingState,
  layout: VersionLayout,
  nodes: readonly GraphNode[],
): Promise<boolean> {
  const seen = new Set<number>();
  const pending = nodes.filter((n) => {
    if (state.probes.has(n.dbId) || seen.has(n.dbId)) return false;
    seen.add(n.dbId);
    return true;
  });
  for (const group of batches(pending, SEEDING.probeBatch)) {
    const batch = group.map((n) => ({
      dbId: n.dbId,
      sides: ringSides(layout, n.label, 1),
    }));
    const result = await io.read(
      `Check the size of ${batch.length} ${batch.length === 1 ? "node" : "nodes"}`,
      buildBoundedProbeRequest(batch),
    );
    if (!result) return false;
    // A failed probe leaves those sizes unknown: they are not read around.
    if (result.ok) {
      const found = parseBoundedProbe(result.body, batch);
      for (const [id, probe] of found) state.probes.set(id, probe);
      io.probed(found);
    }
    if (result.elapsedMs > PROBE_SLOW_MS) break;
  }
  return true;
}

/**
 * Counts the edges of the probed small nodes (edgeCountPlan); false when
 * stale. A failed count leaves the rest uncounted: they are not read around
 * or connected.
 */
export async function countAll(
  io: SeedingIO,
  state: SeedingState,
  nodes: readonly GraphNode[],
): Promise<boolean> {
  const plan = edgeCountPlan(nodes, state.probes);
  const counted = new Map<number, SeedProbe>();
  const store = (id: number, edges: { out: number; in: number }) => {
    const p = state.probes.get(id);
    if (!p) return;
    const probe = { ...p, edges };
    state.probes.set(id, probe);
    counted.set(id, probe);
  };
  for (const id of plan.empty) store(id, { out: 0, in: 0 });
  for (const dbIds of plan.batches) {
    const result = await io.read(
      `Count the edges of ${dbIds.length} ${dbIds.length === 1 ? "node" : "nodes"}`,
      buildEdgeCountRequest({ dbIds }),
    );
    if (!result) return false;
    if (!result.ok) break;
    for (const [id, edges] of parseEdgeCount(result.body, dbIds))
      store(id, edges);
  }
  if (counted.size) io.probed(counted);
  return true;
}

/** Reads one ring around `frontier` (planRing), a batch per read; its new nodes, or null when stale. */
export async function readRing(
  io: SeedingIO,
  state: SeedingState,
  layout: VersionLayout,
  frontier: readonly GraphNode[],
  perNode: number,
  budget: number,
  shape: NodeFetchShape,
): Promise<GraphNode[] | null> {
  const plan = planRing(
    frontier,
    state.probes,
    state.expanded,
    layout,
    perNode,
    budget,
  );
  const added: GraphNode[] = [];
  for (const batch of batches(plan)) {
    const title = `Load the neighbors of ${batch.length} ${batch.length === 1 ? "node" : "nodes"}`;
    const result = await io.read(
      title,
      buildRingRequest({ nodes: batch, shape }),
    );
    if (!result) return null;
    if (!result.ok) {
      io.failed(title, result);
      break;
    }
    for (const node of batch) state.expanded.add(node.dbId);
    added.push(...io.merge(parseRing(result.body, shape)));
  }
  return added;
}

/** Reads the edges among the canvas's nodes from the connectable ones; false when stale. */
export async function connectAll(
  io: SeedingIO,
  state: SeedingState,
  nodes: readonly GraphNode[],
  shape: NodeFetchShape,
): Promise<boolean> {
  const ids = connectable(nodes, state.probes);
  if (!ids.length) return true;
  const title = `Connect ${ids.length} ${ids.length === 1 ? "node" : "nodes"} to the rest`;
  const result = await io.read(
    title,
    buildConnectRequest({ newDbIds: ids, visibleDbIds: io.onCanvas(), shape }),
  );
  if (!result) return false;
  if (!result.ok) io.failed(title, result);
  else io.merge({ edges: parseConnect(result.body) });
  return true;
}

export type SeedOutcome =
  | { kind: "seeded" }
  // No hub type holds anything in the load: the explorer samples instead.
  | { kind: "none" }
  | { kind: "stale" }
  // The first read failed: nothing is on the canvas.
  | { kind: "failed"; failure: Failure };

/**
 * A load's first view, step by step (see the top of this file): seeds on
 * the canvas first, then the rings, then the edges among them. `holding`
 * names the types the load holds (counted above zero).
 */
export async function seedView(
  io: SeedingIO,
  state: SeedingState,
  opts: {
    scope: Scope;
    layout: VersionLayout;
    holding: ReadonlySet<string>;
    shape: NodeFetchShape;
  },
): Promise<SeedOutcome> {
  const { layout, shape } = opts;
  const shares = candidateShares(layout).filter((share) =>
    opts.holding.has(share.type),
  );
  if (!shares.length) return { kind: "none" };
  const picked = await io.read(
    "Pick seeds from the hub types",
    buildSeedCandidatesRequest({ scope: opts.scope, hubs: shares, shape }),
  );
  if (!picked) return { kind: "stale" };
  if (!picked.ok) return { kind: "failed", failure: picked };
  const candidates = parseSeedCandidates(picked.body, shape);
  if (!candidates.length) return { kind: "none" };
  if (!(await probeAll(io, state, layout, candidates)))
    return { kind: "stale" };
  const chosen = chooseSeeds(candidates, state.probes);
  // Nothing small to start from: the candidates alone, read around only when asked.
  const seeds = chosen.length ? chosen : candidates.slice(0, SEEDING.seeds);
  await io.place(seeds);
  if (!(await countAll(io, state, seeds))) return { kind: "stale" };
  let budget = SEEDING.budget - seeds.length;
  const first = await readRing(
    io,
    state,
    layout,
    seeds,
    SEEDING.perNode,
    budget,
    shape,
  );
  if (!first) return { kind: "stale" };
  budget -= first.length;
  if (
    !(await probeAll(io, state, layout, first)) ||
    !(await countAll(io, state, first))
  )
    return { kind: "stale" };
  const second = await readRing(
    io,
    state,
    layout,
    first,
    SEEDING.outerPerNode,
    budget,
    shape,
  );
  if (!second) return { kind: "stale" };
  if (!(await connectAll(io, state, [...seeds, ...first], shape)))
    return { kind: "stale" };
  state.frontier = second.length ? second : first;
  return { kind: "seeded" };
}

/**
 * "Show more": the next ring around the outermost one, within `budget`
 * nodes, then the edges among them; its new nodes (the next frontier), or
 * null when stale.
 */
export async function moreRing(
  io: SeedingIO,
  state: SeedingState,
  layout: VersionLayout,
  shape: NodeFetchShape,
  budget: number,
): Promise<GraphNode[] | null> {
  const present = new Set(io.onCanvas());
  const frontier = state.frontier.filter((n) => present.has(n.dbId));
  if (
    !(await probeAll(io, state, layout, frontier)) ||
    !(await countAll(io, state, frontier))
  )
    return null;
  const ring = await readRing(
    io,
    state,
    layout,
    frontier,
    SEEDING.outerPerNode,
    budget,
    shape,
  );
  if (!ring) return null;
  if (
    !(await probeAll(io, state, layout, ring)) ||
    !(await countAll(io, state, ring))
  )
    return null;
  if (!(await connectAll(io, state, [...frontier, ...ring], shape)))
    return null;
  state.frontier = ring;
  return ring;
}
