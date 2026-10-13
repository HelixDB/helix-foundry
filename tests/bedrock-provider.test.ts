import { afterEach, expect, it, vi } from "vitest";
import {
  ProviderSchema,
  OnboardingProviderSchema,
  bedrockEndpoint,
  bedrockRegion,
  describeActivity,
} from "../packages/shared/src/index.js";
import { Provider, canReuseProviderKey } from "../apps/api/src/providers.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore } from "../apps/api/src/store.js";
import { prepareProvider } from "../apps/api/src/setup-jobs.js";
import { unseal } from "../apps/api/src/security.js";

const bedrock = {
  provider: "bedrock" as const,
  model: "us.anthropic.claude-sonnet-4-6",
  baseUrl: "https://bedrock-runtime.us-east-1.amazonaws.com",
};
const key = "bedrock-api-key-first",
  newKey = "bedrock-api-key-second";
// A Converse reply that calls the submit tool.
const converse = (input: unknown, extra: Record<string, unknown> = {}) =>
  Response.json({
    output: {
      message: {
        role: "assistant",
        content: [{ toolUse: { toolUseId: "t1", name: "submit", input } }],
      },
    },
    stopReason: "tool_use",
    usage: { inputTokens: 30, outputTokens: 12, totalTokens: 42 },
    ...extra,
  });
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("accepts only AWS's own Bedrock Runtime endpoints in both provider APIs", () => {
  for (const contract of [ProviderSchema, OnboardingProviderSchema]) {
    for (const baseUrl of [
      undefined,
      "not-a-url",
      "http://bedrock-runtime.us-east-1.amazonaws.com",
      "https://bedrock-runtime.us-east-1.amazonaws.com.example.com",
      "https://example.com/bedrock-runtime.us-east-1.amazonaws.com",
      "https://bedrock.us-east-1.amazonaws.com",
      "https://bedrock-runtime.us-east-1.amazonaws.com:8443",
      "https://user:password@bedrock-runtime.us-east-1.amazonaws.com",
      "https://bedrock-runtime.us-east-1.amazonaws.com/model",
      "https://bedrock-runtime.us-east-1.amazonaws.com?key=secret",
      "https://bedrock-runtime.us-east-1.amazonaws.com#secret",
    ])
      expect(
        contract.safeParse({ ...bedrock, baseUrl }).success,
        String(baseUrl),
      ).toBe(false);
    for (const baseUrl of [
      bedrock.baseUrl,
      bedrock.baseUrl + "/",
      "https://bedrock-runtime-fips.us-gov-west-1.amazonaws.com",
      "https://vpce-0a1b2c3d-x1y2z3.bedrock-runtime.eu-central-1.vpce.amazonaws.com",
      "https://bedrock-runtime.cn-north-1.amazonaws.com.cn",
    ])
      expect(contract.safeParse({ ...bedrock, baseUrl }).success, baseUrl).toBe(
        true,
      );
  }
  expect(bedrockEndpoint("eu-west-3")).toBe(
    "https://bedrock-runtime.eu-west-3.amazonaws.com",
  );
  expect(bedrockEndpoint("cn-northwest-1")).toBe(
    "https://bedrock-runtime.cn-northwest-1.amazonaws.com.cn",
  );
  expect(bedrockRegion(bedrock.baseUrl)).toBe("us-east-1");
  expect(
    bedrockRegion(
      "https://vpce-0a1b.bedrock-runtime.eu-central-1.vpce.amazonaws.com",
    ),
  ).toBe("eu-central-1");
});

