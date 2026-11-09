import { describe, expect, it } from "vitest";
import * as model from "@/web/areas/reviews/model";
import { MAX_GOAL } from "@/web/areas/ontology/model";
import type { ReviewDecision } from "@/shared/wfe";

const review = (reason: string, candidates = 0, tag = "") => ({
  reason,
  candidates: Array.from({ length: candidates }, (_, i) => `c${i}`),
  tag,
});
const order = (reviews: ReturnType<typeof review>[]) =>
  model.reviewGroups(reviews).map((g) => g.reason);

describe("Needs review's groups", () => {
  it("put what a person can act on first, then the rest as it first appears", () => {
    expect(
      order([
        review("violation"),
        review("unresolved"),
        review("unmatched"),
        review("prop_conflict"),
        review("legal_form"),
        review("unverified"),
        review("low_confidence"),
        review("adjudicated"),
      ]),
    ).toEqual([
      "low_confidence",
      "unverified",
      "legal_form",
      "prop_conflict",
      "unmatched",
      "violation",
      "unresolved",
      "adjudicated",
    ]);
  });
  it("say their reason once, and keep their reviews' order and indexes", () => {
    const groups = model.reviewGroups([
      review("unresolved", 1, "a"),
      review("low_confidence", 0, "b"),
      review("unresolved", 2, "c"),
      review("low_confidence", 0, "d"),
      review("low_confidence", 0, "e"),
    ]);
    expect(
      groups.map((g) => ({
        title: g.title,
        text: g.text,
        items: g.items.map(({ review, index }) => [review.tag, index]),
      })),
    ).toEqual([
      {
        title: "The model wasn’t sure",
        text: "The fact was read with low confidence, so it wasn’t stored.",
        items: [
          ["b", 1],
          ["d", 3],
          ["e", 4],
        ],
      },
      {
        title: "Names something that isn’t in your data",
        text: "No entity has this name, and text never creates one, so nothing was stored about it.",
        items: [
          ["a", 0],
          ["c", 2],
        ],
      },
    ]);
  });
  it("part an ambiguous name with one candidate from one with several, as their cards do", () => {
    const groups = model.reviewGroups([
      review("ambiguous", 2, "many"),
      review("ambiguous", 1, "one"),
      review("ambiguous", 3, "more"),
    ]);
    expect(
      groups.map((g) => [g.title, g.items.map((i) => i.review.tag)]),
    ).toEqual([
      ["Could mean more than one entity", ["many", "more"]],
      ["May be this entity", ["one"]],
    ]);
  });
  it("gather reasons this site doesn't know into one, after the ones it does", () => {
    const groups = model.reviewGroups([
      review("newer_reason"),
      review("unmatched"),
      review("another_new_reason"),
    ]);
    expect(groups.map((g) => [g.title, g.items.length])).toEqual([
      ["A relationship the ontology doesn’t have", 1],
      ["Held for a person to check", 2],
    ]);
    expect(groups[1]!.reason).toBe("newer_reason");
  });
  it("are none for no reviews", () => {
    expect(model.reviewGroups([])).toEqual([]);
  });
});

// A held claim as the engine stores its review, at its own place.
const held = (
  start: number,
  object = "Initech LLC",
  reason = "low_confidence",
) => ({
  review_key: `held-${start}`,
  reason,
  doc_id: "doc",
  claim_key: "a".repeat(64),
  section: 0,
  start,
  end: start + 10,
  suggested_link: "signed_with",
  claim: JSON.stringify({ about: [1, 2] }),
  statement: JSON.stringify({
    subject: { type: "Contract", name: "Hedged deal" },
    object: { type: "Customer", name: object },
    props: {},
  }),
});
const settled = (r: Record<string, unknown>) => ({
  reason: String(r.reason),
  ...model.settleable(r, []),
});
const approved = (start: number, statementHash = "b".repeat(64)) =>
  ({
    kind: "release",
    type: "",
    docId: "doc",
    claimKey: "a".repeat(64),
    claimSection: 0,
    claimStart: start,
    claimEnd: start + 10,
    statementHash,
  }) satisfies ReviewDecision;
const ids = (s: { reviewId: string }[]) => s.map((r) => r.reviewId);
const many = () => false;

