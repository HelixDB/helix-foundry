import { describe, expect, it } from "vitest";
import { shareDoor } from "@/server/share";

// Where a published dashboard's page asks for its snapshot
// (src/server/share.ts): the foundry-events door at a trusted origin, and
// none in a local build.

describe("shareDoor", () => {
  const prod = "https://ingest.helix-db.com";
  it.each([
    ["the stamp's door", {}, prod, false, prod],
    [
      "HELIX_INGEST_ORIGIN over the stamp's",
      { HELIX_INGEST_ORIGIN: "http://127.0.0.1:4110" },
      prod,
      false,
      "http://127.0.0.1:4110",
    ],
    ["none in a local build", {}, prod, true, null],
    ["none without a door", {}, "", false, null],
    ["none over plain http", {}, "http://ingest.example.com", false, null],
    ["none with a path", {}, `${prod}/v1`, false, null],
    ["none for what isn't an address", {}, "ingest", false, null],
  ])("%s", (_, env, eventsUrl, local, door) => {
    expect(shareDoor(env, eventsUrl, local)).toBe(door);
  });
});
