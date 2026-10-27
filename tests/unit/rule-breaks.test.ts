import { describe, expect, it } from "vitest";
import { contractErrors } from "../../mock/agent-contract";
import contractsBundle from "../../mock/fixtures/agent-bundle-contracts.json";
import agentBundle from "../../mock/fixtures/agent-bundle.json";
import { runQuery } from "../../mock/graph/query";
import {
  answer,
  compile,
  evaluate,
  sweepViolations,
} from "../../mock/graph/rules";
import { MockGraph } from "../../mock/graph/store";
import {
  parseEntityResult,
  parseExplainResult,
  type EntityRef,
} from "@/shared/foundry-read";
import type { OntologyVersion } from "@/shared/wfe";
import {
  identityOf,
  liveViolationOf,
  predLabel,
  readFailureText,
  refOf,
  rulesOf,
  sameRef,
} from "@/web/areas/explore/graph-read";
import { entityRequest, parseEntity } from "@/web/areas/explore/model";
import { namingOf } from "@/web/areas/explore/naming";
import {
  RULE_BREAK_LIMITS,
  entityNamesRequest,
  parseEntityNames,
  parseRuleBreakCounts,
  parseRuleBreakPage,
  ruleBreakCountRequests,
  ruleBreakPageRequest,
  type PageAt,
} from "@/web/areas/explore/rule-breaks";

// A load's rule breaks as its Rule breaks page reads them from the
// database: counts per constraint in the load, and pages of one
// constraint's breaks in key order with the entities each names; and how
// the rules' reads name entities.

const VERSION: OntologyVersion = {
  id: "ver_3",
  format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
  definition: contractsBundle,
};
const CONSTRAINT = "invoice_under_contract";

// 60 invoices billed to customers no contract is signed with, and one to a
// customer under contract, in load_1; another load's break in load_2.
function loaded() {
  const graph = new MockGraph();
  const program = compile(contractsBundle.ontology_toml);
  for (const load of ["load_1", "load_2"]) {
    const g = graph.view(load);
    const customer = (k: string, name: string) =>
      g.addNode("Customer", { entity_key: k, name, company_name: name });
    const signed = customer("cus_signed", "Signed Ltd");
    const contract = g.addNode("Contract", { norm_name: "msa", name: "MSA" });
    g.addEdge("signed_with", contract, signed);
    const n = load === "load_1" ? 60 : 1;
    for (let i = 0; i < n; i++) {
      const c = customer(`cus_${String(i).padStart(3, "0")}`, `Shop ${i}`);
      const inv = g.addNode("Invoice", {
        entity_key: `in_${i}`,
        invoice_id: `in_${i}`,
      });
      g.addEdge("billed_to", inv, c);
    }
    const ok = g.addNode("Invoice", {
      entity_key: "in_ok",
      invoice_id: "in_ok",
    });
    g.addEdge("billed_to", ok, signed);
    sweepViolations(evaluate(g, program), "draft");
  }
  return graph;
}

const run = (graph: MockGraph, request: unknown) => {
  const a = runQuery(graph, request);
  expect(a.status, JSON.stringify(a.body)).toBe(200);
  return a.body;
};

describe("rule break counts", () => {
  it("counts each constraint's breaks in the load, the constraint a constant", () => {
    const graph = loaded();
    const batches = ruleBreakCountRequests("load_1", [CONSTRAINT, "missing"]);
    expect(batches).toHaveLength(1);
    expect(JSON.stringify(batches[0]!.request)).toContain(
      '"right":{"constant":{"string":"invoice_under_contract"}}',
    );
    const counts = parseRuleBreakCounts(
      batches.map((b) => run(graph, b.request)),
      batches,
    );
    expect([...counts]).toEqual([
      [CONSTRAINT, 60],
      ["missing", 0],
    ]);
    expect([...parseRuleBreakCounts([{ count_0: -1 }, null], batches)]).toEqual(
      [
        [CONSTRAINT, null],
        ["missing", null],
      ],
    );
  });

  it("asks at most RULE_BREAK_LIMITS.counts constraints a request", () => {
    const many = Array.from(
      { length: RULE_BREAK_LIMITS.counts + 1 },
      (_, i) => `c${i}`,
    );
    expect(
      ruleBreakCountRequests("l", many).map((b) => b.constraints.length),
    ).toEqual([RULE_BREAK_LIMITS.counts, 1]);
  });
});

