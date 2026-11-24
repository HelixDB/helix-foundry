import { createHash } from "node:crypto";
import type { Browser, Page } from "@playwright/test";
import { APP_ORIGIN, expect, mock, signIn, test } from "./support/mock";

// Connected apps against the mock (mock/routes/partner-apps.ts): a partner
// app's request, kept through a sign-in, approved by an owner for the
// sources and scopes chosen and sent back to the app with a code its server
// trades for tokens; Cancel, requests Helix refuses, members, an
// unverified app, a second consent, and the installs in Settings ›
// Connected apps, their sources edited and revoked.

const clientId = (name: string) => "hlxpc_" + name.padEnd(43, "0").slice(0, 43);
const NORTHWIND = {
    clientId: clientId("northwind-insights"),
    secret: "hlxpk_northwind-secret",
    redirectUri: "https://northwind.example/helix/callback",
  },
  CONTOSO = {
    clientId: clientId("contoso-sync"),
    redirectUri: "https://contoso.example/cb",
  },
  FABRIKAM = {
    clientId: clientId("fabrikam-legacy"),
    redirectUri: "https://fabrikam.example/cb",
  };
const VERIFIER = "v".repeat(43),
  CHALLENGE = createHash("sha256").update(VERIFIER).digest("base64url"),
  STATE = "partner-state-1";
const ISSUER = "https://mcp.dev.helix-db.com/apps";

/** The MCP host's redirect to the consent page: its request in the fragment. */
const authorizePath = (over: Record<string, string> = {}) =>
  `/oauth/authorize#${new URLSearchParams({
    client_id: NORTHWIND.clientId,
    redirect_uri: NORTHWIND.redirectUri,
    scope: "foundry.read foundry.ask",
    state: STATE,
    code_challenge: CHALLENGE,
    ...over,
  })}`;
const panel = (page: Page, title: string) =>
  page
    .locator(".hf-panel")
    .filter({ has: page.getByRole("heading", { name: title, exact: true }) });
const consent = (page: Page, app = "Northwind Insights") =>
  panel(page, `Connect ${app} to Helix Foundry`);

/** Answers the app's own site in place of the app. */
const answerAsApp = (page: Page) =>
  Promise.all(
    ["northwind.example", "contoso.example", "fabrikam.example"].map((host) =>
      page.route(`https://${host}/**`, (route) =>
        route.fulfill({ contentType: "text/html", body: "<p>The app</p>" }),
      ),
    ),
  );

/** A signed-out page of its own, as another browser would be. */
async function newPage(browser: Browser) {
  const context = await browser.newContext({ baseURL: APP_ORIGIN });
  return context.newPage();
}

test("an owner connects an app through signing in, and it reads only what was chosen", async ({
  browser,
}) => {
  const page = await newPage(browser);
  await answerAsApp(page);
  await page.goto(authorizePath());
  await expect(page).toHaveURL(/\/login/);
  // The sign-in's return path never carries the request.
  expect(page.url()).not.toContain(CHALLENGE);
  expect(page.url()).not.toContain(STATE);
  await signIn(page);
  // The request survived signing in; the address is bare.
  await expect(page).toHaveURL(/\/oauth\/authorize$/);
  const form = consent(page);
  await expect(form).toContainText("By Northwind Labs");
  await expect(form).toContainText("Verified by Helix");
  await expect(form).toContainText(
    "Signed in as ada@example.com, connecting Acme.",
  );
  await expect(form).toContainText("https://northwind.example");
  await expect(form.locator("[data-replay-block]")).toContainText(
    "ada@example.com",
  );
  await expect(
    form.getByRole("link", { name: "northwind.example" }),
  ).toHaveAttribute("rel", "noopener noreferrer");
  // Nothing chosen yet: Approve waits for a source.
  const approve = form.getByRole("button", { name: "Approve" });
  await expect(approve).toBeDisabled();
  await expect(form).toContainText("Choose at least one source.");
  await form.getByRole("checkbox", { name: /^Stripe ·/ }).check();
  await expect(form).toContainText(
    "every table it syncs and every document set it owns, including ones added later",
  );
  // Both scopes start on; at least one stays on.
  const ask = form.getByRole("checkbox", { name: /^Ask ·/ }),
    read = form.getByRole("checkbox", { name: /^Read ·/ });
  await expect(ask).toBeChecked();
  await ask.uncheck();
  await expect(read).toBeChecked();
  await expect(read).toBeDisabled();

  const approved = page.waitForRequest((r) =>
    r.url().endsWith("/partner-apps/authorization:approve"),
  );
  await approve.click();
  const sent = (await approved).postDataJSON();
  expect(sent).toMatchObject({
    clientId: NORTHWIND.clientId,
    state: STATE,
    codeChallenge: CHALLENGE,
    scopes: ["foundry.read"],
    targets: [{ kind: "KIND_CONNECTOR", connectorId: "conn_acme_stripe" }],
  });
  await page.waitForURL((url) => url.origin === "https://northwind.example");
  const back = new URL(page.url());
  expect(back.pathname).toBe("/helix/callback");
  expect(back.searchParams.get("state")).toBe(STATE);
  expect(back.searchParams.get("iss")).toBe(ISSUER);
  const code = back.searchParams.get("code")!;
  expect(code).toMatch(/^hlxpa_/);

  // The app's server trades the code for tokens.
  const tokens = await mock.partnerExchange({
    clientId: NORTHWIND.clientId,
    clientSecret: NORTHWIND.secret,
    code,
    codeVerifier: VERIFIER,
  });
  expect(tokens.status).toBe(200);
  expect(tokens.body.scope).toBe("foundry.read");
  // Once only.
  expect(
    (
      await mock.partnerExchange({
        clientId: NORTHWIND.clientId,
        clientSecret: NORTHWIND.secret,
        code,
        codeVerifier: VERIFIER,
      })
    ).body.error,
  ).toBe("invalid_grant");

  await page.goto("/settings/apps");
  const apps = page.getByRole("list", { name: "Connected apps" });
  const row = apps.getByRole("listitem", { name: "Northwind Insights" });
  await expect(row).toContainText("Northwind Labs · Read · Reads");
  await expect(row).toContainText("Approved by Ada Lovelace");
  await expect(row).toContainText("Never used");
  // Its key is the install's: Access › Agents leaves it to Connected apps.
  await page.goto("/settings/access?tab=agents");
  const keys = page.getByRole("list", { name: "Agent keys" });
  await expect(keys).toContainText("Support agent");
  await expect(keys).not.toContainText("Northwind");
  await page.getByRole("link", { name: "Settings › Connected apps" }).click();
  await expect(page).toHaveURL(/\/settings\/apps$/);
  // Spent: opening the page again approves nothing.
  await page.goto("/oauth/authorize");
  await expect(
    page.getByRole("heading", { name: "Nothing to approve here" }),
  ).toBeVisible();
});

