import { readFileSync } from "node:fs";
import type { Page } from "@playwright/test";
import {
  APP_ORIGIN,
  choose,
  csrfToken,
  dataTree,
  expect,
  explorer,
  file,
  mock,
  signIn,
  test,
  uploadCard as upload,
  uploadInData,
} from "./support/mock";

// Documents and loads: uploading in Data with the file chooser and with a
// drop (hashed in the browser, PUT straight to the mock's bucket with CORS,
// then completed), what an ontology reads of them listed read only, a load
// of the current agent draft behind its dialog, and what the load wrote,
// read back from the ontology's database with the user's session: the
// document's highlighted text, its facts, entities and reviews, and a
// Stripe customer the contract names, one entity with its table row and
// the contract's quote.

const O = "/ontology/ont_acme_contracts";
const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
);
const row = (page: Page, name: string) =>
  page
    .getByRole("grid", { name: "Documents" })
    .getByRole("row")
    .filter({ hasText: name });

// Drops files on the Data page as a browser does: a DataTransfer of Files.
async function drop(
  page: Page,
  files: { name: string; text: string; type: string }[],
) {
  const data = await page.evaluateHandle((list) => {
    const dt = new DataTransfer();
    for (const f of list)
      dt.items.add(new File([f.text], f.name, { type: f.type }));
    return dt;
  }, files);
  const target = page.getByRole("heading", { level: 1, name: "Connectors" });
  await target.dispatchEvent("dragover", { dataTransfer: data });
  // The default set lights up where the files will go.
  await expect(
    page
      .getByRole("list", { name: "Document sets" })
      .getByRole("listitem", { name: "General" }),
  ).toHaveClass(/is-over/);
  await target.dispatchEvent("drop", { dataTransfer: data });
}

// Makes the contracts draft (version 3) current, through the UI.
async function makeContractsCurrent(page: Page) {
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
}

