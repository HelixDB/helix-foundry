import { readFileSync } from "node:fs";
import {
  expect,
  test as base,
  type Locator,
  type Page,
} from "@playwright/test";
import { e2eLocalPorts } from "../../../mock/env";
import {
  choose,
  createdOntology,
  explorer,
  file,
  startOntology,
  uploadCard,
  uploadInData,
} from "../support/mock";

// A local build (src/shared/deployment.ts) against its own mock, which plays a
// local API: one owner (Ada) and no sign-in, team or billing, and every
// connector open to every workspace.

const MOCK = `http://127.0.0.1:${e2eLocalPorts.mock}`;
// Whether the AI form holds `text`, in its words or a field's value (models
// a field names are typed in).
const holds = (form: Locator, text: string) =>
  expect
    .poll(async () => {
      const values = await form
        .locator("input")
        .evaluateAll((els) => els.map((e) => (e as HTMLInputElement).value));
      return [await form.innerText(), ...values].join("\n");
    })
    .toContain(text);
const configure = (config: {
  refuseLocalToken?: boolean;
  // The install's AI provider (mock/routes/ai.ts), found on its machine
  // unless its .env names it (aiDefaultNamed): false, none; "ollama",
  // Ollama's. aiDetect is what looking again finds.
  aiDefault?: boolean;
  aiDefaultKind?: "ollama";
  aiDefaultNamed?: boolean;
  aiDetect?: "anthropic" | "ollama" | "none";
  // false: the bundled Ollama isn't running.
  ollamaBundled?: boolean;
  // Ada's one workspace is a new install's, "My workspace", with nothing in
  // it (mock/state.ts).
  freshInstall?: boolean;
  aiProbeMs?: number;
  loadStepMs?: number;
  askStepMs?: number;
}) =>
  fetch(`${MOCK}/__mock/config`, {
    method: "POST",
    body: JSON.stringify(config),
  });

const test = base.extend<{ fresh: void }>({
  fresh: [
    // Playwright fixtures must destructure their first argument.
    // eslint-disable-next-line no-empty-pattern
    async ({}, use) => {
      await fetch(`${MOCK}/__mock/reset`, { method: "POST" });
      await use();
    },
    { auto: true },
  ],
});

const CONTRACT = readFileSync(
  new URL("../../../mock/fixtures/contract.md", import.meta.url),
);

// Uploads the seeded contract, makes version 3 current and loads it, as
// tests/e2e/ask.spec.ts does.
async function loadContract(page: Page) {
  await uploadInData(page, [file("contract.md", CONTRACT)]);
  await page.goto("/ontology/ont_acme_contracts/versions/ver_seed_3");
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
  await page.goto("/ontology/ont_acme_contracts");
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

// Creates Globex from the switcher: a new workspace, which opens on its
// onboarding, the first ontology's wizard, until it has an ontology.
async function createGlobex(page: Page) {
  await page.goto("/");
  await page.getByRole("button", { name: "Workspace: Acme" }).click();
  await page.getByRole("menuitem", { name: "Create workspace" }).click();
  const create = page.getByRole("dialog", { name: "Create a workspace" });
  await expect(create).toContainText(
    "A new workspace starts with its own data and ontology.",
  );
  await create.getByLabel("Workspace name").fill("Globex");
  await create.getByRole("button", { name: "Create workspace" }).click();
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(
    page.getByRole("button", { name: "Workspace: Globex" }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { level: 1, name: "Create your first ontology" }),
  ).toBeVisible();
}

test("opens straight into the workspace's main page, Ask", async ({ page }) => {
  await page.goto("/");
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Ask" }),
  ).toBeVisible();

  // The picker names no dashboard and no team.
  await page.goto("/workspaces");
  await expect(
    page.getByText("Each workspace keeps its own data and ontologies."),
  ).toBeVisible();

  // No sign-in page: it opens the main page.
  await page.goto("/login?return_to=/data");
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts$/);
});

test("opens a new install in its workspace's onboarding, never the workspace picker", async ({
  page,
}) => {
  await configure({ freshInstall: true });
  await page.goto("/");
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Create your first ontology" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Workspace: My workspace" }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Create your first workspace" }),
  ).toHaveCount(0);

  // Another workspace is still made from the switcher, and opens on its own
  // onboarding.
  await page.getByRole("button", { name: "Workspace: My workspace" }).click();
  await page.getByRole("menuitem", { name: "Create workspace" }).click();
  const create = page.getByRole("dialog", { name: "Create a workspace" });
  await create.getByLabel("Workspace name").fill("Globex");
  await create.getByRole("button", { name: "Create workspace" }).click();
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(
    page.getByRole("button", { name: "Workspace: Globex" }),
  ).toBeVisible();
});

