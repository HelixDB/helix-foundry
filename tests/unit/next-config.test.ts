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

  // Databuddy's client id: each stamp's, only in a build for its own origin.
  // The "Foundry Dev" and "Foundry" sites in Databuddy.
  const DEV_ID = "a4288db1-0535-4fdd-8cae-bc525f19a172",
    PROD_ID = "81ea8e9c-0f0f-4696-885f-2fb34405e69b";
  const DEV_ORIGIN = "https://dev.foundry.helix-db.com",
    PROD_ORIGIN = "https://foundry.helix-db.com";
  it.each([
    {
      name: "dev",
      appEnv: "dev",
      mode: undefined,
      origin: DEV_ORIGIN,
      id: DEV_ID,
    },
    {
      name: "prod",
      appEnv: " Prod ",
      mode: undefined,
      origin: PROD_ORIGIN,
      id: PROD_ID,
    },
    {
      name: "prod's values on dev's origin",
      appEnv: "prod",
      mode: undefined,
      origin: DEV_ORIGIN,
      id: "",
    },
    {
      name: "pnpm dev",
      appEnv: "dev",
      mode: undefined,
      origin: "https://local.foundry.helix-db.com:3000",
      id: "",
    },
    {
      name: "pnpm docs:media",
      appEnv: "prod",
      mode: undefined,
      origin: "http://localhost:3100",
      id: "",
    },
    {
      name: "the mock",
      appEnv: "mock",
      mode: undefined,
      origin: "http://localhost:3100",
      id: "",
    },
    {
      name: "an unset app env",
      appEnv: undefined,
      mode: undefined,
      origin: PROD_ORIGIN,
      id: "",
    },
    {
      name: "no redirect URI",
      appEnv: "prod",
      mode: undefined,
      origin: undefined,
      id: "",
    },
    {
      name: "neither",
      appEnv: undefined,
      mode: undefined,
      origin: undefined,
      id: "",
    },
    {
      name: "local mode on prod",
      appEnv: "prod",
      mode: "local",
      origin: PROD_ORIGIN,
      id: "",
    },
  ])(
    "sets NEXT_PUBLIC_DATABUDDY_CLIENT_ID for $name",
    async ({ appEnv, mode, origin, id }) => {
      const env = await load({
        NEXT_PUBLIC_APP_ENV: appEnv,
        NEXT_PUBLIC_FOUNDRY_MODE: mode,
        NEXT_PUBLIC_WORKOS_REDIRECT_URI: origin && `${origin}/login/callback`,
      });
      expect(env.NEXT_PUBLIC_DATABUDDY_CLIENT_ID).toBe(id);
    },
  );

  it("names an origin the prompt can append a path to", async () => {
    const url = (await load({ NEXT_PUBLIC_APP_ENV: "dev" }))
      .NEXT_PUBLIC_HELIX_EVENTS_URL!;
    expect(new URL(url).origin).toBe(url);
  });
});
