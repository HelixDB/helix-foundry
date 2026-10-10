import { test, expect, type Page } from "@playwright/test";
import { api, openApp, sampleWorkspace } from "./support/workspace";
import { artifact } from "./support/ports";

// The voice interface around a GPT-Live session: the orb, the transcript in
// the composer, Home -> chat continuity and cleanup. GPT-Live itself can't run
// here, so the WebRTC peer and the session endpoints are stand-ins; the chat,
// its runs and the Analyst are real. tests/live.test.ts covers the server.

// A fake peer connection whose event channel the test drives, and a silent
// microphone made from an audio graph so its tracks are real.
async function fakeVoice(page: Page) {
  await page.addInitScript(() => {
    const w = window as any;
    w.__voice = {
      sent: [] as any[],
      channel: null as any,
      tracks: [] as any[],
    };
    class Channel extends EventTarget {
      readyState = "connecting";
      label: string;
      constructor(label: string) {
        super();
        this.label = label;
      }
      send(data: string) {
        w.__voice.sent.push(JSON.parse(data));
      }
      close() {
        this.readyState = "closed";
      }
    }
    class Peer extends EventTarget {
      connectionState = "new";
      iceGatheringState = "complete";
      localDescription: any = null;
      createDataChannel(label: string) {
        const c = new Channel(label);
        w.__voice.channel = c;
        return c;
      }
      addTrack() {}
      async createOffer() {
        return { type: "offer", sdp: "v=0 fake-offer" };
      }
      async setLocalDescription(d: any) {
        this.localDescription = d;
      }
      async setRemoteDescription() {
        setTimeout(() => {
          this.connectionState = "connected";
          w.__voice.channel.readyState = "open";
          w.__voice.emit({
            type: "session.started",
            session: { id: "live_test" },
          });
        }, 50);
      }
      close() {
        this.connectionState = "closed";
      }
    }
    w.RTCPeerConnection = Peer;
    w.__voice.emit = (event: unknown) =>
      w.__voice.channel.dispatchEvent(
        new MessageEvent("message", { data: JSON.stringify(event) }),
      );
    navigator.mediaDevices.getUserMedia = async () => {
      if (w.__denyMic)
        throw new DOMException("Permission denied", "NotAllowedError");
      const stream = new AudioContext().createMediaStreamDestination().stream;
      w.__voice.tracks.push(...stream.getTracks());
      return stream;
    };
  });
}
const emit = (page: Page, event: unknown) =>
  page.evaluate((e) => (window as any).__voice.emit(e), event);
const micState = (page: Page) =>
  page.evaluate(() =>
    (window as any).__voice.tracks.map((t: MediaStreamTrack) =>
      t.readyState === "ended" ? "ended" : t.enabled ? "on" : "paused",
    ),
  );

