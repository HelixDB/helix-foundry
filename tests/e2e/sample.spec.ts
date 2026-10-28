import {
  createdOntology,
  expect,
  explorer,
  signInToNewWorkspace,
  test,
} from "./support/mock";

// A new workspace's onboarding offers the sample company: one click uploads
// its files into Data, waits until Foundry has read them, then fills in the
// wizard up to its review, kept in the tab like any choice.

test("drafts a new workspace's first ontology from the sample company", async ({
  page,
}) => {
  await signInToNewWorkspace(page);
  await page.getByRole("button", { name: "Use the sample" }).click();
  await expect(page.getByText("Adding the sample company")).toBeVisible();
  await expect(page).toHaveURL(/\/ontology\/new\?step=review$/, {
    timeout: 30_000,
  });
  const steps = page.getByRole("list", { name: "Steps" });
  await expect(steps).toContainText("Kestrel customers");
  await expect(steps).toContainText(
    "Kestrel sample: 6 table files and 9 documents",
  );
  await expect(
    page.getByText("Drafting from the sample company"),
  ).toBeVisible();

  // A refresh keeps it.
  await page.reload();
  await expect(
    page.getByText("Drafting from the sample company"),
  ).toBeVisible();
  await page.getByRole("button", { name: "Create and generate" }).click();
  await createdOntology(page);

  // Its files are in Data, in their own set.
  await page.goto("/data");
  await expect(explorer(page).getByText("Kestrel sample")).toBeVisible();
});

test("adds the sample once, however often it is used", async ({ page }) => {
  await signInToNewWorkspace(page);
  await page.getByRole("button", { name: "Use the sample" }).click();
  await expect(page).toHaveURL(/\?step=review$/, { timeout: 30_000 });
  // A new tab starts the wizard afresh, and the sample again finds the
  // documents the workspace already has.
  await page.evaluate(() => sessionStorage.clear());
  await page.goto("/ontology/new");
  await page.getByRole("button", { name: "Use the sample" }).click();
  await expect(page).toHaveURL(/\?step=review$/, { timeout: 30_000 });
  await expect(page.getByRole("list", { name: "Steps" })).toContainText(
    "Kestrel sample: 6 table files and 9 documents",
  );
});
