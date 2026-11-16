import type { FoundryAgentThread } from "../../../shared/wfe";

// Founder's page (/ask/:ontologyId, founder-page.tsx) and its threads in the
// sidebar (founder-threads.tsx), as pure functions: what the page shows from
// its URL, and Founder's threads as the sidebar lists them.

/** What Founder's page shows. */
export type FounderView =
  // One of Founder's threads.
  | { kind: "chat"; id: string }
  // A new chat: the composer, centred.
  | { kind: "new" }
  // An Analyst thread from before Founder, read only.
  | { kind: "analyst"; id: string }
  // A narrow screen's list of threads, where a wide one has the sidebar.
  | { kind: "threads" };

/**
 * What Founder's page shows, from its URL, the screen, Founder's threads
 * (undefined while they're read or when they couldn't be) and the thread the
 * dock last held (`stored`, "" for none): `?thread=` is an Analyst thread,
 * `?chat=` one of Founder's; `?new`, or no threads at all, a new chat; else,
 * on a narrow screen, the threads; else the dock's thread, while the list
 * names it (or isn't read yet), so the page and the dock hold one
 * conversation; else a new chat.
 *
 * @example
 * const p = (q: string) => new URLSearchParams(q);
 * founderView(p("thread=q1"), false, [], ""); // { kind: "analyst", id: "q1" }
 * founderView(p("chat=t1"), true, undefined, ""); // { kind: "chat", id: "t1" }
 * founderView(p("new=1"), true, [{ id: "t1" }], "t1"); // { kind: "new" }
 * founderView(p(""), true, [{ id: "t1" }], "t1"); // { kind: "threads" }
 * founderView(p(""), false, [{ id: "t1" }], "t1"); // { kind: "chat", id: "t1" }
 * founderView(p(""), false, [{ id: "t1" }], "t9"); // { kind: "new" }
 * founderView(p(""), false, [], "t1"); // { kind: "new" }
 */
export function founderView(
  params: URLSearchParams,
  narrow: boolean,
  threads: readonly { id: string }[] | undefined,
  stored: string,
): FounderView {
  const analyst = params.get("thread");
  if (analyst) return { kind: "analyst", id: analyst };
  const chat = params.get("chat");
  if (chat) return { kind: "chat", id: chat };
  if (params.has("new") || threads?.length === 0) return { kind: "new" };
  if (narrow) return { kind: "threads" };
  return stored && (!threads || threads.some((t) => t.id === stored))
    ? { kind: "chat", id: stored }
    : { kind: "new" };
}

/** One of Founder's threads as the sidebar lists it. */
export type FounderThread = {
  id: string;
  title: string;
  // When it last changed (threadGroups groups by it).
  last: string;
};

/**
 * Founder's threads as the sidebar lists them, in the list's order (newest
 * first): untitled ones named so, and dated by their last change, else by
 * their start.
 *
 * @example
 * founderThreads([
 *   { id: "t1", title: "Unpaid invoices", updatedAt: "2026-11-16T09:00:00Z" },
 *   { id: "t2", createdAt: "2026-11-15T09:00:00Z" },
 * ]);
 * // [{ id: "t1", title: "Unpaid invoices", last: "2026-11-16T09:00:00Z" },
 * //  { id: "t2", title: "Untitled", last: "2026-11-15T09:00:00Z" }]
 * founderThreads(undefined); // []
 */
export function founderThreads(
  threads: readonly FoundryAgentThread[] | undefined,
): FounderThread[] {
  return (threads || []).map((t) => ({
    id: t.id,
    title: t.title || "Untitled",
    last: t.updatedAt || t.createdAt || "",
  }));
}
