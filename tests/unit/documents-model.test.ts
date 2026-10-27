import { describe, expect, it } from "vitest";
import type {
  DocumentRead,
  FoundryDocument,
  FoundryFileReadState,
  GraphJob,
} from "@/shared/wfe";
import {
  DOCUMENT_FORMATS,
  FORMAT_NAMES,
  JSON_PAGE,
  PREVIEW_ROWS,
  andList,
  base64Bytes,
  defaultSetId,
  delimitedPreview,
  documentBadge,
  documentDot,
  documentNameProblem,
  documentViewer,
  extractionBadge,
  fileRead,
  fileReadLabel,
  formatOf,
  formatOfName,
  isPdf,
  jsonChildren,
  jsonSummary,
  jsonlPreview,
  kindText,
  parquetCell,
  parquetPreview,
  previewText,
  readJson,
  readKindOf,
  readsAsText,
  readsBytes,
  renderedLabel,
  sectionPages,
  ontologyReadText,
  ontologySetFolders,
  readersText,
  readOutcome,
  readsText,
  liveRead,
  MAX_SET_NAME,
  setLabel,
  setNameProblem,
  failureText,
  fileProblem,
  formatBytes,
  formatUsd,
  hex,
  putFailure,
  putHeaders,
  markTarget,
  originalSections,
  reviewCopy,
  segments,
  spanAnchor,
  spanTarget,
  spanOf,
  uploadText,
  utf16At,
} from "@/web/areas/documents/model";
import { ontologyPaths } from "@/web/areas/ontology/paths";

// The documents' logic: what may be uploaded and how a document or set may
// be named, an upload's steps and what the ontologies did with it, a
// document's state, and its text's highlights: UTF-8 byte offsets mapped to
// JavaScript's UTF-16 strings, and overlapping marks cut into runs.

const bytes = (s: string) => new TextEncoder().encode(s).length;

describe("uploads", () => {
  it("maps each extension the WFE takes to its media type, in any case", () => {
    const type = (name: string) => formatOfName(name)?.mediaType;
    expect(
      Object.fromEntries(
        [
          "contract.md",
          "NOTES.MARKDOWN",
          "guide.MDX",
          "notes.txt",
          "pricing.json",
          "Q3.CSV",
          "q3.tsv",
          "events.jsonl",
          "events.ndjson",
          "orders.parquet",
          "deck.pdf",
        ].map((n) => [n, type(n)]),
      ),
    ).toEqual({
      "contract.md": "text/markdown",
      "NOTES.MARKDOWN": "text/markdown",
      "guide.MDX": "text/mdx",
      "notes.txt": "text/plain",
      "pricing.json": "application/json",
      "Q3.CSV": "text/csv",
      "q3.tsv": "text/tab-separated-values",
      "events.jsonl": "application/x-ndjson",
      "events.ndjson": "application/x-ndjson",
      "orders.parquet": "application/vnd.apache.parquet",
      "deck.pdf": "application/pdf",
    });
    for (const name of [
      "notes.text",
      "deck.docx",
      "md",
      "csv",
      "a.csv.gz",
      "a.",
    ])
      expect(type(name), name).toBeUndefined();
    // One format a media type, and one an extension.
    const types = DOCUMENT_FORMATS.map((f) => f.mediaType),
      extensions = DOCUMENT_FORMATS.flatMap((f) => f.extensions);
    expect(new Set(types).size).toBe(9);
    expect(new Set(extensions).size).toBe(extensions.length);
    expect(FORMAT_NAMES).toBe(
      "Markdown, MDX, text, JSON, CSV, TSV, JSONL, Parquet, or PDF",
    );
  });

  it("takes text up to 1 MiB and table files and PDFs up to 3 MiB", () => {
    expect(fileProblem({ name: "a.md", size: 1 })).toBeUndefined();
    expect(fileProblem({ name: "a.md", size: 1_048_576 })).toBeUndefined();
    expect(fileProblem({ name: "a.mdx", size: 1_048_577 })).toBe(
      "MDX files hold at most 1.0 MiB. Split it into smaller documents.",
    );
    expect(fileProblem({ name: "a.txt", size: 1_048_577 })).toBe(
      "Text files hold at most 1.0 MiB. Split it into smaller documents.",
    );
    expect(fileProblem({ name: "a.pdf", size: 3_145_728 })).toBeUndefined();
    expect(fileProblem({ name: "a.parquet", size: 3_145_729 })).toBe(
      "Parquet files hold at most 3.0 MiB. Split it into smaller files.",
    );
    expect(fileProblem({ name: "a.csv", size: 0 })).toBe("The file is empty.");
    expect(fileProblem({ name: "deck.docx", size: 10 })).toBe(
      "Only Markdown, MDX, text, JSON, CSV, TSV, JSONL, Parquet, or PDF files can be uploaded.",
    );
  });

  it("picks a document's viewer from its media type, never its name or text", () => {
    expect(
      Object.fromEntries(
        DOCUMENT_FORMATS.map((f) => [f.mediaType, documentViewer(f.mediaType)]),
      ),
    ).toEqual({
      "text/markdown": { kind: "markdown", mdx: false },
      "text/mdx": { kind: "markdown", mdx: true },
      "text/plain": { kind: "plain" },
      "application/json": { kind: "json" },
      "text/csv": { kind: "table", source: "csv" },
      "text/tab-separated-values": { kind: "table", source: "tsv" },
      "application/x-ndjson": { kind: "table", source: "jsonl" },
      "application/vnd.apache.parquet": { kind: "table", source: "parquet" },
      "application/pdf": { kind: "pdf" },
    });
    // A type the page doesn't know, or none, is text with a download.
    expect(documentViewer("image/png")).toEqual({ kind: "unknown" });
    expect(documentViewer(undefined)).toEqual({ kind: "unknown" });
    expect(formatOf("application/pdf")?.name).toBe("PDF");
    expect(formatOf("image/png")).toBeUndefined();
  });

  it("reads the text types' text and the others' bytes", () => {
    const bytes = DOCUMENT_FORMATS.filter((f) => readsBytes(f.viewer)).map(
      (f) => f.mediaType,
    );
    expect(bytes).toEqual([
      "application/vnd.apache.parquet",
      "application/pdf",
    ]);
    expect(readsBytes({ kind: "unknown" })).toBe(true);
    expect(readsBytes({ kind: "table", source: "csv" })).toBe(false);
  });

  it("offers a source view only where there is text to render", () => {
    expect(
      DOCUMENT_FORMATS.map((f) => [f.name, renderedLabel(f.viewer)]),
    ).toEqual([
      ["Markdown", "Rendered"],
      ["MDX", "Rendered"],
      ["text", undefined],
      ["JSON", "Tree"],
      ["CSV", "Table"],
      ["TSV", "Table"],
      ["JSONL", "Table"],
      ["Parquet", undefined],
      ["PDF", undefined],
    ]);
    expect(renderedLabel({ kind: "unknown" })).toBeUndefined();
  });

  it("says how ontologies read each format", () => {
    expect(
      DOCUMENT_FORMATS.filter((f) => f.kind === "text").map((f) => f.mediaType),
    ).toEqual(["text/markdown", "text/mdx", "text/plain"]);
    expect(
      DOCUMENT_FORMATS.filter((f) => f.kind === "table").map((f) => f.name),
    ).toEqual(["JSON", "CSV", "TSV", "JSONL", "Parquet"]);
    expect(
      DOCUMENT_FORMATS.filter((f) => f.kind === "pdf").map((f) => f.name),
    ).toEqual(["PDF"]);
    // As text: text always, a PDF once its text is read, a table file never.
    const doc = (
      mediaType: string,
      state?: FoundryFileReadState,
    ): FoundryDocument => ({
      id: "d",
      status: "uploaded",
      mediaType,
      read: state && { state },
    });
    expect(readsAsText(doc("text/plain"))).toBe(true);
    expect(
      readsAsText(doc("application/pdf", "FOUNDRY_FILE_READ_STATE_READY")),
    ).toBe(true);
    for (const state of [
      "FOUNDRY_FILE_READ_STATE_READING",
      "FOUNDRY_FILE_READ_STATE_FAILED",
    ] as const)
      expect(readsAsText(doc("application/pdf", state))).toBe(false);
    expect(readsAsText(doc("application/pdf"))).toBe(false);
    expect(readsAsText(doc("text/csv", "FOUNDRY_FILE_READ_STATE_READY"))).toBe(
      false,
    );
    expect(readsAsText(doc("image/png"))).toBe(false);
  });

  it("refuses names the WFE refuses for good, as it counts them", () => {
    // Trimmed first, as the name is sent.
    expect(fileProblem({ name: " a.md ", size: 1 })).toBeUndefined();
    // 255 code points, emoji among them, are fine; 256 are not.
    expect(
      fileProblem({ name: `${"\u{1F600}".repeat(252)}.md`, size: 1 }),
    ).toBeUndefined();
    expect(fileProblem({ name: `${"a".repeat(253)}.md`, size: 1 })).toMatch(
      /longer than 255/,
    );
    for (const name of [
      "a/b.md",
      "a\\b.md",
      "a\tb.md",
      "a\u0085b.md",
      "a\u007fb.md",
    ])
      expect(fileProblem({ name, size: 1 }), name).toMatch(/slash/);
    expect(
      fileProblem({ name: "caf\u00e9 \u2014 notes.md", size: 1 }),
    ).toBeUndefined();
  });

  it("checks a new name for a document as the WFE does", () => {
    expect(documentNameProblem("  ")).toBe("Name the document.");
    // Trimmed, and its extension may change.
    expect(documentNameProblem(" Board minutes ")).toBeUndefined();
    expect(documentNameProblem("😀".repeat(255))).toBeUndefined();
    expect(documentNameProblem("a".repeat(256))).toBe(
      "A document’s name is at most 255 characters.",
    );
    for (const name of ["Q3/notes.md", "a\\b", "a\u0007b"])
      expect(documentNameProblem(name), name).toMatch(/slash/);
  });

  it("writes a SHA-256 as lowercase hex", () => {
    expect(hex(new Uint8Array([0, 15, 171, 255]).buffer)).toBe("000fabff");
  });

  it("says where each upload is", () => {
    expect(uploadText({ step: "checking" })).toBe("Preparing…");
    expect(uploadText({ step: "uploading", loaded: 512, total: 2048 })).toBe(
      "Uploading… 25%",
    );
    expect(uploadText({ step: "uploading", loaded: 0, total: 0 })).toBe(
      "Uploading… 0%",
    );
    expect(uploadText({ step: "completing" })).toBe("Finishing…");
    const document = { id: "d" };
    expect(
      uploadText({
        step: "done",
        document,
        result: { kind: "uploaded", reads: [] },
      }),
    ).toBe("Uploaded");
    expect(
      uploadText({ step: "done", document, result: { kind: "existing" } }),
    ).toBe("Already uploaded");
    expect(
      uploadText({ step: "done", document, result: { kind: "finished" } }),
    ).toBe("Uploaded");
    expect(uploadText({ step: "failed", error: "Nope.", retry: true })).toBe(
      "Nope.",
    );
  });

  it("explains a refused PUT and sends only headers a script may set", () => {
    expect(putFailure(0, "")).toMatch(/interrupted/);
    expect(putFailure(403, "<Message>Request has expired</Message>")).toBe(
      "The upload link expired. Try again.",
    );
    expect(putFailure(503, "<Code>SlowDown</Code>")).toMatch(/busy/);
    expect(putFailure(403, "<Code>SignatureDoesNotMatch</Code>")).toBe(
      "Storage refused the upload (403). Try again.",
    );
    expect(
      putHeaders({
        "Content-Type": "text/plain",
        "Content-Length": "4",
        "x-amz-checksum-sha256": "q",
        Host: "h",
      }),
    ).toEqual([
      ["Content-Type", "text/plain"],
      ["x-amz-checksum-sha256", "q"],
    ]);
    expect(putHeaders()).toEqual([]);
  });
});

