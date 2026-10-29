import { readFileSync } from "node:fs";
import type { Page } from "@playwright/test";
import {
  choose,
  createdOntology,
  dataTree,
  expect,
  file,
  mock,
  signIn,
  signInToNewWorkspace,
  startFirstOntology,
  test,
  uploadCard,
  uploadInData,
} from "./support/mock";

// Documents in the Data explorer: sets as folders (new, renamed in place,
// described, deleted when empty), uploads into a set by its button or a
// drop, onto the Data page into the default set, General made for a
// workspace with none; the Data page's list of sets saying what each holds
// and what reads it; a re-upload naming its folder; a document renamed in place,
// moved and deleted from its page, which lists the ontologies that read
// it. Then what reads them: Generate picks whole sets, starting from the
// current version's and sending none when none is chosen; a version's
// scope on its page and in the Load dialog; an upload into a set two
// ontologies read, read by both; a version's Sources tab; and after a
// load, a property's quote opening its span, and a document in a set its
// version reads but no route names, stored, not read.

const O = "/ontology/ont_acme_contracts",
  R = "/ontology/ont_acme_renewals";
const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
);
// A set on the Data page: what it holds and which ontologies read it.
const card = (page: Page, name: string) =>
  page
    .getByRole("list", { name: "Document sets" })
    .getByRole("listitem", { name, exact: true });
// A set (or documents/) in the Data explorer: its documents and buttons.
const folder = (page: Page, name: string) =>
  dataTree(page).getByRole("treeitem", { name, exact: true });
const row = (page: Page, name: string) =>
  page
    .getByRole("grid", { name: "Documents" })
    .getByRole("row")
    .filter({ hasText: name });

// Makes the contracts draft (version 3) current: its scope and route read
// General.
async function makeContractsCurrent(page: Page) {
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
}

// Loads an ontology's current version from its overview, to the end.
async function load(page: Page, o: string, dialog: string) {
  await page.goto(o);
  await page
    .getByRole("status")
    .filter({ hasText: "isn’t loaded yet" })
    .getByRole("button", { name: "Load data" })
    .click();
  await page
    .getByRole("dialog", { name: dialog })
    .getByRole("button", { name: "Load" })
    .click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 20_000 });
}

async function newSet(page: Page, name: string, description = "") {
  await page.getByRole("button", { name: "New set" }).click();
  const dialog = page.getByRole("dialog", { name: "New document set" });
  await dialog.getByLabel("Name").fill(name);
  if (description) await dialog.getByLabel("Description").fill(description);
  await dialog.getByRole("button", { name: "Create set" }).click();
  await expect(dialog).toHaveCount(0);
}

// Drops files on a target as a browser does: a DataTransfer of Files.
async function drop(
  page: Page,
  target: ReturnType<Page["locator"]>,
  files: { name: string; text: string; type: string }[],
) {
  const data = await page.evaluateHandle((list) => {
    const dt = new DataTransfer();
    for (const f of list)
      dt.items.add(new File([f.text], f.name, { type: f.type }));
    return dt;
  }, files);
  await target.dispatchEvent("dragover", { dataTransfer: data });
  await target.dispatchEvent("drop", { dataTransfer: data });
}

