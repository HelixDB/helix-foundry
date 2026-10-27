import type { Page, Route } from "@playwright/test";
import {
  APP_ORIGIN,
  createdOntology,
  csrfToken,
  expect,
  expectGridFits,
  finishOntology,
  mock,
  signIn,
  startOntology,
  test,
} from "./support/mock";

// The Ontology area, on Acme's seeded ontology: the current version's graph,
// versions and making one current, both version formats, generating with
// status polling, failures, and the BFF refusing ontology routes the UI
// never calls.

// Acme's seeded ontology, on its Production project's contracts database.
const O = "/ontology/ont_acme_contracts";
const tab = (page: Page, name: string) =>
  page.getByRole("tab", { name: new RegExp(`^${name}`) });
// A generation's page shows its goal once, in this panel.
const goalPanel = (page: Page) =>
  page.locator(".hf-panel").filter({
    has: page.getByRole("heading", { level: 3, name: "Goal", exact: true }),
  });

test("shows the current version's graph and a newer draft to review", async ({
  page,
}) => {
  await signIn(page);
  await page
    .getByRole("navigation", { name: "Main navigation" })
    .getByRole("link", { name: "Ontologies" })
    .click();
  await page
    .getByRole("complementary", { name: "Ontologies" })
    .getByRole("link", { name: /^Customer contracts/ })
    .click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Customer contracts" }),
  ).toBeVisible();
  await expect(page.getByText("Version 3 is ready to review")).toBeVisible();
  await expect(
    page.getByRole("heading", { level: 2, name: "Version 1" }),
  ).toBeVisible();
  // The schema draws every type as a card and every link as a pill.
  await expect(page.getByText(/^4 types · /)).toBeVisible();
  const types = page
    .getByRole("list")
    .filter({ has: page.locator(".so-node") });
  await expect(types.locator(".so-node")).toHaveCount(4);
  await expect(
    page.getByRole("button", { name: "Charge pays Invoice" }),
  ).toBeVisible();
  await expect(page.getByText("4 tables from 1 source")).toBeVisible();
  // A card opens its type on the version page.
  await page.getByRole("link", { name: "Invoice" }).click();
  await expect(page).toHaveURL(
    /\/ontology\/ont_acme_contracts\/versions\/ver_seed_1\?tab=entities#type-Invoice$/,
  );
  await expect(page.getByRole("region", { name: "Invoice" })).toBeVisible();
});