test("hands-free voice continues from Home into the chat", async ({ page }) => {
  await fakeVoice(page);
  await openApp(page);
  const ws = await sampleWorkspace(page, "Voice " + Date.now());
  // The sample workspace uses the local model: no voice.
  await page.goto("/");
  await expect(
    page.getByRole("textbox", { name: "Ask anything about your data" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Start voice conversation" }),
  ).toHaveCount(0);

  // With OpenAI and GPT-Live access, the session endpoints answer.
  const claims: { runId: string; chatId: string; question: string }[] = [];
  await page.route("**/api/v1/workspaces/*/live", (r) =>
    r.fulfill({ json: { available: true } }),
  );
  let sessionRequest: any;
  await page.route("**/api/v1/workspaces/*/live/sessions", (r) => {
    sessionRequest = r.request().postDataJSON();
    return r.fulfill({
      json: {
        sessionId: "live_test",
        sdp: "v=0 fake-answer",
        chatId: sessionRequest.chatId ?? null,
      },
    });
  });
  await page.route("**/live/sessions/live_test/**", (r) => {
    const url = r.request().url();
    if (url.includes("/delegations/")) {
      const c = claims.shift()!;
      return r.fulfill({
        json: { status: "running", action: "ask", error: null, ...c },
      });
    }
    return r.fulfill({ json: { ok: true } });
  });
  await page.route("**/live/sessions/live_test", (r) =>
    r.fulfill({ json: { ok: true } }),
  );
  await page.reload();
  const orb = page.getByRole("button", { name: "Start voice conversation" });
  await expect(orb).toBeVisible();
  // Nothing is captured until the orb is pressed.
  expect(await micState(page)).toEqual([]);

  await orb.click();
  const stop = page.getByRole("button", { name: "Stop voice conversation" });
  await expect(stop).toHaveAttribute("aria-pressed", "true");
  await expect(stop).toHaveClass(/is-live/);
  expect(sessionRequest).toMatchObject({ sdp: "v=0 fake-offer", chatId: null });
  await expect(page.getByText("Listening · speak naturally")).toBeVisible();

  // The user's words appear in the Ask box as they speak.
  const home = page.getByRole("textbox", {
    name: "Ask anything about your data",
  });
  await emit(page, {
    type: "session.input_transcript.delta",
    delta: "How many orders",
    start_ms: 1000,
    end_ms: 1400,
  });
  await emit(page, {
    type: "session.input_transcript.delta",
    delta: " do we have?",
    start_ms: 1400,
    end_ms: 1800,
  });
  await expect(home).toHaveValue("How many orders do we have?");
  await page.screenshot({ path: artifact("voice-home.png") });

  // GPT-Live delegates; the server answers with an ordinary Analyst run,
  // which opens its chat. Voice stays on.
  const first = await api(page, `${ws.base}/assistant/runs`, {
    goal: "How many orders do we have?",
    intent: "answer",
  });
  claims.push({
    runId: first.id,
    chatId: first.id,
    question: "How many orders do we have?",
  });
  await emit(page, {
    type: "session.delegation.created",
    offset_ms: 1900,
    delegation: { id: "item_1", type: "delegation", target: "client" },
  });
  await expect(page).toHaveURL(new RegExp(`/analyst/${first.id}$`));
  const chatStop = page.getByRole("button", {
    name: "Stop voice conversation",
  });
  await expect(chatStop).toHaveClass(/is-live/);
  await expect(page.locator(".an-question").first()).toContainText(
    "How many orders do we have?",
  );
  const composer = page.getByRole("textbox", { name: "Ask a follow-up" });
  await expect(composer).toHaveValue("");

  // The assistant's words show under the composer while it speaks.
  await emit(page, {
    type: "session.output_transcript.delta",
    delta: "Checking that now.",
    start_ms: 2000,
    end_ms: 2600,
  });
  await expect(page.locator(".an-hint")).toHaveText("Checking that now.");

  // A follow-up, hands-free: the words show in the chat composer.
  await emit(page, {
    type: "session.input_transcript.delta",
    delta: "Which customers ordered most?",
    start_ms: 5000,
    end_ms: 6200,
  });
  await expect(composer).toHaveValue("Which customers ordered most?");
  await page.screenshot({ path: artifact("voice-chat-desktop.png") });

  // Typing takes the composer back and pauses the microphone; clearing the
  // text resumes listening. A typed draft is never sent by voice.
  await composer.press("End");
  await composer.pressSequentially(" by revenue");
  await expect(composer).toHaveValue(
    "Which customers ordered most? by revenue",
  );
  await expect(page.locator(".an-hint")).toContainText("Voice paused");
  expect(await micState(page)).toEqual(["paused"]);
  await composer.fill("");
  await expect(page.locator(".an-hint")).not.toContainText("Voice paused");
  expect(await micState(page)).toEqual(["on"]);

  // Phone width: the same chat, orb centred above the composer.
  await page.setViewportSize({ width: 390, height: 844 });
  const box = (await chatStop.boundingBox())!,
    form = (await page.locator(".an-composer").boundingBox())!;
  expect(box.y + box.height).toBeLessThanOrEqual(form.y + 1);
  expect(
    Math.abs(box.x + box.width / 2 - (form.x + form.width / 2)),
  ).toBeLessThan(4);
  expect(box.width).toBeGreaterThanOrEqual(44);
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  await expect(page.locator(".an-hint")).toHaveText(
    "Listening · speak naturally · Esc to stop",
  );
  await page.screenshot({ path: artifact("voice-chat-mobile.png") });
  // Reduced motion keeps the glow but stops its pulse.
  await page.emulateMedia({ reducedMotion: "reduce" });
  expect(
    await page
      .locator(".hf-voice-orb-mark")
      .evaluate((el) => getComputedStyle(el).animationName),
  ).toBe("none");
  await page.setViewportSize({ width: 1440, height: 1040 });

  // Escape ends voice: the session is closed and the microphone released.
  await page.keyboard.press("Escape");
  await expect(
    page.getByRole("button", { name: "Start voice conversation" }),
  ).toHaveAttribute("aria-pressed", "false");
  await expect.poll(() => micState(page)).toEqual(["ended"]);
  expect(
    await page.evaluate(() =>
      (window as any).__voice.sent.map((c: any) => c.type),
    ),
  ).toContain("session.close");
  await expect(composer).toBeEditable();

  // Starting in an existing chat continues that chat; leaving it ends voice.
  await page.getByRole("button", { name: "Start voice conversation" }).click();
  await expect(chatStop).toHaveClass(/is-live/);
  expect(sessionRequest.chatId).toBe(first.id);
  await page
    .getByRole("navigation", { name: "Main navigation" })
    .getByRole("link", { name: "Data", exact: true })
    .click();
  await expect(page).toHaveURL(/\/data$/);
  await expect.poll(async () => (await micState(page)).at(-1)).toBe("ended");
  await page.goBack();
  await expect(page).toHaveURL(new RegExp(`/analyst/${first.id}$`));
  await expect(
    page.getByRole("button", { name: "Start voice conversation" }),
  ).toHaveAttribute("aria-pressed", "false");

  // A blocked microphone says so, and typing still works.
  await page.evaluate(() => ((window as any).__denyMic = true));
  await page.getByRole("button", { name: "Start voice conversation" }).click();
  await expect(page.getByRole("alert")).toContainText(
    "Microphone access was blocked",
  );
  await expect(
    page.getByRole("button", { name: "Start voice conversation" }),
  ).not.toHaveClass(/is-live/);
  await expect(composer).toBeEditable();
});
