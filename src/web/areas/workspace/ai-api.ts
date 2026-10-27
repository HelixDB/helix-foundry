import catalog from "../../../../contract/ontology_agent/catalog.json";
import {
  FOUNDRY_AI_NOT_CONFIGURED,
  type AgentModelFailure,
  type AgentProbeDetail,
  type AgentProviderKind,
  type FoundryAi,
  type FoundryAiCompatApi,
  type FoundryAiConfig,
  type FoundryAiDefault,
  type FoundryAiModel,
  type FoundryAiModels,
  type FoundryAiPrice,
  type FoundryAiProvider,
  type FoundryAiProviderKind,
  type FoundryAiRates,
  type FoundryAiResponse,
  type FoundryAiRole,
  type UpdateFoundryAiBody,
} from "../../../shared/wfe";
import { ApiError, api } from "../../api";

// Settings › AI models' WFE calls and words: where the
// workspace's model calls go, an owner's save and its test, the cards'
// copy and what each sends where, and the failures a test or a model call
// can end in. A save is tested before it is used, so the query reads again
// every 1.5 s while one is queued or running. Nothing here renders.

const path = (ws: string) =>
  `/v1/workspaces/${encodeURIComponent(ws)}/foundry/ai`;

export const aiKey = (ws: string) => [ws, "foundry-ai"];
/** The setting; read again every 1.5 s while a save is being tested. */
export const aiQuery = (ws: string) => ({
  queryKey: aiKey(ws),
  queryFn: async () => (await api<FoundryAiResponse>(path(ws))).ai || {},
  refetchInterval: (q: { state: { data?: FoundryAi } }) =>
    testing(q.state.data) ? 1500 : false,
});

/** Saves an owner's choice: the default at once, or a provider to test. */
export const saveAi = async (ws: string, body: UpdateFoundryAiBody) =>
  (await api<FoundryAiResponse>(path(ws), body, "PUT")).ai || {};

/**
 * Whether a save is waiting for or in its test.
 *
 * @example
 * testing({ pending: { id: "c1", probeState: "FOUNDRY_AI_PROBE_STATE_RUNNING" } }); // true
 * testing({}); // false
 */
export const testing = (ai?: FoundryAi) =>
  ai?.pending?.probeState === "FOUNDRY_AI_PROBE_STATE_QUEUED" ||
  ai?.pending?.probeState === "FOUNDRY_AI_PROBE_STATE_RUNNING";

/** What the workspace's model calls run on now: one case per source. */
export type InForce =
  | { source: "workspace"; config: FoundryAiConfig }
  | { source: "default"; default: FoundryAiDefault }
  // A local install without a default, before an owner chose one.
  | { source: "none" };

/**
 * @example
 * inForce({ default: { kind: "FOUNDRY_AI_KIND_ANTHROPIC", label: "Claude" } }).source; // "default"
 * inForce({}).source; // "none"
 */
export function inForce(ai: FoundryAi): InForce {
  if (ai.active) return { source: "workspace", config: ai.active };
  if (ai.default) return { source: "default", default: ai.default };
  return { source: "none" };
}

/**
 * Whether the setting is the workspace's own key while, hosted, the
 * workspace is off Pro and Max: the WFE refuses its model work
 * (FOUNDRY_PRO_REQUIRED) until it is on a plan again or an owner chooses
 * Helix's Claude. Its work never moves to Helix's Claude unasked.
 *
 * @example
 * paused({ active: { id: "c" }, proRequired: true }); // true
 * paused({ proRequired: true }); // false
 */
export const paused = (ai: FoundryAi) => !!ai.active && !!ai.proRequired;

// ---------------------------------------------------------------- copy

/**
 * What a card says of a kind: its title, the name failures use, its mark.
 * Its description is its disclosure line.
 */
export type ProviderCopy = {
  title: string;
  // {P} in failure copy and toasts, as the WFE's labels say it.
  label: string;
  // A mark under public/providers, or none (a text card).
  logo?: string;
  // The agent's name for the kind: its catalog rows' `kind`.
  agent: AgentProviderKind;
  // What its API key field shows while empty.
  keyPlaceholder?: string;
};

/** Every kind the contract declares; the page shows those offered. */
export const PROVIDERS: Record<FoundryAiProviderKind, ProviderCopy> = {
  FOUNDRY_AI_KIND_ANTHROPIC: {
    title: "Claude",
    label: "Claude",
    logo: "/providers/claude.svg",
    agent: "anthropic",
    keyPlaceholder: "sk-ant-…",
  },
  FOUNDRY_AI_KIND_OPENAI: {
    title: "OpenAI",
    label: "OpenAI",
    logo: "/providers/openai.svg",
    agent: "openai",
    keyPlaceholder: "sk-…",
  },
  FOUNDRY_AI_KIND_AZURE_OPENAI: {
    title: "Azure OpenAI",
    label: "Azure OpenAI",
    agent: "azure_openai",
    keyPlaceholder: "The resource’s key 1 or key 2",
  },
  FOUNDRY_AI_KIND_BEDROCK: {
    title: "Amazon Bedrock",
    label: "Amazon Bedrock",
    agent: "bedrock",
    keyPlaceholder: "A long-term Bedrock API key, ABSK…",
  },
  FOUNDRY_AI_KIND_OLLAMA: {
    title: "Ollama",
    label: "Ollama",
    agent: "ollama",
  },
  FOUNDRY_AI_KIND_OPENAI_COMPATIBLE: {
    title: "Custom (OpenAI-compatible)",
    label: "the custom endpoint",
    agent: "openai_compatible",
    keyPlaceholder: "Only if the server asks for one",
  },
};

/** The tasks a workspace's models are chosen for, as the page names them. */
export const ROLES: Record<FoundryAiRole, string> = {
  draft: "Drafting",
  ask: "Questions",
  extract: "Reading documents",
  verify: "Checking facts",
};
export const ROLE_IDS = Object.keys(ROLES) as FoundryAiRole[];

/** The phrase failure copy names the AI settings by, which pages link. */
export const SETTINGS_LINK = "Settings › AI models";

/**
 * A text cut where it names the AI settings: the odd parts are the phrase,
 * which pages render as a link.
 *
 * @example
 * splitSettingsLink("An owner can enter it in Settings › AI models."); // ["An owner can enter it in ", "Settings › AI models", "."]
 */
export const splitSettingsLink = (text: string) =>
  text.split(/(Settings › AI models)/);

/** The card for the deployment's own provider, hosted: the default. */
export const HOUSE = {
  title: "Default",
  disclosure: "Runs on hosted providers through us, pay for what you use.",
};
/** What the setting says while paused(). */
export const PAUSED =
  "Paused: this workspace is off Foundry Pro, so its drafts, document reads and questions are refused until it is on Pro again or an owner chooses Default.";
/** What the owner's form says while nothing runs AI. */
export const NO_PROVIDER =
  "No AI provider is set up. Drafting ontologies, reading documents and answering questions need one.";
/** A local install's provider, from its .env (FOUNDRY_AI_*), read-only here. */
export const INSTALL_DEFAULT = "Install default";