test("reads an agent draft: types, links, mappings, rules, questions and TOML", async ({
  page,
}) => {
  await signIn(page);
  await page.goto(`${O}#versions`);
  // The actions column has no header type icon; the others have one each.
  const head = page.locator(".hf-grid-head");
  await expect(head.locator(".on-actions .hf-grid-type")).toBeHidden();
  await expect(head.locator(".hf-grid-type:visible")).toHaveCount(5);
  await page.getByRole("link", { name: "Version 2" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Version 2" }),
  ).toBeVisible();
  await expect(page.getByText("Agent draft", { exact: true })).toBeVisible();
  await expect(
    page.getByText(/This is a first draft that lets customer service/),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Read the agent’s full summary" })
    .click();
  await expect(page.getByText("Keys and joins")).toBeVisible();
  // Parcels nest in shipments: one table, two types.
  await expect(
    page.getByText(
      "6 tables from 1 source, condensed into 8 types and 8 links",
    ),
  ).toBeVisible();

  await tab(page, "Types").click();
  await expect(tab(page, "Types")).toContainText("8");
  const line = page.getByRole("region", { name: "OrderLine" });
  await expect(line).toContainText("order_number, line_no");
  await expect(line).toContainText("Shop database · order_items");
  await expect(line.getByRole("rowheader", { name: "qty" })).toBeVisible();

  await tab(page, "Links").click();
  const links = page.getByRole("grid", { name: "Links" });
  const placed = links.getByRole("row").filter({ hasText: "placed_by" });
  await expect(placed).toContainText("Order");
  await expect(placed).toContainText("Customer");
  await expect(placed).toContainText("Required");
  await expect(
    links.getByRole("row").filter({ hasText: "for_product" }),
  ).toContainText("Optional");

  await tab(page, "Mappings").click();
  // Source ids are hashes of the synced tables, resolved to their names.
  await expect(
    page.getByRole("region", { name: /Shop database · orders/ }),
  ).toContainText("orders");
  // The newest sync dropped shipments; the generation still names it.
  await expect(
    page.getByRole("region", { name: /Shop database · shipments/ }),
  ).toContainText("parcels");
  const orders = page.locator("#mapping-orders");
  await expect(orders).toContainText("Looks up");
  await expect(orders).toContainText("the row of customers with");
  await expect(page.locator("#mapping-parcels")).toContainText(
    "Each element of /packages in a row of shipments",
  );

  await tab(page, "Rules").click();
  await expect(
    page.getByRole("heading", { name: "return_was_ordered" }),
  ).toBeVisible();
  await expect(
    page.getByText("on_order(o, p) :- line_of(l, o), for_product(l, p)."),
  ).toBeVisible();

  await tab(page, "Questions").click();
  await expect(page.locator(".on-question")).toHaveCount(3);
  await expect(page.getByText("Who placed each order?")).toBeVisible();
  await expect(page.getByText("Shop database · accounts.email")).toBeVisible();
  await expect(
    page.getByText(/30 days of the order being placed/),
  ).toBeVisible();

  await tab(page, "TOML").click();
  await expect(page.getByLabel("ontology.toml")).toContainText(
    "[types.Customer]",
  );
  await page.getByRole("radio", { name: "mappings.toml" }).click();
  await expect(page.getByLabel("mappings.toml")).toContainText(
    "[mappings.orders]",
  );
});

test("reads a Definition v1 version", async ({ page }) => {
  await signIn(page);
  await page.goto(`${O}/versions/ver_seed_1`);
  await expect(page.getByText("Definition v1", { exact: true })).toBeVisible();
  await tab(page, "Entities").click();
  const customer = page.getByRole("region", { name: "Customer" });
  await expect(customer).toContainText("Stripe · customers · 1,284 rows");
  await expect(
    customer.getByRole("rowheader", { name: "email" }),
  ).toBeVisible();
  await tab(page, "Relationships").click();
  await expect(
    page.getByText("Charge.invoice_id = Invoice.invoice_id"),
  ).toBeVisible();
  await tab(page, "Rules").click();
  await expect(
    page.getByText("status = 'open' AND amount_due > 0"),
  ).toBeVisible();
  await tab(page, "Constraints").click();
  await expect(
    page.getByRole("row").filter({ hasText: "charge_not_negative" }),
  ).toContainText("amount >= 0");
  await tab(page, "Definition").click();
  await expect(page.getByLabel("definition.json")).toContainText(
    '"primary_key": "customer_id"',
  );
});

test("makes a version current and rolls back", async ({ page }) => {
  await signIn(page);
  await page.goto(`${O}#versions`);
  await expect(
    page.getByRole("heading", { level: 2, name: "Schema versions" }),
  ).toBeVisible();
  const grid = page.getByRole("grid", { name: "Ontology versions" });
  // Newest first, each named by its generation, with the generation that
  // failed to draft one among them.
  await expect(grid.getByRole("row")).toHaveCount(5);
  await expect(grid.getByRole("row").nth(1)).toContainText(
    "Customer contracts and billing",
  );
  await expect(grid.getByRole("row").nth(3)).toContainText("Failed");
  await expectGridFits(grid);
  await grid
    .getByRole("row")
    .filter({ hasText: "Version 2" })
    .getByRole("link", { name: "Review version" })
    .click();
  await page.getByRole("button", { name: "Make current", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Make version 2 current?" });
  // The confirmation wraps inside its card.
  await expect(dialog).toContainText("You can switch back at any time.");
  expect(
    await dialog
      .locator(".hf-dialog-card")
      .evaluate((el) => el.scrollWidth - el.clientWidth),
  ).toBe(0);
  await dialog.getByRole("button", { name: "Make current" }).click();
  await expect(page.getByText("Version 2 is now current.")).toBeVisible();
  await page.goto(`${O}#versions`);
  await expect(
    grid.getByRole("row").filter({ hasText: "Version 2" }),
  ).toContainText("Current");
  await expect(
    grid.getByRole("row").filter({ hasText: "Version 1" }),
  ).not.toContainText("Current");
  // The overview follows, with no draft left to review.
  await page.goto(O);
  await expect(
    page.getByRole("heading", { level: 2, name: "Version 2" }),
  ).toBeVisible();
  // Version 3 is still newer, so it waits for review: one link to it, and
  // Make current once its rules are read.
  await expect(page.getByText("Version 2 is ready to review")).toHaveCount(0);
  await expect(page.getByText("Version 3 is ready to review")).toBeVisible();
  const callout = page
    .getByRole("status")
    .filter({ hasText: "Version 3 is ready to review" });
  await expect(
    callout.getByRole("link", { name: "Review", exact: true }),
  ).toBeVisible();
  await expect(
    callout.getByRole("button", { name: "Make current", exact: true }),
  ).toBeVisible();
  await expect(
    callout.getByRole("link", { name: "Review version" }),
  ).toHaveCount(0);
  // Rolling back is making the older one current again.
  await page.goto(`${O}/versions/ver_seed_1`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 1 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 1 is now current.")).toBeVisible();
  const moves = (await mock.stats()).requests.filter((r) =>
    r.includes("ont_acme_contracts:set-current"),
  );
  expect(moves).toEqual([
    "POST /v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts:set-current 200",
    "POST /v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts:set-current 200",
  ]);
});

test("refuses to move a pointer someone else moved", async ({ page }) => {
  await signIn(page);
  await page.goto(`${O}/versions/ver_seed_2`);
  await expect(
    page.getByRole("button", { name: "Make current" }),
  ).toBeVisible();
  // Another tab makes version 2 current behind this page's back.
  const moved = await page.request.post(
    "/papi/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts:set-current",
    {
      data: { versionId: "ver_seed_2", expectedCurrentVersionId: "ver_seed_1" },
      headers: {
        origin: APP_ORIGIN,
        "x-helix-csrf": await csrfToken(page),
      },
    },
  );
  expect(moved.status()).toBe(200);
  // This page still saw version 1 as current, so the WFE refuses it.
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 2 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(
    page.getByText(
      "The current ontology changed since you loaded it; reload and try again",
    ),
  ).toBeVisible();
  // The page reloads what is current.
  await expect(
    page.locator(".hf-page-header-meta").getByText("Current", { exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Make current" })).toHaveCount(
    0,
  );
});

test("generates an ontology and follows it to the new version", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 1200 });
  await signIn(page);
  await page.goto(O);
  // The header's Generate: the versions list's failed generation names
  // "generate" in its reason too.
  await page.getByRole("link", { name: "Generate", exact: true }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Generate an ontology" }),
  ).toBeVisible();
  await page
    .getByLabel("Goal")
    .fill("Trace orders through shipments and returns");
  const connectors = page.getByRole("list", { name: "Connectors" });
  await connectors.getByRole("checkbox", { name: /Stripe/ }).uncheck();
  // The workspace's document sets go in as the current version reads them:
  // version 1 reads none, so General, empty, starts unchecked, and the
  // summary names no set.
  await expect(
    page.getByText("Shop database: 6 tables.", { exact: true }),
  ).toBeVisible();
  const sets = page.getByRole("list", { name: "Document sets" });
  await expect(
    sets.getByRole("checkbox", { name: /General/ }),
  ).not.toBeChecked();
  await expect(sets).toContainText("No documents yet");
  // The agent drafts afresh, so nothing claims it builds on the current one.
  await expect(page.getByText(/extends version/)).toHaveCount(0);
  await page
    .getByRole("button", { name: "Show Shop database’s tables" })
    .click();
  await expect(
    page
      .getByRole("list", { name: "Shop database tables" })
      .getByRole("listitem"),
  ).toHaveCount(6);
  const sent = page.waitForRequest(/ont_acme_contracts:generate$/);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  expect((await sent).postDataJSON()).toEqual({
    goal: "Trace orders through shipments and returns",
    connectorIds: ["conn_acme_shop"],
    documentSetIds: [],
  });
  await expect(page).toHaveURL(
    /\/ontology\/ont_acme_contracts\/generations\/gen_/,
  );
  // Headed by the title the WFE gave it, the goal in its own panel.
  await expect(
    page.getByRole("heading", {
      level: 1,
      name: "Orders through shipments and returns",
    }),
  ).toBeVisible();
  await expect(page).toHaveTitle(
    "Orders through shipments and returns · Helix Foundry",
  );
  await expect(
    page
      .getByRole("navigation", { name: "Breadcrumb" })
      .locator("[aria-current]"),
  ).toHaveText("Orders through shipments and returns");
  await expect(goalPanel(page)).toContainText(
    "Trace orders through shipments and returns",
  );
  await expect(goalPanel(page).getByRole("button")).toHaveCount(0);
  // Polled until it is stored.
  await expect(page.getByText("Succeeded", { exact: true })).toBeVisible({
    timeout: 15_000,
  });
  await expect(page.getByText("Stored as version 4").first()).toBeVisible();
  await expect(page.getByText("Current when requested")).toBeVisible();
  await page.getByRole("link", { name: "Review version 4" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Version 4" }),
  ).toBeVisible();
  // One per Shop table, the people too.
  await expect(tab(page, "Types")).toContainText("6");
  await expect(page.getByText("Not current")).toBeVisible();
  // The version names its generation by title.
  await expect(page.locator(".hf-page-header-meta")).toContainText(
    "For Orders through shipments and returns",
  );
  // The versions list has it first, by title.
  await page.goto(`${O}#versions`);
  const first = page
    .getByRole("grid", { name: "Ontology versions" })
    .getByRole("row")
    .nth(1);
  await expect(first).toContainText("Version 4");
  await expect(first).toContainText("Orders through shipments and returns");
  const reached = (await mock.stats()).requests;
  expect(reached).toContain(
    "POST /v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts:generate 200",
  );
});

test("nests a kind's connectors under one choice when it has several", async ({
  page,
}) => {
  await signIn(page);
  await page.goto(O);
  // A second PostgreSQL database, synced.
  const api = "/papi/v1/workspaces/ws_acme/foundry/connectors",
    headers = { origin: APP_ORIGIN, "x-helix-csrf": await csrfToken(page) };
  const created = await page.request.post(api, {
    data: {
      name: "Billing replica",
      config: {
        postgres: {
          host: "billing.acme.example.com",
          database: "billing",
          user: "ro",
          password: "pw",
        },
      },
    },
    headers,
  });
  const billing = (await created.json()).connector.id as string;
  await page.request.post(`${api}/${billing}:sync`, { data: {}, headers });
  await expect
    .poll(
      async () =>
        (await (await page.request.get(`${api}/${billing}`)).json()).connector
          .lastSync?.status,
      { timeout: 15_000 },
    )
    .toBe("CONNECTOR_SYNC_STATUS_SUCCEEDED");

  await page.goto(`${O}/generate`);
  const connectors = page.getByRole("list", {
    name: "Connectors",
    exact: true,
  });
  // Stripe, alone, is its own choice as before; PostgreSQL's two sit under
  // one, all chosen.
  const stripe = connectors.getByRole("checkbox", { name: /^Stripe/ }),
    // The kind's choice says how many it holds; its connectors don't.
    postgres = connectors.getByRole("checkbox", {
      name: /^PostgreSQL.* connectors · /,
    });
  await expect(stripe).toBeChecked();
  await expect(postgres).toBeChecked();
  await expect(connectors).toContainText("2 connectors · 2 chosen");
  await expect(
    page.getByRole("list", { name: "PostgreSQL connectors" }),
  ).toHaveCount(0);
  await page
    .getByRole("button", { name: "Show PostgreSQL connectors" })
    .click();
  const nested = page.getByRole("list", { name: "PostgreSQL connectors" });
  await expect(nested.locator(".on-connector-text strong")).toHaveText([
    "Shop database",
    "Billing replica",
  ]);
  // One of two: partly chosen.
  await nested.getByRole("checkbox", { name: /Billing replica/ }).uncheck();
  await expect(postgres).toBeChecked({ indeterminate: true });
  await expect(connectors).toContainText("2 connectors · 1 chosen");
  // Partly chosen chooses all; all chosen clears the kind.
  await postgres.click();
  await expect(postgres).toBeChecked();
  await expect(
    nested.getByRole("checkbox", { name: /Billing replica/ }),
  ).toBeChecked();
  await postgres.click();
  await expect(postgres).not.toBeChecked();
  await expect(nested.getByRole("checkbox")).toHaveCount(2);
  for (const box of await nested.getByRole("checkbox").all())
    await expect(box).not.toBeChecked();
  // Each nested connector still shows its tables.
  await page
    .getByRole("button", { name: "Show Billing replica’s tables" })
    .click();
  await expect(
    page.getByRole("list", { name: "Billing replica tables" }),
  ).toBeVisible();
  await nested.getByRole("checkbox", { name: /Shop database/ }).check();
  await expect(postgres).toBeChecked({ indeterminate: true });
  // The summary names the two chosen, Billing replica left out.
  await expect(
    page.getByText("Stripe and Shop database: 21 tables.", { exact: true }),
  ).toBeVisible();
  const sent = page.waitForRequest(/ont_acme_contracts:generate$/);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  expect((await sent).postDataJSON().connectorIds).toEqual([
    "conn_acme_stripe",
    "conn_acme_shop",
  ]);
});

test("shows why a generation failed and retries with the same choices", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 300, ontologyOutcome: "fail" });
  await signIn(page);
  await page.goto(`${O}/generate`);
  await page.getByLabel("Goal").fill("Billing health");
  await page
    .getByRole("list", { name: "Connectors" })
    .getByRole("checkbox", { name: /Shop database/ })
    .uncheck();
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(page.getByText("The draft failed")).toBeVisible({
    timeout: 15_000,
  });
  await expect(
    page.getByText("Claude declined to draft this ontology."),
  ).toBeVisible();
  await page.getByRole("link", { name: "Try again" }).click();
  await expect(page.getByLabel("Goal")).toHaveValue("Billing health");
  const connectors = page.getByRole("list", { name: "Connectors" });
  await expect(
    connectors.getByRole("checkbox", { name: /Stripe/ }),
  ).toBeChecked();
  await expect(
    connectors.getByRole("checkbox", { name: /Shop database/ }),
  ).not.toBeChecked();
});

test("says why a generation is retrying and when it runs next", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 2000, ontologyOutcome: "retry" });
  await signIn(page);
  await page.goto(`${O}/generate`);
  await page.getByLabel("Goal").fill("Order health");
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(page).toHaveURL(
    /\/ontology\/ont_acme_contracts\/generations\/gen_/,
  );
  // The first attempt failed in a way Helix retries: queued again.
  await expect(page.getByText("Retrying", { exact: true })).toBeVisible({
    timeout: 15_000,
  });
  const callout = page
    .getByRole("status")
    .filter({ hasText: "An attempt failed. Helix will try again" });
  await expect(callout).toContainText(
    "The ontology agent is unavailable; Helix will retry. Next attempt",
  );
  await expect(page.getByText("Picked up by the agent (done)")).toBeVisible();
  // Then the second attempt stores the draft.
  await expect(page.getByText("Succeeded", { exact: true })).toBeVisible({
    timeout: 15_000,
  });
  await expect(callout).toHaveCount(0);
  await expect(page.getByText("2 attempts")).toBeVisible();
});

