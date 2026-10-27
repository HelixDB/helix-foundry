import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type * as wfe from "@/shared/wfe";
import type { MockReply } from "../../mock/http";

// The mock WFE's Settings › AI models (mock): owners
// change it, a save is tested before it is used and a failed test keeps
// the setting, a newer save supersedes an older one, a key is kept only
// while its config is the setting or pending and never answered, Foundry
// Pro gates keys hosted, and model work is refused without a provider in a
// local API. Each case loads the mock afresh, hosted or as a local API.

const WS = "/v1/workspaces/ws_acme";
const AI_PATH = `${WS}/foundry/ai`;
const KEY = "sk-ant-api03-sentinel-0123456789abcd";
const CLAUDE = { provider: { anthropic: {} }, apiKey: KEY };
const OPENAI_KEY = "sk-proj-sentinel-0123456789abcdefwxyz";
const AZURE_KEY = "0123456789abcdef0123456789abcdef";
const BEDROCK_KEY = "ABSKsentinelQmVkcm9ja0FQSUtleS0wMTIz";
const bedrock = (region: string, models?: object, fips?: boolean) => ({
  provider: { bedrock: { region, ...(fips !== undefined && { fips }) } },
  ...(models && { models }),
  apiKey: BEDROCK_KEY,
});
// The Bedrock rows' Claude, which inference profiles are priced as.
const SONNET = "anthropic.claude-sonnet-5-5";
const BEDROCK_SUGGESTED = {
  draft: { name: "global.anthropic.claude-opus-5-5", table: {} },
  ask: { name: "global.anthropic.claude-sonnet-5-5", table: {} },
  extract: { name: "global.anthropic.claude-haiku-5-5", table: {} },
  verify: { name: "global.anthropic.claude-sonnet-5-5", table: {} },
};
const azure = (endpoint: string, models: object, apiKey?: string) => ({
  provider: { azureOpenai: { endpoint } },
  models,
  ...(apiKey && { apiKey }),
});
const each = (m: object) =>
  Object.fromEntries(["draft", "ask", "extract", "verify"].map((r) => [r, m]));
const all = (name: string) =>
  Object.fromEntries(
    ["draft", "ask", "extract", "verify"].map((r) => [r, { name, table: {} }]),
  );
const CHAT = "FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS";
/** A custom server at `baseUrl`, speaking Chat Completions unless `api` says. */
const server = (baseUrl: string, more: object = {}) => ({
  openaiCompatible: { baseUrl, api: CHAT, ...more },
});

/** The mock's routes and state, loaded afresh: hosted, or a local API. */
async function load(local = false) {
  vi.resetModules();
  vi.stubEnv("MOCK_LOCAL_TOKEN", local ? "local-token" : "");
  vi.stubEnv("MOCK_AI_DEFAULT", local ? "anthropic" : "");
  const { routes } = await import("../../mock/routes");
  const state = await import("../../mock/state");
  const billing = await import("../../mock/routes/billing");
  const ai = await import("../../mock/routes/ai");
  state.resetState();
  const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));
  const call = async (
    method: HttpMethod,
    path: string,
    body?: unknown,
    userId = "usr_ada",
  ): Promise<MockReply & { body: any }> => {
    const url = new URL(path, "http://mock");
    for (const r of compiled) {
      const params = r.method === method && matchTemplate(r.c, url.pathname);
      if (!params) continue;
      return (await r.handle({
        method,
        path: url.pathname,
        params,
        query: url.searchParams,
        body,
        principal: { user: state.db.users.get(userId)!, sessionId: "s" },
      })) as MockReply & { body: any };
    }
    throw new Error(`no mock route for ${method} ${path}`);
  };
  const read = async () =>
    (await call("GET", AI_PATH)).body.ai as wfe.FoundryAi;
  const config = (knobs: Record<string, unknown>) =>
    Object.assign(state.config, knobs);
  return { call, read, config, state, billing, ai };
}

beforeEach(() => {
  vi.useFakeTimers({ now: Date.parse("2026-10-23T10:00:00Z") });
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllEnvs();
});

/** Past a save's test (aiProbeMs, 1500 by default). */
const tested = () => vi.advanceTimersByTime(1600);

