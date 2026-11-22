import type { Page } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { expect, mock, postToWfe, signIn, test } from "./support/mock";

// The Slack data connector on Helix's Slack app, against the mock
// (mock/routes/data.ts, slack.ts): a connection made from Add data, adding
// the app to Slack on the way and back; its history's backfill on its
// page, until it is live, with no Sync; a token connection's staged switch;
// what an install lacks (permissions, a Slack admin's install) on the
// form, the page and Settings › Slack, with Reinstall coming back to the
// page; Slack holding back events and days that may miss messages; the
// app removed; what members see; and a Founder thread started in Slack
// tagged in the threads.

const dialog = (page: Page) => page.locator("dialog[open]");
const header = (page: Page) => page.locator(".hf-page-header");
const settingsPanel = (page: Page, title: string) =>
  page
    .locator(".hf-panel")
    .filter({ has: page.getByRole("heading", { name: title, exact: true }) });
const TOKEN = "xoxb-1234-5678-e2eSlackTokenWXYZ";
// A sync runs about 2.5 s in the mock.
const SYNCED = { timeout: 15_000 };

/** Mock Slack's page: Allow, back to wherever the install was started. */
async function allowOnSlack(page: Page) {
  await page.waitForURL(/\/mock\/slack\/authorize\?/);
  await page.getByRole("button", { name: "Allow" }).click();
}

/** Ada adds Helix Foundry to Acme's Slack from Settings › Slack. */
async function addToSlack(page: Page) {
  await page.goto("/settings/slack");
  await page.getByRole("button", { name: "Add to Slack" }).click();
  await allowOnSlack(page);
  await expect(page).toHaveURL(/\/settings\/slack$/);
  await expect(settingsPanel(page, "Slack")).toContainText("Acme Slack");
}

/** A Slack connection through the app, made as its form makes it: its id. */
async function connectThroughApp(page: Page): Promise<string> {
  return (
    await postToWfe(page, "/v1/workspaces/ws_acme/foundry/connectors", {
      name: "Acme Slack",
      config: { slack: { historyDays: 30, helixApp: { noticeVersion: 1 } } },
    })
  ).connector.id;
}

test("connects Slack through Helix's app from Add data, adding the app to Slack on the way", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data?add=slack");
  const form = dialog(page);
  await expect(form).toHaveAttribute("aria-label", "Slack");
  // The app is the default; signing in isn't offered beside it.
  await expect(
    form.getByRole("radio", { name: "Use the Helix Slack app" }),
  ).toHaveAttribute("aria-checked", "true");
  await expect(form.getByRole("button", { name: /Connect with/ })).toHaveCount(
    0,
  );
  await expect(form.locator(".connection-guide")).toContainText(
    "Slack delivers at most 30,000 messages an hour per workspace",
  );
  await expect(form.getByLabel("Bot token")).toHaveCount(0);
  await form
    .getByRole("button", { name: "Add Helix Foundry to Slack" })
    .click();
  await allowOnSlack(page);
  // Back on the form it was added from.
  await expect(page).toHaveURL(/\/data\?add=slack/);
  await expect(dialog(page)).toContainText(
    "Reads Acme Slack through Helix Foundry",
  );
  const days = dialog(page).getByLabel("Days of history", { exact: true });
  await expect(days).toHaveValue("90");
  await expect(days).toHaveAttribute("max", "90");
  const connect = dialog(page).getByRole("button", {
    name: "Connect",
    exact: true,
  });
  // The storage notice comes first.
  await expect(connect).toBeDisabled();
  const notice = dialog(page).getByRole("checkbox", {
    name: /I’m allowed to have Helix keep this copy of Acme Slack’s channels/,
  });
  await expect(dialog(page)).toContainText("deleted 7 days later");
  await notice.check();
  await days.fill("30");
  await connect.click();
  await expect(dialog(page)).toContainText(
    "Connected. Helix Foundry is reading the channels’ history",
  );
  await dialog(page)
    .getByRole("button", { name: /^Open / })
    .click();

  await expect(
    page.getByRole("heading", { level: 1, name: "Slack" }),
  ).toBeVisible();
  // Pushed: Slack sends its messages, so there is no Sync.
  await expect(header(page)).toContainText(
    /Reading history: \d of 4 channels, about \d+ (days|hours) left/,
  );
  await expect(page.getByRole("button", { name: "Sync now" })).toHaveCount(0);
  await expect(page.locator(".hf-kv")).toContainText(
    "Helix Foundry’s Slack app in Acme Slack",
  );
  await expect(page.locator(".hf-kv")).toContainText("30 days");
  // The Data explorer says so too.
  await expect(
    page
      .getByRole("tree")
      .getByRole("treeitem", { name: "Slack, Reading history" }),
  ).toBeVisible();

  // Its history read, it is live.
  await mock.configure({ slackBackfillMs: 1 });
  await page.reload();
  await expect(header(page)).toContainText("Live");
  // A message Slack sends syncs: today's day of #general is a document more.
  const conversations = page
    .getByRole("region", { name: "Documents" })
    .getByRole("listitem", { name: "Conversations" });
  await expect(conversations).toContainText(/\d+ documents/, SYNCED);
  const held = Number(
    /(\d+) documents/.exec((await conversations.textContent()) || "")![1],
  );
  await mock.slackFeed({ channel: "general", text: "Lunch at noon" });
  await expect(async () => {
    await page.reload();
    await expect(conversations).toContainText(`${held + 1} documents`, {
      timeout: 1_000,
    });
  }).toPass(SYNCED);

  // Settings › Slack says which connection reads it.
  await page.goto("/settings/slack");
  await settingsPanel(page, "Slack")
    .getByRole("link", { name: "a Slack connection" })
    .click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Slack" }),
  ).toBeVisible();
});

