// The walkthrough's lanes beside Decide (./walkthrough.tsx): FYI, what the
// load settled on its own and a person may still change (a name the model
// linked, with Undo link; a value a table row keeps against documents, with
// Replace the table's value), and Covered, names in documents a link of the
// same name settles (the backend links it in every document asking at once).
// Neither blocks the walk's finish.
import { Info, Link2 } from "lucide-react";
import type { FoundryLoad } from "../../../shared/wfe";
import { EmptyState } from "../../kit";
import { useWorkspace } from "../../ui";
import { useDocumentNames } from "../documents/documents-api";
import type { Naming } from "../explore/naming";
import { coveredBy, type AdviceContext } from "./advice";
import type { QueueCard } from "./queue";
import { CardHead, Evidence, cardLine } from "./review-card";
import { SettleReview, type HeldDecisions } from "./settle";

/** The FYI lane's cards, each settled as a document's Needs review settles it. */
export function FyiCards({
  keys,
  cards,
  load,
  naming,
  decisions,
  settled,
}: {
  keys: readonly string[];
  cards: ReadonlyMap<string, QueueCard>;
  load: FoundryLoad;
  naming: Naming;
  // The decisions the walk reads by id: an FYI card is open, so the only
  // decisions of it are those settled here.
  decisions: HeldDecisions;
  settled: (key: string) => void;
}) {
  const shown = keys.flatMap((k) => {
    const c = cards.get(k);
    return c && (c.kind === "name" || c.kind === "value") ? [c] : [];
  });
  if (!shown.length)
    return (
      <EmptyState
        icon={Info}
        title="Nothing for information"
        text="Names the model linked and values a table keeps against documents show here."
      />
    );
  return (
    <ol className="rq-list" aria-label="For information">
      {shown.map((c) => (
        <li key={c.key} className="rq-card">
          <CardHead
            title={
              c.kind === "name"
                ? "A name the model linked"
                : "A value the table keeps"
            }
          />
          <Evidence at={c.review} quote={c.review.quote} />
          <SettleReview
            review={c.review}
            load={load}
            link={(path) => path}
            naming={naming}
            decisions={decisions}
            drawn="ranked"
            onSettled={() => settled(c.key)}
          />
        </li>
      ))}
    </ol>
  );
}

/**
 * The Covered lane: each name, its document, and the link that settles it,
 * this walk's (`coveredBy`, words naming the document linked in) or an
 * earlier decision's. Taking the link back opens them again.
 */
export function CoveredCards({
  keys,
  cardOf,
  ctx,
  coveredBy: walkCover,
}: {
  keys: readonly string[];
  cardOf: (key: string) => QueueCard | undefined;
  ctx: Pick<AdviceContext, "names">;
  coveredBy: (key: string) => string | undefined;
}) {
  const { id: ws } = useWorkspace();
  const docName = useDocumentNames(ws, "a document");
  const shown = keys.flatMap((k) => {
    const c = cardOf(k);
    return c?.kind === "name" ? [c] : [];
  });
  if (!shown.length)
    return (
      <EmptyState
        icon={Link2}
        title="Nothing covered"
        text="Linking a name the load asks about links it in every document asking. Those documents’ cards show here."
      />
    );
  return (
    <ul className="rv-covered" aria-label="Covered">
      {shown.map((c) => {
        const earlier = coveredBy(c, ctx);
        const to = earlier
          ? (c.review.candidates.find((x) => x.key === earlier.entityKey)
              ?.entity?.name ?? earlier.entityKey)
          : undefined;
        return (
          <li key={c.key}>
            <strong>{cardLine(c)}</strong> in {docName(c.review.documentId)}:{" "}
            {walkCover(c.key)
              ? `settled by your link ${walkCover(c.key)}`
              : earlier
                ? `settled by a link to ${to} in ${docName(earlier.docId)}`
                : "settled by a link of the same name"}
            .
          </li>
        );
      })}
    </ul>
  );
}