describe("a page of rule breaks", () => {
  it("pages one constraint's breaks in key order, each with its entities", () => {
    const graph = loaded();
    const page = (at: PageAt) =>
      parseRuleBreakPage(
        run(
          graph,
          ruleBreakPageRequest("load_1", CONSTRAINT, at, ["company_name"]),
        ),
      );
    const first = page({ after: null });
    expect(first.breaks).toHaveLength(RULE_BREAK_LIMITS.page);
    expect(first.next).toBe(first.breaks.at(-1)!.key);
    const keys = first.breaks.map((b) => b.key);
    expect([...keys].sort()).toEqual(keys);
    expect(first.breaks[0]).toMatchObject({
      severity: "warn",
      entities: [
        {
          type: "Invoice",
          identity: { type: "Invoice", key: expect.stringMatching(/^in_/) },
        },
      ],
    });
    const second = page({ after: first.next });
    expect(second.breaks).toHaveLength(10);
    expect(second.next).toBeNull();
    expect(second.breaks[0]!.key > first.next!).toBe(true);
    // Nothing of the other load, none twice.
    expect(new Set([...keys, ...second.breaks.map((b) => b.key)]).size).toBe(
      60,
    );
    // From a cited key: that break first.
    const cited = page({ from: keys[7]! });
    expect(cited.breaks[0]!.key).toBe(keys[7]);
    expect(cited.breaks).toHaveLength(RULE_BREAK_LIMITS.page);
  });

  it("keeps a violation's entities in position order and drops malformed rows", () => {
    const body = {
      breaks: [
        { $id: 1, violation_key: "k1", severity: "warn" },
        { $id: 2, violation_key: "" },
        { violation_key: "k3" },
      ],
      concerns: [
        { $from: 1, $to: 11, position: 1 },
        { $from: 1, $to: 10, position: 0 },
        { $from: 1, $to: 99, position: 2 },
        { $from: 1, $to: 10 },
      ],
      ends: [
        { $id: 10, $label: "Customer", entity_key: "c1", name: "C1" },
        { $id: 11, $label: "Invoice", entity_key: "i1" },
      ],
    };
    const p = parseRuleBreakPage(body);
    expect(p.next).toBeNull();
    expect(
      p.breaks.map((b) => [b.key, b.severity, b.entities.map((e) => e.name)]),
    ).toEqual([["k1", "warn", ["C1", "i1"]]]);
    expect(parseRuleBreakPage(null)).toEqual({ breaks: [], next: null });
  });

  it("asks for the next page with a parameter, and the entities' name props", () => {
    const r = ruleBreakPageRequest("load_1", CONSTRAINT, { after: "abc" }, [
      "company_name",
    ]);
    expect(r.parameters).toEqual({ after: "abc" });
    expect(r.parameter_types).toEqual({ after: "string" });
    expect(JSON.stringify(r)).toContain('"company_name"');
    expect(JSON.stringify(r)).toContain('"gt"');
    const from = ruleBreakPageRequest("load_1", CONSTRAINT, { from: "abc" });
    expect(from.parameters).toEqual({ after: "abc" });
    expect(JSON.stringify(from)).toContain('"gte"');
    expect(
      ruleBreakPageRequest("load_1", CONSTRAINT, { after: null }).parameters,
    ).toEqual({});
  });
});

