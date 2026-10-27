import { readFileSync } from "node:fs";
import type { Page } from "@playwright/test";
import {
  dataTree,
  expect,
  explorer,
  mock,
  signIn,
  test,
  uploadCard,
  uploadInData,
} from "../e2e/support/mock";
import { caption, click, poster, saveVideo, shot, videoPage } from "./support";

// docs: foundry/ontology (loads, and what an ontology reads of the
// documents), foundry/data (a document's page once a load read it) and
// foundry/explore (the Explorer, Records, an entity's page and comparing
// loads).

const O = "/ontology/ont_acme_contracts";
const CONTRACT = {
  name: "contract.md",
  mimeType: "text/markdown",
  buffer: readFileSync(
    new URL("../../mock/fixtures/contract.md", import.meta.url),
  ),
};
const graph = (page: Page) =>
  page.getByRole("application", { name: /^Graph of contracts: \d+ nodes/ });
const loaded = (page: Page) =>
  expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 30_000 });

async function makeVersion3Current(page: Page) {
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
}

/** Loads the current version from the Loads page; the load's id. */
async function load(page: Page, shotDialog = false) {
  await page.goto(`${O}#loads`);
  await page
    .getByRole("main")
    .getByRole("button", { name: "Load data" })
    .first()
    .click();
  const dialog = page.getByRole("dialog", {
    name: "Load version 3 into contracts?",
  });
  await expect(dialog).toBeVisible();
  if (shotDialog) await shot(page, "ontology", "load-dialog");
  await dialog.getByRole("button", { name: "Load" }).click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await loaded(page);
  return new URL(page.url()).pathname.split("/").pop()!;
}

test("loads: upload a document, load a version and explore it", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 400, documentOutcome: "fail" });
  await signIn(page);
  await uploadInData(page, [CONTRACT]);
  await makeVersion3Current(page);
  const first = await load(page);
  await mock.configure({ documentOutcome: "succeed" });
  await load(page, true);
  await shot(page, "ontology", "load");
  await page.goto(`${O}#loads`);
  await expect(page.getByRole("grid", { name: "Loads" })).toBeVisible();
  // Wide enough for every column, the row actions included.
  await page.setViewportSize({ width: 1920, height: 800 });
  await shot(page, "ontology", "loads");
  await page.setViewportSize({ width: 1280, height: 800 });

  await page.goto(`${O}/documents`);
  await expect(page.getByRole("grid", { name: "Documents" })).toBeVisible();
  await shot(page, "ontology", "documents");
  await page.getByRole("link", { name: "contract.md" }).first().click();
  await expect(page.getByLabel("Document text")).toContainText("Globex");
  await shot(page, "ontology", "document");

  // The same document in Data: its text, set, and the ontologies that
  // read it, with what each made of it.
  await page.goto("/data");
  await dataTree(page).getByRole("link", { name: "contract.md" }).click();
  await expect(
    page.getByRole("list", { name: "Ontologies" }).getByRole("listitem"),
  ).toHaveCount(2);
  await expect(page.getByLabel("Document text")).toContainText("Globex");
  await shot(page, "data", "document");

  await page.goto("/explore/ont_acme_contracts");
  await expect(graph(page)).toBeVisible({ timeout: 20_000 });
  // Let the seeded first view land, so Globex is on the canvas.
  await page.waitForTimeout(2500);
  const jump = page.getByRole("combobox", { name: "Jump to a node" });
  await jump.fill("Globex");
  // Globex is on the canvas already, so Enter selects it.
  await jump.press("Enter");
  await expect(
    page.getByRole("complementary", { name: "Inspector" }),
  ).toContainText("Globex Corporation", { timeout: 15_000 });
  await page.waitForTimeout(1500);
  await shot(page, "explore", "explorer-node");

  await page.getByRole("radio", { name: "Records" }).click();
  const types = page.getByRole("list", { name: "Types" });
  await types.getByRole("button", { name: /^Customer/ }).click();
  await expect(
    page.getByRole("grid", { name: "Customer records" }),
  ).toBeVisible();
  await shot(page, "explore", "records");
  await page
    .getByRole("grid", { name: "Customer records" })
    .getByRole("row")
    .nth(1)
    .click();
  await expect(page.getByRole("link", { name: "Open its page" })).toBeVisible();
  await shot(page, "explore", "record");
  await page.getByRole("link", { name: "Open its page" }).click();
  await expect(page).toHaveURL(/\/entities\//);
  await page.waitForTimeout(1500);
  await shot(page, "explore", "entity");

  await page.goto(`${O}#loads`);
  await page
    .getByRole("grid", { name: "Loads" })
    .getByRole("row")
    .filter({ has: page.locator(`a[href$="/loads/${first}"]`) })
    .getByRole("button", { name: /^More actions for / })
    .click();
  await page.getByRole("menuitem", { name: "Compare with current" }).click();
  await expect(graph(page)).toHaveCount(2, { timeout: 20_000 });
  const diff = page.getByRole("table", { name: "Counts per type and link" });
  await expect(diff).toBeVisible({ timeout: 20_000 });
  await page.waitForTimeout(2500);
  await page
    .getByTestId("diff-row-Contract")
    .getByRole("button", { name: /Contract/ })
    .click();
  await expect(page.getByTestId("diff-keys-Contract")).toContainText(
    "Added in v3",
  );
  await page.getByTestId("diff-keys-Contract").scrollIntoViewIfNeeded();
  await shot(page, "explore", "compare-diff");

  // Loaded and explored, the ontology is set up: its overview is the
  // current version's schema alone.
  await page.goto(O);
  await expect(
    page.getByRole("heading", { level: 2, name: "Version 3" }),
  ).toBeVisible();
  await expect(page.getByRole("list", { name: "Setup steps" })).toHaveCount(0);
  await shot(page, "ontology", "overview");
});

