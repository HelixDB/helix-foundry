import { readFileSync } from "node:fs";
import type { Browser, Page } from "@playwright/test";
import { expect, explorer, mock, signIn, test } from "./support/mock";

// Settings › Access and restricted mode: an owner makes an agent's key and
// gives it sources, then sees what it can read; restricts a member, who
// then sees only Analyst and the ontologies their sources cover and calls
// nothing the WFE refuses; taking a source away takes the ontology away;
// and a change that would cut someone off asks first. Eve is a member of
// Acme restricted to the General set; Alan, a member who queries databases,
// can't be restricted.

const O = "/ontology/ont_acme_contracts";
const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
);

// Uploads the contract into General, makes version 3 current and loads it:
// Customer contracts' current load reads Stripe's customers and invoices
// and General.
async function loadContract(page: Page) {
  await page.goto("/data");
  await explorer(page).getByLabel("Choose documents").setInputFiles({
    name: "contract.md",
    mimeType: "text/markdown",
    buffer: CONTRACT,
  });
  await expect(
    page
      .getByRole("list", { name: "Uploads" })
      .getByRole("listitem", { name: "contract.md" }),
  ).toContainText("Uploaded");
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
  await page.goto(O);
  await page
    .getByRole("status")
    .filter({ hasText: "Version 3 isn’t loaded yet" })
    .getByRole("button", { name: "Load data" })
    .click();
  await page
    .getByRole("dialog", { name: "Load version 3 into contracts?" })
    .getByRole("button", { name: "Load" })
    .click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 20_000 });
}

/** Opens a principal's sources from its row on Settings › Access. */
async function openSources(page: Page, tab: string, name: string) {
  await page.goto(`/settings/access?tab=${tab}`);
  await page.getByRole("button", { name: `Sources for ${name}` }).click();
  return page.getByRole("dialog", { name: `Sources for ${name}` });
}

/** Gives Eve Stripe beside her General: she reads Customer contracts. */
async function shareWithEve(page: Page) {
  const sources = await openSources(page, "people", "Eve Adler");
  await expect(
    sources.getByRole("checkbox", { name: /^General/ }),
  ).toBeChecked();
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(sources.getByRole("list", { name: "Can read" })).toContainText(
    "Customer contracts",
  );
  await sources.getByRole("button", { name: "Close" }).first().click();
}

/** Eve, signed in in a browser of her own. */
async function asEve(browser: Browser) {
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  const page = await context.newPage();
  await signIn(page, "eve@example.com");
  return page;
}

