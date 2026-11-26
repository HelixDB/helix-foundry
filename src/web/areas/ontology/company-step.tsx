import {
  ArrowLeft,
  ArrowRight,
  Check,
  LoaderCircle,
  Pencil,
  RotateCcw,
  Search,
  Undo2,
} from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { sentence } from "../../api";
import { Button } from "../../kit";
import { AiRefusal, useAiConfigured } from "../workspace/ai-gate";
import type { CompanyChoice, ResearchRun } from "./model";

/**
 * What research does, as it goes: the backend reads the site (its homepage,
 * then the pages it links about the company, 6 s at most: companyresearch
 * Timeout), then one model call writes what Foundry found and its
 * questions. The page hears nothing until it ends, so each step shows from
 * when it can have started: `at` is that time in ms.
 */
const RESEARCH_STEPS = [
  { at: 0, text: (domain: string) => `Reading ${domain}` },
  { at: 1500, text: () => "Following its pages about the company" },
  { at: 6000, text: () => "Working out what it does" },
  { at: 14000, text: () => "Writing the questions only you can answer" },
];

/**
 * The research's steps so far, a chat assistant's way: each one done ticked,
 * the latest shimmering. Shown while `domain` is being read.
 */
function ResearchSteps({ domain }: { domain: string }) {
  const [shown, setShown] = useState(1);
  useEffect(() => {
    const timers = RESEARCH_STEPS.slice(1).map((step, i) =>
      setTimeout(() => setShown(i + 2), step.at),
    );
    return () => timers.forEach(clearTimeout);
  }, []);
  const steps = RESEARCH_STEPS.slice(0, shown);
  return (
    <>
      {/* Announced one step at a time; the list is for the eye. */}
      <p className="hf-sr-only" role="status">
        {steps.at(-1)!.text(domain)}…
      </p>
      <ol className="on-research" aria-hidden="true">
        {steps.map((step, i) => {
          const done = i < steps.length - 1;
          return (
            <li key={step.at} className={done ? "is-done" : "is-current"}>
              {done ? (
                <Check size={14} />
              ) : (
                <LoaderCircle size={14} className="hf-spin" />
              )}
              <span>
                {step.text(domain)}
                {done ? "" : "…"}
              </span>
            </li>
          );
        })}
      </ol>
    </>
  );
}

/**
 * The new-ontology wizard's Your company step, one thing at a time in the
 * middle of the page: first the company's website (`domain`, which the page
 * starts from the user's email when it names a company) with Research,
 * which has the WFE read its site, saying what it does as it goes
 * (ResearchSteps); then what Foundry found (the company and its summary, or
 * that the site couldn't be read), then each of the research's questions,
 * each on its own screen. Every one of those shows Foundry's guess (the
 * summary, a question's best guess) and asks whether it looks right: This
 * looks right to me goes on, Change it opens the field (and a question's
 * options as chips that fill it), and once the answer differs from Foundry's,
 * Undo puts Foundry's back. A screen with nothing to confirm (a
 * question without a guess, or one cleared) opens on its field.
 * The last screen's go-on calls `onFinish`, which drafts the goal and moves
 * on.
 *
 * The page runs the research (`research`, companyFlow), so it carries on
 * while this step is left; editing the domain drops the research and
 * abandons one running. A failure says why, with Retry; without an AI
 * provider, or off Pro on the workspace's own key, the gate shows instead.
 * `onBack`, when given, leaves the website's screen (onboarding's Get
 * started), else `onCancel` leaves the wizard; `onSkip` goes on without a
 * company.
 */
