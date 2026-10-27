import { FOUNDRY_LOAD } from "../../src/shared/foundry-labels";

// A Helix database's graph, in memory: nodes and edges with a label and
// properties, ids from one sequence per database as Helix numbers them, and
// adjacency both ways. The mock's tenants each hold one (./tenants), loads
// write the ontology layout into it through a LoadGraph (../routes/loads.ts),
// and databases:query-read runs v3 read requests over it (./query.ts).

export type Value =
  | null
  | boolean
  | number
  | string
  | Value[]
  | { [key: string]: Value };
export type Props = Record<string, Value>;
export type MockNode = { id: number; label: string; props: Props };
export type MockEdge = {
  id: number;
  label: string;
  from: number;
  to: number;
  props: Props;
};

export class MockGraph {
  readonly nodes = new Map<number, MockNode>();
  readonly edges = new Map<number, MockEdge>();
  // Edge ids by endpoint, in insertion (id) order.
  readonly outgoing = new Map<number, number[]>();
  readonly incoming = new Map<number, number[]>();
  private next = 0;

  addNode(label: string, props: Props = {}): number {
    const id = this.next++;
    this.nodes.set(id, { id, label, props: { ...props } });
    return id;
  }

  addEdge(label: string, from: number, to: number, props: Props = {}): number {
    if (!this.nodes.has(from) || !this.nodes.has(to))
      throw new Error(`edge ${label}: no node ${from} or ${to}`);
    const id = this.next++;
    this.edges.set(id, { id, label, from, to, props: { ...props } });
    for (const [map, at] of [
      [this.outgoing, from],
      [this.incoming, to],
    ] as const)
      map.set(at, [...(map.get(at) || []), id]);
    return id;
  }

  /** The first node with `label` whose `prop` is `value`. */
  find(label: string, prop: string, value: Value): MockNode | undefined {
    for (const n of this.nodes.values())
      if (n.label === label && n.props[prop] === value) return n;
    return undefined;
  }

  /** The first edge with `label` whose `prop` is `value`. */
  findEdge(label: string, prop: string, value: Value): MockEdge | undefined {
    for (const e of this.edges.values())
      if (e.label === label && e.props[prop] === value) return e;
    return undefined;
  }

  /** Drops an edge and its adjacency entries. */
  removeEdge(id: number): void {
    const e = this.edges.get(id);
    if (!e) return;
    this.edges.delete(id);
    for (const [map, at] of [
      [this.outgoing, e.from],
      [this.incoming, e.to],
    ] as const)
      map.set(
        at,
        (map.get(at) || []).filter((x) => x !== id),
      );
  }

  /** Drops a node with every edge on it, as Helix's node drop does. */
  removeNode(id: number): void {
    for (const e of [
      ...(this.outgoing.get(id) || []),
      ...(this.incoming.get(id) || []),
    ])
      this.removeEdge(e);
    this.outgoing.delete(id);
    this.incoming.delete(id);
    this.nodes.delete(id);
  }

  /** One load's view of the graph: see LoadGraph. */
  view(load: string): LoadGraph {
    return new LoadGraph(this, load);
  }

  /**
   * Deletes one load as the engine's DeleteLoad does: its edges, then its
   * nodes, each node with every edge on it (edges someone added to a load's
   * node go with it). Nodes and edges without the load's id stay.
   */
  dropLoad(load: string): { nodes: number; edges: number } {
    const mine = (props: Props) => props[FOUNDRY_LOAD] === load;
    let edges = 0;
    for (const e of [...this.edges.values()].filter((x) => mine(x.props))) {
      this.removeEdge(e.id);
      edges += 1;
    }
    const nodes = [...this.nodes.values()].filter((n) => mine(n.props));
    for (const n of nodes) {
      edges +=
        (this.outgoing.get(n.id)?.length || 0) +
        (this.incoming.get(n.id)?.length || 0);
      this.removeNode(n.id);
    }
    return { nodes: nodes.length, edges };
  }
}

/**
 * What one load writes and finds, as the engine's store bound to a load
 * does: every node and edge it adds carries `foundry_load`, and every lookup
 * only sees that load's own, so loads of different versions sit side by
 * side with equal keys.
 */
export class LoadGraph {
  constructor(
    readonly graph: MockGraph,
    readonly load: string,
  ) {}

  private own(props: Props): boolean {
    return props[FOUNDRY_LOAD] === this.load;
  }

  addNode(label: string, props: Props = {}): number {
    return this.graph.addNode(label, { ...props, [FOUNDRY_LOAD]: this.load });
  }

  addEdge(label: string, from: number, to: number, props: Props = {}): number {
    return this.graph.addEdge(label, from, to, {
      ...props,
      [FOUNDRY_LOAD]: this.load,
    });
  }

  /** A node of this load by id. */
  node(id: number): MockNode | undefined {
    const n = this.graph.nodes.get(id);
    return n && this.own(n.props) ? n : undefined;
  }

  /** This load's nodes, in id order. */
  *nodes(): Iterable<MockNode> {
    for (const n of this.graph.nodes.values()) if (this.own(n.props)) yield n;
  }

  /** The first node of this load with `label` whose `prop` is `value`. */
  find(label: string, prop: string, value: Value): MockNode | undefined {
    for (const n of this.nodes())
      if (n.label === label && n.props[prop] === value) return n;
    return undefined;
  }

  /** The first edge of this load with `label` whose `prop` is `value`. */
  findEdge(label: string, prop: string, value: Value): MockEdge | undefined {
    for (const e of this.graph.edges.values())
      if (e.label === label && this.own(e.props) && e.props[prop] === value)
        return e;
    return undefined;
  }
}
