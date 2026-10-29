import type { Page } from "@playwright/test";
import {
  createFirstOntology,
  csrfToken,
  dataTree as tree,
  expect,
  mock,
  signIn,
  signInToNewWorkspace,
  startOntology,
  test,
} from "./support/mock";

// The Data area: connecting each kind of source with its credential form,
// syncs and their status, synced tables with their keys, and the WFE's
// refusals (bad credentials, Foundry Pro, a sync already running). A
// credential typed into a form never comes back from the BFF.

// A sync runs about 2.5 s in the mock, and the page polls every 1.5 s.
const SYNCED = { timeout: 15_000 };

const dialog = (page: Page) => page.locator("dialog[open]");
const header = (page: Page) => page.locator(".hf-page-header");
// A connection's row in the Data explorer, by its name: the row's name is
// the connection's and its sync's status.
const item = (page: Page, name: string) =>
  tree(page).getByRole("treeitem", { name: `${name}, ` });

// Every /papi response body the page received, to search for credentials.
function recordResponses(page: Page) {
  const bodies: string[] = [];
  page.on("response", async (r) => {
    if (new URL(r.url()).pathname.startsWith("/papi/"))
      bodies.push(await r.text().catch(() => ""));
  });
  return bodies;
}

async function openPicker(page: Page) {
  await page.goto("/data");
  await page.getByRole("button", { name: "Add data" }).first().click();
  await expect(dialog(page)).toHaveAttribute(
    "aria-label",
    "Where is your data?",
  );
}

/**
 * A new workspace's picker. With no ontology it has no Data pages: it
 * connects from its onboarding wizard's sources step, which `?step=sources`
 * opens once the ontology is named (startOntology) and its database chosen.
 */
async function openOnboardingPicker(page: Page) {
  await page.goto("/ontology/new?step=sources");
  await page.getByRole("button", { name: "Add data" }).first().click();
  await expect(dialog(page)).toHaveAttribute(
    "aria-label",
    "Where is your data?",
  );
}

test("connects Stripe, syncs it and opens its tables and keys", async ({
  page,
}) => {
  const key = "rk_test_51E2eRestrictedKeyZq8w";
  const bodies = recordResponses(page);
  await signIn(page);
  await openPicker(page);
  await dialog(page)
    .getByRole("button", { name: "Stripe", exact: true })
    .click();
  // The numbered steps end "paste it below": they sit above the key field.
  const guide = dialog(page).locator(".connection-guide");
  await expect(guide).toContainText("paste it below");
  const keyField = dialog(page).getByLabel("Restricted API key");
  expect((await guide.boundingBox())!.y).toBeLessThan(
    (await keyField.boundingBox())!.y,
  );
  expect(
    await guide
      .locator("ol")
      .evaluate((el) => getComputedStyle(el).listStyleType),
  ).toBe("decimal");
  await dialog(page).getByLabel("Restricted API key").fill(key);
  await dialog(page).getByRole("button", { name: "Test connection" }).click();
  await expect(
    dialog(page).getByText("Stripe objects this key can read"),
  ).toBeVisible();
  await expect(dialog(page)).toContainText(
    "This key cannot read: disputes, payouts, balance_transactions.",
  );
  await dialog(page).getByRole("button", { name: "Connect and sync" }).click();

  await expect(page).toHaveURL(/\/data\/connections\/conn_/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Stripe (test)" }),
  ).toBeVisible();
  await expect(header(page)).toContainText("rk_test…Zq8w");
  // Queued, then syncing, then synced: the page follows it by polling.
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  const tables = page.getByRole("list", { name: "Synced tables" });
  await expect(tables.getByRole("listitem")).toHaveCount(12);
  await expect(
    page.getByRole("grid", { name: "Recent syncs" }).getByRole("row"),
  ).toHaveCount(2);

  await tables
    .getByRole("link", { name: /invoices/ })
    .first()
    .click();
  await expect(
    page.getByRole("heading", { level: 1, name: "invoices" }),
  ).toBeVisible();
  const columns = page.getByRole("grid", { name: "Columns" });
  await expect(
    columns.getByRole("row", { name: /invoice_id/ }).first(),
  ).toContainText("Primary");
  await expect(
    page.getByText("is an API source: its columns are the fields found"),
  ).toBeVisible();
  // The snapshot says when, not which internal sync.
  await expect(page.getByText(/rows, synced /)).toBeVisible();
  await expect(page.getByText(/sync_\w+/)).toHaveCount(0);
  await page
    .getByRole("list", { name: "Foreign keys" })
    .getByRole("link", { name: "customers.customer_id" })
    .click();
  await expect(
    page.getByRole("heading", { level: 1, name: "customers" }),
  ).toBeVisible();
  await expect(page.getByRole("list", { name: "Referenced by" })).toContainText(
    "invoices.customer_id",
  );

  const calls = (await mock.stats()).requests.join("\n");
  expect(calls).toMatch(
    /POST \/v1\/workspaces\/ws_acme\/foundry\/connectors:test 200/,
  );
  expect(calls).toMatch(
    /POST \/v1\/workspaces\/ws_acme\/foundry\/connectors 200/,
  );
  expect(calls).toMatch(
    /POST \/v1\/workspaces\/ws_acme\/foundry\/connectors\/conn_\w+:sync 200/,
  );
  expect(calls).toMatch(
    /GET \/v1\/workspaces\/ws_acme\/foundry\/connector-syncs\/sync_\w+ 200/,
  );
  // The key went out in two requests and never came back.
  expect(bodies.length).toBeGreaterThan(5);
  for (const body of bodies) expect(body).not.toContain(key);
  expect(await page.content()).not.toContain(key);
});

