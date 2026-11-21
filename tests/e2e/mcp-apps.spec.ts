import { readFile } from "node:fs/promises";
import { expect, test, type Page } from "@playwright/test";
import { build } from "esbuild";

// The answer view (src/mcp-apps, served by backend/integration/apps.go) as
// a host with MCP Apps draws it: the committed document in a sandboxed
// frame, handed what the Foundry MCP's ask returns (its result without
// outputs, the visuals' outputs and the thread's link in _meta) by the SDK's
// own host (support/mcp-host.ts). Its pictures, light and dark, are saved to
// test-results/mcp-apps/.

const ASK = "https://foundry.helix-db.com/ask/ont_1?workspace=ws_1&thread=q_1";
const META = "com.helix-db/foundry";
const USAGE = { input: 1, cache_read: 0, cache_write: 0, output: 1, usd: 0.01 };

const BY_STATUS = {
  start: { type: "Invoice", where: [], ids: null, text: null },
  steps: [],
  filters: [],
  return: { group_count: { at: 0, prop: "status", limit: 50 } },
};
const LARGEST = {
  start: { type: "Invoice", where: [], ids: null, text: null },
  steps: [{ via: "billed_to", dir: "out", to: "Customer", where: [] }],
  filters: [],
  return: {
    rows: {
      columns: [
        { at: 0, prop: null },
        { at: 0, prop: "amount_due" },
        { at: 1, prop: null },
      ],
      order_by: { at: 0, prop: "amount_due", desc: true },
      limit: 3,
    },
  },
};
const STATUSES = {
  kind: "group_count",
  groups: [
    { value: { untrusted: "paid" }, count: 358 },
    { value: { untrusted: "overdue" }, count: 3 },
    { value: { untrusted: "open" }, count: 2 },
  ],
  total_groups: 3,
};
const customer = (id: string, name: string) => ({ type: "Customer", id, name });
const invoice = (id: string) => ({ type: "Invoice", id, name: id });
const OPEN = {
  kind: "rows",
  rows: [
    {
      cells: [
        invoice("INV-30348"),
        66063,
        customer("cus_1", "Corbel HVAC Services"),
      ],
      edges: [],
    },
    {
      cells: [
        invoice("INV-30344"),
        52920,
        customer("cus_2", "Pinecrest Home Health"),
      ],
      edges: [],
    },
    {
      cells: [
        invoice("INV-30350"),
        5292,
        customer("cus_3", "Brightmoor Facilities"),
      ],
      edges: [],
    },
  ],
  total: 5,
};

/** What the Foundry MCP's ask returns for a question, with the visuals' outputs in _meta. */
function asked(
  status: string,
  result?: Record<string, unknown>,
  outputs: Record<string, unknown> = {},
) {
  return {
    content: [{ type: "text", text: "untrusted notice" }],
    structuredContent: {
      content_trust: "untrusted",
      id: "q_1",
      thread_id: "q_1",
      load_id: "load_1",
      question:
        "Which invoices are unpaid, and how are invoices split by status?",
      status,
      done: status !== "queued" && status !== "running",
      ...(result && { result }),
      attempts: 1,
      usd_micros: 40000,
      created_at: "2026-11-16T10:00:00Z",
      ...(result && { finished_at: new Date().toISOString() }),
    },
    _meta: { [META]: { ask_url: ASK, outputs } },
  };
}

const ANSWERED = asked(
  "answered",
  {
    status: "answered",
    answer: {
      blocks: [
        { para: "Almost every invoice is paid: 358 of 363 [c1]." },
        {
          list: [
            "Corbel HVAC Services owes the most, $66,063, still open [c2].",
            "Brightmoor Facilities' $5,292 invoice is overdue [c3].",
          ],
        },
      ],
      visuals: [
        {
          evidence_id: "e1",
          after: 0,
          kind: "bar",
          title: "Invoices by status",
        },
        { evidence_id: "e2", title: "Largest unpaid invoices" },
      ],
    },
    citations: [
      {
        marker: "c1",
        evidence_id: "e1",
        target: { result: { evidence_id: "e1" } },
      },
      {
        marker: "c2",
        evidence_id: "e2",
        target: { entity: { type: "Invoice", id: "INV-30348" } },
      },
      {
        marker: "c3",
        evidence_id: "e3",
        target: { quote: { doc_id: "doc_1", section: 2, start: 10, end: 42 } },
      },
    ],
    queries: [
      { evidence_id: "e1", query: BY_STATUS },
      { evidence_id: "e2", query: LARGEST },
    ],
    usage: USAGE,
  },
  { e1: STATUSES, e2: OPEN },
);