describe("document states", () => {
  const doc = (d: Partial<FoundryDocument>): FoundryDocument => ({
    id: "d",
    status: "uploaded",
    ...d,
  });
  const job = (j: Partial<GraphJob>): GraphJob => ({
    id: "j",
    status: "running",
    phase: "documents",
    ...j,
  });
  const badge = (d: Partial<FoundryDocument>, live: GraphJob | null = null) =>
    documentBadge(doc(d), live);

  it("waits for a load, then for the reading", () => {
    expect(badge({ status: "pending_upload" })).toEqual({
      status: "queued",
      label: "Uploading",
    });
    expect(badge({ status: "deleted" })).toEqual({
      status: "draft",
      label: "Deleted",
    });
    expect(badge({})).toEqual({
      status: "queued",
      label: "Waiting for a load",
    });
    expect(badge({}, job({ status: "queued" }))).toEqual({
      status: "queued",
      label: "Queued for the load",
    });
    expect(badge({}, job({ phase: "ingest" }))).toEqual({
      status: "queued",
      label: "Queued for the load",
    });
    expect(badge({}, job({}))).toEqual({
      status: "running",
      label: "Reading",
    });
    // A finished load is not live.
    expect(badge({}, job({ status: "succeeded" }))).toEqual({
      status: "queued",
      label: "Waiting for a load",
    });
  });

  it("waits for a PDF's text to be read, and says when it couldn't be", () => {
    const pdf = (
      state: FoundryFileReadState,
      d: Partial<FoundryDocument> = {},
    ) =>
      badge({ mediaType: "application/pdf", read: { state }, ...d }, job({}));
    expect(pdf("FOUNDRY_FILE_READ_STATE_READING")).toEqual({
      status: "queued",
      label: "Reading its text",
    });
    expect(pdf("FOUNDRY_FILE_READ_STATE_FAILED")).toEqual({
      status: "failed",
      label: "Couldn’t be read",
    });
    // Once its text is read, a load reads it as any text.
    expect(pdf("FOUNDRY_FILE_READ_STATE_READY")).toEqual({
      status: "running",
      label: "Reading",
    });
    expect(
      pdf("FOUNDRY_FILE_READ_STATE_FAILED", {
        extraction: { status: "failed", error: { code: "unsupported" } },
      }),
    ).toEqual({ status: "failed", label: "Couldn’t be read" });
  });

  it("numbers the pages a PDF's sections are on", () => {
    const text = "Cover\fTerms one\n\nTerms two\f\fÉtat\f";
    const at = (part: string) => bytes(text.slice(0, text.indexOf(part)));
    // A section's start counts the breaks before it, even between sections.
    expect(
      sectionPages(
        [
          { text: "Cover", start: 0 },
          { text: "Terms one", start: at("Terms one") },
          { text: "Terms two\f\fÉtat", start: at("Terms two") },
        ],
        text,
      ),
    ).toEqual([
      { first: 1, last: 1 },
      { first: 2, last: 2 },
      { first: 2, last: 4 },
    ]);
    // A start the text doesn't hold, or none: where the one before ended.
    expect(
      sectionPages(
        [
          { text: "Cover\f", start: 0 },
          { text: "Terms", start: 999 },
          { text: "More", start: -1 },
        ],
        text,
      ),
    ).toEqual([
      { first: 1, last: 2 },
      { first: 2, last: 2 },
      { first: 2, last: 2 },
    ]);
    // Past a multibyte character, offsets are bytes.
    expect(sectionPages([{ text: "x", start: bytes("É\f") }], "É\fx")).toEqual([
      { first: 2, last: 2 },
    ]);
    expect(sectionPages([], text)).toEqual([]);
  });

  it("says what reading it found", () => {
    expect(badge({ extraction: { status: "extracted", reviews: 0 } })).toEqual({
      status: "succeeded",
      label: "Read",
    });
    // Its set routed nowhere: stored, not read.
    expect(badge({ extraction: { status: "out_of_scope" } })).toEqual({
      status: "canceled",
      label: "Not read by its version",
    });
    expect(badge({ extraction: { status: "extracted", reviews: 2 } })).toEqual({
      status: "review",
      label: "Needs review · 2",
    });
    // What a live load does doesn't change a document it read.
    expect(badge({ extraction: { status: "extracted" } }, job({}))).toEqual({
      status: "succeeded",
      label: "Read",
    });
    expect(
      badge({ extraction: { status: "duplicate", duplicateOf: "d2" } }),
    ).toEqual({ status: "canceled", label: "Duplicate" });
    expect(
      badge({ extraction: { status: "failed", error: { code: "model" } } }),
    ).toEqual({
      status: "failed",
      label: "Couldn’t be read",
    });
    expect(badge({ extraction: { status: "weird" as "failed" } })).toEqual({
      status: "draft",
      label: "Unknown",
    });
  });

  it("dots each document in an ontology's explorer, in four colours", () => {
    const dot = (d: Partial<FoundryDocument>) => documentDot(doc(d));
    // Orange, red and green: what the current load found.
    expect(dot({ extraction: { status: "extracted", reviews: 2 } })).toEqual({
      status: "review",
      label: "Needs review · 2",
    });
    expect(
      dot({ extraction: { status: "failed", error: { code: "model" } } }),
    ).toEqual({ status: "failed", label: "Couldn’t be read" });
    expect(dot({ extraction: { status: "extracted", reviews: 0 } })).toEqual({
      status: "succeeded",
      label: "Read",
    });
    expect(dot({ extraction: { status: "extracted" } })).toEqual({
      status: "succeeded",
      label: "Read",
    });
    // Grey: waiting.
    expect(dot({})).toEqual({ status: "queued", label: "Waiting" });
    expect(dot({ status: "pending_upload" })).toEqual({
      status: "queued",
      label: "Uploading",
    });
    // Grey: not read, whatever the reason.
    expect(dot({ status: "deleted" })).toEqual({
      status: "canceled",
      label: "Deleted",
    });
    expect(dot({ extraction: { status: "duplicate" } })).toEqual({
      status: "canceled",
      label: "Duplicate",
    });
    expect(dot({ extraction: { status: "out_of_scope" } })).toEqual({
      status: "canceled",
      label: "Not read by its version",
    });
    expect(dot({ extraction: { status: "weird" as "failed" } })).toEqual({
      status: "canceled",
      label: "Unknown",
    });
    // A table file loads through mappings, never as text.
    expect(dot({ mediaType: "text/csv" })).toEqual({
      status: "canceled",
      label: "A table file: loads read its rows through mappings",
    });
    // A PDF: grey while its text is read, red with why it couldn't be,
    // before a load and after one; once read, as any text.
    const pdf = (
      state: FoundryFileReadState,
      d: Partial<FoundryDocument> = {},
    ) =>
      dot({
        mediaType: "application/pdf",
        read: {
          state,
          error: {
            code: "unsupported",
            message: "Scanned PDF: no text to read.",
          },
        },
        ...d,
      });
    expect(pdf("FOUNDRY_FILE_READ_STATE_READING")).toEqual({
      status: "queued",
      label: "Reading its text",
    });
    expect(pdf("FOUNDRY_FILE_READ_STATE_FAILED")).toEqual({
      status: "failed",
      label: "Scanned PDF: no text to read.",
    });
    expect(
      pdf("FOUNDRY_FILE_READ_STATE_FAILED", {
        extraction: { status: "failed", error: { code: "unsupported" } },
      }),
    ).toEqual({ status: "failed", label: "Scanned PDF: no text to read." });
    expect(pdf("FOUNDRY_FILE_READ_STATE_READY")).toEqual({
      status: "queued",
      label: "Waiting",
    });
    expect(
      pdf("FOUNDRY_FILE_READ_STATE_READY", {
        extraction: { status: "extracted", reviews: 1 },
      }),
    ).toEqual({ status: "review", label: "Needs review · 1" });
    // A model failure is as any document's.
    expect(
      pdf("FOUNDRY_FILE_READ_STATE_READY", {
        extraction: { status: "failed", error: { code: "model" } },
      }),
    ).toEqual({ status: "failed", label: "Couldn’t be read" });
    // The WFE says so of a type this page doesn't know.
    expect(
      dot({
        mediaType: "image/png",
        extraction: { status: "failed", error: { code: "unsupported" } },
      }),
    ).toEqual({
      status: "canceled",
      label: "Ontologies don’t read its format yet",
    });
    // A document ontologies read waits, even with no media type.
    expect(dot({ mediaType: "text/markdown" }).label).toBe("Waiting");
  });

  it("says what a load made of it, for each ontology", () => {
    expect(extractionBadge({ status: "extracted", reviews: 2 })).toEqual({
      status: "review",
      label: "Needs review · 2",
    });
    expect(extractionBadge({ status: "out_of_scope" })).toEqual({
      status: "canceled",
      label: "Not read by its version",
    });
  });

  it("explains a failure, and when a load retries it", () => {
    expect(failureText({ code: "too_large" })).toMatch(
      /^It names more than one write can store/,
    );
    // A PDF a load couldn't read: why its read failed, or that the next
    // load reads the text an older load skipped.
    const pdf = (read: FoundryDocument["read"]): FoundryDocument => ({
      id: "d",
      status: "uploaded",
      mediaType: "application/pdf",
      read,
    });
    expect(
      failureText(
        { code: "unsupported", retryable: false },
        pdf({
          state: "FOUNDRY_FILE_READ_STATE_FAILED",
          error: {
            code: "unsupported",
            message: "Scanned PDF: no text to read.",
          },
        }),
      ),
    ).toBe("Scanned PDF: no text to read.");
    expect(
      failureText(
        { code: "too_large" },
        pdf({
          state: "FOUNDRY_FILE_READ_STATE_FAILED",
          error: { code: "too_large" },
        }),
      ),
    ).toBe(
      "It is past a limit of what Foundry reads. Split it into smaller files.",
    );
    expect(
      failureText(
        { code: "unsupported" },
        pdf({ state: "FOUNDRY_FILE_READ_STATE_READY" }),
      ),
    ).toBe("Its text wasn’t read when this load ran. The next load reads it.");
    expect(failureText({ code: "unsupported" }, pdf(undefined))).toBe(
      "Foundry couldn’t read its text.",
    );
    // Any other failure of a PDF is any document's.
    expect(
      failureText(
        { code: "model", retryable: true },
        pdf({ state: "FOUNDRY_FILE_READ_STATE_READY" }),
      ),
    ).toBe("The model couldn’t read it. The next load tries again.");
    // A type the WFE doesn't read.
    expect(
      failureText({ code: "unsupported" }, { id: "d", mediaType: "image/png" }),
    ).toBe("Ontologies don’t read its format yet.");
    expect(
      failureText({ code: "model" }, { id: "d", mediaType: "text/csv" }),
    ).toBe("The model couldn’t read it.");
    expect(failureText(undefined)).toBe("The model couldn’t read it.");
    expect(failureText({ code: "newer" as "model" })).toBe(
      "The model couldn’t read it.",
    );
    expect(failureText({ code: "store", retryable: true })).toBe(
      "The database couldn’t store what was read. The next load tries again.",
    );
    expect(failureText(null)).toBe("The model couldn’t read it.");
  });

  it("words every review reason", () => {
    for (const reason of [
      "unresolved",
      "ambiguous",
      "collision",
      "low_confidence",
      "unverified",
      "conflict",
      "violation",
      "unmatched",
    ] as const) {
      const copy = reviewCopy(reason);
      expect(copy.title.length).toBeGreaterThan(5);
      expect(copy.text).toMatch(/\.$/);
    }
    expect(reviewCopy(undefined)).toEqual(reviewCopy("unverified"));
    expect(reviewCopy("something new")).toEqual(reviewCopy("unverified"));
  });

  it("formats sizes and costs", () => {
    expect(formatBytes("512")).toBe("512 B");
    expect(formatBytes(1536)).toBe("1.5 KiB");
    expect(formatBytes(262_144)).toBe("256 KiB");
    expect(formatBytes(3 * 1024 * 1024)).toBe("3.0 MiB");
    expect(formatBytes(undefined)).toBe("0 B");
    expect(formatUsd("0")).toBe("$0");
    expect(formatUsd("2400")).toBe("<$0.01");
    expect(formatUsd(1_234_567)).toBe("$1.23");
  });
});

