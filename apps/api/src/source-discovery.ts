import type { Resource } from "../../../packages/shared/src/index.js";
import { publicResource } from "./onboarding-state.js";

type Row = Record<string, any>;

// What a source keeps of its connection's discovery: its own table's declared
// keys and foreign keys, the only part anything reads again (declaredKeys in
// discovery.ts). The rest describes the whole database, so a copy on every
// source grew with the database, and one connection's import outgrew a
// HelixDB request (#94). `selection` is the source's settings, with its table.
export function storedDiscovery(
  kind: string,
  discovery: Row | undefined,
  selection: { table?: unknown; schema?: unknown },
) {
  const table = String(selection.table ?? "").toLowerCase();
  // An import reads public when no schema is named.
  const schema =
    kind === "postgres" ? selection.schema || "public" : selection.schema;
  // Names compare as declaredKeys matches them, ignoring case. A row names
  // its schema in PostgreSQL and Neo4j; rows stored by older releases may not.
  const own = (k: Row) =>
    !!table &&
    String(k.tableName ?? k.table_name ?? "").toLowerCase() === table &&
    (!schema || k.table_schema === undefined || k.table_schema === schema);
  const kept: Row = {};
  for (const part of ["keys", "foreignKeys"]) {
    const rows = discovery?.[part];
    if (Array.isArray(rows)) kept[part] = rows.filter(own);
  }
  return kept;
}

// An import operation keeps which sources it created, as one connection, and
// their jobs; onboardingState groups older sources by it.
export const importedItem = (
  source: { id: string; name: string },
  connectionId: string,
  job: { id: string },
) => ({
  source: { id: source.id, name: source.name, connectionId },
  job: { id: job.id },
});

// A repeated import answers as the first did, with the sources and jobs as
// they are now. One that was removed since keeps what the operation stored.
export async function importResponse(
  items: any[],
  read: (ids: string[]) => Promise<(Resource | undefined)[]>,
) {
  const found = await read(items.flatMap((i) => [i.source?.id, i.job?.id]));
  return items.map((item, n) => {
    const source = found[2 * n],
      job = found[2 * n + 1];
    return {
      source: source ? publicResource(source) : item.source,
      job: job || item.job,
    };
  });
}
