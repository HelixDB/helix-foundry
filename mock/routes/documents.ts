import { createHash, randomBytes } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import type {
  ConnectorKindName,
  CreateDocumentResponse,
  CreateDocumentSetRequest,
  DeleteDocumentSetResponse,
  DocumentBytesResponse,
  DocumentContentResponse,
  DocumentExtraction,
  DocumentMediaType,
  DocumentOntology,
  DocumentRead,
  DocumentReadsResponse,
  DocumentResponse,
  DocumentSetResponse,
  DocumentStatus,
  FoundryDocument,
  FoundryDocumentSet,
  FoundryDocumentSource,
  FoundryFileRead,
  FoundryReadKind,
  ListDocumentSetsResponse,
  ListDocumentsResponse,
  MoveDocumentRequest,
  OntologyDocumentStatus,
  UpdateDocumentRequest,
  UpdateDocumentSetRequest,
} from "../../src/shared/wfe";
import { isRouted } from "../graph/scope";
import { blankMdx } from "../mdx";
import { readPdfText, textPdf, type PdfRead } from "../pdf-text";
import { readTableFile, type Row, type TableRead } from "../table-files";
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
  config,
  isReply,
  membership,
  onReset,
  type Membership,
} from "../state";
import {
  advanceLoads,
  currentLoadId,
  currentLoadVersionId,
  documentArrived,
} from "./loads";
import { aiRefusal } from "./ai";
import { advanceSyncs, connectionKind, connectionName } from "./data";
import type { MockSourceDocument, MockStream } from "./data-sources";
import { isScope, ontologyOf, workspaceOntologies } from "./ontologies";
import { currentVersionOf, ontologyVersion } from "./ontology";
import { manages } from "./workspaces";

// The workspace's documents and document sets, as the backend's
// api/documents.go and api/document_sets.go serve them: create (with
// a presigned PUT), the PUT itself (the mock's raw /__uploads/ handler,
// standing in for S3 with CORS), :complete, list, read with the ontologies
// it is associated with, :content, :bytes, rename, :move and delete; and
// sets. What an ontology's current load read from one is its `extraction`,
// on the ontology's read-only routes (GET .../ontologies/{o}/documents...).
//
// - The formats of FORMATS, each by its extensions, 1 byte to 1 MiB of
//   text and 3 MiB for any other, at most 200 documents a workspace:
//   uploaded ones and uploads still within their 15 minutes (an abandoned
//   one asked for again counts again). :content serves the text types'
//   text, :bytes any type's bytes (base64).
// - How ontologies read each is its kind, said in every uploaded document's
//   `read`. Loads read Markdown, MDX (its code blanked, ../mdx.ts) and text.
//   Completing a PDF's or a table file's upload queues its read, reading
//   for `documentReadMs` (1.5 s; `POST /__mock/config`), then ready or
//   failed with why. A PDF's read is its text (../pdf-text.ts, its pages
//   joined by form feeds), which :content then serves and loads and
//   generations read as plain text; one with no text (scanned) fails, and
//   ./loads.ts records a failed one with its read's code, for good. A PDF
//   joins its set's readers' loads once its read is done (wakeReads). A
//   table file (CSV, TSV, JSON, JSONL, Parquet) joins no load and is never
//   extracted: its read gives its rows (../table-files.ts). A generation
//   drafts from the read ones of its sets (readTableFiles), as tables whose
//   source id is the document id, and a load ingests the ones its version
//   maps.
// - The same clientToken (even a deleted document's), else the same bytes
//   (by SHA-256) of a document that isn't deleted, the oldest, return the
//   document already there (`existing`), in the set it is in, with a fresh
//   upload URL while it waits for its bytes (earlier URLs keep working until
//   they expire), without one once they are in.
// - The PUT checks what S3 checks of a presigned PUT: the token, its expiry,
//   the signed Content-Type, the X-Amz-Checksum-Sha256 in the URL, the size,
//   and the bytes' digest; its CORS allows only the content-type header.
//   `POST /__mock/config {"documentUploadFailures": n}` makes the next n PUTs
//   fail as S3 does when it is busy (503).
// - Every document is in one set. Acme starts with "General"
//   (set_acme_general, the set Customer contracts' version 3 and Renewals'
//   version 1 read); a new workspace starts with none. Names are trimmed, 1
//   to 120 characters, unique in the workspace (case and accents aside; a
//   set's own name, byte for byte, is no rename); a workspace creates up to
//   20 sets (checked before the name), and a set holding a document isn't
//   deleted.
// - A version reads the sets of its scope (OntologyVersion.documentSetIds).
//   A load's documents are its members (`members`): the uploaded documents
//   of its version's scope when it was created, and those that joined since.
// - :complete, and :move of an uploaded document, fan out (./loads.ts
//   documentArrived): every live ontology whose live job's load or current
//   load reads the set reads it, and `reads` says how, per ontology, oldest
//   ontology first (the WFE locks them by created_at, id). With
//   `ontologyDatabaseHidden` the caller may write no database: each is
//   SKIPPED.
// - With MOCK_DOCUMENT_FIXTURES set (pnpm dev:mock sets it), Acme's General
//   holds a CSV, a JSON and a PDF document from the start, read, to show
//   their viewers, and a scanned PDF whose read failed; tests start with no
//   document.
// - A connector's syncs write documents too (writeConnectorDocuments, from
//   ./data.ts): a set per connector and stream (`connector`), named by the
//   stream, keeping the newest `documentsPerStream` (1000; `POST
//   /__mock/config`) by their time at the source (`source.at`), which orders
//   them. Those sets and documents are the connector's: uploading or moving
//   into such a set, renaming, describing or deleting it, and renaming,
//   moving or deleting such a document is FAILED_PRECONDITION, saying which
//   connection to change. They take no place among the 20 sets or their
//   names; the 200-document cap, the same-bytes answer of a create and the
//   workspace's list (no setId or ids) are uploads only. `ids` lists those
//   documents of either origin, deleted ones too. Deleting the connector
//   deletes its sets and their documents. An API source's set (Pieces,
//   ./event-sources.ts) keeps its newest 500 at every sync, and has an
//   allowance of $5 a month of work no person started (ALLOWANCES), which
//   the mock spends none of: `{"eventSetAllowanceSpent": true}` spends it
//   all, and pauses the set (`paused`) once it holds documents: syncs no
//   person started write it none, and jobs no person started hold its
//   documents back (./loads.ts).

/** How ontologies read a format (model.FoundryReadKind). */
type ReadKind = "text" | "table" | "pdf";
/** The most bytes a document holds, by how it is read (MaxBytesFor). */
export const MAX_BYTES: Record<ReadKind, number> = {
    text: 1_048_576,
    table: 3_145_728,
    pdf: 3_145_728,
  },
  MAX_DOCUMENTS = 200;
const PAGE = 50,
  // The most documents `ids` names.
  MAX_IDS = 100,
  UPLOAD_TTL_MS = 15 * 60_000;
/**
 * The formats the WFE stores (model.FoundryMediaType): the extensions each
 * is uploaded from, its name in messages, whether its bytes are text
 * (:content) and how ontologies read it.
 */
export const FORMATS: Record<
  DocumentMediaType,
  { extensions: RegExp; name: string; binary: boolean; kind: ReadKind }
