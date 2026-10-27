import type { Page } from "@playwright/test";
import {
  dataTree,
  expect,
  explorer,
  file,
  signIn,
  test,
  uploadInData,
} from "./support/mock";

// The Data explorer beside every Data page, as an IDE's: a tree that is
// one Tab stop, walked with the arrow keys (Enter opens a row, F2 renames
// it, Tab goes on to its buttons), and the row of the page open marked as
// the URL changes, with the folders that hold it opened. A connection opens
// on its tables and its own sets, whose documents page in 50 at a time.

const folder = (page: Page, name: string) =>
  dataTree(page).getByRole("treeitem", { name, exact: true });
// A connection's row: its name says its sync's status too.
const connection = (page: Page, name: string) =>
  dataTree(page).getByRole("treeitem", { name: `${name}, Synced` });
const current = (page: Page) => dataTree(page).locator("[aria-current=page]");

test("walks the tree with the keyboard", async ({ page }) => {
  await signIn(page);
  await page.goto("/data");
  const stripe = folder(page, "Stripe"),
    shop = connection(page, "Shop database");
  // One Tab stop: the first row while no row's page is open.
  await explorer(page).getByRole("searchbox", { name: "Filter data" }).focus();
  await page.keyboard.press("Tab");
  await expect(stripe).toBeFocused();
  await expect(
    dataTree(page).locator("[role=treeitem][tabindex='0']"),
  ).toHaveCount(1);

  // Down into a folder; Left to the folder, then Left closes it and Right
  // opens it again; Right then moves in.
  await page.keyboard.press("ArrowDown");
  await expect(connection(page, "Stripe")).toBeFocused();
  await page.keyboard.press("ArrowLeft");
  await expect(stripe).toBeFocused();
  await page.keyboard.press("ArrowLeft");
  await expect(stripe).toHaveAttribute("aria-expanded", "false");
  await expect(connection(page, "Stripe")).toHaveCount(0);
  await page.keyboard.press("ArrowRight");
  await expect(stripe).toHaveAttribute("aria-expanded", "true");
  await page.keyboard.press("ArrowRight");
  await expect(connection(page, "Stripe")).toBeFocused();
  // End and Home: the last row (General, in documents/) and the first.
  await page.keyboard.press("End");
  await expect(folder(page, "General")).toBeFocused();
  await expect(folder(page, "General")).toHaveAttribute("aria-level", "2");
  await page.keyboard.press("Home");
  await expect(stripe).toBeFocused();
  // Enter on a folder closes it, and opens it again.
  await page.keyboard.press("Enter");
  await expect(stripe).toHaveAttribute("aria-expanded", "false");
  await page.keyboard.press("Enter");
  await expect(stripe).toHaveAttribute("aria-expanded", "true");

  // Enter on a connection opens its page; the focus stays on its row.
  for (let i = 0; i < 3; i++) await page.keyboard.press("ArrowDown");
  await expect(shop).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(
    page.getByRole("heading", { level: 1, name: "Shop database" }),
  ).toBeVisible();
  await expect(shop).toBeFocused();
  await expect(shop).toHaveAttribute("aria-current", "page");

  // F2 renames it; Escape keeps the name and gives the focus back.
  await page.keyboard.press("F2");
  const field = page.getByRole("textbox", { name: "Name for Shop database" });
  await expect(field).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(field).toHaveCount(0);
  await expect(shop).toBeFocused();
  // Tab goes on to its buttons, which show while they have the focus, and
  // Shift+Tab comes back.
  await page.keyboard.press("Tab");
  await expect(
    page.getByRole("button", { name: "Shop database actions" }),
  ).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(
    page.getByRole("button", { name: "Rename Shop database" }),
  ).toBeFocused();
  await page.keyboard.press("Tab");
  const sync = page.getByRole("button", { name: "Sync Shop database" });
  await expect(sync).toBeFocused();
  await expect(sync.locator("xpath=..")).toHaveCSS("opacity", "1");
  for (let i = 0; i < 3; i++) await page.keyboard.press("Shift+Tab");
  await expect(shop).toBeFocused();

  // The row of the page open takes the Tab stop once the tree has had none.
  await page.reload();
  await explorer(page).getByRole("searchbox", { name: "Filter data" }).focus();
  await page.keyboard.press("Tab");
  await expect(shop).toBeFocused();
});

