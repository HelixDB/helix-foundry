// Where new nodes appear before the simulation takes over. A node lands next
// to the neighbors it already has on the canvas, so expanding a node grows the
// graph in place instead of shuffling it.

export interface XY {
  x: number;
  y: number;
}

export interface PlacementInput {
  /** Ids to place, in graph order. */
  ids: readonly string[];
  /** Positions of nodes already on the canvas. */
  placed: ReadonlyMap<string, XY>;
  /** Undirected adjacency of the whole graph, new nodes included. */
  neighbors: ReadonlyMap<string, readonly string[]>;
  labelOf: (id: string) => string;
  /** Where a node with no placed neighbor and no placed node of its label goes: the viewport center, in world units. */
  center: XY;
  random?: () => number;
}

const GOLDEN_ANGLE = Math.PI * (3 - Math.sqrt(5));
/** Distance between disconnected seeds on the spiral around the center. */
const SEED_SPACING = 70;
/** Growth of the fan around a node: about one collision diameter between neighbors. */
const FAN_SPACING = 8;

/**
 * Positions for `ids`. Nodes are placed breadth-first from whatever is already
 * on the canvas: each one goes near the centroid of its placed neighbors, so a
 * node expanded into the graph fans out around it. A node with nothing placed nearby goes near nodes of its label, else on
 * a spiral around `center`.
 */
export function placeNewNodes(input: PlacementInput): Map<string, XY> {
  const { ids, placed, neighbors, labelOf, center } = input;
  const random = input.random ?? Math.random;
  const result = new Map<string, XY>();
  const pending = new Set(ids);
  const positionOf = (id: string) => result.get(id) ?? placed.get(id);
  const degree = (id: string) => neighbors.get(id)?.length ?? 0;

  // Running label centroids, so disconnected nodes join their label's area.
  const labelSums = new Map<string, { x: number; y: number; n: number }>();
  const addToLabel = (id: string, p: XY) => {
    const label = labelOf(id);
    const sum = labelSums.get(label);
    if (sum) {
      sum.x += p.x;
      sum.y += p.y;
      sum.n++;
    } else {
      labelSums.set(label, { x: p.x, y: p.y, n: 1 });
    }
  };
  for (const [id, p] of placed) addToLabel(id, p);

  const put = (id: string, p: XY) => {
    result.set(id, p);
    pending.delete(id);
    addToLabel(id, p);
  };

  const around = (anchor: XY, radius: number): XY => {
    const angle = random() * Math.PI * 2;
    const dist = radius * (0.65 + 0.35 * random());
    return {
      x: anchor.x + Math.cos(angle) * dist,
      y: anchor.y + Math.sin(angle) * dist,
    };
  };

  // Children of one node fan out on a sunflower spiral around it, so even a
  // hub's hundreds of new neighbors start evenly spaced rather than piled up.
  const fans = new Map<string, { angle: number; next: number }>();
  const fanOut = (hubId: string, hub: XY): XY => {
    let fan = fans.get(hubId);
    if (!fan) {
      let placedAround = 0;
      for (const nb of neighbors.get(hubId) ?? [])
        if (placed.has(nb)) placedAround++;
      fan = { angle: random() * Math.PI * 2, next: placedAround };
      fans.set(hubId, fan);
    }
    const k = fan.next++;
    const angle = fan.angle + k * GOLDEN_ANGLE + (random() - 0.5) * 0.3;
    const dist = 24 + FAN_SPACING * Math.sqrt(k) + random() * 4;
    return {
      x: hub.x + Math.cos(angle) * dist,
      y: hub.y + Math.sin(angle) * dist,
    };
  };

  const placeNearNeighbors = (id: string): boolean => {
    let sx = 0;
    let sy = 0;
    let n = 0;
    let hub = "";
    for (const nb of neighbors.get(id) ?? []) {
      const p = nb === id ? undefined : positionOf(nb);
      if (!p) continue;
      sx += p.x;
      sy += p.y;
      n++;
      hub = nb;
    }
    if (n === 0) return false;
    // One placed neighbor: fan out around it. Several: sit between them.
    put(
      id,
      n === 1
        ? fanOut(hub, { x: sx, y: sy })
        : around({ x: sx / n, y: sy / n }, 12),
    );
    return true;
  };

  const queue: string[] = [];
  const queued = new Set<string>();
  const enqueueNeighbors = (id: string) => {
    for (const nb of neighbors.get(id) ?? []) {
      if (pending.has(nb) && !queued.has(nb)) {
        queued.add(nb);
        queue.push(nb);
      }
    }
  };

  for (const id of ids) {
    if ((neighbors.get(id) ?? []).some((nb) => nb !== id && placed.has(nb))) {
      queued.add(id);
      queue.push(id);
    }
  }

  // Seeds for nodes that can't be reached from the canvas: biggest hubs first.
  const seeds = [...ids].sort((a, b) => degree(b) - degree(a));
  let seedCursor = 0;
  let spiral = 0;

  while (pending.size > 0) {
    for (let head = 0; head < queue.length; head++) {
      const id = queue[head];
      if (!pending.has(id)) continue;
      if (placeNearNeighbors(id)) enqueueNeighbors(id);
    }
    queue.length = 0;
    if (pending.size === 0) break;

    while (seedCursor < seeds.length && !pending.has(seeds[seedCursor]))
      seedCursor++;
    const seed = seeds[seedCursor];
    const sum = labelSums.get(labelOf(seed));
    if (sum) {
      const radius = 14 * Math.sqrt(sum.n + 1);
      const angle = random() * Math.PI * 2;
      const dist = radius * Math.sqrt(random());
      put(seed, {
        x: sum.x / sum.n + Math.cos(angle) * dist,
        y: sum.y / sum.n + Math.sin(angle) * dist,
      });
    } else {
      const r = SEED_SPACING * Math.sqrt(spiral);
      const angle = spiral * GOLDEN_ANGLE;
      spiral++;
      put(seed, {
        x: center.x + Math.cos(angle) * r + (random() - 0.5) * 4,
        y: center.y + Math.sin(angle) * r + (random() - 0.5) * 4,
      });
    }
    queued.add(seed);
    enqueueNeighbors(seed);
  }
  return result;
}

/** Bounds of the given points, or null when there are none. */
export function boundsOf(points: Iterable<{ x?: number; y?: number }>): {
  minX: number;
  minY: number;
  maxX: number;
  maxY: number;
} | null {
  let minX = Infinity;
  let minY = Infinity;
  let maxX = -Infinity;
  let maxY = -Infinity;
  for (const p of points) {
    if (
      p.x === undefined ||
      p.y === undefined ||
      !Number.isFinite(p.x) ||
      !Number.isFinite(p.y)
    )
      continue;
    if (p.x < minX) minX = p.x;
    if (p.y < minY) minY = p.y;
    if (p.x > maxX) maxX = p.x;
    if (p.y > maxY) maxY = p.y;
  }
  return Number.isFinite(minX) ? { minX, minY, maxX, maxY } : null;
}
