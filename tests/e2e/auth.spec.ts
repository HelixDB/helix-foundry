import type { Page } from "@playwright/test";
import {
  APP_ORIGIN,
  csrfToken,
  expect,
  mock,
  sessionCookie,
  signIn,
  test,
} from "./support/mock";

// Sign-in through the WFE-brokered flow (mock WFE and mock WorkOS), with the
// code WorkOS emails an unverified address and a company's single sign-on,
// signing in with an email and password (and making an account and
// resetting its password that way), the session's refresh, sign-out, and
// what the /papi BFF refuses.

/** What the main page, Founder's, asks of a new chat on Customer contracts. */
const HELLO = "What do you want to know about Customer contracts?";

test("signs in and returns to the page it came from", async ({ page }) => {
  await page.goto("/data");
  await expect(page).toHaveURL(/\/login\?return_to=%2Fdata$/);
  await expect(
    page.getByRole("heading", { name: "Sign in to Helix Foundry" }),
  ).toBeVisible();
  await signIn(page);
  await expect(page).toHaveURL(/\/data$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Connectors" }),
  ).toBeVisible();
  // The sidebar shows the account; its menu names it.
  await page.getByRole("button", { name: "Account" }).click();
  await expect(page.getByRole("menu")).toContainText("Ada Lovelace");
  await page.keyboard.press("Escape");
  // Host-only, HttpOnly session cookie under Foundry's own name.
  const cookie = await sessionCookie(page);
  expect(cookie?.httpOnly).toBe(true);
  expect(cookie?.domain).toBe("localhost");
  expect((await mock.stats()).loginStarts).toBe(1);
});

test("says what Foundry is only to a cold sign-in", async ({ page }) => {
  const pitch = page.getByRole("region", {
    name: "Your company’s data as one graph your agents can query.",
  });
  await page.goto("/login");
  await expect(pitch).toBeVisible();
  await expect(pitch.getByRole("listitem")).toHaveCount(3);
  const demo = new URL(
    (await pitch
      .getByRole("link", { name: "See it answer a question" })
      .getAttribute("href")) ?? "",
  );
  expect(demo.origin + demo.pathname + demo.hash).toBe(
    "https://www.helix-db.com/#foundry-ask",
  );
  expect(Object.fromEntries(demo.searchParams)).toEqual({
    utm_source: "foundry",
    utm_medium: "login",
    utm_campaign: "pitch",
  });
  // On a phone it sits under the sign-in, beside it on a wide screen.
  const google = page.getByRole("button", { name: "Continue with Google" });
  const boxes = async () => ({
    pitch: (await pitch.boundingBox())!,
    google: (await google.boundingBox())!,
  });
  const wide = await boxes();
  expect(wide.pitch.x).toBeGreaterThan(wide.google.x + wide.google.width);
  await page.setViewportSize({ width: 390, height: 844 });
  const phone = await boxes();
  expect(phone.pitch.y).toBeGreaterThan(phone.google.y + phone.google.height);
  // The app's own way here (a signed-out page) and a failed sign-in show
  // only the sign-in.
  await page.goto("/login?return_to=%2Fdata");
  await expect(
    page.getByRole("button", { name: "Continue with GitHub" }),
  ).toBeVisible();
  await expect(pitch).toHaveCount(0);
  await page.goto("/login?error=code_exchange_failed");
  await expect(
    page.getByRole("button", { name: "Continue with GitHub" }),
  ).toBeVisible();
  await expect(pitch).toHaveCount(0);
});

