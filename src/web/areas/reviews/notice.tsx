// The overview's review notice (../ontology/home.tsx): tells an owner or
// admin that the current load left cards for a person to decide, in a title
// and one short line, with Start reviewing (the Review page's walkthrough)
// and Later, which hides it for that load in this browser, so a new load
// shows it again. The count is the review cards index's (the summary), less
// the walk's own choices; while the index is still sorting the load's
// reviews it says it is counting.
import { ListChecks } from "lucide-react";
import { Button, ButtonLink, formatNumber, useStoredState } from "../../kit";
import { paths } from "../../paths";
import { canManage, useWorkspace } from "../../ui";
import { Callout } from "../ontology/parts";
import { useOntology } from "../ontology/scope";
import { useReviewSummary } from "./cards-api";

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
  const summary = useReviewSummary().get(o);
  const counted = summary?.loadId === loadId ? summary : undefined;
  const open = counted?.open ?? 0;
  if (
    !canManage(role) ||
    !loadId ||
    later === loadId ||
    !counted ||
    (!open && !counted.indexing)
  )
    return null;
  return (
    <Callout
      icon={ListChecks}
      tone="muted"
      title={
        open
          ? `${formatNumber(open)}${counted.more ? "+" : ""} ${open === 1 && !counted.more ? "card" : "cards"} to decide`
          : "Counting the cards to decide…"
      }
      text="Names, held claims and values the load couldn’t settle."
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
