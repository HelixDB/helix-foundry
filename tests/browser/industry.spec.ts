import { test, expect } from "@playwright/test";
import { retailData } from "../fixtures/retail";
import { api, openApp, sampleWorkspace } from "./support/workspace";
import { artifact } from "./support/ports";

test("retail metrics, persisted checks, refresh and source navigation", async ({
  page,
}) => {
  await openApp(page);
  const workspace = await api(page, "/workspaces", {
    name: "Retail validation",
  });
  const base = `/workspaces/${workspace.id}`;
  const data = retailData();
  for (const [name, rows] of Object.entries(data))
    await api(page, base + "/ingest", { name, rows });
  const state = await api(page, base + "/onboarding/ontology", {});
  await api(page, base + "/onboarding/ontology/confirm", {
    hash: state.ontology.hash,
  });
  await expect
    .poll(async () => (await api(page, base + "/onboarding")).complete, {
      timeout: 60000,
    })
    .toBe(true);
  await expect
    .poll(async () => (await api(page, base + "/industry")).checks.length, {
      timeout: 30000,
    })
    .toBe(6);
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  await page.goto("/");
  const metrics = page.getByRole("region", { name: "Key metrics" });
  await expect(metrics).toBeVisible();
  await expect(
    metrics.locator(".wk-metrics-kpis .hf-stat-label").first(),
  ).toHaveText("Recorded order value");
  const home = await api(page, base + "/home/metrics");
  expect(
    home.metrics
      .filter((m: any) => m.kind === "kpi")
      .slice(0, 4)
      .map((m: any) => m.title),
  ).toEqual([
    "Recorded order value",
    "Orders",
    "Average order value",
    "Purchasing customers",
  ]);
  await expect(
    metrics.getByText("Recorded order value", { exact: true }),
  ).toBeVisible();
  await page.goto("/ontology");
  const notes = page.getByRole("region", {
    name: "To understand your customers better",
  });
  await expect(notes).toBeVisible();
  await expect(notes.getByRole("status")).toContainText("Checked");
  const disclosure = notes.locator("summary", { hasText: "What we checked" });
  await disclosure.focus();
  await page.keyboard.press("Enter");
  await expect(
    notes.getByText(/Orders linked to sessions: 10.0%/),
  ).toBeVisible();
  await expect(
    notes.getByText(/Home's recorded totals include these rows/),
  ).toBeVisible();
  await expect(notes.locator(".so-industry-checks li")).toHaveCount(6);
  const before = (await api(page, base + "/industry")).checkedAt;
  await notes.getByRole("button", { name: "Check again", exact: true }).click();
  await expect
    .poll(async () => (await api(page, base + "/industry")).checkedAt)
    .not.toBe(before);
  await page.screenshot({
    path: artifact("retail-desktop.png"),
    fullPage: true,
  });
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  await page.evaluate(() => localStorage.setItem("foundry.theme", "dark"));
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(notes).toBeVisible();
  await notes.locator("summary").click();
  await expect(
    notes.getByText(/Orders linked to sessions: 10.0%/),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  await notes.screenshot({
    path: artifact("retail-mobile-dark.png"),
  });
  await notes.getByRole("link", { name: "Connect a source" }).first().click();
  await expect(
    page.getByRole("dialog", { name: "Where is your data?", exact: true }),
  ).toBeVisible();
});

test("a generic workspace keeps its existing ontology and metrics", async ({
  page,
}) => {
  await openApp(page);
  const workspace = await sampleWorkspace(page, "Generic validation");
  expect((await api(page, workspace.base + "/industry")).scan).toBe(
    "unsupported",
  );
  await page.goto("/ontology");
  await expect(
    page.getByRole("region", { name: "To understand your customers better" }),
  ).toHaveCount(0);
  await page.goto("/");
  await expect(page.getByRole("region", { name: "Key metrics" })).toBeVisible();
});