test("explains a sign-in that did not finish", async ({ page }) => {
  // A browser that lost the sign-in's cookie: an app's built-in browser, often.
  await page.goto("/login/callback?code=nope&state=unknown");
  await expect(page).toHaveURL(/\/login\?error=missing_flow_cookie$/);
  // (Next's route announcer is an empty role=alert too.)
  await expect(page.getByRole("alert").filter({ hasText: /./ })).toContainText(
    "This browser lost the sign-in it started",
  );
  // An exchange the WFE refuses says so in its own words.
  await page.goto("/login?error=code_exchange_failed");
  await expect(page.getByRole("alert").filter({ hasText: /./ })).toContainText(
    "The sign-in link expired or was already used",
  );
  // And the sign-in page still works from there: it opens the main page,
  // Ask.
  await signIn(page);
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts$/);
  await expect(
    page.getByRole("heading", { level: 1, name: HELLO }),
  ).toBeVisible();
});

test("offers the demo without an account", async ({ page }) => {
  await page.goto("/login");
  await expect(
    page.getByRole("link", { name: "See it without an account" }),
  ).toHaveAttribute("href", "https://demo.foundry.invalid/");
});

test.describe("in an app's built-in browser", () => {
  const notice = (page: Page) =>
    page.getByRole("status").filter({ hasText: "Open in your browser" });

  test.describe("on Android", () => {
    test.use({
      userAgent:
        "Mozilla/5.0 (Linux; Android 14; Pixel 7 Build/TQ3A.230805.001; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/120.0.6099.144 Mobile Safari/537.36 TwitterAndroid",
    });

    test("opens the page in Chrome, without its failure", async ({ page }) => {
      await page.goto("/login?error=missing_flow_cookie&return_to=%2Fdata");
      await expect(notice(page)).toContainText(
        "X’s built-in browser can lose your sign-in",
      );
      await expect(
        page.getByRole("link", { name: "Open in Chrome" }),
      ).toHaveAttribute(
        "href",
        `intent://${new URL(APP_ORIGIN).host}/login?return_to=%2Fdata#Intent;scheme=http;package=com.android.chrome;end`,
      );
      // Sign-in stays on offer.
      await expect(
        page.getByRole("button", { name: "Continue with Google" }),
      ).toBeEnabled();
    });
  });

  test.describe("on iOS", () => {
    test.use({
      userAgent:
        "Mozilla/5.0 (iPhone; CPU iPhone OS 17_1_2 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 [LinkedInApp]/9.29.6406",
    });

    test("says to open it in Safari, and copies its link", async ({
      page,
      context,
    }) => {
      await context.grantPermissions(["clipboard-read", "clipboard-write"]);
      await page.goto("/login?return_to=%2Fdata");
      await expect(notice(page)).toContainText("Open in Safari");
      await expect(page.getByRole("link", { name: "Open in Chrome" })).toHaveCount(0);
      await page.getByRole("button", { name: "Copy link" }).click();
      await expect(page.getByRole("button", { name: "Link copied" })).toBeVisible();
      expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(
        `${APP_ORIGIN}/login?return_to=%2Fdata`,
      );
    });
  });

  test("never shows in a browser of its own", async ({ page }) => {
    await page.goto("/login");
    await expect(
      page.getByRole("button", { name: "Continue with Google" }),
    ).toBeVisible();
    await expect(notice(page)).toHaveCount(0);
  });
});

test("explains a sign-in that could not start", async ({ page }) => {
  await page.goto("/auth/login?provider=nope&return_to=%2Fdata");
  await expect(page).toHaveURL(/\/login\?error=request$/);
  await expect(page.getByRole("alert").filter({ hasText: /./ })).toContainText(
    "Sign-in couldn’t start",
  );
  await expect(
    page.getByRole("button", { name: "Continue with GitHub" }),
  ).toBeEnabled();
  expect((await mock.stats()).loginStarts).toBe(0);
});

