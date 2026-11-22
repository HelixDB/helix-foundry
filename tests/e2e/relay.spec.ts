import { createHash } from "node:crypto";
import type { Browser, Page } from "@playwright/test";
import { e2ePorts } from "../../mock/env";
import { APP_ORIGIN, expect, signIn, test } from "./support/mock";

// Helix's side of the Slack relay (/relay/link, Settings › Slack's linked
// installs) against the mock (mock/routes/relay.ts): an owner links a
// Foundry on someone's computer to Acme, its request kept through a
// sign-in and sent back only to that computer, the install's Add to Slack
// finished here and sent back to it, then revoked; a request that would
// leave the computer, or a member, links nothing; and where the relay is
// off, Settings › Slack doesn't mention it.

const MOCK = `http://127.0.0.1:${e2ePorts.mock}`;
// The install's Settings › Slack: a port nothing listens on, answered here.
const INSTALL = "http://localhost:3999",
  REDIRECT = `${INSTALL}/settings/slack`;
const VERIFIER = "v".repeat(43),
  CHALLENGE = createHash("sha256").update(VERIFIER).digest("base64url"),
  STATE = "state-of-the-install";
const linkPath = (over: Record<string, string> = {}) =>
  `/relay/link?${new URLSearchParams({
    state: STATE,
    code_challenge: CHALLENGE,
    redirect_uri: REDIRECT,
    name: "Ada’s laptop",
    ...over,
  })}`;
const post = async (path: string, body: unknown) => {
  const res = await fetch(`${MOCK}${path}`, {
    method: "POST",
    body: JSON.stringify(body),
  });
  return res.json();
};
const settingsPanel = (page: Page, title: string) =>
  page
    .locator(".hf-panel")
    .filter({ has: page.getByRole("heading", { name: title, exact: true }) });

/** Answers the install's Settings › Slack in place of the install. */
const answerAsInstall = (page: Page) =>
  page.route(`${INSTALL}/**`, (route) =>
    route.fulfill({ contentType: "text/html", body: "<p>The install</p>" }),
  );

/** A signed-out page of its own, as another browser would be. */
async function newPage(browser: Browser) {
  const context = await browser.newContext({ baseURL: APP_ORIGIN });
  return context.newPage();
}

test("an owner links a Foundry on someone's computer, finishes its Add to Slack, and revokes it", async ({
  page,
}) => {
  await signIn(page);
  await answerAsInstall(page);
  await page.goto(linkPath());
  // The request leaves the address before any call.
  await expect(page).toHaveURL(/\/relay\/link$/);
  const panel = settingsPanel(page, "Link a Foundry to Helix");
  await expect(panel).toContainText(
    "Link the Foundry on Ada’s laptop to Acme. Slack messages for that install will pass through Helix, and Helix vouches for who clicked in Slack.",
  );
  await expect(panel).toContainText(INSTALL);
  // The install's name may be a person's: replay leaves it out.
  await expect(panel.locator("[data-replay-block]")).toContainText(
    "Ada’s laptop",
  );
  await panel.getByRole("button", { name: "Link", exact: true }).click();
  await page.waitForURL((url) => url.origin === INSTALL);
  const back = new URL(page.url());
  expect(back.pathname).toBe("/settings/slack");
  expect(back.searchParams.get("state")).toBe(STATE);
  const code = back.searchParams.get("code")!;

  // The install redeems it through Foundry's door.
  const { linkId } = (await post("/__mock/relay/redeem", {
    code,
    codeVerifier: VERIFIER,
  })) as { linkId: string };
  await page.goto("/settings/slack");
  const links = page.getByRole("list", { name: "Linked Foundry installs" });
  await expect(links).toContainText("Ada’s laptop");
  await expect(links).toContainText("Linked by Ada Lovelace");
  await expect(links).toContainText("Not in Slack yet.");

  // Cancelled on Slack's page, nothing says whose install it was: the page
  // tells the install's owner to go back to it, not to add it here.
  const cancelled = (await post("/__mock/relay/slack-install", {
    linkId,
  })) as { authorizeUrl: string };
  await page.goto(cancelled.authorizeUrl);
  await page.getByRole("button", { name: "Cancel" }).click();
  await expect(page).toHaveURL(/\/slack\/installed$/);
  await expect(
    page.getByText(
      "Adding it for the Foundry on your computer? Go back there and add it again from its Settings › Slack.",
      { exact: false },
    ),
  ).toBeVisible();

  // Its Add to Slack, through the door, is finished here and sent back.
  const { authorizeUrl } = (await post("/__mock/relay/slack-install", {
    linkId,
  })) as { authorizeUrl: string };
  await page.goto(authorizeUrl);
  await page.getByRole("button", { name: "Allow" }).click();
  await page.waitForURL(`${REDIRECT}`);
  await page.goto("/settings/slack");
  await expect(links).toContainText("Answers in Laptop Slack.");
  // Acme's own Slack is untouched.
  await expect(settingsPanel(page, "Slack")).toContainText("Add to Slack");

  await links.getByRole("button", { name: "Revoke Ada’s laptop" }).click();
  const dialog = page.getByRole("dialog", { name: "Revoke Ada’s laptop?" });
  await dialog.getByRole("button", { name: "Revoke" }).click();
  await expect(dialog).toBeHidden();
  await expect(settingsPanel(page, "Linked Foundry installs")).toContainText(
    "No Foundry install is linked",
  );
});

