import type { Browser, Page } from "@playwright/test";
import { APP_ORIGIN, expect, mock, signIn, test } from "./support/mock";

// Helix Foundry for Slack (Settings › Slack, /slack/installed, /slack/link)
// against the mock (mock/routes/slack.ts): an owner adds the app through the
// mock Slack's page and disconnects it, a person links their Slack account
// from the link the app sends them (its token in the fragment, kept through
// a sign-in), a Slack account that isn't theirs can't be linked, Slack's
// refusals and a cancel say why in place, and members see the setting
// without managing it.

const settingsPanel = (page: Page, title: string) =>
  page
    .locator(".hf-panel")
    .filter({ has: page.getByRole("heading", { name: title, exact: true }) });

/** Ada adds Helix Foundry to Acme's Slack from Settings › Slack. */
async function addToSlack(page: Page) {
  await page.goto("/settings/slack");
  await page.getByRole("button", { name: "Add to Slack" }).click();
  await page.waitForURL(/\/mock\/slack\/authorize\?/);
  await expect(
    page.getByText("Helix Foundry asks to be added to Acme Slack"),
  ).toBeVisible();
  await page.getByRole("button", { name: "Allow" }).click();
  await expect(page).toHaveURL(/\/settings\/slack$/);
  await expect(settingsPanel(page, "Slack")).toContainText("Acme Slack");
}

/** A signed-out page of its own, as another browser would be. */
async function newPage(browser: Browser) {
  const context = await browser.newContext({ baseURL: APP_ORIGIN });
  return context.newPage();
}

