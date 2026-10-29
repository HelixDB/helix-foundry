import type { Page } from "@playwright/test";
import {
  choose,
  expect,
  mock,
  signIn,
  signInToNewWorkspace,
  test,
} from "../e2e/support/mock";
import {
  ask,
  caption,
  click,
  loadContract,
  poster,
  saveVideo,
  shot,
  type,
  videoPage,
} from "./support";

// docs: foundry/start-here (sign in, workspaces, the main page and the
// sidebar, a new workspace's first steps) and foundry/workspace (members,
// plans and billing, AI models, usage).

const signInPage = (page: Page) =>
  expect(
    page.getByRole("button", { name: "Continue with GitHub" }),
  ).toBeVisible();

test("workspace: sign in, choose a workspace, find your way", async ({
  page,
}) => {
  await page.goto("/login");
  await signInPage(page);
  await shot(page, "start-here", "sign-in");

  // The main page: Ask, on the ontology last opened, its threads in the
  // sidebar once it is loaded and asked about.
  await mock.configure({ loadStepMs: 200, askStepMs: 200 });
  await loadContract(page);
  await page.goto("/");
  await ask(page, "Who signed the services agreement?");
  await ask(page, "Which customers renew this quarter?", true);
  await page.getByRole("link", { name: "New thread" }).click();
  await ask(page, "How much did Globex pay us last month?");
  await page.getByRole("link", { name: "New thread" }).click();
  await expect(
    page
      .getByRole("group", { name: "Suggested questions" })
      .getByRole("button")
      .first(),
  ).toBeVisible();
  await shot(page, "start-here", "home");

  // The account menu, with this month's usage.
  await page.getByRole("button", { name: "Account" }).click();
  await expect(
    page.getByRole("menuitemradio", { name: "System" }),
  ).toBeVisible();
  // Read when the menu opens: prod's caps, and what the load and the
  // questions spent.
  await expect(page.getByRole("menu")).toContainText("of $50.00");
  await shot(page, "start-here", "account-menu");
  await shot(page, "workspace", "usage", page.getByRole("menu"));
  await page.keyboard.press("Escape");
  await page.keyboard.press("ControlOrMeta+k");
  await page
    .getByRole("dialog", { name: "Search your workspace" })
    .getByRole("combobox")
    .fill("cus");
  await page.waitForTimeout(600);
  await shot(page, "start-here", "command-palette");
  await page.keyboard.press("Escape");

  await page.goto("/workspaces");
  await expect(
    page.getByRole("button", { name: "Create workspace" }),
  ).toBeVisible();
  await shot(page, "start-here", "workspaces");
  await page.getByRole("button", { name: "Create workspace" }).click();
  await page
    .getByRole("dialog", { name: "Create a workspace" })
    .getByLabel("Workspace name")
    .fill("Globex");
  await shot(page, "start-here", "create-workspace");
});

test("workspace: the rail and a panel on a phone", async ({ page }) => {
  await signIn(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/data");
  await page.getByRole("button", { name: "Open navigation" }).click();
  await expect(
    page.getByRole("complementary", { name: "Connectors explorer" }),
  ).toBeVisible();
  await shot(page, "start-here", "phone-drawer");
});

test("workspace: settings, members and Pro", async ({ page }) => {
  await signIn(page);
  await page.goto("/settings");
  await expect(
    page.getByRole("heading", { level: 1, name: "Settings" }),
  ).toBeVisible();
  await shot(page, "workspace", "settings");
  await page.goto("/settings/members");
  await expect(
    page.getByRole("button", { name: "Invite people" }),
  ).toBeVisible();
  await shot(page, "workspace", "members");
  await page.getByRole("button", { name: "Invite people" }).click();
  const invite = page.getByRole("dialog", { name: "Invite to Acme" });
  await invite.getByLabel("Email").fill("grace@example.com");
  await choose(invite.getByLabel("Role"), "Admin");
  await shot(page, "workspace", "invite");
  await page.keyboard.press("Escape");

  await page.goto("/settings/billing");
  await expect(page.getByRole("button", { name: "Cancel Pro" })).toBeVisible();
  await shot(page, "workspace", "billing-pro");

  // AI models: the default, and Claude with the workspace's own key, the
  // one kind of key prod takes (foundry.ai.kinds).
  await mock.configure({ aiKinds: ["FOUNDRY_AI_KIND_ANTHROPIC"] });
  await page.goto("/settings/ai");
  await expect(
    page.getByRole("radio", { name: "Default", exact: true }),
  ).toBeChecked();
  await shot(page, "workspace", "ai-models");
  await page.getByRole("radio", { name: "Claude", exact: true }).check();
  await expect(page.getByLabel("API key")).toBeVisible();
  await shot(page, "workspace", "ai-models-claude");
});

test("workspace: a new workspace's first steps, and upgrading it", async ({
  page,
}) => {
  await signInToNewWorkspace(page);
  // Set up with your agent: a prompt to copy for the user's own agent.
  await expect(page.getByRole("button", { name: "Copy prompt" })).toBeVisible();
  await shot(page, "start-here", "onboarding");
  await page.goto("/settings/billing");
  await expect(
    page.getByRole("heading", { name: /Foundry Free/ }),
  ).toBeVisible();
  await shot(page, "workspace", "billing-free");
  await page.getByRole("button", { name: "Upgrade", exact: true }).click();
  await expect(
    page.getByRole("dialog", { name: "Upgrade Foundry" }),
  ).toBeVisible();
  await shot(page, "workspace", "upgrade");
});

test("workspace: video", async ({ browser }) => {
  const page = await videoPage(
    browser,
    "Set up a workspace",
    "grace@example.com",
  );
  await page.goto("/workspaces");
  await caption(page, "A workspace holds your data, ontologies and team");
  await page.waitForTimeout(1500);
  await click(page.getByRole("button", { name: "Create workspace" }));
  await type(
    page
      .getByRole("dialog", { name: "Create a workspace" })
      .getByLabel("Workspace name"),
    "Globex",
  );
  await click(
    page
      .getByRole("dialog", { name: "Create a workspace" })
      .getByRole("button", { name: "Create workspace" }),
  );
  await expect(
    page.getByRole("heading", { level: 1, name: "Get started" }),
  ).toBeVisible();
  await caption(
    page,
    "A new workspace opens on Get started: a sample, your own data or your agent",
    2500,
  );
  await caption(page, "Invite the people you work with from Settings");
  await click(page.getByRole("button", { name: "Account" }));
  await click(page.getByRole("menuitem", { name: /^Plan & billing/ }));
  await click(page.getByRole("tab", { name: "Members" }));
  await click(page.getByRole("button", { name: "Invite people" }));
  const invite = page.getByRole("dialog", { name: "Invite to Globex" });
  await type(invite.getByLabel("Email"), "alan@example.com");
  await click(invite.getByRole("button", { name: "Send invitation" }));
  await page.waitForTimeout(1500);
  await caption(
    page,
    "Every plan has every connector; paid plans sync more often",
  );
  await click(page.getByRole("tab", { name: "Plan & billing" }));
  await page.waitForTimeout(1800);
  await click(page.getByRole("button", { name: "Upgrade", exact: true }));
  await caption(page, "Choose Pro or Max, then pay with Stripe Checkout");
  await page.waitForTimeout(1500);
  await poster(page, "workspace", "set-up-a-workspace");
  await page.waitForTimeout(1500);
  await saveVideo(page, "workspace", "set-up-a-workspace");
});
