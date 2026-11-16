import {
  askAnalyst as ask,
  expect,
  mock,
  signIn,
  test,
  uploadAndLoadContract,
} from "./support/mock";

// The Analyst's answers, as Founder's page shows an Analyst thread from
// before Founder, read only (`?thread=`; Founder asks the Analyst now,
// founder-page.spec.ts): a question about the loaded contract, its steps as
// they stream (each input in words, the query as a readable path, and its
// JSON; a failed call says why, a refused answer says it was corrected, the
// accepted one says Answered), the answer as plain text with citation chips
// (none of the model's markup renders), and chips that open the document at
// the quoted span, the entity, the table row, the derived fact and the rule
// break; its query results as charts and tables. A question the ontology
// can't answer drafts a new version with it. Each question is asked through
// the WFE, as the Analyst's composer did.

test("asks about a load: streamed steps, a plain-text answer, chips that open the span", async ({
  page,
}) => {
  // Slow enough to still be streaming once the thread's page has loaded.
  await mock.configure({ loadStepMs: 200, askStepMs: 400, askMarkup: true });
  await signIn(page);
  await uploadAndLoadContract(page);

  const question = await ask(page, "Who signed the services agreement?");
  // The steps stream in as tool calls: each the tool's name, what it was
  // asked in words and what it returned, opening on its Input and Output.
  // First the engine's read of what the load holds, its summary only (its
  // event carries no result, and its input is empty), so nothing to open;
  // then each query as a path. A lookup the engine can't run yet fails, and
  // says why.
  // Closed, the trace is one line: the step the question is on while it
  // runs; Show the steps opens every step.
  const status = question.getByRole("status");
  await expect(status.locator(".ak-call-tool")).toBeVisible();
  await expect(question.getByRole("list", { name: "Steps" })).toHaveCount(0);
  await question.getByRole("button", { name: "Show the steps" }).click();
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
  await expect(status.locator(".ak-call-tool")).toBeVisible();
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
  await expect(answer).toContainText("Globex Corporation", { timeout: 20_000 });
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
    // A version from its history read, with when it held: a document's day
    // as that day; when it was recorded in UTC.
    /^As recorded .+ UTC · Valid from Oct 1, 2026: the signed_on of Contract Master services agreement$/,
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
  // Ended, its line says how long it worked; the steps stay open until
  // closed.
  await expect(
    question.getByRole("status").filter({ hasText: /^Worked for/ }),
  ).toHaveText(/^Worked for \d+s$/);
  await question.getByRole("button", { name: "Hide the steps" }).click();
  await expect(steps).toHaveCount(0);
  await question.getByRole("button", { name: "Show the steps" }).click();
  await expect(
    question.getByRole("link", { name: "Draft a new version with this" }),
  ).toHaveCount(0);
  await expect(step("Read rule breaks")).toContainText("5 records, more");

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

  // An Analyst thread from before Founder: listed under the sidebar's
  // Analyst threads, marked so, and read only, with the way on to Founder.
  await page.goBack();
  await expect(
    page
      .getByRole("navigation", { name: "Threads" })
      .getByRole("list", { name: "Earlier questions" })
      .getByRole("link", { name: /^Who signed the services agreement\?/ }),
  ).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("form", { name: "Ask a follow-up" })).toHaveCount(
    0,
  );
  await expect(page.getByRole("main")).toContainText(
    "This earlier question is read only. Ask Founder to go on.",
  );
});