describe("document sets", () => {
  const sets = [
    { id: "set_b", name: "Board minutes" },
    { id: "set_g", name: "General" },
  ];
  it("checks a set's name as the WFE does", () => {
    expect(setNameProblem("  ", sets)).toBe("Name the set.");
    expect(setNameProblem("x".repeat(MAX_SET_NAME), sets)).toBeUndefined();
    // Characters, not UTF-16 units.
    expect(setNameProblem("😀".repeat(MAX_SET_NAME), sets)).toBeUndefined();
    expect(setNameProblem("x".repeat(MAX_SET_NAME + 1), sets)).toBe(
      "A set’s name is at most 120 characters.",
    );
    // Unique in the workspace, case and accents aside, apart from the set
    // being renamed.
    expect(setNameProblem(" GENERAL ", sets)).toBe(
      "There is a set named General already.",
    );
    expect(setNameProblem("Général", sets)).toBe(
      "There is a set named General already.",
    );
    expect(setNameProblem("general", sets, "set_g")).toBeUndefined();
    expect(setNameProblem("Contracts", sets)).toBeUndefined();
  });

  it("names the ontologies that read a set", () => {
    const name = (id: string) => ({ o1: "Renewals", o2: "Contracts" })[id]!;
    expect(readersText("Board", [], name)).toBe("No ontology reads Board yet.");
    expect(readersText("Board", [{ ontologyId: "o1" }], name)).toBe(
      "Renewals reads Board.",
    );
    expect(
      readersText("Board", [{ ontologyId: "o1" }, { ontologyId: "o2" }], name),
    ).toBe("Renewals and Contracts read Board.");
    expect(andList(["A", "B", "C"])).toBe("A, B, and C");
  });

  it("uploads into General unless it is gone", () => {
    expect(defaultSetId(sets)).toBe("set_g");
    expect(defaultSetId(sets.slice(0, 1))).toBe("set_b");
    expect(defaultSetId([])).toBe("");
  });

  it("never uploads into a connector's set", () => {
    const notes = {
        id: "set_n",
        name: "Notes",
        connector: { connectorId: "con_g", stream: "notes" },
      },
      // A connector's set named General is still the connector's.
      general = {
        id: "set_cg",
        name: "General",
        connector: { connectorId: "con_x", stream: "general" },
      };
    // Only a connector's sets: as a workspace with none, so the first
    // upload makes General.
    expect(defaultSetId([notes])).toBe("");
    expect(defaultSetId([notes, general])).toBe("");
    // The oldest set people made, after a connector's.
    expect(defaultSetId([notes, sets[0]!])).toBe("set_b");
    expect(defaultSetId([general, ...sets])).toBe("set_g");
  });

  it("names a connector's set with its connection", () => {
    expect(
      setLabel({
        id: "set_n",
        name: "Notes",
        connector: { connectorId: "con_g", connectorName: "Granola" },
      }),
    ).toBe("Granola / Notes");
    // A connection without a name yet, and a set without one.
    expect(setLabel({ id: "set_n", connector: { connectorId: "con_g" } })).toBe(
      "A connection / set_n",
    );
    expect(setLabel({ id: "set_g", name: "General" })).toBe("General");
  });

  it("shows an ontology the sets it reads or holds documents of", () => {
    const all = [
      { id: "s1", name: "Contracts", readBy: [{ ontologyId: "o1" }] },
      { id: "s2", name: "Board", readBy: [{ ontologyId: "o2" }] },
      { id: "s3", name: "Moved", readBy: [] },
      { id: "s4", name: "Empty", readBy: [{ ontologyId: "o1" }] },
    ];
    const read = (id: string, setId: string, reviews = 0) => ({
      id,
      name: `${id}.md`,
      setId,
      createdAt: `2026-10-0${id.slice(1)}`,
      extraction: { status: "extracted" as const, reviews },
    });
    const documents = [
      read("d2", "s1", 3),
      read("d1", "s1"),
      read("d3", "s3", 1),
      // A set the sets list doesn't name yet waits for the next read.
      read("d4", "gone"),
    ];
    const shown = (term?: string) =>
      ontologySetFolders("o1", all, documents, term).map((f) => [
        f.set.id,
        f.documents.map((d) => d.id),
        f.review,
      ]);
    // Its sets in the workspace's order, documents oldest first; Board is
    // another ontology's, and Moved holds a document its current load
    // read before it moved.
    expect(shown()).toEqual([
      ["s1", ["d1", "d2"], 1],
      ["s3", ["d3"], 1],
      ["s4", [], 0],
    ]);
    // A search keeps a set by its name, else the documents it finds, and
    // counts what it shows.
    expect(shown("contr")).toEqual([["s1", ["d1", "d2"], 1]]);
    expect(shown("D1")).toEqual([["s1", ["d1"], 0]]);
    expect(shown("nothing")).toEqual([]);
    expect(ontologySetFolders("o1", [], documents)).toEqual([]);
  });
});

