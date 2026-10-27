import { readFileSync } from "node:fs";
import { afterEach, describe, expect, it, vi } from "vitest";
import type * as wfe from "@/shared/wfe";
import { ApiError } from "@/web/api";
import * as AI from "@/web/areas/workspace/ai-api";
import { FAILURE_TEXT } from "@/web/areas/ontology/ask-model";

// Settings › AI models' rules and words: every kind the
// WFE declares has a card, every failure the agent declares has copy, the
// key and endpoint rules mirror the WFE's (ai.CanReuseKey, ai.ModelURL,
// ai.AzureEndpoint), and each @example of src/web/areas/workspace/ai-api.ts
// holds.

const read = (path: string) =>
  JSON.parse(readFileSync(new URL(path, import.meta.url), "utf8"));
const swagger = read("../../contract/foundry.swagger.json") as {
  definitions: Record<string, { enum?: string[] }>;
};
// The agent's oneOf of string constants, by definition.
const constants = (schema: string, def: string) =>
  (
    read(`../../contract/ontology_agent/${schema}.json`).$defs[def].oneOf as {
      const: string;
    }[]
  ).map((o) => o.const);

const KINDS = swagger.definitions.v1FoundryAIKind!.enum!.filter(
  (k) => k !== "FOUNDRY_AI_KIND_UNSPECIFIED",
) as wfe.FoundryAiProviderKind[];
const ROLE_MODELS = (name: string) => ({
  draft: name,
  ask: name,
  extract: name,
  verify: name,
});

describe("every kind and failure the contracts declare", () => {
  it("has a card, a mark or a text card, and a disclosure line", () => {
    expect(KINDS).toHaveLength(6);
    expect(Object.keys(AI.PROVIDERS).sort()).toEqual([...KINDS].sort());
    expect(AI.PROVIDER_KINDS.sort()).toEqual([...KINDS].sort());
    for (const kind of KINDS) {
      expect(AI.PROVIDERS[kind].title).not.toBe("");
      expect(AI.PROVIDERS[kind].label).not.toBe("");
      expect(AI.disclosure(kind)).toMatch(/\.$/);
    }
    // The agent's kinds, which its catalog rows and Ask events name.
    expect(KINDS.map((k) => AI.PROVIDERS[k].agent).sort()).toEqual(
      constants("ask_event", "ProviderKind").sort(),
    );
  });

  it("has copy for each model failure and probe detail", () => {
    const failures = constants("probe_result", "ModelFailure"),
      details = constants("probe_result", "ProbeDetail");
    expect(Object.keys(AI.FAILURE_COPY).sort()).toEqual([...failures].sort());
    expect(Object.keys(AI.PROBE_DETAIL_COPY).sort()).toEqual(
      [...details].sort(),
    );
    for (const f of failures)
      for (const model of ["claude-x", undefined])
        expect(
          AI.FAILURE_COPY[f as wfe.AgentModelFailure]({
            label: "Claude",
            model,
          }),
        ).toMatch(/^[A-Za-z].*\.$/);
    for (const d of details.filter((d) => d !== "none"))
      expect(AI.PROBE_DETAIL_COPY[d as wfe.AgentProbeDetail]).toMatch(
        /^[A-Z].*\.$/,
      );
  });

  it("has copy for each way a question fails", () => {
    expect(Object.keys(FAILURE_TEXT).sort()).toEqual(
      constants("ask_result", "Failure").sort(),
    );
  });

  it("names the tasks of the WFE's models", () => {
    expect(AI.ROLE_IDS).toEqual(
      Object.keys(
        (
          read("../../contract/foundry.swagger.json").definitions
            .v1FoundryAIModels as { properties: object }
        ).properties,
      ),
    );
  });
});

describe("the disclosure lines", () => {
  it("say where each kind sends the work", () => {
    expect(AI.disclosure("FOUNDRY_AI_KIND_ANTHROPIC")).toBe(
      "Sent to Anthropic under your API key.",
    );
    expect(AI.disclosure("FOUNDRY_AI_KIND_OPENAI")).toBe(
      "Sent to OpenAI under your API key, with response storage off.",
    );
    expect(
      AI.disclosure("FOUNDRY_AI_KIND_AZURE_OPENAI", {
        azureOpenai: { endpoint: "https://acme.openai.azure.com/openai/v1" },
      }),
    ).toBe("Sent to your Azure OpenAI resource at acme.openai.azure.com.");
    expect(AI.disclosure("FOUNDRY_AI_KIND_AZURE_OPENAI")).toBe(
      "Sent to your Azure OpenAI resource.",
    );
    expect(
      AI.disclosure("FOUNDRY_AI_KIND_BEDROCK", {
        bedrock: { region: "eu-west-1" },
      }),
    ).toBe("Sent to Amazon Bedrock in eu-west-1, under your AWS account.");
    expect(
      AI.disclosure("FOUNDRY_AI_KIND_BEDROCK", {
        bedrock: { region: "us-gov-west-1", fips: true },
      }),
    ).toBe(
      "Sent to Amazon Bedrock’s FIPS endpoint in us-gov-west-1, under your AWS account.",
    );
    expect(AI.disclosure("FOUNDRY_AI_KIND_BEDROCK")).toBe(
      "Sent to Amazon Bedrock, under your AWS account.",
    );
    for (const baseUrl of [
      "http://127.0.0.1:11434",
      "http://host.docker.internal:11434",
    ])
      expect(
        AI.disclosure("FOUNDRY_AI_KIND_OLLAMA", { ollama: { baseUrl } }),
      ).toBe("Runs on this machine; nothing leaves it.");
    expect(
      AI.disclosure("FOUNDRY_AI_KIND_OLLAMA", {
        ollama: { baseUrl: "http://gpu.lan:11434" },
      }),
    ).toBe("Sent to the Ollama server at gpu.lan.");
    expect(
      AI.disclosure("FOUNDRY_AI_KIND_OPENAI_COMPATIBLE", {
        openaiCompatible: { baseUrl: "https://llm.example.com/v1" },
      }),
    ).toBe(
      "Sent to llm.example.com. Helix can’t see where that endpoint sends it.",
    );
    expect(AI.disclosure("FOUNDRY_AI_KIND_OPENAI_COMPATIBLE")).toBe(
      "Sent to your endpoint. Helix can’t see where that endpoint sends it.",
    );
  });

  it("say where a Bedrock inference profile may run the work outside its region", () => {
    const bedrock = (models: string[], fips = false) =>
      AI.disclosure(
        "FOUNDRY_AI_KIND_BEDROCK",
        { bedrock: { region: "eu-central-1", ...(fips && { fips }) } },
        models,
      );
    const SENT =
      "Sent to Amazon Bedrock in eu-central-1, under your AWS account.";
    const GLOBAL =
      "A global inference profile may run requests in any AWS commercial region.";
    // The models in the region: base IDs, a model's ARN, an application
    // inference profile's (which says nothing of where it runs), none.
    expect(
      bedrock([
        "anthropic.claude-opus-5-5",
        "arn:aws:bedrock:eu-central-1::foundation-model/amazon.nova-pro-v1:0",
        "arn:aws:bedrock:eu-central-1:123456789012:application-inference-profile/a1b2c3",
      ]),
    ).toBe(SENT);
    expect(bedrock([])).toBe(SENT);
    // Recommended: Claude through global profiles, which leave the EU.
    expect(
      bedrock([
        "global.anthropic.claude-opus-5-5",
        "global.anthropic.claude-sonnet-5-5",
        "global.anthropic.claude-haiku-5-5",
        "global.anthropic.claude-sonnet-5-5",
      ]),
    ).toBe(`${SENT} ${GLOBAL}`);
    // One global model is enough, a system profile's ARN too; FIPS alike.
    expect(bedrock(["eu.anthropic.claude-opus-5-5", " global.x "])).toBe(
      `${SENT} ${GLOBAL}`,
    );
    expect(
      bedrock([
        "arn:aws:bedrock:eu-central-1:123456789012:inference-profile/global.anthropic.claude-opus-5-5",
      ]),
    ).toBe(`${SENT} ${GLOBAL}`);
    expect(bedrock(["global.anthropic.claude-opus-5-5"], true)).toBe(
      `Sent to Amazon Bedrock’s FIPS endpoint in eu-central-1, under your AWS account. ${GLOBAL}`,
    );
    expect(
      AI.disclosure("FOUNDRY_AI_KIND_BEDROCK", {}, [
        "global.anthropic.claude-opus-5-5",
      ]),
    ).toBe(`Sent to Amazon Bedrock, under your AWS account. ${GLOBAL}`);
    // A geography's profiles stay within it, each named once.
    expect(
      bedrock(["eu.anthropic.claude-opus-5-5", "eu.amazon.nova-2-lite-v1:0"]),
    ).toBe(
      `${SENT} A cross-region inference profile may run requests in other regions within the EU.`,
    );
    for (const [prefix, within] of [
      ["us.", "the US"],
      ["us-gov.", "AWS GovCloud (US)"],
      ["apac.", "Asia Pacific"],
      ["au.", "Australia"],
      ["jp.", "Japan"],
      ["ca.", "Canada"],
    ])
      expect(bedrock([`${prefix}anthropic.claude-opus-5-5`]), prefix).toBe(
        `${SENT} A cross-region inference profile may run requests in other regions within ${within}.`,
      );
    expect(
      bedrock([
        "us.anthropic.claude-opus-5-5",
        "anthropic.claude-haiku-5-5",
        "eu.anthropic.claude-sonnet-5-5",
      ]),
    ).toBe(
      `${SENT} A cross-region inference profile may run requests in other regions within the US and the EU.`,
    );
    // A prefix counts only at the ID's start; other kinds take no models.
    expect(bedrock(["anthropic.global.x", "meta.us.llama"])).toBe(SENT);
    expect(AI.disclosure("FOUNDRY_AI_KIND_OPENAI", {}, ["global.gpt"])).toBe(
      "Sent to OpenAI under your API key, with response storage off.",
    );
  });

  it("say where the deployment's default sends it, a custom server by its label", () => {
    expect(
      AI.defaultDisclosure({
        kind: "FOUNDRY_AI_KIND_ANTHROPIC",
        label: "Claude",
      }),
    ).toBe("Sent to Anthropic under your API key.");
    expect(
      AI.defaultDisclosure({
        kind: "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE",
        label: "host.docker.internal",
      }),
    ).toBe(
      "Sent to host.docker.internal. Helix can’t see where that endpoint sends it.",
    );
    // The WFE's label when it couldn't read a host.
    expect(
      AI.defaultDisclosure({
        kind: "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE",
        label: "the custom endpoint",
      }),
    ).toBe(
      "Sent to your endpoint. Helix can’t see where that endpoint sends it.",
    );
    expect(
      AI.defaultDisclosure({ kind: "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE" }),
    ).toBe(
      "Sent to your endpoint. Helix can’t see where that endpoint sends it.",
    );
    expect(AI.defaultDisclosure({ kind: "FOUNDRY_AI_KIND_UNSPECIFIED" })).toBe(
      "",
    );
    expect(AI.defaultDisclosure(undefined)).toBe("");
  });
});