test("manages sets in the Data tree, and uploads into them by button or drop", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  // Renewals reads General; nothing reads a new set.
  await expect(card(page, "General")).toContainText("Read by Renewals");
  await newSet(page, "Board", "Minutes of every board meeting.");
  const board = folder(page, "Board"),
    boardCard = card(page, "Board");
  // What reads it is said in words, then what it holds; its row in the
  // explorer says the same in its name's tooltip.
  await expect(boardCard).toContainText(
    "0 documents · No ontology reads Board yet. Minutes of every board meeting.",
  );
  await expect(boardCard).toContainText("Not read");
  await expect(board.locator(":scope > .dt-row .dt-name")).toHaveAttribute(
    "title",
    "Board\n0 documents\nNo ontology reads Board yet.\nMinutes of every board meeting.",
  );
  // Names are unique in the workspace, case and accents aside.
  await page.getByRole("button", { name: "New set" }).click();
  const dialog = page.getByRole("dialog", { name: "New document set" });
  await dialog.getByLabel("Name").fill("Général");
  await expect(dialog).toContainText("There is a set named General already.");
  await expect(
    dialog.getByRole("button", { name: "Create set" }),
  ).toBeDisabled();
  await dialog.getByRole("button", { name: "Cancel" }).click();

  // Its button uploads into it.
  const created = page.waitForRequest(
    (r) => r.method() === "POST" && /\/foundry\/documents$/.test(r.url()),
  );
  await uploadInData(
    page,
    [file("q3.md", "# Q3\n\nGlobex renewed.\n")],
    "Board",
  );
  expect((await created).postDataJSON()).toMatchObject({
    name: "q3.md",
    setId: expect.stringMatching(/^set_/),
  });
  await expect(uploadCard(page, "q3.md")).toContainText("Uploaded into Board");
  await expect(uploadCard(page, "q3.md")).toContainText(
    "No ontology reads Board yet.",
  );
  await expect(board.getByRole("link", { name: "q3.md" })).toBeVisible();
  await expect(boardCard).toContainText("1 document");
  await expect(board.locator(":scope > .dt-row .dt-count")).toHaveText("1");

  // Dropped on a folder, files go into it; dropped elsewhere on the page,
  // into General.
  await drop(page, board, [
    { name: "q4.md", text: "# Q4\n\nInitech left.\n", type: "text/markdown" },
  ]);
  await expect(uploadCard(page, "q4.md")).toContainText("Uploaded into Board");
  await drop(
    page,
    page.getByRole("heading", { level: 1, name: "Connectors" }),
    [{ name: "notes.txt", text: "Hooli called.", type: "text/plain" }],
  );
  await expect(uploadCard(page, "notes.txt")).toContainText(
    "Uploaded into General",
  );
  await expect(uploadCard(page, "notes.txt")).toContainText(
    "Renewals reads it with its first load.",
  );
  // The same bytes again are the document already there: it says where.
  await uploadInData(page, [file("copy.md", "# Q3\n\nGlobex renewed.\n")]);
  await expect(uploadCard(page, "copy.md")).toContainText(
    "Already uploaded: it’s in Board as q3.md",
  );
  await expect(boardCard).toContainText("2 documents");

  // A set and a document are renamed in place; Escape keeps the name.
  await board.getByRole("button", { name: "Rename Board" }).click();
  await page.getByLabel("Name for Board").fill("Board minutes");
  await page.getByLabel("Name for Board").press("Enter");
  const minutes = folder(page, "Board minutes");
  await expect(minutes).toBeVisible();
  await minutes.getByRole("button", { name: "Rename q4.md" }).click();
  await page.getByLabel("Name for q4.md").fill("Q4 minutes.md");
  await page.getByLabel("Name for q4.md").press("Escape");
  await expect(minutes.getByRole("link", { name: "q4.md" })).toBeVisible();
  await minutes.getByRole("button", { name: "Rename q4.md" }).click();
  await page.getByLabel("Name for q4.md").fill("Q4/minutes.md");
  await page.getByLabel("Name for q4.md").press("Enter");
  await expect(
    page.getByRole("alert").filter({ hasText: "can’t have a slash" }),
  ).toBeVisible();
  await minutes.getByRole("button", { name: "Rename q4.md" }).click();
  await page.getByLabel("Name for q4.md").fill("Q4 minutes.md");
  await page.getByLabel("Name for q4.md").press("Enter");
  await expect(
    minutes.getByRole("link", { name: "Q4 minutes.md" }),
  ).toBeVisible();
  // Its description is edited in a dialog.
  await minutes.getByRole("button", { name: "Describe Board minutes" }).click();
  const edit = page.getByRole("dialog", { name: "Edit Board minutes" });
  await edit.getByLabel("Description").fill("What the board decided.");
  await edit.getByRole("button", { name: "Save" }).click();
  await expect(card(page, "Board minutes")).toContainText(
    "What the board decided.",
  );
  // A folder closes and stays closed.
  await minutes.locator(":scope > .dt-row .dt-chevron").click();
  await expect(minutes).toHaveAttribute("aria-expanded", "false");
  await expect(minutes.getByRole("link", { name: "q3.md" })).toHaveCount(0);
  await page.reload();
  await expect(minutes).toHaveAttribute("aria-expanded", "false");
  await expect(minutes.getByRole("link", { name: "q3.md" })).toHaveCount(0);
  await minutes.locator(":scope > .dt-row .dt-chevron").click();
  // The filter finds documents by name, in their set.
  await page.getByRole("searchbox", { name: "Filter data" }).fill("q3");
  await expect(dataTree(page).getByRole("link")).toHaveText(["q3.md"]);
  await page.getByRole("searchbox", { name: "Filter data" }).fill("");

  // A set holding documents can't be deleted; emptied, it can.
  await expect(
    minutes.getByRole("button", { name: "Delete Board minutes" }),
  ).toBeDisabled();
  for (const name of ["q3.md", "Q4 minutes.md"]) {
    await page.goto("/data");
    await folder(page, "Board minutes").getByRole("link", { name }).click();
    await expect(page.getByRole("heading", { level: 1, name })).toBeVisible();
    await page.getByRole("button", { name: "Move" }).click();
    const move = page.getByRole("dialog", { name: `Move ${name}` });
    await expect(move.getByLabel("Set")).toHaveText("General");
    await expect(move).toContainText("Renewals reads General.");
    await move.getByRole("button", { name: "Move" }).click();
    await expect(
      page.getByText(
        `Moved ${name} to General. Renewals reads it with its first load.`,
      ),
    ).toBeVisible();
  }
  await page.goto("/data");
  await folder(page, "Board minutes")
    .getByRole("button", { name: "Delete Board minutes" })
    .click();
  await page
    .getByRole("dialog", { name: "Delete Board minutes?" })
    .getByRole("button", { name: "Delete" })
    .click();
  await expect(folder(page, "Board minutes")).toHaveCount(0);
  await expect(card(page, "Board minutes")).toHaveCount(0);
  await expect(card(page, "General")).toContainText("3 documents");

  // A document is deleted from its page.
  await folder(page, "General")
    .getByRole("link", { name: "notes.txt" })
    .click();
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  await page
    .getByRole("dialog", { name: "Delete notes.txt?" })
    .getByRole("button", { name: "Delete" })
    .click();
  await expect(page).toHaveURL(/\/data$/);
  await expect(card(page, "General")).toContainText("2 documents");
  // The palette finds documents.
  await page.keyboard.press("ControlOrMeta+k");
  await page.getByRole("combobox").fill("Q4 minutes");
  await page.getByRole("option", { name: /Q4 minutes\.md/ }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Q4 minutes.md" }),
  ).toBeVisible();
  await expect(page.getByLabel("Document text")).toContainText("Initech left.");
});

