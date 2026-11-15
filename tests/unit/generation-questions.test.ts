import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import type {
  OntologyGeneration,
  OntologyGenerationQuestion,
} from "@/shared/wfe";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import {
  MAX_ANSWER,
  answersRequest,
  ontologySetup,
  startingAnswer,
  startingGoal,
} from "@/web/areas/ontology/model";
import { ontologyPaths } from "@/web/areas/ontology/paths";
import {
  POLL_MS,
  WAITING_POLL_MS,
  isLive,
  isUnderWay,
  isWaiting,
  ontologyQueries,
} from "@/web/areas/ontology/queries";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import { draftsEnded } from "../../mock/routes/ontology";
import { config, db, resetState } from "../../mock/state";

// A generation the agent asked questions of (needs_input): where its goal
// and answers start (model.ts), the request answering it, how the setup
// reads it, and the mock's :answer and :cancel, which check what the
// backend checks, in its words. Each @example is checked here.

const generation = (
  status: OntologyGeneration["status"],
  more: Partial<OntologyGeneration> = {},
): OntologyGeneration => ({ id: "g", status, ...more });

describe("startingGoal", () => {
  const drafted = generation("ONTOLOGY_GENERATION_STATUS_SUCCEEDED", {
    goal: "  Trace orders  ",
  });
  it("follows its @example", () => {
    expect(startingGoal(undefined, null, drafted)).toBe("Trace orders");
    expect(startingGoal(undefined, "", drafted)).toBe("");
    expect(startingGoal("Mine", "Linked", drafted)).toBe("Mine");
    expect(startingGoal(undefined, null, undefined)).toBe("");
  });

  it("takes a link's goal over the drafting one, and a return's over both", () => {
    expect(startingGoal(undefined, "Linked", drafted)).toBe("Linked");
    expect(startingGoal("", "Linked", drafted)).toBe("");
  });

  // Try again on a generation without a goal links "goal=", so Generate
  // starts empty, as that generation did, not from the current version's.
  it("links an empty goal, and leaves the goal out only when unset", () => {
    expect(ontologyPaths.generate("o1", { goal: "" })).toBe(
      "/ontology/o1/generate?goal=",
    );
    expect(ontologyPaths.generate("o1", {})).toBe("/ontology/o1/generate");
    const link = new URL(
      "http://x" + ontologyPaths.generate("o1", { goal: "" }),
    );
    expect(
      startingGoal(undefined, link.searchParams.get("goal"), drafted),
    ).toBe("");
  });

  it("starts empty from a generation without a goal", () => {
    expect(
      startingGoal(undefined, null, generation(undefined, { goal: "" })),
    ).toBe("");
    expect(startingGoal(undefined, null, null)).toBe("");
  });
});

