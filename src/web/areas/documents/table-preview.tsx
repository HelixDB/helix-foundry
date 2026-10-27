import "./viewers.css";
import { useMemo } from "react";
import { DataGrid, cx } from "../../kit";
import {
  jsonlPreview,
  previewText,
  type TablePreview,
  type TableSource,
} from "./model";

// A table document's first rows in the kit's grid, with how many it holds
// and which couldn't be read. The CSV, TSV and Parquet viewers
// (./delimited.tsx, ./parquet.tsx) parse their files into it; a JSONL
// document needs no parser. Loaded with the first table shown.

export function PreviewGrid({
  preview,
  source,
}: {
  preview: TablePreview;
  source: TableSource;
}) {
  // CSV and TSV cells are text: shown as written, never as dates or numbers.
  const columns = useMemo(
    () =>
      preview.columns.map((key) => ({
        key,
        type: source === "csv" || source === "tsv" ? "string" : undefined,
      })),
    [preview.columns, source],
  );
  return (
    <div className="dc-table">
      <p
        className={cx(
          "dc-table-note",
          preview.failed.length > 0 && "is-failed",
        )}
        role={preview.failed.length ? "alert" : undefined}
      >
        {previewText(preview, source)}
      </p>
      <DataGrid
        label="Document rows"
        rows={preview.rows}
        columns={columns}
        emptyText="It holds no rows."
      />
    </div>
  );
}

/** A JSONL document: one JSON value a line. */
export function Jsonl({ text }: { text: string }) {
  const preview = useMemo(() => jsonlPreview(text), [text]);
  return <PreviewGrid preview={preview} source="jsonl" />;
}
