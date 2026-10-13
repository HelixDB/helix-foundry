import {
  afterAll,
  afterEach,
  beforeAll,
  describe,
  expect,
  it,
  vi,
} from "vitest";
import { createServer } from "node:http";
import { once } from "node:events";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { createApp } from "../apps/api/src/app.js";
import { seal } from "../apps/api/src/security.js";
import { config } from "../apps/api/src/config.js";
import { MemoryStore, transaction } from "../apps/api/src/store.js";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { buildWorkspace } from "../apps/api/src/assistant.js";
import {
  metricsDatasets,
  describeForMetrics,
  computedSpecs,
  schemaSignature,
} from "../apps/api/src/metrics.js";
import { FoundryClient } from "../packages/sdk/src/index.js";
import { datasetTable } from "../packages/shared/src/index.js";
import { pipelineId } from "../apps/api/src/datasets.js";
const originalExecutor = config.executor;
const executor = createServer(async (req, res) => {
  try {
    const chunks: Buffer[] = [];
    for await (const c of req) chunks.push(c);
    const result = await nativeExecute(
      JSON.parse(Buffer.concat(chunks).toString()),
    );
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify(result));
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
  await new Promise<void>((r) => executor.close(() => r()));
});
const owned: { app: Awaited<ReturnType<typeof createApp>>; scope: string }[] =
  [];
