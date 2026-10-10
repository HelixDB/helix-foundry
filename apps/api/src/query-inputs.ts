import type { Resource } from "../../../packages/shared/src/index.js";
import {
  datasetTable,
  datasetAliases,
} from "../../../packages/shared/src/index.js";

const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

// The table references of a query: what follows FROM, JOIN or PIVOT, split
// at commas so each table of a comma join ("FROM orders o, customers c")
// starts its own reference. A reference runs to the next clause keyword or
// bracket; quoted names are skipped whole.
function references(sql: string) {
  const clause =
    /\b(?:from|join|pivot|unpivot)\s+((?:"[^"]*"|[\s\S])*?)(?=\b(?:where|group|order|having|limit|offset|join|left|right|inner|outer|full|cross|natural|positional|asof|semi|anti|lateral|on|using|union|except|intersect|window|qualify|select|returning)\b|[();]|$)/gi;
  return [...sql.matchAll(clause)].flatMap((m) =>
    m[1].split(",").map((s) => s.trim()),
  );
}

// Whether a reference starts with the identifier, bare or double-quoted, as a
// whole token: a dataset called "count" must not match COUNT(*), and "orders"
// must not match "orders_archive".
const starts = (ref: string, identifier: string) =>
  new RegExp(
    `^(?:"${escape(identifier)}"|${escape(identifier)}(?![a-z0-9_]))`,
    "i",
  ).test(ref);

// Whether the identifier appears anywhere in the SQL as a whole token.
const mentions = (sql: string, identifier: string) =>
  new RegExp(
    `(?:"${escape(identifier)}"|(?<![a-z0-9_])${escape(identifier)}(?![a-z0-9_]))`,
    "i",
  ).test(sql);

// The datasets an answer query reads, which are the ones it cites and the
// ones loaded into the sandbox.
// - A dataset table (d_…) counts wherever it appears.
// - A dataset's name or alias (its readable table name) counts as a table
//   reference, whether or not the model declared it.
// - A declared dataset also counts if the SQL mentions it anywhere, so a
//   table in a construct the matcher does not parse is still loaded. A
//   declared dataset the SQL never mentions is dropped.
// Only when the SQL mentions none of them does the declaration stand.
export function queryInputs(
  sql: string,
  listed: string[],
  datasets: Map<string, Resource>,
): string[] {
  const names = (id: string) => datasetAliases(datasets.get(id)!.name);
  const declared = listed.map((ref) => {
    if (datasets.has(ref)) return ref;
    const matches = [...datasets.keys()].filter(
      (id) =>
        datasetTable(id) === ref ||
        names(id).some((n) => n.toLowerCase() === ref.toLowerCase()),
    );
    return matches.length === 1 ? matches[0] : ref;
  });
  const named = new Set(
    (sql.match(/\bd_[a-z0-9_]+\b/gi) || []).map((t) => t.toLowerCase()),
  );
  const refs = references(sql);
  const read = [...datasets.keys()].filter(
    (id) =>
      named.has(datasetTable(id).toLowerCase()) ||
      names(id).some((n) => refs.some((ref) => starts(ref, n))),
  );
  const mentioned = declared.filter(
    (id) =>
      datasets.has(id) &&
      !read.includes(id) &&
      names(id).some((n) => mentions(sql, n)),
  );
  if (read.length || mentioned.length)
    return [
      ...new Set([
        ...declared.filter((id) => read.includes(id) || mentioned.includes(id)),
        ...read,
      ]),
    ];
  return [...new Set(declared.filter((ref) => datasets.has(ref)))];
}
