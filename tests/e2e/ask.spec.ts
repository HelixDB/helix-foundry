import { readFileSync } from "node:fs";
import type { Page } from "@playwright/test";
import type { FoundryQuestion } from "@/shared/wfe";
import { expect, explorer, mock, signIn, test } from "./support/mock";

// Ask: a question about the loaded contract, its steps as they stream (each
// input in words, the query as a readable path, and its JSON; a failed call
// says why, a refused answer says it was corrected, the accepted one says
// Answered), the answer as plain text with citation chips (none
// of the model's markup renders), and chips that open the document at the
// quoted span, the entity, the table row, the derived fact and the rule
// break. A question the ontology can't answer drafts a new version with it,
// a refused one says why, and an entity's page asks about it in a drawer.
// The layout is a chat app's: the threads in the sidebar, grouped by
// recency; a new thread's question and suggestions; the composer pinned
// under the conversation; and on a phone, the threads as the page.

const O = "/ontology/ont_acme_contracts",
  X = "/explore/ont_acme_contracts";
const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
);

// Uploads the seeded contract, makes version 3 current and loads it.
async function loadContract(page: Page) {
  await page.goto("/data");
  await explorer(page).getByLabel("Choose documents").setInputFiles({
    name: "contract.md",
    mimeType: "text/markdown",
    buffer: CONTRACT,
  });
  await expect(
    page
      .getByRole("list", { name: "Uploads" })
      .getByRole("listitem", { name: "contract.md" }),
  ).toContainText("Uploaded");
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
  await page.goto(O);
  await page
    .getByRole("status")
    .filter({ hasText: "Version 3 isn’t loaded yet" })
    .getByRole("button", { name: "Load data" })
    .click();
  await page
    .getByRole("dialog", { name: "Load version 3 into contracts?" })
    .getByRole("button", { name: "Load" })
    .click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 20_000 });
}

async function ask(page: Page, question: string) {
  await page.getByRole("link", { name: "Analyst", exact: true }).click();
  await expect(
    page.getByRole("heading", {
      level: 1,
      name: "What do you want to know about Customer contracts?",
    }),
  ).toBeVisible();
  const form = page.getByRole("form", { name: "Ask a question" });
  await form.getByRole("textbox", { name: /^Question/ }).fill(question);
  await form.getByRole("button", { name: "Ask", exact: true }).click();
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts\?thread=q_/);
  return page.getByRole("article", { name: `Question: ${question}` });
}

