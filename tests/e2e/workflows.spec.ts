import {
  expect,
  makeContractsCurrent,
  loadVersion3,
  mock,
  signIn,
  test,
  uploadAndLoadContract,
} from "./support/mock";

// Workflows: an owner or admin makes one for an agent's task and gives the
// agent its prompt; each run the agent starts (through the Helix MCP, here
// the mock's agent stand-in) shows its request, every read and question it
// made with the run's id, and its answer. A member reads them, and creates
// none.

const O = "/ontology/ont_acme_contracts",
  API = "/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts";

test("an agent's run shows its request, what it read and asked, and its answer", async ({
  page,
}) => {
  await signIn(page);
  await mock.configure({ loadStepMs: 100, askStepMs: 50 });
  await makeContractsCurrent(page);
  const load = await loadVersion3(page);

  // Opened from the ontology's pages.
  await page.goto(O);
  await page
    .getByRole("complementary", { name: "Ontologies" })
    .getByRole("link", { name: "Workflows" })
    .click();
  await expect(
    page.getByRole("heading", { name: "Workflows", level: 1 }),
  ).toBeVisible();
  await expect(page.getByText("No workflows yet")).toBeVisible();

  await page.getByRole("button", { name: "New workflow" }).click();
  const dialog = page.getByRole("dialog", { name: "New workflow" });
  await dialog.getByLabel("Name").fill("Renewal desk");
  await dialog
    .getByLabel("What the agent does (optional)")
    .fill("Answers renewal questions");
  await dialog.getByRole("button", { name: "Create workflow" }).click();
  await expect(page).toHaveURL(/\/workflows\/wf_/);
  const workflow = new URL(page.url()).pathname.split("/").pop()!;
  await expect(
    page.getByRole("heading", { name: "Renewal desk", level: 1 }),
  ).toBeVisible();
  await expect(page.getByText(workflow, { exact: true })).toBeVisible();
  await expect(page.getByText(`workflow_id ${workflow}`)).toBeVisible();
  await expect(page.getByText("No runs yet")).toBeVisible();

  // The agent takes on a request.
  const started = await mock.agent(
    "POST",
    `${API}/workflows/${workflow}/runs`,
    {
      input: "Which customers are up for renewal?",
    },
  );
  expect(started.status).toBe(200);
  const run = started.body.run.id as string;
  const read = await mock.agent("POST", `${API}/loads/${load}:read`, {
    readsJson: JSON.stringify([
      { entity: { type: "Customer", id: "cus_GLOBEX01" } },
    ]),
    workflowRunId: run,
  });
  expect(read.status).toBe(200);
  const asked = await mock.agent("POST", `${API}/questions`, {
    question: "Which customers renew this quarter?",
    clientToken: "renewals-1",
    workflowRunId: run,
  });
  expect(asked.status).toBe(200);
  // A bad run refuses the call before it is made.
  expect(
    (
      await mock.agent("POST", `${API}/loads/${load}:read`, {
        readsJson: "[]",
        workflowRunId: "wfr_nope",
      })
    ).status,
  ).toBe(404);

  // The open run shows on the workflow's page as it goes.
  const runs = page.getByRole("list", { name: "Runs", exact: true });
  // The list asks again every 10 s while no run is open.
  await expect(
    runs.getByText("Which customers are up for renewal?"),
  ).toBeVisible({ timeout: 15_000 });
  await expect(runs.getByText("Open", { exact: true })).toBeVisible();
  await expect(runs.getByText(/^1 read · 1 question/)).toBeVisible();
  await runs
    .getByRole("link", { name: "Which customers are up for renewal?" })
    .click();
  await expect(page).toHaveURL(new RegExp(`/runs/${run}$`));
  const calls = page.getByRole("list", { name: "Recorded calls" });
  await expect(
    calls.getByRole("article", { name: "1. Read entity" }),
  ).toBeVisible();
  await expect(calls.getByText("1 of 1 answered")).toBeVisible();
  await expect(calls.getByText(/^Customer: /)).toBeVisible();
  await expect(
    calls.getByRole("article", { name: "2. Asked a question" }),
  ).toBeVisible();
  await expect(page.getByText("The run is open")).toBeVisible();

  // The agent answers.
  const finished = await mock.agent(
    "POST",
    `${API}/workflows/${workflow}/runs/${run}:finish`,
    {
      output: "Globex renews in March.",
    },
  );
  expect(finished.status).toBe(200);
  await expect(
    page.locator(".wf-text").getByText("Globex renews in March."),
  ).toBeVisible();
  await expect(page.getByText("Finished")).toBeVisible();

  // Its assessment follows: what it read and asked backs its answer.
  await expect(page.getByText("How much the ontology helped")).toBeVisible();
  await expect(page.getByText("Essential", { exact: true })).toBeVisible({
    timeout: 15_000,
  });
  const claims = page.getByRole("region", { name: "Claims" });
  await expect(claims.getByText("Supported")).toBeVisible();
  await expect(claims.getByRole("link", { name: "#1" })).toHaveAttribute(
    "href",
    "#call-1",
  );
  // Back on the workflow's page, the run reads by its verdict, opening to
  // what the assessment found.
  await page.getByRole("link", { name: "Renewal desk" }).click();
  await expect(runs.getByText("Essential", { exact: true })).toBeVisible();
  await runs
    .getByRole("button", { name: "Show what the assessment found" })
    .click();
  await expect(
    runs.getByRole("region", { name: "Claims" }).getByText("Supported"),
  ).toBeVisible();
  await page.goBack();

  // Its question opens in Analyst.
  await calls.getByRole("link", { name: "Open it in Analyst" }).click();
  await expect(page).toHaveURL(
    new RegExp(`/ask/ont_acme_contracts.*${asked.body.question.id}`),
  );

  // Deleting the workflow takes its runs.
  await page.goto(`${O}/workflows/${workflow}`);
  await page.getByRole("button", { name: "Delete" }).click();
  await page
    .getByRole("dialog", { name: "Delete Renewal desk?" })
    .getByRole("button", { name: "Delete workflow" })
    .click();
  await expect(page).toHaveURL(new RegExp(`${O}/workflows$`));
  await expect(page.getByText("No workflows yet")).toBeVisible();
  expect(
    (
      await mock.agent("POST", `${API}/workflows/${workflow}/runs`, {
        input: "again",
      })
    ).status,
  ).toBe(404);
});