test("an owner adds the app, links their Slack account and unlinks it", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/settings/slack");
  await expect(page.getByRole("tab", { name: "Slack" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await expect(
    page.getByText(/AI answers can be wrong; check the cited sources/),
  ).toBeVisible();
  await addToSlack(page);
  const slack = settingsPanel(page, "Slack");
  await expect(slack).toContainText("Ada Lovelace");
  await expect(slack.getByRole("button", { name: "Disconnect" })).toBeVisible();
  const mine = settingsPanel(page, "Your Slack account");
  await expect(mine).toContainText("Not linked. Message @Helix Foundry");
  await expect(settingsPanel(page, "Linked people")).toContainText(
    "No one is linked yet",
  );

  const { url } = await mock.slackLinkToken({
    email: "ada@example.com",
    handle: "ada",
  });
  await page.goto(url);
  // The token leaves the address before any call.
  await expect(page).toHaveURL(/\/slack\/link$/);
  expect(new URL(page.url()).hash).toBe("");
  await expect(
    page.getByText(
      "Slack user @ada (a•••@example.com) in Acme Slack will act as you (ada@example.com) in Acme.",
    ),
  ).toBeVisible();
  await page.getByRole("button", { name: "Link", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Your Slack account is linked" }),
  ).toBeVisible();

  await page.getByRole("link", { name: "Open Settings › Slack" }).click();
  await expect(mine).toContainText("Helix Foundry answers your Slack account");
  await expect(mine).toContainText("U0ADA");
  const people = page.getByRole("list", { name: "Linked people" });
  await expect(people.getByRole("listitem")).toHaveCount(1);
  await expect(people).toContainText("Ada Lovelace");
  await expect(people).toContainText("You");

  await mine.getByRole("button", { name: "Unlink" }).click();
  const dialog = page.getByRole("dialog", {
    name: "Unlink your Slack account?",
  });
  await dialog.getByRole("button", { name: "Unlink" }).click();
  await expect(dialog).toBeHidden();
  await expect(mine).toContainText("Not linked.");
  await expect(settingsPanel(page, "Linked people")).toContainText(
    "No one is linked yet",
  );

  // The link was spent: opening it again says so.
  await page.goto(url);
  await expect(
    page.getByRole("heading", { name: "This link can’t be used" }),
  ).toBeVisible();
  await expect(page.getByText(/This link expired or was used/)).toBeVisible();
});

test("a link keeps its token through signing in", async ({ page, browser }) => {
  await signIn(page);
  await addToSlack(page);
  const { url } = await mock.slackLinkToken({
    email: "alan@example.com",
    handle: "alan",
  });
  const alan = await newPage(browser);
  await alan.goto(url);
  await expect(alan).toHaveURL(/\/login/);
  // The sign-in's return path carries the workspace, never the token.
  expect(alan.url()).not.toContain("hx_sl_");
  await signIn(alan, "alan@example.com");
  await expect(alan).toHaveURL(/\/slack\/link$/);
  await expect(
    alan.getByText(
      "Slack user @alan (a•••@example.com) in Acme Slack will act as you (alan@example.com) in Acme.",
    ),
  ).toBeVisible();
  await alan.getByRole("button", { name: "Link", exact: true }).click();
  await expect(
    alan.getByRole("heading", { name: "Your Slack account is linked" }),
  ).toBeVisible();
  // Ada, an owner, sees him linked and can unlink him.
  await page.reload();
  const people = page.getByRole("list", { name: "Linked people" });
  await expect(people).toContainText("Alan Turing");
  await people.getByRole("button", { name: "Unlink Alan Turing" }).click();
  await page
    .getByRole("dialog", { name: "Unlink Alan Turing?" })
    .getByRole("button", { name: "Unlink" })
    .click();
  await expect(settingsPanel(page, "Linked people")).toContainText(
    "No one is linked yet",
  );
});

test("a Slack account that isn't yours can't be linked", async ({ page }) => {
  await signIn(page);
  await addToSlack(page);
  const link = page.getByRole("button", { name: "Link", exact: true });

  await page.goto(
    (await mock.slackLinkToken({ email: "grace@example.com" })).url,
  );
  await expect(page.getByText(/in Acme Slack will act as you/)).toBeVisible();
  await expect(
    page.getByText("This Slack account’s email isn’t your Foundry email."),
  ).toBeVisible();
  await expect(link).toBeDisabled();

  await page.goto(
    (await mock.slackLinkToken({ email: "ada@example.com", guest: true })).url,
  );
  await expect(
    page.getByText(/isn’t a full member of the connected Slack workspace/),
  ).toBeVisible();
  await expect(link).toBeDisabled();

  await mock.configure({ slackEmailMismatch: true });
  await page.goto(
    (await mock.slackLinkToken({ email: "ada@example.com" })).url,
  );
  await expect(page.getByText(/isn’t your Foundry email/)).toBeVisible();
  await expect(link).toBeDisabled();

  // Cancel forgets the token: the page opened again without one says how
  // to get one.
  await page.getByRole("link", { name: "Cancel" }).click();
  await expect(page).toHaveURL(/\/settings\/slack$/);
  await page.goto("/slack/link");
  await expect(
    page.getByRole("heading", {
      name: "This link has no Slack account to link",
    }),
  ).toBeVisible();
});

test("Slack's refusals and a cancel are said in place", async ({ page }) => {
  await signIn(page);
  await page.goto("/settings/slack");
  await page.getByRole("button", { name: "Add to Slack" }).click();
  await page.waitForURL(/\/mock\/slack\/authorize\?/);
  await page.getByRole("button", { name: "Cancel" }).click();
  await expect(page).toHaveURL(/\/slack\/installed$/);
  await expect(
    page.getByRole("heading", { name: "Helix Foundry wasn’t added" }),
  ).toBeVisible();
  await expect(page.getByText(/was cancelled/)).toBeVisible();

  await mock.configure({ slackTeamTaken: true });
  await page.getByRole("link", { name: "Open Settings › Slack" }).click();
  await page.getByRole("button", { name: "Add to Slack" }).click();
  await page.waitForURL(/\/mock\/slack\/authorize\?/);
  await page.getByRole("button", { name: "Allow" }).click();
  await expect(page).toHaveURL(/\/slack\/installed$/);
  await expect(
    page.getByRole("heading", { name: "Helix Foundry wasn’t added to Slack" }),
  ).toBeVisible();
  await expect(
    page.getByText(
      /This Slack workspace is connected to another Foundry workspace/,
    ),
  ).toBeVisible();
  // Reloaded, there is nothing left to finish.
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "Nothing to finish here" }),
  ).toBeVisible();

  // Slack's other errors are said in fixed words, never echoed; text that
  // isn't one of Slack's codes is nothing to finish.
  await page.goto("/slack/installed?error=invalid_scope");
  await expect(page).toHaveURL(/\/slack\/installed$/);
  await expect(
    page.getByRole("heading", { name: "Slack didn’t add Helix Foundry" }),
  ).toBeVisible();
  await expect(page.getByText("invalid_scope")).toHaveCount(0);
  await page.goto(
    "/slack/installed?error=" +
      encodeURIComponent("Your workspace is suspended; call 555-0100"),
  );
  await expect(page).toHaveURL(/\/slack\/installed$/);
  await expect(
    page.getByRole("heading", { name: "Nothing to finish here" }),
  ).toBeVisible();
  await expect(page.getByText(/suspended/)).toHaveCount(0);
});

