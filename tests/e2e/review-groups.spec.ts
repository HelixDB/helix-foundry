import { createHash } from "node:crypto";
import type { Page } from "@playwright/test";
import {
  expect,
  file,
  loadVersion3,
  makeContractsCurrent,
  mock,
  signIn,
  test,
  uploadInData,
} from "./support/mock";

const O = "/ontology/ont_acme_contracts",
  X = "/explore/ont_acme_contracts";
const entity = (type: string, identity: string) =>
  `${X}/entities/${Buffer.from(`${type}\0${identity}`).toString("base64url")}`;
/** Uploads a document, loads the contracts draft, and opens the document's Needs review. */
async function reviewed(page: Page, text: string) {
  await mock.configure({ loadStepMs: 200, documentUnmatched: true });
  await signIn(page);
  await makeContractsCurrent(page);
  await uploadInData(page, [file("variant.md", text)]);
  await loadVersion3(page);
  await page.goto(`${O}/documents`);
  await page
    .getByRole("grid", { name: "Documents" })
    .getByRole("link", { name: "variant.md", exact: true })
    .click();
  const doc = new URL(page.url()).pathname;
  await page.goto(`${doc}?tab=review`);
  return doc;
}
const reviews = (page: Page) =>
  page.getByRole("list", { name: "Needs review" });
/** A reason's group in Needs review: its head, and its rows. */
const group = (page: Page, title: string) => ({
  head: reviews(page)
    .getByRole("listitem")
    .filter({ has: page.getByRole("heading", { name: title }) }),
  // Its own rows, not the lists inside a row.
  rows: reviews(page).getByRole("list", { name: title }).locator(":scope > li"),
});
const HELD = "The model wasn’t sure";

test("groups a document's reviews by reason, what a person can act on first", async ({
  page,
}) => {
  await reviewed(
    page,
    // The mock reads a customer's currency from a section naming it, so
    // billing has a section of its own.
    "# Hedged deal\n\nUmbrella Holdings was copied. Globex Corporation and Initech may have signed. Globex Corporation supplies Initech. Initech competes with Globex Corporation.\n\n## Billing\n\nGlobex Corporation is billed in GBP.\n",
  );
  // Held claims, then a value in conflict, then links the ontology lacks,
  // then the rest; each says its reason once, with its count.
  await expect(reviews(page).getByRole("heading", { level: 3 })).toHaveText([
    `${HELD} 2`,
    "Disagrees on a value 1",
    "A relationship the ontology doesn’t have 2",
    "Names something that isn’t in your data 1",
  ]);
  await expect(
    reviews(page).getByText(
      "The fact was read with low confidence, so it wasn’t stored.",
    ),
  ).toHaveCount(1);
  // A row is the review alone: its quote, what it's about, its controls.
  const held = group(page, HELD).rows;
  await expect(held).toHaveCount(2);
  await expect(held.first()).toContainText(
    "Globex Corporation and Initech may have signed.",
  );
  await expect(held.first()).toContainText("suggested link: signed_with");
  await expect(held.first()).toContainText("Approving writes:");
  await expect(
    held.first().getByRole("button", { name: "Approve", exact: true }),
  ).toBeVisible();
  await expect(group(page, "Disagrees on a value").rows).toContainText(
    "currency",
  );
  await expect(
    group(page, "A relationship the ontology doesn’t have").rows,
  ).toHaveText([
    /Globex Corporation supplies Initech\..*suggested link: supplier_of/,
    /Initech competes with Globex Corporation\..*suggested link: competitor of/,
  ]);
  // A review marked in the text opens its row, wherever its group is.
  const unresolved = group(page, "Names something that isn’t in your data");
  await page.getByRole("tab", { name: /^Facts/ }).click();
  await page
    .getByLabel("Document text")
    .getByTitle("See why it needs review")
    .filter({ hasText: "Umbrella Holdings" })
    .click();
  await expect(
    page.getByRole("tab", { name: /^Needs review/ }),
  ).toHaveAttribute("aria-selected", "true");
  await expect(unresolved.rows).toHaveClass(/is-active/);
  await expect(held.first()).not.toHaveClass(/is-active/);
  // A row's quote shows its span in the text.
  await unresolved.rows
    .getByRole("button", { name: "“Umbrella Holdings was copied.”" })
    .click();
  await expect(
    page.getByLabel("Document text").locator("mark.is-active").first(),
  ).toContainText("Umbrella Holdings");

  // An entity's page groups its reviews the same way.
  await page.goto(entity("Customer", "k:cus_GLOBEX01"));
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  const headings = reviews(page).getByRole("heading", { level: 3 });
  await expect(headings.first()).toHaveText(`${HELD} 1`);
  await expect(headings.nth(1)).toHaveText("Disagrees on a value 1");
  await expect(group(page, HELD).rows).toContainText(
    "Globex Corporation and Initech may have signed.",
  );
  await expect(group(page, HELD).rows).toContainText("Open in variant.md");
});

