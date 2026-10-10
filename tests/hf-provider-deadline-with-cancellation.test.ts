import { it, expect, vi, afterEach } from "vitest";
import { Provider } from "../apps/api/src/providers.js";
import { ProviderSchema } from "../packages/shared/src/index.js";
afterEach(() => vi.unstubAllGlobals());
const provider = () =>
  new Provider(
    ProviderSchema.parse({
      provider: "openai",
      model: "test",
      apiKey: "test-only",
    }),
  );
const output = (text: string) =>
  Response.json({
    status: "completed",
    output: [{ content: [{ type: "output_text", text }] }],
    usage: { total_tokens: 1 },
  });
it("retains both cancellation and configured timeout", async () => {
  const signals: AbortSignal[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_url, init) => {
      signals.push(init.signal);
      return output("{}");
    }),
  );
  const controller = new AbortController();
  await provider().generate("", {}, {}, controller.signal);
  expect(signals[0]).not.toBe(controller.signal);
  expect(signals[0].aborted).toBe(false);
  controller.abort(new Error("canceled"));
  expect(signals[0].reason.message).toBe("canceled");
});

it("keeps the configured deadline when a caller supplies a signal", async () => {
  const timeout = new AbortController();
  const spy = vi.spyOn(AbortSignal, "timeout").mockReturnValue(timeout.signal);
  let seen: AbortSignal | undefined;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_url, init) => {
      seen = init.signal;
      return output("{}");
    }),
  );
  try {
    await provider().generate("", {}, {}, new AbortController().signal);
    timeout.abort(new Error("deadline"));
    expect(spy).toHaveBeenCalledWith(600000);
    expect(seen?.reason.message).toBe("deadline");
  } finally {
    spy.mockRestore();
  }
});