test("drops two files with the same bytes: one is uploaded, the other says where it is", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  await drop(
    page,
    page.getByRole("heading", { level: 1, name: "Connectors" }),
    [
      { name: "a.md", text: "# Same\n\nGlobex.\n", type: "text/markdown" },
      { name: "b.md", text: "# Same\n\nGlobex.\n", type: "text/markdown" },
    ],
  );
  const cards = page
    .getByRole("list", { name: "Uploads" })
    .getByRole("listitem");
  // Whichever created it first uploaded it; the other found it pending,
  // sent the same bytes, and says it is the other's.
  await expect(
    cards.filter({ hasText: /Already uploaded: it’s in General as [ab]\.md/ }),
  ).toHaveCount(1);
  await expect(cards.filter({ hasText: "Uploaded into General" })).toHaveCount(
    1,
  );
  await expect(card(page, "General")).toContainText("1 document");
});

test("makes General for a workspace's first upload from its onboarding wizard", async ({
  page,
}) => {
  // A new workspace has no Data pages yet: it uploads from its onboarding
  // wizard's sources step.
  await signInToNewWorkspace(page);
  await startFirstOntology(page);
  await expect(
    page.getByRole("heading", { name: "No documents yet" }),
  ).toBeVisible();
  const chooser = page.waitForEvent("filechooser");
  await page.getByRole("button", { name: "Upload documents" }).click();
  await (await chooser).setFiles([file("first.md", "# First\n")]);
  await expect(uploadCard(page, "first.md")).toContainText(
    "Uploaded into General",
  );
  await expect(uploadCard(page, "first.md")).toContainText(
    "No ontology reads General yet.",
  );
  // General is made, and chosen for the first ontology.
  await expect(
    page
      .getByRole("list", { name: "Document sets" })
      .getByRole("checkbox", { name: /General/ }),
  ).toBeChecked();
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("button", { name: "Skip" }).click();
  await page.getByRole("button", { name: "Create and generate" }).click();
  await createdOntology(page);
  // Then the Data tree holds it.
  await page.goto("/data");
  await expect(
    folder(page, "General").getByRole("link", { name: "first.md" }),
  ).toBeVisible();
});