test("asks about a load: streamed steps, a plain-text answer, chips that open the span", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 250, askMarkup: true });
  await signIn(page);
  await loadContract(page);

  const question = await ask(page, "Who signed the services agreement?");
  // The steps stream in as tool calls: each the tool's name, what it was
  // asked in words and what it returned, opening on its Input and Output.
  // First the engine's read of what the load holds, its summary only (its
  // event carries no result, and its input is empty), so nothing to open;
  // then each query as a path. A lookup the engine can't run yet fails, and
  // says why.
  const steps = question.getByRole("list", { name: "Steps" });
  const step = (name: string | RegExp) =>
    steps.getByRole("listitem", {
      name:
        typeof name === "string" ? new RegExp(`^Step \\d+: ${name}$`) : name,
    });
  const profile = steps.getByRole("listitem").first();
  await expect(profile).toHaveAccessibleName(
    "Step 1: Read what the load holds",
  );
  await expect(profile).toContainText(/^load_profile/);
  await expect(profile).toContainText(/\d+ of \d+ reads$/);
  await expect(profile.getByRole("button")).toHaveCount(0);
  await expect(profile.locator("pre")).toHaveCount(0);
  const lookup = steps.getByRole("listitem").nth(1);
  await expect(lookup).toContainText("find_entities");
  await expect(lookup).toContainText(
    "Customer with the identifier “cus_GLOBEX01”",
  );
  await expect(lookup).toContainText(
    "Failed: can't look entities up by identifier yet",
  );
  await expect(lookup).toHaveClass(/is-error/);
  await expect(question.getByRole("status")).toContainText("Answering");
  const queried = step("Queried").first();
  await expect(queried).toContainText("Contract → signed_with → Customer");
  await expect(queried.getByRole("region", { name: "Input" })).toHaveCount(0);
  await queried.getByRole("button").click();
  await expect(queried.getByRole("button")).toHaveAttribute(
    "aria-expanded",
    "true",
  );
  await expect(
    queried.getByRole("region", { name: "Input" }).getByLabel("Query"),
  ).toContainText("Contract → signed_with → Customer");

  // The answer: plain text and numbered chips; the model's image, link and
  // emphasis are dropped, its words kept.
  const answer = question.getByRole("region", { name: "Answer" });
  await expect(answer).toContainText("Globex Corporation", { timeout: 10_000 });
  await expect(answer).toContainText("Read more now");
  await expect(answer.locator("img, script, b, strong")).toHaveCount(0);
  await expect(answer.locator('a[href^="http"]')).toHaveCount(0);
  await expect(answer).not.toContainText("**");
  // Sources name entities as their pages do (a customer by its company
  // name, not its key), and a result by the step that read it.
  const sources = answer.getByRole("list", { name: "Sources" });
  await expect(sources.getByRole("listitem")).toHaveText([
    "Customer Globex Corporation",
    "Contract Master services agreement",
    "Contract Master services agreement signed_with Customer Globex Corporation",
    "Quote in contract.md",
    "Stripe · customers row cus_GLOBEX01",
    "co signed: Customer Globex Corporation, Customer Initech LLC (derived)",
    "A break of invoice_under_contract",
    "The query in step 4",
    "The graph query in step 11",
    "The counts in step 9",
    "The query in step 10",
  ]);
  // The graph query the model wrote opens on its code, with no Output:
  // its event carries none.
  const graph = step("Queried the graph directly");
  await expect(graph).toContainText("helix_query");
  await expect(graph).toContainText("At most 50 results");
  await expect(graph).toContainText(/a value$/);
  await graph.getByRole("button").click();
  await expect(graph.getByLabel("Graph query")).toContainText(
    '"string": "Contract"',
  );
  await expect(graph.getByRole("region", { name: "Output" })).toHaveCount(0);
  // Each step's input reads in words, never as the model's handles; its
  // arguments show once it is opened.
  const read = step("Read the evidence");
  await expect(read).toContainText(
    "For Contract Master services agreement signed_with Customer Globex Corporation",
  );
  await expect(read).toContainText(/2 items$/);
  await expect(read.locator(".ak-call-what")).not.toContainText("n1");
  await read.getByRole("button").click();
  await expect(read.getByLabel("Arguments")).toContainText('"subject": "n2"');
  await expect(step("Explained")).toContainText(
    "Why co_signed holds for Customer Globex Corporation and Customer Initech LLC",
  );
  // The first answer was refused: the page says why in its own words, the
  // engine's words to the model once the call is opened; the second
  // answered, its blocks not repeated in the steps.
  const refused = step("Answer");
  await expect(refused).toContainText("Refused");
  await expect(refused).toContainText(
    "An answer cited something no step had shown; the agent corrected it.",
  );
  await expect(refused.locator("pre")).toHaveCount(0);
  await refused.getByRole("button").click();
  await expect(
    refused.getByRole("region", { name: "What the agent was told" }),
  ).toContainText("citation c2: e4 didn't show");
  const answered = steps.getByRole("listitem").last();
  await expect(answered).toHaveText("Answered");
  await expect(
    question.getByRole("link", { name: "Draft a new version with this" }),
  ).toHaveCount(0);
  await expect(step("Read rule breaks")).toContainText("5 records, more");
  // Spend shows against the questions budget, under the threads, with the
  // limits behind it.
  const budget = page
    .getByRole("region", { name: "Analyst threads" })
    .getByRole("group", { name: "Questions budget" });
  await expect(budget).toContainText("$0.07 of $30.00 this month");
  await expect(budget.getByText(/A question spends at most/)).toBeHidden();
  await budget.getByText("$0.07 of $30.00 this month").click();
  await expect(budget).toContainText(
    "A question spends at most $0.50, 20 steps and 4 minutes; 2 run at a time and 30 an hour per workspace.",
  );

  // A result chip lights the step that returned it.
  await answer
    .getByRole("button", { name: "Source 8: The query in step 4" })
    .click();
  await expect(queried).toHaveClass(/is-lit/);
  await answer
    .getByRole("button", { name: "Source 9: The graph query in step 11" })
    .click();
  await expect(graph).toHaveClass(/is-lit/);
  await expect(queried).not.toHaveClass(/is-lit/);

  // The quote chip opens the document at the span, in the question's load.
  await answer
    .getByRole("link", { name: "Source 4: Quote in contract.md" })
    .click();
  await expect(page).toHaveURL(
    /\/documents\/doc_\w+\?load=load_\w+#s0-\d+-\d+$/,
  );
  // The span's runs are lit, the quoted name among them.
  const text = page.getByLabel("Document text");
  await expect(text.locator("mark.is-active").first()).toBeVisible();
  await expect(
    text.locator("mark.is-active", { hasText: "Globex Corporation" }),
  ).toBeVisible();

  // The entity chip opens its page; the row chip, its table.
  await page.goBack();
  await answer
    .getByRole("link", { name: "Source 1: Customer Globex Corporation" })
    .click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Globex Corporation" }),
  ).toBeVisible();
  await page.goBack();
  await answer
    .getByRole("link", {
      name: "Source 5: Stripe · customers row cus_GLOBEX01",
    })
    .click();
  await expect(page).toHaveURL(
    /\/data\/connections\/conn_acme_stripe\/tables\/customers$/,
  );

  // The derived fact opens its first entity's Derived tab, where the rules
  // say why.
  await page.goBack();
  await answer.getByRole("link", { name: /^Source 6: co signed/ }).click();
  await expect(page.getByRole("tab", { name: /^Derived/ })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await expect(
    page
      .getByRole("region", { name: "co_signed to" })
      .getByRole("link", { name: "Initech LLC" }),
  ).toBeVisible();

  // The rule break opens the load's rule breaks of its constraint, from the
  // cited record.
  await page.goBack();
  await answer
    .getByRole("link", {
      name: "Source 7: A break of invoice_under_contract",
    })
    .click();
  await expect(page).toHaveURL(
    /\/loads\/load_\w+\/rule-breaks\?constraint=invoice_under_contract&key=[0-9a-f]{64}$/,
  );
  const breaks = page.getByRole("list", { name: "Rule breaks" });
  await expect(breaks.locator('li[aria-current="true"]')).toHaveCount(1);
  await expect(breaks.getByRole("listitem").first()).toHaveAttribute(
    "aria-current",
    "true",
  );
  await expect(page.getByText("From the cited break")).toBeVisible();
  await page.getByRole("button", { name: "From the first" }).click();
  await expect(page.getByText("1–50 of 62 breaks")).toBeVisible();

  // A follow-up stays in the thread.
  await page.goBack();
  await expect(page.getByRole("navigation", { name: "Threads" })).toContainText(
    "Who signed the services agreement?",
  );
  const follow = page.getByRole("form", { name: "Ask a follow-up" });
  await follow
    .getByRole("textbox", { name: /^Question/ })
    .fill("And what do they pay?");
  await follow.getByRole("button", { name: "Ask", exact: true }).click();
  await expect(
    page.getByRole("article", { name: "Question: And what do they pay?" }),
  ).toBeVisible();
  await expect(
    follow.getByRole("button", { name: "Ask", exact: true }),
  ).toBeDisabled();
});

