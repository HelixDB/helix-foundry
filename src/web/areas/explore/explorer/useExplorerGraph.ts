import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import type {
  DatabaseQueryResult,
  DynamicQueryRequest,
  QueryExecutor,
} from "../database-query";
import {
  FOUNDRY_EDGE_LABELS,
  FOUNDRY_NODE_LABELS,
} from "../../../../shared/foundry-labels";
import { isShared, type Scope } from "../load-scope";
import type { IndexCatalogEntry, LabelExclusion } from "./lib/types";
import { excludes, shownGraph, shownLabels, shownStats } from "./lib/exclusion";

import {
  canvasRoom,
  connectBatches,
  expansionKey,
  explorerReducer,
  hiddenNodeIds,
  indexLabels,
  initialExplorerState,
  labelKey,
  connectCost,
  connectCostFloor,
  mergeGraph,
  needsEdgeCount,
  nodeSideSize,
  nodeSize,
  planConnect,
  railLabels,
  sideCursorAfter,
  textIndexesFor,
  containsTargetsFor,
  parseIdCandidate,
  UNSTARTED_EXPANSION,
  visibleGraph,
  type ExplorerAction,
  type ExplorerNotice,
  type ExplorerState,
  type MergeNodeOptions,
  type MergeResult,
  type QueryFailure,
  type SideCursor,
} from "./lib/graph-store";
import { extractGraphFromResult, nodeKey } from "./lib/normalize";
import {
  moreRing,
  newSeedingState,
  seedView,
  SEEDING,
  type SeedingIO,
  type VersionLayout,
} from "./lib/seeding";
import {
  buildConnectRequest,
  buildDegreeProbeRequest,
  buildDegreeRequest,
  buildEdgeCountRequest,
  buildIdentityRequest,
  buildLabelCountsRequest,
  buildLabelDiscoveryRequest,
  buildLabelPageRequest,
  buildLabelScanRequest,
  buildNeighborhoodRequest,
  buildNodesByIdRequest,
  buildSampleRequest,
  buildSearchRequest,
  countedLabels,
  emptyLabels,
  EXPLORER_LIMITS,
  PROBE_SLOW_MS,
  extendShape,
  fallbackShape,
  fetchesEdgeProperties,
  isLargeProbe,
  isLightShape,
  lightShape,
  parseConnect,
  parseDegree,
  parseDegreeProbe,
  parseEdgeCount,
  parseIdentity,
  parseLabelCounts,
  parseLabelDiscovery,
  parseLabelPage,
  parseLabelScan,
  parseNeighborhood,
  parseNodes,
  parseSample,
  parseSchemaProbe,
  parseSearch,
  withoutEdgeProperties,
  type LabelDiscovery,
} from "./lib/queries";
import {
  createRequestLimiter,
  delay,
  RATE_LIMIT_RETRY_DELAYS_MS,
  unlimited,
  type RequestLimiter,
  type RequestPacing,
} from "./lib/request-limiter";
import {
  asStringArray,
  readRememberedLabels,
  readStored,
  rememberLabels,
  writeStored,
} from "./lib/storage";
import type {
  DegreeInfo,
  DegreeProbe,
  GraphData,
  GraphNode,
  LabelCount,
  LabelSchema,
  LabelStats,
  NodeFetchShape,
  SearchHit,
} from "./lib/types";

/**
 * The explorer's data layer: one store of nodes and edges, the requests that
 * fill it, and the state the panels read. Every request goes through the
 * injected executor with its own AbortController; a refresh or unmount aborts
 * everything in flight and bumps a generation counter so late responses are
 * dropped. Operations report their own progress (no shared loading flag) and
 * background failures go to `notify`.
 */

export interface ExplorerNotifyEvent {
  tone: "info" | "error";
  title: string;
  description?: string;
  failure?: QueryFailure;
}

export interface UseExplorerGraphOptions {
  execute: QueryExecutor;
  /**
   * What the explorer reads (../load-scope.ts): one load, whose labels are
   * its version's (`layout`) and whose first view is seeded from its hub
   * types, or Other data, found and sampled as a plain database is.
   * Changing it reboots.
   */
  scope: Scope;
  /** The load's version: its types and links, for the rail and the first view. */
  layout?: VersionLayout;
  /**
   * The database's limiter, shared with the other readers of the database
   * (../tenant-reads.tsx); without one the explorer makes its own.
   */
  limiter?: RequestLimiter;
  /** How many times k text search reads, to filter its hits to the scope (loads + 1). */
  textOverFetch?: number;
  canRead: boolean | null;
  indexes?: IndexCatalogEntry[];
  /** localStorage namespace (`helix.explorer.tenant:<id>`); hidden labels persist under it. */
  storageKey: string;
  /** Background events worth telling the user about: a failed expansion, nothing more to load. */
  notify?: (event: ExplorerNotifyEvent) => void;
  /**
   * How the explorer paces its own requests (see request-limiter.ts); false
   * turns pacing off (an injected transport without the gateway's limits).
   */
  pacing?: Partial<RequestPacing> | false;
  /** Waits before retrying a read the database rate-limited; the default is 1 s, 2 s, 4 s. */
  retryDelaysMs?: readonly number[];
  /** Waits before counting labels again after a background count failed; see LATE_COUNT_RETRY_DELAYS_MS. */
  lateCountRetryDelaysMs?: readonly number[];
  /**
   * Labels to leave out of everything (types.ts LabelExclusion): never
   * counted, loaded, searched or expanded over. Changing it reboots.
   */
  exclude?: LabelExclusion;
}

export type ExpandDirection = "both" | "out" | "in";

export interface ExpandOptions {
  direction?: ExpandDirection;
  edgeLabel?: string | null;
  /**
   * The user confirmed expanding a node known to be large (more than
   * `degreeThreshold` relationships on a side it reads) or whose size
   * couldn't be checked (the probe failed). Without it, such an expansion
   * only posts a warning notice with an "Expand anyway" action.
   */
  confirmed?: boolean;
}

export interface SearchOutcome {
  hits: SearchHit[];
  /** The first source that failed; hits from the sources that worked are still listed. */
  failure: QueryFailure | null;
}

export interface SearchOptions {
  /**
   * Called with the id and text-index hits as soon as they arrive, while the
   * slower property scan (`contains`) is still running. The promise still
   * resolves with every hit.
   */
  onPartial?: (outcome: SearchOutcome) => void;
}

export interface ShowResultOutcome {
  nodeIds: string[];
  edgeIds: string[];
}

export interface ExplorerGraph {
  state: ExplorerState;
  /** What the canvas draws: hidden labels and nodes filtered out. Stable while nothing changes. */
  visible: GraphData;
  /** Label counts for the rail: the counted labels, plus labels loaded since (count unknown). */
  labels: { nodes: LabelCount[]; edges: LabelCount[] };
  refresh(options?: { small?: boolean }): void;
  /** Finds and counts labels again, the cheap way (no full scan). */
  retryOverview(): void;
  /**
   * Lists every label with exact counts by reading the whole graph. Slow and
   * heavy on a large database: only ever run because the user asked.
   */
  scanLabels(): Promise<boolean>;
  loadSample(): void;
  /** In a load: reads the next ring around the outermost nodes, within a budget. */
  showMore(): Promise<void>;
  /** Whether showMore has nodes left to read around. */
  canShowMore: boolean;
  expand(nodeId: string, options?: ExpandOptions): Promise<void>;
  loadMore(
    nodeId: string,
    direction: "out" | "in",
    edgeLabel?: string | null,
  ): Promise<void>;
  loadLabelPage(label: string): Promise<void>;
  loadFullNode(nodeId: string): Promise<void>;
  /**
   * A node's degree: the cheap probe (no edges read), then exact per-label
   * counts of the sides under `degreeThreshold` (a bounded read). Large sides
   * stay "1,000+" until countRelationships. `view` is who asked: the
   * inspector (the selected node, the default) or a context menu (while
   * edge labels are hidden). Each keeps one read in flight, so a newer one
   * cancels its own older read, never the other's.
   */
  fetchDegree(nodeId: string, view?: "inspector" | "menu"): void;
  /** Only the cheap probe (what opening a context menu runs while every edge label shows). */
  probeDegree(nodeId: string): void;
  /** Exact relationship counts per edge label, reading every edge of the node: explicit only. */
  countRelationships(nodeId: string): Promise<void>;
  /**
   * The explicit "Connect": checks the nodes' sizes and edge counts, connects
   * the small ones in budgeted requests, and asks again (a warning notice)
   * before the large ones and those it couldn't check. `confirmed` (that
   * warning's "Connect anyway") connects them all, one large node per request.
   */
  connectNodes(
    nodeIds: string[],
    options?: { confirmed?: boolean },
  ): Promise<void>;
  /** Until when (epoch ms) "Scan for all labels" is held back because an earlier scan may still be running. */
  scanBlockedUntil: number | null;
  /**
   * The executor for queries the user runs (the query dock): through the
   * explorer's limiter as user requests (a token, then at once), so the
   * explorer's own requests that follow wait their turn. A read the database
   * rate-limits is retried like the explorer's; a write never is.
   */
  executeUserQuery: QueryExecutor;
  search(text: string, options?: SearchOptions): Promise<SearchOutcome | null>;
  /**
   * Puts a node (a search hit) on the canvas with its first hop when it is
   * known to be small; a large one, or one whose size couldn't be checked,
   * gets notices offering to connect and expand it instead.
   */
  revealNode(node: GraphNode): Promise<string | null>;
  /**
   * Puts the node of `label` with this identity (`entity_key` or
   * `norm_name`) in the scope on the canvas, as a search hit is (compare
   * focuses the same entity on both sides); null when the scope has none.
   */
  revealIdentity(
    label: string,
    property: "entity_key" | "norm_name",
    value: string,
  ): Promise<string | null>;
  /**
   * Loads a node by database id (deep links): the node only, with its size
   * probed in the same request. No expansion; it is connected to the canvas
   * only when it is small.
   */
  loadNodeById(dbId: number): Promise<string | null>;
  showResult(body: unknown): Promise<ShowResultOutcome | null>;
  hideNodes(ids: string[]): void;
  setLabelHidden(target: "node" | "edge", label: string, hidden: boolean): void;
  showAll(): void;
  removeNodes(ids: string[]): void;
  clearCanvas(): void;
  setPinned(nodeId: string, pinned: boolean): void;
  dismissNotice(id: string): void;
  isPending(op: string): boolean;
}

const CAP = EXPLORER_LIMITS.maxCanvasNodes;
const SMALL_SAMPLE_NODES = 100;
const SMALL_SAMPLE_EDGES = 300;
/** Nodes per property refetch with a probed shape (light values only). */
const REFETCH_BATCH = 100;
/** Nodes per refetch of full rows (no probe), which may carry heavy values. */
const REFETCH_FULL_BATCH = 25;
const REFETCH_MIN_BATCH = 5;
const LABEL_PAGE_MAX_REQUESTS = 4;
/**
 * Pages one "load more" of a side known to be small may fetch while every
 * edge it returns is already loaded (any other side gets one page per click).
 */
const LOAD_MORE_MAX_REQUESTS = 4;
const MAX_MISSING_ENDPOINTS = 500;
const SEARCH_KEY = "search";
const SEARCH_SCAN_KEY = "search:contains";

function isTooLarge(failure: QueryFailure): boolean {
  return (
    failure.kind === "too_large" ||
    (failure.kind === "database" &&
      (failure.code === "payload_too_large" || failure.status === 413))
  );
}

function isTimeout(failure: QueryFailure): boolean {
  return (
    (failure.kind === "database" &&
      (failure.code === "query_timeout" ||
        failure.code === "query_deadline_exceeded" ||
        failure.status === 408)) ||
    (failure.kind === "unavailable" && failure.status === 504)
  );
}

/**
 * The database's own rate limit (inner 429 `rate_limited`): worth retrying a
 * read after a short wait. A WFE 429 throttle is "unavailable" and halts instead.
 */
function isRateLimited(result: DatabaseQueryResult): boolean {
  return (
    !result.ok &&
    result.kind === "database" &&
    (result.code === "rate_limited" ||
      (result.status === 429 && result.code === null))
  );
}

/** Failures after which a label scan may still be running on the database. */
function scanMayStillRun(failure: QueryFailure): boolean {
  return (
    isTimeout(failure) ||
    failure.kind === "network" ||
    failure.kind === "aborted"
  );
}

/** How long "Scan for all labels" is held back after a scan that may still be running. */
const SCAN_COOLDOWN_MS = 60_000;

function scanCooldownKey(storageKey: string): string {
  return `${storageKey}.scanCooldownUntil`;
}

const asTimestamp = (value: unknown) =>
  typeof value === "number" && Number.isFinite(value) ? value : null;

/** Gateway codes that mean the database (or its gateway) is refusing work right now. */
const REFUSING_CODES = new Set([
  "rate_limit_unavailable",
  "backend_unavailable",
  "rate_limited",
]);

/**
 * Failures that stop the boot on the spot, with no fallback request: the
 * gateway refusing queries or the database unavailable (more requests only
 * add load), a timeout on a request that is cheap by construction (the
 * database is struggling), and everything outside the database (access,
 * billing, network, the broker), which another request would hit the same way.
 */
function haltsBoot(failure: QueryFailure): boolean {
  if (isTimeout(failure)) return true;
  if (failure.kind !== "database") return !isTooLarge(failure);
  return (
    (failure.code !== null && REFUSING_CODES.has(failure.code)) ||
    [429, 502, 503, 504].includes(failure.status)
  );
}

const formatLabelCount = (count: number) =>
  `${count.toLocaleString("en-US")} ${count === 1 ? "label" : "labels"}`;

