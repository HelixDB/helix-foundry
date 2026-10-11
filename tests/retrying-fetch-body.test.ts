import { createServer, type ServerResponse } from "node:http";
import type { Socket } from "node:net";
import { afterEach, expect, it, vi } from "vitest";
import { retryingFetch } from "../apps/api/src/retrying-fetch.js";

afterEach(() => vi.unstubAllGlobals());

async function withServer(
  respond: (response: ServerResponse, attempt: number) => void,
  run: (url: URL) => Promise<void>,
) {
  const sockets = new Set<Socket>();
  let attempts = 0;
  const server = createServer((_request, response) => respond(response, ++attempts));
  server.on("connection", (socket) => {
    sockets.add(socket);
    socket.on("close", () => sockets.delete(socket));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("Missing fixture address");
  try {
    await run(new URL(`http://127.0.0.1:${address.port}/`));
  } finally {
    for (const socket of sockets) socket.destroy();
    await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
  }
}

it.each([429, 503])("releases a discarded streaming %i response before retrying", async (status) => {
  let firstClosed = false;
  let closedBeforeRetry = false;
  await withServer((response, attempt) => {
    if (attempt === 1) {
      response.on("close", () => { firstClosed = true; });
      response.writeHead(status, { "Content-Type": "text/plain" });
      response.flushHeaders();
      response.write("error response still streaming");
    } else {
      closedBeforeRetry = firstClosed;
      response.end("success");
    }
  }, async (url) => {
    const response = await retryingFetch(url, {});
    expect(await response.text()).toBe("success");
    expect(closedBeforeRetry).toBe(true);
  });
});

it("leaves a terminal response body readable", async () => {
  await withServer((response) => {
    response.writeHead(400);
    response.end("terminal details");
  }, async (url) => {
    const response = await retryingFetch(url, {});
    expect(response.status).toBe(400);
    expect(await response.text()).toBe("terminal details");
  });
});

it("still retries when the discarded body has already aborted", async () => {
  const aborted = new Response(new ReadableStream({
    start(controller) { controller.error(new DOMException("aborted", "AbortError")); },
  }), { status: 429 });
  const fetch = vi.fn().mockResolvedValueOnce(aborted).mockResolvedValueOnce(new Response("success"));
  vi.stubGlobal("fetch", fetch);
  const response = await retryingFetch(new URL("https://fixture.invalid/"), {});
  expect(await response.text()).toBe("success");
  expect(fetch).toHaveBeenCalledTimes(2);
});

it("leaves the exhausted final retry body readable", async () => {
  let attempts = 0;
  vi.stubGlobal("fetch", vi.fn(async () => {
    attempts++;
    return new Response("last details", { status: 429 });
  }));
  const response = await retryingFetch(new URL("https://fixture.invalid/"), {});
  expect(attempts).toBe(7);
  expect(response.status).toBe(429);
  expect(await response.text()).toBe("last details");
}, 20000);
