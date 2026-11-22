// The overview's review notice (../ontology/home.tsx): tells an owner or
// admin that the current load's documents left reviews for a person, with
// Start reviewing (the Review page's walkthrough) and Later, which hides it
// for that load in this browser, so a new load shows it again. Once the
// Review page has read the load, it says the page's count, the cards still
// to decide (none: no notice); until then, what the load's documents left,
// worded as reviews, settled since or not (the load says nothing of what is
// still open, and one card may settle several reviews).
import { useQuery } from "@tanstack/react-query";
import { ListChecks } from "lucide-react";
import { Button, ButtonLink, plural, useStoredState } from "../../kit";
import { paths } from "../../paths";
import { reviewQueries } from "./api";
import { canManage, useWorkspace } from "../../ui";
import { loadDocumentTotals } from "../ontology/model";
import { Callout } from "../ontology/parts";
import { ontologyQueries } from "../ontology/queries";
import { useOntology } from "../ontology/scope";

/** Mounted with `key` set to the ontology's id, so Later reads per ontology. */
export function ReviewNotice() {
  const { id, role } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const loadId = ontology.currentLoad?.id ?? "";
  // The load Later was pressed on.
  const [later, setLater] = useStoredState(
    `foundry.review.notice.later.v1:${id}:${o}`,
    "",
  );
  const shown = canManage(role) && !!loadId && later !== loadId;
  const load = useQuery({
    ...ontologyQueries.load(id, o, loadId),
    enabled: shown,
  }).data?.load;
  const facts = useQuery(reviewQueries.openFacts(id, o)).data;
  const toDecide = facts?.loadId === loadId ? facts.open : undefined;
  const reviews = (load && loadDocumentTotals(load)?.reviews) || 0;
  if (!shown || !(toDecide ?? reviews)) return null;
  return (
    <Callout
      icon={ListChecks}
      tone="muted"
      title={
        toDecide === undefined
          ? `This load’s documents left ${plural(reviews, "review")} for a person`
          : `This load has ${plural(toDecide, "card")} to decide`
      }
      text="Names it couldn’t match, claims it held back and values documents disagree on; one choice may settle several reviews. Founder suggests a choice where its rules can; each one you settle applies now or from the next load."
      action={
        <span className="on-card-actions">
          <ButtonLink size="sm" variant="primary" to={paths.review(o)}>
            Start reviewing
          </ButtonLink>
          <Button size="sm" variant="ghost" onClick={() => setLater(loadId)}>
            Later
          </Button>
        </span>
      }
    />
  );
}
