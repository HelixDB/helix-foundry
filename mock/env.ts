// The environment for running the app against the mock WFE (pnpm dev:mock and
// pnpm e2e). Every value is a mock-only fixture: none is a secret, and none
// works anywhere but against the mock. WorkOS calls (JWKS, refresh, users,
// logout) go to the mock too, through AuthKit's WORKOS_API_* settings.

export type MockPorts = { mock: number; app: number };

export const devPorts: MockPorts = { mock: 4010, app: 3000 };
// E2E_MOCK_PORT and E2E_APP_PORT move a run off the defaults, so several
// checkouts can run the suite at once.
export const e2ePorts: MockPorts = {
  mock: Number(process.env.E2E_MOCK_PORT) || 4110,
  app: Number(process.env.E2E_APP_PORT) || 3100,
};

// The local build's suite (tests/e2e/local) runs a second app and mock.
export const e2eLocalPorts: MockPorts = {
  mock: Number(process.env.E2E_LOCAL_MOCK_PORT) || 4111,
  app: Number(process.env.E2E_LOCAL_APP_PORT) || 3101,
};

/** A local build (src/shared/deployment.ts) against the mock as its API. */
export function localMockEnv({ mock, app }: MockPorts): Record<string, string> {
  const mockOrigin = `http://127.0.0.1:${mock}`,
    token = "mock-only-local-token-0123456789abcdef";
  return {
    NEXT_PUBLIC_APP_ENV: "mock",
    NEXT_PUBLIC_FOUNDRY_MODE: "local",
    HELIX_WFE_ORIGIN: mockOrigin,
    FOUNDRY_ORIGIN: `http://localhost:${app}`,
    FOUNDRY_LOCAL_TOKEN: token,
    MOCK_LOCAL_TOKEN: token,
    // The install's AI provider, found on its machine (FoundryAI.detected),
    // or named by local/.env's FOUNDRY_AI_PROVIDER with `aiDefaultNamed:
    // true` (mock/routes/ai.ts); `aiDefault: false` takes it away.
    MOCK_AI_DEFAULT: "anthropic",
    MOCK_PORT: String(mock),
    MOCK_ORIGIN: mockOrigin,
    NEXT_TELEMETRY_DISABLED: "1",
  };
}

export function mockEnv({ mock, app }: MockPorts): Record<string, string> {
  const mockOrigin = `http://127.0.0.1:${mock}`,
    appOrigin = `http://localhost:${app}`;
  return {
    NEXT_PUBLIC_APP_ENV: "mock",
    HELIX_WFE_ORIGIN: mockOrigin,
    WORKOS_API_HOSTNAME: "127.0.0.1",
    WORKOS_API_PORT: String(mock),
    WORKOS_API_HTTPS: "false",
    WORKOS_CLIENT_ID: "client_mock_foundry",
    WORKOS_API_KEY: "sk_test_mock_foundry",
    WORKOS_COOKIE_PASSWORD: "mock-only-cookie-password-0123456789abcdef",
    // Loopback http: browsers refuse __Host- cookies without https.
    WORKOS_COOKIE_NAME: "foundry-session",
    WORKOS_COOKIE_MAX_AGE: "2592000",
    WORKOS_COOKIE_SAMESITE: "lax",
    NEXT_PUBLIC_WORKOS_REDIRECT_URI: `${appOrigin}/login/callback`,
    HELIX_CSRF_SECRET: "mock-only-csrf-secret-0123456789abcdefghij",
    // Documentation addresses (RFC 5737), never a real stamp's.
    NEXT_PUBLIC_HELIX_EGRESS_IPS: "192.0.2.10,192.0.2.11",
    MOCK_PORT: String(mock),
    MOCK_ORIGIN: mockOrigin,
    NEXT_TELEMETRY_DISABLED: "1",
  };
}
