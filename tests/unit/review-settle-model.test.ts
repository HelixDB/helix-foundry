import { describe, expect, it } from "vitest";
import * as model from "@/web/areas/reviews/model";
import type { EntityRow } from "@/web/areas/explore/model";
import type { FoundryPropValue, ReviewDecision } from "@/shared/wfe";

const customer = (key: string, name: string, dbId = 1): EntityRow => ({
  dbId,
  id: `id-${key}`,
  type: "Customer",
  name,
  identity: { type: "Customer", key },
  props: { entity_key: key, company_name: name, currency: "usd" },
});
const globex = customer("scus_GLOBEX01", "Globex Corporation");
// A resolve review as the engine stores it: identity, candidates as keys.
const name = {
  review_key: "review-key",
  reason: "unresolved",
  type: "Customer",
  doc_id: "doc",
  norm_name: "globexx corporation",
  candidates: ["scus_GLOBEX01", "scus_GLOBAL02"],
};
const quote: model.ReviewValue = {
  value: { text: "GBP" },
  documentId: "doc",
  quote: "Globex Corporation is billed in GBP.",
  section: 0,
  start: 0,
  end: 36,
};
// A PropQuote record as the engine writes it, its value's JSON text as given.
const stored = (value: string) =>
  `{"value":${value},"doc_id":"${quote.documentId}","span":{"section":0,"start":0,"end":36},"quote":"${quote.quote}","confidence":0.94}`;
// A property review: the entity, the prop and the quotes that disagree.
const prop = {
  review_key: "prop-key",
  reason: "prop_conflict",
  type: "Customer",
  entity_key: "scus_GLOBEX01",
  prop: "currency",
  values: `[${stored('"GBP"')}]`,
  candidates: [],
};

const held = {
  review_key: "held-key",
  reason: "low_confidence",
  doc_id: "doc",
  claim_key: "a".repeat(64),
  section: 1,
  start: 4,
  end: 20,
  suggested_link: "promised_to",
  claim: JSON.stringify({
    about: [{ type: "Contract" }, { type: "Customer" }],
  }),
  statement: JSON.stringify({
    subject: { type: "Commitment", name: "24-hour support" },
    object: { type: "Account", name: "Acme Ltd" },
    props: { due_on: "2027-01-01", nested: {} },
  }),
};
const release = {
  kind: "release",
  reviewId: "held-key",
  docId: "doc",
  claimKey: held.claim_key,
  section: 1,
  start: 4,
  end: 20,
  link: "promised_to",
  statement: {
    subject: { type: "Commitment", name: "24-hour support" },
    object: { type: "Account", name: "Acme Ltd" },
    props: { due_on: "2027-01-01", nested: "{}" },
  },
  stored: held.statement,
  named: true,
} as const;
const decided = {
  kind: "release",
  type: "",
  docId: "doc",
  claimKey: held.claim_key,
  claimSection: 1,
  claimStart: 4,
  claimEnd: 20,
  statementHash: "b".repeat(64),
} as const;
describe("a held claim", () => {
  it.each(["low_confidence", "unverified"])(
    "is approved at its place, with what it writes: %s",
    (reason) => {
      expect(model.settleable({ ...held, reason }, []).settle).toEqual(release);
      expect(model.matchesReview(decided, release)).toBe(true);
    },
  );
  it("held as conflicting with what its link holds is approved the same way, said so (contract 4.14.0)", () => {
    const s = model.settleable({ ...held, reason: "conflict" }, []).settle;
    expect(s).toEqual({ ...release, conflict: true });
    expect(model.matchesReview(decided, s!)).toBe(true);
    // One a load wrote before claims kept statements is settled by nothing,
    // as before: no new load is asked for.
    const { statement: _, ...unstated } = held;
    expect(
      model.settleable({ ...unstated, reason: "conflict" }, []).settle,
    ).toBeUndefined();
  });
  it("is matched by its key and place", () => {
    for (const other of [
      { claimKey: "b".repeat(64) },
      { docId: "other" },
      { claimSection: 2 },
      { claimStart: 5 },
      { claimEnd: 21 },
    ])
      expect(model.matchesReview({ ...decided, ...other }, release)).toBe(
        false,
      );
  });
  it("says when reviews at one place state different things", () => {
    const review = (name: string) =>
      model.settleable(
        {
          ...held,
          statement: JSON.stringify({
            subject: { type: "Commitment", name },
            object: { type: "Account", name: "Acme Ltd" },
          }),
        },
        [],
      );
    const [one, other, alone] = model.markDiffering([
      review("support"),
      review("training"),
      model.settleable({ ...held, section: 2 }, []),
    ]);
    expect(one!.settle).toMatchObject({ kind: "release", differs: true });
    expect(other!.settle).toMatchObject({ kind: "release", differs: true });
    expect(alone!.settle).not.toHaveProperty("differs");
    const same = model.markDiffering([review("support"), review("support")]);
    expect(same[0]!.settle).not.toHaveProperty("differs");
  });
  it("shows what the approval writes to the digit", () => {
    const stored =
      '{"subject":{"type":"A","name":"a"},"object":{"type":"B","name":"b"},' +
      '"props":{"rate":0.0425,"share":0.12345,"seats":9007199254740993,"on":true,"due":"2027-01-01"}}';
    const settle = model.settleable({ ...held, statement: stored }, []).settle;
    expect(settle).toMatchObject({
      kind: "release",
      statement: {
        props: {
          rate: "0.0425",
          share: "0.12345",
          seats: "9007199254740993",
          on: "true",
          due: "2027-01-01",
        },
      },
    });
  });
  it("hides no property it would write", () => {
    const stored =
      '{"subject":{"type":"A","name":"a"},"object":{"type":"B","name":"b"},"props":{"many":[1,2]}}';
    expect(
      model.settleable({ ...held, statement: stored }, []).settle,
    ).toMatchObject({
      statement: { props: { many: "[1,2]" } },
    });
  });
  it("written before claims had keys or a place is loaded again", () => {
    const { claim_key: _, ...keyless } = held;
    expect(model.settleable(keyless, []).settle).toEqual({ kind: "stale" });
    const { section: __, ...placeless } = held;
    expect(model.settleable(placeless, []).settle).toEqual({ kind: "stale" });
  });
  it.each([
    ["one end", JSON.stringify({ about: [{ type: "Contract" }] })],
    ["no end", JSON.stringify({ about: [] })],
    ["text that doesn't read", "{"],
    ["nothing", undefined],
  ])("that names %s can be approved, and says so", (_, claim) => {
    expect(model.settleable({ ...held, claim }, []).settle).toEqual({
      ...release,
      named: false,
    });
  });
  it.each([
    ["nothing", undefined],
    ["text that doesn't read", "{"],
    ["no ends", "{}"],
    [
      "an end with no name",
      '{"subject":{"type":"A"},"object":{"type":"B","name":"b"}}',
    ],
  ])("whose statement is %s is loaded again", (_, stated) => {
    expect(model.settleable({ ...held, statement: stated }, []).settle).toEqual(
      { kind: "stale" },
    );
  });
  it("with no link is loaded again", () => {
    const { suggested_link: _, ...linkless } = held;
    expect(model.settleable(linkless, []).settle).toEqual({ kind: "stale" });
  });
  it("is hashed as the backend and the engine hash it", async () => {
    // The value held::Statement::hash and FoundryStatementHash are tested to give.
    const stored =
      '{"subject":{"type":"Company","name":"Zoë Ltd"},"object":{"type":"Company","name":"Acme"},"props":{"since":"2024"}}';
    const hash = await model.statementHash("partner_of", stored);
    expect(hash).toBe(
      "2c785db79d066c94d16e140edb1a8708834d7ecb12dda32c2f71dcdcc0ab0654",
    );
    expect(await model.statementHash("rival_of", stored)).not.toBe(hash);
  });
});