> = {
  "text/markdown": {
    extensions: /\.(md|markdown)$/i,
    name: "Markdown",
    binary: false,
    kind: "text",
  },
  "text/mdx": {
    extensions: /\.mdx$/i,
    name: "MDX",
    binary: false,
    kind: "text",
  },
  "text/plain": {
    extensions: /\.txt$/i,
    name: "Plain text",
    binary: false,
    kind: "text",
  },
  "application/json": {
    extensions: /\.json$/i,
    name: "JSON",
    binary: false,
    kind: "table",
  },
  "text/csv": {
    extensions: /\.csv$/i,
    name: "CSV",
    binary: false,
    kind: "table",
  },
  "text/tab-separated-values": {
    extensions: /\.tsv$/i,
    name: "TSV",
    binary: false,
    kind: "table",
  },
  "application/x-ndjson": {
    extensions: /\.(jsonl|ndjson)$/i,
    name: "JSONL",
    binary: false,
    kind: "table",
  },
  "application/vnd.apache.parquet": {
    extensions: /\.parquet$/i,
    name: "Parquet",
    binary: true,
    kind: "table",
  },
  "application/pdf": {
    extensions: /\.pdf$/i,
    name: "PDF",
    binary: true,
    kind: "pdf",
  },
};
type Knobs = {
  documentUploadFailures?: number;
  // How long the document worker takes to read a table file.
  documentReadMs?: number;
  ontologyDatabaseHidden?: boolean;
  // Acme holds more sets, by these names, as merging ontologies' sets into
  // the workspace may have left it: over the 20 a create allows, or with a
  // name another set has.
  legacyDocumentSets?: string[];
  // The most documents a connector's sync keeps in each of its sets
  // (foundry.documents.max_per_stream).
  documentsPerStream?: number;
  // Every set with an allowance (an API source's) has spent it this month,
  // as if work no person started had, or the workspace's share for all its
  // API sources were spent: its syncs write and trim no documents.
  eventSetAllowanceSpent?: boolean;
};
const knobs = config as typeof config & Knobs;

type StoredDocument = {
  id: string;
  workspaceId: string;
  setId: string;
  name: string;
  mediaType: DocumentMediaType;
  sizeBytes: number;
  sha256: string;
  status: DocumentStatus;
  createdAt: string;
  // Each presigned PUT's secret and when it stops working: a repeat create
  // presigns another, and the earlier ones work until they expire.
  uploads: { token: string; expiresAt: number }[];
} & (
  | { clientToken: string; source?: undefined }
  // A connector's: the row of its table it is the text of, and no token.
  | { clientToken?: undefined; source: Required<FoundryDocumentSource> }
);
const documents = new Map<string, StoredDocument>();

type StoredSet = {
  id: string;
  workspaceId: string;
  name: string;
  description: string;
  createdAt: string;
  // The connector and stream whose documents it holds, and the stream's
  // keepNewest (MockStream); none for a set a person made.
  connector?: { connectorId: string; stream: string; keepNewest?: number };
};
// In creation order: oldest first.
const sets = new Map<string, StoredSet>();
export const MAX_SET_NAME = 120,
  MAX_SET_DESCRIPTION = 1000,
  MAX_SETS = 20,
  MAX_PER_STREAM = 1000;
export const ACME_GENERAL = "set_acme_general";
/**
 * µ$ per UTC month that work no person started may spend extracting a set
 * of a connector of each kind (FoundryConfig.SetAllowances): an API
 * source's $5 (foundry.events.max_system_usd_per_source_month). A kind not
 * here has no allowance.
 */
export const ALLOWANCES: Partial<Record<ConnectorKindName, number>> = {
  events: 5_000_000,
};

/**
 * A connector's set's allowance, what that work spent of it this month
 * (nothing in the mock, which counts no extraction against it, unless
 * `eventSetAllowanceSpent` says all of it), and whether the set is paused,
 * as the WFE's syncer decides it: it holds documents and its allowance is
 * spent. A set with none has nothing for the pause to keep.
 */
function allowanceOf(s: StoredSet) {
  const kind = s.connector && connectionKind(s.connector.connectorId),
    max = kind && ALLOWANCES[kind];
  if (max === undefined) return undefined;
  const spent = knobs.eventSetAllowanceSpent ? max : 0;
  return {
    max,
    spent,
    paused:
      spent >= max &&
      [...documents.values()].some((d) => d.setId === s.id && live(d)),
  };
}

/**
 * What work no person started spent this month on the workspace's sets of
 * connectors of `kind`, in µ$ (allowanceOf's, summed).
 */
export const systemSpend = (workspaceId: string, kind: ConnectorKindName) =>
  [...sets.values()]
    .filter(
      (s) =>
        s.workspaceId === workspaceId &&
        s.connector &&
        connectionKind(s.connector.connectorId) === kind,
    )
    .reduce((n, s) => n + (allowanceOf(s)?.spent || 0), 0);

/**
 * The API source whose month's allowance (allowanceOf) is spent for a set:
 * its name and its allowance (µ$), as the graph worker's allowanceSpent
 * names it when it holds the set's documents back from work no person
 * started; undefined for a set with room, or without an allowance.
 */
export function spentAllowance(setId: string) {
  const s = sets.get(setId),
    allowance = s && allowanceOf(s);
  if (!s?.connector || !allowance || allowance.spent < allowance.max)
    return undefined;
  return {
    source: connectionName(s.connector.connectorId) || "",
    max: allowance.max,
  };
}

// How many legacy sets Acme was given (`legacyDocumentSets`).
let legacy = 0;
/** A workspace's live sets, a connector's too, oldest first. */
export function documentSets(workspaceId: string): StoredSet[] {
  // Each legacy set is made once: one deleted stays deleted.
  const names = knobs.legacyDocumentSets || [];
  while (workspaceId === "ws_acme" && legacy < names.length) {
    legacy += 1;
    const id = `set_acme_legacy_${legacy}`;
    sets.set(id, {
      id,
      workspaceId,
      name: names[legacy - 1]!,
      description: "",
      createdAt: new Date(
        Date.parse("2026-09-28T09:00:00.000Z") + legacy * 1000,
      ).toISOString(),
    });
  }
  return [...sets.values()]
    .filter((s) => s.workspaceId === workspaceId)
    .sort(
      (a, b) =>
        a.createdAt.localeCompare(b.createdAt) || a.id.localeCompare(b.id),
    );
}

// The bytes of each uploaded document, by id.
const contents = new Map<string, Buffer>();
// Each table file's and PDF's read (foundry_document_reads), by document
// id: what it gave, once `at` (ms) has passed; reading until then.
const reads = new Map<string, { at: number; result: TableRead | PdfRead }>();
const READ_MS = 1500;
// PDFs whose read hadn't finished when their upload completed: once it has,
// their set's readers read them (wakeReads).
const waking = new Set<string>();
// What each load read from each document, by load and document id.
const extractions = new Map<string, DocumentExtraction>();
const readKey = (loadId: string, documentId: string) =>
  `${loadId}\u001f${documentId}`;
// Each load's documents, with the set each joined with: a load reads a
// document by the routes of that set.
const members = new Map<string, Map<string, string>>();

const sha256 = (b: Buffer) => createHash("sha256").update(b).digest("hex");
const base64Digest = (hex: string) =>
  Buffer.from(hex, "hex").toString("base64");
const live = (d: StoredDocument) => d.status !== "deleted";
// Where a list puts a document, newest first: a connector's by its time at
// the source.
const listed = (d: StoredDocument) => d.source?.at || d.createdAt;

/**
 * The WFE's refusal of a person's change to a connector's set or document:
 * which connection to change instead, or "a connection" once it is gone.
 */
