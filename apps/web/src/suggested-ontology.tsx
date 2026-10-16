import {
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
} from "react";
import {
  ArrowLeft,
  ArrowRight,
  Braces,
  CaseSensitive,
  Database,
  FileUp,
  GitMerge,
  Globe2,
  LoaderCircle,
  Pencil,
  Sparkles,
  TriangleAlert,
  Unlink,
  Waypoints,
} from "lucide-react";
import type {
  Resource,
  SuggestedEntity,
  SuggestedLink,
  SuggestedOntology,
} from "../../../packages/shared/src";
import { Link } from "react-router-dom";
import { number } from "./api";
import "./suggested-ontology.css";

const sourceNames: Record<string, string> = {
  planetscale: "PlanetScale",
  workos: "WorkOS",
  posthog: "PostHog",
  stripe: "Stripe",
  s3: "S3",
  postgres: "PostgreSQL",
  mysql: "MySQL",
  neo4j: "Neo4j",
  neon: "Neon",
  supabase: "Supabase",
  upload: "Upload",
  rest: "REST API",
  api: "Ingestion API",
};
const logoFiles: Record<string, string> = {
  planetscale: "planetscale",
  workos: "workos",
  posthog: "posthog",
  stripe: "stripe",
  s3: "s3",
  postgres: "postgresql",
  mysql: "mysql-wordmark",
  neon: "neon",
  supabase: "supabase",
};
const genericIcons = {
  upload: FileUp,
  rest: Globe2,
  api: Braces,
  neo4j: Waypoints,
};

function sourceKey(...candidates: unknown[]) {
  const keys = candidates
    .filter((c): c is string => typeof c === "string" && c.length > 0)
    .map((c) => (c.toLowerCase() === "postgresql" ? "postgres" : c));
  return (
    keys.find((k) => sourceNames[k.toLowerCase()])?.toLowerCase() ||
    keys
      .map((k) =>
        Object.keys(sourceNames).find(
          (s) => sourceNames[s].toLowerCase() === k.toLowerCase(),
        ),
      )
      .find(Boolean) ||
    keys[0]?.toLowerCase() ||
    "upload"
  );
}

function SourceLogo({ source }: { source: string }) {
  const file = logoFiles[source];
  const Icon = genericIcons[source as keyof typeof genericIcons] || Database;
  return (
    <span className={`so-logo${source === "mysql" ? " is-wordmark" : ""}`}>
      {file ? (
        <img src={`/connectors/${file}.svg`} alt="" />
      ) : (
        <Icon size={11} aria-hidden />
      )}
    </span>
  );
}

// Card heights are fixed so the schema lays out without measuring the DOM;
// the CSS sizes every row of a card to match (a name may take two lines).
const cardHeight = (e: SuggestedEntity, lines: number) =>
  138 +
  22 * (lines - 1) +
  20 * e.members.length +
  (e.members.length > 1 || e.kind === "activity" ? 26 : 0);
const VW = 1000;
// Past this many entities cards get too small to read, so they stack instead.
const MAX_DRAWN = 16;
// Narrower cards cut names and tables short, so the schema stacks instead.
const MIN_CARD = 250;
const MIN_CARD_READONLY = 200;
// Width the drawn schema breaks out to; matches --so-canvas in the CSS.
const canvasWidth = (wide: boolean) =>
  Math.min(wide ? 1320 : 1120, window.innerWidth - 64);
const sentence = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
const many = (s: string) =>
  /[^aeiou]y$/.test(s)
    ? s.slice(0, -1) + "ies"
    : /(s|x|ch|sh)$/.test(s)
      ? s + "es"
      : s + "s";
// Lines a card's name needs at this card width, from average glyph widths.
function nameLines(name: string, e: SuggestedEntity, cardPx: number) {
  const room =
    cardPx -
    32 -
    40 -
    24 -
    (e.members.length > 1 ? 13 + 11 * e.members.length : 0);
  return name.length * 8.4 > room ? 2 : 1;
}
// Short screens (see the CSS too) get tighter rows, so the hub's row starts higher.
const isShort = () => window.innerHeight <= 880;
type Side = "top" | "bottom" | "left" | "right";
type Box = { x: number; y: number; w: number; h: number };
type Point = [number, number];