describe("how approvals stand toward a held claim", () => {
  const settle = model.settleable(held, []).settle as Extract<
    model.Settle,
    { kind: "release" }
  >;
  const read = "b".repeat(64);
  const saved = (over: Partial<typeof decided> = {}) => ({
    ...decided,
    id: "d1",
    ...over,
  });
  const other = "c".repeat(64);

  it("finds the approval of this review", () => {
    const s = model.approvalOf([saved()], settle, read, true);
    expect(s).toMatchObject({ moved: false });
    expect(s.saved?.id).toBe("d1");
  });
  it("says an approval of what it said before no longer stands", () => {
    const d = saved({ statementHash: other as never });
    expect(model.approvalOf([d], settle, read, true).moved).toBe(true);
  });
  it("waits for the statement's hash before it says so", () => {
    const d = saved({ statementHash: other as never });
    expect(model.approvalOf([d], settle, undefined, true).moved).toBe(false);
  });
  it("says neither on an older load, whose statement may be another's", () => {
    const moved = saved({ statementHash: other as never });
    expect(model.approvalOf([moved], settle, read, false).moved).toBe(false);
    const elsewhere = saved({ claimKey: "e".repeat(64) as never });
    expect(
      model.approvalOf([elsewhere], settle, read, false).saved,
    ).toBeUndefined();
  });
  it("tells a sibling claim at the sentence from this one's key moving", () => {
    // Another claim of the sentence, another statement: nothing of this one.
    const sibling = saved({
      claimKey: "e".repeat(64) as never,
      statementHash: other as never,
    });
    expect(model.approvalOf([sibling], settle, read, true)).toMatchObject({
      saved: undefined,
      moved: false,
    });
    // This statement under another key: the claim's key moved, and the
    // engine matches the approval by its place and hash (contract 4.14.0).
    const keyed = saved({ claimKey: "e".repeat(64) as never });
    expect(model.approvalOf([keyed], settle, read, true)).toMatchObject({
      saved: { id: "d1" },
      moved: false,
    });
    // One of this key comes first.
    expect(
      model.approvalOf(
        [keyed, saved({ id: "d2" as never })],
        settle,
        read,
        true,
      ).saved?.id,
    ).toBe("d2");
  });
  it("doesn't look at another place's approval", () => {
    const d = saved({
      claimKey: "e".repeat(64) as never,
      claimStart: 40 as never,
      claimEnd: 60 as never,
    });
    expect(model.approvalOf([d], settle, read, true).saved).toBeUndefined();
  });
});