describe("a custom OpenAI-compatible server", () => {
  const COMPAT = "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE";

  it("speaks one of the APIs the WFE declares", () => {
    expect(Object.keys(AI.COMPAT_APIS).sort()).toEqual(
      swagger.definitions
        .v1FoundryAICompatAPI!.enum!.filter(
          (a) => a !== "FOUNDRY_AI_COMPAT_API_UNSPECIFIED",
        )
        .sort(),
    );
    for (const api of Object.values(AI.COMPAT_APIS)) {
      expect(api.title).toMatch(/^OpenAI /);
      expect(api.hint).toMatch(/^\/[a-z/]+: /);
    }
  });

  it("is read from its provider message, with the WFE's defaults", () => {
    expect(
      AI.compatFields({
        openaiCompatible: {
          baseUrl: "http://host.docker.internal:1234/v1",
          api: "FOUNDRY_AI_COMPAT_API_RESPONSES",
          maxCallsInFlight: 1,
        },
      }),
    ).toEqual({
      baseUrl: "http://host.docker.internal:1234/v1",
      api: "FOUNDRY_AI_COMPAT_API_RESPONSES",
      calls: "1",
    });
    // Defaults left out of the WFE's JSON, an API this site doesn't know,
    // and no message at all.
    for (const p of [
      { openaiCompatible: {} },
      {
        openaiCompatible: {
          api: "FOUNDRY_AI_COMPAT_API_UNSPECIFIED" as const,
          maxCallsInFlight: 0,
        },
      },
      { openaiCompatible: { api: "SOMETHING_NEW" as wfe.FoundryAiCompatApi } },
      {},
      undefined,
    ])
      expect(AI.compatFields(p), JSON.stringify(p)).toEqual({
        baseUrl: "",
        api: "FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS",
        calls: "4",
      });
  });

  it("is sent normalized, its calls at once a number", () => {
    expect(
      AI.compatProvider({
        baseUrl: " https://LLM.example.com/v1/ ",
        api: "FOUNDRY_AI_COMPAT_API_RESPONSES",
        calls: "16",
      }),
    ).toEqual({
      openaiCompatible: {
        baseUrl: "https://llm.example.com/v1",
        api: "FOUNDRY_AI_COMPAT_API_RESPONSES",
        maxCallsInFlight: 16,
      },
    });
    // Not a URL yet: nothing to send to, so nothing to reuse a key for.
    expect(
      AI.compatProvider({ ...AI.compatFields(), baseUrl: "llm.example.com" })
        .openaiCompatible?.baseUrl,
    ).toBe("");
    // A round trip keeps what was saved.
    const saved = {
      openaiCompatible: {
        baseUrl: "http://10.0.0.5:8000/v1",
        api: "FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS" as const,
        maxCallsInFlight: 2,
      },
    };
    expect(AI.compatProvider(AI.compatFields(saved))).toEqual(saved);
  });

  it("takes 1 to 64 calls at once, as the WFE does", () => {
    for (const ok of ["1", "4", " 64 ", "08"])
      expect(AI.callsProblem(ok), ok).toBeUndefined();
    for (const bad of ["", "0", "65", "1000", "-1", "2.5", "four", "1e1"])
      expect(AI.callsProblem(bad), bad).toBe(
        "Use a whole number from 1 to 64.",
      );
    expect(AI.MAX_CALLS).toBe(64);
    expect(AI.DEFAULT_CALLS).toBe(4);
  });

  it("hints at /v1 only for a URL with no path", () => {
    expect(AI.versionless("http://host.docker.internal:1234")).toBe(true);
    expect(AI.versionless("http://host.docker.internal:1234/")).toBe(true);
    expect(AI.versionless("http://host.docker.internal:1234/v1")).toBe(false);
    expect(AI.versionless("https://openrouter.ai/api/v1")).toBe(false);
    // Not a URL: its problem says so instead.
    expect(AI.versionless("")).toBe(false);
    expect(AI.versionless("llm.example.com")).toBe(false);
  });

  it("takes its key optionally, and Helix prices none of its models", () => {
    expect(AI.PROVIDERS[COMPAT].keyPlaceholder).toBe(
      "Only if the server asks for one",
    );
    expect(AI.pricedModels(COMPAT)).toEqual([]);
    expect(AI.modelProblem(COMPAT, { name: "qwen3-32b", free: {} })).toBe(
      undefined,
    );
    expect(
      AI.modelProblem(COMPAT, {
        name: "Qwen/Qwen3-32B",
        rates: { inputUsdPerMtok: "0.1", outputUsdPerMtok: "0.3" },
      }),
    ).toBeUndefined();
    expect(AI.modelProblem(COMPAT, { name: "qwen3-32b", table: {} })).toBe(
      "Helix has no price for qwen3-32b; enter its prices.",
    );
    expect(AI.pickPriceText(COMPAT, { name: "m", free: {} })).toBe(
      "No cost counted",
    );
  });
});