// Uploads the seeded contract, then loads version 3 and waits for the end.
async function loadContract(page: Page) {
  await uploadInData(page, [file("contract.md", CONTRACT)]);
  await makeContractsCurrent(page);
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

test("uploads documents in Data, and lists what an ontology reads read only", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  await explorer(page)
    .getByLabel("Choose documents")
    .setInputFiles(file("contract.md", CONTRACT));
  await expect(upload(page, "contract.md")).toContainText(
    "Uploaded into General",
  );
  await drop(page, [
    {
      name: "notes.txt",
      text: "Initech asked for a discount.",
      type: "text/plain",
    },
    {
      name: "deck.docx",
      text: "PK",
      type: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    },
  ]);
  await expect(upload(page, "notes.txt")).toContainText("Uploaded");
  // A file that can't be read fails at once, with nothing to retry.
  await expect(upload(page, "deck.docx")).toContainText(
    "Only Markdown, MDX, text, JSON, CSV, TSV, JSONL, Parquet, or PDF files can be uploaded.",
  );
  await expect(
    upload(page, "deck.docx").getByRole("button", { name: "Retry" }),
  ).toHaveCount(0);
  await upload(page, "deck.docx")
    .getByRole("button", { name: "Dismiss deck.docx" })
    .click();
  await expect(upload(page, "deck.docx")).toHaveCount(0);

  // The bytes went to the bucket, never through /papi: the WFE saw only
  // the workspace's create and complete calls.
  const requests = (await mock.stats()).requests.filter(
    (r) => /\/documents/.test(r) && !r.startsWith("GET"),
  );
  expect(requests).toEqual([
    "POST /v1/workspaces/ws_acme/foundry/documents 200",
    expect.stringMatching(
      /^POST \/v1\/workspaces\/ws_acme\/foundry\/documents\/doc_\w+:complete 200$/,
    ),
    "POST /v1/workspaces/ws_acme/foundry/documents 200",
    expect.stringMatching(/:complete 200$/),
  ]);

  // Documents is no section of the sidebar; the ontology's overview (and its
  // pages in the Ontologies panel) links to what it reads, read only:
  // version 3 reads General.
  await makeContractsCurrent(page);
  await page.goto(O);
  await expect(
    page
      .getByRole("navigation", { name: "Main navigation" })
      .getByRole("link", { name: "Documents" }),
  ).toHaveCount(0);
  await page.getByRole("link", { name: "2 documents" }).click();
  await expect(page).toHaveURL(`${O}/documents`);
  await expect(
    page.getByRole("heading", { level: 1, name: "Documents" }),
  ).toBeVisible();
  await expect(
    page.getByText("Documents are read when you load the ontology"),
  ).toBeVisible();
  await expect(row(page, "contract.md")).toContainText("Waiting for a load");
  await expect(
    page.getByRole("grid", { name: "Documents" }).getByRole("row"),
  ).toHaveCount(3);
  await expect(page.getByLabel("Choose documents")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Upload documents" }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("link", { name: "Open in Connectors" }),
  ).toHaveAttribute("href", "/data");

  // Search narrows the list; the status filter is in the URL.
  await page.getByRole("searchbox", { name: "Search documents" }).fill("notes");
  await expect(
    page.getByRole("grid", { name: "Documents" }).getByRole("row"),
  ).toHaveCount(2);
  await page.getByRole("searchbox", { name: "Search documents" }).fill("");
  await choose(page.getByLabel("Status"), "Couldn’t be read");
  await expect(page).toHaveURL(/\/documents\?status=failed$/);
  await expect(page.getByText("No documents match.")).toBeVisible();
  await page.goto(`${O}/documents?status=waiting`);
  await expect(page.getByLabel("Status")).toHaveText("Waiting");
  await expect(
    page.getByRole("grid", { name: "Documents" }).getByRole("row"),
  ).toHaveCount(3);

  // A generation gives the agent the sets the documents are in.
  await page.goto("/ontology/ont_acme_contracts/generate");
  const sets = page.getByRole("list", { name: "Document sets" });
  await expect(sets.getByRole("listitem")).toHaveCount(1);
  await expect(sets).toContainText("General2 documents");
});

test("retries an upload storage refused, and returns the same bytes as the existing document", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  await mock.configure({ documentUploadFailures: 1 });
  await explorer(page)
    .getByLabel("Choose documents")
    .setInputFiles(file("minutes.md", "# Minutes\n\nNothing decided.\n"));
  const card = upload(page, "minutes.md");
  await expect(card).toContainText("Storage is busy. Try again in a moment.");
  await card.getByRole("button", { name: "Retry" }).click();
  await expect(card).toContainText("Uploaded");
  // The retry was the same upload: one document.
  const general = dataTree(page).getByRole("treeitem", {
    name: "General",
    exact: true,
  });
  await expect(general.getByRole("link", { name: "minutes.md" })).toHaveCount(
    1,
  );
  await card.getByRole("button", { name: "Dismiss minutes.md" }).click();
  await explorer(page)
    .getByLabel("Choose documents")
    .setInputFiles(file("minutes-copy.md", "# Minutes\n\nNothing decided.\n"));
  await expect(upload(page, "minutes-copy.md")).toContainText(
    "Already uploaded: it’s in General as minutes.md",
  );
  await expect(
    general.getByRole("link", { name: "minutes-copy.md" }),
  ).toHaveCount(0);
});

