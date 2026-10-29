import type { Locator, Page } from "@playwright/test";
import type { FoundryDocumentSet } from "@/shared/wfe";
import {
  choose,
  createdOntology,
  csrfToken,
  dataTree as tree,
  expect,
  mock,
  signIn,
  signInToNewWorkspace,
  startFirstOntology,
  test,
} from "./support/mock";
import {
  eventSourceCurl,
  eventSourcePiecesExample,
} from "../../src/web/areas/data/event-source-prompt";

// API sources (connector kind events): made in the onboarding wizard's
// sources step and in the Data area, their send tokens (shown once) and the
// prompt they copy, their pieces arriving without a reload, the MCP's token
// link, Generate reading one beside a connector, and a member's view. Then
// entity extraction: a source's text field, chosen on its page or in place
// in the sources step, makes its newest pieces documents of its own set
// (Pieces), which a generation reads in mode extraction, its month's spend
// against its allowance, and the pause and hold once that is spent. The
// site never sends pieces: mock.sendPieces stands in for an agent's send
// through the Helix MCP, refused with the backend's messages. The mock build
// is dev's, so it shows how to send over HTTP too (the mock doesn't serve
// the endpoint): the curl and pieces.json beside each send panel.

const STRUCTURED = "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED",
  EXTRACTION = "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION";
const MAPPINGS =
  "Fields map to types and properties. Every piece is kept, and arrives when the ontology refreshes.";
const EXTRACTS =
  "Foundry’s agent reads each piece’s text as it arrives. The graph keeps the newest 500. About 1 cent a piece for each ontology that reads it, up to $5 a month for this source; past that, new pieces wait for next month.";
const REREAD =
  "Foundry re-reads this API source’s newest 500 pieces. Extracting them comes out of its $5 for the month.";
const PIECES = [
  { id: "line-1", data: { sku: "A1", qty: 2, price: 9.5 } },
  { id: "line-2", data: { sku: "B2", qty: 1, price: 20 } },
  { id: "line-3", data: { sku: "C3", qty: 5, price: 1.25 } },
];
// Pieces whose body is text: two string fields either could hold.
const REPLIES = [
  {
    id: "t-1",
    data: { reply: "Refund sent for order 12.", body: "Order 12 refunded." },
  },
  {
    id: "t-2",
    data: { reply: "Your plan renews on 1 December.", body: "Renewal set." },
  },
  {
    id: "t-3",
    data: {
      reply: "You are on the annual plan now.",
      body: "Moved to annual.",
    },
  },
];
const O = "/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts";
// NEXT_PUBLIC_HELIX_EVENTS_URL of the mock build: next.config.ts's dev one.
const EVENTS_URL = "https://ingest.dev.helix-db.com";

/** The signed-in user's first workspace's id. */
const workspaceId = async (page: Page): Promise<string> =>
  (await (await page.request.get("/papi/v1/workspaces")).json()).workspaces[0]
    .id;

/** A POST to the WFE through the BFF, as the app makes it; its JSON. */
async function post(page: Page, path: string, data: object) {
  const res = await page.request.post(`/papi${path}`, {
    data,
    headers: {
      origin: new URL(page.url()).origin,
      "x-helix-csrf": await csrfToken(page),
    },
  });
  expect(res.status(), await res.text()).toBe(200);
  return res.json();
}
const get = async (page: Page, path: string) =>
  (await page.request.get(`/papi${path}`)).json();

/**
 * Makes an API source through the BFF, as its form does, with `textField`
 * if given, and returns its id. `pieces` are sent to it, and their first
 * sync is waited for.
 */
async function apiSource(
  page: Page,
  ws: string,
  name: string,
  pieces: { id: string; data: object }[] = [],
  textField?: string,
): Promise<string> {
  const id: string = (
    await post(page, `/v1/workspaces/${ws}/foundry/connectors`, {
      name,
      config: {
        events: { description: "", ...(textField ? { textField } : {}) },
      },
    })
  ).connector.id;
  if (!pieces.length) return id;
  expect((await mock.sendPieces(id, { pieces })).status).toBe(200);
  await expect
    .poll(
      async () =>
        (
          await (
            await page.request.get(
              `/papi/v1/workspaces/${ws}/foundry/connectors/${id}`,
            )
          ).json()
        ).connector.tables?.find((t: { name: string }) => t.name === "events")
          ?.rowCount,
      { timeout: 15_000 },
    )
    .toBe(String(pieces.length));
  return id;
}

/**
 * Waits for an API source's own set (Pieces) to hold `n` documents, as its
 * syncs write them, and returns its id.
 */
