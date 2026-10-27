import type { Locator, Page, Response } from "@playwright/test";
import {
  choose,
  csrfToken,
  dataTree as tree,
  expect,
  explorer,
  file,
  mock,
  signIn,
  test,
  uploadInData,
} from "./support/mock";

// The Data tree's layout, shared by the workspace: rows reordered by drag
// and from the grip's menu (Move up, Move down, Move to…), custom folders
// created, renamed and deleted with connections moved in and out, a kind's
// folder and documents/ renamed, all kept across reloads and pages. A write
// at a stale revision is made again on the fresh layout, once; members see
// the order without changing it; a search, a list that failed and a layout
// of a newer version turn arranging off.

const LAYOUT = "/papi/v1/workspaces/ws_acme/foundry/data-layout";
// The root folders' names, in order.
const roots = (page: Page) =>
  tree(page).locator(":scope > [role=treeitem] > .dt-row .dt-name");
// A folder (or set) of the tree, by its name, and a connection or document
// in it, by its name: a connection's row says its sync's status too.
const folder = (page: Page, name: string) =>
  tree(page).getByRole("treeitem", { name, exact: true });
const row = (folder: Locator, name: string) =>
  folder.getByRole("treeitem", { name: new RegExp(`^${name}(,|$)`) });
const grips = (page: Page) =>
  tree(page).getByRole("button", { name: / actions$/ });
const isPut = (r: Response) =>
  r.request().method() === "PUT" && new URL(r.url()).pathname === LAYOUT;
// The next layout write the page sees answered.
const saved = (page: Page, status = 200) =>
  page.waitForResponse((r) => isPut(r) && r.status() === status);
const puts = async () =>
  (await mock.stats()).requests.filter((r) =>
    r.startsWith("PUT /v1/workspaces/ws_acme/foundry/data-layout"),
  );

async function menu(page: Page, label: string, item: string) {
  await page.getByRole("button", { name: label, exact: true }).click();
  await page.getByRole("menuitem", { name: item }).click();
}

// Drags a row by its grip onto `target`: its middle, or near its top. The
// grip starts mid-screen, away from the edges where dnd-kit scrolls the
// page during a drag.
async function drag(
  page: Page,
  grip: Locator,
  target: Locator,
  at: "middle" | "top" = "middle",
) {
  await grip.evaluate((el) => el.scrollIntoView({ block: "center" }));
  const from = (await grip.boundingBox())!,
    to = (await target.boundingBox())!;
  const x = from.x + from.width / 2,
    y = from.y + from.height / 2;
  await page.mouse.move(x, y);
  await page.mouse.down();
  await page.mouse.move(x, y + 8, { steps: 4 });
  await page.mouse.move(
    to.x + 60,
    at === "top" ? to.y + 6 : to.y + to.height / 2,
    { steps: 12 },
  );
  await page.mouse.up();
  // dnd-kit swallows clicks for 50 ms after a drop, so a click on the row
  // isn't taken for the drag's end; a person can't click that fast.
  await page.waitForTimeout(100);
}

async function newFolder(page: Page, name: string) {
  await page.getByRole("button", { name: "New folder" }).click();
  const dialog = page.getByRole("dialog", { name: "New folder" });
  await dialog.getByLabel("Name").fill(name);
  const put = saved(page);
  await dialog.getByRole("button", { name: "Create folder" }).click();
  await put;
}

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

test("reorders by drag and from the keyboard, and keeps the order", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  await addPostgres(page, "Billing replica");
  await page.reload();
  const postgres = folder(page, "PostgreSQL");
  await expect(postgres.getByRole("link")).toHaveText([
    "Shop database",
    "Billing replica",
  ]);

  // Dragged onto Shop database's place, it goes before it.
  let put = saved(page);
  await drag(
    page,
    page.getByRole("button", { name: "Billing replica actions" }),
    row(postgres, "Shop database"),
  );
  await put;
  await expect(postgres.getByRole("link")).toHaveText([
    "Billing replica",
    "Shop database",
  ]);

  // From the keyboard: the grip opens its menu, Move up moves documents/,
  // and the focus stays on its grip for the next move.
  const documents = page.getByRole("button", {
    name: "Documents folder actions",
  });
  await documents.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("menuitem", { name: "Move up" })).toBeFocused();
  put = saved(page);
  await page.keyboard.press("Enter");
  await put;
  await expect(roots(page)).toHaveText(["Stripe", "Documents", "PostgreSQL"]);
  await expect(documents).toBeFocused();
  await page.keyboard.press("Enter");
  put = saved(page);
  await page.keyboard.press("Enter");
  await put;
  await expect(roots(page)).toHaveText(["Documents", "Stripe", "PostgreSQL"]);
  await expect(documents).toBeFocused();
  // First, it can't go up: the menu starts on Move down.
  await page.keyboard.press("Enter");
  await expect(page.getByRole("menuitem", { name: "Move up" })).toHaveAttribute(
    "aria-disabled",
    "true",
  );
  await expect(page.getByRole("menuitem", { name: "Move down" })).toBeFocused();
  // Move down on a connection: a place down in its folder.
  await page.keyboard.press("Escape");
  put = saved(page);
  await menu(page, "Billing replica actions", "Move down");
  await put;
  await expect(postgres.getByRole("link")).toHaveText([
    "Shop database",
    "Billing replica",
  ]);

  await page.reload();
  await expect(roots(page)).toHaveText(["Documents", "Stripe", "PostgreSQL"]);
  await expect(postgres.getByRole("link")).toHaveText([
    "Shop database",
    "Billing replica",
  ]);
  expect(await puts()).toEqual(
    Array(4).fill("PUT /v1/workspaces/ws_acme/foundry/data-layout 200"),
  );
});

