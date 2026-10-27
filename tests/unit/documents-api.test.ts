import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// An upload's failure: Retry only when trying the same upload again can
// succeed, not when the WFE refuses the file itself (INVALID_ARGUMENT). And
// its end: the workspace's document already there, or what each ontology
// that reads its set did with it.

let reply: Response;
beforeEach(() => {
  vi.resetModules();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) =>
      url === "/papi/csrf"
        ? Response.json({ token: "t", expiresInSeconds: 3600 })
        : reply.clone(),
    ),
  );
});
afterEach(() => vi.unstubAllGlobals());

const upload = async (
  retry = false,
  file = new File(["# Notes\n"], "notes.md"),
) => {
  const { uploadDocument } = await import(
    "@/web/areas/documents/documents-api"
  );
  return uploadDocument("ws_1", file, "set_1", "tok", () => {}, retry);
};

describe("uploadDocument", () => {
  it("offers no retry when the WFE refuses the file for good", async () => {
    reply = Response.json(
      { code: 3, message: "client_token is required" },
      { status: 400 },
    );
    expect(await upload()).toEqual({
      step: "failed",
      error: "client_token is required",
      retry: false,
    });
  });

  it("offers a retry when the failure may pass", async () => {
    reply = Response.json(
      { code: 14, message: "documents are not available here" },
      { status: 503 },
    );
    expect(await upload()).toMatchObject({ step: "failed", retry: true });
  });

  it("refuses a file it can tell is bad before calling the WFE", async () => {
    const { uploadDocument } = await import(
      "@/web/areas/documents/documents-api"
    );
    const steps: unknown[] = [];
    const end = await uploadDocument(
      "ws_1",
      new File(["x"], "a\\b.md"),
      "set_1",
      "tok",
      (s) => steps.push(s),
    );
    expect(end).toMatchObject({ step: "failed", retry: false });
    expect(steps).toEqual([end]);
    expect(fetch).not.toHaveBeenCalled();
  });

  it("creates the document in the workspace's set, and returns one it has already", async () => {
    const document = {
      id: "doc_1",
      name: "notes.md",
      setId: "set_2",
      status: "uploaded",
    };
    reply = Response.json({ document, upload: null, existing: true });
    expect(await upload()).toEqual({
      step: "done",
      document,
      result: { kind: "existing" },
    });
    const create = vi
      .mocked(fetch)
      .mock.calls.find(([url]) => String(url).endsWith("/documents"))!;
    expect(String(create[0])).toBe(
      "/papi/v1/workspaces/ws_1/foundry/documents",
    );
    expect(JSON.parse(String(create[1]!.body))).toMatchObject({
      name: "notes.md",
      setId: "set_1",
      clientToken: "tok",
    });
    // A retry finding its own upload already done says it finished.
    expect(await upload(true)).toEqual({
      step: "done",
      document,
      result: { kind: "finished" },
    });
  });

  it("sends the media type the file's extension says", async () => {
    reply = Response.json({
      document: { id: "doc_1" },
      upload: null,
      existing: true,
    });
    for (const [name, type] of [
      ["Q3.CSV", "text/csv"],
      ["deck.pdf", "application/pdf"],
      ["events.ndjson", "application/x-ndjson"],
    ]) {
      vi.mocked(fetch).mockClear();
      await upload(false, new File(["x"], name));
      const create = vi
        .mocked(fetch)
        .mock.calls.find(([url]) => String(url).endsWith("/documents"))!;
      expect(JSON.parse(String(create[1]!.body)).mediaType, name).toBe(type);
    }
  });
});

describe("a document's bytes", () => {
  it("reads :bytes through /papi and decodes its base64", async () => {
    reply = Response.json({
      mediaType: "application/pdf",
      data: Buffer.from("%PDF-1.7").toString("base64"),
    });
    const { documentQueries } = await import(
      "@/web/areas/documents/documents-api"
    );
    const q = documentQueries.bytes("ws_1", "doc 1");
    expect(q.queryKey).toEqual(["ws_1", "documents", "doc 1", "bytes"]);
    const read = await q.queryFn();
    expect(read.mediaType).toBe("application/pdf");
    expect(new TextDecoder().decode(read.bytes)).toBe("%PDF-1.7");
    // A GET: no CSRF token asked for, and the id escaped.
    expect(vi.mocked(fetch).mock.calls.map(([url]) => String(url))).toEqual([
      "/papi/v1/workspaces/ws_1/foundry/documents/doc%201:bytes",
    ]);
  });

  it("passes on the WFE's refusal", async () => {
    reply = Response.json(
      {
        code: 9,
        message:
          "the document is deleted; only an uploaded document can be read",
      },
      { status: 400 },
    );
    const { documentQueries } = await import(
      "@/web/areas/documents/documents-api"
    );
    await expect(documentQueries.bytes("ws_1", "d").queryFn()).rejects.toThrow(
      "only an uploaded document can be read",
    );
  });
});

describe("a connector's documents", () => {
  const urls = () =>
    vi.mocked(fetch).mock.calls.map(([url]) => decodeURI(String(url)));

  it("pages a set's documents 50 at a time, from the WFE's token", async () => {
    reply = Response.json({ documents: [{ id: "d1" }], nextPageToken: "50" });
    const { documentQueries } = await import(
      "@/web/areas/documents/documents-api"
    );
    const q = documentQueries.set("ws_1", "set 1");
    // Under the set's key: refreshing the sets refreshes it.
    expect(q.queryKey).toEqual(["ws_1", "document-sets", "set 1", "documents"]);
    // The query's context, as far as this queryFn reads it.
    const page = (pageParam: string) => q.queryFn!({ pageParam } as never);
    const first = await page("");
    await page("50");
    expect(urls()).toEqual([
      "/papi/v1/workspaces/ws_1/foundry/documents?setId=set+1",
      "/papi/v1/workspaces/ws_1/foundry/documents?setId=set+1&pageToken=50",
    ]);
    expect(q.getNextPageParam(first, [first], "", [""])).toBe("50");
    expect(
      q.getNextPageParam({ nextPageToken: "" }, [first], "", [""]),
    ).toBeUndefined();
  });

  it("reads documents by id, 100 a call, each once, in the order asked", async () => {
    vi.mocked(fetch).mockImplementation(async (url) => {
      const ids = new URL(String(url), "http://x").searchParams.getAll("ids");
      return Response.json({ documents: ids.map((id) => ({ id })) });
    });
    const { documentQueries, MAX_DOCUMENT_IDS } = await import(
      "@/web/areas/documents/documents-api"
    );
    expect(MAX_DOCUMENT_IDS).toBe(100);
    const ids = Array.from({ length: 250 }, (_, i) => `d${i}`);
    const q = documentQueries.byIds("ws_1", [...ids, "d0"]);
    expect(q.queryKey).toEqual(["ws_1", "documents", { ids: [...ids, "d0"] }]);
    expect((await q.queryFn()).map((d) => d.id)).toEqual(ids);
    const calls = urls().map(
      (u) => new URL(u, "http://x").searchParams.getAll("ids").length,
    );
    expect(calls).toEqual([100, 100, 50]);
    expect(urls()[0]).toMatch(
      /^\/papi\/v1\/workspaces\/ws_1\/foundry\/documents\?ids=d0&ids=d1&/,
    );
    // None asked for: no call.
    vi.mocked(fetch).mockClear();
    expect(await documentQueries.byIds("ws_1", []).queryFn()).toEqual([]);
    expect(fetch).not.toHaveBeenCalled();
  });
});