test("Cancel goes back to the app saying so", async ({ page }) => {
  await signIn(page);
  await answerAsApp(page);
  await page.goto(authorizePath());
  await consent(page).getByRole("button", { name: "Cancel" }).click();
  await page.waitForURL((url) => url.origin === "https://northwind.example");
  const back = new URL(page.url());
  expect(back.pathname).toBe("/helix/callback");
  expect(back.searchParams.get("error")).toBe("access_denied");
  expect(back.searchParams.get("state")).toBe(STATE);
  expect(back.searchParams.get("iss")).toBe(ISSUER);
  expect(back.searchParams.get("code")).toBeNull();
});

test("a request Helix refuses, or one that can't be used, goes nowhere", async ({
  page,
}) => {
  await signIn(page);
  await answerAsApp(page);
  const refused = async (
    over: Record<string, string>,
    title: string,
    text: string,
  ) => {
    await page.goto("/ask");
    await page.goto(authorizePath(over));
    const alert = page.getByRole("alert").filter({ hasText: title });
    await expect(alert).toContainText(text);
    await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Cancel" })).toHaveCount(0);
    await expect(page).toHaveURL(/\/oauth\/authorize$/);
  };
  await refused(
    { client_id: clientId("nobody") },
    "This app can’t connect",
    "This app isn't registered with Helix.",
  );
  await refused(
    { redirect_uri: "https://northwind.example/elsewhere" },
    "This app can’t connect",
    "Redirect_uri is not registered for this app.",
  );
  await refused(
    { client_id: FABRIKAM.clientId, redirect_uri: FABRIKAM.redirectUri },
    "This app can’t connect",
    "This app isn't registered with Helix.",
  );
  await refused(
    { redirect_uri: "http://northwind.example/helix/callback" },
    "This link can’t be used",
    "Start connecting from the app again.",
  );
  // A malformed request is kept as nothing.
  await page.goto("/ask");
  await page.goto(authorizePath({ code_challenge: "short" }));
  await expect(
    page.getByRole("heading", { name: "Nothing to approve here" }),
  ).toBeVisible();
});