describe("what became of an approval", () => {
  const review = model.settleable(held, []);
  const place = { section: 1, start: 4, end: 20 };
  const fact = (from: unknown, unread = false) =>
    ({
      link: "promised_to",
      factKey: "f",
      quotes: [{ ...place, quote: "q", link: "promised_to", factKey: "f" }],
      ...(from ? { from } : {}),
      ...(unread ? { unread } : {}),
    }) as never;
  const decision = { ...decided } as never;

  it("is still held while its review is", () => {
    expect(model.releaseStatus(decision, [review], [])).toBe("held");
  });
  it("is written when its quote stands on an edge", () => {
    expect(model.releaseStatus(decision, [], [fact({ id: "e" })])).toBe(
      "written",
    );
  });
  it("wrote no link when its quote stands on none", () => {
    expect(model.releaseStatus(decision, [], [fact(undefined)])).toBe(
      "no_link",
    );
  });
  it("isn't known past what a read takes, or where nothing is left", () => {
    expect(model.releaseStatus(decision, [], [fact(undefined, true)])).toBe(
      "unknown",
    );
    expect(model.releaseStatus(decision, [], [])).toBe("unknown");
  });
});

describe("a claim at the same place", () => {
  const stated = (who: string) =>
    JSON.stringify({
      subject: { type: "A", name: who },
      object: { type: "B", name: "b" },
      props: {},
    });
  it("of another reason counts as a reading: the engine compares it too", () => {
    const review = (reason: string, who: string) => ({
      reason,
      ...model.settleable({ ...held, reason, statement: stated(who) }, []),
    });
    const [approvable, conflict, violation] = model.markDiffering([
      review("low_confidence", "one"),
      review("conflict", "other"),
      review("violation", "third"),
    ]);
    expect(approvable!.settle).toMatchObject({ differs: true });
    // A conflict is a release too from contract 4.14.0, and as unsure.
    expect(conflict!.settle).toMatchObject({ conflict: true, differs: true });
    expect(violation!.settle).toBeUndefined();
    expect(violation!.claim).toBeDefined();
  });
  it("of another reason with no statement counts too", () => {
    const { statement: _, ...none } = held;
    const [approvable] = model.markDiffering([
      { reason: "low_confidence", ...model.settleable(held, []) },
      {
        reason: "conflict",
        ...model.settleable({ ...none, reason: "conflict" }, []),
      },
    ]);
    expect(approvable!.settle).toMatchObject({ differs: true });
  });
  it("names an end a legal form linked", () => {
    const named = {
      ...held,
      statement: JSON.stringify({
        subject: { type: "A", name: "Halvorsen Freight Ltd." },
        object: { type: "B", name: "b" },
        props: {},
      }),
    };
    const formed = (type: string, docId: string) =>
      model.markDiffering([
        { reason: "low_confidence", ...model.settleable(named, []) },
        {
          reason: "legal_form",
          candidates: [],
          settle: {
            kind: "resolve",
            reviewId: "r",
            type,
            docId,
            normName: "halvorsen freight ltd",
          },
        },
      ])[0]!.settle;
    // Named as the document writes it, not by the key it's matched on.
    expect(formed("A", "doc")).toMatchObject({
      unsettled: [{ name: "Halvorsen Freight Ltd.", by: "legal_form" }],
    });
    // A name resolves per document and type, so a link elsewhere holds nothing.
    expect(formed("A", "other-doc")).not.toHaveProperty("unsettled");
    expect(formed("B", "doc")).not.toHaveProperty("unsettled");
  });
  // A link by part of a row's name, or by an identifier the text uses as a
  // name (contract 4.13.0), and where its own review is.
  const linkedEnd = (
    reason: string,
    type: string,
    normName: string,
    at?: { section: number; start: number },
  ) => ({
    reason,
    candidates: [],
    ...at,
    settle: {
      kind: "resolve" as const,
      reviewId: `r-${reason}`,
      type,
      docId: "doc",
      normName,
    },
  });
  const claimOf = (subject: string, object: string) => ({
    reason: "low_confidence",
    ...model.settleable(
      {
        ...held,
        statement: JSON.stringify({
          subject: { type: "A", name: subject },
          object: { type: "B", name: object },
          props: {},
        }),
      },
      [],
    ),
  });
  it.each(["part_name", "own_name"] as const)(
    "names an end linked by %s, and where its review is",
    (by) => {
      const [claim] = model.markDiffering([
        claimOf("Westmere", "b"),
        linkedEnd(by, "A", "westmere", { section: 3, start: 41 }),
      ]);
      expect(claim!.settle).toMatchObject({
        unsettled: [{ name: "Westmere", by, at: { section: 3, start: 41 } }],
      });
    },
  );
  it("keeps an own-name identifier as the document writes it", () => {
    const [claim] = model.markDiffering([
      claimOf("INC-2031-0042", "b"),
      linkedEnd("own_name", "A", "inc 2031 0042"),
    ]);
    expect(claim!.settle).toMatchObject({
      unsettled: [{ name: "INC-2031-0042", by: "own_name" }],
    });
  });
  it("names a link it reads apart from the page's reviews: the other end's, on an entity's page", () => {
    const [claim, ...rest] = model.markDiffering(
      [claimOf("Halvorsen Freight Ltd.", "Westmere")],
      [linkedEnd("part_name", "B", "westmere", { section: 1, start: 7 })],
    );
    expect(rest).toEqual([]);
    expect(claim!.settle).toMatchObject({
      unsettled: [
        { name: "Westmere", by: "part_name", at: { section: 1, start: 7 } },
      ],
    });
  });
  it("names an entity a claim states of itself once", () => {
    const [claim] = model.markDiffering([
      {
        reason: "low_confidence",
        ...model.settleable(
          {
            ...held,
            statement: JSON.stringify({
              subject: { type: "A", name: "Westmere" },
              object: { type: "A", name: "Westmere" },
              props: {},
            }),
          },
          [],
        ),
      },
      linkedEnd("part_name", "A", "westmere"),
    ]);
    expect(
      (claim!.settle as { unsettled: model.Unsettled[] }).unsettled,
    ).toHaveLength(1);
  });
  it("names no place a page didn't read", () => {
    const [claim] = model.markDiffering([
      claimOf("Westmere", "b"),
      linkedEnd("part_name", "A", "westmere"),
    ]);
    expect(claim!.settle).toMatchObject({
      unsettled: [{ name: "Westmere", by: "part_name" }],
    });
    expect(
      (claim!.settle as { unsettled: model.Unsettled[] }).unsettled[0],
    ).not.toHaveProperty("at");
  });
  it("lists both ends in the order the engine refuses them: part of a name before a legal form", () => {
    const [claim] = model.markDiffering([
      claimOf("Halvorsen Freight Ltd.", "Westmere"),
      linkedEnd("legal_form", "A", "halvorsen freight ltd"),
      linkedEnd("part_name", "B", "westmere"),
    ]);
    expect(claim!.settle).toMatchObject({
      unsettled: [
        { name: "Westmere", by: "part_name" },
        { name: "Halvorsen Freight Ltd.", by: "legal_form" },
      ],
    });
    // Of two such links, subject before object.
    const [two] = model.markDiffering([
      claimOf("Tarnwick", "Westmere"),
      linkedEnd("part_name", "B", "westmere"),
      linkedEnd("own_name", "A", "tarnwick"),
    ]);
    expect(
      (two!.settle as { unsettled: model.Unsettled[] }).unsettled.map(
        (u) => u.name,
      ),
    ).toEqual(["Tarnwick", "Westmere"]);
  });
  it("leaves a name the model matched, or no one linked, alone", () => {
    for (const reason of ["adjudicated", "unresolved", "ambiguous"]) {
      const [claim] = model.markDiffering([
        claimOf("Westmere", "b"),
        linkedEnd(reason, "A", "westmere"),
      ]);
      expect(claim!.settle).not.toHaveProperty("unsettled");
    }
  });
});

