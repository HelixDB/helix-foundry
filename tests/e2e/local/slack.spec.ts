import { expect, test as base, type Page } from "@playwright/test";
import { e2eLocalPorts } from "../../../mock/env";

// A local build's Settings › Slack against its own mock (mock/routes/relay.ts,
// mock/routes/slack.ts as a local API): Slack reaches it only through
// Helix's relay. The owner links it to Helix (the mock's /relay/link, which
// hands a code back to Settings › Slack), adds Helix Foundry to Slack
// through Helix, links their own Slack account as the owner, turns
// approving changes from Slack off, allows someone who asked to read and
// ask, removes them, and unlinks; a cancelled or expired link says why in
// place, and Helix not answering says since when.

const MOCK = `http://127.0.0.1:${e2eLocalPorts.mock}`;
const post = async (path: string, body: unknown) =>
  (
    await fetch(`${MOCK}${path}`, {
      method: "POST",
      body: JSON.stringify(body),
    })
  ).json();
const configure = (config: {
  relayLinked?: boolean;
  relayOnline?: boolean;
  relayLinkOutcome?: "expired" | "unavailable";
}) => post("/__mock/config", config);

const test = base.extend<{ fresh: void }>({
  fresh: [
    // Playwright fixtures must destructure their first argument.
    // eslint-disable-next-line no-empty-pattern
    async ({}, use) => {
      await fetch(`${MOCK}/__mock/reset`, { method: "POST" });
      await use();
    },
    { auto: true },
  ],
});

const panel = (page: Page, title: string) =>
  page
    .locator(".hf-panel")
    .filter({ has: page.getByRole("heading", { name: title, exact: true }) });

/** Links the install to Helix through the mock's /relay/link. */
async function linkToHelix(page: Page) {
  await page.goto("/settings/slack");
  await panel(page, "Helix")
    .getByRole("button", { name: "Link to Helix" })
    .click();
  await page.waitForURL(/\/mock\/relay\/link\?/);
  await expect(
    page.getByText("Link the Foundry on Ada’s laptop to Acme"),
  ).toBeVisible();
  await page.getByRole("button", { name: "Link" }).click();
  // Back on Settings › Slack, the code already out of the address.
  await expect(page).toHaveURL(/\/settings\/slack$/);
}

test("links to Helix, adds Slack through it, and lets the owner choose who uses it", async ({
  page,
}) => {
  await page.goto("/settings/slack");
  const helix = panel(page, "Helix");
  await expect(helix).toContainText(
    "Nothing leaves this computer until you link it.",
  );
  // No Slack before Helix.
  const slack = panel(page, "Slack");
  await expect(slack).toContainText("Link this Foundry to Helix first");
  await expect(
    slack.getByRole("button", { name: "Add to Slack" }),
  ).toBeDisabled();

  await linkToHelix(page);
  await expect(
    page.getByText("This Foundry is linked to Helix as ada@example.com"),
  ).toBeVisible();
  await expect(helix).toContainText("ada@example.com");
  await expect(helix).toContainText("In contact");

  await slack.getByRole("button", { name: "Add to Slack" }).click();
  await page.waitForURL(/\/mock\/slack\/authorize\?/);
  await page.getByRole("button", { name: "Allow" }).click();
  // Helix finishes it and sends the browser back here.
  await expect(page).toHaveURL(/\/settings\/slack$/);
  await expect(slack).toContainText("Acme Slack");
  // No shared bot on a local install.
  await expect(panel(page, "Who it answers as")).toHaveCount(0);
  await expect(
    page.getByText(
      "Links in its answers open on the computer running this Foundry.",
    ),
  ).toBeVisible();
  const mine = panel(page, "Your Slack account");
  await expect(mine).toContainText(
    "Not linked. Message @Helix Foundry in Acme Slack from the Slack account that added it",
  );

  // The app's link to the Slack account that added it.
  const { url } = (await post("/__mock/slack/link-token", {
    email: "ada@example.com",
    handle: "ada",
  })) as { url: string };
  await page.goto(url);
  await expect(page).toHaveURL(/\/slack\/link$/);
  await expect(
    page.getByText(
      "Slack user @ada in Acme Slack will act as this Foundry’s owner in Acme.",
    ),
  ).toBeVisible();
  await page.getByRole("button", { name: "Link", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Your Slack account is linked" }),
  ).toBeVisible();
  await page.getByRole("link", { name: "Open Settings › Slack" }).click();
  await expect(mine).toContainText("as this Foundry’s owner");

  // Approving changes from Slack, off.
  const changes = panel(page, "Changes from Slack");
  const approve = changes.getByRole("switch", {
    name: "Approve changes from Slack",
  });
  await expect(approve).toBeChecked();
  await approve.click();
  await expect(approve).not.toBeChecked();
  await expect(changes).toContainText(
    "Helix Foundry only reads and asks in Slack",
  );

  // Someone who asked, allowed read-only, then removed.
  await post("/__mock/slack/access-request", {
    slackUser: "U0BEA",
    name: "Bea",
  });
  await page.reload();
  const people = page.getByRole("list", { name: "People in Slack" });
  const bea = people.getByRole("listitem").filter({ hasText: "U0BEA" });
  await expect(bea).toContainText("Bea");
  await expect(bea).toContainText("Asked");
  await people.getByRole("button", { name: "Allow Bea" }).click();
  await expect(page.getByText("Bea may read and ask in Slack")).toBeVisible();
  await expect(bea).toContainText("Reads and asks as you, changing nothing.");
  await people.getByRole("button", { name: "Remove U0BEA" }).click();
  await page
    .getByRole("dialog", { name: "Remove U0BEA?" })
    .getByRole("button", { name: "Remove" })
    .click();
  await expect(people.getByRole("listitem")).toHaveCount(1);

  // Unlinking from Helix takes Slack with it.
  await helix.getByRole("button", { name: "Unlink" }).click();
  await page
    .getByRole("dialog", { name: "Unlink this Foundry from Helix?" })
    .getByRole("button", { name: "Unlink" })
    .click();
  await expect(
    helix.getByRole("button", { name: "Link to Helix" }),
  ).toBeVisible();
  await expect(
    slack.getByRole("button", { name: "Add to Slack" }),
  ).toBeDisabled();
});

test("says why linking to Helix didn't finish", async ({ page }) => {
  await page.goto("/settings/slack");
  await panel(page, "Helix")
    .getByRole("button", { name: "Link to Helix" })
    .click();
  await page.waitForURL(/\/mock\/relay\/link\?/);
  await page.getByRole("button", { name: "Cancel" }).click();
  await expect(page).toHaveURL(/\/settings\/slack$/);
  await expect(panel(page, "Helix").getByRole("alert")).toHaveText(
    "Linking to Helix was cancelled.",
  );

  await configure({ relayLinkOutcome: "expired" });
  await linkToHelix(page);
  await expect(panel(page, "Helix").getByRole("alert")).toHaveText(
    "This link expired or was finished already: link to Helix again.",
  );
  await expect(
    panel(page, "Helix").getByRole("button", { name: "Link to Helix" }),
  ).toBeVisible();
});

test("says when Helix stopped answering", async ({ page }) => {
  await configure({ relayLinked: true, relayOnline: false });
  await page.goto("/settings/slack");
  const helix = panel(page, "Helix");
  await expect(helix).toContainText("Not answering");
  await expect(helix).toContainText("Helix hasn’t answered this Foundry since");
  // Linked, Slack can be added.
  await expect(
    panel(page, "Slack").getByRole("button", { name: "Add to Slack" }),
  ).toBeEnabled();
});
