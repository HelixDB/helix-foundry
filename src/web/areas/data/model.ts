import type {
  Connector,
  ConnectorConfig,
  ConnectorForeignKey,
  ConnectorKindName,
  ConnectorSync,
  ConnectorTable,
  DatabaseConnectorConfig,
  DocumentSetConnector,
  EventSourceCapacity,
  EventSourceField,
  EventsConnectorConfig,
  FoundryDocument,
  FoundryDocumentSet,
  GitHubConnectorConfig,
  GoogleWorkspaceConnectorConfig,
  GranolaConnectorConfig,
  ListFoundryEventBatchesResponse,
  PostHogConnectorConfig,
  S3ConnectorConfig,
  S3ConnectorSetup,
  SlackConnectorConfig,
  SyncedTable,
} from "../../../shared/wfe";
import type { Status } from "../../kit";
import { formatNumber, formatRelative, plural } from "../../kit/util";
import type { WorkspaceRole } from "../../ui";

// The Data area's logic, kept apart from its components so it is tested on
// its own: connector kinds, the Data page's tree of them and of the
// documents' sets, the credential
// forms and the configs they send, sync states, API sources' states,
// capacity, text fields and documents, how synced tables link to each
// other, and S3's policies, sizes and previews.

/** The WFE's connector kinds, in its catalog's order: API sources last. */
export const KINDS: readonly ConnectorKindName[] = [
  "stripe",
  "workos",
  "posthog",
  "github",
  "linear",
  "calcom",
  "granola",
  "slack",
  "notion",
  "google",
  "postgres",
  "mysql",
  "s3",
  "events",
];
/**
 * A kind connected through a credential form (connection-form.tsx): every
 * kind but events, an API source, which has no credential.
 */
export type CredentialKind = ConnectionForm["kind"];
export const isKnownKind = (kind: string): kind is ConnectorKindName =>
  (KINDS as readonly string[]).includes(kind);
/**
 * An API source (kind events): people and their agents send it pieces of
 * data, which it keeps in one table, `events`. It has no credential, test or
 * schedule: it syncs when pieces arrive.
 */
export const isEventSource = (kind: string): kind is "events" =>
  kind === "events";
export const isDatabase = (kind: string): kind is "postgres" | "mysql" =>
  kind === "postgres" || kind === "mysql";
/** Kinds whose test lists tables to choose from: the databases and S3. */
export const hasTableChoice = (
  kind: string,
): kind is "postgres" | "mysql" | "s3" => isDatabase(kind) || kind === "s3";

/** Owners and admins hold workspace.connectors.manage; members only read. */
export const canManage = (role: WorkspaceRole) => role !== "member";

// ---------------------------------------------------------------- Syncs

export const isLiveSync = (s?: ConnectorSync | null) =>
  s?.status === "CONNECTOR_SYNC_STATUS_QUEUED" ||
  s?.status === "CONNECTOR_SYNC_STATUS_RUNNING";

/**
 * A sync as a status badge. A queued sync that has already run and waits
 * for its next attempt is retrying; a connector with no sync is not synced.
 *
 * @example
 * syncBadge(null); // { status: "draft", label: "Not synced" }
 */
export function syncBadge(
  s: ConnectorSync | null | undefined,
  now = Date.now(),
): { status: Status; label: string } {
  switch (s?.status) {
    case "CONNECTOR_SYNC_STATUS_QUEUED":
      return (s.attempts || 0) > 0 && Date.parse(s.nextAttemptAt || "") > now
        ? { status: "queued", label: "Retrying" }
        : { status: "queued", label: "Queued" };
    case "CONNECTOR_SYNC_STATUS_RUNNING":
      return { status: "running", label: "Syncing" };
    case "CONNECTOR_SYNC_STATUS_SUCCEEDED":
      return { status: "succeeded", label: "Synced" };
    case "CONNECTOR_SYNC_STATUS_FAILED":
      return { status: "failed", label: "Failed" };
    default:
      return { status: "draft", label: "Not synced" };
  }
}

/** When the connector's tables were last refreshed: its newest table snapshot. */
export function lastSyncedAt(c: Connector): string | undefined {
  return (c.tables || [])
    .map((t) => t.syncedAt || "")
    .filter(Boolean)
    .sort()
    .at(-1);
}

export const rowTotal = (c: Connector) =>
  (c.tables || []).reduce((n, t) => n + Number(t.rowCount || 0), 0);

// ---------------------------------------------------------------- API sources

/**
 * An API source's settings as the WFE returns them: its description, and
 * the fields its pieces have carried, each with the JSON type it keeps.
 * Empty for any other kind, or a config the WFE left out.
 *
 * @example
 * eventsConfig({ id: "c", kind: "events", config: { events: { description: "Orders" } } });
 * // { description: "Orders" }
 */
export const eventsConfig = (c: Connector): EventsConnectorConfig =>
  (c.config && "events" in c.config && c.config.events) || {};

/** An API source's one table, `events`, once a sync has written it. */
export const eventsTable = (c: Connector): SyncedTable | undefined =>
  c.tables?.find((t) => t.name === "events");

/**
 * Where an API source's pieces are, from its connector and its newest
 * sends (`event-batches`):
 * - `empty`: nothing was ever sent to it;
 * - `received`: pieces arrived, and none is in its table yet;
 * - `synced`: its table holds `rows` pieces (one per id), as of `syncedAt`,
 *   and `arriving` more wait for its next sync.
 */
export type EventSourceState =
  | { type: "empty" }
  | { type: "received"; pieces: number }
  | { type: "synced"; rows: number; syncedAt?: string; arriving: number };