test("connects Cal.com, syncs it and edits it keeping the stored key", async ({
  page,
}) => {
  const key = "cal_live_e2eCalcomKey0123456789abcd4f2a";
  const bodies = recordResponses(page),
    patches: string[] = [];
  page.on("request", (r) => {
    if (r.method() === "PATCH" && /\/connectors\/conn_\w+$/.test(r.url()))
      patches.push(r.postData() || "");
  });
  await signIn(page);
  await openPicker(page);
  await dialog(page)
    .getByRole("button", { name: "Cal.com", exact: true })
    .click();
  await expect(dialog(page)).toHaveAttribute("aria-label", "Cal.com");
  const guide = dialog(page).locator(".connection-guide");
  await expect(
    guide.getByRole("link", { name: "Settings → Developer → API keys" }),
  ).toHaveAttribute("href", "https://app.cal.com/settings/developer/api-keys");
  await expect(guide).toContainText("Never expires");
  const keyField = dialog(page).getByLabel("API key");
  await expect(keyField).toHaveAttribute("placeholder", "cal_live_…");
  expect((await guide.boundingBox())!.y).toBeLessThan(
    (await keyField.boundingBox())!.y,
  );
  // Another app's key is refused as the WFE refuses it.
  await keyField.fill("lin_api_notcalcom");
  await dialog(page).getByRole("button", { name: "Test connection" }).click();
  await expect(dialog(page).getByRole("alert")).toHaveText(
    "enter a Cal.com API key (cal_live_… or cal_…)",
  );
  // A key whose user administers none of its teams connects, with a warning.
  await keyField.fill("cal_live_member0123456789");
  await dialog(page).getByRole("button", { name: "Test connection" }).click();
  await expect(
    dialog(page).getByRole("list", { name: "Warnings" }),
  ).toContainText(
    "This key's user is not an admin of its Cal.com teams, so team_memberships and users leave out the other members; use a team admin's key to read them.",
  );
  await keyField.fill(key);
  await dialog(page).getByRole("button", { name: "Test connection" }).click();
  const objects = dialog(page).getByRole("group", {
    name: "Cal.com objects this key can read",
  });
  await expect(objects.getByRole("listitem")).toHaveCount(8);
  await expect(objects).toContainText("Every object listed is synced.");
  await expect(objects).not.toContainText("cannot read");
  await dialog(page).getByRole("button", { name: "Connect and sync" }).click();

  await expect(page).toHaveURL(/\/data\/connections\/conn_/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Cal.com" }),
  ).toBeVisible();
  await expect(header(page)).toContainText("cal_live…4f2a");
  await expect(page.locator(".hf-kv")).toContainText("ConnectorCal.com");
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  await expect(
    page.getByRole("list", { name: "Synced tables" }).getByRole("listitem"),
  ).toHaveCount(8);
  await expect(
    tree(page).getByRole("treeitem", { name: "Cal.com", exact: true }),
  ).toBeVisible();

  // An edit starts with the key blank; a key typed and cleared again is
  // sent blank, and the WFE keeps the stored one.
  await page.getByRole("button", { name: "Connection actions" }).click();
  await page.getByRole("menuitem", { name: "Edit connection" }).click();
  const edit = page.getByRole("dialog", { name: "Edit Cal.com" });
  await expect(edit.getByLabel("API key")).toHaveValue("");
  await expect(edit.getByLabel("API key")).toHaveAttribute(
    "placeholder",
    "Leave blank to keep cal_live…4f2a",
  );
  await expect(
    edit.getByRole("button", { name: "Test connection" }),
  ).toHaveCount(0);
  await edit.getByLabel("Connection name").fill("Cal.com bookings");
  await edit.getByLabel("API key").fill("cal_live_typo");
  await edit.getByLabel("API key").fill("");
  await edit.getByRole("button", { name: "Save changes" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Cal.com bookings" }),
  ).toBeVisible();
  await expect(page.locator(".hf-toast")).toContainText("Connection saved");
  await expect(header(page)).toContainText("cal_live…4f2a");
  expect(patches.map((p) => JSON.parse(p))).toEqual([
    { name: "Cal.com bookings", config: { calcom: { apiKey: "" } } },
  ]);
  // The settings didn't move, so no sync was queued.
  await expect(
    page.getByRole("grid", { name: "Recent syncs" }).getByRole("row"),
  ).toHaveCount(2);
  for (const body of bodies) expect(body).not.toContain(key);
  expect(await page.content()).not.toContain(key);
});

test("connects Granola and says what its key can't read", async ({ page }) => {
  await signIn(page);
  // The Helix MCP's connector link opens Granola's form.
  await page.goto("/data?add=granola");
  await expect(dialog(page)).toHaveAttribute("aria-label", "Granola");
  const guide = dialog(page).locator(".connection-guide");
  await expect(guide).toContainText("Settings → Connectors → API keys");
  await expect(guide).toContainText(
    "Transcripts are kept only when you turn them on below.",
  );
  await expect(
    guide.getByRole("link", { name: "Granola Business and Enterprise" }),
  ).toBeVisible();
  const keyField = dialog(page).getByLabel("API key");
  await expect(keyField).toHaveAttribute("placeholder", "grn_…");
  await keyField.fill("grn_badE2eGranolaKey");
  await dialog(page).getByRole("button", { name: "Test connection" }).click();
  await expect(dialog(page).getByRole("alert")).toHaveText(
    "Granola rejected this key; check it and enter it again",
  );
  await expect(
    dialog(page).getByRole("button", { name: "Connect and sync" }),
  ).toHaveCount(0);
  // A personal key can't read spaces: the rest is listed and synced.
  await keyField.fill("grn_personalE2eGranolaKeyQRST");
  await dialog(page).getByRole("button", { name: "Test connection" }).click();
  const objects = dialog(page).getByRole("group", {
    name: "Granola objects this key can read",
  });
  await expect(objects.getByRole("listitem")).toHaveCount(4);
  await expect(objects).toContainText("This key cannot read: spaces.");
  await dialog(page).getByRole("button", { name: "Connect and sync" }).click();

  await expect(
    page.getByRole("heading", { level: 1, name: "Granola" }),
  ).toBeVisible();
  await expect(header(page)).toContainText("grn…QRST");
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  const tables = page.getByRole("list", { name: "Synced tables" });
  await expect(tables.getByRole("listitem")).toHaveCount(4);
  await expect(tables.getByRole("link", { name: /spaces/ })).toHaveCount(0);
  // Folders keep their parent folder but lose the link to spaces.
  await tables.getByRole("link", { name: /^folders/ }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "folders" }),
  ).toBeVisible();
  await expect(
    page.getByRole("list", { name: "Foreign keys" }).getByRole("listitem"),
  ).toHaveText(["parent_folder_idfolders.folder_id"]);
});

test("connects a PostgreSQL database with the tables chosen", async ({
  page,
}) => {
  const password = "e2e-db-password-91x";
  const bodies = recordResponses(page);
  await signIn(page);
  await openPicker(page);
  await dialog(page)
    .getByRole("button", { name: "PostgreSQL", exact: true })
    .click();
  const form = dialog(page);
  // The guide gives the stamp's outbound addresses to allow.
  await expect(form).toContainText(
    "allow these addresses: 192.0.2.10, 192.0.2.11.",
  );
  await form.getByLabel("Host", { exact: true }).fill("db.example.com");
  await form.getByLabel("Database", { exact: true }).fill("warehouse");
  await form.getByLabel("Username", { exact: true }).fill("foundry_readonly");
  await form.getByLabel("Password", { exact: true }).fill(password);
  await form.getByRole("button", { name: "Test connection" }).click();
  await expect(form.getByText("Choose tables to sync")).toBeVisible();
  // Every table starts chosen; choose two, and see what they link to.
  await expect(
    form.getByRole("button", { name: "Connect and sync every table" }),
  ).toBeVisible();
  await form.getByRole("button", { name: "Clear" }).click();
  await form.getByRole("checkbox", { name: /^orders/ }).check();
  await form.getByRole("checkbox", { name: /^order_items/ }).check();
  await expect(
    form.getByRole("checkbox", { name: /^products/ }),
  ).toHaveAccessibleName(/Related to your selection/);
  await form.getByRole("button", { name: "Connect and sync 2 tables" }).click();

  await expect(
    page.getByRole("heading", { level: 1, name: "warehouse" }),
  ).toBeVisible();
  await expect(header(page)).toContainText(
    "foundry_readonly@db.example.com:5432/warehouse",
  );
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  const tables = page.getByRole("list", { name: "Synced tables" });
  await expect(tables.getByRole("listitem")).toHaveCount(2);
  await expect(page.getByText("order_items, orders")).toBeVisible();

  await tables.getByRole("link", { name: /^orders/ }).click();
  const columns = page.getByRole("grid", { name: "Columns" });
  await expect(columns.getByRole("row", { name: /user_id/ })).toContainText(
    "uuid · nullable",
  );
  await expect(columns.getByRole("row", { name: /placed_at/ })).toContainText(
    "timestamp with time zone",
  );
  // accounts was not synced, so its key is named but not linked.
  const foreign = page.getByRole("list", { name: "Foreign keys" });
  await expect(foreign).toContainText("accounts.id");
  await expect(foreign.getByRole("link")).toHaveCount(0);
  await page
    .getByRole("list", { name: "Referenced by" })
    .getByRole("link", { name: "order_items.order_id" })
    .click();
  await expect(
    page.getByRole("heading", { level: 1, name: "order_items" }),
  ).toBeVisible();
  await expect(
    page
      .locator(".hf-panel")
      .filter({ has: page.getByRole("heading", { name: "Primary key" }) }),
  ).toContainText("order_id, line");
  for (const body of bodies) expect(body).not.toContain(password);
});

