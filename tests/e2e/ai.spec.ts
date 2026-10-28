import type { Page } from "@playwright/test";
import {
  choose,
  createdOntology,
  expect,
  finishOntology,
  mock,
  signIn,
  signInToNewWorkspace,
  startOntology,
  test,
} from "./support/mock";

// Settings › AI models: Acme runs on the default
// until its owner brings a Claude key, which is tested before Foundry uses
// it; a key that fails its test leaves the default running; a blank key
// keeps the saved one for Claude; Alan, a member, reads where AI runs; and
// the owner goes back to the default. OpenAI runs on Helix's prices or
// the owner's for a model outside them; Azure OpenAI on a resource's
// deployments, its key kept only for that resource and draft deployment; a
// custom OpenAI-compatible server only on https and a public address, its
// optional key kept only for its base URL. Amazon Bedrock runs in a region,
// FIPS where AWS has it, on model or inference profile IDs priced from
// Helix's table (a profile as its model) or by hand, its key kept across
// regions and models. A new workspace on Free needs Foundry Pro to bring a
// key; Ollama is never offered. The mock's test passes unless the key holds
// "bad" or a model "missing" or "notools"; its DNS makes ".internal" names
// private (mock/routes/ai.ts).

const O = "/ontology/ont_acme_contracts";
const KEY = "sk-ant-api03-e2e-0123456789abcd";

// The owner's form, whose choice is what runs now.
const setting = (page: Page) =>
  page.locator(".hf-panel").filter({
    has: page.getByRole("heading", { name: "Choose where AI runs" }),
  });
// Whether the form holds `text`, in its words or a field's value (models a
// field names, such as a deployment, are typed in).
const holds = (page: Page, text: string) =>
  expect
    .poll(async () => {
      const form = setting(page);
      const values = await form
        .locator("input")
        .evaluateAll((els) => els.map((e) => (e as HTMLInputElement).value));
      return [await form.innerText(), ...values].join("\n");
    })
    .toContain(text);
const radio = (page: Page, name: string) =>
  page.getByRole("radio", { name, exact: true });

/** Chooses Claude with `key`, every task on `model`, and tests and saves it. */
async function bringClaude(page: Page, key: string, model?: string) {
  await radio(page, "Claude").check();
  if (model)
    await choose(
      page.getByRole("combobox", { name: "Model" }),
      new RegExp(`^${model}`),
    );
  await page.getByLabel("API key").fill(key);
  await page.getByRole("button", { name: "Test and save" }).click();
  const confirm = page.getByRole("dialog", { name: "Change where AI runs?" });
  await expect(confirm).toContainText(
    "New drafts, document reads and questions run on Claude, with your API key.",
  );
  await confirm.getByRole("button", { name: "Test and save" }).click();
}

test.beforeEach(async () => {
  await mock.configure({ aiProbeMs: 600 });
});

test("runs on the default", async ({ page }) => {
  await signIn(page);
  await page.goto("/settings");
  await page.getByRole("tab", { name: "AI models" }).click();
  await expect(page).toHaveURL(/\/settings\/ai$/);
  await expect(radio(page, "Default")).toBeChecked();
  await expect(
    page.getByText(
      "Runs on hosted providers through us, pay for what you use.",
    ),
  ).toHaveCount(1);
  await expect(
    page.getByText("Sent to Anthropic under your API key."),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Use the default" }),
  ).toBeDisabled();
  await expect(page.getByRole("heading", { name: "What is sent" })).toHaveCount(
    0,
  );
});

// A native Ollama server takes no key, so a hosted one would have to be
// public: the site never offers it hosted, even where the WFE would.
test("never offers Ollama", async ({ page }) => {
  await mock.configure({
    aiKinds: [
      "FOUNDRY_AI_KIND_ANTHROPIC",
      "FOUNDRY_AI_KIND_OPENAI",
      "FOUNDRY_AI_KIND_AZURE_OPENAI",
      "FOUNDRY_AI_KIND_OLLAMA",
    ],
  });
  await signIn(page);
  await page.goto("/settings/ai");
  const radios = page
    .getByRole("group", { name: "Where AI runs" })
    .getByRole("radio");
  await expect(radios).toHaveCount(4);
  for (const name of ["Default", "Claude", "OpenAI", "Azure OpenAI"])
    await expect(radio(page, name)).toBeVisible();
  await expect(radio(page, "Ollama")).toHaveCount(0);
  await expect(page.getByText(/Ollama/)).toHaveCount(0);
});

test("runs on the workspace's Claude key once its test passes", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 200 });
  await signIn(page);
  await page.goto("/settings/ai");
  await radio(page, "Claude").check();
  // Recommended: a model per task, as the default runs them.
  await expect(
    page.getByText("Drafting: claude-opus-5-5 · Questions:"),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Test and save" }),
  ).toBeDisabled();
  const put = page.waitForRequest(
    (r) => r.method() === "PUT" && r.url().endsWith("/foundry/ai"),
  );
  await bringClaude(page, KEY, "claude-sonnet-5-5");
  expect((await put).postDataJSON()).toEqual({
    provider: { anthropic: {} },
    models: {
      draft: { name: "claude-sonnet-5-5", table: {} },
      ask: { name: "claude-sonnet-5-5", table: {} },
      extract: { name: "claude-sonnet-5-5", table: {} },
      verify: { name: "claude-sonnet-5-5", table: {} },
    },
    apiKey: KEY,
  });
  await expect(
    page.getByRole("status").filter({ hasText: "Testing Claude:" }),
  ).toBeVisible();
  await expect(page.getByText("AI models now run on Claude.")).toBeVisible();
  await expect(radio(page, "Claude")).toBeChecked();
  // The field keeps the key typed until another page; the key is never
  // shown again, nor answered.
  await expect(page.getByLabel("API key")).toHaveValue(KEY);
  await page.reload();
  await expect(page.getByLabel("API key")).toHaveValue("");
  await expect(page.getByLabel("API key")).toHaveAttribute(
    "placeholder",
    "Leave blank to keep sk-ant…abcd",
  );
  const read = await page.request.get("/papi/v1/workspaces/ws_acme/foundry/ai");
  expect(await read.text()).not.toContain(KEY);

  // A new draft runs on it.
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(page).toHaveURL(/\/generations\/gen_/);
  await expect(page.getByText(/^Drafted version \d+$/)).toBeVisible({
    timeout: 15_000,
  });
  await expect(
    page.getByText("claude-sonnet-5-5", { exact: true }),
  ).toBeVisible();
});

