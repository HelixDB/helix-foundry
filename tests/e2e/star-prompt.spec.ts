import type { Page } from "@playwright/test";
import {
  expect,
  mock,
  signIn,
  test,
  uploadAndLoadContract,
} from "./support/mock";

// The GitHub star prompt (src/web/shell/star-prompt.tsx), on the hosted
// site: from the third question asked in Analyst, once the page has gone
// 30 s without input, a card asks for a star; Star on GitHub opens the
// repository, and once it is clicked or dismissed nothing asks again.

const REPOSITORY = "https://github.com/helixdb/helix-foundry";

async function ask(page: Page, question: string) {
  await page.getByRole("link", { name: "Analyst", exact: true }).click();
  const form = page.getByRole("form", { name: "Ask a question" });
  await form.getByRole("textbox", { name: /^Question/ }).fill(question);
  await form.getByRole("button", { name: "Ask", exact: true }).click();
  await expect(
    page.getByRole("article", { name: `Question: ${question}` }),
  ).toBeVisible();
}

const prompt = (page: Page) =>
  page.getByRole("complementary", { name: "Star Helix Foundry" });
const done = (page: Page) =>
  page.evaluate(() => localStorage.getItem("foundry.star-prompt.done"));

test.beforeEach(async ({ page, context }) => {
  // Its 30 s wait, without waiting it.
  await page.clock.install();
  // GitHub, which Star on GitHub opens.
  await context.route(`${REPOSITORY}**`, (route) =>
    route.fulfill({ contentType: "text/html", body: "<title>GitHub</title>" }),
  );
  await mock.configure({ loadStepMs: 200, askStepMs: 100 });
  await signIn(page);
  await uploadAndLoadContract(page);
});

test("asks for a star from the third question, once idle, and opens GitHub", async ({
  page,
  context,
}) => {
  await ask(page, "Who signed the services agreement?");
  await ask(page, "When were the invoices issued?");
  await page.clock.fastForward(60_000);
  await expect(prompt(page)).toBeHidden();

  await ask(page, "How are the invoices doing?");
  // Not while the page is in use: only after 30 s without input.
  await page.clock.fastForward(20_000);
  await expect(prompt(page)).toBeHidden();
  await page.clock.fastForward(11_000);
  await expect(prompt(page)).toBeVisible();
  await expect(prompt(page)).toContainText(
    "Enjoying Helix Foundry?Give us a star on GitHub to let us know!",
  );

  // The hosted site has no GitHub token: the button opens the repository.
  const star = prompt(page).getByRole("link", { name: "Star on GitHub" });
  await expect(star).toHaveAttribute("href", REPOSITORY);
  await expect(star).toHaveAttribute("target", "_blank");
  await expect(star).toHaveAttribute("rel", "noopener noreferrer");
  const opened = context.waitForEvent("page");
  await star.click();
  await expect(await opened).toHaveURL(REPOSITORY);
  await expect(prompt(page)).toBeHidden();
  expect(await done(page)).toBe("1");

  // Never again, in a new session either.
  await page.reload();
  await ask(page, "Who are the customers?");
  await page.clock.fastForward(31_000);
  await expect(prompt(page)).toBeHidden();
});

test("never asks again once dismissed", async ({ page, context }) => {
  await page.evaluate(() =>
    localStorage.setItem("foundry.star-prompt.asked", "2"),
  );
  await ask(page, "Who signed the services agreement?");
  await page.clock.fastForward(31_000);
  await expect(prompt(page)).toBeVisible();
  let opened = false;
  context.on("page", () => (opened = true));
  await prompt(page).getByRole("button", { name: "Dismiss" }).click();
  await expect(prompt(page)).toBeHidden();
  expect(await done(page)).toBe("1");
  expect(opened).toBe(false);

  await page.reload();
  await ask(page, "When were the invoices issued?");
  await page.clock.fastForward(31_000);
  await expect(prompt(page)).toBeHidden();
});
