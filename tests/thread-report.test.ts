import { afterEach, expect, it } from "vitest";
import { mkdir, writeFile, rm } from "node:fs/promises";
import { resolve } from "node:path";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { config } from "../apps/api/src/config.js";
import { execute } from "../services/executor/src/engine.js";
import { FoundryClient } from "../packages/sdk/src/index.js";

const owned: { app: Awaited<ReturnType<typeof createApp>>; scope: string }[] =
  [];
afterEach(async () => {
  for (const { app, scope } of owned.splice(0)) {
    await app.close();
    for (const dir of ["incoming", "datasets", "results"])
      await rm(resolve(config.dataDir, dir, scope), {
        recursive: true,
        force: true,
      });
  }
});
async function fixture() {
  const store = new MemoryStore(),
    app = await createApp(store);
  const origin = await app.listen({ host: "127.0.0.1", port: 0 });
  const scope = (await app.inject("/api/v1/me")).json().workspaces[0].id;
  owned.push({ app, scope });
  await mkdir(resolve(config.dataDir, "incoming", scope), { recursive: true });
  await writeFile(
    resolve(config.dataDir, "incoming", scope, "report.csv"),
    "id,note\n9007199254740993,before\n9007199254740994,after\n",
  );
  await execute({
    mode: "ingest",
    scope,
    version: "report-snapshot",
    file: "report.csv",
    persist: true,
  });
  const sql = "SELECT * FROM report_data ORDER BY id";
  const result = await execute({
    mode: "query",
    scope,
    inputs: [{ table: "report_data", version: "report-snapshot" }],
    sql,
    limit: 1,
  });
  expect(result.rows).toEqual([{ id: "9007199254740993", note: "before" }]);
  const run = resource(
    scope,
    "run",
    "Saved native query",
    {
      goal: "Show the first saved result",
      status: "succeeded",
      intent: "answer",
      checkpoints: { provider: "PRIVATE-CHECKPOINT" },
      authority: { userId: "PRIVATE-AUTHORITY" },
      secret: "PRIVATE-RUN",
      answer: {
        title: "Native result",
        sql,
        rows: result.rows,
        total: result.profile.rows,
        inputs: ["report-dataset"],
        assumptions: ["Business assumption"],
        summary: "One preview row",
        highlights: ["Exact integer retained"],
        citations: [
          {
            datasetId: "report-dataset",
            name: "Dataset",
            version: "report-snapshot",
            secret: "PRIVATE-CITATION",
          },
        ],
        secret: "PRIVATE-ANSWER",
      },
    },
    "saved-chat",
  );
  await transaction(store, scope, async (tx) => {
    tx.put(run);
  });
  const base = `/api/v1/workspaces/${scope}`;
  return {
    app,
    store,
    scope,
    origin,
    run,
    base,
    url: base + "/threads/saved-chat/export",
  };
}

it("exports actual native snapshot results, exact digits, citations and honest preview coverage without private run fields", async () => {
  const f = await fixture();
  const before = await f.store.get(f.scope, f.run.id);
  const response = await f.app.inject(f.url);
  expect(response.statusCode).toBe(200);
  expect(response.headers["content-disposition"]).toContain(
    "analyst-report.json",
  );
  expect(response.headers["cache-control"]).toBe("no-store");
  expect(response.json()).toMatchObject({
    format: "helix-foundry.thread-evidence",
    version: 1,
    workspaceId: f.scope,
    threadId: f.run.id,
    turns: [
      {
        answer: {
          rows: [{ id: "9007199254740993", note: "before" }],
          total: 2,
          includedRows: 1,
          truncated: true,
          citations: [
            {
              datasetId: "report-dataset",
              name: "Dataset",
              version: "report-snapshot",
            },
          ],
        },
      },
    ],
  });
  expect(response.body).not.toContain("PRIVATE-");
  expect(await f.store.get(f.scope, f.run.id)).toEqual(before);
  await writeFile(
    resolve(config.dataDir, "incoming", f.scope, "report.csv"),
    "id,note\n1,new-current-data\n",
  );
  await execute({
    mode: "ingest",
    scope: f.scope,
    version: "report-snapshot",
    file: "report.csv",
    persist: true,
  });
  expect((await f.app.inject(f.url)).json().turns[0].answer.rows).toEqual([
    { id: "9007199254740993", note: "before" },
  ]);
});

it("downloads readable Markdown with fence-breaking text contained and JSON result values preserved", async () => {
  const f = await fixture();
  await transaction(f.store, f.scope, async (tx) => {
    const r = (await tx.get(f.run.id))!;
    tx.update(r, {
      ...r.data,
      goal: "```\n# forged heading\n````",
      answer: {
        ...r.data.answer,
        rows: [
          {
            exact: "9223372036854775807",
            nested: [null, { flag: true }],
            note: "```\nHTML <script>synthetic</script>",
          },
        ],
        total: 1,
      },
    });
  });
  const response = await f.app.inject(f.url + "?format=markdown");
  expect(response.statusCode).toBe(200);
  expect(response.headers["content-type"]).toContain("text/markdown");
  expect(response.body).toContain(
    "`````text\n```\n# forged heading\n````\n`````",
  );
  expect(response.body).toContain('"9223372036854775807"');
  expect(response.body).toContain("report-snapshot");
  expect(response.body).not.toContain("PRIVATE-");
});

