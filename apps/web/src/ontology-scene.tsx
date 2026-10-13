import { useId, type CSSProperties, type ReactNode } from "react";
import { Check, Database } from "lucide-react";
import { number } from "./api";
import "./ontology-scene.css";

export type SceneStage =
  | "reading"
  | "matching"
  | "proposing"
  | "describing"
  | "done";
const stages: SceneStage[] = [
  "reading",
  "matching",
  "proposing",
  "describing",
  "done",
];

const logos: Record<string, string> = {
  planetscale: "planetscale",
  workos: "workos",
  posthog: "posthog",
  stripe: "stripe",
  s3: "s3",
  postgresql: "postgresql",
  postgres: "postgresql",
  neon: "neon",
  supabase: "supabase",
};
const labels: Record<string, string> = {
  planetscale: "PlanetScale",
  workos: "WorkOS",
  posthog: "PostHog",
  stripe: "Stripe",
  s3: "Amazon S3",
  postgresql: "PostgreSQL",
  postgres: "PostgreSQL",
  neon: "Neon",
  supabase: "Supabase",
  mysql: "MySQL",
  neo4j: "Neo4j",
  upload: "Uploads",
  rest: "REST API",
};
export const connectorLogo = (key: string) =>
  logos[key] ? `/connectors/${logos[key]}.svg` : undefined;
export const sourceName = (key: string) =>
  labels[key] || key.charAt(0).toUpperCase() + key.slice(1);

type Pt = [number, number];
const C = Math.cos(Math.PI / 6),
  S = 0.5;
const r1 = (n: number) => Math.round(n * 10) / 10;
// World (u, v) on a horizontal plane centred at c, lifted by z, to screen.
const iso = (c: Pt, u: number, v: number, z = 0): Pt => [
  r1(c[0] + (u - v) * C),
  r1(c[1] + (u + v) * S - z),
];
const path = (ps: Pt[]) => `M${ps.map((p) => p.join(" ")).join("L")}Z`;
const down = (p: Pt, t: number): Pt => [p[0], r1(p[1] + t)];
// Flat 2D drawing mapped onto a floor, a wall facing front-left or one facing front-right.
const floor = (p: Pt) => `matrix(${C} ${S} ${-C} ${S} ${p[0]} ${p[1]})`;
const wallLeft = (p: Pt) => `matrix(${C} ${S} 0 1 ${p[0]} ${p[1]})`;
const wallRight = (p: Pt) => `matrix(${C} ${-S} 0 1 ${p[0]} ${p[1]})`;
const vars = (v: Record<string, number | string>) => v as CSSProperties;
const line = (a: Pt, b: Pt) => `M${a.join(" ")}L${b.join(" ")}`;

function Slab({
  c,
  w,
  t,
  className = "",
}: {
  c: Pt;
  w: number;
  t: number;
  className?: string;
}) {
  const a = iso(c, -w, -w),
    b = iso(c, w, -w),
    k = iso(c, w, w),
    l = iso(c, -w, w);
  return (
    <g className={`os-slab ${className}`}>
      <path className="os-face-l" d={path([l, k, down(k, t), down(l, t)])} />
      <path className="os-face-r" d={path([k, b, down(b, t), down(k, t)])} />
      <path className="os-face-t" d={path([a, b, k, l])} />
    </g>
  );
}

function Plate({
  c,
  w,
  t,
  label,
  caption,
  className = "",
  style,
  children,
}: {
  c: Pt;
  w: number;
  t: number;
  label: string;
  // Where the upright caption's baseline is centred, above or below the plate.
  caption: Pt;
  className?: string;
  style?: CSSProperties;
  children?: ReactNode;
}) {
  const inset = w - Math.max(6, w * 0.06);
  return (
    <g className={`os-plate ${className}`} style={style}>
      <Slab c={c} w={w} t={t} />
      <path
        className="os-rim"
        d={path([
          iso(c, -inset, -inset),
          iso(c, inset, -inset),
          iso(c, inset, inset),
          iso(c, -inset, inset),
        ])}
      />
      {/* Upright, like a caption, so it stays legible as the scene scales down. */}
      <text className="os-plate-label" x={caption[0]} y={caption[1]}>
        {label}
      </text>
      {children}
    </g>
  );
}