/**
 * What the page calls the provider a choice runs on, in "run on …": a
 * custom server by its host, as the WFE labels it.
 *
 * @example
 * runsOn("default", false); // "the default"
 * runsOn("FOUNDRY_AI_KIND_ANTHROPIC", false); // "Claude, with your API key"
 * runsOn("FOUNDRY_AI_KIND_OPENAI_COMPATIBLE", true, { openaiCompatible: { baseUrl: "http://host.docker.internal:1234/v1" } }); // "host.docker.internal"
 */
export function runsOn(
  choice: "default" | FoundryAiProviderKind,
  local: boolean,
  provider: FoundryAiProvider = {},
): string {
  if (choice === "default")
    return local ? "the install default" : "the default";
  if (choice === "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE")
    return (
      hostOf(provider.openaiCompatible?.baseUrl) || PROVIDERS[choice].label
    );
  return choice === "FOUNDRY_AI_KIND_OLLAMA"
    ? PROVIDERS[choice].label
    : `${PROVIDERS[choice].label}, with your API key`;
}

const hostOf = (raw?: string) => {
  try {
    return new URL(raw || "").hostname;
  } catch {
    return "";
  }
};

// Where a Bedrock cross-region inference profile may run a request, by its
// ID's prefix: a geography's, within it (AWS's "Supported Regions and models
// for inference profiles"). A global profile runs in any commercial region.
const BEDROCK_GEOGRAPHIES: [prefix: string, within: string][] = [
  ["us.", "the US"],
  ["us-gov.", "AWS GovCloud (US)"],
  ["eu.", "the EU"],
  ["apac.", "Asia Pacific"],
  ["au.", "Australia"],
  ["jp.", "Japan"],
  ["ca.", "Canada"],
];

/**
 * Where a kind's card says the work goes, in a line. For Bedrock, the
 * models' IDs (a profile's ARN ends in its ID) say whether a cross-region
 * inference profile may run it outside the region.
 *
 * @example
 * disclosure("FOUNDRY_AI_KIND_ANTHROPIC"); // "Sent to Anthropic under your API key."
 * disclosure("FOUNDRY_AI_KIND_OLLAMA", { ollama: { baseUrl: "http://127.0.0.1:11434" } }); // "Runs on this machine; nothing leaves it."
 * disclosure("FOUNDRY_AI_KIND_OLLAMA", { ollama: { baseUrl: "http://127.0.0.1:11434" } }, ["gpt-oss:120b-cloud"]); // "Sent to ollama.com through your Ollama server."
 * disclosure("FOUNDRY_AI_KIND_BEDROCK", { bedrock: { region: "eu-central-1" } }, ["eu.anthropic.claude-sonnet-5-5"]);
 * // "Sent to Amazon Bedrock in eu-central-1, under your AWS account. A cross-region inference profile may run requests in other regions within the EU."
 */
export function disclosure(
  kind: FoundryAiProviderKind,
  provider: FoundryAiProvider = {},
  // The tasks' models: Ollama's may run on ollama.com, and a Bedrock
  // inference profile in other regions.
  models: readonly string[] = [],
): string {
  switch (kind) {
    case "FOUNDRY_AI_KIND_ANTHROPIC":
      return "Sent to Anthropic under your API key.";
    case "FOUNDRY_AI_KIND_OPENAI":
      return "Sent to OpenAI under your API key, with response storage off.";
    case "FOUNDRY_AI_KIND_AZURE_OPENAI": {
      const host = hostOf(provider.azureOpenai?.endpoint);
      return host
        ? `Sent to your Azure OpenAI resource at ${host}.`
        : "Sent to your Azure OpenAI resource.";
    }
    case "FOUNDRY_AI_KIND_BEDROCK": {
      const region = provider.bedrock?.region;
      const sent = !region
        ? "Sent to Amazon Bedrock, under your AWS account."
        : provider.bedrock?.fips
          ? `Sent to Amazon Bedrock’s FIPS endpoint in ${region}, under your AWS account.`
          : `Sent to Amazon Bedrock in ${region}, under your AWS account.`;
      const ids = models.map((m) => m.trim().split("/").at(-1)!);
      if (ids.some((id) => id.startsWith("global.")))
        return `${sent} A global inference profile may run requests in any AWS commercial region.`;
      const within = [
        ...new Set(
          ids.flatMap((id) =>
            BEDROCK_GEOGRAPHIES.filter(([p]) => id.startsWith(p)).map(
              ([, w]) => w,
            ),
          ),
        ),
      ];
      return within.length
        ? `${sent} A cross-region inference profile may run requests in other regions within ${within.join(" and ")}.`
        : sent;
    }
    case "FOUNDRY_AI_KIND_OLLAMA": {
      // The bundled server is on the agent's loopback; host.docker.internal
      // is the computer Docker runs on. A cloud model's calls go on to
      // ollama.com, wherever the server is (known by its name only: one
      // copied under another name isn't).
      const host = hostOf(provider.ollama?.baseUrl);
      const here = [
        "127.0.0.1",
        "localhost",
        "[::1]",
        "host.docker.internal",
      ].includes(host);
      if (models.some(ollamaCloud))
        return host && !here
          ? `Sent to ollama.com through the Ollama server at ${host}.`
          : "Sent to ollama.com through your Ollama server.";
      if (!host) return "Sent to your Ollama server.";
      return here
        ? "Runs on this machine; nothing leaves it."
        : `Sent to the Ollama server at ${host}.`;
    }
    case "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE": {
      const host = hostOf(provider.openaiCompatible?.baseUrl);
      return `Sent to ${host || "your endpoint"}. Helix can’t see where that endpoint sends it.`;
    }
  }
}

/**
 * Where the deployment's default sends the work, in one line, or "" for a
 * kind this site doesn't know. The default carries no endpoint, so a custom
 * server is named by the host the WFE labels it with, and an Ollama one
 * only by whether its models run on ollama.com.
 *
 * @example
 * defaultDisclosure({ kind: "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE", label: "host.docker.internal" }); // "Sent to host.docker.internal. Helix can’t see where that endpoint sends it."
 */
export function defaultDisclosure(def?: FoundryAiDefault): string {
  const kind = PROVIDER_KINDS.find((k) => k === def?.kind);
  if (!kind) return "";
  const models = Object.values(roleModels(def?.models));
  return kind === "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE" && def?.label
    ? disclosure(
        kind,
        { openaiCompatible: { baseUrl: `http://${def.label}` } },
        models,
      )
    : disclosure(kind, {}, models);
}

/**
 * A failed test, by the agent's ModelFailure: {P}, and the model tested
 * when every task uses the same one (undefined when they differ, as the
 * failure doesn't say which task's failed).
 */
export const FAILURE_COPY: Record<
  AgentModelFailure,
  (p: { label: string; model?: string }) => string
