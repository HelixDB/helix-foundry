import { it, expect } from "vitest";
import { checkpointSource } from "../apps/api/src/source-sync.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
it("rejects missing and expired checkpoint leases without changing durable state", async () => {
  for (const leaseUntil of [undefined, null, NaN, Date.now() - 1]) {
    const store = new MemoryStore();
    const source = resource(
      "scope",
      "source",
      "Orders",
      {
        sync: { mode: "cdc", leaseOwner: "worker", leaseUntil, cursor: "0/1" },
      },
      "source",
    );
    await transaction(store, "scope", async (tx) => {
      tx.put(source);
    });
    await expect(
      checkpointSource(store, "scope", "source", "worker", "0/2", true),
    ).rejects.toThrow("lease expired");
    expect((await store.get("scope", "source"))?.data.sync.cursor).toBe("0/1");
  }
});
