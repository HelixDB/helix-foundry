import type { Locator, Page } from "@playwright/test";
import {
  choose,
  createFirstOntology,
  csrfToken,
  expect,
  mock,
  signIn,
  signInToNewWorkspace,
  test,
} from "./support/mock";

// The workspace picker, the switcher and the shell's navigation: Ask, the
// main page, and the Explorer as sections of their own, on the ontology
// last opened, and their Ontology pickers; and the labelled sidebar on
// every page, Ask's threads under its sections and a section's panel beside
// it.

const CONTRACTS = "ont_acme_contracts";

/** The sidebar's section links, by their names. */
const sectionNames = (nav: Locator) =>
  nav
    .getByRole("link")
    .evaluateAll((links) => links.map((l) => l.textContent?.trim()));

/** A second ontology of Acme's, Orders: the newest, so the list's first. */
async function addOrders(page: Page): Promise<string> {
  const res = await page.request.post(
    "/papi/v1/workspaces/ws_acme/foundry/ontologies",
    {
      data: {
        name: "Orders",
        tenantId: "ten_acme_orders",
        clientToken: "e2e-orders",
      },
      headers: {
        origin: new URL(page.url()).origin,
        "x-helix-csrf": await csrfToken(page),
      },
    },
  );
  expect(res.status()).toBe(200);
  return (await res.json()).ontology.id;
}

test("a new user creates their first workspace", async ({ page }) => {
  await signIn(page, "grace@example.com");
  await expect(
    page.getByRole("heading", { name: "Create your first workspace" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Create workspace" }).click();
  const dialog = page.getByRole("dialog", { name: "Create a workspace" });
  await dialog.getByLabel("Workspace name").fill("Globex Corp");
  await expect(dialog).toContainText("globex-corp");
  await dialog.getByRole("button", { name: "Create workspace" }).click();
  // It opens on its onboarding: the wizard for its first ontology.
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Create your first ontology" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Workspace: Globex Corp" }),
  ).toBeVisible();
  // Created through the BFF, in the stamp's default region.
  expect((await mock.stats()).requests).toContain("POST /v1/workspaces 200");
});

test("shows the WFE's reason when a workspace cannot be created", async ({
  page,
}) => {
  await signIn(page);
  // A taken slug is retried once with a suffix; when that is refused too,
  // the dialog says why.
  const sent: string[] = [];
  await page.route("**/papi/v1/workspaces", (route) => {
    if (route.request().method() !== "POST") return route.continue();
    sent.push(route.request().postDataJSON().slug);
    return route.fulfill({
      status: 409,
      contentType: "application/json",
      body: JSON.stringify({
        code: 6,
        message: "workspace already exists",
        details: [],
      }),
    });
  });
  await page.getByRole("button", { name: "Workspace: Acme" }).click();
  await page.getByRole("menuitem", { name: "Create workspace" }).click();
  const dialog = page.getByRole("dialog", { name: "Create a workspace" });
  await dialog.getByLabel("Workspace name").fill("Acme");
  await dialog.getByRole("button", { name: "Create workspace" }).click();
  await expect(dialog.getByRole("alert")).toContainText("already exists");
  expect(sent).toEqual(["acme", expect.stringMatching(/^acme-[a-z0-9]{4}$/)]);
});

test("creates, switches and chooses workspaces", async ({ page }) => {
  await signIn(page);
  // Acme opens on its main page, Ask.
  const ask = page.getByRole("heading", { level: 1, name: "Analyst" });
  await expect(page).toHaveURL(new RegExp(`/ask/${CONTRACTS}$`));
  await expect(ask).toBeVisible();
  await page.getByRole("button", { name: "Workspace: Acme" }).click();
  await page.getByRole("menuitem", { name: "Create workspace" }).click();
  const dialog = page.getByRole("dialog", { name: "Create a workspace" });
  await dialog.getByLabel("Workspace name").fill("Initech");
  await dialog.getByRole("button", { name: "Create workspace" }).click();
  // Without an ontology, Initech opens on its onboarding wizard.
  const wizard = page.getByRole("heading", {
    level: 1,
    name: "Create your first ontology",
  });
  await expect(wizard).toBeVisible();

  await page.getByRole("button", { name: "Workspace: Initech" }).click();
  await page.getByRole("menuitemradio", { name: "Acme" }).click();
  await expect(page).toHaveURL(new RegExp(`/ask/${CONTRACTS}$`));
  await expect(ask).toBeVisible();

  // The choice is remembered per browser.
  await page.reload();
  await expect(ask).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Workspace: Acme" }),
  ).toBeVisible();

  await page.getByRole("button", { name: "Workspace: Acme" }).click();
  await page.getByRole("menuitem", { name: "All workspaces" }).click();
  await expect(
    page.getByRole("heading", { name: "Choose a workspace" }),
  ).toBeVisible();
  const list = page.getByRole("list", { name: "Workspaces" });
  await expect(list.getByRole("button")).toHaveCount(2);
  await list.getByRole("button", { name: /Initech/ }).click();
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(wizard).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Workspace: Initech" }),
  ).toBeVisible();
});