test("an owner disconnects the app", async ({ page }) => {
  await signIn(page);
  await addToSlack(page);
  await page.goto(
    (await mock.slackLinkToken({ email: "ada@example.com" })).url,
  );
  await page.getByRole("button", { name: "Link", exact: true }).click();
  await page.goto("/settings/slack");
  await settingsPanel(page, "Slack")
    .getByRole("button", { name: "Disconnect" })
    .click();
  const dialog = page.getByRole("dialog", { name: "Disconnect Acme Slack?" });
  await expect(dialog).toContainText("Everyone’s links are deleted");
  await dialog.getByRole("button", { name: "Disconnect" }).click();
  await expect(
    page.getByRole("button", { name: "Add to Slack" }),
  ).toBeVisible();
  await expect(settingsPanel(page, "Your Slack account")).toHaveCount(0);
});

test("members see the setting and link themselves, without managing it", async ({
  page,
  browser,
}) => {
  await signIn(page, "alan@example.com");
  await page.goto("/settings/slack");
  await expect(
    page.getByText("An owner or admin adds Helix Foundry to Slack."),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Add to Slack" })).toHaveCount(
    0,
  );

  const ada = await newPage(browser);
  await signIn(ada);
  await addToSlack(ada);

  await page.reload();
  const slack = settingsPanel(page, "Slack");
  await expect(slack).toContainText("Acme Slack");
  await expect(slack.getByRole("button", { name: "Disconnect" })).toHaveCount(
    0,
  );
  await expect(settingsPanel(page, "Linked people")).toHaveCount(0);
  await expect(settingsPanel(page, "Your Slack account")).toContainText(
    "Not linked.",
  );
  await page.goto(
    (await mock.slackLinkToken({ email: "alan@example.com" })).url,
  );
  await page.getByRole("button", { name: "Link", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Your Slack account is linked" }),
  ).toBeVisible();
  await page.goto("/settings/slack");
  await expect(settingsPanel(page, "Your Slack account")).toContainText(
    "U0ALAN",
  );
});

test("a restricted member links themselves and unlinks from Settings › Slack", async ({
  page,
  browser,
}) => {
  await signIn(page);
  await addToSlack(page);

  // Eve's sources restrict her: Settings is hers only for Slack.
  const eve = await newPage(browser);
  await signIn(eve, "eve@example.com");
  await eve.goto(
    (await mock.slackLinkToken({ email: "eve@example.com", handle: "eve" }))
      .url,
  );
  await eve.getByRole("button", { name: "Link", exact: true }).click();
  await expect(
    eve.getByRole("heading", { name: "Your Slack account is linked" }),
  ).toBeVisible();
  await eve.getByRole("link", { name: "Open Settings › Slack" }).click();
  await expect(eve).toHaveURL(/\/settings\/slack$/);
  await expect(eve.getByRole("tab")).toHaveText(["Slack"]);
  await expect(settingsPanel(eve, "Slack")).toContainText("Acme Slack");
  await expect(
    settingsPanel(eve, "Slack").getByRole("button", { name: "Disconnect" }),
  ).toHaveCount(0);
  await expect(settingsPanel(eve, "Linked people")).toHaveCount(0);
  const mine = settingsPanel(eve, "Your Slack account");
  await expect(mine).toContainText("U0EVE");
  await mine.getByRole("button", { name: "Unlink" }).click();
  await eve
    .getByRole("dialog", { name: "Unlink your Slack account?" })
    .getByRole("button", { name: "Unlink" })
    .click();
  await expect(mine).toContainText("Not linked.");
  // Her account menu leads back here; Settings' other tabs send her to Ask.
  await eve.getByRole("button", { name: "Account" }).click();
  await expect(eve.getByRole("menuitem", { name: "Slack" })).toBeVisible();
  await eve.keyboard.press("Escape");
  await eve.goto("/settings/members");
  await expect(eve).toHaveURL(/\/ask/);
});

test("the setting says when Helix doesn't offer the app", async ({ page }) => {
  await signIn(page);
  await mock.configure({ slackInstallAllowed: false });
  await page.goto("/settings/slack");
  await expect(
    page.getByText(/Helix offers Helix Foundry for Slack to a few workspaces/),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Add to Slack" })).toHaveCount(
    0,
  );
  await mock.configure({ slackAvailable: false });
  await page.reload();
  await expect(
    page.getByText("Helix Foundry for Slack isn’t available here."),
  ).toBeVisible();
});
