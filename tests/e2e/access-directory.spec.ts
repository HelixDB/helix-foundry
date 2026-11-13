import { expect, mock, signIn, test } from "./support/mock";

// Teams synced from a directory (SCIM through WorkOS) on Settings › Access:
// their name and members are the group's, so they offer only Sources, and
// giving them sources leaves out whom those can't confine (Alan queries
// databases) instead of refusing. Restricted members who hold nothing, as
// leaving a team that held sources keeps them, are a filter of People, each
// lifted in one click. `directoryTeams` syncs Acme's Okta directory:
// Engineering (Ada and Alan) and Finance (Alan and Eve).

test("a synced team offers only its sources, and leaves out whom they can't confine", async ({
  page,
}) => {
  await mock.configure({ directoryTeams: true });
  await signIn(page);
  await page.goto("/settings/access?tab=teams");
  const finance = page.getByRole("listitem", { name: "Finance" });
  await expect(finance).toContainText("Synced from Okta");
  await expect(finance).toContainText(
    "Members come from Okta; change them there.",
  );
  await expect(finance).toContainText("Alan Turing, Eve Adler");
  await expect(
    finance.getByRole("button", { name: "Edit Finance" }),
  ).toHaveCount(0);
  await expect(
    finance.getByRole("button", { name: "Delete Finance" }),
  ).toHaveCount(0);
  // Connecting the directory and roles from its groups are the dashboard's.
  await expect(
    page.getByRole("link", { name: "Helix dashboard’s Settings" }),
  ).toHaveAttribute("href", /\/acme\/settings$/);

  await finance.getByRole("button", { name: "Sources for Finance" }).click();
  const sources = page.getByRole("dialog", { name: "Sources for Finance" });
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  // Eve is restricted already; Alan is left out, and it says why first.
  const confirm = page.getByRole("dialog", {
    name: "Leave Alan Turing out of Finance?",
  });
  await expect(confirm).toContainText(
    "Alan Turing is left out of the team while it holds sources",
  );
  await expect(confirm).toContainText("outside Foundry");
  await confirm.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved what Finance can read.")).toBeVisible();
  await sources.getByRole("button", { name: "Close" }).first().click();

  await expect(finance).toContainText(
    "Left out: Alan Turing, who writes a project or queries its databases outside Foundry, so the team’s sources can’t confine them.",
  );
  await expect(finance).not.toContainText("Alan Turing, Eve Adler");
  // Alan isn't restricted by it.
  await page.goto("/settings/access?tab=people");
  await expect(
    page.getByRole("listitem", { name: "Alan Turing" }),
  ).toContainText("Writes a project or queries its databases");
  await expect(
    page.getByRole("switch", { name: "Restrict Alan Turing" }),
  ).not.toBeChecked();
});

test("a restricted member who holds nothing is lifted in one click", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/settings/access?tab=people");
  const filter = page.getByRole("radiogroup", { name: "Show" });
  // Eve holds General, so there's no one to lift yet.
  await filter
    .getByRole("radio", { name: /Restricted, holds nothing/ })
    .click();
  await expect(page.getByText("No one to lift")).toBeVisible();

  await filter.getByRole("radio", { name: "Everyone" }).click();
  await page.getByRole("button", { name: "Sources for Eve Adler" }).click();
  const sources = page.getByRole("dialog", { name: "Sources for Eve Adler" });
  await sources.getByRole("checkbox", { name: /^General/ }).uncheck();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved what Eve Adler can read.")).toBeVisible();
  await sources.getByRole("button", { name: "Close" }).first().click();

  await filter
    .getByRole("radio", { name: /Restricted, holds nothing/ })
    .click();
  const idle = page.getByRole("list", { name: "Restricted, holds nothing" });
  await expect(idle.getByRole("listitem")).toHaveCount(1);
  await expect(idle.getByRole("listitem", { name: "Eve Adler" })).toBeVisible();
  await idle
    .getByRole("button", { name: "Lift the restriction on Eve Adler" })
    .click();
  await expect(
    page.getByText("Eve Adler sees everything again."),
  ).toBeVisible();
  await expect(page.getByText("No one to lift")).toBeVisible();
  await filter.getByRole("radio", { name: "Everyone" }).click();
  await expect(
    page.getByRole("switch", { name: "Restrict Eve Adler" }),
  ).not.toBeChecked();
});

