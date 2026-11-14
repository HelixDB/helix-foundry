import type { Page } from "@playwright/test";
import {
  csrfToken,
  expect,
  file,
  loadVersion3,
  makeContractsCurrent,
  mock,
  signIn,
  test,
  uploadInData,
} from "./support/mock";

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

// ⌘J, once the shell (whose Assistant button it presses) is on screen.
async function shortcut(page: Page) {
  await expect(
    page
      .getByRole("button", { name: "Assistant", exact: true })
      .filter({ visible: true })
      .first(),
  ).toBeVisible();
  await page.keyboard.press("ControlOrMeta+j");
}

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

test("answers a question about the page it is opened on", async ({ page }) => {
  await mock.configure({ agentStepMs: 100 });
  await signIn(page);
  await page.goto("/ontology/ont_acme_sales");
  await expect(dockOf(page)).toHaveCount(0);
  await shortcut(page);
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
    name: "Action: Load v3 of “Customer contracts”",
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
  await shortcut(page);
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
  await shortcut(page);
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
  await shortcut(page);
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
    await shortcut(page);
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
  await shortcut(page);
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

// ---------------------------------------------------------------- every operation

const cardOf = (page: Page, title: string) =>
  dockOf(page).getByRole("region", { name: `Action: ${title}` });
/** Runs a card by its button, and waits until it ran. */
async function run(page: Page, title: string, button: string) {
  const card = cardOf(page, title);
  await card.getByRole("button", { name: button, exact: true }).click();
  await expect(card.getByText("Ran", { exact: true })).toBeVisible({
    timeout: 15_000,
  });
  return card;
}

test("builds an ontology card by card: create, draft, make current, load", async ({
  page,
}) => {
  await mock.configure({
    agentStepMs: 100,
    ontologyStepMs: 300,
    loadStepMs: 100,
  });
  await signIn(page);
  await shortcut(page);
  await say(
    page,
    "Create an ontology called Orders graph on database orders, goal: Which customers order the most",
  );
  const created = cardOf(page, "Create the ontology “Orders graph”");
  await expect(created.getByText("orders", { exact: true })).toBeVisible();
  await expect(created.getByText(/Not named by you/)).toHaveCount(0);
  await run(page, "Create the ontology “Orders graph”", "Create");
  // The agent goes on from each result with the next card.
  const draft = cardOf(page, "Draft a new version of “Orders graph”");
  await expect(
    draft.getByText("Which customers order the most", { exact: true }),
  ).toBeVisible();
  await expect(draft.getByText(/^Every connection: /)).toBeVisible();
  await run(page, "Draft a new version of “Orders graph”", "Generate");
  await expect(
    cardOf(page, "Make v1 current for “Orders graph”").getByText("None yet"),
  ).toBeVisible({ timeout: 20_000 });
  await run(page, "Make v1 current for “Orders graph”", "Make current");
  const load = cardOf(page, "Load v1 of “Orders graph”");
  // A load's card keeps its caps.
  await expect(load.getByText(/Reading documents costs at most/)).toBeVisible();
  await run(page, "Load v1 of “Orders graph”", "Load");
  await load.getByRole("link", { name: "Open the load" }).click();
  await expect(page).toHaveURL(/\/ontology\/ont_[^/]+\/loads\/load_/);
});

test("settles a review from a card", async ({ page }) => {
  await mock.configure({ agentStepMs: 100, loadStepMs: 200 });
  await signIn(page);
  await makeContractsCurrent(page);
  await uploadInData(page, [
    file(
      "variant.md",
      "# Variant contract\n\nGlobexx Corporation signed. The Agreement runs for 24 months.\n",
    ),
  ]);
  await loadVersion3(page);
  await shortcut(page);
  await say(page, "Settle the review about Globexx in Customer contracts");
  const card = cardOf(page, "Link “globexx corporation” to Globex Corporation");
  // The document's words, set apart; what the review is and suggests.
  await expect(card.getByText("The document says (its words)")).toBeVisible();
  await expect(card.getByText("variant.md", { exact: true })).toBeVisible();
  await expect(
    card.getByText("Names something that isn’t in your data"),
  ).toBeVisible();
  await expect(
    card.getByText("The name is this entity in every load from the next one."),
  ).toBeVisible();
  // The entity it links to by name, opening its page.
  await expect(
    card.getByRole("link", { name: "Globex Corporation" }).first(),
  ).toHaveAttribute(
    "href",
    `/explore/ont_acme_contracts/entities/${Buffer.from("Customer\0k:cus_GLOBEX01").toString("base64url")}`,
  );
  await run(page, "Link “globexx corporation” to Globex Corporation", "Link");
  await expect(
    dockOf(page).getByText(
      /The decision is saved\. It applies from the next load/,
    ),
  ).toBeVisible();
  await card.getByRole("link", { name: "Open the document" }).click();
  await expect(page).toHaveURL(/\/ontology\/ont_acme_contracts\/documents\//);
});

test("opens the Add data form from a connect link", async ({ page }) => {
  await mock.configure({ agentStepMs: 100 });
  await signIn(page);
  await shortcut(page);
  await say(page, "Connect Slack");
  const dock = dockOf(page);
  await expect(
    dock.getByText(/Connect Slack from the link above/),
  ).toBeVisible();
  // The tool's link, as a link into this app: never the address it named.
  await dock.getByRole("link", { name: "Connect Slack" }).click();
  await expect(page).toHaveURL(/\/data\?add=slack$/);
  await expect(page.getByRole("dialog", { name: "Slack" })).toBeVisible();
});

test("a member gets no card for any operation", async ({ page }) => {
  await mock.configure({ agentStepMs: 100 });
  await signIn(page, "alan@example.com");
  await shortcut(page);
  await say(page, "Sync Stripe");
  const dock = dockOf(page);
  await expect(
    dock.getByText(
      "Only an owner or admin of this workspace can sync a connection, so I can't prepare that for you.",
    ),
  ).toBeVisible();
  await expect(dock.getByRole("region", { name: /^Action:/ })).toHaveCount(0);
});

test("refreshes an expired card of another operation, then runs it", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 100, agentActionTtlMs: 1500 });
  await signIn(page);
  await shortcut(page);
  await say(page, "Sync Stripe");
  const card = cardOf(page, "Sync “Stripe”");
  await expect(card.getByText("Expired", { exact: true })).toBeVisible({
    timeout: 10_000,
  });
  await card.getByRole("button", { name: "Refresh" }).click();
  await card.getByRole("button", { name: "Sync", exact: true }).click();
  await expect(card.getByText("Ran", { exact: true })).toBeVisible();
  await expect(
    dockOf(page).getByRole("link", {
      name: "/data/connections/conn_acme_stripe",
    }),
  ).toBeVisible();
});

