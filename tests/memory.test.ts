import { it, expect } from "vitest";
import {
  localAiMemory,
  localAiMinimumMemory,
  oomKills,
  oomWatcher,
} from "../apps/api/src/memory.js";
import { HelixStore, databaseOutOfMemory } from "../apps/api/src/store.js";

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

it("explains an unreachable database after Docker ran out of memory", async () => {
  const network = Object.assign(new Error("fetch failed"), {
    kind: "Network",
  });
  const fail = (e: Error) => () => Promise.reject(e);
  await expect(
    new HelixStore(() => true).send(fail(network)),
  ).rejects.toMatchObject({
    statusCode: 503,
    message: databaseOutOfMemory,
    cause: network,
  });
  await expect(new HelixStore(() => false).send(fail(network))).rejects.toBe(
    network,
  );
  const remote = Object.assign(new Error("conflict"), { kind: "Remote" });
  await expect(new HelixStore(() => true).send(fail(remote))).rejects.toBe(
    remote,
  );
  expect(await new HelixStore(() => true).send(async () => 1)).toBe(1);
});