describe("what Approve all approves", () => {
  it("is every held claim a card could approve now, of either release reason", () => {
    expect(
      ids(
        model.approvable(
          [settled(held(0)), settled(held(20, "Globex", "unverified"))],
          [],
          many,
        ),
      ),
    ).toEqual(["held-0", "held-20"]);
  });
  it("leaves out a claim already decided, even one approved as it read before", () => {
    const reviews = [settled(held(0)), settled(held(20)), settled(held(40))];
    expect(
      ids(model.approvable(reviews, [approved(0), approved(40, "0")], many)),
    ).toEqual(["held-20"]);
  });
  it("leaves out a sentence read two ways", () => {
    const [one, other, alone] = model.markDiffering([
      settled(held(0, "Initech LLC")),
      settled(held(0, "Globex")),
      settled(held(20)),
    ]);
    expect(ids(model.approvable([one!, other!, alone!], [], many))).toEqual([
      "held-20",
    ]);
  });
  it("leaves out a name only a legal form linked, on a link that holds one value", () => {
    const [claim] = model.markDiffering([
      settled(held(0, "Initech LLC")),
      {
        reason: "legal_form",
        candidates: [],
        settle: {
          kind: "resolve",
          reviewId: "form",
          type: "Customer",
          docId: "doc",
          normName: "initech llc",
        },
      },
    ]);
    expect(claim!.settle).toMatchObject({ formed: "initech llc" });
    expect(model.approvable([claim!], [], () => true)).toEqual([]);
    // A link that holds many writes it anyway: the card offers it too.
    expect(ids(model.approvable([claim!], [], many))).toEqual(["held-0"]);
    // Only the claim's own link decides.
    expect(
      ids(model.approvable([claim!], [], (link) => link !== "signed_with")),
    ).toEqual(["held-0"]);
  });
  it("leaves out a stale claim, and reviews no release settles", () => {
    const { statement: _, ...unstated } = held(0);
    expect(
      model.approvable(
        [
          settled(unstated),
          settled({
            review_key: "r",
            reason: "unresolved",
            type: "Customer",
            doc_id: "doc",
            norm_name: "umbrella holdings",
            candidates: [],
          }),
          { candidates: [] },
        ],
        [],
        many,
      ),
    ).toEqual([]);
  });
  it("goes by each claim's own place, whatever else is decided", () => {
    const decided: ReviewDecision[] = [
      { ...approved(0), claimKey: "c".repeat(64) },
      { ...approved(0), docId: "other" },
      {
        kind: "resolve",
        type: "Customer",
        docId: "doc",
        normName: "initech llc",
        entityKey: "cus_1",
      },
    ];
    expect(ids(model.approvable([settled(held(0))], decided, many))).toEqual([
      "held-0",
    ]);
  });
});

const unmatched = (suggestedLink: string | undefined, quote: string) => ({
  suggestedLink,
  quote,
});
const PREFIX = "Add links the documents state that the ontology lacks: ";
const length = (s: string) => [...s].length;

describe("the goal Add these links fills Generate with", () => {
  it("names each link once, the most suggested first, with two of its quotes", () => {
    expect(
      model.linksGoal([
        unmatched("evaluating_competitor", "Acme Corp is looking at Initech."),
        unmatched("supports", "The board still backs Acme Corp."),
        unmatched("Supports", "Globex supports the renewal."),
        unmatched("supports", "  The board\n still   backs Acme Corp. "),
        unmatched("SUPPORTS", "A third quote is left out."),
      ]),
    ).toBe(
      `${PREFIX}supports (“The board still backs Acme Corp.”, “Globex supports the renewal.”), evaluating_competitor (“Acme Corp is looking at Initech.”).`,
    );
  });
  it("takes spellings that differ in case, spaces, underscores or hyphens as one link", () => {
    expect(
      model.linksGoal([
        unmatched("supplier of", "One."),
        unmatched("Supplier_Of", "Two."),
        unmatched("supplier-of", "Three."),
        unmatched("supplied by", "Four."),
      ]),
    ).toBe(`${PREFIX}supplier of (“One.”, “Two.”), supplied by (“Four.”).`);
  });
  it("keeps links suggested as often in the order they first appear", () => {
    expect(
      model.linksGoal([unmatched("b_link", "b."), unmatched("a_link", "a.")]),
    ).toBe(`${PREFIX}b_link (“b.”), a_link (“a.”).`);
  });
  it("names a link without quotes when its reviews quote nothing", () => {
    expect(model.linksGoal([unmatched("partner_of", "  ")])).toBe(
      `${PREFIX}partner_of.`,
    );
  });
  it("is none when no review names a link", () => {
    expect(model.linksGoal([])).toBeUndefined();
    expect(
      model.linksGoal([unmatched(undefined, "q."), unmatched(" ", "q.")]),
    ).toBeUndefined();
  });
  it("leaves out links that would take it past MAX_GOAL characters", () => {
    const reviews = Array.from({ length: 200 }, (_, i) =>
      unmatched(`link_${i}`, `Quote number ${i} of the documents.`),
    );
    const goal = model.linksGoal(reviews)!;
    expect(length(goal)).toBeLessThanOrEqual(MAX_GOAL);
    expect(goal).toMatch(/^Add links .*link_0 \(“Quote number 0/);
    expect(goal).not.toContain("link_199");
  });
  it("gives a link fewer quotes, then none, before leaving it out", () => {
    const long = "x".repeat(MAX_GOAL);
    const goal = model.linksGoal([
      unmatched("first", "Short."),
      unmatched("first", long),
      unmatched("second", long),
      unmatched("third", "y".repeat(MAX_GOAL / 2)),
      unmatched("x".repeat(MAX_GOAL), "Too long a name."),
    ])!;
    expect(goal.startsWith(`${PREFIX}first (“Short.”), second, third`)).toBe(
      true,
    );
    expect(goal).not.toContain("Too long a name");
    expect(length(goal)).toBeLessThanOrEqual(MAX_GOAL);
  });
  it("counts characters as Generate does, by code point", () => {
    // Each 🔗 is two UTF-16 units but one character.
    const room = MAX_GOAL - length(`${PREFIX}l (“”).`);
    const exact = model.linksGoal([unmatched("l", "🔗".repeat(room))])!;
    expect(length(exact)).toBe(MAX_GOAL);
    expect(exact.length).toBeGreaterThan(MAX_GOAL);
    expect(model.linksGoal([unmatched("l", "🔗".repeat(room + 1))])).toBe(
      `${PREFIX}l.`,
    );
  });
});