test("shows the WFE's reasons for bad credentials and unreachable hosts", async ({
  page,
}) => {
  await signIn(page);
  await openPicker(page);
  await dialog(page)
    .getByRole("button", { name: "Stripe", exact: true })
    .click();
  await dialog(page)
    .getByLabel("Restricted API key")
    .fill("pk_test_publishable");
  await dialog(page).getByRole("button", { name: "Test connection" }).click();
  await expect(dialog(page).getByRole("alert")).toHaveText(
    "enter a Stripe secret or restricted key (sk_… or rk_…)",
  );
  await dialog(page).getByLabel("Restricted API key").fill("sk_test_badkey42");
  await dialog(page).getByRole("button", { name: "Test connection" }).click();
  await expect(dialog(page).getByRole("alert")).toHaveText(
    "Stripe rejected this key; check it and test again",
  );
  // No objects were discovered, so there is nothing to connect.
  await expect(
    dialog(page).getByRole("button", { name: "Connect and sync" }),
  ).toHaveCount(0);

  await dialog(page).getByRole("button", { name: "← All sources" }).click();
  await dialog(page)
    .getByRole("button", { name: "MySQL", exact: true })
    .click();
  const form = dialog(page);
  await form.getByLabel("Host", { exact: true }).fill("10.0.0.12");
  await form.getByLabel("Database", { exact: true }).fill("app");
  await form.getByLabel("Username", { exact: true }).fill("foundry");
  await form.getByLabel("Password", { exact: true }).fill("wrong");
  await form.getByRole("button", { name: "Test connection" }).click();
  await expect(form.getByRole("alert")).toHaveText(
    "that host is not on the public internet; Helix can only reach public database hosts",
  );
  await form.getByLabel("Host", { exact: true }).fill("mysql.example.com");
  await form.getByRole("button", { name: "Test connection" }).click();
  await expect(form.getByRole("alert")).toHaveText(
    "authentication failed; check the username, password and read permissions, then test again",
  );
  await page.keyboard.press("Escape");
  await expect(dialog(page)).toHaveCount(0);
  await expect(page).toHaveURL(/\/data$/);
});

test("says no data is connected when the first ontology read only documents", async ({
  page,
}) => {
  await signInToNewWorkspace(page);
  await createFirstOntology(page);
  await page.goto("/data");
  const connections = page.getByRole("region", { name: "Connections" });
  await expect(
    connections.getByRole("heading", { name: "No data connected" }),
  ).toBeVisible();
  await expect(header(page)).toContainText("0 connections");
  // Its document is there, in General.
  await expect(
    page
      .getByRole("list", { name: "Document sets" })
      .getByRole("listitem", { name: "General", exact: true }),
  ).toContainText("1 document");
  // The empty state connects from here.
  await connections.getByRole("button", { name: "Add data" }).click();
  await expect(dialog(page)).toHaveAttribute(
    "aria-label",
    "Where is your data?",
  );
});

test("offers databases once the workspace upgrades to Foundry Pro", async ({
  page,
}) => {
  await signInToNewWorkspace(page);
  // The new database the wizard starts with is chosen: on to the sources,
  // where nothing is connected yet.
  await startOntology(page, "Orders");
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(
    page.getByRole("heading", { name: "No connectors yet" }),
  ).toBeVisible();
  await openOnboardingPicker(page);
  const postgres = dialog(page).getByRole("button", {
    name: "PostgreSQL",
    exact: true,
  });
  await expect(postgres).toHaveAccessibleDescription(/Needs Foundry Pro/);
  await expect(
    dialog(page).getByRole("button", { name: "Amazon S3", exact: true }),
  ).toHaveAccessibleDescription(/Needs Foundry Pro/);
  await postgres.click();
  const gate = dialog(page).getByRole("note");
  await expect(gate).toContainText("PostgreSQL connections need Foundry Pro");
  await expect(gate).toContainText(
    "Pro adds PostgreSQL, MySQL and S3 to Globex",
  );
  await expect(
    gate.getByRole("link", { name: "Upgrade to Pro" }),
  ).toHaveAttribute("href", "/settings/billing");

  // The WFE holds the gate whatever the page shows.
  const ws = (await (await page.request.get("/papi/v1/workspaces")).json())
    .workspaces[0].id;
  const refused = await page.request.post(
    `/papi/v1/workspaces/${ws}/foundry/connectors`,
    {
      data: {
        name: "Shop",
        config: {
          postgres: {
            host: "db.example.com",
            database: "shop",
            user: "ro",
            password: "pw",
          },
        },
      },
      headers: {
        origin: new URL(page.url()).origin,
        "x-helix-csrf": await csrfToken(page),
      },
    },
  );
  expect(refused.status()).toBe(400);
  expect(await refused.json()).toMatchObject({
    message: "PostgreSQL connectors need Foundry Pro",
    details: [expect.objectContaining({ reason: "FOUNDRY_PRO_REQUIRED" })],
  });

  // The gate leads to Plan & billing; after checkout, Data offers databases.
  await gate.getByRole("link", { name: "Upgrade to Pro" }).click();
  await expect(page).toHaveURL(/\/settings\/billing$/);
  await page.getByRole("button", { name: "Upgrade", exact: true }).click();
  await page
    .getByRole("dialog", { name: "Upgrade Foundry" })
    .getByRole("button", { name: "Continue to payment" })
    .click();
  await page.getByRole("button", { name: "Pay and subscribe" }).click();
  await expect(page).toHaveURL(/\/settings\/billing$/);
  await expect(
    page.getByRole("heading", { name: /Foundry Pro/ }),
  ).toContainText("Active");
  // The wizard carries on where it was, now offering databases.
  await openOnboardingPicker(page);
  await expect(
    dialog(page).getByRole("button", { name: "PostgreSQL", exact: true }),
  ).not.toHaveAccessibleDescription(/Needs Foundry Pro/);
  await dialog(page)
    .getByRole("button", { name: "PostgreSQL", exact: true })
    .click();
  await expect(dialog(page).getByRole("note")).toHaveCount(0);
  await expect(dialog(page).getByLabel("Host", { exact: true })).toBeVisible();
  await dialog(page).getByRole("button", { name: "← All sources" }).click();
  await dialog(page)
    .getByRole("button", { name: "Amazon S3", exact: true })
    .click();
  await expect(dialog(page).getByRole("note")).toHaveCount(0);
  await expect(dialog(page).getByLabel("Bucket")).toBeVisible();
});

test("offers databases on Foundry Max, as on Pro", async ({ page }) => {
  await mock.configure({ checkoutCardOnFile: true });
  await signInToNewWorkspace(page);
  await page.goto("/settings/billing");
  await page.getByRole("button", { name: "Upgrade", exact: true }).click();
  const upgrade = page.getByRole("dialog", { name: "Upgrade Foundry" });
  await upgrade.getByRole("radio", { name: "Max" }).check();
  await upgrade.getByRole("button", { name: "Continue to payment" }).click();
  await expect(
    page.getByRole("heading", { name: /Foundry Max/ }),
  ).toContainText("Active", { timeout: 15_000 });

  await startOntology(page, "Orders");
  await page.getByRole("button", { name: "Continue" }).click();
  await openOnboardingPicker(page);
  for (const name of ["PostgreSQL", "MySQL", "Amazon S3"])
    await expect(
      dialog(page).getByRole("button", { name, exact: true }),
    ).not.toHaveAccessibleDescription(/Needs Foundry Pro/);
  await dialog(page)
    .getByRole("button", { name: "PostgreSQL", exact: true })
    .click();
  await expect(dialog(page).getByRole("note")).toHaveCount(0);
  await expect(dialog(page).getByLabel("Host", { exact: true })).toBeVisible();
});

