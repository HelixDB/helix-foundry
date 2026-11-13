import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import {
  useQuery,
  useQueryClient,
  type QueryClient,
} from "@tanstack/react-query";
import { Database, FileText, RotateCcw, TriangleAlert } from "lucide-react";
import {
  Button,
  ButtonLink,
  EmptyState,
  Page,
  PageHeader,
  formatNumber,
  plural,
} from "../../kit";
import { trackEvent } from "../../analytics";
import { paths } from "../../paths";
import { canManage, useWorkspace } from "../../ui";
import {
  connectorsQuery,
  createConnector,
  syncConnector,
} from "../data/data-api";
import {
  createDocumentSet,
  documentQueries,
  documentSetQueries,
  documentsChanged,
  uploadDocument,
} from "../documents/documents-api";
import { sentence } from "../../api";
import { fileRead } from "../documents/model";
import { AiGate, useAiConfigured } from "../workspace/ai-gate";
import { AgentSetup } from "./agent-setup";
import { QuestionCard } from "./ask";
import { askSuggestions } from "./ask-model";
import { createDatabase, createProject, databasesQuery } from "./databases-api";
import { agentGraph, newDatabase, readAgentBundle, slugify } from "./model";
import { Callout, FlowStep } from "./parts";
import {
  askQuestion,
  createLoad,
  createOntology,
  generateOntology,
  isLive,
  isLiveQuestion,
  ontologyQueries,
  questionQueries,
  setCurrentVersion,
} from "./queries";
import {
  SAMPLE,
  isSampleSet,
  sampleAnswerTiming,
  sampleDatabaseConfig,
  sampleLoadTiming,
  secondsBetween,
} from "./sample";
import { InOntology } from "./scope";
import { OntologySchema } from "./suggested-ontology";
import { useSampleOntology } from "./use-sample-ontology";

// /ontology/sample: the sample company, Acme Corp, step by step, wherever its
// database runs: a local install's acme-db, or the stamp's (./try-sample.tsx
// opens it).
// Opening it runs the whole setup with no AI draft to wait for:
//  1. Acme Corp's data: its PostgreSQL database (a local stack's acme-db,
//     or the stamp's) connected and synced, and its documents uploaded and
//     read, side by side;
//  2. the goal the ontology serves;
//  3. the ontology: created on a new Helix database as the data's steps
//     start, since its gateway takes a new database on only after a few
//     minutes, which a load would otherwise wait out; then drafted from the
//     sample's ready-made draft (GenerateOntologyRequest's sample) once the
//     tables are synced and the documents uploaded, made current, and drawn;
//  4. its load: Acme Corp's records and their links written into the
//     database, then its documents read with AI;
//  5. Ask: the draft's first question, asked once the load is written, and
//     its answer; its other questions, which a click asks in Ask;
//  6. on to the user's own data.
// It first finds what an earlier visit made. An ontology with a current version
// or the sample's draft under way has its data behind it, whatever has been
// renamed, moved or deleted in Data since, so those steps are done and only
// what's left runs: reopening the page (the sample's banner links here) makes
// nothing and shows each step as it is. Before that, each step finds what it
// would make (the connector, the documents, the ontology) first, so a reload
// carries on where it was, and a failure says why with Try again. Each step
// opens as its work finishes and stays open, so the page reads top to bottom
// as what happened.

/**
 * Uploads the sample's files named `names` into its set, found by name or
 * made, as any upload is, telling `onUploaded` how many are done. The
 * workspace keeps one copy of any bytes, so adding them again finds the
 * documents it has, wherever they were moved: the result is their ids and
 * the sets they are in.
 */