describe("hosted", () => {
  it("starts on Helix's Claude, offering Claude with a key", async () => {
    const { read } = await load();
    const ai = await read();
    expect(ai.active).toBeUndefined();
    expect(ai.default).toEqual({
      kind: "FOUNDRY_AI_KIND_ANTHROPIC",
      label: "Claude",
      models: {
        draft: { name: "claude-opus-5-5", table: {} },
        ask: { name: "claude-sonnet-5-5", table: {} },
        extract: { name: "claude-haiku-5-5", table: {} },
        verify: { name: "claude-sonnet-5-5", table: {} },
      },
    });
    // Layer 5's kinds: Claude, OpenAI and Bedrock with suggested models,
    // Azure OpenAI and custom servers with none (their owner names each
    // deployment or model), a custom server's key optional.
    expect(ai.offered).toEqual([
      expect.objectContaining({
        kind: "FOUNDRY_AI_KIND_ANTHROPIC",
        key: "FOUNDRY_AI_KEY_REQUIRED",
      }),
      {
        kind: "FOUNDRY_AI_KIND_OPENAI",
        key: "FOUNDRY_AI_KEY_REQUIRED",
        suggested: {
          ...all("gpt-6.1-sol"),
          extract: { name: "gpt-6-luna", table: {} },
        },
      },
      { kind: "FOUNDRY_AI_KIND_AZURE_OPENAI", key: "FOUNDRY_AI_KEY_REQUIRED" },
      {
        kind: "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE",
        key: "FOUNDRY_AI_KEY_OPTIONAL",
      },
      {
        kind: "FOUNDRY_AI_KIND_BEDROCK",
        key: "FOUNDRY_AI_KEY_REQUIRED",
        suggested: BEDROCK_SUGGESTED,
      },
    ]);
    expect(ai.privateHostsAllowed).toBe(false);
    // Acme is on Pro.
    expect(ai.proRequired).toBe(false);
  });

  it("lets only owners change it; everyone reads it", async () => {
    const { call } = await load();
    expect((await call("GET", AI_PATH, undefined, "usr_alan")).status).toBe(
      200,
    );
    const put = await call("PUT", AI_PATH, CLAUDE, "usr_alan");
    expect(put.status).toBe(403);
    expect(
      (await call("PUT", AI_PATH, { useDefault: {} }, "usr_alan")).status,
    ).toBe(403);
  });

  it("tests a save before it is used, then runs on it", async () => {
    const { call, read, ai } = await load();
    const put = await call("PUT", AI_PATH, {
      ...CLAUDE,
      models: all("claude-sonnet-5-5"),
    });
    expect(put.status).toBe(200);
    const saved = put.body.ai as wfe.FoundryAi;
    expect(saved.active).toBeUndefined();
    expect(saved.pending).toMatchObject({
      kind: "FOUNDRY_AI_KIND_ANTHROPIC",
      probeState: "FOUNDRY_AI_PROBE_STATE_QUEUED",
      hasKey: true,
      keyHint: "sk-ant…abcd",
      label: "Claude",
      createdByName: "Ada Lovelace",
    });
    vi.advanceTimersByTime(800);
    expect((await read()).pending?.probeState).toBe(
      "FOUNDRY_AI_PROBE_STATE_RUNNING",
    );
    // Until it passes, the default runs.
    expect(ai.aiSource("ws_acme")).toBe("default");
    expect(ai.aiModel("ws_acme", "draft")).toBe("claude-opus-5-5");
    tested();
    const now = await read();
    expect(now.pending).toBeUndefined();
    expect(now.active).toMatchObject({
      id: saved.pending!.id,
      probeState: "FOUNDRY_AI_PROBE_STATE_PASSED",
      hasKey: true,
    });
    expect(ai.aiSource("ws_acme")).toBe("workspace");
    expect(ai.aiModel("ws_acme", "draft")).toBe("claude-sonnet-5-5");
    expect(ai.aiLabel("ws_acme")).toBe("Claude");
    expect(ai.aiProvider("ws_acme")).toBe("anthropic");
  });

  it("keeps the setting when a save's test fails, and says why", async () => {
    const { call, read } = await load();
    await call("PUT", AI_PATH, { ...CLAUDE, apiKey: "sk-ant-bad-0123456789" });
    tested();
    const ai = await read();
    expect(ai.active).toBeUndefined();
    expect(ai.lastFailed).toMatchObject({
      probeState: "FOUNDRY_AI_PROBE_STATE_FAILED",
      probeFailure: "auth",
      probeDetail: "none",
      // A failed config holds no key.
      hasKey: false,
    });
    // The failure stays until the setting changes or a save follows.
    await call("PUT", AI_PATH, CLAUDE);
    expect((await read()).lastFailed).toBeUndefined();
    tested();
    expect((await read()).active).toBeDefined();
  });

  it("fails a save the test runs out of time on", async () => {
    const { call, read, config } = await load();
    config({ aiProbeOutcome: "timeout", aiProbeMs: 200 });
    await call("PUT", AI_PATH, CLAUDE);
    vi.advanceTimersByTime(250);
    const ai = await read();
    expect(ai.active).toBeUndefined();
    expect(ai.lastFailed?.probeFailure).toBe("timeout");
  });

  it("fails a model that can't call tools or isn't there", async () => {
    const { call, read, config } = await load();
    // The mock's price table must know them for the save to be tested.
    config({ aiKinds: ["FOUNDRY_AI_KIND_OPENAI_COMPATIBLE"] });
    const compat = (name: string) => ({
      provider: server("https://llm.example.com/v1"),
      models: Object.fromEntries(
        ["draft", "ask", "extract", "verify"].map((r) => [
          r,
          { name, free: {} },
        ]),
      ),
    });
    await call("PUT", AI_PATH, compat("qwen-notools"));
    tested();
    expect((await read()).lastFailed).toMatchObject({
      probeFailure: "unsupported",
      probeDetail: "tools_rejected",
      label: "llm.example.com",
    });
    await call("PUT", AI_PATH, compat("qwen-missing"));
    tested();
    expect((await read()).lastFailed?.probeFailure).toBe("not_found");
  });

  it("supersedes an older save that is still being tested", async () => {
    const { call, read } = await load();
    const first = (await call("PUT", AI_PATH, CLAUDE)).body.ai.pending.id;
    vi.advanceTimersByTime(500);
    const second = (
      await call("PUT", AI_PATH, { ...CLAUDE, models: all("claude-opus-5") })
    ).body.ai.pending.id;
    expect(second).not.toBe(first);
    tested();
    const ai = await read();
    expect(ai.active?.id).toBe(second);
    expect(ai.active?.models?.draft?.name).toBe("claude-opus-5");
  });

  it("drops a test that passes after the owner went back to the default", async () => {
    const { call, read, ai } = await load();
    await call("PUT", AI_PATH, CLAUDE);
    vi.advanceTimersByTime(500);
    const back = await call("PUT", AI_PATH, { useDefault: {} });
    expect(back.status).toBe(200);
    expect(back.body.ai.pending).toBeUndefined();
    tested();
    const now = await read();
    expect(now.active).toBeUndefined();
    expect(now.lastFailed).toBeUndefined();
    expect(ai.aiSource("ws_acme")).toBe("default");
  });

  it("keeps the setting's key for a save without one, while it belongs", async () => {
    const { call, read } = await load();
    await call("PUT", AI_PATH, CLAUDE);
    tested();
    const first = (await read()).active!;
    // Same kind, other models, no key: the setting's key is kept.
    const kept = await call("PUT", AI_PATH, {
      provider: { anthropic: {} },
      models: all("claude-haiku-5-5"),
    });
    expect(kept.status).toBe(200);
    expect(kept.body.ai.pending).toMatchObject({
      hasKey: true,
      keyHint: first.keyHint,
    });
    tested();
    const now = await read();
    expect(now.active?.id).toBe(kept.body.ai.pending.id);
    // The new setting holds the key now.
    expect(now.active?.hasKey).toBe(true);

    // Back to the default: no config keeps a key, so a keyless save is refused.
    await call("PUT", AI_PATH, { useDefault: {} });
    const refused = await call("PUT", AI_PATH, { provider: { anthropic: {} } });
    expect(refused.status).toBe(400);
    expect(refused.body.message).toBe("Enter the API key for this endpoint.");
  });

  it("never answers a key", async () => {
    const { call, read } = await load();
    const put = await call("PUT", AI_PATH, CLAUDE);
    expect(JSON.stringify(put.body)).not.toContain("sentinel");
    tested();
    expect(JSON.stringify(await read())).not.toContain("sentinel");
    await call("PUT", AI_PATH, { ...CLAUDE, apiKey: "sk-ant-bad-sentinel-2" });
    tested();
    expect(JSON.stringify(await read())).not.toContain("sentinel");
  });

  it("hints at a key without showing most of it, and only to owners", async () => {
    const { call, read, ai } = await load();
    for (const [key, hint] of [
      [KEY, "sk-ant…abcd"],
      // A self-hosted server's short key: kind and end would be all of it.
      ["sk-local-key1", "…"],
      ["abcd-efgh-ijkl", "…"],
      ["sk-ant-0123456789ab", "…"],
      // Sixteen hidden with the end shown, but not with the kind too.
      ["sk-ant-0123456789abcdef", "…cdef"],
      ["0123456789abcdef0123", "…0123"],
      ["sk-ant-0123456789abcdefghij", "sk-ant…ghij"],
    ] as const)
      expect(ai.keyHint(key), key).toBe(hint);
    await call("PUT", AI_PATH, CLAUDE);
    tested();
    expect((await read()).active?.keyHint).toBe("sk-ant…abcd");
    // A member reads where AI runs, and nothing of the key.
    const member = (await call("GET", AI_PATH, undefined, "usr_alan")).body
      .ai as wfe.FoundryAi;
    expect(member.active).toMatchObject({ hasKey: true, label: "Claude" });
    expect(member.active).not.toHaveProperty("keyHint");
  });

  it("runs the WFE's checks on a save, in order", async () => {
    const { call, config, billing } = await load();
    const refuse = async (body: unknown, status: number, message: string) => {
      const r = await call("PUT", AI_PATH, body);
      expect(r.status, JSON.stringify(body)).toBe(status);
      expect(r.body.message).toBe(message);
      return r;
    };
    await refuse({}, 400, "choose use_default or a provider");
    await refuse({ provider: {} }, 400, "provider is required");
    await refuse(
      { provider: { anthropic: {}, openai: {} } },
      400,
      "provider is required",
    );
    await refuse(
      { provider: { ollama: { baseUrl: "https://ollama.example.com" } } },
      400,
      "Ollama is not offered here",
    );
    await refuse(
      { useDefault: {}, apiKey: KEY },
      400,
      "use_default takes no models or key",
    );
    await refuse(
      { ...CLAUDE, apiKey: "sk-ant-é" },
      400,
      "the API key must be at most 512 printable characters",
    );
    await refuse(
      { ...CLAUDE, apiKey: "x".repeat(513) },
      400,
      "the API key must be at most 512 printable characters",
    );
    await refuse(
      { ...CLAUDE, models: { draft: { name: "two words" } } },
      400,
      "models.draft.name must be 1 to 200 characters with no spaces",
    );
    const unpriced = await refuse(
      { ...CLAUDE, models: { ask: { name: "claude-unknown" } } },
      400,
      "Helix has no price for the ask model claude-unknown; enter its prices",
    );
    expect(unpriced.body.details[0].reason).toBe("FOUNDRY_AI_PRICE_REQUIRED");

    // Hosted, a custom server must be https on a public host.
    config({
      aiKinds: ["FOUNDRY_AI_KIND_OPENAI_COMPATIBLE", "FOUNDRY_AI_KIND_OLLAMA"],
    });
    await refuse(
      { provider: server("http://llm.example.com/v1") },
      400,
      "models.draft is required",
    );
    const free = all("m");
    for (const r of Object.values(free)) {
      delete (r as { table?: unknown }).table;
      (r as { free?: unknown }).free = {};
    }
    await refuse(
      { provider: server("http://llm.example.com/v1"), models: free },
      400,
      "provider.base_url must be https",
    );
    await refuse(
      { provider: server("https://10.0.0.1/v1"), models: free },
      400,
      "the endpoint resolves to a private address",
    );
    await refuse(
      {
        provider: { ollama: { baseUrl: "https://ollama.example.com" } },
        models: free,
        apiKey: KEY,
      },
      400,
      "Ollama takes no API key",
    );
    // A custom server's key is optional.
    expect(
      (
        await call("PUT", AI_PATH, {
          provider: server("https://llm.example.com/v1"),
          models: free,
        })
      ).status,
    ).toBe(200);

    // Off Pro and Max, a key is refused; going back to the default isn't.
    billing.setFoundryPlan("ws_acme", "FOUNDRY_TIER_FREE");
    config({ aiKinds: undefined });
    const free2 = await refuse(
      CLAUDE,
      400,
      "Bringing your own model key needs Foundry Pro",
    );
    expect(free2.body.details[0].reason).toBe("FOUNDRY_PRO_REQUIRED");
    expect((await call("GET", AI_PATH)).body.ai.proRequired).toBe(true);
    expect((await call("PUT", AI_PATH, { useDefault: {} })).status).toBe(200);
    billing.setFoundryPlan("ws_acme", "FOUNDRY_TIER_MAX");
    expect((await call("PUT", AI_PATH, CLAUDE)).status).toBe(200);
  });

  it("refuses work on a key once the workspace leaves Pro, until it is back or on Helix's Claude", async () => {
    const { call, read, config, ai } = await load();
    await call("PUT", AI_PATH, CLAUDE);
    tested();
    expect(ai.aiRefusal("ws_acme")).toBeUndefined();
    // Cancelled, Pro stays on until the period ends.
    const cancel = { workspaceId: "ws_acme" };
    expect((await call("POST", "/v1/foundry/plan:cancel", cancel)).status).toBe(
      200,
    );
    expect(ai.aiRefusal("ws_acme")).toBeUndefined();
    config({ foundryPeriodEnded: true });
    expect(
      (await call("GET", "/v1/foundry/plan?workspaceId=ws_acme")).body.plan,
    ).toEqual({ tier: "FOUNDRY_TIER_FREE" });
    // The key stays the setting, and its work is refused, never moved to
    // Helix's Claude.
    const now = await read();
    expect(now.active?.hasKey).toBe(true);
    expect(now.proRequired).toBe(true);
    expect(ai.aiSource("ws_acme")).toBe("workspace");
    const refused = (r: MockReply & { body: any }) => {
      expect(r.status).toBe(400);
      expect(r.body.message).toBe(
        "Bringing your own model key needs Foundry Pro",
      );
      expect(r.body.details[0]).toMatchObject({
        reason: "FOUNDRY_PRO_REQUIRED",
        domain: "billing.helix-db.com",
      });
    };
    refused(
      await call(
        "POST",
        `${WS}/foundry/ontologies/ont_acme_contracts:generate`,
        {},
      ),
    );
    refused(
      await call("POST", `${WS}/foundry/ontologies/ont_acme_sales/questions`, {
        clientToken: "t",
        question: "Who?",
      }),
    );
    // Helix's Claude is on every plan.
    expect((await call("PUT", AI_PATH, { useDefault: {} })).status).toBe(200);
    expect(ai.aiRefusal("ws_acme")).toBeUndefined();
  });

  it("caps saves at ten an hour", async () => {
    const { call } = await load();
    for (let i = 0; i < 10; i++)
      expect((await call("PUT", AI_PATH, CLAUDE)).status).toBe(200);
    const over = await call("PUT", AI_PATH, CLAUDE);
    expect(over.status).toBe(429);
    expect(over.body.message).toBe(
      "a workspace may save its AI settings 10 times an hour; try again later",
    );
    vi.advanceTimersByTime(3_600_001);
    expect((await call("PUT", AI_PATH, CLAUDE)).status).toBe(200);
  });

  it("words a draft's failures for the provider in force", async () => {
    const { call, ai } = await load();
    expect(ai.draftFailure("ws_acme", "refused")).toBe(
      "Claude declined to draft this ontology",
    );
    expect(ai.draftFailure("ws_acme", "auth")).toBe(
      "Claude is unavailable; Helix will retry",
    );
    await call("PUT", AI_PATH, CLAUDE);
    tested();
    expect(ai.draftFailure("ws_acme", "auth")).toBe(
      "Claude refused the API key. An owner can enter a new one in Settings › AI models.",
    );
    expect(ai.aiRefusal("ws_acme")).toBeUndefined();
  });
});

