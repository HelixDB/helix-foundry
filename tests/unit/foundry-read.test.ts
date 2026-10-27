import { describe, expect, it } from "vitest";
import {
  outcomeOf,
  parseEntityResult,
  parseEvidenceResult,
  parseExplainResult,
  parseFailure,
  parsePatternResult,
  parsePred,
  parseReadResult,
  parseStepPred,
  parseVia,
  parseViolationsResult,
  predString,
} from "@/shared/foundry-read";
import { contractErrors } from "../../mock/agent-contract";

// The ontology agent's Read result as the WFE carries it: an envelope of outcomes in the reads' order, each parsed by its
// read's parser, keeping only what is well formed. Every well-formed
// fixture here is the engine's JSON: it fits read_result.json.

const globex = { type: "Customer", id: "cus_GLOBEX01" };
const initech = { type: "Customer", id: "cus_INITECH2" };
const msa = { type: "Contract", id: "master services agreement" };
const fits = (value: unknown, def = "Answer") =>
  expect(contractErrors(`read_result#/$defs/${def}`, value)).toEqual([]);

describe("the read envelope", () => {
  it("keeps each outcome in order, a failure's code and fields checked", () => {
    const body = {
      as_of: "2026-10-19",
      results: [
        { ok: { kind: "pattern", rows: [] } },
        { error: { code: "budget", message: "too deep", limit: 2000000 } },
        {
          error: {
            code: "too_broad",
            message: "too many",
            limit: 20000,
            count: 31000,
          },
        },
        {
          error: {
            code: "invalid",
            message: "no such prop",
            pointer: "/start/where/0",
          },
        },
      ],
      stats: { instances: 40, wall_ms: 8 },
    };
    expect(contractErrors("read_result", body)).toEqual([]);
    expect(
      parseReadResult(
        JSON.stringify({
          ...body,
          results: [...body.results, { error: { code: "made_up" } }, "junk"],
        }),
      ),
    ).toEqual({
      asOf: "2026-10-19",
      results: [
        { ok: true, value: { kind: "pattern", rows: [] } },
        {
          ok: false,
          error: { code: "budget", message: "too deep", limit: 2000000 },
        },
        {
          ok: false,
          error: {
            code: "too_broad",
            message: "too many",
            limit: 20000,
            count: 31000,
          },
        },
        {
          ok: false,
          error: {
            code: "invalid",
            message: "no such prop",
            pointer: "/start/where/0",
          },
        },
        { ok: false, error: { code: "invalid", message: "The read failed." } },
        { ok: false, error: { code: "invalid", message: "The read failed." } },
      ],
      stats: { instances: 40, wallMs: 8 },
    });
  });

  it("refuses a failure without the fields its code carries", () => {
    const malformed = { code: "invalid", message: "The read failed." };
    expect(parseFailure({ code: "budget", message: "m" })).toEqual(malformed);
    expect(parseFailure({ code: "hub", message: "m", limit: -1 })).toEqual(
      malformed,
    );
    expect(parseFailure({ code: "too_broad", message: "m", limit: 5 })).toEqual(
      malformed,
    );
    expect(parseFailure(null)).toEqual(malformed);
    expect(parseFailure({ code: "unknown_entity" })).toEqual({
      code: "unknown_entity",
      message: "The read failed.",
    });
    expect(
      parseFailure({ code: "invalid", message: "m", pointer: null }),
    ).toEqual({ code: "invalid", message: "m" });
  });

  it("refuses what isn't an envelope", () => {
    expect(parseReadResult("")).toBeUndefined();
    expect(parseReadResult("{")).toBeUndefined();
    expect(parseReadResult("[]")).toBeUndefined();
    expect(parseReadResult('{"results":{}}')).toBeUndefined();
    expect(parseReadResult('{"results":[]}')).toMatchObject({
      asOf: "",
      results: [],
      stats: { instances: 0, wallMs: 0 },
    });
  });

  it("parses an ok by its read's parser; a malformed ok is invalid", () => {
    expect(
      outcomeOf(
        { ok: true, value: { kind: "pattern", rows: [] } },
        parsePatternResult,
      ),
    ).toEqual({ ok: true, value: { rows: [] } });
    // An answer of another kind doesn't parse as this one.
    expect(
      outcomeOf(
        { ok: true, value: { kind: "explain", steps: [], root: null } },
        parsePatternResult,
      ),
    ).toEqual({
      ok: false,
      error: { code: "invalid", message: "The read's result is malformed." },
    });
    const error = {
      ok: false as const,
      error: { code: "hub" as const, message: "m", limit: 10000 },
    };
    expect(outcomeOf(error, parsePatternResult)).toBe(error);
    expect(outcomeOf(undefined, parsePatternResult)).toMatchObject({
      ok: false,
      error: { code: "invalid" },
    });
  });
});

