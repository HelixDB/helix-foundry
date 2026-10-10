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
import { createServer } from "node:http";
import { once } from "node:events";
it("rechecks digest after moving to the pinned model", async () => {
  const server = createServer((_req, res) =>
    res.end(JSON.stringify({ message: { content: "{}" } })),
  );
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  try {
    const fetchMock = vi.fn(async () =>
      Response.json({ models: [{ name: "qwen3:4b", digest: "wrong" }] }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const p = new Provider(
      ProviderSchema.parse({
        provider: "local",
        model: "test",
        baseUrl: `http://127.0.0.1:${(server.address() as any).port}`,
      }),
    );
    await p.generate("", {}, {});
    p.settings.model = "qwen3:4b";
    await expect(p.generate("", {}, {})).rejects.toThrow("digest");
    expect(fetchMock).toHaveBeenCalledTimes(1);
  } finally {
    server.closeAllConnections();
    await new Promise<void>((r) => server.close(() => r()));
  }
});
