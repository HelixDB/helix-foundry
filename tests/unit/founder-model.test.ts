import { describe, expect, it } from "vitest";
import { threadGroups } from "@/web/areas/ontology/ask-model";
import {
  founderThreads,
  founderView,
} from "@/web/areas/ontology/founder-model";

// Founder's page (src/web/areas/ontology/founder-model.ts): what /ask/:o
// shows from its URL, the screen, Founder's threads and the dock's thread,
// and Founder's threads as the sidebar lists them.

const p = (q: string) => new URLSearchParams(q);
const listed = [{ id: "t1" }, { id: "t2" }];

describe("what Founder's page shows", () => {
  it("does as its examples say", () => {
    expect(founderView(p("thread=q1"), false, [], "")).toEqual({
      kind: "analyst",
      id: "q1",
    });
    expect(founderView(p("chat=t1"), true, undefined, "")).toEqual({
      kind: "chat",
      id: "t1",
    });
    expect(founderView(p("new=1"), true, [{ id: "t1" }], "t1")).toEqual({
      kind: "new",
    });
    expect(founderView(p(""), true, [{ id: "t1" }], "t1")).toEqual({
      kind: "threads",
    });
    expect(founderView(p(""), false, [{ id: "t1" }], "t1")).toEqual({
      kind: "chat",
      id: "t1",
    });
    expect(founderView(p(""), false, [{ id: "t1" }], "t9")).toEqual({
      kind: "new",
    });
    expect(founderView(p(""), false, [], "t1")).toEqual({ kind: "new" });
  });

  it("opens an Analyst thread read only before anything else the URL says", () => {
    expect(
      founderView(p("thread=q1&chat=t1&new=1"), true, listed, "t2"),
    ).toEqual({ kind: "analyst", id: "q1" });
  });

  it("opens the chat the URL names over a new one and the dock's", () => {
    expect(founderView(p("chat=t9&new=1"), false, listed, "t1")).toEqual({
      kind: "chat",
      id: "t9",
    });
    // Even one the list doesn't name: its own read says if it is gone.
    expect(founderView(p("chat=t9"), false, [], "")).toEqual({
      kind: "chat",
      id: "t9",
    });
  });

  it("starts a new chat where there are no threads, on any screen", () => {
    expect(founderView(p(""), true, [], "")).toEqual({ kind: "new" });
    expect(founderView(p(""), false, [], "")).toEqual({ kind: "new" });
  });

  it("starts a new chat for ?new, with or without a value, and an about", () => {
    expect(founderView(p("new"), false, listed, "t1")).toEqual({
      kind: "new",
    });
    expect(founderView(p("new=1&about=Globex"), true, listed, "")).toEqual({
      kind: "new",
    });
  });

  it("lists the threads on a narrow screen until one is chosen, also while they're read", () => {
    expect(founderView(p(""), true, listed, "")).toEqual({ kind: "threads" });
    expect(founderView(p(""), true, undefined, "t1")).toEqual({
      kind: "threads",
    });
    // An entity link's ?focus= chooses nothing.
    expect(founderView(p("focus=Customer%3Acus_1"), true, listed, "")).toEqual({
      kind: "threads",
    });
  });

  it("opens the dock's thread on a wide screen while the list names it or is still read", () => {
    expect(founderView(p(""), false, listed, "t2")).toEqual({
      kind: "chat",
      id: "t2",
    });
    expect(founderView(p(""), false, undefined, "t2")).toEqual({
      kind: "chat",
      id: "t2",
    });
  });

  it("starts a new chat on a wide screen without a thread of the dock's", () => {
    expect(founderView(p(""), false, listed, "")).toEqual({ kind: "new" });
    expect(founderView(p(""), false, undefined, "")).toEqual({ kind: "new" });
    expect(founderView(p(""), false, listed, "gone")).toEqual({ kind: "new" });
  });

  it("ignores empty parameters", () => {
    expect(founderView(p("thread=&chat="), false, listed, "t1")).toEqual({
      kind: "chat",
      id: "t1",
    });
  });
});

describe("Founder's threads in the sidebar", () => {
  it("does as its example says", () => {
    expect(
      founderThreads([
        {
          id: "t1",
          title: "Unpaid invoices",
          updatedAt: "2026-11-16T09:00:00Z",
        },
        {
          id: "t2",
          createdAt: "2026-11-15T09:00:00Z",
          surface: "FOUNDRY_AGENT_SURFACE_SLACK",
        },
      ]),
    ).toEqual([
      {
        id: "t1",
        title: "Unpaid invoices",
        last: "2026-11-16T09:00:00Z",
        slack: false,
      },
      {
        id: "t2",
        title: "Untitled",
        last: "2026-11-15T09:00:00Z",
        slack: true,
      },
    ]);
    expect(founderThreads(undefined)).toEqual([]);
  });

  it("dates a thread by its last change over its start, and leaves one without either undated", () => {
    expect(
      founderThreads([
        {
          id: "t1",
          title: "",
          createdAt: "2026-11-01T09:00:00Z",
          updatedAt: "2026-11-16T09:00:00Z",
        },
        {
          id: "t2",
          createdAt: null,
          updatedAt: null,
          surface: "FOUNDRY_AGENT_SURFACE_UNSPECIFIED",
        },
      ]),
    ).toEqual([
      {
        id: "t1",
        title: "Untitled",
        last: "2026-11-16T09:00:00Z",
        slack: false,
      },
      { id: "t2", title: "Untitled", last: "", slack: false },
    ]);
  });

  it("groups by recency as the Analyst's threads are, undated ones last", () => {
    const now = new Date(2026, 10, 16, 15);
    const threads = founderThreads([
      { id: "a", updatedAt: new Date(2026, 10, 16, 9).toISOString() },
      { id: "b", updatedAt: new Date(2026, 10, 10, 9).toISOString() },
      { id: "c", updatedAt: new Date(2026, 9, 1, 9).toISOString() },
      { id: "d" },
    ]);
    const groups = threadGroups(threads, now).map((g) => [
      g.label,
      g.threads.map((t) => t.id),
    ]);
    expect(groups).toEqual([
      ["Today", ["a"]],
      ["Previous 7 days", ["b"]],
      ["Older", ["c", "d"]],
    ]);
  });
});
