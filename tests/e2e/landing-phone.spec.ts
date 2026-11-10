import { APP_ORIGIN, expect, mock, signIn, test } from "./support/mock";

// The landing page on a phone (src/landing/phone.ts): a short page whose hero
// asks for an email to remind the visitor to try Foundry on a computer
// (src/landing/remind.tsx, POST /remind, the mock's door in
// mock/reminders.ts), and the welcome a visit from a reminder or the README
// gets on a computer (?via=).

const PHONE = { width: 390, height: 844 };
const SOURCE = "https://github.com/helixdb/helix-foundry";
const CONSENT =
  "By sending, you’re agreeing for us to use your email to remind you about Foundry.";
const SENT =
  /^We’ll email you (today|tomorrow|on \w+|on \d+ \w+) at 9am\. Open it on your computer to start\.$/;

test.describe("on a phone", () => {
  test.use({ viewport: PHONE, timezoneId: "Europe/London" });

  test("is a short page with a reminder in place of the setup", async ({
    page,
  }) => {
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(String(error)));
    await page.goto("/");
    await expect(
      page.getByRole("form", { name: "Try it on your computer" }),
    ).toBeVisible();
    await expect(
      page.getByRole("link", { name: "Star on GitHub to find it later" }),
    ).toHaveAttribute("href", SOURCE);
    // None of the calls to action that need a computer.
    await expect(page.locator('[data-copy="agent-prompt"]')).toBeHidden();
    await expect(page.getByRole("button", { name: /git clone/ })).toBeHidden();

    // The Analyst plays; the field and the figures that need it never start.
    await page.locator("#how").scrollIntoViewIfNeeded();
    await expect(
      page
        .locator("#analyst-root")
        .getByText(/support tickets/)
        .first(),
    ).toBeVisible({ timeout: 10_000 });
    await expect(page.locator("#schema")).toBeHidden();
    await expect(page.locator("#explorer")).toBeHidden();
    await expect(page.locator("#cloud")).toBeHidden();
    expect(await page.locator("#schema-root").innerHTML()).toBe("");
    expect(await page.locator("#scene-root").innerHTML()).toBe("");
    await expect(page.locator("html")).not.toHaveClass(/has-field/);
    // The shout's call to action goes back to the form.
    await expect(
      page.getByRole("link", { name: "Remind me on my computer" }),
    ).toHaveAttribute("href", "#remind");
    expect(errors).toEqual([]);

    // Turned wide enough, it is the computer's page, field and all.
    await page.setViewportSize({ width: 1440, height: 900 });
    await expect(page.locator("#schema-root .so-stage")).toBeVisible();
    await expect(page.locator("html")).toHaveClass(/has-field/);
    await expect(
      page.getByRole("form", { name: "Try it on your computer" }),
    ).toBeHidden();
  });

  test("reminds an address, once", async ({ page }) => {
    await page.goto("/");
    const email = page.getByRole("textbox", { name: "Your email" });
    const consent = page.getByText(CONSENT);
    // The consent shows once something is typed.
    await expect(consent).toBeHidden();
    await email.fill(" Ada.L@Example.com ");
    await expect(consent).toBeVisible();
    await page.getByRole("button", { name: "Remind me" }).click();
    await expect(page.getByRole("status")).toHaveText(SENT);
    await expect(email).toBeHidden();
    expect(await mock.reminders()).toEqual([
      expect.objectContaining({
        email: "ada.l@example.com",
        timeZone: "Europe/London",
        consent: "remind-v1",
        status: "scheduled",
      }),
    ]);
    const { nextSendAt } = (await mock.reminders())[0]!;

    // Asking again keeps the schedule it has.
    await page.getByRole("button", { name: "Use another address" }).click();
    await email.fill("ada.l@example.com");
    await page.getByRole("button", { name: "Remind me" }).click();
    await expect(page.getByRole("status")).toHaveText(SENT);
    const kept = await mock.reminders();
    expect(kept).toHaveLength(1);
    expect(kept[0]!.nextSendAt).toBe(nextSendAt);
  });

  test("says why it couldn't", async ({ page }) => {
    await page.goto("/");
    const email = page.getByRole("textbox", { name: "Your email" });
    // The browser takes it; the server doesn't.
    await email.fill("ada@localhost");
    await page.getByRole("button", { name: "Remind me" }).click();
    await expect(page.getByRole("status")).toHaveText(
      "That doesn’t look like an email address. Check it and try again.",
    );
    // Typing again clears it.
    await email.fill("ada@example.com");
    await expect(page.getByRole("status")).toHaveText("");

    await mock.configure({ remindersPerHour: 0 });
    await page.getByRole("button", { name: "Remind me" }).click();
    await expect(page.getByRole("status")).toHaveText(
      "Too many reminders asked for just now. Try again in a minute.",
    );
    expect(await mock.reminders()).toEqual([]);
  });

  test("stops once the address signs in", async ({ page }) => {
    await page.goto("/");
    await page
      .getByRole("textbox", { name: "Your email" })
      .fill("ada@example.com");
    await page.getByRole("button", { name: "Remind me" }).click();
    await expect(page.getByRole("status")).toHaveText(SENT);

    await page
      .getByRole("link", { name: "Or start on Helix Cloud now" })
      .click();
    await signIn(page);
    await expect
      .poll(async () => (await mock.reminders())[0])
      .toMatchObject({ status: "stopped", stopReason: "signed_in" });
  });

  test("takes only this site's form", async ({ page }) => {
    const body = {
      email: "ada@example.com",
      timeZone: "Europe/London",
      consent: "remind-v1",
    };
    const other = await page.request.post("/remind", {
      data: body,
      headers: { Origin: "https://evil.example.test" },
    });
    expect(other.status()).toBe(403);
    const ours = await page.request.post("/remind", {
      data: body,
      headers: { Origin: APP_ORIGIN },
    });
    expect(ours.status()).toBe(200);
    expect(await ours.json()).toMatchObject({ again: false });
  });
});

