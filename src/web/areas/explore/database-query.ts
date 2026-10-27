import type {
  ExecuteDatabaseQueryRequest,
  ExecuteDatabaseQueryResponse,
} from "../../../shared/wfe";
import { LOCAL } from "../../../shared/deployment";
import { ApiError, api } from "../../api";

// Runs v3 dynamic read queries against an ontology's database with the
// user's session, through the control plane's query broker (WFE POST
// /v1/databases:query-read, reached via /papi). Ported from helix-website's
// src/lib/database-query.ts, reads only: Foundry never writes from the
// browser. The broker forwards the request bytes to the database's POST
// /v2/query and answers HTTP 200 with the database's own status and base64
// body, so a failure comes from one of three layers:
//   1. /papi: { code: "CSRF_REJECTED" | "NOT_FOUND" | ..., message }
//   2. WFE:   { code: <gRPC int>, message, details } on a non-2xx status
//   3. the database or its gateway: an inner statusCode with { error, msg }

export type JsonValue =
  | null
  | boolean
  | number
  | string
  | JsonValue[]
  | { [key: string]: JsonValue };

/**
 * Which database a query runs against: always a tenant here, with when its
 * ontology was created (an ISO time), which says whether the database may
 * still be starting (see STARTING_MS).
 */
export type DatabaseQueryTarget = {
  kind: "tenant";
  tenantId: string;
  createdAt?: string | null;
};

/** The v3 dynamic query request: the exact body a database takes on POST /v2/query. */
export interface DynamicQueryRequest {
  request_type: "read";
  query_name?: string | null;
  query: Record<string, JsonValue>;
  parameters?: Record<string, JsonValue>;
  parameter_types?: Record<string, JsonValue>;
}

export type DatabaseQueryFailureKind =
  /** The database (gateway) answered a non-2xx statusCode; see `code`. */
  | "database"
  /** WFE 403: database.query.read required. */
  | "forbidden"
  /** WFE 404: unknown database. */
  | "not_found"
  /** WFE 400: a bad request shape. */
  | "invalid"
  /**
   * Queries paused by the workspace's Helix billing: WFE 400 "tenant queries
   * are disabled" or "queries … are paused: <why>", or an inner 402. See
   * queriesPaused.
   */
  | "disabled"
  /** WFE 429 for a request or response over the broker's limit. */
  | "too_large"
  /** WFE 503, a throttle, or a 5xx without a better mapping. */
  | "unavailable"
  /** The session ended. */
  | "unauthenticated"
  /** fetch threw (offline, DNS...). */
  | "network"
  /** The request was aborted through the AbortSignal. */
  | "aborted"
  /** A 2xx whose body is not an ExecuteDatabaseQueryResponse, or undecodable. */
  | "malformed"
  /**
   * A database in its first minutes (mayBeStarting) answered as its gateway
   * answers a tenant it hasn't taken on yet (isNewDatabaseAnswer): read it
   * again after `retryInMs`.
   */
  | "starting";

type FailureFields = {
  ok: false;
  /** The inner status for "database", "starting" (and an inner 402); the outer HTTP status otherwise; 0 for network or aborted. */
  status: number;
  /** The stable code when there is one: the database's `error`, the gateway's `code` or /papi's. */
  code: string | null;
  /** A message to show. */
  message: string;
  /** The database said whether a retry is safe, when it said. */
  retryable?: boolean;
  /** A write's outcome is unknown. Never set: reads only. */
  outcomeUnknown?: boolean;
  /** The decoded error body, for "show details". */
  body?: unknown;
  elapsedMs: number;
};

/** Only a starting failure says when to read again. */
export type DatabaseQueryFailure =
  | (FailureFields & { kind: Exclude<DatabaseQueryFailureKind, "starting"> })
  | (FailureFields & {
      kind: "starting";
      /** How long to wait before reading it again: startingRetryMs. */
      retryInMs: number;
    });

