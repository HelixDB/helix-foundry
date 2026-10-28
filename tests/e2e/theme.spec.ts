import type { Page } from "@playwright/test";
import { expect, mock, signIn, test } from "./support/mock";

// The theme: System, Light or Dark from the account menu or the command
// palette, kept per browser, applied before first paint on every page (the
// sign-in pages too), and followed by the explorer's canvas at once.

const LIGHT_BG = "#fbfbfb",
  DARK_BG = "#0e0e0e";

const theme = (page: Page) =>
  page.evaluate(() => document.documentElement.dataset.theme ?? null);
// The page's background token, as the tokens resolve it now.
const backgroundToken = (page: Page) =>
  page.evaluate(() =>
    getComputedStyle(document.documentElement)
      .getPropertyValue("--hf-bg")
      .trim(),
  );

async function chooseFromMenu(page: Page, name: "System" | "Light" | "Dark") {
  await page.getByRole("button", { name: "Account" }).click();
  await page.getByRole("menuitemradio", { name }).click();
}

test("switches between System, Light and Dark from the account menu", async ({
  page,
}) => {
  await page.emulateMedia({ colorScheme: "light" });
  await signIn(page);
  // System is the default: no data-theme, and the OS decides.
  await page.getByRole("button", { name: "Account" }).click();
  await expect(
    page.getByRole("menuitemradio", { name: "System" }),
  ).toHaveAttribute("aria-checked", "true");
  await page.keyboard.press("Escape");
  expect(await theme(page)).toBeNull();
  expect(await backgroundToken(page)).toBe(LIGHT_BG);
  await page.emulateMedia({ colorScheme: "dark" });
  expect(await backgroundToken(page)).toBe(DARK_BG);

  // An explicit choice wins over the OS, and is kept across a reload.
  await chooseFromMenu(page, "Light");
  expect(await theme(page)).toBe("light");
  expect(await backgroundToken(page)).toBe(LIGHT_BG);
  await chooseFromMenu(page, "Dark");
  expect(await theme(page)).toBe("dark");
  await page.emulateMedia({ colorScheme: "light" });
  expect(await backgroundToken(page)).toBe(DARK_BG);
  await expect(page.locator("body")).toHaveCSS(
    "background-color",
    "rgb(14, 14, 14)",
  );
  await expect(
    page.locator('meta[name="theme-color"]').first(),
  ).toHaveAttribute("content", DARK_BG);
  await page.reload();
  await expect(
    page.getByRole("navigation", { name: "Main navigation" }),
  ).toBeVisible();
  expect(await theme(page)).toBe("dark");
  await page.getByRole("button", { name: "Account" }).click();
  await expect(
    page.getByRole("menuitemradio", { name: "Dark" }),
  ).toHaveAttribute("aria-checked", "true");
  await page.keyboard.press("Escape");

  // Back to System: the stored choice goes, and the OS decides again.
  await chooseFromMenu(page, "System");
  expect(await theme(page)).toBeNull();
  expect(await backgroundToken(page)).toBe(LIGHT_BG);
  expect(
    await page.evaluate(() => localStorage.getItem("foundry.theme")),
  ).toBeNull();
});

test("switches the theme from the command palette", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "light" });
  await signIn(page);
  // / opens Ask on an ontology (/ask, then /ask/<ontology>): a path change
  // closes the palette, so let it land before the shortcut.
  await expect(page).toHaveURL(/\/ask\/[^/?]+/);
  await expect(
    page.getByRole("navigation", { name: "Main navigation" }),
  ).toBeVisible();
  await page.keyboard.press("ControlOrMeta+k");
  const palette = page.getByRole("dialog", { name: "Search your workspace" });
  await palette.getByRole("combobox").fill("dark");
  await palette.getByRole("option", { name: /Use the dark theme/ }).click();
  expect(await theme(page)).toBe("dark");
  expect(await backgroundToken(page)).toBe(DARK_BG);
  // The theme in use isn't offered again.
  await page.keyboard.press("ControlOrMeta+k");
  await palette.getByRole("combobox").fill("theme");
  await expect(palette.getByRole("option", { name: /theme/ })).toHaveText([
    /Use the system theme/,
    /Use the light theme/,
  ]);
  await palette.getByRole("option", { name: /Use the system theme/ }).click();
  expect(await theme(page)).toBeNull();
  expect(await backgroundToken(page)).toBe(LIGHT_BG);
});