test("loads the current draft and reads the contract back from the database", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 300 });
  await signIn(page);
  await uploadInData(page, [file("contract.md", CONTRACT)]);
  await makeContractsCurrent(page);

  // Load is offered on the current agent draft, behind a dialog with the caps.
  await page.goto(O);
  await page
    .getByRole("status")
    .filter({ hasText: "Version 3 isn’t loaded yet" })
    .getByRole("button", { name: "Load data" })
    .click();
  const dialog = page.getByRole("dialog", {
    name: "Load version 3 into contracts?",
  });
  await expect(dialog).toContainText(
    "2 tables from Stripe (customers, invoices)",
  );
  await expect(dialog).toContainText(
    "From General: 1 document, read for the names and facts they hold",
  );
  await expect(dialog).toContainText(
    "at most $20 for this load and $100 a month",
  );
  await expect(dialog).toContainText("Up to 200 documents of 1.0 MiB each.");
  const sent = page.waitForRequest(/ont_acme_contracts\/loads$/);
  await dialog.getByRole("button", { name: "Load" }).click();
  expect((await sent).postDataJSON()).toEqual({
    versionId: "ver_seed_3",
    clientToken: expect.any(String),
  });
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Version 3" }),
  ).toBeVisible();
  const done = page.getByRole("status").filter({ hasText: "Loaded" }).first();
  await expect(done).toBeVisible({ timeout: 20_000 });
  await expect(done).toContainText(
    "2 chunks of table rows and 1 document written into contracts; 1 item from your documents needs review.",
  );

  // Its details link to what the ontology reads; Review documents opens
  // what needs review.
  await expect(page.getByRole("link", { name: "1 of 1" })).toHaveAttribute(
    "href",
    `${O}/documents`,
  );
  await done.getByRole("link", { name: "Review documents" }).click();
  await expect(page).toHaveURL(/\/documents\?status=review$/);
  await expect(page.getByLabel("Status")).toHaveText("Needs review");
  await expect(row(page, "contract.md")).toContainText("Needs review · 1");
  await row(page, "contract.md")
    .getByRole("link", { name: "contract.md" })
    .click();
  await expect(
    page.getByRole("heading", { level: 1, name: "contract.md" }),
  ).toBeVisible();
  // The document as rendered, with what was read marked in it.
  const text = page.getByLabel("Document text");
  await expect(text).toContainText("Umbrella Holdings introduced the parties");
  // Names underlined, quotes tinted, the review marked: UTF-8 offsets mapped
  // onto text with multi-byte quotes and the euro sign.
  await expect(text.locator("mark.is-mention")).toHaveText([
    "Master services agreement",
    "Globex Corporation",
    "Initech LLC",
    "Globex",
    "Initech",
  ]);
  await expect(text.locator("mark.is-review").first()).toContainText(
    "Umbrella Holdings",
  );
  const facts = page.getByRole("list", { name: "Facts" });
  await expect(facts.getByRole("listitem")).toHaveCount(2);
  await expect(facts.getByRole("listitem").first()).toContainText(
    "Master services agreementsigned_withGlobex Corporation",
  );
  await page.getByRole("tab", { name: /^Entities/ }).click();
  const customers = page.getByRole("region", { name: "Customer" });
  await expect(customers.getByRole("link")).toHaveText([
    "Globex Corporation",
    "Initech LLC",
  ]);
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  const reviews = page.getByRole("list", { name: "Needs review" });
  await expect(reviews).toContainText(
    "Names something that isn’t in your data",
  );
  await expect(reviews).toContainText("suggested link: signed_with");
  await page.getByRole("tab", { name: /^Details/ }).click();
  await expect(page.getByText("claude-haiku-5-5")).toBeVisible();

  // The Stripe customer the contract names is one entity: its row and the
  // contract's quote.
  await page.getByRole("tab", { name: /^Entities/ }).click();
  await customers.getByRole("link", { name: "Globex Corporation" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Globex Corporation" }),
  ).toBeVisible();
  await expect(page.getByText("Key cus_GLOBEX01")).toBeVisible();
  // Its table in the header (the first such link), then on each property.
  await expect(
    page.getByRole("link", { name: "Stripe · customers" }).first(),
  ).toHaveAttribute(
    "href",
    "/data/connections/conn_acme_stripe/tables/customers",
  );
  await expect(
    page.getByRole("link", { name: "contract.md", exact: true }),
  ).toBeVisible();
  // Each property names the row that set it.
  await expect(
    page.getByRole("link", {
      name: "company_name from Stripe · customers row cus_GLOBEX01",
    }),
  ).toBeVisible();
  await page.getByRole("tab", { name: /^Facts/ }).click();
  const signed = page.getByRole("region", { name: "signed_with from" });
  await expect(signed).toContainText("Master services agreement");
  await expect(signed).toContainText("From a document");
  await expect(signed.locator(".ex-quote")).toContainText(
    "Globex Corporation and Initech LLC",
  );
  const billed = page.getByRole("region", { name: "billed_to from" });
  await expect(billed).toContainText("in_1001");
  await expect(billed).toContainText("Stripe · invoices row in_1001");
  await signed.getByRole("link", { name: "Open in contract.md" }).click();
  await expect(page).toHaveURL(/\/documents\/doc_\w+\?tab=facts#s0-/);
  await expect(
    page.getByLabel("Document text").locator("mark.is-active").first(),
  ).toBeVisible();
});