test("keeps only settings and the theme in the account menu", async ({
  page,
}) => {
  await page.goto("/");
  await page.getByRole("button", { name: "Account" }).click();
  const menu = page.getByRole("menu");
  await expect(menu.getByRole("menuitem", { name: "Settings" })).toBeVisible();
  await expect(menu.getByRole("menuitemradio", { name: "Dark" })).toBeVisible();
  for (const name of ["Members", "Plan & billing", "Sign out"])
    await expect(menu.getByRole("menuitem", { name })).toHaveCount(0);
  // No usage: a local install bills nothing, and its API serves none.
  await expect(menu).not.toContainText("Usage");

  await menu.getByRole("menuitem", { name: "Settings" }).click();
  await expect(page.getByRole("tab", { name: "General" })).toBeVisible();
  await expect(page.getByRole("tab", { name: "AI models" })).toBeVisible();
  await expect(page.getByRole("tab")).toHaveCount(2);
  await expect(page.getByText("Helix dashboard")).toHaveCount(0);
  for (const path of ["/settings/members", "/settings/billing"]) {
    await page.goto(path);
    await expect(
      page.getByRole("heading", { name: "Page not found" }),
    ).toBeVisible();
  }
});

test("offers every connector to a new workspace", async ({ page }) => {
  // Its onboarding's sources step adds data through the Data area's picker.
  await createGlobex(page);
  await startOntology(page, "Renewals");
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("button", { name: "Add data" }).first().click();
  const picker = page.locator("dialog[open]");
  await expect(picker).toHaveAccessibleName("Where is your data?");
  for (const name of ["PostgreSQL", "MySQL", "Amazon S3"])
    await expect(
      picker.getByRole("button", { name, exact: true }),
    ).not.toHaveAccessibleDescription(/Foundry Pro/);
  // No API source: its senders need the Helix MCP, which runs only hosted.
  await expect(picker.getByText("From your agent")).toHaveCount(0);
  await expect(
    picker.getByRole("button", { name: "API source", exact: true }),
  ).toHaveCount(0);
  await picker.getByRole("button", { name: "PostgreSQL", exact: true }).click();
  await expect(picker.getByRole("note")).toHaveCount(0);
  // A database on this computer is the Docker host, not the pod's loopback.
  await expect(picker).toContainText(
    "Foundry connects from inside Docker, so a database on this computer is host.docker.internal, not localhost, and must accept connections from Docker’s network. If it has no TLS, choose No TLS under Connection options.",
  );
  await expect(picker.getByLabel("Host")).toBeVisible();
});

test("has no New API source in the palette", async ({ page }) => {
  await page.goto("/data");
  await expect(
    page.getByRole("heading", { level: 1, name: "Connectors" }),
  ).toBeVisible();
  await page.keyboard.press("ControlOrMeta+k");
  const palette = page.getByRole("dialog", { name: "Search your workspace" });
  await palette.getByRole("combobox").fill("new");
  await expect(
    palette.getByRole("option", { name: /Create an ontology/ }),
  ).toBeVisible();
  await expect(
    palette.getByRole("option", { name: /New API source/ }),
  ).toHaveCount(0);
});

test("creates a database in this computer's Helix, without a plan or keys to revoke", async ({
  page,
}) => {
  // Globex is new: in the hosted site it has no Helix plan for a database.
  // A document uploaded in its onboarding gives the first draft something
  // to read.
  await createGlobex(page);
  await startOntology(page, "Renewals");
  await page.getByRole("radio", { name: "Create a new one" }).click();
  await page.getByLabel("Project name").fill("Foundry");
  await page.getByLabel("Database name").fill("Renewals 2026");
  await expect(
    page.getByText(
      "It is kept in the Helix that runs with Foundry on this computer.",
    ),
  ).toBeVisible();
  await page.getByRole("button", { name: "Continue" }).click();
  // The sources step offers no API source, and says nothing of agents'
  // pieces.
  await expect(
    page.getByRole("button", { name: "New API source" }),
  ).toHaveCount(0);
  await expect(page.getByText(/pieces your agents send/)).toHaveCount(0);
  const chooser = page.waitForEvent("filechooser");
  await page.getByRole("button", { name: "Upload documents" }).click();
  await (
    await chooser
  ).setFiles([file("renewals.md", "# Renewals\n\nGlobex renews every March.")]);
  await expect(uploadCard(page, "renewals.md")).toContainText("Uploaded");
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("button", { name: "Skip" }).click();
  await page.getByRole("button", { name: "Create and generate" }).click();
  await createdOntology(page);
  // The page the wizard lands on, where the hosted site notes the key.
  await expect(page.getByText("Ontology generation")).toBeVisible();
  await expect(page.getByText(/first API key/)).toHaveCount(0);
  await expect(page.getByText("Helix dashboard")).toHaveCount(0);
});

