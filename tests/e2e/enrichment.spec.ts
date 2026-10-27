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

// People enrichment (website e2e): the owner's consent
// and credit cap in Settings, turning it on in a version's Edit tab and
// loading it, the agent's suggestion, a load's step and Stop, how each way
// the lookups stop leaves the load, and turning consent off. Acme's Sales
// graph enriches Person from the Shop database's public.people
// (mock/fixtures/sales-graph.ts): its current version 2 as the agent
// suggested, version 1 not at all. Of the 9 people a load looks up, the fake
// context.dev finds Ada, Grace, Alan and Katherine, holds four back and
// finds no match for Bea.

const O = "/ontology/ont_acme_sales",
  X = "/explore/ont_acme_sales";
const FAST = { loadStepMs: 150, enrichmentStepMs: 150 } as const;

/** The owner's choice through /papi, as Settings › People enrichment saves it. */
async function consent(page: Page, enabled: boolean, cap = 50_000) {
  const res = await page.request.patch(
    "/papi/v1/workspaces/ws_acme/foundry/enrichment",
    {
      data: {
        enabled,
        monthlyCreditCap: String(cap),
        noticeVersion: "2026-10-20",
      },
      headers: { origin: APP_ORIGIN, "x-helix-csrf": await csrfToken(page) },
    },
  );
  expect(res.status()).toBe(200);
}

async function signOut(page: Page) {
  await page.getByRole("button", { name: "Account", exact: true }).click();
  await page.getByRole("menuitem", { name: "Sign out", exact: true }).click();
  await expect(page).toHaveURL(/\/login$/);
}

/** The load dialog of the Sales graph's current version, from its Loads page. */
async function loadDialog(page: Page, version = 2) {
  await page.goto(`${O}#loads`);
  await page
    .getByRole("main")
    .getByRole("button", { name: "Load data" })
    .first()
    .click();
  return page.getByRole("dialog", {
    name: `Load version ${version} into sales?`,
  });
}

/** Loads the current version and waits on its page until it is loaded. */
async function load(page: Page, version = 2) {
  await (await loadDialog(page, version))
    .getByRole("button", { name: "Load" })
    .click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await loaded(page);
}

const loaded = (page: Page) =>
  expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 30_000 });
/** The load page's step for People enrichment. */
const step = (page: Page) =>
  page.locator(".feed-step").filter({ hasText: "People enrichment ·" });
/** The page's header, with its status badges. */
const header = (page: Page) => page.locator(".hf-page-header");
/** The load page's callout of what the step applied. */
const outcome = (page: Page) =>
  page.getByRole("status").filter({ hasText: /^People enrichment/ });

/** Ada's record in the Explorer's Records: its detail dialog. */
async function adaRecord(page: Page) {
  await page.goto(`${X}/records`);
  await page
    .getByRole("list", { name: "Types" })
    .getByRole("button", { name: /^Person/ })
    .click();
  await page
    .getByRole("grid", { name: "Person records" })
    .getByRole("row")
    .filter({ hasText: "Ada Lovelace" })
    .click();
  return page.getByRole("dialog", { name: "Ada Lovelace" });
}

/** Ada's page in the Explorer, from Records. */
async function openAda(page: Page) {
  await (await adaRecord(page))
    .getByRole("link", { name: "Open its page" })
    .click();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "Ada Lovelace",
  );
}

/** The load page's Details value for `label`. */
const detailValue = (page: Page, label: string) =>
  page
    .locator(".hf-kv-row")
    .filter({ has: page.getByRole("term").filter({ hasText: label }) })
    .getByRole("definition");