const CANNOT = asked("cannot_answer", {
  status: "cannot_answer",
  kind: "undeclared_property",
  reason: "Shipments have no cargo text: the ontology left it out.",
  gap: "A cargo description on Shipment, from the shipments table's cargo column.",
  queries: [],
  usage: USAGE,
});

let host = "",
  view = "";
test.beforeAll(async () => {
  const bundled = await build({
    entryPoints: ["tests/e2e/support/mcp-host.ts"],
    bundle: true,
    write: false,
    format: "iife",
    platform: "browser",
    logLevel: "silent",
  });
  host = bundled.outputFiles[0]!.text;
  view = await readFile("backend/integration/apps/answer.html", "utf8");
});

async function show(
  page: Page,
  result: Record<string, unknown> | undefined,
  theme: "light" | "dark" = "light",
) {
  await page.setContent("<!doctype html><html><body></body></html>");
  await page.addScriptTag({ content: host });
  await page.evaluate((s) => window.mcpHost.show(s), {
    html: view,
    input: {
      question:
        "Which invoices are unpaid, and how are invoices split by status?",
    },
    result,
    theme,
  });
  return page.frameLocator("iframe");
}

const opened = (page: Page) => page.evaluate(() => window.mcpHost.opened);

test("an answer shows its text, chart and table, and opens its pages through the host", async ({
  page,
}) => {
  for (const theme of ["light", "dark"] as const) {
    const frame = await show(page, ANSWERED, theme);
    await expect(
      frame.getByRole("heading", { name: /Which invoices are unpaid/ }),
    ).toBeVisible();
    await expect(frame.getByText("Almost every invoice is paid")).toBeVisible();
    await expect(frame.getByText("Invoices by status")).toBeVisible();
    await expect(frame.locator(".recharts-surface").first()).toBeVisible();
    await expect(frame.getByText("Largest unpaid invoices")).toBeVisible();
    await expect(frame.getByText("Corbel HVAC Services").first()).toBeVisible();
    // The frame grows to the card, as the view reports its height.
    await expect
      .poll(
        async () => (await page.locator("iframe").boundingBox())?.height ?? 0,
      )
      .toBeGreaterThan(400);
    await page.screenshot({
      path: `test-results/mcp-apps/answer-${theme}.png`,
      fullPage: true,
    });
  }
  const frame = page.frameLocator("iframe");
  await frame
    .getByRole("button", { name: "Source 3: Quote in a document" })
    .click();
  await frame.getByRole("button", { name: /Source 2/ }).click();
  await frame.getByRole("button", { name: "Open in Analyst" }).click();
  await expect
    .poll(() => opened(page))
    .toEqual([
      "https://foundry.helix-db.com/ontology/ont_1/documents/doc_1?load=load_1&workspace=ws_1#s2-10-42",
      ASK,
      ASK,
    ]);
});

test("a question it can't answer says what it lacks and drafts the version that could", async ({
  page,
}) => {
  for (const theme of ["light", "dark"] as const) {
    const frame = await show(page, CANNOT, theme);
    await expect(
      frame.getByText("This ontology can’t answer that"),
    ).toBeVisible();
    await expect(frame.getByText("Property not defined")).toBeVisible();
    await expect(
      frame.getByText(/It lacks: A cargo description/),
    ).toBeVisible();
    await page.screenshot({
      path: `test-results/mcp-apps/cannot-answer-${theme}.png`,
      fullPage: true,
    });
  }
  await page
    .frameLocator("iframe")
    .getByRole("button", { name: "Draft a new version with this" })
    .click();
  const [draft] = await opened(page);
  const url = new URL(draft!);
  expect(url.origin + url.pathname).toBe(
    "https://foundry.helix-db.com/ontology/ont_1/generate",
  );
  expect(url.searchParams.get("goal")).toContain("Which invoices are unpaid");
});

test("while Foundry answers, the card says so", async ({ page }) => {
  let frame = await show(page, undefined);
  await expect(
    frame.getByRole("heading", { name: /Which invoices are unpaid/ }),
  ).toBeVisible();
  await expect(frame.getByText("Asking Foundry")).toBeVisible();
  frame = await show(page, asked("running"), "dark");
  await expect(frame.getByText("Foundry is answering")).toBeVisible();
  await page.screenshot({
    path: "test-results/mcp-apps/running-dark.png",
    fullPage: true,
  });
});

test("a refused question says why", async ({ page }) => {
  const frame = await show(page, {
    isError: true,
    content: [
      {
        type: "text",
        text: "the workspace's questions budget for this month is spent",
      },
    ],
  });
  await expect(
    frame.getByText("Foundry didn’t take the question"),
  ).toBeVisible();
  await expect(
    frame.getByText(/questions budget for this month is spent/),
  ).toBeVisible();
});