test("runs AI on the install's default, or the owner's Claude key without a plan", async ({
  page,
}) => {
  await configure({ aiProbeMs: 600 });
  await page.goto("/settings/ai");
  const setting = page.locator(".hf-panel").filter({
    has: page.getByRole("heading", { name: "Choose where AI runs" }),
  });
  await expect(
    setting.getByRole("radio", { name: "Install default", exact: true }),
  ).toBeChecked();
  // How the install found it.
  await holds(
    setting,
    "Using Claude: Foundry was started with an ANTHROPIC_API_KEY. Sent to Anthropic under your API key.",
  );
  await expect(
    page.getByRole("radio", { name: "Install default", exact: true }),
  ).toBeChecked();
  // No People enrichment here: what is sent says nothing of it.
  await expect(page.getByText(/context\.dev/)).toHaveCount(0);

  await page.getByRole("radio", { name: "Claude", exact: true }).check();
  // No plan is sold: no Pro gate.
  await expect(page.getByText(/needs Foundry Pro/)).toHaveCount(0);
  await page.getByLabel("API key").fill("sk-ant-api03-local-0123456789wxyz");
  await page.getByRole("button", { name: "Test and save" }).click();
  await page
    .getByRole("dialog", { name: "Change where AI runs?" })
    .getByRole("button", { name: "Test and save" })
    .click();
  await expect(page.getByText("AI models now run on Claude.")).toBeVisible();
  await expect(
    setting.getByRole("radio", { name: "Claude", exact: true }),
  ).toBeChecked();

  await page
    .getByRole("radio", { name: "Install default", exact: true })
    .check();
  await page.getByRole("button", { name: "Use the install default" }).click();
  await page
    .getByRole("dialog", { name: "Change where AI runs?" })
    .getByRole("button", { name: "Switch" })
    .click();
  await expect(
    page.getByText("AI models now run on the install default."),
  ).toBeVisible();
});

test("runs AI on OpenAI or an Azure OpenAI resource without a plan, from no provider", async ({
  page,
}) => {
  await configure({ aiDefault: false, aiProbeMs: 600 });
  await page.goto("/settings/ai");
  const setting = page.locator(".hf-panel").filter({
    has: page.getByRole("heading", { name: "Choose where AI runs" }),
  });
  await expect(setting).toContainText("No AI provider was found.");

  // Nothing runs yet, so nothing to confirm.
  await page.getByRole("radio", { name: "OpenAI", exact: true }).check();
  await expect(page.getByText(/needs Foundry Pro/)).toHaveCount(0);
  await page.getByLabel("API key").fill("sk-proj-local-0123456789abcdefwxyz");
  await page.getByRole("button", { name: "Test and save" }).click();
  await expect(page.getByText("AI models now run on OpenAI.")).toBeVisible();
  await expect(
    setting.getByRole("radio", { name: "OpenAI", exact: true }),
  ).toBeChecked();
  await holds(setting, "gpt-6.1-sol");

  // An Azure deployment Helix has no price for, at the owner's rates.
  await page.getByRole("radio", { name: "Azure OpenAI", exact: true }).check();
  await page.getByLabel("Endpoint").fill("https://acme.services.ai.azure.com");
  await page.getByLabel("Deployment", { exact: true }).fill("local-gpt");
  await choose(
    page.getByRole("combobox", { name: "Priced as" }),
    /^Enter prices/,
  );
  await page.getByLabel("Input", { exact: true }).fill("0.4");
  await page.getByLabel("Output", { exact: true }).fill("1.6");
  await page.getByLabel("Cached input").fill("0.1");
  await page.getByLabel("API key").fill("0123456789abcdef0123456789abcdef");
  await page.getByRole("button", { name: "Test and save" }).click();
  await page
    .getByRole("dialog", { name: "Change where AI runs?" })
    .getByRole("button", { name: "Test and save" })
    .click();
  await expect(
    page.getByText("AI models now run on Azure OpenAI."),
  ).toBeVisible();
  await holds(
    setting,
    "Sent to your Azure OpenAI resource at acme.services.ai.azure.com.",
  );
  await holds(setting, "local-gpt");
  await expect(page.getByLabel("Cached input")).toHaveValue("0.1");
});

