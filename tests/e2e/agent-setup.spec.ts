import {
  createdOntology,
  expect,
  finishOntology,
  signIn,
  signInToNewWorkspace,
  startOntology,
  test,
} from "./support/mock";

// Set up with your agent copies a prompt for the user's own agent with the
// Helix MCP, from where the setup is.

test("copies the setup prompt where there is no ontology yet", async ({
  page,
  context,
}) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const clipboard = () => page.evaluate(() => navigator.clipboard.readText());
  // A new workspace opens on its onboarding wizard, which offers it.
  await signInToNewWorkspace(page);
  const setup = page
    .getByRole("status")
    .filter({ hasText: "Set up with your agent" });
  await setup.getByRole("button", { name: "Copy prompt" }).click();
  await expect(page.getByText("Copied the prompt")).toBeVisible();
  await expect.poll(clipboard).toContain("# Set up Helix Foundry with me");
  expect(await clipboard()).toContain("- No ontology yet.");
  expect(await clipboard()).toContain("### 1. Create the ontology");

  // /ontology leads to the wizard, and the prompt with it.
  await page.goto("/ontology");
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await page.evaluate(() => navigator.clipboard.writeText(""));
  await setup.getByRole("button", { name: "Copy prompt" }).click();
  await expect.poll(clipboard).toContain("- No ontology yet.");

  // A workspace with ontologies creates another without it.
  await context.clearCookies();
  await signIn(page);
  await page.goto("/ontology/new");
  await expect(
    page.getByRole("heading", { level: 1, name: "New ontology" }),
  ).toBeVisible();
  await expect(setup).toHaveCount(0);
});

test("copies the setup prompt from an unfinished ontology's step", async ({
  page,
  context,
}) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const clipboard = () => page.evaluate(() => navigator.clipboard.readText());
  await signIn(page);
  await startOntology(page, "Orders");
  await page.getByRole("radio", { name: "orders in Production" }).check();
  await finishOntology(page);
  const overview = await createdOntology(page);
  await page.goto(overview);
  // The setup's one card offers it in its head.
  const agent = page.getByRole("button", { name: "Set up with your agent" });
  await agent.click();
  await expect.poll(clipboard).toContain("Its setup is at “Generate a draft”");
  expect(await clipboard()).toContain("### 1. Generate a draft");
  expect(await clipboard()).toContain("**Stripe**");

  // Its title minimises the card to its head, which says the step next,
  // and that is remembered.
  const toggle = page.getByRole("button", { name: "Set it up" }),
    steps = page.getByRole("list", { name: "Setup steps" });
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "false");
  await expect(steps).toHaveCount(0);
  await expect(page.getByText("Next: Generate a draft")).toBeVisible();
  await expect(agent).toBeVisible();
  await page.reload();
  await expect(toggle).toHaveAttribute("aria-expanded", "false");
  await toggle.click();
  await expect(steps).toBeVisible();

  // A member sees the setup's steps, but not the prompt.
  await context.clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto(overview);
  await expect(page.getByRole("heading", { name: "Set it up" })).toBeVisible();
  await expect(agent).toHaveCount(0);
});
