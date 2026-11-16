import type { Page } from "@playwright/test";
import type { FoundryAgentThread, FoundryQuestion } from "@/shared/wfe";
import {
  choose,
  expect,
  loadContracts,
  loadVersion3,
  makeContractsCurrent,
  mock,
  postToWfe,
  signIn,
  startFounderThread,
  test,
  uploadAndLoadContract,
} from "./support/mock";

// Founder's page (/ask, and /, which opens it): Founder full page, in the
// Analyst page's chrome. A new chat asks what the user wants to know, with
// the load's competency questions as chips that fill the composer; a
// message is a thread of Founder's, whose answers are the Analyst's, inline.
// Its threads are the dock's, in the sidebar by recency (the page opens the
// dock's thread, and the dock the page's), with the Analyst's threads from
// before Founder under them, read only. The page has no dock. On a phone the
// threads are the page until one is chosen. The Load picker's load goes
// with each message, as does an entity's name from its page.

const dockOf = (page: Page) =>
  page.getByRole("complementary", { name: "Founder" });
const composerOf = (page: Page) =>
  page.getByRole("main").getByRole("form", { name: "Message Founder" });
const logOf = (page: Page) =>
  page.getByRole("main").getByRole("log", { name: "Conversation" });
/** The sidebar's threads: Founder's, then the Analyst's. */
const threadsOf = (page: Page) =>
  page
    .getByRole("region", { name: "Threads" })
    .getByRole("navigation", { name: "Threads" });
const HELLO = "What do you want to know about Customer contracts?";
const ANSWERED =
  "That's the answer, from the current load of Customer contracts.";

/** Fast steps for Founder, the Analyst and loads. */
const fast = () =>
  mock.configure({
    agentStepMs: 100,
    agentDeltaMs: 10,
    askStepMs: 100,
    loadStepMs: 100,
  });

/** Sends `text` from Founder's page's composer. */
async function send(page: Page, text: string) {
  const composer = composerOf(page);
  await composer.getByRole("textbox", { name: "Message" }).fill(text);
  await composer.getByRole("button", { name: "Send", exact: true }).click();
  await expect(logOf(page).getByText(text, { exact: true })).toBeVisible();
}

test("opens / on Founder's page, where a suggestion fills the composer and the thread it starts is the dock's too", async ({
  page,
}) => {
  await fast();
  await signIn(page);
  await loadContracts(page);
  // The main page is Founder's, on the ontology last opened.
  await page.goto("/");
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts$/);
  await expect(
    page
      .getByRole("navigation", { name: "Main navigation" })
      .getByRole("link", { name: "Founder", exact: true }),
  ).toHaveAttribute("aria-current", "page");
  await expect(
    page.getByRole("heading", { level: 1, name: HELLO }),
  ).toBeVisible();
  // A new chat: an empty composer that sends this page, and the load's
  // version's competency questions as suggestions.
  const composer = composerOf(page),
    box = composer.getByRole("textbox", { name: "Message" });
  await expect(box).toHaveValue("");
  await expect(
    composer.getByText("Looking at: Customer contracts", { exact: true }),
  ).toBeVisible();
  const suggestions = page.getByRole("group", { name: "Suggested questions" });
  await expect(suggestions.getByRole("button")).toHaveText([
    "Which customers are under contract, and which contracts are they signed with?",
    "What is each customer under contract billed?",
  ]);
  await expect(page.getByRole("link", { name: "New chat" })).toHaveAttribute(
    "aria-current",
    "page",
  );

  // A click fills the composer and sends nothing; Send starts a thread.
  const question = "What is each customer under contract billed?";
  await suggestions.getByRole("button", { name: question }).click();
  await expect(box).toHaveValue(question);
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts$/);
  await composer.getByRole("button", { name: "Send", exact: true }).click();
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts\?chat=thr_\w+$/);
  const log = logOf(page);
  await expect(log.getByText(question, { exact: true })).toBeVisible();

  // Founder asks the Analyst, whose answer shows inline.
  await expect(log.getByText(ANSWERED)).toBeVisible({ timeout: 20_000 });
  await expect(
    log
      .getByRole("article", { name: `Question: ${question}` })
      .getByRole("region", { name: "Answer" }),
  ).toBeVisible();
  // The bar holds the thread's title; the sidebar lists it today, open.
  await expect(
    page.getByRole("heading", { level: 1, name: question }),
  ).toBeVisible();
  await expect(
    threadsOf(page)
      .getByRole("list", { name: "Today" })
      .getByRole("link", { name: /^What is each customer under contract/ }),
  ).toHaveAttribute("aria-current", "page");
  // The conversation followed its end, the reply's composer pinned at the
  // window's foot.
  const reply = composerOf(page);
  await expect(reply.getByRole("textbox", { name: "Message" })).toHaveAttribute(
    "placeholder",
    "Reply to Founder…",
  );
  await expect
    .poll(() =>
      log.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight),
    )
    .toBeLessThan(48);
  const foot = (await reply.boundingBox())!;
  expect(foot.y + foot.height).toBeGreaterThan(900 - 120);
  expect(foot.y + foot.height).toBeLessThanOrEqual(900);

  // Elsewhere, the dock holds the same thread.
  await page.goto("/dashboards");
  await page.getByRole("button", { name: "Founder", exact: true }).click();
  const dock = dockOf(page),
    menu = dock.getByRole("button", { name: /^Threads:/ });
  await expect(menu).toContainText(question);
  await expect(
    dock.getByRole("log", { name: "Conversation" }).getByText(ANSWERED),
  ).toBeVisible();
  await menu.click();
  await expect(
    page.getByRole("menuitemradio", { name: question }),
  ).toHaveAttribute("aria-checked", "true");
});

