import { expect, signIn, test } from "./support/mock";

// The hosted site's / (src/proxy.ts): the landing page on helix-website for
// a visitor who isn't signed in, and the app, on Ask, for anyone with a
// session.

test("sends a visitor to the landing page, and the signed in to the app", async ({
  page,
}) => {
  const out = await page.request.get("/?utm_source=x", { maxRedirects: 0 });
  expect(out.status()).toBe(307);
  expect(out.headers().location).toBe("https://www.helix-db.com/?utm_source=x");

  await signIn(page);
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts$/);
  // Signed in, / is the app, keeping the link's query.
  const home = await page.request.get("/?workspace=ws_acme&a=1&a=2", {
    maxRedirects: 0,
  });
  expect(home.status()).toBe(307);
  expect(home.headers().location).toBe("/ask?workspace=ws_acme&a=1&a=2");
  await page.goto("/");
  await expect(page).toHaveURL(/\/ask\/ont_acme_contracts$/);

  // Signed out, it is the landing page again.
  await page.getByRole("button", { name: "Account" }).click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/login$/);
  const again = await page.request.get("/", { maxRedirects: 0 });
  expect(again.headers().location).toBe("https://www.helix-db.com/");
});
