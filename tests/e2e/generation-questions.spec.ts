import type { Locator, Page } from "@playwright/test";
import {
  createFirstOntology,
  expect,
  mock,
  signIn,
  signInToNewWorkspace,
  test,
} from "./support/mock";

// A generation the agent asks questions of (the mock's `ontologyQuestions`:
// a choice of one with a recommended option, a choice of several, an
// answer in words it suggests, and one it may decide itself): it waits for
// answers with no draft, and its page and an unfinished ontology's setup
// ask them one at a time, with a strip of the questions and Review; sent,
// it drafts on; cancelled, it ends. A new draft of an ontology starts from
// the goal its current version was drafted for.

// Acme's seeded ontology; its current version was drafted for "Customer
// billing health".
const O = "/ontology/ont_acme_contracts";
const questions = (page: Page) =>
  page.locator(".hf-panel").filter({
    has: page.getByRole("heading", {
      level: 3,
      name: /The agent has 4 questions before it drafts/,
    }),
  });
const strip = (asked: Locator) =>
  asked.getByRole("navigation", { name: "Questions" });
const customers = (asked: Locator) =>
  asked.getByRole("radiogroup", { name: /^Is a customer/ });
const signals = (asked: Locator) =>
  asked.getByRole("group", {
    name: "Which signals should mark a renewal as at risk?",
  });

async function generate(page: Page) {
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(questions(page)).toBeVisible({ timeout: 15_000 });
  return questions(page);
}

