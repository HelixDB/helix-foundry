import { afterEach, beforeEach, expect, it, vi } from "vitest";
vi.mock("../apps/api/src/connectors.js", async (original) => ({
  ...(await original<any>()),
  discover: vi.fn(async () => ({ tables: [], columns: [], sample: [] })),
}));
import { createApp } from "../apps/api/src/app.js";
import { discover } from "../apps/api/src/connectors.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { unseal } from "../apps/api/src/security.js";

let app: Awaited<ReturnType<typeof createApp>>,
  store: MemoryStore,
  scope: string;
beforeEach(async () => {
  vi.mocked(discover).mockClear();
  store = new MemoryStore();
  app = await createApp(store);
  await app.ready();
  scope = (await app.inject({ method: "GET", url: "/api/v1/me" })).json()
    .workspaces[0].id;
});
afterEach(() => app.close());
const request = (method: "POST" | "PUT", path: string, payload: unknown) =>
  app.inject({
    method,
    url: `/api/v1/workspaces/${scope}${path}`,
    payload: payload as any,
  });
const connection = {
  name: "Sales",
  kind: "rest",
  config: { url: "http://127.0.0.1/fixture" },
};

const routes = [
  ["POST", "/sources", {}],
  [
    "POST",
    "/onboarding/sources",
    { selections: [{}], operationId: "invalid-schedule" },
  ],
  ["PUT", "/onboarding/sources/existing", {}],
  ["PUT", "/sources/existing/schedule", {}],
] as const;
const existing = () =>
  transaction(store, scope, async (tx) => {
    tx.put(
      resource(
        scope,
        "source",
        "Existing",
        { kind: "rest", secret: "unchanged" },
        "existing",
      ),
    );
  });
const unchanged = async () => {
  expect(discover).not.toHaveBeenCalled();
  const source = await store.get(scope, "existing");
  expect(source?.data.secret).toBe("unchanged");
  expect(source?.data.schedule).toBeUndefined();
  expect(await store.list(scope, "job")).toHaveLength(0);
  expect(await store.list(scope, "operation")).toHaveLength(0);
};

for (const [method, path, extra] of routes)
  it(`rejects invalid schedules through ${method} ${path} before discovery`, async () => {
    await existing();
    const response = await request(method, path, {
      ...connection,
      ...extra,
      schedule: "not a cron schedule",
    });
    expect(response.statusCode, response.body).toBe(400);
    expect(response.json().error).toContain("refresh schedule");
    await unchanged();
  });

// Six-field expressions start with a seconds field.
for (const [method, path, extra] of routes)
  it(`rejects schedules that run more than once a minute through ${method} ${path}`, async () => {
    await existing();
    for (const schedule of [
      "* * * * * *",
      "*/30 * * * * *",
      "0,30 * * * * *",
      "@secondly",
    ]) {
      const response = await request(method, path, {
        ...connection,
        ...extra,
        schedule,
      });
      expect(response.statusCode, response.body).toBe(400);
      expect(response.json().error).toContain("at most once a minute");
    }
    await unchanged();
  });

it("accepts schedules that run at most once a minute", async () => {
  const created = await request("POST", "/sources", connection);
  expect(created.statusCode, created.body).toBe(200);
  for (const schedule of [
    "* * * * *",
    "@minutely",
    "30 * * * * *",
    "0 */5 * * * *",
    "@hourly",
  ]) {
    const response = await request(
      "PUT",
      `/sources/${created.json().id}/schedule`,
      { schedule },
    );
    expect(response.statusCode, schedule + response.body).toBe(200);
    expect(response.json().data.schedule).toBe(schedule);
    expect(response.json().data.nextRun).toBeGreaterThan(Date.now() - 1000);
  }
  const response = await request("POST", "/sources", {
    ...connection,
    schedule: "* * * * *",
  });
  expect(response.statusCode, response.body).toBe(200);
  expect(response.json().data.schedule).toBe("* * * * *");
});

it("accepts valid cron expressions and keeps the saved UTC schedule", async () => {
  const response = await request("POST", "/sources", {
    ...connection,
    schedule: "15 7 * * 1",
  });
  expect(response.statusCode, response.body).toBe(200);
  const saved = (await store.get(scope, response.json().id))!;
  expect(saved.data.schedule).toBe("15 7 * * 1");
  const next = new Date(saved.data.nextRun);
  expect([next.getUTCDay(), next.getUTCHours(), next.getUTCMinutes()]).toEqual([
    1, 7, 15,
  ]);
  expect(unseal(saved.data.secret)).toEqual(connection.config);
});

for (const schedule of [undefined, null, ""])
  it(`keeps the default automatic refresh for ${String(schedule)}`, async () => {
    const response = await request("POST", "/sources", {
      ...connection,
      schedule,
    });
    expect(response.statusCode, response.body).toBe(200);
    const saved = (await store.get(scope, response.json().id))!;
    expect(saved.data.schedule).toBeNull();
    expect(saved.data.nextRun).toBeGreaterThan(Date.now());
  });
