import { readFileSync } from "node:fs";
import type { Page } from "@playwright/test";
import {
  choose,
  csrfToken,
  dataTree,
  expect,
  file,
  mock,
  signIn,
  test,
  uploadInData,
} from "../e2e/support/mock";
import {
  caption,
  click,
  poster,
  saveVideo,
  shot,
  type,
  videoPage,
} from "./support";

// docs: foundry/ontology (create an ontology, generate a draft from
// connectors grouped by kind and document sets, review and choose versions).

const O = "/ontology/ont_acme_contracts";
const GOAL =
  "Track each customer's contracts, invoices and payments, and who signed them";
const CONTRACT = file(
  "contract.md",
  readFileSync(new URL("../../mock/fixtures/contract.md", import.meta.url)),
);

/**
 * A second PostgreSQL connection, synced, so Generate nests the kind's two
 * under one choice.
 */
async function addReplica(page: Page) {
  await page.goto("/data");
  const headers = {
    origin: new URL(page.url()).origin,
    "x-helix-csrf": await csrfToken(page),
  };
  const base = "/papi/v1/workspaces/ws_acme/foundry/connectors";
  const created = await page.request.post(base, {
    headers,
    data: {
      name: "Billing replica",
      config: {
        postgres: {
          host: "billing.acme.example.com",
          database: "billing",
          user: "foundry_readonly",
          password: "docs-example",
        },
      },
    },
  });
  expect(created.status()).toBe(200);
  const { connector } = await created.json();
  const sync = await page.request.post(`${base}/${connector.id}:sync`, {
    headers,
    data: {},
  });
  expect(sync.status()).toBe(200);
  await expect(
    dataTree(page).getByRole("treeitem", { name: "Billing replica, Synced" }),
  ).toBeVisible({ timeout: 20_000 });
}

/** A second document set, with a document, for Generate's documents. */
async function boardMinutes(page: Page) {
  await dataTree(page)
    .getByRole("treeitem", { name: "Documents", exact: true })
    .getByRole("button", { name: "New set" })
    .first()
    .click();
  const create = page.getByRole("dialog", { name: "New document set" });
  await create.getByLabel("Name").fill("Board minutes");
  await create.getByRole("button", { name: "Create set" }).click();
  await uploadInData(
    page,
    [
      file(
        "board-minutes-september.md",
        "# Board minutes, September\n\nGlobex Corporation renewed for two years.\n",
      ),
    ],
    "Board minutes",
  );
}

async function makeVersion3Current(page: Page) {
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
}

/** The Ontologies panel, beside the rail on every ontology page. */
const ontologiesPanel = (page: Page) =>
  page.getByRole("complementary", { name: "Ontologies" });

