import {
  forceCollide,
  forceLink,
  forceManyBody,
  forceSimulation,
  forceX,
  forceY,
  type Force,
  type Simulation,
  type SimulationLinkDatum,
  type SimulationNodeDatum,
} from "d3-force";
import { useLayoutEffect, useRef, useState } from "react";

import type { GraphData, GraphEdge, GraphNode } from "../lib/types";
import { assignEdgeRoutes } from "./edgeGeometry";
import { placeNewNodes, type XY } from "./placement";

/** The simulation's copy of a node. GraphNode objects are never mutated. */
export interface SimNode extends SimulationNodeDatum {
  id: string;
  label: string;
  /** Index of the node's label, for the clustering force. */
  group: number;
  /** Edges to other nodes (self-loops don't count). */
  degree: number;
  /**
   * Where the node sat before the last change to the graph, while the layout
   * folds that change in. Null for nodes free to move (and when cold).
   */
  anchorX: number | null;
  anchorY: number | null;
}

interface SimLink extends SimulationLinkDatum<SimNode> {
  id: string;
}

/** An edge the canvas draws: both endpoints are present. */
export interface LayoutEdge {
  id: string;
  label: string;
  /** Indices into `ForceLayout.nodes`. */
  source: number;
  target: number;
  bend: number;
  loop: number;
}

export interface SyncResult {
  added: number;
  removed: number;
  /** True when this sync took the canvas from empty to non-empty. */
  firstNodes: boolean;
  /** Edges dropped because an endpoint isn't in the graph. */
  danglingEdges: number;
}

const ALPHA_MIN = 0.008;
const ALPHA_DECAY = 0.028;
/** Reheat after nodes arrive: enough to fold them in without shaking the rest. */
const REHEAT_ADDED = 0.3;
const REHEAT_CHANGED = 0.12;
const DRAG_ALPHA_TARGET = 0.25;
/** Spring that holds untouched nodes in place while a change settles. */
const ANCHOR_STRENGTH = 1;
/** Positions remembered for nodes that left, so hiding and showing a label puts them back. */
const REMEMBERED_LIMIT = 20_000;

/**
 * Pulls each label's nodes weakly toward that label's current centroid. The
 * centroids move with the layout, so nothing is dragged toward a fixed spot
 * far from the graph.
 */
function forceLabelClusters(strength: number): Force<SimNode, SimLink> {
  let nodes: SimNode[] = [];
  let sumX = new Float64Array(0);
  let sumY = new Float64Array(0);
  let count = new Float64Array(0);
  let groups = 0;

  const force = (alpha: number) => {
    if (groups < 2) return;
    sumX.fill(0);
    sumY.fill(0);
    count.fill(0);
    for (const n of nodes) {
      sumX[n.group] += n.x ?? 0;
      sumY[n.group] += n.y ?? 0;
      count[n.group]++;
    }
    const k = strength * alpha;
    for (const n of nodes) {
      const c = count[n.group];
      if (!(c >= 2)) continue;
      n.vx = (n.vx ?? 0) + (sumX[n.group] / c - (n.x ?? 0)) * k;
      n.vy = (n.vy ?? 0) + (sumY[n.group] / c - (n.y ?? 0)) * k;
    }
  };
  force.initialize = (next: SimNode[]) => {
    nodes = next;
    groups = 0;
    for (const n of next) groups = Math.max(groups, n.group + 1);
    sumX = new Float64Array(groups);
    sumY = new Float64Array(groups);
    count = new Float64Array(groups);
    const distinct = new Set(next.map((n) => n.group));
    if (distinct.size < 2) groups = 0;
  };
  return force;
}

/**
 * Holds anchored nodes near their anchor. After an expansion only the nodes
 * the change touched move freely; everything else stays where the user last
 * saw it instead of drifting with the reheat.
 */
function forceAnchors(strength: number): Force<SimNode, SimLink> {
  let nodes: SimNode[] = [];
  const force = (alpha: number) => {
    const k = strength * alpha;
    for (const n of nodes) {
      if (n.anchorX === null || n.anchorY === null) continue;
      n.vx = (n.vx ?? 0) + (n.anchorX - (n.x ?? 0)) * k;
      n.vy = (n.vy ?? 0) + (n.anchorY - (n.y ?? 0)) * k;
    }
  };
  force.initialize = (next: SimNode[]) => {
    nodes = next;
  };
  return force;
}

