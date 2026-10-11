import { test, expect } from "@playwright/test";
import { randomBytes } from "node:crypto";
import { writeFile, mkdir } from "node:fs/promises";

import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { datasetTable } from "../../packages/shared/src/index.js";
import { artifact } from "./support/ports";
import { openApp } from "./support/workspace";

// An isolated workspace keeps acceptance-test edits out of the user's records.
test("connect, review, publish, investigate, and edit business records", async ({
  page,
}) => {
  await openApp(page);
  const post = async (path: string, data: unknown) => {
    const response = await page.request.post("/api/v1" + path, { data });
    expect(response.ok(), await response.text()).toBeTruthy();
    return response.json();
  };
  const workspace = await post("/workspaces", {
    name: "UI acceptance " + Date.now(),
  });
  const scope = workspace.id,
    root = "/workspaces/" + scope;

  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    scope,
  );
  await page.goto("/");
  await expect(
    page.getByRole("heading", { name: "A little help with your data" }),
  ).toBeVisible();
  const customers = await post(root + "/ingest", {
    name: "Customers",
    rows: [
      { id: "A", name: "Acme", region: "EU" },
      { id: "B", name: "Beta", region: "NA" },
    ],
  });
  const orders = await post(root + "/ingest", {
    name: "Orders",
    rows: [
      { id: 1, customer: "A", amount: 10, status: "delayed" },
      { id: 2, customer: "A", amount: 20, status: "delivered" },
      { id: 3, customer: "B", amount: 5, status: "delivered" },
    ],
  });
  await page.goto("/");
  await expect(
    page.getByRole("heading", { name: "Understanding your data" }),
  ).toBeVisible();
  await expect(page).toHaveURL(/\/onboarding$/);
  await expect(
    page.getByRole("heading", { name: "Here’s how your data fits together" }),
  ).toBeVisible({ timeout: 30000 });
  await expect(
    page.getByRole("checkbox", { name: "Include Customer" }),
  ).toBeVisible();
  const revenue = `SELECT sum(amount) revenue FROM ${datasetTable(orders.id)}`;
  const proposal = (
    await post(root + "/definitions/import", {
      version: 1,
      proposals: [
        {
          summary: "Revenue and delayed orders",
          assumptions: ["Revenue includes delayed orders."],
          catalog: [],
          pipelines: [
            {
              name: "Customer revenue",
              inputs: [customers.id, orders.id],
              sql: `SELECT c.id, sum(o.amount) revenue FROM ${datasetTable(customers.id)} c JOIN ${datasetTable(orders.id)} o ON c.id=o.customer GROUP BY c.id`,
              nodes: [],
              checks: [],
            },
          ],
          ontology: [
            {
              name: "Customer",
              datasetId: customers.id,
              primaryKey: "id",
              properties: ["id", "name", "region"],
            },
            {
              name: "Order",
              datasetId: orders.id,
              primaryKey: "id",
              properties: ["id", "customer", "amount", "status"],
            },
          ],
          relationships: [
            {
              name: "Placed by",
              fromType: "Order",
              toType: "Customer",
              fromColumn: "customer",
              toColumn: "id",
            },
          ],
          dashboards: [
            {
              name: "Business view",
              widgets: [
                {
                  title: "Revenue",
                  type: "metric",
                  inputs: [orders.id],
                  sql: revenue,
                },
                {
                  title: "Delayed orders",
                  type: "metric",
                  inputs: [orders.id],
                  sql: `SELECT count(*) delayed FROM ${datasetTable(orders.id)} WHERE status='delayed'`,
                },
              ],
            },
            {
              name: "Revenue detail",
              widgets: [
                {
                  title: "Revenue",
                  type: "metric",
                  inputs: [orders.id],
                  sql: revenue,
                },
              ],
            },
          ],
          actions: [
            { name: "Update order", objectType: "Order", fields: ["status"] },
          ],
        },
      ],
    })
  ).items[0];
  expect(proposal.data.validation.valid).toBe(true);
  await page.goto("/proposals?selected=" + proposal.id);
  await expect(
    page.getByRole("heading", { name: "Review changes", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("Revenue includes delayed orders.", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Business records" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await expect(page).toHaveURL("/", { timeout: 30000 });
  // Publishing lands on the workspace home.
  await expect(
    page.getByRole("textbox", { name: "Ask anything about your data" }),
  ).toBeVisible();
  const health = page.getByRole("region", { name: "Workspace health" });
  await expect(health.getByRole("link", { name: /Connections/ })).toBeVisible();
  await expect(
    page.locator(".wk-types").getByRole("link", { name: /^Customer/ }),
  ).toBeVisible();
  // Home's key metrics replace dashboards. Their values are kept to check
  // that a workspace edit leaves them alone.
  const metrics = async () => {
    const response = await page.request.get("/api/v1" + root + "/home/metrics");
    expect(response.ok(), await response.text()).toBeTruthy();
    const body = await response.json();
    return body.metrics.map((m: any) => ({ id: m.id, result: m.result }));
  };
  const publishedMetrics = await metrics();
  expect(publishedMetrics.length).toBeGreaterThan(0);
  await expect(page.getByRole("region", { name: "Key metrics" })).toBeVisible();
  const nav = page.getByRole("navigation", { name: "Main navigation" });
  await expect(
    nav.getByRole("link", { name: "Dashboards", exact: true }),
  ).toHaveCount(0);
  await page.screenshot({
    path: artifact("redesign-desktop.png"),
    fullPage: true,
  });
  // Asking from Home starts a chat: the page becomes the chat, showing the
  // question, with no side panel. The chat heads the history under Analyst.
  const askBox = page.getByRole("textbox", {
    name: "Ask anything about your data",
  });
  const question = "Which customers have the most orders?";
  await askBox.fill(question);
  await askBox.press("Enter");
  await expect(page).toHaveURL(/\/analyst\/.+/);
  await expect(page.getByRole("log").getByText(question).first()).toBeVisible();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  const history = nav.getByRole("list", { name: "Chats" });
  await expect(history.getByRole("link", { name: question })).toHaveAttribute(
    "aria-current",
    "page",
  );
  // The history is there on every page, not only in the Analyst section.
  await nav.getByRole("link", { name: "Home", exact: true }).click();
  await expect(askBox).toBeVisible();
  await history.getByRole("link", { name: question }).click();
  await expect(page).toHaveURL(/\/analyst\/.+/);
  await expect(page.getByRole("log").getByText(question).first()).toBeVisible();
  // The answer arrives with its steps, all reported by the server: the
  // result checked in code as a ranking, and its statement first.
  const steps = page.getByRole("region", { name: "Analysis steps" });
  await expect(steps.getByText("Analysis complete")).toBeVisible({
    timeout: 30000,
  });
  await expect(
    page.getByRole("heading", { name: "Customer A has the most orders." }),
  ).toBeVisible();
  await expect(page.locator(".an-lead strong")).toHaveText([
    "2 orders",
    "1 more",
  ]);
  await steps.getByRole("button", { name: "Show steps" }).click();
  await expect(steps.locator(".an-steps > li")).toContainText([
    "Understand the question: Planned “Customers by order count”",
    "Analyze the data: Ran a read-only query on Orders.",
    "Check the results: 2 rows: order count by customer.",
    "Write the answer: Summarized the result in plain words.",
  ]);
  await nav.getByRole("link", { name: "Ontology", exact: true }).click();
  await nav.getByRole("link", { name: "Explorer", exact: true }).click();
  // The Explorer opens on the record graph; finding a record selects it.
  await expect(
    page.getByRole("img", { name: /^Graph of \d+ records and \d+ links/ }),
  ).toBeVisible();
  await page.getByLabel("Find a record").fill("Order 1");
  await page.getByLabel("Find a record").press("Enter");
  const selected = page.getByRole("complementary", { name: "Order 1" });
  await expect(
    selected.getByRole("button", { name: "Open record" }),
  ).toBeVisible();
  await expect(selected.getByText(/Customer/).first()).toBeVisible();
  await page.getByRole("radio", { name: "Table" }).click();
  await page
    .getByRole("list", { name: "Object types" })
    .getByRole("button", { name: /^Order\b/ })
    .click();
  await page.getByRole("row", { name: /^Order 1\b/ }).click();
  const detail = page.getByRole("dialog", { name: "Order 1", exact: true });
  await expect(detail.getByRole("link", { name: /Customer A/ })).toBeVisible();
  const detailUrl = page.url();
  await page.reload();
  await expect(
    detail.getByRole("heading", { name: "Order 1", exact: true }),
  ).toBeVisible();
  await detail
    .getByRole("button", { name: "Edit record", exact: true })
    .click();
  await detail.getByLabel("Status", { exact: true }).fill("processing");
  await expect(
    detail.getByRole("button", { name: "Apply changes", exact: true }),
  ).toHaveCount(0);
  await detail
    .getByRole("button", { name: "Preview changes", exact: true })
    .click();
  await expect(
    detail.getByRole("cell", { name: "delayed", exact: true }),
  ).toBeVisible();
  await expect(
    detail.getByRole("cell", { name: "processing", exact: true }),
  ).toBeVisible();
  await detail
    .getByRole("button", { name: "Apply changes", exact: true })
    .click();
  await expect(
    detail.getByText("Workspace edit", { exact: true }),
  ).toBeVisible();
  await detail.getByText("Source and history", { exact: true }).click();
  await expect(
    detail.getByRole("cell", { name: "delayed", exact: true }),
  ).toBeVisible();
  await expect(detail.getByText(/Updated Order 1/)).toBeVisible();
  await detail.getByRole("link", { name: /Customer A/ }).click();
  await expect(
    page.getByRole("dialog", { name: "Customer A", exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  await nav.getByRole("link", { name: "Data", exact: true }).click();
  await page.getByRole("link", { name: /Orders/ }).click();
  await expect(
    page.getByRole("heading", { name: "Orders", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "delayed", exact: true }),
  ).toBeVisible();
  await expect(page.getByText("Details", { exact: true })).toBeVisible();
  // The old Advanced hub, SQL and Pipelines pages are gone; links land on Data.
  await expect(nav.getByRole("link", { name: "SQL", exact: true })).toHaveCount(
    0,
  );
  await page.goto("/advanced");
  await expect(page).toHaveURL(/\/data$/);
  await page.goto("/data/connections?add=1");
  const picker = page.getByRole("dialog", {
    name: "Where is your data?",
    exact: true,
  });
  for (const source of [
    "Upload files",
    "PostgreSQL",
    "MySQL",
    "REST API",
    "Blob storage",
    "Ingestion API",
  ])
    await expect(
      picker.getByRole("button", { name: source, exact: true }),
    ).toBeVisible();
  await picker.getByRole("button", { name: "MySQL", exact: true }).click();
  await expect(page.getByRole("dialog")).toHaveCount(1);
  await expect(
    page.getByRole("dialog", { name: "MySQL", exact: true }),
  ).toBeVisible();
  await expect(page.getByLabel("Host", { exact: true })).toBeVisible();
  await expect(
    page.getByLabel("Use TLS with certificate verification"),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  await page.goto("/sources?add=1");
  await page.getByRole("button", { name: "PostgreSQL", exact: true }).click();
  await expect(
    page.getByLabel("Connection URL", { exact: true }),
  ).toHaveAttribute("type", "password");
  await expect(
    page.getByText("Paste the connection URL from your database provider."),
  ).toHaveCount(0);
  await expect(page.getByLabel("Cron expression (UTC)")).toHaveCount(0);
  await expect(page.getByLabel("Host", { exact: true })).toHaveCount(0);
  await page
    .getByRole("button", { name: "Enter details individually" })
    .click();
  await expect(page.getByLabel("Host", { exact: true })).toBeVisible();
  await page.keyboard.press("Escape");

  // A workspace edit is an overlay, not a new data version: Home's metrics,
  // computed from the imported snapshots, must not silently change.
  expect(await metrics()).toEqual(publishedMetrics);
  // Old dashboard links open Home.
  await page.goto("/dashboards/Business%20view");
  await expect(page).toHaveURL("/");
  await expect(askBox).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    page.getByRole("button", { name: "Open navigation" }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  await page.screenshot({
    path: artifact("redesign-mobile.png"),
    fullPage: true,
  });
  await page.getByRole("button", { name: "Open navigation" }).click();
  const drawer = page.getByRole("dialog", { name: "Navigation" });
  await expect(
    drawer.getByRole("link", { name: "Data", exact: true }),
  ).toBeVisible();
  await expect(
    drawer.getByRole("link", { name: "Dashboards", exact: true }),
  ).toHaveCount(0);
  await drawer.getByRole("link", { name: "Data", exact: true }).click();
  await expect(page).toHaveURL(/\/data$/);
  await expect(drawer).toHaveCount(0);
  await page.getByRole("button", { name: "Open navigation" }).click();
  await drawer.getByRole("link", { name: "Ontology", exact: true }).click();
  await expect(page).toHaveURL(/\/ontology\/explore$/);
  await expect(drawer).toHaveCount(0);
  await page.goto(detailUrl);
  await expect(detail).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  await page.screenshot({
    path: artifact("redesign-mobile-record.png"),
    fullPage: true,
  });
  await writeFile(
    artifact("redesign-test-workspace.json"),
    JSON.stringify({ scope, name: workspace.name }),
  );
});