/** Each claim's hash as the engine reads it: its link and statement. */
const hashOf = (name: string) =>
  createHash("sha256")
    .update(
      `signed_with\u001f${JSON.stringify({
        subject: { type: "Contract", name: "Hedged deal" },
        object: { type: "Customer", name },
        props: {},
      })}`,
    )
    .digest("hex");

test("approves every held claim of a group at once, and says which couldn't be", async ({
  page,
}) => {
  const doc = await reviewed(
    page,
    "# Hedged deal\n\nGlobex Corporation, Initech, Elstree Media Group and Lockhart Shipping plc may have signed.\n",
  );
  const { head, rows } = group(page, HELD);
  await expect(rows).toHaveCount(4);
  const approveAll = head.getByRole("button", { name: /^Approve all/ });
  await expect(approveAll).toHaveText("Approve all (4)");
  // A claim approved on its card isn't counted again.
  await rows
    .filter({ hasText: "Globex Corporation (Customer)" })
    .getByRole("button", { name: "Approve", exact: true })
    .click();
  await expect(approveAll).toHaveText("Approve all (3)");
  // One of the three fails; the others are saved, each bound to what its
  // own card shows.
  const sent: string[] = [];
  let calls = 0;
  await page.route("**/papi/**/reviews/*:settle", async (route) => {
    sent.push(route.request().postDataJSON().decision.release.statementHash);
    if (calls++ === 1)
      return route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({ code: 14, message: "Settlement unavailable" }),
      });
    await route.continue();
  });
  await approveAll.click();
  const dialog = page.getByRole("dialog", { name: "Approve 3 claims?" });
  await expect(dialog).toContainText("The next load writes each one it can. Any it can't stays held, and its card says why.");
  await dialog.getByRole("button", { name: "Approve all" }).click();
  await expect(
    head.getByRole("status").filter({ hasText: "couldn’t be" }),
  ).toHaveText("2 approved, 1 couldn’t be: Settlement unavailable");
  expect(sent.sort()).toEqual(
    ["Initech LLC", "Elstree Media Group", "Lockhart Shipping plc"]
      .map(hashOf)
      .sort(),
  );
  // The card that failed is as it was, with its own Approve; one claim
  // isn't a group's worth.
  const open = rows.filter({
    has: page.getByRole("button", { name: "Approve", exact: true }),
  });
  await expect(open).toHaveCount(1);
  await expect(
    rows.filter({ hasText: "Decision saved. Reload to apply." }),
  ).toHaveCount(3);
  await expect(approveAll).toHaveCount(0);
  await page.unroute("**/papi/**/reviews/*:settle");
  // The next load writes what was approved and holds the rest.
  await loadVersion3(page);
  await page.goto(`${doc}?tab=review`);
  await expect(rows).toHaveCount(1);
  await expect(
    page.getByRole("list", { name: "Saved decisions" }).getByRole("listitem"),
  ).toHaveCount(3);
});

test("offers Approve all and Add these links to managers only", async ({
  page,
}) => {
  const doc = await reviewed(
    page,
    "# Hedged deal\n\nGlobex Corporation and Initech may have signed. Globex Corporation supplies Initech.\n",
  );
  await expect(
    page.getByRole("button", { name: /^Approve all/ }),
  ).toBeVisible();
  await expect(
    page.getByRole("link", { name: "Add these links" }),
  ).toBeVisible();
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto(`${doc}?tab=review`);
  await expect(group(page, HELD).rows).toHaveCount(2);
  await expect(page.getByRole("button", { name: /^Approve all/ })).toHaveCount(
    0,
  );
  await expect(page.getByRole("link", { name: "Add these links" })).toHaveCount(
    0,
  );
});

test("drafts the links the documents state that the ontology lacks", async ({
  page,
}) => {
  await reviewed(
    page,
    "# Supply deal\n\nGlobex Corporation supplies Initech. Initech LLC supplies Globex Corporation again. Initech competes with Globex Corporation. Globex Corporation supplies Initech LLC a third time.\n",
  );
  const { head } = group(page, "A relationship the ontology doesn’t have");
  await expect(head).toContainText(
    "Generating costs a model run and drafts a new version for you to review.",
  );
  await head.getByRole("link", { name: "Add these links" }).click();
  await expect(page).toHaveURL(/\/generate\?goal=/);
  // Each link once, the most suggested first, with two of its quotes.
  await expect(page.getByLabel("Goal")).toHaveValue(
    "Add links the documents state that the ontology lacks: supplier_of (“Globex Corporation supplies Initech.”, “Initech LLC supplies Globex Corporation again.”), competitor of (“Initech competes with Globex Corporation.”).",
  );
});