function connectorOwned(setId: string, what: "set" | "document") {
  const owner = sets.get(setId)?.connector,
    name = owner && connectionName(owner.connectorId);
  return fail(
    "FAILED_PRECONDITION",
    `this ${what} belongs to ${name ? `the ${name} connection` : "a connection"}; change its settings there`,
  );
}

/** A set name as the workspace's collation compares it: case and accents aside. */
const folded = (name: string) =>
  name.normalize("NFD").replace(/\p{M}/gu, "").toLowerCase();

/**
 * The version that reads an ontology's new uploads (the current load's,
 * else the current version) with its scope and mappings; none before either.
 */
function reader(ontologyId: string) {
  const versionId =
    currentLoadVersionId(ontologyId) || currentVersionOf(ontologyId) || "";
  const v = ontologyVersion(ontologyId, versionId);
  return {
    scope: v?.documentSetIds || [],
    mappings: String(v?.definition.mappings_toml || ""),
  };
}

// ---------------------------------------------------------------- For ./loads.ts

/**
 * Adds to a load the uploaded documents of the workspace in `scope` it
 * doesn't hold yet, each with the set it is in: what the load reads; or
 * only `documentId` of them, as a document's arrival joins it alone. Table
 * files never join: loads read them as tables, through mappings.
 */
export function joinLoad(
  loadId: string,
  workspaceId: string,
  scope: readonly string[],
  documentId?: string,
) {
  const held = members.get(loadId) || new Map<string, string>();
  for (const d of documentId ? [documents.get(documentId)] : documents.values())
    if (
      d &&
      d.workspaceId === workspaceId &&
      d.status === "uploaded" &&
      FORMATS[d.mediaType].kind !== "table" &&
      scope.includes(d.setId) &&
      !held.has(d.id)
    )
      held.set(d.id, d.setId);
  members.set(loadId, held);
}

/** What reading a table file or a PDF has come to now. */
function readOf(d: StoredDocument) {
  const r = reads.get(d.id);
  return !r || r.at > Date.now() ? undefined : r.result;
}
/** A PDF's text, once its read succeeded. */
function pdfText(d: StoredDocument) {
  const r = FORMATS[d.mediaType].kind === "pdf" ? readOf(d) : undefined;
  return r?.ok && "text" in r ? r.text : undefined;
}

/**
 * The workspace's uploaded table files in `setIds` whose read succeeded,
 * oldest first, with their rows: those a generation drafts from.
 */
export function readTableFiles(
  workspaceId: string,
  setIds: readonly string[],
): { id: string; setId: string; name: string; rows: Row[] }[] {
  return [...documents.values()]
    .filter(
      (d) =>
        d.workspaceId === workspaceId &&
        d.status === "uploaded" &&
        setIds.includes(d.setId),
    )
    .sort(
      (a, b) =>
        a.createdAt.localeCompare(b.createdAt) || a.id.localeCompare(b.id),
    )
    .flatMap((d) => {
      const r = readOf(d);
      return r?.ok && "rows" in r
        ? [{ id: d.id, setId: d.setId, name: d.name, rows: r.rows }]
        : [];
    });
}

/**
 * How many uploaded text documents `setIds` hold: Markdown, MDX and text,
 * and PDFs whose text is read.
 */
export const textDocumentCount = (
  workspaceId: string,
  setIds: readonly string[],
) =>
  [...documents.values()].filter(
    (d) =>
      d.workspaceId === workspaceId &&
      d.status === "uploaded" &&
      (FORMATS[d.mediaType].kind === "text" || pdfText(d) !== undefined) &&
      setIds.includes(d.setId),
  ).length;

/** A set's name, or "" for none of the workspace's. */
export const setName = (setId: string) => sets.get(setId)?.name || "";

/**
 * A load's uploaded documents with their text, the set each joined with and
 * what the load read from each, oldest first. A PDF whose read hasn't
 * finished is none of them yet: neither read nor waiting to be.
 */
export function loadDocuments(loadId: string) {
  const held = members.get(loadId) || new Map<string, string>();
  return [...held.entries()]
    .flatMap(([documentId, setId]) => {
      const d = documents.get(documentId);
      return d &&
        d.status === "uploaded" &&
        (FORMATS[d.mediaType].kind !== "pdf" || readOf(d))
        ? [{ d, setId }]
        : [];
    })
    .sort((a, b) => a.d.createdAt.localeCompare(b.d.createdAt))
    .map(({ d, setId }) => {
      const r = FORMATS[d.mediaType].kind === "pdf" ? readOf(d) : undefined;
      return {
        id: d.id,
        name: d.name,
        setId,
        // MDX is read as Markdown, its code blanked (../mdx.ts); a PDF's
        // text as plain text.
        markdown: d.mediaType === "text/markdown" || d.mediaType === "text/mdx",
        // A PDF whose text couldn't be read: its read's code.
        refused: r && !r.ok ? r.code : undefined,
        text:
          d.mediaType === "text/mdx"
            ? blankMdx(contents.get(d.id)?.toString("utf8") || "")
            : r
              ? pdfText(d) || ""
              : contents.get(d.id)?.toString("utf8") || "",
        // What the load read from it, if it did.
        extraction: extractions.get(readKey(loadId, d.id)),
      };
    });
}
/** Records what a load read from a document. */
export const setExtraction = (
  loadId: string,
  documentId: string,
  x: DocumentExtraction,
) => extractions.set(readKey(loadId, documentId), x);
/** Forgets what a deleted load held and read. */
export function dropExtractions(loadId: string) {
  members.delete(loadId);
  for (const key of [...extractions.keys()])
    if (key.startsWith(`${loadId}\u001f`)) extractions.delete(key);
}

/** For ./ontologies.ts: the uploaded documents its reading version's scope holds. */
export function documentCount(workspaceId: string, ontologyId: string) {
  const { scope } = reader(ontologyId);
  return [...documents.values()].filter(
    (d) =>
      d.workspaceId === workspaceId &&
      d.status === "uploaded" &&
      scope.includes(d.setId),
  ).length;
}

// ---------------------------------------------------------------- Shapes

function toSet(s: StoredSet): FoundryDocumentSet {
  const allowance = allowanceOf(s);
  return {
    id: s.id,
    name: s.name,
    description: s.description,
    documents: [...documents.values()].filter(
      (d) => d.setId === s.id && live(d),
    ).length,
    createdAt: s.createdAt,
    connector: s.connector
      ? {
          connectorId: s.connector.connectorId,
          stream: s.connector.stream,
          maxDocuments:
            s.connector.keepNewest ??
            knobs.documentsPerStream ??
            MAX_PER_STREAM,
          connectorName: connectionName(s.connector.connectorId) || "",
          connectorKind: connectionKind(s.connector.connectorId) || "",
          monthSystemUsdMicros: String(allowance?.spent || 0),
          // Set only for a set with an allowance.
          ...(allowance
            ? { maxMonthSystemUsdMicros: String(allowance.max) }
            : {}),
          paused: !!allowance?.paused,
        }
      : null,
  };
}

const KIND: Record<ReadKind, FoundryReadKind> = {
  text: "FOUNDRY_READ_KIND_TEXT",
  table: "FOUNDRY_READ_KIND_TABLE",
  pdf: "FOUNDRY_READ_KIND_PDF",
};

/**
 * How ontologies read an uploaded document, as foundryFileReadProto says
 * it: text is ready, a table file or a PDF is as its read is (a table
 * file's rows, a PDF's pages). None before its upload completes.
 */
