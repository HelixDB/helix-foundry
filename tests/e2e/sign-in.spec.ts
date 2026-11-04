import type { Browser, Page } from "@playwright/test";
import {
  csrfToken,
  dataTree,
  expect,
  mock,
  signIn,
  signInToNewWorkspace,
  test,
} from "./support/mock";

// Connecting a source by signing in at its provider, through the
// mock's consent page (/mock/connect, standing in for WorkOS Pipes and the
// provider): from Data, from an agent's link (during onboarding too), its
// retries and refusals, and a connector whose account breaks.

const dialog = (page: Page) => page.locator("dialog[open]");
const SYNCED = { timeout: 15_000 };

/** A POST to the WFE through the BFF, as an agent's MCP call makes it. */
async function post(page: Page, path: string, data: object) {
  const res = await page.request.post(`/papi${path}`, {
    data,
    headers: {
      origin: new URL(page.url()).origin,
      "x-helix-csrf": await csrfToken(page),
    },
  });
  expect(res.status(), await res.text()).toBe(200);
  return res.json();
}

/** Starts a sign-in as the signed-in user, as the agent's tool does. */
async function agentSignIn(page: Page, ws: string, body: object) {
  return (await post(page, `/v1/workspaces/${ws}/foundry/sign-ins`, body))
    .signIn as { id: string };
}

/** The provider's consent page: Authorize (as `account`) or Cancel. */
async function consent(
  page: Page,
  action: "Authorize" | "Cancel",
  account = "",
) {
  await expect(page).toHaveURL(/\/mock\/connect\?sign_in=csi_/);
  await expect(
    page.getByRole("heading", { name: "Authorize Helix Foundry" }),
  ).toBeVisible();
  const signInId = new URL(page.url()).searchParams.get("sign_in");
  if (account) await page.getByLabel(/account/).fill(account);
  await page.getByRole("button", { name: action }).click();
  return signInId;
}

/** Opens a kind's form from Data's Add data. */
async function openForm(page: Page, kind: string) {
  await page.goto("/data?add=1");
  await dialog(page).getByRole("button", { name: kind, exact: true }).click();
}

/** Connects GitHub from Data as the signed-in user; its connector's id. */
async function connectGitHub(page: Page, repos = "") {
  await openForm(page, "GitHub");
  if (repos) await dialog(page).getByLabel("Repositories").fill(repos);
  await dialog(page)
    .getByRole("button", { name: "Connect with GitHub" })
    .click();
  await consent(page, "Authorize");
  await expect(page).toHaveURL(/\/data\/connections\/conn_\w+$/);
  return new URL(page.url()).pathname.split("/").pop()!;
}

/** A second browser, signed in as `email`. */
async function as(browser: Browser, email: string) {
  const page = await (await browser.newContext()).newPage();
  await signIn(page, email);
  return page;
}

test("connects GitHub from Data by signing in, never holding the code", async ({
  page,
}) => {
  const visited: string[] = [];
  page.on("framenavigated", (f) => {
    if (f === page.mainFrame()) visited.push(f.url());
  });
  await signIn(page);
  await openForm(page, "GitHub");
  // Its settings sit above the button; the token is the alternative.
  await expect(dialog(page).locator(".connection-guide")).toContainText(
    "It reads only where Helix Foundry is installed",
  );
  await expect(dialog(page).getByLabel("Personal access token")).toHaveCount(0);
  await dialog(page).getByLabel("Repositories").fill("acme/api");
  const connect = dialog(page).getByRole("button", {
    name: "Connect with GitHub",
  });
  await connect.click();
  await consent(page, "Authorize");

  await expect(page).toHaveURL(/\/data\/connections\/conn_\w+$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "acme/api" }),
  ).toBeVisible();
  await expect(page.getByText("Connected acme/api")).toBeVisible();
  await expect(page.locator(".hf-kv")).toContainText(
    "Reads with Ada Lovelace’s GitHub access (ada)",
  );
  await expect(page.locator(".hf-kv")).not.toContainText("Credential");
  // Its first sync was scheduled by the sign-in, and runs.
  await expect(
    page.locator(".hf-page-header").getByText("Synced", { exact: true }),
  ).toBeVisible(SYNCED);

  // The callback took the code out of the URL before the app ran: only the
  // provider's redirect held it, and the bare callback replaced it.
  const held = [...new Set(visited.filter((u) => u.includes("connect_code=")))];
  expect(held).toHaveLength(1);
  expect(new URL(held[0]!).pathname).toBe("/connect/callback");
  expect(visited.slice(visited.lastIndexOf(held[0]!))).toContain(
    new URL("/connect/callback", page.url()).toString(),
  );
  const calls = (await mock.stats()).requests;
  expect(calls.filter((r) => r.includes(":finish"))).toHaveLength(1);
  expect(calls.filter((r) => r.includes(":authorize"))).toHaveLength(1);
});

