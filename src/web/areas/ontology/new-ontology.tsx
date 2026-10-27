import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowRight,
  CreditCard,
  Database,
  Shapes,
  Sparkles,
} from "lucide-react";
import { LOCAL } from "../../../shared/deployment";
import type { FoundryEventSourceMode } from "../../../shared/wfe";
import { ApiError } from "../../api";
import {
  Button,
  ButtonLink,
  EmptyState,
  Page,
  PageHeader,
  Panel,
  Segmented,
  Select,
  SkeletonText,
  cx,
  formatRelative,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { ConnectData } from "../data/connect-data";
import { connectorsQuery } from "../data/data-api";
import { EventSourceSendDialog } from "../data/event-source-send";
import {
  documentQueries,
  documentSetQueries,
} from "../documents/documents-api";
import { AiGate, useAiConfigured } from "../workspace/ai-gate";
import { DASHBOARD_URL } from "../workspace/members";
import { AgentSetup } from "./agent-setup";
import { createDatabase, createProject, databasesQuery } from "./databases-api";
import { GoalPanel, SourcesPicker, type EventChoices } from "./generate-parts";
import {
  STEP_COPY,
  databaseChoices,
  isNoSubscription,
  newDatabase,
  newOntologyWizard,
  slugify,
  stepSummary,
  type DatabasePick,
  type WizardStepId,
} from "./model";
import { Callout, FlowStep, canManage, type Arrival } from "./parts";
import {
  createOntology,
  generateOntology,
  isLive,
  ontologyQueries,
} from "./queries";

// /ontology/new: a new ontology and its first draft, step by step on one
// page: a name, a database (one the workspace has on any of its projects,
// or a new one on Helix's shared cluster, never a dedicated one, in a chosen
// or new project), what to draft from (connectors and document sets, in one
// step: either will do, so someone with only documents generates from
// them), a goal, then a review. Data is connected and documents uploaded in
// place (./generate-parts.tsx, shared with Generate), so nobody leaves for
// Data and comes back. The step is the URL's ?step= (none is the first), so
// Back, Forward and a refresh keep it; the steps' checks, where Back and
// Continue go and what each step chose are the model's (newOntologyWizard).
//
// The choices are kept in this tab (sessionStorage) until the ontology's
// first generation starts, with one client token and what an earlier
// attempt created (project, database), so a refresh keeps them and a retry
// creates neither the ontology nor its database twice. Create makes the
// database if new, the ontology, then its first generation, and opens the
// generation's progress. A generation refused as already running (an
// earlier attempt's) opens that one; any other failure opens the ontology,
// saying why, and its overview carries on from the choices kept, which now
// name the ontology (./setup.tsx).
//
// While the workspace has no ontology, this page is its onboarding, all the
// shell offers (main.tsx): it has no way back to Ontologies, and offers Set
// up with your agent. A connector link sent here (?add=<kind>) opens its
// form over the wizard once, and the new connection stays here. An API
// source's token link (?event_source=<id>&token=new, forwarded from its
// Data page by main.tsx) opens its send panel over the wizard once, at
// whatever step it is on: a link from an agent opens a new tab, whose
// wizard (kept per tab) starts afresh at Name.

// What an earlier attempt created: reused while the choice that made it
// stands.
type Made = {
  project?: { id: string; name: string };
  database?: { id: string; projectId: string; name: string };
};
type Saved = {
  name: string;
  database: DatabasePick | null;
  connectorIds: string[] | null;
  // The API sources chosen, each with its mode; none in choices kept before
  // there were API sources.
  eventSources: [string, FoundryEventSourceMode][];
  setIds: string[];
  goal: string;
  token: string;
  made: Made;
  // Set once the ontology exists and its generation didn't start: the
  // choices are its overview's to carry on from, not a new ontology's.
  ontologyId?: string;
};
const savedKey = (ws: string) => "foundry.new-ontology.v1:" + ws;
/** This tab's new-ontology choices in `ws`, if it kept any. */
export function readNewOntology(ws: string): Saved | null {
  try {
    const s = JSON.parse(sessionStorage.getItem(savedKey(ws)) || "null");
    if (!s || typeof s !== "object") return null;
    const texts = (v: unknown) =>
      Array.isArray(v) && v.every((x) => typeof x === "string") ? v : null;
    const mode = (v: unknown): v is FoundryEventSourceMode =>
      v === "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED" ||
      v === "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION";
    return {
      name: typeof s.name === "string" ? s.name : "",
      database:
        s.database?.kind === "existing" || s.database?.kind === "new"
          ? s.database
          : null,
      connectorIds: texts(s.connectorIds),
      // Each a pair of a non-empty id and a mode; anything else is dropped.
      eventSources: (Array.isArray(s.eventSources)
        ? (s.eventSources as unknown[])
        : []
      ).filter(
        (e): e is [string, FoundryEventSourceMode] =>
          Array.isArray(e) &&
          e.length === 2 &&
          typeof e[0] === "string" &&
          e[0] !== "" &&
          mode(e[1]),
      ),
      setIds: texts(s.setIds) || [],
      goal: typeof s.goal === "string" ? s.goal : "",
      token: typeof s.token === "string" && s.token ? s.token : "",
      made: s.made && typeof s.made === "object" ? s.made : {},
      ontologyId:
        typeof s.ontologyId === "string" && s.ontologyId
          ? s.ontologyId
          : undefined,
    };
  } catch {
    return null;
  }
}
const keep = (ws: string, saved: Saved) => {
  try {
    sessionStorage.setItem(savedKey(ws), JSON.stringify(saved));
  } catch {
    /* storage unavailable: the choices last as long as the page */
  }
};
/** Drops this tab's new-ontology choices in `ws`. */
export const forgetNewOntology = (ws: string) => {
  try {
    sessionStorage.removeItem(savedKey(ws));
  } catch {
    /* storage unavailable: nothing was kept */
  }
};

export function NewOntologyPage() {
  const { id, slug, role, onboarding } = useWorkspace(),
    navigate = useNavigate(),
    qc = useQueryClient(),
    [params, setParams] = useSearchParams();
  const [adding, setAdding] = useState(() => params.get("add")),
    // The API source a token link opens the send panel of.
    [sending, setSending] = useState(() =>
      params.get("token") === "new" ? params.get("event_source") : null,
    );
  useEffect(() => {
    const read = ["add", "event_source", "token"].filter((k) => params.has(k));
    if (!read.length) return;
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        for (const k of read) next.delete(k);
        return next;
      },
      { replace: true },
    );
  }, [params, setParams]);
  const databases = useQuery(databasesQuery(id)),
    ontologies = useQuery(ontologyQueries.list(id)),
    connectors = useQuery(connectorsQuery(id)),
    sets = useQuery(documentSetQueries.list(id)),
    documents = useQuery(documentQueries.workspace(id));
  const choices = useMemo(
    () =>
      databases.data && ontologies.data
        ? databaseChoices(databases.data, ontologies.data.ontologies || [])
        : undefined,
    [databases.data, ontologies.data],
  );
  // Choices that name an ontology are its overview's: this starts afresh.
  const [saved] = useState((): Saved => {
    const s = readNewOntology(id);
    return s && !s.ontologyId && s.token
      ? s
      : {
          name: "",
          database: null,
          connectorIds: null,
          eventSources: [],
          setIds: [],
          goal: "",
          token: crypto.randomUUID(),
          made: {},
        };
  });
  const [name, setName] = useState(saved.name),
    [database, setDatabase] = useState(saved.database),
    [chosen, setChosen] = useState<Set<string> | null>(
      saved.connectorIds && new Set(saved.connectorIds),
    ),
    [chosenEvents, setChosenEvents] = useState<EventChoices | null>(
      () => new Map(saved.eventSources),
    ),
    [chosenSets, setChosenSets] = useState<Set<string> | null>(
      new Set(saved.setIds),
    ),
    [goal, setGoal] = useState(saved.goal),
    [made, setMade] = useState(saved.made);
  const [pending, setPending] = useState(false),
    [error, setError] = useState<unknown>(null);
  // Locally, the first draft waits for an AI provider: the ontology is
  // still created, and its page says why its draft didn't start.
  const aiReady = useAiConfigured();
  // Set once the ontology exists (or on Cancel): these are no longer kept.
  const created = useRef(false);
  const entry: Saved = {
    name,
    database,
    connectorIds: chosen && [...chosen],
    eventSources: [...(chosenEvents || [])],
    setIds: [...(chosenSets || [])],
    goal,
    token: saved.token,
    made,
  };
  useEffect(() => {
    if (!created.current) keep(id, entry);
  }, [id, name, database, chosen, chosenEvents, chosenSets, goal, made]);

  const wizard = newOntologyWizard(
    {
      name,
      database,
      connectorIds: chosen,
      eventSources: chosenEvents || new Map(),
      setIds: chosenSets || new Set(),
      goal,
    },
    {
      databases: choices,
      projects: databases.data?.map((p) => p.project),
      connectors: connectors.data?.connectors,
      sets: sets.data,
      documents: documents.data,
    },
    params.get("step"),
  );
  const at = wizard.steps.findIndex((s) => s.id === wizard.current),
    step = wizard.steps[at]!;
  const crumbs = onboarding
      ? undefined
      : [{ label: "Ontologies", to: paths.ontologies() }],
    title = onboarding ? "Create your first ontology" : "New ontology";

  if (!canManage(role))
    return (
      <Page width="narrow">
        <PageHeader breadcrumbs={crumbs} title={title} />
        {onboarding ? (
          <EmptyState
            icon={Shapes}
            title="This workspace isn’t set up yet"
            text="An owner or admin sets it up. Its ontology shows here once there is one."
          />
        ) : (
          <EmptyState
            icon={Shapes}
            title="Only owners and admins can create ontologies"
            text="Ask an owner or admin of this workspace to create one."
            action={
              <ButtonLink to={paths.ontologies()}>
                Back to Ontologies
              </ButtonLink>
            }
          />
        )}
      </Page>
    );

  // Each step is a history entry; the first has no ?step=.
  const go = (to: WizardStepId) => {
    setError(null);
    setParams(to === "name" ? {} : { step: to });
    window.scrollTo(0, 0);
  };
  const cancel = () => {
    created.current = true;
    forgetNewOntology(id);
    navigate(paths.ontologies());
  };

  const create = async () => {
    const request = wizard.request;
    if (!request) return;
    setPending(true);
    setError(null);
    try {
      const pick = request.database;
      let tenantId = pick.kind === "existing" ? pick.tenantId : "";
      if (pick.kind === "new") {
        const p = pick.project;
        let projectId = p.kind === "existing" ? p.id : "";
        if (p.kind === "new") {
          projectId =
            made.project?.name === p.name
              ? made.project.id
              : (
                  await createProject({
                    workspaceId: id,
                    slug: slugify(p.name, "project"),
                    displayName: p.name,
                  })
                ).id;
          setMade((m) => ({ ...m, project: { id: projectId, name: p.name } }));
        }
        const reuse =
          made.database?.projectId === projectId &&
          made.database.name === pick.name;
        tenantId = reuse
          ? made.database!.id
          : (
              await createDatabase({
                projectId,
                name: pick.name,
                slug: slugify(pick.name, "database"),
              })
            )?.id || "";
        if (!tenantId) throw new Error("Helix didn’t return the new database");
        setMade((m) => ({
          ...m,
          database: { id: tenantId, projectId, name: pick.name },
        }));
      }
      const { ontology } = await createOntology(id, {
        name: request.name,
        tenantId,
        clientToken: saved.token,
      });
      created.current = true;
      const o = ontology.id,
        arrival: Arrival = { newDatabase: pick.kind === "new" };
      // Until its generation starts, the choices are the ontology's.
      keep(id, { ...entry, ontologyId: o });
      const [to, state] = await generateOntology(id, o, request.generate).then(
        (res): [string, Arrival] => [
          paths.generation(o, res.generation!.id),
          arrival,
        ],
        async (e): Promise<[string, Arrival]> => {
          // An earlier attempt's generation is live: follow it.
          const live =
            e instanceof ApiError && e.status === 409
              ? (
                  await qc
                    .fetchQuery({
                      ...ontologyQueries.generations(id, o),
                      staleTime: 0,
                    })
                    .catch(() => undefined)
                )?.generations?.find(isLive)
              : undefined;
          return live
            ? [paths.generation(o, live.id), arrival]
            : [
                paths.ontology(o),
                {
                  ...arrival,
                  generateFailed: {
                    message: (e as Error).message || "it couldn’t start",
                  },
                },
              ];
        },
      );
      if (!state.generateFailed) forgetNewOntology(id);
      navigate(to, { state });
      // Only once this page is left: the new ontology uses its database, so
      // read here it would send the wizard back to the database step.
      for (const key of ["ontologies", "databases", "projects"])
        void qc.invalidateQueries({ queryKey: [id, key] });
    } catch (e) {
      setError(e);
      void qc.invalidateQueries({ queryKey: [id, "databases"] });
    } finally {
      setPending(false);
    }
  };

  const body = () => {
    switch (step.id) {
      case "name":
        return (
          <form
            onSubmit={(e) => {
              e.preventDefault();
              if (wizard.next && step.state.kind === "ready") go(wizard.next);
            }}
          >
            <Panel title="Name">
              <label className="hf-field on-goal">
                <span className="hf-sr-only">Ontology name</span>
                <input
                  required
                  autoFocus
                  maxLength={100}
                  placeholder="e.g. Customer contracts"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </label>
            </Panel>
          </form>
        );
      case "database":
        return (
          <DatabasePanel
            ontology={name}
            pick={database}
            onPick={(next) => {
              setDatabase(next);
              setError(null);
            }}
          />
        );
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
            ontology={name.trim()}
            chosen={chosen}
            chosenEvents={chosenEvents}
            chosenSets={chosenSets}
          />
        );
      case "review":
        return (
          <p className="on-muted">
            Foundry creates {name.trim()}
            {database?.kind === "new" ? " and its new database" : ""}, then its
            agent drafts a first version from what you chose. You review the
            draft before it is used.
          </p>
        );
    }
  };

  const message = error instanceof Error ? error.message : "";
  return (
    <Page width="narrow">
      <PageHeader
        breadcrumbs={crumbs}
        title={title}
        subtitle="An ontology keeps its types, documents and data in one Helix database. Choose what it is drawn from, and Foundry’s agent drafts it."
      />
      {onboarding && <AgentSetup />}
      {adding !== null && (
        <ConnectData
          initialKind={adding}
          onConnected={() => {}}
          onClose={() => setAdding(null)}
        />
      )}
      {sending && (
        <EventSourceSendDialog
          connectorId={sending}
          onClose={() => setSending(null)}
        />
      )}
      <ol className="on-wizard" aria-label="Steps">
        {wizard.steps.map((s, i) => {
          const copy = STEP_COPY[s.id];
          return (
            <FlowStep
              key={s.id}
              number={i + 1}
              title={copy.title}
              text={copy.text}
              optional={s.optional}
              state={i === at ? "current" : i < at ? "done" : "upcoming"}
              summary={stepSummary(s.state)}
              onEdit={() => go(s.id)}
            >
              {i === at && body()}
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
                {s.id === "review" && aiReady === false && (
                  <AiGate action="Its first draft" />
                )}
                {s.id === "review" &&
                  error !== null &&
                  (isNoSubscription(error) ? (
                    <Callout
                      icon={CreditCard}
                      tone="danger"
                      title="A new database needs a Helix plan"
                      text="This workspace has no active Helix subscription, which a database on the shared cluster needs. Choose a plan in the Helix dashboard, or use a database you already have."
                      action={
                        <span className="on-card-actions">
                          <a
                            className="hf-btn hf-btn--secondary hf-btn--sm"
                            href={`${DASHBOARD_URL}/${encodeURIComponent(slug)}/billing`}
                            target="_blank"
                            rel="noreferrer"
                          >
                            Open Helix billing
                          </a>
                          {choices?.some((c) => !c.usedBy) && (
                            <Button
                              size="sm"
                              onClick={() => {
                                setDatabase(null);
                                go("database");
                              }}
                            >
                              Use an existing one
                            </Button>
                          )}
                        </span>
                      }
                    />
                  ) : (
                    <p className="hf-form-error" role="alert">
                      {message
                        ? message.charAt(0).toUpperCase() +
                          message.slice(1) +
                          "."
                        : "Couldn’t create the ontology."}
                    </p>
                  ))}
                <div className="hf-form-actions">
                  {wizard.back ? (
                    <Button onClick={() => go(wizard.back!)}>Back</Button>
                  ) : (
                    !onboarding && <Button onClick={cancel}>Cancel</Button>
                  )}
                  {wizard.next ? (
                    <Button
                      variant="primary"
                      iconRight={ArrowRight}
                      disabled={
                        s.state.kind === "blocked" || s.state.kind === "loading"
                      }
                      onClick={() => go(wizard.next!)}
                    >
                      {s.state.kind === "skipped" ? "Skip" : "Continue"}
                    </Button>
                  ) : (
                    <Button
                      variant="primary"
                      icon={Sparkles}
                      pending={pending}
                      disabled={!wizard.request}
                      onClick={() => void create()}
                    >
                      {aiReady === false ? "Create" : "Create and generate"}
                    </Button>
                  )}
                </div>
              </div>
            </FlowStep>
          );
        })}
      </ol>
    </Page>
  );
}

