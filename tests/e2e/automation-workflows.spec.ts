import type { Page } from "@playwright/test";
import {
  csrfToken,
  expect,
  loadVersion3,
  makeContractsCurrent,
  mock,
  postToWfe,
  signIn,
  test,
} from "./support/mock";

// How much an ontology helps an automation: Foundry makes a workflow of
// each ontology an automation's runs use, the first time one does, and
// records every run that uses it there, assessed as any workflow run is.
// The automation's page says how much each helped, a run's page each
// ontology's verdict for that run, and the workflow links back to its
// automation, which only its deletion lets go of.

const A = "/v1/workspaces/ws_acme/foundry/automations",
  W = "/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts/workflows";
const at = (y: number) => ({ x: 0, y });
const manual = {
  id: "start",
  kind: "trigger",
  config: { kind: "manual" },
  position: at(0),
};

/** Makes Ada's automation of one Assistant step, approved: its id. */
async function make(page: Page, name: string, instruction: string) {
  const made = (
    await postToWfe(page, A, {
      name,
      clientToken: `e2e-${name}`,
      specJson: JSON.stringify({
        v: 1,
        nodes: [
          manual,
          {
            id: "ask",
            kind: "assistant",
            config: { instruction },
            position: at(150),
          },
        ],
        edges: [{ from: "start", to: "ask" }],
      }),
    })
  ).automation;
  await postToWfe(page, `${A}/${made.id}:approve`, {
    expectedRevision: made.revision,
  });
  return made.id as string;
}

/** Runs it by hand and opens the run's page once it ended: the run's id. */
async function runOnce(page: Page, id: string, n: number) {
  const run = (
    await postToWfe(page, `${A}/${id}:run`, { clientToken: `run-${id}-${n}` })
  ).run;
  await page.goto(`/automations/${id}/runs/${run.id}`);
  await expect(
    page
      .getByRole("complementary", { name: "Inspector" })
      .locator(".au-summary-head")
      .getByText("Succeeded", { exact: true }),
  ).toBeVisible({ timeout: 20_000 });
  return run.id as string;
}

/** The ontology's workflows, as the site reads them. */
async function workflows(page: Page) {
  const res = await page.request.get(`/papi${W}`);
  expect(res.status()).toBe(200);
  return ((await res.json()).workflows || []) as {
    id: string;
    name: string;
    runs: string;
    automation?: { automationId: string; deleted?: boolean };
  }[];
}