describe("OpenAI and Azure OpenAI", () => {
  it("runs on OpenAI's suggested models once the key passes its test", async () => {
    const { call, read, ai } = await load();
    const put = await call("PUT", AI_PATH, {
      provider: { openai: {} },
      apiKey: OPENAI_KEY,
    });
    expect(put.status).toBe(200);
    expect(put.body.ai.pending).toMatchObject({
      kind: "FOUNDRY_AI_KIND_OPENAI",
      models: {
        ...all("gpt-6.1-sol"),
        extract: { name: "gpt-6-luna", table: {} },
      },
      keyHint: "sk-proj…wxyz",
      label: "OpenAI",
    });
    tested();
    expect((await read()).active?.kind).toBe("FOUNDRY_AI_KIND_OPENAI");
    expect(ai.aiLabel("ws_acme")).toBe("OpenAI");
    expect(ai.aiProvider("ws_acme")).toBe("openai");
    expect(ai.aiModel("ws_acme", "extract")).toBe("gpt-6-luna");
    expect(ai.draftFailure("ws_acme", "auth")).toBe(
      "OpenAI refused the API key. An owner can enter a new one in Settings › AI models.",
    );
    // An OpenAI key belongs to the account: other models keep it.
    const kept = await call("PUT", AI_PATH, {
      provider: { openai: {} },
      models: each({
        name: "gpt-7-preview",
        rates: { inputUsdPerMtok: "1.250", outputUsdPerMtok: "10" },
      }),
    });
    expect(kept.status).toBe(200);
    // Rates are answered as the WFE writes them back.
    expect(kept.body.ai.pending.models.draft).toEqual({
      name: "gpt-7-preview",
      rates: { inputUsdPerMtok: "1.25", outputUsdPerMtok: "10" },
    });
    expect(kept.body.ai.pending.keyHint).toBe("sk-proj…wxyz");
    // But not a Claude save.
    const claude = await call("PUT", AI_PATH, { provider: { anthropic: {} } });
    expect(claude.body.message).toBe("Enter the API key for this endpoint.");
  });

  it("fails OpenAI's test on a bad key or a model it lacks", async () => {
    const { call, read } = await load();
    await call("PUT", AI_PATH, {
      provider: { openai: {} },
      apiKey: "sk-proj-bad-0123456789abcdef",
    });
    tested();
    expect((await read()).lastFailed).toMatchObject({
      probeFailure: "auth",
      label: "OpenAI",
    });
    await call("PUT", AI_PATH, {
      provider: { openai: {} },
      apiKey: OPENAI_KEY,
      models: each({
        name: "gpt-missing",
        rates: { inputUsdPerMtok: "1", outputUsdPerMtok: "2" },
      }),
    });
    tested();
    expect((await read()).lastFailed?.probeFailure).toBe("not_found");
  });

  it("checks a model's name and prices as the WFE does", async () => {
    const { call } = await load();
    const refuse = async (models: object, message: string) => {
      const r = await call("PUT", AI_PATH, {
        provider: { openai: {} },
        apiKey: OPENAI_KEY,
        models,
      });
      expect(r.body.message, JSON.stringify(models)).toBe(message);
      return r;
    };
    await refuse(
      { draft: { name: "sk-proj-0123", table: {} } },
      "models.draft.name looks like an API key; enter the key in the API key field",
    );
    for (const [rates, message] of [
      [
        { inputUsdPerMtok: "1.2345", outputUsdPerMtok: "2" },
        "input_usd_per_mtok must be a price in dollars, with at most three decimals",
      ],
      [
        { inputUsdPerMtok: "1", outputUsdPerMtok: "-2" },
        "output_usd_per_mtok must be a price in dollars, with at most three decimals",
      ],
      [
        {
          inputUsdPerMtok: "1",
          outputUsdPerMtok: "2",
          cachedInputUsdPerMtok: "x",
        },
        "cached_input_usd_per_mtok must be a price in dollars, with at most three decimals",
      ],
      [
        { inputUsdPerMtok: "1", outputUsdPerMtok: "0.000" },
        "output must be more than 0",
      ],
    ] as const)
      await refuse(
        { ask: { name: "gpt-x", rates } },
        `models.ask.price.rates.${message}`,
      );
    await refuse(
      { verify: { name: "gpt-x", free: {} } },
      "models.verify.price can be free only for Ollama and OpenAI-compatible servers",
    );
    const unpriced = await refuse(
      { extract: { name: "gpt-unknown", table: {} } },
      "Helix has no price for the extract model gpt-unknown; enter its prices",
    );
    expect(unpriced.body.details[0].reason).toBe("FOUNDRY_AI_PRICE_REQUIRED");
  });

  it("runs on an Azure resource's deployments, priced as the model each runs", async () => {
    const { call, read, ai } = await load();
    const models = each({ name: "prod-gpt", tableAs: "gpt-6.1-sol" });
    // Azure suggests nothing: every deployment is named.
    expect(
      (
        await call("PUT", AI_PATH, {
          provider: {
            azureOpenai: { endpoint: "https://acme.openai.azure.com" },
          },
          apiKey: AZURE_KEY,
        })
      ).body.message,
    ).toBe("models.draft is required");
    for (const endpoint of [
      "http://acme.openai.azure.com",
      "https://acme.example.com",
      "https://acme.openai.azure.com/openai/deployments",
      "https://openai.azure.com",
    ])
      expect(
        (await call("PUT", AI_PATH, azure(endpoint, models, AZURE_KEY))).body
          .message,
        endpoint,
      ).toBe(
        "Use the resource’s endpoint, like https://<resource>.openai.azure.com.",
      );
    const unpriced = await call(
      "PUT",
      AI_PATH,
      azure(
        "https://acme.openai.azure.com",
        { ...models, ask: { name: "prod-gpt", tableAs: "gpt-unknown" } },
        AZURE_KEY,
      ),
    );
    expect(unpriced.body.message).toBe(
      "Helix has no price for the ask model gpt-unknown; enter its prices",
    );
    const put = await call(
      "PUT",
      AI_PATH,
      azure("https://Acme.cognitiveservices.azure.com/", models, AZURE_KEY),
    );
    expect(put.status).toBe(200);
    // Stored normalized, as the WFE does; a hex key hints only at its end.
    expect(put.body.ai.pending).toMatchObject({
      kind: "FOUNDRY_AI_KIND_AZURE_OPENAI",
      provider: {
        azureOpenai: {
          endpoint: "https://acme.cognitiveservices.azure.com/openai/v1",
        },
      },
      models,
      keyHint: "…cdef",
      label: "Azure OpenAI",
    });
    tested();
    expect((await read()).active?.kind).toBe("FOUNDRY_AI_KIND_AZURE_OPENAI");
    expect(ai.aiModel("ws_acme", "draft")).toBe("prod-gpt");
    expect(ai.aiProvider("ws_acme")).toBe("azure_openai");
    expect(ai.aiLabel("ws_acme")).toBe("Azure OpenAI");
  });

  it("keeps an Azure key only for its resource and draft deployment", async () => {
    const { call, read } = await load();
    const models = each({ name: "prod-gpt", tableAs: "gpt-6.1-sol" });
    await call(
      "PUT",
      AI_PATH,
      azure("https://acme.openai.azure.com", models, AZURE_KEY),
    );
    tested();
    expect((await read()).active?.hasKey).toBe(true);
    // The same resource written another way, other reading deployments.
    const same = await call(
      "PUT",
      AI_PATH,
      azure("https://acme.openai.azure.com/openai/v1/", {
        ...models,
        extract: {
          name: "reader",
          rates: { inputUsdPerMtok: "0.1", outputUsdPerMtok: "0.4" },
        },
      }),
    );
    expect(same.status).toBe(200);
    tested();
    for (const next of [
      azure("https://globex.openai.azure.com", models),
      azure("https://acme.openai.azure.com", {
        ...models,
        draft: { name: "other-gpt", tableAs: "gpt-6.1-sol" },
      }),
      { provider: { openai: {} } },
    ])
      expect(
        (await call("PUT", AI_PATH, next)).body.message,
        JSON.stringify(next),
      ).toBe("Enter the API key for this endpoint.");
  });

  it("fails an Azure deployment that isn't there or can't call tools", async () => {
    const { call, read } = await load();
    for (const [name, failure] of [
      ["missing-deployment", "not_found"],
      ["gpt-notools", "unsupported"],
    ] as const) {
      await call(
        "PUT",
        AI_PATH,
        azure(
          "https://acme.openai.azure.com",
          each({ name, tableAs: "gpt-6.1-sol" }),
          AZURE_KEY,
        ),
      );
      tested();
      expect((await read()).lastFailed?.probeFailure).toBe(failure);
    }
    expect((await read()).active).toBeUndefined();
  });

  it("needs Foundry Pro for either, hosted", async () => {
    const { call, billing } = await load();
    billing.setFoundryPlan("ws_acme", "FOUNDRY_TIER_FREE");
    for (const body of [
      { provider: { openai: {} }, apiKey: OPENAI_KEY },
      azure(
        "https://acme.openai.azure.com",
        each({ name: "d", tableAs: "gpt-6.1-sol" }),
        AZURE_KEY,
      ),
    ])
      expect((await call("PUT", AI_PATH, body)).body.details[0].reason).toBe(
        "FOUNDRY_PRO_REQUIRED",
      );
  });
});

