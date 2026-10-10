import { randomUUID } from "node:crypto";
import { readdir, rm } from "node:fs/promises";
import { resolve } from "node:path";
import { expect, it } from "vitest";
import { importSource } from "../apps/api/src/connectors.js";
import { config } from "../apps/api/src/config.js";
import { jobProgress } from "../apps/api/src/setup-jobs.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";

it("does not allocate an open snapshot file when the first job progress rejects cancellation", async () => {
  const store = new MemoryStore();
  const scope = "canceled-import-" + randomUUID();
  const job = resource(scope, "job", "Canceled import", { status: "canceled" });
  await transaction(store, scope, async (tx) => {
    tx.put(job);
  });
  const directory = resolve(config.dataDir, "incoming", scope);
  try {
    await expect(
      importSource(scope, "rest", { url: "http://127.0.0.1:9" }, (progress) =>
        jobProgress(store, scope, job.id, progress),
      ),
    ).rejects.toThrow("Operation canceled");
    // Give any incorrectly allocated asynchronous fs.open a turn to complete.
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(await readdir(directory)).toEqual([]);
    expect((await store.get(scope, job.id))?.data.status).toBe("canceled");
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
