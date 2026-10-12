import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  forceCollide,
  forceLink,
  forceManyBody,
  forceSimulation,
  forceX,
  forceY,
  type Simulation,
  type SimulationLinkDatum,
  type SimulationNodeDatum,
} from "d3-force";
import { ArrowRight, Maximize2, Minus, Network, Plus, X } from "lucide-react";
import { api, number } from "../../api";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { typeHue } from "./colors";
import type { Resource } from "../../../../../packages/shared/src";
import {
  Button,
  ButtonLink,
  EmptyState,
  IconButton,
  SearchInput,
  Skeleton,
  cx,
} from "../../kit";

type GraphNode = {
  id: string;
  type: string;
  // "<Type> <key>"; `label` is the readable name shown on the graph.
  name: string;
  label: string;
  degree: number;
};
type GraphEdge = { from: string; to: string; name: string };
type Graph = { total: number; nodes: GraphNode[]; edges: GraphEdge[] };
type Node = GraphNode & SimulationNodeDatum & { r: number };
type Link = SimulationLinkDatum<Node> & { name: string };

const FOCUS_DEPTH = 2;
// Each search scans every type's records, so it waits for this many
// characters and a pause in typing.
const MIN_SEARCH = 2;
const SEARCH_DELAY_MS = 300;