export async function addSampleFiles(
  qc: QueryClient,
  ws: string,
  names: readonly string[],
  onUploaded: (done: number) => void,
): Promise<{ ids: string[]; setIds: string[] }> {
  const sets = await qc.fetchQuery({
    ...documentSetQueries.list(ws),
    staleTime: 0,
  });
  const set =
    sets.find(isSampleSet) ?? (await createDocumentSet(ws, SAMPLE.set)).set;
  let done = 0;
  const ends = await Promise.all(
    names.map(async (name) => {
      const res = await fetch(SAMPLE.base + encodeURIComponent(name));
      if (!res.ok) throw new Error(`couldn’t fetch ${name}`);
      const end = await uploadDocument(
        ws,
        new File([await res.blob()], name),
        set.id,
        crypto.randomUUID(),
        () => {},
      );
      onUploaded(++done);
      return end;
    }),
  );
  await documentsChanged(qc, ws);
  const docs = ends.map((e) => {
    if (e.step !== "done") throw new Error(e.step === "failed" ? e.error : "");
    return e.document;
  });
  return {
    ids: docs.map((d) => d.id),
    setIds: [...new Set(docs.map((d) => d.setId || set.id))],
  };
}

/**
 * The sample's ontology on a new Helix database of the workspace's, in the
 * project newDatabase picks, made if it is new. `clientToken` makes a retry
 * from the same page the same ontology.
 */
async function newSampleOntology(
  qc: QueryClient,
  ws: string,
  workspace: string,
  clientToken: string,
) {
  const pick = newDatabase(
    SAMPLE.ontology,
    workspace,
    (await qc.fetchQuery({ ...databasesQuery(ws), staleTime: 0 })).map(
      (p) => p.project,
    ),
  );
  const projectId =
    pick.project.kind === "existing"
      ? pick.project.id
      : (
          await createProject({
            workspaceId: ws,
            slug: slugify(pick.project.name, "project"),
            displayName: pick.project.name,
          })
        ).id;
  const tenant = await createDatabase({
    projectId,
    name: pick.name,
    slug: slugify(pick.name, "database"),
  });
  if (!tenant?.id) throw new Error("Helix didn’t return the new database");
  return (
    await createOntology(ws, {
      name: SAMPLE.ontology,
      tenantId: tenant.id,
      clientToken,
    })
  ).ontology;
}

// How long one step may wait on Foundry (a sync, the documents' reads, the
// sample's draft) before it gives up.
const WAIT_MS = 180_000;

/**
 * Calls `check` every second until it returns a value, for WAIT_MS, or until
 * the page is left, which ends the walk it is part of.
 */
async function until<T>(
  left: { current: boolean },
  check: () => Promise<T | undefined>,
  what: string,
) {
  for (const end = Date.now() + WAIT_MS; Date.now() < end; ) {
    if (left.current) throw new Error("the page was left");
    const value = await check();
    if (value !== undefined) return value;
    await new Promise((r) => setTimeout(r, 1000));
  }
  throw new Error(`${what} is taking longer than it should`);
}

/**
 * Runs `steps` side by side, each given `stop` for its waits (until): true
 * once the page is left (`left`) or another step has failed. The first
 * failure stops the rest at their next wait and is thrown once every step has
 * ended, so no step outlives its walk, and Try again finds what they made.
 */
async function sideBySide<T extends readonly unknown[]>(
  left: { current: boolean },
  ...steps: { [K in keyof T]: (stop: { current: boolean }) => Promise<T[K]> }
): Promise<T> {
  let failure: { error: unknown } | undefined;
  const stop = {
    get current() {
      return left.current || !!failure;
    },
  };
  const ended = await Promise.allSettled(
    steps.map((step) =>
      step(stop).catch((error: unknown) => {
        failure ??= { error };
        throw error;
      }),
    ),
  );
  if (failure) throw failure.error;
  return ended.map((e) => (e as PromiseFulfilledResult<unknown>).value) as [
    ...T,
  ];
}

// "checking" until the first reads say what an earlier visit made.
const STAGES = ["checking", "data", "ontology", "load"] as const;
type Stage = (typeof STAGES)[number];

// One walk of the steps at a time: a visit's waits for the walk of a visit just
// left, which stops at its next wait, then finds what that one made.
let walking = Promise.resolve();

