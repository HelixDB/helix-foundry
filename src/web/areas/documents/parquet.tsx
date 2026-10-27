import { useEffect, useState } from "react";
import { parquetMetadata, parquetReadObjects } from "hyparquet";
import { compressors } from "hyparquet-compressors";
import { SkeletonText } from "../../kit";
import { PREVIEW_ROWS, parquetPreview, type TablePreview } from "./model";
import { PreviewGrid } from "./table-preview";

// A Parquet document, read in the browser by hyparquet (with every codec
// hyparquet-compressors decodes: Snappy, gzip, Brotli, LZ4, Zstandard):
// its first PREVIEW_ROWS rows, and how many its metadata says it holds.
// Loaded on its own, when one is shown.

/**
 * A Parquet file's preview. Async throughout, so a file that isn't Parquet
 * rejects as one that fails while its rows are read does.
 */
export async function readParquet(
  bytes: Uint8Array<ArrayBuffer>,
): Promise<TablePreview> {
  const file = bytes.buffer.slice(
    bytes.byteOffset,
    bytes.byteOffset + bytes.byteLength,
  );
  const metadata = parquetMetadata(file);
  const rows = await parquetReadObjects({
    file,
    metadata,
    compressors,
    rowEnd: PREVIEW_ROWS,
  });
  return parquetPreview(rows, Number(metadata.num_rows));
}

type Read = { preview: TablePreview } | { error: string };

export function Parquet({ bytes }: { bytes: Uint8Array<ArrayBuffer> }) {
  const [read, setRead] = useState<Read>();
  useEffect(() => {
    let live = true;
    readParquet(bytes).then(
      (preview) => live && setRead({ preview }),
      (e: Error) => live && setRead({ error: e.message }),
    );
    return () => {
      live = false;
    };
  }, [bytes]);
  if (!read) return <SkeletonText lines={8} />;
  if ("error" in read)
    return (
      <p role="alert" className="on-error-text">
        Couldn’t read it as Parquet: {read.error}
      </p>
    );
  return <PreviewGrid preview={read.preview} source="parquet" />;
}
