import type { Page } from "@playwright/test";
import {
  APP_ORIGIN,
  askAnalyst,
  choose,
  csrfToken,
  expect,
  loadContracts,
  mock,
  postToWfe,
  signIn,
  test,
  uploadAndLoadContract,
} from "./support/mock";

// Dashboards: tiles of queries over ontologies' loads, drawn as the
// Analyst draws an answer's visuals. Founder makes one at once when its
// person asks for a dashboard, with Undo; an Analyst answer's chart is
// pinned to one; anyone shares one with the workspace by its page's link
// (Copy link); owners and admins edit, pin and publish them at a public
// link a fresh browser opens with no session, until they unpublish it, it
// expires or an owner turns public links off; a member reads them all and
// changes only their own. What can't be read, saved or shown says so.

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

/** Opens the dashboard's Share menu and its public link's item. */
async function openPublicLink(page: Page, item: string) {
  await page.getByRole("button", { name: "Share", exact: true }).click();
  await page.getByRole("menuitem", { name: item }).click();
}

/** Makes a dashboard through the BFF, as Founder or an Analyst answer does: its id. */
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
  page.getByRole("complementary", { name: "Founder" });

test("makes the dashboard its person asks Founder for at once, with Undo", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 100 });
  await signIn(page);
  await loadContracts(page);
  await page.getByRole("button", { name: "Founder", exact: true }).click();
  const dock = dockOf(page);
  await dock
    .getByRole("textbox", { name: "Message" })
    .fill("Build a dashboard of invoices by status for Customer contracts");
  await dock.getByRole("button", { name: "Send", exact: true }).click();
  // It tried the tile before it made it.
  await expect(dock.getByText("Tried a dashboard tile")).toBeVisible();
  // Asked for a dashboard, it makes it without a card to click.
  const card = dock.getByRole("region", {
    name: "Action: Made the dashboard “Invoices by status”",
  });
  await expect(card).toBeVisible();
  await expect(card.getByText("Made", { exact: true })).toBeVisible();
  await expect(card).toContainText(
    "Founder made it because you asked for a dashboard.",
  );
  await expect(
    dock.getByRole("button", { name: "Create", exact: true }),
  ).toHaveCount(0);
  await expect(dock.getByText(/You ran this action/)).toHaveCount(0);
  await card.getByText("Full request", { exact: true }).click();
  await expect(card.getByText(/"group_count"/)).toBeVisible();
  await expect(
    dock.getByText(/I made the dashboard “Invoices by status”/),
  ).toBeVisible();
  await expect(card.getByRole("button", { name: "Undo" })).toBeEnabled();
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

