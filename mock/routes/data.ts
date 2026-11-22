import type {
  Connector,
  ConnectorAccount,
  ConnectorAccountBroken,
  ConnectorConfig,
  ConnectorDiscovery,
  ConnectorKindName,
  ConnectorResponse,
  ConnectorSlackStatus,
  SlackConnectorSetup,
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
import {
  LOCAL_TOKEN,
  PROD_STAMP,
  config,
  db,
  displayName,
  isReply,
  membership,
  onReset,
  type Membership,
} from "../state";
import { isFoundryPro, proRequired } from "./billing";
import { dropConnectorDocuments, writeConnectorDocuments } from "./documents";
import {
  GITHUB_APP_INSTALL_URL,
  addSlackFeedMessage,
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
import { foundryPrecondition } from "./ai";
import {
  SLACK_NOTICE_VERSION,
  SLACK_ORPHAN_MS,
  canInstallSlack,
  slackFeedOn,
  slackInstallOf,
  slackRemoved,
} from "./slack";

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
//
// The kinds Pipes signs in to (GitHub, Linear, Slack, Notion, PostHog,
// Cal.com, Stripe, Google Workspace, Attio) also connect by signing in at the
// provider (./sign-ins.ts), except in a local build or with `{"signIn":
// false}`: such a connector has an account in place of a key, and only the
// account's member may widen what it reads (widensReach). A signed-in
// PostHog is US Cloud's: Pipes' PostHog has no region. `{"accountBroken": {"<connectorId>":
// "member_left"}}` (or access_revoked, expired, account_missing) breaks its
// account as the WFE does when the member leaves: it needs signing in
// again, its live syncs fail, and Sync now is refused until then.
//
// Slack through Helix's Slack app (where ./slack.ts has the feed on): the
// Slack kind carries its setup (the workspace's install, whether the
// caller may add it, the connection reading it, the notice), and a
// connection made with helixApp is bound to the install as the WFE binds
// it, refused as it refuses (the notice, no install, permissions, no Slack
// admin's install, a Slack workspace read already). It is pushed: no
// schedule, and a sync queued when it is bound and when Slack sends a
// message (POST /__mock/slack/feed {channel, text, workspaceId?}, a
// channel of the app's by id or name). Its history backfill takes
// `slackBackfillMs` (60 s), reading the app's four channels, and says
// about 2.5 days at the start, as Tier 1 would. A token connection edited
// to the app switches in stages: it syncs by its token until the backfill
// ends, then reads through the app (at once with helixApp.now). Its status
// reads the install as ./slack.ts shows it: gone once disconnected (its
// data deleted 7 days later, and its syncs fail), missing permissions with
// `slackScopesOld`, and Slack's limit with `slackEventsLimited`;
// `{"slackLossyDays": n}` has n days published with messages possibly
// missing.

const RUN_MS = 2400,
  EVENT_SYNC_MS = 1000,
  SLACK_BACKFILL_MS = 60_000,
  // The ETA a backfill starts at: Tier 1's 2.5 days for 12 channels.
  SLACK_BACKFILL_ETA_S = 216_000,
  // The mock's channels the app is in (./data-sources.ts).
  SLACK_APP_CHANNELS = 4;

type BreakReason =
  | "member_left"
  | "access_revoked"
  | "expired"
  | "account_missing";
type Knobs = {
  eventSyncMs?: number;
  signIn?: boolean;
  accountBroken?: Record<string, BreakReason>;
  slackBackfillMs?: number;
  slackLossyDays?: number;
};
const knobs = config as typeof config & Knobs;

/** The broker's account a signed-in connector reads with (hosted: Pipes'). */
export type StoredAccount = {
  id: string;
  // The member it reads as, and who the provider says signed in.
  userId: string;
  label: string;
  // Bumped by each sign-in, as the WFE's account_generation.
  generation: number;
  connectedAt: string;
  broken?: ConnectorAccountBroken;
  brokenAt?: string;
};

export type StoredConnector = {
  id: string;
  workspaceId: string;
  kind: ConnectorKindName;
  name: string;
  source: Source;
  createdAt: string;
  updatedAt: string;
  // Set when it was connected by signing in.
  account?: StoredAccount;
  // Set for a Slack connection bound to Helix's Slack app: the Slack
  // workspace it reads, the storage notice accepted, and when its backfill
  // started. While it switches from its token (`staged`: the settings it
  // reads with once the backfill ends), `source` is still the token's.
  slackApp?: {
    teamId: string;
    teamName: string;
    noticeVersion: number;
    boundAt: number;
    staged?: ConnectorConfig;
  };
};

/** The kinds that connect by signing in, where the stamp offers it. */
// Every one Pipes signs in to; prod's stamp (pnpm docs:media) lists GitHub
// and Linear only, as its pipes.kinds starts.
const SIGN_IN_KINDS: readonly ConnectorKindName[] = PROD_STAMP
  ? ["github", "linear"]
  : [
      "github",
      "linear",
      "slack",
      "notion",
      "posthog",
      "calcom",
      "stripe",
      "google",
      "attio",
    ];

/**
 * Whether settings `now` let a signed-in connector read more than `was`
 * (both prepared), as the WFE's widensReach decides: GitHub's repositories
 * (none named where some were, or one not named before, case aside; from
 * every repository any list narrows it), PostHog's project changed (its
 * region is US Cloud, fixed). No other kind's settings reach further.
 */
export function widensReach(
  was: ConnectorConfig,
  now: ConnectorConfig,
): boolean {
  if ("github" in now) {
    const before = ("github" in was && was.github.repos) || [],
      after = now.github.repos || [];
    if (!after.length) return before.length > 0;
    if (!before.length) return false;
    return after.some(
      (r) => !before.some((b) => b.toLowerCase() === r.toLowerCase()),
    );
  }
  if ("posthog" in now) {
    const before = "posthog" in was ? was.posthog : {};
    return (before.projectId || "") !== (now.posthog.projectId || "");
  }
  return false;
}
/**
 * Whether `kind` connects by signing in here: hosted, with the switch on
 * (a local API has no broker).
 */
export const signInOffered = (kind: string) =>
  !LOCAL_TOKEN &&
  knobs.signIn !== false &&
  (SIGN_IN_KINDS as readonly string[]).includes(kind);
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
  applyBreaks();
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

// ---------------------------------------------------------------- Accounts

const BROKEN: Record<BreakReason, ConnectorAccountBroken> = {
  member_left: "CONNECTOR_ACCOUNT_BROKEN_MEMBER_LEFT",
  access_revoked: "CONNECTOR_ACCOUNT_BROKEN_ACCESS_REVOKED",
  expired: "CONNECTOR_ACCOUNT_BROKEN_EXPIRED",
  account_missing: "CONNECTOR_ACCOUNT_BROKEN_ACCOUNT_MISSING",
};
const BROKEN_WHY: Record<ConnectorAccountBroken, string> = {
  CONNECTOR_ACCOUNT_BROKEN_UNSPECIFIED: "its sign-in needs renewing",
  CONNECTOR_ACCOUNT_BROKEN_MEMBER_LEFT:
    "the member who connected it left the workspace",
  CONNECTOR_ACCOUNT_BROKEN_ACCESS_REVOKED:
    "its access was rejected or revoked there",
  CONNECTOR_ACCOUNT_BROKEN_EXPIRED: "its access lapsed and couldn't be renewed",
  CONNECTOR_ACCOUNT_BROKEN_ACCOUNT_MISSING: "its sign-in no longer exists",
};
const kindName = (kind: string) =>
  kinds.find((k) => k.name === kind)?.displayName || kind;
/** The syncer's refusal of a connector whose account is broken. */
export const brokenMessage = (c: StoredConnector) =>
  `${kindName(c.kind)} needs signing in again: ${BROKEN_WHY[c.account!.broken!]}. Its data is frozen as of its last sync. An owner or admin signs it in again from its page.`;

/** FailedPrecondition with an ErrorInfo reason, as the WFE's refusals carry. */
export function refusedFor(reason: string, message: string): MockReply {
  const reply = fail("FAILED_PRECONDITION", message);
  return {
    ...reply,
    body: {
      ...(reply.body as object),
      details: [
        {
          "@type": "type.googleapis.com/google.rpc.ErrorInfo",
          reason,
          domain: "foundry.helix-db.com",
        },
      ],
    },
  };
}

/**
 * Breaks the accounts `accountBroken` names, once each: BreakConnectorAccounts
 * as the WFE runs it when the member leaves, which also ends the connector's
 * live syncs.
 */
function applyBreaks() {
  const asked = knobs.accountBroken;
  if (!asked) return;
  delete knobs.accountBroken;
  const at = now();
  for (const [connectorId, reason] of Object.entries(asked)) {
    const c = connectors.get(connectorId);
    if (!c?.account || c.account.broken || !BROKEN[reason]) continue;
    c.account.broken = BROKEN[reason];
    c.account.brokenAt = at;
    for (const s of syncs)
      if (s.connectorId === c.id && live(s)) {
        s.status = "CONNECTOR_SYNC_STATUS_FAILED";
        s.error = brokenMessage(c);
        s.finishedAt = at;
      }
  }
}

/** The member's name, or "a former member" once they have left. */
function connectedBy(c: StoredConnector, a: StoredAccount) {
  const user = db.users.get(a.userId);
  return user && !isReply(membership(c.workspaceId, a.userId))
    ? displayName(user)
    : "a former member";
}

const toAccount = (c: StoredConnector, a: StoredAccount): ConnectorAccount => ({
  label: a.label,
  state: a.broken
    ? "CONNECTOR_ACCOUNT_STATE_NEEDS_SIGN_IN"
    : "CONNECTOR_ACCOUNT_STATE_CONNECTED",
  broken: a.broken || "CONNECTOR_ACCOUNT_BROKEN_UNSPECIFIED",
  brokenAt: a.brokenAt || null,
  userId: a.userId,
  connectedBy: connectedBy(c, a),
  connectedAt: a.connectedAt,
});

/**
 * Makes the connector a finished sign-in names, reading with `account`, and
 * schedules its first sync at once (the mock has no scheduler: it queues
 * it), as FinishConnectorSignIn does.
 */
export function addSignedInConnector(
  workspaceId: string,
  name: string,
  source: Source,
  account: StoredAccount,
): StoredConnector {
  const at = now();
  const c: StoredConnector = {
    id: id("conn"),
    workspaceId,
    kind: source.kind,
    name,
    source,
    createdAt: at,
    updatedAt: at,
    account,
  };
  connectors.set(c.id, c);
  queueSync(c, false);
  return c;
}

/**
 * Signs a connector in again with `account`: a new generation, the break
 * cleared, and a sync at once. Whether the provider account changed: both
 * labels known, and different but for case.
 */
export function signConnectorInAgain(
  c: StoredConnector,
  account: Omit<StoredAccount, "generation">,
): boolean {
  const was = c.account?.label || "";
  const changed =
    !!was &&
    !!account.label &&
    was.toLowerCase() !== account.label.toLowerCase();
  c.account = { ...account, generation: (c.account?.generation || 0) + 1 };
  c.updatedAt = now();
  if (!syncsOf(c.id).some(live)) queueSync(c, false);
  return changed;
}

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
  const gone = c.slackApp && !c.slackApp.staged ? slackAppFailure(c) : "";
  const written = gone
    ? gone
    : c.kind === "events"
      ? syncEventSource(c.id)
      : syncTables(c.source);
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

/**
 * A connector's syncs that ended, oldest first, with how and the rows they
 * wrote: what an automation's sync_finished trigger looks for
 * (./automations.ts, the backend's FindFoundryAutomationEnded).
 */
export const syncsEnded = (connectorId: string) =>
  syncsOf(connectorId)
    .filter((s) => !live(s) && s.finishedAt)
    .reverse()
    .map((s) => ({
      id: s.id,
      at: Date.parse(s.finishedAt!),
      outcome:
        s.status === "CONNECTOR_SYNC_STATUS_SUCCEEDED" ? "succeeded" : "failed",
      rows: s.rowsSynced,
      error: s.error,
    }));

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
  if ("attio" in settings) return { attio: { apiKey: "" } };
  if ("granola" in settings)
    return {
      granola: { apiKey: "", transcripts: !!settings.granola.transcripts },
    };
  if ("slack" in settings)
    return settings.slack.helixApp
      ? {
          slack: {
            historyDays: settings.slack.historyDays || 0,
            helixApp: {
              noticeVersion: settings.slack.helixApp.noticeVersion || 0,
              now: false,
            },
          },
        }
      : {
          slack: { botToken: "", historyDays: settings.slack.historyDays || 0 },
        };
  if ("notion" in settings) return { notion: { token: "" } };
  if ("databuddy" in settings)
    return {
      databuddy: {
        apiKey: "",
        websiteIds: settings.databuddy.websiteIds || [],
        historyDays: settings.databuddy.historyDays || 0,
      },
    };
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
    // Switching to Helix's Slack app, it says the app's settings.
    config: responseConfig(c.slackApp?.staged || c.source.settings, c.id),
    // A signed-in connector stores no key, nor one through Slack's app.
    secretHint:
      c.account || (c.slackApp && !c.slackApp.staged) ? "" : hint(c.source),
    createdAt: c.createdAt,
    updatedAt: c.updatedAt,
    tables: [...(tables.get(c.id)?.values() || [])].sort((a, b) =>
      a.name.localeCompare(b.name),
    ),
    lastSync: newest ? toSync(newest) : null,
    // The mock never syncs by itself: every connection is manual only, and
    // an API source, which syncs as its pieces arrive, has no schedule, nor
    // has Slack through Helix's app.
    schedule:
      c.kind === "events" || (c.slackApp && !c.slackApp.staged)
        ? null
        : {
            everySeconds: "-1",
            effectiveEverySeconds: "0",
            nextSyncAt: null,
            pausedReason: "",
          },
    account: c.account ? toAccount(c, c.account) : null,
    slackStatus: c.slackApp ? slackStatusOf(c) : null,
  };
}