export type DatabaseQueryResult =
  | {
      ok: true;
      /** The database's status (2xx). */
      status: number;
      contentType: string | null;
      /**
       * The parsed JSON body (keyed by the request's `returns`), the raw text
       * when it isn't JSON, or null when empty. Integers beyond
       * Number.MAX_SAFE_INTEGER come back as exact decimal strings where the
       * runtime allows it (see parseJsonLossless).
       */
      body: unknown;
      text?: string | null;
      /** The decoded body's size in bytes. */
      bytes: number;
      elapsedMs: number;
    }
  | DatabaseQueryFailure;

export interface ExecuteOptions {
  signal?: AbortSignal;
}

/** What the explorer and the reads call to run a query; injectable for tests. */
export type QueryExecutor = (
  request: DynamicQueryRequest,
  options?: ExecuteOptions,
) => Promise<DatabaseQueryResult>;

// ============ Base64 (UTF-8 safe, no Buffer) ============

// String.fromCharCode spreads its arguments: keep each call well under the
// engines' argument limits for large bodies.
const BASE64_CHUNK_BYTES = 0x8000;

function bytesToBase64(bytes: Uint8Array): string {
  let binary = "";
  for (let offset = 0; offset < bytes.length; offset += BASE64_CHUNK_BYTES)
    binary += String.fromCharCode(
      ...bytes.subarray(offset, offset + BASE64_CHUNK_BYTES),
    );
  return btoa(binary);
}

