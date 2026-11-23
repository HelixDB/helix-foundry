import type { FoundryReviewChoice } from "../../../shared/wfe";
import { propValueText, type Candidate, type Settleable } from "./model";

/**
 * One choice a review can be settled with, as a card offers it: its id (the
 * same while the review is), its words, what is said once it is chosen
 * (`done`) and the choice the backend takes.
 */
export type Choice = {
  id: string;
  label: string;
  done: string;
  decision: FoundryReviewChoice;
  kind: "link" | "not_in_data" | "value" | "approve" | "reject";
};

/**
 * A type's name after its article, by how it is said: "an Order", "a User",
 * "an Umbrella" (a U said "you" takes "a").
 *
 * @example
 * aType("Customer"); // "a Customer"
 * aType("Account"); // "an Account"
 * aType("User"); // "a User"
 */
export const aType = (type: string) =>
  `${/^[aeio]|^u(?![^aeiou][aeiou])/i.test(type) ? "an" : "a"} ${type}`;

/**
 * What marking a name as none of its type's records does, as the engine
 * settles it (resolve `notInData`): on the review's document only, in the
 * current load at once and in later loads, the name links nothing and no
 * record is made of it, and what the document states of it is dropped, not
 * held for review (its held claims go with its review).
 *
 * @example
 * notInDataText("Umbrella Holdings", "Customer");
 * // "“Umbrella Holdings” isn’t a Customer in your data: it stays unlinked in this document and no Customer is made. Facts and held claims that name it here are dropped."
 */
export const notInDataText = (name: string, type: string) =>
  `“${name}” isn’t ${aType(type)} in your data: it stays unlinked in this document and no ${type} is made. Facts and held claims that name it here are dropped.`;

/**
 * Every choice a review offers, in the order a card numbers them: a name's
 * candidates (by `nameOf`, with their keys where two read the same), then
 * that it is none of its type's records ("Not a Customer we have",
 * `notInDataText` says what that does); a value's stated values, each once; a held claim's Approve
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
            done: "Linked",
            decision: { resolve: { entityKey: c.key } },
            kind: "link",
          };
        }),
        {
          id: "not_in_data",
          label: `Not ${aType(settle.type)} we have`,
          done: `Marked not ${aType(settle.type)}`,
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
        done: "Value chosen",
        decision: { prop: value },
        kind: "value",
      }));
    case "release":
      return [
        {
          id: "approve",
          label: "Approve",
          done: "Approved",
          decision: { release: { statementHash: settle.hash } },
          kind: "approve",
        },
        {
          id: "reject",
          label: "Reject",
          done: "Rejected",
          decision: { reject: { statementHash: settle.hash } },
          kind: "reject",
        },
      ];
    case "stale":
    case undefined:
      return [];
  }
}

/**
 * How a card offers its choices: Founder's suggestion (`primary`, one of
 * the choices), which Enter takes, then every other choice in order with
 * its key (`rest`): N for a name that is none of its type's records, X for Reject, and the rest numbered
 * 1 to 9 as they show (none past 9). Digit n takes `numbered[n - 1]`, so
 * the digits are the ones shown; the suggestion has none, as it shows none.
 */
export type ChoiceLayout = {
  primary?: Choice;
  rest: readonly { choice: Choice; key?: string }[];
  numbered: readonly Choice[];
};

/**
 * Lays `choices` out around the suggested one (none: no primary).
 *
 * @example
 * const [approve, reject] = choicesOf(held, nameOf);
 * layoutChoices([approve, reject], approve);
 * // { primary: approve, rest: [{ choice: reject, key: "X" }], numbered: [] }
 */
export function layoutChoices(
  choices: readonly Choice[],
  suggested?: Choice,
): ChoiceLayout {
  const rest = choices.filter((c) => c !== suggested);
  const numbered = rest
    .filter((c) => c.kind !== "not_in_data" && c.kind !== "reject")
    .slice(0, 9);
  return {
    ...(suggested && choices.includes(suggested) ? { primary: suggested } : {}),
    rest: rest.map((choice) => {
      const key =
        choice.kind === "not_in_data"
          ? "N"
          : choice.kind === "reject"
            ? "X"
            : numbered.includes(choice)
              ? String(numbered.indexOf(choice) + 1)
              : undefined;
      return key ? { choice, key } : { choice };
    }),
    numbered,
  };
}

/**
 * What F puts in Founder's composer on a card: the rule's suggestion in
 * words, when it makes one (`suggested`, the card's suggested choice), then
 * the ask. The page context tells Founder the card; nothing is sent until
 * the person sends it.
 *
 * @example
 * founderAsk({ kind: "link", label: "Acme Ltd", … }); // "The rule suggests linking Acme Ltd. Help me decide this review."
 * founderAsk(undefined); // "Help me decide this review."
 */
export function founderAsk(suggested: Choice | undefined): string {
  const ask = "Help me decide this review.";
  if (!suggested) return ask;
  const words = {
    link: `linking ${suggested.label}`,
    not_in_data: `marking it “${suggested.label}”`,
    value: `the value ${suggested.label}`,
    approve: "approving it",
    reject: "rejecting it",
  }[suggested.kind];
  return `The rule suggests ${words}. ${ask}`;
}
