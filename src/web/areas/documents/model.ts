import type {
  DocumentExtraction,
  DocumentMediaType,
  DocumentRead,
  DocumentReadOutcome,
  DocumentSetReader,
  ExtractionError,
  FoundryDocument,
  FoundryDocumentSet,
  FoundryFileReadState,
  FoundryLoad,
  FoundryReadKind,
  GraphJob,
} from "../../../shared/wfe";
import type { Status } from "../../kit";
import { plural } from "../../kit/util";
import { setFolders, type SetFolder } from "../data/model";
import type { Span } from "../explore/model";

// The documents' logic, kept apart from their components so it is tested
// on its own: what may be uploaded and how a document or set may be named,
// how ontologies read each format and what reading a file came to, an
// upload's steps and what the ontologies did with it, a document's state,
// and its text's highlights (UTF-8 byte offsets to UTF-16, and overlapping
// marks).

/** The WFE's cap on a text document, for uploading, viewing and reading alike. */
export const MAX_TEXT_BYTES = 1_048_576;
/** The WFE's cap on a table file or a PDF. */
export const MAX_FILE_BYTES = 3_145_728;
/** The longest document name, in characters (code points). */
export const MAX_DOCUMENT_NAME = 255;
const BAD_NAME = /[\p{Cc}\\/]/u;

/**
 * Why a document can't be renamed to `name`, or undefined when it can: as
 * the WFE checks it, once trimmed 1 to 255 characters with no slash,
 * backslash or control character. Its extension may change: its type was
 * fixed at upload.
 *
 * @example
 * documentNameProblem("Q3/notes.md"); // "A document’s name can’t have a slash, a backslash or a control character."
 */
export function documentNameProblem(name: string) {
  const n = name.trim();
  if (!n) return "Name the document.";
  if ([...n].length > MAX_DOCUMENT_NAME)
    return `A document’s name is at most ${MAX_DOCUMENT_NAME} characters.`;
  if (BAD_NAME.test(n))
    return "A document’s name can’t have a slash, a backslash or a control character.";
  return undefined;
}

// ---------------------------------------------------------------- Formats

/** Where a table document's rows come from. */
export type TableSource = "csv" | "tsv" | "jsonl" | "parquet";

/**
 * How a document shows on its Data page: Markdown rendered (its source a
 * click away; MDX too, never compiled or run, its frontmatter a code block),
 * plain text as it is, JSON as a tree (its source a click away), a table's
 * first rows in a grid, a PDF's text once read (its pages in the browser's
 * viewer a click away), and a type this page doesn't know as text with a
 * download. A PDF's pages, Parquet tables and `unknown` read the bytes
 * (`:bytes`); the others, and a PDF's text, the text (`:content`).
 */
export type DocumentViewer =
  | { kind: "markdown"; mdx: boolean }
  | { kind: "plain" }
  | { kind: "json" }
  | { kind: "table"; source: TableSource }
  | { kind: "pdf" }
  | { kind: "unknown" };

/**
 * How ontologies read a format (the WFE's FoundryReadKind):
 * - text (Markdown, MDX, text): loads read its text for what the version's
 *   routes say it fills;
 * - table (CSV, TSV, JSON, JSONL, Parquet): Foundry reads it once, when its
 *   upload completes, as a table of its own that a version's mappings name
 *   by the document's id; never read as text;
 * - pdf: Foundry reads its text once, when its upload completes, its pages
 *   separated by a form feed (`\f`); once read, loads and generations read
 *   that text as plain text. A scanned PDF has no text to read.
 */
export type ReadKind = "text" | "table" | "pdf";

/** A format the WFE stores: one media type, the extensions it is mapped from. */
export type DocumentFormat = {
  mediaType: DocumentMediaType;
  // Its name in copy: "PDF files hold at most 3.0 MiB."
  name: string;
  // Lowercase, with the dot.
  extensions: readonly string[];
  viewer: Exclude<DocumentViewer, { kind: "unknown" }>;
  kind: ReadKind;
  // The most bytes it holds (MAX_TEXT_BYTES or MAX_FILE_BYTES).
  maxBytes: number;
};

/** Every format the WFE stores, as it maps extensions to media types. */
export const DOCUMENT_FORMATS: readonly DocumentFormat[] = [
  {
    mediaType: "text/markdown",
    name: "Markdown",
    extensions: [".md", ".markdown"],
    viewer: { kind: "markdown", mdx: false },
    kind: "text",
    maxBytes: MAX_TEXT_BYTES,
  },
  {
    mediaType: "text/mdx",
    name: "MDX",
    extensions: [".mdx"],
    viewer: { kind: "markdown", mdx: true },
    kind: "text",
    maxBytes: MAX_TEXT_BYTES,
  },
  {
    mediaType: "text/plain",
    name: "text",
    extensions: [".txt"],
    viewer: { kind: "plain" },
    kind: "text",
    maxBytes: MAX_TEXT_BYTES,
  },
  {
    mediaType: "application/json",
    name: "JSON",
    extensions: [".json"],
    viewer: { kind: "json" },
    kind: "table",
    maxBytes: MAX_FILE_BYTES,
  },
  {
    mediaType: "text/csv",
    name: "CSV",
    extensions: [".csv"],
    viewer: { kind: "table", source: "csv" },
    kind: "table",
    maxBytes: MAX_FILE_BYTES,
  },
  {
    mediaType: "text/tab-separated-values",
    name: "TSV",
    extensions: [".tsv"],
    viewer: { kind: "table", source: "tsv" },
    kind: "table",
    maxBytes: MAX_FILE_BYTES,
  },
  {
    mediaType: "application/x-ndjson",
    name: "JSONL",
    extensions: [".jsonl", ".ndjson"],
    viewer: { kind: "table", source: "jsonl" },
    kind: "table",
    maxBytes: MAX_FILE_BYTES,
  },
  {
    mediaType: "application/vnd.apache.parquet",
    name: "Parquet",
    extensions: [".parquet"],
    viewer: { kind: "table", source: "parquet" },
    kind: "table",
    maxBytes: MAX_FILE_BYTES,
  },
  {
    mediaType: "application/pdf",
    name: "PDF",
    extensions: [".pdf"],
    viewer: { kind: "pdf" },
    kind: "pdf",
    maxBytes: MAX_FILE_BYTES,
  },
];

/** The formats by name, as copy lists them: "Markdown, text, …, Parquet, or PDF". */
export const FORMAT_NAMES = new Intl.ListFormat("en", {
  style: "long",
  type: "disjunction",
}).format(DOCUMENT_FORMATS.map((f) => f.name));

/**
 * A file's format from its name's extension (any case), or undefined when
 * the WFE stores no such file.
 *
 * @example
 * formatOfName("Q3.CSV")?.mediaType; // "text/csv"
 * formatOfName("deck.docx"); // undefined
 */
export function formatOfName(name: string): DocumentFormat | undefined {
  const extension = /\.[^.]*$/.exec(name.toLowerCase())?.[0];
  return DOCUMENT_FORMATS.find((f) => f.extensions.includes(extension || ""));
}

