import type {
  AgentModelFailure,
  AgentProbeDetail,
  AgentProviderKind,
  FoundryAi,
  FoundryAiConfig,
  FoundryAiKey,
  FoundryAiModel,
  FoundryAiModels,
  FoundryAiPrice,
  FoundryAiProbeState,
  FoundryAiProvider,
  FoundryAiProviderKind,
  FoundryAiRates,
  FoundryAiRole,
  FoundryOllamaModel,
  FoundryOllamaServer,
  UpdateFoundryAiBody,
} from "../../src/shared/wfe";
import {
  DEFAULT_CALLS,
  MAX_CALLS,
  OLLAMA_CONTEXT,
  OLLAMA_MODEL,
  PROVIDERS,
  ROLE_IDS,
  azureEndpoint,
  baseUrl,
  canReuseKey,
  contextProblem,
  endpointProblem,
  ollamaServer,
  pricedRow,
  privateHost,
  providerKind,
} from "../../src/web/areas/workspace/ai-api";
import { fail, id, ok, type MockReply, type MockRoute } from "../http";
import {
  LOCAL_TOKEN,
  config,
  db,
  displayName,
  isReply,
  membership,
  onReset,
} from "../state";
import { isFoundryPro, proRequired } from "./billing";

// Settings › AI models as the backend serves it:
// a workspace's saved configs, each tested before it is used, and its
// activations, the newest of which is the setting (one with no config is
// the deployment's default). Hosted, the default is Helix's Claude and
// bringing a key needs Foundry Pro or Max, as does running on it (model
// work on a key is refused once the workspace leaves Pro: aiRefusal); a
// local API's default is the install's (MOCK_AI_DEFAULT, "anthropic" in
// localMockEnv), or none, found on its machine: the setting says how
// (`detected`, in the backend's words), and DetectFoundryAIDefault looks
// again. A local API also lists an Ollama server's models by its base URL,
// in the backend's order and words (ollamaModels); hosted, both routes are
// FAILED_PRECONDITION.
//
// A save runs the WFE's checks in its order: the kind is offered (and,
// hosted, the plan has own keys), every model has a name that isn't a key
// and a price the kind takes (rates in dollars with at most three decimals,
// output above 0; another model's row only for an Azure deployment or a
// Bedrock model; no cost only for Ollama and custom servers), the endpoint
// passes the deployment's URL rules (an Azure OpenAI resource's https
// endpoint, normalized to .../openai/v1; an AWS region for Bedrock, FIPS
// kept only when on), a key is given or kept (canReuseKey) or not needed,
// every model priced by table (its own row, a Bedrock cross-region
// profile's model's, or the row it is priced as) is in the agent's price
// table, and the workspace has saves left this hour. It is then queued; older queued or running saves
// are superseded. A custom OpenAI-compatible server is refused in the WFE's
// words: its API format unnamed or over 64 calls at once (0 is 4) before
// anything else, as the WFE reads the message; hosted, an http base URL or
// a private address, where its DNS is the mock's: a name ending in
// ".internal" resolves to a private address and one ending in ".invalid"
// to nothing. Its key is optional, and kept only for the same base URL.
// Its test ends `aiProbeMs` (1500) after it was saved, half of that queued
// and half running, read when the setting is read:
// - a custom server whose host ends in ".invalid" (only a local API saves
//   one): unavailable, as a server that isn't running;
// - a key containing "bad": auth;
// - a model (an Azure deployment) containing "notools": unsupported, its
//   check tools_rejected;
// - a model (an Azure deployment) containing "missing": not_found;
// - `aiProbeOutcome: "timeout"`: timeout;
// - Ollama, with a model containing "short" (one whose own window is under
//   a task's: the save refuses a window under drafting's 65536 tokens):
//   unsupported, its check context_too_small; a model containing "changed":
//   model_changed (it was pulled again during the test);
// - anything else passes, and becomes the setting if it is still the
//   workspace's newest save and nothing was chosen since.
// A config holds its key (the WFE's sealed column) only while it is the
// setting or the pending save; answers never carry it, and only an owner's
// carry its hint, which shows at most its kind and last four characters.
//
// Knobs, through POST /__mock/config: `aiProbeMs`, `aiProbeOutcome`,
// `aiDefault: false` (a local API without a default: model work is
// refused with FOUNDRY_AI_NOT_CONFIGURED), `aiDefaultKind: "ollama"` (a
// local API whose default is Ollama's qwen3.5:4b, from the bundled server),
// `aiDefaultNamed: true` (a local API whose .env names its default,
// FOUNDRY_AI_PROVIDER: no `detected`, and DetectFoundryAIDefault refused),
// `aiDetect: "anthropic" | "ollama" | "none"` (what DetectFoundryAIDefault
// finds from then on, as `aiDefault` and `aiDefaultKind` set it: a key
// given, or a model pulled, since the install started; unset, it finds
// what it found), `ollamaBundled: false` (the bundled Ollama isn't
// running) and `aiKinds`, the kinds offered (Claude, OpenAI, Azure OpenAI,
// custom OpenAI-compatible servers and Amazon Bedrock, and in a local API
// Ollama, which is never offered hosted).
// Ollama servers, by base URL: the bundled one has qwen3.5:4b, which
// Foundry runs, then embeddinggemma and qwen2.5:0.5b, which it can't; the
// one on this computer also qwen3.5:9b (first, the most capable), gemma3:4b
// and a cloud model, gpt-oss:120b-cloud; another has the bundled one's
// models, but one whose host ends in ".invalid" doesn't answer, one whose
// host contains "notollama" isn't Ollama, and one whose host contains
// "old" is Ollama 0.5.7, too old to say which models call tools.

