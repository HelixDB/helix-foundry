import { describe, expect, it } from "vitest";
import * as toml from "smol-toml";
import { readAgentBundle } from "@/web/areas/ontology/model";
import { validateEdit } from "@/web/areas/ontology/edit-model";
import { suggestion } from "@/web/areas/suggestions/model";
import {
  addLinkEdits,
  cardState,
  stateText,
  whyText,
} from "@/web/areas/suggestions/add-link";

// A finding's Add this link, in place: the edits that add the link and route
// it for the set, and the card's state from the backend's word on the move.

const ONTOLOGY = `version = "v1"
[types.Person]
description = "A person."
[types.Company]
description = "A business."
[links.works_at]
from = "Person"
to = "Company"
description = "Works at."
[links.ceo_of]
from = "Person"
to = "Company"
description = "Runs it."
`;

const route = (name: string, documents: string, links: string[]) =>
  `[extractions.${name}]\ndocuments = ${documents}\nlinks = ${JSON.stringify(links)}\n[extractions.${name}.types.Person]\n[extractions.${name}.types.Company]\n`;

const bundle = (ontology: string, ...routes: string[]) =>
  readAgentBundle({
    ontology_toml: ontology,
    mappings_toml: `version = "m1"\nontology = "v1"\n${routes.join("")}`,
  });

const ask = {
  link: "chair_of",
  setId: "set_1",
  from: "Person",
  to: "Company",
  description: " Chairs its board. ",
};

describe("addLinkEdits", () => {
  it("adds the link and routes it for the set, by the route that names the set", () => {
    const b = bundle(
      ONTOLOGY,
      route("all_docs", '"all"', ["works_at"]),
      route("notes", '["set_1"]', ["works_at"]),
    );
    const got = addLinkEdits(b, ask);
    expect(got.kind).toBe("edits");
    if (got.kind !== "edits") return;
    const [link, routed] = got.edits;
    expect(link.put?.itemId).toBe("link.chair_of");
    expect(toml.parse(link.put!.toml)).toEqual({
      links: {
        chair_of: {
          from: "Person",
          to: "Company",
          description: "Chairs its board.",
        },
      },
    });
    expect(routed.put?.itemId).toBe("extraction.notes");
    const table = (toml.parse(routed.put!.toml) as any).extractions.notes;
    expect(table.links).toEqual(["works_at", "chair_of"]);
    expect(table.documents).toEqual(["set_1"]);
    expect(Object.keys(table.types)).toEqual(["Person", "Company"]);
  });

  it("makes edits Revise takes: a new link beside the route that reads it", () => {
    const b = bundle(ONTOLOGY, route("notes", '["set_1"]', ["works_at"]));
    const got = addLinkEdits(b, ask);
    if (got.kind !== "edits") throw new Error(got.why);
    expect(
      got.edits.map((e) => validateEdit(b, e, got.edits)).filter(Boolean),
    ).toEqual([]);
    // The link alone, with no route reading it, is refused, as Revise does.
    expect(validateEdit(b, got.edits[0], [got.edits[0]])).toMatch(
      /EditNotAllowed: a new link must be/,
    );
  });

  it("routes a link the version declares without putting it again", () => {
    const b = bundle(ONTOLOGY, route("notes", '"all"', ["works_at"]));
    const got = addLinkEdits(b, { ...ask, link: "ceo_of" });
    expect(got.kind === "edits" && got.edits.map((e) => e.put?.itemId)).toEqual(
      ["extraction.notes"],
    );
  });

  it("refuses what it can't add", () => {
    const routed = bundle(ONTOLOGY, route("notes", '"all"', ["ceo_of"]));
    const refused = (b: ReturnType<typeof bundle>, a = ask) => {
      const got = addLinkEdits(b, a);
      return got.kind === "refused" ? got.why : "";
    };
    expect(refused(routed, { ...ask, link: "ceo_of" })).toMatch(
      /already state/,
    );
    expect(refused(bundle(ONTOLOGY, route("other", '["set_2"]', [])))).toMatch(
      /No route reads/,
    );
    expect(refused(routed, { ...ask, from: "Deal" })).toMatch(/types/);
    expect(refused(routed, { ...ask, description: "  " })).toMatch(/Describe/);
    expect(refused(readAgentBundle({ ontology_toml: "[x" }))).toMatch(
      /can't be read/,
    );
  });
});