test("keeps only the newest pending sign-ins' cookies", async ({ page }) => {
  // Abandoned sign-ins, each with a long return path, as a page that keeps
  // starting them would leave. The browser sends its cookies oldest first.
  const pending = async () =>
    (await page.context().cookies())
      .filter((c) => c.name.includes("foundry-login-"))
      .map((c) => c.name);
  const started: string[] = [];
  for (let i = 0; i < 5; i++) {
    const returnTo = `/data?${"x".repeat(1500)}${i}`;
    await page.goto(
      `/auth/login?provider=github&return_to=${encodeURIComponent(returnTo)}`,
    );
    started.push(...(await pending()).filter((n) => !started.includes(n)));
  }
  expect(started).toHaveLength(5);
  expect((await pending()).sort()).toEqual(started.slice(2).sort());
});

test("says when this environment refuses the account", async ({ page }) => {
  await mock.configure({ allowedEmails: ["@helix.test"] });
  await page.goto("/login");
  await page.getByRole("button", { name: "Continue with Google" }).click();
  await page.getByLabel("Email").fill("ada@example.com");
  await page.getByRole("button", { name: "Continue", exact: true }).click();
  await expect(page).toHaveURL(/\/login\?error=denied$/);
  await expect(page.getByRole("alert").filter({ hasText: /./ })).toContainText(
    "This account can’t sign in here",
  );
  expect(await sessionCookie(page)).toBeUndefined();
});

/** Starts signing in with `provider` as `email`, through the mock's page. */
async function startSignIn(
  page: Page,
  email: string,
  provider: "GitHub" | "Microsoft" = "GitHub",
) {
  await page.getByRole("button", { name: `Continue with ${provider}` }).click();
  // The sign-in page has an Email field of its own: wait for the mock's.
  await page.waitForURL(/\/mock\/authorize\?/);
  await page.getByLabel("Email").fill(email);
  await page.getByRole("button", { name: "Continue", exact: true }).click();
}

/** The sign-ins in progress, by their cookies' names. */
const pendingLogins = async (page: Page) =>
  (await page.context().cookies())
    .filter((c) => c.name.includes("foundry-login-"))
    .map((c) => c.name);

// A new address containing "unverified" is one WorkOS hasn't seen verified,
// as a GitHub or Microsoft account's often is; the mock's code is 424242.
const UNVERIFIED = "grace.unverified@example.com";

test("asks for the code WorkOS emailed before signing in an unverified address", async ({
  page,
}) => {
  await page.goto("/data");
  await expect(page).toHaveURL(/\/login\?return_to=%2Fdata$/);
  await startSignIn(page, UNVERIFIED);
  await expect(page).toHaveURL(/\/login\/verify\?flow=[\w-]{43}$/);
  await expect(
    page.getByRole("heading", { name: "Check your email" }),
  ).toBeVisible();
  await expect(
    page.getByText(`Enter the code we sent to ${UNVERIFIED}`),
  ).toBeVisible();
  expect(await sessionCookie(page)).toBeUndefined();

  // A wrong code keeps the sign-in for another try.
  const code = page.getByLabel("Verification code"),
    submit = page.getByRole("button", { name: "Verify email" });
  await code.fill("111111");
  await submit.click();
  await expect(page.getByRole("alert").filter({ hasText: /./ })).toContainText(
    "That code isn’t right",
  );
  await code.fill("424242");
  await submit.click();
  // Signed in, back where it started: a new user, so with no workspace yet.
  await expect(page).toHaveURL(/\/data$/);
  await expect(
    page.getByRole("heading", { name: "Create your first workspace" }),
  ).toBeVisible();
  expect(await sessionCookie(page)).toBeDefined();
  expect(await pendingLogins(page)).toEqual([]);
  expect(
    (await mock.stats()).requests.filter((r) => r.includes("verify-email")),
  ).toEqual([
    "POST /v1/auth/verify-email 400",
    "POST /v1/auth/verify-email 200",
  ]);

  // Verified now: the next sign-in asks for no code.
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/login$/);
  await signIn(page, UNVERIFIED);
  await expect(
    page.getByRole("heading", { name: "Create your first workspace" }),
  ).toBeVisible();
});

