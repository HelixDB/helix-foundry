import type {
  Connector,
  ConnectorConfig,
  ConnectorDiscovery,
  ConnectorKindName,
  ConnectorResponse,
  ConnectorSync,
  ConnectorSyncResponse,
  ConnectorSyncStatus,
  EventsConnectorConfig,
  ListConnectorKindsResponse,
  ListConnectorSyncsResponse,
  ListConnectorsResponse,
  SyncedTable,
  TestConnectorResponse,
} from "../../src/shared/wfe";
import {
  fail,
  id,
  now,
  ok,
  type MockReply,
  type MockRequest,
  type MockRoute,
} from "../http";
import { config, isReply, membership, onReset } from "../state";
import { isFoundryPro, proRequired } from "./billing";
import { dropConnectorDocuments, writeConnectorDocuments } from "./documents";
import {
  discover,
  documentStreams,
  hint,
  kinds,
  prepare,
  s3Setup,
  syncTables,
  type Source,
  type SourceError,
} from "./data-sources";
import {
  eventFields,
  forgetEventSource,
  holdsBatches,
  lockTextField,
  pieceStreams,
  syncEventSource,
} from "./event-sources";

// The Data area's WFE routes: connector kinds, connectors (test, create,
// read, edit, delete), their syncs and synced tables. Credentials are kept
// here as the WFE keeps them sealed: never in a response. Responses carry
// unpopulated fields as the WFE's gateway does (null messages, "" strings).
//
// A sync runs on the clock: queued for its first quarter, running until its
// end (RUN_MS), then it writes its tables or fails (see ./data-sources.ts for
// what fails). A kind with documents also writes them into its sets, a set
// per stream (./documents.ts writeConnectorDocuments), and deleting a
// connector deletes its sets and their documents. An API source's sync
// (./event-sources.ts) is queued by the pieces sent to it, or by a change of
// its text field, and takes `eventSyncMs`; it merges every batch waiting,
// then, with a text field, keeps its newest pieces as documents.

const RUN_MS = 2400,
  EVENT_SYNC_MS = 1000;

type Knobs = { eventSyncMs?: number };
const knobs = config as typeof config & Knobs;

export type StoredConnector = {
  id: string;
  workspaceId: string;
  kind: ConnectorKindName;
  name: string;
  source: Source;
  createdAt: string;
  updatedAt: string;
};
type StoredSync = {
  id: string;
  connectorId: string;
  workspaceId: string;
  status: Exclude<ConnectorSyncStatus, "CONNECTOR_SYNC_STATUS_UNSPECIFIED">;
  attempts: number;
  error: string;
  rowsSynced: number;
  createdAt: string;
  // How long it takes, from createdAt; queued for the first quarter.
  runMs: number;
  // Whether a person started it (Sync now, an API source's text field
  // change), whose documents it writes even into a paused set
  // (./documents.ts writeConnectorDocuments).
  byPerson: boolean;
  startedAt: string | null;
  finishedAt: string | null;
};

const connectors = new Map<string, StoredConnector>();
// Newest last.
const syncs: StoredSync[] = [];
// Each connector's synced tables, by name.
const tables = new Map<string, Map<string, SyncedTable>>();
/** A workspace's connectors with their synced tables (for other areas' mocks). */
export function workspaceConnectors(workspaceId: string): Connector[] {
  return [...connectors.values()]
    .filter((c) => c.workspaceId === workspaceId)
    .map(toConnector);
}
/** A connector's name, or undefined once it is deleted (for ./documents.ts). */
export const connectionName = (connectorId: string) =>
  connectors.get(connectorId)?.name;
/** A connector's kind, or undefined once it is deleted (for ./documents.ts). */
export const connectionKind = (connectorId: string) =>
  connectors.get(connectorId)?.kind;
/**
 * Brings the workspace's syncs up to now, so what a finished one wrote is
 * there (the mock runs syncs when they are next looked at).
 */
export const advanceSyncs = (workspaceId: string) =>
  syncs.filter((s) => s.workspaceId === workspaceId).forEach(advance);

// ---------------------------------------------------------------- Syncs

const live = (s: StoredSync) =>
  s.status === "CONNECTOR_SYNC_STATUS_QUEUED" ||
  s.status === "CONNECTOR_SYNC_STATUS_RUNNING";

