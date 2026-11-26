import type { Request } from "@playwright/test";
import {
  chooseDatabase,
  createdOntology,
  expect,
  file,
  finishCompany,
  finishOntology,
  mock,
  openAdvanced,
  signIn,
  signInToNewWorkspace,
  test,
  uploadCard,
} from "./support/mock";

// Onboarding on the user's own data: Get started's two cards (Set up with
// your agent, Set up here) and the sample company's link under them; the one
// new-ontology wizard's Your company step (the domain from the user's email,
// Research, its questions with guesses and chips, what it says when the site
// can't be read or the research fails, Skip); the goal drafted from the
// answers and the chosen data; the review's derived name and Advanced (name
// and database); a first draft that is always made current and loaded; and
// the 50-table cap said with its numbers next to Continue and Generate.

const RESEARCH = /\/foundry\/company-research$/,
  DRAFT = /\/foundry\/company-research\/[^/]+:draft-goal$/;
const isPost = (pattern: RegExp) => (r: Request) =>
  r.method() === "POST" && pattern.test(new URL(r.url()).pathname);
const researched = async () =>
  (await mock.stats()).requests.filter((r) =>
    /^POST \/v1\/workspaces\/\w+\/foundry\/company-research \d+$/.test(r),
  );

test("offers two ways to set up, side by side, with the sample a link under them", async ({
  page,
}) => {
  await signInToNewWorkspace(page);
  const agent = page.locator(".on-start-card").filter({
      hasText: "Set up with your agent",
    }),
    here = page.locator(".on-start-card").filter({ hasText: "Set up here" });
  await expect(
    agent.getByRole("button", { name: "Copy prompt" }),
  ).toBeVisible();
  await expect(here.getByRole("button", { name: "Set up here" })).toBeVisible();
  // No toggle between them, and the sample is no card of its own.
  await expect(page.getByRole("radiogroup")).toHaveCount(0);
  await expect(
    page.getByRole("link", { name: "Or look around a sample company first" }),
  ).toHaveAttribute("href", "/ontology/sample");
  // Two equal cards side by side; one column on a phone.
  const boxes = async () =>
    Promise.all([agent.boundingBox(), here.boundingBox()]);
  let [a, b] = await boxes();
  expect(Math.round(a!.y)).toBe(Math.round(b!.y));
  expect(Math.abs(a!.width - b!.width)).toBeLessThan(2);
  await page.setViewportSize({ width: 375, height: 800 });
  [a, b] = await boxes();
  expect(b!.y).toBeGreaterThan(a!.y + a!.height - 1);
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(375);
});