test("allows one live generation at a time", async ({ page }) => {
  await mock.configure({ ontologyStepMs: 60_000 });
  await signIn(page);
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(
    page.locator(".hf-page-header-meta").getByText("Queued", { exact: true }),
  ).toBeVisible();
  await page.goto(O);
  await expect(page.getByText("A new ontology is queued")).toBeVisible();
  await page.goto(`${O}/generate`);
  await expect(page.getByText("A generation is already queued.")).toBeVisible();
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(
    page.getByText("An ontology generation is already queued or running."),
  ).toBeVisible();
});

test("names a generation by its title and shows its goal once", async ({
  page,
}) => {
  await signIn(page);
  // The versions list names each by its generation's title; the oldest is
  // from before titles, so it reads its goal. A failed one links to its
  // page, saying why.
  await page.goto(`${O}#versions`);
  const grid = page.getByRole("grid", { name: "Ontology versions" });
  await expect(grid.getByRole("columnheader", { name: "Title" })).toBeVisible();
  await expect(grid.locator(".on-cell-text")).toHaveText([
    "Customer contracts and billing",
    "Order tracing through shipments and returns",
    "Late order shipments · none of the tables to generate from are synced any more; sync a connector and try again",
    "Customer billing health",
  ]);
  await expect(
    grid.getByRole("link", { name: /^Late order shipments/ }),
  ).toHaveAttribute("href", `${O}/generations/gen_seed_2`);
  // A version links to its generation by title.
  await grid.getByRole("link", { name: "Version 3", exact: true }).click();
  await page
    .locator(".hf-page-header-meta")
    .getByRole("link", { name: "Customer contracts and billing" })
    .click();
  const heading = page.getByRole("heading", { level: 1 });
  await expect(heading).toHaveText("Customer contracts and billing");
  await expect(heading.locator("[title]")).toHaveAttribute(
    "title",
    "Customer contracts and billing",
  );
  await expect(page).toHaveTitle(
    "Customer contracts and billing · Helix Foundry",
  );
  // The goal shows whole, once, below the drafting.
  await expect(goalPanel(page)).toContainText(
    "Contracts with our customers: who we signed with and what they are billed.",
  );
  await expect(
    page.getByText(/who we signed with and what they are billed/),
  ).toHaveCount(1);
  await expect(page.locator(".hf-kv").getByText("Goal")).toHaveCount(0);

  // A generation's page sits under Overview in the panel.
  const overviewLink = page
    .getByRole("complementary", { name: "Ontologies" })
    .getByRole("link", { name: "Overview" });
  await expect(overviewLink).toHaveAttribute("aria-current", "page");
  // Untitled: named from the goal.
  await page.goto(`${O}/generations/gen_seed_1`);
  await expect(heading).toHaveText("Customer billing health");
  await expect(page).toHaveTitle("Customer billing health · Helix Foundry");

  // A WFE without titles yet: the goal's first sentence, cut to 60
  // characters at a word.
  await page.route(`**/papi/**/generations/gen_seed_4`, async (route) => {
    const res = await route.fetch(),
      body = await res.json();
    delete body.generation.title;
    await route.fulfill({ response: res, json: body });
  });
  await page.goto(`${O}/generations/gen_seed_4`);
  await expect(heading).toHaveText(
    "Contracts with our customers: who we signed with and what…",
  );
  await expect(
    page
      .getByRole("navigation", { name: "Breadcrumb" })
      .locator("[aria-current]"),
  ).toHaveText("Contracts with our customers: who we signed with and what…");
});

