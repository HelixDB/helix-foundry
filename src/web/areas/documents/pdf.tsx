import "./viewers.css";
import { useEffect, useRef, useState, type RefObject } from "react";
import {
  getDocument,
  GlobalWorkerOptions,
  PasswordException,
  RenderingCancelledException,
  version,
  type PDFDocumentProxy,
  type PDFPageProxy,
} from "pdfjs-dist/legacy/build/pdf.mjs";
import { isPdf } from "./model";

// A PDF document's pages, drawn by pdf.js on canvases. The browser's own
// viewer, in an iframe, drew a blank page in some browsers. pdf.js parses
// the PDF in a worker; a page is drawn only while it is near the scrolled
// view and cleared once it is far from it, so a long PDF holds a few pages'
// pixels at a time. Loaded on its own, when one is shown.
//
// The legacy build runs in browsers a few versions back. The files pdf.js
// fetches as it needs them (CMaps for CJK text, the standard fonts, and the
// WebAssembly image decoders scans need) are copied to public/pdfjs/ on
// install (scripts/pdfjs-assets.mjs).

const assets = `/pdfjs/${version}`;

export function Pdf({
  bytes,
  name,
}: {
  bytes: Uint8Array<ArrayBuffer>;
  name: string;
}) {
  const pdf = isPdf(bytes),
    [opened, setOpened] = useState<{
      bytes: Uint8Array;
      doc?: PDFDocumentProxy;
      failure?: string;
    }>();
  useEffect(() => {
    if (!pdf) return;
    // One worker, started by the first PDF shown, opens every one.
    GlobalWorkerOptions.workerPort ??= new Worker(
      new URL("pdfjs-dist/legacy/build/pdf.worker.min.mjs", import.meta.url),
      { type: "module" },
    );
    // pdf.js moves what it is given to its worker, so it gets a copy: these
    // bytes are the query cache's.
    const task = getDocument({
      data: bytes.slice(),
      cMapUrl: `${assets}/cmaps/`,
      standardFontDataUrl: `${assets}/standard_fonts/`,
      wasmUrl: `${assets}/wasm/`,
    });
    let live = true;
    task.promise.then(
      (doc) => live && setOpened({ bytes, doc }),
      (error: unknown) =>
        live &&
        setOpened({
          bytes,
          failure:
            error instanceof PasswordException
              ? "It needs a password, so its pages aren’t shown."
              : "Its pages couldn’t be read, so they aren’t shown. It may be damaged.",
        }),
    );
    return () => {
      live = false;
      void task.destroy();
    };
  }, [bytes, pdf]);
  if (!pdf)
    return (
      <p role="alert" className="on-error-text">
        Its bytes don’t start as a PDF’s do (%PDF-), so it isn’t shown.
      </p>
    );
  // What was opened for other bytes is never shown.
  const current = opened?.bytes === bytes ? opened : undefined;
  if (current?.failure)
    return (
      <p role="alert" className="on-error-text">
        {current.failure}
      </p>
    );
  return current?.doc ? <PdfPages doc={current.doc} name={name} /> : null;
}

/** Every page of doc, as wide as the view, in a scrolling column. */
function PdfPages({ doc, name }: { doc: PDFDocumentProxy; name: string }) {
  const scroller = useRef<HTMLDivElement>(null),
    [width, setWidth] = useState(0),
    [pages, setPages] = useState<
      { doc: PDFDocumentProxy; pages: (PDFPageProxy | undefined)[] } | undefined
    >();
  useEffect(() => {
    let live = true;
    void Promise.allSettled(
      Array.from({ length: doc.numPages }, (_, i) => doc.getPage(i + 1)),
    ).then(
      (settled) =>
        live &&
        setPages({
          doc,
          pages: settled.map((p) =>
            p.status === "fulfilled" ? p.value : undefined,
          ),
        }),
    );
    return () => {
      live = false;
    };
  }, [doc]);
  useEffect(() => {
    const el = scroller.current;
    if (!el) return;
    const observer = new ResizeObserver(([entry]) =>
      setWidth(Math.floor(entry.contentRect.width)),
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, []);
  const shown = pages?.doc === doc ? pages.pages : [];
  return (
    <div ref={scroller} className="dc-pdf" role="document" aria-label={name}>
      {width > 0 &&
        shown.map((page, i) =>
          page ? (
            <PdfPage
              key={i}
              page={page}
              width={width}
              count={shown.length}
              root={scroller}
            />
          ) : (
            <p key={i} className="dc-pdf-missing">
              Page {i + 1} couldn’t be read.
            </p>
          ),
        )}
    </div>
  );
}

/**
 * One page, width CSS pixels wide, drawn at the screen's pixel density
 * while it is within a view's height of root's view.
 */
function PdfPage({
  page,
  width,
  count,
  root,
}: {
  page: PDFPageProxy;
  width: number;
  count: number;
  root: RefObject<HTMLDivElement | null>;
}) {
  const canvas = useRef<HTMLCanvasElement>(null),
    [near, setNear] = useState(false),
    [failed, setFailed] = useState(false),
    size = page.getViewport({ scale: 1 });
  useEffect(() => {
    const el = canvas.current;
    if (!el) return;
    const observer = new IntersectionObserver(
      ([entry]) => setNear(entry.isIntersecting),
      { root: root.current, rootMargin: "100% 0px" },
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, [root]);
  useEffect(() => {
    const el = canvas.current;
    if (!el || !near) return;
    const viewport = page.getViewport({
      scale: (width / size.width) * (window.devicePixelRatio || 1),
    });
    el.width = Math.floor(viewport.width);
    el.height = Math.floor(viewport.height);
    const task = page.render({ canvas: el, viewport });
    task.promise.then(
      () => setFailed(false),
      (error: unknown) =>
        setFailed(!(error instanceof RenderingCancelledException)),
    );
    return () => {
      task.cancel();
      // An empty canvas lets its pixels go; its CSS size keeps its place.
      el.width = 0;
      el.height = 0;
    };
  }, [near, page, width, size.width]);
  return (
    <figure className="dc-pdf-page">
      <canvas
        ref={canvas}
        // No pixels until it is near the view.
        width={0}
        height={0}
        role="img"
        aria-label={`Page ${page.pageNumber} of ${count}`}
        style={{ aspectRatio: `${size.width} / ${size.height}` }}
      />
      <figcaption>
        {failed
          ? `Page ${page.pageNumber} couldn’t be drawn.`
          : `Page ${page.pageNumber}`}
      </figcaption>
    </figure>
  );
}