type Knobs = {
  aiProbeMs?: number;
  aiProbeOutcome?: "timeout";
  aiDefault?: boolean;
  aiDefaultKind?: "ollama";
  aiDefaultNamed?: boolean;
  aiDetect?: "anthropic" | "ollama" | "none";
  ollamaBundled?: boolean;
  aiKinds?: FoundryAiProviderKind[];
};
const knobs = config as typeof config & Knobs;

type Probe = "queued" | "running" | "passed" | "failed" | "superseded";
type Config = {
  id: string;
  workspaceId: string;
  kind: FoundryAiProviderKind;
  provider: FoundryAiProvider;
  models: Record<FoundryAiRole, { name: string } & FoundryAiPrice>;
  // The sealed column: null once the config is neither the setting nor
  // pending.
  key: string | null;
  hint: string;
  probe: Probe;
  failure?: AgentModelFailure;
  detail?: AgentProbeDetail;
  createdBy: string;
  createdAt: number;
};
type Activation = {
  workspaceId: string;
  configId: string | null;
  userId: string;
  at: number;
};

const state = {
  configs: [] as Config[],
  activations: [] as Activation[],
  // When a local API last looked for its default: as it started, then on
  // each DetectFoundryAIDefault.
  detectedAt: Date.now(),
};
const PER_HOUR = 10;
const MAX_KEY = 512;
const COMPAT = "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE";

onReset(() => {
  state.configs.length = 0;
  state.activations.length = 0;
  state.detectedAt = Date.now();
  delete knobs.aiProbeMs;
  delete knobs.aiProbeOutcome;
  delete knobs.aiDefault;
  delete knobs.aiDefaultKind;
  delete knobs.aiDefaultNamed;
  delete knobs.aiDetect;
  delete knobs.ollamaBundled;
  delete knobs.aiKinds;
});

const SUGGESTED: Partial<
  Record<FoundryAiProviderKind, Record<FoundryAiRole, string>>
> = {
  FOUNDRY_AI_KIND_ANTHROPIC: {
    draft: "claude-opus-5-5",
    ask: "claude-sonnet-5-5",
    extract: "claude-haiku-5-5",
    verify: "claude-sonnet-5-5",
  },
  FOUNDRY_AI_KIND_OPENAI: {
    draft: "gpt-6.1-sol",
    ask: "gpt-6.1-sol",
    extract: "gpt-6-luna",
    verify: "gpt-6.1-sol",
  },
  // Claude through Bedrock's global cross-region inference profiles.
  FOUNDRY_AI_KIND_BEDROCK: {
    draft: "global.anthropic.claude-opus-5-5",
    ask: "global.anthropic.claude-sonnet-5-5",
    extract: "global.anthropic.claude-haiku-5-5",
    verify: "global.anthropic.claude-sonnet-5-5",
  },
};
const KEY: Record<FoundryAiProviderKind, FoundryAiKey> = {
  FOUNDRY_AI_KIND_ANTHROPIC: "FOUNDRY_AI_KEY_REQUIRED",
  FOUNDRY_AI_KIND_OPENAI: "FOUNDRY_AI_KEY_REQUIRED",
  FOUNDRY_AI_KIND_AZURE_OPENAI: "FOUNDRY_AI_KEY_REQUIRED",
  FOUNDRY_AI_KIND_BEDROCK: "FOUNDRY_AI_KEY_REQUIRED",
  FOUNDRY_AI_KIND_OLLAMA: "FOUNDRY_AI_KEY_NONE",
  FOUNDRY_AI_KIND_OPENAI_COMPATIBLE: "FOUNDRY_AI_KEY_OPTIONAL",
};

