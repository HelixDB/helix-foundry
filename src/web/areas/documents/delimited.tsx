import { useMemo } from "react";
import Papa from "papaparse";
import { delimitedPreview, type TableSource } from "./model";
import { PreviewGrid } from "./table-preview";

// A CSV or TSV document, split by papaparse (quotes, escaped quotes and
// line breaks in quoted fields) and shown as a table. Loaded on its own,
// when one is shown.

export function Delimited({
  text,
  source,
}: {
  text: string;
  source: TableSource;
}) {
  const preview = useMemo(() => {
    const parsed = Papa.parse<string[]>(text, {
      delimiter: source === "tsv" ? "\t" : ",",
      skipEmptyLines: "greedy",
    });
    return delimitedPreview(
      parsed.data,
      parsed.errors.flatMap((e) => (e.row === undefined ? [] : [e.row])),
    );
  }, [text, source]);
  return <PreviewGrid preview={preview} source={source} />;
}
