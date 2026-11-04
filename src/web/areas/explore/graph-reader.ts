import { useWorkspace } from "../../ui";
import { useOntology } from "../ontology/scope";
import {
  readAccess,
  readsPart,
  type ReadAccess,
} from "../workspace/access-model";
import type { GraphReader } from "./reads";
import { useTenantReads } from "./tenant-reads";

// The one place that decides how the pages of an ontology read its loads
// (Records, an entity's page, a record's drawer, a document's view, Ask's
// citations): the host's callers read its database with their session; a
// user reading through source grants (a grantee, or a member who can't
// query the ontology's project) reads the one load those grants cover
// through the WFE's typed reads (./typed-reads.ts), and someone who can do
// neither reads nothing. A local build has one owner, who reads its
// database.

/** How the user reads the page's ontology (../workspace/access-model.ts readAccess). */
export function useReadAccess(): ReadAccess {
  const me = useWorkspace(),
    { ontology } = useOntology();
  return readAccess(me, ontology);
}

/**
 * Whether the user reads only part of the page's ontology, through grants that
 * hold only some of its current load.
 */
export function useReadsPart(): boolean {
  const me = useWorkspace(),
    { ontology } = useOntology();
  return readsPart(me, ontology);
}

/**
 * The page's access and its reader: the database's, through its shared
 * limiter (`later` waits its turn, for background reads), or typed reads.
 * Nothing should be read while `access` is "none".
 */
export function useGraphReader(later = false): {
  access: ReadAccess;
  reader: GraphReader;
} {
  const access = useReadAccess(),
    { ontology } = useOntology(),
    reads = useTenantReads();
  return {
    access,
    reader:
      access === "database"
        ? { kind: "database", read: later ? reads.readLater : reads.read }
        : { kind: "grants", createdAt: ontology.createdAt },
  };
}