async function piecesOf(
  page: Page,
  ws: string,
  source: string,
  n: number,
): Promise<string> {
  const set = async () =>
    (
      (await get(page, `/v1/workspaces/${ws}/foundry/document-sets`))
        .sets as FoundryDocumentSet[]
    ).find((s) => s.connector?.connectorId === source);
  await expect
    .poll(async () => (await set())?.documents, { timeout: 15_000 })
    .toBe(n);
  return (await set())!.id;
}

/**
 * Drafts a version of Acme's Customer contracts through the BFF, reading
 * `source` by extraction alone, and makes it current: its id.
 */
async function currentFrom(page: Page, source: string): Promise<string> {
  const { generation } = await post(page, `${O}:generate`, {
    goal: "Support replies",
    noConnectors: true,
    eventSources: [{ connectorId: source, mode: EXTRACTION }],
  });
  let versionId = "";
  await expect
    .poll(
      async () => {
        const g = (await get(page, `${O}/generations/${generation.id}`))
          .generation;
        versionId = g.resultVersionId || "";
        return g.status;
      },
      { timeout: 15_000 },
    )
    .toBe("ONTOLOGY_GENERATION_STATUS_SUCCEEDED");
  const { ontology } = await get(page, O);
  await post(page, `${O}:set-current`, {
    versionId,
    expectedCurrentVersionId: ontology.currentVersionId,
  });
  return versionId;
}

/** The 1st of next month (UTC), as a paused set says it: "November 1". */
const nextMonth = () => {
  const at = new Date();
  return new Date(
    Date.UTC(at.getUTCFullYear(), at.getUTCMonth() + 1, 1),
  ).toLocaleDateString("en-US", {
    month: "long",
    day: "numeric",
    timeZone: "UTC",
  });
};

/** A value of an API source page's Details, by its label. */
const detail = (page: Page, label: string) =>
  page
    .locator(".hf-kv-row")
    .filter({ has: page.getByRole("term").getByText(label, { exact: true }) })
    .getByRole("definition");

/** An API source's row in the ontology's sources step. */
const sourceRow = (page: Page, name: string) =>
  page
    .getByRole("list", { name: "API sources" })
    .getByRole("listitem")
    .filter({ hasText: name });
const sourceBox = (row: Locator) => row.getByRole("checkbox");

test("makes an API source in the onboarding wizard and drafts from its pieces", async ({
  page,
  context,
}) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const clipboard = () => page.evaluate(() => navigator.clipboard.readText());
  // Long enough to see its pieces received before they sync.
  await mock.configure({ eventSyncMs: 6000 });
  await signInToNewWorkspace(page);
  await startFirstOntology(page);
  await expect(
    page.getByRole("heading", { name: "No API sources yet" }),
  ).toBeVisible();

  // New API source opens the Data area's form over the wizard.
  await page.getByRole("button", { name: "New API source" }).click();
  const form = page.getByRole("dialog", { name: "API source" });
  await form.getByLabel("Name").fill("Order lines");
  await form
    .getByLabel("Description (optional)")
    .fill("One line of a customer’s order.");
  const created = page.waitForResponse(
    (r) =>
      r.request().method() === "POST" &&
      new URL(r.url()).pathname.endsWith("/foundry/connectors"),
  );
  await form.getByRole("button", { name: "Create API source" }).click();
  const source: string = (await (await created).json()).connector.id;
  await expect(form).toContainText("Order lines is ready.");

  // A send token shows once, with what it allows; Done leaves its prefix.
  await form.getByRole("button", { name: "Create a send token" }).click();
  const made = form.getByRole("group", { name: "New send token" });
  // A password field: its value is copied, never shown.
  const secret = await made.locator("input").inputValue();
  expect(secret).toMatch(/^hx_ev_[0-9a-f]{64}$/);
  await expect(made).toContainText(
    "Anyone with this token can add pieces to this source and replace any piece in it by its id.",
  );
  await expect(made).toContainText(
    "Put it in your app’s or agent’s environment as HELIX_EVENT_TOKEN, never in a chat.",
  );
  await made.getByRole("button", { name: "Done" }).click();
  await expect(made).toHaveCount(0);
  const tokens = form.getByRole("grid", { name: "Send tokens" });
  await expect(tokens).toContainText(`${secret.slice(0, 12)}…`);
  await expect(form.locator("input[readonly]")).toHaveCount(0);

  // Its prompt names the source and the token's variable, never a token.
  await form.getByRole("button", { name: "Copy prompt" }).click();
  await expect(page.getByText("Copied the prompt")).toBeVisible();
  await expect.poll(clipboard).toContain(source);
  const prompt = await clipboard();
  expect(prompt).toContain("helix_foundry_send_pieces");
  expect(prompt).toContain("${HELIX_EVENT_TOKEN}");
  expect(prompt).not.toContain("hx_ev_");
  // With how to send over HTTP, the panel's own curl.
  const curl = eventSourceCurl({ eventsUrl: EVENTS_URL, sourceId: source });
  expect(prompt).toContain(`## Or send over HTTP\n`);
  expect(prompt).toContain(`\n${curl}\n`);
  await expect(
    form
      .getByRole("region", { name: "Send over HTTP" })
      .getByRole("figure", { name: "curl command" })
      .locator("pre"),
  ).toHaveText(curl);

  // Closed without Done, the source is still the step's.
  await form.getByRole("button", { name: "Close" }).click();
  await expect(form).toHaveCount(0);
  const row = sourceRow(page, "Order lines");
  await expect(row).toContainText("Waiting for its first pieces");
  await expect(sourceBox(row)).toBeDisabled();
  await expect(page.getByRole("button", { name: "Continue" })).toBeDisabled();

  // Pieces an agent sends reach the row without a reload, and once synced
  // the source is chosen, read through mappings.
  expect(
    (
      await mock.sendPieces(source, {
        clientToken: crypto.randomUUID(),
        pieces: PIECES,
      })
    ).status,
  ).toBe(200);
  await expect(row).toContainText(
    "3 pieces received; its first sync is about a minute away",
    { timeout: 15_000 },
  );
  await expect(row).toContainText(/3 pieces, synced/, { timeout: 15_000 });
  await expect(sourceBox(row)).toBeChecked();
  await expect(row).toContainText(MAPPINGS);

  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("button", { name: "Skip" }).click();
  await expect(
    page.getByText("Order lines (mappings)", { exact: true }),
  ).toBeVisible();
  const sent = page.waitForRequest(/:generate$/);
  await page.getByRole("button", { name: "Create and generate" }).click();
  const body = (await sent).postDataJSON();
  expect(body).toMatchObject({
    noConnectors: true,
    eventSources: [{ connectorId: source, mode: STRUCTURED }],
    documentSetIds: [],
  });
  expect(body.connectorIds).toBeUndefined();
  await createdOntology(page);
});