test("the owner allows People enrichment with a cap; members read it; Free needs Pro", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/settings/enrichment");
  const allow = page.getByRole("switch", { name: "Allow People enrichment" });
  await expect(allow).not.toBeChecked();
  await allow.check();
  await expect(
    page.getByText(
      "Helix sends the work emails and LinkedIn URLs of people in your tables to context.dev to find public professional profiles, and stores the matches.",
    ),
  ).toBeVisible();
  const save = page.getByRole("button", { name: "Allow People enrichment" });
  // Not before the owner confirms a lawful basis.
  await expect(save).toBeDisabled();
  await page.getByLabel("Monthly credit cap").fill("20000");
  await page
    .getByRole("checkbox", {
      name: "I confirm we have a lawful basis to do this.",
    })
    .check();
  await save.click();
  await expect(page.getByText("People enrichment is on")).toBeVisible();
  await expect(
    page.getByText(
      /^0 of 20,000 credits used this month · resets \w+ \d+ · Allowed by Ada Lovelace on \w+ \d+$/,
    ),
  ).toBeVisible();
  // At most 100,000 a month.
  const cap = page.getByLabel("Monthly credit cap");
  await cap.fill("100001");
  await expect(
    page.getByText("Enter a whole number from 0 to 100,000."),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Save cap" })).toBeDisabled();
  // A cap of 0 is a cap: the form shows it as saved.
  await cap.fill("0");
  await page.getByRole("button", { name: "Save cap" }).click();
  await expect(page.getByText("The cap is 0 credits a month")).toBeVisible();
  await expect(cap).toHaveValue("0");
  await expect(page.getByRole("button", { name: "Save cap" })).toBeDisabled();
  await cap.fill("30000");
  await page.getByRole("button", { name: "Save cap" }).click();
  await expect(
    page.getByText("The cap is 30,000 credits a month"),
  ).toBeVisible();

  // A member reads the setting.
  await signOut(page);
  await signIn(page, "alan@example.com");
  await page.goto("/settings/enrichment");
  await expect(
    page.getByText("On · cap 30,000 · 0 used. Only owners can change this."),
  ).toBeVisible();
  await expect(page.getByRole("switch")).toHaveCount(0);

  // A workspace on Free needs Pro to allow it.
  await signOut(page);
  await signInToNewWorkspace(page);
  await page.goto("/settings/enrichment");
  await expect(
    page.getByText("People enrichment needs Foundry Pro"),
  ).toBeVisible();
  await expect(
    page.getByRole("switch", { name: "Allow People enrichment" }),
  ).toBeDisabled();
});

