import { describe, expect, it } from "vitest";
import { FOUNDRY_NODES } from "@/shared/foundry-labels";
import {
  parseEntityResult,
  parseExplainResult,
  parseViolationsResult,
} from "@/shared/foundry-read";
import { contractErrors } from "../../mock/agent-contract";
import {
  answer,
  compile,
  evaluate,
  readsOf,
  sweepViolations,
} from "../../mock/graph/rules";
import { MockGraph } from "../../mock/graph/store";

// The mock's rules on the read path: a version's rules, link declarations
// and constraints evaluated over one load, answered with the engine's
// shapes (each answer checked against read_result.json), and its violation
// records swept.

const ONTOLOGY = `version = "draft"
[types.Place]
description = "A place, known by its name."
[types.Org]
description = "An organisation, keyed by its code."
props = { code = "string" }
key = ["code"]
[links.in]
from = "Place"
to = "Place"
description = "Inside."
transitive = true
[links.contains]
from = "Place"
to = "Place"
description = "Holds."
inverse_of = "in"
[links.based_in]
from = "Org"
to = "Place"
description = "Based."
implies = "located"
[relations.located]
description = "Somewhere."
[relations.peer]
description = "Same place."
symmetric = true
[rules.peers]
description = "Orgs based in one place are peers."
datalog = "peer(a, b) :- based_in(a, p), based_in(b, p)."
[rules.big]
description = "An org based somewhere."
datalog = "big(o) :- Org(o), based_in(o, _)."
[constraints.org_somewhere]
description = "Every org is based somewhere."
severity = "block"
datalog = "violation(o) :- Org(o), not big(o)."
`;

function load() {
  const g = new MockGraph().view("load_1");
  const place = (n: string) => g.addNode("Place", { norm_name: n, name: n });
  const org = (k: string) => g.addNode("Org", { entity_key: k, code: k });
  const [soho, london, england] = ["soho", "london", "england"].map(place);
  const [acme, bolt, cog] = ["acme", "bolt", "cog"].map(org);
  g.addEdge("in", soho!, london!, { fact_key: "f1" });
  g.addEdge("in", london!, england!, { fact_key: "f2" });
  g.addEdge("based_in", acme!, soho!, { fact_key: "f3" });
  g.addEdge("based_in", bolt!, soho!, { fact_key: "f4" });
  // Another load's fact is never read.
  g.graph.addEdge("based_in", cog!, soho!, { foundry_load: "load_2" });
  return { g, model: evaluate(g, compile(ONTOLOGY)) };
}

const ref = (type: string, id: string) => ({ type, id });
const soho = ref("Place", "soho"),
  london = ref("Place", "london"),
  england = ref("Place", "england"),
  acme = ref("Org", "acme"),
  bolt = ref("Org", "bolt"),
  cog = ref("Org", "cog");

type Model = ReturnType<typeof load>["model"];
/** One read's answer, held to the engine's schema. */
const read = (m: Model, r: Parameters<typeof answer>[1], opts = {}) => {
  const a = answer(m, r, opts);
  expect(contractErrors("read_result#/$defs/ReadOutcome", a)).toEqual([]);
  return a;
};
const entity = (m: Model, r: typeof acme, errors = {}) => {
  const a = read(m, { entity: r }, { errors });
  if (!("ok" in a)) throw new Error(JSON.stringify(a));
  return parseEntityResult(a.ok)!;
};

