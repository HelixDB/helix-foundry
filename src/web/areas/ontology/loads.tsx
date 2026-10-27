import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowRight,
  CircleCheck,
  CircleGauge,
  DatabaseZap,
  GitCompareArrows,
  LoaderCircle,
  MoreHorizontal,
  RotateCcw,
  ShieldAlert,
  Trash2,
  TriangleAlert,
  UserSearch,
} from "lucide-react";
import type {
  FoundryLoad,
  GraphJob,
  OntologyVersion,
} from "../../../shared/wfe";
import { ApiError, sentence } from "../../api";
import {
  Button,
  ButtonLink,
  ConfirmDialog,
  DataGrid,
  Dialog,
  DropdownMenu,
  EmptyState,
  KeyValue,
  MenuItem,
  Page,
  PageHeader,
  Section,
  Panel,
  SkeletonText,
  StatusBadge,
  formatDate,
  formatNumber,
  formatRelative,
  plural,
  useNow,
} from "../../kit";
import { paths } from "../../paths";
import { RefreshFailed, useWorkspace } from "../../ui";
import {
  documentQueries,
  documentSetQueries,
} from "../documents/documents-api";
import { andList, formatBytes, formatUsd, liveRead } from "../documents/model";
import { loadCountsQuery } from "../explore/load-counts";
import { rulesOf } from "../explore/graph-read";
import { startingOf } from "../explore/reads";
import { useTenantReads } from "../explore/tenant-reads";
import { SettingsLinked } from "../workspace/ai-gate";
import {
  enrichmentLoadText,
  enrichmentQuery,
} from "../workspace/enrichment-api";
import { versionFormat } from "./format";
import {
  LOAD_LIMITS,
  budgetStopText,
  enrichmentOutcome,
  enrichmentStepText,
  jobBadge,
  kindLabel,
  jobDuration,
  jobTotals,
  loadActions,
  loadBadge,
  loadRefusal,
  loadRetry,
  loadDocumentPlan,
  loadDocumentsText,
  loadSources,
  ruleEvaluationRefusal,
  scopeNames,
  loadSteps,
  loadVersion,
  lookingUp,
  phaseLabel,
  stoppedWaiting,
  readAgentBundle,
  withLoad,
} from "./model";
import { DataFeed, type FeedItem } from "./ontology-scene";
import { Callout, canManage } from "./parts";
import {
  createLoad,
  deleteLoad,
  isLiveJob,
  ontologyQueries,
  setCurrentLoad,
  stopLoadEnrichment,
} from "./queries";
import { ontologyName, useOntology } from "./scope";
import { useSources, type SourceInfo } from "./sources";

// Loading an ontology's database: Load on the current version (an agent
// draft whose status is done), behind a dialog that says what it reads and
// what it may spend; the overview's Data loads section (#loads), every load
// kept, with what it holds and what may be done to it (open, compare, make
// current, delete); /ontology/:ontologyId/loads/:id, one load, shown like a generation as its data
// feed. Each load writes a full copy of the version beside the others (every
// node and edge carries its id), the newest that finishes becomes current,
// and Foundry keeps the last five.

// Live times tick every second; finished ones only need minutes.
const tick = (live: boolean) => (live ? 1000 : 30_000);

/** A version's draft and its tables (named), for a load. */
function useLoadPlan(v?: OntologyVersion | null) {
  const { id } = useWorkspace(),
    { id: o } = useOntology();
  const bundle = useMemo(
    () =>
      v && versionFormat(v) === "agent_bundle"
        ? readAgentBundle(v.definition)
        : undefined,
    [v],
  );
  const generation = useQuery({
    ...ontologyQueries.generation(id, o, v?.generationId || ""),
    enabled: !!v?.generationId,
  });
  const sources = useSources(generation.data?.generation?.tables);
  const ids = bundle?.mappings.ok ? loadSources(bundle.mappings.value) : [];
  const tables = ids.map((sourceId) => ({
    sourceId,
    info: sources.byAgentId(sourceId),
  }));
  // The table files its generation drafted from, which its mappings name by
  // document id.
  return { bundle, tables, ids, files: generation.data?.generation?.documents };
}

/** "2 tables from Stripe (customers, invoices)" and the like. */
function tablesText(tables: { sourceId: string; info?: SourceInfo }[]) {
  const byConnector = new Map<string, string[]>();
  for (const t of tables) {
    const key = t.info?.connector || "an unknown source";
    byConnector.set(key, [
      ...(byConnector.get(key) || []),
      t.info?.table || t.sourceId,
    ]);
  }
  return `${plural(tables.length, "table")} from ${[...byConnector]
    .map(([c, names]) => `${c} (${names.join(", ")})`)
    .join(" and ")}`;
}

/**
 * Load in a page header or callout: for owners and admins, on the current
 * version when it is an agent draft whose status is done. Opens the dialog,
 * at once when `opened`.
 */