test("runs AI on an OpenAI-compatible server on this computer, over http", async ({
  page,
}) => {
  await configure({ aiDefault: false, aiProbeMs: 600 });
  await page.goto("/settings/ai");
  const setting = page.locator(".hf-panel").filter({
    has: page.getByRole("heading", { name: "Choose where AI runs" }),
  });
  await page
    .getByRole("radio", { name: "Custom (OpenAI-compatible)", exact: true })
    .check();
  // No plan is sold: no Pro gate.
  await expect(page.getByText(/needs Foundry Pro/)).toHaveCount(0);
  const url = page.getByLabel("Base URL");
  await expect(url).toHaveAttribute(
    "placeholder",
    "http://host.docker.internal:1234/v1",
  );
  await expect(
    page.getByText(
      /A server on this computer is host\.docker\.internal, not localhost/,
    ),
  ).toBeVisible();
  await url.fill("http://host.docker.internal:1234");
  await expect(
    page.getByText(
      "Most servers answer under /v1: add it unless yours says otherwise.",
    ),
  ).toBeVisible();
  await url.fill("http://host.docker.internal:1234/v1");
  await page.getByLabel("Model name").fill("qwen/qwen3-8b");
  await expect(page.getByRole("combobox", { name: "Cost" })).toHaveText(
    /^Don’t track cost/,
  );
  // LM Studio takes no key; nothing runs yet, so nothing to confirm.
  const put = page.waitForRequest(
    (r) => r.method() === "PUT" && r.url().endsWith("/foundry/ai"),
  );
  await page.getByRole("button", { name: "Test and save" }).click();
  const model = { name: "qwen/qwen3-8b", free: {} };
  expect((await put).postDataJSON()).toEqual({
    provider: {
      openaiCompatible: {
        baseUrl: "http://host.docker.internal:1234/v1",
        api: "FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS",
        maxCallsInFlight: 4,
      },
    },
    models: { draft: model, ask: model, extract: model, verify: model },
  });
  await expect(
    page.getByText("AI models now run on host.docker.internal."),
  ).toBeVisible();
  await expect(
    setting.getByRole("radio", {
      name: "Custom (OpenAI-compatible)",
      exact: true,
    }),
  ).toBeChecked();
  await holds(
    setting,
    "Sent to host.docker.internal. Helix can’t see where that endpoint sends it.",
  );
  await holds(setting, "qwen/qwen3-8b");

  // Another server is a switch: its ids name its base URL, so documents are
  // read again. One that isn't running fails its test; the setting stays.
  await url.fill("http://lmstudio.invalid:1234/v1");
  await page.getByRole("button", { name: "Test and save" }).click();
  const confirm = page.getByRole("dialog", { name: "Change where AI runs?" });
  await expect(confirm).toContainText(
    "New drafts, document reads and questions run on lmstudio.invalid. Documents are read again at your cost on their next load.",
  );
  await confirm.getByRole("button", { name: "Test and save" }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "stay active" }),
  ).toHaveText(
    "lmstudio.invalid didn’t answer the test, after retries. Try again in a while. Your current settings stay active.",
  );
  // The form keeps the attempt to fix; reloaded, it starts from the setting.
  await page.reload();
  await holds(setting, "Sent to host.docker.internal.");
});

