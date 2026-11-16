import type { Page } from "@playwright/test";
import { expect, mock, signIn, test } from "./support/mock";

// Founder's new chat's suggestions (founder-suggestions.tsx): under the
// composer, a tab per kind (Questions, Dashboards, Ontologies, Workflows)
// and the chosen kind's 2 cards, written for the page's ontology. A card
// previews its prompt as the composer's placeholder while hovered, fills
// the composer when clicked (caret at the end, nothing sent), and the kind
// chosen is kept per browser.

const KINDS = ["Questions", "Dashboards", "Ontologies", "Workflows"];
const PLACEHOLDER = "Ask about Renewals, or ask Founder to do something…";
const kindsOf = (page: Page) =>
  page.getByRole("tablist", { name: "Suggestions" });
const boxOf = (page: Page) =>
  page
    .getByRole("main")
    .getByRole("form", { name: "Message Founder" })
    .getByRole("textbox", { name: "Message" });

test("a new chat suggests 2 cards of each kind, which preview and fill the composer, and keeps the kind chosen", async ({
  page,
}) => {
  await mock.configure({ automationSuggestionsMs: 0 });
  await signIn(page);
  await page.goto("/ask/ont_acme_renewals?new=1");
  const kinds = kindsOf(page),
    tab = (name: string) => kinds.getByRole("tab", { name, exact: true });
  await expect(kinds.getByRole("tab")).toHaveText(KINDS);
  await expect(tab("Questions")).toHaveAttribute("aria-selected", "true");
  // Questions are the version's competency questions.
  await expect(
    page
      .getByRole("tabpanel", { name: "Questions" })
      .locator(".fd-card-prompt"),
  ).toHaveText([
    "Which customers are under contract, and which contracts are they signed with?",
    "What is each customer under contract billed?",
  ]);
  for (const kind of KINDS) {
    await tab(kind).click();
    const panel = page.getByRole("tabpanel", { name: kind });
    await expect(panel.getByRole("button")).toHaveCount(2);
    await expect(panel.locator(".fd-card-ontology")).toHaveText([
      "Renewals",
      "Renewals",
    ]);
  }
  // The ontology's changes start from what it doesn't read yet: Renewals
  // has no load to audit, and doesn't read the Shop database.
  await tab("Ontologies").click();
  await expect(
    page
      .getByRole("tabpanel", { name: "Ontologies" })
      .locator(".fd-card-prompt")
      .first(),
  ).toHaveText(
    "Draft a new version of Renewals that also reads Shop database.",
  );

  // The arrow keys, Home and End move between the kinds.
  await tab("Questions").click();
  await page.keyboard.press("ArrowRight");
  await expect(tab("Dashboards")).toBeFocused();
  await expect(tab("Dashboards")).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("End");
  await expect(tab("Workflows")).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("Home");
  await expect(tab("Questions")).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("ArrowLeft");
  await expect(tab("Workflows")).toHaveAttribute("aria-selected", "true");
  await expect(tab("Workflows")).toBeFocused();

  // Hovered, a card's prompt is the empty composer's placeholder.
  await tab("Dashboards").click();
  const card = page
    .getByRole("tabpanel", { name: "Dashboards" })
    .getByRole("button")
    .first();
  const prompt = (await card.locator(".fd-card-prompt").textContent())!;
  expect(prompt).toMatch(/\bdashboard\b/);
  const box = boxOf(page);
  await card.hover();
  await expect(box).toHaveAttribute("placeholder", prompt);
  await page.mouse.move(1, 1);
  await expect(box).toHaveAttribute("placeholder", PLACEHOLDER);

  // Clicked, it fills the composer, caret at the end, and sends nothing.
  await card.click();
  await expect(box).toHaveValue(prompt);
  await expect(box).toBeFocused();
  expect(
    await box.evaluate((el) => {
      const t = el as HTMLTextAreaElement;
      return [t.selectionStart, t.selectionEnd];
    }),
  ).toEqual([prompt.length, prompt.length]);
  await expect(page).toHaveURL(/\/ask\/ont_acme_renewals\?new=1$/);
  await expect(
    page.getByRole("main").getByRole("log", { name: "Conversation" }),
  ).toHaveCount(0);

  // The kind chosen survives a reload.
  await page.reload();
  await expect(tab("Dashboards")).toHaveAttribute("aria-selected", "true");
  await expect(
    page.getByRole("tabpanel", { name: "Dashboards" }).getByRole("button"),
  ).toHaveCount(2);
});

test("without workflows, a new chat offers no Workflows, and a stored Workflows falls back to Questions", async ({
  page,
}) => {
  await mock.configure({
    automationSuggestionsMs: 0,
    automationsDisabled: true,
  });
  await signIn(page);
  await page.addInitScript(() =>
    localStorage.setItem("foundry.agent.suggestionKind", "workflow"),
  );
  await page.goto("/ask/ont_acme_renewals?new=1");
  const kinds = kindsOf(page);
  await expect(kinds.getByRole("tab")).toHaveText(KINDS.slice(0, 3));
  await expect(
    kinds.getByRole("tab", { name: "Questions", exact: true }),
  ).toHaveAttribute("aria-selected", "true");
});

test("on a phone, the kinds stay in their row and the cards stack, with no page scroll sideways", async ({
  page,
}) => {
  await page.setViewportSize({ width: 375, height: 800 });
  await mock.configure({ automationSuggestionsMs: 0 });
  await signIn(page);
  await page.goto("/ask/ont_acme_renewals?new=1");
  const panel = page.getByRole("tabpanel", { name: "Questions" });
  const cards = panel.getByRole("button");
  await expect(cards).toHaveCount(2);
  const [a, b] = [
    (await cards.nth(0).boundingBox())!,
    (await cards.nth(1).boundingBox())!,
  ];
  expect(b.y).toBeGreaterThan(a.y + a.height - 1);
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
});