describe("Ollama", () => {
  const BUNDLED = "http://127.0.0.1:11434";
  const free = (name: string) => each({ name, free: {} });
  const ollama = (
    baseUrl: string,
    models: object = free("qwen3.5:4b"),
    contextTokens?: string,
  ) => ({
    provider: {
      ollama: {
        baseUrl,
        ...(contextTokens !== undefined && { contextTokens }),
      },
    },
    models,
  });

  it("is never offered hosted", async () => {
    const { call, read } = await load();
    expect((await read()).offered?.map((o) => o.kind)).not.toContain(
      "FOUNDRY_AI_KIND_OLLAMA",
    );
    const put = await call(
      "PUT",
      AI_PATH,
      ollama("https://ollama.example.com"),
    );
    expect(put.status).toBe(400);
    expect(put.body.message).toBe("Ollama is not offered here");
  });

  it("runs on a local install's Ollama once its test passes, keyless and at no cost", async () => {
    const { call, read, ai } = await load(true);
    expect((await read()).offered).toContainEqual({
      kind: "FOUNDRY_AI_KIND_OLLAMA",
      key: "FOUNDRY_AI_KEY_NONE",
    });
    // An owner names every model; unset is the WFE's window.
    expect(
      (
        await call("PUT", AI_PATH, {
          provider: { ollama: { baseUrl: BUNDLED } },
        })
      ).body.message,
    ).toBe("models.draft is required");
    const put = await call("PUT", AI_PATH, ollama(`${BUNDLED}/`));
    expect(put.status).toBe(200);
    expect(put.body.ai.pending).toMatchObject({
      kind: "FOUNDRY_AI_KIND_OLLAMA",
      provider: { ollama: { baseUrl: BUNDLED, contextTokens: "65536" } },
      models: free("qwen3.5:4b"),
      hasKey: false,
      keyHint: "",
      label: "Ollama",
    });
    tested();
    expect((await read()).active?.kind).toBe("FOUNDRY_AI_KIND_OLLAMA");
    expect(ai.aiLabel("ws_acme")).toBe("Ollama");
    expect(ai.aiProvider("ws_acme")).toBe("ollama");
    expect(ai.aiModel("ws_acme", "draft")).toBe("qwen3.5:4b");
    // Another server, its window as written; "0" is the WFE's.
    const other = await call(
      "PUT",
      AI_PATH,
      ollama("http://gpu.lan:11434", free("qwen3.5:9b"), "131072"),
    );
    expect(other.body.ai.pending.provider).toEqual({
      ollama: { baseUrl: "http://gpu.lan:11434", contextTokens: "131072" },
    });
    expect(
      (await call("PUT", AI_PATH, ollama(BUNDLED, free("m"), "0"))).body.ai
        .pending.provider.ollama.contextTokens,
    ).toBe("65536");
  });

  it("refuses a key, a window it can't load or a price", async () => {
    const { call } = await load(true);
    const refused = async (body: object, message: string) => {
      const r = await call("PUT", AI_PATH, body);
      expect(r.status, JSON.stringify(body)).toBe(400);
      expect(r.body.message).toBe(message);
    };
    await refused(
      { ...ollama(BUNDLED), apiKey: "sk-local-key1" },
      "Ollama takes no API key",
    );
    // Under drafting's window the WFE refuses the save, before any test:
    // every task runs in it (ai.MinContextTokens).
    for (const window of ["abc", "-1", "16384", "32768", "65535", "1048577"])
      await refused(
        ollama(BUNDLED, free("m"), window),
        "provider.context_tokens must be 65536 to 1048576",
      );
    expect(
      (await call("PUT", AI_PATH, ollama(BUNDLED, free("m"), "1048576")))
        .status,
    ).toBe(200);
    await refused(
      ollama("ftp://127.0.0.1:11434"),
      "Use an http:// or https:// address with no user, query or fragment.",
    );
    // The agent calls {base}/api/chat: Ollama's OpenAI-compatible API, or
    // its own API's path, is refused as the WFE refuses it.
    for (const base of [
      "http://gpu.lan:11434/v1",
      "http://gpu.lan:11434/api",
      "http://gpu.lan:11434/api/chat",
    ])
      await refused(
        ollama(base),
        "Use the Ollama server’s address, like http://127.0.0.1:11434, without /api or /v1.",
      );
    const unpriced = await call(
      "PUT",
      AI_PATH,
      ollama(BUNDLED, all("qwen3.5:4b")),
    );
    expect(unpriced.body.details[0].reason).toBe("FOUNDRY_AI_PRICE_REQUIRED");
  });

  it("fails its test on a model with a small window, one it lacks or one that changed, keeping the setting", async () => {
    const { call, read, ai } = await load(true);
    const cases: [object, string, string][] = [
      [
        ollama(BUNDLED, free("qwen3.5-short")),
        "unsupported",
        "context_too_small",
      ],
      [ollama(BUNDLED, free("qwen3.5-missing")), "not_found", "none"],
      [ollama(BUNDLED, free("qwen3.5-changed")), "model_changed", "none"],
      [
        ollama(BUNDLED, free("qwen3.5-notools")),
        "unsupported",
        "tools_rejected",
      ],
    ];
    for (const [body, failure, detail] of cases) {
      expect((await call("PUT", AI_PATH, body)).status).toBe(200);
      tested();
      const now = await read();
      expect(now.active, failure).toBeUndefined();
      expect(now.lastFailed).toMatchObject({
        probeState: "FOUNDRY_AI_PROBE_STATE_FAILED",
        probeFailure: failure,
        probeDetail: detail,
      });
      expect(ai.aiLabel("ws_acme")).toBe("Claude");
    }
  });

  it("is the install's default when its .env chose it", async () => {
    const { read, config, ai } = await load(true);
    config({ aiDefaultKind: "ollama" });
    expect((await read()).default).toEqual({
      kind: "FOUNDRY_AI_KIND_OLLAMA",
      label: "Ollama",
      models: free("qwen3.5:4b"),
    });
    expect(ai.aiSource("ws_acme")).toBe("default");
    expect(ai.aiLabel("ws_acme")).toBe("Ollama");
    expect(ai.aiProvider("ws_acme")).toBe("ollama");
    expect(ai.aiModel("ws_acme", "extract")).toBe("qwen3.5:4b");
    // Hosted, the knob does nothing.
    const hosted = await load();
    hosted.config({ aiDefaultKind: "ollama" });
    expect((await hosted.read()).default?.kind).toBe(
      "FOUNDRY_AI_KIND_ANTHROPIC",
    );
  });
});

