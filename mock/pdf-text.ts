import { extractText, getDocumentProxy } from "unpdf";

// The mock's stand-in for the WFE's document worker reading a PDF once,
// when its upload completes (the backend's document/read/pdf.go, PDFium in
// WebAssembly): each page's text by unpdf (pdf.js), normalised as the WFE
// stores it, with a form feed (`\f`) between pages; loads and generations
// read it as plain text and :content serves it. A PDF with no text on any
// page (scanned), one pdf.js can't open (damaged, or locked by a password),
// one of no pages or more than MAX_PAGES, or text past MAX_TEXT_BYTES
// fails, in the WFE's words.

/** The most pages a PDF may have to be read (`max_pages`). */
export const MAX_PAGES = 150;
/** The most text a PDF's read may give, in UTF-8 bytes (`max_text_bytes`). */
export const MAX_TEXT_BYTES = 1_048_576;

export type PdfRead =
  | { ok: true; text: string; pages: number }
  | { ok: false; code: "too_large" | "unsupported"; message: string };

const unsupported = (message: string): PdfRead => ({
  ok: false,
  code: "unsupported",
  message,
});

/**
 * A page's text as the WFE stores it: line breaks as \n, a tab as a space,
 * no other control character (a form feed only ever separates pages), no
 * space at a line's end, no blank line at its start or end and none twice
 * in a row. A page with text ends with \n; one with none is "".
 */
const normalizePage = (raw: string) => {
  const text = raw
    .replace(/\r\n?/g, "\n")
    .replace(/\t/g, " ")
    .replace(/[^\P{Cc}\n]/gu, "")
    .split("\n")
    .map((line) => line.trimEnd())
    .join("\n")
    .replace(/\n{3,}/g, "\n\n")
    .replace(/^\n+|\n+$/g, "");
  return text && `${text}\n`;
};

/** A PDF's text, its pages separated by form feeds, or why it can't be read. */
export async function readPdfText(bytes: Buffer): Promise<PdfRead> {
  // pdf.js takes the buffer it is given: a copy, so the stored bytes stay.
  const opened = await getDocumentProxy(new Uint8Array(bytes), {
    verbosity: 0,
  }).catch((e: Error) => e);
  if (opened instanceof Error)
    return unsupported(
      opened.name === "PasswordException"
        ? "the PDF is encrypted; upload it without a password"
        : "the file is damaged or is not a PDF",
    );
  if (!opened.numPages) return unsupported("the PDF has no pages");
  if (opened.numPages > MAX_PAGES)
    return {
      ok: false,
      code: "too_large",
      message: `the PDF has ${opened.numPages} pages; Helix reads PDFs of up to ${MAX_PAGES}`,
    };
  const pages = await extractText(opened, { mergePages: false })
    .then((r) => r.text.map(normalizePage))
    .catch(() => undefined);
  if (!pages)
    return unsupported("the PDF's pages could not be read; it may be damaged");
  if (pages.every((p) => !p))
    return unsupported("Scanned PDF: no text to read");
  const text = pages.join("\f");
  return Buffer.byteLength(text) > MAX_TEXT_BYTES
    ? {
        ok: false,
        code: "too_large",
        message: `the PDF's text is more than ${MAX_TEXT_BYTES >> 10} KiB, which is as much of a document as Helix reads`,
      }
    : { ok: true, text, pages: pages.length };
}

/**
 * A born-digital PDF of `pages`, each a list of ASCII lines in Helvetica:
 * for the mock's fixture and its tests. An empty page has no text, as a
 * scanned one has none.
 *
 * @example
 * (await readPdfText(textPdf([["Globex renews."], ["Notice"]]))).text; // "Globex renews.\n\fNotice\n"
 */
export function textPdf(pages: readonly (readonly string[])[]): Buffer {
  const escape = (line: string) => line.replace(/[\\()]/g, (c) => `\\${c}`);
  const font = 3 + pages.length * 2;
  const objects = [
    "<< /Type /Catalog /Pages 2 0 R >>",
    `<< /Type /Pages /Kids [${pages.map((_, i) => `${3 + i * 2} 0 R`).join(" ")}] /Count ${pages.length} >>`,
    ...pages.flatMap((lines, i) => {
      const stream = lines.length
        ? `BT /F1 11 Tf 15 TL 72 720 Td ${lines.map((l) => `(${escape(l)}) Tj T*`).join(" ")} ET`
        : "";
      return [
        `<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 ${font} 0 R >> >> /Contents ${4 + i * 2} 0 R >>`,
        `<< /Length ${Buffer.byteLength(stream, "latin1")} >>\nstream\n${stream}\nendstream`,
      ];
    }),
    "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
  ];
  // Each object's byte offset, for the cross-reference table.
  let body = "%PDF-1.4\n";
  const offsets = objects.map((o, i) => {
    const at = body.length;
    body += `${i + 1} 0 obj\n${o}\nendobj\n`;
    return at;
  });
  const xref = [
    `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n`,
    ...offsets.map((o) => `${String(o).padStart(10, "0")} 00000 n \n`),
    `trailer\n<< /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${body.length}\n%%EOF\n`,
  ].join("");
  return Buffer.from(body + xref, "latin1");
}
