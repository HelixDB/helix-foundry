import { it, expect, vi, afterEach } from "vitest";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { config } from "../apps/api/src/config.js";
import { importSource } from "../apps/api/src/connectors.js";
const fixture = vi.hoisted(() => ({ end: vi.fn(async () => {}) }));
vi.mock("pg", () => ({
  default: {
    Pool: class {
      end = fixture.end;
      async connect() {
        throw new Error("fixture unavailable");
      }
    },
  },
}));
it("closes the pool even when no snapshot connection can be acquired", async () => {
  const scope = "postgres-acquisition-fixture";
  try {
    await expect(
      importSource(scope, "postgres", { table: "orders" }),
    ).rejects.toThrow("fixture unavailable");
    expect(fixture.end).toHaveBeenCalledOnce();
  } finally {
    await rm(resolve(config.dataDir, "incoming", scope), {
      recursive: true,
      force: true,
    });
  }
});