test("an agent's key reads the ontologies its sources cover", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  await page.goto("/settings/access");
  await expect(page.getByRole("tab", { name: "Agents" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  // Acme's agent's key holds nothing yet.
  await expect(
    page.getByRole("listitem", { name: "Support agent" }),
  ).toContainText("Reads nothing yet");

  await page.getByRole("button", { name: "New key" }).click();
  const create = page.getByRole("dialog", { name: "New agent key" });
  await create.getByLabel("Name").fill("Billing bot");
  await create.getByRole("button", { name: "Create key" }).click();
  const made = page.getByRole("dialog", { name: "Key for Billing bot" });
  await expect(made.getByRole("textbox", { name: "Agent key" })).toHaveValue(
    /^hlxs_[0-9a-f]+$/,
  );
  await made.getByRole("button", { name: "Done" }).click();

  const sources = await openSources(page, "agents", "Billing bot");
  await expect(sources.getByRole("list", { name: "Can read" })).toHaveCount(0);
  await expect(
    sources.getByRole("list", { name: "Can’t read yet" }),
  ).toContainText("Customer contracts");
  // Stripe covers its tables: they show chosen and can't be changed alone.
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await expect(
    sources.getByRole("checkbox", { name: "invoices", exact: true }),
  ).toBeChecked();
  await expect(
    sources.getByRole("checkbox", { name: "invoices", exact: true }),
  ).toBeDisabled();
  await sources.getByRole("checkbox", { name: /^General/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(sources.getByRole("list", { name: "Can read" })).toContainText(
    "Customer contracts",
  );
  await sources.getByRole("button", { name: "Close" }).first().click();
  await expect(
    page.getByRole("listitem", { name: "Billing bot" }),
  ).toContainText("Reads Customer contracts");

  // Revoked, it goes with its sources.
  await page.getByRole("button", { name: "Revoke Billing bot" }).click();
  await page
    .getByRole("dialog", { name: "Revoke Billing bot?" })
    .getByRole("button", { name: "Revoke" })
    .click();
  await expect(page.getByRole("listitem", { name: "Billing bot" })).toHaveCount(
    0,
  );
});

test("a restricted member sees only what their sources cover", async ({
  page,
  browser,
}) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 100 });
  await signIn(page);
  await loadContract(page);
  // Eve is restricted; Alan queries databases, so he can't be.
  await page.goto("/settings/access?tab=people");
  const eve = page.getByRole("switch", { name: "Restrict Eve Adler" });
  await expect(eve).toBeChecked();
  await expect(
    page.getByRole("switch", { name: "Restrict Alan Turing" }),
  ).toBeDisabled();
  await expect(
    page.getByRole("listitem", { name: "Alan Turing" }),
  ).toContainText("queries its databases outside Foundry");
  await expect(
    page.getByRole("switch", { name: "Restrict Ada Lovelace" }),
  ).toHaveCount(0);
  // Lifted and put back.
  await eve.uncheck();
  await expect(page.getByRole("listitem", { name: "Eve Adler" })).toContainText(
    "Sees everything",
  );
  await eve.check();
  await expect(page.getByRole("listitem", { name: "Eve Adler" })).toContainText(
    "Reads nothing yet",
  );
  await shareWithEve(page);
  await page.goto("/settings/access?tab=people");
  await expect(page.getByRole("listitem", { name: "Eve Adler" })).toContainText(
    "Reads Customer contracts",
  );
  await page.close();

  const before = (await mock.stats()).requests.length;
  const as = await asEve(browser);
  // Analyst opens on the one ontology shared with her.
  await expect(as).toHaveURL(/\/ask\/ont_acme_contracts/);
  const nav = as.getByRole("list", { name: "Workspace" });
  await expect(nav.getByRole("link")).toHaveText(["Analyst", "Ontologies"]);
  await expect(as.getByRole("link", { name: "Settings" })).toHaveCount(0);
  await expect(as.getByRole("button", { name: "Search" })).toHaveCount(0);
  await expect(
    as.getByRole("combobox", { name: "Load", exact: true }),
  ).toHaveCount(0);

  // She asks, and the answer's sources open their steps, not pages.
  const form = as.getByRole("form", { name: "Ask a question" });
  await form
    .getByRole("textbox", { name: /^Question/ })
    .fill("Who signed the services agreement?");
  await form.getByRole("button", { name: "Ask", exact: true }).click();
  const answer = as.getByRole("region", { name: "Answer" });
  await expect(answer).toBeVisible({ timeout: 20_000 });
  await expect(answer.getByRole("link")).toHaveCount(0);
  await expect(as.getByRole("group", { name: "Questions budget" })).toHaveCount(
    0,
  );

  // The Ontologies panel lists only what is shared; the overview is the
  // schema alone.
  await as.goto("/ontology");
  await expect(as).toHaveURL(new RegExp(`${O}$`));
  const panel = as.getByRole("complementary", { name: "Ontologies" });
  const listed = panel.getByRole("list", { name: "Ontologies" });
  await expect(listed).toContainText("Customer contracts");
  await expect(listed).not.toContainText("Sales graph");
  await expect(listed).not.toContainText("Renewals");
  await expect(panel.getByRole("link", { name: "Documents" })).toHaveCount(0);
  await expect(
    as.getByRole("heading", { level: 1, name: "Customer contracts" }),
  ).toBeVisible();
  await expect(as.getByText("Shared with you: the types")).toBeVisible();
  await expect(as.getByRole("button", { name: "Load data" })).toHaveCount(0);
  await expect(as.getByRole("link", { name: "Generate" })).toHaveCount(0);
  await expect(as.getByRole("button", { name: "Delete ontology" })).toHaveCount(
    0,
  );

  // Everything else opens Analyst.
  for (const path of [
    "/data",
    "/explore/ont_acme_contracts",
    "/settings",
    "/settings/access",
    `${O}/loads`,
    "/ontology/new",
  ]) {
    await as.goto(path);
    await expect(as, path).toHaveURL(/\/ask(\/|\?|$)|\/ontology\/ont_/);
  }
  await as.getByRole("button", { name: "Account" }).click();
  await expect(as.getByRole("menuitem", { name: "Settings" })).toHaveCount(0);
  await expect(
    as.getByRole("menuitem", { name: "Plan & billing" }),
  ).toHaveCount(0);
  await as.keyboard.press("Escape");

  // Nothing she did asked for what the WFE refuses her.
  const refused = (await mock.stats()).requests
    .slice(before)
    .filter((r) => / 40[34]$/.test(r));
  expect(refused).toEqual([]);
});

test("taking a source away takes the ontology away", async ({
  page,
  browser,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  await shareWithEve(page);
  const sources = await openSources(page, "people", "Eve Adler");
  await sources.getByRole("checkbox", { name: /^Stripe/ }).uncheck();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(
    sources.getByRole("list", { name: "Can’t read yet" }),
  ).toContainText("Stripe › customers");
  await page.close();

  const before = (await mock.stats()).requests.length;
  const as = await asEve(browser);
  await expect(
    as.getByRole("heading", { name: "Nothing shared with you yet" }),
  ).toBeVisible();
  expect(
    (await mock.stats()).requests.slice(before).filter((r) => / 403$/.test(r)),
  ).toEqual([]);
  await as.goto(O);
  await expect(
    as.getByRole("heading", { name: "This ontology doesn’t exist" }),
  ).toBeVisible();
});

test("asks before a change that would cut a grantee off", async ({ page }) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await loadContract(page);
  await shareWithEve(page);

  // Version 2 reads the Shop database, which Eve isn't given: making it
  // current asks first, and Cancel leaves it as it is.
  await page.goto(`${O}/versions/ver_seed_2`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 2 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  const warning = page.getByRole("dialog", {
    name: "Some people will stop reading",
  });
  await expect(
    warning.getByRole("list", { name: "Who stops reading" }),
  ).toHaveText("Eve Adler: Customer contracts");
  await warning.getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByText("Version 2 is now current.")).toHaveCount(0);

  // Continue makes it current; loading it asks again.
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 2 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await warning.getByRole("button", { name: "Continue" }).click();
  await expect(page.getByText("Version 2 is now current.")).toBeVisible();
  await page.goto(O);
  await page.getByRole("button", { name: "Load data" }).first().click();
  await page
    .getByRole("dialog", { name: /^Load version 2/ })
    .getByRole("button", { name: "Load" })
    .click();
  await expect(
    warning.getByRole("list", { name: "Who stops reading" }),
  ).toHaveText("Eve Adler: Customer contracts");
  await warning.getByRole("button", { name: "Cancel" }).click();
  await expect(page).toHaveURL(new RegExp(`${O}$`));
});