describe("highlights", () => {
  it("maps UTF-8 byte offsets to UTF-16 indexes, refusing the middle of a character", () => {
    // é is 2 bytes and 1 unit, € 3 bytes and 1 unit, 😀 4 bytes and 2 units.
    const text = "é€😀a";
    const at = utf16At(text);
    expect([0, 2, 5, 9, 10].map(at)).toEqual([0, 1, 2, 4, 5]);
    expect([1, 3, 4, 6, 7, 8, 11, -1].map(at)).toEqual(
      Array(8).fill(undefined),
    );
    expect(utf16At("")(0)).toBe(0);
  });

  it("finds a span by its offsets, or by its quote when they disagree", () => {
    const text = "Acme (“we”), Globex Corporation and Initech LLC.";
    const start = bytes("Acme (“we”), "),
      end = start + bytes("Globex Corporation");
    const span = spanOf(text, start, end, "Globex Corporation")!;
    expect(text.slice(span.from, span.to)).toBe("Globex Corporation");
    // The UTF-16 index is smaller than the byte offset: the quotes are 3 bytes.
    expect(span.from).toBe(start - 4);
    // Offsets that miss the quote fall back to finding it.
    expect(spanOf(text, 0, 4, "Initech LLC")).toEqual({
      from: text.indexOf("Initech"),
      to: text.indexOf("Initech") + 11,
    });
    // Inside a character, empty, reversed, or out of range, with no quote to find.
    expect(spanOf(text, 7, 9)).toBeUndefined();
    expect(spanOf(text, 4, 4)).toBeUndefined();
    expect(spanOf(text, 9, 4)).toBeUndefined();
    expect(spanOf(text, 0, 999)).toBeUndefined();
    expect(spanOf(text, undefined, undefined)).toBeUndefined();
    expect(spanOf(text, 0, 4, "Hooli")).toBeUndefined();
    // Without a quote, the offsets alone decide.
    expect(spanOf(text, 0, 4)).toEqual({ from: 0, to: 4 });
  });

  it("cuts overlapping marks into runs that carry every mark covering them", () => {
    const runs = segments("Globex signed with Initech", [
      { from: 0, to: 6, id: "mention" },
      { from: 0, to: 26, id: "quote" },
      { from: 19, to: 26, id: "other" },
      { from: 19, to: 26, id: "same" },
    ]);
    expect(runs.map((r) => [r.text, r.marks.map((m) => m.id)])).toEqual([
      ["Globex", ["mention", "quote"]],
      [" signed with ", ["quote"]],
      ["Initech", ["quote", "other", "same"]],
    ]);
    expect(runs.map((r) => [r.from, r.to])).toEqual([
      [0, 6],
      [6, 19],
      [19, 26],
    ]);
  });

  it("keeps unmarked text and drops marks it can't place", () => {
    expect(segments("plain", [])).toEqual([
      { from: 0, to: 5, text: "plain", marks: [] },
    ]);
    expect(
      segments("abc", [
        { from: 2, to: 2 },
        { from: 2, to: 9 },
        { from: -1, to: 1 },
      ]).map((r) => r.text),
    ).toEqual(["abc"]);
    expect(segments("", [])).toEqual([]);
    const nested = segments("abcdef", [
      { from: 1, to: 5 },
      { from: 2, to: 3 },
    ]);
    expect(nested.map((r) => [r.text, r.marks.length])).toEqual([
      ["a", 0],
      ["b", 1],
      ["c", 2],
      ["de", 1],
      ["f", 0],
    ]);
  });

  it("shows an MDX document's sections as written, its marks where they were", () => {
    const original = [
      'import { Callout } from "./callout";',
      "",
      "<Callout>Globex owes {fmt(1200, “€”)} a year.</Callout>",
      "",
      "Initech pays {props.amount}.",
    ].join("\n");
    // As a load read it: its code blanked to spaces of the same UTF-8 length.
    const blanked = original
      .replace(/^import .*$/m, (m) => " ".repeat(bytes(m)))
      .replace(/\{[^}]*\}/g, (m) => " ".repeat(bytes(m)));
    expect(bytes(blanked)).toBe(bytes(original));
    const section = (text: string) => {
      const start = bytes(blanked.slice(0, blanked.indexOf(text)));
      return { text, start, end: start + bytes(text) };
    };
    const first = blanked.split("\n")[2]!,
      second = blanked.split("\n")[4]!;
    const shown = originalSections([section(first), section(second)], original);
    expect(shown.map((s) => s.text)).toEqual([
      "<Callout>Globex owes {fmt(1200, “€”)} a year.</Callout>",
      "Initech pays {props.amount}.",
    ]);
    // A mark's byte offsets into the stored text fall on the same words.
    const at = first.indexOf("a year"),
      start = bytes(first.slice(0, at));
    const span = spanOf(
      shown[0]!.text,
      start,
      start + bytes("a year"),
      "a year",
    );
    expect(shown[0]!.text.slice(span!.from, span!.to)).toBe("a year");
    expect(spanOf(shown[0]!.text, start, start + 6)).toEqual(span);
  });

  it("keeps the stored text of a section the original doesn't hold", () => {
    const original = "Pay {props.amount} to Globex.";
    const stored = "Pay                to Globex.";
    const ok = { text: stored, start: 0, end: bytes(stored) };
    expect(originalSections([ok], original)[0]!.text).toBe(original);
    for (const s of [
      // No range: an older load.
      { text: stored },
      // Past the end, or a range of another length.
      { ...ok, start: 5, end: 5 + bytes(stored) },
      { ...ok, end: bytes(stored) - 1 },
      // Text that isn't the original's where the load didn't blank.
      { ...ok, text: stored.replace("Globex", "Hooli!") },
    ])
      expect(originalSections([s], original)[0]).toBe(s);
    // A range that cuts a character of the original.
    const euro = { text: "  ", start: 1, end: 3 };
    expect(originalSections([euro], "€uro")[0]).toBe(euro);
    expect(originalSections([], original)).toEqual([]);
  });

  it("anchors spans", () => {
    expect(spanAnchor(2, 118)).toBe("s2-118");
    expect(spanAnchor(undefined, undefined)).toBe("s0-0");
  });

  it("reads the span a link asks for", () => {
    expect(spanTarget("s2-10-40")).toEqual({ section: 2, start: 10, end: 40 });
    for (const hash of ["", "s2-10", "s2-40-10", "s2-4-4", "x2-1-3", "s-1-2"])
      expect(spanTarget(hash), hash).toBe(undefined);
  });
});

