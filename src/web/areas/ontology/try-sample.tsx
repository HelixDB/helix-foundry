import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  CircleCheck,
  FlaskConical,
  LoaderCircle,
  RotateCcw,
} from "lucide-react";
import { Button } from "../../kit";
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
import { SAMPLE } from "./sample";

// "Try it on a sample company", on the first ontology's wizard. One click
// uploads the sample's files (./sample.ts) into its set, found by name or
// made, as any upload is; waits until Foundry has read its tables and PDFs,
// so a draft started next leaves none out; then hands the sets they are in
// to the wizard (`onReady`), which fills in what is still empty and opens
// its review. The workspace keeps one copy of any bytes, so adding the
// sample again finds its documents, wherever they were moved. Once the
// wizard drafts from the sample's set (`chosen`), the card says so. Locally,
// uploads need an AI provider: until one is set up, it says that instead.

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
    setRun({ step: "adding", done: 0 });
    try {
      const sets = await qc.fetchQuery({
        ...documentSetQueries.list(id),
        staleTime: 0,
      });
      const set =
        sets.find((s) => !s.connector && s.name === SAMPLE.set.name) ??
        (await createDocumentSet(id, SAMPLE.set)).set;
      const ends = await Promise.all(
        SAMPLE.files.map(async (name) => {
          const res = await fetch(SAMPLE.base + encodeURIComponent(name));
          if (!res.ok) throw new Error(`couldn’t fetch ${name}`);
          const end = await uploadDocument(
            id,
            new File([await res.blob()], name),
            set.id,
            crypto.randomUUID(),
            () => {},
          );
          setRun((r) =>
            r.step === "adding" ? { step: "adding", done: r.done + 1 } : r,
          );
          return end;
        }),
      );
      await documentsChanged(qc, id);
      const docs = ends.map((e) => {
        if (e.step !== "done")
          throw new Error(e.step === "failed" ? e.error : "");
        return e.document;
      });
      setRun({
        step: "reading",
        ids: docs.map((d) => d.id),
        setIds: [...new Set(docs.map((d) => d.setId || set.id))],
      });
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
      text="Kestrel is a made-up software company: its customers, subscriptions, invoices, support tickets and usage as tables, with contracts, call notes and an incident report as documents. Foundry’s agent drafts an ontology that links them."
      action={
        <Button size="sm" variant="primary" onClick={() => void add()}>
          Use the sample
        </Button>
      }
    />
  );
}
