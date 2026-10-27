import * as toml from "smol-toml";
import type * as wfe from "../../src/shared/wfe";
import {
  batchEnrichmentRefusal,
  editItem,
  editTables,
  validateEdit,
  type EditKind,
} from "../../src/web/areas/ontology/edit-model";
import { readAgentBundle } from "../../src/web/areas/ontology/model";

/**
 * What the mock's Revise measures edited rules on: no comparable load; a
 * comparable one, of which it reads no entity (the engine's `unmeasured:
 * "entities"`); or one measuring would take past its budget.
 */
export type MockLoad = "none" | "comparable" | "over_budget";

/** Deterministic mock of the engine's edit boundary, not a Datalog evaluator.
 * Constraints receive the engine's warn cap (no comparable load, or entities it
 * didn't read) and count as evaluated. Derived rules require measurements
 * and are refused. Past the budget, a revision with rules to measure is
 * refused with one `budget` finding and no cards, as the engine refuses it. No
 * counts claim measured data.
 */
export function reviseDefinition(
  definition: Record<string, unknown>,
  edits: wfe.OntologyEdit[],
  load: MockLoad,
): wfe.OntologyReviseResult & { definition?: Record<string, unknown> } {
  const bundle = readAgentBundle(definition);
  const findings: wfe.OntologyReviseFinding[] = [];
  const cards: wfe.OntologyReviseCard[] = [];
  const ontology = toml.parse(bundle.ontologyToml) as Record<string, any>;
  const mappings = toml.parse(bundle.mappingsToml) as Record<string, any>;
  const draft = structuredClone(
    (definition as wfe.AgentBundleDefinition).draft || {},
  );
  draft.draft ||= {};
  draft.draft.items ||= {};
  const items = draft.draft.items;
  for (const edit of edits) {
    const itemId = editItem(edit);
    const error = validateEdit(bundle, edit, edits);
    if (error) {
      findings.push({
        code: error.startsWith("EditNotAllowed:")
          ? "EditNotAllowed"
          : // validate_enrichments' errors
            error.startsWith("Enrichment")
            ? "Validation"
            : "InvalidEdit",
        message: error,
        item_id: itemId,
      });
      continue;
    }
    const [kind, name] = itemId.split(".") as [EditKind, string];
    const table = editTables[kind],
      doc =
        kind === "extraction" || kind === "enrichment" ? mappings : ontology;
    if (!edit.put) {
      delete doc[table][name];
      if (!Object.keys(doc[table]).length) delete doc[table];
      delete items[itemId];
    } else {
      const value = (toml.parse(edit.put.toml)[table] as Record<string, any>)[
        name
      ];
      doc[table] = { ...doc[table], [name]: value };
      const { clues: _, ...before } = items[itemId] || { toml: "" };
      items[itemId] = {
        ...before,
        toml: edit.put.toml,
        status: "user_edited",
        // A new link serves what its type serves.
        ...(kind === "link" && !items[itemId]
          ? { serves: items[`type.${value.from}`]?.serves || [] }
          : {}),
      };
    }
  }
  // Confidence measured an item's type as it was: an edit to it drops it
  // (a put of the item itself already did).
  for (const [name, e] of Object.entries(mappings.enrichments || {}) as [
    string,
    any,
  ][])
    if (edits.some((x) => editItem(x) === `type.${e.type}`))
      delete items[`enrichment.${name}`]?.clues;
  // Revise then validates the whole revised draft: an edit to a type or a
  // route that breaks the People enrichment item is refused against it.
  const left = findings.length
    ? undefined
    : batchEnrichmentRefusal(bundle, edits);
  if (left)
    findings.push({
      code: "Validation",
      message: left.message,
      item_id: left.itemId,
    });
  if (findings.length) return { status: "refused", findings, cards };
  // Propagate today through named predicates, so an indirect time-dependent
  // constraint is also refused. This mock handles named rule dependencies;
  // The engine owns parsing, type inference and complete semantic validation.
  const rules = Object.entries(ontology.rules || {}) as [string, any][];
  const time = new Set(
    rules
      .filter(([, r]) => /\btoday\b/.test(r.datalog))
      .map(([, r]) => String(r.datalog).match(/^\s*(\w+)\s*\(/)?.[1])
      .filter((s): s is string => !!s),
  );
  for (let n = 0; n < rules.length; n++)
    for (const [, rule] of rules) {
      if (
        [...time].some((p) => new RegExp(`\\b${p}\\s*\\(`).test(rule.datalog))
      ) {
        const head = String(rule.datalog).match(/^\s*(\w+)\s*\(/)?.[1];
        if (head) time.add(head);
      }
    }
  for (const [name, constraint] of Object.entries(
    ontology.constraints || {},
  ) as [string, any][]) {
    if (
      /\btoday\b/.test(constraint.datalog) ||
      [...time].some((p) =>
        new RegExp(`\\b${p}\\s*\\(`).test(constraint.datalog),
      )
    )
      findings.push({
        code: "Validation",
        item_id: `constraint.${name}`,
        message:
          "TimeInConstraint: today is allowed in rules, but cannot be reached by a constraint.",
      });
  }
  const measured = edits.filter((e) =>
    /^(rule|constraint)\./.test(editItem(e)),
  );
  if (load === "over_budget" && !findings.length && measured.some((e) => e.put))
    return {
      status: "refused",
      findings: [
        {
          code: "budget",
          item_id: null,
          message:
            "Measuring the revision on the load needs more than its budget (rule instances); no revision was accepted.",
        },
      ],
      cards: [],
    };
  for (const edit of measured) {
    const itemId = editItem(edit);
    if (load === "none")
      findings.push({
        code: "no_comparable_load",
        item_id: itemId,
        message:
          "Load a version with the same types, links and properties before evaluating these changes.",
      });
    if (edit.put) {
      const derived = itemId.startsWith("rule.");
      if (derived)
        findings.push({
          code: "Evaluation",
          item_id: itemId,
          message:
            "Mock has no load evaluator; derived rules require measurements.",
        });
      const card: wfe.AgentEvidenceCard = {
        rev: draft.draft.rev || 0,
        sample:
          load === "none"
            ? "Mock preview only; no source data measured."
            : "Mock preview only; no entity of the load read.",
        instances: 0,
        confirmed: 0,
        added: 0,
        contradicted: 0,
        unmeasurable: true,
        precision: null,
        failure_rate_bound: null,
        violations: [],
        subjects: 0,
        violation_rate: null,
        review_facts: 0,
        unrepresentable: 0,
        counterexamples: [],
        unmeasured: load === "none" ? "load" : "entities",
        reason:
          "Mock preview only. Rule evaluation requires the Revise worker.",
      };
      const verdict = derived ? "refused" : "warn";
      cards.push({ item: itemId, verdict, card });
      items[itemId] = {
        ...items[itemId]!,
        verdict,
        card,
        pending: null,
      };
      if (!derived) {
        const name = itemId.slice("constraint.".length);
        if (ontology.constraints[name].severity === "block")
          ontology.constraints[name].severity = "warn";
        items[itemId]!.toml = toml.stringify({
          constraints: { [name]: ontology.constraints[name] },
        });
      }
    }
  }
  if (
    findings.some((f) => f.code !== "no_comparable_load" && f.code !== "Lint")
  )
    return { status: "refused", findings, cards };
  const ontology_toml = toml.stringify(ontology),
    mappings_toml = toml.stringify(mappings);
  // The engine counts inference declarations too, but not cardinality/required dry-run gates.
  const gates = [
    ...Object.keys(ontology.rules || {}).map((name) => `rule.${name}`),
    ...Object.keys(ontology.constraints || {}).map(
      (name) => `constraint.${name}`,
    ),
    ...Object.keys(items).filter((id) =>
      /^(link|relation)\.[^.]+\.(implies\.|inverse_of$|symmetric$|transitive$|chain\.)/.test(
        id,
      ),
    ),
  ];
  const count = gates.length;
  const evaluated = gates.filter(
    (id) =>
      items[id]?.card &&
      !items[id]?.pending &&
      ["rule", "warn"].includes(items[id]?.verdict || ""),
  ).length;
  if (evaluated !== count)
    return {
      status: "refused",
      findings: [
        ...findings,
        {
          code: "Evaluation",
          item_id: null,
          message: "Mock cannot evaluate every rule in this draft.",
        },
      ],
      cards,
    };
  return {
    status: "revised",
    findings,
    cards,
    rules: { count, evaluated },
    definition: {
      ...structuredClone(definition),
      ontology_toml,
      mappings_toml,
      draft: { ...draft, ontology_toml, mappings_toml },
      rules: { count, evaluated },
    },
  };
}

/**
 * Comparable (the engine's `comparable`): the draft `a` has
 * the loaded version `b`'s types, keys, props and links, and may add props
 * and link definitions; identifiers and rules may differ.
 */
export function comparableSchema(
  a: Record<string, unknown>,
  b: Record<string, unknown>,
): boolean {
  if (a.format !== "agent_bundle" || b.format !== "agent_bundle") return false;
  const read = (d: Record<string, unknown>) =>
    toml.parse(readAgentBundle(d).ontologyToml) as Record<string, any>;
  const stable = (v: any): any =>
    Array.isArray(v)
      ? v.map(stable)
      : v && typeof v === "object"
        ? Object.fromEntries(
            Object.entries(v)
              .sort(([a], [b]) => a.localeCompare(b))
              .map(([k, v]) => [k, stable(v)]),
          )
        : v;
  const same = (x: unknown, y: unknown) =>
    JSON.stringify(stable(x)) === JSON.stringify(stable(y));
  const draft = read(a),
    loaded = read(b);
  const types = (o: Record<string, any>): Record<string, any> => o.types || {},
    links = (o: Record<string, any>): Record<string, any> => o.links || {},
    shape = (l: any) => ({
      from: l.from,
      to: l.to,
      props: l.props || {},
      identity: l.identity,
    });
  return (
    same(Object.keys(types(draft)).sort(), Object.keys(types(loaded)).sort()) &&
    Object.entries(types(loaded)).every(([name, t]) => {
      const d = types(draft)[name];
      return (
        same(d.key, t.key) &&
        Object.entries(t.props || {}).every(
          ([prop, type]) => (d.props || {})[prop] === type,
        )
      );
    }) &&
    Object.entries(links(loaded)).every(
      ([name, l]) =>
        !!links(draft)[name] && same(shape(links(draft)[name]), shape(l)),
    )
  );
}
