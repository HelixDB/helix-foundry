import { readFileSync } from "node:fs";
import path from "node:path";
import type { Browser, Locator, Page } from "@playwright/test";
import {
  APP_ORIGIN,
  expect,
  file,
  signIn,
  uploadInData,
} from "../e2e/support/mock";

// Helpers for pnpm docs:media. Screenshots come from the test's own page (2x,
// 1280 by 800). Videos come from a page of their own: signed in already, at
// 1x so the recording is 1280 by 800, with a pointer drawn over the page,
// captions, and actions slowed down to a pace a reader can follow.

const ROOT = path.resolve(process.env.DOCS_MEDIA_DIR || "docs-media");
const media = (folder: string, file: string) => path.join(ROOT, folder, file);
const SIZE = { width: 1280, height: 800 };

/** A part of the viewport, in CSS pixels. */
type Clip = { x: number; y: number; width: number; height: number };

/**
 * Saves the viewport, or `target` alone (an element, or a part of the
 * viewport), as <folder>/<name>.png.
 */
export async function shot(
  page: Page,
  folder: string,
  name: string,
  target?: Locator | Clip,
) {
  await page.waitForLoadState("networkidle");
  // Hover and focus rings are not part of what a page looks like.
  await page.mouse.move(0, SIZE.height - 1);
  await page.evaluate(() => {
    const el = document.activeElement;
    if (el?.matches("input, textarea, select")) (el as HTMLElement).blur();
  });
  const clip = target && "width" in target ? target : undefined;
  await (target && !clip ? (target as Locator) : page).screenshot({
    path: media(folder, `${name}.png`),
    clip,
    animations: "disabled",
    caret: "hide",
    // A toast from the step before is not part of the page either.
    style: ".toast, .hf-toast { display: none !important; }",
  });
}

// The recorded page's pointer and captions. Both are popovers, so they can
// sit above a modal <dialog> in the top layer: each is shown again whenever
// it changes, which puts it back on top.
const OVERLAY = String.raw`
addEventListener("DOMContentLoaded", () => {
  const top = (el) => { if (el.matches(":popover-open")) el.hidePopover(); el.showPopover(); };
  const pointer = document.createElement("div");
  pointer.popover = "manual";
  pointer.id = "docs-pointer";
  pointer.style.cssText = "position:fixed;inset:0 auto auto 0;margin:0;padding:0;border:0;background:none;overflow:visible;pointer-events:none;transform:translate(-40px,-40px)";
  pointer.innerHTML = '<svg width="22" height="26" viewBox="0 0 22 26"><path d="M2 2l0 19 5-4.5 3.4 7.5 3.4-1.5-3.3-7.4 6.8-.4z" fill="#1d1d1b" stroke="#fff" stroke-width="1.6" stroke-linejoin="round"/></svg>';
  const caption = document.createElement("div");
  caption.popover = "manual";
  caption.id = "docs-caption";
  caption.style.cssText = "position:fixed;inset:auto 0 28px 0;margin:0 auto;width:fit-content;max-width:900px;padding:12px 22px;border:0;border-radius:12px;background:rgba(29,29,27,.92);color:#fff;font:500 17px/1.4 ui-sans-serif,system-ui,sans-serif;pointer-events:none;box-shadow:0 8px 30px rgba(0,0,0,.18)";
  document.body.append(pointer, caption);
  top(pointer);
  addEventListener("mousemove", (e) => {
    pointer.style.transform = "translate(" + e.clientX + "px," + e.clientY + "px)";
    if (document.querySelector("dialog[open]")) top(pointer);
  }, true);
  addEventListener("mousedown", (e) => {
    const ring = document.createElement("div");
    ring.style.cssText = "position:absolute;left:-14px;top:-14px;width:28px;height:28px;border-radius:50%;background:rgba(38,140,100,.35);transition:transform .45s,opacity .45s";
    pointer.prepend(ring);
    requestAnimationFrame(() => { ring.style.transform = "scale(1.8)"; ring.style.opacity = "0"; });
    setTimeout(() => ring.remove(), 500);
  }, true);
  window.__docsCaption = (text) => {
    caption.textContent = text;
    if (text) { top(caption); top(pointer); } else if (caption.matches(":popover-open")) caption.hidePopover();
  };
});
`;

/**
 * A page that records its video: `email` is signed in, and the video opens
 * on a title card naming the walkthrough.
 */