it("calls Converse with the model in the path, a bearer key and a forced tool", async () => {
  const fetchMock = vi.fn(async () => converse({ ready: true }));
  vi.stubGlobal("fetch", fetchMock);
  // An inference profile ARN has characters that must be escaped in the path.
  const model =
    "arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-sonnet-4-6";
  const provider = new Provider(
    ProviderSchema.parse({ ...bedrock, model, apiKey: key }),
  );
  expect(
    await provider.generate(
      "system prompt",
      { rows: 1 },
      {
        $schema: "https://json-schema.org/draft/2020-12/schema",
        type: "object",
        properties: { ready: { type: "boolean" } },
        required: ["ready"],
      },
    ),
  ).toEqual({ value: { ready: true }, tokens: 42 });
  const [url, init] = fetchMock.mock.calls[0] as any;
  expect(url).toBe(
    `${bedrock.baseUrl}/model/${encodeURIComponent(model)}/converse`,
  );
  expect(url).not.toContain("inference-profile/us.");
  expect(init.headers).toEqual({
    "content-type": "application/json",
    authorization: "Bearer " + key,
  });
  expect(init.redirect).toBe("error");
  const body = JSON.parse(init.body);
  expect(body.system).toEqual([{ text: "system prompt" }]);
  expect(body.messages).toEqual([
    { role: "user", content: [{ text: '{"rows":1}' }] },
  ]);
  expect(body.toolConfig.toolChoice).toEqual({ any: {} });
  expect(body.toolConfig.tools).toHaveLength(1);
  expect(body.toolConfig.tools[0].toolSpec.name).toBe("submit");
  expect(body.toolConfig.tools[0].toolSpec.inputSchema.json).toEqual({
    type: "object",
    properties: { ready: { type: "boolean" } },
    required: ["ready"],
  });
});

it("explains Bedrock failures without repeating the response", async () => {
  const cases: [Response, string][] = [
    [
      new Response(JSON.stringify({ message: "secret row value" }), {
        status: 403,
        headers: { "x-amzn-errortype": "AccessDeniedException:http://x" },
      }),
      "Amazon Bedrock returned HTTP 403 (AccessDeniedException). Check the API key, and that the model is enabled for your account in this region.",
    ],
    [
      new Response(JSON.stringify({ message: "secret row value" }), {
        status: 400,
        headers: { "x-amzn-errortype": "ValidationException" },
      }),
      "Amazon Bedrock returned HTTP 400 (ValidationException). Check the model ID. Some models need an inference profile ID, such as us.anthropic.claude-sonnet-4-6, and the model must support tool use.",
    ],
    [
      new Response("{}", { status: 404 }),
      "Amazon Bedrock returned HTTP 404. Check the model ID and the region.",
    ],
    [
      new Response("{}", {
        status: 429,
        headers: { "x-amzn-errortype": "ThrottlingException" },
      }),
      "Amazon Bedrock returned HTTP 429 (ThrottlingException). Retry later, or ask for a higher quota for this model.",
    ],
    [
      converse({ ready: true }, { stopReason: "max_tokens" }),
      "The Amazon Bedrock model ran out of room for its answer.",
    ],
    [
      Response.json({
        output: { message: { content: [{ text: "Sure! ready" }] } },
        stopReason: "end_turn",
      }),
      "The Amazon Bedrock model did not return a structured response. Choose a model that supports tool use, such as Claude or Amazon Nova.",
    ],
  ];
  for (const [response, message] of cases) {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => response),
    );
    const error = await new Provider(
      ProviderSchema.parse({ ...bedrock, apiKey: key }),
    )
      .generate("", {}, {})
      .catch((e) => e);
    expect(error.message).toBe(message);
    expect(error.message).not.toContain("secret row value");
  }
  await expect(
    new Provider(ProviderSchema.parse(bedrock)).generate("", {}, {}),
  ).rejects.toThrow("Amazon Bedrock API key is required");
});

