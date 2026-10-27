import { afterEach, describe, expect, it, vi } from "vitest";
import type {
  DatabaseQueryResult,
  DynamicQueryRequest,
  QueryExecutor,
} from "@/web/areas/explore/database-query";
import { createRequestLimiter } from "@/web/areas/explore/explorer/lib/request-limiter";
import {
  ReadFailed,
  readOptions,
  readQueries,
  startingOf,
} from "@/web/areas/explore/reads";
import { pacedReads, tenantReads } from "@/web/areas/explore/tenant-reads";

// One limiter per database for every reader of an ontology's pages: a page's
// read goes at once and takes a token, background reads wait their turn, and
// a read the database rate-limits is tried again. A new database still
// starting is not rate-limited: its refusal goes to the query, which reads
// again after the transport's wait.

const REQUEST = {
  request_type: "read",
  query: {},
} as unknown as DynamicQueryRequest;
const ok: DatabaseQueryResult = {
  ok: true,
  status: 200,
  contentType: null,
  body: {},
  bytes: 0,
  elapsedMs: 0,
};
const limited: DatabaseQueryResult = {
  ok: false,
  kind: "database",
  status: 429,
  code: "rate_limited",
  message: "slow down",
  elapsedMs: 0,
};

describe("tenant reads", () => {
  it("retries a rate-limited read, then gives the answer", async () => {
    const answers = [limited, limited, ok];
    let calls = 0;
    const execute: QueryExecutor = async () => answers[calls++]!;
    const read = pacedReads(createRequestLimiter(), execute, false, [0, 0, 0]);
    expect(await read(REQUEST)).toBe(ok);
    expect(calls).toBe(3);
  });

  it("stops retrying after its delays, and when cancelled", async () => {
    let calls = 0;
    const execute: QueryExecutor = async () => {
      calls += 1;
      return limited;
    };
    expect(
      await pacedReads(createRequestLimiter(), execute, false, [0])(REQUEST),
    ).toBe(limited);
    expect(calls).toBe(2);
    const stop = new AbortController();
    stop.abort();
    const cancelled = await pacedReads(createRequestLimiter(), execute, true, [
      0,
    ])(REQUEST, { signal: stop.signal });
    expect(cancelled).toMatchObject({ ok: false, kind: "aborted" });
  });

  it("shares one limiter between a page's reads and background reads", async () => {
    const order: string[] = [];
    const execute: QueryExecutor = async (request) => {
      order.push(String((request as unknown as { tag: string }).tag));
      return ok;
    };
    const reads = tenantReads({ kind: "tenant", tenantId: "ten_1" }, execute);
    const tagged = (tag: string) =>
      ({ ...REQUEST, tag }) as unknown as DynamicQueryRequest;
    // Two page reads use the burst; the background read waits for a token.
    await reads.read(tagged("page-1"));
    await reads.read(tagged("page-2"));
    const background = reads.readLater(tagged("later"));
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(order).toEqual(["page-1", "page-2"]);
    await background;
    expect(order).toEqual(["page-1", "page-2", "later"]);
  });

  it("hands a starting database's refusal to the query once, for it to read again after the wait", async () => {
    const starting: DatabaseQueryResult = {
      ok: false,
      kind: "starting",
      status: 503,
      code: "rate_limit_unavailable",
      message: "Helix is still starting this new database.",
      retryInMs: 5_000,
      elapsedMs: 0,
    };
    let calls = 0;
    const execute: QueryExecutor = async () => {
      calls += 1;
      return starting;
    };
    const reads = tenantReads({ kind: "tenant", tenantId: "ten_1" }, execute);
    // Not a rate limit: the limiter doesn't try it again itself.
    expect(await reads.read(REQUEST)).toBe(starting);
    expect(calls).toBe(1);
    // A load's read through it fails as starting, and its query retries.
    const entity = readQueries.entity(
      "ws",
      "o",
      reads.read,
      { kind: "load", id: "load_1" },
      { type: "Customer", key: "c" },
    );
    const error = await entity.queryFn({}).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ReadFailed);
    expect(startingOf(error)).toMatchObject({ retryInMs: 5_000 });
    expect(entity.retry(0, error as Error)).toBe(true);
    expect(entity.retryDelay(0, error as Error)).toBe(5_000);
    expect(entity.retry).toBe(readOptions.retry);
  });
});

describe("tenant reads' own transport", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetModules();
  });

  it("reads the database's target, creation time included, so a new database reads as starting", async () => {
    const b64 = (text: string) => Buffer.from(text, "utf8").toString("base64");
    const replies = [
      { token: "t", expiresInSeconds: 3600 },
      {
        responseJson: b64(
          '{"error":"rate_limit_unavailable","msg":"tenant rate limit is unavailable"}',
        ),
        statusCode: 503,
        contentType: "application/json",
      },
    ];
    const sent: unknown[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url: string, init: RequestInit = {}) => {
        if (init.body) sent.push(JSON.parse(String(init.body)));
        return new Response(JSON.stringify(replies.shift()), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }),
    );
    vi.stubGlobal("window", {
      location: { pathname: "/", search: "", hash: "", assign: () => {} },
    });
    vi.resetModules();
    const { tenantReads: fresh } = await import(
      "@/web/areas/explore/tenant-reads"
    );
    const reads = fresh({
      kind: "tenant",
      tenantId: "ten_new",
      createdAt: new Date(Date.now() - 20_000).toISOString(),
    });
    expect(await reads.read(REQUEST)).toMatchObject({
      ok: false,
      kind: "starting",
      retryInMs: 5_000,
    });
    expect(sent.at(-1)).toMatchObject({ database: { tenantId: "ten_new" } });
  });
});