// Deterministic PRNG so restarts of the layout search are stable across renders.
function seeded(seed: number) {
  return () => {
    seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
// Scalar geometry helpers: the layout search calls these hundreds of thousands of times.
const orient = (
  ax: number,
  ay: number,
  bx: number,
  by: number,
  cx: number,
  cy: number,
) => Math.sign((bx - ax) * (cy - ay) - (by - ay) * (cx - ax));
// Liang-Barsky: does segment p-q pass through the card-sized box centred on c?
function hitsBox(
  px: number,
  py: number,
  qx: number,
  qy: number,
  cx: number,
  cy: number,
) {
  const dx = qx - px,
    dy = qy - py,
    d = [-dx, dx, -dy, dy],
    room = [px - cx + 0.37, cx + 0.37 - px, py - cy + 0.27, cy + 0.27 - py];
  let t0 = 0,
    t1 = 1;
  for (let k = 0; k < 4; k++) {
    if (d[k] === 0) {
      if (room[k] < 0) return false;
    } else if (d[k] < 0) t0 = Math.max(t0, room[k] / d[k]);
    else t1 = Math.min(t1, room[k] / d[k]);
    if (t0 > t1) return false;
  }
  return true;
}

// Places entities on a small grid around the most-linked entity, searching for
// the arrangement with short links, no links through cards and few crossings.
function arrange(entities: SuggestedEntity[], links: SuggestedLink[]) {
  const n = entities.length,
    index = new Map(entities.map((e, i) => [e.id, i]));
  const directed = links
    .map((l) => [index.get(l.from), index.get(l.to)])
    .filter(
      (p): p is [number, number] =>
        p[0] !== undefined && p[1] !== undefined && p[0] !== p[1],
    );
  const pairs = [
    ...new Map(
      directed.map(([a, b]) => [a < b ? `${a}:${b}` : `${b}:${a}`, [a, b]]),
    ).values(),
  ];
  const degree = entities.map(
    (_, i) => pairs.filter((p) => p.includes(i)).length,
  );
  const rank = entities
    .map((e, i) => i)
    .sort(
      (a, b) =>
        degree[b] - degree[a] ||
        Number(entities[a].kind === "activity") -
          Number(entities[b].kind === "activity") ||
        entities[b].rows - entities[a].rows ||
        a - b,
    );
  const hub = rank[0] ?? 0;
  const cols = n <= 9 ? 3 : n <= 16 ? 4 : 5,
    rows = n <= 3 ? 1 : Math.ceil((n + 1) / cols),
    slots = rows * cols,
    hubSlot = Math.floor((rows - 1) / 2) * cols + Math.floor((cols - 1) / 2);
  // Slot centres in column units; rows are shorter than columns are wide.
  const RH = 0.62,
    X = [...Array(slots)].map((_, s) => (s % cols) + 0.5),
    Y = [...Array(slots)].map((_, s) => (Math.floor(s / cols) + 0.5) * RH);
  function cost(pos: number[]) {
    let total = 0;
    for (const [a, b] of pairs) {
      const p = pos[a],
        q = pos[b];
      if (p < 0 || q < 0) continue;
      const dc = Math.abs(X[p] - X[q]),
        dr = Math.abs(Y[p] - Y[q]) / RH;
      total +=
        Math.hypot(dc, dr * RH) +
        (dc && dr ? 0.3 : 0) +
        Math.max(0, Math.max(dc, dr) - 1) * 2.5;
      for (let k = 0; k < n; k++) {
        const c = pos[k];
        if (c >= 0 && k !== a && k !== b)
          if (hitsBox(X[p], Y[p], X[q], Y[q], X[c], Y[c])) total += 30;
      }
    }
    for (let i = 0; i < pairs.length; i++)
      for (let j = i + 1; j < pairs.length; j++) {
        const [a, b] = pairs[i],
          [c, d] = pairs[j],
          [P, Q, R, S] = [pos[a], pos[b], pos[c], pos[d]];
        if (a === c || a === d || b === c || b === d) continue;
        if (P < 0 || Q < 0 || R < 0 || S < 0) continue;
        if (
          orient(X[P], Y[P], X[Q], Y[Q], X[R], Y[R]) *
            orient(X[P], Y[P], X[Q], Y[Q], X[S], Y[S]) <
            0 &&
          orient(X[R], Y[R], X[S], Y[S], X[P], Y[P]) *
            orient(X[R], Y[R], X[S], Y[S], X[Q], Y[Q]) <
            0
        )
          total += 8;
      }
    // Prefer the referenced ("one") side above the referencing side.
    for (const [from, to] of directed)
      if (pos[from] >= 0 && pos[to] >= 0)
        total += Math.sign(Y[pos[to]] - Y[pos[from]]) * 0.12;
    for (let i = 0; i < n; i++)
      if (pos[i] >= 0 && i !== hub)
        total +=
          0.06 * Math.hypot(X[pos[i]] - X[hubSlot], Y[pos[i]] - Y[hubSlot]);
    return total;
  }
  function improve(start: number[]): [number[], number] {
    let pos = start,
      best = cost(pos);
    for (let round = 0; round < 40; round++) {
      let moved = false;
      for (let i = 0; i < n; i++) {
        if (i === hub) continue;
        for (let s = 0; s < slots; s++) {
          if (s === pos[i] || s === hubSlot) continue;
          const next = pos.slice(),
            j = pos.indexOf(s);
          next[i] = s;
          if (j >= 0) next[j] = pos[i];
          const c = cost(next);
          if (c < best - 1e-9) {
            pos = next;
            best = c;
            moved = true;
          }
        }
      }
      if (!moved) break;
    }
    return [pos, best];
  }
  // Breadth-first from the hub gives the greedy start and the reveal order.
  const order = [hub];
  for (let k = 0; k < order.length || order.length < n; k++) {
    if (k >= order.length) order.push(rank.find((i) => !order.includes(i))!);
    for (const i of rank)
      if (
        !order.includes(i) &&
        pairs.some(
          ([a, b]) =>
            (a === order[k] && b === i) || (b === order[k] && a === i),
        )
      )
        order.push(i);
  }
  const greedy = new Array(n).fill(-1);
  greedy[hub] = hubSlot;
  for (const i of order.slice(1)) {
    let bestSlot = -1,
      bestCost = Infinity;
    for (let s = 0; s < slots; s++) {
      if (greedy.includes(s)) continue;
      greedy[i] = s;
      const c = cost(greedy);
      if (c < bestCost) [bestSlot, bestCost] = [s, c];
    }
    greedy[i] = bestSlot;
  }
  let [pos, best] = improve(greedy);
  const random = seeded(n * 7919 + pairs.length);
  for (let attempt = 0; attempt < (n <= 9 ? 6 : 2) && n > 2; attempt++) {
    const free = [...Array(slots).keys()].filter((s) => s !== hubSlot);
    for (let i = free.length - 1; i > 0; i--) {
      const j = Math.floor(random() * (i + 1));
      [free[i], free[j]] = [free[j], free[i]];
    }
    const start = entities.map((_, i) => (i === hub ? hubSlot : free.pop()!));
    const [p, c] = improve(start);
    if (c < best - 1e-9) [pos, best] = [p, c];
  }
  return {
    hub,
    order,
    cells: pos.map((s) => ({ col: s % cols, row: Math.floor(s / cols) })),
  };
}
type Arrangement = ReturnType<typeof arrange>;

type Route = {
  link: SuggestedLink;
  index: number;
  sides: [Side, Side];
  desired: [number, number];
};
const gridOf = (a: Arrangement) => {
  const cols = a.cells.map((c) => c.col);
  return Math.max(Math.max(...cols) - Math.min(...cols) + 1, 3);
};
const cardShare = (grid: number) => (grid > 3 ? 0.8 : 0.74);

function layoutSchema(
  entities: SuggestedEntity[],
  links: SuggestedLink[],
  { hub, order, cells }: Arrangement,
  canvasPx: number,
  short: boolean,
  nameOf: (e: SuggestedEntity) => string,
) {
  const ROW_GAP = short ? 64 : 84,
    PAD_Y = short ? 20 : 28;
  const minCol = Math.min(...cells.map((c) => c.col)),
    minRow = Math.min(...cells.map((c) => c.row));
  const used = cells.map((c) => ({ col: c.col - minCol, row: c.row - minRow }));
  const cols = Math.max(0, ...used.map((c) => c.col)) + 1,
    rows = Math.max(0, ...used.map((c) => c.row)) + 1;
  const grid = Math.max(cols, 3),
    colW = VW / grid,
    x0 = ((grid - cols) * colW) / 2,
    cardW = colW * cardShare(grid),
    cardPx = (cardW * canvasPx) / VW;
  const lines = entities.map((e) => nameLines(nameOf(e), e, cardPx));
  const heightOf = (i: number) => cardHeight(entities[i], lines[i]);
  // A row with fewer cards than columns is centred, so the schema stays balanced.
  const shift = [...Array(rows)].map((_, r) => {
    const taken = used.filter((c) => c.row === r).map((c) => c.col);
    const lo = Math.min(...taken),
      hi = Math.max(...taken);
    return taken.length === hi - lo + 1 ? (cols - 1 - lo - hi) / 2 : 0;
  });
  const rowH = [...Array(rows)].map((_, r) =>
    Math.max(
      0,
      ...entities.map((_, i) => (used[i].row === r ? heightOf(i) : 0)),
    ),
  );
  const rowTop = rowH.map(
    (_, r) => PAD_Y + rowH.slice(0, r).reduce((sum, h) => sum + h + ROW_GAP, 0),
  );
  const height = rowTop[rows - 1] + rowH[rows - 1] + PAD_Y;
  const boxes = new Map<
    string,
    Box & { col: number; row: number; at: number; lines: number }
  >();
  entities.forEach((e, i) => {
    const { col, row } = used[i],
      h = heightOf(i),
      at = col + shift[row];
    boxes.set(e.id, {
      col,
      row,
      at,
      lines: lines[i],
      x: x0 + at * colW + (colW - cardW) / 2,
      y: rowTop[row] + (rowH[row] - h) / 2,
      w: cardW,
      h,
    });
  });
  const occupied = new Set(used.map((c) => `${c.col}:${c.row}`));
  const routes: Route[] = [];
  links.forEach((link, index) => {
    const a = boxes.get(link.from),
      b = boxes.get(link.to);
    if (!a || !b) return;
    if (link.from === link.to) {
      routes.push({
        link,
        index,
        sides: ["right", "right"],
        desired: [0.3, 0.7],
      });
      return;
    }
    const dc = b.col - a.col,
      dr = b.row - a.row,
      toward = (d: number) => (d === 0 ? 0.5 : d < 0 ? 0.24 : 0.76);
    if (dr !== 0)
      routes.push({
        link,
        index,
        sides: dr > 0 ? ["bottom", "top"] : ["top", "bottom"],
        desired: [toward(b.at - a.at), toward(a.at - b.at)],
      });
    else {
      const blocked = [...Array(Math.abs(dc) - 1)].some((_, k) =>
        occupied.has(`${a.col + Math.sign(dc) * (k + 1)}:${a.row}`),
      );
      routes.push({
        link,
        index,
        sides: blocked
          ? ["top", "top"]
          : dc > 0
            ? ["right", "left"]
            : ["left", "right"],
        desired: blocked ? [toward(dc), toward(-dc)] : [0.5, 0.5],
      });
    }
  });
  // Spread the links that share a card side so they meet it at distinct ports.
  const ports = new Map<string, number>();
  const bySide = new Map<
    string,
    { key: string; desired: number; other: number; index: number }[]
  >();
  for (const r of routes)
    ([0, 1] as const).forEach((end) => {
      const own = end ? r.link.to : r.link.from,
        other = boxes.get(end ? r.link.from : r.link.to)!,
        side = r.sides[end],
        list = bySide.get(`${own}|${side}`) || [];
      list.push({
        key: `${r.index}:${end}`,
        desired: r.desired[end],
        other:
          side === "top" || side === "bottom"
            ? other.x + other.w / 2
            : other.y + other.h / 2,
        index: r.index,
      });
      bySide.set(`${own}|${side}`, list);
    });
  for (const [key, list] of bySide) {
    const vertical = /\|(left|right)$/.test(key),
      sep = vertical ? 0.22 : 0.17,
      lo = vertical ? 0.25 : 0.14,
      hi = 1 - lo;
    list.sort(
      (a, b) => a.other - b.other || a.desired - b.desired || a.index - b.index,
    );
    const t = list.map((p) => p.desired);
    if (list.length > 1 && (list.length - 1) * sep > hi - lo)
      t.forEach((_, i) => (t[i] = lo + ((hi - lo) * i) / (list.length - 1)));
    else {
      for (let i = 1; i < t.length; i++) t[i] = Math.max(t[i], t[i - 1] + sep);
      t[t.length - 1] = Math.min(t[t.length - 1], hi);
      for (let i = t.length - 2; i >= 0; i--)
        t[i] = Math.min(t[i], t[i + 1] - sep);
      t[0] = Math.max(t[0], lo);
    }
    list.forEach((p, i) => ports.set(p.key, t[i]));
  }
  const at = (b: Box, side: Side, t: number): Point =>
    side === "top"
      ? [b.x + b.w * t, b.y]
      : side === "bottom"
        ? [b.x + b.w * t, b.y + b.h]
        : side === "left"
          ? [b.x, b.y + b.h * t]
          : [b.x + b.w, b.y + b.h * t];
  const bezier = (p: Point[], t: number): Point => {
    const u = 1 - t;
    return [0, 1].map(
      (k) =>
        u * u * u * p[0][k] +
        3 * u * u * t * p[1][k] +
        3 * u * t * t * p[2][k] +
        t * t * t * p[3][k],
    ) as Point;
  };
  const pills: Box[] = [];
  const cards = [...boxes.values()];
  const reveal = new Map(order.map((i, k) => [entities[i].id, k]));
  const edges = routes.map((r) => {
    const a = boxes.get(r.link.from)!,
      b = boxes.get(r.link.to)!,
      start = at(a, r.sides[0], ports.get(`${r.index}:0`)!),
      tip = at(b, r.sides[1], ports.get(`${r.index}:1`)!),
      arrow = 7,
      step: Point =
        r.sides[1] === "top"
          ? [0, -arrow]
          : r.sides[1] === "bottom"
            ? [0, arrow]
            : r.sides[1] === "left"
              ? [-arrow, 0]
              : [arrow, 0],
      end: Point = [tip[0] + step[0], tip[1] + step[1]];
    const self = r.link.from === r.link.to;
    let c1: Point, c2: Point;
    if (self) {
      c1 = [start[0] + 52, start[1]];
      c2 = [end[0] + 45, end[1]];
    } else if (r.sides[0] === "top" && r.sides[1] === "top") {
      const lift = ROW_GAP * 0.55;
      c1 = [start[0], start[1] - lift];
      c2 = [end[0], end[1] - lift];
    } else if (r.sides[0] === "left" || r.sides[0] === "right") {
      const half = (end[0] - start[0]) / 2;
      c1 = [start[0] + half, start[1]];
      c2 = [end[0] - half, end[1]];
    } else {
      const half = (end[1] - start[1]) / 2;
      c1 = [start[0], start[1] + half];
      c2 = [end[0], end[1] - half];
    }
    const curve = [start, c1, c2, end];
    let length = 0;
    for (let k = 1; k <= 16; k++) {
      const p = bezier(curve, (k - 1) / 16),
        q = bezier(curve, k / 16);
      length += Math.hypot(q[0] - p[0], q[1] - p[1]);
    }
    // Nudge the label along the curve when it would sit on another label or card.
    const pw = ((r.link.name.length * 6.3 + 22) * VW) / canvasPx,
      ph = 22;
    const overlaps = (p: Point) =>
      [...pills, ...cards].some(
        (o) =>
          p[0] - pw / 2 < o.x + o.w + 4 &&
          p[0] + pw / 2 > o.x - 4 &&
          p[1] - ph / 2 < o.y + o.h + 4 &&
          p[1] + ph / 2 > o.y - 4,
      );
    // Parallel links share a corridor, so also try beside the line, not just along it.
    const across: Point =
      self || r.sides[0] === "top" || r.sides[0] === "bottom"
        ? [pw / 2 + 6, 0]
        : [0, ph / 2 + 5];
    const mid =
      [
        [0.5, 0],
        [0.4, 0],
        [0.6, 0],
        [0.5, 1],
        [0.5, -1],
        [0.32, 0],
        [0.68, 0],
        [0.4, 1],
        [0.6, -1],
      ]
        .map(([t, k]): Point => {
          const p = bezier(curve, t);
          return [p[0] + across[0] * k, p[1] + across[1] * k];
        })
        .find((p) => !overlaps(p)) || bezier(curve, 0.5);
    pills.push({ x: mid[0] - pw / 2, y: mid[1] - ph / 2, w: pw, h: ph });
    const [s1, s2] =
      Math.abs(step[0]) > 0
        ? [
            [0, 4.5],
            [0, -4.5],
          ]
        : [
            [4.5, 0],
            [-4.5, 0],
          ];
    return {
      link: r.link,
      path: `M${start.join(" ")}C${c1.join(" ")} ${c2.join(" ")} ${end.join(" ")}`,
      arrow: `M${end[0] + s1[0]} ${end[1] + s1[1]}L${tip.join(" ")}L${end[0] + s2[0]} ${end[1] + s2[1]}Z`,
      mid,
      length: Math.ceil(length * 1.4 + 20),
      reveal: Math.max(reveal.get(r.link.from)!, reveal.get(r.link.to)!),
    };
  });
  return {
    height,
    hub: entities[hub]?.id,
    boxes,
    edges,
    cardPx,
  };
}

// A small hub-and-spoke map of the schema for screens where the cards stack.
function MiniMap({
  entities,
  links,
  order,
  excluded,
  nameOf,
  onPick,
}: {
  entities: Map<string, SuggestedEntity>;
  links: SuggestedLink[];
  order: string[];
  excluded: ReadonlySet<string>;
  nameOf: (id: string) => string;
  onPick: (id: string) => void;
}) {
  const W = 340,
    H = 220,
    others = order.length - 1;
  // Around the hub, each entity linked to it is followed by the entities that only
  // link through it, so their lines stay short.
  const hub = order[0],
    rest = order.slice(1),
    ring: string[] = [];
  const adjacent = (a: string, b: string) =>
    links.some(
      (l) => (l.from === a && l.to === b) || (l.from === b && l.to === a),
    );
  for (const id of rest.filter((x) => adjacent(x, hub))) {
    ring.push(id);
    for (const o of rest)
      if (!ring.includes(o) && !adjacent(o, hub) && adjacent(o, id))
        ring.push(o);
  }
  ring.push(...rest.filter((o) => !ring.includes(o)));
  const at = new Map<string, Point>(
    [hub, ...ring].map((id, i) => {
      if (!i) return [id, [W / 2, H / 2]];
      const a = -Math.PI / 2 + ((i - 1) * 2 * Math.PI) / others,
        // Alternate rings when crowded, so neighbouring labels stay apart.
        r = others > 8 && i % 2 === 0 ? 0.62 : 1;
      return [
        id,
        [W / 2 + 108 * r * Math.cos(a), H / 2 + 84 * r * Math.sin(a)],
      ];
    }),
  );
  return (
    <div className="so-minimap">
      <svg viewBox={`0 0 ${W} ${H}`} aria-hidden>
        {links.map((l) => {
          const a = at.get(l.from),
            b = at.get(l.to);
          if (!a || !b || l.from === l.to) return null;
          const off = excluded.has(l.from) || excluded.has(l.to);
          return (
            <line
              key={l.id}
              className={off ? "is-off" : ""}
              x1={a[0]}
              y1={a[1]}
              x2={b[0]}
              y2={b[1]}
            />
          );
        })}
      </svg>
      <ul aria-label="Map of the suggested entities">
        {order.map((id, i) => {
          const e = entities.get(id)!,
            [x, y] = at.get(id)!;
          return (
            <li
              key={id}
              style={{ left: `${(x / W) * 100}%`, top: `${(y / H) * 100}%` }}
            >
              <button
                type="button"
                className={[
                  "so-mini-node",
                  i ? "" : "is-hub",
                  e.members.length > 1 ? "is-merged" : "",
                  e.kind === "activity" ? "is-activity" : "",
                  excluded.has(id) ? "is-excluded" : "",
                ].join(" ")}
                onClick={() => onPick(id)}
              >
                {nameOf(id)}
              </button>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

const matchText = (l: SuggestedLink) =>
  `${number(l.matchedRows)} of ${number(l.totalRows)} matched${l.caseInsensitive ? " · matched ignoring case" : ""}`;
const evidenceText = {
  declared: "Declared as a foreign key",
  name: "Matched by column name and values",
  values: "Matched by values",
};
const noteIcons = {
  merged: GitMerge,
  unmatched: TriangleAlert,
  case: CaseSensitive,
  unlinked: Unlink,
};
const plural = (n: number, word: string, many = `${word}s`) =>
  `${number(n)} ${n === 1 ? word : many}`;

export function SuggestedOntologyView({
  ontology,
  datasets,
  describing,
  busy,
  error,
  edits,
  focusHeading,
  onConfirm,
  onBack,
  readOnly = false,
  entityHref,
  confirmLabel = "Looks right — build my workspace",
  backLabel = "Back to data",
  fit = readOnly,
}: {
  ontology: SuggestedOntology;
  datasets: Resource[];
  describing?: boolean;
  busy?: boolean;
  error?: string;
  // Edits confirmed earlier for this suggestion, restored when reviewing it again.
  edits?: { excluded: string[]; names: Record<string, string> };
  // Set when the suggestion replaces the loading scene, so focus follows it.
  focusHeading?: boolean;
  onConfirm?: (choice: {
    excluded: string[];
    names: Record<string, string>;
  }) => void;
  onBack?: () => void;
  // The published ontology inside the workspace: no editing, names link out,
  // and the canvas fits its container instead of breaking out to the window.
  readOnly?: boolean;
  entityHref?: (e: SuggestedEntity) => string | undefined;
  // Updating a published ontology says so instead of building a workspace.
  confirmLabel?: string;
  backLabel?: string;
  // Fits its container, beside the workspace sidebar, instead of the window.
  fit?: boolean;
}) {
  describing = describing ?? false;
  busy = busy ?? false;
  const uid = useId();
  const [excluded, setExcluded] = useState<ReadonlySet<string>>(
    () => new Set(edits?.excluded),
  );
  const [names, setNames] = useState<Record<string, string>>(
    () => edits?.names || {},
  );
  const heading = useRef<HTMLHeadingElement>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const [draft, setDraft] = useState("");
  const [hover, setHover] = useState<string | null>(null);
  const [pinned, setPinned] = useState<string | null>(null);
  const [openMerge, setOpenMerge] = useState<string | null>(null);
  const nameButtons = useRef(new Map<string, HTMLButtonElement>());
  const refocus = useRef<string | null>(null);
  const editingRef = useRef<string | null>(null);
  editingRef.current = editing;
  const { entities, links, notes, unplaced } = ontology;
  const arrangement = useMemo(
    () =>
      entities.length && entities.length <= MAX_DRAWN
        ? arrange(entities, links)
        : null,
    [entities, links],
  );
  const wide = !!arrangement && gridOf(arrangement) > 3;
  const root = useRef<HTMLDivElement>(null);
  const [canvasPx, setCanvasPx] = useState(() => canvasWidth(wide));
  const [short, setShort] = useState(isShort);
  useLayoutEffect(() => {
    const measure = () => {
      const room = fit ? root.current?.clientWidth : undefined;
      setCanvasPx(
        room ? Math.min(wide ? 1320 : 1120, room) : canvasWidth(wide),
      );
      setShort(isShort());
    };
    measure();
    window.addEventListener("resize", measure);
    const observer =
      fit && root.current ? new ResizeObserver(measure) : undefined;
    if (observer && root.current) observer.observe(root.current);
    return () => {
      window.removeEventListener("resize", measure);
      observer?.disconnect();
    };
  }, [wide, fit]);
  const fitted = useMemo(
    () =>
      arrangement &&
      layoutSchema(
        entities,
        links,
        arrangement,
        canvasPx,
        short,
        (e) => names[e.id] || e.name,
      ),
    [arrangement, entities, links, canvasPx, short, names],
  );
  // Too narrow for readable cards: they stack, under a small map of the schema.
  // The workspace page sits beside the sidebar, so its cards may be a little
  // narrower before falling back to the stacked list.
  const layout =
    fitted && fitted.cardPx >= (fit ? MIN_CARD_READONLY : MIN_CARD)
      ? fitted
      : null;
  // Hub first, then outward: the order cards are revealed, read and stacked.
  const order = arrangement
    ? arrangement.order.map((i) => entities[i].id)
    : entities
        .map((e) => ({
          id: e.id,
          degree: links.filter((l) => l.from === e.id || l.to === e.id).length,
        }))
        .sort((a, b) => b.degree - a.degree)
        .map((e) => e.id);
  useEffect(() => {
    if (focusHeading) heading.current?.focus({ preventScroll: true });
  }, [focusHeading]);
  const byDataset = useMemo(
    () => new Map(datasets.map((d) => [d.id, d])),
    [datasets],
  );
  useEffect(() => {
    if (editing || !refocus.current) return;
    nameButtons.current.get(refocus.current)?.focus();
    refocus.current = null;
  }, [editing]);

  const entity = new Map(entities.map((e) => [e.id, e]));
  const nameOf = (id: string) => names[id] || entity.get(id)?.name || id;
  const member = (datasetId: string, fallback = datasetId, source = "") => {
    const d = byDataset.get(datasetId),
      full = d?.name || fallback,
      parts = full.split(" · "),
      key = sourceKey(
        source,
        d?.data.sampleSource,
        d?.data.hostedProvider,
        d?.data.kind,
        parts.length > 1 ? parts[0] : "",
      );
    return {
      table: parts.length > 1 ? parts.slice(1).join(" · ") : full,
      source: key,
      label: sourceNames[key] || (parts.length > 1 ? parts[0] : key),
    };
  };
  const datasetLabel = new Map(
    entities.flatMap((e) =>
      e.members.map(
        (m) =>
          [m.datasetId, member(m.datasetId, m.datasetName, m.source)] as const,
      ),
    ),
  );
  const tableOf = (id: string) =>
    datasetLabel.get(id)?.table || member(id).table;
  const visible = (l: SuggestedLink) =>
    !excluded.has(l.from) && !excluded.has(l.to);
  const active = pinned ? `l:${pinned}` : hover;
  const activeLink = active?.startsWith("l:")
    ? links.find((l) => l.id === active.slice(2))
    : undefined;
  const isHot = (l: SuggestedLink) =>
    active === `l:${l.id}` ||
    active === `e:${l.from}` ||
    active === `e:${l.to}`;

  const included = entities.filter((e) => !excluded.has(e.id)),
    records = included.filter((e) => e.kind === "record").length,
    streams = included.filter((e) => e.kind === "activity"),
    // Nothing identifies rows: the workspace is built from the tables alone.
    keyless = !entities.some((e) => e.kind === "record");
  const tables = new Set([
    ...entities.flatMap((e) => e.members.map((m) => m.datasetId)),
    ...unplaced,
  ]).size;
  const sources = [
    ...new Set(
      entities.flatMap((e) =>
        e.members.map((m) => datasetLabel.get(m.datasetId)!.source),
      ),
    ),
  ];
  const merged = notes.filter((n) => n.kind === "merged"),
    checks = notes.filter((n) => n.kind !== "merged");

  function toggle(id: string) {
    setExcluded((prev) => {
      const next = new Set(prev);
      if (!next.delete(id)) next.add(id);
      return next;
    });
    if (pinned) setPinned(null);
  }
  function startRename(e: SuggestedEntity) {
    setDraft(nameOf(e.id));
    setEditing(e.id);
  }
  function finishRename(save: boolean) {
    const id = editingRef.current;
    if (!id) return;
    editingRef.current = null;
    if (save) {
      const value = draft.trim().slice(0, 60),
        original = entity.get(id)?.name;
      setNames((prev) => {
        const next = { ...prev };
        if (!value || value === original) delete next[id];
        else next[id] = value;
        return next;
      });
    }
    refocus.current = id;
    setEditing(null);
  }
  function confirm() {
    onConfirm?.({
      excluded: [...excluded].filter((id) => entity.has(id)),
      names: Object.fromEntries(
        Object.entries(names).filter(([id]) => entity.has(id)),
      ),
    });
  }

  const pick = (id: string) => {
    const card = document.getElementById(`${uid}-card-${id}`);
    card?.scrollIntoView({ behavior: "smooth", block: "center" });
    card
      ?.querySelector<HTMLElement>(".so-name")
      ?.focus({ preventScroll: true });
  };

  return (
    <div
      ref={root}
      className={`so-view${describing ? " is-describing" : ""}${wide ? " is-wide" : ""}${readOnly ? " is-readonly" : ""}${fit ? " is-fitted" : ""}`}
    >
      {readOnly ? (
        <div className="so-stats">
          <span className="so-stack" aria-hidden>
            {sources.map((s) => (
              <SourceLogo key={s} source={s} />
            ))}
          </span>
          <span>
            {plural(tables, "table")} from {plural(sources.length, "source")},
            condensed into{" "}
            <strong>{plural(entities.length, "entity", "entities")}</strong>
            {links.length > 0 && ` and ${plural(links.length, "connection")}`}
          </span>
        </div>
      ) : (
        <header className="so-head">
          <p className="so-eyebrow">
            <Sparkles size={14} aria-hidden /> Suggested ontology
          </p>
          <h1 ref={heading} tabIndex={-1}>
            Here’s how your data fits together
          </h1>
          {ontology.summary ? (
            <p className="so-summary so-writing">{ontology.summary}</p>
          ) : describing ? (
            <p className="so-summary" aria-hidden>
              <span className="so-skeleton" />
              <span className="so-skeleton short" />
            </p>
          ) : null}
          <div className="so-stats">
            <span className="so-stack" aria-hidden>
              {sources.map((s) => (
                <SourceLogo key={s} source={s} />
              ))}
            </span>
            <span>
              {plural(tables, "table")} from {plural(sources.length, "source")},
              condensed into{" "}
              <strong>{plural(entities.length, "entity", "entities")}</strong>
              {links.length > 0 && ` and ${plural(links.length, "connection")}`}
            </span>
          </div>
          <p className="so-hint" role="status">
            {describing && (
              <>
                <LoaderCircle size={13} className="so-spin" aria-hidden /> AI is
                still writing descriptions
              </>
            )}
          </p>
        </header>
      )}

      {entities.length > 0 && (
        <section
          className={`so-canvas${layout ? "" : " is-stacked"}`}
          aria-labelledby={`${uid}-schema`}
        >
          <h2 id={`${uid}-schema`} className="so-sr">
            Suggested entities
          </h2>
          {!layout && entities.length > 1 && entities.length <= MAX_DRAWN && (
            <MiniMap
              entities={entity}
              links={links}
              order={order}
              excluded={excluded}
              nameOf={nameOf}
              onPick={pick}
            />
          )}
          <span id={`${uid}-rename`} hidden>
            Rename
          </span>
          <div
            className="so-stage"
            style={layout ? { height: layout.height } : undefined}
          >
            {layout && (
              <svg
                className={`so-edges${active ? " has-hot" : ""}`}
                viewBox={`0 0 ${VW} ${layout.height}`}
                preserveAspectRatio="none"
                aria-hidden
              >
                {layout.edges.map((e) => {
                  const kind = [
                    "so-edge",
                    entity.get(e.link.from)?.kind === "activity" ||
                    entity.get(e.link.to)?.kind === "activity"
                      ? "is-activity"
                      : "",
                    visible(e.link) ? "" : "is-hidden",
                    isHot(e.link) ? "is-hot" : "",
                  ].join(" ");
                  return (
                    <g
                      key={e.link.id}
                      className={kind}
                      style={
                        {
                          "--len": e.length,
                          "--d": `${260 + e.reveal * 70}ms`,
                        } as CSSProperties
                      }
                    >
                      <path
                        className="so-line"
                        d={e.path}
                        vectorEffect="non-scaling-stroke"
                      />
                      <path className="so-arrow" d={e.arrow} />
                    </g>
                  );
                })}
              </svg>
            )}
            <ul className="so-nodes">
              {order.map((id, index) => {
                const e = entity.get(id)!,
                  box = layout?.boxes.get(id),
                  out = !excluded.has(id),
                  isMerged = e.members.length > 1,
                  sources = [
                    ...new Set(
                      e.members.map(
                        (m) => datasetLabel.get(m.datasetId)!.source,
                      ),
                    ),
                  ],
                  name = nameOf(id),
                  outgoing = links.filter((l) => l.from === id && visible(l)),
                  linked =
                    activeLink &&
                    (activeLink.from === id || activeLink.to === id);
                return (
                  <li
                    key={id}
                    id={`${uid}-card-${id}`}
                    className={[
                      "so-node",
                      e.kind === "activity" ? "is-activity" : "",
                      isMerged ? "is-merged" : "",
                      id === order[0] ? "is-hub" : "",
                      box && box.x + box.w / 2 > VW * 0.6 ? "is-right" : "",
                      out ? "" : "is-excluded",
                      linked || active === `e:${id}` ? "is-hot" : "",
                    ].join(" ")}
                    style={
                      {
                        ...(box && {
                          left: `${box.x / 10}%`,
                          top: box.y,
                          width: `${box.w / 10}%`,
                          height: box.h,
                          "--lines": box.lines,
                        }),
                        "--i": Math.min(index, 12),
                      } as CSSProperties
                    }
                    onMouseEnter={() => setHover(`e:${id}`)}
                    onMouseLeave={() => setHover(null)}
                    onFocus={() => setHover(`e:${id}`)}
                    onBlur={() => setHover(null)}
                  >
                    <div className="so-node-head">
                      <h3>
                        {readOnly ? (
                          entityHref?.(e) ? (
                            <Link className="so-name" to={entityHref(e)!}>
                              <span title={name}>{name}</span>
                            </Link>
                          ) : (
                            <span className="so-name">
                              <span title={name}>{name}</span>
                            </span>
                          )
                        ) : editing === id ? (
                          <input
                            className="so-name-input"
                            value={draft}
                            maxLength={60}
                            autoFocus
                            aria-label={`Name for ${e.name}`}
                            onFocus={(ev) => ev.currentTarget.select()}
                            onChange={(ev) => setDraft(ev.target.value)}
                            onBlur={() => finishRename(true)}
                            onKeyDown={(ev) => {
                              if (ev.key !== "Enter" && ev.key !== "Escape")
                                return;
                              // Focus returns to the name button before keypress,
                              // which would otherwise reopen the editor.
                              ev.preventDefault();
                              ev.stopPropagation();
                              finishRename(ev.key === "Enter");
                            }}
                          />
                        ) : (
                          <button
                            type="button"
                            className="so-name"
                            aria-describedby={`${uid}-rename`}
                            disabled={busy}
                            ref={(el) => {
                              if (el) nameButtons.current.set(id, el);
                              else nameButtons.current.delete(id);
                            }}
                            onClick={() => startRename(e)}
                          >
                            <span title={name}>{name}</span>
                            <Pencil size={12} aria-hidden />
                          </button>
                        )}
                      </h3>
                      {isMerged && (
                        <span
                          className="so-stack so-node-stack"
                          title={`Merged from ${sources.map((s) => sourceNames[s] || s).join(", ")}`}
                          aria-hidden
                        >
                          {sources.map((s) => (
                            <SourceLogo key={s} source={s} />
                          ))}
                        </span>
                      )}
                      {!readOnly && (
                        <label className="so-switch">
                          <input
                            type="checkbox"
                            checked={out}
                            disabled={busy}
                            aria-label={`Include ${name}`}
                            onChange={() => toggle(id)}
                          />
                          <span aria-hidden />
                        </label>
                      )}
                    </div>
                    <p className="so-meta">
                      {e.kind === "activity"
                        ? plural(e.rows, "row")
                        : plural(e.rows, "record")}{" "}
                      · key <code>{e.key}</code>
                    </p>
                    <p
                      className={`so-desc${e.description ? " so-writing" : ""}`}
                      title={e.description || undefined}
                    >
                      {e.description ||
                        (describing ? (
                          <>
                            <span className="so-skeleton" aria-hidden />
                            <span className="so-skeleton short" aria-hidden />
                          </>
                        ) : (
                          <span className="so-fields">
                            {e.properties.slice(0, 6).join(", ")}
                          </span>
                        ))}
                    </p>
                    <div className="so-members">
                      {(isMerged || e.kind === "activity") && (
                        <div className="so-tags">
                          {isMerged && (
                            <span className="so-merge">
                              <button
                                type="button"
                                className="so-merged"
                                aria-expanded={openMerge === id}
                                aria-describedby={`${uid}-m-${id}`}
                                onClick={() =>
                                  setOpenMerge(openMerge === id ? null : id)
                                }
                                onKeyDown={(ev) =>
                                  ev.key === "Escape" && setOpenMerge(null)
                                }
                                onBlur={() => setOpenMerge(null)}
                              >
                                <GitMerge size={12} aria-hidden /> Merged from{" "}
                                {e.members.length} sources
                              </button>
                              <span
                                role="tooltip"
                                id={`${uid}-m-${id}`}
                                className={`so-pop${openMerge === id ? " is-open" : ""}`}
                              >
                                <span className="so-pop-title">
                                  Starts from{" "}
                                  {datasetLabel.get(e.anchorDatasetId)?.label}{" "}
                                  {datasetLabel.get(e.anchorDatasetId)?.table}{" "}
                                  and adds the matching records from each
                                  source.
                                </span>
                                {e.members.map((m) => {
                                  const info = datasetLabel.get(m.datasetId)!;
                                  return (
                                    <span
                                      className="so-pop-row"
                                      key={m.datasetId}
                                    >
                                      <SourceLogo source={info.source} />
                                      <span>
                                        {info.label} {info.table}
                                        {m.via && (
                                          <small>
                                            {" "}
                                            on {m.via.column} ={" "}
                                            {m.via.targetColumn}
                                          </small>
                                        )}
                                      </span>
                                      <b>
                                        {m.via
                                          ? `${number(m.matched)} of ${number(m.rows)} matched`
                                          : `${number(m.rows)} rows · base`}
                                      </b>
                                    </span>
                                  );
                                })}
                              </span>
                            </span>
                          )}
                          {e.kind === "activity" && (
                            <span className="so-kind">Activity stream</span>
                          )}
                        </div>
                      )}
                      <ul aria-label={`${name} sources`}>
                        {e.members.map((m) => {
                          const info = datasetLabel.get(m.datasetId)!;
                          return (
                            <li key={m.datasetId}>
                              <SourceLogo source={info.source} />
                              <span className="so-table">{info.table}</span>
                              <span className="so-source">{info.label}</span>
                            </li>
                          );
                        })}
                      </ul>
                    </div>
                    {outgoing.length > 0 && (
                      <ul
                        className="so-node-links"
                        aria-label={`${name} links`}
                      >
                        {outgoing.map((l) => (
                          <li key={l.id}>
                            <ArrowRight size={12} aria-hidden />
                            <span>
                              {sentence(l.name)} <strong>{nameOf(l.to)}</strong>
                              <small>{matchText(l)}</small>
                            </span>
                          </li>
                        ))}
                      </ul>
                    )}
                  </li>
                );
              })}
            </ul>
            {layout && (
              <div
                className={`so-pills${active ? " has-hot" : ""}`}
                role="group"
                aria-label="Connections between entities"
              >
                {layout.edges.map((e) => {
                  const l = e.link,
                    from = nameOf(l.from),
                    to = nameOf(l.to);
                  return (
                    <button
                      type="button"
                      key={l.id}
                      className={`so-pill${visible(l) ? "" : " is-hidden"}${isHot(l) ? " is-hot" : ""}`}
                      style={
                        {
                          left: `${e.mid[0] / 10}%`,
                          top: e.mid[1],
                          "--d": `${560 + e.reveal * 70}ms`,
                        } as CSSProperties
                      }
                      aria-label={`${from} ${l.name.charAt(0).toLowerCase()}${l.name.slice(1)} ${to}`}
                      aria-describedby={`${uid}-l-${l.id}`}
                      aria-pressed={pinned === l.id}
                      tabIndex={visible(l) ? 0 : -1}
                      onClick={() => setPinned(pinned === l.id ? null : l.id)}
                      onMouseEnter={() => setHover(`l:${l.id}`)}
                      onMouseLeave={() => setHover(null)}
                      onFocus={() => setHover(`l:${l.id}`)}
                      onBlur={() => {
                        setHover(null);
                        setPinned(null);
                      }}
                    >
                      {sentence(l.name)}
                      <span
                        className="so-tip"
                        role="tooltip"
                        id={`${uid}-l-${l.id}`}
                      >
                        <strong>
                          {from} → {to}
                        </strong>{" "}
                        <span>
                          {number(l.matchedRows)} of {number(l.totalRows)}{" "}
                          matched
                          {l.caseInsensitive && (
                            <em> · matched ignoring case</em>
                          )}
                        </span>{" "}
                        <code>
                          {tableOf(l.fromDatasetId)}.{l.fromColumn} →{" "}
                          {tableOf(l.toDatasetId)}.{l.toColumn}
                        </code>{" "}
                        <small>{evidenceText[l.evidence]}</small>
                      </span>
                    </button>
                  );
                })}
              </div>
            )}
          </div>
          <div className="so-legend">
            <span>
              {readOnly
                ? "Open a type to see its properties and records."
                : "Click a name to rename it. Switch off anything that doesn’t belong."}
            </span>
            <span className="so-keys" aria-hidden>
              <span>
                <i className="so-key-line" /> Link
              </span>
              <span>
                <i className="so-key-line is-dashed" /> Activity
              </span>
              <span>
                <i className="so-key-merge" /> Merged across sources
              </span>
            </span>
          </div>
        </section>
      )}

      {(merged.length > 0 || checks.length > 0 || unplaced.length > 0) && (
        <div className="so-notes">
          {merged.length > 0 && (
            <section aria-labelledby={`${uid}-merged`}>
              <h2 id={`${uid}-merged`}>What we merged</h2>
              <ul>
                {merged.map((n, i) => (
                  <li key={i}>
                    <GitMerge size={14} aria-hidden className="is-merged" />
                    {n.text}
                  </li>
                ))}
              </ul>
            </section>
          )}
          {(checks.length > 0 || unplaced.length > 0) && (
            <section aria-labelledby={`${uid}-checks`}>
              <h2 id={`${uid}-checks`}>Worth checking</h2>
              <ul>
                {checks.map((n, i) => {
                  const Icon = noteIcons[n.kind];
                  return (
                    <li key={i}>
                      <Icon size={14} aria-hidden />
                      {n.text}
                    </li>
                  );
                })}
                {unplaced.length > 0 && (
                  <li>
                    <Database size={14} aria-hidden />
                    {unplaced.map(tableOf).join(", ")}{" "}
                    {unplaced.length === 1 ? "isn’t" : "aren’t"} part of the
                    schema yet. The data stays available as{" "}
                    {unplaced.length === 1 ? "a table" : "tables"}.
                  </li>
                )}
              </ul>
            </section>
          )}
        </div>
      )}

      {onBack && !readOnly && (
        <button
          type="button"
          className="text-button so-back"
          onClick={onBack}
          disabled={busy}
        >
          ← {backLabel}
        </button>
      )}
      {!readOnly && (
        <footer className="so-confirm">
          <div>
            <h2>Does this look right?</h2>
            <p>
              {keyless
                ? "No table has a column that identifies its rows, so Home will show metrics from your tables."
                : records
                  ? // Activity streams are not built as records; they feed metrics.
                    `We’ll build your workspace around ${plural(records, "record type")}.${streams.length ? ` ${sentence(streams.map((e) => many(nameOf(e.id).toLowerCase())).join(", "))} stay available for metrics.` : ""}`
                  : "Include at least one record type to continue."}
            </p>
          </div>
          <div className="so-actions">
            {onBack && (
              <button
                type="button"
                className="button"
                onClick={onBack}
                disabled={busy}
              >
                <ArrowLeft size={15} aria-hidden /> Back
              </button>
            )}
            <button
              type="button"
              className="button primary"
              disabled={(!records && !keyless) || busy}
              aria-busy={busy}
              onClick={confirm}
            >
              {busy && (
                <LoaderCircle size={15} className="so-spin" aria-hidden />
              )}
              {confirmLabel}
              {!busy && <ArrowRight size={15} aria-hidden />}
            </button>
          </div>
          {error && (
            <div className="error-box so-error" role="alert">
              {error}
            </div>
          )}
        </footer>
      )}
    </div>
  );
}
