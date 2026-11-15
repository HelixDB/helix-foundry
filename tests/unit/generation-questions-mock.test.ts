import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import { readQuestions } from "@/web/questions/model";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import { config, db, resetState } from "../../mock/state";
import { checkAnswers, defaultAnswer } from "../../mock/agent-questions";
import {
  ENGINE_QUESTIONS,
  wfeQuestion,
} from "../../mock/fixtures/generation-questions";

// A generation whose agent asks before it drafts (the mock's
// `ontologyQuestions`): it waits on round 1 of the engine's questions, as
// the WFE gives them, and :answer takes that round's answers as the
// backend's agentquestion.Check does, then drafts on.

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
const later = (ms: number) => vi.setSystemTime(Date.now() + ms);

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-14T12:00:00Z"));
  resetState();
  Object.assign(config, { ontologyQuestions: true, ontologyStepMs: 300 });
});
afterEach(() => vi.useRealTimers());

async function waiting() {
  const g = await call("POST", `${O}:generate`, {
    goal: "Renewals",
    connectorIds: [],
    documentSetIds: [],
  });
  expect(g.status).toBe(200);
  later(5_000);
  const read = await call("GET", `${O}/generations/${g.body.generation.id}`);
  return read.body.generation;
}

describe("a generation that asks", () => {
  it("waits on round 1 of the engine's questions, as the WFE gives them", async () => {
    const g = await waiting();
    expect(g.status).toBe("ONTOLOGY_GENERATION_STATUS_NEEDS_INPUT");
    expect(g.questionRound).toBe(1);
    expect(g.questions).toEqual(ENGINE_QUESTIONS.map(wfeQuestion));
    // Every one reads as the flow asks it.
    expect(readQuestions(g.questions)?.map((q) => q.ask.kind)).toEqual([
      "choice",
      "choice",
      "free",
      "choice",
    ]);
  });

  it("takes answers to its round only, checked as the backend checks them", async () => {
    const g = await waiting();
    const answer = (body: object) =>
      call("POST", `${O}/generations/${g.id}:answer`, body);
    const stale = await answer({ answers: [], useDefaults: true, round: 2 });
    expect(stale.status).toBe(400);
    expect(stale.body.message).toContain("waits on round 1");
    const bad = await answer({
      answers: [{ questionId: "Q1", options: ["Nope"] }],
      round: 1,
    });
    expect(bad.status).toBe(400);
    expect(bad.body.message).toBe('question Q1 offers no option "Nope"');
    const blocking = await answer({
      answers: [{ questionId: "Q1", options: ["The company that pays"] }],
      round: 1,
    });
    expect(blocking.body.message).toBe("question Q2 needs an answer");
    const ok = await answer({
      answers: [
        { questionId: "Q2", options: ["Falling usage"], text: "and seats" },
      ],
      useDefaults: true,
      round: 1,
    });
    expect(ok.status).toBe(200);
    expect(ok.body.generation.status).toBe("ONTOLOGY_GENERATION_STATUS_QUEUED");
    expect(
      ok.body.generation.questions.map((q: { answer: unknown }) => q.answer),
    ).toEqual([
      { questionId: "Q1", options: ["The company that pays"] },
      { questionId: "Q2", options: ["Falling usage"], text: "and seats" },
      { questionId: "Q3", options: [], text: "90 days" },
      {
        questionId: "Q4",
        options: [],
        text: "No preference: decide from the data and the goal.",
      },
    ]);
    // Answered, it no longer waits.
    const again = await answer({ answers: [], useDefaults: true, round: 1 });
    expect(again.status).toBe(400);
    expect(again.body.message).toBe(
      "this generation isn't waiting for answers",
    );
  });

  it("is answered for a member only by an owner or admin", async () => {
    const g = await waiting();
    const r = await call(
      "POST",
      `${O}/generations/${g.id}:answer`,
      { answers: [], useDefaults: true, round: 1 },
      "usr_alan",
    );
    expect(r.status).toBe(403);
  });
});

describe("agentquestion.Check, as the mock keeps it", () => {
  const [one, many, free, optional] = ENGINE_QUESTIONS.map(wfeQuestion);
  it("defaults a question left to the agent", () => {
    expect(defaultAnswer(one!)).toEqual({
      questionId: "Q1",
      options: ["The company that pays"],
    });
    expect(defaultAnswer(free!)).toEqual({
      questionId: "Q3",
      options: [],
      text: "90 days",
    });
    expect(defaultAnswer(optional!).text).toBe(
      "No preference: decide from the data and the goal.",
    );
  });

  it("refuses what the backend refuses, in its words", () => {
    const cases: [unknown, string][] = [
      ["x", "answers is a list of {questionId, options, text}"],
      [[1], "answers is a list of {questionId, options, text}"],
      [
        [{ questionId: "Q1", options: "A" }],
        "an answer's options are labels, and its text a string",
      ],
      [
        [{ questionId: "Q2", options: [] }],
        "question Q2 needs an option chosen or an answer in words",
      ],
      [
        [{ questionId: "Q2", options: ["Falling usage", "Falling usage"] }],
        `question Q2's option "Falling usage" is chosen twice`,
      ],
      [
        [{ questionId: "Q3", text: "x".repeat(2001) }],
        "the words answering question Q3 are 1 to 2000 characters",
      ],
    ];
    for (const [given, why] of cases)
      expect(checkAnswers([one!, many!, free!], given, true)).toBe(why);
    // An MCP payload names its question by question_id.
    expect(
      checkAnswers([optional!], [{ question_id: "Q4", text: "Keep" }], false),
    ).toEqual([{ questionId: "Q4", options: [], text: "Keep" }]);
    expect(checkAnswers([optional!], [], false)).toEqual([]);
  });
});
