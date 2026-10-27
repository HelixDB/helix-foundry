import type { Page } from "@playwright/test";
import { csrfToken, expect, mock, signIn, test } from "./support/mock";

// The Explorer's layout: a slim bar over the view; the graph's labels and
// Records' types in the section's panel beside the sidebar (the ontology and
// load meanwhile, and on an entity's page); the graph's overview and
// inspector as a right-hand sidebar the bar hides and shows; and, on a
// phone, the labels in the navigation drawer and the inspector a sheet.

const O = "/ontology/ont_acme_contracts",
  X = "/explore/ont_acme_contracts";

/**
 * The Orders ontology on the team's own database: no load, so its graph is
 * Other data (`?load=other`).
 */
async function addOrders(page: Page): Promise<string> {
  const res = await page.request.post(
    "/papi/v1/workspaces/ws_acme/foundry/ontologies",
    {
      data: {
        name: "Orders",
        tenantId: "ten_acme_orders",
        clientToken: "e2e-orders",
      },
      headers: {
        origin: new URL(page.url()).origin,
        "x-helix-csrf": await csrfToken(page),
      },
    },
  );
  expect(res.status()).toBe(200);
  return (await res.json()).ontology.id;
}

/** Makes version 3 current and loads it: Stripe's customers and invoices. */
async function loadContracts(page: Page) {
  await mock.configure({ loadStepMs: 100 });
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
  await page.goto(`${O}#loads`);
  await page
    .getByRole("main")
    .getByRole("button", { name: "Load data" })
    .first()
    .click();
  await page
    .getByRole("dialog", { name: "Load version 3 into contracts?" })
    .getByRole("button", { name: "Load" })
    .click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 20_000 });
}

const graph = (page: Page) =>
  page.getByRole("application", { name: /^Graph of orders: \d+ nodes/ });

test("puts the labels beside the sidebar and the overview down the right", async ({
  page,
}) => {
  await signIn(page);
  const orders = await addOrders(page);
  await page.goto(`/explore/${orders}?load=other`);
  await expect(graph(page)).toBeVisible({ timeout: 15_000 });

  // The slim bar: the view, the pickers and the explorer's tools.
  await expect(
    page.getByRole("heading", { level: 1, name: "Explorer" }),
  ).toBeAttached();
  await expect(page.getByRole("radiogroup", { name: "View" })).toBeVisible();
  await expect(page.getByRole("combobox", { name: "Ontology" })).toHaveText(
    "Orders",
  );
  await expect(
    page.getByRole("combobox", { name: "Jump to a node" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Reload from the database" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Keyboard shortcuts" }),
  ).toBeVisible();

  // The labels are the section's panel, beside the labelled sidebar.
  const nav = page.getByRole("navigation", { name: "Main navigation" });
  await expect(nav.locator(".hf-nav-link")).toHaveCount(4);
  const labels = page.getByRole("complementary", { name: "Labels" });
  await expect(labels).toContainText("Account");
  await expect(labels).toContainText("PLACED");
  await expect(
    labels.getByRole("checkbox", { name: "Show Foundry internals" }),
  ).toBeVisible();
  const sidebar = (await page.locator(".hf-sidebar").boundingBox())!;
  expect(Math.round((await labels.boundingBox())!.x)).toBe(
    Math.round(sidebar.x + sidebar.width),
  );

  // The overview runs down the right edge, under the bar.
  const inspector = page.getByRole("complementary", { name: "Inspector" });
  await expect(inspector).toContainText("Overview");
  await expect(inspector).toContainText("Read only");
  const side = (await inspector.boundingBox())!;
  expect(Math.round(side.x + side.width)).toBe(1440);
  expect(Math.round(side.y + side.height)).toBe(900);
  // A label picked beside the sidebar shows there.
  await labels.getByRole("button", { name: /^Order node label/ }).click();
  await expect(inspector).toContainText("Node label");

  // The bar hides the overview, and that is remembered.
  await page.getByRole("button", { name: "Hide inspector" }).click();
  await expect(inspector).toHaveCount(0);
  await page.reload();
  await expect(graph(page)).toBeVisible({ timeout: 15_000 });
  const show = page.getByRole("button", { name: "Show inspector" });
  await expect(show).toHaveAttribute("aria-expanded", "false");
  await show.click();
  await expect(inspector).toBeVisible();

  // [ hides the labels' panel for the visit, and shows it again.
  await page.keyboard.press("[");
  await expect(labels).toHaveCount(0);
  await page.keyboard.press("[");
  await expect(labels).toContainText("Account");
});

test("puts Records' types beside the sidebar, and the load meanwhile", async ({
  page,
}) => {
  await signIn(page);
  await loadContracts(page);
  await page.goto(`${X}/records`);
  const types = page
    .getByRole("complementary", { name: "Types" })
    .getByRole("list", { name: "Types" });
  await expect(types.getByRole("button")).toHaveText([
    /^Contract/,
    "Customer64",
    "Invoice65",
  ]);
  await types.getByRole("button", { name: /^Invoice/ }).click();
  await expect(page).toHaveURL(/type=Invoice/);

  // Where the panel isn't on screen (narrow, it is in the drawer), they sit
  // above the table.
  await page.setViewportSize({ width: 700, height: 900 });
  const inline = page.getByRole("main").getByRole("list", { name: "Types" });
  await expect(inline.getByRole("button")).toHaveCount(3);
  await inline.getByRole("button", { name: /^Customer/ }).click();
  await expect(page).toHaveURL(/type=Customer/);
  await page.setViewportSize({ width: 1440, height: 900 });
  await expect(inline).toHaveCount(0);

  // An entity's page has nothing for the panel: it names the ontology and
  // the load, and opens the views.
  await page.goto(
    `${X}/entities/${Buffer.from("Customer\0k:cus_GLOBEX01").toString("base64url")}`,
  );
  await expect(
    page.getByRole("heading", { level: 1, name: "Globex Corporation" }),
  ).toBeVisible({ timeout: 15_000 });
  const context = page.getByRole("complementary", { name: "Explorer panel" });
  await expect(context).toContainText("Customer contracts");
  await expect(context).toContainText("contracts");
  await expect(context.getByRole("region", { name: "Load" })).toContainText(
    "Current · v3",
  );
  await context.getByRole("link", { name: "Records" }).click();
  await expect(page).toHaveURL(new RegExp(`${X}/records$`));
  await expect(
    page.getByRole("complementary", { name: "Types" }),
  ).toBeVisible();
});

test("on a phone, keeps the labels in the drawer and the inspector in a sheet", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await signIn(page);
  const orders = await addOrders(page);
  await page.goto(`/explore/${orders}?load=other`);
  await expect(graph(page)).toBeVisible({ timeout: 15_000 });
  await expect(page.getByRole("complementary", { name: "Labels" })).toHaveCount(
    0,
  );
  const tabs = page.getByRole("navigation", { name: "Explorer panels" });
  await tabs.getByRole("button", { name: "Labels" }).click();
  const drawer = page.getByRole("dialog", { name: "Navigation" });
  const labels = drawer.getByRole("complementary", { name: "Labels" });
  await expect(labels).toContainText("Account");
  // Picking a label closes the drawer and shows it in the inspector.
  await labels.getByRole("button", { name: /^Order node label/ }).click();
  await expect(drawer).toHaveCount(0);
  await expect(
    page.getByRole("complementary", { name: "Inspector" }),
  ).toContainText("Node label");
});
