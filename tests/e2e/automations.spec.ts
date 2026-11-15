import type { Page } from "@playwright/test";
import { expect, mock, postToWfe, signIn, test } from "./support/mock";

// Automations: a person's steps Foundry runs as them, on a schedule or when
// a sync, a load or documents arrive. The overview counts the runs of the
// last days and lists the automations; a new one starts as a conversation
// with the Assistant, from a template, or from scratch; an automation opens
// on a full-page canvas, where its owner edits its steps (or in the
// outline), saves them through the review of what runs unattended, and
// goes Live; nothing runs until its owner approves exactly what runs; the
// Runs tab colours the canvas by how a run went; three failures in a row
// pause it; another member sees how far a run went, never what it found.

const A = "/v1/workspaces/ws_acme/foundry/automations";
const HOUR = 3_600_000,
  DAY = 24 * HOUR;
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

const inspector = (page: Page) =>
  page.getByRole("complementary", { name: "Inspector" });
const live = (page: Page) => page.getByRole("switch", { name: "Live" });
const steps = (page: Page) =>
  page.getByRole("list", { name: "What each step did" });
const canvas = (page: Page) =>
  page.getByRole("region", { name: "Steps on a canvas" });
/** The name in the page's bar. */
const title = (page: Page, name: string) =>
  page
    .getByRole("navigation", { name: "Breadcrumbs" })
    .getByRole("heading", { name });
/** The run open has ended as `outcome`, said in the inspector's head. */
const ended = (page: Page, outcome: string) =>
  expect(
    inspector(page)
      .locator(".au-summary-head")
      .getByText(outcome, { exact: true }),
  ).toBeVisible({ timeout: 20_000 });

test("the overview counts the runs of the last days and lists the automations, which the sidebar lists only on one", async ({
  page,
}) => {
  await signIn(page);
  const tell = step("tell", "notify", { message: "Hi" }, 150);
  const morning = await make(page, "Morning check", [manual, tell]);
  const evening = await make(page, "Evening check", [manual, tell]);
  await mock.pastRuns(morning, [
    { status: "succeeded", ago: HOUR, usdMicros: 30_000 },
    { status: "failed", ago: 2 * DAY },
  ]);
  await mock.pastRuns(evening, [
    { status: "succeeded", ago: 10 * DAY },
    { status: "cancelled", ago: 3 * HOUR },
  ]);
  await page.goto("/automations");
  const stat = (label: string) =>
    page.locator(".au-stat").filter({ hasText: label }).locator("dd").first();
  await expect(stat("Runs")).toHaveText("3");
  await expect(stat("Succeeded")).toHaveText("1");
  await expect(stat("Failed")).toHaveText("1");
  await expect(stat("Waiting for someone")).toHaveText("0");
  await expect(stat("Spend")).toHaveText("$0.03");
  await expect(page.locator(".au-chart")).toBeVisible();
  await page.getByRole("radio", { name: "30 days" }).click();
  await expect(stat("Runs")).toHaveText("4");
  await expect(stat("Succeeded")).toHaveText("2");

  const grid = page.getByRole("grid", { name: "Automations" });
  const row = grid.getByRole("row", { name: /Morning check/ });
  await expect(row).toContainText("Live");
  await expect(row).toContainText("2d ago");
  await expect(row).toContainText("You");
  await expect(grid.getByRole("row", { name: /Evening check/ })).toContainText(
    "Cancelled",
  );

  // The list in the sidebar is for going between automations: it shows on
  // one, never on the overview or a new one's page.
  const list = page.getByRole("navigation", { name: "Automations" });
  await expect(list).toHaveCount(0);
  await grid.getByRole("link", { name: "Morning check" }).click();
  await expect(page).toHaveURL(new RegExp(`/automations/${morning}$`));
  await expect(list).toContainText("Evening check");
  await list.getByRole("link", { name: /Evening check/ }).click();
  await expect(page).toHaveURL(new RegExp(`/automations/${evening}$`));
  await expect(title(page, "Evening check")).toBeVisible();
  await page
    .getByRole("region", { name: "Automations" })
    .getByRole("link", { name: "New automation" })
    .click();
  await expect(page).toHaveURL(/\/automations\/new$/);
  await expect(list).toHaveCount(0);
});