test("switches a token's connection to the app in stages", async ({ page }) => {
  await signIn(page);
  await addToSlack(page);
  const id = (
    await postToWfe(page, "/v1/workspaces/ws_acme/foundry/connectors", {
      name: "Team Slack",
      config: { slack: { botToken: TOKEN, historyDays: 200 } },
    })
  ).connector.id;
  await postToWfe(
    page,
    `/v1/workspaces/ws_acme/foundry/connectors/${id}:sync`,
    {},
  );
  await page.goto(`/data/connections/${id}`);
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  await page.getByRole("button", { name: "Connection actions" }).click();
  await page.getByRole("menuitem", { name: "Edit connection" }).click();
  const form = dialog(page);
  await expect(
    form.getByRole("radio", { name: "Use your own app’s token" }),
  ).toHaveAttribute("aria-checked", "true");
  await form.getByRole("radio", { name: "Use the Helix Slack app" }).click();
  // The app keeps at most 90 days.
  await expect(form.getByLabel("Days of history", { exact: true })).toHaveValue(
    "90",
  );
  await expect(form).toContainText(
    "Your token keeps syncing until Helix Foundry has read the history.",
  );
  const save = form.getByRole("button", { name: "Save changes" });
  await expect(save).toBeDisabled();
  await form.getByRole("checkbox", { name: /I’m allowed/ }).check();
  await save.click();
  await expect(
    page.getByText(/^Switching to Helix Foundry’s Slack app/),
  ).toBeVisible();
  await expect(header(page)).toContainText(
    "Switching from your token: 0 of 4 channels read",
  );
  // Still its token's: it syncs by it.
  await expect(page.getByRole("button", { name: "Sync now" })).toBeVisible();
  await expect(page.locator(".hf-kv")).toContainText("xoxb…WXYZ");

  await mock.configure({ slackBackfillMs: 1 });
  await page.reload();
  await expect(header(page)).toContainText("Live");
  await expect(page.getByRole("button", { name: "Sync now" })).toHaveCount(0);
  await expect(page.locator(".hf-kv")).toContainText(
    "Helix Foundry’s Slack app in Acme Slack",
  );
});

