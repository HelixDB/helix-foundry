import { expect, test, type Page } from "@playwright/test";
import { artifact } from "./support/ports";

// The Analyst's trace and answer in the real chat UI, from fixed runs: no
// model or database. Times are relative to now, so the clock reads true.
const customers = [
  ["Lucas I. Kim", 42],
  ["Noah T. Mehta", 34],
  ["Elena B. Parker", 31],
  ["Kai B. Nguyen", 28],
  ["Hugo S. Chen", 25],
  ["Isla H. Garcia", 23],
  ["Amara H. Martin", 21],
  ["Ava I. Bennett", 19],
  ["Oliver J. Reed", 18],
  ["Mia S. Patel", 16],
  ["Leo K. Brooks", 15],
  ["Zoe M. Turner", 12],
].map(([name, orders], i) => ({
  customer_id: 1000 + i,
  name,
  email: `${String(name).split(" ")[0].toLowerCase()}@example.test`,
  segment: i % 2 ? "Loyalty" : "Business",
  order_count: orders,
}));
const sql = `SELECT c.customer_id, c.name, c.email, c.segment, count(DISTINCT o.order_id) AS order_count
FROM orders o JOIN customers c USING (customer_id)
GROUP BY ALL ORDER BY order_count DESC, c.customer_id LIMIT 100`;

type Fixture = { status: number; json: unknown };
// A chat with one question, its run built from `data` and events at seconds
// after it was asked.
function chatRun(
  data: Record<string, unknown>,
  events: [number, string][],
  askedAgo = 30,
  updated = events.at(-1)?.[0] ?? 0,
) {
  const asked = Date.now() - askedAgo * 1000,
    at = (s: number) => new Date(asked + s * 1000).toISOString();
  return {
    id: "orders",
    kind: "run",
    name: "Who has the most orders?",
    createdAt: at(0),
    updatedAt: at(updated),
    data: {
      goal: "Who has the most orders?",
      intent: "auto",
      resolvedIntent: "answer",
      outline: ["understand", "analyze", "check", "write"],
      calls: 3,
      tokens: 4210,
      authority: { userId: "local-user" },
      ...data,
      events: events.map(([s, stage]) => ({
        at: at(s),
        stage,
        message: "Internal prompt text that must never be shown",
      })),
    },
  };
}
const answered = (askedAgo = 30) =>
  chatRun(
    {
      status: "succeeded",
      checkpoints: { analysis: { title: "Customers by order count", sql } },
      answer: {
        title: "Customers by order count",
        sql,
        rows: customers,
        total: customers.length,
        citations: [
          { datasetId: "orders", name: "Orders" },
          { datasetId: "customers", name: "Customers" },
        ],
        assumptions: ["Each distinct order is counted once."],
        summary:
          "Lucas I. Kim has the most orders. He placed 42 orders, 8 more than Noah T. Mehta, the next customer.",
        highlights: ["The top five customers placed 160 orders in all."],
        check: {
          kind: "ranking",
          label: "name",
          measure: "order_count",
          order: "desc",
          leaders: 1,
          keys: ["customer_id"],
          duplicates: 0,
          missing: 0,
        },
      },
    },
    [
      [1, "routing"],
      [3, "analysis"],
      [9, "query"],
      [12, "check"],
      [12, "summary"],
      [14, "complete"],
    ],
    askedAgo,
  );

// Serves the chat; `next` decides each later refresh of it.
async function openChat(
  page: Page,
  run: object,
  next?: (n: number) => Fixture | undefined,
) {
  const workspace = { id: "trace", name: "Trace", role: "owner" };
  let fetches = 0;
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/events")) return route.fulfill({ status: 204 });
    if (path === "/api/v1/workspaces/trace/threads/orders") {
      const later = fetches++ ? next?.(fetches - 1) : undefined;
      return route.fulfill(
        later ?? {
          status: 200,
          json: {
            id: "orders",
            title: "Who has the most orders?",
            runs: [run],
          },
        },
      );
    }
    const responses: Record<string, unknown> = {
      "/api/v1/me": {
        user: { id: "local-user", name: "Local user" },
        workspaces: [workspace],
      },
      "/api/v1/workspaces/trace/onboarding": { complete: true },
      "/api/v1/workspaces/trace/threads": { items: [] },
    };
    await route.fulfill({
      status: Object.hasOwn(responses, path) ? 200 : 404,
      json: responses[path] ?? { error: "Unexpected fixture request" },
    });
  });
  await page.goto("/analyst/orders");
}
const trace = (page: Page) =>
  page.getByRole("region", { name: "Analysis steps" });