const table = (names: Record<FoundryAiRole, string>): FoundryAiModels =>
  Object.fromEntries(
    ROLE_IDS.map((r) => [r, { name: names[r], table: {} }]),
  ) as FoundryAiModels;
const free = (names: Record<FoundryAiRole, string>): FoundryAiModels =>
  Object.fromEntries(
    ROLE_IDS.map((r) => [r, { name: names[r], free: {} }]),
  ) as FoundryAiModels;

/** The deployment's provider: Helix's Claude hosted, the install's locally. */
function deploymentDefault():
  | { kind: FoundryAiProviderKind; models: Record<FoundryAiRole, string> }
  | undefined {
  if (LOCAL_TOKEN && !(knobs.aiDefault ?? !!process.env.MOCK_AI_DEFAULT))
    return undefined;
  // Its Ollama's model for every task.
  if (LOCAL_TOKEN && knobs.aiDefaultKind === "ollama")
    return {
      kind: "FOUNDRY_AI_KIND_OLLAMA",
      models: Object.fromEntries(
        ROLE_IDS.map((r) => [r, OLLAMA_MODEL]),
      ) as Record<FoundryAiRole, string>,
    };
  return {
    kind: "FOUNDRY_AI_KIND_ANTHROPIC" as const,
    models: SUGGESTED.FOUNDRY_AI_KIND_ANTHROPIC!,
  };
}

/**
 * How a local API that names no default found the one it has, or that it
 * found none, in the backend's words; undefined hosted or where its .env
 * names it.
 */
function detected(): FoundryAi["detected"] {
  if (!LOCAL_TOKEN || knobs.aiDefaultNamed) return undefined;
  const kind = deploymentDefault()?.kind;
  return {
    summary:
      kind === "FOUNDRY_AI_KIND_OLLAMA"
        ? `Using ${OLLAMA_MODEL} from the bundled Ollama: the most capable model it has that calls tools.`
        : kind
          ? "Using Claude: Foundry was started with an ANTHROPIC_API_KEY."
          : "No AI provider was found. Foundry looks for an ANTHROPIC_API_KEY or OPENAI_API_KEY where it was started, a model that calls tools in Ollama, and a model LM Studio serves on this computer.",
    detectedAt: new Date(state.detectedAt).toISOString(),
  };
}

// Ollama servers' models, in the backend's order: those Foundry runs, the
// most capable first, then the rest by name. Sizes are int64 strings.
const QWEN_4B: FoundryOllamaModel = {
  name: "qwen3.5:4b",
  sizeBytes: "3389725347",
  parameterSize: "4.7B",
};
const CANT_RUN: FoundryOllamaModel[] = [
  {
    name: "embeddinggemma",
    sizeBytes: "621875047",
    parameterSize: "307.58M",
    unusable: "Not a chat model.",
  },
  {
    name: "qwen2.5:0.5b",
    sizeBytes: "397821319",
    parameterSize: "494.03M",
    unusable:
      "Its context window, 32,768 tokens, is under the 65,536 drafting needs.",
  },
];
const BUNDLED_MODELS = [QWEN_4B, ...CANT_RUN];
const HOST_MODELS: FoundryOllamaModel[] = [
  { name: "qwen3.5:9b", sizeBytes: "6591893504", parameterSize: "9.7B" },
  QWEN_4B,
  CANT_RUN[0]!,
  {
    name: "gemma3:4b",
    sizeBytes: "3338801804",
    parameterSize: "4.3B",
    unusable: "Calls no tools.",
  },
  {
    name: "gpt-oss:120b-cloud",
    sizeBytes: "384",
    parameterSize: "116.8B",
    unusable:
      "Runs on ollama.com, so what it is sent would leave this computer.",
  },
  CANT_RUN[1]!,
];

/** What the Ollama server at a base URL (as baseUrl() keeps it) has. */
function ollamaModels(url: string): FoundryOllamaServer {
  const which = ollamaServer(url),
    host = new URL(url).hostname;
  if (which === "bundled" && knobs.ollamaBundled === false)
    return {
      problem:
        "The bundled Ollama isn't running. Start it with docker compose --profile ollama up -d.",
    };
  if (host.endsWith(".invalid"))
    return {
      problem: `Nothing answered at ${url}. Check the address and that Ollama runs there.`,
    };
  if (host.includes("notollama"))
    return {
      problem: `Something answered at ${url}, but not Ollama's API. Check the address.`,
    };
  // Too old to say which call tools: listed by name, none ruled out.
  if (host.includes("old"))
    return {
      version: "0.5.7",
      problem:
        "This Ollama (0.5.7) is too old to say which models call tools. Update it from ollama.com.",
      models: BUNDLED_MODELS.map(({ name, sizeBytes, parameterSize }) => ({
        name,
        sizeBytes,
        parameterSize,
      })).sort((a, b) => a.name!.localeCompare(b.name!)),
    };
  return {
    version: "0.13.0",
    models: structuredClone(which === "host" ? HOST_MODELS : BUNDLED_MODELS),
  };
}

