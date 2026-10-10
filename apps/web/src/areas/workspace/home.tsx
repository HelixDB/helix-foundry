import "./home.css";
import {
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  ArrowRight,
  ArrowUp,
  Cable,
  CircleAlert,
  History,
  LoaderCircle,
  Shapes,
  ShieldCheck,
  Sparkles,
  Workflow,
} from "lucide-react";
import type { HomeSummary } from "../../../../../packages/shared/src";
import { api } from "../../api";
import { useWorkspace } from "../../ui";
import {
  Button,
  ButtonLink,
  EmptyState,
  Page,
  Panel,
  Skeleton,
  SourceStack,
  StatCard,
  connectorLabel,
  cx,
  formatCompact,
  formatNumber,
  plural,
  useMedia,
  useTitle,
  type Status,
} from "../../kit";
import { paths } from "../../paths";
import { readRecents, sectionIcon } from "../../shell/recents";
import { useAskAnalyst } from "../analyst/launcher";
import { VoiceOrb, useVoice, voiceHint } from "../analyst/voice";
import { KeyMetrics } from "./metrics";

type Connection = HomeSummary["connections"][number];
// Sample data stands in for real apps, so each sample source counts as a connection.
const isConnection = (c: Connection) => !!c.sourceId || c.sample;

const greeting = (hour: number) =>
  hour < 5
    ? "Good evening"
    : hour < 12
      ? "Good morning"
      : hour < 18
        ? "Good afternoon"
        : "Good evening";
const pluralName = (name: string) => {
  const n = name.toLowerCase();
  return /(s|x|ch|sh)$/.test(n)
    ? n + "es"
    : /[^aeiou]y$/.test(n)
      ? n.slice(0, -1) + "ies"
      : n + "s";
};
const connectionPath = (c: Connection) =>
  c.sourceId ? paths.connection(c.sourceId) : paths.data();
const connectionName = (c: Connection) =>
  c.sample ? connectorLabel(c.connector) : c.name;

