import { describe, expect, it } from "vitest";
import type {
  AskCitation,
  AskUsage,
  CitationTarget,
  FoundryQuestion,
  OntologyQuery,
  QuestionEvent,
  VersionOf,
} from "@/shared/wfe";
import contractsBundle from "../../mock/fixtures/agent-bundle-contracts.json";
import shopBundle from "../../mock/fixtures/agent-bundle.json";
import {
  EMPTY_STREAM,
  answerView,
  askOntologyId,
  askSuggestions,
  askView,
  budgetFull,
  citationHref,
  citationText,
  citedEntities,
  NOTHING_KNOWN,
  draftGoal,
  graphQuery,
  knownOf,
  parseAskEvent,
  parseAskResult,
  parseQuery,
  parseTarget,
  plainText,
  queryText,
  questionFocus,
  questionView,
  reduceEvents,
  refusalText,
  resultLabel,
  resultText,
  stepHeading,
  stepInput,
  threadGroups,
  threadsOf,
  toolLabel,
  usageText,
  type AskStep,
  type CitationContext,
  type Thread,
  workedFor,
} from "@/web/areas/ontology/ask-model";
import { parseEntityId } from "@/web/areas/explore/model";
import { formatDate } from "@/web/kit/util";
import { refKey } from "@/web/areas/explore/rule-breaks";
import { contractErrors } from "../../mock/agent-contract";

// The Ask panel's logic: events folded into steps, typed queries read
// aloud, answers as plain text and chips, and where each citation opens.
// Every well-formed fixture is the engine's JSON: it fits ask_event.json,
// ask_result.json or read_request.json's Query.

const ev = (seq: number, e: unknown): QuestionEvent => ({
  seq,
  eventJson: typeof e === "string" ? e : JSON.stringify(e),
});
/** An event fixture, held to ask_event.json. */
const event = <T>(e: T): T => {
  expect(contractErrors("ask_event", e)).toEqual([]);
  return e;
};
const call = (id: string, tool: string, input: unknown = {}) =>
  event({ event: "tool_call", call_id: id, tool, input });
const result = (id: string, evidence: string, summary: string) =>
  event({
    event: "tool_result",
    call_id: id,
    summary,
    evidence_id: evidence,
  });
const failed = (id: string, summary: string, error: string) =>
  event({ event: "tool_result", call_id: id, summary, error });
const ended = (id: string, summary: string) =>
  event({ event: "tool_result", call_id: id, summary });
const USAGE: AskUsage = {
  input: 0,
  cache_read: 0,
  cache_write: 0,
  output: 0,
  usd: 0,
};

const INVOICES: OntologyQuery = {
  start: {
    type: "Invoice",
    where: [{ prop: "status", op: "=", value: "open" }],
    ids: null,
    text: null,
  },
  steps: [{ via: "billed_to", dir: "out", to: "Customer", where: [] }],
  filters: [
    { at: 0, holds: "overdue" },
    { at: 1, not_holds: "blocked" },
    { at: 1, violates: "credit_limit" },
  ],
  return: {
    rows: {
      columns: [
        { at: 0 },
        { at: 0, prop: "amount" },
        { at: 1, prop: "legal_name" },
      ],
      order_by: { at: 0, prop: "amount", desc: true },
      limit: 50,
    },
  },
};

// INVOICES as the agent ran it: every field written out.
const INVOICES_RAN: OntologyQuery = {
  ...INVOICES,
  steps: [{ via: "billed_to", dir: "out", to: "Customer", where: [] }],
  return: {
    rows: {
      columns: [
        { at: 0, prop: null },
        { at: 0, prop: "amount" },
        { at: 1, prop: "legal_name" },
      ],
      order_by: { at: 0, prop: "amount", desc: true },
      limit: 50,
    },
  },
};

describe("the event reducer", () => {
  it("folds calls and their results into steps, in seq order", () => {
    const s = reduceEvents(EMPTY_STREAM, {
      events: [
        ev(3, result("a", "e1", "1 Customer: Globex")),
        ev(1, { event: "started", model: "claude-sonnet-5-5" }),
        ev(2, call("a", "find_entities", { type: "Customer" })),
        ev(4, call("b", "query", INVOICES)),
      ],
    });
    expect(s.lastSeq).toBe(4);
    expect(s.model).toBe("claude-sonnet-5-5");
    expect(s.finished).toBe(false);
    expect(s.steps).toEqual([
      {
        callId: "a",
        tool: "find_entities",
        input: { type: "Customer" },
        result: {
          kind: "recorded",
          summary: "1 Customer: Globex",
          evidenceId: "e1",
        },
      },
      { callId: "b", tool: "query", input: INVOICES, query: INVOICES },
    ]);
  });

  it("skips events it folded already, across overlapping polls", () => {
    const first = reduceEvents(EMPTY_STREAM, {
      events: [
        ev(1, call("a", "describe_type")),
        ev(2, result("a", "e1", "Props")),
      ],
    });
    const again = reduceEvents(first, {
      events: [ev(2, result("a", "e1", "Props")), ev(3, { event: "finished" })],
    });
    expect(again.steps).toHaveLength(1);
    expect(again.finished).toBe(true);
    expect(again.lastSeq).toBe(3);
    // Folding returns new state; the old one is unchanged.
    expect(first.finished).toBe(false);
    expect(reduceEvents(again, {})).toBe(again);
  });

  it("is complete once the question ended and its events reach the last seq", () => {
    const page = (
      status: string | undefined,
      lastSeq: number,
      seqs: number[],
    ) => ({
      events: seqs.map((n) => ev(n, call(`c${n}`, "describe_type"))),
      ...(status ? { status: status as "answered" } : {}),
      lastSeq,
    });
    // Still live: more may come, however many it read.
    const running = reduceEvents(EMPTY_STREAM, page("running", 2, [1, 2]));
    expect(running.complete).toBe(false);
    expect(reduceEvents(EMPTY_STREAM, page("queued", 0, [])).complete).toBe(
      false,
    );
    // Ended, but a page short of its last seq: the next page has the rest.
    const short = reduceEvents(running, page("answered", 4, [3]));
    expect([short.lastSeq, short.complete]).toEqual([3, false]);
    const done = reduceEvents(short, page("answered", 4, [4]));
    expect([done.lastSeq, done.complete]).toEqual([4, true]);
    expect(reduceEvents(done, page("answered", 4, []))).toBe(done);
    // Cancelled before any event: nothing follows.
    expect(reduceEvents(EMPTY_STREAM, page("cancelled", 0, [])).complete).toBe(
      true,
    );
    // A page without a status (a default left out) says nothing either way.
    expect(reduceEvents(EMPTY_STREAM, page(undefined, 0, [])).complete).toBe(
      false,
    );
  });

  it("keeps a result without its call, failures and endings, and counts what it can't read", () => {
    const s = reduceEvents(EMPTY_STREAM, {
      events: [
        ev(1, "not json"),
        ev(2, { event: "mystery" }),
        ev(3, { event: "tool_call", tool: "query" }),
        ev(4, failed("x", "query: failed", "too_broad: more than 20,000")),
        ev(5, call("q", "query", { start: {} })),
        ev(6, call("z", "answer", { blocks: [], citations: [] })),
        ev(7, ended("z", "answer: ended the question")),
      ],
    });
    expect(s.unreadable).toBe(3);
    expect(s.steps).toEqual([
      {
        callId: "x",
        tool: "",
        input: null,
        result: {
          kind: "failed",
          summary: "query: failed",
          error: "too_broad: more than 20,000",
        },
      },
      // A query input that isn't a typed query keeps its JSON only.
      { callId: "q", tool: "query", input: { start: {} } },
      {
        callId: "z",
        tool: "answer",
        input: { blocks: [], citations: [] },
        result: { kind: "ended", summary: "answer: ended the question" },
      },
    ]);
    // A failed step says why; the call that ended the question says how
    // in its heading, with nothing after it.
    expect(s.steps.map(resultText)).toEqual([
      "Failed: too_broad: more than 20,000",
      "",
      "",
    ]);
    expect(s.steps.map(stepHeading)).toEqual([
      "A result",
      "Queried",
      "Answered",
    ]);
  });

  it("reads a recorded result without the tool's name, which heads the step", () => {
    const step = (tool: string, summary: string) =>
      reduceEvents(EMPTY_STREAM, {
        events: [ev(1, call("a", tool)), ev(2, result("a", "e1", summary))],
      }).steps[0]!;
    expect(resultText(step("query", "query: 3 of 40 rows"))).toBe(
      "3 of 40 rows",
    );
    expect(resultText(step("query", "3 of 40 rows"))).toBe("3 of 40 rows");
    expect(
      resultText(step("find_entities", "find_entities: 1 Customer found")),
    ).toBe("1 Customer found");
  });

  it("gives a second result of one call id its own step", () => {
    const s = reduceEvents(EMPTY_STREAM, {
      events: [
        ev(1, call("a", "query", INVOICES)),
        ev(2, result("a", "e1", "first")),
        ev(3, result("a", "e2", "second")),
      ],
    });
    expect(s.steps.map((x) => x.result?.summary)).toEqual(["first", "second"]);
  });

  it("shows the latest attempt's steps once a retried question starts again", () => {
    // Attempt 1 recorded e1 and left a call without its result; the
    // engine numbers attempt 2's evidence from e1 again.
    const first = reduceEvents(EMPTY_STREAM, {
      events: [
        ev(1, { event: "started", model: "claude-sonnet-5-5" }),
        ev(2, call("a", "describe_type")),
        ev(3, result("a", "e1", "describe_type: Customer")),
        ev(4, call("b", "query", INVOICES)),
        ev(5, "not json"),
      ],
    });
    expect([first.steps.length, first.unreadable]).toEqual([2, 1]);
    const s = reduceEvents(first, {
      events: [
        ev(6, { event: "started", model: "claude-opus-5-5" }),
        ev(7, call("c", "find_entities", { type: "Customer" })),
        ev(8, result("c", "e1", "find_entities: 1 Customer")),
      ],
    });
    expect(s.model).toBe("claude-opus-5-5");
    expect(s.unreadable).toBe(0);
    expect(s.steps).toEqual([
      {
        callId: "c",
        tool: "find_entities",
        input: { type: "Customer" },
        result: {
          kind: "recorded",
          summary: "find_entities: 1 Customer",
          evidenceId: "e1",
        },
      },
    ]);
    // An attempt that finished and was retried isn't finished any more.
    const finished = reduceEvents(EMPTY_STREAM, {
      events: [
        ev(1, { event: "started", model: "m" }),
        ev(2, { event: "finished" }),
        ev(3, { event: "started", model: "m" }),
      ],
    });
    expect(finished.finished).toBe(false);
  });

  it("shows a read the model repeats as a repeat of the step that recorded it", () => {
    const s = reduceEvents(EMPTY_STREAM, {
      events: [
        ev(1, call("a", "query", INVOICES)),
        ev(2, result("a", "e1", "query: 3 of 40 rows")),
        ev(3, call("b", "query", INVOICES)),
        ev(4, result("b", "e1", "query: as e1")),
      ],
    });
    expect(s.steps.map((x) => x.result)).toEqual([
      { kind: "recorded", summary: "query: 3 of 40 rows", evidenceId: "e1" },
      { kind: "repeated", summary: "query: 3 of 40 rows", evidenceId: "e1" },
    ]);
    // Only the first step is the evidence: the page gives it alone e1's
    // element id and lights it alone.
    expect(
      s.steps.filter(
        (x) => x.result?.kind === "recorded" && x.result.evidenceId === "e1",
      ),
    ).toHaveLength(1);
    expect(resultText(s.steps[1]!)).toBe(
      "Same as an earlier step: 3 of 40 rows",
    );
    expect(resultText(s.steps[1]!)).not.toContain("as e1");
  });

  it("heads an answer the engine refused with the call, and says why it failed", () => {
    const s = reduceEvents(EMPTY_STREAM, {
      events: [
        ev(1, call("a", "answer", { blocks: [], citations: [] })),
        ev(
          2,
          failed(
            "a",
            "answer: refused",
            "citation c3 cites e9, which no result recorded",
          ),
        ),
        ev(3, call("b", "answer", { blocks: [], citations: [] })),
        ev(4, ended("b", "answer: ended the question")),
      ],
    });
    // A refused answer says so; the one that ended the question, how.
    expect(s.steps.map((x) => [stepHeading(x), resultText(x)])).toEqual([
      ["Answer", "Refused"],
      ["Answered", ""],
    ]);
    expect(toolLabel("cannot_answer")).toBe("Can’t answer");
  });

  it("says why an answer was refused for a person, and whether it was corrected", () => {
    const told =
      'the answer was refused; fix these and call answer again:\n- citation c2: e3 didn\'t show {"entity":"n9"}; cite a target exactly as a result shows it';
    expect(refusalText(told, true)).toBe(
      "An answer cited something no step had shown; the agent corrected it.",
    );
    expect(
      refusalText("the answer was refused: [c1] has no citation", false),
    ).toBe(
      "An answer didn’t pass the checks every answer must; the agent was told to fix it.",
    );
    // A table or chart placed wrongly says so; a citation problem comes first.
    const visual =
      "the answer was refused; fix these and call answer again:\n- visual 1: `after` is 4, but the answer has 2 blocks; it is the index of the block the visual follows, from 0";
    expect(refusalText(visual, true)).toBe(
      "An answer’s table or chart didn’t pass the checks every visual must; the agent corrected it.",
    );
    expect(
      refusalText(
        `${told}\n- visual 2: e9 is not a result of this question`,
        false,
      ),
    ).toBe(
      "An answer cited something no step had shown; the agent was told to fix it.",
    );
  });

  it("heads a question's ending call by how it ended", () => {
    const step = (tool: string, result?: AskStep["result"]): AskStep => ({
      callId: "a",
      tool,
      input: {},
      ...(result ? { result } : {}),
    });
    const ended = { kind: "ended", summary: "" } as const;
    expect(stepHeading(step("answer", ended))).toBe("Answered");
    expect(stepHeading(step("cannot_answer", ended))).toBe("Couldn’t answer");
    expect(stepHeading(step("cannot_answer"))).toBe("Can’t answer");
    expect(
      stepHeading(
        step("cannot_answer", { kind: "failed", summary: "", error: "x" }),
      ),
    ).toBe("Can’t answer");
    expect(
      resultText(
        step("cannot_answer", {
          kind: "failed",
          summary: "",
          error: "bad gap",
        }),
      ),
    ).toBe("Failed: bad gap");
  });

  it("names tools", () => {
    expect(toolLabel("find_entities")).toBe("Found entities");
    expect(toolLabel("load_profile")).toBe("Read what the load holds");
    expect(toolLabel("helix_query")).toBe("Queried the graph directly");
    expect(toolLabel("history_of")).toBe("Read the history");
    expect(toolLabel("changes")).toBe("Read the changes");
    expect(toolLabel("new_tool")).toBe("new tool");
    expect(toolLabel("")).toBe("A result");
  });

  it("parses each event kind and refuses malformed ones", () => {
    expect(parseAskEvent('{"event":"started"}')).toEqual({
      event: "started",
      model: "",
    });
    expect(parseAskEvent(JSON.stringify(event({ event: "finished" })))).toEqual(
      { event: "finished" },
    );
    // A result is recorded, failed, or ended the question.
    const recorded = result("a", "e2", "query: 1 of 1 rows"),
      fail = failed("a", "find_entities: failed", "no type `Planet`"),
      end = ended("a", "answer: ended the question");
    for (const e of [recorded, fail, end])
      expect(parseAskEvent(JSON.stringify(e))).toEqual(e);
    // Never both, and always for a call.
    expect(parseAskEvent(JSON.stringify({ ...recorded, error: "x" }))).toBe(
      undefined,
    );
    expect(contractErrors("ask_event", { ...recorded, error: "x" })).toEqual(
      [],
    );
    expect(parseAskEvent('{"event":"tool_result","summary":"x"}')).toBe(
      undefined,
    );
    expect(parseAskEvent("[]")).toBe(undefined);
  });
});