it("keeps a Bedrock key across regions and models, and never sends it elsewhere", async () => {
  expect(
    canReuseProviderKey(bedrock, {
      ...bedrock,
      baseUrl: "https://bedrock-runtime.eu-west-1.amazonaws.com",
      model: "eu.amazon.nova-pro-v1:0",
    }),
  ).toBe(true);
  expect(canReuseProviderKey(bedrock, { ...bedrock, provider: "claude" })).toBe(
    false,
  );
  expect(
    canReuseProviderKey(
      { ...bedrock, provider: "custom" },
      { ...bedrock, provider: "bedrock" },
    ),
  ).toBe(false);

  const sent: { url: string; headers: Record<string, string> }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string | URL, init?: RequestInit) => {
      sent.push({
        url: String(url),
        headers: { ...(init?.headers as Record<string, string>) },
      });
      return converse({ ready: true, message: "Connected" });
    }),
  );
  const store = new MemoryStore(),
    app = await createApp(store);
  try {
    const scope = (
      await app.inject({ method: "GET", url: "/api/v1/me" })
    ).json().workspaces[0].id;
    const request = (method: any, path: string, payload?: unknown) =>
      app.inject({
        method,
        url: `/api/v1/workspaces/${scope}` + path,
        payload: payload as any,
      });

    // Setup needs a key, then tests it and saves it encrypted.
    const blank = await request("POST", "/onboarding/provider", bedrock);
    expect(blank.statusCode, blank.body).toBe(400);
    expect(blank.json().error).toBe(
      "Enter an API key for the selected provider.",
    );
    expect(sent).toHaveLength(0);
    const queued = await request("POST", "/onboarding/provider", {
      ...bedrock,
      apiKey: key,
    });
    expect(queued.statusCode, queued.body).toBe(200);
    expect(queued.body).not.toContain(key);
    await prepareProvider(
      store,
      scope,
      (await store.get(scope, queued.json().id))!,
    );
    expect(unseal((await store.get(scope, "provider"))!.data.secret)).toBe(key);
    expect(sent.at(-1)).toEqual({
      url: `${bedrock.baseUrl}/model/us.anthropic.claude-sonnet-4-6/converse`,
      headers: {
        "content-type": "application/json",
        authorization: "Bearer " + key,
      },
    });
    const saved = (await request("GET", "/provider")).json();
    expect(saved).toMatchObject({ ...bedrock, hasKey: true });
    expect(JSON.stringify(saved)).not.toContain(key);

    // Another region and model with a blank key keep the saved key.
    const moved = await request("PUT", "/provider", {
      ...bedrock,
      baseUrl: "https://bedrock-runtime.eu-west-1.amazonaws.com",
      model: "eu.amazon.nova-pro-v1:0",
      apiKey: "",
    });
    expect(moved.statusCode, moved.body).toBe(200);
    expect(sent.at(-1)).toEqual({
      url: "https://bedrock-runtime.eu-west-1.amazonaws.com/model/eu.amazon.nova-pro-v1%3A0/converse",
      headers: {
        "content-type": "application/json",
        authorization: "Bearer " + key,
      },
    });

    // A host outside AWS is refused before anything is sent.
    const before = sent.length;
    const outside = await request("PUT", "/provider", {
      ...bedrock,
      baseUrl: "https://bedrock-runtime.us-east-1.amazonaws.com.example.com",
      apiKey: "",
    });
    expect(outside.statusCode, outside.body).toBe(400);
    expect(sent).toHaveLength(before);

    // A new key replaces the old one.
    const replaced = await request("PUT", "/provider", {
      ...bedrock,
      apiKey: newKey,
    });
    expect(replaced.statusCode, replaced.body).toBe(200);
    expect(sent.at(-1)?.headers.authorization).toBe("Bearer " + newKey);
    expect(unseal((await store.get(scope, "provider"))!.data.secret)).toBe(
      newKey,
    );
    for (const r of sent)
      expect(new URL(r.url).hostname).toMatch(
        /^bedrock-runtime\.[a-z0-9-]+\.amazonaws\.com$/,
      );

    // Hands-free voice needs OpenAI's own API.
    expect((await request("GET", "/live")).json()).toEqual({
      available: false,
      reason: "Voice works with the OpenAI provider.",
    });
  } finally {
    await app.close();
  }
});

it("names Amazon Bedrock in the activity feed", () => {
  expect(
    describeActivity({
      id: "j1",
      kind: "job",
      name: "Prepare bedrock",
      updatedAt: new Date(Date.UTC(2026, 0, 1)).toISOString(),
      data: {
        type: "provider_prepare",
        status: "running",
        settings: { provider: "bedrock" },
      },
    }).title,
  ).toBe("Preparing Amazon Bedrock");
});
