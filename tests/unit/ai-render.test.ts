import { afterEach, describe, expect, it, vi } from "vitest";
import type * as wfe from "@/shared/wfe";

// Settings › AI models as an owner first sees it: the Ollama card only in a
// local build, first of the providers there, with its server, the models
// its server has and its context window and no key; a saved Ollama server
// read back into its fields; an Ollama test's failure in Ollama's words;
// and how a local install found its default, or what it looked for. The
// build is read as the page loads (src/shared/deployment.ts), so each
// render loads the page afresh for it.

afterEach(() => vi.unstubAllEnvs());

const OFFERED: wfe.FoundryAiOffer[] = [
  { kind: "FOUNDRY_AI_KIND_ANTHROPIC", key: "FOUNDRY_AI_KEY_REQUIRED" },
  { kind: "FOUNDRY_AI_KIND_OPENAI", key: "FOUNDRY_AI_KEY_REQUIRED" },
  { kind: "FOUNDRY_AI_KIND_AZURE_OPENAI", key: "FOUNDRY_AI_KEY_REQUIRED" },
  { kind: "FOUNDRY_AI_KIND_OLLAMA", key: "FOUNDRY_AI_KEY_NONE" },
];
const free = (name: string) =>
  Object.fromEntries(
    ["draft", "ask", "extract", "verify"].map((r) => [r, { name, free: {} }]),
  );

// What the bundled server has, as the mock's.
const BUNDLED: wfe.FoundryOllamaServer = {
  version: "0.13.0",
  models: [
    { name: "qwen3.5:4b", sizeBytes: "3389725347" },
    {
      name: "embeddinggemma",
      sizeBytes: "621875047",
      unusable: "Not a chat model.",
    },
  ],
};

async function render(
  local: boolean,
  ai: wfe.FoundryAi,
  // What each Ollama server has, by base URL, as read already.
  ollama: Record<string, wfe.FoundryOllamaServer> = {},
) {
  vi.resetModules();
  vi.stubEnv("NEXT_PUBLIC_FOUNDRY_MODE", local ? "local" : "");
  const { createElement } = await import("react");
  const { renderToStaticMarkup } = await import("react-dom/server");
  const { QueryClient, QueryClientProvider } = await import(
    "@tanstack/react-query"
  );
  const { MemoryRouter } = await import("react-router-dom");
  const { WorkspaceContext } = await import("@/web/ui");
  const { AiTab } = await import("@/web/areas/workspace/ai");
  const { aiKey, ollamaModelsQuery } = await import(
    "@/web/areas/workspace/ai-api"
  );
  const qc = new QueryClient();
  qc.setQueryData(aiKey("ws_1"), ai);
  for (const [url, server] of Object.entries(ollama))
    qc.setQueryData(ollamaModelsQuery("ws_1", url).queryKey, server);
  return renderToStaticMarkup(
    createElement(
      QueryClientProvider,
      { client: qc },
      createElement(
        WorkspaceContext.Provider,
        {
          value: {
            id: "ws_1",
            slug: "acme",
            name: "Acme",
            role: "owner",
            user: { id: "usr_1", name: "Ada", email: "ada@example.com" },
            onboarding: false,
            notify: () => {},
          },
        },
        createElement(MemoryRouter, null, createElement(AiTab)),
      ),
    ),
  );
}

// The provider radios' names, in order, and the one checked.
const cards = (html: string) =>
  [...html.matchAll(/<input type="radio"([^>]*)>/g)].map(
    ([, attrs]) =>
      `${/aria-label="([^"]*)"/.exec(attrs!)![1]}${/ checked=""/.test(attrs!) ? " (checked)" : ""}`,
  );
const field = (html: string, label: string) =>
  new RegExp(`<span>${label}</span>(.*?)</label>`).exec(html)?.[1] ?? "";