export function LoadButton({
  version,
  size,
  variant = "primary",
  opened = false,
  label = "Load data",
}: {
  version: OntologyVersion;
  size?: "sm" | "md";
  variant?: "primary" | "secondary";
  opened?: boolean;
  label?: string;
}) {
  const { id, role } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    navigate = useNavigate(),
    qc = useQueryClient();
  const loads = useQuery(ontologyQueries.loads(id, o)),
    // The workspace's documents, what the current load read of them, and
    // the sets' names: what a load of this version reads is its scope's.
    documents = useQuery(documentQueries.workspace(id)),
    read = useQuery(documentQueries.list(id, o)),
    sets = useQuery(documentSetQueries.list(id)),
    settings = useQuery(enrichmentQuery(id));
  const { bundle, tables, ids, files } = useLoadPlan(version);
  const [open, setOpen] = useState(opened),
    [pending, setPending] = useState(false),
    [error, setError] = useState(""),
    // One per dialog: a retry after an unclear answer is the same load.
    [token, setToken] = useState(() => (opened ? crypto.randomUUID() : ""));
  if (!canManage(role) || !version.isCurrent || !bundle) return null;
  const evaluation = ruleEvaluationRefusal(version.definition);
  if (evaluation) return <p className="on-muted">{evaluation}</p>;
  const refusal = loadRefusal("agent_bundle", bundle.status);
  const live = liveRead(loads.data?.loads);
  if (refusal) return null;
  const scope = version.documentSetIds || [];
  // What the version's routes read: a document of its scope in a set no
  // route names is stored, not read.
  const { total, routed, stored, reading, unreadable, unread, missing } =
    loadDocumentPlan(
      documents.data || [],
      scope,
      bundle.routes.ok ? bundle.routes.value : [],
      new Set(
        (read.data?.documents || [])
          .filter((d) => d.extraction)
          .map((d) => d.id),
      ),
      ids,
      documents.data ? files : [],
      // A connector's sets: their documents aren't among the uploads.
      (sets.data || [])
        .filter((s) => s.connector)
        .map((s) => ({
          id: s.id,
          documents: s.documents || 0,
          read: (read.data?.documents || []).filter(
            (d) => d.extraction && d.setId === s.id,
          ).length,
        })),
    );
  const scopeText = andList(scopeNames(scope, sets.data || [])),
    // The most a connector's set in scope keeps, if one is.
    kept = Math.max(
      0,
      ...(sets.data || [])
        .filter((s) => s.connector && scope.includes(s.id))
        .map((s) => s.connector?.maxDocuments || 0),
    );
  const enriches = bundle.enrichments.ok && bundle.enrichments.value.length > 0;
  const loadedBefore = !!ontology.currentLoad;
  const start = async () => {
    setPending(true);
    setError("");
    try {
      const { load } = await createLoad(id, o, {
        versionId: version.id,
        clientToken: token,
      });
      await qc.invalidateQueries({ queryKey: [id, "ontology", o] });
      setOpen(false);
      navigate(paths.load(o, load.id));
    } catch (e) {
      const message = (e as Error).message;
      setError(message.charAt(0).toUpperCase() + message.slice(1) + ".");
    } finally {
      setPending(false);
    }
  };
  return (
    <>
      {live ? (
        <ButtonLink
          size={size}
          variant={variant}
          icon={LoaderCircle}
          to={paths.load(o, live.load.id)}
        >
          Loading…
        </ButtonLink>
      ) : (
        <Button
          size={size}
          variant={variant}
          icon={DatabaseZap}
          onClick={() => {
            setToken(crypto.randomUUID());
            setError("");
            setOpen(true);
          }}
        >
          {label}
        </Button>
      )}
      {open && (
        <Dialog
          title={`Load version ${version.version} into ${ontology.databaseName || "the database"}?`}
          description={
            loadedBefore
              ? `Foundry writes a new copy of the ontology’s entities and facts into its database, beside the earlier loads, and makes it current once it finishes. Foundry keeps the last ${RETAINED} loads and deletes older ones.`
              : "Foundry writes the ontology’s entities and facts into its database, beside anything already there. The Explorer reads them from it."
          }
          onClose={() => setOpen(false)}
          size="md"
          footer={
            <>
              <Button onClick={() => setOpen(false)}>Cancel</Button>
              <Button
                variant="primary"
                pending={pending}
                disabled={missing.length > 0}
                onClick={() => void start()}
              >
                Load
              </Button>
            </>
          }
        >
          <ul className="on-load-plan">
            <li>
              <strong>Tables</strong>
              <span>
                {tables.length
                  ? tablesText(tables)
                  : "None: this version maps no tables."}
              </span>
            </li>
            <li>
              <strong>Documents</strong>
              <span>
                {!scope.length
                  ? "None: this version reads no document sets. A generation that picks some reads them."
                  : !sets.data || !documents.data
                    ? sets.error || documents.error
                      ? `Couldn’t read the documents: ${(sets.error || documents.error)!.message}`
                      : "Reading the documents…"
                    : !total
                      ? `None in ${scopeText} yet. Documents you upload there later are read as they arrive.`
                      : loadDocumentsText(
                          { routed, stored, reading, unreadable, unread },
                          scopeText,
                          enriches,
                          loadedBefore
                            ? loads.data?.loads?.find((l) => l.current)
                            : null,
                        )}
              </span>
            </li>
            <li>
              <strong>Spending</strong>
              <span>
                Reading documents costs at most ${LOAD_LIMITS.jobUsd} for this
                load and ${LOAD_LIMITS.monthUsd} a month for the workspace. A
                load that reaches the cap stops, and the documents it didn’t
                reach wait for the next one.
              </span>
            </li>
            <li>
              <strong>Limits</strong>
              <span>
                {kept
                  ? `Up to ${LOAD_LIMITS.documents} uploaded documents and the newest ${formatNumber(kept)} of each connection’s set, of ${formatBytes(LOAD_LIMITS.documentBytes)} each.`
                  : `Up to ${LOAD_LIMITS.documents} documents of ${formatBytes(LOAD_LIMITS.documentBytes)} each.`}
              </span>
            </li>
            {enriches && (
              <li>
                <strong>People enrichment</strong>
                <span>
                  {settings.data
                    ? enrichmentLoadText(settings.data)
                    : settings.error
                      ? `Couldn’t read the workspace’s setting. ${settings.error.message}`
                      : "Reading the workspace’s setting…"}
                </span>
              </li>
            )}
          </ul>
          {missing.length > 0 && (
            <p role="alert" className="hf-form-error">
              {andList(missing.map((f) => f.name || "A deleted file"))}, which
              this version maps, {missing.length === 1 ? "was" : "were"} deleted
              or moved out of its sets: a load fails until the version is
              revised to stop mapping {missing.length === 1 ? "it" : "them"}, or
              the file is put back.
            </p>
          )}
          {error && (
            <p role="alert" className="hf-form-error">
              {error}
            </p>
          )}
        </Dialog>
      )}
    </>
  );
}