test("navigates the shell", async ({ page }) => {
  await signIn(page);
  const nav = page.getByRole("navigation", { name: "Main navigation" });
  // Ontologies opens one: the list's first, none opened yet.
  await nav.getByRole("link", { name: "Ontologies" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Customer contracts" }),
  ).toBeVisible();
  await expect(nav.getByRole("link", { name: "Ontologies" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  await nav.getByRole("link", { name: "Connectors" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Connectors" }),
  ).toBeVisible();
  await page.getByRole("link", { name: "Settings" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Settings" }),
  ).toBeVisible();

  // The command palette finds pages.
  await page.keyboard.press("ControlOrMeta+k");
  const palette = page.getByRole("dialog", { name: "Search your workspace" });
  await palette.getByRole("combobox").fill("onto");
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(new RegExp(`/ontology/${CONTRACTS}$`));
  // And its actions: Add data opens the source picker, Create an ontology
  // its form.
  const search = page.getByRole("button", { name: "Search", exact: true });
  await search.click();
  await palette.getByRole("combobox").fill("add data");
  await palette.getByRole("option", { name: /Add data/ }).click();
  await expect(page).toHaveURL(/\/data\?add=1$/);
  await expect(page.locator("dialog[open]")).toHaveAttribute(
    "aria-label",
    "Where is your data?",
  );
  await page.keyboard.press("Escape");
  await search.click();
  await palette.getByRole("combobox").fill("create ontology");
  await palette.getByRole("option", { name: /Create an ontology/ }).click();
  await expect(page).toHaveURL(/\/ontology\/new$/);

  await page.goto("/nowhere");
  await expect(
    page.getByRole("heading", { name: "We couldn’t find that page" }),
  ).toBeVisible();
});

test("opens the Explorer and Ask as sections of their own, on the ontology last opened", async ({
  page,
}) => {
  await signIn(page);
  // The main page, Ask, opens the list's first, none opened yet in this
  // browser, and remembers it.
  await expect(page).toHaveURL(new RegExp(`/ask/${CONTRACTS}$`));
  await expect(
    page.getByRole("heading", { level: 1, name: "Analyst" }),
  ).toBeVisible();
  const orders = await addOrders(page);
  // The app read the ontologies as it opened, before Orders was made, and
  // keeps that list for a few seconds: read it again, with the one Ask
  // opened forgotten.
  await page.evaluate(() =>
    localStorage.removeItem("foundry.ontology.last.v1:ws_acme"),
  );
  await page.goto("/");
  const nav = page.getByRole("navigation", { name: "Main navigation" }),
    ontologies = nav.getByRole("link", { name: "Ontologies" }),
    picker = page.getByRole("combobox", { name: "Ontology" });
  await expect
    .poll(() => sectionNames(nav))
    .toEqual(["Analyst", "Ontologies", "Explorer", "Connectors"]);
  // None opened yet: the list's first, now Orders.
  await expect(page).toHaveURL(new RegExp(`/ask/${orders}$`));

  // The Explorer opens the ontology Ask opened.
  await nav.getByRole("link", { name: "Explorer" }).click();
  await expect(page).toHaveURL(new RegExp(`/explore/${orders}$`));
  await expect(
    page.getByRole("heading", { level: 1, name: "Explorer" }),
  ).toBeVisible();
  await expect(picker).toHaveText("Orders");
  await picker.click();
  await expect(page.getByRole("listbox").getByRole("option")).toHaveText([
    "Orders",
    "Customer contracts",
    "Sales graph",
    "Renewals",
  ]);
  await page.keyboard.press("Escape");
  // Explorer is the section shown: Ontologies isn't, nor its pages.
  await expect(nav.getByRole("link", { name: "Explorer" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  await expect(ontologies).not.toHaveClass(/is-active/);
  await expect(nav.locator(".hf-subnav-link")).toHaveCount(0);

  // An ontology opened is the one they open next, Ontologies too.
  await ontologies.click();
  await expect(page).toHaveURL(new RegExp(`/ontology/${orders}$`));
  await page
    .getByRole("complementary", { name: "Ontologies" })
    .getByRole("link", { name: /^Customer contracts/ })
    .click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Customer contracts" }),
  ).toBeVisible();
  await nav.getByRole("link", { name: "Analyst" }).click();
  await expect(page).toHaveURL(new RegExp(`/ask/${CONTRACTS}$`));
  // Nothing is loaded yet: Ask says so, and offers to load data.
  await expect(
    page.getByRole("heading", { level: 1, name: "Analyst" }),
  ).toBeVisible();
  await expect(
    page.getByRole("main").getByRole("link", { name: "Load data" }),
  ).toBeVisible();
  await expect(picker).toHaveText("Customer contracts");
  await expect(nav.getByRole("link", { name: "Analyst" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  await expect(ontologies).not.toHaveClass(/is-active/);
  await expect(nav.locator(".hf-subnav-link")).toHaveCount(0);
  await page.goto("/explore");
  await expect(page).toHaveURL(new RegExp(`/explore/${CONTRACTS}$`));

  // One that no longer exists gives way to the list's first. It is stored as
  // the next page loads: the Explorer above remembers its ontology once its
  // chunk is in, which can be after the URL changed and would overwrite it.
  await page.addInitScript(() =>
    localStorage.setItem("foundry.ontology.last.v1:ws_acme", "ont_gone"),
  );
  await page.goto("/ask");
  await expect(page).toHaveURL(new RegExp(`/ask/${orders}$`));
});

test("switches the ontology from the Explorer's and Ask's headers", async ({
  page,
}) => {
  await signIn(page);
  const orders = await addOrders(page);
  const picker = page.getByRole("combobox", { name: "Ontology" });
  // The graph opens the other's graph, without this one's load or node.
  await page.goto(`/explore/${CONTRACTS}?load=other&node=3`);
  await choose(picker, "Orders");
  await expect(page).toHaveURL(new RegExp(`/explore/${orders}$`));
  // Orders has nothing loaded and no current version: it says to finish
  // setting it up.
  await expect(
    page.getByRole("main").getByRole("link", { name: "Finish setting up" }),
  ).toBeVisible();
  // Records open the other's records, without this one's view.
  await page.goto(`/explore/${CONTRACTS}/records?load=other&type=Customer`);
  await choose(picker, "Orders");
  await expect(page).toHaveURL(new RegExp(`/explore/${orders}/records$`));
  await expect(
    page.getByRole("main").getByRole("link", { name: "Finish setting up" }),
  ).toBeVisible();
  // An entity is its ontology's own: the other's graph opens.
  await page.goto(
    `/explore/${CONTRACTS}/entities/${Buffer.from("Customer\0k:cus_GLOBEX01").toString("base64url")}?tab=facts`,
  );
  await choose(picker, "Orders");
  await expect(page).toHaveURL(new RegExp(`/explore/${orders}$`));
  // Ask opens the other's new thread.
  await page.goto(`/ask/${CONTRACTS}?new=1`);
  await expect(picker).toHaveText("Customer contracts");
  await choose(picker, "Orders");
  await expect(page).toHaveURL(new RegExp(`/ask/${orders}$`));
  await expect(page.getByRole("link", { name: "New thread" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  // And back, as the picker says.
  await choose(picker, "Customer contracts");
  await expect(page).toHaveURL(new RegExp(`/ask/${CONTRACTS}$`));
});

test("offers a new workspace only its switcher and account until it has an ontology", async ({
  page,
}) => {
  await signInToNewWorkspace(page);
  const nav = page.getByRole("navigation", { name: "Main navigation" }),
    search = page.getByRole("button", { name: "Search", exact: true }),
    palette = page.getByRole("dialog", { name: "Search your workspace" });
  // No search, no Settings link and one section, Get started, back to the
  // onboarding: the sidebar holds it, the workspace and the account.
  await expect(nav.getByRole("link")).toHaveText(["Get started"]);
  await expect(search).toHaveCount(0);
  await expect(page.getByRole("link", { name: "Settings" })).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Workspace: Globex" }),
  ).toBeVisible();
  await page.keyboard.press("ControlOrMeta+k");
  await expect(palette).toHaveCount(0);
  // The main page, the Explorer and Ask lead to the wizard.
  for (const path of ["/", "/explore", "/ask"]) {
    await page.goto(path);
    await expect(page).toHaveURL(/\/ontology\/new$/);
  }
  // The account menu offers the plan, not Settings or Members.
  await page.getByRole("button", { name: "Account" }).click();
  await expect(
    page.getByRole("menuitem", { name: /Plan & billing/ }),
  ).toBeVisible();
  for (const name of ["Settings", "Members"])
    await expect(page.getByRole("menuitem", { name, exact: true })).toHaveCount(
      0,
    );
  await page.keyboard.press("Escape");

  // Its first ontology brings the sections, search and Settings.
  await createFirstOntology(page);
  await expect(nav.getByRole("link", { name: "Explorer" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Settings" })).toBeVisible();
  await search.click();
  await expect(palette).toBeVisible();
});

test("shows the labelled sidebar on every page, Ask's threads under its sections and a section's panel beside it", async ({
  page,
}) => {
  await signIn(page);
  const nav = page.getByRole("navigation", { name: "Main navigation" }),
    threads = page.getByRole("region", { name: "Analyst threads" }),
    panel = page.locator(".hf-section-panel");
  // The main page is Ask, the sidebar's first section: each section a link
  // with its name, the open one current, and Ask's threads under them, in
  // the sidebar itself. Nothing collapses or hides it.
  const sections = ["Analyst", "Ontologies", "Explorer", "Connectors"];
  await expect(page).toHaveURL(new RegExp(`/ask/${CONTRACTS}$`));
  await expect.poll(() => sectionNames(nav)).toEqual(sections);
  await expect(nav.locator('[aria-current="page"]')).toHaveText("Analyst");
  await expect(threads.getByRole("link", { name: "New thread" })).toBeVisible();
  await expect(panel).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: /^(Hide|Show|Collapse|Expand) / }),
  ).toHaveCount(0);
  // The ontology is picked in the page's bar, beside the load.
  await expect(threads.getByRole("combobox")).toHaveCount(0);
  await expect(
    page.getByRole("main").getByRole("combobox", { name: "Ontology" }),
  ).toHaveText("Customer contracts");

  // The sidebar's controls take focus in order, the threads after the
  // sections.
  await page.getByRole("link", { name: "Helix Foundry home" }).focus();
  const expected = ["Search", "Workspace: Acme", ...sections, "New thread"];
  const order: string[] = [];
  for (let i = 0; i < expected.length; i++) {
    await page.keyboard.press("Tab");
    order.push(
      await page.evaluate(() => {
        const el = document.activeElement!;
        return (el.getAttribute("aria-label") || el.textContent || "").trim();
      }),
    );
  }
  expect(order).toEqual(expected);

  // A section with a panel shows it beside the sidebar: the Connectors
  // explorer, and the Ontologies panel. The threads stay Ask's.
  await nav.getByRole("link", { name: "Connectors" }).click();
  await expect(
    page.getByRole("complementary", { name: "Connectors explorer" }),
  ).toBeVisible();
  await expect.poll(() => sectionNames(nav)).toEqual(sections);
  await expect(nav.locator('[aria-current="page"]')).toHaveText("Connectors");
  await expect(threads).toHaveCount(0);
  await nav.getByRole("link", { name: "Ontologies" }).click();
  await expect(
    page.getByRole("complementary", { name: "Ontologies" }),
  ).toBeVisible();
  await expect(panel).toHaveCount(1);

  // Settings has the sidebar too, without a panel.
  await page.getByRole("link", { name: "Settings", exact: true }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Settings" }),
  ).toBeVisible();
  await expect.poll(() => sectionNames(nav)).toEqual(sections);
  await expect(panel).toHaveCount(0);
  await expect(threads).toHaveCount(0);
  await expect(
    page.getByRole("link", { name: "Settings", exact: true }),
  ).toHaveAttribute("aria-current", "page");

  // The sidebar's other controls: the workspace's menu, search and the
  // mark, which opens the main page, Ask, with its threads.
  await page.getByRole("button", { name: "Workspace: Acme" }).click();
  await expect(
    page.getByRole("menuitem", { name: "All workspaces" }),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "Search", exact: true }).click();
  await expect(
    page.getByRole("dialog", { name: "Search your workspace" }),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  await page.getByRole("link", { name: "Helix Foundry home" }).click();
  await expect(page).toHaveURL(new RegExp(`/ask/${CONTRACTS}$`));
  await expect(nav.locator('[aria-current="page"]')).toHaveText("Analyst");
  await expect(threads.getByRole("link", { name: "New thread" })).toBeVisible();
});
