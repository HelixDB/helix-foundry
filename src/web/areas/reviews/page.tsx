// The Review page (/ontology/:ontologyId/review), lazy-loaded by the
// Ontology area's route table: what needs a person, in two tabs, the tab in
// the URL (?tab=ontology; Facts, the default, writes none). Facts is the
// review queue (./index.tsx): what the current load couldn't decide, whose
// decisions apply from the next load. Ontology is the Suggestions area's
// list: what audits of the current load found the ontology lacks, acted on
// by drafting a new version. Where this Foundry doesn't audit loads, there
// is no Ontology tab. Each tab counts what is open in it, and the page
// keeps the Facts count where the Ontologies panel's Review link reads it.
import { useEffect } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
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
import { reviewQueries, type OpenFacts } from "./api";
import { FactsReview, useReviewQueue } from "./index";

const SUBTITLES = {
  facts:
    "What the current load couldn’t decide: which record a name means, and values documents disagree on. Decisions apply from the next load.",
  ontology:
    "What audits of the current load found the ontology lacks. Acting on one drafts a new version of the ontology.",
};

export function ReviewPage() {
  const { id: ws } = useWorkspace(),
    { id: o, ontology } = useOntology();
  useTitle("Review");
  const qc = useQueryClient();
  const queue = useReviewQueue(),
    suggestions = useQuery(suggestionQueries.list(ws, o));
  // The tab shows until the list says audits are off, so a link to it
  // doesn't open Facts first.
  const audits = suggestions.data?.enabled !== false;
  const [tab] = useTabParam(audits ? ["facts", "ontology"] : ["facts"]);
  const loadId = queue.load?.id ?? "",
    open = queue.open;
  useEffect(() => {
    if (open !== undefined && loadId)
      qc.setQueryData<OpenFacts>(reviewQueries.openFacts(ws, o).queryKey, {
        loadId,
        open,
      });
  }, [qc, ws, o, loadId, open]);
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
              { id: "facts", label: "Facts", count: open },
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
      <TabPanel id="facts" value={tab} idPrefix="review">
        <FactsReview queue={queue} />
      </TabPanel>
      <TabPanel id="ontology" value={tab} idPrefix="review">
        <OntologySuggestions />
      </TabPanel>
    </Page>
  );
}