/** The ontology's queued or running load or extract, if any, as a callout. */
export function LiveLoad() {
  const { id } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    qc = useQueryClient();
  const loads = useQuery(ontologyQueries.loads(id, o));
  const live = liveRead(loads.data?.loads);
  const now = useNow();
  // When it ends, the ontology, its documents and the database's reads change.
  const seen = useRef("");
  useEffect(() => {
    if (live) seen.current = live.job.id;
    else if (seen.current) {
      seen.current = "";
      void qc.invalidateQueries({ queryKey: [id, "ontology", o] });
    }
  }, [live?.job.id, live?.job.status]);
  if (!live) return null;
  const { job } = live;
  const p = job.progress || {},
    starting = loadRetry(job, ontology.createdAt, now)?.kind === "starting";
  // Parked while its people are looked up: the tables load next.
  if (lookingUp(job, live.load.enrichment))
    return (
      <Callout
        icon={LoaderCircle}
        live
        title="Loading the database: looking up people"
        text={`${enrichmentStepText(live.load.enrichment!)}. Tables load next.`}
        action={
          <ButtonLink size="sm" to={paths.load(o, live.load.id)}>
            View progress
          </ButtonLink>
        }
      />
    );
  return (
    <Callout
      icon={LoaderCircle}
      live
      title={
        starting
          ? job.kind === "extract"
            ? "New documents are waiting for the new database"
            : "A load is waiting for the new database"
          : job.status === "queued"
            ? job.kind === "extract"
              ? "New documents are queued for reading"
              : "A load is queued"
            : job.kind === "extract"
              ? `Reading new documents into ${loadVersion(live.load)}`
              : `Loading the database: ${phaseLabel(job).toLowerCase()}`
      }
      text={
        starting
          ? `The database is still starting. The ${job.kind === "extract" ? "reading" : "load"} tries again by itself once it takes writes.`
          : p.documentsTotal
            ? `${p.documentsDone || 0} of ${plural(p.documentsTotal, "document")} read so far.`
            : "What it writes shows in the Explorer as it goes."
      }
      action={
        <ButtonLink size="sm" to={paths.load(o, live.load.id)}>
          View progress
        </ButtonLink>
      }
    />
  );
}

/** On an ontology's overview: the current version isn't loaded yet. */
export function NotLoaded({ version }: { version: OntologyVersion }) {
  const { id, role } = useWorkspace(),
    { id: o } = useOntology();
  const loads = useQuery(ontologyQueries.loads(id, o));
  const bundle = useMemo(
    () =>
      versionFormat(version) === "agent_bundle"
        ? readAgentBundle(version.definition)
        : undefined,
    [version],
  );
  const list = loads.data?.loads || [];
  if (!loads.isSuccess || liveRead(list) || !bundle) return null;
  if (loadRefusal("agent_bundle", bundle.status)) return null;
  const evaluation = ruleEvaluationRefusal(version.definition);
  if (evaluation)
    return (
      <Callout
        icon={DatabaseZap}
        tone="muted"
        title="Rules need evaluation"
        text={evaluation}
      />
    );
  const finished = (l: FoundryLoad) =>
    l.status === "ready" || l.status === "partial";
  if (list.some((l) => l.versionId === version.id && finished(l))) return null;
  const other = list.some(finished);
  return (
    <Callout
      icon={DatabaseZap}
      tone="muted"
      title={
        other
          ? `No load holds version ${version.version} yet`
          : `Version ${version.version} isn’t loaded yet`
      }
      text={
        canManage(role)
          ? "Load it to write its tables and your documents into the database."
          : "An owner or admin can load it to write its tables and your documents into the database."
      }
      action={<LoadButton version={version} size="sm" variant="secondary" />}
    />
  );
}