/** The notice that offers to connect nodes the explorer didn't connect on its own. */
const CONNECT_NOTICE_ID = "connect-skipped";
/** The warning before connecting nodes that are large (or of unknown size), once "Connect" checked them. */
const CONNECT_LARGE_NOTICE_ID = "connect-large";
/** What a connect with nothing to connect to resolves with (no request sent). */
const NOTHING_TO_CONNECT: DatabaseQueryResult = {
  ok: true,
  status: 200,
  contentType: null,
  body: null,
  bytes: 0,
  elapsedMs: 0,
};
/**
 * New nodes the explorer considers connecting on its own at a time (one
 * connect request takes at most `connectEdgeBudget` edges anyway); the rest
 * are offered in the notice, whose "Connect" checks them on demand.
 */
const AUTO_CONNECT_MAX = EXPLORER_LIMITS.pageSize;
const expandNoticeId = (nodeId: string) => `expand-large:${nodeId}`;

/** Per-side keys for labels counted since the boot (a name can be both a node and an edge label). */
const countedKey = (target: "node" | "edge", label: string) =>
  `${target}:${label}`;

/**
 * The labels to count at boot, in priority order: what discovery saw (they
 * certainly exist), then the index catalog, then labels remembered from
 * earlier visits. Over the per-request cap, the later ones go uncounted.
 */
function bootLabels(
  discovery: LabelDiscovery | null,
  catalog: { nodes: string[]; edges: string[] },
  remembered: { nodes: string[]; edges: string[] },
): { nodes: string[]; edges: string[] } {
  const order = (...lists: string[][]) => [
    ...new Set(lists.flat().filter((label) => label !== "")),
  ];
  return {
    nodes: order(discovery?.nodes ?? [], catalog.nodes, remembered.nodes),
    edges: order(discovery?.edges ?? [], catalog.edges, remembered.edges),
  };
}

/**
 * A load's labels: its version's types and links, and Foundry's own, less
 * what is excluded (Foundry's, unless shown).
 */
function loadLabels(
  layout: VersionLayout | undefined,
  exclude: LabelExclusion | undefined,
): { nodes: string[]; edges: string[] } {
  return shownLabels(
    {
      nodes: [...(layout?.types ?? []), ...FOUNDRY_NODE_LABELS],
      edges: [
        ...(layout?.links.map((link) => link.name) ?? []),
        ...FOUNDRY_EDGE_LABELS,
      ],
    },
    exclude,
  );
}

/**
 * Labels a counts request covered that its stats left out: counted at zero
 * (parseLabelCounts drops those), so gone or never written.
 */
function emptyLabelsOf(
  asked: { nodes: readonly string[]; edges: readonly string[] },
  stats: LabelStats,
): { nodes: string[]; edges: string[] } {
  const missing = (list: readonly string[], kept: LabelCount[]) => {
    const exists = new Set(kept.map((entry) => entry.label));
    return list.filter((label) => !exists.has(label));
  };
  return {
    nodes: missing(asked.nodes, stats.nodeLabels),
    edges: missing(asked.edges, stats.edgeLabels),
  };
}

/** Late label counting: labels per request and requests per boot. */
const LATE_COUNT_DELAY_MS = 400;
const LATE_COUNT_MAX_REQUESTS = 4;
/**
 * Waits before counting a batch of labels again after its background count
 * failed (or Clear cancelled it), one per consecutive failure; after the last
 * one, background counting stops until a refresh.
 */
export const LATE_COUNT_RETRY_DELAYS_MS: readonly number[] = Object.freeze([
  5_000, 15_000, 45_000,
]);

/**
 * Failures a text index can cause (missing from the database, partitioned by
 * a tenant property, too slow or too large), after which the id lookup is
 * worth running without it.
 */
function textSourceFailure(failure: QueryFailure): boolean {
  return (
    failure.kind === "database" || isTooLarge(failure) || isTimeout(failure)
  );
}

/** Hits from several requests in order, a node found twice keeping its first hit. */
function combineHits(...lists: SearchHit[][]): SearchHit[] {
  const seen = new Set<string>();
  const hits: SearchHit[] = [];
  for (const hit of lists.flat()) {
    if (seen.has(hit.node.id)) continue;
    seen.add(hit.node.id);
    hits.push(hit);
  }
  return hits;
}

function hiddenLabelsKey(storageKey: string): string {
  return `${storageKey}.hiddenLabels`;
}

function caption(node: GraphNode): string {
  return `${node.label} #${node.dbId}`;
}

type SampleOutcome =
  | { kind: "loaded"; graph: GraphData; truncated: boolean; light: boolean }
  | { kind: "failed"; failure: QueryFailure };