function readState(d: StoredDocument): FoundryFileRead | null {
  if (d.status !== "uploaded") return null;
  const kind = FORMATS[d.mediaType].kind;
  if (kind === "text")
    return { kind: KIND.text, state: "FOUNDRY_FILE_READ_STATE_READY" };
  const base = {
    kind: KIND[kind],
    mode: "FOUNDRY_FILE_READ_MODE_STANDARD",
    error: null,
    retrying: false,
  } as const;
  const r = readOf(d);
  return !r
    ? { ...base, state: "FOUNDRY_FILE_READ_STATE_READING" }
    : r.ok
      ? {
          ...base,
          state: "FOUNDRY_FILE_READ_STATE_READY",
          ...("rows" in r
            ? { rows: String(r.rows.length) }
            : { pages: r.pages }),
        }
      : {
          ...base,
          state: "FOUNDRY_FILE_READ_STATE_FAILED",
          error: { code: r.code, message: r.message },
        };
}

function toDocument(
  d: StoredDocument,
  extraction?: DocumentExtraction,
): FoundryDocument {
  return {
    id: d.id,
    setId: d.setId,
    name: d.name,
    mediaType: d.mediaType,
    sizeBytes: String(d.sizeBytes),
    sha256: d.sha256,
    status: d.status,
    createdAt: d.createdAt,
    extraction: extraction || null,
    read: readState(d),
    source: d.source || null,
  };
}
const currentExtraction = (ontologyId: string, documentId: string) =>
  extractions.get(readKey(currentLoadId(ontologyId) || "", documentId));

function upload(d: StoredDocument) {
  const put = {
    token: randomBytes(12).toString("hex"),
    expiresAt: Date.now() + UPLOAD_TTL_MS,
  };
  d.uploads.push(put);
  const origin = process.env.MOCK_ORIGIN || "http://127.0.0.1:4010";
  return {
    // As S3 presigns it: the checksum in the query, Content-Type the only
    // header to send (the browser sets the length).
    url: `${origin}/__uploads/${d.id}?token=${put.token}&X-Amz-Checksum-Sha256=${encodeURIComponent(base64Digest(d.sha256))}`,
    method: "PUT",
    headers: { "Content-Type": d.mediaType },
    expiresAt: new Date(put.expiresAt).toISOString(),
  };
}
/**
 * What the workspace's document cap counts: uploaded documents, and uploads
 * still within their time. An older pending upload was abandoned. A
 * connector's documents take no place.
 */
const capped = (workspaceId: string) =>
  [...documents.values()].filter(
    (d) =>
      d.workspaceId === workspaceId &&
      !d.source &&
      (d.status === "uploaded" ||
        (d.status === "pending_upload" &&
          Date.parse(d.createdAt) >= Date.now() - UPLOAD_TTL_MS)),
  ).length;
const full = () =>
  fail(
    "FAILED_PRECONDITION",
    `a workspace holds up to ${MAX_DOCUMENTS} documents; delete one first`,
  );

/**
 * Brings the workspace's ontologies' jobs up to now before a document
 * changes, so none reads it at a time before it did (the mock runs jobs
 * when they are next looked at).
 */
const settle = (workspaceId: string) =>
  workspaceOntologies(workspaceId).forEach((o) => advanceLoads(o.id));

/**
 * Every live ontology of the workspace that reads the document's set, in
 * the order the WFE locks them (by id), and what it did. A table file joins
 * no load: none reads it so. A PDF whose read hasn't finished joins them
 * (READING), and they read it once it has (wakeReads).
 */
function fanOut(d: StoredDocument): DocumentRead[] {
  const kind = FORMATS[d.mediaType].kind;
  if (kind === "table") return [];
  const reading = kind === "pdf" && !readOf(d);
  return workspaceOntologies(d.workspaceId)
    .sort(
      (a, b) =>
        a.createdAt.localeCompare(b.createdAt) || a.id.localeCompare(b.id),
    )
    .flatMap((o) => {
      const outcome = documentArrived(
        o.id,
        d.id,
        d.setId,
        !knobs.ontologyDatabaseHidden,
        reading,
      );
      return outcome ? [{ ontologyId: o.id, outcome }] : [];
    });
}

/**
 * Has every ontology that reads a PDF's set read it once its read is done,
 * as the WFE's document worker wakes the loads that hold it when it
 * finishes. The mock runs jobs when they are next looked at, so
 * ./loads.ts's advanceLoads wakes them first.
 */
export function wakeReads() {
  for (const id of [...waking]) {
    const d = documents.get(id);
    // Woken already, by a call within this one, or still being read.
    if (!waking.has(id) || (d?.status === "uploaded" && !readOf(d))) continue;
    waking.delete(id);
    if (d?.status === "uploaded") fanOut(d);
  }
}

// ---------------------------------------------------------------- For ./data.ts

/**
 * A source document's title as its name (the syncer's documentName): each
 * slash or backslash a "-", each control character a space, trimmed (Go's
 * White_Space), at most 255 characters; its id at the source, cleaned the
 * same way, when nothing is left.
 */
function documentName(title: string, externalId: string) {
  const trim = (s: string) =>
    s.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, "");
  const clean = (s: string) => {
    const runes = [...trim(s.replace(/[/\\]/g, "-").replace(/\p{Cc}/gu, " "))];
    return runes.length > 255
      ? trim(runes.slice(0, 255).join(""))
      : runes.join("");
  };
  return clean(title) || clean(externalId);
}

/**
 * Writes what a connector's sync read into its sets, as the WFE's syncer
 * publishes a full read. Each stream has a set of its own, made at its
 * first sync with the stream's title and description, which keeps the
 * first `documentsPerStream` documents offered (the newest; an id offered
 * again counts once), or the stream's keepNewest. One held at the same
 * version is kept, and in a keepNewest stream takes the offered time, so a
 * piece sent again as it was moves up in place; a new version with the same
 * text changes it in place; new text is a new document that replaces it;
 * one no longer kept is removed (deleted). A stream turned off is emptied,
 * and its set kept. A paused set (allowanceOf) takes nothing from a sync no
 * person started (`byPerson`): it adds, changes and removes nothing, and
 * the next one with room, or a person's, catches up, since every sync
 * offers the newest. Every ontology whose live or current load reads the
 * set reads each document added, with the trust a refresh has, as no one's
 * work; one of a set with an allowance never joins a load a person's job is
 * writing, and waits for the extract its end owes.
 */
