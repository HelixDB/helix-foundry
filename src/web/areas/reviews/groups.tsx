import { useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Sparkles } from "lucide-react";
import type { FoundryLoad, FoundryReviewDecision } from "../../../shared/wfe";
import { Button, ButtonLink, ConfirmDialog, plural } from "../../kit";
import { paths } from "../../paths";
import { canManage, useWorkspace } from "../../ui";
import { useOntology } from "../ontology/scope";
import type { Naming } from "../explore/naming";
import {
  deleteReviewDecision,
  refusalReason,
  reviewQueries,
  settleReviews,
} from "./api";
import { createOutbox } from "./outbox";
import {
  approvable,
  approveAllText,
  leftToCards,
  linksGoal,
  reviewGroups,
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

/** The most approvals one request of Approve all carries. */
const PER_REQUEST = 25;

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
          <ApproveAll reviews={members} load={load} naming={naming} />
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
 * (approvable), once they confirm, each bound to its own card's statement,
 * through one outbox (PER_REQUEST a request, one in flight) on the load the
 * page shows, expecting each card open: a claim a teammate decided first
 * keeps their decision, and the result says how many. Offered for two or
 * more: one card has its own Approve. Each approval saved shows on its card
 * at once; one that fails leaves its card as it is, and the result says
 * why. "Undo these N" removes only the approvals this person saved. The
 * backend applies approvals that land together in one job, so the load
 * writes them in one go. Claims of a link that holds one value are left
 * out, as an approval decides what such a link holds, and the dialog says
 * so.
 */
function ApproveAll({
  reviews,
  load,
  naming,
}: {
  reviews: readonly Settleable[];
  load?: FoundryLoad;
  naming: Naming;
}) {
  const { id: ws, role, user } = useWorkspace();
  const { id: o } = useOntology();
  const qc = useQueryClient();
  const decisions = useQuery(reviewQueries.list(ws, o));
  const [confirming, setConfirming] = useState(false);
  const [progress, setProgress] = useState<{ done: number; total: number }>();
  const [result, setResult] = useState("");
  // The approvals this person saved in the last run, which Undo removes.
  const [mine, setMine] = useState<readonly FoundryReviewDecision[]>([]);
  if (!canManage(role)) return null;
  // Saved decisions decide what is already approved, so none is offered
  // until they're read. A link of a version that isn't known holds one
  // value, as a card takes it.
  const single = (link: string) =>
    !naming.version || naming.oneValued.has(link);
  const saved = (load?.current && decisions.data) || undefined;
  const ready = saved ? approvable(reviews, saved, single) : [];
  // Those it would approve but leaves to their cards: the claims of links
  // that hold one value.
  const own = saved ? leftToCards(reviews, saved, single) : 0;
  const listKey = reviewQueries.list(ws, o).queryKey;
  const run = async (
    cards: readonly Extract<Settle, { kind: "release" }>[],
  ) => {
    if (!load) return;
    setConfirming(false);
    setResult("");
    setMine([]);
    setProgress({ done: 0, total: cards.length });
    const box = createOutbox(
      (items: readonly Extract<Settle, { kind: "release" }>[]) =>
        settleReviews(ws, o, {
          loadId: load.id,
          items: items.map((s) => ({
            reviewId: s.reviewId,
            decision: { release: { statementHash: s.hash } },
          })),
          expectOpen: true,
        }),
      { max: PER_REQUEST },
    );
    let done = 0;
    try {
      const answers = await Promise.allSettled(
        cards.map(async (s) => {
          const r = await box
            .push(s)
            .finally(() =>
              setProgress({ done: (done += 1), total: cards.length }),
            );
          const decision = r.decision;
          // Its card reads saved, and the count drops, as each one lands.
          if (decision)
            qc.setQueryData<FoundryReviewDecision[]>(
              listKey,
              (saved) =>
                saved && [
                  ...saved.filter((d) => d.id !== decision.id),
                  decision,
                ],
            );
          return r;
        }),
      );
      const saved = answers.flatMap((a) =>
        a.status === "fulfilled" && a.value.decision ? [a.value.decision] : [],
      );
      // Those a teammate decided first: their decision stands.
      const others = answers.flatMap((a) =>
        a.status === "fulfilled" &&
        !a.value.decision &&
        refusalReason(a.value.refused) === "DECIDED"
          ? [a.value.refused?.decided?.kind]
          : [],
      );
      const failed = answers.flatMap((a) =>
        a.status === "rejected"
          ? [(a.reason as Error).message]
          : !a.value.decision && refusalReason(a.value.refused) !== "DECIDED"
            ? [a.value.refused?.message || "refused"]
            : [],
      );
      setMine(saved.filter((d) => d.decidedByUserId === user.id));
      setResult(
        [
          `${saved.length} approved`,
          others.length &&
            `${plural(others.length, "was", "were")} already ${
              others.every((k) => k === "release") ? "approved" : "decided"
            } by others`,
          failed.length &&
            `${failed.length} couldn’t be: ${[...new Set(failed)].join("; ")}`,
        ]
          .filter(Boolean)
          .join(", ") + ".",
      );
      await qc.invalidateQueries({ queryKey: listKey });
    } finally {
      setProgress(undefined);
    }
  };
  const undo = async () => {
    const taking = mine;
    setMine([]);
    const out = await Promise.allSettled(
      taking.map((d) => deleteReviewDecision(ws, o, d.id)),
    );
    const failed = out.filter((r) => r.status === "rejected").length;
    setResult(
      failed
        ? `Took back ${taking.length - failed} of ${plural(taking.length, "approval")}; ${failed} couldn’t be.`
        : `Took back ${plural(taking.length, "approval")}.`,
    );
    await qc.invalidateQueries({ queryKey: [ws, "ontology", o] });
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
        result && (
          <div className="dc-review-actions">
            <p role="status">{result}</p>
            {mine.length > 0 && (
              <Button size="sm" variant="ghost" onClick={() => void undo()}>
                Undo these {mine.length}
              </Button>
            )}
          </div>
        )
      )}
      {confirming && ready.length > 0 && (
        <ConfirmDialog
          title={`Approve ${plural(ready.length, "claim")}?`}
          description={approveAllText(own)}
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