export function useExplorerGraph(
  options: UseExplorerGraphOptions,
): ExplorerGraph {
  const { storageKey } = options;
  const [state, setState] = useState<ExplorerState>(() =>
    initialExplorerState(
      readStored(hiddenLabelsKey(storageKey), asStringArray) ?? [],
    ),
  );
  const stateRef = useRef(state);
  const optionsRef = useRef(options);
  useLayoutEffect(() => {
    optionsRef.current = options;
  });

  const generationRef = useRef(0);
  /**
   * The database was seen to hold excluded labels (found, remembered or
   * scanned since the boot): the counted labels don't cover every node, so
   * a sample never reads "the whole graph".
   */
  const excludedSeenRef = useRef(false);
  const controllersRef = useRef(new Set<AbortController>());
  const keyedRef = useRef(new Map<string, AbortController>());
  const busyRef = useRef(new Map<string, string>());
  const sequenceRef = useRef(0);

  const dispatch = useCallback((action: ExplorerAction) => {
    const next = explorerReducer(stateRef.current, action);
    if (next === stateRef.current) return;
    stateRef.current = next;
    setState(next);
  }, []);

  const notify = useCallback((event: ExplorerNotifyEvent) => {
    if (event.failure?.kind === "aborted") return;
    optionsRef.current.notify?.(event);
  }, []);

  const abortAll = useCallback(() => {
    for (const controller of controllersRef.current) controller.abort();
    controllersRef.current.clear();
    keyedRef.current.clear();
    busyRef.current.clear();
  }, []);

  // One limiter per database: the gateway's rate limit is per database, so
  // the explorer shares the one its page gives it.
  const [ownLimiter] = useState<RequestLimiter>(() =>
    options.pacing === false
      ? unlimited()
      : createRequestLimiter(options.pacing ?? {}),
  );
  const limiter =
    options.pacing === false ? ownLimiter : (options.limiter ?? ownLimiter);

  /**
   * Runs one request and logs it as explorer activity. Returns null when the
   * response no longer matters: aborted, superseded under the same key, or
   * from before a refresh. Automatic requests (the default) wait their turn
   * in the limiter; `automatic: false` marks one the user asked for, which
   * goes at once unless `paced` (a user action that sends several heavy
   * requests, which should still wait their turn). A read the database
   * rate-limits is retried after 1 s, 2 s and 4 s; writes never are (the
   * explorer only reads).
   */
  const run = useCallback(
    async (
      title: string,
      request: DynamicQueryRequest,
      opts: { key?: string; automatic?: boolean; paced?: boolean } = {},
    ): Promise<DatabaseQueryResult | null> => {
      const generation = generationRef.current;
      if (opts.key) keyedRef.current.get(opts.key)?.abort();
      const controller = new AbortController();
      controllersRef.current.add(controller);
      if (opts.key) keyedRef.current.set(opts.key, controller);
      const paced = opts.paced ?? opts.automatic !== false;
      const retryDelays =
        request.request_type === "read"
          ? (optionsRef.current.retryDelaysMs ?? RATE_LIMIT_RETRY_DELAYS_MS)
          : [];
      const aborted = (): DatabaseQueryResult => ({
        ok: false,
        kind: "aborted",
        status: 0,
        code: null,
        message: "The query was cancelled.",
        elapsedMs: 0,
      });

      const attempt = async (retry: number): Promise<DatabaseQueryResult> => {
        const release = await limiter.acquire({
          automatic: paced,
          signal: controller.signal,
        });
        if (!release) return aborted();
        const id = `explorer-${Date.now().toString(36)}-${(sequenceRef.current += 1)}`;
        dispatch({
          type: "activity/start",
          entry: {
            id,
            title: retry > 0 ? `${title} (retry ${retry})` : title,
            request,
            at: Date.now(),
            ok: null,
            status: null,
            elapsedMs: null,
          },
        });
        let result: DatabaseQueryResult;
        try {
          result = await optionsRef.current.execute(request, {
            signal: controller.signal,
          });
        } catch (error) {
          // Executors return failures instead of throwing; this keeps a broken one from wedging an operation.
          result = {
            ok: false,
            kind: controller.signal.aborted ? "aborted" : "network",
            status: 0,
            code: null,
            message:
              error instanceof Error ? error.message : "The query failed.",
            elapsedMs: 0,
          };
        } finally {
          release();
        }
        const cancelled =
          controller.signal.aborted ||
          (!result.ok && result.kind === "aborted");
        dispatch({
          type: "activity/end",
          id,
          ok: cancelled ? null : result.ok,
          status: result.status || null,
          elapsedMs: result.elapsedMs,
        });
        return result;
      };

      let result: DatabaseQueryResult;
      try {
        for (let retry = 0; ; retry += 1) {
          result = await attempt(retry);
          if (
            !isRateLimited(result) ||
            retry >= retryDelays.length ||
            controller.signal.aborted
          )
            break;
          if (!(await delay(retryDelays[retry], controller.signal))) {
            result = aborted();
            break;
          }
        }
      } finally {
        controllersRef.current.delete(controller);
        if (opts.key && keyedRef.current.get(opts.key) === controller)
          keyedRef.current.delete(opts.key);
      }
      const cancelled =
        controller.signal.aborted || (!result.ok && result.kind === "aborted");
      if (cancelled || generation !== generationRef.current) return null;
      return result;
    },
    [dispatch, limiter],
  );

  const executeUserQuery = useCallback<QueryExecutor>(
    async (request, opts = {}) => {
      const { signal } = opts;
      const retryDelays =
        request.request_type === "read"
          ? (optionsRef.current.retryDelaysMs ?? RATE_LIMIT_RETRY_DELAYS_MS)
          : [];
      const cancelled = (): DatabaseQueryResult => ({
        ok: false,
        kind: "aborted",
        status: 0,
        code: null,
        message: "The query was cancelled.",
        elapsedMs: 0,
      });
      for (let retry = 0; ; retry += 1) {
        const release = await limiter.acquire({ automatic: false, signal });
        if (!release) return cancelled();
        let result: DatabaseQueryResult;
        try {
          result = await optionsRef.current.execute(request, opts);
        } finally {
          release();
        }
        if (
          !isRateLimited(result) ||
          retry >= retryDelays.length ||
          signal?.aborted
        )
          return result;
        if (!(await delay(retryDelays[retry], signal))) return cancelled();
      }
    },
    [limiter],
  );

  /** Runs an operation at most once at a time per key, and shows it as pending meanwhile. */
  const track = useCallback(
    async <T>(op: string, task: () => Promise<T>): Promise<T | undefined> => {
      if (busyRef.current.has(op)) return undefined;
      const id = `${op}#${(sequenceRef.current += 1)}`;
      busyRef.current.set(op, id);
      dispatch({ type: "pending/start", op, id });
      try {
        return await task();
      } finally {
        if (busyRef.current.get(op) === id) busyRef.current.delete(op);
        dispatch({ type: "pending/end", op, id });
      }
    },
    [dispatch],
  );

  /**
   * Merges into the current store, first removing hidden nodes when the new
   * ones would not fit under the cap: hiding is how users make room.
   */
  const admit = useCallback(
    (all: Partial<GraphData>, opts: MergeNodeOptions = {}): MergeResult => {
      // Excluded labels never reach the store, whichever read brought them.
      const graph = shownGraph(all, optionsRef.current.exclude);
      const current = stateRef.current;
      const incoming = new Set((graph.nodes ?? []).map((node) => node.id));
      let fresh = 0;
      for (const id of incoming) if (!current.store.nodes.has(id)) fresh += 1;
      const overflow = current.store.nodes.size + fresh - CAP;
      if (overflow > 0) {
        const evict = hiddenNodeIds(current, incoming).slice(0, overflow);
        if (evict.length > 0) dispatch({ type: "nodes/remove", ids: evict });
      }
      return mergeGraph(stateRef.current.store, graph, { cap: CAP, ...opts });
    },
    [dispatch],
  );

  const mergeInto = useCallback(
    (graph: Partial<GraphData>, opts: MergeNodeOptions = {}): MergeResult => {
      const merge = admit(graph, opts);
      dispatch({ type: "merged", merge, cap: CAP });
      return merge;
    },
    [admit, dispatch],
  );

  const capReached = useCallback(() => {
    notify({
      tone: "info",
      title: "The canvas is full",
      description: `It holds up to ${CAP.toLocaleString("en-US")} nodes. Hide or clear some to load more.`,
    });
  }, [notify]);

  /**
   * Probes the size of nodes whose size isn't known yet, `probeBatch` per
   * request (no edges read, but each node costs time in its adjacency size),
   * and stores the results. With `stopEarly` (what the explorer does on its
   * own) it stops after a batch that found a large node or ran slow: the
   * nodes after it are likely more of the same (a label page of hubs, the
   * category hubs around a node), so they stay unknown and are not read on
   * their own. A failed batch stops it too. With `budget` it also stops once
   * the edges connecting `nodeIds` would read are known to reach it (see
   * connectCostFloor): one connect request couldn't take more. Resolves false
   * when the response no longer matters.
   */
  const probeNodes = useCallback(
    async (
      nodeIds: string[],
      opts: { automatic?: boolean; stopEarly?: boolean; budget?: number } = {},
    ): Promise<boolean> => {
      const unknown = (id: string) =>
        stateRef.current.store.nodes.has(id) &&
        nodeSize(stateRef.current, id) === "unknown";
      const pending = [...new Set(nodeIds)].filter(unknown);
      for (
        let start = 0;
        start < pending.length;
        start += EXPLORER_LIMITS.probeBatch
      ) {
        const current = stateRef.current;
        if (
          opts.budget !== undefined &&
          connectCostFloor(current, nodeIds) >= opts.budget
        )
          return true;
        const nodes = pending
          .slice(start, start + EXPLORER_LIMITS.probeBatch)
          .flatMap((id) => {
            const node = current.store.nodes.get(id);
            return node && unknown(id) ? [node] : [];
          });
        if (nodes.length === 0) continue;
        const dbIds = nodes.map((node) => node.dbId);
        const title =
          nodes.length === 1
            ? `Check the size of ${caption(nodes[0])}`
            : `Check the size of ${nodes.length} nodes`;
        const result = await run(title, buildDegreeProbeRequest({ dbIds }), {
          automatic: opts.automatic,
        });
        if (!result) return false;
        // A failed probe leaves the sizes unknown: those nodes are not read automatically.
        if (!result.ok) return true;
        const probes = new Map<string, DegreeProbe>();
        for (const [dbId, probe] of parseDegreeProbe(result.body, dbIds))
          probes.set(nodeKey(dbId), probe);
        dispatch({ type: "probe/loaded", probes });
        const foundLarge = [...probes.values()].some((probe) =>
          isLargeProbe(probe),
        );
        if (opts.stopEarly && (foundLarge || result.elapsedMs > PROBE_SLOW_MS))
          return true;
      }
      return true;
    },
    [dispatch, run],
  );

  /**
   * Counts the edges of nodes the probe found small (see
   * buildEdgeCountRequest: one pair lookup per neighbor, nothing read per
   * edge), in requests whose summed neighbor counts fit `edgeCountBudget`.
   * Nodes of unknown or large size are never counted (on a hub this would
   * cost a lookup per neighbor, millions of them). A node with no neighbors
   * has no edges, so it needs no request. A failure leaves the rest
   * uncounted: they are then not connected on their own. With `budget` it
   * stops once the counted edges of `nodeIds` reach it: one connect request
   * couldn't take more. Resolves false when the response no longer matters.
   */
  const countEdges = useCallback(
    async (
      nodeIds: string[],
      opts: { budget?: number } = {},
    ): Promise<boolean> => {
      const pending = [...new Set(nodeIds)].filter((id) =>
        needsEdgeCount(stateRef.current, id),
      );
      const empty = new Map<string, { out: number; in: number }>();
      for (const id of pending) {
        const probe = stateRef.current.probes.get(id);
        if (probe && probe.out === 0 && probe.in === 0)
          empty.set(id, { out: 0, in: 0 });
      }
      if (empty.size > 0) dispatch({ type: "probe/edges", edges: empty });
      let index = 0;
      while (index < pending.length) {
        const current = stateRef.current;
        if (
          opts.budget !== undefined &&
          connectCostFloor(current, nodeIds, { countedOnly: true }) >=
            opts.budget
        ) {
          return true;
        }
        const batch: GraphNode[] = [];
        let lookups = 0;
        for (
          ;
          index < pending.length &&
          batch.length < EXPLORER_LIMITS.edgeCountBatch;
          index += 1
        ) {
          const id = pending[index];
          const node = current.store.nodes.get(id);
          const probe = current.probes.get(id);
          if (!node || !probe || !needsEdgeCount(current, id)) continue;
          const cost = probe.out + probe.in;
          if (
            batch.length > 0 &&
            lookups + cost > EXPLORER_LIMITS.edgeCountBudget
          )
            break;
          batch.push(node);
          lookups += cost;
        }
        if (batch.length === 0) continue;
        const dbIds = batch.map((node) => node.dbId);
        const title =
          batch.length === 1
            ? `Count the edges of ${caption(batch[0])}`
            : `Count the edges of ${batch.length} nodes`;
        const result = await run(title, buildEdgeCountRequest({ dbIds }));
        if (!result) return false;
        if (!result.ok) return true;
        const edges = new Map<string, { out: number; in: number }>();
        for (const [dbId, counted] of parseEdgeCount(result.body, dbIds))
          edges.set(nodeKey(dbId), counted);
        dispatch({ type: "probe/edges", edges });
      }
      return true;
    },
    [dispatch, run],
  );

  /** "User #12", or "3 nodes (User #12, User #13, Tag #4)", for a notice. */
  const describeNodes = useCallback((ids: readonly string[]) => {
    const store = stateRef.current.store;
    const names = ids
      .slice(0, 3)
      .map((id) => {
        const node = store.nodes.get(id);
        return node ? caption(node) : id;
      })
      .join(", ");
    return ids.length === 1
      ? names
      : `${ids.length.toLocaleString("en-US")} nodes (${names}${ids.length > 3 ? ", …" : ""})`;
  }, []);

  /** Adds (or grows) the notice offering to connect nodes the explorer left unconnected. */
  const noteUnconnected = useCallback(
    (skipped: string[]) => {
      const current = stateRef.current;
      const previous = current.notices.find(
        (notice) => notice.id === CONNECT_NOTICE_ID,
      );
      const earlier =
        previous?.action?.kind === "connect" ? previous.action.nodeIds : [];
      const ids = [...new Set([...earlier, ...skipped])].filter((id) =>
        current.store.nodes.has(id),
      );
      if (ids.length === 0) return;
      const threshold = EXPLORER_LIMITS.degreeThreshold.toLocaleString("en-US");
      const large = ids.filter((id) => nodeSize(current, id) === "large");
      const unknown = ids.filter(
        (id) =>
          nodeSize(current, id) !== "large" &&
          connectCost(current, id) === null,
      );
      const count = (list: string[]) => list.length.toLocaleString("en-US");
      let why: string;
      if (large.length === ids.length) {
        why = `${ids.length === 1 ? "it has" : "each has"} ${threshold}+ relationships, and connecting reads every one of them`;
      } else if (large.length > 0 || unknown.length > 0) {
        // Some may be large: say what connecting them costs, and that it asks before reading a large one.
        const reasons: string[] = [];
        if (large.length > 0)
          reasons.push(
            `${count(large)} ${large.length === 1 ? "has" : "have"} ${threshold}+ relationships`,
          );
        if (unknown.length > 0) {
          reasons.push(
            ids.length === 1
              ? "its size couldn't be checked"
              : `${count(unknown)} couldn't be checked for size yet`,
          );
        }
        why = `${reasons.join(" and ")}. Connecting reads every relationship of each node, which takes a while on a large one; it checks sizes first and asks before reading a large node's`;
      } else {
        why =
          "the explorer reads relationships on its own only for nodes it knows have few, a few at a time";
      }
      const notice: ExplorerNotice = {
        id: CONNECT_NOTICE_ID,
        tone: "info",
        message: `Didn't connect ${describeNodes(ids)} to the rest of the canvas: ${why}.`,
        action: { kind: "connect", label: "Connect", nodeIds: ids },
      };
      dispatch({ type: "notice/add", notice });
    },
    [describeNodes, dispatch],
  );

  /** Asks before connecting nodes that are large (or of unknown size): each costs a read per relationship. */
  const warnConnect = useCallback(
    (large: string[], unknown: string[]) => {
      const ids = [...large, ...unknown].filter((id) =>
        stateRef.current.store.nodes.has(id),
      );
      if (ids.length === 0) return;
      const threshold = EXPLORER_LIMITS.degreeThreshold.toLocaleString("en-US");
      const one = ids.length === 1;
      const what =
        unknown.length === 0
          ? `${one ? "has" : "have"} ${threshold}+ relationships`
          : large.length === 0
            ? `couldn't be checked for size`
            : `have ${threshold}+ relationships or couldn't be checked for size`;
      dispatch({
        type: "notice/add",
        notice: {
          id: CONNECT_LARGE_NOTICE_ID,
          tone: "warning",
          message: `${describeNodes(ids)} ${what}. Connecting ${one ? "it" : "them"} reads every relationship of each before it finds the ones to the canvas, which can take a while and slows the database meanwhile.`,
          action: {
            kind: "connect",
            label: "Connect anyway",
            nodeIds: ids,
            confirmed: true,
          },
        },
      });
    },
    [describeNodes, dispatch],
  );

  /**
   * One connect request: the edges between `ids` and everything in the
   * store. Resolves with the result (null when it no longer matters; ok
   * without a request when there is nothing to connect to).
   */
  const connectBatch = useCallback(
    async (
      ids: string[],
      opts: { automatic: boolean },
    ): Promise<DatabaseQueryResult | null> => {
      const store = stateRef.current.store;
      const batch = new Set(ids);
      const newDbIds = ids.flatMap((id) => {
        const node = store.nodes.get(id);
        return node ? [node.dbId] : [];
      });
      const others = [...store.nodes.values()]
        .filter((node) => !batch.has(node.id))
        .map((node) => node.dbId);
      if (newDbIds.length === 0 || (others.length === 0 && newDbIds.length < 2))
        return NOTHING_TO_CONNECT;
      const title = `Connect ${newDbIds.length} new ${newDbIds.length === 1 ? "node" : "nodes"}`;
      const shape = stateRef.current.shape;
      // Paced either way: an explicit connect of many nodes sends several heavy requests.
      const pacing = { automatic: opts.automatic, paced: true };
      let result = await run(
        title,
        buildConnectRequest({ newDbIds, visibleDbIds: others, shape }),
        pacing,
      );
      if (
        result &&
        !result.ok &&
        isTooLarge(result) &&
        fetchesEdgeProperties(shape)
      ) {
        // Heavy edge values the probe didn't see: connect with ids and endpoints only.
        result = await run(
          `${title} without edge properties`,
          buildConnectRequest({
            newDbIds,
            visibleDbIds: others,
            shape: withoutEdgeProperties(shape),
          }),
          pacing,
        );
      }
      if (result?.ok) mergeInto({ edges: parseConnect(result.body) });
      return result;
    },
    [mergeInto, run],
  );

  /**
   * Fetches the edges between newly added nodes and everything else in the
   * store. Connecting reads one record per edge of the new nodes, so:
   * - "auto" (what the explorer does on its own) takes the first
   *   `AUTO_CONNECT_MAX` of them, probes their sizes, a few at a time,
   *   counts the edges of the small ones (bounded), and connects those whose
   *   edge counts fit one request's budget. Probing and counting stop once
   *   that budget is known to be used up, so a large query result costs a
   *   few requests, not a few per node. The rest (unchecked ones included)
   *   are offered in a notice.
   * - "explicit" (the notice's "Connect") probes and counts every one of
   *   them, connects the small ones in budgeted requests, and asks again
   *   before reading the large ones and those it couldn't check.
   * - "confirmed" (after that warning) connects them all, a large or unknown
   *   one per request.
   */
  const connectNew = useCallback(
    async (
      added: string[],
      opts: { mode?: "auto" | "explicit" | "confirmed" } = {},
    ) => {
      const mode = opts.mode ?? "auto";
      if (added.length === 0) return;
      const present = () =>
        [...new Set(added)].filter((id) =>
          stateRef.current.store.nodes.has(id),
        );
      let ids = present();
      if (
        ids.length === 0 ||
        (stateRef.current.store.nodes.size === ids.length && ids.length < 2)
      )
        return;
      if (mode === "auto") {
        // One connect request's worth: no more nodes, and no more probes or counts, than it can take.
        const candidates = new Set(ids.slice(0, AUTO_CONNECT_MAX));
        const budget = EXPLORER_LIMITS.connectEdgeBudget;
        const kept = () => present().filter((id) => candidates.has(id));
        if (!(await probeNodes(kept(), { stopEarly: true, budget }))) return;
        if (!(await countEdges(kept(), { budget }))) return;
        const plan = planConnect(stateRef.current, kept());
        const skipped = [
          ...plan.skipped,
          ...present().filter((id) => !candidates.has(id)),
        ];
        if (skipped.length > 0) noteUnconnected(skipped);
        if (plan.connect.length > 0)
          await connectBatch(plan.connect, { automatic: true });
        return;
      }
      if (mode !== "confirmed") {
        if (!(await probeNodes(ids))) return;
        if (!(await countEdges(present()))) return;
        ids = present();
      }
      const { batches, large, unknown } = connectBatches(stateRef.current, ids);
      const requests =
        mode === "confirmed"
          ? [...batches, ...[...large, ...unknown].map((id) => [id])]
          : batches;
      for (const batch of requests) {
        const result = await connectBatch(batch, { automatic: false });
        if (!result) return;
        if (!result.ok) {
          notify({
            tone: "error",
            title: "Couldn't connect the nodes",
            failure: result,
          });
          return;
        }
      }
      if (mode === "explicit") warnConnect(large, unknown);
    },
    [
      connectBatch,
      countEdges,
      noteUnconnected,
      notify,
      probeNodes,
      warnConnect,
    ],
  );

  /**
   * Counts labels from their bitmaps and, with `probe`, probes their
   * properties in the same request (one row per label when two are too
   * large; counts alone when the probe keeps failing). Returns null when the
   * response no longer matters, or the failure when the boot should stop.
   */
  const countLabels = useCallback(
    async (
      labels: { nodes: string[]; edges: string[] },
      complete: { nodes: boolean; edges: boolean },
      opts: { probe: boolean },
    ): Promise<
      | {
          kind: "counted";
          stats: LabelStats;
          probe: { schemas: LabelSchema[]; shape: NodeFetchShape } | null;
        }
      | { kind: "failed"; failure: QueryFailure; halt: boolean }
      | null
    > => {
      const title = "Count nodes and edges by label";
      const attempts: { rows: number; title: string }[] = opts.probe
        ? [
            {
              rows: EXPLORER_LIMITS.probeRowsPerLabel,
              title: `${title} and probe their properties`,
            },
            {
              rows: 1,
              title: `${title} and probe their properties (one row each)`,
            },
            { rows: 0, title },
          ]
        : [{ rows: 0, title }];
      let failure: QueryFailure | null = null;
      for (const attempt of attempts) {
        const { scope } = optionsRef.current;
        const result = await run(
          attempt.title,
          buildLabelCountsRequest(labels, {
            scope,
            ...(attempt.rows > 0 ? { probeRows: attempt.rows } : {}),
          }),
        );
        if (!result) return null;
        if (result.ok) {
          const stats = parseLabelCounts(result.body, labels, scope, complete);
          if (attempt.rows === 0)
            return { kind: "counted", stats, probe: null };
          // Probe only what is known to exist: a remembered label counted at zero has no schema to
          // show, and one over the cap (not counted yet) may be gone too.
          const existing = {
            nodes: stats.nodeLabels
              .filter((entry) => entry.count !== null)
              .map((entry) => entry.label),
            edges: stats.edgeLabels
              .filter((entry) => entry.count !== null)
              .map((entry) => entry.label),
          };
          return {
            kind: "counted",
            stats,
            probe: parseSchemaProbe(
              result.body,
              existing,
              optionsRef.current.indexes,
            ),
          };
        }
        failure = result;
        if (haltsBoot(result))
          return { kind: "failed", failure: result, halt: true };
        // Too large (heavy probe rows) or a database error: next attempt is smaller.
      }
      return failure ? { kind: "failed", failure, halt: false } : null;
    },
    [run],
  );

  /** Finds labels without reading the whole graph; null when the response no longer matters. */
  const discoverLabels = useCallback(async (): Promise<
    | { kind: "found"; discovery: LabelDiscovery }
    | { kind: "failed"; failure: QueryFailure }
    | null
  > => {
    const result = await run("Find labels", buildLabelDiscoveryRequest());
    if (!result) return null;
    if (!result.ok) return { kind: "failed", failure: result };
    return { kind: "found", discovery: parseLabelDiscovery(result.body) };
  }, [run]);

  const loadSampleWith = useCallback(
    async (
      stats: Pick<LabelStats, "nodeLabels" | "edgeLabels" | "partial"> | null,
      shape: NodeFetchShape,
      small: boolean,
    ): Promise<SampleOutcome | null> => {
      const nodeLabels = stats?.nodeLabels ?? null;
      const budget = {
        ...(small
          ? { nodeBudget: SMALL_SAMPLE_NODES, edgeBudget: SMALL_SAMPLE_EDGES }
          : {}),
        edgeLabels:
          stats && stats.edgeLabels.length > 0 ? stats.edgeLabels : null,
        scope: optionsRef.current.scope,
      };
      let used = shape;
      let result = await run(
        small ? "Load a small sample" : "Load a sample",
        buildSampleRequest({ nodeLabels, shape, ...budget }),
      );
      if (result && !result.ok && isTooLarge(result) && !isLightShape(shape)) {
        // Ids, labels and endpoints only; each node lists its label's keys as not loaded.
        used = lightShape(shape);
        result = await run(
          "Load a sample without properties",
          buildSampleRequest({ nodeLabels, shape: used, ...budget }),
        );
      }
      if (!result) return null;
      if (!result.ok) return { kind: "failed", failure: result };
      const sample = parseSample(result.body, used);
      const shown = shownGraph(
        { nodes: sample.nodes, edges: sample.edges },
        optionsRef.current.exclude,
      );
      const graph = { nodes: shown.nodes ?? [], edges: shown.edges ?? [] };
      // A sample can't say it left nothing out; exact counts of the labels it
      // shows can (not partial: every count known).
      const total = (entries: LabelCount[]) =>
        entries.reduce((sum, entry) => sum + (entry.count ?? 0), 0);
      const covered =
        stats !== null &&
        !stats.partial?.nodes &&
        !stats.partial?.edges &&
        total(stats.nodeLabels) === graph.nodes.length &&
        total(stats.edgeLabels) === graph.edges.length;
      return {
        kind: "loaded",
        graph,
        truncated: sample.truncated && !covered,
        light: used !== shape,
      };
    },
    [run],
  );

  /**
   * Fills in properties of nodes that were loaded without them, a batch at a
   * time. Without a probed shape it fetches full rows, in smaller batches
   * that shrink further if a response is still too large.
   */
  const refetchProperties = useCallback(
    async (dbIds: number[], shape: NodeFetchShape, generation: number) => {
      let size = shape.keys === null ? REFETCH_FULL_BATCH : REFETCH_BATCH;
      for (let start = 0; start < dbIds.length; ) {
        if (generation !== generationRef.current) return;
        const batch = dbIds.slice(start, start + size);
        // Nodes cleared or removed meanwhile don't need their properties.
        const wanted = batch.filter((dbId) =>
          stateRef.current.store.nodes.has(nodeKey(dbId)),
        );
        if (wanted.length === 0) {
          start += batch.length;
          continue;
        }
        const result = await run(
          `Load properties of ${wanted.length} ${wanted.length === 1 ? "node" : "nodes"}`,
          buildNodesByIdRequest({ dbIds: wanted, shape }),
        );
        if (!result) return;
        if (!result.ok) {
          if (isTooLarge(result) && size > REFETCH_MIN_BATCH) {
            size = Math.max(REFETCH_MIN_BATCH, Math.floor(size / 4));
            continue;
          }
          notify({
            tone: "error",
            title: "Couldn't load node properties",
            description:
              "Some nodes show only their label and id. Expanding a node loads its properties.",
            failure: result,
          });
          return;
        }
        // The shape knows every key these nodes list as not loaded, so its omitted lists are exact.
        mergeInto(
          { nodes: parseNodes(result.body, shape) },
          { replaceOmitted: true },
        );
        start += batch.length;
      }
    },
    [mergeInto, notify, run],
  );

  /**
   * Puts the first view on the canvas. Label pages, search reveals and query
   * results can land while it loads; the sample merges into them instead of
   * replacing them. The sample carries its own edges, so only the few nodes
   * that were there first are connected to it (from their side: the sample's
   * nodes are never probed or connected on their own).
   */
  const placeSample = useCallback(
    async (
      sample: Extract<SampleOutcome, { kind: "loaded" }>,
      truncated: boolean,
    ) => {
      const earlier = [...stateRef.current.store.nodes.keys()];
      const merge = admit(sample.graph);
      dispatch({
        type: "sample/loaded",
        merge,
        truncated,
        light: sample.light,
      });
      if (earlier.length > 0 && merge.addedNodes.length > 0)
        await connectNew(earlier);
    },
    [admit, connectNew, dispatch],
  );

  /**
   * A load's first view as it grows (./lib/seeding.ts): the sizes probed so
   * far, the nodes read around already, and the outermost ring, which "Show
   * more" reads around next. Reset by every boot.
   */
  const seedingRef = useRef(newSeedingState());
  const [canShowMore, setCanShowMore] = useState(false);

  /** The seeding's reads and writes, through this explorer, until `alive` says otherwise. */
  const seedingIO = useCallback(
    (alive: () => boolean): SeedingIO => ({
      read: async (title, request) => {
        const result = await run(title, request);
        return result && alive() ? result : null;
      },
      place: (seeds) =>
        placeSample(
          {
            kind: "loaded",
            graph: { nodes: seeds, edges: [] },
            truncated: true,
            light: false,
          },
          true,
        ),
      merge: (graph) => {
        const merge = mergeInto(graph);
        const fresh = new Set(merge.addedNodes);
        return (graph.nodes ?? []).filter((n) => fresh.has(n.id));
      },
      probed: (probes) =>
        dispatch({
          type: "probe/loaded",
          probes: new Map(
            [...probes].map(([dbId, probe]) => [nodeKey(dbId), probe]),
          ),
        }),
      onCanvas: () =>
        [...stateRef.current.store.nodes.values()].map((n) => n.dbId),
      failed: (title, failure) =>
        notify({
          tone: "error",
          title: `Couldn't ${title.charAt(0).toLowerCase()}${title.slice(1)}`,
          failure,
        }),
    }),
    [dispatch, mergeInto, notify, placeSample, run],
  );

  /**
   * A load's first view (./lib/seeding.ts): seeds from the hub types on the
   * canvas at once, a ring around them, a second while the budget lasts,
   * then the edges among them. When no hub type holds anything in the load,
   * a sample of the load's labels instead.
   */
  const seedFirstView = useCallback(
    async (
      stats: LabelStats | null,
      shape: NodeFetchShape,
      alive: () => boolean,
    ): Promise<void> => {
      const { scope, layout } = optionsRef.current;
      seedingRef.current = newSeedingState();
      setCanShowMore(false);
      const holding = new Set(
        (stats?.nodeLabels ?? [])
          .filter((entry) => entry.count !== 0)
          .map((entry) => entry.label),
      );
      const outcome = layout
        ? await seedView(seedingIO(alive), seedingRef.current, {
            scope,
            layout,
            holding,
            shape,
          })
        : ({ kind: "none" } as const);
      if (!alive()) return;
      if (outcome.kind === "failed") {
        if (haltsBoot(outcome.failure))
          dispatch({
            type: "boot/failed",
            failure: outcome.failure,
            stage: "sample",
          });
        else dispatch({ type: "sample/failed", failure: outcome.failure });
        return;
      }
      if (outcome.kind === "seeded") {
        setCanShowMore(seedingRef.current.frontier.length > 0);
        return;
      }
      if (outcome.kind === "stale") return;
      const sample = await loadSampleWith(stats, shape, false);
      if (!sample || !alive()) return;
      if (sample.kind === "failed")
        dispatch({ type: "sample/failed", failure: sample.failure });
      else await placeSample(sample, sample.truncated);
    },
    [dispatch, loadSampleWith, placeSample, seedingIO],
  );

  const showMore = useCallback(async () => {
    await track("more", async () => {
      const generation = generationRef.current;
      const alive = () => generation === generationRef.current;
      const { layout } = optionsRef.current;
      if (!layout) return;
      const ring = await moreRing(
        seedingIO(alive),
        seedingRef.current,
        layout,
        stateRef.current.shape,
        Math.min(SEEDING.moreBudget, canvasRoom(stateRef.current, CAP)),
      );
      if (!ring || !alive()) return;
      setCanShowMore(ring.length > 0);
      if (!ring.length)
        notify({
          tone: "info",
          title: "Nothing more around these nodes",
          description:
            "Expand a node, or open a label's nodes from the rail, to read further.",
        });
    });
  }, [notify, seedingIO, track]);

  /**
   * Sides of nodes the user chose to expand although they are large (or of
   * unknown size), keyed `${nodeId}|${side}`: later pages of those sides don't
   * ask again, but another side does. Forgotten on a refresh and on Clear.
   */
  const confirmedSidesRef = useRef(new Set<string>());

  /** Labels counted (or tried) since the boot, keyed `node:`/`edge:`, so late counting asks about each once. */
  const countedRef = useRef(new Set<string>());
  /**
   * Labels a counts request left out (over its per-side cap), keyed like
   * countedRef: counted in the background in capped requests of their own,
   * outside the budget for labels seen later, so all of them get counted.
   */
  const backlogRef = useRef(new Set<string>());
  const lateCountsRef = useRef(0);
  /** A background count is in flight or waiting to retry: the next batch waits for it. */
  const lateBusyRef = useRef(false);
  /** Background counts that failed in a row (reset by one that works, and by a refresh). */
  const lateFailuresRef = useRef(0);
  /** Bumped when a background count settles, so the next batch is scheduled whether or not the labels changed. */
  const [lateCountRound, setLateCountRound] = useState(0);
  const markCounted = useCallback(
    (labels: {
      nodes: readonly string[];
      edges: readonly string[];
      dropped?: { nodes: string[]; edges: string[] };
    }) => {
      for (const label of labels.nodes)
        countedRef.current.add(countedKey("node", label));
      for (const label of labels.edges)
        countedRef.current.add(countedKey("edge", label));
      for (const label of labels.dropped?.nodes ?? [])
        backlogRef.current.add(countedKey("node", label));
      for (const label of labels.dropped?.edges ?? [])
        backlogRef.current.add(countedKey("edge", label));
    },
    [],
  );
  /** Undoes markCounted for labels whose count failed, so they are counted again (backlog ones stay in the backlog). */
  const unmarkCounted = useCallback(
    (labels: { nodes: readonly string[]; edges: readonly string[] }) => {
      for (const label of labels.nodes)
        countedRef.current.delete(countedKey("node", label));
      for (const label of labels.edges)
        countedRef.current.delete(countedKey("edge", label));
    },
    [],
  );

  const remember = useCallback(
    (stats: LabelStats, forget?: { nodes: string[]; edges: string[] }) => {
      rememberLabels(
        optionsRef.current.storageKey,
        {
          nodes: stats.nodeLabels.map((entry) => entry.label),
          edges: stats.edgeLabels.map((entry) => entry.label),
        },
        { forget },
      );
    },
    [],
  );

  /**
   * The first view: find labels (bounded discovery reads, the index catalog
   * and labels remembered for this database), count them from their bitmaps
   * and probe their properties in one request, then load a sample per label.
   * Nothing here reads the whole graph; a gateway refusing queries or a
   * timeout on these cheap requests stops the boot right away.
   */
  const boot = useCallback(
    async ({ small = false }: { small?: boolean } = {}) => {
      abortAll();
      const generation = (generationRef.current += 1);
      const alive = () => generation === generationRef.current;
      const { canRead, storageKey: key, indexes } = optionsRef.current;
      confirmedSidesRef.current = new Set();
      countedRef.current = new Set();
      backlogRef.current = new Set();
      lateCountsRef.current = 0;
      lateBusyRef.current = false;
      lateFailuresRef.current = 0;
      dispatch({
        type: "boot",
        noAccess: canRead === false,
        hiddenLabels: readStored(hiddenLabelsKey(key), asStringArray) ?? [],
      });
      if (canRead === false) return;

      // A load: its labels are its version's (and Foundry's own, when
      // shown), counted in the load; the first view is seeded.
      if (optionsRef.current.scope.kind === "load") {
        const { scope, layout, exclude } = optionsRef.current;
        const labels = loadLabels(layout, exclude);
        dispatch({ type: "overview/loading" });
        const counted = await countLabels(
          labels,
          { nodes: true, edges: true },
          { probe: true },
        );
        if (!counted || !alive()) return;
        let stats: LabelStats | null = null;
        let shape = fallbackShape();
        if (counted.kind === "failed") {
          dispatch({ type: "overview/failed", failure: counted.failure });
          if (counted.halt) {
            dispatch({
              type: "boot/failed",
              failure: counted.failure,
              stage: "labels",
            });
            return;
          }
        } else {
          stats = counted.stats;
          markCounted(countedLabels(labels, scope));
          dispatch({ type: "overview/loaded", stats });
          if (counted.probe) {
            dispatch({ type: "schema/loaded", ...counted.probe });
            shape = counted.probe.shape;
          }
          if (stats.nodeLabels.length === 0) {
            dispatch({ type: "boot/empty" });
            return;
          }
        }
        dispatch({ type: "sample/loading" });
        await seedFirstView(stats, shape, alive);
        return;
      }

      dispatch({ type: "overview/loading" });
      const found = await discoverLabels();
      if (!found || !alive()) return;
      if (found.kind === "failed" && haltsBoot(found.failure)) {
        dispatch({ type: "overview/failed", failure: found.failure });
        dispatch({
          type: "boot/failed",
          failure: found.failure,
          stage: "labels",
        });
        return;
      }
      // A database error here (an engine without one of the operations, say)
      // only costs the discovered labels; the others still get counted.
      const discovery = found.kind === "found" ? found.discovery : null;
      if (discovery?.empty) {
        dispatch({
          type: "overview/loaded",
          stats: { nodeLabels: [], edgeLabels: [], nodeTotal: 0, edgeTotal: 0 },
        });
        dispatch({ type: "boot/empty" });
        return;
      }
      const remembered = readRememberedLabels(key);
      const known = bootLabels(discovery, indexLabels(indexes), remembered);
      const labels = shownLabels(known, optionsRef.current.exclude);
      excludedSeenRef.current =
        labels.nodes.length + labels.edges.length <
        known.nodes.length + known.edges.length;
      const complete = {
        nodes: discovery?.nodesExhausted ?? false,
        edges: discovery?.edgesExhausted ?? false,
      };

      let stats: LabelStats | null = null;
      let probe: { schemas: LabelSchema[]; shape: NodeFetchShape } | null =
        null;
      if (labels.nodes.length > 0 || labels.edges.length > 0) {
        const counted = await countLabels(labels, complete, { probe: true });
        if (!counted || !alive()) return;
        if (counted.kind === "failed") {
          dispatch({ type: "overview/failed", failure: counted.failure });
          if (counted.halt) {
            dispatch({
              type: "boot/failed",
              failure: counted.failure,
              stage: "labels",
            });
            return;
          }
        } else {
          stats = counted.stats;
          probe = counted.probe;
          const capped = countedLabels(labels, optionsRef.current.scope);
          // Only the labels the request covered; those over the cap are counted in the background.
          markCounted(capped);
          dispatch({ type: "overview/loaded", stats });
          if (probe) dispatch({ type: "schema/loaded", ...probe });
          // Labels counted at zero are gone (or were never written): stop remembering them.
          remember(stats, emptyLabelsOf(capped, stats));
          const dropped = [...capped.dropped.nodes, ...capped.dropped.edges];
          if (dropped.length > 0) {
            notify({
              tone: "info",
              title: `${formatLabelCount(dropped.length)} not counted yet`,
              description: `Each request counts up to ${EXPLORER_LIMITS.maxCountLabels} node and ${EXPLORER_LIMITS.maxCountLabels} edge labels; the rest (${dropped.slice(0, 5).join(", ")}${dropped.length > 5 ? ", …" : ""}) are counted in the background, a request at a time.`,
            });
          }
        }
      } else if (found.kind === "failed") {
        dispatch({ type: "overview/failed", failure: found.failure });
      }

      if (stats && stats.nodeLabels.length > 0) {
        const shape = probe?.shape ?? fallbackShape();
        dispatch({ type: "sample/loading" });
        const sample = await loadSampleWith(stats, shape, small);
        if (!sample || !alive()) return;
        if (sample.kind === "failed") {
          dispatch({ type: "sample/failed", failure: sample.failure });
          return;
        }
        await placeSample(sample, sample.truncated);
        // A probe that found no light keys leaves nothing to fill in.
        if (sample.light && shape.keys?.length !== 0 && alive()) {
          await refetchProperties(
            sample.graph.nodes.map((node) => node.dbId),
            shape,
            generation,
          );
        }
        return;
      }

      // No label known to hold nodes (discovery failed or found only labels
      // that are gone): start from the first nodes by id, then count and
      // probe the labels they carry.
      if (!stats) dispatch({ type: "overview/loading" });
      dispatch({ type: "sample/loading" });
      const sample = await loadSampleWith(null, fallbackShape(), small);
      if (!sample || !alive()) return;
      if (sample.kind === "failed") {
        if (haltsBoot(sample.failure))
          dispatch({
            type: "boot/failed",
            failure: sample.failure,
            stage: "sample",
          });
        else dispatch({ type: "sample/failed", failure: sample.failure });
        return;
      }
      if (
        sample.graph.nodes.length === 0 &&
        stateRef.current.store.nodes.size === 0
      ) {
        if (!stats)
          dispatch({
            type: "overview/loaded",
            stats: {
              nodeLabels: [],
              edgeLabels: [],
              nodeTotal: 0,
              edgeTotal: 0,
            },
          });
        dispatch({ type: "boot/empty" });
        return;
      }
      await placeSample(sample, true);
      const sampled = {
        nodes: [
          ...new Set(sample.graph.nodes.map((node) => node.label)),
        ].sort(),
        edges: [
          ...new Set(sample.graph.edges.map((edge) => edge.label)),
        ].sort(),
      };
      const counted = await countLabels(
        sampled,
        { nodes: false, edges: false },
        { probe: true },
      );
      if (!counted || !alive()) return;
      if (counted.kind === "failed") {
        dispatch({ type: "overview/failed", failure: counted.failure });
        return;
      }
      markCounted(countedLabels(sampled, optionsRef.current.scope));
      dispatch({ type: "overview/loaded", stats: counted.stats });
      remember(counted.stats);
      if (counted.probe) dispatch({ type: "schema/loaded", ...counted.probe });
      if (sample.light && counted.probe?.shape.keys?.length !== 0) {
        await refetchProperties(
          sample.graph.nodes.map((node) => node.dbId),
          counted.probe?.shape ?? fallbackShape(),
          generation,
        );
      }
    },
    [
      abortAll,
      countLabels,
      discoverLabels,
      dispatch,
      loadSampleWith,
      markCounted,
      notify,
      placeSample,
      refetchProperties,
      remember,
      seedFirstView,
    ],
  );

  // Boot on mount and whenever the database, the load, its version or read
  // access changes.
  const exclusion = options.exclude;
  const scopeId =
    options.scope.kind === "load"
      ? `load:${options.scope.id}`
      : `other:${options.scope.loads.join(",")}`;
  const layoutKey = options.layout
    ? `${options.layout.types.join(",")}|${options.layout.links.map((l) => l.name).join(",")}`
    : "";
  const bootKey = `${storageKey}|${scopeId}|${layoutKey}|${String(options.canRead)}|${exclusion ? `${exclusion.nodePrefix}|${exclusion.edgePrefix}` : ""}`;
  useEffect(() => {
    void boot();
    return () => {
      generationRef.current += 1;
      abortAll();
    };
  }, [bootKey, boot, abortAll]);

  // Hidden labels are remembered per database.
  const hiddenLabels = state.hiddenLabels;
  useEffect(() => {
    if (stateRef.current.phase === "idle") return;
    writeStored(
      hiddenLabelsKey(storageKey),
      hiddenLabels.size ? [...hiddenLabels].sort() : null,
    );
  }, [hiddenLabels, storageKey]);

  const refresh = useCallback(
    (opts?: { small?: boolean }) => void boot(opts),
    [boot],
  );

  /** Finds and counts labels again (discovery plus bitmap counts), keeping the canvas. */
  const retryOverview = useCallback(() => {
    void track("overview", async () => {
      const generation = generationRef.current;
      const previous = stateRef.current.overview;
      const restore = () => {
        // Cancelled (Clear aborts every request): put the failed state, and its
        // retry, back. After a refresh the new boot owns the counts.
        if (generation === generationRef.current) {
          dispatch({
            type: "overview/restore",
            status: previous.status,
            error: previous.error,
          });
        }
      };
      dispatch({ type: "overview/loading" });
      if (optionsRef.current.scope.kind === "load") {
        const { scope, layout, exclude } = optionsRef.current;
        const labels = loadLabels(layout, exclude);
        const counted = await countLabels(
          labels,
          { nodes: true, edges: true },
          { probe: false },
        );
        if (!counted) return restore();
        if (counted.kind === "failed") {
          dispatch({ type: "overview/failed", failure: counted.failure });
          notify({
            tone: "error",
            title: "Couldn't count labels",
            failure: counted.failure,
          });
          return;
        }
        markCounted(countedLabels(labels, scope));
        dispatch({ type: "overview/loaded", stats: counted.stats });
        return;
      }
      const found = await discoverLabels();
      if (!found) return restore();
      if (found.kind === "failed" && haltsBoot(found.failure)) {
        dispatch({ type: "overview/failed", failure: found.failure });
        notify({
          tone: "error",
          title: "Couldn't count labels",
          failure: found.failure,
        });
        return;
      }
      const discovery = found.kind === "found" ? found.discovery : null;
      const current = stateRef.current;
      const seen = railLabels(current);
      const known = bootLabels(
        discovery,
        indexLabels(optionsRef.current.indexes),
        {
          nodes: [
            ...seen.nodes.map((entry) => entry.label),
            ...readRememberedLabels(optionsRef.current.storageKey).nodes,
          ],
          edges: [
            ...seen.edges.map((entry) => entry.label),
            ...readRememberedLabels(optionsRef.current.storageKey).edges,
          ],
        },
      );
      const labels = shownLabels(known, optionsRef.current.exclude);
      if (
        labels.nodes.length + labels.edges.length <
        known.nodes.length + known.edges.length
      )
        excludedSeenRef.current = true;
      const counted = await countLabels(
        labels,
        {
          nodes: discovery?.nodesExhausted ?? false,
          edges: discovery?.edgesExhausted ?? false,
        },
        { probe: false },
      );
      if (!counted) return restore();
      if (counted.kind === "failed") {
        dispatch({ type: "overview/failed", failure: counted.failure });
        notify({
          tone: "error",
          title: "Couldn't count labels",
          failure: counted.failure,
        });
        return;
      }
      const capped = countedLabels(labels, optionsRef.current.scope);
      markCounted(capped);
      dispatch({ type: "overview/loaded", stats: counted.stats });
      remember(counted.stats, emptyLabelsOf(capped, counted.stats));
    });
  }, [
    countLabels,
    discoverLabels,
    dispatch,
    markCounted,
    notify,
    remember,
    track,
  ]);

  // A scan that timed out (or was cancelled) may still be reading on the
  // database; another one right away would double the load. Remembered per
  // database, so a reload doesn't lift it.
  const [scanBlockedUntil, setScanBlockedUntil] = useState<number | null>(
    () => {
      const until = readStored(scanCooldownKey(storageKey), asTimestamp);
      return until !== null && until > Date.now() ? until : null;
    },
  );
  useEffect(() => {
    const until = readStored(scanCooldownKey(storageKey), asTimestamp);
    setScanBlockedUntil(until !== null && until > Date.now() ? until : null);
  }, [storageKey]);
  useEffect(() => {
    if (scanBlockedUntil === null) return;
    const timer = window.setTimeout(
      () => setScanBlockedUntil(null),
      Math.max(0, scanBlockedUntil - Date.now()),
    );
    return () => window.clearTimeout(timer);
  }, [scanBlockedUntil]);
  const holdScans = useCallback(() => {
    const until = Date.now() + SCAN_COOLDOWN_MS;
    writeStored(scanCooldownKey(optionsRef.current.storageKey), until);
    setScanBlockedUntil(until);
  }, []);

  const scanLabels = useCallback(async (): Promise<boolean> => {
    // A load's labels are its version's: nothing to scan for.
    if (optionsRef.current.scope.kind === "load") return false;
    const blocked = readStored(
      scanCooldownKey(optionsRef.current.storageKey),
      asTimestamp,
    );
    if (blocked !== null && blocked > Date.now()) return false;
    const outcome = await track("scan", async () => {
      const result = await run(
        "Scan every node and edge for labels",
        buildLabelScanRequest(),
        {
          key: "scan",
          automatic: false,
        },
      );
      // Cancelled (Clear, a refresh), timed out or cut off: the database may still be scanning.
      if (!result || (!result.ok && scanMayStillRun(result))) holdScans();
      if (!result) return false;
      if (!result.ok) {
        notify({
          tone: "error",
          title: "The label scan failed",
          description: isTimeout(result)
            ? "Reading every node and edge took too long, and it may still be running on the database, so scanning again waits a minute. The labels found so far stay listed."
            : undefined,
          failure: isTimeout(result) ? undefined : result,
        });
        return false;
      }
      const scanned = parseLabelScan(result.body);
      const shownScan = shownStats(scanned, optionsRef.current.exclude);
      if (
        shownScan.nodeLabels.length + shownScan.edgeLabels.length <
        scanned.nodeLabels.length + scanned.edgeLabels.length
      )
        excludedSeenRef.current = true;
      // The scan counts the loads' nodes too: Other data under each label is
      // counted again, from bitmaps, in the scope.
      const listed = {
        nodes: shownScan.nodeLabels.map((entry) => entry.label),
        edges: shownScan.edgeLabels.map((entry) => entry.label),
      };
      const recount = await countLabels(
        listed,
        { nodes: true, edges: true },
        { probe: false },
      );
      if (!recount) return false;
      if (recount.kind === "failed") {
        notify({
          tone: "error",
          title: "Couldn't count the labels the scan found",
          failure: recount.failure,
        });
        return false;
      }
      const stats = recount.stats;
      markCounted(countedLabels(listed, optionsRef.current.scope));
      dispatch({ type: "overview/loaded", stats });
      // The scan lists every label, so it replaces what was remembered: hidden ones too, so
      // showing them later lists them without another scan (reads drop them while hidden).
      rememberLabels(
        optionsRef.current.storageKey,
        {
          nodes: scanned.nodeLabels.map((entry) => entry.label),
          edges: scanned.edgeLabels.map((entry) => entry.label),
        },
        { replace: true },
      );
      notify({
        tone: "info",
        title: "Every label is listed",
        description: "Counts and totals are exact.",
      });
      return true;
    });
    return outcome ?? false;
  }, [countLabels, dispatch, holdScans, markCounted, notify, run, track]);

  /**
   * Loads a fresh sample onto an empty (cleared) canvas without rebooting.
   * The sample carries its own edges, so nothing is connected afterwards
   * (connecting ~400 nodes would read every edge of each, hubs included).
   */
  const loadSample = useCallback(() => {
    void track("sample", async () => {
      const current = stateRef.current;
      const generation = generationRef.current;
      const shape = current.shape;
      if (optionsRef.current.scope.kind === "load") {
        await seedFirstView(
          current.overview.stats,
          shape,
          () => generation === generationRef.current,
        );
        return;
      }
      const result = await loadSampleWith(current.overview.stats, shape, false);
      if (!result) return;
      if (result.kind === "failed") {
        notify({
          tone: "error",
          title: "Couldn't load a sample",
          failure: result.failure,
        });
        return;
      }
      mergeInto(result.graph);
      if (result.light && shape.keys?.length !== 0) {
        await refetchProperties(
          result.graph.nodes.map((node) => node.dbId),
          shape,
          generation,
        );
      }
    });
  }, [
    loadSampleWith,
    mergeInto,
    notify,
    refetchProperties,
    seedFirstView,
    track,
  ]);

  const expandPage = useCallback(
    async (
      node: GraphNode,
      page: {
        direction: ExpandDirection;
        edgeLabel: string | null;
        after: number | null;
        sides: readonly ("out" | "in")[];
        /** Wait for the limiter: a page of a side not known to be small reads all of that side's edges. */
        paced?: boolean;
      },
    ): Promise<{
      merge: MergeResult;
      cursors: { out?: SideCursor; in?: SideCursor };
    } | null> => {
      const limit = EXPLORER_LIMITS.neighborLimit;
      const shape = stateRef.current.shape;
      const parts = [
        page.edgeLabel ? `${page.edgeLabel} ` : "",
        page.direction === "out"
          ? "outgoing "
          : page.direction === "in"
            ? "incoming "
            : "",
      ].join("");
      const title = `${page.after === null ? "Expand" : "Load more"} ${parts}neighbors of ${caption(node)}`;
      const request = (edgeShape: NodeFetchShape) =>
        buildNeighborhoodRequest({
          dbId: node.dbId,
          direction: page.direction,
          edgeLabel: page.edgeLabel,
          limit,
          afterEdgeDbId: page.after,
          shape: edgeShape,
          excludeEdgePrefix: optionsRef.current.exclude?.edgePrefix,
        });
      const pacing = { automatic: false, paced: page.paced ?? false };
      let result = await run(title, request(shape), pacing);
      if (
        result &&
        !result.ok &&
        isTooLarge(result) &&
        fetchesEdgeProperties(shape)
      ) {
        // Edges with heavy values the probe didn't see: the same page without edge properties.
        result = await run(
          `${title} without edge properties`,
          request(withoutEdgeProperties(shape)),
          pacing,
        );
      }
      if (!result) return null;
      if (!result.ok) {
        notify({
          tone: "error",
          title: `Couldn't expand ${caption(node)}`,
          failure: result,
        });
        return null;
      }
      const neighborhood = parseNeighborhood(result.body, node.dbId, shape, {
        limit,
      });
      if (!neighborhood.center) {
        notify({
          tone: "info",
          title: `${caption(node)} no longer exists`,
          description: "It was removed from the canvas.",
        });
        dispatch({ type: "nodes/remove", ids: [node.id] });
        return null;
      }
      const merge = mergeInto({
        nodes: [neighborhood.center, ...neighborhood.nodes],
        edges: neighborhood.edges,
      });
      const cursors: { out?: SideCursor; in?: SideCursor } = {};
      for (const side of page.sides) {
        // Whether more edges exist is the response's call (a page of edges the
        // canvas already had says nothing about the rest). A cursor that
        // doesn't move forward would fetch the same page forever.
        const next = sideCursorAfter(neighborhood, side, limit);
        cursors[side] =
          typeof next === "number" && page.after !== null && next <= page.after
            ? "done"
            : next;
      }
      dispatch({
        type: "expansion",
        key: expansionKey(node.id, page.edgeLabel),
        ...cursors,
      });
      await connectNew(merge.addedNodes);
      return { merge, cursors };
    },
    [connectNew, dispatch, mergeInto, notify, run],
  );

  /**
   * The next page of one side. Other loads often brought a node's next edges
   * already (connecting label pages to a supernode, say), so a page with
   * nothing new moves on to the following one, a few pages at most, but only
   * on a side known to be small: every page of any other side reads all of
   * its edges, so it gets one page per click, paced like the explorer's own
   * requests.
   */
  const pageSide = useCallback(
    async (
      node: GraphNode,
      side: "out" | "in",
      edgeLabel: string | null,
      after: number | null,
    ) => {
      const small = nodeSideSize(stateRef.current, node.id, side) === "small";
      const maxRequests = small ? LOAD_MORE_MAX_REQUESTS : 1;
      let cursor = after;
      let last: MergeResult | null = null;
      for (let requests = 0; requests < maxRequests; requests += 1) {
        const page = await expandPage(node, {
          direction: side,
          edgeLabel,
          after: cursor,
          sides: [side],
          paced: !small,
        });
        if (!page) return null;
        last = page.merge;
        const next = page.cursors[side];
        if (
          page.merge.addedNodes.length > 0 ||
          page.merge.addedEdges.length > 0
        )
          break;
        if (typeof next !== "number" || canvasRoom(stateRef.current, CAP) <= 0)
          break;
        cursor = next;
      }
      return last;
    },
    [expandPage],
  );

  /**
   * The warning in place of an expansion that would read many edges: the
   * node has more than `degreeThreshold` on a side it reads, or its size
   * couldn't be checked (a failed or rate-limited probe), so it may.
   */
  const warnExpand = useCallback(
    (
      node: GraphNode,
      opts: {
        direction: ExpandDirection;
        edgeLabel: string | null;
        large: readonly ("out" | "in")[];
      },
    ) => {
      const which = (sides: readonly ("out" | "in")[]) =>
        sides.length !== 1
          ? ""
          : sides[0] === "out"
            ? "outgoing "
            : "incoming ";
      const threshold = EXPLORER_LIMITS.degreeThreshold.toLocaleString("en-US");
      const shown = EXPLORER_LIMITS.neighborLimit;
      const sides =
        opts.direction === "both" ? (["out", "in"] as const) : [opts.direction];
      const message =
        opts.large.length > 0
          ? `${caption(node)} has ${threshold}+ ${which(opts.large)}relationships. Expanding it reads all of them before it shows the first ${shown}, which can take a while and slows the database meanwhile.`
          : `Couldn't check the size of ${caption(node)}. Expanding it reads all of its ${which(sides)}relationships before it shows the first ${shown}, which can take a while on a large node and slows the database meanwhile.`;
      dispatch({
        type: "notice/add",
        notice: {
          id: expandNoticeId(node.id),
          tone: "warning",
          message,
          action: {
            kind: "expand",
            label: "Expand anyway",
            nodeId: node.id,
            direction: opts.direction,
            edgeLabel: opts.edgeLabel,
          },
        },
      });
    },
    [dispatch],
  );

  const expand = useCallback(
    async (nodeId: string, opts: ExpandOptions = {}) => {
      const direction = opts.direction ?? "both";
      const edgeLabel = opts.edgeLabel || null;
      await track(`expand:${nodeId}`, async () => {
        const start = stateRef.current;
        const node = start.store.nodes.get(nodeId);
        if (!node) return;
        if (canvasRoom(start, CAP) <= 0) {
          capReached();
          return;
        }
        const sides =
          direction === "both"
            ? (["out", "in"] as const)
            : ([direction] as const);
        const sizeOf = (side: "out" | "in") =>
          nodeSideSize(stateRef.current, nodeId, side);
        const sideKey = (side: "out" | "in") => `${nodeId}|${side}`;
        if (opts.confirmed) {
          for (const side of sides)
            confirmedSidesRef.current.add(sideKey(side));
          dispatch({ type: "notice/dismiss", id: expandNoticeId(nodeId) });
        } else {
          // Reading a node's edges costs time in its degree whatever the page size, so a side
          // over the threshold, or one whose size couldn't be checked, is only read once the
          // user confirmed that side: every side this reads is confirmed or known to be small.
          const unconfirmed = sides.filter(
            (side) => !confirmedSidesRef.current.has(sideKey(side)),
          );
          if (unconfirmed.some((side) => sizeOf(side) === "unknown")) {
            if (!(await probeNodes([nodeId], { automatic: false }))) return;
          }
          if (unconfirmed.some((side) => sizeOf(side) !== "small")) {
            warnExpand(node, {
              direction,
              edgeLabel,
              large: unconfirmed.filter((side) => sizeOf(side) === "large"),
            });
            return;
          }
        }
        const current = stateRef.current;
        const cursors =
          current.expansions.get(expansionKey(nodeId, edgeLabel)) ??
          UNSTARTED_EXPANSION;
        const careful = sides.some((side) => sizeOf(side) !== "small");
        let merges: (MergeResult | null)[];
        if (sides.every((side) => cursors[side] === "unstarted")) {
          const page = await expandPage(node, {
            direction,
            edgeLabel,
            after: null,
            sides,
            paced: careful,
          });
          merges = [page?.merge ?? null];
        } else {
          const open = sides.filter((side) => cursors[side] !== "done");
          if (open.length === 0) {
            notify({
              tone: "info",
              title: "Nothing more to expand",
              description: `Every ${edgeLabel ? `${edgeLabel} ` : ""}neighbor of ${caption(node)} is already loaded.`,
            });
            return;
          }
          const pageOf = (side: "out" | "in") => {
            const cursor = cursors[side];
            return pageSide(
              node,
              side,
              edgeLabel,
              cursor === "unstarted" || cursor === "done" ? null : cursor,
            );
          };
          if (careful) {
            // A side that reads all of its edges: one side at a time.
            merges = [];
            for (const side of open) {
              const merge = await pageOf(side);
              merges.push(merge);
              if (!merge) break;
            }
          } else {
            merges = await Promise.all(open.map(pageOf));
          }
        }
        if (
          merges.every(
            (merge) =>
              merge !== null &&
              merge.addedNodes.length === 0 &&
              merge.addedEdges.length === 0,
          )
        ) {
          const degree = stateRef.current.degrees.get(nodeId);
          const none = degree && degree.inDegree + degree.outDegree === 0;
          notify({
            tone: "info",
            title: none
              ? `${caption(node)} has no relationships`
              : "Nothing new to add",
            description: none
              ? undefined
              : `Every neighbor this expansion returned is already loaded.`,
          });
        }
      });
    },
    [
      capReached,
      dispatch,
      expandPage,
      notify,
      pageSide,
      probeNodes,
      track,
      warnExpand,
    ],
  );

  const loadMore = useCallback(
    (nodeId: string, direction: "out" | "in", edgeLabel?: string | null) =>
      expand(nodeId, { direction, edgeLabel }),
    [expand],
  );

  const loadLabelPage = useCallback(
    async (label: string) => {
      await track(`page:${label}`, async () => {
        const start = stateRef.current;
        const page = start.labelPages[label];
        if (page?.exhausted) {
          notify({
            tone: "info",
            title: `Every ${label} node has been loaded`,
          });
          return;
        }
        if (canvasRoom(start, CAP) <= 0) {
          capReached();
          return;
        }
        if (start.hiddenLabels.has(labelKey("node", label))) {
          dispatch({
            type: "label/hidden",
            key: labelKey("node", label),
            hidden: false,
          });
        }
        // Other data under a label the loads share is told apart by a
        // missing load, which no index finds: the page filters the label.
        const { scope } = optionsRef.current;
        if (
          !page &&
          scope.kind === "other" &&
          scope.loads.length > 0 &&
          isShared(scope, "node", label)
        )
          notify({
            tone: "info",
            title: `Filtering every ${label} node`,
            description: `The loads hold ${label} nodes too: finding the ones no load wrote reads the whole label, which takes a while when it is large.`,
          });
        dispatch({ type: "labelPage/loading", label });
        const target = EXPLORER_LIMITS.pageSize;
        const added: string[] = [];
        let after = page?.nextAfter ?? null;
        // Rows of the label already paged past: later pages skip them (see buildLabelPageRequest).
        let offset = after === null ? 0 : (page?.loaded ?? 0);
        // Pages start at the lowest id, so the first ones are often already on
        // the canvas from the sample: keep going until something new arrives.
        for (
          let requests = 0;
          requests < LABEL_PAGE_MAX_REQUESTS && added.length < target;
          requests += 1
        ) {
          const capacity = canvasRoom(stateRef.current, CAP);
          if (capacity <= 0) {
            capReached();
            break;
          }
          const pageSize = Math.min(target, capacity);
          const shape = stateRef.current.shape;
          const result = await run(
            `Load ${label} nodes`,
            buildLabelPageRequest({
              scope: optionsRef.current.scope,
              label,
              afterDbId: after,
              offset,
              pageSize,
              shape,
            }),
            { automatic: false },
          );
          if (!result) return;
          if (!result.ok) {
            dispatch({ type: "labelPage/failed", label });
            notify({
              tone: "error",
              title: `Couldn't load ${label} nodes`,
              failure: result,
            });
            return;
          }
          const parsed = parseLabelPage(result.body, shape, pageSize, after);
          const merge = mergeInto({ nodes: parsed.nodes });
          added.push(...merge.addedNodes);
          dispatch({
            type: "labelPage/loaded",
            label,
            nextAfter: parsed.nextAfter,
            total: parsed.total,
            count: parsed.consumed,
          });
          after = parsed.nextAfter;
          offset += parsed.consumed;
          if (after === null) break;
        }
        if (added.length === 0) {
          notify({
            tone: "info",
            title: `No new ${label} nodes`,
            description: "Everything this page returned is already loaded.",
          });
        }
        await connectNew(added);
      });
    },
    [capReached, connectNew, dispatch, mergeInto, notify, run, track],
  );

  const loadFullNode = useCallback(
    async (nodeId: string) => {
      await track(`full:${nodeId}`, async () => {
        const node = stateRef.current.store.nodes.get(nodeId);
        if (!node) return;
        const full = fallbackShape();
        const result = await run(
          `Load every property of ${caption(node)}`,
          buildNodesByIdRequest({ dbIds: [node.dbId], shape: full }),
          { automatic: false },
        );
        if (!result) return;
        if (!result.ok) {
          notify({
            tone: "error",
            title: `Couldn't load ${caption(node)}`,
            failure: result,
          });
          return;
        }
        const nodes = parseNodes(result.body, full);
        if (nodes.length === 0) {
          notify({ tone: "info", title: `${caption(node)} no longer exists` });
          return;
        }
        mergeInto({ nodes });
        dispatch({
          type: "shape/extended",
          shape: extendShape(stateRef.current.shape, nodes),
        });
      });
    },
    [dispatch, mergeInto, notify, run, track],
  );

  /**
   * A node's degree: the probe (no edges read) unless one is known, then a
   * bounded count of the sides it found small and non-empty (at most
   * `degreeThreshold` + 1 edges each, or every edge of them while edge labels
   * are hidden: see buildDegreeRequest). Large sides stay floors. Keyed per
   * view: selecting another node drops the previous one's, and opening
   * another menu the previous menu's.
   */
  const fetchDegree = useCallback(
    (nodeId: string, view: "inspector" | "menu" = "inspector") => {
      const current = stateRef.current;
      const node = current.store.nodes.get(nodeId);
      if (
        !node ||
        current.degrees.has(nodeId) ||
        current.degreeStatus.get(nodeId) === "loading"
      )
        return;
      dispatch({ type: "degree/loading", nodeId });
      void (async () => {
        let probe: DegreeProbe | undefined =
          stateRef.current.probes.get(nodeId);
        const key = `degree:${view}`;
        if (!probe) {
          const result = await run(
            `Check the size of ${caption(node)}`,
            buildDegreeProbeRequest({ dbIds: [node.dbId] }),
            { key },
          );
          if (!result) return dispatch({ type: "degree/cancelled", nodeId });
          probe = result.ok
            ? parseDegreeProbe(result.body, [node.dbId]).get(node.dbId)
            : undefined;
          if (!probe) return dispatch({ type: "degree/failed", nodeId });
          dispatch({
            type: "probe/loaded",
            probes: new Map([[nodeId, probe]]),
          });
        }
        const known = probe;
        const sides = (["out", "in"] as const).filter(
          (side) => !isLargeProbe(known, side) && known[side] > 0,
        );
        if (sides.length === 0) {
          // Every side is empty or large: the probe says all the explorer counts on its own.
          const degree = parseDegree({}, { sides: [], probe: known });
          return dispatch({
            type: "degree/loaded",
            nodeId,
            degree,
            hasLabels: true,
          });
        }
        const result = await run(
          `Count relationships of ${caption(node)}`,
          buildDegreeRequest({
            dbId: node.dbId,
            sides,
            excludeEdgePrefix: optionsRef.current.exclude?.edgePrefix,
          }),
          { key },
        );
        if (!result) return dispatch({ type: "degree/cancelled", nodeId });
        if (!result.ok) return dispatch({ type: "degree/failed", nodeId });
        const degree: DegreeInfo = parseDegree(result.body, {
          sides,
          probe: known,
        });
        dispatch({ type: "degree/loaded", nodeId, degree, hasLabels: true });
      })();
    },
    [dispatch, run],
  );

  /** The cheap probe only (opening a context menu while every edge label shows): enough to say "1,000+" and to disable empty sides. */
  const probeDegree = useCallback(
    (nodeId: string) => {
      const current = stateRef.current;
      if (
        !current.store.nodes.has(nodeId) ||
        nodeSize(current, nodeId) !== "unknown"
      )
        return;
      void track(`probe:${nodeId}`, () => probeNodes([nodeId]));
    },
    [probeNodes, track],
  );

  /** Exact counts per edge label, reading every edge of the node. Explicit only; it costs time in the degree. */
  const countRelationships = useCallback(
    async (nodeId: string) => {
      await track(`count:${nodeId}`, async () => {
        const node = stateRef.current.store.nodes.get(nodeId);
        if (!node) return;
        const result = await run(
          `Count every relationship of ${caption(node)}`,
          buildDegreeRequest({
            dbId: node.dbId,
            bounded: false,
            excludeEdgePrefix: optionsRef.current.exclude?.edgePrefix,
          }),
          { automatic: false },
        );
        if (!result) return;
        if (!result.ok) {
          notify({
            tone: "error",
            title: `Couldn't count the relationships of ${caption(node)}`,
            description: isTimeout(result)
              ? "Reading every relationship took too long. The counts so far stay shown."
              : undefined,
            failure: isTimeout(result) ? undefined : result,
          });
          return;
        }
        dispatch({
          type: "degree/loaded",
          nodeId,
          degree: parseDegree(result.body, { bounded: false }),
          hasLabels: true,
        });
      });
    },
    [dispatch, notify, run, track],
  );

  const connectNodes = useCallback(
    async (nodeIds: string[], opts: { confirmed?: boolean } = {}) => {
      dispatch({
        type: "notice/dismiss",
        id: opts.confirmed ? CONNECT_LARGE_NOTICE_ID : CONNECT_NOTICE_ID,
      });
      await track("connect", () =>
        connectNew(nodeIds, {
          mode: opts.confirmed ? "confirmed" : "explicit",
        }),
      );
    },
    [connectNew, dispatch, track],
  );

  /**
   * Searches the database in two requests: the id lookup and BM25 (index
   * backed, fast), and the `contains` scan over light string keys (reads
   * whole labels when matches are rare). A slow or failed scan never costs
   * the other hits; a failing text index is dropped and the id lookup rerun.
   * Resolves null when a newer search, a refresh or Clear superseded it.
   */
  const search = useCallback(
    async (
      text: string,
      options: SearchOptions = {},
    ): Promise<SearchOutcome | null> => {
      const current = stateRef.current;
      const shape = current.shape;
      const trimmed = text.trim();
      const idCandidate = parseIdCandidate(text);
      const exclude = optionsRef.current.exclude;
      const textIndexes = textIndexesFor(optionsRef.current.indexes).filter(
        (index) => !excludes(exclude, "node", index.label),
      );
      // An id never needs a substring scan, and the scan is the slow part.
      const containsTargets =
        idCandidate === null
          ? containsTargetsFor(current.schemas, textIndexes).filter(
              (target) => !excludes(exclude, "node", target.label),
            )
          : [];
      // An id lookup finds any node: one of an excluded label isn't a hit.
      const shown = (hits: SearchHit[]) =>
        hits.filter((hit) => !excludes(exclude, "node", hit.node.label));
      // Every source is scoped; text hits are filtered to the scope after
      // the read, so text indexes read more of them.
      const base = {
        scope: optionsRef.current.scope,
        text,
        idCandidate,
        textIndexes,
        containsTargets: [],
        k: EXPLORER_LIMITS.searchK,
        overFetch: optionsRef.current.textOverFetch,
        shape,
      };
      // The previous search's scan is no longer wanted (the indexed request is aborted by its key).
      keyedRef.current.get(SEARCH_SCAN_KEY)?.abort();

      const indexed = async (): Promise<SearchOutcome | null> => {
        let args = base;
        let request = buildSearchRequest(args);
        if (!request) return { hits: [], failure: null };
        let result = await run(`Search for “${trimmed}”`, request, {
          key: SEARCH_KEY,
          automatic: false,
        });
        let failure: QueryFailure | null = null;
        if (
          result &&
          !result.ok &&
          args.textIndexes.length > 0 &&
          textSourceFailure(result)
        ) {
          // A text index failed: one the catalog lists but the database lacks
          // (not worth mentioning), or one that errs or is too slow (reported
          // with whatever else is found). Keep the id lookup.
          if (result.code !== "index_not_found") failure = result;
          args = { ...args, textIndexes: [] };
          request = buildSearchRequest(args);
          if (!request) return { hits: [], failure };
          result = await run(`Search for “${trimmed}” by id`, request, {
            key: SEARCH_KEY,
            automatic: false,
          });
        }
        if (!result) return null;
        if (!result.ok) return { hits: [], failure: result };
        return { hits: shown(parseSearch(result.body, shape, args)), failure };
      };

      const scanned = async (): Promise<SearchOutcome | null> => {
        const args = {
          ...base,
          idCandidate: null,
          textIndexes: [],
          containsTargets,
        };
        const request = buildSearchRequest(args);
        if (!request) return { hits: [], failure: null };
        const result = await run(
          `Search property values for “${trimmed}”`,
          request,
          {
            key: SEARCH_SCAN_KEY,
            automatic: false,
          },
        );
        if (!result) return null;
        if (!result.ok) return { hits: [], failure: result };
        return {
          hits: shown(parseSearch(result.body, shape, args)),
          failure: null,
        };
      };

      const scan = scanned();
      const first = await indexed();
      if (!first) {
        // Superseded: the newer search (or Clear) owns the scan too.
        return null;
      }
      if (containsTargets.length > 0) options.onPartial?.(first);
      const second = await scan;
      if (!second) return null;
      const hits = combineHits(first.hits, second.hits);
      // A hit's label exists; the next visit counts it without discovering it.
      if (hits.length > 0) {
        rememberLabels(optionsRef.current.storageKey, {
          nodes: hits.map((hit) => hit.node.label),
        });
      }
      return { hits, failure: first.failure ?? second.failure };
    },
    [run],
  );

  const unhide = useCallback(
    (nodes: readonly GraphNode[]) => {
      const current = stateRef.current;
      dispatch({ type: "nodes/show", ids: nodes.map((node) => node.id) });
      for (const label of new Set(nodes.map((node) => node.label))) {
        if (current.hiddenLabels.has(labelKey("node", label))) {
          dispatch({
            type: "label/hidden",
            key: labelKey("node", label),
            hidden: false,
          });
        }
      }
    },
    [dispatch],
  );

  const revealNode = useCallback(
    async (node: GraphNode): Promise<string | null> => {
      if (excludes(optionsRef.current.exclude, "node", node.label)) return null;
      const existing = stateRef.current.store.nodes.get(node.id);
      if (existing) {
        unhide([existing]);
        return node.id;
      }
      const merge = mergeInto({ nodes: [node] });
      if (merge.addedNodes.length === 0) {
        capReached();
        return null;
      }
      unhide([node]);
      // Its size first: only a node known to be small is connected and expanded on its own. A
      // large one, or one whose probe failed, gets a notice offering each instead.
      if (!(await probeNodes([node.id]))) return node.id;
      const current = stateRef.current;
      if (!current.store.nodes.has(node.id)) return node.id;
      if (nodeSize(current, node.id) === "small") {
        await Promise.all([connectNew([node.id]), expand(node.id)]);
        return node.id;
      }
      if (current.store.nodes.size > 1) noteUnconnected([node.id]);
      const large = (["out", "in"] as const).filter(
        (side) => nodeSideSize(current, node.id, side) === "large",
      );
      warnExpand(node, { direction: "both", edgeLabel: null, large });
      return node.id;
    },
    [
      capReached,
      connectNew,
      expand,
      mergeInto,
      noteUnconnected,
      probeNodes,
      unhide,
      warnExpand,
    ],
  );

  const revealIdentity = useCallback(
    async (
      label: string,
      property: "entity_key" | "norm_name",
      value: string,
    ): Promise<string | null> => {
      const shape = stateRef.current.shape;
      const known = [...stateRef.current.store.nodes.values()].find(
        (n) => n.label === label && n.properties[property] === value,
      );
      if (known) return revealNode(known);
      const result = await run(
        `Find ${label} ${value}`,
        buildIdentityRequest({
          scope: optionsRef.current.scope,
          label,
          property,
          value,
          shape,
        }),
        { automatic: false },
      );
      if (!result || !result.ok) return null;
      const node = parseIdentity(result.body, shape);
      return node ? revealNode(node) : null;
    },
    [revealNode, run],
  );

  const loadNodeById = useCallback(
    async (dbId: number): Promise<string | null> => {
      const shape = stateRef.current.shape;
      const result = await run(
        `Load node #${dbId}`,
        buildNodesByIdRequest({ dbIds: [dbId], shape, probeDegree: true }),
      );
      if (!result) return null;
      if (!result.ok) {
        notify({
          tone: "error",
          title: `Couldn't load node #${dbId}`,
          failure: result,
        });
        return null;
      }
      const [node] = parseNodes(result.body, shape);
      if (!node) {
        notify({
          tone: "info",
          title: `Node #${dbId} wasn't found`,
          description: "It may have been deleted.",
        });
        return null;
      }
      const exclude = optionsRef.current.exclude;
      if (exclude && excludes(exclude, "node", node.label)) {
        notify({
          tone: "info",
          title: `${caption(node)} is hidden`,
          description: exclude.notice,
        });
        return null;
      }
      const probe = parseDegreeProbe(result.body, [dbId]).get(dbId);
      if (probe)
        dispatch({ type: "probe/loaded", probes: new Map([[node.id, probe]]) });
      const existing = stateRef.current.store.nodes.get(node.id);
      if (existing) {
        unhide([existing]);
        return node.id;
      }
      const merge = mergeInto({ nodes: [node] });
      if (merge.addedNodes.length === 0) {
        capReached();
        return null;
      }
      unhide([node]);
      // No expansion: a link only says which node to show. Connected only when small (the probe above).
      await connectNew([node.id]);
      return node.id;
    },
    [capReached, connectNew, dispatch, mergeInto, notify, run, unhide],
  );

  const showResult = useCallback(
    async (body: unknown): Promise<ShowResultOutcome | null> => {
      // Known labels tell edge rows without endpoints apart from node rows.
      const known = railLabels(stateRef.current);
      const extracted = extractGraphFromResult(body, {
        nodeLabels: known.nodes.map((entry) => entry.label),
        edgeLabels: known.edges.map((entry) => entry.label),
      });
      const edgeHint =
        "Return edges with edge_properties, which adds $from and $to.";
      const nothingToShow = (description: string) => {
        notify({
          tone: "info",
          title: "Nothing to show in the graph",
          description,
        });
        return null;
      };
      if (
        !extracted ||
        (extracted.graph.nodes.length === 0 &&
          extracted.graph.edges.length === 0)
      ) {
        return nothingToShow(
          extracted?.skippedEdgeRows
            ? `The result's rows are edges without their endpoints. ${edgeHint}`
            : "The result has no rows with $id and $label. Return nodes (value_map, or values with $id and $label) or edges (edge_properties).",
        );
      }

      // A row holds whatever the query projected (aliases, a few keys), and
      // an edge row without endpoints looks like a node row whose id may be
      // a real node's. So the canvas gets stored nodes, never rows: loaded
      // ones as they are, the rest fetched by id with the explorer's shape.
      // A row with no node behind it, or one of another label, is left out.
      const loaded = stateRef.current.store.nodes;
      const rowIds = extracted.graph.nodes
        .filter((node) => !loaded.has(node.id))
        .map((node) => node.dbId)
        .slice(0, CAP);
      const endpointIds = extracted.missingNodeDbIds
        .filter((dbId) => !loaded.has(nodeKey(dbId)))
        .slice(0, MAX_MISSING_ENDPOINTS);
      const fetchIds = [...new Set([...rowIds, ...endpointIds])];
      let fetched: Map<string, GraphNode> | null = null;
      if (fetchIds.length > 0) {
        const shape = stateRef.current.shape;
        const result = await run(
          `Load ${fetchIds.length.toLocaleString("en-US")} ${fetchIds.length === 1 ? "node" : "nodes"} of a query result`,
          // Their sizes come along, so connecting them needs no separate probe.
          buildNodesByIdRequest({ dbIds: fetchIds, shape, probeDegree: true }),
          { automatic: false },
        );
        if (!result) return null;
        if (result.ok) {
          fetched = new Map(
            parseNodes(result.body, shape).map((node) => [node.id, node]),
          );
          const probes = new Map<string, DegreeProbe>();
          for (const [dbId, probe] of parseDegreeProbe(result.body, fetchIds))
            probes.set(nodeKey(dbId), probe);
          dispatch({ type: "probe/loaded", probes });
        } else {
          // Fall back to the rows themselves; edge endpoints stay missing.
          notify({
            tone: "error",
            title: "Couldn't load the result's nodes",
            failure: result,
          });
        }
      }
      const current = stateRef.current.store.nodes;
      const requested = new Set(fetchIds);
      const nodes = new Map<string, GraphNode>();
      let notNodes = 0;
      let overCap = 0;
      for (const row of extracted.graph.nodes) {
        let node = current.get(row.id);
        if (!node && fetched) {
          if (!requested.has(row.dbId)) {
            // More new nodes than the canvas holds; the rest were not fetched.
            overCap += 1;
            continue;
          }
          node = fetched.get(row.id);
        }
        // Without a successful fetch the row stands in for its node.
        node ??= fetched ? undefined : row;
        if (node && node.label === row.label) nodes.set(node.id, node);
        else notNodes += 1;
      }
      if (overCap > 0) capReached();
      for (const dbId of endpointIds) {
        const node = fetched?.get(nodeKey(dbId));
        if (node && !nodes.has(node.id)) nodes.set(node.id, node);
      }
      const leftOut = extracted.skippedEdgeRows + notNodes;
      const drawable = (id: string) => nodes.has(id) || current.has(id);
      if (
        nodes.size === 0 &&
        !extracted.graph.edges.some(
          (edge) => drawable(edge.source) && drawable(edge.target),
        )
      ) {
        return nothingToShow(
          leftOut > 0
            ? `The result's rows aren't nodes in this database (edges without endpoints, say). ${edgeHint}`
            : "None of the result's nodes or edge endpoints could be loaded.",
        );
      }
      if (leftOut > 0) {
        const rows =
          leftOut === 1 ? "1 row" : `${leftOut.toLocaleString("en-US")} rows`;
        notify({
          tone: "info",
          title: `Left out ${rows} that ${leftOut === 1 ? "isn't a node" : "aren't nodes"}`,
          description: `Edge rows need their endpoints. ${edgeHint}`,
        });
      }
      const merge = mergeInto({
        nodes: [...nodes.values()],
        edges: extracted.graph.edges,
      });
      const store = stateRef.current.store;
      // Nodes the cap refused are not on the canvas.
      const present = [...nodes.values()].filter((node) =>
        store.nodes.has(node.id),
      );
      unhide(present);
      await connectNew(merge.addedNodes);
      return {
        nodeIds: present.map((node) => node.id),
        edgeIds: extracted.graph.edges
          .map((edge) => edge.id)
          .filter((id) => store.edges.has(id)),
      };
    },
    [capReached, connectNew, dispatch, mergeInto, notify, run, unhide],
  );

  const hideNodes = useCallback(
    (ids: string[]) => dispatch({ type: "nodes/hide", ids }),
    [dispatch],
  );
  const setLabelHidden = useCallback(
    (target: "node" | "edge", label: string, hidden: boolean) =>
      dispatch({ type: "label/hidden", key: labelKey(target, label), hidden }),
    [dispatch],
  );
  const showAll = useCallback(
    () => dispatch({ type: "hidden/reset" }),
    [dispatch],
  );
  const removeNodes = useCallback(
    (ids: string[]) => dispatch({ type: "nodes/remove", ids }),
    [dispatch],
  );
  const clearCanvas = useCallback(() => {
    // Running expansions and pages would refill the canvas; drop them.
    for (const controller of controllersRef.current) controller.abort();
    // Confirmations were about the nodes that just left: one loaded again asks again.
    confirmedSidesRef.current = new Set();
    dispatch({ type: "canvas/clear" });
  }, [dispatch]);
  const setPinned = useCallback(
    (nodeId: string, pinned: boolean) =>
      dispatch({ type: "pin", id: nodeId, pinned }),
    [dispatch],
  );
  const dismissNotice = useCallback(
    (id: string) => dispatch({ type: "notice/dismiss", id }),
    [dispatch],
  );
  const isPending = useCallback(
    (op: string) => stateRef.current.pending.has(op),
    [],
  );

  const visible = useMemo(
    () => visibleGraph(state.store, state.hiddenLabels, state.hiddenNodes),
    [state.store, state.hiddenLabels, state.hiddenNodes],
  );

  // The counted labels plus any loaded or probed since (one discovery missed, or written after the counts).
  const { overview, store, schemas, labelPages } = state;
  const labels = useMemo(
    () => railLabels({ overview, store, schemas, labelPages }),
    [overview, store, schemas, labelPages],
  );

  // Every label seen (counted, sampled, paged, expanded, found, shown from a
  // query) is remembered for this database, so the next visit counts it
  // without having to discover it again.
  useEffect(() => {
    if (labels.nodes.length === 0 && labels.edges.length === 0) return;
    rememberLabels(storageKey, {
      nodes: labels.nodes
        .filter((entry) => entry.count !== 0)
        .map((entry) => entry.label),
      edges: labels.edges
        .filter((entry) => entry.count !== 0)
        .map((entry) => entry.label),
    });
  }, [labels, storageKey]);

  // Labels without a count (seen after the boot, over the per-request cap, or
  // named by an index catalog that arrived late) are counted from their
  // bitmaps in the background: one cheap request at a time, each label once
  // it is counted. Those over the cap are all counted; later ones get a few
  // requests per boot. A batch that fails (or Clear cancels) is counted again
  // after a backoff, a few times at most.
  const phase = state.phase;
  const overviewStatus = overview.status;
  const scanning = state.pending.has("scan");
  const catalog = options.indexes;
  useEffect(() => {
    if (
      phase !== "ready" ||
      overviewStatus !== "ready" ||
      scanning ||
      lateBusyRef.current
    )
      return;
    const fromCatalog = shownLabels(indexLabels(catalog), exclusion);
    const pick = (
      target: "node" | "edge",
      rows: LabelCount[],
      extra: string[],
    ) =>
      [
        ...new Set([
          ...rows
            .filter((entry) => entry.count === null)
            .map((entry) => entry.label),
          ...extra,
        ]),
      ].filter((label) => !countedRef.current.has(countedKey(target, label)));
    const cap = EXPLORER_LIMITS.maxCountLabels;
    const pending = {
      nodes: pick("node", labels.nodes, fromCatalog.nodes),
      edges: pick("edge", labels.edges, fromCatalog.edges),
    };
    // Labels a counts request left out over its cap come first, and don't use up the budget for
    // labels seen later: the boot's notice says they will be counted, so every one of them is.
    const backlog = {
      nodes: pending.nodes
        .filter((label) => backlogRef.current.has(countedKey("node", label)))
        .slice(0, cap),
      edges: pending.edges
        .filter((label) => backlogRef.current.has(countedKey("edge", label)))
        .slice(0, cap),
    };
    const fromBacklog = backlog.nodes.length > 0 || backlog.edges.length > 0;
    if (!fromBacklog && lateCountsRef.current >= LATE_COUNT_MAX_REQUESTS)
      return;
    // What one request counts: Other data's shared labels take a count per load.
    const fit = countedLabels(
      fromBacklog
        ? backlog
        : {
            nodes: pending.nodes.slice(0, cap),
            edges: pending.edges.slice(0, cap),
          },
      optionsRef.current.scope,
    );
    const wanted = { nodes: fit.nodes, edges: fit.edges };
    if (wanted.nodes.length === 0 && wanted.edges.length === 0) return;
    const timer = window.setTimeout(() => {
      const generation = generationRef.current;
      lateBusyRef.current = true;
      markCounted(wanted);
      if (!fromBacklog) lateCountsRef.current += 1;
      void (async () => {
        const title = fromBacklog
          ? "Count the remaining labels"
          : "Count newly seen labels";
        const { scope } = optionsRef.current;
        const result = await run(
          title,
          buildLabelCountsRequest(wanted, { scope }),
        );
        // After a refresh the new boot owns the counts (and reset this bookkeeping).
        if (generation !== generationRef.current) return;
        if (result?.ok) {
          lateFailuresRef.current = 0;
          // Labels counted at zero (remembered, but deleted since) leave the rail and are forgotten.
          const gone = emptyLabels(
            result.body,
            wanted,
            optionsRef.current.scope,
          );
          dispatch({
            type: "overview/merged",
            stats: parseLabelCounts(result.body, wanted, scope),
            gone,
          });
          if (gone.nodes.length > 0 || gone.edges.length > 0) {
            rememberLabels(optionsRef.current.storageKey, {}, { forget: gone });
          }
        } else {
          // Failed, rate-limited past its retries, or cancelled by Clear: the labels stay
          // uncounted (and in the backlog), and are counted again after a wait.
          const retryDelays =
            optionsRef.current.lateCountRetryDelaysMs ??
            LATE_COUNT_RETRY_DELAYS_MS;
          const failures = (lateFailuresRef.current += 1);
          // Every retry failed too: the database is struggling, so background counting stays
          // stopped (busy) until a refresh.
          if (failures > retryDelays.length) return;
          await delay(retryDelays[failures - 1]);
          if (generation !== generationRef.current) return;
          unmarkCounted(wanted);
          if (!fromBacklog) lateCountsRef.current -= 1;
        }
        lateBusyRef.current = false;
        setLateCountRound((round) => round + 1);
      })();
    }, LATE_COUNT_DELAY_MS);
    return () => window.clearTimeout(timer);
  }, [
    phase,
    overviewStatus,
    scanning,
    catalog,
    exclusion,
    labels,
    lateCountRound,
    dispatch,
    markCounted,
    unmarkCounted,
    run,
  ]);

  return useMemo(
    () => ({
      state,
      visible,
      labels,
      refresh,
      retryOverview,
      scanLabels,
      loadSample,
      showMore,
      canShowMore,
      expand,
      loadMore,
      loadLabelPage,
      loadFullNode,
      fetchDegree,
      probeDegree,
      countRelationships,
      connectNodes,
      scanBlockedUntil,
      executeUserQuery,
      search,
      revealNode,
      revealIdentity,
      loadNodeById,
      showResult,
      hideNodes,
      setLabelHidden,
      showAll,
      removeNodes,
      clearCanvas,
      setPinned,
      dismissNotice,
      isPending,
    }),
    [
      state,
      visible,
      labels,
      refresh,
      retryOverview,
      scanLabels,
      loadSample,
      showMore,
      canShowMore,
      expand,
      loadMore,
      loadLabelPage,
      loadFullNode,
      fetchDegree,
      probeDegree,
      countRelationships,
      connectNodes,
      scanBlockedUntil,
      executeUserQuery,
      search,
      revealNode,
      revealIdentity,
      loadNodeById,
      showResult,
      hideNodes,
      setLabelHidden,
      showAll,
      removeNodes,
      clearCanvas,
      setPinned,
      dismissNotice,
      isPending,
    ],
  );
}