test("says why a sign-in was declined, and tries the same one again", async ({
  page,
}) => {
  await signIn(page);
  await openForm(page, "Linear");
  await expect(dialog(page).getByLabel("Personal API key")).toHaveCount(0);
  await dialog(page)
    .getByRole("button", { name: "Connect with Linear" })
    .click();
  const first = await consent(page, "Cancel");
  await expect(page).toHaveURL(/\/connect\/callback$/);
  await expect(page.getByText("Linear wasn’t authorized")).toBeVisible();
  await page.getByRole("button", { name: "Try again" }).click();
  // The same sign-in, so an agent waiting on it follows the retry.
  expect(await consent(page, "Authorize")).toBe(first);
  await expect(
    page.getByRole("heading", { level: 1, name: "Linear" }),
  ).toBeVisible();
  await expect(page.locator(".hf-kv")).toContainText(
    "Reads with Ada Lovelace’s Linear access (Ada Lovelace (Acme))",
  );
});

/** The client tokens of the sign-ins this page starts, in order. */
function startTokens(page: Page) {
  const tokens: string[] = [];
  page.on("request", (r) => {
    if (
      r.method() === "POST" &&
      new URL(r.url()).pathname.endsWith("/foundry/sign-ins")
    )
      tokens.push((r.postDataJSON() as { clientToken: string }).clientToken);
  });
  return tokens;
}

test("starts the form's sign-in again once it ran out", async ({ page }) => {
  await signIn(page);
  const tokens = startTokens(page);
  await openForm(page, "GitHub");
  // Its first Authorize never answers: the form stays, its sign-in started.
  await page.route(/\/sign-ins\/csi_\w+:authorize$/, (route) => route.abort(), {
    times: 1,
  });
  const connect = dialog(page).getByRole("button", {
    name: "Connect with GitHub",
  });
  await connect.click();
  await expect(dialog(page).locator(".error-box")).toBeVisible();
  // It runs out while the form stays open. The next click's token finds
  // it, ended, so the form starts another under a new token.
  await mock.configure({ signInsRunOut: true });
  await connect.click();
  await consent(page, "Authorize");
  await expect(page).toHaveURL(/\/data\/connections\/conn_\w+$/);
  expect(tokens).toHaveLength(3);
  expect(tokens[1]).toBe(tokens[0]);
  expect(tokens[2]).not.toBe(tokens[0]);
});

test("starts another sign-in when the page comes back from the provider", async ({
  page,
}) => {
  await signIn(page);
  const tokens = startTokens(page);
  await openForm(page, "GitHub");
  // The provider's page answers 204, so this page stays, pending: then it
  // is shown again as the back/forward cache restores it after Back.
  await page.route(
    /\/mock\/connect\?sign_in=/,
    (route) => route.fulfill({ status: 204 }),
    { times: 1 },
  );
  await dialog(page)
    .getByRole("button", { name: "Connect with GitHub" })
    .click();
  await expect(
    dialog(page).getByRole("button", { name: "Opening GitHub…" }),
  ).toBeDisabled();
  await page.evaluate(() =>
    window.dispatchEvent(
      new PageTransitionEvent("pageshow", { persisted: true }),
    ),
  );
  // What it started may have ended meanwhile: the next click starts anew.
  await dialog(page)
    .getByRole("button", { name: "Connect with GitHub" })
    .click();
  await consent(page, "Authorize");
  await expect(page).toHaveURL(/\/data\/connections\/conn_\w+$/);
  expect(tokens).toHaveLength(2);
  expect(tokens[1]).not.toBe(tokens[0]);
});

