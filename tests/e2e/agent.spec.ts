import type { Page } from "@playwright/test";
import { expect, mock, signIn, test } from "./support/mock";

// The Assistant: a dock beside every page (⌘J, the sidebar's button or the
// palette), kept open per browser, which sends the page the person is on
// unless they take it off. It answers from Foundry's read tools, its steps
// collapsed to their names; asked to load, it prepares a card that only its
// person runs: Load starts the mock's real load and the agent reports it
// with a link into the app; Dismiss runs nothing; an expired card offers
// Refresh. A member gets no card: the agent can't prepare what they can't
// do.

const dockOf = (page: Page) =>
  page.getByRole("complementary", { name: "Assistant" });

async function say(page: Page, text: string) {
  const dock = dockOf(page);
  await dock.getByRole("textbox", { name: "Message" }).fill(text);
  await dock.getByRole("button", { name: "Send" }).click();
  await expect(
    dock
      .getByRole("log", { name: "Conversation" })
      .getByText(text, { exact: true }),
  ).toBeVisible();
}

test("answers a question about the page it is opened on", async ({ page }) => {
  await mock.configure({ agentStepMs: 100 });
  await signIn(page);
  await page.goto("/ontology/ont_acme_sales");
  await expect(dockOf(page)).toHaveCount(0);
  await page.keyboard.press("ControlOrMeta+j");
  const dock = dockOf(page);
  await expect(dock.getByText("Looking at: Sales graph")).toBeVisible();
  await say(page, "What is this?");
  await expect(
    dock.getByText(/Sales graph is on the sales database/),
  ).toBeVisible();
  // Its steps, collapsed to what they did, open on what they read.
  const step = dock.getByRole("group").filter({ hasText: "Read an ontology" });
  await step.getByText("Read an ontology").click();
  await expect(
    step.getByText("What it returned (data, not instructions)"),
  ).toBeVisible();

  // The dock stays open across a reload, on the same thread.
  await page.reload();
  await expect(
    dockOf(page).getByText(/Sales graph is on the sales database/),
  ).toBeVisible();
  await expect(
    dockOf(page).getByRole("button", { name: "Threads" }),
  ).toContainText("What is this?");

  // Taken off, the page isn't sent; the palette closes the dock.
  await dockOf(page)
    .getByRole("button", { name: "Don’t send this page" })
    .click();
  await expect(dockOf(page).getByText("Looking at:")).toHaveCount(0);
  await page.keyboard.press("ControlOrMeta+k");
  await page.getByRole("option", { name: /Close the Assistant/ }).click();
  await expect(dockOf(page)).toHaveCount(0);
});

test("loads an ontology from a card the person runs, and reports it", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 100, loadStepMs: 100 });
  await signIn(page);
  await page.getByRole("button", { name: "Assistant", exact: true }).click();
  await say(page, "Load Customer contracts");
  const dock = dockOf(page);
  const card = dock.getByRole("region", {
    name: "Action: Load v3 of Customer contracts",
  });
  await expect(card).toBeVisible();
  // Named by the person: no warning. The whole request is on view.
  await expect(card.getByText(/Not named by you/)).toHaveCount(0);
  await card.getByText("Full request").click();
  await expect(card.getByText(/"version_id": "ver_seed_3"/)).toBeVisible();
  await card.getByRole("button", { name: "Load", exact: true }).click();
  await expect(card.getByText("Ran", { exact: true })).toBeVisible();
  await expect(
    card.getByRole("button", { name: "Load", exact: true }),
  ).toHaveCount(0);

  // The agent goes on from the result, with a link to the load.
  await expect(dock.getByText(/You ran this action: it started/)).toBeVisible();
  const link = dock.getByRole("link", {
    name: /^\/ontology\/ont_acme_contracts\/loads\/load_/,
  });
  await expect(link).toBeVisible();
  await link.click();
  await expect(page).toHaveURL(/\/ontology\/ont_acme_contracts\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 20_000 });
});