/** The provider as failure copy names it: a custom server by its host. */
function label(kind: FoundryAiProviderKind, p: FoundryAiProvider) {
  if (kind !== "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE")
    return PROVIDERS[kind].label;
  try {
    return (
      new URL(p.openaiCompatible?.baseUrl || "").hostname ||
      "the custom endpoint"
    );
  } catch {
    return "the custom endpoint";
  }
}

/**
 * The key hint: the key's kind prefix (its first two dash-separated parts,
 * at most 8 characters) and its last four characters, so long as at least
 * HIDDEN characters stay unshown; else its last four alone, on the same
 * terms; else nothing. A short key (a self-hosted server's "sk-local-key1")
 * shows none of itself.
 *
 * @example
 * keyHint("sk-ant-api03-sentinel-0123456789abcd"); // "sk-ant…abcd"
 * keyHint("sk-local-key1"); // "…"
 */
export function keyHint(key: string) {
  const HIDDEN = 16;
  const parts = key.split("-");
  const prefix =
    parts.length >= 3 && parts[0]!.length + parts[1]!.length <= 8
      ? `${parts[0]}-${parts[1]}`
      : "";
  // The prefix, the dash after it and the last four are shown.
  if (prefix && key.length - prefix.length - 1 - 4 >= HIDDEN)
    return `${prefix}…${key.slice(-4)}`;
  return key.length - 4 >= HIDDEN ? `…${key.slice(-4)}` : "…";
}

const probeMs = () => knobs.aiProbeMs ?? 1500;
const ofWorkspace = <T extends { workspaceId: string }>(xs: T[], ws: string) =>
  xs.filter((x) => x.workspaceId === ws);
const newestActivation = (ws: string) =>
  ofWorkspace(state.activations, ws).at(-1);

/** Ends the workspace's due tests, oldest first, as the probe loop would. */
function advance(ws: string) {
  for (const c of ofWorkspace(state.configs, ws)) {
    if (c.probe !== "queued" && c.probe !== "running") continue;
    const elapsed = Date.now() - c.createdAt;
    if (elapsed < probeMs() / 2) continue;
    if (elapsed < probeMs()) {
      c.probe = "running";
      continue;
    }
    const at = c.createdAt + probeMs(),
      names = Object.values(c.models).map((m) => m.name);
    const failed: [AgentModelFailure, AgentProbeDetail] | undefined =
      c.kind === COMPAT && label(c.kind, c.provider).endsWith(".invalid")
        ? ["unavailable", "none"]
        : c.key?.includes("bad")
          ? ["auth", "none"]
          : names.some((n) => n.includes("notools"))
            ? ["unsupported", "tools_rejected"]
            : names.some((n) => n.includes("missing"))
              ? ["not_found", "none"]
              : knobs.aiProbeOutcome === "timeout"
                ? ["timeout", "none"]
                : c.provider.ollama && names.some((n) => n.includes("short"))
                  ? ["unsupported", "context_too_small"]
                  : c.provider.ollama &&
                      names.some((n) => n.includes("changed"))
                    ? ["model_changed", "none"]
                    : undefined;
    if (failed) {
      Object.assign(c, {
        probe: "failed",
        failure: failed[0],
        detail: failed[1],
        key: null,
      });
      continue;
    }
    const newest = ofWorkspace(state.configs, ws).at(-1),
      since = newestActivation(ws);
    if (newest !== c || (since && since.at > c.createdAt)) {
      Object.assign(c, { probe: "superseded", key: null });
      continue;
    }
    const before = since?.configId && find(since.configId);
    if (before) before.key = null;
    c.probe = "passed";
    state.activations.push({
      workspaceId: ws,
      configId: c.id,
      userId: c.createdBy,
      at,
    });
  }
}

const find = (configId: string) => state.configs.find((c) => c.id === configId);

/** The config in force, or undefined on the default (or none). */
function active(ws: string): Config | undefined {
  advance(ws);
  const a = newestActivation(ws);
  return a?.configId ? find(a.configId) : undefined;
}

const PROBE: Record<Probe, FoundryAiProbeState> = {
  queued: "FOUNDRY_AI_PROBE_STATE_QUEUED",
  running: "FOUNDRY_AI_PROBE_STATE_RUNNING",
  passed: "FOUNDRY_AI_PROBE_STATE_PASSED",
  failed: "FOUNDRY_AI_PROBE_STATE_FAILED",
  superseded: "FOUNDRY_AI_PROBE_STATE_SUPERSEDED",
};