export function CompanyPanel({
  domain,
  company,
  research,
  onDomain,
  onResearch,
  onSummary,
  onAnswer,
  onFinish,
  onSkip,
  onBack,
  onCancel,
}: {
  domain: string;
  company: CompanyChoice | null;
  research: ResearchRun;
  onDomain: (domain: string) => void;
  onResearch: () => void;
  onSummary: (summary: string) => void;
  onAnswer: (questionId: string, answer: string) => void;
  onFinish: () => void;
  onSkip: () => void;
  onBack?: () => void;
  onCancel?: () => void;
}) {
  const aiReady = useAiConfigured();
  const pending = research.kind === "pending",
    error = research.kind === "failed" ? research.error : null;
  const r = company?.research,
    questions = r?.questions || [];
  // -1 is the website's screen, 0 the company's, else question at - 1. A
  // research that lands opens the company; coming back to a researched
  // company opens it there too.
  const [at, setAt] = useState(r ? 0 : -1);
  // What a screen holds: what the company does, or a question's answer.
  const valueAt = (screen: number) => {
    const q = screen > 0 ? questions[screen - 1] : undefined;
    return !company
      ? ""
      : q
        ? (company.answers.get(q.id) ?? "")
        : company.summary;
  };
  // Whether the shown screen's field is open. Each screen opens on its
  // guess, asking whether it looks right, or on its field when it has none;
  // decided as it opens, so typing into an empty field keeps it open.
  const [editing, setEditing] = useState(() => !valueAt(0).trim());
  const show = (screen: number) => {
    setAt(screen);
    setEditing(!valueAt(screen).trim());
  };
  const landed = useRef(r?.id);
  useEffect(() => {
    if (r && landed.current !== r.id) show(0);
    if (!r) show(-1);
    landed.current = r?.id;
  }, [r?.id]);

  const q = at > 0 ? questions[at - 1] : undefined;
  const value = valueAt(at);
  // Each screen puts the cursor on what it asks for: the field, else This
  // looks right to me.
  const card = useRef<HTMLFormElement>(null);
  useEffect(() => {
    card.current?.querySelector<HTMLElement>("[data-focus]")?.focus();
  }, [at, editing]);

  if (at < 0 || !r || !company) {
    return (
      <section className="on-focus" aria-labelledby="on-company-ask">
        <form
          ref={card}
          className="on-focus-card"
          onSubmit={(e) => {
            e.preventDefault();
            onResearch();
          }}
        >
          <h2 id="on-company-ask" className="on-focus-title">
            What’s your company’s website?
          </h2>
          <p className="on-muted">
            Foundry reads its homepage and a few pages about the company, then
            asks what the site can’t tell.
          </p>
          <div className="on-company-form">
            <label className="hf-field">
              <span className="hf-sr-only">Company website</span>
              <input
                data-focus
                value={domain}
                placeholder="e.g. acme.com"
                inputMode="url"
                autoComplete="url"
                spellCheck={false}
                maxLength={253}
                onChange={(e) => onDomain(e.target.value)}
              />
            </label>
            <Button
              type="submit"
              variant="primary"
              icon={Search}
              pending={pending}
              disabled={!domain.trim() || aiReady === false}
            >
              Research
            </Button>
          </div>
          {pending && (
            <ResearchSteps key={research.domain} domain={research.domain} />
          )}
          <AiRefusal
            action="Researching your company"
            error={error}
            ready={aiReady}
          >
            {!!error && (
              <p className="hf-form-error on-company-error" role="alert">
                {sentence((error as Error).message || "it couldn’t be read")}{" "}
                <Button size="sm" icon={RotateCcw} onClick={onResearch}>
                  Retry
                </Button>
              </p>
            )}
          </AiRefusal>
          <div className="on-focus-actions">
            {onBack ? (
              <Button icon={ArrowLeft} onClick={onBack}>
                Back
              </Button>
            ) : (
              onCancel && <Button onClick={onCancel}>Cancel</Button>
            )}
            {r && !pending && (
              <Button iconRight={ArrowRight} onClick={() => show(0)}>
                Continue
              </Button>
            )}
            <Button onClick={onSkip}>Skip</Button>
          </div>
        </form>
      </section>
    );
  }

  const total = questions.length + 1;
  const last = at >= questions.length;
  const next = () => (last ? onFinish() : show(at + 1));
  const name = r.companyName || r.domain || "your company";
  const count = `${at + 1} of ${total}`;
  // Foundry's own answer, which Undo puts back once it differs.
  const original = q ? q.bestGuess || "" : r.summary || "";
  const changed = !!original.trim() && value.trim() !== original.trim();
  const undo = () => {
    if (q) onAnswer(q.id, original);
    else onSummary(original);
    setEditing(false);
  };
  return (
    <section className="on-focus" aria-labelledby="on-company-head">
      {/* A screen of its own, so moving on focuses afresh and is read. */}
      <form
        key={at}
        ref={card}
        className="on-focus-card"
        onSubmit={(e) => {
          e.preventDefault();
          next();
        }}
      >
        <p className="on-focus-count">{count}</p>
        {q ? (
          <h2 id="on-company-head" className="on-focus-title">
            {q.question}
          </h2>
        ) : (
          <>
            <h2 id="on-company-head" className="on-focus-title">
              {r.companyName || r.domain || "Your company"}
            </h2>
            {!r.fetched && (
              <p className="on-muted">
                Foundry couldn’t read the site, so these guesses come from the
                domain alone.
              </p>
            )}
          </>
        )}
        {editing ? (
          <>
            <label className="hf-field on-focus-question">
              <span className="hf-sr-only">
                {q ? q.question : `What ${name} does`}
              </span>
              <textarea
                data-focus
                rows={q ? 3 : 5}
                maxLength={q ? 500 : 600}
                value={value}
                placeholder={q ? undefined : `What does ${name} do?`}
                onChange={(e) =>
                  q ? onAnswer(q.id, e.target.value) : onSummary(e.target.value)
                }
                onKeyDown={(e) => {
                  // Enter goes on; Shift+Enter is a new line.
                  if (e.key === "Enter" && !e.shiftKey) {
                    e.preventDefault();
                    next();
                  }
                }}
              />
            </label>
            {!!q?.options?.length && (
              <div
                className="on-chips"
                role="group"
                aria-label={`Answers to “${q.question}”`}
              >
                {q.options.map((o) => (
                  <button
                    key={o}
                    type="button"
                    className="on-chip"
                    aria-pressed={value === o}
                    onClick={() => onAnswer(q.id, o)}
                  >
                    {o}
                  </button>
                ))}
              </div>
            )}
          </>
        ) : (
          <>
            <p id="on-guess" className="on-guess">
              {value}
            </p>
            <p id="on-guess-ask" className="on-guess-ask">
              Does this look right to you?
            </p>
          </>
        )}
        {last && (
          <p className="on-muted">Foundry writes your goal from these next.</p>
        )}
        <div className="on-focus-actions">
          <Button icon={ArrowLeft} onClick={() => show(at - 1)}>
            Back
          </Button>
          {!editing && (
            <Button icon={Pencil} onClick={() => setEditing(true)}>
              Change it
            </Button>
          )}
          {changed && (
            <Button icon={Undo2} title="Back to Foundry’s answer" onClick={undo}>
              Undo
            </Button>
          )}
          <Button
            type="submit"
            variant="primary"
            iconRight={editing ? ArrowRight : undefined}
            icon={editing ? undefined : Check}
            data-focus={editing ? undefined : true}
            // Focused on arrival: says what it confirms.
            aria-describedby={
              editing ? undefined : "on-company-head on-guess on-guess-ask"
            }
          >
            {!editing
              ? "This looks right to me"
              : last
                ? "Write the goal"
                : "Next"}
          </Button>
        </div>
      </form>
    </section>
  );
}