test("makes General for a workspace's first upload in the Data tree, after an ontology on connectors only", async ({
  page,
}) => {
  // The first ontology reads Stripe alone, connected from the wizard: the
  // workspace then has Data pages and no documents.
  await signInToNewWorkspace(page);
  await startFirstOntology(page);
  await page.getByRole("button", { name: "Add data" }).first().click();
  const connect = page.locator("dialog[open]");
  await connect.getByRole("button", { name: "Stripe", exact: true }).click();
  await connect
    .getByLabel("Restricted API key")
    .fill("sk_test_51E2eFirstOntology");
  await connect.getByRole("button", { name: "Test connection" }).click();
  await connect.getByRole("button", { name: "Connect and sync" }).click();
  await expect(connect).toHaveCount(0);
  // Chosen once its first sync ends.
  await expect(
    page.getByRole("list", { name: "Connectors" }).getByRole("checkbox"),
  ).toBeChecked({ timeout: 15_000 });
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("button", { name: "Skip" }).click();
  await page.getByRole("button", { name: "Create and generate" }).click();
  await createdOntology(page);

  await page.goto("/data");
  await expect(folder(page, "Documents")).toContainText("No documents yet");
  await uploadInData(page, [file("first.md", "# First\n")]);
  await expect(uploadCard(page, "first.md")).toContainText(
    "Uploaded into General",
  );
  await expect(uploadCard(page, "first.md")).toContainText(
    "No ontology reads General yet.",
  );
  await expect(
    folder(page, "General").getByRole("link", { name: "first.md" }),
  ).toBeVisible();
  await expect(folder(page, "Documents")).not.toContainText("No documents yet");
});

test("shows a plain member the documents without changes", async ({ page }) => {
  await signIn(page);
  await uploadInData(page, [file("contract.md", CONTRACT)]);
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto("/data");
  await expect(
    folder(page, "General").getByRole("link", { name: "contract.md" }),
  ).toBeVisible();
  for (const name of [
    "Upload documents",
    "New set",
    "Upload into General",
    "Rename General",
    "Rename contract.md",
  ])
    await expect(page.getByRole("button", { name })).toHaveCount(0);
  await folder(page, "General")
    .getByRole("link", { name: "contract.md" })
    .click();
  await expect(page.getByLabel("Document text")).toContainText(
    "Master services agreement",
  );
  for (const name of ["Move", "Delete"])
    await expect(page.getByRole("button", { name })).toHaveCount(0);
});