test("creates, renames and deletes a folder, and moves connections in and out", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  await page.getByRole("button", { name: "New folder" }).click();
  const dialog = page.getByRole("dialog", { name: "New folder" });
  // Another root folder's name, case aside, is taken.
  await dialog.getByLabel("Name").fill(" stripe ");
  await expect(
    dialog.getByText("There’s already a folder called Stripe."),
  ).toBeVisible();
  await expect(
    dialog.getByRole("button", { name: "Create folder" }),
  ).toBeDisabled();
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await newFolder(page, "Finance");
  await expect(roots(page)).toHaveText([
    "Stripe",
    "PostgreSQL",
    "Finance",
    "Documents",
  ]);
  const finance = folder(page, "Finance");
  await expect(finance).toContainText("Empty.");

  // From the menu: Move to… a folder.
  await menu(page, "Stripe actions", "Move to…");
  const move = page.getByRole("dialog", { name: "Move Stripe" });
  const to = move.getByRole("combobox", { name: "Folder" });
  await expect(to).toHaveText("Finance");
  await to.click();
  await expect(page.getByRole("listbox").getByRole("option")).toHaveText([
    "Finance",
  ]);
  await page.keyboard.press("Escape");
  let put = saved(page);
  await move.getByRole("button", { name: "Move" }).click();
  await put;
  // Its kind's folder, empty, goes.
  await expect(roots(page)).toHaveText(["PostgreSQL", "Finance", "Documents"]);
  await expect(finance.getByRole("link")).toHaveText(["Stripe"]);

  // By drag: dropped on the top half of a row in another folder, it goes
  // before that row.
  put = saved(page);
  await drag(
    page,
    page.getByRole("button", { name: "Shop database actions" }),
    row(finance, "Stripe"),
    "top",
  );
  await put;
  await expect(finance.getByRole("link")).toHaveText([
    "Shop database",
    "Stripe",
  ]);
  await expect(roots(page)).toHaveText(["Finance", "Documents"]);

  // Renamed in place.
  await page.getByRole("button", { name: "Rename Finance folder" }).click();
  await page
    .getByRole("textbox", { name: "Name for Finance folder" })
    .fill("Money");
  put = saved(page);
  await page.keyboard.press("Enter");
  await put;
  await page.reload();
  await expect(roots(page)).toHaveText(["Money", "Documents"]);
  const money = folder(page, "Money");
  await expect(money.getByRole("link")).toHaveText(["Shop database", "Stripe"]);

  // Back to its kind's folder from the menu.
  await menu(page, "Stripe actions", "Move to…");
  await expect(to).toHaveText("Stripe");
  await to.click();
  await expect(page.getByRole("listbox").getByRole("option")).toHaveText([
    "Stripe",
  ]);
  await page.keyboard.press("Escape");
  put = saved(page);
  await move.getByRole("button", { name: "Move" }).click();
  await put;
  await expect(roots(page)).toHaveText(["Stripe", "Money", "Documents"]);

  // Deleting a folder sends its connections back to their kinds' folders.
  await menu(page, "Money folder actions", "Delete folder");
  const confirm = page.getByRole("dialog", { name: "Delete Money?" });
  await expect(confirm).toContainText(
    "Its connection goes back to its kind’s folder.",
  );
  put = saved(page);
  await confirm.getByRole("button", { name: "Delete folder" }).click();
  await put;
  await expect(roots(page)).toHaveText(["Stripe", "PostgreSQL", "Documents"]);
  // An empty one goes without asking.
  await newFolder(page, "Spare");
  put = saved(page);
  await menu(page, "Spare folder actions", "Delete folder");
  await put;
  await page.reload();
  await expect(roots(page)).toHaveText(["Stripe", "PostgreSQL", "Documents"]);
  await expect(folder(page, "PostgreSQL").getByRole("link")).toHaveText([
    "Shop database",
  ]);
});