test("clamps a long goal to four lines until asked, and keeps the title on one line", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 60_000 });
  await signIn(page);
  await page.goto(`${O}/generate`);
  const lines = [
    "Help our customer service and operations teams trace every order from the customer who placed it through its items, payments, shipments, carriers and any returns or refunds.",
    "They should be able to answer:",
    "- where is this customer's order right now?",
    "- which orders shipped late, and which carrier was responsible?",
    "- which returns were refunded, and how long did each take?",
    "- which products are returned most, and why?",
  ];
  await page.getByLabel("Goal").fill(lines.join("\n"));
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  const title = "Customer service and operations teams trace every order";
  const heading = page.getByRole("heading", { level: 1 });
  await expect(heading).toHaveText(title);
  // The live callout names it by title too.
  await page.goto(O);
  await expect(
    page.getByRole("status").filter({ hasText: "A new ontology is queued" }),
  ).toContainText(`From 21 tables, for “${title}”.`);
  await page.getByRole("link", { name: "View progress" }).click();

  const goal = goalPanel(page),
    text = goal.locator("p"),
    more = goal.getByRole("button", { name: "Show more" });
  await expect(more).toHaveAttribute("aria-expanded", "false");
  // Four lines of it, its line breaks kept.
  const clamped = await text.evaluate((el) => ({
    lines: Math.round(
      el.clientHeight / parseFloat(getComputedStyle(el).lineHeight),
    ),
    hidden: el.scrollHeight > el.clientHeight,
  }));
  expect(clamped).toEqual({ lines: 4, hidden: true });
  await more.click();
  const less = goal.getByRole("button", { name: "Show less" });
  await expect(less).toHaveAttribute("aria-expanded", "true");
  expect(await text.innerText()).toBe(lines.join("\n"));
  expect(
    await text.evaluate((el) => el.scrollHeight > el.clientHeight + 1),
  ).toBe(false);
  await less.click();
  await expect(more).toBeVisible();

  // At phone width the title stays on one line, cut with an ellipsis, whole
  // in its tooltip, and the page doesn't scroll sideways.
  await page.setViewportSize({ width: 390, height: 844 });
  const line = heading.locator(".on-title");
  await expect(line).toHaveAttribute("title", title);
  await expect
    .poll(() =>
      line.evaluate((el) => ({
        cut: el.scrollWidth > el.clientWidth,
        oneLine:
          el.clientHeight <= parseFloat(getComputedStyle(el).lineHeight) + 1,
      })),
    )
    .toEqual({ cut: true, oneLine: true });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  // The goal still shows Show more at that width.
  await expect(more).toBeVisible();
});