describe("answering the agent's questions", () => {
  const pick: OntologyGenerationQuestion = {
      id: "q1",
      options: ["A", "B"],
      defaultAnswer: "B",
      blocking: true,
    },
    free: OntologyGenerationQuestion = {
      id: "q2",
      options: [],
      defaultAnswer: "Monthly",
      blocking: true,
    },
    open: OntologyGenerationQuestion = { id: "q3" },
    must: OntologyGenerationQuestion = {
      id: "q4",
      options: ["A"],
      blocking: true,
    };

  it("follows startingAnswer's @example", () => {
    expect(startingAnswer(pick)).toEqual({ kind: "option", index: 1 });
    expect(startingAnswer(free)).toEqual({ kind: "agent" });
    expect(startingAnswer(must)).toEqual({ kind: "unset" });
    expect(startingAnswer(open)).toEqual({ kind: "agent" });
  });

  // Only a person can answer it, so nothing is chosen for them, and the
  // answers can't go until they choose.
  it("leaves a blocking question without a default unanswered", () => {
    expect(answersRequest([pick, must], {})).toEqual({
      ok: false,
      questionId: "q4",
      reason: "Choose an answer.",
    });
    expect(
      answersRequest([pick, must], { q4: { kind: "option", index: 0 } }),
    ).toEqual({
      ok: true,
      request: {
        answers: [
          { questionId: "q1", text: "B" },
          { questionId: "q4", text: "A" },
        ],
      },
    });
    expect(answersRequest([must], { q4: { kind: "agent" } })).toEqual({
      ok: true,
      request: { answers: [], useDefaults: true },
    });
  });

  // Options are the model's words: two may read the same, so a choice is
  // its index, and one past the options sends nothing.
  it("answers with the option at the index chosen", () => {
    const twice: OntologyGenerationQuestion = {
      id: "q5",
      options: ["Same", "Same"],
      blocking: true,
    };
    expect(
      answersRequest([twice], { q5: { kind: "option", index: 1 } }),
    ).toMatchObject({
      ok: true,
      request: { answers: [{ questionId: "q5", text: "Same" }] },
    });
    expect(
      answersRequest([twice], { q5: { kind: "option", index: 2 } }),
    ).toMatchObject({ ok: false, questionId: "q5" });
  });

  it("leaves a default that isn't one of the options to the agent", () => {
    expect(
      startingAnswer({ id: "q", options: ["A"], defaultAnswer: "C" }),
    ).toEqual({ kind: "agent" });
  });

  it("follows answersRequest's @example", () => {
    expect(
      answersRequest([{ id: "q1", options: ["A"] }, { id: "q2" }], {
        q1: { kind: "option", index: 0 },
        q2: { kind: "agent" },
      }),
    ).toEqual({
      ok: true,
      request: {
        answers: [{ questionId: "q1", text: "A" }],
        useDefaults: true,
      },
    });
    expect(
      answersRequest([{ id: "q1" }], { q1: { kind: "own", text: "  " } }),
    ).toEqual({
      ok: false,
      questionId: "q1",
      reason: "Write an answer, or choose another.",
    });
    expect(answersRequest([{ id: "q1", blocking: true }], {})).toEqual({
      ok: false,
      questionId: "q1",
      reason: "Choose an answer.",
    });
  });

  it("starts each question where startingAnswer does", () => {
    expect(answersRequest([pick, free, open], {})).toEqual({
      ok: true,
      request: {
        answers: [{ questionId: "q1", text: "B" }],
        useDefaults: true,
      },
    });
  });

  it("answers every question, without defaults, when none is left to the agent", () => {
    expect(
      answersRequest([pick, free], {
        q1: { kind: "option", index: 0 },
        q2: { kind: "own", text: "  Weekly, on Mondays " },
      }),
    ).toEqual({
      ok: true,
      request: {
        answers: [
          { questionId: "q1", text: "A" },
          { questionId: "q2", text: "Weekly, on Mondays" },
        ],
      },
    });
  });

  it("counts an answer's characters, not its UTF-16 units", () => {
    const at = "🙂".repeat(MAX_ANSWER),
      over = at + "🙂";
    expect(
      answersRequest([free], { q2: { kind: "own", text: at } }),
    ).toMatchObject({ ok: true });
    expect(answersRequest([free], { q2: { kind: "own", text: over } })).toEqual(
      { ok: false, questionId: "q2", reason: "1 characters too long." },
    );
  });

  it("names the first question whose answer can't be sent", () => {
    expect(
      answersRequest([pick, free], {
        q1: { kind: "own", text: "" },
        q2: { kind: "own", text: "" },
      }),
    ).toMatchObject({ ok: false, questionId: "q1" });
  });
});

describe("a generation waiting for answers", () => {
  const waiting = generation("ONTOLOGY_GENERATION_STATUS_NEEDS_INPUT");

  // Nothing runs while it waits, so it is read again slowly: for answers
  // given in another tab, or the defaults Helix takes once they are due.
  it("is polled slowly while it waits, and not once it ended", () => {
    const one = ontologyQueries.generation("ws", "o", "g").refetchInterval,
      list = ontologyQueries.generations("ws", "o").refetchInterval;
    expect(one({ state: { data: { generation: waiting } } })).toBe(
      WAITING_POLL_MS,
    );
    expect(
      one({
        state: {
          data: {
            generation: generation("ONTOLOGY_GENERATION_STATUS_RUNNING"),
          },
        },
      }),
    ).toBe(POLL_MS);
    expect(
      one({
        state: {
          data: {
            generation: generation("ONTOLOGY_GENERATION_STATUS_CANCELLED"),
          },
        },
      }),
    ).toBe(false);
    expect(list({ state: { data: { generations: [waiting] } } })).toBe(
      WAITING_POLL_MS,
    );
    expect(
      list({
        state: {
          data: {
            generations: [
              waiting,
              generation("ONTOLOGY_GENERATION_STATUS_QUEUED"),
            ],
          },
        },
      }),
    ).toBe(POLL_MS);
  });

  it("holds the ontology's one live generation", () => {
    expect(isWaiting(waiting)).toBe(true);
    expect(isUnderWay(waiting)).toBe(true);
    expect(isLive(waiting)).toBe(false);
    for (const status of [
      "ONTOLOGY_GENERATION_STATUS_QUEUED",
      "ONTOLOGY_GENERATION_STATUS_RUNNING",
    ] as const)
      expect(isUnderWay(generation(status))).toBe(true);
    for (const status of [
      "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
      "ONTOLOGY_GENERATION_STATUS_FAILED",
      "ONTOLOGY_GENERATION_STATUS_CANCELLED",
    ] as const)
      expect(isUnderWay(generation(status))).toBe(false);
    expect(isUnderWay(undefined)).toBe(false);
  });

  it("is the setup's draft step, waiting; a cancelled one leaves it to do", () => {
    const facts = (generations: OntologyGeneration[]) => ({
      ontology: { id: "o" },
      versions: [],
      generations,
      explored: false,
    });
    expect(ontologySetup(facts([waiting]))).toEqual({
      step: "draft",
      state: "waiting",
      generation: waiting,
    });
    expect(
      ontologySetup(
        facts([
          generation("ONTOLOGY_GENERATION_STATUS_CANCELLED"),
          generation("ONTOLOGY_GENERATION_STATUS_FAILED"),
        ]),
      ),
    ).toEqual({ step: "draft", state: "todo" });
    // A draft stored is still the step after, whatever waits.
    expect(
      ontologySetup({ ...facts([waiting]), versions: [{ id: "v1" }] }),
    ).toEqual({ step: "current", state: "todo", versionId: "v1" });
  });
});