test("keeps the default when a key fails its test, after a reload too", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/settings/ai");
  await bringClaude(page, "sk-ant-bad-0123456789abcd");
  const failed = page.getByRole("alert").filter({ hasText: "refused" });
  await expect(failed).toHaveText(
    "Claude refused the API key. Your current settings stay active.",
  );
  // The key typed stays, to correct or test again.
  await expect(radio(page, "Claude")).toBeChecked();
  await expect(page.getByLabel("API key")).toHaveValue(
    "sk-ant-bad-0123456789abcd",
  );
  await page.reload();
  await expect(radio(page, "Default")).toBeChecked();
  await expect(failed).toBeVisible();
  await expect(radio(page, "Default")).toBeChecked();
});

test("keeps the saved key when Claude's models change, and goes back to the default", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/settings/ai");
  await bringClaude(page, KEY, "claude-sonnet-5-5");
  await expect(page.getByText("AI models now run on Claude.")).toBeVisible();

  // Opened again, the form starts from the setting: Claude, its model, its
  // key kept, the field blank.
  await page.reload();
  await expect(radio(page, "Claude")).toBeChecked();
  const key = page.getByLabel("API key");
  await expect(key).toHaveValue("");
  await expect(key).toHaveAttribute(
    "placeholder",
    "Leave blank to keep sk-ant…abcd",
  );
  await choose(
    page.getByRole("combobox", { name: "Model" }),
    /^claude-haiku-5-5/,
  );
  const put = page.waitForRequest(
    (r) => r.method() === "PUT" && r.url().endsWith("/foundry/ai"),
  );
  await page.getByRole("button", { name: "Test and save" }).click();
  // Other reading models read documents again; the provider stays.
  const confirm = page.getByRole("dialog", { name: "Change where AI runs?" });
  await expect(confirm).toContainText(
    "Documents are read again at your cost on their next load.",
  );
  await expect(confirm).not.toContainText("run on");
  await confirm.getByRole("button", { name: "Test and save" }).click();
  expect((await put).postDataJSON().apiKey).toBeUndefined();
  await expect(page.getByText("AI models now run on Claude.")).toBeVisible();
  await expect(setting(page)).not.toContainText("claude-sonnet-5-5");
  await holds(page, "claude-haiku-5-5");
  await expect(page.getByLabel("API key")).toHaveAttribute(
    "placeholder",
    "Leave blank to keep sk-ant…abcd",
  );

  // Per task: drafting on Opus, the rest as they are.
  await page.getByLabel("Choose a model per task").check();
  await choose(
    page.getByRole("combobox", { name: "Drafting" }),
    /^claude-opus-5-5/,
  );
  await expect(page.getByRole("combobox", { name: "Model" })).toHaveCount(0);
  await page.getByLabel("Choose a model per task").uncheck();
  await expect(page.getByRole("combobox", { name: "Model" })).toHaveText(
    "Recommended for each task",
  );

  // Back to the default, at once.
  await radio(page, "Default").check();
  await page.getByRole("button", { name: "Use the default" }).click();
  const back = page.getByRole("dialog", { name: "Change where AI runs?" });
  await expect(back).toContainText(
    "New drafts, document reads and questions run on the default.",
  );
  await back.getByRole("button", { name: "Switch" }).click();
  await expect(
    page.getByText("AI models now run on the default."),
  ).toBeVisible();
  await expect(radio(page, "Default")).toBeChecked();
  // The key went with the setting: Claude asks for one again.
  await radio(page, "Claude").check();
  await expect(page.getByLabel("API key")).toHaveAttribute(
    "placeholder",
    "sk-ant-…",
  );
});

test("links a draft's key failure to the AI settings", async ({ page }) => {
  await signIn(page);
  await page.goto("/settings/ai");
  await bringClaude(page, KEY);
  await expect(page.getByText("AI models now run on Claude.")).toBeVisible();
  await mock.configure({ ontologyStepMs: 200, ontologyOutcome: "auth" });
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(page.getByText("The draft failed")).toBeVisible({
    timeout: 15_000,
  });
  await expect(
    page.getByText(
      "Claude refused the API key. An owner can enter a new one in Settings › AI models.",
    ),
  ).toBeVisible();
  await page.getByRole("link", { name: "Settings › AI models" }).click();
  await expect(page).toHaveURL(/\/settings\/ai$/);
});

test("links a first draft's key failure on the new ontology's overview too", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/settings/ai");
  await bringClaude(page, KEY);
  await expect(page.getByText("AI models now run on Claude.")).toBeVisible();
  await mock.configure({ ontologyStepMs: 200, ontologyOutcome: "auth" });
  await startOntology(page, "Orders");
  await page.getByRole("radio", { name: "orders in Production" }).check();
  await finishOntology(page);
  const overview = await createdOntology(page);
  await expect(page.getByText("The draft failed")).toBeVisible({
    timeout: 15_000,
  });
  // The overview's setup says it in the WFE's words, once, and links them.
  await page.goto(overview);
  const failed = page
    .getByRole("alert")
    .filter({ hasText: "The draft failed" });
  await expect(failed.locator("p")).toHaveText(
    "Claude refused the API key. An owner can enter a new one in Settings › AI models.",
  );
  await failed.getByRole("link", { name: "Settings › AI models" }).click();
  await expect(page).toHaveURL(/\/settings\/ai$/);
});