test("shows query results after the paragraphs that introduce them, as charts and tables", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 100, askMarkup: true });
  await signIn(page);
  await loadContract(page);
  const question = await ask(page, "How are the invoices doing?");
  const answer = question.getByRole("region", { name: "Answer" });
  // The breakdown follows the paragraph that counts the invoices, drawn
  // from the query's output once the question is read with it.
  const byStatus = answer.getByRole("figure", { name: "Invoices by status" });
  await expect(byStatus).toBeVisible({ timeout: 15_000 });
  await expect(
    answer
      .getByRole("figure", { name: "Largest invoices" })
      .locator(".recharts-bar-rectangle"),
  ).toHaveCount(10);
  // The conversation followed the visuals to its end as they arrived.
  await expect
    .poll(() =>
      page
        .locator(".ak-scroll")
        .evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight),
    )
    .toBeLessThan(2);
  expect(
    await byStatus.evaluate((el) => el.previousElementSibling?.textContent),
  ).toMatch(/^\d+ of the 65 invoices are \w+/);
  await expect(byStatus).toContainText("5 groups");
  const bars = byStatus.locator(".recharts-bar-rectangle");
  await expect(bars).toHaveCount(5);
  // Hovering a bar says its exact value.
  await bars.first().hover();
  await expect(byStatus.locator(".ak-viz-tip")).toContainText(/^\d+\w+/);
  // The toggle shows its table: every group, sortable.
  await byStatus.getByRole("radio", { name: "Table" }).click();
  const statuses = byStatus.getByRole("grid", { name: "Invoices by status" });
  await expect(statuses.getByRole("columnheader")).toHaveText([
    "Invoice.status",
    "Count",
  ]);
  await expect(statuses.getByRole("row")).toHaveCount(6);
  // The engine wraps every group's value as a document's; none is shown
  // as one.
  await expect(statuses.locator(".ak-untrusted")).toHaveCount(0);
  const counts = () =>
    statuses
      .getByRole("row")
      .locator('[role="gridcell"]:nth-child(2)')
      .allTextContents();
  const most = await counts();
  await statuses.getByRole("columnheader", { name: "Count" }).click();
  expect(await counts()).toEqual(
    [...most].sort((a, b) => Number(a) - Number(b)),
  );
  await byStatus.getByRole("radio", { name: "Chart" }).click();
  await expect(bars).toHaveCount(5);

  // The ranking follows the paragraph naming the largest: its bars are
  // invoices, whose labels open their pages in the question's load.
  const largest = answer.getByRole("figure", { name: "Largest invoices" });
  expect(
    await largest.evaluate((el) => el.previousElementSibling?.textContent),
  ).toMatch(/^The largest is in_\d+, for [\d,]+, billed to /);
  await expect(largest).toContainText("10 of 65 rows");
  const first = largest.getByRole("link").first();
  const name = (await first.getAttribute("aria-label"))!;
  expect(name).toMatch(/^in_\d+$/);
  await first.click();
  await expect(page).toHaveURL(
    /\/explore\/ont_acme_contracts\/entities\/[\w-]+\?load=load_\w+$/,
  );
  await expect(page.getByRole("heading", { level: 1, name })).toBeVisible();
  await page.goBack();
  // Its table names the invoices and customers as links too.
  await largest.getByRole("radio", { name: "Table" }).click();
  const rows = largest.getByRole("grid", { name: "Largest invoices" });
  await expect(rows.getByRole("columnheader")).toHaveText([
    "Invoice",
    "Invoice.amount_due",
    "Customer",
  ]);
  await expect(rows.getByRole("link", { name, exact: true })).toBeVisible();
  // Show the query lights the step that ran it.
  await largest.getByRole("button", { name: "Show the query" }).click();
  const steps = question.getByRole("list", { name: "Steps" });
  const ranked = steps.getByRole("listitem").filter({
    hasText: "Invoice → billed_to → Customer",
  });
  await expect(ranked).toHaveClass(/is-lit/);

  // Each query step's Output shows its result the same way: the contract's
  // rows, the date a document gave shown as text.
  const signed = steps
    .getByRole("listitem")
    .filter({ hasText: "Contract → signed_with → Customer" });
  await signed.getByRole("button").click();
  const result = signed.getByRole("grid", {
    name: "Contract → signed_with → Customer",
  });
  await expect(result.getByRole("columnheader")).toHaveText([
    "Contract",
    "Customer",
    "Customer.name",
    "Contract.signed_on",
  ]);
  await expect(result.locator(".ak-untrusted").first()).toHaveText(
    "2026-10-01",
  );
  await expect(
    result.getByRole("link", { name: "Globex Corporation" }),
  ).toBeVisible();

  // Nothing the model wrote renders as markup, a link or an image.
  await page.mouse.move(0, 0);
  await expect(answer.locator("img, script, b, strong")).toHaveCount(0);
  await expect(answer.locator('a[href^="http"]')).toHaveCount(0);
  await expect(answer).toContainText("Read more now");
});

