import type { Page } from "@playwright/test";
import { expect, mock, test } from "../e2e/support/mock";
import {
  CONTRACTS,
  answered,
  ask,
  caption,
  click,
  loadContract,
  poster,
  saveVideo,
  shot,
  type,
  videoPage,
} from "./support";

// docs: foundry/ask (a new thread with its suggestions, and a thread: the
// question, its steps, the answer with its sources, and the threads in the
// sidebar).

const QUESTION = "Who signed the services agreement?";

const newThread = (page: Page) =>
  expect(
    page.getByRole("heading", {
      level: 1,
      name: "What do you want to know about Customer contracts?",
    }),
  ).toBeVisible();

test("ask: a new thread and a thread", async ({ page }) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 250 });
  await loadContract(page);
  // The load's version, read once, names the suggestions.
  await page.goto(`${CONTRACTS}/versions/ver_seed_3`);
  await expect(
    page.getByRole("heading", { level: 1, name: "Version 3" }),
  ).toBeVisible();
  await page
    .getByRole("navigation", { name: "Main navigation" })
    .getByRole("link", { name: "Analyst" })
    .click();
  await newThread(page);
  const suggestions = page.getByRole("group", { name: "Suggested questions" });
  await expect(suggestions.getByRole("button").first()).toBeVisible();
  await shot(page, "ask", "new-thread");

  // A first thread from a suggestion, then a second one.
  const suggested = (await suggestions
    .getByRole("button")
    .first()
    .textContent())!;
  await suggestions.getByRole("button").first().click();
  await page
    .getByRole("form", { name: "Ask a question" })
    .getByRole("button", { name: "Ask", exact: true })
    .click();
  await answered(page, suggested);
  await page.getByRole("link", { name: "New thread" }).click();
  await newThread(page);
  await ask(page, QUESTION);
  await page.waitForTimeout(800);
  await shot(page, "ask", "thread");
});

test("ask: video", async ({ browser, page: setup }) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 700 });
  await loadContract(setup);
  const page = await videoPage(browser, "Ask your data");
  await page.goto("/");
  await newThread(page);
  await caption(
    page,
    "Foundry opens on Ask, which reads the ontology's current load",
    2400,
  );
  await caption(page, "Ask in your words, or start from a suggestion", 2200);
  const form = page.getByRole("form", { name: "Ask a question" });
  await type(form.getByRole("textbox", { name: /^Question/ }), QUESTION);
  await click(form.getByRole("button", { name: "Ask", exact: true }));
  await caption(
    page,
    "Each step shows what it read from the database, as it runs",
  );
  await answered(page, QUESTION);
  await page.waitForTimeout(1500);
  await poster(page, "ask", "ask");
  await caption(page, "The answer cites its sources: open any of them", 2600);
  await caption(page, "Follow up in the same thread");
  const followUp = page.getByRole("form", { name: "Ask a follow-up" });
  const next = "When does that agreement renew?";
  await type(followUp.getByRole("textbox", { name: /^Question/ }), next);
  await click(followUp.getByRole("button", { name: "Ask", exact: true }));
  await answered(page, next);
  await page.waitForTimeout(1500);
  await caption(page, "Threads stay in the sidebar, newest first", 2600);
  await saveVideo(page, "ask", "ask");
});