test("an admin turns it on in Edit, saves it as a version, then makes it current and loads it", async ({
  page,
}) => {
  await mock.configure({ ...FAST, alanRole: "WORKSPACE_ROLE_ADMIN" });
  await signIn(page);
  await consent(page, true);
  await signOut(page);
  await signIn(page, "alan@example.com");
  await page.goto(`${O}/versions/ver_sales_1?tab=edit`);
  // A rule staged first, which the mock's Revise can't measure: the save
  // that carries it is refused.
  await choose(page.getByLabel("Item to edit"), "Add rule");
  await page.getByLabel("Item name", { exact: true }).fill("everyone");
  await page
    .getByLabel("Item TOML")
    .fill(
      "[rules.everyone]\ndescription = \"Every person.\"\ndatalog = 'everyone(p) :- Person(p).'",
    );
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
  await choose(page.getByLabel("Item to edit"), "type.Person");
  const card = page.getByRole("region", {
    name: "People enrichment of Person",
  });
  await expect(card.getByRole("heading", { level: 3 })).toHaveText(
    "Person · key contact_id · names full_name",
  );
  await card.getByRole("switch", { name: "Enrich people" }).check();
  await expect(
    card.getByText(
      "Look up public work profiles of each Person on context.dev. 20 credits per profile found.",
    ),
  ).toBeVisible();
  // The fields the version's tables fill, as the agent would pick them.
  const email = card.getByLabel("Email comes from"),
    linkedin = card.getByLabel("LinkedIn URL comes from");
  await expect(email).toHaveText("email");
  await expect(linkedin).toHaveText("linkedin_url");
  await choose(linkedin, "email");
  // Both selects say so.
  await expect(card.getByText("Pick different fields.")).toHaveCount(2);
  await choose(linkedin, "linkedin_url");
  await expect(card.getByLabel("Current title fills")).toHaveText(
    "current_title (new)",
  );
  const country = card.getByRole("checkbox", { name: "Country" });
  await country.uncheck();
  // The LinkedIn URL identified by hand: until it is staged, the form can't
  // save, and staging it keeps the form's choices.
  const linkedinId = page.getByLabel("Identifier for linkedin_url"),
    saveAs = card.getByRole("button", { name: "Save as version 3" });
  await choose(linkedinId, "exact");
  await expect(saveAs).toBeDisabled();
  await expect(
    page.getByText("Stage this item before previewing."),
  ).toBeVisible();
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
  await expect(linkedinId).toHaveText("exact");
  await expect(
    card.getByRole("switch", { name: "Enrich people" }),
  ).toBeChecked();
  await expect(country).not.toBeChecked();
  await expect(linkedin).toHaveText("linkedin_url");
  await expect(saveAs).toBeEnabled();
  await saveAs.click();
  // Refused: the form stays open with what was picked, to fix and save
  // again.
  await expect(
    page.getByText(
      "Couldn’t save: Mock has no load evaluator; derived rules require measurements.",
    ),
  ).toBeVisible({ timeout: 15_000 });
  const enrich = card.getByRole("switch", { name: "Enrich people" }),
    refused = page.getByText(
      "Couldn’t save: Mock has no load evaluator; derived rules require measurements.",
    ),
    pending = page
      .getByRole("region", { name: "Pending edits" })
      .getByRole("button");
  await expect(enrich).toBeChecked();
  await expect(country).not.toBeChecked();
  const tried = [
    "Undo rule.everyone",
    "Undo type.Person",
    "Undo link.worked_at",
    "Undo enrichment.people",
  ];
  await expect(pending).toHaveText(tried);
  // While the type's identifiers are being edited, setting the form back to
  // the version keeps the refused try staged, and the identifiers as picked.
  const contactId = page.getByLabel("Identifier for contact_id");
  await choose(contactId, "exact");
  await enrich.uncheck();
  await expect(enrich).not.toBeChecked();
  await expect(pending).toHaveText(tried);
  await expect(contactId).toHaveText("exact");
  await expect(
    page.getByText("Stage this item before previewing."),
  ).toBeVisible();
  // Once they are staged, the form, still off, takes the try back to the
  // identifiers staged by hand, the LinkedIn URL's included.
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
  await expect(pending).toHaveText(["Undo rule.everyone", "Undo type.Person"]);
  await expect(refused).toHaveCount(0);
  await expect(enrich).not.toBeChecked();
  await expect(contactId).toHaveText("exact");
  await expect(linkedinId).toHaveText("exact");
  // Tried again and refused, then the type's put undone: the try keeps what
  // its item needs of the type, and the identifiers staged by hand go.
  await enrich.check();
  await country.uncheck();
  await saveAs.click();
  await expect(refused).toBeVisible({ timeout: 15_000 });
  await expect(pending).toHaveText(tried);
  await expect(contactId).toHaveText("exact");
  await page
    .getByRole("button", { name: "Undo type.Person", exact: true })
    .click();
  await expect(pending).toHaveText(tried);
  await expect(contactId).toHaveText("Not an identifier");
  await expect(enrich).toBeChecked();
  await expect(country).not.toBeChecked();
  // Off, as version 1 has it, the form takes back what the refused save
  // staged, and has nothing to save.
  await enrich.uncheck();
  await expect(pending).toHaveText(["Undo rule.everyone"]);
  await expect(refused).toHaveCount(0);
  await expect(enrich).not.toBeChecked();
  await expect(
    card.getByRole("button", { name: "Save as version 3" }),
  ).toHaveCount(0);
  // Undoing the refused item takes back the props and the link it added.
  await enrich.check();
  await country.uncheck();
  await card.getByRole("button", { name: "Save as version 3" }).click();
  await expect(refused).toBeVisible({ timeout: 15_000 });
  await expect(pending).toHaveText([
    "Undo rule.everyone",
    "Undo type.Person",
    "Undo link.worked_at",
    "Undo enrichment.people",
  ]);
  await page
    .getByRole("button", { name: "Undo enrichment.people", exact: true })
    .click();
  await expect(pending).toHaveText(["Undo rule.everyone"]);
  await expect(enrich).not.toBeChecked();
  await enrich.check();
  await country.uncheck();
  await page
    .getByRole("button", { name: "Undo rule.everyone", exact: true })
    .click();
  await card.getByRole("button", { name: "Save as version 3" }).click();
  await expect(page.getByText("Version 3 saved.")).toBeVisible({
    timeout: 15_000,
  });
  await page.getByRole("button", { name: "Make current and load" }).click();
  const dialog = page.getByRole("dialog", {
    name: "Load version 3 into sales?",
  });
  await expect(dialog).toContainText(
    "Looks up people earlier loads haven’t: 20 credits per profile found, 50,000 of 50,000 left this month. The first load takes longer and re-reads documents that mention people.",
  );
  await dialog.getByRole("button", { name: "Load" }).click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await loaded(page);
  await expect(step(page)).toContainText(
    "People enrichment · 9 looked up · 4 found (4 held) · 160 credits",
  );
  await expect(outcome(page)).toContainText(
    "4 profiles (4 new, 0 found before) · 4 applied.",
  );
  await expect(
    header(page).getByText("Current", { exact: true }),
  ).toBeVisible();
});

