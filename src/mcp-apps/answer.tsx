import { Fragment, type ReactNode } from "react";
import {
  Ban,
  CircleAlert,
  ExternalLink,
  FileQuestion,
  Gauge,
  LoaderCircle,
  type LucideIcon,
} from "lucide-react";
import { Button, Tag, cx, formatRelative } from "../web/kit";
import {
  CANNOT_ANSWER_LABELS,
  answerView,
  type AnswerPart,
} from "../web/areas/ontology/ask-model";
import { drawable, placeVisuals } from "../web/areas/ontology/ask-visuals";
import { AskVisual } from "../web/areas/ontology/ask-visual";
import {
  citationLabel,
  citationLink,
  draftLink,
  type AnswerCard,
  type QuestionCard,
} from "./answer-model";

// The answer view: a Foundry question in a host's conversation, drawn as the
// Analyst draws it (ask.tsx's Answer) with its own components. Links open
// through the host (`open`), which may ask the user first: the view's frame
// can't navigate. Nothing a result holds renders as markup.

type Open = (url: string) => void;

/** A note in place of an answer, as the Analyst's callouts. */
function Note({
  icon: Icon,
  title,
  tone,
  live,
  children,
}: {
  icon: LucideIcon;
  title: ReactNode;
  tone?: "danger" | "muted";
  live?: boolean;
  children?: ReactNode;
}) {
  return (
    <div
      className={cx("on-card", tone && `is-${tone}`)}
      role={tone === "danger" ? "alert" : "status"}
    >
      <span className="on-card-icon" aria-hidden>
        <Icon size={16} className={live ? "hf-spin" : undefined} />
      </span>
      <div className="on-card-text">
        <strong>{title}</strong>
        {children && <p>{children}</p>}
      </div>
    </div>
  );
}

/** An answer: its text with citation chips, each visual after the block it follows. */
function Answered({
  card,
  result,
  open,
}: {
  card: QuestionCard;
  result: Extract<QuestionCard["view"], { kind: "answered" }>["result"];
  open: Open;
}) {
  const { blocks, sources } = answerView(
    result.answer.blocks,
    result.citations,
  );
  const shown = new Map(blocks.map((b) => [b.at, b]));
  const drawn = new Map(drawable(result).map((d) => [d.visual.evidence_id, d]));
  const placed = placeVisuals(
    (result.answer.visuals ?? []).map((visual) => ({ visual })),
    result.answer.blocks.length,
  );
  const byNumber = new Map(sources.map((s) => [s.n, s.citation]));
  const chip = (n: number, key: number) => {
    const target = byNumber.get(n)!.target,
      label = citationLabel(card, target),
      to = citationLink(card, target);
    return (
      <button
        key={key}
        type="button"
        className="ak-chip"
        aria-label={`Source ${n}: ${label}`}
        title={label}
        disabled={!to}
        onClick={() => to && open(to)}
      >
        {n}
      </button>
    );
  };
  const run = (parts: AnswerPart[]): ReactNode[] =>
    parts.map((p, i) =>
      "cite" in p ? (
        chip(p.cite, i)
      ) : (
        <span key={i}>
          {i > 0 && !/^[.,;:!?)’”]/.test(p.text) ? " " : ""}
          {p.text}
        </span>
      ),
    );
  return (
    <section className="ak-answer" aria-label="Answer">
      {placed.map((after, i) => {
        const b = shown.get(i);
        return (
          <Fragment key={i}>
            {b &&
              (b.kind === "para" ? (
                <p>{run(b.parts)}</p>
              ) : (
                <ul>
                  {b.items.map((item, j) => (
                    <li key={j}>{run(item)}</li>
                  ))}
                </ul>
              ))}
            {after.map(({ visual }) => {
              const d = drawn.get(visual.evidence_id);
              return d ? (
                <AskVisual
                  key={visual.evidence_id}
                  output={d.output}
                  query={d.query}
                  hint={visual}
                  // An entity's page needs its version's keys: the card
                  // names it only.
                  href={() => undefined}
                />
              ) : null;
            })}
          </Fragment>
        );
      })}
    </section>
  );
}

/** What the question came to, or how far it is. */
function Body({ card, open }: { card: QuestionCard; open: Open }) {
  const { view } = card;
  switch (view.kind) {
    case "waiting":
    case "queued":
    case "running":
      return (
        <Note icon={LoaderCircle} title="Foundry is answering" live>
          Your agent will check back for the answer
          {card.askUrl ? ", or open it in the Analyst to watch." : "."}
        </Note>
      );
    case "retrying":
      return (
        <Note icon={LoaderCircle} title="An attempt failed. Helix will try again" live>
          {view.error}
        </Note>
      );
    case "cancelled":
      return (
        <Note icon={Ban} tone="muted" title="Cancelled">
          It stopped before it answered.
        </Note>
      );
    case "failed":
      return (
        <Note icon={CircleAlert} tone="danger" title="It couldn’t answer">
          {view.error}
        </Note>
      );
    case "budget_exhausted":
      return (
        <Note icon={Gauge} tone="danger" title="Stopped at this question’s limit">
          It used what a question may before it could answer. Ask something
          narrower.
        </Note>
      );
    case "cannot_answer": {
      const { result } = view,
        draft = draftLink(card);
      return (
        <div className="mx-stack">
          <Note icon={FileQuestion} tone="muted" title="This ontology can’t answer that">
            {result.kind && <Tag>{CANNOT_ANSWER_LABELS[result.kind]}</Tag>}{" "}
            {result.reason || "What it holds doesn’t say."}
            {result.gap && <> It lacks: {result.gap}</>}
          </Note>
          {draft && (
            <div>
              <Button size="sm" onClick={() => open(draft)}>
                Draft a new version with this
              </Button>
            </div>
          )}
        </div>
      );
    }
    case "answered":
      return <Answered card={card} result={view.result} open={open} />;
  }
}

/**
 * The card: the question, then its answer or state, then the way to its
 * thread in the Analyst. Before the tool's result, `asking` is the question
 * from the tool's input.
 */
export function AnswerView({
  card,
  asking,
  open,
}: {
  card?: AnswerCard;
  asking?: string;
  open: Open;
}) {
  const question = card?.kind === "question" ? card.question : asking;
  return (
    <article className="mx-card" aria-label="Foundry answer">
      <header className="mx-head">
        <span className="mx-eyebrow">Foundry Analyst</span>
        {question && <h1 className="mx-question">{question}</h1>}
      </header>
      {!card ? (
        <Note icon={LoaderCircle} title="Asking Foundry" live />
      ) : card.kind === "refused" ? (
        <Note icon={CircleAlert} tone="danger" title="Foundry didn’t take the question">
          {card.message}
        </Note>
      ) : (
        <>
          <Body card={card} open={open} />
          {(card.askUrl || card.finishedAt) && (
            <footer className="mx-foot">
              {card.finishedAt && card.view.kind === "answered" && (
                <span className="mx-meta">
                  Answered {formatRelative(card.finishedAt)}
                </span>
              )}
              {card.askUrl && (
                <Button
                  size="sm"
                  variant="ghost"
                  iconRight={ExternalLink}
                  onClick={() => open(card.askUrl!)}
                >
                  Open in Analyst
                </Button>
              )}
            </footer>
          )}
        </>
      )}
    </article>
  );
}
