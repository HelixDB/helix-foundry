import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type * as wfe from "@/shared/wfe";
import { readAgentBundle } from "@/web/areas/ontology/model";
import { RevisionEvidence } from "@/web/areas/ontology/version-edit";
import { AgentRules } from "@/web/areas/ontology/version-agent";

const card: wfe.AgentEvidenceCard = {
  rev: 7,
  sample: "20 sampled rows; document preview unavailable",
  instances: 20,
  confirmed: 18,
  added: 2,
  contradicted: 1,
  unmeasurable: false,
  precision: 18 / 19,
  failure_rate_bound: 0.12,
  violations: [{ constraint: "check", severity: "warn", before: 0, after: 1 }],
  subjects: 20,
  violation_rate: 0.05,
  review_facts: 1,
  unrepresentable: 0,
  counterexamples: [
    {
      fact: '<img src=x onerror="alert(1)">',
      proof: ["<script>alert(1)</script>"],
      rows: [{ source: "src_sample", mapping: "rows" }],
    },
  ],
  unmeasured: "documents",
  reason: "<b>Untrusted source text</b>",
};

describe("engine-shaped rule evidence", () => {
  it.each(["rule", "warn", "refused"] as const)(
    "renders structured cards and %s verdicts for rules and constraints as text",
    (verdict) => {
      const bundle = readAgentBundle({
        format: "agent_bundle",
        ontology_toml:
          '[rules.derive]\ndatalog = "p(x) :- T(x)."\n[constraints.check]\nseverity = "warn"\ndatalog = "violation(x) :- T(x)."',
        draft: {
          draft: {
            items: {
              "rule.derive": { toml: "", verdict, card },
              "constraint.check": { toml: "", verdict, card },
            },
          },
        },
      });
      const html = renderToStaticMarkup(createElement(AgentRules, { bundle }));
      expect(html.match(/aria-label="Evidence card"/g)).toHaveLength(2);
      for (const field of [
        "confirmed",
        "counterexamples",
        "review_facts",
        "unmeasured",
        "documents",
        "src_sample",
        "20 sampled rows",
      ])
        expect(html).toContain(field);
      expect(html).toContain(verdict);
      expect(html).toContain("&lt;b&gt;Untrusted source text&lt;/b&gt;");
      expect(html).toContain("&lt;script&gt;");
      expect(html).not.toMatch(/<script|<img|<b>/);
    },
  );

  it("renders items without evidence", () => {
    const bundle = readAgentBundle({
      ontology_toml: '[rules.derive]\ndatalog = "p(x) :- T(x)."',
      draft: {
        draft: {
          items: { "rule.derive": { toml: "", card: null, verdict: null } },
        },
      },
    });
    const html = renderToStaticMarkup(createElement(AgentRules, { bundle }));
    expect(html).toContain("derive");
    expect(html).not.toContain('aria-label="Evidence card"');
  });
});

it.each(["revised", "refused"] as const)(
  "renders nested Revise cards and %s findings as plain text",
  (status) => {
    const html = renderToStaticMarkup(
      createElement(RevisionEvidence, {
        generation: {
          id: "g",
          status:
            status === "revised"
              ? "ONTOLOGY_GENERATION_STATUS_SUCCEEDED"
              : "ONTOLOGY_GENERATION_STATUS_FAILED",
          revision: {
            dryRun: true,
            resultJson: JSON.stringify({
              status,
              findings: [
                {
                  code: status === "revised" ? "Lint" : "Evaluation",
                  item_id: "<img>",
                  message: "<script>untrusted</script>",
                },
              ],
              cards: [
                {
                  item: "constraint.check",
                  verdict: "warn",
                  card: { ...card, unmeasured: "load" },
                },
              ],
              ...(status === "revised"
                ? { rules: { count: 1, evaluated: 1 } }
                : {}),
            }),
          },
        },
      }),
    );
    expect(html).toContain(
      status === "refused" ? "Edits refused." : "Preview ready.",
    );
    expect(html).toContain("constraint.check");
    expect(html).toContain("load");
    expect(html).toContain("&lt;script&gt;untrusted&lt;/script&gt;");
    expect(html).not.toMatch(/<script|<img|<b>/);
  },
);

it("renders a refusal past the measuring budget in words, with no cards", () => {
  const html = renderToStaticMarkup(
    createElement(RevisionEvidence, {
      generation: {
        id: "g",
        status: "ONTOLOGY_GENERATION_STATUS_FAILED",
        revision: {
          dryRun: true,
          resultJson: JSON.stringify({
            status: "refused",
            findings: [
              {
                code: "budget",
                item_id: null,
                message: "<b>needs more than its budget</b>",
              },
            ],
            cards: [],
          }),
        },
      },
    }),
  );
  expect(html).toContain("Edits refused.");
  expect(html).toContain(
    "Measuring these edits on the current load needed more than its budget",
  );
  expect(html).toContain("&lt;b&gt;needs more than its budget&lt;/b&gt;");
  expect(html).not.toContain(" · warn");
  expect(html).not.toMatch(/<b>/);
});

it.each([
  ["load", "no load of a version with the same types"],
  ["entities", "some of the load’s entities were over the read budget"],
  ["documents", "the preview had none for it"],
  ["rows", "for a rule, rows that state what it derived"],
  [
    "rows, documents",
    "was missing rows it needs, and the preview had no documents",
  ],
] as const)(
  "says in words what a card wasn't measured on: %s",
  (unmeasured, words) => {
    const revision = renderToStaticMarkup(
      createElement(RevisionEvidence, {
        generation: {
          id: "g",
          status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
          revision: {
            dryRun: true,
            resultJson: JSON.stringify({
              status: "revised",
              findings: [],
              cards: [
                {
                  item: "constraint.check",
                  verdict: "warn",
                  card: { ...card, unmeasured },
                },
              ],
              rules: { count: 1, evaluated: 1 },
            }),
          },
        },
      }),
    );
    expect(revision).toContain(words);
    const stored = renderToStaticMarkup(
      createElement(AgentRules, {
        bundle: readAgentBundle({
          format: "agent_bundle",
          ontology_toml:
            '[constraints.check]\nseverity = "warn"\ndatalog = "violation(x) :- T(x)."',
          draft: {
            draft: {
              items: {
                "constraint.check": {
                  toml: "",
                  verdict: "warn",
                  card: { ...card, unmeasured },
                },
              },
            },
          },
        }),
      }),
    );
    expect(stored).toContain(words);
  },
);

it("says nothing in words for an unmeasured value it doesn't know", () => {
  const html = renderToStaticMarkup(
    createElement(AgentRules, {
      bundle: readAgentBundle({
        format: "agent_bundle",
        ontology_toml:
          '[constraints.check]\nseverity = "warn"\ndatalog = "violation(x) :- T(x)."',
        draft: {
          draft: {
            items: {
              "constraint.check": {
                toml: "",
                verdict: "warn",
                card: { ...card, unmeasured: "constructor" },
              },
            },
          },
        },
      }),
    }),
  );
  expect(html).toContain("constructor");
  expect(html).not.toContain("Not measured");
});