/**
 * A stored document's format, from its media type: a rename may change its
 * extension, so the type it was uploaded with decides. Undefined for a type
 * this page doesn't know.
 *
 * @example
 * formatOf("application/pdf")?.name; // "PDF"
 */
export const formatOf = (mediaType?: string): DocumentFormat | undefined =>
  DOCUMENT_FORMATS.find((f) => f.mediaType === mediaType);

/**
 * The viewer for a document's media type. The media type decides, never the
 * text or the name: JSON in a .md file is Markdown. A type this page doesn't
 * know is shown as text, with a download.
 *
 * @example
 * documentViewer("text/markdown"); // { kind: "markdown", mdx: false }
 * documentViewer("text/tab-separated-values"); // { kind: "table", source: "tsv" }
 * documentViewer("image/png"); // { kind: "unknown" }
 */
export const documentViewer = (mediaType?: string): DocumentViewer =>
  formatOf(mediaType)?.viewer ?? { kind: "unknown" };

const KINDS: Partial<Record<FoundryReadKind, ReadKind>> = {
  FOUNDRY_READ_KIND_TEXT: "text",
  FOUNDRY_READ_KIND_TABLE: "table",
  FOUNDRY_READ_KIND_PDF: "pdf",
};

/**
 * How ontologies read a document: the WFE's word (`read.kind`), else its
 * format's (before its upload completes); undefined for a type this page
 * doesn't know.
 *
 * @example
 * readKindOf({ id: "d", mediaType: "text/csv" }); // "table"
 * readKindOf({ id: "d", read: { kind: "FOUNDRY_READ_KIND_TEXT" } }); // "text"
 */
export const readKindOf = (d: FoundryDocument): ReadKind | undefined =>
  KINDS[d.read?.kind as FoundryReadKind] ?? formatOf(d.mediaType)?.kind;

/**
 * What reading a document has come to (the WFE's FoundryFileRead):
 * - ready: ontologies can read it; a table file's rows, or a PDF's pages,
 *   once read;
 * - reading: a table file or a PDF being read, or waiting to be; `retrying`
 *   after a failure that isn't the file's (storage), which is read again
 *   later. A generation started now leaves it out;
 * - failed: it can't be read; `message` says why (a scanned PDF has no text
 *   to read) or what to fix;
 * - none: no read to speak of: its upload didn't complete, it was deleted,
 *   or the WFE didn't say.
 */
export type FileRead =
  | { state: "ready"; rows?: number; pages?: number }
  | { state: "reading"; retrying: boolean }
  | { state: "failed"; code: "too_large" | "unsupported"; message: string }
  | { state: "none" };

const STATES: Partial<
  Record<FoundryFileReadState, "ready" | "reading" | "failed">
> = {
  FOUNDRY_FILE_READ_STATE_READY: "ready",
  FOUNDRY_FILE_READ_STATE_READING: "reading",
  FOUNDRY_FILE_READ_STATE_FAILED: "failed",
};

/**
 * What reading a document has come to, from the WFE's `read`. A failure
 * with no message is said in the words of its code.
 *
 * @example
 * fileRead({ id: "d", status: "uploaded", read: { state: "FOUNDRY_FILE_READ_STATE_READY", rows: "12" } }); // { state: "ready", rows: 12 }
 * fileRead({ id: "d", status: "uploaded", read: { state: "FOUNDRY_FILE_READ_STATE_READY", pages: 3 } }); // { state: "ready", pages: 3 }
 * fileRead({ id: "d", status: "uploaded", read: { state: "FOUNDRY_FILE_READ_STATE_READING", retrying: true } }); // { state: "reading", retrying: true }
 * fileRead({ id: "d" }); // { state: "none" }
 */
export function fileRead(d: FoundryDocument): FileRead {
  const r = d.read;
  switch (
    d.status === "uploaded"
      ? STATES[r?.state ?? "FOUNDRY_FILE_READ_STATE_UNSPECIFIED"]
      : undefined
  ) {
    case "ready": {
      const rows = r?.rows === undefined ? NaN : Number(r.rows);
      return {
        state: "ready",
        ...(Number.isFinite(rows) && { rows }),
        ...(r?.pages !== undefined && { pages: r.pages }),
      };
    }
    case "reading":
      return { state: "reading", retrying: !!r?.retrying };
    case "failed": {
      const code = r?.error?.code === "too_large" ? "too_large" : "unsupported",
        message = r?.error?.message;
      return {
        state: "failed",
        code,
        // The WFE writes a clause ("the file is damaged or is not a PDF"):
        // shown as a sentence.
        message: message
          ? message.charAt(0).toUpperCase() +
            message.slice(1) +
            (/[.!?]$/.test(message) ? "" : ".")
          : code === "too_large"
            ? "It is past a limit of what Foundry reads. Split it into smaller files."
            : `It isn’t ${formatOf(d.mediaType)?.name || "what its type says"}. Upload a fixed file.`,
      };
    }
    case undefined:
      return { state: "none" };
  }
}

/**
 * Whether ontologies read a document as text: Markdown, MDX and text, and a
 * PDF once its text is read. A table file never is: its rows load through
 * mappings.
 *
 * @example
 * readsAsText({ id: "d", mediaType: "text/markdown" }); // true
 * readsAsText({ id: "d", status: "uploaded", mediaType: "application/pdf", read: { state: "FOUNDRY_FILE_READ_STATE_READING" } }); // false
 */
export const readsAsText = (d: FoundryDocument) =>
  readKindOf(d) === "text" ||
  (readKindOf(d) === "pdf" && fileRead(d).state === "ready");

/**
 * A table file's or a PDF's read in a few words, for lists: its rows or
 * pages, or that it is being read or couldn't be. Undefined for text, which
 * loads read as it is, and for a read the WFE didn't say.
 *
 * @example
 * fileReadLabel({ id: "d", status: "uploaded", mediaType: "text/csv", read: { state: "FOUNDRY_FILE_READ_STATE_READY", rows: "1200" } }); // "1,200 rows"
 * fileReadLabel({ id: "d", status: "uploaded", mediaType: "application/pdf", read: { state: "FOUNDRY_FILE_READ_STATE_READY", pages: 1 } }); // "1 page"
 */
export function fileReadLabel(d: FoundryDocument): string | undefined {
  if (readKindOf(d) === "text") return undefined;
  const r = fileRead(d);
  switch (r.state) {
    case "ready":
      return r.rows !== undefined
        ? plural(r.rows, "row")
        : r.pages !== undefined
          ? plural(r.pages, "page")
          : "Read";
    case "reading":
      return r.retrying ? "Waiting to be read again" : "Being read…";
    case "failed":
      return "Couldn’t be read";
    case "none":
      return undefined;
  }
}

/**
 * What ontologies do with a file uploaded or moved into `setName`, in a
 * sentence, when that isn't reading its text: a table file is read once, as
 * a table that a version maps after a generation that reads its set; a PDF
 * whose text couldn't be read isn't read, and why. Undefined for text and
 * any other PDF (being read, or read), whose readers say what they do
 * (`readsText`), and for a type this page doesn't know.
 *
 * @example
 * kindText({ id: "d", mediaType: "text/csv" }, "General"); // "Foundry reads its rows once. A generation that reads General maps it as a table of its own."
 * kindText({ id: "d", status: "uploaded", mediaType: "application/pdf", read: { state: "FOUNDRY_FILE_READ_STATE_FAILED", error: { message: "Scanned PDF: no text to read" } } }, "General"); // "Ontologies can’t read it. Scanned PDF: no text to read."
 * kindText({ id: "d", mediaType: "text/markdown" }, "General"); // undefined
 */