test("an automation that reads an ontology records each run in one workflow, which says how much it helped", async ({
  page,
}) => {
  await signIn(page);
  await mock.configure({
    loadStepMs: 100,
    automationStepMs: 100,
    agentStepMs: 40,
    assessMs: 300,
  });
  await makeContractsCurrent(page);
  await loadVersion3(page);
  const id = await make(
    page,
    "Invoice check",
    "How many invoices does Customer contracts hold?",
  );
  // Its step names no ontology to act on: nothing is made before it runs.
  expect((await workflows(page)).some((w) => w.automation)).toBe(false);

  // Its first run reads the ontology: the run's page says how much it helped.
  const first = await runOnce(page, id, 1);
  const helped = page.getByRole("region", {
    name: "How much each ontology helped",
  });
  await expect(
    helped.getByRole("link", { name: "Customer contracts: its run" }),
  ).toBeVisible();
  await expect(helped.getByText("Helpful", { exact: true })).toBeVisible({
    timeout: 15_000,
  });
  const made = (await workflows(page)).filter((w) => w.automation);
  expect(made).toHaveLength(1);
  expect(made[0]!.name).toBe("Invoice check");
  expect(made[0]!.automation!.automationId).toBe(id);

  // Its workflow run says where it comes from, and links back.
  await helped
    .getByRole("link", { name: "Customer contracts: its run" })
    .click();
  await expect(page).toHaveURL(
    new RegExp(`/ontology/ont_acme_contracts/workflows/${made[0]!.id}/runs/`),
  );
  const origin = page
    .getByRole("note")
    .filter({ hasText: "Made by the automation" });
  await expect(origin).toContainText("Invoice check");
  await expect(
    page.getByRole("list", { name: "Recorded calls" }).getByRole("article"),
  ).toHaveCount(1);
  await expect(
    page.getByText(
      'Run by hand. The automation "Invoice check" ran these steps, in order:',
      { exact: false },
    ),
  ).toBeVisible();
  // Its id, shown here, records nothing for anyone but the automation: not
  // even its owner's own calls.
  const workflowRun = page.url();
  const forged = await page.request.post(
    `/papi/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts/questions`,
    {
      headers: {
        origin: new URL(page.url()).origin,
        "x-helix-csrf": await csrfToken(page),
      },
      data: {
        question: "Who renews?",
        clientToken: "forged-1",
        workflowRunId: workflowRun.split("/runs/")[1],
      },
    },
  );
  expect(forged.status()).toBe(404);
  await origin.getByRole("link", { name: "one of its runs" }).click();
  await expect(page).toHaveURL(new RegExp(`/automations/${id}/runs/${first}$`));

  // A second run reuses the workflow, with a run of its own.
  await runOnce(page, id, 2);
  await expect(
    page
      .getByRole("region", { name: "How much each ontology helped" })
      .getByText("Helpful", { exact: true }),
  ).toBeVisible({ timeout: 15_000 });
  const again = (await workflows(page)).filter((w) => w.automation);
  expect(again).toHaveLength(1);
  expect(again[0]!.runs).toBe("2");

  // The automation's page says how much each ontology helped, overall.
  await page.goto(`/automations/${id}`);
  const impact = page.getByRole("region", {
    name: "How much its ontologies helped",
  });
  await expect(impact).toContainText("Helped 2 of the 2 runs assessed.", {
    timeout: 15_000,
  });
  await expect(impact.getByRole("img", { name: "2 helpful" })).toBeVisible();
  await impact
    .getByRole("link", { name: "Customer contracts: its workflow" })
    .click();
  await expect(page).toHaveURL(
    new RegExp(`/ontology/ont_acme_contracts/workflows/${made[0]!.id}$`),
  );
  await expect(
    page.getByRole("note").filter({ hasText: "Made by the automation" }),
  ).toContainText("It goes only once the automation is deleted.");
  // Only Foundry starts its runs, and it isn't deleted while its automation is.
  await expect(page.getByRole("button", { name: "Delete" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Connect agent" })).toHaveCount(
    0,
  );
  const runs = page.getByRole("list", { name: "Runs", exact: true });
  await expect(runs.getByText(/Automation: Invoice check/)).toHaveCount(2);
  const refused = await page.request.delete(`/papi${W}/${made[0]!.id}`, {
    headers: {
      origin: new URL(page.url()).origin,
      "x-helix-csrf": await csrfToken(page),
    },
  });
  expect(refused.status()).toBe(400);
  expect((await refused.json()).message).toContain(
    "only once the automation is deleted",
  );
  const started = await mock.agent("POST", `${W}/${made[0]!.id}/runs`, {
    input: "Mine",
  });
  expect(started.status).toBe(400);

  // Insights count the automation's runs as its own.
  await page.goto("/ontology/ont_acme_contracts/workflows");
  const insights = page.getByRole("region", { name: "Insights" });
  await insights.getByRole("combobox", { name: "Started by" }).click();
  await page
    .getByRole("option", { name: /^Automation: Invoice check/ })
    .click();
  await expect(
    insights.getByRole("combobox", { name: "Started by" }),
  ).toContainText("Automation: Invoice check");
  await expect(
    insights.getByRole("group", { name: "Metrics" }).getByRole("button", {
      name: /^Runs/,
    }),
  ).toContainText("2");

  // Its workflow keeps a run's record after the automation lets the run go.
  await mock.pruneRun(id, first);
  await page.goto(workflowRun);
  await expect(
    page.getByRole("note").filter({ hasText: "Made by the automation" }),
  ).toContainText(
    "this run records one of its runs, which the automation no longer keeps.",
  );
  await expect(page.getByRole("link", { name: "one of its runs" })).toHaveCount(
    0,
  );

  // Deleting the automation keeps its workflow and runs, as history.
  const res = await page.request.delete(`/papi${A}/${id}`, {
    headers: {
      origin: new URL(page.url()).origin,
      "x-helix-csrf": await csrfToken(page),
    },
  });
  expect(res.status()).toBe(200);
  await page.goto(`/ontology/ont_acme_contracts/workflows/${made[0]!.id}`);
  await expect(
    page.getByRole("note").filter({ hasText: "Made by a deleted automation" }),
  ).toContainText("it keeps the runs it recorded as history.");
  await expect(runs.getByRole("listitem")).toHaveCount(2);
  await expect(page.getByRole("button", { name: "Delete" })).toBeVisible();
});

test("a run that uses no ontology records none", async ({ page }) => {
  await signIn(page);
  await mock.configure({ automationStepMs: 100, agentStepMs: 40 });
  const id = await make(page, "Hello", "Say hello to the team");
  await runOnce(page, id, 1);
  await expect(
    page.getByRole("region", { name: "How much each ontology helped" }),
  ).toHaveCount(0);
  expect((await workflows(page)).some((w) => w.automation)).toBe(false);
  await page.goto(`/automations/${id}`);
  await expect(
    page.getByRole("region", { name: "How much its ontologies helped" }),
  ).toContainText("None yet");
});