describe("Amazon Bedrock", () => {
  it("runs on Claude's profiles in a region once the key passes its test", async () => {
    const { call, read, ai } = await load();
    const put = await call("PUT", AI_PATH, bedrock(" eu-west-1 "));
    expect(put.status).toBe(200);
    // The region is stored trimmed; FIPS left out is off.
    expect(put.body.ai.pending).toMatchObject({
      kind: "FOUNDRY_AI_KIND_BEDROCK",
      provider: { bedrock: { region: "eu-west-1" } },
      models: BEDROCK_SUGGESTED,
      keyHint: "…MTIz",
      label: "Amazon Bedrock",
    });
    expect(put.body.ai.pending.provider.bedrock).not.toHaveProperty("fips");
    expect(JSON.stringify(put.body)).not.toContain("sentinel");
    tested();
    expect((await read()).active?.kind).toBe("FOUNDRY_AI_KIND_BEDROCK");
    expect(ai.aiLabel("ws_acme")).toBe("Amazon Bedrock");
    expect(ai.aiProvider("ws_acme")).toBe("bedrock");
    expect(ai.aiModel("ws_acme", "extract")).toBe(
      "global.anthropic.claude-haiku-5-5",
    );
    expect(ai.draftFailure("ws_acme", "auth")).toBe(
      "Amazon Bedrock refused the API key. An owner can enter a new one in Settings › AI models.",
    );
    expect(ai.draftFailure("ws_acme", "refused")).toBe(
      "Amazon Bedrock declined to draft this ontology",
    );
  });

  it("keeps FIPS only when it is on", async () => {
    const { call } = await load();
    const on = await call("PUT", AI_PATH, bedrock("us-gov-west-1", {}, true));
    expect(on.body.ai.pending.provider).toEqual({
      bedrock: { region: "us-gov-west-1", fips: true },
    });
    const off = await call("PUT", AI_PATH, bedrock("us-east-1", {}, false));
    expect(off.body.ai.pending.provider).toEqual({
      bedrock: { region: "us-east-1" },
    });
  });

  it("refuses what isn't an AWS region", async () => {
    const { call } = await load();
    for (const region of [
      "",
      "US-EAST-1",
      "us-east-1a",
      "us-east-1.sentinel.example",
    ]) {
      const r = await call("PUT", AI_PATH, bedrock(region));
      expect(r.status, region).toBe(400);
      expect(r.body.message, region).toBe("Use an AWS region, like us-east-1.");
    }
  });

  it("prices inference profiles as their model, and others by a row or rates", async () => {
    const { call } = await load();
    const arn =
      "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/a1b2c3d4e5f6";
    const models = {
      draft: { name: `us.${SONNET}`, table: {} },
      ask: { name: `us.${SONNET}`, tableAs: SONNET },
      extract: {
        name: "amazon.nova-lite-v1:0",
        rates: {
          inputUsdPerMtok: "0.06",
          outputUsdPerMtok: "0.24",
          cachedInputUsdPerMtok: "0.015",
        },
      },
      verify: { name: arn, tableAs: `eu.${SONNET}` },
    };
    const put = await call("PUT", AI_PATH, bedrock("us-east-1", models));
    expect(put.status).toBe(200);
    expect(put.body.ai.pending.models).toEqual(models);
    const refuse = async (m: object, message: string, reason?: string) => {
      const r = await call("PUT", AI_PATH, bedrock("us-east-1", m));
      expect(r.body.message, JSON.stringify(m)).toBe(message);
      if (reason) expect(r.body.details[0].reason).toBe(reason);
    };
    await refuse(
      { ask: { name: "us.anthropic.claude-sentinel-next", table: {} } },
      "Helix has no price for the ask model us.anthropic.claude-sentinel-next; enter its prices",
      "FOUNDRY_AI_PRICE_REQUIRED",
    );
    await refuse(
      { extract: { name: `xx.${SONNET}`, table: {} } },
      `Helix has no price for the extract model xx.${SONNET}; enter its prices`,
      "FOUNDRY_AI_PRICE_REQUIRED",
    );
    await refuse(
      { verify: { name: "us.meta.llama", tableAs: "meta.llama" } },
      "Helix has no price for the verify model meta.llama; enter its prices",
      "FOUNDRY_AI_PRICE_REQUIRED",
    );
    await refuse(
      { draft: { name: SONNET, free: {} } },
      "models.draft.price can be free only for Ollama and OpenAI-compatible servers",
    );
    for (const name of [BEDROCK_KEY, "bedrock-api-key-YmVkcm9jaw"])
      await refuse(
        { draft: { name, table: {} } },
        "models.draft.name looks like an API key; enter the key in the API key field",
      );
    // Another model's row prices only an Azure deployment or a Bedrock model.
    const openai = await call("PUT", AI_PATH, {
      provider: { openai: {} },
      apiKey: OPENAI_KEY,
      models: { draft: { name: "gpt-x", tableAs: "gpt-6.1-sol" } },
    });
    expect(openai.body.message).toBe(
      "models.draft.price can be another model's only for an Azure deployment or a Bedrock inference profile",
    );
  });

  it("keeps a key across regions, FIPS and models, but for Bedrock alone", async () => {
    const { call, read } = await load();
    await call("PUT", AI_PATH, bedrock("us-east-1"));
    tested();
    for (const next of [
      { provider: { bedrock: { region: "eu-central-1" } } },
      { provider: { bedrock: { region: "us-east-1", fips: true } } },
      {
        provider: { bedrock: { region: "us-east-1" } },
        models: each({ name: `eu.${SONNET}`, table: {} }),
      },
    ]) {
      const kept = await call("PUT", AI_PATH, next);
      expect(kept.status, JSON.stringify(next)).toBe(200);
      expect(kept.body.ai.pending.keyHint).toBe("…MTIz");
      tested();
      expect((await read()).active?.hasKey).toBe(true);
    }
    expect(
      (await call("PUT", AI_PATH, { provider: { anthropic: {} } })).body
        .message,
    ).toBe("Enter the API key for this endpoint.");
    await call("PUT", AI_PATH, CLAUDE);
    tested();
    expect(
      (
        await call("PUT", AI_PATH, {
          provider: { bedrock: { region: "us-east-1" } },
        })
      ).body.message,
    ).toBe("Enter the API key for this endpoint.");
  });

  it("fails the test on a bad key, a model without tools or one it lacks", async () => {
    const { call, read } = await load();
    for (const [body, failure, detail] of [
      [
        { ...bedrock("us-east-1"), apiKey: "ABSKbad0123456789abcdefghij" },
        "auth",
        "none",
      ],
      [
        bedrock("us-east-1", {
          draft: {
            name: "us.meta.llama-notools",
            rates: { inputUsdPerMtok: "0.2", outputUsdPerMtok: "0.6" },
          },
        }),
        "unsupported",
        "tools_rejected",
      ],
      [
        bedrock("us-east-1", {
          ask: { name: "missing.model", tableAs: SONNET },
        }),
        "not_found",
        "none",
      ],
    ] as const) {
      await call("PUT", AI_PATH, body);
      tested();
      expect((await read()).lastFailed, failure).toMatchObject({
        probeFailure: failure,
        probeDetail: detail,
        label: "Amazon Bedrock",
        hasKey: false,
      });
    }
    expect((await read()).active).toBeUndefined();
  });

  it("needs Foundry Pro, hosted", async () => {
    const { call, billing } = await load();
    billing.setFoundryPlan("ws_acme", "FOUNDRY_TIER_FREE");
    expect(
      (await call("PUT", AI_PATH, bedrock("us-east-1"))).body.details[0].reason,
    ).toBe("FOUNDRY_PRO_REQUIRED");
  });
});

