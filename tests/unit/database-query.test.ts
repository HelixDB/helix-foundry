import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The explorer's and the reads' query transport: a v3 read sent through the
// BFF to databases:query-read, the database's answer decoded from base64,
// and every failure layer (BFF, WFE, database) mapped to a kind and a
// message. Writes never leave the browser.

type Call = { url: string; init: RequestInit };
let calls: Call[], replies: (Response | Error)[];
const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
const b64 = (text: string) => Buffer.from(text, "utf8").toString("base64");
const answer = (statusCode: number, body: string) =>
  json(200, {
    responseJson: b64(body),
    statusCode,
    contentType: "application/json",
  });

beforeEach(() => {
  calls = [];
  replies = [json(200, { token: "t", expiresInSeconds: 3600 })];
  vi.resetModules();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      calls.push({ url, init });
      const next = replies.shift() ?? json(500, { message: "no reply" });
      if (next instanceof Error) throw next;
      return next;
    }),
  );
  vi.stubGlobal("window", {
    location: { pathname: "/", search: "", hash: "", assign: () => {} },
  });
});
afterEach(() => vi.unstubAllGlobals());

const load = () => import("@/web/areas/explore/database-query");
const target = { kind: "tenant" as const, tenantId: "ten_1" };
const request = {
  request_type: "read" as const,
  query: { read: { entries: [], returns: [] } },
};

