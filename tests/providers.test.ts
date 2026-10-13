import { it, expect, vi, afterEach } from "vitest";
import { createServer } from "node:http";
import { once } from "node:events";
import { Provider } from "../apps/api/src/providers.js";
import { ProviderSchema } from "../packages/shared/src/index.js";
afterEach(() => vi.unstubAllGlobals());
for (const provider of ["local", "claude", "openai"] as const)
  it(`uses the native ${provider} protocol without fallbacks`, async () => {
    const requests: any[] = [];
    // The local provider talks to Ollama over node:http, not fetch.
    const ollama = createServer(async (req, res) => {
      let body = "";
      for await (const chunk of req) body += chunk;
      requests.push({
        url: req.url,
        body: JSON.parse(body),
        headers: req.headers,
      });
      res.setHeader("content-type", "application/json");
      res.end(
        JSON.stringify({
          message: { content: '{"ready":true}' },
          prompt_eval_count: 2,
          eval_count: 3,
          done_reason: "stop",
        }),
      );
    });
    ollama.listen(0, "127.0.0.1");
    await once(ollama, "listening");
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url, init) => {
        requests.push({
          url,
          body: JSON.parse(init.body),
          headers: init.headers,
        });
        return Response.json(
          provider === "local"
            ? {
                message: { content: '{"ready":true}' },
                prompt_eval_count: 2,
                eval_count: 3,
              }
            : provider === "claude"
              ? {
                  content: [
                    {
                      type: "tool_use",
                      name: "submit",
                      input: { ready: true },
                    },
                  ],
                  usage: { input_tokens: 2, output_tokens: 3 },
                }
              : {
                  output: [
                    {
                      content: [
                        { type: "output_text", text: '{"ready":true}' },
                      ],
                    },
                  ],
                  usage: { total_tokens: 5 },
                },
        );
      }),
    );
    const p = new Provider(
      ProviderSchema.parse({
        provider,
        model: "test",
        apiKey: "test-only",
        baseUrl:
          provider === "local"
            ? "http://127.0.0.1:" + (ollama.address() as any).port
            : undefined,
      }),
    );
    const r = await p.generate(
      "system",
      { safe: "profile" },
      {
        type: "object",
        properties: { ready: { type: "boolean" } },
        required: ["ready"],
        additionalProperties: false,
      },
    );
    expect(r).toEqual({ value: { ready: true }, tokens: 5 });
    expect(requests).toHaveLength(1);
    expect(requests[0].url).toContain(
      provider === "local"
        ? "/api/chat"
        : provider === "claude"
          ? "/v1/messages"
          : "/v1/responses",
    );
    if (provider === "openai") {
      expect(requests[0].body.store).toBe(false);
      expect(requests[0].body.text.format.strict).toBe(true);
    }
    if (provider === "local") expect(requests[0].body.keep_alive).toBe("30s");
    if (provider === "local")
      expect(requests[0].body.options).toEqual({
        temperature: 0,
        num_ctx: 8192,
        num_predict: expect.any(Number),
      });
    ollama.close();
  });
it("sizes the local context window to the prompt and refuses what cannot fit", async () => {
  const seen: number[] = [];
  const ollama = createServer(async (req, res) => {
    let body = "";
    for await (const chunk of req) body += chunk;
    const options = JSON.parse(body).options;
    seen.push(options.num_ctx);
    res.end(
      JSON.stringify({
        message: { content: '{"ready":true}' },
        prompt_eval_count: 1,
        eval_count: 1,
        done_reason: options.num_ctx === 16384 ? "length" : "stop",
      }),
    );
  });
  ollama.listen(0, "127.0.0.1");
  await once(ollama, "listening");
  const p = new Provider(
    ProviderSchema.parse({
      provider: "local",
      model: "test",
      baseUrl: "http://127.0.0.1:" + (ollama.address() as any).port,
    }),
  );
  try {
    await expect(
      p.generate("", { data: "x".repeat(20_000) }, {}),
    ).rejects.toThrow("ran out of room");
    expect(seen).toEqual([16384]);
    await expect(
      p.generate("", { data: "x".repeat(200_000) }, {}),
    ).rejects.toThrow("Too much data for the local model");
    expect(seen).toHaveLength(1);
  } finally {
    ollama.close();
  }
});
it("reports provider outage without contacting another provider", async () => {
  const fetchMock = vi.fn(async () => new Response("", { status: 503 }));
  vi.stubGlobal("fetch", fetchMock);
  await expect(
    new Provider(
      ProviderSchema.parse({
        provider: "openai",
        model: "test",
        apiKey: "test",
      }),
    ).generate("", {}, {}),
  ).rejects.toThrow("HTTP 503");
  expect(fetchMock).toHaveBeenCalledTimes(1);
});
it("sends Azure OpenAI requests to the resource's v1 endpoint with the deployment as model", async () => {
  const fetchMock = vi.fn(async () =>
    Response.json({
      output: [{ content: [{ type: "output_text", text: '{"ready":true}' }] }],
      usage: { total_tokens: 5 },
    }),
  );
  vi.stubGlobal("fetch", fetchMock);
  // The resource endpoint, with or without the `/openai/v1` route.
  for (const baseUrl of [
    "https://example.openai.azure.com/",
    "https://example.openai.azure.com/openai/v1",
  ]) {
    fetchMock.mockClear();
    const azure = new Provider(
      ProviderSchema.parse({
        provider: "azure",
        model: "gpt-6.1-sol",
        apiKey: "azure-key",
        baseUrl,
      }),
    );
    expect(await azure.generate("", {}, {})).toEqual({
      value: { ready: true },
      tokens: 5,
    });
    const [url, init] = fetchMock.mock.calls[0] as any;
    expect(url).toBe("https://example.openai.azure.com/openai/v1/responses");
    // Azure's v1 API reads the key from api-key only, and a redirect
    // cannot carry it to another host.
    expect(init.headers["api-key"]).toBe("azure-key");
    expect(init.headers).not.toHaveProperty("authorization");
    expect(init.redirect).toBe("error");
    expect(JSON.parse(init.body).model).toBe("gpt-6.1-sol");
    expect(JSON.parse(init.body).text.format.strict).toBe(true);
  }
  expect(
    ProviderSchema.safeParse({
      provider: "azure",
      model: "gpt-6.1-sol",
      apiKey: "azure-key",
    }).success,
  ).toBe(false);
  await expect(
    new Provider({
      ...ProviderSchema.parse({
        provider: "azure",
        model: "gpt-6.1-sol",
        apiKey: "azure-key",
        baseUrl: "https://example.openai.azure.com",
      }),
      baseUrl: undefined,
    }).generate("", {}, {}),
  ).rejects.toThrow("Azure OpenAI endpoint is required");
  expect(fetchMock).toHaveBeenCalledTimes(1);

  fetchMock.mockClear();
  await new Provider(
    ProviderSchema.parse({
      provider: "openai",
      model: "gpt-5.4-mini",
      apiKey: "sk-test",
    }),
  ).generate("", {}, {});
  const [plainUrl, plainInit] = fetchMock.mock.calls[0] as any;
  expect(plainUrl).toBe("https://api.openai.com/v1/responses");
  expect(plainInit.headers["api-key"]).toBeUndefined();
});