const NEW_PROJECT = "__new";

/**
 * The database step: a live database of the workspace's not used by
 * another ontology, or a new one on the shared cluster in a project it has
 * or a new one, filled in from the ontology's name (newDatabase). With no
 * free database, a new one is the only way, so it starts chosen.
 */
function DatabasePanel({
  ontology,
  pick,
  onPick,
}: {
  ontology: string;
  pick: DatabasePick | null;
  onPick: (next: DatabasePick | null) => void;
}) {
  const { id, name } = useWorkspace();
  const databases = useQuery(databasesQuery(id)),
    ontologies = useQuery(ontologyQueries.list(id));
  const choices = useMemo(
    () =>
      databaseChoices(databases.data || [], ontologies.data?.ontologies || []),
    [databases.data, ontologies.data],
  );
  const projects = (databases.data || []).map((p) => p.project);
  // Which half shows; an existing database not chosen yet has no pick.
  const [mode, setMode] = useState(pick?.kind ?? null);
  const shown =
    mode ??
    (databases.isSuccess && !choices.some((c) => !c.usedBy)
      ? "new"
      : "existing");
  // The new database's fields, as typed or as they start.
  const fresh =
    pick?.kind === "new" ? pick : newDatabase(ontology, name, projects);
  useEffect(() => {
    if (mode === null && !pick && shown === "new" && databases.isSuccess)
      onPick(fresh);
  }, [mode, pick, shown, databases.isSuccess]);
  const typed = fresh.name.trim();
  return (
    <Panel
      className="on-db-panel"
      title="Database"
      subtitle="Loads write the ontology’s entities and facts into it, beside anything it already holds. Foundry never deletes it."
      padded={false}
      actions={
        <Segmented
          label="Database"
          value={shown}
          onChange={(v) => {
            setMode(v as DatabasePick["kind"]);
            onPick(v === "new" ? fresh : null);
          }}
          options={[
            { value: "existing", label: "Use an existing one" },
            { value: "new", label: "Create a new one" },
          ]}
        />
      }
    >
      {databases.isPending ? (
        <div className="on-pad">
          <SkeletonText lines={3} />
        </div>
      ) : !databases.data ? (
        <p className="on-pad hf-form-error" role="alert">
          Couldn’t load your databases. {databases.error?.message}
        </p>
      ) : shown === "existing" ? (
        choices.length ? (
          <ul className="on-connectors" aria-label="Databases">
            {choices.map((c) => (
              <li
                key={c.tenantId}
                className={cx("on-connector", c.usedBy && "is-off")}
              >
                <div className="on-connector-row">
                  <label className="on-check">
                    <input
                      type="radio"
                      name="database"
                      value={c.tenantId}
                      checked={
                        pick?.kind === "existing" &&
                        pick.tenantId === c.tenantId
                      }
                      disabled={!!c.usedBy}
                      aria-label={`${c.name} in ${c.project}`}
                      onChange={() => {
                        setMode("existing");
                        onPick({ kind: "existing", tenantId: c.tenantId });
                      }}
                    />
                    <Database size={18} aria-hidden />
                    <span className="on-connector-text">
                      <strong>{c.name}</strong>
                      <small>
                        {c.project}
                        {c.usedBy
                          ? ` · used by ${c.usedBy}`
                          : c.createdAt
                            ? ` · created ${formatRelative(c.createdAt)}`
                            : ""}
                      </small>
                    </span>
                  </label>
                </div>
              </li>
            ))}
          </ul>
        ) : (
          <EmptyState
            size="sm"
            icon={Database}
            title="No databases in this workspace yet"
            text={
              LOCAL
                ? "Create one here: it is kept in the Helix that runs with Foundry on this computer."
                : "Create one here: it goes on Helix’s shared cluster."
            }
            action={
              <Button
                onClick={() => {
                  setMode("new");
                  onPick(fresh);
                }}
              >
                Create a database
              </Button>
            }
          />
        )
      ) : (
        <div className="on-pad on-new-db">
          <label className="hf-field">
            Project
            <Select
              label="Project"
              value={
                fresh.project.kind === "existing"
                  ? fresh.project.id
                  : NEW_PROJECT
              }
              onChange={(id) =>
                onPick({
                  ...fresh,
                  project:
                    id === NEW_PROJECT
                      ? { kind: "new", name: "" }
                      : { kind: "existing", id },
                })
              }
              options={[
                ...projects.map((p) => ({
                  value: p.id,
                  label: p.displayName || p.slug || p.id,
                })),
                { value: NEW_PROJECT, label: "A new project…" },
              ]}
            />
          </label>
          {fresh.project.kind === "new" && (
            <label className="hf-field">
              Project name
              <input
                required
                maxLength={63}
                placeholder="e.g. Foundry"
                value={fresh.project.name}
                onChange={(e) =>
                  onPick({
                    ...fresh,
                    project: { kind: "new", name: e.target.value },
                  })
                }
              />
            </label>
          )}
          <label className="hf-field">
            Database name
            <input
              required
              maxLength={63}
              placeholder="e.g. contracts"
              value={fresh.name}
              onChange={(e) => onPick({ ...fresh, name: e.target.value })}
              aria-describedby="on-db-hint"
            />
            <small id="on-db-hint">
              {typed
                ? `Its address is ${slugify(typed, "database")}, with a short suffix if another database has it. `
                : ""}
              {LOCAL
                ? "It is kept in the Helix that runs with Foundry on this computer."
                : "It goes on Helix’s shared cluster and uses your workspace’s Helix plan."}
            </small>
          </label>
        </div>
      )}
    </Panel>
  );
}
