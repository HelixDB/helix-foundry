import type { Page } from "@playwright/test";
import {
  createFirstOntology,
  expect,
  mock,
  signIn,
  signInToNewWorkspace,
  test,
} from "./support/mock";

// A generation the agent asks questions of (the mock's `ontologyQuestions`):
// it waits for answers with no draft, its page and an unfinished ontology's
// setup ask them, with the agent's defaults chosen to start, and answered
// (or with the defaults) it drafts on; cancelled, it ends. A new draft of
// an ontology starts from the goal its current version was drafted for.

// Acme's seeded ontology; its current version was drafted for "Customer
// billing health".
const O = "/ontology/ont_acme_contracts";
const questions = (page: Page) =>
  page.locator(".hf-panel").filter({
    has: page.getByRole("heading", {
      level: 3,
      name: "The agent has questions before it drafts",
    }),
  });
const question = (page: Page, text: RegExp) =>
  page.getByRole("radiogroup", { name: text });

test("carries the goal on, then answers the agent's questions and drafts on", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 300, ontologyQuestions: true });
  await signIn(page);
  await page.goto(O);
  await page.getByRole("link", { name: "Generate", exact: true }).click();
  await expect(page.getByLabel("Goal")).toHaveValue("Customer billing health");
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(page).toHaveURL(
    /\/ontology\/ont_acme_contracts\/generations\/gen_/,
  );
  const asked = questions(page);
  await expect(asked).toBeVisible({ timeout: 15_000 });
  await expect(
    page.locator(".hf-page-header-meta").getByText("Needs answers"),
  ).toBeVisible();
  await expect(asked).toContainText("If no one answers by");
  // The model's words are text, never markup.
  await expect(
    asked.getByText(
      "Should orders whose status is <b>cancelled</b> stay in the ontology?",
    ),
  ).toBeVisible();
  // The agent's default starts chosen, and labelled.
  const customer = question(page, /^Is a customer/);
  await expect(
    customer.getByRole("radio", { name: /The company that pays/ }),
  ).toBeChecked();
  await expect(customer).toContainText("Agent’s default");
  await customer
    .getByRole("radio", { name: "Each person who signs in" })
    .check();
  // A free answer's default is the agent's to give, unless written.
  const risk = question(page, /^How many days/);
  await expect(
    risk.getByRole("radio", { name: "The agent’s default: 90 days" }),
  ).toBeChecked();
  await risk.getByRole("radio", { name: "In my own words" }).check();
  const own = asked.getByLabel("Your answer");
  await expect(own).toBeVisible();
  // Nothing written: nothing to send.
  await expect(
    asked.getByRole("button", { name: "Send answers" }),
  ).toBeDisabled();
  await expect(asked).toContainText("Write an answer, or choose another.");
  await own.fill("60 days");
  await expect(
    question(page, /^Should orders/).getByRole("radio", {
      name: "Leave it to the agent",
    }),
  ).toBeChecked();
  const sent = page.waitForRequest(/generations\/gen_\w+:answer$/);
  await asked.getByRole("button", { name: "Send answers" }).click();
  expect((await sent).postDataJSON()).toEqual({
    answers: [
      { questionId: "q1", text: "Each person who signs in" },
      { questionId: "q2", text: "60 days" },
    ],
    useDefaults: true,
  });
  // Polled again until it stores its draft, saying what was answered.
  await expect(page.getByText("Succeeded", { exact: true })).toBeVisible({
    timeout: 15_000,
  });
  await expect(asked).toHaveCount(0);
  await expect(
    page.getByText("Answered the agent’s questions (done)"),
  ).toBeVisible();
  const answers = page.locator(".hf-panel").filter({
    has: page.getByRole("heading", { level: 3, name: "Your answers" }),
  });
  await expect(answers).toContainText("Answer: Each person who signs in");
  await expect(answers).toContainText("Answer: 60 days");
  await expect(answers).toContainText(
    "Answer: No preference: decide from the data and the goal.",
  );
  await expect(
    page.getByRole("link", { name: /^Review version/ }),
  ).toBeVisible();
});

test("takes the agent's defaults from an unfinished ontology's setup", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 300, ontologyQuestions: true });
  await signInToNewWorkspace(page);
  const overview = await createFirstOntology(page);
  await page.goto(overview);
  const setup = page.getByRole("list", { name: "Setup steps" });
  const asked = setup.locator(".on-asked");
  await expect(asked).toBeVisible({ timeout: 15_000 });
  await expect(
    setup.locator('[aria-current="step"]').getByText("Generate a draft"),
  ).toBeVisible();
  const sent = page.waitForRequest(/generations\/gen_\w+:answer$/);
  await asked.getByRole("button", { name: "Use the agent’s defaults" }).click();
  expect((await sent).postDataJSON()).toEqual({
    answers: [],
    useDefaults: true,
  });
  // It drafts on, and a first ontology's draft is made current and loaded.
  await expect(asked).toHaveCount(0, { timeout: 15_000 });
  await expect(
    setup.getByText("Generate a draft (done)", { exact: false }),
  ).toBeVisible({ timeout: 15_000 });
});

