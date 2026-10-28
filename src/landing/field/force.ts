// The Explorer's layout, as the app's ObjectGraph simulation settles it: each
// type around its own point on a circle. It is seeded, so it always settles
// the same way, and scripts/export-data.ts runs it once and stores the
// positions with the records; the page never runs the simulation.
import {
  forceCollide,
  forceLink,
  forceManyBody,
  forceSimulation,
  forceX,
  forceY,
  type SimulationLinkDatum,
  type SimulationNodeDatum,
} from "d3-force";

// The app's radius for a record with this many links.
export const radiusOf = (degree: number) =>
  4 + Math.min(9, Math.sqrt(degree) * 1.5);

type Node = SimulationNodeDatum & { i: number; r: number; type: string };

// Positions in layout units, centred on 0, rounded to a hundredth.
export function forceLayout(
  nodes: { type: string; degree: number }[],
  links: readonly (readonly [number, number, ...unknown[]])[],
) {
  const types = [...new Set(nodes.map((n) => n.type))].sort();
  const list: Node[] = nodes.map((n, i) => ({
    i,
    r: radiusOf(n.degree),
    type: n.type,
  }));
  const spread = 60 + Math.sqrt(list.length) * 9;
  const anchor = new Map(
    types.map((t, i) => {
      const a = (i / types.length) * Math.PI * 2 - Math.PI / 2;
      return [t, { x: Math.cos(a) * spread, y: Math.sin(a) * spread }];
    }),
  );
  let seed = 7;
  const random = () => ((seed = (seed * 16807) % 2147483647) - 1) / 2147483646;
  const edges: SimulationLinkDatum<Node>[] = links.map(([s, t]) => ({
    source: s,
    target: t,
  }));
  const sim = forceSimulation<Node>(list)
    .randomSource(random)
    .force(
      "link",
      forceLink<Node, SimulationLinkDatum<Node>>(edges)
        .id((n) => n.i)
        .distance(28)
        .strength(0.35),
    )
    .force("charge", forceManyBody<Node>().strength(-26).distanceMax(260))
    .force("x", forceX<Node>((n) => anchor.get(n.type)!.x).strength(0.06))
    .force("y", forceY<Node>((n) => anchor.get(n.type)!.y).strength(0.06))
    .force(
      "collide",
      forceCollide<Node>((n) => n.r + 1.5),
    )
    .stop();
  for (let i = 0; i < 320; i++) sim.tick();
  const xs = list.map((n) => n.x!),
    ys = list.map((n) => n.y!);
  const cx = (Math.min(...xs) + Math.max(...xs)) / 2,
    cy = (Math.min(...ys) + Math.max(...ys)) / 2;
  const round = (v: number) => Math.round(v * 100) / 100;
  return {
    x: xs.map((x) => round(x - cx)),
    y: ys.map((y) => round(y - cy)),
    w: round(Math.max(...xs) - Math.min(...xs)),
    h: round(Math.max(...ys) - Math.min(...ys)),
  };
}
