import { afterEach, expect, it, vi } from "vitest";
import {
  ProviderSchema,
  OnboardingProviderSchema,
} from "../packages/shared/src/index.js";
import { Provider, canReuseProviderKey } from "../apps/api/src/providers.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore } from "../apps/api/src/store.js";
import { prepareProvider } from "../apps/api/src/setup-jobs.js";
import { unseal } from "../apps/api/src/security.js";

const base = {
  provider: "custom" as const,
  model: "best",
  baseUrl: "https://gateway.example/ai/v1/",
};
const key = "dummy-custom-provider-key";
const schema = {
  type: "object",
  properties: { ready: { type: "boolean" } },
  required: ["ready"],
  additionalProperties: false,
};
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

for (const apiFormat of ["chat-completions", "responses"] as const)
  it(`uses the configured Custom ${apiFormat} endpoint, model and bearer key`, async () => {
    const transport = vi.fn(async () =>
      Response.json(
        apiFormat === "chat-completions"
          ? {
              choices: [
                {
                  message: { content: '{"ready":true}' },
                  finish_reason: "stop",
                },
              ],
              usage: { total_tokens: 5 },
            }
          : {
              status: "completed",
              output: [
                { content: [{ type: "output_text", text: '{"ready":true}' }] },
              ],
              usage: { total_tokens: 5 },
            },
      ),
    );
    vi.stubGlobal("fetch", transport);
    const result = await new Provider(
      ProviderSchema.parse({ ...base, apiFormat, apiKey: key }),
    ).generate("system", { safe: "profile" }, schema);
    expect(result).toEqual({ value: { ready: true }, tokens: 5 });
    expect(transport).toHaveBeenCalledTimes(1);
    const [url, request] = transport.mock.calls[0] as unknown as [
      string,
      RequestInit,
    ];
    expect(url).toBe(
      base.baseUrl +
        (apiFormat === "chat-completions" ? "chat/completions" : "responses"),
    );
    expect(request.headers).toMatchObject({ authorization: "Bearer " + key });
    expect(request.redirect).toBe("error");
    const body = JSON.parse(request.body as string);
    expect(body.model).toBe("best");
    if (apiFormat === "chat-completions") {
      expect(body.response_format.json_schema).toMatchObject({
        strict: true,
        schema,
      });
      expect(body.messages).toEqual([
        { role: "system", content: "system" },
        { role: "user", content: '{"safe":"profile"}' },
      ]);
      expect(body.max_tokens).toBe(8192);
    } else {
      expect(body.text.format).toMatchObject({ strict: true, schema });
      expect(body.store).toBe(false);
    }
  });

it("supports unauthenticated Custom endpoints without sending an authorization header", async () => {
  const transport = vi.fn(async () =>
    Response.json({ choices: [{ message: { content: '{"ready":true}' } }] }),
  );
  vi.stubGlobal("fetch", transport);
  await new Provider(ProviderSchema.parse(base)).generate("", {}, schema);
  expect(
    (transport.mock.calls[0] as unknown as [string, RequestInit])[1].headers,
  ).not.toHaveProperty("authorization");
});

it("validates the Custom base URL in both provider APIs", () => {
  for (const contract of [ProviderSchema, OnboardingProviderSchema]) {
    for (const baseUrl of [
      undefined,
      "not-a-url",
      "ftp://gateway.example/v1",
      "https://user:password@gateway.example/v1",
      "https://gateway.example/v1?token=secret",
      "https://gateway.example/v1#secret",
    ])
      expect(contract.safeParse({ ...base, baseUrl }).success).toBe(false);
    expect(
      contract.safeParse({ ...base, baseUrl: "http://localhost:11434/v1" })
        .success,
    ).toBe(true);
    expect(contract.safeParse({ ...base, model: "  " }).success).toBe(false);
  }
});

for (const data of [
  { choices: [{ message: { content: "not JSON" } }] },
  {
    choices: [
      { message: { content: '{"ready":true}' }, finish_reason: "length" },
    ],
  },
  { choices: [{ message: { refusal: "refused" } }] },
])
  it("rejects invalid, truncated or refused Custom output", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => Response.json(data)),
    );
    await expect(
      new Provider(ProviderSchema.parse(base)).generate("", {}, schema),
    ).rejects.toThrow(/Custom model/);
  });