describe("preds and vias", () => {
  it("reads a pred's kind and name", () => {
    expect(parsePred("link:billed_to")).toEqual({
      kind: "link",
      name: "billed_to",
    });
    expect(parsePred("relation:co_signed")).toEqual({
      kind: "relation",
      name: "co_signed",
    });
    expect(parsePred("violation:a.b")).toEqual({
      kind: "violation",
      name: "a.b",
    });
    for (const bad of [
      "derived:",
      ":x",
      "type:Customer",
      "has:x",
      "x",
      3,
      null,
    ])
      expect(parsePred(bad), String(bad)).toBeUndefined();
    // A proof's steps also name types and `has`.
    expect(parseStepPred("type:Customer")).toEqual({
      kind: "type",
      name: "Customer",
    });
    expect(parseStepPred("has:billed_to")).toEqual({
      kind: "has",
      name: "billed_to",
    });
    // And a value rule's tests, as the rule writes them.
    expect(parseStepPred("test:due_on < today - 30d")).toEqual({
      kind: "test",
      name: "due_on < today - 30d",
    });
    expect(parseStepPred("compare:amount > limit")).toEqual({
      kind: "compare",
      name: "amount > limit",
    });
    expect(parseStepPred("kind:x")).toBeUndefined();
    expect(predString({ kind: "derived", name: "covers" })).toBe(
      "derived:covers",
    );
  });

  it("reads how a step holds", () => {
    for (const kind of ["stated", "property", "type", "member", "not"])
      expect(parseVia(kind)).toEqual({ kind });
    expect(parseVia("rule:co_signers")).toEqual({
      kind: "rule",
      name: "co_signers",
    });
    expect(parseVia("constraint:invoice_under_contract")).toEqual({
      kind: "constraint",
      name: "invoice_under_contract",
    });
    expect(parseVia("decl:co_signed.symmetric")).toEqual({
      kind: "decl",
      owner: "co_signed",
      decl: "symmetric",
    });
    expect(parseVia("decl:contains.inverse_of.in")).toEqual({
      kind: "decl",
      owner: "contains",
      decl: "inverse_of.in",
    });
    for (const bad of [
      "rule:",
      "constraint:",
      "decl:x",
      "decl:.y",
      "proved",
      1,
    ])
      expect(parseVia(bad), String(bad)).toBeUndefined();
  });
});

describe("an entity read", () => {
  it("keeps derived facts, flags, live violations and failed parts", () => {
    const ok = {
      kind: "entity",
      entity: globex,
      derived: [
        {
          pred: "relation:co_signed",
          holder: null,
          args: [globex, initech],
          basis: "derived",
          rules: ["decl:co_signed.symmetric", "rule:co_signers"],
        },
        {
          pred: "link:signed_with",
          holder: "link:signed_with",
          args: [msa, globex],
          basis: "both",
          rules: ["rule:signers"],
        },
      ],
      flags: [{ name: "invoiced", rules: ["rule:invoiced"] }],
      violations: [
        {
          constraint: "co_signed.self_loop",
          severity: "warn",
          entities: [globex],
          stored: false,
        },
      ],
      errors: [
        {
          pred: "derived:covers",
          code: "budget",
          message: "too deep",
          limit: 64,
        },
        { pred: "violations", code: "hub", message: "a hub", limit: 10000 },
      ],
    };
    fits(ok);
    const malformed = {
      ...ok,
      derived: [
        ...ok.derived,
        // A holder that isn't a link, one arg, a stated basis, a bad ref.
        { ...ok.derived[0], holder: "relation:co_signed" },
        { ...ok.derived[0], args: [globex] },
        { ...ok.derived[0], basis: "stated" },
        { ...ok.derived[0], args: [msa, { type: "X" }] },
      ],
      flags: [...ok.flags, { name: "" }, { rules: [] }, 3],
      violations: [
        ...ok.violations,
        { constraint: "x", severity: "fatal", entities: [], stored: true },
        { constraint: "x", severity: "warn", entities: [] },
      ],
      errors: [
        ...ok.errors,
        // A code it doesn't know still fails its part.
        { pred: "derived:invoiced", code: "slow", message: "m" },
        { pred: "nope", code: "invalid", message: "m" },
        // Not a code an entity read's part fails by.
        { pred: "derived:x", code: "unknown_entity", message: "m" },
      ],
    };
    expect(parseEntityResult(malformed)).toEqual({
      entity: globex,
      derived: [
        {
          pred: { kind: "relation", name: "co_signed" },
          holder: null,
          args: [globex, initech],
          basis: "derived",
          rules: ["decl:co_signed.symmetric", "rule:co_signers"],
        },
        {
          pred: { kind: "link", name: "signed_with" },
          holder: "signed_with",
          args: [msa, globex],
          basis: "both",
          rules: ["rule:signers"],
        },
      ],
      flags: [{ name: "invoiced", rules: ["rule:invoiced"] }],
      violations: [
        {
          constraint: "co_signed.self_loop",
          severity: "warn",
          entities: [globex],
          stored: false,
        },
      ],
      errors: [
        {
          part: { kind: "derived", name: "covers" },
          failure: { code: "budget", message: "too deep", limit: 64 },
        },
        {
          part: "violations",
          failure: { code: "hub", message: "a hub", limit: 10000 },
        },
        {
          part: { kind: "derived", name: "invoiced" },
          failure: { code: "invalid", message: "The read failed." },
        },
      ],
    });
    expect(parseEntityResult([])).toBeUndefined();
    expect(parseEntityResult({ kind: "entity" })).toBeUndefined();
    expect(parseEntityResult({ kind: "pattern", entity: globex })).toBe(
      undefined,
    );
    expect(parseEntityResult({ kind: "entity", entity: globex })).toEqual({
      entity: globex,
      derived: [],
      flags: [],
      violations: [],
      errors: [],
    });
  });
});