// How many loads Foundry keeps: the current one and the newest others
// (foundry.graph.retain_loads).
const RETAINED = 5;

/**
 * What a load holds, from bitmap counts in the database: its version's
 * types (nodes) and links (facts). Read in the background, through the
 * database's shared limiter.
 */
function useLoadCounts(load: FoundryLoad, version?: OntologyVersion | null) {
  const { id: ws } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    { readLater } = useTenantReads();
  const query = loadCountsQuery(ws, o, readLater, load, version);
  return useQuery({
    ...query,
    enabled: query.enabled && !!ontology.tenantId,
    select: (counts) => {
      const sum = (m: Map<string, number | null>) =>
        [...m.values()].reduce<number | null>(
          (total, n) => (total === null || n === null ? null : total + n),
          0,
        );
      return { nodes: sum(counts.nodes), facts: sum(counts.edges) };
    },
  });
}

function CountCell({
  load,
  version,
  what,
}: {
  load: FoundryLoad;
  version?: OntologyVersion | null;
  what: "nodes" | "facts";
}) {
  const counts = useLoadCounts(load, version);
  const n = counts.data?.[what];
  if (load.status === "deleting" || load.status === "delete_failed")
    return <span className="on-muted">—</span>;
  if (startingOf(counts.error ?? counts.failureReason))
    return (
      <span
        className="on-muted"
        title="The new database is still starting; this reads it again by itself."
      >
        …
      </span>
    );
  if (counts.error)
    return (
      <span className="on-muted" title={counts.error.message}>
        ?
      </span>
    );
  return n === undefined || n === null ? (
    <span className="on-muted">…</span>
  ) : (
    <>{n.toLocaleString()}</>
  );
}

/**
 * A load's jobs in a line: the newest, with its progress or result (or that
 * it waits for its new database to start: loadRetry) and what it spent.
 */
function jobsText(
  l: FoundryLoad,
  ontologyCreatedAt: string | null | undefined,
  now: number,
) {
  const jobs = l.jobs || [];
  const live = jobs.find(isLiveJob),
    shown = live || jobs[0];
  if (!shown) return "—";
  const p = shown.progress || {};
  const total = jobTotals(l);
  const step =
    loadRetry(shown, ontologyCreatedAt, now)?.kind === "starting"
      ? `${kindLabel(shown)}: database starting`
      : lookingUp(shown, l.enrichment)
        ? `${kindLabel(shown)}: looking up people`
        : isLiveJob(shown)
          ? `${kindLabel(shown)}: ${phaseLabel(shown).toLowerCase()}${p.documentsTotal ? ` ${p.documentsDone || 0}/${p.documentsTotal}` : ""}`
          : `${kindLabel(shown)}: ${jobBadge(shown).label.toLowerCase()}`;
  return `${step} · ${plural(total.count, "job")} · ${formatUsd(total.usdMicros)}`;
}

/** Kept, or on its way out: what retention says of a load. */
const retentionText = (l: FoundryLoad) =>
  l.status === "deleting"
    ? "Deleting"
    : l.status === "delete_failed"
      ? "Delete failed"
      : "Kept";

type Pending =
  | { kind: "delete"; load: FoundryLoad }
  | { kind: "retry"; load: FoundryLoad };

