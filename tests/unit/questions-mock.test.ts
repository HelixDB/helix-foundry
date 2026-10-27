import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import { Readable } from "node:stream";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import {
  EMPTY_STREAM,
  answerView,
  parseAskResult,
  queryText,
  reduceEvents,
} from "@/web/areas/ontology/ask-model";
import { drawable, visualPlan } from "@/web/areas/ontology/ask-visuals";
import type { MockReply } from "../../mock/http";
import { contractErrors } from "../../mock/agent-contract";
import { routes } from "../../mock/routes";
import { handleDocumentUpload } from "../../mock/routes/documents";
import { config, db, resetState } from "../../mock/state";

// The mock WFE's questions behave as the backend's API and ask worker
// do: a ready load only (the current one by default),
// claims oldest first, 2 running at a time, each reserving its cap from the
// workspace's own month, events by afterSeq with the question's status and
// last seq, the result settled, cancel, and refusals past 30 questions an
// hour or the month's budget, in the WFE's words. Its events and results are
// the engine's JSON: each fits ask_event.json or ask_result.json, and the
// Ask panel's parsers read them.

const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));
async function call(
  method: HttpMethod,
  path: string,
  body?: unknown,
  userId = "usr_ada",
): Promise<MockReply & { body: any }> {
  const url = new URL(path, "http://mock");
  for (const r of compiled) {
    const params = r.method === method && matchTemplate(r.c, url.pathname);
    if (!params) continue;
    return (await r.handle({
      method,
      path: url.pathname,
      params,
      query: url.searchParams,
      body,
      principal: { user: db.users.get(userId)!, sessionId: "s" },
    })) as MockReply & { body: any };
  }
  throw new Error(`no mock route for ${method} ${path}`);
}

const O = "/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts";
const Q = `${O}/questions`;
const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
);
const later = (ms: number) => vi.setSystemTime(Date.now() + ms);
const refusal = (r: MockReply & { body: any }) => [r.status, r.body.message];

/** Uploads the seeded contract through its presigned PUT. */
async function uploadContract() {
  const sha = createHash("sha256").update(CONTRACT).digest("hex");
  const { document, upload } = (
    await call("POST", "/v1/workspaces/ws_acme/foundry/documents", {
      name: "contract.md",
      mediaType: "text/markdown",
      sizeBytes: String(CONTRACT.length),
      sha256: sha,
      clientToken: "c",
      setId: "set_acme_general",
    })
  ).body;
  const req = Object.assign(Readable.from([CONTRACT]), {
    method: "PUT",
    headers: Object.fromEntries(
      Object.entries(upload.headers as Record<string, string>).map(([k, v]) => [
        k.toLowerCase(),
        v,
      ]),
    ),
  }) as unknown as IncomingMessage;
  const res = {
    setHeader: () => res,
    writeHead: () => res,
    end: () => undefined,
  } as unknown as ServerResponse;
  await handleDocumentUpload(req, res, new URL(upload.url));
  expect(
    (
      await call(
        "POST",
        `/v1/workspaces/ws_acme/foundry/documents/${document.id}:complete`,
        {},
      )
    ).status,
  ).toBe(200);
  return document.id as string;
}

/** Version 3 loaded with the contract: the load's id. */
async function loaded() {
  Object.assign(config, { loadStepMs: 50 });
  const doc = await uploadContract();
  const load = (
    await call("POST", `${O}/loads`, {
      versionId: "ver_seed_3",
      clientToken: "l",
    })
  ).body.load;
  later(10_000);
  expect((await call("GET", `${O}/loads/${load.id}`)).body.load.status).toBe(
    "ready",
  );
  return { load: load.id as string, doc };
}

/** Every event and the result of a question, held to the engine's schemas. */
async function fitsContract(id: string) {
  const events = (await call("GET", `${Q}/${id}/events`)).body.events;
  for (const e of events)
    expect(contractErrors("ask_event", JSON.parse(e.eventJson))).toEqual([]);
  // The schema leaves its objects open, so the keys are held to it too: a
  // tool_result carries what it found in a line, never what it read.
  for (const e of events.map((e: any) => JSON.parse(e.eventJson)))
    if (e.event === "tool_result")
      expect(
        Object.keys(e).filter(
          (k) =>
            !["event", "call_id", "summary", "evidence_id", "error"].includes(
              k,
            ),
        ),
      ).toEqual([]);
  expect(JSON.parse(events[0].eventJson).event).toBe("started");
  // The engine reads what the load holds before the model's first call.
  expect(events.slice(1, 3).map((e: any) => JSON.parse(e.eventJson))).toEqual([
    {
      event: "tool_call",
      call_id: "load_profile",
      tool: "load_profile",
      input: {},
    },
    {
      event: "tool_result",
      call_id: "load_profile",
      summary: expect.stringMatching(/^load_profile: \d+ of \d+ reads$/),
      evidence_id: "e1",
    },
  ]);
  expect(JSON.parse(events.at(-1).eventJson).event).toBe("finished");
  const q = (await call("GET", `${Q}/${id}`)).body.question;
  expect(contractErrors("ask_result", JSON.parse(q.resultJson))).toEqual([]);
  return q;
}

