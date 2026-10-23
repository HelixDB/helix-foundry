import { afterAll, afterEach, beforeAll, expect, it } from "vitest";
import { createServer } from "node:http";
import { once } from "node:events";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { config } from "../apps/api/src/config.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore } from "../apps/api/src/store.js";

// These cover the graph compute routes end to end against the real DuckDB
// executor, through the same HTTP boundary the web app uses.
const originalExecutor = config.executor;
const executor = createServer(async (req, res) => {
  try {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(chunk);
    res.setHeader("content-type", "application/json");
    res.end(
      JSON.stringify(
        await nativeExecute(JSON.parse(Buffer.concat(chunks).toString())),
      ),
    );
  } catch (e) {
    res.statusCode = 422;
    res.end(JSON.stringify({ error: (e as Error).message }));
  }
});
beforeAll(async () => {
  executor.listen(0, "127.0.0.1");
  await once(executor, "listening");
  config.executor = `http://127.0.0.1:${(executor.address() as any).port}`;
});
afterAll(async () => {
  config.executor = originalExecutor;
  executor.closeAllConnections();
  await new Promise<void>((done) => executor.close(() => done()));
});
let store: MemoryStore,
  app: Awaited<ReturnType<typeof createApp>>,
  scope: string,
  base: string;
afterEach(async () => {
  await app?.close();
  for (const kind of ["incoming", "datasets", "results"])
    await rm(resolve(config.dataDir, kind, scope), {
      recursive: true,
      force: true,
    });
});
const request = async (
  method: any,
  path: string,
  payload?: unknown,
  expectStatus = 200,
) => {
  const response = await app.inject({
    method,
    url: base + path,
    payload: payload as any,
  });
  expect(response.statusCode, response.body.slice(0, 400)).toBe(expectStatus);
  return response;
};
// A five-person ontology with self-links, plus an edge dataset for the
// compute routes. Ids overlap on purpose: compute scoring and the published
// graph talk about the same nodes.
async function fixture() {
  store = new MemoryStore();
  app = await createApp(store);
  await app.ready();
  scope = (await app.inject("/api/v1/me")).json().workspaces[0].id;
  base = `/api/v1/workspaces/${scope}`;
  const people = await (
    await request("POST", "/ingest", {
      name: "People",
      rows: [
        { id: "A", name: "Ann", friend: "B" },
        { id: "B", name: "Bob", friend: "C" },
        { id: "C", name: "Carl", friend: "A" },
        { id: "D", name: "Dot", friend: "C" },
        { id: "E", name: "Eve", friend: "C" },
      ],
    })
  ).json();
  const bundle = {
    summary: "People",
    assumptions: [],
    catalog: [],
    pipelines: [],
    ontology: [
      {
        name: "Person",
        datasetId: people.id,
        primaryKey: "id",
        properties: ["id", "name", "friend"],
      },
    ],
    relationships: [
      {
        name: "Knows",
        fromType: "Person",
        toType: "Person",
        fromColumn: "friend",
        toColumn: "id",
      },
      {
        // Real ontologies carry names that are not valid GraphQL identifiers.
        name: "billed to",
        fromType: "Person",
        toType: "Person",
        fromColumn: "friend",
        toColumn: "id",
      },
    ],
    dashboards: [],
    actions: [],
  };
  const proposal = (
    await (
      await request("POST", "/definitions/import", {
        version: 1,
        proposals: [bundle],
      })
    ).json()
  ).items[0];
  expect(proposal.data.validation.valid).toBe(true);
  await request("POST", `/proposals/${proposal.id}/publish`, {
    hash: proposal.data.hash,
  });
  const flows = await (
    await request("POST", "/ingest", {
      name: "Flows",
      rows: [
        { from_id: "A", to_id: "B", amount: 90 },
        { from_id: "B", to_id: "C", amount: 80 },
        { from_id: "C", to_id: "A", amount: 70 },
        { from_id: "D", to_id: "C", amount: 60 },
        { from_id: "D", to_id: "A", amount: 10 },
        { from_id: "D", to_id: "B", amount: 10 },
        { from_id: "E", to_id: "A", amount: 10 },
        { from_id: "E", to_id: "B", amount: 10 },
      ],
    })
  ).json();
  return { people, flows };
}
const projection = (edgeDatasetId: string) => ({
  name: "test",
  edgeDatasetId,
  srcColumn: "from_id",
  dstColumn: "to_id",
  edgeTypes: [],
});

