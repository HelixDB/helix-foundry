// The hero's graph: a fixed network in a unit ball. Clusters of records, the
// strands of links between them and a thin halo, with a hub in each cluster
// whose spokes reach its members. Deterministic for a given size.
export type HeroGraph = {
  n: number;
  // x, y, z per node.
  pos: Float32Array;
  // 0 for an ordinary node, up to 1 for a hub.
  weight: Float32Array;
  // Node index pairs.
  edges: Uint32Array;
};

export function mulberry32(seed: number) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export function heroGraph(n: number, seed = 23): HeroGraph {
  const rnd = mulberry32(seed);
  const gauss = () => {
    const u = Math.max(rnd(), 1e-9),
      v = rnd();
    return Math.sqrt(-2 * Math.log(u)) * Math.cos(2 * Math.PI * v);
  };
  const pos = new Float32Array(n * 3),
    weight = new Float32Array(n);

  // Cluster centres on a Fibonacci sphere, a little uneven, and one near the middle.
  const K = 11;
  const centres: number[][] = [],
    spread: number[] = [],
    size: number[] = [];
  for (let k = 0; k < K; k++) {
    if (k === 0) {
      centres.push([0.04, -0.02, 0.03]);
    } else {
      const i = k - 0.5,
        y = 1 - (2 * i) / (K - 1),
        r = Math.sqrt(1 - y * y),
        a = i * Math.PI * (3 - Math.sqrt(5)),
        d = 0.5 + rnd() * 0.2;
      centres.push([Math.cos(a) * r * d, y * d * 0.82, Math.sin(a) * r * d]);
    }
    spread.push(0.09 + rnd() * 0.08);
    size.push(0.5 + rnd());
  }
  const total = size.reduce((s, v) => s + v, 0);
  const pickCluster = () => {
    let x = rnd() * total;
    for (let k = 0; k < K; k++) if ((x -= size[k]) <= 0) return k;
    return K - 1;
  };
  // Strands between neighbouring clusters.
  const strands: [number, number][] = [];
  for (let a = 0; a < K; a++) {
    const near = centres
      .map((c, b) => [
        b,
        Math.hypot(
          c[0] - centres[a][0],
          c[1] - centres[a][1],
          c[2] - centres[a][2],
        ),
      ])
      .filter(([b]) => b !== a)
      .sort((p, q) => p[1] - q[1])
      .slice(0, 3);
    for (const [b] of near) if (a < b) strands.push([a, b]);
  }

  const cluster = new Int16Array(n).fill(-1);
  for (let i = 0; i < n; i++) {
    const kind = rnd();
    let x: number, y: number, z: number;
    if (kind < 0.56) {
      const k = pickCluster(),
        c = centres[k],
        s = spread[k];
      cluster[i] = k;
      x = c[0] + gauss() * s;
      y = c[1] + gauss() * s * 0.85;
      z = c[2] + gauss() * s;
    } else if (kind < 0.84) {
      const [a, b] = strands[Math.floor(rnd() * strands.length)],
        t = rnd(),
        bow = Math.sin(Math.PI * t) * 0.12;
      x = centres[a][0] + (centres[b][0] - centres[a][0]) * t + gauss() * 0.028;
      y =
        centres[a][1] +
        (centres[b][1] - centres[a][1]) * t +
        gauss() * 0.028 +
        bow;
      z = centres[a][2] + (centres[b][2] - centres[a][2]) * t + gauss() * 0.028;
    } else {
      // Halo: sparse, toward the rim.
      const u = rnd() * 2 - 1,
        a = rnd() * Math.PI * 2,
        r = 0.55 + Math.pow(rnd(), 0.5) * 0.45;
      const s = Math.sqrt(1 - u * u);
      x = Math.cos(a) * s * r;
      y = u * r * 0.86;
      z = Math.sin(a) * s * r;
    }
    pos[i * 3] = x;
    pos[i * 3 + 1] = y;
    pos[i * 3 + 2] = z;
    weight[i] = Math.pow(rnd(), 4) * 0.5;
  }

  // Nearest neighbours through a spatial hash.
  const CELL = 0.09,
    key = (x: number, y: number, z: number) =>
      ((Math.floor(x / CELL) + 64) * 128 + Math.floor(y / CELL) + 64) * 128 +
      Math.floor(z / CELL) +
      64;
  const grid = new Map<number, number[]>();
  for (let i = 0; i < n; i++) {
    const k = key(pos[i * 3], pos[i * 3 + 1], pos[i * 3 + 2]);
    let list = grid.get(k);
    if (!list) grid.set(k, (list = []));
    list.push(i);
  }
  const seen = new Set<number>();
  const edges: number[] = [];
  const join = (a: number, b: number) => {
    if (a === b) return;
    const id = a < b ? a * n + b : b * n + a;
    if (seen.has(id)) return;
    seen.add(id);
    edges.push(a, b);
  };
  for (let i = 0; i < n; i++) {
    const x = pos[i * 3],
      y = pos[i * 3 + 1],
      z = pos[i * 3 + 2];
    const cx = Math.floor(x / CELL),
      cy = Math.floor(y / CELL),
      cz = Math.floor(z / CELL);
    let best = -1,
      bestD = Infinity,
      second = -1,
      secondD = Infinity;
    for (let dx = -1; dx <= 1; dx++)
      for (let dy = -1; dy <= 1; dy++)
        for (let dz = -1; dz <= 1; dz++) {
          const list = grid.get(
            ((cx + dx + 64) * 128 + cy + dy + 64) * 128 + cz + dz + 64,
          );
          if (!list) continue;
          for (const j of list) {
            if (j === i) continue;
            const d =
              (pos[j * 3] - x) ** 2 +
              (pos[j * 3 + 1] - y) ** 2 +
              (pos[j * 3 + 2] - z) ** 2;
            if (d < bestD) {
              second = best;
              secondD = bestD;
              best = j;
              bestD = d;
            } else if (d < secondD) {
              second = j;
              secondD = d;
            }
          }
        }
    if (best >= 0) join(i, best);
    if (second >= 0 && rnd() < 0.42) join(i, second);
  }

  // A hub per cluster: the member nearest its centre, with spokes to members
  // and links to the neighbouring hubs.
  const hubs: number[] = [];
  for (let k = 0; k < K; k++) {
    let hub = -1,
      d0 = Infinity;
    const members: number[] = [];
    for (let i = 0; i < n; i++) {
      if (cluster[i] !== k) continue;
      members.push(i);
      const d =
        (pos[i * 3] - centres[k][0]) ** 2 +
        (pos[i * 3 + 1] - centres[k][1]) ** 2 +
        (pos[i * 3 + 2] - centres[k][2]) ** 2;
      if (d < d0) [hub, d0] = [i, d];
    }
    if (hub < 0) continue;
    hubs.push(hub);
    weight[hub] = 1;
    const spokes = Math.min(members.length, 10 + Math.floor(rnd() * 14));
    for (let s = 0; s < spokes; s++)
      join(hub, members[Math.floor(rnd() * members.length)]);
    // A few secondary hubs, so the clusters have structure inside.
    for (let s = 0; s < 3 && members.length > 20; s++) {
      const sub = members[Math.floor(rnd() * members.length)];
      weight[sub] = Math.max(weight[sub], 0.7);
      join(hub, sub);
      for (let t = 0; t < 5; t++)
        join(sub, members[Math.floor(rnd() * members.length)]);
    }
  }
  for (const [a, b] of strands)
    if (hubs[a] !== undefined && hubs[b] !== undefined) join(hubs[a], hubs[b]);

  return { n, pos, weight, edges: Uint32Array.from(edges) };
}