test("a new automation starts as a conversation with the Assistant, whose card opens it on its canvas", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 60 });
  await signIn(page);
  await page.goto("/automations");
  await expect(page.getByText("No automations yet")).toBeVisible();
  await page.getByRole("link", { name: "New automation" }).first().click();
  await expect(page).toHaveURL(/\/automations\/new$/);
  await expect(
    page.getByRole("heading", { name: "What should happen?" }),
  ).toBeVisible();
  // A template's words fill the composer; the page tells the Assistant the
  // person is making an automation, so it sets one up from words that say
  // nothing of automating.
  await page.getByRole("button", { name: "Load after every sync" }).click();
  await expect(page.getByRole("textbox", { name: "Message" })).toHaveValue(
    /^When a connection finishes syncing/,
  );
  await expect(page.getByText("Making a new automation")).toBeVisible();
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await expect(page).toHaveURL(/\/automations\/new\?thread=/);
  const card = page.getByRole("region", {
    name: /^Action: Set up the automation/,
  });
  await expect(card).toBeVisible({ timeout: 20_000 });
  // Nothing is made until the person's click, which opens it.
  await expect(
    page.getByRole("navigation", { name: "Automations" }),
  ).toHaveCount(0);
  await card.getByRole("button", { name: "Create and turn on" }).click();
  await expect(page).toHaveURL(/\/automations\/aut_[^/?]+$/);
  await expect(live(page)).toBeChecked();
  await expect(canvas(page).locator(".react-flow__node")).toHaveCount(2);

  // The conversation is the Assistant's: the dock lists it.
  await page.keyboard.press("ControlOrMeta+j");
  const dock = page.getByRole("complementary", { name: "Assistant" });
  await dock.getByRole("button", { name: /^Threads:/ }).click();
  await expect(
    page.getByRole("menuitemradio", {
      name: /^When a connection finishes syncing/,
    }),
  ).toBeVisible();
});

test("starts from scratch on the full canvas: adds, joins, tidies and inspects its steps", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/automations/new");
  await page.getByRole("link", { name: "Start from scratch" }).click();
  await expect(page).toHaveURL(/\/automations\/new\?template=blank$/);
  const nodes = canvas(page).locator(".react-flow__node"),
    edges = canvas(page).locator(".react-flow__edge");
  // Only its trigger, which it can't be saved with.
  await expect(nodes).toHaveCount(1);
  await expect(
    inspector(page).getByRole("list", { name: "Before you save" }),
  ).toContainText("Add a step after what starts it.");
  await expect(
    page.getByRole("button", { name: "Review and save" }),
  ).toBeDisabled();

  // Add step, from the toolbar: after the trigger, joined, selected.
  await page.getByRole("button", { name: "Add step" }).click();
  await page.getByRole("menuitem", { name: "Ask the Assistant" }).click();
  await expect(nodes).toHaveCount(2);
  await expect(edges).toHaveCount(1);
  await page
    .getByLabel("What should it do?")
    .fill("How many ontologies are there?");
  // A, outside a field, opens Add step: after the last step.
  await canvas(page)
    .locator(".react-flow__pane")
    .click({ position: { x: 24, y: 24 } });
  await expect(inspector(page).locator(".au-summary-text")).toBeVisible();
  await page.keyboard.press("a");
  await page.getByRole("menuitem", { name: "Email me" }).click();
  await expect(nodes).toHaveCount(3);
  await expect(edges).toHaveCount(2);
  await expect(page.getByLabel("Subject (optional)")).toBeVisible();

  // Cut a join in the outline, and join them again on the canvas.
  await page.getByRole("button", { name: "Outline" }).click();
  await page
    .getByRole("button", { name: /^Change the step: Ask the Assistant/ })
    .click();
  await page.getByRole("menuitem", { name: /^Email you/ }).click();
  await page.getByRole("button", { name: "Outline" }).click();
  await expect(edges).toHaveCount(1);
  const asks = nodes.filter({ hasText: "How many ontologies" }),
    email = nodes.filter({ hasText: "Email you" });
  await asks
    .locator(".react-flow__handle-bottom")
    .dragTo(email.locator(".react-flow__handle-top"));
  await expect(edges).toHaveCount(2);

  // Moved off its row, Tidy puts it back under the step before it.
  const box = (await email.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + 10);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2 + 260, box.y + 140, {
    steps: 8,
  });
  await page.mouse.up();
  await expect(email).not.toHaveCSS(
    "transform",
    /matrix\(1, 0, 0, 1, 0, 300\)/,
  );
  await page.getByRole("button", { name: "Tidy the layout" }).click();
  await expect(email).toHaveCSS("transform", "matrix(1, 0, 0, 1, 0, 300)");

  // Selecting a step shows its settings; Back, the summary.
  await nodes.filter({ hasText: "Only when you run it" }).click();
  await expect(
    inspector(page).getByText("Starts", { exact: true }),
  ).toBeVisible();
  await inspector(page)
    .getByRole("button", { name: "Back to the summary" })
    .click();
  await expect(inspector(page).locator(".au-summary-text")).toHaveText(
    "When you run it, ask the Assistant: “How many ontologies are there?”, then email you what it found.",
  );

  await page
    .getByRole("textbox", { name: "Name" })
    .fill("Count the ontologies");
  await page.getByRole("button", { name: "Review and save" }).click();
  const review = page.getByRole("dialog", {
    name: "Review what runs unattended",
  });
  await expect(
    review
      .getByRole("list", { name: "Steps that run unattended" })
      .getByRole("listitem"),
  ).toHaveCount(3);
  await review.getByRole("button", { name: "Save and turn on" }).click();
  await expect(page).toHaveURL(/\/automations\/aut_[^/?]+$/);
  await expect(live(page)).toBeChecked();
  await expect(title(page, "Count the ontologies")).toBeVisible();
});