test("Make current and load after a save says why it was refused, and can be tried again", async ({
  page,
}) => {
  await mock.configure(FAST);
  await signIn(page);
  await page.goto(`${O}/versions/ver_sales_1?tab=edit`);
  await choose(page.getByLabel("Item to edit"), "type.Person");
  const card = page.getByRole("region", {
    name: "People enrichment of Person",
  });
  await card.getByRole("switch", { name: "Enrich people" }).check();
  await card.getByRole("button", { name: "Save as version 3" }).click();
  const saved = page.getByText("Version 3 saved.");
  await expect(saved).toBeVisible({ timeout: 15_000 });
  // Someone moved the current version since the page read it.
  const setCurrent = "**/papi/**/ontologies/ont_acme_sales:set-current";
  await page.route(setCurrent, (route) =>
    route.fulfill({
      status: 400,
      contentType: "application/json",
      body: JSON.stringify({
        code: 9,
        message:
          "the current ontology changed since you loaded it; reload and try again",
      }),
    }),
  );
  const make = page.getByRole("button", { name: "Make current and load" });
  await make.click();
  await expect(
    page.getByText(
      "The current ontology changed since you loaded it; reload and try again",
    ),
  ).toBeVisible();
  await expect(page).toHaveURL(/tab=edit/);
  await expect(saved).toBeVisible();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await page.unroute(setCurrent);
  await make.click();
  await expect(
    page.getByRole("dialog", { name: "Load version 3 into sales?" }),
  ).toBeVisible();
});