it("allows read-only SDK exports over real HTTP and preserves existing thread reads", async () => {
  const f = await fixture();
  const token = (
    await f.app.inject({
      method: "POST",
      url: f.base + "/tokens",
      payload: { name: "Report reader", readOnly: true },
    })
  ).json().token;
  expect(
    (
      await f.app.inject({
        method: "POST",
        url: f.base + "/assistant/runs",
        headers: { authorization: "Bearer " + token },
        payload: { goal: "Build a new definition", intent: "build" },
      })
    ).statusCode,
  ).toBe(403);
  const client = new FoundryClient({
    baseUrl: f.origin,
    workspaceId: f.scope,
    token,
  });
  expect(
    (await client.exportThread(f.run.id)).turns[0].answer?.rows[0].id,
  ).toBe("9007199254740993");
  const legacy = await f.app.inject(f.base + "/threads/saved-chat");
  expect(legacy.statusCode).toBe(200);
  expect(legacy.json().runs[0].data.answer.rows[0].id).toBe("9007199254740993");
});

it("refuses working threads, foreign-workspace chat IDs and unsupported formats", async () => {
  const f = await fixture();
  expect((await f.app.inject(f.url + "?format=html")).statusCode).toBe(400);
  expect(
    (await f.app.inject(f.base + "/threads/not-this-chat/export")).statusCode,
  ).toBe(404);
  const other = (
    await f.app.inject({
      method: "POST",
      url: "/api/v1/workspaces",
      payload: { name: "Other" },
    })
  ).json();
  expect(
    (
      await f.app.inject(
        `/api/v1/workspaces/${other.id}/threads/saved-chat/export`,
      )
    ).statusCode,
  ).toBe(404);
  await transaction(f.store, f.scope, async (tx) => {
    const r = (await tx.get(f.run.id))!;
    tx.update(r, { ...r.data, status: "running" });
  });
  expect((await f.app.inject(f.url)).statusCode).toBe(409);
});

it("exports unanswered terminal turns without stale answers and reports unknown legacy preview totals explicitly", async () => {
  const f = await fixture();
  await transaction(f.store, f.scope, async (tx) => {
    const r = (await tx.get(f.run.id))!;
    tx.update(r, { ...r.data, status: "failed" });
  });
  const failed = await f.app.inject(f.url);
  expect(failed.statusCode).toBe(200);
  expect(failed.json().turns[0]).not.toHaveProperty("answer");
  await transaction(f.store, f.scope, async (tx) => {
    const r = (await tx.get(f.run.id))!;
    tx.update(r, {
      ...r.data,
      status: "succeeded",
      answer: { ...r.data.answer, total: undefined },
    });
  });
  expect((await f.app.inject(f.url)).json().turns[0].answer).toMatchObject({
    total: null,
    includedRows: 1,
    truncated: null,
  });
});

it("preserves a complete ordered follow-up thread without exporting unrelated chats", async () => {
  const f = await fixture();
  const sql = "SELECT count(*) AS records FROM report_data";
  const result = await execute({
    mode: "query",
    scope: f.scope,
    inputs: [{ table: "report_data", version: "report-snapshot" }],
    sql,
  });
  await transaction(f.store, f.scope, async (tx) => {
    const later = resource(
      f.scope,
      "run",
      "Follow-up",
      {
        goal: "Count all saved rows",
        status: "succeeded",
        threadId: f.run.id,
        answer: {
          sql,
          rows: result.rows,
          total: result.profile.rows,
          citations: f.run.data.answer.citations,
        },
      },
      "follow-up",
    );
    tx.put({ ...later, createdAt: "2099-01-01T00:00:00.000Z" });
    tx.put(
      resource(
        f.scope,
        "run",
        "Unrelated",
        { goal: "UNRELATED-CHAT", status: "succeeded" },
        "other-chat",
      ),
    );
  });
  const response = await f.app.inject(f.url);
  expect(response.statusCode).toBe(200);
  expect(response.json().turns.map((t: any) => t.id)).toEqual([
    "saved-chat",
    "follow-up",
  ]);
  expect(response.json().turns[1].answer.rows).toEqual(result.rows);
  expect(response.json().turns[1].answer.truncated).toBe(false);
  expect(response.body).not.toContain("UNRELATED-CHAT");
});

it("keeps the established thread API available independently of exports", async () => {
  const f = await fixture();
  const response = await f.app.inject(f.base + "/threads/saved-chat");
  expect(response.statusCode).toBe(200);
  expect(response.json().runs[0].data.answer.rows).toEqual([
    { id: "9007199254740993", note: "before" },
  ]);
});
