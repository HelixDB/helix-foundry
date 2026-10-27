import { keepPreviousData, useQuery } from "@tanstack/react-query";
import type { Connector, OntologyGenerationTable } from "../../../shared/wfe";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { connectorsQuery } from "../data/data-api";
import {
  documentQueries,
  documentSetQueries,
} from "../documents/documents-api";
import { fileRead } from "../documents/model";
import type { SchemaMember } from "./suggested-ontology";
import { agentSourceId, type SourceRef } from "./model";

// Names the tables a version reads. Definition v1 names a connector and a
// table; an agent bundle names a source id: for a synced table a hash of
// the two, for a table file of a document set the document's own id. Ids
// are computed for every table the connectors have synced and for every
// table the version's generation drafted from, so a version still names a
// table a later sync dropped; a table file is named while the workspace
// holds it. Row counts come from the newest sync, or the file's read.

// A load's People enrichment profiles come from ENRICHMENT_SOURCE
// (./enrichment-fields.ts), which is no synced table or table file:
// resolve() names none, and pages name its rows themselves, as profiles of
// that load.

/**
 * A table a version reads, named: a connector's synced table, or a table
 * file of a document set. `connector` and `table` are what lists say ("Shop
 * database · orders", "Contracts · renewals.csv"), `kind` the logo, and
 * `to` its page in Data.
 */
export type SourceInfo = {
  // The connector's name, or the file's set's; the connector's id once it
  // is deleted.
  connector: string;
  // The connector's kind ("postgres"); "upload" for a file; none once the
  // connector is deleted.
  kind: string;
  // The table's name, or the file's.
  table: string;
  rows?: number;
  to: string;
} & (
  | { source: "connector"; connectorId: string }
  | { source: "document"; documentId: string }
);

const none: Connector[] = [];

export function useSources(drafted: OntologyGenerationTable[] = []) {
  const { id } = useWorkspace();
  const connectors = useQuery(connectorsQuery(id)),
    documents = useQuery(documentQueries.workspace(id)),
    sets = useQuery(documentSetQueries.list(id));
  const list = connectors.data?.connectors || none;
  const info = (connectorId: string, table: string): SourceInfo => {
    const c = list.find((x) => x.id === connectorId),
      rows = c?.tables?.find((t) => t.name === table)?.rowCount;
    return {
      source: "connector",
      connectorId,
      connector: c?.name || c?.kind || connectorId,
      kind: c?.kind || "",
      table,
      rows: rows === undefined ? undefined : Number(rows),
      to: paths.syncedTable(connectorId, table),
    };
  };
  // A table file, by its document id: the source id its mappings name.
  const file = (documentId: string): SourceInfo | undefined => {
    const d = documents.data?.find((x) => x.id === documentId);
    if (!d) return undefined;
    const read = fileRead(d);
    return {
      source: "document",
      documentId,
      connector: sets.data?.find((s) => s.id === d.setId)?.name || "Documents",
      kind: "upload",
      table: d.name || d.id,
      rows: read.state === "ready" ? read.rows : undefined,
      to: paths.dataDocument(d.id),
    };
  };
  const pairs = [
    ...list.flatMap((c) =>
      (c.tables || []).map((t) => [c.id, t.name || ""] as const),
    ),
    ...drafted.map((t) => [t.connectorId || "", t.table || ""] as const),
  ];
  // Keyed by the tables alone: a connectors refetch that changes none of
  // them (a sync poll) keeps the ids, and new tables keep the old ones
  // until theirs are hashed.
  const agentIds = useQuery({
    queryKey: [
      id,
      "connectors",
      "agent-source-ids",
      pairs.map((p) => p.join("/")).join(","),
    ],
    enabled: connectors.isSuccess,
    staleTime: Infinity,
    placeholderData: keepPreviousData,
    queryFn: async () =>
      new Map(
        await Promise.all(
          pairs.map(
            async ([c, t]) => [await agentSourceId(c, t), { c, t }] as const,
          ),
        ),
      ),
  });
  const resolve = (ref: SourceRef): SourceInfo | undefined => {
    if (ref.kind === "table")
      return connectors.isSuccess
        ? info(ref.connectorId, ref.table)
        : undefined;
    const pair = agentIds.data?.get(ref.sourceId);
    return pair ? info(pair.c, pair.t) : file(ref.sourceId);
  };
  return {
    connectors: list,
    // Changes whenever what resolve() answers may change: for memos.
    key: `${connectors.dataUpdatedAt}:${agentIds.dataUpdatedAt}:${documents.dataUpdatedAt}:${sets.dataUpdatedAt}`,
    // The names are still loading.
    pending:
      connectors.isPending ||
      documents.isPending ||
      (connectors.isSuccess && agentIds.isPending),
    resolve,
    byAgentId: (sourceId: string) => resolve({ kind: "agent", sourceId }),
    // A schema card's member: the table, its connector's logo and name.
    member: (ref: SourceRef): SchemaMember => {
      const found = resolve(ref);
      const tableId =
          ref.kind === "table"
            ? `${ref.connectorId}/${ref.table}`
            : ref.sourceId,
        id = ref.kind === "agent" ? tableId + (ref.object || "") : tableId;
      if (!found)
        return {
          id,
          tableId,
          source: "upload",
          table: ref.kind === "table" ? ref.table : ref.object || ref.sourceId,
          label: ref.kind === "table" ? ref.connectorId : "Unknown source",
        };
      return {
        id,
        tableId,
        source: found.kind,
        table:
          ref.kind === "agent" && ref.object
            ? `${found.table} ${ref.object}`
            : found.table,
        label: found.connector,
      };
    },
  };
}
export type Sources = ReturnType<typeof useSources>;