test("opens the MCP's token link on the onboarding wizard's send panel", async ({
  page,
  context,
}) => {
  await signInToNewWorkspace(page);
  const ws = await workspaceId(page);
  const source = await apiSource(page, ws, "Order lines");

  // The link create_event_source returns, in a new tab: the workspace has
  // no Data pages yet, so it opens the wizard with the source's send panel.
  const tab = await context.newPage();
  await tab.goto(`/data/connections/${source}?token=new&workspace=${ws}`);
  await expect(tab).toHaveURL(/\/ontology\/new$/);
  const dialog = tab.getByRole("dialog", {
    name: "Send pieces to Order lines",
  });
  const create = dialog.getByRole("button", { name: "Create a send token" });
  await expect(create).toBeFocused();
  await create.click();
  await expect(
    dialog.getByRole("group", { name: "New send token" }).locator("input"),
  ).toHaveValue(/^hx_ev_/);
  await dialog.getByRole("button", { name: "Close" }).click();
  await expect(dialog).toHaveCount(0);
  // Over the first ontology's sources step, past Get started.
  await expect(
    tab.getByRole("heading", { level: 1, name: "Your first ontology" }),
  ).toBeVisible();
  await expect(tab).toHaveURL(/\/ontology\/new$/);

  // A link to a source that is gone says so.
  await tab.goto(`/data/connections/conn_gone?token=new`);
  await expect(
    tab.getByRole("dialog", { name: "Send pieces" }).getByRole("alert"),
  ).toHaveText("This API source is gone.");
});

