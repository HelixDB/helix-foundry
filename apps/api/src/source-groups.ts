import type { Resource } from "../../../packages/shared/src/index.js";
import type { Tx } from "./store.js";

export async function connectionSources(tx: Tx, source: Resource) {
  const operations = await tx.list("operation"),
    legacy = new Map<string, string>();
  for (const op of operations)
    if (Array.isArray(op.data.result))
      for (const item of op.data.result)
        if (item.source?.id) legacy.set(item.source.id, op.id);
  const key = (s: Resource) => s.data.connectionId || legacy.get(s.id) || s.id;
  return (await tx.list("source")).filter((s) => key(s) === key(source));
}