// Directory sync above the teams: an owner connects the directory through
// WorkOS's Admin Portal (the mock's /mock/directory-portal), its groups become
// teams, and the teams that read nothing yet ask for sources first.
test("an owner connects a directory, and its teams ask for sources", async ({
  page,
}) => {
  await signIn(page);
  const read = page.waitForResponse((r) =>
    r.url().endsWith("/papi/v1/workspaces/ws_acme/directory"),
  );
  await page.goto("/settings/access?tab=teams");
  await read;
  await expect(
    page.getByRole("heading", { name: "Teams", exact: true }),
  ).toBeVisible();
  // Not entitled to SSO: nothing about directories.
  await expect(
    page.getByRole("heading", { name: "Directory sync" }),
  ).toHaveCount(0);

  await mock.configure({ directoryEntitled: true, directorySyncMs: 300 });
  await page.reload();
  const panel = page
    .locator(".hf-panel")
    .filter({ has: page.getByRole("heading", { name: "Directory sync" }) });
  await expect(panel).toContainText("Connect your company’s directory");
  const opened = page.waitForEvent("popup");
  await panel.getByRole("button", { name: "Connect your directory" }).click();
  const portal = await opened;
  await expect(
    portal.getByRole("heading", { name: "Set up directory sync" }),
  ).toBeVisible();
  await portal.getByRole("button", { name: "Connect Okta" }).click();
  await expect(
    portal.getByRole("heading", { name: "Okta is connected" }),
  ).toBeVisible();
  await expect(
    portal.getByRole("link", { name: "Return to Helix Foundry" }),
  ).toHaveAttribute("href", /\/settings\/access\?tab=teams$/);
  await portal.close();

  // The panel reads again until the directory shows.
  await expect(panel).toContainText("Connected", { timeout: 10_000 });
  await expect(panel).toContainText("3 people · 2 groups");
  await expect(
    panel.getByRole("link", { name: "Helix dashboard’s Settings" }),
  ).toHaveAttribute("href", /\/acme\/settings$/);
  await expect(page.getByRole("listitem", { name: "Finance" })).toContainText(
    "Synced from Okta",
  );

  const nudge = page
    .getByRole("status")
    .filter({ hasText: "were synced from Okta but read nothing yet" });
  await expect(nudge).toContainText("Engineering and Finance");
  await nudge
    .getByRole("button", { name: "Choose what Engineering reads" })
    .click();
  const sources = page.getByRole("dialog", { name: "Sources for Engineering" });
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  await page
    .getByRole("dialog", { name: "Leave Alan Turing out of Engineering?" })
    .getByRole("button", { name: "Save" })
    .click();
  await expect(
    page.getByText("Saved what Engineering can read."),
  ).toBeVisible();
  await sources.getByRole("button", { name: "Close" }).first().click();
  await expect(
    page.getByRole("status").filter({ hasText: "synced from Okta" }),
  ).toContainText("Finance was synced from Okta but reads nothing yet");

  await panel.getByRole("button", { name: "Sync now" }).click();
  await expect(panel.getByRole("button", { name: "Syncing…" })).toBeDisabled();
  await expect(panel.getByRole("button", { name: "Sync now" })).toBeEnabled({
    timeout: 10_000,
  });
  await expect(panel).toContainText("Synced just now");
});

test("an admin sees directory sync and what needs attention, without its buttons", async ({
  page,
}) => {
  await mock.configure({
    directoryTeams: true,
    alanRole: "WORKSPACE_ROLE_ADMIN",
    directoryIssues: [
      {
        kind: "DIRECTORY_ISSUE_KIND_NOT_PROVISIONED",
        displayName: "Bea Guest",
      },
    ],
  });
  await signIn(page, "alan@example.com");
  await page.goto("/settings/access?tab=teams");
  const panel = page
    .locator(".hf-panel")
    .filter({ has: page.getByRole("heading", { name: "Directory sync" }) });
  await expect(panel).toContainText("Okta · Acme");
  await expect(
    panel.getByRole("list", { name: "Needs attention" }),
  ).toContainText("1 person in Okta has no Helix account yet.");
  await expect(panel).toContainText(
    "Only workspace owners can sync or change the directory.",
  );
  await expect(panel.getByRole("button")).toHaveCount(0);
});

test("a directory chosen in the setup guide that never linked reads as unfinished, not broken", async ({
  page,
}) => {
  await mock.configure({ directorySettingUp: true });
  await signIn(page);
  await page.goto("/settings/access?tab=teams");
  const status = page.getByLabel("Directory status");
  await expect(status).toContainText("your directory · Acme");
  await expect(status).toContainText("Setup not finished");
  await expect(status).toContainText(
    "Finish connecting it in the setup guide. Its groups and people show here once it sends them.",
  );
  await expect(status).toContainText("Not synced yet · 0 people · 0 groups");
  await expect(status).not.toContainText("Disconnected");
  await expect(status).not.toContainText("failed");
  await expect(status.getByRole("alert")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Open setup guide" }),
  ).toBeVisible();
});
