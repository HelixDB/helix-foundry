import {
  memo,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  Info,
  PanelLeftOpen,
  PanelRightClose,
  PanelRightOpen,
  Tag,
  X,
} from "lucide-react";

import {
  createQueryExecutor,
  type DatabaseQueryTarget,
  type QueryExecutor,
} from "../database-query";
import { holds, type Scope } from "../load-scope";
import { PanelContent, usePanelSlot, useShell } from "../../../shell/context";
import { useCurrentTheme } from "../../../shell/theme";
import { cn } from "./ui";
import { excludes } from "./lib/exclusion";
import type { RequestLimiter } from "./lib/request-limiter";
import type { VersionLayout } from "./lib/seeding";
import type { IndexCatalogEntry, LabelExclusion } from "./lib/types";

import { GraphCanvas, type GraphCanvasHandle } from "./canvas/GraphCanvas";
import {
  ActivityIndicator,
  CanvasChip,
  CanvasNotices,
  CanvasToolbar,
  type CanvasNotice,
} from "./CanvasOverlays";
import { ContextMenu, type ContextMenuState } from "./ContextMenu";
import { ExplorerBar, ExplorerTools } from "./ExplorerBar";
import { ExplorerSearch } from "./ExplorerSearch";
import {
  BillingPausedState,
  CanvasLoading,
  EmptyCanvasState,
  EmptyDatabaseState,
  LoadFailedState,
  NoAccessState,
  StartingState,
} from "./ExplorerStates";
import { Inspector, type InspectorSubject } from "./Inspector";
import { LabelRail, type LabelFocus } from "./LabelRail";
import {
  containsTargetsFor,
  edgeToJson,
  loadedLabelCounts,
  nodeToJson,
  parseIdCandidate,
  pruneSelectionHistory,
  pushSelectionHistory,
  textIndexesFor,
  totalsAreFloors,
  type ExplorerNoticeAction,
  type GraphStore,
} from "./lib/graph-store";
import { captionFor, nodeKey } from "./lib/normalize";
import { edgeLabelPairs, inferLabelSchemas } from "./lib/schema";
import {
  asBoolean,
  asStringRecord,
  explorerStorageKey,
  readStored,
  SHOW_INTERNALS_KEY,
  writeStored,
} from "./lib/storage";
import { CANVAS_THEME, createLabelColors } from "./lib/theme";
import type { GraphNode, Selection } from "./lib/types";
import { describeFailure } from "./query/failure";
import { ShortcutsDialog } from "./ShortcutsDialog";
import { ICON_BUTTON, OUTLINE_BUTTON } from "./ui";
import {
  useExplorerGraph,
  type ExplorerNotifyEvent,
  type ExpandOptions,
} from "./useExplorerGraph";
import {
  useCoarsePointer,
  usePane,
  useSplitter,
  useViewportMode,
  type ViewportMode,
} from "./useExplorerLayout";

export interface DatabaseExplorerProps {
  /**
   * Where it sits. "page": it is the page, filling the main area: its labels
   * go in the open section's panel beside the sidebar (shell/context.ts; inline
   * while fullscreen, where the panel isn't), its inspector is the page's
   * right-hand sidebar, and `header` lays out the page's bar around the
   * explorer's tools. "side": one of two explorers side by side (a
   * compare), a bordered box with its own bar, labels and inspector.
   */
  frame:
    | { kind: "page"; header: (tools: ReactNode) => ReactNode }
    | { kind: "side" };
  target: DatabaseQueryTarget;
  /** What it reads: one load (its version's labels, a seeded first view) or Other data. */
  scope: Scope;
  /** The load's version, for its rail and first view. */
  layout?: VersionLayout;
  /** The database's limiter, shared with its other readers. */
  limiter?: RequestLimiter;
  /** Text search reads this many times its hits, filtering them to the scope. */
  textOverFetch?: number;
  /** In a load: shows Other data (the rail's group below the load's labels). */
  onShowOtherData?: () => void;
  /** Where a node came from, for the inspector ("Load v3 · 18 Oct", "Other data"). */
  provenance?: (node: GraphNode) => string;
  /** The selected node changed (compare follows it on the other side). */
  onSelectNode?: (node: GraphNode | null) => void;
  /** Selects and centers the node with this identity, loading it when needed (a newer seq asks again). */
  focus?: {
    label: string;
    property: "entity_key" | "norm_name";
    value: string;
    seq: number;
  } | null;
  /** Keeps `?node=` in step with the selection (one explorer per page does). */
  syncUrl?: boolean;
  /** Display name of the database. */
  databaseName: string;
  /** false when the user is known to lack database.query.read; null lets the server decide. */
  canRead: boolean | null;
  /** Index catalog entries when available; enables text search and index badges. */
  indexes?: IndexCatalogEntry[];
  /** Links the error and empty states use: the Helix dashboard's members and billing, and a load. */
  links?: { members?: string; billing?: string; load?: string };
  /** Shows a toast (Foundry's), for what the explorer reports. */
  notify?: (text: string, error?: boolean) => void;
  /** Opens a node's page in Foundry, for the nodes that have one. */
  openPage?: (node: GraphNode) => (() => void) | undefined;
  /** A node's readable name when Foundry knows one (its ontology's); else the explorer guesses. */
  caption?: (node: GraphNode) => string | undefined;
  /** Properties the inspector leaves out: the layout's own, not the data's. */
  internalProperty?: (
    target: "node" | "edge",
    label: string,
    key: string,
  ) => boolean;
  /**
   * Foundry's own labels. Hidden everywhere (rail, totals, search, canvas)
   * until the viewer turns on "Show Foundry internals" (remembered per
   * browser), then drawn and listed as internal. Turning it either way
   * reloads the graph.
   */
  internalLabels?: LabelExclusion;
  /** Override the query transport (tests). Defaults to the WFE broker for `target`. */
  execute?: QueryExecutor;
}

// The canvas only redraws for its own props; parent renders (search, inspector) shouldn't reach it.
const MemoGraphCanvas = memo(GraphCanvas);

const RAIL_BOUNDS = { min: 208, max: 400 };
const INSPECTOR_BOUNDS = { min: 300, max: 520 };
const LAPTOP_INSPECTOR_WIDTH = 320;
const DEGREE_DELAY_MS = 180;
const FLASH_MS = 6000;
const DEEP_LINK_SETTLE_MS = 1200;

type MobileSheet = "labels" | "inspector" | null;
type PendingCamera =
  | { kind: "node"; id: string }
  | { kind: "fit"; ids: string[] }
  | null;

function sameSelection(a: Selection | null, b: Selection | null): boolean {
  return (
    a === b || (a !== null && b !== null && a.kind === b.kind && a.id === b.id)
  );
}

/** History without entries for `current`: going "back" to what is already selected does nothing. */
function withoutSelection(
  history: Selection[],
  current: Selection | null,
): Selection[] {
  if (!current || !history.some((entry) => sameSelection(entry, current)))
    return history;
  return history.filter((entry) => !sameSelection(entry, current));
}

function inStore(store: GraphStore, selection: Selection): boolean {
  return selection.kind === "node"
    ? store.nodes.has(selection.id)
    : store.edges.has(selection.id);
}

function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  if (target.isContentEditable) return true;
  const tag = target.tagName;
  if (tag === "TEXTAREA" || tag === "SELECT") return true;
  if (tag !== "INPUT") return false;
  const type = (target as HTMLInputElement).type;
  return !["button", "checkbox", "radio", "range", "submit", "reset"].includes(
    type,
  );
}