// ---------------------------------------------------------------- the mock

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
const refusal = (r: MockReply & { body: any }) => [r.status, r.body.message];
const O = "/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts";

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-18T12:00:00Z"));
  resetState();
  Object.assign(config, { ontologyStepMs: 100, ontologyQuestions: true });
});
afterEach(() => vi.useRealTimers());

/** A new generation, read once the agent has asked. */
async function asked() {
  const g = (
    await call("POST", `${O}:generate`, {
      goal: "Contracts and renewals",
      connectorIds: ["conn_acme_stripe"],
    })
  ).body.generation;
  vi.setSystemTime(Date.now() + 1000);
  return (await call("GET", `${O}/generations/${g.id}`)).body
    .generation as OntologyGeneration;
}
const answer = (id: string, body: unknown, user?: string) =>
  call("POST", `${O}/generations/${id}:answer`, body, user);

describe("the mock's questions", () => {
  it("waits for answers with no draft, holding the ontology's live generation", async () => {
    const g = await asked();
    expect(g).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_NEEDS_INPUT",
      resultVersionId: "",
      askedAt: expect.any(String),
    });
    expect(Date.parse(g.answersDueAt!) - Date.parse(g.askedAt!)).toBe(
      7 * 24 * 60 * 60 * 1000,
    );
    expect(g.questions!.map((q) => [q.id, q.blocking, q.answer])).toEqual([
      ["q1", true, undefined],
      ["q2", true, undefined],
      ["q3", false, undefined],
    ]);
    // Lists carry them too.
    const listed = (await call("GET", `${O}/generations`)).body.generations[0];
    expect(listed.questions).toHaveLength(3);
    expect(
      refusal(
        await call("POST", `${O}:generate`, {
          connectorIds: ["conn_acme_stripe"],
        }),
      ),
    ).toEqual([
      409,
      "an ontology generation is already queued, running or waiting for answers: answer or cancel it first",
    ]);
  });

  it("checks answers as the backend does, in its words", async () => {
    const { id } = await asked();
    const cases: [unknown, string][] = [
      [
        { answers: [{ questionId: "q9", text: "x" }] },
        'the generation asked no question "q9"',
      ],
      [
        {
          answers: [
            { questionId: "q1", text: "x" },
            { questionId: "q1", text: "y" },
          ],
        },
        "question q1 is answered twice",
      ],
      [
        { answers: [{ questionId: "q1", text: "   " }] },
        "the answer to question q1 must be 1 to 2000 characters",
      ],
      [
        { answers: [{ questionId: "q1", text: "x".repeat(2001) }] },
        "the answer to question q1 must be 1 to 2000 characters",
      ],
      [
        { answers: [{ questionId: "q1", text: "x" }] },
        "question q2 needs an answer, or use the agent's defaults",
      ],
    ];
    for (const [body, message] of cases)
      expect(refusal(await answer(id, body))).toEqual([400, message]);
    // Nothing was kept.
    expect(
      (await call("GET", `${O}/generations/${id}`)).body.generation.status,
    ).toBe("ONTOLOGY_GENERATION_STATUS_NEEDS_INPUT");
  });

  it("is answered only by owners and admins", async () => {
    const { id } = await asked();
    expect(
      (await answer(id, { answers: [], useDefaults: true }, "usr_alan")).status,
    ).toBe(403);
  });

  it("carries on from the answers, leaving an open question open, and drafts", async () => {
    const { id } = await asked();
    const r = await answer(id, {
      answers: [
        { questionId: "q1", text: "Each person who signs in" },
        { questionId: "q2", text: " 60 days " },
      ],
    });
    expect(r.status).toBe(200);
    expect(r.body.generation).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_QUEUED",
      attempts: 0,
      answersDueAt: null,
    });
    expect(
      r.body.generation.questions.map((q: OntologyGenerationQuestion) => [
        q.id,
        q.answer,
      ]),
    ).toEqual([
      ["q1", "Each person who signs in"],
      ["q2", "60 days"],
      ["q3", undefined],
    ]);
    // Answered once: a second answer finds it no longer waiting.
    expect(
      refusal(await answer(id, { answers: [], useDefaults: true })),
    ).toEqual([400, "this generation isn't waiting for answers"]);
    vi.setSystemTime(Date.now() + 1000);
    const done = (await call("GET", `${O}/generations/${id}`)).body.generation;
    expect(done.status).toBe("ONTOLOGY_GENERATION_STATUS_SUCCEEDED");
    expect(done.resultVersionId).not.toBe("");
  });

  it("takes the agent's defaults, or leaves a question without one to the agent", async () => {
    const { id } = await asked();
    const r = await answer(id, {
      answers: [{ questionId: "q2", text: "30 days" }],
      useDefaults: true,
    });
    expect(
      r.body.generation.questions.map(
        (q: OntologyGenerationQuestion) => q.answer,
      ),
    ).toEqual([
      "The company that pays",
      "30 days",
      "No preference: decide from the data and the goal.",
    ]);
  });

  it("answers overdue questions with the defaults, as the worker does", async () => {
    const { id } = await asked();
    vi.setSystemTime(Date.now() + 7 * 24 * 60 * 60 * 1000);
    const g = (await call("GET", `${O}/generations/${id}`)).body.generation;
    expect(g.status).toBe("ONTOLOGY_GENERATION_STATUS_QUEUED");
    expect(g.questions[0].answer).toBe("The company that pays");
  });

  it("cancels a waiting or live generation, freeing the ontology, and nothing that ended", async () => {
    const { id } = await asked();
    const cancel = (g: string) =>
      call("POST", `${O}/generations/${g}:cancel`, {});
    const r = await cancel(id);
    expect(r.body.generation).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_CANCELLED",
      finishedAt: expect.any(String),
      answersDueAt: null,
    });
    expect(refusal(await cancel(id))).toEqual([
      400,
      "this generation already ended",
    ]);
    expect(refusal(await cancel("gen_nope"))).toEqual([
      404,
      "ontology generation not found",
    ]);
    // The ontology may generate again; a queued one cancels too.
    Object.assign(config, { ontologyQuestions: false });
    const next = (
      await call("POST", `${O}:generate`, {
        connectorIds: ["conn_acme_stripe"],
      })
    ).body.generation;
    expect(next.status).toBe("ONTOLOGY_GENERATION_STATUS_QUEUED");
    expect((await cancel(next.id)).body.generation.status).toBe(
      "ONTOLOGY_GENERATION_STATUS_CANCELLED",
    );
    vi.setSystemTime(Date.now() + 1000);
    expect(
      (await call("GET", `${O}/generations/${next.id}`)).body.generation,
    ).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_CANCELLED",
      resultVersionId: "",
    });
  });

  it("fires no automation's draft trigger while it waits, nor once cancelled", async () => {
    const ontologyId = O.split("/").at(-1)!;
    const before = draftsEnded(ontologyId).length;
    const { id } = await asked();
    expect(draftsEnded(ontologyId)).toHaveLength(before);
    await call("POST", `${O}/generations/${id}:cancel`, {});
    expect(draftsEnded(ontologyId).map((d) => d.id)).not.toContain(id);
  });

  it("asks nothing once a reset turns the knob off", async () => {
    resetState();
    Object.assign(config, { ontologyStepMs: 100 });
    const g = (
      await call("POST", `${O}:generate`, {
        connectorIds: ["conn_acme_stripe"],
      })
    ).body.generation;
    vi.setSystemTime(Date.now() + 1000);
    const done = (await call("GET", `${O}/generations/${g.id}`)).body
      .generation;
    expect(done.status).toBe("ONTOLOGY_GENERATION_STATUS_SUCCEEDED");
    expect(done.questions).toEqual([]);
  });
});
