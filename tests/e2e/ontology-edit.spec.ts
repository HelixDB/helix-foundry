import {
  choose,
  test,
  expect,
  signIn,
  mock,
  csrfToken,
  APP_ORIGIN,
} from "./support/mock";
const O = "/ontology/ont_acme_contracts";
const API = "/papi/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts";

async function loadComparable(page: import("@playwright/test").Page) {
  await mock.configure({ ontologyStepMs: 100, loadStepMs: 100 });
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current", exact: true }).click();
  const confirm = page.getByRole("dialog");
  await confirm
    .getByRole("button", { name: "Make current", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Load data", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Load data", exact: true }).click();
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Load", exact: true })
    .click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({
    timeout: 30_000,
  });
  await page.goto(`${O}/versions/ver_seed_3?tab=edit`);
}

async function addConstraint(
  page: import("@playwright/test").Page,
  expression = "violation(i) :- Invoice(i), i.amount_due > 1000000.",
) {
  await choose(page.getByLabel("Item to edit"), "Add constraint");
  await page.getByLabel("Item name", { exact: true }).fill("large_invoice");
  await page
    .getByLabel("Item TOML")
    .fill(
      `[constraints.large_invoice]\ndescription = "Large invoice needs approval."\nseverity = "warn"\ndatalog = '${expression}'`,
    );
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
}