const compactRows = (page: Page) => trace(page).locator(".an-steps > li");

test("Analyst shows a finished answer's steps, statement and key numbers", async ({
  page,
}) => {
  await openChat(page, answered());
  const steps = trace(page);
  await expect(steps.getByText("Analysis complete")).toBeVisible();
  await expect(steps.getByText("Answered in 14s")).toBeVisible();
  // Folded: the last two steps, each a bold name and its outcome on one line.
  await expect(compactRows(page)).toHaveCount(2);
  await expect(compactRows(page).nth(0)).toContainText(
    "Check the results: 12 rows, ranked by order count, highest first.",
  );
  await expect(compactRows(page).nth(1).locator("strong")).toHaveText(
    "Write the answer:",
  );
  // No prompt text, tabs, SQL footer or Details panel anywhere.
  await expect(page.getByText("Internal prompt text")).toHaveCount(0);
  await expect(page.getByRole("tab")).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^SQL/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Details/ })).toHaveCount(0);

  // The statement leads, bold quantities with their units.
  await expect(
    page.getByRole("heading", {
      level: 2,
      name: "Lucas I. Kim has the most orders.",
    }),
  ).toBeVisible();
  await expect(page.locator(".an-lead strong")).toHaveText([
    "42 orders",
    "8 more",
  ]);
  // 160 isn't a value or a difference in the rows, so it isn't emphasised.
  await expect(page.locator(".an-highlights strong")).toHaveCount(0);
  const weight = await page
    .locator(".an-lead strong")
    .first()
    .evaluate((el) => {
      const s = getComputedStyle(el);
      return { weight: s.fontWeight, background: s.backgroundColor };
    });
  expect(Number(weight.weight)).toBeGreaterThanOrEqual(600);
  expect(weight.background).toBe("rgba(0, 0, 0, 0)");

  // A ranked chart of the top ten, then all of them, then the table with
  // the measure before the keys.
  const chart = page.locator(".an-chart");
  await expect(chart.locator("figcaption")).toHaveText(
    "Order count by name, highest first",
  );
  await expect(chart.locator(".wk-bar-row")).toHaveCount(10);
  await expect(chart.locator(".wk-bar-row.is-first")).toHaveCount(1);
  await page.getByRole("button", { name: "Show all 12" }).click();
  await expect(chart.locator(".wk-bar-row")).toHaveCount(12);
  await page.screenshot({
    path: artifact("analyst-trace-desktop.png"),
    fullPage: true,
  });
  await page.getByRole("radio", { name: "Table" }).click();
  const grid = page.getByRole("grid", { name: "Customers by order count" });
  await expect(grid.getByRole("columnheader")).toHaveText([
    "Name",
    "Order count",
    "Email",
    "Segment",
    "Customer ID",
  ]);
  // Sources, assumptions and every row stay available.
  await expect(page.getByRole("link", { name: "Orders" })).toBeVisible();
  await page.getByRole("button", { name: "Assumptions (1)" }).click();
  await expect(
    page.getByText("Each distinct order is counted once."),
  ).toBeVisible();
  await page.getByRole("button", { name: "Show all 12 rows" }).click();
  await expect(grid.getByRole("row")).toHaveCount(13);

  // Expanded: every step with its time, and the query behind its own
  // disclosure, closed.
  await steps.getByRole("button", { name: "Show steps" }).click();
  await expect(compactRows(page)).toHaveCount(4);
  await expect(compactRows(page)).toContainText([
    "Understand the question: Planned “Customers by order count”",
    "Analyze the data: Ran a read-only query on Orders and Customers",
    "Check the results:",
    "Write the answer: Summarized the result in plain words",
  ]);
  await expect(steps.locator(".an-step-time")).toHaveText([
    "8s",
    "3s",
    "<1s",
    "2s",
  ]);
  const query = steps.locator("details.an-query");
  await expect(query.locator("summary")).toHaveText("Query used");
  await expect(query).not.toHaveAttribute("open", "");
  await expect(steps.getByLabel("SQL query")).toBeHidden();
  await query.locator("summary").click();
  await expect(steps.getByLabel("SQL query")).toContainText("count(DISTINCT");
  await expect(steps.getByText("3 model calls · 4,210 tokens")).toBeVisible();
  await page.getByRole("radio", { name: "Chart" }).click();
  await page.screenshot({
    path: artifact("analyst-trace-expanded.png"),
    fullPage: true,
  });
});

