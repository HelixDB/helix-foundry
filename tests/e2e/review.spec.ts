import type { Page } from "@playwright/test";
import {
  expect,
  file,
  mock,
  signIn,
  test,
  uploadAndLoadContract,
} from "./support/mock";

// The Review page (/ontology/:id/review): one sub-link of the open
// ontology, two tabs, each counting what is open in it. Facts (the
// default) is the review walkthrough, Ontology the suggestions; the tab is
// in the URL, and the old Suggestions page's address opens the Ontology tab.

const O = "/ontology/ont_acme_contracts";
// Held claims, a value in conflict and a name in no row, beside the
// contract's facts (review-queue.spec.ts).
const HEDGED =
  "# Hedged deal\n\nUmbrella Holdings was copied. Globex Corporation and Initech may have signed. Globex Corporation supplies Initech. Initech competes with Globex Corporation.\n\n## Billing\n\nGlobex Corporation is billed in GBP.\n";

const tab = (page: Page, name: "Facts" | "Ontology") =>
    page.getByRole("tab", { name: new RegExp(`^${name}`) }),
  countOf = (page: Page, name: "Facts" | "Ontology") =>
    tab(page, name).locator(".hf-tab-count"),
  walkCard = (page: Page) => page.getByRole("article"),
  suggestionCards = (page: Page) =>
    page.getByRole("list", { name: "Suggestions" }).locator(":scope > li"),
  reviewLink = (page: Page) =>
    page
      .getByRole("list", { name: "Pages" })
      .getByRole("link", { name: /^Review/ });

test("switches between Facts and Ontology, each with its open count, the tab in the URL", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200, documentUnmatched: true });
  await signIn(page);
  await uploadAndLoadContract(page, [file("variant.md", HEDGED)]);
  // The load just made is audited a moment after it is current: Review
  // opens again until the Ontology tab counts its suggestions.
  await expect(async () => {
    await page.goto(O);
    await reviewLink(page).click();
    await expect(countOf(page, "Ontology")).toHaveText("4", { timeout: 2000 });
  }).toPass({ timeout: 45_000 });
  await expect(page).toHaveURL(new RegExp(`${O}/review$`));
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Review");
  await expect(page.getByRole("tab")).toHaveText([/^Facts/, /^Ontology/]);
  await expect(tab(page, "Facts")).toHaveAttribute("aria-selected", "true");
  await expect(
    page.getByText(/^What the current load couldn’t settle on its own/),
  ).toBeVisible();

  // Facts counts the queue's open cards, and one fewer once one is decided.
  await expect(walkCard(page)).toBeVisible();
  await expect(countOf(page, "Facts")).toHaveText(/^\d+$/);
  const cards = Number(await countOf(page, "Facts").textContent());
  // The panel's link counts what both tabs hold.
  await expect(reviewLink(page)).toHaveAccessibleName(
    `Review, ${cards + 4} open`,
  );
  await walkCard(page)
    .getByRole("button", { name: "Approve", exact: true })
    .click();
  await expect(countOf(page, "Facts")).toHaveText(String(cards - 1));
  await expect(reviewLink(page)).toHaveAccessibleName(
    `Review, ${cards + 3} open`,
  );

  // Ontology: its own line under the heading, its suggestions, in the URL.
  await tab(page, "Ontology").click();
  await expect(page).toHaveURL(new RegExp(`${O}/review\\?tab=ontology$`));
  await expect(tab(page, "Ontology")).toHaveAttribute("aria-selected", "true");
  await expect(
    page.getByText(/^What audits of the current load found/),
  ).toBeVisible();
  await expect(suggestionCards(page)).toHaveCount(4);
  await expect(walkCard(page)).toHaveCount(0);
  // Facts keeps its count from the other tab.
  await expect(countOf(page, "Facts")).toHaveText(String(cards - 1));
  // A refresh keeps the tab.
  await page.reload();
  await expect(tab(page, "Ontology")).toHaveAttribute("aria-selected", "true");
  await expect(suggestionCards(page)).toHaveCount(4);
  // The keyboard moves between tabs; Facts writes no tab.
  await tab(page, "Ontology").focus();
  await page.keyboard.press("ArrowLeft");
  await expect(tab(page, "Facts")).toHaveAttribute("aria-selected", "true");
  await expect(page).toHaveURL(new RegExp(`${O}/review$`));
  await expect(walkCard(page)).toBeVisible();
});

test("opens the Ontology tab from the old Suggestions address, keeping its query", async ({
  page,
}) => {
  await signIn(page);
  await page.goto(`${O}/suggestions?from=email`);
  await expect(page).toHaveURL(
    new RegExp(`${O}/review\\?from=email&tab=ontology$`),
  );
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Review");
  await expect(tab(page, "Ontology")).toHaveAttribute("aria-selected", "true");
  // The panel's one link is current, on either tab.
  await expect(reviewLink(page)).toHaveAttribute("aria-current", "page");
  await expect(
    page.getByRole("list", { name: "Pages" }).getByRole("link", {
      name: /Suggestions|Review queue/,
    }),
  ).toHaveCount(0);
});
