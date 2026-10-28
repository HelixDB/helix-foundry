import {
  expect,
  explorer,
  mock,
  signInToNewWorkspace,
  test,
} from "./support/mock";

// A new workspace's onboarding offers the sample company: one click uploads
// its files into Data, waits until Foundry has read them, then creates the
// ontology on the sample's ready-made draft, with no AI draft to wait for:
// its overview opens with the schema current and loading, and then its
// questions.

test("opens a new workspace's first ontology on the sample company, loaded and ready for questions", async ({
  page,
}) => {
  await signInToNewWorkspace(page);
  await page.getByRole("button", { name: "Use the sample" }).click();
  await expect(page.getByText("Adding the sample company")).toBeVisible();
  await expect(page).toHaveURL(/\/ontology\/ont_\w+$/, { timeout: 30_000 });
  await expect(
    page.getByRole("heading", { level: 1, name: "Kestrel customers" }),
  ).toBeVisible();
  await expect(page.getByRole("heading", { name: "Version 1" })).toBeVisible();

  // Once loaded, its questions are the draft's.
  await page
    .getByRole("link", { name: "Ask a question" })
    .click({ timeout: 30_000 });
  await expect(
    page.getByRole("group", { name: "Suggested questions" }),
  ).toContainText(
    "Who are each account's people, and what role does each play?",
  );

  // Its files are in Data, in their own set.
  await page.goto("/data");
  await expect(explorer(page).getByText("Kestrel sample")).toBeVisible();
});

test("opens the sample's generation when its draft fails", async ({ page }) => {
  await mock.configure({ ontologyOutcome: "fail" });
  await signInToNewWorkspace(page);
  await page.getByRole("button", { name: "Use the sample" }).click();
  await expect(page).toHaveURL(/\/ontology\/ont_\w+\/generations\/gen_\w+$/, {
    timeout: 30_000,
  });
});