// Records as dots coloured by type and their links as lines. Hover shows a
// record's connections, click selects it, double-click shows only the records
// around it.
export function ObjectGraph({
  onOpen,
}: {
  onOpen: (logicalId: string, type: string) => void;
}) {
  const { id } = useWorkspace();
  const [focus, setFocus] = useState<GraphNode | null>(null);
  const graph = useQuery({
    queryKey: [id, "graph", focus?.id],
    queryFn: () =>
      api<Graph>(
        `/workspaces/${id}/graph` +
          (focus
            ? `?${new URLSearchParams({ focus: focus.id, depth: String(FOCUS_DEPTH) })}`
            : ""),
      ),
    placeholderData: (previous) => previous,
  });
  const [hidden, setHidden] = useState<Set<string>>(new Set());
  const [selected, setSelected] = useState<string | null>(null);
  const [hover, setHover] = useState<{
    id: string;
    x: number;
    y: number;
  } | null>(null);
  const [search, setSearch] = useState("");
  const [term, setTerm] = useState("");
  const [matchIndex, setMatchIndex] = useState(0);
  const [enterRequested, setEnterRequested] = useState(false);
  const searchable = search.trim().length >= MIN_SEARCH;
  useEffect(() => {
    setMatchIndex(0);
    const next = searchable ? search.trim() : "";
    const timer = setTimeout(() => setTerm(next), SEARCH_DELAY_MS);
    return () => clearTimeout(timer);
  }, [search]);
  const results = useQuery({
    queryKey: [id, "graph-record-search", term],
    queryFn: () =>
      api<{ items: Resource[]; total: number }>(
        `/workspaces/${id}/resources/object?${new URLSearchParams({ search: term, limit: "8" })}`,
      ),
    enabled: !!term,
  });

  const wrap = useRef<HTMLDivElement>(null),
    canvas = useRef<HTMLCanvasElement>(null),
    sim = useRef<Simulation<Node, Link> | null>(null),
    nodes = useRef<Node[]>([]),
    links = useRef<Link[]>([]),
    view = useRef({ x: 0, y: 0, k: 1 }),
    // Whether the view keeps fitting a new layout as it settles: until the
    // person pans, zooms or drags.
    following = useRef(false),
    size = useRef({ w: 0, h: 0 }),
    frame = useRef(0),
    state = useRef({
      hidden,
      selected,
      hover: null as string | null,
      types: [] as string[],
    });
  const data = graph.data;
  // The canvas only exists once there is a graph to draw.
  const ready = !!data?.nodes.length;
  const types = useMemo(
    () => [...new Set((data?.nodes || []).map((n) => n.type))].sort(),
    [data],
  );
  // Drawing and the pointer handlers read these through a ref, so a handler
  // bound once never draws with stale state.
  state.current.hidden = hidden;
  state.current.selected = selected;
  state.current.hover = hover?.id ?? null;
  state.current.types = types;
  const counts = useMemo(() => {
    const c = new Map<string, number>();
    for (const n of data?.nodes || []) c.set(n.type, (c.get(n.type) || 0) + 1);
    return c;
  }, [data]);
  const hueOf = (type: string) => typeHue(state.current.types, type);
  const byId = useMemo(
    () => new Map((data?.nodes || []).map((n) => [n.id, n])),
    [data],
  );
  const neighbours = useMemo(() => {
    const m = new Map<string, { node: GraphNode; name: string }[]>();
    for (const e of data?.edges || []) {
      const from = byId.get(e.from),
        to = byId.get(e.to);
      if (!from || !to) continue;
      if (!m.has(e.from)) m.set(e.from, []);
      if (!m.has(e.to)) m.set(e.to, []);
      m.get(e.from)!.push({ node: to, name: e.name });
      m.get(e.to)!.push({ node: from, name: e.name });
    }
    return m;
  }, [data, byId]);

  const draw = () => {
    frame.current = 0;
    const c = canvas.current,
      ctx = c?.getContext("2d");
    if (!c || !ctx) return;
    const { w, h } = size.current,
      dpr = window.devicePixelRatio || 1,
      { x, y, k } = view.current,
      style = getComputedStyle(c),
      token = (name: string) => style.getPropertyValue(name).trim(),
      { hidden, selected, hover } = state.current,
      active = hover || selected,
      near = new Set<string>(active ? [active] : []);
    if (active)
      for (const l of links.current) {
        const s = (l.source as Node).id,
          t = (l.target as Node).id;
        if (s === active) near.add(t);
        if (t === active) near.add(s);
      }
    const shown = (n: Node) => !hidden.has(n.type);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    ctx.setTransform(dpr * k, 0, 0, dpr * k, dpr * x, dpr * y);
    // Links: thin and recessive; a hovered or selected record's links stand out.
    ctx.lineWidth = 1 / k;
    for (const l of links.current) {
      const s = l.source as Node,
        t = l.target as Node;
      if (!shown(s) || !shown(t)) continue;
      const lit = !!active && (s.id === active || t.id === active);
      ctx.strokeStyle = lit
        ? token("--hf-text-2")
        : token("--hf-border-strong");
      ctx.globalAlpha = lit ? 0.9 : active ? 0.12 : 0.55;
      ctx.lineWidth = (lit ? 1.6 : 1) / k;
      ctx.beginPath();
      ctx.moveTo(s.x!, s.y!);
      ctx.lineTo(t.x!, t.y!);
      ctx.stroke();
    }
    // Records: a surface ring keeps overlapping dots apart.
    const surface = token("--hf-surface"),
      faint = token("--hf-faint");
    for (const n of nodes.current) {
      if (!shown(n)) continue;
      ctx.globalAlpha = active && !near.has(n.id) ? 0.2 : 1;
      ctx.beginPath();
      ctx.arc(n.x!, n.y!, n.r, 0, Math.PI * 2);
      ctx.fillStyle = hueOf(n.type) || faint;
      ctx.fill();
      ctx.lineWidth = 1.5 / k;
      ctx.strokeStyle = surface;
      ctx.stroke();
      if (n.id === selected) {
        ctx.beginPath();
        ctx.arc(n.x!, n.y!, n.r + 3 / k, 0, Math.PI * 2);
        ctx.lineWidth = 2 / k;
        ctx.strokeStyle = token("--hf-ink");
        ctx.stroke();
      }
    }
    // Labels in text ink with a surface halo, for the records in play or, once
    // zoomed in, for every record. The most important go first, and a label
    // that would overlap one already placed is left out.
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.globalAlpha = 1;
    ctx.font = `12px ${token("--hf-font-sans") || "sans-serif"}`;
    ctx.textBaseline = "middle";
    ctx.lineJoin = "round";
    ctx.lineWidth = 3;
    ctx.strokeStyle = surface;
    const all = k >= 1.8,
      rank = (n: Node) =>
        n.id === active ? 3 : n.id === selected ? 2 : n.degree / 1e6;
    const labelled = nodes.current
      .filter(
        (n) =>
          shown(n) && (n.id === selected || (active ? near.has(n.id) : all)),
      )
      .sort((a, b) => rank(b) - rank(a));
    const placed: number[][] = [];
    for (const n of labelled) {
      const sx = n.x! * k + x,
        sy = n.y! * k + y;
      if (sx < -200 || sy < -20 || sx > w + 20 || sy > h + 20) continue;
      const text = n.label.length > 32 ? n.label.slice(0, 31) + "…" : n.label,
        lx = sx + n.r * k + 4,
        box = [lx - 2, sy - 8, lx + ctx.measureText(text).width + 2, sy + 8];
      if (
        placed.some(
          (p) =>
            box[0] < p[2] && box[2] > p[0] && box[1] < p[3] && box[3] > p[1],
        )
      )
        continue;
      placed.push(box);
      ctx.strokeText(text, lx, sy);
      ctx.fillStyle =
        n.id === active ? token("--hf-text") : token("--hf-text-2");
      ctx.fillText(text, lx, sy);
    }
  };
  const redraw = () => {
    if (!frame.current) frame.current = requestAnimationFrame(draw);
  };
  const toScreen = (n: Node) => ({
    x: n.x! * view.current.k + view.current.x,
    y: n.y! * view.current.k + view.current.y,
  });
  const fit = (only?: Set<string>) => {
    const list = nodes.current.filter(
      (n) => !state.current.hidden.has(n.type) && (!only || only.has(n.id)),
    );
    // The detail panel covers the right of the stage while a record is open.
    const { h } = size.current,
      w =
        size.current.w -
        (state.current.selected && size.current.w > 720 ? 324 : 0);
    if (!list.length || !w) return;
    const xs = list.map((n) => n.x!),
      ys = list.map((n) => n.y!);
    const [x0, x1, y0, y1] = [
      Math.min(...xs),
      Math.max(...xs),
      Math.min(...ys),
      Math.max(...ys),
    ];
    const k = Math.min(
      2.5,
      Math.max(0.1, Math.min(w / (x1 - x0 + 120), h / (y1 - y0 + 120))),
    );
    view.current = {
      k,
      x: w / 2 - ((x0 + x1) / 2) * k,
      y: h / 2 - ((y0 + y1) / 2) * k,
    };
    redraw();
  };
  const zoom = (
    factor: number,
    cx = size.current.w / 2,
    cy = size.current.h / 2,
  ) => {
    following.current = false;
    const v = view.current,
      k = Math.min(6, Math.max(0.1, v.k * factor));
    view.current = {
      k,
      x: cx - ((cx - v.x) / v.k) * k,
      y: cy - ((cy - v.y) / v.k) * k,
    };
    redraw();
  };
  const centre = (nodeId: string) => {
    const n = nodes.current.find((n) => n.id === nodeId);
    if (!n) return;
    following.current = false;
    const k = Math.max(view.current.k, 1.4),
      w = size.current.w - (size.current.w > 720 ? 324 : 0);
    view.current = {
      k,
      x: w / 2 - n.x! * k,
      y: size.current.h / 2 - n.y! * k,
    };
    redraw();
  };

  // Lay out a new graph, keeping records that were already on screen in place.
  useEffect(() => {
    if (!data) return;
    const previous = new Map(nodes.current.map((n) => [n.id, n]));
    const list: Node[] = data.nodes.map((n) => ({
      ...n,
      r: 4 + Math.min(9, Math.sqrt(n.degree) * 1.5),
      x: previous.get(n.id)?.x,
      y: previous.get(n.id)?.y,
    }));
    const ids = new Set(list.map((n) => n.id));
    const edges: Link[] = data.edges
      .filter((e) => ids.has(e.from) && ids.has(e.to))
      .map((e) => ({ source: e.from, target: e.to, name: e.name }));
    // Each type gathers around its own point on a circle, so types read as
    // regions and the links between them as the lines that cross.
    const spread = 60 + Math.sqrt(list.length) * 9,
      anchor = new Map(
        types.map((t, i) => {
          const a = (i / Math.max(types.length, 1)) * Math.PI * 2 - Math.PI / 2;
          return [
            t,
            types.length > 1
              ? { x: Math.cos(a) * spread, y: Math.sin(a) * spread }
              : { x: 0, y: 0 },
          ];
        }),
      );
    sim.current?.stop();
    const s = forceSimulation<Node, Link>(list)
      .force(
        "link",
        forceLink<Node, Link>(edges)
          .id((n) => n.id)
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
    const changed =
      list.length !== previous.size || list.some((n) => !previous.has(n.id));
    // A first layout of 1,500 records takes about two seconds before anything
    // is drawn. Enough of it runs to draw, and the rest settles on screen,
    // unless motion is reduced, when all of it runs first.
    const still = matchMedia("(prefers-reduced-motion: reduce)").matches;
    for (let i = 0; i < (previous.size || !still ? 60 : 180); i++) s.tick();
    nodes.current = list;
    links.current = edges;
    sim.current = s;
    following.current = changed;
    if (changed) fit();
    s.alpha(Math.max(s.alpha(), 0.12))
      .on("tick", () => (following.current ? fit() : redraw()))
      .restart();
    return () => {
      s.stop();
    };
  }, [data]);

  // The canvas follows its container and the theme.
  useEffect(() => {
    const c = canvas.current,
      w = wrap.current;
    if (!c || !w) return;
    const resize = () => {
      const r = w.getBoundingClientRect(),
        dpr = window.devicePixelRatio || 1,
        first = !size.current.w;
      size.current = { w: r.width, h: r.height };
      c.width = Math.round(r.width * dpr);
      c.height = Math.round(r.height * dpr);
      c.style.width = r.width + "px";
      c.style.height = r.height + "px";
      if (first) fit();
      redraw();
    };
    resize();
    const ro = new ResizeObserver(resize);
    ro.observe(w);
    const mo = new MutationObserver(redraw);
    mo.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ["data-theme"],
    });
    const media = matchMedia("(prefers-color-scheme: dark)");
    media.addEventListener("change", redraw);
    return () => {
      ro.disconnect();
      mo.disconnect();
      media.removeEventListener("change", redraw);
      cancelAnimationFrame(frame.current);
      frame.current = 0;
    };
  }, [ready]);
  useEffect(redraw, [hidden, selected, hover?.id, types]);
  useEffect(() => {
    if (!selected) return;
    const key = (e: KeyboardEvent) => {
      if (e.key === "Escape") setSelected(null);
    };
    document.addEventListener("keydown", key);
    return () => document.removeEventListener("keydown", key);
  }, [selected]);

  // Wheel zooms around the pointer; dragging a record moves it, dragging the
  // background pans; a click without movement selects or clears.
  useEffect(() => {
    const c = canvas.current;
    if (!c) return;
    const point = (e: MouseEvent) => {
      const r = c.getBoundingClientRect();
      return { x: e.clientX - r.left, y: e.clientY - r.top };
    };
    const hit = (p: { x: number; y: number }) => {
      const v = view.current,
        gx = (p.x - v.x) / v.k,
        gy = (p.y - v.y) / v.k;
      let best: Node | undefined,
        bestD = Infinity;
      for (const n of nodes.current) {
        if (state.current.hidden.has(n.type)) continue;
        const d = Math.hypot(n.x! - gx, n.y! - gy);
        if (d < n.r + 5 / v.k && d < bestD) {
          best = n;
          bestD = d;
        }
      }
      return best;
    };
    let drag:
      | { node?: Node; start: { x: number; y: number }; moved: boolean }
      | undefined;
    const down = (e: PointerEvent) => {
      if (e.button !== 0) return;
      following.current = false;
      const p = point(e);
      drag = { node: hit(p), start: p, moved: false };
      c.setPointerCapture(e.pointerId);
    };
    const move = (e: PointerEvent) => {
      const p = point(e);
      if (!drag) {
        const n = hit(p);
        c.style.cursor = n ? "pointer" : "grab";
        setHover((h) =>
          n ? { id: n.id, x: p.x, y: p.y } : h === null ? h : null,
        );
        return;
      }
      const dx = p.x - drag.start.x,
        dy = p.y - drag.start.y;
      if (!drag.moved && Math.hypot(dx, dy) < 4) return;
      if (!drag.moved) {
        drag.moved = true;
        setHover(null);
      }
      c.style.cursor = "grabbing";
      if (drag.node) {
        const v = view.current;
        drag.node.fx = (p.x - v.x) / v.k;
        drag.node.fy = (p.y - v.y) / v.k;
        sim.current?.alphaTarget(0.15).restart();
      } else {
        view.current = {
          ...view.current,
          x: view.current.x + e.movementX,
          y: view.current.y + e.movementY,
        };
        redraw();
      }
    };
    const up = () => {
      if (!drag) return;
      if (drag.node && drag.moved) {
        drag.node.fx = null;
        drag.node.fy = null;
        sim.current?.alphaTarget(0);
      }
      if (!drag.moved) setSelected(drag.node?.id ?? null);
      drag = undefined;
      c.style.cursor = "grab";
    };
    const wheel = (e: WheelEvent) => {
      e.preventDefault();
      const p = point(e);
      zoom(Math.exp(-e.deltaY * 0.0015), p.x, p.y);
    };
    const leave = () => setHover(null);
    c.addEventListener("pointerdown", down);
    c.addEventListener("pointermove", move);
    c.addEventListener("pointerup", up);
    c.addEventListener("pointercancel", up);
    c.addEventListener("pointerleave", leave);
    c.addEventListener("wheel", wheel, { passive: false });
    return () => {
      c.removeEventListener("pointerdown", down);
      c.removeEventListener("pointermove", move);
      c.removeEventListener("pointerup", up);
      c.removeEventListener("pointercancel", up);
      c.removeEventListener("pointerleave", leave);
      c.removeEventListener("wheel", wheel);
    };
  }, [ready]);

  const show = (n: GraphNode) => {
    setHidden((h) => new Set([...h].filter((t) => t !== n.type)));
    setFocus(n);
    setSelected(n.id);
  };
  const select = (nodeId: string) => {
    setSelected(nodeId);
    const n = nodes.current.find((n) => n.id === nodeId);
    if (n && state.current.hidden.has(n.type))
      setHidden((h) => new Set([...h].filter((t) => t !== n.type)));
    centre(nodeId);
  };
  const searchReady = !!term && term === search.trim() && !results.isFetching;
  const matches: GraphNode[] =
    searchReady && !results.error
      ? (results.data?.items || []).map((r) => ({
          id: r.data.logicalId,
          type: r.data.type,
          name: r.name,
          label: r.data.label,
          degree: r.data.links,
        }))
      : [];
  const choose = (n: GraphNode) => {
    if (byId.has(n.id)) select(n.id);
    else show(n);
    setSearch("");
    setEnterRequested(false);
  };
  useEffect(() => {
    if (!enterRequested || !searchReady) return;
    if (matches[0]) choose(matches[0]);
    else setEnterRequested(false);
  }, [enterRequested, searchReady, results.data, results.error]);
  const current = selected ? byId.get(selected) : undefined;
  const hovered = hover ? byId.get(hover.id) : undefined;
  // Linked records by type and link, e.g. "User · belongs to" on an account.
  const grouped = current
    ? [
        ...(neighbours.get(current.id) || [])
          .reduce((m, x) => {
            const key = x.node.type + "\u0000" + x.name;
            if (!m.has(key)) m.set(key, []);
            m.get(key)!.push(x.node);
            return m;
          }, new Map<string, GraphNode[]>())
          .entries(),
      ].map(([key, list]) => {
        const [type, name] = key.split("\u0000");
        const verb = [type, current.type].reduce(
          (v, t) =>
            v.toLowerCase().startsWith(t.toLowerCase() + " ")
              ? v.slice(t.length + 1)
              : v,
          name,
        );
        return { key, type, verb, list };
      })
    : [];

  if (graph.isPending) return <Skeleton height={560} />;
  if (graph.error) return <p role="alert">{graph.error.message}</p>;
  if (!data!.nodes.length)
    return (
      <EmptyState
        icon={Network}
        title="No records yet"
        text="Records appear here once your ontology is published."
        action={
          <ButtonLink to={paths.ontology()}>View object types</ButtonLink>
        }
      />
    );
  return (
    <div className="og">
      <div className="og-bar">
        <div className="og-search">
          <SearchInput
            value={search}
            onChange={(value) => {
              setEnterRequested(false);
              setSearch(value);
            }}
            label="Find a record"
            placeholder="Find a record"
            role="combobox"
            aria-autocomplete="list"
            aria-controls={
              matches.length ? "graph-matching-records" : undefined
            }
            aria-expanded={matches.length > 0}
            aria-activedescendant={
              matches.length
                ? `graph-match-${Math.min(matchIndex, matches.length - 1)}`
                : undefined
            }
            onKeyDown={(e) => {
              if (e.key === "Enter" && search.trim()) {
                e.preventDefault();
                if (matches[0])
                  choose(matches[Math.min(matchIndex, matches.length - 1)]);
                else if (searchable && !searchReady) setEnterRequested(true);
              }
              if (
                (e.key === "ArrowDown" || e.key === "ArrowUp") &&
                matches.length
              ) {
                e.preventDefault();
                setMatchIndex(
                  (i) =>
                    (i + (e.key === "ArrowDown" ? 1 : -1) + matches.length) %
                    matches.length,
                );
              }
              if (e.key === "Escape") {
                setEnterRequested(false);
                setSearch("");
              }
            }}
          />
          {matches.length > 0 && (
            <ul
              id="graph-matching-records"
              className="og-matches"
              role="listbox"
              aria-label="Matching records"
            >
              {matches.map((n, i) => (
                <li key={n.id}>
                  <button
                    type="button"
                    role="option"
                    id={`graph-match-${i}`}
                    aria-selected={
                      i === Math.min(matchIndex, matches.length - 1)
                    }
                    onClick={() => choose(n)}
                  >
                    <span
                      className="og-dot"
                      style={{ background: hueOf(n.type) || undefined }}
                      aria-hidden
                    />
                    <span className="og-match-name">{n.label}</span>
                    <small>{n.type}</small>
                  </button>
                </li>
              ))}
            </ul>
          )}
          {search.trim() && (
            <p
              className="og-search-status"
              role={searchReady && results.error ? "alert" : "status"}
            >
              {!searchable
                ? `Type at least ${MIN_SEARCH} characters to search`
                : !searchReady
                  ? "Searching all records…"
                  : results.error
                    ? results.error.message
                    : matches.length
                      ? `${results.data!.total} matching records${results.data!.total > 8 ? " · showing the first 8" : ""}`
                      : "No matching records"}
            </p>
          )}
        </div>
        <ul className="og-legend" aria-label="Object types">
          {types.map((t) => {
            const off = hidden.has(t);
            return (
              <li key={t}>
                <button
                  type="button"
                  aria-pressed={!off}
                  className={cx("og-type", off && "is-off")}
                  title={off ? `Show ${t}` : `Hide ${t}`}
                  onClick={() =>
                    setHidden((h) => {
                      const next = new Set(h);
                      if (off) next.delete(t);
                      else next.add(t);
                      return next;
                    })
                  }
                >
                  <span
                    className="og-dot"
                    style={{ background: hueOf(t) || undefined }}
                    aria-hidden
                  />
                  {t}
                  <span className="og-count">{number(counts.get(t))}</span>
                </button>
              </li>
            );
          })}
        </ul>
      </div>
      <p className="og-scope">
        {focus ? (
          <>
            Records within {FOCUS_DEPTH} links of <strong>{focus.label}</strong>{" "}
            · {number(data!.nodes.length)} records
            <button
              type="button"
              className="og-link"
              onClick={() => {
                setFocus(null);
                setSelected(null);
              }}
            >
              Show the best-connected records
            </button>
          </>
        ) : data!.nodes.length < data!.total ? (
          <>
            {number(data!.nodes.length)} of {number(data!.total)} records: the
            best-connected and the records around them. Double-click a record to
            see everything around it. Search finds records across the whole
            workspace.
          </>
        ) : (
          <>
            {number(data!.nodes.length)} records · {number(data!.edges.length)}{" "}
            links. Double-click a record to see only what’s around it.
          </>
        )}
      </p>
      <div
        className="og-stage"
        ref={wrap}
        onDoubleClick={() => {
          const n = selected && byId.get(selected);
          if (n) show(n);
        }}
      >
        <canvas
          ref={canvas}
          role="img"
          aria-label={`Graph of ${data!.nodes.length} records and ${data!.edges.length} links. Use the Table view for the same records as a list.`}
        />
        {graph.isFetching && <div className="og-busy">Loading…</div>}
        {hovered && hover && hovered.id !== selected && (
          <div
            className="og-tip"
            style={{ left: hover.x + 14, top: hover.y + 14 }}
            aria-hidden
          >
            <strong>{hovered.label}</strong>
            <span>
              {hovered.type} · {hovered.degree}{" "}
              {hovered.degree === 1 ? "link" : "links"}
            </span>
          </div>
        )}
        <div className="og-zoom">
          <IconButton
            label="Zoom in"
            icon={Plus}
            size="sm"
            variant="secondary"
            onClick={() => zoom(1.4)}
          />
          <IconButton
            label="Zoom out"
            icon={Minus}
            size="sm"
            variant="secondary"
            onClick={() => zoom(1 / 1.4)}
          />
          <IconButton
            label="Fit to screen"
            icon={Maximize2}
            size="sm"
            variant="secondary"
            onClick={() => fit()}
          />
        </div>
        {current && (
          <aside className="og-panel" aria-label={current.label}>
            <header>
              <span
                className="og-dot"
                style={{ background: hueOf(current.type) || undefined }}
                aria-hidden
              />
              <div>
                <h2>{current.label}</h2>
                <p>
                  {current.label === current.name ? current.type : current.name}{" "}
                  · {current.degree} {current.degree === 1 ? "link" : "links"}
                </p>
              </div>
              <IconButton
                label="Close"
                icon={X}
                size="sm"
                onClick={() => setSelected(null)}
              />
            </header>
            <div className="og-actions">
              <Button
                size="sm"
                variant="primary"
                iconRight={ArrowRight}
                onClick={() => onOpen(current.id, current.type)}
              >
                Open record
              </Button>
              {focus?.id !== current.id && (
                <Button size="sm" onClick={() => show(current)}>
                  Show what’s around it
                </Button>
              )}
            </div>
            {grouped.length ? (
              grouped.map(({ key, type, verb, list }) => (
                <section key={key}>
                  <h3>
                    {type}{" "}
                    <span>
                      · {verb} · {list.length}
                    </span>
                  </h3>
                  <ul>
                    {list.slice(0, 8).map((n) => (
                      <li key={n.id}>
                        <button type="button" onClick={() => select(n.id)}>
                          <span
                            className="og-dot"
                            style={{ background: hueOf(n.type) || undefined }}
                            aria-hidden
                          />
                          <span className="og-match-name">{n.label}</span>
                          <small>{n.type}</small>
                        </button>
                      </li>
                    ))}
                    {list.length > 8 && (
                      <li className="og-more">and {list.length - 8} more</li>
                    )}
                  </ul>
                </section>
              ))
            ) : (
              <p className="og-none">
                {current.degree
                  ? "Its links lead outside this view."
                  : "No links to other records."}
              </p>
            )}
          </aside>
        )}
      </div>
    </div>
  );
}
