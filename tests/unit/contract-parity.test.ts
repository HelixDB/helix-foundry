import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

// contract/ is what the site is written and tested against, and backend/ what
// it talks to: `pnpm contract:sync` copies one from the other, so they never
// differ. A contract change that skips it fails here.
const COPIED: ReadonlyArray<readonly [site: string, backend: string]> = [
  [
    "contract/foundry.swagger.json",
    "backend/pb/gen/openapiv2/helix/foundry/v1/foundry.swagger.json",
  ],
  ...[
    "read_request.json",
    "read_result.json",
    "ask_request.json",
    "ask_event.json",
    "ask_result.json",
    "probe_result.json",
    "catalog.json",
    "VERSION",
  ].map(
    (file) =>
      [
        `contract/ontology_agent/${file}`,
        `backend/pb/proto/ontology_agent/v1/schema/${file}`,
      ] as const,
  ),
];

describe("the vendored contract", () => {
  it.each(COPIED)("%s is %s", (site, backend) => {
    expect(readFileSync(site, "utf8")).toBe(readFileSync(backend, "utf8"));
  });
});
