import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowRight,
  CreditCard,
  Database,
  RotateCcw,
  Shapes,
  Sparkles,
} from "lucide-react";
import { LOCAL } from "../../../shared/deployment";
import { isRecord } from "../../../shared/json";
import type {
  FoundryCompanyResearch,
  FoundryEventSourceMode,
} from "../../../shared/wfe";
import { ApiError, refusalOf, sentence } from "../../api";
import {
  Button,
  ButtonLink,
  ConfirmDialog,
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
import { trackEvent } from "../../analytics";
import { canManage, useWorkspace } from "../../ui";
import { ConnectData } from "../data/connect-data";
import { SignInReturn } from "../data/sign-in";
import { connectorsQuery } from "../data/data-api";
import { EventSourceSendDialog } from "../data/event-source-send";
import {
  documentQueries,
  documentSetQueries,
} from "../documents/documents-api";
import { AiGate, useAiConfigured } from "../workspace/ai-gate";
import { DASHBOARD_URL } from "../workspace/members";
import { AgentSetup } from "./agent-setup";
import { CompanyPanel } from "./company-step";
import { createDatabase, createProject, databasesQuery } from "./databases-api";
import {
  GoalPanel,
  SourcesPicker,
  useGoalSources,
  type EventChoices,
} from "./generate-parts";
import { goalData } from "./goal-prompt";
import {
  STEP_COPY,
  companyDomain,
  companyFlow,
  databaseChoices,
  databasePlan,
  isNoSubscription,
  newDatabase,
  newOntologyWizard,
  slugify,
  stepSummary,
  type CompanyFlow,
  type DatabasePick,
  type MadeResources,
  type WizardStepId,
} from "./model";
import { Callout, FlowStep, type Arrival } from "./parts";
import { SAMPLE, isSampleConnector, isSampleSet } from "./sample";
import {
  createOntology,
  draftGoal,
  generateOntology,
  isUnderWay,
  ontologyQueries,
  researchCompany,
} from "./queries";

// /ontology/new: a new ontology and its first draft, step by step on one
// page (newOntologyWizard): the user's company (optional: its site read and
// its questions answered, ./company-step.tsx), what to draft from
// (connectors, API sources and document sets in one step: any will do, so
// someone with only documents generates from them), a goal (drafted from
// the company's answers and the chosen data when there are any, and the
// goal is empty), then a review, whose Advanced holds the name (else
// derived: the drafted goal's, the research's, the goal's title, else the
// sources') and the database (else a new one named as the ontology). Data
// is connected and documents uploaded in place (./generate-parts.tsx,
// shared with Generate), so nobody leaves for Data and comes back. The
// sample company's connector and set are no source here, whatever the
// choices kept name: this ontology is the user's own. The step is the URL's
// ?step= (none is the first), so Back, Forward and a refresh keep it.
//
// The choices are kept in this tab (sessionStorage) until the ontology's
// first generation starts, with one client token and what an earlier
// attempt created (project, database), so a refresh keeps them and a retry
// creates neither the ontology nor its database twice. Create makes the
// database if new, the ontology, then its first generation, which is made
// current and loaded as soon as it is stored, with an email to say so
// (GenerateOntologyRequest's loadWhenReady: a new ontology has no current
// version), and opens the generation's progress. A generation refused as
// already running (an earlier attempt's) opens that one; any other failure
// opens the ontology, saying why, and its overview carries on from the
// choices kept, which now name the ontology (./setup.tsx).
//
// While the workspace has no ontology, this page is its onboarding, all the
// shell offers (main.tsx): it has no way back to Ontologies, and first asks
// how to start, two cards side by side: Set up with your agent (its MCP,
// the install's own locally) and Set up here (this wizard), with the
// sample company a link under them. A connector link sent here
// (?add=<kind>) opens its form over the page once, and the new connection
// stays here; with an agent's sign-in (&sign_in=<id>) the form is bound to
// it. A sign-in's callback comes back to the sources step with the
// connector it made (?step=sources&connected=<id>), chosen as one connected
// here is. An API source's token link (?event_source=<id>&token=new,
// forwarded from its Data page by main.tsx) opens its send panel over the
// wizard once, at whatever step it is on: a link from an agent opens a new
// tab, whose wizard (kept per tab) starts afresh.

type Saved = {
  // The company's domain as typed, and its research with the answers, by
  // question id.
  domain: string;
  company: {
    research: FoundryCompanyResearch;
    answers: [string, string][];
  } | null;
  // The goal a draft from the research wrote, and the name it came with:
  // null until one is drafted for this research.
  drafted: { goal: string; name: string } | null;
  // Advanced's overrides: "" and null are the derived name and a new
  // database.
  name: string;
  database: DatabasePick | null;
  connectorIds: string[] | null;
  // The API sources chosen, each with its mode.
  eventSources: [string, FoundryEventSourceMode][];
  setIds: string[];
  goal: string;
  token: string;
  // What an earlier attempt created, for a retry to reuse (databasePlan).
  made: MadeResources;
  // Set once the ontology exists and its generation didn't start: the
  // choices are its overview's to carry on from, not a new ontology's.
  ontologyId?: string;
};
const savedKey = (ws: string) => "foundry.new-ontology.v1:" + ws;
// Where a sign-in started here returns: the sources step, its choices kept
// in this tab.
const BACK = { path: paths.newOntology() + "?step=sources" };
/**
 * This tab's new-ontology choices in `ws`, if it kept any; whatever doesn't
 * read as its field is dropped to its empty value. A nested value is
 * checked whole and dropped whole when any part of it doesn't read: the
 * database, the company (its research, questions and answers, each answer
 * naming one of its questions, its pages http(s) URLs) with the goal drafted
 * from it, and each of what an earlier attempt made.
 */
export function readNewOntology(ws: string): Saved | null {
  try {
    const s: unknown = JSON.parse(
      sessionStorage.getItem(savedKey(ws)) || "null",
    );
    if (!isRecord(s)) return null;
    const isText = (v: unknown): v is string => typeof v === "string",
      text = (v: unknown) => (isText(v) ? v : ""),
      id = (v: unknown): v is string => isText(v) && v !== "",
      // Missing, or what `ok` reads.
      optional = (v: unknown, ok: (x: unknown) => boolean) =>
        v === undefined || ok(v);
    const texts = (v: unknown): v is string[] =>
      Array.isArray(v) && v.every(isText);
    const mode = (v: unknown): v is FoundryEventSourceMode =>
      v === "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED" ||
      v === "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION";
    const pairs = <T,>(v: unknown, second: (x: unknown) => x is T) =>
      (Array.isArray(v) ? (v as unknown[]) : []).filter(
        (e): e is [string, T] =>
          Array.isArray(e) && e.length === 2 && id(e[0]) && second(e[1]),
      );

    const database = ((d): DatabasePick | null => {
      if (!isRecord(d)) return null;
      if (d.kind === "existing")
        return isText(d.tenantId)
          ? { kind: "existing", tenantId: d.tenantId }
          : null;
      const p = isRecord(d.project) ? d.project : null;
      if (d.kind !== "new" || !isText(d.name) || !p) return null;
      return p.kind === "existing" && id(p.id)
        ? { kind: "new", project: { kind: "existing", id: p.id }, name: d.name }
        : p.kind === "new" && isText(p.name)
          ? {
              kind: "new",
              project: { kind: "new", name: p.name },
              name: d.name,
            }
          : null;
    })(s.database);

    const company = ((c): Saved["company"] => {
      const r = isRecord(c) && isRecord(c.research) ? c.research : null;
      if (
        !r ||
        !id(r.id) ||
        ![r.domain, r.companyName, r.summary, r.suggestedName].every((v) =>
          optional(v, isText),
        ) ||
        !optional(
          r.sources,
          (v) => texts(v) && v.every((u) => /^https?:\/\//i.test(u)),
        ) ||
        !optional(r.fetched, (v) => typeof v === "boolean") ||
        !(
          r.createdAt === undefined ||
          r.createdAt === null ||
          isText(r.createdAt)
        ) ||
        !optional(r.questions, Array.isArray)
      )
        return null;
      const questions = ((r.questions as unknown[] | undefined) || []).map(
        (q) =>
          isRecord(q) &&
          id(q.id) &&
          optional(q.question, isText) &&
          optional(q.bestGuess, isText) &&
          optional(q.options, texts)
            ? {
                id: q.id,
                question: q.question as string | undefined,
                bestGuess: q.bestGuess as string | undefined,
                options: q.options as string[] | undefined,
              }
            : null,
      );
      const answers =
        isRecord(c) && Array.isArray(c.answers)
          ? (c.answers as unknown[])
          : null;
      if (
        questions.some((q) => !q) ||
        !answers ||
        !answers.every(
          (a) =>
            Array.isArray(a) &&
            a.length === 2 &&
            questions.some((q) => q!.id === a[0]) &&
            isText(a[1]),
        )
      )
        return null;
      return {
        research: {
          id: r.id,
          domain: r.domain as string | undefined,
          companyName: r.companyName as string | undefined,
          summary: r.summary as string | undefined,
          suggestedName: r.suggestedName as string | undefined,
          questions: questions as NonNullable<
            FoundryCompanyResearch["questions"]
          >,
          sources: r.sources as string[] | undefined,
          fetched: r.fetched as boolean | undefined,
          createdAt: r.createdAt as string | null | undefined,
        },
        answers: answers as [string, string][],
      };
    })(s.company);
    const drafted = isRecord(s.drafted) ? s.drafted : null;

    const made = isRecord(s.made) ? s.made : {},
      madeProject = isRecord(made.project) ? made.project : null,
      madeDatabase = isRecord(made.database) ? made.database : null;
    return {
      domain: text(s.domain),
      company,
      // A goal is drafted from research: none without it.
      drafted:
        company && drafted
          ? { goal: text(drafted.goal), name: text(drafted.name) }
          : null,
      name: text(s.name),
      database,
      connectorIds: texts(s.connectorIds) ? s.connectorIds : null,
      eventSources: pairs(s.eventSources, mode),
      setIds: texts(s.setIds) ? s.setIds : [],
      goal: text(s.goal),
      token: text(s.token),
      made: {
        ...(madeProject && id(madeProject.id) && isText(madeProject.name)
          ? { project: { id: madeProject.id, name: madeProject.name } }
          : {}),
        ...(madeDatabase &&
        id(madeDatabase.id) &&
        id(madeDatabase.projectId) &&
        isText(madeDatabase.name)
          ? {
              database: {
                id: madeDatabase.id,
                projectId: madeDatabase.projectId,
                name: madeDatabase.name,
              },
            }
          : {}),
      },
      ontologyId: text(s.ontologyId) || undefined,
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
  const { id, slug, role, onboarding, name: workspace, user } = useWorkspace(),
    navigate = useNavigate(),
    qc = useQueryClient(),
    [params, setParams] = useSearchParams();
  // Onboarding asks how to start until the user picks Set up here, or a
  // link (a step, a connector's form, an API source's token) says so.
  const [own, setOwn] = useState(() =>
      ["step", "add", "event_source", "sign_in", "connected"].some((k) =>
        params.has(k),
      ),
    ),
    [adding, setAdding] = useState(() => params.get("add")),
    // The sign-in an agent's link names, for its form.
    [signIn] = useState(() => params.get("sign_in")),
    // The API source a token link opens the send panel of.
    [sending, setSending] = useState(() =>
      params.get("token") === "new" ? params.get("event_source") : null,
    );
  useEffect(() => {
    const read = [
      "add",
      "event_source",
      "token",
      "sign_in",
      "connected",
    ].filter((k) => params.has(k));
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
  // A read the review's database waits for that failed, for it to say so.
  const readFailed =
    (!databases.data && databases.error) ||
    (!ontologies.data && ontologies.error) ||
    null;
  // Choices that name an ontology are its overview's: this starts afresh,
  // its domain the one the user's email names (none locally: the owner is
  // owner@localhost).
  const [saved] = useState((): Saved => {
    const s = readNewOntology(id);
    return s && !s.ontologyId && s.token
      ? s
      : {
          domain: LOCAL ? "" : companyDomain(user.email),
          company: null,
          drafted: null,
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
  // The company step and the goal, with the research and the goal's draft
  // under way (companyFlow).
  const [flow, setFlow] = useState(
      (): CompanyFlow => ({
        domain: saved.domain,
        company: saved.company && {
          research: saved.company.research,
          answers: new Map(saved.company.answers),
        },
        goal: saved.goal,
        drafted: saved.drafted,
        research: { kind: "idle" },
        draft: { kind: "idle" },
        autoDrafted: saved.drafted ? saved.company!.research.id : null,
      }),
    ),
    [name, setName] = useState(saved.name),
    [database, setDatabase] = useState(saved.database),
    // A connector a sign-in just made joins those chosen, as one connected
    // here does (every synced one, when none was chosen, includes it).
    [chosen, setChosen] = useState<Set<string> | null>(() => {
      const connected = params.get("connected");
      return (
        saved.connectorIds &&
        new Set([...saved.connectorIds, ...(connected ? [connected] : [])])
      );
    }),
    [chosenEvents, setChosenEvents] = useState<EventChoices | null>(
      () => new Map(saved.eventSources),
    ),
    [chosenSets, setChosenSets] = useState<Set<string> | null>(
      new Set(saved.setIds),
    ),
    [made, setMade] = useState(saved.made);
  const [pending, setPending] = useState(false),
    [error, setError] = useState<unknown>(null),
    // Advanced: name and database, open on the review.
    [advanced, setAdvanced] = useState(false),
    [redrafting, setRedrafting] = useState(false);
  // Locally, the first draft waits for an AI provider: the ontology is
  // still created, and its page says why its draft didn't start.
  const aiReady = useAiConfigured();
  // Set once the ontology exists (or on Cancel): these are no longer kept.
  const created = useRef(false);
  const { domain, company, drafted, goal } = flow;
  const setGoal = (goal: string) =>
    setFlow((f) => companyFlow(f, { type: "goal", goal }));
  // A draft of the goal under way: nothing moves on until it lands.
  const drafting = flow.draft.kind === "pending";
  const entry: Saved = {
    domain,
    company: company && {
      research: company.research,
      answers: [...company.answers],
    },
    drafted,
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
  }, [
    id,
    domain,
    company,
    drafted,
    name,
    database,
    chosen,
    chosenEvents,
    chosenSets,
    goal,
    made,
  ]);

  // The workspace's first ontology of its own: none but the sample's.
  const first =
    onboarding ||
    (!!ontologies.data &&
      !ontologies.data.ontologies?.some((o) => o.name !== SAMPLE.ontology));
  const wizard = newOntologyWizard(
    {
      company,
      draftedName: drafted?.name || "",
      name,
      database,
      connectorIds: chosen,
      eventSources: chosenEvents || new Map(),
      setIds: chosenSets || new Set<string>(),
      goal,
    },
    {
      databases: choices,
      projects: databases.data?.map((p) => p.project),
      connectors: connectors.data?.connectors?.filter(
        (c) => !isSampleConnector(c),
      ),
      sets: sets.data?.filter((s) => !isSampleSet(s)),
      documents: documents.data,
    },
    workspace,
    params.get("step"),
  );
  const ontologyName = name.trim() || wizard.derivedName;
  const at = wizard.steps.findIndex((s) => s.id === wizard.current),
    step = wizard.steps[at]!;
  const choosing = onboarding && !own;
  // The chosen data as the goal's draft reads it.
  const sources = useGoalSources({ chosen, chosenEvents, chosenSets });
  // Each call starts only when its start changed the flow, and its answer
  // goes through companyFlow, which drops one the flow has moved past (the
  // domain edited, another research chosen).
  const research = () => {
    const next = companyFlow(flow, { type: "research-start" });
    if (next === flow || next.research.kind !== "pending") return;
    const { domain } = next.research;
    setFlow(next);
    researchCompany(id, domain).then(
      ({ research }) =>
        setFlow((f) =>
          companyFlow(
            f,
            research
              ? { type: "research-done", domain, research }
              : {
                  type: "research-failed",
                  domain,
                  error: new Error("Foundry didn’t return the research"),
                },
          ),
        ),
      (error: unknown) =>
        setFlow((f) =>
          companyFlow(f, { type: "research-failed", domain, error }),
        ),
    );
  };
  // Whether a draft started.
  const writeGoal = (auto: boolean) => {
    const next = companyFlow(flow, { type: "draft-start", auto });
    if (!sources || next === flow || next.draft.kind !== "pending")
      return false;
    const { researchId } = next.draft;
    setFlow(next);
    draftGoal(id, researchId, {
      answers: [...(company?.answers || [])]
        .map(([questionId, answer]) => ({ questionId, answer: answer.trim() }))
        .filter((a) => a.answer),
      data: goalData(sources),
    }).then(
      (r) =>
        setFlow((f) =>
          companyFlow(f, {
            type: "draft-done",
            researchId,
            goal: r.goal || "",
            name: r.name || "",
          }),
        ),
      (error: unknown) =>
        setFlow((f) =>
          companyFlow(f, { type: "draft-failed", researchId, error }),
        ),
    );
    return true;
  };
  // The goal step, shown with research and no goal, drafts one once per
  // research (a failure says why and leaves the field to the user).
  // StrictMode runs the effect twice with one render's flow: the ref keeps
  // the second run from drafting again.
  const autoStarted = useRef<string | null>(null);
  useEffect(() => {
    const researchId = company?.research.id;
    if (step.id !== "goal" || !researchId || autoStarted.current === researchId)
      return;
    if (writeGoal(true)) autoStarted.current = researchId;
  }, [step.id, company?.research.id, !!sources]);
  // A draft that lands puts the cursor in the goal it wrote.
  const goalField = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    if (flow.draft.kind === "done") goalField.current?.focus();
  }, [flow.draft]);
  // The step shown, for where the wizard is left (Databuddy drops ?step=),
  // once `first` is known.
  const shown =
    !choosing &&
    canManage(role) &&
    (onboarding || !!ontologies.data) &&
    step.id;
  useEffect(() => {
    if (shown) trackEvent("ontology_step_shown", { step: shown, first });
  }, [shown, first]);
  const crumbs = onboarding
      ? undefined
      : [{ label: "Ontologies", to: paths.ontologies() }],
    title = choosing
      ? "Get started"
      : onboarding
        ? "Your first ontology"
        : "New ontology";

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
    setParams(to === wizard.steps[0]!.id ? {} : { step: to });
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
      const pick = request.database,
        // Advanced's database left at its default (null): a new one.
        plan = databasePlan(pick, made, database !== null);
      let tenantId = plan.kind === "use" ? plan.tenantId : "";
      if (plan.kind === "create") {
        const p = plan.project;
        const projectId =
          p.kind === "existing"
            ? p.id
            : (
                await createProject({
                  workspaceId: id,
                  slug: slugify(p.name, "project"),
                  displayName: p.name,
                })
              ).id;
        if (p.kind === "new")
          setMade((m) => ({ ...m, project: { id: projectId, name: p.name } }));
        tenantId =
          (
            await createDatabase({
              projectId,
              name: plan.name,
              slug: slugify(plan.name, "database"),
            })
          )?.id || "";
        if (!tenantId) throw new Error("Helix didn’t return the new database");
        setMade((m) => ({
          ...m,
          database: { id: tenantId, projectId, name: plan.name },
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
      const [to, state] = await generateOntology(id, o, {
        ...request.generate,
        loadWhenReady: true,
      }).then(
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
                )?.generations?.find(isUnderWay)
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
      // read here it would move Advanced's database on.
      for (const key of ["ontologies", "databases", "projects"])
        void qc.invalidateQueries({ queryKey: [id, key] });
    } catch (e) {
      setError(e);
      trackEvent("ontology_create_failed", {
        reason: isNoSubscription(e) ? "no_subscription" : refusalOf(e),
      });
      void qc.invalidateQueries({ queryKey: [id, "databases"] });
    } finally {
      setPending(false);
    }
  };

  const body = () => {
    switch (step.id) {
      case "company":
        return (
          <CompanyPanel
            domain={domain}
            company={company}
            research={flow.research}
            onDomain={(domain) =>
              setFlow((f) => companyFlow(f, { type: "domain", domain }))
            }
            onResearch={research}
            onAnswer={(questionId, answer) =>
              setFlow((f) =>
                companyFlow(f, { type: "answer", questionId, answer }),
              )
            }
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
            goal={drafting ? null : goal}
            setGoal={setGoal}
            ontology={ontologyName}
            chosen={chosen}
            chosenEvents={chosenEvents}
            chosenSets={chosenSets}
            fieldRef={goalField}
          >
            {company && (
              <div className="on-goal-draft">
                {flow.draft.kind === "failed" && (
                  <p className="hf-form-error" role="alert">
                    {sentence(
                      (flow.draft.error as Error).message ||
                        "the goal couldn’t be written",
                    )}
                  </p>
                )}
                <Button
                  size="sm"
                  icon={RotateCcw}
                  pending={drafting}
                  disabled={!sources}
                  onClick={() =>
                    goal.trim() && goal !== drafted?.goal
                      ? setRedrafting(true)
                      : writeGoal(false)
                  }
                >
                  Write it again
                </Button>
                {/* Mounted throughout, so what it says next is announced. */}
                <p className="on-muted" role="status">
                  {drafting
                    ? "Writing a goal from your answers…"
                    : flow.draft.kind === "done"
                      ? "Foundry wrote the goal from your answers. Edit it as you like."
                      : ""}
                </p>
              </div>
            )}
          </GoalPanel>
        );
      case "review":
        return (
          <>
            <p className="on-muted">
              Foundry creates {ontologyName || "the ontology"} on{" "}
              {database?.kind === "existing"
                ? "the database you chose"
                : "a new Helix database"}
              , then its agent drafts it from what you chose. Once the draft is
              ready, Foundry makes it current and loads it, and emails you when
              you can ask it questions.
            </p>
            <details
              className="on-advanced"
              // Open while what it holds keeps the review from creating.
              open={advanced || step.state.kind === "blocked"}
              onToggle={(e) => setAdvanced(e.currentTarget.open)}
            >
              <summary>Advanced: name and database</summary>
              <div className="on-stack">
                <Panel title="Name">
                  <label className="hf-field on-goal">
                    <span className="hf-sr-only">Ontology name</span>
                    <input
                      maxLength={100}
                      placeholder={wizard.derivedName}
                      value={name}
                      onChange={(e) => setName(e.target.value)}
                    />
                  </label>
                </Panel>
                <DatabasePanel
                  ontology={ontologyName}
                  pick={database}
                  onPick={(next) => {
                    setDatabase(next);
                    setError(null);
                  }}
                />
              </div>
            </details>
          </>
        );
    }
  };

  const message = error instanceof Error ? error.message : "";
  const connecting = adding !== null && (
    <ConnectData
      initialKind={adding}
      signIn={signIn}
      onConnected={() => {}}
      onClose={() => setAdding(null)}
    />
  );
  if (choosing)
    return (
      <SignInReturn.Provider value={BACK}>
        <Page width="narrow">
          <PageHeader
            title={title}
            subtitle="Foundry turns your data into an ontology: a graph you can ask questions of."
          />
          <div className="on-start-cards">
            <section className="on-card on-start-card">
              <AgentSetup />
            </section>
            <section className="on-card on-start-card">
              <div className="on-card-text">
                <strong>Set up here</strong>
                <p>
                  Foundry reads your company’s site and asks a few questions,
                  then you connect your data. It drafts the ontology from both.
                </p>
              </div>
              <Button
                variant="primary"
                size="sm"
                iconRight={ArrowRight}
                onClick={() => {
                  trackEvent("onboarding_own_data_clicked");
                  setOwn(true);
                  go(wizard.steps[0]!.id);
                }}
              >
                Set up here
              </Button>
            </section>
          </div>
          <p className="on-start-sample">
            <Link
              to={paths.sampleTour()}
              onClick={() => trackEvent("sample_clicked")}
            >
              Or look around a sample company first
            </Link>
          </p>
          {connecting}
        </Page>
      </SignInReturn.Provider>
    );
  return (
    <SignInReturn.Provider value={BACK}>
      <Page width="narrow">
        <PageHeader
          breadcrumbs={crumbs}
          title={title}
          subtitle="Tell Foundry about your company, choose your data and say what you want to understand. Its agent drafts the ontology, loads it into a Helix database and emails you when it’s ready to ask."
        />
        {connecting}
        {sending && (
          <EventSourceSendDialog
            connectorId={sending}
            onClose={() => setSending(null)}
          />
        )}
        {redrafting && (
          <ConfirmDialog
            title="Write the goal again?"
            description="Foundry replaces the goal you edited with a new one written from your answers."
            confirmLabel="Write it again"
            onConfirm={() => {
              setRedrafting(false);
              writeGoal(false);
            }}
            onCancel={() => setRedrafting(false)}
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
                  {s.id === "review" &&
                    i === at &&
                    s.state.kind === "loading" &&
                    readFailed && (
                      <p className="hf-form-error on-review-error" role="alert">
                        {sentence(
                          `Couldn’t read this workspace’s databases, which creating it needs: ${
                            readFailed.message || "try again"
                          }`,
                        )}{" "}
                        <Button
                          size="sm"
                          icon={RotateCcw}
                          onClick={() => {
                            void databases.refetch();
                            void ontologies.refetch();
                          }}
                        >
                          Retry
                        </Button>
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
                                  setDatabase({
                                    kind: "existing",
                                    tenantId: "",
                                  });
                                  setAdvanced(true);
                                  setError(null);
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
                          ? sentence(message)
                          : "Couldn’t create the ontology."}
                      </p>
                    ))}
                  <div className="hf-form-actions">
                    {wizard.back ? (
                      <Button onClick={() => go(wizard.back!)}>Back</Button>
                    ) : onboarding ? (
                      <Button
                        onClick={() => {
                          setOwn(false);
                          setParams({});
                        }}
                      >
                        Back
                      </Button>
                    ) : (
                      <Button onClick={cancel}>Cancel</Button>
                    )}
                    {wizard.next ? (
                      <Button
                        variant="primary"
                        iconRight={ArrowRight}
                        disabled={
                          (drafting && s.id === "goal") ||
                          s.state.kind === "blocked" ||
                          s.state.kind === "loading"
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
                        disabled={!wizard.request || drafting}
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
    </SignInReturn.Provider>
  );
}

const NEW_PROJECT = "__new";

/**
 * Advanced's database: a new one on the shared cluster (`pick` null: named
 * as the ontology, in the workspace's first project or a new one named as
 * the workspace, newDatabase), its project and name editable, or a live
 * database of the workspace's not used by another ontology (an existing
 * pick with no tenant until one is chosen).
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
  const shown = pick?.kind ?? "new";
  // The new database's fields, as typed or as they start.
  const fresh =
    pick?.kind === "new" ? pick : newDatabase(ontology, name, projects);
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
          onChange={(v) =>
            onPick(v === "new" ? null : { kind: "existing", tenantId: "" })
          }
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
                      onChange={() =>
                        onPick({ kind: "existing", tenantId: c.tenantId })
                      }
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
              <Button onClick={() => onPick(null)}>Create a database</Button>
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
