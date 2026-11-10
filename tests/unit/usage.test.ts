import { describe, expect, it } from "vitest";
import {
  MASK_PATTERNS,
  maskPath,
  parseUsageBatch,
  USAGE_LIMITS,
  type UsageBatch,
} from "@/shared/usage";

// What a local install's page posts to its site (src/shared/usage.ts): paths
// masked as the hosted tracker masks them, and only batches that fit.

describe("maskPath", () => {
  it.each([
    [
      "/data/connections/conn_1/tables/public.people",
      "/data/connections/*/tables/*",
    ],
    ["/data/connections/conn_1", "/data/connections/*"],
    ["/data/documents/doc_1", "/data/documents/*"],
    ["/ontology/new", "/ontology/new"],
    ["/ontology/new/anything", "/ontology/new/*"],
    ["/ontology/sample", "/ontology/sample"],
    ["/ontology/ont_1/versions/v_1", "/ontology/*/versions/*"],
    ["/ontology/ont_1/generations/g_1", "/ontology/*/generations/*"],
    ["/ontology/ont_1/loads/load_1", "/ontology/*/loads/*"],
    [
      "/ontology/ont_1/loads/load_1/rule-breaks",
      "/ontology/*/loads/*/rule-breaks",
    ],
    ["/ontology/ont_1/documents/doc_1", "/ontology/*/documents/*"],
    ["/ontology/ont_1", "/ontology/*"],
    ["/ontology/ont_1/documents", "/ontology/*/documents"],
    ["/explore/ont_1/entities/ent_1", "/explore/*/entities/*"],
    ["/explore/ont_1/records", "/explore/*/records"],
    ["/ask/ont_1", "/ask/*"],
    ["/ask", "/ask"],
    ["/data", "/data"],
    ["/settings/ai", "/settings/ai"],
    ["/", "/"],
  ])("masks %s as %s", (path, masked) => {
    expect(maskPath(path)).toBe(masked);
    // Masking again changes nothing, as the server does.
    expect(maskPath(masked)).toBe(masked);
  });

  it("takes patterns of whole segments only: literal, * or **", () => {
    for (const pattern of MASK_PATTERNS)
      for (const part of pattern.split("/"))
        expect(part === "*" || part === "**" || !part.includes("*")).toBe(true);
  });
});

const EVENT = {
  id: "8d1f6a3e-0000-4000-8000-000000000003",
  name: "load_started",
  properties: { workspace_id: "ws_1", tables: 3, sample: false },
  path: "/ask/ont_1",
  at: 1_700_000_000_000,
};
const BATCH = {
  visitor: "8d1f6a3e-0000-4000-8000-000000000001",
  session: "8d1f6a3e-0000-4000-8000-000000000002",
  events: [EVENT],
};

describe("parseUsageBatch", () => {
  it("takes a batch that fits, its paths masked", () => {
    expect(parseUsageBatch(BATCH)).toEqual<UsageBatch>({
      ...BATCH,
      events: [{ ...EVENT, path: "/ask/*" }],
    });
    expect(parseUsageBatch({ ...BATCH, events: [] })).toEqual({
      ...BATCH,
      events: [],
    });
  });

  it("takes the most a post may hold", () => {
    const most = {
      ...BATCH,
      events: Array.from({ length: USAGE_LIMITS.events }, () => ({
        ...EVENT,
        properties: Object.fromEntries(
          Array.from({ length: USAGE_LIMITS.properties }, (_, i) => [
            `p${i}`,
            "x".repeat(USAGE_LIMITS.text),
          ]),
        ),
        path: "/" + "x".repeat(USAGE_LIMITS.path - 1),
      })),
    };
    expect(parseUsageBatch(most)?.events).toHaveLength(USAGE_LIMITS.events);
  });

  const event = (change: Record<string, unknown>) => ({
    ...BATCH,
    events: [{ ...EVENT, ...change }],
  });
  it.each<[string, unknown]>([
    ["nothing", undefined],
    ["an array", [BATCH]],
    ["no visitor", { ...BATCH, visitor: undefined }],
    ["a visitor that isn't an id", { ...BATCH, visitor: "ada@example.com" }],
    ["a session too long", { ...BATCH, session: "a".repeat(65) }],
    ["no events", { ...BATCH, events: undefined }],
    [
      "too many events",
      { ...BATCH, events: Array(USAGE_LIMITS.events + 1).fill(EVENT) },
    ],
    ["an event that isn't an object", { ...BATCH, events: ["page_viewed"] }],
    ["an event without an id", event({ id: undefined })],
    ["an event named in capitals", event({ name: "Page_Viewed" })],
    ["an event name too long", event({ name: "a".repeat(65) })],
    ["a relative path", event({ path: "ask" })],
    ["a full URL", event({ path: "http://localhost:3000/ask" })],
    ["a path too long", event({ path: "/" + "x".repeat(USAGE_LIMITS.path) })],
    ["no time", event({ at: "now" })],
    ["a time that isn't one", event({ at: Number.NaN })],
    ["properties as a list", event({ properties: ["a"] })],
    ["no properties", event({ properties: null })],
    [
      "too many properties",
      event({
        properties: Object.fromEntries(
          Array.from({ length: USAGE_LIMITS.properties + 1 }, (_, i) => [
            `p${i}`,
            1,
          ]),
        ),
      }),
    ],
    ["a property key in capitals", event({ properties: { Email: "a" } })],
    ["a nested property", event({ properties: { goal: { text: "a" } } })],
    ["a null property", event({ properties: { goal: null } })],
    ["an infinite property", event({ properties: { n: Infinity } })],
    [
      "a property text too long",
      event({ properties: { goal: "x".repeat(USAGE_LIMITS.text + 1) } }),
    ],
  ])("refuses %s", (_, value) => {
    expect(parseUsageBatch(value)).toBeNull();
  });
});
