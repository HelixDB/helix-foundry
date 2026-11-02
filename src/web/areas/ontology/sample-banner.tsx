import { useQuery } from "@tanstack/react-query";
import { Link, useLocation } from "react-router-dom";
import { useWorkspace } from "../../ui";
import { canManage } from "./parts";
import { ontologyPaths as paths } from "./paths";
import { ontologyQueries } from "./queries";
import { sampleInView } from "./sample";
import { useSampleOntology } from "./use-sample-ontology";

// Across the top of the app while the workspace is on the sample company
// (AreaShell's banner, ./shell.ts): what it is looking at, and the way back
// to the sample's own page, where it sees what is done. That page is for
// owners and admins, so only they get the link.
export function SampleBanner() {
  const { id, role } = useWorkspace(),
    { pathname } = useLocation();
  const sample = useSampleOntology(),
    ontologies = useQuery({ ...ontologyQueries.list(id), enabled: false }).data
      ?.ontologies?.length;
  if (!sample || !sampleInView(pathname, sample.id, ontologies || 0))
    return null;
  return (
    <div className="hf-banner" role="region" aria-label="Sample data">
      <span>
        Sample data: you’re exploring Acme Corp, a made-up company.
        {canManage(role) && pathname !== paths.sampleTour() && (
          <>
            {" "}
            <Link to={paths.sampleTour()}>Back to getting started</Link>
          </>
        )}
      </span>
    </div>
  );
}