/**
 * An API source's state. Its connector and its sends are read apart, so a
 * connector read before a sync ended may show no rows when no piece is
 * pending any more: the pieces sent count as received until it is read
 * again.
 *
 * @example
 * eventSourceState({ id: "c", kind: "events" }, { batches: [], pendingPieces: "0" });
 * // { type: "empty" }
 */
export function eventSourceState(
  c: Connector,
  sends: ListFoundryEventBatchesResponse,
): EventSourceState {
  const table = eventsTable(c),
    rows = Number(table?.rowCount || 0),
    pending = Number(sends.pendingPieces || 0);
  if (rows > 0)
    return {
      type: "synced",
      rows,
      ...(table?.syncedAt ? { syncedAt: table.syncedAt } : {}),
      arriving: pending,
    };
  const sent =
    pending || (sends.batches || []).reduce((n, b) => n + (b.pieces || 0), 0);
  return sent ? { type: "received", pieces: sent } : { type: "empty" };
}

/**
 * An API source's state in a line, the same wherever it shows.
 *
 * @example
 * eventSourceLine({ type: "received", pieces: 3 });
 * // "3 pieces received; its first sync is about a minute away"
 * eventSourceLine({ type: "synced", rows: 1200, syncedAt: "2026-10-23T10:00:00Z", arriving: 5 }, Date.parse("2026-10-23T10:05:00Z"));
 * // "1,200 pieces, synced 5m ago, 5 more arriving"
 */
export function eventSourceLine(s: EventSourceState, now = Date.now()) {
  switch (s.type) {
    case "empty":
      return "Waiting for its first pieces";
    case "received":
      return `${plural(s.pieces, "piece")} received; its first sync is about a minute away`;
    case "synced":
      return [
        plural(s.rows, "piece"),
        s.syncedAt && `synced ${formatRelative(s.syncedAt, now)}`,
        s.arriving > 0 && `${formatNumber(s.arriving)} more arriving`,
      ]
        .filter(Boolean)
        .join(", ");
  }
}

/**
 * Whether an API source waits for pieces to reach its table: its table has
 * no rows yet, or pieces were sent since its last sync. A page showing a
 * source that waits polls its sends and its connector, so its state moves on
 * without a reload. Before its sends are read, only its rows tell.
 *
 * @example
 * eventSourceWaiting({ id: "c", kind: "events" }); // true
 */
export const eventSourceWaiting = (
  c: Connector,
  sends?: ListFoundryEventBatchesResponse,
) =>
  Number(eventsTable(c)?.rowCount || 0) === 0 ||
  Number(sends?.pendingPieces || 0) > 0;

/** From this share of either cap a source's capacity shows as a warning. */
export const EVENT_CAPACITY_WARNING = 80;
const SIZE_UNITS = ["bytes", "KiB", "MiB", "GiB", "TiB"];

/**
 * How full an API source is against the caps every table has: `percent`,
 * the larger of its pieces' and its bytes' share of their cap in whole
 * percent rounded down (as the WFE's warning counts it), `warn` from 80%,
 * and a line saying both, its bytes in the unit of their cap. Undefined
 * without both caps.
 *
 * @example
 * eventCapacity({ rows: "240000", maxRows: "2000000", bytes: "268435456", maxBytes: "2147483648" });
 * // { percent: 12, warn: false, text: "12% full: 240,000 of 2,000,000 pieces, 0.3 of 2 GiB" }
 */
export function eventCapacity(
  c?: EventSourceCapacity | null,
): { percent: number; warn: boolean; text: string } | undefined {
  const rows = Number(c?.rows || 0),
    maxRows = Number(c?.maxRows || 0),
    bytes = Number(c?.bytes || 0),
    maxBytes = Number(c?.maxBytes || 0);
  if (!(maxRows > 0) || !(maxBytes > 0)) return undefined;
  const percent = Math.max(
    Math.floor((rows * 100) / maxRows),
    Math.floor((bytes * 100) / maxBytes),
  );
  const unit = Math.min(
      SIZE_UNITS.length - 1,
      Math.floor(Math.log2(maxBytes) / 10),
    ),
    size = (n: number) => String(Math.round((n / 1024 ** unit) * 10) / 10);
  return {
    percent,
    warn: percent >= EVENT_CAPACITY_WARNING,
    text: `${percent}% full: ${formatNumber(rows)} of ${formatNumber(maxRows)} pieces, ${size(bytes)} of ${size(maxBytes)} ${SIZE_UNITS[unit]}`,
  };
}

// ---------------------------------------------------------------- API sources' documents

/**
 * The most pieces an API source's set keeps as documents, and the most (µ$)
 * work no person started may spend extracting them in a UTC month:
 * foundry.events' max_extracted and max_system_usd_per_source_month, the
 * same on every plan. A set says its own (its connector's maxDocuments and
 * maxMonthSystemUsdMicros); these stand in before a source has one.
 */
export const EVENT_SET_DEFAULTS = {
  maxDocuments: 500,
  maxMonthUsdMicros: 5_000_000,
} as const;

/**
 * An API source's own set (stream `pieces`): its newest pieces as Markdown
 * documents, which its first sync with a text field makes. Clearing the
 * text field empties it; it stays.
 *
 * @example
 * piecesSet([{ id: "s", connector: { connectorId: "c", stream: "pieces" } }], "c")?.id; // "s"
 */
export const piecesSet = (
  sets: readonly FoundryDocumentSet[] | undefined,
  connectorId: string,
) =>
  sets?.find(
    (s) =>
      s.connector?.connectorId === connectorId &&
      s.connector.stream === "pieces",
  );

/**
 * The fields an API source's text field is chosen from: those its pieces
 * have carried as a string or only as null (no type is null so far), in the
 * WFE's order (byte order of name), with its text field first should its
 * fields not list it. Another name may be typed: textFieldProblem checks it.
 *
 * @example
 * textFieldChoices({ fields: [{ name: "amount", type: "number" }, { name: "reply", type: "string" }] });
 * // ["reply"]
 */