test("keeps the token as the alternative", async ({ page }) => {
  await signIn(page);
  await openForm(page, "GitHub");
  await dialog(page)
    .getByRole("button", { name: "Use a personal access token instead" })
    .click();
  await expect(dialog(page).getByLabel("Personal access token")).toBeVisible();
  await expect(
    dialog(page).getByRole("button", { name: "Test connection" }),
  ).toBeVisible();
  await dialog(page)
    .getByRole("button", { name: "Connect with GitHub instead" })
    .click();
  await expect(
    dialog(page).getByRole("button", { name: "Connect with GitHub" }),
  ).toBeVisible();
});

test("a refreshed callback spends no second code", async ({ page }) => {
  await signIn(page);
  await openForm(page, "GitHub");
  // The first finish reaches the mock, but its answer never reaches the
  // page: it is reloaded first.
  let held = false;
  await page.route("**/foundry/sign-ins/*:finish", async (route) => {
    if (held) return route.continue();
    held = true;
    await route.fetch();
  });
  await dialog(page)
    .getByRole("button", { name: "Connect with GitHub" })
    .click();
  await consent(page, "Authorize");
  await expect
    .poll(async () =>
      (await mock.stats()).requests.some((r) => r.includes(":finish 200")),
    )
    .toBe(true);
  await page.reload();
  await expect(page).toHaveURL(/\/data\/connections\/conn_\w+$/);
  const connectors = (
    await (
      await page.request.get("/papi/v1/workspaces/ws_acme/foundry/connectors")
    ).json()
  ).connectors as { kind: string }[];
  expect(connectors.filter((c) => c.kind === "github")).toHaveLength(1);
  expect(
    (await mock.stats()).requests.filter((r) => r.includes(":finish")),
  ).toHaveLength(2);
});

test("opens an agent's link during onboarding and comes back to the wizard with it chosen", async ({
  page,
}) => {
  await signInToNewWorkspace(page);
  const ws = (
    (await (await page.request.get("/papi/v1/workspaces")).json())
      .workspaces as { id: string; displayName: string }[]
  ).find((w) => w.displayName === "Globex")!.id;
  const s = await agentSignIn(page, ws, {
    kind: "github",
    name: "Globex GitHub",
    config: { github: { repos: ["globex/api"], reviewDays: 30 } },
  });
  await page.goto(`/data?add=github&sign_in=${s.id}&workspace=${ws}`);
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(dialog(page)).toContainText(
    "Your agent asked to connect GitHub to Globex.",
  );
  await expect(dialog(page)).toContainText("Globex GitHub");
  await expect(dialog(page)).toContainText("globex/api");
  // Its settings were chosen when it started: one button, no token.
  await expect(dialog(page).getByRole("textbox")).toHaveCount(0);
  await dialog(page)
    .getByRole("button", { name: "Connect with GitHub" })
    .click();
  await consent(page, "Authorize");

  await expect(page).toHaveURL(/\/ontology\/new\?step=sources$/);
  const row = page
    .getByRole("list", { name: "Connectors" })
    .getByRole("listitem")
    .filter({ hasText: "Globex GitHub" });
  await expect(row.getByRole("checkbox")).toBeChecked(SYNCED);
  const sign = (
    await (
      await page.request.get(
        `/papi/v1/workspaces/${ws}/foundry/sign-ins/${s.id}`,
      )
    ).json()
  ).signIn;
  expect(sign.status).toBe("CONNECTOR_SIGN_IN_STATUS_CONNECTED");
});