test("shows how to send over HTTP: the picker, the palette, and a source's curl and pieces.json", async ({
  page,
  context,
}) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const clipboard = () => page.evaluate(() => navigator.clipboard.readText());
  await signIn(page);
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();

  // The picker and the palette say an app may send as well as an agent.
  await page.goto("/data");
  await page.getByRole("button", { name: "Add data" }).first().click();
  const picker = page.getByRole("dialog", { name: "Where is your data?" });
  await expect(
    picker
      .getByRole("group", { name: "From your app or agent" })
      .getByRole("button", { name: "API source", exact: true }),
  ).toContainText(
    "Send pieces of data from your app or agent, over HTTP or the Helix MCP. Foundry keeps every piece.",
  );
  await picker.getByRole("button", { name: "Close" }).click();
  await page.keyboard.press("ControlOrMeta+k");
  const palette = page.getByRole("dialog", { name: "Search your workspace" });
  await palette.getByRole("combobox").fill("New API source");
  await expect(
    palette.getByRole("option", { name: /New API source/ }),
  ).toContainText("Send pieces of data from your app or agent");
  await page.keyboard.press("Escape");

  // A source's page: the curl names it and reads the token from
  // HELIX_EVENT_TOKEN, and Copy copies exactly that.
  const source = await apiSource(page, "ws_acme", "Order lines");
  await page.goto(`/data/connections/${source}`);
  const send = page.getByRole("region", { name: "Send pieces" });
  await expect(send).toContainText(
    "Apps and agents send pieces over HTTP or the Helix MCP",
  );
  const http = send.getByRole("region", { name: "Send over HTTP" });
  await expect(http).toContainText(
    "For an app or service without an MCP client. Its token goes in HELIX_EVENT_TOKEN on the server; never call this from a browser.",
  );
  const curl = eventSourceCurl({ eventsUrl: EVENTS_URL, sourceId: source });
  expect(curl).toContain(`${EVENTS_URL}/v1/sources/${source}/pieces`);
  expect(curl).toContain('"$HELIX_EVENT_TOKEN"');
  await expect(
    http.getByRole("figure", { name: "curl command" }).locator("pre"),
  ).toHaveText(curl);
  await http.getByRole("button", { name: "Copy curl command" }).click();
  await expect(page.getByText("Copied the curl command.")).toBeVisible();
  await expect.poll(clipboard).toBe(curl);
  expect(await clipboard()).not.toContain("hx_ev_");
  // pieces.json, a file to fill in: data as an object, and a client_token.
  const file = http.getByRole("figure", { name: "pieces.json" });
  await expect(file.locator("pre")).toHaveText(eventSourcePiecesExample());
  await expect(file.getByRole("button")).toHaveCount(0);
  expect(JSON.parse(await file.locator("pre").innerText())).toMatchObject({
    client_token: expect.any(String),
    pieces: [{ data: { "<field>": "<value>" } }],
  });

  // The ?token=new dialog's send panel has it too.
  await page.goto(`/data/connections/${source}?token=new`);
  await expect(
    page
      .getByRole("dialog", { name: "Send pieces to Order lines" })
      .getByRole("region", { name: "Send over HTTP" })
      .getByRole("figure", { name: "curl command" })
      .locator("pre"),
  ).toHaveText(curl);
});

