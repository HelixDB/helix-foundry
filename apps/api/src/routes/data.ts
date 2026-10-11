import type { FastifyInstance } from "fastify";
import { z } from "zod";
import { randomUUID } from "node:crypto";
import { createReadStream, createWriteStream } from "node:fs";
import { mkdir } from "node:fs/promises";
import { resolve } from "node:path";
import { pipeline } from "node:stream/promises";
import { CronExpressionParser } from "cron-parser";
import {
  SourceSchema,
  DemoSchema,
  datasetTable,
} from "../../../../packages/shared/src/index.js";
import { config } from "../config.js";
import { assert, seal } from "../security.js";
import { resource, transaction, type Store } from "../store.js";
import { discover } from "../connectors.js";
import {
  ingestFile,
  ingestRows,
  appendRows,
  queryDatasets,
  seedDemo,
} from "../datasets.js";
import { sourceSyncDefaults } from "../source-sync.js";
import { testedConnection } from "../onboarding.js";
import { resolveHostedConfig } from "../hosting.js";
import type { RequestSchemas, RouteDeps } from "./deps.js";

export const dataSchemas: RequestSchemas = {
  "POST /api/v1/workspaces/:w/demo": DemoSchema,
  "POST /api/v1/workspaces/:w/sources": SourceSchema,
  "POST /api/v1/workspaces/:w/sources/test": SourceSchema,
  "POST /api/v1/workspaces/:w/datasets/:id/append": z.object({
    expectedVersion: z.string().min(1),
    rows: z.array(z.record(z.string(), z.unknown())).min(1).max(10000),
  }),
  "POST /api/v1/workspaces/:w/ingest": z.object({
    name: z.string(),
    rows: z.array(z.record(z.string(), z.unknown())).min(1).max(10000),
    datasetId: z.string().optional(),
  }),
  "POST /api/v1/workspaces/:w/query": z.object({
    sql: z.string(),
    inputs: z.array(z.string()).min(1),
  }),
};