> = {
  refused: ({ label }) => `${label} declined the test.`,
  unavailable: ({ label }) =>
    `${label} didn’t answer the test, after retries. Try again in a while.`,
  auth: ({ label }) => `${label} refused the API key.`,
  permission: ({ model }) =>
    `The API key may not use ${model ?? "one of the models chosen"}.`,
  not_found: ({ label, model }) =>
    model
      ? `${label} has no model ${model}.`
      : `${label} doesn’t have one of the models chosen.`,
  rate_limited: ({ label }) =>
    `${label} kept limiting the test’s calls. Try again in a while.`,
  quota: ({ label }) => `The ${label} account is out of credit or quota.`,
  context_too_long: ({ model }) =>
    `The test was too long for ${model ?? "one of the models chosen"}.`,
  unsupported: ({ model }) =>
    `${model ?? "One of the models chosen"} can’t do what Foundry needs (tool calls or structured answers).`,
  truncated: ({ model }) =>
    `${model ?? "One of the models chosen"} ran out of room for its answer.`,
  malformed: ({ label }) => `${label}’s answer couldn’t be read.`,
  timeout: () => "The test ran out of time.",
  model_changed: () => "The Ollama model changed during the test.",
  private_host: () => "The endpoint resolves to a private address.",
};

/** What a failed test's check saw, after its failure; nothing for `none`. */
export const PROBE_DETAIL_COPY: Record<AgentProbeDetail, string> = {
  tools_rejected: "It refused a request with Foundry’s tools.",
  tool_call_missing: "It answered without calling the tool it was asked to.",
  schema_rejected: "It refused the answer’s JSON schema.",
  json_invalid: "Its answer didn’t fit the JSON schema.",
  text_missing: "It answered with no text.",
  context_too_small: "Its context window is too small for Foundry’s tasks.",
  unpriced: "Helix has no price for it, so nothing was sent.",
  none: "",
};

// ---------------------------------------------------------------- Ollama

/**
 * The Ollama servers a local install's card names: the one that runs with
 * Foundry (local/compose.yaml's ollama profile, in the agent's network
 * namespace, so on its loopback) and one on the computer Docker runs on.
 * Any other is typed in.
 */
export const OLLAMA_SERVERS = {
  bundled: { label: "Bundled", url: "http://127.0.0.1:11434" },
  host: {
    label: "On this computer",
    url: "http://host.docker.internal:11434",
  },
} as const;
export type OllamaServer = keyof typeof OLLAMA_SERVERS | "other";

/**
 * Which of the card's servers a base URL is.
 *
 * @example
 * ollamaServer("http://127.0.0.1:11434/"); // "bundled"
 * ollamaServer("http://gpu.lan:11434"); // "other"
 */
export function ollamaServer(url: string): OllamaServer {
  const u = baseUrl(url);
  return u === OLLAMA_SERVERS.bundled.url
    ? "bundled"
    : u === OLLAMA_SERVERS.host.url
      ? "host"
      : "other";
}

/**
 * How Ollama on a Linux computer listens for Docker and nothing else: on
 * Docker's bridge address, which host.docker.internal is there. Ollama's
 * API takes no key, so 0.0.0.0 would open it to the network.
 */
export const OLLAMA_LINUX_HOST =
  "on Linux, start it with OLLAMA_HOST=172.17.0.1, Docker’s bridge address, so Docker can reach it and your network can’t.";

/**
 * The model a local install's Ollama starts with: local/setup.sh pulls it
 * (FOUNDRY_OLLAMA_MODEL in local/.env.example), and the card suggests it.
 * A candidate: the default is to be the smallest model that finishes a real
 * draft under the local caps, measured on a GPU.
 */
export const OLLAMA_MODEL = "qwen3.5:4b";

/**
 * Whether Ollama runs a model on ollama.com rather than on the server: its
 * cloud models' tags end in "cloud". The bundled server runs none.
 *
 * @example
 * ollamaCloud("gpt-oss:120b-cloud"); // true
 * ollamaCloud("qwen3.5:4b"); // false
 */
export const ollamaCloud = (name: string) =>
  /:(?:.*-)?cloud$/i.test(name.trim());

/**
 * The context window Ollama loads models with unless an owner sets another
 * (ai.DefaultContextTokens), and the least each task's test takes (the
 * agent's probe floors, agent_server::probe::context_floor): drafting needs
 * room for its system prompt, tools and turns, a question for its tools and
 * the ontology. Every task runs in the one window, so the WFE takes none
 * under drafting's (ai.MinContextTokens).
 */
export const OLLAMA_CONTEXT = {
  default: 65536,
  draft: 65536,
  ask: 32768,
  other: 16384,
};
// The WFE's ceiling (ai.MaxContextTokens).
const MAX_CONTEXT_TOKENS = 1 << 20;
const tokens = (n: number) => n.toLocaleString("en-US");

/**
 * Why a context window can't be saved, as the WFE decides (a whole number
 * of tokens, 65,536 to 1,048,576), or undefined when it can.
 *
 * @example
 * contextProblem("65536"); // undefined
 * contextProblem("32768"); // "Enter a whole number of tokens, 65,536 to 1,048,576: drafting needs 65,536."
 */
export function contextProblem(raw: string): string | undefined {
  const t = raw.trim();
  return /^\d{1,7}$/.test(t) &&
    Number(t) >= OLLAMA_CONTEXT.draft &&
    Number(t) <= MAX_CONTEXT_TOKENS
    ? undefined
    : `Enter a whole number of tokens, ${tokens(OLLAMA_CONTEXT.draft)} to ${tokens(MAX_CONTEXT_TOKENS)}: drafting needs ${tokens(OLLAMA_CONTEXT.draft)}.`;
}

/** What the card says of a context window it takes. */
export const OLLAMA_CONTEXT_NOTE = `The tokens every task’s model loads with: drafting needs ${tokens(OLLAMA_CONTEXT.draft)}, questions ${tokens(OLLAMA_CONTEXT.ask)} and the others ${tokens(OLLAMA_CONTEXT.other)}. A larger window takes more memory on the server.`;

/**
 * A failed test on an Ollama server, whose fix is on that server: these
 * lines replace FAILURE_COPY's. `url` is the server's base URL.
 */
export const OLLAMA_FAILURE_COPY: Partial<
  Record<AgentModelFailure, (p: { model?: string; url: string }) => string>
> = {
  not_found: ({ model }) =>
    model
      ? `Ollama has no model ${model}. Pull it on the server with ollama pull ${model}, or choose one it has.`
      : "Ollama doesn’t have one of the models chosen. Pull it on the server with ollama pull, or choose one it has.",
  model_changed: () =>
    "The Ollama model changed during the test: it was pulled again or replaced. Test it again to use it as it is now.",
  // The server didn't answer, or answered that it couldn't run the model
  // (a 5xx: out of memory, or its runner crashed), after retries.
  unavailable: ({ url }) =>
    ({
      bundled:
        "Foundry couldn’t reach the bundled Ollama or run the model on it. Start it with docker compose --profile ollama up -d. If it is running, the model may need more memory than Docker has: try a smaller model or window, or give Docker more memory.",
      host: `Foundry couldn’t reach Ollama on this computer or run the model on it. Check that it is running; ${OLLAMA_LINUX_HOST} If it is running, the model may need more memory than this computer has: try a smaller model or window.`,
      other: `Foundry couldn’t reach the Ollama server at ${hostOf(url) || url} or run the model on it, after retries. Check that it is running and that this computer can reach it. If so, the model may need more memory than the server has: try a smaller model or window.`,
    })[ollamaServer(url)],
  timeout: () =>
    "The test ran out of time. On a CPU a model can take minutes to answer: try a smaller model, or a server with a GPU.",
};

