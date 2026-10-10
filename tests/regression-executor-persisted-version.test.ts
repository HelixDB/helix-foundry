import { it, expect } from "vitest";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execute } from "../services/executor/src/engine.js";
async function fixture(rows: unknown[]) {
  const root = await mkdtemp(join(tmpdir(), "foundry-regression-"));
  await mkdir(join(root, "incoming", "fixture"), { recursive: true });
  await writeFile(
    join(root, "incoming", "fixture", "input.jsonl"),
    rows.map((r) => JSON.stringify(r)).join("\n"),
  );
  return root;
}
it("requires a snapshot identity only when persisting", async () => {
  const root = await fixture([{ id: 1 }]);
  try {
    await expect(
      execute({ mode: "ingest", scope: "fixture", file: "input.jsonl" }, root),
    ).rejects.toThrow("Invalid storage identity");
    await expect(
      execute(
        {
          mode: "query",
          scope: "fixture",
          sql: "SELECT 1 AS n",
          persist: true,
        },
        root,
      ),
    ).rejects.toThrow("Invalid storage identity");
    await expect(
      execute(
        { mode: "ingest", scope: "fixture", version: "", file: "input.jsonl" },
        root,
      ),
    ).rejects.toThrow("Invalid storage identity");
    expect(
      (
        await execute(
          { mode: "query", scope: "fixture", sql: "SELECT 1 AS n" },
          root,
        )
      ).rows,
    ).toEqual([{ n: 1 }]);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