describe("a local API", () => {
  it("starts on the install's default, allowing private hosts and keys without a plan", async () => {
    const { read, call } = await load(true);
    const ai = await read();
    expect(ai.default?.label).toBe("Claude");
    expect(ai.privateHostsAllowed).toBe(true);
    expect(ai.proRequired).toBe(false);
    expect((await call("PUT", AI_PATH, CLAUDE)).status).toBe(200);
    // OpenAI, Azure OpenAI, custom servers and Amazon Bedrock too, with no
    // plan, and Ollama.
    expect(ai.offered?.map((o) => o.kind)).toEqual([
      "FOUNDRY_AI_KIND_ANTHROPIC",
      "FOUNDRY_AI_KIND_OPENAI",
      "FOUNDRY_AI_KIND_AZURE_OPENAI",
      "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE",
      "FOUNDRY_AI_KIND_OLLAMA",
      "FOUNDRY_AI_KIND_BEDROCK",
    ]);
    expect((await call("PUT", AI_PATH, bedrock("ap-southeast-2"))).status).toBe(
      200,
    );
    expect(
      (
        await call("PUT", AI_PATH, {
          provider: { openai: {} },
          apiKey: OPENAI_KEY,
        })
      ).status,
    ).toBe(200);
    expect(
      (
        await call(
          "PUT",
          AI_PATH,
          azure(
            "https://acme.openai.azure.com",
            each({ name: "d", tableAs: "gpt-6.1-sol" }),
            AZURE_KEY,
          ),
        )
      ).status,
    ).toBe(200);
  });

  it("refuses model work while no provider is set up", async () => {
    const { call, read, config, ai } = await load(true);
    config({ aiDefault: false });
    expect((await read()).default).toBeUndefined();
    expect(ai.aiSource("ws_acme")).toBe("none");
    expect(ai.aiModel("ws_acme", "draft")).toBeUndefined();
    const refused = (r: MockReply & { body: any }) => {
      expect(r.status).toBe(400);
      expect(r.body.message).toBe(
        "No AI provider is set up. An owner can choose one in Settings › AI models.",
      );
      expect(r.body.details[0]).toMatchObject({
        reason: "FOUNDRY_AI_NOT_CONFIGURED",
        domain: "foundry.helix-db.com",
      });
    };
    refused(
      await call(
        "POST",
        `${WS}/foundry/ontologies/ont_acme_contracts:generate`,
        {},
      ),
    );
    refused(
      await call("POST", `${WS}/foundry/ontologies/ont_acme_sales/questions`, {
        clientToken: "t",
        question: "Who?",
      }),
    );
    const created = await call("POST", `${WS}/foundry/documents`, {
      name: "notes.md",
      mediaType: "text/markdown",
      sizeBytes: "5",
      sha256: "0".repeat(64),
      clientToken: "doc",
      setId: "set_acme_general",
    });
    expect(created.status).toBe(200);
    refused(
      await call(
        "POST",
        `${WS}/foundry/documents/${created.body.document.id}:complete`,
      ),
    );
    expect((await call("PUT", AI_PATH, { useDefault: {} })).body.message).toBe(
      "this install has no default AI provider; choose one",
    );

    // Once an owner's Claude passes its test, model work runs.
    await call("PUT", AI_PATH, CLAUDE);
    tested();
    expect(ai.aiSource("ws_acme")).toBe("workspace");
    expect(ai.aiRefusal("ws_acme")).toBeUndefined();
  });
});