test("manages an API source in the Data area: fields, refusals, tokens and delete", async ({
  page,
}) => {
  await signIn(page);
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  // The palette's New API source opens its form.
  await page.keyboard.press("ControlOrMeta+k");
  const palette = page.getByRole("dialog", { name: "Search your workspace" });
  await palette.getByRole("combobox").fill("New API source");
  await palette.getByRole("option", { name: /New API source/ }).click();
  const form = page.getByRole("dialog", { name: "API source" });
  await form.getByLabel("Name").fill("Order lines");
  const created = page.waitForResponse(
    (r) =>
      r.request().method() === "POST" &&
      new URL(r.url()).pathname.endsWith("/foundry/connectors"),
  );
  await form.getByRole("button", { name: "Create API source" }).click();
  const source: string = (await (await created).json()).connector.id;
  await form.getByRole("button", { name: "Done" }).click();
  await expect(page).toHaveURL(new RegExp(`/data/connections/${source}$`));
  await expect(page.getByText("API source created")).toBeVisible();
  await expect(
    page.getByRole("heading", { level: 1, name: "Order lines" }),
  ).toBeVisible();

  // The tree keeps it in a folder of its own, with no Sync.
  const folder = tree(page).getByRole("treeitem", {
    name: "API sources",
    exact: true,
  });
  const item = folder.getByRole("treeitem", { name: /^Order lines,/ });
  await expect(item).toBeVisible();
  await expect(item.getByRole("button", { name: /^Sync/ })).toHaveCount(0);

  const header = page.locator(".hf-page-header");
  const fields = page.getByRole("grid", { name: "Fields" });
  const field = (name: string) => fields.getByRole("row", { name });
  await expect(header).toContainText("Waiting for its first pieces");
  await expect(
    page.getByRole("heading", { name: "No fields yet" }),
  ).toBeVisible();

  // Its fields are learned from what is sent, each with its first type.
  expect(
    (
      await mock.sendPieces(source, {
        pieces: [{ id: "line-1", data: { sku: "A1", qty: 2 } }],
      })
    ).status,
  ).toBe(200);
  await expect(header).toContainText(/1 piece, synced/, { timeout: 15_000 });
  await expect(field("sku")).toContainText("string");
  await expect(field("qty")).toContainText("number");
  await expect(field("price")).toHaveCount(0);
  expect(
    (
      await mock.sendPieces(source, {
        pieces: [{ id: "line-2", data: { sku: "B2", qty: 1, price: 20 } }],
      })
    ).status,
  ).toBe(200);
  await page.reload();
  await expect(field("price")).toContainText("number");
  await expect(header).toContainText(/2 pieces, synced/, { timeout: 15_000 });
  await expect(
    page.getByRole("list", { name: "Synced tables" }).getByRole("link"),
  ).toContainText("2 rows");

  // A field keeps its type: a send that changes it is refused whole.
  const conflict = await mock.sendPieces(source, {
    pieces: [{ id: "line-3", data: { sku: "C3", qty: "two" } }],
  });
  expect(conflict.status).toBe(400);
  expect(conflict.body.message).toBe(
    'piece 1 (id "line-3"): "qty" is a string; this source holds "qty" as a number',
  );
  // A client token retries its own send, never another.
  const clientToken = "e2e-order-lines-send-0001";
  const first = { clientToken, pieces: [{ id: "line-4", data: { qty: 3 } }] };
  expect((await mock.sendPieces(source, first)).body.replayed).toBe(false);
  expect((await mock.sendPieces(source, first)).body.replayed).toBe(true);
  const reused = await mock.sendPieces(source, {
    clientToken,
    pieces: [{ id: "line-5", data: { qty: 4 } }],
  });
  expect(reused.status).toBe(400);
  expect(reused.body.message).toBe(
    `client_token "${clientToken}" was already used for a different send; use a new client_token for new pieces`,
  );
  // Three sends stored, the replay and the refusals none.
  await page.reload();
  await expect(header).toContainText(/3 pieces, synced/, { timeout: 15_000 });
  await expect(
    page.getByRole("grid", { name: "Recent sends" }).getByRole("row"),
  ).toHaveCount(4);

  // A token made here, then revoked.
  const send = page.getByRole("region", { name: "Send pieces" });
  await send.getByRole("button", { name: "Create a send token" }).click();
  const secret = await send
    .getByRole("group", { name: "New send token" })
    .locator("input")
    .inputValue();
  await send.getByRole("button", { name: "Done" }).click();
  const prefix = `${secret.slice(0, 12)}…`;
  await send.getByRole("button", { name: `Revoke ${prefix}` }).click();
  await page
    .getByRole("dialog", { name: `Revoke ${prefix}?` })
    .getByRole("button", { name: "Revoke token" })
    .click();
  await expect(page.getByText("Send token revoked")).toBeVisible();
  // Listed still, without Revoke.
  await expect(
    send.getByRole("button", { name: `Revoke ${prefix}` }),
  ).toHaveCount(0);
  await expect(send.getByRole("grid", { name: "Send tokens" })).toContainText(
    prefix,
  );

  // Deleting it leaves the Data page without it.
  await page.getByRole("button", { name: "API source actions" }).click();
  await page.getByRole("menuitem", { name: "Delete API source" }).click();
  await page
    .getByRole("dialog", { name: "Delete Order lines?" })
    .getByRole("button", { name: "Delete API source" })
    .click();
  await expect(page).toHaveURL(/\/data$/);
  await expect(page.getByText("API source deleted")).toBeVisible();
  await expect(folder).toHaveCount(0);
});

test("generates from an API source beside Stripe, and Try again keeps it", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 300, ontologyOutcome: "fail" });
  await signIn(page);
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  const source = await apiSource(page, "ws_acme", "Order lines", PIECES);

  await page.goto("/ontology/ont_acme_contracts/generate");
  await page.getByLabel("Goal").fill("Billing health");
  const connectors = page.getByRole("list", { name: "Connectors" });
  await connectors.getByRole("checkbox", { name: /Shop database/ }).uncheck();
  const row = sourceRow(page, "Order lines");
  await expect(row).toContainText(/3 pieces, synced/);
  // Not chosen until checked: the current version reads none.
  await expect(sourceBox(row)).not.toBeChecked();
  await sourceBox(row).check();
  await expect(row).toContainText(MAPPINGS);
  const sent = page.waitForRequest(/ont_acme_contracts:generate$/);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  expect((await sent).postDataJSON()).toEqual({
    goal: "Billing health",
    connectorIds: ["conn_acme_stripe"],
    eventSources: [{ connectorId: source, mode: STRUCTURED }],
    documentSetIds: [],
  });

  await expect(page.getByText("The draft failed")).toBeVisible({
    timeout: 15_000,
  });
  await page.getByRole("link", { name: "Try again" }).click();
  await expect(page.getByLabel("Goal")).toHaveValue("Billing health");
  await expect(
    connectors.getByRole("checkbox", { name: /Stripe/ }),
  ).toBeChecked();
  await expect(
    connectors.getByRole("checkbox", { name: /Shop database/ }),
  ).not.toBeChecked();
  await expect(sourceBox(row)).toBeChecked();
  const again = page.waitForRequest(/ont_acme_contracts:generate$/);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  expect((await again).postDataJSON()).toMatchObject({
    connectorIds: ["conn_acme_stripe"],
    eventSources: [{ connectorId: source, mode: STRUCTURED }],
  });
});