describe("Ollama", () => {
  const OLLAMA = "FOUNDRY_AI_KIND_OLLAMA";
  const at = (baseUrl: string) => ({ ollama: { baseUrl } });
  const failed = (
    probeFailure: string,
    probeDetail = "",
    baseUrl: string = AI.OLLAMA_SERVERS.bundled.url,
    names = ROLE_MODELS("qwen3.5:4b"),
  ): wfe.FoundryAiConfig => ({
    id: "c",
    label: "Ollama",
    provider: at(baseUrl),
    probeFailure,
    probeDetail,
    models: Object.fromEntries(
      Object.entries(names).map(([r, name]) => [r, { name, free: {} }]),
    ),
  });

  it("runs on this machine only on its loopback or the computer Docker runs on", () => {
    for (const url of [
      "http://127.0.0.1:11434",
      "http://localhost:11434",
      "http://[::1]:11434",
      "http://host.docker.internal:11434/",
    ])
      expect(AI.disclosure(OLLAMA, at(url)), url).toBe(
        "Runs on this machine; nothing leaves it.",
      );
    expect(AI.disclosure(OLLAMA, at("http://10.0.0.7:11434"))).toBe(
      "Sent to the Ollama server at 10.0.0.7.",
    );
    // No server known (the install default's, which the WFE doesn't say).
    expect(AI.disclosure(OLLAMA)).toBe("Sent to your Ollama server.");
    expect(AI.disclosure(OLLAMA, at("not a url"))).toBe(
      "Sent to your Ollama server.",
    );
  });

  it("says a cloud model's calls go on to ollama.com, wherever the server is", () => {
    const models = (name: string) => [
      "qwen3.5:4b",
      name,
      "qwen3.5:4b",
      "qwen3.5:4b",
    ];
    for (const url of [
      AI.OLLAMA_SERVERS.bundled.url,
      AI.OLLAMA_SERVERS.host.url,
      "not a url",
    ])
      expect(
        AI.disclosure(OLLAMA, at(url), models("gpt-oss:120b-cloud")),
        url,
      ).toBe("Sent to ollama.com through your Ollama server.");
    expect(AI.disclosure(OLLAMA, {}, models("glm-5.3:cloud"))).toBe(
      "Sent to ollama.com through your Ollama server.",
    );
    expect(
      AI.disclosure(
        OLLAMA,
        at("http://gpu.lan:11434"),
        models("gpt-oss:120b-cloud"),
      ),
    ).toBe("Sent to ollama.com through the Ollama server at gpu.lan.");
    // Models it runs itself leave the line as it was.
    expect(
      AI.disclosure(OLLAMA, at(AI.OLLAMA_SERVERS.host.url), models("x:7b")),
    ).toBe("Runs on this machine; nothing leaves it.");
    // Other kinds have no cloud models of Ollama's.
    expect(
      AI.disclosure("FOUNDRY_AI_KIND_ANTHROPIC", {}, models("x:cloud")),
    ).toBe("Sent to Anthropic under your API key.");
  });

  it("names the bundled server and the computer's, and any other", () => {
    expect(AI.ollamaServer("http://127.0.0.1:11434/")).toBe("bundled");
    expect(AI.ollamaServer(" http://HOST.docker.internal:11434 ")).toBe("host");
    expect(AI.ollamaServer("http://127.0.0.1:11435")).toBe("other");
    expect(AI.ollamaServer("http://gpu.lan:11434")).toBe("other");
    expect(AI.ollamaServer("")).toBe("other");
    // Both are http on private addresses: a local install's only.
    for (const { url } of Object.values(AI.OLLAMA_SERVERS)) {
      expect(AI.endpointProblem(OLLAMA, url, true)).toBeUndefined();
      expect(AI.endpointProblem(OLLAMA, url, false)).toBe(
        "Use an https:// address.",
      );
    }
    expect(AI.endpointProblem(OLLAMA, "https://10.0.0.7:11434", false)).toBe(
      "The endpoint resolves to a private address.",
    );
  });

  it("takes a server's address, not its API's, as the WFE does", () => {
    const PATH =
      "Use the Ollama server’s address, like http://127.0.0.1:11434, without /api or /v1.";
    // The agent calls {base}/api/chat.
    for (const bad of [
      "http://gpu.lan:11434/v1",
      "http://gpu.lan:11434/v1/",
      "http://gpu.lan:11434/api",
      "http://gpu.lan:11434/api/chat",
      "http://gpu.lan:11434/ollama/api/",
      "http://gpu.lan:11434/proxy/v1",
    ])
      expect(AI.endpointProblem(OLLAMA, bad, true), bad).toBe(PATH);
    // A path in front of the API, as behind a proxy, is the server's.
    for (const ok of [
      "http://gpu.lan:11434",
      "http://gpu.lan:11434/",
      "http://gpu.lan/ollama",
      "http://gpu.lan/v1beta",
      "http://gpu.lan/apis",
    ])
      expect(AI.endpointProblem(OLLAMA, ok, true), ok).toBeUndefined();
    // Only Ollama's: a custom server's base URL includes its /v1.
    expect(
      AI.endpointProblem(
        "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE",
        "http://gpu.lan:11434/v1",
        true,
      ),
    ).toBeUndefined();
    // Hosted, the WFE's other rules too: a public https server only.
    expect(
      AI.endpointProblem(OLLAMA, "https://ollama.example.com", false),
    ).toBe(undefined);
    expect(AI.endpointProblem(OLLAMA, "http://ollama.example.com", false)).toBe(
      "Use an https:// address.",
    );
    expect(AI.endpointProblem(OLLAMA, "https://[fd00::1]", false)).toBe(
      "The endpoint resolves to a private address.",
    );
    expect(AI.endpointProblem(OLLAMA, "gpu.lan:11434", true)).toBe(
      "Use an http:// or https:// address with no user, query or fragment.",
    );
  });

  it("takes a context window the WFE takes, and says what each task needs", () => {
    const BAD =
      "Enter a whole number of tokens, 65,536 to 1,048,576: drafting needs 65,536.";
    for (const ok of ["65536", " 131072 ", "1048576"])
      expect(AI.contextProblem(ok), ok).toBeUndefined();
    // Under drafting's floor the WFE refuses the save (ai.MinContextTokens).
    for (const bad of [
      "",
      "0",
      "1",
      "16384",
      "32768",
      "65535",
      "-1",
      "1.5",
      "64k",
      "1048577",
      "99999999",
    ])
      expect(AI.contextProblem(bad), bad).toBe(BAD);
    expect(AI.OLLAMA_CONTEXT_NOTE).toBe(
      "The tokens every task’s model loads with: drafting needs 65,536, questions 32,768 and the others 16,384. A larger window takes more memory on the server.",
    );
    // The agent's probe floors (agent_server::probe::context_floor), and
    // the window a model loads with unless an owner sets one: drafting's.
    expect(AI.OLLAMA_CONTEXT).toEqual({
      default: 65536,
      draft: 65536,
      ask: 32768,
      other: 16384,
    });
  });

  it("knows the models it runs on ollama.com", () => {
    for (const name of ["gpt-oss:120b-cloud", "glm-5.3:cloud", " x:Cloud "])
      expect(AI.ollamaCloud(name), name).toBe(true);
    for (const name of ["qwen3.5:4b", "cloudy:7b", "x:cloud-7b", "cloud"])
      expect(AI.ollamaCloud(name), name).toBe(false);
  });

  it("counts no cost, and takes no key", () => {
    expect(
      AI.modelProblem(OLLAMA, { name: "qwen3.5:4b", free: {} }),
    ).toBeUndefined();
    expect(AI.modelProblem(OLLAMA, { name: "", free: {} })).toBe(
      "Enter the model’s name.",
    );
    expect(AI.modelProblem(OLLAMA, { name: "qwen 3", free: {} })).toBe(
      "Use a name with no spaces.",
    );
    expect(
      AI.canReuseKey(
        {
          id: "c",
          hasKey: true,
          provider: at(AI.OLLAMA_SERVERS.bundled.url),
        },
        {
          kind: OLLAMA,
          provider: at(AI.OLLAMA_SERVERS.bundled.url),
          models: ROLE_MODELS("m"),
        },
      ),
    ).toBe(false);
    expect(AI.runsOn(OLLAMA, true)).toBe("Ollama");
  });

  it("says how to fix a failed test on the server", () => {
    expect(AI.testFailureText(failed("not_found"))).toBe(
      "Ollama has no model qwen3.5:4b. Pull it on the server with ollama pull qwen3.5:4b, or choose one it has.",
    );
    expect(
      AI.testFailureText(
        failed("not_found", "none", AI.OLLAMA_SERVERS.bundled.url, {
          ...ROLE_MODELS("a"),
          ask: "b",
        }),
      ),
    ).toBe(
      "Ollama doesn’t have one of the models chosen. Pull it on the server with ollama pull, or choose one it has.",
    );
    expect(AI.testFailureText(failed("model_changed", "none"))).toBe(
      "The Ollama model changed during the test: it was pulled again or replaced. Test it again to use it as it is now.",
    );
    // Unreached, or a server that couldn't run the model (out of memory, or
    // its runner crashed): the agent reports both as unavailable.
    expect(AI.testFailureText(failed("unavailable"))).toBe(
      "Foundry couldn’t reach the bundled Ollama or run the model on it. Start it with docker compose --profile ollama up -d. If it is running, the model may need more memory than Docker has: try a smaller model or window, or give Docker more memory.",
    );
    expect(
      AI.testFailureText(failed("unavailable", "", AI.OLLAMA_SERVERS.host.url)),
    ).toBe(
      "Foundry couldn’t reach Ollama on this computer or run the model on it. Check that it is running; on Linux, start it with OLLAMA_HOST=172.17.0.1, Docker’s bridge address, so Docker can reach it and your network can’t. If it is running, the model may need more memory than this computer has: try a smaller model or window.",
    );
    expect(
      AI.testFailureText(failed("unavailable", "", "http://gpu.lan:11434")),
    ).toBe(
      "Foundry couldn’t reach the Ollama server at gpu.lan or run the model on it, after retries. Check that it is running and that this computer can reach it. If so, the model may need more memory than the server has: try a smaller model or window.",
    );
    // Never 0.0.0.0, which opens Ollama, which takes no key, to the network.
    for (const url of [
      AI.OLLAMA_SERVERS.bundled.url,
      AI.OLLAMA_SERVERS.host.url,
      "http://gpu.lan:11434",
    ])
      expect(AI.OLLAMA_FAILURE_COPY.unavailable!({ url })).not.toContain(
        "0.0.0.0",
      );
    expect(AI.testFailureText(failed("timeout"))).toBe(
      "The test ran out of time. On a CPU a model can take minutes to answer: try a smaller model, or a server with a GPU.",
    );
    // A check that says it all replaces the failure's line.
    expect(AI.testFailureText(failed("unsupported", "context_too_small"))).toBe(
      "The context window is too small for Foundry’s tasks: drafting needs 65,536 tokens, questions 32,768 and the others 16,384. Choose a model that takes one that large, or raise the window.",
    );
    expect(AI.testFailureText(failed("unsupported", "tools_rejected"))).toBe(
      "qwen3.5:4b can’t call tools on Ollama. Choose a model whose Ollama page lists tools, like qwen3.5:4b.",
    );
    // Others read as for any provider.
    expect(AI.testFailureText(failed("malformed", "json_invalid"))).toBe(
      "Ollama’s answer couldn’t be read. Its answer didn’t fit the JSON schema.",
    );
    // Not for another kind.
    expect(
      AI.testFailureText({
        ...failed("unsupported", "context_too_small"),
        label: "Claude",
        provider: { anthropic: {} },
      }),
    ).toBe(
      "qwen3.5:4b can’t do what Foundry needs (tool calls or structured answers). Its context window is too small for Foundry’s tasks.",
    );
  });

  it("has a line for each failure and check it words for itself", () => {
    const failures = constants("probe_result", "ModelFailure"),
      details = constants("probe_result", "ProbeDetail");
    for (const [f, copy] of Object.entries(AI.OLLAMA_FAILURE_COPY)) {
      expect(failures).toContain(f);
      for (const url of [
        AI.OLLAMA_SERVERS.bundled.url,
        AI.OLLAMA_SERVERS.host.url,
        "http://gpu.lan:11434",
      ])
        for (const model of ["m", undefined])
          expect(copy({ model, url })).toMatch(/^[A-Z].*\.$/);
    }
    for (const [d, copy] of Object.entries(AI.OLLAMA_DETAIL_COPY)) {
      expect(details).toContain(d);
      for (const model of ["m", undefined])
        expect(copy({ model })).toMatch(/^[A-Za-z].*\.$/);
    }
  });

  it("starts from the model a local install pulls", () => {
    const text = (path: string) =>
      readFileSync(new URL(path, import.meta.url), "utf8");
    const example = text("../../local/.env.example"),
      setup = text("../../local/setup.sh"),
      compose = text("../../local/compose.yaml");
    const value = (file: string, name: string) =>
      new RegExp(`^${name}=(.*)$`, "m").exec(file)?.[1];
    expect(value(example, "FOUNDRY_OLLAMA_MODEL")).toBe(AI.OLLAMA_MODEL);
    expect(value(setup, "OLLAMA_MODEL")).toBe(AI.OLLAMA_MODEL);
    expect(compose).toContain(`\${FOUNDRY_OLLAMA_MODEL:-${AI.OLLAMA_MODEL}}`);
    expect(value(example, "FOUNDRY_OLLAMA_DIGEST")).toMatch(
      /^sha256:[0-9a-f]{64}$/,
    );
    expect(value(setup, "OLLAMA_DIGEST")).toBe(
      value(example, "FOUNDRY_OLLAMA_DIGEST"),
    );
    expect(AI.OLLAMA_MODEL).toMatch(/^[a-z0-9.]+:[a-z0-9.]+$/);
    expect(AI.ollamaServer(AI.OLLAMA_SERVERS.bundled.url)).toBe("bundled");
  });
});

