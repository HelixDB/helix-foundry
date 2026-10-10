import { it, expect, vi, afterEach } from "vitest";
import { FoundryClient, FoundryError } from "../packages/sdk/src/index.js";
const client = () =>
  new FoundryClient({
    baseUrl: "https://foundry.invalid",
    workspaceId: "w",
    token: "fixture",
  });
afterEach(() => vi.unstubAllGlobals());
it("parses comments, event fields, multiline data and chunked CRLF", async () => {
  const chunks = [
    ": heartbeat\r\nevent: update\r\ndata:{\r",
    '\ndata: "ok":true}\r\n\r',
    '\ndata: {"next":2}\n\n',
  ];
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      new Response(
        new ReadableStream({
          start(c) {
            for (const x of chunks) c.enqueue(new TextEncoder().encode(x));
            c.close();
          },
        }),
      ),
    ),
  );
  const out = [];
  for await (const e of client().events()) out.push(e);
  expect(out).toEqual([{ ok: true }, { next: 2 }]);
});