test("shows a member where AI runs, with nothing to change", async ({
  page,
}) => {
  await signIn(page, "alan@example.com");
  await page.goto("/settings/ai");
  await expect(
    page.getByText(
      "AI runs on the default (claude-opus-5-5). Only owners can change this.",
    ),
  ).toBeVisible();
  await expect(page.getByRole("radio")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Test and save" })).toHaveCount(
    0,
  );
});

test("refuses drafts on the workspace's key once it leaves Pro, until it goes back to the default", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 200 });
  await signIn(page);
  await page.goto("/settings/ai");
  await bringClaude(page, KEY);
  await expect(page.getByText("AI models now run on Claude.")).toBeVisible();

  // Pro is cancelled, and its period ends.
  await page.goto("/settings/billing");
  await page.getByRole("button", { name: "Cancel Pro" }).click();
  const cancel = page.getByRole("dialog", { name: "Cancel Foundry Pro?" });
  await cancel.getByRole("button", { name: "Cancel Pro" }).click();
  await expect(cancel).toBeHidden();
  await expect(
    page.getByRole("heading", { name: /Foundry Pro/ }),
  ).toContainText("Ending");
  await mock.configure({ foundryPeriodEnded: true });

  // The key stays the setting, paused: its work never moves to the
  // default unasked.
  await page.goto("/settings/ai");
  await expect(radio(page, "Claude")).toBeChecked();
  await expect(setting(page).getByRole("alert")).toHaveText(
    "Paused: this workspace is off Foundry Pro, so its drafts, document reads and questions are refused until it is on Pro again or an owner chooses Default.",
  );

  // A draft is refused, saying what it needs.
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  const gate = page.getByRole("note").filter({
    hasText: "Bringing your own model key needs Foundry Pro",
  });
  await expect(
    gate.getByRole("link", { name: "Upgrade to Pro" }),
  ).toHaveAttribute("href", "/settings/billing");
  await expect(page).toHaveURL(/\/generate$/);

  // The default is in every plan: back on it, the draft runs.
  await page.goto("/settings/ai");
  await radio(page, "Default").check();
  await page.getByRole("button", { name: "Use the default" }).click();
  await page
    .getByRole("dialog", { name: "Change where AI runs?" })
    .getByRole("button", { name: "Switch" })
    .click();
  await expect(
    page.getByText("AI models now run on the default."),
  ).toBeVisible();
  await expect(setting(page).getByRole("alert")).toHaveCount(0);
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(page).toHaveURL(/\/generations\/gen_/);
});

test("needs Foundry Pro to bring a key on Free", async ({ page }) => {
  await signInToNewWorkspace(page);
  await page.goto("/settings/ai");
  await expect(radio(page, "Default")).toBeChecked();
  for (const card of [
    "Claude",
    "OpenAI",
    "Azure OpenAI",
    "Custom (OpenAI-compatible)",
    "Amazon Bedrock",
  ]) {
    await radio(page, card).check();
    const gate = page.getByRole("note").filter({
      hasText: "Bringing your own model key needs Foundry Pro",
    });
    await expect(gate).toContainText("Default stays available on every plan.");
    await expect(
      gate.getByRole("link", { name: "Upgrade to Pro" }),
    ).toHaveAttribute("href", "/settings/billing");
    await expect(page.getByLabel("API key")).toHaveCount(0);
    await expect(page.getByLabel("Endpoint")).toHaveCount(0);
    await expect(page.getByLabel("Base URL")).toHaveCount(0);
    await expect(page.getByRole("combobox", { name: "Region" })).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: "Test and save" }),
    ).toBeDisabled();
  }
});

/** Confirms a switch to `provider` with your key, and waits for its test. */
async function confirmSwitch(page: Page, provider: string) {
  await page.getByRole("button", { name: "Test and save" }).click();
  const confirm = page.getByRole("dialog", { name: "Change where AI runs?" });
  await expect(confirm).toContainText(
    `New drafts, document reads and questions run on ${provider}, with your API key.`,
  );
  await confirm.getByRole("button", { name: "Test and save" }).click();
}

const OPENAI_KEY = "sk-proj-e2e-0123456789abcdefwxyz";
const AZURE_KEY = "0123456789abcdef0123456789abcdef";

