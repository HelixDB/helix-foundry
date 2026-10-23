import { z } from "zod";
import type {
  ModelProvider,
  ProviderSettings,
} from "../../../packages/shared/src/index.js";
import { config } from "./config.js";
import { assert, AppError } from "./security.js";
import { postJson } from "./post.js";
// Azure OpenAI's OpenAI-compatible base URL, from the resource endpoint
// (`https://RESOURCE.openai.azure.com`) or that endpoint with `/openai/v1`.
export function azureBase(endpoint: string) {
  const base = endpoint.replace(/\/+$/, "");
  return base.endsWith("/openai/v1") ? base : base + "/openai/v1";
}
// A blank key keeps the saved credential only for the endpoint it belongs to:
// the same Custom base URL, or the same Azure OpenAI endpoint and deployment.
// An Amazon Bedrock key keeps working across regions and models, since the
// schema only allows AWS's own Bedrock endpoints.
export function canReuseProviderKey(
  previous: Pick<ProviderSettings, "provider" | "baseUrl" | "model">,
  next: Pick<ProviderSettings, "provider" | "baseUrl" | "model">,
) {
  if (previous.provider !== next.provider) return false;
  if (next.provider === "azure")
    return (
      !!previous.baseUrl &&
      !!next.baseUrl &&
      azureBase(previous.baseUrl) === azureBase(next.baseUrl) &&
      previous.model === next.model
    );
  return (
    next.provider !== "custom" ||
    previous.baseUrl?.replace(/\/+$/, "") === next.baseUrl?.replace(/\/+$/, "")
  );
}
export class Provider implements ModelProvider {
  private verified = false;
  constructor(public settings: ProviderSettings) {}
  async generate(
    system: string,
    context: unknown,
    schema: Record<string, unknown>,
    signal?: AbortSignal,
  ) {
    if (!this.verified) {
      await verifyPinnedModel(this.settings);
      this.verified = true;
    }
    const s = this.settings,
      user = JSON.stringify(context);
    let url: string,
      headers: Record<string, string> = { "content-type": "application/json" },
      body: any;
    if (s.provider === "local") {
      url = (s.baseUrl || config.ollama).replace(/\/$/, "") + "/api/chat";
      // Size the window to the prompt (JSON averages ~2.8 characters per token)
      // and keep room for the answer, so long prompts are never silently cut.
      const promptTokens = Math.ceil((system.length + user.length) / 2.8);
      const numCtx = [8192, 16384, 32768].find((n) => n >= promptTokens + 2560);
      assert(
        numCtx,
        "Too much data for the local model at once. Remove some datasets from setup or choose Claude or OpenAI.",
      );
      body = {
        model: s.model,
        stream: false,
        think: false,
        keep_alive: config.modelKeepAlive,
        messages: [
          { role: "system", content: system },
          { role: "user", content: user },
        ],
        format: boundedLists(schema),
        options: {
          temperature: 0,
          num_ctx: numCtx,
          // Answers are well under this; a looping generation stops instead of
          // running until the run's time budget is spent.
          num_predict: Math.min(numCtx - promptTokens, 3072),
        },
      };
    } else if (s.provider === "claude") {
      assert(s.apiKey, "Claude API key is required");
      url = "https://api.anthropic.com/v1/messages";
      headers["x-api-key"] = s.apiKey;
      headers["anthropic-version"] = "2023-06-01";
      body = {
        model: s.model,
        max_tokens: 8192,
        system,
        messages: [{ role: "user", content: user }],
        tools: [
          {
            name: "submit",
            description: "Return the requested structured result",
            input_schema: schema,
          },
        ],
        tool_choice: { type: "tool", name: "submit" },
      };
    } else if (s.provider === "bedrock") {
      // Amazon Bedrock's Converse API, which serves most of its models the
      // same way. The schema is a tool the model must call, as for Claude.
      assert(s.apiKey, "Amazon Bedrock API key is required");
      assert(s.baseUrl, "Choose an AWS region for Amazon Bedrock.");
      url =
        s.baseUrl.replace(/\/+$/, "") +
        `/model/${encodeURIComponent(s.model)}/converse`;
      headers.authorization = "Bearer " + s.apiKey;
      const json = { ...schema };
      delete json.$schema;
      body = {
        system: [{ text: system }],
        messages: [{ role: "user", content: [{ text: user }] }],
        inferenceConfig: { maxTokens: 8192 },
        toolConfig: {
          tools: [
            {
              toolSpec: {
                name: "submit",
                description: "Return the requested structured result",
                inputSchema: { json },
              },
            },
          ],
          // The only tool, so "any" means this one, on more models than
          // naming it does.
          toolChoice: { any: {} },
        },
      };
    } else if (s.provider === "custom" && s.apiFormat !== "responses") {
      assert(s.baseUrl, "Enter an API base URL.");
      url = s.baseUrl.replace(/\/+$/, "") + "/chat/completions";
      if (s.apiKey) headers.authorization = "Bearer " + s.apiKey;
      body = {
        model: s.model,
        max_tokens: 8192,
        stream: false,
        messages: [
          { role: "system", content: system },
          { role: "user", content: user },
        ],
        response_format: {
          type: "json_schema",
          json_schema: {
            name: "result",
            schema: strictSchema(schema),
            strict: true,
          },
        },
      };
    } else {
      // OpenAI, Azure OpenAI and Custom endpoints in the Responses format.
      if (s.provider === "azure") {
        // The same Responses API, on the Azure resource, with the deployment
        // name as the model. Azure's v1 API reads the key from the api-key
        // header only, so it is not also sent as a bearer token.
        assert(s.apiKey, "Azure OpenAI API key is required");
        assert(s.baseUrl, "Azure OpenAI endpoint is required");
        url = azureBase(s.baseUrl) + "/responses";
        headers["api-key"] = s.apiKey;
      } else if (s.provider === "custom") {
        assert(s.baseUrl, "Enter an API base URL.");
        url = s.baseUrl.replace(/\/+$/, "") + "/responses";
        if (s.apiKey) headers.authorization = "Bearer " + s.apiKey;
      } else {
        assert(s.apiKey, "OpenAI API key is required");
        url = "https://api.openai.com/v1/responses";
        headers.authorization = "Bearer " + s.apiKey;
      }
      body = {
        model: s.model,
        max_output_tokens: 8192,
        store: false,
        instructions: system,
        input: user,
        text: {
          format: {
            type: "json_schema",
            name: "result",
            schema: strictSchema(schema),
            strict: true,
          },
        },
      };
    }
    const abort = signal || AbortSignal.timeout(s.timeoutSeconds * 1000);
    if (s.provider === "local") {
      // Ollama sends no headers until it has read the whole prompt, which can
      // take longer than fetch allows on CPU-only installs.
      const r = await postJson(url, JSON.stringify(body), abort);
      assert(
        r.status >= 200 && r.status < 300,
        `local returned HTTP ${r.status}. Check the model, credentials, and provider quota.`,
        502,
      );
      const data = ollamaResult(r.text);
      assert(
        data.done_reason !== "length",
        "The local model ran out of room for its answer. Remove some datasets from setup or choose Claude or OpenAI.",
      );
      return {
        value: withoutNullOptionals(JSON.parse(data.content)),
        tokens: data.prompt_eval_count + data.eval_count,
      };
    }
    const response = await fetch(url, {
      method: "POST",
      headers,
      body: JSON.stringify(body),
      signal: abort,
      // A user-entered endpoint must not forward the key somewhere else.
      ...(s.provider === "custom" ||
      s.provider === "azure" ||
      s.provider === "bedrock"
        ? { redirect: "error" as const }
        : {}),
    });
    if (!response.ok && s.provider === "bedrock")
      throw new AppError(502, bedrockError(response));
    if (!response.ok)
      throw new AppError(
        502,
        `${providerName(s.provider)} returned HTTP ${response.status}. Check the model, credentials, and provider quota.`,
      );
    let value: unknown,
      tokens = 0;
    const data = (await response.json().catch((error) => {
      throwJsonError(error, s.provider);
    })) as any;
    if (s.provider === "claude") {
      value = data.content.find(
        (x: any) => x.type === "tool_use" && x.name === "submit",
      )?.input;
      tokens =
        (data.usage?.input_tokens || 0) + (data.usage?.output_tokens || 0);
    } else if (s.provider === "bedrock") {
      assert(
        data.stopReason !== "max_tokens",
        "The Amazon Bedrock model ran out of room for its answer.",
        502,
      );
      const input = data.output?.message?.content?.find(
        (x: any) => x.toolUse?.name === "submit",
      )?.toolUse?.input;
      assert(
        input && typeof input === "object",
        "The Amazon Bedrock model did not return a structured response. Choose a model that supports tool use, such as Claude or Amazon Nova.",
        502,
      );
      value = input;
      tokens =
        data.usage?.totalTokens ||
        (data.usage?.inputTokens || 0) + (data.usage?.outputTokens || 0);
    } else if (s.provider === "custom" && s.apiFormat !== "responses") {
      const choice = data.choices?.[0];
      assert(
        choice?.finish_reason !== "length",
        "Custom model ran out of room for its answer.",
        502,
      );
      const text = choice?.message?.content;
      assert(
        typeof text === "string" && text,
        "Custom model did not return a structured response",
        502,
      );
      value = customJson(text);
      tokens = data.usage?.total_tokens || 0;
    } else {
      if (s.provider === "custom")
        assert(
          data.status !== "incomplete" && data.status !== "failed",
          "Custom model did not complete its answer.",
          502,
        );
      const text = data.output
        ?.flatMap((x: any) => x.content || [])
        .find((x: any) => x.type === "output_text")?.text;
      assert(text, "Model did not return a structured response");
      try {
        value = s.provider === "custom" ? customJson(text) : JSON.parse(text);
      } catch (error) {
        throwJsonError(error, s.provider);
      }
      tokens = data.usage?.total_tokens || 0;
    }
    return { value: withoutNullOptionals(value), tokens };
  }
}
// How errors name a provider: "The custom AI provider returned HTTP 401".
function providerName(provider: string) {
  return provider === "custom"
    ? "The custom AI provider"
    : provider === "azure"
      ? "Azure OpenAI"
      : provider === "bedrock"
        ? "Amazon Bedrock"
        : provider;
}
// Amazon Bedrock's errors by kind, from its x-amzn-errortype header. The
// response body is not repeated, since it can quote the request.
function bedrockError(response: Response) {
  const type = response.headers.get("x-amzn-errortype")?.split(":")[0];
  const reason =
    type === "UnrecognizedClientException" ||
    type === "AccessDeniedException" ||
    response.status === 401 ||
    response.status === 403
      ? "Check the API key, and that the model is enabled for your account in this region."
      : type === "ResourceNotFoundException" || response.status === 404
        ? "Check the model ID and the region."
        : type === "ValidationException" || response.status === 400
          ? "Check the model ID. Some models need an inference profile ID, such as us.anthropic.claude-sonnet-4-6, and the model must support tool use."
          : type === "ThrottlingException" || response.status === 429
            ? "Retry later, or ask for a higher quota for this model."
            : "Check the model, credentials, and provider quota.";
  return `Amazon Bedrock returned HTTP ${response.status}${type ? ` (${type})` : ""}. ${reason}`;
}
function customJson(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    throw new AppError(
      502,
      "Custom model did not return valid JSON. Choose a model that supports structured output.",
    );
  }
}
// JSON decoder messages can include response contents. Only decoding failures
// get recovery guidance; transport failures and cancellation keep their identity.
function throwJsonError(error: unknown, provider: string): never {
  if (error instanceof SyntaxError)
    throw new AppError(
      502,
      `${providerName(provider)} returned invalid structured JSON. Retry the request or choose a more capable model.`,
    );
  throw error;
}
// Accepts a single response object or newline-delimited streamed parts.
function ollamaResult(text: string) {
  const result = {
    content: "",
    done_reason: "",
    prompt_eval_count: 0,
    eval_count: 0,
  };
  for (const line of text.split("\n")) {
    if (!line.trim()) continue;
    const part = JSON.parse(line);
    if (part.error) throw new AppError(502, "Local model error: " + part.error);
    result.content += part.message?.content || "";
    if (part.done_reason) result.done_reason = part.done_reason;
    if (part.prompt_eval_count)
      result.prompt_eval_count = part.prompt_eval_count;
    if (part.eval_count) result.eval_count = part.eval_count;
  }
  return result;
}
// Small models can loop inside an unbounded list under constrained decoding.
function boundedLists(schema: any): any {
  if (Array.isArray(schema)) return schema.map(boundedLists);
  if (!schema || typeof schema !== "object") return schema;
  const o = Object.fromEntries(
    Object.entries(schema).map(([k, v]) => [k, boundedLists(v)]),
  ) as any;
  if (o.type === "array" && o.maxItems === undefined) o.maxItems = 12;
  return o;
}
function strictSchema(schema: any): any {
  if (Array.isArray(schema)) return schema.map(strictSchema);
  if (!schema || typeof schema !== "object") return schema;
  // Strict mode rejects propertyNames, which a record's schema carries; the
  // object is closed below anyway.
  const o = Object.fromEntries(
    Object.entries(schema)
      .filter(([k]) => !["$schema", "default", "propertyNames"].includes(k))
      .map(([k, v]) => [k, strictSchema(v)]),
  ) as any;
  if (o.type === "object" && !o.properties) {
    o.properties = {};
  }
  if (o.type === "object" && o.properties) {
    o.additionalProperties = false;
    const required = o.required || [];
    for (const [k, v] of Object.entries(o.properties)) {
      if (!required.includes(k))
        o.properties[k] = { anyOf: [v, { type: "null" }] };
    }
    o.required = Object.keys(o.properties);
  }
  return o;
}
export async function verifyPinnedModel(settings: ProviderSettings) {
  if (settings.provider === "local" && settings.model === "qwen3:4b") {
    const r = await fetch((settings.baseUrl || config.ollama) + "/api/tags", {
      signal: AbortSignal.timeout(5000),
    });
    const tags = (await r.json()) as any;
    assert(
      tags.models?.some(
        (m: any) =>
          m.name === "qwen3:4b" &&
          m.digest ===
            "359d7dd4bcdab3d86b87d73ac27966f4dbb9f5efdfcc75d34a8764a09474fae7",
      ),
      "Default model digest does not match the tested release. Download the pinned model or explicitly choose another model.",
    );
  }
}
export async function testProvider(
  settings: ProviderSettings,
  signal?: AbortSignal,
) {
  const schema = z.object({ ready: z.literal(true), message: z.string() });
  const result = await new Provider(settings).generate(
    'Return ready=true and message="Connected". This tests structured output.',
    {},
    z.toJSONSchema(schema),
    signal,
  );
  schema.parse(result.value);
  return { ok: true, tokens: result.tokens };
}

function withoutNullOptionals(value: any): any {
  if (Array.isArray(value)) return value.map(withoutNullOptionals);
  if (value && typeof value === "object")
    return Object.fromEntries(
      Object.entries(value)
        .filter(([, v]) => v !== null)
        .map(([k, v]) => [k, withoutNullOptionals(v)]),
    );
  return value;
}
