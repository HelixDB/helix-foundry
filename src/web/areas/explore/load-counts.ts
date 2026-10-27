import type { FoundryLoad, OntologyVersion } from "../../../shared/wfe";
import type { QueryExecutor } from "./database-query";
import { loadCountRequests, parseLoadLabelCounts } from "./load-scope";
import { namingOf } from "./naming";
import { readOptions, runRead } from "./reads";

// What a load holds, per type and link of its version, as bitmap counts in
// the load: the Loads page sums them, the compare diff sets two side by side.
// One TanStack Query entry per load and version, so both read it once. A
// new database still starting is read again after its wait (readOptions).

export const loadCountsQuery = (
  ws: string,
  o: string,
  read: QueryExecutor,
  load: Pick<FoundryLoad, "id" | "status">,
  version: OntologyVersion | null | undefined,
) => {
  const naming = namingOf(version);
  const batches = loadCountRequests(load.id, {
    nodes: [...naming.types.keys()],
    edges: [...naming.links],
  });
  return {
    queryKey: [ws, "ontology", o, "read", load.id, "counts", naming.version],
    enabled:
      !!version &&
      load.status !== "deleting" &&
      load.status !== "delete_failed",
    queryFn: async ({ signal }: { signal?: AbortSignal }) => {
      const bodies: unknown[] = [];
      for (const batch of batches)
        bodies.push(await runRead(read, batch.request, signal));
      return parseLoadLabelCounts(bodies, batches);
    },
    ...readOptions,
    staleTime: 30_000,
  };
};