describe("canReuseKey", () => {
  const config = (
    provider: wfe.FoundryAiProvider,
    draft = "m",
    hasKey = true,
  ): wfe.FoundryAiConfig => ({
    id: "c",
    hasKey,
    provider,
    models: { draft: { name: draft } },
  });
  const next = (
    kind: wfe.FoundryAiProviderKind,
    provider: wfe.FoundryAiProvider,
    draft = "m",
  ): AI.Choice => ({ kind, provider, models: { ...ROLE_MODELS("m"), draft } });
  const azure = (endpoint: string) => ({ azureOpenai: { endpoint } });
  const compat = (baseUrl: string) => ({
    openaiCompatible: { baseUrl },
  });

  it.each([
    [
      "no setting",
      undefined,
      next("FOUNDRY_AI_KIND_ANTHROPIC", { anthropic: {} }),
      false,
    ],
    [
      "a setting whose key was dropped",
      config({ anthropic: {} }, "m", false),
      next("FOUNDRY_AI_KIND_ANTHROPIC", { anthropic: {} }),
      false,
    ],
    [
      "Claude to Claude, other models",
      config({ anthropic: {} }, "a"),
      next("FOUNDRY_AI_KIND_ANTHROPIC", { anthropic: {} }, "b"),
      true,
    ],
    [
      "Claude to OpenAI",
      config({ anthropic: {} }),
      next("FOUNDRY_AI_KIND_OPENAI", { openai: {} }),
      false,
    ],
    [
      "OpenAI to OpenAI",
      config({ openai: {} }),
      next("FOUNDRY_AI_KIND_OPENAI", { openai: {} }),
      true,
    ],
    [
      "Bedrock in another region",
      config({ bedrock: { region: "us-east-1" } }),
      next("FOUNDRY_AI_KIND_BEDROCK", { bedrock: { region: "eu-west-1" } }),
      true,
    ],
    [
      "Bedrock on its FIPS endpoint, other models",
      config({ bedrock: { region: "us-east-1" } }, "a"),
      next(
        "FOUNDRY_AI_KIND_BEDROCK",
        { bedrock: { region: "us-east-1", fips: true } },
        "b",
      ),
      true,
    ],
    [
      "Bedrock from Claude",
      config({ anthropic: {} }),
      next("FOUNDRY_AI_KIND_BEDROCK", { bedrock: { region: "us-east-1" } }),
      false,
    ],
    [
      "Claude from Bedrock",
      config({ bedrock: { region: "us-east-1" } }),
      next("FOUNDRY_AI_KIND_ANTHROPIC", { anthropic: {} }),
      false,
    ],
    [
      "Azure, same resource and deployment, written differently",
      config(azure("https://Acme.openai.azure.com/openai/v1"), "gpt"),
      next(
        "FOUNDRY_AI_KIND_AZURE_OPENAI",
        azure("https://acme.openai.azure.com/"),
        "gpt",
      ),
      true,
    ],
    [
      "Azure, another deployment",
      config(azure("https://acme.openai.azure.com"), "gpt"),
      next(
        "FOUNDRY_AI_KIND_AZURE_OPENAI",
        azure("https://acme.openai.azure.com"),
        "gpt-mini",
      ),
      false,
    ],
    [
      "Azure, another resource",
      config(azure("https://acme.openai.azure.com"), "gpt"),
      next(
        "FOUNDRY_AI_KIND_AZURE_OPENAI",
        azure("https://globex.openai.azure.com"),
        "gpt",
      ),
      false,
    ],
    [
      "Azure from a setting that isn't Azure",
      config({ openai: {} }, "gpt"),
      next(
        "FOUNDRY_AI_KIND_AZURE_OPENAI",
        azure("https://acme.openai.azure.com"),
        "gpt",
      ),
      false,
    ],
    [
      "a custom server, same URL but a trailing slash",
      config(compat("https://LLM.example.com/v1")),
      next(
        "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE",
        compat("https://llm.example.com/v1/"),
      ),
      true,
    ],
    [
      "a custom server at another URL",
      config(compat("https://llm.example.com/v1")),
      next(
        "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE",
        compat("https://llm.example.com/v2"),
      ),
      false,
    ],
    [
      "a custom server from a setting that isn't one",
      config({ anthropic: {} }),
      next(
        "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE",
        compat("https://llm.example.com/v1"),
      ),
      false,
    ],
    [
      "Azure, the new endpoint left empty",
      config(azure("https://acme.openai.azure.com"), "m"),
      next("FOUNDRY_AI_KIND_AZURE_OPENAI", { azureOpenai: {} }),
      false,
    ],
    [
      "a custom server, the new URL left empty",
      config(compat("https://llm.example.com/v1")),
      next("FOUNDRY_AI_KIND_OPENAI_COMPATIBLE", { openaiCompatible: {} }),
      false,
    ],
    [
      "Ollama, which takes no key",
      config({ ollama: { baseUrl: "http://127.0.0.1:11434" } }),
      next("FOUNDRY_AI_KIND_OLLAMA", {
        ollama: { baseUrl: "http://127.0.0.1:11434" },
      }),
      false,
    ],
  ])("%s: %s", (_, active, choice, reuse) => {
    expect(AI.canReuseKey(active, choice)).toBe(reuse);
  });
});

