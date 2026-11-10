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