test("tells someone else their link isn't theirs, and a member sees no Connect", async ({
  page,
  browser,
}) => {
  await signIn(page);
  const s = await agentSignIn(page, "ws_acme", { kind: "linear" });
  const link = `/data?add=linear&sign_in=${s.id}&workspace=ws_acme`;

  // Alan, a member, adds no data at all.
  const alan = await as(browser, "alan@example.com");
  await alan.goto(link);
  await expect(
    alan.getByRole("heading", { level: 1, name: "Connectors" }),
  ).toBeVisible();
  await expect(
    alan.getByRole("button", { name: "Connect with Linear" }),
  ).toHaveCount(0);

  // As an admin, he may connect, but not with Ada's sign-in.
  await mock.configure({ alanRole: "WORKSPACE_ROLE_ADMIN" });
  await alan.goto(link);
  await expect(dialog(alan)).toContainText(
    "This link was made for someone else: ask your agent for a new one.",
  );
  await expect(
    dialog(alan).getByRole("button", { name: "Connect with Linear" }),
  ).toHaveCount(0);
});

test("freezes a connector whose account breaks until someone signs it in again", async ({
  page,
  browser,
}) => {
  await signIn(page);
  const id = await connectGitHub(page);
  await expect(
    page.locator(".hf-page-header").getByText("Synced", { exact: true }),
  ).toBeVisible(SYNCED);
  await mock.configure({ accountBroken: { [id]: "member_left" } });

  // An owner: the dialog, once per break, then the banner.
  await page.goto("/data");
  const notice = page.getByRole("dialog", { name: "GitHub stopped syncing" });
  await expect(notice).toContainText(
    "Ada Lovelace, who connected it, left the workspace. Its data is frozen as of",
  );
  await notice.getByRole("button", { name: "Later" }).click();
  await expect(notice).toHaveCount(0);
  const banner = page.getByRole("region", {
    name: "Connections that stopped syncing",
  });
  await expect(banner).toContainText("GitHub stopped syncing.");
  await page.reload();
  await expect(banner).toBeVisible();
  await expect(notice).toHaveCount(0);
  // Its status, wherever it shows.
  await expect(
    dataTree(page).getByRole("treeitem", {
      name: "GitHub, Needs signing in again",
    }),
  ).toBeVisible();
  await expect(page.getByRole("grid", { name: "Connections" })).toContainText(
    "Needs signing in again",
  );
  await page.goto(`/data/connections/${id}`);
  await expect(
    page
      .getByRole("alert")
      .filter({ hasText: "GitHub needs signing in again" }),
  ).toContainText("Ada Lovelace, who connected it, left the workspace.");
  await expect(page.getByRole("button", { name: "Sync now" })).toBeDisabled();

  // A member: the banner, without the button.
  const alan = await as(browser, "alan@example.com");
  const theirs = alan.getByRole("region", {
    name: "Connections that stopped syncing",
  });
  await expect(theirs).toContainText(
    "Ask an owner or admin to sign it in again.",
  );
  await expect(
    theirs.getByRole("button", { name: "Sign in again" }),
  ).toHaveCount(0);
  await expect(alan.getByRole("dialog")).toHaveCount(0);

  // Signing in again, as another GitHub account, clears both.
  await banner.getByRole("button", { name: "Sign in again" }).click();
  await consent(page, "Authorize", "ada-other");
  await expect(page).toHaveURL(new RegExp(`/data/connections/${id}$`));
  await expect(
    page.getByText("Now reads with ada-other’s access"),
  ).toBeVisible();
  await expect(banner).toHaveCount(0);
  await expect(page.locator(".hf-kv")).toContainText(
    "Reads with Ada Lovelace’s GitHub access (ada-other)",
  );
  await alan.reload();
  await expect(alan.getByRole("heading", { level: 1 }).first()).toBeVisible();
  await expect(theirs).toHaveCount(0);
});