// Small iso block standing on a plate, top face centred at p.
function Tile({ p, s, h }: { p: Pt; s: number; h: number }) {
  return <Slab c={[p[0], p[1] - h]} w={s} t={h} className="os-tile" />;
}

function ModelGlyph({ kind }: { kind: number }) {
  // Drawn in floor space around (0, 0) so it lies on a tile top.
  if (kind === 0)
    return (
      <path
        className="os-glyph"
        d="M0 -11C1.6 -3.4 3.4 -1.6 11 0C3.4 1.6 1.6 3.4 0 11C-1.6 3.4 -3.4 1.6 -11 0C-3.4 -1.6 -1.6 -3.4 0 -11Z"
      />
    );
  if (kind === 1)
    return (
      <g className="os-glyph">
        <path d="M-8 -7L0 0L-8 7M0 0L8 -7M0 0L8 7" />
        {[
          [-8, -7],
          [-8, 7],
          [0, 0],
          [8, -7],
          [8, 7],
        ].map(([x, y]) => (
          <circle key={`${x}${y}`} cx={x} cy={y} r={2.4} />
        ))}
      </g>
    );
  if (kind === 2)
    return (
      <g className="os-glyph">
        {[-7, 0, 7].flatMap((x) =>
          [-7, 0, 7].map((y) => (
            <circle key={`${x}${y}`} cx={x} cy={y} r={x === y ? 2.4 : 1.4} />
          )),
        )}
      </g>
    );
  return <path className="os-glyph" d="M-9 -6H9M-9 0H6M-9 6H2" />;
}

type Icon = "cube" | "person" | "stack" | "stream";
const iconFor = (name: string): Icon =>
  /user|person|member|contact|people|employee|requester|owner/i.test(name)
    ? "person"
    : /event|activity|session|log|pageview|click/i.test(name)
      ? "stream"
      : /invoice|payment|order|ticket|transaction|charge|document|file|note/i.test(
            name,
          )
        ? "stack"
        : "cube";

function NodeIcon({ p, icon }: { p: Pt; icon: Icon }) {
  const [x, y] = p;
  if (icon === "person")
    return (
      <g className="os-icon">
        <path
          d={`M${x - 6} ${y}V${y - 7}A6 4 0 0 1 ${x + 6} ${y - 7}V${y}A6 3.4 0 0 1 ${x - 6} ${y}Z`}
        />
        <circle cx={x} cy={y - 15.5} r={4.4} />
      </g>
    );
  if (icon === "stream")
    return (
      <g className="os-icon">
        <path
          d={`M${x - 7} ${y - 11}V${y}A7 3.6 0 0 0 ${x + 7} ${y}V${y - 11}`}
        />
        <ellipse cx={x} cy={y - 11} rx={7} ry={3.6} />
        <path
          className="os-icon-line"
          d={`M${x - 7} ${y - 5.5}A7 3.6 0 0 0 ${x + 7} ${y - 5.5}`}
        />
      </g>
    );
  if (icon === "stack")
    return (
      <g className="os-icon">
        {[0, 5, 10].map((z) => (
          <Slab key={z} c={[x, y - z - 2]} w={5.5} t={2} />
        ))}
      </g>
    );
  return (
    <g className="os-icon">
      <Slab c={[x, y - 10]} w={6} t={10} />
    </g>
  );
}

// Extends above and below the plates for their captions.
const V = { x: 0, y: -24, w: 780, h: 718 };
const onto: Pt = [390, 360],
  ontoW = 165,
  ontoT = 14;
