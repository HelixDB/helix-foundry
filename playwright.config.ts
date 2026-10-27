import { defineConfig } from "@playwright/test";
import { e2eLocalPorts, e2ePorts, localMockEnv, mockEnv } from "./mock/env";

// pnpm e2e: production builds of the app against the mock WFE, on their own
// ports and build directories so a running `pnpm dev:mock` is left alone. The
// hosted site runs tests/e2e; a local build (src/shared/deployment.ts), with
// its own mock as its API, runs tests/e2e/local.
const env = { ...mockEnv(e2ePorts), NEXT_DIST_DIR: ".next-e2e" },
  local = { ...localMockEnv(e2eLocalPorts), NEXT_DIST_DIR: ".next-e2e-local" };

export default defineConfig({
  testDir: "tests/e2e",
  timeout: 60_000,
  // The mock WFE holds one shared state.
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    viewport: { width: 1440, height: 900 },
    trace: "retain-on-failure",
    actionTimeout: 15_000,
  },
  projects: [
    {
      name: "hosted",
      testIgnore: ["support/**", "local/**"],
      use: { baseURL: `http://localhost:${e2ePorts.app}` },
    },
    {
      name: "local",
      testMatch: "local/**/*.spec.ts",
      use: { baseURL: `http://localhost:${e2eLocalPorts.app}` },
    },
  ],
  webServer: [
    {
      command: "tsx mock/server.ts",
      url: `http://127.0.0.1:${e2ePorts.mock}/__mock/stats`,
      env,
      reuseExistingServer: false,
    },
    {
      command: `next build && next start --port ${e2ePorts.app}`,
      url: `http://localhost:${e2ePorts.app}/login`,
      env,
      timeout: 240_000,
      reuseExistingServer: false,
    },
    {
      command: "tsx mock/server.ts",
      url: `http://127.0.0.1:${e2eLocalPorts.mock}/__mock/stats`,
      env: local,
      reuseExistingServer: false,
    },
    {
      command: `next build && next start --port ${e2eLocalPorts.app}`,
      url: `http://localhost:${e2eLocalPorts.app}/`,
      env: local,
      timeout: 240_000,
      reuseExistingServer: false,
    },
  ],
});