test("shows an MDX document as written, with what a load read marked inside its JSX", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 300 });
  await signIn(page);
  const mdx = [
    "---",
    "title: Globex renewal",
    "---",
    'import { Callout } from "./callout";',
    "",
    "# Globex renewal",
    "",
    '<Callout type="info">',
    "Globex Corporation renews its plan on {props.renewsOn}.",
    "</Callout>",
    "",
    "It is made on 1 October 2026 between Acme and Initech LLC.",
  ].join("\n");
  await uploadInData(page, [file("renewal.mdx", mdx)]);
  // In Data, rendered as Markdown: the frontmatter a code block, the JSX
  // and the expression as written.
  await dataTree(page).getByRole("link", { name: "renewal.mdx" }).click();
  const shown = page.getByLabel("Document text");
  await expect(shown.locator("pre code").first()).toHaveText(
    "title: Globex renewal",
  );
  await expect(shown).toContainText('<Callout type="info">');
  await expect(shown).toContainText("{props.renewsOn}");

  await makeContractsCurrent(page);
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
  // On the load's page: the overview's "isn’t loaded yet" would match too.
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toContainText("1 document written into contracts", { timeout: 20_000 });
  await page.goto(`${O}/documents`);
  await row(page, "renewal.mdx")
    .getByRole("link", { name: "renewal.mdx" })
    .click();
  // The load read the code blanked; the page shows each section as written,
  // the names it found marked where they are, inside the JSX too.
  const text = page.getByLabel("Document text");
  await expect(text).toContainText('<Callout type="info">');
  await expect(text).toContainText("renews its plan on {props.renewsOn}.");
  await expect(text.locator("pre code").first()).toContainText(
    "title: Globex renewal",
  );
  await expect(text.locator("mark.is-mention")).toContainText([
    "Globex Corporation",
    "Initech LLC",
  ]);
  await expect(
    text.locator("mark.is-mention", { hasText: "Globex Corporation" }),
  ).toHaveCount(1);
});

test("says nothing was read from a document a load couldn't read", async ({
  page,
}) => {
  await mock.configure({ documentOutcome: "fail" });
  await signIn(page);
  await uploadInData(page, [file("notes.md", "# Notes\n\nGlobex renewed.\n")]);
  await makeContractsCurrent(page);
  await page.goto(O);
  await page
    .getByRole("status")
    .filter({ hasText: "isn’t loaded yet" })
    .getByRole("button", { name: "Load data" })
    .click();
  await page.getByRole("dialog").getByRole("button", { name: "Load" }).click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await page.goto(`${O}/documents`);
  await row(page, "notes.md").getByRole("link", { name: "notes.md" }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "It couldn’t be read" }),
  ).toBeVisible({ timeout: 20_000 });
  // Nothing was read, so it shows as uploaded: not "not read yet".
  await expect(page.getByLabel("Document text")).toBeVisible();
  await expect(page.getByText("Not read yet")).toHaveCount(0);
});