export function LoadsSection() {
  const { id, role, notify } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    navigate = useNavigate(),
    qc = useQueryClient();
  const loads = useQuery(ontologyQueries.loads(id, o));
  const rows = loads.data?.loads || [];
  const versionIds = [
    ...new Set(rows.flatMap((l) => (l.versionId ? [l.versionId] : []))),
  ];
  const versions = useQueries({
    queries: versionIds.map((v) => ontologyQueries.version(id, o, v)),
  });
  const versionOf = (l: FoundryLoad) =>
    versions[versionIds.indexOf(l.versionId || "")]?.data?.version;
  const now = useNow(tick(rows.some((l) => l.jobs?.some(isLiveJob))));
  const [pending, setPending] = useState<Pending | null>(null),
    [busy, setBusy] = useState(""),
    [error, setError] = useState("");
  const currentLoad = rows.find((l) => l.current);
  const manage = canManage(role);
  const act = async (
    l: FoundryLoad,
    run: () => Promise<unknown>,
    done: string,
  ) => {
    setBusy(l.id);
    setError("");
    try {
      await run();
      notify(done);
      await qc.invalidateQueries({ queryKey: [id, "ontology", o] });
      setPending(null);
    } catch (e) {
      const message = (e as Error).message;
      setError(message.charAt(0).toUpperCase() + message.slice(1) + ".");
    } finally {
      setBusy("");
    }
  };
  const explore = (l: FoundryLoad, compareWith?: FoundryLoad) =>
    withLoad(paths.explore(o), l.current ? null : l.id) +
    (compareWith
      ? `${l.current ? "?" : "&"}compare=${encodeURIComponent(compareWith.id)}`
      : "");
  const failed = !loads.data && !!loads.error;
  if (!failed && !rows.length) return null;
  return (
    <div id="loads">
      <Section
        id="on-loads"
        title="Data loads"
        description={`Each one writes your tables, as a schema version maps them, then your documents, into ${ontology.databaseName || "the ontology’s database"}, beside the others. Reads go to the current one. Foundry keeps the last ${RETAINED}: the current one and the newest others; older ones are deleted from the database in the background.`}
      >
        {loads.data && <RefreshFailed query={loads} />}
        {failed ? (
          <EmptyState
            icon={DatabaseZap}
            title="Couldn’t load the loads"
            text={loads.error?.message}
            action={<Button onClick={() => void loads.refetch()}>Retry</Button>}
          />
        ) : (
          <>
            {error && !pending && (
              <p role="alert" className="hf-form-error">
                {error}
              </p>
            )}
            <DataGrid<FoundryLoad & { statusText: string }>
              label="Loads"
              loading={loads.isPending}
              rows={rows.map((l) => ({ ...l, statusText: l.status || "" }))}
              getRowKey={(l) => l.id}
              rowHeight={48}
              maxHeight={640}
              onRowClick={(l) => navigate(paths.load(o, l.id))}
              columns={[
                {
                  key: "version",
                  label: "Version",
                  width: 150,
                  sortValue: (l) => Number(loadVersion(l).slice(1)) || 0,
                  render: (l) => (
                    <span className="on-load-version">
                      <Link
                        to={paths.load(o, l.id)}
                        className="on-row-link"
                        onClick={(e) => e.stopPropagation()}
                      >
                        {loadVersion(l)}
                      </Link>
                      {l.current && (
                        <StatusBadge
                          status="healthy"
                          label="Current"
                          size="sm"
                        />
                      )}
                    </span>
                  ),
                },
                {
                  key: "statusText",
                  label: "Status",
                  width: 130,
                  render: (l) => {
                    const b = loadBadge(l);
                    return (
                      <StatusBadge
                        status={b.status}
                        label={b.label}
                        size="sm"
                      />
                    );
                  },
                },
                {
                  key: "createdAt",
                  label: "Created",
                  width: 120,
                  type: "timestamp",
                  render: (l) => (
                    <span title={formatDate(l.createdAt)}>
                      {formatRelative(l.createdAt || "", now)}
                    </span>
                  ),
                },
                {
                  key: "finishedAt",
                  label: "Finished",
                  width: 120,
                  type: "timestamp",
                  render: (l) =>
                    l.finishedAt ? (
                      <span title={formatDate(l.finishedAt)}>
                        {formatRelative(l.finishedAt, now)}
                      </span>
                    ) : (
                      <span className="on-muted">—</span>
                    ),
                },
                {
                  key: "nodes",
                  label: "Nodes",
                  width: 90,
                  type: "integer",
                  sortable: false,
                  render: (l) => (
                    <CountCell load={l} version={versionOf(l)} what="nodes" />
                  ),
                },
                {
                  key: "facts",
                  label: "Facts",
                  width: 90,
                  type: "integer",
                  sortable: false,
                  render: (l) => (
                    <CountCell load={l} version={versionOf(l)} what="facts" />
                  ),
                },
                {
                  key: "jobs",
                  label: "Jobs",
                  width: 280,
                  sortable: false,
                  render: (l) => (
                    <span
                      className="on-cell-text"
                      title={jobsText(l, ontology.createdAt, now)}
                    >
                      {jobsText(l, ontology.createdAt, now)}
                    </span>
                  ),
                },
                {
                  key: "retention",
                  label: "Retention",
                  width: 110,
                  sortValue: retentionText,
                  render: retentionText,
                },
                {
                  key: "actions",
                  label: <span className="hf-sr-only">Actions</span>,
                  className: "on-actions",
                  width: 130,
                  sortable: false,
                  render: (l) => {
                    // Open, then a menu of what else applies to the row.
                    const can = loadActions(l),
                      gone =
                        l.status === "deleting" || l.status === "delete_failed",
                      compare =
                        !!currentLoad &&
                        !l.current &&
                        l.status !== "writing" &&
                        !gone,
                      setCurrent = manage && can.setCurrent,
                      remove = manage && can.remove,
                      retry = manage && can.retryDelete;
                    // Their clicks and keys (Enter opens the row) stay theirs.
                    const stop = (e: { stopPropagation: () => void }) =>
                      e.stopPropagation();
                    return (
                      <span
                        className="on-row-actions"
                        onClick={stop}
                        onKeyDown={stop}
                      >
                        {!gone && (
                          <ButtonLink size="sm" variant="ghost" to={explore(l)}>
                            Open
                          </ButtonLink>
                        )}
                        {(compare || setCurrent || remove || retry) && (
                          <DropdownMenu
                            label={`More actions for ${loadVersion(l)}`}
                            align="end"
                            triggerClassName="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon"
                            trigger={
                              busy === l.id ? (
                                <LoaderCircle
                                  size={14}
                                  className="hf-spin"
                                  aria-hidden
                                />
                              ) : (
                                <MoreHorizontal size={14} aria-hidden />
                              )
                            }
                          >
                            {compare && (
                              <MenuItem
                                icon={GitCompareArrows}
                                to={explore(l, currentLoad)}
                              >
                                Compare with current
                              </MenuItem>
                            )}
                            {setCurrent && (
                              <MenuItem
                                icon={CircleCheck}
                                disabled={busy === l.id}
                                onSelect={() =>
                                  void act(
                                    l,
                                    () => setCurrentLoad(id, o, l.id),
                                    `${loadVersion(l)} is the current load`,
                                  )
                                }
                              >
                                Set current
                              </MenuItem>
                            )}
                            {remove && (
                              <MenuItem
                                icon={Trash2}
                                danger
                                onSelect={() => {
                                  setError("");
                                  setPending({ kind: "delete", load: l });
                                }}
                              >
                                Delete
                              </MenuItem>
                            )}
                            {retry && (
                              <MenuItem
                                icon={RotateCcw}
                                disabled={busy === l.id}
                                onSelect={() =>
                                  void act(
                                    l,
                                    () => deleteLoad(id, o, l.id),
                                    `Deleting ${loadVersion(l)} again`,
                                  )
                                }
                              >
                                Retry delete
                              </MenuItem>
                            )}
                          </DropdownMenu>
                        )}
                      </span>
                    );
                  },
                },
              ]}
            />
          </>
        )}
      </Section>
      {pending?.kind === "delete" && (
        <ConfirmDialog
          title={`Delete ${loadVersion(pending.load)} from ${formatDate(pending.load.createdAt, false)}?`}
          description="Foundry deletes everything this load wrote from the database, in the background. Edges you added to this load’s nodes are deleted with them. Your own data and the other loads stay."
          confirmLabel="Delete load"
          tone="danger"
          pending={busy === pending.load.id}
          onConfirm={() =>
            act(
              pending.load,
              () => deleteLoad(id, o, pending.load.id),
              `Deleting ${loadVersion(pending.load)}`,
            )
          }
          onCancel={() => setPending(null)}
        >
          {error && (
            <p role="alert" className="hf-form-error">
              {error}
            </p>
          )}
        </ConfirmDialog>
      )}
    </div>
  );
}

