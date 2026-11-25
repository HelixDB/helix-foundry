import { RotateCcw, Search } from "lucide-react";
import { sentence } from "../../api";
import { Button, Panel } from "../../kit";
import { AiRefusal, useAiConfigured } from "../workspace/ai-gate";
import type { CompanyChoice, ResearchRun } from "./model";

/**
 * The new-ontology wizard's Your company step: a domain (`domain`, which the
 * page starts from the user's email when it names a company), Research,
 * which has the WFE read its site, then what it found: the company, the
 * pages read (or that none could be, so the guesses come from the domain
 * alone), and its questions, each answer starting as the best guess, its
 * options as chips that fill it. The page runs the research (`research`,
 * companyFlow), so it carries on while this step is left; editing the
 * domain drops the research and abandons one running. A failure says why,
 * with Retry; without an AI provider, or off Pro on the workspace's own
 * key, the gate shows instead.
 */
export function CompanyPanel({
  domain,
  company,
  research,
  onDomain,
  onResearch,
  onAnswer,
}: {
  domain: string;
  company: CompanyChoice | null;
  research: ResearchRun;
  onDomain: (domain: string) => void;
  onResearch: () => void;
  onAnswer: (questionId: string, answer: string) => void;
}) {
  const aiReady = useAiConfigured();
  const pending = research.kind === "pending",
    error = research.kind === "failed" ? research.error : null;
  const r = company?.research,
    questions = r?.questions?.length || 0;
  return (
    <Panel
      title="Company website"
      subtitle="Foundry reads its public site: the homepage and a few pages about the company."
    >
      <form
        className="on-company-form"
        onSubmit={(e) => {
          e.preventDefault();
          onResearch();
        }}
      >
        <label className="hf-field">
          <span className="hf-sr-only">Company website</span>
          <input
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
          icon={Search}
          pending={pending}
          disabled={!domain.trim() || aiReady === false}
        >
          Research
        </Button>
      </form>
      {/* Mounted throughout, so what it says next is announced. */}
      <p className="on-muted on-company-status" role="status">
        {pending
          ? `Reading ${research.domain}…`
          : r
            ? `Foundry read ${r.companyName || r.domain || "your company"}: ${
                questions === 1
                  ? "1 question"
                  : `${questions.toLocaleString("en-US")} questions`
              } to answer below.`
            : ""}
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
      {r && (
        <div className="on-company">
          <div className="on-card-text">
            <strong>{r.companyName || r.domain}</strong>
            {r.summary && <p>{r.summary}</p>}
          </div>
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
          {!!questions && (
            <ol className="on-questions" aria-label="Questions">
              {r.questions!.map((q) => {
                const value = company.answers.get(q.id) ?? "";
                return (
                  <li key={q.id} className="on-question">
                    <label className="hf-field">
                      {q.question}
                      <textarea
                        rows={2}
                        maxLength={500}
                        value={value}
                        onChange={(e) => onAnswer(q.id, e.target.value)}
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
                  </li>
                );
              })}
            </ol>
          )}
        </div>
      )}
    </Panel>
  );
}