/**
 * What an Ollama test's check saw, in full: these lines replace the whole
 * failure's text.
 */
export const OLLAMA_DETAIL_COPY: Partial<
  Record<AgentProbeDetail, (p: { model?: string }) => string>
> = {
  // The window is the smaller of the card's and the model's own.
  context_too_small: () =>
    `The context window is too small for Foundry’s tasks: drafting needs ${tokens(OLLAMA_CONTEXT.draft)} tokens, questions ${tokens(OLLAMA_CONTEXT.ask)} and the others ${tokens(OLLAMA_CONTEXT.other)}. Choose a model that takes one that large, or raise the window.`,
  tools_rejected: ({ model }) =>
    `${model ?? "One of the models chosen"} can’t call tools on Ollama. Choose a model whose Ollama page lists tools, like ${OLLAMA_MODEL}.`,
};

const known = <K extends string>(table: Record<K, unknown>, x?: string) =>
  x !== undefined && Object.hasOwn(table, x) ? (x as K) : undefined;

/**
 * A config's models by task, its unset ones empty.
 *
 * @example
 * roleModels({ draft: { name: "a" } }); // { draft: "a", ask: "", extract: "", verify: "" }
 */
export const roleModels = (m?: FoundryAiModels) =>
  Object.fromEntries(ROLE_IDS.map((r) => [r, m?.[r]?.name || ""])) as Record<
    FoundryAiRole,
    string
  >;

/**
 * Why a save's test failed, in a line: the failure, then what its check
 * saw. A failure this site doesn't know is still a failed test.
 *
 * @example
 * testFailureText({ id: "c", label: "Claude", probeFailure: "unsupported", probeDetail: "tools_rejected", models: { draft: { name: "claude-x" }, ask: { name: "claude-x" }, extract: { name: "claude-x" }, verify: { name: "claude-x" } } });
 * // "claude-x can’t do what Foundry needs (tool calls or structured answers). It refused a request with Foundry’s tools."
 */
export function testFailureText(c: FoundryAiConfig): string {
  const names = [...new Set(Object.values(roleModels(c.models)))];
  const model = names.length === 1 && names[0] ? names[0] : undefined,
    label = c.label || "The provider";
  const failure = known(FAILURE_COPY, c.probeFailure),
    detail = known(PROBE_DETAIL_COPY, c.probeDetail);
  // An Ollama server's own lines, where it has them.
  const ollama =
    providerKind(c.provider) === "FOUNDRY_AI_KIND_OLLAMA"
      ? { model, url: c.provider?.ollama?.baseUrl || "" }
      : undefined;
  const whole = ollama && detail && OLLAMA_DETAIL_COPY[detail]?.(ollama);
  if (whole) return whole;
  const why = failure
    ? ((ollama && OLLAMA_FAILURE_COPY[failure]?.(ollama)) ??
      FAILURE_COPY[failure]({ label, model }))
    : "The test failed.";
  const saw = detail ? PROBE_DETAIL_COPY[detail] : "";
  // A model's name starts a line as it is written.
  return saw ? `${why} ${saw}` : why;
}

/**
 * The setting in a line for anyone who isn't an owner.
 *
 * @example
 * statusText({ default: { label: "Claude", models: { draft: { name: "claude-opus-5-5" } } } }, false); // "AI runs on the default (claude-opus-5-5). Only owners can change this."
 * statusText({ default: { label: "Ollama" } }, true); // "AI runs on Ollama. Only owners can change this."
 * statusText({}, false); // "No AI provider is set up. Only owners can choose one."
 */
export function statusText(ai: FoundryAi, local: boolean): string {
  const now = inForce(ai);
  if (now.source === "none")
    return "No AI provider is set up. Only owners can choose one.";
  const { label, models } =
    now.source === "workspace" ? now.config : now.default;
  const draft = models?.draft?.name;
  const on =
    now.source === "default" && !local
      ? "the default"
      : label || "the model provider";
  return `AI runs on ${on}${draft ? ` (${draft})` : ""}. Only owners can change this.`;
}

// ---------------------------------------------------------------- prices

/** A row of the agent's price table (contract/ontology_agent/catalog.json). */
export type CatalogRow = (typeof catalog.models)[number];

// The kind whose rows price a kind's models: an Azure deployment is priced
// as the OpenAI model it runs, as the agent does (azure_openai -> openai).
const PRICED_BY: Partial<Record<FoundryAiProviderKind, AgentProviderKind>> = {
  FOUNDRY_AI_KIND_AZURE_OPENAI: "openai",
};

/**
 * The models Helix's price table has for a kind, in the table's order;
 * OpenAI's for Azure OpenAI.
 *
 * @example
 * pricedModels("FOUNDRY_AI_KIND_ANTHROPIC").map((r) => r.model)[0]; // "claude-opus-5-5"
 * pricedModels("FOUNDRY_AI_KIND_OLLAMA"); // []
 */
export const pricedModels = (kind: FoundryAiProviderKind): CatalogRow[] =>
  catalog.models.filter(
    (r) => r.kind === (PRICED_BY[kind] ?? PROVIDERS[kind].agent),
  );

// The prefixes of Bedrock's global and geographic inference profiles: a
// profile without a row of its own is priced as the model it runs, as the
// agent's catalog (llm::catalog::find) and the WFE read it.
const BEDROCK_PROFILES = [
  "global.",
  "us.",
  "eu.",
  "apac.",
  "au.",
  "jp.",
  "ca.",
];

/**
 * The row of Helix's price table that prices a kind's model, as the WFE and
 * the agent look it up: the model's own, or for a Bedrock inference profile
 * without a row of its own, its model's, the profile's prefix taken off.
 *
 * @example
 * pricedRow("FOUNDRY_AI_KIND_OPENAI", "gpt-6.1-sol")?.model; // "gpt-6.1-sol"
 * pricedRow("FOUNDRY_AI_KIND_OPENAI", "us.gpt-6.1-sol"); // undefined
 */
export function pricedRow(
  kind: FoundryAiProviderKind,
  model: string,
): CatalogRow | undefined {
  const rows = pricedModels(kind);
  const own = rows.find((r) => r.model === model);
  if (own || kind !== "FOUNDRY_AI_KIND_BEDROCK") return own;
  const prefix = BEDROCK_PROFILES.find((p) => model.startsWith(p));
  return prefix === undefined
    ? undefined
    : rows.find((r) => r.model === model.slice(prefix.length));
}