test("says what the install lacks, and adds it again back to the connection", async ({
  page,
}) => {
  await signIn(page);
  await addToSlack(page);
  // Made before the feed asked for history: it lacks its scopes.
  await mock.configure({ slackScopesOld: true });
  await page.goto("/data?add=slack");
  await expect(dialog(page)).toContainText(
    "Helix Foundry needs new Slack permissions to sync channels.",
  );
  await expect(
    dialog(page).getByRole("button", { name: "Reinstall" }),
  ).toBeVisible();
  await dialog(page).getByRole("button", { name: "Close" }).click();
  await page.goto("/settings/slack");
  const slack = settingsPanel(page, "Slack");
  await expect(slack).toContainText("Needs new permissions");
  await expect(slack).toContainText("channels:history, groups:history");
  await expect(slack.getByRole("button", { name: "Reinstall" })).toBeVisible();

  // Made by someone who isn't a Slack admin.
  await mock.configure({ slackScopesOld: false, slackInstallerMember: true });
  await page.reload();
  await expect(slack).toContainText(
    "Installed by a non-admin: syncing its channels needs a Slack workspace admin or owner to add it.",
  );
  await page.goto("/data?add=slack");
  await expect(dialog(page)).toContainText(
    "A Slack workspace admin or owner must add Helix Foundry to Slack before it can sync channels.",
  );

  // A connection reading it: Reinstall comes back to its page.
  await mock.configure({ slackInstallerMember: false });
  const id = await connectThroughApp(page);
  await mock.configure({ slackScopesOld: true });
  await page.goto(`/data/connections/${id}`);
  const callout = page.getByRole("alert").filter({
    hasText: "Helix Foundry needs new Slack permissions to sync channels",
  });
  await expect(callout).toContainText("channels:history, groups:history");
  await expect(header(page)).toContainText("Needs new Slack permissions");
  await callout.getByRole("button", { name: "Reinstall" }).click();
  await page.waitForURL(/\/mock\/slack\/authorize\?/);
  await mock.configure({ slackScopesOld: false });
  await page.getByRole("button", { name: "Allow" }).click();
  await expect(page).toHaveURL(new RegExp(`/data/connections/${id}`));
  await expect(callout).toHaveCount(0);
  await expect(header(page)).toContainText("Reading history");
});

test("says when Slack held back events, days may miss messages, and the app was removed", async ({
  page,
}) => {
  await signIn(page);
  await addToSlack(page);
  const id = await connectThroughApp(page);
  await mock.configure({ slackEventsLimited: true, slackLossyDays: 2 });
  await page.goto(`/data/connections/${id}`);
  await expect(
    page.getByText(
      "Slack held back some of Acme Slack’s messages in the last day",
    ),
  ).toBeVisible();
  await expect(page.getByText("2 days may be missing messages")).toBeVisible();
  await page.goto("/settings/slack");
  await expect(settingsPanel(page, "Slack")).toContainText(
    "Slack held back some of Acme Slack’s events",
  );

  // Disconnected: its data is frozen, then deleted.
  await settingsPanel(page, "Slack")
    .getByRole("button", { name: "Disconnect" })
    .click();
  await page
    .getByRole("dialog", { name: "Disconnect Acme Slack?" })
    .getByRole("button", { name: "Disconnect" })
    .click();
  await expect(
    page.getByRole("button", { name: "Add to Slack" }),
  ).toBeVisible();
  await page.goto(`/data/connections/${id}`);
  const gone = page.getByRole("alert").filter({
    hasText: "Its data is frozen as of its last sync",
  });
  await expect(gone).toContainText("Helix Foundry was removed from Acme Slack");

  await expect(gone).toContainText(/is deleted on /);
  await expect(
    gone.getByRole("link", { name: "Settings › Slack" }),
  ).toBeVisible();
  await expect(header(page)).toContainText("Removed from Slack");
});

test("members see a connection through the app without its actions", async ({
  page,
}) => {
  await signIn(page);
  await addToSlack(page);
  const id = await connectThroughApp(page);
  await mock.configure({ slackScopesOld: true });
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto(`/data/connections/${id}`);
  await expect(header(page)).toContainText("Needs new Slack permissions");
  const callout = page.getByRole("alert").filter({
    hasText: "Helix Foundry needs new Slack permissions",
  });
  await expect(callout).toContainText(
    "An owner or admin adds it to Slack again from Settings › Slack.",
  );
  await expect(callout.getByRole("button")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Connection actions" }),
  ).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Sync now" })).toHaveCount(0);
});

test("tags a Founder thread started in Slack", async ({ page }) => {
  await signIn(page);
  await postToWfe(page, "/v1/workspaces/ws_acme/foundry/agent/turns", {
    text: "What ontologies do I have?",
    clientToken: randomUUID(),
    surface: "FOUNDRY_AGENT_SURFACE_SLACK",
  });
  await postToWfe(page, "/v1/workspaces/ws_acme/foundry/agent/turns", {
    text: "Which loads are current?",
    clientToken: randomUUID(),
  });
  await page.goto("/ask");
  const threads = page.getByRole("navigation", { name: "Threads" });
  await expect(
    threads.getByRole("link", { name: /^What ontologies do I have\?/ }),
  ).toContainText("Slack");
  await expect(
    threads.getByRole("link", { name: /^Which loads are current\?/ }),
  ).not.toContainText("Slack");
});
