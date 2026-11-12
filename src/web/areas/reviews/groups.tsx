import { useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Sparkles } from "lucide-react";
import type { FoundryLoad, FoundryReviewDecision } from "../../../shared/wfe";
import { Button, ButtonLink, ConfirmDialog, plural } from "../../kit";
import { paths } from "../../paths";
import { canManage, useWorkspace } from "../../ui";
import { useOntology } from "../ontology/scope";
import type { Naming } from "../explore/naming";
import { reviewQueries, settleReview } from "./api";
import {
  approvable,
  linksGoal,
  reviewGroups,
  statementHash,
  type Settle,
  type Settleable,
} from "./model";
import "./reviews.css";

/** A review as Needs review lists it on a document's or an entity's page. */
type Review = Settleable & {
  reason: string;
  quote: string;
  suggestedLink?: string;
};

/** How many approvals Approve all has in flight at once. */
const APPROVE_AT_ONCE = 4;

/**
 * A page's reviews as items of its Needs review list, grouped by reason
 * (reviewGroups): each group's title, count and explanation once, then the
 * page's compact row for each review (`row`, which draws its `<li>` and its
 * settle controls). Managers get Approve all on a group of held claims and Add
 * these links on the group of relationships the ontology doesn't have, both on
 * the load a person can settle (`load`, current).
 */
export function ReviewGroups<R extends Review>({
  reviews,
  load,
  naming,
  row,
}: {
  reviews: readonly R[];
  load?: FoundryLoad;
  // The rules of the page's version: which links hold one value.
  naming: Naming;
  row: (review: R, index: number) => ReactNode;
}) {
  return reviewGroups(reviews).map((g) => {
    const members = g.items.map((i) => i.review);
    return (
      <li key={g.title} className="dc-review-group">
        <div className="dc-review-head">
          <h3>
            {g.title} <span className="dc-review-count">{g.items.length}</span>
          </h3>
          <p>{g.text}</p>
          <ApproveAll
            reviews={members}
            all={reviews}
            load={load}
            naming={naming}
          />
          {g.reason === "unmatched" && (
            <AddLinks reviews={members} load={load} />
          )}
        </div>
        <ul className="dc-doc-list" aria-label={g.title}>
          {g.items.map(({ review, index }) => row(review, index))}
        </ul>
      </li>
    );
  });
}

/**
 * Approves every held claim of a group a person could approve on its card now
 * (approvable), once they confirm, by the same settle call a card sends, each
 * bound to its own card's statement. Offered for two or more: one card has its
 * own Approve. Each approval saved shows on its card at once; one that fails
 * leaves its card as it is, and the result says why. The backend applies
 * approvals that land together in one job, so the load writes them in one go.
 * Rivals on a link that holds one value are looked for among the page's
 * reviews (`all`), whichever group they are in.
 */
function ApproveAll({
  reviews,
  all,
  load,
  naming,
}: {
  reviews: readonly Settleable[];
  all: readonly Settleable[];
  load?: FoundryLoad;
  naming: Naming;
}) {
  const { id: ws, role } = useWorkspace();
  const { id: o } = useOntology();
  const qc = useQueryClient();
  const decisions = useQuery(reviewQueries.list(ws, o));
  const [confirming, setConfirming] = useState(false);
  const [progress, setProgress] = useState<{ done: number; total: number }>();
  const [result, setResult] = useState("");
  if (!canManage(role)) return null;
  // Saved decisions decide what is already approved, so none is offered
  // until they're read. A link of a version that isn't known holds one
  // value, as a card takes it.
  const ready =
    load?.current && decisions.data
      ? approvable(
          all,
          decisions.data,
          (link) => !naming.version || naming.oneValued.has(link),
        ).filter((s) => reviews.some((r) => r.settle === s))
      : [];
  const run = async (
    cards: readonly Extract<Settle, { kind: "release" }>[],
  ) => {
    setConfirming(false);
    setResult("");
    setProgress({ done: 0, total: cards.length });
    const failed: string[] = [];
    let next = 0,
      done = 0;
    // A few senders share one queue, so a long contract's approvals don't all
    // go at once: a loop each, as a pool needs.
    const send = async () => {
      while (next < cards.length) {
        const s = cards[next++]!;
        try {
          const { decision } = await settleReview(ws, o, s.reviewId, {
            decision: {
              release: { statementHash: await statementHash(s.link, s.stored) },
            },
          });
          // Its card reads saved, and the count drops, as each one lands.
          qc.setQueryData<FoundryReviewDecision[]>(
            reviewQueries.list(ws, o).queryKey,
            (saved) =>
              saved &&
              decision && [
                ...saved.filter((d) => d.id !== decision.id),
                decision,
              ],
          );
        } catch (e) {
          failed.push((e as Error).message);
        }
        done += 1;
        setProgress({ done, total: cards.length });
      }
    };
    try {
      await Promise.all(
        Array.from({ length: Math.min(APPROVE_AT_ONCE, cards.length) }, send),
      );
      await qc.invalidateQueries({
        queryKey: reviewQueries.list(ws, o).queryKey,
      });
    } finally {
      setProgress(undefined);
      setResult(
        `${cards.length - failed.length} approved` +
          (failed.length
            ? `, ${failed.length} couldn’t be: ${[...new Set(failed)].join("; ")}`
            : ""),
      );
    }
  };
  return (
    <>
      {ready.length > 1 && (
        <div className="dc-review-actions">
          <Button
            size="sm"
            disabled={!!progress}
            pending={!!progress}
            onClick={() => setConfirming(true)}
          >
            Approve all ({ready.length})
          </Button>
        </div>
      )}
      {progress ? (
        <p role="status">
          Approving {progress.done} of {progress.total}…
        </p>
      ) : (
        result && <p role="status">{result}</p>
      )}
      {confirming && ready.length > 0 && (
        <ConfirmDialog
          title={`Approve ${plural(ready.length, "claim")}?`}
          description="This load writes each one it can, at once. Any it can't stays held, and its card says why. On a link that holds one value, an approval replaces what the link holds."
          confirmLabel="Approve all"
          onConfirm={() => void run(ready)}
          onCancel={() => setConfirming(false)}
        />
      )}
    </>
  );
}

/**
 * Opens Generate with a goal asking for the links these reviews suggest
 * (linksGoal), for managers, on the load a person can settle.
 */
function AddLinks({
  reviews,
  load,
}: {
  reviews: readonly Review[];
  load?: FoundryLoad;
}) {
  const { role } = useWorkspace();
  const { id: o } = useOntology();
  const goal = linksGoal(reviews);
  if (!canManage(role) || !load?.current || !goal) return null;
  return (
    <div className="dc-review-actions">
      <ButtonLink size="sm" icon={Sparkles} to={paths.generate(o, { goal })}>
        Add these links
      </ButtonLink>
      <small>
        Generating costs a model run and drafts a new version for you to review.
        Your current version stays until you choose otherwise.
      </small>
    </div>
  );
}