describe("a link to a span", () => {
  it("is written by the document's path, after its tab", () => {
    const span = { section: 2, start: 118, end: 140 };
    expect(ontologyPaths.document("o", "d", undefined, span)).toBe(
      "/ontology/o/documents/d#s2-118-140",
    );
    expect(ontologyPaths.document("o", "d", "facts", span)).toBe(
      "/ontology/o/documents/d?tab=facts#s2-118-140",
    );
    expect(
      spanTarget(
        ontologyPaths.document("o", "d", undefined, span).split("#")[1]!,
      ),
    ).toEqual(span);
  });
});

describe("a click on a mark", () => {
  const run = (
    entities: string[],
    facts: string[],
    reviews: string[],
    inLink?: boolean,
  ) => ({ entities, facts, reviews, inLink });
  const facts = [
    { factKey: "f1", from: { id: "from" }, to: { id: "to" } },
    { factKey: "f2", to: { id: "to" } },
    { factKey: "f3" },
  ];

  it("opens a name's entity, before any quote or review over it", () => {
    expect(markTarget(run(["e1", "e2"], ["f1"], ["0"]), facts)).toEqual({
      kind: "entity",
      id: "e1",
    });
  });

  it("opens a quote's fact's entity: its from end, else its to end", () => {
    expect(markTarget(run([], ["f1", "f2"], ["0"]), facts)).toEqual({
      kind: "entity",
      id: "from",
    });
    expect(markTarget(run([], ["f2"], []), facts)).toEqual({
      kind: "entity",
      id: "to",
    });
  });

  it("shows the fact while the facts load, or when it joins no entity", () => {
    for (const read of [undefined, facts, []])
      expect(markTarget(run([], ["f3"], ["0"]), read)).toEqual({
        kind: "fact",
        factKey: "f3",
      });
    expect(markTarget(run([], ["f1"], []), undefined)).toEqual({
      kind: "fact",
      factKey: "f1",
    });
  });

  it("shows a review's card when nothing more specific covers it", () => {
    expect(markTarget(run([], [], ["0", "2"]), facts)).toEqual({
      kind: "review",
      review: "0",
    });
  });

  it("goes nowhere from a link's span alone or inside a link", () => {
    expect(markTarget(run([], [], []), facts)).toEqual({ kind: "none" });
    expect(markTarget(run(["e1"], ["f1"], ["0"], true), facts)).toEqual({
      kind: "none",
    });
  });
});

describe("the job reading documents", () => {
  it("is a live load or extract, with its load; a delete reads none", () => {
    const load = (id: string, jobs: GraphJob[]) => ({ id, jobs });
    expect(liveRead(undefined)).toBeUndefined();
    expect(
      liveRead([load("L1", [{ id: "d", kind: "delete", status: "running" }])]),
    ).toBeUndefined();
    const extract: GraphJob = { id: "x", kind: "extract", status: "queued" };
    expect(
      liveRead([
        load("L2", [{ id: "l", kind: "load", status: "succeeded" }]),
        load("L1", [extract]),
      ]),
    ).toEqual({ load: load("L1", [extract]), job: extract });
  });
});

describe("what the ontologies did with an upload or a move", () => {
  const names: Record<string, string> = {
    o1: "Customer contracts",
    o2: "Renewals",
    o3: "Billing",
  };
  const name = (id: string) => names[id]!;
  const read = (ontologyId: string, outcome: string): DocumentRead => ({
    ontologyId,
    outcome:
      `FOUNDRY_DOCUMENT_READ_OUTCOME_${outcome}` as DocumentRead["outcome"],
  });
  const text = (
    reads: DocumentRead[],
    readers: { ontologyId: string; routed?: boolean }[] = [],
  ) => readsText(reads, { name, setName: "General", readers });

  it("knows the WFE's outcomes and no others", () => {
    expect(readOutcome("FOUNDRY_DOCUMENT_READ_OUTCOME_NO_BUDGET")).toBe(
      "no_budget",
    );
    expect(readOutcome("FOUNDRY_DOCUMENT_READ_OUTCOME_READING")).toBe(
      "reading",
    );
    expect(readOutcome("FOUNDRY_DOCUMENT_READ_OUTCOME_UNSPECIFIED")).toBe(
      undefined,
    );
    expect(readOutcome("FOUNDRY_DOCUMENT_READ_OUTCOME_LATER")).toBe(undefined);
    expect(readOutcome(undefined)).toBe(undefined);
  });

  it("says no ontology reads the set, or which will on their first load", () => {
    expect(text([])).toBe("No ontology reads General yet.");
    expect(text([read("o1", "UNSPECIFIED")])).toBe(
      "No ontology reads General yet.",
    );
    expect(text([], [{ ontologyId: "o2", routed: true }])).toBe(
      "Renewals reads it with its first load.",
    );
  });

  it("names each outcome's ontologies, the verb agreeing with them", () => {
    expect(text([read("o1", "QUEUED"), read("o2", "QUEUED")])).toBe(
      "Customer contracts and Renewals read it now.",
    );
    expect(text([read("o1", "OWED")])).toBe(
      "Customer contracts reads it once the job running now ends.",
    );
    expect(text([read("o1", "NO_BUDGET"), read("o2", "NO_BUDGET")])).toBe(
      "Customer contracts and Renewals read it with their next load: this month’s budget for reading is spent.",
    );
    expect(text([read("o2", "HELD")])).toBe(
      "Renewals keeps what its current load read from it until the next load.",
    );
    expect(text([read("o1", "SKIPPED"), read("o2", "SKIPPED")])).toBe(
      "Customer contracts and Renewals don’t read it yet: you can’t write to their database.",
    );
    // A PDF whose text Foundry is still reading.
    expect(text([read("o1", "READING")])).toBe(
      "Customer contracts reads it once Foundry has read its text.",
    );
    expect(
      text(
        [read("o1", "READING"), read("o2", "READING")],
        [
          { ontologyId: "o1", routed: true },
          { ontologyId: "o2", routed: false },
        ],
      ),
    ).toBe(
      "Customer contracts reads it once Foundry has read its text. Renewals stores it without reading it: no route reads General.",
    );
  });

  it("says an ontology no route of which reads the set only stores it", () => {
    expect(
      text(
        [read("o1", "QUEUED"), read("o2", "OWED"), read("o3", "QUEUED")],
        [
          { ontologyId: "o1", routed: true },
          { ontologyId: "o2", routed: false },
          { ontologyId: "o3", routed: false },
        ],
      ),
    ).toBe(
      "Customer contracts reads it now. Billing and Renewals store it without reading it: no route reads General.",
    );
  });

  it("puts every outcome together, and the readers not loaded yet last", () => {
    expect(
      text(
        [read("o1", "QUEUED"), read("o2", "SKIPPED")],
        [
          { ontologyId: "o1", routed: true },
          { ontologyId: "o2", routed: true },
          { ontologyId: "o3", routed: true },
        ],
      ),
    ).toBe(
      "Customer contracts reads it now. Renewals doesn’t read it yet: you can’t write to its database. Billing reads it with its first load.",
    );
  });
});