const dataC: Pt = [200, 572],
  modelC: Pt = [580, 572],
  baseW = 76,
  baseT = 11;
// Captions sit above the app screens and under each lower plate's front corner.
const apps: { c: Pt; label: string; caption: Pt }[] = [
  { c: [178, 172], label: "ANALYTICS", caption: [178, 76] },
  { c: [390, 104], label: "WORKFLOWS", caption: [390, 8] },
  { c: [602, 172], label: "INTEGRATIONS", caption: [602, 76] },
];
const appW = 54,
  appT = 9;

// A hub with up to seven entities on a ring. Slots are fixed and filled as
// symmetric subsets, so nodes keep their place when the entity count changes;
// the back and far sides stay clear for the connectors to the apps above.
const hub: Pt = [onto[0], onto[1] - 2];
const ringSlots: Pt[] = Array.from({ length: 7 }, (_, i) => {
  const a = ((-90 + 180 / 7 + (i * 360) / 7) * Math.PI) / 180;
  return [r1(hub[0] + 152 * Math.cos(a)), r1(hub[1] + 80 * Math.sin(a))];
});
const ringSets = [
  [3],
  [1, 5],
  [1, 3, 5],
  [0, 1, 5, 6],
  [0, 1, 3, 5, 6],
  [0, 1, 2, 4, 5, 6],
  [0, 1, 2, 3, 4, 5, 6],
];
const chords: [number, number][] = [
  [0, 1],
  [2, 3],
  [3, 4],
  [5, 6],
];
function nodeLayout(count: number) {
  const ring = ringSets[Math.min(7, Math.max(1, count - 1)) - 1];
  const nodes = [
    { id: "hub", p: hub },
    ...ring.map((r) => ({ id: `r${r}`, p: ringSlots[r] })),
  ];
  const links = [
    ...ring.map((r) => ({ id: `hub-r${r}`, a: hub, b: ringSlots[r] })),
    ...chords
      .filter(([a, b]) => ring.includes(a) && ring.includes(b))
      .map(([a, b]) => ({
        id: `r${a}-r${b}`,
        a: ringSlots[a],
        b: ringSlots[b],
      })),
  ];
  return { nodes, links };
}

const row = (v: number, us: number[]) =>
  us.map((u): [number, number] => [u, v]);
const tileSlots = (n: number) =>
  n <= 3
    ? row(0, [-44, 0, 44]).slice(0, n)
    : n <= 5
      ? [...row(-22, [-44, 0, 44]), ...row(22, [-22, 22])].slice(0, n)
      : [...row(-22, [-44, 0, 44]), ...row(22, [-44, 0, 44])];
const modelSlots: [number, number][] = [
  [-24, -24],
  [24, -24],
  [-24, 22],
  [24, 22],
];

// Vertical-tangent curve between two screen points.
const curve = (a: Pt, b: Pt, bend = 0.55) => {
  const dy = (b[1] - a[1]) * bend;
  return `M${a.join(" ")}C${a[0]} ${r1(a[1] + dy)} ${b[0]} ${r1(b[1] - dy)} ${b.join(" ")}`;
};
// Point on the bottom edge of the ontology plate's front faces at screen x.
const ontoUnderside = (x: number): Pt => [
  r1(x),
  r1(onto[1] + (1 - Math.abs(x - onto[0]) / (2 * ontoW * C)) * ontoW + ontoT),
];
const spread = (n: number, from: number, to: number) =>
  Array.from({ length: n }, (_, i) => from + ((to - from) * i) / (n - 1));

function Flow({ d, i }: { d: string; i: number }) {
  return (
    <g className="os-flow" style={vars({ "--i": i })}>
      <path className="os-flow-line" d={d} />
      <path className="os-flow-packet" d={d} pathLength={100} />
    </g>
  );
}

