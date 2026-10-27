import { createHash } from "node:crypto";
import type {
  CreateFoundryEventSourceTokenResponse,
  EventSourceCapacity,
  EventSourceField,
  EventsConnectorConfig,
  FoundryEventBatch,
  FoundryEventSourceToken,
  FoundryEventSourceTokenResponse,
  ListFoundryEventBatchesResponse,
  ListFoundryEventSourceTokensResponse,
} from "../../src/shared/wfe";
import * as rules from "../event-pieces";
import { fail, id, now, ok, type MockReply, type MockRoute } from "../http";
import { config, onReset } from "../state";
import {
  access,
  queueEventSync,
  storedConnector,
  type StoredConnector,
} from "./data";
import type {
  MockSourceDocument,
  MockStream,
  SyncedSnapshot,
} from "./data-sources";

// API sources (connectors of kind events), as the backend serves them
// (api/event_sources.go): what was
// sent to each, its send tokens and its table. The connector itself (name,
// description, syncs and the synced table) is ./data.ts's, which calls
// syncEventSource when a sync of one ends.
//
// - sendPieces is SendFoundryEvents: the rules of ../event-pieces.ts, then
//   a client_token replay or refusal, the source's kind, its fields' types
//   and the caps, in the backend's order and with its messages. A stored
//   send is a batch, and queues a sync (`eventSyncMs`, ./data.ts) unless
//   one is queued or running; the sync merges every batch waiting into the
//   events table by piece id, the later piece replacing the earlier, and
//   pads the rows to the fields learned since. No route sends pieces: POST
//   /__mock/event-sources/{connectorId}/pieces (mock/server.ts) stands in
//   for an agent's send, with no token. The mock has no rate limits, daily
//   counters or pauses.
// - Send tokens are hx_ev_ and 64 hex digits from a generator seeded on
//   reset, so tests can rely on them; only their SHA-256 is kept, and the
//   secret is returned once.
// - `{"eventCaps": {"maxRows": 10, "maxBytes": 100000}}` lowers the caps
//   every table has (2,000,000 rows and 2 GiB), to fill a source.
// - A source's text field (./data.ts takes it on create and update) is
//   locked as a string in its schema (lockTextField). With one, every sync
//   also offers its Pieces stream (pieceStreams): the newest 500 rows of its
//   table by receipt as Markdown documents, which ./documents.ts keeps in
//   the source's own set, the newest 500 at every sync. Without one the set
//   is emptied.

type Knobs = { eventCaps?: rules.Caps };
const knobs = config as typeof config & Knobs;

/** One stored send: foundry_event_batches' row with its pieces. */
type StoredBatch = {
  id: string;
  // From 1 per source, in the order sends were stored.
  seq: number;
  // "" for a send without one.
  clientToken: string;
  send: rules.Send;
  receivedAt: rules.Instant;
  // The earliest and latest piece time: its occurred_at, else receivedAt.
  firstAt: rules.Instant;
  lastAt: rules.Instant;
};

type StoredSource = {
  schema: rules.Schema;
  // Oldest first.
  batches: StoredBatch[];
  // Its events table, as its last sync left it: each piece id's row, every
  // row carrying `columns`.
  rows: Map<string, string>;
  columns: string[];
  // The newest batch merged into rows (its change cursor), 0 for none.
  cursor: number;
};

type StoredToken = {
  id: string;
  workspaceId: string;
  connectorId: string;
  // Hex SHA-256 of the token, never the token.
  hash: string;
  prefix: string;
  createdByUserId: string;
  createdAt: string;
  revokedAt: string | null;
  lastUsedAt: string | null;
};

const MAX_LIVE_TOKENS = 10,
  MAX_LISTED_TOKENS = 50;

const sources = new Map<string, StoredSource>();
// Oldest first.
const tokens: StoredToken[] = [];
// How many tokens the seeded generator has made since the reset.
let minted = 0;

const sourceOf = (connectorId: string): StoredSource => {
  let s = sources.get(connectorId);
  if (!s) {
    s = {
      schema: new Map(),
      batches: [],
      rows: new Map(),
      columns: [],
      cursor: 0,
    };
    sources.set(connectorId, s);
  }
  return s;
};
const sha256 = (s: string) => createHash("sha256").update(s).digest("hex");
const utf8 = new TextEncoder();

// ---------------------------------------------------------------- For ./data.ts

/** An API source's fields, by byte order of name: its config's `fields`. */
export const eventFields = (connectorId: string): EventSourceField[] =>
  rules.fieldsOf(sources.get(connectorId)?.schema || new Map());

/** Whether anything was ever sent to the source: before, nothing syncs it. */
export const holdsBatches = (connectorId: string) =>
  (sources.get(connectorId)?.batches.length || 0) > 0;

