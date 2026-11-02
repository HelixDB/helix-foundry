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

// Connecting GitHub and Linear by signing in at the provider, through the
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

/** Opens GitHub's form from Data's Add data. */
async function openForm(page: Page, kind: "GitHub" | "Linear") {
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
  await page.reload();
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

test("offers no sign-in where the stamp doesn't", async ({ page }) => {
  await mock.configure({ signIn: false });
  await signIn(page);
  await openForm(page, "GitHub");
  await expect(dialog(page).getByLabel("Personal access token")).toBeVisible();
  await expect(
    dialog(page).getByRole("button", { name: /Connect with GitHub/ }),
  ).toHaveCount(0);
});
