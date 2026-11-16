import type { Page } from "@playwright/test";
import { expect, loadContracts, mock, signIn, test } from "./support/mock";

// Founder answers questions about the data with the Analyst: the answer
// shows in the dock as the Analyst gives it (its steps, citations, charts
// and tables), with Open in Analyst on its thread. Asked for a dashboard,
// it asks the Analyst first and makes the dashboard of that answer's
// visuals at once, without a card to click; Undo deletes it.

const dockOf = (page: Page) =>
  page.getByRole("complementary", { name: "Founder" });

async function say(page: Page, text: string) {
  const dock = dockOf(page);
  await dock.getByRole("textbox", { name: "Message" }).fill(text);
  await dock.getByRole("button", { name: "Send", exact: true }).click();
  await expect(
    dock
      .getByRole("log", { name: "Conversation" })
      .getByText(text, { exact: true }),
  ).toBeVisible();
}

/** Signs Ada in, loads Customer contracts and opens the dock. */
async function ready(page: Page) {
  await mock.configure({
    agentStepMs: 100,
    agentDeltaMs: 10,
    askStepMs: 100,
    loadStepMs: 100,
  });
  await signIn(page);
  await loadContracts(page);
  await page.getByRole("button", { name: "Founder", exact: true }).click();
  return dockOf(page);
}

const ASKED =
  "I want to make a dashboard that shows which invoices has each account's subscriptions been billed and which are unpaid or overdue";

test("asked for a dashboard, asks the Analyst, makes it at once and undoes it", async ({
  page,
}) => {
  const dock = await ready(page);
  await say(page, ASKED);
  // It asked the Analyst, whose answer shows in the dock: its words with
  // their citations, and its two visuals.
  await expect(
    dock.getByText("Asked the Analyst", { exact: true }),
  ).toBeVisible();
  const question = dock.getByRole("article", { name: `Question: ${ASKED}` });
  const answer = question.getByRole("region", { name: "Answer" });
  await expect(answer).toContainText("unpaid or overdue", { timeout: 20_000 });
  await expect(answer.locator(".ak-chip").first()).toBeVisible();
  await expect(
    answer.getByRole("figure", { name: "Invoices by account" }),
  ).toBeVisible({ timeout: 15_000 });
  await expect(
    answer.getByRole("figure", { name: "Unpaid or overdue invoices" }),
  ).toBeVisible();

  // The dashboard of them is made without a card to click.
  const card = dock.getByRole("region", {
    name: "Action: Made the dashboard “Invoices by account”",
  });
  await expect(card).toBeVisible({ timeout: 20_000 });
  await expect(
    dock.getByRole("button", { name: "Create", exact: true }),
  ).toHaveCount(0);
  await expect(card.getByText("Made", { exact: true })).toBeVisible();
  await expect(
    dock.getByText(/I made the dashboard “Invoices by account”/),
  ).toBeVisible();
  // The answer shows once, though two steps named its question.
  await expect(question).toHaveCount(1);

  const open = card.getByRole("link", { name: "Open the dashboard" });
  const href = (await open.getAttribute("href"))!;
  expect(href).toMatch(/^\/dashboards\/dash_/);
  const id = href.split("/").pop()!;
  await open.click();
  await expect(page).toHaveURL(new RegExp(`/dashboards/${id}$`));
  await expect(
    page.getByRole("heading", { level: 1, name: "Invoices by account" }),
  ).toBeVisible();
  const main = page.getByRole("main");
  await expect(
    main.getByRole("figure", { name: "Invoices by account" }),
  ).toBeVisible();
  await expect(
    main
      .getByRole("figure", { name: "Unpaid or overdue invoices" })
      .locator(".recharts-bar-rectangle")
      .first(),
  ).toBeVisible();

  // Undo deletes it.
  await page.goBack();
  await expect(page).not.toHaveURL(/\/dashboards\//);
  await card.getByRole("button", { name: "Undo" }).click();
  await expect(
    card.getByText("Undone: the dashboard was deleted."),
  ).toBeVisible();
  await expect(card.getByRole("button", { name: "Undo" })).toHaveCount(0);
  const read = await page.request.get(
    `/papi/v1/workspaces/ws_acme/foundry/dashboards/${id}`,
  );
  expect(read.status()).toBe(404);
  await page.goto("/dashboards");
  await expect(
    page.getByRole("heading", { name: "No dashboards yet" }),
  ).toBeVisible();
  // After a reload the card still says so: its dashboard is gone.
  await expect(
    dockOf(page)
      .getByRole("region", {
        name: "Action: Made the dashboard “Invoices by account”",
      })
      .getByText("Undone: the dashboard was deleted."),
  ).toBeVisible({ timeout: 15_000 });
});

test("answers a data question with the Analyst's answer, which opens in the Analyst", async ({
  page,
}) => {
  const dock = await ready(page);
  const text = "Which customers have unpaid or overdue invoices?";
  await say(page, text);
  await expect(
    dock.getByText(
      "That's the Analyst's answer, from the current load of Customer contracts.",
    ),
  ).toBeVisible({ timeout: 20_000 });
  // Nothing to make: no card.
  await expect(dock.getByRole("region", { name: /^Action:/ })).toHaveCount(0);
  const question = dock.getByRole("article", { name: `Question: ${text}` });
  await expect(question).toHaveCount(1);
  const answer = question.getByRole("region", { name: "Answer" });
  // A citation of an entity opens its page in the load.
  await expect(answer.locator("a.ak-chip").first()).toHaveAttribute(
    "href",
    /\/explore\/ont_acme_contracts\/entities\//,
  );
  await expect(answer.locator("button.ak-chip").first()).toBeVisible();
  // The table, and the chart with its Chart/Table toggle.
  await expect(
    answer
      .getByRole("figure", { name: "Invoices by account" })
      .getByRole("grid", { name: "Invoices by account" }),
  ).toBeVisible();
  const owing = answer.getByRole("figure", {
    name: "Unpaid or overdue invoices",
  });
  const bars = owing.locator(".recharts-bar-rectangle");
  await expect(bars.first()).toBeVisible();
  await owing.getByRole("radio", { name: "Table" }).click();
  await expect(
    owing.getByRole("grid", { name: "Unpaid or overdue invoices" }),
  ).toBeVisible();
  await expect(bars).toHaveCount(0);
  await owing.getByRole("radio", { name: "Chart" }).click();
  await expect(bars.first()).toBeVisible();

  // Open in Analyst opens its thread there.
  await question.getByRole("link", { name: "Open in Analyst" }).click();
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts\?thread=[\w-]+/);
  const there = page
    .getByRole("main")
    .getByRole("article", { name: `Question: ${text}` });
  await expect(there.getByRole("region", { name: "Answer" })).toContainText(
    "unpaid or overdue",
  );
});