/**
 * DeleteConnector of an API source: its tokens are revoked and what was
 * sent to it goes with it.
 */
export function forgetEventSource(connectorId: string) {
  const at = now();
  for (const t of tokens)
    if (t.connectorId === connectorId && !t.revokedAt) t.revokedAt = at;
  sources.delete(connectorId);
}

/**
 * UpdateEventSource's lock of a text field on the source's schema
 * (events.Schema.WithText): the refusal, with the send's codes, or what
 * stores it. Nothing is stored until that is called, so an update refused
 * for another reason writes nothing.
 */
export function lockTextField(
  connectorId: string,
  name: string,
): MockReply | (() => void) {
  const locked = rules.withText(
    sources.get(connectorId)?.schema || new Map(),
    name,
  );
  if (rules.isRefusal(locked)) return refusal(locked);
  return () => {
    sourceOf(connectorId).schema = locked;
  };
}

/**
 * The source's Pieces stream (the events kind's DocumentKind), with what a
 * sync that just merged its table offers it: with a text field, its newest
 * MAX_EXTRACTED rows by receipt, each rendered as a Markdown document,
 * newest first. The document's id at the source is the piece's, its version
 * the SHA-256 of its text and its time the piece's receipt, so an identical
 * resend is the same version, newer. Without a text field it offers none
 * (null), which empties its set.
 */
export function pieceStreams(
  connectorId: string,
  settings: EventsConnectorConfig,
): { stream: MockStream; documents: MockSourceDocument[] | null }[] {
  const stream: MockStream = {
    name: "pieces",
    table: "events",
    title: "Pieces",
    description: settings.description || "",
    mediaType: "text/markdown",
    keepNewest: rules.MAX_EXTRACTED,
  };
  const textField = settings.textField || "";
  if (!textField) return [{ stream, documents: null }];
  const rows = [...(sources.get(connectorId)?.rows.values() || [])].map(
    rules.readRow,
  );
  return [
    {
      stream,
      documents: rules.newest(rows, rules.MAX_EXTRACTED).map((row) => {
        const { title, body } = rules.render(row, textField);
        return {
          externalId: row.id,
          version: sha256(body),
          // As a list's times sort: to the millisecond, the mock's own grain.
          at: row.receivedAt.slice(0, 23) + "Z",
          title,
          url: "",
          text: body,
        };
      }),
    },
  ];
}

/**
 * What a sync of the source writes: its events table, once every batch
 * waiting is merged in seq order, its rows padded to the fields learned
 * since the last sync, and its columns typed.
 */
export function syncEventSource(connectorId: string): SyncedSnapshot[] {
  const s = sourceOf(connectorId),
    learned = [...s.schema.keys()].filter((name) => !s.columns.includes(name));
  if (learned.length)
    for (const [key, row] of s.rows)
      s.rows.set(key, rules.padRow(row, learned));
  for (const b of s.batches.filter((x) => x.seq > s.cursor))
    for (const p of b.send.pieces)
      s.rows.set(p.id, rules.rowOf(p, b.receivedAt, s.schema));
  s.cursor = s.batches.at(-1)?.seq || 0;
  s.columns = [...s.schema.keys()];
  return [
    {
      name: "events",
      columns: rules.columnsOf(s.schema),
      primaryKey: ["id"],
      rows: s.rows.size,
    },
  ];
}

// ---------------------------------------------------------------- Sends

// What the caps count of the source: its table as the last sync left it, and
// the batches no sync has merged yet.
function holding(s: StoredSource): rules.Holding {
  const waiting = s.batches.filter((b) => b.seq > s.cursor);
  return {
    tableRows: s.rows.size,
    // Its snapshot's size uncompressed: a line per row.
    tableBytes: [...s.rows.values()].reduce(
      (n, row) => n + utf8.encode(row).length + 1,
      0,
    ),
    tableFields: s.columns,
    pendingPieces: waiting.reduce((n, b) => n + b.send.pieces.length, 0),
    pendingBytes: waiting.reduce((n, b) => n + b.send.bytes, 0),
  };
}
const caps = () => knobs.eventCaps || rules.CAPS;

// A timestamp as protojson writes one: 0, 3 or 6 fractional digits.
const timestamp = (at: rules.Instant) => {
  const fraction = at.slice(20, 26);
  return (
    at.slice(0, 19) +
    (fraction === "000000"
      ? ""
      : fraction.endsWith("000")
        ? "." + fraction.slice(0, 3)
        : "." + fraction) +
    "Z"
  );
};