/**
 * A row's price per million tokens, in and out. The table is nano-dollars
 * per token, which is dollars per million tokens over 1,000.
 *
 * @example
 * priceText({ input: 4000, output: 20000 }); // "$4 in, $20 out per million tokens"
 */
export const priceText = (r: { input: number; output: number }) =>
  `$${r.input / 1000} in, $${r.output / 1000} out per million tokens`;

/**
 * A task's model as the form edits it and a save sends it: its name (an
 * Azure deployment's, for Azure OpenAI) and its price.
 */
export type ModelPick = { name: string } & FoundryAiPrice;

const pickOf = (m?: FoundryAiModel): ModelPick => {
  const name = m?.name || "";
  if (m?.tableAs) return { name, tableAs: m.tableAs };
  if (m?.rates) return { name, rates: { ...m.rates } };
  if (m?.free) return { name, free: {} };
  return { name, table: {} };
};

/**
 * A config's models by task, each with its price: a price the WFE left out
 * is the table's.
 *
 * @example
 * rolePicks({ draft: { name: "dep", tableAs: "gpt-6.1-sol" } }).draft; // { name: "dep", tableAs: "gpt-6.1-sol" }
 * rolePicks().ask; // { name: "", table: {} }
 */
export const rolePicks = (m?: FoundryAiModels) =>
  Object.fromEntries(ROLE_IDS.map((r) => [r, pickOf(m?.[r])])) as Record<
    FoundryAiRole,
    ModelPick
  >;

/**
 * Whether two picks name the same model at the same price.
 *
 * @example
 * samePick({ name: "a", table: {} }, { name: "a", table: {} }); // true
 * samePick({ name: "a", table: {} }, { name: "a", tableAs: "a" }); // false
 */
export const samePick = (a: ModelPick, b: ModelPick) =>
  JSON.stringify(canonical(a)) === JSON.stringify(canonical(b));
const canonical = (p: ModelPick) => [
  p.name,
  "table" in p,
  "free" in p,
  "tableAs" in p ? p.tableAs : null,
  "rates" in p
    ? [
        p.rates.inputUsdPerMtok || "",
        p.rates.outputUsdPerMtok || "",
        p.rates.cachedInputUsdPerMtok || "",
      ]
    : null,
];

// A price in dollars per million tokens, as the WFE parses it.
const RATE = /^\d{1,7}(\.\d{1,3})?$/;

/**
 * Why rates can't be saved, as the WFE reads them (dollars per million
 * tokens, at most three decimals; output above 0; cached input optional),
 * or undefined when they can.
 *
 * @example
 * ratesProblem({ inputUsdPerMtok: "1.25", outputUsdPerMtok: "10" }); // undefined
 * ratesProblem({ inputUsdPerMtok: "1", outputUsdPerMtok: "0" }); // "The output price must be more than $0."
 */
export function ratesProblem(r: FoundryAiRates): string | undefined {
  const cached = (r.cachedInputUsdPerMtok || "").trim();
  if (
    ![r.inputUsdPerMtok, r.outputUsdPerMtok].every((v) =>
      RATE.test((v || "").trim()),
    ) ||
    (cached && !RATE.test(cached))
  )
    return "Enter prices in dollars per million tokens, with at most three decimals.";
  return Number(r.outputUsdPerMtok) > 0
    ? undefined
    : "The output price must be more than $0.";
}

// Starts of providers' API keys that no model's name starts with: Bedrock's
// long-term ABSK and short-term bedrock-api-key- among them.
const KEY_PREFIXES = ["sk-", "ABSK", "bedrock-api-key-"];

/**
 * Why a model's name (an Azure deployment's, for Azure OpenAI) can't be
 * saved, as the WFE decides, or undefined when it can: 1 to 200 printable
 * characters with no spaces, and not an API key.
 *
 * @example
 * nameProblem("FOUNDRY_AI_KIND_OPENAI", "sk-proj-abc"); // "That looks like an API key; enter it in the API key field."
 */
export function nameProblem(
  kind: FoundryAiProviderKind,
  raw: string,
): string | undefined {
  const name = raw.trim();
  if (!name)
    return kind === "FOUNDRY_AI_KIND_AZURE_OPENAI"
      ? "Enter the deployment’s name."
      : kind === "FOUNDRY_AI_KIND_BEDROCK"
        ? "Enter the model ID."
        : "Enter the model’s name.";
  if ([...name].length > 200) return "Use at most 200 characters.";
  if (!/^[\x21-\x7e\u00a1-\uffff]+$/.test(name))
    return "Use a name with no spaces.";
  if (KEY_PREFIXES.some((p) => name.startsWith(p)))
    return "That looks like an API key; enter it in the API key field.";
  return undefined;
}

/**
 * Why a task's model can't be saved for a kind, as the WFE decides, or
 * undefined when it can: its name (nameProblem), then its price: Helix's
 * table (the model's own row, pricedRow's for a Bedrock inference profile,
 * or for an Azure deployment or a Bedrock profile the row of the model it
 * runs) or rates. Only Ollama and custom servers may count no cost.
 *
 * @example
 * modelProblem("FOUNDRY_AI_KIND_OPENAI", { name: "gpt-6.1-sol", table: {} }); // undefined
 * modelProblem("FOUNDRY_AI_KIND_OPENAI", { name: "gpt-other", table: {} }); // "Helix has no price for gpt-other; enter its prices."
 */
export function modelProblem(
  kind: FoundryAiProviderKind,
  pick: ModelPick,
): string | undefined {
  const named = nameProblem(kind, pick.name);
  if (named) return named;
  const name = pick.name.trim();
  const priced = (m: string) => !!pricedRow(kind, m);
  if ("table" in pick)
    return priced(name)
      ? undefined
      : `Helix has no price for ${name}; enter its prices.`;
  if ("tableAs" in pick)
    return priced(pick.tableAs)
      ? undefined
      : `Helix has no price for ${pick.tableAs}; enter its prices.`;
  if ("rates" in pick) return ratesProblem(pick.rates);
  return kind === "FOUNDRY_AI_KIND_OLLAMA" ||
    kind === "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE"
    ? undefined
    : "Enter its prices.";
}

/**
 * A pick as a save sends it: its name trimmed, and rates without an empty
 * cached-input price.
 *
 * @example
 * sentPick({ name: " dep ", rates: { inputUsdPerMtok: "1", outputUsdPerMtok: "2", cachedInputUsdPerMtok: "" } }); // { name: "dep", rates: { inputUsdPerMtok: "1", outputUsdPerMtok: "2" } }
 */
export function sentPick(p: ModelPick): ModelPick {
  const name = p.name.trim();
  if ("tableAs" in p) return { name, tableAs: p.tableAs };
  if ("rates" in p) {
    const cached = (p.rates.cachedInputUsdPerMtok || "").trim();
    return {
      name,
      rates: {
        inputUsdPerMtok: (p.rates.inputUsdPerMtok || "").trim(),
        outputUsdPerMtok: (p.rates.outputUsdPerMtok || "").trim(),
        ...(cached && { cachedInputUsdPerMtok: cached }),
      },
    };
  }
  return "free" in p ? { name, free: {} } : { name, table: {} };
}

