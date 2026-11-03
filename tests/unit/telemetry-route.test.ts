import { afterEach, describe, expect, it, vi } from "vitest";
import { telemetryEnabled } from "@/server/telemetry";

// A local install's usage analytics: on unless its owner turned them off
// (src/server/telemetry.ts), as GET /telemetry answers the page.

describe("telemetryEnabled", () => {
  it.each([
    [{}, true],
    [{ FOUNDRY_TELEMETRY: "" }, true],
    [{ FOUNDRY_TELEMETRY: "on" }, true],
    [{ FOUNDRY_TELEMETRY: "off" }, false],
    [{ FOUNDRY_TELEMETRY: " OFF " }, false],
    [{ FOUNDRY_TELEMETRY: "false" }, false],
    [{ FOUNDRY_TELEMETRY: "no" }, false],
    [{ FOUNDRY_TELEMETRY: "0" }, false],
    [{ DO_NOT_TRACK: "1" }, false],
    [{ DO_NOT_TRACK: "true" }, false],
    [{ DO_NOT_TRACK: "0" }, true],
    [{ DO_NOT_TRACK: "false" }, true],
    [{ DO_NOT_TRACK: "" }, true],
    [{ FOUNDRY_TELEMETRY: "on", DO_NOT_TRACK: "1" }, false],
  ])("reads %o as %s", (env, enabled) => {
    expect(telemetryEnabled(env)).toBe(enabled);
  });
});

describe("GET /telemetry", () => {
  afterEach(() => vi.unstubAllEnvs());

  it("answers the site's environment when asked, never cached", async () => {
    const { GET } = await import("@/app/telemetry/route");
    vi.stubEnv("FOUNDRY_TELEMETRY", "");
    vi.stubEnv("DO_NOT_TRACK", "");
    let res = GET();
    expect(res.headers.get("cache-control")).toBe("no-store");
    expect(await res.json()).toEqual({ enabled: true });
    vi.stubEnv("FOUNDRY_TELEMETRY", "off");
    res = GET();
    expect(await res.json()).toEqual({ enabled: false });
  });
});
