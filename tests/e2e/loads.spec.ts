import { readFileSync } from "node:fs";
import type { Page } from "@playwright/test";
import {
  choose,
  expect,
  expectGridFits,
  explorer,
  mock,
  signIn,
  test,
} from "./support/mock";

// Versioned loads (the load-versioning plan, section 3): each load writes a
// full copy of a version beside the others; pages read one load (the
// current by default, `?load=` otherwise), compare two side by side, and
// the Loads page makes one current or deletes one. The first view of a load
// is seeded from its hub types into a connected graph.

const O = "/ontology/ont_acme_contracts",
  X = "/explore/ont_acme_contracts";
const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
);

async function makeContractsCurrent(page: Page) {
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
}

/** Loads version 3 from the Loads page and waits for the end; the load's id. */
async function loadVersion3(page: Page): Promise<string> {
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
  return new URL(page.url()).pathname.split("/").pop()!;
}

async function uploadContract(page: Page) {
  await page.goto("/data");
  await explorer(page).getByLabel("Choose documents").setInputFiles({
    name: "contract.md",
    mimeType: "text/markdown",
    buffer: CONTRACT,
  });
  await expect(
    page.getByRole("list", { name: "Uploads" }).getByRole("listitem").first(),
  ).toContainText("Uploaded");
}

/**
 * Two loads of version 3: the first couldn't read the contract, the second
 * did, so the second holds a Contract the first doesn't. The second is
 * current.
 */
async function twoLoads(page: Page) {
  await mock.configure({ loadStepMs: 100, documentOutcome: "fail" });
  await signIn(page);
  await makeContractsCurrent(page);
  await uploadContract(page);
  const first = await loadVersion3(page);
  await mock.configure({ documentOutcome: "succeed" });
  const second = await loadVersion3(page);
  return { first, second };
}

const graph = (page: Page) =>
  page.getByRole("application", { name: /^Graph of contracts: \d+ nodes/ });
const loadsGrid = (page: Page) => page.getByRole("grid", { name: "Loads" });
const loadRow = (page: Page, id: string) =>
  loadsGrid(page)
    .getByRole("row")
    .filter({ has: page.locator(`a[href$="/loads/${id}"]`) });

test("picks a load: the Explorer, Records, an entity's page and a document's view read it", async ({
  page,
}) => {
  const { first } = await twoLoads(page);
  await page.goto(X);
  const picker = page.getByRole("combobox", { name: "Load" }).first();
  // The current load by default; the other is named by its version and date.
  await expect(picker).toHaveText("Current · v3");
  await expect(graph(page)).toBeVisible({ timeout: 15_000 });
  const rail = page.getByRole("complementary", { name: "Labels" });
  await expect(rail).toContainText("Contract");

  // The older load: in the URL, without the contract it couldn't read.
  await choose(picker, /^v3 · /);
  await expect(page).toHaveURL(new RegExp(`load=${first}`));
  await expect(graph(page)).toBeVisible({ timeout: 15_000 });
  await expect(rail).not.toContainText("Contract");
  await expect(rail).toContainText("Customer");

  // Records reads the same load, and a record's page keeps it.
  await page.getByRole("radio", { name: "Records" }).click();
  await expect(page).toHaveURL(new RegExp(`/records\\?load=${first}`));
  const types = page.getByRole("list", { name: "Types" });
  await expect(types.getByRole("button")).toHaveText([
    "Contract0",
    "Customer64",
    "Invoice65",
  ]);
  await expect(page.getByRole("combobox", { name: "Load" }).first()).toHaveText(
    /^v3 · /,
  );
  await types.getByRole("button", { name: /^Customer/ }).click();
  await page
    .getByRole("grid", { name: "Customer records" })
    .getByRole("row")
    .nth(1)
    .click();
  await page.getByRole("link", { name: "Open its page" }).click();
  await expect(page).toHaveURL(new RegExp(`/entities/[^?]+\\?load=${first}`));
  await expect(page.getByRole("combobox", { name: "Load" }).first()).toHaveText(
    /^v3 · /,
  );
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "Globex Corporation",
  );

  // A document's view: the current load read the contract, the first didn't.
  await page.goto(`${O}/documents`);
  await page.getByRole("link", { name: "contract.md" }).first().click();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "contract.md",
  );
  const docPicker = page.getByRole("combobox", { name: "Load" }).first();
  await expect(docPicker).toHaveText("Current · v3");
  await expect(page.locator(".dc-doc-grid")).toContainText("Globex");
  await choose(docPicker, /^v3 · /);
  await expect(page.getByText("This load didn’t read it")).toBeVisible();
  await page.getByRole("button", { name: "Show the current load" }).click();
  await expect(docPicker).toHaveText("Current · v3");
  // Other data holds no documents: it says so rather than blame a load.
  await choose(docPicker, "Other data");
  await expect(page.getByText("Documents are read into loads")).toBeVisible();
  await expect(page.getByText("This load didn’t read it")).toHaveCount(0);
  await page.getByRole("button", { name: "Show the current load" }).click();
  await expect(docPicker).toHaveText("Current · v3");

  await page.goto(`${X}/records?load=${first}`);

  // Other data: nothing in this database was written outside a load.
  await choose(
    page.getByRole("combobox", { name: "Load" }).first(),
    "Other data",
  );
  await expect(page).toHaveURL(/load=other/);
  await page.getByRole("radio", { name: "Graph" }).click();
  await expect(page).toHaveURL(/explore\/ont_acme_contracts\?load=other/);
  await expect(page.getByText("No other data")).toBeVisible({
    timeout: 15_000,
  });

  // A deleted or unknown load says so and offers the current one.
  await page.goto(`${X}?load=load_gone`);
  await expect(page.getByText("This load was deleted")).toBeVisible();
  await page.getByRole("button", { name: /Show the current load/ }).click();
  await expect(page).not.toHaveURL(/load=/);
  await expect(graph(page)).toBeVisible({ timeout: 15_000 });
});