// Asking starts a chat and opens it (/analyst/:id); follow-ups happen there.
function AskBox({ summary }: { summary?: HomeSummary }) {
  const small = useMedia("(max-width: 800px)");
  const ask = useAskAnalyst(),
    [text, setText] = useState(""),
    [pending, setPending] = useState(false),
    sending = useRef(false);
  const input = useRef<HTMLTextAreaElement>(null),
    location = useLocation(),
    hintId = useId();
  // Voice started here belongs to no chat until its first question opens one.
  const voice = useVoice(),
    voiceHere = voice.status !== "off" && !voice.chatId,
    showHeard = voiceHere && !text && !voice.paused && !!voice.heard,
    voiceText =
      voiceHere && text && voice.heard && !voice.paused
        ? `Heard: ${voice.heard}`
        : voiceHere || voice.error
          ? voiceHint(voice)
          : null;
  useEffect(() => {
    if (voiceHere && voice.paused && !text) voice.resumeListening();
  }, [voiceHere, voice.paused, text]);
  // "New chat" in the sidebar lands here with { ask: true }.
  useEffect(() => {
    if ((location.state as { ask?: boolean } | null)?.ask)
      input.current?.focus();
  }, [location.key]);
  // Grow with the question, up to a few lines.
  useLayoutEffect(() => {
    const el = input.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = Math.min(el.scrollHeight, 140) + "px";
  }, [text, showHeard && voice.heard]);
  const suggestions = useMemo(() => {
    const types = summary?.ontology.types || [],
      link = summary?.ontology.links[0];
    const out: string[] = [];
    if (link)
      out.push(
        `Which ${pluralName(link.to)} have the most ${pluralName(link.from)}?`,
      );
    if (types[0]) out.push(`Summarize our ${pluralName(types[0].name)}`);
    if (types[1] && types[1].name !== link?.from)
      out.push(
        `How many ${pluralName(types[1].name)} were created each month?`,
      );
    out.push("What changed in my data this week?");
    return out.slice(0, 3);
  }, [summary]);
  const submit = async (prompt: string) => {
    if (prompt.trim().length < 5 || sending.current) return;
    sending.current = true;
    // A suggestion fills the box, so it stays there to edit if sending fails.
    setText(prompt);
    setPending(true);
    try {
      // A typed question during voice: its chat becomes the voice chat.
      if (voiceHere) voice.noteTyped(prompt.trim());
      // Success opens the chat; on failure the error is shown and the
      // question stays in the box.
      if (!(await ask({ prompt }))) input.current?.focus();
    } finally {
      sending.current = false;
      setPending(false);
    }
  };
  return (
    <form
      className={cx("wk-ask", pending && "is-pending")}
      aria-busy={pending}
      onSubmit={(e) => {
        e.preventDefault();
        void submit(text);
      }}
    >
      <div className="wk-ask-row">
        <div className="wk-ask-box" onClick={() => input.current?.focus()}>
          <Sparkles size={18} className="wk-ask-icon" aria-hidden />
          <textarea
            ref={input}
            rows={1}
            aria-label="Ask anything about your data"
            aria-describedby={hintId}
            className={cx(showHeard && "is-voice")}
            placeholder={
              voiceHere && !voice.paused
                ? "Listening…"
                : small
                  ? "Ask about your data…"
                  : "Ask anything about your data…"
            }
            value={showHeard ? voice.heard : text}
            readOnly={pending}
            onChange={(e) => {
              // Typing takes the box back from voice until sent or cleared.
              if (voiceHere && e.target.value) voice.pauseForTyping();
              setText(e.target.value);
            }}
            onKeyDown={(e) => {
              if (
                e.key === "Enter" &&
                !e.shiftKey &&
                !e.nativeEvent.isComposing
              ) {
                e.preventDefault();
                void submit(text);
              }
            }}
          />
          <button
            type="submit"
            className={cx("wk-ask-send", pending && "is-pending")}
            aria-label="Ask"
            disabled={pending || text.trim().length < 5}
          >
            {pending ? (
              <LoaderCircle size={17} className="hf-spin" aria-hidden />
            ) : (
              <ArrowUp size={17} aria-hidden />
            )}
          </button>
        </div>
        <VoiceOrb chatId={null} placement="home" />
      </div>
      {voiceText && (
        <p
          className={cx("wk-voice-hint", voice.error && "is-error")}
          role={voice.error ? "alert" : undefined}
        >
          {voiceText}
        </p>
      )}
      <span id={hintId} className="hf-sr-only">
        Press Enter to ask. Shift+Enter adds a new line.
      </span>
      {/* The spinner shows progress; this tells screen readers. */}
      <span className="hf-sr-only" aria-live="polite">
        {pending ? "Starting a chat…" : ""}
      </span>
      <div className="wk-suggestions" aria-label="Suggested questions">
        {suggestions.map((s) => (
          <button
            key={s}
            type="button"
            className="wk-suggestion"
            onClick={() => void submit(s)}
            disabled={pending}
          >
            {s}
          </button>
        ))}
      </div>
    </form>
  );
}

function HealthStrip({ s }: { s?: HomeSummary }) {
  const conns = (s?.connections || []).filter(isConnection),
    onlySamples = conns.length > 0 && conns.every((c) => c.sample);
  const count = (st: string) => conns.filter((c) => c.status === st).length;
  const healthy = count("healthy"),
    syncing = count("syncing"),
    failing = count("failed");
  const worst: Status = failing ? "failed" : syncing ? "syncing" : "healthy";
  return (
    <section className="wk-health" aria-label="Workspace health">
      <StatCard
        label="Connections"
        to={paths.connections()}
        loading={!s}
        status={conns.length ? worst : undefined}
        value={
          conns.length ? (
            <>
              {healthy}
              <small>of {conns.length} healthy</small>
            </>
          ) : (
            <>
              0<small>connected</small>
            </>
          )
        }
        hint={
          failing || syncing
            ? [failing && `${failing} failing`, syncing && `${syncing} syncing`]
                .filter(Boolean)
                .join(" · ")
            : onlySamples
              ? "Sample data · connect your own anytime"
              : conns.length
                ? "All connections are healthy"
                : "No data connected yet"
        }
      >
        {conns.length > 0 && (
          <SourceStack sources={conns.map((c) => c.connector)} size={24} />
        )}
      </StatCard>
      <StatCard
        label="Data health"
        to={paths.data()}
        loading={!s}
        status={s ? (s.findings.open ? "stale" : "healthy") : undefined}
        value={
          s?.findings.open ? (
            <>
              {s.findings.open}
              <small>
                {s.findings.open === 1 ? "open issue" : "open issues"}
              </small>
            </>
          ) : (
            <>
              0<small>open issues</small>
            </>
          )
        }
        hint={s?.findings.items[0]?.name || "Checks are passing"}
      />
    </section>
  );
}

