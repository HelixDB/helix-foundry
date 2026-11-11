import { afterEach, describe, expect, it, vi } from "vitest";
import { NextRequest, type NextFetchEvent } from "next/server";

// src/proxy.ts: AI crawlers reading the public pages go to Databuddy's AI
// Agents page, after the response; a build without a client id sends nothing.
// A client asking for Markdown gets the landing page as Markdown.

const agents = vi.hoisted(() => ({ trackAgents: vi.fn(async () => {}) }));
vi.mock("@databuddy/sdk/agents", () => agents);

const request = new NextRequest("https://foundry.helix-db.com/", {
  headers: { "user-agent": "GPTBot/1.2" },
});

/** src/proxy.ts as a build with `clientId` (empty: none) loads it. */
async function load(clientId: string) {
  vi.stubEnv("NEXT_PUBLIC_DATABUDDY_CLIENT_ID", clientId);
  vi.resetModules();
  return import("@/proxy");
}

afterEach(() => {
  vi.unstubAllEnvs();
  agents.trackAgents.mockClear();
});

describe("proxy", () => {
  it("reports the request to the stamp's site once the response is sent", async () => {
    const { proxy } = await load("client-test");
    const event = { waitUntil: vi.fn() } as unknown as NextFetchEvent;
    expect(proxy(request, event)).toBeUndefined();
    expect(agents.trackAgents).toHaveBeenCalledWith(request, {
      websiteId: "client-test",
    });
    expect(event.waitUntil).toHaveBeenCalledWith(
      agents.trackAgents.mock.results[0].value,
    );
  });

  it("reports nothing without a client id", async () => {
    const { proxy } = await load("");
    const event = { waitUntil: vi.fn() } as unknown as NextFetchEvent;
    proxy(request, event);
    expect(agents.trackAgents).not.toHaveBeenCalled();
    expect(event.waitUntil).not.toHaveBeenCalled();
  });

  it("runs only on the public pages and the files for agents", async () => {
    const { config } = await load("client-test");
    expect(config.matcher).toEqual([
      "/",
      "/login",
      "/index.md",
      "/llms.txt",
      "/llms-full.txt",
      "/robots.txt",
      "/sitemap.xml",
      "/.well-known/:path*",
    ]);
  });

  it("serves the landing page as Markdown to a client that asks for it", async () => {
    const { proxy } = await load("");
    const event = { waitUntil: vi.fn() } as unknown as NextFetchEvent;
    const asking = (path: string, accept: string, method = "GET") =>
      proxy(
        new NextRequest(`https://foundry.helix-db.com${path}`, {
          method,
          headers: { accept },
        }),
        event,
      );
    for (const method of ["GET", "HEAD"]) {
      const res = asking("/", "text/markdown", method);
      expect(res?.headers.get("x-middleware-rewrite")).toBe(
        "https://foundry.helix-db.com/index.md",
      );
      expect(res?.headers.get("vary")).toBe("Accept");
    }
    // A browser, another page, or a write, is left alone.
    expect(asking("/", "text/html,*/*;q=0.8")).toBeUndefined();
    expect(asking("/login", "text/markdown")).toBeUndefined();
    expect(asking("/", "text/markdown", "POST")).toBeUndefined();
  });

  it("leaves a local build's responses alone", async () => {
    vi.stubEnv("NEXT_PUBLIC_FOUNDRY_MODE", "local");
    const { proxy } = await load("client-test");
    const event = { waitUntil: vi.fn() } as unknown as NextFetchEvent;
    const req = new NextRequest("http://localhost:3000/", {
      headers: { accept: "text/markdown" },
    });
    expect(proxy(req, event)).toBeUndefined();
    expect(agents.trackAgents).not.toHaveBeenCalled();
  });
});

describe("prefersMarkdown", () => {
  it("prefers Markdown only when it is asked for at least as much as HTML", async () => {
    const { prefersMarkdown } = await load("");
    for (const [accept, markdown] of [
      [null, false],
      ["", false],
      ["text/markdown", true],
      ["text/markdown, text/html;q=0.9", true],
      ["text/html, text/markdown", true],
      ["text/markdown;q=0.5, text/html", false],
      ["text/markdown;q=0", false],
      ["text/markdown;q=0.5, text/*;q=0.4", true],
      ["text/markdown;q=0.5, */*;q=0.8", false],
      ["text/html,application/xhtml+xml,*/*;q=0.8", false],
      ["*/*", false],
      ["text/markdown;q=oops", false],
      ["garbage", false],
    ] as const)
      expect(prefersMarkdown(accept), String(accept)).toBe(markdown);
  });
});