test("hides the banner for the session with Later", async ({ page }) => {
  await signIn(page);
  const id = await connectGitHub(page);
  await mock.configure({ accountBroken: { [id]: "expired" } });
  await page.goto("/data");
  await page
    .getByRole("dialog", { name: "GitHub stopped syncing" })
    .getByRole("button", { name: "Later" })
    .click();
  const banner = page.getByRole("region", {
    name: "Connections that stopped syncing",
  });
  await expect(banner).toContainText(
    "Its access to GitHub lapsed and couldn’t be renewed.",
  );
  await banner.getByRole("button", { name: "Later" }).click();
  await expect(banner).toHaveCount(0);
  // Still broken after a reload, but hidden for the session.
  const read = page.waitForResponse(/connectors\?needsSignIn=true/);
  await page.reload();
  expect((await (await read).json()).connectors).toHaveLength(1);
  await expect(
    page.getByRole("heading", { level: 1, name: "Connectors" }),
  ).toBeVisible();
  await expect(banner).toHaveCount(0);
});

test("says what a new connection can't see, and lets only its member widen it", async ({
  page,
  browser,
}) => {
  await signIn(page);
  const id = await connectGitHub(page, "acme/api\nacme/missing");
  const problem = page.getByRole("status").filter({
    hasText: "Connected, but GitHub can’t read everything it names",
  });
  await expect(problem).toContainText(
    "Helix Foundry can't see acme/missing: install it on acme",
  );
  await expect(
    problem.getByRole("link", { name: "Install Helix Foundry" }),
  ).toHaveAttribute("href", /^https:\/\/github\.com\/apps\//);
  // Its first sync fails until the app is installed there.
  await expect(page.locator(".dc-sync-error")).toContainText(
    "The last sync failed. Helix Foundry can't see acme/missing",
    SYNCED,
  );

  // Another admin may narrow it, but not add a repository.
  await mock.configure({ alanRole: "WORKSPACE_ROLE_ADMIN" });
  const alan = await as(browser, "alan@example.com");
  await alan.goto(`/data/connections/${id}`);
  await alan.getByRole("button", { name: "Connection actions" }).click();
  await alan.getByRole("menuitem", { name: "Edit connection" }).click();
  const edit = dialog(alan);
  await expect(edit).toContainText(
    "Reads with Ada Lovelace’s GitHub access (ada).",
  );
  await expect(edit.getByLabel("Personal access token")).toHaveCount(0);
  const repos = edit.getByLabel("Repositories");
  await repos.fill("acme/api\nacme/web");
  await expect(edit.getByRole("alert")).toContainText(
    "Only Ada Lovelace, whose GitHub access it reads with, can add repositories or read every one.",
  );
  await expect(
    edit.getByRole("button", { name: "Save changes" }),
  ).toBeDisabled();
  await repos.fill("acme/api");
  await expect(
    edit.getByRole("button", { name: "Save changes" }),
  ).toBeEnabled();
});

test("lets another admin narrow a connector that reads every repository", async ({
  page,
  browser,
}) => {
  await signIn(page);
  // Ada leaves its repositories blank: every one it can see.
  const id = await connectGitHub(page);
  await mock.configure({ alanRole: "WORKSPACE_ROLE_ADMIN" });
  const alan = await as(browser, "alan@example.com");
  await alan.goto(`/data/connections/${id}`);
  // Settings change once its first sync has ended.
  await expect(
    alan.locator(".hf-page-header").getByText("Synced", { exact: true }),
  ).toBeVisible(SYNCED);
  await alan.getByRole("button", { name: "Connection actions" }).click();
  await alan.getByRole("menuitem", { name: "Edit connection" }).click();
  const edit = dialog(alan);
  await edit.getByLabel("Repositories").fill("acme/api");
  await expect(edit.getByRole("alert")).toHaveCount(0);
  const save = alan.waitForResponse(
    (r) => r.request().method() === "PATCH" && r.url().includes(id),
  );
  await edit.getByRole("button", { name: "Save changes" }).click();
  expect((await save).status()).toBe(200);
  await expect(alan.locator(".hf-kv")).toContainText("acme/api");
});

test("signs a connector in again from an agent's link, saying whose access it replaces", async ({
  page,
  browser,
}) => {
  await signIn(page);
  const id = await connectGitHub(page);
  await expect(
    page.locator(".hf-page-header").getByText("Synced", { exact: true }),
  ).toBeVisible(SYNCED);
  // Alan, an admin, signs Ada's connector in again, as his agent asks.
  await mock.configure({ alanRole: "WORKSPACE_ROLE_ADMIN" });
  const alan = await as(browser, "alan@example.com");
  const s = await agentSignIn(alan, "ws_acme", {
    kind: "github",
    connectorId: id,
  });
  await alan.goto(`/data?add=github&sign_in=${s.id}&workspace=ws_acme`);
  const form = dialog(alan);
  await expect(form).toContainText(
    "Your agent asked to sign GitHub in again, in Acme.",
  );
  await expect(form).toContainText(
    "Signing in again makes GitHub read with your GitHub access from now on, in place of Ada Lovelace’s (ada).",
  );
  await form.getByRole("button", { name: "Sign in again with GitHub" }).click();
  await consent(alan, "Authorize");
  // Back on the connector it signed in again, said as such.
  await expect(alan).toHaveURL(new RegExp(`/data/connections/${id}$`));
  await expect(
    alan.getByText("Signed in again. GitHub syncs now"),
  ).toBeVisible();
  await expect(alan.locator(".hf-kv")).toContainText(
    "Reads with Alan Turing’s GitHub access (alan)",
  );
});

test("says what happened the last time an agent's sign-in was tried", async ({
  page,
}) => {
  await signIn(page);
  const s = await agentSignIn(page, "ws_acme", { kind: "linear" });
  const link = `/data?add=linear&sign_in=${s.id}&workspace=ws_acme`;
  await page.goto(link);
  await dialog(page)
    .getByRole("button", { name: "Connect with Linear" })
    .click();
  await consent(page, "Cancel");
  await expect(page.getByText("Linear wasn’t authorized")).toBeVisible();
  // The link again: what happened, then the same button.
  await page.goto(link);
  await expect(dialog(page).getByRole("status")).toContainText(
    "Last time: Linear wasn’t authorized. Helix Foundry was declined at Linear, so nothing was connected.",
  );
  await expect(
    dialog(page).getByRole("button", { name: "Connect with Linear" }),
  ).toBeVisible();
});

test("opens an agent's sign-in under its own kind, whatever the link's add= says", async ({
  page,
}) => {
  await signIn(page);
  const s = await agentSignIn(page, "ws_acme", { kind: "github" });
  await page.goto(`/data?add=linear&sign_in=${s.id}&workspace=ws_acme`);
  await expect(dialog(page)).toHaveAttribute("aria-label", "GitHub");
  await expect(dialog(page)).toContainText(
    "Your agent asked to connect GitHub to Acme.",
  );
  // Another kind's form is the kind's own, not the sign-in's.
  await dialog(page).getByRole("button", { name: "← All sources" }).click();
  await dialog(page)
    .getByRole("button", { name: "Linear", exact: true })
    .click();
  await expect(dialog(page)).not.toContainText("Your agent asked");
  await expect(
    dialog(page).getByRole("button", { name: "Connect with Linear" }),
  ).toBeVisible();
});

test("comes back to Generate with a long goal and its picks", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/ontology/ont_acme_contracts/generate");
  // Too long for a link once encoded: it travels with the sign-in instead.
  const goal = "顧客ごとの契約と請求と更新を一つにまとめる".repeat(12);
  await page.getByLabel("Goal").fill(goal);
  const connectors = page.getByRole("list", { name: "Connectors" });
  await connectors.getByRole("checkbox", { name: /Shop database/ }).uncheck();
  await page.getByRole("button", { name: "Add data" }).first().click();
  await dialog(page)
    .getByRole("button", { name: "GitHub", exact: true })
    .click();
  await dialog(page)
    .getByRole("button", { name: "Connect with GitHub" })
    .click();
  await consent(page, "Authorize");
  await expect(page).toHaveURL(
    /\/ontology\/ont_acme_contracts\/generate\?connected=conn_\w+$/,
  );
  await expect(page.getByLabel("Goal")).toHaveValue(goal);
  await expect(
    connectors.getByRole("checkbox", { name: /Shop database/ }),
  ).not.toBeChecked();
  await expect(
    connectors.getByRole("checkbox", { name: /Stripe/ }),
  ).toBeChecked();
  // The new connection is chosen once its first sync ends.
  await expect(
    connectors.getByRole("checkbox", { name: /GitHub/ }),
  ).toBeChecked(SYNCED);
});