test("runs on OpenAI, a task on a model priced by hand", async ({ page }) => {
  await mock.configure({ ontologyStepMs: 200 });
  await signIn(page);
  await page.goto("/settings/ai");
  await expect(
    page.getByText(
      "Sent to OpenAI under your API key, with response storage off.",
    ),
  ).toBeVisible();
  await radio(page, "OpenAI").check();
  await expect(page.getByLabel("API key")).toHaveAttribute(
    "placeholder",
    "sk-…",
  );
  // Recommended: the WFE's suggestions, from Helix's price table.
  await expect(
    page.getByText("Drafting: gpt-6.1-sol · Questions: gpt-6.1-sol"),
  ).toBeVisible();

  // Drafting on a model Helix has no price for, at the owner's rates.
  await page.getByLabel("Choose a model per task").check();
  const drafting = page.getByRole("group", { name: "Drafting" });
  await choose(drafting.getByRole("combobox"), /^Another model/);
  await drafting.getByLabel("Model name").fill("gpt-7-preview");
  await drafting.getByLabel("Input", { exact: true }).fill("1.25");
  await drafting.getByLabel("Output", { exact: true }).fill("0");
  await page.getByLabel("API key").fill(OPENAI_KEY);
  await expect(
    drafting.getByText("The output price must be more than $0."),
  ).toBeVisible();
  const save = page.getByRole("button", { name: "Test and save" });
  await expect(save).toBeDisabled();
  await drafting.getByLabel("Output", { exact: true }).fill("10.5");
  await drafting.getByLabel("Model name").fill("sk-proj-oops");
  await expect(
    drafting.getByText(
      "That looks like an API key; enter it in the API key field.",
    ),
  ).toBeVisible();
  await expect(save).toBeDisabled();
  await drafting.getByLabel("Model name").fill("gpt-7-preview");

  const put = page.waitForRequest(
    (r) => r.method() === "PUT" && r.url().endsWith("/foundry/ai"),
  );
  await confirmSwitch(page, "OpenAI");
  const table = { name: "gpt-6.1-sol", table: {} };
  expect((await put).postDataJSON()).toEqual({
    provider: { openai: {} },
    models: {
      draft: {
        name: "gpt-7-preview",
        rates: { inputUsdPerMtok: "1.25", outputUsdPerMtok: "10.5" },
      },
      ask: table,
      extract: { name: "gpt-6-luna", table: {} },
      verify: table,
    },
    apiKey: OPENAI_KEY,
  });
  await expect(
    page.getByRole("status").filter({ hasText: "Testing OpenAI:" }),
  ).toBeVisible();
  await expect(page.getByText("AI models now run on OpenAI.")).toBeVisible();
  await expect(radio(page, "OpenAI")).toBeChecked();
  await holds(
    page,
    "Sent to OpenAI under your API key, with response storage off.",
  );
  await holds(page, "gpt-7-preview");
  await expect(page.getByLabel("API key")).toHaveAttribute(
    "placeholder",
    "Leave blank to keep sk-proj…wxyz",
  );
  // The form starts from it, its prices as saved; the key is kept.
  await expect(page.getByLabel("Choose a model per task")).toBeChecked();
  await expect(drafting.getByLabel("Output", { exact: true })).toHaveValue(
    "10.5",
  );
  await expect(page.getByLabel("API key")).toHaveAttribute(
    "placeholder",
    "Leave blank to keep sk-proj…wxyz",
  );

  // A new draft runs on it.
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(page).toHaveURL(/\/generations\/gen_/);
  await expect(page.getByText(/^Drafted version \d+$/)).toBeVisible({
    timeout: 15_000,
  });
  await expect(page.getByText("gpt-7-preview", { exact: true })).toBeVisible();
});

test("runs on an Azure OpenAI resource's deployment, and asks for the key again for another", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/settings/ai");
  await radio(page, "Azure OpenAI").check();
  const card = page.locator(".choice-card").filter({ hasText: "Azure OpenAI" });
  await expect(card).toContainText("Sent to your Azure OpenAI resource.");
  const endpoint = page.getByLabel("Endpoint");
  const save = page.getByRole("button", { name: "Test and save" });
  for (const bad of [
    "http://acme.openai.azure.com",
    "https://acme.example.com",
    "https://acme.openai.azure.com/openai/deployments/x",
  ]) {
    await endpoint.fill(bad);
    await expect(
      page.getByText(
        "Use the resource’s endpoint, like https://<resource>.openai.azure.com.",
      ),
    ).toBeVisible();
  }
  await endpoint.fill("https://acme.openai.azure.com");
  await expect(card).toContainText(
    "Sent to your Azure OpenAI resource at acme.openai.azure.com.",
  );
  await expect(save).toBeDisabled();
  await page.getByLabel("Deployment", { exact: true }).fill("prod-gpt");
  // Priced as the OpenAI model it runs, from Helix's table: its first row
  // until another is chosen.
  const pricedAs = page.getByRole("combobox", { name: "Priced as" });
  await expect(pricedAs).toHaveText(/^gpt-6-astra/);
  await choose(pricedAs, /^gpt-6\.1-sol/);
  await expect(
    page.getByText(
      "The model the deployment runs, for Helix’s prices: $2 in, $10 out per million tokens.",
    ),
  ).toBeVisible();
  await expect(page.getByLabel("API key")).toHaveAttribute(
    "placeholder",
    "The resource’s key 1 or key 2",
  );
  await page.getByLabel("API key").fill(AZURE_KEY);
  const put = page.waitForRequest(
    (r) => r.method() === "PUT" && r.url().endsWith("/foundry/ai"),
  );
  await confirmSwitch(page, "Azure OpenAI");
  const deployment = { name: "prod-gpt", tableAs: "gpt-6.1-sol" };
  expect((await put).postDataJSON()).toEqual({
    provider: {
      azureOpenai: { endpoint: "https://acme.openai.azure.com/openai/v1" },
    },
    models: {
      draft: deployment,
      ask: deployment,
      extract: deployment,
      verify: deployment,
    },
    apiKey: AZURE_KEY,
  });
  await expect(
    page.getByText("AI models now run on Azure OpenAI."),
  ).toBeVisible();
  await expect(radio(page, "Azure OpenAI")).toBeChecked();
  await holds(
    page,
    "Sent to your Azure OpenAI resource at acme.openai.azure.com.",
  );
  await holds(page, "prod-gpt");
  await expect(page.getByLabel("API key")).toHaveAttribute(
    "placeholder",
    "Leave blank to keep …cdef",
  );

  // The key belongs to the resource and draft deployment: kept for its
  // own prices, asked for again for another deployment or resource.
  const key = page.getByLabel("API key");
  await expect(endpoint).toHaveValue("https://acme.openai.azure.com/openai/v1");
  await expect(key).toHaveAttribute("placeholder", "Leave blank to keep …cdef");
  await choose(
    page.getByRole("combobox", { name: "Priced as" }),
    /^Enter prices/,
  );
  await page.getByLabel("Input", { exact: true }).fill("2.5");
  await page.getByLabel("Output", { exact: true }).fill("10");
  await expect(key).toHaveAttribute("placeholder", "Leave blank to keep …cdef");
  await page.getByLabel("Deployment", { exact: true }).fill("other-gpt");
  await expect(key).toHaveAttribute(
    "placeholder",
    "The resource’s key 1 or key 2",
  );
  await expect(save).toBeDisabled();
  await page.getByLabel("Deployment", { exact: true }).fill("prod-gpt");
  await key.fill("typed-for-acme");
  await endpoint.fill("https://globex.openai.azure.com");
  // A key typed for one resource isn't sent to another.
  await expect(key).toHaveValue("");
  await expect(save).toBeDisabled();
  // Another resource is a switch, and its ids name its origin: documents
  // are read again.
  await key.fill("typed-for-globex");
  await save.click();
  const confirm = page.getByRole("dialog", { name: "Change where AI runs?" });
  await expect(confirm).toContainText(
    "New drafts, document reads and questions run on Azure OpenAI, with your API key. Documents are read again at your cost on their next load.",
  );
  await confirm.getByRole("button", { name: "Cancel" }).click();
  await endpoint.fill("https://acme.openai.azure.com");
  await expect(save).toBeEnabled();

  // A deployment the resource lacks fails its test; the setting stays.
  await page.getByLabel("Choose a deployment per task").check();
  await page
    .getByRole("group", { name: "Questions" })
    .getByLabel("Questions deployment")
    .fill("missing-gpt");
  // Same kind, same reading deployments: nothing to confirm.
  await save.click();
  await expect(
    page.getByRole("alert").filter({ hasText: "stay active" }),
  ).toHaveText(
    "Azure OpenAI doesn’t have one of the models chosen. Your current settings stay active.",
  );
  await holds(page, "prod-gpt");
  await expect(radio(page, "Azure OpenAI")).toBeChecked();
});