export function kindText(d: FoundryDocument, setName: string) {
  switch (formatOf(d.mediaType)?.kind) {
    case "table":
      return `Foundry reads its rows once. A generation that reads ${setName} maps it as a table of its own.`;
    case "pdf": {
      const r = fileRead(d);
      return r.state === "failed"
        ? `Ontologies can’t read it. ${r.message}`
        : undefined;
    }
    case "text":
    case undefined:
      return undefined;
  }
}

/**
 * Why a file can't be uploaded, or undefined when it can. The name is
 * checked as the WFE checks it once trimmed: at most 255 characters, with no
 * slash, backslash or control character.
 *
 * @example
 * fileProblem({ name: "deck.docx", size: 10 }); // "Only Markdown, MDX, text, JSON, CSV, TSV, JSONL, Parquet, or PDF files can be uploaded."
 */
export function fileProblem(file: { name: string; size: number }) {
  const name = file.name.trim();
  if (!formatOfName(name)) return `Only ${FORMAT_NAMES} files can be uploaded.`;
  // Characters as the WFE counts them: code points, not UTF-16 units.
  if ([...name].length > MAX_DOCUMENT_NAME)
    return "The file's name is longer than 255 characters. Rename it.";
  if (BAD_NAME.test(name))
    return "The file's name has a slash, a backslash or a control character. Rename it.";
  if (file.size === 0) return "The file is empty.";
  const f = formatOfName(name)!;
  if (file.size > f.maxBytes)
    return `${f.name.charAt(0).toUpperCase() + f.name.slice(1)} files hold at most ${formatBytes(f.maxBytes)}. Split it into smaller ${f.kind === "text" ? "documents" : "files"}.`;
  return undefined;
}

/** Lowercase hex, as the WFE takes a SHA-256. */
export const hex = (digest: ArrayBuffer) =>
  [...new Uint8Array(digest)]
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");

/**
 * How an upload ended:
 * - `existing`: the workspace had the document already, from another
 *   upload, in `document.setId` (maybe another set), even if this upload's
 *   bytes were the ones that finished it;
 * - `uploaded`: this upload completed it, and every ontology whose loads
 *   read its set said what it did (`reads`);
 * - `finished`: a retry found this upload already complete (the answer to
 *   its earlier attempt was lost), so what the ontologies did is unknown.
 */
export type UploadResult =
  | { kind: "existing" }
  | { kind: "uploaded"; reads: DocumentRead[] }
  | { kind: "finished" };

/** One file's upload, step by step: what this tab shows while it runs. */
export type UploadStep =
  | { step: "checking" }
  | { step: "uploading"; loaded: number; total: number }
  | { step: "completing" }
  | { step: "done"; document: FoundryDocument; result: UploadResult }
  // `retry`: false when the file itself can't be uploaded.
  | { step: "failed"; error: string; retry: boolean };

/** The step in a few words, for its card. */
export function uploadText(s: UploadStep): string {
  switch (s.step) {
    case "checking":
      return "Preparing…";
    case "uploading":
      return `Uploading… ${s.total ? Math.floor((100 * s.loaded) / s.total) : 0}%`;
    case "completing":
      return "Finishing…";
    case "done":
      return s.result.kind === "existing" ? "Already uploaded" : "Uploaded";
    case "failed":
      return s.error;
  }
}

/** What a failed presigned PUT means, from its HTTP status and S3's code. */
export function putFailure(status: number, body: string): string {
  if (status === 0)
    return "The upload was interrupted. Check your connection and try again.";
  if (/expired/i.test(body)) return "The upload link expired. Try again.";
  if (status === 503 || /SlowDown/.test(body))
    return "Storage is busy. Try again in a moment.";
  return `Storage refused the upload (${status}). Try again.`;
}

// Headers a browser sets itself and refuses from a script.
const FORBIDDEN_HEADERS = new Set(["content-length", "host", "connection"]);
/** The signed headers a presigned PUT must send, minus those the browser sets. */
export const putHeaders = (headers: Record<string, string> = {}) =>
  Object.entries(headers).filter(
    ([name]) => !FORBIDDEN_HEADERS.has(name.toLowerCase()),
  );

// ---------------------------------------------------------------- What ontologies did

/** What completing or moving a document did for one ontology. */
export type ReadOutcome =
  | "queued"
  | "owed"
  | "no_budget"
  | "held"
  | "skipped"
  | "reading";
const outcomes: Partial<Record<DocumentReadOutcome, ReadOutcome>> = {
  FOUNDRY_DOCUMENT_READ_OUTCOME_QUEUED: "queued",
  FOUNDRY_DOCUMENT_READ_OUTCOME_OWED: "owed",
  FOUNDRY_DOCUMENT_READ_OUTCOME_NO_BUDGET: "no_budget",
  FOUNDRY_DOCUMENT_READ_OUTCOME_HELD: "held",
  FOUNDRY_DOCUMENT_READ_OUTCOME_SKIPPED: "skipped",
  FOUNDRY_DOCUMENT_READ_OUTCOME_READING: "reading",
};
/** An outcome the WFE names, or undefined for one it doesn't (unspecified, newer). */
export const readOutcome = (o?: string): ReadOutcome | undefined =>
  outcomes[o as DocumentReadOutcome];

/**
 * Names joined as a sentence says them.
 *
 * @example
 * andList(["General", "Board", "Notes"]); // "General, Board, and Notes"
 */
export const andList = (names: readonly string[]) =>
  new Intl.ListFormat("en", { style: "long", type: "conjunction" }).format(
    names,
  );
// A sentence about some ontologies: the verb agrees with how many.
const say = (
  names: readonly string[],
  one: string,
  many: string,
  rest: (its: string) => string,
) =>
  names.length
    ? [
        `${andList(names)} ${names.length === 1 ? one : many} ${rest(names.length === 1 ? "its" : "their")}`,
      ]
    : [];

/**
 * What the ontologies did with an upload or a move into `setName`, in
 * sentences: each outcome's ontologies by name. One that reads the set
 * without a route naming it (`readers`' `routed` false) stores the document
 * without reading what it holds. A PDF whose text Foundry is still reading
 * is read once it is. A reader with no outcome (no load yet) reads it on
 * its first load. With neither, no ontology reads the set.
 *
 * @example
 * readsText(
 *   [{ ontologyId: "o1", outcome: "FOUNDRY_DOCUMENT_READ_OUTCOME_QUEUED" }],
 *   { name: () => "Renewals", setName: "General", readers: [] },
 * ); // "Renewals reads it now."
 */