// ---------------------------------------------------------------- Slack through Helix's app

/** How a Slack connection through the app reads, as the WFE says it. */
function slackStatusOf(c: StoredConnector): ConnectorSlackStatus {
  const a = c.slackApp!,
    install = slackInstallOf(c.workspaceId),
    removed = slackRemoved(c.workspaceId);
  const state: ConnectorSlackStatus["state"] = !install
    ? "SLACK_FEED_STATE_INSTALL_GONE"
    : install.teamId !== a.teamId
      ? "SLACK_FEED_STATE_TEAM_CHANGED"
      : install.missingScopes?.length
        ? "SLACK_FEED_STATE_NEEDS_REINSTALL"
        : "SLACK_FEED_STATE_LIVE";
  const read =
    (Date.now() - a.boundAt) / (knobs.slackBackfillMs ?? SLACK_BACKFILL_MS);
  return {
    teamId: a.teamId,
    teamName: a.teamName,
    state,
    missingScopes:
      state === "SLACK_FEED_STATE_NEEDS_REINSTALL"
        ? install!.missingScopes
        : [],
    backfill:
      read < 1
        ? {
            channels: SLACK_APP_CHANNELS,
            channelsDone: Math.floor(Math.max(read, 0) * SLACK_APP_CHANNELS),
            channelName: "",
            etaSeconds: String(
              Math.ceil((1 - Math.max(read, 0)) * SLACK_BACKFILL_ETA_S),
            ),
            waitingUntil: null,
          }
        : null,
    lastEventAt: null,
    eventsLimitedAt: install?.eventsLimitedAt || null,
    lossyDays: knobs.slackLossyDays || 0,
    switching: !!a.staged,
    deletedAt:
      state === "SLACK_FEED_STATE_INSTALL_GONE" && removed
        ? new Date(removed.at + SLACK_ORPHAN_MS).toISOString()
        : null,
  };
}

