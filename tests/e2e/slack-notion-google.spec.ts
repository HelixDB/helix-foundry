import type { Locator, Page } from "@playwright/test";
import { dataTree, expect, signIn, test } from "./support/mock";

// Slack, Notion and Google Workspace, each connected end to end: its
// guide and form, the WFE's refusals and what the credential can't read,
// then its tables, its first sync and the set of documents it writes,
// in the Data explorer under the connection. The mock's Slack reads a
// document per channel and day, Notion one per page and Google one per
// Doc. A credential typed into a form never comes back from the BFF.

// A sync runs about 2.5 s in the mock, and the page polls every 1.5 s.
const SYNCED = { timeout: 15_000 };

const dialog = (page: Page) => page.locator("dialog[open]");
const header = (page: Page) => page.locator(".hf-page-header");
const folder = (page: Page, name: string) =>
  dataTree(page).getByRole("treeitem", { name, exact: true });
// A connection's row: its name says its sync's status too.
const connection = (page: Page, name: string) =>
  dataTree(page).getByRole("treeitem", { name: `${name}, Synced` });
/** One of the connection page's sets. */
const set = (page: Page, name: string) =>
  page
    .getByRole("region", { name: "Documents" })
    .getByRole("listitem", { name });
/** A row of the connection page's Details. */
const detail = (page: Page, label: string) =>
  page
    .locator(".hf-kv-row")
    .filter({ has: page.locator("dt", { hasText: new RegExp(`^${label}$`) }) })
    .locator("dd");
// The objects a test lists, not what it can't read below them.
const listed = (objects: Locator) =>
  objects.locator(".table-selection > [role=listitem]");
const runTest = (page: Page) =>
  dialog(page).getByRole("button", { name: "Test connection" }).click();

// Every /papi response body the page received, to search for credentials.
function recordResponses(page: Page) {
  const bodies: string[] = [];
  page.on("response", async (r) => {
    if (new URL(r.url()).pathname.startsWith("/papi/"))
      bodies.push(await r.text().catch(() => ""));
  });
  return bodies;
}

async function openPicker(page: Page, kind: string) {
  await page.goto("/data");
  await page.getByRole("button", { name: "Add data" }).first().click();
  await dialog(page).getByRole("button", { name: kind, exact: true }).click();
  await expect(dialog(page)).toHaveAttribute("aria-label", kind);
}

/**
 * The connection's row in the Data explorer, opened: its tables, then its
 * set, which opens on its newest 50 documents.
 */
async function openInTree(
  page: Page,
  name: string,
  tables: number,
  setName: string,
) {
  const row = connection(page, name);
  await expect(row).toBeVisible(SYNCED);
  await row.locator(":scope > .dt-row .dt-toggle").click();
  await expect(row).toHaveAttribute("aria-expanded", "true");
  const rows = row.locator(":scope > [role=group] > [role=treeitem]");
  await expect(rows).toHaveCount(tables + 1);
  await expect(rows.last()).toHaveAccessibleName(setName);
  const own = folder(page, setName);
  await own.locator(":scope > .dt-row").click();
  await expect(own).toHaveAttribute("aria-expanded", "true");
  return own;
}

