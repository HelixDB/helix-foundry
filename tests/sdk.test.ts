import { it, expect, vi, afterEach } from "vitest";
import { FoundryClient } from "../packages/sdk/src/index.js";
const client = () =>
  new FoundryClient({
    baseUrl: "https://foundry.invalid",
    workspaceId: "w",
    token: "fixture",
  });
afterEach(() => vi.unstubAllGlobals());
it("omits unset list options while preserving zero and empty search", async () => {
  const f = vi.fn().mockResolvedValue(new Response("{}"));
  vi.stubGlobal("fetch", f);
  await client().list("dataset", { offset: 0, limit: undefined, search: "" });
  const u = new URL(f.mock.calls[0][0]);
  expect(u.searchParams.has("limit")).toBe(false);
  expect(u.searchParams.get("offset")).toBe("0");
  expect(u.searchParams.get("search")).toBe("");
});
// Source inputs follow the API's SourceSchema: every connector, including
// Neo4j, and null for the Automatic cadence. Files go through uploads.
it("sends every connector the source routes accept", async () => {
  const f = vi.fn(async (..._: any[]) => new Response("{}"));
  vi.stubGlobal("fetch", f);
  const neo4j = {
    name: "Graph",
    kind: "neo4j" as const,
    config: { url: "http://localhost:7474" },
    schedule: null,
  };
  await client().testSource(neo4j);
  await client().createSource(neo4j);
  await client().correctSource("s1", neo4j);
  expect(
    f.mock.calls.map(([url, init]) => [
      init.method,
      new URL(url).pathname,
      JSON.parse(init.body),
    ]),
  ).toEqual([
    ["POST", "/api/v1/workspaces/w/sources/test", neo4j],
    ["POST", "/api/v1/workspaces/w/sources", neo4j],
    ["PUT", "/api/v1/workspaces/w/onboarding/sources/s1", neo4j],
  ]);
  // @ts-expect-error files are uploaded, not connected
  await client().createSource({ name: "File", kind: "upload", config: {} });
});
it("stages a SQLite file for a sqlite source", async () => {
  const f = vi.fn(
    async (..._: any[]) =>
      new Response(JSON.stringify({ file: "a".repeat(64) + ".sqlite" })),
  );
  vi.stubGlobal("fetch", f);
  const staged = await client().uploadSqlite(new Blob(["SQLite"]), "claims.db");
  expect(staged.file).toMatch(/\.sqlite$/);
  const [url, init] = f.mock.calls[0];
  expect(new URL(url).pathname).toBe("/api/v1/workspaces/w/sources/sqlite");
  expect(init.headers).toEqual({ authorization: "Bearer fixture" });
  expect((init.body as FormData).get("file")).toBeInstanceOf(Blob);
  await client().testSource({
    name: "claims",
    kind: "sqlite",
    config: { file: staged.file },
  });
});
