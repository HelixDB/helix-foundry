import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { FoundryWorkflowAccess } from "@/shared/wfe";
import {
  MAX_ENTITIES,
  VERDICTS,
  accessTitle,
  assessmentOf,
  suggestionGoal,
  verdictsText,
  accessView,
  countsText,
  preview,
  readKinds,
  readOutcomes,
  readText,
  resultEntities,
  runDuration,
  workflowPrompt,
  type AccessView,
} from "@/web/areas/workflows/model";
import { contractErrors } from "../../mock/agent-contract";
import { ok, type MockReply, type MockRequest } from "../../mock/http";
import { routes } from "../../mock/routes";
import { sendPieces } from "../../mock/routes/event-sources";
import {
  ASSESSMENT_USD_MICROS,
  WORKFLOW_CAPS,
  mockAssessment,
  tracked,
} from "../../mock/routes/workflows";
import { config, db, resetState } from "../../mock/state";

// A run's reads and results are the engine's JSON, read without trust; the
// mock serves workflows and records a run's calls as the backend does
// (api/workflows.go).

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
const W = `${O}/workflows`;

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-19T12:00:00Z"));
  resetState();
});
afterEach(() => vi.useRealTimers());

const result = (...results: unknown[]) => JSON.stringify({ results });
const read = (resultJson?: string, readsJson = '[{"entity":{}}]') =>
  accessView({
    seq: 1,
    read: {
      loadId: "l1",
      readsJson,
      ...(resultJson === undefined ? { resultOmitted: true } : { resultJson }),
    },
  }) as Extract<AccessView, { kind: "read" }>;

describe("a run's reads, read without trust", () => {
  it("names each read by its one key", () => {
    expect(readKinds('[{"entity":{}},{"query":{}}]')).toEqual([
      "entity",
      "query",
    ]);
    expect(readKinds('[{"entity":{},"query":{}}, 3, {}]')).toEqual([]);
    expect(readKinds('{"entity":{}}')).toEqual([]);
    expect(readKinds("not json")).toEqual([]);
    expect(readKinds(undefined)).toEqual([]);
  });
  it("counts the reads answered and failed", () => {
    expect(
      readOutcomes(result({ ok: {} }, { error: {} }, { ok: {} }, 7)),
    ).toEqual({
      answered: 2,
      failed: 1,
    });
    expect(readOutcomes('{"results":3}')).toBeNull();
    expect(readOutcomes("[]")).toBeNull();
    expect(readOutcomes("")).toBeNull();
  });
  it("finds the entities a result names, once each", () => {
    const json = result(
      { ok: { entity: { type: "Customer", id: "acme", name: "Acme Corp" } } },
      {
        ok: {
          rows: [
            { type: "Customer", id: "acme" },
            { type: "Invoice", id: "in_1", name: 5 },
          ],
        },
      },
      { ok: { type: "", id: "x" } },
    );
    expect(resultEntities(json)).toEqual([
      { type: "Customer", id: "acme", name: "Acme Corp" },
      { type: "Invoice", id: "in_1" },
    ]);
    expect(resultEntities("nope")).toEqual([]);
  });
  it("names at most MAX_ENTITIES, and walks no deeper than the engine nests", () => {
    const many = result(
      ...Array.from({ length: MAX_ENTITIES + 5 }, (_, i) => ({
        ok: { type: "T", id: `e${i}` },
      })),
    );
    expect(resultEntities(many)).toHaveLength(MAX_ENTITIES);
    let deep: unknown = { type: "Deep", id: "d" };
    for (let i = 0; i < 20; i++) deep = { x: deep };
    expect(resultEntities(JSON.stringify(deep))).toEqual([]);
  });
});

