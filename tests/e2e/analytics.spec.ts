import type { Page } from "@playwright/test";
import { expect, signIn, test } from "./support/mock";

// Databuddy, the hosted site's product analytics (src/web/telemetry.tsx),
// loads only in a stamp's own build (next.config.ts): the mock's build, on
// the landing page, sign-in and the app, loads no tracker and sends nothing.
// tests/e2e/local checks a local build the same way.

/** Every request this page makes to Databuddy, from now on. */
function databuddyRequests(page: Page) {
  const urls: string[] = [];
  page.on("request", (request) => {
    if (new URL(request.url()).hostname.endsWith("databuddy.cc"))
      urls.push(request.url());
  });
  return urls;
}

test("loads no tracker in a build that isn't a stamp's", async ({ page }) => {
  const sent = databuddyRequests(page);
  await page.goto("/");
  await expect(page.locator("#schema-root")).toBeVisible();
  await page.goto("/login");
  await signIn(page);
  await expect(page).toHaveURL(/\/ask\//);
  await page.goto("/data");
  await expect(
    page.getByRole("complementary", { name: "Data explorer" }),
  ).toBeVisible();
  await page.goto("/ontology/ont_acme_contracts");
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  expect(await page.locator("script[data-databuddy-injected]").count()).toBe(0);
  expect(await page.evaluate(() => "databuddy" in window)).toBe(false);
  expect(sent).toEqual([]);
});
