import { useEffect, useId, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowRight,
  ChevronDown,
  FileClock,
  RotateCcw,
  Sparkles,
  TriangleAlert,
} from "lucide-react";
import type {
  FoundryEventSourceMode,
  OntologyVersion,
} from "../../../shared/wfe";
import {
  Button,
  ButtonLink,
  Panel,
  SkeletonText,
  cx,
  formatRelative,
  plural,
  useStoredState,
} from "../../kit";
import { sentence } from "../../api";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { connectorsQuery } from "../data/data-api";
import {
  documentQueries,
  documentSetQueries,
} from "../documents/documents-api";
import { andList } from "../documents/model";
import {
  AiRefusal,
  SettingsLinked,
  useAiConfigured,
} from "../workspace/ai-gate";
import { AgentSetup } from "./agent-setup";
import { formatLabel, versionFormat } from "./format";
import { GoalPanel, SourcesPicker, type EventChoices } from "./generate-parts";
import { LiveLoad, NotLoaded } from "./loads";
import {
  DRAFT_STEPS,
  ONTOLOGY_STEPS,
  STEP_COPY,
  draftWizard,
  generationPicks,
  generationTitle,
  loadRefusal,
  readAgentBundle,
  ruleEvaluationRefusal,
  scopeNames,
  stepSummary,
  type DraftStepId,
  type OntologySetup,
} from "./model";
import { forgetNewOntology, readNewOntology } from "./new-ontology";
import {
  Callout,
  FlowStep,
  LiveGeneration,
  MakeCurrent,
  canManage,
  sourceLabel,
} from "./parts";
import { generateOntology, ontologyQueries } from "./queries";
import { ontologyName, useOntology } from "./scope";

// An unfinished ontology's overview carries on the new-ontology wizard's
// steps from where its setup is (ontologySetup): Name and Database done, then Connect your data or upload documents
// and Goal, Generate a draft, Review and make a version current, Load it,
// and Explore or ask. The first step not done is open with what to do, done
// ones are a line of what they chose, and the rest wait.
//
// Until a draft is under way, the sources and Goal are the wizard's steps
// and pickers (./generate-parts.tsx, connecting and uploading in place),
// walked with ?step= as the wizard walks them (draftWizard), and
// Generate starts the generation from what they chose. They start from what
// was chosen before: this tab's wizard choices when it created this
// ontology but its generation didn't start, else the newest generation's
// (its page's Try again: its API sources apart from its connectors, by
// kind, once the connectors are read), and then open on Generate. A live
// generation shows in its step (the generations query polls while it
// runs), a failed one says why with Try again. Members see the same steps
// without what they can't do.
//
// It is one card: its head says how far the setup is and holds Set up with
// your agent (./agent-setup.tsx); the head's title minimises it to that
// line, saying the step next, remembered per ontology in this browser. What
// an open step shows sits flat in it (its callouts drawn without their own
// cards, a failure apart).

/**
 * A version newer than the current one, or the first draft with none
 * current, waiting to be reviewed: Review, and Make current for those who
 * may once the current version is read. Lists leave definitions out, so it
 * reads the version for Make current's checks.
 */
export function ReadyToReview({
  versionId,
  current,
}: {
  versionId: string;
  // The current version, once read.
  current?: OntologyVersion | null;
}) {
  const { id } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const version = useQuery(ontologyQueries.version(id, o, versionId)).data
    ?.version;
  if (!version) return null;
  const currentId = ontology.currentVersionId || "";
  return (
    <Callout
      icon={FileClock}
      tone="muted"
      title={`Version ${version.version} is ready to review`}
      text={`${formatLabel[versionFormat(version)]}, ${sourceLabel(version.source).toLowerCase()} ${formatRelative(version.createdAt || "")}. ${!currentId ? "No version is current yet." : current ? `It isn’t current yet: the ontology still uses version ${current.version}.` : "It isn’t current yet."}`}
      action={
        <span className="on-card-actions">
          <ButtonLink size="sm" to={paths.version(o, version.id)}>
            Review
          </ButtonLink>
          {(current || !currentId) && (
            <MakeCurrent
              version={version}
              currentId={currentId}
              variant="secondary"
              size="sm"
            />
          )}
        </span>
      }
    />
  );
}

