import type {
  CreateDocumentRequest,
  CreateDocumentResponse,
  CreateDocumentSetRequest,
  DeleteDocumentSetResponse,
  DocumentBytesResponse,
  DocumentContentResponse,
  DocumentReadsResponse,
  DocumentResponse,
  DocumentSetResponse,
  DocumentUpload,
  FoundryDocument,
  ListDocumentSetsResponse,
  ListDocumentsQuery,
  ListDocumentsResponse,
  MoveDocumentRequest,
  OntologyDocumentStatus,
  UpdateDocumentRequest,
  UpdateDocumentSetRequest,
} from "../../../shared/wfe";
import { useEffect, useRef, useState } from "react";
import {
  infiniteQueryOptions,
  keepPreviousData,
  useQuery,
  type QueryClient,
} from "@tanstack/react-query";
import { api, ApiError } from "../../api";
import { POLL_MS } from "../ontology/queries";
import {
  base64Bytes,
  fileProblem,
  fileRead,
  formatOfName,
  hex,
  putFailure,
  putHeaders,
  type UploadStep,
} from "./model";

// The workspace's documents and document sets (the Data area's), and what
// an ontology reads of them. A document is an upload or a connector's (the
// text of a row of one of its tables, in a set of the connector's own):
// the workspace's list is its uploads, at most 200; a set's list pages
// through either, and `byIds` reads any by id. An upload is four steps:
// hash the file (SHA-256, in the browser), create the document in a set
// (the WFE answers with a presigned PUT, or the document the workspace
// already has), PUT the bytes straight to storage with progress, then
// complete (which says, per ontology, what reading it). The bytes never
// pass through /papi.
// Workspace queries sit under [ws, "documents"] and [ws, "document-sets"];
// what an ontology reads under the ontology's key, so its invalidations
// refresh them. A change to documents changes what every ontology reads:
// `documentsChanged` refreshes all of it. Foundry reads a table file once
// its upload completes, in seconds: the documents are read again while one
// is being read, until it is.

const enc = encodeURIComponent;
const foundry = (ws: string) => `/v1/workspaces/${enc(ws)}/foundry`;
const docs = (ws: string) => `${foundry(ws)}/documents`;
const doc = (ws: string, id: string) => `${docs(ws)}/${enc(id)}`;
const setsBase = (ws: string) => `${foundry(ws)}/document-sets`;
const ontologyDocs = (ws: string, o: string) =>
  `${foundry(ws)}/ontologies/${enc(o)}/documents`;

// A workspace holds at most 200 uploads: 4 pages of 50, all read.
async function every(path: string): Promise<FoundryDocument[]> {
  const documents: FoundryDocument[] = [];
  let token = "";
  do {
    const page = await api<ListDocumentsResponse>(
      path +
        (token
          ? `${path.includes("?") ? "&" : "?"}pageToken=${enc(token)}`
          : ""),
    );
    documents.push(...(page.documents || []));
    token = page.nextPageToken || "";
  } while (token && documents.length < 1000);
  return documents;
}

const reading = (d?: FoundryDocument) => !!d && fileRead(d).state === "reading";

/** A page of the workspace's documents list: one set's, or these. */
const listed = (ws: string, query: ListDocumentsQuery) =>
  api<ListDocumentsResponse>(
    `${docs(ws)}?${new URLSearchParams(
      "ids" in query
        ? query.ids.map((id) => ["ids", id])
        : Object.entries(query).filter(([, value]) => value),
    )}`,
  );
/** The most documents one `ids` call reads. */
export const MAX_DOCUMENT_IDS = 100;