// Resources this viewer opened recently in this workspace (this browser only).
function RecentStrip({ workspaceId }: { workspaceId: string }) {
  const recents = useMemo(
    () => readRecents(workspaceId).slice(0, 4),
    [workspaceId],
  );
  if (!recents.length) return null;
  return (
    <section className="wk-recent" aria-labelledby="wk-recent-title">
      <h2 id="wk-recent-title">
        <History size={14} aria-hidden /> Continue where you left off
      </h2>
      <ul>
        {recents.map((r) => {
          const Icon = sectionIcon(r.section) || History;
          return (
            <li key={r.path}>
              <Link to={r.path}>
                <Icon size={15} aria-hidden />
                <span>{r.title}</span>
              </Link>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

// A small hub-and-spoke map of object types and their links.
function OntologyMap({ s }: { s: HomeSummary }) {
  const navigate = useNavigate();
  const types = s.ontology.types.slice(0, 10),
    links = s.ontology.links.filter(
      (l) =>
        types.some((t) => t.name === l.from) &&
        types.some((t) => t.name === l.to),
    );
  const degree = (name: string) =>
    links.filter((l) => l.from === name || l.to === name).length;
  const ordered = [...types].sort(
    (a, b) => degree(b.name) - degree(a.name) || b.objects - a.objects,
  );
  const W = 440,
    H = 260,
    cx = W / 2,
    cy = H / 2 - 6;
  const max = Math.max(1, ...types.map((t) => t.objects));
  const pos = new Map<string, { x: number; y: number; r: number }>();
  ordered.forEach((t, i) => {
    const r = 9 + 12 * Math.sqrt(t.objects / max);
    if (i === 0 && ordered.length > 2) pos.set(t.name, { x: cx, y: cy, r });
    else {
      const ring = ordered.length > 2 ? ordered.length - 1 : ordered.length,
        k = ordered.length > 2 ? i - 1 : i,
        angle = -Math.PI / 2 + (k / ring) * Math.PI * 2 + 0.35;
      pos.set(t.name, {
        x: cx + Math.cos(angle) * 160,
        y: cy + Math.sin(angle) * 86,
        r,
      });
    }
  });
  return (
    <svg className="wk-map" viewBox={`0 0 ${W} ${H}`} aria-hidden>
      {links.map((l, i) => {
        const a = pos.get(l.from)!,
          b = pos.get(l.to)!;
        return (
          <line
            key={i}
            className="wk-map-edge"
            x1={a.x}
            y1={a.y}
            x2={b.x}
            y2={b.y}
          >
            <title>{`${l.from} → ${l.name} → ${l.to}`}</title>
          </line>
        );
      })}
      {ordered.map((t) => {
        const p = pos.get(t.name)!;
        return (
          <g
            key={t.id}
            className="wk-map-node"
            transform={`translate(${p.x} ${p.y})`}
            onClick={() => navigate(paths.objectType(t.typeId))}
          >
            <circle r={p.r + 5} className="wk-map-halo" />
            <circle r={p.r} className="wk-map-dot" />
            <text y={p.r + 16} className="wk-map-label">
              {t.name}
            </text>
            <text y={p.r + 30} className="wk-map-count">
              {formatCompact(t.objects)}
            </text>
          </g>
        );
      })}
    </svg>
  );
}

function OntologyPanel({ s }: { s?: HomeSummary }) {
  const types = s?.ontology.types || [];
  const max = Math.max(1, ...types.map((t) => t.objects));
  return (
    <Panel
      className="wk-ontology"
      title="Ontology"
      subtitle={
        s
          ? `${plural(types.length, "object type")} · ${plural(s.ontology.links.length, "link type")} · ${plural(s.counts.objects, "object")}`
          : undefined
      }
      actions={
        <ButtonLink
          size="sm"
          variant="ghost"
          to={paths.ontology()}
          iconRight={ArrowRight}
        >
          Open
        </ButtonLink>
      }
    >
      {!s ? (
        <div className="wk-ontology-body">
          <Skeleton height={220} />
          <Skeleton height={220} />
        </div>
      ) : !types.length ? (
        <EmptyState
          size="sm"
          icon={Shapes}
          title="No object types yet"
          text="Model the things your team works with from connected data."
          action={<ButtonLink to={paths.ontology()}>Open Ontology</ButtonLink>}
        />
      ) : (
        <div className="wk-ontology-body">
          <OntologyMap s={s} />
          <ul className="wk-types">
            {types.slice(0, 7).map((t) => (
              <li key={t.id}>
                <Link to={paths.objectType(t.typeId)}>
                  <span className="wk-type-name">{t.name}</span>
                  <span className="wk-type-count hf-num">
                    {formatNumber(t.objects)}
                  </span>
                  <span className="wk-type-bar" aria-hidden>
                    <span style={{ width: `${(t.objects / max) * 100}%` }} />
                  </span>
                </Link>
              </li>
            ))}
            {types.length > 7 && (
              <li className="wk-types-more">
                <Link to={paths.ontology()}>
                  {types.length - 7} more object types
                </Link>
              </li>
            )}
          </ul>
        </div>
      )}
    </Panel>
  );
}

function Attention({ s }: { s?: HomeSummary }) {
  if (!s) return null;
  const items = [
    ...s.connections
      .filter((c) => c.status === "failed")
      .map((c) => ({
        key: "c" + c.id,
        icon: Cable,
        text: `${connectionName(c)} failed to sync`,
        detail: c.error,
        to: connectionPath(c),
      })),
    ...s.pipelines.failing.map((p) => ({
      key: "p" + p.pipelineId,
      icon: Workflow,
      text: `${p.name} failed to build`,
      detail: p.error,
      to: paths.data(),
    })),
    ...s.findings.items.map((f) => ({
      key: "f" + f.id,
      icon: ShieldCheck,
      text: f.name,
      detail: f.kind === "schema" ? "Schema change needs review" : undefined,
      to: f.runId
        ? paths.thread(f.runId)
        : f.datasetId
          ? paths.dataset(f.datasetId)
          : paths.data(),
    })),
  ].slice(0, 5);
  if (!items.length) return null;
  return (
    <section className="wk-attention" aria-label="Needs attention">
      <h2>
        <CircleAlert size={15} aria-hidden /> Needs attention
      </h2>
      <ul>
        {items.map((i) => (
          <li key={i.key}>
            <i.icon size={15} aria-hidden />
            <span>
              <strong>{i.text}</strong>
              {i.detail && <small>{i.detail}</small>}
            </span>
            <ButtonLink size="sm" to={i.to}>
              Inspect
            </ButtonLink>
          </li>
        ))}
      </ul>
    </section>
  );
}

export function HomePage() {
  const { id, name } = useWorkspace();
  useTitle("Home");
  const home = useQuery({
    queryKey: [id, "home"],
    queryFn: () => api<HomeSummary>(`/workspaces/${id}/home`),
    refetchInterval: 20000,
  });
  const s = home.data;
  const today = new Date();
  return (
    <Page className="wk-home">
      <header className="wk-hero">
        <p className="wk-date">
          {today.toLocaleDateString(undefined, {
            weekday: "long",
            month: "long",
            day: "numeric",
          })}
        </p>
        <h1>{greeting(today.getHours())}</h1>
        <p className="wk-hero-sub">
          {s ? (
            <>
              <strong>{name}</strong> · {plural(s.counts.datasets, "dataset")} ·{" "}
              {plural(s.counts.objectTypes, "object type")} ·{" "}
              {plural(s.counts.objects, "object")}
            </>
          ) : (
            <strong>{name}</strong>
          )}
        </p>
        <AskBox summary={s} />
      </header>
      <KeyMetrics />
      {home.error ? (
        <div role="alert" className="wk-error">
          <span>
            Couldn’t load your workspace summary. {home.error.message}
          </span>
          <Button size="sm" onClick={() => home.refetch()}>
            Retry
          </Button>
        </div>
      ) : (
        <>
          <HealthStrip s={s} />
          <Attention s={s} />
          <RecentStrip workspaceId={id} />
          <OntologyPanel s={s} />
        </>
      )}
    </Page>
  );
}