describe("a recorded call", () => {
  it("reads every kind, and none it doesn't know", () => {
    const r = read(result({ ok: { entity: { type: "C", id: "1" } } }));
    expect(r).toMatchObject({
      kind: "read",
      loadId: "l1",
      reads: ["entity"],
      answered: 1,
      failed: 0,
    });
    expect(r.entities).toEqual([{ type: "C", id: "1" }]);
    expect(accessTitle(r)).toBe("Read entity");
    expect(readText(r)).toBe("1 of 1 answered · 1 entity");

    const omitted = read();
    expect(omitted).toMatchObject({
      answered: null,
      failed: null,
      entities: [],
      resultJson: "",
    });
    expect(readText(omitted)).toBe("Its result wasn’t kept: it was too large.");
    expect(accessTitle(read(undefined, "[]"))).toBe("Read the graph");

    const many = read(
      result(
        { error: {} },
        ...Array.from({ length: MAX_ENTITIES }, (_, i) => ({
          ok: { type: "T", id: `e${i}` },
        })),
      ),
      "[]",
    );
    expect(readText(many)).toBe(
      `${MAX_ENTITIES} of ${MAX_ENTITIES + 1} answered · 1 failed · ${MAX_ENTITIES} entities or more`,
    );

    const q = accessView({
      seq: 2,
      question: { loadId: "l1", questionId: "q1" },
    })!;
    expect(q).toMatchObject({ kind: "question", questionId: "q1" });
    expect(accessTitle(q)).toBe("Asked a question");
    const s = accessView({
      seq: 3,
      send: { connectorId: "c", seq: "4", pieceIds: ["a", "b"] },
    })!;
    expect(s).toMatchObject({
      kind: "send",
      sendSeq: "4",
      pieceIds: ["a", "b"],
    });
    expect(accessTitle(s)).toBe("Sent 2 pieces");
    const u = accessView({ seq: 4, upload: { documentId: "d", setId: "s" } })!;
    expect(accessTitle(u)).toBe("Uploaded a document");
    expect(accessView({ seq: 5 } as FoundryWorkflowAccess)).toBeUndefined();
    // Fields a backend leaves out read as empty.
    expect(accessView({ seq: 6, send: {} })).toMatchObject({
      connectorId: "",
      sendSeq: "",
      pieceIds: [],
    });
    expect(accessView({ seq: 7, upload: {} })).toMatchObject({
      documentId: "",
      setId: "",
    });
    expect(accessView({ seq: 8, question: {} })).toMatchObject({
      loadId: "",
      questionId: "",
    });
    expect(accessView({ seq: 9, read: {} })).toMatchObject({
      loadId: "",
      readsJson: "",
      answered: null,
    });
  });
});

describe("a run in words", () => {
  it("counts what it recorded", () => {
    expect(countsText(undefined)).toBe("Nothing recorded");
    expect(countsText({ accesses: 0 })).toBe("Nothing recorded");
    expect(
      countsText({ reads: 2, questions: 1, sends: 1, pieces: 3, uploads: 1 }),
    ).toBe("2 reads · 1 question · 1 send (3 pieces) · 1 upload");
    expect(countsText({ sends: 2 })).toBe("2 sends (0 pieces)");
  });
  it("previews a request on one line", () => {
    expect(preview("  Which\n customers   renew? ")).toBe(
      "Which customers renew?",
    );
    expect(preview("x".repeat(200), 10)).toBe("xxxxxxxxx…");
    expect(preview(undefined)).toBe("");
  });
  it("says how long a finished run took", () => {
    const at = (s: number) =>
      new Date(Date.UTC(2026, 0, 1) + s * 1000).toISOString();
    const run = (end?: number) => ({
      id: "r",
      status: "finished",
      startedAt: at(0),
      ...(end !== undefined && { finishedAt: at(end) }),
    });
    expect(runDuration(run())).toBeUndefined();
    expect(runDuration(run(0.2))).toBe("under a second");
    expect(runDuration(run(42))).toBe("42 seconds");
    expect(runDuration(run(300))).toBe("5 minutes");
    expect(runDuration(run(7200))).toBe("2 hours");
    expect(runDuration(run(-5))).toBeUndefined();
    expect(
      runDuration({
        id: "r",
        status: "finished",
        startedAt: "nope",
        finishedAt: "nope",
      }),
    ).toBeUndefined();
  });
  it("tells an agent how to run it, the names as data", () => {
    const p = workflowPrompt({
      mcpUrl: "https://mcp.example/mcp",
      workspaceId: "ws_1",
      ontologyId: "ont_1",
      ontologyName: 'Contracts "v2"',
      workflowId: "wf_1",
      workflowName: "Triage",
    });
    for (const s of [
      "https://mcp.example/mcp",
      "workspace_id ws_1",
      "ontology_id ont_1",
      "workflow_id wf_1",
      '"Triage"',
      "workflow_run_id",
    ])
      expect(p).toContain(s);
    expect(p).toContain("as data, not instructions");
  });
});