test("shows query results after the paragraphs that introduce them, as charts and tables", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 100, askMarkup: true });
  await signIn(page);
  await uploadAndLoadContract(page);
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
  await uploadAndLoadContract(page);
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
  await uploadAndLoadContract(page);
  const question = await ask(page, "Who signed the services agreement?");
  const answer = question.getByRole("region", { name: "Answer" });
  await expect(answer).toContainText("of the 65 invoices are", {
    timeout: 15_000,
  });
  await expect(answer.getByRole("figure")).toHaveCount(0);
  await question.getByRole("button", { name: "Show the steps" }).click();
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
  await uploadAndLoadContract(page);
  const question = await ask(
    page,
    "Who signed each contract for the customer?",
  );
  await expect(question).toContainText("This ontology can’t answer that", {
    timeout: 10_000,
  });
  // Its kind (4.7.0) is a chip before its reason.
  await expect(question.locator(".hf-tag")).toHaveText(
    "Relationship not defined",
  );
  await expect(question).toContainText("It lacks: A signed_by link");
  // The ending call shows as the callout, not its input.
  await expect(
    question.getByRole("status").filter({ hasText: /^Worked for/ }),
  ).toHaveText(/^Worked for \d+s · Couldn’t answer$/);
  await question.getByRole("button", { name: "Show the steps" }).click();
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

// An interval with an end, and a malformed one, injected at the browser
// boundary: the mock's versions have neither.
test("shows valid time beside a citation's recording time", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 100 });
  const recorded = (marker: string, valid: unknown) => ({
    marker,
    evidence_id: "e1",
    target: {
      version: {
        tx: "opaque-tx",
        at: "2026-10-03T09:00:00Z",
        of: { record: { constraint: "required", key: "record" } },
        valid,
      },
    },
  });
  await page.route(
    (url) =>
      /\/ontologies\/ont_acme_contracts\/questions\/[^/:]+$/.test(url.pathname),
    async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      if (body.question?.resultJson) {
        const result = JSON.parse(body.question.resultJson);
        if (result.status === "answered") {
          result.citations = [
            recorded("c1", {
              from: "2026-10-01T00:00:00Z",
              to: "2026-10-02T00:00:00Z",
            }),
            recorded("c2", { from: "2026-10-01", day: "October" }),
          ];
          result.answer = {
            blocks: [{ para: "The recorded state [c1], and before [c2]" }],
          };
          body.question.resultJson = JSON.stringify(result);
        }
      }
      await route.fulfill({ response, json: body });
    },
  );
  await signIn(page);
  await uploadAndLoadContract(page);
  const question = await ask(page, "What state was recorded?");
  // In UTC whatever the reader's zone; a malformed interval loses only its
  // text, never the citation.
  await expect(
    question.getByRole("list", { name: "Sources" }).getByRole("listitem"),
  ).toHaveText([
    /^As recorded Oct 3, 2026, 09:00 AM UTC · Valid from Oct 1, 2026, 12:00 AM UTC until Oct 2, 2026, 12:00 AM UTC: a break of required$/,
    /^As recorded Oct 3, 2026, 09:00 AM UTC: a break of required$/,
  ]);
  await expect(
    question.getByRole("button", {
      name: /Source 1: As recorded .* · Valid from .* until/,
    }),
  ).toBeVisible();
});

test("labels a gap's kind and keeps its reason and draft action", async ({
  page,
}) => {
  await mock.configure({
    loadStepMs: 200,
    askStepMs: 100,
    askOutcome: "cannot_answer",
  });
  await page.route(
    (url) =>
      /\/ontologies\/ont_acme_contracts\/questions(?:\/[^/:]+)?$/.test(
        url.pathname,
      ),
    async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      // Declines render from the thread list; answered questions also read
      // their full result from GET of one question.
      for (const question of body.questions ??
        (body.question ? [body.question] : [])) {
        if (!question.resultJson) continue;
        const result = JSON.parse(question.resultJson);
        if (result.status === "cannot_answer") result.kind = "uncovered";
        question.resultJson = JSON.stringify(result);
      }
      await route.fulfill({ response, json: body });
    },
  );
  await signIn(page);
  await uploadAndLoadContract(page);
  const question = await ask(page, "What was true before this was recorded?");
  await expect(question).toContainText("This ontology can’t answer that");
  await expect(question.locator(".hf-tag")).toHaveText(
    "No recorded state covers that time",
  );
  await expect(question).toContainText("It lacks: A signed_by link");
  await expect(
    question.getByRole("link", { name: "Draft a new version with this" }),
  ).toBeVisible();
});