test("edits a document route, identifiers and a constraint, previews, then creates an immutable version", async ({
  page,
}) => {
  await signIn(page);
  await loadComparable(page);
  await expect(
    page.getByRole("button", { name: "Create version", exact: true }),
  ).toBeDisabled();
  await choose(page.getByLabel("Item to edit"), "extraction.contracts");
  const route = page.getByLabel("Item TOML");
  await route.fill(
    (await route.inputValue()).replace(
      /documents\s*=\s*\[[\s\S]*?\]/,
      'documents = "all"',
    ),
  );
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
  await choose(page.getByLabel("Item to edit"), "type.Customer");
  await choose(
    page.getByRole("combobox", { name: "Identifier for email", exact: true }),
    "exact",
  );
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
  await addConstraint(page);
  const versions = async () =>
    (await (await page.request.get(`${API}/versions`)).json()).versions;
  expect(await versions()).toHaveLength(3);
  await page
    .getByRole("button", { name: "Preview edits", exact: true })
    .click();
  await expect(
    page.getByText("Preview ready. No version created.", { exact: true }),
  ).toBeVisible();
  const evidence = page.getByRole("region", { name: "Revision result" });
  await expect(
    evidence.getByText("constraint.large_invoice · warn", { exact: true }),
  ).toBeVisible();
  // The mock reads none of the comparable load's entities: capped at warn.
  await expect(evidence.getByText("entities", { exact: true })).toBeVisible();
  await expect(evidence).toContainText(
    "some of the load’s entities were over the read budget",
  );
  await expect(evidence).toContainText("6 of 6 rules evaluated.");
  expect(await versions()).toHaveLength(3);
  // Preview is tied to the exact batch: removing and re-adding invalidates it.
  await page
    .getByRole("button", { name: "Undo type.Customer", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Create version", exact: true }),
  ).toBeDisabled();
  await choose(page.getByLabel("Item to edit"), "type.Customer");
  await choose(
    page.getByRole("combobox", { name: "Identifier for email", exact: true }),
    "exact",
  );
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
  await page
    .getByRole("button", { name: "Preview edits", exact: true })
    .click();
  await expect(
    page.getByText("Preview ready. No version created.", { exact: true }),
  ).toBeVisible();
  // The shell takes its phone layout a moment after the resize.
  await page.setViewportSize({ width: 390, height: 844 });
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    )
    .toBe(true);
  await page
    .getByRole("button", { name: "Create version", exact: true })
    .click();
  await expect(
    page.getByText("Version created. Load it to apply these changes.", {
      exact: true,
    }),
  ).toBeVisible();
  expect(await versions()).toHaveLength(4);
  // The version holds the batch now: nothing is left to preview again.
  await expect(page.getByRole("region", { name: "Pending edits" })).toHaveCount(
    0,
  );
  await expect(
    page.getByRole("button", { name: "Preview edits", exact: true }),
  ).toBeDisabled();
  await page
    .getByRole("link", { name: "View new version", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Version 4", exact: true }),
  ).toBeVisible();
  await expect(page.getByText("Not current", { exact: true })).toBeVisible();
  // Its generation is its edits, not another copy of the parent's draft.
  await expect(
    page.getByRole("link", { name: "See its edits", exact: true }),
  ).toBeVisible();
  await page
    .getByRole("link", { name: "Schema versions", exact: true })
    .first()
    .click();
  const row = page
    .getByRole("grid", { name: "Ontology versions" })
    .getByRole("row")
    .filter({ hasText: "Version 4" });
  await expect(row.getByRole("button", { name: "Make current" })).toHaveCount(
    0,
  );
  await row.getByRole("link", { name: "Review version" }).click();

  // Its page: the overview it leaves offers Make current twice, in the
  // ready-to-review callout and the versions list.
  await expect(page).toHaveURL(/\/versions\/ver_/);
  await expect(
    page.getByRole("button", { name: "Make current", exact: true }),
  ).toBeVisible();
  await page.getByRole("tab", { name: /^Sources/ }).click();
  await expect(
    page.getByRole("table", { name: "Sources", exact: true }),
  ).toContainText("All documents");
  await page.getByRole("tab", { name: /^Rules/ }).click();
  await expect(page.getByText("large_invoice", { exact: true })).toBeVisible();
  const list = await versions();
  const version = (
    await (await page.request.get(`${API}/versions/${list[0].id}`)).json()
  ).version;
  expect(version.definition.ontology_toml).toContain('email = "exact"');
  const original = (
    await (await page.request.get(`${API}/versions/ver_seed_3`)).json()
  ).version;
  expect(original.definition.ontology_toml).toContain('email = "email"');
  expect(version.definition.rules).toEqual({ count: 6, evaluated: 6 });
  // An accepted no-load warn cap remains loadable. It cannot enforce block.
  const moved = await page.request.post(`${API}:set-current`, {
    data: { versionId: version.id, expectedCurrentVersionId: "ver_seed_3" },
    headers: { origin: APP_ORIGIN, "x-helix-csrf": await csrfToken(page) },
  });
  expect(moved.status()).toBe(200);
  await page.reload();
  await expect(
    page.getByRole("button", { name: "Load data", exact: true }),
  ).toBeVisible();
  const loaded = await page.request.post(`${API}/loads`, {
    data: { versionId: version.id, clientToken: "evaluated" },
    headers: { origin: APP_ORIGIN, "x-helix-csrf": await csrfToken(page) },
  });
  expect(loaded.status()).toBe(200);
  // The new route must remain a single-segment, explicitly authorized write.
  expect(
    (
      await page.request.post(`${API}/versions/ver_seed_3/extra:revise`, {
        data: { edits: [], dryRun: true },
        headers: { origin: APP_ORIGIN, "x-helix-csrf": await csrfToken(page) },
      })
    ).status(),
  ).toBe(404);
});

test("keeps staged edits across tabs and isolates them by version", async ({
  page,
}) => {
  await signIn(page);
  await page.goto(`${O}/versions/ver_seed_3?tab=edit`);
  await addConstraint(page);
  const pending = page.getByRole("region", { name: "Pending edits" });
  await expect(pending).toContainText("constraint.large_invoice");
  for (const tab of [/^Graph/, /^Rules/, /^TOML/]) {
    await page.getByRole("tab", { name: tab }).click();
    await page.getByRole("tab", { name: /^Edit/ }).click();
    await expect(pending).toContainText("constraint.large_invoice");
    await expect(
      page.getByRole("button", { name: "Preview edits", exact: true }),
    ).toBeEnabled();
    await expect(
      page.getByRole("button", { name: "Create version", exact: true }),
    ).toBeDisabled();
  }
  // Client-side navigation can reuse VersionPage, but must reset its edits.
  await page
    .getByRole("link", { name: "Schema versions", exact: true })
    .first()
    .click();
  await page
    .getByRole("link", { name: "Version 2", exact: true })
    .first()
    .click();
  await page.getByRole("tab", { name: /^Edit/ }).click();
  await expect(pending).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Preview edits", exact: true }),
  ).toBeDisabled();
});