export function textFieldChoices(config: EventsConnectorConfig): string[] {
  const held = config.textField || "",
    names = (config.fields || [])
      .filter((f) => !f.type || f.type === "string" || f.type === "null")
      .map((f) => f.name);
  return held && !names.includes(held) ? [held, ...names] : names;
}

/** The columns every piece's row has beside its data's fields. */
const PIECE_COLUMNS = ["id", "received_at", "occurred_at"];
/** The longest name a piece's top-level field may have, in UTF-8 bytes. */
const MAX_FIELD_NAME_BYTES = 64;

/**
 * Why `name` can't be an API source's text field, or undefined when it can,
 * checked as the WFE checks it (events.ValidFieldName, then
 * Schema.WithText): 1 to 64 bytes of UTF-8, no control character or
 * backtick, not a column every piece has (id, received_at, occurred_at),
 * and not a field `fields` hold as anything but a string or null. A field
 * no piece has carried yet is one, which it then locks as a string. The WFE
 * alone refuses a new field past the 200 a source may have. "" is no name:
 * choosing none clears the text field, and is never checked.
 *
 * @example
 * textFieldProblem("amount", [{ name: "amount", type: "number" }]);
 * // "A text field holds strings; this API source holds amount as a number."
 */
export function textFieldProblem(
  name: string,
  fields: readonly EventSourceField[] = [],
): string | undefined {
  const size = utf8.encode(name).length,
    held = fields.find((f) => f.name === name)?.type;
  if (size < 1 || size > MAX_FIELD_NAME_BYTES)
    return `A field’s name is 1 to ${MAX_FIELD_NAME_BYTES} bytes; this one is ${size}.`;
  // Go's unicode.IsControl: C0, DEL and C1.
  if (/\p{Cc}/u.test(name)) return "A field’s name has no control characters.";
  if (name.includes("`")) return "A field’s name has no backticks.";
  if (PIECE_COLUMNS.includes(name))
    return `${name} is a column of every piece, not a field of its data.`;
  return held && held !== "string" && held !== "null"
    ? `A text field holds strings; this API source holds ${name} as ${/^[aeiou]/.test(held) ? "an" : "a"} ${held}.`
    : undefined;
}

/**
 * What saving `next` as an API source's text field does, as its confirm
 * says it, `held` being the one it has: setting or changing it re-reads the
 * newest pieces its set keeps, and extracting them comes out of the set's
 * allowance for the month; clearing it removes their documents at its next
 * sync. Either names `readers`, the ontologies that read its set. `set` is
 * its own (piecesSet), whose numbers win over EVENT_SET_DEFAULTS. The
 * re-read is a sync a person started, which writes its documents even while
 * the set is paused (setSpend's `paused` at `now`); extracting them then
 * waits for the 1st of next month, or for a load a person starts, and until
 * then the graph keeps what it read from the old field's text.
 *
 * @example
 * textFieldConfirm("", "reply", undefined, ["Support"]).text;
 * // "Foundry re-reads this API source’s newest 500 pieces. Extracting them comes out of its $5 for the month. Support reads them."
 */
export function textFieldConfirm(
  held: string,
  next: string,
  set: FoundryDocumentSet | undefined,
  readers: readonly string[],
  now = Date.now(),
): { title: string; text: string; action: string } {
  const max = formatNumber(
      set?.connector?.maxDocuments || EVENT_SET_DEFAULTS.maxDocuments,
    ),
    micros = Number(
      set?.connector?.maxMonthSystemUsdMicros ||
        EVENT_SET_DEFAULTS.maxMonthUsdMicros,
    ),
    usd = Number.isInteger(micros / 1e6)
      ? `$${micros / 1e6}`
      : `$${(micros / 1e6).toFixed(2)}`,
    spend = setSpend(set?.connector, now),
    read = readers.length
      ? ` ${new Intl.ListFormat("en", { style: "long", type: "conjunction" }).format(readers)} ${readers.length === 1 ? "reads" : "read"} them.`
      : "";
  if (!next)
    return {
      title: `Stop using ${held} as the text field?`,
      text: `Foundry removes this API source’s pieces as documents at its next sync.${read}`,
      action: "Clear text field",
    };
  return {
    title: `Use ${next} as the text field?`,
    text:
      spend?.level === "paused"
        ? `Foundry re-reads this API source’s newest ${max} pieces. Its extraction is paused until ${spend.until}, so ontologies read the new text from then, or from a load a person starts.${read}`
        : `Foundry re-reads this API source’s newest ${max} pieces. Extracting them comes out of its ${usd} for the month.${read}`,
    action: "Save text field",
  };
}

/**
 * A set's spend against its allowance this UTC month: `text` says how much
 * of how much; `warn` from 80% (EVENT_CAPACITY_WARNING); `paused` while the
 * WFE says the set is (its `paused`: it holds documents, and its allowance,
 * the workspace's for all its API sources, or the month's share for work no
 * person started is spent), when its syncs write no documents `until` the
 * 1st of next month (UTC), as "November 1".
 */
export type SetSpend =
  | { level: "room" | "warn"; text: string }
  | { level: "paused"; text: string; until: string };

/**
 * What work no person started has spent extracting a set's documents this
 * UTC month, of its allowance: "$1.20 of $5.00 this month". Undefined for a
 * set without an allowance (only an API source's has one). Cents are
 * rounded down, so it never says more was spent than was. A set may be
 * paused with some of its own allowance left (the workspace's is spent), and
 * not paused with none left (it holds no documents, so a sync writes them).
 *
 * @example
 * setSpend({ monthSystemUsdMicros: "1200000", maxMonthSystemUsdMicros: "5000000" });
 * // { level: "room", text: "$1.20 of $5.00 this month" }
 */
