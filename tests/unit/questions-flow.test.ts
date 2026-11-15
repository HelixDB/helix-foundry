import { describe, expect, it } from "vitest";
import {
  MAX_OPTIONS,
  MAX_OWN,
  answerText,
  answersOf,
  canLeaveRest,
  canReview,
  check,
  flowKey,
  keyAction,
  leftText,
  questionName,
  readQuestion,
  readQuestions,
  reduceFlow,
  restoreFlow,
  rowsOf,
  startFlow,
  type FlowAction,
  type FlowState,
  type Question,
} from "@/web/questions/model";

// The question-by-question flow's pure half (src/web/questions/model.ts):
// reading questions without trust, checking a draft as the WFE checks an
// answer, moving through the steps by clicks and keys, what it sends, and
// what a tab keeps.

const one = readQuestion({
  id: "q1",
  header: "Customers",
  text: "Is a customer the company or the person?",
  options: [{ label: "A", description: "The company" }, { label: "B" }],
  recommended: "B",
  blocking: true,
})!;
const many = readQuestion({
  id: "q2",
  text: "Which signals?",
  options: [{ label: "X" }, { label: "Y" }, { label: "Z" }],
  multiple: true,
  blocking: true,
})!;
const free = readQuestion({
  id: "q3",
  text: "How many days?",
  recommended: "90 days",
  blocking: true,
})!;
const optional = readQuestion({
  id: "q4",
  text: "Keep cancelled orders?",
  options: [{ label: "Keep" }, { label: "Drop" }],
})!;
const all = [one, many, free, optional];
const run = (
  questions: readonly Question[],
  actions: FlowAction[],
  from: FlowState = startFlow(),
) => actions.reduce((s, a) => reduceFlow(questions, s, a), from);

describe("readQuestion", () => {
  it("follows its @example", () => {
    expect(
      readQuestion({
        id: "q1",
        text: "Which?",
        options: [{ label: "A" }, { label: "B" }],
        recommended: "B",
      })?.ask,
    ).toEqual({
      kind: "choice",
      multiple: false,
      options: [
        { label: "B", description: "" },
        { label: "A", description: "" },
      ],
      recommended: "B",
    });
    expect(
      readQuestion({ id: "q1", text: "Which?", options: [{ label: "A" }] }),
    ).toBeUndefined();
  });

  it("reads a free question, its suggestion, header and items", () => {
    expect(
      readQuestion({
        id: "q",
        header: "  Window ",
        text: "How long?",
        recommended: " 90 days ",
        items: ["type.Contract", 3],
        blocking: true,
      }),
    ).toEqual({
      id: "q",
      header: "Window",
      text: "How long?",
      blocking: true,
      items: ["type.Contract"],
      ask: { kind: "free", suggested: "90 days" },
    });
    expect(readQuestion({ id: "q", text: "How long?" })).toMatchObject({
      header: "",
      blocking: false,
      items: [],
      ask: { kind: "free", suggested: "" },
    });
  });

  it("refuses what the WFE refuses", () => {
    const base = { id: "q", text: "Which?" };
    const opts = (n: number) =>
      Array.from({ length: n }, (_, i) => ({ label: `o${i}` }));
    for (const bad of [
      null,
      "q",
      { text: "Which?" },
      { id: "q", text: "  " },
      { ...base, multiple: true },
      { ...base, options: [{ label: "A" }, { label: "A" }] },
      { ...base, options: [{ label: "A" }, { label: " " }] },
      { ...base, options: [{ label: "A" }, "B"] },
      { ...base, options: [{ label: "A" }, { label: "B" }], recommended: "C" },
      { ...base, options: opts(MAX_OPTIONS + 1) },
    ])
      expect(readQuestion(bad)).toBeUndefined();
    expect(readQuestion({ ...base, options: opts(MAX_OPTIONS) })).toBeDefined();
  });

  it("reads a round only when every question reads, ids distinct", () => {
    expect(readQuestions([{ id: "q1", text: "A?" }])).toHaveLength(1);
    expect(readQuestions([])).toBeUndefined();
    expect(readQuestions("x")).toBeUndefined();
    expect(
      readQuestions([{ id: "q1", text: "A?" }, { id: "q2" }]),
    ).toBeUndefined();
    expect(
      readQuestions([
        { id: "q1", text: "A?" },
        { id: "q1", text: "B?" },
      ]),
    ).toBeUndefined();
  });
});

