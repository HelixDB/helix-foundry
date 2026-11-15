import type { Page } from "@playwright/test";
import { expect, mock, postToWfe, signIn, test } from "./support/mock";

// Automations: a person's steps Foundry runs as them, on a schedule or when
// a sync, a load or documents arrive. One starts from a template, its steps
// changed on a canvas or in an outline; nothing runs until its owner
// approves exactly what runs unattended; a run's page colours its steps and
// says what each found; three failures in a row pause it; the Assistant
// sets one up from a card its person runs; another member sees how far a
// run went, never what it found.

const A = "/v1/workspaces/ws_acme/foundry/automations";
const at = (y: number) => ({ x: 0, y });
const manual = {
  id: "start",
  kind: "trigger",
  config: { kind: "manual" },
  position: at(0),
};
const step = (id: string, kind: string, config: object, y: number) => ({
  id,
  kind,
  config,
  position: at(y),
});

/**
 * Makes an automation of Ada's through the WFE, its nodes in a chain (or
 * `edges`), and approves it.
 */
async function make(
  page: Page,
  name: string,
  nodes: object[],
  edges?: object[],
) {
  const ids = nodes.map((n) => (n as { id: string }).id);
  const made = (
    await postToWfe(page, A, {
      name,
      clientToken: `e2e-${name}`,
      specJson: JSON.stringify({
        v: 1,
        nodes,
        edges: edges ?? ids.slice(1).map((to, i) => ({ from: ids[i], to })),
      }),
    })
  ).automation;
  await postToWfe(page, `${A}/${made.id}:approve`, {
    expectedRevision: made.revision,
  });
  return made.id as string;
}

const main = (page: Page) => page.getByRole("main");
const ended = (page: Page, outcome: string) =>
  expect(
    main(page).locator(".hf-page-header").getByText(outcome, { exact: true }),
  ).toBeVisible({
    timeout: 20_000,
  });

test("starts from a template, runs only once approved, and runs now", async ({
  page,
}) => {
  await mock.configure({ automationStepMs: 100, agentStepMs: 60 });
  await signIn(page);
  await page.getByRole("link", { name: "Automations", exact: true }).click();
  await expect(page).toHaveURL(/\/automations$/);
  await page.getByRole("link", { name: /^Sync, then summarise/ }).click();
  // The template's steps, its connection the workspace's first, read as a
  // sentence and drawn on the canvas.
  await expect(page.locator(".au-sentence")).toContainText(
    "Every day at 08:00, sync “Stripe”, then ask the Assistant:",
  );
  await expect(
    page.getByRole("region", { name: "Steps on a canvas" }).locator(".au-node"),
  ).toHaveCount(4);
  await page.getByLabel("Name").fill("Morning Stripe summary");
  await page.getByRole("button", { name: "Review and save" }).click();
  const review = page.getByRole("dialog", {
    name: "Review what runs unattended",
  });
  const steps = review.getByRole("list", { name: "Steps that run unattended" });
  await expect(steps.getByRole("listitem")).toHaveCount(4);
  await expect(steps).toContainText("Sync “Stripe”");
  await expect(steps).toContainText("Runs without asking you, as you.");
  await expect(steps).toContainText("Changes nothing without asking you");
  await review.getByRole("button", { name: "Save and turn on" }).click();

  await expect(page).toHaveURL(/\/automations\/aut_[^/]+$/);
  await expect(main(page).getByText("On", { exact: true })).toBeVisible();
  await main(page).getByRole("button", { name: "Run now" }).click();
  await expect(page).toHaveURL(/\/automations\/aut_[^/]+\/runs\/arun_/);
  await ended(page, "Succeeded");
  const run = page.getByRole("list", { name: "What each step did" });
  await expect(run.getByRole("listitem")).toHaveCount(4);
  await expect(run.getByRole("listitem").nth(1)).toContainText(
    "Started a sync of its connector.",
  );
  await expect(run.getByRole("listitem").nth(2)).toContainText(
    "What the Assistant reported",
  );
  await expect(run.getByRole("listitem").nth(3)).toContainText(
    "Emailed its owner.",
  );
  await expect(
    main(page).getByText("Run by hand", { exact: true }),
  ).toBeVisible();

  // The list says how it last ran.
  await page.goto("/automations");
  const row = page.getByRole("grid", { name: "Automations" }).getByRole("row", {
    name: /Morning Stripe summary/,
  });
  await expect(row).toContainText("Succeeded");
  await expect(row).toContainText("On a schedule");
});