export function LoadPage({ loadId }: { loadId: string }) {
  const { id, role, notify } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    qc = useQueryClient();
  const [stopping, setStopping] = useState(false);
  const query = useQuery(ontologyQueries.load(id, o, loadId));
  const l = query.data?.load;
  // The job the page shows: the live one, else the newest that wrote or read.
  const j =
    l?.jobs?.find(isLiveJob) || l?.jobs?.find((x) => x.kind !== "delete");
  const documents = useQuery(documentQueries.list(id, o));
  const now = useNow(tick(isLiveJob(j)));
  const version = useQuery({
    ...ontologyQueries.version(id, o, l?.versionId || ""),
    enabled: !!l?.versionId,
  });
  const { tables } = useLoadPlan(version.data?.version);
  const rules = rulesOf(version.data?.version);
  // A load that just ended changed the ontology and its documents.
  const wasLive = useRef(false);
  useEffect(() => {
    if (isLiveJob(j)) wasLive.current = true;
    else if (j && wasLive.current) {
      wasLive.current = false;
      void qc.invalidateQueries({ queryKey: [id, "ontology", o] });
    }
  }, [j?.status]);
  // Documents are read while it runs: keep their states current.
  useEffect(() => {
    if (isLiveJob(j) && j?.phase === "documents")
      void qc.invalidateQueries({ queryKey: [id, "ontology", o, "documents"] });
  }, [j?.progress?.documentsDone, j?.phase]);
  const crumbs = [
    { label: "Ontologies", to: paths.ontologies() },
    { label: ontologyName(ontology), to: paths.ontology(o) },
    { label: "Data loads", to: paths.loads(o) },
  ];
  if (query.isPending)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Load" />
        <SkeletonText lines={6} />
      </Page>
    );
  if (!l || !j)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Load unavailable" />
        <EmptyState
          icon={DatabaseZap}
          title={
            query.error instanceof ApiError && query.error.status === 404
              ? "This load was deleted, or isn’t this ontology’s"
              : "Couldn’t load this load"
          }
          text={query.error?.message}
          action={<ButtonLink to={paths.loads(o)}>All loads</ButtonLink>}
        />
      </Page>
    );
  // Its People enrichment step: the load job's.
  const e = j.kind === "load" ? l.enrichment : undefined,
    looking = lookingUp(j, e),
    outcome =
      l.enrichment && !l.jobs?.some((x) => x.kind === "load" && isLiveJob(x))
        ? enrichmentOutcome(l.enrichment, Date.now(), stoppedWaiting(l))
        : undefined;
  const stop = async () => {
    setStopping(true);
    try {
      await stopLoadEnrichment(id, o, l.id);
      notify("Stopping the lookups");
      await qc.invalidateQueries({ queryKey: [id, "ontology", o] });
    } catch (err) {
      const message = (err as Error).message;
      notify(message.charAt(0).toUpperCase() + message.slice(1), true);
    } finally {
      setStopping(false);
    }
  };
  const badge = jobBadge(j, e),
    p = j.progress || {},
    phase = ["provisioning", "ingest", "documents", "finishing"].indexOf(
      j.phase || "provisioning",
    );
  // Its tables, read during the ingest, and the documents, read after. An
  // extract reads only documents uploaded since the tables were loaded.
  const extract = j.kind === "extract";
  const tableItems: FeedItem[] = (extract ? [] : tables).map((t) => ({
    id: t.sourceId,
    name: t.info?.table || t.sourceId,
    source: t.info?.kind || "upload",
    group: t.info?.connector || "Tables",
    rows: t.info?.rows,
    status:
      j.status === "succeeded" || (j.status !== "queued" && phase > 1)
        ? "ready"
        : j.status === "running" && phase === 1
          ? "loading"
          : "pending",
  }));
  const uploaded = (documents.data?.documents || []).filter(
    (d) => d.status === "uploaded",
  );
  const docItems: FeedItem[] = uploaded.map((d) => ({
    id: d.id,
    name: d.name || d.id,
    source: "upload",
    group: "Documents",
    // A document's extraction is the current load's; another load's
    // documents are as far as its job got.
    status: (l.current ? !!d.extraction : j.status === "succeeded")
      ? "ready"
      : j.status === "running" && j.phase === "documents"
        ? "loading"
        : "pending",
  }));
  const reviews = uploaded.reduce(
    (n, d) => n + (d.extraction?.reviews || 0),
    0,
  );
  // The WFE's words for a retry whose tables were re-synced meanwhile.
  const changed = /synced again while this load was written/i.test(
    j.error || "",
  );
  const retry = loadRetry(j, ontology.createdAt, now);
  const loadBadgeShown = loadBadge(l);
  return (
    <Page>
      <PageHeader
        breadcrumbs={[...crumbs, { label: "Load" }]}
        eyebrow="Data load"
        title={`Version ${version.data?.version?.version ?? loadVersion(l).replace(/^v/, "")}`}
        documentTitle="Load"
        meta={
          <>
            <StatusBadge
              status={loadBadgeShown.status}
              label={loadBadgeShown.label}
            />
            {l.current && <StatusBadge status="healthy" label="Current" />}
            <span title={formatDate(j.createdAt)}>
              Requested {formatRelative(j.createdAt || "", now)}
            </span>
            {jobDuration(j, now) && <span>Took {jobDuration(j, now)}</span>}
            <span>{formatUsd(j.usdMicros)} spent</span>
          </>
        }
        actions={
          (l.status === "ready" || l.status === "partial") && (
            <>
              {rules && (
                <ButtonLink
                  icon={ShieldAlert}
                  to={paths.loadRuleBreaks(o, l.id)}
                >
                  Rule breaks
                </ButtonLink>
              )}
              <ButtonLink
                variant="primary"
                iconRight={ArrowRight}
                to={withLoad(paths.explore(o), l.current ? null : l.id)}
              >
                Open Explorer
              </ButtonLink>
            </>
          )
        }
      />
      <div className="on-stack">
        {looking && (
          <Callout
            icon={UserSearch}
            live
            title="Looking up people"
            text={`${enrichmentStepText(e!)}. Tables load next.`}
            action={
              canManage(role) && (
                <Button
                  size="sm"
                  pending={stopping}
                  onClick={() => void stop()}
                >
                  Stop looking up
                </Button>
              )
            }
          />
        )}
        {outcome && (
          <Callout
            icon={UserSearch}
            tone="muted"
            title="People enrichment"
            text={outcome.text}
            action={
              outcome.cap && (
                <ButtonLink size="sm" to={paths.settings("enrichment")}>
                  Credit cap
                </ButtonLink>
              )
            }
          />
        )}
        {retry?.kind === "starting" && (
          <Callout
            icon={LoaderCircle}
            live
            title="The new database is still starting"
            text={`${ontology.databaseName || "The database"} doesn’t take writes yet, which can last a few minutes after it’s created. The load tries again by itself once it does.`}
          />
        )}
        {retry?.kind === "failed" && (
          <Callout
            icon={RotateCcw}
            title="An attempt failed. Helix will try again"
            text={
              retry.reason.charAt(0).toUpperCase() + retry.reason.slice(1) + "."
            }
          />
        )}
        {j.status === "failed" && (
          <Callout
            icon={TriangleAlert}
            tone="danger"
            title={
              changed ? "The tables changed while it ran" : "The load failed"
            }
            text={
              changed ? (
                "Its tables were synced again while it loaded them, so it stopped. Start a new load: it reads the tables as they are now, beside this one."
              ) : j.error ? (
                // The WFE's copy may say where an owner fixes it.
                <SettingsLinked text={sentence(j.error)} />
              ) : (
                "It stopped without saying why. What it wrote before stopping stays in the database until the load is deleted."
              )
            }
            action={
              changed &&
              version.data?.version?.isCurrent && (
                <LoadButton
                  version={version.data.version}
                  size="sm"
                  variant="secondary"
                />
              )
            }
          />
        )}
        {j.status === "budget_exhausted" && (
          <Callout
            icon={CircleGauge}
            tone="muted"
            title="It stopped at the spending cap"
            text={budgetStopText(j)}
          />
        )}
        {j.status === "succeeded" && (
          <Callout
            icon={CircleCheck}
            tone="muted"
            title="Loaded"
            text={`${extract ? "" : `${plural(p.chunksDone || 0, "chunk")} of table rows and `}${plural(p.documentsDone || 0, "document")} written into ${ontology.databaseName || "the database"}${reviews ? `; ${plural(reviews, "item")} from your documents ${reviews === 1 ? "needs" : "need"} review` : ""}.`}
            action={
              reviews > 0 && (
                <ButtonLink size="sm" to={paths.documents(o, "review")}>
                  Review documents
                </ButtonLink>
              )
            }
          />
        )}
        <DataFeed
          items={[...tableItems, ...docItems]}
          steps={loadSteps(j, tableItems.length, e, stoppedWaiting(l))}
          unit={{ one: "source", many: "sources" }}
        />
        <Panel title="Details">
          <KeyValue
            columns={2}
            items={[
              {
                label: "Step",
                value: isLiveJob(j) && !looking ? phaseLabel(j) : badge.label,
              },
              { label: "Reads", value: kindLabel(j) },
              {
                label: "Started",
                value: j.startedAt && formatDate(j.startedAt),
              },
              {
                label: "Finished",
                value: j.finishedAt && formatDate(j.finishedAt),
              },
              { label: "Took", value: jobDuration(j, now) },
              { label: "Chunks loaded", value: String(p.chunksDone || 0) },
              {
                label: "Documents read",
                // What the ontology reads, and how its current load holds each.
                value: (
                  <Link
                    to={paths.documents(o)}
                    title="The ontology’s documents"
                  >
                    {`${p.documentsDone || 0}${p.documentsTotal ? ` of ${p.documentsTotal}` : ""}`}
                  </Link>
                ),
              },
              {
                label: "Spent reading documents",
                value: formatUsd(j.usdMicros),
              },
              ...(l.enrichment
                ? [
                    {
                      label: "Lookup credits",
                      value: formatNumber(Number(l.enrichment.credits || 0)),
                    },
                  ]
                : []),
              { label: "Database", value: ontology.databaseName },
              { label: "Load ID", value: l.id, mono: true },
            ]}
          />
        </Panel>
        <Panel
          title="Jobs"
          subtitle={
            jobTotals(l).count > (l.jobs?.length || 0)
              ? `The newest ${l.jobs?.length || 0} of the ${jobTotals(l).count} jobs run for this load.`
              : "Everything run for this load, newest first."
          }
        >
          <DataGrid<GraphJob>
            label="Jobs"
            rows={l.jobs || []}
            getRowKey={(x) => x.id}
            rowHeight={40}
            columns={[
              {
                key: "kind",
                label: "Job",
                width: 180,
                render: (x) => kindLabel(x),
              },
              {
                key: "status",
                label: "Status",
                width: 160,
                render: (x) => {
                  const b = jobBadge(
                    x,
                    x.kind === "load" ? l.enrichment : undefined,
                  );
                  return (
                    <StatusBadge status={b.status} label={b.label} size="sm" />
                  );
                },
              },
              {
                key: "createdAt",
                label: "Requested",
                width: 140,
                type: "timestamp",
                render: (x) => formatRelative(x.createdAt || "", now),
              },
              {
                key: "documents",
                label: "Documents read",
                width: 130,
                type: "integer",
                sortValue: (x) => x.progress?.documentsDone || 0,
                render: (x) => x.progress?.documentsDone || 0,
              },
              {
                key: "usdMicros",
                label: "Cost",
                width: 90,
                type: "number",
                sortValue: (x) => Number(x.usdMicros || 0),
                render: (x) => formatUsd(x.usdMicros),
              },
              {
                key: "error",
                label: "Result",
                width: 220,
                sortable: false,
                render: (x) =>
                  loadRetry(x, ontology.createdAt, now)?.kind === "starting" ? (
                    <span
                      className="on-muted on-cell-text"
                      title="The new database is still starting; the load tries again by itself once it takes writes."
                    >
                      Database starting
                    </span>
                  ) : x.error ? (
                    <span
                      className="on-error-text on-cell-text"
                      title={x.error}
                    >
                      {x.error}
                    </span>
                  ) : (
                    <span className="on-muted">—</span>
                  ),
              },
            ]}
          />
        </Panel>
      </div>
    </Page>
  );
}