test("gates a database connection's syncs once the workspace leaves Pro", async ({
  page,
}) => {
  // The mock's Pro ends only at a period end, so the catalog says it has.
  await page.route("**/foundry/connector-kinds", (route) =>
    route.fulfill({
      json: {
        kinds: [
          { kind: "stripe", requiresPro: false, available: true },
          { kind: "postgres", requiresPro: true, available: false },
        ],
      },
    }),
  );
  await signIn(page);
  await page.goto("/data/connections/conn_acme_shop");
  const gate = page.getByRole("note");
  await expect(gate).toContainText("PostgreSQL connections need Foundry Pro");
  await expect(
    gate.getByRole("link", { name: "Upgrade to Pro" }),
  ).toHaveAttribute("href", "/settings/billing");
  await expect(page.getByRole("button", { name: "Sync now" })).toBeDisabled();
  // A Stripe connection is in every plan.
  await page.goto("/data/connections/conn_acme_stripe");
  await expect(
    page.getByRole("heading", { level: 1, name: "Stripe" }),
  ).toBeVisible();
  await expect(page.getByRole("note")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Sync now" })).toBeEnabled();
});

test("shows a failed sync, refuses a second live sync and deletes the connection", async ({
  page,
}) => {
  await signIn(page);
  await openPicker(page);
  await dialog(page)
    .getByRole("button", { name: "PostHog", exact: true })
    .click();
  const form = dialog(page);
  await form.getByLabel("Personal API key").fill("phx_e2eKey123");
  await form.getByLabel("Event history (days)").fill("120");
  await form.getByRole("button", { name: "Test connection" }).click();
  // The test pinned the key's only project.
  await expect(form.getByLabel("Project ID (optional)")).toHaveValue("12345");
  await expect(form).toContainText("PostHog objects this key can read");
  await form.getByRole("button", { name: "Connect and sync" }).click();

  await expect(
    page.getByRole("heading", { level: 1, name: "PostHog" }),
  ).toBeVisible();
  await expect(header(page)).toContainText("US Cloud · project 12345");
  await expect(
    page.getByRole("alert").filter({ hasText: "The last sync failed." }),
  ).toContainText(
    "table events has more than 2000000 rows; choose fewer tables",
    SYNCED,
  );
  await expect(header(page).getByText("Failed", { exact: true })).toBeVisible();

  // Sync again; while it runs, a second sync and deleting are refused.
  await page.getByRole("button", { name: "Sync now" }).click();
  await expect(page.getByRole("button", { name: "Syncing…" })).toBeDisabled();
  const id = new URL(page.url()).pathname.split("/").pop()!;
  const again = await page.request.post(
    `/papi/v1/workspaces/ws_acme/foundry/connectors/${id}:sync`,
    {
      data: {},
      headers: {
        origin: new URL(page.url()).origin,
        "x-helix-csrf": await csrfToken(page),
      },
    },
  );
  expect(again.status()).toBe(409);
  expect((await again.json()).message).toBe(
    "a sync of this connector is already queued or running",
  );
  await page.getByRole("button", { name: "Connection actions" }).click();
  await page.getByRole("menuitem", { name: "Delete connection" }).click();
  await page
    .getByRole("dialog", { name: "Delete PostHog?" })
    .getByRole("button", { name: "Delete connection" })
    .click();
  await expect(page.locator(".hf-toast.is-error")).toContainText(
    "a sync of this connector is queued or running; delete it once the sync ends",
  );
  await page
    .getByRole("dialog", { name: "Delete PostHog?" })
    .getByRole("button", { name: "Cancel" })
    .click();

  // Once it has ended, the connection deletes and leaves the Data page.
  await expect(header(page).getByText("Failed", { exact: true })).toBeVisible(
    SYNCED,
  );
  await expect(
    page.getByRole("grid", { name: "Recent syncs" }).getByRole("row"),
  ).toHaveCount(3);
  await page.getByRole("button", { name: "Connection actions" }).click();
  await page.getByRole("menuitem", { name: "Delete connection" }).click();
  await page
    .getByRole("dialog", { name: "Delete PostHog?" })
    .getByRole("button", { name: "Delete connection" })
    .click();
  await expect(page).toHaveURL(/\/data$/);
  await expect(page.locator(".hf-toast")).toContainText("Connection deleted");
  await expect(
    tree(page).getByRole("link", { name: "PostHog", exact: true }),
  ).toHaveCount(0);
  await expect(
    tree(page).getByRole("treeitem", { name: "PostHog", exact: true }),
  ).toHaveCount(0);
  await expect(
    tree(page).getByRole("link", { name: "Stripe", exact: true }),
  ).toBeVisible();
});

test("edits a connection and keeps its stored credential", async ({ page }) => {
  await signIn(page);
  await page.goto("/data");
  await tree(page).getByRole("link", { name: "Shop database" }).click();
  await page.getByRole("button", { name: "Connection actions" }).click();
  await page.getByRole("menuitem", { name: "Edit connection" }).click();
  const form = page.getByRole("dialog", { name: "Edit Shop database" });
  await expect(form.getByLabel("Password", { exact: true })).toHaveValue("");
  await expect(form.getByLabel("Password", { exact: true })).toHaveAttribute(
    "placeholder",
    "Leave blank to keep the stored password",
  );
  await form.getByLabel("Connection name").fill("Shop (primary)");
  await form.getByLabel("Database", { exact: true }).fill("shop_replica");
  await form.getByRole("button", { name: "Save changes" }).click();

  await expect(
    page.getByRole("heading", { level: 1, name: "Shop (primary)" }),
  ).toBeVisible();
  await expect(page.locator(".hf-toast")).toContainText("Connection saved");
  // New settings point elsewhere, so the WFE queued a sync of them, which
  // the stored password still opens.
  await expect(header(page)).toContainText(
    "db.acme.example.com:5432/shop_replica",
  );
  await expect(
    page.getByRole("grid", { name: "Recent syncs" }).getByRole("row"),
  ).toHaveCount(3, SYNCED);
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  expect((await mock.stats()).requests).toContain(
    "PATCH /v1/workspaces/ws_acme/foundry/connectors/conn_acme_shop 200",
  );
});

test("finds connections and tables, and keeps Data routes on the allowlist", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  const search = page.getByRole("searchbox", { name: "Filter data" });
  // A table's name finds its connection, open with the tables it matched.
  await search.fill("order_it");
  await expect(tree(page).getByRole("link")).toHaveText([
    "Shop database",
    "order_items",
  ]);
  await tree(page)
    .getByRole("link", { name: /^order_items/ })
    .click();
  await expect(
    page.getByRole("heading", { level: 1, name: "order_items" }),
  ).toBeVisible();
  await page.goBack();
  // A kind's name finds its connections, without their tables; a name that
  // matches nothing says so.
  await search.fill("postgresql");
  await expect(tree(page).getByRole("link")).toHaveText(["Shop database"]);
  await search.fill("zzz");
  await expect(
    page.getByText("No sources, tables or documents match “zzz”."),
  ).toBeVisible();

  await page.keyboard.press("ControlOrMeta+k");
  const palette = page.getByRole("dialog", { name: "Search your workspace" });
  await palette.getByRole("combobox").fill("balance_trans");
  await expect(palette.getByText("balance_transactions")).toBeVisible();
  await page.keyboard.press("Enter");
  await expect(
    page.getByRole("heading", { level: 1, name: "balance_transactions" }),
  ).toBeVisible();

  const origin = new URL(page.url()).origin;
  const put = await page.request.put(
    "/papi/v1/workspaces/ws_acme/foundry/connectors/conn_acme_shop",
    { headers: { origin }, data: {} },
  );
  expect(put.status()).toBe(405);
  expect(put.headers()["allow"]).toBe("DELETE, GET, PATCH");
  const nested = await page.request.get(
    "/papi/v1/workspaces/ws_acme/foundry/connectors/conn_acme_shop/secret",
  );
  expect(nested.status()).toBe(404);
  expect((await mock.stats()).requests.join("\n")).not.toMatch(/PUT|secret/);
});

