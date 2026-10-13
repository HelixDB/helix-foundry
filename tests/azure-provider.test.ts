import { afterEach, expect, it, vi } from "vitest";
import {
  ProviderSchema,
  OnboardingProviderSchema,
  describeActivity,
} from "../packages/shared/src/index.js";
import { canReuseProviderKey } from "../apps/api/src/providers.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore } from "../apps/api/src/store.js";
import { prepareProvider } from "../apps/api/src/setup-jobs.js";
import { unseal } from "../apps/api/src/security.js";

const azure = {
  provider: "azure" as const,
  model: "gpt-6.1-sol",
  baseUrl: "https://first.openai.azure.com",
};
const key = "azure-key-first",
  newKey = "azure-key-second";
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("accepts only an https:// Azure OpenAI endpoint in both provider APIs", () => {
  for (const contract of [ProviderSchema, OnboardingProviderSchema]) {
    for (const baseUrl of [
      undefined,
      "not-a-url",
      "http://first.openai.azure.com",
      "ftp://first.openai.azure.com",
      "https://user:password@first.openai.azure.com",
      "https://first.openai.azure.com/openai/v1?api-key=secret",
      "https://first.openai.azure.com#secret",
    ])
      expect(contract.safeParse({ ...azure, baseUrl }).success).toBe(false);
    for (const baseUrl of [azure.baseUrl, azure.baseUrl + "/openai/v1/"])
      expect(contract.safeParse({ ...azure, baseUrl }).success).toBe(true);
  }
});

it("reuses an Azure key only for the same endpoint and deployment", () => {
  expect(
    canReuseProviderKey(azure, {
      ...azure,
      baseUrl: azure.baseUrl + "/openai/v1/",
    }),
  ).toBe(true);
  expect(
    canReuseProviderKey(azure, {
      ...azure,
      baseUrl: "https://second.openai.azure.com",
    }),
  ).toBe(false);
  expect(
    canReuseProviderKey(azure, { ...azure, model: "other-deployment" }),
  ).toBe(false);
  expect(
    canReuseProviderKey(
      { ...azure, provider: "custom", baseUrl: azure.baseUrl + "/openai/v1" },
      { ...azure, baseUrl: azure.baseUrl + "/openai/v1" },
    ),
  ).toBe(false);
  expect(canReuseProviderKey(azure, { ...azure, provider: "openai" })).toBe(
    false,
  );
});

it("never sends a saved Azure key to a changed endpoint or deployment", async () => {
  const sent: { url: string; headers: Record<string, string> }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string | URL, init?: RequestInit) => {
      sent.push({
        url: String(url),
        headers: { ...(init?.headers as Record<string, string>) },
      });
      return Response.json({
        output: [
          {
            content: [
              {
                type: "output_text",
                text: '{"ready":true,"message":"Connected"}',
              },
            ],
          },
        ],
        usage: { total_tokens: 5 },
      });
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

    // Setup tests the key on the first endpoint and saves it encrypted.
    const queued = await request("POST", "/onboarding/provider", {
      ...azure,
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
      url: "https://first.openai.azure.com/openai/v1/responses",
      headers: { "content-type": "application/json", "api-key": key },
    });
    expect(
      (await request("GET", "/provider")).json().hasKey,
      "the saved key is reported",
    ).toBe(true);

    // A blank key on the same endpoint, spelled with its /openai/v1 route,
    // keeps the saved key.
    const same = await request("PUT", "/provider", {
      ...azure,
      baseUrl: azure.baseUrl + "/openai/v1",
      apiKey: "",
    });
    expect(same.statusCode, same.body).toBe(200);
    expect(sent.at(-1)?.headers["api-key"]).toBe(key);

    // A blank key with a changed endpoint or deployment is refused, in setup
    // and in Settings, before anything is sent.
    const before = sent.length;
    for (const changed of [
      { ...azure, baseUrl: "https://second.openai.azure.com" },
      { ...azure, model: "other-deployment" },
    ]) {
      const setup = await request("POST", "/onboarding/provider", changed);
      expect(setup.statusCode, setup.body).toBe(400);
      expect(setup.json().error).toBe(
        "Enter an API key for the selected provider.",
      );
      const settings = await request("PUT", "/provider", {
        ...changed,
        apiKey: "",
      });
      expect(settings.statusCode, settings.body).toBe(400);
      expect(settings.json().error).toBe("Azure OpenAI API key is required");
    }
    expect(sent).toHaveLength(before);
    expect((await store.get(scope, "provider"))!.data.settings).toMatchObject({
      baseUrl: azure.baseUrl + "/openai/v1",
      model: azure.model,
    });

    // A new key for the new endpoint goes there, and only there.
    const moved = await request("PUT", "/provider", {
      ...azure,
      baseUrl: "https://second.openai.azure.com",
      apiKey: newKey,
    });
    expect(moved.statusCode, moved.body).toBe(200);
    expect(sent.at(-1)).toEqual({
      url: "https://second.openai.azure.com/openai/v1/responses",
      headers: { "content-type": "application/json", "api-key": newKey },
    });
    for (const r of sent) {
      expect(r.headers).not.toHaveProperty("authorization");
      if (!r.url.startsWith("https://first.openai.azure.com/"))
        expect(JSON.stringify(r)).not.toContain(key);
    }
  } finally {
    await app.close();
  }
});

it("names Azure OpenAI in the activity feed", () => {
  expect(
    describeActivity({
      id: "j1",
      kind: "job",
      name: "Prepare azure",
      updatedAt: new Date(Date.UTC(2026, 0, 1)).toISOString(),
      data: {
        type: "provider_prepare",
        status: "running",
        settings: { provider: "azure" },
      },
    }).title,
  ).toBe("Preparing Azure OpenAI");
});