export function readsText(
  reads: readonly DocumentRead[],
  {
    name,
    setName,
    readers,
  }: {
    name: (ontologyId: string) => string;
    setName: string;
    readers: readonly DocumentSetReader[];
  },
): string {
  const unrouted = new Set(
    readers.filter((r) => r.routed === false).map((r) => r.ontologyId),
  );
  const of = (want: ReadOutcome, routed?: boolean) =>
    reads
      .filter(
        (r) =>
          readOutcome(r.outcome) === want &&
          (routed === undefined || routed !== unrouted.has(r.ontologyId)),
      )
      .map((r) => name(r.ontologyId || ""));
  const reading = (want: ReadOutcome) => of(want, true);
  const storing = [
    ...of("queued", false),
    ...of("owed", false),
    ...of("no_budget", false),
    ...of("reading", false),
  ];
  const later = readers
    .filter((r) => !reads.some((x) => x.ontologyId === r.ontologyId))
    .map((r) => name(r.ontologyId || ""));
  const sentences = [
    ...say(reading("queued"), "reads", "read", () => "it now."),
    ...say(
      reading("owed"),
      "reads",
      "read",
      () => "it once the job running now ends.",
    ),
    ...say(
      reading("no_budget"),
      "reads",
      "read",
      (its) =>
        `it with ${its} next load: this month’s budget for reading is spent.`,
    ),
    ...say(
      reading("reading"),
      "reads",
      "read",
      () => "it once Foundry has read its text.",
    ),
    ...say(
      storing,
      "stores",
      "store",
      () => `it without reading it: no route reads ${setName}.`,
    ),
    ...say(
      of("held"),
      "keeps",
      "keep",
      (its) => `what ${its} current load read from it until the next load.`,
    ),
    ...say(
      of("skipped"),
      "doesn’t",
      "don’t",
      (its) => `read it yet: you can’t write to ${its} database.`,
    ),
    ...say(later, "reads", "read", (its) => `it with ${its} first load.`),
  ];
  return sentences.length
    ? sentences.join(" ")
    : `No ontology reads ${setName} yet.`;
}

const failures: Record<NonNullable<ExtractionError["code"]>, string> = {
  too_large:
    "It names more than one write can store. Split it into smaller documents.",
  unsupported: "Ontologies don’t read its format yet.",
  model: "The model couldn’t read it.",
  store: "The database couldn’t store what was read.",
};
/**
 * Why reading a document failed, in a sentence, and whether a load retries
 * it. A load records a PDF whose text couldn't be read with its read's code
 * (`unsupported`, `too_large`): that read says why. One a load skipped
 * before its text was read (an older load) is read by the next load.
 *
 * @example
 * failureText({ code: "model", retryable: true }); // "The model couldn’t read it. The next load tries again."
 * failureText(
 *   { code: "unsupported" },
 *   { id: "d", status: "uploaded", mediaType: "application/pdf", read: { state: "FOUNDRY_FILE_READ_STATE_FAILED", error: { message: "Scanned PDF: no text to read." } } },
 * ); // "Scanned PDF: no text to read."
 */
export function failureText(
  error?: ExtractionError | null,
  d?: FoundryDocument,
) {
  const read = d && readKindOf(d) === "pdf" ? fileRead(d) : undefined;
  const why =
    read && (error?.code === "unsupported" || error?.code === "too_large")
      ? read.state === "failed"
        ? read.message
        : read.state === "ready"
          ? "Its text wasn’t read when this load ran. The next load reads it."
          : "Foundry couldn’t read its text."
      : failures[error?.code || "model"] || failures.model;
  return error?.retryable ? `${why} The next load tries again.` : why;
}

const isLive = (j?: GraphJob | null) =>
  j?.status === "queued" || j?.status === "running";

/**
 * The job reading documents now, with its load: a load or an extract that
 * is queued or running (a delete reads none). One job runs per ontology.
 */
export function liveRead(
  loads: readonly FoundryLoad[] | undefined,
): { load: FoundryLoad; job: GraphJob } | undefined {
  for (const load of loads || [])
    for (const job of load.jobs || [])
      if (isLive(job) && job.kind !== "delete") return { load, job };
  return undefined;
}

/**
 * A document's state as a status badge: uploading, a PDF's text being read
 * (or why it couldn't be), waiting for a load or being read by the live
 * one, then what reading it found. A table file's is always that: loads
 * read its rows through mappings, not its text (one an older load recorded
 * as unsupported too).
 *
 * @example
 * documentBadge({ id: "d", status: "uploaded" }, null); // { status: "queued", label: "Waiting for a load" }
 */
export function documentBadge(
  d: FoundryDocument,
  liveLoad: GraphJob | null | undefined,
): { status: Status; label: string } {
  if (d.status === "deleted") return { status: "draft", label: "Deleted" };
  if (d.status === "pending_upload")
    return { status: "queued", label: "Uploading" };
  // Loads read a table file's rows through mappings, never its text.
  if (readKindOf(d) === "table")
    return { status: "canceled", label: "Table file" };
  const x = d.extraction,
    pdf = readKindOf(d) === "pdf" ? fileRead(d).state : undefined;
  if (!x && pdf === "reading")
    return { status: "queued", label: "Reading its text" };
  if (!x && pdf === "failed")
    return { status: "failed", label: "Couldn’t be read" };
  if (!x)
    return isLive(liveLoad)
      ? liveLoad!.phase === "documents" && liveLoad!.status === "running"
        ? { status: "running", label: "Reading" }
        : { status: "queued", label: "Queued for the load" }
      : { status: "queued", label: "Waiting for a load" };
  return extractionBadge(x);
}

/**
 * What a load made of a document, as a status badge.
 *
 * @example
 * extractionBadge({ status: "extracted", reviews: 2 }); // { status: "review", label: "Needs review · 2" }
 */
export function extractionBadge(x: DocumentExtraction): {
  status: Status;
  label: string;
} {
  switch (x.status) {
    case "duplicate":
      return { status: "canceled", label: "Duplicate" };
    case "out_of_scope":
      return { status: "canceled", label: "Not read by its version" };
    case "failed":
      return { status: "failed", label: "Couldn’t be read" };
    case "extracted":
      return (x.reviews || 0) > 0
        ? { status: "review", label: `Needs review · ${x.reviews}` }
        : { status: "succeeded", label: "Read" };
    default:
      return { status: "draft", label: "Unknown" };
  }
}

/** A document's dot in an ontology's documents explorer: one of four colours. */
export type DocumentDot = {
  // Orange, red, green, and grey (waiting, or not read).
  status: Extract<
    Status,
    "review" | "failed" | "succeeded" | "queued" | "canceled"
  >;
  label: string;
};

/**
 * What an ontology's current load made of a document, as its dot in the
 * Ontologies panel's documents explorer, in the words of the documents
 * list's filters and `documentBadge`: orange when it needs review, red when
 * it couldn't be read (a PDF's says why), green when read; grey while it
 * waits (uploading, a PDF's text being read, or not read yet) and when it
 * isn't read as text (deleted, a duplicate, out of its version's scope, a
 * table file, whose rows load through mappings, or a type the WFE doesn't
 * read).
 *
 * @example
 * documentDot({ id: "d", extraction: { status: "extracted", reviews: 2 } }); // { status: "review", label: "Needs review · 2" }
 * documentDot({ id: "d", status: "uploaded", mediaType: "application/pdf", read: { state: "FOUNDRY_FILE_READ_STATE_FAILED", error: { message: "Scanned PDF: no text to read." } } }); // { status: "failed", label: "Scanned PDF: no text to read." }
 * documentDot({ id: "d", mediaType: "text/csv" }); // { status: "canceled", label: "A table file: loads read its rows through mappings" }
 */
