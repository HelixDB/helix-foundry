import { readFileSync } from "node:fs";
import type { Page } from "@playwright/test";
import { textPdf } from "../../mock/pdf-text";
import {
  choose,
  dataTree,
  expect,
  explorer,
  file,
  mock,
  signIn,
  test,
  uploadCard,
  uploadInData,
} from "../e2e/support/mock";
import {
  CONTRACTS,
  caption,
  click,
  poster,
  saveVideo,
  shot,
  type,
  videoPage,
} from "./support";

// docs: foundry/data (the Connectors explorer and overview, connect data,
// sync it, read its tables, upload documents, API sources) and the
// connectors reference. A document's page in Data is shot in loads.spec.ts,
// once a load read it.

const SYNCED = { timeout: 20_000 };
const dialog = (page: Page) => page.locator("dialog[open]");
const header = (page: Page) => page.locator(".hf-page-header");
const CONTRACT = file(
  "contract.md",
  readFileSync(new URL("../../mock/fixtures/contract.md", import.meta.url)),
);
const MINUTES = file(
  "board-minutes-september.md",
  "# Board minutes, September\n\nGlobex Corporation renewed for two years. Initech LLC asked for monthly invoices.\n",
);

const RENEWALS = {
  name: "renewals.csv",
  mimeType: "text/csv",
  buffer: Buffer.from(
    [
      "customer,plan,seats,renews_on,owner",
      "Globex Corporation,Enterprise,250,2026-12-01,Ada Lovelace",
      "Initech LLC,Team,40,2027-01-15,Alan Turing",
      'Hooli,"Enterprise, annual",900,2027-03-31,Ada Lovelace',
      "Umbrella Holdings,Team,12,2026-11-20,Grace Hopper",
    ].join("\n"),
  ),
};

// Order lines an app sends an API source, as pieces.
const ORDER_LINES = [
  ["ord-1042", 1, "Globex Corporation", "Enterprise seats", 250, 42],
  ["ord-1042", 2, "Globex Corporation", "Onboarding", 1, 4800],
  ["ord-1043", 1, "Initech LLC", "Team seats", 40, 18],
  ["ord-1044", 1, "Hooli", "Enterprise seats", 900, 39],
  ["ord-1045", 1, "Umbrella Holdings", "Team seats", 12, 18],
].map(([order, line, customer, product, quantity, unitPrice], i) => ({
  id: `${order}-${line}`,
  occurredAt: `2026-10-2${i}T09:${10 + i * 7}:00Z`,
  data: {
    order_id: order,
    customer,
    product,
    quantity,
    unit_price: unitPrice,
    currency: "usd",
  },
}));

// A two-page PDF with a text layer, as a contract exported from a word
// processor is.
const AGREEMENT = {
  name: "master-services-agreement.pdf",
  mimeType: "application/pdf",
  buffer: textPdf([
    [
      "Master Services Agreement",
      "",
      "This agreement is made on 1 March 2026 between Acme Inc. and",
      "Globex Corporation, who agree as follows.",
      "",
      "1. Services. Acme provides the Enterprise plan for 250 seats.",
      "2. Term. Two years from the effective date, renewing yearly.",
    ],
    [
      "3. Fees. Globex pays 42 USD a seat each month, invoiced monthly.",
      "",
      "Signed for Acme Inc. by Ada Lovelace, Chief Executive.",
      "Signed for Globex Corporation by Hank Scorpio, President.",
    ],
  ]),
};

async function openPicker(page: Page) {
  await page.goto("/data");
  await page.getByRole("button", { name: "Add data" }).first().click();
  await expect(dialog(page)).toHaveAttribute(
    "aria-label",
    "Where is your data?",
  );
}

/** A second document set, created from documents/'s New set. */
async function newSet(page: Page, name: string, description: string) {
  await dataTree(page)
    .getByRole("treeitem", { name: "Documents", exact: true })
    .getByRole("button", { name: "New set" })
    .first()
    .click();
  const create = page.getByRole("dialog", { name: "New document set" });
  await create.getByLabel("Name").fill(name);
  await create.getByLabel("Description").fill(description);
  await create.getByRole("button", { name: "Create set" }).click();
  await expect(
    dataTree(page).getByRole("treeitem", { name, exact: true }),
  ).toBeVisible();
}

