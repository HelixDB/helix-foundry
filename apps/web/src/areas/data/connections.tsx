import { useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ArrowRight, Plus, RefreshCw } from "lucide-react";
import { api, number, relative } from "../../api";
import { paths } from "../../paths";
import { useWorkspace, useResources, useAction } from "../../ui";
import { ConnectData } from "../../connect-data";
import {
  Button,
  ButtonLink,
  EmptyState,
  Page,
  PageHeader,
  SearchInput,
  Skeleton,
  SourceLogo,
  StatusBadge,
  connectorLabel,
  connectorOf,
} from "../../kit";
import { sampleSources } from "../../../../../packages/shared/src";
import type { HomeSummary, Resource } from "../../../../../packages/shared/src";

type Card = {
  key: string;
  name: string;
  logo: string;
  kind: "source" | "sample" | "files" | "pipeline";
  source?: Resource;
  datasets: Resource[];
};
const SHOWN = 6;

// How a card's data stays current, in plain words.
function syncText(c: Card) {
  if (c.kind === "sample") return "Sample data · no sync";
  if (c.kind === "pipeline") return "Built from your data by the ontology";
  if (!c.source) return "Uploads and ingestion API";
  const s = c.source.data;
  if (s.paused) return "Paused";
  if (s.sync?.mode === "cdc") return "Live changes";
  if (s.kind === "sqlite")
    return "Uploaded file · upload a new copy to refresh";
  return s.schedule
    ? `Scheduled refresh (${s.schedule} UTC)`
    : "Automatic refresh";
}