test("renames a kind's folder and documents/ in place, and back", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  const rename = async (from: string, to: string) => {
    await page.getByRole("button", { name: `Rename ${from} folder` }).click();
    await page
      .getByRole("textbox", { name: `Name for ${from} folder` })
      .fill(to);
    await page.keyboard.press("Enter");
  };
  let put = saved(page);
  await rename("PostgreSQL", "Databases");
  await put;
  // A name another root folder has is refused, and nothing is saved.
  await rename("Documents", "stripe");
  await expect(page.locator(".hf-toast")).toContainText(
    "There’s already a folder called Stripe.",
  );
  put = saved(page);
  await rename("Documents", "Files");
  await put;
  await expect(roots(page)).toHaveText(["Stripe", "Databases", "Files"]);
  await page.reload();
  await expect(roots(page)).toHaveText(["Stripe", "Databases", "Files"]);
  await expect(folder(page, "Databases").getByRole("link")).toHaveText([
    "Shop database",
  ]);
  // The filter finds a folder by its new name.
  await page.getByRole("searchbox", { name: "Filter data" }).fill("databa");
  await expect(roots(page)).toHaveText(["Databases"]);
  await page.getByRole("searchbox", { name: "Filter data" }).fill("");

  // Its usual name back: the layout forgets it.
  put = saved(page);
  await rename("Databases", "PostgreSQL");
  await put;
  const stored = await (await page.request.get(LAYOUT)).json();
  expect(stored.layout.names).toEqual({ documents: "Files" });
  expect(await puts()).toHaveLength(3);
});

test("shows a page another's order after a refetch, and makes a change again on a stale revision", async ({
  page,
  context,
}) => {
  await signIn(page);
  await page.goto("/data");
  const other = await context.newPage();
  await other.goto("/data");
  await expect(roots(other)).toHaveText(["Stripe", "PostgreSQL", "Documents"]);

  // The first page moves documents/ to the top.
  for (let i = 0; i < 2; i++) {
    const put = saved(page);
    await menu(page, "Documents folder actions", "Move up");
    await put;
  }
  await expect(roots(page)).toHaveText(["Documents", "Stripe", "PostgreSQL"]);

  // The second still shows what it read. Its move is refused at the
  // revision it read, then made again on the first page's layout.
  await expect(roots(other)).toHaveText(["Stripe", "PostgreSQL", "Documents"]);
  const refused = saved(other, 400),
    made = saved(other);
  await menu(other, "PostgreSQL folder actions", "Move up");
  await refused;
  await made;
  await expect(roots(other)).toHaveText(["Documents", "PostgreSQL", "Stripe"]);

  // The first page sees both after a refetch.
  await page.reload();
  await expect(roots(page)).toHaveText(["Documents", "PostgreSQL", "Stripe"]);

  // Refused again on the fresh revision: the move is undone and a toast
  // says so.
  await mock.configure({ dataLayoutConflicts: 2 });
  await menu(page, "Stripe folder actions", "Move up");
  await expect(page.locator(".hf-toast")).toContainText(
    "Couldn’t save the Connectors tree: someone else changed it at the same time. Try again.",
  );
  await expect(roots(page)).toHaveText(["Documents", "PostgreSQL", "Stripe"]);
  expect((await puts()).map((r) => r.split(" ").at(-1))).toEqual([
    "200",
    "200",
    "400",
    "200",
    "400",
    "400",
  ]);
});