test("shows a no-load warn cap, then refuses an unmeasured rule and time constraint", async ({
  page,
}) => {
  await signIn(page);
  await mock.configure({ ontologyStepMs: 100 });
  await page.goto(`${O}/versions/ver_seed_3?tab=edit`);
  await addConstraint(page);
  await page
    .getByRole("button", { name: "Preview edits", exact: true })
    .click();
  await expect(
    page.getByText("no_comparable_load", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("region", { name: "Revision result" }),
  ).toContainText("6 of 6 rules evaluated.");
  await expect(
    page.getByRole("button", { name: "Create version", exact: true }),
  ).toBeEnabled();
  await choose(page.getByLabel("Item to edit"), "Add rule");
  await page.getByLabel("Item name", { exact: true }).fill("large_invoice");
  await page
    .getByLabel("Item TOML")
    .fill(
      "[rules.large_invoice]\ndescription = \"Signed over 30 days ago.\"\ndatalog = 'overdue(c) :- Contract(c), c.signed_on < today - 30d.'",
    );
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
  await choose(page.getByLabel("Item to edit"), "Add constraint");
  await page.getByLabel("Item name", { exact: true }).fill("time_check");
  await page
    .getByLabel("Item TOML")
    .fill(
      '[constraints.time_check]\ndescription = "Overdue contracts."\nseverity = "warn"\ndatalog = \'violation(c) :- overdue(c).\'',
    );
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
  await page
    .getByRole("button", { name: "Preview edits", exact: true })
    .click();
  await expect(page.getByText("Validation", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Create version", exact: true }),
  ).toBeDisabled();
  await page.getByRole("link", { name: "View revision", exact: true }).click();
  await expect(page.getByText("Edits refused.", { exact: true })).toBeVisible();
});

test("refuses edits from members and shows validation and worker failures", async ({
  page,
}) => {
  await signIn(page);
  await page.goto(`${O}/versions/ver_seed_3?tab=edit`);
  await choose(page.getByLabel("Item to edit"), "extraction.contracts");
  await page
    .getByLabel("Item TOML")
    .fill('[mappings.customers]\nsource = "wrong"');
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "EditNotAllowed" }),
  ).toContainText("EditNotAllowed");
  await expect(
    page.getByRole("button", { name: "Preview edits", exact: true }),
  ).toBeDisabled();
  await page.getByRole("button", { name: "Remove item", exact: true }).click();
  await mock.configure({ ontologyStepMs: 100, ontologyOutcome: "fail" });
  await page
    .getByRole("button", { name: "Preview edits", exact: true })
    .click();
  await expect(
    page.getByText("Revision failed.", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Create version", exact: true }),
  ).toBeDisabled();
  await page.getByRole("button", { name: "Account", exact: true }).click();
  await page.getByRole("menuitem", { name: "Sign out", exact: true }).click();
  await expect(page).toHaveURL(/\/login$/);
  await signIn(page, "alan@example.com");
  await page.goto(`${O}/versions/ver_seed_3?tab=edit`);
  await expect(
    page.getByText("Only owners and admins can edit versions.", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Preview edits", exact: true }),
  ).toHaveCount(0);
});

test("refuses a rule without a description, and a route naming a set by its name", async ({
  page,
}) => {
  await signIn(page);
  await page.goto(`${O}/versions/ver_seed_3?tab=edit`);
  await choose(page.getByLabel("Item to edit"), "Add rule");
  await page.getByLabel("Item name", { exact: true }).fill("large");
  await page
    .getByLabel("Item TOML")
    .fill("[rules.large]\ndatalog = 'large(i) :- Invoice(i).'");
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "description" }),
  ).toHaveText("Enter a description.");
  await choose(page.getByLabel("Item to edit"), "extraction.contracts");
  // The page lists the sets' ids, since a route names sets by id.
  await expect(
    page.getByText("Set ids: General = set_acme_general"),
  ).toBeVisible();
  const route = page.getByLabel("Item TOML");
  await route.fill(
    (await route.inputValue()).replace(
      /documents\s*=\s*\[[\s\S]*?\]/,
      'documents = ["General"]',
    ),
  );
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "General" }),
  ).toHaveText("General is a set's name. Use its id: set_acme_general.");
  await expect(page.getByRole("region", { name: "Pending edits" })).toHaveCount(
    0,
  );
  // An untouched type without identifiers stages nothing.
  await choose(page.getByLabel("Item to edit"), "type.Invoice");
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
  await expect(page.getByRole("region", { name: "Pending edits" })).toHaveCount(
    0,
  );
});