describe("endpointProblem", () => {
  const hosted = (kind: wfe.FoundryAiProviderKind, value: string) =>
    AI.endpointProblem(kind, value, false);
  const local = (kind: wfe.FoundryAiProviderKind, value: string) =>
    AI.endpointProblem(kind, value, true);

  it("asks Claude and OpenAI for none", () => {
    expect(hosted("FOUNDRY_AI_KIND_ANTHROPIC", "")).toBeUndefined();
    expect(hosted("FOUNDRY_AI_KIND_OPENAI", "anything")).toBeUndefined();
  });

  it("takes an Azure OpenAI resource's https endpoint, its path empty or /openai/v1", () => {
    for (const ok of [
      "https://acme.openai.azure.com",
      "https://acme.openai.azure.com/openai/v1/",
      "https://acme.cognitiveservices.azure.com",
      "https://acme.services.ai.azure.com",
    ])
      expect(hosted("FOUNDRY_AI_KIND_AZURE_OPENAI", ok), ok).toBeUndefined();
    for (const bad of [
      "http://acme.openai.azure.com",
      "https://openai.azure.com",
      "https://acme.example.com",
      "https://acme.openai.azure.com:8443",
      "https://user@acme.openai.azure.com",
      "https://acme.openai.azure.com/other",
      "https://acme.openai.azure.com?x=1",
      "not a url",
    ])
      expect(hosted("FOUNDRY_AI_KIND_AZURE_OPENAI", bad), bad).toBe(
        "Use the resource’s endpoint, like https://<resource>.openai.azure.com.",
      );
    expect(AI.azureEndpoint("https://Acme.openai.azure.com/")).toBe(
      "https://acme.openai.azure.com/openai/v1",
    );
  });

  it("takes an AWS region for Bedrock", () => {
    expect(hosted("FOUNDRY_AI_KIND_BEDROCK", "us-gov-west-1")).toBeUndefined();
    for (const bad of [
      "us-east",
      "US-EAST-1",
      "bedrock.us-east-1",
      "us-east-1a",
      "us-east-1.example.com",
      "",
    ])
      expect(hosted("FOUNDRY_AI_KIND_BEDROCK", bad), bad).toBe(
        "Use an AWS region, like us-east-1.",
      );
  });

  it("offers Bedrock's regions, FIPS only where AWS has a FIPS endpoint", () => {
    const regions = AI.BEDROCK_REGIONS.map((r) => r.region);
    expect(new Set(regions).size).toBe(regions.length);
    // Every region of AWS's table of where bedrock-runtime answers
    // (Regional availability by endpoints, read 2026-10-24).
    expect([...regions].sort()).toEqual(
      [
        ...["us-east-1", "us-east-2", "us-west-1", "us-west-2"],
        ...["ca-central-1", "ca-west-1", "mx-central-1"],
        ...["eu-central-1", "eu-central-2", "eu-north-1", "eu-south-1"],
        ...["eu-south-2", "eu-west-1", "eu-west-2", "eu-west-3"],
        ...["ap-east-2", "ap-northeast-1", "ap-northeast-2", "ap-northeast-3"],
        ...["ap-south-1", "ap-south-2", "ap-southeast-1", "ap-southeast-2"],
        ...["ap-southeast-3", "ap-southeast-4", "ap-southeast-5"],
        ...["ap-southeast-6", "ap-southeast-7"],
        ...["il-central-1", "me-central-1", "me-south-1", "af-south-1"],
        ...["sa-east-1", "us-gov-east-1", "us-gov-west-1"],
      ].sort(),
    );
    for (const r of AI.BEDROCK_REGIONS) {
      expect(hosted("FOUNDRY_AI_KIND_BEDROCK", r.region), r.region).toBe(
        undefined,
      );
      expect(r.name, r.region).not.toBe("");
      expect(AI.fipsOffered(r.region), r.region).toBe(r.fips);
    }
    expect(
      AI.BEDROCK_REGIONS.filter((r) => r.fips).map((r) => r.region),
    ).toEqual([
      "us-east-1",
      "us-east-2",
      "us-west-2",
      "ca-central-1",
      "us-gov-east-1",
      "us-gov-west-1",
    ]);
    // A region this page doesn't list may be a setting's; it has no FIPS here.
    expect(AI.fipsOffered("ap-southeast-9")).toBe(false);
    expect(AI.bedrockProvider("us-east-1", true)).toEqual({
      region: "us-east-1",
      fips: true,
    });
    expect(AI.bedrockProvider("us-east-1", false)).toEqual({
      region: "us-east-1",
    });
    expect(AI.bedrockProvider("eu-west-1", true)).toEqual({
      region: "eu-west-1",
    });
    // A region typed is sent trimmed, its FIPS endpoint by the trimmed name.
    expect(AI.bedrockProvider(" us-east-1 ", true)).toEqual({
      region: "us-east-1",
      fips: true,
    });
    expect(AI.bedrockProvider(" ap-southeast-9 ", false)).toEqual({
      region: "ap-southeast-9",
    });
  });

  it("takes a public https server hosted, and http or private addresses locally", () => {
    // Ollama's, which never has a /v1, under "Ollama".
    for (const kind of ["FOUNDRY_AI_KIND_OPENAI_COMPATIBLE"] as const) {
      expect(hosted(kind, "https://llm.example.com/v1")).toBeUndefined();
      expect(hosted(kind, "http://llm.example.com/v1")).toBe(
        "Use an https:// address.",
      );
      for (const host of [
        "127.0.0.1",
        "10.1.2.3",
        "172.16.0.1",
        "192.168.1.10",
        "169.254.169.254",
        "100.64.0.1",
        "0.0.0.0",
        "[::1]",
        "[fd00::1]",
        "[fe80::1]",
        "[::ffff:10.0.0.1]",
        "localhost",
        "api.localhost",
      ])
        expect(hosted(kind, `https://${host}/v1`), host).toBe(
          "The endpoint resolves to a private address.",
        );
      for (const ok of [
        "http://127.0.0.1:11434",
        "http://host.docker.internal:8000/v1",
        "https://10.0.0.5/v1",
      ])
        expect(local(kind, ok), ok).toBeUndefined();
      for (const bad of [
        "ftp://llm.example.com",
        "https://user:pass@llm.example.com",
        "https://llm.example.com/v1?x=1",
        "https://llm.example.com/v1?",
        "https://llm.example.com/v1#top",
        "llm.example.com",
      ])
        expect(local(kind, bad), bad).toBe(
          "Use an http:// or https:// address with no user, query or fragment.",
        );
    }
  });

  it("knows the private ranges' edges", () => {
    for (const pub of [
      "172.32.0.1",
      "100.128.0.1",
      "11.0.0.1",
      "8.8.8.8",
      "[2606:4700::1]",
      "999.1.1.1",
    ])
      expect(AI.privateHost(pub), pub).toBe(false);
    for (const priv of ["224.0.0.1", "255.255.255.255", "[ff02::1]", "[::]"])
      expect(AI.privateHost(priv), priv).toBe(true);
  });
});

describe("a failed test", () => {
  const config = (
    probeFailure: string,
    probeDetail = "",
    names = ROLE_MODELS("claude-x"),
  ): wfe.FoundryAiConfig => ({
    id: "c",
    label: "Claude",
    probeFailure,
    probeDetail,
    models: Object.fromEntries(
      Object.entries(names).map(([r, name]) => [r, { name }]),
    ),
  });

  it("says why, and what its check saw", () => {
    expect(AI.testFailureText(config("auth"))).toBe(
      "Claude refused the API key.",
    );
    expect(AI.testFailureText(config("auth", "none"))).toBe(
      "Claude refused the API key.",
    );
    expect(AI.testFailureText(config("not_found"))).toBe(
      "Claude has no model claude-x.",
    );
    expect(AI.testFailureText(config("unsupported", "tools_rejected"))).toBe(
      "claude-x can’t do what Foundry needs (tool calls or structured answers). It refused a request with Foundry’s tools.",
    );
  });

  it("names no one model when the tasks use several", () => {
    const mixed = { ...ROLE_MODELS("a"), ask: "b" };
    expect(AI.testFailureText(config("not_found", "", mixed))).toBe(
      "Claude doesn’t have one of the models chosen.",
    );
    expect(AI.testFailureText(config("permission", "", mixed))).toBe(
      "The API key may not use one of the models chosen.",
    );
    expect(AI.testFailureText(config("unsupported", "", mixed))).toBe(
      "One of the models chosen can’t do what Foundry needs (tool calls or structured answers).",
    );
    expect(AI.testFailureText(config("truncated", "", mixed))).toBe(
      "One of the models chosen ran out of room for its answer.",
    );
    expect(AI.testFailureText(config("context_too_long", "", mixed))).toBe(
      "The test was too long for one of the models chosen.",
    );
  });

  it("still fails for a failure or detail this site doesn't know", () => {
    expect(AI.testFailureText(config("new_failure", "new_detail"))).toBe(
      "The test failed.",
    );
    expect(AI.testFailureText({ id: "c" })).toBe("The test failed.");
  });
});

