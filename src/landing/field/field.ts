// The field: one set of particles behind the whole page that scrolling moves
// through three formations, each pinned to the part of the page it belongs to.
//   0. hero      a turning graph of tiny nodes and hairline links
//   1. schema    the dotted ground behind the ontology schema: the graph's
//                nodes leave it for their dots as soon as scrolling starts
//   2. explorer  some dots take their type's colour and drop into the
//                Explorer as its records, and stay there; the rest become
//                its dotted ground
// It also fills the box after Fig. 2 green as you reach it, and draws the
// hero's graph again in the footer: pinned there, barely moving, never
// reached by the particles.
// Positions are a function of scroll and time, so nothing accumulates and a
// resize simply rebuilds.
import { heroGraph, mulberry32, type HeroGraph } from "./graph";
import {
  hueRgb,
  layout,
  links,
  neighbours,
  radius,
  records,
  tour,
  typeIndex,
} from "./records";
import { demo } from "./demo";

export type Anchor =
  | "hero"
  | "schema"
  | "explorer"
  | "analyst"
  | "shout"
  | "cloud"
  | "floor";

const TAU = Math.PI * 2;
// The schema's dots are the app's: 20px apart, centred in each cell.
const GRID = 20;
const MAX_PARTICLES = 2800;
const EXPLORER_DOT = 26;
// Camera distance for the graph at rest, in units of its radius.
const D0 = 3.4;
// The hero's camera sits just outside the graph, so it fills the screen and
// its near side passes in front of the page.
const HERO_NEAR = 1.85;
const WHITE: Rgb = [228, 236, 246];
const DOT: Rgb = [255, 255, 255];
// Demo pacing: how long each record stays open.
const STEP = 3800;

type Rgb = [number, number, number];
type Box = { left: number; top: number; width: number; height: number };
type Buffer = {
  x: Float32Array;
  y: Float32Array;
  s: Float32Array;
  a: Float32Array;
  r: Float32Array;
  g: Float32Array;
  b: Float32Array;
};
type Camera = {
  cx: number;
  cy: number;
  R: number;
  d: number;
  turn: number;
  tilt: number;
};

const clamp = (v: number, lo = 0, hi = 1) => (v < lo ? lo : v > hi ? hi : v);
const lerp = (a: number, b: number, t: number) => a + (b - a) * t;
const smooth = (a: number, b: number, v: number) => {
  const t = clamp((v - a) / (b - a));
  return t * t * (3 - 2 * t);
};
const inOut = (u: number) =>
  u < 0.5 ? 4 * u * u * u : 1 - Math.pow(-2 * u + 2, 3) / 2;
const out = (u: number) => 1 - Math.pow(1 - u, 3);
// Moves with the scroll, slowly at first and faster the further you go: the
// distance covered per pixel of scroll grows steadily until the node lands.
const glide = (u: number) => u * u;
const buffer = (n: number): Buffer => ({
  x: new Float32Array(n),
  y: new Float32Array(n),
  s: new Float32Array(n),
  a: new Float32Array(n),
  r: new Float32Array(n),
  g: new Float32Array(n),
  b: new Float32Array(n),
});

const anchors: Partial<Record<Anchor, HTMLElement>> = {};
const liveTargets: Partial<Record<"schema" | "explorer", HTMLElement>> = {};
let requestBuild = () => {};

// Sections register the element a formation is pinned to; `live` gets the
// class is-live once that formation's dots have arrived.
export function attach(
  name: Anchor,
  el: HTMLElement | null,
  live?: HTMLElement | null,
) {
  if (el) anchors[name] = el;
  else delete anchors[name];
  if (live && (name === "schema" || name === "explorer"))
    liveTargets[name] = live;
  requestBuild();
}