test("starts again once the sign-in waiting for its code has ended", async ({
  page,
}) => {
  // The browser lost the sign-in it kept: the page says it ran out.
  await page.goto("/login");
  await startSignIn(page, UNVERIFIED, "Microsoft");
  await expect(page).toHaveURL(/\/login\/verify\?/);
  await page.context().clearCookies();
  await page.reload();
  await expect(page).toHaveURL(/\/login\?error=verify$/);
  await expect(page.getByRole("alert").filter({ hasText: /./ })).toContainText(
    "Your email isn’t verified yet.",
  );

  // The WFE forgot it (its ten minutes ran out): the same, keeping where to
  // land, and a new sign-in from there asks for a new code.
  await page.goto("/login?return_to=%2Fdata");
  await startSignIn(page, UNVERIFIED);
  await expect(page).toHaveURL(/\/login\/verify\?/);
  await mock.reset();
  await page.getByLabel("Verification code").fill("424242");
  await page.getByRole("button", { name: "Verify email" }).click();
  await expect(page).toHaveURL(/\/login\?error=verify&return_to=%2Fdata$/);
  expect(await pendingLogins(page)).toEqual([]);
  await startSignIn(page, UNVERIFIED);
  await expect(
    page.getByRole("heading", { name: "Check your email" }),
  ).toBeVisible();
});

test("signs in through the company's single sign-on when it requires it", async ({
  page,
}) => {
  await page.goto("/login?return_to=%2Fdata");
  // sso.example's company signs its people in with its own IdP, so WorkOS
  // refuses Microsoft for them: the sign-in goes on through the IdP.
  await startSignIn(page, "grace@sso.example", "Microsoft");
  await expect(
    page.getByRole("heading", { name: "Mock single sign-on" }),
  ).toBeVisible();
  await expect(page.getByLabel("Email")).toHaveValue("grace@sso.example");
  expect(await pendingLogins(page)).toHaveLength(1);
  await page.getByRole("button", { name: "Continue", exact: true }).click();
  await expect(page).toHaveURL(/\/data$/);
  await expect(
    page.getByRole("heading", { name: "Create your first workspace" }),
  ).toBeVisible();
  expect((await mock.stats()).loginStarts).toBe(2);
  expect(await pendingLogins(page)).toEqual([]);
});

test("signs in with an email and password", async ({ page }) => {
  await page.goto("/data");
  await expect(page).toHaveURL(/\/login\?return_to=%2Fdata$/);
  const form = page.getByRole("form", { name: "Sign in" }),
    email = form.getByLabel("Email"),
    password = form.getByLabel("Password"),
    submit = form.getByRole("button", { name: "Sign in" });
  await expect(submit).toBeDisabled();
  await email.fill("ada@example.com");
  await password.fill("not-her-password");
  await submit.click();
  await expect(form.getByRole("alert")).toHaveText(
    "That email and password don’t match. Try again, or reset your password.",
  );
  await expect(password).toHaveAttribute("aria-invalid", "true");
  expect(await sessionCookie(page)).toBeUndefined();
  // Typing again clears it.
  await password.fill("correct-horse-battery");
  await expect(form.getByRole("alert")).toHaveCount(0);
  await submit.click();
  await expect(page).toHaveURL(/\/data$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Connectors" }),
  ).toBeVisible();
  expect((await sessionCookie(page))?.httpOnly).toBe(true);
  // No page at WorkOS, so no pending sign-in.
  expect((await mock.stats()).loginStarts).toBe(0);
  expect(await pendingLogins(page)).toEqual([]);
});

