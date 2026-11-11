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
  await expect(dialog).toContainText(
    "This load writes each one it can, at once. Any it can't stays held, and its card says why.",
  );
  await dialog.getByRole("button", { name: "Approve all" }).click();
  await expect(
    head.getByRole("status").filter({ hasText: "couldn’t be" }),
  ).toHaveText("2 approved, 1 couldn’t be: Settlement unavailable");
  expect(sent.sort()).toEqual(
    ["Initech LLC", "Elstree Media Group", "Lockhart Shipping plc"]
      .map(hashOf)
      .sort(),
  );
  // The load writes the three approved, with no new load: their cards go.
  // The card that failed is as it was, with its own Approve; one claim
  // isn't a group's worth.
  await expect(rows).toHaveCount(1);
  await expect(
    rows.getByRole("button", { name: "Approve", exact: true }),
  ).toHaveCount(1);
  await expect(approveAll).toHaveCount(0);
  await page.unroute("**/papi/**/reviews/*:settle");
  const saved = page
    .getByRole("list", { name: "Saved decisions" })
    .getByRole("listitem");
  await expect(saved).toHaveCount(3);
  await expect(saved.filter({ hasText: /^Approved: “/ })).toHaveCount(3);
  await page.getByRole("tab", { name: /^Facts/ }).click();
  const facts = page.getByRole("list", { name: "Facts" });
  await expect(facts).toContainText("signed_with");
  await expect(facts).toContainText("Globex Corporation");
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  // A later load writes what was approved and holds the rest.
  await loadVersion3(page);
  await page.goto(`${doc}?tab=review`);
  await expect(rows).toHaveCount(1);
  await expect(saved).toHaveCount(3);
});

test("leaves claims held for a name out of Approve all, on the page of their other end too", async ({
  page,
}) => {
  // Each customer is linked by part of its name: that link's review
  // concerns the customer alone, so the contract's page reads it apart.
  await mock.configure({ documentHeld: "part_name" });
  const doc = await reviewed(
    page,
    "# Hedged deal\n\nGlobex Corporation and Initech may have signed.\n",
  );
  const { head, rows } = group(page, HELD);
  await expect(rows).toHaveCount(2);
  await expect(head.getByRole("button", { name: /^Approve all/ })).toHaveCount(
    0,
  );
  await page.goto(entity("Contract", "n:hedged deal"));
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(rows).toHaveCount(2);
  const globex = rows.filter({ hasText: "Globex Corporation (Customer)" });
  await expect(globex).toContainText(
    "“Globex Corporation” is linked only by part of a row’s name. Settle that name too, or this stays held.",
  );
  await expect(globex).toContainText(
    "the first load after that name is settled writes it",
  );
  await expect(globex).not.toContainText("written to this load at once");
  await expect(head.getByRole("button", { name: /^Approve all/ })).toHaveCount(
    0,
  );
  // Its way to the name's review leads to the document, at the name.
  await globex.getByRole("link", { name: "Review that name" }).click();
  await expect(page).toHaveURL(new RegExp(`${doc}\\?tab=review.*#s\\d+-\\d+$`));
  await expect(
    reviews(page).getByRole("list", { name: "Linked by part of a name" }),
  ).toContainText("Linked to:");
});

test("keeps an entity's page up when the names holding its claims can't be read, and lists the claims once they are", async ({
  page,
}) => {
  await mock.configure({ documentHeld: "part_name" });
  await reviewed(
    page,
    "# Hedged deal\n\nGlobex Corporation and Initech may have signed.\n",
  );
  // Only the read of the names' reviews fails.
  const names = "**/databases:query-read";
  await page.route(names, async (route) => {
    const { queryJson } = route.request().postDataJSON() as {
      queryJson?: string;
    };
    if (queryJson?.includes("foundry_name_links"))
      return route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({ code: 14, message: "unavailable" }),
      });
    await route.continue();
  });
  await page.goto(entity("Contract", "n:hedged deal"));
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(page.getByRole("alert")).toContainText(
    "Couldn’t read which names hold its held claims",
  );
  // No held claim is offered while what holds it isn't known.
  await expect(group(page, HELD).rows).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Approve/ })).toHaveCount(0);
  // The rest of the page stands: the entity, and where documents name it.
  await expect(page.getByRole("tab", { name: /^Mentions/ })).toBeVisible();
  await expect(page.getByText("Couldn’t read this entity")).toHaveCount(0);
  await page.unroute(names);
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await page.getByRole("button", { name: "Retry" }).click();
  const { head, rows } = group(page, HELD);
  await expect(rows).toHaveCount(2);
  await expect(
    rows.filter({ hasText: "Globex Corporation (Customer)" }),
  ).toContainText("Settle that name too, or this stays held.");
  await expect(head.getByRole("button", { name: /^Approve all/ })).toHaveCount(
    0,
  );
});