/** A custom folder at the tree's root, holding `connection`. */
async function folderWith(page: Page, name: string, connection: string) {
  await explorer(page).getByRole("button", { name: "New folder" }).click();
  const create = page.getByRole("dialog", { name: "New folder" });
  await create.getByLabel("Name").fill(name);
  await create.getByRole("button", { name: "Create folder" }).click();
  await page
    .getByRole("button", { name: `${connection} actions`, exact: true })
    .click();
  await page.getByRole("menuitem", { name: "Move to…" }).click();
  const move = page.getByRole("dialog", { name: `Move ${connection}` });
  await choose(move.getByLabel("Folder"), name);
  await move.getByRole("button", { name: "Move" }).click();
  await expect(
    dataTree(page)
      .getByRole("treeitem", { name, exact: true })
      .getByRole("treeitem", { name: new RegExp(`^${connection},`) }),
  ).toBeVisible();
}

test("data: the explorer and the overview", async ({ page }) => {
  await signIn(page);
  await uploadInData(page, [CONTRACT]);
  await shot(page, "data", "documents-uploaded", explorer(page));

  await newSet(
    page,
    "Board minutes",
    "Minutes of each board meeting, with renewals and pricing decisions.",
  );
  await uploadInData(page, [MINUTES], "Board minutes");
  await folderWith(page, "Finance", "Stripe");
  await page.goto("/data");
  await expect(page.getByRole("grid", { name: "Connections" })).toBeVisible();
  await expect(
    page.getByRole("list", { name: "Document sets" }).getByRole("listitem"),
  ).toHaveCount(2);
  await shot(page, "data", "overview");

  // A connection's menu: Move up, Move down and Move to….
  await page
    .getByRole("button", { name: "Shop database actions", exact: true })
    .click();
  await expect(page.getByRole("menuitem", { name: "Move to…" })).toBeVisible();
  await page.waitForTimeout(300);
  await shot(page, "data", "explorer-menu", {
    x: 0,
    y: 0,
    width: 560,
    height: 520,
  });
  await page.keyboard.press("Escape");

  // The same file again: it says where it is already.
  const chooser = page.waitForEvent("filechooser");
  await explorer(page)
    .getByRole("button", { name: "Upload documents" })
    .click();
  await (await chooser).setFiles([CONTRACT]);
  await expect(uploadCard(page, "contract.md")).toContainText(
    "Already uploaded",
  );
  await shot(page, "data", "upload-duplicate", explorer(page));
});

test("data: a CSV document's page", async ({ page }) => {
  await signIn(page);
  await uploadInData(page, [RENEWALS]);
  await dataTree(page).getByRole("link", { name: "renewals.csv" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "renewals.csv" }),
  ).toBeVisible();
  await expect(page.getByRole("main").getByRole("grid")).toContainText(
    "Globex",
  );
  // Once Foundry has read its rows, as a table of its own.
  await expect(page.getByText("Reading its rows")).toHaveCount(0, {
    timeout: 15_000,
  });
  await page.getByRole("button", { name: "Dismiss renewals.csv" }).click();
  await shot(page, "data", "document-csv");
});

test("data: a PDF document's page, once its text is read", async ({ page }) => {
  await signIn(page);
  await uploadInData(page, [AGREEMENT]);
  await dataTree(page).getByRole("link", { name: AGREEMENT.name }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: AGREEMENT.name }),
  ).toBeVisible();
  await expect(page.getByText("Reading its text")).toHaveCount(0, {
    timeout: 15_000,
  });
  await page.getByRole("button", { name: `Dismiss ${AGREEMENT.name}` }).click();
  // Its pages, drawn by pdf.js.
  await page.waitForTimeout(2000);
  await shot(page, "data", "document-pdf");
});

