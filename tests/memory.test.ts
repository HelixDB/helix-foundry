import { it, expect } from "vitest";
import {
  localAiMemory,
  localAiMinimumMemory,
  oomKills,
  oomWatcher,
} from "../apps/api/src/memory.js";
import {
  DatabaseError,
  databaseError,
  databaseOutOfMemory,
} from "../apps/api/src/store.js";

const vmstat = (kills: number) =>
  `pgfault 123\noom_kill ${kills}\nnr_free_pages 4\n`;

it("checks memory only for the Compose model, which shares Docker's memory", () => {
  // The 5.78 GiB Docker Desktop VM from issue #51.
  expect(localAiMemory("http://ollama:11434", 6_206_000_000)).toEqual({
    totalBytes: 6_206_000_000,
    minimumBytes: localAiMinimumMemory,
    sufficient: false,
  });
  expect(localAiMemory("http://ollama:11434", 16_000_000_000)?.sufficient).toBe(
    true,
  );
  for (const url of [
    "http://127.0.0.1:11434",
    "http://host.docker.internal:11434",
    "not a url",
  ])
    expect(localAiMemory(url, 1)).toBeNull();
});

it("reads the kernel's out-of-memory kill counter", () => {
  expect(oomKills(() => vmstat(7))).toBe(7);
  expect(oomKills(() => "pgfault 1\n")).toBeNull();
  expect(
    oomKills(() => {
      throw new Error("ENOENT");
    }),
  ).toBeNull();
});

it("reports a recent out-of-memory kill until the window passes", () => {
  let kills = 2,
    time = 0;
  const killed = oomWatcher(
    () => vmstat(kills),
    1000,
    () => time,
  );
  expect(killed()).toBe(false);
  kills = 3;
  expect(killed()).toBe(true);
  time = 500;
  expect(killed()).toBe(true);
  time = 1500;
  expect(killed()).toBe(false);
  const unreadable = oomWatcher(() => {
    throw new Error("ENOENT");
  });
  expect(unreadable()).toBe(false);
});

it("explains an unreachable database after Docker ran out of memory", () => {
  const network = Object.assign(new TypeError("fetch failed"), {
    cause: { code: "ECONNREFUSED" },
  });
  expect(databaseError(network, "read", 5, () => true)).toMatchObject({
    statusCode: 503,
    message: databaseOutOfMemory,
    code: "oom",
    transient: true,
    cause: network,
  });
  const remote = Object.assign(new Error("conflict"), { kind: "Remote" });
  expect(databaseError(remote, "write", 5, () => true)).toBe(remote);
});

it("names why HelixDB could not answer, and keeps the cause", () => {
  const failed = (code: string) =>
    databaseError(
      Object.assign(new TypeError("fetch failed"), { cause: { code } }),
      "write",
      5,
      () => false,
    ) as DatabaseError;
  expect(failed("ECONNREFUSED")).toMatchObject({
    code: "ECONNREFUSED",
    transient: true,
    message: expect.stringMatching(/not accepting connections/),
  });
  expect(failed("UND_ERR_SOCKET").message).toMatch(/dropped during a write/);
  expect(failed("ENOTFOUND").message).toMatch(/Cannot reach HelixDB/);
  expect(failed("ENOTFOUND").message).not.toMatch(/helix start/);
  const timeout = databaseError(
    Object.assign(new Error("aborted"), { name: "TimeoutError" }),
    "read",
    120_000,
    () => false,
  ) as DatabaseError;
  expect(timeout).toMatchObject({ code: "timeout", transient: true });
  expect(timeout.message).toMatch(/within 120 seconds/);
});