export function setSpend(
  c: DocumentSetConnector | null | undefined,
  now = Date.now(),
): SetSpend | undefined {
  const spent = Math.max(0, Number(c?.monthSystemUsdMicros) || 0),
    max = Number(c?.maxMonthSystemUsdMicros || 0);
  if (!(max > 0)) return undefined;
  const usd = (micros: number) =>
      `$${(Math.floor(micros / 10_000) / 100).toFixed(2)}`,
    text = `${usd(spent)} of ${usd(max)} this month`;
  if (c?.paused) {
    const at = new Date(now);
    return {
      level: "paused",
      text,
      until: new Date(
        Date.UTC(at.getUTCFullYear(), at.getUTCMonth() + 1, 1),
      ).toLocaleDateString("en-US", {
        month: "long",
        day: "numeric",
        timeZone: "UTC",
      }),
    };
  }
  return {
    level: spent * 100 >= max * EVENT_CAPACITY_WARNING ? "warn" : "room",
    text,
  };
}

/**
 * How many documents a connector's set holds of the most it keeps, "120 of
 * 500", or just how many when it says no most.
 *
 * @example
 * setHolding({ id: "s", documents: 120, connector: { maxDocuments: 500 } }); // "120 of 500"
 */
export function setHolding(s: FoundryDocumentSet) {
  const held = formatNumber(s.documents || 0),
    max = s.connector?.maxDocuments || 0;
  return max ? `${held} of ${formatNumber(max)}` : held;
}

// ---------------------------------------------------------------- Tree

/**
 * A connector in the Data page's tree with what it lists: its tables, then
 * its own document sets (FoundryDocumentSet.connector), or with a search
 * those the search found it by (none when its name or kind matched).
 */
export type TreeItem = {
  connector: Connector;
  tables: SyncedTable[];
  sets: FoundryDocumentSet[];
};
/** A folder of the tree: one connector kind and its connectors. */
export type KindFolder = { kind: string; items: TreeItem[] };

/**
 * The Data page's tree: a folder per kind that has a connector, in `kinds`'
 * order (the WFE's connector kinds), then any kind it doesn't list, in the
 * order of its first connector. Each folder keeps its connectors in the
 * order the WFE lists them (as created), each with its tables and the sets
 * of `sets` it owns. A search `term` (any case) keeps a connector its name
 * or kind's `label` matches (listing nothing), else one with tables or sets
 * whose names match (those), and drops the folders it empties. A connector
 * without a name is called by its kind's label, as the pages call it.
 *
 * @example
 * kindFolders(
 *   [
 *     { id: "a", kind: "postgres", name: "Shop" },
 *     { id: "b", kind: "stripe" },
 *     { id: "c", kind: "postgres", name: "Billing" },
 *   ],
 *   ["stripe", "postgres"],
 * ).map((f) => [f.kind, f.items.map((i) => i.connector.id)]);
 * // [["stripe", ["b"]], ["postgres", ["a", "c"]]]
 */
export function kindFolders(
  connectors: readonly Connector[],
  kinds: readonly string[],
  term = "",
  label: (kind: string) => string = (kind) => kind,
  sets: readonly FoundryDocumentSet[] = [],
): KindFolder[] {
  const wanted = term.trim().toLowerCase(),
    matches = (text = "") => text.toLowerCase().includes(wanted);
  const items = connectors.flatMap((connector): TreeItem[] => {
    const kind = label(connector.kind),
      tables = connector.tables || [],
      own = sets.filter((s) => s.connector?.connectorId === connector.id);
    if (!wanted) return [{ connector, tables, sets: own }];
    if (matches(`${connector.name || kind} ${kind}`))
      return [{ connector, tables: [], sets: [] }];
    const found = {
      connector,
      tables: tables.filter((t) => matches(t.name)),
      sets: own.filter((s) => matches(s.name)),
    };
    return found.tables.length || found.sets.length ? [found] : [];
  });
  const order = [...new Set([...kinds, ...connectors.map((c) => c.kind)])];
  return order
    .map((kind) => ({
      kind,
      items: items.filter((i) => i.connector.kind === kind),
    }))
    .filter((f) => f.items.length);
}

/** A folder of the tree's documents/: one set and the documents it holds. */
export type SetFolder = {
  set: FoundryDocumentSet;
  documents: FoundryDocument[];
};

/**
 * A folder per set in the order the WFE lists them (oldest first), each
 * holding those of `documents` in it in the order they were uploaded
 * (oldest first), so new ones go to the end: documents/ of a picker, and
 * an ontology's documents. A document whose set isn't listed (lists read at
 * different times) waits for the next read. The workspace's list is its
 * uploads, so a connector's set finds none in it.
 * Without a search every set shows, empty ones too. A search `term` (any
 * case) keeps a set its name matches, with all its documents, else one with
 * documents whose names match (those documents).
 *
 * @example
 * setFolders(
 *   [{ id: "g", name: "General" }, { id: "b", name: "Board" }],
 *   [{ id: "d1", name: "minutes.md", setId: "b", createdAt: "2026-10-01" }],
 *   "minutes",
 * ).map((f) => [f.set.id, f.documents.map((d) => d.id)]);
 * // [["b", ["d1"]]]
 */
export function setFolders(
  sets: readonly FoundryDocumentSet[],
  documents: readonly FoundryDocument[],
  term = "",
): SetFolder[] {
  const wanted = term.trim().toLowerCase(),
    matches = (text = "") => text.toLowerCase().includes(wanted);
  const byAge = [...documents].sort(
    (a, b) =>
      (a.createdAt || "").localeCompare(b.createdAt || "") ||
      a.id.localeCompare(b.id),
  );
  return sets.flatMap((set) => {
    const held = byAge.filter((d) => d.setId === set.id);
    if (!wanted || matches(set.name)) return [{ set, documents: held }];
    const found = held.filter((d) => matches(d.name));
    return found.length ? [{ set, documents: found }] : [];
  });
}