test("in the dock, the Analyst's steps and queries open as on its page", async ({
  page,
}) => {
  const dock = await ready(page);
  const text = "Which customers have unpaid or overdue invoices?";
  await say(page, text);
  await expect(
    dock.getByText(
      "That's the Analyst's answer, from the current load of Customer contracts.",
    ),
  ).toBeVisible({ timeout: 20_000 });

  // Founder's own step opens on what it asked.
  const step = dock.getByRole("group").filter({ hasText: "Asked the Analyst" });
  await step.getByText("Asked the Analyst", { exact: true }).click();
  await expect(step.getByText(/"question": "Which customers/)).toBeVisible();

  // The Analyst's steps open and close.
  const question = dock.getByRole("article", { name: `Question: ${text}` });
  const steps = question.getByRole("list", { name: "Steps" });
  await expect(steps).toHaveCount(0);
  await question.getByRole("button", { name: "Show the steps" }).click();
  await expect(steps.getByRole("listitem").first()).toHaveAccessibleName(
    "Step 1: Read what the load holds",
  );
  await question.getByRole("button", { name: "Hide the steps" }).click();
  await expect(steps).toHaveCount(0);

  // Show the query lights the step that ran it, in this card.
  const answer = question.getByRole("region", { name: "Answer" });
  await answer
    .getByRole("figure", { name: "Invoices by account" })
    .getByRole("button", { name: "Show the query" })
    .click();
  const lit = steps.locator("li.is-lit");
  await expect(lit).toHaveCount(1);
  await expect(lit).toContainText("Invoice → billed_to → Customer");
  await answer
    .getByRole("figure", { name: "Unpaid or overdue invoices" })
    .getByRole("button", { name: "Show the query" })
    .click();
  await expect(lit).toHaveCount(1);
  await expect(lit).not.toContainText("billed_to");
});