test("starts from a template, runs only once approved, runs now, and colours the run on the canvas", async ({
  page,
}) => {
  await mock.configure({ automationStepMs: 100, agentStepMs: 60 });
  await signIn(page);
  await page.getByRole("link", { name: "Automations", exact: true }).click();
  await expect(page).toHaveURL(/\/automations$/);
  await page.getByRole("link", { name: /^Sync, then summarise/ }).click();
  // The template's steps, its connection the workspace's first, said in
  // words and drawn on the canvas.
  await expect(inspector(page).locator(".au-summary-text")).toContainText(
    "Every day at 08:00, sync “Stripe”, then ask the Assistant:",
  );
  await expect(canvas(page).locator(".au-node")).toHaveCount(4);
  await expect(live(page)).toBeDisabled();
  await page
    .getByRole("textbox", { name: "Name" })
    .fill("Morning Stripe summary");
  await page.getByRole("button", { name: "Review and save" }).click();
  const review = page.getByRole("dialog", {
    name: "Review what runs unattended",
  });
  const unattended = review.getByRole("list", {
    name: "Steps that run unattended",
  });
  await expect(unattended.getByRole("listitem")).toHaveCount(4);
  await expect(unattended).toContainText("Sync “Stripe”");
  await expect(unattended).toContainText("Runs without asking you, as you.");
  await expect(unattended).toContainText("Changes nothing without asking you");
  await review.getByRole("button", { name: "Save and turn on" }).click();

  await expect(page).toHaveURL(/\/automations\/aut_[^/?]+$/);
  await expect(live(page)).toBeChecked();
  await page.getByRole("button", { name: "Run now" }).click();
  await expect(page).toHaveURL(/\/automations\/aut_[^/]+\/runs\/arun_/);
  await ended(page, "Succeeded");
  const coloured = page.getByRole("region", {
    name: "Its steps on a canvas, coloured by how each went",
  });
  await expect(coloured.locator(".au-node--succeeded")).toHaveCount(4);
  await expect(page.getByRole("tab", { name: /Runs/ })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await expect(
    page.getByRole("list", { name: "Runs" }).getByRole("listitem"),
  ).toHaveCount(1);
  await expect(steps(page).getByRole("listitem")).toHaveCount(4);
  await expect(steps(page).getByRole("listitem").nth(1)).toContainText(
    "Started a sync of its connector.",
  );
  await expect(steps(page).getByRole("listitem").nth(2)).toContainText(
    "What the Assistant reported",
  );
  await expect(steps(page).getByRole("listitem").nth(3)).toContainText(
    "Emailed its owner.",
  );
  // A step selected on the run's canvas: what it did, alone.
  await coloured
    .locator(".react-flow__node")
    .filter({ hasText: "Sync" })
    .click();
  await expect(inspector(page)).toContainText("What happened");
  await expect(inspector(page)).toContainText(
    "Started a sync of its connector.",
  );
  await expect(steps(page)).toHaveCount(0);

  // The Editor tab is its steps again, as they stand.
  await page.getByRole("tab", { name: "Editor" }).click();
  await expect(canvas(page)).toBeVisible();
  await expect(canvas(page).locator(".au-node--succeeded")).toHaveCount(0);

  // The overview says how it last ran.
  await page.goto("/automations");
  const row = page.getByRole("grid", { name: "Automations" }).getByRole("row", {
    name: /Morning Stripe summary/,
  });
  await expect(row).toContainText("Succeeded");
  await expect(row).toContainText("On a schedule");
  await expect(row).toContainText("Live");
});

test("edits its steps in the outline, and waits for approval until approved", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/automations/new?template=load-after-sync");
  await page.getByRole("button", { name: "Outline" }).click();
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
  await page.getByLabel("Subject (optional)").fill("Stripe synced");
  await inspector(page)
    .getByRole("button", { name: "Back to the summary" })
    .click();
  await expect(inspector(page).locator(".au-summary-text")).toHaveText(
    "When a sync of “Stripe” succeeds, email you “Stripe synced”.",
  );
  await page.getByRole("button", { name: "Review and save" }).click();
  await page.getByRole("button", { name: "Save, turn on later" }).click();

  // Saved, it waits: what its owner approves is what it shows.
  await expect(page).toHaveURL(/\/automations\/aut_[^/?]+$/);
  await expect(
    inspector(page).getByText("It waits for your approval"),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Run now" })).toHaveCount(0);
  await expect(live(page)).not.toBeChecked();
  await inspector(page)
    .getByRole("button", { name: "Review and approve" })
    .click();
  const approve = page.getByRole("dialog", {
    name: "Approve what runs unattended",
  });
  await expect(
    approve.getByRole("list", { name: "Steps to approve" }),
  ).toContainText("Email you “Stripe synced”");
  await approve.getByRole("button", { name: "Approve and go live" }).click();
  await expect(live(page)).toBeChecked();
  await expect(
    inspector(page).getByText("It waits for your approval"),
  ).toHaveCount(0);

  // An edit of its steps waits for approval again.
  await page.getByRole("button", { name: "Outline" }).click();
  await page
    .getByRole("button", { name: /^Change the step: Email you/ })
    .click();
  await page.getByRole("menuitem", { name: "Ask the Assistant" }).click();
  await page
    .getByLabel("What should it do?")
    .fill("Say how many invoices changed.");
  await expect(page.getByText("Unsaved changes")).toBeVisible();
  await expect(live(page)).toBeDisabled();
  await page.getByRole("button", { name: "Review and save" }).click();
  await page.getByRole("button", { name: "Save, turn on later" }).click();
  await expect(page.getByText("Unsaved changes")).toHaveCount(0);
  await inspector(page)
    .getByRole("button", { name: "Back to the summary" })
    .click();
  await expect(
    inspector(page).getByText("It waits for your approval"),
  ).toBeVisible();
  await expect(live(page)).not.toBeChecked();
});

test("the Live switch approves a waiting automation and pauses it; spaces are no change; a step deleted on the canvas keeps the chain joined; Settings renames and deletes it", async ({
  page,
}) => {
  await signIn(page);
  const made = (
    await postToWfe(page, A, {
      name: "Waiting chain",
      clientToken: "e2e-waiting-chain",
      specJson: JSON.stringify({
        v: 1,
        nodes: [
          manual,
          step("ask", "assistant", { instruction: "Count the invoices." }, 150),
          step("tell", "notify", { message: "Counted" }, 300),
        ],
        edges: [
          { from: "start", to: "ask" },
          { from: "ask", to: "tell" },
        ],
      }),
    })
  ).automation;
  await page.goto(`/automations/${made.id}`);
  await expect(inspector(page).locator(".hf-status")).toHaveText(
    "Needs approval",
  );
  await live(page).click();
  await page
    .getByRole("dialog", { name: "Approve what runs unattended" })
    .getByRole("button", { name: "Approve and go live" })
    .click();
  await expect(live(page)).toBeChecked();
  await live(page).click();
  await expect(live(page)).not.toBeChecked();
  await expect(
    inspector(page).getByText("Paused: it runs only when you run it by hand."),
  ).toBeVisible();
  await live(page).click();
  await expect(live(page)).toBeChecked();

  // A space at the end of an instruction is no change to save.
  const nodes = canvas(page).locator(".react-flow__node");
  await nodes.filter({ hasText: "Count the invoices." }).click();
  await page.getByLabel("What should it do?").fill("Count the invoices. ");
  await expect(
    page.getByRole("button", { name: "Review and save" }),
  ).toHaveCount(0);

  // Deleted on the canvas, the step's neighbours join: nothing is cut off.
  await nodes.filter({ hasText: "Count the invoices." }).click();
  await page.keyboard.press("Delete");
  await expect(nodes).toHaveCount(2);
  await expect(canvas(page).locator(".react-flow__edge")).toHaveCount(1);
  await expect(
    inspector(page).getByRole("list", { name: "Before you save" }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Review and save" }),
  ).toBeEnabled();
  await page.getByRole("button", { name: "Discard" }).click();
  await expect(nodes).toHaveCount(3);

  // Renamed in place, it keeps its approval.
  await page.getByRole("button", { name: "Rename Waiting chain" }).click();
  await page.getByRole("textbox", { name: "Name" }).fill("Invoice count");
  await page.keyboard.press("Enter");
  await expect(title(page, "Invoice count")).toBeVisible();
  await expect(live(page)).toBeChecked();

  await page.getByRole("tab", { name: "Settings" }).click();
  await expect(page.getByRole("heading", { name: "Approval" })).toBeVisible();
  await expect(
    page.getByRole("list", { name: "Approved steps" }).getByRole("listitem"),
  ).toHaveCount(3);
  await page.getByRole("button", { name: "Delete automation" }).click();
  await page
    .getByRole("dialog", { name: "Delete “Invoice count”?" })
    .getByRole("button", { name: "Delete" })
    .click();
  await expect(page).toHaveURL(/\/automations$/);
  await expect(page.getByText("No automations yet")).toBeVisible();
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
  await page.goto(`/automations/${id}?tab=runs`);
  await expect(page.getByText("No runs yet")).toBeVisible();
  // A week on, by the mock's clock: the page sees the run its schedule fired.
  await mock.configure({ automationClockMs: 8 * 24 * 3_600_000 });
  const runs = page.getByRole("list", { name: "Runs" });
  await expect(runs.getByRole("link", { name: /On its schedule/ })).toHaveCount(
    1,
    { timeout: 20_000 },
  );
  await runs.getByRole("link", { name: /On its schedule/ }).click();
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
  await page.goto(`/automations/${id}?tab=runs`);
  await expect(page.getByText("No runs yet")).toBeVisible();
  await page.goto("/data/connections/conn_acme_stripe");
  await page.getByRole("button", { name: "Sync now" }).click();
  await page.goto(`/automations/${id}?tab=runs`);
  const runs = page.getByRole("list", { name: "Runs" });
  await expect(
    runs.getByRole("link", { name: /A sync of “Stripe” succeeded/ }),
  ).toHaveCount(1, { timeout: 20_000 });
  await runs
    .getByRole("link", { name: /A sync of “Stripe” succeeded/ })
    .click();
  await ended(page, "Succeeded");
  await expect(steps(page).getByRole("listitem").nth(1)).toContainText(
    "It was met: it went the “When met” way.",
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
    await page.getByRole("button", { name: "Run now" }).click();
    await ended(page, "Failed");
    await expect(
      inspector(page).getByText(/^It failed\. The assistant step "ask" failed/),
    ).toBeVisible();
    await expect(steps(page).getByRole("listitem").nth(1)).toContainText(
      "The model failed: overloaded.",
    );
    await expect(steps(page).getByRole("listitem").nth(2)).toContainText(
      "Not run",
    );
    await expect(
      page
        .getByRole("region", {
          name: "Its steps on a canvas, coloured by how each went",
        })
        .locator(".au-node--failed"),
    ).toHaveCount(1);
  }
  await page.goto(`/automations/${id}`);
  await expect(
    inspector(page).getByText(
      "Paused after 3 failed runs in a row. Fix what failed, then resume it.",
    ),
  ).toBeVisible();
  await expect(
    inspector(page).getByText(/^The last one failed because:/),
  ).toBeVisible();
  await expect(inspector(page).locator(".hf-status").first()).toHaveText(
    "Paused after failures",
  );
  await mock.configure({ agentOutcome: "" as never });
  await live(page).click();
  await expect(live(page)).toBeChecked();
});

test("leaves an action the owner didn't approve on the run, for them to run", async ({
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
  await page.getByRole("button", { name: "Run now" }).click();
  await ended(page, "Succeeded");
  await expect(
    inspector(page).getByText("One action it prepared waits for you:"),
  ).toBeVisible();
  const card = inspector(page).getByRole("region", {
    name: /^Action: Load v3 of “Customer contracts”/,
  });
  await card.getByRole("button", { name: "Load", exact: true }).click();
  await expect(card.getByText("Ran", { exact: true })).toBeVisible();
});

test("shows another member its steps and how far a run went, never what it found, and lets them change nothing", async ({
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
  await expect(title(alan, "Ada's summary")).toBeVisible();
  for (const name of ["Run now", "Rename Ada's summary", "Add step"])
    await expect(alan.getByRole("button", { name })).toHaveCount(0);
  await expect(live(alan)).toBeDisabled();
  await expect(inspector(alan).getByText("Another member")).toBeVisible();
  await canvas(alan)
    .locator(".react-flow__node")
    .filter({ hasText: "Summarise overdue invoices." })
    .click();
  await expect(alan.getByLabel("What should it do?")).toBeDisabled();
  await expect(
    inspector(alan).getByText("Only its owner changes its steps"),
  ).toBeVisible();
  await alan.getByRole("tab", { name: "Settings" }).click();
  await expect(alan.getByRole("textbox", { name: "Name" })).toHaveAttribute(
    "readonly",
    "",
  );
  await expect(
    alan.getByRole("button", { name: "Delete automation" }),
  ).toHaveCount(0);
  await alan.goto(`/automations/${id}/runs/${run}`);
  await ended(alan, "Succeeded");
  await expect(
    inspector(alan).getByText("What the Assistant reported"),
  ).toHaveCount(0);
  // Their own, they may make.
  await alan.goto("/automations");
  await expect(
    alan.getByRole("link", { name: "New automation" }),
  ).toBeVisible();
  await context.close();
});

test("on a phone the canvas only shows the steps: the inspector is a sheet, and the outline edits them", async ({
  browser,
}) => {
  const context = await browser.newContext({
    viewport: { width: 390, height: 844 },
    isMobile: true,
    hasTouch: true,
  });
  const page = await context.newPage();
  await signIn(page);
  const id = await make(page, "Phone check", [
    manual,
    step("ask", "assistant", { instruction: "Count the invoices." }, 150),
  ]);
  await page.goto(`/automations/${id}`);
  const nodes = canvas(page).locator(".react-flow__node");
  await expect(nodes).toHaveCount(2);
  await expect(page.getByRole("button", { name: "Add step" })).toHaveCount(0);
  await expect(inspector(page)).toHaveCount(0);
  await nodes.filter({ hasText: "Count the invoices." }).tap();
  const sheet = page.getByRole("dialog", { name: "Step" });
  await expect(sheet.getByLabel("What should it do?")).toHaveValue(
    "Count the invoices.",
  );
  await sheet.getByRole("button", { name: "Close" }).click();
  await page.getByRole("button", { name: "Outline" }).click();
  await page
    .getByRole("button", { name: /^Change the step: Ask the Assistant/ })
    .click();
  await page.getByRole("menuitem", { name: "Email me" }).click();
  // The step it added opens in the sheet.
  await expect(
    page.getByRole("dialog", { name: "Step" }).getByLabel("Subject (optional)"),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(
    page.getByRole("list", { name: "Steps" }).getByRole("listitem"),
  ).toHaveCount(3);
  await expect(
    page.getByRole("button", { name: "Review and save" }),
  ).toBeVisible();
  await context.close();
});

test("the Assistant sets one up from a card its person runs, in the dock", async ({
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
  const unattended = card.getByRole("list", { name: "Steps it approves" });
  await expect(unattended.getByRole("listitem")).toHaveCount(4);
  await expect(unattended).toContainText("Sync “Stripe”");
  await expect(unattended).toContainText("When every Monday at 09:00");
  await card.getByRole("button", { name: "Create and turn on" }).click();
  await expect(card.getByText("Ran", { exact: true })).toBeVisible();
  // The overview reads it again.
  await expect(
    page.getByRole("grid", { name: "Automations" }).getByRole("row", {
      name: /Weekly summary/,
    }),
  ).toBeVisible();
  await dock.getByRole("link", { name: /\/automations\/aut_/ }).click();
  await expect(live(page)).toBeChecked();
  await expect(
    page.getByRole("navigation", { name: "Automations" }),
  ).toContainText("Weekly summary");
});