/** Why a sync through the app fails: the app removed from Slack. */
function slackAppFailure(c: StoredConnector): string {
  const s = slackStatusOf(c);
  return s.state === "SLACK_FEED_STATE_INSTALL_GONE"
    ? `Helix Foundry was removed from ${s.teamName}: add it again in Settings › Slack, or switch this connection to a bot token. Its data is frozen as of its last sync and is deleted on ${(s.deletedAt || "").slice(0, 10)}.`
    : "";
}

/**
 * Flips each switch whose backfill has ended: the connection reads through
 * the app from then, with a full read (its sync).
 */
function flipSlackSwitches() {
  const ends = knobs.slackBackfillMs ?? SLACK_BACKFILL_MS;
  for (const c of connectors.values()) {
    const a = c.slackApp;
    if (!a?.staged || Date.now() < a.boundAt + ends) continue;
    c.source = {
      kind: "slack",
      settings: a.staged,
      secret: "",
      workspaceId: c.workspaceId,
    };
    c.slackApp = { ...a, staged: undefined };
    if (!syncsOf(c.id).some(live)) queueSync(c, false);
  }
}

/**
 * The workspace's install a connection binds to, or the WFE's refusal
 * (slackAppInstall): the feed off here, no install, permissions it lacks,
 * no Slack admin or owner's install.
 */