test("cancels a generation waiting for answers, and generates again", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 300, ontologyQuestions: true });
  await signIn(page);
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  const asked = questions(page);
  await expect(asked).toBeVisible({ timeout: 15_000 });
  // While it waits, it is the ontology's live generation.
  const generation = page.url();
  await page.goto(`${O}/generate`);
  await expect(
    page.getByText(
      "A generation is waiting for answers to the agent’s questions.",
    ),
  ).toBeVisible();
  await page.getByRole("link", { name: "Answer them" }).click();
  expect(page.url()).toBe(generation);
  await asked.getByRole("button", { name: "Cancel generation" }).click();
  const dialog = page.getByRole("dialog", { name: "Cancel this generation?" });
  const cancelled = page.waitForRequest(/generations\/gen_\w+:cancel$/);
  await dialog.getByRole("button", { name: "Cancel generation" }).click();
  await cancelled;
  await expect(dialog).toHaveCount(0);
  await expect(asked).toHaveCount(0);
  await expect(
    page.locator(".hf-page-header-meta").getByText("Cancelled"),
  ).toBeVisible();
  await expect(
    page.getByText(
      "Nothing was drafted. Generate again whenever you’re ready.",
    ),
  ).toBeVisible();
  await page.getByRole("link", { name: "Generate again" }).click();
  await expect(page.getByLabel("Goal")).toHaveValue("Customer billing health");
  await expect(page.getByText(/A generation is/)).toHaveCount(0);
});

test("shows members the questions without the form", async ({ page }) => {
  await mock.configure({ ontologyStepMs: 300, ontologyQuestions: true });
  await signIn(page);
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(questions(page)).toBeVisible({ timeout: 15_000 });
  const generation = new URL(page.url()).pathname;
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto(generation);
  const asked = questions(page);
  await expect(asked).toContainText(
    "An owner or admin of this workspace answers them.",
  );
  await expect(asked.getByRole("radio")).toHaveCount(0);
  await expect(asked.getByRole("button")).toHaveCount(0);
  await expect(asked).toContainText("Agent’s default");
});

test("starts Generate from a link's goal, even an empty one", async ({
  page,
}) => {
  await signIn(page);
  await page.goto(`${O}/generate?goal=Track%20renewals`);
  await expect(page.getByLabel("Goal")).toHaveValue("Track renewals");
  // Try again on a generation without a goal links an empty one: never
  // the current version's.
  await page.goto(`${O}/generate?goal=`);
  await expect(page.getByLabel("Goal")).toBeEnabled();
  await expect(page.getByLabel("Goal")).toHaveValue("");
  // Without the current version, there is no goal to carry on: it starts
  // empty, and Generate still goes.
  await page.route(
    "**/papi/v1/workspaces/*/foundry/ontologies/ont_acme_contracts/versions/*",
    (route) =>
      route.request().method() === "GET"
        ? route.fulfill({
            status: 500,
            json: { code: 13, message: "the version couldn't be read" },
          })
        : route.fallback(),
  );
  await page.goto(`${O}/generate`);
  await expect(page.getByLabel("Goal")).toBeEnabled({ timeout: 15_000 });
  await expect(page.getByLabel("Goal")).toHaveValue("");
});

test("says why a cancel failed, in its dialog", async ({ page }) => {
  await mock.configure({ ontologyStepMs: 300, ontologyQuestions: true });
  await signIn(page);
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  const asked = questions(page);
  await expect(asked).toBeVisible({ timeout: 15_000 });
  await page.route(
    "**/papi/v1/workspaces/*/foundry/ontologies/*/generations/*:cancel",
    (route) =>
      route.fulfill({
        status: 400,
        json: { code: 9, message: "this generation already ended" },
      }),
  );
  await asked.getByRole("button", { name: "Cancel generation" }).click();
  const dialog = page.getByRole("dialog", { name: "Cancel this generation?" });
  await dialog.getByRole("button", { name: "Cancel generation" }).click();
  await expect(dialog.getByRole("alert")).toHaveText(
    "This generation already ended.",
  );
  // The answers are still the page's to send.
  await expect(
    asked.getByRole("button", { name: "Use the agent’s defaults" }),
  ).toBeEnabled();
});

test("asks the questions on a set-up ontology's overview", async ({ page }) => {
  await mock.configure({ ontologyStepMs: 300, ontologyQuestions: true });
  await signIn(page);
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(questions(page)).toBeVisible({ timeout: 15_000 });
  await page.goto(O);
  await expect(questions(page)).toBeVisible();
  await expect(
    questions(page).getByRole("button", { name: "Send answers" }),
  ).toBeEnabled();
});