/**
 * A pick's price in a line, or "" while it has none Helix can read.
 *
 * @example
 * pickPriceText("FOUNDRY_AI_KIND_OPENAI", { name: "gpt-6.1-sol", table: {} }); // "$2 in, $10 out per million tokens"
 * pickPriceText("FOUNDRY_AI_KIND_AZURE_OPENAI", { name: "dep", rates: { inputUsdPerMtok: "1.5", outputUsdPerMtok: "6" } }); // "$1.5 in, $6 out per million tokens"
 */
export function pickPriceText(
  kind: FoundryAiProviderKind,
  p: ModelPick,
): string {
  if ("free" in p) return "No cost counted";
  if ("rates" in p)
    return ratesProblem(p.rates)
      ? ""
      : `$${Number(p.rates.inputUsdPerMtok)} in, $${Number(p.rates.outputUsdPerMtok)} out per million tokens`;
  const model = "tableAs" in p ? p.tableAs : p.name.trim();
  const row = pricedRow(kind, model);
  return row ? priceText(row) : "";
}

// ---------------------------------------------------------------- rules

/** A provider's kind: the one field of FoundryAiProvider that is set. */
const PROVIDER_FIELD: Record<FoundryAiProviderKind, keyof FoundryAiProvider> = {
  FOUNDRY_AI_KIND_ANTHROPIC: "anthropic",
  FOUNDRY_AI_KIND_OPENAI: "openai",
  FOUNDRY_AI_KIND_AZURE_OPENAI: "azureOpenai",
  FOUNDRY_AI_KIND_BEDROCK: "bedrock",
  FOUNDRY_AI_KIND_OLLAMA: "ollama",
  FOUNDRY_AI_KIND_OPENAI_COMPATIBLE: "openaiCompatible",
};
export const PROVIDER_KINDS = Object.keys(
  PROVIDER_FIELD,
) as FoundryAiProviderKind[];

/**
 * The kind a provider message is, or undefined unless exactly one field is
 * set.
 *
 * @example
 * providerKind({ anthropic: {} }); // "FOUNDRY_AI_KIND_ANTHROPIC"
 * providerKind({}); // undefined
 */
export function providerKind(
  p?: FoundryAiProvider,
): FoundryAiProviderKind | undefined {
  const set = PROVIDER_KINDS.filter((k) => p?.[PROVIDER_FIELD[k]]);
  return set.length === 1 ? set[0] : undefined;
}

const AZURE_HOSTS = [
  ".openai.azure.com",
  ".cognitiveservices.azure.com",
  ".services.ai.azure.com",
];

/**
 * An Azure OpenAI endpoint as the WFE stores it (https://<host>/openai/v1),
 * or undefined when it isn't one.
 *
 * @example
 * azureEndpoint("https://Acme.openai.azure.com/"); // "https://acme.openai.azure.com/openai/v1"
 * azureEndpoint("https://acme.example.com"); // undefined
 */
export function azureEndpoint(raw: string): string | undefined {
  const u = parse(raw);
  if (
    !u ||
    u.protocol !== "https:" ||
    u.port ||
    u.username ||
    u.password ||
    u.search ||
    u.hash ||
    !AZURE_HOSTS.some(
      (s) => u.hostname.endsWith(s) && u.hostname.length > s.length,
    )
  )
    return undefined;
  const p = u.pathname.replace(/\/+$/, "");
  return p === "" || p === "/openai/v1"
    ? `https://${u.hostname}/openai/v1`
    : undefined;
}

/**
 * A model server's base URL as the WFE stores it (lowercase host, no
 * trailing slash), or undefined when it isn't an http(s) URL with a host
 * and no user, query or fragment.
 *
 * @example
 * baseUrl("https://LLM.example.com/v1/"); // "https://llm.example.com/v1"
 */
export function baseUrl(raw: string): string | undefined {
  const u = parse(raw);
  if (
    !u ||
    !["http:", "https:"].includes(u.protocol) ||
    !u.hostname ||
    u.username ||
    u.password ||
    u.search ||
    u.hash ||
    raw.includes("?") ||
    raw.includes("#")
  )
    return undefined;
  return `${u.protocol}//${u.host}${u.pathname.replace(/\/+$/, "")}`;
}

function parse(raw: string): URL | undefined {
  try {
    return new URL(raw.trim());
  } catch {
    return undefined;
  }
}

const v4 = (host: string) => {
  const m = host.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  return m && m.slice(1).every((x) => Number(x) <= 255)
    ? m.slice(1).map(Number)
    : undefined;
};

/**
 * Whether a host is a private address the WFE refuses hosted: loopback,
 * RFC 1918, link-local (the cloud metadata address included), CGNAT, 0/8,
 * broadcast, multicast, IPv6 loopback, unique-local, link-local and
 * v4-mapped addresses, and localhost. Names are resolved by the WFE; only
 * literals (and localhost) are known here.
 *
 * @example
 * privateHost("169.254.169.254"); // true
 * privateHost("[fd00::1]"); // true
 * privateHost("api.example.com"); // false
 */
export function privateHost(host: string): boolean {
  const h = host.toLowerCase().replace(/^\[|\]$/g, "");
  if (h === "localhost" || h.endsWith(".localhost")) return true;
  const a = v4(h);
  if (a) {
    const [x, y] = a as [number, number, number, number];
    return (
      x === 0 ||
      x === 10 ||
      x === 127 ||
      (x === 100 && y >= 64 && y < 128) ||
      (x === 169 && y === 254) ||
      (x === 172 && y >= 16 && y < 32) ||
      (x === 192 && y === 168) ||
      x >= 224
    );
  }
  if (!h.includes(":")) return false;
  return (
    h === "::" ||
    h === "::1" ||
    h.startsWith("::ffff:") ||
    /^f[cd]/.test(h) ||
    /^fe[89ab]/.test(h) ||
    h.startsWith("ff")
  );
}

const REGION = /^[a-z]{2}(-[a-z]+)+-\d+$/;

/**
 * Why a kind's endpoint can't be saved here, or undefined when it can.
 * `value` is the Azure endpoint, the Bedrock region, or the Ollama or
 * custom server's base URL; Claude and OpenAI take none. Hosted, a server
 * must be https on a public host; a local install allows http and private
 * addresses. An Ollama server's is its own address, not its API's.
 *
 * @example
 * endpointProblem("FOUNDRY_AI_KIND_OLLAMA", "http://gpu.lan:11434/v1", true); // "Use the Ollama server’s address, like http://127.0.0.1:11434, without /api or /v1."
 * endpointProblem("FOUNDRY_AI_KIND_OPENAI_COMPATIBLE", "http://llm.example.com/v1", false); // "Use an https:// address."
 * endpointProblem("FOUNDRY_AI_KIND_OPENAI_COMPATIBLE", "http://host.docker.internal:8000/v1", true); // undefined
 * endpointProblem("FOUNDRY_AI_KIND_BEDROCK", "us-east-1", false); // undefined
 */