describe("the rules' reads", () => {
  const naming = namingOf(VERSION);

  it("names the entities a read names, by type, in the load", () => {
    const graph = loaded();
    const request = entityNamesRequest(
      "load_1",
      [
        { type: "Customer", id: "cus_001" },
        { type: "Customer", id: "cus_001" },
        { type: "Contract", id: "msa" },
        { type: "Ghost", id: "boo" },
        { type: "Customer", id: "cus_nobody" },
      ],
      naming,
    )!;
    expect(request.parameters).toEqual({
      ids_0: ["cus_001", "cus_nobody"],
      ids_1: ["msa"],
    });
    expect([...parseEntityNames(run(graph, request), naming)]).toEqual([
      ["Customer\u0000cus_001", "Shop 1"],
      ["Contract\u0000msa", "MSA"],
    ]);
    expect(
      entityNamesRequest("load_1", [{ type: "Ghost", id: "x" }], naming),
    ).toBeNull();
    const many = Array.from(
      { length: RULE_BREAK_LIMITS.names + 5 },
      (_, i) => ({
        type: "Invoice",
        id: `in_${i}`,
      }),
    );
    expect(
      (
        entityNamesRequest("load_1", many, naming)!.parameters
          ?.ids_0 as string[]
      ).length,
    ).toBe(RULE_BREAK_LIMITS.names);
    expect(parseEntityNames([], naming).size).toBe(0);
  });

  it("names entities by type and id, keyed or by name as the version says", () => {
    expect(refOf({ type: "Customer", key: "cus_1" })).toEqual({
      type: "Customer",
      id: "cus_1",
    });
    expect(refOf({ type: "Contract", name: "msa" })).toEqual({
      type: "Contract",
      id: "msa",
    });
    expect(identityOf({ type: "Customer", id: "cus_1" }, naming)).toEqual({
      type: "Customer",
      key: "cus_1",
    });
    expect(identityOf({ type: "Contract", id: "msa" }, naming)).toEqual({
      type: "Contract",
      name: "msa",
    });
    expect(identityOf({ type: "Ghost", id: "x" }, naming)).toBeUndefined();
    expect(sameRef({ type: "A", id: "1" }, { type: "A", id: "1" })).toBe(true);
    expect(sameRef({ type: "A", id: "1" }, { type: "B", id: "1" })).toBe(false);
  });

  it("reads rules only for an agent draft that has some", () => {
    expect(
      rulesOf(VERSION)?.constraints.map((c) => [c.name, c.severity]),
    ).toEqual([[CONSTRAINT, "warn"]]);
    // The bench draft has a rule and a constraint too.
    expect(
      rulesOf({ ...VERSION, definition: agentBundle })?.rules.map(
        (r) => r.name,
      ),
    ).toEqual(["order_products"]);
    const plain = {
      ...contractsBundle,
      ontology_toml: contractsBundle.ontology_toml.split("\n[relations.")[0],
    };
    expect(rulesOf({ ...VERSION, definition: plain })).toBeNull();
    const declared = {
      ...plain,
      ontology_toml: plain.ontology_toml + "transitive = true\n",
    };
    expect(rulesOf({ ...VERSION, definition: declared })).not.toBeNull();
    expect(rulesOf({ id: "v1" })).toBeNull();
    expect(rulesOf(null)).toBeNull();
    expect(
      rulesOf({
        ...VERSION,
        definition: { format: "agent_bundle", ontology_toml: "[" },
      }),
    ).toBeNull();
  });

  it("says why a read couldn't be answered, and names a pred for a person", () => {
    expect(readFailureText({ code: "budget", message: "m", limit: 64 })).toBe(
      "Too deep to compute live",
    );
    expect(readFailureText({ code: "hub", message: "m", limit: 10000 })).toBe(
      "An entity it passes through has too many facts to compute live",
    );
    expect(
      readFailureText({
        code: "too_broad",
        message: "m",
        limit: 20000,
        count: 31200,
      }),
    ).toBe("Covers too many entities to compute live: 31,200, past 20,000");
    expect(readFailureText({ code: "unknown_entity", message: "m" })).toBe(
      "This load doesn’t hold it",
    );
    expect(
      readFailureText({ code: "invalid", message: "m", pointer: "/steps/0" }),
    ).toBe("The rules can’t answer this (at /steps/0)");
    expect(readFailureText({ code: "invalid", message: "m" })).toBe(
      "The rules can’t answer this",
    );
    expect(predLabel({ name: "under_contract" })).toBe("under contract");
  });
});