describe("check", () => {
  it("follows its @example", () => {
    const q1 = readQuestion({
      id: "q1",
      text: "?",
      options: [{ label: "A" }, { label: "B" }],
    })!;
    expect(check(q1, { kind: "option", label: "A" })).toEqual({
      kind: "answered",
      answer: { questionId: "q1", options: ["A"] },
    });
    expect(check(q1, { kind: "own", text: " " })).toEqual({
      kind: "incomplete",
      reason: "Write your answer, or choose an option.",
    });
    expect(check(q1, undefined)).toEqual({ kind: "unanswered" });
  });

  it("takes words alone, trimmed, up to MAX_OWN characters", () => {
    expect(check(free, { kind: "own", text: " 60 days " })).toEqual({
      kind: "answered",
      answer: { questionId: "q3", options: [], text: "60 days" },
    });
    expect(check(free, { kind: "own", text: "🙂".repeat(MAX_OWN) }).kind).toBe(
      "answered",
    );
    expect(
      check(free, { kind: "own", text: "🙂".repeat(MAX_OWN + 2) }),
    ).toEqual({ kind: "incomplete", reason: "2 characters too long." });
  });

  it("refuses a draft of another kind of question", () => {
    expect(check(free, { kind: "option", label: "A" }).kind).toBe("unanswered");
    expect(check(many, { kind: "option", label: "X" }).kind).toBe("unanswered");
    expect(check(one, { kind: "option", label: "C" }).kind).toBe("unanswered");
    expect(check(many, { kind: "own", text: "x" }).kind).toBe("unanswered");
    expect(check(one, { kind: "many", labels: ["A"], own: null }).kind).toBe(
      "unanswered",
    );
  });

  it("answers a choice of several with options, words, or both", () => {
    expect(
      check(many, { kind: "many", labels: ["Z", "X", "nope"], own: null }),
    ).toEqual({
      kind: "answered",
      answer: { questionId: "q2", options: ["X", "Z"] },
    });
    expect(
      check(many, { kind: "many", labels: ["Y"], own: " and W " }),
    ).toEqual({
      kind: "answered",
      answer: { questionId: "q2", options: ["Y"], text: "and W" },
    });
    expect(check(many, { kind: "many", labels: [], own: "W" })).toEqual({
      kind: "answered",
      answer: { questionId: "q2", options: [], text: "W" },
    });
    // A blank box beside options ticked is no words; alone, it is unfinished.
    expect(check(many, { kind: "many", labels: ["Y"], own: " " })).toEqual({
      kind: "answered",
      answer: { questionId: "q2", options: ["Y"] },
    });
    expect(check(many, { kind: "many", labels: [], own: "" }).kind).toBe(
      "incomplete",
    );
    expect(check(many, { kind: "many", labels: [], own: null }).kind).toBe(
      "unanswered",
    );
    expect(
      check(many, {
        kind: "many",
        labels: ["X"],
        own: "x".repeat(MAX_OWN + 1),
      }).kind,
    ).toBe("incomplete");
  });
});