export async function videoPage(
  browser: Browser,
  title: string,
  email = "ada@example.com",
) {
  const signing = await browser.newContext({ baseURL: APP_ORIGIN });
  await signIn(await signing.newPage(), email);
  const storageState = await signing.storageState();
  await signing.close();
  const context = await browser.newContext({
    baseURL: APP_ORIGIN,
    viewport: SIZE,
    storageState,
    // Kept until saveVideo moves it; Playwright clears test-results each run.
    recordVideo: {
      dir: path.resolve("test-results", "docs-videos"),
      size: SIZE,
    },
  });
  await context.addInitScript(OVERLAY);
  const page = await context.newPage();
  await page.setContent(
    `<body style="margin:0;height:100vh;display:grid;place-items:center;background:#f7f6f2;color:#1d1d1b;font-family:Georgia,serif">
      <div style="text-align:center"><div style="font:600 18px ui-sans-serif,system-ui;letter-spacing:.02em;color:#5f6b63">Helix Foundry</div>
      <h1 style="font-weight:400;font-size:52px;margin:14px 0 0">${title}</h1></div></body>`,
  );
  await page.waitForTimeout(1800);
  return page;
}

/** Shows `text` at the foot of the video for `ms`, or until the next one. */
export async function caption(page: Page, text: string, ms = 0) {
  await page.evaluate((t) => (window as any).__docsCaption?.(t), text);
  if (ms) await page.waitForTimeout(ms);
}

/**
 * Saves the recorded page as it is now, a frame of its video, as
 * <folder>/<video>-poster.png: the cover the docs show before it plays.
 */
export async function poster(page: Page, folder: string, video: string) {
  await page.screenshot({
    path: media(folder, `${video}-poster.png`),
    animations: "disabled",
  });
}

/** Moves the pointer to `target` the way a person would, then clicks it. */
export async function click(target: Locator) {
  const page = target.page();
  await target.scrollIntoViewIfNeeded();
  const box = (await target.boundingBox())!;
  // A caption over the target would hide the click: drop it first.
  await page.evaluate((b) => {
    const r = document.getElementById("docs-caption")?.getBoundingClientRect();
    if (r && r.width && b.y + b.height > r.top && b.y < r.bottom)
      (window as any).__docsCaption("");
  }, box);
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, {
    steps: 24,
  });
  await page.waitForTimeout(300);
  await target.click();
  await page.waitForTimeout(700);
}

/** Clicks `target` and types `text` into it at typing speed. */
export async function type(target: Locator, text: string) {
  await click(target);
  await target.pressSequentially(text, { delay: 30 });
  await target.page().waitForTimeout(400);
}

/** Ends the recording and saves it as <folder>/<name>.webm. */
export async function saveVideo(p: Page, folder: string, name: string) {
  await caption(p, "");
  await p.waitForTimeout(1200);
  const video = p.video()!;
  await p.context().close();
  await video.saveAs(media(folder, `${name}.webm`));
  await video.delete();
}

/** Customer contracts, Acme's seeded ontology, whose version 3 reads General. */
export const CONTRACTS = "/ontology/ont_acme_contracts";
export const CONTRACT = file(
  "contract.md",
  readFileSync(new URL("../../mock/fixtures/contract.md", import.meta.url)),
);

/**
 * Signs in as Ada, uploads contract.md into General, makes Customer
 * contracts' version 3 current and loads it: Ask, the Explorer and Usage
 * then have a load to show.
 */
export async function loadContract(page: Page) {
  await signIn(page);
  await uploadInData(page, [CONTRACT]);
  await page.goto(`${CONTRACTS}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
  await page.getByRole("button", { name: "Load data" }).first().click();
  await page
    .getByRole("dialog", { name: "Load version 3 into contracts?" })
    .getByRole("button", { name: "Load" })
    .click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 30_000 });
}

/**
 * Asks `question` on Ask's page, as a new thread or a follow-up, and waits
 * for its answer.
 */
export async function ask(page: Page, question: string, followUp = false) {
  const form = page.getByRole("form", {
    name: followUp ? "Ask a follow-up" : "Ask a question",
  });
  await form.getByRole("textbox", { name: /^Question/ }).fill(question);
  await form.getByRole("button", { name: "Ask", exact: true }).click();
  await answered(page, question);
}

/** Waits for the answer to `question` in the thread open. */
export const answered = (page: Page, question: string) =>
  expect(
    page
      .getByRole("article", { name: `Question: ${question}` })
      .getByRole("region", { name: "Answer" }),
  ).toBeVisible({ timeout: 30_000 });