test("carries the goal on, then answers the agent's questions one by one and drafts on", async ({
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
  // The strip names each question, then Review, which waits for answers.
  const steps = strip(asked).getByRole("button");
  await expect(steps).toHaveText([
    /Customers/,
    /Risk signals/,
    /At-risk window/,
    /Cancelled orders/,
    /Review/,
  ]);
  await expect(steps.last()).toBeDisabled();
  await expect(steps.first()).toHaveAttribute("aria-current", "step");

  // One question at a time, its recommended option first and marked.
  await expect(
    asked.getByText("Question 1 of 4", { exact: true }),
  ).toBeVisible();
  const radios = customers(asked).getByRole("radio");
  await expect(radios).toHaveCount(3);
  await expect(radios.first()).toContainText("The company that pays");
  await expect(radios.first()).toContainText("Recommended");
  await expect(radios.last()).toContainText("Something else");
  // A choice of one chooses and goes on.
  await customers(asked)
    .getByRole("radio", { name: /^Each person who signs in/ })
    .click();
  await expect(
    asked.getByText("Question 2 of 4", { exact: true }),
  ).toBeVisible();
  await expect(steps.first()).toContainText("Each person who signs in");

  // A choice of several: two ticked, and the person's own words beside them.
  const next = asked.getByRole("button", { name: "Next", exact: true });
  await expect(next).toBeDisabled();
  await signals(asked)
    .getByRole("checkbox", { name: /^Unpaid invoices/ })
    .click();
  await signals(asked)
    .getByRole("checkbox", { name: /^Falling usage/ })
    .click();
  await expect(
    signals(asked).getByRole("checkbox", { checked: true }),
  ).toHaveCount(2);
  await signals(asked)
    .getByRole("checkbox", { name: /^Something else/ })
    .click();
  const own = asked.getByRole("textbox", { name: "Your own words" });
  await expect(own).toBeFocused();
  await own.fill("and churned seats");
  await next.click();

  // An answer in words, which the agent suggests.
  await expect(
    asked.getByText("Question 3 of 4", { exact: true }),
  ).toBeVisible();
  const days = asked.getByRole("textbox", {
    name: "How many days before a contract ends does its renewal count as at risk?",
  });
  await expect(days).toHaveAttribute(
    "placeholder",
    "The agent suggests: 90 days",
  );
  await asked.getByRole("button", { name: "Use “90 days”" }).click();
  await expect(days).toHaveValue("90 days");
  await days.fill("60 days");
  await next.click();

  // The one it may decide: skipped, or left with the rest.
  await expect(
    asked.getByText("Question 4 of 4", { exact: true }),
  ).toBeVisible();
  // The model's words are text, never markup.
  await expect(
    asked.getByText(
      "Should orders whose status is <b>cancelled</b> stay in the ontology?",
    ),
  ).toBeVisible();
  await expect(
    asked.getByRole("button", { name: "Let the agent decide the rest" }),
  ).toBeVisible();

  // Back to change an answer: kept, and changed.
  await steps.first().click();
  await expect(
    customers(asked).getByRole("radio", { name: /^Each person who signs in/ }),
  ).toHaveAttribute("aria-checked", "true");
  await customers(asked)
    .getByRole("radio", { name: /^The company that pays/ })
    .click();
  await expect(
    asked.getByText("Question 2 of 4", { exact: true }),
  ).toBeVisible();
  await asked.getByRole("button", { name: "Back", exact: true }).click();
  await expect(
    asked.getByText("Question 1 of 4", { exact: true }),
  ).toBeVisible();
  await steps.nth(3).click();
  // Touched, it can still be left to the agent: Something else, while its
  // box is empty, closes again, and Leave to the agent clears a choice.
  const cancelled = asked.getByRole("radiogroup", { name: /^Should orders/ });
  const elsewise = cancelled.getByRole("radio", { name: /^Something else/ });
  const words = asked.getByRole("textbox", { name: "Your own words" });
  await elsewise.click();
  await expect(words).toBeVisible();
  // Its last question: Review waits for words or no answer.
  await expect(
    asked.locator(".qf-nav").getByRole("button", { name: "Review" }),
  ).toBeDisabled();
  await elsewise.click();
  await expect(words).toHaveCount(0);
  await expect(
    asked.getByRole("button", { name: "Skip", exact: true }),
  ).toBeEnabled();
  await elsewise.click();
  await asked.getByRole("button", { name: "Leave to the agent" }).click();
  await expect(words).toHaveCount(0);
  await expect(cancelled.getByRole("radio", { checked: true })).toHaveCount(0);
  await expect(
    asked.getByRole("button", { name: "Leave to the agent" }),
  ).toHaveCount(0);
  await asked.getByRole("button", { name: "Skip", exact: true }).click();

  // Review lists every answer, the one left to the agent too.
  await expect(asked.getByText("Review your answers")).toBeVisible();
  const review = asked.locator(".qf-review-list");
  await expect(review).toContainText("The company that pays");
  await expect(review).toContainText(
    "Unpaid invoices, Falling usage, and “and churned seats”",
  );
  await expect(review).toContainText("“60 days”");
  await expect(review).toContainText("Left to the agent");
  const sent = page.waitForRequest(/generations\/gen_\w+:answer$/);
  await asked.getByRole("button", { name: "Send answers" }).click();
  expect((await sent).postDataJSON()).toEqual({
    answers: [
      { questionId: "Q1", options: ["The company that pays"] },
      {
        questionId: "Q2",
        options: ["Unpaid invoices", "Falling usage"],
        text: "and churned seats",
      },
      { questionId: "Q3", options: [], text: "60 days" },
    ],
    round: 1,
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
  const answers = page.getByRole("definition").filter({ hasText: "60 days" });
  await expect(answers).toHaveText("“60 days”");
  const summary = page.getByLabel("Your answers");
  await expect(summary).toContainText("Unpaid invoices, Falling usage");
  // The one left to the agent is its own choice, never shown as an answer.
  await expect(summary).toContainText("Left to the agent");
  await expect(summary).not.toContainText("No preference");
  await expect(
    page.getByRole("link", { name: /^Review version/ }),
  ).toBeVisible();
});

test("answers from the keyboard alone, letting the agent decide the rest, in an unfinished ontology's setup", async ({
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
  // Enter takes the recommended option, first, and goes on; the focus
  // follows.
  await customers(asked).getByRole("radio").first().focus();
  await page.keyboard.press("ArrowDown");
  await expect(
    customers(asked).getByRole("radio", { name: /^Each person/ }),
  ).toBeFocused();
  await page.keyboard.press("ArrowUp");
  await page.keyboard.press("Enter");
  await expect(
    asked.getByText("Question 2 of 4", { exact: true }),
  ).toBeVisible();
  const unpaid = signals(asked).getByRole("checkbox", {
    name: /^Unpaid invoices/,
  });
  await expect(unpaid).toBeFocused();
  // Space ticks, a number ticks its row, Enter goes on.
  await page.keyboard.press(" ");
  await page.keyboard.press("3");
  await expect(
    signals(asked).getByRole("checkbox", { checked: true }),
  ).toHaveCount(2);
  await page.keyboard.press("Enter");
  // A free answer's box takes the focus; Enter goes on.
  await expect(asked.getByRole("textbox")).toBeFocused();
  await page.keyboard.type("30 days");
  await page.keyboard.press("Enter");
  await expect(
    asked.getByText("Question 4 of 4", { exact: true }),
  ).toBeVisible();
  const rest = asked.getByRole("button", {
    name: "Let the agent decide the rest",
  });
  await rest.focus();
  await page.keyboard.press("Enter");
  await expect(asked.getByText("Review your answers")).toBeFocused();
  const sent = page.waitForRequest(/generations\/gen_\w+:answer$/);
  await asked.getByRole("button", { name: "Send answers" }).focus();
  await page.keyboard.press("Enter");
  expect((await sent).postDataJSON()).toEqual({
    answers: [
      { questionId: "Q1", options: ["The company that pays"] },
      {
        questionId: "Q2",
        options: ["Unpaid invoices", "An open support ticket"],
      },
      { questionId: "Q3", options: [], text: "30 days" },
    ],
    round: 1,
    useDefaults: true,
  });
  // It drafts on, and a first ontology's draft is made current and loaded.
  await expect(asked).toHaveCount(0, { timeout: 15_000 });
  await expect(
    setup.getByText("Generate a draft (done)", { exact: false }),
  ).toBeVisible({ timeout: 15_000 });
});

test("keeps a half-answered round through a reload, in this tab", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 300, ontologyQuestions: true });
  await signIn(page);
  const asked = await generate(page);
  await customers(asked)
    .getByRole("radio", { name: /^Each person who signs in/ })
    .click();
  await signals(asked)
    .getByRole("checkbox", { name: /^Falling usage/ })
    .click();
  await page.reload();
  await expect(asked.getByText("Question 2 of 4", { exact: true })).toBeVisible(
    {
      timeout: 15_000,
    },
  );
  await expect(strip(asked).getByRole("button").first()).toContainText(
    "Each person who signs in",
  );
  await expect(
    signals(asked).getByRole("checkbox", { name: /^Falling usage/ }),
  ).toHaveAttribute("aria-checked", "true");
});

test("cancels a generation waiting for answers, and generates again", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 300, ontologyQuestions: true });
  await signIn(page);
  const asked = await generate(page);
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

