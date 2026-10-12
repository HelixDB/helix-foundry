import { beforeEach, afterEach, it, expect, vi } from "vitest";
import { rm, writeFile, mkdir } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as localExecute } from "../services/executor/src/engine.js";
vi.mock("../apps/api/src/executor.js", () => ({
  execute: (job: any) => localExecute(job),
}));
vi.mock("../apps/api/src/connectors.js", async (importOriginal) => ({
  ...(await importOriginal<any>()),
  discover: vi.fn(async (kind: string) => ({
    tables: [
      { name: "customers", schema: kind === "postgres" ? "public" : undefined },
      { name: "orders", schema: kind === "postgres" ? "public" : undefined },
      { name: "private", schema: kind === "postgres" ? "public" : undefined },
    ],
    keys: [],
    columns: [],
    sample: [],
  })),
}));
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { ingestRows, ingestFile } from "../apps/api/src/datasets.js";
import { sampleDatasets } from "../apps/api/src/sample-data.js";
import {
  buildWorkspace,
  publishProposal,
  saveProposal,
} from "../apps/api/src/assistant.js";
import { onboardingState } from "../apps/api/src/onboarding-state.js";
import { connectionGroups } from "../apps/web/src/connection-groups.js";
import { Provider } from "../apps/api/src/providers.js";
import { prepareProvider } from "../apps/api/src/setup-jobs.js";
import { discover } from "../apps/api/src/connectors.js";
import { config } from "../apps/api/src/config.js";
import {
  referenceReply,
  customerRows,
  orderRows,
} from "./fixtures-reference.js";
let store: MemoryStore,
  app: Awaited<ReturnType<typeof createApp>>,
  scope: string,
  actor: string;
const request = (method: any, path: string, payload?: unknown) =>
  app.inject({
    method,
    url: `/api/v1/workspaces/${scope}` + path,
    payload: payload as any,
  });
