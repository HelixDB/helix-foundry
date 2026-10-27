import { useEffect, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Sparkles } from "lucide-react";
import { ApiError, sentence } from "../../api";
import {
  Button,
  ButtonLink,
  EmptyState,
  Page,
  PageHeader,
  Section,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { connectorsQuery } from "../data/data-api";
import {
  documentQueries,
  documentSetQueries,
} from "../documents/documents-api";
import {
  AiRefusal,
  SettingsLinked,
  useAiConfigured,
} from "../workspace/ai-gate";
import { GoalPanel, SourcesPicker, type EventChoices } from "./generate-parts";
import {
  MAX_GOAL,
  STEP_COPY,
  generationSources,
  startingEventSources,
  startingSets,
} from "./model";
import { canManage } from "./parts";
import { generateOntology, isLive, ontologyQueries } from "./queries";
import { ontologyName, useOntology } from "./scope";

// /ontology/:ontologyId/generate: a new version of an ontology, drafted from
// what it should serve and what it reads, in one step as the new-ontology
// wizard's: the connectors whose synced tables to draft from, the API
// sources and the document sets (./generate-parts.tsx, shared with the
// wizard, which connects and uploads in place). Any will do: someone with
// only documents drafts from them, and none of the connectors chosen sends
// `noConnectors` (generationSources checks it all, as the wizard does). The
// connectors start as every synced one, or a link's (`connectors=` with
// none is none); the current version's sets are chosen (none for a first
// generation) unless a link's prefill says otherwise, once both have
// loaded: until then the sets can't be picked and nothing is generated, so
// an unloaded choice never goes out as none. They become the new version's
// scope: the agent reads each set's table files that have been read as
// tables, and the newest few of its other documents, at most 20 sets, and
// drafts the routes that say what they fill. An API source's own set is
// never among them: the API sources start the same way
// (startingEventSources), a link's (`event_sources=`, none for none), else
// those the current version's generation read, through mappings (its
// tables) or by extraction (its sets), once the connectors, the sets, the
// version and its generation have loaded. A generation that can't be read
// starts with none.

export function GeneratePage() {
  const { id, role } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    navigate = useNavigate(),
    qc = useQueryClient(),
    [params] = useSearchParams();
  const connectors = useQuery(connectorsQuery(id)),
    generations = useQuery(ontologyQueries.generations(id, o)),
    sets = useQuery(documentSetQueries.list(id)),
    documents = useQuery(documentQueries.workspace(id)),
    current = useQuery({
      ...ontologyQueries.version(id, o, ontology.currentVersionId || ""),
      enabled: !!ontology.currentVersionId,
    });
  // The chosen sets start as the prefill's, else the current version's,
  // once what they come from has loaded; null until then.
  const prefill = params.get("sets"),
    fromCurrent = prefill === null && !!ontology.currentVersionId;
  const setsLoaded = !!sets.data && (!fromCurrent || !!current.data);
  const [chosenSets, setChosenSets] = useState<Set<string> | null>(null);
  useEffect(() => {
    if (setsLoaded && chosenSets === null)
      setChosenSets(
        new Set(
          startingSets(
            prefill,
            current.data?.version?.documentSetIds,
            sets.data!,
          ),
        ),
      );
  }, [setsLoaded]);
  // The chosen API sources start as the prefill's, else those the current
  // version's generation read; null until what they come from has loaded.
  const eventsPrefill = params.get("event_sources"),
    draftedBy =
      eventsPrefill === null ? current.data?.version?.generationId : undefined,
    drafted = useQuery({
      ...ontologyQueries.generation(id, o, draftedBy || ""),
      enabled: !!draftedBy,
    });
  const eventsLoaded =
    !!connectors.data &&
    !!sets.data &&
    (eventsPrefill !== null ||
      !ontology.currentVersionId ||
      (!!current.data && (!draftedBy || !!drafted.data || !!drafted.error)));
  const [chosenEvents, setChosenEvents] = useState<EventChoices | null>(null);
  useEffect(() => {
    if (eventsLoaded && chosenEvents === null)
      setChosenEvents(
        startingEventSources(
          eventsPrefill,
          drafted.data?.generation || undefined,
          connectors.data!.connectors || [],
          sets.data!,
        ),
      );
  }, [eventsLoaded]);
  const [goal, setGoal] = useState(() => params.get("goal") || "");
  // The WFE counts characters (code points), not UTF-16 units.
  const goalChars = [...goal.trim()].length;
  // Chosen connectors; by default every synced one (or the ones asked for).
  const [chosen, setChosen] = useState<Set<string> | null>(() => {
    const wanted = params.get("connectors");
    return wanted === null ? null : new Set(wanted.split(",").filter(Boolean));
  });
  const [error, setError] = useState<unknown>(null),
    [pending, setPending] = useState(false);
  // Locally, drafting waits for an AI provider: the WFE refuses it until an
  // owner chooses one.
  const aiReady = useAiConfigured();
  const sources = generationSources(
    {
      connectorIds: chosen,
      eventSources: chosenEvents || new Map(),
      setIds: chosenSets || new Set(),
    },
    {
      connectors: connectors.data?.connectors,
      sets: sets.data,
      documents: documents.data,
    },
  );
  const live = generations.data?.generations?.find(isLive);
  const crumbs = [
    { label: "Ontologies", to: paths.ontologies() },
    { label: ontologyName(ontology), to: paths.ontology(o) },
  ];

  if (!canManage(role))
    return (
      <Page width="narrow">
        <PageHeader breadcrumbs={crumbs} title="Generate an ontology" />
        <EmptyState
          icon={Sparkles}
          title="Only owners and admins can generate"
          text="Ask an owner or admin of this workspace to draft a new version."
          action={
            <ButtonLink to={paths.ontology(o)}>Back to the ontology</ButtonLink>
          }
        />
      </Page>
    );

  const submit = async () => {
    setPending(true);
    setError(null);
    try {
      const res = await generateOntology(id, o, {
        goal: goal.trim() || undefined,
        ...sources.request,
      });
      await qc.invalidateQueries({ queryKey: [id, "ontology", o] });
      navigate(paths.generation(o, res.generation!.id));
    } catch (e) {
      setError(e);
      if (e instanceof ApiError && e.status === 409)
        void qc.invalidateQueries({
          queryKey: [id, "ontology", o, "generations"],
        });
    } finally {
      setPending(false);
    }
  };

  const state = sources.state;
  // No <form>: the pickers open the Data area's connection form, a form of
  // its own, whose submit would bubble to this one.
  return (
    <Page width="narrow">
      <PageHeader
        breadcrumbs={crumbs}
        title="Generate an ontology"
        subtitle="Foundry’s agent reads your synced tables and documents and drafts the types, links and mappings between them. The draft becomes a new version; your current one stays until you choose otherwise."
      />
      <div className="on-stack">
        <GoalPanel
          goal={goal}
          setGoal={setGoal}
          ontology={ontology.name || ""}
          chosen={chosen}
          chosenEvents={chosenEvents}
          chosenSets={chosenSets}
        />
        <Section
          id="on-sources"
          title={STEP_COPY.sources.title}
          description={STEP_COPY.sources.text}
        >
          <SourcesPicker
            chosen={chosen}
            setChosen={setChosen}
            chosenEvents={chosenEvents}
            setChosenEvents={setChosenEvents}
            chosenSets={chosenSets}
            setChosenSets={setChosenSets}
            failed={
              fromCurrent && !current.data && current.error
                ? { error: current.error, retry: () => void current.refetch() }
                : null
            }
          />
        </Section>
        <div className="on-generate-foot">
          <p className="on-muted" role="status">
            {state.kind === "ready"
              ? `${state.summary}.`
              : state.kind === "blocked"
                ? state.reason
                : "…"}
          </p>
          {state.kind === "ready" && state.note && (
            <p className="on-muted">{state.note}</p>
          )}
          {live && (
            <p className="on-muted">
              A generation is already{" "}
              {live.status === "ONTOLOGY_GENERATION_STATUS_RUNNING"
                ? "running"
                : "queued"}
              . <Link to={paths.generation(o, live.id)}>See its progress</Link>
            </p>
          )}
          <AiRefusal
            action="Generating an ontology"
            error={error}
            ready={aiReady}
          >
            {!!error && (
              <p className="hf-form-error" role="alert">
                <SettingsLinked text={sentence((error as Error).message)} />
              </p>
            )}
          </AiRefusal>
          <div className="hf-form-actions">
            <ButtonLink to={paths.ontology(o)}>Cancel</ButtonLink>
            <Button
              variant="primary"
              icon={Sparkles}
              pending={pending}
              disabled={
                state.kind !== "ready" ||
                goalChars > MAX_GOAL ||
                chosenSets === null ||
                chosenEvents === null ||
                aiReady === false
              }
              onClick={() => void submit()}
            >
              Generate
            </Button>
          </div>
        </div>
      </div>
    </Page>
  );
}