test("creates an account with a password, verifying its email", async ({
  page,
}) => {
  await page.goto("/login?return_to=%2Fdata");
  await page.getByRole("link", { name: "Create one" }).click();
  await expect(page).toHaveURL(/\/login\/sign-up\?return_to=%2Fdata$/);
  await expect(
    page.getByRole("heading", { name: "Create your Helix account" }),
  ).toBeVisible();
  const name = page.getByLabel("Name"),
    email = page.getByLabel("Email"),
    password = page.getByLabel("Password"),
    create = page.getByRole("button", { name: "Create account" });
  await name.fill("Grace Hopper");
  // An address with an account already.
  await email.fill("ada@example.com");
  await password.fill("a-long-password");
  await create.click();
  await expect(page.getByRole("alert").filter({ hasText: /./ })).toHaveText(
    "An account already uses this email. Sign in instead, or reset its password.",
  );
  // WorkOS's rules, in its words.
  await email.fill("grace@example.com");
  await password.fill("short");
  await create.click();
  await expect(page.getByRole("alert").filter({ hasText: /./ })).toHaveText(
    "Password must be at least 10 characters long.",
  );
  await password.fill("a-long-password");
  await create.click();
  // WorkOS emails a new address a code first.
  await expect(page).toHaveURL(/\/login\/verify\?flow=[\w-]{43}$/);
  await expect(
    page.getByText("Enter the code we sent to grace@example.com"),
  ).toBeVisible();
  await page.getByLabel("Verification code").fill("424242");
  await page.getByRole("button", { name: "Verify email" }).click();
  await expect(page).toHaveURL(/\/data$/);
  await expect(
    page.getByRole("heading", { name: "Create your first workspace" }),
  ).toBeVisible();
  expect(await pendingLogins(page)).toEqual([]);

  // Verified now: signing in again asks for no code.
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/login$/);
  const form = page.getByRole("form", { name: "Sign in" });
  await form.getByLabel("Email").fill("grace@example.com");
  await form.getByLabel("Password").fill("a-long-password");
  await form.getByRole("button", { name: "Sign in" }).click();
  await expect(
    page.getByRole("heading", { name: "Create your first workspace" }),
  ).toBeVisible();
});