describe("step inputs and entity names", () => {
  const step = (
    tool: string,
    input: unknown,
    summary = "",
    evidenceId = "e1",
  ): AskStep => ({
    callId: "a",
    tool,
    input,
    ...(summary
      ? { result: { kind: "recorded", summary, evidenceId } as const }
      : {}),
  });
  // The steps of a question about an order, with the answer that resolved
  // the handles it cited.
  const steps: AskStep[] = [
    step(
      "find_entities",
      { type: "DocOrder", by: "name", value: "ORD-0000002", limit: 5 },
      "find_entities: 1 DocOrder found",
      "e1",
    ),
    step("get_entity", { entity: "n1" }, "get_entity: a DocOrder", "e2"),
    step(
      "evidence",
      {
        target: {
          fact: { link: "doc_includes_product", subject: "n1", object: "n3" },
        },
      },
      "evidence: 3 items",
      "e3",
    ),
    {
      callId: "z",
      tool: "answer",
      input: {
        blocks: [{ para: "It holds a widget [c1] [c2]." }],
        citations: [
          { marker: "c1", evidence_id: "e2", target: { entity: "n1" } },
          {
            marker: "c2",
            evidence_id: "e3",
            target: {
              fact: {
                link: "doc_includes_product",
                subject: "n1",
                object: "n3",
              },
            },
          },
        ],
      },
      result: { kind: "ended", summary: "answer: ended the question" },
    },
  ];
  const order = { type: "DocOrder", id: "ord 0000002" },
    widget = { type: "DocProduct", id: "widget" };
  const citations: AskCitation[] = [
    { marker: "c1", evidence_id: "e2", target: { entity: order } },
    {
      marker: "c2",
      evidence_id: "e3",
      target: {
        fact: { link: "doc_includes_product", subject: order, object: widget },
      },
    },
  ];
  const known = knownOf(steps, citations);

  it("resolves the handles the accepted answer cited, by marker", () => {
    expect(known.entity("n1")).toEqual(order);
    expect(known.entity("n3")).toEqual(widget);
    expect(known.entity("n2")).toBe(undefined);
    expect(known.entity(3)).toBe(undefined);
    // Without an accepted answer, no handle is known.
    expect(knownOf(steps.slice(0, 3), citations).entity("n1")).toBe(undefined);
    // A refused answer resolves nothing.
    const refused = {
      ...steps[3]!,
      result: { kind: "failed", summary: "answer: refused", error: "x" },
    } as AskStep;
    expect(knownOf([refused], citations).entity("n1")).toBe(undefined);
  });

  it("names an entity as a lookup by name wrote it, else keeps its id as is", () => {
    expect(known.name(order)).toBe("ORD-0000002");
    expect(known.name({ type: "DocCustomer", id: "cus 000008" })).toBe(
      "cus 000008",
    );
    // Another type with the same id isn't named by it.
    expect(known.name({ type: "Invoice", id: "ord 0000002" })).toBe(
      "ord 0000002",
    );
    // A lookup by identifier or text names nothing; nor does a failed one.
    const by = (b: string, ok = true): AskStep => ({
      callId: "f",
      tool: "find_entities",
      input: { type: "DocOrder", by: b, value: "ORD-0000002" },
      result: ok
        ? { kind: "recorded", summary: "1 DocOrder found", evidenceId: "e1" }
        : { kind: "failed", summary: "", error: "x" },
    });
    for (const s of [by("identifier"), by("text"), by("name", false)])
      expect(knownOf([s], []).name(order)).toBe("ord 0000002");
    expect(NOTHING_KNOWN.name(order)).toBe("ord 0000002");
  });

  it("describes each tool's input in words, never by handle or JSON", () => {
    const say = (s: AskStep) => stepInput(s, known);
    expect(say(steps[0]!)).toBe("DocOrder named “ORD-0000002”");
    expect(
      say(
        step("find_entities", {
          type: "DocOrder",
          by: "identifier",
          value: "o-2",
        }),
      ),
    ).toBe("DocOrder with the identifier “o-2”");
    expect(
      say(step("find_entities", { type: "DocOrder", by: "text", value: "x" })),
    ).toBe("DocOrder matching “x”");
    expect(say(step("find_entities", { type: "DocOrder" }))).toBe("DocOrder");
    expect(
      say(
        step("find_entities", {
          type: "Doc",
          by: "name",
          value: "  long ".repeat(30),
        }),
      ),
    ).toMatch(/^Doc named “long long .*…”$/);
    // A handle the answer resolved is named; one it didn't is its type,
    // from what the step read, or an entity.
    expect(say(steps[1]!)).toBe("DocOrder ORD-0000002");
    expect(
      say(step("get_entity", { entity: "n7" }, "get_entity: a DocCustomer")),
    ).toBe("One DocCustomer");
    expect(say(step("get_entity", { entity: "n7" }))).toBe("One entity");
    expect(say(steps[2]!)).toBe(
      "For DocOrder ORD-0000002 doc_includes_product DocProduct widget",
    );
    expect(
      say(
        step("evidence", {
          target: { fact: { link: "placed_by", subject: "n1", object: "n8" } },
        }),
      ),
    ).toBe("For a placed_by fact");
    expect(say(step("evidence", { target: { entity: "n1" } }))).toBe(
      "For DocOrder ORD-0000002",
    );
    expect(
      say(step("evidence", { target: { entity: "n9" }, prop: "total" })),
    ).toBe("For the total of an entity");
    expect(
      say(step("explain", { pred: "relation:co_signed", args: ["n1", "n3"] })),
    ).toBe(
      "Why co_signed holds for DocOrder ORD-0000002 and DocProduct widget",
    );
    expect(say(step("explain", { pred: "derived:late", args: ["n9"] }))).toBe(
      "Why late holds",
    );
    expect(say(step("explain", {}))).toBe(undefined);
    expect(say(step("violations", { constraint: "credit_limit" }))).toBe(
      "Breaks of credit_limit",
    );
    expect(
      say(step("violations", { constraint: "credit_limit", type: "Customer" })),
    ).toBe("Breaks of credit_limit naming a Customer");
    expect(say(step("violations", { type: "Customer" }))).toBe(
      "Breaks naming a Customer",
    );
    expect(say(step("violations", {}))).toBe("Every rule break");
    expect(say(step("describe_type", { type: "DocOrder" }))).toBe(
      "The DocOrder type",
    );
    expect(say(step("describe_type", null))).toBe(undefined);
    // A query reads as its path; the ending calls show below the steps;
    // a tool the page doesn't know keeps its JSON only.
    expect(say(step("query", INVOICES))).toBe(undefined);
    expect(say(steps[3]!)).toBe(undefined);
    expect(say(step("cannot_answer", { reason: "r", gap: "g" }))).toBe(
      undefined,
    );
    expect(say(step("new_tool", { a: 1 }))).toBe(undefined);
  });

  it("reads a search of every type as one, by what it looked for", () => {
    const every = (input: unknown) => step("find_entities", input);
    const read = (s: AskStep) => [stepHeading(s), stepInput(s, known)];
    expect(read(every({ by: "text", value: "Acme" }))).toEqual([
      "Searched every type",
      "Matching “Acme”",
    ]);
    expect(read(every({ by: "name", value: "Acme Corp" }))).toEqual([
      "Searched every type",
      "Named “Acme Corp”",
    ]);
    expect(read(every({ by: "identifier", value: "cus_1", limit: 5 }))).toEqual(
      ["Searched every type", "With the identifier “cus_1”"],
    );
    // An empty type is none; without a value there is nothing to say.
    expect(read(every({ type: "", by: "text", value: "x" }))).toEqual([
      "Searched every type",
      "Matching “x”",
    ]);
    expect(read(every({}))).toEqual(["Searched every type", undefined]);
    expect(read(every(null))).toEqual(["Searched every type", undefined]);
    // A search of one type keeps its heading, whatever it found.
    const typed = step(
      "find_entities",
      { type: "DocOrder", by: "text", value: "x" },
      "find_entities: 2 DocOrder found",
    );
    expect(read(typed)).toEqual(["Found entities", "DocOrder matching “x”"]);
    // As its examples say.
    expect(
      stepHeading({
        callId: "c",
        tool: "find_entities",
        input: { by: "text", value: "Acme" },
      }),
    ).toBe("Searched every type");
    expect(
      stepInput(
        {
          callId: "c",
          tool: "find_entities",
          input: { by: "text", value: "Acme" },
        },
        NOTHING_KNOWN,
      ),
    ).toBe("Matching “Acme”");
  });

  it("names an entity as a lookup of every type by name wrote it, its type's first", () => {
    const by = (type: string | undefined, value: string): AskStep => ({
      callId: "f",
      tool: "find_entities",
      input: { ...(type ? { type } : {}), by: "name", value },
      result: { kind: "recorded", summary: "1 found", evidenceId: "e1" },
    });
    const every = knownOf([by(undefined, "  ACME   Corp ")], []);
    expect(every.name({ type: "Customer", id: "acme corp" })).toBe("ACME Corp");
    expect(every.name({ type: "Vendor", id: "acme corp" })).toBe("ACME Corp");
    expect(every.name({ type: "Customer", id: "globex" })).toBe("globex");
    // A lookup of the entity's own type names it first, in any order.
    for (const steps of [
      [by(undefined, "ACME Corp"), by("Customer", "Acme Corp.")],
      [by("Customer", "Acme Corp."), by(undefined, "ACME Corp")],
    ]) {
      const k = knownOf(steps, []);
      expect(k.name({ type: "Customer", id: "acme corp" })).toBe("Acme Corp.");
      expect(k.name({ type: "Vendor", id: "acme corp" })).toBe("ACME Corp");
    }
    // By text or identifier, every type's names nothing.
    for (const b of ["text", "identifier"])
      expect(
        knownOf(
          [
            {
              ...by(undefined, "Acme Corp"),
              input: { by: b, value: "Acme Corp" },
            },
          ],
          [],
        ).name({ type: "Customer", id: "acme corp" }),
      ).toBe("acme corp");
  });

  it("names a cited entity as its page does, before any lookup by name", () => {
    const customer = { type: "DocCustomer", id: "cus 000008" };
    const k = knownOf(
      steps,
      citations,
      new Map([
        [refKey(order), "Order two"],
        [refKey(customer), "Ada Lovelace"],
      ]),
    );
    // Its page's name wins over how a step looked it up, and over its key.
    expect(k.name(order)).toBe("Order two");
    expect(k.name(customer)).toBe("Ada Lovelace");
    // Without its page's name: as a step looked it up, else its id.
    expect(k.name(widget)).toBe("widget");
    expect(knownOf(steps, citations, new Map()).name(order)).toBe(
      "ORD-0000002",
    );
    expect(k.name({ type: "DocOrder", id: "ord 0000009" })).toBe("ord 0000009");
    // Another type's entity with the same id doesn't name it.
    const other = new Map([
      [refKey({ type: "Invoice", id: "ord 0000002" }), "Invoice two"],
    ]);
    expect(knownOf(steps, citations, other).name(order)).toBe("ORD-0000002");
    // The steps read with the same names.
    expect(stepInput(steps[2]!, k)).toBe(
      "For DocOrder Order two doc_includes_product DocProduct widget",
    );
    // As its example says.
    const example = knownOf(
      [
        {
          callId: "a",
          tool: "find_entities",
          input: { type: "Order", by: "name", value: "ORD-0002" },
          result: {
            kind: "recorded",
            summary: "1 Order found",
            evidenceId: "e1",
          },
        },
      ],
      [],
      new Map([[refKey({ type: "Customer", id: "cus_1" }), "Ada Lovelace"]]),
    );
    expect(example.name({ type: "Order", id: "ord 0002" })).toBe("ORD-0002");
    expect(example.name({ type: "Order", id: "ord 0003" })).toBe("ord 0003");
    expect(example.name({ type: "Customer", id: "cus_1" })).toBe(
      "Ada Lovelace",
    );
  });

  it("reads the type an entity read found, whatever its result says after", () => {
    // get_entity's result may now hold mentions; its summary still starts
    // with the type it read.
    for (const summary of [
      "get_entity: a DocCustomer",
      "get_entity: a DocCustomer, 3 mentions",
      "get_entity: an Invoice (2 mentions)",
      "a DocCustomer; 1 mention",
    ])
      expect(
        stepInput(step("get_entity", { entity: "n7" }, summary), known),
      ).toMatch(/^One (DocCustomer|Invoice)$/);
    expect(
      stepInput(
        step("get_entity", { entity: "n7" }, "get_entity: read"),
        known,
      ),
    ).toBe("One entity");
  });

  it("describes a history read by what it is of, never by handle or transaction", () => {
    const say = (input: unknown) => stepInput(step("history_of", input), known);
    expect(say({ target: { entity: "n1" }, limit: 20 })).toBe(
      "History of DocOrder ORD-0000002",
    );
    expect(say({ target: { entity: "n1" }, prop: "status" })).toBe(
      "History of the status of DocOrder ORD-0000002",
    );
    expect(say({ target: { entity: "n9" }, prop: "status" })).toBe(
      "History of the status of an entity",
    );
    expect(
      say({
        target: {
          fact: { link: "doc_includes_product", subject: "n1", object: "n3" },
        },
      }),
    ).toBe(
      "History of DocOrder ORD-0000002 doc_includes_product DocProduct widget",
    );
    expect(
      say({
        target: { fact: { link: "placed_by", subject: "n1", object: "n8" } },
      }),
    ).toBe("History of a placed_by fact");
    expect(
      say({ target: { record: { constraint: "credit_limit", key: "k1" } } }),
    ).toBe("History of a break of credit_limit");
    // A page further back says so, without its transaction.
    expect(
      say({ target: { entity: "n1" }, before: "01JB2Q7X4M", limit: 50 }),
    ).toBe("History of DocOrder ORD-0000002, further back");
    expect(say({ target: { entity: "n1" }, before: "" })).toBe(
      "History of DocOrder ORD-0000002",
    );
    expect(say({ target: { entity: "n1" }, before: 7 })).toBe(
      "History of DocOrder ORD-0000002",
    );
    // A target the page can't read is an entity's, as evidence's is.
    expect(say({ target: { record: { key: "k1" } } })).toBe(
      "History of an entity",
    );
    expect(say(null)).toBe("History of an entity");
  });

  it("describes a changes read by its time range as written", () => {
    const say = (input: unknown) => stepInput(step("changes", input), known);
    expect(say({ limit: 10 })).toBe("Every change");
    expect(say(null)).toBe("Every change");
    expect(say({ since: "2026-10-01T00:00:00Z" })).toBe(
      "Changes since “2026-10-01T00:00:00Z”",
    );
    expect(say({ until: " 2026-10-20T12:00:00Z " })).toBe(
      "Changes until “2026-10-20T12:00:00Z”",
    );
    expect(
      say({ since: "2026-10-01T00:00:00Z", until: "2026-10-20T12:00:00Z" }),
    ).toBe("Changes between “2026-10-01T00:00:00Z” and “2026-10-20T12:00:00Z”");
    // A bound left blank or not a string is no bound.
    expect(say({ since: "  ", until: 3 })).toBe("Every change");
    // A page further on says so, without its transaction.
    expect(say({ since: "2026-10-01T00:00:00Z", after: "01JB2Q7X4M" })).toBe(
      "Changes since “2026-10-01T00:00:00Z”, continued",
    );
    expect(say({ after: "01JB2Q7X4M", limit: 20 })).toBe(
      "Every change, continued",
    );
    expect(say({ after: "" })).toBe("Every change");
    // Its heading and result read as every step's do.
    const read = step("changes", {}, "changes: 2 runs, one a build", "e4");
    expect([stepHeading(read), resultText(read)]).toEqual([
      "Read the changes",
      "2 runs, one a build",
    ]);
  });

  it("labels a result citation by the step that read it", () => {
    const count: OntologyQuery = { ...INVOICES, return: { count: {} } };
    const all: AskStep[] = [
      ...steps.slice(0, 3),
      step("query", INVOICES, "query: 3 of 3 rows", "e4"),
      { ...step("query", count, "query: 1 count", "e5"), query: count },
      step("explain", {}, "explain: 2 steps", "e6"),
      step("violations", {}, "violations: 1 records", "e7"),
      step("describe_type", {}, "describe_type: DocOrder", "e8"),
      step("new_tool", {}, "new_tool: x", "e9"),
    ];
    const ran = new Map<string, OntologyQuery>([
      [
        "e9",
        {
          ...INVOICES,
          return: {
            aggregate: { fn: "avg", at: 0, prop: "amount", group_by: null },
          },
        },
      ],
      ["e10", { ...INVOICES, return: { group_count: { at: 1, prop: "x" } } }],
    ]);
    expect(
      ["e1", "e2", "e3", "e4", "e5", "e6", "e7", "e8", "e9", "e10"].map((e) =>
        resultLabel(all, e, ran),
      ),
    ).toEqual([
      "The search in step 1",
      "The entity read in step 2",
      "The evidence read in step 3",
      "The query in step 4",
      "The count in step 5",
      "The explanation in step 6",
      "The rule breaks read in step 7",
      "The type read in step 8",
      "The result in step 9",
      "A step’s result",
    ]);
    // The query as the agent ran it decides its kind.
    const grouped = [
      step("query", INVOICES, "query: 2 of 2 groups", "e1"),
      step("query", INVOICES, "query: an aggregate", "e2"),
    ];
    expect(resultLabel(grouped, "e1", ran.set("e1", ran.get("e10")!))).toBe(
      "The counts in step 1",
    );
    expect(resultLabel(grouped, "e2", new Map([["e2", ran.get("e9")!]]))).toBe(
      "The average in step 2",
    );
    const history = [
      step("history_of", {}, "history_of: 3 steps", "e1"),
      step("changes", {}, "changes: 2 runs", "e2"),
    ];
    expect(resultLabel(history, "e1")).toBe("The history read in step 1");
    expect(resultLabel(history, "e2")).toBe("The changes read in step 2");
  });
});

