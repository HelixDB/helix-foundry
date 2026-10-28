import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  useQuery,
  useQueryClient,
  type QueryClient,
} from "@tanstack/react-query";
import {
  CircleCheck,
  FlaskConical,
  LoaderCircle,
  RotateCcw,
} from "lucide-react";
import { Button } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import {
  createDocumentSet,
  documentQueries,
  documentSetQueries,
  documentsChanged,
  uploadDocument,
} from "../documents/documents-api";
import { fileRead } from "../documents/model";
import { AiGate, useAiConfigured } from "../workspace/ai-gate";
import { Callout } from "./parts";
import { SAMPLE, SAMPLE_TOUR } from "./sample";

// "Try it on a sample company", on the first ontology's wizard. Where the
// sample's database runs (SAMPLE_TOUR: a local install, or a stamp with
// its own), it opens the sample's own page (./sample-tour.tsx), which
// connects Kestrel's database and shows each step. Elsewhere, one click
// uploads the sample's files (./sample.ts) into its set, waits until Foundry has read
// its tables and PDFs, so a draft started next leaves none out, then hands
// the sets they are in to the wizard (`onReady`), which fills in what is
// still empty and opens its review; once the wizard drafts from the
// sample's set (`chosen`), the card says so. Either way, uploads need an AI
// provider: until one is set up, it says that instead.

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
    sets.find((s) => !s.connector && s.name === SAMPLE.set.name) ??
    (await createDocumentSet(ws, SAMPLE.set)).set;
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

type Run =
  | { step: "idle" }
  | { step: "adding"; done: number }
  | { step: "reading"; ids: string[]; setIds: string[] }
  | { step: "failed"; error: string };

export function TrySample({
  chosen,
  onReady,
}: {
  chosen: boolean;
  onReady: (setIds: string[]) => void;
}) {
  const { id } = useWorkspace(),
    qc = useQueryClient(),
    navigate = useNavigate(),
    aiReady = useAiConfigured();
  const [run, setRun] = useState<Run>({ step: "idle" }),
    [gated, setGated] = useState(false);
  const documents = useQuery(documentQueries.workspace(id));

  // The list is read again while any document is being read.
  useEffect(() => {
    if (run.step !== "reading" || !documents.data) return;
    const read = run.ids.every((d) => {
      const doc = documents.data.find((x) => x.id === d);
      return doc && fileRead(doc).state !== "reading";
    });
    if (!read) return;
    setRun({ step: "idle" });
    onReady(run.setIds);
  }, [run, documents.data]);

  const add = async () => {
    if (aiReady === false) return setGated(true);
    if (SAMPLE_TOUR) return navigate(paths.sampleTour());
    setRun({ step: "adding", done: 0 });
    try {
      const added = await addSampleFiles(qc, id, SAMPLE.files, (done) =>
        setRun({ step: "adding", done }),
      );
      setRun({ step: "reading", ...added });
    } catch (e) {
      setRun({ step: "failed", error: (e as Error).message });
    }
  };

  if (gated && aiReady === false) return <AiGate action="Adding the sample" />;
  if (run.step === "failed")
    return (
      <Callout
        icon={FlaskConical}
        tone="danger"
        title="Couldn’t add the sample company"
        text={
          run.error
            ? run.error.charAt(0).toUpperCase() + run.error.slice(1) + "."
            : undefined
        }
        action={
          <Button size="sm" icon={RotateCcw} onClick={() => void add()}>
            Try again
          </Button>
        }
      />
    );
  if (run.step !== "idle")
    return (
      <Callout
        icon={LoaderCircle}
        live
        tone="muted"
        title="Adding the sample company"
        text={
          run.step === "adding"
            ? `Uploading Kestrel’s files into Data: ${run.done} of ${SAMPLE.files.length}.`
            : "Reading Kestrel’s tables and contracts."
        }
      />
    );
  if (chosen)
    return (
      <Callout
        icon={CircleCheck}
        tone="muted"
        title="Drafting from the sample company"
        text={`Kestrel’s files are in Data, in ${SAMPLE.set.name}, and chosen below. Change anything, then create the ontology.`}
      />
    );
  return (
    <Callout
      icon={FlaskConical}
      tone="muted"
      title="Try it on a sample company"
      text={
        SAMPLE_TOUR
          ? "Kestrel is a made-up software company: its customers, subscriptions, invoices and support tickets in a PostgreSQL database, and its contracts and call notes as documents. See Foundry turn them into an ontology you can ask questions of, step by step, in about a minute."
          : "Kestrel is a made-up software company: its customers, subscriptions, invoices, support tickets and usage as tables, with contracts, call notes and an incident report as documents. Foundry’s agent drafts an ontology that links them."
      }
      action={
        <Button size="sm" variant="primary" onClick={() => void add()}>
          Use the sample
        </Button>
      }
    />
  );
}