/**
 * Owns the d3-force simulation and every node position, keyed by node id, for
 * the canvas's lifetime. `sync` folds a new graph in: nodes that stay keep
 * their position and velocity, new ones start next to a placed neighbor, and
 * the layout is reheated gently. The simulation never runs its own timer; the
 * canvas calls `tick` from its frame loop, so the loop can stop when the
 * layout is cold.
 */
export class ForceLayout {
  /** Live simulation nodes, aligned with `graphNodes`. */
  nodes: SimNode[] = [];
  graphNodes: GraphNode[] = [];
  edges: LayoutEdge[] = [];
  graphEdges: GraphEdge[] = [];
  indexById = new Map<string, number>();
  edgeIndexById = new Map<string, number>();
  /** Bumps whenever a position may have changed. */
  positionsVersion = 0;
  /** Bumps when the node or edge set changes. */
  structureVersion = 0;
  /** Bumps on every sync: node objects may carry new labels or properties under the same ids. */
  dataVersion = 0;
  ticks = 0;

  private readonly sim: Simulation<SimNode, SimLink>;
  private readonly links = forceLink<SimNode, SimLink>([]).id((d) => d.id);
  private readonly charge = forceManyBody<SimNode>()
    .strength((d) => (-32 - 4 * Math.sqrt(d.degree)) * this.spread)
    .distanceMax(600);
  private byId = new Map<string, SimNode>();
  private groupOf = new Map<string, number>();
  private remembered = new Map<string, XY>();
  private pinned: ReadonlySet<string> = new Set();
  private dragging: string | null = null;
  /** Spacing multiplier: 1.6 for a handful of nodes, easing to 1 past a few hundred. */
  private spread = 1;
  private readonly random: () => number;

  constructor(random: () => number = Math.random) {
    this.random = random;
    // Longer springs around hubs give their neighbors room; small graphs get
    // more air overall.
    this.links.distance((l) => {
      const s = l.source as SimNode;
      const t = l.target as SimNode;
      return (26 + 3 * Math.sqrt(Math.max(s.degree, t.degree))) * this.spread;
    });
    this.sim = forceSimulation<SimNode, SimLink>([])
      .alphaMin(ALPHA_MIN)
      .alphaDecay(ALPHA_DECAY)
      .velocityDecay(0.42)
      .force("link", this.links)
      .force("charge", this.charge)
      .force("collide", forceCollide<SimNode>(8).strength(0.7))
      .force("x", forceX<SimNode>(0).strength(0.04))
      .force("y", forceY<SimNode>(0).strength(0.04))
      .force("cluster", forceLabelClusters(0.08))
      .force("anchor", forceAnchors(ANCHOR_STRENGTH))
      .stop();
    this.sim.alpha(0);
  }