test("shows members the questions to read, without the flow", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 300, ontologyQuestions: true });
  await signIn(page);
  await generate(page);
  const generation = new URL(page.url()).pathname;
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto(generation);
  const asked = questions(page);
  await expect(asked).toContainText(
    "An owner or admin of this workspace answers them.",
  );
  await expect(asked.getByRole("radio")).toHaveCount(0);
  await expect(asked.getByRole("checkbox")).toHaveCount(0);
  await expect(asked.getByRole("button")).toHaveCount(0);
  await expect(asked.getByRole("textbox")).toHaveCount(0);
  await expect(asked).toContainText("Recommended");
  await expect(asked).toContainText("Each person who signs in");
  await expect(asked).toContainText("The agent suggests: 90 days");
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
  const asked = await generate(page);
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
  // The questions are still the page's to answer.
  await dialog.getByRole("button", { name: "Keep it" }).click();
  await expect(customers(asked).getByRole("radio").first()).toBeVisible();
});

test("says why answers were refused, on Review", async ({ page }) => {
  await mock.configure({ ontologyStepMs: 300, ontologyQuestions: true });
  await signIn(page);
  const asked = await generate(page);
  await page.route(
    "**/papi/v1/workspaces/*/foundry/ontologies/*/generations/*:answer",
    (route) =>
      route.fulfill({
        status: 400,
        json: {
          code: 9,
          message:
            "these answers are for round 1 of the agent's questions, and it waits on round 2: read its questions again",
        },
      }),
  );
  await customers(asked).getByRole("radio").first().click();
  await signals(asked).getByRole("checkbox").first().click();
  await asked.getByRole("button", { name: "Next", exact: true }).click();
  await asked.getByRole("textbox").fill("60 days");
  await asked.getByRole("button", { name: "Next", exact: true }).click();
  await asked
    .getByRole("button", { name: "Let the agent decide the rest" })
    .click();
  await asked.getByRole("button", { name: "Send answers" }).click();
  await expect(asked.getByRole("alert")).toContainText(
    "These answers are for round 1",
  );
  // Still on Review, its answers kept.
  await expect(asked.getByText("Review your answers")).toBeVisible();
});

