// Founder's suggestion on a walkthrough card (./advice.ts advise), in fixed
// words over its evidence, never model text: the choice, its source (a rule),
// how far it is measured, why, and what the choice does. A card with none
// says why a person decides it.
import { Sparkles } from "lucide-react";
import { Tag, formatDate, formatNumber, plural } from "../../kit";
import type { Advice, Basis, Effect, NoAdvice, Strength } from "./advice";
import { BAND_TEXT, scoreText } from "./queue";

/** Why a card has no suggestion, as it says it. */
export const NO_ADVICE: Record<NoAdvice, string> = {
  fyi: "For information: there’s nothing to decide.",
  tied_candidates: "Needs you: more than one record fits this name.",
  no_candidates: "Needs you: the load found no record for this name.",
  one_valued:
    "Needs you: this link holds one value, so approving decides what it holds.",
  differs: "Needs you: it conflicts with what the graph holds.",
  waits_on_name: "Needs you: a name in it is still to settle.",
  read_again:
    "Needs you: a decision on this claim was saved before it read as it does now.",
  unscored:
    "Needs you: no random checks have measured claims scored like this yet.",
  mostly_wrong:
    "Needs you: random checks found most claims scored like this wrong.",
  values_split: "Needs you: the documents split on the value.",
  few_quotes: "Needs you: too few quotes state a value to go by.",
};

/** How far a suggestion is measured, in a chip. */
export function strengthText(s: Strength): string {
  switch (s.kind) {
    case "measured":
      return `Measured · right ${formatNumber(s.right)} of ${formatNumber(s.total)}`;
    case "repeat":
      return `${s.by === "you" ? "Your" : "The team’s"} earlier choice ×${formatNumber(s.decisions)}`;
    case "unmeasured":
      return "Not measured";
  }
}

/** One line of why, by what the suggestion stands on. */
function whyText(b: Basis, s: Strength): string {
  switch (b.kind) {
    case "precedent":
      return `Chosen the same way for this name in ${plural(b.decisions, "other document")}${b.latest ? `, last on ${formatDate(b.latest, false)}` : ""}.`;
    case "only_candidate":
      return `${b.name} is the only record the load found for it.`;
    case "verifier":
      return `${b.score !== undefined ? `The verifier scored it ${scoreText(b.score)}. ` : ""}Random checks of claims scored ${BAND_TEXT[b.band]} found ${
        s.kind === "measured"
          ? `${formatNumber(s.right)} of ${formatNumber(s.total)}`
          : "most"
      } right.`;
    case "majority":
      return `${b.agree} of ${b.of} quotes state this value.`;
  }
}

/** What a choice does, and when. */
export function effectText(e: Effect): string {
  switch (e.kind) {
    case "writes_now":
      return `Writes ${plural(e.facts, "fact")} to this load now.`;
    case "applies_now":
      return e.documents > 1
        ? `Applies now to ${formatNumber(e.documents)} documents asking about this name.`
        : "Applies to this document, now.";
    case "value_now":
      return "Sends the value to this load now.";
    case "next_load":
      return "Applies from the next load.";
    case "saved_only":
      return "Saved only: nothing is written.";
  }
}

/**
 * Founder's suggestion (`advice`), its choice by the card's word for it
 * (`label`), and what the choice does (`effect`: the suggestion's, else a
 * card's first choice's, worded). With a suggestion, what the choice does is
 * in its box; without one, why a person decides it and what a choice does
 * are plain lines over the choices.
 */
export function FounderSuggestion({
  advice,
  effect,
  label,
}: {
  advice: Advice;
  effect: string | null;
  label?: string;
}) {
  const effectLine = effect && <p className="rv-effect">{effect}</p>;
  if (advice.kind === "suggest" && label)
    return (
      <section className="rv-suggest" aria-label="Founder’s suggestion">
        <p className="rv-suggest-head">
          <Sparkles size={14} aria-hidden />
          <span>
            Founder suggests <strong>{label}</strong>
          </span>
          <Tag>Rule</Tag>
          <Tag
            tone={advice.strength.kind === "unmeasured" ? "neutral" : "success"}
          >
            {strengthText(advice.strength)}
          </Tag>
        </p>
        <ul className="rv-why" aria-label="Why">
          {advice.basis.map((b, i) => (
            <li key={i}>{whyText(b, advice.strength)}</li>
          ))}
        </ul>
        {effectLine}
      </section>
    );
  return (
    <section className="rv-card-body" aria-label="Founder’s suggestion">
      <p className="rv-needs">
        {advice.kind === "none"
          ? NO_ADVICE[advice.why]
          : "Needs you: Founder’s suggestion isn’t among the choices."}
      </p>
      {effectLine}
    </section>
  );
}
