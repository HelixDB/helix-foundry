import type { Page } from "@playwright/test";
import {
  choose,
  expect,
  loadContracts,
  mock,
  postToWfe,
  signIn,
  test,
  uploadAndLoadContract,
} from "./support/mock";

// Dashboards: tiles of queries over ontologies' loads, drawn as the
// Analyst draws an answer's visuals. The Assistant builds one from a card
// its person runs; an Analyst answer's chart is pinned to one; owners and
// admins edit, pin and publish them at a public link a fresh browser opens
// with no session, until they unpublish it; a member reads them all and
// changes only their own.

const D = "/v1/workspaces/ws_acme/foundry/dashboards";
const BY_STATUS = {
  start: { type: "Invoice" },
  return: { group_count: { at: 0, prop: "status" } },
};
const CUSTOMERS = {
  start: { type: "Customer" },
  return: { rows: { columns: [{ at: 0 }, { at: 0, prop: "currency" }] } },
};
const tile = (id: string, title: string, query: object, kind?: string) => ({
  id,
  title,
  ontology_id: "ont_acme_contracts",
  load: { kind: "current" },
  query,
  visual: kind ? { kind } : {},
});

/** Makes a dashboard through the BFF, as its New dashboard does: its id. */
async function makeDashboard(
  page: Page,
  name: string,
  tiles = [
    tile("t1", "Invoices by status", BY_STATUS, "bar"),
    tile("t2", "Customers", CUSTOMERS, "table"),
  ],
): Promise<string> {
  return (
    await postToWfe(page, D, {
      name,
      specJson: JSON.stringify({
        v: 1,
        tiles,
        layout: { rows: [tiles.map((t) => t.id)] },
      }),
      clientToken: `c-${name}`,
    })
  ).dashboard.id;
}

const dockOf = (page: Page) =>
  page.getByRole("complementary", { name: "Assistant" });

test("builds a dashboard from the Assistant's card, which its person runs", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 100 });
  await signIn(page);
  await loadContracts(page);
  await page.getByRole("button", { name: "Assistant", exact: true }).click();
  const dock = dockOf(page);
  await dock
    .getByRole("textbox", { name: "Message" })
    .fill("Build a dashboard of invoices by status for Customer contracts");
  await dock.getByRole("button", { name: "Send", exact: true }).click();
  // It tried the tile before it proposed it.
  await expect(dock.getByText("Tried a dashboard tile")).toBeVisible();
  const card = dock.getByRole("region", { name: "Action: Make a dashboard" });
  await expect(card).toBeVisible();
  // A new dashboard's workspace is the person's own: no warning.
  await expect(card.getByText(/Not named by you/)).toHaveCount(0);
  await expect(card).toContainText("“Invoices by status”");
  await card.getByText("Full request", { exact: true }).click();
  await expect(card.getByText(/"group_count"/)).toBeVisible();
  await card.getByRole("button", { name: "Create", exact: true }).click();
  await expect(card.getByText("Ran", { exact: true })).toBeVisible();
  await expect(
    dock.getByText(/You ran this action: it succeeded/),
  ).toBeVisible();
  await card.getByRole("link", { name: "Open the dashboard" }).click();
  await expect(page).toHaveURL(/\/dashboards\/dash_/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Invoices by status" }),
  ).toBeVisible();
  const chart = page
    .getByRole("main")
    .getByRole("figure", { name: "Invoices by status" });
  await expect(chart.locator(".recharts-bar-rectangle")).toHaveCount(5);
  // The page it is on goes with the next message.
  await expect(dock.getByText("Looking at: Invoices by status")).toBeVisible();
  // The Dashboards section lists it.
  await expect(
    page
      .getByRole("navigation", { name: "Dashboards" })
      .getByRole("link", { name: /Invoices by status/ }),
  ).toHaveAttribute("aria-current", "page");
});

