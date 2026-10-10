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
it("rejects missing requested submit tool", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      Response.json({ content: [{ type: "text", text: "I cannot respond" }] }),
    ),
  );
  const p = new Provider(
    ProviderSchema.parse({
      provider: "claude",
      model: "test",
      apiKey: "test-only",
    }),
  );
  await expect(p.generate("", {}, {})).rejects.toThrow(
    "structured tool result",
  );
});