export function writeConnectorDocuments(
  workspaceId: string,
  connectorId: string,
  streams: readonly {
    stream: MockStream;
    documents: readonly MockSourceDocument[] | null;
  }[],
  byPerson = false,
) {
  settle(workspaceId);
  for (const { stream, documents: offered } of streams) {
    const max = stream.keepNewest ?? knobs.documentsPerStream ?? MAX_PER_STREAM;
    let set = [...sets.values()].find(
      (s) =>
        s.connector?.connectorId === connectorId &&
        s.connector.stream === stream.name,
    );
    if (!set && !offered) continue;
    if (!set) {
      set = {
        id: id("set"),
        workspaceId,
        name: stream.title,
        description: stream.description,
        createdAt: now(),
        connector: {
          connectorId,
          stream: stream.name,
          ...(stream.keepNewest ? { keepNewest: stream.keepNewest } : {}),
        },
      };
      sets.set(set.id, set);
    }
    // Paused: a stream turned off still empties, which spends nothing.
    const allowance = allowanceOf(set);
    if (offered && !byPerson && allowance?.paused) continue;
    const setId = set.id;
    const held = new Map(
      [...documents.values()].flatMap((d) =>
        d.setId === setId && d.status === "uploaded" && d.source
          ? [[d.source.externalId, d] as const]
          : [],
      ),
    );
    const kept = new Set<string>(),
      added: StoredDocument[] = [];
    for (const doc of offered || []) {
      if (kept.has(doc.externalId) || kept.size >= max) continue;
      kept.add(doc.externalId);
      const was = held.get(doc.externalId);
      if (was?.source?.version === doc.version) {
        if (stream.keepNewest) was.source.at = doc.at;
        continue;
      }
      const bytes = Buffer.from(doc.text),
        source = {
          externalId: doc.externalId,
          version: doc.version,
          url: doc.url.startsWith("https://") ? doc.url : "",
          at: doc.at,
        },
        name = documentName(doc.title, doc.externalId);
      if (was?.sha256 === sha256(bytes)) {
        Object.assign(was, { name, source });
        continue;
      }
      if (was) {
        was.status = "deleted";
        contents.delete(was.id);
      }
      const d: StoredDocument = {
        id: id("doc"),
        workspaceId,
        setId,
        name,
        mediaType: stream.mediaType,
        sizeBytes: bytes.length,
        sha256: sha256(bytes),
        status: "uploaded",
        createdAt: now(),
        uploads: [],
        source,
      };
      documents.set(d.id, d);
      contents.set(d.id, bytes);
      added.push(d);
    }
    for (const [externalId, d] of held)
      if (!kept.has(externalId)) {
        d.status = "deleted";
        contents.delete(d.id);
      }
    const readers = workspaceOntologies(workspaceId).sort(
      (a, b) =>
        a.createdAt.localeCompare(b.createdAt) || a.id.localeCompare(b.id),
    );
    // No one's work, whoever started the sync: a set with an allowance's
    // never joins a person's load.
    const by = allowance ? "allowance" : "sync";
    for (const d of added)
      for (const o of readers)
        documentArrived(o.id, d.id, setId, true, false, by);
  }
}

/** A connector is deleted: its sets go, and their documents with them. */
export function dropConnectorDocuments(connectorId: string) {
  for (const s of [...sets.values()])
    if (s.connector?.connectorId === connectorId) {
      for (const d of documents.values())
        if (d.setId === s.id && live(d)) {
          d.status = "deleted";
          contents.delete(d.id);
        }
      sets.delete(s.id);
    }
}

// ---------------------------------------------------------------- Access

// Reading needs membership; writing needs owner or admin
// (workspace.connectors.manage).
function caller(req: MockRequest, manage: boolean): Membership | MockReply {
  const m = membership(req.params.workspaceId!, req.principal.user.id);
  if (isReply(m)) return m;
  return manage && !manages(m)
    ? fail("PERMISSION_DENIED", "permission denied")
    : m;
}
/** A document of the caller's workspace, deleted or not. */
function find(req: MockRequest, manage: boolean): StoredDocument | MockReply {
  const m = caller(req, manage);
  if (isReply(m)) return m;
  const d = documents.get(req.params.id!);
  return d && d.workspaceId === m.workspaceId
    ? d
    : fail("NOT_FOUND", "document not found");
}
const isDocument = (x: StoredDocument | MockReply): x is StoredDocument =>
  "sha256" in x;
const deleted = () => fail("FAILED_PRECONDITION", "the document is deleted");

/** A document name as the WFE checks it, once trimmed or as sent. */
const validName = (name: string) =>
  !!name &&
  [...name].length <= 255 &&
  name.trim() === name &&
  // eslint-disable-next-line no-control-regex
  !/[/\\\u0000-\u001f\u007f-\u009f]/.test(name);
const badName = () =>
  fail(
    "INVALID_ARGUMENT",
    "name must be 1 to 255 characters with no slash, backslash or control character",
  );

function created(
  body: unknown,
  workspaceId: string,
):
  | (Pick<
      StoredDocument,
      "workspaceId" | "setId" | "name" | "mediaType" | "sizeBytes" | "sha256"
    > & { clientToken: string })
  | MockReply {
  const b = (body || {}) as Record<string, unknown>;
  const name = String(b.name ?? ""),
    mediaType = b.mediaType as DocumentMediaType,
    size = Number(b.sizeBytes),
    sha = String(b.sha256 ?? ""),
    clientToken = String(b.clientToken ?? ""),
    setId = String(b.setId ?? "");
  if (!validName(name)) return badName();
  const format = Object.hasOwn(FORMATS, mediaType)
    ? FORMATS[mediaType]
    : undefined;
  if (!format || !format.extensions.test(name))
    return fail(
      "INVALID_ARGUMENT",
      `Foundry takes Markdown (.md, .markdown), MDX (.mdx), plain text (.txt), JSON (.json), CSV (.csv), TSV (.tsv), JSONL (.jsonl, .ndjson), Parquet (.parquet) and PDF (.pdf) documents; "${name}" is none of them`,
    );
  const max = MAX_BYTES[format.kind];
  if (!Number.isInteger(size) || size < 1 || size > max)
    return fail(
      "INVALID_ARGUMENT",
      `a ${format.name} document holds 1 byte to ${max >> 10} KiB; this one is ${size} bytes`,
    );
  if (!/^[0-9a-f]{64}$/.test(sha))
    return fail(
      "INVALID_ARGUMENT",
      "sha256 must be the lowercase hex SHA-256 of the bytes",
    );
  if (!clientToken || clientToken.length > 128)
    return fail(
      "INVALID_ARGUMENT",
      "client_token is required, at most 128 characters",
    );
  if (!setId)
    return fail(
      "INVALID_ARGUMENT",
      "set_id is required: the document set to upload into",
    );
  return {
    workspaceId,
    setId,
    name,
    mediaType,
    sizeBytes: size,
    sha256: sha,
    clientToken,
  };
}

// ---------------------------------------------------------------- Sets

/**
 * A set's name and description as the WFE takes them: trimmed, the name 1
 * to 120 characters and the description at most 1000; each only when sent.
 */
function setFields(
  b: UpdateDocumentSetRequest,
): { name?: string; description?: string } | MockReply {
  const name = b.name === undefined ? undefined : String(b.name).trim(),
    description =
      b.description === undefined ? undefined : String(b.description).trim();
  if (name !== undefined && (!name || [...name].length > MAX_SET_NAME))
    return fail(
      "INVALID_ARGUMENT",
      `name must be 1 to ${MAX_SET_NAME} characters`,
    );
  if (
    description !== undefined &&
    [...description].length > MAX_SET_DESCRIPTION
  )
    return fail(
      "INVALID_ARGUMENT",
      `description must be at most ${MAX_SET_DESCRIPTION} characters`,
    );
  return { name, description };
}
const nameTaken = (name: string) =>
  fail(
    "ALREADY_EXISTS",
    `the workspace already has a document set named "${name}"`,
  );
// A connector's sets take no name.
const taken = (workspaceId: string, name: string, self = "") =>
  documentSets(workspaceId).some(
    (s) => !s.connector && s.id !== self && folded(s.name) === folded(name),
  );

const WS = "/v1/workspaces/{workspaceId}/foundry" as const;
const ONTOLOGY = `${WS}/ontologies/{ontologyId}` as const;
const STATUSES: readonly OntologyDocumentStatus[] = [
  "waiting",
  "extracted",
  "review",
  "duplicate",
  "failed",
  "out_of_scope",
];

