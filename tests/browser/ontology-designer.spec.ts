import { expect, test } from "@playwright/test";
import { api, openApp } from "./support/workspace";

test("map business objects and links without AI, review and publish actual native records", async ({
  page,
}, testInfo) => {
  await openApp(page);
  await page.emulateMedia({ reducedMotion: "reduce" });
  const ws = await api(page, "/workspaces", {
    name: "Mapping acceptance " + Date.now(),
  });
  const base = `/workspaces/${ws.id}`;
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    ws.id,
  );
  const customers = await api(page, base + "/ingest", {
    name: "Mapping customers",
    rows: [
      { id: "A", name: "Acme" },
      { id: "B", name: "Beta" },
    ],
  });
  const orders = await api(page, base + "/ingest", {
    name: "Mapping orders",
    rows: [
      { id: "O1", customer: "A", amount: 20 },
      { id: "O2", customer: "A", amount: 30 },
      { id: "O3", customer: "B", amount: 40 },
    ],
  });
  expect((await api(page, base + "/resources/objectType?limit=1")).total).toBe(
    0,
  );
  await page.goto("/ontology");
  await expect(page).toHaveURL(/\/onboarding$/);
  await page.goto("/data");
  await page
    .getByRole("link", { name: "Map business objects", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Ontology designer", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("No publication yet", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Add object type", exact: true })
    .click();
  const customer = page.getByRole("group", {
    name: "Object type 1",
    exact: true,
  });
  await customer
    .getByRole("textbox", { name: "Object type name", exact: true })
    .fill("Customer");
  await customer
    .getByRole("combobox", { name: "Dataset", exact: true })
    .selectOption(customers.id);
  await customer
    .getByRole("combobox", { name: "Primary key", exact: true })
    .selectOption("id");
  await customer.getByRole("checkbox", { name: "name", exact: true }).check();
  await page
    .getByRole("button", { name: "Add object type", exact: true })
    .click();
  const order = page.getByRole("group", { name: "Object type 2", exact: true });
  await order
    .getByRole("textbox", { name: "Object type name", exact: true })
    .fill("Order");
  await order
    .getByRole("combobox", { name: "Dataset", exact: true })
    .selectOption(orders.id);
  await order
    .getByRole("combobox", { name: "Primary key", exact: true })
    .selectOption("id");
  await order.getByRole("checkbox", { name: "customer", exact: true }).check();
  await order.getByRole("checkbox", { name: "amount", exact: true }).check();
  await page
    .getByRole("button", { name: "Add relationship", exact: true })
    .click();
  const relationship = page.getByRole("group", {
    name: "Relationship 1",
    exact: true,
  });
  await relationship
    .getByRole("textbox", { name: "Relationship name", exact: true })
    .fill("Placed by");
  await relationship
    .getByRole("combobox", { name: "From object type", exact: true })
    .selectOption("Order");
  await relationship
    .getByRole("combobox", { name: "From column", exact: true })
    .selectOption("customer");
  await relationship
    .getByRole("combobox", { name: "To object type", exact: true })
    .selectOption("Customer");
  await relationship
    .getByRole("combobox", { name: "To column", exact: true })
    .selectOption("id");
  await page
    .getByRole("button", { name: "Create mapping review", exact: true })
    .click();
  await expect(page).toHaveURL(/\/proposals\?selected=/);
  await expect(
    page.getByRole("heading", { name: "Business records", exact: true }),
  ).toBeVisible();
  expect(
    (await api(page, base + "/ontology/definitions")).activeGeneration,
  ).toBeNull();
  const reviewUrl = page.url();
  expect((await api(page, base + "/resources/objectType?limit=1")).total).toBe(
    0,
  );
  await page.goto("/ontology");
  await expect(page).toHaveURL(/\/onboarding$/);
  await page.goto(reviewUrl);
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await expect
    .poll(
      async () =>
        (await api(page, base + "/ontology/definitions")).bundle.relationships,
    )
    .toEqual([
      {
        name: "Placed by",
        fromType: "Order",
        toType: "Customer",
        fromColumn: "customer",
        toColumn: "id",
      },
    ]);
  expect((await api(page, base + "/onboarding")).complete).toBe(false);
  expect((await api(page, base + "/resources/objectType?limit=1")).total).toBe(
    2,
  );
  for (const route of ["/", "/analyst"]) {
    await page.goto(route);
    await expect(page).toHaveURL(/\/onboarding$/);
  }
  await page.goto("/ontology");
  await page
    .getByRole("link", { name: "Design mappings", exact: true })
    .click();
  await expect(
    customer.getByRole("textbox", { name: "Object type name", exact: true }),
  ).toHaveValue("Customer");
  await expect(
    relationship.getByRole("textbox", {
      name: "Relationship name",
      exact: true,
    }),
  ).toHaveValue("Placed by");
  await page.screenshot({
    path: testInfo.outputPath("designer-desktop.png"),
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.emulateMedia({ reducedMotion: "reduce" });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("designer-mobile.png"),
    fullPage: true,
  });
  const objects = (await api(page, base + "/resources/object?limit=100")).items;
  const native = objects.find(
    (o: any) => o.data.type === "Customer" && o.data.effective.id === "A",
  );
  expect(native).toBeTruthy();
  await page.goto("/records/" + native.data.logicalId);
  await expect(
    page
      .locator(".related-records")
      .getByRole("link", { name: "Order O1 Order", exact: true }),
  ).toBeVisible();
  await expect(
    page
      .locator(".related-records")
      .getByRole("link", { name: "Order O2 Order", exact: true }),
  ).toBeVisible();
});

