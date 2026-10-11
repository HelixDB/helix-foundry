import { test, expect } from "@playwright/test";
import { datasetTable } from "../../packages/shared/src/index.js";
import { artifact } from "./support/ports";
import { openApp } from "./support/workspace";

// An online store's workspace gets the e-commerce playbook in the background:
// one quiet column on the Ontology page, one line on Home, and Settings ›
// Your business. Nothing asks the owner to choose.
test("industry playbook suggestions stay quiet and in existing pages", async ({
  page,
}) => {
  await openApp(page);
  const post = async (path: string, data: unknown) => {
    const response = await page.request.post("/api/v1" + path, { data });
    expect(response.ok(), await response.text()).toBeTruthy();
    return response.json();
  };
  const workspace = await post("/workspaces", {
    name: "Store " + Date.now(),
  });
  const root = "/workspaces/" + workspace.id;
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  const customers = await post(root + "/ingest", {
    name: "customers",
    rows: [
      { customer_id: 1, email: "a@x.test" },
      { customer_id: 2, email: "b@x.test" },
    ],
  });
  const orders = await post(root + "/ingest", {
    name: "orders",
    rows: [
      {
        order_id: 1,
        customer_id: 1,
        order_date: "2026-02-01",
        status: "completed",
        net_amount: 40,
      },
      {
        order_id: 2,
        customer_id: 2,
        order_date: "2026-02-02",
        status: "returned",
        net_amount: 25,
      },
      {
        order_id: 3,
        customer_id: null,
        order_date: "2026-02-03",
        status: "completed",
        net_amount: 30,
      },
    ],
  });
  await post(root + "/ingest", {
    name: "order_items",
    rows: [
      { order_item_id: 1, order_id: 1, product_id: 1, unit_price: 40 },
      { order_item_id: 2, order_id: 2, product_id: 1, unit_price: 25 },
    ],
  });
  await post(root + "/ingest", {
    name: "products",
    rows: [{ product_id: 1, product_name: "Mug", unit_cost: 12 }],
  });
  await post(root + "/ingest", {
    name: "sessions",
    rows: [
      { session_id: 1, customer_id: 1, traffic_source: "email", order_id: 1 },
      {
        session_id: 2,
        customer_id: 2,
        traffic_source: "social",
        order_id: null,
      },
    ],
  });
  const proposal = (
    await post(root + "/definitions/import", {
      version: 1,
      proposals: [
        {
          summary: "Orders and customers",
          assumptions: [],
          catalog: [],
          pipelines: [],
          ontology: [
            {
              name: "Customer",
              datasetId: customers.id,
              primaryKey: "customer_id",
              properties: ["customer_id", "email"],
            },
            {
              name: "Order",
              datasetId: orders.id,
              primaryKey: "order_id",
              properties: ["order_id", "customer_id", "status", "net_amount"],
            },
          ],
          relationships: [
            {
              name: "Placed by",
              fromType: "Order",
              toType: "Customer",
              fromColumn: "customer_id",
              toColumn: "customer_id",
            },
          ],
          dashboards: [
            {
              name: "Overview",
              widgets: [
                {
                  title: "Revenue",
                  type: "metric",
                  inputs: [orders.id],
                  sql: `SELECT sum(net_amount) revenue FROM ${datasetTable(orders.id)}`,
                },
              ],
            },
          ],
          actions: [],
        },
      ],
    })
  ).items[0];
  expect(proposal.data.validation.valid).toBe(true);
  await page.goto("/proposals?selected=" + proposal.id);
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await expect(page).toHaveURL("/", { timeout: 30000 });

  // Home: metrics as before, plus one quiet line.
  const pointer = page.getByRole("link", {
    name: /suggestions? to see your customers more clearly/,
  });
  await expect(pointer).toBeVisible({ timeout: 30000 });
  await page.screenshot({
    path: artifact("business-home.png"),
    fullPage: true,
  });

  // Ontology: no tabs, a third notes column, data checks under Worth checking.
  await pointer.click();
  await expect(page).toHaveURL(/\/ontology$/);
  const better = page.getByRole("region", {
    name: "To understand your customers better",
  });
  await expect(better).toBeVisible({ timeout: 30000 });
  await expect(better.getByRole("listitem")).toHaveCount(3);
  await expect(
    better.getByRole("link", { name: "Connect a source" }).first(),
  ).toHaveAttribute("href", "/data?add=1");
  const checking = page.getByRole("region", { name: "Worth checking" });
  await expect(checking).toContainText("Orders that link to a customer: 66.7%");
  await expect(checking).toContainText(
    "Returned orders are counted apart from sales.",
  );
  await expect(page.getByRole("tab")).toHaveCount(0);
  await better.scrollIntoViewIfNeeded();
  await page.screenshot({
    path: artifact("business-ontology.png"),
    fullPage: true,
  });

  // Settings › Your business: the inferred industry, typical ranges, and
  // knowledge the owner adds, which wins over them.
  await page.goto("/settings?tab=business");
  await expect(page.getByLabel("Industry")).toHaveValue("Online retail");
  await expect(page.getByText("Conversion is typically 1–3%")).toBeVisible();
  await page
    .getByRole("textbox", { name: "Add something Foundry should know" })
    .fill("Our target return rate is 8%");
  await page.getByRole("button", { name: "Add", exact: true }).click();
  const added = page.locator(".business-knowledge li", {
    hasText: "Our target return rate is 8%",
  });
  await expect(added).toContainText("You");
  await page.screenshot({
    path: artifact("business-settings.png"),
    fullPage: true,
  });
  await added.getByRole("button", { name: /Remove/ }).click();
  await expect(added).toHaveCount(0);

  // A phone gets the same column without sideways scrolling.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/ontology");
  await expect(better).toBeVisible({ timeout: 30000 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth + 1,
    ),
  ).toBe(true);
});
