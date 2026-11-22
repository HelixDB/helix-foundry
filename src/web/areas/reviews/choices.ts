import type { FoundryReviewChoice } from "../../../shared/wfe";
import { propValueText, type Candidate, type Settleable } from "./model";

/**
 * One choice a review can be settled with, as a card offers it: its id (the
 * same while the review is), its words and the choice the backend takes.
 */
export type Choice = {
  id: string;
  label: string;
  decision: FoundryReviewChoice;
  kind: "link" | "not_in_data" | "value" | "approve" | "reject";
};

/**
 * Every choice a review offers, in the order a card numbers them: a name's
 * candidates (by `nameOf`, with their keys where two read the same), then
 * Not in data; a value's stated values, each once; a held claim's Approve
 * and Reject, bound to the statement read. None for a review no decision
 * settles here (stale, or no settle).
 *
 * @example
 * choicesOf({ candidates: [{ key: "scus_1" }], settle: name }, (c) => c.key).map((c) => c.id);
 * // ["key:scus_1", "not_in_data"]
 */
export function choicesOf(
  s: Settleable,
  nameOf: (c: Candidate) => string,
): Choice[] {
  const settle = s.settle;
  switch (settle?.kind) {
    case "resolve":
      return [
        ...s.candidates.map((c): Choice => {
          const name = nameOf(c);
          return {
            id: `key:${c.key}`,
            label: s.candidates.some(
              (other) => other !== c && nameOf(other) === name,
            )
              ? `${name} (${c.key})`
              : name,
            decision: { resolve: { entityKey: c.key } },
            kind: "link",
          };
        }),
        {
          id: "not_in_data",
          label: "Not in data",
          decision: { resolve: { notInData: true } },
          kind: "not_in_data",
        },
      ];
    case "prop":
      return [
        ...new Map(
          settle.values.map((v) => [JSON.stringify(v.value), v.value]),
        ).entries(),
      ].map(([json, value]) => ({
        id: `value:${json}`,
        label: propValueText(value),
        decision: { prop: value },
        kind: "value",
      }));
    case "release":
      return [
        {
          id: "approve",
          label: "Approve",
          decision: { release: { statementHash: settle.hash } },
          kind: "approve",
        },
        {
          id: "reject",
          label: "Reject",
          decision: { reject: { statementHash: settle.hash } },
          kind: "reject",
        },
      ];
    case "stale":
    case undefined:
      return [];
  }
}
