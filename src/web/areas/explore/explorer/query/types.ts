import type { DynamicQueryRequest } from "../../database-query";

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
