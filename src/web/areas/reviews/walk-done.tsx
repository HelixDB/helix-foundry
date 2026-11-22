// The walkthrough's finish (./walkthrough.tsx): what the walk's choices do,
// split by when (the current load now, or the next load, which Load again
// starts), the cards skipped or still to decide, then the spot checks
// (./spot-checks.tsx), which may be skipped.
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
  empty,
  headingRef,
  onBack,
  onReopen,
  onJump,
  spot,
}: {
  walk: Walk;
  // What each card decided in this walk does.
  made: ReadonlyMap<string, { card: Decidable; effect: Effect }>;
  // The walk took no card: nothing to decide under its filters.
  empty: boolean;
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
    (k) => made.get(k)?.effect.kind === "next_load",
  ).length;
  const skipped = of(["skipped"]);
  const left = walk.order.filter((k) => {
    const x = walk.outcomes.get(k);
    return !x || x.kind === "refused";
  });
  const covered = of(["covered"]).length;
  return (
    <section className="rv-done" aria-labelledby="rv-done-title">
      <h2 id="rv-done-title" ref={headingRef} tabIndex={-1}>
        {left.length
          ? `${plural(left.length, "card")} still to decide`
          : empty
            ? "Nothing to decide"
            : skipped.length
              ? `Through to the end, ${formatNumber(skipped.length)} skipped`
              : "All caught up"}
      </h2>
      {empty && (
        <p className="on-muted">
          No card of the current load’s queue that these filters take is left to
          decide.
        </p>
      )}
      {decided.length > 0 && (
        <dl className="rv-done-groups">
          <div>
            <dt>Applied now</dt>
            <dd>
              {plural(decided.length - later, "choice")}
              {covered > 0 && `, and ${plural(covered, "name")} they settled`}:
              the current load takes them as their jobs run.
            </dd>
          </div>
          <div>
            <dt>From the next load</dt>
            <dd>
              {later
                ? `${plural(later, "choice")}: names this load links already, and claims a name holds. Load again to apply them.`
                : "None."}
            </dd>
          </div>
        </dl>
      )}
      <div className="hf-form-actions rv-done-actions">
        {walk.order.length > 0 && (
          <Button variant="ghost" onClick={onBack}>
            <ArrowLeft size={14} aria-hidden /> Back
          </Button>
        )}
        {left[0] !== undefined && (
          <Button onClick={() => onJump(left[0]!)}>
            Go to the first still to decide
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
      </div>
      <section className="rv-spot" aria-labelledby="rv-spot-title">
        <div className="rv-spot-head">
          <div>
            <h3 id="rv-spot-title">Spot checks</h3>
            <p className="on-muted">
              A few claims drawn at random, to measure how often claims at each
              score are right. That measure is what lets Founder suggest
              approving a score’s claims, all at once once it’s high enough.
            </p>
          </div>
          <Button
            size="sm"
            variant="ghost"
            onClick={() => setSpotShown(!spotShown)}
          >
            {spotShown ? "Skip" : "Show"}
          </Button>
        </div>
        {spotShown && spot}
      </section>
    </section>
  );
}