test("retain a mapping draft across reconnect and require an explicit accepted-definition reload", async ({
  page,
}, testInfo) => {
  await openApp(page);
  await page.emulateMedia({ reducedMotion: "reduce" });
  const ws = await api(page, "/workspaces", {
    name: "Concurrent mapping " + Date.now(),
  });
  const base = `/workspaces/${ws.id}`;
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    ws.id,
  );
  const ds = await api(page, base + "/ingest", {
    name: "Concurrent mapping rows",
    rows: [
      { id: "A", name: "Acme" },
      { id: "B", name: "Beta" },
    ],
  });
  const mapping = (name: string) => [
    {
      name,
      description: "",
      datasetId: ds.id,
      primaryKey: "id",
      properties: ["id", "name"],
    },
  ];
  const publish = async (expectedGeneration: string | null, name: string) => {
    const proposal = await api(page, base + "/ontology/draft", {
      expectedGeneration,
      ontology: mapping(name),
      relationships: [],
    });
    expect(proposal.data.validation.valid).toBe(true);
    await api(page, base + `/proposals/${proposal.id}/publish`, {
      hash: proposal.data.hash,
    });
    return (await api(page, base + "/ontology/definitions")).activeGeneration;
  };
  const originalGeneration = await publish(null, "Original customer");
  await page.goto("/ontology/designer");
  const name = page.getByRole("textbox", {
    name: "Object type name",
    exact: true,
  });
  await expect(name).toHaveValue("Original customer");
  await name.fill("Unsaved local customer");
  const nextGeneration = await publish(originalGeneration, "External customer");
  expect(nextGeneration).not.toBe(originalGeneration);
  await page.waitForTimeout(5100);
  await page.context().setOffline(true);
  await expect.poll(() => page.evaluate(() => navigator.onLine)).toBe(false);
  const backgroundResponse = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname ===
        `/api/v1${base}/ontology/definitions` &&
      response.request().method() === "GET",
    { timeout: 5000 },
  );
  await page.context().setOffline(false);
  await expect.poll(() => page.evaluate(() => navigator.onLine)).toBe(true);
  const observed = await backgroundResponse;
  expect((await observed.json()).activeGeneration).toBe(nextGeneration);
  await testInfo.attach("mapping-reconnect-observation", {
    body: JSON.stringify({
      originalGeneration,
      nextGeneration,
      actualReconnectGetObserved: true,
      visibleDraft: await name.inputValue(),
    }),
    contentType: "application/json",
  });
  await expect(name).toHaveValue("Unsaved local customer");
  const draftResponse = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === `/api/v1${base}/ontology/draft` &&
      response.request().method() === "POST",
  );
  await page
    .getByRole("button", { name: "Create mapping review", exact: true })
    .click();
  const rejected = await draftResponse;
  expect(rejected.status()).toBe(409);
  expect(rejected.request().postDataJSON().expectedGeneration).toBe(
    originalGeneration,
  );
  await expect(page.getByRole("alert")).toContainText(
    "Published definitions changed",
  );
  const accepted = await api(page, base + "/ontology/definitions");
  expect(accepted.activeGeneration).toBe(nextGeneration);
  expect(accepted.bundle.ontology).toEqual(mapping("External customer"));
  await page
    .getByRole("button", { name: "Reload accepted definitions", exact: true })
    .click();
  await expect(name).toHaveValue("External customer");
  await name.fill("Discard only on explicit reload");
  await page
    .getByRole("button", { name: "Reload accepted definitions", exact: true })
    .click();
  await expect(name).toHaveValue("External customer");
});

test("a finished published workspace keeps Home and Analyst reachable", async ({
  page,
}) => {
  await openApp(page);
  const ws = await api(page, "/workspaces", {
    name: "Completed mapping gate " + Date.now(),
  });
  const base = `/workspaces/${ws.id}`;
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    ws.id,
  );
  const ds = await api(page, base + "/ingest", {
    name: "Completed rows",
    rows: [{ id: "A", amount: 20 }],
  });
  const proposal = (
    await api(page, base + "/definitions/import", {
      version: 1,
      proposals: [
        {
          summary: "Completed mapping",
          assumptions: [],
          catalog: [],
          pipelines: [],
          ontology: [
            {
              name: "Order",
              datasetId: ds.id,
              primaryKey: "id",
              properties: ["id", "amount"],
            },
          ],
          relationships: [],
          dashboards: [
            {
              name: "Orders",
              widgets: [
                {
                  title: "Order count",
                  type: "metric",
                  inputs: [ds.id],
                  sql: `SELECT count(*) total FROM ${ds.data.table}`,
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
  await api(page, base + `/proposals/${proposal.id}/publish`, {
    hash: proposal.data.hash,
  });
  expect((await api(page, base + "/onboarding")).complete).toBe(true);
  for (const route of ["/", "/analyst"]) {
    await page.goto(route);
    await expect(page).not.toHaveURL(/\/onboarding$/);
    await expect(
      page.getByRole("navigation", { name: "Main navigation" }),
    ).toBeVisible();
  }
});