test("shows a member an API source and its tokens without changes", async ({
  page,
  context,
}) => {
  await signIn(page);
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  const source = await apiSource(page, "ws_acme", "Order lines", PIECES);
  const token = await page.request.post(
    `/papi/v1/workspaces/ws_acme/foundry/connectors/${source}/tokens`,
    {
      data: {},
      headers: {
        origin: new URL(page.url()).origin,
        "x-helix-csrf": await csrfToken(page),
      },
    },
  );
  expect(token.status()).toBe(200);
  const { prefix } = (await token.json()).token;

  await context.clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto(`/data/connections/${source}`);
  await expect(
    page.getByRole("heading", { level: 1, name: "Order lines" }),
  ).toBeVisible();
  await expect(page.locator(".hf-page-header")).toContainText(
    /3 pieces, synced/,
  );
  await expect(
    tree(page)
      .getByRole("treeitem", { name: "API sources", exact: true })
      .getByRole("treeitem", { name: /^Order lines,/ }),
  ).toBeVisible();
  const send = page.getByRole("region", { name: "Send pieces" });
  const tokens = send.getByRole("grid", { name: "Send tokens" });
  await expect(
    tokens.getByRole("row", { name: new RegExp(prefix) }),
  ).toContainText("Live");
  await expect(send).toContainText(
    "An owner or admin of the workspace makes send tokens.",
  );
  await expect(
    page.getByRole("button", { name: "Create a send token" }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: /^Revoke hx_ev_/ }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "API source actions" }),
  ).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Add data" })).toHaveCount(0);

  await page.keyboard.press("ControlOrMeta+k");
  const palette = page.getByRole("dialog", { name: "Search your workspace" });
  await palette.getByRole("combobox").fill("New API source");
  await expect(
    palette.getByRole("option", { name: /New API source/ }),
  ).toHaveCount(0);
});

test("chooses an API source's text field in the onboarding wizard, and drafts from its pieces by extraction", async ({
  page,
}) => {
  await signInToNewWorkspace(page);
  const ws = await workspaceId(page);
  const source = await apiSource(page, ws, "Replies", REPLIES);
  await startFirstOntology(page);
  const row = sourceRow(page, "Replies");
  await expect(row).toContainText(/3 pieces, synced/);

  // Without a text field it starts as mappings; extraction can't be chosen.
  const modes = row.getByRole("radiogroup", {
    name: "How the ontology reads Replies",
  });
  await expect(
    modes.getByRole("radio", { name: "Structured mappings" }),
  ).toHaveAttribute("aria-checked", "true");
  await modes.getByRole("radio", { name: "Entity extraction" }).click();
  await expect(row).toContainText(EXTRACTS);
  await expect(row).toContainText("It has no text field yet");
  await expect(sourceBox(row)).toBeDisabled();

  // Chosen in place: the confirm says what it costs, and the sync it queues
  // makes the pieces documents, so the row can be checked.
  await row.getByRole("button", { name: "Choose its text field" }).click();
  await choose(row.getByRole("combobox", { name: "Text field" }), /^reply/);
  await row.getByRole("button", { name: "Save text field" }).click();
  const confirm = page.getByRole("dialog", {
    name: "Use reply as the text field?",
  });
  await expect(confirm).toContainText(REREAD);
  await confirm.getByRole("button", { name: "Save text field" }).click();
  await expect(page.getByText("Text field saved")).toBeVisible();
  await expect(row).toContainText(
    "Text field reply · 3 of 500 pieces as documents",
    { timeout: 15_000 },
  );
  await expect(sourceBox(row)).toBeEnabled();
  await sourceBox(row).check();

  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("button", { name: "Skip" }).click();
  await expect(
    page.getByText("Replies (extraction)", { exact: true }),
  ).toBeVisible();
  const sent = page.waitForRequest(/:generate$/);
  await page.getByRole("button", { name: "Create and generate" }).click();
  const body = (await sent).postDataJSON();
  // Its own set goes with the source, never among the sets.
  expect(body).toMatchObject({
    noConnectors: true,
    eventSources: [{ connectorId: source, mode: EXTRACTION }],
    documentSetIds: [],
  });
  expect(body.connectorIds).toBeUndefined();
  await createdOntology(page);
});