export const documentQueries = {
  /**
   * The workspace's uploads that are not deleted, newest first: a
   * connector's documents are only in its sets' lists (`set`).
   */
  workspace: (ws: string) => ({
    queryKey: [ws, "documents"],
    queryFn: () => every(docs(ws)),
    refetchInterval: (q: { state: { data?: FoundryDocument[] } }) =>
      q.state.data?.some(reading) ? POLL_MS : false,
  }),
  /**
   * One set's documents that are not deleted, uploads or a connector's,
   * newest first (a connector's by their time at the source), 50 a page: a
   * connector's set may hold a thousand. Under the set's key, so whatever
   * refreshes the sets refreshes it.
   */
  set: (ws: string, setId: string) =>
    infiniteQueryOptions({
      queryKey: [ws, "document-sets", setId, "documents"],
      initialPageParam: "",
      queryFn: ({ pageParam }) => listed(ws, { setId, pageToken: pageParam }),
      getNextPageParam: (last) => last.nextPageToken || undefined,
    }),
  /**
   * These documents of the workspace, uploads or a connector's, deleted ones
   * too, in the order asked, each once; an id it has no document for is left
   * out. MAX_DOCUMENT_IDS a call, the calls at once.
   */
  byIds: (ws: string, ids: readonly string[]) => ({
    queryKey: [ws, "documents", { ids }],
    queryFn: async () => {
      const unique = [...new Set(ids)];
      const pages = await Promise.all(
        Array.from(
          { length: Math.ceil(unique.length / MAX_DOCUMENT_IDS) },
          (_, i) =>
            listed(ws, {
              ids: unique.slice(
                i * MAX_DOCUMENT_IDS,
                (i + 1) * MAX_DOCUMENT_IDS,
              ),
            }),
        ),
      );
      return pages.flatMap((p) => p.documents || []);
    },
  }),
  /** One document of the workspace, with the ontologies associated with it. */
  workspaceDocument: (ws: string, id: string) => ({
    queryKey: [ws, "documents", id],
    queryFn: () => api<DocumentResponse>(doc(ws, id)),
    refetchInterval: (q: { state: { data?: DocumentResponse } }) =>
      reading(q.state.data?.document) ? POLL_MS : false,
  }),
  /**
   * An uploaded text document's text, and a PDF's once Foundry has read it
   * (its pages separated by form feeds); not a Parquet file's.
   */
  content: (ws: string, id: string) => ({
    queryKey: [ws, "documents", id, "content"],
    queryFn: () => api<DocumentContentResponse>(`${doc(ws, id)}:content`),
  }),
  /** An uploaded document's bytes, of any type, decoded from base64. */
  bytes: (ws: string, id: string) => ({
    queryKey: [ws, "documents", id, "bytes"],
    queryFn: async () => {
      const r = await api<DocumentBytesResponse>(`${doc(ws, id)}:bytes`);
      return { mediaType: r.mediaType || "", bytes: base64Bytes(r.data || "") };
    },
  }),
  /**
   * What an ontology reads: its current load's documents and the uploaded
   * documents of its reading version's scope, each with the current load's
   * extraction; those the current load holds so, given a `status`.
   */
  list: (ws: string, o: string, status?: OntologyDocumentStatus) => ({
    queryKey: [ws, "ontology", o, "documents", status || ""],
    queryFn: async () => ({
      documents: await every(
        ontologyDocs(ws, o) + (status ? `?status=${enc(status)}` : ""),
      ),
    }),
  }),
  /** Any document of the workspace, with the ontology's current load's extraction. */
  document: (ws: string, o: string, id: string) => ({
    queryKey: [ws, "ontology", o, "document", id],
    queryFn: () => api<DocumentResponse>(`${ontologyDocs(ws, o)}/${enc(id)}`),
  }),
};

/**
 * Names the workspace's documents by id, else `fallback`: an upload from
 * the uploads list, any other (a connector's, or one deleted) read by id
 * (`byIds`) once it has been asked for and the uploads list, read, doesn't
 * have it, which renders the caller again. For pages that name documents
 * an ontology read (mentions, quotes, citations): any may be a connector's.
 */
export function useDocumentNames(ws: string, fallback: string) {
  const uploads = useQuery(documentQueries.workspace(ws));
  // Only grows, so a name read stays read; an id with no document stays in
  // it, asked for once.
  const [wanted, setWanted] = useState<readonly string[]>([]);
  const others = useQuery({
    ...documentQueries.byIds(ws, wanted),
    enabled: wanted.length > 0,
    placeholderData: keepPreviousData,
  });
  // The ids asked for while rendering that no list named, read after.
  const missed = useRef(new Set<string>());
  useEffect(() => {
    const more = [...missed.current].filter((id) => !wanted.includes(id));
    if (more.length) setWanted([...wanted, ...more]);
  });
  const names = new Map(
    [...(uploads.data || []), ...(others.data || [])].map((d) => [
      d.id,
      d.name || d.id,
    ]),
  );
  return (id: string) => {
    const name = names.get(id);
    if (name === undefined && uploads.data) missed.current.add(id);
    return name ?? fallback;
  };
}

export const documentSetQueries = {
  // Every set, oldest first, with how many documents each holds and the
  // ontologies that read it: one call, no pages.
  list: (ws: string) => ({
    queryKey: [ws, "document-sets"],
    queryFn: async () =>
      (await api<ListDocumentSetsResponse>(setsBase(ws))).sets || [],
  }),
};

/**
 * Refreshes what a change to the workspace's documents touches: the
 * documents, the sets (counts and readers), and every ontology (what it
 * reads, its counts and the jobs a change queued).
 */