/** How much of a group is picked. */
export type Picked = "all" | "some" | "none";

/**
 * How much of a group `ids` the `selected` ids pick: an empty group picks
 * none.
 *
 * @example
 * picked(["a", "b"], new Set(["a"])); // "some"
 */
export function picked(
  ids: readonly string[],
  selected: ReadonlySet<string>,
): Picked {
  const n = ids.filter((id) => selected.has(id)).length;
  return n === 0 ? "none" : n === ids.length ? "all" : "some";
}

// ---------------------------------------------------------------- Forms

/** The credential form of one kind. Every field is text as typed. */
export type ConnectionForm =
  | {
      kind: "stripe" | "workos" | "linear" | "calcom";
      apiKey: string;
    }
  | {
      kind: "granola";
      apiKey: string;
      // Each note's transcript as a document too, in a Transcripts set
      // beside Notes.
      transcripts: boolean;
    }
  | {
      kind: "slack";
      botToken: string;
      historyDays: string;
    }
  | {
      kind: "notion";
      token: string;
    }
  | {
      kind: "google";
      // The service account's JSON key, as pasted.
      serviceAccountKey: string;
      // The Workspace user's email the service account acts as.
      subject: string;
      eventDays: string;
    }
  | {
      kind: "github";
      token: string;
      // owner/name or repository URLs, one per line or comma-separated;
      // blank reads every repository the token can.
      repos: string;
      reviewDays: string;
    }
  | {
      kind: "posthog";
      apiKey: string;
      region: "us" | "eu" | "custom";
      host: string;
      projectId: string;
      eventDays: string;
    }
  | {
      kind: "postgres" | "mysql";
      host: string;
      port: string;
      database: string;
      user: string;
      password: string;
      schema: string;
      tls: "verify" | "disable";
      ca: string;
    }
  | {
      kind: "s3";
      bucket: string;
      // The folder; blank reads the whole bucket.
      prefix: string;
      // Blank lets the test detect it.
      region: string;
      // Both credentials stay in the form; `access` says which is sent.
      access: "role" | "keys";
      roleArn: string;
      accessKeyId: string;
      secretAccessKey: string;
      // Blank for AWS; an S3-compatible store's HTTPS origin otherwise.
      endpoint: string;
    };

export function emptyForm(kind: CredentialKind): ConnectionForm {
  switch (kind) {
    case "stripe":
    case "workos":
    case "linear":
    case "calcom":
      return { kind, apiKey: "" };
    case "granola":
      return { kind, apiKey: "", transcripts: false };
    case "slack":
      return { kind, botToken: "", historyDays: "90" };
    case "notion":
      return { kind, token: "" };
    case "google":
      return { kind, serviceAccountKey: "", subject: "", eventDays: "90" };
    case "github":
      return { kind, token: "", repos: "", reviewDays: "90" };
    case "posthog":
      return {
        kind,
        apiKey: "",
        region: "us",
        host: "",
        projectId: "",
        eventDays: "30",
      };
    case "postgres":
    case "mysql":
      return {
        kind,
        host: "",
        port: kind === "postgres" ? "5432" : "3306",
        database: "",
        user: "",
        password: "",
        schema: "",
        tls: "verify",
        ca: "",
      };
    case "s3":
      return {
        kind,
        bucket: "",
        prefix: "",
        region: "",
        access: "role",
        roleArn: "",
        accessKeyId: "",
        secretAccessKey: "",
        endpoint: "",
      };
  }
}

/** The kind a config sets, or undefined for an empty or unknown config. */
export function configKind(
  config?: ConnectorConfig | null,
): ConnectorKindName | undefined {
  return config ? KINDS.find((k) => k in config) : undefined;
}
function databaseOf(
  config: ConnectorConfig,
): DatabaseConnectorConfig | undefined {
  return "postgres" in config
    ? config.postgres
    : "mysql" in config
      ? config.mysql
      : undefined;
}

/**
 * A stored connector's settings as its form, for editing. The credential
 * never comes back from the WFE, so it starts blank: left blank, the WFE keeps
 * the stored one.
 */
export function formFromConfig(
  kind: CredentialKind,
  config?: ConnectorConfig | null,
): ConnectionForm {
  const form = emptyForm(kind);
  if (!config || configKind(config) !== kind) return form;
  switch (form.kind) {
    case "posthog": {
      const p = (config as { posthog: PostHogConnectorConfig }).posthog;
      return {
        ...form,
        region: p.region === "eu" || p.region === "custom" ? p.region : "us",
        host: p.host || "",
        projectId: p.projectId || "",
        eventDays: String(p.eventDays || 30),
      };
    }
    case "github": {
      const g = (config as { github: GitHubConnectorConfig }).github;
      return {
        ...form,
        repos: (g.repos || []).join("\n"),
        reviewDays: String(g.reviewDays || 90),
      };
    }
    case "granola":
      return {
        ...form,
        transcripts: !!(config as { granola: GranolaConnectorConfig }).granola
          .transcripts,
      };
    case "slack":
      return {
        ...form,
        historyDays: String(
          (config as { slack: SlackConnectorConfig }).slack.historyDays || 90,
        ),
      };
    case "google": {
      const g = (config as { google: GoogleWorkspaceConnectorConfig }).google;
      return {
        ...form,
        subject: g.subject || "",
        eventDays: String(g.eventDays || 90),
      };
    }
    case "postgres":
    case "mysql": {
      const d = databaseOf(config)!;
      return {
        ...form,
        host: d.host || "",
        port: d.port ? String(d.port) : form.port,
        database: d.database || "",
        user: d.user || "",
        schema: form.kind === "postgres" ? d.schema || "" : "",
        tls: d.disableTls ? "disable" : "verify",
        ca: d.ca || "",
      };
    }
    case "s3": {
      const s = (config as { s3: S3ConnectorConfig }).s3,
        keys = "keys" in s ? s.keys : undefined;
      return {
        ...form,
        bucket: s.bucket || "",
        prefix: s.prefix || "",
        region: s.region || "",
        access: keys ? "keys" : "role",
        roleArn: ("role" in s && s.role?.roleArn) || "",
        accessKeyId: keys?.accessKeyId || "",
        endpoint: keys?.endpoint || "",
      };
    }
    default:
      return form;
  }
}

