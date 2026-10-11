import { afterEach, expect, it, vi } from "vitest";
import { Provider } from "../apps/api/src/providers.js";
import { ProviderSchema } from "../packages/shared/src/index.js";

afterEach(() => vi.unstubAllGlobals());
const provider = (kind: "openai" | "claude") =>
  new Provider(
    ProviderSchema.parse({
      provider: kind,
      model: "test",
      apiKey: "test-only",
    }),
  );
const message = (kind: string) =>
  `${kind} returned invalid structured JSON. Retry the request or choose a more capable model.`;

for (const kind of ["openai", "claude"] as const)
  it(`reports malformed ${kind} envelopes without retaining response data`, async () => {
    const fetchMock = vi.fn(async () => new Response('{"private-record":'));
    vi.stubGlobal("fetch", fetchMock);
    await expect(provider(kind).generate("", {}, {})).rejects.toMatchObject({
      statusCode: 502,
      message: message(kind),
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

it("reports incomplete OpenAI output JSON without exposing its contents", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      Response.json({
        output: [
          { content: [{ type: "output_text", text: '{"private-record":' }] },
        ],
      }),
    ),
  );
  await expect(provider("openai").generate("", {}, {})).rejects.toMatchObject({
    statusCode: 502,
    message: message("openai"),
  });
});

for (const kind of ["openai", "claude"] as const)
  it(`preserves ${kind} response-read cancellation`, async () => {
    const abort = new DOMException("Caller canceled", "AbortError");
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => ({
        ok: true,
        json: async () => {
          throw abort;
        },
      })),
    );
    await expect(provider(kind).generate("", {}, {})).rejects.toBe(abort);
  });

it("keeps valid incomplete-status OpenAI output and the first complete message", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      Response.json({
        status: "incomplete",
        output: [
          { content: [{ type: "output_text", text: '{"ready":true}' }] },
          { content: [{ type: "output_text", text: '{"ready":false}' }] },
        ],
        usage: { total_tokens: 7 },
      }),
    ),
  );
  await expect(provider("openai").generate("", {}, {})).resolves.toEqual({
    value: { ready: true },
    tokens: 7,
  });
});

it("preserves Claude's absent submit handling for existing stage validation", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => Response.json({ content: [], usage: {} })),
  );
  await expect(provider("claude").generate("", {}, {})).resolves.toEqual({
    value: undefined,
    tokens: 0,
  });
});