// A second PostgreSQL connection, created through the BFF as the form does.
async function addPostgres(page: Page, name: string) {
  const created = await page.request.post(
    "/papi/v1/workspaces/ws_acme/foundry/connectors",
    {
      data: {
        name,
        config: {
          postgres: {
            host: "billing.acme.example.com",
            database: "billing",
            user: "ro",
            password: "pw",
          },
        },
      },
      headers: {
        origin: new URL(page.url()).origin,
        "x-helix-csrf": await csrfToken(page),
      },
    },
  );
  expect(created.status()).toBe(200);
}

test("lists connections in a folder per kind that keeps closed", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  await addPostgres(page, "Billing replica");
  await page.reload();
  // Folders in the kinds' order, connections as they were created, then
  // the documents.
  const roots = tree(page).locator(":scope > [role=treeitem] > .dt-row");
  await expect(roots.locator(".dt-name")).toHaveText([
    "Stripe",
    "PostgreSQL",
    "Documents",
  ]);
  await expect(roots.locator(".dt-count")).toHaveText(["1", "2", "0"]);
  const postgres = tree(page).getByRole("treeitem", {
    name: "PostgreSQL",
    exact: true,
  });
  await expect(postgres).toHaveAttribute("aria-expanded", "true");
  await expect(postgres).toHaveAttribute("aria-level", "1");
  await expect(postgres.getByRole("link")).toHaveText([
    "Shop database",
    "Billing replica",
  ]);
  // Created without a sync; its row syncs it. Its status is a dot, said in
  // the row's name; what it holds is in its name's tooltip.
  const billing = item(page, "Billing replica"),
    name = billing.getByRole("link", { name: "Billing replica" });
  await expect(billing).toHaveAccessibleName("Billing replica, Not synced");
  await expect(billing).toHaveAttribute("aria-level", "2");
  await expect(name).toHaveAttribute(
    "title",
    "Billing replica\nTables appear after the first sync",
  );
  await billing.getByRole("button", { name: "Sync Billing replica" }).click();
  await expect(page.locator(".hf-toast")).toContainText("Sync queued");
  await expect(billing).toHaveAccessibleName("Billing replica, Synced", SYNCED);
  await expect(name).toHaveAttribute(
    "title",
    /\d+ tables · [\d,]+ records · synced/,
  );

  // Closed by a click on its row, it stays closed on the next visit; a
  // filter opens what it finds.
  await postgres.locator(":scope > .dt-row .dt-chevron").click();
  await expect(postgres).toHaveAttribute("aria-expanded", "false");
  await expect(postgres.getByRole("link")).toHaveCount(0);
  await page.reload();
  await expect(postgres).toHaveAttribute("aria-expanded", "false");
  await expect(item(page, "Shop database")).toHaveCount(0);
  const search = page.getByRole("searchbox", { name: "Filter data" });
  await search.fill("billing");
  await expect(tree(page).getByRole("link")).toHaveText(["Billing replica"]);
  await search.fill("");
  await expect(postgres).toHaveAttribute("aria-expanded", "false");
  await postgres.locator(":scope > .dt-row .dt-chevron").click();

  // Anywhere on a row opens its connection: its link covers it, so a click
  // on its logo lands on the link.
  await item(page, "Billing replica")
    .getByRole("img", { name: "PostgreSQL" })
    .click({ force: true });
  await expect(
    page.getByRole("heading", { level: 1, name: "Billing replica" }),
  ).toBeVisible();
});

test("renames a connection in place, for owners and admins only", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  const rename = page.getByRole("button", { name: "Rename Shop database" }),
    field = page.getByRole("textbox", { name: "Name for Shop database" });
  // Escape keeps the name, and focus goes back to its row.
  await rename.click();
  await expect(field).toBeFocused();
  await expect(field).toHaveValue("Shop database");
  await field.fill("Not this");
  await field.press("Escape");
  await expect(field).toHaveCount(0);
  await expect(item(page, "Shop database")).toBeFocused();
  await expect(
    tree(page).getByRole("link", { name: "Shop database", exact: true }),
  ).toBeVisible();
  // A blank or unchanged name saves nothing.
  await rename.click();
  await field.fill("   ");
  await field.press("Enter");
  await rename.click();
  await field.press("Enter");
  const renames = async () =>
    (await mock.stats()).requests.filter((r) =>
      r.startsWith("PATCH /v1/workspaces/ws_acme/foundry/connectors/"),
    );
  expect(await renames()).toEqual([]);
  // Enter saves it, as the connection's page then shows.
  await rename.click();
  await field.fill("  Shop (primary) ");
  await field.press("Enter");
  await expect(page.locator(".hf-toast")).toContainText("Connection renamed");
  await expect(
    tree(page).getByRole("link", { name: "Shop (primary)", exact: true }),
  ).toBeVisible();
  // Leaving the field saves it too.
  await page.getByRole("button", { name: "Rename Shop (primary)" }).click();
  await page
    .getByRole("textbox", { name: "Name for Shop (primary)" })
    .fill("Shop");
  await page.getByRole("heading", { level: 1, name: "Connectors" }).click();
  await expect(
    tree(page).getByRole("link", { name: "Shop", exact: true }),
  ).toBeVisible();
  expect(await renames()).toEqual([
    "PATCH /v1/workspaces/ws_acme/foundry/connectors/conn_acme_shop 200",
    "PATCH /v1/workspaces/ws_acme/foundry/connectors/conn_acme_shop 200",
  ]);
  await tree(page).getByRole("link", { name: "Shop", exact: true }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Shop" }),
  ).toBeVisible();

  // A member reads the tree without renames or syncs.
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto("/data");
  await expect(
    tree(page).getByRole("link", { name: "Stripe", exact: true }),
  ).toBeVisible();
  await expect(tree(page).getByRole("button", { name: /^Rename/ })).toHaveCount(
    0,
  );
  await expect(tree(page).getByRole("button", { name: /^Sync/ })).toHaveCount(
    0,
  );
  await expect(page.getByRole("button", { name: "Add data" })).toHaveCount(0);
});