describe("the load's overview and graph queries", () => {
  // A Helix v3 JSON read the model ran itself: the load's contracts.
  const READ = {
    count: {
      input: {
        nodes_where: {
          predicate: {
            and: {
              predicates: [
                {
                  eq: {
                    left: { property: "$label" },
                    right: { constant: { string: "Contract" } },
                  },
                },
                {
                  eq: {
                    left: { property: "foundry_load" },
                    right: { constant: { string: "load_1" } },
                  },
                },
              ],
            },
          },
        },
      },
    },
  };
  const stream = reduceEvents(EMPTY_STREAM, {
    events: [
      ev(1, { event: "started", model: "claude-sonnet-5-5" }),
      ev(2, call("p", "load_profile", {})),
      ev(3, result("p", "e1", "load_profile: 3 of 3 reads")),
      ev(4, call("g", "helix_query", { query: READ, limit: 50 })),
      ev(5, result("g", "e2", "helix_query: a value")),
      ev(6, call("h", "helix_query", { query: READ, limit: 50 })),
      ev(7, result("h", "e2", "helix_query: as e2")),
    ],
  });
  const [profile, graph, again] = stream.steps;

  it("records each as evidence with its summary, as any read", () => {
    expect(profile).toEqual({
      callId: "p",
      tool: "load_profile",
      input: {},
      result: {
        kind: "recorded",
        summary: "load_profile: 3 of 3 reads",
        evidenceId: "e1",
      },
    });
    expect(graph!.result).toEqual({
      kind: "recorded",
      summary: "helix_query: a value",
      evidenceId: "e2",
    });
    expect(again!.result).toEqual({
      kind: "repeated",
      summary: "helix_query: a value",
      evidenceId: "e2",
    });
    // A graph query is no typed query, whatever its input holds.
    expect(graph!.query).toBe(undefined);
    // The contract's tool_result carries no read: a field it doesn't name
    // is not kept.
    expect(
      parseAskEvent(
        JSON.stringify({
          event: "tool_result",
          call_id: "g",
          summary: "helix_query: a value",
          evidence_id: "e2",
          result: { count: 1 },
        }),
      ),
    ).toEqual({
      event: "tool_result",
      call_id: "g",
      summary: "helix_query: a value",
      evidence_id: "e2",
    });
  });

  it("heads each step by what it did, its result after", () => {
    expect(stream.steps.map((s) => [stepHeading(s), resultText(s)])).toEqual([
      ["Read what the load holds", "3 of 3 reads"],
      ["Queried the graph directly", "a value"],
      ["Queried the graph directly", "Same as an earlier step: a value"],
    ]);
    // The overview asks for nothing; a graph query says its limit, and
    // shows its code as is.
    expect(stepInput(profile!, NOTHING_KNOWN)).toBe(undefined);
    expect(stepInput(graph!, NOTHING_KNOWN)).toBe("At most 50 results");
    expect(
      stepInput({ ...graph!, input: { query: READ, limit: 1 } }, NOTHING_KNOWN),
    ).toBe("At most 1 result");
    expect(
      stepInput({ ...graph!, input: { query: READ } }, NOTHING_KNOWN),
    ).toBe(undefined);
    expect(
      stepInput(
        { ...graph!, input: { query: READ, limit: -1 } },
        NOTHING_KNOWN,
      ),
    ).toBe(undefined);
    expect(JSON.parse(graphQuery(graph!)!)).toEqual(READ);
    expect(graphQuery(graph!)).toBe(JSON.stringify(READ, null, 2));
    expect(graphQuery(profile!)).toBe(undefined);
    expect(graphQuery({ ...graph!, input: { limit: 5 } })).toBe(undefined);
    expect(graphQuery({ ...graph!, input: "count(nodes)" })).toBe(undefined);
    expect(
      graphQuery({ ...graph!, tool: "query", input: { query: READ } }),
    ).toBe(undefined);
  });

  it("as its examples say", () => {
    expect(
      graphQuery({
        callId: "a",
        tool: "helix_query",
        input: { query: { count: { input: { nodes: { reference: "all" } } } } },
      }),
    ).toBe(
      '{\n  "count": {\n    "input": {\n      "nodes": {\n        "reference": "all"\n      }\n    }\n  }\n}',
    );
    expect(
      graphQuery({ callId: "b", tool: "query", input: { query: {} } }),
    ).toBe(undefined);
    expect(
      resultLabel(
        [
          {
            callId: "p",
            tool: "load_profile",
            input: {},
            result: {
              kind: "recorded",
              summary: "load_profile: 4 of 4 reads",
              evidenceId: "e1",
            },
          },
        ],
        "e1",
      ),
    ).toBe("The overview of the load in step 1");
  });

  it("names a result citation of either by its step, which it lights", () => {
    expect(resultLabel(stream.steps, "e1")).toBe(
      "The overview of the load in step 1",
    );
    expect(resultLabel(stream.steps, "e2")).toBe("The graph query in step 2");
    // The step a citation of e1 lights is the overview's, and only it.
    expect(
      stream.steps.flatMap((s, i) =>
        s.result?.kind === "recorded" && s.result.evidenceId === "e1"
          ? [i]
          : [],
      ),
    ).toEqual([0]);
    const ctx = {
      result: (e: string) => resultLabel(stream.steps, e),
    } as CitationContext;
    expect(citationText({ result: { evidence_id: "e1" } }, ctx)).toBe(
      "The overview of the load in step 1",
    );
    expect(citationHref({ result: { evidence_id: "e1" } }, ctx)).toBe(
      undefined,
    );
  });
});

