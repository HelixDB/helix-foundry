import { QueryClient, QueryObserver } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  STARTING_MS,
  isNewDatabaseAnswer,
  mayBeStarting,
  startingOr,
  startingRetryMs,
  type DatabaseQueryFailure,
} from "@/web/areas/explore/database-query";
import {
  describeFailure,
  isWriteOutcomeUnknown,
} from "@/web/areas/explore/explorer/query/failure";
import {
  ReadFailed,
  ontologyDatabase,
  readOptions,
  startingOf,
} from "@/web/areas/explore/reads";

// A database still starting: the gateway's answers to a tenant it hasn't
// taken on yet, within the first minutes of the database's ontology, are a
// calm "starting" failure that says when to read again; the reads' queries
// read again by themselves after that wait, and only then.

const CREATED = "2026-10-19T08:13:15.000Z";
const T = Date.parse(CREATED);

const database = (
  status: number,
  code: string | null,
  kind: "database" | "unavailable" = "database",
): DatabaseQueryFailure => ({
  ok: false,
  kind,
  status,
  code,
  message: "refused",
  body: { error: code, msg: "tenant rate limit is unavailable" },
  elapsedMs: 3,
});
const RATE_LIMIT = database(503, "rate_limit_unavailable");

describe("mayBeStarting", () => {
  it("holds for the window after the ontology was created", () => {
    expect(mayBeStarting(CREATED, T)).toBe(true);
    expect(mayBeStarting(CREATED, T + 60_000)).toBe(true);
    expect(mayBeStarting(CREATED, T + STARTING_MS - 1)).toBe(true);
    expect(mayBeStarting(CREATED, T + STARTING_MS)).toBe(false);
    expect(mayBeStarting(CREATED, T + 3_600_000)).toBe(false);
    expect(STARTING_MS).toBe(15 * 60_000);
    // The WFE's timestamps carry up to nanoseconds.
    expect(mayBeStarting("2026-10-19T08:13:15.123456789Z", T + 60_000)).toBe(
      true,
    );
  });

  it("holds for a clock behind the server's, and never without a time", () => {
    expect(mayBeStarting(CREATED, T - 30_000)).toBe(true);
    expect(mayBeStarting(undefined, T)).toBe(false);
    expect(mayBeStarting(null, T)).toBe(false);
    expect(mayBeStarting("", T)).toBe(false);
    expect(mayBeStarting("not a time", T)).toBe(false);
  });
});

describe("isNewDatabaseAnswer", () => {
  it("is the gateway's answers to a tenant it hasn't taken on", () => {
    expect(isNewDatabaseAnswer(RATE_LIMIT)).toBe(true);
  });

  it("is no other answer, nor the broker's own", () => {
    for (const f of [
      // The platform key refused, or the shared cluster out: not its age.
      database(503, "backend_unavailable"),
      database(401, "unauthorized"),
      database(403, null),
      database(503, null),
      database(503, "internal_error"),
      database(500, "internal_error"),
      database(429, "rate_limited"),
      database(408, "query_timeout"),
      database(400, "invalid_query_json"),
      database(404, "index_not_found"),
      database(402, "tenant_disabled"),
      database(503, "rate_limit_unavailable", "unavailable"),
      database(403, null, "unavailable"),
    ])
      expect(isNewDatabaseAnswer(f), JSON.stringify(f)).toBe(false);
  });
});

describe("startingRetryMs", () => {
  it("waits 5 s in the first minute, doubling each minute to 30 s", () => {
    expect(startingRetryMs(0)).toBe(5_000);
    expect(startingRetryMs(20_000)).toBe(5_000);
    expect(startingRetryMs(59_999)).toBe(5_000);
    expect(startingRetryMs(60_000)).toBe(10_000);
    expect(startingRetryMs(90_000)).toBe(10_000);
    expect(startingRetryMs(150_000)).toBe(20_000);
    expect(startingRetryMs(180_000)).toBe(30_000);
    expect(startingRetryMs(STARTING_MS)).toBe(30_000);
    expect(startingRetryMs(Number.MAX_SAFE_INTEGER)).toBe(30_000);
  });

  it("never waits less than 5 s, even for a clock ahead of the server's", () => {
    expect(startingRetryMs(-120_000)).toBe(5_000);
  });

  it("never shortens as the database ages", () => {
    const waits = Array.from({ length: 16 }, (_, m) =>
      startingRetryMs(m * 60_000),
    );
    expect(waits).toEqual([...waits].sort((a, b) => a - b));
    expect(new Set(waits)).toEqual(new Set([5_000, 10_000, 20_000, 30_000]));
  });
});

