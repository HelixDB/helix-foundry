import { afterEach, describe, expect, it, vi } from "vitest";

// next.config.ts reads the environment once, when it loads: each case stubs
// it, then loads a fresh copy.
const load = async (env: Record<string, string | undefined>) => {
  for (const [name, value] of Object.entries(env)) vi.stubEnv(name, value);
  vi.resetModules();
  return (await import("../../next.config")).default.env ?? {};
};

afterEach(() => vi.unstubAllEnvs());

describe("next.config.ts", () => {
  const dev = "https://ingest.dev.helix-db.com";
  const prod = "https://ingest.helix-db.com";

  it.each([
    { name: "dev", appEnv: "dev", mode: undefined, url: dev },
    { name: "an unset app env", appEnv: undefined, mode: undefined, url: dev },
    { name: "the mock", appEnv: "mock", mode: undefined, url: dev },
    { name: "prod", appEnv: "prod", mode: undefined, url: prod },
    {
      name: "prod, spaced and capitalised",
      appEnv: " Prod ",
      mode: undefined,
      url: prod,
    },
    { name: "local mode", appEnv: undefined, mode: "local", url: "" },
    { name: "local mode on dev", appEnv: "dev", mode: "local", url: "" },
    { name: "local mode on prod", appEnv: "prod", mode: "local", url: "" },
  ])(
    "sets NEXT_PUBLIC_HELIX_EVENTS_URL for $name",
    async ({ appEnv, mode, url }) => {
      const env = await load({
        NEXT_PUBLIC_APP_ENV: appEnv,
        NEXT_PUBLIC_FOUNDRY_MODE: mode,
      });
      expect(env.NEXT_PUBLIC_HELIX_EVENTS_URL).toBe(url);
    },
  );

  it("names an origin the prompt can append a path to", async () => {
    const url = (await load({ NEXT_PUBLIC_APP_ENV: "dev" }))
      .NEXT_PUBLIC_HELIX_EVENTS_URL!;
    expect(new URL(url).origin).toBe(url);
  });
});
