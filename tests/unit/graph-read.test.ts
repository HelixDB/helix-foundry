import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@/web/api";
import {
  ReadFailed,
  readsBusy,
  typedReadFailure,
} from "@/web/areas/explore/database-query";
import {
  BUSY_RETRY_DELAYS_MS,
  readGraph,
} from "@/web/areas/explore/graph-read";
import { pausedOf, startingOf } from "@/web/areas/explore/reads";

// How this tab sends typed reads: the WFE runs at most four of a workspace's at
// once and refuses the rest, so an ontology's go two at a time, a refused one
// goes again later, and what the refusal says of the database reaches the page.

// The session has long left, so calls go side by side (src/web/api.ts).
const headers = { "x-foundry-session-expires-in": "300" };
const answer = () =>
  Response.json(
    {
      resultJson: JSON.stringify({
        as_of: "2026-11-02",
        results: [{ ok: { kind: "view", ontology_toml: "", partial: [] } }],
        stats: { instances: 0, wall_ms: 1 },
      }),
    },
    { headers },
  );
const refusal = (status: number, message: string) =>
  Response.json({ code: 9, message }, { status, headers });
const VIEW = [{ view: {} }];
// A fresh ontology per test: the turns are kept per ontology.
let ontology = 0;
const o = () => `ont_${(ontology += 1)}`;

/** Fetch's stub: answers CSRF itself and each read from `next`. */
function stub(next: () => Response | Promise<Response>) {
  const reads = vi.fn(next);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) =>
      url === "/papi/csrf"
        ? Response.json({ token: "t", expiresInSeconds: 600 }, { headers })
        : reads(),
    ),
  );
  return reads;
}