test("Analyst shows work in progress truthfully, with a stable estimate", async ({
  page,
}) => {
  const estimate = { low: 9800, high: 19200, samples: 6 };
  await openChat(
    page,
    chatRun(
      { status: "running", estimate },
      [
        [1, "routing"],
        [3, "analysis"],
      ],
      5,
    ),
  );
  const steps = trace(page);
  await expect(steps.getByText("Analyzing your question")).toBeVisible();
  await expect(steps.getByText("About 9–20s")).toBeVisible();
  // The clock ticks but is never read out.
  await expect(steps.locator(".an-trace-time .hf-num")).toHaveAttribute(
    "aria-hidden",
    "true",
  );
  await expect(steps.getByRole("button", { name: "Stop" })).toBeVisible();
  // At most three lines: the step in progress and the two planned after it,
  // which are not presented as done.
  await expect(compactRows(page)).toHaveCount(3);
  await expect(compactRows(page)).toHaveClass([
    /is-running/,
    /is-pending/,
    /is-pending/,
  ]);
  await expect(compactRows(page).nth(1)).toContainText(
    "Analyze the data: Run the query on your data snapshots",
  );
  await expect(page.locator(".an-answer")).toHaveCount(0);
  await page.screenshot({ path: artifact("analyst-trace-working.png") });
  // With reduced motion, the pulse and the spinner stop after one short run.
  await page.emulateMedia({ reducedMotion: "reduce" });
  for (const moving of [".an-trace-dot", ".an-step.is-running .an-step-mark"])
    expect(
      await steps
        .locator(moving)
        .evaluate((el) => getComputedStyle(el).animationIterationCount),
    ).toBe("1");
});

test("Analyst says when an answer takes longer than usual", async ({
  page,
}) => {
  await openChat(
    page,
    chatRun(
      {
        status: "running",
        estimate: { low: 9800, high: 19200, samples: 6 },
        checkpoints: { analysis: { title: "Customers by order count", sql } },
      },
      [
        [1, "routing"],
        [3, "analysis"],
        [9, "query"],
      ],
      40,
    ),
  );
  const steps = trace(page);
  await expect(steps.getByText("Taking longer than usual")).toBeVisible();
  await expect(steps.getByText("Usually 9–20s")).toBeVisible();
  await page.screenshot({ path: artifact("analyst-trace-slow.png") });
  await expect(compactRows(page)).toContainText([
    "Understand the question: Planned “Customers by order count”",
    "Analyze the data: Running a read-only query on your data snapshots",
    "Check the results:",
  ]);
});