describe("reduceFlow", () => {
  it("follows its @example", () => {
    const q1 = readQuestion({
      id: "q1",
      text: "?",
      options: [{ label: "A" }, { label: "B" }],
    })!;
    const s = reduceFlow([q1, free], startFlow(), {
      type: "choose",
      label: "A",
      advance: true,
    });
    expect(s.step).toBe(1);
    expect(s.drafts.q1).toEqual({ kind: "option", label: "A" });
  });

  it("chooses without going on, keeping the cursor on the choice", () => {
    const s = run(all, [{ type: "choose", label: "A", advance: false }]);
    expect(s).toEqual({
      step: 0,
      cursor: 1,
      drafts: { q1: { kind: "option", label: "A" } },
    });
  });

  it("ignores actions that don't apply to the question shown", () => {
    const s = startFlow();
    for (const a of [
      { type: "choose", label: "nope", advance: true },
      { type: "toggle", label: "A" },
      { type: "write", text: "x" },
      { type: "clear" },
      { type: "next" },
    ] satisfies FlowAction[])
      expect(reduceFlow(all, s, a)).toBe(s);
    const atMany = { ...s, step: 1 };
    for (const a of [
      { type: "choose", label: "X", advance: true },
      { type: "toggle", label: "nope" },
      { type: "write", text: "x" },
    ] satisfies FlowAction[])
      expect(reduceFlow(all, atMany, a)).toBe(atMany);
    const atFree = { ...s, step: 2 };
    for (const a of [
      { type: "own" },
      { type: "cursor", to: 3 },
    ] satisfies FlowAction[])
      expect(reduceFlow(all, atFree, a)).toBe(atFree);
    const atReview = { ...s, step: 4 };
    for (const a of [
      { type: "write", text: "x" },
      { type: "own" },
      { type: "cursor", to: 1 },
      { type: "rest" },
    ] satisfies FlowAction[])
      expect(reduceFlow(all, atReview, a)).toBe(atReview);
  });

  it("ticks and unticks options of a choice of several", () => {
    const at = { ...startFlow(), step: 1 };
    let s = run(all, [{ type: "toggle", label: "Y" }], at);
    expect(s.drafts.q2).toEqual({ kind: "many", labels: ["Y"], own: null });
    expect(s.cursor).toBe(1);
    s = run(all, [{ type: "toggle", label: "X" }], s);
    expect(s.drafts.q2).toEqual({
      kind: "many",
      labels: ["Y", "X"],
      own: null,
    });
    s = run(
      all,
      [
        { type: "toggle", label: "Y" },
        { type: "toggle", label: "X" },
      ],
      s,
    );
    expect(s.drafts.q2).toBeUndefined();
  });

  it("opens and closes Something else", () => {
    // A choice of one answers in words, keeping them while it stays chosen.
    let s = run(all, [{ type: "own" }, { type: "write", text: "Both" }]);
    expect(s.drafts.q1).toEqual({ kind: "own", text: "Both" });
    expect(s.cursor).toBe(2);
    expect(run(all, [{ type: "own" }], s).drafts.q1).toEqual({
      kind: "own",
      text: "Both",
    });
    expect(
      run(all, [{ type: "choose", label: "A", advance: false }], s).drafts.q1,
    ).toEqual({ kind: "option", label: "A" });
    // A choice of several opens a box beside its options, and closes it.
    const at = { ...startFlow(), step: 1 };
    s = run(
      all,
      [
        { type: "toggle", label: "X" },
        { type: "own" },
        { type: "write", text: "W" },
      ],
      at,
    );
    expect(s.drafts.q2).toEqual({ kind: "many", labels: ["X"], own: "W" });
    expect(s.cursor).toBe(3);
    expect(run(all, [{ type: "own" }], s).drafts.q2).toEqual({
      kind: "many",
      labels: ["X"],
      own: null,
    });
    s = run(all, [{ type: "own" }], at);
    expect(s.drafts.q2).toEqual({ kind: "many", labels: [], own: "" });
    expect(run(all, [{ type: "own" }], s).drafts.q2).toBeUndefined();
    // Unticking the last option keeps an open box.
    s = run(
      all,
      [
        { type: "toggle", label: "X" },
        { type: "own" },
        { type: "toggle", label: "X" },
      ],
      at,
    );
    expect(s.drafts.q2).toEqual({ kind: "many", labels: [], own: "" });
  });

  it("writes a free answer, and forgets an empty one", () => {
    const at = { ...startFlow(), step: 2 };
    const s = run(all, [{ type: "write", text: "60" }], at);
    expect(s.drafts.q3).toEqual({ kind: "own", text: "60" });
    expect(
      run(all, [{ type: "write", text: "" }], s).drafts.q3,
    ).toBeUndefined();
  });

  it("leaves a question that blocks only once answered, skips one that doesn't", () => {
    let s = run(all, [{ type: "next" }]);
    expect(s.step).toBe(0);
    s = run(all, [{ type: "own" }, { type: "next" }]);
    expect(s.step).toBe(0);
    s = run(all, [
      { type: "choose", label: "B", advance: true },
      { type: "toggle", label: "X" },
      { type: "next" },
      { type: "write", text: "60 days" },
      { type: "next" },
    ]);
    expect(s.step).toBe(3);
    expect(canLeaveRest(all, s.drafts, 3)).toBe(true);
    // Skipped: left to the agent, then Review.
    s = run(all, [{ type: "next" }], s);
    expect(s.step).toBe(4);
    expect(answersOf(all, s.drafts)).toEqual({
      answers: [
        { questionId: "q1", options: ["B"] },
        { questionId: "q2", options: ["X"] },
        { questionId: "q3", options: [], text: "60 days" },
      ],
      left: ["q4"],
    });
  });

  it("goes from the last question back to the first still unanswered", () => {
    const s = run(all, [
      { type: "go", step: 3 },
      { type: "choose", label: "Keep", advance: true },
    ]);
    expect(s.step).toBe(0);
    expect(s.drafts.q4).toEqual({ kind: "option", label: "Keep" });
    expect(s.cursor).toBe(0);
  });

  it("goes back, and to any question; to Review only when it can", () => {
    let s = run(all, [{ type: "back" }]);
    expect(s.step).toBe(0);
    s = run(all, [{ type: "go", step: 2 }, { type: "back" }]);
    expect(s.step).toBe(1);
    expect(run(all, [{ type: "go", step: 4 }]).step).toBe(0);
    expect(run(all, [{ type: "go", step: 1.5 }]).step).toBe(0);
    expect(run(all, [{ type: "go", step: -1 }]).step).toBe(0);
    s = run([optional], [{ type: "go", step: 1 }]);
    expect(s.step).toBe(1);
  });

  it("clears a question that doesn't block, never one that does", () => {
    const s = run(all, [
      { type: "go", step: 3 },
      { type: "choose", label: "Drop", advance: false },
      { type: "clear" },
    ]);
    expect(s.drafts.q4).toBeUndefined();
    const blocked = run(all, [
      { type: "choose", label: "A", advance: false },
      { type: "clear" },
    ]);
    expect(blocked.drafts.q1).toEqual({ kind: "option", label: "A" });
  });

  it("lets the agent decide the rest only when nothing that blocks is left", () => {
    expect(canLeaveRest(all, {}, 0)).toBe(false);
    expect(run(all, [{ type: "rest" }]).step).toBe(0);
    const done = {
      q1: { kind: "option", label: "A" },
      q2: { kind: "many", labels: ["X"], own: null },
      q3: { kind: "own", text: "x" },
    } as const;
    expect(canReview(all, done)).toBe(true);
    expect(canLeaveRest(all, done, 0)).toBe(true);
    expect(canLeaveRest(all, done, 4)).toBe(false);
    expect(
      canLeaveRest(all, { ...done, q4: { kind: "option", label: "Keep" } }, 0),
    ).toBe(false);
    expect(
      run(all, [{ type: "rest" }], { ...startFlow(), drafts: done }).step,
    ).toBe(4);
  });

  it("moves the cursor around a choice's rows, wrapping", () => {
    expect(rowsOf(one)).toBe(3);
    expect(rowsOf(free)).toBe(0);
    expect(run(all, [{ type: "cursor", to: 4 }]).cursor).toBe(1);
    expect(run(all, [{ type: "cursor", to: -1 }]).cursor).toBe(2);
  });

  it("starts a step's cursor on its answer", () => {
    const drafts = {
      q1: { kind: "own", text: "x" },
      q2: { kind: "many", labels: ["Z"], own: null },
    } as const;
    const s = { ...startFlow(), drafts };
    expect(run(all, [{ type: "go", step: 1 }], s).cursor).toBe(2);
    expect(
      run(
        all,
        [
          { type: "go", step: 1 },
          { type: "go", step: 0 },
        ],
        s,
      ).cursor,
    ).toBe(2);
    expect(run(all, [{ type: "go", step: 3 }], s).cursor).toBe(0);
  });
});