export function start(canvas: HTMLCanvasElement) {
  // The drawing helpers draw on `ctx`: the fixed canvas, or the Explorer's own
  // while its settled records are drawn there.
  const page = canvas.getContext("2d")!;
  let ctx = page;
  const calm = matchMedia("(prefers-reduced-motion: reduce)").matches;
  const L = layout();

  let W = 0,
    H = 0,
    dpr = 1,
    n = 0;
  let graph: HeroGraph;
  let gridX = new Float32Array(0),
    gridY = new Float32Array(0);
  let gridH = 0;
  // Which record a particle carries (-1 for none), and the reverse.
  let role = new Int16Array(0);
  const carrier = new Int32Array(records.length);
  // Explorer ground offsets, NaN where a particle has no dot there.
  let bgX = new Float32Array(0),
    bgY = new Float32Array(0);
  let rA = new Float32Array(0),
    rB = new Float32Array(0),
    curl = new Float32Array(0);
  let delayGrid = new Float32Array(0),
    delayExplorer = new Float32Array(0);
  const lit = new Float32Array(records.length).fill(1);
  // G holds the footer's copy of the hero's graph, drawn on the footer's own
  // canvas so it moves with the page however the page moves.
  let footer: Surface | null = null,
    footLift = 0,
    footMiddle = 0;
  // The Explorer's own canvas, the size of its stage. Once the records have
  // landed they are drawn there, in the stage's coordinates, so they move
  // with it however the page scrolls, rather than a frame behind it.
  let pin: Surface | null = null,
    pinDrawn = false;
  let G = buffer(0),
    F = buffer(0),
    T = buffer(0),
    P = buffer(0);

  function build() {
    const box = canvas.getBoundingClientRect();
    W = Math.max(1, Math.round(box.width));
    H = Math.max(1, Math.round(box.height));
    dpr = Math.min(window.devicePixelRatio || 1, 2);
    canvas.width = Math.round(W * dpr);
    canvas.height = Math.round(H * dpr);

    // One particle per schema dot, so the dots are exactly the schema's.
    const stage = anchors.schema?.getBoundingClientRect();
    let spacing = GRID;
    let cols = 0,
      rows = 0;
    if (stage && stage.width > 40 && stage.height > 40) {
      const room = (w: number, h: number, s: number) =>
        Math.floor((w - 2) / s) * Math.floor((h - 2) / s);
      while (room(stage.width, stage.height, spacing) > MAX_PARTICLES)
        spacing += 2;
      cols = Math.floor((stage.width - 2) / spacing);
      rows = Math.floor((stage.height - 2) / spacing);
      gridH = stage.height;
    } else {
      cols = Math.max(8, Math.floor(W / 28));
      rows = Math.max(8, Math.floor(H / 28));
      gridH = rows * spacing;
    }
    n = Math.max(cols * rows, records.length * 2);
    graph = heroGraph(n);
    const rnd = mulberry32(97);
    rA = Float32Array.from({ length: n }, rnd);
    rB = Float32Array.from({ length: n }, rnd);
    curl = Float32Array.from({ length: n }, () => (rnd() - 0.5) * 0.5);

    // Grid points go to particles in order of their height in the graph, so
    // the top of the graph settles into the top rows.
    const byHeight = [...Array(n).keys()].sort(
      (a, b) => graph.pos[a * 3 + 1] - graph.pos[b * 3 + 1],
    );
    const points = [...Array(cols * rows).keys()];
    gridX = new Float32Array(n);
    gridY = new Float32Array(n);
    byHeight.forEach((p, k) => {
      const g = points[Math.min(k, points.length - 1)] ?? 0;
      gridX[p] = 1 + spacing / 2 + (g % cols) * spacing;
      gridY[p] = 1 + spacing / 2 + Math.floor(g / cols) * spacing;
    });
    delayGrid = Float32Array.from(
      { length: n },
      (_, i) => 0.5 * rA[i] + 0.5 * (gridY[i] / Math.max(1, gridH)),
    );

    // Records go to dots spread across the schema, at least two cells apart;
    // left to right in both, so they drop rather than cross.
    role = new Int16Array(n).fill(-1);
    const taken = new Set<string>();
    const chosen: number[] = [];
    const order = [...Array(n).keys()].sort((a, b) => rA[a] - rA[b]);
    for (const gap of [2.5, 1.5, 0]) {
      for (const p of order) {
        if (chosen.length >= records.length) break;
        if (role[p] === -2) continue;
        const cx = Math.round(gridX[p] / (spacing * gap || 1)),
          cy = Math.round(gridY[p] / (spacing * gap || 1));
        const key = `${gap}:${cx}:${cy}`;
        if (gap && taken.has(key)) continue;
        taken.add(key);
        role[p] = -2;
        chosen.push(p);
      }
    }
    chosen.sort((a, b) => gridX[a] - gridX[b]);
    const byX = [...records.keys()].sort((a, b) => L.x[a] - L.x[b]);
    chosen.forEach((p, k) => {
      role[p] = byX[k];
      carrier[byX[k]] = p;
    });
    for (let i = 0; i < n; i++) if (role[i] === -2) role[i] = -1;

    // The Explorer's ground: a sparser grid, filled by the remaining dots row
    // by row, so the schema's sheet of dots slides down into it.
    const ex = anchors.explorer?.getBoundingClientRect();
    bgX = new Float32Array(n).fill(NaN);
    bgY = new Float32Array(n).fill(NaN);
    if (ex) {
      const ec = Math.floor(ex.width / EXPLORER_DOT),
        er = Math.floor(ex.height / EXPLORER_DOT);
      const rest = [...Array(n).keys()]
        .filter((i) => role[i] < 0)
        .sort((a, b) => gridY[a] - gridY[b] || gridX[a] - gridX[b]);
      const slots = ec * er;
      const stride = rest.length / Math.max(1, slots);
      for (let s = 0; s < slots && s * stride < rest.length; s++) {
        const p = rest[Math.floor(s * stride)];
        bgX[p] =
          (ex.width - ec * EXPLORER_DOT) / 2 +
          EXPLORER_DOT / 2 +
          (s % ec) * EXPLORER_DOT;
        bgY[p] =
          (ex.height - er * EXPLORER_DOT) / 2 +
          EXPLORER_DOT / 2 +
          Math.floor(s / ec) * EXPLORER_DOT;
      }
    }
    delayExplorer = Float32Array.from({ length: n }, (_, i) =>
      role[i] >= 0
        ? 0.35 * (byX.indexOf(role[i]) / records.length) + 0.4 * rA[i]
        : 0.75 * rB[i],
    );
    G = buffer(n);
    const foot = anchors.floor?.querySelector("canvas");
    if (foot) {
      // The footer's canvas reaches up behind the closing section and a
      // little above it, so the graph lies under the section, which sits on
      // top of it, and under the footer.
      const cloud = anchors.cloud?.getBoundingClientRect(),
        base = anchors.floor!.getBoundingClientRect(),
        above = Math.round(H * 0.2);
      footLift = cloud
        ? Math.max(0, Math.round(base.top - cloud.top) + above)
        : 0;
      footMiddle = cloud ? above + cloud.height * 0.55 : 0;
      foot.style.top = `${-footLift}px`;
      foot.style.height = `${Math.round(base.height) + footLift}px`;
      const r = foot.getBoundingClientRect();
      footer = {
        c: foot.getContext("2d")!,
        w: Math.max(1, Math.round(r.width)),
        h: Math.max(1, Math.round(r.height)),
      };
      foot.width = Math.round(footer.w * dpr);
      foot.height = Math.round(footer.h * dpr);
    } else footer = null;
    const own = anchors.explorer?.querySelector("canvas");
    if (own) {
      const r = own.getBoundingClientRect();
      pin = {
        c: own.getContext("2d")!,
        w: Math.max(1, Math.round(r.width)),
        h: Math.max(1, Math.round(r.height)),
      };
      own.width = Math.round(pin.w * dpr);
      own.height = Math.round(pin.h * dpr);
      pinDrawn = false;
    } else pin = null;
    F = buffer(n);
    T = buffer(n);
    P = buffer(n);
  }

  // ----- formations -------------------------------------------------------

  // `spread` pushes the graph apart from its middle, across, before it is
  // turned: each node moves out by up to that much, the most for those near
  // the middle, so the graph gathers into two clusters. `side` records where
  // each node went, from -1 to 1.
  const side = { at: new Float32Array(0) };
  function hero(buf: Buffer, cam: Camera, spread = 0) {
    const { pos, weight } = graph;
    const ca = Math.cos(cam.turn),
      sa = Math.sin(cam.turn),
      ct = Math.cos(cam.tilt),
      st = Math.sin(cam.tilt);
    if (spread && side.at.length !== n) side.at = new Float32Array(n);
    for (let i = 0; i < n; i++) {
      let x3 = pos[i * 3];
      const y3 = pos[i * 3 + 1],
        z3 = pos[i * 3 + 2];
      if (spread) {
        side.at[i] = Math.tanh(x3 / 0.22);
        x3 += spread * side.at[i];
      }
      const xr = x3 * ca + z3 * sa,
        zr = -x3 * sa + z3 * ca;
      const yr = y3 * ct - zr * st,
        zz = y3 * st + zr * ct;
      const gap = cam.d - zz;
      const persp = D0 / Math.max(gap, 0.24);
      buf.x[i] = cam.cx + xr * cam.R * persp;
      buf.y[i] = cam.cy + yr * cam.R * persp;
      const depth = clamp((zz + 1) / 2);
      const w = weight[i];
      // Nodes fade as the camera reaches them, so a fly-through stays fine.
      buf.a[i] =
        (0.28 + 0.72 * depth) * (0.58 + 0.42 * w) * smooth(0.26, 0.8, gap);
      buf.s[i] = (0.95 + 1.25 * w) * Math.min(2.4, Math.sqrt(persp));
      buf.r[i] = WHITE[0];
      buf.g[i] = WHITE[1];
      buf.b[i] = WHITE[2];
    }
  }

  function grid(buf: Buffer, box: Box) {
    for (let i = 0; i < n; i++) {
      buf.x[i] = box.left + gridX[i];
      buf.y[i] = box.top + gridY[i];
      buf.s[i] = 1.7;
      buf.a[i] = 0.2;
      buf.r[i] = DOT[0];
      buf.g[i] = DOT[1];
      buf.b[i] = DOT[2];
    }
  }

  // Records take their type's colour where they stand.
  function colour(buf: Buffer, c: number) {
    for (let k = 0; k < records.length; k++) {
      const i = carrier[k],
        h = hueRgb[typeIndex[k]];
      buf.r[i] = lerp(buf.r[i], h[0], c);
      buf.g[i] = lerp(buf.g[i], h[1], c);
      buf.b[i] = lerp(buf.b[i], h[2], c);
      buf.s[i] = lerp(buf.s[i], 3.6, c);
      buf.a[i] = lerp(buf.a[i], 0.95, c);
    }
  }

  const view = { x: 0, y: 0, k: 1, ready: false };
  function region(box: Box) {
    // The open record's panel covers the right of the stage, or its foot on phones.
    const wide = box.width > 720;
    return {
      w: wide ? box.width - 324 : box.width,
      h: wide ? box.height : box.height * 0.55,
    };
  }
  function explorer(buf: Buffer, box: Box, time: number) {
    for (let i = 0; i < n; i++) {
      const k = role[i];
      if (k >= 0) {
        const wob = calm ? 0 : time * 0.0005 * (0.6 + rA[i]);
        const lx = L.x[k] + Math.sin(wob + rA[i] * TAU) * 1.6,
          ly = L.y[k] + Math.cos(wob * 0.8 + rB[i] * TAU) * 1.6;
        // Solid squares; records away from the open one are darkened toward
        // the ground rather than faded, so no link shows through them.
        const h = hueRgb[typeIndex[k]],
          dark = 0.68 * (1 - lit[k]);
        buf.x[i] = box.left + view.x + lx * view.k;
        buf.y[i] = box.top + view.y + ly * view.k;
        buf.s[i] = 1.3 * radius[k] * Math.min(1.25, view.k);
        buf.a[i] = 1;
        buf.r[i] = lerp(h[0], 14, dark);
        buf.g[i] = lerp(h[1], 14, dark);
        buf.b[i] = lerp(h[2], 14, dark);
      } else {
        const ground = !Number.isNaN(bgX[i]);
        buf.x[i] = box.left + (ground ? bgX[i] : rA[i] * box.width);
        buf.y[i] = box.top + (ground ? bgY[i] : rB[i] * box.height);
        buf.s[i] = 1.5;
        buf.a[i] = ground ? 0.09 : 0;
        buf.r[i] = DOT[0];
        buf.g[i] = DOT[1];
        buf.b[i] = DOT[2];
      }
    }
  }

  // Each particle starts when its delay comes up, and bows a little on the
  // way (`swing` scales the bow). With `land`, they all start together and
  // the delay sets when each one arrives instead.
  function morph(
    from: Buffer,
    to: Buffer,
    p: number,
    delay: (i: number) => number,
    spread = 0.5,
    ease = inOut,
    land = false,
    swing = 1,
  ) {
    for (let i = 0; i < n; i++) {
      const u = land
        ? clamp(p / (1 - spread + spread * delay(i)))
        : clamp((p - delay(i) * spread) / (1 - spread));
      let e = ease(u),
        fade = 1;
      if (calm) {
        e = u < 0.5 ? 0 : 1;
        fade = Math.abs(1 - 2 * u);
      }
      const dx = to.x[i] - from.x[i],
        dy = to.y[i] - from.y[i],
        bow = Math.sin(Math.PI * e) * curl[i] * swing;
      P.x[i] = from.x[i] + dx * e - dy * bow;
      P.y[i] = from.y[i] + dy * e + dx * bow;
      P.s[i] = lerp(from.s[i], to.s[i], e);
      P.a[i] = lerp(from.a[i], to.a[i], e) * fade;
      P.r[i] = lerp(from.r[i], to.r[i], e);
      P.g[i] = lerp(from.g[i], to.g[i], e);
      P.b[i] = lerp(from.b[i], to.b[i], e);
    }
  }
  function copy(from: Buffer) {
    P.x.set(from.x);
    P.y.set(from.y);
    P.s.set(from.s);
    P.a.set(from.a);
    P.r.set(from.r);
    P.g.set(from.g);
    P.b.set(from.b);
  }

  // ----- drawing ----------------------------------------------------------

  const glowOf = ([r, g, b]: Rgb) => {
    const c = document.createElement("canvas");
    c.width = c.height = 64;
    const x = c.getContext("2d")!;
    const grad = x.createRadialGradient(32, 32, 0, 32, 32, 32);
    grad.addColorStop(0, `rgba(${r},${g},${b},0.55)`);
    grad.addColorStop(0.35, `rgba(${r},${g},${b},0.18)`);
    grad.addColorStop(1, `rgba(${r},${g},${b},0)`);
    x.fillStyle = grad;
    x.fillRect(0, 0, 64, 64);
    return c;
  };
  const glows = hueRgb.map(glowOf);
  const onScreen = (x: number, y: number, m = 40) =>
    x > -m && y > -m && x < W + m && y < H + m;
  // Where the graph helpers draw: the fixed canvas, or the footer's own.
  type Surface = { c: CanvasRenderingContext2D; w: number; h: number };
  const screen = (): Surface => ({ c: page, w: W, h: H });
  const inside = (on: Surface, x: number, y: number, m = 40) =>
    x > -m && y > -m && x < on.w + m && y < on.h + m;

  function drawGraphEdges(
    weight: number,
    buf: Buffer = P,
    on: Surface = screen(),
    keep?: (a: number, b: number) => boolean,
  ) {
    const c = on.c;
    if (weight <= 0.005) return;
    const e = graph.edges,
      levels = 6;
    const paths: Path2D[] = Array.from({ length: levels }, () => new Path2D());
    const used = new Uint8Array(levels);
    for (let j = 0; j < e.length; j += 2) {
      const a = e[j],
        b = e[j + 1];
      if (keep && !keep(a, b)) continue;
      const al = Math.min(buf.a[a], buf.a[b]);
      if (al < 0.02) continue;
      const ax = buf.x[a],
        ay = buf.y[a],
        bx = buf.x[b],
        by = buf.y[b];
      if (!inside(on, ax, ay, 0) && !inside(on, bx, by, 0)) continue;
      const level = Math.min(levels - 1, Math.floor(al * levels));
      paths[level].moveTo(ax, ay);
      paths[level].lineTo(bx, by);
      used[level] = 1;
    }
    // No wider than one device pixel, so the canvas draws the links as
    // hairlines, its fast path, rather than stroking thousands of outlines.
    // Slightly more opaque to keep the same weight on a 2x screen.
    const width = Math.min(0.6, 0.98 / dpr),
      ink = 0.24 * Math.min(1.25, 0.6 / width);
    c.lineWidth = width;
    for (let l = 0; l < levels; l++) {
      if (!used[l]) continue;
      c.strokeStyle = `rgba(205,222,240,${(((l + 0.5) / levels) * ink * weight).toFixed(3)})`;
      c.stroke(paths[l]);
    }
  }

  // Particles are drawn grouped by colour and opacity, so the canvas sets its
  // fill a few dozen times a frame rather than parsing a colour per particle.
  const fills = new Map<number, string>();
  let byFill = new Float64Array(0);
  function drawParticles(buf: Buffer = P, on: Surface = screen()) {
    const c = on.c;
    if (byFill.length !== n) byFill = new Float64Array(n);
    let m = 0;
    for (let i = 0; i < n; i++) {
      const a = buf.a[i];
      if (a < 0.008 || !inside(on, buf.x[i], buf.y[i])) continue;
      const colour =
        ((buf.r[i] >> 3) << 10) | ((buf.g[i] >> 3) << 5) | (buf.b[i] >> 3);
      byFill[m++] = (colour * 32 + Math.min(31, Math.round(a * 31))) * 4096 + i;
    }
    const list = byFill.subarray(0, m).sort();
    let current = -1;
    for (let j = 0; j < m; j++) {
      const key = Math.floor(list[j] / 4096),
        i = list[j] - key * 4096;
      if (key !== current) {
        current = key;
        const colour = key >> 5;
        let fill = fills.get(colour);
        if (!fill) {
          const c = (shift: number) => ((colour >> shift) & 31) * 8 + 4;
          fill = `rgb(${c(10)},${c(5)},${c(0)})`;
          fills.set(colour, fill);
        }
        c.fillStyle = fill;
        c.globalAlpha = (key & 31) / 31;
      }
      const x = buf.x[i],
        y = buf.y[i],
        s = buf.s[i];
      c.fillRect(x - s / 2, y - s / 2, s, s);
    }
    c.globalAlpha = 1;
  }

  function drawGlows(weight: number) {
    if (weight <= 0.01) return;
    for (let k = 0; k < records.length; k++) {
      const i = carrier[k],
        s = P.s[i] * 2.8,
        a = P.a[i] * weight * 0.55 * (0.2 + 0.8 * lit[k]);
      if (a < 0.01 || !onScreen(P.x[i], P.y[i])) continue;
      ctx.globalAlpha = a;
      ctx.drawImage(glows[typeIndex[k]], P.x[i] - s / 2, P.y[i] - s / 2, s, s);
    }
    ctx.globalAlpha = 1;
  }

  // Links drawn as Fig. 1's connectors (vendor/web/ontology-scene.css):
  // faint dashes between records, stopping short of each square. The open
  // record's links turn white and march, like the lines up to the apps, and a
  // short white packet runs out along each of them.
  function drawRecordLinks(weight: number, now: number) {
    if (weight <= 0.01) return;
    const sel = demo.selected;
    // Where a line from a record's middle along (ux, uy) leaves its square.
    const edge = (i: number, ux: number, uy: number) =>
      P.s[i] / 2 / Math.max(Math.abs(ux), Math.abs(uy), 0.001) + 2.5;
    // The two ends of the line from a to b, between the squares.
    const ends = (a: number, b: number) => {
      const dx = P.x[b] - P.x[a],
        dy = P.y[b] - P.y[a],
        len = Math.hypot(dx, dy) || 1,
        ux = dx / len,
        uy = dy / len,
        from = edge(a, ux, uy),
        to = edge(b, ux, uy);
      return {
        x0: P.x[a] + ux * from,
        y0: P.y[a] + uy * from,
        x1: P.x[b] - ux * to,
        y1: P.y[b] - uy * to,
        len: len - from - to,
      };
    };
    const march = calm ? 0 : -((now / 900) * 12) % 12;
    for (const [f, t] of links) {
      const a = carrier[f],
        b = carrier[t];
      const on = f === sel || t === sel;
      const al =
        Math.min(P.a[a], P.a[b]) *
        weight *
        (on ? 1 : 0.33 * (0.25 + 0.75 * Math.min(lit[f], lit[t])));
      if (al < 0.01) continue;
      const { x0, y0, x1, y1, len } = ends(a, b);
      if (len < 4) continue;
      ctx.setLineDash(on ? [2, 4] : [3, 3]);
      ctx.lineDashOffset = on ? march : 0;
      ctx.strokeStyle = `rgba(255,255,255,${al.toFixed(3)})`;
      ctx.lineWidth = on ? 1.2 : 1;
      ctx.beginPath();
      ctx.moveTo(x0, y0);
      ctx.lineTo(x1, y1);
      ctx.stroke();
    }
    ctx.setLineDash([]);
    ctx.lineDashOffset = 0;
    // A packet runs out from the open record along each of its links, eased
    // and staggered as the scene's are.
    if (sel < 0 || calm) return;
    const from = carrier[sel];
    ctx.strokeStyle = "#fff";
    ctx.lineWidth = 2.4;
    ctx.lineCap = "round";
    neighbours[sel].forEach((nb, j) => {
      const { x0, y0, x1, y1, len } = ends(from, carrier[nb.index]);
      if (len < 10) return;
      const u = inOut(((now - selectedAt) / 2600 + j * 0.14) % 1),
        v = Math.min(1, u + 5 / len);
      ctx.globalAlpha = weight;
      ctx.beginPath();
      ctx.moveTo(lerp(x0, x1, u), lerp(y0, y1, u));
      ctx.lineTo(lerp(x0, x1, v), lerp(y0, y1, v));
      ctx.stroke();
    });
    ctx.lineCap = "butt";
    ctx.globalAlpha = 1;
  }

  // Tactical corners round the open record: four brackets just off its
  // square, which close in on it from wider when it opens.
  function drawSelection(weight: number, now: number) {
    const sel = demo.selected;
    if (sel < 0 || weight <= 0.01) return;
    const i = carrier[sel],
      x = P.x[i],
      y = P.y[i],
      t = calm ? 1 : clamp((now - selectedAt) / 600),
      half = P.s[i] / 2 + 5 + 16 * (1 - out(t)),
      arm = Math.max(4, Math.min(8, (P.s[i] / 2 + 5) * 0.55));
    ctx.globalAlpha = weight * (0.35 + 0.65 * t);
    ctx.strokeStyle = "#ffffff";
    ctx.lineWidth = 1;
    ctx.lineCap = "square";
    ctx.beginPath();
    for (const [sx, sy] of [
      [-1, -1],
      [1, -1],
      [1, 1],
      [-1, 1],
    ]) {
      const cx = x + sx * half,
        cy = y + sy * half;
      ctx.moveTo(cx, cy - sy * arm);
      ctx.lineTo(cx, cy);
      ctx.lineTo(cx - sx * arm, cy);
    }
    ctx.stroke();
    ctx.lineCap = "butt";
    ctx.globalAlpha = 1;
  }

  // Labels as the app draws them: the open record, then its links, with a
  // halo in the ground colour, skipping any that would overlap.
  function drawLabels(weight: number) {
    if (weight <= 0.05) return;
    const sel = demo.selected;
    let list: number[];
    if (sel >= 0)
      list = [
        sel,
        ...neighbours[sel]
          .map((nb) => nb.index)
          .sort((a, b) => records[b].degree - records[a].degree)
          .slice(0, 12),
      ];
    else
      list = [...records.keys()]
        .sort((a, b) => records[b].degree - records[a].degree)
        .slice(0, 5);
    const placed: number[][] = [];
    ctx.textBaseline = "middle";
    ctx.lineJoin = "round";
    for (const k of list) {
      const i = carrier[k];
      const text =
        records[k].label.length > 30
          ? records[k].label.slice(0, 29) + "…"
          : records[k].label;
      const main = k === sel;
      ctx.font = `${main ? 600 : 500} ${main ? 12.5 : 11.5}px "Geist", -apple-system, sans-serif`;
      const lx = P.x[i] + P.s[i] / 2 + (main ? 14 : 5),
        ly = P.y[i];
      const box = [
        lx - 2,
        ly - 8,
        lx + ctx.measureText(text).width + 2,
        ly + 8,
      ];
      if (
        placed.some(
          (p) =>
            box[0] < p[2] && box[2] > p[0] && box[1] < p[3] && box[3] > p[1],
        )
      )
        continue;
      placed.push(box);
      ctx.globalAlpha = weight * (main ? 1 : 0.85);
      ctx.lineWidth = 3.5;
      ctx.strokeStyle = "#0e0e0e";
      ctx.strokeText(text, lx, ly);
      ctx.fillStyle = main ? "#ffffff" : "rgba(255,255,255,0.72)";
      ctx.fillText(text, lx, ly);
    }
    ctx.globalAlpha = 1;
  }

  // The box fills from its top right, under Fig. 2's right edge; its text
  // turns dark once the green has reached it. Only writes the style when the
  // values change.
  const filled = { fill: -1, x: -1 };
  function paintFill(shout: Box, fig: Box, fill: number) {
    const el = anchors.shout;
    if (!el) return;
    const x = Math.round(
      Math.min(fig.left + fig.width + 12, shout.left + shout.width - 10) -
        shout.left,
    );
    if (Math.abs(fill - filled.fill) < 0.002 && x === filled.x) return;
    filled.fill = fill;
    filled.x = x;
    el.style.setProperty("--fill", fill.toFixed(3));
    el.style.setProperty("--fx", `${x}px`);
    el.classList.toggle("is-filled", fill > 0.8);
  }

  // ----- the frame --------------------------------------------------------

  // The formations follow the scroll at a speed in proportion to how far
  // behind they are, up to a top speed. So they move the moment a scroll
  // starts, a quick flick still plays a move out rather than jumping to its
  // end, and every move slows as it lands. A jump of more than a screen and a
  // half, such as a link to the foot of the page, skips ahead to the last
  // stretch. The schema's dots taking colour and dropping into the Explorer
  // is easy to follow, so that stretch follows faster, and going back up
  // faster still. LAG is in seconds, TOP_SPEED and SKIP in screens.
  const LAG = { down: 0.28, toExplorer: 0.18, up: 0.12 },
    TOP_SPEED = { down: 1.1, toExplorer: 2.2, up: 3 },
    SKIP = 1.4;
  let smoothY = window.scrollY,
    last = performance.now(),
    clock = 0,
    selectedAt = -1e9;
  const live = { schema: false, explorer: false };
  demo.subscribe(() => (selectedAt = performance.now()));

  const rectOf = (name: Anchor): Box | null => {
    const el = anchors[name];
    if (!el) return null;
    const r = el.getBoundingClientRect();
    return { left: r.left, top: r.top, width: r.width, height: r.height };
  };
  // The scroll position that brings a box to the middle of the screen, or,
  // when it is taller than most of the screen, its top just under the nav.
  const centred = (box: Box | null, sy: number, max: number) =>
    box
      ? Math.min(max, box.top + sy - (H - Math.min(box.height, H * 0.86)) / 2)
      : Infinity;

  function frame(now: number) {
    const dt = Math.min(64, now - last);
    last = now;
    const time = calm ? 0 : now;
    const sy = window.scrollY;
    const max = Math.max(0, document.documentElement.scrollHeight - H);

    const heroBox = rectOf("hero"),
      schemaBox = rectOf("schema"),
      exBox = rectOf("explorer"),
      analystBox = rectOf("analyst"),
      shoutBox = rectOf("shout"),
      floorBox = rectOf("floor");
    const cG = centred(schemaBox, sy, max),
      cE = centred(exBox, sy, max);
    if (calm) smoothY = sy;
    else {
      const way =
          sy < smoothY
            ? "up"
            : smoothY >= cG && smoothY < cE
              ? "toExplorer"
              : "down",
        top = TOP_SPEED[way] * H;
      if (Math.abs(sy - smoothY) > SKIP * H)
        smoothY = sy - Math.sign(sy - smoothY) * SKIP * H;
      smoothY += clamp((sy - smoothY) / LAG[way], -top, top) * (dt / 1000);
      if (Math.abs(sy - smoothY) < 0.4) smoothY = sy;
    }
    const ss = smoothY;
    const span = (a: number, b: number, at = ss) =>
      b - a > 1 ? clamp((at - a) / (b - a)) : at >= b ? 1 : 0;
    // From the hero to the schema the nodes follow the scroll itself, not the
    // follower: they move only as you scroll, and stop when you stop. They
    // land as the schema settles.
    // The later moves end a little before their section is centred, so the
    // follower's lag still lands them about there.
    const pGrid = span(0, cG - H * 0.04, sy),
      pExp = span(cG + H * 0.22, cE - H * 0.1);
    // The box after Fig. 2 fills as it comes up to the middle of the screen.
    const pFill = span(
        centred(analystBox, sy, max) - H * 0.12,
        centred(shoutBox, sy, max) - H * 0.06,
      ),
      fill = calm ? (pFill > 0.5 ? 1 : 0) : smooth(0.5, 1, pFill);

    const wide = W >= 900;
    // Slow, since the near side of the hero's graph is close to the camera.
    const turn = time * 0.00006 + 0.6,
      tilt = 0.36 + 0.05 * Math.sin(time * 0.00009);
    const heroCam = (): Camera => {
      const top = (heroBox?.top ?? 0) + sy,
        h = heroBox?.height ?? H;
      // On wide screens the graph's core sits to the right of the words and
      // its edges reach across the whole page. It stays where it is on screen
      // as you scroll; only its nodes leave it, for the schema's dots.
      return {
        cx: wide ? W * 0.74 : W * 0.5,
        cy: top + h * (wide ? 0.46 : 0.3),
        R: wide ? W * 0.27 : Math.min(W * 0.42, h * 0.3),
        d: HERO_NEAR,
        turn,
        tilt,
      };
    };

    // The Explorer's camera eases toward the open record.
    if (exBox) {
      const { w, h } = region(exBox);
      const fit = Math.min(w / (L.w + 90), h / (L.h + 90));
      const sel = demo.selected;
      // Closer in on the open record, but never so close, or so far over,
      // that a record leaves the stage.
      let k = sel >= 0 && pExp >= 1 ? fit * 1.14 : fit;
      k = Math.min(k, (exBox.width - 36) / L.w, (exBox.height - 36) / L.h);
      const keep = (want: number, size: number, extent: number) => {
        const least = 18 + (extent / 2) * k,
          most = size - 18 - (extent / 2) * k;
        return least > most ? size / 2 : clamp(want, least, most);
      };
      const x = keep(
          sel >= 0 && pExp >= 1 ? w / 2 - L.x[sel] * k : w / 2,
          exBox.width,
          L.w,
        ),
        y = keep(
          sel >= 0 && pExp >= 1 ? h / 2 - L.y[sel] * k : h / 2,
          exBox.height,
          L.h,
        );
      if (!view.ready || calm) Object.assign(view, { x, y, k, ready: true });
      else {
        const f = 1 - Math.exp(-dt / 650);
        view.x += (x - view.x) * f;
        view.y += (y - view.y) * f;
        view.k += (k - view.k) * f;
      }
    }
    const sel = demo.selected;
    const near = new Set(
      sel >= 0 ? [sel, ...neighbours[sel].map((nb) => nb.index)] : [],
    );
    for (let k = 0; k < records.length; k++) {
      const target = sel < 0 || near.has(k) ? 1 : 0;
      lit[k] = calm
        ? target
        : lit[k] + (target - lit[k]) * (1 - Math.exp(-dt / 220));
    }

    // Which move is under way. Until both figures have rendered the page is
    // too short to place the later formations, so the hero holds.
    const ready = !!(schemaBox && exBox);
    let seg = 0;
    if (!ready) seg = 0;
    else if (pExp > 0) seg = 2;
    else if (pGrid > 0) seg = 1;

    let graphEdges = 0,
      recordLinks = 0,
      glow = 0,
      held = 0;
    if (seg === 0) {
      hero(P, heroCam());
      graphEdges = 1;
    } else if (seg === 1) {
      // Every node sets off on the first scroll and speeds up as the schema
      // comes; those bound for the top rows land first, so arrivals taper.
      hero(F, heroCam());
      grid(T, schemaBox!);
      morph(F, T, pGrid, (i) => delayGrid[i], 0.35, glide, true, 0.35);
      // The links stay whole while the nodes are barely moving, then fade.
      graphEdges = 1 - smooth(0.1, 0.45, pGrid);
    }
    // Every record has landed: the Explorer alone, in its stage's coordinates.
    const pinned = seg === 2 && pExp >= 1 && !!pin,
      local: Box | null = pinned
        ? { left: 0, top: 0, width: exBox!.width, height: exBox!.height }
        : null;
    if (pinned) {
      explorer(P, local!, time);
      recordLinks = glow = held = 1;
    } else if (seg === 2) {
      const c = clamp(pExp / 0.26),
        m = clamp((pExp - 0.2) / 0.8);
      grid(F, schemaBox!);
      colour(F, inOut(c));
      if (m <= 0) copy(F);
      else {
        explorer(T, exBox!, time);
        morph(F, T, m, (i) => delayExplorer[i], 0.4);
      }
      recordLinks = smooth(0.82, 1, m);
      glow = smooth(0.5, 1, m);
      held = smooth(0.94, 1, m);
    }

    // The schema's cards rise once its dots are down; the Explorer's panel
    // opens once its records have landed.
    if (ready && !live.schema && (pGrid > 0.8 || calm)) {
      live.schema = true;
      liveTargets.schema?.classList.add("is-live");
    }
    if (ready && !live.explorer && (pExp > 0.96 || calm)) {
      live.explorer = true;
      liveTargets.explorer?.classList.add("is-live");
    }

    // The demo runs while the Explorer is settled and on screen.
    if (exBox && held >= 1 && exBox.top < H && exBox.top + exBox.height > 0) {
      clock += dt;
      demo.select(tour[Math.floor(clock / STEP) % tour.length]);
    }

    // Paint.
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, W, H);
    if (
      footer &&
      floorBox &&
      floorBox.top - footLift < H &&
      floorBox.top + floorBox.height > 0
    ) {
      // The hero's graph again, under the closing section and the footer,
      // drawn in the footer's own coordinates so scroll never moves it. Its
      // nodes are pushed out from the middle, so it gathers in a cluster at
      // each side with a few links across the gap. It sways a little and
      // each node drifts a few pixels; nothing travels.
      // Faced straight on, so the two clusters mirror each other.
      const { c, w, h } = footer;
      hero(
        G,
        {
          cx: w * 0.5,
          cy: footMiddle || h * 0.5,
          R: wide ? Math.min(w * 0.15, h * 0.34) : Math.min(w * 0.2, h * 0.2),
          d: HERO_NEAR,
          turn: calm ? 0 : 0.08 * Math.sin(time * 0.00016),
          tilt: 0.36 + (calm ? 0 : 0.035 * Math.sin(time * 0.00011)),
        },
        0.66,
      );
      for (let i = 0; i < n; i++) {
        if (!calm) {
          const t = time * 0.0007 * (0.6 + rA[i]);
          G.x[i] += Math.sin(t + rA[i] * TAU) * 3;
          G.y[i] += Math.cos(t * 0.8 + rB[i] * TAU) * 3;
        }
        // It fades in toward the top of the canvas, not cut there,
        // and nodes left in the middle fade, so the gap between stays clear.
        G.a[i] *=
          smooth(0, 260, G.y[i]) * smooth(0.45, 0.85, Math.abs(side.at[i]));
      }
      c.setTransform(dpr, 0, 0, dpr, 0, 0);
      c.clearRect(0, 0, w, h);
      // Links within a cluster, and across the gap only between the
      // clusters' main hubs.
      const { weight } = graph;
      drawGraphEdges(
        1,
        G,
        footer,
        (a, b) =>
          Math.abs(side.at[a] - side.at[b]) < 0.6 ||
          (weight[a] === 1 && weight[b] === 1),
      );
      drawParticles(G, footer);
    }
    if (pinned) {
      const { c, w, h } = pin!;
      c.setTransform(dpr, 0, 0, dpr, 0, 0);
      c.clearRect(0, 0, w, h);
      ctx = c;
      drawGlows(glow);
      drawRecordLinks(recordLinks, now);
      drawParticles(P, pin!);
      drawSelection(held, now);
      drawLabels(held);
      ctx = page;
      pinDrawn = true;
    } else {
      if (pin && pinDrawn) {
        pin.c.setTransform(1, 0, 0, 1, 0, 0);
        pin.c.clearRect(0, 0, pin.c.canvas.width, pin.c.canvas.height);
        pinDrawn = false;
      }
      drawGraphEdges(graphEdges);
      drawGlows(glow);
      drawRecordLinks(recordLinks, now);
      drawParticles();
      if (exBox && (held > 0 || recordLinks > 0)) {
        ctx.save();
        ctx.beginPath();
        ctx.rect(exBox.left, exBox.top, exBox.width, exBox.height);
        ctx.clip();
        drawSelection(held, now);
        drawLabels(Math.min(held, recordLinks));
        ctx.restore();
      }
    }
    if (analystBox && shoutBox) {
      paintFill(shoutBox, analystBox, fill);
    }
    raf = requestAnimationFrame(frame);
  }

  // Rebuild, shortly, whenever the window or a pinned element changes size.
  let raf = 0,
    timer = 0;
  const watched = new Set<Element>();
  const observer = new ResizeObserver(() => requestBuild());
  requestBuild = () => {
    for (const el of Object.values(anchors))
      if (el && !watched.has(el)) {
        watched.add(el);
        observer.observe(el);
      }
    clearTimeout(timer);
    timer = window.setTimeout(build, 120);
  };
  build();
  requestBuild();
  window.addEventListener("resize", requestBuild);
  raf = requestAnimationFrame(frame);
  return () => {
    cancelAnimationFrame(raf);
    observer.disconnect();
    window.removeEventListener("resize", requestBuild);
  };
}