test.describe("coming back on a computer", () => {
  test("from a reminder: welcome back, and the sample first", async ({
    page,
  }) => {
    await page.goto("/?via=email");
    await expect(page.getByText("Welcome back", { exact: true })).toBeVisible();
    const sample = page.getByRole("link", { name: "Try the sample company" });
    await expect(sample).toHaveAttribute(
      "href",
      "/login?return_to=%2Fontology%2Fsample",
    );
    // Counted once: the link's via goes.
    await expect(page.locator("html")).toHaveClass(/has-field/);
    await expect(page).toHaveURL(`${APP_ORIGIN}/`);

    // The sample is a new user's first page once they have a workspace.
    await sample.click();
    await signIn(page, "grace@example.com");
    await page.getByRole("button", { name: "Create workspace" }).click();
    const create = page.getByRole("dialog", { name: "Create a workspace" });
    await create.getByLabel("Workspace name").fill("Globex");
    await create.getByRole("button", { name: "Create workspace" }).click();
    await expect(page).toHaveURL(/\/ontology\/sample$/);
  });

  test("from the README: the sample first, no welcome", async ({ page }) => {
    await page.goto("/?via=github");
    await expect(
      page.getByRole("link", { name: "Try the sample company" }),
    ).toBeVisible();
    await expect(page.getByText("Welcome back", { exact: true })).toHaveCount(
      0,
    );
  });

  test("otherwise, the page as it was", async ({ page }) => {
    await page.goto("/");
    await expect(
      page.getByRole("link", { name: "Try the sample company" }),
    ).toHaveCount(0);
    await expect(page.locator('[data-copy="agent-prompt"]')).toHaveText(
      "Set up with your agent",
    );
    await expect(
      page.getByRole("form", { name: "Try it on your computer" }),
    ).toBeHidden();
  });

  test("signed in, the app drops via", async ({ page }) => {
    await page.goto("/login");
    await signIn(page);
    const home = await page.request.get("/?via=email&workspace=ws_acme", {
      maxRedirects: 0,
    });
    expect(home.status()).toBe(307);
    expect(home.headers().location).toBe("/ask?workspace=ws_acme");
  });
});