  /**
   * Folds `graph` into the layout. Duplicate ids keep their first occurrence;
   * edges with a missing endpoint are skipped. `center` is where nodes with
   * nothing to attach to appear (the viewport center, in world units).
   */
  sync(
    graph: GraphData,
    opts: { center: XY; pinned?: ReadonlySet<string> },
  ): SyncResult {
    if (opts.pinned) this.pinned = opts.pinned;
    const prevCount = this.nodes.length;
    const prevEdgeIds = this.edgeIndexById;

    const nodes: SimNode[] = [];
    const graphNodes: GraphNode[] = [];
    const indexById = new Map<string, number>();
    const fresh: SimNode[] = [];
    let relabeled = false;
    for (const node of graph.nodes) {
      if (indexById.has(node.id)) continue;
      let sim = this.byId.get(node.id);
      if (!sim) {
        sim = {
          id: node.id,
          label: node.label,
          group: 0,
          degree: 0,
          anchorX: null,
          anchorY: null,
        };
        fresh.push(sim);
      } else if (sim.label !== node.label) {
        relabeled = true;
      }
      sim.label = node.label;
      sim.group = this.groupFor(node.label);
      sim.degree = 0;
      indexById.set(node.id, nodes.length);
      nodes.push(sim);
      graphNodes.push(node);
    }

    let removed = 0;
    for (const [id, sim] of this.byId) {
      if (indexById.has(id)) continue;
      removed++;
      if (sim.x !== undefined && sim.y !== undefined)
        this.remember(id, { x: sim.x, y: sim.y });
    }

    const edges: LayoutEdge[] = [];
    const graphEdges: GraphEdge[] = [];
    const edgeIndexById = new Map<string, number>();
    const links: SimLink[] = [];
    const linkedPairs = new Set<string>();
    let danglingEdges = 0;
    for (const edge of graph.edges) {
      if (edgeIndexById.has(edge.id)) continue;
      const s = indexById.get(edge.source);
      const t = indexById.get(edge.target);
      if (s === undefined || t === undefined) {
        danglingEdges++;
        continue;
      }
      edgeIndexById.set(edge.id, edges.length);
      edges.push({
        id: edge.id,
        label: edge.label,
        source: s,
        target: t,
        bend: 0,
        loop: -1,
      });
      graphEdges.push(edge);
      if (s === t) continue;
      nodes[s].degree++;
      nodes[t].degree++;
      // One spring per pair: parallel edges shouldn't pull harder.
      const pair = s < t ? `${s}:${t}` : `${t}:${s}`;
      if (linkedPairs.has(pair)) continue;
      linkedPairs.add(pair);
      links.push({ id: edge.id, source: edge.source, target: edge.target });
    }
    const routes = assignEdgeRoutes(graphEdges);
    for (let i = 0; i < edges.length; i++) {
      edges[i].bend = routes[i].bend;
      edges[i].loop = routes[i].loop;
    }

    if (fresh.length > 0) this.placeFresh(fresh, nodes, edges, opts.center);
    for (const sim of fresh) {
      if (this.pinned.has(sim.id)) {
        sim.fx = sim.x;
        sim.fy = sim.y;
      }
    }

    let edgesChanged = edges.length !== prevEdgeIds.size;
    if (!edgesChanged) {
      for (const id of edgeIndexById.keys()) {
        if (!prevEdgeIds.has(id)) {
          edgesChanged = true;
          break;
        }
      }
    }
    const structureChanged = fresh.length > 0 || removed > 0 || edgesChanged;
    if (structureChanged && prevCount > 0)
      this.anchorUntouched(nodes, edges, graphEdges, fresh);

    this.byId = new Map(nodes.map((n) => [n.id, n]));
    this.nodes = nodes;
    this.graphNodes = graphNodes;
    this.edges = edges;
    this.graphEdges = graphEdges;
    this.indexById = indexById;
    this.edgeIndexById = edgeIndexById;
    this.dataVersion++;
    // The dragged node left the graph: the drag is over, and so is the warmth
    // it was holding the layout at.
    if (this.dragging && !indexById.has(this.dragging)) this.stopDragging();

    // A relabeled node moves to another cluster group: the forces must see it.
    if (structureChanged || relabeled) {
      this.spread = 1 + 0.6 * Math.exp(-nodes.length / 50);
      // A coarser Barnes–Hut approximation on big graphs: about 25% cheaper
      // ticks at 2,000 nodes, and the difference doesn't show.
      this.charge.theta(nodes.length > 1000 ? 1.2 : 0.9);
      // Clear the links first: re-initializing them against the new node list
      // must not see springs to nodes that just left.
      this.links.links([]);
      this.sim.nodes(nodes);
      this.links.links(links);
      this.positionsVersion++;
    }
    if (structureChanged) {
      this.structureVersion++;
      if (prevCount === 0) this.sim.alpha(1);
      else this.reheat(fresh.length > 0 ? REHEAT_ADDED : REHEAT_CHANGED);
    }
    if (nodes.length === 0) this.sim.alpha(0);

    return {
      added: fresh.length,
      removed,
      firstNodes: prevCount === 0 && nodes.length > 0,
      danglingEdges,
    };
  }