describe("cardState", () => {
  it("reads each state, and none for no state", () => {
    expect(cardState(undefined)).toBeUndefined();
    expect(cardState({ open: {} })).toEqual({ kind: "open" });
    expect(cardState({})).toEqual({ kind: "open" });
    expect(cardState({ covered: { versionId: "v8", version: "8" } })).toEqual({
      kind: "covered",
      version: 8,
      versionId: "v8",
    });
    expect(cardState({ moving: { versionId: "v8", version: "8" } })).toEqual({
      kind: "moving",
      version: 8,
    });
    expect(
      cardState({ moved: { version: "8", linksAdded: ["ceo_of"] } }),
    ).toEqual({ kind: "moved", version: 8, links: ["ceo_of"] });
    expect(
      cardState({
        needsLoad: {
          versionId: "v9",
          version: "9",
          why: [{ kind: "type_added", item: "types.Deal" }],
        },
      }),
    ).toEqual({
      kind: "needs_load",
      version: 9,
      versionId: "v9",
      why: ["it adds types.Deal"],
    });
    // A version that isn't a number reads as none.
    expect(cardState({ moving: { version: "x" } })).toEqual({
      kind: "moving",
      version: 0,
    });
  });

  it("says each state in a line, and why a load is needed in words", () => {
    expect(stateText({ kind: "open" })).toBeUndefined();
    expect(stateText({ kind: "covered", version: 8, versionId: "v8" })).toBe(
      "Version 8 adds this link: make it current to update the current load.",
    );
    expect(stateText({ kind: "moving", version: 0 })).toBe(
      "Updating the current load to the new version…",
    );
    expect(stateText({ kind: "moved", version: 8, links: [] })).toMatch(
      /^The current load is on version 8 now: documents read from now on state this link\./,
    );
    expect(
      stateText({ kind: "needs_load", version: 9, versionId: "v9", why: [] }),
    ).toBe("Needs a new load: version 9 changes more than a link.");
    expect(
      stateText({
        kind: "needs_load",
        version: 3,
        versionId: "v3",
        why: ["it adds types.Deal", "it changes extractions.notes.types.Deal"],
      }),
    ).toBe(
      "Needs a new load: version 3 changes more than a link: it adds types.Deal; it changes extractions.notes.types.Deal.",
    );
    expect(whyText({ kind: "sources", item: "table_files" })).toBe(
      "it reads other table files",
    );
    expect(whyText({ kind: "marker_elsewhere", item: "foundry-v3" })).toBe(
      "the load holds foundry-v3",
    );
    expect(whyText({ kind: "load_moved", item: "4" })).toBe(
      "the load moved to version 4 for another suggestion",
    );
    expect(whyText({ kind: "move_failed", item: "agent down" })).toBe(
      "moving the load failed",
    );
    expect(whyText({ kind: "link_referenced", item: "links.ceo_of" })).toBe(
      "rules or other links read links.ceo_of",
    );
    expect(whyText({ kind: "a_later_kind", item: "x.y" })).toBe(
      "a later kind x.y",
    );
    expect(whyText({})).toBe("");
  });
});

describe("a fill of the documents behind a finding", () => {
  const fill = {
    versionId: "v8",
    version: "8",
    linksAdded: ["mentors"],
    docsOwed: "2",
    docsDone: "1",
    docsLeft: "0",
    reviewsResolved: "3",
    factsAdded: "2",
    spentUsd: 0.0125,
    capUsd: 5,
  };
  const counted = {
    owed: 2,
    done: 1,
    left: 0,
    resolved: 3,
    facts: 2,
    spent: 0.0125,
    cap: 5,
    pausedUntil: undefined,
    stopReason: "",
  };

  it("reads each of its states, counts read without trust", () => {
    expect(cardState({ applying: fill })).toEqual({
      kind: "applying",
      version: 8,
      ...counted,
    });
    expect(cardState({ fixed: fill })).toEqual({
      kind: "fixed",
      version: 8,
      ...counted,
    });
    expect(
      cardState({ stopped: { ...fill, stopReason: "cap_reached" } }),
    ).toEqual({
      kind: "stopped",
      version: 8,
      ...counted,
      stopReason: "cap_reached",
    });
    const broken = cardState({
      applying: {
        docsOwed: "-3",
        docsDone: "x",
        spentUsd: -1,
        pausedUntil: "soon",
      },
    });
    expect(broken).toMatchObject({
      owed: 0,
      done: 0,
      spent: 0,
      pausedUntil: undefined,
    });
  });

  it("says how far it got, what it spent and what it did", () => {
    expect(stateText({ kind: "applying", version: 8, ...counted })).toBe(
      "Filling the current load (version 8) with this link: 1 of 2 documents, $0.01 of $5.00.",
    );
    // A $0 local model has no spend to say.
    expect(
      stateText({ kind: "applying", version: 8, ...counted, cap: 0 }),
    ).toBe(
      "Filling the current load (version 8) with this link: 1 of 2 documents.",
    );
    expect(
      stateText({
        kind: "applying",
        version: 8,
        ...counted,
        pausedUntil: new Date("2026-11-24T14:00:00Z"),
      }),
    ).toMatch(/ Paused until 14:00 UTC\.$/);
    expect(
      stateText({
        kind: "fixed",
        version: 8,
        ...counted,
        resolved: 1,
        facts: 1,
      }),
    ).toBe("1 review resolved, 1 fact added in the current load (version 8).");
    expect(stateText({ kind: "fixed", version: 8, ...counted, left: 3 })).toBe(
      "3 reviews resolved, 2 facts added in the current load (version 8). 3 more documents state it: a new load asks them.",
    );
    expect(
      stateText({
        kind: "stopped",
        version: 8,
        ...counted,
        stopReason: "dismissed",
      }),
    ).toBe(
      "Filling stopped after 1 of 2 documents: the suggestion was dismissed. 3 reviews resolved, 2 facts added.",
    );
    expect(
      stateText({
        kind: "stopped",
        version: 8,
        ...counted,
        stopReason: "a_later_reason",
      }),
    ).toMatch(/: a later reason\./);
  });
});

describe("a suggestion's Add this link in place", () => {
  const finding = (state?: object, proposal: object | null = null) => ({
    fingerprint: "f".repeat(64),
    kind: "unmatched",
    status: "open",
    support: "3",
    sources: "3",
    findingJson: JSON.stringify({
      signal: {
        kind: "unmatched",
        canonical: "mentors",
        set: "set_1",
        backing: { count: 3, sources: 3, examples: [] },
      },
      proposal,
    }),
    ...(state && { state }),
  });

  it("is offered only where the backend says its state", () => {
    expect(suggestion(finding()).addLink).toBeUndefined();
    expect(suggestion(finding({ open: {} })).addLink).toEqual({
      link: "mentors",
      setId: "set_1",
      describe: "Mentors, as documents state it.",
    });
    // Gathered under a link the version has: Generate drafts it.
    const route = { proposal: "route", set: "set_1", links: ["works_at"] };
    expect(suggestion(finding({ open: {} }, route)).addLink).toBeUndefined();
  });
});
