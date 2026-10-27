import { defineConfig } from "@playwright/test";
import e2e from "./playwright.config";
import { e2ePorts, mockEnv } from "./mock/env";

// pnpm docs:media: the screenshots and walkthrough videos of the public docs
// (docs.helix-db.com's Helix Foundry tab), taken from a production build
// against the mock, as pnpm e2e runs it. They are written to DOCS_MEDIA_DIR
// (docs-media/ by default), one folder per docs section, and published with
// pnpm docs:media:upload.
//
// The docs describe prod, so the app is built as prod's (NEXT_PUBLIC_APP_ENV:
// its MCP, API source and dashboard addresses, its load caps) and the mock
// serves prod's stamp (MOCK_STAMP: its region and monthly caps). Only the
// hosted site is captured, in a build directory of its own.
const env = {
  ...mockEnv(e2ePorts),
  NEXT_PUBLIC_APP_ENV: "prod",
  MOCK_STAMP: "prod",
  NEXT_DIST_DIR: ".next-docs",
};

export default defineConfig({
  ...e2e,
  testDir: "tests/docs",
  testIgnore: "support.ts",
  retries: 0,
  timeout: 180_000,
  reporter: "list",
  use: {
    ...e2e.use,
    baseURL: `http://localhost:${e2ePorts.app}`,
    viewport: { width: 1280, height: 800 },
    deviceScaleFactor: 2,
  },
  projects: [{ name: "docs" }],
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
  ],
});
