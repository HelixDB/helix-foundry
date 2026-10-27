import { describe, expect, it, vi } from "vitest";
import { singleFlight } from "@/server/single-flight";

const deferred = <T>() => {
  let resolve!: (v: T) => void, reject!: (e: unknown) => void;
  const promise = new Promise<T>((a, b) => ((resolve = a), (reject = b)));
  return { promise, resolve, reject };
};

describe("singleFlight", () => {
  it("runs one job for concurrent callers with the same key", async () => {
    const run = singleFlight<string>({ graceMs: 1000, keep: () => true });
    const d = deferred<string>(),
      job = vi.fn(() => d.promise);
    const calls = Array.from({ length: 8 }, () => run("k", job));
    d.resolve("token-2");
    expect(await Promise.all(calls)).toEqual(Array(8).fill("token-2"));
    expect(job).toHaveBeenCalledTimes(1);
  });

  it("keeps different keys apart", async () => {
    const run = singleFlight<string>({ graceMs: 1000, keep: () => true });
    const job = vi.fn(async () => "x");
    await Promise.all([run("a", job), run("b", job)]);
    expect(job).toHaveBeenCalledTimes(2);
  });

  it("reuses a kept result during the grace period only", async () => {
    let t = 0;
    const now = () => t;
    const run = singleFlight<number>({ graceMs: 100, keep: () => true });
    let n = 0;
    const job = async () => ++n;
    expect(await run("k", job, now)).toBe(1);
    t = 99;
    expect(await run("k", job, now)).toBe(1);
    t = 100;
    expect(await run("k", job, now)).toBe(2);
  });

  it("does not keep results `keep` refuses, or failures", async () => {
    const run = singleFlight<string>({
      graceMs: 1000,
      keep: (v) => v === "ok",
    });
    const refused = vi.fn(async () => "no");
    await run("k", refused);
    await run("k", refused);
    expect(refused).toHaveBeenCalledTimes(2);
    const failing = vi.fn(async () => {
      throw new Error("down");
    });
    await expect(run("f", failing)).rejects.toThrow("down");
    await expect(run("f", failing)).rejects.toThrow("down");
    expect(failing).toHaveBeenCalledTimes(2);
  });

  it("bounds how many results it keeps", async () => {
    const run = singleFlight<string>({
      graceMs: 1000,
      maxKept: 2,
      keep: () => true,
    });
    const job = vi.fn(async () => "x");
    await run("a", job);
    await run("b", job);
    await run("c", job);
    await run("a", job);
    expect(job).toHaveBeenCalledTimes(4);
    await run("c", job);
    expect(job).toHaveBeenCalledTimes(4);
  });
});
