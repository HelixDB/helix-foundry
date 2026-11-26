import {
  ArrowLeft,
  ArrowRight,
  Check,
  Pencil,
  RotateCcw,
  Search,
} from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { sentence } from "../../api";
import { Button } from "../../kit";
import { AiRefusal, useAiConfigured } from "../workspace/ai-gate";
import type { CompanyChoice, ResearchRun } from "./model";

/**
 * The new-ontology wizard's Your company step, one thing at a time in the
 * middle of the page: first the company's website (`domain`, which the page
 * starts from the user's email when it names a company) with Research,
 * which has the WFE read its site; then what Foundry found (the company, its
 * summary and the pages read, or that none could be), then each of the
 * research's questions, each on its own screen. Every one of those shows
 * Foundry's guess (the summary, a question's best guess) and asks whether it
 * looks right: This looks right to me goes on, Change it opens the field
 * (and a question's options as chips that fill it). A screen with nothing to
 * confirm (a question without a guess, or one cleared) opens on its field.
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
  // Whether the shown screen's field is open: each screen opens on its
  // guess, asking whether it looks right.
  const [changing, setChanging] = useState(false);
  const show = (screen: number) => {
    setAt(screen);
    setChanging(false);
  };
  const landed = useRef(r?.id);
  useEffect(() => {
    if (r && landed.current !== r.id) show(0);
    if (!r) show(-1);
    landed.current = r?.id;
  }, [r?.id]);

  const q = at > 0 ? questions[at - 1] : undefined;
  const value = !company
    ? ""
    : q
      ? (company.answers.get(q.id) ?? "")
      : company.summary;
  const editing = changing || !value.trim();
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
              <Button iconRight={ArrowRight} onClick={() => show(0)}>
                Continue
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

  const total = questions.length + 1;
  const last = at >= questions.length;
  const next = () => (last ? onFinish() : show(at + 1));
  const name = r.companyName || r.domain || "your company";
  const count = `${at + 1} of ${total}`;
  return (
    <section className="on-focus" aria-label={count}>
      <form
        ref={card}
        className="on-focus-card"
        onSubmit={(e) => {
          e.preventDefault();
          next();
        }}
      >
        <p className="on-focus-count">{count}</p>
        {q ? (
          <h2 className="on-focus-title">{q.question}</h2>
        ) : (
          <>
            <h2 className="on-focus-title">
              {r.companyName || r.domain || "Your company"}
            </h2>
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
                  q
                    ? onAnswer(q.id, e.target.value)
                    : onSummary(e.target.value)
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
            <p className="on-guess">{value}</p>
            <p className="on-guess-ask">Does this look right to you?</p>
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
            <Button icon={Pencil} onClick={() => setChanging(true)}>
              Change it
            </Button>
          )}
          <Button
            type="submit"
            variant="primary"
            iconRight={editing ? ArrowRight : undefined}
            icon={editing ? undefined : Check}
            data-focus={editing ? undefined : true}
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