test("runs AI on Ollama with no key, once a test on a window and model it can run passes", async ({
  page,
}) => {
  await configure({ aiDefault: false, aiProbeMs: 600 });
  await page.goto("/settings/ai");
  const setting = page.locator(".hf-panel").filter({
    has: page.getByRole("heading", { name: "Choose where AI runs" }),
  });
  const failure = page
    .getByRole("alert")
    .filter({ hasText: "Your current settings stay active." });
  // First of the providers, on the bundled server, with no key to enter.
  const ollama = page.getByRole("radio", { name: "Ollama", exact: true });
  const field = page.getByRole("group", { name: "Model", exact: true });
  const model = field.getByRole("combobox", { name: "Model" }),
    named = field.getByLabel("Model name");
  await expect(ollama).toBeChecked();
  await expect(
    page.getByRole("group", { name: "Where AI runs" }).getByRole("radio"),
  ).toHaveCount(6);
  await expect(page.getByRole("combobox", { name: "Server" })).toContainText(
    "Bundled",
  );
  await expect(
    page.getByText(
      "The Ollama that runs with Foundry, from the compose file’s ollama profile: docker compose --profile ollama up -d.",
    ),
  ).toBeVisible();
  // The server's models, with their sizes: the one Foundry runs is chosen.
  await expect(model).toHaveText("qwen3.5:4b · 3.4 GB");
  await expect(field).toContainText("No cost is counted.");
  await expect(page.getByLabel("API key")).toHaveCount(0);
  await expect(
    page.getByText("a small local model may stop a draft early"),
  ).toBeVisible();
  await expect(
    page.getByText("Runs on this machine; nothing leaves it."),
  ).toBeVisible();
  // One it can't run is listed with why, and can't be chosen.
  await model.click();
  const unusable = page
    .getByRole("listbox")
    .getByRole("option", { name: /^embeddinggemma · 621 MB/ });
  await expect(unusable).toContainText("Not a chat model.");
  await expect(unusable).toBeDisabled();
  await expect(
    page.getByRole("listbox").getByRole("option", { name: /^qwen2\.5:0\.5b/ }),
  ).toContainText(
    "Its context window, 32,768 tokens, is under the 65,536 drafting needs.",
  );
  await unusable.click({ force: true });
  await page.keyboard.press("Escape");
  await expect(model).toHaveText("qwen3.5:4b · 3.4 GB");

  // A window drafting can't run in can't be saved, as the WFE refuses it.
  const window = page.getByLabel("Context window");
  await expect(window).toHaveValue("65536");
  await expect(
    page.getByText(
      "The tokens every task’s model loads with: drafting needs 65,536, questions 32,768 and the others 16,384.",
    ),
  ).toBeVisible();
  await window.fill("32768");
  await expect(
    page.getByText(
      "Enter a whole number of tokens, 65,536 to 1,048,576: drafting needs 65,536.",
    ),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Test and save" }),
  ).toBeDisabled();
  await window.fill("65536");

  // Another model by name, which the server hasn't pulled: one whose own
  // window is smaller fails its test.
  await choose(model, /^Another model/);
  await expect(named).toHaveValue("");
  await named.fill("qwen3.5-short");
  await expect(field).toContainText(
    "The server hasn’t pulled qwen3.5-short. Pull it there with ollama pull qwen3.5-short before testing it.",
  );
  await page.getByRole("button", { name: "Test and save" }).click();
  await expect(failure).toContainText(
    "The context window is too small for Foundry’s tasks: drafting needs 65,536 tokens, questions 32,768 and the others 16,384.",
  );
  await expect(setting).toContainText("No AI provider was found.");

  // So does a model the server hasn't pulled.
  await named.fill("qwen3.5-missing");
  await page.getByRole("button", { name: "Test and save" }).click();
  await expect(failure).toContainText(
    "Ollama has no model qwen3.5-missing. Pull it on the server with ollama pull qwen3.5-missing, or choose one it has.",
  );

  await choose(model, /^qwen3\.5:4b/);
  await expect(named).toHaveCount(0);
  await page.getByRole("button", { name: "Test and save" }).click();
  await expect(page.getByText("AI models now run on Ollama.")).toBeVisible();
  await expect(
    setting.getByRole("radio", { name: "Ollama", exact: true }),
  ).toBeChecked();
  await holds(setting, "Runs on this machine; nothing leaves it.");
  await expect(model).toHaveText("qwen3.5:4b · 3.4 GB");
  await expect(failure).toHaveCount(0);

  // On this computer, on Linux, needs Ollama listening for Docker, and
  // only for Docker: its API takes no key. Its most capable model is
  // chosen; its cloud model isn't one Foundry runs here.
  await choose(
    page.getByRole("combobox", { name: "Server" }),
    /^On this computer/,
  );
  await expect(
    page.getByText(
      "Ollama installed on this computer; on Linux, start it with OLLAMA_HOST=172.17.0.1, Docker’s bridge address, so Docker can reach it and your network can’t.",
    ),
  ).toBeVisible();
  await expect(model).toHaveText("qwen3.5:9b · 6.6 GB");
  await model.click();
  await expect(
    page
      .getByRole("listbox")
      .getByRole("option", { name: /^gpt-oss:120b-cloud/ }),
  ).toContainText(
    "Runs on ollama.com, so what it is sent would leave this computer.",
  );
  await page.keyboard.press("Escape");

  // Another server, which the work is sent to.
  await choose(page.getByRole("combobox", { name: "Server" }), /^Other/);
  const address = page.getByLabel("Server address");
  await address.fill("http://gpu.lan:11434?x=1");
  await expect(
    page.getByText(
      "Use an http:// or https:// address with no user, query or fragment.",
    ),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Test and save" }),
  ).toBeDisabled();
  // Its own address, not its OpenAI-compatible API's.
  await address.fill("http://gpu.lan:11434/v1");
  await expect(
    page.getByText(
      "Use the Ollama server’s address, like http://127.0.0.1:11434, without /api or /v1.",
    ),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Test and save" }),
  ).toBeDisabled();
  // One that doesn't answer, or is too old to say which models call
  // tools, says so; a model is still named by hand.
  await address.fill("http://gpu.invalid:11434");
  await expect(field).toContainText(
    "Nothing answered at http://gpu.invalid:11434. Check the address and that Ollama runs there.",
  );
  await expect(named).toHaveValue("qwen3.5:9b");
  await address.fill("http://old-gpu.lan:11434");
  await expect(field).toContainText(
    "This Ollama (0.5.7) is too old to say which models call tools. Update it from ollama.com.",
  );
  await choose(model, /^qwen3\.5:4b · 3\.4 GB/);
  await expect(field).toContainText(
    "This Ollama (0.5.7) is too old to say which models call tools.",
  );
  await address.fill("http://gpu.lan:11434");
  await expect(
    page.getByText("Sent to the Ollama server at gpu.lan."),
  ).toBeVisible();
  await expect(model).toHaveText("qwen3.5:4b · 3.4 GB");
  // The same kind and models: nothing to confirm.
  await page.getByRole("button", { name: "Test and save" }).click();
  await expect(setting).toContainText("Sent to the Ollama server at gpu.lan.", {
    timeout: 10_000,
  });
  await page.reload();
  await expect(page.getByLabel("Server address")).toHaveValue(
    "http://gpu.lan:11434",
  );
  await expect(model).toHaveText("qwen3.5:4b · 3.4 GB");

  // The bundled server, once it stops, says how to start it.
  await configure({ ollamaBundled: false });
  await choose(page.getByRole("combobox", { name: "Server" }), /^Bundled/);
  await expect(field).toContainText(
    "The bundled Ollama isn't running. Start it with docker compose --profile ollama up -d.",
  );
  await expect(named).toBeVisible();
});