test("shows the agent's suggestion with its confidence, and who the last load could look up", async ({
  page,
}) => {
  await mock.configure(FAST);
  await signIn(page);
  await page.goto(`${O}/versions/ver_sales_2?tab=types`);
  const panel = page.getByRole("tabpanel");
  await expect(panel.getByText("Enriched · email, LinkedIn")).toBeVisible();
  await expect(panel.getByText("Suggested by the agent")).toBeVisible();
  await expect(
    panel.getByText(
      "Why: CQ1 asks each contact's current title; people.email fills Person.email.",
    ),
  ).toBeVisible();
  await expect(
    panel.getByText(
      "Confidence: High · 12 of 14 people have a usable email or LinkedIn (86%) · 0 shared · 1 without a full name",
    ),
  ).toBeVisible();
  await expect(panel.getByText("From People enrichment").first()).toBeVisible();
  // Without consent, loads apply no profiles.
  await expect(
    panel.getByText(
      "People enrichment is off for this workspace, so loads apply no profiles. An owner can allow it in Settings.",
    ),
  ).toBeVisible();

  await consent(page, true);
  await load(page);
  await page.goto(`${O}/versions/ver_sales_2?tab=types`);
  await expect(
    panel.getByText("Last load: 9 of 14 could be looked up · 4 found"),
  ).toBeVisible();
  await expect(panel.getByText(/is off for this workspace/)).toHaveCount(0);

  // Edit enrichment opens the type in Edit; turning it off keeps the props.
  await panel.getByRole("link", { name: "Edit enrichment" }).click();
  await expect(page).toHaveURL(/tab=edit&item=type\.Person/);
  const card = page.getByRole("region", {
    name: "People enrichment of Person",
  });
  await expect(card.getByText("Enriched · email, LinkedIn")).toBeVisible();
  // Link each employer, ticked again, links as the version's item does.
  const employers = card.getByRole("checkbox", { name: "Link each employer" }),
    saveAs = card.getByRole("button", { name: "Save as version 3" });
  await employers.uncheck();
  await expect(card.getByLabel("Employer type")).toHaveCount(0);
  await expect(saveAs).toBeVisible();
  await employers.check();
  await expect(card.getByLabel("Link", { exact: true })).toHaveText(
    "worked_at",
  );
  await expect(card.getByLabel("Employer type")).toHaveText("Company");
  await expect(card.getByLabel("Matched by")).toHaveText("domain");
  await expect(saveAs).toHaveCount(0);
  await card.getByRole("switch", { name: "Enrich people" }).uncheck();
  await expect(
    card.getByText(
      "Lookups stop from the next load. current_title, location, country, profile_url and worked_at stay, empty.",
    ),
  ).toBeVisible();
  await expect(saveAs).toBeVisible();
  // While the item stays, its email clue stays an email identifier.
  await choose(
    page.getByRole("combobox", { name: "Identifier for email", exact: true }),
    "Not an identifier",
  );
  await page.getByRole("button", { name: "Stage item", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({
      hasText: "EnrichmentClueKind: email must be an email identifier.",
    }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Undo type.Person", exact: true }),
  ).toHaveCount(0);
});

test("a load looks people up before its tables, and the entity shows what it found", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 150, enrichmentStepMs: 1500 });
  await signIn(page);
  await consent(page, true);
  await (await loadDialog(page)).getByRole("button", { name: "Load" }).click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  const loadPage = page.url();
  // Parked while the people are looked up.
  await expect(
    page.getByRole("status").filter({ hasText: /^Looking up people/ }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Stop looking up" }),
  ).toBeVisible();
  await expect(
    page
      .getByRole("grid", { name: "Jobs" })
      .getByText("Looking up people", { exact: true }),
  ).toBeVisible();

  // The Loads list says so,
  await page.goto(`${O}#loads`);
  await expect(
    page
      .getByRole("grid", { name: "Loads" })
      .getByRole("row")
      .filter({ hasText: "Tables and documents: looking up people" }),
  ).toHaveCount(1);

  // and the overview, which links the load.
  await page.goto(O);
  const overview = page
    .getByRole("status")
    .filter({ hasText: "Loading the database: looking up people" });
  await expect(overview).toContainText(
    /(Waiting for a lookup slot \(up to 5 min\)|\d of 9 looked up · \d found)\. Tables load next\./,
  );
  // Its header's badge says so too, not that the parked job is queued.
  await expect(page.locator(".hf-page-header-meta .hf-status")).toHaveText(
    "Looking up people",
  );
  await overview.getByRole("link", { name: "View progress" }).click();
  await expect(page).toHaveURL(loadPage);
  await loaded(page);
  await expect(step(page)).toContainText(
    "People enrichment · 9 looked up · 4 found (4 held) · 160 credits",
  );
  await expect(outcome(page)).toContainText(
    "4 profiles (4 new, 0 found before) · 4 applied.",
  );
  await expect(detailValue(page, "Lookup credits")).toHaveText("160");

  // Records names her profile among what states her, and as the evidence
  // of the employers it links her to.
  const record = await adaRecord(page);
  const from = record.getByRole("region", { name: "Where it comes from" });
  await expect(from).toContainText(
    "People enrichment · profile li:ada-lovelace",
  );
  await expect(from).toContainText("people row c_17");
  await expect(from).not.toContainText("A table row");
  await expect(
    record
      .getByRole("region", { name: "worked_at to" })
      .getByRole("list", { name: "Evidence" })
      .first(),
  ).toContainText("People enrichment · profile li:ada-lovelace");

  // Ada's title came from her profile; her email still from her row.
  await record.getByRole("link", { name: "Open its page" }).click();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "Ada Lovelace",
  );
  // Her header says a profile states her, linking the load.
  await expect(
    page.getByRole("link", {
      name: "Profile from People enrichment (context.dev)",
    }),
  ).toHaveAttribute("href", new URL(loadPage).pathname);
  const chip = page.getByRole("link", {
    name: "current_title from People enrichment (context.dev), profile li:ada-lovelace",
  });
  await expect(chip).toHaveText("People enrichment");
  // It opens the load that looked her up.
  await expect(chip).toHaveAttribute("href", new URL(loadPage).pathname);
  await expect(
    page.getByRole("link", { name: /^email from .* row c_17$/ }),
  ).toBeVisible();
  await page.getByRole("tab", { name: /^Facts/ }).click();
  const fact = page
    .locator(".ex-facts > li")
    .filter({ hasText: "VP Engineering" });
  await expect(fact).toContainText("current");
  await expect(
    fact.getByRole("link", {
      name: "worked_at from People enrichment (context.dev)",
    }),
  ).toBeVisible();
  await expect(
    fact.getByText(
      /^People enrichment · profile li:ada-lovelace · profiles as of \w+ \d+$/,
    ),
  ).toBeVisible();

  // The second load finds everyone carried forward: nobody is looked up.
  await mock.configure(FAST);
  await load(page);
  await expect(step(page)).toContainText(
    "People enrichment · 0 looked up · 0 found · 4 found before · 0 credits",
  );
  await expect(outcome(page)).toContainText(
    "4 profiles (0 new, 4 found before) · 4 applied.",
  );
  await expect(detailValue(page, "Lookup credits")).toHaveText("0");
  // It reused her profile, found by the first load: its own date isn't the
  // profile's.
  await openAda(page);
  await page.getByRole("tab", { name: /^Facts/ }).click();
  await expect(
    page
      .locator(".ex-facts > li")
      .filter({ hasText: "VP Engineering" })
      .getByText(/^People enrichment · profile li:ada-lovelace$/),
  ).toBeVisible();
});