test("comes back to an ontology's setup with what it chose", async ({
  page,
}) => {
  await signIn(page);
  const { ontology } = await post(
    page,
    "/v1/workspaces/ws_acme/foundry/ontologies",
    { name: "Billing", tenantId: "ten_acme_sandbox", clientToken: "billing-1" },
  );
  await page.goto(`/ontology/${ontology.id}?step=sources`);
  const connectors = page.getByRole("list", { name: "Connectors" });
  await connectors.getByRole("checkbox", { name: /Shop database/ }).uncheck();
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByLabel("Goal").fill("Billing health");
  await page.getByRole("button", { name: "Back" }).click();
  await page.getByRole("button", { name: "Add data" }).first().click();
  await dialog(page)
    .getByRole("button", { name: "GitHub", exact: true })
    .click();
  await dialog(page)
    .getByRole("button", { name: "Connect with GitHub" })
    .click();
  await consent(page, "Authorize");
  // Its sources step, as it was, with the new connection among them.
  await expect(page).toHaveURL(
    new RegExp(`/ontology/${ontology.id}\\?step=sources&connected=conn_\\w+$`),
  );
  await expect(
    connectors.getByRole("checkbox", { name: /Shop database/ }),
  ).not.toBeChecked();
  await expect(
    connectors.getByRole("checkbox", { name: /GitHub/ }),
  ).toBeChecked(SYNCED);
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(page.getByLabel("Goal")).toHaveValue("Billing health");
});

