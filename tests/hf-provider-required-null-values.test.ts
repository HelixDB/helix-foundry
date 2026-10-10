import { it, expect, vi, afterEach } from "vitest";
import { Provider } from "../apps/api/src/providers.js";
import { ProviderSchema } from "../packages/shared/src/index.js";
afterEach(() => vi.unstubAllGlobals());
const provider = () =>
  new Provider(
    ProviderSchema.parse({
      provider: "openai",
      model: "test",
      apiKey: "test-only",
    }),
  );
const output = (text: string) =>
  Response.json({
    status: "completed",
    output: [{ content: [{ type: "output_text", text }] }],
    usage: { total_tokens: 1 },
  });
it("preserves required nullable fields and dictionary nulls while omitting optional fields", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      output('{"required":null,"optional":null,"map":{"key":null}}'),
    ),
  );
  const schema = {
    type: "object",
    properties: {
      required: { type: ["string", "null"] },
      optional: { type: "string" },
      map: {
        type: "object",
        additionalProperties: { type: ["string", "null"] },
      },
    },
    required: ["required", "map"],
  };
  expect((await provider().generate("", {}, schema)).value).toEqual({
    required: null,
    map: { key: null },
  });
});

it("retains optional-null cleanup inside union object schemas", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => output('{"optional":null}')),
  );
  const schema = {
    anyOf: [{ type: "object", properties: { optional: { type: "string" } } }],
  };
  expect((await provider().generate("", {}, schema)).value).toEqual({});
});