test("opens a thread started in the dock, and shows no dock of its own", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 100, agentDeltaMs: 10 });
  await signIn(page);
  await page.goto("/dashboards");
  await page.getByRole("button", { name: "Founder", exact: true }).click();
  const dock = dockOf(page);
  await dock
    .getByRole("textbox", { name: "Message" })
    .fill("What ontologies do I have?");
  await dock.getByRole("button", { name: "Send", exact: true }).click();
  const answer = /^This workspace has \d+ ontologies: /;
  await expect(dock.getByText(answer)).toBeVisible({ timeout: 20_000 });
  const palette = page.getByRole("dialog", { name: "Search your workspace" });
  await page.keyboard.press("ControlOrMeta+k");
  await palette.getByRole("combobox").fill("founder");
  await expect(
    palette.getByRole("option", { name: /Close Founder/ }),
  ).toBeVisible();
  await page.keyboard.press("Escape");

  // Founder's page opens the dock's thread, listed and open in the sidebar.
  await page
    .getByRole("navigation", { name: "Main navigation" })
    .getByRole("link", { name: "Founder", exact: true })
    .click();
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts$/);
  await expect(logOf(page).getByText(answer)).toBeVisible();
  await expect(
    page.getByRole("heading", {
      level: 1,
      name: "What ontologies do I have?",
    }),
  ).toBeVisible();
  const link = threadsOf(page).getByRole("link", {
    name: /^What ontologies do I have\?/,
  });
  await expect(link).toHaveAttribute("aria-current", "page");

  // The page is Founder: no dock, nor a way to open one.
  await expect(dockOf(page)).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Founder", exact: true }),
  ).toHaveCount(0);
  await page.keyboard.press("ControlOrMeta+j");
  await expect(dockOf(page)).toHaveCount(0);
  await page.keyboard.press("ControlOrMeta+k");
  await palette.getByRole("combobox").fill("founder");
  await expect(
    palette.getByRole("option", { name: /(Open|Close) Founder/ }),
  ).toHaveCount(0);
  await page.keyboard.press("Escape");

  // Its link names it, and /ask keeps that as it opens the ontology.
  const href = (await link.getAttribute("href"))!;
  expect(href).toMatch(/^\/ask\/ont_acme_contracts\?chat=thr_\w+$/);
  const id = new URLSearchParams(href.split("?")[1]).get("chat")!;
  await page.goto(`/ask?chat=${id}`);
  await expect(page).toHaveURL(
    new RegExp(`/ask/ont_acme_contracts\\?chat=${id}$`),
  );
  await expect(logOf(page).getByText(answer)).toBeVisible();

  // Back on another page, the dock is open again.
  await page.goto("/dashboards");
  await expect(dockOf(page).getByText(answer)).toBeVisible();
});

