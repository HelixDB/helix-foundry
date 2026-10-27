import { expect, test, type Page } from "@playwright/test";
import { choose, startOntology, uploadCard } from "../e2e/support/mock";

// The real local stack, from a fresh install: the first workspace, a
// PostgreSQL connection synced into the store from its onboarding, and an
// ontology on a new database in the local HelixDB. Without an AI provider
// (run.sh's default), uploads and the draft wait for one.
// FOUNDRY_STACK_MODELS=1 (Claude as the install's provider, with the shell's
// ANTHROPIC_API_KEY) PUTs a document from the browser straight to the store,
// drafts from both, waits for the agent's draft, loads it, reads its records
// and asks about it.
// FOUNDRY_STACK_OLLAMA=1 (run.sh passes its model as
// FOUNDRY_STACK_OLLAMA_MODEL) chooses Ollama in Docker in Settings › AI
// models with no provider before, passes its test, then runs the same
// upload, draft, load, document read and question on it; with
// FOUNDRY_STACK_MODELS=1 too, Claude drafts and Ollama reads the document
// again on a second load.

test.describe.configure({ mode: "serial" });

const claude = !!process.env.FOUNDRY_STACK_MODELS,
  ollama = process.env.FOUNDRY_STACK_OLLAMA_MODEL || "";
const models = claude || !!ollama;

const dialog = (page: Page) => page.locator("dialog[open]");
// The new ontology's overview, and its first generation's page.
let ontologyPath = "",
  generationPath = "";

// The first workspace opens on its onboarding, the first ontology's wizard,
// which is all it offers until it has an ontology: its sources step connects
// the database and uploads the document. Each test opens the wizard afresh,
// on the new database, at that step.
async function toSources(page: Page) {
  await startOntology(page, "Shop");
  await page.getByRole("radio", { name: "Create a new one" }).click();
  await page.getByLabel("Project name").fill("Foundry");
  await page.getByLabel("Database name").fill("Shop");
  await expect(
    page.getByText(
      "It is kept in the Helix that runs with Foundry on this computer.",
    ),
  ).toBeVisible();
  await page.getByRole("button", { name: "Continue" }).click();
}

test("creates the first workspace", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("button", { name: "Create workspace" }).click();
  const create = page.getByRole("dialog", { name: "Create a workspace" });
  await create.getByLabel("Workspace name").fill("Acme");
  await create.getByRole("button", { name: "Create workspace" }).click();
  await expect(page).toHaveURL(/\/ontology\/new$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Create your first ontology" }),
  ).toBeVisible();
});

test("connects and syncs a PostgreSQL database", async ({ page }) => {
  await toSources(page);
  await page.getByRole("button", { name: "Add data" }).first().click();
  await dialog(page)
    .getByRole("button", { name: "PostgreSQL", exact: true })
    .click();
  const form = dialog(page);
  await form.getByLabel("Host", { exact: true }).fill("sample-db");
  await form.getByLabel("Database", { exact: true }).fill("shop");
  await form.getByLabel("Username", { exact: true }).fill("foundry");
  await form
    .getByLabel("Password", { exact: true })
    .fill("stack-only-sample-password");
  await form.getByText("Connection options", { exact: true }).click();
  await choose(form.getByRole("combobox", { name: "Encryption" }), "No TLS");
  await form.getByRole("button", { name: "Test connection" }).click();
  await expect(form.getByText("Choose tables to sync")).toBeVisible();
  await form
    .getByRole("button", { name: "Connect and sync every table" })
    .click();
  // The wizard keeps the connection, chosen once it has synced.
  await expect(dialog(page)).toHaveCount(0);
  const shop = page
    .getByRole("list", { name: "Connectors" })
    .getByRole("checkbox")
    .first();
  await expect(shop).toBeEnabled({ timeout: 5 * 60_000 });
  await expect(shop).toBeChecked();
  await page.getByRole("button", { name: "Show shop’s tables" }).click();
  const tables = page.getByRole("list", { name: "shop tables" });
  for (const table of ["customers", "orders", "order_items", "products"])
    await expect(tables.getByText(table, { exact: true })).toBeVisible();
});

test("waits for an AI provider to upload or draft", async ({ page }) => {
  test.skip(claude, "the stack has an AI provider");
  await toSources(page);
  const upload = page.getByRole("button", { name: "Upload documents" });
  await expect(upload).toBeEnabled();
  const chooser = page.waitForEvent("filechooser");
  await upload.click();
  await (
    await chooser
  ).setFiles({
    name: "returns.md",
    mimeType: "text/markdown",
    buffer: Buffer.from("# Returns policy\n"),
  });
  await expect(
    page.getByRole("note").filter({
      hasText: "Uploading documents needs an AI provider",
    }),
  ).toBeVisible();
});

