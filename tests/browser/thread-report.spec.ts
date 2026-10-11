import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { openApp, api } from "./support/workspace";
import { datasetTable } from "../../packages/shared/src/index.js";

test("downloads saved Analyst evidence through the real chat, API, worker and DuckDB", async ({
  page,
}) => {
  await openApp(page);
  const workspace = await api(page, "/workspaces", {
    name: "Evidence report acceptance",
  });
  workspace.base = `/workspaces/${workspace.id}`;
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  await api(page, workspace.base + "/onboarding/provider", {
    provider: "local",
    model: "qwen3:4b",
  });
  const dataset = await api(page, workspace.base + "/ingest", {
    name: "Evidence report Accounts",
    rows: [{ id: "A" }, { id: "B" }],
  });
  const proposal = (
    await api(page, workspace.base + "/definitions/import", {
      version: 1,
      proposals: [
        {
          summary: "Evidence report workspace",
          assumptions: [],
          catalog: [],
          pipelines: [],
          ontology: [],
          relationships: [],
          actions: [],
          dashboards: [
            {
              name: "Accounts",
              widgets: [
                {
                  title: "Accounts",
                  type: "metric",
                  inputs: [dataset.id],
                  sql: `SELECT count(*) accounts FROM ${datasetTable(dataset.id)}`,
                },
              ],
            },
          ],
        },
      ],
    })
  ).items[0];
  expect(proposal.data.validation.valid).toBe(true);
  await api(page, workspace.base + `/proposals/${proposal.id}/publish`, {
    hash: proposal.data.hash,
  });
  const question = "How many accounts are in our saved data?";
  const run = await api(page, workspace.base + "/assistant/runs", {
    goal: question,
    intent: "answer",
  });
  await expect
    .poll(
      async () =>
        (await api(page, workspace.base + `/resources/run/${run.id}`)).data
          .status,
      { timeout: 60000, intervals: [500] },
    )
    .toBe("succeeded");
  const saved = await api(page, workspace.base + `/threads/${run.id}`);
  expect(saved.runs[0].data.answer.citations.length).toBeGreaterThan(0);
  await page.goto(`/analyst/${run.id}`);
  await expect(page.getByRole("heading", { name: question })).toBeVisible();
  await page.getByRole("button", { name: "Chat options" }).click();
  const jsonWait = page.waitForEvent("download");
  await page.getByRole("menuitem", { name: "Download evidence JSON" }).click();
  const json = await jsonWait;
  expect(json.suggestedFilename()).toBe("analyst-report.json");
  const report = JSON.parse(await readFile((await json.path())!, "utf8"));
  expect(report.turns[0].answer.rows).toEqual(saved.runs[0].data.answer.rows);
  expect(report.turns[0].answer.sql).toBe(saved.runs[0].data.answer.sql);
  expect(report.turns[0].answer.citations).toEqual(
    saved.runs[0].data.answer.citations,
  );
  expect(report.turns[0].question).toBe(question);
  await page.getByRole("button", { name: "Chat options" }).click();
  const mdWait = page.waitForEvent("download");
  await page
    .getByRole("menuitem", { name: "Download evidence report" })
    .click();
  const md = await mdWait;
  expect(md.suggestedFilename()).toBe("analyst-report.md");
  const text = await readFile((await md.path())!, "utf8");
  expect(text).toContain(question);
  expect(text).toContain(report.turns[0].answer.sql);
  expect(text).toContain(report.turns[0].answer.citations[0].version);
  expect(text).toContain('"includedRows"');
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("button", { name: "Chat options" }).click();
  await expect(
    page.getByRole("menuitem", { name: "Download evidence report" }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
});