  /**
   * Anchors every surviving node the change didn't touch. Touched: new nodes,
   * and the endpoints of edges that appeared between existing nodes or went
   * away. A node that only gained new neighbors (the one being expanded)
   * stays put and its new neighbors fan out around it.
   */
  private anchorUntouched(
    nodes: SimNode[],
    edges: LayoutEdge[],
    graphEdges: GraphEdge[],
    fresh: SimNode[],
  ) {
    const freshIds = new Set(fresh.map((n) => n.id));
    const touched = new Set<string>(freshIds);
    const prevEdges = this.edgeIndexById;
    const nextEdges = new Set<string>();
    for (let i = 0; i < edges.length; i++) {
      const id = graphEdges[i].id;
      nextEdges.add(id);
      if (prevEdges.has(id)) continue;
      const s = nodes[edges[i].source];
      const t = nodes[edges[i].target];
      if (freshIds.has(s.id) || freshIds.has(t.id)) continue;
      touched.add(s.id);
      touched.add(t.id);
    }
    for (const [id, i] of prevEdges) {
      if (nextEdges.has(id)) continue;
      const e = this.edges[i];
      touched.add(this.nodes[e.source].id);
      touched.add(this.nodes[e.target].id);
    }
    for (const n of nodes) {
      if (touched.has(n.id) || n.x === undefined || n.y === undefined) {
        n.anchorX = null;
        n.anchorY = null;
      } else {
        n.anchorX = n.x;
        n.anchorY = n.y;
      }
    }
  }

  /** Lets every node move freely again. */
  private releaseAnchors() {
    for (const n of this.nodes) {
      n.anchorX = null;
      n.anchorY = null;
    }
  }

  /** Reheats the whole layout with nothing held in place (the canvas's "re-layout"). */
  relayout(alpha = 0.8): void {
    this.releaseAnchors();
    this.reheat(alpha);
  }

  /** Applies the orchestrator's pinned set: pins fix nodes where they are, unpinned nodes are released. */
  syncPins(pinned: ReadonlySet<string>): void {
    this.pinned = pinned;
    let released = false;
    for (const n of this.nodes) {
      const want = pinned.has(n.id) || n.id === this.dragging;
      if (want && (n.fx == null || n.fy == null)) {
        n.fx = n.x;
        n.fy = n.y;
      } else if (!want && (n.fx != null || n.fy != null)) {
        n.fx = null;
        n.fy = null;
        released = true;
      }
    }
    if (released) this.reheat(0.08);
  }

  setPinned(id: string, pinned: boolean): void {
    const n = this.byId.get(id);
    if (!n) return;
    if (pinned) {
      n.fx = n.x;
      n.fy = n.y;
    } else if (id !== this.dragging) {
      n.fx = null;
      n.fy = null;
      this.reheat(0.08);
    }
  }

  isFixed(id: string): boolean {
    const n = this.byId.get(id);
    return !!n && n.fx != null && n.fy != null;
  }

  startDrag(id: string): void {
    const n = this.byId.get(id);
    if (!n) return;
    this.dragging = id;
    // A drag should pull its neighbors along, not fight anchors.
    this.releaseAnchors();
    n.fx = n.x;
    n.fy = n.y;
    this.sim.alphaTarget(DRAG_ALPHA_TARGET);
    this.reheat(0.1);
  }

  dragTo(id: string, x: number, y: number): void {
    const n = this.byId.get(id);
    if (!n || this.dragging !== id) return;
    n.fx = x;
    n.fy = y;
    n.x = x;
    n.y = y;
    this.positionsVersion++;
  }

  /** Ends a drag. The node stays where it was dropped when `keepFixed` or it is pinned. */
  endDrag(id: string, keepFixed: boolean): void {
    if (this.dragging !== id) {
      // A drag that already ended (its node left the graph) must not keep the
      // layout warm; one still running on another node is left alone.
      if (this.dragging === null) this.sim.alphaTarget(0);
      return;
    }
    this.stopDragging();
    const n = this.byId.get(id);
    if (n && !keepFixed && !this.pinned.has(id)) {
      n.fx = null;
      n.fy = null;
    }
  }

  /** Forgets the drag and lets the layout cool again. */
  private stopDragging(): void {
    this.dragging = null;
    this.sim.alphaTarget(0);
  }

  get draggingId(): string | null {
    return this.dragging;
  }

  /** True while the layout still moves and needs frames. */
  isActive(): boolean {
    return (
      this.nodes.length > 0 &&
      (this.sim.alpha() >= this.sim.alphaMin() || this.dragging !== null)
    );
  }

  alpha(): number {
    return this.sim.alpha();
  }