function slackAppInstall(workspaceId: string) {
  if (!slackFeedOn())
    return foundryPrecondition(
      workspaceId,
      "FOUNDRY_SLACK_FEED_DISABLED",
      "Syncing Slack through Helix Foundry's app isn't available here: use your own Slack app's token",
    );
  const install = slackInstallOf(workspaceId);
  if (!install)
    return foundryPrecondition(
      workspaceId,
      "FOUNDRY_SLACK_APP_NOT_INSTALLED",
      "Add Helix Foundry to Slack from Settings › Slack first",
    );
  if (install.missingScopes?.length)
    return foundryPrecondition(
      workspaceId,
      "FOUNDRY_SLACK_APP_SCOPES",
      "Helix Foundry needs new Slack permissions to sync channels: add it to Slack again from Settings › Slack",
    );
  if (!install.installerAdmin)
    return foundryPrecondition(
      workspaceId,
      "FOUNDRY_SLACK_APP_ADMIN",
      "A Slack workspace admin or owner must add Helix Foundry to Slack before it can sync channels",
    );
  return { teamId: install.teamId!, teamName: install.teamName! };
}

/** A binding refused for a notice that isn't the current one. */
const noticeRefusal = (workspaceId: string) =>
  foundryPrecondition(
    workspaceId,
    "FOUNDRY_SLACK_APP_NOTICE",
    "Read and accept Helix Foundry's current storage notice to sync Slack through its app",
  );