for (const [knob, text, why] of [
  [
    "cap",
    /People enrichment · 2 of 9 looked up · credit cap reached; 7 wait until it resets on \w+ \d+/,
    "7 people wait for the next load: this month’s credit cap was reached.",
  ],
  [
    "provider_credits",
    "People enrichment · context.dev stopped answering after 2; the rest next load",
    "7 people wait for the next load: context.dev stopped answering.",
  ],
  [
    "time",
    "People enrichment · Time limit reached after 2; the rest next load",
    "7 people wait for the next load: the time limit was reached.",
  ],
  [
    "busy",
    "People enrichment · Lookups were busy; profiles found before applied, the rest next load",
    "9 people wait for the next load: lookups were busy.",
  ],
] as const)
  test(`ends partial(${knob}) and the load is still Ready and current`, async ({
    page,
  }) => {
    await mock.configure({ ...FAST, enrichmentOutcome: knob });
    await signIn(page);
    await consent(page, true);
    await load(page);
    await expect(
      header(page).getByText("Ready", { exact: true }),
    ).toBeVisible();
    await expect(
      header(page).getByText("Current", { exact: true }),
    ).toBeVisible();
    await expect(step(page)).toContainText(text);
    await expect(outcome(page)).toContainText(why);
    // The cap's callout links the setting.
    await expect(
      outcome(page).getByRole("link", { name: "Credit cap" }),
    ).toHaveCount(knob === "cap" ? 1 : 0);
  });

test("stops the lookups, and the load goes on with what was found", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 150, enrichmentStepMs: 1500 });
  await signIn(page);
  await consent(page, true);
  await (await loadDialog(page)).getByRole("button", { name: "Load" }).click();
  await page.getByRole("button", { name: "Stop looking up" }).click();
  await expect(page.getByText("Stopping the lookups")).toBeVisible();
  await loaded(page);
  await expect(step(page)).toContainText(
    /People enrichment · Stopped after \d; the rest next load/,
  );
  await expect(outcome(page)).toContainText(
    /people wait for the next load: the lookups were stopped\./,
  );
  await expect(
    header(page).getByText("Current", { exact: true }),
  ).toBeVisible();
});

test("a refused Stop says why, and a member sees the lookups without Stop", async ({
  page,
}) => {
  // A batch of lookups takes long enough to look at the step twice.
  await mock.configure({ loadStepMs: 150, enrichmentStepMs: 30_000 });
  await signIn(page);
  await consent(page, true);
  await (await loadDialog(page)).getByRole("button", { name: "Load" }).click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  const url = page.url(),
    looking = page.getByRole("strong").filter({ hasText: "Looking up people" }),
    stop = page.getByRole("button", { name: "Stop looking up" });
  await expect(looking).toBeVisible();
  const route = "**/papi/**/loads/*:stop-enrichment";
  await page.route(route, (r) =>
    r.fulfill({
      status: 400,
      contentType: "application/json",
      body: JSON.stringify({ code: 9, message: "the lookups ended already" }),
    }),
  );
  await stop.click();
  await expect(page.getByText("The lookups ended already")).toBeVisible();
  await expect(page.getByText("Stopping the lookups")).toHaveCount(0);
  await expect(stop).toBeEnabled();
  await page.unroute(route);
  // A member reads the step, and can't stop it.
  await signOut(page);
  await signIn(page, "alan@example.com");
  await page.goto(url);
  await expect(looking).toBeVisible();
  await expect(stop).toHaveCount(0);
});

test("says what a load applies without consent or credits", async ({
  page,
}) => {
  await mock.configure(FAST);
  await signIn(page);
  const off = await loadDialog(page);
  await expect(off).toContainText(
    "Off for this workspace: this load applies no profiles.",
  );
  await off.getByRole("button", { name: "Cancel" }).click();
  // No credits left: it applies what was found before, and doesn't park.
  // Saved behind the open page, which reads it once reloaded.
  await consent(page, true, 0);
  await page.reload();
  const spent = await loadDialog(page);
  await expect(spent).toContainText(
    /0 credits left this month: applies profiles found before\. The cap resets on \w+ \d+\./,
  );
  await spent.getByRole("button", { name: "Load" }).click();
  await loaded(page);
  await expect(step(page)).toContainText(
    /People enrichment · 0 of 9 looked up · credit cap reached; 9 wait until it resets on/,
  );
});