describe("the query renderer", () => {
  it("reads a typed query as its path, filters and rows", () => {
    expect(queryText(INVOICES)).toEqual({
      path: 'Invoice where status = "open" → billed_to → Customer',
      filters: [
        "Invoice holds overdue",
        "Customer doesn’t hold blocked",
        "Customer breaks credit_limit",
      ],
      returns:
        "Rows: Invoice, Invoice.amount, Customer.legal_name · sorted by Invoice.amount, highest first · at most 50",
    });
  });

  it("reads every condition, inbound steps, ids, text and relative dates", () => {
    const q: OntologyQuery = {
      start: {
        type: "Customer",
        ids: ["cus_1", "cus_2"],
        text: "acme",
        where: [
          { prop: "closed_at", op: "is_null" },
          { prop: "email", op: "not_null" },
          { prop: "name", op: "contains", value: "Corp" },
          { prop: "name", op: "starts_with", value: "Glo" },
          { prop: "since", op: "<", value: "today-30d" },
          { prop: "tier", op: ">=", value: 2 },
          { prop: "vip", op: "!=", value: true },
        ],
      },
      steps: [
        { via: "billed_to", dir: "in", to: "Invoice" },
        {
          via: "billed_to",
          dir: "out",
          to: "Customer",
          where: [{ prop: "x", op: "=", value: null }],
        },
      ],
      return: { count: { distinct_at: 2 } },
    };
    const t = queryText(q);
    expect(t.path).toBe(
      'Customer@0 (cus_1, cus_2) matching "acme" where closed_at is empty and email is set and name contains "Corp" and name starts with "Glo" and since < today-30d and tier >= 2 and vip != true ← billed_to ← Invoice → billed_to → Customer@2 where x = null',
    );
    expect(t.filters).toEqual([]);
    expect(t.returns).toBe("Count of distinct Customer@2");
    // A count at the last position, as the model may write it.
    for (const count of [{}, { distinct_at: null }])
      expect(queryText({ ...q, return: { count } }).returns).toBe(
        "Count of distinct Customer@2",
      );
    expect(contractErrors("read_request#/$defs/Query", q)).toEqual([]);
  });

  it("reads groups and aggregates", () => {
    const base = {
      start: { type: "Invoice" },
      steps: [{ via: "billed_to", dir: "out" as const, to: "Customer" }],
    };
    expect(
      queryText({
        ...base,
        return: { group_count: { at: 1, prop: "industry", limit: 50 } },
      }).returns,
    ).toBe("Count by Customer.industry · top 50");
    expect(
      queryText({
        ...base,
        return: { group_count: { at: 1, prop: "industry" } },
      }).returns,
    ).toBe("Count by Customer.industry");
    expect(
      queryText({
        ...base,
        return: {
          aggregate: {
            fn: "sum",
            at: 0,
            prop: "amount",
            group_by: { at: 1, prop: "legal_name" },
          },
        },
      }).returns,
    ).toBe("Sum of Invoice.amount by Customer.legal_name");
    expect(
      queryText({
        ...base,
        return: { aggregate: { fn: "avg", at: 0, prop: "amount" } },
      }).returns,
    ).toBe("Average of Invoice.amount");
    expect(
      queryText({
        start: { type: "A" },
        return: { rows: { columns: [{ at: 3 }] } },
      }).returns,
    ).toBe("Rows: position 3");
  });

  it("parses typed queries and refuses what isn't one", () => {
    expect(contractErrors("read_request#/$defs/Query", INVOICES)).toEqual([]);
    expect(parseQuery(INVOICES)).toEqual(INVOICES);
    expect(
      parseQuery({
        start: { type: "A" },
        return: { aggregate: { fn: "sum", at: 0, prop: "x", limit: 5 } },
      }),
    ).toEqual({
      start: { type: "A", where: [], ids: null, text: null },
      steps: [],
      filters: [],
      return: {
        aggregate: { fn: "sum", at: 0, prop: "x", group_by: null, limit: 5 },
      },
    });
    expect(
      parseQuery({ start: { type: "A" }, return: { count: {} } })?.return,
    ).toEqual({ count: { distinct_at: null } });
    // An order needs its prop.
    expect(
      parseQuery({
        start: { type: "A" },
        return: { rows: { columns: [{ at: 0 }], order_by: { at: 0 } } },
      })?.return,
    ).toEqual({ rows: { columns: [{ at: 0 }], order_by: null } });
    for (const bad of [
      null,
      { start: { type: "A" } },
      { start: { type: "A" }, return: { median: {} } },
      {
        start: { type: "A", where: [{ prop: "x", op: "~" }] },
        return: { count: { distinct_at: 0 } },
      },
      {
        start: { type: "A" },
        steps: [{ via: "l", dir: "up", to: "B" }],
        return: { count: { distinct_at: 0 } },
      },
      {
        start: { type: "A" },
        steps: "x",
        return: { count: { distinct_at: 0 } },
      },
      {
        start: { type: "A" },
        filters: [{ at: -1, holds: "x" }],
        return: { count: { distinct_at: 0 } },
      },
      {
        start: { type: "A" },
        filters: [{ at: 0, maybe: "x" }],
        return: { count: { distinct_at: 0 } },
      },
      { start: { type: "A" }, return: { rows: { columns: [{ at: 1.5 }] } } },
      { start: { type: "A" }, return: { group_count: { at: 0 } } },
      { start: { type: "A" }, return: { count: { distinct_at: -1 } } },
    ])
      expect(parseQuery(bad), JSON.stringify(bad)).toBe(undefined);
  });
});