test("a member reads workflows and creates none", async ({ page }) => {
  await signIn(page);
  await page.goto(`${O}/workflows`);
  await page.getByRole("button", { name: "New workflow" }).click();
  const dialog = page.getByRole("dialog", { name: "New workflow" });
  await dialog.getByLabel("Name").fill("Support triage");
  await dialog.getByRole("button", { name: "Create workflow" }).click();
  await expect(page).toHaveURL(/\/workflows\/wf_/);
  await page.getByRole("button", { name: "Account" }).click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/login$/);

  await signIn(page, "alan@example.com");
  await page.goto(`${O}/workflows`);
  await expect(
    page.getByRole("link", { name: "Support triage" }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "New workflow" })).toHaveCount(
    0,
  );
  await expect(page.getByText("No workflows yet")).toHaveCount(0);
  await page.getByRole("link", { name: "Support triage" }).click();
  await expect(page.getByRole("button", { name: "Copy prompt" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Delete" })).toHaveCount(0);
});

test("a restricted member uses workflows while their Workflows permission is on", async ({
  page,
  browser,
}) => {
  await signIn(page);
  await mock.configure({ loadStepMs: 100 });
  // Its current load reads Stripe and General, so Eve, holding General, holds part of it.
  await uploadAndLoadContract(page);
  const made = await mock.agent("POST", `${API}/workflows`, {
    name: "Renewal desk",
    clientToken: "renewal-desk",
  });
  expect(made.status).toBe(200);
  const workflow = made.body.workflow.id as string;
  const run = await mock.agent("POST", `${API}/workflows/${workflow}/runs`, {
    input: "Which customers are up for renewal?",
  });
  expect(run.status).toBe(200);

  // Eve, restricted to General, reads part of Customer contracts: she sees
  // its workflows, and only the runs she started, which are none.
  const eve = await (
    await browser.newContext({ viewport: { width: 1440, height: 900 } })
  ).newPage();
  await signIn(eve, "eve@example.com");
  await eve.goto(O);
  await eve
    .getByRole("complementary", { name: "Ontologies" })
    .getByRole("link", { name: "Workflows" })
    .click();
  await eve.getByRole("link", { name: "Renewal desk" }).click();
  await expect(eve.getByRole("heading", { name: "Your runs" })).toBeVisible();
  await expect(
    eve.getByText("you see the runs you started", { exact: false }),
  ).toBeVisible();
  await expect(eve.getByText("No runs yet")).toBeVisible();

  // Given Stripe too, she reads all of it: every run.
  await page.goto("/settings/access?tab=people");
  await page.getByRole("button", { name: "Sources for Eve Adler" }).click();
  let sources = page.getByRole("dialog", { name: "Sources for Eve Adler" });
  await expect(
    sources.getByRole("checkbox", { name: /^Workflows/ }),
  ).toBeChecked();
  await sources.getByRole("checkbox", { name: /^Stripe/ }).check();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved what Eve Adler can read.")).toBeVisible();
  await sources.getByRole("button", { name: "Close" }).first().click();
  await eve.reload();
  await expect(
    eve.getByRole("heading", { name: "Runs", exact: true }),
  ).toBeVisible();
  await expect(
    eve.getByText("Which customers are up for renewal?"),
  ).toBeVisible();

  // Turned off, she has no Workflows, and their pages open the overview.
  await page.getByRole("button", { name: "Sources for Eve Adler" }).click();
  sources = page.getByRole("dialog", { name: "Sources for Eve Adler" });
  await sources.getByRole("checkbox", { name: /^Workflows/ }).uncheck();
  await sources.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved what Eve Adler can read.")).toBeVisible();
  await sources.getByRole("button", { name: "Close" }).first().click();
  await page.getByRole("button", { name: "Sources for Eve Adler" }).click();
  await expect(
    page
      .getByRole("dialog", { name: "Sources for Eve Adler" })
      .getByRole("checkbox", { name: /^Workflows/ }),
  ).not.toBeChecked();
  await eve.goto(`${O}/workflows/${workflow}`);
  await expect(eve).toHaveURL(new RegExp(`${O}$`));
  await expect(
    eve
      .getByRole("complementary", { name: "Ontologies" })
      .getByRole("link", { name: "Workflows" }),
  ).toHaveCount(0);
  expect(
    (await mock.agent("GET", `${API}/workflows/${workflow}/runs`)).status,
  ).toBe(200);
});

test("the overview: the metrics, who ran them, the workflows and what to change", async ({
  page,
}) => {
  await mock.configure({ workflowDemo: true });
  await signIn(page);
  await page.goto(`${O}/workflows`);
  const insights = page.getByRole("region", { name: "Insights" });
  const metrics = insights.getByRole("group", { name: "Metrics" });
  await expect(metrics.getByRole("button")).toHaveCount(4);
  // How often the ontology helped is charted first, over a week a day.
  await expect(
    insights.getByRole("heading", { name: "Ontology helped, by day" }),
  ).toBeVisible();
  await expect(
    metrics.getByRole("button", { name: /^Ontology helped/ }),
  ).toHaveAttribute("aria-pressed", "true");
  const runs = metrics.getByRole("button", { name: /^Runs/ });
  const everyone = await runs.textContent();

  // One who started runs narrows every number.
  await insights.getByRole("combobox", { name: "Started by" }).click();
  await page.getByRole("option", { name: /^Alan Turing/ }).click();
  await expect(runs).not.toHaveText(everyone || "");
  await insights.getByRole("combobox", { name: "Started by" }).click();
  await page.getByRole("option", { name: /^Everyone/ }).click();
  await expect(runs).toHaveText(everyone || "");

  // The runs card charts runs a day by verdict.
  await runs.click();
  await expect(
    insights.getByRole("heading", {
      name: "Runs a day, by how much the ontology helped",
    }),
  ).toBeVisible();

  // The workflows side by side, a row opening one.
  const grid = page.getByRole("grid", { name: "Workflows" });
  await expect(grid.getByRole("row")).toHaveCount(4);

  // Suggestions ticked draft one version with them as its goal.
  const draft = page.getByRole("button", { name: "Draft a version" });
  await expect(draft).toBeDisabled();
  const suggestions = page.getByRole("list", { name: "Suggested changes" });
  await suggestions.getByRole("checkbox").nth(0).check();
  await suggestions.getByRole("checkbox").nth(1).check();
  await expect(page.getByText("2 changes chosen")).toBeVisible();
  await expect(
    page.getByRole("link", { name: "Draft a version" }),
  ).toHaveAttribute("href", /\/generate\?goal=Make\+these\+changes/);
  // A gap opens the run that named it last.
  await page
    .getByRole("list", { name: "Recurring gaps" })
    .getByRole("link")
    .first()
    .click();
  await expect(page).toHaveURL(/\/workflows\/wf_demo_\w+\/runs\/wfr_demo_/);
});

test("a workflow's page: its numbers, its runs and what to change for it", async ({
  page,
}) => {
  await mock.configure({ workflowDemo: true });
  await signIn(page);
  // The overview's misleading count links here, filtered.
  await page.goto(`${O}/workflows/wf_demo_renewals?verdict=misleading`);
  await expect(
    page.getByRole("region", { name: "Numbers" }).getByText("Misleading runs"),
  ).toBeVisible();
  const filter = page.getByRole("radiogroup", { name: "Show runs" });
  await expect(
    filter.getByRole("radio", { name: /^Misleading/ }),
  ).toHaveAttribute("aria-checked", "true");
  const runs = page.getByRole("list", { name: "Runs", exact: true });
  await expect(runs.getByText("Essential", { exact: true })).toHaveCount(0);

  // All of them: ten at first, more on asking.
  await filter.getByRole("radio", { name: /^All/ }).click();
  await expect(page).not.toHaveURL(/verdict=/);
  await expect(runs.getByRole("listitem")).toHaveCount(10);
  await page.getByRole("button", { name: "Show more runs" }).click();
  await expect(runs.getByRole("listitem")).toHaveCount(30);

  // A gap narrows them to the runs that named it.
  await page
    .getByRole("list", { name: "Recurring gaps" })
    .getByRole("button", { name: /^Show its/ })
    .first()
    .click();
  await expect(page.getByText(/^Naming/)).toBeVisible();
  await page.getByRole("button", { name: "Show every run" }).click();
  await expect(page.getByText(/^Naming/)).toHaveCount(0);

  // It connects its agent from a dialog.
  await page.getByRole("button", { name: "Connect agent" }).click();
  await expect(
    page
      .getByRole("dialog", { name: "Connect your agent" })
      .getByText("wf_demo_renewals", { exact: false })
      .first(),
  ).toBeVisible();
});