test("opens an Analyst thread from before Founder read only, from the sidebar", async ({
  page,
}) => {
  await fast();
  await signIn(page);
  await uploadAndLoadContract(page);
  const question = "How are the invoices doing?";
  await postToWfe(
    page,
    "/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts/questions",
    { question, clientToken: "e2e-analyst-thread" },
  );
  await page.goto("/ask/ont_acme_contracts");
  // Under Founder's threads, marked Earlier question.
  const link = threadsOf(page)
    .getByRole("list", { name: "Earlier questions" })
    .getByRole("link", { name: /^How are the invoices doing\?/ });
  await expect(link.locator(".hf-tag")).toHaveText("Earlier question");
  await link.click();
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts\?thread=q_\w+$/);
  await expect(link).toHaveAttribute("aria-current", "page");

  // Its answer as the Analyst gave it: citations, charts and tables, each
  // of which can go on a dashboard.
  const main = page.getByRole("main");
  await expect(
    main.getByRole("heading", { level: 1, name: question }),
  ).toBeVisible();
  await expect(main.locator(".ak-bar .hf-tag")).toHaveText(
    "Earlier question",
  );
  const answer = main
    .getByRole("article", { name: `Question: ${question}` })
    .getByRole("region", { name: "Answer" });
  const byStatus = answer.getByRole("figure", { name: "Invoices by status" });
  await expect(byStatus).toBeVisible({ timeout: 15_000 });
  await expect(
    byStatus.getByRole("button", { name: "Add to dashboard" }),
  ).toBeVisible();
  const sources = answer.getByRole("list", { name: "Sources" });
  await expect(sources.getByRole("listitem").first()).toBeVisible();

  // Read only: no composer, but the way on to Founder.
  await expect(composerOf(page)).toHaveCount(0);
  await expect(main.getByRole("form", { name: "Ask a follow-up" })).toHaveCount(
    0,
  );
  await expect(main).toContainText(
    "This earlier question is read only. Ask Founder to go on.",
  );
  await main.getByRole("link", { name: "New chat" }).click();
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts\?new=1$/);
  await expect(
    page.getByRole("heading", { level: 1, name: HELLO }),
  ).toBeVisible();
  await expect(composerOf(page)).toBeVisible();
});

test("sends the load the picker chose with each message", async ({ page }) => {
  await fast();
  await signIn(page);
  await makeContractsCurrent(page);
  const first = await loadVersion3(page);
  await loadVersion3(page);
  await page.goto("/ask/ont_acme_contracts?new=1");
  const composer = composerOf(page),
    picker = page.getByRole("combobox", { name: "Load", exact: true });
  await expect(picker).toHaveText("Current · v3");
  await expect(
    composer.getByText("Looking at: Customer contracts", { exact: true }),
  ).toBeVisible();
  // The older load, in the URL and in what the composer sends.
  await choose(picker, /^v3 · /);
  await expect(page).toHaveURL(new RegExp(`[?&]load=${first}(&|$)`));
  await expect(
    composer.getByText("Looking at: Customer contracts / v3 load", {
      exact: true,
    }),
  ).toBeVisible();
  const sent = page.waitForRequest(
    (r) => r.method() === "POST" && r.url().endsWith("/foundry/agent/turns"),
  );
  await send(page, "What ontologies do I have?");
  expect((await sent).postDataJSON().pageContext).toEqual({
    ontologyId: "ont_acme_contracts",
    loadId: first,
  });
  // The thread keeps it.
  await expect(page).toHaveURL(new RegExp(`\\?load=${first}&chat=thr_\\w+$`));
  await expect(picker).toHaveText(/^v3 · /);
});