const SERVER_KEY = "sk-or-v1-e2e-0123456789abcdefwxyz";

test("runs on a custom OpenAI-compatible server, only on https at a public address", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 200 });
  await signIn(page);
  await page.goto("/settings/ai");
  const card = page
    .locator(".choice-card")
    .filter({ hasText: "Custom (OpenAI-compatible)" });
  await expect(card).toContainText(
    "Sent to your endpoint. Helix can’t see where that endpoint sends it.",
  );
  await radio(page, "Custom (OpenAI-compatible)").check();
  const url = page.getByLabel("Base URL"),
    calls = page.getByLabel("Calls at once"),
    key = page.getByLabel("API key"),
    save = page.getByRole("button", { name: "Test and save" });
  await expect(url).toHaveAttribute(
    "placeholder",
    "https://llm.example.com/v1",
  );
  await expect(
    page.getByText(
      "The server’s OpenAI-compatible API, including /v1. It must be https on a public address.",
    ),
  ).toBeVisible();
  await page.getByLabel("Model name").fill("qwen3-32b");
  await expect(save).toBeDisabled();

  // Hosted, Helix calls only https servers at public addresses.
  for (const [bad, why] of [
    ["http://llm.example.com/v1", "Use an https:// address."],
    ["https://10.0.0.5:8000/v1", "The endpoint resolves to a private address."],
    [
      "https://localhost:1234/v1",
      "The endpoint resolves to a private address.",
    ],
    [
      "llm.example.com/v1",
      "Use an http:// or https:// address with no user, query or fragment.",
    ],
  ] as const) {
    await url.fill(bad);
    await expect(page.getByText(why)).toBeVisible();
    await expect(save).toBeDisabled();
  }
  // A name only the WFE can resolve: it refuses one that is private.
  await url.fill("https://llm.corp.internal/v1");
  await save.click();
  const confirm = page.getByRole("dialog", { name: "Change where AI runs?" });
  await expect(confirm).toContainText(
    "New drafts, document reads and questions run on llm.corp.internal.",
  );
  await confirm.getByRole("button", { name: "Test and save" }).click();
  await expect(confirm.getByRole("alert")).toHaveText(
    "The endpoint resolves to a private address.",
  );
  await confirm.getByRole("button", { name: "Cancel" }).click();

  // A public one: Responses, eight calls at once, with a key.
  await url.fill("https://LLM.example.com/v1/");
  await expect(card).toContainText(
    "Sent to llm.example.com. Helix can’t see where that endpoint sends it.",
  );
  const format = page.getByRole("combobox", { name: "API format" });
  await expect(format).toHaveText(/^OpenAI Chat Completions/);
  await choose(format, /^OpenAI Responses/);
  await expect(calls).toHaveValue("4");
  await calls.fill("0");
  await expect(
    page.getByText("Use a whole number from 1 to 64."),
  ).toBeVisible();
  await expect(save).toBeDisabled();
  await calls.fill("8");
  // No cost counted unless prices are entered.
  const cost = page.getByRole("combobox", { name: "Cost" });
  await expect(cost).toHaveText(/^Don’t track cost/);
  await choose(cost, /^Enter prices/);
  await expect(save).toBeDisabled();
  await page.getByLabel("Input", { exact: true }).fill("0.2");
  await page.getByLabel("Output", { exact: true }).fill("0.6");
  await expect(save).toBeEnabled();
  await choose(cost, /^Don’t track cost/);
  await expect(page.getByLabel("Input", { exact: true })).toHaveCount(0);
  await expect(key).toHaveAttribute(
    "placeholder",
    "Only if the server asks for one",
  );
  await key.fill(SERVER_KEY);
  const put = page.waitForRequest(
    (r) => r.method() === "PUT" && r.url().endsWith("/foundry/ai"),
  );
  await save.click();
  await expect(confirm).toContainText(
    "New drafts, document reads and questions run on llm.example.com.",
  );
  await confirm.getByRole("button", { name: "Test and save" }).click();
  const model = { name: "qwen3-32b", free: {} };
  expect((await put).postDataJSON()).toEqual({
    provider: {
      openaiCompatible: {
        baseUrl: "https://llm.example.com/v1",
        api: "FOUNDRY_AI_COMPAT_API_RESPONSES",
        maxCallsInFlight: 8,
      },
    },
    models: { draft: model, ask: model, extract: model, verify: model },
    apiKey: SERVER_KEY,
  });
  await expect(
    page.getByRole("status").filter({ hasText: "Testing llm.example.com:" }),
  ).toBeVisible();
  await expect(
    page.getByText("AI models now run on llm.example.com."),
  ).toBeVisible();
  await expect(radio(page, "Custom (OpenAI-compatible)")).toBeChecked();
  await holds(
    page,
    "Sent to llm.example.com. Helix can’t see where that endpoint sends it.",
  );
  await holds(page, "qwen3-32b");
  await expect(page.getByLabel("API key")).toHaveAttribute(
    "placeholder",
    "Leave blank to keep sk-or…wxyz",
  );

  // The form starts from it; its key is kept only for its base URL.
  await expect(url).toHaveValue("https://llm.example.com/v1");
  await expect(format).toHaveText(/^OpenAI Responses/);
  await expect(calls).toHaveValue("8");
  await expect(key).toHaveAttribute(
    "placeholder",
    "Leave blank to keep sk-or…wxyz",
  );
  await key.fill("typed-for-llm");
  await url.fill("https://llm2.example.com/v1");
  // A key typed for one server isn't sent to another, nor is the saved one.
  await expect(key).toHaveValue("");
  await expect(key).toHaveAttribute(
    "placeholder",
    "Only if the server asks for one",
  );
  await expect(
    page.getByText(
      "The saved key is sent only to llm.example.com; enter it again if this server needs one.",
      { exact: false },
    ),
  ).toBeVisible();
  // Another server is a switch, and its ids name its base URL: documents
  // are read again.
  await save.click();
  await expect(confirm).toContainText(
    "New drafts, document reads and questions run on llm2.example.com. Documents are read again at your cost on their next load.",
  );
  await confirm.getByRole("button", { name: "Cancel" }).click();
  await url.fill("https://llm.example.com/v1");
  await expect(key).toHaveAttribute(
    "placeholder",
    "Leave blank to keep sk-or…wxyz",
  );

  // A model that can't call tools fails its test; the setting stays.
  await page.getByLabel("Model name").fill("tiny-notools");
  await save.click();
  await expect(confirm).toContainText(
    "Documents are read again at your cost on their next load.",
  );
  await confirm.getByRole("button", { name: "Test and save" }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "stay active" }),
  ).toHaveText(
    "tiny-notools can’t do what Foundry needs (tool calls or structured answers). It refused a request with Foundry’s tools. Your current settings stay active.",
  );
  // The form keeps the attempt to fix; reloaded, it starts from the setting.
  await page.reload();
  await holds(page, "qwen3-32b");

  // A new draft runs on it.
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(page).toHaveURL(/\/generations\/gen_/);
  await expect(page.getByText(/^Drafted version \d+$/)).toBeVisible({
    timeout: 15_000,
  });
  await expect(page.getByText("qwen3-32b", { exact: true })).toBeVisible();
});