/** Another connection of the workspace that reads through the app: IN_USE. */
function slackInUse(workspaceId: string, except?: StoredConnector) {
  const other = [...connectors.values()].find(
    (o) => o.workspaceId === workspaceId && o.slackApp && o !== except,
  );
  if (!other) return undefined;
  const reply = foundryPrecondition(
    workspaceId,
    "FOUNDRY_SLACK_APP_IN_USE",
    `${other.name} already reads this Slack workspace`,
  );
  (reply.body.details[0]!.metadata as Record<string, string>).connector_id =
    other.id;
  return reply;
}

/** The Slack kind's setup, for `m`: where the feed is on. */
function slackSetup(m: Membership): SlackConnectorSetup | undefined {
  if (!slackFeedOn()) return undefined;
  const install = slackInstallOf(m.workspaceId);
  return {
    install: install || null,
    canInstall: canInstallSlack(m),
    inUseByConnectorId:
      [...connectors.values()].find(
        (c) => c.workspaceId === m.workspaceId && c.slackApp,
      )?.id || "",
    installerAdmin: !!install?.installerAdmin,
    noticeVersion: SLACK_NOTICE_VERSION,
  };
}

/** CreateConnector with helixApp: bound to the install, its first sync queued. */
function createSlackApp(workspaceId: string, name: string, config: any) {
  if (Number(config.slack.helixApp.noticeVersion) !== SLACK_NOTICE_VERSION)
    return noticeRefusal(workspaceId);
  const install = slackAppInstall(workspaceId);
  if ("status" in install) return install;
  const tested = testSource(workspaceId, config);
  if ("status" in tested) return tested;
  const inUse = slackInUse(workspaceId);
  if (inUse) return inUse;
  const at = now();
  const c: StoredConnector = {
    id: id("conn"),
    workspaceId,
    kind: "slack",
    name,
    source: tested.source,
    createdAt: at,
    updatedAt: at,
    slackApp: {
      ...install,
      noticeVersion: SLACK_NOTICE_VERSION,
      boundAt: Date.now(),
    },
  };
  connectors.set(c.id, c);
  queueSync(c, false);
  return ok({
    connector: toConnector(c),
    discovery: discoveryJson(tested.discovery),
  } satisfies ConnectorResponse);
}