describe("mock rules", () => {
  it("derives through declarations and rules, never a self-loop", () => {
    const { model } = load();
    const soho_ = entity(model, soho);
    expect(
      soho_.derived.map((f) => [
        f.pred.name,
        f.args.map((a) => a.id),
        f.holder,
        f.rules,
      ]),
    ).toEqual(
      expect.arrayContaining([
        // A declared inverse derives each side from the other, so the stated
        // in(soho, london) is derived too. A link's facts are its own.
        ["in", ["soho", "london"], "in", ["decl:contains.inverse_of.in"]],
        [
          "in",
          ["soho", "england"],
          "in",
          ["decl:contains.inverse_of.in", "decl:in.transitive"],
        ],
        [
          "contains",
          ["london", "soho"],
          "contains",
          ["decl:in.inverse_of.contains"],
        ],
        // Only rules give a relation's facts here.
        ["located", ["acme", "soho"], null, ["decl:based_in.implies.located"]],
      ]),
    );
    const acme_ = entity(model, acme);
    expect(acme_.entity).toEqual(acme);
    expect(acme_.flags).toEqual([{ name: "big", rules: ["rule:big"] }]);
    expect(
      acme_.derived
        .filter((f) => f.pred.name === "peer")
        .map((f) => [f.args.map((a) => a.id), f.holder, f.rules]),
    ).toEqual([
      [["acme", "bolt"], null, ["decl:peer.symmetric", "rule:peers"]],
      [["bolt", "acme"], null, ["decl:peer.symmetric", "rule:peers"]],
    ]);
    // peer(acme, acme) is a self-loop: shown live, never derived or stored.
    expect(acme_.violations).toEqual([
      {
        constraint: "peer.self_loop",
        severity: "warn",
        entities: [acme],
        stored: false,
      },
    ]);
    expect(model.selfLoops).toHaveLength(2);
    // A fact stated and derived is "both"; one only stated is "stated".
    const a = read(model, {
      pattern: { pred: "link:in", args: [soho, null] },
    });
    expect("ok" in a && a.ok).toEqual({
      kind: "pattern",
      rows: [
        {
          args: [soho, england],
          holder: "link:in",
          basis: "derived",
          rules: ["decl:contains.inverse_of.in", "decl:in.transitive"],
        },
        {
          args: [soho, london],
          holder: "link:in",
          basis: "both",
          rules: ["decl:contains.inverse_of.in"],
        },
      ],
    });
    const based = read(model, {
      pattern: { pred: "link:based_in", args: [null, soho] },
    });
    expect("ok" in based && based.ok).toEqual({
      kind: "pattern",
      rows: [
        {
          args: [acme, soho],
          holder: "link:based_in",
          basis: "stated",
          rules: [],
        },
        {
          args: [bolt, soho],
          holder: "link:based_in",
          basis: "stated",
          rules: [],
        },
      ],
    });
    // A pattern needs a bound argument.
    expect(
      read(model, { pattern: { pred: "link:in", args: [null, null] } }),
    ).toMatchObject({ error: { code: "invalid" } });
  });

  it("finds violations through negation and stores them in a sweep", () => {
    const { g, model } = load();
    expect(entity(model, cog).violations).toEqual([
      {
        constraint: "org_somewhere",
        severity: "block",
        entities: [cog],
        stored: false,
      },
    ]);
    sweepViolations(model, "draft");
    sweepViolations(model, "draft");
    const records = [...g.nodes()].filter(
      (n) => n.label === FOUNDRY_NODES.Violation,
    );
    expect(records).toHaveLength(1);
    expect(records[0]!.props).toMatchObject({
      constraint: "org_somewhere",
      severity: "block",
      entities: ["Org\u001f\u0002cog"],
      opened_by: "sweep",
      foundry_load: "load_1",
    });
    expect(entity(model, cog).violations[0]!.stored).toBe(true);
    const page = (limit: number, after: string | null = null) => {
      const a = read(model, {
        violations: { constraint: "org_somewhere", entity: cog, after, limit },
      });
      return "ok" in a ? parseViolationsResult(a.ok) : undefined;
    };
    const key = String(records[0]!.props.violation_key);
    // A full page says where the next starts; the one after it is empty.
    expect(page(1)).toEqual({
      records: [
        {
          key,
          constraint: "org_somewhere",
          version: "draft",
          severity: "block",
          entities: [cog],
          openedBy: "sweep",
        },
      ],
      next: key,
    });
    expect(page(10)?.next).toBeNull();
    expect(page(10, key)).toEqual({ records: [], next: null });
    // The engine's defaults: every constraint, 100 a page.
    expect(read(model, { violations: {} })).toMatchObject({
      ok: { records: [{ violation_key: key }], next: null },
    });
    // A page of 0, or of a constraint the program hasn't, is refused.
    expect(read(model, { violations: { limit: 0 } })).toMatchObject({
      error: { code: "invalid" },
    });
    expect(read(model, { violations: { constraint: "nope" } })).toMatchObject({
      error: { code: "invalid" },
    });
  });

  it("explains a derived fact down to stated ones, and a violation through `not`", () => {
    const { model } = load();
    const explained = (pred: string, args: (typeof acme)[]) => {
      const a = read(model, { explain: { pred, args } });
      return parseExplainResult("ok" in a ? a.ok : null)!;
    };
    const p = explained("link:in", [soho, england]);
    expect(p.steps[p.root!]).toMatchObject({
      via: { kind: "decl", owner: "in", decl: "transitive" },
      args: [soho, england],
    });
    expect(p.steps[p.root!]!.premises.map((i) => p.steps[i]!.via.kind)).toEqual(
      ["stated", "stated"],
    );
    const v = explained("violation:org_somewhere", [cog]);
    const root = v.steps[v.root!]!;
    expect(root.via).toEqual({ kind: "constraint", name: "org_somewhere" });
    // Its type atom is a step, and so is `not big(cog)`.
    expect(root.premises.map((i) => v.steps[i])).toEqual([
      {
        pred: { kind: "type", name: "Org" },
        args: [cog],
        via: { kind: "type" },
        premises: [],
      },
      {
        pred: { kind: "derived", name: "big" },
        args: [cog],
        via: { kind: "not" },
        premises: [],
      },
    ]);
    // A stated fact is its own proof, though a declaration derives it too.
    const stated = explained("link:in", [soho, london]);
    expect(stated.steps).toHaveLength(1);
    expect(explained("link:in", [england, soho])).toEqual({
      steps: [],
      root: null,
    });
  });

  it("fails a part as told, and refuses what it can't answer", () => {
    const { model } = load();
    const acme_ = entity(model, acme, {
      "derived:big": "budget",
      violations: "hub",
    });
    expect(acme_.flags).toEqual([]);
    expect(acme_.violations).toEqual([]);
    expect(acme_.errors).toEqual([
      {
        part: { kind: "derived", name: "big" },
        failure: expect.objectContaining({ code: "budget", limit: 64 }),
      },
      {
        part: "violations",
        failure: expect.objectContaining({ code: "hub", limit: 10000 }),
      },
    ]);
    expect(read(model, { entity: ref("Org", "nobody") })).toEqual({
      error: {
        code: "unknown_entity",
        message: "the load holds no Org nobody",
      },
    });
    expect(read(model, { entity: ref("Ghost", "boo") })).toMatchObject({
      error: { code: "invalid" },
    });
    expect(
      read(model, {
        explain: { pred: "link:in", args: [ref("Place", "mars"), soho] },
      }),
    ).toMatchObject({ error: { code: "unknown_entity" } });
    // The mock's facts carry no evidence: none is stored for them.
    expect(
      read(model, {
        evidence: { fact: { link: "in", subject: soho, object: london } },
      }),
    ).toEqual({ ok: { kind: "evidence", items: [] } });
    expect(
      read(model, {
        evidence: { fact: { link: "in", subject: soho, object: england } },
      }),
    ).toEqual({ ok: { kind: "evidence", items: [] } });
    expect(read(model, { evidence: { entity: acme, prop: "code" } })).toEqual({
      ok: { kind: "evidence", items: [] },
    });
    expect(
      read(model, { evidence: { entity: acme, limit: 101 } }),
    ).toMatchObject({ error: { code: "invalid" } });
    expect(
      read(model, {
        query: {
          start: { type: "Org" },
          return: { count: {} },
        } as never,
      }),
    ).toMatchObject({ error: { code: "invalid" } });
  });

  it("refuses datalog it can't read and negation through a cycle", () => {
    const rule = (datalog: string) =>
      `version = "x"\n[types.T]\ndescription = "t"\n[rules.r]\ndescription = "r"\ndatalog = ${JSON.stringify(datalog)}\n`;
    expect(() => compile(rule('a(x) :- b(x, "London").'))).toThrow(
      /can't read/,
    );
    expect(() =>
      evaluate(
        new MockGraph().view("l"),
        compile(rule("a(x) :- T(x), not b(x). b(x) :- T(x), not a(x).")),
      ),
    ).toThrow(/cycle/);
    const g = new MockGraph().view("l");
    g.addNode("T", { norm_name: "t" });
    expect(() => evaluate(g, compile(rule("a(x, y) :- T(x).")))).toThrow(
      /unbound variable/,
    );
  });

  it("reads `readsJson` as the WFE does", () => {
    expect(readsOf('[{"entity":{"type":"T","id":"x"}}]')).toHaveLength(1);
    expect(readsOf("{")).toBe("readsJson isn't JSON");
    expect(readsOf("[]")).toBe("readsJson must be a list of reads");
    expect(readsOf(JSON.stringify(Array(21).fill({ entity: {} })))).toBe(
      "at most 20 reads at once",
    );
    // A read that doesn't fit read_request.json, as the engine refuses it.
    expect(readsOf('[{"entity":{},"explain":{}}]')).toMatch(/^read 0: /);
    expect(readsOf('[{"write":{}}]')).toMatch(/^read 0: /);
    expect(
      readsOf('[{"entity":{"type":"T","id":"x"}},{"violations":{"limit":-1}}]'),
    ).toMatch(/^read 1: /);
  });
});