test("shows a call a limit kept from running as not run", async ({ page }) => {
  await mock.configure({ agentStepMs: 100, agentOutcome: "not_run" });
  await signIn(page);
  await shortcut(page);
  await say(page, "What ontologies do I have?");
  const step = dockOf(page).getByRole("group").filter({ hasText: "Not run" });
  await expect(step).toBeVisible();
  await step.getByText("Not run").click();
  await expect(
    step.getByText(
      "One reply runs at most 8 tool calls, and this was past them.",
    ),
  ).toBeVisible();
});

test("shows its answer as the model writes it, once, after a reload mid-answer too", async ({
  page,
}) => {
  // A text streams about four words a delta, 1.2 s apart.
  await mock.configure({ agentStepMs: 200, agentDeltaMs: 1200 });
  await signIn(page);
  await shortcut(page);
  await say(page, "What ontologies do I have?");
  const writing = () =>
    dockOf(page).getByRole("log", { name: "Conversation" }).locator(".ag-live");
  await expect(writing()).toHaveText(/^This workspace has/);
  await expect(writing()).not.toContainText("or to load one.");
  // Reloaded mid-answer, it shows what was written so far and goes on.
  await page.reload();
  await expect(writing()).toBeVisible();
  await expect(writing()).toHaveCount(0, { timeout: 15_000 });
  // The text replaces what was written: shown once.
  await expect(
    dockOf(page).getByText(
      /^This workspace has \d+ ontologies: .*Ask me about one of them, or to load one\.$/,
    ),
  ).toHaveCount(1);
  await expect(dockOf(page).getByText("Working…")).toHaveCount(0);
});

test("answers on another provider once it is the setting", async ({ page }) => {
  await mock.configure({ agentStepMs: 100, aiProbeMs: 600 });
  await signIn(page);
  await page.goto("/settings/ai");
  await page.getByRole("radio", { name: "OpenAI", exact: true }).check();
  await page.getByLabel("API key").fill("sk-proj-e2e-0123456789abcdefwxyz");
  await page.getByRole("button", { name: "Test and save" }).click();
  await page
    .getByRole("dialog", { name: "Change where AI runs?" })
    .getByRole("button", { name: "Test and save" })
    .click();
  await expect(page.getByText("AI models now run on OpenAI.")).toBeVisible();
  // Its tool check passed: nothing to say.
  await expect(page.getByText(/The Assistant can’t run/)).toHaveCount(0);
  await shortcut(page);
  await expect(
    dockOf(page).getByText(
      "Enter to send, Shift+Enter for a new line. Nothing runs until you click a card’s button.",
    ),
  ).toBeVisible();
  await say(page, "What ontologies do I have?");
  await expect(
    dockOf(page).getByText(/^This workspace has \d+ ontologies: /),
  ).toBeVisible();
});

