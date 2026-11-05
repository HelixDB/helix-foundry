import { readFileSync } from "node:fs";
import type { Browser, Page } from "@playwright/test";
import { expect, explorer, mock, signIn, test } from "./support/mock";

// Settings › Access and restricted mode: an owner makes an agent's key and
// gives it sources, then sees what it can read; restricts a member, who
// then sees only Analyst, the ontologies their sources cover and their
// Records, entities and documents, read through typed reads, and calls
// nothing the WFE refuses and never the database; a member who can't query
// databases reads the same way, and only what their sources cover; someone
// whose sources hold part of an ontology reads that part, told so; taking
// every source away takes the ontology away; and a change that would cut
// someone off asks first. Eve is a member of Acme restricted to the General
// set; Alan, a member who queries databases, can't be restricted.

const O = "/ontology/ont_acme_contracts";
/** A customer's id in an entity page's address. */
const customer = (key: string) =>
  Buffer.from(`Customer\0k:${key}`).toString("base64url");
const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
);

// Uploads the contract into General, makes version 3 current and loads it:
// Customer contracts' current load reads Stripe's customers and invoices
// and General.
async function loadContract(page: Page) {
  await page.goto("/data");
  await explorer(page).getByLabel("Choose documents").setInputFiles({
    name: "contract.md",
    mimeType: "text/markdown",
    buffer: CONTRACT,
  });
  await expect(
    page
      .getByRole("list", { name: "Uploads" })
      .getByRole("listitem", { name: "contract.md" }),
  ).toContainText("Uploaded");
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
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

/** Opens a principal's sources from its row on Settings › Access. */
async function openSources(page: Page, tab: string, name: string) {
  await page.goto(`/settings/access?tab=${tab}`);
  await page.getByRole("button", { name: `Sources for ${name}` }).click();
  return page.getByRole("dialog", { name: `Sources for ${name}` });
}

/** Gives Eve Stripe beside her General: she reads Customer contracts. */
async function shareWithEve(page: Page) {
  const sources = await openSources(page, "people", "Eve Adler");
  await expect(
    sources.getByRole("checkbox", { name: /^General/ }),
  ).toBeChecked();
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved what Eve Adler can read.")).toBeVisible();
  await expect(sources.getByRole("list", { name: "Can read" })).toContainText(
    "Customer contracts",
  );
  await sources.getByRole("button", { name: "Close" }).first().click();
}

/** Eve, signed in in a browser of her own. */
async function asEve(browser: Browser) {
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  const page = await context.newPage();
  await signIn(page, "eve@example.com");
  return page;
}

test("an agent's key reads the ontologies its sources cover", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  await page.goto("/settings/access");
  await expect(page.getByRole("tab", { name: "Agents" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  // Acme's agent's key holds nothing yet.
  await expect(
    page.getByRole("listitem", { name: "Support agent" }),
  ).toContainText("Reads nothing yet");

  await page.getByRole("button", { name: "New key" }).click();
  const create = page.getByRole("dialog", { name: "New agent key" });
  await create.getByLabel("Name").fill("Billing bot");
  await create.getByRole("button", { name: "Create key" }).click();
  const made = page.getByRole("dialog", { name: "Key for Billing bot" });
  await expect(made.getByRole("textbox", { name: "Agent key" })).toHaveValue(
    /^hlxs_[0-9a-f]+$/,
  );
  await made.getByRole("button", { name: "Done" }).click();

  const sources = await openSources(page, "agents", "Billing bot");
  await expect(sources.getByRole("list", { name: "Can read" })).toHaveCount(0);
  await expect(
    sources.getByRole("list", { name: "Can’t read yet" }),
  ).toContainText("Customer contracts");
  // Stripe covers its tables: they show chosen and can't be changed alone.
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await expect(
    sources.getByRole("checkbox", { name: "invoices", exact: true }),
  ).toBeChecked();
  await expect(
    sources.getByRole("checkbox", { name: "invoices", exact: true }),
  ).toBeDisabled();
  await sources.getByRole("checkbox", { name: /^General/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(sources.getByRole("list", { name: "Can read" })).toContainText(
    "Customer contracts",
  );
  await sources.getByRole("button", { name: "Close" }).first().click();
  await expect(
    page.getByRole("listitem", { name: "Billing bot" }),
  ).toContainText("Reads Customer contracts");

  // Revoked, it goes with its sources.
  await page.getByRole("button", { name: "Revoke Billing bot" }).click();
  await page
    .getByRole("dialog", { name: "Revoke Billing bot?" })
    .getByRole("button", { name: "Revoke" })
    .click();
  await expect(page.getByRole("listitem", { name: "Billing bot" })).toHaveCount(
    0,
  );
});

test("a restricted member sees only what their sources cover", async ({
  page,
  browser,
}) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 100 });
  await signIn(page);
  await loadContract(page);
  // Eve is restricted; Alan queries databases, so he can't be.
  await page.goto("/settings/access?tab=people");
  const eve = page.getByRole("switch", { name: "Restrict Eve Adler" });
  await expect(eve).toBeChecked();
  await expect(
    page.getByRole("switch", { name: "Restrict Alan Turing" }),
  ).toBeDisabled();
  await expect(
    page.getByRole("listitem", { name: "Alan Turing" }),
  ).toContainText("queries its databases outside Foundry");
  await expect(
    page.getByRole("switch", { name: "Restrict Ada Lovelace" }),
  ).toHaveCount(0);
  // Holding sources is what restricts her, so it can't be lifted.
  await expect(eve).toBeDisabled();
  // Her General is part of what the load holds.
  await expect(page.getByRole("listitem", { name: "Eve Adler" })).toContainText(
    "Reads part of Customer contracts",
  );
  await shareWithEve(page);
  await page.goto("/settings/access?tab=people");
  await expect(page.getByRole("listitem", { name: "Eve Adler" })).toContainText(
    "Reads Customer contracts",
  );
  await page.close();

  const before = (await mock.stats()).requests.length;
  const as = await asEve(browser);
  // Analyst opens on the one ontology shared with her.
  await expect(as).toHaveURL(/\/ask\/ont_acme_contracts/);
  const nav = as.getByRole("list", { name: "Workspace" });
  await expect(nav.getByRole("link")).toHaveText([
    "Analyst",
    "Ontologies",
    "Explorer",
  ]);
  await expect(as.getByRole("link", { name: "Settings" })).toHaveCount(0);
  await expect(as.getByRole("button", { name: "Search" })).toHaveCount(0);
  await expect(
    as.getByRole("combobox", { name: "Load", exact: true }),
  ).toHaveCount(0);

  // She asks; the answer's sources open the pages her sources read, named
  // as those pages name them. A rule break has no page there: its source
  // opens its step.
  const form = as.getByRole("form", { name: "Ask a question" });
  await form
    .getByRole("textbox", { name: /^Question/ })
    .fill("Who signed the services agreement?");
  await form.getByRole("button", { name: "Ask", exact: true }).click();
  const answer = as.getByRole("region", { name: "Answer" });
  await expect(answer).toBeVisible({ timeout: 20_000 });
  await expect(as.getByRole("group", { name: "Questions budget" })).toHaveCount(
    0,
  );
  await expect(
    answer.getByRole("button", {
      name: "Source 7: A break of invoice_under_contract",
    }),
  ).toBeVisible();
  // An entity's page: its properties with what states them, its facts;
  // where documents name it isn't hers.
  const thread = as.url();
  await answer
    .getByRole("link", { name: "Source 1: Customer Globex Corporation" })
    .click();
  await expect(
    as.getByRole("heading", { level: 1, name: "Globex Corporation" }),
  ).toBeVisible();
  await expect(as.getByRole("main")).toContainText("emailap@globex.example");
  await as.getByRole("tab", { name: /^Facts/ }).click();
  await expect(
    as.getByRole("region", { name: /^billed_to from/ }),
  ).toBeVisible();
  await as.getByRole("tab", { name: "Mentions" }).click();
  await expect(
    as.getByText("Where documents name it is the load’s own record"),
  ).toBeVisible();
  // A quote opens its document, as uploaded.
  await as.goto(thread);
  await answer
    .getByRole("link", { name: "Source 4: Quote in contract.md" })
    .click();
  await expect(
    as.getByRole("heading", { level: 1, name: "contract.md" }),
  ).toBeVisible();
  await expect(as.getByLabel("Document text")).toContainText(
    "Globex Corporation",
  );

  // Records is her Explorer: the graph opens it, with no Graph view, and it
  // pages, searches and opens a record.
  await nav.getByRole("link", { name: "Explorer" }).click();
  await expect(as).toHaveURL(/\/explore\/ont_acme_contracts\/records$/);
  await expect(as.getByRole("radio", { name: "Graph" })).toHaveCount(0);
  const types = as.getByRole("list", { name: "Types" });
  await expect(types.getByRole("button")).toHaveText([
    "Contract1",
    "Customer64",
    "Invoice65",
  ]);
  await types.getByRole("button", { name: /^Customer/ }).click();
  const customers = as.getByRole("grid", { name: "Customer records" });
  await expect(as.getByText("1–50 of 64")).toBeVisible();
  await as.getByRole("button", { name: "Next" }).click();
  await expect(as.getByText("51–64 of 64")).toBeVisible();
  await as
    .getByRole("searchbox", { name: "Search Customer records" })
    .fill("globex");
  await expect(customers.getByRole("row")).toHaveCount(2);
  await customers.getByRole("row").nth(1).click();
  const record = as.getByRole("dialog", { name: "Globex Corporation" });
  await expect(record).toContainText("Customer · cus_GLOBEX01");
  await expect(
    record.getByRole("button", { name: "Open in graph" }),
  ).toHaveCount(0);
  await record.getByRole("link", { name: "Open its page" }).click();
  await expect(
    as.getByRole("heading", { level: 1, name: "Globex Corporation" }),
  ).toBeVisible();

  // The Ontologies panel lists only what is shared; the overview is the
  // schema alone.
  await as.goto("/ontology");
  await expect(as).toHaveURL(new RegExp(`${O}$`));
  const panel = as.getByRole("complementary", { name: "Ontologies" });
  const listed = panel.getByRole("list", { name: "Ontologies" });
  await expect(listed).toContainText("Customer contracts");
  await expect(listed).not.toContainText("Sales graph");
  await expect(listed).not.toContainText("Renewals");
  await expect(panel.getByRole("link", { name: "Documents" })).toHaveCount(0);
  await expect(
    as.getByRole("heading", { level: 1, name: "Customer contracts" }),
  ).toBeVisible();
  await expect(as.getByText("Shared with you: the types")).toBeVisible();
  await expect(as.getByRole("button", { name: "Load data" })).toHaveCount(0);
  await expect(as.getByRole("link", { name: "Generate" })).toHaveCount(0);
  await expect(as.getByRole("button", { name: "Delete ontology" })).toHaveCount(
    0,
  );

  // Everything else opens Analyst.
  for (const path of [
    "/data",
    "/settings",
    "/settings/access",
    `${O}/loads`,
    "/ontology/new",
  ]) {
    await as.goto(path);
    await expect(as, path).toHaveURL(/\/ask(\/|\?|$)|\/ontology\/ont_/);
  }
  await as.getByRole("button", { name: "Account" }).click();
  await expect(as.getByRole("menuitem", { name: "Settings" })).toHaveCount(0);
  await expect(
    as.getByRole("menuitem", { name: "Plan & billing" }),
  ).toHaveCount(0);
  await as.keyboard.press("Escape");

  // Nothing she did asked for what the WFE refuses her, or read the
  // database.
  const requests = (await mock.stats()).requests.slice(before);
  expect(requests.filter((r) => / 40[34]$/.test(r))).toEqual([]);
  expect(requests.filter((r) => r.includes("databases:query-read"))).toEqual(
    [],
  );
  expect(requests.some((r) => /\/loads\/load_\w+:read 200$/.test(r))).toBe(
    true,
  );
});

test("a member who can't query reads only what their sources cover", async ({
  page,
  browser,
}) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 100 });
  await signIn(page);
  await loadContract(page);
  // Alan can't query databases: he reads through what he is given.
  await mock.configure({ memberQueryRead: false });
  const sources = await openSources(page, "people", "Alan Turing");
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await sources.getByRole("checkbox", { name: /^General/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  await page
    .getByRole("dialog", { name: "Restrict Alan Turing?" })
    .getByRole("button", { name: "Save and restrict" })
    .click();
  await expect(sources.getByRole("list", { name: "Can read" })).toContainText(
    "Customer contracts",
  );
  await page.close();

  const before = (await mock.stats()).requests.length;
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  const as = await context.newPage();
  await signIn(as, "alan@example.com");
  // Given sources, he is restricted: no Connectors. Analyst opens the one
  // ontology he reads, and offers no other.
  await expect(
    as.getByRole("list", { name: "Workspace" }).getByRole("link"),
  ).toHaveText(["Analyst", "Ontologies", "Explorer"]);
  await as.goto("/ask");
  await expect(as).toHaveURL(/\/ask\/ont_acme_contracts/);
  await as.getByRole("combobox", { name: "Ontology" }).click();
  await expect(as.getByRole("listbox").getByRole("option")).toHaveText([
    "Customer contracts",
  ]);
  await as.keyboard.press("Escape");
  await expect(
    as.getByRole("combobox", { name: "Load", exact: true }),
  ).toHaveCount(0);
  // One he doesn't read isn't there for him.
  await as.goto("/ask/ont_acme_sales");
  await expect(
    as.getByRole("heading", { name: "This ontology doesn’t exist" }),
  ).toBeVisible();
  // The one he reads: Records through typed reads.
  await as.goto("/explore/ont_acme_contracts");
  await expect(as).toHaveURL(/\/explore\/ont_acme_contracts\/records$/);
  await as
    .getByRole("list", { name: "Types" })
    .getByRole("button", { name: /^Invoice/ })
    .click();
  await expect(as.getByText("1–50 of 65")).toBeVisible();

  // Nothing refused but the ontology he was sent to that isn't there for him.
  const requests = (await mock.stats()).requests.slice(before);
  expect(
    requests.filter(
      (r) => / 40[34]$/.test(r) && !r.includes("/ont_acme_sales"),
    ),
  ).toEqual([]);
  expect(requests.filter((r) => r.includes("databases:query-read"))).toEqual(
    [],
  );
});

test("a member who can't query names only what their sources fill", async ({
  page,
  browser,
}) => {
  await mock.configure({ loadStepMs: 200, invoiceOnlyCustomers: 90 });
  await signIn(page);
  await loadContract(page);
  await mock.configure({ memberQueryRead: false });
  const sources = await openSources(page, "people", "Alan Turing");
  await sources
    .getByRole("checkbox", { name: "customers", exact: true })
    .check();
  await sources.getByRole("button", { name: "Save" }).click();
  await page
    .getByRole("dialog", { name: "Restrict Alan Turing?" })
    .getByRole("button", { name: "Save and restrict" })
    .click();
  await expect(sources.getByRole("list", { name: "Can read" })).toContainText(
    "Part of it",
  );
  await page.close();

  const before = (await mock.stats()).requests.length;
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  const as = await context.newPage();
  await signIn(as, "alan@example.com");
  await as.goto("/explore/ont_acme_contracts/records");
  // His version is whole; what he reads of it is the load's view.
  await expect(
    as.getByText("You can read part of this ontology"),
  ).toBeVisible();
  await expect(
    as.getByRole("list", { name: "Types" }).getByRole("button"),
  ).toHaveText(["Customer—"]);
  await expect(as.getByText("The first 64 records you can read")).toBeVisible();
  const requests = (await mock.stats()).requests.slice(before);
  expect(requests.filter((r) => r.includes("databases:query-read"))).toEqual(
    [],
  );
  expect(requests.filter((r) => / 40[34]$/.test(r))).toEqual([]);
});

test("taking every source away takes the ontology away", async ({
  page,
  browser,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  await shareWithEve(page);
  const sources = await openSources(page, "people", "Eve Adler");
  await sources.getByRole("checkbox", { name: /^Stripe/ }).uncheck();
  await sources.getByRole("checkbox", { name: /^General/ }).uncheck();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved what Eve Adler can read.")).toBeVisible();
  await expect(
    sources.getByRole("list", { name: "Can’t read yet" }),
  ).toContainText("Stripe › customers");
  await page.close();

  const before = (await mock.stats()).requests.length;
  const as = await asEve(browser);
  await expect(
    as.getByRole("heading", { name: "Nothing shared with you yet" }),
  ).toBeVisible();
  expect(
    (await mock.stats()).requests.slice(before).filter((r) => / 403$/.test(r)),
  ).toEqual([]);
  await as.goto(O);
  await expect(
    as.getByRole("heading", { name: "This ontology doesn’t exist" }),
  ).toBeVisible();
});

test("someone whose sources hold part of an ontology reads that part", async ({
  page,
  browser,
}) => {
  // The invoices table also creates 90 customers, which come first in a read
  // of Customer; Eve is given only the customers table.
  await mock.configure({
    loadStepMs: 200,
    askStepMs: 100,
    invoiceOnlyCustomers: 90,
  });
  await signIn(page);
  await loadContract(page);
  const sources = await openSources(page, "people", "Eve Adler");
  await sources.getByRole("checkbox", { name: /^General/ }).uncheck();
  await sources
    .getByRole("checkbox", { name: "customers", exact: true })
    .check();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved what Eve Adler can read.")).toBeVisible();
  const reads = sources.getByRole("list", { name: "Can read" });
  await expect(reads).toContainText("Customer contracts");
  await expect(reads).toContainText("Part of it: not Stripe › invoices");
  await sources.getByRole("button", { name: "Close" }).first().click();
  await expect(page.getByRole("listitem", { name: "Eve Adler" })).toContainText(
    "Reads part of Customer contracts",
  );
  // Who an ontology is shared with: she reads part of it.
  await page.getByRole("tab", { name: "Ontologies" }).click();
  await expect(
    page.getByRole("listitem", { name: "Customer contracts" }),
  ).toContainText("Part of it: Eve Adler");
  await page.close();

  const before = (await mock.stats()).requests.length;
  const as = await asEve(browser);
  const note = as.getByText("You can read part of this ontology");

  // Ask says so, and answers from what she holds.
  await expect(as).toHaveURL(/\/ask\/ont_acme_contracts/);
  await expect(note).toBeVisible();
  const form = as.getByRole("form", { name: "Ask a question" });
  await form.getByRole("textbox", { name: /^Question/ }).fill("Who are they?");
  await form.getByRole("button", { name: "Ask", exact: true }).click();
  const answer = as.getByRole("region", { name: "Answer" });
  await expect(answer).toContainText(
    "Of what the sources shared with you hold",
    { timeout: 20_000 },
  );
  // A thread says so too.
  await expect(as).toHaveURL(/thread=/);
  await expect(note).toBeVisible();

  // Records: only Customer, uncounted: the first rows she can read, no pages.
  await as.goto("/explore/ont_acme_contracts/records");
  await expect(note).toBeVisible();
  const types = as.getByRole("list", { name: "Types" });
  await expect(types.getByRole("button")).toHaveText(["Customer—"]);
  const customers = as.getByRole("grid", { name: "Customer records" });
  await expect(as.getByText("The first 64 records you can read")).toBeVisible();
  await expect(customers).not.toContainText("cus_0");
  await expect(as.getByRole("button", { name: "Next" })).toHaveCount(0);
  // A page of an old link is the same read.
  await as.goto("/explore/ont_acme_contracts/records?type=Customer&page=3");
  await expect(as.getByText("The first 64 records you can read")).toBeVisible();
  await expect(as.getByRole("button", { name: "Previous" })).toHaveCount(0);

  // A sort of what is partly held: said, the rows as they were.
  await as.goto(
    "/explore/ont_acme_contracts/records?type=Customer&sort=customer_id",
  );
  await expect(
    as.getByText(
      "You can’t sort by Customer id: only part of it is in the sources shared with you.",
    ),
  ).toBeVisible();
  await expect(as.getByText("The first 64 records you can read")).toBeVisible();
  // A filter of it: said, in the table's place.
  await as.goto(
    "/explore/ont_acme_contracts/records?type=Customer&field=customer_id&value=cus_1",
  );
  await expect(
    as.getByText(
      "You can’t filter by Customer id: only part of it is in the sources shared with you.",
    ),
  ).toBeVisible();
  await expect(customers).toHaveCount(0);
  // What it holds whole filters as anyone's.
  await as.goto(
    "/explore/ont_acme_contracts/records?type=Customer&field=email&value=ap%40globex.example",
  );
  await expect(customers.getByRole("row").nth(1)).toContainText(
    "Globex Corporation",
  );
  await expect(as.getByText("You can’t filter by")).toHaveCount(0);

  // An entity's page: what she holds, none of what she doesn't.
  await as.goto(
    `/explore/ont_acme_contracts/entities/${customer("cus_GLOBEX01")}`,
  );
  await expect(
    as.getByRole("heading", { level: 1, name: "Globex Corporation" }),
  ).toBeVisible();
  await expect(note).toBeVisible();
  await expect(as.getByRole("main")).toContainText("ap@globex.example");
  await as.getByRole("tab", { name: /^Facts/ }).click();
  await expect(as.getByRole("region", { name: /^billed_to/ })).toHaveCount(0);
  // One only the invoices table states isn't in the load, for her.
  await as.goto(
    `/explore/ont_acme_contracts/entities/${customer("cus_00001")}`,
  );
  await expect(as.getByText("This entity isn’t in this load")).toBeVisible();

  // The overview's schema is what her sources fill.
  await as.goto(O);
  await expect(note).toBeVisible();
  await expect(as.getByRole("main")).not.toContainText("Invoice");
  const panel = as.getByRole("complementary", { name: "Ontologies" });
  await expect(panel).toContainText("Part shared with you");

  // She never read the database, or asked what the WFE refuses her.
  const requests = (await mock.stats()).requests.slice(before);
  expect(requests.filter((r) => r.includes("databases:query-read"))).toEqual(
    [],
  );
  expect(requests.filter((r) => / 403$/.test(r))).toEqual([]);
});

test("someone who reads part of an ontology is told plainly that its past isn't shared", async ({
  page,
  browser,
}) => {
  // Asked about another time, the agent shows such a reader no past, so it
  // declines as uncovered (contract 4.7.0).
  await mock.configure({
    loadStepMs: 200,
    askStepMs: 100,
    invoiceOnlyCustomers: 90,
    askOutcome: "cannot_answer",
  });
  await signIn(page);
  await loadContract(page);
  const sources = await openSources(page, "people", "Eve Adler");
  await sources.getByRole("checkbox", { name: /^General/ }).uncheck();
  await sources
    .getByRole("checkbox", { name: "customers", exact: true })
    .check();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved what Eve Adler can read.")).toBeVisible();
  await page.close();

  const as = await asEve(browser);
  await expect(
    as.getByText("You can read part of this ontology"),
  ).toBeVisible();
  const form = as.getByRole("form", { name: "Ask a question" });
  await form
    .getByRole("textbox", { name: /^Question/ })
    .fill("Which customers did we have last March?");
  await form.getByRole("button", { name: "Ask", exact: true }).click();
  await expect(as.getByText("This ontology can’t answer that")).toBeVisible({
    timeout: 20_000,
  });
  await expect(as.locator(".on-card .hf-tag")).toHaveText(
    "Only current values are shared with you",
  );
  await expect(as.getByText("No recorded state covers that time")).toHaveCount(
    0,
  );
  await expect(
    as.getByRole("link", { name: "Draft a new version with this" }),
  ).toHaveCount(0);
});

test("someone who holds all of an ontology reads an uncovered time as the load's", async ({
  page,
  browser,
}) => {
  // They see valid times and past values, so uncovered means the load
  // doesn't say: the general label, not the part reader's.
  await mock.configure({
    loadStepMs: 200,
    askStepMs: 100,
    askOutcome: "cannot_answer",
  });
  await signIn(page);
  await loadContract(page);
  await shareWithEve(page);
  await page.close();

  const as = await asEve(browser);
  await as.route(
    (url) =>
      /\/ontologies\/ont_acme_contracts\/questions(?:\/[^/:]+)?$/.test(
        url.pathname,
      ),
    async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      for (const question of body.questions ??
        (body.question ? [body.question] : [])) {
        if (!question.resultJson) continue;
        const result = JSON.parse(question.resultJson);
        if (result.status === "cannot_answer") result.kind = "uncovered";
        question.resultJson = JSON.stringify(result);
      }
      await route.fulfill({ response, json: body });
    },
  );
  await expect(as.getByText("You can read part of this ontology")).toHaveCount(
    0,
  );
  const form = as.getByRole("form", { name: "Ask a question" });
  await form
    .getByRole("textbox", { name: /^Question/ })
    .fill("Who signed it last March?");
  await form.getByRole("button", { name: "Ask", exact: true }).click();
  await expect(as.getByText("This ontology can’t answer that")).toBeVisible({
    timeout: 20_000,
  });
  await expect(as.locator(".on-card .hf-tag")).toHaveText(
    "No recorded state covers that time",
  );
});

test("someone whose sources hold all of a type pages past the first read", async ({
  page,
  browser,
}) => {
  await mock.configure({ loadStepMs: 200, invoiceOnlyCustomers: 150 });
  await signIn(page);
  await loadContract(page);
  await shareWithEve(page);
  await page.close();

  const as = await asEve(browser);
  await as.goto("/explore/ont_acme_contracts/records");
  await expect(as.getByText("You can read part of this ontology")).toHaveCount(
    0,
  );
  await as
    .getByRole("list", { name: "Types" })
    .getByRole("button", { name: /^Customer/ })
    .click();
  await expect(as.getByText("1–50 of 214")).toBeVisible();
  for (const span of ["51–100", "101–150", "151–200", "201–214"]) {
    await as.getByRole("button", { name: "Next" }).click();
    await expect(as.getByText(`${span} of 214`)).toBeVisible();
  }
  await expect(as.getByRole("button", { name: "Next" })).toBeDisabled();
  await expect(
    as.getByRole("grid", { name: "Customer records" }).getByRole("row"),
  ).toHaveCount(15);
});

test("someone who holds a type whole pages it, beside one they hold part of", async ({
  page,
  browser,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  // Stripe without the documents: invoices are whole; a customer's currency,
  // which documents may state too, isn't.
  const sources = await openSources(page, "people", "Eve Adler");
  await sources.getByRole("checkbox", { name: /^General/ }).uncheck();
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved what Eve Adler can read.")).toBeVisible();
  await expect(sources.getByRole("list", { name: "Can read" })).toContainText(
    "Part of it",
  );
  await page.close();

  const as = await asEve(browser);
  await as.goto("/explore/ont_acme_contracts/records?type=Invoice");
  await expect(
    as.getByText("You can read part of this ontology"),
  ).toBeVisible();
  await expect(as.getByText("1–50 of 65")).toBeVisible();
  await as.getByRole("button", { name: "Next" }).click();
  await expect(as.getByText("51–65 of 65")).toBeVisible();
  await expect(
    as.getByRole("grid", { name: "Invoice records" }).getByRole("row"),
  ).toHaveCount(16);
  await as.goto("/explore/ont_acme_contracts/records?type=Customer");
  await expect(as.getByText("The first 64 records you can read")).toBeVisible();
  await expect(as.getByRole("button", { name: "Next" })).toHaveCount(0);
});

test("a busy or paused database reads as what it is, through grants", async ({
  page,
  browser,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  await shareWithEve(page);
  await page.close();

  const as = await asEve(browser);
  // Every slot taken: it waits its turn, and the page never says so.
  await mock.configure({ readsBusy: 3 });
  await as.goto("/explore/ont_acme_contracts/records?type=Invoice");
  await expect(as.getByText("1–50 of 65")).toBeVisible();
  await expect(as.getByText("already runs")).toHaveCount(0);
  // Queries paused: billing's words, with nothing to retry.
  await mock.configure({
    readsPaused:
      "queries are paused: the workspace has no active plan; choose a plan to resume",
  });
  await as.goto("/explore/ont_acme_contracts/records?type=Customer");
  await expect(
    as.getByText("Queries are paused: this workspace has no active Helix plan"),
  ).toBeVisible();
  await expect(as.getByRole("button", { name: "Retry" })).toHaveCount(0);
});

test("access changed elsewhere shows on coming back to the tab", async ({
  page,
  browser,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  await shareWithEve(page);
  const as = await asEve(browser);
  await expect(as).toHaveURL(/\/ask\/ont_acme_contracts/);
  await expect(as.getByRole("form", { name: "Ask a question" })).toBeVisible();

  const sources = await openSources(page, "people", "Eve Adler");
  await sources.getByRole("checkbox", { name: /^Stripe/ }).uncheck();
  await sources.getByRole("checkbox", { name: /^General/ }).uncheck();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved what Eve Adler can read.")).toBeVisible();
  await expect(
    sources.getByRole("list", { name: "Can’t read yet" }),
  ).toContainText("Stripe › customers");

  await as.evaluate(() => window.dispatchEvent(new Event("visibilitychange")));
  await expect(as.getByRole("form", { name: "Ask a question" })).toHaveCount(0);
});

test("offers a grantee nothing of the sample or of getting into production", async ({
  page,
  browser,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  await shareWithEve(page);
  await page.close();

  const before = (await mock.stats()).requests.length;
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  const as = await context.newPage();
  // An ontology named as the sample is, which only its generations confirm.
  await as.route("**/foundry/ontologies?*", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    for (const o of json.ontologies || []) o.name = "Acme Corp customers";
    await route.fulfill({ response, json });
  });
  await signIn(as, "eve@example.com");
  await expect(as).toHaveURL(/\/ask\/ont_acme_contracts/);
  await expect(as.getByRole("form", { name: "Ask a question" })).toBeVisible();
  await expect(
    as.getByRole("region", { name: "Getting into production" }),
  ).toHaveCount(0);
  await expect(as.getByRole("region", { name: "Sample data" })).toHaveCount(0);
  // The page it can't have sends it home.
  await as.goto("/ontology/sample");
  await expect(as).toHaveURL(/\/ask/);
  const requests = (await mock.stats()).requests.slice(before);
  expect(requests.filter((r) => r.includes("/generations"))).toEqual([]);
  expect(requests.filter((r) => r.includes("databases:query-read"))).toEqual(
    [],
  );
  expect(requests.filter((r) => / 403$/.test(r))).toEqual([]);
});

test("a member reading through grants isn't sent to query the database", async ({
  page,
  browser,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  await mock.configure({ memberQueryRead: false });
  const sources = await openSources(page, "people", "Alan Turing");
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await sources.getByRole("checkbox", { name: /^General/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  await page
    .getByRole("dialog", { name: "Restrict Alan Turing?" })
    .getByRole("button", { name: "Save and restrict" })
    .click();
  await expect(sources.getByRole("list", { name: "Can read" })).toContainText(
    "Customer contracts",
  );
  await page.close();

  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  const as = await context.newPage();
  await signIn(as, "alan@example.com");
  await as.goto("/ask/ont_acme_contracts");
  await expect(as.getByRole("form", { name: "Ask a question" })).toBeVisible();
  await expect(
    as.getByRole("region", { name: "Getting into production" }),
  ).toHaveCount(0);
  // Given it back he stays restricted, so he still reads through grants.
  await mock.configure({ memberQueryRead: true });
  await as.reload();
  await expect(as.getByRole("form", { name: "Ask a question" })).toBeVisible();
  await expect(
    as.getByRole("region", { name: "Getting into production" }),
  ).toHaveCount(0);
});

test("the threads say why they can't be read, with a retry", async ({
  page,
}) => {
  await signIn(page);
  let refuse = true;
  await page.route(
    "**/papi/v1/workspaces/*/foundry/ontologies/ont_acme_contracts",
    (route) =>
      refuse && route.request().method() === "GET"
        ? route.fulfill({
            status: 500,
            json: { code: 13, message: "the ontology couldn't be read" },
          })
        : route.fallback(),
  );
  await page.goto("/ask/ont_acme_contracts");
  const threads = page.getByRole("navigation", { name: "Threads" });
  await expect(threads.getByRole("alert")).toContainText(
    "Couldn’t read the threads: the ontology couldn't be read",
    { timeout: 15_000 },
  );
  refuse = false;
  await threads.getByRole("button", { name: "Retry" }).click();
  await expect(threads.getByRole("alert")).toHaveCount(0);
});

test("flags a restricted member who holds raw access again", async ({
  page,
}) => {
  await signIn(page);
  await mock.configure({ memberQueryRead: false });
  await page.goto("/settings/access?tab=people");
  await page.getByRole("switch", { name: "Restrict Alan Turing" }).check();
  await expect(page.getByText("Alan Turing now sees only")).toBeVisible();
  const row = page.getByRole("listitem", { name: "Alan Turing" });
  await expect(row).not.toContainText("Raw access");
  // Given query access since, in the Helix dashboard.
  await mock.configure({ memberQueryRead: true });
  await page.reload();
  await expect(row).toContainText("Raw access");
  await expect(row).toContainText("queries its databases outside Foundry");
  // Nobody else is flagged, and the switch still takes it off.
  await expect(
    page.getByRole("listitem", { name: "Eve Adler" }),
  ).not.toContainText("Raw access");
  await expect(
    page.getByRole("switch", { name: "Restrict Alan Turing" }),
  ).toBeEnabled();
});

test("giving a member sources restricts them, and taking them away widens nothing", async ({
  page,
  browser,
}) => {
  await mock.configure({ loadStepMs: 200, memberQueryRead: false });
  await signIn(page);
  await loadContract(page);
  await page.goto("/settings/access?tab=people");
  const row = page.getByRole("listitem", { name: "Alan Turing" }),
    alan = page.getByRole("switch", { name: "Restrict Alan Turing" });
  await expect(row).toContainText("Sees everything");
  await expect(alan).not.toBeChecked();

  // It says what they'll lose first, and backing out changes nothing.
  await page.getByRole("button", { name: "Sources for Alan Turing" }).click();
  const sources = page.getByRole("dialog", {
    name: "Sources for Alan Turing",
  });
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  const confirm = page.getByRole("dialog", { name: "Restrict Alan Turing?" });
  await expect(confirm).toContainText("see only what these sources hold");
  await expect(confirm).toContainText("other documents");
  await confirm.getByRole("button", { name: "Cancel" }).click();
  await expect(confirm).toHaveCount(0);
  await sources.getByRole("button", { name: "Save" }).click();
  await confirm.getByRole("button", { name: "Save and restrict" }).click();
  await expect(
    page.getByText("Saved what Alan Turing can read."),
  ).toBeVisible();

  // Now restricted, it asks no more, and holding sources can't be lifted.
  await sources.getByRole("checkbox", { name: /^General/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(confirm).toHaveCount(0);
  await expect(sources.getByRole("list", { name: "Can read" })).toContainText(
    "Customer contracts",
  );
  await sources.getByRole("button", { name: "Close" }).first().click();
  await expect(alan).toBeChecked();
  await expect(alan).toBeDisabled();
  await expect(alan).toHaveAttribute(
    "aria-description",
    /Take those away first/,
  );

  // He sees only what they hold: no Connectors, and no other document's text.
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  const as = await context.newPage();
  await signIn(as, "alan@example.com");
  await expect(
    as.getByRole("list", { name: "Workspace" }).getByRole("link"),
  ).toHaveText(["Analyst", "Ontologies", "Explorer"]);

  // His last source taken away, he reads nothing, not everything.
  const again = await openSources(page, "people", "Alan Turing");
  await again.getByRole("checkbox", { name: /^Stripe/ }).uncheck();
  await again.getByRole("checkbox", { name: /^General/ }).uncheck();
  await again.getByRole("button", { name: "Save" }).click();
  await expect(page.getByRole("dialog", { name: /^Restrict/ })).toHaveCount(0);
  await again.getByRole("button", { name: "Close" }).first().click();
  await expect(alan).toBeChecked();
  await as.goto("/ask");
  await as.reload();
  await expect(
    as.getByRole("heading", { name: "Nothing shared with you yet" }),
  ).toBeVisible();

  // Only lifting it, with none left, gives him the workspace back.
  await expect(alan).toBeEnabled();
  await alan.uncheck();
  await expect(row).toContainText("Sees everything");
  await as.goto("/data");
  await expect(
    as.getByRole("list", { name: "Workspace" }).getByRole("link"),
  ).toHaveText(["Analyst", "Ontologies", "Explorer", "Connectors"]);
});

test("a member who reads databases is given no sources, on their own or by a team", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/settings/access?tab=people");
  const button = page.getByRole("button", { name: "Sources for Alan Turing" });
  await expect(button).toBeDisabled();
  await expect(button).toHaveAttribute("title", /outside Foundry/);
  // Through a team: the grant is refused, with why.
  await page.goto("/settings/access?tab=teams");
  await page.getByRole("button", { name: "New team" }).click();
  const make = page.getByRole("dialog", { name: "New team" });
  await make.getByRole("textbox", { name: "Name" }).fill("Finance");
  await make.getByRole("checkbox", { name: "Alan Turing" }).check();
  await make.getByRole("button", { name: "Create team" }).click();
  await page.getByRole("button", { name: "Sources for Finance" }).click();
  const sources = page.getByRole("dialog", { name: "Sources for Finance" });
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  await page
    .getByRole("dialog", { name: "Restrict 1 member of Finance?" })
    .getByRole("button", { name: "Save and restrict" })
    .click();
  await expect(sources.getByRole("alert")).toContainText(
    "a member of this team can write a project",
  );
});

test("a team's sources restrict its members, and whoever joins it", async ({
  page,
}) => {
  await signIn(page);
  await mock.configure({ memberQueryRead: false });
  await page.goto("/settings/access?tab=teams");
  await page.getByRole("button", { name: "New team" }).click();
  const make = page.getByRole("dialog", { name: "New team" });
  await make.getByRole("textbox", { name: "Name" }).fill("Finance");
  await make.getByRole("checkbox", { name: "Alan Turing" }).check();
  // An owner may sit on a team; it changes nothing for them.
  await make.getByRole("checkbox", { name: "Ada Lovelace" }).check();
  await make.getByRole("button", { name: "Create team" }).click();

  await page.getByRole("button", { name: "Sources for Finance" }).click();
  const sources = page.getByRole("dialog", { name: "Sources for Finance" });
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  const confirm = page.getByRole("dialog", {
    name: "Restrict 1 member of Finance?",
  });
  await expect(confirm).toContainText("Alan Turing");
  await expect(confirm).not.toContainText("Ada Lovelace");
  await confirm.getByRole("button", { name: "Save and restrict" }).click();
  await expect(page.getByText("Saved what Finance can read.")).toBeVisible();

  await page.goto("/settings/access?tab=people");
  const alan = page.getByRole("switch", { name: "Restrict Alan Turing" });
  await expect(alan).toBeChecked();
  await expect(alan).toBeDisabled();
  await expect(
    page.getByRole("listitem", { name: "Ada Lovelace" }),
  ).not.toContainText("Raw access");
});

test("joining a team that holds sources asks first, and restricts", async ({
  page,
}) => {
  await signIn(page);
  await mock.configure({ memberQueryRead: false });
  await page.goto("/settings/access?tab=teams");
  await page.getByRole("button", { name: "New team" }).click();
  const make = page.getByRole("dialog", { name: "New team" });
  await make.getByRole("textbox", { name: "Name" }).fill("Finance");
  await make.getByRole("checkbox", { name: "Ada Lovelace" }).check();
  await make.getByRole("button", { name: "Create team" }).click();
  // Only an owner on it: no one to restrict, so nothing to ask.
  await page.getByRole("button", { name: "Sources for Finance" }).click();
  const sources = page.getByRole("dialog", { name: "Sources for Finance" });
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved what Finance can read.")).toBeVisible();
  await sources.getByRole("button", { name: "Close" }).first().click();

  await page.getByRole("button", { name: "Edit Finance" }).click();
  const edit = page.getByRole("dialog", { name: "Edit Finance" });
  await edit.getByRole("checkbox", { name: "Alan Turing" }).check();
  await edit.getByRole("button", { name: "Save" }).click();
  const confirm = page.getByRole("dialog", { name: "Restrict Alan Turing?" });
  await expect(confirm).toContainText("what Finance’s sources hold");
  // Backing out changes nothing.
  await confirm.getByRole("button", { name: "Cancel" }).click();
  await edit.getByRole("button", { name: "Save" }).click();
  await confirm.getByRole("button", { name: "Save and restrict" }).click();
  await expect(page.getByText("Saved Finance.")).toBeVisible();

  await page.goto("/settings/access?tab=people");
  const alan = page.getByRole("switch", { name: "Restrict Alan Turing" });
  await expect(alan).toBeChecked();
  await expect(alan).toBeDisabled();
});

test("a team's confirmation still asks when its members can't be read", async ({
  page,
}) => {
  await signIn(page);
  await mock.configure({ memberQueryRead: false });
  await page.goto("/settings/access?tab=teams");
  await page.getByRole("button", { name: "New team" }).click();
  const make = page.getByRole("dialog", { name: "New team" });
  await make.getByRole("textbox", { name: "Name" }).fill("Finance");
  await make.getByRole("checkbox", { name: "Alan Turing" }).check();
  await make.getByRole("button", { name: "Create team" }).click();
  await expect(page.getByText("Made Finance.")).toBeVisible();
  await page.route("**/papi/v1/workspaces/*/members*", (route) =>
    route.fulfill({
      status: 500,
      json: { code: 13, message: "members couldn't be read" },
    }),
  );
  await page.reload();
  await page.getByRole("button", { name: "Sources for Finance" }).click();
  const sources = page.getByRole("dialog", { name: "Sources for Finance" });
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(
    page.getByRole("dialog", { name: "Restrict the members of Finance?" }),
  ).toContainText("Its members will see only");
});

test("nothing changes for anyone with no grants", async ({ page, browser }) => {
  await mock.configure({ loadStepMs: 200, invoiceOnlyCustomers: 90 });
  await signIn(page);
  await loadContract(page);
  await page.close();

  const before = (await mock.stats()).requests.length;
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  const as = await context.newPage();
  await signIn(as, "alan@example.com");
  await as.goto("/explore/ont_acme_contracts/records");
  await as
    .getByRole("list", { name: "Types" })
    .getByRole("button", { name: /^Customer/ })
    .click();
  // He reads the database, whole: its totals, and no word of sources.
  await expect(as.getByText("1–50 of 154")).toBeVisible();
  await expect(as.getByText("You can read part of this ontology")).toHaveCount(
    0,
  );
  const requests = (await mock.stats()).requests.slice(before);
  expect(requests.some((r) => r.includes("databases:query-read"))).toBe(true);
  expect(requests.some((r) => /:read /.test(r))).toBe(false);
});

test("asks before a change that would cut a grantee off", async ({ page }) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  await shareWithEve(page);

  // Version 2 reads the Shop database, which Eve isn't given: making it
  // current asks first, and Cancel leaves it as it is.
  await page.goto(`${O}/versions/ver_seed_2`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 2 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  const warning = page.getByRole("dialog", {
    name: "Some people will stop reading",
  });
  await expect(
    warning.getByRole("list", { name: "Who stops reading" }),
  ).toHaveText("Eve Adler: Customer contracts");
  await warning.getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByText("Version 2 is now current.")).toHaveCount(0);

  // Continue makes it current; loading it asks again.
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 2 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await warning.getByRole("button", { name: "Continue" }).click();
  await expect(page.getByText("Version 2 is now current.")).toBeVisible();
  await page.goto(O);
  await page.getByRole("button", { name: "Load data" }).first().click();
  await page
    .getByRole("dialog", { name: /^Load version 2/ })
    .getByRole("button", { name: "Load" })
    .click();
  await expect(
    warning.getByRole("list", { name: "Who stops reading" }),
  ).toHaveText("Eve Adler: Customer contracts");
  await warning.getByRole("button", { name: "Cancel" }).click();
  await expect(page).toHaveURL(new RegExp(`${O}$`));
});

test("asks before making an older load current cuts a grantee off", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  // Version 2's load, which reads the Shop database, then version 3's.
  for (const n of [2, 3]) {
    await page.goto(`${O}/versions/ver_seed_${n}`);
    await page.getByRole("button", { name: "Make current" }).click();
    await page
      .getByRole("dialog", { name: `Make version ${n} current?` })
      .getByRole("button", { name: "Make current" })
      .click();
    await expect(page.getByText(`Version ${n} is now current.`)).toBeVisible();
    await page.goto(O);
    await page.getByRole("button", { name: "Load data" }).first().click();
    await page
      .getByRole("dialog", { name: new RegExp(`^Load version ${n}`) })
      .getByRole("button", { name: "Load" })
      .click();
    await expect(page).toHaveURL(/\/loads\/load_/);
    await expect(
      page.getByRole("status").filter({ hasText: "Loaded" }).first(),
    ).toBeVisible({ timeout: 20_000 });
  }
  await shareWithEve(page);

  await page.goto(`${O}#loads`);
  const setCurrent = async () => {
    await page.getByRole("button", { name: "More actions for v2" }).click();
    await page.getByRole("menuitem", { name: "Set current" }).click();
  };
  await setCurrent();
  const warning = page.getByRole("dialog", {
    name: "Some people will stop reading",
  });
  await expect(
    warning.getByRole("list", { name: "Who stops reading" }),
  ).toHaveText("Eve Adler: Customer contracts");
  await warning.getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByText("v2 is the current load")).toHaveCount(0);
  await setCurrent();
  await warning.getByRole("button", { name: "Continue" }).click();
  await expect(page.getByText("v2 is the current load")).toBeVisible();
});

test("says who would keep part of what a change takes", async ({ page }) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  await shareWithEve(page);
  // Without Stripe, Eve's General is still part of Customer contracts.
  await page.goto("/data/connections/conn_acme_stripe");
  await page.getByRole("button", { name: "Connection actions" }).click();
  await page.getByRole("menuitem", { name: "Delete connection" }).click();
  await page
    .getByRole("dialog", { name: /^Delete/ })
    .getByRole("button", { name: "Delete connection" })
    .click();
  const warning = page.getByRole("dialog", {
    name: "Some people will stop reading",
  });
  await expect(
    warning.getByRole("list", { name: "Who stops reading" }),
  ).toHaveText(
    "Eve Adler: Customer contracts (keeps the part their sources hold)",
  );
  await warning.getByRole("button", { name: "Cancel" }).click();
});