test("says when the loads or Other data's versions can't be read, and retries", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 100 });
  await signIn(page);
  await makeContractsCurrent(page);
  await loadVersion3(page);
  const down = (route: { fulfill: (r: object) => Promise<void> }) =>
    route.fulfill({
      status: 503,
      contentType: "application/json",
      body: JSON.stringify({ code: 14, message: "upstream connect error" }),
    });

  // Without the loads list nothing is read, and the page says so.
  const loadsRoute = "**/foundry/ontologies/ont_acme_contracts/loads";
  await page.route(loadsRoute, down);
  await page.goto(`${X}/records`);
  await expect(page.getByText("Couldn’t load the loads")).toBeVisible({
    timeout: 10_000,
  });
  await page.unroute(loadsRoute);
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(page.getByRole("list", { name: "Types" })).toBeVisible();

  // Other data needs every load's version: without one it reads nothing,
  // rather than take that load's entities for the team's own.
  const versionRoute =
    "**/foundry/ontologies/ont_acme_contracts/versions/ver_seed_3";
  await page.route(versionRoute, down);
  await page.goto(`${X}?load=other`);
  await expect(
    page.getByText("Couldn’t read the versions of the loads"),
  ).toBeVisible({ timeout: 10_000 });
  await expect(graph(page)).toHaveCount(0);
  await page.unroute(versionRoute);
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(page.getByText("No other data")).toBeVisible({
    timeout: 15_000,
  });
});

test("compares two loads side by side over their diff", async ({ page }) => {
  const { first, second } = await twoLoads(page);
  await page.goto(`${O}#loads`);
  await loadRow(page, first)
    .getByRole("button", { name: "More actions for v3" })
    .click();
  await page.getByRole("menuitem", { name: "Compare with current" }).click();
  await expect(page).toHaveURL(
    new RegExp(`${X}\\?load=${first}&compare=${second}`),
  );
  // Two canvases, one per load.
  await expect(graph(page)).toHaveCount(2, { timeout: 15_000 });
  const diff = page.getByRole("table", { name: "Counts per type and link" });
  await expect(diff).toBeVisible({ timeout: 15_000 });
  const contract = page.getByTestId("diff-row-Contract");
  await expect(contract).toContainText("0");
  await expect(contract).toContainText("1");
  await expect(contract).toContainText("+1");
  await expect(page.getByTestId("diff-row-Customer")).toContainText("64");
  // Opening a type compares its entities by key.
  await contract.getByRole("button", { name: /Contract/ }).click();
  const keys = page.getByTestId("diff-keys-Contract");
  await expect(keys).toContainText("Added in v3 · 1");
  await expect(keys).toContainText("Removed since v3 · 0");
  await keys.getByRole("button", { name: "master services agreement" }).click();
  // The entity is selected on the side that has it.
  await expect(
    page
      .getByRole("complementary", { name: "Inspector" })
      .filter({ hasText: "Master services agreement" })
      .first(),
  ).toBeVisible({ timeout: 15_000 });
  // Comparing nothing goes back to one canvas.
  await choose(page.getByRole("combobox", { name: "Compare with" }), "Nothing");
  await expect(graph(page)).toHaveCount(1);
});