describe("the setting", () => {
  const house: wfe.FoundryAi = {
    default: {
      kind: "FOUNDRY_AI_KIND_ANTHROPIC",
      label: "Claude",
      models: { draft: { name: "claude-opus-5-5" } },
    },
  };
  const own: wfe.FoundryAiConfig = {
    id: "c1",
    kind: "FOUNDRY_AI_KIND_ANTHROPIC",
    provider: { anthropic: {} },
    label: "Claude",
    models: { draft: { name: "claude-sonnet-5-5" } },
  };

  it("is the workspace's own, the default, or none", () => {
    expect(AI.inForce({ ...house, active: own })).toEqual({
      source: "workspace",
      config: own,
    });
    expect(AI.inForce(house)).toEqual({
      source: "default",
      default: house.default,
    });
    expect(AI.inForce({})).toEqual({ source: "none" });
  });

  it("reads as a line for those who can't change it", () => {
    expect(AI.statusText(house, false)).toBe(
      "AI runs on the default (claude-opus-5-5). Only owners can change this.",
    );
    expect(AI.statusText(house, true)).toBe(
      "AI runs on Claude (claude-opus-5-5). Only owners can change this.",
    );
    expect(AI.statusText({ ...house, active: own }, false)).toBe(
      "AI runs on Claude (claude-sonnet-5-5). Only owners can change this.",
    );
    expect(AI.statusText({ default: {} }, true)).toBe(
      "AI runs on the model provider. Only owners can change this.",
    );
    expect(AI.statusText({}, false)).toBe(
      "No AI provider is set up. Only owners can choose one.",
    );
  });

  it("is paused while it is the workspace's key off Pro, hosted", () => {
    expect(AI.paused({ ...house, active: own, proRequired: true })).toBe(true);
    // On Pro, or on Helix's Claude, nothing waits.
    expect(AI.paused({ ...house, active: own })).toBe(false);
    expect(AI.paused({ ...house, proRequired: true })).toBe(false);
  });

  it("is read every 1.5 s only while a save is being tested", () => {
    const interval = (ai?: wfe.FoundryAi) =>
      AI.aiQuery("ws").refetchInterval({ state: { data: ai } });
    expect(
      interval({
        pending: { id: "c", probeState: "FOUNDRY_AI_PROBE_STATE_QUEUED" },
      }),
    ).toBe(1500);
    expect(
      interval({
        pending: { id: "c", probeState: "FOUNDRY_AI_PROBE_STATE_RUNNING" },
      }),
    ).toBe(1500);
    expect(
      interval({
        pending: { id: "c", probeState: "FOUNDRY_AI_PROBE_STATE_PASSED" },
      }),
    ).toBe(false);
    expect(interval(house)).toBe(false);
    expect(interval(undefined)).toBe(false);
    expect(AI.aiKey("ws")).toEqual(["ws", "foundry-ai"]);
  });

  it("says who runs a choice", () => {
    expect(AI.runsOn("default", false)).toBe("the default");
    expect(AI.runsOn("default", true)).toBe("the install default");
    expect(AI.runsOn("FOUNDRY_AI_KIND_ANTHROPIC", false)).toBe(
      "Claude, with your API key",
    );
    expect(AI.runsOn("FOUNDRY_AI_KIND_OLLAMA", true)).toBe("Ollama");
    // A custom server by its host, with or without a key.
    expect(
      AI.runsOn("FOUNDRY_AI_KIND_OPENAI_COMPATIBLE", false, {
        openaiCompatible: { baseUrl: "https://LLM.example.com/v1" },
      }),
    ).toBe("llm.example.com");
    expect(AI.runsOn("FOUNDRY_AI_KIND_OPENAI_COMPATIBLE", false)).toBe(
      "the custom endpoint",
    );
  });

  it("reads documents again only with another kind, endpoint or reading models", () => {
    const claude = { agent: "anthropic", extract: "h", verify: "s" } as const;
    expect(AI.documentsReread(claude, { ...claude })).toBe(false);
    expect(AI.documentsReread(claude, { ...claude, agent: "openai" })).toBe(
      true,
    );
    expect(AI.documentsReread(claude, { ...claude, extract: "o" })).toBe(true);
    expect(AI.documentsReread(claude, { ...claude, verify: "o" })).toBe(true);
    // A custom server's ids hash its base URL, an Azure resource's its
    // origin (the engine's Wire::json_id): another one reads them again,
    // the same one written another way doesn't.
    const server = (baseUrl: string) => ({
      agent: "openai_compatible" as const,
      endpoint: AI.idEndpoint({ openaiCompatible: { baseUrl } }),
      extract: "qwen3-32b",
      verify: "qwen3-32b",
    });
    expect(
      AI.documentsReread(
        server("https://llm.example.com/v1"),
        server("https://LLM.example.com/v1/"),
      ),
    ).toBe(false);
    for (const other of [
      "https://llm2.example.com/v1",
      "https://llm.example.com/api/v1",
      "https://llm.example.com:8443/v1",
    ])
      expect(
        AI.documentsReread(server("https://llm.example.com/v1"), server(other)),
      ).toBe(true);
    const resource = (endpoint: string) => ({
      agent: "azure_openai" as const,
      endpoint: AI.idEndpoint({ azureOpenai: { endpoint } }),
      extract: "prod-gpt",
      verify: "prod-gpt",
    });
    expect(
      AI.documentsReread(
        resource("https://acme.openai.azure.com"),
        resource("https://acme.openai.azure.com/openai/v1"),
      ),
    ).toBe(false);
    expect(
      AI.documentsReread(
        resource("https://acme.openai.azure.com"),
        resource("https://globex.openai.azure.com"),
      ),
    ).toBe(true);
    // The default's endpoint isn't known here: a custom default counts as
    // another server than the workspace's.
    expect(
      AI.documentsReread(
        { ...server("https://llm.example.com/v1"), endpoint: undefined },
        server("https://llm.example.com/v1"),
      ),
    ).toBe(true);
  });
});

describe("Helix's price table", () => {
  it("is the vendored catalog's rows, by kind", () => {
    const catalog = read("../../contract/ontology_agent/catalog.json") as {
      models: { kind: string; model: string }[];
    };
    expect(
      AI.pricedModels("FOUNDRY_AI_KIND_ANTHROPIC").map((r) => r.model),
    ).toEqual(
      catalog.models.filter((r) => r.kind === "anthropic").map((r) => r.model),
    );
    expect(AI.pricedModels("FOUNDRY_AI_KIND_OLLAMA")).toEqual([]);
    // OpenAI's rows price OpenAI's models and the models Azure deployments
    // run; the table has no Azure rows of its own.
    const openai = catalog.models
      .filter((r) => r.kind === "openai")
      .map((r) => r.model);
    expect(openai.length).toBeGreaterThan(0);
    expect(catalog.models.some((r) => r.kind === "azure_openai")).toBe(false);
    expect(
      AI.pricedModels("FOUNDRY_AI_KIND_OPENAI").map((r) => r.model),
    ).toEqual(openai);
    expect(
      AI.pricedModels("FOUNDRY_AI_KIND_AZURE_OPENAI").map((r) => r.model),
    ).toEqual(openai);
    const bedrock = catalog.models
      .filter((r) => r.kind === "bedrock")
      .map((r) => r.model);
    // Claude by its global profiles and, for the rest, its own ID.
    expect(bedrock).toEqual(
      expect.arrayContaining([
        "global.anthropic.claude-opus-5-5",
        "anthropic.claude-opus-5-5",
        "global.anthropic.claude-sonnet-5-5",
        "anthropic.claude-sonnet-5-5",
        "global.anthropic.claude-haiku-5-5",
        "anthropic.claude-haiku-5-5",
      ]),
    );
    expect(
      AI.pricedModels("FOUNDRY_AI_KIND_BEDROCK").map((r) => r.model),
    ).toEqual(bedrock);
  });

  it("prices a Bedrock cross-region inference profile as its model, as the WFE does", () => {
    const BEDROCK = "FOUNDRY_AI_KIND_BEDROCK",
      base = "anthropic.claude-sonnet-5-5";
    const row = AI.pricedRow(BEDROCK, base);
    expect(row?.model).toBe(base);
    for (const prefix of ["us.", "eu.", "apac.", "au.", "jp.", "ca."])
      expect(AI.pricedRow(BEDROCK, prefix + base), prefix).toBe(row);
    // A global profile has a row of its own, priced apart; one without
    // reads past its prefix too.
    expect(AI.pricedRow(BEDROCK, `global.${base}`)?.model).toBe(
      `global.${base}`,
    );
    expect(AI.pricedRow(BEDROCK, `global.${base}`)).not.toBe(row);
    expect(AI.pricedRow(BEDROCK, "global.amazon.nova-pro-v1:0")?.model).toBe(
      "amazon.nova-pro-v1:0",
    );
    // Only those geographies, once, and only on Bedrock.
    for (const id of [
      `xx.${base}`,
      `us-gov.${base}`,
      `us.us.${base}`,
      `${base}-v2`,
      `arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.${base}`,
    ])
      expect(AI.pricedRow(BEDROCK, id), id).toBeUndefined();
    expect(
      AI.pricedRow("FOUNDRY_AI_KIND_OPENAI", "us.gpt-6.1-sol"),
    ).toBeUndefined();
    expect(AI.pricedRow("FOUNDRY_AI_KIND_OPENAI", "gpt-6.1-sol")?.model).toBe(
      "gpt-6.1-sol",
    );
  });

  it("is read in dollars per million tokens", () => {
    expect(AI.priceText({ input: 4000, output: 20000 })).toBe(
      "$4 in, $20 out per million tokens",
    );
    expect(AI.priceText({ input: 100, output: 500 })).toBe(
      "$0.1 in, $0.5 out per million tokens",
    );
  });
});

