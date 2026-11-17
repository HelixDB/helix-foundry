import { Link } from "react-router-dom";
import { Database } from "lucide-react";
import type {
  FoundryAutomation,
  FoundryAutomationOntology,
  FoundryAutomationRun,
} from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import { Tag, formatRelative } from "../../kit";
import { paths } from "../../paths";
import {
  VERDICTS,
  VERDICT_LABELS,
  verdictsText,
  type Verdict,
} from "../workflows/model";
import { impactText, runImpact } from "./model";

// How much each ontology an automation uses helped it: Foundry records each
// run that uses one in a workflow of the ontology, which this links, and
// each run's assessment gives a verdict (the backend's automation
// workflows).

/** An ontology's verdicts as one bar, in the verdicts' tokens. */
function VerdictBar({ o }: { o: FoundryAutomationOntology }) {
  const parts = VERDICTS.map((verdict) => ({
    verdict,
    runs: Number(o.verdicts?.find((v) => v.verdict === verdict)?.runs || 0),
  })).filter((p) => p.runs > 0);
  if (!parts.length) return <span className="au-vbar is-empty" aria-hidden />;
  return (
    <span className="au-vbar" role="img" aria-label={verdictsText(o.verdicts)}>
      {parts.map((p) => (
        <span
          key={p.verdict}
          style={{
            flexGrow: p.runs,
            background: `var(--hf-verdict-${p.verdict})`,
          }}
        />
      ))}
    </span>
  );
}

/**
 * The ontologies an automation's runs use, each with how much it helped,
 * the gaps its newest assessed run named and its workflow; one Foundry
 * couldn't make a workflow of says why its runs go unrecorded.
 */
export function OntologyImpact({
  automation: a,
}: {
  automation: FoundryAutomation;
}) {
  const ontologies = a.ontologies || [];
  return (
    <section
      className="au-side-section"
      aria-label="How much its ontologies helped"
    >
      <h3>How much its ontologies helped</h3>
      {ontologies.length ? (
        <ul className="au-impact">
          {ontologies.map((o) => (
            <li key={o.ontologyId} className="au-impact-item">
              <div className="au-impact-head">
                <Database size={13} aria-hidden />
                {o.workflowId ? (
                  <Link
                    to={paths.workflow(o.ontologyId, o.workflowId)}
                    onClick={() =>
                      trackEvent("automation_ontology_opened", {
                        from: "automation",
                      })
                    }
                    aria-label={`${o.ontologyName || "Ontology"}: its use case`}
                  >
                    {o.ontologyName || "An ontology"}
                  </Link>
                ) : (
                  <span>{o.ontologyName || "An ontology"}</span>
                )}
                {o.lastRunAt && <small>{formatRelative(o.lastRunAt)}</small>}
              </div>
              {o.workflowId && <VerdictBar o={o} />}
              <p
                className={
                  o.untracked ? "au-impact-note is-warning" : "au-impact-note"
                }
              >
                {impactText(o)}
                {o.workflowId && o.verdicts?.length
                  ? ` ${verdictsText(o.verdicts)}.`
                  : ""}
              </p>
              {!!o.gaps?.length && (
                <div className="au-impact-gaps">
                  <span>What it lacked, as the last assessed run said:</span>
                  <ul
                    aria-label={`What ${o.ontologyName || "the ontology"} lacked`}
                  >
                    {o.gaps.map((g, i) => (
                      <li key={i}>{g}</li>
                    ))}
                  </ul>
                </div>
              )}
            </li>
          ))}
        </ul>
      ) : (
        <p className="au-hint">
          None yet: once a run reads an ontology or acts on one, Foundry records
          how much it helped, here and on the ontology’s Usage page.
        </p>
      )}
    </section>
  );
}

/**
 * The ontologies one run used, each with its verdict, opening the run that
 * records it.
 */
export function RunOntologies({ run }: { run: FoundryAutomationRun }) {
  const ontologies = run.ontologies || [];
  if (!ontologies.length) return null;
  return (
    <section
      className="au-side-section"
      aria-label="How much each ontology helped"
    >
      <h3>How much each ontology helped</h3>
      <ul className="au-impact">
        {ontologies.map((o) => {
          const impact = runImpact(o);
          const verdict =
            impact.kind === "verdict" && impact.verdict in VERDICT_LABELS
              ? VERDICT_LABELS[impact.verdict as Verdict]
              : undefined;
          return (
            <li key={o.workflowRunId} className="au-impact-run">
              <Link
                to={paths.workflowRun(
                  o.ontologyId,
                  o.workflowId,
                  o.workflowRunId,
                )}
                onClick={() =>
                  trackEvent("automation_ontology_opened", { from: "run" })
                }
                aria-label={`${o.ontologyName || "Ontology"}: its run`}
              >
                <Database size={13} aria-hidden />
                <span>{o.ontologyName || "An ontology"}</span>
              </Link>
              {verdict ? (
                <Tag tone={verdict.tone}>{verdict.label}</Tag>
              ) : (
                <span className="au-impact-state">
                  {impact.kind === "verdict" ? impact.verdict : impact.text}
                </span>
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}