test("keeps a loaded page when a refresh fails, and says what failed", async ({
  page,
}) => {
  const stripe =
    "**/papi/v1/workspaces/ws_acme/foundry/connectors/conn_acme_stripe";
  const down = (route: { fulfill: (r: object) => Promise<void> }) =>
    route.fulfill({
      status: 503,
      contentType: "application/json",
      body: JSON.stringify({ code: 14, message: "upstream connect error" }),
    });
  await signIn(page);
  await page.goto("/data/connections/conn_acme_stripe");
  await expect(
    header(page).getByRole("heading", { name: "Stripe" }),
  ).toBeVisible();

  // Its next refetch fails: the page stays, with a note and Retry.
  await page.route(stripe, down);
  await page
    .getByRole("link", { name: "Connectors", exact: true })
    .first()
    .click();
  await expect(
    page.getByRole("grid", { name: "Connections" }).getByText("Shop database"),
  ).toBeVisible();
  await page.waitForTimeout(5500);
  await page
    .getByRole("link", { name: /Stripe/ })
    .first()
    .click();
  const note = page
    .getByRole("status")
    .filter({ hasText: "Couldn’t refresh." });
  await expect(note).toBeVisible({ timeout: 10_000 });
  await expect(
    header(page).getByRole("heading", { name: "Stripe" }),
  ).toBeVisible();
  await page.unroute(stripe);
  await note.getByRole("button", { name: "Retry" }).click();
  await expect(note).toHaveCount(0);

  // Never loaded: an error that is not a 404 offers Retry, not "gone".
  await page.route(stripe, down);
  await page.goto("/data/connections/conn_acme_stripe");
  await expect(page.getByText("Couldn’t load this connection")).toBeVisible({
    timeout: 10_000,
  });
  await page.unroute(stripe);
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(
    header(page).getByRole("heading", { name: "Stripe" }),
  ).toBeVisible();

  // A 404 is a connection that no longer exists.
  await page.goto("/data/connections/conn_gone");
  await expect(
    page.getByText("This connection no longer exists"),
  ).toBeVisible();
});

// ---------------------------------------------------------------- Amazon S3

async function openS3(page: Page) {
  await openPicker(page);
  await dialog(page)
    .getByRole("group", { name: "Or read files" })
    .getByRole("button", { name: "Amazon S3", exact: true })
    .click();
  await expect(dialog(page)).toHaveAttribute("aria-label", "Amazon S3");
}
const ROLE = "arn:aws:iam::123456789012:role/helix-foundry";
// A secret in a response would show as a non-empty secretAccessKey.
const SECRET_FIELD = /"secretAccessKey":"[^"]/;

test("connects Amazon S3 with an IAM role, syncs the chosen tables and opens one", async ({
  page,
  context,
}) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const bodies = recordResponses(page);
  await signIn(page);
  await openS3(page);
  const form = dialog(page);
  await expect(form.locator(".connection-guide")).toContainText(
    "must require that ID",
  );
  await expect(
    form.getByRole("radio", { name: "IAM role (recommended)" }),
  ).toBeChecked();
  // The workspace's external ID, read-only and copyable.
  const externalId = form.getByLabel("External ID", { exact: true });
  await expect(externalId).toHaveValue("helix-foundry-ws_acme");
  await expect(externalId).toHaveAttribute("readonly", "");
  const clipboard = () => page.evaluate(() => navigator.clipboard.readText());
  await form.getByRole("button", { name: "Copy external ID" }).click();
  await expect.poll(clipboard).toBe("helix-foundry-ws_acme");
  // The trust policy names Helix's principal and requires the ID.
  const trust = form
    .getByRole("figure", { name: "trust policy" })
    .locator("pre");
  expect(JSON.parse((await trust.textContent())!)).toEqual({
    Version: "2012-10-17",
    Statement: [
      {
        Effect: "Allow",
        Principal: {
          AWS: "arn:aws:iam::000000000000:role/helix-mock-foundry-connector",
        },
        Action: ["sts:AssumeRole", "sts:TagSession"],
        Condition: {
          StringEquals: { "sts:ExternalId": "helix-foundry-ws_acme" },
        },
      },
    ],
  });
  await form.getByRole("button", { name: "Copy trust policy" }).click();
  await expect.poll(clipboard).toBe(await trust.textContent());
  await expect(page.locator(".hf-toast")).toContainText(
    "Copied the trust policy.",
  );

  // A pasted s3:// URI fills bucket and folder, and the permissions follow.
  const permissions = form.getByRole("figure", {
    name: "permissions policy",
  });
  await expect(permissions).toContainText("arn:aws:s3:::your-bucket/*");
  await page.evaluate(() =>
    navigator.clipboard.writeText("s3://acme-exports/e2e/"),
  );
  await form.getByLabel("Bucket").focus();
  await page.keyboard.press("ControlOrMeta+V");
  await expect(form.getByLabel("Bucket")).toHaveValue("acme-exports");
  await expect(form.getByLabel("Folder (optional)")).toHaveValue("e2e/");
  await expect(permissions).toContainText("arn:aws:s3:::acme-exports/e2e/*");
  await expect(permissions).toContainText('"e2e/*"');
  await form.getByLabel("Role ARN").fill(ROLE);
  await form.getByRole("button", { name: "Test connection" }).click();

  // The test found the region and the folder's tables.
  await expect(form.getByText("The bucket is in eu-west-2.")).toBeVisible();
  const orders = form.getByRole("group", { name: "orders" });
  await expect(orders).toContainText("PARQUET");
  await expect(orders).toContainText(
    "2 files · 1.2 GB · 8 columns · No primary key",
  );
  await expect(form.getByRole("group", { name: "customers" })).toContainText(
    "1 file · 84 KB · 5 columns · Key id",
  );
  await expect(form.getByRole("status")).toHaveText(
    "Selected 1.2 GB of 20 GB.",
  );
  await expect(form).toContainText("notes.xlsx: unsupported file type");
  await form.getByRole("button", { name: "Clear" }).click();
  await expect(form.getByRole("status")).toHaveText("Selected 0 B of 20 GB.");
  for (const name of ["customers", "events", "orders"])
    await form.getByRole("checkbox", { name: new RegExp(`^${name}`) }).check();
  await form.getByRole("button", { name: "Connect and sync 3 tables" }).click();

  await expect(
    page.getByRole("heading", { level: 1, name: "acme-exports/e2e" }),
  ).toBeVisible();
  await expect(header(page)).toContainText(
    "Amazon S3 · s3://acme-exports/e2e/",
  );
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  const details = page.locator(".hf-kv");
  for (const text of [
    "IAM role",
    "eu-west-2",
    ROLE,
    "customers, events, orders",
  ])
    await expect(details).toContainText(text);
  const tables = page.getByRole("list", { name: "Synced tables" });
  await expect(tables.getByRole("listitem")).toHaveCount(3);
  // The sync measured orders' key, which the test could not.
  const synced = tables.getByRole("link", { name: /^orders/ });
  await expect(synced).toContainText("24,310 rows · 8 columns");
  await expect(synced).toContainText("Key id");
  await synced.click();
  const columns = page.getByRole("grid", { name: "Columns" });
  await expect(columns.getByRole("row", { name: /placed_at/ })).toContainText(
    "timestamptz",
  );
  await expect(columns.getByRole("row", { name: /shipping/ })).toContainText(
    "json · nullable",
  );
  await expect(
    page.getByText("Column types are inferred from the files."),
  ).toBeVisible();
  await expect(page.getByText("is an API source")).toHaveCount(0);

  const calls = (await mock.stats()).requests.join("\n");
  expect(calls).toMatch(
    /POST \/v1\/workspaces\/ws_acme\/foundry\/connectors:test 200/,
  );
  expect(calls).toMatch(
    /POST \/v1\/workspaces\/ws_acme\/foundry\/connectors\/conn_\w+:sync 200/,
  );
  for (const body of bodies) expect(body).not.toMatch(SECRET_FIELD);
});