export function documentDot(d: FoundryDocument): DocumentDot {
  if (d.status === "deleted") return { status: "canceled", label: "Deleted" };
  if (d.status === "pending_upload")
    return { status: "queued", label: "Uploading" };
  if (readKindOf(d) === "table")
    return {
      status: "canceled",
      label: "A table file: loads read its rows through mappings",
    };
  const x = d.extraction,
    read = readKindOf(d) === "pdf" ? fileRead(d) : undefined;
  if (!x)
    return read?.state === "reading"
      ? { status: "queued", label: "Reading its text" }
      : read?.state === "failed"
        ? { status: "failed", label: read.message }
        : { status: "queued", label: "Waiting" };
  switch (x.status) {
    case "extracted":
      return (x.reviews || 0) > 0
        ? { status: "review", label: `Needs review · ${x.reviews}` }
        : { status: "succeeded", label: "Read" };
    case "failed": {
      const code = x.error?.code;
      // A PDF's read says why; the WFE doesn't read a type this page
      // doesn't know.
      return read && (code === "unsupported" || code === "too_large")
        ? { status: "failed", label: failureText(x.error, d) }
        : code === "unsupported"
          ? { status: "canceled", label: failureText(x.error).slice(0, -1) }
          : { status: "failed", label: "Couldn’t be read" };
    }
    case "duplicate":
      return { status: "canceled", label: "Duplicate" };
    case "out_of_scope":
      return { status: "canceled", label: "Not read by its version" };
    default:
      return { status: "canceled", label: "Unknown" };
  }
}

/**
 * Which ontologies read a set, in a sentence: those whose reading version
 * has it in scope.
 *
 * @example
 * readersText("Board", [{ ontologyId: "o1" }], () => "Renewals"); // "Renewals reads Board."
 */
export function readersText(
  setName: string,
  readers: readonly DocumentSetReader[],
  name: (ontologyId: string) => string,
) {
  return readers.length
    ? `${andList(readers.map((r) => name(r.ontologyId || "")))} ${readers.length === 1 ? "reads" : "read"} ${setName}.`
    : `No ontology reads ${setName} yet.`;
}

/**
 * How an ontology associated with a document reads it, in a sentence, from
 * its entry in the set's `readBy`: its reading version's routes read the
 * set, or it only stores the set's documents; with no entry, its current
 * load holds the document from before it moved, and the next load drops it.
 *
 * @example
 * ontologyReadText("General", undefined); // "Its current load holds it, but its version doesn’t read General: the next load drops it."
 */
export function ontologyReadText(
  setName: string,
  reader: DocumentSetReader | undefined,
) {
  return !reader
    ? `Its current load holds it, but its version doesn’t read ${setName}: the next load drops it.`
    : reader.routed
      ? `Its routes read ${setName}: loads read what it holds.`
      : `No route reads ${setName}: loads store it without reading it.`;
}

// ---------------------------------------------------------------- Document sets

/** The longest set name, in characters (code points), as the WFE allows. */
export const MAX_SET_NAME = 120;

/** A set name as the WFE compares names: case and accents aside. */
const folded = (name: string) =>
  name.normalize("NFD").replace(/\p{M}/gu, "").toLowerCase();

/**
 * Why a set name can't be used, or undefined: as the WFE checks it, once
 * trimmed 1 to 120 characters, and no other set's name in the workspace
 * (case and accents aside).
 *
 * @example
 * setNameProblem(" ", []); // "Name the set."
 * setNameProblem("général", [{ id: "set_1", name: "General" }]); // "There is a set named General already."
 */
export function setNameProblem(
  name: string,
  sets: readonly FoundryDocumentSet[],
  self?: string,
) {
  const n = name.trim();
  if (!n) return "Name the set.";
  if ([...n].length > MAX_SET_NAME)
    return `A set’s name is at most ${MAX_SET_NAME} characters.`;
  const taken = sets.find(
    (s) => s.id !== self && folded(s.name || "") === folded(n),
  );
  return taken ? `There is a set named ${taken.name} already.` : undefined;
}

/** The set a workspace without one gets on its first upload. */
export const DEFAULT_SET_NAME = "General";

/**
 * The set uploads go to unless another is picked: of the sets people made,
 * General when the workspace has it, else the oldest; "" when it has none.
 * A connector's sets take no uploads, so a workspace with only those has
 * none, and its first upload makes General.
 *
 * @example
 * defaultSetId([{ id: "a", name: "Board" }, { id: "g", name: "General" }]); // "g"
 * defaultSetId([{ id: "n", name: "Notes", connector: { connectorId: "c" } }]); // ""
 */
export function defaultSetId(sets: readonly FoundryDocumentSet[]) {
  const made = sets.filter((s) => !s.connector);
  return (made.find((s) => s.name === DEFAULT_SET_NAME) || made[0])?.id || "";
}

/**
 * A set as the user calls it beside other sets: a connector's is
 * "<connection> / <name>" (FoundryDocumentSet.DisplayName in the WFE).
 *
 * @example
 * setLabel({ id: "n", name: "Notes", connector: { connectorName: "Granola" } }); // "Granola / Notes"
 * setLabel({ id: "g", name: "General" }); // "General"
 */
export const setLabel = (s: FoundryDocumentSet) =>
  [s.connector && (s.connector.connectorName || "A connection"), s.name || s.id]
    .filter(Boolean)
    .join(" / ");

/** A folder of an ontology's documents explorer, and how many of the documents it shows need review. */
export type OntologySetFolder = SetFolder & { review: number };

/**
 * An ontology's documents explorer (the Ontologies panel on its documents
 * pages): the sets ontology `o` reads (their `readBy` names it) or holds
 * documents of (its current load's, from before they moved), each with
 * what it reads of them (`documents`, its documents list), ordered and
 * filtered by a search `term` as the Data tree's documents/ (`setFolders`),
 * and how many of those shown need review.
 *
 * @example
 * ontologySetFolders(
 *   "o1",
 *   [
 *     { id: "g", name: "General", readBy: [{ ontologyId: "o1" }] },
 *     { id: "b", name: "Board" },
 *   ],
 *   [{ id: "d", setId: "g", extraction: { status: "extracted", reviews: 1 } }],
 * ).map((f) => [f.set.id, f.documents.length, f.review]);
 * // [["g", 1, 1]]
 */
export function ontologySetFolders(
  o: string,
  sets: readonly FoundryDocumentSet[],
  documents: readonly FoundryDocument[],
  term = "",
): OntologySetFolder[] {
  const read = sets.filter(
    (s) =>
      s.readBy?.some((r) => r.ontologyId === o) ||
      documents.some((d) => d.setId === s.id),
  );
  return setFolders(read, documents, term).map((f) => ({
    ...f,
    review: f.documents.filter((d) => documentDot(d).status === "review")
      .length,
  }));
}

// ---------------------------------------------------------------- Viewers

