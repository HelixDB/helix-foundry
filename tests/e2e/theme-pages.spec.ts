import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { Page } from "@playwright/test";
import { dataTree, expect, explorer, mock, signIn, test } from "./support/mock";

// Every main page in both themes: a screenshot of each for review, saved in
// test-results/themes/<page>.<theme>.png, and a check that its text keeps
// WCAG AA contrast against what it sits on. The theme is switched in place,
// as the account menu does, so what draws its own colors must follow.

const O = "/ontology/ont_acme_contracts",
  X = "/explore/ont_acme_contracts";
const THEMES = ["light", "dark"] as const;

/**
 * Text whose color is under 4.5:1 (3:1 when large) against the backgrounds
 * under it. Hidden, screen-reader-only, disabled and decorative text is left
 * out, and so is text over a canvas, an image or a gradient, whose
 * background the page can't read.
 */
function lowContrastText() {
  const parse = (c: string) => {
    const m = /rgba?\(([^)]+)\)/.exec(c);
    if (!m) return null;
    const p = m[1]
      .split(/[\s,/]+/)
      .filter(Boolean)
      .map(Number);
    return [p[0], p[1], p[2], p[3] ?? 1];
  };
  const channel = (v: number) =>
    (v /= 255) <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  const luminance = (c: number[]) =>
    0.2126 * channel(c[0]) + 0.7152 * channel(c[1]) + 0.0722 * channel(c[2]);
  const over = (top: number[], under: number[]) =>
    [0, 1, 2].map((i) => top[i] * top[3] + under[i] * (1 - top[3]));
  const failures: string[] = [];
  for (const el of document.querySelectorAll<HTMLElement>("body *")) {
    const own = [...el.childNodes].some(
      (n) => n.nodeType === Node.TEXT_NODE && n.textContent!.trim(),
    );
    if (!own || el.closest("[aria-hidden=true], :disabled, svg, option"))
      continue;
    const rect = el.getBoundingClientRect();
    if (rect.width <= 1 || rect.height <= 1) continue;
    const style = getComputedStyle(el);
    if (style.visibility !== "visible") continue;
    // The backgrounds under the text, down to the first opaque one.
    const layers: number[][] = [];
    let unknown = false;
    for (let e: HTMLElement | null = el; e; e = e.parentElement) {
      const s = getComputedStyle(e);
      if (Number(s.opacity) < 1) unknown = true;
      if (s.backgroundImage !== "none") unknown = true;
      const bg = parse(s.backgroundColor);
      if (bg && bg[3] > 0) layers.push(bg);
      if (bg && bg[3] >= 1) break;
    }
    if (unknown || !layers.length || layers.at(-1)![3] < 1) continue;
    const ground = layers
      .reverse()
      .reduce((under, top) => over(top, under), [0, 0, 0]);
    const fg = parse(style.color);
    if (!fg) continue;
    const ink = over(fg, ground);
    const [hi, lo] = [luminance(ink), luminance(ground)].sort((a, b) => b - a);
    const ratio = (hi + 0.05) / (lo + 0.05);
    const size = parseFloat(style.fontSize),
      bold = Number(style.fontWeight) >= 700;
    const needed = size >= 24 || (size >= 18.66 && bold) ? 3 : 4.5;
    if (ratio < needed)
      failures.push(
        `"${el.textContent!.trim().slice(0, 40)}" ${style.color} on rgb(${ground.map(Math.round)}): ${ratio.toFixed(2)}`,
      );
  }
  return failures;
}

async function settle(page: Page) {
  await page.waitForLoadState("networkidle");
  await expect(page.locator("[aria-busy=true]")).toHaveCount(0);
}