test("drops an S3 test's result when the settings change while it runs", async ({
  page,
}) => {
  // The first test is held until the folder has been changed.
  let release = () => {};
  const held = new Promise<void>((r) => (release = r));
  await page.route(
    "**/foundry/connectors:test",
    async (route) => {
      const response = await route.fetch();
      await held;
      await route.fulfill({ response });
    },
    { times: 1 },
  );
  await signIn(page);
  await openS3(page);
  const form = dialog(page);
  await form.getByLabel("Bucket").fill("acme-exports");
  await form.getByLabel("Role ARN").fill(ROLE);
  const testButton = form.getByRole("button", { name: /^Test/ });
  await testButton.click();
  await expect(testButton).toHaveText("Testing connection…");
  await form.getByLabel("Folder (optional)").fill("e2e");
  release();
  // The result describes the whole bucket: the edit stays, nothing is shown.
  await expect(testButton).toHaveText("Test connection");
  await expect(form.getByLabel("Folder (optional)")).toHaveValue("e2e");
  await expect(form.getByText("Choose tables to sync")).toHaveCount(0);
  await expect(form.getByRole("alert")).toHaveCount(0);
  // Tested again, the folder's tables show.
  await testButton.click();
  await expect(form.getByText("The bucket is in eu-west-2.")).toBeVisible();
  await expect(form.getByLabel("Folder (optional)")).toHaveValue("e2e");
  await expect(
    form.getByRole("button", { name: "Connect and sync every table" }),
  ).toBeVisible();
});

test("takes an S3 role connection's region when the test asks for it", async ({
  page,
}) => {
  const tested = page.waitForRequest("**/foundry/connectors:test");
  await signIn(page);
  await openS3(page);
  const form = dialog(page);
  await form.getByLabel("Bucket").fill("acme-exports");
  await form.getByLabel("Role ARN").fill(ROLE);
  // A role has no endpoint, so Advanced holds only the region.
  await form.getByText("Advanced", { exact: true }).click();
  await expect(form.getByLabel(/^Endpoint/)).toHaveCount(0);
  const region = form.getByLabel("Region");
  await expect(region).toHaveAttribute("placeholder", "Detected automatically");
  await region.fill("eu-west");
  await form.getByRole("button", { name: "Test connection" }).click();
  await expect(form.getByRole("alert")).toHaveText(
    "enter the bucket's region, like eu-west-2",
  );
  expect((await tested).postDataJSON().config.s3).toEqual({
    bucket: "acme-exports",
    region: "eu-west",
    role: { roleArn: ROLE },
  });
  await region.fill("eu-west-2");
  await form.getByRole("button", { name: "Test connection" }).click();
  await expect(form.getByText("The bucket is in eu-west-2.")).toBeVisible();
});

test("connects Amazon S3 with an access key once S3 accepts it, and edits it keeping the key", async ({
  page,
}) => {
  const secret = "e2eS3cretAccessKey/ExampleOnly+Zq8w";
  const bodies = recordResponses(page);
  // Without Helix's connector role, only keys are offered.
  await mock.configure({ s3RoleAccess: false });
  await signIn(page);
  await openS3(page);
  const form = dialog(page);
  await expect(
    form.getByRole("radio", { name: "IAM role (recommended)" }),
  ).toBeDisabled();
  await expect(form.getByRole("radio", { name: "Access key" })).toBeChecked();
  await expect(form).toContainText(
    "IAM role access isn’t available here; use an access key.",
  );
  await expect(form.getByLabel("External ID", { exact: true })).toHaveCount(0);
  // AWS keys come with the permissions an IAM user needs.
  await form.getByLabel("Bucket").fill("acme-exports");
  await expect(
    form.getByRole("figure", { name: "permissions policy" }),
  ).toContainText('"Resource": "arn:aws:s3:::acme-exports/*"');
  await form.getByLabel("Access key ID").fill("AKIAE2EEXAMPLE");
  await form.getByLabel("Secret access key").fill("bad-secret");
  await form.getByRole("button", { name: "Test connection" }).click();
  await expect(form.getByRole("alert")).toHaveText(
    "S3 rejected the access key; check the key ID and secret",
  );
  await expect(
    form.getByRole("button", { name: /^Connect and sync/ }),
  ).toHaveCount(0);
  await form.getByLabel("Secret access key").fill(secret);
  await form.getByRole("button", { name: "Test connection" }).click();
  await expect(form.getByText("Choose tables to sync")).toBeVisible();
  await form
    .getByRole("button", { name: "Connect and sync every table" })
    .click();

  await expect(
    page.getByRole("heading", { level: 1, name: "acme-exports" }),
  ).toBeVisible();
  await expect(header(page)).toContainText("s3://acme-exports/");
  await expect(page.locator(".hf-kv")).toContainText("secret …Zq8w");
  await expect(page.locator(".hf-kv")).toContainText("AKIAE2EEXAMPLE");
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  await expect(
    page.getByRole("list", { name: "Synced tables" }).getByRole("listitem"),
  ).toHaveCount(5);

  // An edit starts with the secret blank and keeps the stored one.
  await page.getByRole("button", { name: "Connection actions" }).click();
  await page.getByRole("menuitem", { name: "Edit connection" }).click();
  const edit = page.getByRole("dialog", { name: "Edit acme-exports" });
  await expect(edit.getByLabel("Secret access key")).toHaveValue("");
  await expect(edit.getByLabel("Secret access key")).toHaveAttribute(
    "placeholder",
    "Saved — leave blank to keep",
  );
  await expect(edit).toContainText(
    "Syncs every table. To change which tables sync, enter the secret access key and test the connection.",
  );
  await expect(
    edit.getByRole("button", { name: "Test connection" }),
  ).toHaveCount(0);
  await expect(edit.getByLabel("Access key ID")).toHaveValue("AKIAE2EEXAMPLE");
  await edit.getByLabel("Folder (optional)").fill("e2e");
  await edit.getByRole("button", { name: "Save changes" }).click();
  await expect(page.locator(".hf-toast")).toContainText("Connection saved");
  await expect(header(page)).toContainText("s3://acme-exports/e2e/");
  await expect(page.locator(".hf-kv")).toContainText("secret …Zq8w");
  // The new folder queued a sync, which the stored key opens.
  await expect(
    page.getByRole("grid", { name: "Recent syncs" }).getByRole("row"),
  ).toHaveCount(3, SYNCED);
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  expect((await mock.stats()).requests).toContain(
    `PATCH /v1/workspaces/ws_acme/foundry/connectors/${page.url().split("/").pop()} 200`,
  );
  for (const body of bodies) {
    expect(body).not.toContain(secret);
    expect(body).not.toMatch(SECRET_FIELD);
  }
  expect(await page.content()).not.toContain(secret);
});

