// One card of the review walkthrough (./walkthrough.tsx), and the parts every
// card of the Review page shares: its head, its evidence (the quote, its
// document and its section, first, before anything is said of it), a claim's
// line and the verifier's score. A walkthrough card asks one question, says
// what Founder suggests and what a choice does, then offers the choices, each
// a button that also has a key (./keys.ts). Members read it, settling nothing.
import { useId, type ReactNode, type Ref } from "react";
import { Link } from "react-router-dom";
import type { DecisionApplied } from "../../../shared/wfe";
import {
  ArrowLeft,
  ArrowRight,
  CircleHelp,
  CornerDownLeft,
  MessageCircle,
  Undo2,
} from "lucide-react";
import { Button, IconButton, Tag, plural } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { useDocumentNames } from "../documents/documents-api";
import { evidenceShapeText, heldUnnamed, reviewCopy } from "../documents/model";
import type { Span } from "../explore/model";
import type { Naming } from "../explore/naming";
import { useOntology } from "../ontology/scope";
import {
  cardName,
  settleOf,
  statesScore,
  type Advice,
  type Decidable,
  type Effect,
} from "./advice";
import type { Choice, ChoiceLayout } from "./choices";
import type { KeyCommand } from "./keys";
import {
  holdingEnds,
  propValueText,
  resolvedText,
  type Statement,
} from "./model";
import { BAND_TEXT, bandOf, scoreText } from "./queue";
import { ClaimWritten } from "./settle";
import { FounderSuggestion, effectText } from "./suggestion";
import type { Outcome } from "./walk";

export function CardHead({
  title,
  random,
}: {
  title: string;
  random?: boolean;
}) {
  return (
    <div className="rq-head">
      <h3>{title}</h3>
      {random && <Tag tone="accent">Random check</Tag>}
    </div>
  );
}

/**
 * Where a card's claim is, before anything is said of it: its quote, its
 * document and its section, linked to the document's review at the quote.
 */