beforeEach(async () => {
  store = new MemoryStore();
  app = await createApp(store);
  await app.ready();
  const r = await app.inject({ method: "GET", url: "/api/v1/me" });
  expect(r.statusCode).toBe(200);
  scope = r.json().workspaces[0].id;
  actor = r.json().user.id;
});
afterEach(async () => {
  vi.restoreAllMocks();
  await app.close();
  for (const d of ["incoming", "datasets"])
    await rm(resolve(config.dataDir, d, scope), {
      recursive: true,
      force: true,
    });
});
async function inputs() {
  await ingestRows(
    store,
    scope,
    "Customers",
    customerRows,
    undefined,
    "customers",
  );
  await ingestRows(store, scope, "Orders", orderRows, undefined, "orders");
  await transaction(store, scope, async (tx) =>
    tx.put(
      resource(
        scope,
        "provider",
        "AI",
        { settings: { provider: "local", model: "test-model" } },
        "provider",
      ),
    ),
  );
}
function model() {
  return vi
    .spyOn(Provider.prototype, "generate")
    .mockImplementation(async (system, context) => ({
      value: referenceReply(system, context),
      tokens: 12,
    }));
}
it("adds sample datasets alongside existing data and restores only the samples on retry", async () => {
  const real = await ingestRows(store, scope, "Customers", [
    { id: "real-customer" },
  ]);
  await request("PATCH", "/onboarding", { excludedIds: [real.id] });
  const initial = await request("POST", "/demo", {});
  expect(initial.statusCode, initial.body).toBe(200);
  const samples = initial.json().datasets as any[];
  const expected = sampleDatasets();
  expect(samples.map((d) => d.name)).toEqual(expected.map((d) => d.name));
  expect(samples.map((d) => d.data.profile.rows)).toEqual(
    expected.map((d) => d.rows.length),
  );
  expect(samples.map((d) => d.data.sampleSource)).toEqual(
    expected.map((d) => d.source),
  );
  expect(samples.every((d) => d.data.sampleData)).toBe(true);
  const ids = samples.map((d) => d.id);
  expect(ids).not.toContain(real.id);
  await request("PATCH", "/onboarding", { excludedIds: [real.id, ...ids] });
  expect((await onboardingState(store, scope)).datasets).toHaveLength(0);
  const retry = await request("POST", "/demo", {});
  expect(retry.json().datasets.map((d: any) => d.id)).toEqual(ids);
  expect(retry.json().datasets.map((d: any) => d.data.activeVersion)).toEqual(
    samples.map((d) => d.data.activeVersion),
  );
  const state = await onboardingState(store, scope);
  expect(state.inputsReady).toBe(true);
  expect(state.excludedIds).toEqual([real.id]);
  expect(state.datasets).toHaveLength(expected.length);
  expect(connectionGroups(state)).toHaveLength(0);
  expect(await store.list(scope, "dataset")).toHaveLength(expected.length + 1);
  expect((await store.get(scope, real.id))?.data.profile.rows).toBe(1);
});
it("replaces an earlier sample set and keeps it out of setup and AI runs", async () => {
  const old = await ingestRows(store, scope, "Customers", [
    { customer_id: "C001" },
  ]);
  await transaction(store, scope, async (tx) => {
    const workspace = (await tx.get("workspace"))!;
    tx.update(workspace, {
      ...workspace.data,
      sampleDatasetIds: { Customers: old.id },
    });
  });
  const r = await request("POST", "/demo", {});
  expect(r.statusCode, r.body).toBe(200);
  const state = await onboardingState(store, scope);
  expect(state.excludedIds).toContain(old.id);
  expect(state.datasets.map((d) => d.id)).not.toContain(old.id);
  expect(state.datasets).toHaveLength(sampleDatasets().length);
  expect((await store.get(scope, old.id))?.data.retiredSample).toBe(true);
  expect(
    (await store.get(scope, "workspace"))?.data.sampleDatasetIds,
  ).not.toHaveProperty("Customers");
});
it("retries recommendations that cite missing columns", async () => {
  await inputs();
  let calls = 0;
  vi.spyOn(Provider.prototype, "generate").mockImplementation(
    async (system, context: any) => {
      const value: any = referenceReply(system, context);
      // The first answer cites a column that does not exist.
      if (system.includes("up to three distinct") && calls++ === 0)
        value.outcomes[0].evidence[0].columns = ["invented"];
      return { value, tokens: 12 };
    },
  );
  const r = await request("POST", "/onboarding/recommendations", {});
  await buildWorkspace(store, scope, r.json().id);
  expect(calls).toBe(2);
  expect(
    (await onboardingState(store, scope)).recommendations?.outcomes,
  ).toHaveLength(1);
});
it("recommends from full snapshots, resumes, tests one view and atomically completes on publication", async () => {
  await inputs();
  const ai = model();
  const r = await request("POST", "/onboarding/recommendations", {});
  expect(r.statusCode, r.body).toBe(200);
  expect(
    (await request("POST", "/onboarding/recommendations", {})).json().id,
  ).toBe(r.json().id);
  await buildWorkspace(store, scope, r.json().id);
  const setup = await onboardingState(store, scope);
  expect(setup.recommendations?.outcomes).toHaveLength(1);
  expect(setup.complete).toBe(false);
  const goal = setup.recommendations!.outcomes[0].goal;
  const run = (await request("POST", "/onboarding/build", { goal })).json();
  await buildWorkspace(store, scope, run.id);
  const reviewed = await onboardingState(store, scope);
  expect(reviewed.proposal?.data.validation.valid).toBe(true);
  expect(reviewed.proposal?.data.bundle.dashboards).toHaveLength(1);
  expect(
    String(
      reviewed.proposal?.data.validation.previews["chart:Business view:0"][0]
        .revenue,
    ),
  ).toBe("35");
  expect(
    String(
      reviewed.proposal?.data.validation.previews["chart:Business view:1"][0]
        .delayed,
    ),
  ).toBe("1");
  await request("PATCH", "/onboarding", { deferred: true });
  await app.close();
  app = await createApp(store);
  await app.ready();
  expect((await request("GET", "/onboarding")).json().proposal.id).toBe(
    reviewed.proposal!.id,
  );
  const p = reviewed.proposal!;
  await publishProposal(store, scope, p.id, p.data.hash, actor);
  const complete = await onboardingState(store, scope);
  expect(complete.complete).toBe(true);
  expect(complete.dashboardId).toBeTruthy();
  expect(
    (await store.get(scope, "workspace"))?.data.onboarding.completedAt,
  ).toBeTruthy();
  expect(await store.list(scope, "object")).toHaveLength(5);
  expect(await store.list(scope, "relation")).toHaveLength(3);
  expect(ai).toHaveBeenCalledTimes(5);
});
it.each(["mysql", "postgres"])(
  "imports only selected %s tables, deduplicates operations and blocks incomplete imports",
  async (kind) => {
    const payload = {
      kind,
      name: "Sales",
      config: { database: "sales", password: "secret-test" },
      selections: [
        {
          table: "customers",
          ...(kind === "postgres" ? { schema: "public" } : {}),
        },
        {
          table: "orders",
          ...(kind === "postgres" ? { schema: "public" } : {}),
        },
      ],
      operationId: "connection-123",
    };
    const r = await request("POST", "/onboarding/sources", payload);
    expect(r.statusCode, r.body).toBe(200);
    expect(r.json()).toHaveLength(2);
    expect(r.body).not.toContain("secret-test");
    expect(
      (await request("POST", "/onboarding/sources", payload)).json(),
    ).toEqual(r.json());
    expect(await store.list(scope, "source")).toHaveLength(2);
    const groups = connectionGroups(await onboardingState(store, scope));
    expect(groups).toHaveLength(1);
    expect(groups[0].sources).toHaveLength(2);
    expect(groups[0].name).toBe("Sales");
    // Previous installations have the batch identity only on the operation.
    await transaction(store, scope, async (tx) => {
      for (const source of await tx.list("source")) {
        const { connectionId, connectionName, ...data } = source.data;
        tx.update(source, data);
      }
    });
    expect(connectionGroups(await onboardingState(store, scope))).toHaveLength(
      1,
    );
    await inputs();
    expect(
      (await request("POST", "/onboarding/build", { goal: "Sales dashboard" }))
        .statusCode,
    ).toBe(409);
    const jobs = await store.list(scope, "job");
    await transaction(store, scope, async (tx) =>
      tx.update(jobs[0], {
        ...jobs[0].data,
        status: "failed",
        error: "Connection interrupted",
      }),
    );
    expect(
      (
        await request("POST", `/onboarding/imports/${jobs[0].id}/retry`, {})
      ).json().data.status,
    ).toBe("queued");
    expect(
      (
        await request("POST", "/onboarding/sources", {
          ...payload,
          selections: [{ table: "private" }],
        })
      ).statusCode,
    ).toBe(409);
    const sources = await store.list(scope, "source");
    await request("PATCH", "/onboarding", {
      excludedIds: sources.map((s) => s.id),
    });
    expect((await onboardingState(store, scope)).inputsReady).toBe(true);
  },
);
it("blocks stale approvals when goal, selection or source versions change, including definition retests", async () => {
  await inputs();
  model();
  const run = (
    await request("POST", "/onboarding/build", {
      goal: "Revenue and delayed orders",
    })
  ).json();
  await buildWorkspace(store, scope, run.id);
  const p = (await onboardingState(store, scope)).proposal!;
  await request("PATCH", "/onboarding", { goal: "Only delivered revenue" });
  await expect(
    publishProposal(store, scope, p.id, p.data.hash, actor),
  ).rejects.toThrow(/Setup inputs or outcome changed/);
  const retested = await saveProposal(store, scope, p.data.bundle, p.id);
  await expect(
    publishProposal(store, scope, retested.id, retested.data.hash, actor),
  ).rejects.toThrow(/Setup inputs or outcome changed/);
  await request("POST", "/onboarding/build", {
    goal: "Revenue and delayed orders",
  });
  await ingestRows(
    store,
    scope,
    "Orders",
    [...orderRows, { id: 4, customer: "B", amount: 99, status: "delayed" }],
    undefined,
    "orders",
  );
  expect((await onboardingState(store, scope)).stale).toBe(true);
  await expect(
    publishProposal(store, scope, p.id, p.data.hash, actor),
  ).rejects.toThrow(/Input data changed/);
});
it("enforces workspace and credential boundaries and cannot accept a completion flag", async () => {
  expect(
    (await request("PATCH", "/onboarding", { complete: true })).json().complete,
  ).toBe(false);
  expect(
    (await request("PATCH", "/onboarding", { excludedIds: ["foreign-id"] }))
      .statusCode,
  ).toBe(404);
  const { token } = (
    await request("POST", "/tokens", { name: "Read only", readOnly: true })
  ).json();
  const readOnly = (method: any, path: string, payload: unknown) =>
    app.inject({
      method,
      url: `/api/v1/workspaces/${scope}` + path,
      payload: payload as any,
      headers: { authorization: `Bearer ${token}` },
    });
  expect(
    (
      await readOnly("POST", "/onboarding/provider", {
        provider: "openai",
        model: "test",
        apiKey: "private-key",
      })
    ).statusCode,
  ).toBe(403);
  expect(
    (
      await readOnly("POST", "/onboarding/sources", {
        kind: "mysql",
        name: "Test",
        config: {},
        selections: [{ table: "customers" }],
        operationId: "operation-123",
      })
    ).statusCode,
  ).toBe(403);
  expect(
    (await readOnly("PATCH", "/onboarding", { step: "data" })).statusCode,
  ).toBe(403);
});
it("stages asynchronous uploads durably, reuses retries and refuses canceled snapshot commits", async () => {
  const boundary = "onboarding-test-boundary",
    body = `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="Customers.csv"\r\nContent-Type: text/csv\r\n\r\nid,name\nA,Acme\n\r\n--${boundary}--\r\n`;
  const upload = () =>
    app.inject({
      method: "POST",
      url: `/api/v1/workspaces/${scope}/onboarding/uploads`,
      headers: {
        "content-type": `multipart/form-data; boundary=${boundary}`,
        "idempotency-key": "upload-operation",
      },
      payload: body,
    });
  const r = await upload();
  expect(r.statusCode, r.body).toBe(200);
  expect(r.json().data.file).toBeUndefined();
  expect((await upload()).json().id).toBe(r.json().id);
  expect(await store.list(scope, "dataset")).toHaveLength(0);
  const job = (await store.get(scope, r.json().id))!;
  await request("POST", `/onboarding/imports/${job.id}/cancel`, {});
  await expect(
    ingestFile(
      store,
      scope,
      job.name,
      job.data.file,
      undefined,
      job.data.datasetId,
      job.id,
      undefined,
      job.id,
    ),
  ).rejects.toThrow("Import canceled");
  expect(await store.list(scope, "dataset")).toHaveLength(0);
  await request("POST", `/onboarding/imports/${job.id}/retry`, {});
  await ingestFile(
    store,
    scope,
    job.name,
    job.data.file,
    undefined,
    job.data.datasetId,
    job.id,
    undefined,
    job.id,
  );
  await ingestFile(
    store,
    scope,
    job.name,
    job.data.file,
    undefined,
    job.data.datasetId,
    job.id,
    undefined,
    job.id,
  );
  expect(await store.list(scope, "dataset")).toHaveLength(1);
  expect(await store.list(scope, "version")).toHaveLength(1);
  expect((await store.get(scope, job.id))?.kind).toBe("job");
});
it("saves the Data step atomically with a background provider job and reuses saved credentials", async () => {
  const input = {
    provider: "claude",
    model: "test-model",
    apiKey: "private-setup-key",
    advanceToData: true,
  };
  const first = await request("POST", "/onboarding/provider", input);
  expect(first.statusCode, first.body).toBe(200);
  expect(first.json().data.status).toBe("queued");
  expect(first.body).not.toContain(input.apiKey);
  expect(await store.get(scope, "provider")).toBeUndefined();
  const state = await onboardingState(store, scope);
  expect(state.step).toBe("data");
  expect(state.providerReady).toBe(false);
  expect(state.providerJob?.data.settings.provider).toBe("claude");
  expect(JSON.stringify(state)).not.toContain(input.apiKey);
  await request("PATCH", "/onboarding", { step: "ai" });
  const repeated = await request("POST", "/onboarding/provider", {
    ...input,
    apiKey: "",
  });
  expect(repeated.json().id).toBe(first.json().id);
  expect((await onboardingState(store, scope)).step).toBe("data");
  expect(await store.list(scope, "job")).toHaveLength(1);
});
it("requires a key for a new hosted provider without changing the setup step", async () => {
  const response = await request("POST", "/onboarding/provider", {
    provider: "openai",
    model: "test",
    advanceToData: true,
  });
  expect(response.statusCode).toBe(400);
  expect((await onboardingState(store, scope)).step).toBe("ai");
  expect(await store.list(scope, "job")).toHaveLength(0);
});
it("keeps active provider on failed tests, masks preparation secrets and activates only the selected provider", async () => {
  await inputs();
  const r = await request("POST", "/onboarding/provider", {
    provider: "openai",
    model: "test",
    apiKey: "private-hosted-key",
  });
  expect(r.statusCode, r.body).toBe(200);
  expect(r.body).not.toContain("private-hosted-key");
  const job = (await store.get(scope, r.json().id))!;
  vi.spyOn(Provider.prototype, "generate").mockRejectedValueOnce(
    new Error("Provider unavailable"),
  );
  await expect(prepareProvider(store, scope, job)).rejects.toThrow(
    "Provider unavailable",
  );
  expect((await store.get(scope, "provider"))?.data.settings.provider).toBe(
    "local",
  );
  expect(
    (await request("GET", `/resources/job/${job.id}`)).json().data.secret,
  ).toBeUndefined();
  vi.spyOn(Provider.prototype, "generate").mockResolvedValueOnce({
    value: { ready: true, message: "Connected" },
    tokens: 5,
  });
  await prepareProvider(store, scope, job);
  expect((await store.get(scope, "provider"))?.data.settings.provider).toBe(
    "openai",
  );
  expect((await onboardingState(store, scope)).providerReady).toBe(true);
});
it("rejects invented recommendation columns and preserves drafts when budgets are exhausted", async () => {
  await inputs();
  vi.spyOn(Provider.prototype, "generate").mockResolvedValue({
    value: {
      description: "Invented",
      outcomes: [
        {
          title: "Profit",
          question: "How much profit?",
          goal: "Show profit",
          evidence: [{ datasetId: "orders", columns: ["imaginary_margin"] }],
        },
      ],
    },
    tokens: 8,
  });
  const r = (await request("POST", "/onboarding/recommendations", {})).json();
  await buildWorkspace(store, scope, r.id);
  expect((await store.get(scope, r.id))?.data.status).toBe("failed");
  expect((await onboardingState(store, scope)).recommendations).toBeUndefined();
  vi.restoreAllMocks();
  model();
  await transaction(store, scope, async (tx) => {
    const p = (await tx.get("provider"))!;
    tx.update(p, { ...p.data, settings: { ...p.data.settings, maxCalls: 1 } });
  });
  const run = (
    await request("POST", "/onboarding/build", {
      goal: "Revenue and delayed orders",
    })
  ).json();
  await buildWorkspace(store, scope, run.id);
  const stopped = (await store.get(scope, run.id))!;
  expect(stopped.data.error).toContain("budget exhausted");
  expect(stopped.data.checkpoints.catalog).toBeTruthy();
  expect(await store.list(scope, "proposal")).toHaveLength(0);
});
it("surfaces TLS/authentication failures without creating partial connections", async () => {
  vi.mocked(discover).mockRejectedValueOnce(
    new Error("TLS certificate verification failed"),
  );
  const r = await request("POST", "/onboarding/sources", {
    name: "MySQL",
    kind: "mysql",
    config: { tls: true },
    selections: [{ table: "orders" }],
    operationId: "tls-failure",
  });
  expect(r.statusCode).toBeGreaterThanOrEqual(400);
  expect(await store.list(scope, "source")).toHaveLength(0);
});
it("stores each imported table's own columns, so a wide database's selection fits one write", async () => {
  // Discovery describes the whole database, far more than one table's share.
  const tables = Array.from({ length: 120 }, (_, i) => ({
    schema: "public",
    name: `t${i}`,
  }));
  const columns = tables.flatMap((t) =>
    Array.from({ length: 40 }, (_, c) => ({
      schema: t.schema,
      table_name: t.name,
      name: `column_${c}`,
      type: "character varying",
      nullable: "YES",
    })),
  );
  const foreignKeys = [
    {
      table_schema: "public",
      table_name: "t1",
      column_name: "column_0",
      referenced_schema: "public",
      referenced_table: "t0",
      referenced_column: "column_0",
    },
  ];
  vi.mocked(discover).mockResolvedValueOnce({
    tables,
    keys: [],
    columns,
    foreignKeys,
    sample: [],
  } as any);
  // HelixDB refuses a request body over its limit.
  const limit = 2_000_000,
    commit = store.commit.bind(store);
  vi.spyOn(store, "commit").mockImplementation(async (s, v, changes) => {
    if (JSON.stringify(changes).length > limit)
      throw new Error("failed to read request body: length limit exceeded");
    return commit(s, v, changes);
  });
  const r = await request("POST", "/onboarding/sources", {
    kind: "postgres",
    name: "Warehouse",
    config: { database: "warehouse" },
    selections: tables
      .slice(0, 100)
      .map((t) => ({ schema: t.schema, table: t.name })),
    operationId: "wide-database",
  });
  expect(r.statusCode, r.body).toBe(200);
  const sources = await store.list(scope, "source");
  expect(sources).toHaveLength(100);
  for (const s of sources) {
    expect(s.data.discovery.tables).toEqual([
      { schema: "public", name: s.name },
    ]);
    expect(s.data.discovery.columns).toHaveLength(40);
    expect(
      s.data.discovery.columns.every((c: any) => c.table_name === s.name),
    ).toBe(true);
    expect(s.data.discovery.foreignKeys).toEqual(foreignKeys);
  }
});
it("keeps S3 selections explicit and detects ingestion API arrivals without seeding data", async () => {
  expect((await onboardingState(store, scope)).datasets).toHaveLength(0);
  vi.mocked(discover).mockResolvedValueOnce({
    tables: [
      { name: "customers.csv" },
      { name: "orders.csv" },
      { name: "private.csv" },
    ],
  } as any);
  const r = await request("POST", "/onboarding/sources", {
    kind: "s3",
    name: "Business files",
    config: {
      bucket: "fixture",
      accessKeyId: "private-access",
      secretAccessKey: "private-secret",
    },
    selections: [{ key: "customers.csv" }, { key: "orders.csv" }],
    operationId: "s3-selected-files",
  });
  expect(r.statusCode, r.body).toBe(200);
  expect(r.json().map((v: any) => v.source.name)).toEqual([
    "customers.csv",
    "orders.csv",
  ]);
  expect(r.body).not.toContain("private-secret");
  await request("PATCH", "/onboarding", {
    excludedIds: r.json().map((v: any) => v.source.id),
  });
  const arrival = await request("POST", "/ingest", {
    name: "Orders",
    rows: orderRows,
  });
  expect(arrival.statusCode).toBe(200);
  const setup = await onboardingState(store, scope);
  expect(setup.datasets).toHaveLength(1);
  expect(setup.datasets[0].id).toBe(arrival.json().id);
  expect(setup.inputsReady).toBe(true);
});
