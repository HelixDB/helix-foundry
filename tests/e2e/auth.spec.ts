import {
  APP_ORIGIN,
  csrfToken,
  expect,
  mock,
  sessionCookie,
  signIn,
  test,
} from "./support/mock";

// Sign-in through the WFE-brokered flow (mock WFE and mock WorkOS), the
// session's refresh, sign-out, and what the /papi BFF refuses.

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

test("explains a sign-in that did not finish", async ({ page }) => {
  await page.goto("/login/callback?code=nope&state=unknown");
  await expect(page).toHaveURL(/\/login\?error=callback$/);
  // (Next's route announcer is an empty role=alert too.)
  await expect(page.getByRole("alert").filter({ hasText: /./ })).toContainText(
    "Sign-in didn’t finish",
  );
  // And the sign-in page still works from there: it opens the main page,
  // Ask.
  await signIn(page);
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Analyst" }),
  ).toBeVisible();
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

test("refreshes an expired session once for parallel calls", async ({
  page,
}) => {
  await mock.configure({ accessTokenTtlSeconds: 1 });
  await signIn(page);
  await expect(
    page.getByRole("heading", { level: 1, name: "Analyst" }),
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
    page.getByRole("heading", { level: 1, name: "Analyst" }),
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