describe("a task's model", () => {
  const OPENAI = "FOUNDRY_AI_KIND_OPENAI",
    AZURE = "FOUNDRY_AI_KIND_AZURE_OPENAI";
  const rates = (input: string, output: string, cached?: string) => ({
    inputUsdPerMtok: input,
    outputUsdPerMtok: output,
    ...(cached !== undefined && { cachedInputUsdPerMtok: cached }),
  });

  it("is read from a config with its price, the table's when left out", () => {
    expect(
      AI.rolePicks({
        draft: { name: "a" },
        ask: { name: "b", tableAs: "gpt-6.1-sol" },
        extract: { name: "c", rates: rates("1", "2") },
        verify: { name: "d", free: {} },
      }),
    ).toEqual({
      draft: { name: "a", table: {} },
      ask: { name: "b", tableAs: "gpt-6.1-sol" },
      extract: { name: "c", rates: rates("1", "2") },
      verify: { name: "d", free: {} },
    });
    expect(AI.rolePicks().draft).toEqual({ name: "", table: {} });
  });

  it("is the same only with the same name and price", () => {
    const a = { name: "a", rates: rates("1", "2") };
    expect(AI.samePick(a, { name: "a", rates: rates("1", "2") })).toBe(true);
    expect(AI.samePick(a, { name: "a", rates: rates("1", "3") })).toBe(false);
    // Rates left out are rates left empty.
    expect(
      AI.samePick(
        { name: "a", rates: {} },
        { name: "a", rates: rates("", "") },
      ),
    ).toBe(true);
    expect(AI.samePick(a, { name: "a", rates: rates("1", "2", "0.1") })).toBe(
      false,
    );
    expect(AI.samePick({ name: "a", free: {} }, { name: "a", table: {} })).toBe(
      false,
    );
    expect(
      AI.samePick({ name: "a", tableAs: "x" }, { name: "a", tableAs: "y" }),
    ).toBe(false);
    expect(
      AI.samePick({ name: "a", table: {} }, { name: "b", table: {} }),
    ).toBe(false);
  });

  it("takes rates as the WFE parses them", () => {
    const FORMAT =
      "Enter prices in dollars per million tokens, with at most three decimals.";
    for (const ok of [
      rates("0", "0.001"),
      rates("1.25", "10", ""),
      rates(" 2 ", "1234567.999", "0.075"),
    ])
      expect(AI.ratesProblem(ok), JSON.stringify(ok)).toBeUndefined();
    for (const bad of [
      rates("", "1"),
      rates("1", ""),
      rates("1.2345", "1"),
      rates("-1", "1"),
      rates("1e3", "1"),
      rates("12345678", "1"),
      rates("1", "1", "x"),
      {},
    ])
      expect(AI.ratesProblem(bad), JSON.stringify(bad)).toBe(FORMAT);
    expect(AI.ratesProblem(rates("1", "0.000"))).toBe(
      "The output price must be more than $0.",
    );
  });

  it("checks its name and price as the WFE does", () => {
    expect(AI.modelProblem(OPENAI, { name: "", table: {} })).toBe(
      "Enter the model’s name.",
    );
    expect(AI.modelProblem(AZURE, { name: " ", tableAs: "gpt-6.1-sol" })).toBe(
      "Enter the deployment’s name.",
    );
    expect(
      AI.modelProblem(OPENAI, {
        name: "x".repeat(201),
        rates: rates("1", "2"),
      }),
    ).toBe("Use at most 200 characters.");
    expect(
      AI.modelProblem(OPENAI, {
        name: "x".repeat(200),
        rates: rates("1", "2"),
      }),
    ).toBeUndefined();
    for (const name of ["two words", "tab\there"])
      expect(
        AI.modelProblem(OPENAI, { name, rates: rates("1", "2") }),
        name,
      ).toBe("Use a name with no spaces.");
    for (const name of ["sk-proj-abc", "ABSKabc"])
      expect(
        AI.modelProblem(OPENAI, { name, rates: rates("1", "2") }),
        name,
      ).toBe("That looks like an API key; enter it in the API key field.");
    expect(
      AI.modelProblem(OPENAI, { name: "gpt-6.1-sol", table: {} }),
    ).toBeUndefined();
    // Only the kind's own rows price it.
    expect(
      AI.modelProblem("FOUNDRY_AI_KIND_ANTHROPIC", {
        name: "gpt-6.1-sol",
        table: {},
      }),
    ).toBe("Helix has no price for gpt-6.1-sol; enter its prices.");
    expect(
      AI.modelProblem(AZURE, { name: "prod", tableAs: "gpt-6.1-sol" }),
    ).toBeUndefined();
    expect(AI.modelProblem(AZURE, { name: "prod", tableAs: "gpt-x" })).toBe(
      "Helix has no price for gpt-x; enter its prices.",
    );
    expect(
      AI.modelProblem(AZURE, { name: "prod", rates: rates("1", "0") }),
    ).toBe("The output price must be more than $0.");
    expect(AI.modelProblem(OPENAI, { name: "m", free: {} })).toBe(
      "Enter its prices.",
    );
    for (const kind of [
      "FOUNDRY_AI_KIND_OLLAMA",
      "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE",
    ] as const)
      expect(AI.modelProblem(kind, { name: "m", free: {} })).toBeUndefined();
  });

  it("is sent trimmed, without an empty cached-input price", () => {
    expect(AI.sentPick({ name: " a ", table: {} })).toEqual({
      name: "a",
      table: {},
    });
    expect(AI.sentPick({ name: "a", tableAs: "gpt-6.1-sol" })).toEqual({
      name: "a",
      tableAs: "gpt-6.1-sol",
    });
    expect(AI.sentPick({ name: "a", free: {} })).toEqual({
      name: "a",
      free: {},
    });
    expect(AI.sentPick({ name: "a", rates: rates(" 1 ", "2", " ") })).toEqual({
      name: "a",
      rates: rates("1", "2"),
    });
    expect(AI.sentPick({ name: "a", rates: rates("1", "2", "0.5") })).toEqual({
      name: "a",
      rates: rates("1", "2", "0.5"),
    });
    expect(AI.sentPick({ name: "a", rates: {} })).toEqual({
      name: "a",
      rates: rates("", ""),
    });
  });

  it("says its price, when Helix can read one", () => {
    expect(AI.pickPriceText(OPENAI, { name: "gpt-6.1-sol", table: {} })).toBe(
      "$2 in, $10 out per million tokens",
    );
    expect(AI.pickPriceText(OPENAI, { name: "gpt-x", table: {} })).toBe("");
    expect(
      AI.pickPriceText(AZURE, { name: "prod", tableAs: "gpt-6.1-sol" }),
    ).toBe("$2 in, $10 out per million tokens");
    expect(
      AI.pickPriceText(AZURE, { name: "d", rates: rates("1.50", "6") }),
    ).toBe("$1.5 in, $6 out per million tokens");
    expect(AI.pickPriceText(AZURE, { name: "d", rates: rates("", "6") })).toBe(
      "",
    );
    expect(
      AI.pickPriceText("FOUNDRY_AI_KIND_OLLAMA", { name: "m", free: {} }),
    ).toBe("No cost counted");
  });

  it("checks a Bedrock model or inference profile ID, and its price", () => {
    const BEDROCK = "FOUNDRY_AI_KIND_BEDROCK",
      base = "anthropic.claude-sonnet-5-5",
      arn =
        "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/a1b2c3d4e5f6";
    const row = AI.pricedRow(BEDROCK, base)!;
    expect(AI.modelProblem(BEDROCK, { name: " ", table: {} })).toBe(
      "Enter the model ID.",
    );
    for (const name of ["ABSKQmVkcm9jaw", "bedrock-api-key-YmVkcm9jaw"])
      expect(AI.modelProblem(BEDROCK, { name, table: {} }), name).toBe(
        "That looks like an API key; enter it in the API key field.",
      );
    for (const name of [base, `us.${base}`, `global.${base}`])
      expect(AI.modelProblem(BEDROCK, { name, table: {} }), name).toBe(
        undefined,
      );
    expect(
      AI.modelProblem(BEDROCK, { name: "us.anthropic.claude-next", table: {} }),
    ).toBe(
      "Helix has no price for us.anthropic.claude-next; enter its prices.",
    );
    // An application inference profile is priced as the model it runs.
    expect(AI.modelProblem(BEDROCK, { name: arn, tableAs: base })).toBe(
      undefined,
    );
    expect(AI.modelProblem(BEDROCK, { name: arn, tableAs: `eu.${base}` })).toBe(
      undefined,
    );
    expect(AI.modelProblem(BEDROCK, { name: arn, tableAs: "meta.x" })).toBe(
      "Helix has no price for meta.x; enter its prices.",
    );
    expect(
      AI.modelProblem(BEDROCK, {
        name: "amazon.nova-lite-v1:0",
        rates: rates("0.06", "0.24", "0.015"),
      }),
    ).toBeUndefined();
    expect(AI.modelProblem(BEDROCK, { name: "m", free: {} })).toBe(
      "Enter its prices.",
    );
    expect(AI.pickPriceText(BEDROCK, { name: `apac.${base}`, table: {} })).toBe(
      AI.priceText(row),
    );
    expect(
      AI.pickPriceText(BEDROCK, { name: arn, tableAs: `us.${base}` }),
    ).toBe(AI.priceText(row));
    expect(AI.pickPriceText(BEDROCK, { name: `xx.${base}`, table: {} })).toBe(
      "",
    );
  });

  it("has a key placeholder for every kind with a form", () => {
    expect(AI.PROVIDERS.FOUNDRY_AI_KIND_ANTHROPIC.keyPlaceholder).toBe(
      "sk-ant-…",
    );
    expect(AI.PROVIDERS.FOUNDRY_AI_KIND_OPENAI.keyPlaceholder).toBe("sk-…");
    expect(AI.PROVIDERS.FOUNDRY_AI_KIND_AZURE_OPENAI.keyPlaceholder).toBe(
      "The resource’s key 1 or key 2",
    );
    expect(AI.PROVIDERS.FOUNDRY_AI_KIND_BEDROCK.keyPlaceholder).toBe(
      "A long-term Bedrock API key, ABSK…",
    );
  });
});

