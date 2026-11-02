import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { outcomeOf, parseViewResult } from "../../../shared/foundry-read";
import { useWorkspace } from "../../ui";
import { ontologyQueries } from "../ontology/queries";
import { useOntology } from "../ontology/scope";
import { readGraph } from "./graph-read";
import { useReadsPart } from "./graph-reader";
import { namingOf, type Naming } from "./naming";

/**
 * The naming rules of a version of the page's ontology: the version of the
 * load the page reads (../ontology/load-selection.tsx). `ready` once they
 * are known: at once with no version, else when the version has been read
 * (or couldn't be: then nothing is named, every label keeps its raw label
 * and id, and `error` says why, for a page that can't do without the
 * version; `refetch` reads it again). Someone who reads only part of the
 * ontology names only what their sources fill: a restricted member's version
 * already says so, and anyone else's is read as the load's view.
 */
export function useNaming(versionId: string): {
  naming: Naming;
  ready: boolean;
  error: Error | null;
  refetch: () => void;
} {
  const { id: ws, grantee } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    part = useReadsPart();
  const current = versionId;
  const version = useQuery({
    ...ontologyQueries.version(ws, o, current),
    enabled: !!current,
  });
  const loadId = ontology.currentLoad?.id || "",
    viewed = part && !grantee && !!loadId;
  const view = useQuery({
    queryKey: [ws, "ontology", o, "read", loadId, "view", current],
    queryFn: async ({ signal }: { signal?: AbortSignal }) => {
      const [outcome] = await readGraph(ws, o, loadId, [{ view: {} }], signal);
      const r = outcomeOf(outcome, parseViewResult);
      if (!r.ok) throw new Error(r.error.message);
      return r.value;
    },
    enabled: viewed && !!current,
  });
  const data = version.data?.version,
    seen = viewed ? view.data : undefined;
  const naming = useMemo(
    () =>
      namingOf(
        data && seen
          ? {
              ...data,
              definition: {
                ...data.definition,
                ontology_toml: seen.ontologyToml,
                mappings_toml: seen.mappingsToml,
              },
            }
          : data,
      ),
    [data, seen],
  );
  const viewSettled = !viewed || view.isSuccess || view.isError;
  return {
    naming,
    ready: !current || ((version.isSuccess || version.isError) && viewSettled),
    error: current ? version.error || (viewed ? view.error : null) : null,
    refetch: () => void (version.error ? version.refetch() : view.refetch()),
  };
}