/** A page of a newest-first list, by `createdAt` (see `listed`). */
function page<T extends { createdAt: string; id: string }>(
  all: T[],
  token: string | null,
) {
  const sorted = [...all].sort(
    (a, b) =>
      b.createdAt.localeCompare(a.createdAt) || b.id.localeCompare(a.id),
  );
  const from = Number(token || 0) || 0;
  return {
    items: sorted.slice(from, from + PAGE),
    nextPageToken: from + PAGE < sorted.length ? String(from + PAGE) : "",
  };
}

export const documentRoutes: MockRoute[] = [
  {
    method: "POST",
    path: `${WS}/documents`,
    handle: (req) => {
      const m = caller(req, true);
      if (isReply(m)) return m;
      const want = created(req.body, m.workspaceId);
      if ("status" in want) return want;
      // An upload is no repeat of a connector's document.
      const mine = [...documents.values()]
        .filter((d) => d.workspaceId === m.workspaceId && !d.source)
        .sort(
          (a, b) =>
            a.createdAt.localeCompare(b.createdAt) || a.id.localeCompare(b.id),
        );
      const again =
        mine.find((d) => d.clientToken === want.clientToken) ||
        mine.find((d) => live(d) && d.sha256 === want.sha256);
      if (again) {
        if (
          again.clientToken === want.clientToken &&
          again.sha256 !== want.sha256
        )
          return fail(
            "INVALID_ARGUMENT",
            "client_token was already used for other bytes",
          );
        // An abandoned upload asked for again takes a place again.
        if (
          again.status === "pending_upload" &&
          Date.parse(again.createdAt) < Date.now() - UPLOAD_TTL_MS &&
          capped(m.workspaceId) >= MAX_DOCUMENTS
        )
          return full();
        return ok({
          document: toDocument(again),
          upload: again.status === "pending_upload" ? upload(again) : null,
          existing: true,
        } satisfies CreateDocumentResponse);
      }
      const into = documentSets(m.workspaceId).find((s) => s.id === want.setId);
      if (!into) return fail("NOT_FOUND", "document set not found");
      if (into.connector) return connectorOwned(into.id, "set");
      if (capped(m.workspaceId) >= MAX_DOCUMENTS) return full();
      const d: StoredDocument = {
        ...want,
        id: id("doc"),
        status: "pending_upload",
        createdAt: now(),
        uploads: [],
      };
      documents.set(d.id, d);
      return ok({
        document: toDocument(d),
        upload: upload(d),
        existing: false,
      } satisfies CreateDocumentResponse);
    },
  },
  {
    method: "POST",
    path: `${WS}/documents/{id}:complete`,
    handle: async (req) => {
      const d = find(req, true);
      if (!isDocument(d)) return d;
      if (d.status === "deleted") return deleted();
      // Every completed upload may be read by a model: none without one.
      const unset = aiRefusal(d.workspaceId);
      if (unset) return unset;
      // Completing twice is no error, and reads nothing again.
      if (d.status === "uploaded")
        return ok({ document: toDocument(d) } satisfies DocumentReadsResponse);
      const bytes = contents.get(d.id);
      if (!bytes || sha256(bytes) !== d.sha256)
        return fail(
          "FAILED_PRECONDITION",
          "upload the document's bytes to its URL first",
        );
      // A table file's or a PDF's read is queued with it: the worker reads
      // it within `documentReadMs`.
      const kind = FORMATS[d.mediaType].kind;
      if (kind !== "text")
        reads.set(d.id, {
          at: Date.now() + (knobs.documentReadMs ?? READ_MS),
          result:
            kind === "pdf"
              ? await readPdfText(bytes)
              : await readTableFile(d.mediaType, bytes),
        });
      settle(d.workspaceId);
      d.status = "uploaded";
      const answer = fanOut(d);
      // Its set's readers read a PDF once its read is done.
      if (kind === "pdf" && !readOf(d)) waking.add(d.id);
      return ok({
        document: toDocument(d),
        reads: answer,
      } satisfies DocumentReadsResponse);
    },
  },
  {
    method: "GET",
    path: `${WS}/documents`,
    handle: (req) => {
      const m = caller(req, false);
      if (isReply(m)) return m;
      advanceSyncs(m.workspaceId);
      const setId = req.query.get("setId") || "",
        token = req.query.get("pageToken"),
        ids = req.query.getAll("ids");
      // These documents only, as asked, each once, deleted ones too.
      if (ids.length) {
        if (ids.length > MAX_IDS)
          return fail(
            "INVALID_ARGUMENT",
            `ids may name at most ${MAX_IDS} documents`,
          );
        if (setId || token)
          return fail(
            "INVALID_ARGUMENT",
            "ids takes no set_id or page_token: it lists those documents only",
          );
        return ok({
          documents: [...new Set(ids)].flatMap((id) => {
            const d = documents.get(id);
            return d && d.workspaceId === m.workspaceId ? [toDocument(d)] : [];
          }),
          nextPageToken: "",
        } satisfies ListDocumentsResponse);
      }
      // Without a set: the workspace's uploads.
      const { items, nextPageToken } = page(
        [...documents.values()]
          .filter(
            (d) =>
              d.workspaceId === m.workspaceId &&
              live(d) &&
              (setId ? d.setId === setId : !d.source),
          )
          .map((d) => ({ d, id: d.id, createdAt: listed(d) })),
        token,
      );
      return ok({
        documents: items.map((r) => toDocument(r.d)),
        nextPageToken,
      } satisfies ListDocumentsResponse);
    },
  },
  {
    method: "GET",
    path: `${WS}/documents/{id}`,
    handle: (req) => {
      const d = find(req, false);
      if (!isDocument(d)) return d;
      // Each live ontology, newest first, whose current load holds it or
      // whose reading version has its set in scope.
      const ontologies = workspaceOntologies(d.workspaceId).flatMap(
        (o): DocumentOntology[] => {
          advanceLoads(o.id);
          const holds = !!members.get(currentLoadId(o.id) || "")?.has(d.id);
          const r = reader(o.id);
          if (!holds && !r.scope.includes(d.setId)) return [];
          return [
            {
              ontologyId: o.id,
              routed: isRouted(r.mappings, d.setId),
              extraction: currentExtraction(o.id, d.id) || null,
            },
          ];
        },
      );
      return ok({
        document: { ...toDocument(d), ontologies },
      } satisfies DocumentResponse);
    },
  },
  {
    method: "GET",
    path: `${WS}/documents/{id}:content`,
    handle: (req) => {
      const d = find(req, false);
      if (!isDocument(d)) return d;
      const pdf = FORMATS[d.mediaType].kind === "pdf";
      if (FORMATS[d.mediaType].binary && !pdf)
        return fail(
          "FAILED_PRECONDITION",
          `${FORMATS[d.mediaType].name} documents have no text; use :bytes`,
        );
      if (d.status !== "uploaded")
        return fail(
          "FAILED_PRECONDITION",
          `the document is ${d.status}; only an uploaded document can be read`,
        );
      // A PDF's text, once Foundry has read it.
      if (pdf) {
        const r = readOf(d);
        if (r?.ok && "text" in r)
          return ok({ text: r.text } satisfies DocumentContentResponse);
        return fail(
          "FAILED_PRECONDITION",
          r && !r.ok
            ? `the PDF's text can't be read: ${r.message}`
            : "the PDF's text is still being read; try again shortly",
        );
      }
      const bytes = contents.get(d.id);
      if (!bytes)
        return fail("FAILED_PRECONDITION", "the document's original is gone");
      return ok({
        text: bytes.toString("utf8"),
      } satisfies DocumentContentResponse);
    },
  },
  {
    method: "GET",
    path: `${WS}/documents/{id}:bytes`,
    handle: (req) => {
      const d = find(req, false);
      if (!isDocument(d)) return d;
      if (d.status !== "uploaded")
        return fail(
          "FAILED_PRECONDITION",
          `the document is ${d.status}; only an uploaded document can be read`,
        );
      const bytes = contents.get(d.id);
      if (!bytes)
        return fail("FAILED_PRECONDITION", "the document's original is gone");
      return ok({
        mediaType: d.mediaType,
        data: bytes.toString("base64"),
      } satisfies DocumentBytesResponse);
    },
  },
  {
    method: "PATCH",
    path: `${WS}/documents/{id}`,
    handle: (req) => {
      const name = String(
        (req.body as Partial<UpdateDocumentRequest>)?.name ?? "",
      ).trim();
      if (!validName(name)) return badName();
      const d = find(req, true);
      if (!isDocument(d)) return d;
      if (d.source) return connectorOwned(d.setId, "document");
      if (d.status === "deleted") return deleted();
      d.name = name;
      return ok({ document: toDocument(d) } satisfies DocumentResponse);
    },
  },
  {
    method: "DELETE",
    path: `${WS}/documents/{id}`,
    handle: (req) => {
      const d = find(req, true);
      if (!isDocument(d)) return d;
      if (d.source) return connectorOwned(d.setId, "document");
      // What loads stored from it stays in their databases; its original,
      // and what reading it gave, go.
      d.status = "deleted";
      contents.delete(d.id);
      reads.delete(d.id);
      waking.delete(d.id);
      return ok({ document: toDocument(d) } satisfies DocumentResponse);
    },
  },
  {
    method: "POST",
    path: `${WS}/documents/{id}:move`,
    handle: (req) => {
      const d = find(req, true);
      if (!isDocument(d)) return d;
      if (d.source) return connectorOwned(d.setId, "document");
      if (d.status === "deleted") return deleted();
      const setId = String(
        (req.body as Partial<MoveDocumentRequest>)?.setId ?? "",
      );
      if (!setId)
        return fail(
          "INVALID_ARGUMENT",
          "set_id is required: the document set to move into",
        );
      const into = documentSets(d.workspaceId).find((s) => s.id === setId);
      if (!into) return fail("NOT_FOUND", "document set not found");
      if (into.connector) return connectorOwned(into.id, "set");
      // A load keeps what it read, in the set it read it in; an uploaded
      // document is then read as an upload into the set is.
      settle(d.workspaceId);
      d.setId = setId;
      return ok({
        document: toDocument(d),
        reads: d.status === "uploaded" ? fanOut(d) : [],
      } satisfies DocumentReadsResponse);
    },
  },
  {
    method: "GET",
    path: `${ONTOLOGY}/documents`,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      const status = req.query.get("status") || "";
      if (status && !STATUSES.includes(status as OntologyDocumentStatus))
        return fail(
          "INVALID_ARGUMENT",
          `status "${status}" is none of waiting, extracted, review, duplicate, failed or out_of_scope`,
        );
      advanceLoads(s.o.id);
      const held = members.get(currentLoadId(s.o.id) || ""),
        { scope } = reader(s.o.id);
      const read = [...documents.values()].flatMap((d) => {
        if (
          d.workspaceId !== s.o.workspaceId ||
          !live(d) ||
          !(
            held?.has(d.id) ||
            (d.status === "uploaded" && scope.includes(d.setId))
          )
        )
          return [];
        const x = currentExtraction(s.o.id, d.id);
        const keep =
          !status ||
          (status === "waiting"
            ? // A table file is never extracted, so it never waits.
              d.status === "uploaded" &&
              !x &&
              FORMATS[d.mediaType].kind !== "table"
            : status === "review"
              ? x?.status === "extracted" && (x.reviews || 0) > 0
              : x?.status === status);
        return keep ? [{ d, x }] : [];
      });
      const { items, nextPageToken } = page(
        read.map((r) => ({ ...r, id: r.d.id, createdAt: listed(r.d) })),
        req.query.get("pageToken"),
      );
      return ok({
        documents: items.map((r) => toDocument(r.d, r.x)),
        nextPageToken,
      } satisfies ListDocumentsResponse);
    },
  },
  {
    method: "GET",
    path: `${ONTOLOGY}/documents/{id}`,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      advanceLoads(s.o.id);
      // Any document of the workspace, so a citation of one that moved out
      // of scope still opens.
      const d = documents.get(req.params.id!);
      return d && d.workspaceId === s.o.workspaceId
        ? ok({
            document: toDocument(d, currentExtraction(s.o.id, d.id)),
          } satisfies DocumentResponse)
        : fail("NOT_FOUND", "document not found");
    },
  },
  {
    method: "GET",
    path: `${WS}/document-sets`,
    handle: (req) => {
      const m = caller(req, false);
      if (isReply(m)) return m;
      advanceSyncs(m.workspaceId);
      const readers = workspaceOntologies(m.workspaceId).map((o) => {
        advanceLoads(o.id);
        return { id: o.id, ...reader(o.id) };
      });
      return ok({
        sets: documentSets(m.workspaceId).map((s) => ({
          ...toSet(s),
          readBy: readers
            .filter((r) => r.scope.includes(s.id))
            .map((r) => ({
              ontologyId: r.id,
              routed: isRouted(r.mappings, s.id),
            })),
        })),
      } satisfies ListDocumentSetsResponse);
    },
  },
  {
    method: "POST",
    path: `${WS}/document-sets`,
    handle: (req) => {
      const m = caller(req, true);
      if (isReply(m)) return m;
      const f = setFields({
        name: "",
        ...((req.body || {}) as Partial<CreateDocumentSetRequest>),
      });
      if ("status" in f) return f;
      const name = f.name!;
      if (
        documentSets(m.workspaceId).filter((s) => !s.connector).length >=
        MAX_SETS
      )
        return fail(
          "FAILED_PRECONDITION",
          `a workspace holds up to ${MAX_SETS} document sets; delete an empty one first`,
        );
      if (taken(m.workspaceId, name)) return nameTaken(name);
      const s: StoredSet = {
        id: id("set"),
        workspaceId: m.workspaceId,
        name,
        description: f.description || "",
        createdAt: now(),
      };
      sets.set(s.id, s);
      return ok({ set: toSet(s) } satisfies DocumentSetResponse);
    },
  },
  {
    method: "PATCH",
    path: `${WS}/document-sets/{setId}`,
    handle: (req) => {
      const m = caller(req, true);
      if (isReply(m)) return m;
      // Each field sent is set; one left out stays.
      const f = setFields((req.body || {}) as UpdateDocumentSetRequest);
      if ("status" in f) return f;
      const s = documentSets(m.workspaceId).find(
        (x) => x.id === req.params.setId,
      );
      if (!s) return fail("NOT_FOUND", "document set not found");
      if (s.connector) return connectorOwned(s.id, "set");
      const { name, description } = f;
      // Its own name, byte for byte, is no rename, even beside a set that
      // has it from before the names were unique.
      if (
        name !== undefined &&
        name !== s.name &&
        taken(m.workspaceId, name, s.id)
      )
        return nameTaken(name);
      if (name !== undefined) s.name = name;
      if (description !== undefined) s.description = description;
      return ok({ set: toSet(s) } satisfies DocumentSetResponse);
    },
  },
  {
    method: "DELETE",
    path: `${WS}/document-sets/{setId}`,
    handle: (req) => {
      const m = caller(req, true);
      if (isReply(m)) return m;
      const s = documentSets(m.workspaceId).find(
        (x) => x.id === req.params.setId,
      );
      if (!s) return fail("NOT_FOUND", "document set not found");
      if (s.connector) return connectorOwned(s.id, "set");
      if ([...documents.values()].some((d) => d.setId === s.id && live(d)))
        return fail(
          "FAILED_PRECONDITION",
          "the document set still holds documents; move or delete them first",
        );
      sets.delete(s.id);
      return ok({} satisfies DeleteDocumentSetResponse);
    },
  },
];