// Moves a sync along the clock; a finished sync writes its tables.
function advance(s: StoredSync) {
  if (!live(s)) return;
  const age = Date.now() - Date.parse(s.createdAt),
    queuedMs = s.runMs / 4;
  if (age < queuedMs) return;
  if (!s.startedAt) {
    s.status = "CONNECTOR_SYNC_STATUS_RUNNING";
    s.attempts = 1;
    s.startedAt = new Date(Date.parse(s.createdAt) + queuedMs).toISOString();
  }
  if (age < s.runMs) return;
  s.finishedAt = new Date(Date.parse(s.createdAt) + s.runMs).toISOString();
  const c = connectors.get(s.connectorId);
  if (!c) {
    s.status = "CONNECTOR_SYNC_STATUS_FAILED";
    s.error = "the connector was deleted during the sync";
    return;
  }
  const written =
    c.kind === "events" ? syncEventSource(c.id) : syncTables(c.source);
  if (typeof written === "string") {
    s.status = "CONNECTOR_SYNC_STATUS_FAILED";
    s.error = written;
    return;
  }
  s.status = "CONNECTOR_SYNC_STATUS_SUCCEEDED";
  s.rowsSynced = written.reduce((n, t) => n + t.rows, 0);
  // A successful sync drops the tables it did not write.
  tables.set(
    s.connectorId,
    new Map(
      written.map((t) => [
        t.name,
        {
          name: t.name,
          rowCount: String(t.rows),
          syncedAt: s.finishedAt,
          syncId: s.id,
          columns: (t.columns || []).map((col) => ({
            name: col.name,
            type: col.type || "",
            nullable: !!col.nullable,
          })),
          primaryKey: t.primaryKey || [],
          foreignKeys: t.foreignKeys || [],
        },
      ]),
    ),
  );
  writeConnectorDocuments(
    c.workspaceId,
    c.id,
    "events" in c.source.settings
      ? pieceStreams(c.id, c.source.settings.events)
      : documentStreams(c.source, s.finishedAt),
    s.byPerson,
  );
}

function syncsOf(connectorId: string) {
  return syncs
    .filter((s) => s.connectorId === connectorId)
    .map((s) => (advance(s), s))
    .reverse();
}

const toSync = (s: StoredSync): ConnectorSync => ({
  id: s.id,
  connectorId: s.connectorId,
  status: s.status,
  attempts: s.attempts,
  error: s.error,
  rowsSynced: String(s.rowsSynced),
  createdAt: s.createdAt,
  startedAt: s.startedAt,
  finishedAt: s.finishedAt,
  nextAttemptAt: s.createdAt,
});

function queueSync(
  c: StoredConnector,
  byPerson: boolean,
  createdAt = now(),
): StoredSync {
  const s: StoredSync = {
    id: id("sync"),
    connectorId: c.id,
    workspaceId: c.workspaceId,
    status: "CONNECTOR_SYNC_STATUS_QUEUED",
    attempts: 0,
    error: "",
    rowsSynced: 0,
    createdAt,
    runMs: c.kind === "events" ? (knobs.eventSyncMs ?? EVENT_SYNC_MS) : RUN_MS,
    byPerson,
    startedAt: null,
    finishedAt: null,
  };
  syncs.push(s);
  return s;
}

/**
 * A connector by id alone, with its syncs moved along the clock, so what an
 * API source's sync merged is in its state; undefined once deleted. The
 * sends of ./event-sources.ts name a connector by id alone, as a send
 * token does.
 */
export function storedConnector(id: string): StoredConnector | undefined {
  const c = connectors.get(id);
  if (c) syncsOf(c.id);
  return c;
}

/**
 * Queues an API source's sync, which no person started, unless one is
 * queued or running already.
 */
export function queueEventSync(c: StoredConnector) {
  if (!syncsOf(c.id).some(live)) queueSync(c, false);
}

// ---------------------------------------------------------------- Connectors