/**
 * UpdateConnector of a Slack connection through the app, or switching to
 * it (updateSlackConnector): back to a token; a token's switch, staged;
 * staged, its settings, or the flip with helixApp.now; through the app,
 * its settings (another Slack workspace binds again, with the notice).
 * What to store, or the WFE's refusal.
 */
function updateSlackApp(
  c: StoredConnector,
  config: any,
):
  | {
      source: Source;
      slackApp: StoredConnector["slackApp"] | null;
      discovery?: ConnectorDiscovery;
    }
  | MockReply {
  const ws = c.workspaceId,
    sl = config.slack || {},
    token = String(sl.botToken || "").trim(),
    app = sl.helixApp as { noticeVersion?: number; now?: boolean } | undefined;
  const busy = () =>
    syncsOf(c.id).some(live)
      ? fail(
          "FAILED_PRECONDITION",
          "a sync of this connector is queued or running; change its settings once the sync ends",
        )
      : undefined;
  if (!app && token) {
    const tested = testSource(ws, {
      slack: { botToken: token, historyDays: sl.historyDays },
    });
    if ("status" in tested) return tested;
    return (
      busy() || {
        source: tested.source,
        slackApp: null,
        discovery: tested.discovery,
      }
    );
  }
  if (app && Number(app.noticeVersion) !== SLACK_NOTICE_VERSION)
    return noticeRefusal(ws);
  const install = slackAppInstall(ws);
  if ("status" in install) return install;
  const rebind = !c.slackApp || c.slackApp.teamId !== install.teamId;
  if (rebind && !app) return noticeRefusal(ws);
  const tested = testSource(ws, {
    slack: {
      historyDays: sl.historyDays,
      helixApp: {
        noticeVersion: app?.noticeVersion || c.slackApp!.noticeVersion,
      },
    },
  });
  if ("status" in tested) return tested;
  const inUse = rebind && slackInUse(ws, c);
  if (inUse) return inUse;
  const bound = rebind
    ? { ...install, noticeVersion: SLACK_NOTICE_VERSION, boundAt: Date.now() }
    : c.slackApp!;
  // From a token, or still switching: it syncs by its token until the flip.
  if ((!c.slackApp || c.slackApp.staged) && !app?.now)
    return (
      busy() || {
        source: c.source,
        slackApp: { ...bound, staged: tested.source.settings },
        discovery: tested.discovery,
      }
    );
  const changed =
    JSON.stringify(tested.source.settings) !==
    JSON.stringify(c.source.settings);
  return (
    (changed && busy()) || {
      source: tested.source,
      slackApp: { ...bound, staged: undefined },
      discovery: tested.discovery,
    }
  );
}