// ---------------------------------------------------------------- The presigned PUT

const s3Error = (
  res: ServerResponse,
  status: number,
  code: string,
  message: string,
) => {
  res.writeHead(status, { "Content-Type": "application/xml" });
  res.end(
    `<?xml version="1.0" encoding="UTF-8"?><Error><Code>${code}</Code><Message>${message}</Message></Error>`,
  );
};

function readBytes(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => resolve(Buffer.concat(chunks)));
    req.on("error", reject);
  });
}

/**
 * The bucket's side of a presigned PUT, at /__uploads/{id}?token=: CORS for
 * the app's origin (a preflight, then the PUT), and S3's checks. True when it
 * handled the request.
 */
export async function handleDocumentUpload(
  req: IncomingMessage,
  res: ServerResponse,
  url: URL,
): Promise<boolean> {
  const match = /^\/__uploads\/([^/]+)$/.exec(url.pathname);
  if (!match) return false;
  const origin = req.headers.origin;
  if (origin) {
    res.setHeader("Access-Control-Allow-Origin", origin);
    res.setHeader("Vary", "Origin");
    res.setHeader("Access-Control-Expose-Headers", "ETag");
  }
  if (req.method === "OPTIONS") {
    res.writeHead(204, {
      "Access-Control-Allow-Methods": "PUT",
      // The bucket's CORS allows only content-type, so any other header
      // fails the preflight.
      "Access-Control-Allow-Headers": "content-type",
      "Access-Control-Max-Age": "600",
    });
    res.end();
    return true;
  }
  const body = await readBytes(req);
  if (req.method !== "PUT") {
    s3Error(
      res,
      405,
      "MethodNotAllowed",
      "The specified method is not allowed.",
    );
    return true;
  }
  const d = documents.get(match[1]!);
  const signed = d?.uploads.find(
    (u) => u.token === url.searchParams.get("token"),
  );
  if (!d || !signed) {
    s3Error(res, 403, "AccessDenied", "Access Denied");
    return true;
  }
  if (signed.expiresAt < Date.now()) {
    s3Error(res, 403, "AccessDenied", "Request has expired");
    return true;
  }
  if ((knobs.documentUploadFailures || 0) > 0) {
    knobs.documentUploadFailures! -= 1;
    s3Error(res, 503, "SlowDown", "Please reduce your request rate.");
    return true;
  }
  if (
    req.headers["content-type"] !== d.mediaType ||
    url.searchParams.get("X-Amz-Checksum-Sha256") !== base64Digest(d.sha256) ||
    body.length !== d.sizeBytes
  ) {
    s3Error(
      res,
      403,
      "SignatureDoesNotMatch",
      "The request signature we calculated does not match the signature you provided.",
    );
    return true;
  }
  if (sha256(body) !== d.sha256) {
    s3Error(
      res,
      400,
      "BadDigest",
      "The SHA256 you specified did not match the calculated checksum.",
    );
    return true;
  }
  contents.set(d.id, body);
  res.writeHead(200, { ETag: `"${sha256(body).slice(0, 32)}"` });
  res.end();
  return true;
}

