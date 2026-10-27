import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspace } from "../../ui";
import { ontologyQueries } from "../ontology/queries";
import { useOntology } from "../ontology/scope";
import { namingOf, type Naming } from "./naming";

/**
 * The naming rules of a version of the page's ontology: the version of the
 * load the page reads (../ontology/load-selection.tsx). `ready` once they
 * are known: at once with no version, else when the version has been read
 * (or couldn't be: then nothing is named, every label keeps its raw label
 * and id, and `error` says why, for a page that can't do without the
 * version; `refetch` reads it again).
 */
export function useNaming(versionId: string): {
  naming: Naming;
  ready: boolean;
  error: Error | null;
  refetch: () => void;
} {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const current = versionId;
  const version = useQuery({
    ...ontologyQueries.version(ws, o, current),
    enabled: !!current,
  });
  const data = version.data?.version;
  const naming = useMemo(() => namingOf(data), [data]);
  return {
    naming,
    ready: !current || version.isSuccess || version.isError,
    error: current ? version.error : null,
    refetch: () => void version.refetch(),
  };
}