test("explores the loaded database and opens an entity's page from it", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  await page.getByRole("link", { name: "Open Explorer" }).click();
  await expect(
    page.getByRole("application", { name: /^Graph of contracts: \d+ nodes/ }),
  ).toBeVisible({ timeout: 15_000 });
  const labels = page.getByRole("complementary", { name: "Labels" });
  await expect(labels).toContainText("Customer");
  await expect(labels).toContainText("signed_with");
  const search = page.getByRole("combobox", { name: "Jump to a node" });
  await search.fill("Globex");
  await page
    .getByRole("option", { name: /Globex Corporation/ })
    .first()
    .click();
  const inspector = page.getByRole("complementary", { name: "Inspector" });
  await expect(inspector).toContainText("Globex Corporation");
  await inspector.getByRole("button", { name: "More actions" }).click();
  await inspector.getByRole("menuitem", { name: /Open its page/ }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Globex Corporation" }),
  ).toBeVisible();
});

test("hides Foundry's own records in the Explorer until they're shown", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  await page.goto("/explore/ont_acme_contracts");
  const graph = page.getByRole("application", {
    name: /^Graph of contracts: \d+ nodes/,
  });
  await expect(graph).toBeVisible({ timeout: 15_000 });
  const labels = page.getByRole("complementary", { name: "Labels" });
  const toggle = labels.getByRole("checkbox", {
    name: "Show Foundry internals",
  });
  await expect(toggle).not.toBeChecked();
  // The load wrote documents, sections, mentions and evidence beside the
  // entities: none of it is listed, counted or found.
  await expect(labels).toContainText("Customer");
  await expect(labels).toContainText("signed_with");
  for (const hidden of ["FoundryDocument", "FoundryMention", "FOUNDRY_"])
    await expect(labels).not.toContainText(hidden);
  // Every node and edge of the labels shown is on the canvas: not a sample.
  await expect(page.getByText(/Showing a sample/)).toHaveCount(0);
  // The contract mentions Globex: the search finds the customer, never a
  // mention of it.
  const search = page.getByRole("combobox", { name: "Jump to a node" });
  await search.fill("Globex");
  await expect(
    page.getByRole("option", { name: /Globex Corporation/ }).first(),
  ).toBeVisible();
  await expect(page.getByText("In database")).toBeVisible();
  await expect(
    page.getByRole("option").filter({ hasText: "Foundry" }),
  ).toHaveCount(0);
  await search.fill("");

  // Shown, they're listed apart, after the data's labels, and stay shown.
  await toggle.check();
  const internalNodes = labels.getByRole("list", {
    name: "Foundry internal node labels",
  });
  await expect(internalNodes).toContainText("FoundryDocument", {
    timeout: 15_000,
  });
  await expect(internalNodes).toContainText("FoundryMention");
  await expect(
    labels.getByRole("list", { name: "Foundry internal edge labels" }),
  ).toContainText("FOUNDRY_MENTION_OF");
  await expect(
    labels.getByRole("button", {
      name: /^FoundryReview Foundry internal node label/,
    }),
  ).toBeVisible();
  await page.reload();
  await expect(toggle).toBeChecked();
  await expect(internalNodes).toContainText("FoundryDocument", {
    timeout: 15_000,
  });

  await toggle.uncheck();
  await expect(graph).toBeVisible({ timeout: 15_000 });
  await expect(labels).toContainText("Customer");
  await expect(internalNodes).toHaveCount(0, { timeout: 15_000 });
  await expect(labels).not.toContainText("FoundryDocument");
});

