import { afterAll, afterEach, beforeAll, expect, it, vi } from "vitest";
import { createServer, type IncomingMessage, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { config } from "../apps/api/src/config.js";
import { execute } from "../apps/api/src/executor.js";
// Stands in for the executor: each test sets how it answers.
let answer: (
  body: any,
  req: IncomingMessage,
) => Promise<[number, unknown] | undefined>;
let server: Server;
const executorUrl = config.executor,
  internalKey = config.internalKey;
beforeAll(async () => {
  server = createServer(async (req, res) => {
    let body = "";
    for await (const chunk of req) body += chunk;
    const reply = await answer(JSON.parse(body), req);
    if (!reply) return; // Never answers.
    res.writeHead(reply[0], { "content-type": "application/json" });
    res.end(JSON.stringify(reply[1]));
  });
  await new Promise<void>((done) => server.listen(0, "127.0.0.1", done));
  config.executor = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  config.internalKey = "k".repeat(64);
});
afterEach(() => {
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
});
afterAll(async () => {
  config.executor = executorUrl;
  config.internalKey = internalKey;
  server.closeAllConnections();
  await new Promise((done) => server.close(done));
});
const query = {
    mode: "query" as const,
    scope: "a",
    version: "v",
    inputs: [],
    sql: "SELECT 1",
  },
  materialize = { ...query, mode: "materialize" as const };
it("waits for the executor's time limit for the job, and a margin to stop it", async () => {
  const timeout = vi.spyOn(AbortSignal, "timeout");
  answer = async (body, req) => {
    expect(req.headers["x-internal-key"]).toBe("k".repeat(64));
    return [200, { version: body.version, count: 3 }];
  };
  expect(await execute(materialize)).toEqual({ version: "v", count: 3 });
  expect(timeout).toHaveBeenLastCalledWith(15 * 60000 + 15000);
  await execute(query);
  expect(timeout).toHaveBeenLastCalledWith(2 * 60000 + 15000);
  vi.stubEnv("EXECUTOR_JOB_TIMEOUT", "3600");
  await execute(materialize);
  expect(timeout).toHaveBeenLastCalledWith(3600000 + 15000);
});
it("receives an answer sent after the job's time limit, within the margin", async () => {
  // The executor stops the job at one second, then removes its scratch.
  vi.stubEnv("EXECUTOR_JOB_TIMEOUT", "1");
  answer = async () => {
    await new Promise((done) => setTimeout(done, 1500));
    return [422, { error: "Stopped at the executor's time limit" }];
  };
  await expect(execute(materialize)).rejects.toMatchObject({
    statusCode: 422,
    message: "Stopped at the executor's time limit",
  });
});
it("names the time limit when the executor does not answer", async () => {
  const timeout = AbortSignal.timeout.bind(AbortSignal);
  vi.spyOn(AbortSignal, "timeout").mockImplementation(() => timeout(100));
  answer = async () => undefined;
  await expect(execute(materialize)).rejects.toMatchObject({
    statusCode: 504,
    message:
      "The executor did not answer within its time limit (15 minutes, EXECUTOR_JOB_TIMEOUT)",
  });
});
it("refuses an invalid time limit before calling the executor", async () => {
  vi.stubEnv("EXECUTOR_QUERY_TIMEOUT", "2m");
  answer = async () => {
    throw new Error("Called the executor");
  };
  await expect(execute(query)).rejects.toThrow(
    "Invalid EXECUTOR_QUERY_TIMEOUT: 2m (seconds, from 1 to 86400)",
  );
});