// configToProto: the settings with every write-only field empty, and an API
// source's fields, learned from its pieces (none for a test).
function responseConfig(
  settings: ConnectorConfig,
  connectorId = "",
): ConnectorConfig {
  if ("events" in settings)
    return {
      events: {
        description: settings.events.description || "",
        fields: connectorId ? eventFields(connectorId) : [],
        textField: settings.events.textField || "",
      },
    };
  if ("stripe" in settings) return { stripe: { apiKey: "" } };
  if ("workos" in settings) return { workos: { apiKey: "" } };
  if ("posthog" in settings)
    return {
      posthog: {
        apiKey: "",
        region: settings.posthog.region || "",
        host: settings.posthog.host || "",
        projectId: settings.posthog.projectId || "",
        eventDays: settings.posthog.eventDays || 0,
      },
    };
  if ("github" in settings)
    return {
      github: {
        token: "",
        repos: settings.github.repos || [],
        reviewDays: settings.github.reviewDays || 0,
      },
    };
  if ("linear" in settings) return { linear: { apiKey: "" } };
  if ("calcom" in settings) return { calcom: { apiKey: "" } };
  if ("granola" in settings)
    return {
      granola: { apiKey: "", transcripts: !!settings.granola.transcripts },
    };
  if ("slack" in settings)
    return {
      slack: { botToken: "", historyDays: settings.slack.historyDays || 0 },
    };
  if ("notion" in settings) return { notion: { token: "" } };
  if ("google" in settings)
    return {
      google: {
        serviceAccountKey: "",
        subject: settings.google.subject || "",
        eventDays: settings.google.eventDays || 0,
      },
    };
  if ("s3" in settings) {
    const s = settings.s3;
    return {
      s3: {
        bucket: s.bucket || "",
        prefix: s.prefix || "",
        region: s.region || "",
        tables: s.tables || [],
        // The auth oneof: only the member set is emitted.
        ...("keys" in s
          ? {
              keys: {
                accessKeyId: s.keys.accessKeyId || "",
                secretAccessKey: "",
                endpoint: s.keys.endpoint || "",
              },
            }
          : { role: { roleArn: s.role.roleArn || "" } }),
      },
    };
  }
  const kind = "postgres" in settings ? "postgres" : "mysql",
    d = "postgres" in settings ? settings.postgres : settings.mysql;
  const db = {
    host: d.host || "",
    port: d.port || 0,
    database: d.database || "",
    user: d.user || "",
    password: "",
    schema: d.schema || "",
    tables: d.tables || [],
    disableTls: !!d.disableTls,
    ca: d.ca || "",
  };
  return kind === "postgres" ? { postgres: db } : { mysql: db };
}

function toConnector(c: StoredConnector): Connector {
  const newest = syncsOf(c.id)[0];
  return {
    id: c.id,
    workspaceId: c.workspaceId,
    kind: c.kind,
    name: c.name,
    config: responseConfig(c.source.settings, c.id),
    secretHint: hint(c.source),
    createdAt: c.createdAt,
    updatedAt: c.updatedAt,
    tables: [...(tables.get(c.id)?.values() || [])].sort((a, b) =>
      a.name.localeCompare(b.name),
    ),
    lastSync: newest ? toSync(newest) : null,
    // The mock never syncs by itself: every connection is manual only, and
    // an API source, which syncs as its pieces arrive, has no schedule.
    schedule:
      c.kind === "events"
        ? null
        : {
            everySeconds: "-1",
            effectiveEverySeconds: "0",
            nextSyncAt: null,
            pausedReason: "",
          },
  };
}

const discoveryJson = (d: ConnectorDiscovery): ConnectorDiscovery => ({
  tables: (d.tables || []).map((t) => ({
    name: t.name,
    columns: (t.columns || []).map((col) => ({
      name: col.name,
      type: col.type || "",
      nullable: !!col.nullable,
    })),
    primaryKey: t.primaryKey || [],
    foreignKeys: t.foreignKeys || [],
    format: t.format || "",
    fileCount: t.fileCount || 0,
    bytes: t.bytes || "0",
    preview: t.preview || [],
    problem: t.problem || "",
  })),
  unavailable: d.unavailable || [],
  maxBytes: d.maxBytes || "0",
  warnings: d.warnings || [],
});

const sourceFailure = (e: SourceError) => fail(e.code, e.message);

// testSource: the Pro gate, the settings check, then discovery.
function testSource(
  workspaceId: string,
  config: unknown,
): { source: Source; discovery: ConnectorDiscovery } | MockReply {
  const prepared = prepare(config, workspaceId);
  if ("code" in prepared) return sourceFailure(prepared);
  const kind = kinds.find((k) => k.name === prepared.kind)!;
  if (kind.requiresPro && !isFoundryPro(workspaceId))
    return proRequired(
      workspaceId,
      `${kind.displayName} connectors need Foundry Pro`,
    );
  const found = discover(prepared);
  if ("code" in found) return sourceFailure(found);
  return {
    source: { ...prepared, settings: found.settings },
    discovery: found.discovery,
  };
}