type Place = "above" | "below" | "left" | "right";
// Labels sit on the side of the node facing away from the hub, so each reads as
// its own node's label rather than a neighbour's.
function placeFor(p: Pt): Place {
  if (p[0] === hub[0] && p[1] === hub[1]) return "above";
  const dx = (p[0] - hub[0]) / 152,
    dy = (p[1] - hub[1]) / 80;
  return dy > 0.3 ? "below" : dy < -0.3 ? "above" : dx > 0 ? "right" : "left";
}

function PillShape({
  at,
  text,
  i,
  skeleton,
  place,
}: {
  at: Pt;
  text: string;
  i: number;
  skeleton?: boolean;
  place: Place;
}) {
  const w = skeleton ? 44 + (i % 3) * 10 : Math.round(text.length * 5.9 + 18),
    h = 17,
    x = r1(
      place === "right" ? at[0] : place === "left" ? at[0] - w : at[0] - w / 2,
    ),
    y = r1(
      place === "above" ? at[1] - h : place === "below" ? at[1] : at[1] - h / 2,
    );
  return (
    <>
      <rect x={x} y={y} width={w} height={h} rx={h / 2} />
      {skeleton ? (
        <rect
          className="os-pill-bar"
          x={x + 9}
          y={y + 7}
          width={w - 18}
          height={3}
          rx={1.5}
        />
      ) : (
        <text x={r1(x + w / 2)} y={y + 11.8}>
          {text}
        </text>
      )}
    </>
  );
}

function Pill({
  node,
  rx,
  text,
  i,
  skeleton,
  replaced,
}: {
  node: Pt;
  rx: number;
  text: string;
  i: number;
  skeleton?: boolean;
  replaced?: boolean;
}) {
  const place = placeFor(node),
    [x, y] = node;
  // The stem runs from the node's edge to where the pill starts.
  const [from, at]: [Pt, Pt] =
    place === "above"
      ? [
          [x, y - 13],
          [x, y - 30],
        ]
      : place === "below"
        ? [
            [x, y + 18],
            [x, y + 27],
          ]
        : place === "right"
          ? [
              [x + rx + 1, y + 1],
              [x + rx + 10, y + 1],
            ]
          : [
              [x - rx - 1, y + 1],
              [x - rx - 10, y + 1],
            ];
  const short = clip(text, 12);
  return (
    <g
      className={`os-pill is-${place}${skeleton ? " is-skeleton" : ""}${replaced ? " is-replaced" : ""}`}
      style={vars({ "--i": i })}
    >
      <path className="os-pill-stem" d={line(from, at)} />
      {/* Phones scale the body up from its stem; a shorter name keeps it clear. */}
      <g className="os-pill-body">
        <g className={short === text ? "" : "os-pill-long"}>
          <PillShape
            at={at}
            text={text}
            i={i}
            skeleton={skeleton}
            place={place}
          />
        </g>
        {short !== text && (
          <g className="os-pill-short">
            <PillShape at={at} text={short} i={i} place={place} />
          </g>
        )}
      </g>
    </g>
  );
}

function clip(s: string, n = 18) {
  return s.length > n ? `${s.slice(0, n - 1).trimEnd()}…` : s;
}

function sceneLabel(stage: SceneStage, sources: string[], entities: string[]) {
  const from = sources.length
    ? `${sources.length} source${sources.length === 1 ? "" : "s"} (${sources.map(sourceName).join(", ")})`
    : "your data";
  const list = entities.join(", ");
  return {
    reading: `AI is reading tables from ${from} into a shared ontology.`,
    matching: `AI is finding keys and matching records across ${from}.`,
    proposing: `AI is proposing business entities${list ? `: ${list}` : ""}.`,
    describing: `AI is writing descriptions for ${list || "the proposed entities"}, ready for analytics, workflows and integrations.`,
    done: `Suggested ontology ready${list ? `: ${list}` : ""}.`,
  }[stage];
}

