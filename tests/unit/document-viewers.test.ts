import { readFileSync } from "node:fs";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it } from "vitest";
import type { FoundryDocument } from "@/shared/wfe";
import { Delimited } from "@/web/areas/documents/delimited";
import {
  DocumentView,
  PdfReadCallout,
} from "@/web/areas/documents/document-view";
import { documentQueries } from "@/web/areas/documents/documents-api";
import { JsonTree } from "@/web/areas/documents/json-tree";
import { readParquet } from "@/web/areas/documents/parquet";
import { Pdf } from "@/web/areas/documents/pdf";
import { Jsonl } from "@/web/areas/documents/table-preview";

// The Data document page's viewers: JSON as a tree that renders only what
// is open, CSV and TSV split by papaparse, JSONL, Parquet read by hyparquet
// (Zstandard included), a PDF's pages only when its bytes are one, and a
// PDF's text, once read, page by page.

const html = (el: Parameters<typeof renderToStaticMarkup>[0]) =>
  renderToStaticMarkup(el);

describe("JSON", () => {
  it("opens the root only, and renders no closed node's children", () => {
    const out = html(
      createElement(JsonTree, {
        text: JSON.stringify({
          name: "Acme",
          seats: 12,
          active: true,
          owner: null,
          plans: [{ id: "team-plan-id" }],
        }),
      }),
    );
    expect(out).toContain('aria-expanded="true"');
    expect(out).toContain("{5 keys}");
    expect(out).toContain(
      '<span class="dc-json-value is-string">&quot;Acme&quot;</span>',
    );
    expect(out).toContain('<span class="dc-json-value is-number">12</span>');
    expect(out).toContain('<span class="dc-json-value is-boolean">true</span>');
    expect(out).toContain('<span class="dc-json-value is-null">null</span>');
    // plans is closed: its summary, not its items.
    expect(out).toContain("[1 item]");
    expect(out).not.toContain("team-plan-id");
  });

  it("shows a large array's first page, the rest a click away", () => {
    const out = html(
      createElement(JsonTree, {
        text: JSON.stringify(Array.from({ length: 450 }, (_, i) => i)),
      }),
    );
    expect(out.match(/dc-json-leaf/g)).toHaveLength(200);
    expect(out).toContain("Show 200 more of 250");
  });

  it("says when a document isn't JSON", () => {
    expect(html(createElement(JsonTree, { text: "{nope" }))).toMatch(
      /role="alert".*It isn’t valid JSON/,
    );
  });
});

describe("tables", () => {
  it("splits CSV with papaparse, quotes and line breaks included", () => {
    const out = html(
      createElement(Delimited, {
        text: 'customer,plan\nHooli,"Enterprise, annual"\n"Multi\nline",Team\nbad\n',
        source: "csv",
      }),
    );
    expect(out).toContain("2 rows. Row 4 couldn’t be read.");
    expect(out).toContain('aria-label="Document rows"');
    expect(out).toContain("Enterprise, annual");
  });

  it("splits TSV at tabs, and says when a quote never closes", () => {
    expect(
      html(createElement(Delimited, { text: "a\tb\n1\t2,3\n", source: "tsv" })),
    ).toContain("1 row.");
    expect(
      html(
        createElement(Delimited, {
          text: 'a,b\n1,2\n"open,3\n',
          source: "csv",
        }),
      ),
    ).toMatch(/1 row\. Row 3 couldn’t be read\./);
  });

  it("reads JSONL a line a row", () => {
    expect(
      html(createElement(Jsonl, { text: '{"a":1}\n{"a":2}\nnope\n' })),
    ).toContain("2 rows. Line 3 couldn’t be read.");
  });

  it("reads a Zstandard Parquet file's first rows and its row count", async () => {
    const file = readFileSync(
      new URL("./__fixtures__/orders.parquet", import.meta.url),
    );
    const p = await readParquet(new Uint8Array(file));
    expect(p.total).toBe(5003);
    expect(p.rows).toHaveLength(5000);
    expect(p.columns).toEqual(["id", "customer", "amount", "paid_at", "tags"]);
    expect(p.rows[1]).toEqual({
      id: 1n,
      customer: "cust_1",
      amount: 1.25,
      paid_at: "2026-01-02T00:00:00.000Z",
      tags: ["a", "b"],
    });
  });

  it("rejects bytes that aren't Parquet", async () => {
    await expect(
      readParquet(new TextEncoder().encode("a,b\n1,2\n")),
    ).rejects.toThrow();
  });
});

