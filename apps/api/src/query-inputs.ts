import type { Resource } from "../../../packages/shared/src/index.js";
import {
  datasetTable,
  datasetAliases,
} from "../../../packages/shared/src/index.js";

const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

// Text in a literal or comment is data, not a dataset reference. Keep quoted
// identifiers intact and replace only non-code spans; execution still receives
// the original SQL. This is a lexical filter, not a SQL parser.
function referenceSql(sql: string) {
  let result = "",
    start = 0,
    i = 0;
  const blank = (end: number) => {
    result += sql.slice(start, i) + sql.slice(i, end).replace(/[^\r\n]/g, " ");
    start = i = end;
  };
  while (i < sql.length) {
    if (sql[i] === '"') {
      // Double quotes delimit identifiers; doubled quotes are part of the name.
      i++;
      while (i < sql.length) {
        if (sql[i++] === '"') {
          if (sql[i] !== '"') break;
          i++;
        }
      }
    } else if (sql[i] === "'") {
      const escaped = /(?:^|[^a-z0-9_$])e$/i.test(
        sql.slice(Math.max(0, i - 2), i),
      );
      let end = i + 1;
      // The E of an escape string is syntax too, not a reference to a table E.
      if (escaped) i--;
      while (end < sql.length) {
        if (escaped && sql[end] === "\\") end += 2;
        else if (sql[end++] === "'") {
          if (sql[end] === "'") {
            end++;
            continue;
          }
          // A newline can continue the same string, retaining its E escape
          // mode. Line comments are whitespace in this continuation grammar.
          let next = end,
            newline = false;
          while (next < sql.length) {
            if (/[ \t\f\r\n]/.test(sql[next])) {
              newline ||= /[\r\n]/.test(sql[next]);
              next++;
            } else if (sql.startsWith("--", next)) {
              while (next < sql.length && !/[\r\n]/.test(sql[next])) next++;
            } else break;
          }
          if (!newline || sql[next] !== "'") break;
          end = next + 1;
        }
      }
      blank(Math.min(end, sql.length));
    } else if (sql.startsWith("--", i)) {
      const end = sql.slice(i).search(/[\r\n]/);
      blank(end < 0 ? sql.length : i + end);
    } else if (sql.startsWith("/*", i)) {
      let end = i + 2,
        depth = 1;
      while (end < sql.length && depth) {
        if (sql.startsWith("/*", end)) {
          depth++;
          end += 2;
        } else if (sql.startsWith("*/", end)) {
          depth--;
          end += 2;
        } else end++;
      }
      blank(end);
    } else {
      const delimiter =
        sql[i] === "$" && !/[a-z0-9_$\u0080-\uffff]/i.test(sql[i - 1] || "")
          ? /^\$(?:[a-z_\u0080-\uffff][a-z0-9_\u0080-\uffff]*)?\$/i.exec(
              sql.slice(i),
            )?.[0]
          : undefined;
      if (delimiter) {
        const end = sql.indexOf(delimiter, i + delimiter.length);
        blank(end < 0 ? sql.length : end + delimiter.length);
      } else i++;
    }
  }
  return result + sql.slice(start);
}

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
  sql = referenceSql(sql);
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