describe("a link on less than a name", () => {
  it("is one by legal form, part of a name or an own-name identifier", () => {
    for (const r of ["legal_form", "part_name", "own_name"])
      expect([model.isWeakLink(r), model.isUndoableLink(r)]).toEqual([
        true,
        true,
      ]);
    expect([
      model.isWeakLink("adjudicated"),
      model.isUndoableLink("adjudicated"),
    ]).toEqual([false, true]);
    for (const r of ["name", "identifier", "alias", "user", "later", undefined])
      expect([model.isWeakLink(r), model.isUndoableLink(r)]).toEqual([
        false,
        false,
      ]);
  });
  it("holds an approval whatever its link holds, but a legal form only a one-valued one", () => {
    const end = (by: model.WeakLink) => ({ name: "n", by });
    for (const single of [true, false]) {
      expect(model.holdingEnds([end("part_name")], single)).toHaveLength(1);
      expect(model.holdingEnds([end("own_name")], single)).toHaveLength(1);
      expect(model.holdingEnds([end("legal_form")], single)).toHaveLength(
        single ? 1 : 0,
      );
      expect(model.holdingEnds(undefined, single)).toEqual([]);
      expect(model.holdingEnds([], single)).toEqual([]);
    }
    // Both ends, in order, where both hold.
    const both = [end("part_name"), end("legal_form")];
    expect(model.holdingEnds(both, true)).toEqual(both);
    expect(model.holdingEnds(both, false)).toEqual([end("part_name")]);
  });
});