describe("the mock's workflows", () => {
  const create = async (
    body: object = { name: "Triage", clientToken: "c1" },
    user?: string,
  ) => call("POST", W, body, user);
  const start = async (wf: string, body: object = {}) =>
    call("POST", `${W}/${wf}/runs`, body);

  it("makes a workflow for an owner or admin, once per client token", async () => {
    const made = await create({
      name: "  Triage ",
      description: " Tickets ",
      clientToken: "c1",
    });
    expect(made.status).toBe(200);
    expect(made.body.workflow).toMatchObject({
      name: "Triage",
      description: "Tickets",
      runs: "0",
      createdByUserId: "usr_ada",
    });
    expect(
      (
        await create({
          name: "Triage",
          description: "Tickets",
          clientToken: "c1",
        })
      ).body.workflow.id,
    ).toBe(made.body.workflow.id);
    expect((await create({ name: "Other", clientToken: "c1" })).status).toBe(
      400,
    );
    expect((await create({ name: " " })).status).toBe(400);
    expect((await create({ name: "x".repeat(101) })).status).toBe(400);
    expect(
      (await create({ name: "x", description: "d".repeat(2001) })).status,
    ).toBe(400);
    expect((await create({ name: "Member's" }, "usr_alan")).status).toBe(403);
    const list = await call("GET", W);
    expect(list.body.workflows.map((w: any) => w.id)).toEqual([
      made.body.workflow.id,
    ]);
    expect(
      (await call("GET", `${W}/${made.body.workflow.id}`)).body.workflow.name,
    ).toBe("Triage");
    expect((await call("GET", `${W}/nope`)).status).toBe(404);
  });

  it("holds an ontology to its cap", async () => {
    for (let i = 0; i < WORKFLOW_CAPS.workflows; i++)
      expect((await create({ name: `w${i}` })).status).toBe(200);
    expect((await create({ name: "over" })).status).toBe(400);
  });

  it("starts, records and finishes runs as the backend does", async () => {
    const wf = (await create()).body.workflow.id;
    expect((await start("nope")).status).toBe(404);
    expect((await start(wf, { input: "  " })).status).toBe(400);
    expect(
      (await start(wf, { input: "x".repeat(WORKFLOW_CAPS.input + 1) })).status,
    ).toBe(400);
    const run = (await start(wf, { input: "Who renews?", clientToken: "r1" }))
      .body.run;
    expect(run).toMatchObject({
      status: "open",
      input: "Who renews?",
      startedByUserId: "usr_ada",
    });
    expect(
      (await start(wf, { input: "Who renews?", clientToken: "r1" })).body.run
        .id,
    ).toBe(run.id);
    expect(
      (await start(wf, { input: "Other", clientToken: "r1" })).status,
    ).toBe(400);

    const finish = (body: object) =>
      call("POST", `${W}/${wf}/runs/${run.id}:finish`, body);
    expect((await finish({ output: " " })).status).toBe(400);
    expect((await finish({ input: "Else?", output: "Acme." })).status).toBe(
      400,
    );
    expect((await finish({ input: "  ", output: "Acme." })).status).toBe(400);
    const done = (await finish({ output: "Acme." })).body.run;
    expect(done).toMatchObject({
      status: "finished",
      output: "Acme.",
      input: "Who renews?",
    });
    expect((await finish({ output: "Acme." })).status).toBe(200);
    expect((await finish({ output: "Globex." })).status).toBe(400);
    expect(
      (await call("POST", `${W}/${wf}/runs/nope:finish`, { output: "a" }))
        .status,
    ).toBe(404);

    const bare = (await start(wf)).body.run;
    expect(
      (await call("POST", `${W}/${wf}/runs/${bare.id}:finish`, { output: "a" }))
        .status,
    ).toBe(400);
    expect(
      (
        await call("POST", `${W}/${wf}/runs/${bare.id}:finish`, {
          input: "q",
          output: "a",
        })
      ).body.run.input,
    ).toBe("q");

    const listed = (await call("GET", `${W}/${wf}/runs`)).body.runs;
    expect(listed.map((r: any) => r.id)).toEqual([bare.id, run.id]);
    expect((await call("GET", `${W}/${wf}`)).body.workflow).toMatchObject({
      runs: "2",
      finishedRuns: "2",
    });
    expect((await call("GET", `${W}/nope/runs`)).status).toBe(404);
    expect((await call("GET", `${W}/${wf}/runs/${run.id}`)).body.run.id).toBe(
      run.id,
    );
    expect((await call("GET", `${W}/other/runs/${run.id}`)).status).toBe(404);

    // Deleting the workflow takes its runs; only a manager deletes.
    expect(
      (await call("DELETE", `${W}/${wf}`, undefined, "usr_alan")).status,
    ).toBe(403);
    expect((await call("DELETE", `${W}/${wf}`)).status).toBe(200);
    expect((await call("GET", `${W}/${wf}/runs/${run.id}`)).status).toBe(404);
    expect((await call("DELETE", `${W}/${wf}`)).status).toBe(404);
  });

  it("records a call under the run its body names, checked first", async () => {
    const wf = (await create()).body.workflow.id;
    const run = (await start(wf, { input: "q" })).body.run;
    // A grantee holding part of the ontology: Eve, who records only under
    // her own runs.
    const req = (body: object, grantee = false): MockRequest =>
      ({
        method: "POST",
        path: "/",
        params: { workspaceId: "ws_acme", ontologyId: "ont_acme_contracts" },
        query: new URLSearchParams(),
        body,
        principal: { user: db.users.get("usr_ada")!, sessionId: "s" },
        ...(grantee && {
          grantee: {
            principal: "KIND_USER:usr_eve",
            load: { loadId: "l1", versionId: "v1", partial: true },
          },
        }),
      }) as MockRequest;
    let handled = 0;
    const handle =
      (body: unknown = { resultJson: '{"results":[]}' }) =>
      () => {
        handled++;
        return ok(body);
      };
    const asRead = (body: unknown) => ({
      read: {
        loadId: "l1",
        readsJson: "[]",
        resultJson: (body as { resultJson: string }).resultJson,
      },
    });

    // No run: the call alone.
    expect(
      (await tracked(req({}), "ont_acme_contracts", handle(), asRead)).status,
    ).toBe(200);
    for (const [body, ontology, grantee, status] of [
      [{ workflowRunId: "nope" }, "ont_acme_contracts", false, 404],
      [{ workflowRunId: run.id }, "ont_other", false, 404],
      [{ workflowRunId: run.id }, "ont_acme_contracts", true, 404],
    ] as const) {
      expect(
        (await tracked(req(body, grantee), ontology, handle(), asRead)).status,
      ).toBe(status);
    }
    expect(handled).toBe(1);

    const ok1 = await tracked(
      req({ workflowRunId: run.id }),
      "ont_acme_contracts",
      handle(),
      asRead,
    );
    expect(ok1.status).toBe(200);
    const big = {
      resultJson: JSON.stringify({ pad: "x".repeat(WORKFLOW_CAPS.result) }),
    };
    await tracked(
      req({ workflowRunId: run.id }),
      "ont_acme_contracts",
      handle(big),
      asRead,
    );
    // A failed call records nothing.
    await tracked(
      req({ workflowRunId: run.id }),
      "ont_acme_contracts",
      () => ({ status: 400, body: {} }),
      asRead,
    );
    const question = { question: { loadId: "l1", questionId: "q1" } };
    for (let i = 0; i < 2; i++)
      await tracked(
        req({ workflowRunId: run.id }),
        "ont_acme_contracts",
        handle(),
        () => question,
      );

    // A send of an API source of the workspace, once per send.
    const source = (
      await call("POST", "/v1/workspaces/ws_acme/foundry/connectors", {
        name: "Orders",
        config: { events: { description: "Orders" } },
      })
    ).body.connector.id;
    const send = (workflowRunId: string) =>
      sendPieces(source, {
        clientToken: "0123456789abcdef-1",
        pieces: [{ id: "o-1", data: { n: 1 } }],
        workflowRunId,
      });
    expect(send(run.id).status).toBe(200);
    expect(send(run.id).status).toBe(200);
    expect(send("nope").status).toBe(404);

    const accesses = (await call("GET", `${W}/${wf}/runs/${run.id}/accesses`))
      .body;
    expect(accesses.more).toBe(false);
    expect(
      accesses.accesses.map(
        (a: any) => Object.keys(a).filter((k) => !["seq", "at"].includes(k))[0],
      ),
    ).toEqual(["read", "read", "question", "send"]);
    expect(accesses.accesses[0].read.resultJson).toBe('{"results":[]}');
    expect(accesses.accesses[1].read).toMatchObject({ resultOmitted: true });
    expect(accesses.accesses[3].send).toMatchObject({
      connectorId: source,
      seq: "1",
      pieceIds: ["o-1"],
    });
    expect(
      (await call("GET", `${W}/${wf}/runs/${run.id}`)).body.run.counts,
    ).toMatchObject({
      accesses: 4,
      reads: 2,
      questions: 1,
      sends: 1,
      pieces: 1,
      resultsOmitted: 1,
    });
    expect(
      (
        await call("GET", `${W}/${wf}/runs/${run.id}/accesses?afterSeq=3`)
      ).body.accesses.map((a: any) => a.seq),
    ).toEqual([4]);

    // Finished, it records nothing more.
    await call("POST", `${W}/${wf}/runs/${run.id}:finish`, { output: "a" });
    expect(
      (
        await tracked(
          req({ workflowRunId: run.id }),
          "ont_acme_contracts",
          handle(),
          asRead,
        )
      ).status,
    ).toBe(400);
  });

  it("pages a long run's calls and stops at the cap", async () => {
    const wf = (await create()).body.workflow.id;
    const run = (await start(wf, { input: "q" })).body.run;
    const req = {
      method: "POST",
      path: "/",
      params: { workspaceId: "ws_acme" },
      query: new URLSearchParams(),
      body: { workflowRunId: run.id },
      principal: { user: db.users.get("usr_ada")!, sessionId: "s" },
    } as MockRequest;
    for (let i = 0; i < WORKFLOW_CAPS.accesses; i++)
      await tracked(
        req,
        undefined,
        () => ok({}),
        () => ({ upload: { documentId: `d${i}`, setId: "s" } }),
      );
    expect(
      (
        await tracked(
          req,
          undefined,
          () => ok({}),
          () => ({ upload: { documentId: "x", setId: "s" } }),
        )
      ).status,
    ).toBe(429);
    const first = (await call("GET", `${W}/${wf}/runs/${run.id}/accesses`))
      .body;
    expect(first.accesses).toHaveLength(100);
    expect(first.more).toBe(true);
  });
});