afterEach(async () => {
  vi.restoreAllMocks();
  for (const f of owned.splice(0)) {
    await f.app.close();
    for (const d of ["incoming", "datasets", "results"])
      await rm(resolve(config.dataDir, d, f.scope), {
        recursive: true,
        force: true,
      });
  }
});
async function fixture() {
  const store = new MemoryStore(),
    app = await createApp(store),
    origin = await app.listen({ host: "127.0.0.1", port: 0 });
  const scope = (await app.inject("/api/v1/me")).json().workspaces[0].id,
    base = `/api/v1/workspaces/${scope}`;
  owned.push({ app, scope });
  const rows = Array.from({ length: 6 }, (_, i) => ({
    id: i,
    region: i % 2 ? "PRIVATE-SOUTH" : "PRIVATE-NORTH",
    status: i % 2 ? "active" : "inactive",
    amount: 10 + i,
    email: `private-${i}@example.com`,
  }));
  const ingest = async (rows: any[], datasetId?: string) => {
    const r = await app.inject({
      method: "POST",
      url: base + "/ingest",
      payload: { name: "Private policies", rows, datasetId },
    });
    expect(r.statusCode).toBe(200);
    return r.json();
  };
  const ds = await ingest(rows),
    get = async () => {
      const r = await app.inject(base + "/resources/dataset/" + ds.id);
      expect(r.statusCode).toBe(200);
      return r.json();
    };
  const policy = async (additionalColumns: string[]) => {
    const r = await app.inject({
      method: "PUT",
      url: base + `/datasets/${ds.id}/privacy`,
      payload: { additionalColumns },
    });
    expect(r.statusCode).toBe(200);
    return r.json();
  };
  return { store, app, scope, base, origin, rows, ds, ingest, get, policy };
}
async function modelRequest(
  f: Awaited<ReturnType<typeof fixture>>,
  provider: "openai" | "local" = "openai",
  allowRawData = false,
) {
  const requests: any[] = [];
  const plan = {
    title: "Revenue",
    inputs: [f.ds.id],
    sql: `SELECT sum(amount) revenue FROM "${datasetTable(f.ds.id)}"`,
    assumptions: [],
  };
  const model = createServer(async (req, res) => {
    const c: Buffer[] = [];
    for await (const x of req) c.push(x);
    const b = JSON.parse(Buffer.concat(c).toString());
    requests.push(b);
    res.setHeader("content-type", "application/json");
    res.end(
      JSON.stringify(
        provider === "openai"
          ? {
              output: [
                {
                  content: [
                    { type: "output_text", text: JSON.stringify(plan) },
                  ],
                },
              ],
              usage: { total_tokens: 1 },
            }
          : {
              message: { content: JSON.stringify(plan) },
              prompt_eval_count: 1,
              eval_count: 1,
            },
      ),
    );
  });
  model.listen(0, "127.0.0.1");
  await once(model, "listening");
  const url = `http://127.0.0.1:${(model.address() as any).port}`;
  const nativeFetch = globalThis.fetch;
  const fetchSpy = vi
    .spyOn(globalThis, "fetch")
    .mockImplementation((input, init) => {
      const target = String(input);
      if (target === "https://api.openai.com/v1/responses")
        return nativeFetch(url + "/responses", init);
      const parsed = new URL(target);
      if (parsed.hostname !== "127.0.0.1" && parsed.hostname !== "localhost")
        throw new Error("External request forbidden in privacy fixture");
      return nativeFetch(input, init);
    });
  try {
    await transaction(f.store, f.scope, async (tx) => {
      const p = await tx.get("provider");
      const data = {
        secret: seal("synthetic-fixture-key"),
        settings: {
          provider,
          model: "fixture",
          baseUrl: url,
          allowRawData,
        },
      };
      if (p) tx.update(p, data);
      else
        tx.put({
          id: "provider",
          workspaceId: f.scope,
          kind: "provider",
          name: "Fixture",
          description: "",
          revision: 1,
          createdAt: new Date().toISOString(),
          updatedAt: new Date().toISOString(),
          data,
        });
    });
    const run = (
      await f.app.inject({
        method: "POST",
        url: f.base + "/assistant/runs",
        payload: { goal: "What is revenue?", intent: "answer" },
      })
    ).json();
    await buildWorkspace(f.store, f.scope, run.id);
    const done = (await f.store.get(f.scope, run.id))!;
    expect(done.data.status, String(done.data.error)).toBe("succeeded");
    expect(done.data.answer.rows[0].revenue).toBe("75");
    return provider === "openai"
      ? JSON.parse(requests[0].input)
      : JSON.parse(requests[0].messages[1].content);
  } finally {
    fetchSpy.mockRestore();
    model.closeAllConnections();
    await new Promise<void>((r) => model.close(() => r()));
  }
}
describe("owner-managed additional dataset sensitivity", () => {
  it("retains policy when a real proposal republishes the stable pipeline dataset identity", async () => {
    const f = await fixture();
    const name = "Privacy derived",
      id = pipelineId(name);
    const publish = async () => {
      const draft = await f.app.inject({
        method: "POST",
        url: f.base + "/pipelines/draft",
        payload: {
          name,
          inputs: [f.ds.id],
          sql: `SELECT * FROM "${datasetTable(f.ds.id)}"`,
        },
      });
      expect(draft.statusCode, draft.body).toBe(200);
      const proposal = draft.json();
      expect(
        proposal.data.validation.valid,
        JSON.stringify(proposal.data.validation),
      ).toBe(true);
      const result = await f.app.inject({
        method: "POST",
        url: f.base + `/proposals/${proposal.id}/publish`,
        payload: { hash: proposal.data.hash },
      });
      expect(result.statusCode, result.body).toBe(200);
    };
    await publish();
    expect(
      (
        await f.app.inject({
          method: "PUT",
          url: f.base + `/datasets/${id}/privacy`,
          payload: { additionalColumns: ["region"] },
        })
      ).statusCode,
    ).toBe(200);
    const first = (await f.store.get(f.scope, id))!;
    await publish();
    const next = (
      await f.app.inject(f.base + `/resources/dataset/${id}`)
    ).json();
    expect(next.data.generation).not.toBe(first.data.generation);
    expect(next.data.privacy.additionalColumns).toEqual(["region"]);
    expect(
      next.data.profile.columns.find((c: any) => c.name === "region").sensitive,
    ).toBe(true);
    expect(
      (await f.store.get(
        f.scope,
        next.data.activeVersion,
      ))!.data.profile.columns.find((c: any) => c.name === "region").sensitive,
    ).toBe(false);
  });
  it("retains native query values and automatic hosted masking when no policy is set", async () => {
    const f = await fixture();
    const ds = await f.get();
    expect(
      ds.data.profile.columns.find((c: any) => c.name === "region").sensitive,
    ).toBe(false);
    const context = await modelRequest(f);
    expect(context.datasets[0].sample[0].email).toBe("[masked]");
    expect(context.datasets[0].sample[0].region).toContain("PRIVATE-");
    const metrics = await metricsDatasets(f.store, f.scope);
    expect(JSON.stringify(describeForMetrics(metrics))).toContain(
      "PRIVATE-NORTH",
    );
  });
  it("writes canonical owner policy through the SDK and rejects read-only, foreign and unknown-column changes", async () => {
    const f = await fixture(),
      token = (
        await f.app.inject({
          method: "POST",
          url: f.base + "/tokens",
          payload: { name: "Privacy SDK", readOnly: false },
        })
      ).json();
    const client = new FoundryClient({
      baseUrl: f.origin,
      workspaceId: f.scope,
      token: token.token,
    });
    const result = await client.setDatasetPrivacy(f.ds.id, [
      "region",
      "region",
    ]);
    expect(result.data.privacy.additionalColumns).toEqual(["region"]);
    const readOnly = (
      await f.app.inject({
        method: "POST",
        url: f.base + "/tokens",
        payload: { name: "Read only" },
      })
    ).json();
    expect(
      (
        await f.app.inject({
          method: "PUT",
          url: f.base + `/datasets/${f.ds.id}/privacy`,
          headers: { authorization: "Bearer " + readOnly.token },
          payload: { additionalColumns: [] },
        })
      ).statusCode,
    ).toBe(403);
    expect(
      (
        await f.app.inject({
          method: "PUT",
          url: f.base + `/datasets/${f.ds.id}/privacy`,
          payload: { additionalColumns: ["invented"] },
        })
      ).statusCode,
    ).toBe(400);
    const other = (
      await f.app.inject({
        method: "POST",
        url: "/api/v1/workspaces",
        payload: { name: "Foreign" },
      })
    ).json();
    expect(
      (
        await f.app.inject({
          method: "PUT",
          url: `/api/v1/workspaces/${other.id}/datasets/${f.ds.id}/privacy`,
          payload: { additionalColumns: [] },
        })
      ).statusCode,
    ).toBe(404);
  });
  it("masks additional hosted samples and metric categories without altering immutable profile or query access", async () => {
    const f = await fixture(),
      before = await f.get(),
      signature = schemaSignature(await metricsDatasets(f.store, f.scope));
    await f.policy(["region", "amount"]);
    const effective = await f.get();
    expect(effective.data.activeVersion).toBe(before.data.activeVersion);
    expect(effective.data.profile.fingerprint).toBe(
      before.data.profile.fingerprint,
    );
    const immutable = (await f.store.get(f.scope, f.ds.data.activeVersion))!;
    expect(
      immutable.data.profile.columns.find((c: any) => c.name === "region")
        .sensitive,
    ).toBe(false);
    const context = await modelRequest(f);
    expect(context.datasets[0].sample[0].region).toBe("[masked]");
    expect(context.datasets[0].sample[0].amount).toBe("[masked]");
    expect(context.datasets[0].sample[0].status).toBeTruthy();
    const metrics = await metricsDatasets(f.store, f.scope);
    expect(JSON.stringify(describeForMetrics(metrics))).not.toContain(
      "PRIVATE-NORTH",
    );
    expect(schemaSignature(metrics)).not.toBe(signature);
    expect(
      computedSpecs(metrics).some(
        (s) => s.column === "amount" || s.groupBy === "region",
      ),
    ).toBe(false);
    const preview = (
      await f.app.inject({
        method: "POST",
        url: f.base + `/datasets/${f.ds.id}/preview`,
        payload: {},
      })
    ).json();
    expect(preview.rows[0].region).toContain("PRIVATE-");
    await f.policy([]);
    expect(
      (await f.get()).data.profile.columns.find((c: any) => c.name === "email")
        .sensitive,
    ).toBe(true);
    expect(schemaSignature(await metricsDatasets(f.store, f.scope))).toBe(
      signature,
    );
  });
  it("keeps privacy across ordinary refresh, accepted schema changes and disappearing/reappearing columns", async () => {
    const f = await fixture();
    await f.policy(["region"]);
    await f.ingest(
      f.rows.map((r) => ({ ...r, amount: r.amount + 1 })),
      f.ds.id,
    );
    expect(
      (await f.get()).data.profile.columns.find((c: any) => c.name === "region")
        .sensitive,
    ).toBe(true);
    const updated = await f.ingest(
      f.rows.map(({ region, ...r }) => ({ ...r, extra: 1 })),
      f.ds.id,
    );
    expect(updated.data.pendingVersion).toBeTruthy();
    expect(
      (
        await f.app.inject({
          method: "POST",
          url: f.base + `/datasets/${f.ds.id}/accept-version`,
          payload: {},
        })
      ).statusCode,
    ).toBe(200);
    expect((await f.get()).data.privacy.additionalColumns).toEqual(["region"]);
    await f.ingest(
      f.rows.map((r) => ({ ...r, extra: 1 })),
      f.ds.id,
    );
    expect(
      (
        await f.app.inject({
          method: "POST",
          url: f.base + `/datasets/${f.ds.id}/accept-version`,
          payload: {},
        })
      ).statusCode,
    ).toBe(200);
    expect(
      (await f.get()).data.profile.columns.find((c: any) => c.name === "region")
        .sensitive,
    ).toBe(true);
  });
  it("preserves explicit hosted raw-data opt-in and Local AI behavior with additional policy", async () => {
    const f = await fixture();
    await f.policy(["region"]);
    for (const [provider, allowRawData] of [
      ["openai", true],
      ["local", false],
    ] as const) {
      const context = await modelRequest(f, provider, allowRawData);
      expect(context.datasets[0].sample[0].region).toContain("PRIVATE-");
      expect(context.datasets[0].sample[0].email).toContain("@example.com");
    }
  });
});