test("researches the company from the user's email, drafts the goal from the answers, and creates the ontology by the name it derives", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 300 });
  await signInToNewWorkspace(page, "Globex", "grace@globex-industries.com");
  await page.getByRole("button", { name: "Set up here" }).click();
  const steps = page.getByRole("list", { name: "Steps" });
  await expect(steps.locator('[aria-current="step"]')).toContainText(
    "Your company",
  );
  // The domain comes from the email.
  const domain = page.getByLabel("Company website");
  await expect(domain).toHaveValue("globex-industries.com");
  const asked = page.waitForRequest(isPost(RESEARCH));
  await page.getByRole("button", { name: "Research" }).click();
  await expect(
    page.getByRole("status").filter({ hasText: "Reading" }),
  ).toHaveText("Reading globex-industries.com…");
  expect((await asked).postDataJSON()).toEqual({
    domain: "globex-industries.com",
  });
  // What it found, on a screen of its own: the company, the pages it read
  // and what it does, asking whether that looks right.
  const count = page.locator(".on-focus-count");
  await expect(count).toHaveText("1 of 5");
  await expect(
    page.getByRole("heading", { level: 2, name: "Globex Industries" }),
  ).toBeVisible();
  // Not the pages it read: what it found is the point.
  await expect(
    page.getByRole("link", { name: "globex-industries.com/about" }),
  ).toHaveCount(0);
  const guess = page.locator(".on-guess");
  await expect(guess).toHaveText(
    /^Globex Industries sells software that helps teams run their operations\./,
  );
  await expect(page.getByText("Does this look right to you?")).toBeVisible();
  const right = page.getByRole("button", { name: "This looks right to me" });
  await expect(right).toBeFocused();
  // Change it opens what it does to correct.
  await page.getByRole("button", { name: "Change it" }).click();
  const does = page.getByRole("textbox", {
    name: "What Globex Industries does",
  });
  await expect(does).toBeFocused();
  // Undo puts Foundry's back, on its question.
  await does.fill("Globex Industries makes nothing.");
  await page.getByRole("button", { name: "Undo", exact: true }).click();
  await expect(guess).toHaveText(
    /^Globex Industries sells software that helps teams run their operations\./,
  );
  await expect(right).toBeFocused();
  await expect(
    page.getByRole("button", { name: "Undo", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "Change it" }).click();
  await does.fill("Globex Industries makes sensors and sells their data.");
  await page.getByRole("button", { name: "Next" }).click();

  // Then each question on its own screen, its best guess to confirm.
  await expect(count).toHaveText("2 of 5");
  await expect(
    page.getByRole("heading", {
      level: 2,
      name: "Who will ask this ontology questions, and what for?",
    }),
  ).toBeVisible();
  await expect(guess).toHaveText(
    "Globex Industries’s operations and finance teams, to see how customers, orders and revenue connect.",
  );
  await page.getByRole("button", { name: "Change it" }).click();
  const who = page.getByRole("textbox", {
    name: "Who will ask this ontology questions, and what for?",
  });
  await expect(who).toBeFocused();
  await who.fill("");
  await page.getByRole("button", { name: "Next" }).click();
  await expect(count).toHaveText("3 of 5");
  await expect(guess).toHaveText(
    "Which customers are growing or at risk, and why.",
  );
  await page.getByRole("button", { name: "Change it" }).click();
  const most = page.getByRole("textbox", {
    name: "Which questions matter most?",
  });
  // A chip fills its answer; any answer can be typed.
  const chips = page.getByRole("group", {
    name: "Answers to “Which questions matter most?”",
  });
  await chips.getByRole("button", { name: "Churn risk" }).click();
  await expect(most).toHaveValue("Churn risk");
  await expect(
    chips.getByRole("button", { name: "Churn risk" }),
  ).toHaveAttribute("aria-pressed", "true");
  // Back keeps the answers, and a cleared one opens on its field, which
  // stays open as it is typed in.
  await page.getByRole("button", { name: "Back", exact: true }).click();
  await expect(count).toHaveText("2 of 5");
  await expect(who).toHaveValue("");
  await expect(who).toBeFocused();
  await who.pressSequentially("Ops");
  await expect(who).toBeFocused();
  await expect(who).toHaveValue("Ops");
  await who.fill("");
  await page.getByRole("button", { name: "Next" }).click();
  await expect(guess).toHaveText("Churn risk");
  await right.click();
  await expect(count).toHaveText("4 of 5");
  await page.getByRole("button", { name: "Change it" }).click();
  await page
    .getByRole("textbox", {
      name: "When two sources describe a customer, which one is right?",
    })
    .fill("Stripe for billing, the CRM for people");
  await page.getByRole("button", { name: "Next" }).click();
  await expect(count).toHaveText("5 of 5");
  await expect(
    page.getByText("Foundry writes your goal from these next."),
  ).toBeVisible();
  // The last one's This looks right to me drafts the goal at once, from
  // what it does as corrected and the answers left, and moves on to the
  // data while it is written.
  const drafting = page.waitForRequest(isPost(DRAFT));
  await right.click();
  await expect(page).toHaveURL(/\/ontology\/new\?step=sources$/);
  const body = (await drafting).postDataJSON();
  expect(body.summary).toBe(
    "Globex Industries makes sensors and sells their data.",
  );
  expect(body.answers).toEqual([
    { questionId: "q2", answer: "Churn risk" },
    { questionId: "q3", answer: "Stripe for billing, the CRM for people" },
    { questionId: "q4", answer: "Paid an invoice in the last 90 days." },
  ]);
  await expect(steps.getByRole("listitem").first()).toContainText(
    "Globex Industries: 3 answers",
  );
  // Only the current step says what it asks.
  await expect(
    page.getByRole("heading", {
      level: 2,
      name: "Connect your data or upload documents",
    }),
  ).toBeVisible();
  await expect(page.getByRole("main")).not.toContainText(
    "Describe the questions you want to answer.",
  );

  // Sources: one uploaded document.
  const chooser = page.waitForEvent("filechooser");
  await page.getByRole("button", { name: "Upload documents" }).click();
  await (
    await chooser
  ).setFiles([file("notes.md", "# Notes\n\nRenewals slip.\n")]);
  await expect(uploadCard(page, "notes.md")).toContainText("Uploaded");
  await page.getByRole("button", { name: "Continue" }).click();

  // The goal is waiting, written from the answers.
  const goal = page.getByLabel("Goal");
  await expect(goal).toHaveValue(
    /^Globex Industries uses this ontology to answer its team’s questions/,
  );
  await expect(goal).toHaveValue(/Which questions matter most\? Churn risk/);
  await expect(goal).toHaveValue(/makes sensors and sells their data/);
  await expect(
    page.getByText("Foundry wrote the goal from your answers."),
  ).toBeVisible();
  // One draft: the goal step doesn't write another.
  expect(
    (await mock.stats()).requests.filter((r) => r.includes(":draft-goal")),
  ).toHaveLength(1);
  const drafted = await goal.inputValue();

  // Write it again asks before replacing an edited goal.
  await goal.fill("Renewals at risk, by customer.");
  await page.getByRole("button", { name: "Write it again" }).click();
  const confirm = page.getByRole("dialog", { name: "Write the goal again?" });
  await confirm.getByRole("button", { name: "Cancel" }).click();
  await expect(goal).toHaveValue("Renewals at risk, by customer.");
  const again = page.waitForRequest(isPost(DRAFT));
  await page.getByRole("button", { name: "Write it again" }).click();
  await confirm.getByRole("button", { name: "Write it again" }).click();
  // Written again, it reads the data chosen since: the uploaded notes.
  expect((await again).postDataJSON().data).toContain("General");
  await expect(goal).not.toHaveValue("Renewals at risk, by customer.");
  await expect(goal).toHaveValue(
    /^Globex Industries uses this ontology to answer its team’s questions/,
  );
  const redrafted = await goal.inputValue();
  expect(redrafted).not.toBe(drafted);
  await page.getByRole("button", { name: "Continue" }).click();

  // The review: the name the draft came with, the database a new one named
  // as it, both in Advanced.
  await expect(page.getByRole("main")).toContainText(
    "Foundry creates Globex Industries operations on a new Helix database",
  );
  await openAdvanced(page);
  await expect(page.getByLabel("Ontology name")).toHaveValue("");
  await expect(page.getByLabel("Ontology name")).toHaveAttribute(
    "placeholder",
    "Globex Industries operations",
  );
  await expect(page.getByLabel("Database name")).toHaveValue(
    "Globex Industries operations",
  );
  const created = page.waitForRequest(isPost(/\/foundry\/ontologies$/)),
    generated = page.waitForRequest(isPost(/:generate$/));
  await finishOntology(page);
  expect((await created).postDataJSON()).toMatchObject({
    name: "Globex Industries operations",
  });
  // Every new ontology's first draft is made current and loaded.
  expect((await generated).postDataJSON()).toMatchObject({
    noConnectors: true,
    goal: redrafted,
    loadWhenReady: true,
  });
  await page.goto(await createdOntology(page));
  await expect(
    page.getByRole("heading", {
      level: 1,
      name: "Globex Industries operations",
    }),
  ).toBeVisible();
});

