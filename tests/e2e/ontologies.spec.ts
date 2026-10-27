import type { Page } from "@playwright/test";
import {
  APP_ORIGIN,
  choose,
  createFirstOntology,
  createdOntology,
  csrfToken,
  expect,
  finishOntology,
  mock,
  signIn,
  signInToNewWorkspace,
  startOntology,
  test,
} from "./support/mock";

// The workspace's ontologies: the Ontologies panel beside the sidebar (rows,
// the open one's pages, a filter, and a phone's drawer), /ontology opening
// the ontology last opened, a workspace without one offering only the
// wizard, creating one step by step on a database of any project or on a
// new database (in a chosen or new project, on the shared cluster) with its
// first draft, the Helix plan a new database needs, one ontology per
// database, deleting one without touching its database, exploring what a
// database already holds, and the BFF refusing Helix routes Foundry never
// calls.

const panelOf = (page: Page) =>
  page.getByRole("complementary", { name: "Ontologies" });

test("creates an ontology on an existing database and explores what it holds", async ({
  page,
}) => {
  await signIn(page);
  // /ontology opens the list's first: none opened yet in this browser, or
  // the one the main page, Ask, opened.
  await page.goto("/ontology");
  await expect(page).toHaveURL(/\/ontology\/ont_acme_contracts$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Customer contracts" }),
  ).toBeVisible();
  const panel = panelOf(page),
    list = panel.getByRole("list", { name: "Ontologies" });
  // Customer contracts, Sales graph and Renewals, each with its database.
  await expect(list.locator(".on-panel-row")).toHaveText([
    /^Customer contracts.*contracts$/,
    /^Sales graph.*sales$/,
    /^Renewals.*subscriptions$/,
  ]);
  // The overview says what the list did: the database and its project.
  await expect(page.locator(".hf-page-header-meta")).toContainText(
    "contracts · Production",
  );
  await panel.getByRole("link", { name: "New ontology" }).click();
  await expect(
    panel.getByRole("link", { name: "New ontology" }),
  ).toHaveAttribute("aria-current", "page");
  // One step at a time, the current one open, the step in the URL: name,
  // database, sources (connected data or documents, one step), goal and
  // review.
  const steps = page.getByRole("list", { name: "Steps" });
  await expect(steps.getByRole("listitem")).toHaveCount(5);
  await expect(steps.locator('[aria-current="step"]')).toContainText("Name");
  await page.getByLabel("Ontology name").fill("Orders");
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(page).toHaveURL(/\/ontology\/new\?step=database$/);
  await expect(steps.getByRole("listitem").first()).toContainText("Orders");
  // Every live database of every project; one ontology per database.
  const databases = page.getByRole("list", { name: "Databases" });
  await expect(databases.getByRole("listitem")).toHaveCount(5);
  await expect(
    page.getByRole("radio", { name: "contracts in Production" }),
  ).toBeDisabled();
  await expect(
    page.getByRole("radio", { name: "sales in Production" }),
  ).toBeDisabled();
  await expect(databases).toContainText("used by Customer contracts");
  await expect(databases).toContainText("used by Renewals");
  await expect(databases).toContainText("used by Sales graph");
  await expect(databases).not.toContainText("old-orders");
  const sent = page.waitForRequest(/foundry\/ontologies$/),
    drafted = page.waitForRequest(/foundry\/ontologies\/ont_\w+:generate$/);
  const orders = page.getByRole("radio", { name: "orders in Production" });
  await orders.check();
  // Back keeps what was chosen; the browser's Back and Forward too. Back is
  // a history entry of its own, so the browser's Back returns from it.
  await page.getByRole("button", { name: "Back" }).click();
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(page.getByLabel("Ontology name")).toHaveValue("Orders");
  await page.goBack();
  await expect(page).toHaveURL(/\/ontology\/new\?step=database$/);
  await expect(orders).toBeChecked();
  await page.goBack();
  await expect(page.getByLabel("Ontology name")).toHaveValue("Orders");
  await page.goForward();
  await expect(orders).toBeChecked();
  await finishOntology(page);
  expect((await sent).postDataJSON()).toEqual({
    name: "Orders",
    tenantId: "ten_acme_orders",
    clientToken: expect.any(String),
  });
  // Its first draft, from every synced connector, no documents, no goal.
  expect((await drafted).postDataJSON()).toEqual({
    connectorIds: ["conn_acme_stripe", "conn_acme_shop"],
    documentSetIds: [],
  });
  await page.goto(await createdOntology(page));
  await expect(
    page.getByRole("heading", { level: 1, name: "Orders" }),
  ).toBeVisible();

  // Its pages sit under its row in the panel; the Explorer, a section of
  // its own, opens it: with nothing loaded it says to finish setting it up,
  // and reads the database the team already filled as Other data.
  const nav = page.getByRole("navigation", { name: "Main navigation" });
  await expect(list.locator(".on-panel-row")).toHaveCount(4);
  await expect(list.getByRole("link", { name: /^Orders/ })).toHaveAttribute(
    "aria-current",
    "true",
  );
  // Its Overview, then Documents, what it reads of the workspace's, with
  // its count.
  await expect(list.locator(".hf-subnav-link")).toHaveText([
    "Overview",
    /^Documents\d*$/,
  ]);
  await nav.getByRole("link", { name: "Explorer" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Explorer" }),
  ).toBeVisible();
  await page
    .getByRole("main")
    .getByRole("link", { name: "Finish setting up" })
    .waitFor();
  await page.goto(`${new URL(page.url()).pathname}?load=other`);
  await expect(
    page.getByRole("application", {
      name: "Graph of orders: 7 nodes, 4 edges",
    }),
  ).toBeVisible({ timeout: 15_000 });
  const labels = page.getByRole("complementary", { name: "Labels" });
  await expect(labels).toContainText("Account");
  await expect(labels).toContainText("PLACED");
  await labels.getByRole("button", { name: /^Order node label/ }).click();
  const inspector = page.getByRole("complementary", { name: "Inspector" });
  await expect(inspector).toContainText("Node label");
  await expect(inspector).toContainText("In the database4");
  // Every read went through query-read for this database; nothing wrote.
  const reads = (await mock.stats()).requests.filter((r) =>
    r.includes("/v1/databases:"),
  );
  expect(reads.length).toBeGreaterThan(0);
  expect(new Set(reads)).toEqual(
    new Set(["POST /v1/databases:query-read 200"]),
  );
});

test("lists the ontologies beside the sidebar, with the open one's pages", async ({
  page,
}) => {
  await signIn(page);
  const nav = page.getByRole("navigation", { name: "Main navigation" }),
    panel = panelOf(page),
    list = panel.getByRole("list", { name: "Ontologies" });
  // The section has a panel, beside the sidebar and its sections' names,
  // Ontologies current.
  await nav.getByRole("link", { name: "Ontologies" }).click();
  await expect(page).toHaveURL(/\/ontology\/ont_acme_contracts$/);
  await expect(nav.locator(".hf-nav-link")).toHaveText([
    "Ask",
    "Ontologies",
    "Explorer",
    "Connectors",
  ]);
  await expect(panel).toBeVisible();
  await expect(nav.getByRole("link", { name: "Ontologies" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  const contracts = list.getByRole("link", { name: /^Customer contracts/ }),
    renewals = list.getByRole("link", { name: /^Renewals/ });
  await expect(contracts).toHaveAttribute("aria-current", "true");
  await expect(renewals).not.toHaveAttribute("aria-current");
  // Each row's dot says how its last load went.
  await expect(renewals.getByRole("img")).toHaveAttribute(
    "aria-label",
    /^(Loaded|Not loaded|Failed|Loading|Queued)$/,
  );
  // Only the open one's pages show, the page open marked: its Overview and
  // Documents, with its documents as a file system under it (none yet).
  const pages = list.locator(".hf-subnav-link");
  await expect(pages).toHaveText(["Overview", /^Documents\d*$/]);
  await expect(list).toContainText("No documents yet");
  const current = list.locator('.hf-subnav-link[aria-current="page"]');
  await expect(current).toHaveText("Overview");
  // Documents opens the list, the panel otherwise as it was.
  await list.getByRole("link", { name: /^Documents/ }).click();
  await expect(page).toHaveURL(/\/ontology\/ont_acme_contracts\/documents$/);
  await expect(current).toHaveText(/^Documents/);
  await expect(list.locator(".on-panel-row")).toHaveCount(3);
  await page.goto("/ontology/ont_acme_contracts");
  await expect(current).toHaveText("Overview");
  // The overview lists versions and loads: their pages and the generate
  // form sit under it.
  await page.goto("/ontology/ont_acme_contracts/generations/gen_seed_1");
  await expect(current).toHaveText("Overview");
  await page.goto("/ontology/ont_acme_contracts/generate");
  await expect(current).toHaveText("Overview");
  await page.goto("/ontology/ont_acme_contracts/versions/ver_seed_1");
  await expect(current).toHaveText("Overview");

  // Another row opens that ontology's overview, its pages under it.
  await renewals.click();
  await expect(page).toHaveURL(/\/ontology\/ont_acme_renewals$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Renewals" }),
  ).toBeVisible();
  await expect(renewals).toHaveAttribute("aria-current", "true");
  await expect(pages).toHaveText(["Overview", /^Documents\d*$/]);
  await expect(current).toHaveText("Overview");
  // Nothing toggles the panel: it always shows.
  await expect(
    page.getByRole("button", { name: /^(Hide|Show) Ontologies$/ }),
  ).toHaveCount(0);

  // It is the one the main page, Ask, and the sidebar's Ontologies open next,
  // and Back skips / and /ontology.
  await page.goto("/");
  await expect(page).toHaveURL(/\/ask\/ont_acme_renewals$/);
  await nav.getByRole("link", { name: "Ontologies" }).click();
  await expect(page).toHaveURL(/\/ontology\/ont_acme_renewals$/);
  await page.goBack();
  await expect(page).toHaveURL(/\/ask\/ont_acme_renewals$/);
});

test("filters a long list of ontologies by name or database", async ({
  page,
}) => {
  const API = "**/papi/v1/workspaces/ws_acme/foundry/ontologies";
  // Seven more, so the list is long enough for a filter.
  await page.route(API, async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const res = await route.fetch(),
      body = await res.json();
    body.ontologies.push(
      ...Array.from({ length: 7 }, (_, i) => ({
        id: `ont_more_${i}`,
        name: `Ledger ${i}`,
        databaseName: `ledger${i}`,
      })),
    );
    await route.fulfill({ response: res, json: body });
  });
  await signIn(page);
  await page.goto("/ontology/ont_acme_contracts");
  const panel = panelOf(page),
    rows = panel
      .getByRole("list", { name: "Ontologies" })
      .locator(".on-panel-row"),
    filter = panel.getByRole("searchbox", { name: "Filter ontologies" });
  await expect(rows).toHaveCount(10);
  await filter.fill("renew");
  await expect(rows).toHaveText([/^Renewals/]);
  // By database too.
  await filter.fill("SUBSCR");
  await expect(rows).toHaveText([/^Renewals/]);
  await filter.fill("nothing like it");
  await expect(panel).toContainText("No ontologies match “nothing like it”.");
  await panel.getByRole("button", { name: "Clear search" }).click();
  await expect(rows).toHaveCount(10);
});

test("gives a new workspace only the wizard until its first ontology", async ({
  page,
}) => {
  await signInToNewWorkspace(page);
  // No sections and no Ontologies panel: every page leads to the wizard,
  // titled for the first ontology, with no way back to Ontologies.
  const nav = page.getByRole("navigation", { name: "Main navigation" }),
    wizard = page.getByRole("heading", {
      level: 1,
      name: "Create your first ontology",
    });
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(nav).toHaveCount(0);
  await expect(panelOf(page)).toHaveCount(0);
  for (const path of ["/", "/ontology", "/explore", "/ask", "/data", "/x"]) {
    await page.goto(path);
    await expect(page).toHaveURL(/\/ontology\/new$/);
    await expect(wizard).toBeVisible();
  }
  await expect(
    page.getByRole("navigation", { name: "Breadcrumb" }),
  ).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Cancel" })).toHaveCount(0);

  // Its first ontology opens every page, and the panel lists it.
  const overview = await createFirstOntology(page);
  await expect(nav).toBeVisible();
  await expect(
    panelOf(page).getByRole("link", { name: /^Orders/ }),
  ).toHaveAttribute("href", overview);
  await page.goto("/ontology");
  await expect(page).toHaveURL(new RegExp(`${overview}$`));
  await page.goto("/ontology/new");
  await expect(
    page.getByRole("heading", { level: 1, name: "New ontology" }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Cancel" })).toBeVisible();
});

test("goes back to the wizard once the last ontology is deleted, and tells a member it isn't set up", async ({
  page,
}) => {
  await signIn(page);
  const headers = {
    origin: APP_ORIGIN,
    "x-helix-csrf": await csrfToken(page),
  };
  for (const o of ["ont_acme_sales", "ont_acme_renewals"])
    expect(
      (
        await page.request.delete(
          `/papi/v1/workspaces/ws_acme/foundry/ontologies/${o}`,
          { headers },
        )
      ).status(),
    ).toBe(200);
  await page.goto("/ontology/ont_acme_contracts");
  await page.getByRole("button", { name: "Delete ontology" }).click();
  await page
    .getByRole("dialog", { name: "Delete Customer contracts?" })
    .getByRole("button", { name: "Delete ontology" })
    .click();
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Create your first ontology" }),
  ).toBeVisible();
  await expect(
    page.getByRole("navigation", { name: "Main navigation" }),
  ).toHaveCount(0);

  // A member can't create one: the wizard says who sets the workspace up.
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(page.getByText("This workspace isn’t set up yet")).toBeVisible();
  await expect(page.getByLabel("Ontology name")).toHaveCount(0);
  await expect(
    page.getByRole("status").filter({ hasText: "Set up with your agent" }),
  ).toHaveCount(0);
});

test("on a phone, shows the rail and the ontologies in the drawer", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await signIn(page);
  await page.goto("/ontology/ont_acme_contracts");
  await expect(
    page.getByRole("heading", { level: 1, name: "Customer contracts" }),
  ).toBeVisible();
  // No panel beside the page.
  await expect(panelOf(page)).toHaveCount(0);
  await page.getByRole("button", { name: "Open navigation" }).click();
  const drawer = page.getByRole("dialog", { name: "Navigation" });
  const rail = drawer.getByRole("navigation", { name: "Main navigation" });
  await expect(rail.getByRole("link")).toHaveCount(4);
  await expect(rail.getByRole("link", { name: "Ontologies" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  const panel = drawer.getByRole("complementary", { name: "Ontologies" });
  // Its documents are a file system under its Documents (none yet here).
  await expect(panel.getByRole("link", { name: /^Documents/ })).toBeVisible();
  await expect(panel).toContainText("No documents yet");
  // Another ontology closes the drawer.
  await panel.getByRole("link", { name: /^Renewals/ }).click();
  await expect(drawer).toHaveCount(0);
  await expect(
    page.getByRole("heading", { level: 1, name: "Renewals" }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
});

test("creates a database in a new project, on the shared cluster", async ({
  page,
}) => {
  await signIn(page);
  await startOntology(page, "Renewals");
  await page.getByRole("radio", { name: "Create a new one" }).click();
  await choose(
    page.getByRole("combobox", { name: /^Project/ }),
    "A new project…",
  );
  await page.getByLabel("Project name").fill("Foundry");
  await page.getByLabel("Database name").fill("Renewals 2026");
  await expect(
    page.getByText(
      "Its address is renewals-2026, with a short suffix if another database has it.",
    ),
  ).toBeVisible();
  const tenant = page.waitForRequest(/\/papi\/v1\/tenants$/);
  await finishOntology(page);
  // No cluster: the WFE places it on the shared cluster.
  expect((await tenant).postDataJSON()).toEqual({
    projectId: expect.stringMatching(/^prj_/),
    name: "Renewals 2026",
    slug: "renewals-2026",
  });
  const overview = await createdOntology(page);
  // Helix made the database a first key that Foundry never showed: the page
  // the wizard lands on says so once, with a link to revoke it.
  const key = page.getByRole("status").filter({
    hasText: "Helix gave Renewals 2026 a first API key",
  });
  await expect(
    key.getByRole("link", { name: "Open its keys" }),
  ).toHaveAttribute("href", /\/acme\/prj_\w+\/database\/ten_\w+$/);
  await key.getByRole("button", { name: "Dismiss" }).click();
  await expect(key).toBeHidden();
  const created = (await mock.stats()).requests.filter((r) =>
    r.startsWith("POST"),
  );
  expect(created).toEqual(
    expect.arrayContaining([
      "POST /v1/projects 200",
      "POST /v1/tenants 200",
      "POST /v1/workspaces/ws_acme/foundry/ontologies 200",
    ]),
  );
  await page.goto(overview);
  await expect(page.locator(".hf-page-header-meta")).toContainText(
    "Renewals 2026 · Foundry",
  );
  // /ontology opens the ontology last opened.
  const renewals = page.url();
  await page.goto("/ontology");
  await expect(page).toHaveURL(renewals);
});

test("gives a new database a suffix when another database has its address", async ({
  page,
}) => {
  await signIn(page);
  await startOntology(page, "Contracts again");
  await page.getByRole("radio", { name: "Create a new one" }).click();
  // contracts is taken on the shared cluster, which every workspace shares.
  await page.getByLabel("Database name").fill("Contracts");
  const slugs: string[] = [];
  page.on("request", (r) => {
    if (/\/papi\/v1\/tenants$/.test(r.url()) && r.method() === "POST")
      slugs.push(r.postDataJSON().slug);
  });
  await finishOntology(page);
  await createdOntology(page);
  expect(slugs).toEqual([
    "contracts",
    expect.stringMatching(/^contracts-[0-9a-f]{6}$/),
  ]);
  expect(
    (await mock.stats()).requests.filter((r) =>
      r.startsWith("POST /v1/tenants"),
    ),
  ).toEqual(["POST /v1/tenants 409", "POST /v1/tenants 200"]);
});

test("fits the new-ontology form on a phone", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 800 });
  await signIn(page);
  await startOntology(page, "Orders");
  await expect(
    page.getByRole("radio", { name: "Create a new one" }),
  ).toBeVisible();
  for (const mode of ["Use an existing one", "Create a new one"]) {
    await page.getByRole("radio", { name: mode }).click();
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth),
      mode,
    ).toBeLessThanOrEqual(375);
  }
});

test("says a new database needs a Helix plan, and offers one the workspace has", async ({
  page,
}) => {
  await mock.configure({ helixSubscription: false });
  await signIn(page);
  await startOntology(page, "Pipeline");
  await page.getByRole("radio", { name: "Create a new one" }).click();
  await page.getByLabel("Database name").fill("pipeline");
  await finishOntology(page);
  const callout = page.getByRole("alert").filter({
    hasText: "A new database needs a Helix plan",
  });
  await expect(callout).toBeVisible();
  await expect(
    callout.getByRole("link", { name: "Open Helix billing" }),
  ).toHaveAttribute("href", /\/acme\/billing$/);
  // Nothing was created but the refusal.
  expect(
    (await mock.stats()).requests.filter((r) => r.startsWith("POST /v1/")),
  ).toContain("POST /v1/tenants 400");
  // Back to the database step, the other choices kept.
  await callout.getByRole("button", { name: "Use an existing one" }).click();
  await page.getByRole("radio", { name: "sandbox in Staging" }).check();
  await finishOntology(page);
  await page.goto(await createdOntology(page));
  await expect(
    page.getByRole("heading", { level: 1, name: "Pipeline" }),
  ).toBeVisible();
});

test("deletes an ontology and its loads, leaving its database for another", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/ontology/ont_acme_contracts");
  await page.getByRole("button", { name: "Delete ontology" }).click();
  const confirm = page.getByRole("dialog", {
    name: "Delete Customer contracts?",
  });
  await expect(confirm).toContainText(
    "deletes what its loads wrote from contracts, with any edges added to those nodes. The database stays in Helix with your own data, and your documents stay in Connectors.",
  );
  await confirm.getByRole("button", { name: "Delete ontology" }).click();
  // /ontology opens the list's first left, never the one deleted: the
  // Sales graph, with Renewals after it.
  await expect(page).toHaveURL(/\/ontology\/ont_acme_sales$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Sales graph" }),
  ).toBeVisible();
  await expect(
    panelOf(page)
      .getByRole("list", { name: "Ontologies" })
      .locator(".on-panel-row"),
  ).toHaveText([/^Sales graph/, /^Renewals/]);
  await page.goto("/ontology/ont_acme_contracts");
  await expect(page.getByText("This ontology doesn’t exist")).toBeVisible();
  await startOntology(page, "Contracts");
  await expect(
    page.getByRole("radio", { name: "contracts in Production" }),
  ).toBeEnabled();
});

test("shows a plain member the ontologies without changes", async ({
  page,
}) => {
  await signIn(page, "alan@example.com");
  await page.goto("/ontology");
  await expect(
    panelOf(page)
      .getByRole("list", { name: "Ontologies" })
      .locator(".on-panel-row"),
  ).toHaveCount(3);
  await expect(page.getByRole("link", { name: "New ontology" })).toHaveCount(0);
  await page.goto("/ontology/new");
  await expect(
    page.getByText("Only owners and admins can create ontologies"),
  ).toBeVisible();
  await page.goto("/ontology/ont_acme_contracts");
  await expect(
    page.getByRole("button", { name: "Delete ontology" }),
  ).toHaveCount(0);
});

test("tells a member who can't read the database's project so", async ({
  page,
}) => {
  await mock.configure({ ontologyDatabaseHidden: true });
  await signIn(page, "alan@example.com");
  await page.goto("/ontology");
  await expect(page.getByText("Not visible to you").first()).toBeVisible();
  const hidden = page.getByText("You can’t read the ontology’s database");
  await page.goto("/explore/ont_acme_contracts");
  await expect(hidden).toBeVisible();
  await page.goto(
    "/explore/ont_acme_contracts/entities/Q3VzdG9tZXIAazpjdXNfR0xPQkVYMDE",
  );
  await expect(hidden).toBeVisible();
});

test("the BFF reaches Helix databases only as Foundry reads them", async ({
  page,
}) => {
  await signIn(page);
  const headers = { origin: APP_ORIGIN, "x-helix-csrf": await csrfToken(page) };
  // Dedicated clusters, writes and keys never pass.
  for (const [method, path] of [
    ["GET", "/papi/v1/clusters"],
    ["POST", "/papi/v1/clusters"],
    ["POST", "/papi/v1/databases:query-write"],
    ["DELETE", "/papi/v1/tenants/ten_acme_orders"],
    ["GET", "/papi/v1/tenants/ten_acme_orders/keys"],
    ["DELETE", "/papi/v1/projects/prj_acme_prod"],
  ] as const) {
    const res = await page.request.fetch(path, {
      method,
      headers,
      data: method === "GET" ? undefined : {},
    });
    expect(res.status(), `${method} ${path}`).toBe(404);
  }
  expect(
    (await mock.stats()).requests.filter((r) =>
      /clusters|query-write|\/keys|DELETE \/v1\/(tenants|projects)/.test(r),
    ),
  ).toEqual([]);
});
