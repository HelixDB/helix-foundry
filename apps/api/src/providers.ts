import { z } from "zod";
import { request as httpRequest } from "node:http";
import { request as httpsRequest } from "node:https";
import type {
  ModelProvider,
  ProviderSettings,
} from "../../../packages/shared/src/index.js";
import { config } from "./config.js";
import { assert, AppError } from "./security.js";
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
    } else {
      assert(s.apiKey, "OpenAI API key is required");
      url = "https://api.openai.com/v1/responses";
      headers.authorization = "Bearer " + s.apiKey;
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
      const r = await postLocal(url, JSON.stringify(body), abort);
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
    });
    if (!response.ok)
      throw new AppError(
        502,
        `${s.provider} returned HTTP ${response.status}. Check the model, credentials, and provider quota.`,
      );
    let value: unknown,
      tokens = 0;
    const data = (await response.json()) as any;
    if (s.provider === "claude") {
      value = data.content.find(
        (x: any) => x.type === "tool_use" && x.name === "submit",
      )?.input;
      tokens =
        (data.usage?.input_tokens || 0) + (data.usage?.output_tokens || 0);
    } else {
      const text = data.output
        ?.flatMap((x: any) => x.content || [])
        .find((x: any) => x.type === "output_text")?.text;
      assert(text, "Model did not return a structured response");
      value = JSON.parse(text);
      tokens = data.usage?.total_tokens || 0;
    }
    return { value: withoutNullOptionals(value), tokens };
  }
}
// Ollama sends no headers until it has read the whole prompt, which can take
// longer than fetch's fixed five-minute header timeout on CPU-only installs.
function postLocal(url: string, body: string, signal: AbortSignal) {
  return new Promise<{ status: number; text: string }>((resolve, reject) => {
    const target = new URL(url);
    const req = (target.protocol === "https:" ? httpsRequest : httpRequest)(
      target,
      {
        method: "POST",
        headers: { "content-type": "application/json" },
        signal,
      },
      (res) => {
        let text = "";
        res.setEncoding("utf8");
        res.on("data", (chunk) => (text += chunk));
        res.on("end", () => resolve({ status: res.statusCode || 0, text }));
        res.on("error", (error) =>
          reject(signal.aborted ? signal.reason : error),
        );
      },
    );
    // Surface the abort reason (a timeout or "Run canceled"), as fetch does.
    req.on("error", (error) => reject(signal.aborted ? signal.reason : error));
    req.end(body);
  });
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
  const o = Object.fromEntries(
    Object.entries(schema)
      .filter(([k]) => !["$schema", "default"].includes(k))
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