  /** Raises the temperature to at least `alpha`; never cools a hotter layout. */
  reheat(alpha: number): void {
    if (this.nodes.length === 0) return;
    this.sim.alpha(Math.max(this.sim.alpha(), alpha));
  }

  tick(): void {
    if (this.nodes.length === 0) return;
    this.sim.tick();
    this.ticks++;
    this.positionsVersion++;
    if (this.sim.alpha() < this.sim.alphaMin()) this.releaseAnchors();
  }

  /** Ticks synchronously until cold, `maxTicks`, or `budgetMs`. Returns the ticks run. */
  warmUp(maxTicks: number, budgetMs: number): number {
    const start = now();
    let n = 0;
    while (
      n < maxTicks &&
      this.sim.alpha() >= this.sim.alphaMin() &&
      now() - start < budgetMs
    ) {
      this.tick();
      n++;
    }
    return n;
  }

  private groupFor(label: string): number {
    let g = this.groupOf.get(label);
    if (g === undefined) {
      g = this.groupOf.size;
      this.groupOf.set(label, g);
    }
    return g;
  }

  private remember(id: string, p: XY) {
    this.remembered.delete(id);
    this.remembered.set(id, p);
    if (this.remembered.size > REMEMBERED_LIMIT) {
      const oldest = this.remembered.keys().next().value;
      if (oldest !== undefined) this.remembered.delete(oldest);
    }
  }

  private placeFresh(
    fresh: SimNode[],
    nodes: SimNode[],
    edges: LayoutEdge[],
    center: XY,
  ) {
    const placed = new Map<string, XY>();
    const freshIds = new Set(fresh.map((n) => n.id));
    for (const n of nodes) {
      if (!freshIds.has(n.id) && n.x !== undefined && n.y !== undefined)
        placed.set(n.id, { x: n.x, y: n.y });
    }
    // Nodes coming back keep their old spot.
    const toPlace: string[] = [];
    for (const n of fresh) {
      const p = this.remembered.get(n.id);
      if (p) {
        this.remembered.delete(n.id);
        n.x = p.x;
        n.y = p.y;
        placed.set(n.id, p);
      } else {
        toPlace.push(n.id);
      }
    }
    if (toPlace.length === 0) return;

    const neighbors = new Map<string, string[]>();
    const link = (a: string, b: string) => {
      const list = neighbors.get(a);
      if (list) list.push(b);
      else neighbors.set(a, [b]);
    };
    for (const e of edges) {
      if (e.source === e.target) continue;
      const a = nodes[e.source].id;
      const b = nodes[e.target].id;
      link(a, b);
      link(b, a);
    }
    const labels = new Map(nodes.map((n) => [n.id, n.label]));
    const positions = placeNewNodes({
      ids: toPlace,
      placed,
      neighbors,
      labelOf: (id) => labels.get(id) ?? "",
      center,
      random: this.random,
    });
    for (const n of fresh) {
      const p = positions.get(n.id);
      if (!p) continue;
      n.x = p.x;
      n.y = p.y;
      n.vx = 0;
      n.vy = 0;
    }
  }
}

function now(): number {
  return typeof performance !== "undefined" ? performance.now() : Date.now();
}

/**
 * A {@link ForceLayout} that lives as long as the component and follows the
 * `graph` prop. Syncs run in a layout effect, so the frame after a graph
 * change already draws it.
 */
export function useGraphSimulation(
  graph: GraphData,
  options: {
    pinned: ReadonlySet<string>;
    /** World point that nodes with nothing to attach to gather around: the viewport center. */
    spawnCenter: () => XY;
    onSync?: (result: SyncResult) => void;
  },
): ForceLayout {
  const [layout] = useState(() => new ForceLayout());
  const optionsRef = useRef(options);
  useLayoutEffect(() => {
    optionsRef.current = options;
  });

  const { nodes, edges } = graph;
  useLayoutEffect(() => {
    const { pinned, spawnCenter, onSync } = optionsRef.current;
    const result = layout.sync(
      { nodes, edges },
      { center: spawnCenter(), pinned },
    );
    onSync?.(result);
  }, [layout, nodes, edges]);

  const { pinned } = options;
  useLayoutEffect(() => {
    layout.syncPins(pinned);
  }, [layout, pinned]);

  return layout;
}
