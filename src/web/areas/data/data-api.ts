import { useEffect, useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { LOCAL } from "../../../shared/deployment";
import type {
  Connector,
  ConnectorConfig,
  ConnectorResponse,
  ConnectorSyncResponse,
  CreateConnectorRequest,
  CreateFoundryEventSourceTokenResponse,
  DataLayoutResponse,
  FoundryEventSourceTokenResponse,
  ListConnectorKindsResponse,
  ListConnectorSyncsResponse,
  ListConnectorsResponse,
  ListFoundryEventBatchesResponse,
  ListFoundryEventSourceTokensResponse,
  PutDataLayoutRequest,
  TestConnectorRequest,
  TestConnectorResponse,
  UpdateConnectorRequest,
} from "../../../shared/wfe";
import { api } from "../../api";
import { eventSourceWaiting, isEventSource, isLiveSync } from "./model";

// The Data area's WFE calls: query options for TanStack Query and the
// changes. Keys start with the workspace, and every connector query sits
// under [workspace, "connectors"], so one invalidation refreshes them all.
// The tree's layout sits under [workspace, "data-layout"].

const enc = encodeURIComponent;
const foundry = (workspaceId: string) =>
  `/v1/workspaces/${enc(workspaceId)}/foundry`;
const connectorPath = (workspaceId: string, connectorId: string) =>
  `${foundry(workspaceId)}/connectors/${enc(connectorId)}`;

// While a sync is queued or running its page polls this often.
export const SYNC_POLL_MS = 1500;
// While an API source shown waits for its pieces (model.ts's
// eventSourceWaiting), its sends and the connectors are polled this often.
export const EVENT_POLL_MS = 3000;

/**
 * The kinds the WFE's catalog offers. A local build offers no API source: its
 * senders reach one only through the Helix MCP, which a local install doesn't
 * run, and the local API refuses its send tokens.
 */
export const connectorKindsQuery = (workspaceId: string) => ({
  queryKey: [workspaceId, "connector-kinds"],
  queryFn: async () => {
    const res = await api<ListConnectorKindsResponse>(
      `${foundry(workspaceId)}/connector-kinds`,
    );
    return LOCAL
      ? { ...res, kinds: res.kinds?.filter((k) => !isEventSource(k.kind)) }
      : res;
  },
});

/**
 * Every connector with its tables; polls while any of them is syncing, and
 * while `waiting`: an API source the page shows waits for its pieces.
 */
export const connectorsQuery = (workspaceId: string, waiting = false) => ({
  queryKey: [workspaceId, "connectors"],
  queryFn: () =>
    api<ListConnectorsResponse>(`${foundry(workspaceId)}/connectors`),
  refetchInterval: (q: { state: { data?: ListConnectorsResponse } }) =>
    q.state.data?.connectors?.some((c) => isLiveSync(c.lastSync))
      ? SYNC_POLL_MS
      : waiting && EVENT_POLL_MS,
});

/**
 * Re-reads the workspace's document sets, and the pages of their documents
 * (under their key), whenever one of `connectors`' syncs ends: it may have
 * written its connector's own sets. The first read only notes the syncs
 * that have ended. For any page that lists sets beside connectors.
 */
export function useSetsAfterSyncs(
  workspaceId: string,
  connectors: readonly Connector[] | undefined,
) {
  const qc = useQueryClient();
  const ended = (connectors || []).flatMap((c) =>
      c.lastSync && !isLiveSync(c.lastSync) ? [c.lastSync.id] : [],
    ),
    seen = useRef<ReadonlySet<string> | null>(null);
  useEffect(() => {
    if (!connectors) return;
    const before = seen.current;
    if (before && ended.some((id) => !before.has(id)))
      void qc.invalidateQueries({ queryKey: [workspaceId, "document-sets"] });
    seen.current = new Set(ended);
  }, [ended.join(",")]);
}

/** One connector; polls while `waiting`, as connectorsQuery does. */
export const connectorQuery = (
  workspaceId: string,
  connectorId: string,
  waiting = false,
) => ({
  queryKey: [workspaceId, "connectors", connectorId],
  queryFn: () =>
    api<ConnectorResponse>(connectorPath(workspaceId, connectorId)),
  refetchInterval: waiting && EVENT_POLL_MS,
});

export const connectorSyncsQuery = (
  workspaceId: string,
  connectorId: string,
) => ({
  queryKey: [workspaceId, "connectors", connectorId, "syncs"],
  queryFn: () =>
    api<ListConnectorSyncsResponse>(
      `${connectorPath(workspaceId, connectorId)}/syncs?limit=50`,
    ),
});

/** One sync, polled while it is queued or running. */
export const connectorSyncQuery = (workspaceId: string, syncId: string) => ({
  queryKey: [workspaceId, "connector-syncs", syncId],
  queryFn: () =>
    api<ConnectorSyncResponse>(
      `${foundry(workspaceId)}/connector-syncs/${enc(syncId)}`,
    ),
  refetchInterval: (q: { state: { data?: ConnectorSyncResponse } }) =>
    !q.state.data || isLiveSync(q.state.data.sync) ? SYNC_POLL_MS : false,
});

/** Tests a credential and settings without saving them. */
export const testConnector = (workspaceId: string, config: ConnectorConfig) =>
  api<TestConnectorResponse>(`${foundry(workspaceId)}/connectors:test`, {
    config,
  } satisfies TestConnectorRequest);

/** Tests, then saves a connector; nothing is synced yet. */
export const createConnector = (
  workspaceId: string,
  body: CreateConnectorRequest,
) => api<ConnectorResponse>(`${foundry(workspaceId)}/connectors`, body);

/** Renames a connector or replaces its settings (tested before saving). */
export const updateConnector = (
  workspaceId: string,
  connectorId: string,
  body: UpdateConnectorRequest,
) =>
  api<ConnectorResponse>(
    connectorPath(workspaceId, connectorId),
    body,
    "PATCH",
  );

/**
 * Sets an API source's text field (owners and admins), or clears it with
 * "". It sends the text field alone, so its description and name stay. A
 * change queues one full sync, which re-renders its newest pieces as
 * documents (none once cleared); the WFE refuses it while a sync of the
 * source is live (FAILED_PRECONDITION), and a field its pieces hold as
 * another type, as their sends are refused.
 */
export const setEventTextField = (
  workspaceId: string,
  connectorId: string,
  textField: string,
) =>
  updateConnector(workspaceId, connectorId, {
    config: { events: { textField } },
  });

export const deleteConnector = (workspaceId: string, connectorId: string) =>
  api<object>(connectorPath(workspaceId, connectorId), undefined, "DELETE");

/** Queues a sync; the WFE refuses a second one while one is live. */
export const syncConnector = (workspaceId: string, connectorId: string) =>
  api<ConnectorSyncResponse>(
    `${connectorPath(workspaceId, connectorId)}:sync`,
    {},
  );

/**
 * An API source's send tokens: newest first, revoked ones too, at most 50.
 * Members read them; only their prefixes, never a token.
 */
export const eventTokensQuery = (workspaceId: string, connectorId: string) => ({
  queryKey: [workspaceId, "connectors", connectorId, "tokens"],
  queryFn: () =>
    api<ListFoundryEventSourceTokensResponse>(
      `${connectorPath(workspaceId, connectorId)}/tokens`,
    ),
});

/**
 * Makes a send token (owners and admins; at most 10 live per source). Its
 * `secret` is in this answer only: the WFE keeps its hash.
 */
export const createEventToken = (workspaceId: string, connectorId: string) =>
  api<CreateFoundryEventSourceTokenResponse>(
    `${connectorPath(workspaceId, connectorId)}/tokens`,
    {},
  );

/** Revokes a send token at once (owners and admins); twice is no error. */
export const revokeEventToken = (
  workspaceId: string,
  connectorId: string,
  tokenId: string,
) =>
  api<FoundryEventSourceTokenResponse>(
    `${connectorPath(workspaceId, connectorId)}/tokens/${enc(tokenId)}:revoke`,
    {},
  );

/**
 * An API source's newest `limit` sends (at most 100), its pieces no sync has
 * read yet and its capacity. Polls while the source waits for its pieces
 * (eventSourceWaiting, with `source` as the page last read it).
 */
export const eventBatchesQuery = (
  workspaceId: string,
  source: Connector,
  limit = 20,
) => ({
  queryKey: [workspaceId, "connectors", source.id, "event-batches", limit],
  queryFn: () =>
    api<ListFoundryEventBatchesResponse>(
      `${connectorPath(workspaceId, source.id)}/event-batches?limit=${limit}`,
    ),
  refetchInterval: (q: { state: { data?: ListFoundryEventBatchesResponse } }) =>
    eventSourceWaiting(source, q.state.data) && EVENT_POLL_MS,
});

/** How the Data tree is arranged: the stored JSON and its revision. */
export const dataLayoutQuery = (workspaceId: string) => ({
  queryKey: [workspaceId, "data-layout"],
  queryFn: () => api<DataLayoutResponse>(`${foundry(workspaceId)}/data-layout`),
});

/**
 * Replaces the layout read at `expectedRevision`. FAILED_PRECONDITION (gRPC
 * code 9) when another write came first.
 */
export const putDataLayout = (
  workspaceId: string,
  body: PutDataLayoutRequest,
) =>
  api<DataLayoutResponse>(`${foundry(workspaceId)}/data-layout`, body, "PUT");