it("rejects an incomplete Custom Responses answer", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      Response.json({
        status: "incomplete",
        output: [
          { content: [{ type: "output_text", text: '{"ready":true}' }] },
        ],
      }),
    ),
  );
  await expect(
    new Provider(
      ProviderSchema.parse({ ...base, apiFormat: "responses" }),
    ).generate("", {}, schema),
  ).rejects.toThrow("did not complete");
});

it("does not retry a failed Custom endpoint against a different provider", async () => {
  const transport = vi.fn(
    async () => new Response("unavailable", { status: 503 }),
  );
  vi.stubGlobal("fetch", transport);
  await expect(
    new Provider(ProviderSchema.parse(base)).generate("", {}, schema),
  ).rejects.toThrow("The custom AI provider returned HTTP 503.");
  expect(transport).toHaveBeenCalledTimes(1);
});

it("names the Custom provider when its reply is not JSON", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response('{"private-record":')),
  );
  await expect(
    new Provider(ProviderSchema.parse(base)).generate("", {}, schema),
  ).rejects.toThrow("The custom AI provider returned invalid structured JSON.");
});

it("reuses keys only for the same Custom endpoint", () => {
  expect(
    canReuseProviderKey(base, {
      ...base,
      baseUrl: base.baseUrl.replace(/\/$/, ""),
    }),
  ).toBe(true);
  expect(
    canReuseProviderKey(base, { ...base, baseUrl: "https://other.example/v1" }),
  ).toBe(false);
  expect(canReuseProviderKey(base, { ...base, provider: "openai" })).toBe(
    false,
  );
});

it("tests and encrypts Custom settings, keeps saved keys on the same URL and drops them on endpoint changes", async () => {
  const store = new MemoryStore(),
    app = await createApp(store);
  const testedKeys: (string | undefined)[] = [];
  const generate = vi
    .spyOn(Provider.prototype, "generate")
    .mockImplementation(async function (this: Provider) {
      testedKeys.push(this.settings.apiKey);
      return { value: { ready: true, message: "Connected" }, tokens: 5 };
    });
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
    const queued = await request("POST", "/onboarding/provider", {
      ...base,
      apiKey: key,
    });
    expect(queued.statusCode, queued.body).toBe(200);
    expect(queued.body).not.toContain(key);
    const job = (await store.get(scope, queued.json().id))!;
    await prepareProvider(store, scope, job);
    const saved = (await store.get(scope, "provider"))!;
    expect(saved.data.settings.provider).toBe("custom");
    expect(JSON.stringify(saved.data)).not.toContain(key);
    expect(unseal(saved.data.secret)).toBe(key);
    const publicSettings = await request("GET", "/provider");
    expect(publicSettings.json()).toMatchObject({
      provider: "custom",
      model: "best",
      baseUrl: base.baseUrl,
      hasKey: true,
    });
    expect(publicSettings.body).not.toContain(key);
    const reused = await request("PUT", "/provider", { ...base, apiKey: "" });
    expect(reused.statusCode, reused.body).toBe(200);
    expect(testedKeys.at(-1)).toBe(key);
    const changed = { ...base, baseUrl: "https://other.example/v1" };
    const queuedChanged = await request(
      "POST",
      "/onboarding/provider",
      changed,
    );
    const changedJob = (await store.get(scope, queuedChanged.json().id))!;
    expect(unseal<any>(changedJob.data.secret).apiKey).toBeUndefined();
    const updated = await request("PUT", "/provider", changed);
    expect(updated.statusCode, updated.body).toBe(200);
    expect(testedKeys.at(-1)).toBeUndefined();
    expect((await store.get(scope, "provider"))?.data.secret).toBeNull();
    generate.mockRejectedValueOnce(new Error("Endpoint unavailable"));
    expect((await request("PUT", "/provider", base)).statusCode).toBe(500);
    expect((await store.get(scope, "provider"))?.data.settings.baseUrl).toBe(
      changed.baseUrl,
    );
  } finally {
    await app.close();
  }
});