export const documentsChanged = (qc: QueryClient, ws: string) =>
  Promise.all(
    ["documents", "document-sets", "ontology", "ontologies"].map((key) =>
      qc.invalidateQueries({ queryKey: [ws, key] }),
    ),
  );

export const renameDocument = (ws: string, id: string, name: string) =>
  api<DocumentResponse>(
    doc(ws, id),
    { name } satisfies UpdateDocumentRequest,
    "PATCH",
  );

export const deleteDocument = (ws: string, id: string) =>
  api<DocumentResponse>(doc(ws, id), undefined, "DELETE");

/** Moves a document to another set; the ontologies that read it there do. */
export const moveDocument = (ws: string, id: string, setId: string) =>
  api<DocumentReadsResponse>(`${doc(ws, id)}:move`, {
    setId,
  } satisfies MoveDocumentRequest);

export const createDocumentSet = (ws: string, body: CreateDocumentSetRequest) =>
  api<DocumentSetResponse>(setsBase(ws), body);

export const updateDocumentSet = (
  ws: string,
  id: string,
  body: UpdateDocumentSetRequest,
) => api<DocumentSetResponse>(`${setsBase(ws)}/${enc(id)}`, body, "PATCH");

export const deleteDocumentSet = (ws: string, id: string) =>
  api<DeleteDocumentSetResponse>(
    `${setsBase(ws)}/${enc(id)}`,
    undefined,
    "DELETE",
  );

/**
 * PUTs the bytes to the presigned URL with every signed header, reporting
 * progress. XMLHttpRequest, because fetch reports no upload progress.
 */
export function putBytes(
  upload: DocumentUpload,
  body: Blob,
  progress: (loaded: number, total: number) => void,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open(upload.method || "PUT", upload.url);
    for (const [name, value] of putHeaders(upload.headers))
      xhr.setRequestHeader(name, value);
    xhr.upload.onprogress = (e) =>
      progress(e.loaded, e.lengthComputable ? e.total : body.size);
    xhr.onload = () =>
      xhr.status >= 200 && xhr.status < 300
        ? resolve()
        : reject(new Error(putFailure(xhr.status, xhr.responseText || "")));
    xhr.onerror = () => reject(new Error(putFailure(0, "")));
    xhr.send(body);
  });
}

/**
 * Uploads one file into a set of the workspace, reporting each step.
 * `clientToken` makes a retry the same upload: the WFE answers with the same
 * document and a fresh URL. Bytes the workspace already has return its
 * document (`existing`), in whatever set it is: with nothing to upload once
 * its bytes are in, else with a URL this upload PUTs to and completes, the
 * document staying the other upload's. On a `retry` the document found by
 * the token is this upload's own.
 */
export async function uploadDocument(
  ws: string,
  file: File,
  setId: string,
  clientToken: string,
  report: (step: UploadStep) => void,
  retry = false,
): Promise<UploadStep> {
  const end = (step: UploadStep) => (report(step), step);
  const problem = fileProblem(file);
  if (problem) return end({ step: "failed", error: problem, retry: false });
  try {
    report({ step: "checking" });
    const bytes = await file.arrayBuffer();
    const sha256 = hex(await crypto.subtle.digest("SHA-256", bytes));
    const created = await api<CreateDocumentResponse>(docs(ws), {
      // The WFE refuses a name with spaces around it.
      name: file.name.trim(),
      mediaType: formatOfName(file.name.trim())!.mediaType,
      sizeBytes: String(file.size),
      sha256,
      clientToken,
      setId,
    } satisfies CreateDocumentRequest);
    // Another upload's document, unless this is this card's retry.
    const theirs = created.existing === true && !retry;
    if (!created.upload)
      return end({
        step: "done",
        document: created.document,
        result: { kind: theirs ? "existing" : "finished" },
      });
    report({ step: "uploading", loaded: 0, total: file.size });
    await putBytes(created.upload, new Blob([bytes]), (loaded, total) =>
      report({ step: "uploading", loaded, total }),
    );
    report({ step: "completing" });
    const done = await api<DocumentReadsResponse>(
      `${doc(ws, created.document.id)}:complete`,
      {},
    );
    return end({
      step: "done",
      document: done.document,
      result: theirs
        ? { kind: "existing" }
        : { kind: "uploaded", reads: done.reads || [] },
    });
  } catch (e) {
    // INVALID_ARGUMENT refuses the file itself: trying again can't help.
    return end({
      step: "failed",
      error: (e as Error).message,
      retry: !(e instanceof ApiError && e.code === 3),
    });
  }
}