test("keeps a stored S3 role when role access or the kinds are missing, and renames it", async ({
  page,
}) => {
  await signIn(page);
  const created = await page.request.post(
    "/papi/v1/workspaces/ws_acme/foundry/connectors",
    {
      data: {
        name: "Exports",
        config: { s3: { bucket: "acme-exports", role: { roleArn: ROLE } } },
      },
      headers: {
        origin: new URL(page.url()).origin,
        "x-helix-csrf": await csrfToken(page),
      },
    },
  );
  expect(created.status()).toBe(200);
  const id = (await created.json()).connector.id;
  const edit = page.getByRole("dialog", { name: "Edit Exports" });
  const openEdit = async () => {
    await page.goto(`/data/connections/${id}`);
    await page.getByRole("button", { name: "Connection actions" }).click();
    await page.getByRole("menuitem", { name: "Edit connection" }).click();
  };

  // The kinds can't be read: the form can't know the access choices.
  await page.route("**/foundry/connector-kinds", (route) =>
    route.fulfill({
      status: 403,
      json: { message: "the connector kinds are unavailable" },
    }),
  );
  await openEdit();
  await expect(edit.getByRole("alert")).toHaveText(
    "the connector kinds are unavailable",
  );
  await expect(edit.getByRole("button", { name: "Save changes" })).toHaveCount(
    0,
  );
  await page.unroute("**/foundry/connector-kinds");

  // Role access is off: the role stays, shown but not tested.
  await mock.configure({ s3RoleAccess: false });
  await openEdit();
  await expect(
    edit.getByRole("radio", { name: "IAM role (recommended)" }),
  ).toBeChecked();
  await expect(
    edit.getByRole("radio", { name: "IAM role (recommended)" }),
  ).toBeDisabled();
  await expect(edit.getByLabel("Role ARN")).toHaveValue(ROLE);
  await expect(edit.getByLabel("Role ARN")).toHaveAttribute("readonly", "");
  await expect(edit.getByLabel("Access key ID")).toHaveCount(0);
  await expect(edit.getByLabel("Secret access key")).toHaveCount(0);
  await expect(edit.getByRole("button", { name: /^Test/ })).toHaveCount(0);
  await expect(edit).toContainText(
    "Syncs every table. To change which tables sync, use an access key and test the connection.",
  );
  // A rename sends the name alone, which needs no test.
  const patched = page.waitForRequest(
    (r) => r.method() === "PATCH" && r.url().endsWith(id),
  );
  await edit.getByLabel("Connection name").fill("Exports (role)");
  await edit.getByRole("button", { name: "Save changes" }).click();
  expect((await patched).postDataJSON()).toEqual({ name: "Exports (role)" });
  await expect(page.locator(".hf-toast")).toContainText("Connection saved");
  await expect(
    page.getByRole("heading", { level: 1, name: "Exports (role)" }),
  ).toBeVisible();
  await expect(page.locator(".hf-kv")).toContainText(ROLE);
});

test("previews S3 tables, warns of a problem and holds Connect over the size limit", async ({
  page,
}) => {
  // The mock's folder is small, so the WFE's limit is made smaller: 1 GiB.
  await page.route("**/foundry/connectors:test", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    if (body.discovery) body.discovery.maxBytes = String(2 ** 30);
    await route.fulfill({ response, json: body });
  });
  await signIn(page);
  await openS3(page);
  const form = dialog(page);
  await form.getByLabel("Bucket").fill("s3://bad-records/e2e");
  await form.getByLabel("Role ARN").fill(ROLE);
  // The URI splits once the field is left.
  await expect(form.getByLabel("Bucket")).toHaveValue("bad-records");
  await expect(form.getByLabel("Folder (optional)")).toHaveValue("e2e");
  await form.getByRole("button", { name: "Test connection" }).click();

  // A malformed sample is a warning on its table, not a refusal.
  await expect(form.getByRole("group", { name: "events" })).toContainText(
    "events/2026-10-02.jsonl.zst record 12: not a JSON object. The sync will fail until this is fixed.",
  );
  // Columns and preview rows on demand; Parquet has no preview yet.
  const customers = form.getByRole("group", { name: "customers" });
  await expect(customers.getByRole("table")).toBeHidden();
  await customers.getByText("Columns and preview").click();
  await expect(
    customers.getByRole("list", { name: "customers columns" }),
  ).toContainText("signed_upstring");
  const preview = customers.getByRole("table", { name: "customers preview" });
  await expect(preview.getByRole("row")).toHaveCount(6);
  await expect(preview).toContainText("ada@example.com");
  const orders = form.getByRole("group", { name: "orders" });
  await orders.getByText("Columns and preview").click();
  await expect(orders).toContainText("placed_attimestamptz");
  await expect(orders).toContainText("Preview after the first sync.");
  await expect(form).toContainText(
    "archive/old.csv: archived in Glacier; restore it first",
  );

  // Over the limit, Connect waits for fewer tables.
  await expect(form.getByRole("status")).toHaveText(
    "Selected 1.2 GB of 1 GB. Choose fewer tables or a narrower folder to connect.",
  );
  const connect = form.getByRole("button", { name: /^Connect and sync/ });
  await expect(connect).toBeDisabled();
  await form.getByRole("checkbox", { name: /^orders/ }).uncheck();
  await expect(form.getByRole("status")).toHaveText("Selected 6.1 MB of 1 GB.");
  await expect(connect).toHaveText("Connect and sync 4 tables");
  await connect.click();

  // events is in the sync, so it fails on the record the test sampled.
  await expect(
    page.getByRole("heading", { level: 1, name: "bad-records/e2e" }),
  ).toBeVisible();
  await expect(
    page.getByRole("alert").filter({ hasText: "The last sync failed." }),
  ).toContainText(
    "events/2026-10-02.jsonl.zst record 12: not a JSON object",
    SYNCED,
  );
});

// The Helix MCP's connector links name the workspace and the kind, so the
// user enters the credentials here and never through an agent.
test("opens a connector's form from a link to its workspace", async ({
  page,
}) => {
  const link = "/data?add=postgres&workspace=ws_acme";
  // Signed out, the link survives signing in.
  await page.goto(link);
  await expect(page).toHaveURL(/\/login\?return_to=/);
  await signIn(page);
  await expect(dialog(page)).toHaveAttribute("aria-label", "PostgreSQL");
  await expect(dialog(page).getByLabel("Host", { exact: true })).toBeVisible();
  await expect(page).toHaveURL(/\/data\?add=postgres$/);
  await page.keyboard.press("Escape");
  await expect(page).toHaveURL(/\/data$/);

  // A workspace without an ontology has no Data pages: a link of its own
  // opens the form over its onboarding wizard, and the wizard stays.
  await page.getByRole("button", { name: "Workspace: Acme" }).click();
  await page.getByRole("menuitem", { name: "Create workspace" }).click();
  const create = page.getByRole("dialog", { name: "Create a workspace" });
  await create.getByLabel("Workspace name").fill("Initech");
  await create.getByRole("button", { name: "Create workspace" }).click();
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Get started" }),
  ).toBeVisible();
  // The link starts the user's own data: the wizard, past Get started.
  const wizard = page.getByRole("heading", {
    level: 1,
    name: "Your first ontology",
  });
  await page.goto("/data?add=stripe");
  await expect(dialog(page)).toHaveAttribute("aria-label", "Stripe");
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(wizard).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(dialog(page)).toHaveCount(0);
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(
    page.getByRole("button", { name: "Workspace: Initech" }),
  ).toBeVisible();

  // With another workspace open, the link opens its own, and it stays chosen.
  await page.goto(link);
  await expect(
    page.getByRole("button", { name: "Workspace: Acme" }),
  ).toBeVisible();
  await expect(dialog(page)).toHaveAttribute("aria-label", "PostgreSQL");
  await page.goto("/");
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts$/);
  await expect(
    page.getByRole("button", { name: "Workspace: Acme" }),
  ).toBeVisible();

  // A workspace the user isn't in switches nothing, and a kind Foundry
  // doesn't offer opens the picker.
  await page.goto("/data?add=oracle&workspace=ws_globex");
  await expect(page.locator(".hf-toast")).toContainText(
    "That link is for a workspace you aren’t a member of.",
  );
  await expect(dialog(page)).toHaveAttribute(
    "aria-label",
    "Where is your data?",
  );
  await expect(page).toHaveURL(/\/data\?add=oracle$/);
  await expect(
    page.getByRole("button", { name: "Workspace: Acme" }),
  ).toBeVisible();
});