describe("executeDatabaseQuery", () => {
  it("sends the request as base64 to query-read and decodes the answer", async () => {
    const { executeDatabaseQuery } = await load();
    replies.push(answer(200, '{"n":[{"$id":1,"name":"Café ☕"}]}'));
    const result = await executeDatabaseQuery(target, request);
    expect(result).toMatchObject({
      ok: true,
      status: 200,
      body: { n: [{ $id: 1, name: "Café ☕" }] },
    });
    const sent = calls.find((c) => c.url === "/papi/v1/databases:query-read")!;
    expect(sent.init.method).toBe("POST");
    const body = JSON.parse(String(sent.init.body));
    expect(body.database).toEqual({ tenantId: "ten_1" });
    expect(
      JSON.parse(Buffer.from(body.queryJson, "base64").toString("utf8")),
    ).toEqual(request);
  });

  it("keeps integers past 2^53 exact, and an empty answer as null", async () => {
    const { executeDatabaseQuery, parseJsonLossless } = await load();
    expect(parseJsonLossless('{"t":1759777777123456789,"f":1.5}')).toEqual({
      t: expect.anything(),
      f: 1.5,
    });
    replies.push(answer(200, ""));
    expect(await executeDatabaseQuery(target, request)).toMatchObject({
      ok: true,
      body: null,
      bytes: 0,
    });
  });

  it("refuses writes and malformed requests before sending anything", async () => {
    const { executeDatabaseQuery, requestValidationMessage } = await load();
    const write = await executeDatabaseQuery(target, {
      ...request,
      request_type: "write",
    } as never);
    expect(write).toMatchObject({ ok: false, kind: "invalid" });
    expect(requestValidationMessage(null)).toMatch(/JSON object/);
    expect(requestValidationMessage({ request_type: "read" })).toMatch(
      /query object/,
    );
    expect(calls).toEqual([]);
  });

  it("maps the database's own failures", async () => {
    const { executeDatabaseQuery } = await load();
    replies.push(
      answer(
        400,
        '{"error":"invalid_query_json","msg":"unsupported operation x"}',
      ),
    );
    expect(await executeDatabaseQuery(target, request)).toMatchObject({
      ok: false,
      kind: "database",
      status: 400,
      code: "invalid_query_json",
      message: "unsupported operation x",
    });
    replies.push(answer(408, '{"error":"query_timeout","msg":"slow"}'));
    expect(await executeDatabaseQuery(target, request)).toMatchObject({
      kind: "database",
      message: "The query took too long. Add a limit or narrow the label.",
    });
    replies.push(answer(402, '{"error":"tenant_disabled"}'));
    expect(await executeDatabaseQuery(target, request)).toMatchObject({
      kind: "disabled",
    });
    replies.push(answer(503, "<html>bad gateway</html>"));
    expect(await executeDatabaseQuery(target, request)).toMatchObject({
      kind: "database",
      message:
        "The database is temporarily unavailable. Try again in a moment.",
    });
    replies.push(answer(403, '{"error":"forbidden"}'));
    expect(await executeDatabaseQuery(target, request)).toMatchObject({
      message: expect.stringMatching(/platform’s own credentials/),
    });
  });

  it("maps the WFE's and the BFF's refusals", async () => {
    const { executeDatabaseQuery } = await load();
    const cases: [Response, object][] = [
      [
        json(403, { code: 7, message: "permission denied" }),
        { kind: "forbidden" },
      ],
      [
        json(404, { code: 5, message: "database not found" }),
        { kind: "not_found" },
      ],
      [
        json(400, { code: 9, message: "tenant queries are disabled" }),
        { kind: "disabled" },
      ],
      // A billing gate's FAILED_PRECONDITION keeps the WFE's words.
      [
        json(400, {
          code: 9,
          message:
            "queries are paused: the workspace has no active plan; choose a plan to resume",
        }),
        {
          kind: "disabled",
          status: 400,
          message:
            "Queries are paused: the workspace has no active plan; choose a plan to resume.",
        },
      ],
      [
        json(400, {
          code: 9,
          message:
            "queries for this database are paused; check the workspace's billing status or contact support",
        }),
        { kind: "disabled" },
      ],
      [json(400, { code: 3, message: "bad shape" }), { kind: "invalid" }],
      [
        json(429, { code: 8, message: "query response exceeds broker limit" }),
        {
          kind: "too_large",
          message: expect.stringMatching(/result is too large/),
        },
      ],
      [
        json(429, { code: 8, message: "rate limit exceeded" }),
        { kind: "unavailable", code: "rate_limited" },
      ],
      [json(503, { code: 14, message: "down" }), { kind: "unavailable" }],
      [json(504, { code: 4, message: "deadline" }), { kind: "unavailable" }],
      [json(500, { code: 13, message: "boom" }), { kind: "unavailable" }],
      [
        json(404, { code: "NOT_FOUND", message: "not allowed" }),
        {
          kind: "unavailable",
          message: expect.stringMatching(/can’t read databases/),
        },
      ],
      [json(200, { nope: true }), { kind: "malformed" }],
      [
        json(200, { statusCode: 200, responseJson: "%%%" }),
        { kind: "malformed" },
      ],
    ];
    for (const [reply, expected] of cases) {
      replies.push(reply);
      expect(
        await executeDatabaseQuery(target, request),
        JSON.stringify(expected),
      ).toMatchObject({
        ok: false,
        ...expected,
      });
    }
  });

  it("says when the network failed or the read was cancelled", async () => {
    const { createQueryExecutor } = await load();
    const run = createQueryExecutor(target);
    replies.push(new TypeError("offline"));
    expect(await run(request)).toMatchObject({ ok: false, kind: "network" });
    const controller = new AbortController();
    controller.abort();
    expect(await run(request, { signal: controller.signal })).toMatchObject({
      ok: false,
      kind: "aborted",
    });
  });

  it("says a database created moments ago is starting, and when to read it again", async () => {
    const { executeDatabaseQuery } = await load();
    const refused = () =>
      answer(
        503,
        '{"error":"rate_limit_unavailable","msg":"tenant rate limit is unavailable"}',
      );
    const fresh = {
      ...target,
      createdAt: new Date(Date.now() - 20_000).toISOString(),
    };
    replies.push(refused());
    expect(await executeDatabaseQuery(fresh, request)).toMatchObject({
      ok: false,
      kind: "starting",
      status: 503,
      code: "rate_limit_unavailable",
      retryInMs: 5_000,
      message: expect.stringMatching(/still starting/),
    });
    // The shared cluster out is not the database's age.
    replies.push(answer(503, '{"error":"backend_unavailable"}'));
    expect(await executeDatabaseQuery(fresh, request)).toMatchObject({
      ok: false,
      kind: "database",
      code: "backend_unavailable",
    });
    // Its answer once it takes reads.
    replies.push(answer(200, '{"n":[]}'));
    expect(await executeDatabaseQuery(fresh, request)).toMatchObject({
      ok: true,
    });
    // After the window, or with no creation time: the platform failure.
    for (const createdAt of [
      new Date(Date.now() - 16 * 60_000).toISOString(),
      undefined,
    ]) {
      replies.push(refused());
      expect(
        await executeDatabaseQuery({ ...target, createdAt }, request),
      ).toMatchObject({
        ok: false,
        kind: "database",
        code: "rate_limit_unavailable",
        message: expect.stringMatching(/refusing queries/),
      });
    }
  });
});

