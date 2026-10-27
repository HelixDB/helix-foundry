import { defineConfig } from "@playwright/test";

// tests/stack against a running local stack (local/compose.yaml with
// tests/stack/compose.yaml), at FOUNDRY_URL. No mock: the real API, workers,
// ontology agent, store and HelixDB. tests/stack/run.sh (pnpm e2e:stack)
// starts a fresh stack and runs this in Playwright's image.
export default defineConfig({
  testDir: "tests/stack",
  timeout: 30 * 60_000,
  workers: 1,
  fullyParallel: false,
  reporter: "list",
  use: {
    baseURL: process.env.FOUNDRY_URL || "http://localhost:3000",
    viewport: { width: 1440, height: 900 },
    trace: "retain-on-failure",
    actionTimeout: 30_000,
  },
});
