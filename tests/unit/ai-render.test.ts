import { afterEach, describe, expect, it, vi } from "vitest";
import type * as wfe from "@/shared/wfe";

// Settings › AI models as an owner first sees it: the Ollama card only in a
// local build, first of the providers there, with its server, model and
// context window and no key; a saved Ollama server read back into its
// fields; and an Ollama test's failure in Ollama's words. The build is
// read as the page loads (src/shared/deployment.ts), so each render loads
// the page afresh for it.

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

async function render(local: boolean, ai: wfe.FoundryAi) {
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
  const { aiKey } = await import("@/web/areas/workspace/ai-api");
  const qc = new QueryClient();
  qc.setQueryData(aiKey("ws_1"), ai);
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
    const html = await render(true, {
      offered: OFFERED,
      privateHostsAllowed: true,
    });
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
      "The Ollama that runs with Foundry",
    );
    expect(html).not.toContain("Server address");
    expect(field(html, "Model")).toContain('value="qwen3.5:4b"');
    expect(field(html, "Model")).toContain("No cost is counted.");
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
    const html = await render(true, {
      active: saved,
      default: { kind: "FOUNDRY_AI_KIND_ANTHROPIC", label: "Claude" },
      offered: OFFERED,
    });
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
    // A model per task, as saved.
    expect(field(html, "Drafting")).toContain('value="qwen3.5:9b"');
    expect(field(html, "Reading documents")).toContain('value="qwen3.5:4b"');
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
});
