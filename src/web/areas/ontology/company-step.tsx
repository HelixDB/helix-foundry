import { ArrowLeft, ArrowRight, RotateCcw, Search } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { sentence } from "../../api";
import { Button } from "../../kit";
import { AiRefusal, useAiConfigured } from "../workspace/ai-gate";
import type { CompanyChoice, ResearchRun } from "./model";

/**
 * The new-ontology wizard's Your company step, one thing at a time in the
 * middle of the page: first the company's website (`domain`, which the page
 * starts from the user's email when it names a company) with Research,
 * which has the WFE read its site; then each of the research's questions on
 * its own screen, its answer starting as the best guess and its options as
 * chips that fill it, the first one under what Foundry found (the company,
 * and the pages read, or that none could be). The last question's Continue
 * calls `onFinish`, which drafts the goal and moves on.
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
  // -1 is the website's screen, else the question shown. A research that
  // lands opens its first question; coming back to a researched company
  // opens it there too.
  const [at, setAt] = useState(r ? 0 : -1);
  const landed = useRef(r?.id);
  useEffect(() => {
    if (r && landed.current !== r.id) setAt(0);
    if (!r) setAt(-1);
    landed.current = r?.id;
  }, [r?.id]);
  // Each screen puts the cursor in its field.
  const field = useRef<HTMLInputElement & HTMLTextAreaElement>(null);
  useEffect(() => {
    field.current?.focus();
  }, [at]);

  if (at < 0 || !r) {
    return (
      <section className="on-focus" aria-labelledby="on-company-ask">
        <form
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
                ref={field}
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
          {/* Mounted throughout, so what it says next is announced. */}
          <p className="on-muted on-company-status" role="status">
            {pending ? `Reading ${research.domain}…` : ""}
          </p>
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
              <Button iconRight={ArrowRight} onClick={() => setAt(0)}>
                {questions.length ? "Back to the questions" : "Continue"}
              </Button>
            )}
            <Button variant="ghost" onClick={onSkip}>
              Skip
            </Button>
          </div>
        </form>
      </section>
    );
  }

  const q = questions[at];
  const last = at >= questions.length - 1;
  const next = () => (last ? onFinish() : setAt(at + 1));
  const value = q ? (company!.answers.get(q.id) ?? "") : "";
  return (
    <section
      className="on-focus"
      aria-label={
        q ? `Question ${at + 1} of ${questions.length}` : (r.companyName ?? "")
      }
    >
      <form
        className="on-focus-card"
        onSubmit={(e) => {
          e.preventDefault();
          next();
        }}
      >
        {at === 0 && (
          <div className="on-company">
            <p className="on-focus-kicker">
              {r.companyName || r.domain || "Your company"}
            </p>
            {r.summary && <p className="on-company-summary">{r.summary}</p>}
            {r.fetched ? (
              !!r.sources?.length && (
                <p className="on-company-sources">
                  Read{" "}
                  {r.sources.map((url, i) => (
                    <span key={url}>
                      {i > 0 && ", "}
                      <a href={url} target="_blank" rel="noopener noreferrer">
                        {url.replace(/^https?:\/\//, "").replace(/\/$/, "")}
                      </a>
                    </span>
                  ))}
                </p>
              )
            ) : (
              <p className="on-muted">
                Foundry couldn’t read the site, so these guesses come from the
                domain alone.
              </p>
            )}
          </div>
        )}
        {q ? (
          <>
            <p className="on-focus-count">
              Question {at + 1} of {questions.length}
            </p>
            <label className="hf-field on-focus-question">
              <span className="on-focus-title">{q.question}</span>
              <textarea
                ref={field}
                rows={3}
                maxLength={500}
                value={value}
                onChange={(e) => onAnswer(q.id, e.target.value)}
                onKeyDown={(e) => {
                  // Enter goes on; Shift+Enter is a new line.
                  if (e.key === "Enter" && !e.shiftKey) {
                    e.preventDefault();
                    next();
                  }
                }}
              />
            </label>
            {!!q.options?.length && (
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
          <p className="on-muted">
            Foundry has no questions for you: it writes the goal from the site.
          </p>
        )}
        <div className="on-focus-actions">
          <Button icon={ArrowLeft} onClick={() => setAt(at - 1)}>
            Back
          </Button>
          <Button type="submit" variant="primary" iconRight={ArrowRight}>
            {last ? "Write the goal" : "Next"}
          </Button>
        </div>
      </form>
    </section>
  );
}