export function Evidence({
  at,
  quote,
}: {
  at: Span & { documentId: string };
  quote: string;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const name = useDocumentNames(ws, "a document");
  const text = quote.trim();
  return (
    <div className="rq-evidence">
      {text ? <blockquote>“{text}”</blockquote> : null}
      <p>
        <Link to={paths.document(o, at.documentId, "review", at)}>
          {name(at.documentId)}
        </Link>{" "}
        · section {at.section + 1}
      </p>
    </div>
  );
}

/** What a claim states: its ends, as named, and its link. */
export function ClaimLine({
  from,
  link,
  to,
}: {
  from?: string;
  link: string;
  to?: string;
}) {
  return (
    <>
      <strong>{from ?? "…"}</strong> <span aria-label="to">→</span>{" "}
      <code>{link}</code> <span aria-label="to">→</span>{" "}
      <strong>{to ?? "…"}</strong>
    </>
  );
}
export const statementLine = (s: Statement, link: string) => (
  <ClaimLine from={s.subject.name} link={link} to={s.object.name} />
);

/** The verifier's score, after the evidence: what the model said. */
export function Score({ confidence }: { confidence?: number }) {
  return (
    <p className="rq-score on-muted">
      {confidence === undefined || bandOf(confidence) === "none"
        ? "The verifier gave it no score."
        : `Verifier score ${scoreText(confidence)} (${BAND_TEXT[bandOf(confidence)]}).`}
    </p>
  );
}

/**
 * How a held claim or value is stated, and, for one the checker scored
 * high but its part of the document doesn't name the end of (contract
 * 4.18.0), why it is held anyway: nothing for a shape or reason this site
 * doesn't know.
 */
export function HeldHow({
  review,
}: {
  review: {
    reason: string;
    candidates: readonly unknown[];
    confidence?: number;
    evidenceShape?: string;
  };
}) {
  // A sentence is how most claims are stated: saying so adds nothing.
  const shape =
    review.evidenceShape === "prose_sentence"
      ? undefined
      : evidenceShapeText(review.evidenceShape);
  const copy = reviewCopy(
    review.reason,
    review.candidates.length,
    review.confidence,
  );
  return (
    <>
      {heldUnnamed(review.reason, review.confidence) && (
        <p role="note">
          <strong>{copy.title}.</strong> {copy.text}
        </p>
      )}
      {shape && <p className="on-muted">How it’s stated: {shape}</p>}
    </>
  );
}

/**
 * A card in one line, as the rail, a batch's list and the load-changed
 * banner name it: a name and its type, a claim's ends and link, a value's
 * property and entity.
 *
 * @example
 * cardLine(nameCard); // "“Umbrella Holdings” · Customer"
 */
export function cardLine(card: Decidable): string {
  switch (card.kind) {
    case "name":
      return `“${cardName(card)}” · ${settleOf(card).type}`;
    case "claim": {
      const { statement: st, link } = settleOf(card);
      return `${st.subject.name} → ${link} → ${st.object.name}`;
    }
    case "value": {
      const s = settleOf(card);
      return `${s.prop} of ${card.review.candidates[0]?.entity?.name ?? s.type}`;
    }
  }
}

/** What a card is, in a word: its tag, and the rail's second line. */
export const KIND_TEXT: Record<Decidable["kind"], string> = {
  name: "Name",
  claim: "Held claim",
  value: "Value",
};

/**
 * What became of a card in this walk, said on it; for a name's choice, also
 * what the current load made of it once that is known (`applied`): linked
 * in place, or why not, the next load applying it.
 */
function OutcomeLine({
  outcome,
  applied,
  choices,
  coveredBy,
}: {
  outcome?: Outcome;
  applied?: DecisionApplied | null;
  choices: readonly Choice[];
  coveredBy?: string;
}) {
  switch (outcome?.kind) {
    case undefined:
      return null;
    case "sending":
    case "saved": {
      // The choice made: sent as it is, or saved as the backend keeps it.
      const label = choices.find((c) => {
        if (outcome.kind === "sending")
          return JSON.stringify(c.decision) === JSON.stringify(outcome.choice);
        const d = outcome.decision;
        switch (d.kind) {
          case "resolve":
            return (
              !!c.decision.resolve &&
              c.decision.resolve.entityKey === d.entityKey &&
              !!c.decision.resolve.notInData === !d.entityKey
            );
          case "prop":
            return JSON.stringify(c.decision.prop) === JSON.stringify(d.value);
          case "reject":
            return c.kind === "reject";
          default:
            return c.kind === "approve";
        }
      })?.label;
      const inLoad =
        outcome.kind === "saved" &&
        outcome.decision.kind === "resolve" &&
        applied?.status !== "pending"
          ? resolvedText(applied)
          : undefined;
      return (
        <p role="status" className="rv-outcome">
          {outcome.kind === "sending" ? "Saving" : "Saved"}
          {label ? `: ${label}` : ""}.{inLoad ? ` ${inLoad}` : ""} Undo with Z
          while it’s your last choice.
        </p>
      );
    }
    case "refused":
      return (
        <p role="alert" className="hf-form-error">
          Couldn’t save it: {outcome.message}
          {outcome.retry ? " Choose again." : ""}
        </p>
      );
    case "skipped":
      return (
        <p role="status" className="rv-outcome">
          Skipped. Choose now, or come back to it at the end.
        </p>
      );
    case "covered":
      return (
        <p role="status" className="rv-outcome">
          Settled by your link{coveredBy ? ` ${coveredBy}` : ""}: the load links
          this name in every document asking about it.
        </p>
      );
    case "elsewhere":
      return (
        <p role="status" className="rv-outcome">
          Decided by someone else meanwhile.
        </p>
      );
    case "taken_back":
      return (
        <p role="status" className="rv-outcome">
          Link taken back. This load keeps it; the next load asks about this
          name again.
        </p>
      );
  }
}

/** A key's hint beside its button, hidden from screen readers (aria-keyshortcuts says it). */
const Kbd = ({ children }: { children: ReactNode }) => (
  <kbd aria-hidden>{children}</kbd>
);

/**
 * One card of the walkthrough: the question, the evidence, what the card
 * concerns, Founder's suggestion and what a choice does (`effect`), what
 * became of it (`outcome`), then its choices as `layout` lays them out: the
 * suggested one first, as the primary button (Enter), then the others, each
 * with its key (`onChoose`). Under them Back, Skip and Undo, then Accept
 * all like this (`batch`, the cards it takes), Ask Founder (`founder`) and
 * the shortcuts, each a command the walkthrough also takes from a key
 * (`onCommand`). `readOnly` (a member, or a load that isn't current)
 * shows the card without its choices, its reading keys (Back, Next, Ask
 * Founder, the shortcuts) still working; a card decided in this walk shows
 * what became of it instead.
 */
export function ReviewCard({
  card,
  advice,
  choices,
  layout,
  effect,
  outcome,
  applied,
  coveredBy,
  readOnly,
  batch,
  founder,
  naming,
  headingRef,
  onCommand,
  onChoose,
}: {
  card: Decidable;
  advice: Advice;
  choices: readonly Choice[];
  layout: ChoiceLayout;
  effect: Effect | null;
  outcome?: Outcome;
  // What the current load made of the saved decision, as last read.
  applied?: DecisionApplied | null;
  coveredBy?: string;
  readOnly: boolean;
  batch: number;
  founder: boolean;
  naming: Naming;
  headingRef: Ref<HTMLHeadingElement>;
  onCommand: (c: KeyCommand) => void;
  onChoose: (c: Choice) => void;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const docName = useDocumentNames(ws, "A document");
  const open =
    !outcome || outcome.kind === "refused" || outcome.kind === "skipped";
  // A card's key may hold spaces and quotes (a name's is JSON), no IDREF.
  const headingId = useId();
  return (
    <article className="rv-card" aria-labelledby={headingId}>
      <header className="rv-card-head">
        <Tag>{KIND_TEXT[card.kind]}</Tag>
        <h2 id={headingId} ref={headingRef} tabIndex={-1}>
          {card.kind === "name"
            ? `${docName(card.review.documentId)} names “${cardName(card)}”. Which ${settleOf(card).type} is it?`
            : card.kind === "claim"
              ? `Is this right: ${cardLine(card)}?`
              : `Which ${settleOf(card).prop} of ${card.review.candidates[0]?.entity?.name ?? `this ${settleOf(card).type}`} is right?`}
        </h2>
      </header>
      <Evidence at={card.review} quote={card.review.quote} />
      {card.kind === "name" && card.held.length > 0 && (
        <p className="rv-card-body">
          {`${plural(card.held.length, "held claim")} ${card.held.length > 1 ? "wait" : "waits"} on it, next.`}
        </p>
      )}
      {card.kind === "value" && (
        <div className="rv-card-body">
          {settleOf(card).table !== undefined && (
            <p>The table holds another value; documents state these:</p>
          )}
          <ul className="ex-evidence-list" aria-label="Conflicting values">
            {settleOf(card).values.map((v, i) => (
              <li key={i}>
                <strong>{propValueText(v.value)}</strong>
                {" · "}
                <Link to={paths.document(o, v.documentId, "review", v)}>
                  “{v.quote}”
                </Link>
              </li>
            ))}
          </ul>
          <HeldHow review={card.review} />
        </div>
      )}
      <FounderSuggestion
        advice={advice}
        effect={
          [
            effect && effectText(effect),
            card.kind === "name" &&
              card.blocked > 0 &&
              `Settling it writes ${plural(card.blocked, "fact")} the load holds back for it.`,
          ]
            .filter(Boolean)
            .join(" ") || null
        }
        label={layout.primary?.label}
      />
      <OutcomeLine
        outcome={outcome}
        applied={applied}
        choices={choices}
        coveredBy={coveredBy}
      />
      {!readOnly && open && (
        <ul className="rv-choices" aria-label="Choices">
          {layout.primary && (
            <li>
              <Button
                variant="primary"
                aria-keyshortcuts="Enter"
                onClick={() => onCommand({ kind: "accept" })}
              >
                {layout.primary.kind === "link"
                  ? `Link to ${layout.primary.label}`
                  : layout.primary.kind === "value"
                    ? `Use ${layout.primary.label}`
                    : layout.primary.label}{" "}
                <Kbd>
                  <CornerDownLeft size={12} />
                </Kbd>
              </Button>
            </li>
          )}
          {layout.rest.map(({ choice, key }) => (
            <li key={choice.id}>
              <Button aria-keyshortcuts={key} onClick={() => onChoose(choice)}>
                {key && <Kbd>{key}</Kbd>} {choice.label}
              </Button>
            </li>
          ))}
        </ul>
      )}
      {/* What a held claim's approval writes, its score and how it's
          stated, after its choices: the suggestion says why already, and a
          phone shows the choice without scrolling. */}
      {card.kind === "claim" && (
        <div className="rv-card-body rv-card-detail">
          <ClaimWritten
            settle={settleOf(card)}
            single={
              !naming.version || naming.oneValued.has(settleOf(card).link)
            }
            holding={holdingEnds(
              settleOf(card).unsettled,
              !naming.version || naming.oneValued.has(settleOf(card).link),
            )}
            link={(path) => path}
          />
          {!statesScore(advice) && (
            <Score confidence={card.review.confidence} />
          )}
          <HeldHow review={card.review} />
        </div>
      )}
      <div className="rv-nav">
        <div className="rv-nav-side">
          <Button
            size="sm"
            variant="ghost"
            aria-keyshortcuts="ArrowLeft"
            onClick={() => onCommand({ kind: "back" })}
          >
            <ArrowLeft size={14} aria-hidden /> Back
          </Button>
          <Button
            size="sm"
            variant="ghost"
            aria-keyshortcuts="S"
            onClick={() => onCommand({ kind: "skip" })}
          >
            {open && !readOnly ? "Skip" : "Next"}{" "}
            <ArrowRight size={14} aria-hidden />
          </Button>
          {!readOnly && (
            <Button
              size="sm"
              variant="ghost"
              aria-keyshortcuts="Z"
              onClick={() => onCommand({ kind: "undo" })}
            >
              <Undo2 size={14} aria-hidden /> Undo
            </Button>
          )}
        </div>
        <div className="rv-nav-side">
          {!readOnly && batch > 1 && open && (
            <Button
              size="sm"
              aria-keyshortcuts="G"
              onClick={() => onCommand({ kind: "batch" })}
            >
              Accept all like this ({batch}) <Kbd>G</Kbd>
            </Button>
          )}
          {founder && (
            <Button
              size="sm"
              variant="ghost"
              aria-keyshortcuts="F"
              onClick={() => onCommand({ kind: "founder" })}
            >
              <MessageCircle size={14} aria-hidden /> Ask Founder
            </Button>
          )}
          <IconButton
            size="sm"
            label="Keyboard shortcuts"
            icon={CircleHelp}
            aria-keyshortcuts="Shift+?"
            onClick={() => onCommand({ kind: "help" })}
          />
        </div>
      </div>
    </article>
  );
}