test("only owners and admins connect an app", async ({ browser }) => {
  const alan = await newPage(browser);
  await answerAsApp(alan);
  await signIn(alan, "alan@example.com");
  await alan.goto(authorizePath());
  const refusal = alan
    .getByRole("alert")
    .filter({ hasText: "Only a workspace’s owners and admins connect apps" });
  await expect(refusal).toContainText(
    "Ask an owner or admin to connect Northwind Insights, or sign in as one.",
  );
  // Nor does he see Connected apps.
  await alan.goto("/settings");
  await expect(alan.getByRole("tab", { name: "Connected apps" })).toHaveCount(
    0,
  );
  await alan.goto(authorizePath());
  await refusal.getByRole("button", { name: "Cancel" }).click();
  await alan.waitForURL((url) => url.origin === "https://northwind.example");
  expect(new URL(alan.url()).searchParams.get("error")).toBe("access_denied");

  // As an admin, he can.
  await mock.configure({ alanRole: "WORKSPACE_ROLE_ADMIN" });
  await alan.goto("/ask");
  await alan.goto(authorizePath());
  const form = consent(alan);
  await expect(form).toContainText(
    "Signed in as alan@example.com, connecting Acme.",
  );
  await form.getByRole("checkbox", { name: /^Shop database ·/ }).check();
  await form.getByRole("button", { name: "Approve" }).click();
  await alan.waitForURL((url) => url.origin === "https://northwind.example");
  expect(new URL(alan.url()).searchParams.get("code")).toMatch(/^hlxpa_/);
});

test("an unverified app says so, and asks only for what it holds", async ({
  page,
}) => {
  await signIn(page);
  await page.goto(
    authorizePath({
      client_id: CONTOSO.clientId,
      redirect_uri: CONTOSO.redirectUri,
      scope: "foundry.read",
    }),
  );
  const form = consent(page, "Contoso Sync");
  await expect(form).toContainText("Unverified by Helix");
  await expect(form).toContainText(
    "Helix hasn’t checked who runs this app. Connect it only if you trust Contoso.",
  );
  await expect(form).not.toContainText("Verified by Helix");
  await expect(form.getByRole("checkbox", { name: /^Read ·/ })).toBeChecked();
  await expect(form.getByRole("checkbox", { name: /^Read ·/ })).toBeDisabled();
  await expect(form.getByRole("checkbox", { name: /^Ask ·/ })).toHaveCount(0);
});

test("a second consent starts from what the app reads, and replaces it", async ({
  page,
}) => {
  await mock.configure({ partnerAppInstalled: true });
  await signIn(page);
  await answerAsApp(page);
  await page.goto(authorizePath());
  const form = consent(page);
  await expect(form).toContainText(
    "Northwind Insights is already connected to Acme. Approving replaces what it can read.",
  );
  await expect(form.getByRole("checkbox", { name: /^Stripe ·/ })).toBeChecked();
  await expect(
    form.getByRole("checkbox", { name: /^Shop database ·/ }),
  ).not.toBeChecked();
  await form.getByRole("checkbox", { name: /^Stripe ·/ }).uncheck();
  await form.getByRole("checkbox", { name: /^Shop database ·/ }).check();
  await form.getByRole("button", { name: "Approve" }).click();
  await page.waitForURL((url) => url.origin === "https://northwind.example");
  const code = new URL(page.url()).searchParams.get("code")!;
  expect(
    (
      await mock.partnerExchange({
        clientId: NORTHWIND.clientId,
        clientSecret: NORTHWIND.secret,
        code,
        codeVerifier: VERIFIER,
      })
    ).status,
  ).toBe(200);
  await page.goto("/settings/apps");
  const rows = page
    .getByRole("list", { name: "Connected apps" })
    .getByRole("listitem");
  await expect(rows).toHaveCount(1);
  await rows
    .getByRole("button", { name: "Edit sources for Northwind Insights" })
    .click();
  const dialog = page.getByRole("dialog", {
    name: "Sources for Northwind Insights",
  });
  await expect(
    dialog.getByRole("checkbox", { name: /^Shop database ·/ }),
  ).toBeChecked();
  await expect(
    dialog.getByRole("checkbox", { name: /^Stripe ·/ }),
  ).not.toBeChecked();
});

test("a failed read of the installs offers Retry, and nothing to approve until they read", async ({
  page,
}) => {
  await mock.configure({ partnerAppInstalled: true });
  await signIn(page);
  await answerAsApp(page);
  let failing = true;
  await page.route("**/papi/v1/workspaces/*/partner-apps/installs", (route) =>
    failing
      ? route.fulfill({
          status: 503,
          contentType: "application/json",
          body: JSON.stringify({ code: 14, message: "the backend is busy" }),
        })
      : route.continue(),
  );
  await page.goto(authorizePath());
  const form = consent(page);
  await expect(form.getByRole("alert")).toContainText(
    "Couldn’t read the workspace’s connected apps. The backend is busy.",
  );
  await expect(form.getByRole("button", { name: "Approve" })).toHaveCount(0);
  await expect(form.getByRole("checkbox", { name: /^Stripe ·/ })).toHaveCount(
    0,
  );
  failing = false;
  await form.getByRole("button", { name: "Retry" }).click();
  // Read, the page says the app is connected and starts from what it reads.
  await expect(form).toContainText(
    "Northwind Insights is already connected to Acme. Approving replaces what it can read.",
  );
  await expect(form.getByRole("checkbox", { name: /^Stripe ·/ })).toBeChecked();
  await expect(form.getByRole("button", { name: "Approve" })).toBeEnabled();
});