test("asks the questions on a set-up ontology's overview", async ({ page }) => {
  await mock.configure({ ontologyStepMs: 300, ontologyQuestions: true });
  await signIn(page);
  await generate(page);
  await page.goto(O);
  await expect(questions(page)).toBeVisible();
  await expect(customers(questions(page)).getByRole("radio")).toHaveCount(3);
});

test.describe("on a phone", () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test("asks one question per screen, the strip its marks", async ({
    page,
  }) => {
    await mock.configure({ ontologyStepMs: 300, ontologyQuestions: true });
    await signIn(page);
    const asked = await generate(page);
    await expect(asked.locator(".qf-step")).toHaveCount(1);
    // Only the current step is named in the strip; each mark is still
    // named, with its answer, for a screen reader and in its title.
    await expect(strip(asked).getByText("Customers")).toBeVisible();
    await expect(strip(asked).getByText("Risk signals")).toBeHidden();
    const signalsStep = strip(asked).getByRole("button", {
      name: "Risk signals: Needs an answer",
    });
    await expect(signalsStep).toBeVisible();
    await expect(signalsStep).toHaveAttribute(
      "title",
      "Risk signals: Needs an answer",
    );
    await expect(
      strip(asked).getByRole("button", { name: "Review" }),
    ).toBeVisible();
    await customers(asked)
      .getByRole("radio", { name: /^The company that pays/ })
      .click();
    await expect(
      asked.getByText("Question 2 of 4", { exact: true }),
    ).toBeVisible();
    await expect(strip(asked).getByText("Risk signals")).toBeVisible();
    await expect(
      strip(asked).getByRole("button", {
        name: "Customers: The company that pays",
      }),
    ).toBeVisible();
    // Nothing scrolls sideways.
    expect(
      await page.evaluate(
        () =>
          document.documentElement.scrollWidth <=
          document.documentElement.clientWidth,
      ),
    ).toBe(true);
  });
});

test.describe("an option that is a time zone id", () => {
  test.use({ timezoneId: "America/New_York" });

  test("shows its city and offset, and sends the id back", async ({ page }) => {
    await mock.configure({
      ontologyStepMs: 300,
      ontologyQuestions: "time_zone",
    });
    await signIn(page);
    await page.goto(`${O}/generate`);
    await page.getByRole("button", { name: "Generate", exact: true }).click();
    const zones = page.getByRole("radiogroup", {
      name: /^Which time zone are the dates/,
    });
    await expect(zones).toBeVisible({ timeout: 15_000 });
    const radios = zones.getByRole("radio");
    // The recommended first, each in words; never an id with underscores.
    // Its name, without the aria-hidden shortcut digit.
    await expect(radios.first()).toHaveAccessibleName(/^New York · GMT−[45]/);
    await expect(radios.first()).toContainText("Recommended");
    await expect(zones).toContainText(/London · GMT(\+1)?/);
    await expect(zones).toContainText(/Los Angeles · GMT−[78]/);
    await expect(zones).not.toContainText("_");
    await expect(zones).not.toContainText("America/");
    await zones.getByRole("radio", { name: /^London/ }).click();
    const sent = page.waitForRequest(/generations\/gen_\w+:answer$/);
    await page.getByRole("button", { name: "Send answers" }).click();
    expect((await sent).postDataJSON().answers).toEqual([
      { questionId: "Q1", options: ["Europe/London"] },
    ]);
  });
});