test("fails a custom server's test on a key it refuses, naming its host", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/settings/ai");
  await radio(page, "Custom (OpenAI-compatible)").check();
  await page.getByLabel("Base URL").fill("https://gateway.example.com/api/v1");
  await page.getByLabel("Model name").fill("acme/qwen3-32b");
  await page.getByLabel("API key").fill("sk-bad-e2e-0123456789abcdef");
  await page.getByRole("button", { name: "Test and save" }).click();
  await page
    .getByRole("dialog", { name: "Change where AI runs?" })
    .getByRole("button", { name: "Test and save" })
    .click();
  const failed = page.getByRole("alert").filter({ hasText: "refused" });
  await expect(failed).toHaveText(
    "gateway.example.com refused the API key. Your current settings stay active.",
  );
  await page.reload();
  await expect(radio(page, "Default")).toBeChecked();
  await expect(failed).toBeVisible();
});

const BEDROCK_KEY = "ABSKZTJlLWJlZHJvY2sta2V5LTAxMjM0NTY3wxyz";
const PROFILE_ARN =
  "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/e2e0001";

test("runs on Amazon Bedrock in a region, an inference profile priced as its model", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 200 });
  await signIn(page);
  await page.goto("/settings/ai");
  await radio(page, "Amazon Bedrock").check();
  const card = page
    .locator(".choice-card")
    .filter({ hasText: "Amazon Bedrock" });
  // Recommended: Claude through global inference profiles, which may run
  // outside the region chosen.
  const GLOBAL =
    "A global inference profile may run requests in any AWS commercial region.";
  await expect(card).toContainText(
    `Sent to Amazon Bedrock, under your AWS account. ${GLOBAL}`,
  );
  await expect(
    page.getByText(
      "Where Foundry calls Bedrock, under your AWS account. A cross-region inference profile, like global.… or eu.…, may run the models in other regions.",
    ),
  ).toBeVisible();
  const key = page.getByLabel("API key");
  await expect(key).toHaveAttribute(
    "placeholder",
    "A long-term Bedrock API key, ABSK…",
  );
  await key.fill(BEDROCK_KEY);
  // Recommended: Claude through Bedrock's global inference profiles.
  await expect(
    page.getByText(
      "Drafting: global.anthropic.claude-opus-5-5 · Questions: global.anthropic.claude-sonnet-5-5 · Reading documents: global.anthropic.claude-haiku-5-5",
    ),
  ).toBeVisible();
  // Nothing is sent until a region is chosen; Ireland has no FIPS endpoint.
  const region = page.getByRole("combobox", { name: "Region" });
  await expect(region).toHaveText("Choose a region");
  const save = page.getByRole("button", { name: "Test and save" });
  await expect(save).toBeDisabled();
  const fips = page.getByLabel("Use the region’s FIPS endpoint");
  await choose(region, /^eu-west-1/);
  await expect(card).toContainText(
    `Sent to Amazon Bedrock in eu-west-1, under your AWS account. ${GLOBAL}`,
  );
  await expect(fips).toHaveCount(0);
  await expect(save).toBeEnabled();
  await choose(region, /^us-east-1/);
  await fips.check();
  await expect(card).toContainText(
    "Sent to Amazon Bedrock’s FIPS endpoint in us-east-1, under your AWS account.",
  );

  // Drafting on a US inference profile, priced as its model by Helix's
  // table: no prices to type.
  await page.getByLabel("Choose a model per task").check();
  const drafting = page.getByRole("group", { name: "Drafting" });
  await expect(drafting.getByRole("combobox")).toHaveText(
    "global.anthropic.claude-opus-5-5",
  );
  await choose(drafting.getByRole("combobox"), /^Another model ID/);
  await expect(save).toBeDisabled();
  await drafting.getByLabel("Model ID").fill("us.anthropic.claude-opus-5-5");
  await expect(
    drafting.getByText(
      "Priced as anthropic.claude-opus-5-5, from Helix’s prices: $4.4 in, $22 out per million tokens.",
    ),
  ).toBeVisible();
  await expect(drafting.getByLabel("Input", { exact: true })).toHaveCount(0);
  // Questions on an application inference profile, priced as the model it
  // runs; until one is chosen, by hand.
  const questions = page.getByRole("group", { name: "Questions" });
  await choose(
    questions.getByRole("combobox", { name: "Questions", exact: true }),
    /^Another model ID/,
  );
  await questions.getByLabel("Model ID").fill(PROFILE_ARN);
  await expect(questions.getByLabel("Input", { exact: true })).toBeVisible();
  await expect(save).toBeDisabled();
  await choose(
    questions.getByRole("combobox", { name: "Questions priced as" }),
    /^anthropic\.claude-sonnet-5-5/,
  );
  await expect(
    questions.getByText(
      "The model the ID runs, for Helix’s prices: $2.2 in, $11 out per million tokens.",
    ),
  ).toBeVisible();
  // Reading documents on a model Helix has no price for, at the owner's
  // rates.
  const reading = page.getByRole("group", { name: "Reading documents" });
  await choose(
    reading.getByRole("combobox", { name: "Reading documents", exact: true }),
    /^Another model ID/,
  );
  await reading.getByLabel("Model ID").fill("bedrock-api-key-oops");
  await expect(
    reading.getByText(
      "That looks like an API key; enter it in the API key field.",
    ),
  ).toBeVisible();
  await reading.getByLabel("Model ID").fill("meta.llama-5-maverick");
  await reading.getByLabel("Input", { exact: true }).fill("0.24");
  await reading.getByLabel("Output", { exact: true }).fill("0.97");

  const put = page.waitForRequest(
    (r) => r.method() === "PUT" && r.url().endsWith("/foundry/ai"),
  );
  await confirmSwitch(page, "Amazon Bedrock");
  expect((await put).postDataJSON()).toEqual({
    provider: { bedrock: { region: "us-east-1", fips: true } },
    models: {
      draft: { name: "us.anthropic.claude-opus-5-5", table: {} },
      ask: { name: PROFILE_ARN, tableAs: "anthropic.claude-sonnet-5-5" },
      extract: {
        name: "meta.llama-5-maverick",
        rates: { inputUsdPerMtok: "0.24", outputUsdPerMtok: "0.97" },
      },
      verify: { name: "global.anthropic.claude-sonnet-5-5", table: {} },
    },
    apiKey: BEDROCK_KEY,
  });
  await expect(
    page.getByRole("status").filter({ hasText: "Testing Amazon Bedrock:" }),
  ).toBeVisible();
  await expect(
    page.getByText("AI models now run on Amazon Bedrock."),
  ).toBeVisible();
  await expect(radio(page, "Amazon Bedrock")).toBeChecked();
  // Checking facts runs on a global profile still.
  await holds(
    page,
    `Sent to Amazon Bedrock’s FIPS endpoint in us-east-1, under your AWS account. ${GLOBAL}`,
  );
  await holds(page, "us.anthropic.claude-opus-5-5");
  await expect(page.getByLabel("API key")).toHaveAttribute(
    "placeholder",
    "Leave blank to keep …wxyz",
  );
  await expect(page.getByLabel("API key")).toHaveValue(BEDROCK_KEY);

  // A new draft runs on it.
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(page).toHaveURL(/\/generations\/gen_/);
  await expect(page.getByText(/^Drafted version \d+$/)).toBeVisible({
    timeout: 15_000,
  });
  await expect(
    page.getByText("us.anthropic.claude-opus-5-5", { exact: true }),
  ).toBeVisible();
});