describe("what settles a review", () => {
  it.each([
    "unresolved",
    "ambiguous",
    "collision",
    "identifier_conflict",
    "adjudicated",
    "legal_form",
    "part_name",
    "own_name",
  ])("a %s review with its identity takes a resolve choice", (reason) => {
    expect(model.settleable({ ...name, reason }, [globex])).toEqual({
      candidates: [
        { key: "scus_GLOBEX01", entity: globex },
        { key: "scus_GLOBAL02" },
      ],
      settle: {
        kind: "resolve",
        reviewId: "review-key",
        type: "Customer",
        docId: "doc",
        normName: "globexx corporation",
      },
    });
  });
  it.each(["review_key", "type", "doc_id", "norm_name"])(
    "a resolve review without %s is stale: the backend refuses it",
    (field) => {
      const r = { ...name, [field]: undefined };
      expect(model.settleable(r, [globex]).settle).toEqual({ kind: "stale" });
      expect(model.settleable({ ...name, [field]: "" }, []).settle).toEqual({
        kind: "stale",
      });
    },
  );
  it("names candidates only by an entity of the review's type and identity", () => {
    const named: EntityRow = {
      ...globex,
      type: "Company",
      identity: { type: "Company", name: "globex" },
    };
    const other = { ...globex, type: "Supplier" };
    const r = model.settleable(
      { ...name, type: "Company", candidates: ["globex", "scus_GLOBEX01"] },
      [named, other],
    );
    expect(r.candidates).toEqual([
      { key: "globex", entity: named },
      { key: "scus_GLOBEX01" },
    ]);
  });
  it("keeps each candidate once and drops ones that aren't text", () => {
    expect(
      model.settleable(
        { ...name, candidates: ["a", "a", "", 4, null, "b"] },
        [],
      ).candidates,
    ).toEqual([{ key: "a" }, { key: "b" }]);
    expect(
      model.settleable({ ...name, candidates: "a" }, []).candidates,
    ).toEqual([]);
  });
  it.each(["violation", "unmatched", "identity", undefined])(
    "nothing settles a %s review",
    (reason) => {
      expect(model.settleable({ ...name, reason }, [globex])).toEqual({
        candidates: [
          { key: "scus_GLOBEX01", entity: globex },
          { key: "scus_GLOBAL02" },
        ],
      });
    },
  );
  it("a row conflict carries the table's value it would replace", () => {
    expect(model.settleable(prop, [globex])).toEqual({
      candidates: [],
      settle: {
        kind: "prop",
        reviewId: "prop-key",
        type: "Customer",
        entityKey: "scus_GLOBEX01",
        prop: "currency",
        values: [quote],
        table: "usd",
      },
    });
  });
  it("a documents-only conflict has no stored value", () => {
    const contract: EntityRow = {
      dbId: 2,
      id: "id-contract",
      type: "Contract",
      name: "MSA",
      identity: { type: "Contract", name: "msa" },
      props: { norm_name: "msa", name: "MSA", odd: [1] },
    };
    for (const p of ["term_months", "odd"]) {
      const s = model.settleable(
        { ...prop, type: "Contract", entity_key: "msa", prop: p },
        [contract],
      ).settle;
      expect(s).toMatchObject({ kind: "prop", entityKey: "msa" });
      expect(s).not.toHaveProperty("table");
    }
  });
  it.each([
    { review_key: undefined },
    { type: "" },
    { entity_key: undefined },
    { prop: undefined },
    { values: "[]" },
    { values: "{broken" },
    { entity_key: "scus_OTHER" },
  ])(
    "a property review missing its identity, values or entity is stale (%o)",
    (bad) => {
      expect(model.settleable({ ...prop, ...bad }, [globex]).settle).toEqual({
        kind: "stale",
      });
    },
  );
});

describe("review choices", () => {
  const review = model.settleable(name, [globex]);
  // Each quote's value as the engine wrote it, and the arm that sends it.
  const stated = [
    ["0", { integer: "0" }],
    ["false", { boolean: false }],
    ['""', { text: "" }],
    ["36", { integer: "36" }],
    ["2.5", { number: 2.5 }],
    ["36.0", { number: 36 }],
    ["9007199254740993", { integer: "9007199254740993" }],
    ["-9223372036854775808", { integer: "-9223372036854775808" }],
  ] as const;
  const conflict = model.settleable(
    { ...prop, values: `[${stated.map(([raw]) => stored(raw)).join(",")}]` },
    [globex],
  );
  it("sends a listed candidate or explicit not-in-data", () => {
    expect(model.decisionFor(review, { entityKey: "scus_GLOBAL02" })).toEqual({
      resolve: { entityKey: "scus_GLOBAL02" },
    });
    expect(model.decisionFor(review, { entityKey: null })).toEqual({
      resolve: { notInData: true },
    });
  });
  it("refuses unlisted candidates and a value for a name", () => {
    expect(
      model.decisionFor(review, { entityKey: "invented" }),
    ).toBeUndefined();
    expect(
      model.decisionFor(review, { value: { integer: "36" } }),
    ).toBeUndefined();
  });
  it("sends the selected value in its arm, including false and zero, and an integer's every digit", () => {
    expect(
      conflict.settle?.kind === "prop" &&
        conflict.settle.values.map((v) => v.value),
    ).toEqual(stated.map(([, value]) => value));
    for (const [, value] of stated)
      expect(model.decisionFor(conflict, { value })).toEqual({ prop: value });
  });
  it("refuses an unlisted property value, one of another kind and a candidate for a property", () => {
    for (const value of [
      { integer: "48" },
      { text: "36" },
      { number: 0 },
      { integer: "9007199254740992" },
      undefined,
    ])
      expect(model.decisionFor(conflict, { value })).toBeUndefined();
    expect(
      model.decisionFor(conflict, { entityKey: "scus_GLOBEX01" }),
    ).toBeUndefined();
  });
  it("offers nothing for a stale review or one nothing settles", () => {
    const stale = model.settleable({ ...name, type: undefined }, []);
    expect(model.decisionFor(stale, { entityKey: null })).toBeUndefined();
    const other = model.settleable({ ...name, reason: "violation" }, []);
    expect(model.decisionFor(other, { entityKey: null })).toBeUndefined();
  });
  it("matches overrides by ontology identity, not review or load id", () => {
    const s = review.settle!,
      p = conflict.settle!;
    const d: ReviewDecision = {
      kind: "resolve",
      type: "Customer",
      docId: "doc",
      normName: "globexx corporation",
      entityKey: "scus_GLOBEX01",
    };
    expect(model.matchesReview(d, s)).toBe(true);
    for (const other of [
      { docId: "other" },
      { type: "Other" },
      { normName: "other" },
    ])
      expect(model.matchesReview({ ...d, ...other }, s)).toBe(false);
    const v: ReviewDecision = {
      kind: "prop",
      type: "Customer",
      entityKey: "scus_GLOBEX01",
      prop: "currency",
      value: { text: "GBP" },
    };
    expect(model.matchesReview(v, p)).toBe(true);
    for (const other of [
      { prop: "other" },
      { entityKey: "other" },
      { type: "Other" },
    ])
      expect(model.matchesReview({ ...v, ...other }, p)).toBe(false);
    // A decision of one kind never stands for a review of the other.
    expect(model.matchesReview(v, s)).toBe(false);
    expect(model.matchesReview(d, p)).toBe(false);
    expect(model.matchesReview(d, { kind: "stale" })).toBe(false);
  });
});

