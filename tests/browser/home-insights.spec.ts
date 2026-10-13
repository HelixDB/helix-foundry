// Home's "Some things we noticed" on a desktop and a 390px phone: three
// compact findings side by side, or one at a time with manual navigation;
// evidence on demand; dismiss and undo; the rest of Home unchanged.
import { expect, test, type Page } from "@playwright/test";
import { api, openApp } from "./support/workspace";
import { artifact } from "./support/ports";
import { clinic, commerce, weekStart } from "../fixtures/business";

// A published workspace from fixture tables, once the background scan has
// found `findings` shifts; selected in the browser.
async function fixtureWorkspace(
  page: Page,
  name: string,
  data: Record<string, Record<string, unknown>[]>,
  findings: number,
) {
  const workspace = await api(page, "/workspaces", { name });
  const base = `/workspaces/${workspace.id}`;
  for (const [table, rows] of Object.entries(data))
    await api(page, base + "/ingest", { name: table, rows });
  const state = await api(page, base + "/onboarding/ontology", {});
  await api(page, base + "/onboarding/ontology/confirm", {
    hash: state.ontology.hash,
    excluded: [],
    names: {},
  });
  await expect
    .poll(async () => (await api(page, base + "/onboarding")).complete, {
      timeout: 90000,
      intervals: [1000],
    })
    .toBe(true);
  await expect
    .poll(
      async () => (await api(page, base + "/home/insights")).findings.length,
      { timeout: 60000, intervals: [1000] },
    )
    .toBe(findings);
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  return base;
}
const noticed = (page: Page) =>
  page.getByRole("region", { name: "Some things we noticed" });
const cards = (page: Page) => noticed(page).locator("article.wk-finding");
const noOverflow = (page: Page) =>
  page.evaluate(
    () => document.documentElement.scrollWidth <= window.innerWidth,
  );

test("shows three findings on a desktop and one at a time on a phone", async ({
  page,
}) => {
  await openApp(page);
  const shop = commerce({ weeks: 5 });
  for (const order of shop.orders)
    if (Date.parse(String(order.ordered_at)) >= weekStart(0))
      order.net_amount = Number(order.net_amount) * 1.5;
  await fixtureWorkspace(page, "Meridian shop", shop, 3);
  await page.goto("/");
  const section = noticed(page);
  await expect(section).toBeVisible();
  await expect(cards(page)).toHaveCount(3);
  for (let i = 0; i < 3; i++) await expect(cards(page).nth(i)).toBeVisible();
  // Labelled status, three or four words beneath, no Important control.
  for (const label of ["Positive", "Warning"])
    await expect(
      section.getByText(label, { exact: true }).first(),
    ).toBeVisible();
  await expect(section.getByText("Important")).toHaveCount(0);
  await expect(
    section.getByRole("heading", { name: "Referral sessions grew 150%." }),
  ).toBeVisible();
  await expect(section.getByText(/^Checked:/)).toBeVisible();
  await expect(
    section.getByRole("button", { name: "Previous finding" }),
  ).toHaveCount(0);
  // Details holds the evidence, collapsed until asked for.
  const referral = cards(page).filter({
    has: page.getByRole("heading", { name: "Referral sessions grew 150%." }),
  });
  // The figure is bold within three or four words.
  const support = referral.locator(".wk-finding-support");
  await expect(support).toHaveText("500 referral sessions");
  await expect(support.locator("strong")).toHaveText("500");
  const table = referral.getByRole("table");
  await expect(table).toBeHidden();
  await referral.getByText("Details").click();
  await expect(table).toBeVisible();
  await expect(referral.getByText("Why it matters:")).toBeVisible();
  await expect(
    table.getByRole("row", { name: /referral 200 500 \+150% 10% → 25%/ }),
  ).toBeVisible();
  // The rest of Home is still there.
  await expect(
    page.getByRole("textbox", { name: "Ask anything about your data" }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Key metrics" }),
  ).toBeVisible();
  await expect(
    page.getByRole("region", { name: "Workspace health" }),
  ).toBeVisible();
  await expect(page.getByRole("heading", { name: "Ontology" })).toBeVisible();
  await referral.getByText("Details").click();
  await page.screenshot({
    path: artifact("home-insights-desktop.png"),
    fullPage: true,
  });
  // Dismiss, then undo.
  await referral
    .getByRole("button", { name: "Dismiss: Referral sessions grew 150%." })
    .click();
  await expect(cards(page)).toHaveCount(2);
  await expect(
    section.getByRole("status").filter({ hasText: "Finding dismissed." }),
  ).toBeVisible();
  const undo = section.getByRole("button", { name: "Undo" });
  await expect(undo).toBeFocused();
  await undo.click();
  await expect(cards(page)).toHaveCount(3);
  await expect(section.getByText("Finding dismissed.")).toHaveCount(0);

  // A 390px phone: one finding at a time, moved by hand, no page overflow.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.reload();
  const carousel = page.getByRole("region", { name: "Business findings" });
  await expect(carousel).toBeVisible();
  await expect(cards(page).locator("visible=true")).toHaveCount(1);
  await expect(section.getByText("1 of 3")).toBeVisible();
  await expect(
    section.getByRole("button", { name: "Previous finding" }),
  ).toBeDisabled();
  await section.getByRole("button", { name: "Next finding" }).click();
  await expect(section.getByText("2 of 3")).toBeVisible();
  await expect(cards(page).locator("visible=true")).toHaveCount(1);
  await expect(cards(page).locator("visible=true")).toHaveAttribute(
    "aria-label",
    "Finding 2 of 3",
  );
  expect(await noOverflow(page)).toBe(true);
  await page.screenshot({ path: artifact("home-insights-phone.png") });
  await cards(page).locator("visible=true").getByText("Details").click();
  expect(await noOverflow(page)).toBe(true);
  await page.screenshot({
    path: artifact("home-insights-phone-details.png"),
    fullPage: true,
  });
});

test("shows fewer cards when fewer findings are meaningful", async ({
  page,
}) => {
  await openApp(page);
  await fixtureWorkspace(page, "Riverside clinic", clinic(5), 1);
  await page.goto("/");
  await expect(cards(page)).toHaveCount(1);
  await expect(
    noticed(page).getByRole("heading", {
      name: "Online appointments grew 100%.",
    }),
  ).toBeVisible();
  await noticed(page).getByText("What we checked").click();
  await expect(
    noticed(page)
      .getByText("No recorded link between appointments and a later outcome.")
      .first(),
  ).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    noticed(page).getByRole("button", { name: "Next finding" }),
  ).toHaveCount(0);
  await expect(cards(page).locator("visible=true")).toHaveCount(1);
  expect(await noOverflow(page)).toBe(true);
});