test("chooses an API source's text field on its page: its Pieces, this month's spend, a description alone, and a live sync's refusal", async ({
  page,
}) => {
  await signIn(page);
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  const source = await apiSource(page, "ws_acme", "Replies", REPLIES);
  await page.goto(`/data/connections/${source}`);
  const docs = page.getByRole("region", { name: "Documents", exact: true });
  await expect(
    docs.getByRole("heading", { name: "No text field" }),
  ).toBeVisible();
  await expect(detail(page, "Text field")).toHaveText("None");

  // The confirm says what it re-reads and what that costs; only the text
  // field is sent.
  const patched = () =>
    page.waitForRequest(
      (r) =>
        r.method() === "PATCH" && r.url().endsWith(`/connectors/${source}`),
    );
  await choose(docs.getByRole("combobox", { name: "Text field" }), /^reply/);
  await docs.getByRole("button", { name: "Save text field" }).click();
  const confirm = page.getByRole("dialog", {
    name: "Use reply as the text field?",
  });
  await expect(confirm).toContainText(REREAD);
  const saved = patched();
  await confirm.getByRole("button", { name: "Save text field" }).click();
  expect((await saved).postDataJSON()).toEqual({
    config: { events: { textField: "reply" } },
  });
  await expect(page.getByText("Text field saved")).toBeVisible();
  await expect(detail(page, "Text field")).toHaveText("reply");
  // Its sync makes the pieces documents of its own set, N of 500, and
  // says what extracting them has spent this month.
  await expect(
    docs
      .getByRole("list", { name: "Document sets" })
      .getByRole("listitem", { name: "Pieces" }),
  ).toContainText("3 of 500 pieces", { timeout: 15_000 });
  await expect(detail(page, "Extraction")).toContainText(
    "$0.00 of $5.00 this month",
  );

  // A description alone leaves the text field.
  await page.getByRole("button", { name: "API source actions" }).click();
  await page.getByRole("menuitem", { name: "Edit API source" }).click();
  const edit = page.getByRole("dialog", { name: "Edit Replies" });
  await edit
    .getByLabel("Description (optional)")
    .fill("A reply our support agent sent.");
  const described = patched();
  await edit.getByRole("button", { name: "Save changes" }).click();
  expect((await described).postDataJSON()).toEqual({
    config: { events: { description: "A reply our support agent sent." } },
  });
  await expect(page.getByText("API source saved")).toBeVisible();
  await expect(detail(page, "Description")).toHaveText(
    "A reply our support agent sent.",
  );
  await expect(detail(page, "Text field")).toHaveText("reply");

  // While a sync of it is live, a change is refused with the WFE's words,
  // and nothing changes.
  await mock.configure({ eventSyncMs: 60_000 });
  expect(
    (
      await mock.sendPieces(source, {
        pieces: [{ id: "t-4", data: { reply: "Late.", body: "Late." } }],
      })
    ).status,
  ).toBe(200);
  await choose(docs.getByRole("combobox", { name: "Text field" }), /^body/);
  await docs.getByRole("button", { name: "Save text field" }).click();
  await page
    .getByRole("dialog", { name: "Use body as the text field?" })
    .getByRole("button", { name: "Save text field" })
    .click();
  await expect(docs.getByRole("alert")).toHaveText(
    "a sync of this API source is running; change its text field in a minute",
  );
  await page.reload();
  await expect(detail(page, "Text field")).toHaveText("reply");
});

test("generates from a version that read an API source by extraction, keeps it on Try again, and never lists its Pieces among the sets", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 300 });
  await signIn(page);
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  const source = await apiSource(page, "ws_acme", "Replies", REPLIES, "reply");
  const pieces = await piecesOf(page, "ws_acme", source, 3);
  await currentFrom(page, source);
  await mock.configure({ ontologyOutcome: "fail" });

  // Generate starts from what the current version's generation read.
  await page.goto("/ontology/ont_acme_contracts/generate");
  await page.getByLabel("Goal").fill("Support health");
  const row = sourceRow(page, "Replies");
  const extraction = row.getByRole("radio", { name: "Entity extraction" });
  await expect(sourceBox(row)).toBeChecked();
  await expect(extraction).toHaveAttribute("aria-checked", "true");
  await expect(row).toContainText(EXTRACTS);
  await expect(row).toContainText(
    "Text field reply · 3 of 500 pieces as documents",
  );
  // Its Pieces are the source's, never a set to choose.
  const sets = page.getByRole("list", { name: "Document sets" });
  await expect(sets).toBeVisible();
  await expect(sets).not.toContainText("Pieces");
  await expect(page.getByRole("list", { name: "Replies sets" })).toHaveCount(0);
  const sent = page.waitForRequest(/ont_acme_contracts:generate$/);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  const body = (await sent).postDataJSON();
  expect(body).toMatchObject({
    goal: "Support health",
    eventSources: [{ connectorId: source, mode: EXTRACTION }],
  });
  expect(body.documentSetIds).not.toContain(pieces);

  await expect(page.getByText("The draft failed")).toBeVisible({
    timeout: 15_000,
  });
  await page.getByRole("link", { name: "Try again" }).click();
  await expect(page.getByLabel("Goal")).toHaveValue("Support health");
  await expect(sourceBox(row)).toBeChecked();
  await expect(extraction).toHaveAttribute("aria-checked", "true");
  const again = page.waitForRequest(/ont_acme_contracts:generate$/);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  const retried = (await again).postDataJSON();
  expect(retried).toMatchObject({
    eventSources: [{ connectorId: source, mode: EXTRACTION }],
  });
  expect(retried.documentSetIds).not.toContain(pieces);
});