describe("queriesPaused", () => {
  const refused = (
    kind: "disabled" | "invalid" | "database",
    message: string,
    status = 400,
  ) => ({
    ok: false as const,
    kind,
    status,
    code: null,
    message,
    elapsedMs: 1,
  });
  const NO_PLAN = refused(
    "disabled",
    "Queries are paused: the workspace has no active plan; choose a plan to resume.",
  );

  it("tells a workspace with no active plan from the other billing gates", async () => {
    const { queriesPaused } = await load();
    expect(queriesPaused(NO_PLAN)).toBe("no_plan");
    for (const message of [
      "Queries are paused: the workspace's plan payment is overdue; update the payment method to resume.",
      "Queries are paused: the workspace has used its plan's query quota; turn on overage or upgrade the plan to resume.",
      "Queries are paused: overage is on but the credit balance is spent; top up to resume.",
      "Queries for this database are paused; check the workspace's billing status or contact support.",
    ])
      expect(queriesPaused(refused("disabled", message))).toBe("billing");
    // The database's own 402 and the WFE's older words.
    expect(
      queriesPaused(
        refused("disabled", "Queries are paused for billing.", 402),
      ),
    ).toBe("billing");
  });

  it("is null for any other failure, whatever its message says", async () => {
    const { queriesPaused } = await load();
    expect(queriesPaused(refused("invalid", "bad shape"))).toBeNull();
    expect(
      queriesPaused(refused("database", "the workspace has no active plan")),
    ).toBeNull();
  });

  it("maps the WFE's no-plan refusal end to end", async () => {
    const { executeDatabaseQuery, queriesPaused } = await load();
    replies.push(
      json(400, {
        code: 9,
        message:
          "queries are paused: the workspace has no active plan; choose a plan to resume",
      }),
    );
    const result = await executeDatabaseQuery(target, request);
    expect(!result.ok && queriesPaused(result)).toBe("no_plan");
  });

  it("is what the explorer's words and the pages' reads say", async () => {
    const { describeFailure } = await import(
      "@/web/areas/explore/explorer/query/failure"
    );
    const { ReadFailed, pausedOf } = await import("@/web/areas/explore/reads");
    expect(describeFailure(NO_PLAN, "read")).toEqual({
      title: "Queries are paused: this workspace has no active Helix plan",
      hint: "Choose a plan in the Helix dashboard to resume them.",
      tone: "amber",
    });
    expect(
      describeFailure(refused("disabled", "Paused.", 402), "read").title,
    ).toBe("Queries are paused for billing");
    expect(pausedOf(new ReadFailed(NO_PLAN))).toBe(NO_PLAN);
    expect(pausedOf(new ReadFailed(refused("invalid", "bad")))).toBeNull();
    expect(pausedOf(new Error("Queries are paused"))).toBeNull();
    expect(pausedOf(undefined)).toBeNull();
  });
});
