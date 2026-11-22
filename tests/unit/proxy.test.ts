import { afterEach, describe, expect, it, vi } from "vitest";
import { NextRequest, type NextFetchEvent } from "next/server";

// src/proxy.ts: AI crawlers reading the public pages go to Databuddy's AI
// Agents page, after the response; a build without a client id sends nothing.

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
      "/llms.txt",
      "/llms-full.txt",
      "/setup.md",
      "/robots.txt",
      "/.well-known/:path*",
    ]);
  });

  it("leaves a local build's responses alone", async () => {
    vi.stubEnv("NEXT_PUBLIC_FOUNDRY_MODE", "local");
    const { proxy } = await load("client-test");
    const event = { waitUntil: vi.fn() } as unknown as NextFetchEvent;
    const req = new NextRequest("http://localhost:3000/");
    expect(proxy(req, event)).toBeUndefined();
    expect(agents.trackAgents).not.toHaveBeenCalled();
  });
});