test("adds an Analyst answer's charts to a dashboard, then edits it", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 100 });
  await signIn(page);
  await uploadAndLoadContract(page);
  // An Analyst thread, read only on Founder's page, keeps Add to dashboard.
  const question = await askAnalyst(page, "How are the invoices doing?");
  const answer = question.getByRole("region", { name: "Answer" });
  const byStatus = answer.getByRole("figure", { name: "Invoices by status" });
  await expect(byStatus).toBeVisible({ timeout: 15_000 });

  // A new dashboard, named here.
  await byStatus.getByRole("button", { name: "Add to dashboard" }).click();
  let dialog = page.getByRole("dialog", { name: "Add to a dashboard" });
  await expect(dialog.getByRole("combobox", { name: "Dashboard" })).toHaveText(
    /New dashboard…/,
  );
  const add = dialog.getByRole("button", { name: "Add", exact: true });
  await expect(add).toBeDisabled();
  await dialog.getByLabel("Name of the new dashboard").fill("Invoices");
  await add.click();
  dialog = page.getByRole("dialog", { name: "Added to a dashboard" });
  await expect(dialog).toContainText(
    "“Invoices by status” is a tile of Invoices now.",
  );
  await dialog.getByRole("button", { name: "Done" }).click();

  // The next goes to it, reading the load the answer read.
  const largest = answer.getByRole("figure", { name: "Largest invoices" });
  await largest.getByRole("button", { name: "Add to dashboard" }).click();
  dialog = page.getByRole("dialog", { name: "Add to a dashboard" });
  await expect(dialog.getByRole("combobox", { name: "Dashboard" })).toHaveText(
    /Invoices/,
  );
  await dialog.getByLabel("Keep showing the load this answer read").check();
  await dialog.getByRole("button", { name: "Add", exact: true }).click();
  await page
    .getByRole("dialog", { name: "Added to a dashboard" })
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
  await openPublicLink(page, "Make a public link…");
  let dialog = page.getByRole("dialog", { name: "Make a public link?" });
  await expect(dialog).toContainText(
    "Anyone with the link can view it, without signing in.",
  );
  await expect(dialog).toContainText(
    "It doesn’t change until you publish it again.",
  );
  await dialog.getByRole("button", { name: "Publish", exact: true }).click();
  await expect(page.getByText("Published at a public link.")).toBeVisible();
  dialog = page.getByRole("dialog", { name: "Public link" });
  const link = await dialog.getByLabel("Link", { exact: true }).inputValue();
  expect(link).toMatch(/\/share\/hxd_[A-Za-z0-9_-]{43}$/);
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"]);
  await dialog.getByRole("button", { name: "Copy link" }).click();
  await expect(page.getByText("Public link copied.")).toBeVisible();
  await dialog.getByRole("button", { name: "Close" }).click();
  await expect(
    page.getByRole("main").getByText("Public link", { exact: true }),
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
  await expect(pub.getByText(/^As of /)).toBeVisible();
  // Foundry's wordmark and footer link to its landing page, sending no
  // referrer (the address holds the link's token).
  for (const name of ["Helix Foundry", "Made with Helix Foundry"]) {
    const link = pub.getByRole("link", { name, exact: true });
    await expect(link).toHaveAttribute("href", "/");
    await expect(link).toHaveAttribute("rel", "noopener noreferrer");
  }
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
  await postToWfe(page, `${D}/${id}:pin`, {
    pinned: true,
    expectedRevision: "1",
  });
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
  await openPublicLink(page, "Manage public link…");
  dialog = page.getByRole("dialog", { name: "Public link" });
  await expect(dialog).toContainText("You’ve edited the dashboard since");
  await dialog.getByRole("button", { name: "Refresh snapshot" }).click();
  await expect(page.getByText("Snapshot refreshed.")).toBeVisible();
  await pub.reload();
  await expect(
    pub.getByRole("heading", { level: 1, name: "Revenue this quarter" }),
  ).toBeVisible();

  // Unpublished in one click, the link shows nothing at once.
  await dialog.getByRole("button", { name: "Unpublish" }).click();
  await expect(
    page.getByText(
      "Unpublished: the link shows nothing now. Publishing again makes a new link.",
    ),
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
  await postToWfe(page, `${D}/${pinned}:pin`, {
    pinned: true,
    expectedRevision: "1",
  });

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
  const own = (
    await postToWfe(alan, D, { name: "Alan’s view", clientToken: "c-alan" })
  ).dashboard.id;
  await alan.goto(`/dashboards/${own}`);
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

test("the library: a card per dashboard with its miniature, searched and filtered", async ({
  page,
}) => {
  await signIn(page);
  await loadContracts(page);
  await makeDashboard(page, "Revenue");
  const health = await makeDashboard(page, "Invoice health", [
    tile("n", "Invoices", {
      start: { type: "Invoice" },
      return: { count: {} },
    }),
    tile("s", "By status", BY_STATUS, "line"),
  ]);
  await postToWfe(page, `${D}/${health}:pin`, {
    pinned: true,
    expectedRevision: "1",
  });
  await page.goto("/dashboards");
  // The library's list, not the sidebar's group of the same name.
  const cards = page
    .getByRole("main")
    .getByRole("list", { name: "Dashboards", exact: true });
  await expect(cards.getByRole("article")).toHaveCount(2);
  // Pinned first, each a miniature drawn from its spec, without a tile read.
  const first = cards.getByRole("article").first();
  await expect(first.getByRole("link")).toHaveText("Invoice health");
  await expect(first.getByText("Pinned", { exact: true })).toBeVisible();
  await expect(first.locator(".db-mini-title")).toHaveText([
    "Invoices",
    "By status",
  ]);
  await expect(first.locator(".db-mini-figure")).toHaveCount(1);
  await expect(first.locator(".db-mini-chart polyline")).toHaveCount(1);
  const revenue = cards.getByRole("article").nth(1);
  await expect(revenue.locator(".db-mini-chart rect")).toHaveCount(6);
  await expect(revenue.locator(".db-mini-table")).toHaveCount(1);
  const stats = await mock.stats();
  expect(stats.requests.filter((r) => r.includes(":read"))).toEqual([]);

  // Search by name, case and accents aside; filters for pinned and own.
  const search = page.getByRole("searchbox", { name: "Search dashboards…" });
  await search.fill("REVEN");
  await expect(cards.getByRole("article")).toHaveCount(1);
  await expect(cards.getByRole("link")).toHaveText("Revenue");
  await expect(page.getByText("1 of 2 dashboards")).toBeVisible();
  await search.fill("nothing like it");
  await expect(
    page.getByRole("heading", {
      name: "No dashboards match “nothing like it”",
    }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Show all dashboards" }).click();
  await expect(cards.getByRole("article")).toHaveCount(2);
  await page.getByRole("radio", { name: /Pinned/ }).click();
  await expect(cards.getByRole("link")).toHaveText(["Invoice health"]);
  await page.getByRole("radio", { name: /Yours/ }).click();
  await expect(cards.getByRole("article")).toHaveCount(2);
  await page.getByRole("radio", { name: /All/ }).click();

  // A card opens its dashboard.
  await revenue.click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Revenue" }),
  ).toBeVisible();

  // One column on a phone, the page never wider than it.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/dashboards");
  await expect(cards.getByRole("article")).toHaveCount(2);
  const [a, b] = await cards
    .getByRole("article")
    .evaluateAll((els) => els.map((e) => e.getBoundingClientRect().left));
  expect(a).toBe(b);
  expect(
    await page.evaluate(
      () => document.scrollingElement!.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
});

test("New dashboard opens Founder with its request started, the caret at its end", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/dashboards");
  await page
    .getByRole("main")
    .getByRole("button", { name: "New dashboard" })
    .click();
  // No dialog of its own: Founder's dock, on a new thread.
  await expect(page.getByRole("dialog", { name: "New dashboard" })).toHaveCount(
    0,
  );
  const dock = dockOf(page);
  const box = dock.getByRole("textbox", { name: "Message" });
  await expect(box).toHaveValue("Build me a dashboard for: ");
  await expect(box).toBeFocused();
  expect(
    await box.evaluate((el: HTMLTextAreaElement) => [
      el.selectionStart,
      el.selectionEnd,
      el.value.length,
    ]),
  ).toEqual([26, 26, 26]);
  await expect(
    dock.getByRole("button", { name: /^Threads: New thread/ }),
  ).toBeVisible();
  // What is typed follows it.
  await page.keyboard.type("invoices by status");
  await expect(box).toHaveValue("Build me a dashboard for: invoices by status");

  // The sidebar's New dashboard starts it again, on a new thread.
  await page
    .getByRole("complementary")
    .filter({ has: page.getByRole("navigation", { name: "Dashboards" }) })
    .getByRole("button", { name: "New dashboard" })
    .click();
  await expect(box).toHaveValue("Build me a dashboard for: ");

  // On a phone, Founder's drawer.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.reload();
  await page
    .getByRole("main")
    .getByRole("button", { name: "New dashboard" })
    .click();
  const drawer = page.getByRole("dialog", { name: "Founder" });
  await expect(drawer.getByRole("textbox", { name: "Message" })).toHaveValue(
    "Build me a dashboard for: ",
  );
});

test("a public dashboard's wide table scrolls inside its tile, never the page", async ({
  page,
  browser,
}) => {
  await signIn(page);
  await loadContracts(page);
  const WIDE = {
    start: { type: "Invoice" },
    steps: [{ via: "billed_to", dir: "out", to: "Customer" }],
    return: {
      rows: {
        columns: [
          { at: 1, prop: "name" },
          { at: 1, prop: "currency" },
          { at: 1 },
          { at: 0 },
          { at: 0, prop: "invoice_id" },
          { at: 0, prop: "status" },
          { at: 0, prop: "amount_due" },
        ],
      },
    },
  };
  const id = await makeDashboard(page, "Collections", [
    tile("w", "Unpaid or overdue invoices by account", WIDE, "table"),
  ]);
  const token = (
    await postToWfe(page, `${D}/${id}:publish`, { expectedRevision: "1" })
  ).dashboard.share.token as string;
  const other = await browser.newContext({
    viewport: { width: 390, height: 844 },
  });
  const pub = await other.newPage();
  await pub.goto(`/share/${token}`);
  const table = pub
    .getByRole("figure", { name: "Unpaid or overdue invoices by account" })
    .getByRole("grid");
  await expect(table.getByRole("columnheader")).toHaveCount(7);
  await expect(
    pub.getByRole("link", { name: "Helix Foundry", exact: true }),
  ).toBeVisible();
  // The table is wider than its tile and scrolls; the page doesn't.
  expect(await table.evaluate((el) => el.scrollWidth > el.clientWidth)).toBe(
    true,
  );
  expect(
    await pub.evaluate(
      () => document.scrollingElement!.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  // A fade at the right says there is more; scrolled to the end, at the left.
  const wrap = pub.locator(".hf-grid-wrap");
  await expect(wrap).toHaveClass(/is-scrolled-right/);
  await table.evaluate((el) => el.scrollTo({ left: el.scrollWidth }));
  await expect(wrap).toHaveClass(/is-scrolled-left/);
  await expect(wrap).not.toHaveClass(/is-scrolled-right/);
  // A header cut short shows whole on hover; numbers sit right.
  await expect(
    table.getByRole("columnheader").last().getByRole("button"),
  ).toHaveAttribute("title", /amount_due|Amount due/i);
  await expect(table.getByRole("columnheader").last()).toHaveClass(/is-right/);

  // On a desktop it fits or scrolls the same way, the page never sideways.
  await pub.setViewportSize({ width: 1440, height: 1000 });
  expect(
    await pub.evaluate(
      () => document.scrollingElement!.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  await other.close();
});

/** A change through the BFF, as another person's tab would make it. */
async function changeOnWfe(
  page: Page,
  method: "PATCH" | "DELETE",
  path: string,
  data?: object,
) {
  const res = await page.request.fetch(`/papi${path}`, {
    method,
    ...(data ? { data } : {}),
    headers: { origin: APP_ORIGIN, "x-helix-csrf": await csrfToken(page) },
  });
  expect(res.status(), await res.text()).toBe(200);
}

test("on a dashboard's page, Founder's renames, pins and deletes show at once", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 50 });
  await signIn(page);
  const id = await makeDashboard(page, "Revenue");
  await page.goto(`/dashboards/${id}`);
  await expect(
    page.getByRole("heading", { level: 1, name: "Revenue" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Founder", exact: true }).click();
  const dock = dockOf(page);
  const run = async (text: string, card: string, button: string) => {
    await dock.getByRole("textbox", { name: "Message" }).fill(text);
    await dock.getByRole("button", { name: "Send", exact: true }).click();
    const action = dock.getByRole("region", { name: `Action: ${card}` });
    await action.getByRole("button", { name: button, exact: true }).click();
    await expect(action.getByText("Ran", { exact: true })).toBeVisible();
  };
  await run(
    "Rename it to Quarterly revenue",
    "Change the dashboard “Revenue”",
    "Save",
  );
  await expect(
    page.getByRole("heading", { level: 1, name: "Quarterly revenue" }),
  ).toBeVisible();
  await expect(
    page
      .getByRole("navigation", { name: "Dashboards" })
      .getByRole("link", { name: /Quarterly revenue/ }),
  ).toBeVisible();
  await run(
    "Pin this dashboard",
    "Pin the dashboard “Quarterly revenue” for everyone",
    "Pin",
  );
  await expect(page.getByRole("main").getByText("Pinned")).toBeVisible();
  await run(
    "Delete this dashboard",
    "Delete the dashboard “Quarterly revenue”",
    "Delete",
  );
  await expect(page).toHaveURL(/\/dashboards$/);
  await expect(
    page.getByRole("heading", { name: "No dashboards yet" }),
  ).toBeVisible();
});

test("an edit saved after someone else's change keeps theirs and says so", async ({
  page,
}) => {
  await signIn(page);
  const id = await makeDashboard(page, "Revenue");
  await page.goto(`/dashboards/${id}`);
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  // Another tab renames it, and this page reads it again (back in view).
  await changeOnWfe(page, "PATCH", `${D}/${id}`, {
    expectedRevision: "1",
    name: "Renamed elsewhere",
  });
  await page.waitForTimeout(5_500);
  await page.evaluate(() =>
    document.dispatchEvent(new Event("visibilitychange", { bubbles: true })),
  );
  await expect(
    page.getByRole("heading", { level: 1, name: "Editing Renamed elsewhere" }),
  ).toBeVisible();
  const head = page.getByRole("region", { name: "Name and description" });
  await head.getByLabel("Description (optional)").fill("Mine");
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(
    head.getByText("Someone changed this dashboard while you were editing it."),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Save changes" }),
  ).toBeDisabled();
  await head.getByRole("button", { name: "Discard my edits" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Renamed elsewhere" }),
  ).toBeVisible();
});

test("says why a tile shows nothing: no load, a failed read, a deleted ontology", async ({
  page,
}) => {
  await signIn(page);
  const id = await makeDashboard(page, "Health", [
    tile("t1", "Invoices by status", BY_STATUS, "bar"),
    tile("t2", "Nothing", {
      start: { type: "Nothing" },
      return: { count: {} },
    }),
    { ...tile("t3", "Renewals", CUSTOMERS), ontology_id: "ont_acme_renewals" },
  ]);
  await page.goto(`/dashboards/${id}`);
  const main = page.getByRole("main");
  await expect(
    main.getByRole("figure", { name: "Invoices by status" }),
  ).toContainText("Its ontology has nothing loaded yet.");

  await loadContracts(page);
  await changeOnWfe(
    page,
    "DELETE",
    "/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_renewals",
  );
  await page.goto(`/dashboards/${id}`);
  await expect(
    main
      .getByRole("figure", { name: "Invoices by status" })
      .locator(".recharts-bar-rectangle"),
  ).toHaveCount(5);
  await expect(main.getByRole("figure", { name: "Nothing" })).toContainText(
    "Couldn’t read it:",
  );
  await expect(main.getByRole("figure", { name: "Renewals" })).toContainText(
    "It reads an ontology that was deleted or isn’t shared with you.",
  );
  // A stale tile blocks no edit of the rest.
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await page
    .getByRole("region", { name: "Tile: Nothing" })
    .getByRole("button", { name: "Arrange Nothing" })
    .click();
  await page.getByRole("menuitem", { name: "Remove tile" }).click();
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByText("Dashboard saved.")).toBeVisible();
  await expect(main.getByRole("figure")).toHaveCount(2);
});

test("the share page says when its link is busy or failed", async ({
  page,
  browser,
}) => {
  await signIn(page);
  await loadContracts(page);
  const id = await makeDashboard(page, "Revenue");
  const token = (
    await postToWfe(page, `${D}/${id}:publish`, { expectedRevision: "1" })
  ).dashboard.share.token as string;
  const other = await browser.newContext();
  const pub = await other.newPage();
  let answer = 429;
  await pub.route("**/v1/shared-dashboards/**", (route) =>
    route.fulfill({
      status: answer,
      contentType: "application/json",
      body: JSON.stringify({ error: { code: "x", message: "x" } }),
    }),
  );
  await pub.goto(`/share/${token}`);
  await expect(
    pub.getByRole("heading", { name: "Too many tries from here" }),
  ).toBeVisible();
  await expect(pub.getByText("Made with Helix Foundry")).toHaveCount(0);
  answer = 500;
  await pub.reload();
  await expect(
    pub.getByRole("heading", { name: "Couldn’t open this dashboard" }),
  ).toBeVisible();
  await other.close();
});

test("with public links turned off, an owner still ends a link for good", async ({
  page,
  browser,
}) => {
  await signIn(page);
  await loadContracts(page);
  const id = await makeDashboard(page, "Revenue");
  const token = (
    await postToWfe(page, `${D}/${id}:publish`, { expectedRevision: "1" })
  ).dashboard.share.token as string;
  await mock.configure({ dashboardsPublic: false });
  await page.goto(`/dashboards/${id}`);
  await openPublicLink(page, "Manage public link…");
  const dialog = page.getByRole("dialog", { name: "Make a public link?" });
  await expect(dialog).toContainText(
    "Public links are turned off here, so its link shows nothing now.",
  );
  await expect(dialog.getByRole("button", { name: "Copy link" })).toHaveCount(
    0,
  );
  await dialog.getByRole("button", { name: "Unpublish" }).click();
  await expect(
    page.getByText(/^Unpublished: the link shows nothing now\./),
  ).toBeVisible();
  // Nothing left to share publicly while links are off: only the
  // workspace's link.
  await page.getByRole("button", { name: "Share", exact: true }).click();
  await expect(page.getByRole("menuitem", { name: "Copy link" })).toBeVisible();
  await expect(page.getByRole("menuitem", { name: /public link/ })).toHaveCount(
    0,
  );
  await page.keyboard.press("Escape");
  // Turned on again, the link stays down.
  await mock.configure({ dashboardsPublic: true });
  const other = await browser.newContext();
  const pub = await other.newPage();
  await pub.goto(`/share/${token}`);
  await expect(
    pub.getByRole("heading", { name: "There’s no dashboard at this link" }),
  ).toBeVisible();
  await other.close();
});

test("a member can't delete their dashboard once it is published", async ({
  page,
  browser,
}) => {
  await signIn(page);
  await loadContracts(page);
  const context = await browser.newContext();
  const alan = await context.newPage();
  await signIn(alan, "alan@example.com");
  const id = await makeDashboard(alan, "Alan’s numbers");
  await postToWfe(page, `${D}/${id}:publish`, { expectedRevision: "1" });
  await alan.goto(`/dashboards/${id}`);
  await expect(alan.getByText("Public link")).toBeVisible();
  // Theirs to edit, not to take down.
  await expect(
    alan.getByRole("button", { name: "Edit", exact: true }),
  ).toBeVisible();
  for (const name of ["More actions", "Public link", "Publish"])
    await expect(alan.getByRole("button", { name, exact: true })).toHaveCount(
      0,
    );
  await context.close();
});

test("Copy link shares a dashboard's page with the workspace, from its page and its card", async ({
  page,
}) => {
  await signIn(page);
  await loadContracts(page);
  const id = await makeDashboard(page, "Revenue");
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"]);
  const expected = `${APP_ORIGIN}/dashboards/${id}?workspace=ws_acme`;
  await page.goto(`/dashboards/${id}`);
  await page.getByRole("button", { name: "Share", exact: true }).click();
  const menu = page.getByRole("menu", { name: "Share" });
  await expect(menu).toContainText("Share in Acme");
  await expect(menu).toContainText("Anyone in Acme who can read its data");
  await menu.getByRole("menuitem", { name: "Copy link" }).click();
  await expect(
    page.getByText(
      "Link copied. Anyone in Acme who can read its data can open it.",
    ),
  ).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(
    expected,
  );
  // No snapshot is made: the dashboard has no public link.
  await expect(
    page.getByRole("main").getByText(/^Public link/),
  ).toHaveCount(0);

  await page.evaluate(() => navigator.clipboard.writeText(""));
  await page.goto("/dashboards");
  await page.getByRole("button", { name: "More actions for Revenue" }).click();
  await page.getByRole("menuitem", { name: "Copy link" }).click();
  await expect(page.getByText(/^Link copied\./)).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(
    expected,
  );
});

test("a member copies the workspace link but can't make a public link", async ({
  page,
  browser,
}) => {
  await signIn(page);
  await loadContracts(page);
  const context = await browser.newContext();
  const alan = await context.newPage();
  await signIn(alan, "alan@example.com");
  const id = await makeDashboard(alan, "Alan’s numbers");
  await alan.goto(`/dashboards/${id}`);
  await alan.getByRole("button", { name: "Share", exact: true }).click();
  await expect(
    alan.getByRole("menuitem", { name: "Copy link" }),
  ).toBeEnabled();
  await expect(
    alan.getByRole("menuitem", {
      name: "Only owners and admins make public links",
    }),
  ).toBeDisabled();
  await expect(
    alan.getByRole("menuitem", { name: "Make a public link…" }),
  ).toHaveCount(0);
  // Nor can it be done by hand.
  const refused = await alan.request.post(`/papi${D}/${id}:publish`, {
    data: { expectedRevision: "1" },
    headers: {
      origin: new URL(alan.url()).origin,
      "x-helix-csrf": await csrfToken(alan),
    },
  });
  expect(refused.status()).toBe(400);
  expect((await refused.json()).details?.[0]?.reason).toBe(
    "FOUNDRY_DASHBOARD_PUBLIC_ADMIN_ONLY",
  );
  await context.close();
});

test("a public link set to expire ends then, and publishing again makes a new one", async ({
  page,
  browser,
}) => {
  await signIn(page);
  await loadContracts(page);
  const id = await makeDashboard(page, "Revenue");
  await page.goto(`/dashboards/${id}`);
  await openPublicLink(page, "Make a public link…");
  let dialog = page.getByRole("dialog", { name: "Make a public link?" });
  await choose(dialog.getByRole("button", { name: "Link ends" }), "After 7 days");
  await dialog.getByRole("button", { name: "Publish", exact: true }).click();
  dialog = page.getByRole("dialog", { name: "Public link" });
  const link = await dialog.getByLabel("Link", { exact: true }).inputValue();
  await expect(dialog).toContainText(/The link ends on /);
  await dialog.getByRole("button", { name: "Close" }).click();
  await expect(
    page.getByRole("main").getByText(/^Public link · expires /),
  ).toBeVisible();

  const other = await browser.newContext();
  const pub = await other.newPage();
  await pub.goto(link);
  await expect(
    pub.getByRole("heading", { level: 1, name: "Revenue" }),
  ).toBeVisible();

  // Eight days on, it shows nothing, and the page says it expired.
  await mock.configure({ dashboardClockDays: 8 });
  await pub.reload();
  await expect(
    pub.getByRole("heading", { name: "There’s no dashboard at this link" }),
  ).toBeVisible();
  await page.reload();
  await expect(
    page.getByRole("main").getByText("Public link · expired", { exact: true }),
  ).toBeVisible();
  await openPublicLink(page, "Manage public link…");
  dialog = page.getByRole("dialog", { name: "Make a public link?" });
  await expect(dialog).toContainText("Publishing again makes a new link.");
  await dialog.getByRole("button", { name: "Publish a new link" }).click();
  dialog = page.getByRole("dialog", { name: "Public link" });
  const fresh = await dialog.getByLabel("Link", { exact: true }).inputValue();
  expect(fresh).not.toBe(link);
  await pub.reload();
  await expect(
    pub.getByRole("heading", { name: "There’s no dashboard at this link" }),
  ).toBeVisible();
  await other.close();
});

test("an owner turns public links off for the workspace: every link shows nothing", async ({
  page,
  browser,
}) => {
  await signIn(page);
  await loadContracts(page);
  const id = await makeDashboard(page, "Revenue");
  const other = await makeDashboard(page, "Pipeline");
  const token = (
    await postToWfe(page, `${D}/${id}:publish`, { expectedRevision: "1" })
  ).dashboard.share.token as string;

  await page.goto("/settings/sharing");
  const allow = page.getByRole("switch", {
    name: "Allow public dashboard links",
  });
  await expect(allow).toBeChecked();
  await allow.uncheck();
  await page
    .getByRole("dialog", { name: "Turn off public links?" })
    .getByRole("button", { name: "Turn off" })
    .click();
  await expect(page.getByText("Public links are off")).toBeVisible();
  await expect(allow).not.toBeChecked();

  const ctx = await browser.newContext();
  const pub = await ctx.newPage();
  await pub.goto(`/share/${token}`);
  await expect(
    pub.getByRole("heading", { name: "There’s no dashboard at this link" }),
  ).toBeVisible();

  await page.goto(`/dashboards/${id}`);
  await expect(
    page
      .getByRole("main")
      .getByText("Public link · turned off for this workspace", {
        exact: true,
      }),
  ).toBeVisible();
  await page.goto(`/dashboards/${other}`);
  await page.getByRole("button", { name: "Share", exact: true }).click();
  await expect(
    page.getByRole("menuitem", { name: "Turned off for this workspace" }),
  ).toBeDisabled();
  await page.keyboard.press("Escape");

  // On again, the link not ended meanwhile shows again.
  await page.goto("/settings/sharing");
  await page
    .getByRole("switch", { name: "Allow public dashboard links" })
    .check();
  await expect(page.getByText("Public links are on")).toBeVisible();
  await pub.reload();
  await expect(
    pub.getByRole("heading", { level: 1, name: "Revenue" }),
  ).toBeVisible();
  await ctx.close();
});