describe("a pattern read", () => {
  it("keeps rows with their holder, basis and rules", () => {
    const ok = {
      kind: "pattern",
      rows: [
        {
          args: [globex, initech],
          holder: null,
          basis: "derived",
          rules: ["rule:co_signers"],
        },
        {
          args: [msa, globex],
          holder: "link:signed_with",
          basis: "stated",
          rules: [],
        },
      ],
    };
    fits(ok);
    expect(
      parsePatternResult({
        ...ok,
        rows: [
          ...ok.rows,
          { args: [msa, globex], holder: null, basis: "guessed" },
          { args: "x", holder: null, basis: "both" },
          { args: [msa, globex], basis: "both" },
        ],
      }),
    ).toEqual({
      rows: [
        {
          args: [globex, initech],
          holder: null,
          basis: "derived",
          rules: ["rule:co_signers"],
        },
        {
          args: [msa, globex],
          holder: "signed_with",
          basis: "stated",
          rules: [],
        },
      ],
    });
  });
});

describe("an explain read", () => {
  const steps = [
    {
      pred: "link:signed_with",
      args: [msa, globex],
      via: "stated",
      premises: [],
    },
    {
      pred: "link:signed_with",
      args: [msa, initech],
      via: "stated",
      premises: [],
    },
    {
      pred: "type:Customer",
      args: [globex],
      via: "type",
      premises: [],
    },
    {
      pred: "derived:blocked",
      args: [globex, null],
      via: "not",
      premises: [],
    },
    {
      pred: "relation:co_signed",
      args: [globex, initech],
      via: "rule:co_signers",
      premises: [0, 1, 2, 3],
    },
  ];

  it("keeps a proof whose premises are steps", () => {
    fits({ kind: "explain", steps, root: 4 });
    const p = parseExplainResult({ kind: "explain", steps, root: 4 });
    expect(p?.root).toBe(4);
    expect(p?.steps[4]).toEqual({
      pred: { kind: "relation", name: "co_signed" },
      args: [globex, initech],
      via: { kind: "rule", name: "co_signers" },
      premises: [0, 1, 2, 3],
    });
    expect(p?.steps[2]).toMatchObject({
      pred: { kind: "type", name: "Customer" },
      via: { kind: "type" },
    });
    // A `not` step's open place is null.
    expect(p?.steps[3]?.args).toEqual([globex, null]);
    // A value rule's property test keeps the test it stands for.
    const tested = {
      kind: "explain",
      steps: [
        {
          test: "amount > 1000",
          pred: "test:amount > 1000",
          args: [msa],
          via: "property",
          premises: [],
        },
        {
          test: "absent signed_on",
          pred: "test:signed_on != null",
          args: [msa],
          via: "not",
          premises: [],
        },
        {
          pred: "derived:large_unsigned",
          args: [msa],
          via: "rule:large_unsigned",
          premises: [0, 1],
        },
      ],
      root: 2,
    };
    fits(tested);
    expect(parseExplainResult(tested)?.steps).toEqual([
      {
        pred: { kind: "test", name: "amount > 1000" },
        args: [msa],
        via: { kind: "property" },
        premises: [],
        test: "amount > 1000",
      },
      {
        pred: { kind: "test", name: "signed_on != null" },
        args: [msa],
        via: { kind: "not" },
        premises: [],
        test: "absent signed_on",
      },
      {
        pred: { kind: "derived", name: "large_unsigned" },
        args: [msa],
        via: { kind: "rule", name: "large_unsigned" },
        premises: [0, 1],
      },
    ]);
    // A fact that doesn't hold has no root.
    const none = { kind: "explain", steps: [], root: null };
    fits(none);
    expect(parseExplainResult(none)).toEqual({ steps: [], root: null });
  });

  it("refuses half a proof: a bad step, a premise or root out of range, a cycle", () => {
    const proof = (s: unknown[], root: unknown) =>
      parseExplainResult({ kind: "explain", steps: s, root });
    expect(proof([...steps, 1], 4)).toBeUndefined();
    expect(proof([{ ...steps[0], via: "magic" }], 0)).toBeUndefined();
    // A null arg only in a `not` step.
    expect(proof([{ ...steps[0], args: [msa, null] }], 0)).toBeUndefined();
    expect(proof([{ ...steps[0], premises: [-1] }], 0)).toBeUndefined();
    expect(proof([{ ...steps[0], premises: [3] }], 0)).toBeUndefined();
    expect(proof(steps, 5)).toBeUndefined();
    expect(proof(steps, "2")).toBeUndefined();
    expect(parseExplainResult({ kind: "explain", root: 0 })).toBeUndefined();
    expect(parseExplainResult({ steps, root: 4 })).toBeUndefined();
    expect(
      proof(
        [
          { ...steps[4], premises: [1] },
          { ...steps[4], premises: [0] },
        ],
        0,
      ),
    ).toBeUndefined();
    // A premise shared by two steps is no cycle.
    expect(
      proof([steps[0], { ...steps[4], premises: [0, 0] }], 1),
    ).toBeDefined();
  });
});

