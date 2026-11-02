import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspace } from "../../ui";
import { ontologyQueries } from "../ontology/queries";
import { useOntology } from "../ontology/scope";
import { graphReadQueries } from "./graph-read";
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
    ...graphReadQueries.view(ws, o, loadId, ontology.createdAt),
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