test("connects Slack by signing in, keeping its history and refusing a user token", async ({
  page,
}) => {
  await signIn(page);
  await openForm(page, "Slack");
  const form = dialog(page);
  await expect(form.locator(".connection-guide")).toContainText(
    "invite it to each channel to sync",
  );
  await expect(form.getByLabel("Bot token")).toHaveCount(0);
  await form.getByLabel("Days of history").fill("30");
  await form.getByRole("button", { name: "Connect with Slack" }).click();
  // Signed in as a user token's account: refused, as a pasted one is.
  await consent(page, "Authorize", "Ada (xoxp user token)");
  await expect(
    page.getByText("The connection’s settings were refused"),
  ).toBeVisible();
  await page.getByRole("link", { name: "Start again" }).click();
  await expect(dialog(page)).toHaveAttribute("aria-label", "Slack");
  await dialog(page).getByLabel("Days of history").fill("30");
  await dialog(page)
    .getByRole("button", { name: "Connect with Slack" })
    .click();
  await consent(page, "Authorize");
  await expect(
    page.getByRole("heading", { level: 1, name: "Slack" }),
  ).toBeVisible();
  const details = page.locator(".hf-kv");
  await expect(details).toContainText(
    "Reads with Ada Lovelace’s Slack access (Acme · @Helix Foundry)",
  );
  await expect(details).toContainText("30 days");
  await expect(
    page.locator(".hf-page-header").getByText("Synced", { exact: true }),
  ).toBeVisible(SYNCED);
});