function pendingLabel(pending: ReadonlyMap<string, string>): string | null {
  const ops = [...pending.keys()];
  const page = ops.find((op) => op.startsWith("page:"));
  if (page) return `Loading ${page.slice(5)} nodes…`;
  if (ops.some((op) => op.startsWith("expand:"))) return "Expanding…";
  if (ops.some((op) => op.startsWith("full:"))) return "Loading the full node…";
  if (ops.some((op) => op.startsWith("count:")))
    return "Counting every relationship…";
  if (ops.includes("connect")) return "Connecting…";
  if (ops.includes("sample")) return "Loading a sample…";
  if (ops.includes("scan")) return "Scanning every node and edge for labels…";
  if (ops.includes("overview")) return "Counting labels…";
  return null;
}

function fileSlug(name: string): string {
  return (
    name
      .replace(/[^a-z0-9-_]+/gi, "-")
      .replace(/^-+|-+$/g, "")
      .toLowerCase() || "graph"
  );
}

/** Connected groups of a graph and its nodes without an edge, by union-find. */
function graphShape(graph: {
  nodes: readonly { id: string }[];
  edges: readonly { source: string; target: string }[];
}): { components: number; isolated: number } {
  const parent = new Map(graph.nodes.map((n) => [n.id, n.id]));
  const root = (id: string): string => {
    let at = id;
    while (parent.get(at) !== at) at = parent.get(at)!;
    parent.set(id, at);
    return at;
  };
  const touched = new Set<string>();
  for (const e of graph.edges) {
    if (!parent.has(e.source) || !parent.has(e.target)) continue;
    touched.add(e.source);
    touched.add(e.target);
    parent.set(root(e.source), root(e.target));
  }
  return {
    components: new Set(graph.nodes.map((n) => root(n.id))).size,
    isolated: graph.nodes.filter((n) => !touched.has(n.id)).length,
  };
}

/** "⌘" on Apple devices, "Ctrl+" elsewhere: how shortcuts are written. */
const primaryModifier = () =>
  typeof navigator !== "undefined" &&
  /Mac|iPhone|iPad|iPod/i.test(navigator.platform || navigator.userAgent)
    ? "⌘"
    : "Ctrl+";

/**
 * The graph explorer for one database: a label rail, the canvas and an
 * inspector, ported from helix-website without its query dock (Foundry only
 * reads). It fills the page's content area.
 */