// Each doc comment's @example, as written.
describe("viewers", () => {
  it("decodes the WFE's base64 bytes, whatever its alphabet or padding", () => {
    const all = Uint8Array.from({ length: 256 }, (_, i) => i);
    const std = Buffer.from(all).toString("base64");
    expect(base64Bytes(std)).toEqual(all);
    // URL-safe, unpadded.
    expect(base64Bytes(Buffer.from(all).toString("base64url"))).toEqual(all);
    expect(base64Bytes("")).toEqual(new Uint8Array());
    expect(() => base64Bytes("not base64!")).toThrow();
  });

  it("knows a PDF by its first bytes", () => {
    const bytes = (s: string) => new TextEncoder().encode(s);
    expect(isPdf(bytes("%PDF-"))).toBe(true);
    expect(isPdf(bytes("%PDF"))).toBe(false);
    expect(isPdf(bytes(" %PDF-1.7"))).toBe(false);
    expect(isPdf(bytes("<html>"))).toBe(false);
    expect(isPdf(new Uint8Array())).toBe(false);
  });

  it("previews CSV and TSV records: every column, the bad rows by number", () => {
    // Blank headers are named by position, repeats numbered.
    expect(
      delimitedPreview(
        [
          [" ", "a", "a", "a", ""],
          ["1", "2", "3", "4", "5"],
        ],
        [],
      ),
    ).toEqual({
      columns: ["column 1", "a", "a (2)", "a (3)", "column 5"],
      rows: [
        {
          "column 1": "1",
          a: "2",
          "a (2)": "3",
          "a (3)": "4",
          "column 5": "5",
        },
      ],
      total: 1,
      failed: [],
    });
    // Rows papaparse refused (by record index) and rows with too many or
    // too few fields failed, counting the header as row 1.
    expect(
      delimitedPreview(
        [
          ["a", "b"],
          ["1", "2"],
          ["3", "4"],
          ["5"],
          ["6", "7", "8"],
          ["9", "0"],
        ],
        [2],
      ),
    ).toEqual({
      columns: ["a", "b"],
      rows: [
        { a: "1", b: "2" },
        { a: "9", b: "0" },
      ],
      total: 2,
      failed: [3, 4, 5],
    });
    // A header that names __proto__ is a column like any other.
    const proto = delimitedPreview([["__proto__"], ["x"]], []);
    expect(Object.keys(proto.rows[0]!)).toEqual(["__proto__"]);
    // Nothing, or only a header.
    expect(delimitedPreview([], [])).toEqual({
      columns: [],
      rows: [],
      total: 0,
      failed: [],
    });
    expect(delimitedPreview([["a"]], [])).toEqual({
      columns: ["a"],
      rows: [],
      total: 0,
      failed: [],
    });
  });

  it("shows a table's first rows only, and says how many it holds", () => {
    const records = [
      ["n"],
      ...Array.from({ length: PREVIEW_ROWS + 2 }, (_, i) => [String(i)]),
    ];
    const p = delimitedPreview(records, []);
    expect(p.rows).toHaveLength(PREVIEW_ROWS);
    expect(p.rows.at(-1)).toEqual({ n: String(PREVIEW_ROWS - 1) });
    expect(p.total).toBe(PREVIEW_ROWS + 2);
    expect(previewText(p, "csv")).toBe("The first 5,000 of 5,002 rows.");
    const lines = Array.from({ length: PREVIEW_ROWS + 1 }, (_, i) =>
      JSON.stringify({ i }),
    ).join("\n");
    expect(jsonlPreview(lines).rows).toHaveLength(PREVIEW_ROWS);
    expect(jsonlPreview(lines).total).toBe(PREVIEW_ROWS + 1);
  });

  it("previews JSONL: a line a row, the bad lines by number", () => {
    expect(
      jsonlPreview('{"a":1}\r\n  \n{"b":"x","a":2}\n{bad\n"text"\nnull\n'),
    ).toEqual({
      columns: ["a", "b", "value"],
      rows: [{ a: 1 }, { b: "x", a: 2 }, { value: "text" }, { value: null }],
      total: 4,
      failed: [4],
    });
    expect(jsonlPreview("")).toEqual({
      columns: [],
      rows: [],
      total: 0,
      failed: [],
    });
  });

  it("makes Parquet values cells the grid can show", () => {
    expect(parquetCell(12n)).toBe(12n);
    expect(parquetCell(new Date("2026-10-01T00:00:00Z"))).toBe(
      "2026-10-01T00:00:00.000Z",
    );
    expect(parquetCell(new Uint8Array([0, 255]))).toBe("00ff");
    expect(parquetCell([1n, { d: new Date(0) }])).toEqual([
      "1",
      { d: "1970-01-01T00:00:00.000Z" },
    ]);
    for (const v of [null, undefined, "x", 1.5, true])
      expect(parquetCell(v)).toBe(v);
    // Its nested values survive JSON, as the grid shows them.
    expect(() => JSON.stringify(parquetCell({ a: [9n] }))).not.toThrow();
    // A file whose metadata undercounts still counts what was read.
    expect(parquetPreview([{ a: 1 }, { a: 2 }], 1).total).toBe(2);
  });

  it("says what a preview holds and what couldn't be read", () => {
    const p = (rows: number, total: number, failed: number[] = []) => ({
      columns: [],
      rows: new Array(rows),
      total,
      failed,
    });
    expect(previewText(p(1, 1), "csv")).toBe("1 row.");
    expect(previewText(p(0, 0), "parquet")).toBe("0 rows.");
    expect(previewText(p(1200, 1200), "tsv")).toBe("1,200 rows.");
    expect(previewText(p(2, 2, [3]), "csv")).toBe(
      "2 rows. Row 3 couldn’t be read.",
    );
    expect(previewText(p(2, 2, [7]), "jsonl")).toBe(
      "2 rows. Line 7 couldn’t be read.",
    );
    expect(previewText(p(2, 2, [1, 2, 3, 4, 5, 6, 7, 1200]), "jsonl")).toBe(
      "2 rows. Lines 1, 2, 3, 4, 5, and 3 more couldn’t be read.",
    );
  });

  it("walks a JSON value as its tree shows it", () => {
    expect(jsonChildren("x")).toEqual([]);
    expect(jsonChildren(null)).toEqual([]);
    expect(jsonChildren([])).toEqual([]);
    expect(jsonSummary({})).toBe("{0 keys}");
    expect(jsonSummary([1])).toBe("[1 item]");
    expect(jsonSummary(1.5)).toBe("1.5");
    expect(jsonSummary(false)).toBe("false");
    expect(readJson("[1, 2]")).toEqual({ ok: true, value: [1, 2] });
    expect(readJson("")).toMatchObject({ ok: false });
    expect(JSON_PAGE).toBe(200);
  });
});

