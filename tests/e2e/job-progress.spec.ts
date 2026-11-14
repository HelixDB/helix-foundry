import {
  expect,
  makeContractsCurrent,
  mock,
  signIn,
  startContractsLoad,
  test,
} from "./support/mock";

// A job's bar (src/web/areas/ontology/job-progress.tsx): the step it is
// at, a bar and the time left or so far, on the job's own page and the
// overview's live callouts; and the job dock (job-dock.tsx), at the
// sidebar's foot of every other page, following what this browser started
// until it ends and is seen.

const O = "/ontology/ont_acme_contracts";
const dock = (page: import("@playwright/test").Page) =>
  page.getByRole("region", { name: "Jobs in progress" });

test("follows a load from any page with its step and bar, then says it ended", async ({
  page,
}) => {
  await signIn(page);
  await mock.configure({ loadStepMs: 2500 });
  await makeContractsCurrent(page);
  await startContractsLoad(page);
  await expect(page).toHaveURL(/\/loads\/load_/);
  // Its own page draws the bar, and leaves the sidebar to the rest.
  await expect(
    page.getByRole("progressbar", { name: "Progress of v3" }),
  ).toBeVisible();
  await expect(dock(page)).toHaveCount(0);

  // Another page's sidebar follows it, a reload too.
  await page.goto("/data");
  const card = dock(page);
  await expect(card).toContainText("Customer contracts");
  await expect(
    card.getByRole("progressbar", {
      name: "Progress of Customer contracts’s load",
    }),
  ).toBeVisible();
  await expect(card.getByRole("link", { name: "View progress" })).toHaveAttribute(
    "href",
    /\/ontology\/ont_acme_contracts\/loads\/load_/,
  );

  // It ends there, pointing at the Explorer, which it stops following.
  await expect(card).toContainText("Data loaded", { timeout: 30_000 });
  await card.getByRole("link", { name: "Explore it" }).click();
  await expect(page).toHaveURL(/\/explore\/ont_acme_contracts/);
  await expect(dock(page)).toHaveCount(0);
});

test("follows a draft and its bar, and stops following one that's dismissed", async ({
  page,
}) => {
  await signIn(page);
  await mock.configure({ ontologyStepMs: 4000 });
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(page).toHaveURL(/\/generations\/gen_/);
  // Its page: a bar against how long this ontology's drafts take.
  const bar = page.getByRole("progressbar", { name: /^Progress of / });
  await expect(bar).toBeVisible();

  await page.goto("/data");
  const card = dock(page);
  await expect(card).toContainText(/Drafting|Waiting/);
  await expect(card.getByRole("progressbar")).toBeVisible();
  await expect(card).toContainText("Draft ready to review", {
    timeout: 30_000,
  });
  await expect(card.getByRole("link", { name: "Review it" })).toHaveAttribute(
    "href",
    /\/ontology\/ont_acme_contracts\/versions\//,
  );
  await card.getByRole("button", { name: /^Dismiss/ }).click();
  await expect(dock(page)).toHaveCount(0);
  await page.reload();
  await expect(
    page.getByRole("heading", { level: 1, name: "Connectors" }),
  ).toBeVisible();
  await expect(dock(page)).toHaveCount(0);
});

test("stops following a job that ended on its own page", async ({ page }) => {
  await signIn(page);
  await mock.configure({ ontologyStepMs: 300 });
  await page.goto(`${O}/generate`);
  await page.getByRole("button", { name: "Generate", exact: true }).click();
  await expect(page).toHaveURL(/\/generations\/gen_/);
  await expect(page.getByText(/^Drafted version \d+$/)).toBeVisible({
    timeout: 15_000,
  });
  await page.goto("/data");
  await expect(
    page.getByRole("heading", { level: 1, name: "Connectors" }),
  ).toBeVisible();
  await expect(dock(page)).toHaveCount(0);
});

test("draws a live load's bar on the overview's callout", async ({ page }) => {
  await signIn(page);
  await mock.configure({ loadStepMs: 2500 });
  await makeContractsCurrent(page);
  await startContractsLoad(page);
  await expect(page).toHaveURL(/\/loads\/load_/);
  await page.goto(O);
  await expect(
    page.getByRole("progressbar", {
      name: "Progress of Customer contracts’s load",
    }),
  ).toBeVisible();
});