test("keeps a Bedrock key across regions, and keeps the setting when a model can't call tools", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/settings/ai");
  await radio(page, "Amazon Bedrock").check();
  const region = page.getByRole("combobox", { name: "Region" });
  await choose(region, /^us-east-2/);
  await page.getByLabel("Use the region’s FIPS endpoint").check();
  await page.getByLabel("API key").fill(BEDROCK_KEY);
  await confirmSwitch(page, "Amazon Bedrock");
  await expect(
    page.getByText("AI models now run on Amazon Bedrock."),
  ).toBeVisible();

  // The form starts from the setting; a Bedrock key belongs to the AWS
  // account, so another region keeps it, and Claude asks for its own.
  await expect(region).toHaveText("us-east-2");
  const fips = page.getByLabel("Use the region’s FIPS endpoint");
  await expect(fips).toBeChecked();
  const key = page.getByLabel("API key");
  await expect(key).toHaveAttribute("placeholder", "Leave blank to keep …wxyz");
  await radio(page, "Claude").check();
  await expect(key).toHaveAttribute("placeholder", "sk-ant-…");
  await radio(page, "Amazon Bedrock").check();
  await choose(region, /^eu-central-1/);
  await expect(fips).toHaveCount(0);
  await expect(key).toHaveAttribute("placeholder", "Leave blank to keep …wxyz");

  // A model without tool use fails its test; the setting stays.
  await choose(
    page.getByRole("combobox", { name: "Model" }),
    /^Another model ID/,
  );
  await page.getByLabel("Model ID").fill("eu.meta.llama-notools");
  // An EU profile stays in the EU.
  await expect(
    page.locator(".choice-card").filter({ hasText: "Amazon Bedrock" }),
  ).toContainText(
    "Sent to Amazon Bedrock in eu-central-1, under your AWS account. A cross-region inference profile may run requests in other regions within the EU.",
  );
  await page.getByLabel("Input", { exact: true }).fill("0.2");
  await page.getByLabel("Output", { exact: true }).fill("0.6");
  const put = page.waitForRequest(
    (r) => r.method() === "PUT" && r.url().endsWith("/foundry/ai"),
  );
  await page.getByRole("button", { name: "Test and save" }).click();
  // Other reading models read documents again, at the workspace's cost.
  const confirm = page.getByRole("dialog", { name: "Change where AI runs?" });
  await expect(confirm).toContainText(
    "Documents are read again at your cost on their next load.",
  );
  await confirm.getByRole("button", { name: "Test and save" }).click();
  const sent = (await put).postDataJSON();
  expect(sent.provider).toEqual({ bedrock: { region: "eu-central-1" } });
  expect(sent.apiKey).toBeUndefined();
  await expect(
    page.getByRole("alert").filter({ hasText: "stay active" }),
  ).toHaveText(
    "eu.meta.llama-notools can’t do what Foundry needs (tool calls or structured answers). It refused a request with Foundry’s tools. Your current settings stay active.",
  );
  await page.reload();
  await holds(
    page,
    "Sent to Amazon Bedrock’s FIPS endpoint in us-east-2, under your AWS account. A global inference profile may run requests in any AWS commercial region.",
  );
});