describe("whether a load read a decision", () => {
  const at = "2026-10-20T11:00:00.000Z";
  it("reads one saved before it started, not after", () => {
    expect(model.loadRead({ decidedAt: "2026-10-20T10:59:59Z" }, at)).toBe(
      true,
    );
    expect(model.loadRead({ decidedAt: "2026-10-20T11:00:01Z" }, at)).toBe(
      false,
    );
    expect(model.loadRead({ decidedAt: at }, at)).toBe(false);
  });
  it("reads none when either time is unknown", () => {
    expect(model.loadRead({ decidedAt: at }, undefined)).toBe(false);
    expect(model.loadRead({ decidedAt: at }, null)).toBe(false);
    expect(model.loadRead({ decidedAt: null }, at)).toBe(false);
    expect(model.loadRead({}, at)).toBe(false);
    expect(model.loadRead({ decidedAt: "soon" }, at)).toBe(false);
  });
});

describe("an approval the current load has yet to apply", () => {
  it("is one whose job hasn't said what became of it", () => {
    const decisions = [
      { id: "a", applied: { status: "pending" as const } },
      { id: "b", applied: { status: "written" as const } },
      { id: "c", applied: { status: "refused" as const, reason: "changed" } },
      { id: "d", applied: { status: "" as const } },
      { id: "e", applied: null },
      { id: "f" },
    ];
    expect(model.waiting(decisions).map((d) => d.id)).toEqual(["a"]);
    expect(model.waiting([])).toEqual([]);
  });
  it("is a chosen value the load has yet to be sent, too", () => {
    const decisions = [
      { id: "a", kind: "prop", applied: { status: "pending" as const } },
      { id: "b", kind: "prop", applied: { status: "applied" as const } },
      { id: "c", kind: "prop", applied: { status: "unconfirmed" as const } },
    ];
    expect(model.waiting(decisions).map((d) => d.id)).toEqual(["a"]);
  });
});

describe("whether a stored value is the one a decision chose", () => {
  it("compares each kind of value as it is stored", () => {
    expect(model.holdsDecided({ integer: "36" }, 36)).toBe(true);
    expect(model.holdsDecided({ number: 1.5 }, 1.5)).toBe(true);
    expect(model.holdsDecided({ text: "GBP" }, "GBP")).toBe(true);
    expect(model.holdsDecided({ boolean: false }, false)).toBe(true);
    expect(model.holdsDecided({ text: "GBP" }, "usd")).toBe(false);
    expect(model.holdsDecided({ integer: "36" }, "36")).toBe(false);
    expect(model.holdsDecided({ integer: "36" }, 24)).toBe(false);
  });
  it("is never so for a value removed, or a property that holds none", () => {
    expect(model.holdsDecided({ null: null }, "usd")).toBe(false);
    expect(model.holdsDecided({ text: "GBP" }, undefined)).toBe(false);
  });
});

describe("what a card says of a value chosen for its property", () => {
  it("says it is on its way, and what it waits behind", () => {
    const sending = "Decision saved. Sending it to this load…";
    expect(model.waitingText(undefined, "prop")).toBe(sending);
    expect(model.waitingText("resend", "prop")).toBe(sending);
    expect(model.decidedText({ status: "pending" }, "36")).toBe(sending);
    expect(model.decidedText({ status: "pending" }, "36", "extract")).toBe(
      "Decision saved. Another job is running, so this waits: it’s applied to this load when that ends.",
    );
    expect(model.decidedText({ status: "pending" }, "36", "load")).toBe(
      "Decision saved. A load is running, so this waits: it’s applied when that ends, to the load that is current then.",
    );
  });
  it("says only what the load knows once it was sent", () => {
    const text = model.decidedText({ status: "applied" }, "36");
    expect(text).toBe(
      "Decided: 36. This load was sent the decision, and holds that value unless a document dated after the decision states another. The documents’ values stay listed here, as in every load: this decision settles them.",
    );
    // A value removed shows as none.
    expect(model.decidedText({ status: "applied" }, "")).toMatch(
      /^Decided: no value\./,
    );
  });
  it("asks for the choice again when its job failed", () => {
    expect(
      model.decidedText({ status: "unconfirmed", reason: "job_failed" }, "36"),
    ).toBe(
      "Decided: 36, but the job that sends it to this load failed. Choose it again to send it.",
    );
  });
  it("says it settles the review where the load holds the value, sent or read when the load was made (contract 4.14.0)", () => {
    for (const status of ["applied", ""] as const)
      expect(model.decidedText({ status }, "36", null, true)).toBe(
        "Decided: 36. This load holds it. The documents’ values stay listed here, as in every load: this decision settles them.",
      );
  });
  it("tells a load that read it when it was made from one that hasn't", () => {
    expect(model.decidedText({ status: "" }, "36")).toBe(
      "Decided: 36. This load read the decision and holds another value: a document dated after the decision states it. Choose again or remove the decision.",
    );
    for (const unread of [undefined, null])
      expect(model.decidedText(unread, "36")).toBe(
        "A decision is saved (36), and this load hasn’t read it. Choose it again to apply it now.",
      );
  });
});