export function AiWorkingScene({
  sources,
  entities,
  stage,
  label,
}: {
  sources: string[];
  entities: string[];
  stage: SceneStage;
  // Replaces the stage's own description, e.g. while the workspace is built.
  label?: string;
}) {
  const uid = `os${useId().replace(/[^\w-]/g, "")}`;
  const rank = Math.max(0, stages.indexOf(stage));
  const reached = stages
    .slice(1, rank + 1)
    .map((s) => `reached-${s}`)
    .join(" ");
  const keys = [...new Set(sources)];
  const shown = keys.length > 6 ? [...keys.slice(0, 5), "+"] : keys;
  // Tiles sit at the plate centre and are moved by `translate`, so a new
  // source arriving eases the others into their new places.
  const tiles = tileSlots(Math.max(1, shown.length)).map(([u, v], i) => ({
    key: shown[i] ?? "",
    at: iso([0, 0], u, v),
  }));
  const names = entities.slice(0, 8);
  const { nodes, links } = nodeLayout(names.length ? names.length : 6);
  const named = rank >= 2;

  // The base plates' back edges run parallel to the ontology's underside, so
  // the connectors between them are evenly spaced vertical columns.
  const dataIn = spread(7, -baseW + 16, baseW - 16).map((t) => {
    const p = iso(dataC, t, -baseW + 9);
    return line(p, ontoUnderside(p[0]));
  });
  const modelsIn = spread(7, -baseW + 16, baseW - 16).map((t) => {
    const p = iso(modelC, -baseW + 9, t);
    return line(p, ontoUnderside(p[0]));
  });
  // A fanned bundle per app, rising from the ontology's back edges.
  const appsIn = apps.flatMap(({ c }, ai) => {
    const side = ai - 1;
    return [-1.5, -0.5, 0.5, 1.5].map((k) => {
      const start: Pt = side
        ? [onto[0] + side * (218 - k * 12), onto[1] - 17 - k * 7]
        : [onto[0] + k * 12, onto[1] - 150];
      const dx = (side || -1) * -k * 20;
      return curve(
        start,
        [c[0] + dx, r1(c[1] + appW + appT - (Math.abs(dx) * S) / C)],
        0.6,
      );
    });
  });

  return (
    <div className={`ontology-scene is-${stage} ${reached}`}>
      <svg
        viewBox={`${V.x} ${V.y} ${V.w} ${V.h}`}
        role="img"
        aria-label={label || sceneLabel(stage, keys, entities)}
      >
        <defs>
          <linearGradient id={`${uid}-sweep`} x1="0" x2="1" y1="0" y2="0">
            <stop offset="0" className="os-stop" stopOpacity="0" />
            <stop offset="0.5" className="os-stop" stopOpacity="0.5" />
            <stop offset="1" className="os-stop" stopOpacity="0" />
          </linearGradient>
          <clipPath id={`${uid}-clip`}>
            <rect
              x={-ontoW + 8}
              y={-ontoW + 8}
              width={(ontoW - 8) * 2}
              height={(ontoW - 8) * 2}
            />
          </clipPath>
          <radialGradient id={`${uid}-glow`}>
            <stop offset="0" className="os-stop" stopOpacity="0.55" />
            <stop offset="1" className="os-stop" stopOpacity="0" />
          </radialGradient>
          {links.map((l, i) => (
            <mask
              key={l.id}
              id={`${uid}-${l.id}`}
              maskUnits="userSpaceOnUse"
              x={V.x}
              y={V.y}
              width={V.w}
              height={V.h}
            >
              <path
                className="os-draw"
                style={vars({ "--i": i })}
                d={line(l.a, l.b)}
                pathLength={1}
              />
            </mask>
          ))}
        </defs>

        <Plate
          c={dataC}
          w={baseW}
          t={baseT}
          label="DATA"
          caption={[dataC[0], dataC[1] + baseW + baseT + 26]}
          className="os-data"
        >
          {tiles.map((t, i) => (
            <g
              key={t.key || i}
              className="os-tile-wrap"
              style={vars({ "--i": i, translate: `${t.at[0]}px ${t.at[1]}px` })}
            >
              <Tile p={dataC} s={16} h={9} />
              <g transform={floor([dataC[0], dataC[1] - 9])}>
                {t.key === "+" ? (
                  <text className="os-more" x="0" y="3.5">
                    +{keys.length - 5}
                  </text>
                ) : connectorLogo(t.key) ? (
                  <image
                    href={connectorLogo(t.key)}
                    x={-11}
                    y={-11}
                    width={22}
                    height={22}
                  />
                ) : (
                  <g className="os-glyph">
                    <ellipse cx={0} cy={-6} rx={8} ry={3.4} />
                    <path d="M-8 -6V6A8 3.4 0 0 0 8 6V-6M-8 0A8 3.4 0 0 0 8 0" />
                  </g>
                )}
              </g>
            </g>
          ))}
        </Plate>
        <Plate
          c={modelC}
          w={baseW}
          t={baseT}
          label="MODELS"
          caption={[modelC[0], modelC[1] + baseW + baseT + 26]}
          className="os-models"
        >
          {modelSlots.map(([u, v], i) => {
            const p = iso(modelC, u, v);
            return (
              <g
                key={i}
                className="os-tile-wrap os-model"
                style={vars({ "--i": i })}
              >
                <Tile p={p} s={16} h={9} />
                <g transform={floor([p[0], p[1] - 9])}>
                  <ModelGlyph kind={i} />
                </g>
              </g>
            );
          })}
        </Plate>

        <g className="os-flows os-flows-data">
          {dataIn.map((d, i) => (
            <Flow key={i} d={d} i={i} />
          ))}
        </g>
        <g className="os-flows os-flows-models">
          {modelsIn.map((d, i) => (
            <Flow key={i} d={d} i={i} />
          ))}
        </g>

        <Plate
          c={onto}
          w={ontoW}
          t={ontoT}
          label="ONTOLOGY"
          caption={[onto[0], onto[1] + ontoW + ontoT + 17]}
          className="os-onto"
        >
          <g transform={floor(onto)} className="os-grid">
            {[-120, -80, -40, 0, 40, 80, 120].map((n) => (
              <path
                key={n}
                d={`M${n} -152V152M-152 ${n}H152`}
                vectorEffect="non-scaling-stroke"
              />
            ))}
          </g>
          <g transform={floor(onto)}>
            <g clipPath={`url(#${uid}-clip)`}>
              <rect
                className="os-sweep"
                x={-ontoW - 120}
                y={-ontoW}
                width={120}
                height={ontoW * 2}
                fill={`url(#${uid}-sweep)`}
              />
            </g>
          </g>
        </Plate>

        <g className="os-flows os-flows-apps">
          {appsIn.map((d, i) => (
            <Flow key={i} d={d} i={i} />
          ))}
        </g>

        <g className="os-links">
          {links.map((l) => (
            <path key={l.id} mask={`url(#${uid}-${l.id})`} d={line(l.a, l.b)} />
          ))}
        </g>

        <g className="os-nodes">
          {nodes.map(({ id, p }, i) => {
            const name = named ? names[i] : undefined,
              rx = i === 0 ? 25 : 20,
              ry = r1(rx * 0.577),
              h = 6;
            return (
              <g
                key={id}
                className={`os-node${i === 0 ? " is-hub" : ""}`}
                style={vars({ "--i": i })}
              >
                <ellipse
                  className="os-node-glow"
                  cx={p[0]}
                  cy={p[1]}
                  rx={rx * 1.9}
                  ry={ry * 1.9}
                  fill={`url(#${uid}-glow)`}
                />
                <g className="os-node-body">
                  <path
                    className="os-node-side"
                    d={`M${p[0] - rx} ${p[1]}V${p[1] + h}A${rx} ${ry} 0 0 0 ${p[0] + rx} ${p[1] + h}V${p[1]}Z`}
                  />
                  <ellipse
                    className="os-node-top"
                    cx={p[0]}
                    cy={p[1]}
                    rx={rx}
                    ry={ry}
                  />
                  <ellipse
                    className="os-node-ring"
                    cx={p[0]}
                    cy={p[1]}
                    rx={rx - 5}
                    ry={r1((rx - 5) * 0.577)}
                  />
                  <NodeIcon
                    p={[p[0], p[1] + 2]}
                    icon={name ? iconFor(name) : "cube"}
                  />
                </g>
              </g>
            );
          })}
        </g>

        <g className="os-pills">
          {nodes.map(({ id, p }, i) => {
            const name = named ? names[i] : undefined,
              rx = i === 0 ? 25 : 20;
            return (
              <g key={id}>
                <Pill
                  node={p}
                  rx={rx}
                  text=""
                  i={i}
                  skeleton
                  replaced={!!name}
                />
                {name && <Pill node={p} rx={rx} text={clip(name)} i={i} />}
              </g>
            );
          })}
        </g>

        {apps.map((a, i) => (
          <Plate
            key={a.label}
            c={a.c}
            w={appW}
            t={appT}
            label={a.label}
            caption={a.caption}
            className="os-app"
            style={vars({ "--i": i })}
          >
            <AppScreen c={a.c} kind={i} />
          </Plate>
        ))}
      </svg>
    </div>
  );
}

