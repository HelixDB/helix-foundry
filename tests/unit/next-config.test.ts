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
  const DEV_ORIGIN = "https://dev.foundry.helix-db.com",
    PROD_ORIGIN = "https://foundry.helix-db.com";
  it.each([
    {
      name: "dev",
      appEnv: "dev",
      mode: undefined,
      origin: DEV_ORIGIN,
      id: "dev",
    },
    {
      name: "prod",
      appEnv: " Prod ",
      mode: undefined,
      origin: PROD_ORIGIN,
      id: "prod",
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
      const ids = await load({
        NEXT_PUBLIC_APP_ENV: id || "dev",
        NEXT_PUBLIC_FOUNDRY_MODE: undefined,
        NEXT_PUBLIC_WORKOS_REDIRECT_URI: `${id === "prod" ? PROD_ORIGIN : DEV_ORIGIN}/login/callback`,
      });
      expect(env.NEXT_PUBLIC_DATABUDDY_CLIENT_ID).toBe(
        id ? ids.NEXT_PUBLIC_DATABUDDY_CLIENT_ID : "",
      );
      if (id) expect(env.NEXT_PUBLIC_DATABUDDY_CLIENT_ID).not.toBe("");
    },
  );

  it("gives each stamp a client id of its own", async () => {
    const dev = await load({
      NEXT_PUBLIC_APP_ENV: "dev",
      NEXT_PUBLIC_WORKOS_REDIRECT_URI: `${DEV_ORIGIN}/login/callback`,
    });
    const prod = await load({
      NEXT_PUBLIC_APP_ENV: "prod",
      NEXT_PUBLIC_WORKOS_REDIRECT_URI: `${PROD_ORIGIN}/login/callback`,
    });
    expect(dev.NEXT_PUBLIC_DATABUDDY_CLIENT_ID).toMatch(/^[0-9a-f-]{36}$/);
    expect(prod.NEXT_PUBLIC_DATABUDDY_CLIENT_ID).toMatch(/^[0-9a-f-]{36}$/);
    expect(dev.NEXT_PUBLIC_DATABUDDY_CLIENT_ID).not.toBe(
      prod.NEXT_PUBLIC_DATABUDDY_CLIENT_ID,
    );
  });

  it("names an origin the prompt can append a path to", async () => {
    const url = (await load({ NEXT_PUBLIC_APP_ENV: "dev" }))
      .NEXT_PUBLIC_HELIX_EVENTS_URL!;
    expect(new URL(url).origin).toBe(url);
  });
});