describe("why a load kept an approved claim held", () => {
  it("says what a person can do about each reason the engine gives", () => {
    const said = (reason: string) => model.refusalText(reason);
    expect(said("unresolved")).toContain("isn’t matched to one record");
    expect(said("unresolved")).toContain("Settle that name and load again");
    expect(said("changed")).toContain("reads differently now");
    expect(said("differs")).toContain("read more than one way");
    expect(said("conflict")).toContain("conflicts with what the graph holds");
    // From contract 4.14.0 an approval replaces a one-valued link's value:
    // what's left of a conflict is a property, or an old load's link.
    expect(said("conflict")).toContain("a property of it differs");
    expect(said("conflict")).not.toContain("same date");
    expect(said("blocked")).toContain("break a rule");
    expect(said("no_statement")).toContain("keeps nothing to write");
    expect(said("not_stated")).toContain("no longer states it");
    expect(said("too_large")).toContain("more held claims");
  });
  const end = (name: string, by: model.WeakLink) => ({ name, by });
  it("names the name a legal form linked, when the card knows it", () => {
    expect(
      model.refusalText("legal_form", [
        end("Globex Corporation", "legal_form"),
      ]),
    ).toBe(
      "“Globex Corporation” is linked only by its legal form, which doesn’t write a link that holds one value. Settle that name, and the next load writes this.",
    );
    expect(model.refusalText("legal_form")).toMatch(/^A name in it is linked/);
  });
  it("says a name linked by part of a name, or by an own-name identifier, is settled first", () => {
    expect(model.refusalText("part_name", [end("Westmere", "part_name")])).toBe(
      "“Westmere” is linked only by part of a row’s name, and an approval doesn’t stand on such a link. Settle that name, and the next load writes this.",
    );
    expect(
      model.refusalText("own_name", [end("INC-2031-0042", "own_name")]),
    ).toBe(
      "“INC-2031-0042” is linked only by a code the document calls it by, and an approval doesn’t stand on such a link. Settle that name, and the next load writes this.",
    );
    for (const reason of ["part_name", "own_name"]) {
      expect(model.refusalText(reason)).toMatch(/^A name in it is linked only/);
      // Unlike a legal form's, it holds whatever the link holds.
      expect(model.refusalText(reason)).not.toContain("holds one value");
    }
  });
  it("names only the ends linked the way the engine refused, both where both are", () => {
    const halvorsen = end("Halvorsen Freight Ltd.", "legal_form"),
      westmere = end("Westmere", "part_name");
    // The engine refused for the end the card didn't mark: no name, no
    // wrong one.
    expect(model.refusalText("part_name", [halvorsen])).toMatch(
      /^A name in it is linked only by part of a row’s name/,
    );
    expect(model.refusalText("part_name", [halvorsen, westmere])).toMatch(
      /^“Westmere” is linked only by part/,
    );
    expect(
      model.refusalText("part_name", [westmere, end("Tarnwick", "part_name")]),
    ).toBe(
      "“Westmere” and “Tarnwick” are each linked only by part of a row’s name, and an approval doesn’t stand on such a link. Settle those names, and the next load writes this.",
    );
  });
  it("says where to settle a name the load has no review of the link for", () => {
    for (const reason of ["part_name", "own_name"]) {
      expect(model.refusalText(reason)).toContain(
        "Where this load has no review of such a link, it lists that name as not in your data, or as one that could mean more than one entity: settle it there.",
      );
      expect(
        model.refusalText(reason, [end("Westmere", reason as model.WeakLink)]),
      ).not.toContain("no review of such a link");
    }
    expect(model.refusalText("legal_form")).not.toContain(
      "no review of such a link",
    );
  });
  it("promises the next load only with every name that holds the claim", () => {
    // Refused for one name, while another holds it too: both are settled
    // before a load writes it.
    expect(
      model.refusalText("part_name", [
        end("Westmere", "part_name"),
        end("Halvorsen Freight Ltd.", "legal_form"),
      ]),
    ).toBe(
      "“Westmere” is linked only by part of a row’s name, and an approval doesn’t stand on such a link. Settle that name and “Halvorsen Freight Ltd.”, which holds it too, and the next load writes this.",
    );
    expect(
      model.refusalText("own_name", [
        end("INC-2031-0042", "own_name"),
        end("Westmere", "part_name"),
      ]),
    ).toContain("Settle that name and “Westmere”, which holds it too,");
  });
  it("says what the backend found when the engine was never asked", () => {
    expect(model.refusalText("not_replayed")).toContain(
      "would cost a model call. Load again",
    );
    expect(model.refusalText("document_gone")).toBe(
      "Its document was deleted, or is no longer part of this load.",
    );
    // A document the load holds, whose reading there didn't finish, is not gone.
    expect(model.refusalText("document_unread")).toBe(
      "This load holds its document, but has no finished reading of it to write from. Load again to apply it.",
    );
    expect(model.refusalText("document_unread")).not.toContain("no longer");
    expect(model.refusalText("document_failed")).toContain(
      "couldn’t read its document again",
    );
  });
  it("says what an approval waits behind when another job is running", () => {
    const writing = "Approved. Writing it to this load…";
    expect(model.waitingText()).toBe(writing);
    expect(model.waitingText(null)).toBe(writing);
    expect(model.waitingText("resend")).toBe(writing);
    // A load, an extract or an apply holds the database: the approval's own
    // job follows it.
    expect(model.waitingText("extract")).toBe(
      "Approved. Another job is running, so this waits: it’s written to this load when that ends.",
    );
    expect(model.waitingText("apply")).toBe(model.waitingText("extract"));
    // A new load may become current, and takes the approval with it.
    expect(model.waitingText("load")).toBe(
      "Approved. A load is running, so this waits: it’s written when that ends, to the load that is current then.",
    );
  });
  it("reads the decisions less often the longer an approval has waited", () => {
    expect(model.applyPollMs(0)).toBe(2000);
    expect(model.applyPollMs(29_999)).toBe(2000);
    expect(model.applyPollMs(30_000)).toBe(10_000);
    expect(model.applyPollMs(5 * 60_000 - 1)).toBe(10_000);
    expect(model.applyPollMs(5 * 60_000)).toBe(60_000);
    expect(model.applyPollMs(24 * 60 * 60_000)).toBe(60_000);
    // A clock set back reads as no wait at all.
    expect(model.applyPollMs(-5_000)).toBe(2000);
  });
  it("shows a reason it doesn't know as it came, and says so with none", () => {
    expect(model.refusalText("a_later_reason")).toBe(
      "The load kept it held (a_later_reason).",
    );
    expect(model.refusalText(undefined)).toBe("The load kept it held.");
    expect(model.refusalText("")).toBe("The load kept it held.");
  });
});