describe("readGraph", () => {
  beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("sends an ontology's reads two at a time", async () => {
    const waiting: ((r: Response) => void)[] = [];
    const reads = stub(() => new Promise((done) => waiting.push(done)));
    const ont = o();
    const all = Array.from({ length: 5 }, () =>
      readGraph("ws", ont, "l", VIEW),
    );
    await vi.advanceTimersByTimeAsync(10);
    expect(reads).toHaveBeenCalledTimes(2);
    waiting.shift()!(answer());
    await vi.advanceTimersByTimeAsync(10);
    expect(reads).toHaveBeenCalledTimes(3);
    // Another ontology's don't wait for them.
    const other = readGraph("ws", o(), "l", VIEW);
    await vi.advanceTimersByTimeAsync(10);
    expect(reads).toHaveBeenCalledTimes(4);
    waiting.forEach((done) => done(answer()));
    await vi.advanceTimersByTimeAsync(10);
    waiting.splice(0, 2).forEach((done) => done(answer()));
    await vi.advanceTimersByTimeAsync(10);
    waiting.forEach((done) => done(answer()));
    await expect(Promise.all([...all, other])).resolves.toHaveLength(6);
  });

  it("leaves the line when its reader no longer wants it", async () => {
    const waiting: ((r: Response) => void)[] = [];
    const reads = stub(() => new Promise((done) => waiting.push(done)));
    const ont = o(),
      gone = new AbortController();
    const first = [
      readGraph("ws", ont, "l", VIEW),
      readGraph("ws", ont, "l", VIEW),
    ];
    const given = readGraph("ws", ont, "l", VIEW, { signal: gone.signal });
    const last = readGraph("ws", ont, "l", VIEW);
    await vi.advanceTimersByTimeAsync(10);
    gone.abort();
    await expect(given).rejects.toMatchObject({ name: "AbortError" });
    waiting.splice(0).forEach((done) => done(answer()));
    await vi.advanceTimersByTimeAsync(10);
    // The one after it went in its place.
    expect(reads).toHaveBeenCalledTimes(3);
    waiting.splice(0).forEach((done) => done(answer()));
    await expect(Promise.all([...first, last])).resolves.toHaveLength(3);
  });

  it("sends a read again while the workspace's slots are taken", async () => {
    let busy = 2;
    const reads = stub(() =>
      busy-- > 0
        ? refusal(
            429,
            "the workspace already runs 4 graph reads; try again when one ends",
          )
        : answer(),
    );
    const read = readGraph("ws", o(), "l", VIEW);
    await vi.advanceTimersByTimeAsync(10);
    expect(reads).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(BUSY_RETRY_DELAYS_MS[0]!);
    expect(reads).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(BUSY_RETRY_DELAYS_MS[1]!);
    expect(reads).toHaveBeenCalledTimes(3);
    await expect(read).resolves.toHaveLength(1);
  });

  it("gives up in the end with the WFE's words, holding no turn", async () => {
    const words =
      "the workspace already runs 4 graph reads; try again when one ends";
    const reads = stub(() => refusal(429, words));
    const ont = o();
    const read = readGraph("ws", ont, "l", VIEW).catch((e: unknown) => e);
    await vi.advanceTimersByTimeAsync(
      BUSY_RETRY_DELAYS_MS.reduce((a, b) => a + b, 0) + 100,
    );
    const failed = await read;
    expect(failed).toBeInstanceOf(ApiError);
    expect((failed as ApiError).message).toBe(words);
    expect(reads).toHaveBeenCalledTimes(BUSY_RETRY_DELAYS_MS.length + 1);
    // Its turns came back.
    reads.mockImplementation(async () => answer());
    await expect(readGraph("ws", ont, "l", VIEW)).resolves.toHaveLength(1);
  });

  it("stops waiting to send again once it isn't wanted", async () => {
    const reads = stub(() => refusal(429, "busy"));
    const gone = new AbortController();
    const read = readGraph("ws", o(), "l", VIEW, { signal: gone.signal }).catch(
      (e: unknown) => e,
    );
    await vi.advanceTimersByTimeAsync(10);
    gone.abort();
    expect(await read).toMatchObject({ name: "AbortError" });
    expect(reads).toHaveBeenCalledTimes(1);
  });

  it("doesn't send a read again for anything else", async () => {
    // Too much to answer is a 429 too, and no slot will help.
    const large = stub(() => refusal(429, "response exceeds the maximum size"));
    await expect(readGraph("ws", o(), "l", VIEW)).rejects.toBeInstanceOf(
      ApiError,
    );
    expect(large).toHaveBeenCalledTimes(1);
    const gone = stub(() => refusal(404, "load not found"));
    await expect(readGraph("ws", o(), "l", VIEW)).rejects.toMatchObject({
      status: 404,
      message: "load not found",
    });
    expect(gone).toHaveBeenCalledTimes(1);
  });

  it("says a database is paused, as its page does", async () => {
    const words = "queries are paused: the workspace has no active plan";
    stub(() => refusal(400, words));
    const failed = await readGraph("ws", o(), "l", VIEW).catch(
      (e: unknown) => e,
    );
    expect(failed).toBeInstanceOf(ReadFailed);
    expect(pausedOf(failed)).toMatchObject({ kind: "disabled" });
    expect(startingOf(failed)).toBeNull();
  });

  it("says a new database is starting, only within its window", async () => {
    vi.setSystemTime(Date.parse("2026-11-02T12:00:00Z"));
    stub(() => refusal(503, "the graph reader failed; try again"));
    const failed = await readGraph("ws", o(), "l", VIEW, {
      createdAt: "2026-11-02T11:59:00Z",
    }).catch((e: unknown) => e);
    expect(failed).toBeInstanceOf(ReadFailed);
    expect(startingOf(failed)).toMatchObject({
      kind: "starting",
      retryInMs: 10_000,
    });
    // Past it, or with no time to go by, it's the WFE's own words.
    for (const createdAt of ["2026-11-02T11:00:00Z", undefined, null])
      await expect(
        readGraph("ws", o(), "l", VIEW, { createdAt }),
      ).rejects.toMatchObject({
        status: 503,
        message: "the graph reader failed; try again",
      });
  });

  it("refuses more reads than go in a request", async () => {
    await expect(readGraph("ws", o(), "l", [])).rejects.toThrow(
      "reads go in one request",
    );
  });
});

describe("a refused typed read", () => {
  it("tells a paused database, a starting one and neither apart", () => {
    const paused = (m: string) =>
      typedReadFailure(new ApiError(m, 400), undefined, 0)?.kind;
    expect(paused("tenant queries are disabled")).toBe("disabled");
    expect(paused("queries are paused: billing")).toBe("disabled");
    expect(paused("the load is being deleted")).toBeUndefined();
    expect(typedReadFailure(new ApiError("x", 500), undefined, 0)).toBeNull();
  });

  it("tells a full workspace from too much to answer", () => {
    expect(readsBusy(new ApiError("the workspace already runs 4", 429))).toBe(
      true,
    );
    expect(
      readsBusy(new ApiError("response exceeds the maximum size", 429)),
    ).toBe(false);
    expect(readsBusy(new ApiError("the graph reader failed", 503))).toBe(false);
  });
});