test("applies the stored theme before first paint, on sign-in too", async ({
  page,
}) => {
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/login");
  await page.evaluate(() => localStorage.setItem("foundry.theme", "dark"));
  // Without the app's scripts only the layout's inline script runs: the
  // page is dark from its first paint.
  await page.route("**/_next/static/**/*.js", (route) => route.abort());
  await page.reload();
  expect(await theme(page)).toBe("dark");
  await expect(page.locator("body")).toHaveCSS(
    "background-color",
    "rgb(14, 14, 14)",
  );
  // An unknown stored value leaves it to the OS.
  await page.evaluate(() => localStorage.setItem("foundry.theme", "sepia"));
  await page.reload();
  expect(await theme(page)).toBeNull();
  await expect(page.locator("body")).toHaveCSS(
    "background-color",
    "rgb(251, 251, 251)",
  );

  // With the app, sign-in shows in the stored theme.
  await page.unrouteAll();
  await page.evaluate(() => localStorage.setItem("foundry.theme", "dark"));
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "Sign in to Helix Foundry" }),
  ).toBeVisible();
  await expect(page.locator(".hf-app")).toHaveCSS(
    "background-color",
    "rgb(14, 14, 14)",
  );
  await expect(
    page.locator('meta[name="theme-color"]').first(),
  ).toHaveAttribute("content", DARK_BG);
});

test("redraws the explorer's canvas when the theme changes", async ({
  page,
}) => {
  await page.emulateMedia({ colorScheme: "light" });
  await mock.configure({ loadStepMs: 100 });
  await signIn(page);
  // A load of version 3, for the explorer to draw.
  const O = "/ontology/ont_acme_contracts";
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
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
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 20_000 });
  // Counts the frames the page asks for.
  await page.addInitScript(() => {
    const w = window as unknown as { frames: number };
    const request = window.requestAnimationFrame.bind(window);
    w.frames = 0;
    window.requestAnimationFrame = (callback) => {
      w.frames++;
      return request(callback);
    };
  });
  await page.goto("/explore/ont_acme_contracts");
  await expect(
    page.getByRole("application", { name: /^Graph of contracts: [1-9]/ }),
  ).toBeVisible({ timeout: 30_000 });
  // The canvas's corner is mostly its background (and a grid dot or two),
  // drawn in the theme's color.
  const corner = () =>
    page.evaluate(() => {
      const canvas = document.querySelector<HTMLCanvasElement>(
        "canvas.ex-canvas-surface",
      )!;
      const data = canvas.getContext("2d")!.getImageData(0, 0, 16, 16).data;
      const counts = new Map<string, number>();
      for (let i = 0; i < data.length; i += 4) {
        const color = `rgb(${data[i]}, ${data[i + 1]}, ${data[i + 2]})`;
        counts.set(color, (counts.get(color) ?? 0) + 1);
      }
      return [...counts].sort((a, b) => b[1] - a[1])[0][0];
    });
  await expect.poll(corner).toBe("rgb(251, 251, 251)");
  // Once the layout has settled the page asks for no more frames by itself:
  // a switch must ask for one.
  const idle = () =>
    page.evaluate(async () => {
      const w = window as unknown as { frames?: number };
      const before = w.frames;
      await new Promise((done) => setTimeout(done, 500));
      return w.frames === before;
    });
  await expect.poll(idle, { timeout: 30_000 }).toBe(true);
  await chooseFromMenu(page, "Dark");
  await expect.poll(corner).toBe("rgb(14, 14, 14)");
  // System follows the OS while the page is open.
  await chooseFromMenu(page, "System");
  await expect.poll(corner).toBe("rgb(251, 251, 251)");
  await page.emulateMedia({ colorScheme: "dark" });
  await expect.poll(corner).toBe("rgb(14, 14, 14)");
});