describe("the documents model's examples", () => {
  it("say what they say", () => {
    expect(documentNameProblem("Q3/notes.md")).toBe(
      "A document’s name can’t have a slash, a backslash or a control character.",
    );
    expect(andList(["General", "Board", "Notes"])).toBe(
      "General, Board, and Notes",
    );
    expect(
      readsText(
        [{ ontologyId: "o1", outcome: "FOUNDRY_DOCUMENT_READ_OUTCOME_QUEUED" }],
        { name: () => "Renewals", setName: "General", readers: [] },
      ),
    ).toBe("Renewals reads it now.");
    expect(extractionBadge({ status: "extracted", reviews: 2 })).toEqual({
      status: "review",
      label: "Needs review · 2",
    });
    expect(
      documentDot({ id: "d", extraction: { status: "extracted", reviews: 2 } }),
    ).toEqual({ status: "review", label: "Needs review · 2" });
    expect(
      documentDot({
        id: "d",
        status: "uploaded",
        mediaType: "application/pdf",
        read: {
          state: "FOUNDRY_FILE_READ_STATE_FAILED",
          error: { message: "Scanned PDF: no text to read." },
        },
      }),
    ).toEqual({ status: "failed", label: "Scanned PDF: no text to read." });
    expect(documentDot({ id: "d", mediaType: "text/csv" })).toEqual({
      status: "canceled",
      label: "A table file: loads read its rows through mappings",
    });
    expect(
      ontologySetFolders(
        "o1",
        [
          { id: "g", name: "General", readBy: [{ ontologyId: "o1" }] },
          { id: "b", name: "Board" },
        ],
        [
          {
            id: "d",
            setId: "g",
            extraction: { status: "extracted", reviews: 1 },
          },
        ],
      ).map((f) => [f.set.id, f.documents.length, f.review]),
    ).toEqual([["g", 1, 1]]);
    expect(readersText("Board", [{ ontologyId: "o1" }], () => "Renewals")).toBe(
      "Renewals reads Board.",
    );
    expect(ontologyReadText("General", undefined)).toBe(
      "Its current load holds it, but its version doesn’t read General: the next load drops it.",
    );
    expect(setNameProblem(" ", [])).toBe("Name the set.");
    expect(setNameProblem("général", [{ id: "set_1", name: "General" }])).toBe(
      "There is a set named General already.",
    );
    expect(formatOfName("Q3.CSV")?.mediaType).toBe("text/csv");
    expect(formatOfName("deck.docx")).toBeUndefined();
    expect(formatOf("application/pdf")?.name).toBe("PDF");
    expect(documentViewer("text/markdown")).toEqual({
      kind: "markdown",
      mdx: false,
    });
    expect(documentViewer("text/tab-separated-values")).toEqual({
      kind: "table",
      source: "tsv",
    });
    expect(documentViewer("image/png")).toEqual({ kind: "unknown" });
    expect(readsAsText({ id: "d", mediaType: "text/markdown" })).toBe(true);
    expect(
      readsAsText({
        id: "d",
        status: "uploaded",
        mediaType: "application/pdf",
        read: { state: "FOUNDRY_FILE_READ_STATE_READING" },
      }),
    ).toBe(false);
    expect(readKindOf({ id: "d", mediaType: "text/csv" })).toBe("table");
    expect(
      readKindOf({ id: "d", read: { kind: "FOUNDRY_READ_KIND_TEXT" } }),
    ).toBe("text");
    expect(
      fileRead({
        id: "d",
        status: "uploaded",
        read: { state: "FOUNDRY_FILE_READ_STATE_READY", rows: "12" },
      }),
    ).toEqual({ state: "ready", rows: 12 });
    expect(
      fileRead({
        id: "d",
        status: "uploaded",
        read: { state: "FOUNDRY_FILE_READ_STATE_READY", pages: 3 },
      }),
    ).toEqual({ state: "ready", pages: 3 });
    expect(
      fileRead({
        id: "d",
        status: "uploaded",
        read: { state: "FOUNDRY_FILE_READ_STATE_READING", retrying: true },
      }),
    ).toEqual({ state: "reading", retrying: true });
    expect(fileRead({ id: "d" })).toEqual({ state: "none" });
    expect(
      fileReadLabel({
        id: "d",
        status: "uploaded",
        mediaType: "text/csv",
        read: { state: "FOUNDRY_FILE_READ_STATE_READY", rows: "1200" },
      }),
    ).toBe("1,200 rows");
    expect(
      fileReadLabel({
        id: "d",
        status: "uploaded",
        mediaType: "application/pdf",
        read: { state: "FOUNDRY_FILE_READ_STATE_READY", pages: 1 },
      }),
    ).toBe("1 page");
    expect(kindText({ id: "d", mediaType: "text/csv" }, "General")).toBe(
      "Foundry reads its rows once. A generation that reads General maps it as a table of its own.",
    );
    expect(
      kindText(
        {
          id: "d",
          status: "uploaded",
          mediaType: "application/pdf",
          read: {
            state: "FOUNDRY_FILE_READ_STATE_FAILED",
            error: { message: "Scanned PDF: no text to read" },
          },
        },
        "General",
      ),
    ).toBe("Ontologies can’t read it. Scanned PDF: no text to read.");
    expect(
      kindText({ id: "d", mediaType: "text/markdown" }, "General"),
    ).toBeUndefined();
    expect(fileProblem({ name: "deck.docx", size: 10 })).toBe(
      "Only Markdown, MDX, text, JSON, CSV, TSV, JSONL, Parquet, or PDF files can be uploaded.",
    );
    expect(failureText({ code: "model", retryable: true })).toBe(
      "The model couldn’t read it. The next load tries again.",
    );
    expect(
      failureText(
        { code: "unsupported" },
        {
          id: "d",
          status: "uploaded",
          mediaType: "application/pdf",
          read: {
            state: "FOUNDRY_FILE_READ_STATE_FAILED",
            error: { message: "Scanned PDF: no text to read." },
          },
        },
      ),
    ).toBe("Scanned PDF: no text to read.");
    expect(
      sectionPages(
        [
          { text: "One", start: 0 },
          { text: "Two\fThree", start: 4 },
        ],
        "One\fTwo\fThree",
      ),
    ).toEqual([
      { first: 1, last: 1 },
      { first: 2, last: 3 },
    ]);
    expect(sectionPages([{ text: "One\fTwo" }, { text: "Three" }])).toEqual([
      { first: 1, last: 2 },
      { first: 2, last: 2 },
    ]);
    expect(readsBytes({ kind: "table", source: "parquet" })).toBe(true);
    expect(readsBytes({ kind: "table", source: "csv" })).toBe(false);
    expect(renderedLabel({ kind: "json" })).toBe("Tree");
    expect(renderedLabel({ kind: "pdf" })).toBeUndefined();
    expect(readJson('{"a":1}')).toEqual({ ok: true, value: { a: 1 } });
    expect(readJson("{").ok).toBe(false);
    expect([...base64Bytes("JVBERi0=")]).toEqual([37, 80, 68, 70, 45]);
    expect(isPdf(new TextEncoder().encode("%PDF-1.7"))).toBe(true);
    expect(
      delimitedPreview([["id", "name", "name"], ["1", "Ada", "L"], ["2"]], []),
    ).toEqual({
      columns: ["id", "name", "name (2)"],
      rows: [{ id: "1", name: "Ada", "name (2)": "L" }],
      total: 1,
      failed: [3],
    });
    expect(jsonlPreview('{"id":1}\n\nnope\n[2]')).toEqual({
      columns: ["id", "value"],
      rows: [{ id: 1 }, { value: [2] }],
      total: 2,
      failed: [3],
    });
    expect(parquetCell({ at: new Date(0), n: 2n })).toEqual({
      at: "1970-01-01T00:00:00.000Z",
      n: "2",
    });
    expect(parquetPreview([{ id: 1n }], 12_000)).toEqual({
      columns: ["id"],
      rows: [{ id: 1n }],
      total: 12000,
      failed: [],
    });
    expect(
      previewText(
        { columns: [], rows: new Array(5000), total: 12345, failed: [4, 9] },
        "csv",
      ),
    ).toBe("The first 5,000 of 12,345 rows. Rows 4 and 9 couldn’t be read.");
    expect(jsonChildren({ a: 1, b: [2] })).toEqual([
      ["a", 1],
      ["b", [2]],
    ]);
    expect(jsonChildren(["x"])).toEqual([["0", "x"]]);
    expect([
      jsonSummary({ a: 1 }),
      jsonSummary([1, 2]),
      jsonSummary("hi"),
      jsonSummary(null),
    ]).toEqual(["{1 key}", "[2 items]", '"hi"', "null"]);
    expect(
      markTarget({ entities: ["e1"], facts: ["f1"], reviews: [] }, undefined),
    ).toEqual({ kind: "entity", id: "e1" });
    expect(
      markTarget({ entities: [], facts: ["f1"], reviews: [] }, [
        { factKey: "f1", to: { id: "e2" } },
      ]),
    ).toEqual({ kind: "entity", id: "e2" });
    expect(
      markTarget({ entities: [], facts: ["f1"], reviews: ["0"] }, undefined),
    ).toEqual({ kind: "fact", factKey: "f1" });
    expect(markTarget({ entities: [], facts: [], reviews: ["0"] }, [])).toEqual(
      { kind: "review", review: "0" },
    );
    expect(
      markTarget(
        { entities: ["e1"], facts: [], reviews: [], inLink: true },
        [],
      ),
    ).toEqual({ kind: "none" });
  });

  it("say how an ontology reads a document's set", () => {
    expect(
      ontologyReadText("General", { ontologyId: "o1", routed: true }),
    ).toBe("Its routes read General: loads read what it holds.");
    expect(
      ontologyReadText("General", { ontologyId: "o1", routed: false }),
    ).toBe("No route reads General: loads store it without reading it.");
  });
});

