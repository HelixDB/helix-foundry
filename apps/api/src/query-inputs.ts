import type { Resource } from "../../../packages/shared/src/index.js";
import {
  datasetTable,
  datasetAliases,
} from "../../../packages/shared/src/index.js";

// Whether the SQL reads an identifier as a table: right after FROM or JOIN,
// bare or double-quoted, as a whole token. A dataset called "count" must not
// match COUNT(*), and "orders" must not match "orders_archive".
function reads(sql: string, identifier: string) {
  const escaped = identifier.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return new RegExp(
    `\\b(from|join)\\s+(?:"${escaped}"|${escaped}(?![a-z0-9_]))`,
    "i",
  ).test(sql);
}

// The datasets an answer query reads, which are the ones it cites and the
// ones loaded into the sandbox. A dataset table (d_…) counts wherever it
// appears, comma joins included; a dataset name or alias counts after FROM
// or JOIN. Every dataset the SQL reads is an input, whether or not the model
// declared it; a declared dataset the SQL never reads is not. Only when the
// SQL reads nothing that can be recognised does the declaration stand as
// the model wrote it.
export function queryInputs(
  sql: string,
  listed: string[],
  datasets: Map<string, Resource>,
): string[] {
  const declared = listed.map((ref) => {
    if (datasets.has(ref)) return ref;
    const matches = [...datasets.values()].filter(
      (d) =>
        d.name.toLowerCase() === ref.toLowerCase() ||
        datasetTable(d.id) === ref,
    );
    return matches.length === 1 ? matches[0].id : ref;
  });
  const named = new Set(
    (sql.match(/\bd_[a-z0-9_]+\b/gi) || []).map((t) => t.toLowerCase()),
  );
  const read = [...datasets.keys()].filter(
    (id) =>
      named.has(datasetTable(id).toLowerCase()) ||
      datasetAliases(datasets.get(id)!.name).some((t) => reads(sql, t)),
  );
  if (read.length)
    return [
      ...new Set([...declared.filter((id) => read.includes(id)), ...read]),
    ];
  return [...new Set(declared.filter((ref) => datasets.has(ref)))];
}