test("edits its steps in the outline, and waits for approval until approved", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/automations/new?template=load-after-sync");
  await page.getByRole("radio", { name: "Outline" }).click();
  const outline = page.getByRole("list", { name: "Steps" });
  // The load waits for nothing it can be told to: take it out, and email
  // instead.
  await page.getByRole("button", { name: /^Change the step: Load/ }).click();
  await page.getByRole("menuitem", { name: "Remove this step" }).click();
  await page
    .getByRole("button", { name: /^Change the step: When a sync/ })
    .click();
  await page.getByRole("menuitem", { name: "Email me" }).click();
  await expect(outline.getByRole("listitem")).toHaveCount(2);
  await expect(page.locator(".au-sentence")).toHaveText(
    "When a sync of “Stripe” succeeds, email you what it found.",
  );
  await page.getByLabel("Subject (optional)").fill("Stripe synced");
  await page.getByRole("button", { name: "Review and save" }).click();
  await page.getByRole("button", { name: "Save, turn on later" }).click();

  // Saved, it waits: what its owner approves is what it shows.
  await expect(
    main(page).getByText("It waits for your approval"),
  ).toBeVisible();
  await expect(main(page).getByRole("button", { name: "Run now" })).toHaveCount(
    0,
  );
  await expect(
    main(page).getByRole("list", { name: "Steps to approve" }),
  ).toContainText("Email you “Stripe synced”");
  await main(page).getByRole("button", { name: "Approve and turn on" }).click();
  await expect(main(page).getByText("On", { exact: true })).toBeVisible();
  await expect(main(page).getByText("It waits for your approval")).toHaveCount(
    0,
  );

  // An edit of its steps waits for approval again.
  await main(page).getByRole("link", { name: "Edit" }).click();
  await page.getByRole("radio", { name: "Outline" }).click();
  await page
    .getByRole("button", { name: /^Change the step: Email you/ })
    .click();
  await page.getByRole("menuitem", { name: "Ask the Assistant" }).click();
  await page
    .getByLabel("What should it do?")
    .fill("Say how many invoices changed.");
  await page.getByRole("button", { name: "Review and save" }).click();
  await page.getByRole("button", { name: "Save, turn on later" }).click();
  await expect(
    main(page).getByText("It waits for your approval"),
  ).toBeVisible();
});

test("fires on its schedule when the time comes", async ({ page }) => {
  await mock.configure({ automationStepMs: 100 });
  await signIn(page);
  const id = await make(page, "Weekly note", [
    {
      id: "start",
      kind: "trigger",
      config: {
        kind: "schedule",
        every: "week",
        weekday: "monday",
        at: "09:00",
        time_zone: "UTC",
      },
      position: at(0),
    },
    step("tell", "notify", { message: "Monday" }, 150),
  ]);
  await page.goto(`/automations/${id}`);
  await expect(main(page).getByText("No runs yet")).toBeVisible();
  // A week on, by the mock's clock: the page sees the run its schedule fired.
  await mock.configure({ automationClockMs: 8 * 24 * 3_600_000 });
  const runs = page.getByRole("grid", { name: "Runs" });
  await expect(runs.getByRole("row", { name: /On its schedule/ })).toHaveCount(
    1,
    {
      timeout: 20_000,
    },
  );
  await runs.getByRole("link", { name: "On its schedule" }).click();
  await ended(page, "Succeeded");
});

test("starts after a sync of its connection", async ({ page }) => {
  await mock.configure({ automationStepMs: 100 });
  await signIn(page);
  const id = await make(
    page,
    "After Stripe syncs",
    [
      {
        id: "start",
        kind: "trigger",
        config: {
          kind: "sync_finished",
          connector_id: "conn_acme_stripe",
          outcome: "succeeded",
        },
        position: at(0),
      },
      step(
        "enough",
        "condition",
        { check: "count", op: "at_least", value: 1 },
        150,
      ),
      step("tell", "notify", { message: "Rows synced" }, 300),
    ],
    [
      { from: "start", to: "enough" },
      { from: "enough", to: "tell", branch: "true" },
    ],
  );
  // What ended before it was on starts nothing.
  await page.goto(`/automations/${id}`);
  await expect(main(page).getByText("No runs yet")).toBeVisible();
  await page.goto("/data/connections/conn_acme_stripe");
  await page.getByRole("button", { name: "Sync now" }).click();
  await page.goto(`/automations/${id}`);
  const runs = page.getByRole("grid", { name: "Runs" });
  await expect(
    runs.getByRole("row", { name: /A sync of “Stripe” succeeded/ }),
  ).toHaveCount(1, { timeout: 20_000 });
  await runs
    .getByRole("link", { name: /A sync of “Stripe” succeeded/ })
    .click();
  await ended(page, "Succeeded");
  const steps = page.getByRole("list", { name: "What each step did" });
  await expect(steps.getByRole("listitem").nth(1)).toContainText(
    "It was true: it went the Yes way.",
  );
});