/** A config as the WFE answers it; its key hint only to an owner. */
function configView(c: Config, owner = true): FoundryAiConfig {
  const user = db.users.get(c.createdBy);
  return {
    id: c.id,
    kind: c.kind,
    provider: structuredClone(c.provider),
    models: structuredClone(c.models) as FoundryAiModels,
    ...(owner && { keyHint: c.hint }),
    hasKey: c.key !== null,
    probeState: PROBE[c.probe],
    probeFailure: c.failure || "",
    probeDetail: c.detail || "",
    createdAt: new Date(c.createdAt).toISOString(),
    createdByName: user ? displayName(user) : "",
    label: label(c.kind, c.provider),
  };
}

const offered = (): FoundryAiProviderKind[] =>
  knobs.aiKinds ?? [
    "FOUNDRY_AI_KIND_ANTHROPIC",
    "FOUNDRY_AI_KIND_OPENAI",
    "FOUNDRY_AI_KIND_AZURE_OPENAI",
    COMPAT,
    // Never hosted: a native Ollama server takes no key, so a hosted one
    // would have to be public.
    ...(LOCAL_TOKEN ? ["FOUNDRY_AI_KIND_OLLAMA" as const] : []),
    "FOUNDRY_AI_KIND_BEDROCK",
  ];

function view(ws: string, owner = true): FoundryAi {
  const now = active(ws),
    a = newestActivation(ws),
    mine = ofWorkspace(state.configs, ws),
    newest = mine.at(-1),
    pending = mine.find((c) => c.probe === "queued" || c.probe === "running"),
    def = deploymentDefault(),
    found = detected();
  return {
    ...(now && { active: configView(now, owner) }),
    ...(def && {
      default: {
        kind: def.kind,
        models:
          def.kind === "FOUNDRY_AI_KIND_OLLAMA"
            ? free(def.models)
            : table(def.models),
        label: PROVIDERS[def.kind].label,
      },
    }),
    ...(pending && { pending: configView(pending, owner) }),
    ...(newest?.probe === "failed" &&
      (!a || newest.createdAt > a.at) && {
        lastFailed: configView(newest, owner),
      }),
    offered: offered().map((kind) => ({
      kind,
      key: KEY[kind],
      ...(SUGGESTED[kind] && { suggested: table(SUGGESTED[kind]) }),
    })),
    privateHostsAllowed: !!LOCAL_TOKEN,
    proRequired: !LOCAL_TOKEN && !isFoundryPro(ws),
    ...(found && { detected: found }),
  };
}

/** FailedPrecondition with the ErrorInfo the WFE's foundryPrecondition adds. */
function foundryPrecondition(ws: string, reason: string, message: string) {
  const reply = fail("FAILED_PRECONDITION", message);
  return {
    ...reply,
    body: {
      ...(reply.body as object),
      details: [
        {
          "@type": "type.googleapis.com/google.rpc.ErrorInfo",
          reason,
          domain: "foundry.helix-db.com",
          metadata: { workspace_id: ws },
        },
      ],
    },
  };
}

/**
 * Whose provider a workspace's model calls run on: its own saved one, the
 * deployment's default, or none (a local API without a default).
 */
export function aiSource(ws: string): "workspace" | "default" | "none" {
  return active(ws) ? "workspace" : deploymentDefault() ? "default" : "none";
}

/** The model in force for a task, or undefined while none is set up. */
export function aiModel(ws: string, role: FoundryAiRole): string | undefined {
  return active(ws)?.models[role].name ?? deploymentDefault()?.models[role];
}

/** The provider in force as failure copy names it: "Claude" for the house. */
export function aiLabel(ws: string): string {
  const c = active(ws);
  return c
    ? label(c.kind, c.provider)
    : PROVIDERS[deploymentDefault()?.kind ?? "FOUNDRY_AI_KIND_ANTHROPIC"].label;
}

/** The agent's name for the kind in force, as Ask's started event says it. */
export function aiProvider(ws: string): AgentProviderKind {
  return PROVIDERS[
    active(ws)?.kind ?? deploymentDefault()?.kind ?? "FOUNDRY_AI_KIND_ANTHROPIC"
  ].agent;
}

/**
 * The WFE's preflight on model work (generating, asking, completing an
 * upload): FOUNDRY_AI_NOT_CONFIGURED while the setting resolves to none;
 * hosted, FOUNDRY_PRO_REQUIRED while it is the workspace's own key and the
 * workspace is off Pro and Max, as Pro-only connectors are refused when
 * used after it leaves Pro (its work never moves to Helix's Claude
 * unasked); else undefined.
 */