describe("a custom OpenAI-compatible server", () => {
  const free = each({ name: "qwen3-32b", free: {} });
  const SERVER_KEY = "sk-or-v1-sentinel-0123456789abcdefwxyz";

  it("runs on a public https server once its test passes, keyless", async () => {
    const { call, read, ai } = await load();
    const put = await call("PUT", AI_PATH, {
      provider: server("https://LLM.Example.com/v1/", {
        api: "FOUNDRY_AI_COMPAT_API_RESPONSES",
      }),
      models: free,
    });
    expect(put.status).toBe(200);
    // Stored normalized, with the WFE's 4 calls at once for none given;
    // labelled by its host.
    expect(put.body.ai.pending).toMatchObject({
      kind: "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE",
      provider: {
        openaiCompatible: {
          baseUrl: "https://llm.example.com/v1",
          api: "FOUNDRY_AI_COMPAT_API_RESPONSES",
          maxCallsInFlight: 4,
        },
      },
      models: free,
      hasKey: false,
      keyHint: "",
      label: "llm.example.com",
    });
    tested();
    expect((await read()).active?.label).toBe("llm.example.com");
    expect(ai.aiSource("ws_acme")).toBe("workspace");
    expect(ai.aiLabel("ws_acme")).toBe("llm.example.com");
    expect(ai.aiProvider("ws_acme")).toBe("openai_compatible");
    expect(ai.aiModel("ws_acme", "ask")).toBe("qwen3-32b");
    expect(ai.draftFailure("ws_acme", "auth")).toBe(
      "llm.example.com refused the API key. An owner can enter a new one in Settings › AI models.",
    );
    expect(ai.draftFailure("ws_acme", "refused")).toBe(
      "llm.example.com declined to draft this ontology",
    );
  });

  it("refuses what the WFE refuses, hosted, in its words", async () => {
    const { call } = await load();
    const refuse = async (provider: object, message: string, models = free) => {
      const r = await call("PUT", AI_PATH, { provider, models });
      expect(r.status, JSON.stringify(provider)).toBe(400);
      expect(r.body.message, JSON.stringify(provider)).toBe(message);
    };
    // Its API and calls at once, before anything else.
    for (const api of [undefined, "FOUNDRY_AI_COMPAT_API_UNSPECIFIED"])
      await refuse(
        { openaiCompatible: { baseUrl: "https://llm.example.com/v1", api } },
        "provider.api must be chat_completions or responses",
      );
    for (const maxCallsInFlight of [65, -1, 2.5])
      await refuse(
        server("https://llm.example.com/v1", { maxCallsInFlight }),
        "provider.max_calls_in_flight must be 1 to 64",
      );
    for (const bad of ["llm.example.com/v1", "ftp://llm.example.com", ""])
      await refuse(
        server(bad),
        "provider.base_url must be an http or https URL with a host",
      );
    for (const bad of [
      "https://user:pass@llm.example.com/v1",
      "https://llm.example.com/v1?key=x",
      "https://llm.example.com/v1?",
      "https://llm.example.com/v1#top",
    ])
      await refuse(
        server(bad),
        "provider.base_url must have no user, query or fragment",
      );
    await refuse(
      server("http://llm.example.com/v1"),
      "provider.base_url must be https",
    );
    // Hosted, Helix calls only public addresses, a name's included.
    for (const host of [
      "127.0.0.1",
      "10.0.0.5:8000",
      "169.254.169.254",
      "[::1]",
      "localhost:1234",
      "host.docker.internal",
      "llm.corp.internal",
    ])
      await refuse(
        server(`https://${host}/v1`),
        "the endpoint resolves to a private address",
      );
    await refuse(
      server("https://llm.example.invalid/v1"),
      "provider names a host that does not resolve",
    );
    // Helix's table prices none of its models.
    const unpriced = await call("PUT", AI_PATH, {
      provider: server("https://llm.example.com/v1"),
      models: all("qwen3-32b"),
    });
    expect(unpriced.body.message).toBe(
      "Helix has no price for the draft model qwen3-32b; enter its prices",
    );
    expect(unpriced.body.details[0].reason).toBe("FOUNDRY_AI_PRICE_REQUIRED");
    // Its own rates are taken.
    expect(
      (
        await call("PUT", AI_PATH, {
          provider: server("https://llm.example.com/v1", {
            maxCallsInFlight: 64,
          }),
          models: each({
            name: "qwen3-32b",
            rates: { inputUsdPerMtok: "0.1", outputUsdPerMtok: "0.3" },
          }),
        })
      ).status,
    ).toBe(200);
  });

  it("keeps its key only for the same base URL", async () => {
    const { call, read } = await load();
    await call("PUT", AI_PATH, {
      provider: server("https://llm.example.com/v1"),
      models: free,
      apiKey: SERVER_KEY,
    });
    tested();
    const first = (await read()).active!;
    expect(first).toMatchObject({ hasKey: true, keyHint: "sk-or…wxyz" });
    // The same URL written another way, other models and calls at once.
    const same = await call("PUT", AI_PATH, {
      provider: server("https://LLM.example.com/v1/", { maxCallsInFlight: 2 }),
      models: each({ name: "qwen3-8b", free: {} }),
    });
    expect(same.body.ai.pending).toMatchObject({
      hasKey: true,
      keyHint: "sk-or…wxyz",
    });
    tested();
    expect((await read()).active?.models?.draft?.name).toBe("qwen3-8b");
    // Another URL gets no key unless one is given: the server may take none.
    const other = await call("PUT", AI_PATH, {
      provider: server("https://llm.example.com/v2"),
      models: free,
    });
    expect(other.status).toBe(200);
    expect(other.body.ai.pending).toMatchObject({ hasKey: false, keyHint: "" });
    expect(JSON.stringify(other.body)).not.toContain("sentinel");
  });

  it("fails its test on a bad key or a model without tools, and keeps the setting", async () => {
    const { call, read } = await load();
    await call("PUT", AI_PATH, {
      provider: server("https://llm.example.com/v1"),
      models: free,
      apiKey: "sk-bad-0123456789abcdef0123",
    });
    tested();
    expect((await read()).lastFailed).toMatchObject({
      probeFailure: "auth",
      label: "llm.example.com",
      hasKey: false,
    });
    await call("PUT", AI_PATH, {
      provider: server("https://llm.example.com/v1"),
      models: each({ name: "tiny-notools", free: {} }),
    });
    tested();
    const ai = await read();
    expect(ai.lastFailed).toMatchObject({
      probeFailure: "unsupported",
      probeDetail: "tools_rejected",
    });
    expect(ai.active).toBeUndefined();
  });

  it("needs Foundry Pro, hosted", async () => {
    const { call, billing } = await load();
    billing.setFoundryPlan("ws_acme", "FOUNDRY_TIER_FREE");
    const r = await call("PUT", AI_PATH, {
      provider: server("https://llm.example.com/v1"),
      models: free,
    });
    expect(r.body.details[0].reason).toBe("FOUNDRY_PRO_REQUIRED");
  });

  it("takes http and private addresses in a local API, and fails one that isn't running", async () => {
    const { call, read, ai } = await load(true);
    for (const baseUrl of [
      "http://host.docker.internal:1234/v1",
      "http://127.0.0.1:8000/v1",
      "http://192.168.1.20:11434/v1",
      "https://10.0.0.5/v1",
    ])
      expect(
        (
          await call("PUT", AI_PATH, {
            provider: server(baseUrl),
            models: free,
          })
        ).status,
        baseUrl,
      ).toBe(200);
    tested();
    expect(ai.aiLabel("ws_acme")).toBe("10.0.0.5");
    // Still a URL with no user, query or fragment.
    expect(
      (
        await call("PUT", AI_PATH, {
          provider: server("http://host.docker.internal:1234/v1?x=1"),
          models: free,
        })
      ).body.message,
    ).toBe("provider.base_url must have no user, query or fragment");
    // A server nobody answers at fails its test, after retries.
    await call("PUT", AI_PATH, {
      provider: server("http://lmstudio.invalid:1234/v1"),
      models: free,
    });
    tested();
    const now = await read();
    expect(now.lastFailed).toMatchObject({
      probeFailure: "unavailable",
      label: "lmstudio.invalid",
    });
    expect(now.active?.label).toBe("10.0.0.5");
  });
});