// Settings › AI models' Ollama card against the real probe on the bundled
// server: tool calls with Foundry's real tools, a structured answer and a
// title, on a CPU as much as a GPU (the install allows 15 minutes).
async function chooseOllama(page: Page, confirm: boolean) {
  await page.goto("/settings/ai");
  await page.getByRole("radio", { name: "Ollama", exact: true }).check();
  await expect(page.getByRole("combobox", { name: "Server" })).toContainText(
    "Bundled",
  );
  await page
    .getByRole("group", { name: "Model", exact: true })
    .getByRole("textbox")
    .fill(ollama);
  await page.getByRole("button", { name: "Test and save" }).click();
  if (confirm)
    await page
      .getByRole("dialog", { name: "Change where AI runs?" })
      .getByRole("button", { name: "Test and save" })
      .click();
  const passed = page.getByText("AI models now run on Ollama.");
  await expect(
    passed.or(page.getByRole("alert").filter({ hasText: "stay active" })),
  ).toBeVisible({ timeout: 16 * 60_000 });
  await expect(passed).toBeVisible();
  await expect(
    page.getByText("Runs on this machine; nothing leaves it."),
  ).toBeVisible();
}

// The upload, as an ontology reads it: by Ollama's model, when it ran.
async function readBy(page: Page, model: string) {
  await page.goto(`${ontologyPath}/documents`);
  await page.getByRole("link", { name: "returns.md" }).first().click();
  await page.getByRole("tab", { name: /^Details/ }).click();
  await expect(page.getByText(model, { exact: true })).toBeVisible();
}

test("runs AI on Ollama in Docker once its test passes", async ({ page }) => {
  test.skip(!ollama || claude, "set FOUNDRY_STACK_OLLAMA=1 alone");
  test.setTimeout(20 * 60_000);
  await chooseOllama(page, false);
});

test("uploads a document straight to the store", async ({ page }) => {
  test.skip(
    !models,
    "uploads need an AI provider: FOUNDRY_STACK_MODELS=1 or FOUNDRY_STACK_OLLAMA=1",
  );
  await toSources(page);
  // The chooser is there before the folders are read, and drops files until
  // they are: the button waits for them.
  const upload = page.getByRole("button", { name: "Upload documents" });
  await expect(upload).toBeEnabled();
  const chooser = page.waitForEvent("filechooser");
  await upload.click();
  await (
    await chooser
  ).setFiles({
    name: "returns.md",
    mimeType: "text/markdown",
    buffer: Buffer.from(
      "# Returns policy\n\nAcme accepts returns within 30 days of delivery. " +
        "Grace Hopper's order of two 27-inch monitors was returned on 2026-09-20.\n",
    ),
  });
  await expect(uploadCard(page, "returns.md")).toContainText("Uploaded", {
    timeout: 60_000,
  });
});

test("creates an ontology on a new database in the local HelixDB", async ({
  page,
}) => {
  await toSources(page);
  // The synced database is chosen already; the upload's folder is not.
  await expect(
    page
      .getByRole("list", { name: "Connectors" })
      .getByRole("checkbox")
      .first(),
  ).toBeChecked();
  if (models)
    await page
      .getByRole("list", { name: "Document sets" })
      .getByRole("checkbox")
      .first()
      .check();
  await page.getByRole("button", { name: "Continue" }).click();
  await page
    .getByLabel("Goal")
    .fill("Customers, the orders they place and the products in them");
  await page.getByRole("button", { name: "Continue" }).click();
  if (!models) {
    // The ontology and its database; its draft waits for an AI provider.
    await page.getByRole("button", { name: "Create", exact: true }).click();
    await expect(page).toHaveURL(/\/ontology\/[^/]+$/);
    await expect(page.getByText("No AI provider is set up.")).toBeVisible();
    ontologyPath = new URL(page.url()).pathname;
    // With an ontology the workspace opens on Ask, about it, which sends
    // the owner to finish setting it up.
    await page.goto("/");
    await expect(page).toHaveURL(
      new RegExp(`${ontologyPath.replace(/^\/ontology\//, "/ask/")}$`),
    );
    await expect(
      page.getByRole("main").getByRole("link", { name: "Finish setting up" }),
    ).toBeVisible();
    return;
  }
  // The ontology, its database and its first generation, which needs the
  // agent's model to succeed.
  await page.getByRole("button", { name: "Create and generate" }).click();
  await expect(page).toHaveURL(/\/ontology\/[^/]+\/generations\/[^/]+$/);
  generationPath = new URL(page.url()).pathname;
  ontologyPath = generationPath.replace(/\/generations\/.*$/, "");
});