test("pins an Analyst answer's charts to a dashboard, then edits it", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 100 });
  await signIn(page);
  await uploadAndLoadContract(page);
  await page.getByRole("link", { name: "Analyst", exact: true }).click();
  const form = page.getByRole("form", { name: "Ask a question" });
  await form
    .getByRole("textbox", { name: /^Question/ })
    .fill("How are the invoices doing?");
  await form.getByRole("button", { name: "Ask", exact: true }).click();
  const answer = page
    .getByRole("article", { name: "Question: How are the invoices doing?" })
    .getByRole("region", { name: "Answer" });
  const byStatus = answer.getByRole("figure", { name: "Invoices by status" });
  await expect(byStatus).toBeVisible({ timeout: 15_000 });

  // A new dashboard, named here.
  await byStatus.getByRole("button", { name: "Pin to dashboard" }).click();
  let dialog = page.getByRole("dialog", { name: "Pin to a dashboard" });
  await expect(dialog.getByRole("combobox", { name: "Dashboard" })).toHaveText(
    /New dashboard…/,
  );
  const pin = dialog.getByRole("button", { name: "Pin", exact: true });
  await expect(pin).toBeDisabled();
  await dialog.getByLabel("Name of the new dashboard").fill("Invoices");
  await pin.click();
  dialog = page.getByRole("dialog", { name: "Pinned to a dashboard" });
  await expect(dialog).toContainText(
    "“Invoices by status” is a tile of Invoices now.",
  );
  await dialog.getByRole("button", { name: "Done" }).click();

  // The next goes to it, reading the load the answer read.
  const largest = answer.getByRole("figure", { name: "Largest invoices" });
  await largest.getByRole("button", { name: "Pin to dashboard" }).click();
  dialog = page.getByRole("dialog", { name: "Pin to a dashboard" });
  await expect(dialog.getByRole("combobox", { name: "Dashboard" })).toHaveText(
    /Invoices/,
  );
  await dialog.getByLabel("Keep showing the load this answer read").check();
  await dialog.getByRole("button", { name: "Pin", exact: true }).click();
  await page
    .getByRole("dialog", { name: "Pinned to a dashboard" })
    .getByRole("link", { name: "Open Invoices" })
    .click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Invoices" }),
  ).toBeVisible();
  const main = page.getByRole("main");
  await expect(
    main
      .getByRole("figure", { name: "Invoices by status" })
      .locator(".recharts-bar-rectangle"),
  ).toHaveCount(5);
  const ranked = main.getByRole("figure", { name: "Largest invoices" });
  await expect(ranked.locator(".recharts-bar-rectangle")).toHaveCount(10);
  // Its table names entities, each a link into the load it read.
  await ranked.getByRole("radio", { name: "Table" }).click();
  await expect(ranked.getByRole("link").first()).toHaveAttribute(
    "href",
    /\/explore\/ont_acme_contracts\/entities\/.+\?load=load_/,
  );

  // Edit: a new name, the second tile beside the first, the first shown as
  // a table under a title of its own; its read on view.
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await page
    .getByRole("region", { name: "Name and description" })
    .getByLabel("Name", { exact: true })
    .fill("Invoice health");
  const second = page.getByRole("region", { name: "Tile: Largest invoices" });
  await second
    .getByRole("button", { name: "Arrange Largest invoices" })
    .click();
  await page.getByRole("menuitem", { name: "Move up" }).click();
  const first = page.getByRole("region", { name: "Tile: Invoices by status" });
  await choose(
    first.getByRole("combobox", { name: "Show Invoices by status as" }),
    "Table",
  );
  await first.getByLabel("Title").fill("Status counts");
  await page
    .getByRole("region", { name: "Tile: Status counts" })
    .getByText("The read")
    .click();
  await expect(
    page.getByRole("region", { name: "Tile: Status counts" }),
  ).toContainText("Reads the current load of Customer contracts");
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByText("Dashboard saved.")).toBeVisible();
  await expect(
    page.getByRole("heading", { level: 1, name: "Invoice health" }),
  ).toBeVisible();
  await page.reload();
  const counts = main.getByRole("figure", { name: "Status counts" });
  await expect(counts.getByRole("grid")).toBeVisible();
  // Side by side, in one row.
  const row = main.locator(".db-row");
  await expect(row).toHaveCount(1);
  await expect(row.getByRole("figure")).toHaveCount(2);

  // A tile removed, then the dashboard.
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await page
    .getByRole("region", { name: "Tile: Largest invoices" })
    .getByRole("button", { name: "Arrange Largest invoices" })
    .click();
  await page.getByRole("menuitem", { name: "Remove tile" }).click();
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(main.getByRole("figure")).toHaveCount(1);
  await page.getByRole("button", { name: "More actions" }).click();
  await page.getByRole("menuitem", { name: "Delete dashboard" }).click();
  await page
    .getByRole("dialog", { name: "Delete Invoice health?" })
    .getByRole("button", { name: "Delete dashboard" })
    .click();
  await expect(page).toHaveURL(/\/dashboards$/);
  await expect(
    page.getByRole("heading", { name: "No dashboards yet" }),
  ).toBeVisible();
});

