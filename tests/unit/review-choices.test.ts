import { describe, expect, it } from "vitest";
import { choicesOf, layoutChoices } from "@/web/areas/reviews/choices";
import type { Candidate } from "@/web/areas/reviews/model";
import {
  entity,
  heldCard,
  nameCard,
  valueCard,
} from "./__fixtures__/review-cards";

// The choices a review offers (reviews/choices.ts), which a card numbers
// and Needs review's form lists.

const nameOf = (c: Candidate) => c.entity?.name ?? c.key;

describe("choicesOf", () => {
  it("offers a name's candidates, then Not in data", () => {
    const card = nameCard("umbrella", {
      candidates: [
        { key: "scus_1", entity: entity("scus_1", "Umbrella") },
        { key: "scus_2" },
      ],
    });
    const choices = choicesOf(card.review, nameOf);
    expect(choices).toEqual([
      {
        id: "key:scus_1",
        label: "Umbrella",
        decision: { resolve: { entityKey: "scus_1" } },
        kind: "link",
      },
      {
        id: "key:scus_2",
        label: "scus_2",
        decision: { resolve: { entityKey: "scus_2" } },
        kind: "link",
      },
      {
        id: "not_in_data",
        label: "Not in data",
        decision: { resolve: { notInData: true } },
        kind: "not_in_data",
      },
    ]);
  });
  it("tells candidates that read the same apart by their keys", () => {
    const card = nameCard("umbrella", {
      candidates: [
        { key: "scus_1", entity: entity("scus_1", "Umbrella") },
        { key: "scus_2", entity: entity("scus_2", "Umbrella") },
        { key: "scus_3", entity: entity("scus_3", "Globex") },
      ],
    });
    expect(choicesOf(card.review, nameOf).map((c) => c.label)).toEqual([
      "Umbrella (scus_1)",
      "Umbrella (scus_2)",
      "Globex",
      "Not in data",
    ]);
  });
  it("offers each stated value once", () => {
    const card = valueCard([
      { text: "GBP" },
      { integer: "36" },
      { text: "GBP" },
    ]);
    expect(choicesOf(card.review, nameOf)).toEqual([
      {
        id: 'value:{"text":"GBP"}',
        label: "GBP",
        decision: { prop: { text: "GBP" } },
        kind: "value",
      },
      {
        id: 'value:{"integer":"36"}',
        label: "36",
        decision: { prop: { integer: "36" } },
        kind: "value",
      },
    ]);
  });
  it("offers a held claim's Approve and Reject, bound to what it reads", () => {
    const card = heldCard(1);
    const hash =
      card.review.settle?.kind === "release" ? card.review.settle.hash : "";
    expect(choicesOf(card.review, nameOf)).toEqual([
      {
        id: "approve",
        label: "Approve",
        decision: { release: { statementHash: hash } },
        kind: "approve",
      },
      {
        id: "reject",
        label: "Reject",
        decision: { reject: { statementHash: hash } },
        kind: "reject",
      },
    ]);
  });
  it("offers nothing for a review no decision settles here", () => {
    expect(
      choicesOf({ candidates: [], settle: { kind: "stale" } }, nameOf),
    ).toEqual([]);
    expect(choicesOf({ candidates: [] }, nameOf)).toEqual([]);
  });
});

describe("layoutChoices", () => {
  const name = (n: number) =>
    choicesOf(
      nameCard("umbrella", {
        candidates: Array.from({ length: n }, (_, i) => ({
          key: `scus_${i + 1}`,
          entity: entity(`scus_${i + 1}`, `Company ${i + 1}`),
        })),
      }).review,
      nameOf,
    );
  const keys = (l: ReturnType<typeof layoutChoices>) =>
    l.rest.map((r) => [r.key, r.choice.label]);

  it("makes the suggestion the primary choice, shown once, and numbers the others as they show", () => {
    const choices = name(3);
    const l = layoutChoices(choices, choices[1]);
    expect(l.primary).toBe(choices[1]);
    expect(keys(l)).toEqual([
      ["1", "Company 1"],
      ["2", "Company 3"],
      ["N", "Not in data"],
    ]);
    // Digit n takes the choice shown with n.
    expect(l.numbered.map((c) => c.label)).toEqual(["Company 1", "Company 3"]);
  });
  it("has no primary without a suggestion, numbering every choice but Not in data and Reject", () => {
    const choices = name(2);
    const l = layoutChoices(choices);
    expect(l.primary).toBeUndefined();
    expect(keys(l)).toEqual([
      ["1", "Company 1"],
      ["2", "Company 2"],
      ["N", "Not in data"],
    ]);
    const held = choicesOf(heldCard(1).review, nameOf);
    const plain = layoutChoices(held);
    expect(keys(plain)).toEqual([
      ["1", "Approve"],
      ["X", "Reject"],
    ]);
    expect(plain.numbered).toEqual([held[0]]);
  });
  it("leaves a held claim's Reject as X once Approve is suggested, and no digit", () => {
    const held = choicesOf(heldCard(1).review, nameOf);
    const l = layoutChoices(held, held[0]);
    expect(l.primary).toBe(held[0]);
    expect(keys(l)).toEqual([["X", "Reject"]]);
    expect(l.numbered).toEqual([]);
  });
  it("makes Not in data the primary when it is suggested, keeping the candidates' digits", () => {
    const choices = name(1);
    const l = layoutChoices(choices, choices[1]);
    expect(l.primary?.kind).toBe("not_in_data");
    expect(keys(l)).toEqual([["1", "Company 1"]]);
  });
  it("numbers values, and none past 9", () => {
    const values = choicesOf(
      valueCard([{ text: "GBP" }, { text: "USD" }, { text: "EUR" }]).review,
      nameOf,
    );
    expect(keys(layoutChoices(values, values[2]))).toEqual([
      ["1", "GBP"],
      ["2", "USD"],
    ]);
    const many = layoutChoices(name(11));
    expect(many.numbered).toHaveLength(9);
    expect(keys(many).slice(8)).toEqual([
      ["9", "Company 9"],
      [undefined, "Company 10"],
      [undefined, "Company 11"],
      ["N", "Not in data"],
    ]);
  });
  it("ignores a suggestion that isn't one of the choices", () => {
    const choices = name(1);
    const [other] = name(1);
    const l = layoutChoices(choices, { ...other! });
    expect(l.primary).toBeUndefined();
    expect(l.rest).toHaveLength(2);
  });
});