test("resets a password from the emailed link", async ({ page }) => {
  await page.goto("/login");
  const form = page.getByRole("form", { name: "Sign in" });
  await form.getByLabel("Email").fill("ada@example.com");
  await page.getByRole("link", { name: "Forgot your password?" }).click();
  await expect(page).toHaveURL(/\/login\/forgot\?email=ada%40example\.com$/);
  await expect(
    page.getByRole("heading", { name: "Reset your password" }),
  ).toBeVisible();
  await expect(page.getByLabel("Email")).toHaveValue("ada@example.com");
  await page.getByRole("button", { name: "Email me a link" }).click();
  await expect(
    page.getByRole("heading", { name: "Check your email" }),
  ).toBeVisible();
  await expect(
    page.getByText("If an account uses ada@example.com"),
  ).toBeVisible();
  // An address without an account reads the same, and gets no link.
  await page.getByRole("button", { name: "send another" }).click();
  await page.getByLabel("Email").fill("nobody@example.com");
  await page.getByRole("button", { name: "Email me a link" }).click();
  await expect(
    page.getByText("If an account uses nobody@example.com"),
  ).toBeVisible();
  const resets = await mock.passwordResets();
  expect(resets.map((r) => r.email)).toEqual(["ada@example.com"]);

  // A link the mock never sent.
  await page.goto("/reset-password?token=forged");
  await expect(page).toHaveURL(/\/login\/reset$/);
  await page.getByLabel("New password").fill("a-brand-new-password");
  await page.getByRole("button", { name: "Change password" }).click();
  await expect(page.getByRole("alert").filter({ hasText: /./ })).toHaveText(
    "Reset link is invalid or expired.",
  );

  // The emailed one: its token never shows in the page's URL.
  await page.goto(`/reset-password?token=${resets[0]!.token}`);
  await expect(page).toHaveURL(/\/login\/reset$/);
  expect(
    (await page.context().cookies()).find((c) => c.name === "foundry-reset")
      ?.httpOnly,
  ).toBe(true);
  // Too short, in WorkOS's words; the token stays for another try.
  await page.getByLabel("New password").fill("short");
  await page.getByRole("button", { name: "Change password" }).click();
  await expect(page.getByRole("alert").filter({ hasText: /./ })).toHaveText(
    "Password must be at least 10 characters long.",
  );
  await page.getByLabel("New password").fill("a-brand-new-password");
  await page.getByRole("button", { name: "Change password" }).click();
  await expect(
    page.getByRole("heading", { name: "Password changed" }),
  ).toBeVisible();
  expect(
    (await page.context().cookies()).some((c) => c.name === "foundry-reset"),
  ).toBe(false);
  await page.getByRole("link", { name: "Sign in" }).click();
  await expect(page).toHaveURL(/\/login$/);
  await form.getByLabel("Email").fill("ada@example.com");
  await form.getByLabel("Password").fill("a-brand-new-password");
  await form.getByRole("button", { name: "Sign in" }).click();
  await expect(page).toHaveURL(/\/ask\//);
  // The link works once.
  expect(await mock.passwordResets()).toEqual([]);
});

test("a password sign-up goes through the company's single sign-on when it requires it", async ({
  page,
}) => {
  await page.goto("/login/sign-up?return_to=%2Fdata");
  await page.getByLabel("Name").fill("Grace Hopper");
  await page.getByLabel("Email").fill("grace@sso.example");
  await page.getByLabel("Password").fill("a-long-password");
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(
    page.getByRole("heading", { name: "Mock single sign-on" }),
  ).toBeVisible();
  await expect(page.getByLabel("Email")).toHaveValue("grace@sso.example");
  await page.getByRole("button", { name: "Continue", exact: true }).click();
  await expect(page).toHaveURL(/\/data$/);
  await expect(
    page.getByRole("heading", { name: "Create your first workspace" }),
  ).toBeVisible();
});

test("refreshes an expired session once for parallel calls", async ({
  page,
}) => {
  await mock.configure({ accessTokenTtlSeconds: 1 });
  await signIn(page);
  await expect(
    page.getByRole("heading", { level: 1, name: HELLO }),
  ).toBeVisible();
  // Let the access token expire, then call the BFF six times at once.
  await page.waitForTimeout(2500);
  const before = (await mock.stats()).refreshes;
  const statuses = await page.evaluate(() =>
    Promise.all(
      Array.from({ length: 6 }, () =>
        fetch("/papi/v1/whoami").then((r) => r.status),
      ),
    ),
  );
  expect(statuses).toEqual([200, 200, 200, 200, 200, 200]);
  // The mock spends refresh tokens once, as WorkOS does: a second refresh
  // with the same token would have failed one of the calls.
  expect((await mock.stats()).refreshes - before).toBe(1);
});

test("the app takes turns to refresh an expired session", async ({ page }) => {
  // A one-second token, left to run out; the next is a normal one.
  await mock.configure({ accessTokenTtlSeconds: 1 });
  await signIn(page);
  await expect(
    page.getByRole("heading", { level: 1, name: HELLO }),
  ).toBeVisible();
  await page.waitForTimeout(2500);
  await mock.configure({ accessTokenTtlSeconds: 300 });
  const before = (await mock.stats()).refreshes;
  // A page that loads several queries at once.
  await page.goto("/data");
  await expect(
    page.getByRole("grid", { name: "Connections" }).getByText("Shop database"),
  ).toBeVisible();
  await expect(page).toHaveURL(/\/data$/);
  expect((await mock.stats()).refreshes - before).toBe(1);
  // Every answer says how long the session's token has left.
  const res = await page.request.get("/papi/v1/whoami");
  const left = Number(res.headers()["x-foundry-session-expires-in"]);
  expect(left).toBeGreaterThan(250);
  expect(left).toBeLessThanOrEqual(300);
});

test("signs out", async ({ page }) => {
  await signIn(page);
  await page.getByRole("button", { name: "Account" }).click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/login$/);
  await expect(
    page.getByRole("heading", { name: "Sign in to Helix Foundry" }),
  ).toBeVisible();
  expect(await sessionCookie(page)).toBeUndefined();
  expect((await page.request.get("/papi/v1/whoami")).status()).toBe(401);
  // Signed-out pages send you back to sign-in.
  await page.goto("/ontology");
  await expect(page).toHaveURL(/\/login\?return_to=%2Fontology$/);
});