const CITES: AskCitation[] = [
  {
    marker: "c1",
    evidence_id: "e1",
    target: { entity: { type: "Customer", id: "cus_1" } },
  },
  {
    marker: "c2",
    evidence_id: "e2",
    target: { quote: { doc_id: "doc_1", section: 2, start: 10, end: 40 } },
  },
  {
    marker: "c3",
    evidence_id: "e3",
    target: { result: { evidence_id: "e3" } },
  },
];

describe("the answer renderer", () => {
  it("renders plain text with citation chips numbered by first use", () => {
    const v = answerView(
      [
        { para: "Globex [c2] signed it [c1], twice [c2]." },
        { list: ["By query [c3]", "", "[c9]"] },
      ],
      CITES,
    );
    expect(v.blocks).toEqual([
      {
        at: 0,
        kind: "para",
        parts: [
          { text: "Globex" },
          { cite: 1 },
          { text: "signed it" },
          { cite: 2 },
          { text: ", twice" },
          { cite: 1 },
          { text: "." },
        ],
      },
      { at: 1, kind: "list", items: [[{ text: "By query" }, { cite: 3 }]] },
    ]);
    expect(v.sources.map((s) => [s.n, s.citation.marker])).toEqual([
      [1, "c2"],
      [2, "c1"],
      [3, "c3"],
    ]);
  });

  it("drops markup: HTML, images and links from the model are never rendered", () => {
    const v = answerView(
      [
        {
          para: '<img src="x" onerror="alert(1)"><script>alert(2)</script> ![logo](https://e.com/x.png) [Read more](https://example.com/phish) **now** `code` __bold__ [c1]',
        },
        { para: '## Heading\n<a href="https://evil.example">click</a>' },
        { para: "<b></b>" },
        { list: ["<i>[c1]</i>"] },
      ],
      CITES,
    );
    const text = JSON.stringify(v.blocks);
    for (const banned of ["<", ">", "https://", "](", "**", "`", "__", "##"])
      expect(text).not.toContain(banned);
    // The empty block is left out; the others keep their places.
    expect(v.blocks).toEqual([
      {
        at: 0,
        kind: "para",
        parts: [{ text: "alert(2) Read more now code bold" }, { cite: 1 }],
      },
      { at: 1, kind: "para", parts: [{ text: "Heading\nclick" }] },
      { at: 3, kind: "list", items: [[{ cite: 1 }]] },
    ]);
  });

  it("keeps plain text that only looks like markup", () => {
    expect(plainText("5 < 6 and 7 > 3, a*b, see [c1] (above)")).toBe(
      "5 < 6 and 7 > 3, a*b, see [c1] (above)",
    );
  });
});