test("keeps the request through signing in, and cancels back to the install", async ({
  browser,
}) => {
  const page = await newPage(browser);
  await answerAsInstall(page);
  await page.goto(linkPath());
  await expect(page).toHaveURL(/\/login/);
  // The sign-in's return path never carries the request.
  expect(page.url()).not.toContain(CHALLENGE);
  await signIn(page);
  await expect(page).toHaveURL(/\/relay\/link$/);
  await page.getByRole("button", { name: "Cancel" }).click();
  await page.waitForURL((url) => url.origin === INSTALL);
  const back = new URL(page.url());
  expect(back.searchParams.get("error")).toBe("access_denied");
  expect(back.searchParams.get("state")).toBe(STATE);
  expect(back.searchParams.get("code")).toBeNull();
  // Spent: opening the page again links nothing.
  await page.goto("/relay/link");
  await expect(
    page.getByRole("heading", { name: "Nothing to link here" }),
  ).toBeVisible();
});

test("links nothing off this computer, for a member, or off the install list", async ({
  page,
  browser,
}) => {
  await signIn(page);
  await page.goto(
    linkPath({ redirect_uri: "https://evil.example/settings/slack" }),
  );
  await expect(
    page.getByRole("heading", { name: "This link can’t be used" }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Link" })).toHaveCount(0);

  await fetch(`${MOCK}/__mock/config`, {
    method: "POST",
    body: JSON.stringify({ slackInstallAllowed: false }),
  });
  await page.goto(linkPath());
  await page.getByRole("button", { name: "Link", exact: true }).click();
  await expect(page.locator(".hf-form-error")).toHaveText(
    "Helix Foundry for Slack isn't offered to this workspace yet.",
  );
  await expect(page).toHaveURL(/\/relay\/link$/);

  const alan = await newPage(browser);
  await signIn(alan, "alan@example.com");
  await alan.goto(linkPath());
  await expect(
    alan.getByRole("heading", { name: "You can’t link a Foundry here" }),
  ).toBeVisible();
  // Nor does he see the linked installs.
  await alan.goto("/settings/slack");
  await expect(settingsPanel(alan, "Slack")).toBeVisible();
  await expect(settingsPanel(alan, "Linked Foundry installs")).toHaveCount(0);
});

test("says nothing of linked installs where the relay is off", async ({
  page,
}) => {
  await fetch(`${MOCK}/__mock/config`, {
    method: "POST",
    body: JSON.stringify({ relayAvailable: false }),
  });
  await signIn(page);
  const asked = page.waitForResponse((r) =>
    r.url().endsWith("/foundry/relay-links"),
  );
  await page.goto("/settings/slack");
  expect((await asked).status()).toBe(400);
  await expect(settingsPanel(page, "Slack")).toContainText("Add to Slack");
  await expect(settingsPanel(page, "Linked Foundry installs")).toHaveCount(0);
});