describe("a run's assessment, read without trust", () => {
  const full = {
    verdict: "helpful",
    summary: "It read Globex's renewal.",
    claims: [
      { text: "Globex renews in March.", support: "supported", calls: [1, 2] },
      { text: " ", support: "supported", calls: [] },
      { text: "Initech renews too.", support: "maybe", calls: ["x", -1, 3] },
    ],
    gaps: [
      { kind: "missing_property", what: "Customer.renewal_owner", calls: [2] },
      { kind: "later_kind", what: "Something new", calls: [] },
      { kind: "other", what: "" },
    ],
    suggestions: ["Add renewal_owner to Customer.", "", 7],
  };
  it("keeps what the contract names and drops the rest", () => {
    expect(assessmentOf(JSON.stringify(full))).toEqual({
      verdict: "helpful",
      summary: "It read Globex's renewal.",
      claims: [
        {
          text: "Globex renews in March.",
          support: "supported",
          calls: [1, 2],
        },
        { text: "Initech renews too.", support: "unsupported", calls: [3] },
      ],
      gaps: [
        {
          kind: "missing_property",
          label: "Missing property",
          what: "Customer.renewal_owner",
          calls: [2],
        },
        {
          kind: "later_kind",
          label: "Other",
          what: "Something new",
          calls: [],
        },
      ],
      suggestions: ["Add renewal_owner to Customer."],
    });
    expect(assessmentOf(JSON.stringify({ verdict: "great" }))).toBeNull();
    expect(assessmentOf("not json")).toBeNull();
    expect(assessmentOf(undefined)).toBeNull();
    expect(assessmentOf('{"verdict":"unused"}')).toEqual({
      verdict: "unused",
      summary: "",
      claims: [],
      gaps: [],
      suggestions: [],
    });
  });
  it("counts a workflow's verdicts in their order", () => {
    expect(
      verdictsText([
        { verdict: "unused", runs: "1" },
        { verdict: "essential", runs: "1200" },
        { verdict: "later", runs: "9" },
        { verdict: "helpful", runs: "0" },
      ]),
    ).toBe("1,200 essential · 1 unused");
    expect(verdictsText(undefined)).toBe("");
    expect(VERDICTS).toHaveLength(5);
  });
  it("turns a suggestion into a draft's goal within its cap", () => {
    const a = assessmentOf(JSON.stringify(full))!;
    expect(suggestionGoal("Add renewal_owner.", a.gaps, 2000)).toBe(
      "Add renewal_owner.\n\nAn agent's run lacked: missing property: Customer.renewal_owner; other: Something new.",
    );
    expect(suggestionGoal("Add renewal_owner.", [], 2000)).toBe(
      "Add renewal_owner.",
    );
    expect([...suggestionGoal("é".repeat(50), a.gaps, 10)]).toHaveLength(10);
  });
});