describe("results", () => {
  it("parses a result and leaves out what doesn't parse", () => {
    const answered = {
      status: "answered",
      answer: { blocks: [{ para: "A [c1]" }, { list: ["x"] }] },
      citations: CITES,
      queries: [{ evidence_id: "e3", query: INVOICES_RAN }],
      usage: { ...USAGE, input: 10, usd: 0.07 },
    };
    expect(contractErrors("ask_result", answered)).toEqual([]);
    // A query's columns leave out a null prop: the entity itself.
    const parsed = <T extends { queries: { query: OntologyQuery }[] }>(
      x: T,
    ) => ({
      ...x,
      queries: x.queries.map((r) => ({ ...r, query: parseQuery(r.query) })),
    });
    expect(parseAskResult(JSON.stringify(answered))).toEqual(parsed(answered));
    const r = parseAskResult(
      JSON.stringify({
        ...answered,
        answer: {
          blocks: [{ para: "A [c1]" }, { list: ["x", 3] }, { img: "u" }, 4],
        },
        citations: [
          ...CITES,
          { marker: "c4", evidence_id: "e1", target: { link: "https://e" } },
          {
            marker: "c5",
            evidence_id: "e1",
            target: { entity: { type: "A" } },
          },
          { marker: "c6", target: { result: { evidence_id: "e1" } } },
          "x",
        ],
        queries: [...answered.queries, { evidence_id: "e4", query: {} }],
        usage: { usd: 0.07, output: "many" },
      }),
    )!;
    expect(r).toEqual({
      ...parsed(answered),
      usage: { ...USAGE, usd: 0.07 },
    });
    // A recorded version is kept; one without a time is left out, as any
    // target that doesn't parse is.
    const recorded: AskCitation = {
      marker: "c4",
      evidence_id: "e4",
      target: {
        version: {
          tx: "01JB3Z8Q6WJ4X7M2N5P9R0S1T2",
          at: "2026-10-03T09:14:07Z",
          of: {
            prop: { entity: { type: "Customer", id: "cus_1" }, prop: "tier" },
          },
        },
      },
    };
    expect(
      parseAskResult(
        JSON.stringify({
          ...answered,
          citations: [
            ...CITES,
            recorded,
            {
              marker: "c5",
              evidence_id: "e4",
              target: {
                version: { tx: "01JB", of: { entity: { type: "A", id: "a" } } },
              },
            },
          ],
        }),
      ),
    ).toEqual({ ...parsed(answered), citations: [...CITES, recorded] });
    expect(parseAskResult('{"status":"unknown"}')).toBe(undefined);
    expect(parseAskResult("nope")).toBe(undefined);
    expect(parseAskResult('{"status":"cannot_answer","gap":"A link"}')).toEqual(
      {
        status: "cannot_answer",
        gap: "A link",
        reason: "",
        queries: [],
        usage: USAGE,
      },
    );
    // Each status's fields, as the engine writes them.
    for (const ended of [
      {
        status: "cannot_answer",
        reason: "No link says who signed.",
        gap: "A signed_by link.",
        queries: [],
        usage: USAGE,
      },
      {
        status: "budget_exhausted",
        queries: [{ evidence_id: "e2", query: INVOICES_RAN }],
        usage: USAGE,
      },
      { status: "failed", reason: "store_failed", queries: [], usage: USAGE },
    ]) {
      expect(contractErrors("ask_result", ended)).toEqual([]);
      expect(parseAskResult(JSON.stringify(ended))).toEqual(parsed(ended));
    }
    // A failure without a reason it knows is no result to trust.
    expect(parseAskResult('{"status":"failed","reason":"gremlins"}')).toBe(
      undefined,
    );
  });

  it("says what a question spent", () => {
    expect(
      usageText({
        input: 6_100,
        cache_read: 12_000,
        cache_write: 0,
        output: 1_450,
        usd: 0.0734,
      }),
    ).toBe("$0.07 · 18,100 tokens in (12,000 cached), 1,450 out");
    expect(usageText(USAGE)).toBe("$0.00 · 0 tokens in, 0 out");
  });

  it("parses every citation target and refuses others", () => {
    const e = { type: "Customer", id: "cus_1" };
    for (const t of [
      { entity: e },
      { fact: { link: "billed_to", subject: e, object: e } },
      { derived: { pred: "derived:blocked", args: [e] } },
      { row: { source: "src_1", mapping: "customers", row_key: "cus_1" } },
      // A People enrichment profile, cited without its id (3.12.0).
      { row: { source: "foundry_enrichment", mapping: "people", row_key: "" } },
      { quote: { doc_id: "d", section: 0, start: 0, end: 4 } },
      { violation: { constraint: "credit_limit", key: "9f2c" } },
      { result: { evidence_id: "e1" } },
    ]) {
      expect(contractErrors("ask_result#/$defs/Target", t)).toEqual([]);
      expect(parseTarget(t)).toEqual(t);
    }
    for (const t of [
      null,
      { entity: e, quote: {} },
      { entity: "x" },
      { fact: { link: "l", subject: e } },
      { derived: { pred: "p", args: [{}] } },
      { row: { source: "s" } },
      { row: { source: "s", mapping: "m", row_key: "" } },
      // A profile's id never reaches a stored result.
      {
        row: {
          source: "foundry_enrichment",
          mapping: "people",
          row_key: "li:ada",
        },
      },
      { quote: { doc_id: "d", section: 0, start: 4, end: 4 } },
      { quote: { doc_id: "d", start: 0, end: 4 } },
      { violation: { constraint: "c" } },
      { violation: { constraint: "c", entity: e } },
      { result: {} },
      { url: { href: "https://e.com" } },
    ])
      expect(parseTarget(t), JSON.stringify(t)).toBe(undefined);
    expect(parseTarget({ row: { source: "s", row_key: "k" } })).toEqual({
      row: { source: "s", mapping: "", row_key: "k" },
    });
    expect(parseTarget({ row: { source: "foundry_enrichment" } })).toEqual({
      row: { source: "foundry_enrichment", mapping: "", row_key: "" },
    });
  });

  // A recorded version (the engine's Target::Version). The vendored contract
  // predates it, so these are held to its shape here, not to the schema.
  it("parses a version of each kind and refuses a malformed one", () => {
    const e = { type: "Customer", id: "cus_1" },
      msa = { type: "Contract", id: "msa" },
      tx = "01JB3Z8Q6WJ4X7M2N5P9R0S1T2",
      at = "2026-10-03T09:14:07.123456Z";
    const version = (of: unknown, v: object = {}) => ({
      version: { tx, at, of, ...v },
    });
    for (const of of [
      { entity: e },
      { prop: { entity: e, prop: "tier" } },
      { fact: { link: "signed_with", subject: msa, object: e } },
      { record: { constraint: "must_sign", key: "9f2c" } },
    ])
      expect(parseTarget(version(of))).toEqual(version(of));
    // Every offset and case RFC 3339 allows.
    for (const when of [
      "2026-10-03T09:14:07Z",
      "2026-10-03t09:14:07z",
      "2026-10-03T09:14:07+02:00",
      "2026-10-03T09:14:07.5-05:30",
    ])
      expect(parseTarget(version({ entity: e }, { at: when }))).toEqual(
        version({ entity: e }, { at: when }),
      );
    // What the page doesn't read is left out, as other targets' is.
    expect(
      parseTarget(version({ entity: e }, { by: "model", of: { entity: e } })),
    ).toEqual(version({ entity: e }));
    for (const t of [
      { version: null },
      { version: [] },
      { version: { at, of: { entity: e } } },
      version({ entity: e }, { tx: "" }),
      version({ entity: e }, { tx: 7 }),
      { version: { tx, of: { entity: e } } },
      version({ entity: e }, { at: "" }),
      version({ entity: e }, { at: "yesterday" }),
      version({ entity: e }, { at: 1791018847 }),
      // A day, a time without an offset, a month that isn't one.
      version({ entity: e }, { at: "2026-10-03" }),
      version({ entity: e }, { at: "2026-10-03T09:14:07" }),
      version({ entity: e }, { at: "2026-13-03T09:14:07Z" }),
      version({ entity: e }, { at: "2026-10-03T25:14:07Z" }),
      { version: { tx, at } },
      version(null),
      version("entity"),
      version({}),
      version({ entity: e, record: { constraint: "c", key: "k" } }),
      // A handle the model wrote, not an entity.
      version({ entity: "n1" }),
      version({ entity: { type: "Customer" } }),
      version({ prop: { entity: e } }),
      version({ prop: { entity: e, prop: "" } }),
      version({ prop: { entity: { id: "cus_1" }, prop: "tier" } }),
      version({ prop: "tier" }),
      version({ fact: { link: "signed_with", subject: msa } }),
      version({ fact: { subject: msa, object: e } }),
      version({ record: { constraint: "must_sign" } }),
      version({ record: { key: "9f2c" } }),
      // A record is `record`, not the target's `violation`.
      version({ violation: { constraint: "must_sign", key: "9f2c" } }),
      version({ row: { source: "s", mapping: "m", row_key: "k" } }),
      version({ quote: { doc_id: "d", section: 0, start: 0, end: 4 } }),
      version({ derived: { pred: "derived:p", args: [e] } }),
      version({ result: { evidence_id: "e1" } }),
      version(version({ entity: e })),
      { version: { tx, at, of: { entity: e } }, entity: e },
    ])
      expect(parseTarget(t), JSON.stringify(t)).toBe(undefined);
  });
});

describe("citations", () => {
  const ctx: CitationContext = {
    ontologyId: "ont_1",
    loadId: "load_1",
    keyed: (t) =>
      t === "Customer" ? true : t === "Contract" ? false : undefined,
    docName: (id) => (id === "doc_1" ? "contract.md" : id),
    table: (s) =>
      s === "src_1"
        ? {
            to: "/data/connections/conn_1/tables/customers",
            table: "customers",
            connector: "Stripe",
          }
        : undefined,
    name: (r) => (r.id === "master services agreement" ? "MSA" : r.id),
    result: (e) => `The query in step ${e.slice(1)}`,
  };
  const cus = { type: "Customer", id: "cus_1" },
    msa = { type: "Contract", id: "master services agreement" };

  it("opens an entity's page in the question's load, by key or by name", () => {
    const href = citationHref({ entity: cus }, ctx)!;
    expect(href).toMatch(/^\/explore\/ont_1\/entities\/[\w-]+\?load=load_1$/);
    expect(parseEntityId(href.split("/")[4]!.split("?")[0]!)).toEqual({
      type: "Customer",
      key: "cus_1",
    });
    const named = citationHref(
      { fact: { link: "signed_with", subject: msa, object: cus } },
      ctx,
    )!;
    expect(named).toMatch(/\?tab=facts&load=load_1$/);
    expect(parseEntityId(named.split("/")[4]!.split("?")[0]!)).toEqual({
      type: "Contract",
      name: "master services agreement",
    });
    // A derived fact opens its first entity's Derived tab.
    expect(
      citationHref({ derived: { pred: "derived:p", args: [cus] } }, ctx),
    ).toMatch(/\/entities\/[\w-]+\?tab=derived&load=load_1$/);
    expect(citationHref({ derived: { pred: "p", args: [] } }, ctx)).toBe(
      undefined,
    );
    // A rule break the rules find now opens its first entity's Needs review.
    expect(
      citationHref(
        { derived: { pred: "violation:credit_limit", args: [cus] } },
        ctx,
      ),
    ).toMatch(/\/entities\/[\w-]+\?tab=reviews&load=load_1$/);
    // A violation opens the load's rule breaks of its constraint, at it.
    expect(
      citationHref(
        { violation: { constraint: "credit_limit", key: "9f2c" } },
        ctx,
      ),
    ).toBe(
      "/ontology/ont_1/loads/load_1/rule-breaks?constraint=credit_limit&key=9f2c",
    );
    // A type the version doesn't know has no page to open.
    expect(citationHref({ entity: { type: "Ghost", id: "g" } }, ctx)).toBe(
      undefined,
    );
  });

  it("opens a quote's span, a row's table, and a result in the page", () => {
    expect(
      citationHref(
        { quote: { doc_id: "doc_1", section: 2, start: 10, end: 40 } },
        ctx,
      ),
    ).toBe("/ontology/ont_1/documents/doc_1?load=load_1#s2-10-40");
    expect(
      citationHref(
        { row: { source: "src_1", mapping: "m", row_key: "cus_1" } },
        ctx,
      ),
    ).toBe("/data/connections/conn_1/tables/customers");
    expect(
      citationHref(
        { row: { source: "src_x", mapping: "m", row_key: "k" } },
        ctx,
      ),
    ).toBe(undefined);
    // A People enrichment profile opens the load that applied it.
    expect(
      citationHref(
        {
          row: {
            source: "foundry_enrichment",
            mapping: "enrichment_people",
            row_key: "",
          },
        },
        ctx,
      ),
    ).toBe("/ontology/ont_1/loads/load_1");
    expect(citationHref({ result: { evidence_id: "e1" } }, ctx)).toBe(
      undefined,
    );
  });

  it("lists the entities citations name, once each", () => {
    const inv = { type: "Invoice", id: "in_1" };
    const cite = (target: AskCitation["target"]): AskCitation => ({
      marker: "c",
      evidence_id: "e1",
      target,
    });
    expect(
      citedEntities([
        cite({ entity: cus }),
        cite({ fact: { link: "signed_with", subject: msa, object: cus } }),
        cite({ derived: { pred: "relation:co_signed", args: [cus, inv] } }),
        cite({ derived: { pred: "violation:credit_limit", args: [inv] } }),
        cite({ row: { source: "src_1", mapping: "m", row_key: "cus_1" } }),
        cite({ quote: { doc_id: "doc_1", section: 0, start: 0, end: 1 } }),
        cite({ violation: { constraint: "credit_limit", key: "9f2c" } }),
        cite({ result: { evidence_id: "e1" } }),
      ]),
    ).toEqual([cus, msa, inv]);
    // A recorded version names what its subject names; a record, nothing.
    const rec = { type: "Customer", id: "cus_2" },
      ord = { type: "Order", id: "ord 9" },
      at = "2026-10-03T09:14:07Z";
    expect(
      citedEntities([
        cite({ version: { tx: "01JB", at, of: { entity: rec } } }),
        cite({
          version: {
            tx: "01JB",
            at,
            of: { prop: { entity: ord, prop: "total" } },
          },
        }),
        cite({
          version: {
            tx: "01JB",
            at,
            of: { fact: { link: "signed_with", subject: msa, object: rec } },
          },
        }),
        cite({
          version: {
            tx: "01JB",
            at,
            of: { record: { constraint: "credit_limit", key: "9f2c" } },
          },
        }),
      ]),
    ).toEqual([rec, ord, msa]);
    expect(citedEntities([])).toEqual([]);
    // As its example says.
    const order = { type: "Order", id: "ord 2" },
      ada = { type: "Customer", id: "cus_1" };
    expect(
      citedEntities([
        cite({ entity: order }),
        cite({ fact: { link: "placed_by", subject: order, object: ada } }),
      ]),
    ).toEqual([order, ada]);
  });

  it("names every entity a citation names as its page does", () => {
    // The pages' names of a keyed customer and a named contract.
    const named = knownOf(
      [],
      [],
      new Map([
        [refKey(cus), "Globex Corporation"],
        [refKey(msa), "Master services agreement"],
      ]),
    );
    const paged = { ...ctx, name: named.name };
    expect(
      [
        { entity: cus },
        { fact: { link: "signed_with", subject: msa, object: cus } },
        { derived: { pred: "relation:co_signed", args: [cus, msa] } },
        { derived: { pred: "violation:credit_limit", args: [cus] } },
        {
          version: {
            tx: "01JB",
            at: "2026-10-03T09:14:07Z",
            of: { prop: { entity: cus, prop: "tier" } },
          },
        },
        { entity: { type: "Customer", id: "cus_2" } },
      ].map((t) => citationText(t, paged)),
    ).toEqual([
      "Customer Globex Corporation",
      "Contract Master services agreement signed_with Customer Globex Corporation",
      "co signed: Customer Globex Corporation, Contract Master services agreement (derived)",
      "A break of credit_limit: Customer Globex Corporation",
      `As recorded ${formatDate("2026-10-03T09:14:07Z")}: the tier of Customer Globex Corporation`,
      // A page that can't name it keeps its key.
      "Customer cus_2",
    ]);
  });

  it("names every citation", () => {
    expect(
      [
        { entity: cus },
        { fact: { link: "signed_with", subject: msa, object: cus } },
        { derived: { pred: "relation:co_signed", args: [cus, cus] } },
        { derived: { pred: "violation:credit_limit", args: [cus, msa] } },
        { row: { source: "src_1", mapping: "m", row_key: "cus_1" } },
        { row: { source: "src_x", mapping: "m", row_key: "k" } },
        {
          row: {
            source: "foundry_enrichment",
            mapping: "enrichment_people",
            row_key: "",
          },
        },
        {
          row: {
            source: "foundry_enrichment",
            mapping: "enrichment_people_experience",
            row_key: "",
          },
        },
        { quote: { doc_id: "doc_1", section: 0, start: 0, end: 1 } },
        { violation: { constraint: "credit_limit", key: "9f2c" } },
        { result: { evidence_id: "e1" } },
      ].map((t) => citationText(t, ctx)),
    ).toEqual([
      "Customer cus_1",
      "Contract MSA signed_with Customer cus_1",
      "co signed: Customer cus_1, Customer cus_1 (derived)",
      "A break of credit_limit: Customer cus_1, Contract MSA",
      "Stripe · customers row cus_1",
      "A table row k",
      "A People enrichment profile",
      "A People enrichment profile",
      "Quote in contract.md",
      "A break of credit_limit",
      "The query in step 1",
    ]);
  });

  describe("a recorded version", () => {
    const at = "2026-10-03T09:14:07Z";
    const version = (of: VersionOf): CitationTarget => ({
      version: { tx: "01JB3Z8Q6WJ4X7M2N5P9R0S1T2", at, of },
    });
    const ofEntity = version({ entity: msa }),
      ofProp = version({ prop: { entity: cus, prop: "tier" } }),
      ofFact = version({
        fact: { link: "signed_with", subject: msa, object: cus },
      }),
      ofRecord = version({
        record: { constraint: "credit_limit", key: "9f2c" },
      });

    it("names when it was recorded and what of", () => {
      expect(
        [ofEntity, ofProp, ofFact, ofRecord].map((t) => citationText(t, ctx)),
      ).toEqual(
        [
          "Contract MSA",
          "the tier of Customer cus_1",
          "Contract MSA signed_with Customer cus_1",
          "a break of credit_limit",
        ].map((what) => `As recorded ${formatDate(at)}: ${what}`),
      );
      // The reader's own clock and words, not the engine's text.
      expect(citationText(ofEntity, ctx)).not.toContain(at);
    });

    it("opens its subject as the load holds it now, and a record nothing", () => {
      expect(citationHref(ofEntity, ctx)).toBe(
        citationHref({ entity: msa }, ctx),
      );
      expect(citationHref(ofEntity, ctx)).toMatch(/\?load=load_1$/);
      // A prop opens its entity's page, where the entity's props are.
      expect(citationHref(ofProp, ctx)).toBe(
        citationHref({ entity: cus }, ctx),
      );
      // A fact opens its subject's Facts, as a fact does.
      expect(citationHref(ofFact, ctx)).toMatch(/\?tab=facts&load=load_1$/);
      expect(
        parseEntityId(citationHref(ofFact, ctx)!.split("/")[4]!.split("?")[0]!),
      ).toEqual({ type: "Contract", name: "master services agreement" });
      // The record may be gone from the load's rule breaks.
      expect(citationHref(ofRecord, ctx)).toBe(undefined);
      // A type the version doesn't know has no page, whatever the version.
      const ghost = { type: "Ghost", id: "g" };
      expect(citationHref(version({ entity: ghost }), ctx)).toBe(undefined);
      expect(
        citationHref(version({ prop: { entity: ghost, prop: "p" } }), ctx),
      ).toBe(undefined);
    });
  });
});