test("asks about an entity from its page, in a new chat that names it", async ({
  page,
}) => {
  await fast();
  await signIn(page);
  await uploadAndLoadContract(page);
  await page.goto(
    `/explore/ont_acme_contracts/entities/${Buffer.from("Customer\0k:cus_GLOBEX01").toString("base64url")}`,
  );
  await expect(
    page.getByRole("heading", { level: 1, name: "Globex Corporation" }),
  ).toBeVisible();
  await page.getByRole("link", { name: "Ask about this" }).click();
  await expect(page).toHaveURL(
    /\/ask\/ont_acme_contracts\?about=Globex\+Corporation&new=1$/,
  );
  // Its message starts with the entity's name.
  const box = composerOf(page).getByRole("textbox", { name: "Message" });
  await expect(box).toHaveValue("About Globex Corporation: ");
  await box.press("End");
  await page.keyboard.type("which invoices did they get?");
  const text = "About Globex Corporation: which invoices did they get?";
  await composerOf(page)
    .getByRole("button", { name: "Send", exact: true })
    .click();
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts\?chat=thr_\w+$/);
  await expect(
    logOf(page)
      .getByRole("article", { name: `Question: ${text}` })
      .getByRole("region", { name: "Answer" }),
  ).toBeVisible({ timeout: 20_000 });
});

test("says why the Analyst refused a question, in Founder's words", async ({
  page,
}) => {
  await fast();
  await mock.configure({ askSpentUsdMicros: 29_600_000 });
  await signIn(page);
  await uploadAndLoadContract(page);
  await page.goto("/ask/ont_acme_contracts");
  await send(page, "Which customers signed it?");
  await expect(
    logOf(page).getByText(
      "This workspace has used the month's budget for questions of the data: that limit is on questions, not on me. It starts again next month.",
    ),
  ).toBeVisible({ timeout: 20_000 });
});

/** Founder's threads the list answers with, for layout. */
async function listThreads(page: Page, threads: FoundryAgentThread[]) {
  await page.route(
    (url) => url.pathname.endsWith("/foundry/agent/threads"),
    (route) =>
      route.request().method() === "GET"
        ? route.fulfill({ json: { threads } })
        : route.fallback(),
  );
}
/** The Analyst's questions the list answers with, for layout. */
async function listQuestions(page: Page, questions: FoundryQuestion[]) {
  await page.route(
    (url) =>
      url.pathname.endsWith("/ontologies/ont_acme_contracts/questions") &&
      !url.searchParams.has("threadId"),
    (route) =>
      route.fulfill({
        json: {
          questions,
          budget: {
            monthlyUsdMicros: "30000000",
            spentUsdMicros: "280000",
            reservedUsdMicros: "0",
            questionUsdMicros: "500000",
          },
        },
      }),
  );
}
const day = 86_400_000;
/** Local midnight `days` days back. */
const midnight = (days: number) => {
  const d = new Date();
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() - days);
};
const chat = (id: string, title: string, at: Date): FoundryAgentThread => ({
  id,
  title,
  createdAt: at.toISOString(),
  updatedAt: at.toISOString(),
});

test("groups Founder's threads by when they last changed, the Analyst's under them", async ({
  page,
}) => {
  await listThreads(page, [
    chat("thr_now", "Who signed the services agreement?", new Date()),
    chat(
      "thr_week",
      "Which invoices bill no customer under contract?",
      new Date(midnight(3).getTime() + day / 2),
    ),
    chat(
      "thr_old",
      "What did Globex sign last year?",
      new Date(midnight(30).getTime() + day / 2),
    ),
    chat("thr_blank", "", new Date(midnight(40).getTime() + day / 2)),
  ]);
  await listQuestions(page, [
    {
      id: "q_old",
      threadId: "q_old",
      question: "Who renewed last quarter?",
      status: "answered",
      createdAt: new Date().toISOString(),
    },
  ]);
  await signIn(page);
  await page.goto("/ask/ont_acme_contracts");
  const threads = threadsOf(page);
  await expect(threads.getByRole("heading", { level: 2 })).toHaveText([
    "Today",
    "Previous 7 days",
    "Older",
    "Earlier questions",
  ]);
  await expect(
    threads.getByRole("list", { name: "Today" }).getByRole("link"),
  ).toHaveText(["Who signed the services agreement?just now"]);
  await expect(
    threads.getByRole("list", { name: "Previous 7 days" }).getByRole("link"),
  ).toHaveText([/^Which invoices bill no customer under contract\?\dd ago$/]);
  await expect(
    threads.getByRole("list", { name: "Older" }).getByRole("link"),
  ).toHaveText([/^What did Globex sign last year\?/, /^Untitled/]);
  await expect(
    threads.getByRole("list", { name: "Earlier questions" }).getByRole("link"),
  ).toHaveText([/^Who renewed last quarter\?Earlier question/]);
  // Each opens on Founder's page: a chat, or the Analyst's thread.
  await expect(
    threads.getByRole("link", { name: /^Who signed the services/ }),
  ).toHaveAttribute("href", "/ask/ont_acme_contracts?chat=thr_now");
  await expect(
    threads.getByRole("link", { name: /^Who renewed last quarter/ }),
  ).toHaveAttribute("href", "/ask/ont_acme_contracts?thread=q_old");
  // A new chat is the page.
  await expect(page.getByRole("link", { name: "New chat" })).toHaveAttribute(
    "aria-current",
    "page",
  );
});