export function aiRefusal(ws: string): MockReply | undefined {
  const source = aiSource(ws);
  if (source === "none")
    return foundryPrecondition(
      ws,
      "FOUNDRY_AI_NOT_CONFIGURED",
      "No AI provider is set up. An owner can choose one in Settings › AI models.",
    );
  if (source === "workspace" && !isFoundryPro(ws))
    return proRequired(ws, "Bringing your own model key needs Foundry Pro");
  return undefined;
}

/**
 * A draft whose model calls failed, worded as the WFE's DraftFailure: the
 * house's key-class failures are Helix's to fix, so they read as
 * unavailable.
 */
export function draftFailure(ws: string, failure: "refused" | "auth"): string {
  const p = aiLabel(ws);
  if (failure === "refused") return `${p} declined to draft this ontology`;
  return aiSource(ws) === "workspace"
    ? `${p} refused the API key. An owner can enter a new one in Settings › AI models.`
    : `${p} is unavailable; Helix will retry`;
}

const PRINTABLE = /^[\x20-\x7e]*$/;
const USD = /^(\d{1,7})(?:\.(\d{1,3}))?$/;

/**
 * Rates as the WFE reads and answers them: each a price in dollars with at
 * most three decimals, written back without trailing zeros, cached input
 * left out when empty, and output above 0. A string is the field at fault
 * and why.
 */
function parseRates(r: FoundryAiRates): FoundryAiRates | string {
  const usd = (v?: string) => {
    const m = USD.exec((v || "").trim());
    return m ? String(Number(m[0])) : undefined;
  };
  const input = usd(r.inputUsdPerMtok),
    output = usd(r.outputUsdPerMtok),
    cached = (r.cachedInputUsdPerMtok || "").trim();
  const bad = "must be a price in dollars, with at most three decimals";
  if (input === undefined) return `input_usd_per_mtok ${bad}`;
  if (output === undefined) return `output_usd_per_mtok ${bad}`;
  if (cached && usd(cached) === undefined)
    return `cached_input_usd_per_mtok ${bad}`;
  if (Number(output) === 0) return "output must be more than 0";
  return {
    inputUsdPerMtok: input,
    outputUsdPerMtok: output,
    ...(cached && { cachedInputUsdPerMtok: usd(cached) }),
  };
}

type Server = NonNullable<FoundryAiProvider["openaiCompatible"]>;

/**
 * A custom server's API and calls at once as the WFE reads them
 * (providerFromProto): the API named, and at most MAX_CALLS calls, 0 being
 * DEFAULT_CALLS. A string is why not, in its words.
 */
function serverShape(s: Server): Omit<Server, "baseUrl"> | string {
  if (
    s.api !== "FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS" &&
    s.api !== "FOUNDRY_AI_COMPAT_API_RESPONSES"
  )
    return "provider.api must be chat_completions or responses";
  const calls = Number(s.maxCallsInFlight ?? 0);
  if (!Number.isInteger(calls) || calls < 0 || calls > MAX_CALLS)
    return `provider.max_calls_in_flight must be 1 to ${MAX_CALLS}`;
  return { api: s.api, maxCallsInFlight: calls || DEFAULT_CALLS };
}

/**
 * Why the WFE refuses a custom server's base URL, in its words, or
 * undefined: ai.ModelURL, then, hosted, its host through the guard, by the
 * mock's DNS (".internal" names are private, ".invalid" ones don't resolve).
 */
