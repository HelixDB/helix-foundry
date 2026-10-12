import { test, expect } from "@playwright/test";
import { api, openApp } from "./support/workspace";

test("Developer tools show saved token metadata and revoke only the selected integration", async ({
  page,
}) => {
  await openApp(page);
  const workspace = await api(page, "/workspaces", {
    name: "Token lifecycle " + Date.now(),
  });
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  const keep = await api(page, `/workspaces/${workspace.id}/tokens`, {
    name: "Existing notebook",
    readOnly: true,
  });
  await page.goto("/settings?tab=developer");
  const inventory = page.getByRole("region", { name: "Active API tokens" });
  await expect(inventory).toContainText("Existing notebook");
  await expect(inventory).toContainText("Read-only");
  const created = page.waitForResponse(
    (r) =>
      r.request().method() === "POST" &&
      new URL(r.url()).pathname === `/api/v1/workspaces/${workspace.id}/tokens`,
  );
  await page.getByLabel("Token name").fill("New automation");
  await page.getByLabel("Read-only access").uncheck();
  await page.getByRole("button", { name: "Create API token" }).click();
  const issued = await (await created).json();
  await expect(inventory).toContainText("New automation");
  await expect(inventory).toContainText("Read and write");
  await expect(page.locator(".copy-box code")).toHaveText(issued.token);
  await page.reload();
  await expect(inventory).toContainText("New automation");
  await expect(page.locator(".copy-box")).toHaveCount(0);
  await page.getByRole("button", { name: "Revoke New automation" }).click();
  const confirm = page.getByRole("dialog", {
    name: "Revoke “New automation”?",
  });
  await expect(confirm).toContainText(
    "Integrations that use this token will stop working.",
  );
  await confirm.getByRole("button", { name: "Revoke token" }).click();
  await expect(confirm).toHaveCount(0);
  await expect(inventory).not.toContainText("New automation");
  await expect(inventory).toContainText("Existing notebook");
  const revoked = await page.request.get(
    `/api/v1/workspaces/${workspace.id}/overview`,
    { headers: { authorization: "Bearer " + issued.token } },
  );
  expect(revoked.status()).toBe(401);
  const retained = await page.request.get(
    `/api/v1/workspaces/${workspace.id}/overview`,
    { headers: { authorization: "Bearer " + keep.token } },
  );
  expect(retained.status()).toBe(200);
});