let n = 0;
const ask = (body: Record<string, unknown>) =>
  call("POST", Q, {
    question: "Who signed it?",
    clientToken: `t${++n}`,
    ...body,
  });

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-19T12:00:00Z"));
  resetState();
});
afterEach(() => vi.useRealTimers());

describe("mock questions", () => {
  it("answers from the load: events by afterSeq, a result with citations, spend settled", async () => {
    const { load, doc } = await loaded();
    Object.assign(config, { askStepMs: 100 });
    const asked = await ask({ loadId: load });
    expect(asked.status).toBe(200);
    const q = asked.body.question;
    expect(q).toMatchObject({
      status: "queued",
      threadId: q.id,
      loadId: load,
      versionId: "ver_seed_3",
      question: "Who signed it?",
    });
    // The same client token is the same question, and only that question.
    const again = (question: string) =>
      call("POST", Q, { question, loadId: load, clientToken: `t${n}` });
    expect((await again(" Who signed it? ")).body.question.id).toBe(q.id);
    expect(refusal(await again("x"))).toEqual([
      400,
      "client_token was already used for another question",
    ]);

    later(250);
    const running = (await call("GET", `${Q}/${q.id}`)).body.question;
    expect(running).toMatchObject({
      status: "running",
      reservedMicros: "500000",
    });
    const first = (await call("GET", `${Q}/${q.id}/events`)).body;
    expect(first.events.map((e: any) => e.seq)).toEqual([1, 2]);
    expect([first.status, first.lastSeq]).toEqual(["running", 2]);
    let stream = reduceEvents(EMPTY_STREAM, first);
    expect(stream.model).toBe("claude-sonnet-5-5");
    expect(stream.complete).toBe(false);
    // Mid-thread, the list holds the claim back from the month.
    expect((await call("GET", Q)).body.budget).toMatchObject({
      monthlyUsdMicros: "30000000",
      spentUsdMicros: "0",
      reservedUsdMicros: "500000",
      questionUsdMicros: "500000",
      maxSteps: 20,
      maxWallSecs: 240,
      concurrentLimit: 2,
      hourlyLimit: 30,
    });

    later(3_000);
    const rest = (
      await call("GET", `${Q}/${q.id}/events?afterSeq=${stream.lastSeq}`)
    ).body;
    expect(rest.events[0].seq).toBe(3);
    expect(rest.status).toBe("answered");
    stream = reduceEvents(stream, rest);
    expect(stream.finished).toBe(true);
    expect(stream.complete).toBe(true);
    expect(stream.lastSeq).toBe(rest.lastSeq);
    expect(
      refusal(await call("GET", `${Q}/${q.id}/events?afterSeq=-1`)),
    ).toEqual([400, "after_seq must not be negative"]);
    // The engine reads what the load holds first; a lookup the engine
    // can't run yet fails; each read is recorded as evidence; the first
    // answer is refused, the second ends the question.
    expect(
      stream.steps.map((s) => [
        s.tool,
        s.result?.kind,
        s.result?.kind === "recorded" ? s.result.evidenceId : undefined,
      ]),
    ).toEqual([
      ["load_profile", "recorded", "e1"],
      ["find_entities", "failed", undefined],
      ["find_entities", "recorded", "e2"],
      ["query", "recorded", "e3"],
      ["evidence", "recorded", "e4"],
      ["explain", "recorded", "e5"],
      ["violations", "recorded", "e6"],
      ["find_entities", "recorded", "e7"],
      ["query", "recorded", "e8"],
      ["query", "recorded", "e9"],
      ["helix_query", "recorded", "e10"],
      ["answer", "failed", undefined],
      ["answer", "ended", undefined],
    ]);
    expect(queryText(stream.steps[3]!.query!).path).toBe(
      "Contract → signed_with → Customer",
    );
    // What the load holds, from the load: its types, entities and open
    // rule breaks, in a line.
    const profile = stream.steps[0]!;
    expect(profile.input).toEqual({});
    expect(profile.result).toEqual({
      kind: "recorded",
      summary: "load_profile: 4 of 4 reads",
      evidenceId: "e1",
    });
    // The graph query the model ran itself: its code as it wrote it,
    // recorded as any read.
    const graph = stream.steps[10]!;
    expect(graph.input).toMatchObject({ limit: 50 });
    expect(JSON.stringify(graph.input)).toContain(`"string":"${load}"`);
    expect(graph.result).toEqual({
      kind: "recorded",
      summary: "helix_query: a value",
      evidenceId: "e10",
    });
    // The answer it wrote shows its visuals, as the result does.
    expect(
      (stream.steps.at(-1)!.input as { visuals: unknown }).visuals,
    ).toEqual([
      { evidence_id: "e8", after: 2, title: "Invoices by status" },
      { evidence_id: "e9", after: 3, kind: "bar", title: "Largest invoices" },
    ]);

    const done = await fitsContract(q.id);
    expect(done).toMatchObject({
      status: "answered",
      usdMicros: "73400",
      reservedMicros: "0",
      attempts: 1,
      lastSeq: stream.lastSeq,
      askedByUserId: "usr_ada",
      focusJson: "",
    });
    const result = parseAskResult(done.resultJson)!;
    if (result.status !== "answered") throw new Error(result.status);
    // Only typed queries are query records: never the graph query.
    expect(result.queries.map((r) => queryText(r.query).path)).toEqual([
      "Contract → signed_with → Customer",
      "Invoice",
      "Invoice → billed_to → Customer",
    ]);
    expect(result.queries.map((r) => r.evidence_id)).toEqual([
      "e3",
      "e8",
      "e9",
    ]);
    const view = answerView(result.answer.blocks, result.citations);
    expect(view.sources.map((s) => Object.keys(s.citation.target)[0])).toEqual([
      "entity",
      "entity",
      "fact",
      "quote",
      "row",
      "derived",
      "violation",
      "result",
      "result",
      "result",
      "result",
    ]);
    // The answer cites what the graph query counted, as a result.
    expect(view.sources[8]!.citation).toEqual({
      marker: "c11",
      evidence_id: "e10",
      target: { result: { evidence_id: "e10" } },
    });
    expect(result.answer.blocks[1]).toMatchObject({
      list: expect.arrayContaining(["The load holds 1 Contract entity [c11]"]),
    });
    // Each query's output is the load's, as the engine records it: the
    // contract's rows, its date a document's; the invoices by status; the
    // ten largest with who they bill.
    const [signed, statuses, largest] = result.queries.map((r) => r.output);
    expect(signed).toEqual({
      kind: "rows",
      rows: [
        {
          cells: [
            {
              type: "Contract",
              id: "master services agreement",
              name: "Master services agreement",
            },
            {
              type: "Customer",
              id: "cus_GLOBEX01",
              name: "Globex Corporation",
            },
            "Globex Corporation",
            { untrusted: "2026-10-01" },
          ],
          edges: [
            {
              step: 0,
              fact: {
                link: "signed_with",
                subject: { type: "Contract", id: "master services agreement" },
                object: { type: "Customer", id: "cus_GLOBEX01" },
              },
            },
          ],
        },
        expect.objectContaining({
          cells: expect.arrayContaining([
            expect.objectContaining({ id: "cus_INITECH2" }),
          ]),
        }),
      ],
      total: 2,
    });
    expect(statuses).toMatchObject({ kind: "group_count", total_groups: 5 });
    if (statuses?.kind !== "group_count") throw new Error("no groups");
    expect(statuses.groups.reduce((n, g) => n + g.count, 0)).toBe(65);
    // As the engine gives them: every group's value as a document's text.
    expect(statuses.groups.map((g) => g.value)).toEqual(
      statuses.groups.map((g) =>
        g.value === null ? null : { untrusted: expect.any(String) },
      ),
    );
    const top = statuses.groups[0]!.value as { untrusted: string };
    expect(largest).toMatchObject({ kind: "rows", total: 65 });
    if (largest?.kind !== "rows") throw new Error("no rows");
    const amounts = largest.rows.map((r) => r.cells[1] as number);
    expect(amounts).toHaveLength(10);
    expect(amounts).toEqual([...amounts].sort((a, b) => b - a));
    // Both visuals draw: the statuses as bars, the largest as bars of
    // invoices that open their pages.
    const plans = drawable(result).map((d) =>
      visualPlan(d.output, d.query, d.visual),
    );
    expect(plans.map((p) => [p.kind, p.title])).toEqual([
      ["chart", "Invoices by status"],
      ["chart", "Largest invoices"],
    ]);
    // The statuses' table doesn't call them a document's.
    if (plans[0]!.kind !== "chart") throw new Error(plans[0]!.kind);
    expect(
      plans[0]!.table.rows.filter((r) => r[0]!.kind === "text"),
    ).toHaveLength(statuses.groups.filter((g) => g.value !== null).length);
    expect(plans[0]!.table.rows.map((r) => r[0])).not.toContainEqual(
      expect.objectContaining({ untrusted: true }),
    );
    if (plans[1]!.kind !== "chart") throw new Error(plans[1]!.kind);
    expect(plans[1]!.chart.points[0]).toMatchObject({
      label: expect.any(String),
      value: amounts[0],
      entity: { type: "Invoice" },
    });
    // The paragraph before each says what it shows.
    expect(result.answer.blocks[2]).toEqual({
      para: `${statuses.groups[0]!.count} of the 65 invoices are ${top.untrusted} [c9].`,
    });
    // Lists leave the outputs out; GET of one question keeps them.
    const inList = (await call("GET", `${Q}?threadId=${q.id}`)).body
      .questions[0];
    const fromList = JSON.parse(inList.resultJson);
    expect(fromList.queries.map((r: object) => "output" in r)).toEqual([
      false,
      false,
      false,
    ]);
    expect(fromList.answer.visuals).toHaveLength(2);
    expect(contractErrors("ask_result", fromList)).toEqual([]);
    // The violation it cites is a record the load keeps.
    const broken = view.sources[6]!.citation.target as {
      violation: { constraint: string; key: string };
    };
    expect(broken.violation.constraint).toBe("invoice_under_contract");
    expect(broken.violation.key).toMatch(/^[0-9a-f]{64}$/);
    const quote = view.sources[3]!.citation.target as {
      quote: { doc_id: string; section: number; start: number; end: number };
    };
    // The span is the quote's, in UTF-8 bytes of its section.
    expect(quote.quote).toMatchObject({ doc_id: doc, section: 0 });
    const section = CONTRACT.toString().split("\n\n")[1]!;
    expect(
      Buffer.from(section)
        .subarray(quote.quote.start, quote.quote.end)
        .toString(),
    ).toContain("Globex Corporation");
    expect((await call("GET", Q)).body.budget.spentUsdMicros).toBe("73400");
  });

  it("threads follow-ups, filters by thread and pages newest first", async () => {
    const { load } = await loaded();
    Object.assign(config, { askStepMs: 10 });
    const first = (await ask({ loadId: load })).body.question;
    later(1_000);
    const next = (await ask({ loadId: load, threadId: first.id })).body
      .question;
    expect(next.threadId).toBe(first.id);
    later(1_000);
    await ask({ loadId: load });
    const thread = (await call("GET", `${Q}?threadId=${first.id}`)).body;
    expect(thread.questions.map((x: any) => x.id)).toEqual([next.id, first.id]);
    expect((await call("GET", Q)).body.questions).toHaveLength(3);
    // A thread is named by its first question only.
    for (const threadId of ["q_none", next.id])
      expect(refusal(await ask({ loadId: load, threadId }))).toEqual([
        404,
        "thread not found",
      ]);
  });

  it("asks the current load by default, about an entity as the WFE keeps it", async () => {
    expect(refusal(await ask({}))).toEqual([
      400,
      "the ontology has no load to ask yet; load a version first",
    ]);
    const { load } = await loaded();
    const q = (
      await ask({ focusJson: '{ "id": "cus_GLOBEX01", "type": "Customer" }' })
    ).body.question;
    expect(q.loadId).toBe(load);
    // Stored as the canonical EntityRef the worker sends the engine.
    expect(q.focusJson).toBe('{"type":"Customer","id":"cus_GLOBEX01"}');
    expect(
      contractErrors("ask_request#/$defs/EntityRef", JSON.parse(q.focusJson)),
    ).toEqual([]);
  });

  it("refuses what it can't ask about", async () => {
    const { load } = await loaded();
    const refused = async (body: Record<string, unknown>) =>
      refusal(await call("POST", Q, body));
    expect(await refused({ loadId: load, question: "q" })).toEqual([
      400,
      "client_token is required, at most 64 characters",
    ]);
    expect(
      await refused({
        loadId: load,
        question: "q",
        clientToken: "x".repeat(65),
      }),
    ).toEqual([400, "client_token is required, at most 64 characters"]);
    const TOO_LONG = [400, "question is required, at most 4000 characters"];
    expect(
      await refused({ loadId: load, question: "  ", clientToken: "a" }),
    ).toEqual(TOO_LONG);
    expect(
      await refused({
        loadId: load,
        question: "é".repeat(4001),
        clientToken: "a",
      }),
    ).toEqual(TOO_LONG);
    // 4,000 characters, counted as such rather than as UTF-16 units.
    expect(
      (await ask({ loadId: load, question: "😀".repeat(4000) })).status,
    ).toBe(200);
    const FOCUS = [
      400,
      'focus_json: a focus is {"type": ..., "id": ...}, both set',
    ];
    for (const focusJson of [
      '{"type":"A"}',
      '{"type":"A","id":""}',
      '{"type":"A","id":"b","key":"c"}',
      '{"type":"A","id":1}',
      '{"type":"A","id":"b"} {}',
      "null",
    ])
      expect(
        await refused({
          loadId: load,
          question: "q",
          clientToken: "b",
          focusJson,
        }),
      ).toEqual(FOCUS);
    expect(
      await refused({
        loadId: load,
        question: "q",
        clientToken: "b",
        focusJson: JSON.stringify({ type: "A", id: "b".repeat(4096) }),
      }),
    ).toEqual([400, "focus_json is at most 4096 bytes"]);
    expect(
      await refused({ loadId: "load_x", question: "q", clientToken: "c" }),
    ).toEqual([404, "load not found"]);
    // A load still being written can't be asked about.
    const writing = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_seed_3",
        clientToken: "w",
      })
    ).body.load;
    expect(
      await refused({ loadId: writing.id, question: "q", clientToken: "d" }),
    ).toEqual([400, "only a ready or partial load can be asked"]);
    // A member who can't read the database can't ask.
    Object.assign(config, { ontologyDatabaseHidden: true });
    expect((await call("GET", Q)).status).toBe(403);
  });

  it("runs 2 at a time, oldest first, and cancels what is queued or running", async () => {
    const { load } = await loaded();
    Object.assign(config, { askStepMs: 60_000 });
    const [a, b, c] = [
      (await ask({ loadId: load })).body.question,
      (await ask({ loadId: load })).body.question,
      (await ask({ loadId: load })).body.question,
    ];
    later(60_000);
    const status = async () =>
      (await call("GET", Q)).body.questions.map((x: any) => x.status);
    // Newest first: the third waits, queued, for a place.
    expect(await status()).toEqual(["queued", "running", "running"]);
    const get = async (id: string) =>
      (await call("GET", `${Q}/${id}`)).body.question;
    expect(await get(c.id)).toMatchObject({ attempts: 0, reservedMicros: "0" });
    // A running question is charged what it reserved, and frees its place.
    const cancelled = (await call("POST", `${Q}/${a.id}:cancel`, {})).body
      .question;
    expect(cancelled).toMatchObject({
      status: "cancelled",
      usdMicros: "500000",
      reservedMicros: "0",
    });
    // Cancelling it again is the same answer.
    expect(
      (await call("POST", `${Q}/${a.id}:cancel`, {})).body.question,
    ).toEqual(cancelled);
    later(1);
    expect((await get(c.id)).status).toBe("running");
    // Its events start once it was claimed, not when it was asked.
    expect((await call("GET", `${Q}/${c.id}/events`)).body.events).toHaveLength(
      1,
    );
    // A queued question is charged nothing.
    const d = (await ask({ loadId: load })).body.question;
    expect(
      (await call("POST", `${Q}/${d.id}:cancel`, {})).body.question,
    ).toMatchObject({ status: "cancelled", usdMicros: "0" });
    // One that ended otherwise can't be.
    Object.assign(config, { askStepMs: 1 });
    later(3_600_000);
    expect((await get(b.id)).status).toBe("answered");
    expect(refusal(await call("POST", `${Q}/${b.id}:cancel`, {}))).toEqual([
      400,
      "the question already ended",
    ]);
  });

  it("refuses past 30 questions an hour", async () => {
    const { load } = await loaded();
    Object.assign(config, { askStepMs: 1 });
    for (let i = 0; i < 30; i++) {
      expect((await ask({ loadId: load })).status).toBe(200);
      later(50);
    }
    expect(refusal(await ask({ loadId: load }))).toEqual([
      429,
      "the workspace has asked 30 questions in the last hour; try again later",
    ]);
    later(3_600_000);
    expect((await ask({ loadId: load })).status).toBe(200);
  });

  it("holds the month's budget: refused, waiting on reservations, failed at its claim", async () => {
    const { load } = await loaded();
    const SPENT = [
      400,
      "the workspace has spent this month's $30.00 of questions",
    ];
    // What the month spent and a question's cap pass the budget.
    Object.assign(config, { askStepMs: 10, askSpentUsdMicros: 29_600_000 });
    expect(refusal(await ask({ loadId: load }))).toEqual(SPENT);
    // Another question's reservation leaves too little: it waits, queued.
    Object.assign(config, { askStepMs: 100, askSpentUsdMicros: 29_200_000 });
    const a = (await ask({ loadId: load })).body.question;
    const b = (await ask({ loadId: load })).body.question;
    later(150);
    const get = async (id: string) =>
      (await call("GET", `${Q}/${id}`)).body.question;
    expect((await get(a.id)).status).toBe("running");
    expect((await get(b.id)).status).toBe("queued");
    expect((await call("GET", `${Q}/${b.id}/events`)).body.events).toEqual([]);
    // Once it settles, the month holds the next.
    later(10_000);
    expect((await get(a.id)).status).toBe("answered");
    expect(await get(b.id)).toMatchObject({ status: "answered", attempts: 1 });
    // One the month can no longer fit fails at its claim, in the WFE's words.
    Object.assign(config, { askSpentUsdMicros: 29_000_000 });
    const c = (await ask({ loadId: load })).body.question;
    Object.assign(config, { askSpentUsdMicros: 29_600_000 });
    later(10_000);
    expect(await get(c.id)).toMatchObject({
      status: "failed",
      error: SPENT[1],
      attempts: 0,
    });
  });

  it("ends as the knobs say: cannot answer, a budget stop, a failure", async () => {
    const { load } = await loaded();
    const end = async (outcome: string) => {
      Object.assign(config, { askStepMs: 10, askOutcome: outcome });
      const q = (await ask({ loadId: load })).body.question;
      later(10_000);
      return (await call("GET", `${Q}/${q.id}`)).body.question;
    };
    const cannot = await end("cannot_answer");
    await fitsContract(cannot.id);
    expect(cannot.status).toBe("cannot_answer");
    expect(parseAskResult(cannot.resultJson)).toMatchObject({
      status: "cannot_answer",
      gap: expect.stringContaining("signed_by"),
    });
    const stopped = await end("budget_exhausted");
    await fitsContract(stopped.id);
    expect(stopped).toMatchObject({ status: "failed", usdMicros: "500000" });
    expect(parseAskResult(stopped.resultJson)?.status).toBe("budget_exhausted");
    // The worker keeps a failed result, and words the failure itself.
    const failed = await end("failed");
    await fitsContract(failed.id);
    expect(failed.status).toBe("failed");
    expect(failed.error).toBe("the model could not be reached; ask again");
    expect(parseAskResult(failed.resultJson)).toMatchObject({
      status: "failed",
      reason: "model_failed",
    });
  });

  it("answers as a 3.10.0 engine did, without outputs or visuals, when asked", async () => {
    const { load } = await loaded();
    Object.assign(config, { askStepMs: 10, askVisuals: false });
    const q = (await ask({ loadId: load })).body.question;
    later(10_000);
    const done = await fitsContract(q.id);
    const result = JSON.parse(done.resultJson);
    expect(result.status).toBe("answered");
    expect(result.answer.visuals).toBe(undefined);
    expect(result.queries.map((r: object) => "output" in r)).toEqual([
      false,
      false,
      false,
    ]);
    // A budget stop keeps the query it ran, and its output when it can.
    Object.assign(config, { askVisuals: true, askOutcome: "budget_exhausted" });
    const stopped = (await ask({ loadId: load })).body.question;
    later(10_000);
    const kept = JSON.parse((await fitsContract(stopped.id)).resultJson);
    expect(kept.queries[0].output).toMatchObject({ kind: "rows", total: 2 });
  });
});