test("draws counts by day as a line on a time axis, every day there is", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 100 });
  // The breakdown as the engine would record counts by the day issued:
  // three days, the last a week after the second.
  await page.route(
    (url) =>
      /\/ontologies\/ont_acme_contracts\/questions\/[^/:]+$/.test(url.pathname),
    async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      const json = body.question?.resultJson;
      if (!json) return route.fulfill({ response, json: body });
      const result = JSON.parse(json);
      for (const q of result.queries ?? [])
        if (q.evidence_id === "e8" && q.output) {
          q.query.return = {
            group_count: { at: 0, prop: "issued_on", limit: 50 },
          };
          q.output = {
            kind: "group_count",
            groups: [
              { value: { untrusted: "2026-10-02" }, count: 30 },
              { value: { untrusted: "2026-10-01" }, count: 20 },
              { value: { untrusted: "2026-10-09" }, count: 15 },
            ],
            total_groups: 3,
          };
        }
      for (const v of result.answer?.visuals ?? [])
        if (v.evidence_id === "e8") v.title = "Invoices by day";
      body.question.resultJson = JSON.stringify(result);
      await route.fulfill({ response, json: body });
    },
  );
  await signIn(page);
  await loadContract(page);
  const question = await ask(page, "When were the invoices issued?");
  const byDay = question
    .getByRole("region", { name: "Answer" })
    .getByRole("figure", { name: "Invoices by day" });
  await expect(byDay.locator(".recharts-area-curve")).toBeVisible({
    timeout: 15_000,
  });
  await expect(byDay).toContainText("3 groups");
  // A tick at each day, oldest first, spaced by the time between them.
  const ticks = byDay.locator(
    ".recharts-xAxis-tick-labels .recharts-cartesian-axis-tick-value",
  );
  await expect(ticks).toHaveText(["Oct 1", "Oct 2", "Oct 9"]);
  const xs = await ticks.evaluateAll((els) =>
    els.map((el) => {
      const r = el.getBoundingClientRect();
      return r.x + r.width / 2;
    }),
  );
  expect((xs[2]! - xs[1]!) / (xs[1]! - xs[0]!)).toBeCloseTo(7, 0);
  // Hovering by a day says its count and date.
  const plot = byDay.locator(".recharts-cartesian-grid");
  await plot.scrollIntoViewIfNeeded();
  const box = (await plot.boundingBox())!;
  const tip = byDay.locator(".ak-viz-tip");
  await page.mouse.move(box.x + 2, box.y + box.height / 2);
  await expect(tip).toContainText("20");
  await expect(tip).toContainText("Oct 1, 2026");
  await page.mouse.move(box.x + box.width - 2, box.y + box.height / 2);
  await expect(tip).toContainText("15");
  await expect(tip).toContainText("Oct 9, 2026");
});