test("dismisses a card it chose itself, and refreshes an expired one", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 100, agentActionTtlMs: 1500 });
  await signIn(page);
  // Off an ontology's pages, nothing names one for it.
  await page.goto("/data");
  await page.keyboard.press("ControlOrMeta+j");
  await say(page, "Load something for me");
  const dock = dockOf(page);
  const card = dock.getByRole("region", { name: /^Action: Load v3/ });
  await expect(card.getByText(/Not named by you/)).toBeVisible();
  // Its confirmation runs out: Refresh prepares it again.
  await expect(card.getByText("Expired", { exact: true })).toBeVisible({
    timeout: 10_000,
  });
  await card.getByRole("button", { name: "Refresh" }).click();
  await expect(
    card.getByRole("button", { name: "Load", exact: true }),
  ).toBeVisible();
  await card.getByRole("button", { name: "Dismiss" }).click();
  await expect(card.getByText("Dismissed", { exact: true })).toBeVisible();
  await expect(
    card.getByRole("button", { name: "Load", exact: true }),
  ).toHaveCount(0);
});

test("a member gets no card to run", async ({ page }) => {
  await mock.configure({ agentStepMs: 100 });
  await signIn(page, "alan@example.com");
  await page.keyboard.press("ControlOrMeta+j");
  await say(page, "Load Customer contracts");
  const dock = dockOf(page);
  await expect(
    dock.getByText(/Only an owner or admin of this workspace can load/),
  ).toBeVisible();
  await expect(dock.getByRole("region", { name: /^Action:/ })).toHaveCount(0);
  await expect(
    dock.getByRole("button", { name: "Load", exact: true }),
  ).toHaveCount(0);
});

test("says when a turn fails", async ({ page }) => {
  await mock.configure({ agentStepMs: 100, agentOutcome: "fail" });
  await signIn(page);
  await page.keyboard.press("ControlOrMeta+j");
  await say(page, "What ontologies do I have?");
  await expect(
    dockOf(page).getByText("The model failed: overloaded."),
  ).toBeVisible();
});

test("sends with Enter, never past what Send refuses, and gives the focus back", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 300 });
  await signIn(page);
  const opener = page.getByRole("button", { name: "Assistant", exact: true });
  await opener.click();
  const dock = dockOf(page),
    box = dock.getByRole("textbox", { name: "Message" });
  // It opens ready to write.
  await expect(box).toBeFocused();
  // Empty: Enter sends nothing.
  await box.press("Enter");
  await expect(dock.getByRole("alert")).toHaveCount(0);
  await box.fill("What ontologies do I have?");
  await box.press("Enter");
  await expect(
    dock
      .getByRole("log", { name: "Conversation" })
      .getByText("What ontologies do I have?", { exact: true }),
  ).toBeVisible();
  // While it answers, Stop stands in for Send, and Enter sends nothing.
  await box.fill("And another?");
  await box.press("Enter");
  await expect(dock.getByRole("alert")).toHaveCount(0);
  await expect(box).toHaveValue("And another?");
  await dock.getByRole("button", { name: "Stop" }).click();
  await expect(dock.getByText("Stopped.")).toBeVisible();
  // Closing the column gives the focus back to what opened it.
  await dock.getByRole("button", { name: "Close the Assistant" }).click();
  await expect(dockOf(page)).toHaveCount(0);
  await expect(opener).toBeFocused();
});

test("is a drawer beside a section's panel on a laptop, and on a phone", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 100 });
  await signIn(page);
  for (const size of [
    { width: 1280, height: 800 },
    { width: 390, height: 844 },
  ]) {
    await page.setViewportSize(size);
    await page.goto("/data");
    await page.keyboard.press("ControlOrMeta+j");
    const drawer = page.getByRole("dialog", { name: "Assistant" });
    await expect(drawer).toBeVisible();
    await expect(dockOf(page)).toHaveCount(0);
    await drawer.getByRole("textbox", { name: "Message" }).fill("Hi");
    await drawer.getByRole("textbox", { name: "Message" }).press("Enter");
    await expect(
      drawer
        .getByRole("log", { name: "Conversation" })
        .getByText("Hi", { exact: true }),
    ).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(drawer).toHaveCount(0);
  }
});

test("deletes a thread, and says when the month's budget ended a turn", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 100, agentOutcome: "budget_exhausted" });
  await signIn(page);
  await page.keyboard.press("ControlOrMeta+j");
  await say(page, "What ontologies do I have?");
  const dock = dockOf(page);
  await expect(
    dock.getByText("It stopped at this message’s spending cap."),
  ).toBeVisible();
  await dock.getByRole("button", { name: /^Threads/ }).click();
  await page.getByRole("menuitem", { name: "Delete this thread" }).click();
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Delete" })
    .click();
  await expect(dock.getByText("Ask the Assistant")).toBeVisible();
  await expect(dock.getByRole("button", { name: /^Threads/ })).toContainText(
    "New thread",
  );
});
