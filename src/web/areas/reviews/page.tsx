// The Review page (/ontology/:ontologyId/review), lazy-loaded by the
// Ontology area's route table: what needs a person, in two tabs, the tab in
// the URL (?tab=ontology; Facts, the default, writes none). Facts is the
// review walkthrough (./index.tsx): what the current load couldn't decide,
// one card at a time. Ontology is the Suggestions area's
// list: what audits of the current load found the ontology lacks, acted on
// by drafting a new version. Where this Foundry doesn't audit loads, there
// is no Ontology tab. Each tab counts what is open in it: Facts, the review
// cards index's count (the summary, which the Ontologies panel and the job
// dock read too), less this walk's choices.
import { useQuery } from "@tanstack/react-query";
import {
  Page,
  PageHeader,
  TabPanel,
  Tabs,
  useTabParam,
  useTitle,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { ontologyName, useOntology } from "../ontology/scope";
import { OntologySuggestions } from "../suggestions";
import { suggestionQueries } from "../suggestions/api";
import { FactsReview, useReviewCards } from "./index";

const SUBTITLES = {
  facts:
    "What the current load couldn’t settle on its own, one card at a time, with Founder’s suggestion where a rule makes one.",
  ontology:
    "What audits of the current load found the ontology lacks. Acting on one drafts a new version.",
};

export function ReviewPage() {
  const { id: ws } = useWorkspace(),
    { id: o, ontology } = useOntology();
  useTitle("Review");
  const queue = useReviewCards(),
    suggestions = useQuery(suggestionQueries.list(ws, o));
  // The tab shows until the list says audits are off, so a link to it
  // doesn't open Facts first.
  const audits = suggestions.data?.enabled !== false;
  const [tab] = useTabParam(audits ? ["facts", "ontology"] : ["facts"]);
  return (
    <Page>
      <PageHeader
        breadcrumbs={[
          { label: "Ontologies", to: paths.ontologies() },
          { label: ontologyName(ontology), to: paths.ontology(o) },
          { label: "Review" },
        ]}
        title="Review"
        subtitle={tab === "ontology" ? SUBTITLES.ontology : SUBTITLES.facts}
        tabs={
          <Tabs
            label="Review"
            idPrefix="review"
            items={[
              { id: "facts", label: "Facts", count: queue.open },
              ...(audits
                ? [
                    {
                      id: "ontology",
                      label: "Ontology",
                      count: suggestions.data?.findings?.length,
                    },
                  ]
                : []),
            ]}
          />
        }
      />
      <TabPanel id="facts" value={tab} idPrefix="review" className="rv-tab">
        <FactsReview queue={queue} />
      </TabPanel>
      <TabPanel id="ontology" value={tab} idPrefix="review" className="rv-tab">
        <OntologySuggestions />
      </TabPanel>
    </Page>
  );
}