test("generates from the current version's sets, and none when none is chosen", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 300 });
  await signIn(page);
  await uploadInData(page, [file("contract.md", CONTRACT)]);
  await newSet(page, "Board");
  // Version 1 reads no set: a first generation picks none.
  await page.goto(`${O}/versions/ver_seed_1`);
  await expect(page.getByText("Reads no documents")).toBeVisible();
  await page.goto(`${O}/generate`);
  const picker = page.getByRole("list", { name: "Document sets" });
  const general = picker.getByRole("checkbox").first();
  await expect(picker.getByRole("listitem")).toHaveCount(2);
  await expect(general).not.toBeChecked();
  await expect(
    page.getByText("Stripe and Shop database: 21 tables.", { exact: true }),
  ).toBeVisible();
  // A set's documents are listed for context.
  await picker
    .getByRole("button", { name: "Show General’s documents" })
    .click();
  await expect(
    picker.getByRole("list", { name: "General documents" }),
  ).toContainText("contract.md");
  // Documents are uploaded here, into a set, without leaving for Data.
  await expect(
    page.getByRole("button", { name: "Upload documents" }),
  ).toBeEnabled();
  await expect(
    picker.getByRole("button", { name: "Upload into General" }),
  ).toBeEnabled();

  // Version 3 reads General: Generate starts from it.
  await makeContractsCurrent(page);
  await expect(page.getByText("Reads General")).toBeVisible();
  await page.goto(`${O}/generate`);
  await expect(general).toBeChecked();
  await expect(
    page.getByText(
      "Stripe and Shop database: 21 tables; General: 1 document.",
      { exact: true },
    ),
  ).toBeVisible();
  // Cleared, it sends none, and says so.
  await general.uncheck();
  await expect(
    page.getByText("Stripe and Shop database: 21 tables.", { exact: true }),
  ).toBeVisible();
  const sent = page.waitForRequest(/ont_acme_contracts:generate$/);
  await page.getByRole("button", { name: "Generate" }).click();
  expect((await sent).postDataJSON()).toMatchObject({ documentSetIds: [] });
  await expect(page).toHaveURL(/\/generations\/gen_/);
  // Its version reads none.
  await expect(
    page.getByRole("link", { name: /Review version 4/ }),
  ).toBeVisible({ timeout: 15_000 });
  await page.getByRole("link", { name: /Review version 4/ }).click();
  await expect(page.getByText("Reads no documents")).toBeVisible();

  // A link's sets win over the current version's.
  await page.goto(`${O}/generate?sets=`);
  await expect(general).not.toBeChecked();
  await page.goto(`${O}/generate`);
  await picker.getByRole("checkbox").nth(1).check();
  const both = page.waitForRequest(/ont_acme_contracts:generate$/);
  await page.getByRole("button", { name: "Generate" }).click();
  expect((await both).postDataJSON()).toMatchObject({
    documentSetIds: ["set_acme_general", expect.stringMatching(/^set_/)],
  });
});