const batchOut = (s: StoredSource, b: StoredBatch): FoundryEventBatch => ({
  id: b.id,
  seq: String(b.seq),
  pieces: b.send.pieces.length,
  bytes: String(b.send.bytes),
  firstAt: timestamp(b.firstAt),
  lastAt: timestamp(b.lastAt),
  createdAt: timestamp(b.receivedAt),
  synced: b.seq <= s.cursor,
});
const capacityOut = (c: rules.Capacity): EventSourceCapacity => ({
  rows: String(c.rows),
  maxRows: String(c.maxRows),
  bytes: String(c.bytes),
  maxBytes: String(c.maxBytes),
});

// A FAILED_PRECONDITION with its reason, as ErrorInfo.
const withReason = (reply: MockReply, reason: string): MockReply => ({
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
});
// A refusal of the rules as the backend answers it.
const refusal = (r: rules.Refusal): MockReply =>
  r.refused === "invalid"
    ? fail("INVALID_ARGUMENT", r.message)
    : withReason(
        fail("FAILED_PRECONDITION", r.message),
        r.refused === "full"
          ? "FOUNDRY_EVENT_SOURCE_FULL"
          : "FOUNDRY_EVENT_SOURCE_FIELDS",
      );

/** SendFoundryEventsResponse, which the MCP's send tool answers from. */
export type SendPiecesResponse = {
  batch: FoundryEventBatch;
  // This client_token's send was already stored: nothing new was.
  replayed: boolean;
  newFields: EventSourceField[];
  capacity: EventSourceCapacity;
  // From 80% full.
  warning: string;
};

/**
 * SendFoundryEvents for the source: `{clientToken?, pieces: [{id,
 * occurredAt?, data}]}`, each piece's data any JSON value, which is sent as
 * its JSON text. Stored whole or refused whole, with the backend's codes
 * and messages.
 */
export function sendPieces(connectorId: string, body: unknown): MockReply {
  const b = (body || {}) as { clientToken?: unknown; pieces?: unknown };
  const clientToken = typeof b.clientToken === "string" ? b.clientToken : "";
  if (clientToken && !rules.validClientToken(clientToken))
    return fail(
      "INVALID_ARGUMENT",
      "client_token must be 16 to 64 printable ASCII characters with no spaces: a new random value, such as a UUID, for each send",
    );
  // Each piece's time is read first, as a front end reads it.
  const given = (Array.isArray(b.pieces) ? b.pieces : []) as {
    id?: unknown;
    occurredAt?: unknown;
    data?: unknown;
  }[];
  const pieces: rules.Piece[] = [];
  for (const [i, p] of given.entries()) {
    const pieceId = typeof p?.id === "string" ? p.id : "";
    const at =
      typeof p?.occurredAt === "string" && p.occurredAt
        ? rules.parseOccurredAt(i, pieceId, p.occurredAt)
        : undefined;
    if (rules.isRefusal(at)) return refusal(at);
    pieces.push({
      id: pieceId,
      ...(at ? { occurredAt: at } : {}),
      data: utf8.encode(JSON.stringify(p?.data) ?? ""),
    });
  }
  const send = rules.validate(pieces);
  if (rules.isRefusal(send)) return refusal(send);
  // Read with its syncs moved along the clock, so a replay says whether its
  // batch was synced.
  const c = storedConnector(connectorId);
  // A retry of a stored send learns what happened, before any other check.
  const stored = sources
    .get(connectorId)
    ?.batches.find((x) => clientToken && x.clientToken === clientToken);
  if (stored) {
    if (!rules.sameSend(stored.send, send))
      return fail(
        "INVALID_ARGUMENT",
        `client_token ${JSON.stringify(clientToken)} was already used for a different send; use a new client_token for new pieces`,
      );
    const s = sourceOf(connectorId),
      held = rules.capacity(holding(s), s.schema, caps());
    return ok({
      batch: batchOut(s, stored),
      replayed: true,
      newFields: [],
      capacity: capacityOut(held),
      warning: rules.warning(held),
    } satisfies SendPiecesResponse);
  }
  if (!c) return fail("NOT_FOUND", "connector not found");
  if (c.kind !== "events") return notEventSource(c);
  const s = sourceOf(connectorId);
  const merged = rules.mergeSchema(s.schema, send);
  if (rules.isRefusal(merged)) return refusal(merged);
  const projected = rules.project(holding(s), send, merged.schema, caps());
  if (rules.isRefusal(projected)) return refusal(projected);
  const receivedAt = rules.instant(new Date()),
    times = send.pieces.map((p) => p.occurredAt || receivedAt).sort();
  const batch: StoredBatch = {
    id: id("evbatch"),
    seq: (s.batches.at(-1)?.seq || 0) + 1,
    clientToken,
    send,
    receivedAt,
    firstAt: times[0]!,
    lastAt: times.at(-1)!,
  };
  s.batches.push(batch);
  s.schema = merged.schema;
  queueEventSync(c);
  return ok({
    batch: batchOut(s, batch),
    replayed: false,
    newFields: merged.newFields,
    capacity: capacityOut(projected),
    warning: rules.warning(projected),
  } satisfies SendPiecesResponse);
}

