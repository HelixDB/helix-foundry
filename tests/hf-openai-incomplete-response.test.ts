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
it("rejects a valid JSON prefix when provider reports incomplete", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      Response.json({
        status: "incomplete",
        incomplete_details: { reason: "max_output_tokens" },
        output: [
          { content: [{ type: "output_text", text: '{"ready":true}' }] },
        ],
      }),
    ),
  );
  await expect(provider().generate("", {}, {})).rejects.toThrow(
    "did not complete",
  );
});