test("waits for the sets and the current version before Generate can send any", async ({
  page,
}) => {
  await signIn(page);
  await makeContractsCurrent(page);
  // The set list and the current version answer late.
  let release = () => {};
  const held = new Promise<void>((r) => (release = r));
  for (const url of [
    "**/papi/v1/workspaces/ws_acme/foundry/document-sets",
    "**/papi/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts/versions/ver_seed_3",
  ])
    await page.route(url, async (route) => {
      await held;
      await route.continue();
    });
  await page.goto(`${O}/generate`);
  const generate = page.getByRole("button", { name: "Generate", exact: true });
  // The connectors have loaded; what Generate would send waits for the sets.
  await expect(page.getByRole("checkbox", { name: /^Stripe/ })).toBeChecked();
  await expect(page.getByText("…", { exact: true })).toBeVisible();
  await expect(page.getByRole("list", { name: "Document sets" })).toHaveCount(
    0,
  );
  await expect(generate).toBeDisabled();
  release();
  const general = page
    .getByRole("list", { name: "Document sets" })
    .getByRole("checkbox", { name: /General/ });
  // Version 3's set, once both have loaded.
  await expect(general).toBeChecked();
  await expect(general).toBeEnabled();
  await expect(
    page.getByText("Stripe and Shop database: 21 tables; General.", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(generate).toBeEnabled();
  const sent = page.waitForRequest(/ont_acme_contracts:generate$/);
  await generate.click();
  expect((await sent).postDataJSON()).toMatchObject({
    documentSetIds: ["set_acme_general"],
  });
});

test("says when the sets couldn't load, and retries", async ({ page }) => {
  await signIn(page);
  await makeContractsCurrent(page);
  const sets = "**/papi/v1/workspaces/ws_acme/foundry/document-sets";
  await page.route(sets, (route) =>
    route.fulfill({
      status: 503,
      contentType: "application/json",
      body: JSON.stringify({ code: 14, message: "the sets are unavailable" }),
    }),
  );
  await page.goto(`${O}/generate`);
  const failed = page
    .getByRole("alert")
    .filter({ hasText: "Couldn’t load the document sets" });
  await expect(failed).toContainText("the sets are unavailable", {
    timeout: 15_000,
  });
  await expect(
    page.getByRole("button", { name: "Generate", exact: true }),
  ).toBeDisabled();
  await page.unroute(sets);
  await failed.getByRole("button", { name: "Retry" }).click();
  await expect(
    page
      .getByRole("list", { name: "Document sets" })
      .getByRole("checkbox", { name: /General/ }),
  ).toBeChecked();
  await expect(
    page.getByRole("button", { name: "Generate", exact: true }),
  ).toBeEnabled();
});

test("generates from at most 20 sets, and says so when that is reached", async ({
  page,
}) => {
  // Merged ontologies left Acme 21 sets.
  await mock.configure({
    legacyDocumentSets: Array.from({ length: 20 }, (_, i) => `Legacy ${i + 1}`),
  });
  await signIn(page);
  await page.goto(`${O}/generate`);
  const picker = page.getByRole("list", { name: "Document sets" });
  await expect(picker.getByRole("checkbox")).toHaveCount(21);
  await page
    .getByRole("main")
    .getByRole("button", { name: "Select all" })
    .last()
    .click();
  await expect(picker.getByRole("checkbox", { checked: true })).toHaveCount(20);
  const last = picker
    .getByRole("listitem")
    .filter({ hasText: "Legacy 20" })
    .getByRole("checkbox");
  await expect(last).toBeDisabled();
  await expect(
    page.getByText(
      "A generation reads at most 20 document sets: uncheck one to choose another.",
    ),
  ).toBeVisible();
  await expect(
    page.getByText(
      "Stripe and Shop database: 21 tables; General, Legacy 1, Legacy 2, and 17 more.",
      { exact: true },
    ),
  ).toBeVisible();
  // One unchecked, another can be chosen.
  await picker.getByRole("checkbox", { name: /General/ }).uncheck();
  await expect(last).toBeEnabled();
  await last.check();
  const sent = page.waitForRequest(/ont_acme_contracts:generate$/);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  const body = (await sent).postDataJSON();
  expect(body.documentSetIds).toHaveLength(20);
  expect(body.documentSetIds).not.toContain("set_acme_general");
  await expect(page).toHaveURL(/\/generations\/gen_/);
});

test("reads an upload into a set two ontologies read into both", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await makeContractsCurrent(page);
  await load(page, O, "Load version 3 into contracts?");
  await load(page, R, "Load version 1 into subscriptions?");
  await uploadInData(page, [file("contract.md", CONTRACT)]);
  await expect(uploadCard(page, "contract.md")).toContainText(
    "Renewals and Customer contracts read it now.",
  );
  await expect(card(page, "General")).toContainText("Read by 2 ontologies");
  await expect(card(page, "General")).toContainText(
    "Customer contracts and Renewals read General.",
  );
  await folder(page, "General")
    .getByRole("link", { name: "contract.md" })
    .click();
  const ontologies = page.getByRole("list", { name: "Ontologies" });
  for (const name of ["Customer contracts", "Renewals"]) {
    const it = ontologies.getByRole("listitem", { name });
    await expect(it).toContainText("Its routes read General");
    await expect(it).toContainText("Needs review · 1", { timeout: 20_000 });
  }
  // Each opens that ontology's view of it.
  await ontologies
    .getByRole("listitem", { name: "Renewals" })
    .getByRole("link", { name: "Open in Renewals" })
    .click();
  await expect(page).toHaveURL(
    /\/ontology\/ont_acme_renewals\/documents\/doc_/,
  );
  await expect(page.getByLabel("Document text")).toContainText(
    "Umbrella Holdings introduced the parties",
  );
  await page.getByRole("link", { name: "Open in Connectors" }).click();
  await expect(page).toHaveURL(/\/data\/documents\/doc_/);
});

test("shows a version's sources: tables and document routes per type and link", async ({
  page,
}) => {
  await signIn(page);
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("tab", { name: /^Sources/ }).click();
  const table = page.getByRole("table", { name: "Sources" });
  await expect(
    table.getByRole("row").first().getByRole("columnheader"),
  ).toHaveText([
    "Type or link",
    /Stripe · customers.*customers/,
    /Stripe · invoices.*invoices/,
    /Documents · General.*contracts/,
  ]);
  const cells = (name: string) =>
    table
      .getByRole("row")
      .filter({ has: page.getByRole("rowheader", { name, exact: true }) })
      .getByRole("cell");
  // Only documents create a Contract, with the props the route lets them fill.
  await expect(cells("Contract")).toHaveText([
    "—",
    "—",
    "Createssigned_on, term_months",
  ]);
  // Customers: created by their table, looked up by invoices, found by
  // documents, which state their currency too.
  await expect(cells("Customer")).toHaveText([
    "Createscompany_name, currency, customer_id, email",
    "Looks upcustomer_id",
    "Findscurrency",
  ]);
  await expect(cells("signed_with")).toHaveText(["—", "—", "States it"]);
  await expect(cells("billed_to")).toHaveText(["—", "States it", "—"]);

  // The types name their identifiers and how they compare.
  await page.getByRole("tab", { name: /^Types/ }).click();
  const customer = page.getByRole("region", { name: "Customer" });
  await expect(
    customer.getByRole("row").filter({ hasText: "customer_id" }),
  ).toContainText("Identifier · code");
  await expect(
    customer.getByRole("row").filter({ hasText: /^email/ }),
  ).toContainText("Identifier · email");
});

test("opens a property's quote on its span; reads only its version's sets", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await makeContractsCurrent(page);
  await uploadInData(page, [file("contract.md", CONTRACT)]);
  await newSet(page, "Other");
  await uploadInData(
    page,
    [file("notes.md", "# Notes\n\nGlobex Corporation renewed.\n")],
    "Other",
  );
  await page.goto(O);
  await page
    .getByRole("status")
    .filter({ hasText: "Version 3 isn’t loaded yet" })
    .getByRole("button", { name: "Load data" })
    .click();
  const dialog = page.getByRole("dialog", {
    name: "Load version 3 into contracts?",
  });
  // It says up front what the version reads: its sets, not Other.
  await expect(dialog).toContainText(
    "From General: 1 document, read for the names and facts they hold.",
  );
  await dialog.getByRole("button", { name: "Load" }).click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 20_000 });

  // The ontology's documents are its sets': not Other's.
  await page.goto(`${O}/documents`);
  await expect(row(page, "contract.md")).toContainText("Needs review · 1");
  await expect(row(page, "notes.md")).toHaveCount(0);

  // The contract created its Contract, with the props it states.
  await row(page, "contract.md")
    .getByRole("link", { name: "contract.md" })
    .click();
  await page.getByRole("tab", { name: /^Entities/ }).click();
  await page
    .getByRole("region", { name: "Contract" })
    .getByRole("link", { name: "Master services agreement" })
    .click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Master services agreement" }),
  ).toBeVisible();
  await expect(page.getByText("Created by documents")).toBeVisible();
  const props = page.locator(".ex-props");
  await expect(props).toContainText("signed_on");
  await expect(props).toContainText("2026-10-01");
  await expect(props).toContainText("term_months");
  await expect(props).toContainText("24");
  await page.getByRole("tab", { name: /^Mentions/ }).click();
  await expect(
    page.getByText("section 1 · created from the text"),
  ).toBeVisible();
  await page.getByRole("tab", { name: /^Overview/ }).click();

  // Its quote opens the document on the span that states it.
  await page
    .getByRole("link", { name: "term_months quoted in contract.md" })
    .click();
  await expect(page).toHaveURL(/\/documents\/doc_\w+(\?[^#]*)?#s\d+-\d+-\d+$/);
  const span = page.getByLabel("Document text").locator("mark.is-span");
  await expect(span).toHaveText(
    "The Agreement runs for 24 months and renews for 12 months unless a party gives 90 days’ notice.",
  );
  await expect(span).toBeInViewport();

  // Moved out of its version's sets, the current load still holds it until
  // the next load drops it.
  await page.goto("/data");
  await folder(page, "General")
    .getByRole("link", { name: "contract.md" })
    .click();
  await page.getByRole("button", { name: "Move" }).click();
  const move = page.getByRole("dialog", { name: "Move contract.md" });
  await choose(move.getByLabel("Set"), "Other");
  await expect(move).toContainText("No ontology reads Other yet.");
  await move.getByRole("button", { name: "Move" }).click();
  await expect(
    page.getByText("Moved contract.md to Other. No ontology reads Other yet."),
  ).toBeVisible();
  await expect(
    page
      .getByRole("list", { name: "Ontologies" })
      .getByRole("listitem", { name: "Customer contracts" }),
  ).toContainText(
    "Its current load holds it, but its version doesn’t read Other: the next load drops it.",
  );
});

test("stores a document of a set its version reads but no route names", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200, ontologyStepMs: 300 });
  await signIn(page);
  await page.goto("/data");
  await newSet(page, "Other");
  await uploadInData(
    page,
    [file("notes.md", "# Notes\n\nGlobex Corporation renewed.\n")],
    "Other",
  );
  // A generation that picks Other reads it; the mock's agent routes none.
  await page.goto(`${O}/generate?sets=`);
  await page
    .getByRole("list", { name: "Document sets" })
    .getByRole("listitem")
    .filter({ hasText: "Other" })
    .getByRole("checkbox")
    .check();
  await page.getByRole("button", { name: "Generate" }).click();
  await page
    .getByRole("link", { name: /Review version 4/ })
    .click({ timeout: 15_000 });
  await expect(page.getByText("Reads Other")).toBeVisible();
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 4 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await page.getByRole("button", { name: "Load data" }).click();
  const dialog = page.getByRole("dialog", {
    name: "Load version 4 into contracts?",
  });
  await expect(dialog).toContainText(
    "From Other: none read. 1 document in sets no route reads is stored, not read.",
  );
  await dialog.getByRole("button", { name: "Load" }).click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 20_000 });
  await page.goto(`${O}/documents`);
  await expect(row(page, "notes.md")).toContainText("Not read by its version");
  await row(page, "notes.md").getByRole("link", { name: "notes.md" }).click();
  await expect(
    page
      .getByRole("status")
      .filter({ hasText: "The current load didn’t read it" }),
  ).toContainText("reads nothing from the Other set");
  // Its text is stored, with nothing marked in it.
  const notes = page.getByLabel("Document text");
  await expect(notes).toContainText("Globex Corporation renewed.");
  await expect(notes.locator("mark")).toHaveCount(0);
});
