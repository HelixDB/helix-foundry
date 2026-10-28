import { APP_ORIGIN, expect, signIn, test } from "./support/mock";

// The hosted site's / (src/app/(landing)): the landing page for a visitor who
// isn't signed in, and the app, on Ask, for anyone with a session.

const HEADING = "Your company’s data, connected into one ontology";

test("shows the landing page to a visitor who isn't signed in", async ({
  page,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(String(error)));
  await page.goto("/");
  await expect(page).toHaveURL(/\/$/);
  await expect(
    page.getByRole("heading", { level: 1, name: HEADING }),
  ).toBeVisible();
  // Its own document: none of the app's shell.
  await expect(
    page.getByRole("navigation", { name: "Main navigation" }),
  ).toHaveCount(0);

  // The figures render in the browser: the schema's cards, the Explorer's
  // legend, the setup scene, the Analyst's first question and the
  // connectors' grid.
  const schema = page.locator("#schema-root");
  await expect(schema.getByText("Merged from 3 sources")).toBeVisible();
  await expect(page.locator("#explorer-root .og-legend li")).not.toHaveCount(0);
  await page.locator("#how").scrollIntoViewIfNeeded();
  await expect(page.locator("#scene-root")).toContainText("WORKFLOWS");
  await expect(
    page
      .locator("#analyst-root")
      .getByText(/support tickets/)
      .first(),
  ).toBeVisible({ timeout: 10_000 });
  const marks = page.getByRole("figure", {
    name: "Every connector Foundry has, and the AI it works with",
  });
  await expect(marks.getByRole("listitem")).toHaveCount(15);
  await expect(marks).toContainText("PostgreSQL");
  // The field runs once the fonts are in.
  await expect(page.locator("html")).toHaveClass(/has-field/);
  expect(errors).toEqual([]);
});

test("copies the local setup", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto("/");
  // The page's behaviours start with the field.
  await expect(page.locator("html")).toHaveClass(/has-field/);
  // Named by its command too, since its label changes.
  const copy = page.getByRole("button", { name: /git clone/ });
  await expect(copy).toContainText("Copy local setup");
  await copy.click();
  await expect(copy).toContainText("Copied");
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(
    "git clone https://github.com/helixdb/helix-foundry.git\n" +
      "cd helix-foundry && docker compose up -d --wait",
  );
});

test("copies the prompt that sets Foundry up with your agent", async ({
  page,
  context,
}) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto("/");
  await expect(page.locator("html")).toHaveClass(/has-field/);
  // Its own button, by its place, since its label changes.
  const copy = page.locator('[data-copy="agent-prompt"]');
  await expect(copy).toHaveText("Set up with your agent");
  await copy.click();
  await expect(copy).toHaveText("Copied");
  const prompt = await page.evaluate(() => navigator.clipboard.readText());
  // The app's onboarding prompt, for someone without a workspace yet, on this
  // site and the stamp's Helix MCP.
  expect(prompt.startsWith("# Set up Helix Foundry with me\n")).toBe(true);
  expect(prompt).toContain(
    `Foundry is at ${APP_ORIGIN}. Every Foundry link takes`,
  );
  expect(prompt).toContain(
    "`claude mcp add --transport http helix https://mcp.dev.helix-db.com/mcp`",
  );
  expect(prompt).toContain("Then call `helix_list_workspaces`");
  expect(prompt).toContain("### 1. Create the ontology");
});

test("starts on Helix Cloud through sign-in, then opens the app", async ({
  page,
}) => {
  await page.goto("/");
  await page.getByRole("link", { name: "Start free" }).click();
  await expect(page).toHaveURL(/\/login$/);
  await expect(
    page.getByRole("heading", { name: "Sign in to Helix Foundry" }),
  ).toBeVisible();
  await signIn(page);
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts$/);

  // Signed in, / is the app, keeping the link's query.
  const home = await page.request.get("/?workspace=ws_acme&a=1&a=2", {
    maxRedirects: 0,
  });
  expect(home.status()).toBe(307);
  expect(home.headers().location).toBe("/ask?workspace=ws_acme&a=1&a=2");
  await page.goto("/");
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts$/);

  // Signed out, it is the landing page again.
  await page.getByRole("button", { name: "Account" }).click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/login$/);
  await page.goto("/");
  await expect(
    page.getByRole("heading", { level: 1, name: HEADING }),
  ).toBeVisible();
});