/**
 * Whether a viewer reads the document's bytes (`:bytes`: a PDF, a Parquet
 * table, a type this page doesn't know) rather than its text (`:content`,
 * which the WFE serves for text types only).
 *
 * @example
 * readsBytes({ kind: "table", source: "parquet" }); // true
 * readsBytes({ kind: "table", source: "csv" }); // false
 */
export const readsBytes = (v: DocumentViewer) =>
  v.kind === "pdf" ||
  v.kind === "unknown" ||
  (v.kind === "table" && v.source === "parquet");

/**
 * What a viewer's Rendered | Source toggle calls its rendered view, or
 * undefined when it has none: plain text is its own source, and a PDF's or
 * a Parquet file's are bytes.
 *
 * @example
 * renderedLabel({ kind: "json" }); // "Tree"
 * renderedLabel({ kind: "pdf" }); // undefined
 */
export function renderedLabel(v: DocumentViewer) {
  if (v.kind === "markdown") return "Rendered";
  if (v.kind === "json") return "Tree";
  if (v.kind === "table" && v.source !== "parquet") return "Table";
  return undefined;
}

/**
 * A JSON document's value, or why it isn't JSON.
 *
 * @example
 * readJson('{"a":1}'); // { ok: true, value: { a: 1 } }
 * readJson("{").ok; // false
 */
export function readJson(
  text: string,
): { ok: true; value: unknown } | { ok: false; error: string } {
  try {
    return { ok: true, value: JSON.parse(text) };
  } catch (e) {
    return { ok: false, error: (e as Error).message };
  }
}

/**
 * Bytes from base64, as grpc-gateway writes a `bytes` field (standard
 * alphabet, padded); the URL-safe alphabet and missing padding read too.
 *
 * @example
 * [...base64Bytes("JVBERi0=")]; // [37, 80, 68, 70, 45]: "%PDF-"
 */
