import { useQuery } from "@tanstack/react-query";
import type { FoundryOntology } from "../../../shared/wfe";
import { useWorkspace } from "../../ui";
import { ontologyQueries } from "./queries";
import { SAMPLE } from "./sample";

/**
 * The workspace's sample ontology: undefined while that is being read, null
 * when it has none. The ontology list has no sample field, so it goes by the
 * name, in the list the app already keeps (main.tsx's WorkspaceApp, so this
 * reads it only from the cache), then confirms it with that one ontology's
 * generations, one of which drafts from the sample: no read per ontology. A
 * generation's `sample` never changes, so it doesn't poll them.
 */
export function useSampleOntology(): FoundryOntology | null | undefined {
  const { id } = useWorkspace();
  const named = useQuery({
    ...ontologyQueries.list(id),
    enabled: false,
  }).data?.ontologies?.find((o) => o.name === SAMPLE.ontology);
  const generations = useQuery({
    ...ontologyQueries.generations(id, named?.id || ""),
    enabled: !!named,
    refetchInterval: false,
  });
  if (!named) return null;
  if (generations.isPending) return undefined;
  return generations.data?.generations?.some((g) => g.sample === SAMPLE.id)
    ? named
    : null;
}