describe("questions and threads", () => {
  const q = (over: Partial<FoundryQuestion>): FoundryQuestion => ({
    id: "q1",
    threadId: "q1",
    status: "queued",
    createdAt: "2026-10-19T10:00:00Z",
    ...over,
  });
  const budget = (spent: number, reserved = 0) => ({
    monthlyUsdMicros: "30000000",
    spentUsdMicros: String(spent),
    reservedUsdMicros: String(reserved),
    questionUsdMicros: "500000",
  });

  it("says a queued question waits once the month can't hold its reservation", () => {
    expect(budgetFull(budget(29_000_000, 500_000))).toBe(false);
    expect(budgetFull(budget(29_000_000, 500_001))).toBe(true);
    expect(budgetFull(null)).toBe(false);
    expect(questionView(q({}), budget(29_600_000))).toEqual({
      kind: "waiting",
    });
    expect(questionView(q({}), budget(0))).toEqual({ kind: "queued" });
    // A queued question with attempts is waiting to be retried, and says why.
    expect(
      questionView(
        q({
          attempts: 1,
          error: "the question stopped before an answer; Helix will retry",
        }),
        budget(29_600_000),
      ),
    ).toEqual({
      kind: "retrying",
      error: "The question stopped before an answer; Helix will retry.",
    });
    expect(questionView(q({ attempts: 2 }))).toEqual({
      kind: "retrying",
      error: "The last attempt stopped.",
    });
    expect(questionView(q({ status: "running" }))).toEqual({ kind: "running" });
    expect(questionView(q({ status: "cancelled" }))).toEqual({
      kind: "cancelled",
    });
  });

  it("tells how a question ended from its result", () => {
    const res = (status: string) =>
      JSON.stringify({ status, queries: [], usage: USAGE });
    expect(
      questionView(q({ status: "answered", resultJson: res("answered") })).kind,
    ).toBe("answered");
    expect(
      questionView(
        q({ status: "cannot_answer", resultJson: res("cannot_answer") }),
      ).kind,
    ).toBe("cannot_answer");
    // The backend records a budget stop as failed; its result says why.
    expect(
      questionView(q({ status: "failed", resultJson: res("budget_exhausted") }))
        .kind,
    ).toBe("budget_exhausted");
    // A failure says the engine's reason.
    const failure = {
      status: "failed",
      reason: "model_refused",
      queries: [],
      usage: USAGE,
    } as const;
    expect(
      questionView(
        q({
          status: "failed",
          error: "the model declined to answer; ask another way",
          resultJson: JSON.stringify(failure),
        }),
      ),
    ).toEqual({
      kind: "failed",
      error: "The model declined to answer.",
      result: failure,
    });
    // Helix's Claude's model failures read as they did before contract
    // 4.0.0 split them out of model_failed: the WFE's lowercase words for
    // each say it is unavailable.
    const house = "the model could not be reached; ask again";
    for (const reason of [
      "model_failed",
      "model_auth",
      "model_permission",
      "model_not_found",
      "model_rate_limited",
      "model_quota",
      "model_unsupported",
      "model_truncated",
      "model_malformed",
      "model_timeout",
    ] as const) {
      const r = { ...failure, reason };
      expect(
        questionView(
          q({ status: "failed", error: house, resultJson: JSON.stringify(r) }),
        ),
      ).toEqual({
        kind: "failed",
        error: "The model couldn’t be reached, after retries.",
        result: r,
      });
    }
    // A workspace's own provider's are the WFE's full sentences, shown as
    // written: they name the provider, and may start with a model's name.
    const auth = { ...failure, reason: "model_auth" } as const;
    const unsupported = { ...failure, reason: "model_unsupported" } as const;
    expect(
      questionView(
        q({
          status: "failed",
          error:
            "claude-sonnet-5-5 can’t do what this task needs (tool calls or structured answers).",
          resultJson: JSON.stringify(unsupported),
        }),
      ),
    ).toEqual({
      kind: "failed",
      error:
        "claude-sonnet-5-5 can’t do what this task needs (tool calls or structured answers).",
      result: unsupported,
    });
    expect(
      questionView(
        q({
          status: "failed",
          error:
            "Claude refused the API key. An owner can enter a new one in Settings › AI models.",
          resultJson: JSON.stringify(auth),
        }),
      ),
    ).toEqual({
      kind: "failed",
      error:
        "Claude refused the API key. An owner can enter a new one in Settings › AI models.",
      result: auth,
    });
    expect(
      questionView(
        q({ status: "failed", error: "", resultJson: JSON.stringify(auth) }),
      ),
    ).toEqual({
      kind: "failed",
      error: "The model couldn’t be reached, after retries.",
      result: auth,
    });
    // The worker's words, as a sentence.
    expect(
      questionView(
        q({
          status: "failed",
          error: "the workspace has spent this month's $30.00 of questions",
        }),
      ),
    ).toEqual({
      kind: "failed",
      error: "The workspace has spent this month's $30.00 of questions.",
    });
    expect(questionView(q({ status: "failed" }))).toEqual({
      kind: "failed",
      error: "It stopped before answering.",
    });
    expect(questionView(q({ status: "answered", resultJson: "{" }))).toEqual({
      kind: "failed",
      error: "Its answer couldn’t be read.",
    });
  });

  it("groups questions into threads, newest activity first", () => {
    const threads = threadsOf([
      q({
        id: "q3",
        threadId: "q1",
        createdAt: "2026-10-19T12:00:00Z",
        status: "running",
      }),
      q({
        id: "q2",
        threadId: "q2",
        createdAt: "2026-10-19T11:00:00Z",
        status: "answered",
      }),
      q({
        id: "q1",
        threadId: "q1",
        createdAt: "2026-10-19T10:00:00Z",
        status: "answered",
      }),
      q({
        id: "q0",
        threadId: undefined,
        createdAt: "2026-10-19T09:00:00Z",
        status: "answered",
      }),
    ]);
    expect(
      threads.map((t) => [t.id, t.questions.map((x) => x.id), t.live]),
    ).toEqual([
      ["q1", ["q1", "q3"], true],
      ["q2", ["q2"], false],
      ["q0", ["q0"], false],
    ]);
  });

  it("drafts a goal from a question the ontology can't answer", () => {
    expect(
      draftGoal(" Who signed it? ", {
        status: "cannot_answer",
        gap: "A **signed_by** link",
        reason: "No link records it.",
        queries: [],
        usage: USAGE,
      }),
    ).toBe(
      "Make the ontology able to answer: “Who signed it?”\nIt lacks: A signed_by link\nWhy it couldn’t: No link records it.",
    );
    expect(
      draftGoal("Q", {
        status: "cannot_answer",
        gap: "",
        reason: "",
        queries: [],
        usage: USAGE,
      }),
    ).toBe("Make the ontology able to answer: “Q”");
  });

  it("reads what a question was asked about from its focusJson", () => {
    expect(
      questionFocus(q({ focusJson: '{"type":"Customer","id":"cus_1"}' })),
    ).toEqual({ type: "Customer", id: "cus_1" });
    expect(questionFocus(q({}))).toBe(undefined);
    expect(questionFocus(q({ focusJson: "" }))).toBe(undefined);
    expect(questionFocus(q({ focusJson: "{" }))).toBe(undefined);
    expect(questionFocus(q({ focusJson: '{"type":"Customer"}' }))).toBe(
      undefined,
    );
    expect(questionFocus(q({ focusJson: '["Customer","cus_1"]' }))).toBe(
      undefined,
    );
  });
});