test("reads an answer from before visuals as text, its queries' results not kept", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 100, askVisuals: false });
  await signIn(page);
  await loadContract(page);
  const question = await ask(page, "Who signed the services agreement?");
  const answer = question.getByRole("region", { name: "Answer" });
  await expect(answer).toContainText("of the 65 invoices are", {
    timeout: 15_000,
  });
  await expect(answer.getByRole("figure")).toHaveCount(0);
  const signed = question
    .getByRole("list", { name: "Steps" })
    .getByRole("listitem")
    .filter({ hasText: "Contract → signed_with → Customer" });
  await signed.getByRole("button").click();
  await expect(signed).toContainText("Its result wasn’t kept with the answer.");
});

test("drafts a new version from a question the ontology can't answer", async ({
  page,
}) => {
  await mock.configure({
    loadStepMs: 200,
    askStepMs: 100,
    askOutcome: "cannot_answer",
  });
  await signIn(page);
  await loadContract(page);
  const question = await ask(
    page,
    "Who signed each contract for the customer?",
  );
  await expect(question).toContainText("This ontology can’t answer that", {
    timeout: 10_000,
  });
  await expect(question).toContainText("It lacks: A signed_by link");
  // The ending call shows as the callout, not its input.
  await expect(
    question.getByRole("list", { name: "Steps" }).getByRole("listitem").last(),
  ).toHaveText("Couldn’t answer");
  await question
    .getByRole("status")
    .filter({ hasText: "This ontology can’t answer that" })
    .getByRole("link", { name: "Draft a new version with this" })
    .click();
  await expect(page).toHaveURL(/\/generate\?goal=/);
  await expect(page.getByLabel("Goal")).toHaveValue(
    /Make the ontology able to answer: “Who signed each contract for the customer\?”\nIt lacks: A signed_by link/,
  );
});