describe("PDF", () => {
  it("shows nothing but a refusal for bytes that aren't a PDF", () => {
    expect(
      html(
        createElement(Pdf, {
          bytes: new TextEncoder().encode("<html>"),
          name: "deck.pdf",
        }),
      ),
    ).toMatch(/role="alert".*don’t start as a PDF’s do/);
    // A PDF's pages wait for pdf.js to open it, once it is shown.
    expect(
      html(
        createElement(Pdf, {
          bytes: new TextEncoder().encode("%PDF-1.7"),
          name: "deck.pdf",
        }),
      ),
    ).toBe("");
  });
});

describe("a PDF's text", () => {
  const pdf = (read: FoundryDocument["read"]): FoundryDocument => ({
    id: "doc_1",
    name: "terms.pdf",
    mediaType: "application/pdf",
    status: "uploaded",
    read,
  });
  // The view, with what :content and :bytes answered already cached.
  const view = (d: FoundryDocument, pages = false) => {
    const qc = new QueryClient();
    qc.setQueryData(documentQueries.content("ws", d.id).queryKey, {
      text: "Renewal terms\nGlobex renews.\fNotice\f",
    });
    return html(
      createElement(
        QueryClientProvider,
        { client: qc },
        createElement(DocumentView, {
          workspaceId: "ws",
          document: d,
          source: false,
          pages,
        }),
      ),
    );
  };
  const ready = pdf({
    kind: "FOUNDRY_READ_KIND_PDF",
    state: "FOUNDRY_FILE_READ_STATE_READY",
    pages: 3,
  });

  it("shows each page after a rule with its number, once read", () => {
    const out = view(ready);
    expect(out.match(/role="separator"/g)).toHaveLength(3);
    expect(out).toContain(
      '<span class="dc-page-break" role="separator" aria-label="Page 1">Page 1</span><p class="dc-doc-section">Renewal terms\nGlobex renews.</p>',
    );
    expect(out).toContain(
      'aria-label="Page 2">Page 2</span><p class="dc-doc-section">Notice</p>',
    );
    // An empty last page is a page too.
    expect(out).toContain(
      'aria-label="Page 3">Page 3</span><p class="dc-doc-section"></p>',
    );
    expect(out).not.toContain("\f");
  });

  it("shows its pages instead when asked, or until its text is read", () => {
    // The bytes aren't cached: the pages' viewer waits for them.
    for (const out of [
      view(ready, true),
      view(pdf({ state: "FOUNDRY_FILE_READ_STATE_READING" })),
      view(pdf({ state: "FOUNDRY_FILE_READ_STATE_FAILED" })),
    ]) {
      expect(out).not.toContain("separator");
      expect(out).not.toContain("Globex");
    }
  });

  it("says why its text isn't shown, and nothing once it is", () => {
    const callout = (d: FoundryDocument) =>
      html(createElement(PdfReadCallout, { document: d }));
    expect(
      callout(
        pdf({
          state: "FOUNDRY_FILE_READ_STATE_FAILED",
          error: {
            code: "unsupported",
            message: "Scanned PDF: no text to read.",
          },
        }),
      ),
    ).toMatch(/Ontologies can’t read it.*Scanned PDF: no text to read\./);
    expect(callout(pdf({ state: "FOUNDRY_FILE_READ_STATE_READING" }))).toMatch(
      /Reading its text/,
    );
    expect(
      callout(
        pdf({ state: "FOUNDRY_FILE_READ_STATE_READING", retrying: true }),
      ),
    ).toMatch(/Waiting to be read again/);
    expect(callout(ready)).toBe("");
    expect(callout(pdf(undefined))).toBe("");
    // Another type's read is its own page's to say.
    expect(
      callout({
        ...pdf({ state: "FOUNDRY_FILE_READ_STATE_FAILED" }),
        mediaType: "text/csv",
      }),
    ).toBe("");
  });
});
