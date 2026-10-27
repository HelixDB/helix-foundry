import { beforeEach, describe, expect, it, vi } from "vitest";
import type { FoundryDocument } from "@/shared/wfe";

// useDocumentNames, rendered by hand: React's hooks keep their state here
// between renders, each effect runs once its render is done, and useQuery
// answers from `cache` by query key, as a read query would.
const react = vi.hoisted(() => ({
  state: [] as unknown[],
  refs: [] as { current: unknown }[],
  effects: [] as (() => void)[],
  next: { state: 0, ref: 0 },
  queries: [] as { queryKey: readonly unknown[]; enabled?: boolean }[],
  cache: new Map<string, unknown>(),
}));
vi.mock("react", async (original) => ({
  ...(await original<typeof import("react")>()),
  useState: (initial: unknown) => {
    const i = react.next.state++;
    if (!(i in react.state)) react.state[i] = initial;
    return [react.state[i], (v: unknown) => (react.state[i] = v)];
  },
  useRef: (initial: unknown) =>
    (react.refs[react.next.ref++] ??= { current: initial }),
  useEffect: (effect: () => void) => react.effects.push(effect),
}));
vi.mock("@tanstack/react-query", async (original) => ({
  ...(await original<typeof import("@tanstack/react-query")>()),
  useQuery: (options: { queryKey: readonly unknown[]; enabled?: boolean }) => {
    react.queries.push(options);
    return {
      data:
        options.enabled === false
          ? undefined
          : react.cache.get(JSON.stringify(options.queryKey)),
    };
  },
}));

// Called by hand, not by a component: named so the hooks lint lets it be.
const { useDocumentNames: documentNames } = await import(
  "@/web/areas/documents/documents-api"
);

/** Renders a page naming `ids`: their names, then its effects run. */
function render(ids: string[]) {
  react.next.state = react.next.ref = 0;
  react.queries = [];
  react.effects = [];
  const name = documentNames("ws_1", "A document");
  const names = ids.map(name);
  react.effects.forEach((effect) => effect());
  return names;
}
const byIds = () =>
  react.queries.find((q) => JSON.stringify(q.queryKey).includes('"ids"'))!;
const doc = (id: string, name: string): FoundryDocument => ({ id, name });

describe("useDocumentNames", () => {
  beforeEach(() => {
    react.state = [];
    react.refs = [];
    react.cache.clear();
  });

  it("names uploads from their list and reads any other by id", () => {
    // Until the uploads are read, nothing is asked for by id.
    expect(render(["d_up", "d_note"])).toEqual(["A document", "A document"]);
    expect(byIds().enabled).toBe(false);
    render(["d_up", "d_note"]);
    expect(byIds().enabled).toBe(false);

    // The uploads list names one; the other, a connector's, is read by id
    // after the render that asked for it.
    react.cache.set(JSON.stringify(["ws_1", "documents"]), [
      doc("d_up", "contract.md"),
    ]);
    expect(render(["d_up", "d_note"])).toEqual(["contract.md", "A document"]);
    expect(byIds().enabled).toBe(false);
    render(["d_up", "d_note"]);
    expect(byIds()).toMatchObject({
      queryKey: ["ws_1", "documents", { ids: ["d_note"] }],
      enabled: true,
    });
    react.cache.set(
      JSON.stringify(["ws_1", "documents", { ids: ["d_note"] }]),
      [doc("d_note", "Weekly pipeline review")],
    );
    expect(render(["d_up", "d_note"])).toEqual([
      "contract.md",
      "Weekly pipeline review",
    ]);
  });

  it("asks for an id once, and keeps what it has read", () => {
    react.cache.set(JSON.stringify(["ws_1", "documents"]), []);
    render(["d_a"]);
    render(["d_a", "d_gone"]);
    // The render the state change makes.
    render(["d_a", "d_gone"]);
    // Each id is added once, in the order asked: one the workspace has no
    // document for stays asked for, and is never asked again.
    expect(byIds().queryKey).toEqual([
      "ws_1",
      "documents",
      { ids: ["d_a", "d_gone"] },
    ]);
    react.cache.set(
      JSON.stringify(["ws_1", "documents", { ids: ["d_a", "d_gone"] }]),
      [doc("d_a", "Notes")],
    );
    expect(render(["d_a", "d_gone"])).toEqual(["Notes", "A document"]);
    expect(render(["d_a", "d_gone"])).toEqual(["Notes", "A document"]);
    expect(byIds().queryKey).toEqual([
      "ws_1",
      "documents",
      { ids: ["d_a", "d_gone"] },
    ]);
    // A page that stops naming one keeps reading it with the rest.
    render(["d_b"]);
    render(["d_b"]);
    expect(byIds().queryKey).toEqual([
      "ws_1",
      "documents",
      { ids: ["d_a", "d_gone", "d_b"] },
    ]);
  });
});