it("lists every algorithm and fraud recipe the executor can run", async () => {
  await fixture();
  const schema = await (await request("GET", "/graph/schema")).json();
  expect(schema.algorithms.map((a: any) => a.kind)).toEqual([
    "degree",
    "wcc",
    "pagerank",
    "louvain",
    "betweenness",
    "node_similarity",
    "knn",
    "shortest_path",
    "all_paths",
    "node_embeddings",
    "link_prediction",
    "vector_search",
  ]);
  expect(schema.fraudRecipes).toHaveLength(5);
});

it("persists algorithm scores as a dataset and reuses the dataset on a re-run", async () => {
  const { flows } = await fixture();
  const run = (await (
    await request("POST", "/graph/algorithms", {
      kind: "degree",
      name: "Degrees",
      projection: projection(flows.id),
      persist: true,
    })
  ).json()) as any;
  expect(run.datasetId).toBeTruthy();
  expect(run.durationMs).toBeGreaterThanOrEqual(0);
  expect(Object.keys(run.rows[0])).toContain("node_id");
  const ds = await (
    await request("GET", `/resources/dataset/${run.datasetId}`)
  ).json();
  expect(ds.data.activeVersion).toBeTruthy();
  expect(ds.name).toBe("Degrees");  // A second run under the same name adds a snapshot instead of a duplicate.
  const again = await (
    await request("POST", "/graph/algorithms", {
      kind: "degree",
      name: "Degrees",
      projection: projection(flows.id),
      persist: true,
    })
  ).json();
  expect(again.datasetId).toBe(run.datasetId);
  const after = await (
    await request("GET", `/resources/dataset/${run.datasetId}`)
  ).json();
  expect(after.data.versions).toHaveLength(2);
  // Without persist there is no dataset, only the score table.
  const ephemeral = await (
    await request("POST", "/graph/algorithms", {
      kind: "degree",
      name: "Degrees",
      projection: projection(flows.id),
      persist: false,
    })
  ).json();
  expect(ephemeral.datasetId).toBeUndefined();
});

it("persists fraud findings as a dataset", async () => {
  const { flows } = await fixture();
  const scan = await (
    await request("POST", "/graph/fraud/scan", {
      recipe: "structuring",
      name: "Micro transfers",
      projection: projection(flows.id),
      params: { amountColumn: "amount", topN: 20 },
      persist: true,
    })
  ).json();
  expect(scan.datasetId).toBeTruthy();
  expect(Array.isArray(scan.findings)).toBe(true);
});

it("computes vector search on its own, without a prior embeddings run", async () => {
  const { flows } = await fixture();
  const search = await (
    await request("POST", "/graph/algorithms", {
      kind: "vector_search",
      name: "Neighbours of A",
      projection: projection(flows.id),
      params: { sourceId: "A", topK: 5 },
      persist: false,
    })
  ).json();
  expect(search.rows.length).toBeGreaterThan(0);
  expect(search.columns).toContain("neighbour_id");
});

it("exports the published graph with the right file type, name and body", async () => {
  await fixture();
  const get = async (format: string) =>
    app.inject({ method: "GET", url: `${base}/graph/export/${format}` });
  const graphml = await get("graphml");
  expect(graphml.statusCode).toBe(200);
  expect(graphml.headers["content-type"]).toContain("application/xml");
  expect(graphml.headers["content-disposition"]).toContain("graph.graphml");
  expect(graphml.body).toContain("<graphml");
  // Five people came from the published ontology; the relationship's
  // name rides along as edge data.
  expect(graphml.body.split("<node ")).toHaveLength(6);
  expect(graphml.body).toContain("Knows");
  const jsonld = await get("jsonld");
  expect(jsonld.headers["content-type"]).toContain("application/ld+json");
  expect(JSON.parse(jsonld.body)["@graph"].length).toBeGreaterThan(0);
  const dot = await get("dot");
  expect(dot.headers["content-type"]).toContain("graphviz");
  expect(dot.body).toContain("digraph");
  const unknown = await get("csv");
  expect(unknown.statusCode).toBe(400);
});