// The Data page: one card per place data comes from (connections, the sample
// sources that stand in for them, files, then pipeline outputs), each with
// how it stays current and the tables it holds.
export function DataOverview() {
  const { id, role } = useWorkspace(),
    datasets = useResources("dataset"),
    sources = useResources("source"),
    findings = useResources("finding"),
    action = useAction();
  const home = useQuery({
    queryKey: [id, "home"],
    queryFn: () => api<HomeSummary>(`/workspaces/${id}/home`),
    refetchInterval: 20000,
  });
  const [picker, setPicker] = useState(
    new URLSearchParams(location.search).has("add"),
  );
  const [search, setSearch] = useState(""),
    [expanded, setExpanded] = useState<Set<string>>(new Set());
  const cards = new Map<string, Card>();
  for (const s of sources.data?.items || []) {
    const key = s.data.connectionId || s.id;
    if (!cards.has(key))
      cards.set(key, {
        key,
        name: s.data.connectionName || s.name,
        logo: connectorOf(s),
        kind: "source",
        source: s,
        datasets: [],
      });
  }
  for (const d of datasets.data?.items || []) {
    const source = (sources.data?.items || []).find(
      (s: Resource) => s.id === d.data.sourceId,
    );
    const key = d.data.generation
      ? "pipeline"
      : source
        ? source.data.connectionId || source.id
        : d.data.sampleSource
          ? "sample:" + d.data.sampleSource
          : "files";
    const card: Card = cards.get(key) || {
      key,
      ...(d.data.generation
        ? { name: "Pipeline outputs", logo: "pipeline", kind: "pipeline" }
        : d.data.sampleSource
          ? {
              name: connectorLabel(d.data.sampleSource),
              logo: d.data.sampleSource,
              kind: "sample",
            }
          : { name: "Files and API", logo: "upload", kind: "files" }),
      datasets: [],
    };
    card.datasets.push(d);
    cards.set(key, card);
  }
  const rank = (c: Card) =>
    c.kind === "source"
      ? 0
      : c.kind === "sample"
        ? 1 + (sampleSources as readonly string[]).indexOf(c.logo) / 10
        : c.kind === "files"
          ? 2
          : 3;
  // Search matches a card's name or its tables; a card shows the tables that
  // match unless its own name does.
  const term = search.trim().toLowerCase(),
    matches = (text: string) => text.toLowerCase().includes(term);
  const ordered = [...cards.values()]
    .sort((a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name))
    .map((c) =>
      !term || matches(c.name)
        ? c
        : {
            ...c,
            datasets: c.datasets.filter((d) =>
              matches(d.name + " " + (d.description || "")),
            ),
          },
    )
    .filter((c) => !term || c.datasets.length || matches(c.name));
  const health = new Map((home.data?.connections || []).map((c) => [c.id, c]));
  const issues =
    findings.data?.items.filter((f: Resource) => !f.data.dismissed) || [];
  const loading = datasets.isPending || sources.isPending;
  const empty = !loading && !cards.size;
  return (
    <Page>
      <PageHeader
        title="Data"
        subtitle="Where your data comes from, what it holds and how it stays up to date."
        actions={
          role !== "viewer" && (
            <Button
              variant="primary"
              icon={Plus}
              onClick={() => setPicker(true)}
            >
              Add data
            </Button>
          )
        }
      />
      {issues.length > 0 && (
        <details className="disclosure data-issues">
          <summary>
            {issues.length} source{" "}
            {issues.length === 1 ? "issue needs" : "issues need"} attention
          </summary>
          {issues.map((f: Resource) => (
            <div key={f.id}>
              <strong>{f.name}</strong>
              <p>
                {f.data.evidence?.error ||
                  f.data.evidence?.qualityIssues?.join(" · ") ||
                  "The source changed. Check the next snapshot before accepting it."}
              </p>
              {f.data.runId && (
                <Link to={paths.thread(f.data.runId)}>
                  Inspect prepared repair
                </Link>
              )}
            </div>
          ))}
        </details>
      )}
      {!empty && (
        <SearchInput
          className="cn-search"
          value={search}
          onChange={setSearch}
          label="Search data"
          placeholder="Search sources and tables"
        />
      )}
      {loading ? (
        <div className="cn-grid">
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} height={180} />
          ))}
        </div>
      ) : empty ? (
        <EmptyState
          title="No data connected"
          text="Connect a database or an app, or upload files."
          action={
            role !== "viewer" && (
              <Button
                variant="primary"
                icon={Plus}
                onClick={() => setPicker(true)}
              >
                Add data
              </Button>
            )
          }
        />
      ) : !ordered.length ? (
        <p className="cn-none">No sources or tables match “{search.trim()}”.</p>
      ) : (
        <div className="cn-grid">
          {ordered.map((c) => {
            const h = health.get(c.key),
              records = c.datasets.reduce(
                (n, d) => n + (d.data.profile?.rows || 0),
                0,
              ),
              refreshed = h?.lastSyncAt || c.source?.data.lastSync,
              open = expanded.has(c.key) || !!term,
              tables = open ? c.datasets : c.datasets.slice(0, SHOWN);
            return (
              <section key={c.key} className="cn-card">
                <header>
                  <SourceLogo source={c.logo} size={32} />
                  <div className="cn-title">
                    <h2>
                      {c.name}
                      {c.kind === "sample" && (
                        <span className="ds-tag">Sample</span>
                      )}
                    </h2>
                    <p>
                      {c.kind === "source" &&
                        connectorLabel(c.logo) !== c.name &&
                        `${connectorLabel(c.logo)} · `}
                      {syncText(c)}
                    </p>
                  </div>
                  {h && <StatusBadge status={h.status} size="sm" />}
                </header>
                {h?.error && <p className="cn-error">{h.error}</p>}
                <ul>
                  {tables.map((d) => (
                    <li key={d.id}>
                      <Link to={paths.dataset(d.id)} title={d.name}>
                        <span className="ds-name">
                          {d.name.split(" · ").at(-1)}
                        </span>
                        {d.data.pendingVersion && (
                          <span className="attention">Schema review</span>
                        )}
                        <span className="ds-rows">
                          {d.data.profile ? number(d.data.profile.rows) : "—"}
                        </span>
                      </Link>
                    </li>
                  ))}
                  {!open && c.datasets.length > SHOWN && (
                    <li className="cn-more">
                      <button
                        type="button"
                        onClick={() =>
                          setExpanded((e) => new Set([...e, c.key]))
                        }
                      >
                        Show {c.datasets.length - SHOWN} more
                      </button>
                    </li>
                  )}
                  {!c.datasets.length && (
                    <li className="cn-more">Waiting for the first import</li>
                  )}
                </ul>
                <footer>
                  <span>
                    {c.datasets.length}{" "}
                    {c.datasets.length === 1 ? "table" : "tables"} ·{" "}
                    {number(records)} records
                    {refreshed && ` · refreshed ${relative(refreshed)}`}
                  </span>
                  {c.source && role !== "viewer" && (
                    <Button
                      size="sm"
                      variant="ghost"
                      icon={RefreshCw}
                      disabled={action.pending}
                      onClick={() =>
                        action.run(
                          () =>
                            api(
                              `/workspaces/${id}/sources/${c.source!.id}/sync`,
                              {},
                            ),
                          "Refresh queued",
                        )
                      }
                    >
                      Refresh
                    </Button>
                  )}
                  {c.source && (
                    <ButtonLink
                      size="sm"
                      variant="ghost"
                      iconRight={ArrowRight}
                      to={paths.connection(c.source.id)}
                    >
                      Open
                    </ButtonLink>
                  )}
                </footer>
              </section>
            );
          })}
        </div>
      )}
      {picker && <ConnectData onClose={() => setPicker(false)} />}
    </Page>
  );
}