// Acme's fixtures for pnpm dev:mock (MOCK_DOCUMENT_FIXTURES): a CSV, a
// JSON and a PDF document in General, uploaded, and a scan with no text
// layer, whose read fails.
const FIXTURES: {
  name: string;
  mediaType: DocumentMediaType;
  bytes: Buffer;
}[] = [
  {
    name: "renewals.csv",
    mediaType: "text/csv",
    bytes: Buffer.from(
      [
        "customer,plan,seats,renews_on,owner",
        "Globex,Enterprise,250,2026-12-01,Ada Lovelace",
        "Initech,Team,40,2027-01-15,Alan Turing",
        'Hooli,"Enterprise, annual",900,2027-03-31,Ada Lovelace',
        "Umbrella,Team,12,2026-11-20,Grace Hopper",
      ].join("\n"),
    ),
  },
  {
    name: "pricing.json",
    mediaType: "application/json",
    bytes: Buffer.from(
      JSON.stringify(
        {
          currency: "usd",
          plans: [
            { id: "team", seat: 12, minimum: 5, features: ["sso", "audit"] },
            {
              id: "enterprise",
              seat: 30,
              minimum: 100,
              features: ["sso", "audit", "scim", "support"],
            },
          ],
          discounts: { annual: 0.15, nonprofit: 0.3 },
          updated: "2026-10-01",
        },
        null,
        2,
      ),
    ),
  },
  {
    name: "renewal-terms.pdf",
    mediaType: "application/pdf",
    bytes: textPdf([
      [
        "Renewal terms",
        "Globex Corporation renews its analytics plan on 1 December 2026,",
        "for 250 seats at the Enterprise rate.",
        "Initech LLC renews on 15 January 2027 for 40 seats.",
      ],
      [
        "Notice",
        "Either party gives 90 days notice to end a renewal.",
        "Umbrella Holdings has no renewal with us.",
      ],
    ]),
  },
  {
    name: "signed-order-form.pdf",
    mediaType: "application/pdf",
    bytes: textPdf([[], []]),
  },
];

onReset(() => {
  documents.clear();
  sets.clear();
  contents.clear();
  reads.clear();
  waking.clear();
  extractions.clear();
  members.clear();
  legacy = 0;
  delete knobs.documentUploadFailures;
  delete knobs.documentReadMs;
  delete knobs.legacyDocumentSets;
  delete knobs.documentsPerStream;
  delete knobs.eventSetAllowanceSpent;
  sets.set(ACME_GENERAL, {
    id: ACME_GENERAL,
    workspaceId: "ws_acme",
    name: "General",
    description: "",
    createdAt: "2026-09-28T09:00:00.000Z",
  });
  if (!process.env.MOCK_DOCUMENT_FIXTURES) return;
  FIXTURES.forEach((f, i) => {
    const { bytes } = f,
      id = `doc_acme_fixture_${i + 1}`;
    documents.set(id, {
      id,
      workspaceId: "ws_acme",
      setId: ACME_GENERAL,
      name: f.name,
      mediaType: f.mediaType,
      sizeBytes: bytes.length,
      sha256: sha256(bytes),
      clientToken: id,
      status: "uploaded",
      createdAt: `2026-09-28T09:0${i + 1}:00.000Z`,
      uploads: [],
    });
    contents.set(id, bytes);
    void (
      FORMATS[f.mediaType].kind === "pdf"
        ? readPdfText(bytes)
        : readTableFile(f.mediaType, bytes)
    ).then((result) => reads.set(id, { at: 0, result }));
  });
});