describe("a violations read", () => {
  it("keeps a page of records and the key of the next", () => {
    const record = {
      violation_key: "k1",
      constraint: "invoice_under_contract",
      version: "draft",
      severity: "warn",
      entities: [{ type: "Invoice", id: "in_1004" }],
      opened_by: "sweep",
    };
    const ok = { kind: "violations", records: [record], next: "k1" };
    fits(ok);
    expect(
      parseViolationsResult({
        ...ok,
        records: [...ok.records, { ...record, violation_key: "" }],
      }),
    ).toEqual({
      records: [
        {
          key: "k1",
          constraint: "invoice_under_contract",
          version: "draft",
          severity: "warn",
          entities: [{ type: "Invoice", id: "in_1004" }],
          openedBy: "sweep",
        },
      ],
      next: "k1",
    });
    const last = { kind: "violations", records: [], next: null };
    fits(last);
    expect(parseViolationsResult(last)).toEqual({ records: [], next: null });
    expect(parseViolationsResult(null)).toBeUndefined();
  });
});

describe("an evidence read", () => {
  it("keeps quotes and rows", () => {
    const ok = {
      kind: "evidence",
      items: [
        {
          kind: "quote",
          doc_id: "doc_1",
          section: 1,
          start: 4,
          end: 20,
          quote: "signed with Globex",
          confidence: 0.93,
        },
        {
          kind: "row",
          source: "src_1",
          mapping: "invoices",
          row_key: "in_1001",
          confidence: 1,
        },
      ],
    };
    fits(ok);
    expect(
      parseEvidenceResult({
        ...ok,
        items: [
          ...ok.items,
          { kind: "row", source: "src_1" },
          { kind: "quote", doc_id: "doc_1", section: 1, start: 9, end: 2 },
          { kind: "other" },
        ],
      }),
    ).toEqual({
      items: [
        {
          kind: "quote",
          documentId: "doc_1",
          section: 1,
          start: 4,
          end: 20,
          quote: "signed with Globex",
          confidence: 0.93,
        },
        {
          kind: "row",
          source: "src_1",
          mapping: "invoices",
          rowKey: "in_1001",
          confidence: 1,
        },
      ],
    });
    expect(parseEvidenceResult("x")).toBeUndefined();
  });

  it("keeps the value a property's quote states", () => {
    const quote = {
      kind: "quote",
      doc_id: "doc_1",
      section: 0,
      start: 0,
      end: 12,
      quote: "Net 30 terms",
      confidence: 0.9,
    };
    const ok = {
      kind: "evidence",
      items: [
        { ...quote, value: "net_30" },
        { ...quote, value: 30 },
        { ...quote, value: { nested: true } },
      ],
    };
    fits(ok);
    expect(
      parseEvidenceResult(ok)?.items.map((i) =>
        i.kind === "quote" ? i.value : null,
      ),
    ).toEqual(["net_30", 30, undefined]);
  });
});
