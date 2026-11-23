// The walkthrough's finish (./walkthrough.tsx): what the walk's choices do,
// split by when (the current load now, or the next load, which Load again
// starts), the cards skipped or still to decide (for a member, who decides
// none, those waiting for an owner or admin), then the spot checks
// (./spot-checks.tsx), which may be skipped. While the index lists cards
// the walk hasn't read yet, it says it is reading them instead.
import { useState, type ReactNode, type Ref } from "react";
import { ArrowLeft } from "lucide-react";
import { Button, ButtonLink, formatNumber, plural } from "../../kit";
import { paths } from "../../paths";
import { useOntology } from "../ontology/scope";
import type { Decidable, Effect } from "./advice";
import type { Walk } from "./walk";

export function WalkDone({
  walk,
  made,
  notInPlace,
  member,
  empty,
  indexing,
  more,
  onClearFilters,
  headingRef,
  onBack,
  onReopen,
  onJump,
  spot,
}: {
  walk: Walk;
  // What each card decided in this walk does.
  made: ReadonlyMap<string, { card: Decidable; effect: Effect }>;
  // The cards whose saved choice the current load refused to take in place:
  // the next load applies it, whatever was foreseen when it was sent.
  notInPlace: ReadonlySet<string>;
  // A member, who reads the cards and decides none.
  member: boolean;
  // The walk took no card: nothing to decide under its filters.
  empty: boolean;
  // The index is still sorting the load's reviews into cards: more may
  // come, so the walk is never done.
  indexing: boolean;
  // The index lists more cards than the walk has read yet (the next page,
  // or its reviews): the walk isn't at its end, and no spot check is drawn.
  more: boolean;
  // Clears the walk's filters; unset while none is set.
  onClearFilters?: () => void;
  headingRef: Ref<HTMLHeadingElement>;
  onBack: () => void;
  // Opens the cards again (those skipped), from the first.
  onReopen: (keys: readonly string[]) => void;
  onJump: (key: string) => void;
  spot: ReactNode;
}) {
  const { id: o } = useOntology();
  const [spotShown, setSpotShown] = useState(true);
  const of = (kinds: readonly string[]) =>
    walk.order.filter((k) => kinds.includes(walk.outcomes.get(k)?.kind ?? ""));
  const decided = of(["sending", "saved"]);
  const later = decided.filter(
    (k) => made.get(k)?.effect.kind === "next_load" || notInPlace.has(k),
  ).length;
  const skipped = of(["skipped"]);
  const left = walk.order.filter((k) => {
    const x = walk.outcomes.get(k);
    return !x || x.kind === "refused" || x.kind === "unconfirmed";
  });
  const covered = of(["covered"]).length;
  return (
    <section className="rv-done" aria-labelledby="rv-done-title">
      <div className="rv-done-head">
        <h2 id="rv-done-title" ref={headingRef} tabIndex={-1}>
          {left.length
            ? member
              ? `${plural(left.length, "card")} ${left.length > 1 ? "wait" : "waits"} for an owner or admin`
              : `${plural(left.length, "card")} still to decide`
            : more
              ? "Reading the next cards…"
              : indexing
                ? "More cards may come"
                : empty
                  ? "Nothing to decide"
                  : skipped.length
                    ? `Through to the end, ${formatNumber(skipped.length)} skipped`
                    : "All caught up"}
        </h2>
        {walk.order.length > 0 && (
          <Button size="sm" variant="ghost" onClick={onBack}>
            <ArrowLeft size={14} aria-hidden /> Back
          </Button>
        )}
      </div>
      {indexing ? (
        <p className="on-muted">
          Counting… the load’s reviews are still being sorted into cards. Come
          back in a few minutes for the rest.
        </p>
      ) : (
        empty &&
        !more && (
          <p className="on-muted">
            No card of the current load
            {onClearFilters ? " that these filters take" : ""} is left to
            decide.
          </p>
        )
      )}
      {decided.length > 0 && (
        <dl className="rv-done-groups">
          {decided.length > later && (
            <div>
              <dt>Applied now</dt>
              <dd>
                {`${plural(decided.length - later, "choice")}${covered > 0 ? `, and ${plural(covered, "name")} they settled` : ""}: the current load takes them as their jobs run.`}
              </dd>
            </div>
          )}
          {later > 0 && (
            <div>
              <dt>From the next load</dt>
              <dd>
                {`${plural(later, "choice")} the current load doesn’t take in place: names it links already or was read before it could link, and claims a name holds. Load again to apply them.`}
              </dd>
            </div>
          )}
        </dl>
      )}
      {(left.length > 0 ||
        skipped.length > 0 ||
        later > 0 ||
        (empty && onClearFilters)) && (
        <div className="hf-form-actions rv-done-actions">
          {left[0] !== undefined && (
            <Button onClick={() => onJump(left[0]!)}>
              {member
                ? "Go back to the first"
                : "Go to the first still to decide"}
            </Button>
          )}
          {skipped.length > 0 && (
            <Button onClick={() => onReopen(skipped)}>
              Go through {formatNumber(skipped.length)} skipped
            </Button>
          )}
          {later > 0 && (
            <ButtonLink to={paths.newLoad(o)} variant="primary">
              Load again
            </ButtonLink>
          )}
          {empty && onClearFilters && (
            <Button onClick={onClearFilters}>Clear filters</Button>
          )}
        </div>
      )}
      {!more && (
        <section className="rv-spot" aria-labelledby="rv-spot-title">
          <div className="rv-spot-head">
            <h3 id="rv-spot-title">Spot checks</h3>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setSpotShown(!spotShown)}
            >
              {spotShown ? "Skip" : "Show"}
            </Button>
          </div>
          <p className="on-muted rv-spot-intro">
            A few claims drawn at random, to measure how often claims at each
            score are right. That measure is what lets Founder suggest approving
            a score’s claims, all at once once it’s high enough.
          </p>
          {spotShown && spot}
        </section>
      )}
    </section>
  );
}
