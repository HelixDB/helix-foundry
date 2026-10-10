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
it("honors zero limits and rejects non-integral pagination", async () => {
  const root = await fixture([{ id: 1 }, { id: 2 }]);
  try {
    await execute(
      { mode: "ingest", scope: "fixture", version: "one", file: "input.jsonl" },
      root,
    );
    const request = {
      mode: "query" as const,
      scope: "fixture",
      inputs: [{ table: "input", version: "one" }],
      sql: "SELECT * FROM input",
    };
    expect((await execute({ ...request, limit: 0 }, root)).rows).toEqual([]);
    expect(
      (await execute({ ...request, limit: 1, offset: 1 }, root)).rows,
    ).toEqual([{ id: "2" }]);
    for (const value of [-1, 1.5, NaN, Infinity])
      await expect(execute({ ...request, limit: value }, root)).rejects.toThrow(
        "Invalid pagination",
      );
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