function serverRefusal(raw: string): string | undefined {
  const field = "provider.base_url";
  let u: URL;
  try {
    u = new URL(raw.trim());
  } catch {
    return `${field} must be an http or https URL with a host`;
  }
  if (!["http:", "https:"].includes(u.protocol) || !u.hostname)
    return `${field} must be an http or https URL with a host`;
  if (u.username || u.password || u.search || u.hash || /[?#]/.test(raw))
    return `${field} must have no user, query or fragment`;
  if (LOCAL_TOKEN) return undefined;
  if (u.protocol === "http:") return `${field} must be https`;
  const host = u.hostname.toLowerCase();
  if (privateHost(host) || host.endsWith(".internal"))
    return "the endpoint resolves to a private address";
  if (host.endsWith(".invalid"))
    return "provider names a host that does not resolve";
  return undefined;
}

function save(
  ws: string,
  userId: string,
  body: Extract<UpdateFoundryAiBody, { provider: unknown }>,
): MockReply {
  const kind = providerKind(body.provider);
  if (!kind) return fail("INVALID_ARGUMENT", "provider is required");
  const server =
    kind === COMPAT ? serverShape(body.provider.openaiCompatible!) : undefined;
  if (typeof server === "string") return fail("INVALID_ARGUMENT", server);
  const p = label(kind, body.provider);
  if (!offered().includes(kind))
    return fail("INVALID_ARGUMENT", `${p} is not offered here`);
  if (!LOCAL_TOKEN && !isFoundryPro(ws))
    return proRequired(ws, "Bringing your own model key needs Foundry Pro");
  const models = {} as Config["models"];
  for (const role of ROLE_IDS) {
    const given = body.models?.[role] as FoundryAiModel | undefined,
      suggested = SUGGESTED[kind]?.[role];
    if (!given && !suggested)
      return fail("INVALID_ARGUMENT", `models.${role} is required`);
    const name = (given?.name ?? suggested!).trim();
    if (!/^\S{1,200}$/.test(name))
      return fail(
        "INVALID_ARGUMENT",
        `models.${role}.name must be 1 to 200 characters with no spaces`,
      );
    if (["sk-", "ABSK", "bedrock-api-key-"].some((k) => name.startsWith(k)))
      return fail(
        "INVALID_ARGUMENT",
        `models.${role}.name looks like an API key; enter the key in the API key field`,
      );
    if (given?.rates) {
      const rates = parseRates(given.rates);
      if (typeof rates === "string")
        return fail("INVALID_ARGUMENT", `models.${role}.price.rates.${rates}`);
      models[role] = { name, rates };
    } else if (given?.free) {
      if (
        kind !== "FOUNDRY_AI_KIND_OLLAMA" &&
        kind !== "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE"
      )
        return fail(
          "INVALID_ARGUMENT",
          `models.${role}.price can be free only for Ollama and OpenAI-compatible servers`,
        );
      models[role] = { name, free: {} };
    } else if (given?.tableAs) {
      if (
        kind !== "FOUNDRY_AI_KIND_AZURE_OPENAI" &&
        kind !== "FOUNDRY_AI_KIND_BEDROCK"
      )
        return fail(
          "INVALID_ARGUMENT",
          `models.${role}.price can be another model's only for an Azure deployment or a Bedrock inference profile`,
        );
      models[role] = { name, tableAs: given.tableAs.trim() };
    } else models[role] = { name, table: {} };
  }
  const endpoint =
    body.provider.azureOpenai?.endpoint ??
    body.provider.bedrock?.region ??
    body.provider.ollama?.baseUrl ??
    body.provider.openaiCompatible?.baseUrl ??
    "";
  const problem =
    kind === COMPAT
      ? serverRefusal(endpoint)
      : endpointProblem(kind, endpoint, !!LOCAL_TOKEN);
  if (problem) return fail("INVALID_ARGUMENT", problem);
  // Ollama's window: int64 as a string (or a number), "0" or unset 65536.
  const window = String(body.provider.ollama?.contextTokens ?? "0").trim();
  if (body.provider.ollama && window !== "0" && contextProblem(window))
    return fail(
      "INVALID_ARGUMENT",
      `provider.context_tokens must be ${OLLAMA_CONTEXT.draft} to 1048576`,
    );
  const provider: FoundryAiProvider = body.provider.azureOpenai
    ? { azureOpenai: { endpoint: azureEndpoint(endpoint)! } }
    : server
      ? { openaiCompatible: { baseUrl: baseUrl(endpoint)!, ...server } }
      : body.provider.ollama
        ? {
            ollama: {
              baseUrl: baseUrl(endpoint)!,
              contextTokens:
                window === "0"
                  ? String(OLLAMA_CONTEXT.default)
                  : String(Number(window)),
            },
          }
        : body.provider.bedrock
          ? {
              bedrock: {
                region: endpoint.trim(),
                ...(body.provider.bedrock.fips && { fips: true }),
              },
            }
          : structuredClone(body.provider);

  const given = (body.apiKey || "").trim(),
    rule = KEY[kind],
    now = active(ws);
  if (rule === "FOUNDRY_AI_KEY_NONE" && given)
    return fail("INVALID_ARGUMENT", `${p} takes no API key`);
  if (given.length > MAX_KEY || !PRINTABLE.test(given))
    return fail(
      "INVALID_ARGUMENT",
      `the API key must be at most ${MAX_KEY} printable characters`,
    );
  const kept =
    !given &&
    now?.key &&
    canReuseKey(configView(now), {
      kind,
      provider,
      models: Object.fromEntries(
        ROLE_IDS.map((r) => [r, models[r].name]),
      ) as Record<FoundryAiRole, string>,
    })
      ? now
      : undefined;
  if (!given && !kept && rule === "FOUNDRY_AI_KEY_REQUIRED")
    return fail("INVALID_ARGUMENT", "Enter the API key for this endpoint.");

  for (const role of ROLE_IDS) {
    const m = models[role];
    const row = "table" in m ? m.name : "tableAs" in m ? m.tableAs : undefined;
    if (row !== undefined && !pricedRow(kind, row))
      return foundryPrecondition(
        ws,
        "FOUNDRY_AI_PRICE_REQUIRED",
        `Helix has no price for the ${role} model ${row}; enter its prices`,
      );
  }
  const hourAgo = Date.now() - 3_600_000;
  if (
    ofWorkspace(state.configs, ws).filter((c) => c.createdAt > hourAgo)
      .length >= PER_HOUR
  )
    return fail(
      "RESOURCE_EXHAUSTED",
      `a workspace may save its AI settings ${PER_HOUR} times an hour; try again later`,
    );

  for (const c of ofWorkspace(state.configs, ws))
    if (c.probe === "queued" || c.probe === "running")
      Object.assign(c, { probe: "superseded", key: null });
  const key = given || kept?.key || null;
  state.configs.push({
    id: id("aicfg"),
    workspaceId: ws,
    kind,
    provider,
    models,
    key,
    hint: kept ? kept.hint : key ? keyHint(key) : "",
    probe: "queued",
    createdBy: userId,
    createdAt: Date.now(),
  });
  return ok({ ai: view(ws) });
}

export const aiRoutes: MockRoute[] = [
  {
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ai",
    handle: ({ params, principal: { user } }) => {
      const m = membership(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      // workspace.ai.manage reads the key hints: owners only.
      return ok({ ai: view(m.workspaceId, m.role === "WORKSPACE_ROLE_OWNER") });
    },
  },
  {
    method: "PUT",
    path: "/v1/workspaces/{workspaceId}/foundry/ai",
    handle: ({ params, body, principal: { user } }) => {
      const m = membership(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      // workspace.ai.manage: owners only.
      if (m.role !== "WORKSPACE_ROLE_OWNER")
        return fail("PERMISSION_DENIED", "permission denied");
      const req = (body || {}) as UpdateFoundryAiBody;
      const ws = m.workspaceId;
      if ("useDefault" in req && req.useDefault) {
        if (!deploymentDefault())
          return fail(
            "FAILED_PRECONDITION",
            "this install has no default AI provider; choose one",
          );
        if (
          (req as { models?: unknown }).models ||
          (req as { apiKey?: string }).apiKey
        )
          return fail("INVALID_ARGUMENT", "use_default takes no models or key");
        const now = active(ws);
        if (now) now.key = null;
        for (const c of ofWorkspace(state.configs, ws))
          if (c.probe === "queued" || c.probe === "running")
            Object.assign(c, { probe: "superseded", key: null });
        state.activations.push({
          workspaceId: ws,
          configId: null,
          userId: user.id,
          at: Date.now(),
        });
        return ok({ ai: view(ws) });
      }
      if (!("provider" in req))
        return fail("INVALID_ARGUMENT", "choose use_default or a provider");
      return save(ws, user.id, req);
    },
  },
  {
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ai/detect",
    handle: ({ params, principal: { user } }) => {
      const m = membership(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      if (m.role !== "WORKSPACE_ROLE_OWNER")
        return fail("PERMISSION_DENIED", "permission denied");
      if (!LOCAL_TOKEN)
        return fail(
          "FAILED_PRECONDITION",
          "only a local install looks for its default AI provider",
        );
      if (knobs.aiDefaultNamed)
        return fail(
          "FAILED_PRECONDITION",
          "this install names its default AI provider (FOUNDRY_AI_PROVIDER)",
        );
      if (knobs.aiDetect) {
        knobs.aiDefault = knobs.aiDetect !== "none";
        if (knobs.aiDetect === "ollama") knobs.aiDefaultKind = "ollama";
        else delete knobs.aiDefaultKind;
      }
      state.detectedAt = Date.now();
      return ok({ ai: view(m.workspaceId) });
    },
  },
  {
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ai/ollama-models",
    handle: ({ params, query, principal: { user } }) => {
      const m = membership(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      if (m.role !== "WORKSPACE_ROLE_OWNER")
        return fail("PERMISSION_DENIED", "permission denied");
      if (!LOCAL_TOKEN)
        return fail("FAILED_PRECONDITION", "Ollama is not offered here");
      const raw = query.get("baseUrl") || "";
      const problem = endpointProblem("FOUNDRY_AI_KIND_OLLAMA", raw, true);
      if (problem) return fail("INVALID_ARGUMENT", problem);
      return ok({ server: ollamaModels(baseUrl(raw)!) });
    },
  },
];