test("loads: video", async ({ browser, page: setup }) => {
  await mock.configure({ loadStepMs: 700 });
  await signIn(setup);
  await makeVersion3Current(setup);
  const page = await videoPage(browser, "Load and explore your data");
  await page.goto("/data");
  await caption(
    page,
    "Upload contracts, notes, table files or PDFs in Connectors",
  );
  await page.waitForTimeout(1800);
  const chooser = page.waitForEvent("filechooser");
  await click(explorer(page).getByRole("button", { name: "Upload documents" }));
  await (await chooser).setFiles(CONTRACT);
  await expect(uploadCard(page, "contract.md")).toContainText("Uploaded");
  await caption(page, "It goes into a set, which the current version reads");
  await page.waitForTimeout(2200);
  await caption(
    page,
    "A load writes the current version into the ontology's database",
  );
  await page.goto(`${O}#loads`);
  await page.waitForTimeout(1200);
  await click(
    page.getByRole("main").getByRole("button", { name: "Load data" }).first(),
  );
  await caption(page, "It says what it reads, and what it may spend");
  await page.waitForTimeout(2200);
  await click(page.getByRole("dialog").getByRole("button", { name: "Load" }));
  await caption(
    page,
    "Tables first, then documents; the page follows each step",
  );
  await loaded(page);
  await page.waitForTimeout(1500);
  await caption(page, "The Explorer draws what the load wrote");
  await click(
    page
      .getByRole("navigation", { name: "Main navigation" })
      .getByRole("link", {
        name: "Explorer",
      }),
  );
  await expect(graph(page)).toBeVisible({ timeout: 20_000 });
  await page.waitForTimeout(2500);
  await caption(page, "Jump to any record by name");
  const jump = page.getByRole("combobox", { name: "Jump to a node" });
  await click(jump);
  await jump.pressSequentially("Globex", { delay: 60 });
  await jump.press("Enter");
  await expect(
    page.getByRole("complementary", { name: "Inspector" }),
  ).toContainText("Globex Corporation", { timeout: 15_000 });
  await page.waitForTimeout(1500);
  await poster(page, "explore", "load-and-explore");
  await page.waitForTimeout(1000);
  await caption(page, "Records lists the same data as tables, by type");
  await click(page.getByRole("radio", { name: "Records" }));
  await click(
    page.getByRole("list", { name: "Types" }).getByRole("button", {
      name: /^Customer/,
    }),
  );
  await page.waitForTimeout(1200);
  await click(
    page
      .getByRole("grid", { name: "Customer records" })
      .getByRole("row")
      .nth(1),
  );
  await page.waitForTimeout(2500);
  await saveVideo(page, "explore", "load-and-explore");
});
