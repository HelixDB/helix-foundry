import type { Page } from "@playwright/test";
import {
  APP_ORIGIN,
  choose,
  csrfToken,
  expect,
  mock,
  signIn,
  signInToNewWorkspace,
  test,
} from "./support/mock";

// The workspace area: settings (general, members and invitations, plan and
// billing) and the account menu. Ada owns Acme, which is on Foundry
// Pro; Alan is a member of it. Upgrades start from a new, Free workspace;
// switches between Pro and Max start from Acme.

// The shell's toast with this text (other status regions may be on screen).
const toast = (page: Page, text: string) =>
  page.locator(".hf-toast").filter({ hasText: text });

// The plan each checkout call asked for, in order.
function checkoutTiers(page: Page) {
  const tiers: unknown[] = [];
  page.on("request", (r) => {
    if (r.method() === "POST" && r.url().endsWith("/foundry/plan:checkout"))
      tiers.push(r.postDataJSON()?.tier);
  });
  return tiers;
}

test("shows the workspace and opens settings from the account menu", async ({
  page,
}) => {
  await signIn(page);
  await page.getByRole("button", { name: "Account" }).click();
  // The menu names the plan beside Plan & billing.
  await expect(
    page.getByRole("menuitem", { name: "Plan & billing" }),
  ).toContainText("Pro");
  // It shows this month's AI spend by task, each against its cap.
  const usage = page.getByRole("menu").locator(".wk-usage");
  await expect(usage).toContainText("This month");
  await expect(usage.locator(".wk-usage-row")).toHaveText([
    /^Loads<?\$[\d.]+ of \$100\.00$/,
    /^API sources<?\$[\d.]+ of \$20\.00$/,
    /^Questions<?\$[\d.]+ of \$30\.00$/,
  ]);
  await page.getByRole("menuitem", { name: "Settings" }).click();
  await expect(page).toHaveURL(/\/settings$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Settings" }),
  ).toBeVisible();
  await expect(page.getByRole("tab", { name: "General" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  const facts = page.getByRole("tabpanel");
  await expect(facts).toContainText("Acme");
  await expect(facts).toContainText("acme");
  // Created in the stamp's default region, one the WFE accepts.
  await expect(facts).toContainText("us-east-2");
  await expect(facts).toContainText("Owner");

  // Tabs are pages: the URL follows, and reloading keeps the tab.
  await page.getByRole("tab", { name: "Plan & billing" }).click();
  await expect(page).toHaveURL(/\/settings\/billing$/);
  await page.reload();
  await expect(
    page.getByRole("tab", { name: "Plan & billing" }),
  ).toHaveAttribute("aria-selected", "true");
  await page.goto("/settings/nowhere");
  await expect(
    page.getByRole("heading", { name: "We couldn’t find that page" }),
  ).toBeVisible();
});

test("lists members and invites people by email", async ({ page }) => {
  await signIn(page);
  await page.getByRole("button", { name: "Account" }).click();
  await page.getByRole("menuitem", { name: "Members" }).click();
  await expect(page).toHaveURL(/\/settings\/members$/);
  const members = page.getByRole("list", { name: "Members" });
  await expect(members.getByRole("listitem")).toHaveCount(2);
  // Owners first; the signed-in user is marked.
  await expect(members.getByRole("listitem").first()).toContainText(
    "Ada LovelaceYou",
  );
  await expect(members.getByRole("listitem").first()).toContainText("Owner");
  await expect(members.getByRole("listitem").nth(1)).toContainText(
    "Alan Turing",
  );
  await expect(members.getByRole("listitem").nth(1)).toContainText(
    "alan@example.com",
  );
  await expect(members.getByRole("listitem").nth(1)).toContainText("Member");

  await page.getByRole("button", { name: "Invite people" }).click();
  let dialog = page.getByRole("dialog", { name: "Invite to Acme" });
  await dialog.getByLabel("Email").fill("grace@example.com");
  await choose(dialog.getByLabel("Role"), "Admin");
  await expect(dialog).toContainText("manages billing");
  await dialog.getByRole("button", { name: "Send invitation" }).click();
  await expect(dialog).toBeHidden();
  await expect(
    toast(page, "Invitation sent to grace@example.com"),
  ).toBeVisible();

  // The WFE refuses a second invitation and an existing member.
  await page.getByRole("button", { name: "Invite people" }).click();
  dialog = page.getByRole("dialog", { name: "Invite to Acme" });
  await dialog.getByLabel("Email").fill("grace@example.com");
  await dialog.getByRole("button", { name: "Send invitation" }).click();
  await expect(dialog.getByRole("alert")).toContainText(
    "a pending invitation already exists",
  );
  await dialog.getByLabel("Email").fill("alan@example.com");
  await dialog.getByRole("button", { name: "Send invitation" }).click();
  await expect(dialog.getByRole("alert")).toContainText(
    "already a member of this workspace",
  );
  const requests = (await mock.stats()).requests.filter((r) =>
    r.includes("members:invite"),
  );
  expect(requests).toEqual([
    "POST /v1/workspaces/ws_acme/members:invite 200",
    "POST /v1/workspaces/ws_acme/members:invite 409",
    "POST /v1/workspaces/ws_acme/members:invite 409",
  ]);
});

test("a member sees members and the plan read-only", async ({ page }) => {
  await signIn(page, "alan@example.com");
  await page.goto("/settings/members");
  await expect(page.getByRole("list", { name: "Members" })).toContainText(
    "Alan TuringYou",
  );
  await expect(page.getByRole("button", { name: "Invite people" })).toHaveCount(
    0,
  );
  await expect(page.getByRole("tabpanel")).toContainText(
    "Only owners and admins can invite people.",
  );
  await page.getByRole("tab", { name: "Plan & billing" }).click();
  await expect(
    page.getByRole("heading", { name: /Foundry Pro/ }),
  ).toContainText("Active");
  await expect(page.getByRole("button", { name: "Cancel Pro" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Switch to Max" })).toHaveCount(
    0,
  );
  await expect(
    page.getByRole("button", { name: "Manage billing" }),
  ).toHaveCount(0);
  await expect(page.getByRole("tabpanel")).toContainText(
    "Only owners and admins can change the plan.",
  );
  // And the WFE refuses them, through the BFF, all the same.
  const token = await csrfToken(page);
  const invite = await page.request.post(
    "/papi/v1/workspaces/ws_acme/members:invite",
    {
      data: { email: "eve@example.com", role: "WORKSPACE_ROLE_MEMBER" },
      headers: { origin: APP_ORIGIN, "x-helix-csrf": token },
    },
  );
  expect(invite.status()).toBe(403);
  for (const tier of ["FOUNDRY_TIER_PRO", "FOUNDRY_TIER_MAX"]) {
    const checkout = await page.request.post("/papi/v1/foundry/plan:checkout", {
      data: { workspaceId: "ws_acme", clientToken: "t", tier },
      headers: { origin: APP_ORIGIN, "x-helix-csrf": token },
    });
    expect(checkout.status()).toBe(403);
  }
});

test("upgrades to Pro through checkout, cancels and resumes", async ({
  page,
}) => {
  const tiers = checkoutTiers(page);
  await signInToNewWorkspace(page);
  await page.goto("/settings/billing");
  await expect(
    page.getByRole("heading", { name: /Foundry Free/ }),
  ).toBeVisible();
  await expect(
    page.getByRole("region", { name: "Foundry Free" }),
  ).toContainText("Current plan");
  // Pro and Max side by side, with their prices.
  await expect(page.getByRole("region", { name: "Foundry Pro" })).toContainText(
    "$100per month",
  );
  await expect(page.getByRole("region", { name: "Foundry Max" })).toContainText(
    "$500per month",
  );

  await page.getByRole("button", { name: "Upgrade", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Upgrade Foundry" });
  // Pro is picked first; Max is a click away.
  await expect(dialog.getByRole("radio", { name: "Pro" })).toBeChecked();
  await expect(
    dialog.getByRole("radio", { name: "Pro" }),
  ).toHaveAccessibleDescription("$100 a month");
  await expect(
    dialog.getByRole("radio", { name: "Max" }),
  ).toHaveAccessibleDescription("$500 a month");
  await dialog.getByLabel("Promotion code").fill("NOPE");
  await dialog.getByRole("button", { name: "Continue to payment" }).click();
  await expect(dialog.getByRole("alert")).toContainText(
    "promotion code is invalid",
  );
  await dialog.getByLabel("Promotion code").fill("FOUNDRYTEST50");
  await dialog.getByRole("button", { name: "Continue to payment" }).click();

  // Stripe Checkout (the mock's), then back to /settings/billing.
  await expect(
    page.getByRole("heading", { name: "Foundry Pro" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Pay and subscribe" }).click();
  await expect(page).toHaveURL(/\/settings\/billing$/);
  await expect(
    page.getByRole("heading", { name: /Foundry Pro/ }),
  ).toContainText("Active");
  await expect(toast(page, "Globex is on Foundry Pro")).toBeVisible();
  await expect(page.getByRole("tabpanel")).toContainText("Renews on");
  await expect(page.getByRole("region", { name: "Foundry Pro" })).toContainText(
    "Current plan",
  );

  await page.getByRole("button", { name: "Cancel Pro" }).click();
  const confirm = page.getByRole("dialog", { name: "Cancel Foundry Pro?" });
  await expect(confirm).toContainText("Pro stays on until");
  await confirm.getByRole("button", { name: "Cancel Pro" }).click();
  await expect(confirm).toBeHidden();
  await expect(
    page.getByRole("heading", { name: /Foundry Pro/ }),
  ).toContainText("Ending");
  await expect(toast(page, "Foundry Pro ends on")).toBeVisible();

  await page.getByRole("button", { name: "Resume Pro" }).click();
  await expect(
    page.getByRole("heading", { name: /Foundry Pro/ }),
  ).toContainText("Active");
  const calls = (await mock.stats()).requests.filter((r) =>
    r.startsWith("POST /v1/foundry/"),
  );
  expect(calls).toEqual([
    "POST /v1/foundry/plan:checkout 400",
    "POST /v1/foundry/plan:checkout 200",
    "POST /v1/foundry/plan:cancel 200",
    "POST /v1/foundry/plan:resume 200",
  ]);
  expect(tiers).toEqual(["FOUNDRY_TIER_PRO", "FOUNDRY_TIER_PRO"]);
});

test("buys Max through checkout and shows it wherever the plan shows", async ({
  page,
}) => {
  const tiers = checkoutTiers(page);
  await signInToNewWorkspace(page);
  await page.goto("/settings/billing");
  // The Max card opens the upgrade dialog with Max picked.
  await page
    .getByRole("region", { name: "Foundry Max" })
    .getByRole("button", { name: "Choose Max" })
    .click();
  const dialog = page.getByRole("dialog", { name: "Upgrade Foundry" });
  await expect(dialog.getByRole("radio", { name: "Max" })).toBeChecked();
  await expect(dialog).toContainText("Max adds more soon.");
  await expect(dialog).toContainText("Syncs every 15 minutes");
  await dialog.getByLabel("Promotion code").fill("FOUNDRYTEST50");
  await dialog.getByRole("button", { name: "Continue to payment" }).click();

  await expect(
    page.getByRole("heading", { name: "Foundry Max" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Pay and subscribe" }).click();
  await expect(page).toHaveURL(/\/settings\/billing$/);
  await expect(
    page.getByRole("heading", { name: /Foundry Max/ }),
  ).toContainText("Active");
  await expect(toast(page, "Globex is on Foundry Max")).toBeVisible();
  await expect(page.getByRole("region", { name: "Foundry Max" })).toContainText(
    "Current plan",
  );
  await expect(
    page.getByRole("region", { name: "Foundry Pro" }),
  ).not.toContainText("Current plan");
  expect(tiers).toEqual(["FOUNDRY_TIER_MAX"]);

  // The account menu names Max too, and the user, whom the mock names from
  // their email.
  await page.getByRole("button", { name: "Account" }).click();
  await expect(page.getByRole("menu")).toContainText("Grace");
  await expect(
    page.getByRole("menuitem", { name: "Plan & billing" }),
  ).toContainText("Max");
});

test("switches Pro to Max and back, and cancels and resumes Max", async ({
  page,
}) => {
  const tiers = checkoutTiers(page);
  await signIn(page);
  await page.goto("/settings/billing");
  await expect(
    page.getByRole("heading", { name: /Foundry Pro/ }),
  ).toContainText("Active");
  await expect(
    page.getByRole("region", { name: "Foundry Pro" }),
  ).not.toContainText("Switch to");

  await page.getByRole("button", { name: "Switch to Max" }).click();
  const up = page.getByRole("dialog", { name: "Switch to Foundry Max?" });
  await expect(up).toContainText(
    "Acme moves from Pro at $100 a month to Max at $500 a month",
  );
  // Keeping Pro attaches nothing.
  await up.getByRole("button", { name: "Keep Pro" }).click();
  await expect(up).toBeHidden();
  expect(tiers).toEqual([]);

  await page.getByRole("button", { name: "Switch to Max" }).click();
  await up.getByRole("button", { name: "Switch to Max" }).click();
  // The switch answers the old plan for a moment; the page waits for Max.
  await expect(up).toBeHidden();
  await expect(
    page.getByRole("heading", { name: /Foundry Max/ }),
  ).toContainText("Active", { timeout: 15_000 });
  await expect(toast(page, "Acme is on Foundry Max")).toBeVisible();
  await expect(page.getByText("Checking your payment…")).toBeHidden();
  await expect(page.getByRole("region", { name: "Foundry Max" })).toContainText(
    "Current plan",
  );

  await page.getByRole("button", { name: "Cancel Max" }).click();
  const cancel = page.getByRole("dialog", { name: "Cancel Foundry Max?" });
  await expect(cancel).toContainText("Max stays on until");
  await cancel.getByRole("button", { name: "Cancel Max" }).click();
  await expect(cancel).toBeHidden();
  await expect(
    page.getByRole("heading", { name: /Foundry Max/ }),
  ).toContainText("Ending");
  await expect(toast(page, "Foundry Max ends on")).toBeVisible();
  await page.getByRole("button", { name: "Resume Max" }).click();
  await expect(
    page.getByRole("heading", { name: /Foundry Max/ }),
  ).toContainText("Active");
  await expect(toast(page, "Acme will stay on Foundry Max")).toBeVisible();

  await page.getByRole("button", { name: "Switch to Pro" }).click();
  const down = page.getByRole("dialog", { name: "Switch to Foundry Pro?" });
  await expect(down).toContainText(
    "Acme moves from Max at $500 a month to Pro at $100 a month",
  );
  await down.getByRole("button", { name: "Switch to Pro" }).click();
  await expect(
    page.getByRole("heading", { name: /Foundry Pro/ }),
  ).toContainText("Active", { timeout: 15_000 });
  await expect(toast(page, "Acme is on Foundry Pro")).toBeVisible();

  expect(tiers).toEqual(["FOUNDRY_TIER_MAX", "FOUNDRY_TIER_PRO"]);
  const calls = (await mock.stats()).requests.filter((r) =>
    r.startsWith("POST /v1/foundry/"),
  );
  expect(calls).toEqual([
    "POST /v1/foundry/plan:checkout 200",
    "POST /v1/foundry/plan:cancel 200",
    "POST /v1/foundry/plan:resume 200",
    "POST /v1/foundry/plan:checkout 200",
  ]);
});

test("waits for Pro when a card on file is charged", async ({ page }) => {
  await mock.configure({ checkoutCardOnFile: true });
  await signInToNewWorkspace(page);
  await page.goto("/settings/billing");
  await page.getByRole("button", { name: "Upgrade", exact: true }).click();
  await page
    .getByRole("dialog", { name: "Upgrade Foundry" })
    .getByRole("button", { name: "Continue to payment" })
    .click();
  // The charge answers Free for a moment; the page keeps reading the plan.
  await expect(page).toHaveURL(/\/settings\/billing$/);
  await expect(page.getByText("Checking your payment…")).toBeVisible();
  await expect(
    page.getByRole("heading", { name: /Foundry Pro/ }),
  ).toContainText("Active", { timeout: 15_000 });
  await expect(page.getByText("Checking your payment…")).toBeHidden();
  await expect(toast(page, "Globex is on Foundry Pro")).toBeVisible();
});

test("opens the billing portal and comes back", async ({ page }) => {
  await signIn(page);
  await page.goto("/settings/billing");
  await page.getByRole("button", { name: "Manage billing" }).click();
  await expect(
    page.getByRole("heading", { name: "Mock billing portal" }),
  ).toBeVisible();
  await expect(page.getByRole("main")).toContainText("Acme");
  await page.getByRole("link", { name: "Return to Helix Foundry" }).click();
  await expect(page).toHaveURL(/\/settings\/billing$/);
  await expect(
    page.getByRole("heading", { name: /Foundry Pro/ }),
  ).toContainText("Active");
});

test("dialogs open on their first field, or on Cancel before a deletion", async ({
  page,
}) => {
  await signIn(page);
  await page.getByRole("button", { name: "Workspace: Acme" }).click();
  await page.getByRole("menuitem", { name: "Create workspace" }).click();
  const create = page.getByRole("dialog", { name: "Create a workspace" });
  await page.keyboard.type("Globex");
  await expect(create.getByLabel("Workspace name")).toHaveValue("Globex");
  await page.keyboard.press("Escape");

  await page.goto("/settings/members");
  await page.getByRole("button", { name: "Invite people" }).click();
  const invite = page.getByRole("dialog", { name: "Invite to Acme" });
  await page.keyboard.type("grace@example.com");
  await expect(invite.getByLabel("Email")).toHaveValue("grace@example.com");
  await page.keyboard.press("Escape");

  await page.goto("/data/connections/conn_acme_shop");
  await page.getByRole("button", { name: "Connection actions" }).click();
  await page.getByRole("menuitem", { name: "Delete connection" }).click();
  await expect(
    page.getByRole("dialog").getByRole("button", { name: "Cancel" }),
  ).toBeFocused();
});

test("creates a workspace whose name another one already took", async ({
  page,
}) => {
  // Acme's slug, "acme", belongs to Ada's workspace.
  await signInToNewWorkspace(page, "Acme");
  await page.goto("/settings");
  await expect(page.getByText(/^acme-[a-z0-9]{4}$/)).toBeVisible();
  const posted = (await mock.stats()).requests.filter((r) =>
    r.startsWith("POST /v1/workspaces"),
  );
  expect(posted).toEqual([
    "POST /v1/workspaces 409",
    "POST /v1/workspaces 200",
  ]);
});

test("creates workspaces only in a region the WFE accepts", async ({
  page,
}) => {
  await mock.configure({ allowedRegions: ["us-east-1"] });
  await signIn(page);
  await page.getByRole("button", { name: "Workspace: Acme" }).click();
  await page.getByRole("menuitem", { name: "Create workspace" }).click();
  const dialog = page.getByRole("dialog", { name: "Create a workspace" });
  await dialog.getByLabel("Workspace name").fill("Initech");
  await dialog.getByRole("button", { name: "Create workspace" }).click();
  await expect(dialog.getByRole("alert")).toContainText(
    'region "us-east-2" is not enabled',
  );
});

test("never forwards role changes, removals or invitation management", async ({
  page,
}) => {
  await signIn(page);
  const token = await csrfToken(page);
  const headers = { origin: APP_ORIGIN, "x-helix-csrf": token };
  const role = await page.request.patch(
    "/papi/v1/workspaces/ws_acme/members/usr_alan",
    { data: { role: "WORKSPACE_ROLE_ADMIN" }, headers },
  );
  expect(role.status()).toBe(404);
  const remove = await page.request.delete(
    "/papi/v1/workspaces/ws_acme/members/usr_alan",
    { headers },
  );
  expect(remove.status()).toBe(404);
  const pending = await page.request.get(
    "/papi/v1/workspaces/ws_acme/invitations",
  );
  expect(pending.status()).toBe(404);
  const members = await page.request.post(
    "/papi/v1/workspaces/ws_acme/members",
    { data: {}, headers },
  );
  expect(members.status()).toBe(405);
  expect(members.headers()["allow"]).toBe("GET");
  const reached = (await mock.stats()).requests.join("\n");
  expect(reached).not.toMatch(
    /PATCH|DELETE|invitations|POST \/v1\/workspaces\/ws_acme\/members /,
  );
});