test("Analyst keeps finished work when the analysis fails", async ({
  page,
}) => {
  await openChat(
    page,
    chatRun(
      {
        status: "failed",
        error: "Binder Error: column nope not found",
        partialStage: { stage: "analysis-repair", value: { sql } },
      },
      [
        [1, "routing"],
        [3, "analysis"],
        [9, "query"],
        [10, "analysis-repair"],
        [12, "query"],
        [13, "failed"],
      ],
    ),
  );
  const steps = trace(page);
  await expect(steps.getByText("Analysis stopped with an error")).toBeVisible();
  await expect(steps.getByText("Stopped after 13s")).toBeVisible();
  await expect(compactRows(page)).toHaveClass([
    /is-done/,
    /is-failed/,
    /is-pending/,
  ]);
  await expect(page.getByText("Couldn’t answer this")).toBeVisible();
  await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
  await steps.getByRole("button", { name: "Show steps" }).click();
  await expect(steps.locator("details.an-query summary")).toHaveText(
    "Query attempted",
  );
  await page.screenshot({ path: artifact("analyst-trace-failed.png") });
});

test("Analyst tells a lost connection apart from a failed analysis", async ({
  page,
}) => {
  const running = chatRun(
    { status: "running" },
    [
      [1, "routing"],
      [3, "analysis"],
      [9, "query"],
    ],
    12,
  );
  let down = true;
  await openChat(page, running, () =>
    down ? { status: 503, json: { error: "Unavailable" } } : undefined,
  );
  const steps = trace(page);
  await expect(steps.getByText("Analyzing your question")).toBeVisible();
  await expect(steps.getByText("Connection lost, reconnecting…")).toBeVisible({
    timeout: 15000,
  });
  await expect(steps.getByRole("status")).toHaveText(
    "Lost the connection to Foundry. The analysis may still be running; reconnecting.",
  );
  // The last known progress stays; nothing is marked failed.
  await expect(compactRows(page).nth(1)).toHaveClass(/is-running/);
  await expect(page.getByText("Couldn’t answer this")).toHaveCount(0);
  down = false;
  await expect(steps.getByText("Analyzing your question")).toBeVisible({
    timeout: 15000,
  });
});

test("Analyst trace and answer fit a phone, in dark mode with reduced motion", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.emulateMedia({ reducedMotion: "reduce" });
  // Dark mode is a stored choice.
  await page.addInitScript(() => localStorage.setItem("foundry.theme", "dark"));
  await openChat(page, answered());
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expect(
    page.getByRole("heading", { level: 2, name: /has the most orders/ }),
  ).toBeVisible();
  // Nothing scrolls sideways; folded steps stay on one line, cut short with
  // their whole text in a tooltip.
  const wide = await page.evaluate(
    () => document.documentElement.scrollWidth > window.innerWidth,
  );
  expect(wide).toBe(false);
  const row = compactRows(page).first();
  await expect(row).toHaveAttribute("title", /^Check the results: 12 rows/);
  const text = row.locator(".an-step-text");
  expect(await text.evaluate((el) => getComputedStyle(el).whiteSpace)).toBe(
    "nowrap",
  );
  const box = await text.boundingBox();
  expect(box!.height).toBeLessThanOrEqual(22);
  // Expanded steps wrap instead.
  await trace(page).getByRole("button", { name: "Show steps" }).click();
  expect(
    await trace(page)
      .locator(".an-step-text")
      .first()
      .evaluate((el) => getComputedStyle(el).whiteSpace),
  ).toBe("normal");
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth > window.innerWidth,
    ),
  ).toBe(false);
  // Keyboard: the toggle takes focus with the visible ring.
  const toggle = trace(page).getByRole("button", { name: "Hide steps" });
  await toggle.focus();
  await page.keyboard.press("Shift+Tab");
  await page.keyboard.press("Tab");
  expect(await toggle.evaluate((el) => getComputedStyle(el).outlineStyle)).toBe(
    "solid",
  );
  await page.screenshot({
    path: artifact("analyst-trace-mobile-dark.png"),
    fullPage: true,
  });
  await page.evaluate(() => (document.documentElement.dataset.theme = "light"));
  await trace(page).getByRole("button", { name: "Hide steps" }).click();
  await page.screenshot({
    path: artifact("analyst-trace-mobile.png"),
    fullPage: true,
  });
});
