import { readFileSync } from "node:fs";
import {
  dataTree,
  expect,
  explorer,
  file,
  mock,
  test,
  uploadCard,
} from "../e2e/support/mock";
import { caption, click, poster, saveVideo, type, videoPage } from "./support";

// docs: foundry/start-here/quickstart, the whole path in one video: synced
// data, a generated draft, made current, loaded, explored, then asked.

const O = "/ontology/ont_acme_contracts";
const CONTRACT = file(
  "contract.md",
  readFileSync(new URL("../../mock/fixtures/contract.md", import.meta.url)),
);

test("tour: video", async ({ browser }) => {
  await mock.configure({
    ontologyStepMs: 900,
    loadStepMs: 500,
    askStepMs: 300,
  });
  const page = await videoPage(browser, "Helix Foundry in two minutes");
  await page.goto("/");
  await expect(
    page.getByRole("heading", { level: 1, name: "Ask" }),
  ).toBeVisible();
  await caption(page, "Foundry opens on Ask: questions about your data", 2500);

  await caption(page, "1. Connect and sync your data, and upload documents");
  await click(
    page
      .getByRole("navigation", { name: "Main navigation" })
      .getByRole("link", { name: "Connectors" }),
  );
  await page.waitForTimeout(2000);
  const chooser = page.waitForEvent("filechooser");
  await click(explorer(page).getByRole("button", { name: "Upload documents" }));
  await (await chooser).setFiles(CONTRACT);
  await expect(uploadCard(page, "contract.md")).toContainText("Uploaded");
  await page.waitForTimeout(1500);
  await click(
    dataTree(page).getByRole("link", { name: "Stripe", exact: true }),
  );
  await page.waitForTimeout(2200);

  await caption(page, "2. Generate an ontology from the synced tables");
  await page.goto(O);
  await page.waitForTimeout(1500);
  await click(page.getByRole("link", { name: "Generate", exact: true }));
  await type(
    page.getByLabel("Goal"),
    "Which customers each contract is with, and what they are billed",
  );
  await click(page.getByRole("button", { name: "Generate", exact: true }));
  await expect(page.getByText("Succeeded", { exact: true })).toBeVisible({
    timeout: 20_000,
  });
  await page.waitForTimeout(1200);

  await caption(page, "3. Review the draft and make it current");
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.waitForTimeout(1800);
  await click(page.getByRole("button", { name: "Make current" }));
  await click(
    page.getByRole("dialog").getByRole("button", { name: "Make current" }),
  );
  await page.waitForTimeout(1200);

  await caption(page, "4. Load it into the ontology's Helix database");
  await click(
    page
      .getByRole("complementary", { name: "Ontologies" })
      .getByRole("link", { name: "Overview" }),
  );
  await click(
    page.getByRole("main").getByRole("button", { name: "Load data" }).first(),
  );
  await click(page.getByRole("dialog").getByRole("button", { name: "Load" }));
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 30_000 });
  await page.waitForTimeout(1200);

  await caption(page, "5. Explore the graph, and read it as records");
  await click(page.getByRole("link", { name: "Open Explorer" }));
  await expect(
    page.getByRole("application", { name: /^Graph of contracts: \d+ nodes/ }),
  ).toBeVisible({ timeout: 20_000 });
  // The seeded view draws its links a moment after its records.
  await page.waitForTimeout(6000);
  await poster(page, "start-here", "tour");
  await click(page.getByRole("radio", { name: "Records" }));
  await page.waitForTimeout(2500);

  await caption(page, "6. Ask it questions in plain language");
  await click(
    page
      .getByRole("navigation", { name: "Main navigation" })
      .getByRole("link", { name: "Ask" }),
  );
  const suggestion = page
    .getByRole("group", { name: "Suggested questions" })
    .getByRole("button")
    .first();
  const question = (await suggestion.textContent())!;
  await click(suggestion);
  await click(
    page
      .getByRole("form", { name: "Ask a question" })
      .getByRole("button", { name: "Ask", exact: true }),
  );
  await expect(
    page
      .getByRole("article", { name: `Question: ${question}` })
      .getByRole("region", { name: "Answer" }),
  ).toBeVisible({ timeout: 20_000 });
  await page.waitForTimeout(3000);
  await saveVideo(page, "start-here", "tour");
});
