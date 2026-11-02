import { useQuery } from "@tanstack/react-query";
import { Link, useLocation } from "react-router-dom";
import { useWorkspace } from "../../ui";
import { canManage } from "./parts";
import { ontologyPaths as paths } from "./paths";
import { isLiveQuestion, ontologyQueries, questionQueries } from "./queries";
import { sampleInView } from "./sample";
import { useSampleOntology } from "./use-sample-ontology";

// Across the top of the app while the workspace is on the sample company
// (AreaShell's banner, ./shell.ts): what it is looking at, and the way back to
// the sample's own page, where it sees what is done. Once the tour's question
// (the ontology's oldest) has ended and the sample is still its only ontology,
// the user's own data is next, so the link opens that page on its last step.
// That page is for owners and admins, so only they get the link.
export function SampleBanner() {
  const { id, role } = useWorkspace(),
    { pathname } = useLocation();
  const sample = useSampleOntology(),
    ontologies = useQuery({ ...ontologyQueries.list(id), enabled: false }).data
      ?.ontologies?.length,
    inView = !!sample && sampleInView(pathname, sample.id, ontologies || 0),
    link = canManage(role) && pathname !== paths.sampleTour();
  const oldest = useQuery({
    ...questionQueries.list(id, sample?.id || ""),
    enabled: inView && link && ontologies === 1,
  }).data?.questions?.at(-1);
  if (!inView) return null;
  const ownData = !!oldest && !isLiveQuestion(oldest);
  return (
    <div className="hf-banner" role="region" aria-label="Sample data">
      <span>
        Sample data: you’re exploring Acme Corp, a made-up company.
        {link && (
          <>
            {" "}
            <Link to={ownData ? paths.sampleOwnData() : paths.sampleTour()}>
              {ownData
                ? "Get started with your own data"
                : "Back to getting started"}
            </Link>
          </>
        )}
      </span>
    </div>
  );
}