test("connects Slack, says which scope its token lacks, and keeps a document per channel and day", async ({
  page,
}) => {
  const token = "xoxb-1234-5678-e2eSlackTokenWXYZ";
  const bodies = recordResponses(page);
  await signIn(page);
  await openPicker(page, "Slack");
  const guide = dialog(page).locator(".connection-guide");
  await expect(
    guide.getByRole("link", { name: "your apps in Slack" }),
  ).toHaveAttribute("href", "https://api.slack.com/apps");
  // Each kind of channel's scopes, as the kind reads them.
  await expect(guide).toContainText(
    "channels:read and channels:history for public channels, groups:read and groups:history for private ones",
  );
  await expect(guide).toContainText("/invite @Helix Foundry");
  await expect(guide).toContainText("Direct messages are never read.");
  const tokenField = dialog(page).getByLabel("Bot token");
  await expect(tokenField).toHaveAttribute("placeholder", "xoxb-…");
  await expect(dialog(page).getByLabel("Days of history")).toHaveValue("90");

  // A user token is refused as the WFE refuses it, and a revoked one by
  // Slack.
  await tokenField.fill("xoxp-1234-user");
  await runTest(page);
  await expect(dialog(page).getByRole("alert")).toHaveText(
    "enter a Slack bot token (xoxb-…)",
  );
  await tokenField.fill("xoxb-1234-bad");
  await runTest(page);
  await expect(dialog(page).getByRole("alert")).toHaveText(
    "Slack rejected this token; check it and enter it again",
  );
  // Without channels:history, the history's tables say what to add; the
  // members need only the channel lists.
  await tokenField.fill("xoxb-1234-noscope");
  await runTest(page);
  const objects = dialog(page).getByRole("group", {
    name: "Slack objects this key can read",
  });
  await expect(listed(objects)).toHaveCount(3);
  await expect(listed(objects).last()).toContainText("channel_members");
  const unreadable = dialog(page).locator(".dc-unavailable li");
  await expect(unreadable).toHaveText(
    ["messages", "channel_days"].map(
      (t) => `${t}: add the channels:history scope to the app and reinstall it`,
    ),
  );
  // An app in no channel is told how to invite it.
  await tokenField.fill("xoxb-1234-lonely");
  await runTest(page);
  await expect(dialog(page).getByRole("list", { name: "Warnings" })).toHaveText(
    "The app is in no channels yet: invite it to each channel to sync with /invite @Foundry, then test again.",
  );

  await tokenField.fill(token);
  await dialog(page).getByLabel("Days of history").fill("30");
  await runTest(page);
  await expect(listed(objects)).toHaveCount(5);
  await expect(objects).not.toContainText("cannot read");
  await expect(
    dialog(page).getByRole("list", { name: "Warnings" }),
  ).toHaveCount(0);
  await dialog(page).getByRole("button", { name: "Connect and sync" }).click();

  await expect(page).toHaveURL(/\/data\/connections\/conn_/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Slack" }),
  ).toBeVisible();
  await expect(header(page)).toContainText("xoxb…WXYZ");
  await expect(detail(page, "Message history")).toHaveText("30 days");
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  await expect(
    page.getByRole("list", { name: "Synced tables" }).getByRole("listitem"),
  ).toHaveCount(5);
  await expect(set(page, "Conversations")).toContainText(/\d+ documents/);

  const conversations = await openInTree(page, "Slack", 5, "Conversations");
  const documents = conversations.getByRole("link");
  await expect(documents).toHaveCount(50);
  await expect(documents.first()).toHaveText(
    /^#(general|sales|support|engineering), \d{4}-\d{2}-\d{2}$/,
  );
  // A channel day's page: its messages, from Slack.
  await documents.first().click();
  await expect(page).toHaveURL(/\/data\/documents\/[^/]+$/);
  await expect(page.locator(".hf-page-header-meta")).toContainText(
    "From Slack · Conversations",
  );
  await expect(
    page.getByRole("link", { name: "Open in Slack" }),
  ).toHaveAttribute(
    "href",
    /^https:\/\/app\.slack\.com\/client\/T0MOCKACME\/C0MOCK\w+$/,
  );
  await expect(page.getByRole("main")).toContainText(/Channel: #\w+ \(C0MOCK/);
  for (const body of bodies) expect(body).not.toContain(token);
  expect(await page.content()).not.toContain(token);
});

test("connects Notion, shows its databases' first rows, and keeps its pages as documents", async ({
  page,
}) => {
  const secret = "ntn_e2eNotionSecret0123456789QRST";
  const bodies = recordResponses(page);
  await signIn(page);
  // The Helix MCP's connector link opens Notion's form.
  await page.goto("/data?add=notion");
  await expect(dialog(page)).toHaveAttribute("aria-label", "Notion");
  const guide = dialog(page).locator(".connection-guide");
  await expect(
    guide.getByRole("link", { name: "Integrations in Notion" }),
  ).toHaveAttribute("href", "https://www.notion.so/profile/integrations");
  await expect(guide).toContainText(
    "Read user information including email addresses",
  );
  const secretField = dialog(page).getByLabel("Integration secret");
  await expect(secretField).toHaveAttribute("placeholder", "ntn_…");
  await secretField.fill("ntn-not-a-secret");
  await runTest(page);
  await expect(dialog(page).getByRole("alert")).toHaveText(
    "enter a Notion integration secret (ntn_…)",
  );
  await secretField.fill("ntn_badSecret");
  await runTest(page);
  await expect(dialog(page).getByRole("alert")).toHaveText(
    "Notion rejected this secret; check it and enter it again",
  );
  await secretField.fill("ntn_emptySecret");
  await runTest(page);
  await expect(dialog(page).getByRole("alert")).toHaveText(
    "nothing is shared with this integration yet: in Notion, add it to a page or database from its ••• menu › Connections, then test again",
  );
  const objects = dialog(page).getByRole("group", {
    name: "Notion objects this key can read",
  });
  await secretField.fill("ntn_nousersSecret");
  await runTest(page);
  await expect(listed(objects)).toHaveCount(3);
  await expect(dialog(page).locator(".dc-unavailable li")).toHaveText([
    "users: turn on Read user information in the integration's capabilities",
  ]);

  await secretField.fill(secret);
  await runTest(page);
  await expect(listed(objects)).toHaveCount(4);
  await expect(dialog(page).locator(".dc-unavailable")).toHaveCount(0);
  // A shared database is a table of its own: its columns and first rows.
  const accounts = listed(objects).filter({ hasText: "customer_accounts" });
  await accounts.getByText("Columns and preview").click();
  await expect(
    accounts.getByRole("list", { name: "customer_accounts columns" }),
  ).toContainText("renewal_date");
  const preview = accounts.getByRole("table", {
    name: "customer_accounts preview",
  });
  await expect(preview.locator("tbody tr")).toHaveCount(5);
  await expect(preview.locator("tbody tr").first()).toContainText("Globex");
  await dialog(page).getByRole("button", { name: "Connect and sync" }).click();

  await expect(
    page.getByRole("heading", { level: 1, name: "Notion" }),
  ).toBeVisible();
  await expect(header(page)).toContainText("ntn…QRST");
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  await expect(
    page.getByRole("list", { name: "Synced tables" }).getByRole("listitem"),
  ).toHaveCount(4);
  await expect(set(page, "Pages")).toContainText("60 documents");

  const pages = await openInTree(page, "Notion", 4, "Pages");
  await expect(pages.locator(":scope > .dt-row .dt-count")).toHaveText("60");
  const documents = pages.getByRole("link");
  await expect(documents).toHaveCount(50);
  await expect(documents.first()).toHaveText("Onboarding playbook");
  await pages.getByRole("button", { name: "Show more" }).click();
  await expect(documents).toHaveCount(60);
  await documents.first().click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Onboarding playbook" }),
  ).toBeVisible();
  await expect(
    page.getByRole("link", { name: "Open in Notion" }),
  ).toHaveAttribute(
    "href",
    /^https:\/\/www\.notion\.so\/Onboarding-playbook-[0-9a-f]{32}$/,
  );
  for (const body of bodies) expect(body).not.toContain(secret);
  expect(await page.content()).not.toContain(secret);
});