test("a failed read of who is signed in, or their workspaces, is no refusal", async ({
  page,
}) => {
  await signIn(page);
  await answerAsApp(page);
  const failing = { whoami: false, workspaces: false };
  const unavailable = {
    status: 503,
    contentType: "application/json",
    body: JSON.stringify({ code: 14, message: "unavailable" }),
  };
  await page.route("**/papi/v1/whoami", (route) =>
    failing.whoami ? route.fulfill(unavailable) : route.continue(),
  );
  await page.route("**/papi/v1/workspaces?*", (route) =>
    failing.workspaces ? route.fulfill(unavailable) : route.continue(),
  );
  const notRefused = async () => {
    await expect(
      page.getByText("Only a workspace’s owners and admins connect apps"),
    ).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Cancel" })).toHaveCount(0);
    await expect(page).toHaveURL(/\/oauth\/authorize$/);
  };
  for (const which of ["whoami", "workspaces"] as const) {
    failing[which] = true;
    await page.goto("/ask");
    await page.goto(authorizePath());
    const retry = page.getByRole("button", { name: "Retry" });
    await expect(retry).toBeVisible({ timeout: 10_000 });
    await notRefused();
    failing[which] = false;
    await retry.click();
    await expect(
      consent(page).getByRole("button", { name: "Approve" }),
    ).toBeVisible();
  }
});

test("an install's sources are edited, alone, and it is revoked", async ({
  page,
}) => {
  await mock.configure({ partnerAppInstalled: true });
  await signIn(page);
  await page.goto("/settings/apps");
  const apps = page.getByRole("list", { name: "Connected apps" }),
    row = apps.getByRole("listitem", { name: "Northwind Insights" });
  await expect(row).toContainText("Verified");
  await expect(row).toContainText("Northwind Labs · Read, Ask · Reads");
  await expect(row).toContainText("Last used");
  await row
    .getByRole("button", { name: "Edit sources for Northwind Insights" })
    .click();
  const dialog = page.getByRole("dialog", {
    name: "Sources for Northwind Insights",
  });
  // Its sources alone: no Usage permission to change.
  await expect(dialog.getByRole("checkbox", { name: /^Usage ·/ })).toHaveCount(
    0,
  );
  await expect(
    dialog.getByRole("checkbox", { name: /^Stripe ·/ }),
  ).toBeChecked();
  const save = dialog.getByRole("button", { name: "Save" });
  await expect(save).toBeDisabled();
  await dialog.getByRole("checkbox", { name: /^Stripe ·/ }).uncheck();
  // Nothing chosen: revoke it instead.
  await expect(save).toBeDisabled();
  await expect(dialog).toContainText(
    "Choose at least one source, or revoke the app.",
  );
  await dialog.getByRole("checkbox", { name: /^Shop database ·/ }).check();
  const put = page.waitForRequest(
    (r) => r.method() === "PUT" && r.url().endsWith("/foundry/access/grants"),
  );
  await save.click();
  const body = (await put).postDataJSON();
  expect(body.principal.kind).toBe("KIND_SERVICE_CREDENTIAL");
  expect(body.targets).toEqual([
    { kind: "KIND_CONNECTOR", connectorId: "conn_acme_shop" },
  ]);
  expect("workflows" in body).toBe(false);
  await expect(dialog).toBeHidden();
  await expect(
    page.getByText("Saved what Northwind Insights can read."),
  ).toBeVisible();

  await row.getByRole("button", { name: "Revoke Northwind Insights" }).click();
  const confirm = page.getByRole("dialog", {
    name: "Revoke Northwind Insights?",
  });
  await expect(confirm).toContainText(
    "Northwind Insights stops reading Acme at once",
  );
  await confirm.getByRole("button", { name: "Revoke" }).click();
  await expect(confirm).toBeHidden();
  await expect(
    page.getByRole("heading", { name: "No apps are connected" }),
  ).toBeVisible();
  // Its key went with it.
  await page.goto("/settings/access?tab=agents");
  await expect(
    page.getByRole("link", { name: "Settings › Connected apps" }),
  ).toHaveCount(0);
});

test("says so where Helix offers no connected apps", async ({ page }) => {
  await mock.configure({ partnerAppsAvailable: false });
  await signIn(page);
  await page.goto(authorizePath());
  await expect(
    page.getByRole("alert").filter({ hasText: "This app can’t connect" }),
  ).toContainText("Connected apps aren't offered here.");
  await page.goto("/settings/apps");
  await expect(
    page
      .getByRole("alert")
      .filter({ hasText: "Connected apps aren't offered here." }),
  ).toBeVisible();
});
