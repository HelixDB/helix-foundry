import type {
  DatabaseQueryTarget,
  DynamicQueryRequest,
  QueryExecutor,
} from "../../database-query";

export type QueryMode = "read" | "write";

/** A query the explorer itself ran (loading a label, expanding a node...). */
export interface ExplorerActivityEntry {
  id: string;
  title: string;
  request: DynamicQueryRequest;
  at: number;
  ok: boolean | null;
  status: number | null;
  elapsedMs: number | null;
}

/** What the templates and the dock know about the database being explored. */
export interface QueryDockContext {
  nodeLabels: string[];
  edgeLabels: string[];
  selectedNode: { dbId: number; label: string } | null;
}

/**
 * A helix instance on the user's machine, explored directly instead of
 * through Helix Cloud (the standalone local explorer). "Copy as" then talks
 * to the instance itself.
 */
export interface LocalInstance {
  /** The instance's name in helix.toml, for `helix query <name>`. */
  name: string;
  /** Its base URL as the user's shell reaches it, e.g. `http://localhost:6969`. */
  url: string;
  /** The instance expects `Authorization: Bearer $HELIX_API_KEY`. */
  apiKey: boolean;
}

export interface QueryDockProps {
  databaseName: string;
  target: DatabaseQueryTarget;
  /** Gateway hostname for copy-as-curl. */
  connectionEndpoint?: string;
  /** Set when the database is a local instance; the snippets address it directly. */
  local?: LocalInstance;
  execute: QueryExecutor;
  /** Offer the Write mode toggle. */
  canWrite: boolean;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Loaded into the editor (and the dock opened) whenever `nonce` changes. */
  seed: { request: DynamicQueryRequest; nonce: number } | null;
  /** Queries the explorer itself ran, newest first. */
  activity: ExplorerActivityEntry[];
  context: QueryDockContext;
  onShowInGraph: (body: unknown) => void;
  /** localStorage namespace, e.g. `helix.explorer.tenant:<id>`. */
  storageKey: string;
  /** Hears the Read/Write toggle, so the explorer can show which mode is on. */
  onModeChange?: (mode: QueryMode) => void;
  /** Take the whole parent column instead of a resizable share (phones). */
  fill?: boolean;
  className?: string;
}