test("connects Notion by signing in, reading the pages chosen at Notion", async ({
  page,
}) => {
  await signIn(page);
  await openForm(page, "Notion");
  const form = dialog(page);
  await expect(form.locator(".connection-guide")).toContainText(
    "At Notion’s screen, choose the pages and databases Helix Foundry may read",
  );
  await expect(form.getByLabel("Integration secret")).toHaveCount(0);
  await expect(
    form.getByRole("button", { name: "Use an integration secret instead" }),
  ).toBeVisible();
  await form.getByRole("button", { name: "Connect with Notion" }).click();
  await consent(page, "Authorize");
  await expect(
    page.getByRole("heading", { level: 1, name: "Notion" }),
  ).toBeVisible();
  await expect(page.locator(".hf-kv")).toContainText(
    "Reads with Ada Lovelace’s Notion access (Acme)",
  );
  await expect(
    page.locator(".hf-page-header").getByText("Synced", { exact: true }),
  ).toBeVisible(SYNCED);
  // Its pages are documents of its own, as a key's are.
  await expect(page.getByRole("list", { name: "Document sets" })).toContainText(
    "Pages",
  );
});

test("signs PostHog in on US Cloud only, and lets only its member change the project", async ({
  page,
  browser,
}) => {
  await signIn(page);
  await openForm(page, "PostHog");
  const form = dialog(page);
  await expect(form.getByLabel("Personal API key")).toHaveCount(0);
  // EU Cloud keeps its key: Pipes' PostHog signs in to US Cloud only.
  await form.getByRole("combobox", { name: "Region" }).click();
  await page.getByRole("option", { name: "EU Cloud" }).click();
  await expect(form.getByLabel("Personal API key")).toBeVisible();
  await expect(form).toContainText(
    "EU Cloud and self-hosted PostHog connect with a personal API key: signing in reaches US Cloud only.",
  );
  await expect(
    form.getByRole("button", { name: "Connect with PostHog" }),
  ).toHaveCount(0);
  await form.getByRole("combobox", { name: "Region" }).click();
  await page.getByRole("option", { name: "US Cloud" }).click();
  await form.getByLabel("Project ID (optional)").fill("12345");
  await form.getByRole("button", { name: "Connect with PostHog" }).click();
  await consent(page, "Authorize");
  await expect(page).toHaveURL(/\/data\/connections\/conn_\w+$/);
  const id = new URL(page.url()).pathname.split("/").pop()!;
  await expect(
    page.locator(".hf-page-header").getByText("Synced", { exact: true }),
  ).toBeVisible(SYNCED);

  await mock.configure({ alanRole: "WORKSPACE_ROLE_ADMIN" });
  const alan = await as(browser, "alan@example.com");
  await alan.goto(`/data/connections/${id}`);
  await alan.getByRole("button", { name: "Connection actions" }).click();
  await alan.getByRole("menuitem", { name: "Edit connection" }).click();
  const edit = dialog(alan);
  const project = edit.getByLabel("Project ID (optional)");
  await expect(project).toHaveValue("12345");
  // Signed in, its region is US Cloud's.
  await expect(edit.getByRole("combobox", { name: "Region" })).toHaveCount(0);
  await project.fill("67890");
  await expect(edit.getByRole("alert")).toHaveText(
    "Only Ada Lovelace, whose PostHog access it reads with, can change its project.",
  );
  await expect(
    edit.getByRole("button", { name: "Save changes" }),
  ).toBeDisabled();
  // Its event history is anyone's to change.
  await project.fill("12345");
  await edit.getByLabel("Event history (days)").fill("60");
  await expect(edit.getByRole("alert")).toHaveCount(0);
  const save = alan.waitForResponse(
    (r) => r.request().method() === "PATCH" && r.url().includes(id),
  );
  await edit.getByRole("button", { name: "Save changes" }).click();
  expect((await save).status()).toBe(200);
});

test("offers no sign-in where the stamp doesn't", async ({ page }) => {
  await mock.configure({ signIn: false });
  await signIn(page);
  await openForm(page, "GitHub");
  await expect(dialog(page).getByLabel("Personal access token")).toBeVisible();
  await expect(
    dialog(page).getByRole("button", { name: /Connect with GitHub/ }),
  ).toHaveCount(0);
});