test("keeps a running create across tabs, and names revisions by what they do", async ({
  page,
}) => {
  await signIn(page);
  await mock.configure({ ontologyStepMs: 100 });
  await page.goto(`${O}/versions/ver_seed_3?tab=edit`);
  await addConstraint(page);
  await page
    .getByRole("button", { name: "Preview edits", exact: true })
    .click();
  await expect(
    page.getByText("Preview ready. No version created.", { exact: true }),
  ).toBeVisible();
  await mock.configure({ ontologyStepMs: 3000 });
  await page
    .getByRole("button", { name: "Create version", exact: true })
    .click();
  await expect(
    page.getByText("Checking edits…", { exact: true }),
  ).toBeVisible();
  // Another tab and back: the create is still the page's.
  await page.getByRole("tab", { name: /^Rules/ }).click();
  await page.getByRole("tab", { name: /^Edit/ }).click();
  await expect(
    page.getByText("Checking edits…", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Preview edits", exact: true }),
  ).toBeDisabled();
  // The overview's callout says what runs, not the parent's goal.
  await page.goto(O);
  await expect(page.getByText("Creating an edited version")).toBeVisible();
  await expect(page.getByText("Drafting a new ontology")).toHaveCount(0);
  // The versions list says so above it, then lists the new version by what
  // made it. A preview stored nothing, so it isn't listed.
  await page.goto(`${O}#versions`);
  await expect(page.getByText("Creating an edited version")).toBeVisible();
  const rows = page
    .getByRole("grid", { name: "Ontology versions" })
    .getByRole("row");
  await expect(rows.filter({ hasText: "Edited version" })).toContainText(
    "Version 4",
    { timeout: 15_000 },
  );
  await expect(page.getByText("Creating an edited version")).toHaveCount(0);
  await expect(rows.filter({ hasText: "Preview edits" })).toHaveCount(0);
});

test("refuses a revision measured past its budget, in words", async ({
  page,
}) => {
  await signIn(page);
  await loadComparable(page);
  await mock.configure({ reviseOutcome: "budget" });
  await addConstraint(page);
  await page
    .getByRole("button", { name: "Preview edits", exact: true })
    .click();
  const evidence = page.getByRole("region", { name: "Revision result" });
  await expect(
    evidence.getByText("Edits refused.", { exact: true }),
  ).toBeVisible();
  await expect(evidence).toContainText(
    "Measuring these edits on the current load needed more than its budget, so none was accepted.",
  );
  await expect(evidence.getByText("budget", { exact: true })).toBeVisible();
  await expect(evidence).not.toContainText(" · warn");
  await expect(
    page.getByRole("button", { name: "Create version", exact: true }),
  ).toBeDisabled();
});

test("offers no editing of an unfinished draft", async ({ page }) => {
  await signIn(page);
  await page.route(`**${API}/versions/ver_seed_3`, async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    body.version.definition.status = "needs_input";
    await route.fulfill({ response, json: body });
  });
  await page.goto(`${O}/versions/ver_seed_3?tab=edit`);
  await expect(
    page.getByText("Finish this draft with the agent before editing it.", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(page.getByLabel("Item to edit")).toHaveCount(0);
});

test("hides what Revise measured on the load from one who can't query its database", async ({
  page,
}) => {
  await signIn(page);
  await mock.configure({ ontologyStepMs: 100 });
  const created = await page.request.post(`${API}/versions/ver_seed_3:revise`, {
    data: {
      edits: [
        {
          put: {
            itemId: "constraint.large_invoice",
            toml: `[constraints.large_invoice]\ndescription = "Large invoice needs approval."\nseverity = "warn"\ndatalog = 'violation(i) :- Invoice(i), i.amount_due > 1000000.'`,
          },
        },
      ],
      dryRun: false,
    },
    headers: { origin: APP_ORIGIN, "x-helix-csrf": await csrfToken(page) },
  });
  expect(created.status()).toBe(200);
  const generation = (await created.json()).generation.id;
  await mock.configure({ ontologyDatabaseHidden: true });
  await page.goto(`${O}/generations/${generation}`);
  const evidence = page.getByRole("region", { name: "Revision result" });
  await expect(
    evidence.getByText("Version created. Load it to apply these changes.", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    evidence.getByText("Evidence hidden", { exact: true }),
  ).toBeVisible();
  await expect(evidence).not.toContainText(" · warn");
  await page
    .getByRole("link", { name: "View new version", exact: true })
    .click();
  await page.getByRole("tab", { name: /^Rules/ }).click();
  await expect(page.getByText("large_invoice", { exact: true })).toBeVisible();
  await expect(
    page.getByText("Evidence hidden", { exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("region", { name: "Evidence card" })).toHaveCount(
    0,
  );
  // The parent's cards measure no load: they stay.
  await page.goto(`${O}/versions/ver_seed_3?tab=rules`);
  await expect(
    page.getByRole("region", { name: "Evidence card" }).first(),
  ).toBeVisible();
  await expect(page.getByText("Evidence hidden", { exact: true })).toHaveCount(
    0,
  );
});