test("queries the new database in the local HelixDB", async ({ page }) => {
  await page.goto(ontologyPath);
  const origin = new URL(page.url()).origin;
  const papi = (path: string) => `${origin}/papi${path}`;
  const { workspaces } = await (
    await page.request.get(papi("/v1/workspaces"))
  ).json();
  const ontologyId = ontologyPath.split("/").pop()!;
  const ontology = await (
    await page.request.get(
      papi(
        `/v1/workspaces/${workspaces[0].id}/foundry/ontologies/${ontologyId}`,
      ),
    )
  ).json();
  const tenantId = ontology.ontology?.tenantId ?? ontology.tenantId;
  expect(tenantId).toMatch(/^[0-9A-Z]{26}$/);
  const { token } = await (await page.request.get(papi("/csrf"))).json();
  // The Explorer's kind of read: Customers, of which nothing is loaded yet.
  const query = {
    request_type: "read",
    query_name: "stack_probe",
    query: {
      read: {
        entries: [
          {
            query: {
              name: "rows",
              root: {
                values: {
                  input: {
                    limit: {
                      input: {
                        nodes_where: {
                          predicate: {
                            eq: {
                              left: { property: "$label" },
                              right: { constant: { string: "Customer" } },
                            },
                          },
                        },
                      },
                      count: { literal: 1 },
                    },
                  },
                  properties: ["name"],
                },
              },
            },
          },
        ],
        returns: ["rows"],
      },
    },
    parameters: {},
    parameter_types: {},
  };
  const res = await page.request.post(papi("/v1/databases:query-read"), {
    headers: { origin, "x-helix-csrf": token },
    // bytes in the WFE's JSON: base64.
    data: {
      database: { tenantId },
      queryJson: Buffer.from(JSON.stringify(query)).toString("base64"),
    },
  });
  const body = await res.json();
  expect(res.status(), JSON.stringify(body)).toBe(200);
  const answer = Buffer.from(body.responseJson ?? "", "base64").toString();
  expect(body.statusCode, answer).toBe(200);
  // Nothing is loaded yet: no rows (HelixDB writes an empty result as null).
  expect(JSON.parse(answer).rows ?? []).toEqual([]);
});