export function endpointProblem(
  kind: FoundryAiProviderKind,
  value: string,
  local: boolean,
): string | undefined {
  switch (kind) {
    case "FOUNDRY_AI_KIND_ANTHROPIC":
    case "FOUNDRY_AI_KIND_OPENAI":
      return undefined;
    case "FOUNDRY_AI_KIND_AZURE_OPENAI":
      return azureEndpoint(value)
        ? undefined
        : "Use the resource’s endpoint, like https://<resource>.openai.azure.com.";
    case "FOUNDRY_AI_KIND_BEDROCK":
      return REGION.test(value.trim())
        ? undefined
        : "Use an AWS region, like us-east-1.";
    case "FOUNDRY_AI_KIND_OLLAMA":
    case "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE": {
      const url = baseUrl(value);
      if (!url)
        return "Use an http:// or https:// address with no user, query or fragment.";
      // The agent calls {base}/api/chat: Ollama's OpenAI-compatible /v1, or
      // a path of its API, is a mistake every call would 404 on.
      if (
        kind === "FOUNDRY_AI_KIND_OLLAMA" &&
        /\/(api|v1)$|\/api\//.test(new URL(url).pathname)
      )
        return "Use the Ollama server’s address, like http://127.0.0.1:11434, without /api or /v1.";
      if (local) return undefined;
      if (url.startsWith("http:")) return "Use an https:// address.";
      return privateHost(new URL(url).hostname)
        ? "The endpoint resolves to a private address."
        : undefined;
    }
  }
}

/** The APIs a custom server may speak: each FoundryAiCompatApi but unset. */
export type CompatApi = Exclude<
  FoundryAiCompatApi,
  "FOUNDRY_AI_COMPAT_API_UNSPECIFIED"
>;
/** Each API's name and what it is, as the API format field offers them. */
export const COMPAT_APIS: Record<CompatApi, { title: string; hint: string }> = {
  FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS: {
    title: "OpenAI Chat Completions",
    hint: "/chat/completions: vLLM, LM Studio, Ollama and most gateways",
  },
  FOUNDRY_AI_COMPAT_API_RESPONSES: {
    title: "OpenAI Responses",
    hint: "/responses: servers with OpenAI’s Responses API",
  },
};

/**
 * Calls sent to a custom server at once where its owner sets none, and the
 * most the WFE takes.
 */
export const DEFAULT_CALLS = 4,
  MAX_CALLS = 64;

/** A custom server's fields as the form edits them: calls at once as typed. */
export type CompatFields = { baseUrl: string; api: CompatApi; calls: string };

/**
 * A custom server's fields from its provider message, or the defaults
 * (Chat Completions, 4 calls at once) where it sets none.
 *
 * @example
 * compatFields({ openaiCompatible: { baseUrl: "https://llm.example.com/v1", api: "FOUNDRY_AI_COMPAT_API_RESPONSES", maxCallsInFlight: 2 } }); // { baseUrl: "https://llm.example.com/v1", api: "FOUNDRY_AI_COMPAT_API_RESPONSES", calls: "2" }
 * compatFields().calls; // "4"
 */
export function compatFields(p?: FoundryAiProvider): CompatFields {
  const c = p?.openaiCompatible;
  return {
    baseUrl: c?.baseUrl || "",
    api: known(COMPAT_APIS, c?.api) ?? "FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS",
    calls: String(Number(c?.maxCallsInFlight) || DEFAULT_CALLS),
  };
}

/**
 * The provider message a save sends for a custom server's fields: its base
 * URL normalized (empty until it is one), and calls at once as a number.
 *
 * @example
 * compatProvider({ baseUrl: "https://LLM.example.com/v1/", api: "FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS", calls: " 8 " }); // { openaiCompatible: { baseUrl: "https://llm.example.com/v1", api: "FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS", maxCallsInFlight: 8 } }
 */
export const compatProvider = (f: CompatFields): FoundryAiProvider => ({
  openaiCompatible: {
    baseUrl: baseUrl(f.baseUrl) ?? "",
    api: f.api,
    maxCallsInFlight: Number(f.calls.trim()),
  },
});

/**
 * Why calls at once can't be saved, as the WFE decides (a whole number from
 * 1 to MAX_CALLS), or undefined when they can.
 *
 * @example
 * callsProblem("4"); // undefined
 * callsProblem("0"); // "Use a whole number from 1 to 64."
 */
export const callsProblem = (raw: string) =>
  /^\d{1,3}$/.test(raw.trim()) && Number(raw) >= 1 && Number(raw) <= MAX_CALLS
    ? undefined
    : `Use a whole number from 1 to ${MAX_CALLS}.`;

/**
 * Whether a server's base URL has no path, where most servers' API is under
 * /v1 (a hint only: some gateways answer elsewhere).
 *
 * @example
 * versionless("http://host.docker.internal:1234"); // true
 * versionless("https://openrouter.ai/api/v1"); // false
 */
export const versionless = (raw: string) => {
  const url = baseUrl(raw);
  return !!url && new URL(url).pathname === "/";
};

/** A region Amazon Bedrock's runtime answers in. */
export type BedrockRegion = { region: string; name: string; fips: boolean };

/**
 * The regions Amazon Bedrock's runtime answers in, from AWS's table of them
 * (Bedrock's "Regional availability by endpoints"), and whether each has a
 * FIPS endpoint too (bedrock-runtime-fips.<region>.amazonaws.com). The WFE
 * takes any AWS region; the page offers these, and another typed for a
 * region AWS adds.
 */
export const BEDROCK_REGIONS: readonly BedrockRegion[] = [
  { region: "us-east-1", name: "US East (N. Virginia)", fips: true },
  { region: "us-east-2", name: "US East (Ohio)", fips: true },
  { region: "us-west-1", name: "US West (N. California)", fips: false },
  { region: "us-west-2", name: "US West (Oregon)", fips: true },
  { region: "ca-central-1", name: "Canada (Central)", fips: true },
  { region: "ca-west-1", name: "Canada West (Calgary)", fips: false },
  { region: "mx-central-1", name: "Mexico (Central)", fips: false },
  { region: "sa-east-1", name: "South America (São Paulo)", fips: false },
  { region: "eu-central-1", name: "Europe (Frankfurt)", fips: false },
  { region: "eu-central-2", name: "Europe (Zurich)", fips: false },
  { region: "eu-north-1", name: "Europe (Stockholm)", fips: false },
  { region: "eu-south-1", name: "Europe (Milan)", fips: false },
  { region: "eu-south-2", name: "Europe (Spain)", fips: false },
  { region: "eu-west-1", name: "Europe (Ireland)", fips: false },
  { region: "eu-west-2", name: "Europe (London)", fips: false },
  { region: "eu-west-3", name: "Europe (Paris)", fips: false },
  { region: "ap-east-2", name: "Asia Pacific (Taipei)", fips: false },
  { region: "ap-northeast-1", name: "Asia Pacific (Tokyo)", fips: false },
  { region: "ap-northeast-2", name: "Asia Pacific (Seoul)", fips: false },
  { region: "ap-northeast-3", name: "Asia Pacific (Osaka)", fips: false },
  { region: "ap-south-1", name: "Asia Pacific (Mumbai)", fips: false },
  { region: "ap-south-2", name: "Asia Pacific (Hyderabad)", fips: false },
  { region: "ap-southeast-1", name: "Asia Pacific (Singapore)", fips: false },
  { region: "ap-southeast-2", name: "Asia Pacific (Sydney)", fips: false },
  { region: "ap-southeast-3", name: "Asia Pacific (Jakarta)", fips: false },
  { region: "ap-southeast-4", name: "Asia Pacific (Melbourne)", fips: false },
  { region: "ap-southeast-5", name: "Asia Pacific (Malaysia)", fips: false },
  {
    region: "ap-southeast-6",
    name: "Asia Pacific (New Zealand)",
    fips: false,
  },
  { region: "ap-southeast-7", name: "Asia Pacific (Thailand)", fips: false },
  { region: "il-central-1", name: "Israel (Tel Aviv)", fips: false },
  { region: "me-central-1", name: "Middle East (UAE)", fips: false },
  { region: "me-south-1", name: "Middle East (Bahrain)", fips: false },
  { region: "af-south-1", name: "Africa (Cape Town)", fips: false },
  { region: "us-gov-east-1", name: "AWS GovCloud (US-East)", fips: true },
  { region: "us-gov-west-1", name: "AWS GovCloud (US-West)", fips: true },
];

