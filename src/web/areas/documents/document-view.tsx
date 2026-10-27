import { Fragment, Suspense, lazy } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  Download,
  FileQuestion,
  LoaderCircle,
  TriangleAlert,
} from "lucide-react";
import type { FoundryDocument } from "../../../shared/wfe";
import { Button, SkeletonText } from "../../kit";
import { Callout } from "../ontology/parts";
import { documentQueries } from "./documents-api";
import {
  documentViewer,
  fileRead,
  readKindOf,
  readsBytes,
  renderedLabel,
} from "./model";

// What an uploaded document holds, shown by its media type
// (documentViewer): its text from :content, or its bytes from :bytes for a
// PDF's pages, a Parquet file and a type this page doesn't know. A PDF
// shows the text Foundry read from it, its pages one after another, each
// after a rule with its number, once that read is done, unless `pages`
// asks for its pages. `source` shows a rendered view's text as uploaded.
// Each viewer is loaded with the first document that needs it.

const Markdown = lazy(() =>
  import("./markdown").then((m) => ({ default: m.Markdown })),
);
const JsonTree = lazy(() =>
  import("./json-tree").then((m) => ({ default: m.JsonTree })),
);
const Delimited = lazy(() =>
  import("./delimited").then((m) => ({ default: m.Delimited })),
);
const Jsonl = lazy(() =>
  import("./table-preview").then((m) => ({ default: m.Jsonl })),
);
const Parquet = lazy(() =>
  import("./parquet").then((m) => ({ default: m.Parquet })),
);
const Pdf = lazy(() => import("./pdf").then((m) => ({ default: m.Pdf })));

const PlainText = ({ text }: { text: string }) => (
  <div className="dc-doc-text" aria-label="Document text">
    <p className="dc-doc-section">{text}</p>
  </div>
);

/** Where a PDF's page starts in its text: a rule with the page's number. */
export const PageBreak = ({ page }: { page: number }) => (
  <span className="dc-page-break" role="separator" aria-label={`Page ${page}`}>
    Page {page}
  </span>
);

// A PDF's text as Foundry read it: its pages, separated by form feeds.
const PdfText = ({ text }: { text: string }) => (
  <div className="dc-doc-text" aria-label="Document text">
    {text.split("\f").map((page, i) => (
      <Fragment key={i}>
        <PageBreak page={i + 1} />
        <p className="dc-doc-section">{page}</p>
      </Fragment>
    ))}
  </div>
);

/**
 * Why a PDF's text isn't shown: Foundry is reading it (or waits to read it
 * again), or couldn't read it (why: a scanned PDF has no text to read).
 * Nothing for any other document, or once its text is read.
 */
export function PdfReadCallout({ document: d }: { document: FoundryDocument }) {
  if (readKindOf(d) !== "pdf") return null;
  const read = fileRead(d);
  switch (read.state) {
    case "reading":
      return (
        <Callout
          icon={LoaderCircle}
          live
          title={
            read.retrying ? "Waiting to be read again" : "Reading its text"
          }
          text={
            read.retrying
              ? "Reading it failed for a reason that isn’t the file’s. Foundry tries again on its own."
              : "Ontologies read it once its text is read. A generation started now leaves it out."
          }
        />
      );
    case "failed":
      return (
        <Callout
          icon={TriangleAlert}
          tone="danger"
          title="Ontologies can’t read it"
          text={read.message}
        />
      );
    case "ready":
    case "none":
      return null;
  }
}

// A type this page doesn't know (from a newer WFE): its bytes as text, and
// the file to download.
function Unknown({
  bytes,
  mediaType,
  name,
}: {
  bytes: Uint8Array<ArrayBuffer>;
  mediaType: string;
  name: string;
}) {
  const download = () => {
    const url = URL.createObjectURL(new Blob([bytes], { type: mediaType }));
    Object.assign(window.document.createElement("a"), {
      href: url,
      download: name,
    }).click();
    // The download reads the URL after the click returns.
    setTimeout(() => URL.revokeObjectURL(url), 40_000);
  };
  return (
    <div className="on-stack">
      <Callout
        icon={FileQuestion}
        tone="muted"
        title="Foundry can’t show this type of document yet"
        text={`It is ${mediaType || "of no known type"}: here are its bytes as text. Download it to open it elsewhere.`}
        action={
          <Button size="sm" icon={Download} onClick={download}>
            Download
          </Button>
        }
      />
      <PlainText text={new TextDecoder().decode(bytes)} />
    </div>
  );
}

export function DocumentView({
  workspaceId,
  document: d,
  source,
  pages = false,
}: {
  workspaceId: string;
  document: FoundryDocument;
  source: boolean;
  // A PDF's pages, even once its text is read.
  pages?: boolean;
}) {
  const viewer = documentViewer(d.mediaType),
    pdfText = viewer.kind === "pdf" && !pages && fileRead(d).state === "ready",
    binary = readsBytes(viewer) && !pdfText;
  const text = useQuery({
    ...documentQueries.content(workspaceId, d.id),
    enabled: !binary,
  });
  const raw = useQuery({
    ...documentQueries.bytes(workspaceId, d.id),
    enabled: binary,
  });
  const read = binary ? raw : text;
  if (read.isPending) return <SkeletonText lines={8} />;
  if (read.error)
    return (
      <p role="alert" className="on-error-text">
        Couldn’t read it: {read.error.message}{" "}
        <Button size="sm" variant="ghost" onClick={() => void read.refetch()}>
          Retry
        </Button>
      </p>
    );
  const t = text.data?.text || "",
    bytes = raw.data?.bytes || new Uint8Array();
  const view =
    source && renderedLabel(viewer) ? (
      <PlainText text={t} />
    ) : viewer.kind === "markdown" ? (
      <Markdown text={t} mdx={viewer.mdx} />
    ) : viewer.kind === "plain" ? (
      <PlainText text={t} />
    ) : viewer.kind === "json" ? (
      <JsonTree text={t} />
    ) : pdfText ? (
      <PdfText text={t} />
    ) : viewer.kind === "pdf" ? (
      <Pdf bytes={bytes} name={d.name || d.id} />
    ) : viewer.kind === "unknown" ? (
      <Unknown
        bytes={bytes}
        mediaType={raw.data?.mediaType || d.mediaType || ""}
        name={d.name || d.id}
      />
    ) : viewer.source === "parquet" ? (
      <Parquet bytes={bytes} />
    ) : viewer.source === "jsonl" ? (
      <Jsonl text={t} />
    ) : (
      <Delimited text={t} source={viewer.source} />
    );
  return <Suspense fallback={<SkeletonText lines={8} />}>{view}</Suspense>;
}