test("names the ontology and picks an existing database in Advanced", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/ontology/new");
  // Not a personal mail domain: the email's is offered.
  await expect(page.getByLabel("Company website")).toHaveValue("example.com");
  await page.getByLabel("Company website").fill("https://www.Acme.com/about");
  await page.getByRole("button", { name: "Research" }).click();
  await expect(page.locator(".on-guess")).toContainText("Acme");
  // Back from what it found returns to the website, the research kept;
  // editing the domain drops it.
  await page.getByRole("button", { name: "Back", exact: true }).click();
  await expect(page.getByRole("button", { name: "Continue" })).toBeVisible();
  await page.getByLabel("Company website").fill("acme.co");
  await expect(page.getByRole("button", { name: "Continue" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Skip" })).toBeVisible();
  await page.getByLabel("Company website").fill("acme.com");
  await page.getByRole("button", { name: "Research" }).click();
  // Confirmed as found, what the company does goes as the research's own.
  const drafting = page.waitForRequest(isPost(DRAFT));
  await finishCompany(page);
  expect((await drafting).postDataJSON()).not.toHaveProperty("summary");
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(page.getByLabel("Goal")).toHaveValue(/^Acme uses this ontology/);
  await page.getByRole("button", { name: "Continue" }).click();
  await openAdvanced(page);
  await expect(page.getByLabel("Ontology name")).toHaveAttribute(
    "placeholder",
    "Acme operations",
  );
  await page.getByLabel("Ontology name").fill("Acme renewals");
  await expect(page.getByRole("main")).toContainText(
    "Foundry creates Acme renewals on a new Helix database",
  );
  await chooseDatabase(page, "orders in Production");
  await expect(page.getByRole("main")).toContainText(
    "Foundry creates Acme renewals on the database you chose",
  );
  const created = page.waitForRequest(isPost(/\/foundry\/ontologies$/)),
    generated = page.waitForRequest(isPost(/:generate$/));
  await finishOntology(page);
  expect((await created).postDataJSON()).toEqual({
    name: "Acme renewals",
    tenantId: "ten_acme_orders",
    clientToken: expect.any(String),
  });
  expect((await generated).postDataJSON()).toMatchObject({
    connectorIds: ["conn_acme_stripe", "conn_acme_shop"],
    loadWhenReady: true,
  });
  await page.goto(await createdOntology(page));
  await expect(
    page.getByRole("heading", { level: 1, name: "Acme renewals" }),
  ).toBeVisible();
  // No new database or project was made.
  expect(
    (await mock.stats()).requests.filter((r) =>
      /^POST \/v1\/(tenants|projects)/.test(r),
    ),
  ).toEqual([]);
});

test("offers no domain for a personal email, refuses one, and skips the company", async ({
  page,
}) => {
  await signInToNewWorkspace(page, "Grace", "grace@gmail.com");
  await page.getByRole("button", { name: "Set up here" }).click();
  const domain = page.getByLabel("Company website"),
    research = page.getByRole("button", { name: "Research" });
  await expect(domain).toHaveValue("");
  await expect(research).toBeDisabled();
  await domain.fill("gmail.com");
  await research.click();
  await expect(page.locator(".on-company-error")).toContainText(
    "Gmail.com is a personal mail provider: enter your company’s domain.",
  );
  // Skip goes on without it: no research, so no goal is drafted.
  await page.getByRole("button", { name: "Skip" }).click();
  await expect(page).toHaveURL(/\/ontology\/new\?step=sources$/);
  const steps = page.getByRole("list", { name: "Steps" });
  await expect(steps.getByRole("listitem").first()).toContainText("Skipped");
  const chooser = page.waitForEvent("filechooser");
  await page.getByRole("button", { name: "Upload documents" }).click();
  await (await chooser).setFiles([file("notes.md", "# Notes\n")]);
  await expect(uploadCard(page, "notes.md")).toContainText("Uploaded");
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(page.getByLabel("Goal")).toHaveValue("");
  await expect(
    page.getByRole("button", { name: "Write it again" }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "Skip" }).click();
  // Named from what it drafts from.
  await openAdvanced(page);
  await expect(page.getByLabel("Ontology name")).toHaveAttribute(
    "placeholder",
    "General",
  );
  expect(
    (await mock.stats()).requests.filter((r) => r.includes(":draft-goal")),
  ).toEqual([]);
});

test("says when the site can't be read, and when the research fails, with Retry", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/ontology/new");
  const domain = page.getByLabel("Company website"),
    research = page.getByRole("button", { name: "Research" });
  // While it reads, it says what it does, a step at a time: the done ones
  // ticked, the latest announced.
  await mock.configure({ companyResearchMs: 2500 });
  // Unreadable: guesses from the domain alone, and no pages.
  await domain.fill("unreadable-co.com");
  await research.click();
  const steps = page.locator(".on-research li");
  await expect(steps).toHaveCount(2);
  await expect(steps.nth(0)).toHaveText("Reading unreadable-co.com");
  await expect(steps.nth(0)).toHaveClass("is-done");
  await expect(steps.nth(1)).toHaveText(
    "Following its pages about the company…",
  );
  await expect(
    page.getByRole("status").filter({ hasText: "Following its pages" }),
  ).toHaveText("Following its pages about the company…");
  await expect(
    page.getByText(
      "Foundry couldn’t read the site, so these guesses come from the domain alone.",
    ),
  ).toBeVisible();
  await expect(page.locator(".on-company-sources")).toHaveCount(0);
  await expect(page.locator(".on-focus-count")).toBeVisible();

  // The model failing: why, with Retry, which asks again; Skip still works.
  await page.getByRole("button", { name: "Back", exact: true }).click();
  await domain.fill("fail-co.com");
  await research.click();
  const failed = page.locator(".on-company-error");
  await expect(failed).toContainText(
    "The model couldn’t finish the research; try again.",
  );
  await expect(page.locator(".on-focus-count")).toHaveCount(0);
  await failed.getByRole("button", { name: "Retry" }).click();
  await expect.poll(async () => (await researched()).length).toBe(3);
  await expect(failed).toBeVisible();
  // This month's allowance spent, and the hourly cap, in the WFE's words.
  await mock.configure({ companyResearchOutcome: "budget" });
  await domain.fill("globex.com");
  await research.click();
  await expect(failed).toContainText(
    "This workspace has spent this month’s allowance for company research.",
  );
  await mock.configure({ companyResearchOutcome: "rate_limit" });
  await failed.getByRole("button", { name: "Retry" }).click();
  await expect(failed).toContainText(
    "Too many company researches this hour; try again later.",
  );
  await page.getByRole("button", { name: "Skip" }).click();
  await expect(page).toHaveURL(/\/ontology\/new\?step=sources$/);
});

test("leaves the goal to the user when drafting it fails", async ({ page }) => {
  await signIn(page);
  await page.goto("/ontology/new");
  await page.getByLabel("Company website").fill("acme.com");
  await page.getByRole("button", { name: "Research" }).click();
  await expect(page.locator(".on-focus-count")).toBeVisible();
  await mock.configure({ companyResearchOutcome: "fail" });
  await finishCompany(page);
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(page.locator(".on-goal-draft [role=alert]")).toHaveText(
    "The model couldn’t finish the research; try again.",
  );
  const goal = page.getByLabel("Goal");
  await expect(goal).toHaveValue("");
  await goal.fill("Which customers renew late?");
  await page.getByRole("button", { name: "Continue" }).click();
  // The research's name still names it.
  await openAdvanced(page);
  await expect(page.getByLabel("Ontology name")).toHaveAttribute(
    "placeholder",
    "Acme operations",
  );
});

test("says what the 50-table cap needs untick, beside Continue and Generate", async ({
  page,
}) => {
  // A synced warehouse of 60 tables beside Acme's Stripe and Shop: more
  // than one draft reads, every synced connector being the default.
  await page.route(
    /\/papi\/v1\/workspaces\/ws_acme\/foundry\/connectors$/,
    async (route) => {
      if (route.request().method() !== "GET") return route.continue();
      const res = await route.fetch(),
        json = await res.json();
      const shop = json.connectors.find(
        (c: { id: string }) => c.id === "conn_acme_shop",
      );
      json.connectors.push({
        ...shop,
        id: "conn_e2e_warehouse",
        name: "Warehouse",
        tables: Array.from({ length: 60 }, (_, i) => ({
          name: `public.t${i + 1}`,
          rowCount: "10",
        })),
      });
      await route.fulfill({ response: res, json });
    },
  );
  await signIn(page);
  await page.goto("/ontology/new?step=sources");
  const reason = page
      .getByRole("status")
      .filter({ hasText: "These sources hold" }),
    count = page.locator(".on-table-count"),
    onward = page.getByRole("button", { name: "Continue" });
  await expect(reason).toHaveText(
    /^These sources hold (\d+) tables and one draft reads at most 50, table files included\. Untick connectors or sets until it’s 50 or fewer\.$/,
  );
  const held = Number(/hold (\d+)/.exec((await reason.textContent())!)![1]);
  expect(held).toBeGreaterThan(60);
  await expect(count).toHaveText(`${held} of 50 tables`);
  await expect(count).toHaveClass(/is-over/);
  await expect(onward).toBeDisabled();
  // Shop and Warehouse are both PostgreSQL, under one choice.
  const connectors = page.getByRole("list", { name: "Connectors" });
  await connectors
    .getByRole("button", { name: "Show PostgreSQL connectors" })
    .click();
  await connectors.getByRole("checkbox", { name: /Warehouse/ }).uncheck();
  await expect(reason).toHaveCount(0);
  await expect(count).toHaveText(`${held - 60} of 50 tables`);
  await expect(count).not.toHaveClass(/is-over/);
  await expect(onward).toBeEnabled();

  // Generate says the same.
  await page.goto("/ontology/ont_acme_contracts/generate");
  await expect(reason).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Generate", exact: true }),
  ).toBeDisabled();
});
