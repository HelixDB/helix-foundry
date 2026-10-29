import type { NextConfig } from "next";

// dev | prod; anything else (local, mock) behaves like dev.
const appEnv = process.env.NEXT_PUBLIC_APP_ENV?.trim().toLowerCase();

// src/shared/deployment.ts: unset is the hosted site; a typo must not build it.
const mode = process.env.NEXT_PUBLIC_FOUNDRY_MODE;
if (mode && mode !== "local")
  throw new Error(
    `NEXT_PUBLIC_FOUNDRY_MODE must be unset or "local", not "${mode}"`,
  );

// Databuddy, the hosted site's product analytics (src/web/analytics.ts): each
// stamp's site by its client id, which is public, and only in a build for
// the stamp's own origin (its WorkOS redirect URI's), so its values built
// anywhere else (pnpm docs:media builds prod's against the mock, pnpm dev
// dev's) load no tracker. A local build, the mock and e2e have none.
const DATABUDDY = {
  dev: {
    origin: "https://dev.foundry.helix-db.com",
    clientId: "a4288db1-0535-4fdd-8cae-bc525f19a172",
  },
  prod: {
    origin: "https://foundry.helix-db.com",
    clientId: "81ea8e9c-0f0f-4696-885f-2fb34405e69b",
  },
};
const databuddy =
  appEnv === "dev" || appEnv === "prod" ? DATABUDDY[appEnv] : undefined;
const origin = URL.parse(
  process.env.NEXT_PUBLIC_WORKOS_REDIRECT_URI ?? "",
)?.origin;

const nextConfig: NextConfig = {
  // e2e builds into its own directory so it never clobbers `pnpm dev`.
  distDir: process.env.NEXT_DIST_DIR || ".next",
  // A local build runs as its own container (Dockerfile): only what it needs.
  output: mode === "local" ? "standalone" : undefined,
  // `pnpm dev` serves https://local.foundry.helix-db.com:3000.
  allowedDevOrigins: ["local.foundry.helix-db.com"],
  poweredByHeader: false,
  // AGENTS.md is ours: next dev must not write its own.
  agentRules: false,
  // The dev badge sits over the sidebar's account menu.
  devIndicators: false,
  env: {
    // CreateWorkspace requires a region; the stamp's default workspace region,
    // or a local API's only one.
    NEXT_PUBLIC_DEFAULT_WORKSPACE_REGION:
      mode === "local"
        ? "local"
        : appEnv === "prod"
          ? "us-east-1"
          : "us-east-2",
    // Where role changes, member removal and invitations live.
    NEXT_PUBLIC_HELIX_DASHBOARD_URL:
      appEnv === "prod"
        ? "https://prod.app.helix-db.com"
        : "https://dev.app.helix-db.com",
    // The Helix MCP, which "Set up with your agent"'s prompt has agents add.
    NEXT_PUBLIC_HELIX_MCP_URL:
      appEnv === "prod"
        ? "https://mcp.helix-db.com/mcp"
        : "https://mcp.dev.helix-db.com/mcp",
    // Acme Corp's PostgreSQL database, which "Use the sample" connects
    // (src/web/areas/ontology/sample.ts): the local stack's acme-db, or
    // the stamp's own (helix-hyperscale backend/infra/sample-db).
    NEXT_PUBLIC_SAMPLE_DB_HOST:
      mode === "local"
        ? "acme-db"
        : appEnv === "prod"
          ? "helix-prod-sample.csf4assug8lo.us-east-1.rds.amazonaws.com"
          : "helix-dev-sample.cdmqgow4207f.us-east-2.rds.amazonaws.com",
    // Where API sources take pieces over HTTP (Foundry's foundry-events
    // role), an origin with no trailing slash. Empty in local
    // mode: hides HTTP.
    NEXT_PUBLIC_HELIX_EVENTS_URL:
      process.env.NEXT_PUBLIC_FOUNDRY_MODE === "local"
        ? ""
        : appEnv === "prod"
          ? "https://ingest.helix-db.com"
          : "https://ingest.dev.helix-db.com",
    NEXT_PUBLIC_DATABUDDY_CLIENT_ID:
      mode !== "local" && databuddy && databuddy.origin === origin
        ? databuddy.clientId
        : "",
  },
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "X-Frame-Options", value: "DENY" },
          { key: "Content-Security-Policy", value: "frame-ancestors 'none'" },
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
        ],
      },
    ];
  },
};

export default nextConfig;