describe("file reads", () => {
  const csv = (read: FoundryDocument["read"], status = "uploaded") =>
    ({
      id: "d",
      mediaType: "text/csv",
      status,
      read,
    }) as FoundryDocument;

  it("reads each WFE state, and nothing before an upload completes", () => {
    expect(fileRead(csv({ state: "FOUNDRY_FILE_READ_STATE_READY" }))).toEqual({
      state: "ready",
    });
    expect(
      fileRead(csv({ state: "FOUNDRY_FILE_READ_STATE_READY", rows: "x" })),
    ).toEqual({ state: "ready" });
    expect(fileRead(csv({ state: "FOUNDRY_FILE_READ_STATE_READING" }))).toEqual(
      { state: "reading", retrying: false },
    );
    // A state this page doesn't know is no read to speak of.
    expect(
      fileRead(
        csv({ state: "FOUNDRY_FILE_READ_STATE_NEWER" as FoundryFileReadState }),
      ),
    ).toEqual({ state: "none" });
    expect(
      fileRead(csv({ state: "FOUNDRY_FILE_READ_STATE_UNSPECIFIED" })),
    ).toEqual({ state: "none" });
    expect(
      fileRead(
        csv({ state: "FOUNDRY_FILE_READ_STATE_READY" }, "pending_upload"),
      ),
    ).toEqual({ state: "none" });
  });

  it("says why a read failed, in its code's words without a message", () => {
    expect(
      fileRead(
        csv({
          state: "FOUNDRY_FILE_READ_STATE_FAILED",
          error: { code: "too_large" },
        }),
      ),
    ).toEqual({
      state: "failed",
      code: "too_large",
      message:
        "It is past a limit of what Foundry reads. Split it into smaller files.",
    });
    expect(fileRead(csv({ state: "FOUNDRY_FILE_READ_STATE_FAILED" }))).toEqual({
      state: "failed",
      code: "unsupported",
      message: "It isn’t CSV. Upload a fixed file.",
    });
    expect(
      fileRead(
        csv({
          state: "FOUNDRY_FILE_READ_STATE_FAILED",
          error: { code: "unsupported", message: "Line 3 isn’t CSV." },
        }),
      ),
    ).toMatchObject({ message: "Line 3 isn’t CSV." });
    // The WFE writes a clause: shown as a sentence.
    expect(
      fileRead(
        csv({
          state: "FOUNDRY_FILE_READ_STATE_FAILED",
          error: {
            code: "too_large",
            message: "it is nested more than 64 levels deep",
          },
        }),
      ),
    ).toEqual({
      state: "failed",
      code: "too_large",
      message: "It is nested more than 64 levels deep.",
    });
    expect(
      fileRead(
        csv({
          state: "FOUNDRY_FILE_READ_STATE_FAILED",
          error: { message: "Is it CSV?" },
        }),
      ),
    ).toMatchObject({ message: "Is it CSV?" });
  });

  it("labels a table file's or a PDF's read, never text's", () => {
    expect(
      fileReadLabel(csv({ state: "FOUNDRY_FILE_READ_STATE_READY", rows: "1" })),
    ).toBe("1 row");
    expect(fileReadLabel(csv({ state: "FOUNDRY_FILE_READ_STATE_READY" }))).toBe(
      "Read",
    );
    expect(
      fileReadLabel(csv({ state: "FOUNDRY_FILE_READ_STATE_READING" })),
    ).toBe("Being read…");
    expect(
      fileReadLabel(
        csv({ state: "FOUNDRY_FILE_READ_STATE_READING", retrying: true }),
      ),
    ).toBe("Waiting to be read again");
    expect(
      fileReadLabel(csv({ state: "FOUNDRY_FILE_READ_STATE_FAILED" })),
    ).toBe("Couldn’t be read");
    const pdf = (read: FoundryDocument["read"]) =>
      fileReadLabel({
        id: "p",
        mediaType: "application/pdf",
        status: "uploaded",
        read,
      });
    expect(pdf({ state: "FOUNDRY_FILE_READ_STATE_READY", pages: 12 })).toBe(
      "12 pages",
    );
    expect(pdf({ state: "FOUNDRY_FILE_READ_STATE_READY" })).toBe("Read");
    expect(pdf({ state: "FOUNDRY_FILE_READ_STATE_READING" })).toBe(
      "Being read…",
    );
    expect(pdf({ state: "FOUNDRY_FILE_READ_STATE_FAILED" })).toBe(
      "Couldn’t be read",
    );
    expect(fileReadLabel(csv(undefined))).toBeUndefined();
    expect(
      fileReadLabel({
        id: "m",
        mediaType: "text/markdown",
        status: "uploaded",
        read: { state: "FOUNDRY_FILE_READ_STATE_READY" },
      }),
    ).toBeUndefined();
  });

  it("says what happens to a file by its kind", () => {
    const pdf = (read: FoundryDocument["read"]) =>
      kindText(
        { id: "p", mediaType: "application/pdf", status: "uploaded", read },
        "General",
      );
    expect(
      pdf({
        state: "FOUNDRY_FILE_READ_STATE_FAILED",
        error: {
          code: "unsupported",
          message: "Scanned PDF: no text to read.",
        },
      }),
    ).toBe("Ontologies can’t read it. Scanned PDF: no text to read.");
    // The WFE's clause, as a sentence.
    expect(
      pdf({
        state: "FOUNDRY_FILE_READ_STATE_FAILED",
        error: {
          code: "unsupported",
          message: "the PDF is encrypted; upload it without a password",
        },
      }),
    ).toBe(
      "Ontologies can’t read it. The PDF is encrypted; upload it without a password.",
    );
    // Being read, read, or before its upload completes, readsText says it.
    expect(pdf({ state: "FOUNDRY_FILE_READ_STATE_READING" })).toBeUndefined();
    expect(pdf({ state: "FOUNDRY_FILE_READ_STATE_READY" })).toBeUndefined();
    expect(pdf(undefined)).toBeUndefined();
    expect(
      kindText({ id: "i", mediaType: "image/png" }, "General"),
    ).toBeUndefined();
    expect(readKindOf({ id: "d", mediaType: "image/png" })).toBeUndefined();
    expect(
      readKindOf({
        id: "d",
        mediaType: "text/csv",
        read: { kind: "FOUNDRY_READ_KIND_PDF" },
      }),
    ).toBe("pdf");
  });
});