function AppScreen({ c, kind }: { c: Pt; kind: number }) {
  // A small monitor standing towards the back of its plate.
  const w = 70,
    h = 46,
    lift = 10,
    back = -14;
  const right = kind === 2;
  const foot = right ? iso(c, back, 0) : iso(c, 0, back);
  const transform = right
    ? wallRight(iso(c, back, w / 2, lift + h))
    : wallLeft(iso(c, -w / 2, back, lift + h));
  return (
    <g className="os-screen">
      <ellipse
        className="os-screen-foot"
        cx={foot[0]}
        cy={foot[1]}
        rx={9}
        ry={5}
      />
      <path
        className="os-screen-stand"
        d={`M${foot[0]} ${foot[1]}V${r1(foot[1] - lift - 2)}`}
      />
      <g transform={transform}>
        <rect
          className="os-screen-frame"
          x={0}
          y={0}
          width={w}
          height={h}
          rx={2.5}
        />
        <path className="os-screen-bar" d={`M0 8H${w}`} />
        <circle className="os-screen-dot" cx={5} cy={4.2} r={1.3} />
        <circle className="os-screen-dot" cx={9.5} cy={4.2} r={1.3} />
        {kind === 0 && (
          <g>
            {[12, 20, 15, 27, 22, 31].map((bh, i) => (
              <rect
                key={i}
                className="os-screen-fill os-bar"
                style={vars({ "--j": i })}
                x={8 + i * 9.5}
                y={h - 5 - bh}
                width={5.5}
                height={bh}
              />
            ))}
          </g>
        )}
        {kind === 1 && (
          <g>
            <rect
              className="os-screen-fill"
              x={6}
              y={20}
              width={14}
              height={9}
              rx={2}
            />
            <rect
              className="os-screen-box"
              x={28}
              y={14}
              width={14}
              height={9}
              rx={2}
            />
            <rect
              className="os-screen-box"
              x={28}
              y={30}
              width={14}
              height={9}
              rx={2}
            />
            <rect
              className="os-screen-fill"
              x={50}
              y={22}
              width={14}
              height={9}
              rx={2}
            />
            <path
              className="os-screen-line"
              d="M20 24.5H24V18.5H28M24 24.5V34.5H28M42 18.5H46V26.5H50M42 34.5H46V26.5"
            />
          </g>
        )}
        {kind === 2 && (
          <g>
            {[16, 26, 36].map((y, i) => (
              <g key={y}>
                <circle className="os-screen-fill" cx={11} cy={y} r={3.4} />
                <path className="os-screen-line" d={`M17 ${y}H${44 - i * 7}`} />
                <rect
                  className={i === 1 ? "os-screen-fill" : "os-screen-box"}
                  x={50}
                  y={y - 3.5}
                  width={13}
                  height={7}
                  rx={3.5}
                />
              </g>
            ))}
          </g>
        )}
      </g>
    </g>
  );
}