test("data: connect Stripe, sync it and read a table", async ({ page }) => {
  await signIn(page);
  // Tall enough for every kind in the picker, the API source included.
  await page.setViewportSize({ width: 1280, height: 1300 });
  await openPicker(page);
  await expect(
    dialog(page).getByRole("button", { name: "API source", exact: true }),
  ).toBeInViewport();
  await shot(page, "data", "add-data");
  await page.setViewportSize({ width: 1280, height: 800 });
  await dialog(page)
    .getByRole("button", { name: "Stripe", exact: true })
    .click();
  await dialog(page)
    .getByLabel("Restricted API key")
    .fill("rk_live_51DocsExampleKeyQx7m");
  await shot(page, "data", "stripe-form");
  await dialog(page).getByRole("button", { name: "Test connection" }).click();
  await expect(
    dialog(page).getByText("Stripe objects this key can read"),
  ).toBeVisible();
  await shot(page, "data", "stripe-tested");

  await dialog(page).getByRole("button", { name: "Connect and sync" }).click();
  await expect(page).toHaveURL(/\/data\/connections\/conn_/);
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  await shot(page, "data", "connection");

  await page
    .getByRole("main")
    .getByRole("link", { name: /^customers/ })
    .first()
    .click();
  await expect(page).toHaveURL(/\/tables\/customers/);
  await shot(page, "data", "synced-table");
});

test("data: each connector's form", async ({ page }) => {
  await signIn(page);
  const slug: Record<string, string> = {
    "Amazon S3": "s3",
    "Cal.com": "calcom",
  };
  for (const [kind, fill] of [
    ["WorkOS", { "API key": "sk_live_DocsExample" }],
    ["PostHog", { "Personal API key": "phx_DocsExample" }],
    ["Cal.com", { "API key": "cal_live_DocsExample" }],
    ["Granola", { "API key": "grn_DocsExample" }],
    [
      "PostgreSQL",
      {
        Host: "db.example.com",
        Database: "app",
        Username: "foundry_readonly",
      },
    ],
    ["Amazon S3", { Bucket: "acme-exports", "Folder (optional)": "exports/" }],
  ] as const) {
    await openPicker(page);
    await dialog(page).getByRole("button", { name: kind, exact: true }).click();
    for (const [label, value] of Object.entries(fill))
      await dialog(page).getByLabel(label, { exact: true }).fill(value);
    await shot(page, "data", `${slug[kind] ?? kind.toLowerCase()}-form`);
  }
  await dialog(page)
    .getByLabel("Role ARN")
    .fill("arn:aws:iam::123456789012:role/helix-foundry");
  await dialog(page).getByRole("button", { name: "Test connection" }).click();
  await expect(
    dialog(page).getByText("The bucket is in eu-west-2."),
  ).toBeVisible();
  await dialog(page)
    .getByRole("group", { name: "orders" })
    .scrollIntoViewIfNeeded();
  await shot(page, "data", "s3-tested");
});

test("data: an API source, its send panel and its page", async ({ page }) => {
  await signIn(page);
  await openPicker(page);
  await dialog(page)
    .getByRole("button", { name: "API source", exact: true })
    .click();
  await dialog(page).getByLabel("Name").fill("Order lines");
  await dialog(page)
    .getByLabel("Description (optional)")
    .fill(
      "One line of a customer’s order: what was bought, how many and for how much.",
    );
  await shot(page, "data", "api-source-form");
  await dialog(page).getByRole("button", { name: "Create API source" }).click();
  await expect(dialog(page)).toContainText("Order lines is ready.");

  // A send token, shown once, and how to send with it.
  await dialog(page)
    .getByRole("button", { name: "Create a send token" })
    .click();
  const made = dialog(page).getByRole("group", { name: "New send token" });
  await expect(made).toBeVisible();
  // Tall enough for the token, the prompt and Send over HTTP.
  await page.setViewportSize({ width: 1280, height: 1560 });
  await expect(
    dialog(page).getByRole("figure", { name: "pieces.json" }),
  ).toBeInViewport();
  await shot(page, "data", "api-source-send");
  await page.setViewportSize({ width: 1280, height: 800 });
  await made.getByRole("button", { name: "Done" }).click();
  await dialog(page).getByRole("button", { name: "Done" }).click();
  await expect(page).toHaveURL(/\/data\/connections\/conn_/);
  const id = new URL(page.url()).pathname.split("/").pop()!;

  // An app sends order lines; they sync into the source's table.
  expect((await mock.sendPieces(id, { pieces: ORDER_LINES })).status).toBe(200);
  await expect(page.getByRole("grid", { name: "Fields" })).toContainText(
    "unit_price",
    { timeout: 20_000 },
  );
  await expect(page.getByRole("list", { name: "Synced tables" })).toContainText(
    "events",
    { timeout: 20_000 },
  );
  await page.waitForTimeout(800);
  await shot(page, "data", "api-source");

  // Generate reads it through mappings, or by entity extraction.
  await page.goto(`${CONTRACTS}/generate`);
  const sources = page.getByRole("list", { name: "API sources" });
  await expect(sources).toContainText("Order lines");
  await sources.getByRole("checkbox").first().check();
  await shot(
    page,
    "ontology",
    "generate-api-sources",
    page.locator(".hf-panel", { has: sources }),
  );
});

