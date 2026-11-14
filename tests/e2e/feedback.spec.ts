import {
  expect,
  mock,
  signIn,
  signInToNewWorkspace,
  test,
} from "./support/mock";

// Feedback (src/web/shell/feedback.tsx) at the sidebar's foot on every page,
// and Request a connector (src/web/areas/data/request-connector.tsx) on the
// Connectors page and under the picker's kinds: both go to the WFE's POST
// /v1/feedback, which the mock keeps (GET /__mock/feedback).

test("sends feedback from the sidebar with its kind and the page it was on", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  const button = page.getByRole("button", { name: "Feedback" });
  await expect(button).toBeVisible();
  await button.click();
  const dialog = page.getByRole("dialog", { name: "Send feedback" });
  const send = dialog.getByRole("button", { name: "Send feedback" });
  await expect(send).toBeDisabled();
  await dialog.getByRole("radio", { name: "Idea" }).click();
  await dialog
    .getByLabel("What would you like Foundry to do?")
    .fill("Let me pin a connection to the top.");
  await send.click();
  await expect(dialog).toHaveCount(0);
  await expect(
    page.getByText("Thanks. Your feedback is with the Helix team"),
  ).toBeVisible();
  expect(await mock.feedback()).toEqual([
    {
      category: "FEEDBACK_CATEGORY_MISSING_CAPABILITY",
      message: "Let me pin a connection to the top.",
      page: "/data",
      workspaceId: "ws_acme",
      id: expect.any(String),
      userId: expect.any(String),
      createdAt: expect.any(String),
    },
  ]);
});

test("says why the WFE refused feedback, and keeps what was written", async ({
  page,
}) => {
  await signIn(page);
  // The hour's 20 are spent.
  await page.route("**/papi/v1/feedback", (route) =>
    route.fulfill({
      status: 429,
      contentType: "application/json",
      body: JSON.stringify({
        code: 8,
        message: "at most 20 feedback messages an hour; try again later",
        details: [],
      }),
    }),
  );
  await page.getByRole("button", { name: "Feedback" }).click();
  const dialog = page.getByRole("dialog", { name: "Send feedback" });
  const text = dialog.getByLabel("What happened, and what did you expect?");
  await text.fill("It broke");
  await dialog.getByRole("button", { name: "Send feedback" }).click();
  await expect(dialog.getByRole("alert")).toContainText(
    "at most 20 feedback messages an hour",
  );
  await expect(text).toHaveValue("It broke");
});

test("offers Feedback while a new workspace is onboarding", async ({
  page,
}) => {
  await signInToNewWorkspace(page);
  await page.getByRole("button", { name: "Feedback" }).click();
  const dialog = page.getByRole("dialog", { name: "Send feedback" });
  await dialog.getByRole("radio", { name: "Confusing" }).click();
  await dialog.getByLabel("What was confusing?").fill("Which database to pick");
  await dialog.getByRole("button", { name: "Send feedback" }).click();
  await expect(dialog).toHaveCount(0);
  const [sent] = await mock.feedback();
  expect(sent).toMatchObject({
    category: "FEEDBACK_CATEGORY_CONFUSING_RESULT",
    page: "/ontology/new",
  });
});

test("requests a connector from the Connectors page and from the picker", async ({
  page,
}) => {
  await signIn(page);
  await page.goto("/data");
  await page.getByRole("button", { name: "Request a connector" }).click();
  const dialog = page.getByRole("dialog", { name: "Request a connector" });
  const send = dialog.getByRole("button", { name: "Send request" });
  await expect(send).toBeDisabled();
  await dialog.getByLabel("Tool or database").fill("HubSpot");
  await dialog
    .getByLabel("What would you want from it? (optional)")
    .fill("Deals and contacts");
  await send.click();
  await expect(dialog).toHaveCount(0);
  await expect(
    page.getByText("Thanks. We’ve passed on your request for HubSpot"),
  ).toBeVisible();

  // Under the picker's kinds, it takes the picker's place, then gives it back.
  await page.getByRole("button", { name: "Add data" }).first().click();
  const picker = page.getByRole("dialog", { name: "Where is your data?" });
  await picker.getByRole("button", { name: "Request a connector" }).click();
  const request = page.getByRole("dialog", { name: "Request a connector" });
  await request.getByLabel("Tool or database").fill("Snowflake");
  await request.getByRole("button", { name: "Send request" }).click();
  await expect(
    page.getByRole("dialog", { name: "Where is your data?" }),
  ).toBeVisible();

  expect(
    (await mock.feedback()).map(({ category, message, page: at }) => ({
      category,
      message,
      at,
    })),
  ).toEqual([
    {
      category: "FEEDBACK_CATEGORY_CONNECTOR_REQUEST",
      message: "Connector: HubSpot\n\nDeals and contacts",
      at: "/data",
    },
    {
      category: "FEEDBACK_CATEGORY_CONNECTOR_REQUEST",
      message: "Connector: Snowflake",
      at: "/data",
    },
  ]);
});

test("lets a member request a connector, though only owners and admins add data", async ({
  page,
}) => {
  await signIn(page, "alan@example.com");
  await page.goto("/data");
  await expect(page.getByRole("button", { name: "Add data" })).toHaveCount(0);
  await page.getByRole("button", { name: "Request a connector" }).click();
  const dialog = page.getByRole("dialog", { name: "Request a connector" });
  await dialog.getByLabel("Tool or database").fill("Salesforce");
  await dialog.getByRole("button", { name: "Send request" }).click();
  await expect(dialog).toHaveCount(0);
  expect(await mock.feedback()).toMatchObject([
    { category: "FEEDBACK_CATEGORY_CONNECTOR_REQUEST", workspaceId: "ws_acme" },
  ]);
});
