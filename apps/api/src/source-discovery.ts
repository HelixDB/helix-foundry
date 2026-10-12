import type { Resource } from "../../../packages/shared/src/index.js";
import { now, resource, transaction, type Store } from "./store.js";
import { unseal } from "./security.js";
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

const MIGRATIONS = "migration_state";

// Releases before #94 stored the whole discovery on every source, and again
// on its import operation. Once per workspace, each is cut down to what is
// read now, one record per transaction, since a single old record can be near
// the request limit. The reduced shape still has keys and foreignKeys, so an
// older release reads it as before.
export async function reduceStoredDiscovery(store: Store, scope: string) {
  if ((await store.get(scope, MIGRATIONS))?.data.storedDiscovery) return;
  let unreadable = 0;
  for (const listed of await store.list(scope, "source")) {
    if (!listed.data.discovery) continue;
    const readable = await transaction(store, scope, async (tx) => {
      const s = await tx.get(listed.id);
      if (!s?.data.discovery) return true;
      let settings: Row;
      try {
        settings = unseal(s.data.secret);
      } catch {
        return false;
      }
      const discovery = storedDiscovery(
        s.data.kind,
        s.data.discovery,
        settings,
      );
      if (JSON.stringify(discovery) !== JSON.stringify(s.data.discovery))
        tx.put({
          ...s,
          revision: s.revision + 1,
          data: { ...s.data, discovery },
        });
      return true;
    });
    if (!readable) {
      // This key cannot decrypt its settings, so it cannot sync either.
      // Correcting the source stores the reduced shape; until then the pass
      // runs again at each start.
      unreadable++;
      console.error(
        JSON.stringify({
          level: "warn",
          msg: "Stored discovery not reduced: source settings unreadable",
          scope,
          sourceId: listed.id,
        }),
      );
    }
  }
  const full = (item: any) => !!item?.source?.data;
  for (const listed of await store.list(scope, "operation")) {
    if (!Array.isArray(listed.data.result) || !listed.data.result.some(full))
      continue;
    await transaction(store, scope, async (tx) => {
      const op = await tx.get(listed.id);
      if (!Array.isArray(op?.data.result) || !op.data.result.some(full)) return;
      tx.put({
        ...op,
        revision: op.revision + 1,
        data: {
          ...op.data,
          result: op.data.result.map((item: any) =>
            full(item)
              ? importedItem(
                  item.source,
                  item.source.data.connectionId || op.id,
                  item.job || {},
                )
              : item,
          ),
        },
      });
    });
  }
  if (!unreadable)
    await transaction(store, scope, async (tx) => {
      const m = await tx.get(MIGRATIONS);
      tx.put(
        m
          ? {
              ...m,
              revision: m.revision + 1,
              data: { ...m.data, storedDiscovery: now() },
            }
          : resource(
              scope,
              "migrationState",
              "Migrations",
              { storedDiscovery: now() },
              MIGRATIONS,
            ),
      );
    });
}