test("won't send on a model that doesn't call tools, and says why, with a link to the setting", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 100, aiProbeMs: 600 });
  await signIn(page);
  await page.goto("/settings/ai");
  await page
    .getByRole("radio", { name: "Custom (OpenAI-compatible)", exact: true })
    .check();
  await page.getByLabel("Base URL").fill("https://llm.example.com/v1");
  await page.getByLabel("Model name").fill("qwen-notoolcall");
  await page.getByRole("button", { name: "Test and save" }).click();
  await page
    .getByRole("dialog", { name: "Change where AI runs?" })
    .getByRole("button", { name: "Test and save" })
    .click();
  await expect(
    page.getByText("AI models now run on llm.example.com."),
  ).toBeVisible();
  // The setting runs everything else; its owner is told the Assistant can't.
  const why =
    "The Assistant can’t run on qwen-notoolcall: it answered the Assistant’s test without calling a tool. Drafts, loads and the Analyst still run on it. Choose a model for questions that calls tools to use the Assistant.";
  await expect(
    page.getByRole("note").filter({ hasText: "The Assistant can’t run" }),
  ).toHaveText(why);
  // The dock says so before a message is spent on a turn that would fail.
  await page.goto("/data");
  await shortcut(page);
  const dock = dockOf(page),
    box = dock.getByRole("textbox", { name: "Message" });
  await expect(dock.locator(".ag-hint")).toHaveText(
    `${why} Open Settings › AI models.`,
  );
  await box.fill("What ontologies do I have?");
  await expect(
    dock.getByRole("button", { name: "Send", exact: true }),
  ).toBeDisabled();
  await box.press("Enter");
  await expect(box).toHaveValue("What ontologies do I have?");
  await expect(
    dock
      .getByRole("log", { name: "Conversation" })
      .getByText("What ontologies do I have?"),
  ).toHaveCount(0);
  // On a phone the dock is a drawer, which the link closes as it opens the setting.
  await page.setViewportSize({ width: 390, height: 844 });
  await shortcut(page);
  const drawer = page.getByRole("dialog", { name: "Assistant" });
  await expect(drawer).toBeVisible();
  await drawer.getByRole("link", { name: "Settings › AI models" }).click();
  await expect(page).toHaveURL(/\/settings\/ai$/);
  await expect(drawer).toHaveCount(0);

  // A member reads where AI runs, without the owner's notice, and the dock
  // says only that an owner can fix it.
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto("/settings/ai");
  await expect(page.getByText(/^AI runs on llm\.example\.com/)).toBeVisible();
  await expect(
    page.getByRole("note").filter({ hasText: "The Assistant can’t run" }),
  ).toHaveCount(0);
  // The dock is still open in this browser.
  await expect(dockOf(page).locator(".ag-hint")).toHaveText(
    "The Assistant can’t run on this workspace’s AI model. An owner can fix this in Settings › AI models.",
  );
  await dockOf(page)
    .getByRole("textbox", { name: "Message" })
    .fill("What ontologies do I have?");
  await expect(
    dockOf(page).getByRole("button", { name: "Send", exact: true }),
  ).toBeDisabled();
});

test("a turn sent before the setting failed its check says why, and its link closes a phone's drawer", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 100, aiProbeMs: 300 });
  await signIn(page);
  await page.setViewportSize({ width: 390, height: 844 });
  // On the settings page itself, so following the link changes no route:
  // the link closes the drawer, not the route change.
  await page.goto("/settings/ai");
  await shortcut(page);
  const drawer = page.getByRole("dialog", { name: "Assistant" });
  await expect(drawer).toBeVisible();
  // Another tab changes the setting while the dock is open, to a key the
  // Assistant's calls are refused on.
  const res = await page.request.put("/papi/v1/workspaces/ws_acme/foundry/ai", {
    data: {
      provider: { openai: {} },
      apiKey: "sk-proj-agentauth-0123456789abcdef",
    },
    headers: {
      origin: new URL(page.url()).origin,
      "x-helix-csrf": await csrfToken(page),
    },
  });
  expect(res.status(), await res.text()).toBe(200);
  await expect
    .poll(async () => {
      const r = await page.request.get(
        "/papi/v1/workspaces/ws_acme/foundry/ai",
      );
      return (await r.json()).ai?.active?.assistantCheck;
    })
    .toBe("FOUNDRY_AI_ASSISTANT_CHECK_AUTH");
  await drawer
    .getByRole("textbox", { name: "Message" })
    .fill("What ontologies do I have?");
  await drawer.getByRole("button", { name: "Send", exact: true }).click();
  const failure = drawer.locator(".ag-card-error");
  await expect(failure).toHaveText(
    "OpenAI refused the workspace's key for the Assistant. An owner can check it in Settings › AI models.",
  );
  await failure.getByRole("link", { name: "Settings › AI models" }).click();
  await expect(page).toHaveURL(/\/settings\/ai$/);
  await expect(drawer).toHaveCount(0);
});
