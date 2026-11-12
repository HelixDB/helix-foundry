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
        review("own_name"),
        review("part_name"),
        review("legal_form"),
        review("unverified"),
        review("low_confidence"),
        review("adjudicated"),
      ]),
    ).toEqual([
      "low_confidence",
      "unverified",
      "legal_form",
      "part_name",
      "own_name",
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
  it("leaves out a claim held as conflicting with what its link holds, which a many-valued link never writes", () => {
    const reviews = [settled(held(0)), settled(held(20, "Globex", "conflict"))];
    expect(ids(model.approvable(reviews, [], many))).toEqual(["held-0"]);
  });
  it("leaves every claim of a link that holds one value to its card: an approval decides what it holds (contract 4.14.0)", () => {
    const reviews = [
      settled(held(0, "Initech LLC")),
      settled(held(20, "Globex")),
      settled(held(40, "Initech LLC")),
      settled(held(60, "Globex", "conflict")),
    ];
    // Whatever the link holds now, and whoever put it there: a page can't
    // tell a person's approval or a table's row from a document's.
    expect(ids(model.approvable(reviews, [], () => true))).toEqual([]);
    expect(ids(model.approvable([reviews[0]!], [], () => true))).toEqual([]);
    // On a link that holds many, each is its own fact.
    expect(ids(model.approvable(reviews.slice(0, 3), [], many))).toEqual([
      "held-0",
      "held-20",
      "held-40",
    ]);
    // Only the claim's own link decides.
    expect(
      ids(
        model.approvable(
          [
            settled(held(0)),
            settled({ ...held(20), suggested_link: "partner_of" }),
          ],
          [],
          (link) => link === "signed_with",
        ),
      ),
    ).toEqual(["held-20"]);
  });
  it("leaves out a claim already decided, even one approved as it read before", () => {
    const reviews = [settled(held(0)), settled(held(20)), settled(held(40))];
    expect(
      ids(model.approvable(reviews, [approved(0), approved(40, "0")], many)),
    ).toEqual(["held-20"]);
  });
  it("leaves out a claim approved or rejected under the key it had before, of what it reads", () => {
    // A rebuild keyed the sentence otherwise: the engine binds the approval
    // by its place and what it reads (contract 4.14.0), and approving the
    // card again would only approve that decision again.
    const reviews = [settled(held(0)), settled(held(20)), settled(held(40))];
    const read = (start: number) =>
      model.statementHash("signed_with", held(start).statement);
    const moved = (start: number) => ({
      ...approved(start, read(start)),
      claimKey: "c".repeat(64),
    });
    expect(
      ids(
        model.approvable(
          reviews,
          [moved(0), { ...moved(40), kind: "reject" as const }],
          many,
        ),
      ),
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
    expect(claim!.settle).toMatchObject({
      unsettled: [{ name: "Initech LLC", by: "legal_form" }],
    });
    expect(model.approvable([claim!], [], () => true)).toEqual([]);
    // A link that holds many writes it anyway: the card offers it too.
    expect(ids(model.approvable([claim!], [], many))).toEqual(["held-0"]);
    // Only the claim's own link decides.
    expect(
      ids(model.approvable([claim!], [], (link) => link !== "signed_with")),
    ).toEqual(["held-0"]);
  });
  it.each(["part_name", "own_name"])(
    "leaves out a name linked by %s, whatever its link holds",
    (reason) => {
      const [claim] = model.markDiffering([
        settled(held(0, "Initech LLC")),
        {
          reason,
          candidates: [],
          settle: {
            kind: "resolve",
            reviewId: "part",
            type: "Customer",
            docId: "doc",
            normName: "initech llc",
          },
        },
      ]);
      expect(model.approvable([claim!], [], () => true)).toEqual([]);
      // The engine refuses it on a link that holds many too.
      expect(model.approvable([claim!], [], many)).toEqual([]);
    },
  );
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
  it("leaves out a claim a person rejected", () => {
    const reviews = [settled(held(0)), settled(held(20))];
    expect(
      ids(
        model.approvable(
          reviews,
          [{ ...approved(0), kind: "reject" as const }],
          many,
        ),
      ),
    ).toEqual(["held-20"]);
  });
});

describe("the reviews a page lists once claims are rejected", () => {
  const rejected = (start: number) =>
    ({
      ...approved(start),
      kind: "reject" as const,
    }) satisfies ReviewDecision;
  const name = settled({
    review_key: "r",
    reason: "unresolved",
    type: "Customer",
    doc_id: "doc",
    norm_name: "umbrella holdings",
    candidates: [],
  });
  it("are all of them while none is rejected", () => {
    const reviews = [settled(held(0)), name, settled(held(20))];
    expect(model.unrejected(reviews, [])).toEqual(reviews);
    // An approval hides nothing: its card says what became of it.
    expect(model.unrejected(reviews, [approved(0)])).toEqual(reviews);
  });
  it("leave out each held claim a reject names, and keep their order", () => {
    const reviews = [
      settled(held(0)),
      name,
      settled(held(20)),
      settled(held(40, "Globex", "unverified")),
    ];
    const left = model.unrejected(reviews, [rejected(0), rejected(40)]);
    expect(left).toEqual([name, reviews[2]]);
    // So a page's count, its groups and Approve all follow.
    expect(model.reviewGroups(left).map((g) => g.items.length)).toEqual([1, 1]);
    expect(
      ids(model.approvable(left, [rejected(0), rejected(40)], many)),
    ).toEqual(["held-20"]);
  });
  it("go by the claim's document, key and place", () => {
    const reviews = [settled(held(0))];
    for (const other of [
      { ...rejected(0), docId: "other" },
      { ...rejected(0), claimKey: "c".repeat(64) },
      { ...rejected(0), claimSection: 1 },
      { ...rejected(0), claimStart: 1 },
      { ...rejected(0), claimEnd: 11 },
    ] satisfies ReviewDecision[])
      expect(model.unrejected(reviews, [other])).toEqual(reviews);
    // However it reads now: the person said that claim there is wrong.
    expect(
      model.unrejected(reviews, [{ ...rejected(0), statementHash: "0" }]),
    ).toEqual([]);
  });
  it("follow what a reject read at the place under the key a rebuild gave the claim", () => {
    const reviews = [settled(held(0)), settled(held(0, "Globex"))];
    const read = model.statementHash("signed_with", held(0).statement);
    // Under another key, of what the first card reads: that card goes, as
    // the backend keeps every approval of it from the engine; the sibling
    // claim of the sentence stays.
    const moved = {
      ...rejected(0),
      claimKey: "c".repeat(64),
      statementHash: read,
    };
    expect(model.unrejected(reviews, [moved])).toEqual([reviews[1]]);
    // Not at another place, nor of another document.
    for (const other of [
      { ...moved, claimStart: 1 },
      { ...moved, docId: "other" },
    ])
      expect(model.unrejected(reviews, [other])).toEqual(reviews);
    // An approval hides nothing.
    expect(
      model.unrejected(reviews, [{ ...moved, kind: "release" as const }]),
    ).toEqual(reviews);
  });
  it("hide every reading of a sentence read two ways", () => {
    const twice = model.markDiffering([
      settled(held(0, "Initech LLC")),
      settled(held(0, "Globex")),
    ]);
    expect(model.unrejected(twice, [rejected(0)])).toEqual([]);
  });
  it("keep a review no reject can name", () => {
    const { statement: _, ...unstated } = held(0);
    const stale = settled(unstated);
    expect(model.unrejected([stale, name], [rejected(0)])).toEqual([
      stale,
      name,
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
  it("names every link that fits, the most suggested first, before any quote", () => {
    const reviews = Array.from({ length: 400 }, (_, i) =>
      unmatched(`link_${i}`, `Quote number ${i} of the documents.`),
    );
    const goal = model.linksGoal(reviews)!;
    expect(length(goal)).toBeLessThanOrEqual(MAX_GOAL);
    const named = goal.slice(PREFIX.length, -1).split(", ");
    // As many names as fit, in order, and no room left for another.
    expect(named).toEqual(named.map((_, i) => `link_${i}`));
    expect(
      length(`${goal.slice(0, -1)}, link_${named.length}.`),
    ).toBeGreaterThan(MAX_GOAL);
  });
  it("gives quotes the room the names leave, every link's first before any second", () => {
    const room = MAX_GOAL - length(`${PREFIX}a (“”, “”).`);
    const [first, second] = [
      "x".repeat(Math.floor(room / 2)),
      "y".repeat(Math.ceil(room / 2)),
    ];
    // Both of a's quotes would fill it, leaving b out.
    expect(
      model.linksGoal([
        unmatched("a", first!),
        unmatched("a", second!),
        unmatched("b", "Short."),
      ]),
    ).toBe(`${PREFIX}a (“${first}”), b (“Short.”).`);
    expect(
      model.linksGoal([
        unmatched("a", "q."),
        unmatched("b", "r."),
        unmatched("a", "s."),
      ]),
    ).toBe(`${PREFIX}a (“q.”, “s.”), b (“r.”).`);
  });
  it("takes a link's second quote alone when its first doesn't fit", () => {
    expect(
      model.linksGoal([
        unmatched("a", "x".repeat(MAX_GOAL)),
        unmatched("a", "Short."),
      ]),
    ).toBe(`${PREFIX}a (“Short.”).`);
  });
  it("leaves out a link whose name alone doesn't fit", () => {
    expect(
      model.linksGoal([
        unmatched("x".repeat(MAX_GOAL), "q."),
        unmatched("b", "r."),
      ]),
    ).toBe(`${PREFIX}b (“r.”).`);
    expect(
      model.linksGoal([unmatched("x".repeat(MAX_GOAL), "q.")]),
    ).toBeUndefined();
  });
  it("counts characters as Generate does, by code point", () => {
    // Each 🔗 is two UTF-16 units but one character.
    const room = MAX_GOAL - length(`${PREFIX}l (“”).`);
    const quote = (n: number) => "🔗".repeat(10) + "a".repeat(n - 10);
    const exact = model.linksGoal([unmatched("l", quote(room))])!;
    expect(length(exact)).toBe(MAX_GOAL);
    expect(exact.length).toBeGreaterThan(MAX_GOAL);
    expect(model.linksGoal([unmatched("l", quote(room + 1))])).toBe(
      `${PREFIX}l.`,
    );
  });
  it("stays short enough to open Generate with, in any script", () => {
    // Each あ is nine characters in a URL.
    const fits = model.linksGoal([unmatched("l", "あ".repeat(700))])!;
    expect(fits).toContain("あ");
    expect(encodeURIComponent(fits).length).toBeLessThanOrEqual(8000);
    expect(model.linksGoal([unmatched("l", "あ".repeat(1500))])).toBe(
      `${PREFIX}l.`,
    );
  });
});