test("names an install default of Ollama its .env names", async ({ page }) => {
  await configure({ aiDefaultKind: "ollama", aiDefaultNamed: true });
  await page.goto("/settings/ai");
  const setting = page.locator(".hf-panel").filter({
    has: page.getByRole("heading", { name: "Choose where AI runs" }),
  });
  await expect(
    setting.getByRole("radio", { name: "Install default", exact: true }),
  ).toBeChecked();
  // The default's own models aren't listed: only where its work goes.
  await holds(setting, "Ollama, from .env. Sent to your Ollama server.");
  await expect(setting).not.toContainText("Using ");
});

test("finds a model pulled since the install started once Settings › AI models opens", async ({
  page,
}) => {
  // Nothing was found as the install started; a model has been pulled
  // since.
  await configure({ aiDefault: false, aiDetect: "ollama" });
  await page.goto("/ontology/ont_acme_contracts/generate");
  const gate = page.getByRole("note").filter({
    hasText: "Generating an ontology needs an AI provider",
  });
  await expect(gate).toBeVisible();
  await gate.getByRole("link", { name: "Choose a provider" }).click();
  await expect(page).toHaveURL(/\/settings\/ai$/);
  // Opening the page looks again, and the form starts from what it found.
  const setting = page.locator(".hf-panel").filter({
    has: page.getByRole("heading", { name: "Choose where AI runs" }),
  });
  await expect(
    setting.getByRole("radio", { name: "Install default", exact: true }),
  ).toBeChecked();
  await holds(
    setting,
    "Using qwen3.5:4b from the bundled Ollama: the most capable model it has that calls tools. Sent to your Ollama server.",
  );
  await expect(setting).not.toContainText("No AI provider was found.");
  // Drafts run on it, without a restart.
  await page.goto("/ontology/ont_acme_contracts/generate");
  await expect(
    page.getByRole("button", { name: "Generate", exact: true }),
  ).toBeEnabled();
  await expect(gate).toHaveCount(0);
});

