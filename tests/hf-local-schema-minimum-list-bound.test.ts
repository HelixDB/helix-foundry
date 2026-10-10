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
it("does not contradict a declared minimum", async () => {
  let format: any;
  const server = createServer(async (req, res) => {
    let body = "";
    for await (const c of req) body += c;
    format = JSON.parse(body).format;
    res.end(JSON.stringify({ message: { content: "{}" } }));
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  try {
    const p = new Provider(
      ProviderSchema.parse({
        provider: "local",
        model: "test",
        baseUrl: `http://127.0.0.1:${(server.address() as any).port}`,
      }),
    );
    await p.generate(
      "",
      {},
      { type: "array", minItems: 20, items: { type: "string" } },
    );
    expect(format.maxItems).toBeGreaterThanOrEqual(format.minItems);
  } finally {
    server.closeAllConnections();
    await new Promise<void>((r) => server.close(() => r()));
  }
});