// Data Connection: sample data, ingestion, uploads, datasets, sources and syncs,
// SQL queries and data-health findings.
export function registerDataRoutes(
  app: FastifyInstance,
  store: Store,
  deps: RouteDeps,
) {
  const { access, scrub } = deps;
  app.post("/api/v1/workspaces/:w/demo", async (req) => {
    const a = await access(req, "editor"),
      body = DemoSchema.parse(req.body ?? {});
    return { datasets: await seedDemo(store, a.scope, body) };
  });
  app.post("/api/v1/workspaces/:w/ingest", async (req) => {
    const a = await access(req, "editor"),
      b = z
        .object({
          name: z.string().min(1),
          rows: z.array(z.record(z.string(), z.unknown())).min(1).max(10000),
          datasetId: z.string().optional(),
        })
        .parse(req.body);
    if (b.datasetId)
      assert(
        (await store.get(a.scope, b.datasetId))?.kind === "dataset",
        "Dataset unavailable",
        404,
      );
    return ingestRows(store, a.scope, b.name, b.rows, undefined, b.datasetId);
  });
  app.post("/api/v1/workspaces/:w/datasets/:id/append", async (req: any) => {
    const a = await access(req, "editor"),
      b = z
        .object({
          expectedVersion: z.string().min(1),
          rows: z.array(z.record(z.string(), z.unknown())).min(1).max(10000),
        })
        .parse(req.body);
    return appendRows(store, a.scope, req.params.id, b.expectedVersion, b.rows);
  });
  app.post("/api/v1/workspaces/:w/upload", async (req, reply) => {
    const a = await access(req, "editor"),
      part = await req.file();
    assert(part, "Select a file");
    const ext = part.filename.split(".").at(-1)?.toLowerCase();
    assert(
      ext && ["csv", "json", "jsonl", "parquet"].includes(ext),
      "Unsupported file format",
    );
    const file = randomUUID() + "." + ext;
    await mkdir(resolve(config.dataDir, "incoming", a.scope), {
      recursive: true,
    });
    await pipeline(
      part.file,
      createWriteStream(resolve(config.dataDir, "incoming", a.scope, file)),
    );
    assert(!part.file.truncated, "File exceeds 256 MB limit");
    return ingestFile(
      store,
      a.scope,
      part.filename.replace(/\.[^.]+$/, ""),
      file,
    );
  });
  app.post("/api/v1/workspaces/:w/datasets/:id/preview", async (req: any) => {
    const a = await access(req);
    const ds = await store.get(a.scope, req.params.id);
    assert(ds?.kind === "dataset", "Dataset unavailable", 404);
    const b = z
      .object({
        offset: z.number().int().min(0).default(0),
        limit: z.number().int().min(1).max(500).default(50),
      })
      .parse(req.body || {});
    return queryDatasets(
      store,
      a.scope,
      [ds.id],
      `SELECT * FROM "${datasetTable(ds.id)}"`,
      b,
    );
  });
  app.patch("/api/v1/workspaces/:w/datasets/:id", async (req: any) => {
    const a = await access(req, "editor"),
      b = z.object({ description: z.string().max(4000) }).parse(req.body);
    return transaction(store, a.scope, async (tx) => {
      const ds = await tx.get(req.params.id);
      assert(ds?.kind === "dataset", "Dataset unavailable", 404);
      return tx.put({
        ...ds,
        description: b.description,
        revision: ds.revision + 1,
        data: { ...ds.data, manualDescription: true },
      });
    });
  });
  app.post(
    "/api/v1/workspaces/:w/datasets/:id/accept-version",
    async (req: any) => {
      const a = await access(req, "editor");
      return transaction(store, a.scope, async (tx) => {
        const ds = await tx.get(req.params.id);
        assert(ds?.data.pendingVersion, "No pending version");
        const v = await tx.get(ds.data.pendingVersion);
        assert(v, "Version missing");
        return tx.update(ds, {
          ...ds.data,
          activeVersion: v.id,
          pendingVersion: null,
          profile: v.data.profile,
        });
      });
    },
  );
  app.get(
    "/api/v1/workspaces/:w/datasets/:id/export",
    async (req: any, reply) => {
      const a = await access(req),
        ds = await store.get(a.scope, req.params.id);
      assert(ds?.kind === "dataset", "Dataset unavailable", 404);
      reply
        .header("content-disposition", `attachment; filename="dataset.parquet"`)
        .type("application/octet-stream");
      return createReadStream(
        resolve(
          config.dataDir,
          "datasets",
          a.scope,
          ds.data.activeVersion + ".parquet",
        ),
      );
    },
  );
  app.post("/api/v1/workspaces/:w/sources", async (req) => {
    const a = await access(req, "owner"),
      b = SourceSchema.parse(req.body);
    if (b.schedule) CronExpressionParser.parse(b.schedule, { tz: "UTC" });
    const resolved = await resolveHostedConfig(
      store,
      a.scope,
      b.kind,
      b.config,
    );
    const discovery = await discover(b.kind, resolved.config);
    return transaction(store, a.scope, async (tx) =>
      scrub(
        tx.put(
          resource(a.scope, "source", b.name, {
            kind: b.kind,
            secret: seal(resolved.config),
            hostedProvider: resolved.provider,
            discovery,
            ...sourceSyncDefaults(b.kind, b.schedule),
          }),
        ),
      ),
    );
  });
  app.post("/api/v1/workspaces/:w/sources/test", async (req) => {
    const a = await access(req, "owner");
    const b = SourceSchema.parse(req.body);
    return testedConnection(
      b.kind,
      (await resolveHostedConfig(store, a.scope, b.kind, b.config)).config,
    );
  });
  app.post("/api/v1/workspaces/:w/sources/:id/sync", async (req: any) => {
    const a = await access(req, "editor"),
      source = await store.get(a.scope, req.params.id);
    assert(source?.kind === "source", "Source unavailable", 404);
    return transaction(store, a.scope, async (tx) => {
      const pending = (await tx.list("job")).find(
        (j) =>
          j.data.sourceId === source.id &&
          ["queued", "running"].includes(j.data.status),
      );
      // A scheduled or CDC sync can be waiting for a workspace build; asked
      // for, it runs as soon as the worker is free, like a manual sync.
      if (pending?.data.background && pending.data.status === "queued")
        return tx.update(pending, { ...pending.data, background: false });
      if (pending) return pending;
      return tx.put(
        resource(a.scope, "job", "Sync " + source.name, {
          type: "sync",
          sourceId: source.id,
          status: "queued",
        }),
      );
    });
  });
  app.post("/api/v1/workspaces/:w/query", async (req) => {
    const a = await access(req),
      b = z
        .object({
          inputs: z.array(z.string()).min(1).max(20),
          sql: z.string().max(20000),
          filters: z
            .array(
              z.object({
                datasetId: z.string(),
                column: z.string(),
                value: z.union([z.string(), z.number(), z.boolean(), z.null()]),
              }),
            )
            .max(20)
            .optional(),
        })
        .parse(req.body);
    return queryDatasets(store, a.scope, b.inputs, b.sql, {
      filters: b.filters,
    });
  });
  app.post("/api/v1/workspaces/:w/queries", async (req) => {
    const a = await access(req, "editor"),
      b = z
        .object({
          name: z.string(),
          inputs: z.array(z.string()).min(1),
          sql: z.string(),
        })
        .parse(req.body);
    await queryDatasets(store, a.scope, b.inputs, b.sql);
    return transaction(store, a.scope, async (tx) =>
      tx.put(resource(a.scope, "query", b.name, b)),
    );
  });
  app.post("/api/v1/workspaces/:w/findings/:id/dismiss", async (req: any) => {
    const a = await access(req, "editor");
    return transaction(store, a.scope, async (tx) => {
      const f = await tx.get(req.params.id);
      assert(f?.kind === "finding", "Finding unavailable");
      return tx.update(f, { ...f.data, dismissed: true });
    });
  });
}