test("pauses after three failed runs in a row, and says why", async ({
  page,
}) => {
  await mock.configure({
    automationStepMs: 100,
    agentStepMs: 60,
    agentOutcome: "fail",
  });
  await signIn(page);
  const id = await make(page, "Summaries", [
    manual,
    step(
      "ask",
      "assistant",
      { instruction: "Summarise overdue invoices." },
      150,
    ),
    step("tell", "notify", { message: "" }, 300),
  ]);
  for (let i = 0; i < 3; i++) {
    await page.goto(`/automations/${id}`);
    await main(page).getByRole("button", { name: "Run now" }).click();
    await ended(page, "Failed");
    await expect(
      main(page).getByText(/^It failed\. The assistant step "ask" failed/),
    ).toBeVisible();
    const steps = page.getByRole("list", { name: "What each step did" });
    await expect(steps.getByRole("listitem").nth(1)).toContainText(
      "The model failed: overloaded.",
    );
    await expect(steps.getByRole("listitem").nth(2)).toContainText("Not run");
  }
  await page.goto(`/automations/${id}`);
  await expect(
    main(page).getByText(
      "Paused after 3 failed runs in a row. Fix what failed, then resume it.",
    ),
  ).toBeVisible();
  await expect(
    main(page).getByText(/^The last one failed because:/),
  ).toBeVisible();
  await mock.configure({ agentOutcome: "" as never });
  await main(page).getByRole("button", { name: "Turn on" }).click();
  await expect(main(page).getByText("On", { exact: true })).toBeVisible();
});

test("leaves an action the owner didn't approve on the run's page, for them to run", async ({
  page,
}) => {
  await mock.configure({
    automationStepMs: 100,
    agentStepMs: 60,
    loadStepMs: 100,
  });
  await signIn(page);
  const id = await make(page, "Load contracts", [
    manual,
    step(
      "ask",
      "assistant",
      { instruction: "Load Customer contracts and report what it holds." },
      150,
    ),
  ]);
  await page.goto(`/automations/${id}`);
  await main(page).getByRole("button", { name: "Run now" }).click();
  await ended(page, "Succeeded");
  await expect(
    main(page).getByText("One action it prepared waits for you:"),
  ).toBeVisible();
  const card = main(page).getByRole("region", {
    name: /^Action: Load v3 of Customer contracts/,
  });
  await card.getByRole("button", { name: "Load", exact: true }).click();
  await expect(card.getByText("Ran", { exact: true })).toBeVisible();
});

test("shows another member how far a run went, never what it found", async ({
  page,
  browser,
}) => {
  await mock.configure({ automationStepMs: 100, agentStepMs: 60 });
  await signIn(page);
  const id = await make(page, "Ada's summary", [
    manual,
    step(
      "ask",
      "assistant",
      { instruction: "Summarise overdue invoices." },
      150,
    ),
  ]);
  const run = (
    await postToWfe(page, `${A}/${id}:run`, { clientToken: "member" })
  ).run.id as string;
  await page.goto(`/automations/${id}/runs/${run}`);
  await ended(page, "Succeeded");

  const context = await browser.newContext();
  const alan = await context.newPage();
  await signIn(alan, "alan@example.com");
  await alan.goto(`/automations/${id}`);
  await expect(
    main(alan).getByRole("heading", { name: "Ada's summary" }),
  ).toBeVisible();
  for (const name of ["Run now", "Pause", "Edit", "More actions"])
    await expect(main(alan).getByRole("button", { name })).toHaveCount(0);
  await expect(main(alan).getByText("Another member")).toBeVisible();
  await alan.goto(`/automations/${id}/runs/${run}`);
  await ended(alan, "Succeeded");
  await expect(main(alan).getByText("What the Assistant reported")).toHaveCount(
    0,
  );
  // Their own, they may make.
  await alan.goto("/automations");
  await expect(
    main(alan).getByRole("link", { name: "New automation" }),
  ).toBeVisible();
  await context.close();
});

test("the Assistant sets one up from a card its person runs", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 60 });
  await signIn(page);
  await page.goto("/automations");
  // ⌘J, once the shell (whose Assistant button it presses) is on screen.
  await expect(
    page
      .getByRole("button", { name: "Assistant", exact: true })
      .filter({ visible: true })
      .first(),
  ).toBeVisible();
  await page.keyboard.press("ControlOrMeta+j");
  const dock = page.getByRole("complementary", { name: "Assistant" });
  await dock
    .getByRole("textbox", { name: "Message" })
    .fill(
      "Every Monday at 9, sync Stripe and email me a summary of overdue invoices.",
    );
  await dock.getByRole("button", { name: "Send", exact: true }).click();
  const card = dock.getByRole("region", {
    name: /^Action: Set up the automation/,
  });
  await expect(card).toBeVisible({ timeout: 20_000 });
  const steps = card.getByRole("list", { name: "Steps it approves" });
  await expect(steps.getByRole("listitem")).toHaveCount(4);
  await expect(steps).toContainText("Sync “Stripe”");
  await expect(steps).toContainText("When every Monday at 09:00");
  await card.getByRole("button", { name: "Create and turn on" }).click();
  await expect(card.getByText("Ran", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("navigation", { name: "Automations" }),
  ).toContainText("Weekly summary");
  await dock.getByRole("link", { name: /\/automations\/aut_/ }).click();
  await expect(main(page).getByText("On", { exact: true })).toBeVisible();
});
