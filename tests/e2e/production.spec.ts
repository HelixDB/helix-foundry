import type { Page } from "@playwright/test";
import { expect, explorer, mock, signIn, test } from "./support/mock";

// Getting into production: creating an ontology, then querying it through the
// Helix MCP. Acme already has ontologies, so its next step is the MCP prompt.
// Only the Analyst shows them, in a card. The sample doesn't count as an
// ontology (sample.spec.ts).

const O = "/ontology/ont_acme_contracts",
  A = "/ask/ont_acme_contracts";
const production = (page: Page) =>
  page.getByRole("region", { name: "Getting into production" });
const toggle = (page: Page) =>
  production(page).getByRole("button", { name: /^Getting into production/ });

/** Makes version 3 current and loads it, so the Analyst has its composer. */
async function loadContracts(page: Page) {
  await mock.configure({ loadStepMs: 100 });
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
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
}

test("the Analyst's card copies the prompt, minimises, and floats beside the conversation", async ({
  page,
  context,
}) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await signIn(page);
  await loadContracts(page);
  await page.setViewportSize({ width: 1800, height: 900 });
  await page.goto(`${A}?new=1`);
  const card = production(page),
    copy = card.getByRole("button", { name: "Copy prompt" }),
    composer = page.getByRole("form", { name: "Ask a question" });

  // Open, with the ontology done, under the bar's right, beside the column.
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "true");
  await expect(card).toContainText("1 of 2");
  await expect(card).toContainText("Create an ontology (done)");
  await expect(card.locator('[aria-current="step"]')).toContainText(
    "Query it through the Helix MCP",
  );
  await expect(copy).toBeVisible();
  await expect(composer).toBeVisible();
  const box = (await card.boundingBox())!,
    column = (await composer.boundingBox())!;
  expect(box.x).toBeGreaterThanOrEqual(column.x + column.width);

  // Minimised to its header, and remembered.
  await toggle(page).click();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  await expect(toggle(page)).toContainText("1 of 2");
  await expect(copy).toHaveCount(0);
  await page.reload();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  await toggle(page).click();
  await page.reload();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "true");

  // The prompt names the workspace and the open ontology; copying marks the
  // step done and leaves the card open.
  await copy.click();
  await expect(page.getByText("Copied the prompt")).toBeVisible();
  const prompt = await page.evaluate(() => navigator.clipboard.readText());
  expect(prompt).toContain("# Get my Helix Foundry ontology into production");
  expect(prompt).toContain("id `ws_acme`");
  expect(prompt).toContain("id `ont_acme_contracts`");
  await expect(card).toContainText("2 of 2");
  await expect(card).toContainText("Query it through the Helix MCP (done)");
  await expect(copy).toBeVisible();

  // Both done, it starts minimised, and the prompt stays a click away.
  await page.reload();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  await expect(toggle(page)).toContainText("2 of 2");
  await toggle(page).click();
  await expect(copy).toBeVisible();
});

test("the card is a box in the top right on a laptop, and a row on a phone", async ({
  page,
}) => {
  await signIn(page);
  await loadContracts(page);
  const composer = page.getByRole("form", { name: "Ask a question" }),
    copy = production(page).getByRole("button", { name: "Copy prompt" });

  // On a laptop it starts open, in the corner, clear of a new thread's composer.
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${A}?new=1`);
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "true");
  await expect(composer).toBeVisible();
  const box = (await production(page).boundingBox())!;
  expect(box.x + box.width).toBeGreaterThan(1400);
  expect(box.width).toBeLessThan(300);
  expect(box.y + box.height).toBeLessThanOrEqual(
    (await composer.boundingBox())!.y,
  );

  // Too narrow to start open, it waits in the corner, and opens over the page.
  await page.setViewportSize({ width: 1100, height: 900 });
  await page.goto(`${A}?new=1`);
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  const closed = (await composer.boundingBox())!;
  expect((await production(page).boundingBox())!.width).toBeLessThan(300);
  await toggle(page).click();
  await expect(copy).toBeVisible();
  expect((await composer.boundingBox())!.y).toBe(closed.y);
  await toggle(page).click();

  // On a phone, a row the conversation sits under, open or not.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${A}?new=1`);
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  await toggle(page).click();
  await expect(copy).toBeVisible();
  const row = (await production(page).boundingBox())!;
  expect(row.y + row.height).toBeLessThanOrEqual(
    (await composer.boundingBox())!.y,
  );
  await toggle(page).click();
});

test("only the Analyst shows it, reading past a value it can't read", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  await expect(explorer(page)).toBeVisible();
  await expect(production(page)).toHaveCount(0);
  await page.goto("/ontology/new");
  await expect(
    page.getByRole("heading", { level: 1, name: "New ontology" }),
  ).toBeVisible();
  await expect(production(page)).toHaveCount(0);

  await page.evaluate(() =>
    localStorage.setItem("foundry.production.v1:ws_acme", "{not json"),
  );
  await page.goto(A);
  await expect(toggle(page)).toContainText("1 of 2");
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "true");
});

test("a member copies the prompt too", async ({ page }) => {
  await signIn(page, "alan@example.com");
  await page.goto(A);
  await expect(
    production(page).getByRole("button", { name: "Copy prompt" }),
  ).toBeVisible();
});