describe("keyAction", () => {
  const s = startFlow();
  it("follows its @example", () => {
    const q1 = readQuestion({
      id: "q1",
      text: "?",
      options: [{ label: "A" }, { label: "B" }],
    })!;
    expect(keyAction(q1, s, "ArrowDown")).toEqual({ type: "cursor", to: 1 });
    expect(keyAction(q1, s, "Enter")).toEqual({
      type: "choose",
      label: "A",
      advance: true,
    });
  });

  it("moves with the arrows, Home and End", () => {
    expect(keyAction(one, { ...s, cursor: 1 }, "ArrowUp")).toEqual({
      type: "cursor",
      to: 0,
    });
    expect(keyAction(one, s, "ArrowRight")).toEqual({ type: "cursor", to: 1 });
    expect(keyAction(one, s, "ArrowLeft")).toEqual({ type: "cursor", to: -1 });
    expect(keyAction(one, s, "Home")).toEqual({ type: "cursor", to: 0 });
    expect(keyAction(one, s, "End")).toEqual({ type: "cursor", to: 2 });
  });

  it("chooses with Space and Enter, ticks with Space, goes on with Enter", () => {
    // One's recommended option is its first row.
    expect(keyAction(one, s, " ")).toEqual({
      type: "choose",
      label: "B",
      advance: false,
    });
    expect(keyAction(many, s, " ")).toEqual({ type: "toggle", label: "X" });
    expect(keyAction(many, s, "Enter")).toEqual({ type: "next" });
    expect(keyAction(one, { ...s, cursor: 2 }, "Enter")).toEqual({
      type: "own",
    });
    expect(keyAction(many, { ...s, cursor: 3 }, " ")).toEqual({
      type: "own",
    });
  });

  it("picks a row by its number", () => {
    expect(keyAction(one, s, "2")).toEqual({
      type: "choose",
      label: "A",
      advance: true,
    });
    expect(keyAction(many, s, "3")).toEqual({ type: "toggle", label: "Z" });
    expect(keyAction(one, s, "3")).toEqual({ type: "own" });
    expect(keyAction(one, s, "4")).toBeNull();
    expect(keyAction(one, s, "0")).toBeNull();
    expect(keyAction(one, s, "a")).toBeNull();
    expect(keyAction(free, s, "1")).toBeNull();
  });
});