test("every main page in both themes", async ({ page }, info) => {
  test.setTimeout(240_000);
  // Animations off: what is checked is where they end.
  await page.emulateMedia({ reducedMotion: "reduce", colorScheme: "light" });
  await mock.configure({ loadStepMs: 100, askStepMs: 100 });
  const failures: string[] = [];
  const shoot = async (name: string) => {
    for (const theme of THEMES) {
      await page.evaluate((t) => {
        localStorage.setItem("foundry.theme", t);
        document.documentElement.dataset.theme = t;
      }, theme);
      // A frame for the canvas and the components that follow the theme.
      await page.evaluate(
        () => new Promise((done) => requestAnimationFrame(() => done(null))),
      );
      await page.screenshot({
        path: join(info.project.outputDir, "themes", `${name}.${theme}.png`),
        fullPage: true,
      });
      for (const failure of await page.evaluate(lowContrastText))
        failures.push(`${name} (${theme}): ${failure}`);
    }
  };
  const visit = async (name: string, path: string) => {
    await page.goto(path);
    await settle(page);
    await shoot(name);
  };

  await visit("sign-in", "/login");
  await visit("sign-in-error", "/login?error=callback");
  await signIn(page);

  // A current agent draft, a document and a load, so every page has data.
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
  await page.goto("/data");
  await explorer(page)
    .getByLabel("Choose documents")
    .setInputFiles({
      name: "contract.md",
      mimeType: "text/markdown",
      buffer: readFileSync(
        new URL("../../mock/fixtures/contract.md", import.meta.url),
      ),
    });
  await expect(
    page.getByRole("list", { name: "Uploads" }).getByRole("listitem").first(),
  ).toContainText("Uploaded");
  await page.goto(`${O}#loads`);
  await page
    .getByRole("main")
    .getByRole("button", { name: "Load data" })
    .first()
    .click();
  await page
    .getByRole("dialog", { name: "Load version 3 into contracts?" })
    .getByRole("button", { name: "Load" })
    .click();
  // On the load's page: the overview's "isn’t loaded yet" would pass for
  // "Loaded", and leaving it would drop the load before it is created.
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 20_000 });
  const load = new URL(page.url()).pathname;

  await visit("data", "/data");
  await visit("data-connection", "/data/connections/conn_acme_stripe");
  await visit(
    "data-table",
    "/data/connections/conn_acme_stripe/tables/customers",
  );
  await page.goto("/data");
  await dataTree(page).getByRole("link", { name: "contract.md" }).click();
  await expect(page.getByLabel("Document text")).toBeVisible();
  await settle(page);
  await shoot("data-document");
  await visit("data-add", "/data?add=1");
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "PostgreSQL" })
    .click();
  await shoot("data-add-postgres");

  await visit("ontologies", "/ontology");
  await visit("ontology-new", "/ontology/new");
  await visit("ontology-schema", O);
  await visit("versions", `${O}#versions`);
  await visit("version-agent", `${O}/versions/ver_seed_2`);
  await visit("version-agent-types", `${O}/versions/ver_seed_2?tab=types`);
  await visit("version-foundry", `${O}/versions/ver_seed_1`);
  await visit("generation", `${O}/generations/gen_seed_4`);
  await visit("generate", `${O}/generate`);
  await visit("loads", `${O}#loads`);
  await visit("load", load);

  await visit("documents", `${O}/documents`);
  await page.getByRole("link", { name: "contract.md" }).first().click();
  await settle(page);
  await shoot("document");

  await page.goto(X);
  await expect(
    page.getByRole("application", { name: /^Graph of contracts: [1-9]/ }),
  ).toBeVisible({ timeout: 30_000 });
  await settle(page);
  await shoot("explorer");

  await visit("records", `${X}/records?type=Customer`);
  await page
    .getByRole("grid", { name: "Customer records" })
    .getByRole("row")
    .nth(1)
    .click();
  await shoot("records-drawer");
  await page.getByRole("link", { name: "Open its page" }).click();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "Globex Corporation",
  );
  await settle(page);
  await shoot("entity");

  // Ask: a new thread, then a thread with its answer.
  await visit("ask", "/ask/ont_acme_contracts");
  const form = page.getByRole("form", { name: "Ask a question" });
  await form
    .getByRole("textbox", { name: /^Question/ })
    .fill("Who signed the services agreement?");
  await form.getByRole("button", { name: "Ask", exact: true }).click();
  await expect(page.getByRole("region", { name: "Answer" })).toBeVisible({
    timeout: 20_000,
  });
  await settle(page);
  await shoot("ask-thread");

  await visit("settings", "/settings");
  await visit("members", "/settings/members");
  await visit("billing", "/settings/billing");
  await page.getByRole("button", { name: "Switch to Max" }).click();
  await shoot("billing-switch");
  await page.keyboard.press("Escape");

  await visit("palette", "/");
  await page.keyboard.press("ControlOrMeta+k");
  await page
    .getByRole("dialog", { name: "Search your workspace" })
    .getByRole("combobox")
    .fill("theme");
  await shoot("palette");
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "Account" }).click();
  await shoot("account-menu");

  expect(failures).toEqual([]);
});