describe("stored property review quotes", () => {
  const raw = {
    value: 24,
    doc_id: "doc",
    span: { section: 0, start: 0, end: 9 },
    quote: "24 months",
    confidence: 0.94,
  };
  const quote: model.ReviewValue = {
    value: { integer: "24" },
    documentId: "doc",
    quote: "24 months",
    section: 0,
    start: 0,
    end: 9,
  };
  it("parses engine JSON text and keeps every quote", () => {
    expect(
      model.reviewValues(
        JSON.stringify([
          raw,
          { ...raw, value: false },
          { ...raw, value: "<b>untrusted</b>" },
        ]),
      ),
    ).toEqual([
      quote,
      { ...quote, value: { boolean: false } },
      { ...quote, value: { text: "<b>untrusted</b>" } },
    ]);
  });
  it("reads an integer from its JSON text, past 2^53 too, and only a quote's value so", () => {
    const [big] = model.reviewValues(
      '[{"value":12345678901234567,"doc_id":"doc","span":{"section":0,"start":0,"end":9},"quote":"24 months","confidence":0.94}]',
    );
    expect(big).toEqual({ ...quote, value: { integer: "12345678901234567" } });
    // JSON.parse alone would round it.
    expect(String(JSON.parse("12345678901234567"))).toBe("12345678901234568");
    expect(
      model.reviewValues(JSON.stringify([{ ...raw, value: 1e21 }]))[0]?.value,
    ).toEqual({ number: 1e21 });
  });
  it.each([undefined, [], "{broken", "null", "{}", "false"])(
    "ignores malformed values %s",
    (raw) => {
      expect(model.reviewValues(raw)).toEqual([]);
    },
  );
  it("drops bad records individually", () => {
    const bad = [
      null,
      false,
      {},
      { ...raw, span: null },
      { ...raw, value: [] },
      { ...raw, value: null },
      { ...raw, doc_id: "" },
      { ...raw, doc_id: 4 },
      { ...raw, quote: false },
      { ...raw, span: { section: -1, start: 0, end: 9 } },
      { ...raw, span: { section: 0, start: 2, end: 1 } },
      { ...raw, span: { section: 0.5, start: 0, end: 9 } },
    ];
    expect(model.reviewValues(JSON.stringify([...bad, raw]))).toEqual([quote]);
  });
});

describe("a prop decision's value as text", () => {
  it.each([
    [{ integer: "9007199254740993" }, "9,007,199,254,740,993"],
    [{ integer: "-3" }, "-3"],
    [{ number: 2.5 }, "2.5"],
    [{ number: 1200000 }, "1,200,000"],
    [{ boolean: true }, "Yes"],
    [{ boolean: false }, "No"],
    [{ text: "GBP" }, "GBP"],
    [{ null: null }, ""],
  ] satisfies [FoundryPropValue, string][])("%o reads %s", (v, text) =>
    expect(model.propValueText(v)).toBe(text),
  );
});