/**
 * A message Slack sends the workspace's app (POST /__mock/slack/feed): kept
 * for its Slack connection through the app, which syncs it.
 */
export function slackFeedPost(body: {
  channel?: string;
  text?: string;
  workspaceId?: string;
}): MockReply {
  const ws = body.workspaceId || "ws_acme";
  flipSlackSwitches();
  const c = [...connectors.values()].find(
    (x) => x.workspaceId === ws && x.slackApp && !x.slackApp.staged,
  );
  if (!c)
    return fail(
      "FAILED_PRECONDITION",
      "no Slack connection reads through the app here",
    );
  const channel = addSlackFeedMessage(ws, body.channel || "", body.text || "");
  if (!channel)
    return fail("INVALID_ARGUMENT", "the app is in no such channel");
  if (!syncsOf(c.id).some(live)) queueSync(c, false);
  return ok({ channel });
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

// testSource: the Pro gate, the settings check, then discovery. A
// signed-in connector's (`signIn`) reads with the broker's token.
export function testSource(
  workspaceId: string,
  config: unknown,
  signIn = false,
): { source: Source; discovery: ConnectorDiscovery } | MockReply {
  const prepared = prepare(config, workspaceId, signIn);
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
  applyBreaks();
  flipSlackSwitches();
  const m = membership(req.params.workspaceId!, req.principal.user.id);
  if (isReply(m)) return m;
  if (manage && m.role === "WORKSPACE_ROLE_MEMBER")
    return fail("PERMISSION_DENIED", "permission denied");
  return undefined;
}

export function load(
  req: MockRequest,
  id: string,
): StoredConnector | MockReply {
  const c = connectors.get(id);
  return c && c.workspaceId === req.params.workspaceId
    ? c
    : fail("NOT_FOUND", "connector not found");
}
export const isConnector = (
  x: StoredConnector | MockReply,
): x is StoredConnector => "source" in x;

const BASE = "/v1/workspaces/{workspaceId}/foundry" as const;

export const dataRoutes: MockRoute[] = [
  {
    method: "GET",
    path: `${BASE}/connector-kinds`,
    handle: (req) => {
      const denied = access(req, false);
      if (denied) return denied;
      const m = membership(req.params.workspaceId!, req.principal.user.id);
      const slack = isReply(m) ? undefined : slackSetup(m);
      return ok({
        kinds: kinds.map((k) => ({
          kind: k.name,
          displayName: k.displayName,
          requiresPro: k.requiresPro,
          available: !k.requiresPro || isFoundryPro(req.params.workspaceId!),
          signIn: signInOffered(k.name),
          // The setup oneof: set for s3 where role access is configured,
          // and for GitHub where it signs in.
          ...(k.name === "s3" && s3Setup(req.params.workspaceId!)
            ? { s3: s3Setup(req.params.workspaceId!) }
            : {}),
          ...(k.name === "github" && signInOffered(k.name)
            ? { github: { installUrl: GITHUB_APP_INSTALL_URL } }
            : {}),
          // And for Slack where a connection may read through Helix's app.
          ...(k.name === "slack" && slack ? { slack } : {}),
        })),
      } satisfies ListConnectorKindsResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/connectors:test`,
    handle: (req) => {
      const denied = access(req, true);
      if (denied) return denied;
      // Through Helix's Slack app, the workspace's install is tested.
      if (req.body?.config?.slack?.helixApp) {
        const install = slackAppInstall(req.params.workspaceId!);
        if ("status" in install) return install;
      }
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
    // needsSignIn: only those whose account needs signing in again,
    // without their tables or syncs.
    handle: (req) =>
      access(req, false) ||
      ok({
        connectors:
          req.query.get("needsSignIn") === "true"
            ? [...connectors.values()]
                .filter(
                  (c) =>
                    c.workspaceId === req.params.workspaceId &&
                    !!c.account?.broken,
                )
                .map((c) => ({ ...toConnector(c), tables: [], lastSync: null }))
            : workspaceConnectors(req.params.workspaceId!),
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
      if (req.body?.config?.slack?.helixApp)
        return createSlackApp(req.params.workspaceId!, name, req.body.config);
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
        lock: (() => void) | undefined,
        // Slack through Helix's app: its binding to store (null: none).
        slackApp: StoredConnector["slackApp"] | null | undefined;
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
        } else if (
          c.kind === "slack" &&
          (c.slackApp || config.slack?.helixApp)
        ) {
          const updated = updateSlackApp(c, config);
          if ("status" in updated) return updated;
          source = updated.source;
          slackApp = updated.slackApp;
          discovery = updated.discovery;
        } else if (c.account) {
          // Signed in: no key to change, the broker's token re-tests it,
          // and only the account's member widens what it reads.
          const part = config[c.kind] || (config[c.kind] = {});
          if (
            part.token ||
            part.apiKey ||
            part.botToken ||
            part.serviceAccountKey
          )
            return fail(
              "FAILED_PRECONDITION",
              `this connector signs in with ${kindName(c.kind)}: sign it in again instead`,
            );
          if (c.account.broken)
            return refusedFor("ACCOUNT_NEEDS_SIGN_IN", brokenMessage(c));
          const prepared = prepare(config, c.workspaceId, true);
          if ("code" in prepared) return sourceFailure(prepared);
          if (
            widensReach(c.source.settings, prepared.settings) &&
            req.principal.user.id !== c.account.userId
          )
            return refusedFor(
              "ACCOUNT_OWNER_ONLY",
              c.kind === "posthog"
                ? `Only ${connectedBy(c, c.account)} can change its project: ${c.name} reads with their ${kindName(c.kind)} access`
                : `Only ${connectedBy(c, c.account)} can add repositories, or read every one: ${c.name} reads with their ${kindName(c.kind)} access. You can narrow them`,
            );
          const tested = testSource(c.workspaceId, config, true);
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
        } else {
          const part = config[c.kind] || (config[c.kind] = {});
          let kept = false;
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
            kept = !part[field];
            if (kept) part[field] = c.source.secret;
          }
          const tested = testSource(c.workspaceId, config);
          if ("status" in tested) return tested;
          // As the backend's connector.Targeted: a kept credential goes only
          // to the server it was entered for (a database's host, port, TLS
          // and CA, PostHog's region and host).
          const target = (settings: ConnectorConfig) => {
            const s: Record<string, unknown> = Object.values(settings)[0] || {};
            return JSON.stringify([
              s.host,
              s.port,
              s.disableTls,
              s.ca,
              s.region,
            ]);
          };
          if (
            kept &&
            target(tested.source.settings) !== target(c.source.settings)
          )
            return fail(
              "INVALID_ARGUMENT",
              "these settings connect to a different server; enter the credential again",
            );
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
      if (slackApp !== undefined) c.slackApp = slackApp || undefined;
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
      if (c.account?.broken)
        return refusedFor("ACCOUNT_NEEDS_SIGN_IN", brokenMessage(c));
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
  delete knobs.signIn;
  delete knobs.accountBroken;
  delete knobs.slackBackfillMs;
  delete knobs.slackLossyDays;
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