describe("the threads panel", () => {
  const at = (id: string, last: Date | string): Thread => ({
    id,
    questions: [],
    last: typeof last === "string" ? last : last.toISOString(),
    live: false,
  });
  const groups = (threads: Thread[], now: Date) =>
    threadGroups(threads, now).map((g) => [
      g.label,
      g.threads.map((t) => t.id),
    ]);
  const now = new Date(2026, 9, 22, 15);

  it("groups threads by when they were last asked in, as its example says", () => {
    expect(
      groups(
        [
          at("a", new Date(2026, 9, 22, 9)),
          at("b", new Date(2026, 9, 15, 0)),
          at("c", new Date(2026, 9, 14, 23)),
        ],
        now,
      ),
    ).toEqual([
      ["Today", ["a"]],
      ["Previous 7 days", ["b"]],
      ["Older", ["c"]],
    ]);
  });

  it("draws each line at local midnight", () => {
    expect(
      groups(
        [
          at("midnight", new Date(2026, 9, 22, 0)),
          at("just before", new Date(2026, 9, 21, 23, 59, 59, 999)),
          at("week start", new Date(2026, 9, 15, 0)),
          at("just older", new Date(2026, 9, 14, 23, 59, 59, 999)),
        ],
        now,
      ),
    ).toEqual([
      ["Today", ["midnight"]],
      ["Previous 7 days", ["just before", "week start"]],
      ["Older", ["just older"]],
    ]);
    // Just after midnight, yesterday evening is no longer today.
    expect(
      groups(
        [at("evening", new Date(2026, 9, 21, 22))],
        new Date(2026, 9, 22, 0, 5),
      ),
    ).toEqual([["Previous 7 days", ["evening"]]]);
  });

  it("keeps the threads' order and leaves empty groups out", () => {
    expect(
      groups(
        [
          at("new", new Date(2026, 9, 20)),
          at("old", new Date(2026, 9, 18)),
          at("older", new Date(2025, 0, 1)),
        ],
        now,
      ),
    ).toEqual([
      ["Previous 7 days", ["new", "old"]],
      ["Older", ["older"]],
    ]);
    expect(threadGroups([], now)).toEqual([]);
  });

  it("puts a thread from a clock ahead today, and an undated one with the older", () => {
    expect(
      groups(
        [
          at("ahead", new Date(2026, 9, 23, 1)),
          at("undated", ""),
          at("garbled", "yesterday"),
        ],
        now,
      ),
    ).toEqual([
      ["Today", ["ahead"]],
      ["Older", ["undated", "garbled"]],
    ]);
  });

  it("suggests up to four of an agent draft's competency questions", () => {
    // The shop draft holds seven: the first four, in order.
    expect(
      askSuggestions({
        id: "v2",
        format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
        definition: shopBundle,
      }),
    ).toEqual([
      "Who placed each order?",
      "Which products, in which quantities, were on each order?",
      "In which shipments, with which carriers, did each order go out?",
      "Which parcels were in each order's shipments?",
    ]);
    // The contracts draft holds two; a definition without format says it
    // in its own field.
    expect(askSuggestions({ id: "v3", definition: contractsBundle })).toEqual([
      "Which customers are under contract, and which contracts are they signed with?",
      "What is each customer under contract billed?",
    ]);
  });

  it("suggests nothing for a Definition v1, a draft without questions or a version not read", () => {
    expect(
      askSuggestions({ id: "v1", format: "ONTOLOGY_FORMAT_FOUNDRY_V1" }),
    ).toEqual([]);
    expect(askSuggestions(undefined)).toEqual([]);
    expect(askSuggestions(null)).toEqual([]);
    expect(
      askSuggestions({
        id: "v",
        format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
        definition: { format: "agent_bundle" },
      }),
    ).toEqual([]);
    // A question left blank is no suggestion.
    expect(
      askSuggestions({
        id: "v",
        format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
        definition: {
          format: "agent_bundle",
          draft: {
            draft: {
              items: {
                "cq.CQ1": { toml: '[cq.CQ1]\nquestion = "  "\n' },
                "cq.CQ2": { toml: '[cq.CQ2]\nquestion = "Who?"\n' },
              },
            },
          },
        },
      }),
    ).toEqual(["Who?"]);
    // Nor is one asked twice.
    expect(
      askSuggestions({
        id: "v",
        format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
        definition: {
          format: "agent_bundle",
          draft: {
            draft: {
              items: {
                "cq.CQ1": { toml: '[cq.CQ1]\nquestion = "Who?"\n' },
                "cq.CQ2": { toml: '[cq.CQ2]\nquestion = " Who? "\n' },
                "cq.CQ3": { toml: '[cq.CQ3]\nquestion = "What?"\n' },
              },
            },
          },
        },
      }),
    ).toEqual(["Who?", "What?"]);
  });

  it("reads the ontology from an Ask path", () => {
    expect(askOntologyId("/ask/ont_acme%20contracts")).toBe(
      "ont_acme contracts",
    );
    expect(askOntologyId("/ask/ont_1/anything")).toBe("ont_1");
    expect(askOntologyId("/ask")).toBe("");
    expect(askOntologyId("/ask/")).toBe("");
    expect(askOntologyId("/explore/ont_1")).toBe("");
    expect(askOntologyId("/asking/ont_1")).toBe("");
    // Not percent-encoding: no ontology, rather than a throw.
    expect(askOntologyId("/ask/ont%ZZ")).toBe("");
    expect(askOntologyId("/ask/%E0%A4%A")).toBe("");
  });

  it("says what the page shows, as its examples say", () => {
    const p = (q: string) => new URLSearchParams(q);
    expect(askView(p("thread=q1"), true, 3)).toEqual({
      kind: "thread",
      id: "q1",
    });
    expect(askView(p(""), true, 3)).toEqual({ kind: "threads" });
    expect(askView(p("new=1"), true, 3)).toEqual({ kind: "new" });
    expect(askView(p(""), true, 0)).toEqual({ kind: "new" });
    expect(askView(p(""), false, 3)).toEqual({ kind: "new" });
  });

  it("shows a thread over anything else, and the threads while they're read or failed", () => {
    const p = (q: string) => new URLSearchParams(q);
    // A wide screen lists the threads beside the page: a new thread, even
    // while they're read.
    expect(askView(p("thread=q1&new=1"), false, undefined)).toEqual({
      kind: "thread",
      id: "q1",
    });
    expect(askView(p(""), false, undefined)).toEqual({ kind: "new" });
    // A narrow one lists them until it knows there are none, and says there
    // why they couldn't be read.
    expect(askView(p(""), true, undefined)).toEqual({ kind: "threads" });
    expect(askView(p("focus=Q3VzdG9tZXIAazpjdXNfMQ"), true, 3)).toEqual({
      kind: "new",
    });
    expect(askView(p("new="), true, 3)).toEqual({ kind: "new" });
    expect(askView(p("thread=&focus="), true, 3)).toEqual({
      kind: "threads",
    });
    expect(askView(p("load=L1"), true, 3)).toEqual({ kind: "threads" });
  });
});

describe("workedFor", () => {
  const at = (s: number) =>
    new Date(Date.UTC(2026, 9, 30, 10) + s).toISOString();
  it("says seconds, minutes and seconds, or hours and minutes", () => {
    expect(workedFor(at(0), at(22_400))).toBe("22s");
    expect(workedFor(at(0), at(200))).toBe("1s");
    expect(workedFor(at(0), at(59_400))).toBe("59s");
    expect(workedFor(at(0), at(59_600))).toBe("1m");
    expect(workedFor(at(0), at(65_000))).toBe("1m 5s");
    expect(workedFor(at(0), at(3_600_000))).toBe("1h");
    expect(workedFor(at(0), at(3_720_000))).toBe("1h 2m");
  });
  it("is empty while a time is unknown, unreadable or out of order", () => {
    expect(workedFor(at(0), null)).toBe("");
    expect(workedFor(undefined, at(0))).toBe("");
    expect(workedFor("not a time", at(0))).toBe("");
    expect(workedFor(at(1000), at(0))).toBe("");
  });
});