test("rejects a held claim: its card and its count go, Approve all leaves it out, and removing the decision lists it again", async ({
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
  const tab = page.getByRole("tab", { name: /^Needs review/ });
  const listed = async () =>
    Number((await tab.locator(".hf-tab-count").innerText()).replace(/\D/g, ""));
  const before = await listed();
  // From here the page is never loaded again until it says so.
  let loads = 0;
  page.on("load", () => (loads += 1));

  // Rejected, bound to what its card shows. Nothing is written, so no job
  // runs: the card goes at once, and every count with it.
  const globex = rows.filter({ hasText: "Globex Corporation (Customer)" });
  await expect(globex).toContainText(
    "Reject it if it’s wrong: nothing is written, and it leaves Needs review in this load and later ones.",
  );
  const sent = page.waitForRequest(
    (r) => r.method() === "POST" && /\/reviews\/[^/]+:settle$/.test(r.url()),
  );
  await globex.getByRole("button", { name: "Reject", exact: true }).click();
  expect((await sent).postDataJSON()).toEqual({
    decision: { reject: { statementHash: hashOf("Globex Corporation") } },
  });
  await expect(rows).toHaveCount(3);
  await expect(globex).toHaveCount(0);
  await expect(head.getByRole("heading")).toHaveText(`${HELD} 3`);
  expect(await listed()).toBe(before - 1);
  await expect(approveAll).toHaveText("Approve all (3)");
  const saved = page
    .getByRole("list", { name: "Saved decisions" })
    .getByRole("listitem");
  await expect(saved.filter({ hasText: /^Rejected: “/ })).toHaveCount(1);
  await expect(page.getByText("Writing it to this load")).toHaveCount(0);

  // Approve all approves the three left, and never the one rejected.
  const approved: string[] = [];
  await page.route("**/papi/**/reviews/*:settle", async (route) => {
    approved.push(
      route.request().postDataJSON().decision.release.statementHash,
    );
    await route.continue();
  });
  await approveAll.click();
  await page
    .getByRole("dialog", { name: "Approve 3 claims?" })
    .getByRole("button", { name: "Approve all" })
    .click();
  await expect(rows).toHaveCount(0);
  expect(approved.sort()).toEqual(
    ["Initech LLC", "Elstree Media Group", "Lockhart Shipping plc"]
      .map(hashOf)
      .sort(),
  );
  await page.unroute("**/papi/**/reviews/*:settle");
  expect(await listed()).toBe(before - 4);
  await expect(saved.filter({ hasText: /^Approved: “/ })).toHaveCount(3);
  await expect(saved.filter({ hasText: /^Rejected: “/ })).toHaveCount(1);
  // The rejected claim was written nowhere.
  await page.getByRole("tab", { name: /^Facts/ }).click();
  const facts = page.getByRole("list", { name: "Facts" });
  await expect(facts).toContainText(/signed_with\s*Initech LLC/);
  await expect(facts).not.toContainText(/signed_with\s*Globex Corporation/);
  expect(loads).toBe(0);

  // A later load holds the claim again, and its card stays out.
  await loadVersion3(page);
  await page.goto(`${doc}?tab=review`);
  await expect(saved.filter({ hasText: /^Rejected: “/ })).toHaveCount(1);
  await expect(rows).toHaveCount(0);
  await expect(
    reviews(page).getByText("Globex Corporation (Customer)"),
  ).toHaveCount(0);
  const hidden = await listed();

  // The decision removed, the claim is listed again, to approve or reject.
  await saved
    .filter({ hasText: /^Rejected: “/ })
    .getByRole("button", { name: "Remove decision" })
    .click();
  await expect(
    page.getByText("Decision removed. The claim is listed for review again."),
  ).toBeVisible();
  await expect(rows).toHaveCount(1);
  await expect(globex).toContainText("Approving writes:");
  await expect(
    globex.getByRole("button", { name: "Approve", exact: true }),
  ).toBeVisible();
  await expect(
    globex.getByRole("button", { name: "Reject", exact: true }),
  ).toBeVisible();
  expect(await listed()).toBe(hidden + 1);

  // A member reads the card and can't reject it.
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto(`${doc}?tab=review`);
  await expect(globex).toContainText("Approving writes:");
  await expect(
    globex.getByRole("button", { name: "Reject", exact: true }),
  ).toHaveCount(0);
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