test("says why a load failed: its tables were re-synced while it ran", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200, loadOutcome: "sources_changed" });
  await signIn(page);
  await makeContractsCurrent(page);
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
  const failed = page.getByRole("alert").filter({
    hasText: "The tables changed while it ran",
  });
  await expect(failed).toBeVisible({ timeout: 20_000 });
  await expect(failed).toContainText("Start a new load");
  // A new load is offered right there, beside this one.
  await failed.getByRole("button", { name: "Load data" }).click();
  await expect(
    page.getByRole("dialog", { name: "Load version 3 into contracts?" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Cancel" }).click();
  await page.goto(`${O}#loads`);
  await expect(
    page.getByRole("grid", { name: "Loads" }).getByRole("row").nth(1),
  ).toContainText("Failed");
});

test("shows a live load on the overview and stops at the spending cap", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 1500, loadOutcome: "budget_exhausted" });
  await signIn(page);
  await uploadInData(page, [
    file("contract.md", CONTRACT),
    file("notes.txt", "Initech asked for a discount."),
  ]);
  await makeContractsCurrent(page);
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
  await page.goto(O);
  const live = page
    .getByRole("status")
    .filter({ hasText: /A load is queued|Loading the database/ });
  await expect(live).toBeVisible();
  // One load at a time: the header offers its progress instead of Load.
  await expect(page.getByRole("link", { name: "Loading…" })).toBeVisible();
  await live.getByRole("link", { name: "View progress" }).click();
  await expect(page.getByText("It stopped at the spending cap")).toBeVisible({
    timeout: 20_000,
  });
  // The job's own words for the cap it reached.
  await expect(page.getByText("The load spent its $20.00 cap.")).toBeVisible();
});

test("shows a plain member the documents without uploads", async ({ page }) => {
  await signIn(page, "alan@example.com");
  await page.goto(`${O}/documents`);
  // The page's own word, not the Ontologies panel's.
  await expect(
    page.getByText("No documents yet", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("link", { name: "Open in Connectors" }),
  ).toBeVisible();
  await page.goto("/data");
  await expect(
    page.getByRole("button", { name: "Upload documents" }),
  ).toHaveCount(0);
  await expect(page.getByLabel("Choose documents")).toHaveCount(0);
  await page.goto(`${O}/versions/ver_seed_3`);
  await expect(page.getByRole("button", { name: "Load data" })).toHaveCount(0);
});

test("the BFF refuses document and load routes the UI never calls", async ({
  page,
}) => {
  await signIn(page);
  const headers = { origin: APP_ORIGIN, "x-helix-csrf": await csrfToken(page) };
  const base =
    "/papi/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts";
  // An ontology's documents are read only here.
  const replace = await page.request.delete(`${base}/documents/doc_1`, {
    headers,
  });
  expect(replace.status()).toBe(405);
  expect(replace.headers()["allow"]).toBe("GET");
  const create = await page.request.post(`${base}/documents`, {
    data: {},
    headers,
  });
  expect(create.status()).toBe(405);
  expect(create.headers()["allow"]).toBe("GET");
  const cancel = await page.request.patch(`${base}/loads/load_1`, {
    data: {},
    headers,
  });
  expect(cancel.status()).toBe(405);
  expect(cancel.headers()["allow"]).toBe("DELETE, GET");
  // The Data layout is read and replaced, never posted.
  const layout = await page.request.post(
    "/papi/v1/workspaces/ws_acme/foundry/data-layout",
    { data: {}, headers },
  );
  expect(layout.status()).toBe(405);
  expect(layout.headers()["allow"]).toBe("GET, PUT");
  // Bytes never go through /papi; the ontology-scoped writes and the old
  // graph route aren't reachable.
  for (const path of [
    "/papi/__uploads/doc_1",
    `${base}/documents/doc_1:complete`,
    `${base}/documents/doc_1:move`,
    `${base}/document-sets`,
    "/papi/v1/workspaces/ws_acme/foundry/graph:load",
  ]) {
    const res = await page.request.post(path, { data: {}, headers });
    expect(res.status(), path).toBe(404);
  }
  expect(
    (await mock.stats()).requests.filter((r) => /__uploads|graph/.test(r)),
  ).toEqual([]);
});