it("introspects, counts stats and answers adjacency for a node", async () => {
  const { people } = await fixture();
  const intro = await (await request("GET", "/graph/introspect")).json();
  expect(JSON.stringify(intro)).toContain("Person");
  const stats = await (await request("GET", "/graph/stats")).json();
  expect(stats.nodeCount).toBeGreaterThan(0);
  expect(stats.edgeCount).toBeGreaterThan(0);
  // Node ids in the compute routes are logical ids, as the explorer uses them.
  const graph = await (await request("GET", "/graph")).json();
  const nodeId = graph.nodes[0].id as string;
  const adj = await (await request("GET", `/graph/adjacency/${nodeId}`)).json();
  expect(adj.nodeId).toBe(nodeId);
  expect(adj.degree).toBeGreaterThan(0);
  void people;
});

it("merges two records and marks the secondary as merged", async () => {
  await fixture();
  const objects = await (
    await request("GET", `/resources/object?limit=10`)
  ).json();
  const ordered = objects.items.sort((a: any, b: any) =>
    a.id.localeCompare(b.id),
  );
  const [primary, secondary] = ordered;
  const preview = await (
    await request("POST", "/actions/change/preview", {
      operation: "merge",
      primaryId: primary.id,
      secondaryId: secondary.id,
    })
  ).json();
  expect(preview.hash).toBeTruthy();
  const audit = await (
    await request("POST", "/actions/change/execute", {
      action: { operation: "merge", primaryId: primary.id, secondaryId: secondary.id },
      hash: preview.hash,
      idempotencyKey: "merge-test-0001",
    })
  ).json();
  expect(audit.data.evidence.after.merged).toBe(true);
  const after = await store.get(scope, secondary.id);
  expect(after?.data.merged).toBe(true);
  expect(after?.data.mergedInto).toBe(primary.id);
});

it("finds a shortest path between two node ids, or none", async () => {
  const { flows } = await fixture();
  const found = (
    await (
      await request("POST", "/graph/paths", {
        projection: projection(flows.id),
        sourceId: "D",
        targetId: "A",
        maxDepth: 6,
      })
    ).json()
  ) as any;
  // The edge list is walked undirected, so D and A are adjacent: one hop.
  expect(found.nodeIds[0]).toBe("D");
  expect(found.nodeIds.at(-1)).toBe("A");
  expect(found.length).toBe(1);
  // An unknown node id finds no path at all.
  const none = (
    await (
      await request("POST", "/graph/paths", {
        projection: projection(flows.id),
        sourceId: "Z",
        targetId: "A",
        maxDepth: 4,
      })
    ).json()
  ) as any;
  expect(none).toBeNull();
});

it("answers GraphQL over the published ontology, with relation links", async () => {
  await fixture();
  const r = (
    await (
      await request("POST", "/graphql", {
        query: '{ Person(key: "A") { name friend Knows } }',
      })
    ).json()
  ) as any;
  expect(r.errors).toBeUndefined();
  expect(r.data.Person.name).toBe("Ann");
  expect(r.data.Person.Knows).toEqual(["B"]);
  expect(r.data.Person.friend).toBe("B");
  const list = (
    await (
      await request("POST", "/graphql", {
        query: "{ PersonList(limit: 2) { _key } }",
      })
    ).json()
  ) as any;
  expect(list.data.PersonList).toHaveLength(2);
  // A relationship named "billed to" is exposed under its sanitized field.
  const bad = (
    await (
      await request("POST", "/graphql", {
        query: '{ Person(key: "A") { billed_to } }',
      })
    ).json()
  ) as any;
  expect(bad.errors).toBeUndefined();
  expect(bad.data.Person.billed_to).toEqual(["B"]);
});