/**
 * The repositories a GitHub form names, as owner/name in the order typed:
 * one per line or comma-separated, a pasted github.com URL read as its
 * repository. The WFE checks each and drops repeats.
 *
 * @example
 * githubRepos("acme/api\nhttps://github.com/acme/web.git, "); // ["acme/api", "acme/web"]
 */
export function githubRepos(text: string): string[] {
  return text
    .split(/[\s,]+/)
    .filter(Boolean)
    .map((r) =>
      r
        .replace(/^(?:https?:\/\/)?(?:www\.)?github\.com\//i, "")
        .replace(/(?:\.git)?\/*$/, ""),
    );
}

/**
 * The config a form sends to test, create or update a connector. `tables`
 * is the database or S3 sync selection; empty means every table. Blank
 * optional fields are left for the WFE's defaults.
 *
 * @example
 * toConfig({ kind: "stripe", apiKey: " sk_test_1 " }); // { stripe: { apiKey: "sk_test_1" } }
 */
export function toConfig(
  form: ConnectionForm,
  tables: string[] = [],
): ConnectorConfig {
  switch (form.kind) {
    case "stripe":
      return { stripe: { apiKey: form.apiKey.trim() } };
    case "workos":
      return { workos: { apiKey: form.apiKey.trim() } };
    case "linear":
      return { linear: { apiKey: form.apiKey.trim() } };
    case "calcom":
      return { calcom: { apiKey: form.apiKey.trim() } };
    case "granola":
      return {
        granola: {
          apiKey: form.apiKey.trim(),
          transcripts: form.transcripts,
        },
      };
    case "slack":
      return {
        slack: {
          botToken: form.botToken.trim(),
          historyDays: Number(form.historyDays) || 0,
        },
      };
    case "notion":
      return { notion: { token: form.token.trim() } };
    case "google":
      return {
        google: {
          serviceAccountKey: form.serviceAccountKey.trim(),
          subject: form.subject.trim(),
          eventDays: Number(form.eventDays) || 0,
        },
      };
    case "github":
      return {
        github: {
          token: form.token.trim(),
          repos: githubRepos(form.repos),
          reviewDays: Number(form.reviewDays) || 0,
        },
      };
    case "posthog":
      return {
        posthog: {
          apiKey: form.apiKey.trim(),
          region: form.region,
          ...(form.region === "custom" ? { host: form.host.trim() } : {}),
          ...(form.projectId.trim()
            ? { projectId: form.projectId.trim() }
            : {}),
          eventDays: Number(form.eventDays) || 0,
        },
      };
    case "postgres":
    case "mysql": {
      const db: DatabaseConnectorConfig = {
        host: form.host.trim(),
        port: Number(form.port) || 0,
        database: form.database.trim(),
        user: form.user.trim(),
        // Sent byte for byte: a password may start or end with spaces.
        password: form.password,
        ...(form.kind === "postgres" && form.schema.trim()
          ? { schema: form.schema.trim() }
          : {}),
        ...(tables.length ? { tables } : {}),
        ...(form.tls === "disable" ? { disableTls: true } : {}),
        ...(form.ca.trim() ? { ca: form.ca } : {}),
      };
      return form.kind === "postgres" ? { postgres: db } : { mysql: db };
    }
    case "s3": {
      const prefix = form.prefix.trim(),
        region = form.region.trim(),
        endpoint = form.endpoint.trim();
      const s3: S3ConnectorConfig = {
        bucket: form.bucket.trim(),
        ...(prefix ? { prefix } : {}),
        ...(region ? { region } : {}),
        ...(tables.length ? { tables } : {}),
        ...(form.access === "role"
          ? { role: { roleArn: form.roleArn.trim() } }
          : {
              keys: {
                accessKeyId: form.accessKeyId.trim(),
                secretAccessKey: form.secretAccessKey.trim(),
                ...(endpoint ? { endpoint } : {}),
              },
            }),
      };
      return { s3 };
    }
  }
}

/**
 * Whether the form holds a credential (an edit may leave it blank): an S3
 * role needs its ARN, an S3 key its secret.
 */
export function hasCredential(form: ConnectionForm) {
  switch (form.kind) {
    case "postgres":
    case "mysql":
      return !!form.password;
    case "s3":
      return form.access === "role"
        ? !!form.roleArn.trim()
        : !!form.secretAccessKey.trim();
    case "github":
    case "notion":
      return !!form.token.trim();
    case "slack":
      return !!form.botToken.trim();
    case "google":
      return !!form.serviceAccountKey.trim();
    default:
      return !!form.apiKey.trim();
  }
}

/** The longest connection name the WFE stores, in UTF-8 bytes. */
export const MAX_NAME_BYTES = 120;
const utf8 = new TextEncoder();

/**
 * The name a new connection gets when none is typed: the database's name,
 * the bucket and folder, a GitHub connection's one repository, or the app
 * with a Stripe key's mode. A bucket and
 * folder too long to store keep the bucket and the folder's last segments
 * ("acme/…/daily/2026"), or are cut short, within MAX_NAME_BYTES.
 *
 * @example
 * suggestedName({ kind: "stripe", apiKey: "rk_live_abc" }); // "Stripe (live)"
 */
export function suggestedName(form: ConnectionForm) {
  switch (form.kind) {
    case "stripe": {
      const mode = /^(?:sk|rk)_(live|test)_/.exec(form.apiKey.trim())?.[1];
      return mode ? `Stripe (${mode})` : "Stripe";
    }
    case "workos":
      return "WorkOS";
    case "posthog":
      return "PostHog";
    case "github": {
      const repos = githubRepos(form.repos);
      return repos.length === 1 ? repos[0]! : "GitHub";
    }
    case "linear":
      return "Linear";
    case "calcom":
      return "Cal.com";
    case "granola":
      return "Granola";
    case "slack":
      return "Slack";
    case "notion":
      return "Notion";
    case "google":
      return "Google Workspace";
    case "postgres":
      return form.database.trim() || "PostgreSQL";
    case "mysql":
      return form.database.trim() || "MySQL";
    case "s3": {
      const bucket = form.bucket.trim(),
        folder = form.prefix.trim().replace(/^\/+|\/+$/g, ""),
        full = [bucket, folder].filter(Boolean).join("/");
      const fits = (name: string) => utf8.encode(name).length <= MAX_NAME_BYTES;
      if (!bucket) return "Amazon S3";
      if (fits(full)) return full;
      // Shorter tails fit first, so the scan stops at the longest that does.
      const segments = folder.split("/"),
        tail = (from: number) =>
          [bucket, "…", ...segments.slice(from)].join("/");
      let from = segments.length;
      while (from > 1 && fits(tail(from - 1))) from--;
      if (from < segments.length) return tail(from);
      // Even the last segment is too long: cut by characters, never mid one.
      let cut = "";
      for (const ch of full) {
        if (!fits(cut + ch + "…")) break;
        cut += ch;
      }
      return cut + "…";
    }
  }
}

/**
 * Where a connector reads from, in a line: "foundry@db.example.com:5432/app",
 * "s3://acme-exports/foundry/" or a GitHub connection's repositories. Never
 * includes a credential.
 */
export function sourceSummary(c: Connector): string {
  const config = c.config;
  if (!config) return c.secretHint || "";
  if ("s3" in config)
    return `s3://${config.s3.bucket || ""}/${config.s3.prefix || ""}`;
  const d = databaseOf(config);
  if (d) {
    const where = `${d.host}${d.port ? ":" + d.port : ""}/${d.database}`;
    return (d.user ? d.user + "@" : "") + where;
  }
  if ("posthog" in config) {
    const p = config.posthog,
      host =
        p.region === "custom"
          ? (p.host || "").replace(/^https:\/\//, "")
          : `${p.region === "eu" ? "EU" : "US"} Cloud`;
    return [host, p.projectId && `project ${p.projectId}`]
      .filter(Boolean)
      .join(" · ");
  }
  if ("github" in config) {
    const repos = config.github.repos || [];
    return repos.length > 2
      ? `${repos.length} repositories`
      : repos.join(", ") || "Every repository";
  }
  return c.secretHint || "";
}

/**
 * The tables a selection syncs: none chosen sends nothing to sync,
 * every table sends [] (every table, including ones added later), otherwise
 * the chosen names in discovery order.
 */
export function tableSelection(
  all: string[],
  chosen: ReadonlySet<string>,
): string[] | "none" {
  const picked = all.filter((t) => chosen.has(t));
  if (!picked.length) return "none";
  return picked.length === all.length ? [] : picked;
}

// ---------------------------------------------------------------- Keys

type Keyed = Pick<ConnectorTable, "name" | "foreignKeys">;

/** Tables linked to the chosen ones by a foreign key, not chosen themselves. */
export function relatedTables(
  tables: Keyed[],
  chosen: ReadonlySet<string>,
): Set<string> {
  const related = new Set<string>();
  for (const t of tables)
    for (const fk of t.foreignKeys || []) {
      if (chosen.has(t.name)) related.add(fk.referencedTable);
      if (chosen.has(fk.referencedTable)) related.add(t.name);
    }
  for (const name of chosen) related.delete(name);
  return related;
}

/** The foreign keys in other tables that point at `table`. */
export function referencesTo(
  tables: Keyed[],
  table: string,
): (ConnectorForeignKey & { table: string })[] {
  return tables.flatMap((t) =>
    (t.foreignKeys || [])
      .filter((fk) => fk.referencedTable === table)
      .map((fk) => ({ ...fk, table: t.name })),
  );
}

/**
 * A table's primary key in a few words.
 *
 * @example
 * keyLabel({ primaryKey: ["order_id", "line"] }); // "Key order_id, line"
 * keyLabel({}); // "No primary key"
 */
export function keyLabel(t: Pick<ConnectorTable, "primaryKey">) {
  const key = t.primaryKey || [];
  return key.length ? `Key ${key.join(", ")}` : "No primary key";
}

/**
 * A table's keys in a line: its primary key, then its links if it has any.
 *
 * @example
 * keySummary({ primaryKey: ["id"], foreignKeys: [] }); // "Key id"
 * keySummary({ primaryKey: ["order_id", "line"], foreignKeys: [toAccounts, toUsers] });
 * // "Key order_id, line · 2 links"
 */
export function keySummary(
  t: Pick<ConnectorTable, "primaryKey" | "foreignKeys">,
) {
  const links = (t.foreignKeys || []).length;
  return [
    keyLabel(t),
    links ? `${links} ${links === 1 ? "link" : "links"}` : "",
  ]
    .filter(Boolean)
    .join(" · ");
}

// ---------------------------------------------------------------- S3

/**
 * A pasted s3:// URI as bucket and folder, or null for anything else.
 *
 * @example
 * parseS3Uri("s3://acme-exports/foundry/"); // { bucket: "acme-exports", prefix: "foundry/" }
 */
export function parseS3Uri(
  value: string,
): { bucket: string; prefix: string } | null {
  const m = /^s3:\/\/([^/]*)\/?(.*)$/i.exec(value.trim());
  return m ? { bucket: m[1]!, prefix: m[2]! } : null;
}

// AWS policies as the S3 guide shows them: two-space JSON.
const policy = (statements: object[]) =>
  JSON.stringify({ Version: "2012-10-17", Statement: statements }, null, 2);

/**
 * The trust policy a customer's role needs: Helix's connector role may
 * assume it (and tag the session) only with the workspace's external ID.
 */
export function s3TrustPolicy(setup: S3ConnectorSetup) {
  return policy([
    {
      Effect: "Allow",
      Principal: { AWS: setup.principalArn || "" },
      Action: ["sts:AssumeRole", "sts:TagSession"],
      Condition: { StringEquals: { "sts:ExternalId": setup.externalId || "" } },
    },
  ]);
}

/**
 * The permissions a role or key needs: list the folder and read its files.
 * The folder is normalised as the WFE stores it ("foundry" reads
 * "foundry/"); without one, the whole bucket.
 */
export function s3PermissionsPolicy(bucket: string, prefix: string) {
  const b = bucket.trim(),
    trimmed = prefix.trim().replace(/^\//, ""),
    folder = trimmed && !trimmed.endsWith("/") ? trimmed + "/" : trimmed;
  return policy([
    {
      Sid: "List",
      Effect: "Allow",
      Action: "s3:ListBucket",
      Resource: `arn:aws:s3:::${b}`,
      ...(folder
        ? { Condition: { StringLike: { "s3:prefix": [`${folder}*`] } } }
        : {}),
    },
    {
      Sid: "Read",
      Effect: "Allow",
      Action: "s3:GetObject",
      Resource: `arn:aws:s3:::${b}/${folder}*`,
    },
  ]);
}

const BYTE_UNITS = ["B", "KB", "MB", "GB", "TB", "PB"];
/**
 * An int64 byte count in binary units, as the WFE's limits are written:
 * "20 GB" for 20 GiB. Anything unreadable is "0 B".
 *
 * @example
 * formatBytes("1288490189"); // "1.2 GB"
 */
export function formatBytes(bytes?: string) {
  let n = Math.max(0, Number(bytes) || 0),
    unit = 0;
  while (unit < BYTE_UNITS.length - 1 && Math.round(n * 10) / 10 >= 1024) {
    n /= 1024;
    unit++;
  }
  return `${Math.round(n * 10) / 10} ${BYTE_UNITS[unit]}`;
}

/** A preview cell's longest text, in characters. */
export const PREVIEW_CELL_CHARS = 80;

// One JSON token and the space before it: a string, a structural character,
// or a literal (a number, true, false or null).
const JSON_TOKEN = /\s*("(?:[^"\\]|\\.)*"|[{}[\],:]|[^\s{}[\],:"]+)/gy;

/**
 * A JSON object's members as their source text, so a number keeps its exact
 * digits: JSON.parse rounds 9223372036854775807 and drops 12.30's zero. `text`
 * must already parse as an object; a repeated key keeps its last value, as
 * JSON.parse does.
 */
function rawMembers(text: string): Map<string, string> {
  const members = new Map<string, string>();
  let depth = 0,
    key: string | undefined,
    from = 0;
  for (const m of text.matchAll(JSON_TOKEN)) {
    const token = m[1]!;
    // The object's own members are at depth 1: a key, ":", one value token
    // (a nested value's inner tokens are deeper), then "," or "}".
    if (depth === 1) {
      if (token === "," || token === "}") {
        if (key !== undefined) members.set(key, text.slice(from, m.index));
        key = undefined;
      } else if (key === undefined) {
        key = JSON.parse(token) as string;
        continue;
      } else if (token !== ":") from = m.index + m[0].length - token.length;
    }
    if (token === "{" || token === "[") depth++;
    else if (token === "}" || token === "]") depth--;
  }
  return members;
}

/**
 * A tested table's preview rows as a grid: the table's columns (or the rows'
 * keys when it has none), then each row's cells as text cut to 80
 * characters, null for a null or missing value. Values other than strings
 * show as the WFE wrote them, so numbers keep every digit. A row that is not
 * a JSON object is left out: nothing trusts the shape.
 */
export function previewGrid(t: Pick<ConnectorTable, "columns" | "preview">): {
  columns: string[];
  rows: (string | null)[][];
} {
  const records = (t.preview || []).flatMap((row) => {
    try {
      const v: unknown = JSON.parse(row);
      return v && typeof v === "object" && !Array.isArray(v)
        ? [rawMembers(row)]
        : [];
    } catch {
      return [];
    }
  });
  const columns = (t.columns || []).map((c) => c.name);
  for (const r of records)
    for (const k of r.keys()) if (!columns.includes(k)) columns.push(k);
  const cell = (raw?: string) => {
    if (raw === undefined || raw === "null") return null;
    const text = [...(raw.startsWith('"') ? (JSON.parse(raw) as string) : raw)];
    return text.length > PREVIEW_CELL_CHARS
      ? text.slice(0, PREVIEW_CELL_CHARS - 1).join("") + "…"
      : text.join("");
  };
  return {
    columns,
    rows: records.map((r) => columns.map((k) => cell(r.get(k)))),
  };
}
