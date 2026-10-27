import { useQueries, useQuery } from "@tanstack/react-query";
import type {
  FoundryDocument,
  FoundryOntology,
  OntologyVersion,
} from "../../../shared/wfe";
import { useWorkspace } from "../../ui";
import { fileMapping, type FileMapping } from "../ontology/model";
import { ontologyQueries } from "../ontology/queries";
import { readKindOf } from "./model";

// Which ontologies' current versions map a table file (fileMapping): what
// its Rows panel lists, and what its delete and move dialogs warn about. A
// mapped file that is deleted or moved out of the version's sets fails the
// version's next load until it is revised. Each ontology's current version
// is read (versions are immutable, so the reads are shared and cached), and
// its generation once the file isn't mapped, to tell a revision that can map
// it (its generation drafted from the file) from a new generation.

/** One ontology's current version, and how it reads the file once both are read. */
export type FileMappingRow = {
  ontology: FoundryOntology;
  version?: OntologyVersion;
  mapping?: FileMapping;
};

/**
 * How each ontology with a current version reads the table file `d`; none
 * for a document of any other kind.
 */
export function useFileMappings(d: FoundryDocument): FileMappingRow[] {
  const { id } = useWorkspace(),
    table = readKindOf(d) === "table";
  const list = useQuery({ ...ontologyQueries.list(id), enabled: table });
  const current = table
    ? (list.data?.ontologies || []).filter((o) => o.currentVersionId)
    : [];
  const versions = useQueries({
    queries: current.map((o) =>
      ontologyQueries.version(id, o.id, o.currentVersionId!),
    ),
  });
  const generations = useQueries({
    queries: current.map((o, i) => {
      const generationId = versions[i]?.data?.version?.generationId || "";
      return {
        ...ontologyQueries.generation(id, o.id, generationId),
        enabled: !!generationId,
      };
    }),
  });
  return current.map((ontology, i) => {
    const version = versions[i]?.data?.version || undefined,
      generation = generations[i];
    if (!version) return { ontology };
    const mapped = fileMapping(d.id, d.setId || "", version);
    // Whether a revision can map it waits for the generation, if any.
    return mapped.kind !== "unmapped" || !version.generationId
      ? { ontology, version, mapping: mapped }
      : generation?.data
        ? {
            ontology,
            version,
            mapping: fileMapping(
              d.id,
              d.setId || "",
              version,
              generation.data.generation || undefined,
            ),
          }
        : generation?.isError
          ? { ontology, version, mapping: mapped }
          : { ontology, version };
  });
}