describe("the Ollama card", () => {
  it("is hidden hosted, even offered", async () => {
    const html = await render(false, {
      default: { kind: "FOUNDRY_AI_KIND_ANTHROPIC", label: "Claude" },
      offered: OFFERED,
    });
    expect(cards(html)).toEqual([
      "Default (checked)",
      "Claude",
      "OpenAI",
      "Azure OpenAI",
    ]);
    expect(html).not.toContain("Ollama");
  });

  it("comes first locally, on the bundled server with the install's model, keyless", async () => {
    const html = await render(
      true,
      { offered: OFFERED, privateHostsAllowed: true },
      { "http://127.0.0.1:11434": BUNDLED },
    );
    // No default: the first provider is chosen.
    expect(cards(html)).toEqual([
      "Ollama (checked)",
      "Claude",
      "OpenAI",
      "Azure OpenAI",
    ]);
    expect(html).toContain("Runs on this machine; nothing leaves it.");
    expect(field(html, "Server")).toContain("Bundled");
    expect(field(html, "Server")).toContain(
      "The Ollama that runs with Foundry, from the compose file’s ollama profile: docker compose --profile ollama up -d.",
    );
    expect(field(html, "Server")).not.toContain("setup.sh");
    expect(html).not.toContain("Server address");
    // One the server has, with its size; nothing to type.
    expect(field(html, "Model")).toContain("qwen3.5:4b · 3.4 GB");
    expect(field(html, "Model")).toContain("No cost is counted.");
    expect(html).not.toContain("<span>Model name</span>");
    // Drafting's window, which the bundled server loads models with.
    expect(field(html, "Context window")).toContain('value="65536"');
    expect(field(html, "Context window")).toContain(
      "drafting needs 65,536, questions 32,768 and the others 16,384. A larger window takes more memory on the server.",
    );
    expect(html).toContain("Choose a model per task");
    expect(html).toContain("a small local model may stop a draft early");
    expect(html).not.toContain("<span>API key</span>");
    // Its fields in the order the card reads them.
    const at = (f: string) => html.indexOf(`<span>${f}</span>`);
    expect(at("Server")).toBeGreaterThan(-1);
    expect(at("Server")).toBeLessThan(at("Model"));
    expect(at("Model")).toBeLessThan(at("Context window"));
    expect(html).toContain("Test and save");
  });

  it("starts from the saved server, window and models", async () => {
    const saved: wfe.FoundryAiConfig = {
      id: "cfg_1",
      kind: "FOUNDRY_AI_KIND_OLLAMA",
      provider: {
        ollama: { baseUrl: "http://gpu.lan:11434", contextTokens: "65536" },
      },
      models: {
        ...free("qwen3.5:9b"),
        extract: { name: "qwen3.5:4b", free: {} },
      },
      label: "Ollama",
      probeState: "FOUNDRY_AI_PROBE_STATE_PASSED",
    };
    const html = await render(
      true,
      {
        active: saved,
        default: { kind: "FOUNDRY_AI_KIND_ANTHROPIC", label: "Claude" },
        offered: OFFERED,
      },
      {
        "http://gpu.lan:11434": {
          models: [
            { name: "qwen3.5:9b", sizeBytes: "6591893504" },
            { name: "qwen3.5:4b", sizeBytes: "3389725347" },
          ],
        },
      },
    );
    expect(cards(html)).toEqual([
      "Install default",
      "Ollama (checked)",
      "Claude",
      "OpenAI",
      "Azure OpenAI",
    ]);
    // The card says where it goes.
    expect(html.split("Sent to the Ollama server at gpu.lan.")).toHaveLength(2);
    expect(field(html, "Server")).toContain("Other");
    expect(field(html, "Server address")).toContain(
      'value="http://gpu.lan:11434"',
    );
    expect(field(html, "Context window")).toContain('value="65536"');
    // A model per task, as saved, as the server lists it.
    expect(field(html, "Drafting")).toContain("qwen3.5:9b · 6.6 GB");
    expect(field(html, "Reading documents")).toContain("qwen3.5:4b · 3.4 GB");
  });

  it("says a cloud model's work leaves this machine, in the card", async () => {
    const html = await render(true, {
      active: {
        id: "cfg_3",
        kind: "FOUNDRY_AI_KIND_OLLAMA",
        provider: {
          ollama: {
            baseUrl: "http://host.docker.internal:11434",
            contextTokens: "65536",
          },
        },
        models: { ...free("qwen3.5:4b"), ask: { name: "gpt-oss:120b-cloud" } },
        label: "Ollama",
        probeState: "FOUNDRY_AI_PROBE_STATE_PASSED",
      },
      offered: OFFERED,
    });
    expect(
      html.split("Sent to ollama.com through your Ollama server."),
    ).toHaveLength(2);
    expect(html).not.toContain("nothing leaves it");
    // The install default's models count too.
    const fallback = await render(true, {
      default: {
        kind: "FOUNDRY_AI_KIND_OLLAMA",
        label: "Ollama",
        models: free("glm-5.3:cloud"),
      },
      offered: OFFERED,
    });

    expect(fallback).toContain(
      "Ollama, from .env. Sent to ollama.com through your Ollama server.",
    );
  });

  it("says why an Ollama test failed, in Ollama's words", async () => {
    const html = await render(true, {
      default: { kind: "FOUNDRY_AI_KIND_ANTHROPIC", label: "Claude" },
      lastFailed: {
        id: "cfg_2",
        kind: "FOUNDRY_AI_KIND_OLLAMA",
        provider: {
          ollama: {
            baseUrl: "http://host.docker.internal:11434",
            contextTokens: "8192",
          },
        },
        models: free("qwen3.5:4b"),
        label: "Ollama",
        probeState: "FOUNDRY_AI_PROBE_STATE_FAILED",
        probeFailure: "unsupported",
        probeDetail: "context_too_small",
      },
      offered: OFFERED,
    });
    expect(html).toContain(
      "The context window is too small for Foundry’s tasks: drafting needs 65,536 tokens, questions 32,768 and the others 16,384. Choose a model that takes one that large, or raise the window. Your current settings stay active.",
    );
  });

  it("names an install default of Ollama", async () => {
    const html = await render(true, {
      default: {
        kind: "FOUNDRY_AI_KIND_OLLAMA",
        label: "Ollama",
        models: free("qwen3.5:4b"),
      },
      offered: OFFERED,
    });
    expect(html).toContain("Ollama, from .env. Sent to your Ollama server.");
    expect(cards(html)[0]).toBe("Install default (checked)");
  });

  it("keeps the model chosen while the server's are read", async () => {
    const html = await render(true, { offered: OFFERED });
    expect(field(html, "Model")).toContain("qwen3.5:4b");
    expect(field(html, "Model")).toContain(
      "Reading the models the server has…",
    );
    expect(html).not.toContain("<span>Model name</span>");
  });

  it("offers a model the server can't run, greyed with why, and another by name", async () => {
    const html = await render(
      true,
      { offered: OFFERED },
      {
        "http://127.0.0.1:11434": {
          version: "0.13.0",
          models: [
            {
              name: "embeddinggemma",
              sizeBytes: "621875047",
              unusable: "Not a chat model.",
            },
          ],
        },
      },
    );
    // Nothing on it Foundry runs: the install's model, typed, which the
    // server hasn't pulled.
    expect(field(html, "Model")).toContain("Another model");
    expect(field(html, "Model name")).toContain('value="qwen3.5:4b"');
    expect(field(html, "Model name")).toContain(
      "The server hasn’t pulled qwen3.5:4b. Pull it there with ollama pull qwen3.5:4b before testing it.",
    );
  });

  it("says why a server's models can't be listed, and takes one by name", async () => {
    const problem =
      "The bundled Ollama isn't running. Start it with docker compose --profile ollama up -d.";
    const html = await render(
      true,
      { offered: OFFERED },
      { "http://127.0.0.1:11434": { problem } },
    );
    expect(field(html, "Model name")).toContain('value="qwen3.5:4b"');
    // As the markup escapes it.
    expect(field(html, "Model name")).toContain(problem.replace("'", "&#x27;"));
  });
});