// The rest calls Claude (FOUNDRY_STACK_MODELS=1, with ANTHROPIC_API_KEY in
// the shell) or Ollama through the ontology agent. A draft takes up to 20
// minutes on Claude, and up to the install's 40-minute cap on Ollama.
test.describe("with models", () => {
  test.skip(
    !models,
    "set FOUNDRY_STACK_MODELS=1, with ANTHROPIC_API_KEY in the shell, or FOUNDRY_STACK_OLLAMA=1",
  );
  const onOllama = !!ollama && !claude;
  const done = (page: Page, ok: string) =>
    page
      .getByText(ok, { exact: true })
      .or(page.getByText("Failed", { exact: true }))
      .first();

  test("drafts the ontology with the agent and makes it current", async ({
    page,
  }) => {
    test.setTimeout((onOllama ? 50 : 30) * 60_000);
    // The wizard started it.
    await page.goto(generationPath);
    await expect(done(page, "Succeeded")).toBeVisible({
      timeout: (onOllama ? 45 : 25) * 60_000,
    });
    await expect(done(page, "Succeeded")).toHaveText("Succeeded");
    await page.getByRole("link", { name: /^Review version \d+$/ }).click();
    await page.getByRole("button", { name: "Make current" }).click();
    await page
      .getByRole("dialog", { name: /^Make version \d+ current\?$/ })
      .getByRole("button", { name: "Make current" })
      .click();
    await expect(
      page.getByText(/^Version \d+ is now current\.$/),
    ).toBeVisible();
  });

  test("loads it into HelixDB, the document included", async ({ page }) => {
    await page.goto(ontologyPath);
    await page
      .getByRole("status")
      .filter({ hasText: /isn’t loaded yet/ })
      .getByRole("button", { name: "Load data" })
      .click();
    await page
      .getByRole("dialog", { name: /^Load version \d+ into / })
      .getByRole("button", { name: "Load" })
      .click();
    await expect(page).toHaveURL(/\/loads\/[0-9A-Z]{26}$/);
    const loaded = page
      .getByRole("status")
      .filter({ hasText: /^(Loaded|Failed)/ })
      .first();
    await expect(loaded).toBeVisible({ timeout: 20 * 60_000 });
    await expect(loaded).toContainText("Loaded");
    if (onOllama) await readBy(page, ollama);
  });

  test("reads the loaded records back from HelixDB", async ({ page }) => {
    await page.goto(
      ontologyPath.replace("/ontology/", "/explore/") + "/records",
    );
    await expect(page.getByText("Nothing loaded yet")).toHaveCount(0);
    await expect(
      page.getByRole("list", { name: "Types" }).getByRole("button").first(),
    ).toBeVisible();
    await expect(page.getByRole("grid").getByRole("row").nth(1)).toBeVisible();
  });

  test("answers a question about the load", async ({ page }) => {
    await page.goto(ontologyPath);
    await page.getByRole("link", { name: "Ask", exact: true }).click();
    const form = page.getByRole("form", { name: "Ask a question" });
    const question = "Which customer has placed the most orders?";
    await form.getByRole("textbox", { name: /^Question/ }).fill(question);
    await form.getByRole("button", { name: "Ask", exact: true }).click();
    const answer = page
      .getByRole("article", { name: `Question: ${question}` })
      .getByRole("region", { name: "Answer" });
    // Customer 1, Ada Lovelace, with two orders: the answer may name her or
    // her number, as the draft names customers. A small local model's
    // answer is only read for being there.
    await expect(answer).toContainText(
      onOllama ? /\S/ : /Ada|customer (number |#)?1\b/i,
      { timeout: (onOllama ? 15 : 5) * 60_000 },
    );
  });

  // Settings › AI models against the real probe: a key Anthropic refuses
  // fails its test and the install default keeps running; the workspace's
  // own key passes it, then answers questions; and back to the default.
  async function bringClaude(page: Page, key: string) {
    await page.goto("/settings/ai");
    await page.getByRole("radio", { name: "Claude", exact: true }).check();
    await page.getByLabel("API key").fill(key);
    await page.getByRole("button", { name: "Test and save" }).click();
    await page
      .getByRole("dialog", { name: "Change where AI runs?" })
      .getByRole("button", { name: "Test and save" })
      .click();
  }

  test("keeps the install default when Anthropic refuses a key", async ({
    page,
  }) => {
    test.skip(!claude, "Claude is the install's provider");
    await bringClaude(page, "sk-ant-api03-stack-refused-0123456789abcdefghij");
    await expect(
      page.getByRole("alert").filter({ hasText: "refused the API key" }),
    ).toBeVisible({ timeout: 3 * 60_000 });
    await page.reload();
    await expect(
      page.getByRole("radio", { name: "Install default", exact: true }),
    ).toBeChecked();
  });

  test("asks on the workspace's own Claude key, then goes back to the default", async ({
    page,
  }) => {
    const key = process.env.FOUNDRY_STACK_OWN_KEY;
    test.skip(
      !claude || !key,
      "run.sh passes ANTHROPIC_API_KEY as the workspace's key",
    );
    await bringClaude(page, key!);
    await expect(page.getByText("AI models now run on Claude.")).toBeVisible({
      timeout: 3 * 60_000,
    });
    await expect(
      page.getByRole("radio", { name: "Claude", exact: true }),
    ).toBeChecked();

    await page.goto(ontologyPath);
    await page.getByRole("link", { name: "Ask", exact: true }).click();
    const form = page.getByRole("form", { name: "Ask a question" });
    const question = "How many orders were cancelled?";
    await form.getByRole("textbox", { name: /^Question/ }).fill(question);
    await form.getByRole("button", { name: "Ask", exact: true }).click();
    await expect(
      page
        .getByRole("article", { name: `Question: ${question}` })
        .getByRole("region", { name: "Answer" }),
    ).toContainText(/\b(1|one)\b/i, { timeout: 5 * 60_000 });

    await page.goto("/settings/ai");
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

  // Claude drafted and read the document; Ollama reads it again on a new
  // load, its reading models being others.
  test("reads the document again on Ollama, on a new load", async ({
    page,
  }) => {
    test.skip(
      !claude || !ollama,
      "set FOUNDRY_STACK_MODELS=1 and FOUNDRY_STACK_OLLAMA=1",
    );
    test.setTimeout(40 * 60_000);
    await chooseOllama(page, true);
    await page.goto(ontologyPath);
    await page
      .locator(".hf-page-header")
      .getByRole("button", { name: "Load data" })
      .click();
    await page
      .getByRole("dialog", { name: /^Load version \d+ into / })
      .getByRole("button", { name: "Load" })
      .click();
    await expect(page).toHaveURL(/\/loads\/[0-9A-Z]{26}$/);
    const loaded = page
      .getByRole("status")
      .filter({ hasText: /^(Loaded|Failed)/ })
      .first();
    await expect(loaded).toBeVisible({ timeout: 30 * 60_000 });
    await expect(loaded).toContainText("Loaded");
    await readBy(page, ollama);
  });
});