export function base64Bytes(data: string): Uint8Array<ArrayBuffer> {
  const binary = atob(data.replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(binary, (c) => c.charCodeAt(0));
}

/**
 * Whether bytes are a PDF: they start with "%PDF-", as every PDF does.
 *
 * @example
 * isPdf(new TextEncoder().encode("%PDF-1.7")); // true
 */
export const isPdf = (bytes: Uint8Array) =>
  bytes.length >= 5 && String.fromCharCode(...bytes.subarray(0, 5)) === "%PDF-";

/** The most rows a table's preview shows: its first. */
export const PREVIEW_ROWS = 5000;

/**
 * A table document's preview: its first PREVIEW_ROWS rows, their columns in
 * the order they first appear, how many rows were read in all (`total`),
 * and the rows that couldn't be (`failed`, by number: a CSV or TSV row
 * counting the header as 1, a JSONL line).
 */
export type TablePreview = {
  columns: string[];
  rows: Record<string, unknown>[];
  total: number;
  failed: number[];
};

const preview = (
  rows: Record<string, unknown>[],
  failed: number[] = [],
): TablePreview => {
  const shown = rows.slice(0, PREVIEW_ROWS);
  return {
    columns: [...new Set(shown.flatMap((r) => Object.keys(r)))],
    rows: shown,
    total: rows.length,
    failed,
  };
};

/**
 * A CSV or TSV document's records (as papaparse splits them, the header
 * first) as a preview. A blank header is named by its position and a
 * repeated one numbered, so every column shows. A record papaparse
 * couldn't read (`bad`, by index), or with more or fewer fields than the
 * header, failed.
 *
 * @example
 * delimitedPreview([["id", "name", "name"], ["1", "Ada", "L"], ["2"]], []);
 * // { columns: ["id", "name", "name (2)"], rows: [{ id: "1", name: "Ada", "name (2)": "L" }], total: 1, failed: [3] }
 */
export function delimitedPreview(
  records: readonly string[][],
  bad: readonly number[],
): TablePreview {
  const [head = [], ...body] = records;
  const seen = new Map<string, number>();
  const header = head.map((h, i) => {
    const name = h.trim() || `column ${i + 1}`,
      n = (seen.get(name) || 0) + 1;
    seen.set(name, n);
    return n === 1 ? name : `${name} (${n})`;
  });
  const unread = new Set(bad),
    failed: number[] = [];
  const rows = body.flatMap((r, i) => {
    if (unread.has(i + 1) || r.length !== header.length) {
      failed.push(i + 2);
      return [];
    }
    return [Object.fromEntries(header.map((h, j) => [h, r[j]]))];
  });
  return { ...preview(rows, failed), columns: header };
}

/**
 * A JSONL document as a preview: each line that isn't blank one JSON value,
 * an object's keys its columns (any other value under `value`). A line that
 * isn't JSON failed.
 *
 * @example
 * jsonlPreview('{"id":1}\n\nnope\n[2]');
 * // { columns: ["id", "value"], rows: [{ id: 1 }, { value: [2] }], total: 2, failed: [3] }
 */
export function jsonlPreview(text: string): TablePreview {
  const failed: number[] = [];
  const rows = text.split(/\r?\n/).flatMap((line, i) => {
    if (!line.trim()) return [];
    try {
      const v: unknown = JSON.parse(line);
      return [
        v && typeof v === "object" && !Array.isArray(v)
          ? (v as Record<string, unknown>)
          : { value: v },
      ];
    } catch {
      failed.push(i + 1);
      return [];
    }
  });
  return preview(rows, failed);
}

/**
 * A Parquet value as the grid shows it: a bigint stays exact (the grid
 * formats it), a timestamp is ISO text and bytes are hex; within a list or
 * a struct, a bigint is its digits, so the cell is JSON.
 *
 * @example
 * parquetCell({ at: new Date(0), n: 2n }); // { at: "1970-01-01T00:00:00.000Z", n: "2" }
 */
export function parquetCell(v: unknown, nested = false): unknown {
  if (typeof v === "bigint") return nested ? v.toString() : v;
  if (v instanceof Date) return v.toISOString();
  if (v instanceof Uint8Array)
    return [...v].map((b) => b.toString(16).padStart(2, "0")).join("");
  if (Array.isArray(v)) return v.map((x) => parquetCell(x, true));
  if (v && typeof v === "object")
    return Object.fromEntries(
      Object.entries(v).map(([k, x]) => [k, parquetCell(x, true)]),
    );
  return v;
}

/**
 * A Parquet file's first rows (as read, at most PREVIEW_ROWS) as a preview,
 * with how many rows its metadata says it holds.
 *
 * @example
 * parquetPreview([{ id: 1n }], 12_000);
 * // { columns: ["id"], rows: [{ id: 1n }], total: 12000, failed: [] }
 */
export function parquetPreview(
  rows: readonly Record<string, unknown>[],
  total: number,
): TablePreview {
  const read = rows.map((r) =>
    Object.fromEntries(Object.entries(r).map(([k, v]) => [k, parquetCell(v)])),
  );
  return { ...preview(read), total: Math.max(total, read.length) };
}

/**
 * What a preview holds, in a sentence: its rows, the first of how many when
 * it shows only some, and the rows (lines, for JSONL) that couldn't be read.
 *
 * @example
 * previewText({ columns: [], rows: new Array(5000), total: 12345, failed: [4, 9] }, "csv");
 * // "The first 5,000 of 12,345 rows. Rows 4 and 9 couldn’t be read."
 */
export function previewText(p: TablePreview, source: TableSource) {
  const n = (x: number) => x.toLocaleString("en-US");
  const shown =
    p.rows.length < p.total
      ? `The first ${n(p.rows.length)} of ${n(p.total)} rows.`
      : `${n(p.total)} ${p.total === 1 ? "row" : "rows"}.`;
  if (!p.failed.length) return shown;
  const unit = source === "jsonl" ? "Line" : "Row",
    listed = p.failed.slice(0, 5).map(n),
    more = p.failed.length - listed.length;
  return `${shown} ${p.failed.length === 1 ? unit : `${unit}s`} ${andList(more ? [...listed, `${n(more)} more`] : listed)} couldn’t be read.`;
}

/** The most children a JSON node shows at once; more are a click away. */
export const JSON_PAGE = 200;

/**
 * A JSON value's children, as its tree shows them: an object's entries, an
 * array's items by index; none for a scalar.
 *
 * @example
 * jsonChildren({ a: 1, b: [2] }); // [["a", 1], ["b", [2]]]
 * jsonChildren(["x"]); // [["0", "x"]]
 */
export const jsonChildren = (v: unknown): [string, unknown][] =>
  v && typeof v === "object" ? Object.entries(v) : [];

/**
 * A JSON value in a few words, as a collapsed node shows it: an object by
 * its keys, an array by its items, a scalar as JSON.
 *
 * @example
 * [jsonSummary({ a: 1 }), jsonSummary([1, 2]), jsonSummary("hi"), jsonSummary(null)];
 * // ["{1 key}", "[2 items]", '"hi"', "null"]
 */
export function jsonSummary(v: unknown) {
  if (Array.isArray(v))
    return `[${v.length} ${v.length === 1 ? "item" : "items"}]`;
  if (v && typeof v === "object") {
    const keys = Object.keys(v).length;
    return `{${keys} ${keys === 1 ? "key" : "keys"}}`;
  }
  return JSON.stringify(v);
}

/** The size of a file in bytes, KiB or MiB. */
export function formatBytes(n: number | string | undefined) {
  const b = Number(n || 0);
  return b < 1024
    ? `${b} B`
    : b < 1024 * 1024
      ? `${(b / 1024).toFixed(b < 10 * 1024 ? 1 : 0)} KiB`
      : `${(b / 1024 / 1024).toFixed(1)} MiB`;
}

/** Millionths of a dollar as dollars: "$0.02", "<$0.01" or "$0". */
export function formatUsd(micros: number | string | undefined) {
  const usd = Number(micros || 0) / 1e6;
  return usd === 0 ? "$0" : usd < 0.01 ? "<$0.01" : `$${usd.toFixed(2)}`;
}

// ---------------------------------------------------------------- Highlights

/**
 * Maps UTF-8 byte offsets of `text` to UTF-16 indexes (JavaScript string
 * positions). An offset inside a character, or past the end, is undefined.
 *
 * @example
 * const at = utf16At("€5"); // € is 3 bytes, 1 UTF-16 unit
 * [at(0), at(3), at(4), at(1)]; // [0, 1, 2, undefined]
 */
export function utf16At(text: string): (byte: number) => number | undefined {
  const starts = new Map<number, number>();
  let byte = 0,
    index = 0;
  for (const ch of text) {
    starts.set(byte, index);
    const point = ch.codePointAt(0)!;
    byte += point < 0x80 ? 1 : point < 0x800 ? 2 : point < 0x10000 ? 3 : 4;
    index += ch.length;
  }
  starts.set(byte, index);
  return (b) => starts.get(b);
}

/**
 * The UTF-16 range of a UTF-8 byte range of `text`. When `quote` is given the
 * range must hold it; if it doesn't (offsets from other text), the quote's
 * first occurrence is used instead. Undefined when neither works.
 *
 * @example
 * spanOf("Paid €5 to Globex", 13, 19); // { from: 11, to: 17 }
 */
export function spanOf(
  text: string,
  start: number | undefined,
  end: number | undefined,
  quote?: string,
  at = utf16At(text),
): { from: number; to: number } | undefined {
  const from = at(start ?? -1),
    to = at(end ?? -1);
  if (
    from !== undefined &&
    to !== undefined &&
    from < to &&
    (quote === undefined || text.slice(from, to) === quote)
  )
    return { from, to };
  const found = quote ? text.indexOf(quote) : -1;
  return found >= 0 ? { from: found, to: found + quote!.length } : undefined;
}

/**
 * An MDX document's sections as it was written. A load reads MDX with its
 * code (`import`/`export` blocks, `{…}` expressions) overwritten by spaces
 * of the same UTF-8 length, so each section's byte range (`start`, `end`)
 * of the original holds the text the load stored with the code back in,
 * and every mark's offsets into it still fall on the same characters. A
 * section whose range doesn't fit the original, or whose stored text
 * differs from it anywhere but a blanked space, or that cuts a character,
 * keeps the text the load stored.
 *
 * @example
 * const original = "Pay {props.amount} to Globex";
 * originalSections([{ text: "Pay                to Globex", start: 0, end: 28 }], original)[0].text;
 * // "Pay {props.amount} to Globex"
 */
export function originalSections<
  S extends { text: string; start?: number; end?: number },
>(sections: readonly S[], original: string): S[] {
  const bytes = new TextEncoder().encode(original),
    decoder = new TextDecoder("utf-8", { fatal: true });
  return sections.map((s) => {
    const { start, end } = s;
    if (start === undefined || end === undefined) return s;
    const stored = new TextEncoder().encode(s.text);
    if (start < 0 || end > bytes.length || end - start !== stored.length)
      return s;
    const slice = bytes.subarray(start, end);
    // A space the load stored may be blanked code; anything else is the
    // original's own.
    if (stored.some((b, i) => b !== 0x20 && b !== slice[i])) return s;
    try {
      return { ...s, text: decoder.decode(slice) };
    } catch {
      return s;
    }
  });
}

/**
 * The first and last page of each section of a PDF's text. Foundry
 * separates a PDF's pages with a form feed (`\f`, one byte in UTF-8), so a
 * section starting at byte `start` of the whole `text` (`:content`) starts
 * on the page after the form feeds before it, and ends after its own. The
 * space between two sections (where a form feed may be) isn't theirs, so
 * without the text, or with a `start` that doesn't fit it, a section starts
 * on the page the one before ends on. Form feeds are few (one a page), so
 * each section counts them all.
 *
 * @example
 * sectionPages([{ text: "One", start: 0 }, { text: "Two\fThree", start: 4 }], "One\fTwo\fThree");
 * // [{ first: 1, last: 1 }, { first: 2, last: 3 }]
 * sectionPages([{ text: "One\fTwo" }, { text: "Three" }]);
 * // [{ first: 1, last: 2 }, { first: 2, last: 2 }]
 */
export function sectionPages(
  sections: readonly { text: string; start?: number }[],
  text?: string,
): { first: number; last: number }[] {
  const bytes = text === undefined ? undefined : new TextEncoder().encode(text),
    feeds: number[] = [];
  bytes?.forEach((b, i) => b === 0x0c && feeds.push(i));
  let page = 1;
  return sections.map(({ text: t, start }) => {
    if (bytes && start !== undefined && start >= 0 && start <= bytes.length)
      page = 1 + feeds.filter((f) => f < start).length;
    const first = page;
    page += t.split("\f").length - 1;
    return { first, last: page };
  });
}

/** A run of text and the marks that cover all of it. */
export type Segment<M> = { from: number; to: number; text: string; marks: M[] };

/**
 * Cuts `text` at every mark's ends, so overlapping marks become runs that
 * each carry every mark covering them. Marks are UTF-16 ranges; empty or
 * out-of-range ones are dropped. A section holds at most 4000 bytes and a
 * few dozen marks, so each run checks every mark (O(runs × marks)).
 *
 * @example
 * segments("abcdef", [{ from: 1, to: 4 }, { from: 2, to: 6 }]).map((s) => [s.text, s.marks.length]);
 * // [["a", 0], ["b", 1], ["cd", 2], ["ef", 1]]
 */
export function segments<M extends { from: number; to: number }>(
  text: string,
  marks: M[],
): Segment<M>[] {
  const valid = marks.filter(
    (m) => m.from >= 0 && m.to <= text.length && m.from < m.to,
  );
  const cuts = [
    ...new Set([0, text.length, ...valid.flatMap((m) => [m.from, m.to])]),
  ].sort((a, b) => a - b);
  return cuts.slice(0, -1).flatMap((from, i) => {
    const to = cuts[i + 1]!;
    return from === to
      ? []
      : [
          {
            from,
            to,
            text: text.slice(from, to),
            marks: valid.filter((m) => m.from <= from && m.to >= to),
          },
        ];
  });
}

/** The anchor of a span in a document's text: "s2-118". */
export const spanAnchor = (
  section: number | undefined,
  start: number | undefined,
) => `s${section ?? 0}-${start ?? 0}`;

/**
 * The span a link's hash asks for, `s{section}-{start}-{end}` in UTF-8 bytes
 * of the section (paths.document's `span`), or undefined for any other hash.
 *
 * @example
 * spanTarget("s2-10-40"); // { section: 2, start: 10, end: 40 }
 * spanTarget("s2-10"); // undefined: a mark's own anchor
 */
export function spanTarget(hash: string): Span | undefined {
  const m = /^s(\d+)-(\d+)-(\d+)$/.exec(hash);
  if (!m) return undefined;
  const [section, start, end] = m.slice(1).map(Number) as [
    number,
    number,
    number,
  ];
  return start < end ? { section, start, end } : undefined;
}

/** Where a click on a marked run of a document's text goes. */
export type MarkTarget =
  | { kind: "entity"; id: string }
  // The fact's item in the Facts tab.
  | { kind: "fact"; factKey: string }
  // The review's card in the Needs review tab, by its index in the document's reviews.
  | { kind: "review"; review: string }
  | { kind: "none" };

/**
 * Where a click on a marked run goes, by its most specific mark: a name
 * opens its entity; a quote, its fact's entity (the from end, else the to
 * end), or the fact itself while the facts load (`facts` undefined) or when
 * it joins no entity; a review, its card. Each of the run's lists is
 * innermost mark first. A run inside a link of the document, or marked only
 * as the span a link points at, goes nowhere: a click there is the text's.
 *
 * @example
 * markTarget({ entities: ["e1"], facts: ["f1"], reviews: [] }, undefined); // { kind: "entity", id: "e1" }
 * markTarget({ entities: [], facts: ["f1"], reviews: [] }, [{ factKey: "f1", to: { id: "e2" } }]); // { kind: "entity", id: "e2" }
 * markTarget({ entities: [], facts: ["f1"], reviews: ["0"] }, undefined); // { kind: "fact", factKey: "f1" }
 * markTarget({ entities: [], facts: [], reviews: ["0"] }, []); // { kind: "review", review: "0" }
 * markTarget({ entities: ["e1"], facts: [], reviews: [], inLink: true }, []); // { kind: "none" }
 */
export function markTarget(
  run: {
    entities: readonly string[];
    facts: readonly string[];
    reviews: readonly string[];
    inLink?: boolean;
  },
  facts:
    | readonly { factKey: string; from?: { id: string }; to?: { id: string } }[]
    | undefined,
): MarkTarget {
  if (run.inLink) return { kind: "none" };
  const [entity] = run.entities;
  if (entity) return { kind: "entity", id: entity };
  const [factKey] = run.facts;
  if (factKey) {
    const fact = facts?.find((f) => f.factKey === factKey);
    const end = fact?.from ?? fact?.to;
    return end ? { kind: "entity", id: end.id } : { kind: "fact", factKey };
  }
  const [review] = run.reviews;
  return review ? { kind: "review", review } : { kind: "none" };
}

/** Why a load kept something for a person to decide (the engine's review.reason). */
export type ReviewReason =
  | "unresolved"
  | "ambiguous"
  | "collision"
  | "low_confidence"
  | "unverified"
  | "conflict"
  | "violation"
  | "unmatched"
  | "prop_conflict"
  | "attribute_low_confidence"
  | "identifier_conflict"
  | "adjudicated";

const reviews: Record<ReviewReason, { title: string; text: string }> = {
  unresolved: {
    title: "Names something that isn’t in your data",
    text: "No entity has this name, and text never creates one, so nothing was stored about it.",
  },
  ambiguous: {
    title: "Could mean more than one entity",
    text: "The name matches several entities, so the fact wasn’t stored on any of them.",
  },
  collision: {
    title: "Two names here match one entity",
    text: "Different names in the text resolve to the same entity, so neither was stored.",
  },
  low_confidence: {
    title: "The model wasn’t sure",
    text: "The fact was read with low confidence, so it wasn’t stored.",
  },
  unverified: {
    title: "Couldn’t be checked",
    text: "A second model couldn’t confirm the quote says this.",
  },
  conflict: {
    title: "Disagrees with your data",
    text: "The text says something different from what the tables say.",
  },
  violation: {
    title: "Would break a rule",
    text: "Storing it would break one of the ontology’s rules.",
  },
  unmatched: {
    title: "A relationship the ontology doesn’t have",
    text: "The text describes a link the current ontology can’t store. Generating a new version can add it.",
  },
  adjudicated: {
    title: "Matched by the model",
    text: "The model linked this name to an entity. Undo the link if it’s wrong; the next load leaves the name unlinked.",
  },
  prop_conflict: {
    title: "Disagrees on a value",
    text: "Documents state different values for this property, or one the tables don’t hold, so the stored value is the table’s or none.",
  },
  attribute_low_confidence: {
    title: "The model wasn’t sure of a value",
    text: "A value was read with low confidence, so it wasn’t stored.",
  },
  identifier_conflict: {
    title: "Its identifiers point at different entities",
    text: "The text quotes identifiers of more than one entity for one name, so it wasn’t linked to any.",
  },
};
/** A review's reason as a title and a sentence for people who didn't build the ontology. */
export const reviewCopy = (reason?: string) =>
  reviews[(reason || "unverified") as ReviewReason] || reviews.unverified;