export function SampleTour() {
  const { id, role, name: workspace } = useWorkspace(),
    qc = useQueryClient(),
    aiReady = useAiConfigured(),
    sample = useSampleOntology();
  const [stage, setStage] = useState<Stage>("checking"),
    [error, setError] = useState(""),
    [uploaded, setUploaded] = useState(0),
    [made, setMade] = useState<{
      connectorId?: string;
      ontologyId?: string;
      versionId?: string;
    }>({});
  const token = useRef(crypto.randomUUID()),
    started = useRef(false),
    left = useRef(false);
  useEffect(() => {
    left.current = false;
    return () => {
      left.current = true;
    };
  }, []);

  const walk = async () => {
    setError("");
    try {
      // What an earlier visit made.
      let ontology = (
        await qc.fetchQuery({ ...ontologyQueries.list(id), staleTime: 0 })
      ).ontologies?.find((o) => o.name === SAMPLE.ontology);
      let generation =
        ontology &&
        (
          await qc.fetchQuery({
            ...ontologyQueries.generations(id, ontology.id),
            staleTime: 0,
          })
        ).generations?.find(
          (g) => g.sample === SAMPLE.id && (isLive(g) || g.resultVersionId),
        );
      let versionId = ontology?.currentVersionId || "";
      const connectors = (
          await qc.fetchQuery({ ...connectorsQuery(id), staleTime: 0 })
        ).connectors,
        named =
          connectors?.find(
            (c) => c.id === generation?.tables?.[0]?.connectorId,
          ) ??
          connectors?.find(
            (c) => c.kind === "postgres" && c.name === SAMPLE.database.name,
          );
      setMade((m) => ({ ...m, connectorId: named?.id }));

      if (!versionId && !generation) {
        // 1. Acme Corp's database, connected and synced, and its documents
        // uploaded, side by side; and 3. the ontology on a new database,
        // which its gateway takes minutes to take on, so it starts first.
        setStage("data");
        const began = Date.now();
        const [made, connector, { ids, setIds }] = await sideBySide(
          left,
          async () =>
            ontology ??
            (await newSampleOntology(qc, id, workspace, token.current)),
          async (stop) => {
            const connector =
              named ??
              (
                await createConnector(id, {
                  name: SAMPLE.database.name,
                  config: await sampleDatabaseConfig(),
                })
              ).connector;
            if (!connector.lastSync)
              await syncConnector(id, connector.id, "sample");
            setMade((m) => ({ ...m, connectorId: connector.id }));
            await until(
              stop,
              async () => {
                const c = (
                  await qc.fetchQuery({ ...connectorsQuery(id), staleTime: 0 })
                ).connectors?.find((x) => x.id === connector.id);
                if (c?.lastSync?.status === "CONNECTOR_SYNC_STATUS_FAILED")
                  throw new Error(
                    c.lastSync.error || "Acme Corp’s database didn’t sync",
                  );
                return SAMPLE.database.tables.every((t) =>
                  c?.tables?.some((s) => s.name === t),
                )
                  ? true
                  : undefined;
              },
              "Syncing Acme Corp’s database",
            );
            return connector;
          },
          async () => {
            const files = await addSampleFiles(
              qc,
              id,
              SAMPLE.documents,
              setUploaded,
            );
            if (files.setIds.length !== 1)
              throw new Error(
                `Acme Corp’s documents are in ${files.setIds.length} folders: move them back into ${SAMPLE.set.name}`,
              );
            return files;
          },
        );
        ontology = made;
        // 3. The ontology's draft needs only the synced tables and the
        // documents' set, so it is drafted while the documents are read.
        generation = (
          await generateOntology(id, made.id, {
            goal: SAMPLE.goal,
            connectorIds: [connector.id],
            documentSetIds: setIds,
            sample: SAMPLE.id,
          })
        ).generation!;
        // What the sample's banner confirms it by (./use-sample-ontology.ts).
        await qc.invalidateQueries({
          queryKey: [id, "ontology", made.id, "generations"],
        });
        // 1. The documents read, which the load needs (a PDF still being
        // read would join it only after).
        await until(
          left,
          async () => {
            const docs = await qc.fetchQuery({
              ...documentQueries.workspace(id),
              staleTime: 0,
            });
            return ids.every((d) => {
              const doc = docs.find((x) => x.id === d);
              return doc && fileRead(doc).state !== "reading";
            })
              ? true
              : undefined;
          },
          "Reading Acme Corp’s documents",
        );
        trackEvent("sample_data_ready", {
          seconds: Math.round((Date.now() - began) / 1000),
        });
      }
      const o = ontology!.id;
      setMade((m) => ({ ...m, ontologyId: o }));
      if (!versionId) {
        // 3. The ontology's ready-made draft, made current.
        setStage("ontology");
        const drafting = generation!.id;
        const drafted = await until(
          left,
          async () => {
            const g = (
              await qc.fetchQuery({
                ...ontologyQueries.generation(id, o, drafting),
                staleTime: 0,
              })
            ).generation;
            if (g && !isLive(g) && !g.resultVersionId)
              throw new Error(
                g.error || "Acme Corp’s ontology couldn’t be set up",
              );
            return g?.resultVersionId ? g : undefined;
          },
          "Setting up Acme Corp’s ontology",
        );
        versionId = drafted.resultVersionId!;
        await setCurrentVersion(id, o, {
          versionId,
          expectedCurrentVersionId: "",
        });
        trackEvent("sample_ontology_ready", {
          seconds: secondsBetween(drafted.createdAt, drafted.finishedAt),
        });
      }
      // 4. Its load: the records, their links, then the documents. Read before
      // the stage moves on, so a reopened page shows every step as it is at
      // once.
      await qc.fetchQuery(ontologyQueries.version(id, o, versionId));
      const { loads } = await qc.fetchQuery({
        ...ontologyQueries.loads(id, o),
        staleTime: 0,
      });
      setMade((m) => ({ ...m, versionId }));
      setStage("load");
      if (!loads?.length)
        await createLoad(id, o, { versionId, clientToken: token.current });
      await qc.invalidateQueries({ queryKey: [id, "ontology", o, "loads"] });
      // The workspace has an ontology now: the shell offers every page.
      await qc.invalidateQueries({ queryKey: [id, "ontologies"] });
    } catch (e) {
      const message = (e as Error).message || "it stopped";
      setError(message.charAt(0).toUpperCase() + message.slice(1) + ".");
    }
  };
  const run = () => {
    walking = walking.then(walk);
  };
  useEffect(() => {
    // Adding the sample needs an AI provider; one already added shows as it
    // is without one.
    if (started.current || !canManage(role) || sample === undefined) return;
    if (!sample && aiReady !== true) return;
    started.current = true;
    run();
  }, [role, aiReady, sample]);

  // What the steps show, as Foundry has it.
  const connector = useQuery({
    ...connectorsQuery(id),
    enabled: !!made.connectorId,
    refetchInterval: stage === "data" ? 1000 : false,
  }).data?.connectors?.find((c) => c.id === made.connectorId);
  const rows = (connector?.tables || []).reduce(
    (n, t) => n + Number(t.rowCount || 0),
    0,
  );
  const o = made.ontologyId || "";
  const version = useQuery({
    ...ontologyQueries.version(id, o, made.versionId || ""),
    enabled: !!made.versionId,
  }).data?.version;
  const graph = useMemo(
    () =>
      version ? agentGraph(readAgentBundle(version.definition)) : undefined,
    [version],
  );
  const loads = useQuery({
    ...ontologyQueries.loads(id, o),
    enabled: stage === "load",
  }).data?.loads;
  // The load reads go to, else the newest.
  const load = loads?.find((l) => l.current) ?? loads?.[0];
  const job = load?.jobs?.[0];
  const loaded = load?.status === "ready" || load?.status === "partial";
  // How long the load and the first answer took, said only by a page that
  // watched them end: one opened later would count the time it was away.
  const watchedLoad = useRef(false),
    watchedAnswer = useRef(false);
  useEffect(() => {
    if (load && !loaded) watchedLoad.current = true;
  }, [load, loaded]);
  useEffect(() => {
    if (loaded)
      trackEvent(
        "sample_loaded",
        watchedLoad.current && load
          ? sampleLoadTiming(load, connector?.createdAt)
          : {},
      );
  }, [loaded]);
  const suggestions = useMemo(() => askSuggestions(version), [version]);
  // 5. Once the load is written, the draft's first question is asked, unless
  // the ontology has questions already (a reload, or Ask's): the oldest is
  // the tour's. The others are asked with a click, in Ask.
  const navigate = useNavigate();
  const questionList = useQuery({
      ...questionQueries.list(id, o),
      enabled: loaded,
    }),
    questions = questionList.data;
  const ontology = useQuery({
    ...ontologyQueries.ontology(id, o),
    enabled: loaded,
  }).data?.ontology;
  const first = questions?.questions?.at(-1);
  const [askError, setAskError] = useState(""),
    [asking, setAsking] = useState(false);
  const firstAsked = useRef(false),
    firstToken = useRef(crypto.randomUUID());
  const ask = async (question: string, clientToken: string) => {
    setAskError("");
    setAsking(true);
    try {
      const { question: q } = await askQuestion(id, o, {
        loadId: load!.id,
        question,
        clientToken,
      });
      await qc.invalidateQueries({
        queryKey: [id, "ontology", o, "questions"],
      });
      return q;
    } catch (e) {
      setAskError(sentence((e as Error).message || "it couldn’t be asked"));
    } finally {
      setAsking(false);
    }
  };
  useEffect(() => {
    // A list from the cache may be older than the question an earlier visit
    // asked: only a read that has ended says there is none.
    if (
      !loaded ||
      !load ||
      !questions ||
      questionList.isFetching ||
      !suggestions[0] ||
      firstAsked.current
    )
      return;
    firstAsked.current = true;
    if (!questions.questions?.length)
      void ask(suggestions[0], firstToken.current);
  }, [loaded, load, questions, questionList.isFetching, suggestions]);
  const answered = !!first && !isLiveQuestion(first);
  useEffect(() => {
    if (first && !answered) watchedAnswer.current = true;
  }, [first, answered]);
  useEffect(() => {
    if (answered)
      trackEvent(
        "sample_answered",
        watchedAnswer.current && first
          ? sampleAnswerTiming(first, connector?.createdAt)
          : {},
      );
  }, [answered]);
  // The banner's link opens this page on its last step. What sits above that
  // arrives when it arrives (the steps as the walk reads them, then an answer's
  // charts and names, read once near the view), so the page stays on it until
  // the reader moves: a click, a key or scrolling up, as a resting finger on a
  // trackpad sends tiny wheel events.
  const toOwnData = useLocation().hash === "#own-data";
  useEffect(() => {
    if (!toOwnData) return;
    const growing = new ResizeObserver(() =>
      document.getElementById("own-data")?.scrollIntoView({ block: "end" }),
    );
    growing.observe(document.body);
    const moved = new AbortController(),
      { signal } = moved;
    const stop = () => {
      growing.disconnect();
      moved.abort();
    };
    window.addEventListener(
      "wheel",
      (e) => {
        if (e.deltaY < -4) stop();
      },
      { signal },
    );
    for (const input of ["pointerdown", "keydown"])
      window.addEventListener(input, stop, { signal });
    return stop;
  }, [toOwnData]);
  // What the load made of each document: read, or failed and why.
  const extractions = (
    useQuery({ ...documentQueries.list(id, o), enabled: loaded }).data
      ?.documents || []
  ).map((d) => d.extraction?.status);
  const read = extractions.filter((x) => x === "extracted").length,
    unread = extractions.filter((x) => x === "failed").length;

  if (!canManage(role))
    return (
      <Page width="narrow">
        <PageHeader title="Acme Corp, a sample company" />
        <EmptyState
          icon={TriangleAlert}
          title="Only owners and admins can add the sample"
          text="Ask an owner or admin of this workspace to set it up."
        />
      </Page>
    );
  const dataDone = STAGES.indexOf(stage) > STAGES.indexOf("data");
  return (
    <Page width="wide">
      <PageHeader
        title="Acme Corp, a sample company"
        subtitle="Watch Foundry turn a company’s database and documents into an ontology you can ask questions of."
      />
      {aiReady === false && sample === null && (
        <AiGate action="Adding the sample" />
      )}
      {error && (
        <Callout
          icon={TriangleAlert}
          tone="danger"
          title="The sample stopped"
          text={error}
          action={
            <Button size="sm" icon={RotateCcw} onClick={run}>
              Try again
            </Button>
          }
        />
      )}
      <ol className="on-wizard" aria-label="Steps">
        <FlowStep
          number={1}
          title="Acme Corp’s data"
          text="A made-up company that sells scheduling software to field-service businesses."
          state={
            dataDone ? "done" : stage === "checking" ? "upcoming" : "running"
          }
          summary="A made-up company that sells scheduling software to field-service businesses."
          kept
        >
          <ul className="on-tour-sources">
            <li>
              <Database size={16} aria-hidden />
              <span>
                <strong>PostgreSQL database</strong>
                <small>
                  {connector?.tables?.length
                    ? `Customers, contacts, subscriptions, invoices, support tickets and usage: ${plural(connector.tables.length, "table")}, ${formatNumber(rows)} rows.`
                    : dataDone
                      ? "Customers, contacts, subscriptions, invoices, support tickets and usage."
                      : "Connecting to Acme Corp’s database and syncing its tables…"}
                </small>
              </span>
            </li>
            <li>
              <FileText size={16} aria-hidden />
              <span>
                <strong>Documents</strong>
                <small>
                  {dataDone
                    ? `${plural(SAMPLE.documents.length, "document")}: contracts, call notes, a support escalation, an incident report and the price list.`
                    : uploaded < SAMPLE.documents.length
                      ? `Uploading: ${uploaded} of ${SAMPLE.documents.length}…`
                      : "Reading the contracts and notes…"}
                </small>
              </span>
            </li>
          </ul>
        </FlowStep>
        <FlowStep
          number={2}
          title="The goal"
          text="What the ontology is for, in Acme Corp’s words."
          state={dataDone ? "done" : "upcoming"}
          summary="What the ontology is for, in Acme Corp’s words."
          kept
        >
          <p className="on-goal-text">{SAMPLE.goal}</p>
        </FlowStep>
        <FlowStep
          number={3}
          title="The ontology"
          text="The kinds of things in Acme Corp’s data, and how they link."
          state={graph ? "done" : stage === "ontology" ? "running" : "upcoming"}
          summary="The kinds of things in Acme Corp’s data, and how they link."
          kept
        >
          {graph ? (
            <>
              <p className="on-muted">
                To answer that goal, Foundry models Acme Corp as{" "}
                {plural(graph.nodes.length, "type")} and{" "}
                {plural(graph.edges.length, "link")} between them. The
                database’s rows become accounts, contacts, subscriptions,
                invoices, tickets and usage; the documents add the conversations
                held and the commitments made.
              </p>
              <OntologySchema
                entities={graph.nodes.map((n) => ({
                  id: n.name,
                  name: n.name,
                  description: n.description,
                  kind: "record",
                  key: n.key.join(", "),
                  rows: 0,
                  members: [],
                  properties: n.props.map((p) => p.name),
                }))}
                links={graph.edges.map((e) => ({
                  id: e.name,
                  name: e.name,
                  from: e.from,
                  to: e.to,
                  facts:
                    e.cardinality === "one" || e.cardinality === "many"
                      ? `${e.cardinality}${e.required ? " · required" : ""}`
                      : e.cardinality,
                  description: e.description || undefined,
                }))}
              />
            </>
          ) : (
            <p className="on-muted">
              Creating the ontology from Acme Corp’s ready-made draft…
            </p>
          )}
        </FlowStep>
        <FlowStep
          number={4}
          title="Loading"
          text="Acme Corp’s records and the links between them, written into a Helix database, then its documents read with AI."
          state={
            loaded
              ? "done"
              : load?.status === "failed"
                ? "current"
                : stage === "load"
                  ? "running"
                  : "upcoming"
          }
          summary="Acme Corp’s records and the links between them, written into a Helix database, then its documents read with AI."
          kept
        >
          <p className="on-muted">
            {load?.status === "failed"
              ? "The load failed."
              : loaded
                ? `Loaded: ${rows ? `${formatNumber(rows)} rows` : "Acme Corp’s rows"} are records now, each linked to its account, subscription or contact. ${unread ? `${read} of ${SAMPLE.documents.length} documents were read; ${unread} couldn’t be.` : read ? `${plural(read, "document")} read.` : ""}`
                : job?.phase === "documents"
                  ? `Reading the documents with AI: ${job.progress?.documentsDone ?? 0} of ${job.progress?.documentsTotal ?? SAMPLE.documents.length}…`
                  : job?.phase === "ingest"
                    ? "Writing the records and their links…"
                    : "Starting the load…"}{" "}
            {load && <Link to={paths.load(o, load.id)}>See the load</Link>}
          </p>
        </FlowStep>
        <FlowStep
          number={5}
          title="Ask the Analyst"
          text="Questions about Acme Corp in plain words, answered from what the load wrote, with the records and documents each answer comes from."
          state={
            answered
              ? "done"
              : loaded && (first || asking)
                ? "running"
                : loaded
                  ? "current"
                  : "upcoming"
          }
          summary="Questions about Acme Corp in plain words, answered from what the load wrote, with the records and documents each answer comes from."
          kept
        >
          {first && ontology ? (
            <div className="on-tour-answer">
              <InOntology ontology={ontology}>
                <QuestionCard question={first} budget={questions?.budget} />
              </InOntology>
            </div>
          ) : askError ? (
            <p className="hf-form-error" role="alert">
              {askError}
            </p>
          ) : (
            suggestions[0] && (
              <p className="on-muted">Asking “{suggestions[0]}”…</p>
            )
          )}
          {answered && suggestions.length > 1 && (
            <>
              <p className="on-muted">Ask another:</p>
              <div
                className="ak-suggestions on-tour-asks"
                role="group"
                aria-label="Suggested questions"
              >
                {suggestions.slice(1).map((q) => (
                  <button
                    key={q}
                    type="button"
                    className="ak-suggestion"
                    disabled={asking}
                    onClick={() =>
                      void ask(q, crypto.randomUUID()).then(
                        (asked) =>
                          asked &&
                          navigate(
                            paths.ask(o, {
                              thread: asked.threadId || asked.id,
                            }),
                          ),
                      )
                    }
                  >
                    {q}
                  </button>
                ))}
              </div>
            </>
          )}
          <span className="on-card-actions">
            <ButtonLink
              to={paths.ask(o, { new: true })}
              onClick={() => trackEvent("sample_ask_opened")}
            >
              Ask your own question
            </ButtonLink>
            <ButtonLink to={paths.explore(o)}>See the records</ButtonLink>
          </span>
        </FlowStep>
        <FlowStep
          id="own-data"
          number={6}
          title="Now connect your own data"
          text="Your databases, apps and documents, made into an ontology the same way."
          state={answered ? "current" : "upcoming"}
        >
          <p className="on-muted">
            Choose your sources and describe the questions you want to answer.
            Foundry’s agent drafts the ontology, loads it, and emails you when
            it’s ready to ask, in a few minutes.
          </p>
          <span className="on-card-actions">
            <ButtonLink
              variant="primary"
              to={paths.newOntology()}
              onClick={() => trackEvent("sample_own_data_clicked")}
            >
              Connect your own data
            </ButtonLink>
            <AgentSetup compact />
          </span>
        </FlowStep>
      </ol>
    </Page>
  );
}