test("while Helix pauses lookups, the owner can only turn it off, and a load looks no one up", async ({
  page,
}) => {
  await mock.configure(FAST);
  await signIn(page);
  await consent(page, true);
  await mock.configure({ enrichmentFlag: false });
  await page.goto("/settings/enrichment");
  await expect(
    page.getByText(
      "Helix has paused lookups for now. Loads still apply profiles found before.",
    ),
  ).toBeVisible();
  const allow = page.getByRole("switch", { name: "Allow People enrichment" });
  await expect(allow).toBeChecked();
  await expect(allow).toBeEnabled();
  await expect(page.getByLabel("Monthly credit cap")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Save cap" })).toHaveCount(0);

  // The version editor offers no Enrich people, and Types no way to it.
  await page.goto(`${O}/versions/ver_sales_1?tab=edit`);
  await choose(page.getByLabel("Item to edit"), "type.Person");
  await expect(page.getByLabel("Identifier for email")).toBeVisible();
  await expect(
    page.getByRole("region", { name: "People enrichment of Person" }),
  ).toHaveCount(0);
  await page.goto(`${O}/versions/ver_sales_2?tab=types`);
  const types = page.getByRole("tabpanel");
  await expect(types.getByText("Enriched · email, LinkedIn")).toBeVisible();
  await expect(
    types.getByRole("link", { name: "Edit enrichment" }),
  ).toHaveCount(0);

  // A load applies what was found before: nothing yet.
  const dialog = await loadDialog(page);
  await expect(dialog).toContainText(
    "Lookups are paused by Helix; profiles found before still apply.",
  );
  await dialog.getByRole("button", { name: "Load" }).click();
  await loaded(page);
  await expect(step(page)).toContainText(
    "People enrichment · Lookups paused by Helix: profiles found before applied",
  );
  await expect(outcome(page)).toContainText(
    "9 people wait for the next load: Helix paused lookups.",
  );
  await expect(
    header(page).getByText("Current", { exact: true }),
  ).toBeVisible();

  // Turning it off still works.
  await page.goto("/settings/enrichment");
  await allow.click();
  await page
    .getByRole("dialog", { name: "Turn off People enrichment?" })
    .getByRole("button", { name: "Turn off" })
    .click();
  await expect(page.getByText("People enrichment is off")).toBeVisible();
  await expect(allow).not.toBeChecked();
  await expect(allow).toBeDisabled();
});

test("a save the WFE refuses says why, and leaves the setting as it was", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/settings/enrichment");
  const allow = page.getByRole("switch", { name: "Allow People enrichment" }),
    lawful = page.getByRole("checkbox", {
      name: "I confirm we have a lawful basis to do this.",
    }),
    save = page.getByRole("button", { name: "Allow People enrichment" }),
    unavailable = page
      .getByRole("alert")
      .filter({ hasText: "People enrichment isn't available" });
  await allow.check();
  await lawful.check();
  // Helix pauses lookups after the page read the setting.
  await mock.configure({ enrichmentFlag: false });
  await save.click();
  await expect(unavailable).toBeVisible();
  // The form keeps what was picked, to try again.
  await expect(allow).toBeChecked();
  await expect(lawful).toBeChecked();
  await expect(save).toBeEnabled();
  await page.reload();
  await expect(allow).not.toBeChecked();
  await expect(allow).toBeDisabled();

  // A new cap, once it is on.
  await mock.configure({ enrichmentFlag: true });
  await consent(page, true);
  await page.reload();
  const cap = page.getByLabel("Monthly credit cap");
  await cap.fill("20000");
  await mock.configure({ enrichmentFlag: false });
  await page.getByRole("button", { name: "Save cap" }).click();
  await expect(unavailable).toBeVisible();
  await expect(cap).toHaveValue("20000");
  await page.reload();
  await expect(
    page.getByText(
      "Helix has paused lookups for now. Loads still apply profiles found before.",
    ),
  ).toBeVisible();
  await expect(
    page.getByText(/^0 of 50,000 credits used this month/),
  ).toBeVisible();

  // Turning it off, refused: the dialog says why, once.
  const patch = "**/papi/v1/workspaces/ws_acme/foundry/enrichment";
  await page.route(patch, (route) =>
    route.request().method() !== "PATCH"
      ? route.continue()
      : route.fulfill({
          status: 503,
          contentType: "application/json",
          body: JSON.stringify({ code: 14, message: "Settings unavailable" }),
        }),
  );
  await allow.click();
  const dialog = page.getByRole("dialog", {
    name: "Turn off People enrichment?",
  });
  await dialog.getByRole("button", { name: "Turn off" }).click();
  await expect(dialog.getByRole("alert")).toHaveText("Settings unavailable");
  await expect(
    page.getByRole("alert").filter({ hasText: "Settings unavailable" }),
  ).toHaveCount(1);
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByText("Settings unavailable")).toHaveCount(0);
  await expect(allow).toBeChecked();
  await page.unroute(patch);
});