test("starts a new ontology empty, drafting from the workspace's synced connectors", async ({
  page,
}) => {
  await signIn(page);
  // A fresh ontology on Staging's empty database.
  // Another ontology's live generation doesn't hold a new one's first.
  await startOntology(page, "Scratch");
  await page.getByRole("radio", { name: /sandbox/ }).check();
  await page.getByRole("button", { name: "Continue" }).click();
  // Acme's connectors are synced, so the wizard offers them.
  await expect(
    page.getByRole("list", { name: "Connectors" }).getByRole("checkbox"),
  ).toHaveCount(2);
  await page.getByRole("button", { name: "Back" }).click();
  await finishOntology(page);
  await createdOntology(page);
});

test("the BFF refuses ontology writes the UI never makes", async ({ page }) => {
  await signIn(page);
  const token = await csrfToken(page);
  const headers = { origin: APP_ORIGIN, "x-helix-csrf": token };
  const create = await page.request.post(
    "/papi/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts/versions",
    { data: { definition: {} }, headers },
  );
  expect(create.status()).toBe(405);
  expect(create.headers()["allow"]).toBe("GET");
  const remove = await page.request.delete(
    "/papi/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts/versions/ver_seed_1",
    { headers },
  );
  expect(remove.status()).toBe(405);
  expect(
    (await mock.stats()).requests.filter((r) => /\/versions/.test(r)),
  ).toEqual([]);
});