test("publishes a snapshot that a fresh browser opens, until it is unpublished", async ({
  page,
  browser,
}) => {
  await signIn(page);
  await loadContracts(page);
  const id = await makeDashboard(page, "Revenue");
  await page.goto(`/dashboards/${id}`);
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  let dialog = page.getByRole("dialog", { name: "Publish this dashboard?" });
  await expect(dialog).toContainText(
    "Anyone with the link sees this dashboard’s data, without signing in.",
  );
  await dialog.getByRole("button", { name: "Publish", exact: true }).click();
  await expect(page.getByText("Published at a public link.")).toBeVisible();
  dialog = page.getByRole("dialog", { name: "Public link" });
  const link = await dialog.getByLabel("Link").inputValue();
  expect(link).toMatch(/\/share\/hxd_[A-Za-z0-9_-]{43}$/);
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"]);
  await dialog.getByRole("button", { name: "Copy link" }).click();
  await expect(page.getByText("Link copied.")).toBeVisible();
  await dialog.getByRole("button", { name: "Close" }).click();
  await expect(
    page.getByRole("button", { name: "Public link", exact: true }),
  ).toBeVisible();

  // A fresh browser: no session, no app, no id of the workspace's.
  const other = await browser.newContext();
  const pub = await other.newPage();
  const res = await pub.goto(link);
  expect(res?.headers()["referrer-policy"]).toBe("no-referrer");
  expect(res?.headers()["x-robots-tag"]).toBe("noindex, nofollow");
  await expect(
    pub.getByRole("heading", { level: 1, name: "Revenue" }),
  ).toBeVisible();
  await expect(pub.getByText(/^Snapshot from /)).toBeVisible();
  await expect(
    pub
      .getByRole("figure", { name: "Invoices by status" })
      .locator(".recharts-bar-rectangle"),
  ).toHaveCount(5);
  const customers = pub.getByRole("figure", { name: "Customers" });
  await expect(customers.getByRole("grid")).toBeVisible();
  // Its entities are names, with nothing to open.
  await expect(customers.getByRole("link")).toHaveCount(0);
  await expect(pub.getByRole("navigation")).toHaveCount(0);
  const html = await pub.content();
  for (const secret of ["ws_acme", "ont_acme_contracts", id, "usr_ada"])
    expect(html).not.toContain(secret);
  await expect(pub).toHaveTitle("Revenue · Helix Foundry");

  // An edit reaches the link only once the snapshot is refreshed.
  await postToWfe(page, `${D}/${id}:pin`, { pinned: true });
  await page.reload();
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await page
    .getByRole("region", { name: "Name and description" })
    .getByLabel("Name", { exact: true })
    .fill("Revenue this quarter");
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByText("Dashboard saved.")).toBeVisible();
  await pub.reload();
  await expect(
    pub.getByRole("heading", { level: 1, name: "Revenue" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Public link", exact: true }).click();
  dialog = page.getByRole("dialog", { name: "Public link" });
  await expect(dialog).toContainText("You’ve edited the dashboard since");
  await dialog.getByRole("button", { name: "Refresh snapshot" }).click();
  await expect(page.getByText("Snapshot refreshed.")).toBeVisible();
  await pub.reload();
  await expect(
    pub.getByRole("heading", { level: 1, name: "Revenue this quarter" }),
  ).toBeVisible();

  // Unpublished, the link shows nothing at once.
  await dialog.getByRole("button", { name: "Unpublish" }).click();
  await page
    .getByRole("dialog", { name: "Unpublish this dashboard?" })
    .getByRole("button", { name: "Unpublish" })
    .click();
  await expect(
    page.getByText("Unpublished: the link shows nothing now."),
  ).toBeVisible();
  await pub.reload();
  await expect(
    pub.getByRole("heading", { name: "There’s no dashboard at this link" }),
  ).toBeVisible();
  // Nor does a link of no token's form, asked of no one.
  await pub.goto("/share/not-a-token");
  await expect(
    pub.getByRole("heading", { name: "There’s no dashboard at this link" }),
  ).toBeVisible();
  await other.close();
});

test("a member reads every dashboard and changes only their own", async ({
  page,
  browser,
}) => {
  await signIn(page);
  await loadContracts(page);
  const pinned = await makeDashboard(page, "Team revenue");
  await postToWfe(page, `${D}/${pinned}:pin`, { pinned: true });

  const context = await browser.newContext();
  const alan = await context.newPage();
  await signIn(alan, "alan@example.com");
  await alan.getByRole("link", { name: "Dashboards", exact: true }).click();
  const list = alan.getByRole("navigation", { name: "Dashboards" });
  await expect(list.getByRole("list", { name: "Pinned" })).toContainText(
    "Team revenue",
  );
  await list.getByRole("link", { name: /Team revenue/ }).click();
  await expect(
    alan
      .getByRole("main")
      .getByRole("figure", { name: "Invoices by status" })
      .locator(".recharts-bar-rectangle"),
  ).toHaveCount(5);
  // Not theirs, and pinned: nothing to change, nothing to publish.
  for (const name of ["Edit", "Publish", "More actions"])
    await expect(alan.getByRole("button", { name, exact: true })).toHaveCount(
      0,
    );

  // Their own: they edit and delete it, and neither pin nor publish it.
  await alan.getByRole("link", { name: "New dashboard" }).click();
  const create = alan.getByRole("dialog", { name: "New dashboard" });
  await create.getByLabel("Name").fill("Alan’s view");
  await create.getByRole("button", { name: "Create dashboard" }).click();
  await expect(
    alan.getByRole("heading", { level: 1, name: "Alan’s view" }),
  ).toBeVisible();
  await expect(alan.getByText("No tiles yet")).toBeVisible();
  await expect(
    alan.getByRole("button", { name: "Edit", exact: true }),
  ).toBeVisible();
  await expect(
    alan.getByRole("button", { name: "Publish", exact: true }),
  ).toHaveCount(0);
  await alan.getByRole("button", { name: "More actions" }).click();
  await expect(
    alan.getByRole("menuitem", { name: "Delete dashboard" }),
  ).toBeVisible();
  await expect(alan.getByRole("menuitem", { name: /Pin/ })).toHaveCount(0);
  await context.close();
});