// CreateEventSource's checks, which a rename passes too: a workspace's API
// sources have names of their own, compared as the WFE's column compares
// them (MySQL's default collation, blind to case and accents), and it has at
// most MAX_EVENT_SOURCES. The oldest source with the name is the one named.
const MAX_EVENT_SOURCES = 20;
const sameName = (a: string, b: string) =>
  a.localeCompare(b, "en", { sensitivity: "base" }) === 0;
function eventSourceRefusal(
  workspaceId: string,
  name: string,
  renamed?: StoredConnector,
): MockReply | undefined {
  const others = [...connectors.values()].filter(
    (c) =>
      c.workspaceId === workspaceId && c.kind === "events" && c !== renamed,
  );
  const same = others.find((c) => sameName(c.name, name));
  if (same)
    return fail(
      "ALREADY_EXISTS",
      `the API source ${same.id} already has this name; choose another${renamed ? "" : ", or send to that source"}`,
    );
  if (!renamed && others.length >= MAX_EVENT_SOURCES)
    return fail(
      "RESOURCE_EXHAUSTED",
      `a workspace has at most ${MAX_EVENT_SOURCES} API sources: delete one first`,
    );
  return undefined;
}

function connectorName(raw: unknown): string | MockReply {
  const name = String(raw ?? "").trim();
  return name && name.length <= 120
    ? name
    : fail("INVALID_ARGUMENT", "name must be 1 to 120 characters");
}

// Reading needs membership; changing needs owner or admin.
export function access(
  req: MockRequest,
  manage: boolean,
): MockReply | undefined {
  const m = membership(req.params.workspaceId!, req.principal.user.id);
  if (isReply(m)) return m;
  if (manage && m.role === "WORKSPACE_ROLE_MEMBER")
    return fail("PERMISSION_DENIED", "permission denied");
  return undefined;
}

function load(req: MockRequest, id: string): StoredConnector | MockReply {
  const c = connectors.get(id);
  return c && c.workspaceId === req.params.workspaceId
    ? c
    : fail("NOT_FOUND", "connector not found");
}
const isConnector = (x: StoredConnector | MockReply): x is StoredConnector =>
  "source" in x;

const BASE = "/v1/workspaces/{workspaceId}/foundry" as const;