test("says why a question is refused, in the WFE's words", async ({ page }) => {
  await mock.configure({ loadStepMs: 200, askSpentUsdMicros: 29_600_000 });
  await signIn(page);
  await loadContract(page);
  await page.getByRole("link", { name: "Analyst", exact: true }).click();
  const form = page.getByRole("form", { name: "Ask a question" });
  await form.getByRole("textbox", { name: /^Question/ }).fill("Who signed it?");
  await form.getByRole("button", { name: "Ask", exact: true }).click();
  await expect(form.getByRole("alert")).toHaveText(
    "The workspace has spent this month's $30.00 of questions.",
  );
});

test("asks about an entity from its page, in a drawer", async ({ page }) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 100 });
  await signIn(page);
  await loadContract(page);
  await page.goto(
    `${X}/entities/${Buffer.from("Customer\0k:cus_GLOBEX01").toString("base64url")}`,
  );
  await expect(
    page.getByRole("heading", { level: 1, name: "Globex Corporation" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Ask about this" }).click();
  const drawer = page.getByRole("dialog", {
    name: "Ask about Globex Corporation",
  });
  await expect(drawer).toContainText("About Customer cus_GLOBEX01");
  await drawer
    .getByRole("textbox", { name: /^Question/ })
    .fill("What did they sign?");
  await drawer.getByRole("button", { name: "Ask", exact: true }).click();
  await expect(drawer.getByRole("region", { name: "Answer" })).toContainText(
    "Globex Corporation",
    { timeout: 10_000 },
  );
  await drawer.getByRole("link", { name: "Open in Analyst" }).click();
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts\?thread=q_/);
  await expect(
    page.getByRole("article", { name: "Question: What did they sign?" }),
  ).toContainText("About Customer cus_GLOBEX01");
});