test("refuses unsafe requests without a CSRF token or from another origin", async ({
  page,
}) => {
  await signIn(page);
  const body = { slug: "nope", region: "us-east-2", displayName: "Nope" };
  const noToken = await page.evaluate(async (b) => {
    const r = await fetch("/papi/v1/workspaces", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(b),
    });
    return { status: r.status, body: await r.json() };
  }, body);
  expect(noToken).toEqual({
    status: 403,
    body: expect.objectContaining({ code: "CSRF_REJECTED" }),
  });
  const token = await csrfToken(page);
  const forged = await page.request.post("/papi/v1/workspaces", {
    data: body,
    headers: { origin: "https://evil.example", "x-helix-csrf": token },
  });
  expect(forged.status()).toBe(403);
  expect((await forged.json()).code).toBe("ORIGIN_REJECTED");
  const wrongToken = await page.request.post("/papi/v1/workspaces", {
    data: body,
    headers: {
      origin: APP_ORIGIN,
      "x-helix-csrf": token.replace(/.$/, (c) => (c === "A" ? "B" : "A")),
    },
  });
  expect(wrongToken.status()).toBe(403);
  // Sign-out is CSRF-protected too.
  const logout = await page.request.post("/auth/logout", {
    headers: { origin: APP_ORIGIN },
  });
  expect(logout.status()).toBe(403);
  expect(
    (await mock.stats()).requests.filter((r) =>
      r.startsWith("POST /v1/workspaces"),
    ),
  ).toEqual([]);
});

test("refuses paths off the allowlist without reaching the WFE", async ({
  page,
}) => {
  await signIn(page);
  const clusters = await page.request.get("/papi/v1/clusters");
  expect(clusters.status()).toBe(404);
  expect((await clusters.json()).code).toBe("NOT_FOUND");
  const sso = await page.request.get("/papi/v1/workspaces/ws_acme/sso");
  expect(sso.status()).toBe(404);
  const remove = await page.request.delete("/papi/v1/workspaces/ws_acme", {
    headers: { origin: APP_ORIGIN },
  });
  expect(remove.status()).toBe(405);
  expect(remove.headers()["allow"]).toBe("GET");
  const sneaky = await page.request.get("/papi/v1/workspaces/..%2Fclusters");
  expect(sneaky.status()).toBe(404);
  const reached = (await mock.stats()).requests.join("\n");
  expect(reached).not.toMatch(/clusters|\/sso|DELETE/);
});

test("signs out from the workspace picker and a failed load", async ({
  page,
}) => {
  // A new user lands on the picker, outside the shell's account menu.
  await signIn(page, "grace@example.com");
  await expect(
    page.getByRole("heading", { name: "Create your first workspace" }),
  ).toBeVisible();
  await expect(page.getByText("Signed in as grace@example.com")).toBeVisible();
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/login$/);
  expect(await sessionCookie(page)).toBeUndefined();

  // The workspaces would not load: Retry, or sign out.
  await signIn(page);
  await page.route("**/papi/v1/workspaces?*", (route) =>
    route.fulfill({ status: 503, body: "{}" }),
  );
  await page.reload();
  // (Next's route announcer is an empty role=alert too.)
  await expect(page.getByRole("alert").filter({ hasText: /./ })).toContainText(
    "Couldn’t load your workspaces.",
    { timeout: 10_000 },
  );
  await expect(page.getByText("Signed in as ada@example.com")).toBeVisible();
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/login$/);
  expect(await sessionCookie(page)).toBeUndefined();
});