// ---------------------------------------------------------------- Routes

const notEventSource = (c: StoredConnector) =>
  withReason(
    fail("FAILED_PRECONDITION", `connector ${c.id} is not an API source`),
    "FOUNDRY_NOT_EVENT_SOURCE",
  );

// loadConnector, then the kind: another workspace's connector is not found.
function eventSource(
  workspaceId: string,
  connectorId: string,
): StoredConnector | MockReply {
  const c = storedConnector(connectorId);
  if (!c || c.workspaceId !== workspaceId)
    return fail("NOT_FOUND", "connector not found");
  return c.kind === "events" ? c : notEventSource(c);
}
const isSource = (x: StoredConnector | MockReply): x is StoredConnector =>
  "source" in x;

const tokenOut = (t: StoredToken): FoundryEventSourceToken => ({
  id: t.id,
  connectorId: t.connectorId,
  prefix: t.prefix,
  createdByUserId: t.createdByUserId,
  createdAt: t.createdAt,
  revokedAt: t.revokedAt,
  lastUsedAt: t.lastUsedAt,
});

const BASE =
  "/v1/workspaces/{workspaceId}/foundry/connectors/{connectorId}" as const;

export const eventSourceRoutes: MockRoute[] = [
  {
    method: "POST",
    path: `${BASE}/tokens`,
    handle: (req) => {
      const c =
        access(req, true) ||
        eventSource(req.params.workspaceId!, req.params.connectorId!);
      if (!isSource(c)) return c;
      const live = tokens.filter((t) => t.connectorId === c.id && !t.revokedAt);
      if (live.length >= MAX_LIVE_TOKENS)
        return fail(
          "RESOURCE_EXHAUSTED",
          `an API source has at most ${MAX_LIVE_TOKENS} live send tokens: revoke one first`,
        );
      minted += 1;
      const secret = "hx_ev_" + sha256(`mock event source token ${minted}`);
      const t: StoredToken = {
        id: id("evtok"),
        workspaceId: c.workspaceId,
        connectorId: c.id,
        hash: sha256(secret),
        prefix: secret.slice(0, 12),
        createdByUserId: req.principal.user.id,
        createdAt: now(),
        revokedAt: null,
        lastUsedAt: null,
      };
      tokens.push(t);
      return ok({
        token: tokenOut(t),
        secret,
      } satisfies CreateFoundryEventSourceTokenResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/tokens`,
    handle: (req) => {
      const c =
        access(req, false) ||
        eventSource(req.params.workspaceId!, req.params.connectorId!);
      if (!isSource(c)) return c;
      return ok({
        tokens: tokens
          .filter((t) => t.connectorId === c.id)
          .reverse()
          .slice(0, MAX_LISTED_TOKENS)
          .map(tokenOut),
      } satisfies ListFoundryEventSourceTokensResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/tokens/{id}:revoke`,
    handle: (req) => {
      const c =
        access(req, true) ||
        eventSource(req.params.workspaceId!, req.params.connectorId!);
      if (!isSource(c)) return c;
      const t = tokens.find(
        (x) =>
          x.id === req.params.id &&
          x.connectorId === c.id &&
          x.workspaceId === c.workspaceId,
      );
      if (!t) return fail("NOT_FOUND", "send token not found");
      // Revoking twice is no error.
      t.revokedAt ||= now();
      return ok({
        token: tokenOut(t),
      } satisfies FoundryEventSourceTokenResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/event-batches`,
    handle: (req) => {
      const c =
        access(req, false) ||
        eventSource(req.params.workspaceId!, req.params.connectorId!);
      if (!isSource(c)) return c;
      const s = sourceOf(c.id),
        asked = Number(req.query.get("limit") || 0),
        limit = Math.min(asked > 0 ? asked : 20, 100),
        held = holding(s);
      return ok({
        batches: [...s.batches]
          .reverse()
          .slice(0, limit)
          .map((b) => batchOut(s, b)),
        pendingPieces: String(held.pendingPieces),
        capacity: capacityOut(rules.capacity(held, s.schema, caps())),
      } satisfies ListFoundryEventBatchesResponse);
    },
  },
];

onReset(() => {
  sources.clear();
  tokens.length = 0;
  minted = 0;
  delete knobs.eventCaps;
});