test("pauses an API source once its allowance is spent: its spend line says so, and a load no one started holds its pieces with its own message", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 300, loadStepMs: 200 });
  await signIn(page);
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  const source = await apiSource(page, "ws_acme", "Replies", REPLIES, "reply");
  await piecesOf(page, "ws_acme", source, 3);
  const versionId = await currentFrom(page, source);
  // A person's load of it reads the pieces, and is current.
  const { load } = await post(page, `${O}/loads`, {
    versionId,
    clientToken: crypto.randomUUID(),
  });
  await expect
    .poll(async () => (await get(page, `${O}/loads/${load.id}`)).load.status, {
      timeout: 15_000,
    })
    .toBe("ready");

  await mock.configure({ eventSetAllowanceSpent: true });
  await page.goto(`/data/connections/${source}`);
  const until = nextMonth();
  await expect(detail(page, "Extraction")).toContainText(
    "$5.00 of $5.00 this month",
  );
  await expect(detail(page, "Extraction")).toContainText(
    `Paused until ${until}: its syncs add no new pieces as documents until then, so ontologies keep the pieces they have.`,
  );

  // A text field's change is the person's sync, which writes even now; the
  // extract it owes is no one's, and holds the pieces back.
  const docs = page.getByRole("region", { name: "Documents", exact: true });
  await choose(docs.getByRole("combobox", { name: "Text field" }), /^body/);
  await docs.getByRole("button", { name: "Save text field" }).click();
  const confirm = page.getByRole("dialog", {
    name: "Use body as the text field?",
  });
  await expect(confirm).toContainText(
    `Foundry re-reads this API source’s newest 500 pieces. Its extraction is paused until ${until}, so ontologies read the new text from then, or from a load a person starts. Customer contracts reads them.`,
  );
  await confirm.getByRole("button", { name: "Save text field" }).click();
  await expect(page.getByText("Text field saved")).toBeVisible();
  await expect
    .poll(
      async () =>
        (await get(page, `${O}/loads/${load.id}`)).load.jobs?.[0]?.status,
      { timeout: 15_000 },
    )
    .toBe("budget_exhausted");

  await page.goto(`/ontology/ont_acme_contracts/loads/${load.id}`);
  await expect(page.getByText("It stopped at the spending cap")).toBeVisible();
  await expect(
    page.getByText(
      'The API source "Replies" spent its $5.00 of this month\'s extraction by work no one started; its newest pieces wait for next month or a load a person starts.',
      { exact: true },
    ),
  ).toBeVisible();
});

test("shows a member an API source's text field, Pieces and spend without changes", async ({
  page,
  context,
}) => {
  await signIn(page);
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  const source = await apiSource(page, "ws_acme", "Replies", REPLIES, "reply");
  await piecesOf(page, "ws_acme", source, 3);

  await context.clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto(`/data/connections/${source}`);
  await expect(
    page.getByRole("heading", { level: 1, name: "Replies" }),
  ).toBeVisible();
  await expect(detail(page, "Text field")).toHaveText("reply");
  await expect(detail(page, "Extraction")).toContainText(
    "$0.00 of $5.00 this month",
  );
  const docs = page.getByRole("region", { name: "Documents", exact: true });
  await expect(
    docs
      .getByRole("list", { name: "Document sets" })
      .getByRole("listitem", { name: "Pieces" }),
  ).toContainText("3 of 500 pieces");
  await expect(docs.getByRole("combobox", { name: "Text field" })).toHaveCount(
    0,
  );
  await expect(
    page.getByRole("button", { name: "Save text field" }),
  ).toHaveCount(0);
});