/**
 * Whether Bedrock has a FIPS endpoint in a region.
 *
 * @example
 * fipsOffered("us-gov-west-1"); // true
 * fipsOffered("eu-west-1"); // false
 */
export const fipsOffered = (region: string) =>
  BEDROCK_REGIONS.some((r) => r.region === region && r.fips);

/**
 * A Bedrock provider as a save sends it: its region as typed, trimmed, and
 * its FIPS endpoint only where chosen and the region has one.
 *
 * @example
 * bedrockProvider("us-east-1", true); // { region: "us-east-1", fips: true }
 * bedrockProvider("eu-west-1", true); // { region: "eu-west-1" }
 * bedrockProvider(" ap-southeast-9 ", false); // { region: "ap-southeast-9" }
 */
export const bedrockProvider = (typed: string, fips: boolean) => {
  const region = typed.trim();
  return { region, ...(fips && fipsOffered(region) && { fips: true }) };
};

/** A provider as the page edits it: its kind, its message and its models. */
export type Choice = {
  kind: FoundryAiProviderKind;
  provider: FoundryAiProvider;
  models: Record<FoundryAiRole, string>;
};

/**
 * Whether saving `next` with the key left blank keeps the setting's key,
 * as the WFE decides (ai.CanReuseKey): only while the setting still holds
 * one, and only where it belongs to the endpoint the calls go to. A Claude,
 * OpenAI or Bedrock key belongs to the account, whatever the models; an
 * Azure key to its resource and draft deployment; a custom server's to its
 * URL; Ollama takes none.
 *
 * @example
 * canReuseKey({ id: "c", hasKey: true, provider: { anthropic: {} } }, { kind: "FOUNDRY_AI_KIND_ANTHROPIC", provider: { anthropic: {} }, models: { draft: "x", ask: "x", extract: "x", verify: "x" } }); // true
 */
export function canReuseKey(
  active: FoundryAiConfig | undefined,
  next: Choice,
): boolean {
  if (!active?.hasKey) return false;
  const was = active.provider;
  switch (next.kind) {
    case "FOUNDRY_AI_KIND_ANTHROPIC":
    case "FOUNDRY_AI_KIND_OPENAI":
    case "FOUNDRY_AI_KIND_BEDROCK":
      return providerKind(was) === next.kind;
    case "FOUNDRY_AI_KIND_AZURE_OPENAI": {
      const before = azureEndpoint(was?.azureOpenai?.endpoint || ""),
        after = azureEndpoint(next.provider.azureOpenai?.endpoint || "");
      return (
        !!before &&
        before === after &&
        active.models?.draft?.name === next.models.draft
      );
    }
    case "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE": {
      const before = baseUrl(was?.openaiCompatible?.baseUrl || ""),
        after = baseUrl(next.provider.openaiCompatible?.baseUrl || "");
      return !!before && before === after;
    }
    case "FOUNDRY_AI_KIND_OLLAMA":
      return false;
  }
}

/**
 * What a switch reads documents with: the kind, the endpoint its models'
 * ids name ({@link idEndpoint}) and its reading models.
 */
export type Readers = {
  agent: AgentProviderKind;
  endpoint?: string;
  extract: string;
  verify: string;
};

/**
 * The endpoint a provider's model ids name, as the WFE stores it: an Azure
 * resource's (its origin is in azure:{h8}/…) or a custom server's base URL
 * (compatible:{h8}/…); undefined for a kind whose ids name only the model,
 * and for the default, whose endpoint the WFE doesn't say. So a custom or
 * Azure default counts as another endpoint than any the workspace names.
 *
 * @example
 * idEndpoint({ openaiCompatible: { baseUrl: "https://LLM.example.com/v1/" } }); // "https://llm.example.com/v1"
 * idEndpoint({ azureOpenai: { endpoint: "https://acme.openai.azure.com" } }); // "https://acme.openai.azure.com/openai/v1"
 * idEndpoint({ anthropic: {} }); // undefined
 */
export const idEndpoint = (p?: FoundryAiProvider) =>
  azureEndpoint(p?.azureOpenai?.endpoint || "") ??
  baseUrl(p?.openaiCompatible?.baseUrl || "");

/**
 * Whether documents are read again after a switch: the agent keeps what
 * it read by the reading models' ids, which Claude keeps whoever's key it
 * is, so only another kind, another Azure resource or custom server, or
 * other reading models read them again.
 *
 * @example
 * documentsReread({ agent: "anthropic", extract: "h", verify: "s" }, { agent: "anthropic", extract: "h", verify: "s" }); // false
 * documentsReread({ agent: "anthropic", extract: "h", verify: "s" }, { agent: "openai", extract: "h", verify: "s" }); // true
 * documentsReread({ agent: "openai_compatible", endpoint: "https://a.example.com/v1", extract: "q", verify: "q" }, { agent: "openai_compatible", endpoint: "https://b.example.com/v1", extract: "q", verify: "q" }); // true
 */
export const documentsReread = (from: Readers, to: Readers) =>
  from.agent !== to.agent ||
  from.endpoint !== to.endpoint ||
  from.extract !== to.extract ||
  from.verify !== to.verify;

/**
 * True when the WFE refused model work because no AI provider is set up
 * (google.rpc.ErrorInfo reason FOUNDRY_AI_NOT_CONFIGURED), which only a
 * local install without a default does. Show <AiGate/> then.
 */
export function isAiNotConfigured(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    (error.details || []).some(
      (d) =>
        !!d &&
        typeof d === "object" &&
        (d as { reason?: unknown }).reason === FOUNDRY_AI_NOT_CONFIGURED,
    )
  );
}