test("data: video", async ({ browser }) => {
  const page = await videoPage(browser, "Connect your data");
  await page.goto("/data");
  await expect(page.getByText("Synced").first()).toBeVisible();
  await caption(
    page,
    "Connectors: the explorer beside it lists every connection and document",
    2600,
  );
  await caption(
    page,
    "Add data, then choose where the data is: every plan has every kind",
  );
  await click(page.getByRole("button", { name: "Add data" }).first());
  await click(
    dialog(page).getByRole("button", { name: "Stripe", exact: true }),
  );
  await caption(
    page,
    "Follow the steps to create a read-only key, and paste it",
  );
  await page.waitForTimeout(1500);
  await type(
    dialog(page).getByLabel("Restricted API key"),
    "rk_live_51DocsExampleKeyQx7m",
  );
  await caption(page, "Test the connection to see what the key can read");
  await click(dialog(page).getByRole("button", { name: "Test connection" }));
  await expect(
    dialog(page).getByText("Stripe objects this key can read"),
  ).toBeVisible();
  await page.waitForTimeout(1800);
  await caption(page, "Connect and sync: the first sync starts at once");
  await click(dialog(page).getByRole("button", { name: "Connect and sync" }));
  await expect(header(page).getByText("Synced", { exact: true })).toBeVisible(
    SYNCED,
  );
  await caption(
    page,
    "It joins its kind's folder; each synced table shows its rows and keys",
    1500,
  );
  // Off the caption, and past the "first sync is queued" toast.
  await page.mouse.move(1000, 420, { steps: 20 });
  await page.waitForTimeout(2500);
  await poster(page, "data", "connect-data");
  await click(
    page
      .getByRole("main")
      .getByRole("link", { name: /^customers/ })
      .first(),
  );
  await page.waitForTimeout(2500);
  await saveVideo(page, "data", "connect-data");
});

test("data: organise video", async ({ browser }) => {
  const page = await videoPage(browser, "Organise your data");
  await page.goto("/data");
  await expect(page.getByText("Synced").first()).toBeVisible();
  await caption(
    page,
    "The explorer: a folder per kind of connection, and documents/",
    2600,
  );
  await caption(page, "Make a folder of your own for connections");
  await click(explorer(page).getByRole("button", { name: "New folder" }));
  const create = page.getByRole("dialog", { name: "New folder" });
  await type(create.getByLabel("Name"), "Finance");
  await click(create.getByRole("button", { name: "Create folder" }));
  await caption(page, "Move a connection into it from its menu, or drag it");
  const stripe = dataTree(page).getByRole("treeitem", {
    name: /^Stripe,/,
  });
  await stripe.hover();
  await click(
    page.getByRole("button", { name: "Stripe actions", exact: true }),
  );
  await click(page.getByRole("menuitem", { name: "Move to…" }));
  await click(
    page
      .getByRole("dialog", { name: "Move Stripe" })
      .getByRole("button", { name: "Move" }),
  );
  await page.waitForTimeout(1200);
  await caption(page, "Upload documents: each goes into a set, a folder");
  const chooser = page.waitForEvent("filechooser");
  await click(explorer(page).getByRole("button", { name: "Upload documents" }));
  await (await chooser).setFiles([CONTRACT]);
  await expect(uploadCard(page, "contract.md")).toContainText("Uploaded");
  await page.waitForTimeout(2200);
  await caption(page, "Open a document to read it, move it or delete it");
  await click(dataTree(page).getByRole("link", { name: "contract.md" }));
  await expect(
    page.getByRole("heading", { level: 1, name: "contract.md" }),
  ).toBeVisible();
  await page.waitForTimeout(1500);
  await poster(page, "data", "organise-data");
  await caption(
    page,
    "Its ontologies say whether they read it, and what they made of it",
    2600,
  );
  await saveVideo(page, "data", "organise-data");
});
