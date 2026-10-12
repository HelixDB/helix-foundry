import { FoundryClient } from "@helix-foundry/sdk";

// A real typed consumer of the existing public source creation API. This
// function is never invoked by the compiler-only gate.
export async function existingSourceConsumer(client: FoundryClient) {
  await client.createSource({
    name: "Neo4j source",
    kind: "neo4j",
    config: { url: "http://127.0.0.1:7474", database: "neo4j", user: "synthetic", password: "synthetic-only" },
  });
  await client.createSource({
    name: "Automatic REST source",
    kind: "rest",
    config: { url: "http://127.0.0.1:39001/fixture" },
    schedule: null,
  });
  await client.createSource({ name: "Scheduled REST source", kind: "rest", config: { url: "http://127.0.0.1:39001/fixture" }, schedule: "0 * * * *" });
  // Files are uploaded through the separate upload endpoint, not a connected source.
  // @ts-expect-error manual uploads are not source connections
  await client.createSource({ name: "Wrong route", kind: "upload", config: {} });
  // @ts-expect-error the server has no such connector
  await client.createSource({ name: "Unsupported source", kind: "unsupported", config: {} });
  // @ts-expect-error schedule accepts cron text or null, not a number
  await client.createSource({ name: "Invalid cadence", kind: "rest", config: {}, schedule: 123 });
}