test("starts a thread from a suggestion, its composer pinned under the conversation", async ({
  page,
}) => {
  // Without visuals, the steps' last disclosure is in view at the end.
  await mock.configure({ loadStepMs: 200, askStepMs: 150, askVisuals: false });
  await signIn(page);
  await loadContract(page);
  // The load's version was read on the way: its draft's competency
  // questions are suggestions, under a composer with nothing in it.
  await page.getByRole("link", { name: "Analyst", exact: true }).click();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "What do you want to know about Customer contracts?",
  );
  const form = page.getByRole("form", { name: "Ask a question" }),
    box = form.getByRole("textbox", { name: /^Question/ });
  await expect(box).toHaveValue("");
  await expect(form).toContainText(
    // ⌘ on a Mac, Ctrl+ elsewhere (CI).
    /Answers from the current load \(v3\) · (⌘|Ctrl\+)Enter to ask/,
  );
  const suggestions = form.getByRole("group", { name: "Suggested questions" });
  await expect(suggestions.getByRole("button")).toHaveText([
    "Which customers are under contract, and which contracts are they signed with?",
    "What is each customer under contract billed?",
  ]);
  // A click asks it at once.
  await suggestions
    .getByRole("button", {
      name: "What is each customer under contract billed?",
    })
    .click();
  await expect(page).toHaveURL(/\?thread=q_/);
  // The bar holds the thread's title; the panel lists it today.
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "What is each customer under contract billed?",
  );
  const threads = page
    .getByRole("region", { name: "Analyst threads" })
    .getByRole("navigation", { name: "Threads" });
  await expect(threads.getByRole("heading", { level: 2 })).toHaveText([
    "Today",
  ]);
  await expect(
    threads.getByRole("link", { name: /^What is each/ }),
  ).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("region", { name: "Answer" })).toContainText(
    "Globex Corporation",
    { timeout: 10_000 },
  );

  // The conversation scrolls under the bar and the composer: it followed the
  // answer to its end, and the composer sits at the window's foot.
  const scroller = page.locator(".ak-scroll"),
    follow = page.getByRole("form", { name: "Ask a follow-up" });
  const metrics = () =>
    scroller.evaluate((el) => ({
      overflow: el.scrollHeight - el.clientHeight,
      fromEnd: el.scrollHeight - el.scrollTop - el.clientHeight,
    }));
  expect((await metrics()).overflow).toBeGreaterThan(100);
  await expect.poll(async () => (await metrics()).fromEnd).toBeLessThan(2);
  const pinned = async () => {
    const r = (await follow.boundingBox())!;
    expect(r.y + r.height).toBeGreaterThan(900 - 80);
    expect(r.y + r.height).toBeLessThanOrEqual(900);
  };
  await pinned();
  await expect(
    page.getByRole("button", { name: "Jump to latest" }),
  ).toHaveCount(0);
  // Scrolled up, it stays where the reader is and offers the way back.
  await scroller.hover();
  await page.mouse.wheel(0, -4000);
  await expect(
    page.getByRole("button", { name: "Jump to latest" }),
  ).toBeVisible();
  await expect.poll(async () => (await metrics()).fromEnd).toBeGreaterThan(100);
  await pinned();
  await page.getByRole("button", { name: "Jump to latest" }).click();
  await expect.poll(async () => (await metrics()).fromEnd).toBeLessThan(2);
  await expect(
    page.getByRole("button", { name: "Jump to latest" }),
  ).toHaveCount(0);
  // A step opened at the end stays where it is: the conversation stops
  // following, and offers the way back.
  const call = page
    .getByRole("list", { name: "Steps" })
    .getByRole("button")
    .last();
  await expect(call).toBeInViewport();
  const at = (await call.boundingBox())!.y;
  await call.click();
  await expect(call).toHaveAttribute("aria-expanded", "true");
  await expect(
    page.getByRole("button", { name: "Jump to latest" }),
  ).toBeVisible();
  expect((await call.boundingBox())!.y).toBeCloseTo(at, 0);

  // Once something was asked, a version no page has read isn't read for
  // suggestions: a new tab's new thread has none.
  await page.goto("/ask/ont_acme_contracts?new=1");
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "What do you want to know about Customer contracts?",
  );
  await expect(
    page.getByRole("form", { name: "Ask a question" }),
  ).toBeVisible();
  await expect(
    page.getByRole("group", { name: "Suggested questions" }),
  ).toHaveCount(0);
});

/** Questions the list answers with, in place of the mock's, for layout. */
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
const asked = (id: string, question: string, at: Date): FoundryQuestion => ({
  id,
  threadId: id,
  question,
  status: "answered",
  createdAt: at.toISOString(),
});
/** Local midnight `days` days back. */
const midnight = (days: number) => {
  const d = new Date();
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() - days);
};