describe("what it sends and says", () => {
  it("sends nothing while a question can't be left", () => {
    expect(answersOf(all, {})).toBeUndefined();
  });

  it("follows answerText's @example", () => {
    expect(
      answerText({ options: ["Monthly", "Yearly"], text: "and quarterly" }),
    ).toBe("Monthly, Yearly, and “and quarterly”");
    expect(answerText({ options: [], text: "60 days" })).toBe("“60 days”");
    expect(answerText({ options: ["A"] })).toBe("A");
    expect(answerText({})).toBe("");
  });

  it("says what a question left to the agent takes, and names it", () => {
    expect(leftText(one)).toBe("Left to the agent: it recommends B");
    expect(leftText(many, "the Assistant")).toBe("Left to the Assistant");
    expect(leftText(free)).toBe("Left to the agent: it suggests “90 days”");
    expect(leftText(readQuestion({ id: "q", text: "?" })!)).toBe(
      "Left to the agent",
    );
    expect(questionName(one, 0)).toBe("Customers");
    expect(questionName(many, 1)).toBe("Question 2");
  });
});

describe("restoreFlow", () => {
  it("follows its @example", () => {
    const q1 = readQuestion({
      id: "q1",
      text: "?",
      options: [{ label: "A" }, { label: "B" }],
    })!;
    expect(
      restoreFlow(
        [q1],
        '{"step":0,"drafts":{"q1":{"kind":"option","label":"A"}}}',
      ).drafts,
    ).toEqual({ q1: { kind: "option", label: "A" } });
    expect(restoreFlow([q1], "not json")).toEqual(startFlow());
  });

  it("starts afresh from nothing kept", () => {
    expect(restoreFlow(all, null)).toEqual(startFlow());
    expect(restoreFlow(all, "[1]")).toEqual(startFlow());
    expect(restoreFlow(all, '{"step":2}')).toEqual({ ...startFlow(), step: 2 });
  });

  it("keeps only drafts that fit their questions", () => {
    const kept = JSON.stringify({
      step: 1,
      drafts: {
        q1: { kind: "option", label: "nope" },
        q2: { kind: "many", labels: ["X", "X", "Y"], own: "w" },
        q3: { kind: "own", text: "60" },
        q4: { kind: "many", labels: ["Keep"], own: null },
        gone: { kind: "own", text: "x" },
      },
    });
    const s = restoreFlow(all, kept);
    expect(s.drafts).toEqual({
      q2: { kind: "many", labels: ["X", "Y"], own: "w" },
      q3: { kind: "own", text: "60" },
    });
    expect(s.step).toBe(1);
    expect(s.cursor).toBe(0);
    for (const bad of [
      { kind: "many", labels: "X", own: null },
      { kind: "many", labels: [1], own: null },
      { kind: "many", labels: ["X"], own: 3 },
      { kind: "option" },
      { kind: "own" },
      { kind: "nope" },
      "x",
    ])
      expect(
        restoreFlow(all, JSON.stringify({ drafts: { q2: bad } })).drafts,
      ).toEqual({});
    expect(
      restoreFlow(
        all,
        JSON.stringify({ drafts: { q1: { kind: "own", text: "x" } } }),
      ).drafts,
    ).toEqual({ q1: { kind: "own", text: "x" } });
  });

  it("keeps Review only while it can show", () => {
    const answered = {
      q1: { kind: "option", label: "A" },
      q2: { kind: "many", labels: ["X"], own: null },
      q3: { kind: "own", text: "x" },
    };
    expect(
      restoreFlow(all, JSON.stringify({ step: 4, drafts: answered })).step,
    ).toBe(4);
    expect(restoreFlow(all, JSON.stringify({ step: 4, drafts: {} })).step).toBe(
      0,
    );
    expect(restoreFlow(all, JSON.stringify({ step: 9 })).step).toBe(0);
    expect(restoreFlow(all, JSON.stringify({ step: 0.5 })).step).toBe(0);
  });

  it("is kept per round, in this tab", () => {
    expect(flowKey("generation:gen_1:2")).toBe(
      "foundry.questions.v1:generation:gen_1:2",
    );
  });
});
