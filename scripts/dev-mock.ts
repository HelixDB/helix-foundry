import concurrently from "concurrently";
import { devPorts, localMockEnv, mockEnv } from "../mock/env";

// pnpm dev:mock: the mock WFE and `next dev`, wired together. Open
// http://localhost:3000 and sign in with any provider; no accounts needed.
// Acme's General starts with a CSV and a JSON document here, not in tests.
// pnpm dev:mock:local (`local`): a local build, already signed in as Ada.
const env = (process.argv[2] === "local" ? localMockEnv : mockEnv)(devPorts);
const { result } = concurrently(
  [
    {
      name: "mock",
      command: "tsx watch mock/server.ts",
      env: { ...env, MOCK_DOCUMENT_FIXTURES: "1" },
    },
    { name: "web", command: `next dev --port ${devPorts.app}`, env },
  ],
  { killOthersOn: ["failure", "success"], prefixColors: ["magenta", "cyan"] },
);
result.catch(() => process.exit(1));
