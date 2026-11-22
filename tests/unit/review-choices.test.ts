import { describe, expect, it } from "vitest";
import { choicesOf } from "@/web/areas/reviews/choices";
import { decisionFor, type Candidate } from "@/web/areas/reviews/model";
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
    // Each is a choice the review takes.
    expect(choices.map((c) => c.decision)).toEqual([
      decisionFor(card.review, { entityKey: "scus_1" }),
      decisionFor(card.review, { entityKey: "scus_2" }),
      decisionFor(card.review, { entityKey: null }),
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