test("a new cap from a page that still shows it on doesn't turn it back on", async ({
  page,
}) => {
  await signIn(page);
  await consent(page, true);
  await page.goto("/settings/enrichment");
  const allow = page.getByRole("switch", { name: "Allow People enrichment" }),
    cap = page.getByLabel("Monthly credit cap");
  await expect(allow).toBeChecked();
  await cap.fill("20000");
  // Another owner turns it off after the page read the setting.
  await consent(page, false);
  await page.getByRole("button", { name: "Save cap" }).click();
  await expect(
    page.getByText(
      "People enrichment was turned off meanwhile, so the cap wasn’t saved.",
    ),
  ).toBeVisible();
  // The page reads it off, with the notice and the lawful basis to allow
  // it again.
  await expect(allow).not.toBeChecked();
  await expect(cap).toHaveCount(0);
  await page.reload();
  await expect(allow).not.toBeChecked();
  await allow.check();
  await expect(
    page.getByRole("checkbox", {
      name: "I confirm we have a lawful basis to do this.",
    }),
  ).not.toBeChecked();
});

test("says when the setting can't be read, and saves no cap without reading it", async ({
  page,
}) => {
  await mock.configure(FAST);
  await signIn(page);
  await consent(page, true);
  const settings = "**/papi/v1/workspaces/ws_acme/foundry/enrichment";
  let patches = 0;
  const unreadable = () =>
    page.route(settings, (route) => {
      if (route.request().method() !== "GET") {
        patches++;
        return route.continue();
      }
      return route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({ code: 14, message: "Settings unavailable" }),
      });
    });
  await unreadable();
  await page.goto("/settings/enrichment");
  await expect(
    page
      .getByRole("alert")
      .filter({ hasText: "Couldn’t read the setting. Settings unavailable" }),
  ).toBeVisible();
  // The load dialog says so too.
  const dialog = await loadDialog(page);
  await expect(dialog).toContainText(
    "Couldn’t read the workspace’s setting. Settings unavailable",
  );
  await dialog.getByRole("button", { name: "Cancel" }).click();
  // Retry reads it again.
  await page.goto("/settings/enrichment");
  const retry = page.getByRole("button", { name: "Retry" });
  await expect(retry).toBeVisible();
  await page.unroute(settings);
  await retry.click();
  const cap = page.getByLabel("Monthly credit cap"),
    save = page.getByRole("button", { name: "Save cap" });
  await expect(cap).toBeVisible();
  await expect(retry).toHaveCount(0);
  // A new cap reads the setting first: unreadable, nothing is sent.
  await unreadable();
  await cap.fill("20000");
  await save.click();
  await expect(
    page.getByRole("alert").filter({ hasText: "Settings unavailable" }),
  ).toBeVisible();
  expect(patches).toBe(0);
  await expect(cap).toHaveValue("20000");
  await page.unroute(settings);
  await save.click();
  await expect(
    page.getByText("The cap is 20,000 credits a month"),
  ).toBeVisible();
});

test("turning consent off removes the enriched values as the ontology reloads", async ({
  page,
}) => {
  await mock.configure(FAST);
  await signIn(page);
  await consent(page, true);
  await load(page);
  await openAda(page);
  await expect(
    page.getByRole("link", { name: /^current_title from People enrichment/ }),
  ).toBeVisible();

  // The clean load runs slowly enough to watch.
  await mock.configure({ loadStepMs: 1500 });
  await page.goto("/settings/enrichment");
  // The switch stays on until the owner confirms.
  await page.getByRole("switch", { name: "Allow People enrichment" }).click();
  const confirm = page.getByRole("dialog", {
    name: "Turn off People enrichment?",
  });
  await expect(confirm).toContainText(
    "Lookups stop and stored profiles are deleted. Enriched values are removed as each ontology reloads; Helix starts those loads.",
  );
  await confirm.getByRole("button", { name: "Turn off" }).click();
  await expect(page.getByText("People enrichment is off")).toBeVisible();
  const purging = page
    .getByRole("status")
    .filter({ hasText: "Removing enriched values: 1 ontology left" });
  await expect(purging).toBeVisible();
  await expect(purging).toHaveCount(0, { timeout: 30_000 });

  // The clean load is current and applied no profiles.
  await page.goto(`${O}#loads`);
  await page
    .getByRole("grid", { name: "Loads" })
    .getByRole("row")
    .filter({ has: page.getByText("Current", { exact: true }) })
    .getByRole("link", { name: "v2" })
    .click();
  await expect(step(page)).toContainText(
    "People enrichment · Off for this workspace: no profiles applied",
  );
  await openAda(page);
  await expect(
    page.getByRole("link", { name: /from People enrichment/ }),
  ).toHaveCount(0);
});