describe("the install default", () => {
  it("says how a local install found it", async () => {
    const summary =
      "Using qwen3.5:4b from the bundled Ollama: the most capable model it has that calls tools.";
    const html = await render(true, {
      default: {
        kind: "FOUNDRY_AI_KIND_OLLAMA",
        label: "Ollama",
        models: free("qwen3.5:4b"),
      },
      detected: { summary, detectedAt: "2026-10-27T10:00:00Z" },
      offered: OFFERED,
    });
    expect(cards(html)[0]).toBe("Install default (checked)");
    expect(html).toContain(`${summary} Sent to your Ollama server.`);
    expect(html).not.toContain("from .env");
  });

  it("says what a local install looked for when it found none", async () => {
    const summary =
      "No AI provider was found. Foundry looks for an ANTHROPIC_API_KEY or OPENAI_API_KEY where it was started, a model that calls tools in Ollama, and a model LM Studio serves on this computer.";
    const html = await render(true, {
      detected: { summary },
      offered: OFFERED,
    });
    expect(cards(html)).not.toContain("Install default");
    expect(html).toContain(summary);
    expect(html).not.toContain("No AI provider is set up.");
    // Named by the install as none: today's words.
    expect(await render(true, { offered: OFFERED })).toContain(
      "No AI provider is set up.",
    );
  });
});