test("saves a Bedrock region the list lacks, typed, and starts from it", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/settings/ai");
  await radio(page, "Amazon Bedrock").check();
  const card = page
    .locator(".choice-card")
    .filter({ hasText: "Amazon Bedrock" });
  const region = page.getByRole("combobox", { name: "Region" });
  // AWS's regions for Bedrock, Melbourne and the UAE among them.
  await choose(region, /^ap-southeast-4/);
  await choose(region, /^me-central-1/);
  await expect(card).toContainText("Sent to Amazon Bedrock in me-central-1");
  // GovCloud has no global profiles to run the recommended models on.
  await choose(region, /^us-gov-west-1/);
  await expect(
    page.getByText(
      "Bedrock has no global inference profiles in us-gov-west-1: choose models it offers there.",
    ),
  ).toBeVisible();
  await expect(page.getByLabel("Use the region’s FIPS endpoint")).toBeVisible();
  // One AWS adds later, by its code: checked as the WFE checks it.
  await choose(region, /^Another region/);
  const code = page.getByLabel("Region code");
  await expect(code).toHaveValue("");
  await page.getByLabel("API key").fill(BEDROCK_KEY);
  const save = page.getByRole("button", { name: "Test and save" });
  await expect(save).toBeDisabled();
  await code.fill("ap-southeast-9a");
  await expect(
    page.getByText("Use an AWS region, like us-east-1."),
  ).toBeVisible();
  await expect(save).toBeDisabled();
  await code.fill(" ap-southeast-9 ");
  await expect(card).toContainText("Sent to Amazon Bedrock in ap-southeast-9,");
  await expect(page.getByLabel("Use the region’s FIPS endpoint")).toHaveCount(
    0,
  );
  const put = page.waitForRequest(
    (r) => r.method() === "PUT" && r.url().endsWith("/foundry/ai"),
  );
  await confirmSwitch(page, "Amazon Bedrock");
  expect((await put).postDataJSON().provider).toEqual({
    bedrock: { region: "ap-southeast-9" },
  });
  await expect(
    page.getByText("AI models now run on Amazon Bedrock."),
  ).toBeVisible();
  await holds(page, "Sent to Amazon Bedrock in ap-southeast-9,");
  // The form starts from the setting, its region still typed.
  await expect(region).toHaveText("Another region");
  await expect(code).toHaveValue("ap-southeast-9");
});

test("keeps the default when a Bedrock key fails its test", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/settings/ai");
  await radio(page, "Amazon Bedrock").check();
  await choose(page.getByRole("combobox", { name: "Region" }), /^us-west-2/);
  await page.getByLabel("API key").fill("ABSKbad0123456789abcdefghijklmn");
  await confirmSwitch(page, "Amazon Bedrock");
  await expect(
    page.getByRole("alert").filter({ hasText: "refused" }),
  ).toHaveText(
    "Amazon Bedrock refused the API key. Your current settings stay active.",
  );
  await page.reload();
  await expect(radio(page, "Default")).toBeChecked();
});