export type FeedItem = {
  id: string;
  name: string;
  source: string;
  rows?: number;
  status: "pending" | "loading" | "ready";
};
export type FeedStep = { label: string; status: "pending" | "active" | "done" };

function SourceMark({ source }: { source: string }) {
  const src = connectorLogo(source);
  return src ? (
    <img src={src} alt="" width={16} height={16} />
  ) : (
    <Database size={15} strokeWidth={1.7} aria-hidden="true" />
  );
}

export function DataFeed({
  items,
  steps,
}: {
  items: FeedItem[];
  steps: FeedStep[];
}) {
  const groups = new Map<string, FeedItem[]>();
  for (const item of items)
    groups.set(item.source, [...(groups.get(item.source) || []), item]);
  const ready = items.filter((i) => i.status === "ready").length;
  const current =
    steps.find((s) => s.status === "active") ||
    steps.findLast((s) => s.status === "done");
  return (
    <div className="data-feed">
      <p className="feed-sr" role="status" aria-live="polite">
        {current ? `${current.label}. ` : ""}
        {items.length ? `${ready} of ${items.length} tables read.` : ""}
      </p>
      <div className="feed-grid">
        <ol className="feed-steps">
          {steps.map((s) => (
            <li key={s.label} className={`feed-step is-${s.status}`}>
              <span className="feed-step-mark" aria-hidden="true">
                {s.status === "done" && <Check size={10} strokeWidth={3.2} />}
              </span>
              <span>
                {s.label}
                <span className="feed-sr">
                  {s.status === "done"
                    ? " (done)"
                    : s.status === "active"
                      ? " (in progress)"
                      : ""}
                </span>
              </span>
            </li>
          ))}
        </ol>
        <div className="feed-sources">
          {[...groups].map(([source, rows]) => {
            const done = rows.filter((r) => r.status === "ready");
            const total = done.reduce((n, r) => n + (r.rows || 0), 0);
            // One unit per header: tables while any is loading, then records.
            const all = done.length === rows.length;
            return (
              <section key={source} className="feed-source">
                <h4>
                  <SourceMark source={source} />
                  <span>{sourceName(source)}</span>
                  <small key={all ? "records" : "tables"}>
                    {all
                      ? `${number(total)} ${total === 1 ? "record" : "records"}`
                      : `${done.length} of ${rows.length} ${rows.length === 1 ? "table" : "tables"}`}
                  </small>
                </h4>
                <ul>
                  {rows.map((item, i) => (
                    <li
                      key={item.id}
                      className={`feed-item is-${item.status}`}
                      style={vars({ "--i": i })}
                    >
                      <span className="feed-item-mark" aria-hidden="true">
                        {item.status === "ready" && (
                          <Check size={10} strokeWidth={3} />
                        )}
                      </span>
                      <span className="feed-item-name" title={item.name}>
                        {item.name.split(" · ").at(-1)}
                      </span>
                      <span className="feed-item-rows">
                        {item.status === "ready" && item.rows !== undefined ? (
                          <>
                            {number(item.rows)}
                            <span className="feed-sr"> records</span>
                          </>
                        ) : item.status === "loading" ? (
                          <span className="feed-shimmer" />
                        ) : item.status === "pending" ? (
                          "Queued"
                        ) : null}
                      </span>
                    </li>
                  ))}
                </ul>
              </section>
            );
          })}
        </div>
      </div>
    </div>
  );
}