test("finds ontologies from the command palette", async ({ page }) => {
  await signIn(page);
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  await page.keyboard.press("ControlOrMeta+k");
  const palette = page.getByRole("dialog", { name: "Search your workspace" });
  await palette.getByRole("combobox").fill("Customer contracts");
  await palette.getByRole("option", { name: /Customer contracts/ }).click();
  await expect(page).toHaveURL(/\/ontology\/ont_acme_contracts$/);
});

test("shows a plain member the ontology without its changes", async ({
  page,
}) => {
  await signIn(page, "alan@example.com");
  await page.goto(O);
  await expect(page.getByText("Version 3 is ready to review")).toBeVisible();
  await expect(
    page.getByRole("link", { name: "Generate", exact: true }),
  ).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Make current" })).toHaveCount(
    0,
  );
  await page.goto(`${O}/versions/ver_seed_2`);
  await expect(page.getByText("Not current")).toBeVisible();
  await expect(page.getByRole("button", { name: "Make current" })).toHaveCount(
    0,
  );
  await page.goto(`${O}/generate`);
  await expect(
    page.getByText("Only owners and admins can generate"),
  ).toBeVisible();
});

test("offers Retry when a list couldn't be loaded", async ({ page }) => {
  const api =
      "**/papi/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts",
    down = (route: Route) =>
      route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({ code: 14, message: "upstream connect error" }),
      });
  await signIn(page);
  const grid = page.getByRole("grid", { name: "Ontology versions" });
  // Without the versions, nothing to list.
  await page.route(`${api}/versions?*`, down);
  await page.goto(`${O}#versions`);
  const failed = page.getByText("Couldn’t load the versions");
  await expect(failed).toBeVisible({ timeout: 10_000 });
  await page.unroute(`${api}/versions?*`);
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(failed).toHaveCount(0);
  await expect(grid.getByRole("row")).toHaveCount(5);
  // Without the generations, the versions still list, by what made them,
  // and can still be made current; the failed generation waits for Retry.
  await page.route(`${api}/generations?*`, down);
  await page.reload();
  await expect(grid.getByRole("row")).toHaveCount(4, { timeout: 10_000 });
  await expect(grid.getByRole("row").nth(1)).toContainText("Generated");
  await expect(
    grid.getByRole("link", { name: "Review version" }).first(),
  ).toBeVisible();
  await page.unroute(`${api}/generations?*`);
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(grid.getByRole("row")).toHaveCount(5);
  await expect(grid.getByRole("row").nth(1)).toContainText(
    "Customer contracts and billing",
  );
});