describe("the mock's assessments", () => {
  const wrapped = (assessment: object) => ({
    outcome: { assessed: { assessment } },
    usage: { input: 1, cache_read: 0, cache_write: 0, output: 1, usd: 0.0042 },
  });
  it("are the engine's shapes, whatever the run read", () => {
    const read = (resultJson: string, seq: number): FoundryWorkflowAccess => ({
      seq,
      read: { loadId: "l", readsJson: "[]", resultJson },
    });
    for (const accesses of [
      [],
      [read(JSON.stringify({ results: [{ ok: {} }] }), 1)],
      [
        read(
          JSON.stringify({
            results: [{ error: { code: "unknown_entity", message: "m" } }],
          }),
          1,
        ),
      ],
      [
        read(JSON.stringify({ results: [{ ok: {} }] }), 1),
        { seq: 2, question: { loadId: "l", questionId: "q" } },
      ],
    ] as FoundryWorkflowAccess[][]) {
      const assessment = mockAssessment({
        output: "Globex renews in March. It pays monthly.",
        accesses,
      });
      expect(contractErrors("assess_result", wrapped(assessment))).toEqual([]);
    }
    expect(
      (mockAssessment({ accesses: [] }) as { verdict: string }).verdict,
    ).toBe("unused");
  });

  const finishRun = async (input = "Who renews?") => {
    const wf = (await call("POST", W, { name: "Triage" })).body.workflow.id;
    const run = (await call("POST", `${W}/${wf}/runs`, { input })).body.run;
    const done = (
      await call("POST", `${W}/${wf}/runs/${run.id}:finish`, {
        output: "Globex renews in March.",
      })
    ).body.run;
    return { wf, run: done };
  };
  const later = (ms: number) => vi.setSystemTime(Date.now() + ms);

  it("queue on finish, and end with the mock agent's assessment", async () => {
    const { wf, run } = await finishRun();
    expect(run.assessment).toMatchObject({ status: "queued" });
    later(500);
    expect(
      (await call("GET", `${W}/${wf}/runs/${run.id}`)).body.run.assessment
        .status,
    ).toBe("running");
    later(2000);
    const done = (await call("GET", `${W}/${wf}/runs/${run.id}`)).body.run
      .assessment;
    expect(done).toMatchObject({
      status: "done",
      verdict: "unused",
      usdMicros: String(ASSESSMENT_USD_MICROS),
    });
    expect(JSON.parse(done.assessmentJson).verdict).toBe("unused");
    expect((await call("GET", `${W}/${wf}`)).body.workflow.verdicts).toEqual([
      { verdict: "unused", runs: "1" },
    ]);
    const usage = (await call("GET", "/v1/workspaces/ws_acme/foundry/usage"))
      .body.usage;
    expect(
      usage.tasks.find(
        (t: any) => t.task === "FOUNDRY_USAGE_TASK_WORKFLOW_ASSESSMENTS",
      ),
    ).toMatchObject({
      spentUsdMicros: String(ASSESSMENT_USD_MICROS),
      capUsdMicros: "20000000",
    });

    // Asked for again by a manager, once it ended; spend adds up.
    expect(
      (await call("POST", `${W}/${wf}/runs/${run.id}:assess`, {}, "usr_alan"))
        .status,
    ).toBe(403);
    const again = await call("POST", `${W}/${wf}/runs/${run.id}:assess`, {});
    expect(again.body.run.assessment.status).toBe("queued");
    expect(
      (await call("POST", `${W}/${wf}/runs/${run.id}:assess`, {})).status,
    ).toBe(400);
    later(2000);
    expect(
      (await call("GET", `${W}/${wf}/runs/${run.id}`)).body.run.assessment
        .usdMicros,
    ).toBe(String(2 * ASSESSMENT_USD_MICROS));
  });

  it("fail, skip, or aren't there, as the knobs say", async () => {
    Object.assign(config, { assessOutcome: "fail" });
    let { wf, run } = await finishRun();
    later(2000);
    expect(
      (await call("GET", `${W}/${wf}/runs/${run.id}`)).body.run.assessment,
    ).toMatchObject({
      status: "failed",
      error: "the model declined to assess this run",
    });
    Object.assign(config, { assessOutcome: "skip" });
    ({ wf, run } = await finishRun("Another?"));
    later(2000);
    expect(
      (await call("GET", `${W}/${wf}/runs/${run.id}`)).body.run.assessment
        .status,
    ).toBe("skipped");
    Object.assign(config, { assessEnabled: false });
    ({ wf, run } = await finishRun("A third?"));
    expect(run.assessment).toBeUndefined();
    expect(
      (await call("POST", `${W}/${wf}/runs/${run.id}:assess`, {})).status,
    ).toBe(400);
  });

  it("aren't asked for of an open run", async () => {
    const wf = (await call("POST", W, { name: "Triage" })).body.workflow.id;
    const run = (await call("POST", `${W}/${wf}/runs`, { input: "q" })).body
      .run;
    expect(
      (await call("POST", `${W}/${wf}/runs/${run.id}:assess`, {})).status,
    ).toBe(400);
    expect((await call("POST", `${W}/${wf}/runs/nope:assess`, {})).status).toBe(
      404,
    );
  });
});
