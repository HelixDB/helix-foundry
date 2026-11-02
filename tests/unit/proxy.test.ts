import { afterEach, describe, expect, it, vi } from "vitest";
import type { NextFetchEvent, NextRequest } from "next/server";

// src/proxy.ts: AI crawlers reading the public pages go to Databuddy's AI
// Agents page, after the response; a build without a client id sends nothing.

const agents = vi.hoisted(() => ({ trackAgents: vi.fn(async () => {}) }));
vi.mock("@databuddy/sdk/agents", () => agents);

const request = new Request("https://foundry.helix-db.com/", {
  headers: { "user-agent": "GPTBot/1.2" },
}) as NextRequest;

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

  it("runs only on the public pages", async () => {
    const { config } = await load("client-test");
    expect(config.matcher).toEqual(["/", "/login"]);
  });
});