test("ontology: the panel, the overview and a new one", async ({ page }) => {
  await signIn(page);
  await uploadInData(page, [CONTRACT]);
  await page.goto(O);
  await expect(
    page.getByRole("heading", { level: 2, name: "Version 1" }),
  ).toBeVisible();
  await expect(
    ontologiesPanel(page).getByRole("link", { name: /^Documents/ }),
  ).toBeVisible();
  // The rail and the panel's top: the ontologies, the open one's pages.
  await shot(page, "ontology", "ontologies", {
    x: 0,
    y: 0,
    width: 600,
    height: 340,
  });

  // An ontology not set up yet: its overview carries the setup on, here
  // at Load it. (The overview of one set up is shot in loads.spec.ts.)
  await page.goto("/ontology/ont_acme_renewals");
  await expect(
    page
      .getByRole("list", { name: "Setup steps" })
      .getByText("Load it", { exact: true }),
  ).toBeVisible();
  await shot(page, "ontology", "overview-setup");

  // New ontology, step by step.
  await ontologiesPanel(page)
    .getByRole("link", { name: "New ontology" })
    .click();
  await page.getByLabel("Ontology name").fill("Orders");
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("radio", { name: "orders in Production" }).check();
  await shot(page, "ontology", "new-ontology");
  await page.getByRole("radio", { name: "Create a new one" }).click();
  await choose(page.getByRole("combobox", { name: /^Project/ }), "Staging");
  await page.getByLabel("Database name").fill("orders-graph");
  await shot(page, "ontology", "new-database");
  await page.getByRole("radio", { name: "Use an existing one" }).click();
  await page.getByRole("radio", { name: "orders in Production" }).check();
  await page.getByRole("button", { name: "Continue" }).click();
  // One step for what it drafts from: tables, API sources and documents.
  await expect(
    page.getByRole("list", { name: "Connectors" }).getByRole("checkbox"),
  ).toHaveCount(2);
  await page
    .getByRole("list", { name: "Document sets" })
    .getByRole("checkbox")
    .first()
    .check();
  // The whole step: its tables, API sources and documents.
  await page.setViewportSize({ width: 1280, height: 1440 });
  await page.getByRole("heading", { level: 1 }).scrollIntoViewIfNeeded();
  await shot(page, "ontology", "new-ontology-data");
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByLabel("Goal").fill(GOAL);
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(
    page.getByRole("button", { name: "Create and generate" }),
  ).toBeEnabled();
  await shot(page, "ontology", "new-ontology-review");
});

test("ontology: generate a draft and make it current", async ({ page }) => {
  await mock.configure({ ontologyStepMs: 800 });
  await signIn(page);
  await addReplica(page);
  await uploadInData(page, [CONTRACT]);
  await boardMinutes(page);
  // Version 3 reads General, so Generate starts with General chosen.
  await makeVersion3Current(page);
  await page.goto(`${O}/generate`);
  await page.getByLabel("Goal").fill(GOAL);
  const connectors = page.getByRole("list", { name: "Connectors" });
  await connectors
    .getByRole("button", { name: "Show PostgreSQL connectors" })
    .click();
  // One of the two: the kind's choice shows as partly chosen.
  await page
    .getByRole("list", { name: "PostgreSQL connectors" })
    .getByRole("checkbox")
    .nth(1)
    .uncheck();
  const sets = page.getByRole("list", { name: "Document sets" });
  await expect(sets.getByRole("checkbox").first()).toBeChecked();
  await expect(sets.getByRole("checkbox").nth(1)).not.toBeChecked();
  await shot(page, "ontology", "generate");
  await sets.getByRole("button", { name: "Show General’s documents" }).click();
  await shot(
    page,
    "ontology",
    "generate-documents",
    page.locator(".hf-panel", { has: sets }),
  );

  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(page).toHaveURL(/\/generations\/gen_/);
  await expect(page.getByText("Succeeded", { exact: true })).toBeVisible({
    timeout: 15_000,
  });
  await shot(page, "ontology", "generation-done");

  await page.goto(`${O}/versions/ver_seed_3`);
  await expect(
    page.getByRole("heading", { level: 1, name: "Version 3" }),
  ).toBeVisible();
  await expect(page.getByText("Reads General")).toBeVisible();
  await shot(page, "ontology", "version");

  await page.goto(`${O}#versions`);
  await expect(
    page.getByRole("grid", { name: "Ontology versions" }),
  ).toBeVisible();
  await shot(page, "ontology", "versions");
});

