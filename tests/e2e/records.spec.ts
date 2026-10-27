import { readFileSync } from "node:fs";
import type { Locator, Page } from "@playwright/test";
import {
  choose,
  createdOntology,
  expect,
  explorer,
  finishOntology,
  mock,
  signIn,
  startOntology,
  test,
} from "./support/mock";

// An ontology's records, by type, beside the Explorer's graph, and the
// readable names everything shows: a load of the contracts draft writes 64
// Stripe customers, 65 invoices and the seeded contract; Records pages,
// searches, sorts and filters them, opens one with its links and sources,
// and focuses the graph on it; the canvas and the inspector name each node
// as the current version does, and leave the layout's own properties out;
// with no current version, everything keeps its raw label.

const O = "/ontology/ont_acme_contracts",
  X = "/explore/ont_acme_contracts";
const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
);

async function makeCurrent(page: Page, version: 1 | 3) {
  await page.goto(`${O}/versions/ver_seed_${version}`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: `Make version ${version} current?` })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(
    page.getByText(`Version ${version} is now current.`),
  ).toBeVisible();
}

// Uploads the seeded contract (and any other markdown documents), then
// loads version 3 and waits for the end.
async function load(page: Page, more: { name: string; text: string }[] = []) {
  await page.goto("/data");
  const files = [
    { name: "contract.md", buffer: CONTRACT },
    ...more.map((d) => ({ name: d.name, buffer: Buffer.from(d.text) })),
  ];
  await explorer(page)
    .getByLabel("Choose documents")
    .setInputFiles(files.map((f) => ({ ...f, mimeType: "text/markdown" })));
  for (const f of files)
    await expect(
      page
        .getByRole("list", { name: "Uploads" })
        .getByRole("listitem", { name: f.name }),
    ).toContainText("Uploaded");
  await makeCurrent(page, 3);
  await page.goto(O);
  await page
    .getByRole("status")
    .filter({ hasText: "Version 3 isn’t loaded yet" })
    .getByRole("button", { name: "Load data" })
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

// Where the Explorer's bar sits: its title, the Ontology and Load pickers
// and the Graph/Records switch.
async function headerBoxes(page: Page) {
  const box = async (l: Locator) => {
    const b = (await l.boundingBox())!;
    return { x: Math.round(b.x), y: Math.round(b.y), h: Math.round(b.height) };
  };
  const pickerOf = (name: string) =>
    page
      .locator(".ex-load-picker")
      .filter({ has: page.getByRole("combobox", { name, exact: true }) });
  const picker = pickerOf("Load");
  await expect(picker).toBeVisible();
  // Measure once the sidebar has settled.
  await page
    .locator(".hf-sidebar")
    .evaluate((el) => Promise.all(el.getAnimations().map((a) => a.finished)));
  return {
    title: await box(page.getByRole("heading", { level: 1, name: "Explorer" })),
    ontology: await box(pickerOf("Ontology")),
    picker: await box(picker),
    select: await box(picker.getByRole("combobox")),
    switch: await box(page.getByRole("radiogroup", { name: "View" })),
  };
}

const grid = (page: Page, type: string) =>
  page.getByRole("grid", { name: `${type} records` });
const firstRow = (page: Page, type: string) =>
  grid(page, type).getByRole("row").nth(1);

test("pages, searches, sorts and filters records, and opens one in the graph", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  // Before a load, Records says what to do, and the panel's Load says the
  // same.
  await page.goto(`${X}/records`);
  await expect(
    page.getByRole("main").getByRole("heading", { name: "Nothing loaded yet" }),
  ).toBeVisible();
  await expect(
    page
      .getByRole("complementary", { name: "Explorer panel" })
      .getByRole("region", { name: "Load" }),
  ).toContainText("Nothing loaded yet");
  await load(page);

  // From the Explorer's graph, the switch opens Records.
  await page
    .getByRole("navigation", { name: "Main navigation" })
    .getByRole("link", { name: "Explorer" })
    .click();
  const graph = await headerBoxes(page);
  // The Load picker keeps its label and select on one line, inside its frame.
  expect(graph.select.y).toBeGreaterThanOrEqual(graph.picker.y);
  expect(graph.select.y + graph.select.h).toBeLessThanOrEqual(
    graph.picker.y + graph.picker.h,
  );
  await page.getByRole("radio", { name: "Records" }).click();
  await expect(page).toHaveURL(new RegExp(`${X}/records$`));
  const types = page.getByRole("list", { name: "Types" });
  await expect(types.getByRole("button")).toHaveText([
    "Contract1",
    "Customer64",
    "Invoice65",
  ]);
  // Switching views moves nothing in the header.
  expect(await headerBoxes(page)).toEqual(graph);
  // The first type with records opens; the contract is named by its title.
  await expect(
    types.getByRole("button", { name: /^Contract/ }),
  ).toHaveAttribute("aria-pressed", "true");
  await expect(firstRow(page, "Contract")).toContainText(
    "Master services agreement",
  );

  await types.getByRole("button", { name: /^Customer/ }).click();
  await expect(page).toHaveURL(/type=Customer/);
  await expect(page.getByText("1–50 of 64")).toBeVisible();
  // A record reads by its name, with its key under it; normalized names
  // never show.
  await expect(firstRow(page, "Customer")).toContainText(
    "Globex Corporationcus_GLOBEX01",
  );
  await expect(grid(page, "Customer")).not.toContainText("__nn");
  // The record column stands for the key and names; the rest show, labelled.
  const headers = grid(page, "Customer").getByRole("columnheader");
  await expect(headers).toHaveText(["Record", "Currency", "Email"]);

  // Pages.
  await page.getByRole("button", { name: "Next" }).click();
  await expect(page.getByText("51–64 of 64")).toBeVisible();
  await expect(page).toHaveURL(/page=2/);
  await expect(page.getByRole("button", { name: "Next" })).toBeDisabled();
  await page.getByRole("button", { name: "Previous" }).click();
  await expect(page.getByText("1–50 of 64")).toBeVisible();

  // Search by name, however it is written.
  const search = page.getByRole("searchbox", {
    name: "Search Customer records",
  });
  await search.fill("INITECH");
  await expect(page).toHaveURL(/q=INITECH/);
  await expect(grid(page, "Customer").getByRole("row")).toHaveCount(2);
  await expect(firstRow(page, "Customer")).toContainText("Initech LLC");
  await expect(page.getByText("1–1 of 1")).toBeVisible();
  await search.fill("nobody here");
  await expect(page.getByText("No records match.")).toBeVisible();
  await expect(grid(page, "Customer")).toHaveCount(0);
  await search.fill("");
  await expect(page.getByText("1–50 of 64")).toBeVisible();
  // A page past the last match says so, with the count, and goes back.
  await page.goto(`${X}/records?type=Customer&q=a&page=3`);
  await expect(page.getByText("No records on this page.")).toBeVisible();
  await expect(page.getByText(/^0 of \d+$/)).toBeVisible();
  await page.getByRole("button", { name: "Previous" }).click();
  await expect(page.getByText(/^\d+–\d+ of \d+$/)).toBeVisible();
  await expect(firstRow(page, "Customer")).toBeVisible();
  // At phone width, "No records match." shows in view.
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(`${X}/records?type=Customer&q=nobodyhere`);
  await expect(page.getByText("No records match.")).toBeInViewport();
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${X}/records?type=Customer`);

  // Sort by a column: records sort by name, case-blind, last first.
  const header = grid(page, "Customer").getByRole("columnheader", {
    name: "Record",
  });
  await header.getByRole("button").click();
  await expect(firstRow(page, "Customer")).toContainText("Alder & Finch LLP");
  await header.getByRole("button").click();
  await expect(page).toHaveURL(/sort=%24record&dir=desc/);
  await expect(firstRow(page, "Customer")).toContainText("Zephyrine Studios");
  await expect(header).toHaveAttribute("aria-sort", "descending");

  // The Columns menu shows a names prop too, and its header sorts it.
  const menu = page.locator("summary", { hasText: "Columns" });
  await menu.click();
  await page.getByRole("checkbox", { name: "Company name" }).check();
  await expect(headers).toHaveText([
    "Record",
    "Company name",
    "Currency",
    "Email",
  ]);
  await grid(page, "Customer")
    .getByRole("columnheader", { name: "Company name" })
    .getByRole("button")
    .click();
  await expect(page).toHaveURL(/sort=company_name(&|$)/);
  await expect(firstRow(page, "Customer")).toContainText("Alder & Finch LLP");
  await page.getByRole("checkbox", { name: "Email" }).uncheck();
  await expect(headers).toHaveText(["Record", "Company name", "Currency"]);
  await page.getByRole("button", { name: "Reset columns" }).click();
  await expect(headers).toHaveText(["Record", "Currency", "Email"]);
  await menu.click();

  // Filter by a field.
  await choose(page.getByLabel("Filter by field"), "Currency");
  await page.getByLabel("Currency equals").fill("gbp");
  await expect(page).toHaveURL(/field=currency&value=gbp/);
  const rows = grid(page, "Customer").getByRole("row");
  await expect(rows.nth(1)).toContainText("gbp");
  for (const row of (await rows.all()).slice(1))
    await expect(row).toContainText("gbp");
  await expect(page.getByText(/^1–\d+ of \d+$/)).toBeVisible();
  await page.getByRole("button", { name: "Clear", exact: true }).click();
  await expect(page.getByText("1–50 of 64")).toBeVisible();
  // A field picked with no value yet filters nothing, from a link too.
  await page.goto(`${X}/records?type=Customer&field=currency`);
  await expect(page.getByText("1–50 of 64")).toBeVisible();
  await expect(firstRow(page, "Customer")).toContainText("Globex Corporation");
  await page.goto(`${X}/records?type=Customer`);

  // Open a record: its props, its links by name, and where it comes from.
  await search.fill("globex");
  await expect(grid(page, "Customer").getByRole("row")).toHaveCount(2);
  await expect(firstRow(page, "Customer")).toContainText("Globex");
  await firstRow(page, "Customer").click();
  const detail = page.getByRole("dialog", { name: "Globex Corporation" });
  await expect(detail).toContainText("Customer · cus_GLOBEX01");
  await expect(page).toHaveURL(/record=/);
  const props = detail.getByRole("region", { name: "Properties" });
  await expect(props).toContainText("Company nameGlobex Corporation");
  await expect(props).not.toContainText("__nn");
  await expect(props).not.toContainText("entity_key");
  const billed = detail.getByRole("region", { name: "billed_to from" });
  // Each link group heads one level under "Links".
  await expect(billed.getByRole("heading", { level: 4 })).toContainText(
    "billed_to",
  );
  await expect(detail.getByRole("heading", { level: 2 })).toHaveCount(1);
  await expect(billed).toContainText("in_1001");
  await expect(billed).toContainText("in_1002");
  await expect(billed).toContainText("Stripe · invoices row in_1001");
  const signed = detail.getByRole("region", { name: "signed_with from" });
  await expect(signed).toContainText("Master services agreement");
  await expect(signed).toContainText("From a document");
  await expect(signed.locator(".ex-quote")).toContainText(
    "Globex Corporation and Initech LLC",
  );
  await expect(
    signed.getByRole("link", { name: "Open in contract.md" }),
  ).toHaveAttribute("href", /\/documents\/doc_\w+\?tab=facts#s0-/);
  const sources = detail.getByRole("region", { name: "Where it comes from" });
  await expect(sources).toContainText("Stripe · customers row cus_GLOBEX01");
  await expect(sources).toContainText("section 1");
  await expect(
    sources.getByRole("link", { name: "Open in contract.md" }).first(),
  ).toHaveAttribute("href", /\/documents\/doc_\w+\?tab=entities#s0-/);

  // A link's other end opens in the drawer, its type in the table.
  await signed
    .getByRole("button", { name: "Master services agreement" })
    .click();
  const contract = page.getByRole("dialog", {
    name: "Master services agreement",
  });
  await expect(contract).toContainText("Contract");
  await expect(
    contract.getByRole("region", { name: "signed_with to" }),
  ).toContainText("Initech LLC");
  await expect(
    types.getByRole("button", { name: /^Contract/ }),
  ).toHaveAttribute("aria-pressed", "true");
  await page.goBack();
  await expect(detail).toBeVisible();
  // Back sets the search box again, so it never rewrites the URL it came to.
  await expect(search).toHaveValue("globex");

  // Open in graph: the Explorer selects it and names it as Records does.
  await detail.getByRole("button", { name: "Open in graph" }).click();
  await expect(page).toHaveURL(new RegExp(`${X}\\?node=\\d+$`));
  const inspector = page.getByRole("complementary", { name: "Inspector" });
  await expect(inspector).toContainText("Globex Corporation", {
    timeout: 15_000,
  });
  await expect(inspector).toContainText("Customer");
  await expect(inspector).toContainText("company_name");
  await expect(inspector).not.toContainText("__nn");
  await expect(inspector).not.toContainText("entity_key");
  await expect(page.locator(".ex-canvas [aria-live=polite]")).toHaveText(
    "Selected Customer: Globex Corporation",
  );
  await expect(page.getByRole("radio", { name: "Graph" })).toHaveAttribute(
    "aria-checked",
    "true",
  );
});

test("names what a load holds by the load's version", async ({ page }) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await load(page);

  // Version 3 names customers by their company and invoices by their key.
  await page.goto(X);
  await expect(
    page.getByRole("application", { name: /^Graph of contracts: \d+ nodes/ }),
  ).toBeVisible({ timeout: 15_000 });
  const jump = page.getByRole("combobox", { name: "Jump to a node" });
  // Hooli isn't in the first view: the database search finds it.
  await jump.fill("Hooli");
  await jump.press("Enter");
  await page
    .getByRole("option", { name: /Hooli Inc/ })
    .first()
    .click();
  const inspector = page.getByRole("complementary", { name: "Inspector" });
  await expect(inspector.locator(".ex-subject-title")).toHaveText("Hooli Inc");
  // The inspector says which load wrote it.
  await expect(inspector.getByTestId("node-load")).toContainText("Load v3");
  const announced = page.locator(".ex-canvas [aria-live=polite]");
  await expect(announced).toHaveText("Selected Customer: Hooli Inc");
  await expect(page).toHaveURL(/\?node=\d+$/);
  const link = new URL(page.url()).searchParams.get("node");

  // Making version 1 current changes no load: the load still reads by the
  // version it wrote, in the graph and in Records.
  await makeCurrent(page, 1);
  await page.goto(`${X}?node=${link}`);
  await expect(inspector.locator(".ex-subject-title")).toHaveText("Hooli Inc", {
    timeout: 15_000,
  });
  await jump.fill("Master");
  await page
    .getByRole("option", { name: /Master services agreement/ })
    .first()
    .click();
  await expect(announced).toHaveText(
    "Selected Contract: Master services agreement",
  );

  await page.getByRole("radio", { name: "Records" }).click();
  const types = page.getByRole("list", { name: "Types" });
  await expect(types.getByRole("button")).toHaveText([
    "Contract1",
    "Customer64",
    "Invoice65",
  ]);
  await types.getByRole("button", { name: /^Customer/ }).click();
  await expect(firstRow(page, "Customer")).toContainText(
    "Globex Corporationcus_GLOBEX01",
  );
  // Search finds a key.
  await page
    .getByRole("searchbox", { name: "Search Customer records" })
    .fill("HOOLI");
  await expect(firstRow(page, "Customer")).toContainText("Hooli Inc");
  await expect(grid(page, "Customer").getByRole("row")).toHaveCount(2);
});

test("falls back to raw labels with no current version", async ({ page }) => {
  // A new ontology on the team's own database: no version names anything.
  await signIn(page);
  await startOntology(page, "Orders");
  await page.getByRole("radio", { name: "orders in Production" }).check();
  await finishOntology(page);
  await createdOntology(page);
  await page
    .getByRole("navigation", { name: "Main navigation" })
    .getByRole("link", { name: "Explorer" })
    .click();
  await page.getByRole("radio", { name: "Records" }).click();
  // Nothing is loaded: Records says to finish setting it up, and the team's
  // own data is Other data, whose graph Records opens.
  await expect(
    page.getByRole("main").getByRole("link", { name: "Finish setting up" }),
  ).toBeVisible();
  await page.goto(`${new URL(page.url()).pathname}?load=other`);
  await expect(page.getByText("No version is current yet")).toBeVisible();
  await page.getByRole("link", { name: "Open the graph" }).click();
  await expect(page).toHaveURL(/\?load=other$/);

  // The graph keeps the explorer's own captions and every property.
  await expect(
    page.getByRole("application", {
      name: "Graph of orders: 7 nodes, 4 edges",
    }),
  ).toBeVisible({ timeout: 15_000 });
  const jump = page.getByRole("combobox", { name: "Jump to a node" });
  await jump.fill("Grace");
  await page
    .getByRole("option", { name: /Grace Hopper/ })
    .first()
    .click();
  await expect(page.locator(".ex-canvas [aria-live=polite]")).toHaveText(
    "Selected Account: Grace Hopper",
  );
  await expect(
    page.getByRole("complementary", { name: "Inspector" }),
  ).toContainText("account_id");
});

test("says when it couldn't read the current version, and reads it again", async ({
  page,
}) => {
  const version =
    "**/papi/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts/versions/ver_seed_1";
  await page.route(version, (route) =>
    route.fulfill({
      status: 503,
      contentType: "application/json",
      body: JSON.stringify({ code: 14, message: "upstream connect error" }),
    }),
  );
  await signIn(page);
  // Other data: with nothing loaded, the default reads no version.
  await page.goto(`${X}/records?load=other`);
  // Not "names no types": a failed read says so, and offers Retry.
  await expect(page.getByText("Couldn’t read the current version")).toBeVisible(
    { timeout: 10_000 },
  );
  await expect(page.getByText("upstream connect error")).toBeVisible();
  await expect(page.getByText("names no types")).toHaveCount(0);
  await page.unroute(version);
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(page.getByText("Nothing loaded yet")).toBeVisible();
});

test("says how many more quotes a document has than the drawer shows", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  // Notes that name Initech five times, over two sections.
  await load(page, [
    {
      name: "notes.md",
      text: [
        "# Initech call notes",
        "",
        "Initech LLC called on Monday. Initech asked about the renewal.",
        "",
        "## Follow-up",
        "",
        "Initech agreed to the terms. Initech paid the first invoice.",
        "",
      ].join("\n"),
    },
  ]);
  await page.goto(`${X}/records?type=Customer&q=initech`);
  await firstRow(page, "Customer").click();
  const sources = page
    .getByRole("dialog", { name: "Initech LLC" })
    .getByRole("region", { name: "Where it comes from" });
  await expect(
    sources.getByRole("link", { name: "Open in notes.md" }),
  ).toHaveCount(3);
  await expect(sources).toContainText("2 more in notes.md · See all");
  await expect(sources.getByRole("link", { name: "See all" })).toHaveAttribute(
    "href",
    /\/entities\/[\w-]+\?tab=mentions$/,
  );
  // contract.md names it twice: all show, with nothing more to say.
  await expect(
    sources.getByRole("link", { name: "Open in contract.md" }),
  ).toHaveCount(2);
  await expect(sources).not.toContainText("more in contract.md");
});