test("runs AI on Amazon Bedrock without a plan, from no provider", async ({
  page,
}) => {
  await configure({ aiDefault: false, aiProbeMs: 600 });
  await page.goto("/settings/ai");
  const setting = page.locator(".hf-panel").filter({
    has: page.getByRole("heading", { name: "Choose where AI runs" }),
  });
  await expect(setting).toContainText("No AI provider was found.");
  await page
    .getByRole("radio", { name: "Amazon Bedrock", exact: true })
    .check();
  await expect(page.getByText(/needs Foundry Pro/)).toHaveCount(0);
  await choose(
    page.getByRole("combobox", { name: "Region" }),
    /^ap-southeast-2/,
  );
  await expect(page.getByLabel("Use the region’s FIPS endpoint")).toHaveCount(
    0,
  );
  // A model from Helix's table, for every task.
  await choose(
    page.getByRole("combobox", { name: "Model" }),
    /^amazon\.nova-pro-v1:0/,
  );
  await expect(
    page.getByText(
      "Every task runs on it: $0.8 in, $3.2 out per million tokens.",
    ),
  ).toBeVisible();
  await page.getByLabel("API key").fill("ABSKbG9jYWwtYmVkcm9jay0wMTIzNDU2wxyz");
  // Nothing runs yet, so nothing to confirm.
  await page.getByRole("button", { name: "Test and save" }).click();
  await expect(
    page.getByText("AI models now run on Amazon Bedrock."),
  ).toBeVisible();
  await expect(
    setting.getByRole("radio", { name: "Amazon Bedrock", exact: true }),
  ).toBeChecked();
  // A model in the region, so no inference profile runs it elsewhere.
  await holds(
    setting,
    "Sent to Amazon Bedrock in ap-southeast-2, under your AWS account.",
  );
  await expect(
    setting.locator(".choice-card").filter({ hasText: "Amazon Bedrock" }),
  ).not.toContainText("inference profile");
  await holds(setting, "amazon.nova-pro-v1:0");
  await expect(page.getByLabel("API key")).toHaveAttribute(
    "placeholder",
    "Leave blank to keep …wxyz",
  );
});