export function DatabaseExplorer({
  frame,
  target,
  scope,
  layout,
  limiter,
  textOverFetch,
  onShowOtherData,
  provenance,
  onSelectNode,
  focus,
  syncUrl = true,
  databaseName,
  canRead,
  indexes,
  links,
  notify: toast,
  openPage,
  caption,
  internalProperty,
  internalLabels,
  execute: injectedExecute,
}: DatabaseExplorerProps) {
  const execute = useMemo<QueryExecutor>(
    () => injectedExecute ?? createQueryExecutor(target),
    [injectedExecute, target],
  );
  const storageKey = explorerStorageKey(target);
  // A compare's side is half the page: it lays out as on a tablet (its
  // labels and inspector over its canvas), and as on a phone on one.
  const windowViewport = useViewportMode();
  const viewport: ViewportMode =
    frame.kind === "side" && windowViewport !== "mobile"
      ? "tablet"
      : windowViewport;
  const coarse = useCoarsePointer();
  const primary = useMemo(primaryModifier, []);

  const rootRef = useRef<HTMLDivElement>(null);
  const setRoot = useCallback((element: HTMLDivElement | null) => {
    rootRef.current = element;
  }, []);
  const canvasRef = useRef<GraphCanvasHandle>(null);
  const searchRef = useRef<HTMLInputElement>(null);

  // ============ Notifications ============

  const [fullscreen, setFullscreen] = useState(false);
  const [fullscreenAvailable, setFullscreenAvailable] = useState(false);
  const fullscreenRef = useRef(false);
  const [flashes, setFlashes] = useState<CanvasNotice[]>([]);
  const flashSeq = useRef(0);

  const notify = useCallback(
    (event: ExplorerNotifyEvent) => {
      const description = event.failure
        ? `${describeFailure(event.failure, "read").title}. ${event.failure.message}`
        : event.description;
      toast?.(
        description ? `${event.title}: ${description}` : event.title,
        event.tone === "error",
      );
      // Toasts render outside the explorer, so they're invisible in fullscreen (and absent without a notifier).
      if (fullscreenRef.current || !toast) {
        const id = `flash-${(flashSeq.current += 1)}`;
        setFlashes((list) => [
          ...list.slice(-2),
          {
            id,
            tone: event.tone === "error" ? "error" : "info",
            message: (
              <>
                <strong>{event.title}</strong>
                {description ? <span> — {description}</span> : null}
              </>
            ),
          },
        ]);
        window.setTimeout(
          () => setFlashes((list) => list.filter((flash) => flash.id !== id)),
          FLASH_MS,
        );
      }
    },
    [toast],
  );

  // ============ Foundry internals ============

  const [showInternals, setShowInternals] = useState(
    () => readStored(SHOW_INTERNALS_KEY, asBoolean) ?? false,
  );
  const changeShowInternals = useCallback((shown: boolean) => {
    setShowInternals(shown);
    writeStored(SHOW_INTERNALS_KEY, shown || null);
  }, []);
  const internalsHidden = !!internalLabels && !showInternals;
  const isInternal = useCallback(
    (target: "node" | "edge", label: string) =>
      excludes(internalLabels, target, label),
    [internalLabels],
  );

  const explorer = useExplorerGraph({
    execute,
    scope,
    layout,
    limiter,
    textOverFetch,
    canRead,
    indexes,
    storageKey,
    notify,
    exclude: internalsHidden ? internalLabels : undefined,
    // The gateway's rate limit applies to the WFE broker; an injected transport (tests, a local
    // harness) has none to stay under.
    pacing: injectedExecute ? false : undefined,
  });
  const { state, visible, labels } = explorer;
  // The operations are stable callbacks; depending on them (not on `explorer`, which changes with every
  // state update) keeps the handlers below stable too.
  const {
    refresh: rebootExplorer,
    clearCanvas: clearExplorer,
    revealNode,
    expand: expandNode,
    setPinned,
    hideNodes,
    dismissNotice: dismissExplorerNotice,
  } = explorer;
  const { store } = state;
  const storeRef = useRef(store);
  const pinnedRef = useRef(state.pinned);
  useLayoutEffect(() => {
    storeRef.current = store;
    pinnedRef.current = state.pinned;
  });

  // ============ Colors and captions ============

  const colors = useMemo(() => createLabelColors(), []);
  const labelNamesKey = labels.nodes.map((entry) => entry.label).join("\u0000");
  // A theme switch gives a new colorFor, so the rail, inspector and canvas
  // take the theme's hues (the canvas recomputes its palette and redraws).
  const theme = useCurrentTheme();
  const colorFor = useMemo(() => {
    // Register the sorted labels first so the first ten never share a color. Internal labels
    // take no hue: they draw grey, so the data's colors are the same whether they show or not.
    colors.register(
      (labelNamesKey ? labelNamesKey.split("\u0000") : []).filter(
        (label) => !isInternal("node", label),
      ),
    );
    return (label: string) =>
      isInternal("node", label)
        ? CANVAS_THEME.internalNode
        : colors.colorFor(label);
    // `theme` is read through colors.colorFor and CANVAS_THEME.
  }, [colors, labelNamesKey, isInternal, theme]);

  const captionsKey = `${storageKey}.captions`;
  const [captionKeys, setCaptionKeys] = useState<Record<string, string>>({});
  // State, not a ref: nothing is written until the stored choices have rendered, so a StrictMode
  // double run of these effects can't save the empty initial value over them.
  const [captionsLoaded, setCaptionsLoaded] = useState<string | null>(null);
  useEffect(() => {
    setCaptionKeys(readStored(captionsKey, asStringRecord) ?? {});
    setCaptionsLoaded(captionsKey);
  }, [captionsKey]);
  useEffect(() => {
    if (captionsLoaded !== captionsKey) return;
    writeStored(
      captionsKey,
      Object.keys(captionKeys).length ? captionKeys : null,
    );
  }, [captionKeys, captionsKey, captionsLoaded]);
  const changeCaptionKey = useCallback((label: string, key: string | null) => {
    setCaptionKeys((current) => {
      const next = { ...current };
      if (key) next[label] = key;
      else delete next[label];
      return next;
    });
  }, []);
  // A caption chosen for the label wins, then Foundry's name, then the guess.
  const captionOf = useCallback(
    (node: GraphNode) =>
      (!captionKeys[node.label] && caption?.(node)) ||
      captionFor(node, captionKeys[node.label]),
    [captionKeys, caption],
  );

  // ============ Selection ============

  const [selected, setSelected] = useState<Selection | null>(null);
  const selectedRef = useRef<Selection | null>(null);
  const [history, setHistory] = useState<Selection[]>([]);
  const historyRef = useRef<Selection[]>([]);
  useLayoutEffect(() => {
    historyRef.current = history;
  });
  const [labelFocus, setLabelFocus] = useState<LabelFocus | null>(null);

  const [cameraTick, setCameraTick] = useState(0);
  const pendingCamera = useRef<PendingCamera>(null);
  const focusSoon = useCallback((id: string) => {
    pendingCamera.current = { kind: "node", id };
    setCameraTick((tick) => tick + 1);
  }, []);

  const select = useCallback((next: Selection | null) => {
    const previous = selectedRef.current;
    if (sameSelection(previous, next)) return;
    // Re-selecting something from history (X → nothing → X) mustn't leave "Back to X" behind.
    setHistory((list) =>
      withoutSelection(pushSelectionHistory(list, previous, next), next),
    );
    selectedRef.current = next;
    setSelected(next);
    if (next) setLabelFocus(null);
  }, []);

  /** Returns whether there was somewhere to go back to. */
  const goBack = useCallback((): boolean => {
    const pruned = withoutSelection(
      pruneSelectionHistory(historyRef.current, storeRef.current),
      selectedRef.current,
    );
    const previous = pruned[pruned.length - 1];
    if (!previous) {
      if (pruned !== historyRef.current) setHistory(pruned);
      return false;
    }
    setHistory(pruned.slice(0, -1));
    selectedRef.current = previous;
    setSelected(previous);
    setLabelFocus(null);
    if (previous.kind === "node") focusSoon(previous.id);
    return true;
  }, [focusSoon]);

  // Elements that leave the store take the selection (and history entries) with them.
  useEffect(() => {
    const current = selectedRef.current;
    if (current && !inStore(store, current)) {
      selectedRef.current = null;
      setSelected(null);
    }
    setHistory((list) => pruneSelectionHistory(list, store));
  }, [store]);

  const resetSelection = useCallback(() => {
    selectedRef.current = null;
    setSelected(null);
    setHistory([]);
    setLabelFocus(null);
  }, []);

  // ============ Deep link (?node=<id>) ============
  // A link only says which node to show: it is loaded by id and selected, never expanded, and
  // connected to the canvas only when it is small (the selection then probes its size, no edges).

  const deepLink = useRef<"waiting" | "loading" | "done">("waiting");
  /** The deep-linked node being loaded; selected as soon as it reaches the store. */
  const deepLinkTarget = useRef<string | null>(null);
  const { loadNodeById } = explorer;
  useEffect(() => {
    if (deepLink.current !== "waiting") return;
    if (!syncUrl) {
      deepLink.current = "done";
      return;
    }
    if (state.phase !== "ready") {
      // A failed load can be retried (or recover through a label page): keep the link until it's ready.
      if (state.phase === "empty" || state.phase === "no-access")
        deepLink.current = "done";
      return;
    }
    const dbId = parseIdCandidate(
      new URLSearchParams(window.location.search).get("node") ?? "",
    );
    // Something picked meanwhile wins over the link.
    if (dbId === null || selectedRef.current) {
      deepLink.current = "done";
      return;
    }
    const id = nodeKey(dbId);
    const existing = storeRef.current.nodes.get(id);
    if (existing) {
      deepLink.current = "done";
      // Its label (or the node) may be hidden, which is remembered per database: show it.
      void revealNode(existing);
      select({ kind: "node", id });
      // The first layout is still settling; center on the node once it has (if it's still selected).
      window.setTimeout(() => {
        if (selectedRef.current?.id === id) focusSoon(id);
      }, DEEP_LINK_SETTLE_MS);
      return;
    }
    deepLink.current = "loading";
    deepLinkTarget.current = id;
    void loadNodeById(dbId).then((loaded) => {
      deepLink.current = "done";
      deepLinkTarget.current = null;
      if (!loaded || selectedRef.current) return;
      select({ kind: "node", id: loaded });
      focusSoon(loaded);
    });
  }, [state.phase, loadNodeById, revealNode, select, focusSoon]);

  // A loaded deep link is selected when the node lands, not after its neighbors have loaded too.
  useEffect(() => {
    const id = deepLinkTarget.current;
    if (!id || !store.nodes.has(id)) return;
    deepLinkTarget.current = null;
    if (selectedRef.current) return;
    select({ kind: "node", id });
    focusSoon(id);
  }, [store, select, focusSoon]);

  // Keep ?node= in step with the selected node, without a navigation. A
  // node picked while the link is still unresolved (the first view loading)
  // wins over the link, as above, so it is written at once.
  useEffect(() => {
    if (!syncUrl) return;
    if (deepLink.current !== "done") {
      if (!selected) return;
      deepLink.current = "done";
    }
    const url = new URL(window.location.href);
    const node =
      selected?.kind === "node" ? store.nodes.get(selected.id) : undefined;
    const wanted = node ? String(node.dbId) : null;
    if (url.searchParams.get("node") === wanted) return;
    if (wanted) url.searchParams.set("node", wanted);
    else url.searchParams.delete("node");
    window.history.replaceState(
      null,
      "",
      `${url.pathname}${url.search}${url.hash}`,
    );
  }, [selected, store]);

  // ============ Following another explorer (compare) ============

  const selectedNode =
    selected?.kind === "node" ? store.nodes.get(selected.id) : undefined;
  const onSelectNodeRef = useRef(onSelectNode);
  onSelectNodeRef.current = onSelectNode;
  useEffect(() => {
    onSelectNodeRef.current?.(selectedNode ?? null);
  }, [selectedNode?.id]);
  const { revealIdentity } = explorer;
  useEffect(() => {
    if (!focus || state.phase !== "ready") return;
    let live = true;
    void revealIdentity(focus.label, focus.property, focus.value).then((id) => {
      if (!live || !id) return;
      select({ kind: "node", id });
      focusSoon(id);
    });
    return () => {
      live = false;
    };
  }, [focus?.seq, state.phase === "ready"]);

  // How the drawn graph hangs together: its connected groups and the nodes
  // with no edge drawn (on the canvas element, for tests and support, as is
  // the count of nodes outside the scope below).
  const drawn = useMemo(() => graphShape(visible), [visible]);

  // Nodes outside the scope (reached over an edge someone added, or another
  // load's): drawn dashed, never hidden.
  const outside = useMemo(() => {
    const ids = new Set<string>();
    for (const node of store.nodes.values())
      if (!holds(scope, node.properties)) ids.add(node.id);
    return ids;
  }, [store, scope]);

  // ============ Layout ============

  const rail = usePane(
    `${storageKey}.rail`,
    { open: true, width: 256 },
    RAIL_BOUNDS,
  );
  const inspectorPane = usePane(
    `${storageKey}.inspector`,
    { open: true, width: 360 },
    INSPECTOR_BOUNDS,
  );
  const [railFlyout, setRailFlyout] = useState(false);
  const [inspectorOverlay, setInspectorOverlay] = useState(false);
  const [mobileSheet, setMobileSheet] = useState<MobileSheet>(null);
  const [shortcutsOpen, setShortcutsOpen] = useState(false);
  const [menu, setMenu] = useState<ContextMenuState | null>(null);
  const [sampleNoticeDismissed, setSampleNoticeDismissed] = useState(false);

  const mobile = viewport === "mobile";
  // The labels are the section panel's, unless the explorer is a compare's
  // side or fullscreen (the panel isn't on screen then). The panel shows
  // them while it is open: beside the sidebar, or in a phone's drawer, which
  // `slot.onNavigate` closes.
  const docked = frame.kind === "page" && !fullscreen;
  const slot = usePanelSlot(),
    { togglePanel } = useShell();

  // Camera moves wait until the canvas has the node (it syncs before this effect runs) and is on
  // screen: a hidden canvas has no size to center in.
  useEffect(() => {
    const pending = pendingCamera.current;
    if (!pending || !canvasRef.current) return;
    const shown = new Set(visible.nodes.map((node) => node.id));
    if (pending.kind === "node") {
      // Picking something else meanwhile cancels the move (it follows the selection, not old picks).
      if (
        !sameSelection(selectedRef.current, { kind: "node", id: pending.id })
      ) {
        pendingCamera.current = null;
        return;
      }
      // A node that is still being expanded moves as its neighbors arrive; it stays put once they have.
      if (!shown.has(pending.id) || state.pending.has(`expand:${pending.id}`))
        return;
      canvasRef.current.focusNode(pending.id, { animate: true });
    } else {
      const ids = pending.ids.filter((id) => shown.has(id));
      if (ids.length === 0) return;
      canvasRef.current.fitToView({ nodeIds: ids, animate: true });
    }
    pendingCamera.current = null;
  }, [visible, cameraTick, state.pending, selected]);

  // Degree counts for the inspector, fetched once the selection settles: the cheap probe (no edges
  // read), then exact counts of the sides it found small. Large sides stay "1,000+" until the user
  // asks for an exact count.
  const selectedNodeId = selected?.kind === "node" ? selected.id : null;
  const selectedDegree =
    selectedNodeId === null
      ? null
      : state.degrees.has(selectedNodeId)
        ? "loaded"
        : (state.degreeStatus.get(selectedNodeId) ?? "none");
  const { fetchDegree, probeDegree } = explorer;
  useEffect(() => {
    if (!selectedNodeId || selectedDegree !== "none") return;
    const timer = window.setTimeout(
      () => fetchDegree(selectedNodeId),
      DEGREE_DELAY_MS,
    );
    return () => window.clearTimeout(timer);
  }, [selectedNodeId, selectedDegree, fetchDegree]);

  const previousViewport = useRef<ViewportMode>(viewport);
  useEffect(() => {
    if (previousViewport.current === viewport) return;
    previousViewport.current = viewport;
    setRailFlyout(false);
    setInspectorOverlay(false);
    setMobileSheet(null);
  }, [viewport]);

  // On tablets the inspector slides over the canvas when something is picked.
  useEffect(() => {
    if (viewport === "tablet" && selected) setInspectorOverlay(true);
  }, [selected, viewport]);

  const railSplitter = useSplitter({
    width: rail.width,
    ...RAIL_BOUNDS,
    edge: "right",
    onResize: rail.setWidth,
  });
  const inspectorSplitter = useSplitter({
    width: inspectorPane.width,
    ...INSPECTOR_BOUNDS,
    edge: "left",
    onResize: inspectorPane.setWidth,
  });

  const toggleRail = useCallback(() => {
    if (docked) togglePanel();
    else if (viewport === "desktop") rail.setOpen((open) => !open);
    else if (viewport === "mobile")
      setMobileSheet((sheet) => (sheet === "labels" ? null : "labels"));
    else setRailFlyout((open) => !open);
  }, [docked, togglePanel, viewport, rail]);
  // Whether the inspector shows where it docks (the toggle in the page's bar).
  const inspectorShown =
    viewport === "desktop" || viewport === "laptop"
      ? inspectorPane.open
      : viewport === "tablet"
        ? inspectorOverlay
        : mobileSheet === "inspector";
  const toggleInspector = useCallback(() => {
    if (viewport === "desktop" || viewport === "laptop")
      inspectorPane.setOpen((open) => !open);
    else if (viewport === "mobile")
      setMobileSheet((sheet) => (sheet === "inspector" ? null : "inspector"));
    else setInspectorOverlay((open) => !open);
  }, [viewport, inspectorPane]);

  useEffect(() => {
    const onChange = () => {
      const on =
        document.fullscreenElement !== null &&
        document.fullscreenElement === rootRef.current;
      fullscreenRef.current = on;
      setFullscreen(on);
    };
    setFullscreenAvailable(
      Boolean(document.fullscreenEnabled && rootRef.current?.requestFullscreen),
    );
    document.addEventListener("fullscreenchange", onChange);
    return () => document.removeEventListener("fullscreenchange", onChange);
  }, []);
  const toggleFullscreen = useCallback(() => {
    if (document.fullscreenElement)
      void document.exitFullscreen().catch(() => {});
    else void rootRef.current?.requestFullscreen?.().catch(() => {});
  }, []);

  // ⌘K opens Foundry's command palette, which renders outside the explorer and so would be
  // invisible (yet modal) in fullscreen. Leave fullscreen and let the palette have the key: this
  // neither binds nor stops it. Window capture, like the palette's own listener.
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (
        !document.fullscreenElement ||
        document.fullscreenElement !== rootRef.current
      )
        return;
      if (
        (event.metaKey || event.ctrlKey) &&
        !event.altKey &&
        event.key.toLowerCase() === "k"
      )
        void document.exitFullscreen().catch(() => {});
    };
    window.addEventListener("keydown", onKeyDown, { capture: true });
    return () =>
      window.removeEventListener("keydown", onKeyDown, { capture: true });
  }, []);

  // ============ Actions ============

  const refresh = useCallback(
    (options?: { small?: boolean }) => {
      resetSelection();
      setMenu(null);
      setSampleNoticeDismissed(false);
      rebootExplorer(options);
    },
    [rebootExplorer, resetSelection],
  );

  const clearCanvas = useCallback(() => {
    resetSelection();
    clearExplorer();
  }, [clearExplorer, resetSelection]);

  // The hook reports the scan's outcome itself, so the rail and the overview behave the same.
  const { scanLabels: scanExplorerLabels } = explorer;
  const scanLabels = useCallback(
    () => void scanExplorerLabels(),
    [scanExplorerLabels],
  );

  const chooseSearchResult = useCallback(
    (node: GraphNode, loaded: boolean) => {
      if (viewport === "mobile") setMobileSheet(null);
      const id = node.id;
      // revealNode adds (and unhides) the node right away; its connections and neighbors load after.
      // Select it now: the camera waits for the expansion by itself, and a later pick wins.
      const revealed = revealNode(node);
      select({ kind: "node", id });
      focusSoon(id);
      if (loaded) return;
      void revealed.then((result) => {
        // The canvas was full, so nothing was added: drop the selection of a node that isn't there.
        if (
          result === null &&
          selectedRef.current?.id === id &&
          !storeRef.current.nodes.has(id)
        ) {
          selectedRef.current = null;
          setSelected(null);
        }
      });
    },
    [revealNode, viewport, select, focusSoon],
  );

  const expand = useCallback(
    (nodeId: string, options?: ExpandOptions) =>
      void expandNode(nodeId, options),
    [expandNode],
  );
  const togglePin = useCallback(
    (nodeId: string) =>
      setPinned(
        nodeId,
        storeRef.current.nodes.has(nodeId) && !pinnedRef.current.has(nodeId),
      ),
    [setPinned],
  );
  const hideNode = useCallback(
    (nodeId: string) => {
      hideNodes([nodeId]);
      if (selectedRef.current?.id === nodeId) select(null);
    },
    [hideNodes, select],
  );
  /** Copies to the clipboard and says whether it worked (the context menu is gone by then). */
  const copyText = useCallback(
    async (what: string, text: string, note?: string) => {
      try {
        if (!navigator.clipboard?.writeText) throw new Error("unavailable");
        await navigator.clipboard.writeText(text);
        notify({ tone: "info", title: `Copied ${what}`, description: note });
      } catch {
        notify({
          tone: "error",
          title: `Couldn't copy ${what}`,
          description: window.isSecureContext
            ? "The browser blocked access to the clipboard."
            : "The clipboard needs a secure (https) connection.",
        });
      }
    },
    [notify],
  );
  const copy = useCallback(
    (label: string, text: string) =>
      void copyText(label === "json" ? "JSON" : label, text),
    [copyText],
  );
  const copyJson = useCallback(
    (selection: Selection) => {
      const current = storeRef.current;
      if (selection.kind === "node") {
        const node = current.nodes.get(selection.id);
        if (!node) return;
        const note =
          !node.complete && node.omitted.length > 0
            ? `Without ${node.omitted.join(", ")}, which ${node.omitted.length === 1 ? "isn't" : "aren't"} loaded.`
            : undefined;
        void copyText("JSON", JSON.stringify(nodeToJson(node), null, 2), note);
      } else {
        const edge = current.edges.get(selection.id);
        if (edge)
          void copyText(
            "JSON",
            JSON.stringify(edgeToJson(edge, current), null, 2),
          );
      }
    },
    [copyText],
  );

  const exportPng = useCallback(async () => {
    const blob = await canvasRef.current?.exportPng();
    if (!blob) {
      notify({ tone: "info", title: "Nothing to export yet" });
      return;
    }
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = `${fileSlug(databaseName)}-graph.png`;
    link.click();
    window.setTimeout(() => URL.revokeObjectURL(url), 1000);
  }, [databaseName, notify]);

  const openMenu = useCallback(
    (
      menuTarget: Selection | null,
      point: { clientX: number; clientY: number },
    ) => {
      setMenu({ target: menuTarget, x: point.clientX, y: point.clientY });
      if (menuTarget?.kind !== "node") return;
      // The probe reads no edges and says enough to mark a large node "1,000+", but it counts
      // every edge: while Foundry's internals are hidden, the menu reads the degree, which
      // leaves them out (and reads the edges of the node's small sides).
      if (internalsHidden) fetchDegree(menuTarget.id, "menu");
      else probeDegree(menuTarget.id);
    },
    [fetchDegree, internalsHidden, probeDegree],
  );
  const closeMenu = useCallback(() => setMenu(null), []);

  const focusLabel = useCallback(
    (focus: LabelFocus | null) => {
      setLabelFocus(focus);
      if (focus) select(null);
      if (focus && viewport === "tablet") setInspectorOverlay(true);
    },
    [select, viewport],
  );

  // ============ Keyboard ============

  const keyHandler = useRef<(event: KeyboardEvent) => void>(() => {});
  keyHandler.current = (event: KeyboardEvent) => {
    if (event.defaultPrevented) return;
    const root = rootRef.current;
    if (!root) return;
    const target = event.target instanceof Node ? event.target : null;
    const inExplorer =
      !target ||
      target === document.body ||
      target === document.documentElement ||
      root.contains(target) ||
      (docked && !!slot?.element.contains(target));
    if (
      !inExplorer ||
      (target instanceof Element &&
        target.closest('[role="dialog"],[role="alertdialog"]'))
    )
      return;
    if (shortcutsOpen) return;

    if (event.metaKey || event.ctrlKey) return;
    // Fields own their keys, Esc included.
    if (isTypingTarget(event.target)) return;
    if (event.key === "Escape") {
      if (menu) setMenu(null);
      else if (mobileSheet) setMobileSheet(null);
      else if (railFlyout) setRailFlyout(false);
      else if (selectedRef.current) select(null);
      else if (labelFocus) setLabelFocus(null);
      else if (inspectorOverlay) setInspectorOverlay(false);
      else return;
      event.preventDefault();
      return;
    }
    if (event.altKey) {
      // Alt+← is the browser's Back on Windows and Linux: take it only when there's a selection to return to.
      if (event.key === "ArrowLeft" && goBack()) event.preventDefault();
      return;
    }
    const current = selectedRef.current;
    const nodeId =
      current?.kind === "node" && storeRef.current.nodes.has(current.id)
        ? current.id
        : null;
    switch (event.key) {
      case "/":
        event.preventDefault();
        searchRef.current?.focus();
        searchRef.current?.select();
        return;
      case "?":
        event.preventDefault();
        setShortcutsOpen(true);
        return;
      case "[":
        event.preventDefault();
        toggleRail();
        return;
      case "]":
        event.preventDefault();
        toggleInspector();
        return;
      case "r":
      case "R":
        event.preventDefault();
        canvasRef.current?.relayout();
        return;
      case "e":
      case "E":
        if (!nodeId) return;
        event.preventDefault();
        expand(nodeId);
        return;
      case "h":
      case "H":
        if (!nodeId) return;
        event.preventDefault();
        hideNode(nodeId);
        return;
      case "p":
      case "P":
        if (!nodeId) return;
        event.preventDefault();
        togglePin(nodeId);
        return;
    }
  };
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => keyHandler.current(event);
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  // ============ Derived views ============

  const storeGraph = useMemo(
    () => ({
      nodes: [...store.nodes.values()],
      edges: [...store.edges.values()],
    }),
    [store],
  );
  const schemas = useMemo(
    () => inferLabelSchemas(storeGraph, state.schemas, indexes),
    [storeGraph, state.schemas, indexes],
  );
  const pairs = useMemo(() => edgeLabelPairs(storeGraph), [storeGraph]);
  const loadedCounts = useMemo(() => loadedLabelCounts(store), [store]);
  const hiddenCount = state.hiddenLabels.size + state.hiddenNodes.size;
  const hasTextIndexes = useMemo(
    () => textIndexesFor(indexes).length > 0,
    [indexes],
  );
  // The labels a database search scans for exact-case text (the same targets the hook's search uses).
  const searchScanLabels = useMemo(
    () =>
      containsTargetsFor(state.schemas, textIndexesFor(indexes)).map(
        (target) => target.label,
      ),
    [state.schemas, indexes],
  );

  const emphasis = useMemo(() => {
    if (!labelFocus) return null;
    const ids = new Set<string>();
    if (labelFocus.target === "node") {
      for (const node of visible.nodes)
        if (node.label === labelFocus.label) ids.add(node.id);
    } else {
      for (const edge of visible.edges) {
        if (edge.label !== labelFocus.label) continue;
        ids.add(edge.source);
        ids.add(edge.target);
      }
    }
    return ids;
  }, [labelFocus, visible]);

  const subject = useMemo<InspectorSubject>(() => {
    if (selected?.kind === "node") {
      const node = store.nodes.get(selected.id);
      if (node) return { kind: "node", node };
    }
    if (selected?.kind === "edge") {
      const edge = store.edges.get(selected.id);
      if (edge) return { kind: "edge", edge };
    }
    if (labelFocus) return { kind: "label", focus: labelFocus };
    return { kind: "overview" };
  }, [selected, store, labelFocus]);

  const back = useMemo(() => {
    const previous = [...history]
      .reverse()
      .find((entry) => inStore(store, entry));
    if (!previous) return null;
    if (previous.kind === "node") {
      const node = store.nodes.get(previous.id);
      return node ? { title: captionOf(node) } : null;
    }
    const edge = store.edges.get(previous.id);
    return edge ? { title: `${edge.label} edge` } : null;
  }, [history, store, captionOf]);

  const stats = state.overview.stats;
  const phase = state.phase;
  const booting = phase === "idle" || phase === "loading";
  const activity = pendingLabel(state.pending);
  /** No access or billing paused: nothing here can be queried. */
  const blocked =
    phase === "no-access" ||
    (phase === "error" && state.bootError?.kind === "disabled");

  // A new database that doesn't take reads yet: its card stays on screen
  // while the explorer boots again, by itself after the wait the transport
  // gave or on Retry, until a boot ends otherwise.
  const bootStarting =
    phase === "error" && state.bootError?.kind === "starting"
      ? state.bootError
      : null;
  const [starting, setStarting] = useState(bootStarting);
  if (bootStarting !== starting && (bootStarting || !booting))
    setStarting(bootStarting);
  useEffect(() => {
    if (!starting || booting) return;
    const timer = window.setTimeout(() => refresh(), starting.retryInMs);
    return () => window.clearTimeout(timer);
  }, [starting, booting, refresh]);

  const fatal =
    blocked ||
    !!starting ||
    (phase === "error" && !stats && store.nodes.size === 0);

  const { connectNodes, expand: expandWith } = explorer;
  const runNoticeAction = useCallback(
    (id: string, action: ExplorerNoticeAction) => {
      dismissExplorerNotice(id);
      if (action.kind === "connect")
        void connectNodes(action.nodeIds, { confirmed: action.confirmed });
      else
        void expandWith(action.nodeId, {
          direction: action.direction,
          edgeLabel: action.edgeLabel,
          confirmed: true,
        });
    },
    [connectNodes, dismissExplorerNotice, expandWith],
  );
  // Totals are floors when labels were found by sampling or some are uncounted; the bar, the
  // overview and the sample notice all say "at least" from the same test.
  const floors = useMemo(() => totalsAreFloors(stats, labels), [stats, labels]);
  const notices = useMemo<CanvasNotice[]>(() => {
    const list: CanvasNotice[] = state.notices.map((notice) => {
      const action = notice.action;
      return {
        id: notice.id,
        tone: notice.tone,
        message: notice.message,
        ...(action
          ? {
              action: {
                label: action.label,
                onClick: () => runNoticeAction(notice.id, action),
              },
            }
          : {}),
      };
    });
    if (
      phase === "ready" &&
      state.sample.truncated &&
      !sampleNoticeDismissed &&
      store.nodes.size > 0
    ) {
      const total = stats?.nodeTotal ?? null;
      const of = floors.nodes ? "at least " : "";
      list.unshift({
        id: "sample",
        tone: "info",
        message:
          total !== null && total >= store.nodes.size
            ? `Showing a sample of ${store.nodes.size.toLocaleString("en-US")} of ${of}${total.toLocaleString("en-US")} nodes. Load more from Labels or double-click a node to expand it.`
            : "Showing a sample. Load more from Labels or double-click a node to expand it.",
      });
    }
    return [...list, ...flashes];
  }, [
    state.notices,
    state.sample.truncated,
    phase,
    sampleNoticeDismissed,
    store.nodes.size,
    stats,
    floors,
    flashes,
    runNoticeAction,
  ]);
  const dismissNotice = useCallback(
    (id: string) => {
      if (id === "sample") setSampleNoticeDismissed(true);
      else if (id.startsWith("flash-"))
        setFlashes((list) => list.filter((flash) => flash.id !== id));
      else dismissExplorerNotice(id);
    },
    [dismissExplorerNotice],
  );

  // ============ Panels ============

  // The selected node's page in Foundry, when it has one.
  const subjectPage =
    subject.kind === "node" ? openPage?.(subject.node) : undefined;

  const renderRail = (onClose?: () => void, closeIcon?: "panel" | "close") => (
    <LabelRail
      nodeLabels={labels.nodes}
      edgeLabels={labels.edges}
      loading={
        booting && labels.nodes.length === 0 && labels.edges.length === 0
      }
      countsFailed={state.overview.status === "error"}
      countsRetrying={state.pending.has("overview")}
      onRetryCounts={explorer.retryOverview}
      countsPartial={floors.nodes || floors.edges}
      scanning={state.pending.has("scan")}
      scanBlockedUntil={explorer.scanBlockedUntil}
      onScanLabels={scope.kind === "other" ? scanLabels : undefined}
      otherData={
        scope.kind === "load" && onShowOtherData
          ? {
              onShow: () => {
                if (docked) slot?.onNavigate?.();
                onShowOtherData();
              },
            }
          : undefined
      }
      loaded={loadedCounts}
      hiddenLabels={state.hiddenLabels}
      hiddenCount={hiddenCount}
      focus={labelFocus}
      colorFor={colorFor}
      isLoadingLabel={(label) => state.pending.has(`page:${label}`)}
      isLabelExhausted={(label) => state.labelPages[label]?.exhausted ?? false}
      onFocus={(focus) => {
        focusLabel(focus);
        if (focus && docked) slot?.onNavigate?.();
        if (viewport === "mobile" && focus) setMobileSheet("inspector");
      }}
      onLoad={(label) => void explorer.loadLabelPage(label)}
      onToggleHidden={explorer.setLabelHidden}
      onShowAll={explorer.showAll}
      internals={
        internalLabels
          ? {
              shown: showInternals,
              onShownChange: changeShowInternals,
              isInternal,
            }
          : undefined
      }
      onCollapse={onClose}
      collapseIcon={closeIcon}
      touch={coarse || viewport === "mobile"}
    />
  );

  const renderInspector = (
    onClose?: () => void,
    closeIcon?: "panel" | "close",
  ) => (
    <Inspector
      subject={subject}
      databaseName={databaseName}
      explorer={explorer}
      schemas={schemas}
      pairs={pairs}
      colorFor={colorFor}
      captionOf={captionOf}
      captionKeys={captionKeys}
      onCaptionKeyChange={changeCaptionKey}
      internalProperty={internalProperty}
      back={back}
      onBack={goBack}
      onSelect={(selection) => {
        select(selection);
        if (selection.kind === "node") focusSoon(selection.id);
      }}
      onFocusLabel={focusLabel}
      onFocusNode={(id) => {
        if (viewport === "mobile") setMobileSheet(null);
        canvasRef.current?.focusNode(id, { animate: true });
      }}
      onOpenPage={subjectPage && (() => subjectPage())}
      provenance={
        provenance &&
        ((node: GraphNode) => ({
          text: provenance(node),
          outside: outside.has(node.id),
        }))
      }
      primaryModifier={primary}
      touch={coarse || viewport === "mobile"}
      booting={booting}
      onCollapse={onClose}
      collapseIcon={closeIcon}
    />
  );

  // ============ Canvas area ============

  let canvasState: ReactNode = null;
  if (booting) {
    canvasState = (
      <CanvasLoading
        databaseName={databaseName}
        step={state.overview.status === "loading" ? "overview" : "sample"}
      />
    );
  } else if (phase === "empty") {
    canvasState = (
      <EmptyDatabaseState
        databaseName={databaseName}
        loadHref={links?.load}
        internalsHidden={internalsHidden}
        other={scope.kind === "other" && scope.loads.length > 0}
      />
    );
  } else if (phase === "error" && state.bootError) {
    canvasState = (
      <LoadFailedState
        failure={state.bootError}
        stage={state.bootFailedAt ?? "sample"}
        onRetry={() => refresh()}
        onSmaller={() => refresh({ small: true })}
      />
    );
  } else if (phase === "ready" && visible.nodes.length === 0) {
    canvasState = (
      <EmptyCanvasState
        databaseName={databaseName}
        labels={labels.nodes}
        colorFor={colorFor}
        everythingHidden={store.nodes.size > 0}
        onShowAll={explorer.showAll}
        onLoadSample={explorer.loadSample}
        loadingSample={state.pending.has("sample")}
        onLoadLabel={(label) => void explorer.loadLabelPage(label)}
      />
    );
  }

  const canvasArea = (
    <div
      className="ex-canvas-area"
      data-components={drawn.components}
      data-isolated={drawn.isolated}
      data-outside={outside.size}
    >
      <MemoGraphCanvas
        graph={visible}
        colorForLabel={colorFor}
        captionFor={captionOf}
        selected={selected}
        emphasis={emphasis}
        pinned={state.pinned}
        outside={outside}
        onSelect={select}
        onExpand={expand}
        onContextMenu={openMenu}
        onPinnedChange={setPinned}
        ariaLabel={`Graph of ${databaseName}: ${visible.nodes.length} nodes, ${visible.edges.length} edges`}
        handleRef={canvasRef}
      />

      {!booting && phase !== "empty" && (
        <div className="ex-overlay is-top-left">
          {store.nodes.size > 0 && (
            <CanvasChip
              nodes={visible.nodes.length}
              edges={visible.edges.length}
              hidden={hiddenCount}
              onShowAll={explorer.showAll}
              onClear={clearCanvas}
              more={
                explorer.canShowMore
                  ? {
                      onShowMore: () => void explorer.showMore(),
                      pending: state.pending.has("more"),
                    }
                  : undefined
              }
            />
          )}
          <CanvasNotices notices={notices} onDismiss={dismissNotice} />
        </div>
      )}
      <div className="ex-overlay is-top-right">
        <ActivityIndicator label={booting ? null : activity} />
        {frame.kind === "side" &&
          viewport === "tablet" &&
          !inspectorOverlay && (
            <button
              type="button"
              onClick={() => setInspectorOverlay(true)}
              aria-label="Show inspector"
              title="Show inspector (])"
              className={cn(ICON_BUTTON, "is-raised")}
            >
              <PanelRightOpen aria-hidden="true" size={16} />
            </button>
          )}
      </div>

      {canvasState && <div className="ex-overlay is-center">{canvasState}</div>}

      <CanvasToolbar
        onZoomIn={() => canvasRef.current?.zoomBy(1.3)}
        onZoomOut={() => canvasRef.current?.zoomBy(1 / 1.3)}
        onFit={() => canvasRef.current?.fitToView({ animate: true })}
        onRelayout={() => canvasRef.current?.relayout()}
        onResetView={() => canvasRef.current?.resetView()}
        onExport={() => void exportPng()}
        onToggleFullscreen={toggleFullscreen}
        fullscreen={fullscreen}
        fullscreenAvailable={fullscreenAvailable}
        disabled={visible.nodes.length === 0}
        className={cn("ex-overlay is-bottom", mobile ? "is-right" : "is-left")}
      />

      {viewport === "tablet" && inspectorOverlay && (
        <aside aria-label="Inspector" className="ex-flyout is-right">
          {renderInspector(() => setInspectorOverlay(false), "close")}
        </aside>
      )}
    </div>
  );

  const search = (
    <ExplorerSearch
      inputRef={searchRef}
      nodes={visible.nodes}
      isLoaded={(id) => storeRef.current.nodes.has(id)}
      captionOf={captionOf}
      colorFor={colorFor}
      search={explorer.search}
      onChoose={chooseSearchResult}
      disabled={fatal}
      hasTextIndexes={hasTextIndexes}
      scanLabels={searchScanLabels}
    />
  );

  // ============ Render ============

  let fatalView: ReactNode = null;
  if (fatal) {
    const failure = state.bootError;
    fatalView = (
      <div className="ex-fatal">
        {starting ? (
          <StartingState
            databaseName={databaseName}
            onRetry={() => refresh()}
          />
        ) : phase === "no-access" ? (
          <NoAccessState
            databaseName={databaseName}
            membersHref={links?.members}
            failure={failure}
          />
        ) : failure?.kind === "disabled" ? (
          <BillingPausedState billingHref={links?.billing} failure={failure} />
        ) : failure ? (
          <LoadFailedState
            failure={failure}
            stage={state.bootFailedAt ?? "sample"}
            onRetry={() => refresh()}
            onSmaller={() => refresh({ small: true })}
          />
        ) : null}
      </div>
    );
  }
  const body = (
    <div className="ex-body">
      {!docked &&
        !fatal &&
        viewport === "desktop" &&
        (rail.open ? (
          <aside
            aria-label="Labels"
            className="ex-side is-left"
            style={{ width: rail.width }}
          >
            {renderRail(() => rail.setOpen(false), "panel")}
            <div
              {...railSplitter.props}
              aria-label="Resize labels panel"
              className={cn(
                "ex-splitter is-right",
                railSplitter.dragging && "is-dragging",
              )}
            />
          </aside>
        ) : (
          <RailStrip onOpen={() => rail.setOpen(true)} />
        ))}
      {!docked && !fatal && viewport === "laptop" && (
        <RailStrip
          onOpen={() => setRailFlyout((open) => !open)}
          expanded={railFlyout}
        />
      )}

      <div className="ex-main">{fatal ? fatalView : canvasArea}</div>

      {!fatal &&
        (viewport === "desktop" || viewport === "laptop") &&
        (inspectorPane.open ? (
          <aside
            aria-label="Inspector"
            className="ex-side is-right"
            style={{
              width:
                viewport === "desktop"
                  ? inspectorPane.width
                  : LAPTOP_INSPECTOR_WIDTH,
            }}
          >
            {viewport === "desktop" && (
              <div
                {...inspectorSplitter.props}
                aria-label="Resize inspector"
                className={cn(
                  "ex-splitter is-left",
                  inspectorSplitter.dragging && "is-dragging",
                )}
              />
            )}
            {/* As the page's sidebar, the bar's toggle hides it. */}
            {frame.kind === "page"
              ? renderInspector()
              : renderInspector(() => inspectorPane.setOpen(false), "panel")}
          </aside>
        ) : (
          frame.kind === "side" && (
            <InspectorStrip onOpen={() => inspectorPane.setOpen(true)} />
          )
        ))}

      {!docked && !fatal && viewport === "laptop" && railFlyout && (
        <>
          <button
            type="button"
            aria-label="Close labels"
            tabIndex={-1}
            onClick={() => setRailFlyout(false)}
            className="ex-scrim is-clear is-after-strip"
          />
          <aside
            aria-label="Labels"
            className="ex-flyout is-left is-after-strip"
          >
            {renderRail(() => setRailFlyout(false), "close")}
          </aside>
        </>
      )}
      {!docked && !fatal && viewport === "tablet" && railFlyout && (
        <>
          <button
            type="button"
            aria-label="Close labels"
            tabIndex={-1}
            onClick={() => setRailFlyout(false)}
            className="ex-scrim"
          />
          <aside aria-label="Labels" className="ex-flyout is-left">
            {renderRail(() => setRailFlyout(false), "close")}
          </aside>
        </>
      )}
      {!fatal && mobile && mobileSheet && (
        <>
          <button
            type="button"
            aria-label="Close panel"
            tabIndex={-1}
            onClick={() => setMobileSheet(null)}
            className="ex-scrim"
          />
          <aside
            aria-label={mobileSheet === "labels" ? "Labels" : "Inspector"}
            className="ex-sheet"
          >
            {mobileSheet === "labels"
              ? renderRail(() => setMobileSheet(null), "close")
              : renderInspector(() => setMobileSheet(null), "close")}
          </aside>
        </>
      )}
    </div>
  );

  const menuNode =
    menu?.target?.kind === "node"
      ? (store.nodes.get(menu.target.id) ?? null)
      : null;
  const menuEdge =
    menu?.target?.kind === "edge"
      ? (store.edges.get(menu.target.id) ?? null)
      : null;
  const menuPage = menuNode ? openPage?.(menuNode) : undefined;

  const bar = {
    search: fatal ? null : search,
    stats:
      phase !== "ready"
        ? null
        : {
            nodes: visible.nodes.length,
            edges: visible.edges.length,
            totalNodes: stats?.nodeTotal ?? null,
            totalEdges: stats?.edgeTotal ?? null,
            atLeast: floors,
          },
    onRefresh: () => refresh(),
    refreshing: booting && phase !== "idle",
    onShortcuts: () => setShortcutsOpen(true),
  };

  return (
    <div ref={setRoot} className={cn("ex", frame.kind === "page" && "is-page")}>
      {frame.kind === "page" ? (
        frame.header(
          <ExplorerTools
            {...bar}
            trailing={
              !fatal &&
              !mobile && (
                <button
                  type="button"
                  onClick={toggleInspector}
                  aria-label={
                    inspectorShown ? "Hide inspector" : "Show inspector"
                  }
                  aria-expanded={inspectorShown}
                  title={`${inspectorShown ? "Hide" : "Show"} inspector (])`}
                  className={ICON_BUTTON}
                >
                  {inspectorShown ? (
                    <PanelRightClose aria-hidden="true" size={16} />
                  ) : (
                    <PanelRightOpen aria-hidden="true" size={16} />
                  )}
                </button>
              )
            }
          />,
        )
      ) : (
        <ExplorerBar
          {...bar}
          databaseName={databaseName}
          leading={
            viewport === "tablet" && !fatal ? (
              <button
                type="button"
                onClick={() => setRailFlyout((open) => !open)}
                aria-pressed={railFlyout}
                title="Labels ([)"
                className={cn(OUTLINE_BUTTON, railFlyout && "is-on")}
              >
                <Tag aria-hidden="true" size={14} />
                Labels
              </button>
            ) : null
          }
        />
      )}

      {body}

      {mobile && !fatal && (
        <nav aria-label="Explorer panels" className="ex-tabbar">
          {(
            [
              {
                id: "labels",
                label: "Labels",
                icon: Tag,
                active: !docked && mobileSheet === "labels",
                // Docked, the labels are in the navigation drawer's panel.
                onClick: docked
                  ? togglePanel
                  : () =>
                      setMobileSheet((s) => (s === "labels" ? null : "labels")),
              },
              {
                id: "inspector",
                label: "Inspector",
                icon: Info,
                active: mobileSheet === "inspector",
                dot: subject.kind !== "overview",
                onClick: () =>
                  setMobileSheet((s) =>
                    s === "inspector" ? null : "inspector",
                  ),
              },
            ] as const
          ).map((tab) => {
            const Icon = tab.icon;
            return (
              <button
                key={tab.id}
                type="button"
                aria-pressed={tab.active}
                onClick={tab.onClick}
                className={cn("ex-tab", tab.active && "is-active")}
              >
                <span className="ex-tab-icon">
                  <Icon aria-hidden="true" size={16} />
                  {"dot" in tab && tab.dot && (
                    <span aria-hidden="true" className="ex-tab-dot" />
                  )}
                </span>
                {tab.label}
              </button>
            );
          })}
        </nav>
      )}

      {menu && (
        <ContextMenu
          menu={menu}
          node={menuNode}
          edge={menuEdge}
          source={menuEdge ? (store.nodes.get(menuEdge.source) ?? null) : null}
          target={menuEdge ? (store.nodes.get(menuEdge.target) ?? null) : null}
          degree={menuNode ? state.degrees.get(menuNode.id) : undefined}
          probe={menuNode ? state.probes.get(menuNode.id) : undefined}
          edgesHidden={internalsHidden}
          degreeLoading={
            menuNode
              ? state.degreeStatus.get(menuNode.id) === "loading" ||
                state.pending.has(`probe:${menuNode.id}`)
              : false
          }
          pinned={menuNode ? state.pinned.has(menuNode.id) : false}
          captionOf={captionOf}
          onClose={closeMenu}
          onExpand={expand}
          onFocusNode={(id) =>
            canvasRef.current?.focusNode(id, { animate: true })
          }
          onHide={hideNode}
          onTogglePin={togglePin}
          onCopy={copy}
          fallbackFocus={() =>
            rootRef.current?.querySelector<HTMLElement>(
              'canvas[role="application"]',
            ) ?? null
          }
          onCopyJson={copyJson}
          onOpenPage={menuPage && (() => menuPage())}
          onSelect={(selection) => {
            select(selection);
            if (selection.kind === "node") focusSoon(selection.id);
          }}
          onFit={() => canvasRef.current?.fitToView({ animate: true })}
          onRelayout={() => canvasRef.current?.relayout()}
          onResetView={() => canvasRef.current?.resetView()}
          onExportPng={() => void exportPng()}
        />
      )}

      {docked && !fatal && (
        <PanelContent label="Labels">
          <div className="ex is-panel">{renderRail()}</div>
        </PanelContent>
      )}

      <ShortcutsDialog
        open={shortcutsOpen}
        onOpenChange={setShortcutsOpen}
        primaryModifier={primary}
      />
    </div>
  );
}

function RailStrip({
  onOpen,
  expanded,
}: {
  onOpen: () => void;
  expanded?: boolean;
}) {
  return (
    <div className="ex-strip is-left">
      <button
        type="button"
        onClick={onOpen}
        aria-label="Show labels"
        aria-expanded={expanded}
        title="Show labels ([)"
        className={cn(ICON_BUTTON, expanded && "is-on")}
      >
        {expanded ? (
          <X aria-hidden="true" size={16} />
        ) : (
          <PanelLeftOpen aria-hidden="true" size={16} />
        )}
      </button>
      <button
        type="button"
        onClick={onOpen}
        aria-label="Labels"
        title="Labels"
        className={ICON_BUTTON}
      >
        <Tag aria-hidden="true" size={16} />
      </button>
    </div>
  );
}

function InspectorStrip({ onOpen }: { onOpen: () => void }) {
  return (
    <div className="ex-strip is-right">
      <button
        type="button"
        onClick={onOpen}
        aria-label="Show inspector"
        title="Show inspector (])"
        className={ICON_BUTTON}
      >
        <PanelRightOpen aria-hidden="true" size={16} />
      </button>
    </div>
  );
}