test("ontology: video", async ({ browser, page }) => {
  await mock.configure({ ontologyStepMs: 1500 });
  await signIn(page);
  await uploadInData(page, [CONTRACT]);
  await makeVersion3Current(page);
  const video = await videoPage(browser, "Generate an ontology");
  await video.goto(O);
  await expect(
    video.getByRole("heading", { level: 2, name: "Version 3" }),
  ).toBeVisible();
  await caption(
    video,
    "An ontology's overview says what's left to set it up",
    2200,
  );
  await caption(video, "Generate asks Foundry's agent for a new draft");
  await click(video.getByRole("link", { name: "Generate", exact: true }));
  await caption(video, "Say what it should serve, in your words");
  await type(video.getByLabel("Goal"), GOAL);
  await caption(video, "Choose the connectors whose synced tables it reads");
  await click(video.getByRole("button", { name: "Show Stripe’s tables" }));
  await video.waitForTimeout(1200);
  await caption(
    video,
    "and the document sets it reads: the current version's to start with",
  );
  const sets = video.getByRole("list", { name: "Document sets" });
  await sets.scrollIntoViewIfNeeded();
  await video.waitForTimeout(2400);
  await click(video.getByRole("button", { name: "Generate", exact: true }));
  await caption(
    video,
    "The generation is queued, then runs; the page follows it",
  );
  await expect(video.getByText("Succeeded", { exact: true })).toBeVisible({
    timeout: 20_000,
  });
  await video.waitForTimeout(1200);
  await caption(video, "The draft is stored as a new version: review it");
  await click(video.getByRole("link", { name: /^Review version/ }));
  await video.waitForTimeout(1500);
  await poster(video, "ontology", "generate");
  await caption(video, "Make it current when it reads right");
  await click(video.getByRole("button", { name: "Make current" }));
  await video.waitForTimeout(1200);
  await click(
    video.getByRole("dialog").getByRole("button", { name: "Make current" }),
  );
  await video.waitForTimeout(1500);
  await caption(video, "The overview lists every version, and failed drafts");
  await click(ontologiesPanel(video).getByRole("link", { name: "Overview" }));
  await video.waitForTimeout(2500);
  await saveVideo(video, "ontology", "generate");
});

test("ontology: create video", async ({ browser }) => {
  await mock.configure({ ontologyStepMs: 1500 });
  const video = await videoPage(browser, "Create an ontology");
  await video.goto(O);
  await expect(
    video.getByRole("heading", { level: 2, name: "Version 1" }),
  ).toBeVisible();
  await caption(video, "The Ontologies panel lists the workspace's ontologies");
  await video.waitForTimeout(1200);
  await click(
    ontologiesPanel(video).getByRole("link", { name: "New ontology" }),
  );
  await caption(video, "Name it, then go step by step");
  await type(video.getByLabel("Ontology name"), "Orders");
  await click(video.getByRole("button", { name: "Continue" }));
  await caption(video, "Choose the Helix database its loads write into");
  await click(video.getByRole("radio", { name: "orders in Production" }));
  await click(video.getByRole("button", { name: "Continue" }));
  await caption(
    video,
    "Choose what it drafts from: synced tables, API sources, documents",
  );
  await video.waitForTimeout(1800);
  await caption(video, "Upload documents in place, into a set it reads");
  const chooser = video.waitForEvent("filechooser");
  await click(video.getByRole("button", { name: "Upload documents" }));
  await (await chooser).setFiles(CONTRACT);
  await expect(
    video
      .getByRole("list", { name: "Document sets" })
      .getByRole("checkbox")
      .first(),
  ).toBeChecked({ timeout: 15_000 });
  await video.waitForTimeout(1500);
  await click(video.getByRole("button", { name: "Continue" }));
  await caption(video, "Say what it should serve, in your words");
  await type(video.getByLabel("Goal"), GOAL);
  await click(video.getByRole("button", { name: "Continue" }));
  await caption(video, "Review, then create it with its first draft", 1800);
  await poster(video, "ontology", "create-an-ontology");
  await click(video.getByRole("button", { name: "Create and generate" }));
  await caption(video, "Its agent drafts a first version; the page follows it");
  await expect(video.getByText("Succeeded", { exact: true })).toBeVisible({
    timeout: 20_000,
  });
  await video.waitForTimeout(1200);
  await caption(video, "Review the draft, then make it current", 2200);
  await saveVideo(video, "ontology", "create-an-ontology");
});
