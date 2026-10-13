import { beforeAll, afterAll, afterEach, expect, it } from "vitest";
import { createServer } from "node:http";
import { once } from "node:events";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { createElement as h } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { config } from "../apps/api/src/config.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore } from "../apps/api/src/store.js";
import { datasetTable } from "../packages/shared/src/index.js";
import {
  chartPlan,
  AnswerChart,
} from "../apps/web/src/areas/analyst/answer-view.js";

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
let app: Awaited<ReturnType<typeof createApp>> | undefined, scope: string;
afterEach(async () => {
  await app?.close();
  for (const kind of ["incoming", "datasets", "results"])
    await rm(resolve(config.dataDir, kind, scope), {
      recursive: true,
      force: true,
    });
});
async function render(values: number[]) {
  app = await createApp(new MemoryStore());
  await app.ready();
  scope = (await app.inject("/api/v1/me")).json().workspaces[0].id;
  const dataset = (
    await app.inject({
      method: "POST",
      url: `/api/v1/workspaces/${scope}/ingest`,
      payload: {
        name: "Signed profits",
        rows: values.map((profit, i) => ({
          category: `Category ${i}`,
          profit,
        })),
      },
    })
  ).json();
  const response = await app.inject({
    method: "POST",
    url: `/api/v1/workspaces/${scope}/query`,
    payload: {
      inputs: [dataset.id],
      sql: `SELECT category, sum(profit) profit FROM ${datasetTable(dataset.id)} GROUP BY category ORDER BY category`,
    },
  });
  expect(response.statusCode, response.body).toBe(200);
  const rows = response.json().rows;
  expect(rows.map((r: any) => Number(r.profit))).toEqual(values);
  const plan = chartPlan(rows)!;
  expect(plan.kind).toBe("breakdown");
  const html = renderToStaticMarkup(h(AnswerChart, { plan }));
  const bars = [...html.matchAll(/class="wk-bar" style="([^"]+)"/g)].map((m) =>
    Object.fromEntries(m[1].split(";").map((s) => s.split(":"))),
  );
  expect(bars).toHaveLength(values.length);
  return { html, bars };
}
it("draws actual mixed-sign query results on either side of a shared zero", async () => {
  const { html, bars } = await render([-50, 100, 0]);
  expect(parseFloat(bars[0].width)).toBeCloseTo(100 / 3);
  expect(parseFloat(bars[0].left)).toBe(0);
  expect(parseFloat(bars[1].left)).toBeCloseTo(100 / 3);
  expect(parseFloat(bars[1].width)).toBeCloseTo(200 / 3);
  expect(parseFloat(bars[2].width)).toBe(0);
  expect(html.match(/class="wk-bar-zero"/g)).toHaveLength(3);
  expect(html).toContain("Category 0 -50");
});
it("draws all-negative results with zero at the right edge", async () => {
  const { bars } = await render([-80, -20]);
  expect(parseFloat(bars[0].width)).toBe(100);
  expect(parseFloat(bars[0].left)).toBe(0);
  expect(parseFloat(bars[1].width)).toBe(25);
  expect(parseFloat(bars[1].left)).toBe(75);
});
it("keeps existing positive proportions and exact labels", async () => {
  const { html, bars } = await render([100, 25]);
  expect(bars.map((b) => parseFloat(b.width))).toEqual([100, 25]);
  expect(html).toContain("Category 0 100");
  expect(html).toContain("Category 1 25");
});
it("keeps all-zero results finite and without phantom bars", async () => {
  const { html, bars } = await render([0, 0]);
  expect(bars.map((b) => parseFloat(b.width))).toEqual([0, 0]);
  expect(html).not.toMatch(/NaN|Infinity/);
});

it("keeps opposite large finite totals on a finite proportional axis", async () => {
  const { html, bars } = await render([-1e308, 1e308]);
  expect(bars.map((b) => parseFloat(b.width))).toEqual([50, 50]);
  expect(bars.map((b) => parseFloat(b.left))).toEqual([0, 50]);
  expect(html).not.toMatch(/NaN|Infinity/);
});