function base64ToBytes(base64: string): Uint8Array {
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

/** The `queryJson` bytes field: base64 of the UTF-8 request JSON. */
export function encodeQueryJson(request: DynamicQueryRequest): string {
  return bytesToBase64(new TextEncoder().encode(JSON.stringify(request)));
}

function decodeBody(responseJson: string): { text: string; bytes: number } {
  if (responseJson === "") return { text: "", bytes: 0 };
  const bytes = base64ToBytes(responseJson);
  return { text: new TextDecoder().decode(bytes), bytes: bytes.length };
}

/** Decodes the `responseJson` bytes field to text; throws on invalid base64. */
export function decodeResponseJson(responseJson: string): string {
  return decodeBody(responseJson).text;
}

// ============ Response parsing ============

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

// An integer literal in value position with at least 16 digits: every
// integer beyond Number.MAX_SAFE_INTEGER has one. Floats are excluded.
const LONG_INTEGER_LITERAL = /(?:^|[[:,])\s*-?\d{16,}(?![\d.eE])/;
const INTEGER_SOURCE = /^-?\d+$/;

function keepUnsafeIntegers(
  _key: string,
  value: unknown,
  // JSON.parse source text access (ES2026); undefined on older runtimes.
  context?: { source?: unknown },
): unknown {
  if (
    typeof value === "number" &&
    !Number.isSafeInteger(value) &&
    typeof context?.source === "string" &&
    INTEGER_SOURCE.test(context.source)
  )
    return context.source;
  return value;
}

/**
 * JSON.parse that keeps integers beyond Number.MAX_SAFE_INTEGER as their
 * exact decimal strings, where the runtime passes the reviver the literal's
 * source. Elsewhere it is plain JSON.parse. Throws like JSON.parse.
 */
export function parseJsonLossless(text: string): unknown {
  if (!LONG_INTEGER_LITERAL.test(text)) return JSON.parse(text);
  return JSON.parse(
    text,
    keepUnsafeIntegers as (key: string, value: unknown) => unknown,
  );
}

/** As the CLI: empty → null, JSON when it parses, otherwise the text. */
function parseBody(text: string, contentType: string | null): unknown {
  const trimmed = text.trim();
  if (trimmed === "") return null;
  if ((contentType && /\bjson\b/i.test(contentType)) || /^[[{]/.test(trimmed))
    try {
      return parseJsonLossless(text);
    } catch {
      // Not JSON after all: keep the text.
    }
  return text;
}

function isExecuteResponse(
  value: unknown,
): value is ExecuteDatabaseQueryResponse {
  return (
    isRecord(value) &&
    typeof value.statusCode === "number" &&
    Number.isInteger(value.statusCode) &&
    value.statusCode >= 100 &&
    value.statusCode <= 599 &&
    (value.responseJson === undefined ||
      value.responseJson === null ||
      typeof value.responseJson === "string") &&
    (value.contentType === undefined ||
      value.contentType === null ||
      typeof value.contentType === "string")
  );
}

// ============ Failure mapping ============

const MESSAGES = {
  aborted: "Query cancelled.",
  network: "Couldn’t reach Helix. Check your connection and try again.",
  unauthenticated: "Your session has ended. Sign in again.",
  routeNotEnabled:
    "This version of Foundry can’t read databases yet. Reload the page, and contact support if it keeps happening.",
  csrfRejected:
    "The request failed Foundry’s security check. Reload the page and try again.",
  forbidden:
    "You don’t have read access to this database. Ask a workspace owner or admin to grant Query read on its project.",
  notFound:
    "This database wasn’t found. It may have been deleted, or it isn’t in this workspace.",
  billingDisabled:
    "Queries are paused for this database because of the workspace’s Helix billing. Check Billing in the Helix dashboard to resume.",
  requestTooLarge:
    "This query is too large to send. Send fewer ids or smaller values.",
  responseTooLarge:
    "The result is too large to return. Add a limit or return fewer properties.",
  tooLarge:
    "The query or its result is too large. Add a limit or return fewer properties.",
  brokerUnavailable:
    "The query service is temporarily unavailable. Try again in a moment.",
  gatewayTimeout: "The query took too long. Narrow it and try again.",
  malformed:
    "Helix returned a response Foundry couldn’t read. Try again, and contact support if it keeps happening.",
  undecodable: "Helix returned a database response Foundry couldn’t decode.",
  queryTimeout: "The query took too long. Add a limit or narrow the label.",
  rateLimited: "Rate limited — try again in a moment.",
  throttled:
    "Helix is limiting how many queries this database takes right now. Wait a moment before running more.",
  payloadTooLarge:
    "The query is larger than the database accepts. Send fewer ids or smaller values.",
  readViewChanged:
    "The data changed while the query was running. Run it again.",
  databaseUnavailable:
    "The database is temporarily unavailable. Try again in a moment.",
  backendUnavailable:
    "The gateway can’t reach this database (backend_unavailable). It may be starting or failing over. Try again in a few minutes.",
  gatewayRefusing:
    "This database’s gateway is refusing queries (rate_limit_unavailable). Try again in a few minutes.",
  platformAuthorization:
    "The database rejected the Helix platform’s own credentials. This is a problem on Helix’s side. Try again later or contact support.",
  databaseInternal:
    "The database hit an internal error. Try again, and contact support if it keeps happening.",
  starting:
    "Helix is still starting this new database, which can take a few minutes. Try again shortly.",
} as const;

function failure(
  kind: Exclude<DatabaseQueryFailureKind, "starting">,
  status: number,
  message: string,
  elapsedMs: number,
  extra: { code?: string | null; retryable?: boolean; body?: unknown } = {},
): DatabaseQueryFailure {
  const result: DatabaseQueryFailure = {
    ok: false,
    kind,
    status,
    code: extra.code ?? null,
    message,
    elapsedMs,
  };
  if (extra.retryable !== undefined) result.retryable = extra.retryable;
  if (extra.body !== undefined) result.body = extra.body;
  return result;
}

/** The CLI's pre-send checks; null when the request passes. */
export function requestValidationMessage(request: unknown): string | null {
  if (!isRecord(request)) return "The query request must be a JSON object.";
  if (request.request_type !== "read")
    return 'Foundry only runs queries whose request_type is "read".';
  if (!isRecord(request.query))
    return "The query request needs a query object.";
  return null;
}

const isSizeMessage = (m: string) =>
  /exceeds (the )?broker limit|larger than|too large|message size|size limit|max(imum)? (message|size)/i.test(
    m,
  );
const tooLargeMessage = (m: string) =>
  /response|received message larger/i.test(m)
    ? MESSAGES.responseTooLarge
    : /request/i.test(m)
      ? MESSAGES.requestTooLarge
      : MESSAGES.tooLarge;

/** Maps a refused call: /papi's string code, or the WFE's gRPC status. */
function outerFailure(e: ApiError, elapsedMs: number): DatabaseQueryFailure {
  const status = e.status,
    message = e.message === "Request failed" ? "" : e.message;
  const fail = (
    kind: Exclude<DatabaseQueryFailureKind, "starting">,
    text: string,
  ) =>
    failure(kind, status, text, elapsedMs, {
      code: typeof e.code === "string" ? e.code : null,
      body: { code: e.code, message: e.message, details: e.details },
    });
  if (status === 401) return fail("unauthenticated", MESSAGES.unauthenticated);
  if (e.code === "NOT_FOUND")
    return fail("unavailable", MESSAGES.routeNotEnabled);
  if (e.code === "CSRF_REJECTED")
    return fail("unavailable", MESSAGES.csrfRejected);
  switch (status) {
    case 400:
      if (/tenant queries are disabled/i.test(message))
        return fail("disabled", MESSAGES.billingDisabled);
      // The WFE's FAILED_PRECONDITION for a paused database says why in
      // words (it sends no reason code), which the message keeps.
      if (/queries (for this database )?are paused/i.test(message))
        return fail(
          "disabled",
          `${message.charAt(0).toUpperCase()}${message.slice(1)}.`,
        );
      return fail(
        "invalid",
        message
          ? `Helix refused the query: ${message}.`
          : "Helix refused the query.",
      );
    case 403:
      return fail("forbidden", MESSAGES.forbidden);
    case 404:
      return fail("not_found", MESSAGES.notFound);
    case 405:
      return fail("unavailable", MESSAGES.routeNotEnabled);
    case 413:
      return fail("too_large", MESSAGES.requestTooLarge);
    case 429:
      if (isSizeMessage(message))
        return fail("too_large", tooLargeMessage(message));
      return failure("unavailable", status, MESSAGES.throttled, elapsedMs, {
        code: "rate_limited",
      });
    case 503:
      return fail("unavailable", MESSAGES.brokerUnavailable);
    case 504:
      return fail("unavailable", MESSAGES.gatewayTimeout);
  }
  if (status >= 500)
    return fail(
      "unavailable",
      `Helix hit an unexpected error (HTTP ${status}). Try again in a moment.`,
    );
  return fail(
    "invalid",
    message || `Helix refused the query (HTTP ${status}).`,
  );
}

function databaseErrorEnvelope(body: unknown): {
  code: string | null;
  message: string | null;
  retryable?: boolean;
} {
  if (typeof body === "string") {
    const t = body.trim();
    return {
      code: null,
      message: !t || t.startsWith("<") ? null : t.slice(0, 300),
    };
  }
  if (!isRecord(body)) return { code: null, message: null };
  const retryable =
    typeof body.retryable === "boolean" ? body.retryable : undefined;
  // The gateway's terminal failures put the code in `code` and the message
  // in `error`; everything else uses { error: code, msg }.
  if (typeof body.code === "string")
    return {
      code: body.code,
      message: typeof body.error === "string" ? body.error : null,
      retryable,
    };
  return {
    code: typeof body.error === "string" ? body.error : null,
    message: typeof body.msg === "string" ? body.msg : null,
    retryable,
  };
}

function databaseMessage(
  status: number,
  code: string | null,
  message: string | null,
): string {
  switch (code) {
    case "query_timeout":
    case "query_deadline_exceeded":
      return MESSAGES.queryTimeout;
    case "rate_limited":
      return MESSAGES.rateLimited;
    case "payload_too_large":
      return MESSAGES.payloadTooLarge;
    case "request_read_view_changed":
    case "query_cancelled_by_reader_retirement":
      return MESSAGES.readViewChanged;
    case "backend_unavailable":
      return MESSAGES.backendUnavailable;
    case "rate_limit_unavailable":
      return MESSAGES.gatewayRefusing;
    case "internal_error":
      return MESSAGES.databaseInternal;
  }
  // The broker authenticates with a platform key: an inner 401 or 403 is
  // never the user's.
  if (status === 401 || status === 403) return MESSAGES.platformAuthorization;
  if (message) return message;
  if (status === 408) return MESSAGES.queryTimeout;
  if (status === 413) return MESSAGES.payloadTooLarge;
  if (status === 429) return MESSAGES.rateLimited;
  if (status >= 500) return MESSAGES.databaseUnavailable;
  return `The database returned HTTP ${status}.`;
}

/**
 * The result for a database's own answer to POST /v2/query: its status,
 * content type and body text.
 */
export function databaseResponseResult(
  response: {
    status: number;
    contentType: string | null;
    text: string;
    bytes?: number;
  },
  elapsedMs: number,
): DatabaseQueryResult {
  const { status, contentType, text } = response;
  const body = parseBody(text, contentType);
  if (status < 200 || status >= 300) {
    const envelope = databaseErrorEnvelope(body);
    const extra = {
      code: envelope.code,
      retryable: envelope.retryable,
      body: body ?? undefined,
    };
    if (status === 402 || envelope.code === "tenant_disabled")
      return failure(
        "disabled",
        status,
        MESSAGES.billingDisabled,
        elapsedMs,
        extra,
      );
    return failure(
      "database",
      status,
      databaseMessage(status, envelope.code, envelope.message),
      elapsedMs,
      extra,
    );
  }
  return {
    ok: true,
    status,
    contentType,
    body,
    text,
    bytes: response.bytes ?? new TextEncoder().encode(text).length,
    elapsedMs,
  };
}

// ============ Queries paused for billing ============

/**
 * Why a read's database takes no queries, from its "disabled" failure: the
 * workspace has no active Helix plan ("no_plan", the WFE's "queries are
 * paused: the workspace has no active plan; choose a plan to resume"), or
 * another billing gate ("billing": a plan unpaid, a quota or credit spent,
 * or a pause the WFE gives no reason for). Null for any other failure.
 * Reading again changes neither: only the plan does.
 *
 * @example
 * queriesPaused({ ...refused, kind: "disabled", message: "Queries are paused: the workspace has no active plan; choose a plan to resume." }); // "no_plan"
 * queriesPaused({ ...refused, kind: "disabled", status: 402 }); // "billing"
 * queriesPaused({ ...refused, kind: "invalid" }); // null
 */
export function queriesPaused(
  failure: DatabaseQueryFailure,
): "no_plan" | "billing" | null {
  if (failure.kind !== "disabled") return null;
  return /no active plan/i.test(failure.message) ? "no_plan" : "billing";
}

// ============ A database that is still starting ============
//
// For its first minutes a new database's gateway refuses reads it will soon
// take: it serves a tenant only once the tenant's rate limit has arrived,
// and its watches of keys and of limits land apart. A failure
// the gateway answers so, from a database whose ontology is younger than
// STARTING_MS, is "starting": every reader says so calmly and reads again
// after `retryInMs`, until the database answers or the window passes. After
// the window the same answer is the platform failure it is.

/** How long after its ontology was created a database may still be starting. */
export const STARTING_MS = 15 * 60_000;

/**
 * Whether a database whose ontology was created at `createdAt` (ISO) may
 * still be starting at `now` (ms). A missing or unreadable time never is, nor
 * is any in a local build: its one HelixDB serves every database from the
 * start, so a refusal there is the failure it says.
 *
 * @example
 * const t = Date.parse("2026-10-19T08:13:15Z");
 * mayBeStarting("2026-10-19T08:13:15Z", t + 60_000); // true
 * mayBeStarting("2026-10-19T08:13:15Z", t + STARTING_MS); // false
 * mayBeStarting(undefined, t); // false
 */
export function mayBeStarting(
  createdAt: string | null | undefined,
  now: number,
): boolean {
  const at = Date.parse(createdAt || "");
  return !LOCAL && Number.isFinite(at) && now - at < STARTING_MS;
}

/**
 * Whether the gateway answered as it does for a tenant it hasn't taken on
 * yet: 503 `rate_limit_unavailable` (no rate limit for it yet). The broker's
 * platform key is cluster-scoped, so a new tenant passes the gateway's key
 * check and is stopped only at its rate limiter. Every other answer (401, 403,
 * `backend_unavailable`) is about the platform or the shared cluster, not
 * the database's age, and stays the failure it is.
 *
 * @example
 * isNewDatabaseAnswer({ kind: "database", status: 503, code: "rate_limit_unavailable", ... }); // true
 * isNewDatabaseAnswer({ kind: "database", status: 503, code: "backend_unavailable", ... }); // false
 */
export function isNewDatabaseAnswer(failure: DatabaseQueryFailure): boolean {
  return (
    failure.kind === "database" &&
    failure.status === 503 &&
    failure.code === "rate_limit_unavailable"
  );
}

/**
 * How long to wait before reading a starting database again, by its age:
 * 5 s in its first minute, doubling each minute after, to at most 30 s.
 * Counted from its creation rather than per reader, every read of it (the
 * explorer's, each page's queries, another tab's) backs off alike and none
 * keeps state.
 *
 * @example
 * startingRetryMs(20_000); // 5_000
 * startingRetryMs(90_000); // 10_000
 * startingRetryMs(150_000); // 20_000
 * startingRetryMs(600_000); // 30_000
 */
export function startingRetryMs(ageMs: number): number {
  const minutes = Math.max(0, Math.floor(ageMs / 60_000));
  return Math.min(30_000, 5_000 * 2 ** Math.min(minutes, 3));
}

/**
 * `failure` as a starting failure when it is a new database's answer within
 * its window (mayBeStarting), keeping its status, code and body; otherwise
 * `failure` itself.
 */
export function startingOr(
  failure: DatabaseQueryFailure,
  createdAt: string | null | undefined,
  now: number,
): DatabaseQueryFailure {
  if (!isNewDatabaseAnswer(failure) || !mayBeStarting(createdAt, now))
    return failure;
  return {
    ...failure,
    kind: "starting",
    message: MESSAGES.starting,
    retryInMs: startingRetryMs(now - Date.parse(createdAt || "")),
  };
}

const isAbortError = (e: unknown) =>
  typeof e === "object" &&
  e !== null &&
  (e as { name?: unknown }).name === "AbortError";

/**
 * Runs one read. Never throws: validation, transport, permission, broker and
 * database failures all come back as `{ ok: false, kind }`, and a database
 * that is still starting as `kind: "starting"` with when to read it again.
 */
export async function executeDatabaseQuery(
  target: DatabaseQueryTarget,
  request: DynamicQueryRequest,
  options: ExecuteOptions = {},
): Promise<DatabaseQueryResult> {
  const { signal } = options;
  const invalid = requestValidationMessage(request);
  if (invalid) return failure("invalid", 0, invalid, 0);
  let queryJson: string;
  try {
    queryJson = encodeQueryJson(request);
  } catch {
    return failure("invalid", 0, "The query can’t be serialized as JSON.", 0);
  }
  if (signal?.aborted) return failure("aborted", 0, MESSAGES.aborted, 0);
  const startedAt = performance.now();
  const elapsed = () => Math.round(performance.now() - startedAt);
  let payload: unknown;
  try {
    payload = await api<ExecuteDatabaseQueryResponse>(
      "/v1/databases:query-read",
      {
        database: { tenantId: target.tenantId },
        queryJson,
      } satisfies ExecuteDatabaseQueryRequest,
      "POST",
      undefined,
      { signal },
    );
  } catch (e) {
    if (signal?.aborted || isAbortError(e))
      return failure("aborted", 0, MESSAGES.aborted, elapsed());
    if (e instanceof ApiError) return outerFailure(e, elapsed());
    return failure("network", 0, MESSAGES.network, elapsed());
  }
  const elapsedMs = elapsed();
  if (!isExecuteResponse(payload))
    return failure("malformed", 0, MESSAGES.malformed, elapsedMs, {
      body: payload,
    });
  let decoded: { text: string; bytes: number };
  try {
    decoded = decodeBody(payload.responseJson ?? "");
  } catch {
    return failure(
      "malformed",
      payload.statusCode,
      MESSAGES.undecodable,
      elapsedMs,
      {
        body: payload,
      },
    );
  }
  const result = databaseResponseResult(
    {
      status: payload.statusCode,
      contentType: payload.contentType || null,
      text: decoded.text,
      bytes: decoded.bytes,
    },
    elapsedMs,
  );
  return result.ok ? result : startingOr(result, target.createdAt, Date.now());
}

export function createQueryExecutor(
  target: DatabaseQueryTarget,
): QueryExecutor {
  return (request, options) => executeDatabaseQuery(target, request, options);
}