describe("an entity's stored rule breaks", () => {
  // A contract signed by three people on a link that takes one: the
  // engine's `signer.cardinality` stores one record per conflicting pair.
  const ONTOLOGY = `version = "draft"
[types.Contract]
description = "A contract, known by its title."
[types.Person]
description = "A person, keyed by their id."
props = { person_id = "string" }
key = ["person_id"]
[links.signer]
from = "Contract"
to = "Person"
description = "Signed by."
cardinality = "one"
`;
  const msa: EntityRef = { type: "Contract", id: "msa" };
  const person = (id: string): EntityRef => ({ type: "Person", id });

  function signed() {
    const graph = new MockGraph();
    const g = graph.view("load_1");
    const contract = g.addNode("Contract", { norm_name: "msa", name: "MSA" });
    for (const p of ["p1", "p2", "p3"])
      g.addEdge(
        "signer",
        contract,
        g.addNode("Person", { entity_key: p, person_id: p, name: p }),
        { fact_key: `f_${p}` },
      );
    const model = evaluate(g, compile(ONTOLOGY));
    sweepViolations(model, "draft");
    // An entity's page: its stored rule breaks from the database, and the
    // violations the rules find now that name it first.
    const page = (ref: EntityRef) => {
      const stored = parseEntity(
        run(
          graph,
          entityRequest(
            { kind: "load", id: "load_1" },
            ref.type === "Person"
              ? { type: ref.type, key: ref.id }
              : { type: ref.type, name: ref.id },
          ),
        ),
      ).violations;
      const read = answer(model, { entity: ref });
      expect(contractErrors("read_result#/$defs/ReadOutcome", read)).toEqual(
        [],
      );
      const live = parseEntityResult("ok" in read ? read.ok : null)!;
      return { stored, live: live.violations };
    };
    return { model, page };
  }
  const ids = (refs: { id: string }[]) => refs.map((r) => r.id);

  it("explains each record of a constraint with its own entities", () => {
    const { model, page } = signed();
    const { stored, live } = page(msa);
    expect(
      stored.map((v) => ids(v.entities.map((e) => refOf(e.identity)))),
    ).toEqual(
      expect.arrayContaining([
        ["msa", "p1", "p2"],
        ["msa", "p1", "p3"],
        ["msa", "p2", "p3"],
      ]),
    );
    expect(stored).toHaveLength(3);
    expect(live.map((l) => [l.constraint, l.severity, l.stored])).toEqual(
      Array(3).fill(["signer.cardinality", "block", true]),
    );
    // Each row's Why asks for its own pair, and the proof is of that pair.
    for (const v of stored) {
      const args = liveViolationOf(live, v)?.entities;
      expect(args).toEqual(v.entities.map((e) => refOf(e.identity)));
      const read = answer(model, {
        explain: { pred: "violation:signer.cardinality", args: args! },
      });
      expect(contractErrors("read_result#/$defs/ReadOutcome", read)).toEqual(
        [],
      );
      const proof = parseExplainResult("ok" in read ? read.ok : null)!;
      const root = proof.steps[proof.root!]!;
      expect(root.args).toEqual(args);
      expect(root.via).toEqual({
        kind: "decl",
        owner: "signer",
        decl: "cardinality",
      });
      expect(
        root.premises.map((i) => ids(proof.steps[i]!.args as EntityRef[])),
      ).toEqual([
        ["msa", args![1]!.id],
        ["msa", args![2]!.id],
      ]);
    }
  });

  it("has no proof for a record that names another entity first", () => {
    const { page } = signed();
    const { stored, live } = page(person("p2"));
    // p2 is in two pairs, never first: the rules list neither on its page.
    expect(
      stored.map((v) => ids(v.entities.map((e) => refOf(e.identity)))),
    ).toEqual(
      expect.arrayContaining([
        ["msa", "p1", "p2"],
        ["msa", "p2", "p3"],
      ]),
    );
    expect(stored).toHaveLength(2);
    expect(live).toEqual([]);
    expect(stored.map((v) => liveViolationOf(live, v))).toEqual([
      undefined,
      undefined,
    ]);
    // A record the rules no longer find, or find of another constraint or
    // order, has none either.
    const own = stored[0]!;
    const refs = own.entities.map((e) => refOf(e.identity));
    const found = (over: object) => ({
      constraint: own.constraint,
      severity: "block" as const,
      entities: refs,
      stored: true,
      ...over,
    });
    expect(liveViolationOf([found({})], own)?.entities).toEqual(refs);
    expect(liveViolationOf([found({ stored: false })], own)).toBeUndefined();
    expect(liveViolationOf([found({ constraint: "x" })], own)).toBeUndefined();
    expect(
      liveViolationOf([found({ entities: [...refs].reverse() })], own),
    ).toBeUndefined();
    expect(
      liveViolationOf([found({ entities: refs.slice(0, 2) })], own),
    ).toBeUndefined();
  });
});