test("makes an older load current, and deletes one that isn't", async ({
  page,
}) => {
  const { first, second } = await twoLoads(page);
  await page.goto(`${O}#loads`);
  await expect(
    page.getByRole("heading", { level: 2, name: "Data loads" }),
  ).toBeVisible();
  await expect(
    page.getByText(
      "Foundry keeps the last 5: the current one and the newest others",
    ),
  ).toBeVisible();
  await expect(loadRow(page, second)).toContainText("Current");
  await expectGridFits(loadsGrid(page));
  await expect(loadRow(page, second)).toContainText("Kept");
  // Neither the current load nor its delete is offered: the current row has
  // no menu at all.
  await expect(
    loadRow(page, second).getByRole("button", { name: "More actions for v3" }),
  ).toHaveCount(0);

  const sent = page.waitForRequest(
    (r) =>
      r.method() === "POST" && r.url().endsWith(`/loads/${first}:set-current`),
  );
  await loadRow(page, first)
    .getByRole("button", { name: "More actions for v3" })
    .click();
  await page.getByRole("menuitem", { name: "Set current" }).click();
  await sent;
  await expect(page.getByText("v3 is the current load")).toBeVisible();
  await expect(loadRow(page, first)).toContainText("Current");
  await expect(loadRow(page, second)).not.toContainText("Current");
  // The Explorer now reads the first load by default.
  await page.goto(X);
  await expect(page.getByRole("combobox", { name: "Load" }).first()).toHaveText(
    "Current · v3",
  );

  // Delete the second: the confirmation says what goes with it.
  await page.goto(`${O}#loads`);
  await loadRow(page, second)
    .getByRole("button", { name: "More actions for v3" })
    .click();
  await page.getByRole("menuitem", { name: "Delete", exact: true }).click();
  const confirm = page.getByRole("dialog", { name: /^Delete v3 from / });
  await expect(confirm).toContainText(
    "Edges you added to this load’s nodes are deleted with them.",
  );
  const deleted = page.waitForRequest(
    (r) => r.method() === "DELETE" && r.url().endsWith(`/loads/${second}`),
  );
  await confirm.getByRole("button", { name: "Delete load" }).click();
  await deleted;
  // Deleting, then gone (the catch-up extract of the first runs before it).
  await expect(loadRow(page, second)).toHaveCount(0, { timeout: 20_000 });
  await expect(loadsGrid(page).getByRole("row")).toHaveCount(2);
  await page.goto(`${X}?load=${second}`);
  await expect(page.getByText("This load was deleted")).toBeVisible();
});

test("opens a load on a connected first view, and shows more", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 100 });
  await signIn(page);
  await makeContractsCurrent(page);
  await uploadContract(page);
  await loadVersion3(page);
  await page.goto(X);
  await expect(graph(page)).toBeVisible({ timeout: 15_000 });
  const area = page.locator(".ex-canvas-area");
  // Seeds, then their neighbors, then the edges among them: no node is
  // left alone, and each customer's invoices are drawn with it.
  await expect(page.getByRole("button", { name: "Show more" })).toBeVisible({
    timeout: 20_000,
  });
  await expect(area).toHaveAttribute("data-isolated", "0");
  const nodes = async () =>
    Number(
      /: (\d+) nodes/.exec((await graph(page).getAttribute("aria-label"))!)![1],
    );
  const before = await nodes();
  expect(before).toBeGreaterThan(20);
  expect(before).toBeLessThanOrEqual(320);
  expect(Number(await area.getAttribute("data-components"))).toBeLessThan(
    before / 2,
  );
  // Every node is the load's: none drawn as outside it.
  await expect(area).toHaveAttribute("data-outside", "0");
  // Show more reads the ring around the outermost nodes. The mock's load is
  // a star per Stripe customer (its invoices), so nothing lies beyond the
  // first view's, and it says so (seeding.test.ts checks that it grows on a
  // connected graph).
  await page.getByRole("button", { name: "Show more" }).click();
  await expect(page.getByText("Nothing more around these nodes")).toBeVisible({
    timeout: 15_000,
  });
  expect(await nodes()).toBe(before);
  await expect(page.getByRole("button", { name: "Show more" })).toHaveCount(0);
  await expect(area).toHaveAttribute("data-isolated", "0");
  await expect(area).toHaveAttribute("data-outside", "0");
});