test("gates drafts and uploads until an AI provider is chosen", async ({
  page,
}) => {
  await configure({ aiDefault: false, aiProbeMs: 600 });
  // Generate says why it can't, and where to choose one.
  await page.goto("/ontology/ont_acme_contracts/generate");
  const gate = page.getByRole("note").filter({
    hasText: "Generating an ontology needs an AI provider",
  });
  await expect(gate).toContainText(
    "Choose where Foundry’s AI runs in Settings › AI models.",
  );
  await expect(
    page.getByRole("button", { name: "Generate", exact: true }),
  ).toBeDisabled();

  // Uploads are refused too: any document may be read by a model.
  await page.goto("/data");
  const chooser = page.waitForEvent("filechooser");
  await explorer(page)
    .getByRole("button", { name: "Upload documents" })
    .click();
  await (await chooser).setFiles([file("notes.md", "# Notes")]);
  await expect(
    page.getByRole("note").filter({
      hasText: "Uploading documents needs an AI provider",
    }),
  ).toBeVisible();

  // A new ontology is still created; its draft waits, and its page says so.
  await startOntology(page, "Returns");
  await page.getByRole("radio", { name: "Create a new one" }).click();
  await page.getByLabel("Database name").fill("Returns");
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("button", { name: "Skip" }).click();
  await expect(
    page.getByRole("note").filter({
      hasText: "Its first draft needs an AI provider",
    }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await expect(page).toHaveURL(/\/ontology\/ont_\w+$/);
  await expect(
    page.getByText("Returns is created, but its draft didn’t start"),
  ).toBeVisible();
  await expect(
    page.getByText(
      "No AI provider is set up. An owner can choose one in Settings › AI models. What you chose is kept below",
    ),
  ).toBeVisible();
  await expect(
    page.getByRole("link", { name: "Settings › AI models" }),
  ).toHaveAttribute("href", "/settings/ai");

  await page.goto("/ontology/ont_acme_contracts/generate");
  await gate.getByRole("link", { name: "Choose a provider" }).click();
  await expect(page).toHaveURL(/\/settings\/ai$/);
  await expect(
    page.getByText("No AI provider was found.", { exact: false }).first(),
  ).toBeVisible();
  await expect(
    page.getByRole("radio", { name: "Install default" }),
  ).toHaveCount(0);
  // Ollama comes first; the owner chooses Claude.
  await expect(
    page.getByRole("radio", { name: "Ollama", exact: true }),
  ).toBeChecked();
  await page.getByRole("radio", { name: "Claude", exact: true }).check();
  await page.getByLabel("API key").fill("sk-ant-api03-local-0123456789wxyz");
  // Nothing ran before, so nothing to confirm switching from.
  await page.getByRole("button", { name: "Test and save" }).click();
  await expect(page.getByText("AI models now run on Claude.")).toBeVisible();

  await page.goto("/ontology/ont_acme_contracts/generate");
  await expect(gate).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Generate", exact: true }),
  ).toBeEnabled();
});

test("asks again once a provider is chosen after a refusal, and the gate goes", async ({
  page,
  context,
}) => {
  await configure({ loadStepMs: 200, askStepMs: 100, aiProbeMs: 300 });
  await loadContract(page);
  await page.goto("/ask/ont_acme_contracts");
  const first = page.getByRole("form", { name: "Ask a question" });
  await first
    .getByRole("textbox", { name: /^Question/ })
    .fill("Who signed the services agreement?");
  await first.getByRole("button", { name: "Ask", exact: true }).click();
  await expect(page).toHaveURL(/\?thread=q_/);
  await expect(page.getByRole("region", { name: "Answer" })).toBeVisible({
    timeout: 15_000,
  });

  // The install's provider goes: the follow-up is refused, and the
  // setting read again says there is none.
  await configure({ aiDefault: false });
  const follow = page.getByRole("form", { name: "Ask a follow-up" }),
    send = follow.getByRole("button", { name: "Ask", exact: true });
  await follow
    .getByRole("textbox", { name: /^Question/ })
    .fill("And what do they pay?");
  await send.click();
  const gate = page.getByRole("note").filter({
    hasText: "Asking questions needs an AI provider",
  });
  await expect(gate).toBeVisible();
  await expect(send).toBeDisabled();

  // The owner chooses Claude in another tab.
  const other = await context.newPage();
  await other.goto("/settings/ai");
  await other.getByRole("radio", { name: "Claude", exact: true }).check();
  await other.getByLabel("API key").fill("sk-ant-api03-local-0123456789wxyz");
  await other.getByRole("button", { name: "Test and save" }).click();
  await expect(other.getByText("AI models now run on Claude.")).toBeVisible();
  await other.close();

  // This tab reads the setting again once it is shown, and asks; the gate
  // goes with the refusal.
  await expect(async () => {
    await page.evaluate(() =>
      window.dispatchEvent(new Event("visibilitychange")),
    );
    await expect(send).toBeEnabled({ timeout: 1000 });
  }).toPass({ timeout: 20_000 });
  await send.click();
  await expect(
    page.getByRole("article", { name: "Question: And what do they pay?" }),
  ).toBeVisible();
  await expect(gate).toHaveCount(0);
});

test("says so when the API refuses the site's token", async ({ page }) => {
  await configure({ refuseLocalToken: true });
  await page.goto("/data");
  await expect(page.locator("p[role=alert]")).toHaveText(
    "The Foundry API refused this site’s local token. Start both from the same install, with docker compose up -d.",
  );
  await expect(page).toHaveURL(/\/data$/);
  await expect(page.getByRole("button", { name: "Sign out" })).toHaveCount(0);

  await configure({ refuseLocalToken: false });
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Connectors" }),
  ).toBeVisible();
});

test("names the address it answers when opened on another", async ({
  page,
}) => {
  await page.goto(`http://127.0.0.1:${e2eLocalPorts.app}/`);
  await expect(page.locator("p[role=alert]")).toHaveText(
    `Open Foundry at http://localhost:${e2eLocalPorts.app}: it answers no other address.`,
  );
});

test("answers only the host it was given", async ({ playwright }) => {
  const other = await playwright.request.newContext({
    baseURL: `http://127.0.0.1:${e2eLocalPorts.app}`,
  });
  const res = await other.get("/papi/v1/whoami");
  expect(res.status()).toBe(403);
  expect(await res.json()).toMatchObject({ code: "HOST_REJECTED" });
  await other.dispose();
});