export function SetupFlow({
  setup,
  current: v,
  database,
}: {
  setup: Exclude<OntologySetup, { step: "done" }>;
  // The current version, once read; none without one.
  current?: OntologyVersion | null;
  // What the Database step says: the database and its project.
  database: string;
}) {
  const { id, role } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    qc = useQueryClient(),
    [params, setParams] = useSearchParams();
  // Minimised in this browser, per ontology.
  const [minimisedText, setMinimisedText] = useStoredState(
      `foundry.ontology.setup.minimised.v1:${id}:${o}`,
      "",
    ),
    minimised = minimisedText === "1",
    stepsId = useId();
  const connectors = useQuery(connectorsQuery(id)),
    sets = useQuery(documentSetQueries.list(id)),
    documents = useQuery(documentQueries.workspace(id)),
    generations = useQuery(ontologyQueries.generations(id, o));
  // The newest generation that drafts (a revision edits a version), and what
  // it was asked for; until the connectors and sets are read, its API
  // sources count as connectors and their own sets as sets, which the
  // sources step leaves out.
  const latest = generations.data?.generations?.find((g) => !g.revision),
    picks = generationPicks(
      latest,
      connectors.data?.connectors || [],
      sets.data || [],
    ),
    asked = latest && {
      connectorIds: picks.connectorIds,
      eventSources: picks.eventSources.map(
        (p): [string, FoundryEventSourceMode] => [p.connectorId, p.mode],
      ),
      setIds: picks.documentSetIds,
      goal: latest.goal || "",
    };
  const [saved] = useState(() => {
    const s = readNewOntology(id);
    return s?.ontologyId === o ? s : null;
  });
  const before = saved || asked;
  const [chosen, setChosen] = useState<Set<string> | null>(() =>
      before?.connectorIds ? new Set(before.connectorIds) : null,
    ),
    [goal, setGoal] = useState(before?.goal || "");
  // The API sources and sets chosen: the wizard's, else, once the
  // connectors and sets are read, the newest generation's, its API sources'
  // own sets among its sources; null until then.
  const [chosenEvents, setChosenEvents] = useState<EventChoices | null>(() =>
      saved ? new Map(saved.eventSources) : null,
    ),
    [chosenSets, setChosenSets] = useState<Set<string> | null>(() =>
      saved ? new Set(saved.setIds) : null,
    );
  useEffect(() => {
    if (chosenEvents !== null || !connectors.data || !sets.data) return;
    setChosenEvents(new Map(asked?.eventSources));
    setChosenSets(new Set(asked?.setIds));
  }, [connectors.data, sets.data]);
  const [pending, setPending] = useState(false),
    [error, setError] = useState<unknown>(null);
  // Locally, drafting waits for an AI provider: the WFE refuses it until an
  // owner chooses one.
  const aiReady = useAiConfigured();
  // Why the current version can't be loaded, besides its rules (NotLoaded
  // says that).
  const refusal = useMemo(
    () =>
      v && !ruleEvaluationRefusal(v.definition)
        ? loadRefusal(
            versionFormat(v),
            versionFormat(v) === "agent_bundle"
              ? readAgentBundle(v.definition).status
              : undefined,
          )
        : undefined,
    [v],
  );
  const manage = canManage(role),
    // Until a draft is under way, Data, Documents and Goal are walked.
    drafting = setup.step === "draft" && setup.state !== "running",
    failed = setup.step === "draft" && setup.state === "failed";
  // A draft that ends stored a version (or failed): the overview reads the
  // ontology's versions again, as LiveGeneration does where it stays shown.
  const running = setup.step === "draft" && !drafting,
    wasRunning = useRef(running);
  useEffect(() => {
    if (wasRunning.current && !running)
      void qc.invalidateQueries({ queryKey: [id, "ontology", o] });
    wasRunning.current = running;
  }, [running]);
  const wizard = draftWizard(
    {
      connectorIds: chosen,
      eventSources: chosenEvents || new Map(),
      setIds: chosenSets || new Set(),
      goal,
    },
    {
      // Loading until the API sources to start with are known.
      connectors: chosenEvents ? connectors.data?.connectors : undefined,
      sets: sets.data,
      documents: documents.data,
    },
    params.get("step") ?? (before ? "draft" : null),
  );
  const at = DRAFT_STEPS.indexOf(wizard.current),
    reached = setup.step === "draft" ? 0 : ONTOLOGY_STEPS.indexOf(setup.step);
  // Once a draft is under way, its steps say what it was asked for: its
  // connectors' tables, then its API sources, read through mappings (one
  // table each) or by extraction (their own sets, apart from its sets).
  const nameOf = (c: string) => {
    const x = connectors.data?.connectors?.find((k) => k.id === c);
    return x ? [x.name || x.kind] : [];
  };
  const names = (asked?.connectorIds || []).flatMap(nameOf),
    eventNames = (asked?.eventSources || []).flatMap(([c, mode]) =>
      nameOf(c).map(
        (n) =>
          `${n} (${mode === "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION" ? "extraction" : "mappings"})`,
      ),
    ),
    tables =
      (latest?.tables?.length || 0) -
      (asked?.eventSources || []).filter(
        ([, mode]) => mode === "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED",
      ).length;
  const files = latest?.documents?.length || 0;
  const said: Record<DraftStepId, string> = latest
    ? {
        sources: [
          ...(tables > 0
            ? [
                `${names.length ? `${andList(names)}: ` : ""}${plural(tables, "table")}`,
              ]
            : []),
          ...(eventNames.length ? [andList(eventNames)] : []),
          ...(asked?.setIds.length
            ? [
                `${andList(scopeNames(asked.setIds, sets.data || []))}${files ? `: ${plural(files, "table file")}` : ""}`,
              ]
            : []),
        ].join("; "),
        goal: generationTitle(latest, "Skipped"),
        draft: latest.finishedAt
          ? `Drafted ${formatRelative(latest.finishedAt)}`
          : "Drafted",
      }
    : {
        sources: "…",
        goal: "…",
        draft: "Drafted",
      };

  const go = (to: DraftStepId) => {
    setError(null);
    setParams({ step: to });
  };
  const generate = async () => {
    if (!wizard.request) return;
    setPending(true);
    setError(null);
    try {
      await generateOntology(id, o, wizard.request);
      if (saved) forgetNewOntology(id);
      setParams({}, { replace: true });
    } catch (e) {
      setError(e);
    }
    // Its step follows the generation, or the live one that refused it.
    await qc.invalidateQueries({ queryKey: [id, "ontology", o] });
    setPending(false);
  };

  const loadedAt =
    ontology.currentLoad?.finishedAt || ontology.lastLoad?.finishedAt;
  // The open step of the ontology's own, once a draft is under way.
  const open =
    setup.step === "current" ? (
      <ReadyToReview versionId={setup.versionId} current={v} />
    ) : setup.step === "load" ? (
      setup.state === "running" ? (
        <LiveLoad />
      ) : !v ? (
        <SkeletonText lines={2} />
      ) : (
        <>
          {ontology.lastLoad?.status === "failed" && (
            <p className="on-muted">
              The last load failed.{" "}
              {ontology.lastLoad.jobId && (
                <Link to={paths.load(o, ontology.lastLoad.jobId)}>See why</Link>
              )}
            </p>
          )}
          <NotLoaded version={v} />
          {refusal && <p className="on-muted">{refusal}</p>}
        </>
      )
    ) : (
      setup.step === "explore" && (
        <span className="on-card-actions">
          <ButtonLink variant="primary" size="sm" to={paths.explore(o)}>
            Open the Explorer
          </ButtonLink>
          <ButtonLink size="sm" to={paths.ask(o)}>
            Ask a question
          </ButtonLink>
        </span>
      )
    );
  const body = (step: DraftStepId) => {
    if (!manage)
      return (
        <p className="on-muted">
          An owner or admin chooses what it is drafted from and generates it.
        </p>
      );
    switch (step) {
      case "sources":
        return (
          <SourcesPicker
            chosen={chosen}
            setChosen={setChosen}
            chosenEvents={chosenEvents}
            setChosenEvents={setChosenEvents}
            chosenSets={chosenSets}
            setChosenSets={setChosenSets}
          />
        );
      case "goal":
        return (
          <GoalPanel
            goal={goal}
            setGoal={setGoal}
            ontology={ontology.name || ""}
            chosen={chosen}
            chosenEvents={chosenEvents}
            chosenSets={chosenSets}
          />
        );
      case "draft":
        return (
          <p className="on-muted">
            Foundry’s agent drafts a first version from what you chose. You
            review the draft before it is used.
          </p>
        );
    }
  };

  const done = 2 + (drafting && at === 0 ? 0 : 1) + reached,
    next =
      STEP_COPY[drafting ? wizard.steps[at]!.id : ONTOLOGY_STEPS[reached]!]
        .title;
  return (
    <Panel
      className={cx("on-setup", minimised && "is-minimised")}
      padded={!minimised}
      title={
        <button
          type="button"
          className="on-setup-toggle"
          aria-expanded={!minimised}
          aria-controls={minimised ? undefined : stepsId}
          title={minimised ? "Show the steps" : "Minimise"}
          onClick={() => setMinimisedText(minimised ? "" : "1")}
        >
          <ChevronDown size={16} className="on-setup-chevron" aria-hidden />
          Set it up
        </button>
      }
      subtitle={`${done} of 7 required steps done${minimised ? ` · Next: ${next}` : ""}`}
      actions={<AgentSetup ontology={{ ...ontology, setup }} compact />}
    >
      {!minimised && (
        <ol id={stepsId} className="on-wizard" aria-label="Setup steps">
          <FlowStep
            number={1}
            title={STEP_COPY.name.title}
            text={STEP_COPY.name.text}
            state="done"
            summary={ontologyName(ontology)}
          />
          <FlowStep
            number={2}
            title={STEP_COPY.database.title}
            text={STEP_COPY.database.text}
            state="done"
            summary={database}
          />
          {wizard.steps.map((s, i) => {
            const copy = STEP_COPY[s.id],
              state = drafting
                ? i === at
                  ? "current"
                  : i < at
                    ? "done"
                    : "upcoming"
                : s.id === "draft" && setup.step === "draft"
                  ? "running"
                  : "done";
            return (
              <FlowStep
                key={s.id}
                number={i + 3}
                title={copy.title}
                text={copy.text}
                optional={s.optional}
                state={state}
                summary={drafting ? stepSummary(s.state) : said[s.id]}
                onEdit={drafting && manage ? () => go(s.id) : undefined}
              >
                {state === "running" ? (
                  <LiveGeneration />
                ) : (
                  state === "current" && (
                    <>
                      {s.id === "draft" &&
                        setup.step === "draft" &&
                        setup.state === "failed" && (
                          <Callout
                            icon={TriangleAlert}
                            tone="danger"
                            title="The draft failed"
                            text={
                              setup.generation.error ? (
                                // The WFE's copy may say where an owner fixes it.
                                <SettingsLinked
                                  text={sentence(setup.generation.error)}
                                />
                              ) : (
                                "Foundry’s agent couldn’t draft it."
                              )
                            }
                            action={
                              <ButtonLink
                                size="sm"
                                to={paths.generation(o, setup.generation.id)}
                              >
                                Details
                              </ButtonLink>
                            }
                          />
                        )}
                      {body(s.id)}
                      {manage && (
                        <div className="on-generate-foot">
                          {s.state.kind === "blocked" && (
                            <p className="on-muted" role="status">
                              {s.state.reason}
                            </p>
                          )}
                          {s.state.kind === "ready" && s.state.note && (
                            <p className="on-muted" role="status">
                              {s.state.note}
                            </p>
                          )}
                          {s.id === "draft" && (
                            <AiRefusal
                              action="Generating an ontology"
                              error={error}
                              ready={aiReady}
                            >
                              {!!error && (
                                <p className="hf-form-error" role="alert">
                                  <SettingsLinked
                                    text={sentence((error as Error).message)}
                                  />
                                </p>
                              )}
                            </AiRefusal>
                          )}
                          <div className="hf-form-actions">
                            {wizard.back && (
                              <Button onClick={() => go(wizard.back!)}>
                                Back
                              </Button>
                            )}
                            {wizard.next ? (
                              <Button
                                variant="primary"
                                iconRight={ArrowRight}
                                disabled={
                                  s.state.kind === "blocked" ||
                                  s.state.kind === "loading"
                                }
                                onClick={() => go(wizard.next!)}
                              >
                                {s.state.kind === "skipped"
                                  ? "Skip"
                                  : "Continue"}
                              </Button>
                            ) : (
                              <Button
                                variant="primary"
                                icon={failed ? RotateCcw : Sparkles}
                                pending={pending}
                                disabled={!wizard.request || aiReady === false}
                                onClick={() => void generate()}
                              >
                                {failed ? "Try again" : "Generate"}
                              </Button>
                            )}
                          </div>
                        </div>
                      )}
                    </>
                  )
                )}
              </FlowStep>
            );
          })}
          {ONTOLOGY_STEPS.map((step, i) => {
            if (step === "draft") return null;
            const copy = STEP_COPY[step],
              state =
                i < reached
                  ? "done"
                  : i > reached
                    ? "upcoming"
                    : setup.state === "running"
                      ? "running"
                      : "current";
            return (
              <FlowStep
                key={step}
                number={i + DRAFT_STEPS.length + 2}
                title={copy.title}
                text={copy.text}
                state={state}
                summary={
                  step === "current"
                    ? v
                      ? `Version ${v.version}`
                      : "…"
                    : loadedAt
                      ? `Loaded ${formatRelative(loadedAt)}`
                      : "Loaded"
                }
              >
                {open}
              </FlowStep>
            );
          })}
        </ol>
      )}
    </Panel>
  );
}