test("marks the row of the page open as the URL changes", async ({ page }) => {
  await signIn(page);
  await uploadInData(page, [file("notes.md", "# Notes\n\nHooli called.\n")]);
  const shop = connection(page, "Shop database"),
    notes = folder(page, "notes.md");
  // The Data page opens no row.
  await expect(current(page)).toHaveCount(0);

  // A connection's page marks its row; a table's page marks the table's,
  // the connection opened on it.
  await expect(shop).toHaveAttribute("aria-expanded", "false");
  await shop.getByRole("link").click();
  await expect(page).toHaveURL(/\/data\/connections\/conn_acme_shop$/);
  await expect(shop).toHaveAttribute("aria-current", "page");
  await page
    .getByRole("list", { name: "Synced tables" })
    .getByRole("link")
    .first()
    .click();
  await expect(page).toHaveURL(/\/conn_acme_shop\/tables\/accounts$/);
  await expect(shop).toHaveAttribute("aria-expanded", "true");
  await expect(current(page)).toHaveAccessibleName("accounts");
  // A document's page marks its row instead.
  await notes.getByRole("link").click();
  await expect(
    page.getByRole("heading", { level: 1, name: "notes.md" }),
  ).toBeVisible();
  await expect(notes).toHaveAttribute("aria-current", "page");
  await expect(current(page)).toHaveCount(1);
  // Back in history, the table again.
  await page.goBack();
  await expect(current(page)).toHaveAccessibleName("accounts");
  await expect(notes).not.toHaveAttribute("aria-current");

  // While the filter finds a table, its page marks the table's row.
  const filter = explorer(page).getByRole("searchbox", { name: "Filter data" });
  await filter.fill("order_it");
  await dataTree(page).getByRole("link", { name: "order_items" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "order_items" }),
  ).toBeVisible();
  await expect(current(page)).toHaveAccessibleName("order_items");
  await filter.fill("");
  await expect(current(page)).toHaveAccessibleName("order_items");

  // A page opened with its folders closed opens them, once: closed again,
  // they stay closed on that page.
  const postgres = folder(page, "PostgreSQL"),
    documents = folder(page, "Documents");
  await postgres.locator(":scope > .dt-row .dt-chevron").click();
  await documents.locator(":scope > .dt-row .dt-chevron").click();
  await expect(postgres).toHaveAttribute("aria-expanded", "false");
  await expect(documents).toHaveAttribute("aria-expanded", "false");
  await page.goto("/data/connections/conn_acme_shop");
  await expect(shop).toHaveAttribute("aria-current", "page");
  await expect(postgres).toHaveAttribute("aria-expanded", "true");
  await expect(documents).toHaveAttribute("aria-expanded", "false");
  await postgres.locator(":scope > .dt-row .dt-chevron").click();
  await expect(postgres).toHaveAttribute("aria-expanded", "false");
  await expect(shop).toHaveCount(0);
  // Opened from the Data page's grid, its row shows again.
  await explorer(page).getByRole("link", { name: "Add data" }).click();
  await expect(page).toHaveURL(/\/data\?add=1$/);
  const picker = page.getByRole("dialog", { name: "Where is your data?" });
  await expect(picker).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(picker).toHaveCount(0);
  await page
    .getByRole("grid", { name: "Connections" })
    .getByRole("link", { name: "Shop database" })
    .click();
  await expect(postgres).toHaveAttribute("aria-expanded", "true");
  await expect(shop).toHaveAttribute("aria-current", "page");
});

test("opens a connection on its tables and its own sets, 50 documents at a time", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data?add=granola");
  const form = page.getByRole("dialog", { name: "Granola" });
  await form.getByLabel("API key").fill("grn_e2eTeamGranolaKey");
  await form.getByRole("button", { name: "Test connection" }).click();
  await form.getByRole("button", { name: "Connect and sync" }).click();
  // Its sync writes each note into a set of its own.
  const granola = connection(page, "Granola");
  await expect(granola).toBeVisible({ timeout: 15_000 });
  await expect(granola).toHaveAttribute("aria-expanded", "false");
  await granola.locator(":scope > .dt-row .dt-toggle").click();
  await expect(granola).toHaveAttribute("aria-expanded", "true");
  const rows = granola.locator(":scope > [role=group] > [role=treeitem]");
  await expect(rows).toHaveCount(6);
  const notes = folder(page, "Notes");
  await expect(rows.last()).toHaveAccessibleName("Notes");
  await expect(notes).toHaveAttribute("aria-level", "3");
  await expect(notes.locator(":scope > .dt-row .dt-count")).toHaveText("418");
  await expect(notes).toHaveAttribute("aria-expanded", "false");
  // documents/ holds the sets people make.
  await expect(
    folder(page, "Documents").getByRole("treeitem", { name: "Notes" }),
  ).toHaveCount(0);

  // Opened, the set reads its newest 50; Show more reads 50 more.
  await notes.locator(":scope > .dt-row").click();
  await expect(notes).toHaveAttribute("aria-expanded", "true");
  const documents = notes.getByRole("link");
  await expect(documents).toHaveCount(50);
  await expect(documents.first()).toHaveText("Weekly pipeline review");
  await notes.getByRole("button", { name: "Show more" }).click();
  await expect(documents).toHaveCount(100);
  // The arrow keys walk into the documents read.
  await notes.focus();
  await page.keyboard.press("ArrowRight");
  await expect(notes.getByRole("treeitem").first()).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(notes.getByRole("treeitem").nth(1)).toBeFocused();

  // A document opens its page, and its row is the page's; reloaded, the
  // connection and its set open on it again.
  await documents.nth(1).click();
  await expect(page).toHaveURL(/\/data\/documents\/[^/]+$/);
  await expect(current(page)).toHaveAccessibleName("Globex renewal call");
  await granola.locator(":scope > .dt-row .dt-toggle").click();
  await expect(granola).toHaveAttribute("aria-expanded", "false");
  await page.reload();
  await expect(granola).toHaveAttribute("aria-expanded", "true");
  await expect(current(page)).toHaveAccessibleName("Globex renewal call");
});