test("orders sets and documents, and moves nothing while a search is on", async ({
  page,
}) => {
  await signIn(page);
  // One at a time, so a.md is the older.
  await uploadInData(page, [file("a.md", "# A")]);
  await uploadInData(page, [file("b.md", "# B")]);
  await page.getByRole("button", { name: "New set" }).click();
  const create = page.getByRole("dialog", { name: "New document set" });
  await create.getByLabel("Name").fill("Board");
  await create.getByRole("button", { name: "Create set" }).click();
  const setNames = tree(page).locator(
    "[data-row=documents] > [role=group] > [role=treeitem] > .dt-row .dt-name",
  );
  await expect(setNames).toHaveText(["General", "Board"]);

  let put = saved(page);
  await menu(page, "Board set actions", "Move up");
  await put;
  await expect(setNames).toHaveText(["Board", "General"]);

  const general = folder(page, "General");
  await expect(general.getByRole("link")).toHaveText(["a.md", "b.md"]);
  put = saved(page);
  await drag(
    page,
    page.getByRole("button", { name: "b.md actions" }),
    row(general, "a.md"),
  );
  await put;
  await expect(general.getByRole("link")).toHaveText(["b.md", "a.md"]);

  // Another set is the WFE's move, from the same menu.
  await menu(page, "a.md actions", "Move to another set…");
  const move = page.getByRole("dialog", { name: "Move a.md" });
  await choose(move.getByLabel("Set"), "Board");
  await move.getByRole("button", { name: "Move" }).click();
  await expect(page.locator(".hf-toast")).toContainText("Moved a.md to Board.");
  await expect(folder(page, "Board").getByRole("link")).toHaveText(["a.md"]);

  await page.reload();
  await expect(setNames).toHaveText(["Board", "General"]);
  await expect(general.getByRole("link")).toHaveText(["b.md"]);

  // A filter shows what it finds without grips; clearing it brings them
  // back.
  const search = page.getByRole("searchbox", { name: "Filter data" });
  await search.fill(".md");
  await expect(tree(page).getByRole("link")).toHaveText(["a.md", "b.md"]);
  await expect(grips(page)).toHaveCount(0);
  await search.fill("");
  await expect(
    page.getByRole("button", { name: "b.md actions" }),
  ).toBeAttached();
});

test("shows a member the workspace's order without letting them change it", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  await newFolder(page, "Finance");
  // Dropped on a folder's head, a connection goes last in it.
  let put = saved(page);
  await drag(
    page,
    page.getByRole("button", { name: "Stripe actions", exact: true }),
    folder(page, "Finance").locator(":scope > .dt-row"),
  );
  await put;
  put = saved(page);
  await menu(page, "Documents folder actions", "Move up");
  await put;
  await expect(roots(page)).toHaveText(["PostgreSQL", "Documents", "Finance"]);

  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto("/data");
  await expect(roots(page)).toHaveText(["PostgreSQL", "Documents", "Finance"]);
  await expect(folder(page, "Finance").getByRole("link")).toHaveText([
    "Stripe",
  ]);
  await expect(grips(page)).toHaveCount(0);
  await expect(tree(page).getByRole("button", { name: /^Rename/ })).toHaveCount(
    0,
  );
  await expect(page.getByRole("button", { name: "New folder" })).toHaveCount(0);
  // The WFE refuses a member's write.
  const refused = await page.request.put(LAYOUT, {
    headers: {
      origin: new URL(page.url()).origin,
      "x-helix-csrf": await csrfToken(page),
    },
    data: { layout: {}, expectedRevision: "3" },
  });
  expect(refused.status()).toBe(403);
});

test("arranges nothing while a list failed, or for a layout of a newer version", async ({
  page,
}) => {
  await signIn(page);
  // The documents list fails: the tree shows, and nothing moves.
  await page.route("**/papi/v1/workspaces/ws_acme/foundry/documents", (r) =>
    r.fulfill({
      status: 503,
      contentType: "application/json",
      body: JSON.stringify({ code: 14, message: "unavailable", details: [] }),
    }),
  );
  await page.goto("/data");
  await expect(
    explorer(page)
      .getByRole("alert")
      .filter({ hasText: "Couldn’t load the documents" }),
  ).toBeVisible();
  await expect(roots(page)).toHaveText(["Stripe", "PostgreSQL", "Documents"]);
  await expect(grips(page)).toHaveCount(0);
  await expect(page.getByRole("button", { name: "New folder" })).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Rename Stripe folder" }),
  ).toHaveCount(0);
  await page.unroute("**/papi/v1/workspaces/ws_acme/foundry/documents");
  await page.reload();
  await expect(grips(page).first()).toBeAttached();

  // Another version of the site wrote the layout: it is left alone.
  const written = await page.request.put(LAYOUT, {
    headers: {
      origin: new URL(page.url()).origin,
      "x-helix-csrf": await csrfToken(page),
    },
    data: {
      layout: { v: 2, order: { root: ["documents"] } },
      expectedRevision: "0",
    },
  });
  expect(written.status()).toBe(200);
  await page.reload();
  await expect(
    page
      .getByRole("status")
      .filter({ hasText: "A newer version of Foundry arranged this tree" }),
  ).toBeVisible();
  await expect(roots(page)).toHaveText(["Stripe", "PostgreSQL", "Documents"]);
  await expect(grips(page)).toHaveCount(0);
  await expect(page.getByRole("button", { name: "New folder" })).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Rename Stripe folder" }),
  ).toHaveCount(0);
  // A connection still renames: its name is the WFE's, not the layout's.
  await expect(
    page.getByRole("button", { name: "Rename Shop database" }),
  ).toBeAttached();
  expect(await puts()).toEqual([
    "PUT /v1/workspaces/ws_acme/foundry/data-layout 200",
  ]);
});