describe("startingOr", () => {
  it("makes a new database's answer in its window a starting failure", () => {
    const f = startingOr(RATE_LIMIT, CREATED, T + 90_000);
    expect(f).toEqual({
      ...RATE_LIMIT,
      kind: "starting",
      message: expect.stringMatching(/still starting/),
      retryInMs: 10_000,
    });
  });

  it("keeps every other failure as it was", () => {
    // After the window: today's error.
    expect(startingOr(RATE_LIMIT, CREATED, T + STARTING_MS)).toBe(RATE_LIMIT);
    // No creation time known.
    expect(startingOr(RATE_LIMIT, undefined, T)).toBe(RATE_LIMIT);
    // Not a new database's answer.
    const timeout = database(408, "query_timeout");
    expect(startingOr(timeout, CREATED, T)).toBe(timeout);
  });
});

describe("describeFailure", () => {
  it("words a starting database calmly, not as a platform failure", () => {
    const f = startingOr(RATE_LIMIT, CREATED, T);
    expect(describeFailure(f, "read")).toEqual({
      title: "Your new database is starting",
      hint: expect.stringMatching(/Try again shortly/),
      tone: "neutral",
    });
    // Its words also reach one-off actions, which nothing reads again.
    expect(f.message).not.toMatch(/by itself/);
    expect(describeFailure(f, "read").hint).not.toMatch(/by itself/);
    // The gateway refuses before it forwards anything.
    expect(isWriteOutcomeUnknown(f)).toBe(false);
    expect(describeFailure(RATE_LIMIT, "read").title).toBe(
      "The database's gateway is refusing queries",
    );
  });
});

describe("the reads", () => {
  const starting = new ReadFailed(startingOr(RATE_LIMIT, CREATED, T));

  it("tell a starting failure from the others", () => {
    expect(startingOf(starting)).toMatchObject({
      kind: "starting",
      retryInMs: 5_000,
    });
    expect(startingOf(new ReadFailed(RATE_LIMIT))).toBeNull();
    expect(startingOf(new Error("boom"))).toBeNull();
    expect(startingOf(null)).toBeNull();
  });

  it("retry a starting read after the transport's wait, and nothing else", () => {
    expect(readOptions.retry(0, starting)).toBe(true);
    expect(readOptions.retry(40, starting)).toBe(true);
    expect(readOptions.retryDelay(0, starting)).toBe(5_000);
    const later = new ReadFailed(startingOr(RATE_LIMIT, CREATED, T + 600_000));
    expect(readOptions.retryDelay(3, later)).toBe(30_000);
    for (const e of [new ReadFailed(RATE_LIMIT), new Error("boom")]) {
      expect(readOptions.retry(0, e)).toBe(false);
      expect(readOptions.retryDelay(0, e)).toBe(0);
    }
  });

  describe("as a query", () => {
    afterEach(() => vi.useRealTimers());

    // Every result a query's observer gives, as a page reads it.
    function observe(answers: (Error | string)[]) {
      vi.useFakeTimers();
      let calls = 0;
      const observer = new QueryObserver(new QueryClient(), {
        queryKey: ["read"],
        queryFn: async () => {
          const a = answers[calls++]!;
          if (a instanceof Error) throw a;
          return a;
        },
        ...readOptions,
      });
      const seen: { status: string; starting: boolean; data?: string }[] = [];
      observer.subscribe((r) =>
        seen.push({
          status: r.status,
          starting: !!startingOf(r.error ?? r.failureReason),
          data: r.data,
        }),
      );
      return { seen, calls: () => calls };
    }

    it("stays pending and starting between attempts, never falling back to loading", async () => {
      const q = observe([starting, starting, "rows"]);
      await vi.advanceTimersByTimeAsync(0);
      expect(q.calls()).toBe(1);
      expect(q.seen.at(-1)).toEqual({ status: "pending", starting: true });
      await vi.advanceTimersByTimeAsync(4_999);
      expect(q.calls()).toBe(1);
      await vi.advanceTimersByTimeAsync(1);
      expect(q.calls()).toBe(2);
      await vi.advanceTimersByTimeAsync(5_000);
      expect(q.calls()).toBe(3);
      // From the first refusal on, every result says starting until the rows.
      const first = q.seen.findIndex((r) => r.starting);
      expect(first).toBeGreaterThanOrEqual(0);
      const rest = q.seen.slice(first);
      expect(rest.at(-1)).toEqual({
        status: "success",
        starting: false,
        data: "rows",
      });
      expect(rest.slice(0, -1).every((r) => r.starting)).toBe(true);
    });

    it("settles any other failure at once, with no retry", async () => {
      const refused = new ReadFailed(RATE_LIMIT);
      const q = observe([refused, "rows"]);
      await vi.advanceTimersByTimeAsync(60_000);
      expect(q.calls()).toBe(1);
      expect(q.seen.at(-1)).toEqual({ status: "error", starting: false });
    });
  });

  it("read an ontology's database as starting from the ontology's creation", () => {
    expect(
      ontologyDatabase({ id: "o", tenantId: "ten_1", createdAt: CREATED }),
    ).toEqual({ kind: "tenant", tenantId: "ten_1", createdAt: CREATED });
    expect(ontologyDatabase({ id: "o" })).toEqual({
      kind: "tenant",
      tenantId: "",
      createdAt: undefined,
    });
  });
});