export const dataRoutes: MockRoute[] = [
  {
    method: "GET",
    path: `${BASE}/connector-kinds`,
    handle: (req) =>
      access(req, false) ||
      ok({
        kinds: kinds.map((k) => ({
          kind: k.name,
          displayName: k.displayName,
          requiresPro: k.requiresPro,
          available: !k.requiresPro || isFoundryPro(req.params.workspaceId!),
          // The setup oneof: set for s3 where role access is configured.
          ...(k.name === "s3" && s3Setup(req.params.workspaceId!)
            ? { s3: s3Setup(req.params.workspaceId!) }
            : {}),
        })),
      } satisfies ListConnectorKindsResponse),
  },
  {
    method: "POST",
    path: `${BASE}/connectors:test`,
    handle: (req) => {
      const denied = access(req, true);
      if (denied) return denied;
      const tested = testSource(req.params.workspaceId!, req.body?.config);
      if ("status" in tested) return tested;
      return ok({
        discovery: discoveryJson(tested.discovery),
        config: responseConfig(tested.source.settings),
      } satisfies TestConnectorResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/connectors`,
    handle: (req) =>
      access(req, false) ||
      ok({
        connectors: workspaceConnectors(req.params.workspaceId!),
      } satisfies ListConnectorsResponse),
  },
  {
    method: "POST",
    path: `${BASE}/connectors`,
    handle: (req) => {
      const denied = access(req, true);
      if (denied) return denied;
      const name = connectorName(req.body?.name);
      if (typeof name !== "string") return name;
      const tested = testSource(req.params.workspaceId!, req.body?.config);
      if ("status" in tested) return tested;
      const refused =
        tested.source.kind === "events" &&
        eventSourceRefusal(req.params.workspaceId!, name);
      if (refused) return refused;
      // An API source's text field is locked as a string from the start.
      const connectorId = id("conn"),
        textField =
          "events" in tested.source.settings &&
          tested.source.settings.events.textField,
        lock = textField ? lockTextField(connectorId, textField) : undefined;
      if (lock && typeof lock !== "function") return lock;
      const at = now();
      const c: StoredConnector = {
        id: connectorId,
        workspaceId: req.params.workspaceId!,
        kind: tested.source.kind,
        name,
        source: tested.source,
        createdAt: at,
        updatedAt: at,
      };
      connectors.set(c.id, c);
      lock?.();
      return ok({
        connector: toConnector(c),
        discovery: discoveryJson(tested.discovery),
      } satisfies ConnectorResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/connectors/{id}`,
    handle: (req) => {
      const c = access(req, false) || load(req, req.params.id!);
      return isConnector(c)
        ? ok({ connector: toConnector(c), discovery: null })
        : c;
    },
  },
  {
    method: "PATCH",
    path: `${BASE}/connectors/{id}`,
    handle: (req) => {
      const c = access(req, true) || load(req, req.params.id!);
      if (!isConnector(c)) return c;
      const body = (req.body || {}) as { name?: unknown; config?: unknown };
      let name: string | undefined, discovery: ConnectorDiscovery | undefined;
      if (body.name !== undefined) {
        const checked = connectorName(body.name);
        if (typeof checked !== "string") return checked;
        name = checked;
      }
      let source: Source | undefined,
        // An API source's new text field: its lock, stored with the rest.
        textChanged = false,
        lock: (() => void) | undefined;
      if (body.config !== undefined && body.config !== null) {
        // A blank credential keeps the stored one.
        const config = structuredClone(body.config) as Record<string, any>;
        const kind = Object.keys(config)[0];
        if (kind && kind !== c.kind)
          return fail(
            "INVALID_ARGUMENT",
            "a connector's kind cannot change; create a new connector",
          );
        if (c.kind === "events") {
          // UpdateEventSource, under the lock sends take: the description
          // and the text field, each only when sent, and an update that
          // sets neither, nor a name, is refused. A description alone is
          // never barred by a live sync, and tests or queues nothing. A
          // text field that changes is locked as a string and queues one
          // full sync, which re-renders the newest pieces; it waits for a
          // live sync to end. Its fields' other types, cursor and next sync
          // are never written.
          const prepared = prepare(config, c.workspaceId);
          if ("code" in prepared) return sourceFailure(prepared);
          const held: EventsConnectorConfig =
              "events" in c.source.settings ? c.source.settings.events : {},
            asked: EventsConnectorConfig =
              "events" in prepared.settings ? prepared.settings.events : {};
          textChanged =
            asked.textField !== undefined &&
            asked.textField !== (held.textField || "");
          if (textChanged && syncsOf(c.id).some(live))
            return fail(
              "FAILED_PRECONDITION",
              "a sync of this API source is running; change its text field in a minute",
            );
          if (textChanged && asked.textField) {
            const locked = lockTextField(c.id, asked.textField);
            if (typeof locked !== "function") return locked;
            lock = locked;
          }
          if (asked.description !== undefined || asked.textField !== undefined)
            source = {
              ...c.source,
              settings: { events: { ...held, ...asked } },
            };
        } else {
          const part = config[c.kind] || (config[c.kind] = {});
          // S3 keeps it only in key mode: a role stores no secret, so moving
          // to one drops the old key.
          if (c.kind === "s3") {
            if (part.keys && !part.keys.secretAccessKey)
              part.keys.secretAccessKey = c.source.secret;
          } else {
            const field =
              c.kind === "postgres" || c.kind === "mysql"
                ? "password"
                : c.kind === "github" || c.kind === "notion"
                  ? "token"
                  : c.kind === "slack"
                    ? "botToken"
                    : c.kind === "google"
                      ? "serviceAccountKey"
                      : "apiKey";
            if (!part[field]) part[field] = c.source.secret;
          }
          const tested = testSource(c.workspaceId, config);
          if ("status" in tested) return tested;
          if (
            JSON.stringify(tested.source.settings) !==
              JSON.stringify(c.source.settings) &&
            syncsOf(c.id).some(live)
          )
            return fail(
              "FAILED_PRECONDITION",
              "a sync of this connector is queued or running; change its settings once the sync ends",
            );
          source = tested.source;
          discovery = tested.discovery;
        }
      }
      if (name === undefined && !source)
        return fail(
          "INVALID_ARGUMENT",
          c.kind === "events"
            ? "set a name, description or text field to update"
            : "set a name or config to update",
        );
      const refused =
        c.kind === "events" &&
        name !== undefined &&
        eventSourceRefusal(c.workspaceId, name, c);
      if (refused) return refused;
      const moved =
        c.kind !== "events" &&
        !!source &&
        JSON.stringify(source.settings) !== JSON.stringify(c.source.settings);
      if (name !== undefined) c.name = name;
      if (source) c.source = source;
      lock?.();
      c.updatedAt = now();
      // Settings that point elsewhere queue a sync, as does an API source's
      // new text field: its one full sync, no other being live.
      if ((moved && !syncsOf(c.id).some(live)) || textChanged)
        queueSync(c, true);
      return ok({
        connector: toConnector(c),
        discovery: discovery ? discoveryJson(discovery) : null,
      } satisfies ConnectorResponse);
    },
  },
  {
    method: "DELETE",
    path: `${BASE}/connectors/{id}`,
    handle: (req) => {
      const c = access(req, true) || load(req, req.params.id!);
      if (!isConnector(c)) return c;
      if (syncsOf(c.id).some(live))
        return fail(
          "FAILED_PRECONDITION",
          "a sync of this connector is queued or running; delete it once the sync ends",
        );
      connectors.delete(c.id);
      tables.delete(c.id);
      dropConnectorDocuments(c.id);
      if (c.kind === "events") forgetEventSource(c.id);
      return ok({});
    },
  },
  {
    method: "POST",
    path: `${BASE}/connectors/{id}:sync`,
    handle: (req) => {
      const c = access(req, true) || load(req, req.params.id!);
      if (!isConnector(c)) return c;
      const kind = kinds.find((k) => k.name === c.kind)!;
      if (kind.requiresPro && !isFoundryPro(c.workspaceId))
        return proRequired(
          c.workspaceId,
          `${kind.displayName} connectors need Foundry Pro`,
        );
      if (c.kind === "events" && !holdsBatches(c.id))
        return fail(
          "FAILED_PRECONDITION",
          "an API source syncs when pieces arrive; send some first",
        );
      if (syncsOf(c.id).some(live))
        return fail(
          "ALREADY_EXISTS",
          "a sync of this connector is already queued or running",
        );
      return ok({
        sync: toSync(queueSync(c, true)),
      } satisfies ConnectorSyncResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/connectors/{connectorId}/syncs`,
    handle: (req) => {
      const c = access(req, false) || load(req, req.params.connectorId!);
      if (!isConnector(c)) return c;
      const asked = Number(req.query.get("limit") || 0),
        limit = Math.min(asked > 0 ? asked : 20, 100);
      return ok({
        syncs: syncsOf(c.id).slice(0, limit).map(toSync),
      } satisfies ListConnectorSyncsResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/connector-syncs/{id}`,
    handle: (req) => {
      const denied = access(req, false);
      if (denied) return denied;
      const s = syncs.find(
        (x) =>
          x.id === req.params.id && x.workspaceId === req.params.workspaceId,
      );
      if (!s) return fail("NOT_FOUND", "sync not found");
      advance(s);
      return ok({ sync: toSync(s) } satisfies ConnectorSyncResponse);
    },
  },
];

// Acme (on Pro from the billing mock's seed, which runs first) starts with a
// synced Stripe account and a PostgreSQL database, so dev:mock opens on a
// populated Data page.
const SEED_TIME = "2026-10-01T09:00:00.000Z";
function seed() {
  connectors.clear();
  syncs.length = 0;
  tables.clear();
  delete knobs.eventSyncMs;
  const sources: [string, string, unknown][] = [
    [
      "conn_acme_stripe",
      "Stripe",
      { stripe: { apiKey: "sk_test_51AcmeMockKey4f2a" } },
    ],
    [
      "conn_acme_shop",
      "Shop database",
      {
        postgres: {
          host: "db.acme.example.com",
          database: "shop",
          user: "foundry_readonly",
          password: "mock-only",
        },
      },
    ],
  ];
  for (const [connectorId, name, config] of sources) {
    const tested = testSource("ws_acme", config);
    if ("status" in tested) throw new Error(`mock seed: ${name}`);
    const c: StoredConnector = {
      id: connectorId,
      workspaceId: "ws_acme",
      kind: tested.source.kind,
      name,
      source: tested.source,
      createdAt: SEED_TIME,
      updatedAt: SEED_TIME,
    };
    connectors.set(c.id, c);
    advance(queueSync(c, false, SEED_TIME));
  }
}
onReset(seed);