test("connects Google Workspace, keeps its stored key on an edit, and keeps its Docs as documents", async ({
  page,
}) => {
  const key = (email = "foundry@acme-data.iam.gserviceaccount.com") =>
    JSON.stringify({
      type: "service_account",
      project_id: "acme-data",
      private_key_id: "0123456789abcdef9f3a",
      private_key:
        "-----BEGIN PRIVATE KEY-----\nMIIEe2eGoogleKey\n-----END PRIVATE KEY-----\n",
      client_email: email,
    });
  const bodies = recordResponses(page),
    patches: string[] = [];
  page.on("request", (r) => {
    if (r.method() === "PATCH" && /\/connectors\/conn_\w+$/.test(r.url()))
      patches.push(r.postData() || "");
  });
  await signIn(page);
  await openPicker(page, "Google Workspace");
  const guide = dialog(page).locator(".connection-guide");
  await expect(
    guide.getByRole("link", { name: "Admin console" }),
  ).toHaveAttribute("href", "https://admin.google.com");
  await expect(guide.locator("pre")).toHaveText(
    "https://www.googleapis.com/auth/drive.readonly,https://www.googleapis.com/auth/calendar.readonly,https://www.googleapis.com/auth/admin.directory.user.readonly",
  );
  const keyField = dialog(page).getByLabel("Service account JSON key"),
    subject = dialog(page).getByLabel("Acts as");
  await expect(dialog(page).getByLabel("Days of past events")).toHaveValue(
    "90",
  );
  await keyField.fill('{"type": "authorized_user"}');
  await subject.fill("ada@example.com");
  await runTest(page);
  await expect(dialog(page).getByRole("alert")).toHaveText(
    "paste the service account's JSON key",
  );
  await keyField.fill(key());
  await subject.fill("nobody@example.com");
  await runTest(page);
  await expect(dialog(page).getByRole("alert")).toHaveText(
    "Google has no user nobody@example.com, or the service account may not act as them; check the email",
  );
  // A member can't read the Directory, and a key without Calendar's scope
  // its calendars.
  await keyField.fill(key("nocalendar@acme-data.iam.gserviceaccount.com"));
  await subject.fill("alan.member@example.com");
  await runTest(page);
  const objects = dialog(page).getByRole("group", {
    name: "Google Workspace objects this key can read",
  });
  await expect(listed(objects)).toHaveCount(1);
  await expect(dialog(page).locator(".dc-unavailable li")).toHaveText([
    "users: alan.member@example.com is not an admin, so the Directory's users can't be read",
    ...["calendars", "calendar_events", "event_attendees"].map(
      (t) =>
        `${t}: allow the scope https://www.googleapis.com/auth/calendar.readonly for the service account's client ID in the Admin console's domain-wide delegation`,
    ),
  ]);

  await keyField.fill(key());
  await subject.fill("ada@example.com");
  await runTest(page);
  await expect(listed(objects)).toHaveCount(5);
  await dialog(page).getByRole("button", { name: "Connect and sync" }).click();

  await expect(
    page.getByRole("heading", { level: 1, name: "Google Workspace" }),
  ).toBeVisible();
  const hint = "foundry@acme-data.iam.gserviceaccount.com…9f3a";
  await expect(header(page)).toContainText(hint);
  await expect(detail(page, "Acts as")).toHaveText("ada@example.com");
  await expect(detail(page, "Event history")).toHaveText("90 days");
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  await expect(
    page.getByRole("list", { name: "Synced tables" }).getByRole("listitem"),
  ).toHaveCount(5);
  await expect(set(page, "Docs")).toContainText("14 documents");

  // An edit starts with the key blank and keeps the stored one when it is
  // left blank.
  await page.getByRole("button", { name: "Connection actions" }).click();
  await page.getByRole("menuitem", { name: "Edit connection" }).click();
  const edit = page.getByRole("dialog", { name: "Edit Google Workspace" });
  await expect(edit.getByLabel("Service account JSON key")).toHaveValue("");
  await expect(edit.getByLabel("Service account JSON key")).toHaveAttribute(
    "placeholder",
    `Leave blank to keep ${hint}`,
  );
  await expect(edit.getByLabel("Acts as")).toHaveValue("ada@example.com");
  await edit.getByLabel("Days of past events").fill("30");
  await edit.getByRole("button", { name: "Save changes" }).click();
  await expect(page.locator(".hf-toast")).toContainText("Connection saved");
  await expect(detail(page, "Event history")).toHaveText("30 days");
  await expect(header(page)).toContainText(hint);
  expect(patches.map((p) => JSON.parse(p))).toEqual([
    {
      config: {
        google: {
          serviceAccountKey: "",
          subject: "ada@example.com",
          eventDays: 30,
        },
      },
    },
  ]);

  // The new days queued a sync; the Docs are the same once it ends.
  const docs = await openInTree(page, "Google Workspace", 5, "Docs");
  await expect(docs.locator(":scope > .dt-row .dt-count")).toHaveText("14");
  await expect(docs.getByRole("link")).toHaveCount(14);
  await docs.getByRole("link", { name: "Q4 planning" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Q4 planning" }),
  ).toBeVisible();
  await expect(
    page.getByRole("link", { name: "Open in Google Workspace" }),
  ).toHaveAttribute(
    "href",
    /^https:\/\/docs\.google\.com\/document\/d\/1mockDoc0001\w+\/edit\?usp=drivesdk$/,
  );
  await expect(
    page.getByRole("heading", { name: "Next steps", exact: true }),
  ).toBeVisible();
  for (const body of bodies) expect(body).not.toContain("MIIEe2eGoogleKey");
  expect(await page.content()).not.toContain("MIIEe2eGoogleKey");
});