test("groups the threads by when they were last asked in", async ({ page }) => {
  await listQuestions(page, [
    asked("q_now", "Who signed the services agreement?", new Date()),
    asked(
      "q_week",
      "Which invoices bill no customer under contract?",
      new Date(midnight(3).getTime() + day / 2),
    ),
    asked(
      "q_old",
      "What did Globex sign last year?",
      new Date(midnight(30).getTime() + day / 2),
    ),
  ]);
  await signIn(page);
  await page.goto("/ask/ont_acme_contracts");
  const threads = page
    .getByRole("region", { name: "Analyst threads" })
    .getByRole("navigation", { name: "Threads" });
  await expect(threads.getByRole("heading", { level: 2 })).toHaveText([
    "Today",
    "Previous 7 days",
    "Older",
  ]);
  await expect(
    threads.getByRole("list", { name: "Today" }).getByRole("link"),
  ).toHaveText(["Who signed the services agreement?just now"]);
  await expect(
    threads.getByRole("list", { name: "Previous 7 days" }).getByRole("link"),
  ).toHaveText([/^Which invoices bill no customer under contract\?\dd ago$/]);
  await expect(
    threads.getByRole("list", { name: "Older" }).getByRole("link"),
  ).toHaveText([/^What did Globex sign last year\?/]);
  // A new thread is the page; the month's spend sits under the threads.
  await expect(page.getByRole("link", { name: "New thread" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  await expect(
    page.getByRole("group", { name: "Questions budget" }),
  ).toContainText("$0.28 of $30.00 this month");
});

test("on a phone, shows the threads as the page and in the drawer's sidebar", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await listQuestions(page, [
    asked("q_now", "Who signed the services agreement?", new Date()),
  ]);
  await signIn(page);
  await page.goto("/ask/ont_acme_contracts");
  // No sidebar beside the page: the threads are the page until one is
  // chosen.
  await expect(
    page.getByRole("region", { name: "Analyst threads" }),
  ).toHaveCount(0);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Analyst");
  const main = page.getByRole("main");
  await expect(
    main.getByRole("navigation", { name: "Threads" }).getByRole("link"),
  ).toHaveText(["Who signed the services agreement?just now"]);
  await expect(
    main.getByRole("link", { name: "New thread" }),
  ).not.toHaveAttribute("aria-current");
  // New thread opens where the threads were (nothing is loaded yet, so it
  // offers to load data), and Threads goes back.
  await main.getByRole("link", { name: "New thread" }).click();
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts\?new=1$/);
  await expect(main.getByRole("link", { name: "Load data" })).toBeVisible();
  await page.getByRole("button", { name: "Threads" }).click();
  await expect(main.getByRole("navigation", { name: "Threads" })).toBeVisible();

  // The drawer holds the sidebar, the threads under its sections; choosing
  // a thread closes it.
  await page.getByRole("button", { name: "Open navigation" }).click();
  const drawer = page.getByRole("dialog", { name: "Navigation" });
  const sections = drawer.getByRole("navigation", { name: "Main navigation" });
  await expect(sections.getByRole("link")).toHaveCount(4);
  await expect(sections.getByRole("link", { name: "Analyst" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  await expect(
    drawer.getByRole("button", { name: "Close navigation" }),
  ).toBeFocused();
  const threads = drawer.getByRole("region", { name: "Analyst threads" });
  await expect(threads.getByRole("link", { name: "New thread" })).toBeVisible();
  await threads
    .getByRole("link", { name: /^Who signed the services agreement\?/ })
    .click();
  await expect(drawer).toHaveCount(0);
  await expect(page).toHaveURL(/\?thread=q_now$/);
  // A section's link closes it too.
  await page.getByRole("button", { name: "Open navigation" }).click();
  await drawer
    .getByRole("navigation", { name: "Main navigation" })
    .getByRole("link", { name: "Connectors" })
    .click();
  await expect(drawer).toHaveCount(0);
  await expect(
    page.getByRole("heading", { level: 1, name: "Connectors" }),
  ).toBeVisible();
});

test("on a phone, says why the threads couldn't be read, where they would show", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  let failing = true;
  await page.route(
    (url) =>
      url.pathname.endsWith("/ontologies/ont_acme_contracts/questions") &&
      !url.searchParams.has("threadId"),
    (route) =>
      failing
        ? route.fulfill({
            status: 503,
            json: { code: 14, message: "the questions are unavailable" },
          })
        : route.continue(),
  );
  await signIn(page);
  await page.goto("/ask/ont_acme_contracts");
  const threads = page
    .getByRole("main")
    .getByRole("navigation", { name: "Threads" });
  await expect(threads.getByRole("alert")).toContainText(
    "Couldn’t read the threads",
    { timeout: 15_000 },
  );
  // Read again, there are none to list: the new thread shows straight away
  // (nothing is loaded yet, so it offers to load data).
  failing = false;
  await threads.getByRole("button", { name: "Retry" }).click();
  await expect(
    page.getByRole("main").getByRole("link", { name: "Load data" }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Threads" })).toHaveCount(0);
});