test("on a phone, shows Founder's threads as the page until one is chosen, and in the drawer", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 100, agentDeltaMs: 10 });
  await signIn(page);
  await startFounderThread(page, "What ontologies do I have?");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/ask/ont_acme_contracts");
  // No sidebar beside the page, and no dock: the threads are the page.
  const main = page.getByRole("main");
  await expect(page.getByRole("region", { name: "Threads" })).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Founder", exact: true }),
  ).toHaveCount(0);
  await expect(main.getByRole("heading", { level: 1 })).toHaveText("Founder");
  const list = main.getByRole("navigation", { name: "Threads" }),
    thread = list.getByRole("link", { name: /^What ontologies do I have\?/ });
  await expect(
    list.getByRole("list", { name: "Today" }).getByRole("link"),
  ).toHaveText([/^What ontologies do I have\?/]);
  await expect(
    main.getByRole("link", { name: "New chat" }),
  ).not.toHaveAttribute("aria-current");

  // A thread opens where the list was, and Threads goes back.
  await thread.click();
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts\?chat=thr_\w+$/);
  await expect(
    logOf(page).getByText(/^This workspace has \d+ ontologies: /),
  ).toBeVisible({ timeout: 20_000 });
  await main.getByRole("button", { name: "Threads" }).click();
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts$/);
  await expect(thread).toBeVisible();
  // So does New chat.
  await main.getByRole("link", { name: "New chat" }).click();
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts\?new=1$/);
  await expect(composerOf(page)).toBeVisible();
  await main.getByRole("button", { name: "Threads" }).click();
  await expect(thread).toBeVisible();

  // The drawer holds the sidebar, the threads under its sections; choosing
  // a thread closes it.
  await page.getByRole("button", { name: "Open navigation" }).click();
  const drawer = page.getByRole("dialog", { name: "Navigation" });
  const sections = drawer.getByRole("navigation", { name: "Main navigation" });
  await expect(sections.getByRole("link")).toHaveCount(4);
  await expect(
    sections.getByRole("link", { name: "Founder", exact: true }),
  ).toHaveAttribute("aria-current", "page");
  const threads = drawer.getByRole("region", { name: "Threads" });
  await expect(threads.getByRole("link", { name: "New chat" })).toBeVisible();
  await threads
    .getByRole("link", { name: /^What ontologies do I have\?/ })
    .click();
  await expect(drawer).toHaveCount(0);
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts\?chat=thr_\w+$/);
});

test("on a phone, says why Founder's threads couldn't be read, where they would show", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  let failing = true;
  await page.route(
    (url) => url.pathname.endsWith("/foundry/agent/threads"),
    (route) =>
      failing && route.request().method() === "GET"
        ? route.fulfill({
            status: 503,
            json: { code: 14, message: "the threads are unavailable" },
          })
        : route.continue(),
  );
  await signIn(page);
  await page.goto("/ask/ont_acme_contracts");
  const threads = page
    .getByRole("main")
    .getByRole("navigation", { name: "Threads" });
  await expect(threads.getByRole("alert")).toContainText(
    "Couldn’t read the threads: the threads are unavailable",
    { timeout: 15_000 },
  );
  // Read again, there are none to list: a new chat shows straight away.
  failing = false;
  await threads.getByRole("button", { name: "Retry" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: HELLO }),
  ).toBeVisible();
  await expect(composerOf(page)).toBeVisible();
  await expect(page.getByRole("button", { name: "Threads" })).toHaveCount(0);
});