describe("the module's examples", () => {
  it("hold", () => {
    expect(
      AI.testing({
        pending: { id: "c1", probeState: "FOUNDRY_AI_PROBE_STATE_RUNNING" },
      }),
    ).toBe(true);
    expect(AI.testing({})).toBe(false);
    expect(
      AI.inForce({
        default: { kind: "FOUNDRY_AI_KIND_ANTHROPIC", label: "Claude" },
      }).source,
    ).toBe("default");
    expect(
      AI.splitSettingsLink("An owner can enter it in Settings › AI models."),
    ).toEqual(["An owner can enter it in ", "Settings › AI models", "."]);
    expect(AI.splitSettingsLink("No link here")).toEqual(["No link here"]);
    expect(AI.roleModels({ draft: { name: "a" } })).toEqual({
      draft: "a",
      ask: "",
      extract: "",
      verify: "",
    });
    expect(AI.providerKind({ anthropic: {} })).toBe(
      "FOUNDRY_AI_KIND_ANTHROPIC",
    );
    expect(AI.providerKind({})).toBeUndefined();
    expect(AI.providerKind({ anthropic: {}, openai: {} })).toBeUndefined();
    expect(AI.providerKind(undefined)).toBeUndefined();
    expect(AI.baseUrl("https://LLM.example.com/v1/")).toBe(
      "https://llm.example.com/v1",
    );
    expect(
      AI.idEndpoint({
        openaiCompatible: { baseUrl: "https://LLM.example.com/v1/" },
      }),
    ).toBe("https://llm.example.com/v1");
    expect(
      AI.idEndpoint({
        azureOpenai: { endpoint: "https://acme.openai.azure.com" },
      }),
    ).toBe("https://acme.openai.azure.com/openai/v1");
    expect(AI.idEndpoint({ anthropic: {} })).toBeUndefined();
    expect(
      AI.documentsReread(
        {
          agent: "openai_compatible",
          endpoint: "https://a.example.com/v1",
          extract: "q",
          verify: "q",
        },
        {
          agent: "openai_compatible",
          endpoint: "https://b.example.com/v1",
          extract: "q",
          verify: "q",
        },
      ),
    ).toBe(true);
    expect(AI.azureEndpoint("https://acme.example.com")).toBeUndefined();
    expect(AI.azureEndpoint("https://Acme.openai.azure.com/")).toBe(
      "https://acme.openai.azure.com/openai/v1",
    );
    expect(
      AI.rolePicks({ draft: { name: "dep", tableAs: "gpt-6.1-sol" } }).draft,
    ).toEqual({ name: "dep", tableAs: "gpt-6.1-sol" });
    expect(AI.rolePicks().ask).toEqual({ name: "", table: {} });
    expect(
      AI.samePick({ name: "a", table: {} }, { name: "a", tableAs: "a" }),
    ).toBe(false);
    expect(
      AI.ratesProblem({ inputUsdPerMtok: "1.25", outputUsdPerMtok: "10" }),
    ).toBeUndefined();
    expect(AI.nameProblem("FOUNDRY_AI_KIND_OPENAI", "sk-proj-abc")).toBe(
      "That looks like an API key; enter it in the API key field.",
    );
    expect(AI.nameProblem("FOUNDRY_AI_KIND_OPENAI", " gpt-x ")).toBeUndefined();
    expect(
      AI.modelProblem("FOUNDRY_AI_KIND_OPENAI", {
        name: "gpt-other",
        table: {},
      }),
    ).toBe("Helix has no price for gpt-other; enter its prices.");
    expect(
      AI.sentPick({
        name: " dep ",
        rates: {
          inputUsdPerMtok: "1",
          outputUsdPerMtok: "2",
          cachedInputUsdPerMtok: "",
        },
      }),
    ).toEqual({
      name: "dep",
      rates: { inputUsdPerMtok: "1", outputUsdPerMtok: "2" },
    });
    expect(AI.pricedRow("FOUNDRY_AI_KIND_OPENAI", "gpt-6.1-sol")?.model).toBe(
      "gpt-6.1-sol",
    );
    expect(
      AI.pricedRow("FOUNDRY_AI_KIND_OPENAI", "us.gpt-6.1-sol"),
    ).toBeUndefined();
    expect(AI.fipsOffered("us-gov-west-1")).toBe(true);
    expect(AI.fipsOffered("eu-west-1")).toBe(false);
    expect(AI.bedrockProvider("us-east-1", true)).toEqual({
      region: "us-east-1",
      fips: true,
    });
    expect(AI.bedrockProvider("eu-west-1", true)).toEqual({
      region: "eu-west-1",
    });
    expect(AI.bedrockProvider(" ap-southeast-9 ", false)).toEqual({
      region: "ap-southeast-9",
    });
    expect(
      AI.disclosure(
        "FOUNDRY_AI_KIND_BEDROCK",
        { bedrock: { region: "eu-central-1" } },
        ["eu.anthropic.claude-sonnet-5-5"],
      ),
    ).toBe(
      "Sent to Amazon Bedrock in eu-central-1, under your AWS account. A cross-region inference profile may run requests in other regions within the EU.",
    );
    expect(AI.privateHost("169.254.169.254")).toBe(true);
    expect(AI.privateHost("[fd00::1]")).toBe(true);
    expect(AI.privateHost("api.example.com")).toBe(false);
    expect(
      AI.canReuseKey(
        { id: "c", hasKey: true, provider: { anthropic: {} } },
        {
          kind: "FOUNDRY_AI_KIND_ANTHROPIC",
          provider: { anthropic: {} },
          models: ROLE_MODELS("x"),
        },
      ),
    ).toBe(true);
    expect(
      AI.runsOn("FOUNDRY_AI_KIND_OPENAI_COMPATIBLE", true, {
        openaiCompatible: { baseUrl: "http://host.docker.internal:1234/v1" },
      }),
    ).toBe("host.docker.internal");
    expect(
      AI.compatFields({
        openaiCompatible: {
          baseUrl: "https://llm.example.com/v1",
          api: "FOUNDRY_AI_COMPAT_API_RESPONSES",
          maxCallsInFlight: 2,
        },
      }),
    ).toEqual({
      baseUrl: "https://llm.example.com/v1",
      api: "FOUNDRY_AI_COMPAT_API_RESPONSES",
      calls: "2",
    });
    expect(AI.compatFields().calls).toBe("4");
    expect(
      AI.compatProvider({
        baseUrl: "https://LLM.example.com/v1/",
        api: "FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS",
        calls: " 8 ",
      }),
    ).toEqual({
      openaiCompatible: {
        baseUrl: "https://llm.example.com/v1",
        api: "FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS",
        maxCallsInFlight: 8,
      },
    });
    expect(AI.callsProblem("4")).toBeUndefined();
    expect(AI.callsProblem("0")).toBe("Use a whole number from 1 to 64.");
    expect(AI.versionless("http://host.docker.internal:1234")).toBe(true);
    expect(AI.versionless("https://openrouter.ai/api/v1")).toBe(false);
  });
});

describe("the WFE calls", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("read the setting and save a choice with PUT", async () => {
    const sent: { url: string; method?: string; body?: string }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        if (url === "/papi/csrf")
          return Response.json({ token: "t", expiresInSeconds: 600 });
        sent.push({ url, method: init?.method, body: init?.body as string });
        return Response.json(
          url.endsWith("/ai") && init?.method === "PUT"
            ? { ai: { privateHostsAllowed: false } }
            : {},
        );
      }),
    );
    expect(await AI.aiQuery("ws 1").queryFn()).toEqual({});
    // An answer without its setting reads as an empty one.
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) =>
        url === "/papi/csrf"
          ? Response.json({ token: "t", expiresInSeconds: 600 })
          : Response.json({}),
      ),
    );
    expect(await AI.saveAi("ws", { useDefault: {} })).toEqual({});
    vi.unstubAllGlobals();
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        if (url === "/papi/csrf")
          return Response.json({ token: "t", expiresInSeconds: 600 });
        sent.push({ url, method: init?.method, body: init?.body as string });
        return Response.json(
          url.endsWith("/ai") && init?.method === "PUT"
            ? { ai: { privateHostsAllowed: false } }
            : {},
        );
      }),
    );
    expect(await AI.saveAi("ws 1", { useDefault: {} })).toEqual({
      privateHostsAllowed: false,
    });
    expect(sent).toEqual([
      {
        url: "/papi/v1/workspaces/ws%201/foundry/ai",
        method: "GET",
        body: undefined,
      },
      {
        url: "/papi/v1/workspaces/ws%201/foundry/ai",
        method: "PUT",
        body: JSON.stringify({ useDefault: {} }),
      },
    ]);
  });

  it("tells the refusal of model work without a provider from others", () => {
    const refusal = (reason: string) =>
      new ApiError("no", 400, 9, [
        { "@type": "type.googleapis.com/google.rpc.ErrorInfo", reason },
      ]);
    expect(AI.isAiNotConfigured(refusal("FOUNDRY_AI_NOT_CONFIGURED"))).toBe(
      true,
    );
    expect(AI.isAiNotConfigured(refusal("FOUNDRY_PRO_REQUIRED"))).toBe(false);
    expect(AI.isAiNotConfigured(new ApiError("no", 400, 9))).toBe(false);
    expect(AI.isAiNotConfigured(new ApiError("no", 400, 9, [null, 1]))).toBe(
      false,
    );
    expect(AI.isAiNotConfigured(new Error("no"))).toBe(false);
  });
});
