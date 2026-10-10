import { expect, test, type Page } from "@playwright/test";

// Exercise the real chat UI with deterministic results and no model or database.
async function openAnswer(page: Page, rows: Record<string, unknown>[]) {
  const workspace = { id: "precision", name: "Precision", role: "owner" };
  const run = {
    id: "answer",
    kind: "run",
    name: "Compare totals",
    createdAt: "2026-10-10T10:00:00.000Z",
    updatedAt: "2026-10-10T10:00:01.000Z",
    data: {
      goal: "Compare totals",
      status: "succeeded",
      intent: "answer",
      answer: { title: "Totals", rows, citations: [], assumptions: [] },
    },
  };
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/events")) {
      await route.fulfill({ status: 204 });
      return;
    }
    const responses: Record<string, unknown> = {
      "/api/v1/me": {
        user: { id: "local-user", name: "Local user" },
        workspaces: [workspace],
      },
      "/api/v1/workspaces/precision/onboarding": { complete: true },
      "/api/v1/workspaces/precision/threads": { items: [] },
      "/api/v1/workspaces/precision/threads/answer": {
        id: "answer",
        title: "Compare totals",
        runs: [run],
      },
    };
    await route.fulfill({
      status: Object.hasOwn(responses, path) ? 200 : 404,
      json: responses[path] ?? { error: "Unexpected fixture request" },
    });
  });
  await page.goto("/analyst/answer");
}

test("Analyst sorts large integers exactly and leaves nulls last", async ({
  page,
}) => {
  await openAnswer(page, [
    { category: "Higher", total: "9007199254740993" },
    { category: "Lower", total: "9007199254740992" },
    { category: "Missing", total: null },
    { category: "Negative", total: "-9007199254740993" },
    { category: "Small", total: 10 },
  ]);
  const grid = page.getByRole("grid", { name: "Totals" });
  const values = grid.locator(
    '[role="row"] [role="gridcell"][aria-colindex="2"]',
  );
  const header = grid.getByRole("columnheader", { name: "Total" });
  await expect(values).toHaveText([
    "9,007,199,254,740,993",
    "9,007,199,254,740,992",
    "null",
    "-9,007,199,254,740,993",
    "10",
  ]);
  await header.getByRole("button").click();
  await expect(header).toHaveAttribute("aria-sort", "ascending");
  await expect(values).toHaveText([
    "-9,007,199,254,740,993",
    "10",
    "9,007,199,254,740,992",
    "9,007,199,254,740,993",
    "null",
  ]);
  await header.getByRole("button").focus();
  await page.keyboard.press("Enter");
  await expect(header).toHaveAttribute("aria-sort", "descending");
  await expect(values).toHaveText([
    "9,007,199,254,740,993",
    "9,007,199,254,740,992",
    "10",
    "-9,007,199,254,740,993",
    "null",
  ]);
  await expect(page.locator(".an-chart")).toHaveCount(0);
});

test("Analyst shows the same exact total in a single-result answer", async ({
  page,
}) => {
  await openAnswer(page, [{ total: "9223372036854775807" }]);
  await expect(page.locator(".an-stats dd")).toHaveText(
    "9,223,372,036,854,775,807",
  );
  await page.setViewportSize({ width: 390, height: 844 });
  await page.emulateMedia({ reducedMotion: "reduce" });
  await expect(page.locator(".an-stats dd")).toBeVisible();
});
