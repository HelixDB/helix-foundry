import { useEffect, useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { LOCAL } from "../../../shared/deployment";
import type {
  AuthorizeConnectorSignInRequest,
  AuthorizeConnectorSignInResponse,
  Connector,
  ConnectorConfig,
  ConnectorResponse,
  ConnectorSignInResponse,
  ConnectorSyncResponse,
  CreateConnectorRequest,
  CreateFoundryEventSourceTokenResponse,
  DataLayoutResponse,
  FinishConnectorSignInRequest,
  FoundryEventSourceTokenResponse,
  ListConnectorKindsResponse,
  ListConnectorSyncsResponse,
  ListConnectorsResponse,
  ListFoundryEventBatchesResponse,
  ListFoundryEventSourceTokensResponse,
  PutDataLayoutRequest,
  StartConnectorSignInRequest,
  TestConnectorRequest,
  TestConnectorResponse,
  UpdateConnectorRequest,
} from "../../../shared/wfe";
import { jobSeen, jobStarted, type SyncFrom } from "../../analytics";
import { api } from "../../api";
import {
  eventSourceWaiting,
  isEventSource,
  isKnownKind,
  isLiveSync,
} from "./model";

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
  queryFn: async () => {
    const r = await api<ListConnectorsResponse>(
      `${foundry(workspaceId)}/connectors`,
    );
    jobSeen(
      "sync",
      (r.connectors || []).map(
        (c) =>
          c.lastSync && {
            ...c.lastSync,
            kind: isKnownKind(c.kind) ? c.kind : ("other" as const),
          },
      ),
    );
    return r;
  },
  refetchInterval: (q: { state: { data?: ListConnectorsResponse } }) =>
    q.state.data?.connectors?.some((c) => isLiveSync(c.lastSync))
      ? SYNC_POLL_MS
      : waiting && EVENT_POLL_MS,
});

// How long the shell's read of the connectors that need signing in again
// stays fresh: a break is rare, and any change to the connectors (a
// sign-in, a sync's end) reads it again.
const NEEDS_SIGN_IN_STALE_MS = 5 * 60_000;

/**
 * The connectors whose account needs signing in again, without their
 * tables or syncs: the cheap read behind the shell's dialog and banner.
 * Under the connectors' key, so whatever refreshes them refreshes it.
 */
export const needsSignInQuery = (workspaceId: string) => ({
  queryKey: [workspaceId, "connectors", "needs-sign-in"],
  queryFn: () =>
    api<ListConnectorsResponse>(
      `${foundry(workspaceId)}/connectors?needsSignIn=true`,
    ),
  staleTime: NEEDS_SIGN_IN_STALE_MS,
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
export const syncConnector = async (
  workspaceId: string,
  connectorId: string,
  from: SyncFrom,
) => {
  const r = await api<ConnectorSyncResponse>(
    `${connectorPath(workspaceId, connectorId)}:sync`,
    {},
  );
  jobStarted({ job: "sync", from }, r.sync.id);
  return r;
};

// Connecting by signing in at the provider (GitHub, Linear): a sign-in is
// started with the connector's settings, authorized in this browser (the
// provider's page, which returns to /connect/callback), then finished by the
// callback. Only its starter may authorize or finish it.

const signInPath = (workspaceId: string, signInId: string) =>
  `${foundry(workspaceId)}/sign-ins/${enc(signInId)}`;

/**
 * Starts a sign-in for a new connector of `kind`, or for `connectorId`
 * again. The same client token returns the sign-in it started.
 */
export const startSignIn = (
  workspaceId: string,
  body: StartConnectorSignInRequest,
) => api<ConnectorSignInResponse>(`${foundry(workspaceId)}/sign-ins`, body);

/** A sign-in as it stands; past its expiry it reads EXPIRED. */
export const getSignIn = (workspaceId: string, signInId: string) =>
  api<ConnectorSignInResponse>(signInPath(workspaceId, signInId));

/** One sign-in (an agent's link names it), for its form. */
export const signInQuery = (workspaceId: string, signInId: string) => ({
  queryKey: [workspaceId, "sign-ins", signInId],
  queryFn: () => getSignIn(workspaceId, signInId),
});

/**
 * The provider's page to authorize the sign-in at, returning to
 * `returnTo`, the site's callback. Again after a decline or a failure: the
 * same sign-in, so an agent waiting on it follows the retry.
 */
export const authorizeSignIn = (
  workspaceId: string,
  signInId: string,
  returnTo: string,
) =>
  api<AuthorizeConnectorSignInResponse>(
    `${signInPath(workspaceId, signInId)}:authorize`,
    { returnTo } satisfies AuthorizeConnectorSignInRequest,
  );

/**
 * Finishes the sign-in with what the provider sent the callback. A repeat
 * returns it as it stands, and spends no second code.
 */
export const finishSignIn = (
  workspaceId: string,
  signInId: string,
  body: FinishConnectorSignInRequest,
) =>
  api<ConnectorSignInResponse>(
    `${signInPath(workspaceId, signInId)}:finish`,
    body,
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
